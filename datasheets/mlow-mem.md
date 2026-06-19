# Datasheet: `mlow/mem`

The embedded heap-memory window and table-base pointers used to replicate exact
pointer arithmetic when reading the runtime-built CDF tables, plus the fixed cosine
approximation table for the LSF root search. Media layer; a data/table provider that
the pulse/pitch/gains and LSF decode paths read through.

**Validation vector:** `smpl_tables.json` — the runtime-built CDF tables this module
backs (the decode paths that consume the heap window and cosine table are pinned by
it). Copy it verbatim into `mlow/testdata/`. The heap window itself loads from
`smpl_cc_blob.json`, which must also sit in `mlow/testdata/`.

## Reference source (verbatim — authoritative)

### Heap-memory window and table-base pointers

```rust
//! Embedded window of the MLow WASM heap (the runtime-built CDF tables func 3559 constructs into
//! BSS-pointed heap blocks) plus the table base pointers, so the pulse/pitch/gains decode can
//! replicate the WASM's exact pointer arithmetic byte for byte. Ported from the Go reference
//! (`smpl_mem.go`); the window was dumped from a live func-3559 run into `smpl_cc_blob.json`.

use base64::Engine;
use std::sync::OnceLock;

struct SmplMemRegion {
    base: u32,
    data: Vec<u8>,
}

pub(crate) struct SmplMem {
    regions: Vec<SmplMemRegion>,
    pub(crate) g_cc: u32,
    pub(crate) g_nrg: u32,
    pub(crate) g_pitch: u32,
    pub(crate) g_clk: u32,
}

static SMPL_MEM: OnceLock<SmplMem> = OnceLock::new();

pub(crate) fn load_smpl_mem() -> &'static SmplMem {
    SMPL_MEM.get_or_init(|| {
        #[derive(serde::Deserialize)]
        struct RawRegion {
            base: u32,
            b64: String,
        }
        #[derive(serde::Deserialize)]
        struct Raw {
            regions: Vec<RawRegion>,
            g_cc: u32,
            g_nrg: u32,
            g_pitch: u32,
            clk: u32,
        }
        let raw: Raw = serde_json::from_str(include_str!("testdata/smpl_cc_blob.json"))
            .expect("smpl_cc_blob.json must parse");
        let engine = base64::engine::general_purpose::STANDARD;
        let regions = raw
            .regions
            .into_iter()
            .map(|r| SmplMemRegion {
                base: r.base,
                data: engine.decode(r.b64).expect("smpl_cc_blob region b64"),
            })
            .collect();
        SmplMem {
            regions,
            g_cc: raw.g_cc,
            g_nrg: raw.g_nrg,
            g_pitch: raw.g_pitch,
            g_clk: raw.clk,
        }
    })
}

impl SmplMem {
    /// Region containing `[addr, addr+n)` and the byte offset of `addr` within it, or `None`.
    fn region_for(&self, addr: u32, n: usize) -> Option<(&[u8], usize)> {
        for r in &self.regions {
            if addr >= r.base && (addr - r.base) as usize + n <= r.data.len() {
                return Some((&r.data, (addr - r.base) as usize));
            }
        }
        None
    }

    pub(crate) fn u8(&self, addr: u32) -> u8 {
        self.region_for(addr, 1).map_or(0, |(d, off)| d[off])
    }

    pub(crate) fn u16(&self, addr: u32) -> u16 {
        self.region_for(addr, 2)
            .map_or(0, |(d, off)| u16::from_le_bytes([d[off], d[off + 1]]))
    }

    pub(crate) fn i16(&self, addr: u32) -> i16 {
        self.u16(addr) as i16
    }

    pub(crate) fn u32(&self, addr: u32) -> u32 {
        self.region_for(addr, 4).map_or(0, |(d, off)| {
            u32::from_le_bytes([d[off], d[off + 1], d[off + 2], d[off + 3]])
        })
    }

    pub(crate) fn i32(&self, addr: u32) -> i32 {
        self.u32(addr) as i32
    }

    /// Materialize the n-entry cumulative u16 CDF at WASM address `addr` (for `decode_cdf`). Entries
    /// outside the window read as 0 — matching the WASM's out-of-region fallback.
    pub(crate) fn cdf_at(&self, addr: u32, n: usize) -> Vec<u16> {
        (0..n)
            .map(|i| self.u16(addr.wrapping_add((i as u32) * 2)))
            .collect()
    }
}
```

### LSF cosine approximation table

```rust
// Auto-extracted from silk/table_LSF_cos.c (silk_LSFCosTab_FIX_Q12, Q12, 129 entries).
// Cosine approximation table for the silk A2NLSF root search.
#[rustfmt::skip]
const SILK_LSF_COS_TAB_FIX_Q12: [i32; 129] = [
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
];
```

## Go envelope (signatures only)

```go
package mlow

type smplMemRegion struct {
	base uint32
	data []byte
}

type SmplMem struct {
	regions []smplMemRegion
	GCC     uint32
	GNrg    uint32
	GPitch  uint32
	GClk    uint32
}

func LoadSmplMem() *SmplMem

func (m *SmplMem) regionFor(addr uint32, n int) (data []byte, off int, ok bool)
func (m *SmplMem) U8(addr uint32) uint8
func (m *SmplMem) U16(addr uint32) uint16
func (m *SmplMem) I16(addr uint32) int16
func (m *SmplMem) U32(addr uint32) uint32
func (m *SmplMem) I32(addr uint32) int32
func (m *SmplMem) CDFAt(addr uint32, n int) []uint16

var silkLSFCosTabFIXQ12 = [129]int32{
	// 129 entries, see verbatim source
}
```

## Implementation suggestions (guidance, not authoritative)

- `region_for` returns `Option<(&[u8], usize)>`; the idiomatic Go form is a
  multi-value return with a trailing `ok bool` (or a `nil` slice). Every accessor must
  preserve the "out of region reads as 0" fallback — that is observable behavior the
  CDF materialization relies on.
- Width mapping: `u8`/`u16`/`u32` → `uint8`/`uint16`/`uint32`; `i16`/`i32` are the
  signed reinterpretations of the same bytes (`int16(m.U16(addr))`, `int32(m.U32(addr))`),
  not separate reads.
- All multi-byte reads are little-endian; use `encoding/binary.LittleEndian.Uint16` /
  `Uint32` over the offset slice rather than hand-assembling shifts.
- The blob loads once and is shared immutably; `sync.OnceLock` maps to a `sync.Once`
  (or a package-level `var ... = loadSmplMem()` if eager init is acceptable). The
  decoded regions are read-only after init.
- The base64 region payloads use standard encoding (`base64.StdEncoding`). TODO(human):
  confirm the JSON field names (`base`/`b64` per region; top-level `g_cc`, `g_nrg`,
  `g_pitch`, `clk`) match what your struct tags expect, since `clk` maps to the
  `GClk` field, not `g_clk`.
- The cosine table is a fixed 129-entry `int32` array (Q12, symmetric around index 64
  where the value is 0); a package-level array literal is fine — no runtime init.
```