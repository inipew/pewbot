package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
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

type ConfigurablePlugin interface {
	OnConfigChange(ctx context.Context, raw json.RawMessage) error
}

type CallbackProvider interface {
	Callbacks() []CallbackRoute
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
	// last config blob hash per running plugin (used to avoid redundant OnConfigChange calls)
	lastRawHash map[string]uint64
	// last hash of selected global config values that plugins may implicitly depend on
	lastGlobalHash uint64

	// Internal, long-lived base context for all plugin contexts.
	// IMPORTANT: baseCtx is NOT the app ctx passed to StartAll/OnConfigUpdate (which may be call-scoped).
	// We "bind" app ctx only as a bridge: when appCtx is done, baseCancel is called.
	baseCtx    context.Context
	baseCancel context.CancelFunc
	bound      bool

	// per-plugin run context (cancelled on disable/stop)
	pctx    map[string]context.Context
	pcancel map[string]context.CancelFunc

	cmdm *CommandManager
}

func NewPluginManager(log *slog.Logger, cfgm *ConfigManager, deps PluginDeps, cmdm *CommandManager) *PluginManager {
	baseCtx, baseCancel := context.WithCancel(context.Background())
	return &PluginManager{
		log:            log,
		cfgm:           cfgm,
		deps:           deps,
		reg:            map[string]Plugin{},
		run:            map[string]bool{},
		lastRawHash:    map[string]uint64{},
		lastGlobalHash: 0,
		baseCtx:        baseCtx,
		baseCancel:     baseCancel,
		pctx:           map[string]context.Context{},
		pcancel:        map[string]context.CancelFunc{},
		cmdm:           cmdm,
	}
}

// globalDepsHash captures a small, conservative subset of config that plugins might implicitly depend on.
// Keeping this small avoids poking unrelated plugins on common service-level config changes.
func globalDepsHash(cfg *Config) uint64 {
	if cfg == nil {
		return 0
	}
	type deps struct {
		Telegram struct {
			OwnerUserIDs []int64 `json:"owner_user_ids"`
			GroupLog     string  `json:"group_log"`
		} `json:"telegram"`
	}
	var d deps
	d.Telegram.OwnerUserIDs = cfg.Telegram.OwnerUserIDs
	d.Telegram.GroupLog = cfg.Telegram.GroupLog
	b, _ := json.Marshal(d)
	return hashBytes(b)
}

// BindContext binds appCtx to baseCtx via cancellation bridge. First non-nil bind wins.
// This avoids plugins dying because caller passed a short-lived ctx into StartAll/OnConfigUpdate.
func (pm *PluginManager) BindContext(appCtx context.Context) {
	pm.mu.Lock()
	if pm.bound || appCtx == nil {
		pm.mu.Unlock()
		return
	}
	pm.bound = true
	baseCancel := pm.baseCancel
	pm.mu.Unlock()

	go func() {
		<-appCtx.Done()
		baseCancel()
	}()
}

func (pm *PluginManager) ctxOr(fallback context.Context, name string) context.Context {
	pm.mu.Lock()
	pctx := pm.pctx[name]
	pm.mu.Unlock()
	if pctx != nil {
		return pctx
	}
	return fallback
}

func (pm *PluginManager) Register(p ...Plugin) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for _, pl := range p {
		pm.reg[pl.Name()] = pl
	}
	pm.refreshRegistryLocked(pm.cfgm.Get())
}

func (pm *PluginManager) StartAll(ctx context.Context) error {
	pm.BindContext(ctx)
	return pm.reconcile(pm.cfgm.Get())
}

func (pm *PluginManager) StopAll(ctx context.Context, reason StopReason) {
	pm.mu.Lock()
	names := make([]string, 0, len(pm.reg))
	for name := range pm.reg {
		names = append(names, name)
	}
	pm.mu.Unlock()

	for _, name := range names {
		pm.stopOne(ctx, name, reason)
	}

	pm.mu.Lock()
	pm.refreshRegistryLocked(pm.cfgm.Get())
	pm.mu.Unlock()
}

