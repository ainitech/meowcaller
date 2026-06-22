# Datasheet: `meowcaller/session`

The per-call signaling state machine plus the media-pipeline composition that
stitches RTP framing and E2E-SRTP crypto into one protect/unprotect path. Signaling
layer (the call lifecycle) and media layer (the pipeline composition).

**Validation vector:** (integration — no single vector). Behavior is pinned by the
inline `tests` module below: the lifecycle transition tables and a media-pipeline
round-trip plus ciphertext-pinning tests that the send keystream is keyed by the
self LID and the recv keystream by the peer LID. The byte-level crypto/framing it
composes is vector-pinned in its own modules (RTP, E2E-SRTP, WARP), not here.

**Reference pinned at:** `41095d4e6ba4610e054e9ede3af1d5e88a83faee` (whatsapp-rust
`src/voip/session.rs` — main crate `src/`, not `wacore/src/`).

## Reference source (verbatim — authoritative)

```rust
//! Call state machine and the media pipeline composition (Opus payload to RTP WARP header to
//! E2E SRTP protect, and the reverse). The byte-level crypto/framing lives in `wacore::voip`;
//! this stitches it together. Live media flow over the relay is deferred (PORT_PLAN.md).

use wacore::voip::e2e_srtp::{
    E2eSrtpKeys, RecvRocTracker, RocTracker, append_warp_mi_tag, crypt_payload, derive_e2e_keys,
};
use wacore::voip::rtp::{
    RtpHeader, RtpStream, encode_rtp_header, parse_rtp_header, rtp_header_byte_length,
};
use wacore::voip::ssrc::format_e2e_srtp_participant_id;
use wacore::voip::warp::WARP_MI_TAG_LEN;
use wacore_binary::Jid;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CallDirection {
    Outgoing,
    Incoming,
}

/// Lifecycle phase of a call.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CallPhase {
    Idle,
    Calling,
    Ringing,
    Connecting,
    Active,
    Ended,
}

/// Per-call signaling state. Transitions are validated so an out-of-order server message
/// can't silently advance a torn-down call.
#[derive(Debug, Clone)]
pub struct CallSession {
    pub call_id: String,
    pub peer_jid: Jid,
    pub call_creator: Jid,
    pub direction: CallDirection,
    pub is_video: bool,
    phase: CallPhase,
}

impl CallSession {
    pub fn new_outgoing(call_id: impl Into<String>, peer_jid: Jid, call_creator: Jid) -> Self {
        Self {
            call_id: call_id.into(),
            peer_jid,
            call_creator,
            direction: CallDirection::Outgoing,
            is_video: false,
            phase: CallPhase::Idle,
        }
    }

    pub fn new_incoming(call_id: impl Into<String>, peer_jid: Jid, call_creator: Jid) -> Self {
        Self {
            call_id: call_id.into(),
            peer_jid,
            call_creator,
            direction: CallDirection::Incoming,
            is_video: false,
            phase: CallPhase::Ringing,
        }
    }

    pub fn phase(&self) -> CallPhase {
        self.phase
    }

    pub fn is_active(&self) -> bool {
        self.phase == CallPhase::Active
    }

    pub fn is_ended(&self) -> bool {
        self.phase == CallPhase::Ended
    }

    /// Attempt a phase transition; returns false (no-op) if it is not legal from the
    /// current phase. `Ended` is reachable from anything except `Ended`.
    pub fn transition_to(&mut self, next: CallPhase) -> bool {
        let ok = match (self.phase, next) {
            (CallPhase::Ended, _) => false,
            (_, CallPhase::Ended) => true,
            (CallPhase::Idle, CallPhase::Calling) => self.direction == CallDirection::Outgoing,
            (CallPhase::Calling, CallPhase::Ringing) => true,
            (CallPhase::Ringing, CallPhase::Connecting) => true,
            (CallPhase::Connecting, CallPhase::Active) => true,
            // Idempotent self-transition is allowed.
            (a, b) if a == b => true,
            _ => false,
        };
        if ok {
            self.phase = next;
        }
        ok
    }
}

/// Composes the outbound (protect) and inbound (unprotect) media pipeline for E2E 1:1.
/// SFrame is omitted (default-off on send; plain Opus inside WAHKDF SRTP).
pub struct MediaPipeline {
    send_keys: E2eSrtpKeys,
    recv_keys: E2eSrtpKeys,
    warp_mi_tag_len: usize,
    rtp: RtpStream,
    send_roc: RocTracker,
    recv_roc: RecvRocTracker,
}

impl MediaPipeline {
    /// Derive both directions from the 32-byte callKey. The HKDF `info` is the *sender's* own
    /// participant id, so send keys come from our self LID and recv keys from the peer LID
    /// (SFrame uses the opposite convention). JIDs are normalized with the E2E-SRTP
    /// participant-id rule (keep an existing `:device`, bare `@lid` becomes `:0@lid`), which
    /// must match the form the peer derives our SSRC from.
    /// Returns `None` when `call_key` is shorter than 32 bytes (a malformed peer callKey).
    pub fn new(
        call_key: &[u8],
        self_jid: &str,
        peer_jid: &str,
        ssrc: u32,
        samples_per_packet: u32,
    ) -> Option<Self> {
        Some(Self {
            send_keys: derive_e2e_keys(call_key, &format_e2e_srtp_participant_id(self_jid))?,
            recv_keys: derive_e2e_keys(call_key, &format_e2e_srtp_participant_id(peer_jid))?,
            warp_mi_tag_len: WARP_MI_TAG_LEN,
            rtp: RtpStream::new(ssrc, samples_per_packet, false),
            send_roc: RocTracker::default(),
            recv_roc: RecvRocTracker::default(),
        })
    }

    /// Outbound: wrap an Opus payload in an RTP WARP header, E2E-SRTP encrypt, append the WARP MI tag.
    pub fn protect_audio(&mut self, opus_payload: &[u8]) -> Vec<u8> {
        let header = self.rtp.next_packet(opus_payload, false);
        let roc = self.send_roc.advance(header.sequence_number);
        let header_bytes = encode_rtp_header(&header);
        let encrypted = crypt_payload(
            &self.send_keys,
            header.ssrc,
            header.sequence_number,
            roc,
            opus_payload,
        );
        let mut packet = header_bytes;
        packet.extend_from_slice(&encrypted);
        append_warp_mi_tag(&self.send_keys.auth_key, &packet, roc, self.warp_mi_tag_len)
    }

    /// Inbound: strip the WARP MI tag (not verified), parse the header, decrypt the payload.
    /// The ROC is derived per-packet from the recv tracker (RFC 3711 guess-index), so the keystream
    /// stays aligned with the sender's across 16-bit seq wraps even under reorder/loss.
    pub fn unprotect_audio(&mut self, packet: &[u8]) -> Option<(RtpHeader, Vec<u8>)> {
        if packet.len() < 12 + self.warp_mi_tag_len {
            return None;
        }
        let without_tag = &packet[..packet.len() - self.warp_mi_tag_len];
        let header = parse_rtp_header(without_tag)?;
        let header_len = rtp_header_byte_length(without_tag)?;
        if without_tag.len() <= header_len {
            return None;
        }
        let roc = self.recv_roc.guess_roc(header.sequence_number);
        let cipher = &without_tag[header_len..];
        let plain = crypt_payload(
            &self.recv_keys,
            header.ssrc,
            header.sequence_number,
            roc,
            cipher,
        );
        Some((header, plain))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use wacore_binary::Server;

    fn peer() -> Jid {
        Jid::new("222222222222222", Server::Lid)
    }
    fn creator() -> Jid {
        Jid::new("111111111111111", Server::Lid).with_device(1)
    }

    #[test]
    fn outgoing_lifecycle() {
        let mut s = CallSession::new_outgoing("CID", peer(), creator());
        assert_eq!(s.phase(), CallPhase::Idle);
        assert!(s.transition_to(CallPhase::Calling));
        assert!(s.transition_to(CallPhase::Ringing));
        assert!(s.transition_to(CallPhase::Connecting));
        assert!(s.transition_to(CallPhase::Active));
        assert!(s.is_active());
        // Illegal jump is rejected.
        assert!(!s.transition_to(CallPhase::Calling));
        assert!(s.transition_to(CallPhase::Ended));
        assert!(s.is_ended());
        // Nothing advances after Ended.
        assert!(!s.transition_to(CallPhase::Active));
    }

    #[test]
    fn incoming_starts_ringing_and_cannot_call() {
        let mut s = CallSession::new_incoming("CID", peer(), creator());
        assert_eq!(s.phase(), CallPhase::Ringing);
        // Incoming can't go to Calling.
        assert!(!s.transition_to(CallPhase::Calling));
        assert!(s.transition_to(CallPhase::Connecting));
        assert!(s.transition_to(CallPhase::Active));
    }

    #[test]
    fn media_pipeline_round_trips_composition() {
        // Same LID both directions so the loopback exercises header+crypt+tag stitching. This
        // cannot catch a send/recv direction inversion (the scheme is symmetric between two
        // equally-configured peers); `protect_uses_self_lid_for_send` guards that.
        let call_key: Vec<u8> = (0u8..32).collect();
        let lid = "222222222222222:0@lid";
        let mut tx = MediaPipeline::new(&call_key, lid, lid, 0x12345678, 960).unwrap();
        let mut rx = MediaPipeline::new(&call_key, lid, lid, 0x12345678, 960).unwrap();

        let opus = vec![0x48u8, 0x11, 0x22, 0x33, 0x44, 0x55];
        let packet = tx.protect_audio(&opus);
        // First packet: seq=1 gives roc=0.
        let (header, payload) = rx.unprotect_audio(&packet).unwrap();
        assert_eq!(header.sequence_number, 1);
        assert_eq!(header.ssrc, 0x12345678);
        assert_eq!(header.payload_type, 120);
        assert_eq!(payload, opus);
    }

    #[test]
    fn protect_uses_self_lid_for_send() {
        // The outbound keystream must be keyed by our *self* LID (the sender's id) so a real
        // WhatsApp peer, which derives its recv keys from our LID, can decrypt us. An inversion
        // back to the peer LID would re-key this body and break interop (was the garbled-audio /
        // reconnect bug). Round-trip tests can't see this; pinning the ciphertext can.
        let call_key: Vec<u8> = (0u8..32).collect();
        let self_lid = "111111111111111:0@lid";
        let peer_lid = "222222222222222:0@lid";
        let ssrc = 0x12345678u32;
        let mut pipe = MediaPipeline::new(&call_key, self_lid, peer_lid, ssrc, 960).unwrap();
        let opus = vec![0x10u8, 0x21, 0x32, 0x43];
        let packet = pipe.protect_audio(&opus);

        let without_tag = &packet[..packet.len() - WARP_MI_TAG_LEN];
        let header_len = rtp_header_byte_length(without_tag).unwrap();
        let body = &without_tag[header_len..];
        // First packet is seq=1, roc=0.
        let expect = crypt_payload(
            &derive_e2e_keys(&call_key, self_lid).unwrap(),
            ssrc,
            1,
            0,
            &opus,
        );
        assert_eq!(
            body,
            expect.as_slice(),
            "send must encrypt under the self LID"
        );
        // And NOT under the peer LID (the inverted form).
        let inverted = crypt_payload(
            &derive_e2e_keys(&call_key, peer_lid).unwrap(),
            ssrc,
            1,
            0,
            &opus,
        );
        assert_ne!(body, inverted.as_slice());
    }

    #[test]
    fn recv_uses_peer_lid_for_recv() {
        // The recv keystream must be keyed by the PEER's LID: a real peer encrypts under its own
        // (self) LID, which is our peer LID. A round-trip test can't catch a recv-direction key
        // inversion because the scheme is symmetric; this pins the direction.
        let call_key: Vec<u8> = (0u8..32).collect();
        let self_lid = "111111111111111:0@lid";
        let peer_lid = "222222222222222:0@lid";
        let ssrc = 0x12345678u32;

        // Our recv pipe (keys self=self_lid / peer=peer_lid).
        let mut us = MediaPipeline::new(&call_key, self_lid, peer_lid, ssrc, 960).unwrap();
        // The peer's send pipe is OUR mirror: its self LID is our peer LID.
        let mut peer_tx = MediaPipeline::new(&call_key, peer_lid, self_lid, ssrc, 960).unwrap();

        let opus = vec![0x48u8, 0x01, 0x02, 0x03, 0x04, 0x05];
        let from_peer = peer_tx.protect_audio(&opus);
        let (_, recovered) = us
            .unprotect_audio(&from_peer)
            .expect("peer packet must decrypt under our recv (peer-LID) keys");
        assert_eq!(recovered, opus, "recv must use the peer-LID keystream");

        // A packet a mis-keyed peer would send under OUR self LID must NOT recover: that proves the
        // recv side is not silently keyed by the self LID.
        let mut self_keyed_tx =
            MediaPipeline::new(&call_key, self_lid, peer_lid, ssrc, 960).unwrap();
        let wrong = self_keyed_tx.protect_audio(&opus);
        let mut us2 = MediaPipeline::new(&call_key, self_lid, peer_lid, ssrc, 960).unwrap();
        let (_, mis) = us2.unprotect_audio(&wrong).unwrap();
        assert_ne!(mis, opus, "recv must not recover a self-LID-keyed packet");
    }
}
```

