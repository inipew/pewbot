package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"pewbot/internal/eventbus"
)

// AddSchedule parses schedule and registers either a cron or interval task.
//
// Supported schedule formats:
//   - Cron: "*/5 * * * *", "55 * * * *", "@hourly", "@every 55m"
//   - Interval duration: "55m", "2h30m"
//   - Interval HH:MM: "00:50" (50 minutes), "02:30" (2 hours 30 minutes)
func (s *Service) AddSchedule(name, schedule string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	return s.AddScheduleOpt(name, schedule, timeout, TaskOptions{}, job)
}

// AddScheduleOpt is AddSchedule with task options.
func (s *Service) AddScheduleOpt(name, schedule string, timeout time.Duration, opt TaskOptions, job func(ctx context.Context) error) (string, error) {
	ps, err := ParseSchedule(schedule)
	if err != nil {
		return "", err
	}
	switch ps.Kind {
	case SpecCron:
		return s.AddCronOpt(name, ps.Cron, timeout, opt, job)
	case SpecInterval:
		return s.AddIntervalOpt(name, ps.Every, timeout, opt, job)
	default:
		return "", fmt.Errorf("unsupported schedule kind")
	}
}

func (s *Service) AddCron(name, spec string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	return s.AddCronOpt(name, spec, timeout, TaskOptions{}, job)
}

func (s *Service) AddCronOpt(name, spec string, timeout time.Duration, opt TaskOptions, job func(ctx context.Context) error) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(name) == "" {
		return "", errors.New("name required")
	}
	// Upsert by name: remove previous schedule with the same name to prevent duplicates
	// across hot-reloads or repeated registrations.
	_ = s.removeScheduleLocked(name)
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
	if s.c != nil {
		err := s.addCronLocked(&s.defs[len(s.defs)-1])
		if err != nil {
			s.log.Error("schedule register failed", slog.String("name", name), slog.String("spec", spec), slog.Any("err", err))
		} else {
			next := s.previewNextRunsLocked(spec, 4)
			args := []any{slog.String("name", name), slog.String("id", id), slog.String("spec", spec), slog.Duration("timeout", d.timeout)}
			if next != "" {
				args = append(args, slog.String("next", next))
			}
			s.log.Debug("schedule registered", args...)
		}
		return id, err
	}
	// Scheduler not started/enabled yet: keep definition and register when Start() runs.
	return id, nil
}

func (s *Service) AddInterval(name string, every time.Duration, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	return s.AddIntervalOpt(name, every, timeout, TaskOptions{}, job)
}

func (s *Service) AddIntervalOpt(name string, every time.Duration, timeout time.Duration, opt TaskOptions, job func(ctx context.Context) error) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(name) == "" {
		return "", errors.New("name required")
	}
	_ = s.removeScheduleLocked(name)
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
	if s.c != nil {
		err := s.addCronLocked(&s.defs[len(s.defs)-1])
		if err != nil {
			s.log.Error("schedule register failed", slog.String("name", name), slog.String("spec", spec), slog.Any("err", err))
		} else {
			next := s.previewNextRunsLocked(spec, 4)
			args := []any{slog.String("name", name), slog.String("id", id), slog.String("spec", spec), slog.Duration("timeout", d.timeout)}
			if next != "" {
				args = append(args, slog.String("next", next))
			}
			s.log.Debug("schedule registered", args...)
		}
		return id, err
	}
	return id, nil
}

// Helper: daily at HH:MM (scheduler timezone)

