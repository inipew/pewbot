package systemd

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"pewbot/internal/core"
	sysd "pewbot/pkg/systemd"
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

	cfg       Config
	downSince map[string]time.Time
}

func New() *Plugin { return &Plugin{downSince: map[string]time.Time{}} }
func (p *Plugin) Name() string { return "systemd" }

func (p *Plugin) Init(ctx context.Context, deps core.PluginDeps) error {
	p.deps = deps
	p.log = deps.Logger.With(slog.String("plugin", p.Name()))
	return nil
}
func (p *Plugin) Start(ctx context.Context) error { return nil }
func (p *Plugin) Stop(ctx context.Context) error  { return nil }

func (p *Plugin) OnConfigChange(ctx context.Context, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	p.cfg = c

	// optional auto recover scheduling
	if c.AutoRecover.Enabled && p.deps.Services != nil && p.deps.Services.Scheduler != nil {
		interval := mustDur(c.AutoRecover.Interval, 30*time.Second)
		minDown := mustDur(c.AutoRecover.MinDown, 3*time.Second)

		_, _ = p.deps.Services.Scheduler.AddInterval("systemd-auto-recover", interval, 10*time.Second, func(ctx context.Context) error {
			for _, u := range p.cfg.AllowUnits {
				active, _ := sysd.IsActive(ctx, u)
				if active {
					delete(p.downSince, u)
					continue
				}
				if _, seen := p.downSince[u]; !seen {
					p.downSince[u] = time.Now()
					continue
				}
				if time.Since(p.downSince[u]) >= minDown {
					p.log.Warn("auto-recover restarting unit", slog.String("unit", u))
					_ = sysd.Restart(ctx, u)
					delete(p.downSince, u)
				}
			}
			return nil
		})
	}
	return nil
}

func (p *Plugin) Commands() []core.Command {
	return []core.Command{
		{
			Route:       "systemd start",
			Aliases:     []string{"systemd_start"},
			Description: "systemctl start <unit>",
			Usage:       "/systemd start <unit>",
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				unit, err := p.unitArg(req)
				if err != nil {
					_, _ = req.Adapter.SendText(ctx, req.Chat, err.Error(), nil)
					return nil
				}
				if err := sysd.Start(ctx, unit); err != nil {
					_, _ = req.Adapter.SendText(ctx, req.Chat, p.cfg.Prefix+"failed: "+err.Error(), nil)
					return nil
				}
				_, _ = req.Adapter.SendText(ctx, req.Chat, p.cfg.Prefix+"started "+unit, nil)
				return nil
			},
		},
		{
			Route:       "systemd stop",
			Aliases:     []string{"systemd_stop"},
			Description: "systemctl stop <unit>",
			Usage:       "/systemd stop <unit>",
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				unit, err := p.unitArg(req)
				if err != nil {
					_, _ = req.Adapter.SendText(ctx, req.Chat, err.Error(), nil)
					return nil
				}
				if err := sysd.Stop(ctx, unit); err != nil {
					_, _ = req.Adapter.SendText(ctx, req.Chat, p.cfg.Prefix+"failed: "+err.Error(), nil)
					return nil
				}
				_, _ = req.Adapter.SendText(ctx, req.Chat, p.cfg.Prefix+"stopped "+unit, nil)
				return nil
			},
		},
		{
			Route:       "systemd restart",
			Aliases:     []string{"systemd_restart"},
			Description: "systemctl restart <unit>",
			Usage:       "/systemd restart <unit>",
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				unit, err := p.unitArg(req)
				if err != nil {
					_, _ = req.Adapter.SendText(ctx, req.Chat, err.Error(), nil)
					return nil
				}
				if err := sysd.Restart(ctx, unit); err != nil {
					_, _ = req.Adapter.SendText(ctx, req.Chat, p.cfg.Prefix+"failed: "+err.Error(), nil)
					return nil
				}
				_, _ = req.Adapter.SendText(ctx, req.Chat, p.cfg.Prefix+"restarted "+unit, nil)
				return nil
			},
		},
	}
}

func (p *Plugin) unitArg(req *core.Request) (string, error) {
	if len(req.Args) < 1 {
		return "", errors.New("usage: " + usageFor(req.Path) + " <unit>")
	}
	u := strings.TrimSpace(req.Args[0])
	if !p.allowed(u) {
		return "", errors.New("unit not allowed")
	}
	return u, nil
}

func usageFor(path []string) string {
	if len(path) == 0 {
		return "/systemd"
	}
	// Telegram shows as '/systemd start' etc
	return "/" + strings.Join(path, " ")
}

func (p *Plugin) allowed(u string) bool {
	for _, x := range p.cfg.AllowUnits {
		if x == u {
			return true
		}
	}
	return false
}
func mustDur(s string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
