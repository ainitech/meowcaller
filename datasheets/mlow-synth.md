<!-- Datasheet = three things only: the reference source VERBATIM, the Go envelope
     (signatures, no bodies), and implementation suggestions. No behavioral summary,
     no implementation. The verbatim source is the only authoritative content. -->

# Datasheet: `mlow/synth`

Turns one internal frame's decoded parameters (LSF, pulses, LTP/pitch, gains)
into 16 kHz PCM: NLSF reconstruction, NLSF-to-LPC, pulse excitation scaling, the
adaptive-codebook / long-term-prediction synthesis, and order-16 LPC synthesis.
Media layer; the per-frame core of the decoder.

**Validation vector:** `e2e_vectors.json` — the end-to-end decode vector. Copy it
verbatim into `mlow/testdata/`. (The CELP-decoder path additionally pins its
pre-noise excitation against `exc_pre_lags.json` driven by
`inbound_capture_frames.json`.)

## Reference source (verbatim — authoritative)

`smpl_synth.rs`:

```rust
//! MLow (smpl_audio_codec) SYNTHESIS LB-core: turns the byte-exact decoded parameters (LSF, pulses,
//! LTP, gains) into 16 kHz PCM. Faithful port of WASM func 3597's low-band core (NLSF reconstruct →
//! NLSF2A → pulse excitation × gain → fractional LTP → order-16 LPC synthesis). Ported from the Go
//! reference (`smpl_synth.go` + `smpl_excitation.go`). The synthesis TAIL is real (the original codec
//! C source — tlcs/smpl_opus — confirms the decoder applies an HP pitch-harmonic postfilter + a tilt
//! postfilter; the Go `stageMask==0` claim was wrong). The HP postfilter is ported (`smpl_harmcomb`,
//! gated behind `SMPL_HP_POSTFILTER`); the residual-domain tilt/uv-shaping stages are still WIP.
//!
//! The float constants below are byte-exact copies of the reference's f32 literals (the extra digits
//! round to the same f32 the WASM uses), so excessive-precision is allowed module-wide.
#![allow(clippy::excessive_precision)]

use super::smpl_harmcomb::{HpPostfilterState, smpl_hp_postfilter};
use super::smpl_postfilter::{SmplPostfilterState, smpl_comb_postfilter};
use std::sync::OnceLock;

/// Gate for the Region-1 per-subframe excitation postfilter (func 3524, the harmonic comb).
const SMPL_TAIL_REGION1: bool = false;
/// Gate for the post-LPC HP (pitch-harmonic) postfilter (`smpl_hp_postfilter`).
const SMPL_HP_POSTFILTER: bool = false;

pub(crate) const SMPL_ORDER: usize = 16;
pub(crate) const SMPL_SUBFR_LEN: usize = 80; // 5 ms @ 16 kHz
pub(crate) const SMPL_INTF_LEN: usize = 320; // 20 ms internal frame
pub(crate) const SMPL_SUBFR_COUNT: usize = 4;
pub(crate) const SMPL_LTP_HIST: usize = 728;
const SMPL_FRAC_STATE_LEN: i32 = 728;
pub(crate) const SMPL_VOICED_NORM_GAIN: f64 = 1.0;
const LTP_HIST_LEN: usize = SMPL_LTP_HIST + SMPL_INTF_LEN + 64;

const SMPL_NLSF_WEIGHT_W_MAX: f32 = 999.9999;
const SMPL_NLSF_WEIGHT_EPS: f32 = 0.0009999999;
const SMPL_PI_F32: f32 = 3.1415927410125;
const SMPL_STABILIZE_MAX_LOOPS: i32 = 1000;
const SMPL_STABILIZE_EPS: f32 = 9.5367431640625e-07;

/// 16-tap symmetric fractional-delay interpolation FIR (WASM mem 0xe8780, func 3523/3507).
const SMPL_FIR16: [f32; 16] = [
    -0.000006392598606907995,
    0.00011064113641623408,
    -0.0009153038263320923,
    0.0048477197997272015,
    -0.018698347732424736,
    0.05759090930223465,
    -0.15997476875782013,
    0.617045521736145,
    0.6170454621315002,
    -0.15997475385665894,
    0.05759090557694435,
    -0.018698347732424736,
    0.0048477197997272015,
    -0.0009153038263320923,
    0.00011064114369219169,
    -0.0000063925981521606445,
];

#[derive(serde::Deserialize)]
pub(crate) struct SmplSynthTables {
    /// `[stage1][config][grid][coeff]` -> per-symbol NLSF residual values.
    pub(crate) valtables: Vec<Vec<Vec<Vec<Vec<f32>>>>>,
    /// `[stage1][grid]` -> 16 base NLSF (radians, half-scale).
    pub(crate) centroids: Vec<Vec<Vec<f32>>>,
    /// `[stage1][grid]` -> 16x16 decorrelation matrix (`mat[row][col]`).
    pub(crate) matrices: Vec<Vec<Vec<Vec<f32>>>>,
    /// `[stage1]` -> 17 NLSF stabilize minimum spacings.
    pub(crate) min_spacing: Vec<Vec<f32>>,
    /// grid==16 base NLSF tables (selected INVERTED by signal type).
    pub(crate) grid16_w: Vec<Vec<f32>>,
    pub(crate) grid16_alpha: Vec<f32>,
    /// `[sig][config]` -> 256-entry flat column-major grid==16 decorrelation matrix.
    pub(crate) grid16_matrices: Vec<Vec<Vec<f32>>>,
}

static SMPL_SYNTH_TABLES: OnceLock<SmplSynthTables> = OnceLock::new();

pub(crate) fn load_smpl_synth_tables() -> &'static SmplSynthTables {
    SMPL_SYNTH_TABLES.get_or_init(|| {
        serde_json::from_str(include_str!("testdata/smpl_synth_tables.json"))
            .expect("smpl_synth_tables.json must parse")
    })
}

/// func 3530 (silk_NLSF_VQ_weights_laroia): inverse-gap weights w[k] = invgap[k] + invgap[k+1].
fn smpl_nlsf_laroia_weights(nlsf: &[f32], out: &mut [f32]) {
    let mut inv = [0f32; SMPL_ORDER + 1];
    let clamp = |gap: f32| -> f32 {
        if gap > SMPL_NLSF_WEIGHT_EPS {
            1.0 / gap
        } else {
            SMPL_NLSF_WEIGHT_W_MAX
        }
    };
    inv[0] = clamp(nlsf[0]);
    let mut prev = nlsf[0];
    for k in 1..SMPL_ORDER {
        let gap = nlsf[k] - prev;
        inv[k] = clamp(gap);
        prev = nlsf[k];
    }
    inv[SMPL_ORDER] = clamp(SMPL_PI_F32 - nlsf[SMPL_ORDER - 1]);
    for k in 0..SMPL_ORDER {
        out[k] = inv[k] + inv[k + 1];
    }
}

/// func 3485 (decorrelation-matrix apply): out[r] = sum_c mat[c*16 + r] * vec[c] (column-major mat).
fn smpl_nlsf_decorr(mat: &[f32], vec: &[f32], out: &mut [f32]) {
    let mut scr = [0f32; SMPL_ORDER];
    let v0 = vec[0];
    for r in 0..SMPL_ORDER {
        scr[r] = v0 * mat[r];
    }
    for (c, &v) in vec.iter().enumerate().take(SMPL_ORDER).skip(1) {
        let base = c * SMPL_ORDER;
        for r in 0..SMPL_ORDER {
            scr[r] += mat[base + r] * v;
        }
    }
    out[..SMPL_ORDER].copy_from_slice(&scr);
}

/// Reconstruct the order-16 NLSF (radians) from the decoded LSF indices (f3597 1437..2002).
pub(crate) fn smpl_reconstruct_nlsf(
    t: &SmplSynthTables,
    stage1: usize,
    config: usize,
    grid: usize,
    stage2: &[i32; 16],
    prev_nlsf: &[f32],
) -> Vec<f32> {
    let val = &t.valtables[stage1][config][grid];
    let mut resid = [0f32; SMPL_ORDER];
    for (k, r) in resid.iter_mut().enumerate() {
        let sym = stage2[k];
        if sym >= 0 && (sym as usize) < val[k].len() {
            *r = val[k][sym as usize];
        }
    }

    let mut out = vec![0f32; SMPL_ORDER];
    if grid == 16 {
        // grid==16: interpolate base between prevNLSF and the inverted grid16 base table.
        let mut base = [0f32; SMPL_ORDER];
        let base_tbl = &t.grid16_w[1 - stage1];
        let alpha = t.grid16_alpha[stage1];
        for k in 0..SMPL_ORDER {
            let pv = if k < prev_nlsf.len() {
                prev_nlsf[k]
            } else {
                0.0
            };
            base[k] = pv + alpha * (base_tbl[k] - pv);
        }
        let mut w = [0f32; SMPL_ORDER];
        smpl_nlsf_laroia_weights(&base, &mut w);
        for wk in w.iter_mut() {
            *wk = (*wk as f64).sqrt() as f32;
        }
        let mut decorr = [0f32; SMPL_ORDER];
        let mat16 = &t.grid16_matrices[stage1][config];
        smpl_nlsf_decorr(mat16, &resid, &mut decorr);
        for k in 0..SMPL_ORDER {
            out[k] = base[k] + decorr[k] / w[k];
        }
        smpl_stabilize_nlsf(&mut out, &t.min_spacing[stage1]);
        return out;
    }

    // matrix case (grid < 16): NLSF[r] = 2*centroid[r] + sum_c mat[c][r]*resid[c].
    let cent = &t.centroids[stage1][grid];
    let mat = &t.matrices[stage1][grid];
    for r in 0..SMPL_ORDER {
        let mut acc = 2.0 * cent[r];
        for c in 0..SMPL_ORDER {
            acc += mat[c][r] * resid[c];
        }
        out[r] = acc;
    }
    smpl_stabilize_nlsf(&mut out, &t.min_spacing[stage1]);
    out
}

/// func 3533 (silk_NLSF_stabilize): enforce minimum spacing + ordering in the margin domain.
fn smpl_stabilize_nlsf(nlsf: &mut [f32], min_spacing: &[f32]) {
    const PI: f32 = SMPL_PI_F32;
    const L: usize = SMPL_ORDER;
    let mut marg = [0f32; L + 1];
    marg[0] = nlsf[0] - min_spacing[0];
    for i in 1..L {
        marg[i] = nlsf[i] - nlsf[i - 1] - min_spacing[i];
    }
    marg[L] = PI - nlsf[L - 1] - min_spacing[L];

    let argmin = |marg: &[f32; L + 1]| -> (f32, usize) {
        let mut m = marg[0];
        let mut idx = 0;
        for (i, &v) in marg.iter().enumerate().take(L + 1).skip(1) {
            if v < m {
                m = v;
                idx = i;
            }
        }
        (m, idx)
    };
    let (mut min, mut sel) = argmin(&marg);
    let mut loop_n = 0i32;
    while min < 0.0 {
        let d = loop_n as f32 * SMPL_STABILIZE_EPS - min;
        if sel == 0 {
            marg[0] += d;
            marg[1] -= d;
        } else if sel == L {
            marg[L] += d;
            marg[L - 1] -= d;
        } else {
            marg[sel] += d;
            let half = d * 0.5;
            marg[sel - 1] -= half;
            marg[sel + 1] -= half;
        }
        let (m, s) = argmin(&marg);
        min = m;
        sel = s;
        if min < 0.0 {
            loop_n += 1;
            if loop_n == SMPL_STABILIZE_MAX_LOOPS {
                break;
            }
        }
    }
    nlsf[0] = min_spacing[0] + marg[0];
    let mut run = nlsf[0];
    for i in 1..L {
        run = run + marg[i] + min_spacing[i];
        nlsf[i] = run;
    }
}

/// func 3513 (silk_NLSF2A): order-16 NLSF (radians) -> LPC coefficients a[0..16], a[0]=1.0.
pub(crate) fn smpl_nlsf2a(nlsf: &[f32]) -> Vec<f32> {
    let order = nlsf.len();
    let half = order / 2;
    let cosv: Vec<f64> = nlsf.iter().map(|&x| (x as f64).cos()).collect();
    let mut p = vec![0f64; half + 1];
    let mut q = vec![0f64; half + 1];
    smpl_nlsf_poly(&mut p, &cosv, half, 0);
    smpl_nlsf_poly(&mut q, &cosv, half, 1);

    let mut a = vec![0f32; order + 1];
    a[0] = 1.0;
    for k in 0..half {
        let pt = p[k + 1] + p[k];
        let qt = q[k + 1] - q[k];
        a[k + 1] = (0.5 * (pt + qt)) as f32;
        a[order - k] = (0.5 * (pt - qt)) as f32;
    }
    a
}

fn smpl_nlsf_poly(out: &mut [f64], cosv: &[f64], half: usize, parity: usize) {
    out[0] = 1.0;
    out[1] = -2.0 * cosv[parity];
    for k in 1..half {
        let c = -2.0 * cosv[2 * k + parity];
        out[k + 1] = 2.0 * out[k - 1] + c * out[k];
        let mut n = k;
        while n > 1 {
            out[n] += out[n - 2] + c * out[n - 1];
            n -= 1;
        }
        out[1] += c;
    }
}

/// func 3503 (order-16 LPC synthesis): out[n] = ex[n] - sum_{j=1..16} a[j]*out[n-j]. `state` holds
/// the previous `order` outputs (carried across subframes/frames), updated in place.
fn smpl_lpc_synthesis(ex: &[f32], a: &[f32], out: &mut [f32], state: &mut [f32]) {
    let order = SMPL_ORDER;
    for n in 0..ex.len() {
        let mut acc = ex[n] as f64;
        for j in 1..=order {
            let prev = if n >= j {
                out[n - j] as f64
            } else {
                state[order + n - j] as f64
            };
            acc -= a[j] as f64 * prev;
        }
        out[n] = acc as f32;
    }
    if out.len() >= order {
        state[..order].copy_from_slice(&out[out.len() - order..]);
    }
}

/// func 3597 gain: quantized log-gain -> linear gain (fast pow2 bit-cast).
pub(crate) fn smpl_gain_lin(gain_q: i32) -> f64 {
    let y = gain_q as f32 * 6.103515625e-05 * 0.10000000149011612 * 27749388.0 + 1064866816.0;
    let i: i32 = if y < 2147483648.0 && y > -2147483648.0 {
        y as i32
    } else {
        -2147483648
    };
    let mut f = f32::from_bits(i as u32) - 3.1622775509276835e-09;
    if f < 0.0 {
        f = 0.0;
    }
    f as f64
}

fn smpl_floor_f32(x: f32) -> f32 {
    let mut i = x as i32;
    if i as f32 > x {
        i -= 1;
    }
    i as f32
}

fn abs_f32(x: f32) -> f32 {
    if x < 0.0 { -x } else { x }
}

/// func 3507: 8-tap symmetric FIR16 application, IN-PLACE over `sig` (the WASM passes in==out, so
/// overlapping read/write regions must see prior writes). f32 accumulation order matches the WASM.
fn smpl_fir8(sig: &mut [f32], in_base: i32, out_base: i32, cnt: i32) {
    for jj in 0..cnt {
        let mut acc = 0f32;
        for i in 0..8 {
            acc += (sig[(in_base + jj + i) as usize] + sig[(in_base + jj + 15 - i) as usize])
                * SMPL_FIR16[i as usize];
        }
        sig[(out_base + jj) as usize] = acc;
    }
}

/// func 3523 (fractional LTP + interpolation). Reads `sig` backward from `sig_end` and writes two
/// regions per subframe into `out` (len 2*num_subfr*40); also mutates `sig` in place.
fn smpl_frac_ltp(
    lag: &[f32],
    num_subfr: i32,
    sig: &mut [f32],
    sig_end: i32,
    state_len: i32,
    out: &mut [f32],
) {
    let mut lb = sig_end - (40 * num_subfr - state_len);
    for sf in 0..num_subfr {
        let fl = smpl_floor_f32(lag[sf as usize]);
        let int_lag = fl as i32;
        if int_lag as f32 == lag[sf as usize] {
            for k in 0..40 {
                sig[(lb + k) as usize] = sig[(lb + k - int_lag) as usize];
            }
            for k in 0..40 {
                out[(sf * 40 + k) as usize] = sig[(lb + k) as usize];
                out[((num_subfr + sf) * 40 + k) as usize] =
                    sig[(lb + k - int_lag - 1) as usize] + sig[(lb + k - int_lag + 1) as usize];
            }
        } else {
            let b = (num_subfr + sf) * 40;
            for k in 0..40 {
                out[(b + k) as usize] =
                    sig[(lb - int_lag - 1 + k) as usize] + sig[(lb - int_lag + 1 + k) as usize];
            }
            let mut l10 = 0f32;
            for j in 0..16 {
                l10 += sig[(lb - 9 - int_lag + j) as usize] * SMPL_FIR16[j as usize];
            }
            smpl_fir8(sig, lb - int_lag - 8, lb, 40);
            let mut l11 = 0f32;
            for j in 0..16 {
                l11 += sig[(lb + 32 - int_lag + j) as usize] * SMPL_FIR16[j as usize];
            }
            for k in 0..40 {
                out[(sf * 40 + k) as usize] = sig[(lb + k) as usize];
            }
            out[b as usize] = l10 + sig[(lb + 1) as usize];
            for k in 0..38 {
                out[(b + 1 + k) as usize] = sig[(lb + k) as usize] + sig[(lb + 2 + k) as usize];
            }
            out[(b + 39) as usize] = l11 + sig[(lb + 38) as usize];
        }
        lb += 40;
    }
}

/// func 3522 (per-subframe LTP gain-apply) state, reset per internal frame.
#[derive(Default, Clone, Copy)]
pub(crate) struct SmplExcGainState {
    s0: f32,
    s1: f32,
}

fn smpl_exc_gain_apply(
    sub_len: usize,
    input: &[f32],
    st: &mut SmplExcGainState,
    out: &mut [f32],
    gain: f32,
) {
    if gain != 0.0 {
        let s5 = st.s1;
        let s6 = (s5 + s5) + st.s0;
        let d = st.s0 - s5;
        let abs_d = abs_f32(d);
        let abs_s6 = abs_f32(s6);
        let mut mn = abs_d + gain;
        if abs_s6 < mn {
            mn = abs_s6;
        }
        let t = d * mn / (abs_d + 1e-12);
        st.s1 = (s6 - t) / 3.0;
        st.s0 = (2.0 * t + s6) / 3.0;
    }
    if sub_len == 0 {
        return;
    }
    let s0 = st.s0;
    for n in 0..sub_len {
        out[n] = s0 * input[n];
    }
    let s1 = st.s1;
    for n in 0..sub_len {
        out[n] += s1 * input[sub_len + n];
    }
}

/// func 3597 offset 4342: fractional-LTP gain = normGain*-0.17 + 0.35.
pub(crate) fn smpl_ltp_frac_gain(norm_gain: f64) -> f32 {
    norm_gain as f32 * -0.16999998688697815 + 0.3499999940395355
}

/// One 80-sample subframe's fractional-LTP prediction (func 3523 + func 3522).
pub(crate) fn smpl_ltp_subframe_pred(
    hist: &mut [f32],
    hist_pos: i32,
    lag_f: f32,
    gain_frac: f32,
    gst: &mut SmplExcGainState,
    pred_out: &mut [f32],
) {
    let mut frac_out = [0f32; 2 * 2 * 40];
    let lags = [lag_f, lag_f];
    smpl_frac_ltp(
        &lags,
        2,
        hist,
        hist_pos - 648,
        SMPL_FRAC_STATE_LEN,
        &mut frac_out,
    );
    smpl_exc_gain_apply(SMPL_SUBFR_LEN, &frac_out, gst, pred_out, gain_frac);
}

/// LTP parameters the synthesis needs for one internal frame.
#[derive(Clone)]
pub(crate) struct SmplPitchSynth {
    pub(crate) voiced: bool,
    pub(crate) lag_subfr: [f64; 4], // per-subframe func-3523 lag = intLagQ6[sf]*0.5 + 32
    pub(crate) norm_gain: f64,
}

/// Cross-internal-frame synthesis state (LPC + LTP/excitation history + the post-LPC band tail).
#[derive(Clone)]
pub(crate) struct SmplFrameSynth {
    lpc_state: [f32; SMPL_ORDER],
    ltp_hist: Vec<f32>,
    gst: SmplExcGainState,
    /// Region-1 per-subframe excitation postfilter state (func 3524), persistent.
    pf: SmplPostfilterState,
    /// Post-LPC HP (pitch-harmonic) postfilter state, persistent across the stream.
    hp: HpPostfilterState,
}

impl Default for SmplFrameSynth {
    fn default() -> Self {
        SmplFrameSynth {
            lpc_state: [0.0; SMPL_ORDER],
            ltp_hist: vec![0.0; LTP_HIST_LEN],
            gst: SmplExcGainState::default(),
            pf: SmplPostfilterState::default(),
            hp: HpPostfilterState::default(),
        }
    }
}

/// Turn one 20 ms internal frame's decoded parameters into 320 LB PCM samples (float, ~int16-scaled).
/// Returns (signal, nlsf); `nlsf` becomes the next frame's `prev_nlsf`.
#[allow(clippy::too_many_arguments)]
pub(crate) fn synth_internal_frame(
    t: &SmplSynthTables,
    st: &mut SmplFrameSynth,
    stage1: usize,
    config: usize,
    grid: usize,
    stage2: &[i32; 16],
    prev_nlsf: &[f32],
    pulses: &[i32],
    gain_q: &[i32; 4],
    pitch: &SmplPitchSynth,
) -> (Vec<f32>, Vec<f32>) {
    let nlsf = smpl_reconstruct_nlsf(t, stage1, config, grid, stage2, prev_nlsf);
    let a = smpl_nlsf2a(&nlsf);

    let sub_gain = |sf: usize| -> f64 {
        let gq = if sf < gain_q.len() { gain_q[sf] } else { 0 };
        smpl_gain_lin(gq) * SMPL_SUBFR_LEN as f64
    };

    let mut ex = vec![0f32; SMPL_INTF_LEN];
    for n in 0..SMPL_INTF_LEN {
        ex[n] = (pulses[n] as f64 * sub_gain(n / SMPL_SUBFR_LEN)) as f32;
    }
    let hist = &mut st.ltp_hist;

    if pitch.voiced {
        const G_LTP: f32 = 0.949999988079071;
        let gain_frac = smpl_ltp_frac_gain(pitch.norm_gain);
        let mut pred_out = vec![0f32; SMPL_SUBFR_LEN];
        st.gst = SmplExcGainState::default();
        for sf in 0..SMPL_SUBFR_COUNT {
            let lag_f = pitch.lag_subfr[sf] as f32;
            let int_lag = lag_f as i32;
            if int_lag <= 0 {
                let from = sf * SMPL_SUBFR_LEN;
                let to = (sf + 1) * SMPL_SUBFR_LEN;
                hist[SMPL_LTP_HIST + from..SMPL_LTP_HIST + to].copy_from_slice(&ex[from..to]);
                continue;
            }
            let ex_base = sf * SMPL_SUBFR_LEN;
            let hist_pos = (SMPL_LTP_HIST + ex_base) as i32;
            if int_lag > 0 && (int_lag as usize) < SMPL_SUBFR_LEN {
                for n in (int_lag as usize)..SMPL_SUBFR_LEN {
                    ex[ex_base + n] += G_LTP * ex[ex_base + n - int_lag as usize];
                }
            }
            smpl_ltp_subframe_pred(hist, hist_pos, lag_f, gain_frac, &mut st.gst, &mut pred_out);
            for n in 0..SMPL_SUBFR_LEN {
                ex[ex_base + n] += pred_out[n];
            }
            hist[hist_pos as usize..hist_pos as usize + SMPL_SUBFR_LEN]
                .copy_from_slice(&ex[ex_base..ex_base + SMPL_SUBFR_LEN]);
        }
    } else {
        hist[SMPL_LTP_HIST..SMPL_LTP_HIST + SMPL_INTF_LEN].copy_from_slice(&ex);
    }

    // Region-1 excitation postfilter (func 3524), per subframe: derive a short resonator from each
    // subframe's autocorrelation and add resonated env-shaped noise into the excitation before LPC
    // synthesis. The LTP history keeps the pre-postfilter excitation (the comb is a synthesis-path
    // addition). The env params use a {0, rms} placeholder pending ground truth (they only drive the
    // trailing-biquad bandwidth).
    if SMPL_TAIL_REGION1 {
        let mut pf_out = [0f32; SMPL_SUBFR_LEN];
        for sf in 0..SMPL_SUBFR_COUNT {
            let base = sf * SMPL_SUBFR_LEN;
            let sub: Vec<f32> = ex[base..base + SMPL_SUBFR_LEN].to_vec();
            let ss: f32 = sub.iter().map(|&v| v * v).sum();
            let rms = (ss / SMPL_SUBFR_LEN as f32).sqrt();
            st.pf.env_state = rms;
            smpl_comb_postfilter(
                &mut st.pf,
                &sub,
                SMPL_SUBFR_LEN,
                true,
                0.0,
                [0.0, rms],
                &mut pf_out,
            );
            for i in 0..SMPL_SUBFR_LEN {
                ex[base + i] += pf_out[i];
            }
        }
    }

    let mut out = vec![0f32; SMPL_INTF_LEN];
    smpl_lpc_synthesis(&ex, &a, &mut out, &mut st.lpc_state);

    // Post-LPC HP (pitch-harmonic) postfilter: de-emphasis -> ARMA2 comb resonating at the pitch
    // frequency (f = 1/lag) -> companion pre-emphasis. The comb's average lag is the energy-weighted
    // mean of the subframe pitch lags (0 -> the default fixed-corner curve, for unvoiced).
    if SMPL_HP_POSTFILTER {
        let lag = if pitch.voiced {
            let (mut sl, mut sll) = (0f32, 0f32);
            for &l in &pitch.lag_subfr {
                let lf = l as f32;
                sl += lf;
                sll += lf * lf;
            }
            if sl > 0.0 { sll / sl } else { 0.0 }
        } else {
            0.0
        };
        let mut tail = vec![0f32; SMPL_INTF_LEN];
        smpl_hp_postfilter(&mut st.hp, &out, SMPL_INTF_LEN, lag, &mut tail);
        out.copy_from_slice(&tail);
    }

    // roll the LTP history forward by one internal frame; clear the forward margin.
    hist.copy_within(SMPL_INTF_LEN..SMPL_LTP_HIST + SMPL_INTF_LEN, 0);
    for v in hist
        .iter_mut()
        .take(LTP_HIST_LEN)
        .skip(SMPL_LTP_HIST + SMPL_INTF_LEN)
    {
        *v = 0.0;
    }
    (out, nlsf)
}

/// Cross-frame decoder state (the persistent LSF/pitch predictor, prev NLSF, CELP synthesis).
#[derive(Default)]
pub(crate) struct SmplDecoderState {
    pub(crate) lstate: super::smpl_decode::SmplLsfState,
    pub(crate) prev_nlsf: Vec<f32>,
    /// C-float-domain CELP synthesis state (excitation + ACB + gen_noise + LPC synth).
    pub(crate) celp: super::smpl_celpdec::CelpDecState,
    /// Per-packet harmonic postfilter state (runs once per packet after all internal frames).
    pub(crate) harm: super::smpl_harm_postfilter::HarmPostfilterState,
}
```

