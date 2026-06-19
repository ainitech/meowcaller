<!-- Datasheet = three things only: the reference source VERBATIM, the Go envelope
     (signatures, no bodies), and implementation suggestions. No behavioral summary,
     no implementation. The verbatim source is the only authoritative content. -->

# Datasheet: `mlow/red`

SplitRed depacketization: the outermost wire layer of an RTP audio payload that
carries redundant copies of recent frames ahead of the main frame. Transport
layer; applied only when redundancy was negotiated, otherwise the payload is one
bare frame and this layer must not run.

**Validation vector:** (integration — no single vector). Pinned by the inline unit
tests: one redundant block plus main, header-only (no redundant) plus main, empty
packet, and the rejection of a bare high-bit-set frame.

## Reference source (verbatim — authoritative)


```rust
//! MLow RED ("SplitRed") depacketization — the OUTERMOST wire layer of a WhatsApp MLow RTP audio
//! payload (WASM func 3819). It is OPTIONAL: applied only when the call negotiated
//! `mlow_red_redundancy_level > 0`. When off (the common case, and our captures), the RTP payload
//! is a single BARE MLow frame with no wrapper and this MUST NOT be applied (a bare frame's first
//! byte has its high bit set and would be misread as a redundant block header).
//!
//! On-wire (N = redundancy): `[ red_hdr[0..N] (2B each) ][ main_marker (1B) ][ red_payloads ][ main_payload ]`.
//! `red_hdr[i]`: byte0 = `0x80 | (time_code & 0x7f)`, byte1 = payload size. `main_marker`: high bit
//! clear, low 7 bits = main time offset. Ported from the Go reference (`mlow_red.go`).

/// One frame extracted from a SplitRed payload: raw MLow frame bytes (TOC + body) plus RED metadata.
/// `data` borrows the input payload (no copy).
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct MlowFrame<'a> {
    pub data: &'a [u8],
    pub time_code: u8,
    pub is_main: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum RedError {
    PktSizeZero,
    HeaderTooShort,
    RedundantTooShort,
    MainTooShort,
}

/// Parse a SplitRed RED packet into its frames (redundant blocks in header order, then the main
/// frame last). Exact port of WASM func 3819 — errors on the same malformed inputs (including a
/// bare MLow frame whose high-bit-set first byte makes it un-parseable as SplitRed). Only call when
/// RED was negotiated; for a bare-frame stream feed the whole RTP payload as one MLow frame.
pub(crate) fn depack_split_red(p: &[u8]) -> Result<Vec<MlowFrame<'_>>, RedError> {
    let n = p.len();
    if n == 0 {
        return Err(RedError::PktSizeZero);
    }
    struct RedBlock {
        code: u8,
        size: u8,
    }
    let mut red: Vec<RedBlock> = Vec::new();
    let mut cur = 0usize;
    let mut rem = n;
    loop {
        if rem == 0 {
            return Err(RedError::HeaderTooShort);
        }
        let b0 = p[cur];
        if b0 < 0x80 {
            // main marker (high bit clear) terminates the header run
            if rem <= 1 {
                return Err(RedError::MainTooShort);
            }
            break;
        }
        if rem <= 2 {
            return Err(RedError::RedundantTooShort);
        }
        let size = p[cur + 1];
        if size as usize + 2 >= rem {
            return Err(RedError::RedundantTooShort);
        }
        red.push(RedBlock {
            code: b0 & 0x7f,
            size,
        });
        cur += 2;
        rem -= size as usize + 2;
    }

    let main_code = p[cur] & 0x7f;
    cur += 1;

    let mut frames = Vec::with_capacity(red.len() + 1);
    for r in &red {
        frames.push(MlowFrame {
            data: &p[cur..cur + r.size as usize],
            time_code: r.code,
            is_main: false,
        });
        cur += r.size as usize;
    }
    let main_size = rem - 1; // total - header_size - sum(redundant sizes)
    frames.push(MlowFrame {
        data: &p[cur..cur + main_size],
        time_code: main_code,
        is_main: true,
    });
    log::debug!(
        "mlow RED: {} redundant + 1 main frame ({}B main)",
        red.len(),
        main_size
    );
    Ok(frames)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn depack_one_redundant_plus_main() {
        // N=1: hdr [0x85,0x03] [main_marker 0x00] | red payload [AA BB CC] | main [50 11 22 33].
        let p = [0x85, 0x03, 0x00, 0xAA, 0xBB, 0xCC, 0x50, 0x11, 0x22, 0x33];
        let frames = depack_split_red(&p).unwrap();
        assert_eq!(frames.len(), 2);
        assert_eq!(
            frames[0],
            MlowFrame {
                data: &[0xAA, 0xBB, 0xCC],
                time_code: 5,
                is_main: false
            }
        );
        assert_eq!(
            frames[1],
            MlowFrame {
                data: &[0x50, 0x11, 0x22, 0x33],
                time_code: 0,
                is_main: true
            }
        );
    }

    #[test]
    fn depack_no_redundant_just_main() {
        // header is just the main marker (0x00), then the main payload.
        let p = [0x00, 0x50, 0x11, 0x22];
        let frames = depack_split_red(&p).unwrap();
        assert_eq!(frames.len(), 1);
        assert!(frames[0].is_main);
        assert_eq!(frames[0].data, &[0x50, 0x11, 0x22]);
    }

    #[test]
    fn empty_packet_errors() {
        assert_eq!(depack_split_red(&[]), Err(RedError::PktSizeZero));
    }

    #[test]
    fn bare_frame_is_rejected() {
        // A bare MLow frame (high-bit-set TOC like a DTX 0x90) must NOT parse as SplitRed.
        assert!(depack_split_red(&[0x90, 0x01, 0x02]).is_err());
    }
}
```

