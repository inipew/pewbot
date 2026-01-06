package notifier

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	logx "pewbot/pkg/logx"
	"sync"
	"time"

	"pewbot/internal/eventbus"
	rtsup "pewbot/internal/runtime/supervisor"
	"pewbot/internal/storage"
	kit "pewbot/internal/transport"

	"golang.org/x/time/rate"
)

var (
	ErrDisabled  = errors.New("notifier disabled")
	ErrQueueFull = errors.New("notifier queue full")
	ErrStopped   = errors.New("notifier stopped")
)

type job struct {
	n kit.Notification
	// dedupKey is computed at enqueue time for cheap per-worker processing.
	dedupKey string
}

// Service implements an async notification pipeline:
// queue + worker pool + rate limit + retry + dedup.
//
// It is safe for concurrent use.
type Service struct {
	mu sync.Mutex

	log     logx.Logger
	adapter kit.Adapter
	bus     eventbus.Bus
	store   storage.Store

	cfg     Config
	limiter *rate.Limiter

	accepting bool
	sendWG    sync.WaitGroup

	queue    chan job
	sup      *rtsup.Supervisor
	stopDone chan struct{} // non-nil while stopping

	// In-memory dedup cache: key -> suppress until
	dmu   sync.Mutex
	dedup map[string]time.Time

	// Optional persistent dedup writes (best-effort)
	persistCh chan dedupWrite

	// In-memory history (for /status)
	hmu     sync.Mutex
	history []HistoryItem
}

// Supervisor returns the notifier's internal supervisor (nil if not started).
// This is used for operational visibility (e.g. /health).
func (s *Service) Supervisor() *rtsup.Supervisor {
	s.mu.Lock()
	sup := s.sup
	s.mu.Unlock()
	return sup
}

type dedupWrite struct {
	key   string
	until time.Time
}

func New(cfg Config, adapter kit.Adapter, log logx.Logger, bus eventbus.Bus, store storage.Store) *Service {
	if log.IsZero() {
		log = logx.Nop()
	}
	s := &Service{
		adapter: adapter,
		log:     log,
		bus:     bus,
		store:   store,
		dedup:   map[string]time.Time{},
	}
	s.applyLocked(cfg)
	return s
}

func (s *Service) Enabled() bool {
	s.mu.Lock()
	en := s.cfg.Enabled
	s.mu.Unlock()
	return en
}

func (s *Service) Apply(cfg Config) {
	s.mu.Lock()
	s.applyLocked(cfg)
	s.mu.Unlock()
}

func (s *Service) applyLocked(cfg Config) {
	// Defaults
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 512
	}
	if cfg.RatePerSec <= 0 {
		cfg.RatePerSec = 3
	}
	if cfg.RetryMax < 0 {
		cfg.RetryMax = 0
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 500 * time.Millisecond
	}
	if cfg.RetryMaxDelay <= 0 {
		cfg.RetryMaxDelay = 10 * time.Second
	}
	if cfg.DedupWindow < 0 {
		cfg.DedupWindow = 0
	}
	if cfg.DedupMaxEntries <= 0 {
		cfg.DedupMaxEntries = 2000
	}

	s.cfg = cfg
	// Token bucket: burst = rate per sec, so short spikes don't block too hard.
	s.limiter = rate.NewLimiter(rate.Limit(cfg.RatePerSec), cfg.RatePerSec)
}