`smpl_celpdec.rs`:

```rust
//! Decoder-side CELP synthesis in the codec's native float domain — a faithful port of the
//! per-subframe loop in `smpl_core_decoder.c` (excitation → CELP/ACB decode → gen_noise → LPC
//! synthesis) plus the LSF interpolation (`smpl_lpc_interpol`) and the FCB gain tables.
//!
//! This replaces the Go-derived int16-domain LB synthesis. The Go reference diverged from the real
//! codec (wrong amplitude domain, no gen_noise), so the byte-exact target here is the smpl_opus C
//! fork (validated bit-exact against the WASM useSmpl reference). Output is float in [-1, 1].
//!
//! Self-contained on purpose (mirrors `smpl_celp.rs`): the leaf helpers are local so the decoder
//! path is decoupled from the encoder.
#![allow(clippy::excessive_precision)]
#![allow(clippy::needless_range_loop)]
#![allow(clippy::too_many_arguments)]

use super::smpl_gennoise::{
    NoiseGenerator, smpl_celp_gen_noise, smpl_decode_resnrg, smpl_get_normalized_bitrate,
};
use std::sync::OnceLock;

const SMPL_LPC_ORDER: usize = 16;
const SMPL_SUBFR_LEN: usize = 80; // 5 ms @ 16 kHz, num_subframes==4
const SMPL_NUM_SUBFR: usize = 4;
const SMPL_FRAME_LEN: usize = 320; // 20 ms
const SMPL_LAG_SUBFRLEN: usize = 40;
const SMPL_LTP_INTERPOL_DELAY: usize = 8;
const SMPL_MAX_PITCH_LAG: usize = 320;
const SMPL_ACBG_M: usize = 2;
const SMPL_PITCH_SHARPENING_COEF: f32 = 0.9881;

const SMPL_FCBG_V_N: usize = 34;
const SMPL_UV_GAIN_IDX_LEN: usize = 90;
const SMPL_V_GAIN_MIN_DB: f32 = -100.0;
const SMPL_V_GAIN_STEP_DB: f32 = 3.0;
const SMPL_UV_GAIN_MIN_DB: f32 = -90.0;
const SMPL_UV_GAIN_STEP_DB: f32 = 1.0;

/// ACB high-boost endpoints (`smpl_dec_acb_high_boost`).
const SMPL_DEC_ACB_HIGH_BOOST: [f32; 2] = [0.35, 0.18];

/// LSF→LPC interpolation factors per subframe, `[lsf_interpol_idx][sf]` (`smpl_lsf_interpol_4`).
const SMPL_LSF_INTERPOL_4: [[f32; 4]; 2] = [[0.55, 0.88, 1.0, 1.0], [0.3, 0.65, 0.95, 1.0]];

/// 16-tap symmetric LTP interpolation kernel (`smpl_interpol_kernel`).
#[rustfmt::skip]
const SMPL_INTERPOL_KERNEL: [f32; 2 * SMPL_LTP_INTERPOL_DELAY] = [
    -6.3925986e-6, 0.00011064114, -0.0009153038, 0.00484772, -0.018698348, 0.05759091, -0.15997477, 0.6170455,
    0.61704546, -0.15997475, 0.057590906, -0.018698348, 0.00484772, -0.0009153038, 0.000110641144, -6.392598e-6,
];

/// Per-subframe ACB-gain codebook (Q14), low-rate and high-rate (`smpl_cb_acbgains_{lr,hr}_Q14`).
/// Only the high-rate table is exercised by this capture (low_rate==0).
fn acbgains_cb_hr() -> &'static [i16] {
    super::smpl_celp::cb_acbgains_hr_q14()
}
fn acbgains_cb_lr() -> &'static [i16] {
    super::smpl_celp::cb_acbgains_lr_q14()
}

struct FcbGains {
    uv: [f32; SMPL_UV_GAIN_IDX_LEN + 1],
    v: [f32; SMPL_FCBG_V_N],
}

fn fcb_gains() -> &'static FcbGains {
    static T: OnceLock<FcbGains> = OnceLock::new();
    T.get_or_init(|| {
        let mut uv = [0.0f32; SMPL_UV_GAIN_IDX_LEN + 1];
        let mut v = [0.0f32; SMPL_FCBG_V_N];
        for ix in 0..=SMPL_UV_GAIN_IDX_LEN {
            uv[ix] = 10.0f32.powf(0.05 * (ix as f32 * SMPL_UV_GAIN_STEP_DB + SMPL_UV_GAIN_MIN_DB));
        }
        for ix in 0..SMPL_FCBG_V_N {
            v[ix] = 10.0f32.powf(0.05 * (ix as f32 * SMPL_V_GAIN_STEP_DB + SMPL_V_GAIN_MIN_DB));
        }
        FcbGains { uv, v }
    })
}

#[inline]
fn smpl_dot_prod(a: &[f32], b: &[f32], l: usize) -> f32 {
    let mut r = 0.0f32;
    for i in 0..l {
        r += a[i] * b[i];
    }
    r
}

/// `smpl_NLSF2A`-equivalent: order-16 NLSF (radians) → LPC `a[0..16]`, a[0]=1 (matches smpl_synth).
fn nlsf2a(nlsf: &[f32]) -> [f32; SMPL_LPC_ORDER + 1] {
    let order = SMPL_LPC_ORDER;
    let half = order / 2;
    let cosv: Vec<f64> = nlsf.iter().map(|&x| (x as f64).cos()).collect();
    let mut p = [0f64; SMPL_LPC_ORDER / 2 + 1];
    let mut q = [0f64; SMPL_LPC_ORDER / 2 + 1];
    nlsf_poly(&mut p, &cosv, half, 0);
    nlsf_poly(&mut q, &cosv, half, 1);
    let mut a = [0f32; SMPL_LPC_ORDER + 1];
    a[0] = 1.0;
    for k in 0..half {
        let pt = p[k + 1] + p[k];
        let qt = q[k + 1] - q[k];
        a[k + 1] = (0.5 * (pt + qt)) as f32;
        a[order - k] = (0.5 * (pt - qt)) as f32;
    }
    a
}

fn nlsf_poly(out: &mut [f64], cosv: &[f64], half: usize, parity: usize) {
    out[0] = 1.0;
    out[1] = -2.0 * cosv[parity];
    for k in 1..half {
        let c = -2.0 * cosv[2 * k + parity];
        out[k + 1] = 2.0 * out[k - 1] + c * out[k];
        let mut n = k;
        while n > 1 {
            out[n] += out[n - 2] + c * out[n - 1];
            n -= 1;
        }
        out[1] += c;
    }
}

/// `smpl_lpc_interpol`: per-subframe interpolation of the LSF between `prev_lsf` and `lsf`, then
/// NLSF→A. Returns the per-subframe A coefficients and writes the per-subframe interpolated LSF.
/// Mutates `prev_lsf` to the last interpolated LSF (codec carries it across frames).
fn lpc_interpol(
    lsf: &[f32],
    prev_lsf: &mut [f32; SMPL_LPC_ORDER],
    interpol: &[f32; 4],
    a_out: &mut [[f32; SMPL_LPC_ORDER + 1]; SMPL_NUM_SUBFR],
    lsfs_out: &mut [[f32; SMPL_LPC_ORDER]; SMPL_NUM_SUBFR],
) {
    if prev_lsf[SMPL_LPC_ORDER - 1] == 0.0 {
        prev_lsf[..SMPL_LPC_ORDER].copy_from_slice(&lsf[..SMPL_LPC_ORDER]);
    }
    let mut ilsf = [0.0f32; SMPL_LPC_ORDER];
    let mut prev_factor = -1.0f32;
    for j in 0..SMPL_NUM_SUBFR {
        if interpol[j] == prev_factor {
            a_out[j] = a_out[j - 1];
        } else {
            if interpol[j] == 1.0 {
                ilsf.copy_from_slice(&lsf[..SMPL_LPC_ORDER]);
            } else {
                for k in 0..SMPL_LPC_ORDER {
                    ilsf[k] = prev_lsf[k] * (1.0 - interpol[j]) + lsf[k] * interpol[j];
                }
            }
            a_out[j] = nlsf2a(&ilsf);
        }
        prev_factor = interpol[j];
        lsfs_out[j] = ilsf;
    }
    prev_lsf.copy_from_slice(&ilsf);
}

#[inline]
fn acb_dequant(low_rate: bool, acb_idx: i32, acb_g: &mut [f32; SMPL_ACBG_M]) {
    let cb = if low_rate {
        acbgains_cb_lr()
    } else {
        acbgains_cb_hr()
    };
    let sc = 1.0f32 / ((1i32 << 14) as f32);
    for m in 0..SMPL_ACBG_M {
        acb_g[m] = cb[acb_idx as usize * SMPL_ACBG_M + m] as f32 * sc;
    }
}

/// `adjust_acbgains` then 3-tap symmetric ACB synthesis (`acb_synthesize` with high-boost applied).
fn acb_synthesize(
    fcb_subfrlen: usize,
    acb_basis: &[f32],
    acb_g_in: &[f32; SMPL_ACBG_M],
    high_boost: f32,
    acb: &mut [f32],
) {
    let mut acb_g = *acb_g_in;
    if high_boost != 0.0 {
        let f0 = acb_g[0] + 2.0 * acb_g[1];
        let f1 = acb_g[0] - acb_g[1];
        let abs_f2new = (f1.abs() + high_boost).min(f0.abs());
        let f1 = f1 * (abs_f2new / (f1.abs() + 1e-12));
        acb_g[0] = (f0 + 2.0 * f1) / 3.0;
        acb_g[1] = (f0 - f1) / 3.0;
    }
    for i in 0..fcb_subfrlen {
        acb[i] = acb_g[0] * acb_basis[i];
    }
    for i in 0..fcb_subfrlen {
        acb[i] += acb_g[1] * acb_basis[fcb_subfrlen + i];
    }
}

#[inline]
fn pitch_sharp(x: &mut [f32], lag: usize, l: usize) {
    for i in lag..l {
        x[i] += x[i - lag] * SMPL_PITCH_SHARPENING_COEF;
    }
}

/// `smpl_syn_ltp_basis`: build the ACB basis from the excitation history; mutates `state` forward.
/// `state` is the full ACB state; the logical start is `state[state_len - n_lags*40]`.
fn syn_ltp_basis(
    lags: &[f32],
    n_lags: usize,
    state: &mut [f32],
    state_len: usize,
    acb_basis: &mut [f32],
) {
    let mut p = state_len - n_lags * SMPL_LAG_SUBFRLEN;
    for subfr in 0..n_lags {
        let i_lag = lags[subfr].floor() as i32;
        if (i_lag as f32) == lags[subfr] {
            let il = i_lag as usize;
            for i in 0..SMPL_LAG_SUBFRLEN {
                state[p + i] = state[(p + i) - il];
            }
            for i in 0..SMPL_LAG_SUBFRLEN {
                acb_basis[subfr * SMPL_LAG_SUBFRLEN + i] = state[p + i];
            }
            for i in 0..SMPL_LAG_SUBFRLEN {
                let a = state[(p + i) - il - 1];
                let b = state[(p + i) - il + 1];
                acb_basis[(n_lags + subfr) * SMPL_LAG_SUBFRLEN + i] = a + b;
            }
        } else {
            let il = i_lag;
            let base_first = (p as i32) + (-1 - il - SMPL_LTP_INTERPOL_DELAY as i32);
            let first = smpl_dot_prod(
                &state[base_first as usize..],
                &SMPL_INTERPOL_KERNEL,
                2 * SMPL_LTP_INTERPOL_DELAY,
            );
            {
                let src_base = (p as i32) + (-il - SMPL_LTP_INTERPOL_DELAY as i32);
                for nn in 0..SMPL_LAG_SUBFRLEN {
                    let mut ret = 0.0f32;
                    for i in 0..8 {
                        let s0 = state[(src_base + nn as i32 + i as i32) as usize];
                        let s1 = state[(src_base + nn as i32 + 15 - i as i32) as usize];
                        ret += (s0 + s1) * SMPL_INTERPOL_KERNEL[i];
                    }
                    state[p + nn] = ret;
                }
            }
            // C uses `i = SMPL_LAG_SUBFRLEN` here (i runs -1 -> 0 -> +SMPL_LAG_SUBFRLEN), so the tap base
            // is p + (SMPL_LAG_SUBFRLEN - i_lag - delay), not -1 of that.
            let base_last =
                (p as i32) + (SMPL_LAG_SUBFRLEN as i32 - il - SMPL_LTP_INTERPOL_DELAY as i32);
            let last = smpl_dot_prod(
                &state[base_last as usize..],
                &SMPL_INTERPOL_KERNEL,
                2 * SMPL_LTP_INTERPOL_DELAY,
            );
            for i in 0..SMPL_LAG_SUBFRLEN {
                acb_basis[subfr * SMPL_LAG_SUBFRLEN + i] = state[p + i];
            }
            let b1 = (n_lags + subfr) * SMPL_LAG_SUBFRLEN;
            acb_basis[b1] = first + state[p + 1];
            for i in 0..SMPL_LAG_SUBFRLEN - 2 {
                acb_basis[b1 + 1 + i] = state[p + i] + state[p + i + 2];
            }
            let i_last = SMPL_LAG_SUBFRLEN - 1;
            acb_basis[b1 + i_last] = state[p + i_last - 1] + last;
        }
        p += SMPL_LAG_SUBFRLEN;
    }
}

/// `smpl_celp_decode` (voiced branch): add the ACB (LTP) contribution into `lpc_res`, then push the
/// subframe into the ACB state. `acb_state_len = subfrlen + 2*MAX_PITCH_LAG + LTP_INTERPOL_DELAY`.
fn celp_decode(
    acb_state: &mut [f32],
    acb_state_len: usize,
    voiced: bool,
    acb_gain_idx: i32,
    lags: &[f32],
    num_lags: usize,
    subfrlen: usize,
    low_rate: bool,
    normalized_bitrate: f32,
    lpc_res: &mut [f32],
) {
    if voiced {
        let high_boost = SMPL_DEC_ACB_HIGH_BOOST[0]
            + (SMPL_DEC_ACB_HIGH_BOOST[1] - SMPL_DEC_ACB_HIGH_BOOST[0]) * normalized_bitrate;
        let i_lag = lags[num_lags - 1] as i32;
        if low_rate {
            pitch_sharp(lpc_res, i_lag as usize, subfrlen);
        }
        let mut acb_basis = vec![0.0f32; subfrlen * SMPL_ACBG_M];
        let mut acb = vec![0.0f32; subfrlen];
        syn_ltp_basis(lags, num_lags, acb_state, acb_state_len, &mut acb_basis);
        let mut acb_gain = [0.0f32; SMPL_ACBG_M];
        acb_dequant(low_rate, acb_gain_idx, &mut acb_gain);
        acb_synthesize(subfrlen, &acb_basis, &acb_gain, high_boost, &mut acb);
        for i in 0..subfrlen {
            lpc_res[i] += acb[i];
        }
    }
    // Update ACB state: shift left by subfrlen, append this subframe's excitation.
    acb_state.copy_within(subfrlen..acb_state_len - subfrlen, 0);
    acb_state[acb_state_len - 2 * subfrlen..acb_state_len - subfrlen]
        .copy_from_slice(&lpc_res[..subfrlen]);
}

/// `smpl_filt_ar16` over one subframe: `y[n] = x[n] - sum_{i} coef[16-i]*y[n-16+i]`. `ybuf` holds a
/// 16-sample history prefix at `ybuf[base-16 .. base]`, so synthesis flows contiguously across the
/// frame (cross-subframe and cross-frame history is the same buffer).
fn filt_ar16(x: &[f32], a: &[f32; SMPL_LPC_ORDER + 1], ybuf: &mut [f32], base: usize, n: usize) {
    for nn in 0..n {
        let mut res = x[nn];
        for i in 0..SMPL_LPC_ORDER {
            res -= a[SMPL_LPC_ORDER - i] * ybuf[base + nn - SMPL_LPC_ORDER + i];
        }
        ybuf[base + nn] = res;
    }
}

/// Per-subframe decoded params the synthesis consumes.
pub(crate) struct CelpDecParams {
    pub(crate) voiced: bool,
    pub(crate) sf_pulses: [i32; SMPL_NUM_SUBFR],
    pub(crate) fcbg_idx: [i32; SMPL_NUM_SUBFR],
    pub(crate) nrgres_dbq_q14: [i32; SMPL_NUM_SUBFR],
    /// ACB gain index per subframe (voiced only).
    pub(crate) acbg_idx: [i32; SMPL_NUM_SUBFR],
    /// Per-40-block pitch lag (codec units: float), 8 per frame, 0 for unvoiced. Synthesis hands the
    /// two blocks of subframe `sf` (`block_lags[2*sf]`, `block_lags[2*sf+1]`) to the ACB/LTP basis, so
    /// fractional intra-subframe lag changes are preserved (`lags_per_subframe == 2` in C).
    pub(crate) block_lags: [f32; 2 * SMPL_NUM_SUBFR],
    pub(crate) total_pulses: i32,
}

/// Persistent decoder synthesis state (C float domain).
pub(crate) struct CelpDecState {
    noise: NoiseGenerator,
    acb_state: Vec<f32>,
    acb_state_len: usize,
    lpc_synth_mem: [f32; SMPL_LPC_ORDER],
    lsf_prev: [f32; SMPL_LPC_ORDER],
    prev_nrgres: f32,
    /// Post-LPC HP (pitch-harmonic) postfilter state, persistent across the stream.
    hp: super::smpl_harmcomb::HpPostfilterState,
    /// Test-only capture of the per-subframe pre-noise excitation (== C `exc_pre`), 80/subframe.
    #[cfg(test)]
    pub(crate) dbg_exc_pre: Vec<f32>,
}

impl Default for CelpDecState {
    fn default() -> Self {
        let acb_state_len = SMPL_SUBFR_LEN + 2 * SMPL_MAX_PITCH_LAG + SMPL_LTP_INTERPOL_DELAY;
        CelpDecState {
            noise: NoiseGenerator::default(),
            acb_state: vec![0.0; acb_state_len],
            acb_state_len,
            lpc_synth_mem: [0.0; SMPL_LPC_ORDER],
            lsf_prev: [0.0; SMPL_LPC_ORDER],
            prev_nrgres: 0.0,
            hp: super::smpl_harmcomb::HpPostfilterState::default(),
            #[cfg(test)]
            dbg_exc_pre: Vec::new(),
        }
    }
}

impl CelpDecState {
    /// Synthesize one 20 ms internal frame (4 subframes) into 320 float samples in [-1, 1], mirroring
    /// the `smpl_core_decoder.c` subframe loop. `nlsf` is the reconstructed order-16 NLSF (radians);
    /// `pulses` are the signed FCB pulse magnitudes (320 positions). `low_rate` is the TOC bit.
    pub(crate) fn synth_frame(
        &mut self,
        nlsf: &[f32],
        lsf_interpol_idx: usize,
        pulses: &[i32],
        params: &CelpDecParams,
        low_rate: bool,
        frame_length_16: i32,
        out: &mut [f32],
    ) {
        let gains = fcb_gains();
        // Per-subframe LPC interpolation.
        let mut a = [[0.0f32; SMPL_LPC_ORDER + 1]; SMPL_NUM_SUBFR];
        let mut lsfs = [[0.0f32; SMPL_LPC_ORDER]; SMPL_NUM_SUBFR];
        let interpol = &SMPL_LSF_INTERPOL_4[lsf_interpol_idx.min(1)];
        lpc_interpol(nlsf, &mut self.lsf_prev, interpol, &mut a, &mut lsfs);

        let normalized_bitrate = smpl_get_normalized_bitrate(params.total_pulses, frame_length_16);

        // Excitation: sparse FCB pulses scaled by the per-subframe FCB gain.
        let mut lpc_res = [0.0f32; SMPL_FRAME_LEN];
        let gain_tab: &[f32] = if params.voiced { &gains.v } else { &gains.uv };
        for pos in 0..SMPL_FRAME_LEN {
            if pulses[pos] != 0 {
                let sf = pos / SMPL_SUBFR_LEN;
                lpc_res[pos] = pulses[pos] as f32 * gain_tab[params.fcbg_idx[sf] as usize];
            }
        }

        let lags_per_subfr = 2; // 80-sample subframe / 40-sample lag subframe
        // Contiguous synthesis buffer: 16-sample history prefix + 320 frame samples.
        let mut ybuf = [0.0f32; SMPL_LPC_ORDER + SMPL_FRAME_LEN];
        ybuf[..SMPL_LPC_ORDER].copy_from_slice(&self.lpc_synth_mem);
        for sf in 0..SMPL_NUM_SUBFR {
            let base = sf * SMPL_SUBFR_LEN;
            // CELP (ACB/LTP) decode — adds the voiced adaptive-codebook contribution + updates state.
            // C `smpl_celp_decode` gets `&lags[frame*lags_per_frame + sf*lags_per_subframe]` with
            // lags_per_subframe == 2: the two 40-blocks of this subframe carry independent lags.
            let sf_lags = [params.block_lags[2 * sf], params.block_lags[2 * sf + 1]];
            celp_decode(
                &mut self.acb_state,
                self.acb_state_len,
                params.voiced,
                params.acbg_idx[sf],
                &sf_lags,
                lags_per_subfr,
                SMPL_SUBFR_LEN,
                low_rate,
                normalized_bitrate,
                &mut lpc_res[base..base + SMPL_SUBFR_LEN],
            );

            #[cfg(test)]
            self.dbg_exc_pre
                .extend_from_slice(&lpc_res[base..base + SMPL_SUBFR_LEN]);

            // Residual noise energy + shaped noise (the dominant unvoiced fix).
            let nrgres = smpl_decode_resnrg(params.nrgres_dbq_q14[sf], SMPL_SUBFR_LEN as i32);
            if !params.voiced {
                self.prev_nrgres = nrgres;
            }

            let mut noise = [0.0f32; 160];
            smpl_celp_gen_noise(
                &mut self.noise,
                &lpc_res[base..base + SMPL_SUBFR_LEN],
                SMPL_SUBFR_LEN,
                params.voiced,
                params.sf_pulses[sf],
                nrgres,
                params.fcbg_idx[sf],
                &lsfs[sf],
                normalized_bitrate,
                &gains.uv,
                &mut noise,
            );
            for i in 0..SMPL_SUBFR_LEN {
                lpc_res[base + i] += noise[i];
            }

            // LPC synthesis (contiguous history across subframes/frames).
            filt_ar16(
                &lpc_res[base..base + SMPL_SUBFR_LEN],
                &a[sf],
                &mut ybuf,
                SMPL_LPC_ORDER + base,
                SMPL_SUBFR_LEN,
            );
        }
        out[..SMPL_FRAME_LEN].copy_from_slice(&ybuf[SMPL_LPC_ORDER..]);
        self.lpc_synth_mem
            .copy_from_slice(&ybuf[SMPL_LPC_ORDER + SMPL_FRAME_LEN - SMPL_LPC_ORDER..]);

        // Post-LPC HP (pitch-harmonic) postfilter, per `smpl_core_decoder.c` (LPC postfilter is off on
        // this stream, tilt postfilter is low_rate-only). The comb lag is the energy-weighted mean of
        // the 8 per-40-block lags (0 -> the default fixed-corner curve, unvoiced).
        let lag = if params.voiced {
            let (mut sl, mut sll) = (0f32, 0f32);
            for &l in &params.block_lags {
                sl += l;
                sll += l * l;
            }
            if sl > 0.0 { sll / sl } else { 0.0 }
        } else {
            0.0
        };
        let mut hp_out = [0f32; SMPL_FRAME_LEN];
        super::smpl_harmcomb::smpl_hp_postfilter(
            &mut self.hp,
            &out[..SMPL_FRAME_LEN],
            SMPL_FRAME_LEN,
            lag,
            &mut hp_out,
        );
        out[..SMPL_FRAME_LEN].copy_from_slice(&hp_out);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::voip::mlow::smpl_decode::{SmplLsfState, decode_smpl_lsf, load_smpl_tables};
    use crate::voip::mlow::smpl_gains::decode_smpl_gains;
    use crate::voip::mlow::smpl_mem::load_smpl_mem;
    use crate::voip::mlow::smpl_pitch::decode_smpl_pitch;
    use crate::voip::mlow::smpl_pulse::decode_smpl_pulses;
    use crate::voip::mlow::smpl_synth::{load_smpl_synth_tables, smpl_reconstruct_nlsf};
    use serde_json::Value;

    /// Validate the pre-noise excitation (FCB pulses × gain + voiced ACB) against the C `exc_pre`
    /// dump, per subframe. This proves the excitation domain (`fcbgains_uv/v[fcbg_idx]`) and the
    /// voiced ACB/LTP synthesis are faithful, independent of the PRNG-driven noise.
    #[test]
    fn exc_pre_matches_c() {
        let recs: Value = serde_json::from_str(include_str!("testdata/exc_pre_lags.json")).unwrap();
        let carr = recs.as_array().unwrap();

        // Key C exc_pre by (packet, frame, sf).
        use std::collections::HashMap;
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

        let frames: Vec<String> =
            serde_json::from_str(include_str!("testdata/inbound_capture_frames.json")).unwrap();
        let tbl = load_smpl_tables();
        let synth_t = load_smpl_synth_tables();
        let mem = load_smpl_mem();
        let mut lstate = SmplLsfState::default();
        let mut celp = CelpDecState::default();
        let mut prev_nlsf: Vec<f32> = Vec::new();

        let (mut uv_ok, mut uv_bad, mut v_ok, mut v_bad) = (0, 0, 0, 0);
        let mut worst = 0f32;
        for (packet, hex_frame) in frames.iter().enumerate() {
            let frame = hex::decode(hex_frame).unwrap();
            if frame.is_empty() {
                continue;
            }
            let toc = crate::voip::mlow::toc::parse_mlow_toc(frame[0]);
            if toc.std_opus || toc.sid || !toc.active {
                continue;
            }
            let config = (frame[0] >> 2) as usize & 1;
            let low_rate = (frame[0] >> 2) & 1 != 0;
            let mut dec = crate::voip::mlow::rangecoder::RangeDecoder::new(&frame[1..]);
            for f in 0..3 {
                let lsf = decode_smpl_lsf(&mut dec, tbl, &mut lstate, config, f);
                let pulses =
                    decode_smpl_pulses(&mut dec, mem, 320, 4, 1, config as i32, lsf.stage1);
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
                        &mut lstate,
                        320,
                        4,
                        config as i32,
                        pulses.subfr,
                    );
                    for b in 0..8 {
                        params.block_lags[b] =
                            ((pr.block_lags[b] as f64 * 0.5 + 32.0).min(320.0)) as f32;
                    }
                    for sf in 0..4 {
                        params.acbg_idx[sf] = pr.gain_idx[sf];
                        params.fcbg_idx[sf] = pr.filt_idx[sf].max(0);
                    }
                } else {
                    let g = decode_smpl_gains(&mut dec, mem, 4, pulses.subfr);
                    params.nrgres_dbq_q14 = g.gain_q;
                    params.fcbg_idx = g.nrg_res;
                }
                let nlsf = smpl_reconstruct_nlsf(
                    synth_t,
                    lsf.stage1 as usize,
                    config,
                    lsf.grid as usize,
                    &lsf.stage2,
                    &prev_nlsf,
                );
                celp.dbg_exc_pre.clear();
                let mut sig = [0f32; SMPL_FRAME_LEN];
                celp.synth_frame(
                    &nlsf,
                    lsf.extra as usize,
                    &pulses.pulses,
                    &params,
                    low_rate,
                    320,
                    &mut sig,
                );
                prev_nlsf = nlsf;
                // Compare each subframe's exc_pre to C.
                for sf in 0..4 {
                    let Some(c) = cmap.get(&(packet as i64, f as i64, sf as i64)) else {
                        continue;
                    };
                    // Cross-check that our reconstructed per-block lags equal the C dump's two lags for
                    // this subframe (the decode that drives the ACB/LTP basis).
                    if voiced {
                        let clags = c["lags"].as_array().unwrap();
                        let c0 = clags[0].as_f64().unwrap() as f32;
                        let c1 = clags[1].as_f64().unwrap() as f32;
                        assert_eq!(
                            (params.block_lags[2 * sf], params.block_lags[2 * sf + 1]),
                            (c0, c1),
                            "per-block lags diverge at pkt={packet} f={f} sf={sf}"
                        );
                    }
                    // Reconstruct the dense C exc_pre from the sparse nonzero list.
                    let mut cexc = [0f32; SMPL_SUBFR_LEN];
                    for pair in c["nz"].as_array().unwrap() {
                        let p = pair.as_array().unwrap();
                        let idx = p[0].as_u64().unwrap() as usize;
                        cexc[idx] = p[1].as_f64().unwrap() as f32;
                    }
                    let base = sf * SMPL_SUBFR_LEN;
                    let mut bad = false;
                    for i in 0..SMPL_SUBFR_LEN {
                        let d = (celp.dbg_exc_pre[base + i] - cexc[i]).abs();
                        worst = worst.max(d);
                        // Excitation amplitudes are ~1e-4; a tight absolute tolerance.
                        if d > 2e-5 {
                            bad = true;
                        }
                    }
                    if voiced {
                        if bad { v_bad += 1 } else { v_ok += 1 }
                    } else if bad {
                        uv_bad += 1
                    } else {
                        uv_ok += 1
                    }
                }
            }
        }
        eprintln!(
            "exc_pre vs C: unvoiced ok={uv_ok} bad={uv_bad}; voiced ok={v_ok} bad={v_bad}; worst abs diff={worst:.2e}"
        );
        // Unvoiced excitation is deterministic (pulses × fcbgains_uv) — must match.
        assert_eq!(
            uv_bad, 0,
            "unvoiced exc_pre diverges from C ({uv_bad} subframes)"
        );
        // Voiced excitation (FCB pulses × gain + ACB/LTP per-block-lag synthesis) is also deterministic.
        assert_eq!(
            v_bad, 0,
            "voiced exc_pre diverges from C ({v_bad} subframes)"
        );
    }
}
```

