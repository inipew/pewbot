package systemd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	core "pewbot/internal/plugin"
	"pewbot/internal/plugin/kit"
	"pewbot/internal/storage"
	kit "pewbot/internal/transport"
	sm "pewbot/pkg/systemdmanager"
)

var errMissingUnitName = errors.New("missing unit name")

type opRes struct {
	Unit    string `json:"unit"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// opCtx binds a command/callback context to the plugin lifecycle context.
//
// This ensures in-flight systemd operations are cancelled when the plugin is
// disabled/stopped, while still respecting per-command timeouts (reqCtx).
// Implemented without spawning an extra goroutine (uses context.AfterFunc).
func (p *Plugin) opCtx(reqCtx context.Context) (context.Context, context.CancelFunc) {
	plug := p.Context()
	if plug == nil {
		if reqCtx == nil {
			return context.WithCancel(context.Background())
		}
		return context.WithCancel(reqCtx)
	}
	if reqCtx == nil {
		return context.WithCancel(plug)
	}
	cctx, cancel := context.WithCancel(plug)
	stop := context.AfterFunc(reqCtx, func() {
		cancel()
	})
	return cctx, func() {
		_ = stop()
		cancel()
	}
}

func (p *Plugin) Commands() []core.Command {
	return []core.Command{
		{
			Route:       "systemd",
			Description: "kelola systemd (bantuan)",
			Usage:       "/systemd  (lihat subcommand)",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdHelp,
		},
		{
			Route:       "systemd list",
			Description: "daftar unit yang dikelola",
			Usage:       "/systemd list",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdList,
		},
		{
			Route:       "systemd status",
			Description: "status unit (atau semua unit yang dikelola)",
			Usage:       "/systemd status [unit]",
			Aliases:     []string{"systemd_st"},
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdStatus,
		},
		{
			Route:       "systemd auto",
			Description: "status/konfigurasi auto-recover",
			Usage:       "/systemd auto",
			Aliases:     []string{"systemd_autorecover", "systemd_auto"},
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdAuto,
		},
		{
			Route:       "systemd start",
			Description: "start unit",
			Usage:       "/systemd start <unit unit...>",
			Aliases:     []string{"systemd_start"},
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				return p.cmdOperate(ctx, req, "start", req.Args)
			},
		},
		{
			Route:       "systemd stop",
			Description: "stop unit",
			Usage:       "/systemd stop <unit unit...>",
			Aliases:     []string{"systemd_stop"},
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				return p.cmdOperate(ctx, req, "stop", req.Args)
			},
		},
		{
			Route:       "systemd restart",
			Description: "restart unit",
			Usage:       "/systemd restart <unit unit...>",
			Aliases:     []string{"systemd_restart"},
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				return p.cmdOperate(ctx, req, "restart", req.Args)
			},
		},
		{
			Route:       "systemd enable",
			Description: "aktifkan unit saat boot",
			Usage:       "/systemd enable <unit>",
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				return p.cmdOperate(ctx, req, "enable", req.Args)
			},
		},
		{
			Route:       "systemd disable",
			Description: "matikan unit saat boot",
			Usage:       "/systemd disable <unit>",
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				return p.cmdOperate(ctx, req, "disable", req.Args)
			},
		},
		{
			Route:       "systemd failed",
			Description: "daftar unit gagal",
			Usage:       "/systemd failed",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdFailed,
		},
		{
			Route:       "systemd inactive",
			Description: "daftar unit tidak aktif",
			Usage:       "/systemd inactive",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdInactive,
		},
	}
}

func (p *Plugin) cmdAuto(ctx context.Context, req *core.Request) error {
	cfg := p.cfgSnapshot()
	mgr := p.mgrSnapshot()

	lines := []string{"auto-recover:"}
	sc := cfg.Scheduler
	name := sc.NameOr("auto_recover")
	lines = append(lines, fmt.Sprintf("- scheduler.enabled: %v", sc.Enabled))
	lines = append(lines, "- scheduler.task_name: "+name)
	if sc.Schedule != "" {
		lines = append(lines, "- scheduler.schedule: "+sc.Schedule)
	}
	lines = append(lines, "- min_down: "+cfg.AutoRecover.MinDown)
	lines = append(lines, "- restart_timeout: "+cfg.AutoRecover.RestartTimeout)
	lines = append(lines, "- backoff_base: "+cfg.AutoRecover.BackoffBase)
	lines = append(lines, "- backoff_max: "+cfg.AutoRecover.BackoffMax)
	lines = append(lines, fmt.Sprintf("- alert_streak: %d", cfg.AutoRecover.FailAlertStreak))
	lines = append(lines, "- alert_interval: "+cfg.AutoRecover.AlertInterval)
	if mgr == nil {
		lines = append(lines, "- manager: unavailable")
	} else {
		lines = append(lines, "- manager: ok")
	}
	if p.Schedule() == nil {
		lines = append(lines, "- scheduler: unavailable")
	} else {
		lines = append(lines, "- scheduler: ok")
	}

	// Show next tries for units (best-effort).
	p.autoMu.Lock()
	state := make(map[string]unitRecoverState, len(p.recoverState))
	for k, v := range p.recoverState {
		if v == nil {
			continue
		}
		state[k] = *v
	}
	p.autoMu.Unlock()

	if len(state) > 0 {
		keys := make([]string, 0, len(state))
		for k := range state {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lines = append(lines, "", "state:")
		now := time.Now()
		for _, u := range keys {
			st := state[u]
			if st.Missing {
				lines = append(lines, "- "+u+": missing")
				continue
			}
			next := "-"
			if !st.NextTry.IsZero() {
				next = st.NextTry.Format(time.RFC3339)
				if st.NextTry.After(now) {
					next += " (in " + durShort(st.NextTry.Sub(now)) + ")"
				}
			}
			lines = append(lines, fmt.Sprintf("- %s: streak=%d next_try=%s last_err=%s", u, st.FailStreak, next, st.LastErr))
		}
	}

	_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+strings.Join(lines, "\n"), &kit.SendOptions{DisablePreview: true})
	return nil
}

func (p *Plugin) cmdHelp(ctx context.Context, req *core.Request) error {
	cfg := p.cfgSnapshot()
	units := append([]string(nil), cfg.AllowUnits...)

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
		"/systemd auto",
		"allowed: " + strings.Join(units, ", "),
	}
	_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+strings.Join(lines, "\n"), nil)
	return nil
}

func (p *Plugin) cmdList(ctx context.Context, req *core.Request) error {
	mgr := p.mgrSnapshot()
	cfg := p.cfgSnapshot()
	ctx, cancel := p.opCtx(ctx)
	defer cancel()

	if mgr == nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"systemd manager not available (non-linux/dbus permission?)", nil)
		return nil
	}

	units := mgr.GetManagedServices()
	if len(units) == 0 {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"no managed units configured", nil)
		return nil
	}
	sort.Strings(units)
	_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"managed units:\n- "+strings.Join(units, "\n- "), nil)
	return nil
}

func (p *Plugin) cmdFailed(ctx context.Context, req *core.Request) error {
	mgr := p.mgrSnapshot()
	cfg := p.cfgSnapshot()
	ctx, cancel := p.opCtx(ctx)
	defer cancel()

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
	mgr := p.mgrSnapshot()
	cfg := p.cfgSnapshot()
	ctx, cancel := p.opCtx(ctx)
	defer cancel()

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
	start := time.Now()
	mgr := p.mgrSnapshot()
	cfg := p.cfgSnapshot()
	ctx, cancel := p.opCtx(ctx)
	defer cancel()

	if mgr == nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"systemd manager not available (non-linux/dbus permission?)", nil)
		return nil
	}

	units, err := p.unitsFromArgs(args)
	if err != nil {
		// UX: if unit is missing, show a picker UI.
		if errors.Is(err, errMissingUnitName) {
			if p.ui != nil {
				st := pluginkit.UIState{View: pickViewForAction(action), Page: 0, Size: 12}
				msg, _ := p.renderView(ctx, req, st)
				_, _ = msg.Send(ctx, req.Adapter, req.Chat)
				return nil
			}
		}
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+err.Error(), nil)
		return nil
	}
	for _, u := range units {
		if !p.allowed(u) {
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"unit not allowed: "+u, nil)
			return nil
		}
	}

	results := make([]opRes, 0, len(units))

	// Execute
	if len(units) == 1 {
		u := units[0]
		switch action {
		case "start":
			res := mgr.StartWithResult(ctx, u)
			results = append(results, opRes{Unit: res.ServiceName, Success: res.Success, Error: errStr(res.Error)})
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+res.Message, nil)
		case "stop":
			res := mgr.StopWithResult(ctx, u)
			results = append(results, opRes{Unit: res.ServiceName, Success: res.Success, Error: errStr(res.Error)})
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+res.Message, nil)
		case "restart":
			res := mgr.RestartWithResult(ctx, u)
			results = append(results, opRes{Unit: res.ServiceName, Success: res.Success, Error: errStr(res.Error)})
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+res.Message, nil)
		case "enable":
			e := mgr.EnableContext(ctx, u)
			results = append(results, opRes{Unit: u, Success: e == nil, Error: errStr(e)})
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+plainOp("enable", u, e), nil)
		case "disable":
			e := mgr.DisableContext(ctx, u)
			results = append(results, opRes{Unit: u, Success: e == nil, Error: errStr(e)})
			_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+plainOp("disable", u, e), nil)
		}
		p.auditOperate(ctx, req, action, units, results, time.Since(start))
		return nil
	}

	// multi ops
	switch action {
	case "start", "stop", "restart":
		var br sm.BatchResult
		if action == "start" {
			br = mgr.BatchStart(ctx, units)
		} else if action == "stop" {
			br = mgr.BatchStop(ctx, units)
		} else {
			br = mgr.BatchRestart(ctx, units)
		}
		lines := make([]string, 0, len(br.Results)+1)
		lines = append(lines, fmt.Sprintf("%s batch: ok=%d fail=%d total=%d", action, br.SuccessCount, br.FailureCount, br.Total))
		for _, r := range br.Results {
			results = append(results, opRes{Unit: r.ServiceName, Success: r.Success, Error: errStr(r.Error)})
			lines = append(lines, r.Message)
		}
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+strings.Join(lines, "\n"), nil)
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
			results = append(results, opRes{Unit: u, Success: e == nil, Error: errStr(e)})
			out = append(out, plainOp(action, u, e))
		}
		msg := fmt.Sprintf("%s batch: ok=%d fail=%d\n%s", action, ok, len(units)-ok, strings.Join(out, "\n"))
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+msg, nil)
	}

	p.auditOperate(ctx, req, action, units, results, time.Since(start))
	return nil
}

func (p *Plugin) auditOperate(ctx context.Context, req *core.Request, action string, units []string, results []opRes, took time.Duration) {
	// Best-effort audit only (storage may be disabled)
	a := storage.AuditEntry{
		At:       time.Now(),
		ActorID:  req.FromID,
		ChatID:   req.Chat.ChatID,
		ThreadID: req.Chat.ThreadID,
		Plugin:   "systemd",
		Action:   "systemd." + action,
		Target:   strings.Join(units, ","),
		TookMS:   took.Milliseconds(),
	}
	// Username (best-effort)
	if req.Update.Message != nil {
		a.ActorUsername = req.Update.Message.FromUsername
	}
	// Summarize ok/fail + errors + meta
	for _, r := range results {
		if r.Success {
			a.OK++
		} else {
			a.Fail++
			if a.Error == "" && r.Error != "" {
				a.Error = r.Error
			}
		}
	}
	meta := map[string]any{"units": units, "action": action, "cmd": req.Command, "args": req.Args, "results": results}
	if b, err := json.Marshal(meta); err == nil {
		a.MetaJSON = string(b)
	}

	auditCtx := ctx
	if auditCtx == nil {
		auditCtx = context.Background()
	}
	cctx, cancel := context.WithTimeout(auditCtx, 1*time.Second)
	defer cancel()
	_ = p.AppendAudit(cctx, a)
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (p *Plugin) cmdStatus(ctx context.Context, req *core.Request) error {
	mgr := p.mgrSnapshot()
	cfg := p.cfgSnapshot()
	ctx, cancel := p.opCtx(ctx)
	defer cancel()

	if mgr == nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, cfg.Prefix+"systemd manager not available (non-linux/dbus permission?)", nil)
		return nil
	}

	args := req.Args
	if len(args) == 0 {
		// UX: interactive status list with refresh/close.
		if p.ui != nil {
			st := pluginkit.UIState{View: viewStatusList, Page: 0, Size: 10}
			msg, _ := p.viewStatusList(ctx, req, st)
			_, _ = msg.Send(ctx, req.Adapter, req.Chat)
			return nil
		}
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
		// UX: interactive unit detail with refresh/close.
		if p.ui != nil {
			st := pluginkit.UIState{View: viewStatusUnit, Key: u}
			msg, _ := p.viewStatusUnit(ctx, req, st)
			_, _ = msg.Send(ctx, req.Adapter, req.Chat)
			return nil
		}
	}

	lines := make([]string, 0, len(units)+1)
	lines = append(lines, "status:")
	for _, u := range units {
		if !p.allowed(u) {
			lines = append(lines, u+": not allowed")
			continue
		}
		st, err := mgr.GetStatusFullContext(ctx, u)
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
		return nil, errMissingUnitName
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
		return nil, errMissingUnitName
	}
	// accept "foo.service" and normalize
	return normalizeUnits(units), nil
}

func (p *Plugin) allowed(u string) bool {
	p.mu.RLock()
	set := p.cfg.allowSet
	p.mu.RUnlock()
	if set == nil {
		return false
	}
	_, ok := set[u]
	return ok
}