func (pm *PluginManager) OnConfigUpdate(ctx context.Context, cfg *Config) {
	pm.BindContext(ctx)
	_ = pm.reconcile(cfg)
}

// SetOwnerUserIDs updates the owner list in PluginDeps so plugins that rely on deps.OwnerUserID
// can observe changes after a hot-reload.
func (pm *PluginManager) SetOwnerUserIDs(ids []int64) {
	cp := append([]int64(nil), ids...)
	pm.mu.Lock()
	pm.deps.OwnerUserID = cp
	pm.mu.Unlock()
}

func (pm *PluginManager) stopOne(stopCtx context.Context, name string, reason StopReason) {
	pm.mu.Lock()
	p := pm.reg[name]
	running := pm.run[name]
	cancel := pm.pcancel[name]
	pctx := pm.pctx[name]
	pm.mu.Unlock()

	if !running || p == nil {
		return
	}

	start := time.Now()
	pm.log.Debug("stopping plugin", slog.String("plugin", name), slog.String("reason", string(reason)))

	// cancel plugin context first (stop background loops promptly)
	if cancel != nil {
		cancel()
	}

	// call Stop with stopCtx, but do not allow a misbehaving plugin to block shutdown forever.
	done := make(chan struct{})
	go func() {
		_ = pm.safeCall("plugin.stop."+name, func() error { return p.Stop(stopCtx) })
		close(done)
	}()
	select {
	case <-done:
		// ok
	case <-stopCtx.Done():
		pm.log.Warn("plugin stop timeout (continuing)", slog.String("plugin", name), slog.String("err", stopCtx.Err().Error()))
	}

	pm.mu.Lock()
	pm.run[name] = false
	delete(pm.pctx, name)
	delete(pm.pcancel, name)
	delete(pm.lastRawHash, name)
	pm.mu.Unlock()

	took := time.Since(start)
	if took >= 500*time.Millisecond {
		pm.log.Info("plugin stopped", slog.String("plugin", name), slog.String("reason", string(reason)), slog.Duration("took", took), slog.Bool("ctx_was_set", pctx != nil))
	} else {
		pm.log.Debug("plugin stopped", slog.String("plugin", name), slog.String("reason", string(reason)), slog.Duration("took", took), slog.Bool("ctx_was_set", pctx != nil))
	}
}

