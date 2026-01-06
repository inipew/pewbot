package engine

import (
	"strings"
	"sync"
)

// groupSemaphore is a simple channel-based semaphore used for concurrency groups.
// Tokens are pre-filled up to limit.
//
// Note: limit is fixed for the life of the semaphore. If callers request a
// different limit for an existing key, the engine will keep the first value.
type groupSemaphore struct {
	limit int
	ch    chan struct{}
}

func newGroupSemaphore(limit int) *groupSemaphore {
	if limit <= 0 {
		limit = 1
	}
	gs := &groupSemaphore{limit: limit, ch: make(chan struct{}, limit)}
	for i := 0; i < limit; i++ {
		gs.ch <- struct{}{}
	}
	return gs
}

func (g *groupSemaphore) tryAcquire() bool {
	if g == nil {
		return true
	}
	select {
	case <-g.ch:
		return true
	default:
		return false
	}
}

func (g *groupSemaphore) release() {
	if g == nil {
		return
	}
	// Best-effort: never block on release.
	select {
	case g.ch <- struct{}{}:
	default:
	}
}

// groupKey derives the effective group key.
func groupKey(concurrencyKey, name string) string {
	k := strings.TrimSpace(concurrencyKey)
	if k == "" {
		k = strings.TrimSpace(name)
	}
	return k
}

// groupLimiterStore holds group semaphores.
// Embedded into Service via fields.
type groupLimiterStore struct {
	mu     sync.Mutex
	groups map[string]*groupSemaphore
}

func (s *groupLimiterStore) get(key string, limit int) *groupSemaphore {
	if s == nil {
		return nil
	}
	if limit <= 0 {
		return nil
	}
	k := strings.TrimSpace(key)
	if k == "" {
		return nil
	}

	s.mu.Lock()
	if s.groups == nil {
		s.groups = make(map[string]*groupSemaphore)
	}
	gs := s.groups[k]
	if gs == nil {
		gs = newGroupSemaphore(limit)
		s.groups[k] = gs
	}
	// If limit mismatches, keep the first-seen limit to avoid unsafe runtime resizing.
	s.mu.Unlock()
	return gs
}
