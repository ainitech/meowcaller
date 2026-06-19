package mlow

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

func loadJSON(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
}

// TestA2NLSFMatchesC checks the fixed-point forward A→NLSF reproduces the C lsf
// from the post-BWE A for every captured record. Shared fixed-point arithmetic
// makes this exact to within the Q15→radians float rounding.
func TestA2NLSFMatchesC(t *testing.T) {
	var recs []struct {
		A   []float32 `json:"A"`
		LSF []float32 `json:"lsf"`
	}
	loadJSON(t, "lsf_quant_io.json", &recs)

	var worst float32
	for n, r := range recs {
		got := smplA2NLSF16(r.A)
		for k := range SmplLPCOrder {
			d := float32(math.Abs(float64(got[k] - r.LSF[k])))
			if d > worst {
				worst = d
			}
			if d >= 1e-4 {
				t.Errorf("rec %d nlsf[%d]: got %.7f want %.7f (d=%.2e)", n, k, got[k], r.LSF[k], d)
			}
		}
	}
	t.Logf("a2nlsf worst abs error = %.3e", worst)
}

// TestFrontEndAMatchesC checks the analysis front-end against the C: the windowing
// is exact (<1e-6) and the FFT-autocorr → A tracks the C to a tight tolerance on
// frames above the energy floor.
func TestFrontEndAMatchesC(t *testing.T) {
	var recs []struct {
		LpcBuf   []float32 `json:"lpcbuf"`
		Windowed []float32 `json:"windowed"`
		A        []float32 `json:"A"`
		R        []float64 `json:"R"`
		NumFrame int       `json:"numframe"`
	}
	loadJSON(t, "fe_dump.json", &recs)
	if len(recs) < 12 {
		t.Fatalf("need front-end vectors, got %d", len(recs))
	}

	var worst, worstWin float32
	for n, r := range recs {
		if len(r.LpcBuf) != SmplLPCBufLen {
			t.Fatalf("rec %d lpcbuf len %d", n, len(r.LpcBuf))
		}
		var buf [SmplLPCBufLen]float32
		copy(buf[:], r.LpcBuf)
		// use_long_win = numframe < 2 (frames_per_packet-1 == 2)
		win := smplWindowLPC20(&buf, r.NumFrame < 2)
		for k := range SmplLPCBufLen {
			if d := float32(math.Abs(float64(win[k] - r.Windowed[k]))); d > worstWin {
				worstWin = d
			}
		}
		a, _ := smplLPCAnalyzeWithF2(&win)
		var rd float32
		for k := 0; k <= SmplLPCOrder; k++ {
			if d := float32(math.Abs(float64(a[k] - r.A[k]))); d > rd {
				rd = d
			}
		}
		if rd > worst {
			worst = rd
		}
		// Above the energy floor, A must match tightly (FFT-internal rounding only);
		// below it the LPC is ill-conditioned and the frame is silent regardless.
		if r.R[0] > 1e-7 && rd >= 5e-3 {
			t.Errorf("rec %d (nf %d, R0=%.2e) |dA|=%.2e too large", n, r.NumFrame, r.R[0], rd)
		}
	}
	if worstWin >= 1e-6 {
		t.Errorf("windowing |dwin|=%.2e exceeds 1e-6", worstWin)
	}
	t.Logf("front-end worst |dA|=%.2e worst |dwin|=%.2e", worst, worstWin)
}

// TestDecoderReconstructsCQlsf is the wire round-trip check (qi → decoder NLSF
// reconstruction vs C qlsf). It depends on the LSF quantizer (#06) and synth (#10)
// modules, so it lands once those exist.
func TestDecoderReconstructsCQlsf(t *testing.T) {
	t.Skip("depends on smpl_lsf_quant (#06) and smpl_synth (#10), not yet built")
}
