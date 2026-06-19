# Datasheet: `mlow/decoder`

The top-level stateful decoder: strips RED redundancy, routes on the TOC byte, and
drives the active-frame decode (three chained 20 ms internal frames: LSF → pulses →
pitch/gains → CELP synthesis) into one 60 ms / 960-sample PCM frame. Media layer;
this is the integration point that ties every other `mlow` module together.

**Validation vector:** `e2e_vectors.json` and `inbound_capture_frames.json` — the
real captured wire frames (hex, one per line) decoded end-to-end and compared
against the reference PCM. This is the integration milestone (no single isolated
vector). Copy both verbatim into `mlow/testdata/`.

## Reference source (verbatim — authoritative)


### `decoder.rs`

```rust
//! MLow top-level decoder: RED strip → TOC routing → active-frame decode (3 chained 20 ms internal
//! frames: LSF → pulses → pitch/gains → CELP synthesis) → 60 ms PCM. The bitstream parse is pinned
//! against the WASM; the synthesis (`smpl_celpdec`) is a faithful port of the smpl_opus C decoder
//! (excitation in the codec's float domain + gen_noise + LPC synthesis). The cross-frame predictor
//! and synthesis history persist across calls because the stream is continuous.

use super::rangecoder::RangeDecoder;
use super::red::depack_split_red;
use super::smpl_celpdec::CelpDecParams;
use super::smpl_decode::{decode_smpl_lsf, load_smpl_tables};
use super::smpl_gains::decode_smpl_gains;
use super::smpl_mem::load_smpl_mem;
use super::smpl_pitch::decode_smpl_pitch;
use super::smpl_pulse::decode_smpl_pulses;
use super::smpl_synth::{
    SMPL_INTF_LEN, SmplDecoderState, load_smpl_synth_tables, smpl_reconstruct_nlsf,
};
use super::toc::parse_mlow_toc;

const OPUS_FRAME_SAMPS: usize = 960; // 60 ms @ 16 kHz

/// Stateful pure-Rust MLow decoder. Decodes one RTP payload (a bare MLow frame, or a SplitRed
/// packet when redundancy was negotiated) into a 60 ms / 960-sample PCM frame at 16 kHz.
pub struct MlowDecoder {
    state: SmplDecoderState,
    redundancy: i32,
}

impl Default for MlowDecoder {
    fn default() -> Self {
        Self::new()
    }
}

impl MlowDecoder {
    pub fn new() -> Self {
        MlowDecoder {
            state: SmplDecoderState::default(),
            redundancy: 0,
        }
    }

    /// Set the negotiated RED redundancy level (0 = bare frames, the common case).
    pub fn set_redundancy(&mut self, n: i32) {
        self.redundancy = n;
    }

    /// Clear the cross-frame state (call at a stream discontinuity).
    pub fn reset(&mut self) {
        self.state = SmplDecoderState::default();
    }

    /// Decode one RTP MLow payload into a 60 ms (960-sample) PCM frame, float in [-1, 1].
    pub fn decode(&mut self, payload: &[u8]) -> Vec<f32> {
        if payload.is_empty() {
            return vec![0.0; OPUS_FRAME_SAMPS];
        }
        if self.redundancy > 0 {
            return match depack_split_red(payload) {
                // the main (current) frame is last; copy it out before the borrow ends.
                Ok(frames) => {
                    let main = frames.last().map(|f| f.data.to_vec()).unwrap_or_default();
                    self.decode_frame(&main)
                }
                Err(e) => {
                    log::warn!("mlow RED depacketization failed: {e:?}");
                    vec![0.0; OPUS_FRAME_SAMPS]
                }
            };
        }
        self.decode_frame(payload)
    }

    fn decode_frame(&mut self, frame: &[u8]) -> Vec<f32> {
        if frame.is_empty() {
            return vec![0.0; OPUS_FRAME_SAMPS];
        }
        let toc = parse_mlow_toc(frame[0]);
        let out_len = if toc.std_opus {
            (16000 / 1000 * toc.frame_ms) as usize
        } else {
            (toc.sample_rate / 1000 * toc.frame_ms) as usize
        };
        if toc.std_opus {
            log::debug!(
                "mlow: standard-opus TOC 0x{:02x} -> {out_len} samples silence",
                frame[0]
            );
            return vec![0.0; out_len];
        }
        if toc.sid || !toc.active {
            log::debug!(
                "mlow: DTX/SID TOC 0x{:02x} -> {out_len} samples silence",
                frame[0]
            );
            return vec![0.0; out_len];
        }
        self.decode_active_frame(frame, out_len)
    }

    fn decode_active_frame(&mut self, frame: &[u8], out_len: usize) -> Vec<f32> {
        let config = (frame[0] >> 2) as usize & 1;
        let tbl = load_smpl_tables();
        let synth_t = load_smpl_synth_tables();
        let mem = load_smpl_mem();
        let mut dec = RangeDecoder::new(&frame[1..]);

        // The low_rate bit of the smpl TOC (this capture is low_rate==0; the synth gates on it).
        let low_rate = (frame[0] >> 2) & 1 != 0;

        let mut out: Vec<f32> = Vec::with_capacity(3 * SMPL_INTF_LEN);
        // Collect the per-40-block lags (8 per frame, 24 per packet) and the average normalized
        // bitrate for the per-packet harmonic postfilter.
        let mut packet_lags: Vec<f32> = Vec::with_capacity(3 * 8);
        let mut avg_norm_br = 0.0f32;
        for f in 0..3 {
            let lsf = decode_smpl_lsf(&mut dec, tbl, &mut self.state.lstate, config, f);
            let pulses = decode_smpl_pulses(
                &mut dec,
                mem,
                SMPL_INTF_LEN as i32,
                4,
                1,
                config as i32,
                lsf.stage1,
            );
            let voiced = lsf.stage1 == 1;
            let mut params = CelpDecParams {
                voiced,
                sf_pulses: pulses.subfr,
                fcbg_idx: [0; 4],
                nrgres_dbq_q14: [0; 4],
                acbg_idx: [0; 4],
                block_lags: [0.0; 8],
                total_pulses: pulses.subfr.iter().sum(),
            };
            if voiced {
                let pr = decode_smpl_pitch(
                    &mut dec,
                    mem,
                    &mut self.state.lstate,
                    SMPL_INTF_LEN as i32,
                    4,
                    config as i32,
                    pulses.subfr,
                );
                // lag = laginds*0.5 + SMPL_MIN_PITCH_LAG, clamped; one per 40-block, 8 per frame.
                for b in 0..8 {
                    params.block_lags[b] =
                        ((pr.block_lags[b] as f64 * 0.5 + 32.0).min(320.0)) as f32;
                }
                for sf in 0..4 {
                    params.acbg_idx[sf] = pr.gain_idx[sf];
                    // The voiced FCB gain index is decoded in the pitch block (filt_idx).
                    params.fcbg_idx[sf] = pr.filt_idx[sf].max(0);
                }
            } else {
                let g = decode_smpl_gains(&mut dec, mem, 4, pulses.subfr);
                // D5: the Rust gains decode reads the same bits as the C decode_lb_unvoiced; gain_q is
                // the C nrgres_dbq_Q14 and nrg_res is the C fcbg_idx (validated bit-exact).
                params.nrgres_dbq_q14 = g.gain_q;
                params.fcbg_idx = g.nrg_res;
            }
            packet_lags.extend_from_slice(&params.block_lags);
            avg_norm_br += super::smpl_gennoise::smpl_get_normalized_bitrate(
                params.total_pulses,
                SMPL_INTF_LEN as i32,
            );

            let nlsf = smpl_reconstruct_nlsf(
                synth_t,
                lsf.stage1 as usize,
                config,
                lsf.grid as usize,
                &lsf.stage2,
                &self.state.prev_nlsf,
            );
            let mut sig = [0f32; SMPL_INTF_LEN];
            self.state.celp.synth_frame(
                &nlsf,
                lsf.extra as usize,
                &pulses.pulses,
                &params,
                low_rate,
                SMPL_INTF_LEN as i32,
                &mut sig,
            );
            self.state.prev_nlsf = nlsf;
            out.extend_from_slice(&sig);
        }

        // Per-packet harmonic postfilter (the codec's final pitch comb + 48-sample group delay), run
        // once over the whole packet with the 24 per-40-block lags and the average normalized bitrate.
        let plen = out.len();
        super::smpl_harm_postfilter::smpl_harm_postfilter(
            &mut self.state.harm,
            &mut out,
            plen,
            &packet_lags,
            packet_lags.len(),
            avg_norm_br / 3.0,
        );

        // The C-domain synthesis output is already float in [-1, 1].
        let mut pcm: Vec<f32> = out.iter().map(|&v| v.clamp(-1.0, 1.0)).collect();
        if out_len > 0 && out_len != pcm.len() {
            pcm.resize(out_len, 0.0);
        }
        if dec.err != 0 {
            log::warn!("mlow: range-coder error after active-frame decode (parse desync)");
        }
        log::debug!(
            "mlow: active frame decoded -> {} samples (config={config})",
            pcm.len()
        );
        pcm
    }
}

/// Per-subframe param snapshot for the param-decode-match (T1) test.
#[cfg(test)]
pub(crate) struct DiagParam {
    pub(crate) packet: usize,
    pub(crate) frame: usize,
    pub(crate) sf: usize,
    pub(crate) voiced: bool,
    /// Rust `gain_q` — equals the C `nrgres_dbq_Q14` (validated bit-exact).
    pub(crate) nrgres_dbq_q14: i32,
    /// Rust per-subframe `nrg_res` / voiced `filt_idx` symbol — equals the C `fcbg_idx`.
    pub(crate) fcbg_idx: i32,
}

/// Re-run the active-frame decode over the capture and capture per-subframe unvoiced params, keyed
/// by (packet, frame, sf), to compare against the instrumented-C dump.
#[cfg(test)]
pub(crate) fn diag_decode_params() -> Vec<DiagParam> {
    let frames: Vec<String> =
        serde_json::from_str(include_str!("testdata/inbound_capture_frames.json")).unwrap();
    let tbl = load_smpl_tables();
    let mem = load_smpl_mem();
    let mut lstate = super::smpl_decode::SmplLsfState::default();
    let mut out = Vec::new();
    for (packet, hex_frame) in frames.iter().enumerate() {
        let frame = hex::decode(hex_frame).unwrap();
        if frame.is_empty() {
            continue;
        }
        let toc = parse_mlow_toc(frame[0]);
        if toc.std_opus || toc.sid || !toc.active {
            continue;
        }
        let config = (frame[0] >> 2) as usize & 1;
        let mut dec = RangeDecoder::new(&frame[1..]);
        for f in 0..3 {
            let lsf = decode_smpl_lsf(&mut dec, tbl, &mut lstate, config, f);
            let pulses = decode_smpl_pulses(
                &mut dec,
                mem,
                SMPL_INTF_LEN as i32,
                4,
                1,
                config as i32,
                lsf.stage1,
            );
            if lsf.stage1 == 1 {
                let pr = decode_smpl_pitch(
                    &mut dec,
                    mem,
                    &mut lstate,
                    SMPL_INTF_LEN as i32,
                    4,
                    config as i32,
                    pulses.subfr,
                );
                for sf in 0..4 {
                    out.push(DiagParam {
                        packet,
                        frame: f,
                        sf,
                        voiced: true,
                        nrgres_dbq_q14: pr.gain_idx[sf],
                        fcbg_idx: pr.filt_idx[sf],
                    });
                }
            } else {
                let g = decode_smpl_gains(&mut dec, mem, 4, pulses.subfr);
                for sf in 0..4 {
                    out.push(DiagParam {
                        packet,
                        frame: f,
                        sf,
                        voiced: false,
                        nrgres_dbq_q14: g.gain_q[sf],
                        fcbg_idx: g.nrg_res[sf],
                    });
                }
            }
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    // End-to-end: decode the whole capture and compare against the libopus `useSmpl` reference
    // (`ref_usesmpl_expected.raw`, the real smpl_opus codec, validated bit-exact vs the WASM).
    //
    // The previous version validated against the Go reference (`e2e_vectors.json`), but that decode
    // was proven wrong: it used the int16-domain `*nrgres` excitation with no shaped noise (the Go
    // tail-off bug), and correlates ~0 with the real codec. The faithful target is the C useSmpl
    // output. With the per-block voiced ACB/LTP lags, the HP postfilter, and the harmonic postfilter
    // (which emits the codec's SMPL_TOT_POSTFILT_DELAY = 48-sample group delay) all in place, the
    // decode now aligns sample-for-sample at lag 0.
    #[test]
    fn e2e_decode_matches_usesmpl() {
        let frames: Vec<String> =
            serde_json::from_str(include_str!("testdata/inbound_capture_frames.json"))
                .expect("inbound_capture_frames.json");
        let refp: Vec<f32> = include_bytes!("testdata/ref_usesmpl_expected.raw")
            .chunks_exact(2)
            .map(|b| i16::from_le_bytes([b[0], b[1]]) as f32 / 32768.0)
            .collect();

        let mut dec = MlowDecoder::new();
        let mut out: Vec<f32> = Vec::new();
        for hex_frame in &frames {
            let frame = hex::decode(hex_frame).unwrap();
            out.extend_from_slice(&dec.decode(&frame));
        }
        assert_eq!(out.len(), refp.len(), "decode length vs reference");

        // Aligned at lag 0 now (the harmonic postfilter emits the 48-sample group delay).
        const LAG: usize = 0;
        let n = refp.len() - LAG;
        let (r, o) = (&refp[LAG..LAG + n], &out[..n]);
        let mr: f64 = r.iter().map(|&v| v as f64).sum::<f64>() / n as f64;
        let mo: f64 = o.iter().map(|&v| v as f64).sum::<f64>() / n as f64;
        let (mut sxy, mut sxx, mut syy) = (0f64, 0f64, 0f64);
        for i in 0..n {
            let dr = r[i] as f64 - mr;
            let dz = o[i] as f64 - mo;
            sxy += dr * dz;
            sxx += dr * dr;
            syy += dz * dz;
        }
        let corr = sxy / (sxx * syy).sqrt();
        assert!(
            corr > 0.95,
            "lag-0 corr {corr:.4} vs useSmpl reference (was 0.31 before the gen_noise/domain port, \
             0.66 before the per-block lags + postfilters)"
        );
    }
}
```

