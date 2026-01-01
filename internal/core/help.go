package core

import (
	"strings"
)

func (m *CommandManager) helpText(path []string) string {
	m.mu.RLock()
	root := m.root
	alias := m.alias
	m.mu.RUnlock()

	// Walk to requested node
	cur := root
	full := make([]string, 0, len(path))
	for _, p := range path {
		n, ok := cur.child(p)
		if !ok {
			// maybe it's an alias
			if leaf, ok2 := alias[p]; ok2 && leaf != nil {
				cur = leaf
				full = splitRoute(leaf.cmd.Route)
				break
			}
			return "unknown command. try /help"
		}
		cur = n
		full = append(full, p)
	}

	// Top-level list
	if len(path) == 0 {
		lines := []string{"📚 *Commands* (use /help <cmd> ...):"}
		for _, name := range root.childNames() {
			n, _ := root.child(name)
			if n == nil {
				continue
			}
			desc := ""
			if n.cmd != nil && n.cmd.Description != "" {
				desc = n.cmd.Description
			} else if len(n.children) > 0 {
				desc = "subcommands"
			}
			lines = append(lines, "- /"+name+padDesc(desc))
		}
		lines = append(lines, "", "Tips: type /<cmd> without args to see subcommands.")
		return strings.Join(filterEmpty(lines), "\n")
	}

	// Detailed view
	title := "/" + strings.Join(full, " ")
	lines := []string{"📚 *Help* " + title}

	if cur.cmd != nil {
		if cur.cmd.Description != "" {
			lines = append(lines, cur.cmd.Description)
		}
		if cur.cmd.Usage != "" {
			lines = append(lines, "Usage: "+cur.cmd.Usage)
		}
		if len(cur.cmd.Aliases) > 0 {
			lines = append(lines, "Aliases: /"+strings.Join(cur.cmd.Aliases, ", /"))
		}
	} else {
		lines = append(lines, "This is a command group.")
	}

	// Subcommands
	if len(cur.children) > 0 {
		lines = append(lines, "", "*Subcommands:*")
		for _, name := range cur.childNames() {
			n, _ := cur.child(name)
			if n == nil {
				continue
			}
			desc := ""
			if n.cmd != nil && n.cmd.Description != "" {
				desc = n.cmd.Description
			} else if len(n.children) > 0 {
				desc = "subcommands"
			}
			lines = append(lines, "- /"+strings.Join(append(full, name), " ")+padDesc(desc))
		}
	} else if cur.cmd == nil {
		lines = append(lines, "", "(no subcommands)")
	}

	return strings.Join(filterEmpty(lines), "\n")
}

func padDesc(desc string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return ""
	}
	return " — " + desc
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
