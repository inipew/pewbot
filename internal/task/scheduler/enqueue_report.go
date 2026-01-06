package scheduler

import (
	"errors"
	"time"

	"pewbot/internal/task/engine"
	logx "pewbot/pkg/logx"
)

const enqueueWarnThrottle = 5 * time.Second

func (s *Service) reportEnqueueError(name string, err error) {
	if err == nil {
		return
	}
	// Overlap skips can happen during normal operation.
	if errors.Is(err, engine.ErrOverlapSkip) {
		if !s.log.IsZero() {
			s.log.Debug("schedule trigger skipped", logx.String("schedule", name), logx.Any("err", err))
		}
		return
	}

	now := time.Now()
	s.enqMu.Lock()
	if s.lastEnqWarn == nil {
		s.lastEnqWarn = make(map[string]time.Time)
	}
	last := s.lastEnqWarn[name]
	if !last.IsZero() && now.Sub(last) < enqueueWarnThrottle {
		s.enqMu.Unlock()
		return
	}
	s.lastEnqWarn[name] = now
	s.enqMu.Unlock()

	if s.log.IsZero() {
		return
	}

	// Queue full / stopping are important but can be bursty.
	s.log.Warn("schedule failed to enqueue task", logx.String("schedule", name), logx.Any("err", err))
}
