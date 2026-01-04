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

	snap := ps.Plugins.Snapshot()

	// Basic runtime info.
	up := time.Since(p.startedAt)
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	// Use plain text (no Markdown) to avoid Telegram parse failures.
	// This command is operational; reliability beats fancy formatting.

	loaded := len(snap.Plugins)
	enabledN := 0
	runningN := 0
	quarantinedN := 0
	unhealthyN := 0
	for _, st := range snap.Plugins {
		if st.Enabled {
			enabledN++
		}
		if st.Running {
			runningN++
		}
		if st.Quarantined {
			quarantinedN++
		}
		if st.Enabled && st.Running && st.HasHealthChecker && st.LastHealth.Err != "" {
			unhealthyN++
		}
	}

	status := "Running"
	if quarantinedN > 0 || unhealthyN > 0 {
		status = "Degraded"
	}

	// Scheduler summary (if available).
	schedEnabled := false
	schedWorkers := 0
	schedQueue := "-"
	schedDropped := uint64(0)
	schedCount := 0
	defTimeout := time.Duration(0)
	retryLine := "disabled"
	if ps.Scheduler != nil && ps.Scheduler.Enabled() {
		s := ps.Scheduler.Snapshot()
		schedEnabled = true
		schedWorkers = s.Workers
		schedDropped = s.Dropped
		schedCount = len(s.Schedules)
		defTimeout = s.DefaultTimeout
		schedQueue = fmt.Sprintf("%d", s.QueueLen)
		if s.QueueCap > 0 {
			schedQueue = fmt.Sprintf("%d/%d", s.QueueLen, s.QueueCap)
		}
		if s.RetryMax > 0 {
			retryLine = fmt.Sprintf("max=%d, base=%s, max_delay=%s, jitter=%.0f%%", s.RetryMax, s.RetryBase, s.RetryMaxDelay, s.RetryJitter*100)
		}
	}

	admins := "-"
	if len(req.OwnerUserID) > 0 {
		parts := make([]string, 0, len(req.OwnerUserID))
		for _, id := range req.OwnerUserID {
			parts = append(parts, fmt.Sprintf("%d", id))
		}
		admins = strings.Join(parts, ", ")
	}

	// Render.
	var b strings.Builder
	b.Grow(2048)

	// Header
	b.WriteString("🏥 Bot Health Status\n")
	b.WriteString("━━━━━━━━━━━━━━━━━━━━\n")
	b.WriteString(fmt.Sprintf("Status: %s\n", status))
	b.WriteString(fmt.Sprintf("Uptime: %s\n", durRel(up)))
	b.WriteString(fmt.Sprintf("Admin ID: %s\n", admins))
	b.WriteString(fmt.Sprintf("Plugins: %d loaded (%d enabled, %d running", loaded, enabledN, runningN))
	if quarantinedN > 0 {
		b.WriteString(fmt.Sprintf(", %d quarantined", quarantinedN))
	}
	b.WriteString(")\n")
	b.WriteString(fmt.Sprintf("Scheduled Tasks: %d\n", schedCount))
	b.WriteString("\n")

	// Memory
	b.WriteString("💾 Memory Usage\n")
	b.WriteString(fmt.Sprintf("  • Allocated: %s\n", fmtBytes(m.Alloc)))
	b.WriteString(fmt.Sprintf("  • System:    %s\n", fmtBytes(m.Sys)))
	b.WriteString(fmt.Sprintf("  • Heap Inuse: %s\n", fmtBytes(m.HeapInuse)))
	b.WriteString(fmt.Sprintf("  • GC Runs:   %d\n", m.NumGC))
	b.WriteString("\n")

	// Runtime
	b.WriteString("🤖 Runtime\n")
	b.WriteString(fmt.Sprintf("  • Go Version: %s\n", runtime.Version()))
	b.WriteString(fmt.Sprintf("  • Goroutines: %d\n", runtime.NumGoroutine()))
	b.WriteString(fmt.Sprintf("  • CPUs:       %d\n", runtime.NumCPU()))
	b.WriteString("\n")

	// Scheduler/Engine
	b.WriteString("📊 Scheduler / Task Engine\n")
	b.WriteString(fmt.Sprintf("  • Enabled: %v\n", schedEnabled))
	if schedEnabled {
		b.WriteString(fmt.Sprintf("  • Workers: %d\n", schedWorkers))
		b.WriteString(fmt.Sprintf("  • Queue:   %s\n", schedQueue))
		b.WriteString(fmt.Sprintf("  • Dropped: %d\n", schedDropped))
		if defTimeout > 0 {
			b.WriteString(fmt.Sprintf("  • Default Timeout: %s\n", defTimeout))
		} else {
			b.WriteString("  • Default Timeout: none\n")
		}
		b.WriteString(fmt.Sprintf("  • Retry: %s\n", retryLine))
	}
	b.WriteString("\n")

	// Plugins
	b.WriteString("🔌 Plugins\n")
	if loaded == 0 {
		b.WriteString("  • (none)\n")
	} else {
		for _, st := range snap.Plugins {
			icon := "✅"
			switch {
			case st.Quarantined:
				icon = "🧯"
			case !st.Enabled:
				icon = "⛔"
			case !st.Running:
				icon = "🟨"
			}

			h := "-"
			if st.HasHealthChecker {
				if st.LastHealth.At.IsZero() {
					h = "no data"
				} else if st.LastHealth.Err != "" {
					if st.Enabled && st.Running && !st.Quarantined {
						icon = "⚠️"
					}
					age := time.Since(st.LastHealth.At)
					h = fmt.Sprintf("fail (%s ago): %s", durRel(age), st.LastHealth.Err)
				} else {
					age := time.Since(st.LastHealth.At)
					status := st.LastHealth.Status
					if status == "" {
						status = "ok"
					}
					h = fmt.Sprintf("%s (%s ago)", status, durRel(age))
				}
			}

			extra := ""
			if st.Quarantined && st.QuarantineErr != "" {
				extra = " | quarantined: " + st.QuarantineErr
			}
			b.WriteString(fmt.Sprintf("  • %s %s — %s%s\n", icon, st.Name, h, extra))
		}
	}

	if check {
		b.WriteString("\n")
		b.WriteString("Refreshed: yes\n")
	}

	_, err := req.Adapter.SendText(ctx, req.Chat, b.String(), &kit.SendOptions{ParseMode: "", DisablePreview: true})
	return err
}
