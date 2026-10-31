package meowcaller

import "sync"

// relayTransport is the relay media channel surface the media loop uses, so a leg can be
// replaced under it.
type relayTransport interface {
	Send([]byte) (int, error)
	Recv([]byte) (int, error)
	Close() error
}

// relayLink is the call's current relay leg. A redial swaps the leg in place: senders keep
// writing through the link, and a receiver blocked on the old leg moves to the new one.
type relayLink struct {
	mu         sync.RWMutex
	ch         relayTransport
	generation uint64
}

func newRelayLink(ch relayTransport) *relayLink {
	return &relayLink{ch: ch}
}

func (l *relayLink) current() (relayTransport, uint64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.ch, l.generation
}

// Send writes on the current leg.
func (l *relayLink) Send(data []byte) (int, error) {
	ch, _ := l.current()
	return ch.Send(data)
}

// Recv reads from the current leg; a read that fails because the leg was swapped continues
// on the new leg, and any other failure is returned.
func (l *relayLink) Recv(buf []byte) (int, error) {
	// Source of truth: https://github.com/vinikjkkj/zapo/blob/bceb268c846763a2cd0b494da830a54aacdc3818/packages/voip-media/src/relay/WaSctpRelay.ts#L638-L655
	for {
		ch, generation := l.current()
		n, err := ch.Recv(buf)
		if err == nil {
			return n, nil
		}
		if _, now := l.current(); now == generation {
			return n, err
		}
	}
}

// swap replaces the current leg and closes the old one.
func (l *relayLink) swap(ch relayTransport) {
	l.mu.Lock()
	old := l.ch
	l.ch = ch
	l.generation++
	l.mu.Unlock()
	_ = old.Close()
}

// Close closes the current leg.
func (l *relayLink) Close() error {
	ch, _ := l.current()
	return ch.Close()
}