func (s *Service) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	// Start is idempotent.
	s.mu.Lock()
	// If stopping, wait for it to finish before restarting.
	if s.stopDone != nil {
		done := s.stopDone
		s.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
		s.mu.Lock()
	}
	if s.queue != nil {
		s.mu.Unlock()
		return
	}
	if !s.cfg.Enabled {
		s.mu.Unlock()
		return
	}

	s.queue = make(chan job, s.cfg.QueueSize)
	s.accepting = true
	workers := s.cfg.Workers
	if workers <= 0 {
		workers = 2
	}

	// Optional persistent dedup writes.
	if s.cfg.PersistDedup && s.store != nil {
		s.persistCh = make(chan dedupWrite, 1024)
	}

	s.sup = rtsup.NewSupervisor(ctx,
		rtsup.WithLogger(s.log.With(logx.String("comp", "notifier"))),
		// notifier failures should not take down the whole app; treat as best-effort.
		rtsup.WithCancelOnError(false),
	)
	sup := s.sup
	q := s.queue
	pch := s.persistCh
	st := s.store
	s.mu.Unlock()

	if pch != nil {
		sup.GoRestart("dedup.persist", func(c context.Context) error {
			s.persistLoop(c, pch, st)
			// Clean exits happen on shutdown.
			s.mu.Lock()
			stopping := s.stopDone != nil
			s.mu.Unlock()
			if stopping {
				return context.Canceled
			}
			if c.Err() != nil {
				return c.Err()
			}
			return errors.New("notifier persist loop exited unexpectedly")
		}, rtsup.WithPublishFirstError(true))
	}

	for i := 0; i < workers; i++ {
		idx := i
		name := fmt.Sprintf("worker.%d", idx)
		sup.GoRestart(name, func(c context.Context) error {
			s.workerLoop(c, q, idx)
			// Clean exits happen on shutdown (queue close).
			s.mu.Lock()
			stopping := s.stopDone != nil
			s.mu.Unlock()
			if stopping {
				return context.Canceled
			}
			if c.Err() != nil {
				return c.Err()
			}
			return errors.New("notifier worker exited unexpectedly")
		}, rtsup.WithPublishFirstError(true))
	}
}

// Stop stops intake and drains the queue best-effort until ctx deadline.
func (s *Service) Stop(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	q := s.queue
	pch := s.persistCh
	sup := s.sup
	// If not running, nothing to do.
	if q == nil {
		s.mu.Unlock()
		return
	}
	// If already stopping, wait.
	if s.stopDone != nil {
		done := s.stopDone
		s.mu.Unlock()
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}

	done := make(chan struct{})
	s.stopDone = done
	// Block new notifies.
	s.accepting = false
	s.mu.Unlock()

	// Shutdown happens asynchronously so callers can time out without leaking state.
	go func() {
		defer close(done)
		// Wait for in-flight enqueues to finish, then close the queue so workers can drain.
		s.sendWG.Wait()
		if pch != nil {
			func() {
				defer func() { _ = recover() }()
				close(pch)
			}()
		}
		func() {
			defer func() { _ = recover() }()
			close(q)
		}()
		if sup != nil {
			_ = sup.Wait(context.Background())
		}

		s.mu.Lock()
		s.queue = nil
		s.persistCh = nil
		s.stopDone = nil
		s.sup = nil
		s.mu.Unlock()
	}()

	select {
	case <-done:
		return
	case <-ctx.Done():
		// Force-stop internal loops.
		if sup != nil {
			sup.Cancel()
		}
		return
	}
}

