<!-- Datasheet = three things only: the reference source VERBATIM, the Go envelope
     (signatures, no bodies), and implementation suggestions. No behavioral summary,
     no implementation. The verbatim source is the only authoritative content. -->

# Datasheet: `mlow/postfilter`

The decoder's postfilter chain: the excitation-domain harmonic comb (applied per
subframe before LPC synthesis), the post-LPC HP pitch-harmonic comb, and the
per-packet harmonic postfilter that runs on the full low-band output. Media
layer; the synthesis tail that shapes pitch harmonics in the decoded PCM.

**Validation vector:** `e2e_vectors.json` — the end-to-end decode vector. Copy it
verbatim into `mlow/testdata/`. (The two comb filters additionally pin against the
raw C dumps `hp_postfilter_vectors.raw` and `harm_postfilter_vectors.raw`.)

## Reference source (verbatim — authoritative)

`smpl_postfilter.rs`:

```rust
//! MLow excitation-domain HARMONIC COMB POSTFILTER (WASM func 3524 + leaf helpers). Applied per
//! subframe to the LB excitation BEFORE LPC synthesis (Region 1 of func 3597); its output is ADDED
//! back into the excitation. Derives a short pitch-resonant 2nd-order filter from the excitation's
//! own 3-lag autocorrelation (NOT the pitch lag), then resonates env-shaped noise through it. Ported
//! from the Go reference (`smpl_postfilter.go`); the comb resonance shape is bit-exact. The Go found
//! a single unresolved `8/7` output scalar vs the WASM export — we validate the composed output
//! against the real decoder instead of hardcoding it.
#![allow(
    clippy::needless_range_loop,
    clippy::excessive_precision,
    clippy::too_many_arguments,
    unused_assignments
)]

use super::smpl_harmcomb::smpl_pf_fir3;

/// func 9014 (expf) with the WASM's saturation bounds.
fn smpl_expf(x: f32) -> f32 {
    if x > 88.72283172607422 {
        return f32::INFINITY;
    }
    if x < -103.97207641601562 {
        return 0.0;
    }
    (x as f64).exp() as f32
}

/// func 3492 (log2f).
fn smpl_log2f(x: f32) -> f32 {
    if x.to_bits() == 0x3f800000 {
        return 0.0;
    }
    (x as f64).log2() as f32
}

/// func 3489: LCG white-noise fill scaled by 8.1e-10; `state` persists. Bit-exact vs WASM.
fn smpl_pf_noise_fill(out: &mut [f32], length: usize, state: &mut i32) {
    const MUL: i32 = 196314165;
    const ADD: i32 = 907633515;
    const SCALE: f32 = 8.100000115085493e-10;
    let mut s = *state;
    let mut i = 0;
    if length >= 4 {
        let end = length - 3;
        while i < end {
            s = MUL.wrapping_mul(s).wrapping_add(ADD);
            out[i] = s as f32 * SCALE;
            out[i + 1] = (s << 8) as f32 * SCALE;
            out[i + 2] = (s << 16) as f32 * SCALE;
            out[i + 3] = (s << 24) as f32 * SCALE;
            i += 4;
        }
        *state = s;
    }
    if length > i {
        s = *state;
        while i < length {
            s = MUL.wrapping_mul(s).wrapping_add(ADD);
            out[i] = s as f32 * SCALE;
            i += 1;
        }
        *state = s;
    }
}

/// func 3487: recursive RMS-envelope smoother (4-wide). Bit-exact vs WASM.
fn smpl_pf_env_smooth(
    input: &[f32],
    length: usize,
    coef: f32,
    prev_env: &mut f32,
    out: &mut [f32],
) {
    let c2 = coef * coef;
    let c1 = 1.0 - c2;
    let c10 = c2 * c1;
    let c11 = c2 * c2;
    let pe = *prev_env + 9.99999993922529e-09;
    let mut env = pe * pe;
    let mut i = 0;
    while i + 4 <= length {
        let s01 = input[i] * input[i] + input[i + 1] * input[i + 1];
        let v1 = ((c1 * s01 + c2 * env) as f64).sqrt() as f32;
        out[i] = v1;
        out[i + 1] = v1;
        env = c11 * env
            + c1 * (input[i + 2] * input[i + 2] + input[i + 3] * input[i + 3])
            + c10 * s01;
        let v3 = (env as f64).sqrt() as f32;
        out[i + 2] = v3;
        out[i + 3] = v3;
        i += 4;
    }
    if length > 0 {
        *prev_env = out[length - 1];
    }
}

/// Persistent harmonic-comb postfilter state (WASM func 3524's p0 struct). Zero value = fresh/reset.
#[derive(Default, Clone)]
pub(crate) struct SmplPostfilterState {
    pitch_gain: f32,
    pub(crate) env_state: f32,
    biq_state: [f32; 2],
    deemph_state: [f32; 2],
    smoothed_c: [f32; 3],
    reson_fir_state: [f32; 2],
    init_flag: i32,
    call_count: i32,
    lcg_state: i32,
}

/// Runtime g_pitch 3x16 comb-coeff decorrelation basis (WASM func 3559, ptr 0x12b2c0). Row 0 = DC
/// (all 0.25); rows 1/2 are cosine bases. Inlined from the live WASM dump.
const G_PITCH: [[f32; 16]; 3] = [
    [
        0.25, 0.25, 0.25, 0.25, 0.25, 0.25, 0.25, 0.25, 0.25, 0.25, 0.25, 0.25, 0.25, 0.25, 0.25,
        0.25,
    ],
    [
        0.24879617989063263,
        0.23923508822917938,
        0.2204803079366684,
        0.19325260818004608,
        0.15859831869602203,
        0.11784916371107101,
        0.07257115840911865,
        0.0245042834430933,
        -0.02450430579483509,
        -0.07257118076086044,
        -0.1178492084145546,
        -0.15859831869602203,
        -0.19325262308120728,
        -0.22048033773899078,
        -0.23923508822917938,
        -0.24879617989063263,
    ],
    [
        0.24519631266593933,
        0.20786739885807037,
        0.1388925462961197,
        0.04877255856990814,
        -0.04877258092164993,
        -0.13889259099960327,
        -0.20786741375923157,
        -0.24519632756710052,
        -0.24519631266593933,
        -0.20786738395690918,
        -0.1388925015926361,
        -0.04877259582281113,
        0.048772603273391724,
        0.13889260590076447,
        0.20786739885807037,
        0.24519632756710052,
    ],
];

/// Static de-emphasis FIR coef {0.25, -0.496, 0.25} (rodata 0xe8c74).
const PF_DEEMPH_COEF: [f32; 3] = [0.25, -0.49599999, 0.25];

/// func 3504: 1st-order FIR `t[n]=cf0*in[n]+cf1*in[n-1]` then 1-pole IIR `y[n]=t[n]-ci1*y[n-1]`.
fn smpl_pf_section1_full(
    input: &[f32],
    n: usize,
    cf0: f32,
    cf1: f32,
    ci1: f32,
    state: &mut [f32; 2],
    out: &mut [f32],
) {
    let src: Vec<f32> = input[..n].to_vec();
    let mut tmp = vec![0f32; n];
    let mut prev_in = state[0];
    for i in 0..n {
        let x = src[i];
        tmp[i] = cf0 * x + cf1 * prev_in;
        prev_in = x;
    }
    if n > 0 {
        state[0] = src[n - 1];
    }
    let neg_pole = -ci1;
    let mut y = state[1];
    for i in 0..n {
        y = tmp[i] + neg_pole * y;
        out[i] = y;
    }
    state[1] = y;
}

/// `g5 = sigmoid(0.2*(nrgEnv[1]-nrgEnv[0]+1e-30) - 3)`.
fn smpl_pf_trailing_pole(nrg_env: [f32; 2]) -> f32 {
    let ratio = 0.2 * (nrg_env[1] - nrg_env[0] + 1.0000000031710769e-30) - 3.0;
    if ratio > 80.0 {
        return 1.0;
    }
    if ratio < -80.0 {
        return 0.0;
    }
    1.0 / (1.0 + smpl_expf(-ratio))
}

/// Levinson-style 2-iteration resonator solve. Returns (r5, r8, denom, ok). Transcribed from the WASM.
fn smpl_comb_resonator_solve(c0: f32, c1: f32, c2: f32) -> (f32, f32, f32, bool) {
    let l28 = 1.0 / c0;
    let l27 = c1;
    let l29 = c2;
    let mut r5 = l28 * l29;
    let mut r8 = c1 / (c0 * (r5 + 1.0));
    let mut loop_n = 1i32;
    loop {
        let l18 = r5 * r5 + r8 * r8 + 1.0;
        let l22 = -2.0 / l18;
        let l21 = r8 * l22;
        let l20 = r5 * l21;
        let l26 = r8 * r5 + r8;
        let l23 = l26 * l21 + r5 + 1.0;
        let l24 = r8 + r8 + l18 * l21;
        let l30 = l20 * l20 + l23 * l23 + l24 * l24;
        let l25 = r5 * l22;
        let l21b = r5 * l25 + 1.0;
        let l22b = l26 * l25 + r8;
        let l25b = r5 + r5 + l18 * l25;
        let l31 = l21b * l21b + l22b * l22b + l25b * l25b;
        let dot = l20 * l21b + l23 * l22b + l24 * l25b;
        let det = l30 * l31 - dot * dot;
        if det < 9.999999747378752e-05 {
            return (r5, r8, r5 * r5 + r8 * r8 + 1.0, false);
        }
        let ndot = -dot;
        let l18b = l28 * l18;
        let l20c = l18b * l29 - r5;
        let l18c = l18b * l27 - l26;
        let l23d = l20c * l20c + l18c * l23;
        let l18d = l21b * l20c + l18c * l22b;
        let inv_det = 1.0 / det;
        r5 += (ndot * l23d + l18d * l30) * inv_det;
        r8 += (l31 * l23d + l18d * ndot) * inv_det;
        if loop_n == 0 {
            break;
        }
        loop_n = 0;
    }
    (r5, r8, r5 * r5 + r8 * r8 + 1.0, true)
}

/// func 3524: the harmonic comb postfilter on one subframe. `out` is the N-sample contribution the
/// caller ADDS into the excitation. `lag`/`gain8` are reserved (only the `active=false` path uses gain8).
pub(crate) fn smpl_comb_postfilter(
    st: &mut SmplPostfilterState,
    input: &[f32],
    n: usize,
    active: bool,
    gain8: f32,
    nrg_env: [f32; 2],
    out: &mut [f32],
) {
    let p3 = if active { 1 } else { 0 };
    let mut local5: f32 = 1.0;
    let local23: f32 = 1.0;

    let mut comb_c = [0f32; 3]; // fr[156,160,164]
    let mut noise = vec![0f32; n]; // fr[1488..]
    let mut reson = [0f32; 3]; // fr[4,8,12] = {g, a2, a1}

    if p3 == 1 {
        // (1) 3-lag autocorrelation.
        let mut auto = [0f32; 3];
        for l in 0..3 {
            let mut acc = 0f32;
            for k in 0..n - l {
                acc += input[k] * input[k + l];
            }
            auto[l] = acc;
        }
        let c0 = auto[0] + 9.999999960041972e-13;
        auto[0] = c0;
        local5 = c0;

        // (2) smooth into st.smoothed_c (coef 0.16 for N=80, 0.4 for N=160).
        let coef = if n == 160 {
            0.4000000059604645f32
        } else {
            0.1599999964237213f32
        };
        for i in 0..3 {
            let s = st.smoothed_c[i];
            st.smoothed_c[i] = coef * (auto[i] - s) + s;
        }
        local5 = local5 * 0.1224999949336052 / st.smoothed_c[0];
        // scaled = local5 * smoothedC, with lags 1 and 2 doubled (WASM fr[172],fr[176] *= 2).
        let scaled = [
            local5 * st.smoothed_c[0],
            local5 * st.smoothed_c[1] * 2.0,
            local5 * st.smoothed_c[2] * 2.0,
        ];

        // (2a) proj = g_pitch^T . scaled (3 -> 16); refl[i] = 1.5*peak - proj[i].
        let mut proj = [0f32; 16];
        for j in 0..16 {
            proj[j] =
                scaled[0] * G_PITCH[0][j] + scaled[1] * G_PITCH[1][j] + scaled[2] * G_PITCH[2][j];
        }
        let mut peak = proj[0];
        for j in 1..16 {
            if proj[j] > peak {
                peak = proj[j];
            }
        }
        let scale = peak * 1.5;
        let mut refl = [0f32; 16];
        for i in 0..16 {
            refl[i] = scale - proj[i];
        }
        // (2b) comb coeffs c[r] = g_pitch[r] . refl.
        for r in 0..3 {
            let mut acc = 0f32;
            for i in 0..16 {
                acc += G_PITCH[r][i] * refl[i];
            }
            comb_c[r] = acc;
        }

        // (3) noise fill.
        smpl_pf_noise_fill(&mut noise, n, &mut st.lcg_state);
        // (4) seed pitch_gain on first call.
        if st.init_flag == 0 {
            st.pitch_gain = st.env_state;
        }
        // (5) env-smooth input -> env.
        let mut env = vec![0f32; n];
        smpl_pf_env_smooth(input, n, 0.949999988079071, &mut st.pitch_gain, &mut env);
        // (6) env-shaped noise.
        for i in 0..n {
            noise[i] *= env[i];
        }
        // (7) normalize comb coeffs by the noise energy.
        let mut sum = 0f32;
        for i in 0..n {
            sum += noise[i] * noise[i];
        }
        local5 /= sum + 9.999999960041972e-13;
        for i in 0..3 {
            comb_c[i] *= local5;
        }
    } else {
        st.smoothed_c = [0.0; 3];
        st.lcg_state = 0;
        let mut energy = 0f32;
        for n_ in 0..n {
            energy += input[n_] * input[n_];
        }
        let mut env = vec![0f32; n];
        smpl_pf_env_smooth(input, n, 0.9950000047683716, &mut st.pitch_gain, &mut env);
        let local8 = gain8 * 20.0 + 10.0;
        let ratio = energy / (local5 + 9.999999682655225e-21);
        let e = smpl_expf(local8 * (1.0 - ratio));
        local5 = smpl_log2f(1.0 + e) * local5 / local8;
    }

    // (8) resonator coeffs from comb_c.
    let c0v = comb_c[0];
    if c0v >= 0.0 {
        let c0v = c0v + 1.0000000031710769e-30;
        comb_c[0] = c0v;
        let (r5, r8, denom, ok) = smpl_comb_resonator_solve(c0v, comb_c[1], comb_c[2]);
        if ok {
            let g = (c0v as f64 / denom as f64).sqrt() as f32;
            reson = [g, r8 * g, r5 * g];
        } else {
            reson = [(c0v as f64).sqrt() as f32, 0.0, 0.0];
        }
    } else {
        reson = [0.0, 0.0, 0.0];
    }

    // (9) resonator FIR over the env-shaped noise.
    let mut reson_out = vec![0f32; n];
    smpl_pf_fir3(&noise, n, reson, &mut st.reson_fir_state, &mut reson_out);

    // (10) seed/zero the trailing-biquad buffer.
    let mut trail = vec![0f32; n];
    if st.init_flag == 0 {
        smpl_pf_noise_fill(&mut trail, n, &mut st.lcg_state);
        let mut d = st.env_state * 0.9900000095367432;
        let mut i = 0;
        while i + 2 <= n {
            trail[i] *= d;
            trail[i + 1] *= d * 0.9900000095367432;
            d *= 0.9801000356674194;
            i += 2;
        }
    }
    // (callCount<=1 leaves trail zeroed, which it already is)

    // (11) de-emphasis FIR -> out.
    if st.init_flag != 0 || p3 != 0 {
        smpl_pf_fir3(&reson_out, n, PF_DEEMPH_COEF, &mut st.deemph_state, out);
    } else {
        for i in 0..n {
            out[i] = 0.0;
        }
    }

    // (12) trailing biquad, unless (active && callCount>1).
    if !(p3 != 0 && st.call_count > 1) {
        let g5 = smpl_pf_trailing_pole(nrg_env);
        let comb = local23 * 0.4 + 0.6;
        let v19 = if comb > 1.0 { 800.0 } else { comb * 800.0 };
        let mut band = (nrg_env[0] + nrg_env[1]) * 16000.0 / 12.566370964050293 * 3.0 * g5;
        if band > v19 {
            band = v19;
        }
        let w = if band >= 1500.0 {
            0.5625
        } else {
            band * 6.0 / 16000.0
        };
        let ar_coef = w - 1.0;
        let fircoef = (ar_coef * -0.5 + 1.0) * 0.8;
        let mut tb = vec![0f32; n];
        smpl_pf_section1_full(
            &trail,
            n,
            fircoef,
            -fircoef,
            ar_coef,
            &mut st.biq_state,
            &mut tb,
        );
        for i in 0..n {
            out[i] += tb[i];
        }
    }

    // (13) state bookkeeping.
    st.init_flag = p3;
    if p3 == 0 {
        st.biq_state[0] = 0.0;
        st.call_count = 0;
    }
    st.call_count += 1;
}
```

