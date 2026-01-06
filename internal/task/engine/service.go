package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pewbot/internal/eventbus"
	logx "pewbot/pkg/logx"

	rtsup "pewbot/internal/runtime/supervisor"
)

const warnThrottleEvery = 5 * time.Second

type Service struct {
	mu  sync.Mutex
	cfg Config
	log logx.Logger
	bus eventbus.Bus

	q chan queuedTask

	// Adaptive concurrency (soft limit).
	inFlight         int32
	waitingForPermit int32
	permitMax        int32
	permitLimit      int32
	permits          chan struct{}

	sup      *rtsup.Supervisor
	stopCh   chan struct{}
	stopDone chan struct{}

	stateMu sync.Mutex
	states  map[string]*RunState

	// Concurrency groups (optional per task via TaskOptions.ConcurrencyLimit).
	groups groupLimiterStore

	// Circuit breaker state (consecutive failures).
	circuits circuitStore

	hmu     sync.Mutex
	history []HistoryItem

	idSeq uint64

	dropped          uint64
	droppedQueueFull uint64
	droppedStale     uint64

	lastQueueFullWarnAt int64
	lastStaleWarnAt     int64
}

type queuedTask struct {
	task Task

	enqueuedAt time.Time
	timeout    time.Duration
	opt        TaskOptions

	state *RunState
	track bool
}

func New(cfg Config, log logx.Logger, bus eventbus.Bus) *Service {
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 3
	}
	if cfg.HistorySize <= 0 {
		cfg.HistorySize = 200
	}
	// Circuit breaker defaults: enabled by default.
	if cfg.CircuitTripFailures == 0 {
		cfg.CircuitTripFailures = 5
	}
	if cfg.CircuitBaseDelay <= 0 {
		cfg.CircuitBaseDelay = 5 * time.Second
	}
	if cfg.CircuitMaxDelay <= 0 {
		cfg.CircuitMaxDelay = 2 * time.Minute
	}
	if cfg.CircuitResetAfter <= 0 {
		cfg.CircuitResetAfter = 5 * time.Minute
	}
	return &Service{
		cfg:    cfg,
		log:    log,
		bus:    bus,
		states: make(map[string]*RunState),
	}
}

func (s *Service) Enabled() bool {
	s.mu.Lock()
	en := s.cfg.Enabled
	s.mu.Unlock()
	return en
}

// Supervisor returns the task engine's internal supervisor (nil if not started).
// This is used for operational visibility (e.g. /health).
func (s *Service) Supervisor() *rtsup.Supervisor {
	s.mu.Lock()
	sup := s.sup
	s.mu.Unlock()
	return sup
}

func (s *Service) Apply(ctx context.Context, cfg Config) {
	s.mu.Lock()
	prev := s.cfg
	s.cfg = cfg
	running := s.stopCh != nil && s.stopDone == nil
	s.mu.Unlock()

	if !running {
		return
	}

	// If core execution settings changed, restart workers.
	if prev.Workers != cfg.Workers || prev.QueueSize != cfg.QueueSize {
		s.Stop(ctx)
		s.Start(ctx)
	}
}

func (s *Service) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	cfg := s.cfg
	if !cfg.Enabled {
		s.mu.Unlock()
		return
	}

	// Start is idempotent.
	if s.stopCh != nil {
		// If stopping, wait for it to finish before restarting.
		done := s.stopDone
		s.mu.Unlock()
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return
			}
		} else {
			return
		}
		s.mu.Lock()
		// Re-check after wait.
		if s.stopCh != nil {
			s.mu.Unlock()
			return
		}
	}

	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}

	s.q = make(chan queuedTask, cfg.QueueSize)
	s.stopCh = make(chan struct{})
	s.stopDone = nil
	stopCh := s.stopCh
	queue := s.q
	workers := cfg.Workers

	// Initialize adaptive concurrency controller.
	atomic.StoreInt32(&s.inFlight, 0)
	atomic.StoreInt32(&s.waitingForPermit, 0)
	atomic.StoreInt32(&s.permitMax, int32(workers))
	initLim := initialPermitLimit(workers)
	atomic.StoreInt32(&s.permitLimit, initLim)
	s.permits = make(chan struct{}, workers)
	for i := int32(0); i < initLim; i++ {
		s.permits <- struct{}{}
	}

	s.sup = rtsup.NewSupervisor(ctx,
		rtsup.WithLogger(s.log.With(logx.String("comp", "taskengine"))),
		// taskengine failures should not hard-kill the app; treat as best-effort.
		rtsup.WithCancelOnError(false),
	)
	sup := s.sup
	s.mu.Unlock()

	for i := 0; i < workers; i++ {
		idx := i
		name := fmt.Sprintf("worker.%d", idx)
		// Auto-restart workers if they panic or exit unexpectedly.
		sup.GoRestart(name, func(c context.Context) error {
			s.worker(c, stopCh, queue, idx)
			// Clean exits happen only on shutdown.
			select {
			case <-stopCh:
				return context.Canceled
			default:
			}
			if c.Err() != nil {
				return c.Err()
			}
			return errors.New("worker exited unexpectedly")
		},
			rtsup.WithPublishFirstError(true),
		)
	}

	// Adaptive concurrency controller.
	sup.GoRestart("autoscale", func(c context.Context) error {
		s.autoscale(c, stopCh, queue)
		select {
		case <-stopCh:
			return context.Canceled
		default:
		}
		if c.Err() != nil {
			return c.Err()
		}
		return errors.New("autoscale exited unexpectedly")
	},
		rtsup.WithPublishFirstError(true),
	)

	s.log.Info("task engine started", logx.Int("workers", workers), logx.Int("active_limit", int(atomic.LoadInt32(&s.permitLimit))), logx.Int("queue", cap(queue)))
}

