package speedtest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"pewbot/internal/core"
)

func New() *Plugin {
	return &Plugin{
		history: &SpeedtestHistory{
			Results: make([]SpeedtestResult, 0),
		},
	}
}

// Name returns plugin name
func (p *Plugin) Name() string {
	return "speedtest"
}

// Init initializes the plugin
func (p *Plugin) Init(ctx context.Context, deps core.PluginDeps) error {
	p.InitEnhanced(deps, p.Name())
	return nil
}

// Start starts the plugin
func (p *Plugin) Start(ctx context.Context) error {
	p.StartEnhanced(ctx)
	// Load existing history if configured
	p.mu.RLock()
	historyFile := p.cfg.HistoryFile
	p.mu.RUnlock()

	if historyFile != "" {
		if err := p.loadHistory(historyFile); err != nil {
			p.Log.Warn("Failed to load history", slog.Any("err", err))
		} else {
			p.Log.Info("History loaded", slog.Int("count", len(p.history.Results)))
		}
	}
	return nil
}

// Stop stops the plugin
func (p *Plugin) Stop(ctx context.Context) error {
	return p.StopEnhanced(ctx)
}

// OnConfigChange handles configuration updates
func (p *Plugin) OnConfigChange(ctx context.Context, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}

	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}

	// Set defaults
	if c.ServerCount == 0 {
		c.ServerCount = 3
	}
	if c.Timeout == 0 {
		c.Timeout = 30
	}

	p.mu.Lock()
	p.cfg = c
	p.mu.Unlock()

	// Setup / remove auto speedtest schedule.
	if c.AutoSpeed.Schedule == "" {
		if p.Schedule() != nil {
			p.Schedule().Remove("auto")
		}
		return nil
	}
	if p.Schedule() == nil {
		return nil
	}
	// Use helper API: schedule can be cron (crontab.guru), HH:MM, or a Go duration.
	// Namespaced task name becomes "speedtest:auto".
	return p.Schedule().Spec("auto", c.AutoSpeed.Schedule).
		Timeout(60 * time.Second).
		SkipIfRunning().
		Do(func(ctx context.Context) error {
			result, msg, err := p.runSpeedtest(ctx)
			if err != nil {
				_ = p.Notify().Error("Speedtest failed: " + err.Error())
				return err
			}
			if result != nil {
				if err := p.saveResult(result); err != nil {
					p.Log.Warn("Failed to save result", slog.Any("err", err))
				}
			}
			_ = p.Notify().Info(msg)
			return nil
		})

}
