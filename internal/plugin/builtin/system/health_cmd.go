package system

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	core "pewbot/internal/plugin"
	rtsup "pewbot/internal/runtime/supervisor"
	"pewbot/pkg/tgui"
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
	supDetail := false
	if len(req.Args) > 0 && strings.EqualFold(req.Args[0], "check") {
		check = true
	}
	if len(req.Args) > 0 {
		if strings.EqualFold(req.Args[0], "sup") || strings.EqualFold(req.Args[0], "supervisor") || strings.EqualFold(req.Args[0], "detail") {
			supDetail = true
		}
	}
	if req.BoolFlags != nil {
		if req.BoolFlags["check"] || req.BoolFlags["refresh"] {
			check = true
		}
		if req.BoolFlags["sup"] || req.BoolFlags["supervisor"] || req.BoolFlags["detail"] {
			supDetail = true
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
	schedActiveMax := 0
	schedActiveLimit := 0
	schedInFlight := 0
	schedWaiting := 0
	schedQueue := "-"
	schedDropped := uint64(0)
	schedDroppedQF := uint64(0)
	schedDroppedStale := uint64(0)
	schedMaxQueueDelay := time.Duration(0)
	totalSchedules := 0
	unscopedSchedules := 0
	perPluginSchedules := map[string]int{}
	if ps.Scheduler != nil {
		s := ps.Scheduler.Snapshot()
		if ps.Scheduler.Enabled() {
			schedState = "enabled"
		}
		schedWorkers = s.Workers
		schedActiveMax = s.ActiveMax
		schedActiveLimit = s.ActiveLimit
		schedInFlight = s.InFlight
		schedWaiting = s.WaitingForPermit
		schedDropped = s.Dropped
		schedDroppedQF = s.DroppedQueueFull
		schedDroppedStale = s.DroppedStale
		schedMaxQueueDelay = s.MaxQueueDelay
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
	extraSup := map[string]*rtsup.Supervisor{}
	if ps.RuntimeSupervisors != nil {
		extraSup = ps.RuntimeSupervisors.Snapshot()
	}
	extraNames := make([]string, 0, len(extraSup))
	for name, sup := range extraSup {
		if sup == nil {
			continue
		}
		extraNames = append(extraNames, name)
	}
	sort.Strings(extraNames)

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
	schedActiveMaxSafe := schedActiveMax
	if schedActiveMaxSafe <= 0 {
		schedActiveMaxSafe = 1
	}
	if schedActiveLimit > 0 {
		b.WriteString(fmt.Sprintf("  workers:   %d (active %d/%d inflight=%d waiting=%d)\n", schedWorkers, schedActiveLimit, schedActiveMaxSafe, schedInFlight, schedWaiting))
	} else {
		b.WriteString(fmt.Sprintf("  workers:   %d\n", schedWorkers))
	}
	b.WriteString(fmt.Sprintf("  queue:     %s\n", schedQueue))
	if schedMaxQueueDelay > 0 {
		b.WriteString(fmt.Sprintf("  max_wait:  %s\n", schedMaxQueueDelay))
	}
	if schedDropped > 0 {
		b.WriteString(fmt.Sprintf("  dropped:   %d (queue_full=%d stale=%d)\n", schedDropped, schedDroppedQF, schedDroppedStale))
	}
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
	for _, name := range extraNames {
		sup := extraSup[name]
		if sup == nil {
			continue
		}
		c := sup.Counters()
		b.WriteString(fmt.Sprintf("  %s: active=%d started=%d\n", name, c.Active, c.Started))
	}

	if supDetail {
		b.WriteString("\n🧵 supervisor detail\n")
		if ps.AppSupervisor != nil {
			b.WriteString("\n  app goroutines\n")
			writeSupDetails(&b, ps.AppSupervisor.Snapshot(), 12)
		}
		if sup := p.Supervisor(); sup != nil {
			b.WriteString("\n  plugin goroutines\n")
			writeSupDetails(&b, sup.Snapshot(), 12)
		}
		for _, name := range extraNames {
			sup := extraSup[name]
			if sup == nil {
				continue
			}
			b.WriteString("\n  " + name + " goroutines\n")
			writeSupDetails(&b, sup.Snapshot(), 12)
		}
	}

	if check {
		b.WriteString("\nrefreshed: yes\n")
	}

	msg := tgui.New().
		ParseMode("").
		DisablePreview(true).
		Title("🩺", "health").
		Blank().
		RawLine(b.String()).
		Build()
	_, err := msg.Send(ctx, req.Adapter, req.Chat)
	return err
}

func writeSupDetails(b *strings.Builder, snap rtsup.SupervisorSnapshot, limit int) {
	if limit <= 0 {
		limit = 10
	}
	n := 0
	for _, g := range snap.Goroutines {
		// Hide internal wrapper goroutines used by GoRestart to avoid noise.
		if strings.HasSuffix(g.Name, ".restart") {
			continue
		}
		if g.Active == 0 && g.Started == 0 {
			continue
		}
		line := fmt.Sprintf("    - %s active=%d started=%d restarts=%d panics=%d", g.Name, g.Active, g.Started, g.Restarts, g.Panics)
		if g.LastErr != "" {
			when := ""
			if !g.LastErrAt.IsZero() {
				when = fmt.Sprintf(" (%s ago)", durRel(time.Since(g.LastErrAt)))
			}
			line += ", last_err=" + shorten(g.LastErr, 96) + when
		}
		if !g.LastStopAt.IsZero() {
			line += fmt.Sprintf(", last_stop=%s ago", durRel(time.Since(g.LastStopAt)))
		}
		b.WriteString(line + "\n")
		n++
		if n >= limit {
			break
		}
	}
	if n == 0 {
		b.WriteString("    (no data)\n")
	}
}
