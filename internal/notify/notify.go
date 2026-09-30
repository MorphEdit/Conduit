// Package notify is a tiny fan-out wake-up signal.
package notify

import "sync"

type Bus struct {
	mu   sync.Mutex
	subs []chan struct{}
}

// Subscribe returns a channel that receives at most one pending signal.
func (b *Bus) Subscribe() <-chan struct{} {
	c := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs = append(b.subs, c)
	b.mu.Unlock()
	return c
}

// Unsubscribe stops delivering to c.
func (b *Bus) Unsubscribe(c <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, s := range b.subs {
		if s == c {
			b.subs = append(b.subs[:i], b.subs[i+1:]...)
			return
		}
	}
}

// Publish wakes every subscriber without blocking.
func (b *Bus) Publish() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}
