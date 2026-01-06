package pluginkit

import (
	"context"
	"errors"
	"fmt"
	logx "pewbot/pkg/logx"
	"sync"
	"sync/atomic"
	"time"

	"pewbot/internal/eventbus"
	core "pewbot/internal/plugin"
)

// ScheduleHelper provides a fluent, plugin-scoped API over core.SchedulerPort.
//
// It automatically namespaces tasks as "<plugin>:<name>" to avoid collisions
// across plugins, and tracks registered task names for automatic cleanup.
type ScheduleHelper struct {
	pluginName string
	svc        core.SchedulerPort
	bus        eventbus.Bus
	log        logx.Logger

	mu    sync.Mutex
	tasks map[string]struct{} // fullName set
	ctx   context.Context
}

// TaskCancelledByPluginEvent is emitted when a scheduled task run is cancelled
// due to the plugin lifecycle context being canceled (e.g. plugin disabled).
//
// This is a best-effort operational signal to help operators understand why a
// task stopped early.
type TaskCancelledByPluginEvent struct {
	Plugin   string        `json:"plugin"`
	Task     string        `json:"task"`
	FullName string        `json:"full_name"`
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration"`
	Error    string        `json:"error,omitempty"`
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func NewScheduleHelper(pluginName string, deps core.PluginDeps) *ScheduleHelper {
	var svc core.SchedulerPort
	if deps.Services != nil {
		svc = deps.Services.Scheduler
	}
	lg := deps.Logger
	if lg.IsZero() {
		lg = logx.Nop()
	}
	return &ScheduleHelper{
		pluginName: pluginName,
		svc:        svc,
		bus:        deps.Bus,
		log:        lg.With(logx.String("plugin", pluginName), logx.String("component", "schedule")),
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
	// Default behavior: skip if a previous run is already in-flight (or queued).
	b.overlap.Overlap = core.OverlapSkipIfRunning
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
		overlap: core.TaskOptions{Overlap: core.OverlapSkipIfRunning},
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
		overlap:  core.TaskOptions{Overlap: core.OverlapSkipIfRunning},
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
		overlap: core.TaskOptions{Overlap: core.OverlapSkipIfRunning},
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
		overlap:  core.TaskOptions{Overlap: core.OverlapSkipIfRunning},
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
	if job == nil {
		return errors.New("job is nil")
	}
	fullName := b.helper.ns(b.name)

	// Ensure scheduled jobs are also bound to the plugin lifecycle context.
	//
	// Scheduler/taskengine provide the per-run context (runCtx) which is still the
	// primary owner of deadlines/timeouts and bot shutdown. We add pluginCtx as an
	// additional cancellation source so that disabling/stopping a plugin cancels
	// any in-flight job sooner.
	//
	// This is intentionally implemented without spawning an extra goroutine per
	// task run. On Go 1.22+ context.AfterFunc can register cancellation callbacks
	// efficiently when pluginCtx is cancelable.
	pluginCtx := b.helper.ctx
	wrapped := job
	if pluginCtx != nil {
		wrapped = func(runCtx context.Context) error {
			started := time.Now()
			var cancelledByPlugin atomic.Bool
			cctx, cancel := context.WithCancel(runCtx)
			stop := context.AfterFunc(pluginCtx, func() {
				// Only mark + cancel if the run is still active.
				select {
				case <-runCtx.Done():
					return
				default:
				}
				cancelledByPlugin.Store(true)
				cancel()
			})
			defer func() {
				_ = stop() // stop returns whether it stopped the callback; ignore.
				cancel()
			}()

			err := job(cctx)
			// Publish an operator-friendly event if the run was cancelled because the
			// plugin lifecycle context ended (and not because the scheduler stopped).
			if cancelledByPlugin.Load() && runCtx.Err() == nil && (errors.Is(err, context.Canceled) || errors.Is(cctx.Err(), context.Canceled)) {
				ev := TaskCancelledByPluginEvent{
					Plugin:   b.helper.pluginName,
					Task:     b.name,
					FullName: fullName,
					Started:  started,
					Duration: time.Since(started),
					Error:    errString(err),
				}
				if b.helper.bus != nil {
					b.helper.bus.Publish(eventbus.Event{Type: "task.cancelled_by_plugin", Data: ev})
				}
				if !b.helper.log.IsZero() {
					b.helper.log.Info("scheduled task cancelled by plugin", logx.String("task", fullName), logx.String("plugin", b.helper.pluginName), logx.Duration("duration", ev.Duration))
				}
			}
			return err
		}
	}

	var err error
	switch b.kind {
	case kindCron:
		_, err = b.helper.svc.AddCronOpt(fullName, b.spec, b.timeout, b.overlap, wrapped)
	case kindInterval:
		_, err = b.helper.svc.AddIntervalOpt(fullName, b.interval, b.timeout, b.overlap, wrapped)
	case kindOnce:
		_, err = b.helper.svc.AddOnce(fullName, b.at, b.timeout, wrapped)
	case kindDaily:
		_, err = b.helper.svc.AddDaily(fullName, b.timeHHMM, b.timeout, wrapped)
	default:
		return fmt.Errorf("unknown schedule kind: %d", b.kind)
	}
	if err != nil {
		return err
	}
	b.helper.track(fullName)
	return nil
}
