package taskengine

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pewbot/internal/eventbus"
)

type queuedTask struct {
	task    Task
	opt     TaskOptions
	state   *RunState
	track   bool // release state when done
	timeout time.Duration
}

// Service executes tasks from a queue using a worker pool.
//
// It is panic-safe (worker goroutines recover), and cooperates with shutdown
// via Start/Stop.
type Service struct {
	mu sync.Mutex

	log *slog.Logger
	bus eventbus.Bus
	cfg Config

	queue     chan queuedTask
	stopCh    chan struct{}
	stopDone  chan struct{}
	runCtx    context.Context
	runCancel context.CancelFunc
	workerWG  sync.WaitGroup

	stateMu sync.Mutex
	states  map[string]*RunState

	hmu     sync.Mutex
	history []HistoryItem

	// Counters (lifetime) for operator diagnostics.
	dropped uint64
}

func New(cfg Config, log *slog.Logger, bus eventbus.Bus) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{cfg: cfg, log: log, bus: bus, states: map[string]*RunState{}}
}

// Enabled reports the current config flag. (Thread-safe; Apply() may run concurrently.)
func (s *Service) Enabled() bool {
	s.mu.Lock()
	en := s.cfg.Enabled
	s.mu.Unlock()
	return en
}

func (s *Service) Apply(cfg Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	// Note: live pool resizing is out of scope for this phase.
}

func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	cur := s.cfg
	s.mu.Unlock()
	s.log.Debug("start requested", slog.Bool("enabled", cur.Enabled), slog.Int("workers", cur.Workers), slog.Int("queue_size", cur.QueueSize))

	// If a Stop() is in progress, wait for it to complete (prevents double worker pools).
	for {
		s.mu.Lock()
		if s.stopCh == nil {
			break
		}
		done := s.stopDone
		if done == nil {
			// already running
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		select {
		case <-done:
			// loop
		case <-ctx.Done():
			return
		}
	}
	defer s.mu.Unlock()

	s.stopCh = make(chan struct{})
	s.runCtx, s.runCancel = context.WithCancel(ctx)

	workers := s.cfg.Workers
	if workers <= 0 {
		workers = 2
	}
	qs := s.cfg.QueueSize
	if qs <= 0 {
		qs = 256
	}
	// Fresh queue per run to avoid executing stale items after a stop/start toggle.
	s.queue = make(chan queuedTask, qs)

	runCtx := s.runCtx
	stopCh := s.stopCh
	queue := s.queue

	s.workerWG.Add(workers)
	for i := 0; i < workers; i++ {
		idx := i
		go func() {
			defer s.workerWG.Done()
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("panic in taskengine worker", slog.Int("worker", idx), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
				}
			}()
			s.log.Debug("worker started", slog.Int("worker", idx))
			s.worker(runCtx, stopCh, queue, idx)
			s.log.Debug("worker stopped", slog.Int("worker", idx))
		}()
	}

	s.log.Info("service started", slog.Int("workers", workers), slog.Int("queue_size", qs))
}

func (s *Service) Stop(ctx context.Context) {
	start := time.Now()
	s.mu.Lock()
	if s.stopCh == nil {
		s.mu.Unlock()
		return
	}
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
	stopCh := s.stopCh
	cancel := s.runCancel
	s.runCancel = nil
	s.mu.Unlock()

	close(stopCh)
	if cancel != nil {
		cancel()
	}

	go func() {
		s.workerWG.Wait()
		s.mu.Lock()
		s.stopCh = nil
		s.runCtx = nil
		s.queue = nil
		s.stopDone = nil
		s.mu.Unlock()
		close(done)
		s.log.Info("service stopped", slog.Duration("took", time.Since(start)))
	}()

	select {
	case <-done:
		return
	case <-ctx.Done():
		// stop continues in background
		return
	}
}

func (s *Service) stateFor(key string) *RunState {
	key = strings.TrimSpace(key)
	if key == "" {
		return &RunState{}
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

// Enqueue submits a task for execution.
//
// It is non-blocking: if the queue is full it returns ErrQueueFull and drops the task.
func (s *Service) Enqueue(t Task) error {
	s.mu.Lock()
	cfg := s.cfg
	q := s.queue
	bus := s.bus
	log := s.log
	s.mu.Unlock()

	if !cfg.Enabled {
		return ErrDisabled
	}
	if q == nil {
		return ErrStopped
	}
	if t.Run == nil {
		return errors.New("task Run is nil")
	}

	opt := t.Opt.withDefaults(cfg)

	st := t.State
	if st == nil {
		key := strings.TrimSpace(t.ConcurrencyKey)
		if key == "" {
			key = t.Name
		}
		st = s.stateFor(key)
	}

	track := false
	if opt.Overlap == OverlapSkipIfRunning {
		if !st.tryAcquire() {
			now := time.Now()
			log.Debug("task skipped (overlap)", slog.String("task", t.Name))
			if bus != nil {
				bus.Publish(eventbus.Event{Type: "task.skipped", Time: now, Data: TaskEvent{ID: t.ID, Name: t.Name, Started: now, Attempts: 0, Error: "overlap_skip"}})
			}
			return ErrOverlapSkip
		}
		track = true
	}

	timeout := t.Timeout
	if timeout <= 0 && cfg.DefaultTimeout > 0 {
		timeout = cfg.DefaultTimeout
	}

	qt := queuedTask{task: t, opt: opt, state: st, track: track, timeout: timeout}
	select {
	case q <- qt:
		return nil
	default:
		if track {
			st.release()
		}
		atomic.AddUint64(&s.dropped, 1)
		now := time.Now()
		log.Warn("taskengine queue full; dropping task", slog.String("task", t.Name), slog.Int("queue_len", len(q)), slog.Int("queue_cap", cap(q)))
		if bus != nil {
			bus.Publish(eventbus.Event{Type: "task.dropped", Time: now, Data: TaskEvent{ID: t.ID, Name: t.Name, Started: now, Attempts: 0, Error: "queue_full"}})
		}
		return ErrQueueFull
	}
}

func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	enabled := s.cfg.Enabled
	workers := s.cfg.Workers
	ql := 0
	qc := 0
	defTimeout := s.cfg.DefaultTimeout
	retryMax := s.cfg.RetryMax
	if s.queue != nil {
		ql = len(s.queue)
		qc = cap(s.queue)
	}
	s.mu.Unlock()

	dropped := atomic.LoadUint64(&s.dropped)

	if workers <= 0 {
		workers = 2
	}

	s.hmu.Lock()
	hist := make([]HistoryItem, len(s.history))
	copy(hist, s.history)
	s.hmu.Unlock()

	return Snapshot{
		Enabled:        enabled,
		Workers:        workers,
		QueueLen:       ql,
		QueueCap:       qc,
		Dropped:        dropped,
		DefaultTimeout: defTimeout,
		RetryMax:       retryMax,
		History:        hist,
	}
}
