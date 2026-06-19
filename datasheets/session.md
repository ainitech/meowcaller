# Datasheet: `meowcaller/session`

The per-call signaling state machine plus the media-pipeline composition that
stitches RTP framing and E2E-SRTP crypto into one protect/unprotect path. Signaling
layer (the call lifecycle) and media layer (the pipeline composition).

**Validation vector:** (integration — no single vector). Behavior is pinned by the
inline `tests` module below: the lifecycle transition tables and a media-pipeline
round-trip plus a ciphertext-pinning test that the send keystream is keyed by the
self LID. The byte-level crypto/framing it composes is vector-pinned in its own
modules (RTP, E2E-SRTP, WARP), not here. If a composition vector file is later
extracted, copy it verbatim into `meowcaller/testdata/`.

## Reference source (verbatim — authoritative)

```rust
//! Call state machine and the media pipeline composition (Opus payload → RTP WARP header →
//! E2E SRTP protect, and the reverse). The byte-level crypto/framing lives in `wacore::voip`;
//! this stitches it together. Live media flow over the relay is deferred (PORT_PLAN.md).

use wacore::voip::e2e_srtp::{
    E2eSrtpKeys, RocTracker, append_warp_mi_tag, crypt_payload, derive_e2e_keys,
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

/// Lifecycle phase, mirroring zapo-caller `CallState.state`.
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
/// SFrame is omitted (default-off on send per zapo; plain Opus inside WAHKDF SRTP).
pub struct MediaPipeline {
    send_keys: E2eSrtpKeys,
    recv_keys: E2eSrtpKeys,
    warp_mi_tag_len: usize,
    rtp: RtpStream,
    send_roc: RocTracker,
}

impl MediaPipeline {
    /// Derive both directions from the 32-byte callKey. The HKDF `info` is the *sender's* own
    /// participant id, so send keys come from our self LID and recv keys from the peer LID (per
    /// zapo `session.ts`; SFrame uses the opposite convention). JIDs are normalized with the
    /// E2E-SRTP participant-id rule (keep an existing `:device`, bare `@lid` → `:0@lid`), which
    /// must match the form the peer derives our SSRC from.
    pub fn new(
        call_key: &[u8],
        self_jid: &str,
        peer_jid: &str,
        ssrc: u32,
        samples_per_packet: u32,
    ) -> Self {
        Self {
            send_keys: derive_e2e_keys(call_key, &format_e2e_srtp_participant_id(self_jid)),
            recv_keys: derive_e2e_keys(call_key, &format_e2e_srtp_participant_id(peer_jid)),
            warp_mi_tag_len: WARP_MI_TAG_LEN,
            rtp: RtpStream::new(ssrc, samples_per_packet, false),
            send_roc: RocTracker::default(),
        }
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
    /// `roc` defaults to 0 for in-order packets; the full ROC search is part of the live path.
    pub fn unprotect_audio(&self, packet: &[u8], roc: u32) -> Option<(RtpHeader, Vec<u8>)> {
        if packet.len() < 12 + self.warp_mi_tag_len {
            return None;
        }
        let without_tag = &packet[..packet.len() - self.warp_mi_tag_len];
        let header = parse_rtp_header(without_tag)?;
        let header_len = rtp_header_byte_length(without_tag)?;
        if without_tag.len() <= header_len {
            return None;
        }
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
        // equally-configured peers) — `protect_uses_self_lid_for_send` guards that.
        let call_key: Vec<u8> = (0u8..32).collect();
        let lid = "222222222222222:0@lid";
        let mut tx = MediaPipeline::new(&call_key, lid, lid, 0x12345678, 960);
        let rx = MediaPipeline::new(&call_key, lid, lid, 0x12345678, 960);

        let opus = vec![0x48u8, 0x11, 0x22, 0x33, 0x44, 0x55];
        let packet = tx.protect_audio(&opus);
        // First packet: seq=1 → roc=0.
        let (header, payload) = rx.unprotect_audio(&packet, 0).unwrap();
        assert_eq!(header.sequence_number, 1);
        assert_eq!(header.ssrc, 0x12345678);
        assert_eq!(header.payload_type, 120);
        assert_eq!(payload, opus);
    }

    #[test]
    fn protect_uses_self_lid_for_send() {
        // The outbound keystream must be keyed by our *self* LID (the sender's id) so a real
        // WhatsApp peer — which derives its recv keys from our LID — can decrypt us. An inversion
        // back to the peer LID would re-key this body and break interop (was the garbled-audio /
        // reconnect bug). Round-trip tests can't see this; pinning the ciphertext can.
        let call_key: Vec<u8> = (0u8..32).collect();
        let self_lid = "111111111111111:0@lid";
        let peer_lid = "222222222222222:0@lid";
        let ssrc = 0x12345678u32;
        let mut pipe = MediaPipeline::new(&call_key, self_lid, peer_lid, ssrc, 960);
        let opus = vec![0x10u8, 0x21, 0x32, 0x43];
        let packet = pipe.protect_audio(&opus);

        let without_tag = &packet[..packet.len() - WARP_MI_TAG_LEN];
        let header_len = rtp_header_byte_length(without_tag).unwrap();
        let body = &without_tag[header_len..];
        // First packet is seq=1, roc=0.
        let expect = crypt_payload(&derive_e2e_keys(&call_key, self_lid), ssrc, 1, 0, &opus);
        assert_eq!(
            body,
            expect.as_slice(),
            "send must encrypt under the self LID"
        );
        // And NOT under the peer LID (the inverted form).
        let inverted = crypt_payload(&derive_e2e_keys(&call_key, peer_lid), ssrc, 1, 0, &opus);
        assert_ne!(body, inverted.as_slice());
    }
}
```

