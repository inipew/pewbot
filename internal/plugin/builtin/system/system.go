package system

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	core "pewbot/internal/plugin"
	pluginkit "pewbot/internal/plugin/kit"
	tasksched "pewbot/internal/task/scheduler"
	kit "pewbot/internal/transport"
	"pewbot/pkg/tgui"
)

type Plugin struct {
	pluginkit.EnhancedPluginBase
	startedAt time.Time
}

func New() *Plugin             { return &Plugin{} }
func (p *Plugin) Name() string { return "system" }

func (p *Plugin) Init(ctx context.Context, deps core.PluginDeps) error {
	p.InitEnhanced(deps, p.Name())
	if p.startedAt.IsZero() {
		p.startedAt = time.Now()
	}
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	p.StartEnhanced(ctx)
	if p.startedAt.IsZero() {
		p.startedAt = time.Now()
	}
	return nil
}

func (p *Plugin) Stop(ctx context.Context) error { return p.StopEnhanced(ctx) }

func (p *Plugin) Commands() []core.Command {
	return []core.Command{
		{
			Route:       "ping",
			Description: "cek kesehatan bot",
			Usage:       "/ping",
			Access:      core.AccessEveryone,
			Handle: func(ctx context.Context, req *core.Request) error {
				_, _ = req.Adapter.SendText(ctx, req.Chat, "pong", nil)
				return nil
			},
		},
		{
			Route:       "health",
			Aliases:     []string{"status"},
			Description: "cek health bot (plugin/services)",
			Usage:       "/health [check]",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdHealth,
		},
		{
			Route:       "uptime",
			Aliases:     []string{"up"},
			Description: "tampilkan lama bot berjalan",
			Usage:       "/uptime",
			Access:      core.AccessEveryone,
			Handle: func(ctx context.Context, req *core.Request) error {
				up := time.Since(p.startedAt)
				_, _ = req.Adapter.SendText(ctx, req.Chat, "uptime: "+durRel(up), nil)
				return nil
			},
		},
		{
			Route:       "sysinfo",
			Description: "info runtime/sistem",
			Usage:       "/sysinfo",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdSysinfo,
		},
		{
			// /sched is intentionally short: currently it's only a list command.
			// If more subcommands are added later (add/remove/etc.), /sched will remain a sensible default.
			Route:       "sched",
			Aliases:     []string{"sched_list", "tasks", "task_list"},
			Description: "daftar task terjadwal",
			Usage:       "/sched  (atau /sched_list)",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdSchedList,
		},
		{
			Route:       "sched list",
			Description: "daftar task terjadwal",
			Usage:       "/sched list  (alias: /sched)",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdSchedList,
		},
		{
			Route:       "sched status",
			Aliases:     []string{"sched_status"},
			Description: "ringkasan scheduler & task engine",
			Usage:       "/sched status",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdSchedStatus,
		},
		{
			Route:       "task status",
			Aliases:     []string{"task_status"},
			Description: "alias untuk /sched status",
			Usage:       "/task status",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdSchedStatus,
		},
	}
}

func (p *Plugin) cmdSysinfo(ctx context.Context, req *core.Request) error {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	bi, _ := debug.ReadBuildInfo()
	mod := ""
	if bi != nil {
		mod = bi.Main.Path + " " + bi.Main.Version
	}

	msg := tgui.New().
		Title("🧠", "sysinfo").
		KV("go", runtime.Version()).
		KV("module", mod).
		KV("goroutines", fmt.Sprintf("%d", runtime.NumGoroutine())).
		KV("mem_alloc", fmtBytes(m.Alloc)).
		KV("mem_sys", fmtBytes(m.Sys)).
		Build()

	_, _ = msg.Send(ctx, req.Adapter, req.Chat)
	return nil
}

func (p *Plugin) cmdSchedList(ctx context.Context, req *core.Request) error {
	var s core.SchedulerPort
	if p.Deps.Services != nil {
		s = p.Deps.Services.Scheduler
	}
	if s == nil || !s.Enabled() {
		_, _ = req.Adapter.SendText(ctx, req.Chat, "scheduler is disabled", nil)
		return nil
	}

	snap := s.Snapshot()
	if len(snap.Schedules) == 0 {
		_, _ = req.Adapter.SendText(ctx, req.Chat, "no scheduled tasks", nil)
		return nil
	}

	sort.Slice(snap.Schedules, func(i, j int) bool { return snap.Schedules[i].Name < snap.Schedules[j].Name })

	now := time.Now()
	lines := make([]string, 0, len(snap.Schedules)+3)
	lines = append(lines, "⏱ scheduled tasks ("+snap.Timezone+"):")
	queueStr := fmt.Sprintf("%d", snap.QueueLen)
	if snap.QueueCap > 0 {
		queueStr = fmt.Sprintf("%d/%d", snap.QueueLen, snap.QueueCap)
	}
	wLine := fmt.Sprintf("- workers: %d, queue: %s", snap.Workers, queueStr)
	if snap.ActiveLimit > 0 {
		wLine = fmt.Sprintf("- workers: %d, active: %d/%d, inflight: %d, waiting: %d, queue: %s", snap.Workers, snap.ActiveLimit, maxInt(1, snap.ActiveMax), snap.InFlight, snap.WaitingForPermit, queueStr)
	}
	lines = append(lines, wLine)

	// Show effective executor defaults to avoid confusion when per-task timeout = 0.
	if snap.DefaultTimeout > 0 {
		lines = append(lines, fmt.Sprintf("- default timeout: %s (applies when task timeout=0)", snap.DefaultTimeout))
	} else {
		lines = append(lines, "- default timeout: disabled (task timeout=0 means no timeout)")
	}
	if snap.RetryMax > 0 {
		lines = append(lines, fmt.Sprintf("- retry: max=%d, base=%s, max_delay=%s, jitter=%.0f%%",
			snap.RetryMax, snap.RetryBase, snap.RetryMaxDelay, snap.RetryJitter*100))
	} else {
		lines = append(lines, "- retry: disabled (max=0)")
	}

	for _, t := range snap.Schedules {
		next := "-"
		if !t.Next.IsZero() {
			next = t.Next.Local().Format("2006-01-02 15:04:05")
			if t.Next.After(now) {
				next += " (" + durRel(t.Next.Sub(now)) + ")"
			}
		}
		timeout := "-"
		switch {
		case t.Timeout > 0:
			timeout = t.Timeout.String()
		case snap.DefaultTimeout > 0:
			timeout = "default(" + snap.DefaultTimeout.String() + ")"
		default:
			timeout = "none"
		}
		lines = append(lines, fmt.Sprintf("- %s: spec=%s, next=%s, timeout=%s", t.Name, t.Spec, next, timeout))
	}

	_, _ = req.Adapter.SendText(ctx, req.Chat, strings.Join(lines, "\n"), &kit.SendOptions{DisablePreview: true})
	return nil
}

