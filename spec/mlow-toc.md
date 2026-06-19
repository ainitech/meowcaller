# Datasheet: `mlow/toc`

**Status:** planned (recovered + KAT-proven elsewhere; ready to scaffold)
**Registry #:** 02  ·  **Depends on:** none  ·  **Depended on by:** `mlow/decoder`, `rtp` (routing)

## Purpose

Parse the first byte (the "smpl TOC") of a bare MLow frame to learn how to decode
the rest of it, and to route standard-Opus frames away from the MLow path. Every
inbound media frame starts here.

## Source of truth

- **Rust reference:** `whatsapp-rust/wacore/src/voip/mlow/toc.rs` —
  `parse_mlow_toc(b: u8) -> MlowToc` (lines ~43-87) and `standard_opus_frame_ms`
  (lines ~25-39). Itself ported from the byte-exact Go reference (WASM func 3544).
- **wacrg doc:** `docs/codec/mlow/decode-pipeline.md` (§TOC routing).
- **Provenance / confidence:** `probable`, with executable backing — the bit
  layout is recovered from the WASM and pinned by a 256-entry KAT shared with the
  Rust. The standard-Opus frame-duration table is RFC 6716 Table 2.

## Interface

```go
package mlow

// SmplTOC is the decoded smpl TOC. When StdOpus is true the remaining fields are
// unused and the frame is a standard Opus/CELT packet (decode with stock libopus,
// not the smpl path).
type SmplTOC struct {
	StdOpus    bool
	SID        bool
	VAD        bool
	SampleRate int // Hz: 16000 or 32000
	FrameMs    int // 10, 20, 60, or 120 (smpl); RFC6716 durations for std-opus
	Voiced     bool
	Active     bool
	Flag2      bool
	Flag0      bool
}

func ParseSmplTOC(b byte) SmplTOC
```

## Behavior

If `(b & 0xC0) == 0xC0` the frame is **standard Opus/CELT**: return
`{StdOpus: true, SampleRate: 16000, FrameMs: standardOpusFrameMs(b)}`, all other
fields zero. `standardOpusFrameMs` reads the config field `b>>3` per RFC 6716
Table 2 (SILK `config<12`, Hybrid `<16`, CELT otherwise; 2.5 ms rounds up to 3).

Otherwise it is an **smpl** frame, bit layout (LSB = bit0):

| bit | field |
| --- | --- |
| 7 | `SID` (DTX / comfort noise) |
| 6 | `VAD` |
| 5 | internal rate: 0 → 16000 Hz, 1 → 32000 Hz |
| 4:3 | frame-size index into `{10, 20, 60, 120}` ms |
| 2 | `Flag2` |
| 1 | voiced-enable (`bit1`) |
| 0 | `Flag0` |

Derived: `Voiced = VAD && bit1`, `Active = VAD || bit1`.

## Constants and tables

- `0xC0` mask — selects the standard-Opus escape.
- frame-size table `{10, 20, 60, 120}` ms (smpl); `{10,20,40,60}` / `{10,20}` and
  the CELT `{3,5,10,20}` map (standard Opus, RFC 6716).
- No external ROM; fully self-contained.

## Inputs / outputs

- **Input:** one byte (the frame's first byte).
- **Output:** the `SmplTOC` struct. Pure function, no state.

## Validation

- **KAT:** `toc_vectors.json` — every one of the **256** possible byte values with
  its expected decode (`b, std, sid, vad, sr, ms, voiced, active, f2, f0`). Copy it
  verbatim from `whatsapp-rust/wacore/src/voip/mlow/testdata/toc_vectors.json` into
  `mlow/testdata/`.
- **How to run:** `go test ./mlow -run TestParseSmplTOCAgainstKAT`.
- **Done when:** all 256 entries match exactly. (This port has already been proven
  to pass 256/256; see `PLAN.md` Appendix A for the exact code.)

## Assumptions and open decisions

- None outstanding. The full byte space is covered by the KAT, so there is no
  unobserved input. This module is the safe first one to land.

## Notes

This is distinct from any Opus-style TOC parser. The smpl TOC is a different bit
layout; do not conflate the two. The `StdOpus` branch is the routing hook that
sends `(b & 0xC0) == 0xC0` frames to a standard Opus decoder, not to MLow.