### `params.rs`

```rust
//! Encoder-facing per-frame parameters: the structured output of the analysis, consumed by the
//! entropy encoder. Ported from the Go reference (`smpl_params.go`). The pulse/pitch blocks also
//! carry the raw entropy symbols the encoder replays (the structured counts alone are lossy w.r.t.
//! the exact bitstream).

#[derive(Default, Clone)]
pub(crate) struct SmplLsfParams {
    pub stage1: i32,
    pub grid: i32,
    pub stage2: [i32; 16],
    pub extra: i32,
}

/// One uniform raw-symbol write (`encode(sym, sym+1, 1<<nbits)`).
#[derive(Clone, Copy)]
pub(crate) struct SmplRawSym {
    pub sym: u32,
    pub nbits: u32,
}

#[derive(Default, Clone)]
pub(crate) struct SmplPulseParams {
    pub total: i32,
    pub subfr: [i32; 4],
    /// Per-position run-length symbols (decodeCDF results) in read order across all subframes.
    pub mag_runs: Vec<i32>,
    /// Per-batch raw sign symbols in order.
    pub sign_syms: Vec<SmplRawSym>,
}

#[derive(Default, Clone)]
pub(crate) struct SmplGainParams {
    pub gain_main: i32,
    pub gain_delta: i32,
    pub nrg_res: [i32; 4],
}

#[derive(Default, Clone)]
#[allow(dead_code)] // populated for the voiced path, not read on the unvoiced encode
pub(crate) struct SmplPitchParams {
    pub gain_idx: [i32; 4],
    pub filt_idx: [i32; 4],
    pub lag_abs_sym: i32,
    pub lag_delta_sym: i32,
    pub lag_ref_sym: i32,
    pub lag: i32,
    pub contour: i32,
    pub fine_read: bool,
    pub fine_sym: i32,
    pub frac_syms: Vec<i32>,
}

#[derive(Default, Clone)]
pub(crate) struct SmplInternalParams {
    pub lsf: SmplLsfParams,
    pub pulses: SmplPulseParams,
    #[allow(dead_code)]
    pub has_pitch: bool,
    #[allow(dead_code)]
    pub pitch: SmplPitchParams,
    pub gains: SmplGainParams,
}

/// Full decoded/analyzed parameter set for one 60 ms MLow frame.
pub(crate) struct SmplFrameParams {
    pub toc: u8,
    pub config: usize,
    pub internal: [SmplInternalParams; 3],
}
```

