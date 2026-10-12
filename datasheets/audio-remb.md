# Audio REMB (receiver estimated maximum bitrate for the peer's audio)

**Status:** packet KAT-verified; estimator implemented; live bitrate retest pending

**Reference pinned at:**

- zapo `3c594911144e09951bd58f084f40162145bde9cc`
  (`packages/voip-media/src/media/rtcp.ts#L902-L1009`, `call/WaCallMediaPlane.ts#L2010-L2036`)
- driving change: zapo `04073f8834ffee819078fec8bd2dc315f8f374bc` (PR #309) "bring call audio to parity with the official clients"

## Observed facts (zapo, measured live)

- The peer's unified audio quality controller (UAQC) only leaves probing on a REMB for the
  audio stream above `uaqc_probing_remb_threshold` (22 000). Without one the official
  clients held audio at 15 kbps for the whole call.
- With a REMB starting at 24 kbps and holding at 30 kbps the peer moved to managed
  bandwidth and sent 24 to 25 kbps. UAQC's managed band lower threshold is 16 000 and its
  high-quality target 25 000; the peer reads the lower of our REMB and its own estimate.
- REMB wire form: PSFB (PT 206) FMT 15, sender SSRC, media source 0, FCI `REMB`, one SSRC,
  6-bit exponent over 18-bit mantissa, then the SSRC. The WhatsApp profile bit is left
  off REMB, as the client does.

## Reference source (zapo, MIT)

```ts
export function nextAudioReceiverEstimate(previous, lossPercent) {
    if (!(previous > 0)) return AUDIO_REMB_INITIAL_BITRATE          // 24_000
    let next = previous
    if (lossPercent >= REMB_CONGESTED_LOSS_PERCENT) next = previous * REMB_BACKOFF_FACTOR   // 10, 0.85
    else if (lossPercent < REMB_QUIET_LOSS_PERCENT) next = previous * AUDIO_REMB_GROWTH_FACTOR // 2, 1.1
    if (next < AUDIO_REMB_MIN_BITRATE) return AUDIO_REMB_MIN_BITRATE // 16_000
    if (next > AUDIO_REMB_MAX_BITRATE) return AUDIO_REMB_MAX_BITRATE // 30_000
    return Math.floor(next)
}
```

## Go target

- `rtp.BuildReceiverEstimatedMaxBitrate`, `rtp.EncodeRembBitrate`,
  `rtp.NextAudioReceiverEstimate`, `RtcpReceptionStats.TakeLossPercent`.
- `engine_media.go` RTCP ticker: after the audio sender report, on a 1:1 call with an audio
  reception report, one REMB for the peer's audio SSRC through the audio SRTCP sender.
