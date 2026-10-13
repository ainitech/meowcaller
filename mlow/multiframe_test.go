package mlow

import (
	"bytes"
	"testing"
)

func TestPackMultiframeRoundtripsThroughTheDecoderSplit(t *testing.T) {
	first := append([]byte{0x50}, bytes.Repeat([]byte{0xaa}, 100)...)
	second := append([]byte{0x50}, bytes.Repeat([]byte{0xbb}, 97)...)
	packed, err := PackMultiframe([][]byte{first, second})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if packed[0] != 0x92 || packed[1] != 2 || packed[2] != byte(len(first)) {
		t.Fatalf("header = %x, want 92 02 %02x (marker 0x82|toc&0x39, count, first length)", packed[:3], len(first))
	}
	frames, ok := splitContainer(packed)
	if !ok || len(frames) != 2 || !bytes.Equal(frames[0], first) || !bytes.Equal(frames[1], second) {
		t.Fatalf("decoder split = (%d frames, %v), want the two frames back intact", len(frames), ok)
	}
}

func TestPackMultiframeLongLengthPrefix(t *testing.T) {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/3c594911144e09951bd58f084f40162145bde9cc/packages/voip-media/src/media/mlow-codec.ts#L186-L216
	first := append([]byte{0x50}, make([]byte, 300)...)
	second := []byte{0x50, 1, 2, 3}
	packed, err := PackMultiframe([][]byte{first, second})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	// 301 = 253 + 12*4: first byte 252 + (301-252)%4 = 253, second byte (301-253)/4 = 12.
	if packed[2] != 253 || packed[3] != 12 {
		t.Fatalf("length prefix = %d %d, want 253 12", packed[2], packed[3])
	}
	if !bytes.Equal(packed[4:4+len(first)], first) || !bytes.Equal(packed[4+len(first):], second) {
		t.Fatal("frames not laid out after the two-byte length prefix")
	}
}

func TestPackMultiframeRejectsBadSets(t *testing.T) {
	if _, err := PackMultiframe([][]byte{{0x50, 1}}); err == nil {
		t.Fatal("one frame must not pack")
	}
	if _, err := PackMultiframe([][]byte{{0x50, 1}, {}}); err == nil {
		t.Fatal("an empty frame must not pack")
	}
}
