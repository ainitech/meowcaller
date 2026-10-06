package rtp

import (
	"bytes"
	"testing"
)

// The official client's id-13 content for a 110,238 bps estimate, captured by zapo:
// bitmap 0x09, the 3-byte estimate, the receiver capacity field and a flags byte.
var fastRembObservedContent = []byte{0x09, 0x01, 0xae, 0x9e, 0x00, 0x0c, 0x3f, 0x11, 0x01}

const fastRembObservedBps = 110_238

func TestFastRembElementMatchesZapoVector(t *testing.T) {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/87dd5b0cdd5e7e0c40b209b3bc7d47b0043d2349/packages/voip-media/src/media/__tests__/fast-remb.test.ts#L14-L47
	want := []byte{0xd3, 0x01, 0x01, 0xae, 0x9e}
	if got := appendFastRemb(nil, fastRembObservedBps); !bytes.Equal(got, want) {
		t.Fatalf("fast remb element = %x, want %x", got, want)
	}
	if got := appendFastRemb(nil, 0xffff_ffff); !bytes.Equal(got[2:], []byte{0xff, 0xff, 0xff}) {
		t.Fatalf("saturated estimate = %x, want ff ff ff", got[2:])
	}
}

func TestVideoExtensionCarriesAndParsesReceiverEstimate(t *testing.T) {
	frame := uint16(7)
	ext := VideoRtpExtension{MediaFrameInfo: 0x08, FrameNumber: &frame, ShortOffset: 29, TransportSequence: 3, ReceiverEstimateBps: fastRembObservedBps}
	encoded := ext.encode()
	if len(encoded)%4 != 0 || len(encoded) != 20 {
		t.Fatalf("encoded length = %d, want 20 (13 + 5 bytes padded)", len(encoded))
	}
	header := EncodeRtpHeader(&RtpHeader{PayloadType: RtpPayloadTypeH264, Ssrc: 1, VideoExtension: &ext})
	parsed, ok := ParseWhatsappVideoExtension(header)
	if !ok {
		t.Fatal("extension did not parse")
	}
	if parsed.ReceiverEstimateBps != fastRembObservedBps || parsed.TransportSequence != 3 || parsed.FrameNumber == nil || *parsed.FrameNumber != 7 {
		t.Fatalf("parsed = %+v", parsed)
	}

	without := VideoRtpExtension{MediaFrameInfo: 0x08, ShortOffset: 29}
	if n := len(without.encode()); n != 12 {
		t.Fatalf("extension without estimate = %d bytes, want the unchanged 12", n)
	}
}

func TestParseVideoExtensionReadsOfficialFastRembContent(t *testing.T) {
	ext := []byte{0x30, 0x08, 0x61, 0, 29}
	ext = append(ext, FastRembExtensionID<<4|byte(len(fastRembObservedContent)-1))
	ext = append(ext, fastRembObservedContent...)
	for len(ext)%4 != 0 {
		ext = append(ext, 0)
	}
	packet := []byte{0x90, RtpPayloadTypeH264, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1, 0xde, 0xbe, 0, byte(len(ext) / 4)}
	packet = append(packet, ext...)
	parsed, ok := ParseWhatsappVideoExtension(packet)
	if !ok {
		t.Fatal("official extension did not parse")
	}
	if parsed.ReceiverEstimateBps != fastRembObservedBps {
		t.Fatalf("estimate = %d, want %d", parsed.ReceiverEstimateBps, fastRembObservedBps)
	}
}

func TestNextReceiverMaxBitrateBands(t *testing.T) {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/87dd5b0cdd5e7e0c40b209b3bc7d47b0043d2349/packages/voip-media/src/media/rtcp.ts#L860-L884
	cases := []struct {
		name     string
		previous uint32
		octets   uint64
		elapsed  uint64
		loss     float64
		want     uint32
	}{
		{"first window announces the initial ceiling", 0, 0, 0, 0, 300_000},
		{"quiet path grows by 1.5", 300_000, 1000, 1000, 1, 450_000},
		{"quiet path follows overshoot above the ceiling", 100_000, 50_000, 1000, 0, 600_000},
		{"intermediate loss holds", 300_000, 1000, 1000, 5, 300_000},
		{"congested backs off from the ceiling", 400_000, 100_000, 1000, 12, 340_000},
		{"congested anchors to a lower measurement", 400_000, 10_000, 1000, 12, 68_000},
		{"floor", 70_000, 100, 1000, 50, 64_000},
		{"cap", 1_900_000, 0, 1000, 0, 2_000_000},
	}
	for _, c := range cases {
		if got := NextReceiverMaxBitrate(c.previous, c.octets, c.elapsed, c.loss); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestReceiverEstimateWindowsAndLoss(t *testing.T) {
	var est ReceiverEstimate
	if est.Tick(1000, nil) != 0 || est.Ceiling() != 0 {
		t.Fatal("ticked without video must announce nothing")
	}
	est.Observe(0x55, 500, 1000)
	est.Observe(0x55, 500, 1500)
	if got := est.Tick(2500, nil); got != 300_000 || est.Ceiling() != 300_000 {
		t.Fatalf("first close = %d, want the initial 300000", got)
	}
	est.Observe(0x55, 100, 3000)
	lossSeen := uint32(0)
	got := est.Tick(4000, func(ssrc uint32) float64 { lossSeen = ssrc; return 20 })
	if lossSeen != 0x55 {
		t.Fatalf("loss queried for ssrc %#x, want 0x55", lossSeen)
	}
	if got != 64_000 {
		t.Fatalf("congested close = %d, want the floor: the 800 bps measurement anchors the backoff below 64000", got)
	}
}

func TestCumulativeLossPercentDoesNotConsumeTheInterval(t *testing.T) {
	var s RtcpReceptionStats
	for seq := uint16(1); seq <= 10; seq++ {
		if seq == 3 || seq == 4 {
			continue
		}
		s.Observe(1, seq, uint32(seq)*3000, uint64(seq)*33, 90000)
	}
	if got := s.CumulativeLossPercent(); got != 20 {
		t.Fatalf("loss = %v%%, want 20", got)
	}
	report := s.Report(400)
	if report == nil || report.FractionLost == 0 {
		t.Fatalf("report after the loss read = %+v, want the interval still unconsumed", report)
	}
	var set RtcpReceptionStatsSet
	if set.CumulativeLossPercent(99) != 0 {
		t.Fatal("untracked ssrc must read 0")
	}
}