## Go envelope (signatures only)

```go
package meowcaller

import (
	"go.mau.fi/whatsmeow/types"

	"github.com/purpshell/meowcaller/rtp"
)

type CallDirection int

const (
	CallDirectionOutgoing CallDirection = iota
	CallDirectionIncoming
)

type CallPhase int

const (
	CallPhaseIdle CallPhase = iota
	CallPhaseCalling
	CallPhaseRinging
	CallPhaseConnecting
	CallPhaseActive
	CallPhaseEnded
)

type CallSession struct {
	CallID      string
	PeerJID     types.JID
	CallCreator types.JID
	Direction   CallDirection
	IsVideo     bool
	// unexported current phase
}

func NewOutgoingSession(callID string, peerJID, callCreator types.JID) *CallSession

func NewIncomingSession(callID string, peerJID, callCreator types.JID) *CallSession

func (s *CallSession) Phase() CallPhase

func (s *CallSession) IsActive() bool

func (s *CallSession) IsEnded() bool

func (s *CallSession) TransitionTo(next CallPhase) bool

type MediaPipeline struct {
	// unexported: send/recv E2E-SRTP keys, WARP MI tag length,
	// RTP stream state, send-side + recv-side ROC trackers
}

func NewMediaPipeline(callKey []byte, selfJID, peerJID string, ssrc, samplesPerPacket uint32) (*MediaPipeline, error)

func (p *MediaPipeline) ProtectAudio(opusPayload []byte) ([]byte, error)

func (p *MediaPipeline) UnprotectAudio(packet []byte) (rtp.RtpHeader, []byte, bool)
```

