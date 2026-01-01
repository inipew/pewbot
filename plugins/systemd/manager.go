package systemd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pewbot/internal/core"
	"pewbot/internal/kit"
	sm "pewbot/pkg/systemdmanager"
)

type AutoRecover struct {
	Enabled  bool   `json:"enabled"`
	Interval string `json:"interval"`
	MinDown  string `json:"min_down"`
}

type Config struct {
	Prefix      string      `json:"prefix"`
	AllowUnits  []string    `json:"allow_units"`
	AutoRecover AutoRecover `json:"auto_recover"`
}

type Plugin struct {
	log  *slog.Logger
	deps core.PluginDeps

	mu     sync.RWMutex
	cfg    Config
	runCtx context.Context

	mgr *sm.ServiceManager

	// plugin-owned lifecycle for auto-recover schedule (safe under hot-reload)
	autoMu      sync.Mutex
	autoRunning int32 // atomic guard
	downSince   map[string]time.Time
}

func New() *Plugin { return &Plugin{downSince: map[string]time.Time{}} }
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

func (p *Plugin) Commands() []core.Command {
	return []core.Command{
		{
			Route:       "systemd",
			Description: "systemd manager (help)",
			Usage:       "/systemd (subcommands: list/status/start/stop/restart/enable/disable/failed/inactive)",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdHelp,
		},
		{
			Route:       "systemd list",
			Description: "list managed units",
			Usage:       "/systemd list",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdList,
		},
		{
			Route:       "systemd status",
			Description: "show status for a unit (or all managed)",
			Usage:       "/systemd status [unit]",
			Aliases:     []string{"systemd_st"},
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdStatus,
		},
		{
			Route:       "systemd start",
			Description: "start unit(s)",
			Usage:       "/systemd start <unit unit...>",
			Aliases:     []string{"systemd_start"},
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				return p.cmdOperate(ctx, req, "start", req.Args)
			},
		},
		{
			Route:       "systemd stop",
			Description: "stop unit(s)",
			Usage:       "/systemd stop <unit unit...>",
			Aliases:     []string{"systemd_stop"},
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				return p.cmdOperate(ctx, req, "stop", req.Args)
			},
		},
		{
			Route:       "systemd restart",
			Description: "restart unit(s)",
			Usage:       "/systemd restart <unit unit...>",
			Aliases:     []string{"systemd_restart"},
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				return p.cmdOperate(ctx, req, "restart", req.Args)
			},
		},
		{
			Route:       "systemd enable",
			Description: "enable a unit on boot",
			Usage:       "/systemd enable <unit>",
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				return p.cmdOperate(ctx, req, "enable", req.Args)
			},
		},
		{
			Route:       "systemd disable",
			Description: "disable a unit on boot",
			Usage:       "/systemd disable <unit>",
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				return p.cmdOperate(ctx, req, "disable", req.Args)
			},
		},
		{
			Route:       "systemd failed",
			Description: "list failed managed units",
			Usage:       "/systemd failed",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdFailed,
		},
		{
			Route:       "systemd inactive",
			Description: "list inactive managed units",
			Usage:       "/systemd inactive",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdInactive,
		},
	}
}

func (p *Plugin) cmdHelp(ctx context.Context, req *core.Request) error {
	p.mu.RLock()
	cfg := p.cfg
	units := append([]string(nil), p.cfg.AllowUnits...)
	p.mu.RUnlock()
	sort.Strings(units)

	lines := []string{
		"systemd help:",
		"/systemd list",
		"/systemd status [unit]",
		"/systemd start <unit unit...>",
		"/systemd stop <unit unit...>",
		"/systemd restart <unit unit...>",
		"/systemd enable <unit>",
		"/systemd disable <unit>",
		"/systemd failed",
		"/systemd inactive",
		"allowed: " + strings.Join(units, ", "),
	}
	_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+strings.Join(lines, "\n"), nil)
	return nil
}

func (p *Plugin) cmdList(ctx context.Context, req *core.Request) error {
	mgr := p.mgr
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()

	if mgr == nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"systemd manager not available (non-linux/dbus permission?)", nil)
		return nil
	}

	units := mgr.GetManagedServices()
	if len(units) == 0 {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"no managed units configured", nil)
		return nil
	}
	_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"managed units:\n- "+strings.Join(units, "\n- "), nil)
	return nil
}

func (p *Plugin) cmdFailed(ctx context.Context, req *core.Request) error {
	mgr := p.mgr
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()

	if mgr == nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"systemd manager not available (non-linux/dbus permission?)", nil)
		return nil
	}

	failed, err := mgr.GetFailedServices(ctx)
	if err != nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"failed: "+err.Error(), nil)
		return nil
	}
	if len(failed) == 0 {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"no failed services", nil)
		return nil
	}
	_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"failed:\n- "+strings.Join(failed, "\n- "), nil)
	return nil
}

