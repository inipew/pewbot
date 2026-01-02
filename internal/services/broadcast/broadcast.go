package broadcast

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"pewbot/internal/kit"
)

type Config struct {
	Enabled    bool
	Workers    int
	RatePerSec int
	RetryMax   int
}

type job struct {
	id      string
	name    string
	targets []kit.ChatTarget
	text    string
	opt     *kit.SendOptions
}

type JobStatus struct {
	ID        string
	Name      string
	Total     int
	Done      int
	Failed    int
	Failures  []kit.ChatTarget
	StartedAt time.Time
	DoneAt    time.Time
	Running   bool
}

type Service struct {
	mu sync.Mutex

	cfg     Config
	adapter kit.Adapter
	log     *slog.Logger

	limiter *rate.Limiter
	queue   chan job
	stopCh  chan struct{}
	// stopDone is non-nil while a Stop() is in progress; it is closed when workers fully exit.
	stopDone chan struct{}

	statusMu  sync.RWMutex
	status    map[string]*JobStatus
	runCtx    context.Context
	runCancel context.CancelFunc
	workerWG  sync.WaitGroup
}

func New(cfg Config, adapter kit.Adapter, log *slog.Logger) *Service {
	rps := cfg.RatePerSec
	if rps <= 0 {
		rps = 10
	}
	return &Service{
		cfg:     cfg,
		adapter: adapter,
		log:     log,
		limiter: rate.NewLimiter(rate.Limit(rps), rps),
		queue:   make(chan job, 256),
		status:  map[string]*JobStatus{},
	}
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
	defer s.mu.Unlock()
	s.cfg = cfg
	// Note: live pool resizing is out of scope for starter.
	rps := s.cfg.RatePerSec
	if rps <= 0 {
		rps = 10
	}
	s.limiter = rate.NewLimiter(rate.Limit(rps), rps)
}

func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	cur := s.cfg
	s.mu.Unlock()
	s.log.Debug("start requested", slog.Bool("enabled", cur.Enabled), slog.Int("workers", cur.Workers), slog.Int("rps", cur.RatePerSec))
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
		workers = 4
	}
	rps := s.cfg.RatePerSec
	if rps <= 0 {
		rps = 10
	}
	s.limiter = rate.NewLimiter(rate.Limit(rps), rps)

	// keep queue across restarts (jobs remain pending)
	queue := s.queue
	stopCh := s.stopCh
	runCtx := s.runCtx

	s.workerWG.Add(workers)
	for i := 0; i < workers; i++ {
		idx := i
		go func() {
			defer s.workerWG.Done()
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("panic in broadcaster worker", slog.Int("worker", idx), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
				}
			}()
			s.log.Debug("worker started", slog.Int("worker", idx))
			s.worker(runCtx, stopCh, queue, idx)
			s.log.Debug("worker stopped", slog.Int("worker", idx))
		}()
	}

	s.log.Info("service started", slog.Int("workers", workers), slog.Int("rps", rps))
}

func (s *Service) Stop(ctx context.Context) {
	start := time.Now()
	s.mu.Lock()
	if s.stopCh == nil {
		s.mu.Unlock()
		return
	}
	// If a stop is already in progress, just wait for it.
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

func (s *Service) NewJob(name string, targets []kit.ChatTarget, text string, opt *kit.SendOptions) string {
	id := fmt.Sprintf("bc:%d", time.Now().UnixNano())
	st := &JobStatus{ID: id, Name: name, Total: len(targets)}
	s.statusMu.Lock()
	s.status[id] = st
	s.statusMu.Unlock()

	s.mu.Lock()
	q := s.queue
	s.mu.Unlock()
	if q != nil {
		select {
		case q <- job{id: id, name: name, targets: targets, text: text, opt: opt}:
			s.log.Debug("broadcast job enqueued", slog.String("job", id), slog.String("name", name), slog.Int("total", len(targets)), slog.Int("queue_len", len(q)), slog.Int("queue_cap", cap(q)))
		default:
			s.log.Warn("broadcast queue full; dropping job", slog.String("job", id), slog.String("name", name), slog.Int("queue_len", len(q)), slog.Int("queue_cap", cap(q)))
			s.statusMu.Lock()
			if st := s.status[id]; st != nil {
				st.DoneAt = time.Now()
				st.Running = false
				st.Failed = st.Total
			}
			s.statusMu.Unlock()
		}
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
	if !ok {
		return nil, false
	}
	cp := *st
	return cp, true
}

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

	s.log.Info("broadcast job started", slog.String("job", j.id), slog.String("name", j.name), slog.Int("total", len(j.targets)))

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
			fields := []any{
				slog.String("job", j.id),
				slog.String("name", j.name),
				slog.Int("total", st.Total),
				slog.Int("failed", st.Failed),
				slog.Duration("dur", time.Since(start)),
			}
			if st.Failed > 0 {
				s.log.Warn("broadcast job finished with failures", fields...)
			} else {
				s.log.Info("broadcast job finished", fields...)
			}
			return
		}
	}
	s.log.Info("broadcast job finished", slog.String("job", j.id), slog.String("name", j.name), slog.Duration("dur", time.Since(start)))
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
		s.log.Debug("broadcast send retry scheduled", slog.String("job", jobID), slog.String("name", jobName), slog.Int64("chat_id", t.ChatID), slog.Int("attempt", i+2), slog.Duration("delay", delay), slog.Any("err", err))
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
		s.log.Warn("broadcast send failed", slog.String("job", jobID), slog.String("name", jobName), slog.Int64("chat_id", t.ChatID), slog.Int("thread_id", t.ThreadID), slog.Any("err", last))
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
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if st := s.status[id]; st != nil {
		st.DoneAt = time.Now()
		st.Running = false
	}
}
