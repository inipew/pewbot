package broadcast

import (
	"context"
	"fmt"
	logx "pewbot/pkg/logx"
	"time"

	kit "pewbot/internal/transport"
)

func (s *Service) NewJob(name string, targets []kit.ChatTarget, text string, opt *kit.SendOptions) string {
	now := time.Now()
	id := fmt.Sprintf("bc:%d", now.UnixNano())
	s.pruneStatus(now)
	st := &JobStatus{ID: id, Name: name, Total: len(targets), CreatedAt: now}
	s.statusMu.Lock()
	s.status[id] = st
	s.statusMu.Unlock()

	s.mu.Lock()
	q := s.queue
	s.mu.Unlock()
	if q != nil {
		select {
		case q <- job{id: id, name: name, targets: targets, text: text, opt: opt}:
			s.log.Debug("broadcast job enqueued", logx.String("job", id), logx.String("name", name), logx.Int("total", len(targets)), logx.Int("queue_len", len(q)), logx.Int("queue_cap", cap(q)))
		default:
			s.log.Warn("broadcast queue full; dropping job", logx.String("job", id), logx.String("name", name), logx.Int("queue_len", len(q)), logx.Int("queue_cap", cap(q)))
			s.statusMu.Lock()
			if st := s.status[id]; st != nil {
				st.DoneAt = time.Now()
				st.Running = false
				st.Failed = st.Total
			}
			s.statusMu.Unlock()
		}
	} else {
		s.log.Debug("broadcast not running; dropping job", logx.String("job", id), logx.String("name", name))
		s.statusMu.Lock()
		if st := s.status[id]; st != nil {
			st.DoneAt = time.Now()
			st.Running = false
			st.Failed = st.Total
		}
		s.statusMu.Unlock()
	}
	return id
}

func (s *Service) StartJob(ctx context.Context, jobID string) error {
	// In this starter, NewJob already enqueued. Keep API for plugin convenience.
	return nil
}

func (s *Service) Status(jobID string) (any, bool) {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	st, ok := s.status[jobID]
	if !ok || st == nil {
		return nil, false
	}
	cp := *st
	if len(st.Failures) > 0 {
		cp.Failures = append([]kit.ChatTarget(nil), st.Failures...)
	}
	return cp, true
}