### `param_decode_match.rs`

```rust
//! T1: validate that the Rust unvoiced/voiced parameter decode produces the SAME per-subframe
//! `nrgres_dbq_Q14` and `fcbg_idx` as the smpl_opus C reference (`gennoise_params_dump.json`).
//!
//! The Rust gains decode (`decode_smpl_gains`) reads the same bits as the C `decode_lb_unvoiced`,
//! just under different field names: its `gain_q` IS the C `nrgres_dbq_Q14` and its per-subframe
//! `nrg_res` symbol IS the C `fcbg_idx`. The voiced FCB gain index is the pitch block's `filt_idx`.
//! This test pins that correspondence exactly so the excitation/gen_noise inputs stay faithful.
#![cfg(test)]

use super::decoder::diag_decode_params;
use serde_json::Value;
use std::collections::HashMap;

#[test]
fn nrgres_fcbg_match_c_reference() {
    let cdump: Value =
        serde_json::from_str(include_str!("testdata/gennoise_params_dump.json")).unwrap();
    let carr = cdump.as_array().unwrap();
    let mut cmap: HashMap<(i64, i64, i64), &Value> = HashMap::new();
    for c in carr {
        cmap.insert(
            (
                c["packet"].as_i64().unwrap(),
                c["frame"].as_i64().unwrap(),
                c["sf"].as_i64().unwrap(),
            ),
            c,
        );
    }

    let rust = diag_decode_params();
    let (mut uv_nrgres, mut uv_fcbg, mut v_fcbg, mut voiced_class) = (0, 0, 0, 0);
    for r in &rust {
        let Some(c) = cmap.get(&(r.packet as i64, r.frame as i64, r.sf as i64)) else {
            continue;
        };
        let cv = c["voiced"].as_i64().unwrap() == 1;
        let cnrg = c["nrgres_dbq_Q14"].as_i64().unwrap() as i32;
        let cfcbg = c["fcbg_idx"].as_i64().unwrap() as i32;
        let cnp = c["sf_pulses"].as_i64().unwrap() as i32;

        assert_eq!(
            r.voiced,
            cv,
            "voiced flag at {:?}",
            (r.packet, r.frame, r.sf)
        );
        voiced_class += 1;
        if cv {
            // Voiced: the FCB gain index (filt_idx) must match the C fcbg_idx where pulses exist.
            if cnp > 0 {
                assert_eq!(
                    r.fcbg_idx,
                    cfcbg,
                    "voiced fcbg_idx at {:?}",
                    (r.packet, r.frame, r.sf)
                );
                v_fcbg += 1;
            }
        } else {
            assert_eq!(
                r.nrgres_dbq_q14,
                cnrg,
                "unvoiced nrgres_dbq_Q14 at {:?}",
                (r.packet, r.frame, r.sf)
            );
            uv_nrgres += 1;
            if cnp > 0 {
                assert_eq!(
                    r.fcbg_idx,
                    cfcbg,
                    "unvoiced fcbg_idx at {:?}",
                    (r.packet, r.frame, r.sf)
                );
                uv_fcbg += 1;
            }
        }
    }
    assert!(
        voiced_class > 0 && uv_nrgres > 0 && uv_fcbg > 0 && v_fcbg > 0,
        "coverage too thin: class={voiced_class} uv_nrgres={uv_nrgres} uv_fcbg={uv_fcbg} v_fcbg={v_fcbg}"
    );
}
```

