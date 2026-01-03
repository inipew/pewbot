package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"pewbot/internal/eventbus"
)

func (s *Service) enqueue(t task) {
	s.mu.Lock()
	q := s.queue
	s.mu.Unlock()
	if q == nil {
		s.log.Debug("scheduler not running; dropping task", slog.String("task", t.name))
		return
	}
	select {
	case q <- t:
		// ok
	default:
		s.log.Warn("scheduler queue full; dropping task", slog.String("task", t.name), slog.Int("queue_len", len(q)), slog.Int("queue_cap", cap(q)))
	}
}

func (s *Service) worker(ctx context.Context, stopCh <-chan struct{}, queue <-chan task, idx int) {
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

func (s *Service) execOne(ctx context.Context, stopCh <-chan struct{}, t task) {
	start := time.Now()
	if s.bus != nil {
		s.bus.Publish(eventbus.Event{Type: "task.started", Time: start, Data: TaskEvent{ID: t.id, Name: t.name, Started: start}})
	}

	// Mark running for overlap control (shared state between cron invocations).
	if t.state != nil {
		t.state.mu.Lock()
		t.state.running = true
		t.state.mu.Unlock()
		defer func() {
			t.state.mu.Lock()
			t.state.running = false
			t.state.mu.Unlock()
		}()
	}

	// Copy scheduler config to avoid data races with Apply().
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()

	opt := t.opt.withDefaults(cfg)
	retries := opt.RetryMax
	if retries < 0 {
		retries = 0
	}

	var err error
	attempts := 0
	maxAttempts := 1 + retries
attemptLoop:
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attempts = attempt
		// Per-attempt timeout (so a timed-out first attempt doesn't poison retries).
		runCtx := ctx
		var cancel func()
		if t.timeout > 0 {
			runCtx, cancel = context.WithTimeout(ctx, t.timeout)
		}
		err = t.run(runCtx)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			break
		}
		if attempt >= maxAttempts {
			break
		}

		delay := backoffDelay(opt, attempt) // attempt=1 => first retry
		if delay > 0 {
			s.log.Debug("task retry scheduled", slog.String("task", t.name), slog.Int("attempt", attempt+1), slog.Duration("delay", delay), slog.Any("err", err))
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
				err = errors.New("scheduler stopped")
				break attemptLoop
			case <-tmr.C:
			}
		}
	}

	item := HistoryItem{
		ID:       t.id,
		Name:     t.name,
		Started:  start,
		Duration: time.Since(start),
	}
	dur := time.Since(start)
	if err != nil {
		item.Error = err.Error()
		s.log.Warn("task failed", slog.String("task", t.name), slog.Any("err", err), slog.Duration("dur", dur), slog.Int("attempts", attempts))
		if s.bus != nil {
			s.bus.Publish(eventbus.Event{Type: "task.failed", Time: time.Now(), Data: TaskEvent{ID: t.id, Name: t.name, Started: start, Duration: dur, Attempts: attempts, Error: item.Error}})
		}
	} else {
		// Avoid noisy logs for very frequent tasks: only elevate to INFO when it took noticeable time.
		if dur >= 750*time.Millisecond {
			s.log.Info("task completed", slog.String("task", t.name), slog.Duration("dur", dur), slog.Int("attempts", attempts))
		} else {
			s.log.Debug("task completed", slog.String("task", t.name), slog.Duration("dur", dur), slog.Int("attempts", attempts))
		}
		if s.bus != nil {
			s.bus.Publish(eventbus.Event{Type: "task.finished", Time: time.Now(), Data: TaskEvent{ID: t.id, Name: t.name, Started: start, Duration: dur, Attempts: attempts}})
		}
	}

	s.hmu.Lock()
	defer s.hmu.Unlock()
	s.history = append(s.history, item)
	historySize := cfg.HistorySize
	// Safety: a zero/negative history_size previously meant unbounded growth.
	// That can slowly retain memory on long-running bots, so we default to a sensible cap.
	if historySize <= 0 {
		historySize = 200
	}
	if len(s.history) > historySize {
		s.history = s.history[len(s.history)-historySize:]
	}
}

func parseHHMM(s string) (hour int, minute int, err error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid time %q, expected HH:MM", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, 0, fmt.Errorf("invalid hour in %q", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("invalid minute in %q", s)
	}
	return h, m, nil
}

func backoffDelay(opt TaskOptions, retry int) time.Duration {
	// retry starts at 1 (first retry)
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
	// exp growth
	d := base
	for i := 1; i < retry; i++ {
		d *= 2
		if d > maxD {
			d = maxD
			break
		}
	}
	// jitter [1-j, 1+j]
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
