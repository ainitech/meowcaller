<!-- Datasheet = three things only: the reference source VERBATIM, the Go envelope
     (signatures, no bodies), and implementation suggestions. No behavioral summary,
     no implementation. The verbatim source is the only authoritative content. -->

# Datasheet: `mlow/noise`

Two synthesis-domain building blocks: the CELP noise generator that builds the
shaped residual noise the decoder adds into the LPC excitation, and the encoder
perceptual-weighting front-end (FFT-based perceptual autocorrelation plus the
bitrate controller). Media layer; decoder excitation shaping and encoder rate
control.

**Validation vector:** `gennoise_vectors.json` — the instrumented-C noise-generator
vectors (input `NoiseGenerator` state, excitation, params, lsf, expected
`noise[80]` and output state). Copy it verbatim into `mlow/testdata/`.

## Reference source (verbatim — authoritative)

`smpl_gennoise.rs`:

```rust
//! Faithful port of the smpl_opus CELP noise generator (`smpl_celp_gen_noise` + `add_noise_uv`,
//! `smpl_celp_util.c`) and its helpers (`smpl_RAND`, `smpl_gen_rand_pulses`, `smpl_get_env`/`get_env0`,
//! `smpl_sigmoid`, `smpl_spec_fact2`, the noise DCT) plus `smpl_decode_resnrg`
//! (`smpl_quant_nrg_res.c`) and `smpl_get_normalized_bitrate` (`smpl_helpers.c`).
//!
//! The decoder runs this per subframe: it derives the shaped residual noise that the real codec
//! adds into the LPC excitation before synthesis. Without it the excitation is bare FCB pulses
//! (buzzy/robotic, starved highs). The unvoiced branch is the dominant path; the voiced branch
//! shapes a small high-band noise floor.
//!
//! The float constants are byte-exact copies of the C f32 literals.
#![allow(clippy::excessive_precision)]
#![allow(clippy::needless_range_loop)]
// SMPL_PI is the verbatim C literal `3.1415926535897f`; keep it for bit-exactness.
#![allow(clippy::approx_constant)]

const SMPL_MAX_SF_LEN: usize = 160;
const SMPL_NOISE_CORR_ORDER: usize = 2;
const SMPL_NOISE_DCT_ORDER: usize = 16;
const SMPL_CELP_FS_KHZ: usize = 16;
const SMPL_PI: f32 = 3.1415926535897f32;

const SMPL_DEC_NOISE_V_NOISE_GAIN: f32 = 0.35;
const SMPL_DEC_NOISE_UV_NOISE_GAIN: f32 = 0.8;
const SMPL_DEC_NOISE_UV_FCORNER_HZ: f32 = 800.0;
const SMPL_ENV_SMTH_COEF_V: f32 = 0.95;
const SMPL_ENV_SMTH_COEF_UV: f32 = 0.995;
const SMPL_ENV_SMTH_COEF_UV_V: f32 = 0.99;

const SMPL_RES_NRG_BIAS: f32 = 3.1622776e-9;

/// `smpl_RAND`: LCG, wrapping i32 arithmetic (C uses u32 mul + add on the i32 seed).
#[inline]
fn smpl_rand(seed: i32) -> i32 {
    // smpl_MLA_ovflw(907633515, seed, 196314165) = 907633515 + (u32)seed * 196314165
    (907633515i32).wrapping_add((seed as u32).wrapping_mul(196314165) as i32)
}

/// `smpl_sigmoid` with the same +-80 clamp as C (avoids inf/denormal under fast-math).
#[inline]
fn smpl_sigmoid(x: f32) -> f32 {
    if x > 80.0 {
        return 1.0;
    }
    if x < -80.0 {
        return 0.0;
    }
    1.0 / (1.0 + (-x).exp())
}

#[inline]
fn smpl_nrg(x: &[f32]) -> f32 {
    let mut nrg = 0.0f32;
    for &v in x {
        nrg += v * v;
    }
    nrg
}

#[inline]
fn smpl_sum(x: &[f32]) -> f32 {
    let mut s = 0.0f32;
    for &v in x {
        s += v;
    }
    s
}

#[inline]
fn smpl_maximum(x: &[f32]) -> f32 {
    let mut m = x[0];
    for &v in &x[1..] {
        if v > m {
            m = v;
        }
    }
    m
}

/// `smpl_gen_rand_pulses` (live version): 4-at-a-time bit-rotated white pulses scaled by 8.1e-10.
fn smpl_gen_rand_pulses(noise: &mut [f32], l: usize, seed: &mut i32) {
    const SC: f32 = 8.1e-10;
    let mut i = 0;
    while i + 3 < l {
        *seed = smpl_rand(*seed);
        let s = *seed as u32;
        noise[i] = SC * (*seed as f32);
        noise[i + 1] = SC * ((s << 8) as i32 as f32);
        noise[i + 2] = SC * ((s << 16) as i32 as f32);
        noise[i + 3] = SC * ((s << 24) as i32 as f32);
        i += 4;
    }
    while i < l {
        *seed = smpl_rand(*seed);
        noise[i] = SC * (*seed as f32);
        i += 1;
    }
}

/// `smpl_get_env`: squared-signal smoothing envelope (4-wide, mirrors the C accumulation order).
fn smpl_get_env(
    exc: &[f32],
    len: usize,
    mut smth_coef: f32,
    smth_state: &mut f32,
    env: &mut [f32],
) {
    smth_coef *= smth_coef; // operate on squared signal
    let mut state = *smth_state + 1e-8;
    state *= state;
    let gain_coef = 1.0 - smth_coef;
    let smth_coef2 = smth_coef * smth_coef;
    let gain_smth_coef = gain_coef * smth_coef;
    let mut i = 0;
    while i + 3 < len {
        let tmp0 = exc[i] * exc[i] + exc[i + 1] * exc[i + 1];
        let tmp1 = exc[i + 2] * exc[i + 2] + exc[i + 3] * exc[i + 3];
        let y1 = gain_coef * tmp1 + gain_smth_coef * tmp0 + smth_coef2 * state;
        let y0 = gain_coef * tmp0 + smth_coef * state;
        env[i] = y0.sqrt();
        env[i + 1] = env[i];
        env[i + 2] = y1.sqrt();
        env[i + 3] = env[i + 2];
        state = y1;
        i += 4;
    }
    *smth_state = env[len - 1];
}

/// `smpl_get_env0`: decaying envelope when there is no excitation to seed from.
fn smpl_get_env0(len: usize, smth_coef: f32, smth_state: &mut f32, env: &mut [f32]) {
    let smth_coef2 = smth_coef * smth_coef;
    env[0] = (*smth_state + 1e-8) * smth_coef;
    env[1] = env[0];
    let mut i = 2;
    while i + 2 < len {
        env[i + 2] = env[i - 1] * smth_coef2;
        env[i + 3] = env[i + 2];
        env[i] = env[i - 1] * smth_coef;
        env[i + 1] = env[i];
        i += 4;
    }
    env[len - 2] = env[len - 3] * smth_coef;
    env[len - 1] = env[len - 2];
    *smth_state = env[len - 1];
}

// ---- filters (smpl_filt.c) ----

/// `smpl_filt_ma1` (coef_len=2, state_len=1). `x != y`.
fn smpl_filt_ma1(x: &[f32], n: usize, coef: &[f32; 2], state: &mut [f32; 1], y: &mut [f32]) {
    if coef[0] == 1.0 {
        for k in 1..n {
            y[k] = x[k] + coef[1] * x[k - 1];
        }
    } else {
        for k in 0..n {
            y[k] = coef[0] * x[k];
        }
        for k in 1..n {
            y[k] += coef[1] * x[k - 1];
        }
    }
    y[0] = coef[0] * x[0] + coef[1] * state[0];
    state[0] = x[n - 1];
}

/// `smpl_filt_ar1` (coef_len=2, state_len=1, coef[0]==1).
fn smpl_filt_ar1(x: &[f32], n: usize, coef: &[f32; 2], state: &mut [f32; 1], y: &mut [f32]) {
    let ar1 = -coef[1];
    let mut ytmp = state[0];
    for nn in 0..n {
        ytmp = x[nn] + ytmp * ar1;
        y[nn] = ytmp;
    }
    state[0] = ytmp;
}

/// `smpl_filt_arma1`: MA1 then AR1, state {ma, ar}. Supports in-place (x==y) via a temp buffer.
fn smpl_filt_arma1(
    x: &[f32],
    n: usize,
    coef_ma: &[f32; 2],
    coef_ar: &[f32; 2],
    state: &mut [f32; 2],
    y: &mut [f32],
) {
    let mut tmp = [0.0f32; SMPL_MAX_SF_LEN];
    let mut ma_state = [state[0]];
    smpl_filt_ma1(x, n, coef_ma, &mut ma_state, &mut tmp);
    state[0] = ma_state[0];
    let mut ar_state = [state[1]];
    smpl_filt_ar1(&tmp, n, coef_ar, &mut ar_state, y);
    state[1] = ar_state[0];
}

/// `smpl_filt_ma2` (coef_len=3, state_len=2). `x != y`.
fn smpl_filt_ma2(x: &[f32], n: usize, coef: &[f32; 3], state: &mut [f32; 2], y: &mut [f32]) {
    if coef[0] == 1.0 {
        for i in 1..n {
            y[i] = x[i] + coef[1] * x[i - 1];
        }
    } else {
        for i in 0..n {
            y[i] = coef[0] * x[i];
        }
        for i in 1..n {
            y[i] += coef[1] * x[i - 1];
        }
    }
    for i in 2..n {
        y[i] += coef[2] * x[i - 2];
    }
    y[0] = coef[0] * x[0] + coef[1] * state[0] + coef[2] * state[1];
    y[1] += coef[2] * state[0];
    state[0] = x[n - 1];
    state[1] = x[n - 2];
}

/// `smpl_spec_fact2`: spectral factorization of a 3-tap autocorrelation into a 3-tap MA.
fn smpl_spec_fact2(c_in: &[f32; 3], a: &mut [f32; 3]) {
    let mut c = *c_in;
    c[0] += 1e-30;
    let inv_c0 = 1.0 / c[0];
    let mut r2 = c[2] * inv_c0;
    let mut r1 = c[1] / (c[0] * (1.0 + r2));
    for _ in 0..2 {
        let v0 = 1.0 + r1 * r1 + r2 * r2;
        let v1 = r1 + r1 * r2;
        let mut s = -2.0 / v0;
        let da0 = s * r1;
        let da1 = s * r2;
        s = v0 * inv_c0;
        let e1 = s * c[1] - v1;
        let e2 = s * c[2] - r2;
        let r0 = 2.0 * r1 + v0 * da0;
        let r3 = 2.0 * r2 + v0 * da1;
        let mut rr00 = r0 * r0;
        let mut rr01 = r0 * r3;
        let mut rr11 = r3 * r3;
        let rcap1 = 1.0 + r2 + v1 * da0;
        let r4 = r1 + v1 * da1;
        rr00 += rcap1 * rcap1;
        rr01 += rcap1 * r4;
        rr11 += r4 * r4;
        let mut re0 = rcap1 * e1;
        let mut re1 = r4 * e1;
        let r2c = r2 * da0;
        let r5 = 1.0 + r2 * da1;
        rr00 += r2c * r2c;
        rr01 += r2c * r5;
        rr11 += r5 * r5;
        re0 += r2c * e2;
        re1 += r5 * e2;
        s = rr00 * rr11 - rr01 * rr01;
        if s < 1e-4 {
            break;
        }
        s = 1.0 / s;
        r1 += (rr11 * re0 - rr01 * re1) * s;
        r2 += (-rr01 * re0 + rr00 * re1) * s;
    }
    let sc = (c[0] / (1.0 + r1 * r1 + r2 * r2)).sqrt();
    a[0] = sc;
    a[1] = sc * r1;
    a[2] = sc * r2;
}

/// The noise DCT (`dct_mat_t[NOISE_CORR_ORDER+1][NOISE_DCT_ORDER]`) built like `smpl_create_celp_tables`.
fn noise_dct() -> &'static [[f32; SMPL_NOISE_DCT_ORDER]; SMPL_NOISE_CORR_ORDER + 1] {
    use std::sync::OnceLock;
    static DCT: OnceLock<[[f32; SMPL_NOISE_DCT_ORDER]; SMPL_NOISE_CORR_ORDER + 1]> =
        OnceLock::new();
    DCT.get_or_init(|| {
        let mut m = [[0.0f32; SMPL_NOISE_DCT_ORDER]; SMPL_NOISE_CORR_ORDER + 1];
        let sc = 1.0f32 / (SMPL_NOISE_DCT_ORDER as f32).sqrt();
        for i in 0..SMPL_NOISE_DCT_ORDER {
            let d_omega = ((0.5 + i as f32) * SMPL_PI) / SMPL_NOISE_DCT_ORDER as f32;
            let mut omega = 0.0f32;
            for j in 0..SMPL_NOISE_CORR_ORDER + 1 {
                m[j][i] = omega.cos() * sc;
                omega += d_omega;
            }
        }
        m
    })
}

/// `smpl_matrix_mult_transp_16`: y[0..16] = sum_j C[j*16 + i] * x[j].
fn matrix_mult_transp_16(
    c: &[[f32; SMPL_NOISE_DCT_ORDER]; SMPL_NOISE_CORR_ORDER + 1],
    x: &[f32],
    y: &mut [f32],
    len_x: usize,
) {
    let mut yt = [0.0f32; SMPL_NOISE_DCT_ORDER];
    let xtmp = x[0];
    for i in 0..SMPL_NOISE_DCT_ORDER {
        yt[i] = c[0][i] * xtmp;
    }
    for j in 1..len_x {
        let xtmp = x[j];
        for i in 0..SMPL_NOISE_DCT_ORDER {
            yt[i] += c[j][i] * xtmp;
        }
    }
    y[..SMPL_NOISE_DCT_ORDER].copy_from_slice(&yt);
}

/// `smpl_matrix_mult`: y[i] = dot(C[i*len_x ..], x, len_x). C here is dct_mat_t flattened by [j][i].
fn matrix_mult(
    c: &[[f32; SMPL_NOISE_DCT_ORDER]; SMPL_NOISE_CORR_ORDER + 1],
    x: &[f32],
    y: &mut [f32],
) {
    // C is treated row-major as C[i][k]; but dct_mat_t is [k][i]. The C call passes
    // &pTbl->dct_mat_t[0][0] with len_y=CORR+1, len_x=DCT_ORDER, reading row i = dct_mat_t flat
    // offset i*DCT_ORDER, i.e. dct_mat_t[i][*]. So row i is dct_mat_t[i].
    for i in 0..SMPL_NOISE_CORR_ORDER + 1 {
        let mut acc = 0.0f32;
        for k in 0..SMPL_NOISE_DCT_ORDER {
            acc += c[i][k] * x[k];
        }
        y[i] = acc;
    }
}

/// `smpl_get_normalized_bitrate` (`smpl_helpers.c`).
pub(crate) fn smpl_get_normalized_bitrate(num_pulses: i32, frame_length_16: i32) -> f32 {
    let pulses_per_20ms = (num_pulses * frame_length_16) as f32 / (20.0 * 16.0);
    smpl_sigmoid(1.4 * (pulses_per_20ms + 1.0).log2() - 6.5)
}

/// `smpl_decode_resnrg` (`smpl_quant_nrg_res.c`). `nrgres_frame_dbq_Q14` is the per-subframe Q14
/// residual-energy value; returns the linear residual energy scaled by the subframe length.
pub(crate) fn smpl_decode_resnrg(nrgres_frame_dbq_q14: i32, fcb_subfrlen: i32) -> f32 {
    let exp = 0.1 * (nrgres_frame_dbq_q14 as f32 / ((1i32 << 14) as f32));
    let mut resnrg = 10.0f32.powf(exp) - SMPL_RES_NRG_BIAS;
    if resnrg < 0.0 {
        resnrg = 0.0;
    }
    resnrg * fcb_subfrlen as f32
}

/// Persistent decoder-side noise generator state (mirrors C `NoiseGenerator`).
#[derive(Clone)]
pub(crate) struct NoiseGenerator {
    pub(crate) env_smth: f32,
    pub(crate) env_last: f32,
    pub(crate) out_state_uv: [f32; 2],
    pub(crate) out_state_v: [f32; 2],
    pub(crate) corr_smth: [f32; SMPL_NOISE_CORR_ORDER + 1],
    pub(crate) shape_state: [f32; SMPL_NOISE_CORR_ORDER],
    pub(crate) prev_voiced: bool,
    pub(crate) since_unvoiced: i32,
    pub(crate) rand_seed: i32,
}

impl Default for NoiseGenerator {
    fn default() -> Self {
        // C zero-inits the whole struct in smpl_core_decode_init.
        NoiseGenerator {
            env_smth: 0.0,
            env_last: 0.0,
            out_state_uv: [0.0; 2],
            out_state_v: [0.0; 2],
            corr_smth: [0.0; SMPL_NOISE_CORR_ORDER + 1],
            shape_state: [0.0; SMPL_NOISE_CORR_ORDER],
            prev_voiced: false,
            since_unvoiced: 0,
            rand_seed: 0,
        }
    }
}

const COEF_MA_V: [f32; 3] = [0.25, -0.496, 0.25];

/// `add_noise_uv`: HP-shape the unvoiced noise (corner climbs when LSF[0..1] are close) and add it
/// into `noise`. `fcbgains_uv` is the codec's UV gain table (only `lsf[0..1]` are read).
fn add_noise_uv(
    ng: &mut NoiseGenerator,
    exc_noise_uv: &mut [f32],
    l: usize,
    lsf: &[f32],
    nrg_ratio: f32,
    noise: &mut [f32],
) {
    let lsf_hz = 16000.0 * (lsf[0] + lsf[1]) / (4.0 * SMPL_PI);
    let min_uv_fcorner_hz = lsf_hz * 3.0 * smpl_sigmoid(0.2 / (lsf[1] - lsf[0] + 1e-30) - 3.0);
    let mut uv_fcorner_hz = SMPL_DEC_NOISE_UV_FCORNER_HZ * (0.6f32 + 0.4 * nrg_ratio).min(1.0);
    uv_fcorner_hz = uv_fcorner_hz.max(min_uv_fcorner_hz);
    uv_fcorner_hz = uv_fcorner_hz.min(1500.0);
    let coef_tmp = 6.0 * uv_fcorner_hz / 16000.0;
    let coef_ma_uv = [
        (1.0 - 0.5 * coef_tmp) * SMPL_DEC_NOISE_UV_NOISE_GAIN,
        -((1.0 - 0.5 * coef_tmp) * SMPL_DEC_NOISE_UV_NOISE_GAIN),
    ];
    let coef_ar_uv = [1.0, -1.0 + coef_tmp];
    let mut filtered = [0.0f32; SMPL_MAX_SF_LEN];
    smpl_filt_arma1(
        exc_noise_uv,
        l,
        &coef_ma_uv,
        &coef_ar_uv,
        &mut ng.out_state_uv,
        &mut filtered,
    );
    exc_noise_uv[..l].copy_from_slice(&filtered[..l]);
    for i in 0..l {
        noise[i] += exc_noise_uv[i];
    }
}

/// `smpl_celp_gen_noise`: build the shaped residual noise for one subframe (faithful port).
#[allow(clippy::too_many_arguments)]
pub(crate) fn smpl_celp_gen_noise(
    ng: &mut NoiseGenerator,
    exc_lpc: &[f32],
    l: usize,
    voiced: bool,
    num_pulses: i32,
    nrgres: f32,
    fcbg_idx: i32,
    lsf: &[f32],
    normalized_bitrate: f32,
    fcbgains_uv: &[f32],
    noise: &mut [f32],
) {
    let mut nrg_ratio = 1.0f32;
    let mut noise_uv = [0.0f32; SMPL_MAX_SF_LEN];
    let mut noise_v = [0.0f32; SMPL_MAX_SF_LEN];
    let mut noise_v2 = [0.0f32; SMPL_MAX_SF_LEN];
    let mut env = [0.0f32; SMPL_MAX_SF_LEN];

    if voiced {
        let mut corrs = [0.0f32; SMPL_NOISE_CORR_ORDER + 1];
        let mut c = [0.0f32; SMPL_NOISE_CORR_ORDER + 1];
        let mut ctgt = [0.0f32; SMPL_NOISE_CORR_ORDER + 1];
        for i in 0..SMPL_NOISE_CORR_ORDER + 1 {
            let mut acc = 0.0f32;
            for k in 0..l - i {
                acc += exc_lpc[k] * exc_lpc[k + i];
            }
            corrs[i] = acc;
        }
        corrs[0] += 1e-12;
        let corr_smth_coef = if l == SMPL_CELP_FS_KHZ * 10 {
            0.4
        } else {
            0.16
        };
        for i in 0..SMPL_NOISE_CORR_ORDER + 1 {
            ng.corr_smth[i] += corr_smth_coef * (corrs[i] - ng.corr_smth[i]);
        }
        let scale =
            SMPL_DEC_NOISE_V_NOISE_GAIN * SMPL_DEC_NOISE_V_NOISE_GAIN * corrs[0] / ng.corr_smth[0];
        for i in 0..SMPL_NOISE_CORR_ORDER + 1 {
            c[i] = ng.corr_smth[i] * scale;
        }
        c[1] *= 2.0;
        c[2] *= 2.0;

        let dct = noise_dct();
        let mut f2 = [0.0f32; SMPL_NOISE_DCT_ORDER];
        let mut f2_tgt = [0.0f32; SMPL_NOISE_DCT_ORDER];
        matrix_mult_transp_16(dct, &c, &mut f2, SMPL_NOISE_CORR_ORDER + 1);
        let m = smpl_maximum(&f2[..SMPL_NOISE_DCT_ORDER]) * 1.5;
        for i in 0..SMPL_NOISE_DCT_ORDER {
            f2_tgt[i] = m - f2[i];
        }
        matrix_mult(dct, &f2_tgt, &mut ctgt);
        smpl_gen_rand_pulses(&mut noise_v, l, &mut ng.rand_seed);
        if !ng.prev_voiced {
            ng.env_smth = ng.env_last;
        }
        smpl_get_env(exc_lpc, l, SMPL_ENV_SMTH_COEF_V, &mut ng.env_smth, &mut env);
        for i in 0..l {
            noise_v[i] *= env[i];
        }
        let nrg_noise = smpl_nrg(&noise_v[..l]);
        let inv = 1.0 / (nrg_noise + 1e-12);
        for i in 0..SMPL_NOISE_CORR_ORDER + 1 {
            ctgt[i] *= inv;
        }
        let mut coef_ma = [0.0f32; SMPL_NOISE_CORR_ORDER + 1];
        smpl_spec_fact2(&ctgt, &mut coef_ma);
        smpl_filt_ma2(&noise_v, l, &coef_ma, &mut ng.shape_state, &mut noise_v2);

        if !ng.prev_voiced {
            smpl_gen_rand_pulses(&mut noise_uv, l, &mut ng.rand_seed);
            let mut env_val = ng.env_last * SMPL_ENV_SMTH_COEF_UV_V;
            let mut i = 0;
            while i < l {
                noise_uv[i] *= env_val;
                noise_uv[i + 1] *= env_val * SMPL_ENV_SMTH_COEF_UV_V;
                env_val *= SMPL_ENV_SMTH_COEF_UV_V * SMPL_ENV_SMTH_COEF_UV_V;
                i += 2;
            }
        } else if ng.since_unvoiced < 2 {
            for v in noise_uv.iter_mut().take(l) {
                *v = 0.0;
            }
        }
        ng.env_last = env[l - 1];
    } else {
        for v in ng.corr_smth.iter_mut() {
            *v = 0.0;
        }
        for v in ng.shape_state.iter_mut() {
            *v = 0.0;
        }
        for v in noise_v2.iter_mut().take(l) {
            *v = 0.0;
        }

        let nrg_tgt;
        if num_pulses > 0 {
            nrg_ratio = smpl_nrg(&exc_lpc[..l]) / (nrgres + 1e-20);
            let hardness = 10.0 + 20.0 * normalized_bitrate;
            nrg_tgt = nrgres * ((hardness * (1.0 - nrg_ratio)).exp() + 1.0).ln() / hardness;
            smpl_get_env(
                exc_lpc,
                l,
                SMPL_ENV_SMTH_COEF_UV,
                &mut ng.env_smth,
                &mut env,
            );
        } else {
            nrg_ratio = 0.0;
            nrg_tgt = nrgres;
            smpl_get_env0(l, SMPL_ENV_SMTH_COEF_UV, &mut ng.env_smth, &mut env);
        }

        let mut scale = 1.0 / l as f32;
        let nrg_tgt = nrg_tgt * scale + 1e-30;
        let nrg_env = smpl_nrg(&env[..l]) * scale;
        let mut f = nrg_tgt.sqrt();
        let mut g = (nrg_tgt / nrg_env).sqrt();
        let ge = g * env[0];
        let env_last = ng.env_last;
        if env_last < f.min(ge) {
            if f < ge {
                g = 0.0;
            } else {
                f = 0.0;
            }
        } else if env_last > f.max(ge) {
            if f > ge {
                g = 0.0;
            } else {
                f = 0.0;
            }
        } else {
            let sum_env = smpl_sum(&env[..l]) * scale;
            let a = nrg_env + env[0] * env[0] - 2.0 * sum_env * env[0];
            let b = 2.0 * env_last * (sum_env - env[0]);
            let cc = env_last * env_last - nrg_tgt;
            let mut tmp = b * b - 4.0 * a * cc;
            if tmp < 1e-35 || a < 1e-25 {
                f = 0.0;
                g = 0.0;
            } else {
                tmp = tmp.sqrt();
                scale = 0.5 / a;
                g = (-b + tmp) * scale;
                f = env_last - env[0] * g;
                if f < 0.0 {
                    g = (-b - tmp) * scale;
                    f = env_last - env[0] * g;
                }
            }
        }

        smpl_gen_rand_pulses(&mut noise_uv, l, &mut ng.rand_seed);
        if num_pulses > 0 {
            let max_val = fcbgains_uv[fcbg_idx as usize] * 0.5;
            for i in 0..l {
                if exc_lpc[i] == 0.0 {
                    noise_uv[i] *= (f + g * env[i]).min(max_val);
                } else {
                    noise_uv[i] = 0.0;
                }
            }
            ng.env_last = (f + g * env[l - 1]).min(max_val);
        } else {
            for i in 0..l {
                noise_uv[i] *= f + g * env[i];
            }
            ng.env_last = f + g * env[l - 1];
        }
    }

    if ng.prev_voiced || voiced {
        smpl_filt_ma2(&noise_v2, l, &COEF_MA_V, &mut ng.out_state_v, noise);
    } else {
        for v in noise.iter_mut().take(l) {
            *v = 0.0;
        }
    }
    if ng.since_unvoiced < 2 || !voiced {
        add_noise_uv(ng, &mut noise_uv, l, lsf, nrg_ratio, noise);
    } else {
        ng.out_state_uv = [0.0, 0.0];
    }
    ng.prev_voiced = voiced;
    if voiced {
        ng.since_unvoiced += 1;
    } else {
        ng.since_unvoiced = 0;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::Value;

    fn f32v(v: &Value) -> Vec<f32> {
        v.as_array()
            .unwrap()
            .iter()
            .map(|x| x.as_f64().unwrap() as f32)
            .collect()
    }

    fn ng_from(v: &Value) -> NoiseGenerator {
        let g = |k: &str| v[k].as_f64().unwrap() as f32;
        let arr2 = |k: &str| {
            let a = f32v(&v[k]);
            [a[0], a[1]]
        };
        let arr3 = |k: &str| {
            let a = f32v(&v[k]);
            [a[0], a[1], a[2]]
        };
        NoiseGenerator {
            env_smth: g("env_smth"),
            env_last: g("env_last"),
            out_state_uv: arr2("out_state_uv"),
            out_state_v: arr2("out_state_v"),
            corr_smth: arr3("corr_smth"),
            shape_state: arr2("shape_state"),
            prev_voiced: v["prev_voiced"].as_i64().unwrap() != 0,
            since_unvoiced: v["since_unvoiced"].as_i64().unwrap() as i32,
            rand_seed: v["rand_seed"].as_i64().unwrap() as i32,
        }
    }

    /// T2: gen_noise is PRNG- and state-driven, so the only deterministic validation is
    /// bit-exactness against the instrumented-C vectors. Each vector carries the full input
    /// `NoiseGenerator` state, the excitation, params, lsf, and the expected `noise[80]` plus the
    /// output `NoiseGenerator` state (`ng_out`). Seeding the generator with the captured `ng_in` and
    /// running our port must reproduce both bit-for-bit across voiced and unvoiced paths.
    #[test]
    fn gen_noise_matches_c() {
        // fcbgains_uv[ix] = 10^(0.05*(ix-90)), ix in 0..=90.
        let fcbgains_uv: Vec<f32> = (0..=90)
            .map(|ix| 10.0f32.powf(0.05 * (ix as f32 - 90.0)))
            .collect();

        let recs: Value =
            serde_json::from_str(include_str!("testdata/gennoise_vectors.json")).unwrap();
        let arr = recs.as_array().unwrap();
        assert!(!arr.is_empty());

        let (mut v_checked, mut uv0_checked, mut uvp_checked) = (0, 0, 0);
        for rec in arr {
            let voiced = rec["voiced"].as_i64().unwrap() == 1;
            let exc = f32v(&rec["exc_pre"]);
            let lsf = f32v(&rec["lsf"]);
            let noise_expected = f32v(&rec["noise"]);
            let nrgres = rec["nrgres"].as_f64().unwrap() as f32;
            let fcbg = rec["fcbg_idx"].as_i64().unwrap() as i32;
            let np = rec["sf_pulses"].as_i64().unwrap() as i32;
            let nbr = rec["norm_br"].as_f64().unwrap() as f32;
            let seed_out = rec["seed_out"].as_i64().unwrap() as i32;

            let mut ng = ng_from(&rec["ng_in"]);
            let ng_out_expected = ng_from(&rec["ng_out"]);
            let mut noise = [0.0f32; SMPL_MAX_SF_LEN];
            smpl_celp_gen_noise(
                &mut ng,
                &exc,
                80,
                voiced,
                np,
                nrgres,
                fcbg,
                &lsf,
                nbr,
                &fcbgains_uv,
                &mut noise,
            );

            // Seed transition is bit-exact (proves the PRNG + gen_rand_pulses call count).
            assert_eq!(
                ng.rand_seed, seed_out,
                "seed_out mismatch (voiced={voiced} np={np})"
            );
            assert_eq!(ng.rand_seed, ng_out_expected.rand_seed, "ng_out.rand_seed");

            // noise[80] bit-exact (tiny tolerance for f32 transcendental last-ULP across libm).
            for i in 0..80 {
                assert!(
                    (noise[i] - noise_expected[i]).abs() < 1e-6,
                    "noise[{i}] mismatch (voiced={voiced} np={np}): rust={} c={}",
                    noise[i],
                    noise_expected[i]
                );
            }
            // Output generator state bit-exact (env_last / out states drive the next subframe).
            assert!(
                (ng.env_last - ng_out_expected.env_last).abs() < 1e-6,
                "ng_out.env_last (voiced={voiced} np={np}): rust={} c={}",
                ng.env_last,
                ng_out_expected.env_last
            );
            for k in 0..2 {
                assert!(
                    (ng.out_state_uv[k] - ng_out_expected.out_state_uv[k]).abs() < 1e-6,
                    "ng_out.out_state_uv[{k}]"
                );
                assert!(
                    (ng.out_state_v[k] - ng_out_expected.out_state_v[k]).abs() < 1e-6,
                    "ng_out.out_state_v[{k}]"
                );
            }

            if voiced {
                v_checked += 1;
            } else if np == 0 {
                uv0_checked += 1;
            } else {
                uvp_checked += 1;
            }
        }
        assert!(
            v_checked > 0 && uv0_checked > 0 && uvp_checked > 0,
            "vectors must exercise all three paths: voiced={v_checked} uv0={uv0_checked} uvp={uvp_checked}"
        );
    }
}
```

