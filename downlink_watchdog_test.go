package meowcaller

import (
	"testing"
	"time"
)

func TestDownlinkWatchdogSchedule(t *testing.T) {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/bceb268c846763a2cd0b494da830a54aacdc3818/packages/voip-media/src/relay/WaRelayDownlinkWatchdog.ts#L42-L62
	var w downlinkWatchdog
	t0 := time.Unix(1_000, 0)
	tickAt := func(d time.Duration) (downlinkAction, int) { return w.tick(t0.Add(d)) }
	if a, _ := tickAt(0); a != downlinkNone {
		t.Fatal("the first tick opens the stall clock")
	}
	if a, _ := tickAt(4 * time.Second); a != downlinkNone {
		t.Fatal("no action before 5 s")
	}
	if a, _ := tickAt(5 * time.Second); a != downlinkReallocate {
		t.Fatal("re-allocate at 5 s")
	}
	if a, _ := tickAt(9 * time.Second); a != downlinkNone {
		t.Fatal("re-allocate fires once; the redial waits")
	}
	if a, n := tickAt(10 * time.Second); a != downlinkRedial || n != 1 {
		t.Fatalf("first redial at 10 s, got (%v, %d)", a, n)
	}
	// The stall clock restarts at the redial: re-allocate at +5 s, second redial at +15 s.
	if a, _ := tickAt(15 * time.Second); a != downlinkReallocate {
		t.Fatal("re-allocate again 5 s after the first redial")
	}
	if a, _ := tickAt(24 * time.Second); a != downlinkNone {
		t.Fatal("second redial waits 5 + 10 s")
	}
	if a, n := tickAt(25 * time.Second); a != downlinkRedial || n != 2 {
		t.Fatalf("second redial at 25 s, got (%v, %d)", a, n)
	}
	if a, _ := tickAt(30 * time.Second); a != downlinkReallocate {
		t.Fatal("re-allocate again after the second redial")
	}
	if a, n := tickAt(50 * time.Second); a != downlinkRedial || n != 3 {
		t.Fatalf("third redial at 25 + 5 + 20 s, got (%v, %d)", a, n)
	}
	if a, _ := tickAt(55 * time.Second); a != downlinkReallocate {
		t.Fatal("re-allocate still fires after the last redial")
	}
	if a, _ := tickAt(200 * time.Second); a != downlinkNone {
		t.Fatal("no fourth redial per call")
	}
}

func TestDownlinkWatchdogPeerMediaClearsTheStallButNotTheRedialCount(t *testing.T) {
	var w downlinkWatchdog
	t0 := time.Unix(1_000, 0)
	w.tick(t0)
	w.tick(t0.Add(5 * time.Second))
	if a, n := w.tick(t0.Add(10 * time.Second)); a != downlinkRedial || n != 1 {
		t.Fatal("setup: first redial")
	}
	w.notePeerMedia()
	if a, _ := w.tick(t0.Add(11 * time.Second)); a != downlinkNone {
		t.Fatal("peer media reopens the stall clock from scratch")
	}
	if a, _ := w.tick(t0.Add(16 * time.Second)); a != downlinkReallocate {
		t.Fatal("a new stall re-allocates 5 s after it opened")
	}
	if a, n := w.tick(t0.Add(26 * time.Second)); a != downlinkRedial || n != 2 {
		t.Fatalf("the redial count carries across stalls: got (%v, %d), want attempt 2 at 5 + 10 s", a, n)
	}
}