func (pm *PluginManager) reconcile(cfg *Config) error {
	// compute global dependency hash once per reconcile (kept intentionally small)
	newGlobal := globalDepsHash(cfg)
	pm.mu.Lock()
	globalChanged := newGlobal != pm.lastGlobalHash
	pm.mu.Unlock()

	// snapshot desired actions without holding lock during plugin calls
	type op struct {
		name    string
		p       Plugin
		raw     PluginConfigRaw
		enabled bool
		run     bool
	}
	pm.mu.Lock()
	ops := make([]op, 0, len(pm.reg))
	for name, p := range pm.reg {
		raw, ok := cfg.Plugins[name]
		enabled := ok && raw.Enabled
		running := pm.run[name]
		ops = append(ops, op{name: name, p: p, raw: raw, enabled: enabled, run: running})
	}
	pm.mu.Unlock()

	const callTimeout = 10 * time.Second

	for _, o := range ops {
		switch {
		case o.enabled && !o.run:
			pm.log.Debug("plugin enable requested", slog.String("plugin", o.name))
			// start: create LONG-LIVED plugin ctx from internal base ctx
			pctx, cancel := context.WithCancel(pm.baseCtx)

			// init (bounded by timeout ctx)
			{
				ictx, icancel := context.WithTimeout(pctx, callTimeout)
				err := pm.safeCall("plugin.init."+o.name, func() error { return o.p.Init(ictx, pm.deps) })
				icancel()
				if err != nil {
					pm.log.Error("plugin init failed", slog.String("plugin", o.name), slog.Any("err", err))
					cancel()
					continue
				}
			}

			// apply config before Start (bounded by timeout ctx)
			if v, ok := o.p.(ConfigValidator); ok {
				cctx, ccancel := context.WithTimeout(pctx, callTimeout)
				if err := v.ValidateConfig(cctx, o.raw.Config); err != nil {
					ccancel()
					pm.log.Error("plugin config validate failed", slog.String("plugin", o.name), slog.Any("err", err))
					cancel()
					continue
				}
				ccancel()
			}

			if cp, ok := o.p.(ConfigurablePlugin); ok {
				cctx, ccancel := context.WithTimeout(pctx, callTimeout)
				_ = pm.safeCall("plugin.config."+o.name, func() error { return cp.OnConfigChange(cctx, o.raw.Config) })
				ccancel()
			}

			// Start should receive pctx (long-lived). We enforce timeout externally.
			if err := pm.startWithTimeout(o.name, o.p, pctx, cancel, callTimeout); err != nil {
				pm.log.Error("plugin start failed", slog.String("plugin", o.name), slog.Any("err", err))
				cancel()
				continue
			}

			pm.mu.Lock()
			pm.run[o.name] = true
			pm.pctx[o.name] = pctx
			pm.pcancel[o.name] = cancel
			pm.lastRawHash[o.name] = canonicalHashJSON(o.raw.Config)
			pm.mu.Unlock()

			pm.log.Info("plugin started", slog.String("plugin", o.name))

		case !o.enabled && o.run:
			pm.log.Debug("plugin disable requested", slog.String("plugin", o.name))
			stopCtx, cancel := context.WithTimeout(pm.baseCtx, callTimeout)
			pm.stopOne(stopCtx, o.name, StopPluginDisable)
			cancel()
		case o.enabled && o.run:
			if cp, ok := o.p.(ConfigurablePlugin); ok {
				newHash := canonicalHashJSON(o.raw.Config)
				pm.mu.Lock()
				oldHash := pm.lastRawHash[o.name]
				pctx := pm.pctx[o.name]
				pm.mu.Unlock()
				// If the raw config blob didn't change and global deps didn't change, skip OnConfigChange.
				// This prevents thrashing schedules/background loops on unrelated config reloads.
				if newHash == oldHash && !globalChanged {
					pm.log.Debug("plugin config unchanged; skipping", slog.String("plugin", o.name))
					break
				}
				if newHash == oldHash && globalChanged {
					pm.log.Debug("plugin config unchanged, but global deps changed; reapplying", slog.String("plugin", o.name))
				}
				pm.mu.Lock()
				pm.lastRawHash[o.name] = newHash
				pm.mu.Unlock()
				if pctx == nil {
					pctx = pm.baseCtx
				}
				cctx, ccancel := context.WithTimeout(pctx, callTimeout)
				err := pm.safeCall("plugin.config."+o.name, func() error { return cp.OnConfigChange(cctx, o.raw.Config) })
				if err != nil {
					pm.log.Warn("plugin config apply failed", slog.String("plugin", o.name), slog.Any("err", err))
				}
				ccancel()
			}
		}
	}

	pm.mu.Lock()
	pm.lastGlobalHash = newGlobal
	pm.mu.Unlock()

	pm.mu.Lock()
	pm.refreshRegistryLocked(cfg)
	pm.mu.Unlock()
	return nil
}

// startWithTimeout calls Start(pctx) but enforces a deadline. If it times out, plugin ctx is cancelled.
func (pm *PluginManager) startWithTimeout(name string, p Plugin, pctx context.Context, cancel context.CancelFunc, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		done <- pm.safeCall("plugin.start."+name, func() error { return p.Start(pctx) })
	}()

	if timeout <= 0 {
		return <-done
	}

	t := time.NewTimer(timeout)
	defer t.Stop()

	select {
	case err := <-done:
		return err
	case <-t.C:
		// cancel plugin ctx and wait small grace for Start() to return
		cancel()

		grace := time.NewTimer(2 * time.Second)
		defer grace.Stop()
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("start timeout (%s): %w", timeout, err)
			}
			return fmt.Errorf("start timeout (%s)", timeout)
		case <-grace.C:
			return fmt.Errorf("start timeout (%s): start did not return after cancel", timeout)
		}
	}
}

