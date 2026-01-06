package speedtest

import (
	"context"
	"encoding/json"
	"fmt"
	logx "pewbot/pkg/logx"
	"time"

	core "pewbot/internal/plugin"
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
	if c.ServerCount <= 0 {
		c.ServerCount = 5
	}
	if c.FullTestServers <= 0 {
		c.FullTestServers = 1
	}
	if c.FullTestServers > c.ServerCount {
		c.FullTestServers = c.ServerCount
	}
	// Default saving mode to true unless explicitly set.
	if _, ok := probe["saving_mode"]; !ok {
		c.SavingMode = true
	}
	if c.MaxConnections <= 0 {
		c.MaxConnections = 4
	}
	if c.PingConcurrency <= 0 {
		c.PingConcurrency = 4
	}

	// Default resource-lean network behavior unless explicitly configured.
	if _, ok := probe["disable_http2"]; !ok {
		c.DisableHTTP2 = true
	}
	if _, ok := probe["disable_keepalives"]; !ok {
		c.DisableKeepAlives = true
	}

	// Packet loss probing is extra network work; keep it off by default.
	if _, ok := probe["packet_loss_enabled"]; !ok {
		c.PacketLossEnabled = false
	}
	if c.PacketLossTimeoutSeconds > 0 {
		c.packetLossTO = time.Duration(c.PacketLossTimeoutSeconds) * time.Second
	} else {
		c.packetLossTO = 3 * time.Second
	}

	// Default post-run memory reclamation unless explicitly configured.
	if _, ok := probe["post_run_gc"]; !ok {
		c.PostRunGC = true
	}
	if _, ok := probe["post_run_free_os_memory"]; !ok {
		c.PostRunFreeOSMemory = true
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
			result, msg, err := p.runSpeedtest(ctx, "schedule")
			if err == ErrAlreadyRunning {
				// Avoid failing the scheduled task when a manual run is in progress.
				p.PublishEvent("speedtest.schedule.skipped_running", map[string]any{"task": sc.TaskName})
				return nil
			}
			if err != nil {
				_ = p.Notify().Error("Speedtest failed: " + err.Error())
				return err
			}
			if result != nil {
				if h := p.history(); h != nil {
					if err := h.Append(result); err != nil {
						p.Log.Warn("Failed to save result", logx.Any("err", err))
					}
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