`smpl_perc.rs`:

```rust
//! MLow (smpl_audio_codec) perceptual-weighting front-end — faithful transcription of Meta's
//! `smpl_perc_wght.c` (FFT-based perceptual autocorrelation -> perceptual LPC response) and
//! `smpl_bitrate_controller.c` (per-subframe pulse budget + importance), with the leaf helpers
//! they pull from `smpl_lpc.c` / `smpl_lpc_util.c` / `smpl_codec_util.c` / `smpl_filt.c`.
//!
//! The C uses pffft (PFFFT_REAL, ordered). pffft is not portable to wasm32/esp32 without a C dep,
//! so we run a self-contained mixed-radix complex FFT and re-pack the spectrum into pffft's exact
//! ordered real layout (`f[0]`=DC, `f[1]`=Nyquist, then interleaved `[re,im]` for bins 1..N/2-1).
//! `smth_filt` and the inverse therefore index identical bins and the returned autocorrelation `R[]`
//! matches the C math bit-for-bit in structure (only float rounding of the FFT differs). PERCW_NFFT
//! is 576 = 2^6 * 3^2, NOT a power of two, hence the mixed-radix (radix-2/3 + naive base) DFT.
//!
//! Self-contained on purpose: small leaf helpers (vector ops, window generation, ac2rc/rc2a) are
//! duplicated locally rather than shared with `smpl_celp.rs`, so this front-end can be wired in
//! separately without coupling.
#![allow(dead_code)]
#![allow(clippy::needless_range_loop)]
#![allow(clippy::too_many_arguments)]
#![allow(clippy::excessive_precision)]
// SMPL_PI is the verbatim C literal; keep it, do not swap for std PI.
#![allow(clippy::approx_constant)]
// C clamps as min(max(x, lo), hi); the equivalent `.max().min()` form mirrors the source order.
#![allow(clippy::manual_clamp)]
// The `% k == 0`, `(k+1)/2`, `>= x+1`, and range comparisons below are verbatim C; keep them so the
// transcription stays diffable against the source rather than rewritten into idiomatic Rust.
#![allow(clippy::manual_is_multiple_of)]
#![allow(clippy::manual_div_ceil)]
#![allow(clippy::int_plus_one)]
#![allow(clippy::manual_range_contains)]

// ---------------------------------------------------------------------------
// Constants (smpl_perc_wght.h / smpl_defines.h)
// ---------------------------------------------------------------------------

const SMPL_PI: f32 = 3.1415926535897;

const PERCW_NFFT: usize = 512 + 64; // 576
const PERCW_FS_KHZ: f32 = 16.0;

const SMPL_PERC_MASK_SMTH: f32 = 0.1158;
const SMPL_PERC_MEL_FC_HZ: f32 = 320.0;

const SMPL_WINNEXT_WB_LEN: usize = 16 * 2; // 32
const SMPL_WINNEXT_WB_LONG_LEN: usize = 16 * 4; // 64
const SMPL_WIN3_SHORT_LEN: usize = SMPL_WINNEXT_WB_LEN; // 32
const SMPL_WIN3_LONG_LEN: usize = SMPL_WINNEXT_WB_LONG_LEN; // 64
const SMPL_WINPREV_PERC_LEN: usize = 16 * 12; // 192
const SMPL_PERC_WIN1_10MS_LEN: usize = 192;
const SMPL_PERC_WIN1_20MS_LEN: usize = 352;

const SMPL_MAX_L_RESP: usize = 32 + 1; // 33
const SMPL_MAX_SF_LEN: usize = 16 * 10; // 160
const SMPL_PERC_RESP_LEN: usize = 16 * 2; // 32
pub(crate) const SMPL_PERC_REG: f32 = 1e-3;

const SMPL_TRUE: i32 = 1;
const SMPL_FALSE: i32 = 0;

// Bitrate-controller constants (smpl_defines.h / smpl_tables.c)
const SMPL_CELP_IDX_FEC: usize = 0;
const SMPL_CELP_IDX_MAIN: usize = 1;
const SMPL_CELP_MAX_RATES: usize = SMPL_CELP_IDX_MAIN + 1; // 2
const SMPL_MAX_PULSES_PER_SF: i32 = 40;
const SMPL_RATE_CONT_SCALE: f32 = 26.0;
const SMPL_E: f32 = 2.7182818284590;

// SmplFrameTypes (smpl_defines.h)
const BACKGROUND_NOISE: usize = 0;
const UNVOICED: usize = 1;
const VOICED: usize = 2;

// [lowRate][BACKGROUND_NOISE/UNVOICED/VOICED]
const SMPL_MAX_PULSES_PER_FRAME: [[u8; 3]; 2] = [[80, 160, 160], [16, 32, 32]];

// [framelenidx][lowrate][8]
const SMPL_RATE_CONTROL_MODEL_COMP5: [[[f32; 8]; 2]; 4] = [
    [
        [
            5.166876656946171,
            -8.981699804753452,
            0.07280811614105594,
            0.1301196310618402,
            -0.01597680442864421,
            1.7601470147884113,
            -3.8161195433141755,
            0.3038629198331684,
        ],
        [
            -71.71229978402292,
            14.197572549553076,
            -0.9863630205846172,
            0.032124893286072924,
            -0.0003538411576874928,
            1.803705259861388e-11,
            10.0,
            1.2454667523627154,
        ],
    ],
    [
        [
            32.5371190670542,
            -41.270234279452104,
            10.490270829170875,
            -1.102121269442237,
            0.03848319274046071,
            3.405326741403831,
            -5.102658181889428,
            0.2141935195026695,
        ],
        [
            -177.10486363500775,
            43.952329593498376,
            -3.7049735533247454,
            0.14239771116996938,
            -0.001919963993993193,
            7.953695588409639e-6,
            5.220317075476664,
            0.6435364076926223,
        ],
    ],
    [
        [
            -79.2663194911617,
            45.00981883522089,
            -10.063311543498518,
            1.2311531056576501,
            -0.06023559069137118,
            0.059204788212259364,
            3.033961466462233,
            1.0111383197827808,
        ],
        [
            -122.04861900525415,
            31.62096398905459,
            -2.613237037423586,
            0.10050433143234094,
            -0.0013233009240188039,
            2.14859438836692e-7,
            1.9077791307787761,
            0.7059420500333776,
        ],
    ],
    [
        [
            -182.64255084224325,
            122.90780796179816,
            -31.308790671748525,
            3.7850563849431462,
            -0.1750480676903051,
            0.05399618467364628,
            3.009451055091342,
            1.1243365512229038,
        ],
        [
            -132.4565456943888,
            34.361297004632966,
            -2.7956546289118887,
            0.10428149547078584,
            -0.001322667891395693,
            2.678747426340249e-6,
            6.9940208056381925,
            0.7551244069345737,
        ],
    ],
];

// [framelenidx][lowrate]
const SMPL_RATE_CONTROL_THRS_COMP5: [[u16; 2]; 4] =
    [[7500, 10000], [4500, 5750], [4000, 5000], [4000, 4750]];

// smpl_perc_emph_v/uv (smpl_tables.c) — exposed for the caller selecting voiced vs unvoiced emphasis.
pub(crate) const SMPL_PERC_EMPH_V: [f32; 2] = [-0.72, -0.77];
pub(crate) const SMPL_PERC_EMPH_UV: [f32; 2] = [-0.55, -0.6];

// ---------------------------------------------------------------------------
// Leaf vector helpers (smpl_codec_util.c) — duplicated locally.
// ---------------------------------------------------------------------------

#[inline]
fn smpl_nrg(x: &[f32]) -> f32 {
    let mut nrg = 0.0f32;
    for &v in x {
        nrg += v * v;
    }
    nrg
}

// x = y * win (smpl_mul_vec; C signature is (in, win, out) computing out = in .* win)
#[inline]
fn smpl_mul_vec(input: &[f32], win: &[f32], out: &mut [f32], l: usize) {
    for i in 0..l {
        out[i] = win[i] * input[i];
    }
}

// y = x * g (smpl_scale_vec)
#[inline]
fn smpl_scale_vec(x: &[f32], y: &mut [f32], l: usize, g: f32) {
    for i in 0..l {
        y[i] = x[i] * g;
    }
}

// y = x0 + g * x1 (smpl_add_scale_vec)
#[inline]
fn smpl_add_scale_vec(x0: &[f32], x1: &[f32], y: &mut [f32], l: usize, g: f32) {
    for i in 0..l {
        y[i] = x0[i] + g * x1[i];
    }
}

// y += g * x (smpl_add_scale_vec_inplace)
#[inline]
fn smpl_add_scale_vec_inplace(x: &[f32], y: &mut [f32], l: usize, g: f32) {
    for i in 0..l {
        y[i] += g * x[i];
    }
}

// smpl_sigmoid (smpl_codec_util.h)
#[inline]
fn smpl_sigmoid(x: f32) -> f32 {
    if x > 80.0 {
        return 1.0;
    }
    if x < -80.0 {
        return 0.0;
    }
    1.0 / (1.0 + (-x).exp())
}

// smpl_filt_ma2 (smpl_filt.c): 2nd-order MA, may be non-monic. State sits in `state[0..2]`.
fn smpl_filt_ma2(x: &[f32], n: usize, coef: &[f32], state: &[f32; 2], y: &mut [f32]) {
    debug_assert!(coef.len() == 3);
    debug_assert!(n > 1);

    if coef[0] == 1.0 {
        // monic
        smpl_add_scale_vec(&x[1..], x, &mut y[1..], n - 1, coef[1]);
    } else {
        smpl_scale_vec(x, y, n, coef[0]);
        smpl_add_scale_vec_inplace(x, &mut y[1..], n - 1, coef[1]);
    }
    smpl_add_scale_vec_inplace(x, &mut y[2..], n - 2, coef[2]);
    y[0] = coef[0] * x[0] + coef[1] * state[0] + coef[2] * state[1];
    y[1] += coef[2] * state[0];
    // state mutation in the C is dead for our single call (state is a stack temp), so we drop it.
}

// smpl_ac2rc_dbl (smpl_lpc_util.c): autocorrelation -> reflection coeffs, Levinson, double precision.
fn smpl_ac2rc_dbl(corr: &[f64], order: usize, reg: f64, rc: &mut [f32]) {
    debug_assert!(order > 0);
    debug_assert!(order - 1 <= SMPL_MAX_SF_LEN);
    let mut c0 = vec![0.0f64; order + 1];
    let mut c1 = vec![0.0f64; order + 1];
    c0[..(order + 1)].copy_from_slice(&corr[..(order + 1)]);
    c0[0] *= 1.0f64 + reg;
    c1.copy_from_slice(&c0);
    for r in rc[..order].iter_mut() {
        *r = 0.0;
    }
    for k in 0..order {
        if c0[k + 1] > c1[0] {
            rc[k] = -1.0;
            break;
        }
        if c0[k + 1] < -c1[0] {
            rc[k] = 1.0;
            break;
        }
        if c1[0] == 0.0 {
            break;
        }
        let rc_tmp = -c0[k + 1] / c1[0];
        rc[k] = rc_tmp as f32;
        for n in 0..(order - k) {
            let ctmp1 = c0[n + k + 1];
            let ctmp2 = c1[n];
            c0[n + k + 1] = ctmp1 + ctmp2 * rc_tmp;
            c1[n] = ctmp2 + ctmp1 * rc_tmp;
        }
    }
}

// smpl_ac2rc (smpl_lpc_util.c): float wrapper that promotes to double precision before Levinson.
fn smpl_ac2rc(corr: &[f32], order: usize, reg: f32, rc: &mut [f32]) {
    debug_assert!(order > 0);
    debug_assert!(order - 1 <= SMPL_MAX_SF_LEN);
    let mut corr_dbl = vec![0.0f64; order + 1];
    for i in 0..(order + 1) {
        corr_dbl[i] = corr[i] as f64;
    }
    smpl_ac2rc_dbl(&corr_dbl, order, reg as f64, rc);
}

// smpl_rc2a (smpl_lpc.c): reflection coeffs -> LPC polynomial A[0..order].
fn smpl_rc2a(rc: &[f32], order: usize, a: &mut [f32]) {
    for v in a[1..=order].iter_mut() {
        *v = 0.0;
    }
    a[0] = 1.0;
    for k in 0..order {
        let rc_tmp = rc[k];
        for n in 0..((k + 1) / 2) {
            let tmp1 = a[n + 1];
            let tmp2 = a[k - n];
            a[n + 1] = tmp1 + tmp2 * rc_tmp;
            a[k - n] = tmp2 + tmp1 * rc_tmp;
        }
        a[k + 1] = rc_tmp;
    }
}

// ---------------------------------------------------------------------------
// Self-contained mixed-radix complex FFT (no external deps).
// ---------------------------------------------------------------------------

#[derive(Clone, Copy)]
struct Cpx {
    re: f32,
    im: f32,
}

impl Cpx {
    #[inline]
    fn zero() -> Self {
        Cpx { re: 0.0, im: 0.0 }
    }
    #[inline]
    fn add(self, o: Cpx) -> Cpx {
        Cpx {
            re: self.re + o.re,
            im: self.im + o.im,
        }
    }
    #[inline]
    fn mul(self, o: Cpx) -> Cpx {
        Cpx {
            re: self.re * o.re - self.im * o.im,
            im: self.re * o.im + self.im * o.re,
        }
    }
}

/// Smallest prime factor of `n` (>= 2). For our use n is always composite of 2 and 3.
fn smallest_factor(n: usize) -> usize {
    if n % 2 == 0 {
        return 2;
    }
    let mut p = 3;
    while p * p <= n {
        if n % p == 0 {
            return p;
        }
        p += 2;
    }
    n
}

/// Recursive mixed-radix Cooley-Tukey DFT. `sign` is -1 for forward, +1 for inverse (unnormalized).
/// `x[i]` are the n input samples at stride `stride` within the parent array; `out` is contiguous.
fn fft_rec(x: &[Cpx], stride: usize, n: usize, sign: f32, out: &mut [Cpx]) {
    if n == 1 {
        out[0] = x[0];
        return;
    }
    let p = smallest_factor(n);
    if p == n {
        // Prime (or n with no small factor): naive O(n^2) DFT base case.
        for k in 0..n {
            let mut acc = Cpx::zero();
            let ang_k = sign * 2.0 * SMPL_PI * (k as f32) / (n as f32);
            for j in 0..n {
                let ang = ang_k * (j as f32);
                let w = Cpx {
                    re: ang.cos(),
                    im: ang.sin(),
                };
                acc = acc.add(x[j * stride].mul(w));
            }
            out[k] = acc;
        }
        return;
    }
    let m = n / p;
    // Compute p sub-DFTs of length m over the decimated inputs (sub[q*m .. (q+1)*m]).
    let mut sub = vec![Cpx::zero(); n];
    for (q, dst) in sub.chunks_mut(m).enumerate() {
        fft_rec(&x[q * stride..], stride * p, m, sign, dst);
    }
    // Combine: out[k] = sum_q twiddle(q,k) * sub_q[k mod m], with the radix-p butterfly.
    for k in 0..n {
        let kmod = k % m;
        let mut acc = Cpx::zero();
        for q in 0..p {
            let ang = sign * 2.0 * SMPL_PI * (k as f32) * (q as f32) / (n as f32);
            let tw = Cpx {
                re: ang.cos(),
                im: ang.sin(),
            };
            acc = acc.add(sub[q * m + kmod].mul(tw));
        }
        out[k] = acc;
    }
}

/// Complex FFT of a power-/mixed-radix length into `out`. `sign=-1` forward, `+1` inverse.
fn cfft(input: &[Cpx], out: &mut [Cpx], sign: f32) {
    let n = input.len();
    debug_assert!(out.len() == n);
    fft_rec(input, 1, n, sign, out);
}

/// Forward real FFT of `n` real samples, re-packed into pffft's ordered REAL layout:
/// f[0]=DC.re, f[1]=Nyquist.re, then [re,im] pairs for bins 1..n/2-1. Output length is n.
pub(crate) fn rfft_forward_ordered(time: &[f32], f: &mut [f32]) {
    let n = time.len();
    debug_assert!(f.len() == n);
    let mut cin = vec![Cpx::zero(); n];
    for i in 0..n {
        cin[i].re = time[i];
        cin[i].im = 0.0;
    }
    let mut spec = vec![Cpx::zero(); n];
    cfft(&cin, &mut spec, -1.0);
    f[0] = spec[0].re;
    f[1] = spec[n / 2].re;
    for i in 1..(n / 2) {
        f[2 * i] = spec[i].re;
        f[2 * i + 1] = spec[i].im;
    }
}

/// Inverse real FFT consuming pffft's ordered REAL layout (as produced/modified above) and
/// producing n real time samples, unnormalized (matches pffft: BACKWARD(FORWARD(x)) = n*x).
fn rfft_backward_ordered(f: &[f32], time: &mut [f32]) {
    let n = f.len();
    debug_assert!(time.len() == n);
    // Rebuild the full Hermitian complex spectrum from the ordered real layout.
    let mut spec = vec![Cpx::zero(); n];
    spec[0] = Cpx { re: f[0], im: 0.0 };
    spec[n / 2] = Cpx { re: f[1], im: 0.0 };
    for i in 1..(n / 2) {
        let re = f[2 * i];
        let im = f[2 * i + 1];
        spec[i] = Cpx { re, im };
        spec[n - i] = Cpx { re, im: -im }; // conjugate symmetry
    }
    let mut tout = vec![Cpx::zero(); n];
    cfft(&spec, &mut tout, 1.0);
    for i in 0..n {
        time[i] = tout[i].re;
    }
}

// ---------------------------------------------------------------------------
// Perceptual model (smpl_perc_wght.c)
// ---------------------------------------------------------------------------

/// Window tables built once (mirrors smpl_create_lpc_windows for the perc-relevant windows).
struct PercWindows {
    perc_win1_10ms: Vec<f32>, // gen_sin_win, len 192
    perc_win1_20ms: Vec<f32>, // gen_sin_win, len 352
    win3_short: Vec<f32>,     // gen_cos_win, len 32
    win3_long: Vec<f32>,      // gen_cos_win, len 64
}

// gen_sin_win (smpl_lpc.c): win[i] = sin((i+1)/(N+1) * pi/2)
fn gen_sin_win(n: usize) -> Vec<f32> {
    let mut w = vec![0.0f32; n];
    for i in 0..n {
        w[i] = ((i as f32 + 1.0) / (n as f32 + 1.0) * SMPL_PI / 2.0).sin();
    }
    w
}

// gen_cos_win (smpl_lpc.c): win[i] = cos((i+1)/(N+1) * pi/2)
fn gen_cos_win(n: usize) -> Vec<f32> {
    let mut w = vec![0.0f32; n];
    for i in 0..n {
        w[i] = ((i as f32 + 1.0) / (n as f32 + 1.0) * SMPL_PI / 2.0).cos();
    }
    w
}

impl PercWindows {
    fn new() -> Self {
        PercWindows {
            perc_win1_10ms: gen_sin_win(SMPL_PERC_WIN1_10MS_LEN),
            perc_win1_20ms: gen_sin_win(SMPL_PERC_WIN1_20MS_LEN),
            win3_short: gen_cos_win(SMPL_WIN3_SHORT_LEN),
            win3_long: gen_cos_win(SMPL_WIN3_LONG_LEN),
        }
    }
}

// smpl_window (smpl_lpc.c) for the perc case (use_lpc_win == FALSE).
fn smpl_window_perc(
    win: &PercWindows,
    input: &[f32],
    out: &mut [f32],
    len: usize,
    frame_ms: i32,
    use_long_win: bool,
) {
    let (win1len, win1): (usize, &[f32]) = if frame_ms == 10 {
        (SMPL_PERC_WIN1_10MS_LEN, &win.perc_win1_10ms)
    } else {
        (SMPL_PERC_WIN1_20MS_LEN, &win.perc_win1_20ms)
    };
    let (win3len, win3): (usize, &[f32]) = if use_long_win {
        (SMPL_WIN3_LONG_LEN, &win.win3_long)
    } else {
        (SMPL_WIN3_SHORT_LEN, &win.win3_short)
    };

    smpl_mul_vec(input, win1, out, win1len);
    let mid = len - win1len - SMPL_WIN3_LONG_LEN;
    out[win1len..win1len + mid].copy_from_slice(&input[win1len..win1len + mid]);
    smpl_mul_vec(
        &input[len - SMPL_WIN3_LONG_LEN..],
        win3,
        &mut out[len - SMPL_WIN3_LONG_LEN..],
        win3len,
    );
    if !use_long_win {
        let start = len - SMPL_WIN3_LONG_LEN + SMPL_WIN3_SHORT_LEN;
        for v in out[start..len].iter_mut() {
            *v = 0.0;
        }
    }
}

// smth_filt (smpl_perc_wght.c): bidirectional masking smooth across the power spectrum.
fn smth_filt(f: &mut [f32], smthcoef: &[f32]) {
    let half = PERCW_NFFT / 2;
    let mut f2smth = f[0];
    for i in 1..half {
        let f2new = f[2 * i];
        f2smth = f2new + smthcoef[i] * (f2smth - f2new);
        f[2 * i] = f2smth;
    }
    f[1] = f[1] + smthcoef[half] * (f2smth - f[1]);
    f2smth = f[1];
    let mut i = half - 1;
    while i > 0 {
        let f2new = f[2 * i];
        f2smth = f2new + smthcoef[i] * (f2smth - f2new);
        f[2 * i] = f2smth;
        i -= 1;
    }
    f[0] = f[0] + smthcoef[0] * (f2smth - f[0]);
}

/// State carried across `smpl_perc_model` calls (the `buf` history of length PERCW_NFFT).
pub(crate) struct PercModelState {
    buf: [f32; PERCW_NFFT],
    smthcoef: Vec<f32>, // length PERCW_NFFT/2 + 1
    windows: PercWindows,
}

impl PercModelState {
    pub(crate) fn new() -> Self {
        // smpl_create_perc_model_tables: per-bin mel-width smoothing coefficients.
        let fs_step = (PERCW_FS_KHZ * 1000.0) / PERCW_NFFT as f32;
        let mut smthcoef = vec![0.0f32; PERCW_NFFT / 2 + 1];
        for i in 0..(PERCW_NFFT / 2 + 1) {
            let perc_width_per_bin =
                SMPL_PERC_MASK_SMTH * (fs_step * i as f32 + SMPL_PERC_MEL_FC_HZ) / fs_step;
            smthcoef[i] = perc_width_per_bin / (perc_width_per_bin + 1.0);
        }
        PercModelState {
            buf: [0.0; PERCW_NFFT],
            smthcoef,
            windows: PercWindows::new(),
        }
    }
}

/// smpl_perc_model: windowed power spectrum -> bidirectional masking smooth -> inverse -> 1/NFFT scale.
/// Returns the first `len_r` autocorrelation lags. `buf` history advances exactly as the C memmove/copy.
pub(crate) fn smpl_perc_model(
    state: &mut PercModelState,
    xsubfr: &[f32],
    xsubfr_len: usize,
    frame_ms: i32,
    is_last_subfr: i32,
    len_r: usize,
) -> Vec<f32> {
    debug_assert!(xsubfr_len <= PERCW_NFFT);

    // Advance history: drop the consumed front, keep the (NFFT - xsubfr_len) tail, then append xsubfr.
    // Source offset is xsubfr_len - (WB_LONG_LEN - WB_LEN) = xsubfr_len - 32.
    let src_off = xsubfr_len - (SMPL_WINNEXT_WB_LONG_LEN - SMPL_WINNEXT_WB_LEN);
    let keep = PERCW_NFFT - xsubfr_len;
    state.buf.copy_within(src_off..src_off + keep, 0);
    state.buf[keep..keep + xsubfr_len].copy_from_slice(&xsubfr[..xsubfr_len]);

    let winlen = SMPL_WINPREV_PERC_LEN + (frame_ms as usize) * 16 + SMPL_WIN3_LONG_LEN;
    let skip_samples = PERCW_NFFT - winlen;
    debug_assert!(frame_ms == 10 || skip_samples == 0);

    let mut buf_win = vec![0.0f32; PERCW_NFFT];
    // memset(buf_win, 0, skip_samples) — already zero from vec init.
    smpl_window_perc(
        &state.windows,
        &state.buf[skip_samples..],
        &mut buf_win[skip_samples..],
        winlen,
        frame_ms,
        is_last_subfr == SMPL_FALSE,
    );

    let mut f = vec![0.0f32; PERCW_NFFT];
    rfft_forward_ordered(&buf_win, &mut f);
    f[0] = f[0] * f[0]; // DC
    f[1] = f[1] * f[1]; // Nyquist
    for i in 1..(PERCW_NFFT / 2) {
        f[2 * i] = f[2 * i] * f[2 * i] + f[2 * i + 1] * f[2 * i + 1];
        f[2 * i + 1] = 0.0;
    }
    smth_filt(&mut f, &state.smthcoef);
    rfft_backward_ordered(&f, &mut buf_win);

    let mut r = vec![0.0f32; len_r];
    smpl_scale_vec(&buf_win, &mut r, len_r, 1.0 / PERCW_NFFT as f32);
    r
}

/// smpl_perc_ac2a: ma2 (b={pe, 1+pe^2, pe}) on R[1..] then Levinson + rc2a -> A[0..perc_resp_len].
pub(crate) fn smpl_perc_ac2a(
    r: &[f32],
    len_r: usize,
    perc_emph: f32,
    perc_resp_len: usize,
    reg: f32,
) -> Vec<f32> {
    debug_assert!(len_r >= perc_resp_len + 1);
    debug_assert!(SMPL_MAX_L_RESP >= perc_resp_len);

    let b = [perc_emph, 1.0 + perc_emph * perc_emph, perc_emph];
    let state = [r[0], r[1]];
    let mut r_ = vec![0.0f32; SMPL_MAX_L_RESP];
    smpl_filt_ma2(&r[1..], perc_resp_len, &b, &state, &mut r_);

    let mut rc = vec![0.0f32; SMPL_MAX_L_RESP];
    smpl_ac2rc(&r_, perc_resp_len - 1, reg, &mut rc);

    let mut a = vec![0.0f32; perc_resp_len];
    smpl_rc2a(&rc, perc_resp_len - 1, &mut a);
    a
}

// ---------------------------------------------------------------------------
// Bitrate controller (smpl_bitrate_controller.c)
// ---------------------------------------------------------------------------

// bitrate2pulses (smpl_bitrate_controller.c)
fn bitrate2pulses(rate_kbps: f32, coeff: &[f32; 8]) -> f32 {
    coeff[0]
        + coeff[1] * rate_kbps
        + coeff[2] * rate_kbps * rate_kbps
        + coeff[3] * rate_kbps.powf(3.0)
        + coeff[4] * rate_kbps.powf(4.0)
        + coeff[5] * SMPL_E.powf((rate_kbps - coeff[6]) * coeff[7])
}

// bitrate2pulses_hr_fec (smpl_bitrate_controller.c)
fn bitrate2pulses_hr_fec(rate_kbps: f32, coeff: &[f32; 8], one_pulse_rate_bps: f32) -> f32 {
    const RATE_THRES_KBPS: f32 = 9.0; // only compensate below this value
    if rate_kbps >= RATE_THRES_KBPS {
        bitrate2pulses(rate_kbps, coeff)
    } else if one_pulse_rate_bps >= RATE_THRES_KBPS * 1000.0 {
        1.0
    } else {
        let pulses_thres = bitrate2pulses(RATE_THRES_KBPS, coeff);
        let sc = (RATE_THRES_KBPS - rate_kbps) / (RATE_THRES_KBPS - one_pulse_rate_bps / 1000.0);
        pulses_thres - sc * (pulses_thres - 1.0)
    }
}

/// Encoder-control inputs the controller reads (the `smpl_EncControlStruct` fields it touches).
#[derive(Clone, Copy)]
pub(crate) struct BitrateControllerInputs {
    pub internal_sample_rate: i32,
    pub payload_size_ms: i32,
    pub fec_bit_rate: i32,
    pub main_bit_rate: i32,
    pub complexity: i32,
    pub use_fec_rate_compensation: i32,
    pub use_dtx: i32,
    pub sub_frame_importance_factor: f32,
}

/// BitrateController state (smpl_structs.h) carried across frames.
pub(crate) struct BitrateController {
    prev_voiced: i32,
    rate_cont_wnrg_smth: f32,
    rate_cont_bitrate_scale: [f32; SMPL_CELP_MAX_RATES],
    bitrate_delta_smth: [f32; SMPL_CELP_MAX_RATES],
    rate_cont_bitrate: [f32; SMPL_CELP_MAX_RATES],
    adjustment_factor: [f32; SMPL_CELP_MAX_RATES],
}

impl BitrateController {
    /// bitrate_controller_init (smpl_bitrate_controller.c) plus zeroed state (struct was calloc'd).
    pub(crate) fn new() -> Self {
        BitrateController {
            prev_voiced: 0,
            rate_cont_wnrg_smth: 0.0,
            rate_cont_bitrate_scale: [0.0; SMPL_CELP_MAX_RATES],
            bitrate_delta_smth: [0.0; SMPL_CELP_MAX_RATES],
            rate_cont_bitrate: [0.0; SMPL_CELP_MAX_RATES],
            adjustment_factor: [1.0; SMPL_CELP_MAX_RATES],
        }
    }

    /// bitrate_controller (smpl_bitrate_controller.c). Returns (max_pulses_per_subfr, subfr_importance).
    pub(crate) fn control(
        &mut self,
        enc: &BitrateControllerInputs,
        dtx_sid_frame: i32,
        coded_as_active_voice: i32,
        sp_act_prob: f32,
        nonflatness: f32,
        voicing_strength: f32,
        voiced: i32,
        wnrg: f32,
        wnrg_next: f32,
        low_rate: i32,
        framelen: i32,
        subfrlen: i32,
    ) -> ([i16; SMPL_CELP_MAX_RATES], [f32; SMPL_CELP_MAX_RATES]) {
        debug_assert!(sp_act_prob >= 0.0 && sp_act_prob <= 1.0);

        let mut bwe_bitrate = 0i32;
        if enc.internal_sample_rate > 16000 {
            bwe_bitrate += if low_rate != 0 { 450 } else { 750 };
            bwe_bitrate += if enc.payload_size_ms == 10 { 450 } else { 0 };
        }

        self.rate_cont_wnrg_smth += 0.6 * (wnrg - self.rate_cont_wnrg_smth);

        let framelen_idx = if enc.payload_size_ms == 10 {
            0usize
        } else if enc.payload_size_ms == 20 {
            1
        } else if enc.payload_size_ms == 60 {
            2
        } else {
            3
        };

        let mut max_pulses_per_subfr = [0i16; SMPL_CELP_MAX_RATES];
        let mut subfr_importance = [0.0f32; SMPL_CELP_MAX_RATES];

        // C precedence: `SMPL_CELP_IDX_FEC + (a == 0) || (b == c)` parses as
        // `(SMPL_CELP_IDX_FEC + (a == 0)) || (b == c)` (a `||` whose value is 0 or 1).
        let start_r: usize = if ((SMPL_CELP_IDX_FEC as i32 + (enc.fec_bit_rate == 0) as i32) != 0)
            || (enc.fec_bit_rate == enc.main_bit_rate)
        {
            1
        } else {
            0
        };

        let lr_idx = if low_rate != 0 { 0usize } else { 1 };

        for r in start_r..=SMPL_CELP_IDX_MAIN {
            let mut bit_rate = if r == SMPL_CELP_IDX_FEC {
                enc.fec_bit_rate as f32
            } else {
                enc.main_bit_rate as f32
            };
            bit_rate = bit_rate.min(30000.0); // don't extrapolate pulses_per_20ms_target_max curves
            let mut rate_kbps = (bit_rate - bwe_bitrate as f32) / 1000.0;
            if low_rate == 0 {
                rate_kbps *= match enc.complexity {
                    1 => 0.9900990,
                    2 => 0.9900990,
                    3 => 1.0101010,
                    4 => 1.0101010,
                    _ => 1.0,
                };
            }

            let pulses_per_20ms_target_max;
            let rate_control_thrs = SMPL_RATE_CONTROL_THRS_COMP5[framelen_idx][lr_idx] as f32;
            if (bit_rate - bwe_bitrate as f32) < rate_control_thrs {
                pulses_per_20ms_target_max = 1.0;
            } else {
                let coeff = &SMPL_RATE_CONTROL_MODEL_COMP5[framelen_idx][lr_idx];
                if (r == SMPL_CELP_IDX_FEC) && low_rate == 0 && enc.use_fec_rate_compensation != 0 {
                    pulses_per_20ms_target_max =
                        bitrate2pulses_hr_fec(rate_kbps, coeff, rate_control_thrs).max(1.0);
                } else {
                    pulses_per_20ms_target_max = bitrate2pulses(rate_kbps, coeff).max(1.0);
                }
            }

            let rel_pulserate = pulses_per_20ms_target_max / 16.0 * (320.0 / framelen as f32);
            debug_assert!(rel_pulserate > 0.0);
            let rel_pulserate_log = rel_pulserate.ln();
            if self.rate_cont_bitrate[r] != bit_rate {
                let bitrate_scale = SMPL_RATE_CONT_SCALE
                    * rel_pulserate
                    * (1.0 + 0.4 * rel_pulserate_log * rel_pulserate_log);
                self.rate_cont_bitrate_scale[r] = bitrate_scale;
                self.rate_cont_bitrate[r] = bit_rate;
            }

            let numsubfrs = framelen / subfrlen;
            let mut mpps =
                1 + (pulses_per_20ms_target_max * (1.0 + 0.5) / numsubfrs as f32).round() as i32;
            if enc.use_dtx != 0 && dtx_sid_frame != 0 {
                mpps = 0;
            } else {
                mpps = (mpps as f32 * (0.5 + 0.5 * (sp_act_prob + 1e-12).sqrt())).round() as i32;
                let frame_type = if coded_as_active_voice == 0 {
                    BACKGROUND_NOISE
                } else if voiced == 1 {
                    VOICED
                } else {
                    UNVOICED
                };
                // smpl_max_pulses_per_frame is indexed by the raw low_rate flag (0=high-rate row),
                // unlike the model/thrs tables which use `low_rate ? 0 : 1`.
                let max_pulses = SMPL_MAX_PULSES_PER_FRAME[low_rate as usize][frame_type] as i32
                    * framelen
                    / 320;
                mpps = mpps.min(max_pulses / numsubfrs); // don't overshoot the PDF
            }
            debug_assert!(mpps <= SMPL_MAX_PULSES_PER_SF);
            max_pulses_per_subfr[r] = mpps as i16;

            let mut imp =
                (wnrg + 0.01 * wnrg_next) / (self.rate_cont_wnrg_smth + 0.02 * wnrg_next + 1e-12);
            if voiced != 0 {
                if bit_rate <= 9000.0 {
                    imp = (imp + 1e-12).sqrt();
                }
            } else {
                imp *= 0.9 + 0.3 * smpl_sigmoid(nonflatness - 2.0);
                imp *= 0.8;
            }
            if voiced != self.prev_voiced {
                imp *= 1.1;
            }
            imp *= 0.9 + 0.3 * 1.0 / (1.0 + 25.0 * voicing_strength * voicing_strength);

            // Speech-activity bitrate allocation shaping (see the C META comment block).
            let mut imp_factor = enc.sub_frame_importance_factor;
            if imp_factor <= 1.0 {
                imp *= (1.0 - imp_factor) + imp_factor * (sp_act_prob + 1e-12).sqrt();
            } else if imp_factor <= 2.0 {
                imp_factor -= 1.0;
                imp *= (1.0 - imp_factor) + imp_factor * sp_act_prob;
            } else {
                debug_assert!(imp_factor <= 3.0);
                imp_factor -= 2.0;
                imp *= (1.0 - imp_factor) + imp_factor * sp_act_prob * sp_act_prob;
            }
            imp *= self.adjustment_factor[r] * self.rate_cont_bitrate_scale[r];
            subfr_importance[r] = imp;
            self.prev_voiced = voiced;
        }

        (max_pulses_per_subfr, subfr_importance)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn percw_nfft_is_576_non_pow2() {
        assert_eq!(PERCW_NFFT, 576);
        assert_ne!(
            PERCW_NFFT & (PERCW_NFFT - 1),
            0,
            "576 is not a power of two"
        );
    }

    // FFT -> IFFT round-trips to n*x within float tolerance.
    #[test]
    fn fft_roundtrip() {
        let n = PERCW_NFFT;
        let mut x = vec![0.0f32; n];
        // deterministic pseudo-random-ish signal
        let mut s: u32 = 12345;
        for v in x.iter_mut() {
            s = s.wrapping_mul(196314165).wrapping_add(907633515);
            *v = ((s >> 9) as f32 / (1u32 << 23) as f32) - 1.0;
        }
        let mut f = vec![0.0f32; n];
        rfft_forward_ordered(&x, &mut f);
        let mut back = vec![0.0f32; n];
        rfft_backward_ordered(&f, &mut back);
        for i in 0..n {
            let expected = x[i] * n as f32;
            assert!(
                (back[i] - expected).abs() < 1e-1 * (1.0 + expected.abs()),
                "idx {i}: got {}, want {}",
                back[i],
                expected
            );
        }
    }

    // Known signal: a single real cosine at bin k has power N^2/4 at bin k (one-sided), 0 elsewhere.
    #[test]
    fn power_spectrum_of_cosine() {
        let n = PERCW_NFFT;
        let k = 9usize; // a bin reachable by both radix-2 and radix-3 paths
        let mut x = vec![0.0f32; n];
        for i in 0..n {
            x[i] = (2.0 * SMPL_PI * (k as f32) * (i as f32) / (n as f32)).cos();
        }
        let mut f = vec![0.0f32; n];
        rfft_forward_ordered(&x, &mut f);
        // power at bin k (interleaved [re,im] for 1..n/2-1)
        let pk = f[2 * k] * f[2 * k] + f[2 * k + 1] * f[2 * k + 1];
        let expected = (n as f32 / 2.0) * (n as f32 / 2.0);
        assert!(
            (pk - expected).abs() < 1e-2 * expected,
            "bin {k} power {pk}, want {expected}"
        );
        // a far-away bin should be ~0
        let m = 40usize;
        let pm = f[2 * m] * f[2 * m] + f[2 * m + 1] * f[2 * m + 1];
        assert!(pm < 1e-2 * expected, "bin {m} power {pm} not ~0");
    }

    // smpl_perc_model end-to-end smoke: with zero input it returns all-zero R; a DC step gives R[0]>0.
    #[test]
    fn perc_model_smoke() {
        let mut st = PercModelState::new();
        let xsubfr = vec![0.0f32; 320];
        let r = smpl_perc_model(&mut st, &xsubfr, 320, 20, SMPL_FALSE, SMPL_MAX_L_RESP);
        assert_eq!(r.len(), SMPL_MAX_L_RESP);
        for &v in &r {
            assert!(v.abs() < 1e-6, "zero input must give ~0 autocorr, got {v}");
        }

        let mut st2 = PercModelState::new();
        let dc = vec![1.0f32; 320];
        let r2 = smpl_perc_model(&mut st2, &dc, 320, 20, SMPL_FALSE, SMPL_MAX_L_RESP);
        assert!(
            r2[0] > 0.0,
            "DC input must give positive R[0], got {}",
            r2[0]
        );
        // ac2a must run without panicking and return A[0] == 1.0
        let a = smpl_perc_ac2a(
            &r2,
            SMPL_MAX_L_RESP,
            SMPL_PERC_EMPH_V[0],
            SMPL_PERC_RESP_LEN,
            SMPL_PERC_REG,
        );
        assert_eq!(a.len(), SMPL_PERC_RESP_LEN);
        assert_eq!(a[0], 1.0);
    }

    #[test]
    fn bitrate_controller_runs() {
        let mut bc = BitrateController::new();
        let enc = BitrateControllerInputs {
            internal_sample_rate: 16000,
            payload_size_ms: 20,
            fec_bit_rate: 0,
            main_bit_rate: 16000,
            complexity: 5,
            use_fec_rate_compensation: 0,
            use_dtx: 0,
            sub_frame_importance_factor: 1.0,
        };
        let (mp, si) = bc.control(&enc, 0, 1, 0.9, 1.5, 0.3, 1, 100.0, 80.0, 0, 320, 80);
        // FEC disabled (fec_bit_rate==0) so start_r==1: only MAIN index is populated.
        assert_eq!(mp[0], 0);
        assert!(mp[1] >= 1);
        assert!(si[1].is_finite());
    }

    // R4 budget: for the active MLow config (20 kbps, 60 ms payload, complexity 8, high-rate, unvoiced)
    // the per-subframe pulse budget must equal the C reference (`smpl_bitrate_controller.c` dumped 23
    // per subframe via the instrumented libopus). Regression for the inverted `smpl_max_pulses_per_frame`
    // index that capped unvoiced subframes at 8 (the `{16,32,32}` low-rate row) instead of 23.
    #[test]
    fn bitrate_controller_active_unvoiced_budget_matches_c() {
        let mut bc = BitrateController::new();
        let enc = BitrateControllerInputs {
            internal_sample_rate: 16000,
            payload_size_ms: 60,
            fec_bit_rate: 0,
            main_bit_rate: 20000,
            complexity: 8,
            use_fec_rate_compensation: 0,
            use_dtx: 0,
            sub_frame_importance_factor: 1.0,
        };
        // coded_as_active_voice=1, voiced=0, low_rate=0, framelen=320, subfrlen=80, sp_act_prob≈1.
        let (mp, _si) = bc.control(
            &enc, 0, 1, 0.9961, 0.2, -0.18, 0, 3.0e-5, 5.0e-5, 0, 320, 80,
        );
        assert_eq!(
            mp[1], 23,
            "active-unvoiced max_pulses must match the C dump (23/subframe)"
        );
    }
}
```

