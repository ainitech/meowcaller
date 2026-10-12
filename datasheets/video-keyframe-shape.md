# H.264 key frame packet shape

**Status:** implemented; unit-tested through SRTP; live render retest pending

**Reference pinned at:**

- zapo `3c594911144e09951bd58f084f40162145bde9cc` (`packages/voip-media/src/media/h264.ts#L22-L94`)
- driving change: zapo `64767d9a658ea2b51889d03cb51d8eebc2d9dc63` (PR #307, "send H.264 key
  frames in WhatsApp's own packet shape")

## Observed facts (zapo, measured live)

- Sending each parameter set of a key frame as its own single-NAL packet: a phone answering
  a video call received the stream without loss but never rendered a frame and kept
  requesting key frames.
- The official clients never split a key frame: SPS, PPS and IDR travel as one unit. When
  the access unit fits the payload budget and the 16-bit size fields, it goes out as one
  STAP-A (F clear, NRI the larger of the parts). Otherwise it goes out from the SPS to the
  end as FU-A typed SPS with the inner start codes kept, which is what the Android client
  needs to render.

## Reference source (zapo, MIT)

```ts
const keyFrameSps = keyFrameSpsIndex(nals)
if (keyFrameSps >= 0) {
    const aggregated = stapA(nals, maxPayload)
    if (aggregated) return [aggregated]
}
// ... single NAL or FU-A for the units ahead of the SPS, then
if (keyFrameSps >= 0) pushFuA(payloads, data.subarray(spsOffset), maxPayload)
```

## Go target

- `videoSender.protectAccessUnitLocked`: on an IDR access unit with an SPS ahead of the IDR,
  `rtp.PackageH264STAPA` over the kept NAL units; when it does not fit, the existing
  whole-access-unit FU-A (typed SPS) path.
- `keyFrameHasSPSBeforeIDR` mirrors `keyFrameSpsIndex`.

## Deviations

- Payload budget stays at meowcaller's 800 bytes (zapo uses 1100). Units ahead of the SPS
  (SEI) stay inside the single FU-A run rather than going out as separate packets.
