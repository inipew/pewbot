package systemd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"pewbot/internal/core"
	"pewbot/internal/kit"
	sm "pewbot/pkg/systemdmanager"
)

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
