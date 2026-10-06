# Video send hold (first packet after the peer is ready)

**Status:** implemented; unit-tested; live retest pending

**Reference pinned at:**

- zapo `999bd439f8d7ffb1962604dc0f57b9c9a379d26a` (PR #303, commit
  "fix(voip): hold our video until the peer can receive it")
- zapo `87dd5b0cdd5e7e0c40b209b3bc7d47b0043d2349` for the `voip_settings` apply
  (`packages/voip-media/src/call/WaCallMediaPlane.ts#L613-L627`)

## Observed facts (zapo, measured live against WhatsApp Web)

- If our first video packet reaches WhatsApp Web before it has created the inbound
  stream for it, the receiver keeps the call on key frames alone.
- On an upgrade the peer asked for, the peer is ready once its `<video state=1>` has
  arrived; zapo waits that plus 300 ms, three seconds at most.
- On a call that is video from the start, in either direction, the peer is ready once
  its `<mute_v2>` after the accept has arrived; zapo waits that plus 150 ms, two seconds
  at most.
- While held, frames are dropped; the stream reopens on a key frame.
- `rc.rtcp_interval_ms` in `<voip_settings>` is the server's RTCP cadence and a `null`
  value restores the compiled one; `vid_rc.disable_rtcp_remb` turns REMB over RTCP off
  per call.

## Reference source (zapo commit message, PR #303)

```
Our frames now wait for the peer: on an upgrade the peer asked for, until its
<video state=1> and 300 ms more, three seconds at most; on a call that is video
from the start, in either direction, until the peer's <mute_v2> after the
accept and 150 ms more, two seconds at most. The plan carries the hold as
video.sendHeld, and the media plane drops frames while it is set and reopens
the stream on a key frame.
```

## Go target

- `videoSender.holdFor(max)` / `releaseHoldIn(d)`: a hold never extends; releasing
  sets `keyframeRequired`, so the stream reopens on an IDR.
- From-start video: armed when the sender attaches at media start (2 s), re-armed at
  the caller's `onAccept` while the peer's `<mute_v2>` is still to come, released
  150 ms after the peer's **first** `<mute_v2>` (later ones are mute toggles).
- Upgrade the peer asked for: `transitionVideo(UpgradeAccept)` marks
  `peerVideoPending`; the hold (3 s) arms when our sender is already active or when
  `setVideoEnabled(true)` follows; the peer's `<video state=1>` releases it after 300 ms.
- `voip_settings`: `rtcpIntervalMs` and `rtcpRembDisabled` recorded on the call; the
  SRTCP ticker starts on the server interval and resets when it changes.

## Deviations

- meowcaller already has a separate `videoGate` for our own upgrade request (wait for the
  peer's accept); the hold is additive and does not touch it.
- REMB over RTCP is still not emitted, so `disable_rtcp_remb` is recorded, not acted on.