## Go envelope (signatures only)

The corresponding Go declarations — exported types and function **signatures with
no bodies**. This is the surface to implement; it is not the implementation.

```go
package mlow

import "errors"

// One frame extracted from a SplitRed payload: raw MLow frame bytes (TOC + body) plus RED metadata.
// Data is a subslice of the input payload (no copy).
type MlowFrame struct {
	Data     []byte
	TimeCode uint8
	IsMain   bool
}

var (
	ErrPktSizeZero       = errors.New("mlow red: packet size zero")
	ErrHeaderTooShort    = errors.New("mlow red: header too short")
	ErrRedundantTooShort = errors.New("mlow red: redundant block too short")
	ErrMainTooShort      = errors.New("mlow red: main frame too short")
)

// Parse a SplitRed RED packet into its frames (redundant blocks in header order, then the main
// frame last). Only call when RED was negotiated; for a bare-frame stream feed the whole RTP
// payload as one MLow frame.
func DepackSplitRed(p []byte) ([]MlowFrame, error)
```

## Implementation suggestions (guidance, not authoritative)

- `usize` → Go `int`; `u8` → `byte`/`uint8`. The header math (`size as usize + 2`,
  `rem -= size + 2`, `main_size = rem - 1`) is all small non-negative integer
  arithmetic; plain `int` is fine but keep the comparisons exactly
  (`size as usize + 2 >= rem` is `>=`, not `>`).
- The Rust returns borrowed subslices (`&p[cur..cur+size]`). In Go, return slices
  that alias the input `p` (`p[cur:cur+size]`) to match the "no copy" contract — do
  not allocate per frame. `TODO(human):` if the caller may retain frames past the
  lifetime of `p`, decide whether to copy instead.
- The `RedError` enum maps to distinct sentinel `error` values (above) so callers
  can branch on the failure mode; the inline tests only check `is_err()` for the
  bare-frame case, but the redundant/main/empty cases assert specific variants.
- The loop terminates the header run on the first byte with the high bit clear
  (`b0 < 0x80`), which is the main marker; every preceding byte must have the high
  bit set (`0x80 | code`). A bare MLow frame's TOC has the high bit set, so it never
  reaches a valid main marker and errors out — that rejection is the whole reason
  this layer must only run when RED was negotiated.
- The reference logs at debug level (`log::debug!`). In Go this is optional; drop it
  or wire it to the project's logger. It is not part of the behavior the tests pin.
- Bounds: each subslice is taken only after the header loop validated the sizes, so
  no further bounds check is needed if the arithmetic is reproduced exactly. Go will
  still panic on an out-of-range slice, which is an acceptable backstop, but the
  intent is that valid inputs never hit it.
