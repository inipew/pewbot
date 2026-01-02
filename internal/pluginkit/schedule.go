package pluginkit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"pewbot/internal/core"
)

// ScheduleHelper provides a fluent, plugin-scoped API over core.SchedulerPort.
//
// It automatically namespaces tasks as "<plugin>:<name>" to avoid collisions
// across plugins, and tracks registered task names for automatic cleanup.
type ScheduleHelper struct {
	pluginName string
	svc        core.SchedulerPort

	mu    sync.Mutex
	tasks map[string]struct{} // fullName set
	ctx   context.Context
}

func NewScheduleHelper(pluginName string, services *core.Services) *ScheduleHelper {
	var svc core.SchedulerPort
	if services != nil {
		svc = services.Scheduler
	}
	return &ScheduleHelper{
		pluginName: pluginName,
		svc:        svc,
		tasks:      map[string]struct{}{},
	}
}

func (h *ScheduleHelper) bindContext(ctx context.Context) { h.ctx = ctx }

// Spec parses a schedule string and returns a builder for either a cron or interval task.
//
// Supported formats:
//   - Cron: "*/5 * * * *", "55 * * * *", "@hourly", "@every 55m"
//   - Interval duration: "55m", "2h30m"
//   - Interval HH:MM: "00:50" (50 minutes), "02:30" (2 hours 30 minutes)
//
// For interval schedules, the first run is relative to the time the scheduler starts
// (robfig/cron's "@every" behavior).
func (h *ScheduleHelper) Spec(name, schedule string) *ScheduleBuilder {
	b := &ScheduleBuilder{helper: h, name: name, timeout: 30 * time.Second}
	ps, err := core.ParseSchedule(schedule)
	if err != nil {
		b.parseErr = err
		return b
	}
	switch ps.Kind {
	case core.ScheduleCron:
		b.kind = kindCron
		b.spec = ps.Cron
	case core.ScheduleInterval:
		b.kind = kindInterval
		b.interval = ps.Every
	default:
		b.parseErr = fmt.Errorf("unsupported schedule kind")
	}
	return b
}

// Cron begins building a cron schedule.
func (h *ScheduleHelper) Cron(name, spec string) *ScheduleBuilder {
	return &ScheduleBuilder{
		helper:  h,
		name:    name,
		kind:    kindCron,
		spec:    spec,
		timeout: 30 * time.Second,
	}
}

// Every begins building an interval schedule.
func (h *ScheduleHelper) Every(name string, interval time.Duration) *ScheduleBuilder {
	return &ScheduleBuilder{
		helper:   h,
		name:     name,
		kind:     kindInterval,
		interval: interval,
		timeout:  30 * time.Second,
	}
}

// At begins building a one-time schedule.
func (h *ScheduleHelper) At(name string, t time.Time) *ScheduleBuilder {
	return &ScheduleBuilder{
		helper:  h,
		name:    name,
		kind:    kindOnce,
		at:      t,
		timeout: 30 * time.Second,
	}
}

// Daily begins building a daily HH:MM schedule.
func (h *ScheduleHelper) Daily(name, timeHHMM string) *ScheduleBuilder {
	return &ScheduleBuilder{
		helper:   h,
		name:     name,
		kind:     kindDaily,
		timeHHMM: timeHHMM,
		timeout:  30 * time.Second,
	}
}

// Remove removes a specific scheduled task by short name (auto-namespaced).
func (h *ScheduleHelper) Remove(name string) {
	if h == nil || h.svc == nil {
		return
	}
	fullName := h.ns(name)
	h.svc.Remove(fullName)

	h.mu.Lock()
	delete(h.tasks, fullName)
	h.mu.Unlock()
}

// cleanup removes all schedules registered through this helper.
// Called automatically by EnhancedPluginBase.StopEnhanced.
func (h *ScheduleHelper) cleanup() {
	if h == nil || h.svc == nil {
		return
	}
	h.mu.Lock()
	keys := make([]string, 0, len(h.tasks))
	for k := range h.tasks {
		keys = append(keys, k)
	}
	h.tasks = map[string]struct{}{}
	h.mu.Unlock()

	for _, fullName := range keys {
		h.svc.Remove(fullName)
	}
}

func (h *ScheduleHelper) track(fullName string) {
	h.mu.Lock()
	h.tasks[fullName] = struct{}{}
	h.mu.Unlock()
}

func (h *ScheduleHelper) ns(name string) string {
	if h.pluginName == "" {
		return name
	}
	if name == "" {
		return h.pluginName
	}
	return h.pluginName + ":" + name
}

type scheduleKind uint8

const (
	kindCron scheduleKind = iota
	kindInterval
	kindOnce
	kindDaily
)

// ScheduleBuilder configures schedule options before registering it.
type ScheduleBuilder struct {
	helper   *ScheduleHelper
	name     string
	kind     scheduleKind
	spec     string
	interval time.Duration
	at       time.Time
	timeHHMM string
	timeout  time.Duration
	overlap  core.TaskOptions
	parseErr error
}

func (b *ScheduleBuilder) Timeout(d time.Duration) *ScheduleBuilder {
	b.timeout = d
	return b
}

func (b *ScheduleBuilder) SkipIfRunning() *ScheduleBuilder {
	b.overlap.Overlap = core.OverlapSkipIfRunning
	return b
}

func (b *ScheduleBuilder) AllowOverlap() *ScheduleBuilder {
	b.overlap.Overlap = core.OverlapAllow
	return b
}

// Do registers the schedule and tracks it for automatic cleanup.
//
// NOTE: it tracks by full task name (not returned taskID), because core scheduler
// removes schedules by name.
func (b *ScheduleBuilder) Do(job func(ctx context.Context) error) error {
	if b == nil || b.helper == nil || b.helper.svc == nil {
		return errors.New("scheduler not available")
	}
	if b.parseErr != nil {
		return b.parseErr
	}
	fullName := b.helper.ns(b.name)

	var err error
	switch b.kind {
	case kindCron:
		_, err = b.helper.svc.AddCronOpt(fullName, b.spec, b.timeout, b.overlap, job)
	case kindInterval:
		_, err = b.helper.svc.AddIntervalOpt(fullName, b.interval, b.timeout, b.overlap, job)
	case kindOnce:
		_, err = b.helper.svc.AddOnce(fullName, b.at, b.timeout, job)
	case kindDaily:
		_, err = b.helper.svc.AddDaily(fullName, b.timeHHMM, b.timeout, job)
	default:
		return fmt.Errorf("unknown schedule kind: %d", b.kind)
	}
	if err != nil {
		return err
	}
	b.helper.track(fullName)
	return nil
}
