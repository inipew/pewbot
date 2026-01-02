package scheduler

import (
	"context"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

func New(cfg Config, log *slog.Logger) *Service {
	return &Service{
		cfg: cfg,
		log: log,
		// SecondOptional allows both 5-field and 6-field (with seconds) cron specs.
		parser:      cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor),
		timers:      map[string]*time.Timer{},
		onceAt:      map[string]time.Time{},
		onceTimeout: map[string]time.Duration{},
		onceJob:     map[string]func(ctx context.Context) error{},
		onceVer:     map[string]uint64{},
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

	// detect timezone change
	oldTZ := strings.TrimSpace(s.cfg.Timezone)
	newTZ := strings.TrimSpace(cfg.Timezone)
	s.cfg = cfg

	if s.stopCh == nil {
		return
	}
	if oldTZ != newTZ {
		// restart cron with new location and re-register definitions
		s.restartLocked()
	}
	// pool resizing dynamically is out of scope for starter
}

func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	cur := s.cfg
	s.mu.Unlock()
	s.log.Debug("start requested", slog.Bool("enabled", cur.Enabled), slog.Int("workers", cur.Workers), slog.String("tz", strings.TrimSpace(cur.Timezone)))
	// If a Stop() is in progress, wait for it to complete (prevents double worker pools).
	for {
		s.mu.Lock()
		if s.stopCh == nil {
			break
		}
		done := s.stopDone
		// already running (no stop in progress)
		if done == nil {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		select {
		case <-done:
			// loop and try again
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
	// Fresh queue per run to avoid executing "stale" enqueued tasks after a stop/start toggle.
	s.queue = make(chan task, 256)

	loc := s.loadLocationLocked()
	s.loc = loc
	s.c = cron.New(cron.WithParser(s.parser), cron.WithLocation(loc))

	// re-register existing defs (if any)
	for i := range s.defs {
		s.addCronLocked(&s.defs[i])
	}

	// Local captures prevent races if fields are swapped/nilled during Stop().
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
					s.log.Error("panic in scheduler worker", slog.Int("worker", idx), slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
				}
			}()
			s.log.Debug("worker started", slog.Int("worker", idx))
			s.worker(runCtx, stopCh, queue, idx)
			s.log.Debug("worker stopped", slog.Int("worker", idx))
		}()
	}
	s.c.Start()
	// Rebuild one-time timers from persistent definitions.
	s.rebuildOnceTimersLocked()
	s.log.Info("service started", slog.Int("workers", workers), slog.String("tz", loc.String()), slog.Int("schedules", len(s.defs)))
}

func (s *Service) Stop(ctx context.Context) {
	start := time.Now()
	s.log.Info("stop requested")
	// If a stop is already in progress, just wait (best-effort).
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
	// Initiate stop.
	done := make(chan struct{})
	s.stopDone = done
	stopCh := s.stopCh
	cancel := s.runCancel
	c := s.c
	// prevent new cron enqueues quickly
	s.c = nil
	s.runCancel = nil
	s.mu.Unlock()

	// signal workers to exit promptly
	close(stopCh)
	if cancel != nil {
		cancel()
	}

	if c != nil {
		<-c.Stop().Done()
	}

	// stop all runtime one-time timers (keep definitions so they can resume on next Start())
	s.tmu.Lock()
	for _, t := range s.timers {
		_ = t.Stop()
	}
	s.timers = map[string]*time.Timer{}
	s.tmu.Unlock()

	// finalize cleanup in background so Stop() can return on timeout safely.
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
