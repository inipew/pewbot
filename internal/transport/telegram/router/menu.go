package router

import (
	"sort"
	"strings"
	"unicode"

	kit "pewbot/internal/transport"
)

// sanitizeTelegramCommand converts an arbitrary route/alias into a Telegram-safe bot command name.
// Telegram command names are restricted to [a-z0-9_]{1,32}.
func sanitizeTelegramCommand(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(s))
	lastUnderscore := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if r == '_' {
			if !lastUnderscore {
				b.WriteRune('_')
				lastUnderscore = true
			}
			continue
		}
		// Common separators become underscores.
		if r == '-' || unicode.IsSpace(r) || r == '/' {
			if b.Len() > 0 && !lastUnderscore {
				b.WriteRune('_')
				lastUnderscore = true
			}
			continue
		}
		// drop anything else
	}

	out := strings.Trim(b.String(), "_")
	if out == "" {
		return ""
	}
	if len(out) > 32 {
		out = strings.TrimRight(out[:32], "_")
	}
	if out == "" {
		return ""
	}
	// Telegram clients generally expect commands to start with a letter.
	if out[0] >= '0' && out[0] <= '9' {
		out = "cmd_" + out
		if len(out) > 32 {
			out = strings.TrimRight(out[:32], "_")
		}
	}
	return out
}

// telegramCommandNameFromRoute builds a Telegram-safe command for a route.
// Examples:
//
//	["systemd","restart"] -> "systemd_restart"
//	["speedtest-stats"]    -> "speedtest_stats"
func telegramCommandNameFromRoute(route []string) (string, bool) {
	if len(route) == 0 {
		return "", false
	}
	base := ""
	if len(route) == 1 {
		base = route[0]
	} else {
		base = strings.Join(route, "_")
	}
	out := sanitizeTelegramCommand(base)
	if out == "" {
		return "", false
	}
	return out, true
}

func buildTelegramMenuCommands(root *cmdNode, leafCmds []Command) []kit.BotCommand {
	// We prioritize top-level commands/groups first, then leaf shortcuts.
	type entry struct {
		cmd  string
		desc string
		prio int
	}
	byCmd := map[string]entry{}
	add := func(cmd string, desc string, prio int) {
		cmd = sanitizeTelegramCommand(cmd)
		if cmd == "" {
			return
		}
		if len(cmd) > 32 {
			cmd = strings.TrimRight(cmd[:32], "_")
			if cmd == "" {
				return
			}
		}
		desc = strings.TrimSpace(desc)
		desc = strings.ReplaceAll(desc, "\n", " ")
		if desc == "" {
			desc = cmd
		}
		if len(desc) > 256 {
			desc = desc[:256]
		}

		if cur, ok := byCmd[cmd]; ok {
			// Keep the better priority; if equal, keep the shorter (more compact) description.
			if prio < cur.prio || (prio == cur.prio && len(desc) < len(cur.desc)) {
				byCmd[cmd] = entry{cmd: cmd, desc: desc, prio: prio}
			}
			return
		}
		byCmd[cmd] = entry{cmd: cmd, desc: desc, prio: prio}
	}

	// 1) Top-level command list (best for autocomplete in Telegram).
	if root != nil {
		for _, name := range root.childNames() {
			n, _ := root.child(name)
			if n == nil {
				continue
			}
			desc := summarizeNodeDesc(n)
			if nodeIsOwnerOnly(n) {
				desc = "🔒 " + desc
			}
			add(name, desc, 0)
		}
	}

	// 2) Leaf shortcuts (multi-token commands become /a_b).
	for _, c := range leafCmds {
		route := splitRoute(c.Route)
		if len(route) == 0 {
			continue
		}
		// Single-token routes are already covered by the top-level list.
		if len(route) == 1 {
			continue
		}
		menu, ok := telegramCommandNameFromRoute(route)
		if !ok {
			continue
		}

		desc := strings.TrimSpace(c.Description)
		if desc == "" {
			desc = strings.Join(route, " ")
		}
		if c.Access == AccessOwnerOnly {
			desc = "🔒 " + desc
		}
		add(menu, desc, 1)
	}

	// Finalize sorted list.
	entries := make([]entry, 0, len(byCmd))
	for _, e := range byCmd {
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].prio != entries[j].prio {
			return entries[i].prio < entries[j].prio
		}
		return entries[i].cmd < entries[j].cmd
	})

	out := make([]kit.BotCommand, 0, len(entries))
	for _, e := range entries {
		out = append(out, kit.BotCommand{Command: e.cmd, Description: e.desc})
		if len(out) >= 100 {
			break
		}
	}
	return out
}