## Go envelope (signatures only)

The corresponding Go declarations — exported types and function **signatures with
no bodies**. This is the surface to implement; it is not the implementation.

```go
package mlow

// --- CELP noise generator (smpl_celp_util.c) ---

// Persistent decoder-side noise generator state.
type NoiseGenerator struct {
	EnvSmth      float32
	EnvLast      float32
	OutStateUV   [2]float32
	OutStateV    [2]float32
	CorrSmth     [3]float32 // NOISE_CORR_ORDER + 1
	ShapeState   [2]float32 // NOISE_CORR_ORDER
	PrevVoiced   bool
	SinceUnvoiced int32
	RandSeed     int32
}

func NewNoiseGenerator() *NoiseGenerator // zero-init

func SmplGetNormalizedBitrate(numPulses, frameLength16 int32) float32

func SmplDecodeResnrg(nrgresFrameDbqQ14, fcbSubfrlen int32) float32

// Builds the shaped residual noise for one subframe (writes l samples into noise).
func SmplCelpGenNoise(
	ng *NoiseGenerator,
	excLpc []float32,
	l int,
	voiced bool,
	numPulses int32,
	nrgres float32,
	fcbgIdx int32,
	lsf []float32,
	normalizedBitrate float32,
	fcbgainsUV []float32,
	noise []float32,
)

// --- perceptual-weighting front-end (smpl_perc_wght.c) ---

const SmplPercReg float32 = 1e-3

var (
	SmplPercEmphV  = [2]float32{-0.72, -0.77}
	SmplPercEmphUV = [2]float32{-0.55, -0.6}
)

// Forward real FFT (576-point mixed-radix) into pffft's ordered REAL layout.
func RfftForwardOrdered(time []float32, f []float32)

// State carried across SmplPercModel calls.
type PercModelState struct {
	// buf history (576), per-bin mel-width smoothing coefficients, window tables.
}

func NewPercModelState() *PercModelState

// Windowed power spectrum -> masking smooth -> inverse -> 1/NFFT scale; returns first lenR autocorr lags.
func SmplPercModel(state *PercModelState, xsubfr []float32, xsubfrLen int, frameMs, isLastSubfr int32, lenR int) []float32

// ma2 on R[1..] then Levinson + rc2a -> A[0..percRespLen].
func SmplPercAC2A(r []float32, lenR int, percEmph float32, percRespLen int, reg float32) []float32

// --- bitrate controller (smpl_bitrate_controller.c) ---

type BitrateControllerInputs struct {
	InternalSampleRate       int32
	PayloadSizeMs            int32
	FecBitRate              int32
	MainBitRate             int32
	Complexity              int32
	UseFecRateCompensation  int32
	UseDtx                  int32
	SubFrameImportanceFactor float32
}

type BitrateController struct {
	// prevVoiced, smoothed weighted-energy, per-rate bitrate scale / delta / bitrate / adjustment.
}

func NewBitrateController() *BitrateController

// Returns (maxPulsesPerSubfr, subfrImportance), both length SMPL_CELP_MAX_RATES (2).
func (bc *BitrateController) Control(
	enc *BitrateControllerInputs,
	dtxSidFrame, codedAsActiveVoice int32,
	spActProb, nonflatness, voicingStrength float32,
	voiced int32,
	wnrg, wnrgNext float32,
	lowRate, framelen, subfrlen int32,
) (maxPulsesPerSubfr [2]int16, subfrImportance [2]float32)
```

