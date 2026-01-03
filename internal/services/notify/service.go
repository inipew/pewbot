package notify

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand"
	"runtime/debug"
	"sync"
	"time"

	"pewbot/internal/eventbus"
	"pewbot/internal/kit"

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

	log     *slog.Logger
	adapter kit.Adapter
	bus     eventbus.Bus

	cfg     Config
	limiter *rate.Limiter

	accepting bool
	sendWG    sync.WaitGroup

	queue    chan job
	stopDone chan struct{}

	runCtx    context.Context
	runCancel context.CancelFunc
	workerWG  sync.WaitGroup

	// In-memory dedup cache: key -> suppress until
	dmu   sync.Mutex
	dedup map[string]time.Time

	// In-memory history (for /status)
	hmu     sync.Mutex
	history []HistoryItem
}

func New(cfg Config, adapter kit.Adapter, log *slog.Logger, bus eventbus.Bus) *Service {
	s := &Service{
		adapter: adapter,
		log:     log,
		bus:     bus,
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
	s.mu.Lock()
	if s.queue != nil {
		// already running
		s.mu.Unlock()
		return
	}
	if !s.cfg.Enabled {
		s.mu.Unlock()
		return
	}

	s.queue = make(chan job, s.cfg.QueueSize)
	s.accepting = true
	s.stopDone = make(chan struct{})
	s.runCtx, s.runCancel = context.WithCancel(ctx)
	workers := s.cfg.Workers
	s.mu.Unlock()

	for i := 0; i < workers; i++ {
		i := i
		s.workerWG.Add(1)
		go func() {
			defer s.workerWG.Done()
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("panic in notifier worker", slog.Int("worker", i), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
				}
			}()
			s.workerLoop(i)
		}()
	}
}

// Stop stops intake and drains the queue best-effort until ctx deadline.
func (s *Service) Stop(ctx context.Context) {
	s.mu.Lock()
	q := s.queue
	done := s.stopDone
	cancel := s.runCancel
	if q == nil {
		s.mu.Unlock()
		return
	}
	// Block new notifies.
	s.accepting = false
	s.mu.Unlock()

	// Wait for in-flight enqueues to finish, then close the queue so workers can drain.
	ch := make(chan struct{})
	go func() {
		s.sendWG.Wait()
		close(ch)
	}()
	select {
	case <-ctx.Done():
		if cancel != nil {
			cancel()
		}
		return
	case <-ch:
	}

	// Now it's safe to close the queue.
	func() {
		defer func() { _ = recover() }()
		close(q)
	}()

	// Wait for workers.
	go func() {
		s.workerWG.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		if cancel != nil {
			cancel()
		}
	case <-done:
		if cancel != nil {
			cancel()
		}
	}

	s.mu.Lock()
	s.queue = nil
	s.stopDone = nil
	s.runCancel = nil
	s.runCtx = nil
	s.mu.Unlock()
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
	s.sendWG.Add(1)
	s.mu.Unlock()
	defer s.sendWG.Done()

	// Build dedup key and apply suppression.
	key := dedupKey(n)
	if dedupWindow > 0 && key != "" {
		if !s.dedupAllow(key, dedupWindow, dedupMax) {
			if s.bus != nil {
				now := time.Now()
				s.bus.Publish(eventbus.Event{Type: "notify.deduped", Time: now, Data: NotificationEvent{Channel: n.Channel, ChatID: n.Target.ChatID, ThreadID: n.Target.ThreadID, Key: key, At: now}})
			}
			return nil
		}
	}

	if s.bus != nil {
		now := time.Now()
		s.bus.Publish(eventbus.Event{Type: "notify.queued", Time: now, Data: NotificationEvent{Channel: n.Channel, ChatID: n.Target.ChatID, ThreadID: n.Target.ThreadID, Key: key, At: now}})
	}

	select {
	case q <- job{n: n, dedupKey: key}:
		return nil
	default:
		if s.bus != nil {
			now := time.Now()
			s.bus.Publish(eventbus.Event{Type: "notify.dropped", Time: now, Data: NotificationEvent{Channel: n.Channel, ChatID: n.Target.ChatID, ThreadID: n.Target.ThreadID, Key: key, At: now, Error: ErrQueueFull.Error()}})
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

func (s *Service) workerLoop(idx int) {
	// Copy stable references.
	s.mu.Lock()
	q := s.queue
	runCtx := s.runCtx
	s.mu.Unlock()

	for j := range q {
		// If the app is stopping, stop quickly.
		if runCtx != nil {
			select {
			case <-runCtx.Done():
				return
			default:
			}
		}
		s.sendWithRetry(runCtx, j)
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
				bus.Publish(eventbus.Event{Type: "notify.sent", Time: now, Data: NotificationEvent{Channel: j.n.Channel, ChatID: j.n.Target.ChatID, ThreadID: j.n.Target.ThreadID, Key: j.dedupKey, At: now}})
			}
			return
		}
		lastErr = err
		log.Debug("notify send failed", slog.Any("err", err), slog.Int("attempt", attempt), slog.Int("max", maxAttempts))

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
		bus.Publish(eventbus.Event{Type: "notify.failed", Time: now, Data: NotificationEvent{Channel: j.n.Channel, ChatID: j.n.Target.ChatID, ThreadID: j.n.Target.ThreadID, Key: j.dedupKey, At: now, Error: lastErr.Error()}})
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

func (s *Service) dedupAllow(key string, window time.Duration, max int) bool {
	now := time.Now()

	s.dmu.Lock()
	defer s.dmu.Unlock()
	if s.dedup == nil {
		s.dedup = map[string]time.Time{}
	}
	if until, ok := s.dedup[key]; ok && now.Before(until) {
		return false
	}
	s.dedup[key] = now.Add(window)

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
