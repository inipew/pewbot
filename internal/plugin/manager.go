package plugin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	logx "pewbot/pkg/logx"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"pewbot/internal/eventbus"
	"pewbot/internal/storage"
	kit "pewbot/internal/transport"
)

type pluginEvent struct {
	Plugin string `json:"plugin"`
	Stage  string `json:"stage,omitempty"`
	Reason string `json:"reason,omitempty"`
	Err    string `json:"err,omitempty"`
	TookMS int64  `json:"took_ms,omitempty"`
	Count  int    `json:"count,omitempty"`
}

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
	Logger      logx.Logger
	Adapter     kit.Adapter
	Config      *ConfigManager
	Services    *Services
	Bus         eventbus.Bus
	Store       storage.Store
	OwnerUserID []int64
}

type PluginManager struct {
	mu sync.Mutex

	log  logx.Logger
	cfgm *ConfigManager
	deps PluginDeps
	reg  map[string]Plugin
	run  map[string]bool
	// inited tracks plugins that have successfully passed Init at least once.
	// We avoid re-calling Init on every enable/disable cycle to prevent
	// accidental double-initialization leaks (goroutines, resources, etc.).
	// Plugins that need to react to config changes should implement
	// ConfigurablePlugin and/or HealthChecker.
	inited map[string]bool
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

	// quarantine tracks plugins that are intentionally kept disabled due to invalid config.
	// Most commonly this is triggered by invalid standardized timeouts.
	quarantine map[string]quarantineState

	// per-plugin capability allowlist (mutable for hot reload)
	caps map[string]*capRef

	// health loops started for running plugins implementing HealthChecker
	healthStarted map[string]bool
	// last known health result per plugin (updated by periodic loop and on-demand checks)
	healthLast map[string]PluginHealthResult

	cmdm *CommandManager
}

// HealthChecker is an optional plugin interface.
// If implemented, core will periodically call Health() and publish an event.
type HealthChecker interface {
	Health(ctx context.Context) (status string, err error)
}

// HealthLoopOptIn is an optional marker interface.
//
// By default, the plugin manager will NOT start a periodic health loop even if a
// plugin implements HealthChecker. Plugins must explicitly opt-in to avoid
// creating background loops for simple plugins.
//
// This is especially important because many plugins embed PluginBase, which may
// provide a trivial Health() implementation for convenience.
type HealthLoopOptIn interface {
	HealthLoopEnabled() bool
}

// SupervisorProvider is an optional interface implemented by plugins embedding
// core.PluginBase (directly or via pluginkit.EnhancedPluginBase).
// It allows the plugin manager to attach internal goroutines (like health loops)
// to the plugin's supervisor so they are owned + joinable.
type SupervisorProvider interface {
	Supervisor() *Supervisor
}

type quarantineState struct {
	rawHash uint64
	err     string
	since   time.Time
	count   int
}

func NewPluginManager(log logx.Logger, cfgm *ConfigManager, deps PluginDeps, cmdm *CommandManager) *PluginManager {
	if log.IsZero() {
		log = logx.Nop()
	}
	baseCtx, baseCancel := context.WithCancel(context.Background())
	return &PluginManager{
		log:            log,
		cfgm:           cfgm,
		deps:           deps,
		reg:            map[string]Plugin{},
		run:            map[string]bool{},
		inited:         map[string]bool{},
		lastRawHash:    map[string]uint64{},
		lastGlobalHash: 0,
		baseCtx:        baseCtx,
		baseCancel:     baseCancel,
		pctx:           map[string]context.Context{},
		pcancel:        map[string]context.CancelFunc{},
		quarantine:     map[string]quarantineState{},
		caps:           map[string]*capRef{},
		healthStarted:  map[string]bool{},
		healthLast:     map[string]PluginHealthResult{},
		cmdm:           cmdm,
	}
}

func (pm *PluginManager) emit(typ string, data pluginEvent) {
	bus := pm.deps.Bus
	if bus == nil {
		return
	}
	bus.Publish(eventbus.Event{Type: typ, Data: data})
}

