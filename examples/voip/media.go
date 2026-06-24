package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	meowcaller "github.com/purpshell/meowcaller"
	"github.com/purpshell/meowcaller/mlow"
	"github.com/purpshell/meowcaller/relay"
	"github.com/purpshell/meowcaller/rtp"
	"github.com/purpshell/meowcaller/stun"
	"github.com/rs/zerolog"
	waBinary "go.mau.fi/whatsmeow/binary"
)

// pktDump writes a JSONL wire trace ({seq,t_ms,dir,kind,len,hex}) matching the reference
// example's VOIP_DUMP, so a meowcaller run can be diffed packet-for-packet against it.
// Gated on MEOW_DUMP (off by default).
type pktDump struct {
	mu    sync.Mutex
	f     *os.File
	seq   int
	start time.Time
}

func newPktDump(log zerolog.Logger) *pktDump {
	if os.Getenv("MEOW_DUMP") == "" {
		return nil
	}
	path := fmt.Sprintf("/tmp/meow-voip-dump-%d.jsonl", os.Getpid())
	f, err := os.Create(path)
	if err != nil {
		log.Warn().Err(err).Msg("dump file create failed")
		return nil
	}
	log.Debug().Str("path", path).Msg("packet dump -> path")
	return &pktDump{f: f, start: time.Now()}
}

func (d *pktDump) rec(dir, kind string, b []byte) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	fmt.Fprintf(d.f, "{\"seq\":%d,\"t_ms\":%d,\"dir\":%q,\"kind\":%q,\"len\":%d,\"hex\":%q}\n",
		d.seq, time.Since(d.start).Milliseconds(), dir, kind, len(b), hex.EncodeToString(b))
}

func (d *pktDump) close() {
	if d != nil && d.f != nil {
		_ = d.f.Close()
	}
}

// ---- relay signaling parse (port of wacore/src/voip/relay_parse.rs essentials) ----

type relayAddress struct {
	ipv4 string
	port uint16
}

type relayEndpoint struct {
	relayID     uint32
	relayName   string
	tokenID     uint32
	authTokenID uint32
	isFNA       bool
	addresses   []relayAddress
}

type relayData struct {
	relayKeyASCII []byte   // raw <key> content — the STUN MESSAGE-INTEGRITY key
	relayTokens   [][]byte // indexed <token id=…>
	endpoints     []relayEndpoint
}

// stunAttrSummary lists a STUN packet's attribute types + values (hex, capped) so the
// relay's addressing/subscription scheme is visible in the log.
func stunAttrSummary(pkt []byte) string {
	var parts []string
	for _, a := range stun.ParseStunAttributes(pkt) {
		v := a.Value
		if len(v) > 20 {
			v = v[:20]
		}
		parts = append(parts, fmt.Sprintf("0x%04x(%dB)=%x", a.AttrType, len(a.Value), v))
	}
	return strings.Join(parts, " ")
}

func nodeBytes(n *waBinary.Node) []byte {
	switch c := n.Content.(type) {
	case []byte:
		return c
	case string:
		return []byte(c)
	}
	return nil
}

func childByTag(n *waBinary.Node, tag string) *waBinary.Node {
	kids := n.GetChildren()
	for i := range kids {
		if kids[i].Tag == tag {
			return &kids[i]
		}
	}
	return nil
}

// findRelay recursively locates the <relay> node anywhere under n (it can sit under
// <offer> or a sibling <relaylatency>/<transport>).
func findRelay(n *waBinary.Node) *waBinary.Node {
	if n == nil {
		return nil
	}
	if n.Tag == "relay" {
		return n
	}
	kids := n.GetChildren()
	for i := range kids {
		if r := findRelay(&kids[i]); r != nil {
			return r
		}
	}
	return nil
}

// findChild recursively locates the first node with the given tag under n.
func findChild(n *waBinary.Node, tag string) *waBinary.Node {
	if n == nil {
		return nil
	}
	if n.Tag == tag {
		return n
	}
	kids := n.GetChildren()
	for i := range kids {
		if r := findChild(&kids[i], tag); r != nil {
			return r
		}
	}
	return nil
}

