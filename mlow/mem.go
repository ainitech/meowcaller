package mlow

import (
	"bytes"
	"compress/zlib"
	_ "embed"
	"encoding/binary"
	"io"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/purpshell/meowcaller/mlow/internal/tables"
)

// smplCCBlob is the embedded heap window — the runtime-built CDF tables plus the
// table-base pointers — as a zlib-compressed protobuf HeapWindow. The blob and its
// tables.proto schema are shared verbatim with the reference.
//
//go:embed smpl_cc_blob.bin
var smplCCBlob []byte

type smplMemRegion struct {
	base uint32
	data []byte
}

// SmplMem is an embedded window of the codec's heap holding the runtime-built CDF
// tables, plus the table-base pointers, so the decode paths can replicate the
// original pointer arithmetic exactly.
type SmplMem struct {
	regions []smplMemRegion
	GCC     uint32
	GNrg    uint32
	GPitch  uint32
	GClk    uint32
}

var (
	smplMemOnce sync.Once
	smplMem     *SmplMem
)

// LoadSmplMem decodes the embedded heap blob once and returns the shared,
// read-only window.
func LoadSmplMem() *SmplMem {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/b90291b1ae979d504adf71d9555b3daf5c7325b1/wacore/src/voip/mlow/smpl_tables_blob.rs#L32-L35
	smplMemOnce.Do(func() {
		zr, err := zlib.NewReader(bytes.NewReader(smplCCBlob))
		if err != nil {
			panic("mlow: open heap blob: " + err.Error())
		}
		raw, err := io.ReadAll(zr)
		if err != nil {
			panic("mlow: inflate heap blob: " + err.Error())
		}
		_ = zr.Close()
		var hw tables.HeapWindow
		if err := proto.Unmarshal(raw, &hw); err != nil {
			panic("mlow: decode heap blob: " + err.Error())
		}
		m := &SmplMem{
			GCC:    hw.GetGCc(),
			GNrg:   hw.GetGNrg(),
			GPitch: hw.GetGPitch(),
			GClk:   hw.GetGClk(),
		}
		for _, r := range hw.GetRegions() {
			m.regions = append(m.regions, smplMemRegion{base: r.GetBase(), data: r.GetData()})
		}
		smplMem = m
	})
	return smplMem
}

// regionFor returns the region data containing [addr, addr+n) and the byte offset
// of addr within it. ok is false when no region covers the range.
func (m *SmplMem) regionFor(addr uint32, n int) (data []byte, off int, ok bool) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/b90291b1ae979d504adf71d9555b3daf5c7325b1/wacore/src/voip/mlow/smpl_mem.rs#L79-L86
	for _, r := range m.regions {
		if addr >= r.base && int(addr-r.base)+n <= len(r.data) {
			return r.data, int(addr - r.base), true
		}
	}
	return nil, 0, false
}

// U8 reads one byte at addr, or 0 if addr is outside every region.
func (m *SmplMem) U8(addr uint32) uint8 {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/b90291b1ae979d504adf71d9555b3daf5c7325b1/wacore/src/voip/mlow/smpl_mem.rs#L88-L90
	if data, off, ok := m.regionFor(addr, 1); ok {
		return data[off]
	}
	return 0
}

// U16 reads a little-endian uint16 at addr, or 0 if out of region.
func (m *SmplMem) U16(addr uint32) uint16 {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/b90291b1ae979d504adf71d9555b3daf5c7325b1/wacore/src/voip/mlow/smpl_mem.rs#L92-L95
	if data, off, ok := m.regionFor(addr, 2); ok {
		return binary.LittleEndian.Uint16(data[off:])
	}
	return 0
}

// I16 is the signed reinterpretation of U16.
func (m *SmplMem) I16(addr uint32) int16 {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/b90291b1ae979d504adf71d9555b3daf5c7325b1/wacore/src/voip/mlow/smpl_mem.rs#L97-L99
	return int16(m.U16(addr))
}

// U32 reads a little-endian uint32 at addr, or 0 if out of region.
func (m *SmplMem) U32(addr uint32) uint32 {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/b90291b1ae979d504adf71d9555b3daf5c7325b1/wacore/src/voip/mlow/smpl_mem.rs#L101-L105
	if data, off, ok := m.regionFor(addr, 4); ok {
		return binary.LittleEndian.Uint32(data[off:])
	}
	return 0
}

// I32 is the signed reinterpretation of U32.
func (m *SmplMem) I32(addr uint32) int32 {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/b90291b1ae979d504adf71d9555b3daf5c7325b1/wacore/src/voip/mlow/smpl_mem.rs#L107-L109
	return int32(m.U32(addr))
}

// CDFAt materializes the n-entry cumulative uint16 CDF at addr; entries outside
// the window read as 0.
func (m *SmplMem) CDFAt(addr uint32, n int) []uint16 {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/b90291b1ae979d504adf71d9555b3daf5c7325b1/wacore/src/voip/mlow/smpl_mem.rs#L113-L117
	out := make([]uint16, n)
	for i := range n {
		out[i] = m.U16(addr + uint32(i)*2)
	}
	return out
}

// silkLSFCosTabFIXQ12 is the Q12 cosine approximation table (129 entries,
// symmetric around index 64) for the LSF root search.
//
// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/b90291b1ae979d504adf71d9555b3daf5c7325b1/wacore/src/voip/mlow/silk_lsf_cos_tab.rs#L4-L22
var silkLSFCosTabFIXQ12 = [129]int32{
	8192, 8190, 8182, 8170, 8152, 8130, 8104, 8072,
	8034, 7994, 7946, 7896, 7840, 7778, 7714, 7644,
	7568, 7490, 7406, 7318, 7226, 7128, 7026, 6922,
	6812, 6698, 6580, 6458, 6332, 6204, 6070, 5934,
	5792, 5648, 5502, 5352, 5198, 5040, 4880, 4718,
	4552, 4382, 4212, 4038, 3862, 3684, 3502, 3320,
	3136, 2948, 2760, 2570, 2378, 2186, 1990, 1794,
	1598, 1400, 1202, 1002, 802, 602, 402, 202,
	0, -202, -402, -602, -802, -1002, -1202, -1400,
	-1598, -1794, -1990, -2186, -2378, -2570, -2760, -2948,
	-3136, -3320, -3502, -3684, -3862, -4038, -4212, -4382,
	-4552, -4718, -4880, -5040, -5198, -5352, -5502, -5648,
	-5792, -5934, -6070, -6204, -6332, -6458, -6580, -6698,
	-6812, -6922, -7026, -7128, -7226, -7318, -7406, -7490,
	-7568, -7644, -7714, -7778, -7840, -7896, -7946, -7994,
	-8034, -8072, -8104, -8130, -8152, -8170, -8182, -8190,
	-8192,
}
