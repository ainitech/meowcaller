<!-- Datasheet = three things only: the reference source VERBATIM, the Go envelope
     (signatures, no bodies), and implementation suggestions. No behavioral summary,
     no implementation. The verbatim source is the only authoritative content. -->

# Datasheet: `mlow/gains`

Decodes the per-subframe quantized log-gains and energy-residual symbols of one
internal frame from the range-coded bitstream (the unvoiced gains block).
Media layer (codec decode pipeline).

**Validation vector:** `gains_vectors.json` — the per-subframe `gain_q[]` and
`nrg_res[]` this module must reproduce byte-exact. Copy it verbatim into
`mlow/testdata/`.

## Reference source (verbatim — authoritative)

```rust
//! MLow gains + nrgres decode (func 3545 GAINS block), run for UNVOICED (stage-1 selector == 0)
//! internal frames — mutually exclusive with the pitch block. Ported from the Go reference
//! (`smpl_gains.go`). The config=0 active capture is all voiced, so this never runs on real audio
//! (the synth uses gainQ=0 for voiced frames); the test below validates the arithmetic byte-exact by
//! force-running it on a voiced frame's post-pulse decoder state.

use super::rangecoder::RangeDecoder;
use super::smpl_mem::SmplMem;

pub(crate) struct SmplGainResult {
    /// Per-subframe quantized log-gain (Q-domain).
    pub(crate) gain_q: [i32; 4],
    /// Per-subframe energy-residual symbol (only subframes with pulses are read).
    pub(crate) nrg_res: [i32; 4],
}

/// Decode the gains+nrgres reads (the p3==4 path). `subfr_counts` are the per-subframe pulse counts.
pub(crate) fn decode_smpl_gains(
    dec: &mut RangeDecoder,
    mem: &SmplMem,
    p3: i32,
    subfr_counts: [i32; 4],
) -> SmplGainResult {
    let mut res = SmplGainResult {
        gain_q: [0; 4],
        nrg_res: [0; 4],
    };
    let g_nrg = mem.g_nrg;

    // main gain (n=85) + delta gain (n=99)
    let gain_main = dec.decode_cdf(&mem.cdf_at(g_nrg.wrapping_add(0x1362), 85));
    let gain_delta = dec.decode_cdf(&mem.cdf_at(g_nrg.wrapping_add(0x1098), 99));
    let cfg_sel = 2i32;

    // gain reconstruction. The index sf + p3*gain_delta is NOT bounded to the visible 64-entry array
    // (gain_delta up to 98) — the WASM reads adjacent rodata, which the heap window reproduces.
    let gain_tab_addr: u32 = if p3 == 4 { 0xf35f0 } else { 0xf3970 };
    let off6 = p3 * gain_delta;
    let base7 =
        gain_main * (mem.i16(0xf35e0u32.wrapping_add((cfg_sel as u32) * 2)) as i32) - 0x154000;
    for sf in 0..(p3 as usize).min(4) {
        let cbv = mem.i16(gain_tab_addr.wrapping_add(((sf as i32 + off6) as u32) * 2)) as i32;
        res.gain_q[sf] = base7 + (cbv << 4);
    }

    // nrgres: per-subframe bucketed CDF (n=92) with a gain-derived address shift.
    let nrg_base = g_nrg.wrapping_add((cfg_sel as u32) * 0x588);
    for (sf, &cnt) in subfr_counts.iter().enumerate().take((p3 as usize).min(4)) {
        if cnt <= 0 {
            continue;
        }
        let bucket = if cnt >= 30 { 3 } else { (cnt & 0xffff) / 10 };
        let cdfp = nrg_base.wrapping_add((bucket as u32) * 0x162);
        // g = clamp((gainQ[sf]+8192)>>14, floor -85); neg_part = (g>>31)&g; addr = cdfp - 2*neg_part.
        let mut g = (res.gain_q[sf] + 8192) >> 14;
        if g < -85 {
            g = -85;
        }
        let neg_part = (g >> 31) & g;
        let off = cdfp.wrapping_sub((neg_part << 1) as u32);
        res.nrg_res[sf] = dec.decode_cdf(&mem.cdf_at(off, 92));
    }
    log::trace!(
        "mlow gains: main={gain_main} delta={gain_delta} gain_q={:?} nrg_res={:?}",
        res.gain_q,
        res.nrg_res
    );
    res
}

#[cfg(test)]
mod tests {
    use super::super::smpl_decode::{SmplLsfState, decode_smpl_lsf, load_smpl_tables};
    use super::super::smpl_mem::load_smpl_mem;
    use super::super::smpl_pulse::decode_smpl_pulses;
    use super::*;
    use serde_json::Value;

    // Force-runs gains on each active frame's post-pulse decoder state (semantically a voiced frame,
    // so the bits aren't "really" gains — but the decode is deterministic and must match Go exactly,
    // validating the gains arithmetic + memory reads byte-for-byte).
    #[test]
    fn gains_match_go() {
        let recs: Value = serde_json::from_str(include_str!("testdata/gains_vectors.json"))
            .expect("gains_vectors");
        let tbl = load_smpl_tables();
        let mem = load_smpl_mem();
        let arr = recs.as_array().unwrap();
        assert!(!arr.is_empty());
        let as_i32 = |v: &Value| -> Vec<i32> {
            v.as_array()
                .unwrap()
                .iter()
                .map(|x| x.as_i64().unwrap() as i32)
                .collect()
        };
        for rec in arr {
            let frame = hex::decode(rec["frame"].as_str().unwrap()).unwrap();
            let mut st = SmplLsfState::default();
            let mut dec = RangeDecoder::new(&frame[1..]);
            let lsf = decode_smpl_lsf(&mut dec, tbl, &mut st, 0, 0);
            let pulses = decode_smpl_pulses(&mut dec, mem, 320, 4, 1, 0, lsf.stage1);
            let g = decode_smpl_gains(&mut dec, mem, 4, pulses.subfr);
            assert_eq!(g.gain_q.to_vec(), as_i32(&rec["gain_q"]), "gain_q");
            assert_eq!(g.nrg_res.to_vec(), as_i32(&rec["nrg_res"]), "nrg_res");
        }
    }
}
```