func (s *Service) Stop(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.stopCh == nil {
		s.mu.Unlock()
		return
	}
	// If already stopping, wait.
	if s.stopDone != nil {
		done := s.stopDone
		s.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return
	}

	done := make(chan struct{})
	s.stopDone = done
	close(s.stopCh)
	sup := s.sup
	s.mu.Unlock()

	if sup != nil {
		sup.Cancel()
	}

	go func() {
		// Wait unbounded in background; caller can still time out.
		if sup != nil {
			_ = sup.Wait(context.Background())
		}
		s.mu.Lock()
		s.q = nil
		s.stopCh = nil
		s.stopDone = nil
		s.sup = nil
		s.permits = nil
		atomic.StoreInt32(&s.inFlight, 0)
		atomic.StoreInt32(&s.waitingForPermit, 0)
		atomic.StoreInt32(&s.permitMax, 0)
		atomic.StoreInt32(&s.permitLimit, 0)
		s.mu.Unlock()
		close(done)
	}()

	select {
	case <-done:
		s.log.Info("task engine stopped")
	case <-ctx.Done():
		s.log.Warn("task engine stop timed out", logx.Any("err", ctx.Err()))
	}
}

// Enqueue tries to enqueue a task without blocking. If the queue is full, the task is dropped.
//
// Use Submit() when you want backpressure instead of dropping.
func (s *Service) Enqueue(t Task) error {
	return s.enqueue(context.Background(), t, false)
}

// Submit enqueues a task and blocks until it is accepted, ctx is canceled, or the engine stops.
func (s *Service) Submit(ctx context.Context, t Task) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return s.enqueue(ctx, t, true)
}

func (s *Service) enqueue(ctx context.Context, t Task, block bool) error {
	if t.Run == nil {
		return fmt.Errorf("task Run is nil")
	}
	name := strings.TrimSpace(t.Name)
	if name == "" {
		return fmt.Errorf("task Name is required")
	}
	t.Name = name

	now := time.Now()
	if strings.TrimSpace(t.ID) == "" {
		t.ID = s.newTaskID(now)
	}

	s.mu.Lock()
	cfg := s.cfg
	q := s.q
	stopCh := s.stopCh
	stopping := s.stopDone != nil
	log := s.log
	bus := s.bus
	s.mu.Unlock()

	if !cfg.Enabled {
		return ErrDisabled
	}
	if q == nil || stopCh == nil {
		return ErrStopped
	}
	if stopping {
		return ErrStopping
	}

	timeout := t.Timeout
	if timeout <= 0 && cfg.DefaultTimeout > 0 {
		timeout = cfg.DefaultTimeout
	}
	opt := t.Opt.withDefaults(cfg)

	// Circuit breaker: if this task has been failing consecutively, skip enqueue
	// to avoid hammering downstream dependencies.
	if open, until := s.circuitIsOpen(now, t.Name, cfg, opt); open {
		if bus != nil {
			bus.Publish(eventbus.Event{Type: "task.skipped", Time: now, Data: TaskEvent{ID: t.ID, Name: t.Name, Started: now, Attempts: 0, Error: "circuit_open"}})
		}
		if !log.IsZero() {
			log.Debug("task skipped: circuit open", logx.String("task", t.Name), logx.String("id", t.ID), logx.String("until", until.Format(time.RFC3339)))
		}
		// Record in history for operator visibility.
		item := HistoryItem{ID: t.ID, Name: t.Name, Started: now, QueueDelay: 0, Duration: 0, Error: "circuit_open"}
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

		return ErrCircuitOpen
	}

	st := t.State
	if st == nil {
		st = s.stateFor(t.ConcurrencyKey, t.Name)
	}

	track := false
	if opt.Overlap == OverlapSkipIfRunning {
		track = true
		if !st.tryAcquire() {
			if bus != nil {
				bus.Publish(eventbus.Event{Type: "task.skipped", Time: now, Data: TaskEvent{ID: t.ID, Name: t.Name, Started: now, Attempts: 0, Error: "overlap_skip"}})
			}
			if !log.IsZero() {
				log.Debug("task skipped due to overlap", logx.String("task", t.Name), logx.String("id", t.ID))
			}
			return ErrOverlapSkip
		}
	}

	qt := queuedTask{task: t, enqueuedAt: now, timeout: timeout, opt: opt, state: st, track: track}

	if !block {
		select {
		case q <- qt:
			return nil
		default:
			if track && st != nil {
				st.release()
			}
			s.onQueueFullDropped(now, t, q, log, bus)
			return ErrQueueFull
		}
	}

	select {
	case q <- qt:
		return nil
	case <-ctx.Done():
		if track && st != nil {
			st.release()
		}
		return ctx.Err()
	case <-stopCh:
		if track && st != nil {
			st.release()
		}
		return ErrStopping
	}
}