func (s *Service) Notify(ctx context.Context, n kit.Notification) error {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}

	s.mu.Lock()
	if !s.cfg.Enabled {
		s.mu.Unlock()
		return ErrDisabled
	}
	if !s.accepting || s.queue == nil {
		s.mu.Unlock()
		return ErrStopped
	}
	q := s.queue
	// Capture current config snapshot for dedup computation.
	dedupWindow := s.cfg.DedupWindow
	dedupMax := s.cfg.DedupMaxEntries
	persistDedup := s.cfg.PersistDedup
	st := s.store
	pch := s.persistCh
	s.sendWG.Add(1)
	s.mu.Unlock()
	defer s.sendWG.Done()

	// Build dedup key and apply suppression.
	key := dedupKey(n)
	if dedupWindow > 0 && key != "" {
		if !s.dedupAllow(ctx, key, dedupWindow, dedupMax, persistDedup, st, pch) {
			if s.bus != nil {
				now := time.Now()
				s.bus.Publish(eventbus.Event{Type: "notifier.deduped", Time: now, Data: NotificationEvent{Channel: n.Channel, ChatID: n.Target.ChatID, ThreadID: n.Target.ThreadID, Key: key, At: now}})
			}
			return nil
		}
	}

	if s.bus != nil {
		now := time.Now()
		s.bus.Publish(eventbus.Event{Type: "notifier.queued", Time: now, Data: NotificationEvent{Channel: n.Channel, ChatID: n.Target.ChatID, ThreadID: n.Target.ThreadID, Key: key, At: now}})
	}

	select {
	case q <- job{n: n, dedupKey: key}:
		return nil
	default:
		if s.bus != nil {
			now := time.Now()
			s.bus.Publish(eventbus.Event{Type: "notifier.dropped", Time: now, Data: NotificationEvent{Channel: n.Channel, ChatID: n.Target.ChatID, ThreadID: n.Target.ThreadID, Key: key, At: now, Error: ErrQueueFull.Error()}})
		}
		return ErrQueueFull
	}
}

func (s *Service) Snapshot() []HistoryItem {
	s.hmu.Lock()
	out := append([]HistoryItem(nil), s.history...)
	s.hmu.Unlock()
	return out
}

func (s *Service) appendHistory(text string) {
	s.hmu.Lock()
	s.history = append(s.history, HistoryItem{At: time.Now(), Text: text})
	if len(s.history) > 300 {
		s.history = s.history[len(s.history)-300:]
	}
	s.hmu.Unlock()
}

func (s *Service) persistLoop(ctx context.Context, ch <-chan dedupWrite, st storage.Store) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ch == nil || st == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case w, ok := <-ch:
			if !ok {
				return
			}
			cctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			_ = st.PutDedup(cctx, w.key, w.until)
			cancel()
		}
	}
}

func (s *Service) workerLoop(ctx context.Context, q <-chan job, idx int) {
	_ = idx // kept for future per-worker metrics
	if ctx == nil {
		ctx = context.Background()
	}
	if q == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case j, ok := <-q:
			if !ok {
				return
			}
			s.sendWithRetry(ctx, j)
		}
	}
}

func (s *Service) sendWithRetry(runCtx context.Context, j job) {
	// config snapshot for this send
	s.mu.Lock()
	cfg := s.cfg
	lim := s.limiter
	ad := s.adapter
	log := s.log
	bus := s.bus
	s.mu.Unlock()

	if ad == nil {
		return
	}

	// Ensure message is tagged by priority.
	text := prefixForPriority(j.n.Priority) + j.n.Text
	if text == "" {
		return
	}

	maxAttempts := 1
	if cfg.RetryMax > 0 {
		maxAttempts = 1 + cfg.RetryMax
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Rate limit (honor cancellation).
		if lim != nil {
			wctx := runCtx
			if wctx == nil {
				wctx = context.Background()
			}
			if err := lim.Wait(wctx); err != nil {
				return
			}
		}

		// Bound per-send call. Keep tight to avoid hanging workers.
		callCtx := runCtx
		if callCtx == nil {
			callCtx = context.Background()
		}
		callCtx, cancel := context.WithTimeout(callCtx, 10*time.Second)
		_, err := ad.SendText(callCtx, j.n.Target, text, j.n.Options)
		cancel()
		if err == nil {
			s.appendHistory(text)
			if bus != nil {
				now := time.Now()
				bus.Publish(eventbus.Event{Type: "notifier.sent", Time: now, Data: NotificationEvent{Channel: j.n.Channel, ChatID: j.n.Target.ChatID, ThreadID: j.n.Target.ThreadID, Key: j.dedupKey, At: now}})
			}
			return
		}
		lastErr = err
		log.Debug("notify send failed", logx.Any("err", err), logx.Int("attempt", attempt), logx.Int("max", maxAttempts))

		if attempt >= maxAttempts {
			break
		}

		delay := retryDelay(cfg, attempt)
		if delay <= 0 {
			continue
		}
		t := time.NewTimer(delay)
		rc := runCtx
		if rc == nil {
			rc = context.Background()
		}
		select {
		case <-t.C:
		case <-rc.Done():
			if !t.Stop() {
				<-t.C
			}
			return
		}
	}

	if lastErr != nil && bus != nil {
		now := time.Now()
		bus.Publish(eventbus.Event{Type: "notifier.failed", Time: now, Data: NotificationEvent{Channel: j.n.Channel, ChatID: j.n.Target.ChatID, ThreadID: j.n.Target.ThreadID, Key: j.dedupKey, At: now, Error: lastErr.Error()}})
	}
}

