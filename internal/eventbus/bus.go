package eventbus

import (
	"sync"
	"sync/atomic"
	"time"
)

// Event is a lightweight, in-memory signal used to decouple components.
//
// Contract:
//   - Publish MUST be non-blocking.
//   - Subscribers MUST use buffered channels.
//   - Slow subscribers may drop events (bounded backpressure).
//
// Data should be small and ideally JSON-serializable.
type Event struct {
	Type string
	Time time.Time
	Data any
}

type Bus interface {
	Publish(e Event)
	Subscribe(buffer int) (ch <-chan Event, unsubscribe func())
}

// New returns a simple in-memory fanout bus.
//
// It intentionally does not own any background goroutines.
func New() Bus {
	return &memBus{subs: map[uint64]chan Event{}}
}

type memBus struct {
	mu   sync.RWMutex
	subs map[uint64]chan Event
	seq  atomic.Uint64
}

func (b *memBus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	// Snapshot subscribers so Publish doesn't hold locks while attempting sends.
	b.mu.RLock()
	chs := make([]chan Event, 0, len(b.subs))
	for _, ch := range b.subs {
		chs = append(chs, ch)
	}
	b.mu.RUnlock()

	for _, ch := range chs {
		// Non-blocking delivery. If subscriber is slow, we drop.
		// If a subscriber unsubscribes concurrently and the channel closes,
		// recover from a possible panic (send on closed channel).
		func() {
			defer func() { _ = recover() }()
			select {
			case ch <- e:
			default:
			}
		}()
	}
}

func (b *memBus) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 8
	}
	ch := make(chan Event, buffer)
	id := b.seq.Add(1)

	b.mu.Lock()
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			// Closing is safe because Publish recovers from send panics.
			close(ch)
		})
	}
	return ch, unsub
}