`smpl_harm_postfilter.rs`:

```rust
//! MLow harmonic postfilter (`smpl_harm_postfilter`) — the final per-packet pitch-comb that runs on
//! the full LB output after the HP postfilter. Faithful port of `smpl_harm_postfilter.c` (the LP
//! filter table) + `smpl_harm_postfilter_util.c` (the apply loop). It enhances pitch harmonics by
//! mixing `x[-lag] + x[+lag]` into the signal, low-pass filtered by a lag-dependent kernel, and it
//! introduces the codec's `SMPL_TOT_POSTFILT_DELAY = 48`-sample group delay (8 FB + 40 lag-subframe).
//!
//! The reference is built with `-ffast-math -mavx`, so the recursive/accumulating math is not
//! IEEE-strict; the Rust port matches it to within i16 output quantization (validated against a C
//! dump), not bit-for-bit.
#![allow(clippy::needless_range_loop)]

use std::sync::OnceLock;

const FRAME_LEN: usize = 320;
const MAX_FRAMES_PER_PACKET: usize = 6;
const MIN_PITCH_LAG: i32 = 32; // SMPL_MINPITCH_MS(2) * 16
const MAX_PITCH_LAG: i32 = 320; // SMPL_MAXPITCH_MS(20) * 16
const MAXPITCH_LEN: usize = 320; // SMPL_MAXPITCH_MS * SMPL_PITCH_FS_KHZ(16)
const FB_DELAY: usize = 8; // SMPL_HARM_POSTF_FB_DELAY
const LAG_SUBFR_LEN: usize = 40; // SMPL_HARM_POSTF_LAG_SUBFR_LEN = FRAME_LEN / 8
const HARM_DELAY: usize = LAG_SUBFR_LEN; // SMPL_HARM_POSTF_DELAY
/// Total group delay the harmonic postfilter introduces (`SMPL_TOT_POSTFILT_DELAY`), so the decoded
/// PCM aligns at lag 0 with the reference. Used by the delay-aligned round-trip/validation tests.
#[cfg(test)]
pub(crate) const TOT_POSTFILT_DELAY: usize = FB_DELAY + HARM_DELAY; // 48
const PITCH_NUM_SUBFRAMES: usize = 8;
const FB_STRENGTH: f32 = 0.4734;
const STRENGTH: f32 = 0.6438;
const CUTOFF_HZ: f32 = 4000.0;
const NHARM_CUTOFF: f32 = 6.3;
const REDUCTION_FAC: f32 = 0.0579;
const SMPL_PI: f32 = std::f32::consts::PI;

const STATE_COMB_LEN: usize = MAXPITCH_LEN + FRAME_LEN * MAX_FRAMES_PER_PACKET + HARM_DELAY;
const LP_FILT_RES: i32 = 2500;
const NUM_LP_FILT: usize = ((LP_FILT_RES / 80) - LP_FILT_RES / MAX_PITCH_LAG + 1) as usize;

#[inline]
fn lag_to_filt_ix(lag: i32) -> usize {
    (LP_FILT_RES / (lag + 30).max(80) - LP_FILT_RES / MAX_PITCH_LAG) as usize
}

/// LP-filter bank, one symmetric `2*FB_DELAY+1`-tap kernel per quantized lag bucket
/// (`smpl_create_harm_postfilt_tables`).
struct HarmTables {
    lp_filters: Vec<[f32; 2 * FB_DELAY + 1]>,
}

fn harm_tables() -> &'static HarmTables {
    static T: OnceLock<HarmTables> = OnceLock::new();
    T.get_or_init(|| {
        let mut filt_win = [0f32; FB_DELAY];
        let d_omega = (0.5 * SMPL_PI) / (FB_DELAY as f32 + 1.0);
        let mut omega = d_omega;
        for i in 0..FB_DELAY {
            filt_win[i] = omega.cos() / (i as f32 + 1.0);
            omega += d_omega;
        }
        let mut lp = vec![[0f32; 2 * FB_DELAY + 1]; NUM_LP_FILT];
        let mut ix_prev = -1i32;
        for lag in MIN_PITCH_LAG..=MAX_PITCH_LAG {
            let ix = lag_to_filt_ix(lag) as i32;
            if ix != ix_prev {
                let omega0 = 2.0 * SMPL_PI / lag as f32;
                create_lp_filter(omega0, &filt_win, &mut lp[ix as usize]);
                ix_prev = ix;
            }
        }
        HarmTables { lp_filters: lp }
    })
}

fn create_lp_filter(omega0: f32, filt_win: &[f32; FB_DELAY], blp: &mut [f32; 2 * FB_DELAY + 1]) {
    let omega_c = (omega0 * NHARM_CUTOFF).min(CUTOFF_HZ / 16000.0 * SMPL_PI);
    let mut sum_b = 0.0f32;
    let mut omega_c_sum = omega_c;
    for i in 0..FB_DELAY {
        let b = filt_win[i] * omega_c_sum.sin();
        omega_c_sum += omega_c;
        blp[FB_DELAY + i + 1] = b;
        blp[FB_DELAY - i - 1] = b;
        sum_b += 2.0 * b;
    }
    blp[FB_DELAY] = omega_c;
    sum_b += omega_c;
    let sc = 1.0 / sum_b;
    for v in blp.iter_mut() {
        *v *= sc;
    }
}

/// Persistent harm-postfilter state (C `HarmPst`). `prev_lag = 0` after init (first packet's lag).
#[derive(Clone)]
pub(crate) struct HarmPostfilterState {
    state1: [f32; 2 * FB_DELAY],
    lpcoefs: [f32; 2 * FB_DELAY + 1],
    state_comb: Vec<f32>,
    prev_lag: i32,
    prev_did_filter: i32,
}

impl Default for HarmPostfilterState {
    fn default() -> Self {
        HarmPostfilterState {
            state1: [0.0; 2 * FB_DELAY],
            lpcoefs: [0.0; 2 * FB_DELAY + 1],
            state_comb: vec![0.0; STATE_COMB_LEN],
            prev_lag: 0,
            prev_did_filter: 0,
        }
    }
}

#[inline]
fn dot_prod(a: &[f32], b: &[f32], l: usize) -> f32 {
    let mut r = 0.0f32;
    for i in 0..l {
        r += a[i] * b[i];
    }
    r
}

#[inline]
fn nrg(x: &[f32], n: usize) -> f32 {
    let mut r = 0.0f32;
    for i in 0..n {
        r += x[i] * x[i];
    }
    r
}

/// `smpl_filt_ma16_sym`: 17-tap symmetric MA. `x` has 16 samples of history at `x[-16..0]`; here it
/// reads from a base offset into the shared buffer.
#[inline]
fn filt_ma16_sym(buf: &[f32], x_base: usize, n: usize, coef: &[f32; 17], out: &mut [f32]) {
    for nn in 0..n {
        let c = x_base + nn;
        let mut res = buf[c - 8] * coef[8];
        for i in 0..8 {
            res += coef[i] * (buf[c - i] + buf[c - 16 + i]);
        }
        out[nn] = res;
    }
}

/// `harm_postfilter_core` for one 40-sample lag block. `comb` is the StateComb buffer; `comb_x` is
/// the index of this block's read pointer (`x` in C). `out`/`out_off` is the caller's destination
/// (`y_harm` in C) — it doubles as scratch and holds the final block output. The filtered result is
/// also fed back into `comb[comb_x - FB_DELAY ..]`, which is what makes the comb recursive and is the
/// 48-sample-delayed location the next packets read from.
#[allow(clippy::too_many_arguments)]
fn harm_postfilter_core(
    lpcoefs: &mut [f32; 2 * FB_DELAY + 1],
    comb: &mut [f32],
    comb_x: usize,
    future_samples: i32,
    lag: i32,
    diff: &mut [f32],
    diff_base: usize,
    out: &mut [f32],
    out_off: usize,
    l: usize,
    fb_strength: f32,
    prev_did_filter: &mut i32,
) {
    let tables = harm_tables();
    let lag_u = lag as usize;
    let mut xy = 0.0f32;
    // y_harm scratch lives in `out[out_off..out_off+l]` (the C `y_harm` parameter).
    if lag > 0 {
        let lookforward = l as i32 + lag - future_samples;
        if lookforward > 0 {
            let l2 = (l as i32 - lookforward).max(0) as usize;
            for i in 0..l2 {
                out[out_off + i] = comb[comb_x + i - lag_u] + comb[comb_x + i + lag_u];
            }
            for i in 0..(l - l2) {
                out[out_off + l2 + i] = comb[comb_x + l2 + i - lag_u] + comb[comb_x + l2 + i];
            }
        } else {
            for i in 0..l {
                out[out_off + i] = comb[comb_x + i - lag_u] + comb[comb_x + i + lag_u];
            }
        }
        xy = dot_prod(&comb[comb_x..], &out[out_off..], l);
    }
    if lag > 0 && xy > 0.0 {
        let xx = nrg(&comb[comb_x..], l);
        let yy = 0.25 * nrg(&out[out_off..], l);
        let strength = 0.5 * xy / yy.max(xx);
        let high_lag_reduction = 1.0
            - REDUCTION_FAC
                * ((lag - MIN_PITCH_LAG) as f32 / (MAX_PITCH_LAG - MIN_PITCH_LAG) as f32);
        let strength = strength * high_lag_reduction * STRENGTH;
        for i in 0..l {
            out[out_off + i] *= 0.5 * strength;
        }
        // diff = -strength * x + y_harm  (smpl_add_scale_vec(y_harm, x, diff, L, -strength))
        for i in 0..l {
            diff[diff_base + i] = out[out_off + i] + (-strength) * comb[comb_x + i];
        }
        let kernel = &tables.lp_filters[lag_to_filt_ix(lag)];
        for k in 0..(2 * FB_DELAY + 1) {
            lpcoefs[k] = kernel[k] * fb_strength;
        }
        let coef17: [f32; 17] = *lpcoefs;
        // y_harm = MA(diff); then y_harm += comb[x - FB_DELAY] (the 48-delayed base signal). The comb
        // is read-only here — `smpl_add_vec_inplace(x - FB_DELAY, y_harm, L)` modifies y_harm (2nd arg).
        let mut yh = [0f32; LAG_SUBFR_LEN];
        filt_ma16_sym(diff, diff_base, l, &coef17, &mut yh);
        for i in 0..l {
            out[out_off + i] = yh[i] + comb[comb_x - FB_DELAY + i];
        }
        *prev_did_filter = 1;
    } else {
        for v in diff[diff_base..diff_base + LAG_SUBFR_LEN].iter_mut() {
            *v = 0.0;
        }
        if *prev_did_filter != 0 {
            // zero-input response of the previous filter for the first 2*FB_DELAY samples, added onto
            // the delayed base; the tail is the plain 48-delayed comb.
            let coef17: [f32; 17] = *lpcoefs;
            let mut yh = [0f32; 2 * FB_DELAY];
            filt_ma16_sym(diff, diff_base, 2 * FB_DELAY, &coef17, &mut yh);
            for i in 0..(2 * FB_DELAY) {
                out[out_off + i] = yh[i] + comb[comb_x - FB_DELAY + i];
            }
            for i in (2 * FB_DELAY)..l {
                out[out_off + i] = comb[comb_x + FB_DELAY + i - 2 * FB_DELAY];
            }
        } else {
            for i in 0..l {
                out[out_off + i] = comb[comb_x - FB_DELAY + i];
            }
        }
        *prev_did_filter = 0;
    }
}

/// Apply the harmonic postfilter to a full packet in place. `x` is `packetlen_16` samples; `lags`
/// are the per-40-block lags (`nlags = packetlen/40`), `normalized_bitrate` is the packet average.
pub(crate) fn smpl_harm_postfilter(
    st: &mut HarmPostfilterState,
    x: &mut [f32],
    x_len: usize,
    lags: &[f32],
    n_lags: usize,
    normalized_bitrate: f32,
) {
    debug_assert_eq!(x_len, n_lags * LAG_SUBFR_LEN);
    // diff buffer with 16 samples of history prefix (C: diff_[FRAME_LEN + 2*FB_DELAY], diff = +2*FB_DELAY).
    const DIFF_PREFIX: usize = 2 * FB_DELAY;
    let mut diff = vec![0f32; FRAME_LEN + DIFF_PREFIX];

    let mut lag = st.prev_lag;
    // StateComb layout: [history | current packet]; current packet starts at MAX_PITCH_LAG + HARM_DELAY.
    let comb_cur = MAX_PITCH_LAG as usize + HARM_DELAY;
    st.state_comb[comb_cur..comb_cur + x_len].copy_from_slice(&x[..x_len]);

    let fb_strength = 1.0 - FB_STRENGTH * normalized_bitrate;
    let mut offset1 = 0usize;

    let mut lag_ctr = 0usize;
    while lag_ctr < n_lags {
        let mut offset2 = 0usize;
        // diff[-16..0] = state1
        diff[DIFF_PREFIX - 16..DIFF_PREFIX].copy_from_slice(&st.state1);
        let lag_ctr_end = (lag_ctr + PITCH_NUM_SUBFRAMES).min(n_lags);
        while lag_ctr < lag_ctr_end {
            let comb_x = MAX_PITCH_LAG as usize + offset1;
            let future_samples = HARM_DELAY as i32 + x_len as i32 - offset1 as i32;
            harm_postfilter_core(
                &mut st.lpcoefs,
                &mut st.state_comb,
                comb_x,
                future_samples,
                lag,
                &mut diff,
                DIFF_PREFIX + offset2,
                x,
                offset1,
                LAG_SUBFR_LEN,
                fb_strength,
                &mut st.prev_did_filter,
            );
            offset1 += LAG_SUBFR_LEN;
            offset2 += LAG_SUBFR_LEN;
            lag = lags[lag_ctr].round() as i32;
            lag_ctr += 1;
        }
        // state1 = diff[offset2-16 .. offset2]
        st.state1
            .copy_from_slice(&diff[DIFF_PREFIX + offset2 - 16..DIFF_PREFIX + offset2]);
    }

    st.prev_lag = lag;
    // shift StateComb left by x_len
    st.state_comb.copy_within(x_len..x_len + comb_cur, 0);
}

#[cfg(test)]
mod tests {
    use super::*;

    fn rf32(b: &[u8], o: &mut usize) -> f32 {
        let v = f32::from_le_bytes([b[*o], b[*o + 1], b[*o + 2], b[*o + 3]]);
        *o += 4;
        v
    }
    fn ri32(b: &[u8], o: &mut usize) -> i32 {
        let v = i32::from_le_bytes([b[*o], b[*o + 1], b[*o + 2], b[*o + 3]]);
        *o += 4;
        v
    }

    /// Validate `smpl_harm_postfilter` against the instrumented C decoder, processing the full active
    /// packet sequence in order (the filter carries StateComb/state1/prev_lag across packets). The
    /// dump carries, per packet, the per-block lags, the packet bitrate, the input (post-hp) signal,
    /// and the C output.
    ///
    /// The reference is `-ffast-math` (reassociating/FMA-contracting), so this is not bit-for-bit.
    /// Two regimes: every voiced packet and every steady silence packet match within the i16 output
    /// quantization step (the comb math is feed-forward there). The only larger residual is the first
    /// 48 samples of a *silence packet immediately following voiced* — the comb's zero-input response,
    /// driven recursively by the prior frame's `-ffast-math`-built LP coefficients. That residual is
    /// bounded by `TRANSITION_TOL` and only lands on near-silent transitions, so it is inaudible.
    #[test]
    fn harm_postfilter_matches_c() {
        const I16_LSB: f32 = 1.0 / 32768.0;
        // The voiced→silence transition zero-input response under -ffast-math; bulk stays under I16_LSB.
        const TRANSITION_TOL: f32 = 6.0e-4;
        let data = include_bytes!("testdata/harm_postfilter_vectors.raw");
        let mut o = 0usize;
        let count = ri32(data, &mut o);
        let mut st = HarmPostfilterState::default();
        let mut worst = 0f32;
        let mut worst_steady = 0f32;
        for _ in 0..count {
            let _packet = ri32(data, &mut o);
            let plen = ri32(data, &mut o) as usize;
            let nlags = ri32(data, &mut o) as usize;
            let nbr = rf32(data, &mut o);
            let mut lags = vec![0f32; nlags];
            for l in lags.iter_mut() {
                *l = rf32(data, &mut o);
            }
            let mut inp = vec![0f32; plen];
            for v in inp.iter_mut() {
                *v = rf32(data, &mut o);
            }
            let mut cout = vec![0f32; plen];
            for v in cout.iter_mut() {
                *v = rf32(data, &mut o);
            }
            // A silent packet (lag0 == 0) carries the transition zero-input response in its first 48
            // samples; everywhere else is the i16-exact regime.
            let transition = lags[0] == 0.0;
            smpl_harm_postfilter(&mut st, &mut inp, plen, &lags, nlags, nbr);
            for i in 0..plen {
                let d = (inp[i] - cout[i]).abs();
                worst = worst.max(d);
                if !(transition && i < TOT_POSTFILT_DELAY) {
                    worst_steady = worst_steady.max(d);
                }
            }
        }
        eprintln!(
            "harm_postfilter vs C: packets={count} worst={worst:.2e} worst_steady={worst_steady:.2e} \
             (i16 LSB={I16_LSB:.2e})"
        );
        assert!(
            worst_steady < I16_LSB,
            "harm_postfilter steady-state diverges from C by {worst_steady:.2e} (>= i16 LSB {I16_LSB:.2e})"
        );
        assert!(
            worst < TRANSITION_TOL,
            "harm_postfilter transition residual {worst:.2e} exceeds tolerance {TRANSITION_TOL:.2e}"
        );
    }
}
```

