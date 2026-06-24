package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/purpshell/meowcaller/signaling"
	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite"
)

// meowcallerDBPath is meowcaller's own call store, deliberately a different file
// from whatsmeow's wa-voip.db auth store.
const meowcallerDBPath = "meowcaller.db"

// connectClient opens whatsmeow's auth store (its own file, separate from the
// meowcaller call store) and logs in (QR on first run), returning a connected
// client. busy_timeout absorbs brief lock contention so a busy session doesn't
// error out with "database is locked".
func connectClient(ctx context.Context) (*whatsmeow.Client, error) {
	log := zerolog.Ctx(ctx)
	// Present as a Google Chrome web client. The connection already advertises the
	// WEB platform; these companion props make the linked-device entry read
	// "Google Chrome (Mac OS)" instead of the default. DeviceProps is read at
	// pairing time, so re-pair for an already-linked device to pick this up.
	store.DeviceProps.Os = proto.String("Mac OS")
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_CHROME.Enum()

	container, err := sqlstore.New(ctx, "sqlite", "file:wa-voip.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", waLog.Zerolog(*zerolog.Ctx(ctx)).Sub("db"))
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("load device: %w", err)
	}
	client := whatsmeow.NewClient(device, waLog.Zerolog(*zerolog.Ctx(ctx)).Sub("wa"))
	// whatsmeow drops <ack> nodes; the outbound call's relay allocation rides in
	// <ack class="call">. Install the interceptor before Connect.
	installCallAckHook(client)

	if client.Store.ID == nil {
		qr, _ := client.GetQRChannel(ctx)
		if err := client.Connect(); err != nil {
			return nil, fmt.Errorf("connect: %w", err)
		}
		for evt := range qr {
			if evt.Event == "code" {
				log.Info().Int("valid_s", int(evt.Timeout.Seconds())).Str("qr_code", evt.Code).Msg("scan in WhatsApp > Linked devices")
			} else {
				log.Info().Str("event", evt.Event).Msg("login event")
			}
		}
	} else if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	// After QR pairing the server sends a 515 and whatsmeow disconnects to reconnect
	// with the new creds. WaitForConnection bails on that *expected* disconnect, so we
	// instead wait for the Connected event (dispatched only after authentication) and
	// the connected+logged-in state to settle across the reconnect.
	if err := waitUntilReady(ctx, client, 60*time.Second); err != nil {
		return nil, err
	}
	log.Info().Str("self_lid", client.Store.GetLID().String()).Msg("connected")

	// A device with no push name can't send presence; give it one, then announce
	// availability so the server delivers call signaling to us.
	if client.Store.PushName == "" {
		client.Store.PushName = "meowcaller"
	}
	if err := client.SendPresence(ctx, types.PresenceAvailable); err != nil {
		log.Warn().Err(err).Msg("send presence failed; continuing")
	}
	return client, nil
}

