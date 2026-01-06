package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"time"

	"pewbot/internal/eventbus"
	logx "pewbot/pkg/logx"
)

func (s *Service) worker(ctx context.Context, stopCh <-chan struct{}, queue chan queuedTask, idx int) {
	// Per-worker RNG: avoids global lock contention when many tasks retry concurrently.
	seed := time.Now().UnixNano() ^ (int64(idx) << 32)
	rng := rand.New(rand.NewSource(seed))

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
		case t, ok := <-queue:
			if !ok {
				// Queue is not expected to close in normal operation, but handle it defensively.
				return
			}
			// Dequeue first, then wait for a concurrency permit.
			atomic.AddInt32(&s.waitingForPermit, 1)
			if !s.acquirePermit(ctx, stopCh) {
				atomic.AddInt32(&s.waitingForPermit, -1)
				// If overlap gating was acquired at enqueue time, release it so future runs are not blocked.
				if t.track && t.state != nil {
					t.state.release()
				}
				return
			}
			atomic.AddInt32(&s.waitingForPermit, -1)

			// Optional concurrency-group limiting (per ConcurrencyKey).
			var releaseGroup func()
			if t.opt.ConcurrencyLimit > 0 {
				key := groupKey(t.task.ConcurrencyKey, t.task.Name)
				gs := s.groups.get(key, t.opt.ConcurrencyLimit)
				if gs != nil {
					if !gs.tryAcquire() {
						// Group is at capacity: put the task back and try other work.
						s.releasePermit()
						select {
						case <-ctx.Done():
							if t.track && t.state != nil {
								t.state.release()
							}
							return
						case <-stopCh:
							if t.track && t.state != nil {
								t.state.release()
							}
							return
						case queue <- t:
						default:
							// Should be rare since we just dequeued, but handle defensively.
							if t.track && t.state != nil {
								t.state.release()
							}
							s.onQueueFullDropped(time.Now(), t.task, queue, s.log, s.bus)
						}
						runtime.Gosched()
						continue
					}
					releaseGroup = gs.release
				}
			}

			atomic.AddInt32(&s.inFlight, 1)
			s.execOne(ctx, stopCh, t, rng)
			atomic.AddInt32(&s.inFlight, -1)
			if releaseGroup != nil {
				releaseGroup()
			}
			s.releasePermit()
		}
	}
}

func (s *Service) execOne(ctx context.Context, stopCh <-chan struct{}, qt queuedTask, rng *rand.Rand) {
	start := time.Now()
	queueDelay := time.Duration(0)
	if !qt.enqueuedAt.IsZero() {
		queueDelay = start.Sub(qt.enqueuedAt)
		if queueDelay < 0 {
			queueDelay = 0
		}
	}

	// Copy config early for stale-queue checks and race-free history trimming.
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()

	if cfg.MaxQueueDelay > 0 && queueDelay > cfg.MaxQueueDelay {
		if qt.track && qt.state != nil {
			qt.state.release()
		}
		s.onStaleDropped(start, qt.task, queueDelay)

		item := HistoryItem{ID: qt.task.ID, Name: qt.task.Name, Started: start, QueueDelay: queueDelay, Duration: 0, Error: "stale_queue_delay"}
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
		return
	}

	s.log.Debug("task.started", logx.String("task", qt.task.Name), logx.Duration("queue_delay", queueDelay))

	if s.bus != nil {
		s.bus.Publish(eventbus.Event{Type: "task.started", Time: start, Data: TaskEvent{ID: qt.task.ID, Name: qt.task.Name, Started: start, QueueDelay: queueDelay}})
	}
	if qt.track && qt.state != nil {
		defer qt.state.release()
	}

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
		// Guard against task panics: convert to error so one bad task can't crash the whole bot
		// or permanently kill a worker.
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
					s.log.Error("task.panic", logx.String("task", qt.task.Name), logx.Any("panic", r), logx.String("stack", string(debug.Stack())))
				}
			}()
			err = qt.task.Run(runCtx)
		}()
		if cancel != nil {
			cancel()
		}
		if err == nil {
			break
		}
		// Allow tasks to mark failures as non-retryable.
		var nr noRetryError
		if errors.As(err, &nr) {
			err = nr.err
			break
		}
		if attempt >= maxAttempts {
			break
		}

		delay := backoffDelayWithHint(qt.opt, attempt, err, rng)
		if delay > 0 {
			s.log.Debug("task retry scheduled", logx.String("task", qt.task.Name), logx.Int("attempt", attempt+1), logx.Duration("delay", delay), logx.Any("err", err))
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
	item := HistoryItem{ID: qt.task.ID, Name: qt.task.Name, Started: start, Duration: dur, QueueDelay: queueDelay}
	if err != nil {
		item.Error = err.Error()
		s.log.Warn("task.failed", logx.String("task", qt.task.Name), logx.Any("err", err), logx.Duration("queue_delay", queueDelay), logx.Duration("dur", dur), logx.Int("attempts", attempts))
		if s.bus != nil {
			s.bus.Publish(eventbus.Event{Type: "task.failed", Time: time.Now(), Data: TaskEvent{ID: qt.task.ID, Name: qt.task.Name, Started: start, QueueDelay: queueDelay, Duration: dur, Attempts: attempts, Error: item.Error}})
		}
	} else {
		if dur >= 750*time.Millisecond {
			s.log.Info("task.completed", logx.String("task", qt.task.Name), logx.Duration("queue_delay", queueDelay), logx.Duration("dur", dur), logx.Int("attempts", attempts))
		} else {
			s.log.Debug("task.completed", logx.String("task", qt.task.Name), logx.Duration("queue_delay", queueDelay), logx.Duration("dur", dur), logx.Int("attempts", attempts))
		}
		if s.bus != nil {
			s.bus.Publish(eventbus.Event{Type: "task.finished", Time: time.Now(), Data: TaskEvent{ID: qt.task.ID, Name: qt.task.Name, Started: start, QueueDelay: queueDelay, Duration: dur, Attempts: attempts}})
		}
	}

	// Update circuit breaker state based on final result (after retries).
	finish := time.Now()
	s.circuitRecordResult(finish, qt.task.Name, cfg, qt.opt, err)

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

func backoffDelayWithHint(opt TaskOptions, retry int, err error, rng *rand.Rand) time.Duration {
	// Respect explicit retry-after hints if provided by the task.
	var ra RetryAfterError
	if err != nil && errors.As(err, &ra) {
		d := ra.RetryAfter()
		if d < 0 {
			d = 0
		}
		maxD := opt.RetryMaxDelay
		if maxD <= 0 {
			maxD = 15 * time.Second
		}
		if d > maxD {
			d = maxD
		}
		// Apply the configured jitter on top of the hint to avoid thundering herds.
		j := opt.RetryJitter
		if j <= 0 {
			j = 0.2
		}
		if j > 0 && d > 0 && rng != nil {
			r := (rng.Float64()*2 - 1) * j
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
	return backoffDelay(opt, retry, rng)
}

func backoffDelay(opt TaskOptions, retry int, rng *rand.Rand) time.Duration {
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
	if j > 0 && rng != nil {
		r := (rng.Float64()*2 - 1) * j
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