## Go envelope (signatures only)

```go
package mlow

// SmplGainResult holds the decoded per-subframe gains and energy-residual symbols.
type SmplGainResult struct {
	GainQ  [4]int32 // per-subframe quantized log-gain (Q-domain)
	NrgRes [4]int32 // per-subframe energy-residual symbol (only subframes with pulses are read)
}

// DecodeSmplGains decodes the gains+nrgres reads (the p3==4 path). subfrCounts
// are the per-subframe pulse counts.
func DecodeSmplGains(dec *RangeDecoder, mem *SmplMem, p3 int32, subfrCounts [4]int32) SmplGainResult
```

## Implementation suggestions (guidance, not authoritative)

- Gain values are `i32` Q-domain → Go `int32`. The arithmetic-right-shift semantics
  matter: `>>14` and `>>31` on signed values must be arithmetic (sign-extending),
  which Go gives for signed `int32`; do not use an unsigned type for `gain_q`.
- `neg_part = (g >> 31) & g` is the classic "min(g,0)" via sign-mask; with signed
  `int32` shift Go reproduces it directly. Keep `g` signed.
- Table reads use `mem.i16(addr)` (signed 16-bit) widened to `i32`, and `cbv << 4`;
  use `int32(mem.I16(addr))` then shift, preserving the signedness.
- Address math is `wrapping_*` on `u32` (heap-window offsets, deliberately reading
  adjacent rodata past the visible array). Use `uint32` with plain Go operators so
  wrap-around matches.
- `cnt & 0xffff` masks before the `/10` bucket; keep the mask even though `cnt` is
  small, since it is part of the byte-exact contract.
- Depends on `RangeDecoder` (decode_cdf) and `SmplMem` (i16/cdf_at, plus the `g_nrg`
  base). `TODO(human):` confirm those bases/offsets in the heap-window module match
  before wiring this in.
