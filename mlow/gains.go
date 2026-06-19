package mlow

// Per-subframe gains + energy-residual decode (func 3545 GAINS block), for UNVOICED
// internal frames (LSF stage-1 selector 0) — mutually exclusive with the pitch block.

// SmplGainResult holds the decoded per-subframe gains and energy-residual symbols.
type SmplGainResult struct {
	GainQ  [4]int32 // per-subframe quantized log-gain (Q-domain)
	NrgRes [4]int32 // per-subframe energy-residual symbol (only subframes with pulses are read)
}

// DecodeSmplGains decodes the gains+nrgres reads (the p3==4 path). subfrCounts are
// the per-subframe pulse counts.
func DecodeSmplGains(dec *RangeDecoder, mem *SmplMem, p3 int32, subfrCounts [4]int32) SmplGainResult {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/ed12f35/wacore/src/voip/mlow/smpl_gains.rs#L18-L69
	var res SmplGainResult
	gNrg := mem.GNrg

	// main gain (n=85) + delta gain (n=99)
	gainMain := dec.DecodeCDF(mem.CDFAt(gNrg+0x1362, 85))
	gainDelta := dec.DecodeCDF(mem.CDFAt(gNrg+0x1098, 99))
	cfgSel := int32(2)

	// gain reconstruction. The index sf + p3*gain_delta is NOT bounded to the visible
	// 64-entry array (gain_delta up to 98) — the WASM reads adjacent rodata, which the
	// heap window reproduces.
	gainTabAddr := uint32(0xf3970)
	if p3 == 4 {
		gainTabAddr = 0xf35f0
	}
	off6 := p3 * gainDelta
	base7 := gainMain*int32(mem.I16(0xf35e0+uint32(cfgSel)*2)) - 0x154000
	take := int(p3)
	if take > 4 {
		take = 4
	}
	for sf := 0; sf < take; sf++ {
		cbv := int32(mem.I16(gainTabAddr + uint32(int32(sf)+off6)*2))
		res.GainQ[sf] = base7 + (cbv << 4)
	}

	// nrgres: per-subframe bucketed CDF (n=92) with a gain-derived address shift.
	nrgBase := gNrg + uint32(cfgSel)*0x588
	for sf := 0; sf < take; sf++ {
		cnt := subfrCounts[sf]
		if cnt <= 0 {
			continue
		}
		var bucket int32
		if cnt >= 30 {
			bucket = 3
		} else {
			bucket = (cnt & 0xffff) / 10
		}
		cdfp := nrgBase + uint32(bucket)*0x162
		// g = clamp((gainQ[sf]+8192)>>14, floor -85); neg_part = (g>>31)&g; addr = cdfp - 2*neg_part.
		g := (res.GainQ[sf] + 8192) >> 14
		if g < -85 {
			g = -85
		}
		negPart := (g >> 31) & g
		off := cdfp - uint32(negPart<<1)
		res.NrgRes[sf] = dec.DecodeCDF(mem.CDFAt(off, 92))
	}
	return res
}
