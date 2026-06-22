package relay

import (
	"crypto/tls"
	"fmt"
	"net"

	"github.com/pion/datachannel"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/sctp"
)

// Relay media transport: a pre-negotiated WebRTC DataChannel over
// SCTP-over-DTLS-over-UDP to a single WhatsApp relay endpoint. Only
// ClassifyRelayPacket is unit-testable; the connection path talks to a live relay.

// RelayPacketKind classifies a packet seen on the relay channel by its first byte.
type RelayPacketKind int

const (
	RelayPacketStun RelayPacketKind = iota
	RelayPacketRtcp
	RelayPacketRtp
	RelayPacketOther
)

const (
	// DataChannelLabel is the pre-negotiated (id=0) DataChannel label WA Web uses.
	DataChannelLabel = "pre-negotiated"
	// SctpPort is the SCTP-over-DTLS WebRTC port (a WebRTC convention; pion's
	// sctp.Client negotiates over the DTLS conn and does not take it as config).
	SctpPort = 5000
)

// ClassifyRelayPacket demuxes by first byte: top two bits zero ⇒ STUN; 0x80/0x81 ⇒
// RTCP; 0x90 ⇒ RTP (WARP); anything else ⇒ Other.
func ClassifyRelayPacket(data []byte) RelayPacketKind {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/src/voip/transport.rs#L57-L70
	if len(data) < 2 {
		return RelayPacketOther
	}
	first := data[0]
	if first&0xc0 != 0 {
		switch first {
		case 0x80, 0x81:
			return RelayPacketRtcp
		case 0x90:
			return RelayPacketRtp
		default:
			return RelayPacketOther
		}
	}
	return RelayPacketStun
}

// CallTransportError categorizes a relay-transport failure so a consumer can branch:
// Connect is fatal (the call can't reach the relay); Send/Recv are recoverable on an
// established channel.
type CallTransportError struct {
	Op  string // "connect", "send", or "recv"
	Err error
}

func (e *CallTransportError) Error() string { return "relay " + e.Op + ": " + e.Err.Error() }
func (e *CallTransportError) Unwrap() error { return e.Err }

// RelayMediaChannel is an open relay media channel; STUN/RTP/RTCP travel as binary
// DataChannel messages. It owns the whole stack so Close tears it down cleanly
// (the reference relies on Rust Drop; Go needs explicit cleanup).
type RelayMediaChannel struct {
	udp      net.PacketConn
	dtlsConn net.Conn
	assoc    *sctp.Association
	dc       *datachannel.DataChannel
}

// Close tears down the media stack in reverse order of construction.
func (c *RelayMediaChannel) Close() error {
	var firstErr error
	for _, closer := range []func() error{c.dc.Close, c.assoc.Close, c.dtlsConn.Close, c.udp.Close} {
		if err := closer(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Send writes one media/STUN packet as a binary DataChannel message.
func (c *RelayMediaChannel) Send(data []byte) (int, error) {
	// NOT VALIDATED: no vector exists for the live transport; exercised only against a real relay.
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/src/voip/transport.rs#L118-L124
	n, err := c.dc.Write(data)
	if err != nil {
		return n, &CallTransportError{Op: "send", Err: err}
	}
	return n, nil
}

// Recv reads one DataChannel message into buf, returning its length.
func (c *RelayMediaChannel) Recv(buf []byte) (int, error) {
	// NOT VALIDATED: no vector exists for the live transport; exercised only against a real relay.
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/src/voip/transport.rs#L126-L132
	n, err := c.dc.Read(buf)
	if err != nil {
		return n, &CallTransportError{Op: "recv", Err: err}
	}
	return n, nil
}

// ConnectRelayMedia connects the full media stack (UDP→DTLS→SCTP→DataChannel) to one
// relay endpoint. Self-signed cert; server-cert verification skipped (media auth is
// HBH SRTP, not DTLS). No vector — validated only against a live relay.
func ConnectRelayMedia(relayAddr *net.UDPAddr) (*RelayMediaChannel, error) {
	// NOT VALIDATED: no vector exists for the live transport; exercised only against a real relay.
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/src/voip/transport.rs#L136-L195
	// Roll back already-allocated resources if a later step fails.
	var cleanup []func() error
	fail := func(err error) (*RelayMediaChannel, error) {
		for i := len(cleanup) - 1; i >= 0; i-- {
			_ = cleanup[i]()
		}
		return nil, &CallTransportError{Op: "connect", Err: err}
	}

	// 1. UDP socket.
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, &CallTransportError{Op: "connect", Err: fmt.Errorf("bind udp: %w", err)}
	}
	cleanup = append(cleanup, udp.Close)

	// 2. DTLS client (self-signed cert; skip server-cert verification).
	cert, err := selfsign.GenerateSelfSignedWithDNS("wa-voip")
	if err != nil {
		return fail(fmt.Errorf("dtls self-signed cert: %w", err))
	}
	dtlsConn, err := dtls.Client(udp, relayAddr, &dtls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
	})
	if err != nil {
		return fail(fmt.Errorf("dtls handshake: %w", err))
	}
	cleanup = append(cleanup, dtlsConn.Close)

	// 3. SCTP association over the DTLS conn.
	assoc, err := sctp.ClientWithOptions(sctp.WithNetConn(dtlsConn), sctp.WithName("wa-voip"))
	if err != nil {
		return fail(fmt.Errorf("sctp client: %w", err))
	}
	cleanup = append(cleanup, assoc.Close)

	// 4. Pre-negotiated DataChannel id=0.
	dc, err := datachannel.Dial(assoc, 0, &datachannel.Config{
		Negotiated: true,
		Label:      DataChannelLabel,
	})
	if err != nil {
		return fail(fmt.Errorf("datachannel dial: %w", err))
	}

	return &RelayMediaChannel{udp: udp, dtlsConn: dtlsConn, assoc: assoc, dc: dc}, nil
}
