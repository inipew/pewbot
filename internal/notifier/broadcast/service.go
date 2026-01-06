package broadcast

import (
	"context"
	logx "pewbot/pkg/logx"
	"runtime/debug"
	"time"

	"golang.org/x/time/rate"

	kit "pewbot/internal/transport"
)

func New(cfg Config, adapter kit.Adapter, log logx.Logger) *Service {
	if log.IsZero() {
		log = logx.Nop()
	}
	rps := cfg.RatePerSec
	if rps <= 0 {
		rps = 10
	}
	// Bound the status cache by default to prevent unbounded growth over time.
	statusMax := 200
	statusTTL := 24 * time.Hour
	return &Service{
		cfg:       cfg,
		adapter:   adapter,
		log:       log,
		limiter:   rate.NewLimiter(rate.Limit(rps), rps),
		queue:     make(chan job, 256),
		status:    map[string]*JobStatus{},
		statusMax: statusMax,
		statusTTL: statusTTL,
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
	s.log.Debug("start requested", logx.Bool("enabled", cur.Enabled), logx.Int("workers", cur.Workers), logx.Int("rps", cur.RatePerSec))
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
					s.log.Error("panic in broadcaster worker", logx.Int("worker", idx), logx.Any("panic", r), logx.String("stack", string(debug.Stack())))
				}
			}()
			s.log.Debug("worker started", logx.Int("worker", idx))
			s.worker(runCtx, stopCh, queue, idx)
			s.log.Debug("worker stopped", logx.Int("worker", idx))
		}()
	}

	s.log.Info("service started", logx.Int("workers", workers), logx.Int("rps", rps))
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
		s.log.Info("service stopped", logx.Duration("took", time.Since(start)))
	}()

	select {
	case <-done:
		return
	case <-ctx.Done():
		// stop continues in background
		return
	}
}