func (pm *PluginManager) capsFor(name string) *capRef {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.caps[name]
}

func (pm *PluginManager) ensureCaps(name string, allow []string) *capRef {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	cr := pm.caps[name]
	if cr == nil {
		cr = newCapRef(allow)
		pm.caps[name] = cr
		return cr
	}
	cr.Update(allow)
	return cr
}

func (pm *PluginManager) deleteCapsLocked(name string) {
	delete(pm.caps, name)
	delete(pm.healthStarted, name)
}

func (pm *PluginManager) depsForPlugin(name string, raw PluginConfigRaw) PluginDeps {
	// Create a per-plugin allowlist ref and wrap selected ports.
	cr := pm.ensureCaps(name, raw.Allow)
	d := pm.deps
	d.Services = wrapServicesForPlugin(pm.deps.Services, cr)
	d.Store = wrapStoreForPlugin(pm.deps.Store, cr)
	return d
}

func (pm *PluginManager) startHealthLoop(name string, p Plugin, hc HealthChecker, pluginCtx context.Context) {
	if hc == nil {
		return
	}

	// Require explicit opt-in to avoid spawning health loops for every plugin
	// that happens to implement HealthChecker (e.g. by embedding PluginBase).
	if oi, ok := p.(HealthLoopOptIn); !ok || !oi.HealthLoopEnabled() {
		return
	}

	pm.mu.Lock()
	if pm.healthStarted[name] {
		pm.mu.Unlock()
		return
	}
	pm.healthStarted[name] = true
	pm.mu.Unlock()

	const (
		interval   = 30 * time.Second
		timeout    = 3 * time.Second
		failThresh = 3
	)

	pm.log.Debug("plugin health loop started", logx.String("plugin", name))
	pm.emit("plugin.health.loop_started", pluginEvent{Plugin: name})

	loop := func(ctx context.Context) {
		defer func() {
			pm.log.Debug("plugin health loop stopped", logx.String("plugin", name))
			pm.emit("plugin.health.loop_stopped", pluginEvent{Plugin: name})
		}()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		fails := 0

		run := func() {
			hctx, cancel := context.WithTimeout(ctx, timeout)
			status, err := hc.Health(hctx)
			cancel()
			at := time.Now()
			if err != nil {
				fails++
				pm.mu.Lock()
				pm.healthLast[name] = PluginHealthResult{Plugin: name, At: at, Status: status, Err: err.Error(), Fails: fails}
				pm.mu.Unlock()
				pm.emit("plugin.health", pluginEvent{Plugin: name, Stage: status, Err: err.Error(), Count: fails})
				if fails == failThresh {
					pm.log.Warn("plugin health failing repeatedly", logx.String("plugin", name), logx.Int("fails", fails), logx.String("err", err.Error()))
					pm.emit("plugin.unhealthy", pluginEvent{Plugin: name, Err: err.Error(), Count: fails})
				}
				return
			}
			if fails > 0 {
				pm.emit("plugin.recovered", pluginEvent{Plugin: name, Stage: status, Count: fails})
				fails = 0
			}
			pm.mu.Lock()
			pm.healthLast[name] = PluginHealthResult{Plugin: name, At: at, Status: status}
			pm.mu.Unlock()
			pm.emit("plugin.health", pluginEvent{Plugin: name, Stage: status})
		}

		// initial check
		run()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}

	// Prefer attaching to the plugin supervisor so the goroutine becomes owned + joinable.
	if sp, ok := p.(SupervisorProvider); ok {
		if sup := sp.Supervisor(); sup != nil {
			sup.Go0("health.loop", loop)
			return
		}
	}

	// Fallback: run on a raw goroutine bound to pluginCtx.
	// For opt-in plugins we expect SupervisorProvider to be available.
	if pluginCtx == nil {
		pluginCtx = context.Background()
	}
	go loop(pluginCtx)
}

