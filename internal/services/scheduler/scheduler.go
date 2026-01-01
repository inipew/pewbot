package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

type Config struct {
	Enabled          bool
	Workers          int
	DefaultTimeoutMS int
	HistorySize      int
	Timezone         string // IANA TZ, e.g. "Asia/Jakarta"
}

type HistoryItem struct {
	ID       string
	Name     string
	Started  time.Time
	Duration time.Duration
	Error    string
}

type task struct {
	id      string
	name    string
	timeout time.Duration
	run     func(ctx context.Context) error
	retry   int
}

type scheduleDef struct {
	id      string
	name    string
	spec    string // cron spec or @every
	timeout time.Duration
	job     func(ctx context.Context) error
}

type Service struct {
	mu sync.Mutex

	log *slog.Logger
	cfg Config
	loc *time.Location

	parser cron.Parser
	c      *cron.Cron
	defs   []scheduleDef

	queue  chan task
	stopCh chan struct{}

	// one-time timers
	tmu    sync.Mutex
	timers map[string]*time.Timer

	hmu     sync.Mutex
	history []HistoryItem
}

func New(cfg Config, log *slog.Logger) *Service {
	return &Service{
		cfg:    cfg,
		log:    log,
		parser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor),
		timers: map[string]*time.Timer{},
	}
}

func (s *Service) Enabled() bool { return s.cfg.Enabled }

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
	defer s.mu.Unlock()
	if s.stopCh != nil {
		return
	}
	s.stopCh = make(chan struct{})

	workers := s.cfg.Workers
	if workers <= 0 {
		workers = 2
	}
	s.queue = make(chan task, 256)

	loc := s.loadLocationLocked()
	s.loc = loc
	s.c = cron.New(cron.WithParser(s.parser), cron.WithLocation(loc))

	// re-register existing defs (if any)
	for _, d := range s.defs {
		s.addCronLocked(d)
	}

	for i := 0; i < workers; i++ {
		go s.worker(ctx, i)
	}
	s.c.Start()
	s.log.Info("scheduler started", slog.Int("workers", workers), slog.String("tz", loc.String()))
}

func (s *Service) Stop(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopCh == nil {
		return
	}
	close(s.stopCh)
	s.stopCh = nil
	if s.c != nil {
		<-s.c.Stop().Done()
		s.c = nil
	}

	// stop all one-time timers
	s.tmu.Lock()
	for _, t := range s.timers {
		_ = t.Stop()
	}
	s.timers = map[string]*time.Timer{}
	s.tmu.Unlock()

	s.log.Info("scheduler stopped")
}

func (s *Service) AddCron(name, spec string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil {
		return "", errors.New("scheduler not started")
	}
	id := fmt.Sprintf("cron:%d", time.Now().UnixNano())
	d := scheduleDef{id: id, name: name, spec: spec, timeout: s.resolveTimeout(timeout), job: job}
	s.defs = append(s.defs, d)
	return id, s.addCronLocked(d)
}

func (s *Service) AddInterval(name string, every time.Duration, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil {
		return "", errors.New("scheduler not started")
	}
	id := fmt.Sprintf("interval:%d", time.Now().UnixNano())
	spec := fmt.Sprintf("@every %s", every.String())
	d := scheduleDef{id: id, name: name, spec: spec, timeout: s.resolveTimeout(timeout), job: job}
	s.defs = append(s.defs, d)
	return id, s.addCronLocked(d)
}

// Helper: daily at HH:MM (scheduler timezone)
func (s *Service) AddDaily(name string, atHHMM string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	h, m, err := parseHHMM(atHHMM)
	if err != nil {
		return "", err
	}
	spec := fmt.Sprintf("%d %d * * *", m, h)
	return s.AddCron(name, spec, timeout, job)
}

// Helper: weekly at HH:MM for given weekday (scheduler timezone)
func (s *Service) AddWeekly(name string, weekday time.Weekday, atHHMM string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	h, m, err := parseHHMM(atHHMM)
	if err != nil {
		return "", err
	}
	dow := int(weekday) // Sunday=0
	spec := fmt.Sprintf("%d %d * * %d", m, h, dow)
	return s.AddCron(name, spec, timeout, job)
}

func (s *Service) addCronLocked(d scheduleDef) error {
	_, err := s.c.AddFunc(d.spec, func() {
		s.enqueue(task{id: d.id, name: d.name, timeout: d.timeout, run: d.job, retry: 1})
	})
	return err
}

func (s *Service) restartLocked() {
	if s.c != nil {
		<-s.c.Stop().Done()
	}
	loc := s.loadLocationLocked()
	s.loc = loc
	s.c = cron.New(cron.WithParser(s.parser), cron.WithLocation(loc))
	for _, d := range s.defs {
		_ = s.addCronLocked(d)
	}
	s.c.Start()
	s.log.Info("scheduler restarted", slog.String("tz", loc.String()))
}

func (s *Service) loadLocationLocked() *time.Location {
	tz := strings.TrimSpace(s.cfg.Timezone)
	if tz == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		s.log.Warn("invalid timezone, falling back to Local", slog.String("tz", tz), slog.String("err", err.Error()))
		return time.Local
	}
	return loc
}

func (s *Service) resolveTimeout(t time.Duration) time.Duration {
	if t > 0 {
		return t
	}
	if s.cfg.DefaultTimeoutMS > 0 {
		return time.Duration(s.cfg.DefaultTimeoutMS) * time.Millisecond
	}
	return 0
}

func (s *Service) enqueue(t task) {
	select {
	case s.queue <- t:
	default:
		s.log.Warn("scheduler queue full, dropping task", slog.String("task", t.name))
	}
}

func (s *Service) worker(ctx context.Context, idx int) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case t := <-s.queue:
			s.execOne(ctx, t)
		}
	}
}

func (s *Service) execOne(ctx context.Context, t task) {
	start := time.Now()
	runCtx := ctx
	var cancel func()
	if t.timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, t.timeout)
		defer cancel()
	}

	err := t.run(runCtx)
	if err != nil && t.retry > 0 {
		// simple retry once
		time.Sleep(500 * time.Millisecond)
		err = t.run(runCtx)
	}

	item := HistoryItem{
		ID:       t.id,
		Name:     t.name,
		Started:  start,
		Duration: time.Since(start),
	}
	if err != nil {
		item.Error = err.Error()
		s.log.Warn("task failed", slog.String("task", t.name), slog.String("err", err.Error()))
	} else {
		s.log.Info("task ok", slog.String("task", t.name))
	}

	s.hmu.Lock()
	defer s.hmu.Unlock()
	s.history = append(s.history, item)
	if s.cfg.HistorySize > 0 && len(s.history) > s.cfg.HistorySize {
		s.history = s.history[len(s.history)-s.cfg.HistorySize:]
	}
}

func parseHHMM(s string) (hour int, minute int, err error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid time %q, expected HH:MM", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, 0, fmt.Errorf("invalid hour in %q", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("invalid minute in %q", s)
	}
	return h, m, nil
}