`smpl_harmcomb.rs`:

```rust
//! MLow HP (harmonic/pitch) postfilter — the post-LPC-synthesis comb that resonates the output at the
//! PITCH frequency. Ported from the C source (`smpl_postfilter_util.c::smpl_hp_postfilter` +
//! `smpl_calc_hp_coefs.c` + `smpl_postfilter.c::get_hp_pitch_coefs`). Structure per frame:
//!   de-emphasis (AR1 leaky integrator {1,-0.995}) -> ARMA2 comb (MA2 numerator + AR2 denominator,
//!   coefficients derived from the pitch lag f=1/lag) -> companion pre-emphasis (MA1 differentiator).
//!
//! This replaces an earlier reverse-engineered "spectral-tilt resonator keyed on the band energy
//! ratio". That RE had two bugs the real source disproves: the comb keys on the PITCH LAG (f=1/lag),
//! not an energy ratio, and the AR denominator radius factor uses the `arr` curve (negative -> stable
//! pole), which the RE had swapped with `arf` (positive -> unstable). The constants themselves matched.
#![allow(clippy::needless_range_loop, clippy::excessive_precision)]

use std::sync::OnceLock;

/// Low-emphasis coef pair {1.0, -0.995}: AR1 = de-emphasis (leaky integrator), MA1 = companion
/// pre-emphasis (differentiator). The comb is bracketed by these.
const LO_EMPH: [f32; 2] = [1.0, -0.995];

/// 1.2 dB peak voiced pitch-comb curve (`get_hp_pitch_coefs`): maf, arf (cos angle), arr (radius).
const HP_PITCH_MAF: f32 = 0.1;
const HP_PITCH_ARF: [f32; 2] = [0.608057355, 0.070939485];
const HP_PITCH_ARR: [f32; 2] = [-2.187380512, 2.291030664];
/// Default (lag<=0) curve (`smpl_get_hp_coefs`), corner 50 Hz -> f = 50/16000.
const HP_DEF_MAF: f32 = 0.1;
const HP_DEF_ARF: [f32; 2] = [0.728508218, 0.476039848];
const HP_DEF_ARR: [f32; 2] = [-4.363803713, 8.441854006];
const HP_DEF_FCORNER_HZ: f32 = 50.0;

const SMPL_PI: f32 = 3.1415927410125;
const LAG_CHANGE_THRESHOLD: f32 = 1.25;
const FRAME_LEN: usize = 320;
const HP_POSTF_TRANSITION_SPEED: f32 = 2.0;

/// Persistent HP-postfilter state (C `HpPst`). `lag_old < 0` marks a fresh/reset filter.
#[derive(Clone)]
pub(crate) struct HpPostfilterState {
    state_lo_emph1: f32,
    state_lo_emph2: f32,
    state_hp: [f32; 4], // [ma2 x[-1], ma2 x[-2], ar2 y[-1], ar2 y[-2]]
    lag_old: f32,
    x_old: Vec<f32>,
    coef_ma: [f32; 3],
    coef_ar: [f32; 3],
}

impl Default for HpPostfilterState {
    fn default() -> Self {
        Self {
            state_lo_emph1: 0.0,
            state_lo_emph2: 0.0,
            state_hp: [0.0; 4],
            lag_old: -1.0,
            x_old: vec![0.0; FRAME_LEN],
            coef_ma: [0.0; 3],
            coef_ar: [0.0; 3],
        }
    }
}

/// `cos_approx(x) = 1 - 0.5*x^2` (the source's small-angle cosine).
#[inline]
fn cos_approx(x: f32) -> f32 {
    1.0 - 0.5 * x * x
}

/// 3-tap FIR with carried 2-sample input history (`smpl_filt_ma2`, general/monic). Also reused by the
/// excitation postfilter (comb #1).
pub(crate) fn smpl_pf_fir3(
    input: &[f32],
    n: usize,
    coef: [f32; 3],
    state: &mut [f32; 2],
    out: &mut [f32],
) {
    let xm1 = state[0];
    let xm2 = state[1];
    for i in 0..n {
        let p1 = if i >= 1 { input[i - 1] } else { xm1 };
        let p2 = if i >= 2 {
            input[i - 2]
        } else if i == 1 {
            xm1
        } else {
            xm2
        };
        out[i] = coef[0] * input[i] + coef[1] * p1 + coef[2] * p2;
    }
    if n >= 2 {
        state[0] = input[n - 1];
        state[1] = input[n - 2];
    } else if n == 1 {
        state[1] = xm1;
        state[0] = input[0];
    }
}

/// 2nd-order all-pole `y[n] = in[n] - c1*y[n-1] - c2*y[n-2]` (`smpl_filt_ar2`, monic), state {y[-1],y[-2]}.
/// Replicates the C scalar 4-wide unrolled block (precomputed coefficient powers) verbatim so the
/// floating-point rounding is bit-exact with `smpl_filt.c` (the codec was built without NEON).
fn smpl_filt_ar2(input: &[f32], n: usize, c1: f32, c2: f32, state: &mut [f32; 2], out: &mut [f32]) {
    let mut ytmp0 = state[1];
    let mut ytmp1 = state[0];
    let ar1 = -c1;
    let ar2 = -c2;
    let ar1_2 = ar1 * ar1;
    let ar1_3 = ar1 * ar1_2;
    let ar1_4 = ar1 * ar1_3;
    let imp1 = ar1;
    let imp2 = ar1_2 + ar2;
    let imp3 = ar1_3 + 2.0 * ar1 * ar2;
    let imp4 = ar1_4 + ar2 * ar2 + 3.0 * ar1_2 * ar2;
    let ymp1 = ar2;
    let ymp2 = ar2 * imp1;
    let ymp3 = ar2 * imp2;
    let ymp4 = ar2 * imp3;
    let mut nn = 0usize;
    while nn + 3 < n {
        let xtmp0 = input[nn];
        let xtmp1 = input[nn + 1];
        let xtmp2 = input[nn + 2];
        out[nn + 2] = xtmp2 + imp1 * xtmp1 + imp2 * xtmp0 + imp3 * ytmp1 + ymp3 * ytmp0;
        let xtmp3 = input[nn + 3];
        out[nn + 3] =
            xtmp3 + imp1 * xtmp2 + imp2 * xtmp1 + imp3 * xtmp0 + imp4 * ytmp1 + ymp4 * ytmp0;
        out[nn] = xtmp0 + imp1 * ytmp1 + ymp1 * ytmp0;
        out[nn + 1] = xtmp1 + imp1 * xtmp0 + imp2 * ytmp1 + ymp2 * ytmp0;
        ytmp0 = out[nn + 2];
        ytmp1 = out[nn + 3];
        nn += 4;
    }
    while nn < n {
        out[nn] = input[nn] + ar1 * ytmp1 + ar2 * ytmp0;
        ytmp0 = ytmp1;
        ytmp1 = out[nn];
        nn += 1;
    }
    state[1] = ytmp0;
    state[0] = ytmp1;
}

/// AR1 leaky integrator `y[n] = x[n] - c1*y[n-1]` (`smpl_filt_ar1`, here the de-emphasis {1,-0.995}).
/// Replicates the C scalar 5-wide unrolled block (precomputed `ar1` powers) for bit-exact rounding.
fn smpl_filt_ar1(input: &[f32], n: usize, c1: f32, state: &mut f32, out: &mut [f32]) {
    let ar1 = -c1;
    let ar1_2 = ar1 * ar1;
    let ar1_3 = ar1 * ar1_2;
    let ar1_4 = ar1 * ar1_3;
    let ar1_5 = ar1 * ar1_4;
    let mut ytmp = *state;
    let mut nn = 0usize;
    while nn + 4 < n {
        let xtmp0 = input[nn];
        let xtmp1 = input[nn + 1];
        let xtmp2 = input[nn + 2];
        let xtmp3 = input[nn + 3];
        let xtmp4 = input[nn + 4];
        out[nn + 4] =
            xtmp4 + ar1 * xtmp3 + ar1_2 * xtmp2 + ar1_3 * xtmp1 + ar1_4 * xtmp0 + ar1_5 * ytmp;
        out[nn] = xtmp0 + ar1 * ytmp;
        out[nn + 1] = xtmp1 + ar1 * xtmp0 + ar1_2 * ytmp;
        out[nn + 2] = xtmp2 + ar1 * xtmp1 + ar1_2 * xtmp0 + ar1_3 * ytmp;
        out[nn + 3] = xtmp3 + ar1 * xtmp2 + ar1_2 * xtmp1 + ar1_3 * xtmp0 + ar1_4 * ytmp;
        ytmp = out[nn + 4];
        nn += 5;
    }
    while nn < n {
        ytmp = input[nn] + ytmp * ar1;
        out[nn] = ytmp;
        nn += 1;
    }
    *state = ytmp;
}

/// MA1 `y[n] = x[n] + c1*x[n-1]` (`smpl_filt_ma1`, here the companion pre-emphasis {1,-0.995}).
fn smpl_filt_ma1(input: &[f32], n: usize, c1: f32, state: &mut f32, out: &mut [f32]) {
    let prev = *state;
    for i in (1..n).rev() {
        out[i] = input[i] + c1 * input[i - 1];
    }
    if n > 0 {
        out[0] = input[0] + c1 * prev;
        *state = input[n - 1];
    }
}

/// `smpl_get_hp_coefs`: the default fixed-corner ARMA2 biquad (also the encoder's input high-pass).
/// `fcorner_hz` clamped to [5, 1500]; `f = fcorner/16000`.
pub(crate) fn smpl_get_hp_coefs(fcorner_hz: f32) -> ([f32; 3], [f32; 3]) {
    let fc = fcorner_hz.clamp(5.0, 1500.0);
    smpl_calc_hp_coefs(HP_DEF_MAF, HP_DEF_ARF, HP_DEF_ARR, fc / 16000.0)
}

/// `smpl_filt_arma2`: MA2 numerator then AR2 denominator, shared 4-wide state {ma x[-1],x[-2], ar y[-1],y[-2]}.
pub(crate) fn smpl_filt_arma2(
    input: &[f32],
    n: usize,
    coef_ma: [f32; 3],
    coef_ar: [f32; 3],
    state: &mut [f32; 4],
    out: &mut [f32],
) {
    let mut tmp = vec![0f32; n];
    let mut ma_st = [state[0], state[1]];
    smpl_pf_fir3(input, n, coef_ma, &mut ma_st, &mut tmp);
    state[0] = ma_st[0];
    state[1] = ma_st[1];
    let mut ar_st = [state[2], state[3]];
    smpl_filt_ar2(&tmp, n, coef_ar[1], coef_ar[2], &mut ar_st, out);
    state[2] = ar_st[0];
    state[3] = ar_st[1];
}

/// `smpl_calc_hp_coefs`: build the unity-numerator-DC comb biquad. The AR denominator is a resonance
/// at the pitch angle `2*pi*arf*f` with radius `1 + arr*f` (arr negative -> stable), then the MA
/// numerator is scaled so the comb has unity DC gain.
fn smpl_calc_hp_coefs(maf: f32, arf: [f32; 2], arr: [f32; 2], f: f32) -> ([f32; 3], [f32; 3]) {
    let mut coef_ma = [1.0f32, -2.0 * cos_approx(2.0 * SMPL_PI * maf * f), 1.0];
    let far_ = arf[0] * f + arf[1] * f * f;
    let rar_ = arr[0] * f + arr[1] * f * f;
    let coef_ar = [
        1.0,
        -2.0 * cos_approx(2.0 * SMPL_PI * far_) * (1.0 + rar_),
        1.0 + (2.0 * rar_ + rar_ * rar_),
    ];
    let sc = (1.0 - coef_ar[1] + coef_ar[2]) / (1.0 - coef_ma[1] + coef_ma[2]);
    for c in coef_ma.iter_mut() {
        *c *= sc;
    }
    (coef_ma, coef_ar)
}

/// `new_coefs`: voiced pitch curve when lag>0 (f=1/lag), else the default 50 Hz-corner curve.
fn new_coefs(st: &mut HpPostfilterState, lag: f32) {
    let (ma, ar) = if lag > 0.0 {
        let f = 1.0 / lag;
        smpl_calc_hp_coefs(HP_PITCH_MAF, HP_PITCH_ARF, HP_PITCH_ARR, f)
    } else {
        let fc = HP_DEF_FCORNER_HZ.clamp(5.0, 1500.0);
        smpl_calc_hp_coefs(HP_DEF_MAF, HP_DEF_ARF, HP_DEF_ARR, fc / 16000.0)
    };
    st.coef_ma = ma;
    st.coef_ar = ar;
}

/// `cos(omega)^2` down-ramp for the lag-change overlap-add (built like `smpl_create_postfilt_tables`).
/// `omega` is accumulated by repeated addition (not `d_omega * i`) to stay bit-exact with the C table.
fn ramp_dn(len: usize) -> &'static Vec<f32> {
    static RAMP: OnceLock<Vec<f32>> = OnceLock::new();
    RAMP.get_or_init(|| {
        let d_omega = SMPL_PI / (2.0 * (FRAME_LEN as f32 + 1.0));
        let mut omega = d_omega;
        let mut v = Vec::with_capacity(FRAME_LEN);
        for _ in 0..FRAME_LEN {
            v.push(omega.cos().powf(HP_POSTF_TRANSITION_SPEED));
            omega += d_omega;
        }
        v
    });
    debug_assert_eq!(len, FRAME_LEN);
    RAMP.get().unwrap()
}

/// Apply the HP (pitch-harmonic) postfilter to one frame's post-LPC output. `lag` is the frame's
/// average pitch lag (`sum(l^2)/sum(l)` over the subframe lags), 0 for unvoiced.
pub(crate) fn smpl_hp_postfilter(
    st: &mut HpPostfilterState,
    x_in: &[f32],
    n: usize,
    lag: f32,
    out: &mut [f32],
) {
    // de-emphasis (AR1) into a working buffer.
    let mut x = vec![0f32; n];
    smpl_filt_ar1(x_in, n, LO_EMPH[1], &mut st.state_lo_emph1, &mut x);

    let mut overlap = false;
    let mut y_old = vec![0f32; n];
    if st.lag_old < 0.0 {
        new_coefs(st, lag);
        st.lag_old = lag;
    } else if lag > LAG_CHANGE_THRESHOLD * st.lag_old || LAG_CHANGE_THRESHOLD * lag < st.lag_old {
        overlap = true;
        smpl_filt_arma2(&x, n, st.coef_ma, st.coef_ar, &mut st.state_hp, &mut y_old);
        new_coefs(st, lag);
        st.lag_old = lag;
        let x_old = st.x_old.clone();
        let mut dummy = vec![0f32; n];
        smpl_filt_arma2(
            &x_old,
            n,
            st.coef_ma,
            st.coef_ar,
            &mut st.state_hp,
            &mut dummy,
        );
    } else if lag != st.lag_old {
        new_coefs(st, lag);
        st.lag_old = lag;
    }
    st.x_old[..n].copy_from_slice(&x[..n]);

    let mut y_tmp = vec![0f32; n];
    smpl_filt_arma2(&x, n, st.coef_ma, st.coef_ar, &mut st.state_hp, &mut y_tmp);

    if overlap {
        let ramp = ramp_dn(n);
        for i in 0..n {
            y_tmp[i] += (y_old[i] - y_tmp[i]) * ramp[i];
        }
    }

    // companion pre-emphasis (MA1).
    smpl_filt_ma1(&y_tmp, n, LO_EMPH[1], &mut st.state_lo_emph2, out);
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Seed a state from a C `HpPst` snapshot (test-only; mirrors the dumped field order).
    #[allow(clippy::too_many_arguments)]
    fn seed_state(
        lo1: f32,
        lo2: f32,
        hp: [f32; 4],
        lag_old: f32,
        x_old: Vec<f32>,
        coef_ma: [f32; 3],
        coef_ar: [f32; 3],
    ) -> HpPostfilterState {
        HpPostfilterState {
            state_lo_emph1: lo1,
            state_lo_emph2: lo2,
            state_hp: hp,
            lag_old,
            x_old,
            coef_ma,
            coef_ar,
        }
    }

    fn rf32(b: &[u8], o: &mut usize) -> f32 {
        let v = f32::from_le_bytes([b[*o], b[*o + 1], b[*o + 2], b[*o + 3]]);
        *o += 4;
        v
    }
    fn ri32(b: &[u8], o: &mut usize) -> i32 {
        let v = i32::from_le_bytes([b[*o], b[*o + 1], b[*o + 2], b[*o + 3]]);
        *o += 4;
        v
    }

    /// Validate `smpl_hp_postfilter` against the instrumented C decoder. Each active frame is
    /// self-contained: the dump carries the C `HpPst` state in, the 8 per-40-block lags, the pre-hp
    /// signal, and the post-hp signal; we seed, run, and compare sample-for-sample. The frame lag is
    /// the energy-weighted mean `sum(l^2)/sum(l)` over the 8 lags (0 -> default 50 Hz curve).
    ///
    /// The reference libopus is built with `-ffast-math -mavx`, which reassociates the recursive
    /// AR/MA accumulations (and may contract to FMA). Straightforward IEEE-strict Rust therefore
    /// cannot reproduce its output to the last bit through the near-unit-circle pitch-comb feedback —
    /// a 1-ULP de-emphasis difference drifts to ~1.5e-5 across the resonant pole. We assert the error
    /// stays well under the i16 output quantization step (1/32768 ≈ 3.05e-5), i.e. inaudible and
    /// identical once written to the 16-bit PCM the codec emits.
    #[test]
    fn hp_postfilter_matches_c() {
        const I16_LSB: f32 = 1.0 / 32768.0;
        let data = include_bytes!("testdata/hp_postfilter_vectors.raw");
        let mut o = 0usize;
        let count = ri32(data, &mut o);
        let mut worst = 0f32;
        for _ in 0..count {
            let _packet = ri32(data, &mut o);
            let _frame = ri32(data, &mut o);
            let mut lags = [0f32; 8];
            for l in lags.iter_mut() {
                *l = rf32(data, &mut o);
            }
            let lo1 = rf32(data, &mut o);
            let lo2 = rf32(data, &mut o);
            let mut hp = [0f32; 4];
            for h in hp.iter_mut() {
                *h = rf32(data, &mut o);
            }
            let lag_old = rf32(data, &mut o);
            let mut x_old = vec![0f32; FRAME_LEN];
            for x in x_old.iter_mut() {
                *x = rf32(data, &mut o);
            }
            let mut coef_ma = [0f32; 3];
            for c in coef_ma.iter_mut() {
                *c = rf32(data, &mut o);
            }
            let mut coef_ar = [0f32; 3];
            for c in coef_ar.iter_mut() {
                *c = rf32(data, &mut o);
            }
            let mut y_pre = vec![0f32; FRAME_LEN];
            for y in y_pre.iter_mut() {
                *y = rf32(data, &mut o);
            }
            let mut y_post = vec![0f32; FRAME_LEN];
            for y in y_post.iter_mut() {
                *y = rf32(data, &mut o);
            }

            let lag = if lags[0] > 0.0 {
                let (mut sl, mut sll) = (0f32, 0f32);
                for &l in &lags {
                    sl += l;
                    sll += l * l;
                }
                sll / sl
            } else {
                0.0
            };

            let mut st = seed_state(lo1, lo2, hp, lag_old, x_old, coef_ma, coef_ar);
            let mut out = vec![0f32; FRAME_LEN];
            smpl_hp_postfilter(&mut st, &y_pre, FRAME_LEN, lag, &mut out);
            for i in 0..FRAME_LEN {
                worst = worst.max((out[i] - y_post[i]).abs());
            }
        }
        eprintln!(
            "hp_postfilter vs C: frames={count} worst_abs_diff={worst:.2e} (i16 LSB={I16_LSB:.2e})"
        );
        assert!(
            worst < I16_LSB,
            "hp_postfilter diverges from C by {worst:.2e} (>= i16 LSB {I16_LSB:.2e})"
        );
    }
}
```