func (pm *PluginManager) isQuarantined(name string, rawHash uint64) bool {
	pm.mu.Lock()
	st, ok := pm.quarantine[name]
	pm.mu.Unlock()
	return ok && st.rawHash == rawHash
}

func (pm *PluginManager) clearQuarantineOnChange(name string, rawHash uint64) {
	pm.mu.Lock()
	st, ok := pm.quarantine[name]
	if ok && st.rawHash != rawHash {
		delete(pm.quarantine, name)
		pm.mu.Unlock()
		pm.log.Info("plugin quarantine cleared (config changed)", logx.String("plugin", name))
		pm.emit("plugin.quarantine_cleared", pluginEvent{Plugin: name})
		return
	}
	pm.mu.Unlock()
}

func (pm *PluginManager) setQuarantine(name string, rawHash uint64, err error, stage string) {
	if err == nil {
		return
	}
	errStr := err.Error()
	pm.mu.Lock()
	prev, ok := pm.quarantine[name]
	// Avoid spamming logs when reconcile runs repeatedly with the same broken config.
	if ok && prev.rawHash == rawHash && prev.err == errStr {
		prev.count++
		pm.quarantine[name] = prev
		pm.mu.Unlock()
		return
	}
	count := 1
	if ok {
		count = prev.count + 1
	}
	pm.quarantine[name] = quarantineState{rawHash: rawHash, err: errStr, since: time.Now(), count: count}
	pm.mu.Unlock()

	pm.log.Error("plugin quarantined", logx.String("plugin", name), logx.String("stage", stage), logx.String("err", errStr))
	pm.emit("plugin.quarantined", pluginEvent{Plugin: name, Stage: stage, Err: errStr, Count: count})
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
	pm.log.Debug("stopping plugin", logx.String("plugin", name), logx.String("reason", string(reason)))

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
		pm.log.Warn("plugin stop timeout (continuing)", logx.String("plugin", name), logx.String("err", stopCtx.Err().Error()))
		pm.emit("plugin.stop_timeout", pluginEvent{Plugin: name, Reason: string(reason), Err: stopCtx.Err().Error()})
	}

	pm.mu.Lock()
	pm.run[name] = false
	pm.healthLast[name] = PluginHealthResult{Plugin: name, At: time.Now(), Status: "stopped"}
	delete(pm.pctx, name)
	delete(pm.pcancel, name)
	delete(pm.lastRawHash, name)
	pm.deleteCapsLocked(name)
	pm.mu.Unlock()

	took := time.Since(start)
	pm.emit("plugin.stopped", pluginEvent{Plugin: name, Reason: string(reason), TookMS: took.Milliseconds()})
	if took >= 500*time.Millisecond {
		pm.log.Info("plugin stopped", logx.String("plugin", name), logx.String("reason", string(reason)), logx.Duration("took", took), logx.Bool("ctx_was_set", pctx != nil))
	} else {
		pm.log.Debug("plugin stopped", logx.String("plugin", name), logx.String("reason", string(reason)), logx.Duration("took", took), logx.Bool("ctx_was_set", pctx != nil))
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
		rawHash uint64
		enabled bool
		run     bool
	}
	pm.mu.Lock()
	ops := make([]op, 0, len(pm.reg))
	for name, p := range pm.reg {
		raw, ok := cfg.Plugins[name]
		enabled := ok && raw.Enabled
		running := pm.run[name]
		rh := effectivePluginHash(raw)
		ops = append(ops, op{name: name, p: p, raw: raw, rawHash: rh, enabled: enabled, run: running})
	}
	pm.mu.Unlock()

	const callTimeout = 10 * time.Second

	for _, o := range ops {
		switch {
		case o.enabled && !o.run:
			// If config changed since last quarantine, clear it so we can retry.
			pm.clearQuarantineOnChange(o.name, o.rawHash)
			if pm.isQuarantined(o.name, o.rawHash) {
				pm.log.Warn("plugin enable skipped (quarantined)", logx.String("plugin", o.name))
				continue
			}
			// Standardized timeout validation is enforced via quarantine (not global config rejection).
			if err := validateStandardTimeouts(o.name, o.raw.Config); err != nil {
				pm.setQuarantine(o.name, o.rawHash, err, "timeouts")
				continue
			}

			pm.log.Debug("plugin enable requested", logx.String("plugin", o.name))
			pm.emit("plugin.enable_requested", pluginEvent{Plugin: o.name})

			// start: create LONG-LIVED plugin ctx from internal base ctx
			pctx, cancel := context.WithCancel(pm.baseCtx)
			deps := pm.depsForPlugin(o.name, o.raw)

			// init (bounded by timeout ctx)
			pm.mu.Lock()
			needInit := !pm.inited[o.name]
			pm.mu.Unlock()
			if needInit {
				ictx, icancel := context.WithTimeout(pctx, callTimeout)
				err := pm.safeCall("plugin.init."+o.name, func() error { return o.p.Init(ictx, deps) })
				icancel()
				if err != nil {
					pm.log.Error("plugin init failed", logx.String("plugin", o.name), logx.Any("err", err))
					pm.emit("plugin.init_failed", pluginEvent{Plugin: o.name, Err: err.Error()})
					cancel()
					continue
				}
				pm.mu.Lock()
				pm.inited[o.name] = true
				pm.mu.Unlock()
			} else {
				pm.log.Debug("plugin already initialized; skipping Init", logx.String("plugin", o.name))
			}

			// apply config before Start (bounded by timeout ctx)
			if v, ok := o.p.(ConfigValidator); ok {
				cctx, ccancel := context.WithTimeout(pctx, callTimeout)
				if err := v.ValidateConfig(cctx, o.raw.Config); err != nil {
					ccancel()
					pm.setQuarantine(o.name, o.rawHash, fmt.Errorf("config validate: %w", err), "validate")
					pm.emit("plugin.config_invalid", pluginEvent{Plugin: o.name, Err: err.Error()})
					cancel()
					continue
				}
				ccancel()
			}

			if cp, ok := o.p.(ConfigurablePlugin); ok {
				cctx, ccancel := context.WithTimeout(pctx, callTimeout)
				err := pm.safeCall("plugin.config."+o.name, func() error { return cp.OnConfigChange(cctx, o.raw.Config) })
				ccancel()
				if err != nil {
					pm.setQuarantine(o.name, o.rawHash, fmt.Errorf("config apply: %w", err), "config")
					pm.emit("plugin.config_failed", pluginEvent{Plugin: o.name, Err: err.Error()})
					cancel()
					continue
				}
				pm.emit("plugin.config_applied", pluginEvent{Plugin: o.name})
			}

			// Start should receive pctx (long-lived). We enforce timeout externally.
			if err := pm.startWithTimeout(o.name, o.p, pctx, cancel, callTimeout); err != nil {
				pm.log.Error("plugin start failed", logx.String("plugin", o.name), logx.Any("err", err))
				pm.emit("plugin.start_failed", pluginEvent{Plugin: o.name, Err: err.Error()})
				cancel()
				continue
			}

			pm.mu.Lock()
			pm.run[o.name] = true
			pm.pctx[o.name] = pctx
			pm.pcancel[o.name] = cancel
			pm.lastRawHash[o.name] = o.rawHash
			delete(pm.quarantine, o.name)
			pm.mu.Unlock()

			pm.log.Info("plugin started", logx.String("plugin", o.name))
			pm.emit("plugin.started", pluginEvent{Plugin: o.name})
			if hc, ok := o.p.(HealthChecker); ok {
				pm.startHealthLoop(o.name, o.p, hc, pctx)
			}

		case !o.enabled && o.run:
			pm.log.Debug("plugin disable requested", logx.String("plugin", o.name))
			pm.emit("plugin.disable_requested", pluginEvent{Plugin: o.name})
			stopCtx, cancel := context.WithTimeout(pm.baseCtx, callTimeout)
			pm.stopOne(stopCtx, o.name, StopPluginDisable)
			cancel()
		case o.enabled && o.run:
			if cp, ok := o.p.(ConfigurablePlugin); ok {
				// Update capability allowlist even if config blob itself didn't change.
				pm.ensureCaps(o.name, o.raw.Allow)
				newHash := o.rawHash
				pm.mu.Lock()
				oldHash := pm.lastRawHash[o.name]
				pctx := pm.pctx[o.name]
				pm.mu.Unlock()
				// If the raw config blob didn't change and global deps didn't change, skip OnConfigChange.
				// This prevents thrashing schedules/background loops on unrelated config reloads.
				if newHash == oldHash && !globalChanged {
					pm.log.Debug("plugin config unchanged; skipping", logx.String("plugin", o.name))
					break
				}
				// If raw config changed, enforce standardized timeout validation via quarantine.
				if newHash != oldHash {
					if err := validateStandardTimeouts(o.name, o.raw.Config); err != nil {
						pm.setQuarantine(o.name, newHash, err, "timeouts")
						stopCtx, cancel := context.WithTimeout(pm.baseCtx, callTimeout)
						pm.stopOne(stopCtx, o.name, StopPluginQuarantine)
						cancel()
						break
					}
				}
				if newHash == oldHash && globalChanged {
					pm.log.Debug("plugin config unchanged, but global deps changed; reapplying", logx.String("plugin", o.name))
				}
				if pctx == nil {
					pctx = pm.baseCtx
				}
				cctx, ccancel := context.WithTimeout(pctx, callTimeout)
				err := pm.safeCall("plugin.config."+o.name, func() error { return cp.OnConfigChange(cctx, o.raw.Config) })
				ccancel()
				if err != nil {
					pm.setQuarantine(o.name, newHash, fmt.Errorf("config apply: %w", err), "config")
					pm.emit("plugin.config_failed", pluginEvent{Plugin: o.name, Err: err.Error()})
					stopCtx, cancel := context.WithTimeout(pm.baseCtx, callTimeout)
					pm.stopOne(stopCtx, o.name, StopPluginQuarantine)
					cancel()
					break
				}
				pm.emit("plugin.config_applied", pluginEvent{Plugin: o.name})
				pm.mu.Lock()
				pm.lastRawHash[o.name] = newHash
				delete(pm.quarantine, o.name)
				pm.mu.Unlock()
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
				logx.String("call", label),
				logx.Any("panic", r),
				logx.String("stack", string(debug.Stack())),
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

		for _, c := range pm.safeCommands(name, p) {
			c.PluginName = name
			// If plugin timeout set and command doesn't override, apply it.
			if has && c.Timeout <= 0 {
				c.Timeout = pto
			}
			cmds = append(cmds, c)
		}

		if cbp, ok := p.(CallbackProvider); ok {
			for _, r := range pm.safeCallbacks(name, cbp) {
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

func (pm *PluginManager) safeCommands(name string, p Plugin) (out []Command) {
	if p == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			pm.log.Error("panic in plugin Commands()",
				logx.String("plugin", name),
				logx.Any("panic", r),
				logx.String("stack", string(debug.Stack())),
			)
			out = nil
		}
	}()
	return p.Commands()
}

func (pm *PluginManager) safeCallbacks(name string, p CallbackProvider) (out []CallbackRoute) {
	if p == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			pm.log.Error("panic in plugin Callbacks()",
				logx.String("plugin", name),
				logx.Any("panic", r),
				logx.String("stack", string(debug.Stack())),
			)
			out = nil
		}
	}()
	return p.Callbacks()
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
		// Validate standardized timeouts schema if present.
		if err := validateStandardTimeouts(o.name, o.raw.Config); err != nil {
			return err
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

// Snapshot implements PluginsPort.
func (pm *PluginManager) Snapshot() PluginsSnapshot {
	cfg := pm.cfgm.Get()
	pm.mu.Lock()
	names := make([]string, 0, len(pm.reg))
	for name := range pm.reg {
		names = append(names, name)
	}
	sort.Strings(names)
	out := PluginsSnapshot{Time: time.Now(), Plugins: make([]PluginStatus, 0, len(names))}
	for _, name := range names {
		p := pm.reg[name]
		running := pm.run[name]
		hasHealth := false
		if p != nil {
			_, hasHealth = p.(HealthChecker)
		}
		enabled := false
		hasCfg := false
		if cfg != nil && cfg.Plugins != nil {
			if r, ok := cfg.Plugins[name]; ok {
				enabled = r.Enabled
				hasCfg = true
			}
		}
		q, qok := pm.quarantine[name]
		last := pm.healthLast[name]
		out.Plugins = append(out.Plugins, PluginStatus{
			Name:             name,
			Enabled:          enabled,
			Running:          running,
			HasConfig:        hasCfg,
			Quarantined:      qok,
			QuarantineErr:    q.err,
			QuarantineSince:  q.since,
			HasHealthChecker: hasHealth,
			HealthLoopActive: pm.healthStarted[name],
			LastHealth:       last,
		})
	}
	pm.mu.Unlock()
	return out
}

// CheckHealth implements PluginsPort.
func (pm *PluginManager) CheckHealth(ctx context.Context, names []string) []PluginHealthResult {
	const perPluginTimeout = 3 * time.Second

	// Determine targets without holding lock during plugin calls.
	type target struct {
		name    string
		p       Plugin
		hc      HealthChecker
		running bool
	}

	pm.mu.Lock()
	var targets []target
	if len(names) > 0 {
		for _, name := range names {
			p := pm.reg[name]
			if p == nil {
				continue
			}
			hc, _ := p.(HealthChecker)
			targets = append(targets, target{name: name, p: p, hc: hc, running: pm.run[name]})
		}
	} else {
		for name, p := range pm.reg {
			hc, ok := p.(HealthChecker)
			if !ok {
				continue
			}
			if !pm.run[name] {
				continue
			}
			targets = append(targets, target{name: name, p: p, hc: hc, running: true})
		}
	}
	pm.mu.Unlock()

	sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })

	results := make([]PluginHealthResult, 0, len(targets))
	for _, t := range targets {
		at := time.Now()
		// If not running or no health checker, just record a synthetic state.
		if !t.running || t.hc == nil {
			r := PluginHealthResult{Plugin: t.name, At: at, Status: "stopped"}
			pm.mu.Lock()
			pm.healthLast[t.name] = r
			pm.mu.Unlock()
			results = append(results, r)
			continue
		}

		base := pm.ctxOr(context.Background(), t.name)
		hctx, cancel := context.WithTimeout(base, perPluginTimeout)
		// Also respect the caller context (command), without changing the base owner context.
		// context.AfterFunc returns a stop func with signature func() bool.
		stop := func() bool { return false }
		if ctx != nil {
			stop = context.AfterFunc(ctx, cancel)
		}
		status, err := t.hc.Health(hctx)
		_ = stop()
		cancel()

		pm.mu.Lock()
		prev := pm.healthLast[t.name]
		fails := prev.Fails
		r := PluginHealthResult{Plugin: t.name, At: at, Status: status}
		if err != nil {
			fails++
			r.Err = err.Error()
			r.Fails = fails
		} else {
			fails = 0
			r.Fails = 0
		}
		pm.healthLast[t.name] = r
		pm.mu.Unlock()

		results = append(results, r)
	}

	return results
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

func effectivePluginHash(raw PluginConfigRaw) uint64 {
	// Hash config (canonical JSON) + allowlist (order-insensitive).
	ch := canonicalHashJSON(raw.Config)

	allow := append([]string(nil), raw.Allow...)
	sort.Strings(allow)
	ab, _ := json.Marshal(allow)
	ah := hashBytes(ab)

	var buf [16]byte
	binary.LittleEndian.PutUint64(buf[0:8], ch)
	binary.LittleEndian.PutUint64(buf[8:16], ah)
	return hashBytes(buf[:])
}