## Go envelope (signatures only)

```go
package meowcaller

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
	PeerJID     Jid
	CallCreator Jid
	Direction   CallDirection
	IsVideo     bool
	// unexported current phase
}

func NewOutgoingSession(callID string, peerJID, callCreator Jid) *CallSession

func NewIncomingSession(callID string, peerJID, callCreator Jid) *CallSession

func (s *CallSession) Phase() CallPhase

func (s *CallSession) IsActive() bool

func (s *CallSession) IsEnded() bool

func (s *CallSession) TransitionTo(next CallPhase) bool

type MediaPipeline struct {
	// unexported: send/recv E2E-SRTP keys, WARP MI tag length,
	// RTP stream state, send-side ROC tracker
}

func NewMediaPipeline(callKey []byte, selfJID, peerJID string, ssrc, samplesPerPacket uint32) *MediaPipeline

func (p *MediaPipeline) ProtectAudio(opusPayload []byte) []byte

func (p *MediaPipeline) UnprotectAudio(packet []byte, roc uint32) (RtpHeader, []byte, bool)
```

## Implementation suggestions (guidance, not authoritative)

- The transition table is the load-bearing part and is fully pinned by
  `outgoing_lifecycle` / `incoming_starts_ringing_and_cannot_call`. Port the match
  arms exactly: `Ended` is a sink (nothing leaves it); anything-except-`Ended` may
  go to `Ended`; `Idle→Calling` only when `Outgoing`; the linear
  `Calling→Ringing→Connecting→Active` chain; and the idempotent `a==b` self-loop.
  Keep `transition_to` returning a bool no-op on illegal moves.
- `u32` → Go `uint32` (ssrc, samples, roc, sequence numbers). `usize` lengths → Go
  `int`. The length guards (`< 12 + tag_len`, `<= header_len`) must use the same
  byte arithmetic.
- Rust `Option<(RtpHeader, Vec<u8>)>` → Go `(RtpHeader, []byte, bool)` (the trailing
  bool is the `Some`/`None` flag). `None` returns become the zero/false case;
  prefer this over returning an error since the Rust uses `Option`, not `Result`.
- `protect_audio` takes `&mut self` (it advances `rtp` and `send_roc`);
  `unprotect_audio` takes `&self`. Mirror that: the protect method mutates pipeline
  state, the unprotect method does not.
- `TODO(human):` the send/recv key-direction convention is a real interop hazard,
  not a free choice. Send keys are derived from the *self* LID, recv keys from the
  *peer* LID, and the `protect_uses_self_lid_for_send` test pins the exact
  ciphertext. Wire `NewMediaPipeline` so send uses `selfJID` and recv uses `peerJID`
  — an inversion compiles, passes the round-trip, and silently breaks interop.
- `TODO(human):` this module only *composes* lower modules (RTP header
  encode/parse, `crypt_payload`, `append_warp_mi_tag`, `derive_e2e_keys`, the
  participant-id normalizer, `RocTracker`). Those must already exist and match
  their own vectors before this composition can be validated; there is no vector
  that exercises this file in isolation beyond the inline tests.
- `TODO(human):` the inbound ROC is hardcoded to the caller-supplied value (0 for
  in-order in the tests); the real receiver needs a ROC-search/recovery strategy on
  the live path. That logic is not present here and is a human decision.
