package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"pewbot/internal/kit"
)

type Plugin interface {
	Name() string
	Init(ctx context.Context, deps PluginDeps) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Commands() []Command
}

type CallbackProvider interface {
	Callbacks() []CallbackRoute
}

type ConfigurablePlugin interface {
	OnConfigChange(ctx context.Context, raw json.RawMessage) error
}

type PluginDeps struct {
	Logger      *slog.Logger
	Adapter     kit.Adapter
	Config      *ConfigManager
	Services    *Services
	OwnerUserID []int64
}

type PluginManager struct {
	mu sync.Mutex

	log  *slog.Logger
	cfgm *ConfigManager
	deps PluginDeps
	reg  map[string]Plugin
	run  map[string]bool
	cmdm *CommandManager
}

func NewPluginManager(log *slog.Logger, cfgm *ConfigManager, deps PluginDeps, cmdm *CommandManager) *PluginManager {
	return &PluginManager{
		log:  log,
		cfgm: cfgm,
		deps: deps,
		reg:  map[string]Plugin{},
		run:  map[string]bool{},
		cmdm: cmdm,
	}
}

func (pm *PluginManager) Register(p ...Plugin) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for _, pl := range p {
		pm.reg[pl.Name()] = pl
	}
}

func (pm *PluginManager) StartAll(ctx context.Context) error {
	return pm.reconcile(ctx, pm.cfgm.Get())
}

func (pm *PluginManager) StopAll(ctx context.Context) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for name, p := range pm.reg {
		if pm.run[name] {
			_ = p.Stop(ctx)
			pm.run[name] = false
		}
	}
	pm.refreshRegistryLocked()
}

func (pm *PluginManager) OnConfigUpdate(ctx context.Context, cfg *Config) {
	_ = pm.reconcile(ctx, cfg)
}

func (pm *PluginManager) reconcile(ctx context.Context, cfg *Config) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for name, p := range pm.reg {
		raw, ok := cfg.Plugins[name]
		enabled := ok && raw.Enabled

		if enabled && !pm.run[name] {
			if err := p.Init(ctx, pm.deps); err != nil {
				pm.log.Error("plugin init failed", slog.String("plugin", name), slog.String("err", err.Error()))
				continue
			}
			if cp, ok := p.(ConfigurablePlugin); ok {
				_ = cp.OnConfigChange(ctx, raw.Config)
			}
			if err := p.Start(ctx); err != nil {
				pm.log.Error("plugin start failed", slog.String("plugin", name), slog.String("err", err.Error()))
				continue
			}
			pm.run[name] = true
			pm.log.Info("plugin started", slog.String("plugin", name))
		}

		if !enabled && pm.run[name] {
			_ = p.Stop(ctx)
			pm.run[name] = false
			pm.log.Info("plugin stopped", slog.String("plugin", name))
		}

		if enabled && pm.run[name] {
			if cp, ok := p.(ConfigurablePlugin); ok {
				_ = cp.OnConfigChange(ctx, raw.Config)
			}
		}
	}

	pm.refreshRegistryLocked()
	return nil
}

func (pm *PluginManager) refreshRegistryLocked() {
	cfg := pm.cfgm.Get()
	var cmds []Command
	var cbs []CallbackRoute

	for name, p := range pm.reg {
		if !pm.run[name] {
			continue
		}
		raw := cfg.Plugins[name]
		pto, has := raw.TimeoutDuration()

		for _, c := range p.Commands() {
			c.PluginName = name
			// If plugin timeout set and command doesn't override, apply it.
			if has && c.Timeout <= 0 {
				c.Timeout = pto
			}
			cmds = append(cmds, c)
		}

		if cbp, ok := p.(CallbackProvider); ok {
			for _, r := range cbp.Callbacks() {
				r.Plugin = name // enforce plugin namespace
				if has && r.Timeout <= 0 {
					r.Timeout = pto
				}
				cbs = append(cbs, r)
			}
		}
	}

	pm.cmdm.SetRegistry(cmds, cbs)
}

func (pm *PluginManager) DebugStatus() string {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	out := ""
	for name := range pm.reg {
		out += fmt.Sprintf("- %s: %v\n", name, pm.run[name])
	}
	return out
}

func mustDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}