func (p *Plugin) cmdSchedStatus(ctx context.Context, req *core.Request) error {
	var s core.SchedulerPort
	if p.Deps.Services != nil {
		s = p.Deps.Services.Scheduler
	}
	if s == nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, "scheduler service not available", nil)
		return nil
	}

	snap := s.Snapshot()
	lines := make([]string, 0, 32)
	lines = append(lines, "🧭 sched status")
	lines = append(lines, fmt.Sprintf("- enabled: %t", snap.Enabled))
	lines = append(lines, fmt.Sprintf("- tz: %s", snap.Timezone))

	queueStr := fmt.Sprintf("%d", snap.QueueLen)
	if snap.QueueCap > 0 {
		queueStr = fmt.Sprintf("%d/%d", snap.QueueLen, snap.QueueCap)
	}
	if snap.Dropped > 0 {
		wLine := fmt.Sprintf("- workers: %d, queue: %s (dropped=%d)", snap.Workers, queueStr, snap.Dropped)
		if snap.ActiveLimit > 0 {
			wLine = fmt.Sprintf("- workers: %d, active: %d/%d, inflight: %d, waiting: %d, queue: %s (dropped=%d)", snap.Workers, snap.ActiveLimit, maxInt(1, snap.ActiveMax), snap.InFlight, snap.WaitingForPermit, queueStr, snap.Dropped)
		}
		lines = append(lines, wLine)
	} else {
		wLine := fmt.Sprintf("- workers: %d, queue: %s", snap.Workers, queueStr)
		if snap.ActiveLimit > 0 {
			wLine = fmt.Sprintf("- workers: %d, active: %d/%d, inflight: %d, waiting: %d, queue: %s", snap.Workers, snap.ActiveLimit, maxInt(1, snap.ActiveMax), snap.InFlight, snap.WaitingForPermit, queueStr)
		}
		lines = append(lines, wLine)
	}

	// Next runs (top 5)
	if len(snap.Schedules) == 0 {
		lines = append(lines, "- schedules: none")
	} else {
		items := append([]tasksched.ScheduleInfo(nil), snap.Schedules...)
		sort.Slice(items, func(i, j int) bool {
			a, b := items[i].Next, items[j].Next
			if a.IsZero() && b.IsZero() {
				return items[i].Name < items[j].Name
			}
			if a.IsZero() {
				return false
			}
			if b.IsZero() {
				return true
			}
			if a.Equal(b) {
				return items[i].Name < items[j].Name
			}
			return a.Before(b)
		})
		n := 5
		if len(items) < n {
			n = len(items)
		}
		now := time.Now()
		lines = append(lines, fmt.Sprintf("- next schedules (top %d):", n))
		for i := 0; i < n; i++ {
			it := items[i]
			next := "-"
			if !it.Next.IsZero() {
				next = it.Next.Local().Format("2006-01-02 15:04:05")
				if it.Next.After(now) {
					next += " (in " + durRel(it.Next.Sub(now)) + ")"
				}
			}
			lines = append(lines, fmt.Sprintf("  %d) %s: next=%s, spec=%s", i+1, it.Name, next, it.Spec))
		}
	}

	// Last history item (most recent)
	if len(snap.History) == 0 {
		lines = append(lines, "- last run: -")
	} else {
		it := snap.History[len(snap.History)-1]
		st := it.Started.Local().Format("2006-01-02 15:04:05")
		ag := durRel(time.Since(it.Started))
		status := "ok"
		if it.Error != "" {
			status = "fail: " + shorten(it.Error, 120)
		}
		lines = append(lines, "- last run:")
		lines = append(lines, fmt.Sprintf("  %s (%s) started=%s (%s ago) qdelay=%s dur=%s", it.Name, status, st, ag, it.QueueDelay, it.Duration))
	}

	_, _ = req.Adapter.SendText(ctx, req.Chat, strings.Join(lines, "\n"), &kit.SendOptions{DisablePreview: true})
	return nil
}

func shorten(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func fmtBytes(n uint64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1fGB", float64(n)/GB)
	case n >= MB:
		return fmt.Sprintf("%.1fMB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.1fKB", float64(n)/KB)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func durRel(d time.Duration) string {
	if d < 0 {
		d = -d
	}
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
