package meowcaller

import (
	"go.mau.fi/whatsmeow/types"

	"github.com/purpshell/meowcaller/rtp"
	"github.com/purpshell/meowcaller/srtp"
)

// Call state machine and the media-pipeline composition (Opus payload → RTP WARP
// header → E2E-SRTP protect, and the reverse). The byte-level crypto/framing lives
// in the rtp/srtp packages; this stitches it together.

// CallDirection is the originating direction of a call.
type CallDirection int

const (
	CallDirectionOutgoing CallDirection = iota
	CallDirectionIncoming
)

// CallPhase is the lifecycle phase of a call.
type CallPhase int

const (
	CallPhaseIdle CallPhase = iota
	CallPhaseCalling
	CallPhaseRinging
	CallPhaseConnecting
	CallPhaseActive
	CallPhaseEnded
)

// CallSession is the per-call signaling state with validated phase transitions.
type CallSession struct {
	CallID      string
	PeerJID     types.JID
	CallCreator types.JID
	Direction   CallDirection
	IsVideo     bool
	phase       CallPhase
}

// NewOutgoingSession starts an outgoing call session in the Idle phase.
func NewOutgoingSession(callID string, peerJID, callCreator types.JID) *CallSession {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/src/voip/session.rs#L45-L54
	// TODO
	// agent suggestion: &CallSession{CallID, PeerJID, CallCreator, Direction: Outgoing, phase: Idle}.
	// human input:
	return nil
}

// NewIncomingSession starts an incoming call session in the Ringing phase.
func NewIncomingSession(callID string, peerJID, callCreator types.JID) *CallSession {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/src/voip/session.rs#L56-L65
	// TODO
	// agent suggestion: &CallSession{..., Direction: Incoming, phase: Ringing}.
	// human input:
	return nil
}

// Phase returns the current lifecycle phase.
func (s *CallSession) Phase() CallPhase {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/src/voip/session.rs#L67-L69
	// TODO
	// agent suggestion: return s.phase.
	// human input:
	return CallPhaseIdle
}

// IsActive reports whether the call is in the Active phase.
func (s *CallSession) IsActive() bool {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/src/voip/session.rs#L71-L73
	// TODO
	// agent suggestion: return s.phase == CallPhaseActive.
	// human input:
	return false
}

// IsEnded reports whether the call has ended.
func (s *CallSession) IsEnded() bool {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/src/voip/session.rs#L75-L77
	// TODO
	// agent suggestion: return s.phase == CallPhaseEnded.
	// human input:
	return false
}

// TransitionTo attempts a phase transition, returning false (no-op) if illegal.
// Ended is reachable from anything except Ended.
func (s *CallSession) TransitionTo(next CallPhase) bool {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/src/voip/session.rs#L81-L97
	// TODO
	// agent suggestion: port the match arms exactly (Ended sink; →Ended ok; Idle→Calling only when
	// Outgoing; linear Calling→Ringing→Connecting→Active; a==b idempotent); set phase iff ok.
	// human input:
	return false
}

// MediaPipeline composes the outbound (protect) and inbound (unprotect) E2E 1:1
// media path. SFrame is omitted (plain Opus inside WAHKDF SRTP).
type MediaPipeline struct {
	sendKeys     srtp.E2eSrtpKeys
	recvKeys     srtp.E2eSrtpKeys
	warpMITagLen int
	stream       *rtp.RtpStream
	sendRoc      srtp.RocTracker
	recvRoc      srtp.RecvRocTracker
}

// NewMediaPipeline derives both directions from the 32-byte callKey: send keys from
// the self LID, recv keys from the peer LID (an interop-load-bearing convention).
func NewMediaPipeline(callKey []byte, selfJID, peerJID string, ssrc, samplesPerPacket uint32) (*MediaPipeline, error) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/src/voip/session.rs#L118-L133
	// TODO
	// agent suggestion: sendKeys = srtp.DeriveE2eKeys(callKey, rtp.FormatE2ESrtpParticipantID(selfJID));
	// recvKeys from peerJID; bubble the derive error; stream = rtp.NewRtpStream(ssrc, samples, false);
	// warpMITagLen = srtp.WarpMITagLen.
	// human input:
	return nil, nil
}

// ProtectAudio wraps an Opus payload in an RTP WARP header, E2E-SRTP encrypts, and
// appends the WARP MI tag.
func (p *MediaPipeline) ProtectAudio(opusPayload []byte) ([]byte, error) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/src/voip/session.rs#L136-L150
	// TODO
	// agent suggestion: header := stream.NextPacket(opus, false); roc := sendRoc.Advance(header.Seq);
	// hdr := rtp.EncodeRtpHeader(&header); enc, err := srtp.CryptPayload(&sendKeys, ssrc, seq, roc, opus)
	// (bubble); packet := hdr||enc; return srtp.AppendWarpMITag(sendKeys.AuthKey[:], packet, roc, tagLen).
	// human input:
	return nil, nil
}

// UnprotectAudio strips the WARP MI tag (not verified), parses the header, and
// decrypts the payload, guessing the ROC from the recv tracker. ok=false on a
// malformed packet.
func (p *MediaPipeline) UnprotectAudio(packet []byte) (rtp.RtpHeader, []byte, bool) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/src/voip/session.rs#L155-L175
	// TODO
	// agent suggestion: guard len>=12+tagLen; withoutTag = packet[:len-tagLen]; ParseRtpHeader +
	// RtpHeaderByteLength (ok-gate); roc := recvRoc.GuessRoc(header.Seq); decrypt cipher via
	// srtp.CryptPayload(&recvKeys,...); any miss/err → (zero, nil, false).
	// human input:
	return rtp.RtpHeader{}, nil, false
}