## Go envelope (signatures only)

The corresponding Go declarations — exported types and function **signatures with
no bodies**. This is the surface to implement; it is not the implementation.

```go
package mlow

const OpusFrameSamps = 960 // 60 ms @ 16 kHz

// MlowDecoder is the stateful top-level MLow decoder. The cross-frame predictor and
// synthesis history persist across Decode calls.
type MlowDecoder struct {
	state      SmplDecoderState
	redundancy int
}

func NewMlowDecoder() *MlowDecoder

// SetRedundancy sets the negotiated RED redundancy level (0 = bare frames).
func (d *MlowDecoder) SetRedundancy(n int)

// Reset clears the cross-frame state (call at a stream discontinuity).
func (d *MlowDecoder) Reset()

// Decode turns one RTP MLow payload into a 60 ms (960-sample) PCM frame, float in [-1, 1].
func (d *MlowDecoder) Decode(payload []byte) []float32

func (d *MlowDecoder) decodeFrame(frame []byte) []float32

func (d *MlowDecoder) decodeActiveFrame(frame []byte, outLen int) []float32

// SmplLsfParams is the LSF block of one internal frame.
type SmplLsfParams struct {
	Stage1 int32
	Grid   int32
	Stage2 [16]int32
	Extra  int32
}

// SmplRawSym is one uniform raw-symbol write (encode(sym, sym+1, 1<<nbits)).
type SmplRawSym struct {
	Sym   uint32
	NBits uint32
}

type SmplPulseParams struct {
	Total    int32
	Subfr    [4]int32
	MagRuns  []int32
	SignSyms []SmplRawSym
}

type SmplGainParams struct {
	GainMain  int32
	GainDelta int32
	NrgRes    [4]int32
}

type SmplPitchParams struct {
	GainIdx     [4]int32
	FiltIdx     [4]int32
	LagAbsSym   int32
	LagDeltaSym int32
	LagRefSym   int32
	Lag         int32
	Contour     int32
	FineRead    bool
	FineSym     int32
	FracSyms    []int32
}

type SmplInternalParams struct {
	Lsf      SmplLsfParams
	Pulses   SmplPulseParams
	HasPitch bool
	Pitch    SmplPitchParams
	Gains    SmplGainParams
}

// SmplFrameParams is the full decoded/analyzed parameter set for one 60 ms MLow frame.
type SmplFrameParams struct {
	TOC      uint8
	Config   int
	Internal [3]SmplInternalParams
}
```

