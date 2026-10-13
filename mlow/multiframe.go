package mlow

import "errors"

// Multiframe packet: [0x82 | toc&0x39][count][count-1 lengths][sub-frames, each with its
// TOC]. The official 1:1 client bundles two 60 ms frames per packet, driven by rc.maxfpp.

const (
	multiframeMarker    = 0x82
	multiframeMaxFrames = 8
)

// ErrMultiframeFrames is returned when the frame set cannot be packed.
var ErrMultiframeFrames = errors.New("mlow multiframe: need 2 to 8 non-empty frames")

// PackMultiframe bundles encoded MLow frames into one multiframe packet.
func PackMultiframe(frames [][]byte) ([]byte, error) {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/3c594911144e09951bd58f084f40162145bde9cc/packages/voip-media/src/media/mlow-codec.ts#L38-L42
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/3c594911144e09951bd58f084f40162145bde9cc/packages/voip-media/src/media/mlow-codec.ts#L186-L216
	if len(frames) < 2 || len(frames) > multiframeMaxFrames {
		return nil, ErrMultiframeFrames
	}
	size := 2
	for i, f := range frames {
		if len(f) == 0 {
			return nil, ErrMultiframeFrames
		}
		size += len(f)
		if i < len(frames)-1 {
			size += multiframeLengthBytes(len(f))
		}
	}
	out := make([]byte, 0, size)
	out = append(out, multiframeMarker|frames[0][0]&0x39, byte(len(frames)))
	for _, f := range frames[:len(frames)-1] {
		out = appendMultiframeLength(out, len(f))
	}
	for _, f := range frames {
		out = append(out, f...)
	}
	return out, nil
}

// multiframeLengthBytes is the Opus-style length prefix size: one byte below 252, else two.
func multiframeLengthBytes(n int) int {
	if n < 252 {
		return 1
	}
	return 2
}

// appendMultiframeLength writes n as [n] below 252, else [252 + (n-252)%4][(n-first)/4].
func appendMultiframeLength(out []byte, n int) []byte {
	if n < 252 {
		return append(out, byte(n))
	}
	first := 252 + (n-252)%4
	return append(out, byte(first), byte((n-first)/4))
}