func (pm *PluginManager) safeCall(label string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			pm.log.Error("panic in plugin call",
				slog.String("call", label),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
			)
			err = fmt.Errorf("panic in %s: %v", label, r)
		}
	}()
	return fn()
}

func (pm *PluginManager) refreshRegistryLocked(cfg *Config) {
	cmds := []Command{}
	cbs := []CallbackRoute{}
	for name, p := range pm.reg {
		if !pm.run[name] {
			continue
		}
		raw, ok := cfg.Plugins[name]
		if !ok || !raw.Enabled {
			continue
		}
		pto, has := pluginCommandTimeout(cfg, name)

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

func pluginCommandTimeout(cfg *Config, plugin string) (time.Duration, bool) {
	raw, ok := cfg.Plugins[plugin]
	if !ok || len(raw.Config) == 0 {
		return 0, false
	}
	// Standard schema: plugin.config.timeouts.command
	type wrap struct {
		Timeouts struct {
			Command string `json:"command"`
		} `json:"timeouts"`
	}
	var w wrap
	if err := json.Unmarshal(raw.Config, &w); err != nil {
		return 0, false
	}
	if w.Timeouts.Command == "" {
		return 0, false
	}
	d := mustDuration(w.Timeouts.Command, 0)
	if d <= 0 {
		return 0, false
	}
	return d, true
}

func validateStandardTimeouts(plugin string, raw json.RawMessage) error {
	// Only validate if "timeouts" is present; this keeps legacy plugins flexible.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil
	}
	b, ok := top["timeouts"]
	if !ok || len(b) == 0 || string(b) == "null" {
		return nil
	}
	var tm map[string]json.RawMessage
	if err := json.Unmarshal(b, &tm); err != nil {
		return fmt.Errorf("plugin %s: timeouts must be an object", plugin)
	}
	for k, v := range tm {
		switch k {
		case "command", "task", "operation":
			// ok
		case "job":
			return fmt.Errorf("plugin %s: timeouts.job is no longer supported; use timeouts.task", plugin)
		case "request":
			return fmt.Errorf("plugin %s: timeouts.request is no longer supported; use timeouts.operation", plugin)
		default:
			return fmt.Errorf("plugin %s: unknown timeouts field %q (supported: command, task, operation)", plugin, k)
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return fmt.Errorf("plugin %s: invalid timeouts.%s: %w", plugin, k, err)
		}
		if s == "" {
			continue
		}
		if _, err := time.ParseDuration(s); err != nil {
			return fmt.Errorf("plugin %s: invalid timeouts.%s: %w", plugin, k, err)
		}
	}
	return nil
}

// ValidateConfig performs per-plugin config validation BEFORE committing/applying a new config.
// It does not call Init/Start/Stop and should be fast.
func (pm *PluginManager) ValidateConfig(ctx context.Context, cfg *Config) error {
	pm.mu.Lock()
	ops := make([]struct {
		name string
		p    Plugin
		raw  PluginConfigRaw
		en   bool
	}, 0, len(pm.reg))
	for name, p := range pm.reg {
		raw, ok := cfg.Plugins[name]
		enabled := ok && raw.Enabled
		// validate standardized plugin.config.timeouts (if present)
		if ok && len(raw.Config) > 0 {
			if err := validateStandardTimeouts(name, raw.Config); err != nil {
				pm.mu.Unlock()
				return err
			}
		}
		ops = append(ops, struct {
			name string
			p    Plugin
			raw  PluginConfigRaw
			en   bool
		}{name: name, p: p, raw: raw, en: enabled})
	}
	pm.mu.Unlock()

	for _, o := range ops {
		if !o.en || o.p == nil {
			continue
		}
		if v, ok := o.p.(ConfigValidator); ok {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := v.ValidateConfig(cctx, o.raw.Config)
			cancel()
			if err != nil {
				return fmt.Errorf("plugin %s: config validate: %w", o.name, err)
			}
		}
	}
	return nil
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
