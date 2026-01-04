package taskengine

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"pewbot/internal/eventbus"
)

func (s *Service) worker(ctx context.Context, stopCh <-chan struct{}, queue <-chan queuedTask, idx int) {
	for {
		// Fast-exit check so a closed stopCh wins over queued work.
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
		case t := <-queue:
			s.execOne(ctx, stopCh, t)
		}
	}
}

func (s *Service) execOne(ctx context.Context, stopCh <-chan struct{}, qt queuedTask) {
	start := time.Now()
	if s.bus != nil {
		s.bus.Publish(eventbus.Event{Type: "task.started", Time: start, Data: TaskEvent{ID: qt.task.ID, Name: qt.task.Name, Started: start}})
	}
	if qt.track && qt.state != nil {
		defer qt.state.release()
	}

	// Copy config for race-free history trimming.
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()

	retries := qt.opt.RetryMax
	if retries < 0 {
		retries = 0
	}

	var err error
	attempts := 0
	maxAttempts := 1 + retries
attemptLoop:
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attempts = attempt

		runCtx := ctx
		var cancel func()
		if qt.timeout > 0 {
			runCtx, cancel = context.WithTimeout(ctx, qt.timeout)
		}
		err = qt.task.Run(runCtx)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			break
		}
		if attempt >= maxAttempts {
			break
		}

		delay := backoffDelay(qt.opt, attempt)
		if delay > 0 {
			s.log.Debug("task retry scheduled", slog.String("task", qt.task.Name), slog.Int("attempt", attempt+1), slog.Duration("delay", delay), slog.Any("err", err))
			tmr := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				if !tmr.Stop() {
					<-tmr.C
				}
				err = ctx.Err()
				break attemptLoop
			case <-stopCh:
				if !tmr.Stop() {
					<-tmr.C
				}
				err = errors.New("taskengine stopped")
				break attemptLoop
			case <-tmr.C:
			}
		}
	}

	dur := time.Since(start)
	item := HistoryItem{ID: qt.task.ID, Name: qt.task.Name, Started: start, Duration: dur}
	if err != nil {
		item.Error = err.Error()
		s.log.Warn("task failed", slog.String("task", qt.task.Name), slog.Any("err", err), slog.Duration("dur", dur), slog.Int("attempts", attempts))
		if s.bus != nil {
			s.bus.Publish(eventbus.Event{Type: "task.failed", Time: time.Now(), Data: TaskEvent{ID: qt.task.ID, Name: qt.task.Name, Started: start, Duration: dur, Attempts: attempts, Error: item.Error}})
		}
	} else {
		if dur >= 750*time.Millisecond {
			s.log.Info("task completed", slog.String("task", qt.task.Name), slog.Duration("dur", dur), slog.Int("attempts", attempts))
		} else {
			s.log.Debug("task completed", slog.String("task", qt.task.Name), slog.Duration("dur", dur), slog.Int("attempts", attempts))
		}
		if s.bus != nil {
			s.bus.Publish(eventbus.Event{Type: "task.finished", Time: time.Now(), Data: TaskEvent{ID: qt.task.ID, Name: qt.task.Name, Started: start, Duration: dur, Attempts: attempts}})
		}
	}

	s.hmu.Lock()
	s.history = append(s.history, item)
	historySize := cfg.HistorySize
	if historySize <= 0 {
		historySize = 200
	}
	if len(s.history) > historySize {
		s.history = s.history[len(s.history)-historySize:]
	}
	s.hmu.Unlock()
}

func backoffDelay(opt TaskOptions, retry int) time.Duration {
	base := opt.RetryBase
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	maxD := opt.RetryMaxDelay
	if maxD <= 0 {
		maxD = 15 * time.Second
	}
	j := opt.RetryJitter
	if j <= 0 {
		j = 0.2
	}

	d := base
	for i := 1; i < retry; i++ {
		d *= 2
		if d > maxD {
			d = maxD
			break
		}
	}
	if j > 0 {
		r := (randFloat64()*2 - 1) * j
		d = time.Duration(float64(d) * (1 + r))
		if d < 0 {
			d = 0
		}
	}
	if d > maxD {
		d = maxD
	}
	return d
}

var rngMu sync.Mutex
var rngOnce sync.Once

func randFloat64() float64 {
	rngOnce.Do(func() { rand.Seed(time.Now().UnixNano()) })
	rngMu.Lock()
	defer rngMu.Unlock()
	return rand.Float64()
}
