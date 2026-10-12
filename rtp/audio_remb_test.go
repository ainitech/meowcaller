package rtp

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEncodeRembBitrate(t *testing.T) {
	// Below 2^18 the pair is the plain value; above, the exponent climbs and truncates.
	if got := EncodeRembBitrate(110_238); got != 110_238 {
		t.Fatalf("plain = %d, want 110238", got)
	}
	if got := EncodeRembBitrate(300_000); got != 1<<18|150_000 {
		t.Fatalf("300000 = %#x, want exponent 1 mantissa 150000", got)
	}
	if got := EncodeRembBitrate(2_000_001); got != 3<<18|250_000 {
		t.Fatalf("2000001 = %#x, want exponent 3 mantissa 250000 (truncated)", got)
	}
	if got := EncodeRembBitrate(0xffff_ffff); got != 14<<18|0x3ffff {
		t.Fatalf("max = %#x, want exponent 14 saturated mantissa", got)
	}
}

func TestBuildReceiverEstimatedMaxBitrate(t *testing.T) {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/3c594911144e09951bd58f084f40162145bde9cc/packages/voip-media/src/media/rtcp.ts#L993-L1009
	got := BuildReceiverEstimatedMaxBitrate(0x11112222, 0x55556666, 24_000)
	want := []byte{
		0x8f, 206, 0x00, 0x05,
		0x11, 0x11, 0x22, 0x22,
		0x00, 0x00, 0x00, 0x00,
		'R', 'E', 'M', 'B',
		0x01, 0x00, 0x5d, 0xc0,
		0x55, 0x55, 0x66, 0x66,
	}
	if !bytes.Equal(got[:], want) {
		t.Fatalf("remb = %x, want %x", got[:], want)
	}
	if pt, ok := RtcpPayloadType(got[:]); !ok || pt != RtcpPtPsfb {
		t.Fatalf("payload type = (%d, %v), want PSFB", pt, ok)
	}
	if binary.BigEndian.Uint32(got[16:20])>>24 != 1 {
		t.Fatal("ssrc count must be 1")
	}
}

func TestNextAudioReceiverEstimateBands(t *testing.T) {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/3c594911144e09951bd58f084f40162145bde9cc/packages/voip-media/src/media/rtcp.ts#L902-L925
	cases := []struct {
		name     string
		previous uint32
		loss     float64
		want     uint32
	}{
		{"first interval opens above the 22 kbps probing threshold", 0, 0, 24_000},
		{"quiet grows 10%", 24_000, 1, 26_400},
		{"cap at 30 kbps", 29_000, 0, 30_000},
		{"intermediate loss holds", 26_400, 5, 26_400},
		{"congested backs off 15%", 30_000, 12, 25_500},
		{"floor at 16 kbps", 17_000, 50, 16_000},
	}
	for _, c := range cases {
		if got := NextAudioReceiverEstimate(c.previous, c.loss); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestTakeLossPercentHasItsOwnInterval(t *testing.T) {
	var s RtcpReceptionStats
	for seq := uint16(1); seq <= 10; seq++ {
		if seq == 5 {
			continue
		}
		s.Observe(7, seq, uint32(seq)*960, uint64(seq)*60, 16000)
	}
	if got := s.TakeLossPercent(); got != 10 {
		t.Fatalf("first take = %v%%, want 10", got)
	}
	if got := s.TakeLossPercent(); got != 0 {
		t.Fatalf("second take with nothing new = %v%%, want 0", got)
	}
	report := s.Report(700)
	if report == nil || report.FractionLost == 0 {
		t.Fatalf("report = %+v, want the sender-report interval untouched by TakeLossPercent", report)
	}
	for seq := uint16(11); seq <= 20; seq++ {
		if seq == 12 || seq == 13 {
			continue
		}
		s.Observe(7, seq, uint32(seq)*960, uint64(seq)*60, 16000)
	}
	if got := s.TakeLossPercent(); got != 20 {
		t.Fatalf("third take = %v%%, want 20 over the new interval only", got)
	}
	var set RtcpReceptionStatsSet
	if set.TakeLossPercent(99) != 0 {
		t.Fatal("untracked ssrc must read 0")
	}
}
