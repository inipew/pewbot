package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
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
	RetryMax         int    // max retries per task (default 3)
}

type OverlapPolicy int

const (
	OverlapAllow OverlapPolicy = iota
	OverlapSkipIfRunning
)

type TaskOptions struct {
	Overlap       OverlapPolicy
	RetryMax      int
	RetryBase     time.Duration
	RetryMaxDelay time.Duration
	RetryJitter   float64 // 0.2 = 20%
}

func (o TaskOptions) withDefaults(cfg Config) TaskOptions {
	// defaults from scheduler config
	if o.RetryMax <= 0 {
		o.RetryMax = cfg.RetryMax
	}
	if o.RetryBase <= 0 {
		o.RetryBase = 500 * time.Millisecond
	}
	if o.RetryMaxDelay <= 0 {
		o.RetryMaxDelay = 15 * time.Second
	}
	if o.RetryJitter <= 0 {
		o.RetryJitter = 0.2
	}
	// default overlap: skip (safer)
	if o.Overlap != OverlapAllow && o.Overlap != OverlapSkipIfRunning {
		o.Overlap = OverlapSkipIfRunning
	}
	return o
}

type runState struct {
	mu      sync.Mutex
	running bool
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
	opt     TaskOptions
	state   *runState
}

type scheduleDef struct {
	id      string
	name    string
	spec    string // cron spec or @every
	timeout time.Duration
	job     func(ctx context.Context) error
	entryID cron.EntryID
	opt     TaskOptions
	state   *runState
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
	tmu         sync.Mutex
	timers      map[string]*time.Timer
	onceAt      map[string]time.Time
	onceTimeout map[string]time.Duration

	hmu       sync.Mutex
	history   []HistoryItem
	runCtx    context.Context
	runCancel context.CancelFunc
	workerWG  sync.WaitGroup
}

type ScheduleInfo struct {
	ID      string
	Name    string
	Spec    string
	Timeout time.Duration
	Next    time.Time
	Prev    time.Time
}

type Snapshot struct {
	Enabled   bool
	Timezone  string
	Workers   int
	QueueLen  int
	Schedules []ScheduleInfo
	History   []HistoryItem
}

func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	enabled := s.cfg.Enabled
	tz := s.cfg.Timezone
	workers := s.cfg.Workers
	ql := 0
	if s.queue != nil {
		ql = len(s.queue)
	}
	defs := make([]scheduleDef, len(s.defs))
	copy(defs, s.defs)
	c := s.c
	loc := s.loc
	s.mu.Unlock()

	if loc == nil {
		loc = time.Local
	}
	if tz == "" && loc != nil {
		tz = loc.String()
	}

	items := make([]ScheduleInfo, 0, len(defs))
	for _, d := range defs {
		it := ScheduleInfo{ID: d.id, Name: d.name, Spec: d.spec, Timeout: d.timeout}
		if c != nil && d.entryID != 0 {
			e := c.Entry(d.entryID)
			it.Next = e.Next
			it.Prev = e.Prev
		}
		items = append(items, it)
	}

	s.hmu.Lock()
	hist := make([]HistoryItem, len(s.history))
	copy(hist, s.history)
	s.hmu.Unlock()

	return Snapshot{
		Enabled:   enabled,
		Timezone:  tz,
		Workers:   workers,
		QueueLen:  ql,
		Schedules: items,
		History:   hist,
	}
}

func New(cfg Config, log *slog.Logger) *Service {
	return &Service{
		cfg:         cfg,
		log:         log,
		parser:      cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor),
		timers:      map[string]*time.Timer{},
		onceAt:      map[string]time.Time{},
		onceTimeout: map[string]time.Duration{},
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
	defer s.mu.Unlock()
	if s.stopCh != nil {
		return
	}
	s.stopCh = make(chan struct{})
	s.runCtx, s.runCancel = context.WithCancel(ctx)

	workers := s.cfg.Workers
	if workers <= 0 {
		workers = 2
	}
	s.queue = make(chan task, 256)

	loc := s.loadLocationLocked()
	s.loc = loc
	s.c = cron.New(cron.WithParser(s.parser), cron.WithLocation(loc))

	// re-register existing defs (if any)
	for i := range s.defs {
		s.addCronLocked(&s.defs[i])
	}

	s.workerWG.Add(workers)
	for i := 0; i < workers; i++ {
		idx := i
		go func() {
			defer s.workerWG.Done()
			s.worker(s.runCtx, idx)
		}()
	}
	s.c.Start()
	s.log.Info("started", slog.Int("workers", workers), slog.String("tz", loc.String()))
}