// decodeLatency reverses the relay-latency wire encoding (0x2000000 + rttMs).
func decodeLatency(enc string) uint32 {
	v, err := strconv.ParseUint(enc, 10, 32)
	if err != nil || v < 0x0200_0000 {
		return 0
	}
	return uint32(v) - 0x0200_0000
}

func attrUint(n *waBinary.Node, key string) uint32 {
	v, _ := strconv.ParseUint(n.AttrGetter().String(key), 10, 32)
	return uint32(v)
}

const maxRelayTokens = 64

func parseIndexedTokens(node *waBinary.Node, tag string) [][]byte {
	var tokens [][]byte
	kids := node.GetChildren()
	for i := range kids {
		c := &kids[i]
		if c.Tag != tag {
			continue
		}
		b := nodeBytes(c)
		if b == nil {
			continue
		}
		id := len(tokens)
		if s := c.AttrGetter().String("id"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				id = n
			}
		}
		if id >= maxRelayTokens {
			continue
		}
		for len(tokens) <= id {
			tokens = append(tokens, nil)
		}
		tokens[id] = b
	}
	return tokens
}

// parseRelayData ports parse_relay_data: <key>, indexed <token>, and te2 endpoints.
func parseRelayData(node *waBinary.Node) *relayData {
	rd := &relayData{}
	if key := childByTag(node, "key"); key != nil {
		rd.relayKeyASCII = nodeBytes(key)
	}
	rd.relayTokens = parseIndexedTokens(node, "token")

	kids := node.GetChildren()
	for i := range kids {
		te2 := &kids[i]
		if te2.Tag != "te2" {
			continue
		}
		ab := nodeBytes(te2)
		if len(ab) != 6 { // IPv4:port only (IPv6 endpoints skipped for this demo)
			continue
		}
		ep := relayEndpoint{
			relayID:     attrUint(te2, "relay_id"),
			relayName:   te2.AttrGetter().String("relay_name"),
			tokenID:     attrUint(te2, "token_id"),
			authTokenID: attrUint(te2, "auth_token_id"),
			isFNA:       te2.AttrGetter().String("is_fna") == "1",
			addresses: []relayAddress{{
				ipv4: fmt.Sprintf("%d.%d.%d.%d", ab[0], ab[1], ab[2], ab[3]),
				port: binary.BigEndian.Uint16(ab[4:6]),
			}},
		}
		rd.endpoints = append(rd.endpoints, ep)
	}
	return rd
}

// getMediaRelayEndpoint mirrors the reference: prefer an outbound (non-FNA,
// auth_token_id≠0) endpoint, else any non-FNA, else the first.
func getMediaRelayEndpoint(rd *relayData) *relayEndpoint {
	for i := range rd.endpoints {
		if e := &rd.endpoints[i]; !e.isFNA && e.authTokenID != 0 {
			return e
		}
	}
	for i := range rd.endpoints {
		if e := &rd.endpoints[i]; !e.isFNA {
			return e
		}
	}
	if len(rd.endpoints) > 0 {
		return &rd.endpoints[0]
	}
	return nil
}

// ---- relay connect + media loop (port of voip.rs connect_and_allocate + run_media) ----

