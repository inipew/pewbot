package scheduler

import (
	"time"

	"pewbot/internal/task/engine"
)

func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	enabled := s.cfg.Enabled
	tz := s.cfg.Timezone
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
	activeMax := 0
	activeLimit := 0
	inFlight := 0
	waiting := 0
	ql := 0
	qc := 0
	dropped := uint64(0)
	droppedQF := uint64(0)
	droppedStale := uint64(0)
	defTimeout := time.Duration(0)
	maxQueueDelay := time.Duration(0)
	retryMax := 0
	hist := []HistoryItem{}
	if eng != nil {
		es := eng.Snapshot()
		workers = es.Workers
		activeMax = es.ActiveMax
		activeLimit = es.ActiveLimit
		inFlight = es.InFlight
		waiting = es.WaitingForPermit
		ql = es.QueueLen
		qc = es.QueueCap
		dropped = es.Dropped
		droppedQF = es.DroppedQueueFull
		droppedStale = es.DroppedStale
		defTimeout = es.DefaultTimeout
		maxQueueDelay = es.MaxQueueDelay
		retryMax = es.RetryMax
		hist = es.History
	}

	// Surface effective retry defaults used by the executor.
	opt := engine.DefaultTaskOptions(engine.Config{RetryMax: retryMax})

	return Snapshot{
		Enabled:          enabled,
		Timezone:         tz,
		Workers:          workers,
		ActiveMax:        activeMax,
		ActiveLimit:      activeLimit,
		InFlight:         inFlight,
		WaitingForPermit: waiting,
		QueueLen:         ql,
		QueueCap:         qc,
		Dropped:          dropped,
		DroppedQueueFull: droppedQF,
		DroppedStale:     droppedStale,
		DefaultTimeout:   defTimeout,
		MaxQueueDelay:    maxQueueDelay,
		RetryMax:         opt.RetryMax,
		RetryBase:        opt.RetryBase,
		RetryMaxDelay:    opt.RetryMaxDelay,
		RetryJitter:      opt.RetryJitter,
		Schedules:        items,
		History:          hist,
	}
}