func (s *Service) AddOnce(name string, at time.Time, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	if name == "" {
		return "", errors.New("name required")
	}
	if at.IsZero() {
		return "", errors.New("at required")
	}

	// snapshot location + default timeout config under s.mu
	s.mu.Lock()
	loc := s.loc
	cfg := s.cfg
	s.mu.Unlock()
	if loc == nil {
		loc = time.Local
	}
	runAt := at.In(loc)
	resolved := timeout
	if resolved <= 0 && cfg.DefaultTimeout > 0 {
		resolved = cfg.DefaultTimeout
	}

	localName := name
	localAt := runAt

	s.tmu.Lock()
	// upsert: stop existing timer with the same name
	if t, ok := s.timers[localName]; ok {
		_ = t.Stop()
		delete(s.timers, localName)
	}
	// bump version to ignore stale callbacks from previously scheduled timers
	ver := s.onceVer[localName] + 1
	s.onceVer[localName] = ver

	s.onceAt[localName] = localAt
	s.onceTimeout[localName] = resolved
	s.onceJob[localName] = job

	delay := time.Until(localAt)
	if delay < 0 {
		delay = 0
	}
	localVer := ver
	timer := time.AfterFunc(delay, func() {
		// If the task was removed or replaced, ignore this callback.
		s.tmu.Lock()
		curVer := s.onceVer[localName]
		jobNow := s.onceJob[localName]
		timeoutNow := s.onceTimeout[localName]
		_, okAt := s.onceAt[localName]
		if curVer != localVer || jobNow == nil || !okAt {
			s.tmu.Unlock()
			return
		}
		// cleanup persisted definition first (prevents double-exec on restart)
		delete(s.timers, localName)
		delete(s.onceAt, localName)
		delete(s.onceTimeout, localName)
		delete(s.onceJob, localName)
		delete(s.onceVer, localName)
		s.tmu.Unlock()

		// enqueue task
		s.mu.Lock()
		cfgNow := s.cfg
		s.mu.Unlock()
		s.enqueue(task{
			id:      fmt.Sprintf("once:%d", time.Now().UnixNano()),
			name:    localName,
			timeout: timeoutNow,
			run:     jobNow,
			opt:     TaskOptions{}.withDefaults(cfgNow),
			state:   &runState{},
		})
	})
	s.timers[localName] = timer
	s.tmu.Unlock()

	return localName, nil
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
// Safe to call even when scheduler is not started/enabled (it will still remove persisted defs).
func (s *Service) Remove(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	removed := false

	// Remove cron/interval schedules.
	s.mu.Lock()
	removed = s.removeScheduleLocked(name) || removed
	s.mu.Unlock()

	// Remove one-time timers/definitions.
	s.tmu.Lock()
	if t, ok := s.timers[name]; ok {
		_ = t.Stop()
		delete(s.timers, name)
		removed = true
	}
	if _, ok := s.onceAt[name]; ok {
		delete(s.onceAt, name)
		removed = true
	}
	if _, ok := s.onceTimeout[name]; ok {
		delete(s.onceTimeout, name)
		removed = true
	}
	if _, ok := s.onceJob[name]; ok {
		delete(s.onceJob, name)
		removed = true
	}
	if _, ok := s.onceVer[name]; ok {
		delete(s.onceVer, name)
		removed = true
	}
	s.tmu.Unlock()

	if removed {
		s.log.Debug("schedule removed", slog.String("name", name))
	}
	return removed
}

// removeScheduleLocked removes all defs matching name and unregisters them from cron if running.
// Call with s.mu held.
func (s *Service) removeScheduleLocked(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	removed := false
	if s.c != nil {
		for i := range s.defs {
			if s.defs[i].name == name && s.defs[i].entryID != 0 {
				s.c.Remove(s.defs[i].entryID)
				s.defs[i].entryID = 0
				removed = true
			}
		}
	}
	// remove from persisted defs regardless of running state
	n := 0
	for _, d := range s.defs {
		if d.name == name {
			removed = true
			continue
		}
		s.defs[n] = d
		n++
	}
	if n < len(s.defs) {
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
				s.log.Debug("schedule skipped (previous run still running)", slog.String("task", d.name))
				if s.bus != nil {
					now := time.Now()
					s.bus.Publish(eventbus.Event{Type: "task.skipped", Time: now, Data: TaskEvent{ID: d.id, Name: d.name, Started: now, Attempts: 0, Error: "overlap_skip"}})
				}
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
	s.log.Info("service restarted", slog.String("tz", loc.String()), slog.Int("schedules", len(s.defs)))
}

// rebuildOnceTimersLocked recreates runtime timers from the persisted once definitions.
// Call with s.mu held.
func (s *Service) rebuildOnceTimersLocked() {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	// stop any existing timers (should already be empty after Stop())
	for _, t := range s.timers {
		_ = t.Stop()
	}
	s.timers = map[string]*time.Timer{}

	// recreate timers from persisted definitions
	for name, runAt := range s.onceAt {
		job := s.onceJob[name]
		if job == nil {
			delete(s.onceAt, name)
			delete(s.onceTimeout, name)
			delete(s.onceJob, name)
			delete(s.onceVer, name)
			continue
		}
		ver := s.onceVer[name]
		if ver == 0 {
			ver = 1
			s.onceVer[name] = ver
		}
		localName := name
		localAt := runAt
		localVer := ver
		delay := time.Until(localAt)
		if delay < 0 {
			delay = 0
		}
		tmr := time.AfterFunc(delay, func() {
			// if the task was removed or replaced, ignore
			s.tmu.Lock()
			curVer := s.onceVer[localName]
			jobNow := s.onceJob[localName]
			timeoutNow := s.onceTimeout[localName]
			_, okAt := s.onceAt[localName]
			if curVer != localVer || jobNow == nil || !okAt {
				s.tmu.Unlock()
				return
			}
			delete(s.timers, localName)
			delete(s.onceAt, localName)
			delete(s.onceTimeout, localName)
			delete(s.onceJob, localName)
			delete(s.onceVer, localName)
			s.tmu.Unlock()

			s.mu.Lock()
			cfgNow := s.cfg
			s.mu.Unlock()
			s.enqueue(task{
				id:      fmt.Sprintf("once:%d", time.Now().UnixNano()),
				name:    localName,
				timeout: timeoutNow,
				run:     jobNow,
				opt:     TaskOptions{}.withDefaults(cfgNow),
				state:   &runState{},
			})
		})
		s.timers[localName] = tmr
	}
}

func (s *Service) loadLocationLocked() *time.Location {
	tz := strings.TrimSpace(s.cfg.Timezone)
	if tz == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		s.log.Warn("invalid timezone; falling back to Local", slog.String("tz", tz), slog.Any("err", err))
		return time.Local
	}
	return loc
}

func (s *Service) resolveTimeout(t time.Duration) time.Duration {
	if t > 0 {
		return t
	}
	if s.cfg.DefaultTimeout > 0 {
		return s.cfg.DefaultTimeout
	}
	return 0
}

// previewNextRunsLocked returns a short, human-friendly list of upcoming run times
// for the given cron spec. Call with s.mu held.
func (s *Service) previewNextRunsLocked(spec string, n int) string {
	if s.log == nil {
		return ""
	}
	if !s.log.Enabled(context.Background(), slog.LevelDebug) {
		return ""
	}
	if n <= 0 {
		return ""
	}
	loc := s.loc
	if loc == nil {
		loc = s.loadLocationLocked()
	}
	sched, err := s.parser.Parse(spec)
	if err != nil {
		return ""
	}
	// Use local time in scheduler TZ.
	t := time.Now().In(loc)
	var b strings.Builder
	for i := 0; i < n; i++ {
		t = sched.Next(t)
		if t.IsZero() {
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(t.Format("2006-01-02 15:04:05"))
	}
	return b.String()
}
