package scheduler

import (
	"context"
	"errors"
	"fmt"
	logx "pewbot/pkg/logx"
	"strconv"
	"strings"
	"time"

	"pewbot/internal/task/engine"

	"github.com/robfig/cron/v3"
)

// AddSchedule parses schedule and registers either a cron or interval task.
//
// Supported schedule formats:
//   - Cron: "*/5 * * * *", "55 * * * *", "@hourly", "@every 55m"
//   - Interval duration: "55m", "2h30m"
//   - Interval HH:MM: "00:50" (50 minutes), "02:30" (2 hours 30 minutes)
func (s *Service) AddSchedule(name, schedule string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	// Default for scheduled jobs is to skip if a previous run is already in-flight
	// (or queued), to avoid queue blow-ups.
	return s.AddScheduleOpt(name, schedule, timeout, TaskOptions{Overlap: OverlapSkipIfRunning}, job)
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
	return s.AddCronOpt(name, spec, timeout, TaskOptions{Overlap: OverlapSkipIfRunning}, job)
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
	s.removeOnceLocked(name)
	id := fmt.Sprintf("cron:%d", time.Now().UnixNano())
	d := scheduleDef{
		id:      id,
		name:    name,
		spec:    spec,
		timeout: timeout,
		job:     job,
		opt:     opt,
		state:   &engine.RunState{},
	}
	s.defs = append(s.defs, d)
	if s.c != nil {
		err := s.addCronLocked(&s.defs[len(s.defs)-1])
		if err != nil {
			s.log.Error("schedule register failed", logx.String("name", name), logx.String("spec", spec), logx.Any("err", err))
		} else {
			next := s.previewNextRunsLocked(spec, 4)
			args := []logx.Field{logx.String("name", name), logx.String("id", id), logx.String("spec", spec), logx.Duration("timeout", d.timeout)}
			if next != "" {
				args = append(args, logx.String("next", next))
			}
			s.log.Debug("schedule registered", args...)
		}
		// Return the schedule name (stable identifier for Remove(name)).
		return name, err
	}
	// Scheduler not started/enabled yet: keep definition and register when Start() runs.
	return name, nil
}

func (s *Service) AddInterval(name string, every time.Duration, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	return s.AddIntervalOpt(name, every, timeout, TaskOptions{Overlap: OverlapSkipIfRunning}, job)
}

func (s *Service) AddIntervalOpt(name string, every time.Duration, timeout time.Duration, opt TaskOptions, job func(ctx context.Context) error) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(name) == "" {
		return "", errors.New("name required")
	}
	_ = s.removeScheduleLocked(name)
	s.removeOnceLocked(name)
	id := fmt.Sprintf("interval:%d", time.Now().UnixNano())
	spec := fmt.Sprintf("@every %s", every.String())
	d := scheduleDef{
		id:      id,
		name:    name,
		spec:    spec,
		timeout: timeout,
		job:     job,
		opt:     opt,
		state:   &engine.RunState{},
	}
	s.defs = append(s.defs, d)
	if s.c != nil {
		err := s.addCronLocked(&s.defs[len(s.defs)-1])
		if err != nil {
			s.log.Error("schedule register failed", logx.String("name", name), logx.String("spec", spec), logx.Any("err", err))
		} else {
			next := s.previewNextRunsLocked(spec, 4)
			args := []logx.Field{logx.String("name", name), logx.String("id", id), logx.String("spec", spec), logx.Duration("timeout", d.timeout)}
			if next != "" {
				args = append(args, logx.String("next", next))
			}
			s.log.Debug("schedule registered", args...)
		}
		// Return the schedule name (stable identifier for Remove(name)).
		return name, err
	}
	return name, nil
}

// Helper: daily at HH:MM (scheduler timezone)