func connectAndAllocate(ctx context.Context, rd *relayData) (*relay.RelayMediaChannel, []byte, error) {
	log := zerolog.Ctx(ctx)
	ep := getMediaRelayEndpoint(rd)
	if ep == nil || len(ep.addresses) == 0 {
		return nil, nil, fmt.Errorf("relay has no usable endpoint")
	}
	addr := &net.UDPAddr{IP: net.ParseIP(ep.addresses[0].ipv4), Port: int(ep.addresses[0].port)}
	log.Info().Str("relay_name", ep.relayName).Str("addr", addr.String()).Msg("connecting media transport to relay")

	type result struct {
		ch  *relay.RelayMediaChannel
		err error
	}
	done := make(chan result, 1)
	go func() {
		ch, err := relay.ConnectRelayMedia(addr)
		done <- result{ch, err}
	}()
	var ch *relay.RelayMediaChannel
	select {
	case r := <-done:
		if r.err != nil {
			return nil, nil, fmt.Errorf("relay connect: %w", r.err)
		}
		ch = r.ch
	case <-time.After(12 * time.Second):
		return nil, nil, fmt.Errorf("relay connect timed out (DTLS didn't complete)")
	}
	log.Info().Str("relay_name", ep.relayName).Msg("relay DataChannel open")

	if int(ep.tokenID) >= len(rd.relayTokens) || rd.relayTokens[ep.tokenID] == nil {
		return nil, nil, fmt.Errorf("no relay token #%d", ep.tokenID)
	}
	if len(rd.relayKeyASCII) == 0 {
		return nil, nil, fmt.Errorf("relay has no <key>")
	}
	endpointXor, ok := stun.EncodeXorRelayEndpoint(ep.addresses[0].ipv4, ep.addresses[0].port)
	if !ok {
		return nil, nil, fmt.Errorf("bad endpoint XOR")
	}
	var tx [12]byte
	_, _ = rand.Read(tx[:])
	allocate := stun.BuildWasmStunAllocateRequest(tx, rd.relayTokens[ep.tokenID], endpointXor, rd.relayKeyASCII)
	log.Info().
		Str("relay_name", ep.relayName).
		Str("addr", addr.String()).
		Uint32("token_id", ep.tokenID).
		Int("token_bytes", len(rd.relayTokens[ep.tokenID])).
		Uint32("auth_token_id", ep.authTokenID).
		Int("key_bytes", len(rd.relayKeyASCII)).
		Msg("allocate creds")
	if _, err := ch.Send(allocate); err != nil {
		return nil, nil, fmt.Errorf("allocate send: %w", err)
	}
	log.Info().Int("bytes", len(allocate)).Msg("sent STUN allocate")
	return ch, allocate, nil
}

