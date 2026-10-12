package rtp

import "sync"

// Receiver bandwidth estimate for inbound video. The announced value is the bandwidth
// this endpoint estimates it can receive, not the rate arriving: the peer uses it as
// the ceiling of its own estimator, so echoing the measured rate would pin a collapsed
// sender where it is.

const (
	receiverEstimateInitialBps   = 300_000
	receiverEstimateMinBps       = 64_000
	receiverEstimateMaxBps       = 2_000_000
	receiverEstimateQuietLoss    = 2.0
	receiverEstimateCongestLoss  = 10.0
	receiverEstimateGrowthFactor = 1.5
	receiverEstimateBackoff      = 0.85
)

// NextReceiverMaxBitrate returns the next ceiling to announce from the previous one
// (0 before the first), the video payload octets received in the closed window, the
// window's real duration, and the stream's cumulative loss percent.
func NextReceiverMaxBitrate(previous uint32, octets uint64, elapsedMs uint64, lossPercent float64) uint32 {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/87dd5b0cdd5e7e0c40b209b3bc7d47b0043d2349/packages/voip-media/src/media/rtcp.ts#L860-L884
	// ASSUMPTION: the band constants (300k initial, 64k floor, 2M cap, 2%/10% loss bands,
	// x1.5 growth, x0.85 backoff) are zapo's engineering choices, measured against WhatsApp
	// Web's sender; a live meowcaller capture showing a different ramp would invalidate them.
	if previous == 0 {
		return receiverEstimateInitialBps
	}
	var measured float64
	if elapsedMs > 0 && octets > 0 {
		measured = float64(octets) * 8 * 1000 / float64(elapsedMs)
	}
	prev := float64(previous)
	var next float64
	switch {
	case lossPercent >= receiverEstimateCongestLoss:
		anchor := prev
		if measured > 0 && measured < prev {
			anchor = measured
		}
		next = anchor * receiverEstimateBackoff
	case lossPercent >= receiverEstimateQuietLoss:
		next = prev
	default:
		next = max(prev*receiverEstimateGrowthFactor, measured*receiverEstimateGrowthFactor)
	}
	if next < receiverEstimateMinBps {
		return receiverEstimateMinBps
	}
	if next > receiverEstimateMaxBps {
		return receiverEstimateMaxBps
	}
	return uint32(next)
}

const (
	audioReceiverEstimateInitialBps = 24_000
	audioReceiverEstimateMinBps     = 16_000
	audioReceiverEstimateMaxBps     = 30_000
	audioReceiverEstimateGrowth     = 1.1
)

// NextAudioReceiverEstimate returns the next audio REMB ceiling: 24 kbps first, 10 % up per
// interval under 2 % loss to 30 kbps, held up to 10 %, 15 % down above, never below 16 kbps.
// The peer's audio quality controller leaves its probing state only once a REMB above its
// 22 kbps threshold arrives; without one it holds audio at 15 kbps for the whole call.
func NextAudioReceiverEstimate(previous uint32, lossPercent float64) uint32 {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/3c594911144e09951bd58f084f40162145bde9cc/packages/voip-media/src/media/rtcp.ts#L902-L925
	if previous == 0 {
		return audioReceiverEstimateInitialBps
	}
	next := float64(previous)
	switch {
	case lossPercent >= receiverEstimateCongestLoss:
		next *= receiverEstimateBackoff
	case lossPercent < receiverEstimateQuietLoss:
		next *= audioReceiverEstimateGrowth
	}
	if next < audioReceiverEstimateMinBps {
		return audioReceiverEstimateMinBps
	}
	if next > audioReceiverEstimateMaxBps {
		return audioReceiverEstimateMaxBps
	}
	return uint32(next)
}

// ReceiverEstimate accumulates inbound video octets per window and carries the
// announced ceiling across windows. Safe for the receive loop and the RTCP ticker.
type ReceiverEstimate struct {
	mu            sync.Mutex
	ceiling       uint32
	octets        uint64
	windowStartMs uint64
	ssrc          uint32
}

// Observe records one authenticated inbound video payload from ssrc.
func (r *ReceiverEstimate) Observe(ssrc uint32, payloadBytes int, nowMs uint64) {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/87dd5b0cdd5e7e0c40b209b3bc7d47b0043d2349/packages/voip-media/src/call/WaCallMediaPlane.ts#L1725-L1742
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.windowStartMs == 0 {
		r.windowStartMs = nowMs
	}
	r.ssrc = ssrc
	r.octets += uint64(payloadBytes)
}

// Tick closes the window when video has arrived and returns the ceiling to announce,
// or 0 while nothing has been received yet.
func (r *ReceiverEstimate) Tick(nowMs uint64, lossPercent func(ssrc uint32) float64) uint32 {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/87dd5b0cdd5e7e0c40b209b3bc7d47b0043d2349/packages/voip-media/src/call/WaCallMediaPlane.ts#L1725-L1742
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.windowStartMs == 0 {
		return 0
	}
	var loss float64
	if lossPercent != nil {
		loss = lossPercent(r.ssrc)
	}
	var elapsed uint64
	if nowMs > r.windowStartMs {
		elapsed = nowMs - r.windowStartMs
	}
	r.ceiling = NextReceiverMaxBitrate(r.ceiling, r.octets, elapsed, loss)
	r.octets = 0
	r.windowStartMs = nowMs
	return r.ceiling
}

// Ceiling is the last announced ceiling, 0 before the first window closed.
func (r *ReceiverEstimate) Ceiling() uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ceiling
}
