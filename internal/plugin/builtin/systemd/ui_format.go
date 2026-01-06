package systemd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	sm "pewbot/pkg/systemdmanager"
	"pewbot/pkg/tgui"
)

func tsNow() string { return time.Now().Format("2006-01-02 15:04:05") }

func tsNowShort() string { return time.Now().Format("15:04:05") }

func hUpdated() tgui.H {
	return tgui.Esc("upd " + tsNowShort())
}

// stateChar returns a single-character state badge for compact tabular UIs.
// A: active (running/listening/exited)
// a: active (other)
// ~: activating
// F: failed
// I: inactive
// ?: unknown
func stateChar(active, sub string) string {
	a := strings.ToLower(strings.TrimSpace(active))
	s := strings.ToLower(strings.TrimSpace(sub))
	switch {
	case a == "active" && (s == "running" || s == "listening" || s == "exited"):
		return "A"
	case a == "active":
		return "a"
	case a == "activating":
		return "~"
	case a == "failed":
		return "F"
	case a == "inactive":
		return "I"
	default:
		return "?"
	}
}

func enabledChar(enabled bool) string {
	if enabled {
		return "E"
	}
	return "-"
}

func truncRunes(s string, w int) string {
	s = strings.TrimSpace(s)
	if w <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= w {
		return s
	}
	if w == 1 {
		return string(rs[:1])
	}
	// Reserve 1 rune for ellipsis.
	return string(rs[:w-1]) + "…"
}

// durClock renders uptime in a fixed-width format:
//   - mm:ss for < 1h
//   - hh:mm for < 100h
//   - >99h for >= 100h
func durClock(d time.Duration) string {
	if d <= 0 {
		return "--:--"
	}
	sec := int64(d.Seconds())
	if sec < 0 {
		sec = 0
	}
	if sec < 3600 {
		m := sec / 60
		s := sec % 60
		return fmt.Sprintf("%02d:%02d", m, s)
	}
	h := sec / 3600
	if h >= 100 {
		return ">99h"
	}
	m := (sec % 3600) / 60
	return fmt.Sprintf("%02d:%02d", h, m)
}

