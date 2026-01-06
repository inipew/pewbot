package scheduler

import (
	"context"
	logx "pewbot/pkg/logx"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"pewbot/internal/eventbus"
	"pewbot/internal/task/engine"
)

func New(cfg Config, engine *engine.Service, log logx.Logger, bus eventbus.Bus) *Service {
	if log.IsZero() {
		log = logx.Nop()
	}
	return &Service{
		cfg:    cfg,
		log:    log,
		bus:    bus,
		engine: engine,
		// SecondOptional allows both 5-field and 6-field (with seconds) cron specs.
		parser:      cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor),
		timers:      map[string]*time.Timer{},
		onceAt:      map[string]time.Time{},
		onceTimeout: map[string]time.Duration{},
		onceJob:     map[string]func(ctx context.Context) error{},
		onceVer:     map[string]uint64{},
		lastEnqWarn: map[string]time.Time{},
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

	if s.c == nil {
		return
	}
	if oldTZ != newTZ {
		// restart cron with new location and re-register definitions
		s.restartLocked()
	}
}

// Start starts cron triggering and restores one-time timers.
//
// NOTE: This service is trigger-only in Phase 2; execution happens in engine.Service.
func (s *Service) Start(ctx context.Context) {
	_ = ctx // reserved for future (e.g., context-driven drain/stop policies)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		return
	}
	cur := s.cfg
	s.log.Debug("start requested", logx.Bool("enabled", cur.Enabled), logx.String("tz", strings.TrimSpace(cur.Timezone)))

	loc := s.loadLocationLocked()
	s.loc = loc
	s.c = cron.New(cron.WithParser(s.parser), cron.WithLocation(loc))

	// register existing defs (if any)
	for i := range s.defs {
		_ = s.addCronLocked(&s.defs[i])
	}
	s.c.Start()
	// Restore one-time timers from persisted definitions.
	s.rebuildOnceTimersLocked()
	s.log.Info("service started", logx.String("tz", loc.String()), logx.Int("schedules", len(s.defs)))
}

// Stop stops cron triggering and stops all runtime one-time timers.
// Persisted one-time definitions remain so they can resume on next Start().
func (s *Service) Stop(ctx context.Context) {
	start := time.Now()
	s.log.Info("stop requested")

	s.mu.Lock()
	c := s.c
	s.c = nil
	s.mu.Unlock()

	if c != nil {
		select {
		case <-c.Stop().Done():
		case <-ctx.Done():
			// best-effort
		}
	}

	s.tmu.Lock()
	for _, t := range s.timers {
		_ = t.Stop()
	}
	s.timers = map[string]*time.Timer{}
	s.tmu.Unlock()

	s.log.Info("service stopped", logx.Duration("took", time.Since(start)))
}
