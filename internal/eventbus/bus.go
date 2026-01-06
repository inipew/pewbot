package eventbus

import (
	logx "pewbot/pkg/logx"
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
	return NewWithLogger(logx.Nop())
}

// NewWithLogger is like New but allows controlling where drop warnings are logged.
//
// The bus is intentionally non-blocking; when a subscriber is slow and its buffer
// fills up, events are dropped. This constructor enables periodic warnings when
// drops occur.
func NewWithLogger(log logx.Logger) Bus {
	if log.IsZero() {
		log = logx.Nop()
	}
	return &memBus{subs: map[uint64]chan Event{}, log: log}
}

type memBus struct {
	mu   sync.RWMutex
	subs map[uint64]chan Event
	seq  atomic.Uint64
	log  logx.Logger

	// drop tracking (best-effort; used for periodic warnings)
	dropsTotal    atomic.Uint64
	lastLogUnixNS atomic.Int64
	lastLogged    atomic.Uint64
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
				b.noteDrop(len(chs))
			}
		}()
	}
}

func (b *memBus) noteDrop(subs int) {
	total := b.dropsTotal.Add(1)
	log := b.log
	if log.IsZero() {
		return
	}

	// Rate-limit warnings to avoid noisy logs during bursts.
	const interval = 30 * time.Second
	now := time.Now().UnixNano()
	last := b.lastLogUnixNS.Load()
	if last != 0 && time.Duration(now-last) < interval {
		return
	}
	if !b.lastLogUnixNS.CompareAndSwap(last, now) {
		return
	}
	prev := b.lastLogged.Swap(total)
	delta := total - prev
	log.Warn("eventbus dropped events (slow subscriber)",
		logx.Uint64("dropped_since_last", delta),
		logx.Uint64("dropped_total", total),
		logx.Int("subscribers", subs),
	)
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