// memShort renders memory as compact, column-friendly text (max ~6 runes).
func memShort(n uint64) string {
	if n == 0 {
		return "--"
	}
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case n >= GB:
		// keep 1 decimal for <10G, otherwise integer
		x := float64(n) / float64(GB)
		if x < 10 {
			return fmt.Sprintf("%.1fG", x)
		}
		return fmt.Sprintf("%dG", int(x+0.5))
	case n >= MB:
		x := float64(n) / float64(MB)
		if x < 10 {
			return fmt.Sprintf("%.1fM", x)
		}
		return fmt.Sprintf("%dM", int(x+0.5))
	case n >= KB:
		return fmt.Sprintf("%dK", (n+KB/2)/KB)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func pageShort(page, size, total int) string {
	if size <= 0 {
		size = 10
	}
	if total < 0 {
		total = 0
	}
	pages := (total + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	start := page*size + 1
	end := (page + 1) * size
	if end > total {
		end = total
	}
	if total == 0 {
		start, end = 0, 0
	}
	return "p" + strconv.Itoa(page+1) + "/" + strconv.Itoa(pages) + " " + strconv.Itoa(start) + "-" + strconv.Itoa(end) + "/" + strconv.Itoa(total)
}

// statusList renders a compact, non-tabular monospace list for multiple units.
//
// Example (each unit = 1 line):
//
//	AE caddy — running — 02:50 — 120M
//	F- sing-box — failed — --:-- — --
func statusList(items []sm.ServiceStatus) string {
	const (
		unitMax = 28
		subMax  = 14
	)
	lines := make([]string, 0, len(items))
	for _, st := range items {
		u := truncRunes(st.Name, unitMax)
		sub := strings.TrimSpace(st.SubState)
		if sub == "" {
			sub = "?"
		}
		sub = truncRunes(sub, subMax)
		badge := stateChar(st.Active, st.SubState) + enabledChar(st.Enabled)
		lines = append(lines, fmt.Sprintf("%s %s — %s — %s — %s", badge, u, sub, durClock(st.Uptime), memShort(st.Memory)))
	}
	return strings.Join(lines, "\n")
}

// statusTreeLines renders a compact tree (no <pre>), so it stays clean on dark themes.
func statusTreeLines(items []sm.ServiceStatus) []string {
	const (
		nameMax  = 32
		stateMax = 16
	)

	lines := make([]string, 0, len(items)*2)

	for i, st := range items {
		isLast := i == len(items)-1
		branch := "├"
		stem := "│  "
		if isLast {
			branch = "└"
			stem = "   "
		}

		name := truncRunes(strings.TrimSpace(st.Name), nameMax)

		dot, state, icon := stateEmoji(st.Active, st.SubState)
		state = truncRunes(state, stateMax)

		bootIcon, boot := bootEmoji(st.Enabled)

		loadTag := ""
		if ls := strings.TrimSpace(st.LoadState); ls != "" && !strings.EqualFold(ls, "loaded") {
			loadTag = "  ⚠️ " + truncRunes(ls, 14)
		}

		lines = append(lines, fmt.Sprintf("%s %s %s  %s %s  %s %s%s", branch, icon, name, dot, state, bootIcon, boot, loadTag))

		details := make([]string, 0, 2)
		if st.Uptime > 0 {
			details = append(details, "⏱ "+durShort(st.Uptime))
		}
		if st.Memory > 0 {
			details = append(details, "💾 "+bytes(st.Memory))
		}
		if len(details) > 0 {
			lines = append(lines, fmt.Sprintf("%s└ %s", stem, strings.Join(details, "  •  ")))
		}
	}

	return lines
}

// statusDetailLines renders a per-service mini tree (no <pre>) similar to:
//
//	nginx 🟢
//	├ Status: active (running)
//	├ Boot: enabled
//	├ Uptime: 3h4m
//	└ Memory: 81.7MB
//
// Design goals:
//   - no table / no monospace block (avoids dark background)
//   - readable, "on point" labels
//   - no leading checkmark icons (✅)
func statusDetailLines(items []sm.ServiceStatus) []string {
	const nameMax = 42

	lines := make([]string, 0, len(items)*6)
	for idx, st := range items {
		name := truncRunes(strings.TrimSpace(st.Name), nameMax)
		dot := activeEmoji(st.Active, st.SubState)
		if strings.TrimSpace(dot) == "" {
			dot = "❔"
		}
		lines = append(lines, fmt.Sprintf("%s %s", name, dot))

		active := strings.TrimSpace(st.Active)
		if active == "" {
			active = "unknown"
		}
		sub := strings.TrimSpace(st.SubState)
		statusVal := active
		if sub != "" {
			statusVal = fmt.Sprintf("%s (%s)", active, sub)
		}

		bootVal := "disabled"
		if st.Enabled {
			bootVal = "enabled"
		}

		details := [][2]string{{"Status", statusVal}, {"Boot", bootVal}}
		if st.Uptime > 0 {
			details = append(details, [2]string{"Uptime", durShort(st.Uptime)})
		}
		if st.Memory > 0 {
			details = append(details, [2]string{"Memory", bytes(st.Memory)})
		}

		for i, kv := range details {
			prefix := "├"
			if i == len(details)-1 {
				prefix = "└"
			}
			lines = append(lines, fmt.Sprintf("%s %s: %s", prefix, kv[0], kv[1]))
		}

		if idx != len(items)-1 {
			lines = append(lines, "")
		}
	}
	return lines
}

// statusCards renders a compact, non-tabular "card list" for multiple units.
// Design goals:
//   - 2 lines per service (header + quick stats)
//   - readable at a glance (icons + short tokens)
//   - no column padding / no headers (so it's not a table)
func statusCards(items []sm.ServiceStatus) string {
	const (
		nameMax  = 32
		stateMax = 16
	)

	lines := make([]string, 0, len(items)*3)

	for i, st := range items {
		name := truncRunes(strings.TrimSpace(st.Name), nameMax)

		dot, state, icon := stateEmoji(st.Active, st.SubState)
		state = truncRunes(state, stateMax)

		bootIcon, boot := bootEmoji(st.Enabled)

		loadTag := ""
		if ls := strings.TrimSpace(st.LoadState); ls != "" && !strings.EqualFold(ls, "loaded") {
			loadTag = "  ⚠️ " + truncRunes(ls, 14)
		}

		lines = append(lines, fmt.Sprintf("%s %s  %s %s  %s %s%s", icon, name, dot, state, bootIcon, boot, loadTag))

		// One concise details line
		details := make([]string, 0, 2)
		if st.Uptime > 0 {
			details = append(details, "⏱ "+durShort(st.Uptime))
		}
		if st.Memory > 0 {
			details = append(details, "💾 "+bytes(st.Memory))
		}
		if len(details) > 0 {
			lines = append(lines, "└ "+strings.Join(details, "  •  "))
		}

		if i != len(items)-1 {
			lines = append(lines, "")
		}
	}

	return strings.Join(lines, "\n")
}

func stateEmoji(active, sub string) (dot, label, icon string) {
	a := strings.ToLower(strings.TrimSpace(active))
	s := strings.TrimSpace(sub)
	if s == "" {
		s = strings.TrimSpace(active)
	}
	if s == "" {
		s = "?"
	}

	switch a {
	case "active":
		dot, icon = "🟢", "✅"
	case "inactive":
		dot, icon = "⚪️", "⏹️"
	case "failed":
		dot, icon = "🔴", "❌"
	case "activating":
		dot, icon = "🟡", "⏳"
	case "deactivating":
		dot, icon = "🟠", "⏳"
	default:
		dot, icon = "🟡", "⚠️"
	}
	return dot, s, icon
}

func bootEmoji(enabled bool) (icon, label string) {
	if enabled {
		return "🔁", "enabled"
	}
	return "⏸️", "disabled"
}

// statusTable renders a compact monospace table for multiple units.
func runeLen(s string) int { return len([]rune(s)) }

func clampInt(x, lo, hi int) int {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

func statusTable(items []sm.ServiceStatus) string {
	// Dynamic UNIT width to reduce truncation while keeping the table compact.
	unitW := 16
	for _, st := range items {
		if l := runeLen(strings.TrimSpace(st.Name)); l > unitW {
			unitW = l
		}
	}
	unitW = clampInt(unitW, 12, 26)

	const (
		subW = 9
		upW  = 5
		memW = 6
	)

	// Header
	lines := []string{fmt.Sprintf("S E %-*s %-*s %*s %*s", unitW, "UNIT", subW, "SUB", upW, "UP", memW, "MEM")}
	for _, st := range items {
		u := truncRunes(st.Name, unitW)
		sub := st.SubState
		if strings.TrimSpace(sub) == "" {
			sub = "?"
		}
		sub = truncRunes(sub, subW)
		lines = append(lines, fmt.Sprintf(
			"%s %s %-*s %-*s %*s %*s",
			stateChar(st.Active, st.SubState),
			enabledChar(st.Enabled),
			unitW, u,
			subW, sub,
			upW, durClock(st.Uptime),
			memW, memShort(st.Memory),
		))
	}
	return strings.Join(lines, "\n")
}

func detailTable(st sm.ServiceStatus) string {
	kw := 9
	active := strings.TrimSpace(st.Active)
	if active == "" {
		active = "unknown"
	}
	sub := strings.TrimSpace(st.SubState)
	if sub == "" {
		sub = "?"
	}
	load := strings.TrimSpace(st.LoadState)
	if load == "" {
		load = "?"
	}
	en := "no"
	if st.Enabled {
		en = "yes"
	}
	rows := [][2]string{
		{"Unit", st.Name},
		{"State", active + "/" + sub},
		{"Load", load},
		{"Enabled", en},
	}
	if !st.ActiveSince.IsZero() {
		rows = append(rows, [2]string{"Since", st.ActiveSince.Format("2006-01-02 15:04:05")})
	}
	if st.Uptime > 0 {
		rows = append(rows, [2]string{"Uptime", durClock(st.Uptime)})
	}
	if st.Memory > 0 {
		rows = append(rows, [2]string{"Memory", memShort(st.Memory)})
	}
	if strings.TrimSpace(st.Description) != "" {
		rows = append(rows, [2]string{"Desc", truncRunes(st.Description, 64)})
	}

	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("%-*s %s", kw, r[0]+":", r[1]))
	}
	return strings.Join(lines, "\n")
}

func activeEmoji(active, sub string) string {
	a := strings.ToLower(strings.TrimSpace(active))
	s := strings.ToLower(strings.TrimSpace(sub))
	switch {
	case a == "active" && (s == "running" || s == "listening" || s == "exited"):
		return "🟢"
	case a == "active":
		return "🟡"
	case a == "activating":
		return "🟠"
	case a == "failed":
		return "🔴"
	case a == "inactive":
		return "⚪️"
	default:
		return "❔"
	}
}

func enabledEmoji(enabled bool) string {
	if enabled {
		return "✅"
	}
	return "🚫"
}

func activeLabel(st sm.ServiceStatus) string {
	a := st.Active
	if strings.TrimSpace(a) == "" {
		a = "unknown"
	}
	sub := st.SubState
	if strings.TrimSpace(sub) == "" {
		sub = "?"
	}
	return fmt.Sprintf("%s/%s", a, sub)
}

// statusLineH renders one line for status list view.
// Example: 🟢 nginx  active/running • ✅ enabled • ⏱ 12m3s • 🧠 10.2MB
func statusLineH(st sm.ServiceStatus) tgui.H {
	parts := []tgui.H{
		tgui.Esc(activeEmoji(st.Active, st.SubState)),
		tgui.B(st.Name),
		tgui.Code(activeLabel(st)),
	}

	if st.Enabled {
		parts = append(parts, tgui.Esc("• "+enabledEmoji(true)+" enabled"))
	} else {
		parts = append(parts, tgui.Esc("• "+enabledEmoji(false)+" disabled"))
	}

	if st.Uptime > 0 {
		parts = append(parts, tgui.Esc("• ⏱ "+durShort(st.Uptime)))
	}
	if st.Memory > 0 {
		parts = append(parts, tgui.Esc("• 🧠 "+bytes(st.Memory)))
	}
	if st.Description != "" {
		// Keep list compact.
		parts = append(parts, tgui.Esc("• "+tgui.TruncRunes(st.Description, 42)))
	}
	return tgui.JoinH(" ", parts...)
}

func statusSummaryLines(st sm.ServiceStatus) []string {
	active := st.Active
	if strings.TrimSpace(active) == "" {
		active = "unknown"
	}
	sub := st.SubState
	if strings.TrimSpace(sub) == "" {
		sub = "?"
	}
	load := st.LoadState
	if strings.TrimSpace(load) == "" {
		load = "?"
	}

	en := "disabled"
	if st.Enabled {
		en = "enabled"
	}

	out := []string{
		"Status" + ": " + active + " / " + sub,
		"Load" + ": " + load,
		"Enabled" + ": " + en,
	}
	if st.Uptime > 0 {
		out = append(out, "Uptime"+": "+durShort(st.Uptime))
	}
	if st.Memory > 0 {
		out = append(out, "Memory"+": "+bytes(st.Memory))
	}
	return out
}