`smpl_nrgres.rs`:

```rust
//! MLow unvoiced residual-energy quantizer: faithful port of `smpl_quant_nrg_res`
//! (`smpl_quant_nrg_res.c`). The unvoiced excitation LEVEL is carried entirely by the per-subframe
//! quantized residual-energy floor (`nrgres_dbq_Q14`): a frame-mean scalar quant plus a shape VQ.
//! Our decoder reads this floor back as `gain_q` (validated bit-exact in `param_decode_match`), so the
//! encoder must produce the same `nrgres_dbq_Q14` for the round-trip level to be right.
//!
//! The reconstruction is validated against the C `enc_dump` (the `nrgres_dbq_Q14` test). It is wired
//! live in `analysis.rs`'s unvoiced path: the wire gain block IS the nrgres layout
//! (`gain_main`==`nrgres_frame_qi`, `gain_delta`==`nrgres_shape_qi`, the gain table == the shape
//! codebook, `cb1` == the frame step), so `gain_q[sf]` decodes back as `nrgres_dbq_Q14`.
#![allow(dead_code)] // serde-only fields + test-only reconstruction helpers
#![allow(clippy::needless_range_loop)]

const SMPL_RES_NRG_BIAS: f32 = 3.1622776e-9;
const SMPL_RES_NRG_MIN_DB: f32 = -85.0;
const SMPL_RES_NRG_MAX_DB: f32 = 0.0;
/// `smpl_nrg_step_db_Q14[2]` (the 4-subframe table index).
const SMPL_NRG_STEP_DB_Q14_4: i32 = 16686;
const SMPL_RES_NRG_SHAPE_CB_N_4: usize = 98;

/// `nrgres_shape_CB_4_Q10` (98 vectors x 4 subframes), verbatim from `smpl_nrgres_tables.c`.
#[rustfmt::skip]
const NRGRES_SHAPE_CB_4_Q10: [i16; SMPL_RES_NRG_SHAPE_CB_N_4 * 4] = [
    -2515, -2238, 2632, 2121, 790, 3973, -2872, -1891, -533, 2847, 1453, -3767, -6174, -402, 2668, 3908,
    -1623, -1458, 153, 2928, -1254, 3197, -476, -1467, 1803, -1086, 270, -987, 1952, -66, -1257, -629,
    161, 19, -85, -96, 4833, 3147, -105, -7875, -1320, 1377, -1156, 1099, 3398, -2247, 1485, -2637,
    -3031, 2756, 1841, -1566, -1487, 2202, -2668, 1954, 5518, -5344, 522, -696, 8400, -3123, -6235, 958,
    5152, -2444, -2811, 102, 2513, -82, 1181, -3612, -561, -197, -1074, 1832, -294, -1250, -1839, 3383,
    5126, 522, -782, -4866, -7760, -5178, -1840, 14779, -1119, 6007, -1489, -3399, -4567, -2543, 1855, 5255,
    53, -1626, 67, 1506, -12256, -7706, -1982, 21943, 3549, -969, -1096, -1484, -10824, 2981, 2204, 5639,
    -229, 1106, 945, -1821, -9237, 10157, 1616, -2537, 4916, -199, -2177, -2540, 6673, 984, -3355, -4302,
    -7130, -4677, 8925, 2882, 445, 2762, -348, -2859, -196, -1859, 1761, 294, 2725, -2093, -966, 334,
    -3908, -308, 3675, 541, 735, 890, -2516, 891, 504, 1631, -1157, -977, -17817, 2119, 7104, 8594,
    -2056, 1897, -198, 356, 292, -4544, -287, 4538, -1455, -304, 603, 1156, -18259, -12643, 15247, 15655,
    4177, 1778, -1815, -4140, 1425, 576, -294, -1707, -1301, 5132, 2838, -6669, -4727, -3148, -905, 8781,
    -650, 152, -4654, 5152, 13746, 2320, -6259, -9807, -1356, 396, 3789, -2829, 2337, 1947, -29, -4256,
    6033, 820, -5730, -1123, -1795, 1091, 1080, -377, 2208, -1921, -3314, 3027, 9688, 5218, -3754, -11152,
    3814, -3941, -6183, 6310, -1017, -2391, 4393, -984, 10944, -1182, -5011, -4751, -4640, 7201, -218, -2343,
    -1278, 4720, -4212, 770, 2777, 1333, -5944, 1833, -16066, 8107, 5165, 2795, 2530, -5020, 6073, -3582,
    -2111, -7534, 4575, 5070, -8702, -3762, 4050, 8414, 1335, -997, -1567, 1229, 9348, 1534, -3959, -6922,
    2440, 1153, -2175, -1418, -2715, -4538, -4478, 11730, 569, -885, 2032, -1716, 3529, -91, -3218, -219,
    2157, -4121, 191, 1772, -2123, -1968, -1355, 5446, 1475, -354, 3651, -4772, 1654, -3521, 2726, -859,
    2393, 6820, -2958, -6255, -3861, 1365, 1177, 1319, 7614, -1638, -2789, -3187, -3628, -2635, 6902, -639,
    1925, 2295, -1451, -2769, -3683, 4517, -981, 147, -1260, -529, 2339, -550, 3013, 639, -1050, -2602,
    3651, 1959, -3218, -2391, 6267, 3124, -2926, -6464, -8180, 3900, 4191, 89, -3372, -611, 1042, 2941,
    -2510, 856, -925, 2579, -11667, -8436, 10605, 9498, 6427, -2733, 1887, -5581, 1581, -1722, -328, 469,
    2011, 1989, -3606, -394, -1014, 2197, -1200, 17, 1544, -2555, 765, 247, 1188, -183, 1966, -2972,
    -6057, 3480, -2284, 4860, -25659, 8466, 8891, 8303,
];

/// Result of `smpl_quant_nrg_res` for the 4-subframe path.
pub(crate) struct NrgResQuant {
    pub frame_qi: i32,
    pub shape_qi: i32,
    /// Per-subframe quantized residual-energy floor (Q14) — the decoder reads this as `gain_q`.
    pub dbq_q14: [i32; 4],
}

/// Port of `smpl_quant_nrg_res` for num_subfr == 4. `nrgres[sf]` is the per-subframe residual energy
/// `nrg(reslpc_sf)/subfrlen` in the SAME (int16-scaled) domain the C `reslpc` lives in.
pub(crate) fn quant_nrg_res_4(nrgres: &[f32; 4]) -> NrgResQuant {
    let mut nrgres_db = [0.0f32; 4];
    let mut frame_db = 0.0f32;
    for i in 0..4 {
        nrgres_db[i] = (10.0 * (nrgres[i] + SMPL_RES_NRG_BIAS).log10()).min(SMPL_RES_NRG_MAX_DB);
        frame_db += nrgres_db[i];
    }
    frame_db /= 4.0;
    let sc_q14 = 1.0f32 / (1i32 << 14) as f32;
    let frame_qi = ((frame_db - SMPL_RES_NRG_MIN_DB) / (sc_q14 * SMPL_NRG_STEP_DB_Q14_4 as f32))
        .round() as i32;
    let mut frame_dbq_q14 = frame_qi * SMPL_NRG_STEP_DB_Q14_4;
    frame_dbq_q14 += (SMPL_RES_NRG_MIN_DB as i32) * (1 << 14);
    for i in 0..4 {
        nrgres_db[i] -= frame_dbq_q14 as f32 * sc_q14;
    }
    // Shape VQ (min RD) over the 98 codebook vectors.
    let sc_q10 = 1.0f32 / (1i32 << 10) as f32;
    let mut best_rd = 1e30f32;
    let mut qi = 0usize;
    for n in 0..SMPL_RES_NRG_SHAPE_CB_N_4 {
        let mut rd = 0.0f32;
        for i in 0..4 {
            let d = nrgres_db[i] - NRGRES_SHAPE_CB_4_Q10[n * 4 + i] as f32 * sc_q10;
            rd += d * d;
        }
        if rd < best_rd {
            qi = n;
            best_rd = rd;
        }
    }
    let mut dbq_q14 = [0i32; 4];
    for i in 0..4 {
        dbq_q14[i] = frame_dbq_q14 + (NRGRES_SHAPE_CB_4_Q10[qi * 4 + i] as i32) * 16;
    }
    NrgResQuant {
        frame_qi,
        shape_qi: qi as i32,
        dbq_q14,
    }
}

/// Reconstruct the per-subframe `nrgres_dbq_Q14` from a frame/shape index pair, mirroring the C
/// `decode_lb_unvoiced` reconstruction. Used to validate the quantizer against the reference dump.
fn dbq_from_indices(frame_qi: i32, shape_qi: usize) -> [i32; 4] {
    let frame_dbq = frame_qi * SMPL_NRG_STEP_DB_Q14_4 + (SMPL_RES_NRG_MIN_DB as i32) * (1 << 14);
    std::array::from_fn(|sf| frame_dbq + (NRGRES_SHAPE_CB_4_Q10[shape_qi * 4 + sf] as i32) * 16)
}

#[cfg(test)]
mod tests {
    use super::*;

    // The reconstruction must match the C reference dump: for the first internal frame the C committed
    // nrgres_frame_qi=0, nrgres_shape_qi=8, yielding these exact per-subframe nrgres_dbq_Q14 (which the
    // decoder reads back as gain_q).
    #[test]
    fn dbq_reconstruction_matches_c_dump() {
        assert_eq!(
            dbq_from_indices(0, 8),
            [-1390064, -1392336, -1394000, -1394176]
        );
    }

    // Round-trip: quantizing a residual energy and reconstructing from the resulting indices must agree
    // with the direct per-subframe dbq the quantizer reports.
    #[test]
    fn quant_then_reconstruct_consistent() {
        let nrgres = [0.01f32, 0.02, 0.005, 0.03];
        let q = quant_nrg_res_4(&nrgres);
        assert_eq!(q.dbq_q14, dbq_from_indices(q.frame_qi, q.shape_qi as usize));
    }
}
```