## Implementation suggestions (guidance, not authoritative)

- `i32`/`usize` map to Go `int32`/`int`; PCM samples are `f32` → `float32`. `&[u8]`
  payloads become `[]byte` and `Vec<f32>` returns become `[]float32`.
- `decode` returns `Vec` of silence on empty/SID/std-opus/error paths; in Go return a
  freshly allocated `[]float32` of the computed length (never `nil` for a decoded
  frame). The `log::warn!`/`log::debug!` calls are non-fatal — map to your logger or
  drop them; they never change the returned samples.
- State carried across calls (`SmplDecoderState`: `lstate`, `prev_nlsf`, `celp`,
  `harm`) must live in the struct, not be re-zeroed per call — the stream is
  continuous. `Reset` re-zeroes all of it.
- The three internal frames mutate one shared `RangeDecoder` in sequence; the parse is
  position-dependent, so the calls must run strictly in order 0,1,2.
- `out_len` resize semantics: clamp samples to [-1, 1], then resize to `out_len` only
  when it differs from the synthesized length. Use a slice `resize`/reslice that
  zero-fills on grow.
- `total_pulses` is `pulses.subfr.iter().sum()`; `block_lags` are derived as
  `lagq * 0.5 + 32`, clamped to 320. Keep these in `float64` before the final `float32`
  cast to match the Rust `as f64 ... as f32` rounding.
- `TODO(human)`: decide whether `MlowDecoder` is value or pointer-receiver in Go and
  whether `SmplDecoderState` sub-objects (CELP/harmonic-postfilter) are owned by value
  or pointer — the Rust holds them by value inside `state`.
- `TODO(human)`: the `#[cfg(test)]` `diag_decode_params` / `DiagParam` path exists only
  to pin per-subframe params against `gennoise_params_dump.json`; decide whether to port
  it as a test helper or omit it from the production package.
```

