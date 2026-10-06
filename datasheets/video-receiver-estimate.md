# Video receiver bandwidth estimate (fast-REMB header extension)

**Status:** wire element KAT-verified against zapo's captured vector; estimator
implemented; live ramp-up retest pending

**Reference pinned at:**

- zapo `87dd5b0cdd5e7e0c40b209b3bc7d47b0043d2349`
  (`packages/voip-media/src/media/fast-remb.ts`, `media/rtcp.ts#L860-L884`,
  `call/WaCallMediaPlane.ts#L807-L812` and `#L1725-L1742`)
- driving change: zapo `bc81d7bda0f18ff619054c368d286bcc657179a0` (PR #287)

## Observed facts (zapo, measured live against WhatsApp Web)

- WhatsApp does not read REMB over RTCP for video (`vid_rc.disable_rtcp_remb`); it
  reads a receive bandwidth estimate from an RTP header extension element inside the
  video stream, one-byte-header id 13.
- In a capture of 176 inbound video packets, 35 carried the id-13 element, always with
  9 bytes of content, only on video. Content: presence bitmap (`0x09` from the client),
  estimate as a plain 24-bit big-endian integer, receiver capacity field, flags byte.
  The client parser accepts bitmap `0x01` with 4 bytes of content.
- Captured vector: content `09 01 ae 9e 00 0c 3f 11 01` carries 110,238 bps. The element
  zapo emits for the same estimate is `d3 01 01 ae 9e`.
- Emitting it took the peer's `sbwe_ramp_up_count` from 0 to 91 and inbound video from
  ~380 packets in 75 s to 10,375 in 68 s.
- The announced value is a ceiling the peer's estimator adopts, not the arriving rate;
  echoing the measured rate locks a collapsed sender in place.

## Reference source (zapo, MIT)

```ts
export const WA_FAST_REMB_EXTENSION_ID = 13
const MAX_UINT24 = 0xffffff
const FAST_REMB_PRESENCE_BITMAP = 0x01
export function writeFastRembExtension(target, offset, bitsPerSecond) {
    target[offset] = (WA_FAST_REMB_EXTENSION_ID << 4) | (WA_FAST_REMB_PAYLOAD_LENGTH - 1)
    return 1 + writeFastRembPayload(target, offset + 1, bitsPerSecond)
}
```

```ts
export function nextReceiverMaxBitrate(previous, octets, elapsedMs, lossPercent) {
    if (!(previous > 0)) return REMB_INITIAL_BITRATE            // 300_000
    const measured = elapsedMs > 0 && octets > 0 ? (octets * 8 * 1000) / elapsedMs : 0
    let next
    if (lossPercent >= REMB_CONGESTED_LOSS_PERCENT) {           // 10
        const anchor = measured > 0 && measured < previous ? measured : previous
        next = anchor * REMB_BACKOFF_FACTOR                     // 0.85
    } else if (lossPercent >= REMB_QUIET_LOSS_PERCENT) {        // 2
        next = previous
    } else {
        const grown = previous * REMB_GROWTH_FACTOR             // 1.5
        const overshoot = measured * REMB_GROWTH_FACTOR
        next = overshoot > grown ? overshoot : grown
    }
    if (next < REMB_MIN_BITRATE) return REMB_MIN_BITRATE        // 64_000
    if (next > REMB_MAX_BITRATE) return REMB_MAX_BITRATE        // 2_000_000
    return Math.floor(next)
}
```

## Go target

- `rtp.VideoRtpExtension.ReceiverEstimateBps`: encoded as the id-13 element after id 9
  when non-zero; parsed from id 13 when the bitmap's bit 0 is set and content ≥ 4 bytes.
- `rtp.VideoRtpStream.SetReceiverEstimate`: announced on the opening packet of each
  access unit only.
- `rtp.NextReceiverMaxBitrate`, `rtp.ReceiverEstimate`: window per RTCP tick (1.5 s in
  meowcaller, 1 s in zapo), loss from `RtcpReceptionStats.CumulativeLossPercent`.
- `engine_media.go`: receive path observes video payload octets; the RTCP ticker closes
  the window; `videoSender` reads the ceiling before packetizing.

## Deviations

- REMB over RTCP is not emitted (zapo emits it only when `voip_settings` leaves it on;
  meowcaller does not parse that key yet).
- Window cadence follows meowcaller's existing 1.5 s sender-report ticker.
