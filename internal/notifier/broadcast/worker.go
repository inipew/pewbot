package broadcast

import (
	"context"
	logx "pewbot/pkg/logx"
	"time"

	kit "pewbot/internal/transport"
)

func (s *Service) worker(ctx context.Context, stopCh <-chan struct{}, queue <-chan job, idx int) {
	for {
		// fast-exit so stop wins over queued work
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		default:
		}

		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case j := <-queue:
			s.execJob(ctx, j)
		}
	}
}

func (s *Service) execJob(ctx context.Context, j job) {
	start := time.Now()
	s.setRunning(j.id, true)
	defer s.setRunning(j.id, false)

	s.log.Info("broadcast job started", logx.String("job", j.id), logx.String("name", j.name), logx.Int("total", len(j.targets)))

	for _, t := range j.targets {
		if err := s.sendOne(ctx, j.id, j.name, t, j.text, j.opt); err != nil {
			s.markFail(j.id, t)
		}
		s.markDone(j.id)
	}
	s.finish(j.id)

	// Final summary.
	stAny, ok := s.Status(j.id)
	if ok {
		if st, ok2 := stAny.(JobStatus); ok2 {
			fields := []logx.Field{
				logx.String("job", j.id),
				logx.String("name", j.name),
				logx.Int("total", st.Total),
				logx.Int("failed", st.Failed),
				logx.Duration("dur", time.Since(start)),
			}
			if st.Failed > 0 {
				s.log.Warn("broadcast job finished with failures", fields...)
			} else {
				s.log.Info("broadcast job finished", fields...)
			}
			return
		}
	}
	s.log.Info("broadcast job finished", logx.String("job", j.id), logx.String("name", j.name), logx.Duration("dur", time.Since(start)))
}

func (s *Service) sendOne(ctx context.Context, jobID, jobName string, t kit.ChatTarget, text string, opt *kit.SendOptions) error {
	// Snapshot mutable dependencies to avoid races with Apply().
	s.mu.Lock()
	lim := s.limiter
	retry := s.cfg.RetryMax
	adapter := s.adapter
	s.mu.Unlock()

	if lim != nil {
		if err := lim.Wait(ctx); err != nil {
			return err
		}
	}
	var last error
	for i := 0; i <= retry; i++ {
		_, err := adapter.SendText(ctx, t, text, opt)
		if err == nil {
			return nil
		}
		last = err
		if i == retry {
			break
		}
		delay := time.Duration(200+100*i) * time.Millisecond
		s.log.Debug("broadcast send retry scheduled", logx.String("job", jobID), logx.String("name", jobName), logx.Int64("chat_id", t.ChatID), logx.Int("attempt", i+2), logx.Duration("delay", delay), logx.Any("err", err))
		tmr := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !tmr.Stop() {
				<-tmr.C
			}
			return ctx.Err()
		case <-tmr.C:
		}
	}
	if last != nil {
		s.log.Warn("broadcast send failed", logx.String("job", jobID), logx.String("name", jobName), logx.Int64("chat_id", t.ChatID), logx.Int("thread_id", t.ThreadID), logx.Any("err", last))
	}
	return last
}

func (s *Service) setRunning(id string, v bool) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if st := s.status[id]; st != nil {
		if v {
			st.StartedAt = time.Now()
			st.Running = true
		}
	}
}
func (s *Service) markDone(id string) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if st := s.status[id]; st != nil {
		st.Done++
	}
}
func (s *Service) markFail(id string, t kit.ChatTarget) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if st := s.status[id]; st != nil {
		st.Failed++
		if len(st.Failures) < 200 {
			st.Failures = append(st.Failures, t)
		}
	}
}
func (s *Service) finish(id string) {
	now := time.Now()
	s.statusMu.Lock()
	if st := s.status[id]; st != nil {
		st.DoneAt = now
		st.Running = false
	}
	s.statusMu.Unlock()
	// Keep the map bounded even if nobody queries old job IDs.
	s.pruneStatus(now)
}
