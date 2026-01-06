package broadcast

import (
	"sort"
	"time"
)

// cleanupStatuses evicts old/completed job statuses so memory usage stays bounded.
// Call with no locks; it will take statusMu internally.
func (s *Service) cleanupStatuses(now time.Time) {
	// Fast path: if bounds are not set, use safe defaults.
	max := s.statusMax
	if max <= 0 {
		max = 200
	}
	ttl := s.statusTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}

	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	// 1) Remove nil entries and TTL-expired completed jobs.
	for id, st := range s.status {
		if st == nil {
			delete(s.status, id)
			continue
		}
		if !st.DoneAt.IsZero() {
			if now.Sub(st.DoneAt) > ttl {
				delete(s.status, id)
			}
			continue
		}
		// If a job never started (or is stuck) and isn't running, evict after TTL too.
		if !st.Running && !st.CreatedAt.IsZero() && now.Sub(st.CreatedAt) > ttl {
			delete(s.status, id)
		}
	}

	// 2) Enforce max size: remove oldest non-running jobs.
	over := len(s.status) - max
	if over <= 0 {
		return
	}

	type cand struct {
		id string
		t  time.Time
	}
	cands := make([]cand, 0, len(s.status))
	for id, st := range s.status {
		if st == nil || st.Running {
			continue
		}
		key := st.DoneAt
		if key.IsZero() {
			key = st.CreatedAt
		}
		cands = append(cands, cand{id: id, t: key})
	}
	// If everything is running (unlikely), do nothing.
	if len(cands) == 0 {
		return
	}

	sort.Slice(cands, func(i, j int) bool {
		// zero time sorts first
		if cands[i].t.IsZero() && !cands[j].t.IsZero() {
			return true
		}
		if !cands[i].t.IsZero() && cands[j].t.IsZero() {
			return false
		}
		return cands[i].t.Before(cands[j].t)
	})

	for i := 0; i < len(cands) && over > 0; i++ {
		delete(s.status, cands[i].id)
		over--
	}
}