func (p *Plugin) cmdInactive(ctx context.Context, req *core.Request) error {
	mgr := p.mgr
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()

	if mgr == nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"systemd manager not available (non-linux/dbus permission?)", nil)
		return nil
	}

	inactive, err := mgr.GetInactiveServices(ctx)
	if err != nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"failed: "+err.Error(), nil)
		return nil
	}
	if len(inactive) == 0 {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"no inactive services", nil)
		return nil
	}
	_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"inactive:\n- "+strings.Join(inactive, "\n- "), nil)
	return nil
}

func (p *Plugin) cmdOperate(ctx context.Context, req *core.Request, action string, args []string) error {
	mgr := p.mgr
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()

	if mgr == nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"systemd manager not available (non-linux/dbus permission?)", nil)
		return nil
	}

	units, err := p.unitsFromArgs(args)
	if err != nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+err.Error(), nil)
		return nil
	}
	for _, u := range units {
		if !p.allowed(u) {
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"unit not allowed: "+u, nil)
			return nil
		}
	}

	if len(units) == 1 {
		u := units[0]
		switch action {
		case "start":
			res := mgr.StartWithResult(ctx, u)
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+res.Message, nil)
		case "stop":
			res := mgr.StopWithResult(ctx, u)
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+res.Message, nil)
		case "restart":
			res := mgr.RestartWithResult(ctx, u)
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+res.Message, nil)
		case "enable":
			err := mgr.EnableContext(ctx, u)
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+plainOp("enable", u, err), nil)
		case "disable":
			err := mgr.DisableContext(ctx, u)
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+plainOp("disable", u, err), nil)
		}
		return nil
	}

	// multi ops
	var br sm.BatchResult
	switch action {
	case "start":
		br = mgr.BatchStart(ctx, units)
	case "stop":
		br = mgr.BatchStop(ctx, units)
	case "restart":
		br = mgr.BatchRestart(ctx, units)
	default:
		// enable/disable batch: sequential for safety
		ok := 0
		out := make([]string, 0, len(units))
		for _, u := range units {
			var e error
			if action == "enable" {
				e = mgr.EnableContext(ctx, u)
			} else {
				e = mgr.DisableContext(ctx, u)
			}
			if e == nil {
				ok++
			}
			out = append(out, plainOp(action, u, e))
		}
		msg := fmt.Sprintf("%s batch: ok=%d fail=%d\n%s", action, ok, len(units)-ok, strings.Join(out, "\n"))
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+msg, nil)
		return nil
	}

	lines := make([]string, 0, len(br.Results)+1)
	lines = append(lines, fmt.Sprintf("%s batch: ok=%d fail=%d total=%d", action, br.SuccessCount, br.FailureCount, br.Total))
	for _, r := range br.Results {
		lines = append(lines, r.Message)
	}
	_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+strings.Join(lines, "\n"), nil)
	return nil
}

func (p *Plugin) cmdStatus(ctx context.Context, req *core.Request) error {
	mgr := p.mgr
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()

	if mgr == nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"systemd manager not available (non-linux/dbus permission?)", nil)
		return nil
	}

	args := req.Args
	if len(args) == 0 {
		statuses, err := mgr.GetAllStatusContext(ctx)
		if err != nil {
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"failed: "+err.Error(), nil)
			return nil
		}
		if len(statuses) == 0 {
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"no managed units configured", nil)
			return nil
		}
		lines := make([]string, 0, len(statuses)+1)
		lines = append(lines, "status (managed):")
		for _, st := range statuses {
			lines = append(lines, formatStatusLine(st))
		}
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+strings.Join(lines, "\n"), &kit.SendOptions{DisablePreview: true})
		return nil
	}

	units, err := p.unitsFromArgs(args)
	if err != nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+err.Error(), nil)
		return nil
	}

	if len(units) == 1 {
		u := units[0]
		if !p.allowed(u) {
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"unit not allowed: "+u, nil)
			return nil
		}
		st, err := mgr.GetStatusContext(ctx, u)
		if err != nil {
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"failed: "+err.Error(), nil)
			return nil
		}
		msg := formatStatusDetail(*st)
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+msg, &kit.SendOptions{DisablePreview: true})
		return nil
	}

	lines := make([]string, 0, len(units)+1)
	lines = append(lines, "status:")
	for _, u := range units {
		if !p.allowed(u) {
			lines = append(lines, u+": not allowed")
			continue
		}
		st, err := mgr.GetStatusContext(ctx, u)
		if err != nil {
			lines = append(lines, u+": error: "+err.Error())
			continue
		}
		lines = append(lines, formatStatusLine(*st))
	}
	_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+strings.Join(lines, "\n"), &kit.SendOptions{DisablePreview: true})
	return nil
}

func (p *Plugin) unitsFromArgs(args []string) ([]string, error) {
	if len(args) < 1 {
		return nil, errors.New("missing unit name")
	}
	units := make([]string, 0, len(args))
	for _, a := range args {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		units = append(units, a)
	}
	if len(units) == 0 {
		return nil, errors.New("missing unit name")
	}
	return units, nil
}

func (p *Plugin) allowed(u string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, x := range p.cfg.AllowUnits {
		if x == u {
			return true
		}
	}
	return false
}