## Implementation suggestions (guidance, not authoritative)

- `usize` → Go `int`; `i32` → `int32`; `i16` → `int16`; `u8`/`u16` constants stay
  as their narrow types in the lookup tables. `f32` → `float32`, `f64` → `float64`.
- `smpl_rand` is a 32-bit LCG done in wrapping i32 with an unsigned multiply:
  `907633515 + int32(uint32(seed) * 196314165)`. Go `int32`/`uint32` arithmetic
  wraps, so reproduce the exact cast points. The 4-lane `smpl_gen_rand_pulses` reads
  `s<<8`, `s<<16`, `s<<24` from the `u32` seed then casts back through `i32` to
  `f32`: `float32(int32(uint32(s) << k))`. The validation asserts the seed
  transition exactly, so the call count and lane order are load-bearing.
- `smpl_get_env`/`get_env0`/`smpl_filt_*` are written 4-wide / partially unrolled to
  match the C accumulation order; keep that order. The `sqrt`/`exp`/`ln` in the
  noise path use real `math` functions on `f32` — call `float32(math.Sqrt(float64(x)))`
  etc. The vector tolerates `< 1e-6` last-ULP transcendental drift, but the
  arithmetic structure must match.
- The DCT and the window/smoothing tables are built at init (`OnceLock` in Rust) by
  recurrence/accumulation, not closed form — build them once in Go (e.g. `sync.Once`
  or package init) with the same accumulation (`omega += d_omega`) to stay
  bit-faithful.
