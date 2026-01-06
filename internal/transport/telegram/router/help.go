package router

import (
	"fmt"
	"html"
	"sort"
	"strings"
)

// helpText renders Telegram-friendly help in HTML parse mode.
// It is safe to pass directly to Telegram with ParseMode="HTML".
func (m *CommandManager) helpText(path []string) string {
	m.mu.RLock()
	root := m.root
	alias := m.alias
	m.mu.RUnlock()

	// Walk to requested node.
	cur := root
	full := make([]string, 0, len(path))
	for _, p := range path {
		n, ok := cur.child(p)
		if !ok {
			// maybe it's an alias
			if leaf, ok2 := alias[p]; ok2 && leaf != nil && leaf.cmd != nil {
				cur = leaf
				full = splitRoute(leaf.cmd.Route)
				break
			}
			return m.helpUnknownHTML()
		}
		cur = n
		full = append(full, p)
	}

	if len(path) == 0 {
		return m.helpTopHTML(root)
	}
	return m.helpNodeHTML(cur, full)
}

func (m *CommandManager) helpUnknownHTML() string {
	return strings.Join([]string{
		"❓ <b>Perintah tidak dikenal</b>",
		"Coba ketik <code>/help</code> untuk melihat daftar perintah.",
	}, "\n")
}

func (m *CommandManager) helpTopHTML(root *cmdNode) string {
	// Build a stable list of top-level entries.
	names := root.childNames()
	rows := make([]topRow, 0, len(names))
	for _, name := range names {
		n, _ := root.child(name)
		if n == nil {
			continue
		}
		desc := summarizeNodeDesc(n)
		lock := nodeIsOwnerOnly(n)
		rows = append(rows, topRow{name: name, desc: desc, lock: lock})
	}
	// Group: owner-only at the bottom, but keep alphabetical within groups.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].lock != rows[j].lock {
			return !rows[i].lock && rows[j].lock
		}
		return rows[i].name < rows[j].name
	})

	lines := []string{
		"📚 <b>Daftar Perintah</b>",
		"Ketik <code>/help &lt;cmd&gt;</code> untuk detail.",
		"",
	}

	for _, r := range rows {
		cmd := "/" + html.EscapeString(r.name)
		suffix := ""
		if r.desc != "" {
			suffix = " — " + html.EscapeString(r.desc)
		}
		prefix := "• "
		if r.lock {
			prefix = "• 🔒 "
		}
		lines = append(lines, prefix+"<code>"+cmd+"</code>"+suffix)
	}

	lines = append(lines,
		"",
		"Tip: di Telegram, ketik <code>/</code> lalu mulai mengetik untuk melihat saran (autocomplete).",
	)
	return strings.Join(filterEmpty(lines), "\n")
}

type topRow struct {
	name string
	desc string
	lock bool
}

func (m *CommandManager) helpNodeHTML(cur *cmdNode, full []string) string {
	// Title.
	title := "/" + strings.Join(full, " ")
	lines := []string{fmt.Sprintf("📚 <b>Bantuan</b> <code>%s</code>", html.EscapeString(title))}

	// Command details (if this node is a real command).
	if cur != nil && cur.cmd != nil {
		c := cur.cmd
		if strings.TrimSpace(c.Description) != "" {
			lines = append(lines, html.EscapeString(strings.TrimSpace(c.Description)))
		}
		if c.Access == AccessOwnerOnly {
			lines = append(lines, "🔒 <i>Khusus owner</i>")
		}

		// Usage.
		if strings.TrimSpace(c.Usage) != "" {
			lines = append(lines, "", "<b>Usage</b>")
			lines = append(lines, "<code>"+html.EscapeString(strings.TrimSpace(c.Usage))+"</code>")
		}

		// Shortcuts (aliases + Telegram-safe menu command).
		short := buildShortcuts(*c)
		if len(short) > 0 {
			lines = append(lines, "", "<b>Shortcut</b>")
			for _, s := range short {
				lines = append(lines, "• <code>/"+html.EscapeString(s)+"</code>")
			}
		}
	} else {
		lines = append(lines, "Grup perintah (punya subcommand).")
		if nodeIsOwnerOnly(cur) {
			lines = append(lines, "🔒 <i>Khusus owner</i>")
		}
	}

	// Subcommands.
	if cur != nil && len(cur.children) > 0 {
		lines = append(lines, "", "<b>Subcommand</b>")
		for _, name := range cur.childNames() {
			n, _ := cur.child(name)
			if n == nil {
				continue
			}
			path := append(append([]string(nil), full...), name)
			cmd := "/" + strings.Join(path, " ")
			desc := summarizeNodeDesc(n)
			suffix := ""
			if desc != "" {
				suffix = " — " + html.EscapeString(desc)
			}
			prefix := "• "
			if nodeIsOwnerOnly(n) {
				prefix = "• 🔒 "
			}
			lines = append(lines, prefix+"<code>"+html.EscapeString(cmd)+"</code>"+suffix)
		}
	}

	return strings.Join(filterEmpty(lines), "\n")
}

func summarizeNodeDesc(n *cmdNode) string {
	if n == nil {
		return ""
	}
	if n.cmd != nil {
		if d := strings.TrimSpace(n.cmd.Description); d != "" {
			return d
		}
		// If leaf command has no description, fall through.
	}
	if len(n.children) == 0 {
		return ""
	}

	// For groups, show the first few subcommands as a hint.
	kids := n.childNames()
	if len(kids) == 0 {
		return ""
	}
	max := 3
	if len(kids) < max {
		max = len(kids)
	}
	s := strings.Join(kids[:max], ", ")
	if len(kids) > max {
		s += ", …"
	}
	return "subcommand: " + s
}

func nodeIsOwnerOnly(n *cmdNode) bool {
	if n == nil {
		return false
	}
	// If this node is a leaf command, use its Access.
	if n.cmd != nil {
		return n.cmd.Access == AccessOwnerOnly
	}
	// Otherwise, if all descendants are owner-only, treat the group as owner-only.
	ownerOnly := true
	walk := func(x *cmdNode) {}
	walk = func(x *cmdNode) {
		if x == nil || !ownerOnly {
			return
		}
		if x.cmd != nil && x.cmd.Access == AccessEveryone {
			ownerOnly = false
			return
		}
		for _, ch := range x.children {
			walk(ch)
			if !ownerOnly {
				return
			}
		}
	}
	walk(n)
	return ownerOnly
}

func buildShortcuts(c Command) []string {
	out := make([]string, 0, 8)
	seen := map[string]bool{}

	// Telegram menu command: route joined with underscore & sanitized.
	if menu, ok := telegramCommandNameFromRoute(splitRoute(c.Route)); ok {
		if !seen[menu] {
			out = append(out, menu)
			seen[menu] = true
		}
	}

	for _, a := range c.Aliases {
		a = strings.TrimSpace(a)
		if a == "" || strings.Contains(a, " ") {
			continue
		}
		if !seen[a] {
			out = append(out, a)
			seen[a] = true
		}
		// Also include a Telegram-safe variant (e.g. speedtest-stats -> speedtest_stats).
		if sa := sanitizeTelegramCommand(a); sa != "" && !seen[sa] {
			out = append(out, sa)
			seen[sa] = true
		}
	}

	sort.Strings(out)
	return out
}

func filterEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}
