package scheduler

import (
	"time"

	"pewbot/internal/services/taskengine"
)

func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	enabled := s.cfg.Enabled
	tz := s.cfg.Timezone
	defTimeout := s.cfg.DefaultTimeout
	retryMax := s.cfg.RetryMax
	defs := make([]scheduleDef, len(s.defs))
	copy(defs, s.defs)
	c := s.c
	loc := s.loc
	eng := s.engine
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

	workers := 0
	ql := 0
	qc := 0
	dropped := uint64(0)
	hist := []HistoryItem{}
	if eng != nil {
		es := eng.Snapshot()
		workers = es.Workers
		ql = es.QueueLen
		qc = es.QueueCap
		dropped = es.Dropped
		hist = es.History
	}

	// Surface effective retry defaults used by the executor.
	opt := taskengine.DefaultTaskOptions(taskengine.Config{RetryMax: retryMax})

	return Snapshot{
		Enabled:        enabled,
		Timezone:       tz,
		Workers:        workers,
		QueueLen:       ql,
		QueueCap:       qc,
		Dropped:        dropped,
		DefaultTimeout: defTimeout,
		RetryMax:       opt.RetryMax,
		RetryBase:      opt.RetryBase,
		RetryMaxDelay:  opt.RetryMaxDelay,
		RetryJitter:    opt.RetryJitter,
		Schedules:      items,
		History:        hist,
	}
}
