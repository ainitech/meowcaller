# MLow audio bundling (two frames per packet)

**Status:** implemented; unit-tested; live retest pending

**Reference pinned at:**

- zapo `3c594911144e09951bd58f084f40162145bde9cc`
  (`packages/voip-media/src/media/mlow-codec.ts#L38-L42`, `#L186-L216`;
  `packages/voip-media/src/call/WaCallMediaPlane.ts#L1307-L1352`;
  `packages/voip/src/call/WaCallMediaSession.ts#L122-L125`)
- driving change: zapo `04073f8834ffee819078fec8bd2dc315f8f374bc` (PR #309)

## Observed facts (zapo, measured live)

- The official 1:1 client sends two 60 ms MLow frames per packet, driven by `rc.maxfpp` in
  its `voip_settings` (2 in the captured profile). More than two is not sent.
- Multiframe packet: `[0x82 | toc & 0x39][count][count-1 lengths][sub-frames, each with its
  TOC]`; a length below 252 is one byte, otherwise `252 + (n-252)%4` then `(n-first)/4`.
- Only speech frames (TOC VAD bit) are bundled; a frame left alone when voice activity
  stops goes out by itself. The packet's RTP timestamp advances by both frames.

## Go target

- `signaling.VoipSettings.MaxFramesPerPacket` from `rc.maxfpp`;
  `audioFramesPerPacket(vs)` = 2 when `maxfpp >= 2` and `frame_ms == 60`, else 1.
- `mlow.PackMultiframe`: the container above; `splitContainer` already reads it.
- `rtp.RtpStream.NextPacketSpanning`, `MediaPipeline.ProtectAudioFrames`: timestamp span.
- `engine_media.go` send loop: a pending frame waits one tick for its pair.

## Deviations

- meowcaller's encoder emits a fixed TOC `0x50` (VAD set, 60 ms) for every frame, encoded
  silence included, so every frame is bundle-eligible; there is no lone trailing frame
  at voice-activity stop. Outbound audio gains one frame (60 ms) of latency when bundling.
