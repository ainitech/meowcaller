# Datasheet: `mlow/rangecoder`

The Opus/CELT range entropy coder (decoder and encoder) that all other media-frame
parameter reads and writes flow through. Media layer; this is the bit-level
primitive every symbol decode/encode is built on.

**Validation vector:** `rc_vectors.json` — replays a mixed script of icdf / raw
back-bits / bit_logp / uint / cdf operations that must decode to the listed values
and re-encode to the listed bytes, bit-for-bit. Copy it verbatim into
`mlow/testdata/`.

## Reference source (verbatim — authoritative)

```rust
//! Opus/CELT range decoder (`ec_dec`, RFC 6716 §4.1) — the entropy coder MLow's smpl_audio_codec
//! reuses verbatim (WASM func 3631). Ported byte-for-byte from the Go reference
//! (`meowmeow/voip/media/rangecoder.go`) so the symbols match the WhatsApp WASM bit-for-bit.
//! Range-coded symbols come from the front of the buffer, raw bits from the back. `wrapping_*` is
//! used wherever the Go reference relies on uint32 modular arithmetic.
//!
//! This is a COMPLETE ec_dec port: the raw-bits / icdf / uint primitives are validated by the
//! round-trip vector test but the mlow decode path itself only exercises a subset, so the unused
//! primitives are allowed rather than removed (keeping the entropy coder faithful and reusable).
#![allow(dead_code)]

const EC_SYM_BITS: u32 = 8;
const EC_CODE_BITS: u32 = 32;
const EC_SYM_MAX: u32 = (1 << EC_SYM_BITS) - 1; // 255
const EC_CODE_TOP: u32 = 1u32 << (EC_CODE_BITS - 1);
const EC_CODE_BOT: u32 = EC_CODE_TOP >> EC_SYM_BITS;
const EC_CODE_EXTRA: u32 = (EC_CODE_BITS - 2) % EC_SYM_BITS + 1; // 7
const EC_WINDOW_SIZE: u32 = 32;
const EC_UINT_BITS: i32 = 8;
const EC_CODE_SHIFT: u32 = EC_CODE_BITS - EC_SYM_BITS - 1; // 23

/// EC_ILOG: floor(log2(x))+1 for x>0, 0 for x==0 (Go `bits.Len32`).
#[inline]
fn ilog(x: u32) -> i32 {
    (EC_CODE_BITS - x.leading_zeros()) as i32
}

#[inline]
fn ec_mini(a: u32, b: u32) -> u32 {
    if a < b { a } else { b }
}

pub(crate) struct RangeDecoder<'a> {
    buf: &'a [u8],
    storage: u32,
    end_offs: u32,
    end_window: u32,
    nend_bits: i32,
    nbits_total: i32,
    offs: u32,
    rng: u32,
    val: u32,
    ext: u32,
    rem: i32,
    /// Sticky decode error (degenerate/malformed table or exhausted bits). Inspectable so the
    /// higher layers can fail loud instead of synthesizing from garbage.
    pub(crate) err: i32,
}

impl<'a> RangeDecoder<'a> {
    /// RFC 6716 `ec_dec_init`.
    pub(crate) fn new(buf: &'a [u8]) -> Self {
        let mut d = RangeDecoder {
            buf,
            storage: buf.len() as u32,
            end_offs: 0,
            end_window: 0,
            nend_bits: 0,
            nbits_total: EC_CODE_BITS as i32 + 1
                - (((EC_CODE_BITS - EC_CODE_EXTRA) / EC_SYM_BITS) * EC_SYM_BITS) as i32,
            offs: 0,
            rng: 1u32 << EC_CODE_EXTRA,
            val: 0,
            ext: 0,
            rem: 0,
            err: 0,
        };
        d.rem = d.read_byte() as i32;
        d.val = d.rng - 1 - ((d.rem >> (EC_SYM_BITS - EC_CODE_EXTRA)) as u32);
        d.normalize();
        d
    }

    fn read_byte(&mut self) -> u32 {
        if self.offs < self.storage {
            let b = self.buf[self.offs as usize];
            self.offs += 1;
            b as u32
        } else {
            0
        }
    }

    fn read_byte_from_end(&mut self) -> u32 {
        if self.end_offs < self.storage {
            self.end_offs += 1;
            self.buf[(self.storage - self.end_offs) as usize] as u32
        } else {
            0
        }
    }

    fn normalize(&mut self) {
        while self.rng <= EC_CODE_BOT {
            self.nbits_total += EC_SYM_BITS as i32;
            self.rng <<= EC_SYM_BITS;
            let sym0 = self.rem;
            self.rem = self.read_byte() as i32;
            let sym = (sym0 << EC_SYM_BITS | self.rem) >> (EC_SYM_BITS - EC_CODE_EXTRA);
            self.val = (self
                .val
                .wrapping_shl(EC_SYM_BITS)
                .wrapping_add(EC_SYM_MAX & !(sym as u32)))
                & (EC_CODE_TOP - 1);
        }
    }

    /// Cumulative frequency in [0, ft) for the next symbol; caller locates the symbol and calls
    /// `update`.
    pub(crate) fn decode(&mut self, ft: u32) -> u32 {
        if ft == 0 {
            self.err = 1;
            self.ext = 1;
            return 0;
        }
        self.ext = self.rng / ft;
        if self.ext == 0 {
            self.err = 1;
            self.ext = 1;
            return 0;
        }
        let s = self.val / self.ext;
        ft - ec_mini(s + 1, ft)
    }

    #[allow(dead_code)] // used by the parse layer (smpl sign coder), landing next
    fn decode_bin(&mut self, bits_n: u32) -> u32 {
        self.ext = self.rng >> bits_n;
        if self.ext == 0 {
            self.err = 1;
            self.ext = 1;
            return 0;
        }
        let s = self.val / self.ext;
        let ft = 1u32 << bits_n;
        ft - ec_mini(s + 1, ft)
    }

    /// Uniform `nbits`-bit symbol decoded directly off the range stream (func 3545 sign coder).
    #[allow(dead_code)] // used by the parse layer, landing next
    pub(crate) fn decode_raw_symbol(&mut self, nbits: u32) -> u32 {
        let sym = self.decode_bin(nbits);
        self.update(sym, sym + 1, 1u32 << nbits);
        sym
    }

    /// Advance past the symbol with cumulative range [fl,fh) out of ft.
    pub(crate) fn update(&mut self, fl: u32, fh: u32, ft: u32) {
        let s = self.ext.wrapping_mul(ft - fh);
        self.val = self.val.wrapping_sub(s);
        if fl > 0 {
            self.rng = self.ext.wrapping_mul(fh - fl);
        } else {
            self.rng = self.rng.wrapping_sub(s);
        }
        self.normalize();
    }

    /// One bit with P(0) = 1/2^logp (`ec_dec_bit_logp`).
    pub(crate) fn bit_logp(&mut self, logp: u32) -> i32 {
        let r = self.rng;
        let dv = self.val;
        let s = r >> logp;
        let ret = if dv < s { 1 } else { 0 };
        if ret == 0 {
            self.val = dv - s;
            self.rng = r - s;
        } else {
            self.rng = s;
        }
        self.normalize();
        ret
    }

    /// Symbol against an inverse-CDF table (`ec_dec_icdf`); `ftb = log2(ft)`.
    pub(crate) fn decode_icdf(&mut self, icdf: &[u8], ftb: u32) -> i32 {
        if icdf.is_empty() {
            self.err = 1;
            return 0;
        }
        let s0 = self.rng;
        let dv = self.val;
        let r = s0 >> ftb;
        let mut ret: i32 = -1;
        let mut t;
        let mut s = s0;
        loop {
            t = s;
            ret += 1;
            s = r.wrapping_mul(icdf[ret as usize] as u32);
            if dv >= s || ret as usize >= icdf.len() - 1 {
                break;
            }
        }
        self.val = dv - s;
        self.rng = t - s;
        self.normalize();
        ret
    }

    /// Symbol against a u16 CUMULATIVE CDF table (WASM func 3476, the smpl primitive). Effective
    /// total is `cdf[n-1] - cdf[0]` (a non-zero base is subtracted out).
    pub(crate) fn decode_cdf(&mut self, cdf: &[u16]) -> i32 {
        let n = cdf.len();
        if n < 2 {
            self.err = 1;
            return 0;
        }
        let base = cdf[0] as u32;
        if cdf[n - 1] as u32 <= base {
            self.err = 1;
            return 0;
        }
        let ft = cdf[n - 1] as u32 - base;
        let fs = self.decode(ft);
        let target = base + fs;
        let mut k = 0usize;
        while k < n - 1 {
            if cdf[k + 1] as u32 > target {
                break;
            }
            k += 1;
        }
        self.update(cdf[k] as u32 - base, cdf[k + 1] as u32 - base, ft);
        k as i32
    }

    /// Raw `n` bits from the BACK of the buffer (`ec_dec_bits`), LSB-first.
    pub(crate) fn bits_n(&mut self, n: u32) -> u32 {
        let mut window = self.end_window;
        let mut available = self.nend_bits;
        if (available as u32) < n {
            loop {
                window |= self.read_byte_from_end() << (available as u32);
                available += EC_SYM_BITS as i32;
                if available as u32 > EC_WINDOW_SIZE - EC_SYM_BITS {
                    break;
                }
            }
        }
        let ret = window & ((1u32 << n) - 1);
        window >>= n;
        available -= n as i32;
        self.end_window = window;
        self.nend_bits = available;
        self.nbits_total += n as i32;
        ret
    }

    /// Integer uniformly distributed in [0, ft) for ft>1 (`ec_dec_uint`).
    pub(crate) fn decode_uint(&mut self, ft0: u32) -> u32 {
        let ft = ft0 - 1;
        let mut ftb = ilog(ft);
        if ftb > EC_UINT_BITS {
            ftb -= EC_UINT_BITS;
            let t = (ft >> (ftb as u32)) + 1;
            let s = self.decode(t);
            self.update(s, s + 1, t);
            let v = (s << (ftb as u32)) | self.bits_n(ftb as u32);
            if v <= ft {
                return v;
            }
            self.err = 1;
            return ft;
        }
        let ft = ft + 1;
        let s = self.decode(ft);
        self.update(s, s + 1, ft);
        s
    }

    /// 64-symbol uniform fine-lag read (func 3545 2106..2174): `ext = rng>>6`,
    /// `sym = clamp(63 - val/ext, 0, 64)`, then `update(sym, sym+1, 64)`.
    pub(crate) fn decode_64_fine_sym(&mut self) -> i32 {
        self.ext = self.rng >> 6;
        if self.ext == 0 {
            self.err = 1;
            self.ext = 1;
            return 0;
        }
        let s = self.val / self.ext;
        let sym = (63i64 - s as i64).clamp(0, 64) as i32;
        self.update(sym as u32, sym as u32 + 1, 64);
        sym
    }

    /// Bits consumed so far, rounded up (`ec_tell`).
    #[allow(dead_code)]
    pub(crate) fn tell(&self) -> i32 {
        self.nbits_total - ilog(self.rng)
    }
}

/// Opus/CELT range ENCODER (`ec_enc`, libopus celt/entenc.c) — the exact inverse of `RangeDecoder`,
/// used by the mlow ENCODER. Writes range-coded symbols toward the front and raw bits toward the
/// back; `done()` flushes and merges them, after which `bytes()` is the finished payload.
pub(crate) struct RangeEncoder {
    buf: Vec<u8>,
    storage: u32,
    end_offs: u32,
    end_window: u32,
    nend_bits: i32,
    nbits_total: i32,
    offs: u32,
    rng: u32,
    val: u32,
    ext: u32,
    rem: i32,
    err: i32,
}

impl RangeEncoder {
    pub(crate) fn new(size: usize) -> Self {
        RangeEncoder {
            buf: vec![0u8; size],
            storage: size as u32,
            end_offs: 0,
            end_window: 0,
            nend_bits: 0,
            nbits_total: EC_CODE_BITS as i32 + 1,
            offs: 0,
            rng: EC_CODE_TOP,
            val: 0,
            ext: 0,
            rem: -1,
            err: 0,
        }
    }

    pub(crate) fn err(&self) -> i32 {
        self.err
    }

    fn write_byte(&mut self, b: u32) {
        if self.offs + self.end_offs < self.storage {
            self.buf[self.offs as usize] = b as u8;
            self.offs += 1;
        } else {
            self.err = -1;
        }
    }

    fn write_byte_at_end(&mut self, b: u32) {
        if self.offs + self.end_offs < self.storage {
            self.end_offs += 1;
            self.buf[(self.storage - self.end_offs) as usize] = b as u8;
        } else {
            self.err = -1;
        }
    }

    fn carry_out(&mut self, c: i32) {
        if c as u32 != EC_SYM_MAX {
            let carry = c >> EC_SYM_BITS;
            if self.rem >= 0 {
                self.write_byte((self.rem + carry) as u32);
            }
            if self.ext > 0 {
                let sym = ((EC_SYM_MAX as i32 + carry) & EC_SYM_MAX as i32) as u32;
                loop {
                    self.write_byte(sym);
                    self.ext -= 1;
                    if self.ext == 0 {
                        break;
                    }
                }
            }
            self.rem = c & EC_SYM_MAX as i32;
        } else {
            self.ext += 1;
        }
    }

    fn normalize(&mut self) {
        while self.rng <= EC_CODE_BOT {
            self.carry_out((self.val >> EC_CODE_SHIFT) as i32);
            self.val = self.val.wrapping_shl(EC_SYM_BITS) & (EC_CODE_TOP - 1);
            self.rng <<= EC_SYM_BITS;
            self.nbits_total += EC_SYM_BITS as i32;
        }
    }

    pub(crate) fn encode(&mut self, fl: u32, fh: u32, ft: u32) {
        if ft == 0 {
            self.err = -1;
            return;
        }
        let r = self.rng / ft;
        if fl > 0 {
            self.val = self
                .val
                .wrapping_add(self.rng.wrapping_sub(r.wrapping_mul(ft - fl)));
            self.rng = r.wrapping_mul(fh - fl);
        } else {
            self.rng = self.rng.wrapping_sub(r.wrapping_mul(ft - fh));
        }
        self.normalize();
    }

    pub(crate) fn bit_logp(&mut self, val: i32, logp: u32) {
        let r = self.rng;
        let l = self.val;
        let s = r >> logp;
        let r2 = r - s;
        if val != 0 {
            self.val = l.wrapping_add(r2);
            self.rng = s;
        } else {
            self.rng = r2;
        }
        self.normalize();
    }

    pub(crate) fn encode_icdf(&mut self, s: i32, icdf: &[u8], ftb: u32) {
        let r = self.rng >> ftb;
        if s > 0 {
            self.val = self.val.wrapping_add(
                self.rng
                    .wrapping_sub(r.wrapping_mul(icdf[(s - 1) as usize] as u32)),
            );
            self.rng = r.wrapping_mul(icdf[(s - 1) as usize].wrapping_sub(icdf[s as usize]) as u32);
        } else {
            self.rng = self
                .rng
                .wrapping_sub(r.wrapping_mul(icdf[s as usize] as u32));
        }
        self.normalize();
    }

    /// Inverse of `decode_cdf`: encode symbol `s` against a u16 cumulative CDF (`ft = cdf[n-1]-cdf[0]`).
    pub(crate) fn encode_cdf(&mut self, s: i32, cdf: &[u16]) {
        let n = cdf.len();
        if n < 2 || s < 0 || (s + 1) as usize >= n {
            self.err = -1;
            return;
        }
        let base = cdf[0] as u32;
        if cdf[n - 1] as u32 <= base {
            self.err = -1;
            return;
        }
        let ft = cdf[n - 1] as u32 - base;
        self.encode(
            cdf[s as usize] as u32 - base,
            cdf[(s + 1) as usize] as u32 - base,
            ft,
        );
    }

    /// Raw `n` bits toward the back of the buffer.
    pub(crate) fn bits_n(&mut self, fl: u32, n: u32) {
        let mut window = self.end_window;
        let mut used = self.nend_bits;
        if used + n as i32 > EC_WINDOW_SIZE as i32 {
            loop {
                self.write_byte_at_end(window & EC_SYM_MAX);
                window >>= EC_SYM_BITS;
                used -= EC_SYM_BITS as i32;
                if used < EC_SYM_BITS as i32 {
                    break;
                }
            }
        }
        window |= fl.wrapping_shl(used as u32);
        used += n as i32;
        self.end_window = window;
        self.nend_bits = used;
        self.nbits_total += n as i32;
    }

    pub(crate) fn encode_uint(&mut self, fl: u32, ft0: u32) {
        let ft = ft0 - 1;
        let ftb = ilog(ft);
        if ftb > EC_UINT_BITS {
            let ftb = (ftb - EC_UINT_BITS) as u32;
            let t = (ft >> ftb) + 1;
            self.encode(fl >> ftb, (fl >> ftb) + 1, t);
            self.bits_n(fl & ((1u32 << ftb) - 1), ftb);
        } else {
            self.encode(fl, fl + 1, ft + 1);
        }
    }

    /// Inverse of `decode_raw_symbol`: encode a uniform `nbits`-bit symbol on the range stream.
    pub(crate) fn encode_raw_symbol(&mut self, sym: u32, nbits: u32) {
        self.encode(sym, sym + 1, 1u32 << nbits);
    }

    /// Inverse of `decode_64_fine_sym`: encode the 64-symbol uniform fine-lag value.
    pub(crate) fn encode_64_fine_sym(&mut self, sym: i32) {
        self.encode(sym as u32, sym as u32 + 1, 64);
    }

    /// Flush the range coder and merge the back raw-bit stream. After this, `bytes()` is the payload.
    pub(crate) fn done(&mut self) {
        let mut l = EC_CODE_BITS as i32 - ilog(self.rng);
        let mut msk = (EC_CODE_TOP - 1) >> (l as u32);
        let mut end = self.val.wrapping_add(msk) & !msk;
        if end | msk >= self.val.wrapping_add(self.rng) {
            l += 1;
            msk >>= 1;
            end = self.val.wrapping_add(msk) & !msk;
        }
        while l > 0 {
            self.carry_out((end >> EC_CODE_SHIFT) as i32);
            end = end.wrapping_shl(EC_SYM_BITS) & (EC_CODE_TOP - 1);
            l -= EC_SYM_BITS as i32;
        }
        if self.rem >= 0 || self.ext > 0 {
            self.carry_out(0);
        }
        let mut window = self.end_window;
        let mut used = self.nend_bits;
        while used >= EC_SYM_BITS as i32 {
            self.write_byte_at_end(window & EC_SYM_MAX);
            window >>= EC_SYM_BITS;
            used -= EC_SYM_BITS as i32;
        }
        if self.err == 0 {
            for i in self.offs..(self.storage - self.end_offs) {
                self.buf[i as usize] = 0;
            }
            if used > 0 {
                if self.end_offs >= self.storage - self.offs {
                    self.err = -1;
                } else {
                    self.buf[(self.storage - self.end_offs - 1) as usize] |= window as u8;
                }
            }
        }
    }

    pub(crate) fn bytes(&self) -> &[u8] {
        &self.buf
    }

    /// Meaningful body length = front (range) bytes + back (raw-bit) bytes; the gap between is
    /// zero-fill padding that `done()` wrote.
    pub(crate) fn consumed_len(&self) -> usize {
        (self.offs + self.end_offs) as usize
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::Value;

    fn vectors() -> Value {
        serde_json::from_str(include_str!("testdata/rc_vectors.json"))
            .expect("rc_vectors.json must parse")
    }

    // Replays a deterministic mixed script (icdf / raw back-bits / bit_logp / uint) that the Go
    // reference encoded, requiring identical decoded values — proves ec_dec parity bit-for-bit.
    #[test]
    fn range_decoder_matches_go_vectors() {
        let v = vectors();
        // The dumped `icdf` is the fixed table the Go script encoded against (ftb=8).
        let icdf: [u8; 6] = [255, 200, 150, 90, 30, 0];
        let bytes = hex::decode(v["bytesHex"].as_str().unwrap()).unwrap();
        let mut d = RangeDecoder::new(&bytes);
        for (i, op) in v["ops"].as_array().unwrap().iter().enumerate() {
            let kind = op["kind"].as_u64().unwrap();
            let a = op["a"].as_u64().unwrap() as u32;
            let b = op["b"].as_u64().unwrap() as u32;
            match kind {
                0 => assert_eq!(d.decode_icdf(&icdf, 8), a as i32, "op {i} icdf"),
                1 => assert_eq!(d.bits_n(a), b, "op {i} bits({a})"),
                2 => assert_eq!(d.bit_logp(a) as u32, b, "op {i} bit_logp({a})"),
                3 => assert_eq!(d.decode_uint(a), b, "op {i} uint(ft={a})"),
                _ => unreachable!("bad op kind {kind}"),
            }
        }
        assert_eq!(d.err, 0, "no decode error");
    }

    // Exercises decodeCDF against cumulative tables (including non-zero-base ones).
    #[test]
    fn range_decoder_cdf_matches_go_vectors() {
        let v = vectors();
        let tables: Vec<Vec<u16>> = v["cdfTables"]
            .as_array()
            .unwrap()
            .iter()
            .map(|t| {
                t.as_array()
                    .unwrap()
                    .iter()
                    .map(|x| x.as_u64().unwrap() as u16)
                    .collect()
            })
            .collect();
        let bytes = hex::decode(v["cdfBytesHex"].as_str().unwrap()).unwrap();
        let mut d = RangeDecoder::new(&bytes);
        for (i, op) in v["cdfOps"].as_array().unwrap().iter().enumerate() {
            let ti = op["kind"].as_u64().unwrap() as usize;
            let sym = op["a"].as_u64().unwrap() as i32;
            assert_eq!(d.decode_cdf(&tables[ti]), sym, "cdf op {i} table {ti}");
        }
        assert_eq!(d.err, 0, "no decode error");
    }

    // Re-encodes the same script the Go reference encoded and requires byte-identical output — proves
    // our ec_enc matches the Go/WASM range encoder bit-for-bit (the foundation of the mlow encoder).
    #[test]
    fn range_encoder_matches_go_bytes() {
        let v = vectors();
        let icdf: [u8; 6] = [255, 200, 150, 90, 30, 0];
        let want = hex::decode(v["bytesHex"].as_str().unwrap()).unwrap();
        let mut e = RangeEncoder::new(want.len());
        for op in v["ops"].as_array().unwrap() {
            let kind = op["kind"].as_u64().unwrap();
            let a = op["a"].as_u64().unwrap() as u32;
            let b = op["b"].as_u64().unwrap() as u32;
            match kind {
                0 => e.encode_icdf(a as i32, &icdf, 8),
                1 => e.bits_n(b, a),
                2 => e.bit_logp(b as i32, a),
                3 => e.encode_uint(b, a),
                _ => unreachable!(),
            }
        }
        e.done();
        assert_eq!(e.err(), 0, "encoder error");
        assert_eq!(e.bytes(), want.as_slice(), "encoder output differs from Go");
    }

    #[test]
    fn range_encoder_cdf_matches_go_bytes() {
        let v = vectors();
        let tables: Vec<Vec<u16>> = v["cdfTables"]
            .as_array()
            .unwrap()
            .iter()
            .map(|t| {
                t.as_array()
                    .unwrap()
                    .iter()
                    .map(|x| x.as_u64().unwrap() as u16)
                    .collect()
            })
            .collect();
        let want = hex::decode(v["cdfBytesHex"].as_str().unwrap()).unwrap();
        let mut e = RangeEncoder::new(want.len());
        for op in v["cdfOps"].as_array().unwrap() {
            let ti = op["kind"].as_u64().unwrap() as usize;
            let sym = op["a"].as_u64().unwrap() as i32;
            e.encode_cdf(sym, &tables[ti]);
        }
        e.done();
        assert_eq!(e.err(), 0, "encoder error");
        assert_eq!(
            e.bytes(),
            want.as_slice(),
            "cdf encoder output differs from Go"
        );
    }
}
```

