package meowcaller

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeLeg blocks Recv until a packet is queued or the leg is closed.
type fakeLeg struct {
	mu     sync.Mutex
	queue  chan []byte
	closed bool
	sent   [][]byte
}

func newFakeLeg() *fakeLeg { return &fakeLeg{queue: make(chan []byte, 8)} }

func (f *fakeLeg) Send(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, append([]byte(nil), p...))
	return len(p), nil
}

func (f *fakeLeg) Recv(buf []byte) (int, error) {
	p, ok := <-f.queue
	if !ok {
		return 0, errors.New("leg closed")
	}
	return copy(buf, p), nil
}

func (f *fakeLeg) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.queue)
	}
	return nil
}

func TestRelayLinkRecvContinuesOnTheRedialledLeg(t *testing.T) {
	first, second := newFakeLeg(), newFakeLeg()
	link := newRelayLink(first)
	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 16)
		n, err := link.Recv(buf)
		if err != nil {
			got <- nil
			return
		}
		got <- buf[:n]
	}()
	time.Sleep(20 * time.Millisecond)
	link.swap(second)
	if _, err := link.Send([]byte{1}); err != nil || len(second.sent) != 1 || len(first.sent) != 0 {
		t.Fatal("sends after the swap must go to the new leg only")
	}
	second.queue <- []byte{0xaa, 0xbb}
	select {
	case p := <-got:
		if len(p) != 2 || p[0] != 0xaa {
			t.Fatalf("recv after swap = %x, want the new leg's packet", p)
		}
	case <-time.After(time.Second):
		t.Fatal("receiver did not move to the new leg")
	}
	if !first.closed {
		t.Fatal("the old leg must be closed by the swap")
	}
}

func TestRelayLinkRecvReturnsALegFailureWithoutSwap(t *testing.T) {
	leg := newFakeLeg()
	link := newRelayLink(leg)
	_ = leg.Close()
	if _, err := link.Recv(make([]byte, 4)); err == nil {
		t.Fatal("a closed leg with no swap must surface its error")
	}
}
