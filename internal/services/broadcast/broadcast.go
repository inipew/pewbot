package broadcast

import (
	"context"
	"fmt"
	"log/slog"
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

	statusMu sync.RWMutex
	status   map[string]*JobStatus
}

func New(cfg Config, adapter kit.Adapter, log *slog.Logger) *Service {
	return &Service{
		cfg:     cfg,
		adapter: adapter,
		log:     log,
		status:  map[string]*JobStatus{},
	}
}

func (s *Service) Enabled() bool { return s.cfg.Enabled }

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
	defer s.mu.Unlock()
	if s.stopCh != nil {
		return
	}
	workers := s.cfg.Workers
	if workers <= 0 {
		workers = 4
	}
	rps := s.cfg.RatePerSec
	if rps <= 0 {
		rps = 10
	}
	s.limiter = rate.NewLimiter(rate.Limit(rps), rps)

	s.queue = make(chan job, 64)
	s.stopCh = make(chan struct{})
	for i := 0; i < workers; i++ {
		go s.worker(ctx, i)
	}
	s.log.Info("broadcaster started", slog.Int("workers", workers), slog.Int("rps", rps))
}

func (s *Service) Stop(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopCh == nil {
		return
	}
	close(s.stopCh)
	s.stopCh = nil
	s.log.Info("broadcaster stopped")
}

func (s *Service) NewJob(name string, targets []kit.ChatTarget, text string, opt *kit.SendOptions) string {
	id := fmt.Sprintf("bc:%d", time.Now().UnixNano())
	st := &JobStatus{ID: id, Name: name, Total: len(targets)}
	s.statusMu.Lock()
	s.status[id] = st
	s.statusMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queue != nil {
		s.queue <- job{id: id, name: name, targets: targets, text: text, opt: opt}
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

func (s *Service) worker(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case j := <-s.queue:
			s.execJob(ctx, j)
		}
	}
}

func (s *Service) execJob(ctx context.Context, j job) {
	s.setRunning(j.id, true)
	defer s.setRunning(j.id, false)

	for _, t := range j.targets {
		if err := s.sendOne(ctx, t, j.text, j.opt); err != nil {
			s.markFail(j.id, t)
		}
		s.markDone(j.id)
	}
	s.finish(j.id)
}

func (s *Service) sendOne(ctx context.Context, t kit.ChatTarget, text string, opt *kit.SendOptions) error {
	if s.limiter != nil {
		_ = s.limiter.Wait(ctx)
	}
	var last error
	retry := s.cfg.RetryMax
	for i := 0; i <= retry; i++ {
		_, err := s.adapter.SendText(ctx, t, text, opt)
		if err == nil {
			return nil
		}
		last = err
		time.Sleep(time.Duration(200+100*i) * time.Millisecond)
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