// runMedia pipes mic↔speaker over the relay DataChannel: mic → MLow → E2E-SRTP
// protect → DataChannel, and DataChannel → unprotect → MLow → speaker, with a 1 Hz
// allocate+ping keepalive (the relay drops us without consent-freshness traffic).
func runMedia(ctx context.Context, callID string, callKey []byte, selfLID, peerLID string, rd *relayData) error {
	log := zerolog.Ctx(ctx)
	ch, allocate, err := connectAndAllocate(ctx, rd)
	if err != nil {
		return err
	}
	defer ch.Close()

	dump := newPktDump(*zerolog.Ctx(ctx))
	defer dump.close()
	dump.rec("out", "allocate", allocate)

	// Send a consent ping (0x0801) immediately, together with the allocate and BEFORE any
	// RTP — exactly as the working reference does. The relay won't forward the peer's media
	// until consent (ping → pong) is established; RTP sent before the first ping is dropped,
	// and the relay then never bridges. (meowcaller previously didn't ping until the 1 Hz
	// keepalive at ~t+1s, so every early RTP packet went out unconsented.)
	{
		var ptx [12]byte
		_, _ = rand.Read(ptx[:])
		initPing := stun.BuildWhatsappPing(ptx)
		_, _ = ch.Send(initPing[:])
		dump.rec("out", "ping", initPing[:])
	}

	ssrc, err := rtp.DeriveWasmParticipantSsrc(callID, rtp.FormatE2ESrtpParticipantID(selfLID), 0)
	if err != nil {
		return err
	}
	log.Info().
		Str("self_lid", selfLID).
		Str("peer_lid", peerLID).
		Str("ssrc", fmt.Sprintf("0x%08x", ssrc)).
		Msg("media session")

	enc := mlow.NewMlowEncoder()
	dec := mlow.NewMlowDecoder()
	txPipe, err := meowcaller.NewMediaPipeline(callKey, selfLID, peerLID, ssrc, frameSamps)
	if err != nil {
		return err
	}
	rxPipe, err := meowcaller.NewMediaPipeline(callKey, selfLID, peerLID, ssrc, frameSamps)
	if err != nil {
		return err
	}

	// relayRx counts packets received from the relay, so the keepalive can warn if
	// the relay never answers our allocate.
	var relayRx atomic.Uint64

	// Decoded PCM the recv loop hands to the speaker. Buffered + non-blocking so the
	// recv loop (which also answers the relay's consent checks) never stalls on audio.
	toSpeaker := make(chan []int16, 16)

	// Fast silence detector: inbound calls are torn down by the caller within ~400ms
	// if our relay bind never comes alive, so the 1 Hz keepalive's warning fires too
	// late to ever be seen. Check at 400ms and 900ms and say so explicitly.
	go func() {
		for _, d := range []time.Duration{400 * time.Millisecond, 900 * time.Millisecond} {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
			if relayRx.Load() == 0 {
				log.Warn().Dur("after", d).Msg("relay silent after allocate, no bytes back yet (allocate undelivered or rejected)")
			}
		}
	}()

	// Keepalive: re-send the Allocate AND a WhatsApp ping (0x0801) ~1 Hz. This is exactly
	// what the working reference does — a captured reference call sends allocate+ping every
	// second and sends NO STUN binding-requests at all; the relay answers allocate-success
	// (0x0103) + pong (0x0802) and bridges the peer's media. Binding-requests instead flip
	// the relay into ICE-consent mode and the bridge never forms.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			var tx [12]byte
			_, _ = rand.Read(tx[:])
			ping := stun.BuildWhatsappPing(tx)
			if _, err := ch.Send(allocate); err != nil {
				return
			}
			dump.rec("out", "allocate", allocate)
			_, _ = ch.Send(ping[:])
			dump.rec("out", "ping", ping[:])
		}
	}()

	// Send loop: frame-paced from call connect, NOT gated on the mic. WhatsApp starts media
	// on relay connection and sends DTX/silence frames for the first few seconds; the relay
	// learns our SSRC from our FIRST RTP and won't bridge the peer's media until it sees our
	// stream. Opening PortAudio can take >1s, so waiting for the mic means our first packet
	// lands seconds late — after the relay has stopped wiring the bridge. Send silence until
	// real mic frames arrive.
	micIn := make(chan []int16, 3)
	frameInterval := time.Duration(frameSamps) * time.Second / sampleRate
	go func() {
		silence := make([]float32, frameSamps)
		ticker := time.NewTicker(frameInterval)
		defer ticker.Stop()
		var txCount uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			frame := silence
			select {
			case pcm := <-micIn:
				frame = pcmToFloat(pcm)
			default:
			}
			payload, err := enc.Encode(frame)
			if err != nil {
				continue
			}
			packet, err := txPipe.ProtectAudio(payload)
			if err != nil {
				continue
			}
			if _, err := ch.Send(packet); err != nil {
				return
			}
			dump.rec("out", "rtp", packet)
			if txCount++; txCount == 1 {
				log.Info().Int("bytes", len(packet)).Msg("first RTP sent to relay, outbound media flowing (silence until mic ready)")
			} else if txCount%250 == 0 {
				log.Debug().Uint64("packet_count", txCount).Msg("sent RTP packets to relay")
			}
		}
	}()

	// Audio devices (mic/speaker), off the relay's critical path — opening PortAudio can
	// block >1s. Feeds mic frames to the send loop and drains decoded PCM to the speaker.
	go func() {
		a, err := newAudio()
		if err != nil {
			log.Error().Err(err).Msg("audio init failed")
			return
		}
		defer a.close()
		mic, stopMic, err := a.openMic()
		if err != nil {
			log.Error().Err(err).Msg("open mic failed")
			return
		}
		defer stopMic()
		speaker, stopSpeaker, err := a.openSpeaker()
		if err != nil {
			log.Error().Err(err).Msg("open speaker failed")
			return
		}
		defer stopSpeaker()
		log.Info().Msg("audio devices ready")

		// Speaker pump: drain decoded PCM from the recv loop to the speaker.
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case d := <-toSpeaker:
					speaker <- d
				}
			}
		}()

		// Mic pump: hand frames to the frame-paced send loop (drop if it's busy).
		for {
			select {
			case <-ctx.Done():
				return
			case pcm, ok := <-mic:
				if !ok {
					return
				}
				select {
				case micIn <- pcm:
				default:
				}
			}
		}
	}()

	// Receive: DataChannel → classify. RTP → unprotect → decode → speaker. A non-RTP
	// STUN binding request gets a binding-success reply (ICE consent freshness, RFC
	// 7675); without it the relay drops the binding and the peer's call fails.
	buf := make([]byte, 1500)
	var rtpIn, rtpSeen, unprotectFail, nonRtpLogged uint64
	var dumpedAlloc, dumpedBindReq, dumpedBindOK bool
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := ch.Recv(buf)
		if err != nil {
			return fmt.Errorf("relay recv: %w", err)
		}
		relayRx.Add(1)
		pkt := buf[:n]
		if relay.ClassifyRelayPacket(pkt) != relay.RelayPacketRtp {
			dump.rec("in", "stun", pkt)
			mt, isStun := stun.StunMessageType(pkt)
			// Answer the relay's STUN binding requests with a binding-success (ICE
			// consent freshness, RFC 7675) or the relay drops the binding.
			if isStun && mt == stun.MsgBindingRequest {
				if txid, ok := stun.StunTransactionID(pkt); ok && len(txid) == 12 {
					var tx [12]byte
					copy(tx[:], txid)
					resp := stun.EncodeStunRequest(stun.MsgBindingSuccess, tx, nil, rd.relayKeyASCII, true)
					if _, err := ch.Send(resp); err != nil {
						return fmt.Errorf("relay send binding-success: %w", err)
					}
				}
			}
			// Dump the attributes of the key relay packets once each — the allocate-success
			// and the relay's binding-request/success carry the addressing/subscription that
			// governs how (and whether) media bridges.
			switch {
			case !dumpedAlloc && mt == stun.MsgAllocateSuccess:
				dumpedAlloc = true
				log.Debug().Str("attrs", stunAttrSummary(pkt)).Msg("allocate-success attrs")
			case !dumpedBindReq && isStun && mt == stun.MsgBindingRequest:
				dumpedBindReq = true
				log.Debug().Str("attrs", stunAttrSummary(pkt)).Msg("relay binding-request attrs")
			case !dumpedBindOK && mt == stun.MsgBindingSuccess:
				dumpedBindOK = true
				log.Debug().Str("attrs", stunAttrSummary(pkt)).Msg("binding-success attrs")
			}
			// Diagnostic: log the first 30 non-RTP packets so the relay handshake is visible.
			if nonRtpLogged < 30 {
				nonRtpLogged++
				switch {
				case stun.IsAllocateError(pkt):
					if code, ok := stun.ParseStunErrorCode(pkt); ok {
						log.Debug().Str("kind", "allocate-error").Int("code", int(code)).Int("bytes", n).Msg("relay packet")
					} else {
						log.Debug().Str("kind", "allocate-error").Int("bytes", n).Msg("relay packet")
					}
				case isStun && mt == stun.MsgBindingRequest:
					log.Debug().Str("kind", "binding-request").Str("stun_type", fmt.Sprintf("0x%04x", mt)).Int("bytes", n).Msg("relay packet answered binding-success")
				case stun.IsAllocateOrBindingSuccess(pkt):
					log.Debug().Str("kind", "success").Str("stun_type", fmt.Sprintf("0x%04x", mt)).Int("bytes", n).Msg("relay packet")
				case isStun:
					log.Debug().Str("kind", "stun").Str("stun_type", fmt.Sprintf("0x%04x", mt)).Int("bytes", n).Msg("relay packet")
				default:
					log.Debug().Str("kind", "non-rtp").Str("first_byte", fmt.Sprintf("0x%02x", pkt[0])).Int("bytes", n).Msg("relay packet")
				}
			}
			continue
		}
		dump.rec("in", "rtp", pkt)
		if rtpSeen++; rtpSeen == 1 {
			log.Info().Int("bytes", n).Msg("first RTP-classified packet from relay, relay is bridging the peer's media")
		}
		_, payload, ok := rxPipe.UnprotectAudio(pkt)
		if !ok {
			if unprotectFail++; unprotectFail == 1 {
				log.Warn().Int("bytes", n).Msg("RTP arrived but failed to unprotect, keying/SSRC mismatch on the recv path")
			}
			continue
		}
		select {
		case toSpeaker <- floatToPCM(dec.Decode(payload)):
		default: // speaker not draining (audio not ready yet) — drop rather than stall consent
		}
		if rtpIn++; rtpIn == 1 {
			log.Info().Msg("first RTP decoded from relay, inbound audio flowing")
		}
	}
}