func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	cfg := s.cfg
	q := s.q
	s.mu.Unlock()

	ql := 0
	qc := 0
	if q != nil {
		ql = len(q)
		qc = cap(q)
	}

	s.hmu.Lock()
	h := make([]HistoryItem, len(s.history))
	copy(h, s.history)
	s.hmu.Unlock()

	now := time.Now()
	ct, co := s.circuitSnapshot(now, cfg)

	return Snapshot{
		Enabled:          cfg.Enabled,
		Workers:          cfg.Workers,
		QueueLen:         ql,
		QueueCap:         qc,
		ActiveMax:        int(atomic.LoadInt32(&s.permitMax)),
		ActiveLimit:      int(atomic.LoadInt32(&s.permitLimit)),
		InFlight:         int(atomic.LoadInt32(&s.inFlight)),
		WaitingForPermit: int(atomic.LoadInt32(&s.waitingForPermit)),
		Dropped:          atomic.LoadUint64(&s.dropped),
		DroppedQueueFull: atomic.LoadUint64(&s.droppedQueueFull),
		DroppedStale:     atomic.LoadUint64(&s.droppedStale),
		DefaultTimeout:   cfg.DefaultTimeout,
		MaxQueueDelay:    cfg.MaxQueueDelay,
		RetryMax:         cfg.RetryMax,
		CircuitTotal:     ct,
		CircuitOpen:      co,
		History:          h,
	}
}

func (s *Service) stateFor(concurrencyKey, name string) *RunState {
	key := strings.TrimSpace(concurrencyKey)
	if key == "" {
		key = strings.TrimSpace(name)
	}
	if key == "" {
		key = "default"
	}

	s.stateMu.Lock()
	st := s.states[key]
	if st == nil {
		st = &RunState{}
		s.states[key] = st
	}
	s.stateMu.Unlock()
	return st
}

func (s *Service) newTaskID(now time.Time) string {
	seq := atomic.AddUint64(&s.idSeq, 1)
	// Make it short but still unique-ish across restarts.
	return fmt.Sprintf("tsk-%x-%x", now.UnixNano(), seq)
}

func (s *Service) shouldWarn(last *int64, now time.Time) bool {
	prev := atomic.LoadInt64(last)
	n := now.UnixNano()
	if prev != 0 && (n-prev) < int64(warnThrottleEvery) {
		return false
	}
	return atomic.CompareAndSwapInt64(last, prev, n)
}

func (s *Service) onQueueFullDropped(now time.Time, t Task, q chan queuedTask, log logx.Logger, bus eventbus.Bus) {
	atomic.AddUint64(&s.dropped, 1)
	atomic.AddUint64(&s.droppedQueueFull, 1)

	if bus != nil {
		bus.Publish(eventbus.Event{Type: "task.dropped", Time: now, Data: TaskEvent{ID: t.ID, Name: t.Name, Started: now, QueueDelay: 0, Duration: 0, Attempts: 0, Error: "queue_full"}})
	}

	if !log.IsZero() && s.shouldWarn(&s.lastQueueFullWarnAt, now) {
		ql := 0
		qc := 0
		if q != nil {
			ql = len(q)
			qc = cap(q)
		}
		log.Warn(
			"task dropped: queue full",
			logx.String("task", t.Name),
			logx.String("id", t.ID),
			logx.Int("queue_len", ql),
			logx.Int("queue_cap", qc),
			logx.Uint64("dropped_queue_full", atomic.LoadUint64(&s.droppedQueueFull)),
		)
	}
}

func (s *Service) onStaleDropped(now time.Time, t Task, queueDelay time.Duration) {
	atomic.AddUint64(&s.dropped, 1)
	atomic.AddUint64(&s.droppedStale, 1)

	if s.bus != nil {
		s.bus.Publish(eventbus.Event{Type: "task.dropped", Time: now, Data: TaskEvent{ID: t.ID, Name: t.Name, Started: now, QueueDelay: queueDelay, Duration: 0, Attempts: 0, Error: "stale_queue_delay"}})
	}

	if !s.log.IsZero() && s.shouldWarn(&s.lastStaleWarnAt, now) {
		s.log.Warn(
			"task dropped: stale queue",
			logx.String("task", t.Name),
			logx.String("id", t.ID),
			logx.Duration("queue_delay", queueDelay),
			logx.Uint64("dropped_stale", atomic.LoadUint64(&s.droppedStale)),
		)
	}
}
