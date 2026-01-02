package system

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"pewbot/internal/core"
	"pewbot/internal/kit"
	"pewbot/internal/pluginkit"
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
			Aliases:     []string{"health"},
			Description: "health check",
			Usage:       "/ping",
			Access:      core.AccessEveryone,
			Handle: func(ctx context.Context, req *core.Request) error {
				_, _ = req.Adapter.SendText(ctx, req.Chat, "pong", nil)
				return nil
			},
		},
		{
			Route:       "uptime",
			Aliases:     []string{"up"},
			Description: "show process uptime",
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
			Description: "runtime/system info (owner only)",
			Usage:       "/sysinfo",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdSysinfo,
		},
		{
			Route:       "sched list",
			Aliases:     []string{"sched_list", "tasks", "task_list"},
			Description: "list scheduled tasks (owner only)",
			Usage:       "/sched_list",
			Access:      core.AccessOwnerOnly,
			Handle:      p.cmdSchedList,
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

	msg := strings.Join([]string{
		"🧠 *sysinfo*",
		"- go: " + runtime.Version(),
		"- module: " + mod,
		fmt.Sprintf("- goroutines: %d", runtime.NumGoroutine()),
		"- mem_alloc: " + fmtBytes(m.Alloc),
		"- mem_sys: " + fmtBytes(m.Sys),
	}, "\n")

	_, _ = req.Adapter.SendText(ctx, req.Chat, msg, &kit.SendOptions{ParseMode: "Markdown"})
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
	lines = append(lines, fmt.Sprintf("- workers: %d, queue: %d", snap.Workers, snap.QueueLen))

	for _, t := range snap.Schedules {
		next := "-"
		if !t.Next.IsZero() {
			next = t.Next.Local().Format("2006-01-02 15:04:05")
			if t.Next.After(now) {
				next += " (" + durRel(t.Next.Sub(now)) + ")"
			}
		}
		timeout := "-"
		if t.Timeout > 0 {
			timeout = t.Timeout.String()
		}
		lines = append(lines, fmt.Sprintf("- %s: spec=%s, next=%s, timeout=%s", t.Name, t.Spec, next, timeout))
	}

	_, _ = req.Adapter.SendText(ctx, req.Chat, strings.Join(lines, "\n"), &kit.SendOptions{DisablePreview: true})
	return nil
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