## Go envelope (signatures only)

The corresponding Go declarations — exported types and function **signatures with
no bodies**. This is the surface to implement; it is not the implementation.

```go
package mlow

// --- excitation-domain harmonic comb (WASM func 3524) ---

type SmplPostfilterState struct {
	// pitchGain, EnvState, biquad/de-emphasis/resonator FIR state, smoothed autocorrelation,
	// init flag, call count, LCG state.
	EnvState float32
}

// out is the n-sample contribution the caller ADDS into the excitation.
func SmplCombPostfilter(
	st *SmplPostfilterState,
	input []float32,
	n int,
	active bool,
	gain8 float32,
	nrgEnv [2]float32,
	out []float32,
)

// --- post-LPC HP (pitch-harmonic) comb ---

type HpPostfilterState struct {
	// lo-emph AR1/MA1 state, ARMA2 comb state, lagOld, xOld history, coefMA/coefAR.
}

func NewHpPostfilterState() *HpPostfilterState

// Shared 3-tap FIR with carried 2-sample input history.
func SmplPfFir3(input []float32, n int, coef [3]float32, state *[2]float32, out []float32)

// Default fixed-corner ARMA2 biquad; returns (coefMA, coefAR).
func SmplGetHpCoefs(fcornerHz float32) (coefMA, coefAR [3]float32)

func SmplFiltArma2(input []float32, n int, coefMA, coefAR [3]float32, state *[4]float32, out []float32)

// lag is the frame's average pitch lag (sum(l^2)/sum(l)), 0 for unvoiced.
func SmplHpPostfilter(st *HpPostfilterState, xIn []float32, n int, lag float32, out []float32)

// --- per-packet harmonic postfilter (smpl_harm_postfilter.c) ---

type HarmPostfilterState struct {
	// state1 history, lpcoefs, stateComb buffer, prevLag, prevDidFilter.
}

func NewHarmPostfilterState() *HarmPostfilterState

// Applies the harmonic postfilter to a full packet IN PLACE. x is xLen samples; lags are the
// per-40-block lags (nLags = packetlen/40); normalizedBitrate is the packet average.
func SmplHarmPostfilter(
	st *HarmPostfilterState,
	x []float32,
	xLen int,
	lags []float32,
	nLags int,
	normalizedBitrate float32,
)
```