func (s *Service) AddOnce(name string, at time.Time, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	if name == "" {
		return "", errors.New("name required")
	}
	if at.IsZero() {
		return "", errors.New("at required")
	}

	// snapshot location under s.mu (also remove any cron/interval schedule with the same name)
	s.mu.Lock()
	loc := s.loc
	_ = s.removeScheduleLocked(name)
	s.mu.Unlock()
	if loc == nil {
		loc = time.Local
	}
	runAt := at.In(loc)

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
	s.onceTimeout[localName] = timeout
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

		if s.engine != nil {
			err := s.engine.Enqueue(engine.Task{
				ID:      "",
				Name:    localName,
				Timeout: timeoutNow,
				Run:     jobNow,
				Opt:     TaskOptions{},
				State:   &engine.RunState{},
			})
			if err != nil {
				s.reportEnqueueError(localName, err)
			}
		}
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
	return s.AddCronOpt(name, spec, timeout, TaskOptions{Overlap: OverlapSkipIfRunning}, job)
}

// Helper: weekly at HH:MM for given weekday (scheduler timezone)
func (s *Service) AddWeekly(name string, weekday time.Weekday, atHHMM string, timeout time.Duration, job func(ctx context.Context) error) (string, error) {
	h, m, err := parseHHMM(atHHMM)
	if err != nil {
		return "", err
	}
	dow := int(weekday) // Sunday=0
	spec := fmt.Sprintf("%d %d * * %d", m, h, dow)
	return s.AddCronOpt(name, spec, timeout, TaskOptions{Overlap: OverlapSkipIfRunning}, job)
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
		s.log.Debug("schedule removed", logx.String("name", name))
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

func (s *Service) removeOnceLocked(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	removed := false
	s.tmu.Lock()
	defer s.tmu.Unlock()
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
	return removed
}

func (s *Service) addCronLocked(d *scheduleDef) error {
	// Apply startup spread only for interval schedules (@every ...), to avoid
	// thundering herd right after service start.
	spec := strings.TrimSpace(d.spec)
	job := cron.FuncJob(func() {
		if s.engine == nil {
			return
		}
		err := s.engine.Enqueue(engine.Task{
			ID:      "",
			Name:    d.name,
			Timeout: d.timeout,
			Run:     d.job,
			Opt:     d.opt,
			State:   d.state,
		})
		if err != nil {
			s.reportEnqueueError(d.name, err)
		}
	})

	if strings.HasPrefix(spec, "@every") {
		everyStr := strings.TrimSpace(strings.TrimPrefix(spec, "@every"))
		every, err := time.ParseDuration(everyStr)
		if err == nil && every > 0 {
			loc := s.loc
			if loc == nil {
				loc = time.Local
			}
			sched, jitter := makeIntervalScheduleWithSpread(every, time.Now().In(loc), d.name)
			d.startupSpread = jitter
			eid := s.c.Schedule(sched, job)
			d.entryID = eid
			return nil
		}
	}

	// Fallback: normal cron parsing.
	d.startupSpread = 0
	eid, err := s.c.AddJob(d.spec, job)
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
	s.log.Info("service restarted", logx.String("tz", loc.String()), logx.Int("schedules", len(s.defs)))
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

			if s.engine != nil {
				err := s.engine.Enqueue(engine.Task{
					ID:      "",
					Name:    localName,
					Timeout: timeoutNow,
					Run:     jobNow,
					Opt:     TaskOptions{},
					State:   &engine.RunState{},
				})
				if err != nil {
					s.reportEnqueueError(localName, err)
				}
			}
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
		s.log.Warn("invalid timezone; falling back to Local", logx.String("tz", tz), logx.Any("err", err))
		return time.Local
	}
	return loc
}

// previewNextRunsLocked returns a short, human-friendly list of upcoming run times
// for the given cron spec. Call with s.mu held.
func (s *Service) previewNextRunsLocked(spec string, n int) string {
	if s.log.IsZero() {
		return ""
	}
	if !s.log.Enabled(logx.LevelDebug) {
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