func normalizeUnits(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, u := range in {
		u = strings.TrimSpace(strings.TrimSuffix(u, ".service"))
		if u == "" {
			continue
		}
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	sort.Strings(out)
	return out
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func plainOp(action, unit string, err error) string {
	if err != nil {
		return fmt.Sprintf("%s %s: error: %v", action, unit, err)
	}
	return fmt.Sprintf("%s %s: ok", action, unit)
}

func formatStatusLine(st sm.ServiceStatus) string {
	active := st.Active
	if active == "" {
		active = "unknown"
	}
	en := "disabled"
	if st.Enabled {
		en = "enabled"
	}
	up := ""
	if st.Uptime > 0 {
		up = ", up " + durShort(st.Uptime)
	}
	mem := ""
	if st.Memory > 0 {
		mem = ", mem " + bytes(st.Memory)
	}
	return fmt.Sprintf("%s: %s/%s (%s)%s%s", st.Name, active, st.SubState, en, up, mem)
}

func formatStatusDetail(st sm.ServiceStatus) string {
	active := st.Active
	if active == "" {
		active = "unknown"
	}
	en := "disabled"
	if st.Enabled {
		en = "enabled"
	}
	lines := []string{
		"unit: " + st.Name,
		"active: " + active,
		"sub: " + st.SubState,
		"load: " + st.LoadState,
		"enabled: " + en,
	}
	if st.Description != "" {
		lines = append(lines, "desc: "+st.Description)
	}
	if st.Uptime > 0 {
		lines = append(lines, "uptime: "+durShort(st.Uptime))
	}
	if st.Memory > 0 {
		lines = append(lines, "memory: "+bytes(st.Memory))
	}
	return strings.Join(lines, "\n")
}

func durShort(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm%ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh%dm", h, m)
}

func bytes(n uint64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case n >= GB:
		return fmt2(n, GB, "GB")
	case n >= MB:
		return fmt2(n, MB, "MB")
	case n >= KB:
		return fmt2(n, KB, "KB")
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func fmt2(n, div uint64, unit string) string {
	x := float64(n) / float64(div)
	ix := int(x * 10) // 1 decimal
	return fmt.Sprintf("%d.%d%s", ix/10, ix%10, unit)
}

// ---- auto-recover loop ----

func (p *Plugin) startAutoRecover(parent context.Context, cfg Config) {
	if !cfg.AutoRecover.Enabled || p.mgr == nil {
		return
	}

	s := p.deps.Services.Scheduler
	if s == nil || !s.Enabled() {
		p.log.Warn("auto-recover skipped (scheduler disabled)")
		return
	}

	interval := mustDur(cfg.AutoRecover.Interval, 30*time.Second)
	minDown := mustDur(cfg.AutoRecover.MinDown, 3*time.Second)

	// ensure a clean schedule on start/reload
	_ = s.Remove("systemd:auto-recover")

	// reset state for new schedule
	p.autoMu.Lock()
	if p.downSince == nil {
		p.downSince = map[string]time.Time{}
	} else {
		for k := range p.downSince {
			delete(p.downSince, k)
		}
	}
	p.autoMu.Unlock()

	_, err := s.AddInterval("systemd:auto-recover", interval, 0, func(ctx context.Context) error {
		// prevent overlap
		if !atomic.CompareAndSwapInt32(&p.autoRunning, 0, 1) {
			return nil
		}
		defer atomic.StoreInt32(&p.autoRunning, 0)

		// If plugin is stopping, skip quickly.
		select {
		case <-parent.Done():
			return nil
		default:
		}
		return p.autoRecoverTick(ctx, minDown)
	})
	if err != nil {
		p.log.Error("auto-recover schedule failed", slog.String("err", err.Error()))
		return
	}

	p.log.Info("auto-recover scheduled", slog.Duration("interval", interval), slog.Duration("min_down", minDown))
}


func (p *Plugin) stopAutoRecover() {
	s := p.deps.Services.Scheduler
	if s != nil {
		_ = s.Remove("systemd:auto-recover")
	}

	// reset state/guard
	atomic.StoreInt32(&p.autoRunning, 0)

	p.autoMu.Lock()
	if p.downSince != nil {
		for k := range p.downSince {
			delete(p.downSince, k)
		}
	}
	p.autoMu.Unlock()
}

func (p *Plugin) autoRecoverTick(ctx context.Context, minDown time.Duration) error {
	mgr := p.mgr
	if mgr == nil {
		return nil
	}

	p.mu.RLock()
	units := append([]string(nil), p.cfg.AllowUnits...)
	p.mu.RUnlock()

	for _, u := range units {
		st, err := mgr.GetStatusContext(ctx, u)
		if err != nil || st == nil {
			continue
		}
		if st.Active == "active" {
			delete(p.downSince, u)
			continue
		}
		if _, seen := p.downSince[u]; !seen {
			p.downSince[u] = time.Now()
			continue
		}
		if time.Since(p.downSince[u]) >= minDown {
			p.log.Warn("auto-recover restarting unit", slog.String("unit", u), slog.String("state", st.Active))
			_ = mgr.RestartContext(ctx, u)
			delete(p.downSince, u)
		}
	}
	return nil
}

func mustDur(s string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