func prefixForPriority(p int) string {
	switch {
	case p >= 9:
		return "🚨 "
	case p >= 7:
		return "⚠️ "
	case p >= 5:
		return "ℹ️ "
	default:
		return ""
	}
}

func dedupKey(n kit.Notification) string {
	// If no target, don't dedup.
	if n.Channel == "" {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(n.Channel))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(fmt.Sprintf("%d:%d:%d|", n.Target.ChatID, n.Target.ThreadID, n.Priority)))
	_, _ = h.Write([]byte(n.Text))
	return fmt.Sprintf("%x", h.Sum64())
}

func (s *Service) dedupAllow(ctx context.Context, key string, window time.Duration, max int, persist bool, st storage.Store, pch chan dedupWrite) bool {
	now := time.Now()

	// 1) In-memory check.
	s.dmu.Lock()
	if s.dedup == nil {
		s.dedup = map[string]time.Time{}
	}
	if until, ok := s.dedup[key]; ok && now.Before(until) {
		s.dmu.Unlock()
		return false
	}
	s.dmu.Unlock()

	// 2) Persistent check (best-effort) for cross-restart dedup.
	if persist && st != nil {
		qctx := ctx
		if qctx == nil {
			qctx = context.Background()
		}
		cctx, cancel := context.WithTimeout(qctx, 25*time.Millisecond)
		until, ok, err := st.GetDedup(cctx, key)
		cancel()
		if err == nil && ok && now.Before(until) {
			s.dmu.Lock()
			s.dedup[key] = until
			s.dmu.Unlock()
			return false
		}
	}

	// 3) Allow and set new window.
	until := now.Add(window)
	s.dmu.Lock()
	s.dedup[key] = until

	// Prune expired and cap.
	if len(s.dedup) > 0 {
		for k, until := range s.dedup {
			if !now.Before(until) {
				delete(s.dedup, k)
			}
		}
	}
	if max > 0 && len(s.dedup) > max {
		// Remove entries with earliest expiry until within cap.
		for len(s.dedup) > max {
			var (
				minKey string
				minT   time.Time
				set    bool
			)
			for k, t := range s.dedup {
				if !set || t.Before(minT) {
					minKey, minT, set = k, t, true
				}
			}
			if minKey == "" {
				break
			}
			delete(s.dedup, minKey)
		}
	}
	s.dmu.Unlock()

	// 4) Persist new suppress-until asynchronously (best-effort).
	if persist && st != nil && pch != nil {
		select {
		case pch <- dedupWrite{key: key, until: until}:
		default:
		}
	}
	return true
}

func retryDelay(cfg Config, attempt int) time.Duration {
	// attempt starts at 1 (first attempt), delay is for the NEXT attempt.
	base := cfg.RetryBase
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	maxD := cfg.RetryMaxDelay
	if maxD <= 0 {
		maxD = 10 * time.Second
	}
	// Exponential backoff: base * 2^(attempt-1)
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= maxD {
			d = maxD
			break
		}
	}
	// Jitter 0.7..1.3
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	j := 0.7 + rng.Float64()*0.6
	d = time.Duration(float64(d) * j)
	if d < 0 {
		return 0
	}
	if d > maxD {
		d = maxD
	}
	return d
}
