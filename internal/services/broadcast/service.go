package broadcast

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"golang.org/x/time/rate"

	"pewbot/internal/kit"
)

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