## Go envelope (signatures only)

```go
package mlow

type RangeDecoder struct {
	buf        []byte
	storage    uint32
	endOffs    uint32
	endWindow  uint32
	nendBits   int32
	nbitsTotal int32
	offs       uint32
	rng        uint32
	val        uint32
	ext        uint32
	rem        int32
	// Sticky decode error (degenerate/malformed table or exhausted bits).
	Err int32
}

func NewRangeDecoder(buf []byte) *RangeDecoder

func (d *RangeDecoder) Decode(ft uint32) uint32
func (d *RangeDecoder) DecodeRawSymbol(nbits uint32) uint32
func (d *RangeDecoder) Update(fl, fh, ft uint32)
func (d *RangeDecoder) BitLogp(logp uint32) int32
func (d *RangeDecoder) DecodeICDF(icdf []byte, ftb uint32) int32
func (d *RangeDecoder) DecodeCDF(cdf []uint16) int32
func (d *RangeDecoder) BitsN(n uint32) uint32
func (d *RangeDecoder) DecodeUint(ft0 uint32) uint32
func (d *RangeDecoder) Decode64FineSym() int32
func (d *RangeDecoder) Tell() int32

type RangeEncoder struct {
	buf        []byte
	storage    uint32
	endOffs    uint32
	endWindow  uint32
	nendBits   int32
	nbitsTotal int32
	offs       uint32
	rng        uint32
	val        uint32
	ext        uint32
	rem        int32
	err        int32
}

func NewRangeEncoder(size int) *RangeEncoder

func (e *RangeEncoder) Err() int32
func (e *RangeEncoder) Encode(fl, fh, ft uint32)
func (e *RangeEncoder) BitLogp(val int32, logp uint32)
func (e *RangeEncoder) EncodeICDF(s int32, icdf []byte, ftb uint32)
func (e *RangeEncoder) EncodeCDF(s int32, cdf []uint16)
func (e *RangeEncoder) BitsN(fl, n uint32)
func (e *RangeEncoder) EncodeUint(fl, ft0 uint32)
func (e *RangeEncoder) EncodeRawSymbol(sym, nbits uint32)
func (e *RangeEncoder) Encode64FineSym(sym int32)
func (e *RangeEncoder) Done()
func (e *RangeEncoder) Bytes() []byte
func (e *RangeEncoder) ConsumedLen() int
```