func (s *Service) Stop(ctx context.Context) {
	s.mu.Lock()
	if s.stopCh == nil {
		s.mu.Unlock()
		return
	}
	// Close stopCh to signal workers to exit, but don't nil it until workers are fully stopped.
	// This avoids data races where workers are still selecting on s.stopCh.
	close(s.stopCh)
	cancel := s.runCancel
	s.runCancel = nil
	c := s.c
	s.c = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	if c != nil {
		<-c.Stop().Done()
	}

	// stop all one-time timers
	s.tmu.Lock()
	for _, t := range s.timers {
		_ = t.Stop()
	}
	s.timers = map[string]*time.Timer{}
	s.onceAt = map[string]time.Time{}
	s.onceTimeout = map[string]time.Duration{}
	s.tmu.Unlock()

	done := make(chan struct{})
	go func() {
		s.workerWG.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		// still log; workers will stop soon because runCtx cancelled
		s.mu.Lock()
		s.stopCh = nil
		s.runCtx = nil
		s.mu.Unlock()
		s.log.Info("stopped")
		return
	case <-done:
		s.mu.Lock()
		s.stopCh = nil
		s.runCtx = nil
		s.mu.Unlock()
		s.log.Info("stopped")
		return
	}
}

func (s *Service) AddCron(name, spec string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	return s.AddCronOpt(name, spec, timeout, TaskOptions{}, job)
}

func (s *Service) AddCronOpt(name, spec string, timeout time.Duration, opt TaskOptions, job func(ctx context.Context) error) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil {
		return "", errors.New("scheduler not started")
	}
	id := fmt.Sprintf("cron:%d", time.Now().UnixNano())
	opt = opt.withDefaults(s.cfg)
	d := scheduleDef{
		id:      id,
		name:    name,
		spec:    spec,
		timeout: s.resolveTimeout(timeout),
		job:     job,
		opt:     opt,
		state:   &runState{},
	}
	s.defs = append(s.defs, d)
	return id, s.addCronLocked(&s.defs[len(s.defs)-1])
}

func (s *Service) AddInterval(name string, every time.Duration, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	return s.AddIntervalOpt(name, every, timeout, TaskOptions{}, job)
}

func (s *Service) AddIntervalOpt(name string, every time.Duration, timeout time.Duration, opt TaskOptions, job func(ctx context.Context) error) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil {
		return "", errors.New("scheduler not started")
	}
	id := fmt.Sprintf("interval:%d", time.Now().UnixNano())
	spec := fmt.Sprintf("@every %s", every.String())
	opt = opt.withDefaults(s.cfg)
	d := scheduleDef{
		id:      id,
		name:    name,
		spec:    spec,
		timeout: s.resolveTimeout(timeout),
		job:     job,
		opt:     opt,
		state:   &runState{},
	}
	s.defs = append(s.defs, d)
	return id, s.addCronLocked(&s.defs[len(s.defs)-1])
}

// Helper: daily at HH:MM (scheduler timezone)

func (s *Service) AddOnce(name string, at time.Time, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	if name == "" {
		return "", errors.New("name required")
	}
	if at.IsZero() {
		return "", errors.New("at required")
	}

	s.mu.Lock()
	// upsert: stop existing timer with same name
	if t, ok := s.timers[name]; ok {
		t.Stop()
		delete(s.timers, name)
		delete(s.onceAt, name)
		delete(s.onceTimeout, name)
	}
	loc := s.loc
	if loc == nil {
		loc = time.Local
	}
	runAt := at.In(loc)
	s.onceAt[name] = runAt
	s.onceTimeout[name] = s.resolveTimeout(timeout)

	// timer callback: enqueue then cleanup
	delay := time.Until(runAt)
	if delay < 0 {
		delay = 0
	}
	timer := time.AfterFunc(delay, func() {
		// enqueue task
		s.enqueue(task{
			id:      fmt.Sprintf("once:%d", time.Now().UnixNano()),
			name:    name,
			timeout: s.resolveTimeout(timeout),
			run:     job,
			opt:     TaskOptions{}.withDefaults(s.cfg),
			state:   &runState{},
		})

		// cleanup
		s.mu.Lock()
		if t2, ok := s.timers[name]; ok {
			_ = t2 // ignore
		}
		delete(s.timers, name)
		delete(s.onceAt, name)
		delete(s.onceTimeout, name)
		s.mu.Unlock()
	})
	s.timers[name] = timer
	s.mu.Unlock()

	return name, nil
}

func (s *Service) AddDaily(name string, atHHMM string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	h, m, err := parseHHMM(atHHMM)
	if err != nil {
		return "", err
	}
	spec := fmt.Sprintf("%d %d * * *", m, h)
	return s.AddCronOpt(name, spec, timeout, TaskOptions{}, job)
}

