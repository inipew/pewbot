package systemd

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"pewbot/internal/core"
	sm "pewbot/pkg/systemdmanager"
)

type AutoRecoverConfig struct {
	Enabled         bool   `json:"enabled"`
	Interval        string `json:"interval"`
	MinDown         string `json:"min_down"`
	BackoffBase     string `json:"backoff_base"`
	BackoffMax      string `json:"backoff_max"`
	FailAlertStreak int    `json:"fail_alert_streak"`
	AlertInterval   string `json:"alert_interval"`
	RestartTimeout  string `json:"restart_timeout"`
}

type Config struct {
	Prefix      string            `json:"prefix"`
	AllowUnits  []string          `json:"allow_units"`
	AutoRecover AutoRecoverConfig `json:"auto_recover"`
}

type unitRecoverState struct {
	FailStreak int
	NextTry    time.Time
	LastErr    string
	LastAlert  time.Time
	Missing    bool
}

type Plugin struct {
	log  *slog.Logger
	deps core.PluginDeps

	mu     sync.RWMutex
	cfg    Config
	runCtx context.Context

	mgr *sm.ServiceManager

	// plugin-owned lifecycle for auto-recover schedule (safe under hot-reload)
	autoMu             sync.Mutex
	autoRecoverRunning int32 // atomic guard
	recoverState       map[string]*unitRecoverState
	missingWarned      map[string]bool
}

func New() *Plugin {
	return &Plugin{}
}
func (p *Plugin) Name() string { return "systemd" }

func (p *Plugin) Init(ctx context.Context, deps core.PluginDeps) error {
	p.deps = deps
	p.log = deps.Logger.With(slog.String("plugin", p.Name()))
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	// ctx here is the long-lived plugin ctx (cancelled on stop)
	p.mu.Lock()
	p.runCtx = ctx
	cfg := p.cfg
	p.mu.Unlock()

	// Ensure manager exists (in case OnConfigChange ran before Start but failed, or config applied after)
	p.ensureManager(cfg.AllowUnits)

	p.stopAutoRecover()
	p.startAutoRecover(ctx, cfg)
	return nil
}

func (p *Plugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	p.runCtx = nil
	p.mu.Unlock()

	p.stopAutoRecover()

	if p.mgr != nil {
		_ = p.mgr.Close()
		p.mgr = nil
	}
	return nil
}

func (p *Plugin) OnConfigChange(ctx context.Context, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	if c.Prefix == "" {
		c.Prefix = "svc: "
	}
	c.AllowUnits = normalizeUnits(c.AllowUnits)

	// auto-recover defaults
	if c.AutoRecover.BackoffBase == "" {
		c.AutoRecover.BackoffBase = "5s"
	}
	if c.AutoRecover.BackoffMax == "" {
		c.AutoRecover.BackoffMax = "5m"
	}
	if c.AutoRecover.AlertInterval == "" {
		c.AutoRecover.AlertInterval = "10m"
	}
	if c.AutoRecover.RestartTimeout == "" {
		c.AutoRecover.RestartTimeout = "15s"
	}
	if c.AutoRecover.FailAlertStreak <= 0 {
		c.AutoRecover.FailAlertStreak = 3
	}

	p.mu.Lock()
	p.cfg = c
	run := p.runCtx // snapshot
	p.mu.Unlock()

	p.ensureManager(c.AllowUnits)

	// Only restart background loop if plugin already started (run ctx available).
	if run != nil {
		p.stopAutoRecover()
		p.startAutoRecover(run, c)
	}
	return nil
}

func (p *Plugin) ensureManager(allow []string) {
	// if no allow list, keep manager nil
	if len(allow) == 0 {
		if p.mgr != nil {
			_ = p.mgr.Close()
			p.mgr = nil
		}
		return
	}
	// Recreate manager if nil or allow list changed.
	need := false
	if p.mgr == nil {
		need = true
	} else {
		cur := p.mgr.GetManagedServices()
		if !sameStringSlice(cur, allow) {
			need = true
		}
	}
	if !need {
		return
	}
	if p.mgr != nil {
		_ = p.mgr.Close()
		p.mgr = nil
	}
	mgr, err := sm.NewServiceManager(allow)
	if err != nil {
		p.log.Warn("failed to create systemd manager", slog.String("err", err.Error()))
		return
	}
	p.mgr = mgr
}