- The perceptual model's FFT is a deliberately self-contained mixed-radix DFT for
  `N = 576 = 2^6 * 3^2` (NOT a power of two); it re-packs into pffft's ordered real
  layout (`f[0]`=DC, `f[1]`=Nyquist, then interleaved `[re,im]`). Port the recursive
  `fft_rec`/`cfft` as-is; the radix-2/3 split plus the naive prime base case is what
  makes 576 work. `TODO(human):` decide whether Go uses this same hand-rolled DFT or
  a vetted mixed-radix library that reproduces the same ordered-real packing.
- The bitrate controller carries a deliberate C operator-precedence quirk in
  `start_r`: `SMPL_CELP_IDX_FEC + (a == 0) || (b == c)` parses as
  `(0 + (a==0)) || (b==c)`. Preserve the exact boolean grouping or the FEC index is
  skipped wrongly. The comment in the source spells it out — keep that logic.
- `SMPL_MAX_PULSES_PER_FRAME` is indexed by the RAW `low_rate` flag (row 0 = high
  rate), while the model/threshold tables use `low_rate ? 0 : 1`. These two index
  conventions are opposite on purpose (a fixed regression); do not "unify" them.
- The bitrate-controller polynomial uses `rate_kbps.powf(3.0)`/`.powf(4.0)` and
  `SMPL_E.powf(...)`; in Go use `math.Pow(x, 3)`/`math.Pow(x, 4)` and
  `math.Pow(math.E, ...)` (or `math.Exp`) cast to `float32`, matching the f32
  rounding the reference rounds to. The R4 budget vector pins the integer result
  (`mp[1] == 23`).
