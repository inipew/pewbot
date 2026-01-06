package systemd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	sm "pewbot/pkg/systemdmanager"
)

func normalizeUnits(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, u := range in {
		u = strings.TrimSpace(strings.TrimSuffix(u, ".service"))
		if u == "" {
			continue
		}
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	sort.Strings(out)
	return out
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func plainOp(action, unit string, err error) string {
	if err != nil {
		return fmt.Sprintf("%s %s: error: %v", action, unit, err)
	}
	return fmt.Sprintf("%s %s: ok", action, unit)
}

func formatStatusLine(st sm.ServiceStatus) string {
	active := st.Active
	if active == "" {
		active = "unknown"
	}
	en := "disabled"
	if st.Enabled {
		en = "enabled"
	}
	up := ""
	if st.Uptime > 0 {
		up = ", up " + durShort(st.Uptime)
	}
	mem := ""
	if st.Memory > 0 {
		mem = ", mem " + bytes(st.Memory)
	}
	return fmt.Sprintf("%s: %s/%s (%s)%s%s", st.Name, active, st.SubState, en, up, mem)
}

func formatStatusDetail(st sm.ServiceStatus) string {
	active := st.Active
	if active == "" {
		active = "unknown"
	}
	en := "disabled"
	if st.Enabled {
		en = "enabled"
	}
	lines := []string{
		"unit: " + st.Name,
		"active: " + active,
		"sub: " + st.SubState,
		"load: " + st.LoadState,
		"enabled: " + en,
	}
	if st.Description != "" {
		lines = append(lines, "desc: "+st.Description)
	}
	if st.Uptime > 0 {
		lines = append(lines, "uptime: "+durShort(st.Uptime))
	}
	if st.Memory > 0 {
		lines = append(lines, "memory: "+bytes(st.Memory))
	}
	return strings.Join(lines, "\n")
}

func durShort(d time.Duration) string {
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
		return fmt.Sprintf("%dB", n)
	}
}

func fmt2(n, div uint64, unit string) string {
	x := float64(n) / float64(div)
	ix := int(x * 10) // 1 decimal
	return fmt.Sprintf("%d.%d%s", ix/10, ix%10, unit)
}
