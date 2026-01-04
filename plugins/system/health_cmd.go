package system

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"pewbot/internal/core"
	"pewbot/internal/kit"
)

func (p *Plugin) cmdHealth(ctx context.Context, req *core.Request) error {
	ps := req.Services
	if ps == nil || ps.Plugins == nil {
		_, _ = req.Adapter.SendText(ctx, req.Chat, "plugins service is unavailable", nil)
		return nil
	}

	// Usage:
	//   /health        -> show last known (cached) health state
	//   /health check  -> trigger on-demand checks (bounded) then show updated state
	check := false
	if len(req.Args) > 0 && strings.EqualFold(req.Args[0], "check") {
		check = true
	}
	if req.BoolFlags != nil {
		if req.BoolFlags["check"] || req.BoolFlags["refresh"] {
			check = true
		}
	}

	if check {
		// Keep the refresh bounded even if a plugin blocks.
		cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		_ = ps.Plugins.CheckHealth(cctx, nil)
		cancel()
	}

	plSnap := ps.Plugins.Snapshot()

	// Runtime info.
	up := time.Since(p.startedAt)
	gos := runtime.NumGoroutine()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	// Scheduler state + per-plugin schedule counts.
	schedState := "disabled"
	schedWorkers := 0
	schedQueue := "-"
	totalSchedules := 0
	unscopedSchedules := 0
	perPluginSchedules := map[string]int{}
	if ps.Scheduler != nil {
		s := ps.Scheduler.Snapshot()
		if ps.Scheduler.Enabled() {
			schedState = "enabled"
		}
		schedWorkers = s.Workers
		if s.QueueCap > 0 {
			schedQueue = fmt.Sprintf("%d/%d", s.QueueLen, s.QueueCap)
		} else {
			schedQueue = fmt.Sprintf("%d", s.QueueLen)
		}
		totalSchedules = len(s.Schedules)
		for _, t := range s.Schedules {
			name := t.Name
			if i := strings.IndexByte(name, ':'); i > 0 {
				perPluginSchedules[name[:i]]++
			} else {
				unscopedSchedules++
			}
		}
	}

	// Supervisor counters.
	appSupLine := "app:   n/a"
	if ps.AppSupervisor != nil {
		c := ps.AppSupervisor.Counters()
		appSupLine = fmt.Sprintf("app:   active=%d started=%d", c.Active, c.Started)
	}
	plSupLine := "plugin: n/a"
	if sup := p.Supervisor(); sup != nil {
		c := sup.Counters()
		plSupLine = fmt.Sprintf("plugin(%s): active=%d started=%d", p.Name(), c.Active, c.Started)
	}

	// Render.
	// Use plain text (no Markdown) to avoid Telegram parse failures.
	var b strings.Builder
	b.Grow(4096)

	b.WriteString("🏥 /health\n")
	b.WriteString("━━━━━━━━━━━━━━━━━━━━\n")
	b.WriteString(fmt.Sprintf("uptime: %s\n", durRel(up)))
	b.WriteString(fmt.Sprintf("goroutines: %d\n", gos))
	b.WriteString("\n")

	b.WriteString("💾 mem\n")
	b.WriteString(fmt.Sprintf("  Alloc:     %s\n", fmtBytes(m.Alloc)))
	b.WriteString(fmt.Sprintf("  Sys:       %s\n", fmtBytes(m.Sys)))
	b.WriteString(fmt.Sprintf("  HeapAlloc: %s\n", fmtBytes(m.HeapAlloc)))
	b.WriteString(fmt.Sprintf("  HeapInuse: %s\n", fmtBytes(m.HeapInuse)))
	b.WriteString(fmt.Sprintf("  NumGC:     %d\n", m.NumGC))
	b.WriteString("\n")

	b.WriteString("⏱ scheduler\n")
	b.WriteString(fmt.Sprintf("  state:     %s\n", schedState))
	b.WriteString(fmt.Sprintf("  workers:   %d\n", schedWorkers))
	b.WriteString(fmt.Sprintf("  queue:     %s\n", schedQueue))
	b.WriteString(fmt.Sprintf("  schedules: %d\n", totalSchedules))
	if unscopedSchedules > 0 {
		b.WriteString(fmt.Sprintf("  unscoped:  %d\n", unscopedSchedules))
	}
	b.WriteString("\n")

	b.WriteString("🔌 plugins\n")
	if len(plSnap.Plugins) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, st := range plSnap.Plugins {
			schedN := perPluginSchedules[st.Name]

			// health summary (best-effort)
			h := "health=na"
			if st.HasHealthChecker {
				switch {
				case st.LastHealth.At.IsZero():
					h = "health=nodata"
				case st.LastHealth.Err != "":
					h = "health=fail"
				default:
					status := st.LastHealth.Status
					if status == "" {
						status = "ok"
					}
					h = "health=" + status
				}
			}

			line := fmt.Sprintf("  - %s en=%v run=%v sched=%d %s", st.Name, st.Enabled, st.Running, schedN, h)
			if st.Quarantined {
				q := strings.TrimSpace(st.QuarantineErr)
				if q == "" {
					q = "(no reason)"
				}
				if !st.QuarantineSince.IsZero() {
					line += fmt.Sprintf(" | quarantine %s ago: %s", durRel(time.Since(st.QuarantineSince)), q)
				} else {
					line += " | quarantine: " + q
				}
			}
			b.WriteString(line + "\n")
		}
		if unscopedSchedules > 0 {
			b.WriteString(fmt.Sprintf("  - <unscoped> en=- run=- sched=%d\n", unscopedSchedules))
		}
	}
	b.WriteString("\n")

	b.WriteString("🧵 supervisor\n")
	b.WriteString("  " + appSupLine + "\n")
	b.WriteString("  " + plSupLine + "\n")

	if check {
		b.WriteString("\nrefreshed: yes\n")
	}

	_, err := req.Adapter.SendText(ctx, req.Chat, b.String(), &kit.SendOptions{ParseMode: "", DisablePreview: true})
	return err
}
