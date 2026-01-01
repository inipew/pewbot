package system

import (
	"context"
	"log/slog"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"pewbot/internal/core"
	"pewbot/internal/kit"
)

type Plugin struct {
	log  *slog.Logger
	deps core.PluginDeps
}

func New() *Plugin             { return &Plugin{} }
func (p *Plugin) Name() string { return "system" }

func (p *Plugin) Init(ctx context.Context, deps core.PluginDeps) error {
	p.deps = deps
	p.log = deps.Logger.With(slog.String("plugin", p.Name()))
	return nil
}
func (p *Plugin) Start(ctx context.Context) error { return nil }
func (p *Plugin) Stop(ctx context.Context) error  { return nil }

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
			Route:       "sysinfo",
			Description: "runtime/system info (owner only)",
			Usage:       "/sysinfo",
			Access:      core.AccessOwnerOnly,
			Handle: func(ctx context.Context, req *core.Request) error {
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
					"- goroutines: " + itoa(runtime.NumGoroutine()),
					"- mem_alloc: " + bytes(m.Alloc),
					"- mem_sys: " + bytes(m.Sys),
				}, "\n")

				_, _ = req.Adapter.SendText(ctx, req.Chat, msg, &kit.SendOptions{ParseMode: "Markdown"})
				return nil
			},
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

// lightweight helpers (avoid extra deps)
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [32]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + (i % 10))
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
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
		return itoa(int(n)) + "B"
	}
}

func fmt2(n, div uint64, unit string) string {
	x := float64(n) / float64(div)
	ix := int(x * 10) // 1 decimal
	return itoa(ix/10) + "." + itoa(ix%10) + unit
}

func (p *Plugin) cmdSchedList(ctx context.Context, req *core.Request) error {
	s := p.deps.Services.Scheduler
	if s == nil || !s.Enabled() {
		_, _ = req.Adapter.SendText(ctx, req.Chat, "scheduler is disabled", nil)
		return nil
	}

	snap := s.Snapshot()
	if len(snap.Schedules) == 0 {
		_, _ = req.Adapter.SendText(ctx, req.Chat, "no scheduled tasks", nil)
		return nil
	}

	// sort by name for stable output
	sort.Slice(snap.Schedules, func(i, j int) bool { return snap.Schedules[i].Name < snap.Schedules[j].Name })

	now := time.Now()
	lines := make([]string, 0, len(snap.Schedules)+3)
	lines = append(lines, "⏱ scheduled tasks ("+snap.Timezone+"):")
	lines = append(lines, "- workers: "+itoa(snap.Workers)+", queue: "+itoa(snap.QueueLen))

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
		lines = append(lines, "- "+t.Name+": spec="+t.Spec+", next="+next+", timeout="+timeout)
	}

	_, _ = req.Adapter.SendText(ctx, req.Chat, strings.Join(lines, "\n"), &kit.SendOptions{DisablePreview: true})
	return nil
}

func durRel(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return itoa(int(d.Seconds())) + "s"
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return itoa(m) + "m" + itoa(s) + "s"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return itoa(h) + "h" + itoa(m) + "m"
}
