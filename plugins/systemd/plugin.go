package systemd

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"pewbot/internal/core"
	"pewbot/internal/pluginkit"
	sm "pewbot/pkg/systemdmanager"
)

// AutoRecoverOptions configures the behavior of the auto-recover feature.
//
// Scheduling is configured via Config.Scheduler.
type AutoRecoverOptions struct {
	MinDown         string `json:"min_down"`
	BackoffBase     string `json:"backoff_base"`
	BackoffMax      string `json:"backoff_max"`
	FailAlertStreak int    `json:"fail_alert_streak"`
	AlertInterval   string `json:"alert_interval"`
	RestartTimeout  string `json:"restart_timeout"`
}

type Config struct {
	Prefix      string                        `json:"prefix"`
	AllowUnits  []string                      `json:"allow_units"`
	Scheduler   pluginkit.SchedulerTaskConfig `json:"scheduler"`
	Timeouts    pluginkit.TimeoutsConfig      `json:"timeouts,omitempty"`
	AutoRecover AutoRecoverOptions            `json:"auto_recover"`

	allowSet map[string]struct{} `json:"-"`
}

type unitRecoverState struct {
	FailStreak int
	NextTry    time.Time
	LastErr    string
	LastAlert  time.Time
	Missing    bool
}

type Plugin struct {
	pluginkit.EnhancedPluginBase

	mu  sync.RWMutex
	cfg Config
	mgr *sm.ServiceManager

	autoMu       sync.Mutex
	recoverState map[string]*unitRecoverState
	missingWarn  map[string]bool
	autoTask     string // last scheduled short name
}

func New() *Plugin             { return &Plugin{} }
func (p *Plugin) Name() string { return "systemd" }

func (p *Plugin) Init(ctx context.Context, deps core.PluginDeps) error {
	p.InitEnhanced(deps, p.Name())
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	p.StartEnhanced(ctx)
	cfg := p.cfgSnapshot()
	p.ensureManager(cfg.AllowUnits)
	p.reconcileAutoRecover(ctx, cfg)
	return nil
}

func (p *Plugin) Stop(ctx context.Context) error {
	// Stop schedules first (auto cleanup), then tear down plugin-owned resources.
	err := p.StopEnhanced(ctx)
	p.resetAutoState()
	p.closeManager()
	return err
}

func (p *Plugin) OnConfigChange(ctx context.Context, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}

	// defaults
	if c.Prefix == "" {
		c.Prefix = "svc: "
	}
	c.AllowUnits = normalizeUnits(c.AllowUnits)
	c.allowSet = make(map[string]struct{}, len(c.AllowUnits))
	for _, u := range c.AllowUnits {
		c.allowSet[u] = struct{}{}
	}
	applyAutoRecoverDefaults(&c.AutoRecover)

	// Validate optional standardized timeouts.
	if err := c.Timeouts.Validate("systemd.timeouts"); err != nil {
		return err
	}

	// Scheduler defaults.
	if c.Scheduler.TaskName == "" {
		c.Scheduler.TaskName = "auto_recover"
	}
	if c.Scheduler.Enabled && c.Scheduler.Schedule == "" {
		return fmt.Errorf("systemd.scheduler.schedule is required when scheduler.enabled=true")
	}
	if c.Scheduler.Enabled {
		if _, err := core.ParseSchedule(c.Scheduler.Schedule); err != nil {
			return fmt.Errorf("invalid systemd.scheduler.schedule: %w", err)
		}
	}

	p.mu.Lock()
	p.cfg = c
	p.mu.Unlock()

	p.ensureManager(c.AllowUnits)

	// Only reconcile background schedules if running.
	run := p.Context()
	if run != nil {
		p.reconcileAutoRecover(ctx, c)
	}
	return nil
}

func (p *Plugin) cfgSnapshot() Config {
	p.mu.RLock()
	c := p.cfg
	c.AllowUnits = append([]string(nil), p.cfg.AllowUnits...)
	p.mu.RUnlock()
	return c
}

func (p *Plugin) mgrSnapshot() *sm.ServiceManager {
	p.mu.RLock()
	m := p.mgr
	p.mu.RUnlock()
	return m
}

func (p *Plugin) closeManager() {
	p.mu.Lock()
	m := p.mgr
	p.mgr = nil
	p.mu.Unlock()
	if m != nil {
		_ = m.Close()
	}
}

func (p *Plugin) ensureManager(allow []string) {
	// if no allow list, keep manager nil
	if len(allow) == 0 {
		p.closeManager()
		return
	}

	p.mu.RLock()
	cur := p.mgr
	p.mu.RUnlock()

	if cur != nil {
		managed := cur.GetManagedServices()
		if sameStringSlice(managed, allow) {
			return
		}
		p.closeManager()
	}

	mgr, err := sm.NewServiceManagerContext(p.Context(), allow)
	if err != nil {
		p.Log.Warn("failed to create systemd manager", "err", err)
		return
	}
	p.mu.Lock()
	p.mgr = mgr
	p.mu.Unlock()
}

func applyAutoRecoverDefaults(c *AutoRecoverOptions) {
	if c.BackoffBase == "" {
		c.BackoffBase = "5s"
	}
	if c.BackoffMax == "" {
		c.BackoffMax = "5m"
	}
	if c.AlertInterval == "" {
		c.AlertInterval = "10m"
	}
	if c.RestartTimeout == "" {
		c.RestartTimeout = "15s"
	}
	if c.MinDown == "" {
		c.MinDown = "3s"
	}
	if c.FailAlertStreak <= 0 {
		c.FailAlertStreak = 3
	}
}