// Helper: weekly at HH:MM for given weekday (scheduler timezone)
func (s *Service) AddWeekly(name string, weekday time.Weekday, atHHMM string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	h, m, err := parseHHMM(atHHMM)
	if err != nil {
		return "", err
	}
	dow := int(weekday) // Sunday=0
	spec := fmt.Sprintf("%d %d * * %d", m, h, dow)
	return s.AddCronOpt(name, spec, timeout, TaskOptions{}, job)
}

// Remove unschedules all schedules with the given name. It returns true if something was removed.
// Safe to call even when scheduler disabled; it will no-op and return false if not started.
func (s *Service) Remove(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil {
		return false
	}

	removed := false
	// remove from cron first
	for i := range s.defs {
		if s.defs[i].name == name && s.defs[i].entryID != 0 {
			s.c.Remove(s.defs[i].entryID)
			removed = true
			// mark entry removed
			s.defs[i].entryID = 0
		}
	}

	// compact defs (remove entries by name)
	if removed {
		n := 0
		for _, d := range s.defs {
			if d.name == name {
				continue
			}
			s.defs[n] = d
			n++
		}
		s.defs = s.defs[:n]
	}

	return removed
}

func (s *Service) addCronLocked(d *scheduleDef) error {
	eid, err := s.c.AddFunc(d.spec, func() {
		if d.opt.Overlap == OverlapSkipIfRunning {
			d.state.mu.Lock()
			running := d.state.running
			d.state.mu.Unlock()
			if running {
				s.log.Debug("skip schedule (running)", slog.String("task", d.name))
				return
			}
		}
		s.enqueue(task{id: d.id, name: d.name, timeout: d.timeout, run: d.job, opt: d.opt, state: d.state})
	})
	if err == nil {
		d.entryID = eid
	}
	return err
}

func (s *Service) restartLocked() {
	if s.c != nil {
		<-s.c.Stop().Done()
	}
	loc := s.loadLocationLocked()
	s.loc = loc
	s.c = cron.New(cron.WithParser(s.parser), cron.WithLocation(loc))
	for i := range s.defs {
		_ = s.addCronLocked(&s.defs[i])
	}
	s.c.Start()
	s.log.Info("restarted", slog.String("tz", loc.String()))
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

	// Mark running for overlap control (shared state between cron invocations).
	if t.state != nil {
		t.state.mu.Lock()
		t.state.running = true
		t.state.mu.Unlock()
		defer func() {
			t.state.mu.Lock()
			t.state.running = false
			t.state.mu.Unlock()
		}()
	}

	// Copy scheduler config to avoid data races with Apply().
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()

	opt := t.opt.withDefaults(cfg)
	retries := opt.RetryMax
	if retries < 0 {
		retries = 0
	}

	var err error
	maxAttempts := 1 + retries
attemptLoop:
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Per-attempt timeout (so a timed-out first attempt doesn't poison retries).
		runCtx := ctx
		var cancel func()
		if t.timeout > 0 {
			runCtx, cancel = context.WithTimeout(ctx, t.timeout)
		}
		err = t.run(runCtx)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			break
		}
		if attempt >= maxAttempts {
			break
		}

		delay := backoffDelay(opt, attempt) // attempt=1 => first retry
		if delay > 0 {
			s.log.Debug("task retry", slog.String("task", t.name), slog.Int("attempt", attempt+1), slog.Duration("delay", delay), slog.String("err", err.Error()))
			tmr := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				if !tmr.Stop() {
					<-tmr.C
				}
				err = ctx.Err()
				break attemptLoop
			case <-s.stopCh:
				if !tmr.Stop() {
					<-tmr.C
				}
				err = errors.New("scheduler stopped")
				break attemptLoop
			case <-tmr.C:
			}
		}
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

func backoffDelay(opt TaskOptions, retry int) time.Duration {
	// retry starts at 1 (first retry)
	base := opt.RetryBase
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	maxD := opt.RetryMaxDelay
	if maxD <= 0 {
		maxD = 15 * time.Second
	}
	j := opt.RetryJitter
	if j <= 0 {
		j = 0.2
	}
	// exp growth
	d := base
	for i := 1; i < retry; i++ {
		d *= 2
		if d > maxD {
			d = maxD
			break
		}
	}
	// jitter [1-j, 1+j]
	if j > 0 {
		r := (randFloat64()*2 - 1) * j
		d = time.Duration(float64(d) * (1 + r))
		if d < 0 {
			d = 0
		}
	}
	if d > maxD {
		d = maxD
	}
	return d
}

var rngMu sync.Mutex

var rngOnce sync.Once

func randFloat64() float64 {
	rngOnce.Do(func() { rand.Seed(time.Now().UnixNano()) })
	rngMu.Lock()
	defer rngMu.Unlock()
	return rand.Float64()
}
