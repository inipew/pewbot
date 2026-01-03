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
	return &Plugin{}
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
	// Load/compact existing history if configured without retaining it in memory.
	p.mu.RLock()
	historyFile := p.cfg.HistoryFile
	p.mu.RUnlock()

	if historyFile != "" {
		if count, err := p.compactHistoryFile(historyFile); err != nil {
			p.Log.Warn("Failed to compact history", slog.Any("err", err))
		} else if count > 0 {
			p.Log.Info("History loaded", slog.Int("count", count))
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
	// Fail fast on removed legacy fields to avoid silent misconfig.
	// (Old schema: timeout_seconds, timeouts.job/request)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err == nil {
		if _, ok := probe["timeout_seconds"]; ok {
			return fmt.Errorf("speedtest.timeout_seconds is no longer supported; use speedtest.timeouts.operation")
		}
	}

	if err := json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("unmarshal config: %w", err)
	}

	// Set defaults
	if c.ServerCount == 0 {
		c.ServerCount = 3
	}

	// Validate optional standardized timeouts.
	if err := c.Timeouts.Validate("speedtest.timeouts"); err != nil {
		return err
	}

	// timeouts.operation bounds a single speedtest run.
	c.operationTimeout = c.Timeouts.OperationOr(30 * time.Second)

	// timeouts.task controls scheduler task timeout.
	c.taskTimeout = c.Timeouts.TaskOr(60 * time.Second)

	p.mu.Lock()
	p.cfg = c
	p.mu.Unlock()

	// Setup / remove auto speedtest schedule (new unified schema).
	sc := c.Scheduler
	if sc.TaskName == "" {
		sc.TaskName = "auto_speedtest"
	}
	if sc.Enabled && sc.Schedule == "" {
		return fmt.Errorf("speedtest.scheduler.schedule is required when scheduler.enabled=true")
	}
	// Validate schedule early to avoid removing a working schedule on a bad config.
	if sc.Enabled {
		if _, err := core.ParseSchedule(sc.Schedule); err != nil {
			return fmt.Errorf("invalid speedtest.scheduler.schedule: %w", err)
		}
	}

	// Reconcile auto schedule.
	oldTask := func() string {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.autoTask
	}()
	if p.Schedule() != nil {
		if oldTask != "" && oldTask != sc.TaskName {
			p.Schedule().Remove(oldTask)
		}
		if !sc.Enabled || sc.Schedule == "" {
			if oldTask != "" {
				p.Schedule().Remove(oldTask)
			}
			p.mu.Lock()
			p.autoTask = ""
			p.mu.Unlock()
			return nil
		}
	} else {
		// scheduler service not available; keep config but don't fail reload
		p.Log.Warn("scheduler not available; auto speedtest not scheduled")
		return nil
	}

	// Use helper API: schedule can be cron (crontab.guru), HH:MM, or a Go duration.
	// Namespaced task name becomes "speedtest:<task_name>".
	err := p.Schedule().Spec(sc.TaskName, sc.Schedule).
		Timeout(c.taskTimeout).
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
	if err == nil {
		p.mu.Lock()
		p.autoTask = sc.TaskName
		p.mu.Unlock()
	}
	return err
}
