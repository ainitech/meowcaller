package meowcaller

import (
	"sync"
	"time"
)

// Relay downlink watchdog: an accepted call whose leg stops hearing the peer while its
// pongs and allocates are still answered re-allocates after 5 s and redials the relay 5 s
// later, with a doubling wait and at most three redials per call. Any authenticated RTP,
// SRTCP or app-data counts as downlink, so a peer in DTX keeps it quiet.
const (
	downlinkReallocateAfter = 5 * time.Second
	downlinkRedialAfter     = 5 * time.Second
	downlinkMaxRedials      = 3
)

type downlinkAction int

const (
	downlinkNone downlinkAction = iota
	downlinkReallocate
	downlinkRedial
)

type downlinkWatchdog struct {
	mu           sync.Mutex
	stalledSince time.Time
	reallocated  bool
	redials      int
}

// tick is called once per keepalive on an accepted call and returns what to do now; for
// downlinkRedial the attempt number (1-based) is returned as well.
func (w *downlinkWatchdog) tick(now time.Time) (downlinkAction, int) {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/bceb268c846763a2cd0b494da830a54aacdc3818/packages/voip-media/src/relay/WaRelayDownlinkWatchdog.ts#L42-L62
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stalledSince.IsZero() {
		w.stalledSince = now
		return downlinkNone, 0
	}
	stalled := now.Sub(w.stalledSince)
	if !w.reallocated {
		if stalled < downlinkReallocateAfter {
			return downlinkNone, 0
		}
		w.reallocated = true
		return downlinkReallocate, 0
	}
	if w.redials >= downlinkMaxRedials {
		return downlinkNone, 0
	}
	redialAfter := downlinkReallocateAfter + downlinkRedialAfter*time.Duration(1<<w.redials)
	if stalled < redialAfter {
		return downlinkNone, 0
	}
	w.redials++
	w.stalledSince = now
	w.reallocated = false
	return downlinkRedial, w.redials
}

// notePeerMedia records authenticated downlink from the peer and clears the stall.
func (w *downlinkWatchdog) notePeerMedia() {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/bceb268c846763a2cd0b494da830a54aacdc3818/packages/voip-media/src/relay/WaRelayDownlinkWatchdog.ts#L64-L67
	w.mu.Lock()
	w.stalledSince = time.Time{}
	w.reallocated = false
	w.mu.Unlock()
}
