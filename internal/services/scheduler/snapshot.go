package scheduler

import (
	"time"
)

func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	enabled := s.cfg.Enabled
	tz := s.cfg.Timezone
	workers := s.cfg.Workers
	ql := 0
	if s.queue != nil {
		ql = len(s.queue)
	}
	defs := make([]scheduleDef, len(s.defs))
	copy(defs, s.defs)
	c := s.c
	loc := s.loc
	s.mu.Unlock()

	if loc == nil {
		loc = time.Local
	}
	if tz == "" && loc != nil {
		tz = loc.String()
	}

	items := make([]ScheduleInfo, 0, len(defs))
	for _, d := range defs {
		it := ScheduleInfo{ID: d.id, Name: d.name, Spec: d.spec, Timeout: d.timeout}
		if c != nil && d.entryID != 0 {
			e := c.Entry(d.entryID)
			it.Next = e.Next
			it.Prev = e.Prev
		}
		items = append(items, it)
	}

	s.hmu.Lock()
	hist := make([]HistoryItem, len(s.history))
	copy(hist, s.history)
	s.hmu.Unlock()

	return Snapshot{
		Enabled:   enabled,
		Timezone:  tz,
		Workers:   workers,
		QueueLen:  ql,
		Schedules: items,
		History:   hist,
	}
}