## Go envelope (signatures only)

The corresponding Go declarations — exported types and function **signatures with
no bodies**. This is the surface to implement; it is not the implementation.

```go
package mlow

const (
	SmplOrder      = 16
	SmplSubfrLen   = 80  // 5 ms @ 16 kHz
	SmplIntfLen    = 320 // 20 ms internal frame
	SmplSubfrCount = 4
	SmplLtpHist    = 728
)

// --- NLSF reconstruction / synthesis tables ---

type SmplSynthTables struct {
	Valtables      [][][][][]float32 // [stage1][config][grid][coeff][sym]
	Centroids      [][][]float32     // [stage1][grid][16]
	Matrices       [][][][]float32   // [stage1][grid][row][col]
	MinSpacing     [][]float32       // [stage1][17]
	Grid16W        [][]float32
	Grid16Alpha    []float32
	Grid16Matrices [][][]float32 // [sig][config][256]
}

func LoadSmplSynthTables() *SmplSynthTables

func SmplReconstructNLSF(t *SmplSynthTables, stage1, config, grid int, stage2 *[16]int32, prevNLSF []float32) []float32

func SmplNLSF2A(nlsf []float32) []float32

func SmplGainLin(gainQ int32) float64

func SmplLTPFracGain(normGain float64) float32

// --- low-band synthesis (WASM func 3597 core) ---

type SmplExcGainState struct {
	S0 float32
	S1 float32
}

type SmplPitchSynth struct {
	Voiced   bool
	LagSubfr [4]float64
	NormGain float64
}

type SmplFrameSynth struct {
	// LPC state, LTP/excitation history, gain state, excitation + HP postfilter state.
}

func NewSmplFrameSynth() *SmplFrameSynth

func SmplLTPSubframePred(hist []float32, histPos int32, lagF, gainFrac float32, gst *SmplExcGainState, predOut []float32)

// Returns (signal, nlsf); nlsf becomes the next frame's prevNLSF.
func SynthInternalFrame(
	t *SmplSynthTables,
	st *SmplFrameSynth,
	stage1, config, grid int,
	stage2 *[16]int32,
	prevNLSF []float32,
	pulses []int32,
	gainQ *[4]int32,
	pitch *SmplPitchSynth,
) (signal []float32, nlsf []float32)

// --- C-float-domain CELP synthesis (smpl_core_decoder.c) ---

type CelpDecParams struct {
	Voiced       bool
	SfPulses     [4]int32
	FcbgIdx      [4]int32
	NrgresDbqQ14 [4]int32
	AcbgIdx      [4]int32
	BlockLags    [8]float32 // per-40-block pitch lag (codec units), 0 for unvoiced
	TotalPulses  int32
}

type CelpDecState struct {
	// noise generator, ACB state, LPC synthesis memory, prev LSF, HP postfilter state.
}

func NewCelpDecState() *CelpDecState

func (s *CelpDecState) SynthFrame(
	nlsf []float32,
	lsfInterpolIdx int,
	pulses []int32,
	params *CelpDecParams,
	lowRate bool,
	frameLength16 int32,
	out []float32,
)

// --- cross-frame decoder state ---

type SmplDecoderState struct {
	Lstate   SmplLsfState
	PrevNLSF []float32
	Celp     CelpDecState
	Harm     HarmPostfilterState
}

// --- unvoiced residual-energy quantizer (smpl_quant_nrg_res.c) ---

type NrgResQuant struct {
	FrameQi int32
	ShapeQi int32
	DbqQ14  [4]int32 // decoder reads this as gainQ
}

func QuantNrgRes4(nrgres *[4]float32) NrgResQuant
```

