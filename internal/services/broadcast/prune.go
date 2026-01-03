package broadcast

import (
	"sort"
	"time"
)

const (
	// Keep broadcaster status memory bounded.
	// Broadcast jobs can be created frequently (e.g., periodic notifications),
	// and keeping all statuses forever will steadily retain memory.
	defaultStatusMax = 200
	defaultStatusTTL = 24 * time.Hour
)

func (s *Service) pruneStatus(now time.Time) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	max := s.statusMax
	if max <= 0 {
		max = defaultStatusMax
	}
	ttl := s.statusTTL
	if ttl <= 0 {
		ttl = defaultStatusTTL
	}

	if len(s.status) == 0 {
		return
	}

	// 1) Drop completed jobs older than TTL.
	for id, st := range s.status {
		if st == nil {
			delete(s.status, id)
			continue
		}
		// Prefer DoneAt for completed jobs; fallback to CreatedAt/StartedAt.
		reference := st.DoneAt
		if reference.IsZero() {
			reference = st.CreatedAt
			if reference.IsZero() {
				reference = st.StartedAt
			}
		}
		if !reference.IsZero() && now.Sub(reference) > ttl {
			delete(s.status, id)
		}
	}

	if len(s.status) <= max {
		return
	}

	// 2) Still too big: drop oldest by DoneAt (or StartedAt for running jobs).
	type kv struct {
		id string
		t  time.Time
	}

	items := make([]kv, 0, len(s.status))
	for id, st := range s.status {
		if st == nil {
			continue
		}
		t := st.DoneAt
		if t.IsZero() {
			t = st.StartedAt
		}
		items = append(items, kv{id: id, t: t})
	}

	sort.Slice(items, func(i, j int) bool { return items[i].t.Before(items[j].t) })

	excess := len(s.status) - max
	for i := 0; i < excess && i < len(items); i++ {
		delete(s.status, items[i].id)
	}
}