## Implementation suggestions (guidance, not authoritative)

- All `u32` arithmetic is modular: Go `uint32` wraps on overflow by default, so the
  Rust `wrapping_shl`/`wrapping_add`/`wrapping_sub`/`wrapping_mul` calls become plain
  `<<` / `+` / `-` / `*` on `uint32`. Keep every working value typed `uint32` so the
  wrap happens automatically; do not promote to `uint64` mid-expression.
- `ilog` is `floor(log2)+1`; Go's `bits.Len32(x)` is the exact equivalent (and is 0 at
  x==0). `leading_zeros()` → `bits.LeadingZeros32`.
- The decoder borrows the input slice (`&'a [u8]`); in Go just hold the `[]byte` —
  out-of-range reads return 0 (`read_byte` / `read_byte_from_end`), so preserve that
  bounds check rather than indexing blindly.
- `rem` is signed and initialized to `-1` in the encoder (a sentinel meaning "no
  pending byte"); keep it `int32`, not `uint`. The `carry_out`/`done` logic depends on
  the `rem >= 0` test.
- Errors are sticky flags (`err` set to `1` on the decoder, `-1` on the encoder), not
  returned values — replicate the field, do not convert to Go `error` returns, or the
  vector's `err == 0` assertion changes shape.
- `decode_64_fine_sym` clamps via an i64 intermediate (`63 - val/ext`) before casting
  to i32; in Go compute `63 - int64(s)` then clamp into `[0,64]`. TODO(human): confirm
  whether your build needs the i64 widening or whether i32 suffices for the observed
  ranges.
- `bits_n` packs raw bits LSB-first from the back of the buffer; the window is a
  `uint32`. Watch the shift `1<<n` when `n` could be 0 — the reference never calls it
  with n==0 on this path, but guard if you generalize.
```