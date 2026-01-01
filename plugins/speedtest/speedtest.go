package speedtest

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"pewbot/internal/core"
	st "pewbot/pkg/speedtest"
)

type Auto struct {
	Schedule string `json:"schedule"`
}
type Config struct {
	Prefix      string `json:"prefix"`
	AutoSpeed   Auto   `json:"auto_speedtest"`
	HistoryFile string `json:"history_file"`
}

type Plugin struct {
	log  *slog.Logger
	deps core.PluginDeps
	cfg  Config
}

func New() *Plugin { return &Plugin{} }
func (p *Plugin) Name() string { return "speedtest" }

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

	// optional cron auto speedtest
	if c.AutoSpeed.Schedule != "" && p.deps.Services != nil && p.deps.Services.Scheduler != nil {
		_, _ = p.deps.Services.Scheduler.AddCron("speedtest-auto", c.AutoSpeed.Schedule, 60*time.Second, func(ctx context.Context) error {
			_, _ = st.Run(ctx)
			return nil
		})
	}
	return nil
}

func (p *Plugin) Commands() []core.Command {
	return []core.Command{
		{
			Route:       "speedtest",
			Aliases:     []string{"st"},
			Description: "run speedtest (stub in starter)",
			Usage:       "/speedtest",
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
				_, err := st.Run(ctx)
				if err != nil {
					_, _ = req.Adapter.SendText(ctx, req.Chat, p.cfg.Prefix+err.Error(), nil)
					return nil
				}
				_, _ = req.Adapter.SendText(ctx, req.Chat, p.cfg.Prefix+"ok", nil)
				return nil
			},
		},
	}
}