## Implementation suggestions (guidance, not authoritative)

- `usize` → Go `int`; `i32` → `int32`; `f32`/`f64` → `float32`/`float64`. The
  inner accumulations are deliberately mixed-precision: `smpl_lpc_synthesis` and
  `smpl_nlsf2a` accumulate in `f64` then truncate to `f32` per sample — preserve
  that exact widening or the bit-exact target drifts.
- Several buffers are sized as fixed arrays in the reference (e.g. the 16-tap FIR,
  the 17-coefficient LPC vector `a[0..16]`); keep those as Go arrays (`[N]float32`)
  rather than slices where the length is a compile-time constant, and use slices
  only where the reference uses `Vec`.
- The LTP/ACB history buffers are indexed with negative offsets relative to a
  moving base pointer (e.g. `state[(p + i) - il]`, `sig[(lb + k - int_lag)]`). In
  Go these must be plain `int` index arithmetic into one backing slice; the layout
  constants (`SMPL_LTP_HIST`, `acb_state_len`, the `comb_cur`-style offsets) are
  load-bearing — copy them verbatim.
- `f32::from_bits(i as u32)` in `smpl_gain_lin` is a raw IEEE-754 reinterpret of an
  `i32` as a `float32`. Use `math.Float32frombits(uint32(i))`. The surrounding
  `y as i32` saturating cast and the `2147483648.0` bounds must be reproduced
  exactly (Go float-to-int conversion of out-of-range values is undefined, so guard
  the bounds as the reference does).
- `smpl_floor_f32` is a custom floor (cast-to-int then adjust), not `math.Floor`;
  the equality test `int_lag as f32 == lag[sf]` distinguishes integer vs fractional
  lag and selects different code paths — keep the custom helper.
- The reference gates the Region-1 excitation comb and the post-LPC HP postfilter
  behind compile-time `false` constants (`SMPL_TAIL_REGION1`, `SMPL_HP_POSTFILTER`)
  in `smpl_synth.rs`, but the `smpl_celpdec.rs` path always runs the HP postfilter.
  `TODO(human):` decide whether the Go port exposes these as runtime flags or fixes
  them to the values the validation vector was captured under.
- The reference reaches into sibling modules (`smpl_harmcomb`, `smpl_postfilter`,
  `smpl_gennoise`, `smpl_decode`, `smpl_celp`'s ACB gain tables). Those are
  separate datasheets; wire the Go package's equivalents in by their envelope
  signatures.
- Errors: the reference does not return errors from the synthesis path (it
  `expect`s only when loading the JSON tables). `TODO(human):` decide whether table
  loading returns an error or panics in Go.