## Implementation suggestions (guidance, not authoritative)

- `usize` → Go `int`; `i32` → `int32`; `f32` → `float32`. Several inner steps in
  the comb (`smpl_pf_env_smooth`, the resonator gain `sqrt`) deliberately promote
  to `f64` for the `sqrt`/`exp`/`log2` then truncate back to `f32`. Use `float64(x)`
  → `math.Sqrt` → `float32(...)` and preserve the promotion points exactly.
- The LCG noise fill writes 4 lanes per iteration as `s`, `s<<8`, `s<<16`, `s<<24`
  reinterpreted as signed `i32` then cast to `f32`. Reproduce the wrapping i32
  multiply/add (`MUL.wrapping_mul(s).wrapping_add(ADD)`) with Go `int32` arithmetic
  (Go `int32` overflow wraps), and the left-shift-then-cast lanes as
  `float32(int32(uint32(s) << k))`.
- `smpl_filt_ar1` / `smpl_filt_ar2` are written as the C compiler's unrolled
  (5-wide / 4-wide) scalar blocks with precomputed coefficient powers, on purpose,
  for bit-exact rounding. Do NOT simplify them back to the naive recursion — the
  intermediate rounding differs. Keep the unrolled form and the scalar tail loop.
- `smpl_filt_ma1` iterates the body in reverse (`(1..n).rev()`) before fixing up
  `out[0]` and the state; keep that ordering since the buffer may alias.
- `x.to_bits() == 0x3f800000` in `smpl_log2f` is an exact bit-pattern test for
  `1.0f32`; use `math.Float32bits(x) == 0x3f800000`. `f32::from_bits` elsewhere maps
  to `math.Float32frombits`.
- The reference uses fixed-array state (`[2]`, `[3]`, `[4]`, `[2*FB_DELAY+1]==[17]`)
  and a heap `Vec` only for the long `state_comb` / `x_old` buffers. Mirror that:
  Go arrays for the small fixed-length state, slices for the long history buffers.
- The two comb filters (`smpl_hp_postfilter`, `smpl_harm_postfilter`) are only
  matched to within the i16 output-quantization step (`1/32768`), not bit-for-bit,
  because the reference C was built with `-ffast-math -mavx`. `TODO(human):` decide
  the Go validation tolerance against `e2e_vectors.json` / the raw C dumps; do not
  expect exact float equality through the near-unit-circle pitch pole.
- `smpl_harm_postfilter` mutates `x` in place and advances `state_comb` via
  `copy_within`; in Go use `copy(dst, src)` on overlapping subslices of the same
  backing array, taking the same forward/left-shift direction the reference uses.