// waitUntilReady blocks until the client is connected and logged in, tolerating the
// expected post-pair (515) disconnect+reconnect. It keys off events.Connected, which
// whatsmeow dispatches only after successful authentication, so it returns once the
// reconnect-with-creds has fully settled rather than aborting on the planned drop.
func waitUntilReady(ctx context.Context, client *whatsmeow.Client, timeout time.Duration) error {
	ready := make(chan struct{}, 8)
	id := client.AddEventHandler(func(evt any) {
		if _, ok := evt.(*events.Connected); ok {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
	})
	defer client.RemoveEventHandler(id)

	deadline := time.After(timeout)
	for !(client.IsConnected() && client.IsLoggedIn()) {
		select {
		case <-ready:
		case <-deadline:
			return errors.New("timed out waiting for whatsmeow connection")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// resolvePeerLID turns a CLI target (phone number, phone JID, or @lid JID) into the
// peer's LID — the address the call's E2E keys and SSRCs derive from. A LID is used
// directly; a phone JID is mapped via the LID store, seeded by a usync query if not
// cached.
//
// parseCallTarget turns a CLI target into a JID. A string with '@' is a real JID (a
// LID to call directly, or a phone JID to resolve); a bare string is a phone number.
// ParseJID does NOT error on a missing '@' (it puts the whole string in the server
// field), so we branch on '@' ourselves rather than trusting a parse error.
func parseCallTarget(target string) (types.JID, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return types.EmptyJID, errors.New("empty call target")
	}
	if strings.ContainsRune(target, '@') {
		jid, err := types.ParseJID(target)
		if err != nil {
			return types.EmptyJID, fmt.Errorf("parse target JID %q: %w", target, err)
		}
		return jid, nil
	}
	return types.NewJID(strings.TrimPrefix(target, "+"), types.DefaultUserServer), nil
}

func resolvePeerLID(ctx context.Context, cli *whatsmeow.Client, target string) (types.JID, error) {
	jid, err := parseCallTarget(target)
	if err != nil {
		return types.EmptyJID, err
	}
	if jid.Server == types.HiddenUserServer {
		return jid, nil // already a LID — call it directly
	}
	if lid, err := cli.Store.LIDs.GetLIDForPN(ctx, jid); err == nil && !lid.IsEmpty() {
		return lid, nil
	}
	// usync: GetUserInfo issues the lid-bearing query and persists the PN→LID mapping.
	info, err := cli.GetUserInfo(ctx, []types.JID{jid})
	if err != nil {
		return types.EmptyJID, fmt.Errorf("usync %s: %w", jid.User, err)
	}
	for _, ui := range info {
		if !ui.LID.IsEmpty() {
			return ui.LID, nil
		}
	}
	if lid, err := cli.Store.LIDs.GetLIDForPN(ctx, jid); err == nil && !lid.IsEmpty() {
		return lid, nil
	}
	return types.EmptyJID, fmt.Errorf("usync returned no LID for %s (peer unreachable or not on WhatsApp)", jid.User)
}

// callKeyPlaintext wraps the raw callKey as the Signal message body
// Message{Call{CallKey}} (whatsmeow adds Signal padding during encryption).
func callKeyPlaintext(callKey []byte) ([]byte, error) {
	return proto.Marshal(&waE2E.Message{Call: &waE2E.Call{CallKey: callKey}})
}

// encryptCallKeyForDevice encrypts the callKey to one peer device's Signal session,
// fetching a pre-key bundle if no session exists yet. Returns the ciphertext, the
// enc type ("pkmsg" for a fresh session, "msg" for an existing one), and whether
// the offer must carry our <device-identity> (true for pkmsg, so the peer can
// verify the new session — without it the server drops the offer unacked).
func encryptCallKeyForDevice(ctx context.Context, cli *whatsmeow.Client, dev types.JID, callKey []byte) ([]byte, string, bool, error) {
	pt, err := callKeyPlaintext(callKey)
	if err != nil {
		return nil, "", false, err
	}
	di := cli.DangerousInternals()
	enc, needIdentity, err := di.EncryptMessageForDevice(ctx, pt, dev, nil, nil, nil)
	if err != nil {
		bundles := di.FetchPreKeysNoError(ctx, []types.JID{dev})
		enc, needIdentity, err = di.EncryptMessageForDevice(ctx, pt, dev, bundles[dev], nil, nil)
		if err != nil {
			return nil, "", false, err
		}
	}
	ct, ok := enc.Content.([]byte)
	if !ok {
		return nil, "", false, errors.New("enc node has no ciphertext")
	}
	return ct, enc.AttrGetter().String("type"), needIdentity, nil
}

// runCall connects, resolves the peer LID, discovers devices, encrypts a fresh
// callKey per device, and sends the <call><offer>.
func runCall(ctx context.Context, target string) error {
	log := zerolog.Ctx(ctx)
	cli, err := connectClient(ctx)
	if err != nil {
		return err
	}
	defer cli.Disconnect()

	store, err := openMeowStore(ctx, meowcallerDBPath)
	if err != nil {
		return err
	}
	defer store.Close()

	self := cli.Store.GetLID()
	if self.IsEmpty() {
		return errors.New("no own LID on this session")
	}
	peerLID, err := resolvePeerLID(ctx, cli, target)
	if err != nil {
		return err
	}
	log.Info().Str("peer_lid", peerLID.String()).Str("self_lid", self.String()).Msg("resolved peer LID")

	devices, err := cli.GetUserDevices(ctx, []types.JID{peerLID})
	if err != nil {
		return fmt.Errorf("device discovery: %w", err)
	}
	log.Info().Int("device_count", len(devices)).Str("peer_lid", peerLID.String()).Msg("peer device count")
	if len(devices) == 0 {
		return fmt.Errorf("peer %s has no devices (unreachable / not on WhatsApp); the server would reject the offer with 404", peerLID)
	}

	var callKey [32]byte
	if _, err := rand.Read(callKey[:]); err != nil {
		return err
	}
	deviceKeys := make([]signaling.OfferDeviceKey, 0, len(devices))
	needIdentity := false
	for _, dev := range devices {
		ct, encType, ni, err := encryptCallKeyForDevice(ctx, cli, dev, callKey[:])
		if err != nil {
			return fmt.Errorf("encrypt callKey for %s: %w", dev, err)
		}
		needIdentity = needIdentity || ni
		deviceKeys = append(deviceKeys, signaling.OfferDeviceKey{DeviceJid: dev, Ciphertext: ct, EncType: encType})
	}

	// pkmsg offers must carry our signed device identity so the peer can verify the
	// new session; the server drops the offer (no ack) otherwise.
	var deviceIdentity []byte
	if needIdentity {
		deviceIdentity, err = proto.Marshal(cli.Store.Account)
		if err != nil {
			return fmt.Errorf("marshal device identity: %w", err)
		}
	}

	// Include the peer's privacy token when we have one (the server requires it to
	// place a call to contacts with privacy enabled; it arrives via receipts/notifs).
	var privacyToken []byte
	if pt, err := cli.Store.PrivacyTokens.GetPrivacyToken(ctx, peerLID); err == nil && pt != nil {
		privacyToken = pt.Token
		log.Info().Int("token_bytes", len(privacyToken)).Str("peer_lid", peerLID.String()).Msg("attaching privacy token")
	} else {
		log.Info().Str("peer_lid", peerLID.String()).Msg("no privacy token; offer may be rejected if the peer requires one")
	}

	callID := newCallID()
	offer := signaling.BuildOffer(&signaling.OfferParams{
		CallID:         callID,
		To:             peerLID,
		CallCreator:    self,
		DeviceKeys:     deviceKeys,
		PrivacyToken:   privacyToken,
		Capability:     signaling.CapabilityOffer,
		DeviceIdentity: deviceIdentity,
	})
	// The builder leaves the <call> stanza id to the I/O layer. Without a stanza id
	// the server can't route/ack the offer, so it never reaches the callee.
	offer.Attrs["id"] = cli.GenerateMessageID()
	// Pre-seed the media coordinator with our generated callKey, then bring up media
	// when the relay endpoint arrives (relaylatency/transport) after the peer accepts.
	coord := newCoordinator(ctx, cli, store)
	setCallAckHandler(coord.onCallAck)
	m := coord.entry(callID)
	m.callKey = callKey[:]
	m.selfLID = self.String()
	m.peerLID = peerLID.String()
	m.direction = "outgoing"
	coord.persist(callID, "calling", m)
	cli.AddEventHandler(func(evt any) {
		switch e := evt.(type) {
		case *events.CallRelayLatency:
			coord.onRelay(e.CallID, e.Data)
		case *events.CallTransport:
			coord.onRelay(e.CallID, e.Data)
		case *events.CallTerminate:
			log.Info().Str("call_id", e.CallID).Str("reason", e.Reason).Msg("call terminated")
			coord.onTerminate(e.CallID)
		}
	})

	if err := cli.DangerousInternals().SendNode(ctx, offer); err != nil {
		return fmt.Errorf("send offer: %w", err)
	}
	log.Info().Str("call_id", callID).Msg("offer sent; media starts when the relay endpoint arrives. Ctrl+C to stop")
	<-ctx.Done()
	return nil
}

// callMedia tracks the per-call inputs needed to start media: the decrypted
// callKey (from the offer) and the relay data (from the offer or a later
// relaylatency/transport stanza). Media starts once both are present.
type callMedia struct {
	callKey   []byte
	relay     *relayData
	selfLID   string
	peerLID   string
	direction string
	started   bool
	cancel    context.CancelFunc // tears down this call's media goroutine
	// The callee <accept> is deferred until the caller's <mute_v2> arrives.
	acceptPending bool
}

// coordinator answers inbound offers and brings up the media loop once the relay
// endpoint arrives.
type coordinator struct {
	ctx   context.Context
	cli   *whatsmeow.Client
	store *meowStore
	log   zerolog.Logger
	mu    sync.Mutex
	cmap  map[string]*callMedia
}

func newCoordinator(ctx context.Context, cli *whatsmeow.Client, store *meowStore) *coordinator {
	return &coordinator{ctx: ctx, cli: cli, store: store, log: *zerolog.Ctx(ctx), cmap: map[string]*callMedia{}}
}

// persist writes a call's current meowcaller-side state to the meowcaller store.
// It never aborts the call: a store error is logged and the call continues.
func (c *coordinator) persist(callID, phase string, m *callMedia) {
	if c.store == nil {
		return
	}
	rec := callRecord{
		CallID:    callID,
		Direction: m.direction,
		SelfLID:   m.selfLID,
		PeerLID:   m.peerLID,
		CallKey:   m.callKey,
		Phase:     phase,
	}
	if m.relay != nil {
		if ep := getMediaRelayEndpoint(m.relay); ep != nil && len(ep.addresses) > 0 {
			rec.RelayIP = ep.addresses[0].ipv4
			rec.RelayPort = ep.addresses[0].port
		}
	}
	if err := c.store.SaveCall(c.ctx, rec); err != nil {
		c.log.Warn().Err(err).Str("call_id", callID).Msg("meowcaller-db: save call failed")
	}
}

func (c *coordinator) entry(callID string) *callMedia {
	if c.cmap[callID] == nil {
		c.cmap[callID] = &callMedia{}
	}
	return c.cmap[callID]
}

// onOffer decrypts the callKey, sends <preaccept> then <accept> back-to-back, and
// brings up media. The accept goes out immediately after the preaccept (the order a
// real caller acknowledges with a <receipt>); the media/relay bind then proceeds in
// the background. The offer <receipt> is sent separately by sendOfferReceipt (it
// needs the raw <call> stanza id).
func (c *coordinator) onOffer(e *events.CallOffer) {
	// A "call ended" notification arrives offer-shaped, carrying is_call_ended/terminate_reason
	// (e.g. accepted_elsewhere when the call was picked up on another device, often delivered
	// from the offline queue on reconnect). It is not a live call — engaging it (preaccept/
	// accept) just earns an "accept error 500". Ack-only; do not process.
	oag := e.Data.AttrGetter()
	if oag.OptionalString("is_call_ended") == "1" || oag.OptionalString("terminate_reason") != "" {
		c.log.Warn().Str("call_id", e.CallID).Str("reason", oag.OptionalString("terminate_reason")).Msg("ignoring already-ended offer; not a live call")
		return
	}

	callKey, err := decryptInboundCallKey(c.ctx, c.cli, e)
	if err != nil {
		c.log.Warn().Err(err).Str("call_id", e.CallID).Msg("decrypt callKey failed")
		return
	}
	c.log.Info().Int("key_bytes", len(callKey)).Str("call_id", e.CallID).Msg("decrypted callKey")

	// Preaccept: single rate 16000 + encopt + capability (0105f709e4bb13), NO metadata —
	// built inline to match the captured WA-Web preaccept body exactly (BuildPreaccept uses
	// the preaccept-specific capability blob / both rates).
	pre := waBinary.Node{
		Tag:   "call",
		Attrs: waBinary.Attrs{"to": e.From, "id": c.cli.DangerousInternals().GenerateRequestID()},
		Content: []waBinary.Node{{
			Tag:   "preaccept",
			Attrs: waBinary.Attrs{"call-id": e.CallID, "call-creator": e.CallCreator},
			Content: []waBinary.Node{
				{Tag: "audio", Attrs: waBinary.Attrs{"enc": "opus", "rate": "16000"}},
				{Tag: "encopt", Attrs: waBinary.Attrs{"keygen": "2"}},
				{Tag: "capability", Attrs: waBinary.Attrs{"ver": "1"}, Content: signaling.CapabilityOffer},
			},
		}},
	}
	if err := c.cli.DangerousInternals().SendNode(c.ctx, pre); err != nil {
		c.log.Error().Err(err).Str("call_id", e.CallID).Msg("send preaccept failed")
		return
	}
	c.log.Info().Str("call_id", e.CallID).Msg("preaccepted; accept deferred until mute_v2")

	peer := e.CallCreator
	if peer.IsEmpty() {
		peer = e.From
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.entry(e.CallID)
	m.callKey = callKey
	m.selfLID = c.cli.Store.GetLID().String()
	m.peerLID = peer.String()
	m.direction = "incoming"
	m.acceptPending = true
	if r := findRelay(e.Data); r != nil {
		m.relay = parseRelayData(r)
	}
	c.persist(e.CallID, "preaccepted", m)
	c.maybeStart(e.CallID, m)
}

// sendAccept sends the deferred callee <accept> (once), in the WA-Web format (metadata +
// single rate — the peer keeps the call alive with this; capability+both-rates setup_fails).
func (c *coordinator) sendAccept(callID string, to, creator types.JID) {
	c.mu.Lock()
	m := c.cmap[callID]
	if m == nil || !m.acceptPending {
		c.mu.Unlock()
		return
	}
	m.acceptPending = false
	c.mu.Unlock()

	accept := signaling.BuildAccept(&signaling.AcceptParams{
		CallID: callID, To: to, CallCreator: creator,
		AudioRates: []string{"16000"},
		Metadata:   waBinary.Attrs{"peer_abtest_bucket_id_list": "125208,94276"},
	})
	accept.Attrs["id"] = c.cli.DangerousInternals().GenerateRequestID()
	if err := c.cli.DangerousInternals().SendNode(c.ctx, accept); err != nil {
		c.log.Error().Err(err).Str("call_id", callID).Msg("send accept failed")
		return
	}
	c.log.Info().Str("call_id", callID).Msg("accepted (after mute_v2)")
	if c.store != nil {
		_ = c.store.SetPhase(c.ctx, callID, "accepted")
	}
}

// onRelay records relay data from a relaylatency/transport stanza.
func (c *coordinator) onRelay(callID string, data *waBinary.Node) {
	r := findRelay(data)
	if r == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.entry(callID)
	m.relay = parseRelayData(r)
	c.persist(callID, "relay", m)
	c.maybeStart(callID, m)
}

// onCallRaw sees every raw <call> node before whatsmeow processes it: it sends the offer
// <receipt>, and fires the deferred <accept> when the caller's <mute_v2> arrives (whatsmeow
// surfaces no mute event, so this is the only place we see it).
func (c *coordinator) onCallRaw(callNode *waBinary.Node) {
	kids := callNode.GetChildren()
	if len(kids) != 1 {
		return
	}
	switch kids[0].Tag {
	case "offer":
		c.sendOfferReceipt(callNode)
	case "mute_v2":
		mv := kids[0].AttrGetter()
		callID := mv.String("call-id")
		if callID == "" {
			return
		}
		c.log.Info().Str("call_id", callID).Msg("mute_v2 received; sending deferred accept")
		c.sendAccept(callID, callNode.AttrGetter().JID("from"), mv.JID("call-creator"))
	}
}

// sendOfferReceipt sends the WA-Web/reference-style <receipt> for an incoming
// <call><offer> (it carries the <call> stanza id, which the CallOffer event
// drops). Real callees and the reference (send_offer_ack_receipt) send this to
// register the device as a call participant; whatsmeow's auto <ack class="call">
// is not a substitute.
func (c *coordinator) sendOfferReceipt(callNode *waBinary.Node) {
	kids := callNode.GetChildren()
	if len(kids) != 1 || kids[0].Tag != "offer" {
		return
	}
	offer := &kids[0]
	oag := offer.AttrGetter()
	// Skip a "call ended" notification (accepted_elsewhere etc.) — whatsmeow still auto-acks
	// the <call>, but we don't receipt or engage an already-dead call.
	if oag.OptionalString("is_call_ended") == "1" || oag.OptionalString("terminate_reason") != "" {
		return
	}
	cag := callNode.AttrGetter()
	stanzaID := cag.String("id")
	caller := cag.JID("from")
	if stanzaID == "" || caller.IsEmpty() {
		return
	}
	// own "from": LID for a LID call, else PN (matches the reference).
	ownFrom := c.cli.Store.GetJID()
	if caller.Server == types.HiddenUserServer {
		ownFrom = c.cli.Store.GetLID()
	}
	receipt := waBinary.Node{
		Tag: "receipt",
		Attrs: waBinary.Attrs{
			"to":   caller,
			"id":   stanzaID,
			"from": ownFrom,
		},
		Content: []waBinary.Node{{
			Tag: "offer",
			Attrs: waBinary.Attrs{
				"call-id":      oag.String("call-id"),
				"call-creator": oag.JID("call-creator"),
			},
		}},
	}
	if err := c.cli.DangerousInternals().SendNode(c.ctx, receipt); err != nil {
		c.log.Error().Err(err).Str("call_id", oag.String("call-id")).Msg("send offer receipt failed")
		return
	}
	c.log.Info().Str("call_id", oag.String("call-id")).Msg("sent offer receipt")
}

// onRelayLatency answers the caller's relaylatency probes (the callee's half of the relay
// election). It does NOT send the accept — that is deferred until the caller's <mute_v2>.
func (c *coordinator) onRelayLatency(e *events.CallRelayLatency) {
	c.mu.Lock()
	m := c.cmap[e.CallID]
	c.mu.Unlock()
	if m == nil || m.direction != "incoming" {
		return
	}
	rl := findChild(e.Data, "relaylatency")
	if rl == nil {
		return
	}
	var probes []rlProbe
	for i := range rl.GetChildren() {
		te := &rl.GetChildren()[i]
		if te.Tag != "te" {
			continue
		}
		ag := te.AttrGetter()
		probes = append(probes, rlProbe{
			latency:   decodeLatency(ag.String("latency")),
			relayName: ag.String("relay_name"),
			addr:      nodeBytes(te),
		})
	}
	if sent := c.sendRelayLatency(e.CallID, e.From, e.CallCreator, probes); sent > 0 {
		c.log.Info().Int("packet_count", sent).Str("call_id", e.CallID).Msg("answered relaylatency probes")
	}
}

// rlProbe is one relay candidate from a relaylatency probe, captured so we can
// re-send it for a second election round.
type rlProbe struct {
	latency   uint32
	relayName string
	addr      []byte
}

// sendRelayLatency emits one <relaylatency> response per probe and returns how many
// it sent (the callee's half of the relay election).
func (c *coordinator) sendRelayLatency(callID string, to, creator types.JID, probes []rlProbe) int {
	sent := 0
	for _, p := range probes {
		resp := signaling.BuildRelayLatency(&signaling.RelayLatencyParams{
			CallID:       callID,
			To:           to,
			CallCreator:  creator,
			LatencyMs:    p.latency,
			RelayName:    p.relayName,
			AddressBytes: p.addr,
		})
		resp.Attrs["id"] = c.cli.GenerateMessageID()
		if err := c.cli.DangerousInternals().SendNode(c.ctx, resp); err != nil {
			c.log.Error().Err(err).Str("call_id", callID).Msg("send relaylatency failed")
			return sent
		}
		sent++
	}
	return sent
}

// onCallAck handles an <ack class="call"> node. For an outbound offer the relay
// allocation arrives here (whatsmeow otherwise drops the ack), which is what lets
// the caller bring up media.
func (c *coordinator) onCallAck(ack *waBinary.Node) {
	// An error ack (e.g. 404 unreachable, 439 bad offer, 500 server error) carries no
	// usable relay. Report it accurately (offer/accept/relaylatency/…) and tear down
	// any media we already started for that call rather than spin forever.
	if errCode := ack.AttrGetter().String("error"); errCode != "" {
		ackType := ack.AttrGetter().String("type")
		callID := ""
		if en := findChild(ack, "error"); en != nil {
			callID = en.AttrGetter().String("call-id")
		}
		c.log.Warn().Str("call_id", callID).Str("ack_type", ackType).Str("error_code", errCode).Msg("call rejected by server")
		c.stopMedia(callID)
		if c.store != nil && callID != "" {
			_ = c.store.SetPhase(c.ctx, callID, "failed:"+errCode)
		}
		return
	}
	r := findRelay(ack)
	if r == nil {
		return
	}
	callID := r.AttrGetter().String("call-id")
	if callID == "" {
		return
	}
	c.log.Info().Str("call_id", callID).Msg("relay allocation arrived in call ack")
	c.onRelay(callID, ack)
}

// onTerminate tears down a call's media and records its end.
func (c *coordinator) onTerminate(callID string) {
	c.stopMedia(callID)
	if c.store == nil {
		return
	}
	if err := c.store.SetPhase(c.ctx, callID, "terminated"); err != nil {
		c.log.Warn().Err(err).Str("call_id", callID).Msg("meowcaller-db: terminate failed")
	}
}

// maybeStart launches the media loop once the callKey and relay endpoint are known.
func (c *coordinator) maybeStart(callID string, m *callMedia) {
	if m.started || m.callKey == nil || m.relay == nil {
		return
	}
	m.started = true
	mctx, cancel := context.WithCancel(c.ctx)
	m.cancel = cancel
	c.persist(callID, "media", m)
	c.log.Info().Str("call_id", callID).Msg("starting media")
	go func() {
		if err := runMedia(mctx, callID, m.callKey, m.selfLID, m.peerLID, m.relay); err != nil {
			c.log.Warn().Err(err).Str("call_id", callID).Msg("media ended")
		}
	}()
}

// stopMedia cancels a call's media goroutine if it's running.
func (c *coordinator) stopMedia(callID string) {
	c.mu.Lock()
	if m := c.cmap[callID]; m != nil && m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	c.mu.Unlock()
}

// runListen connects and, with autoAccept, answers incoming calls and pipes media.
func runListen(ctx context.Context, autoAccept bool) error {
	log := zerolog.Ctx(ctx)
	cli, err := connectClient(ctx)
	if err != nil {
		return err
	}
	defer cli.Disconnect()

	store, err := openMeowStore(ctx, meowcallerDBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if n, err := store.CountCalls(ctx); err == nil {
		log.Info().Str("path", meowcallerDBPath).Int("call_count", n).Msg("meowcaller store loaded, separate from whatsmeow's wa-voip.db")
	}
	coord := newCoordinator(ctx, cli, store)
	setCallAckHandler(coord.onCallAck)
	setCallRawHandler(coord.onCallRaw)

	cli.AddEventHandler(func(evt any) {
		switch e := evt.(type) {
		case *events.CallOffer:
			log.Info().Str("call_id", e.CallID).Str("peer_lid", e.From.String()).Bool("auto_accept", autoAccept).Msg("incoming call")
			if autoAccept {
				coord.onOffer(e)
			}
		case *events.CallRelayLatency:
			if autoAccept {
				coord.onRelay(e.CallID, e.Data)
				coord.onRelayLatency(e)
			}
		case *events.CallTransport:
			if autoAccept {
				coord.onRelay(e.CallID, e.Data)
			}
		case *events.CallTerminate:
			log.Info().Str("call_id", e.CallID).Str("reason", e.Reason).Msg("call terminated")
			coord.onTerminate(e.CallID)
		}
	})
	log.Info().Bool("auto_accept", autoAccept).Msg("listening for calls. Ctrl+C to stop")
	<-ctx.Done()
	return nil
}

// decryptInboundCallKey pulls the <enc> from the offer node and decrypts the
// Message{Call{CallKey}} under our Signal session.
func decryptInboundCallKey(ctx context.Context, cli *whatsmeow.Client, e *events.CallOffer) ([]byte, error) {
	if e.Data == nil {
		return nil, errors.New("offer has no data node")
	}
	var enc *waBinary.Node
	for i := range e.Data.GetChildren() {
		if c := &e.Data.GetChildren()[i]; c.Tag == "enc" {
			enc = c
			break
		}
	}
	if enc == nil {
		return nil, errors.New("offer has no enc node")
	}
	isPreKey := enc.AttrGetter().String("type") == "pkmsg"
	pt, _, err := cli.DangerousInternals().DecryptDM(ctx, enc, e.From, isPreKey, e.Timestamp)
	if err != nil {
		return nil, err
	}
	var msg waE2E.Message
	if err := proto.Unmarshal(pt, &msg); err != nil {
		return nil, err
	}
	key := msg.GetCall().GetCallKey()
	if len(key) == 0 {
		return nil, errors.New("offer message carried no callKey")
	}
	return key, nil
}

// newCallID returns a call/wrapper id in WhatsApp's shape: 16 random bytes as
// uppercase hex (32 chars).
func newCallID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return strings.ToUpper(hex.EncodeToString(b[:]))
}