## Implementation suggestions (guidance, not authoritative)

- This module only *composes* already-verified modules: `rtp` (header encode/parse,
  `RtpStream`, `FormatE2ESrtpParticipantID`), `srtp` (`DeriveE2eKeys`, `CryptPayload`,
  `AppendWarpMITag`, `RocTracker`, `RecvRocTracker`, `WarpMITagLen`), and whatsmeow
  `types.JID`. There is no vector exercising this file beyond the inline tests.
- The transition table is load-bearing and fully pinned by `outgoing_lifecycle` /
  `incoming_starts_ringing_and_cannot_call`: `Ended` is a sink; anything-except-`Ended`
  may go to `Ended`; `Idle→Calling` only when `Outgoing`; the linear
  `Calling→Ringing→Connecting→Active` chain; and the idempotent `a==b` self-loop.
- **Send/recv key direction is an interop hazard, not a free choice.** Send keys are
  derived from the *self* LID, recv keys from the *peer* LID;
  `protect_uses_self_lid_for_send` and `recv_uses_peer_lid_for_recv` pin the exact
  ciphertext. Wire `NewMediaPipeline` so send uses `selfJID` and recv uses `peerJID`.
- The recv ROC is now tracked **internally** (`RecvRocTracker`, RFC 3711 guess-index):
  `unprotect_audio` takes `&mut self` and no `roc` argument (the older datasheet passed
  `roc` in — that is gone). `protect_audio` mutates `rtp` + `send_roc`.
- **Error mapping (this port deviates from the reference's `Option`/infallible
  shapes because the lower Go modules are error-based):** `derive_e2e_keys` →
  `srtp.DeriveE2eKeys (E2eSrtpKeys, error)`, so `NewMediaPipeline` returns
  `(*MediaPipeline, error)` (bubbling the `<32`-byte guard). `crypt_payload` →
  `srtp.CryptPayload ([]byte, error)`, so `ProtectAudio` returns `([]byte, error)`.
  `UnprotectAudio` keeps the reference's `Option` shape as `(rtp.RtpHeader, []byte,
  bool)`: a malformed packet — or the (impossible-for-fixed-size-keys) crypt error —
  folds to `false`.
- The package lives at the repo root (`package meowcaller`), alongside the future
  `client.go`/`call.go` call-control surface.
```
