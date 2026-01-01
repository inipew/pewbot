package core

import (
	"strings"
)

func (m *CommandManager) helpText(path []string) string {
	// If no path: show top-level commands
	if len(path) == 0 {
		lines := []string{"📚 *Commands* (use /help <cmd> ...):"}
		for _, name := range m.root.childNames() {
			n, _ := m.root.child(name)
			desc := ""
			if n.cmd != nil && n.cmd.Description != "" {
				desc = n.cmd.Description
			}
			suffix := ""
			if len(n.children) > 0 {
				suffix = " …"
			}
			if desc != "" {
				lines = append(lines, "- /"+name+suffix+" — "+desc)
			} else {
				lines = append(lines, "- /"+name+suffix)
			}
		}
		return strings.Join(lines, "\n")
	}

	n := m.root.find(path)
	if n == nil {
		// try alias -> show its canonical route
		if len(path) == 1 {
			if leaf, ok := m.alias[path[0]]; ok && leaf != nil && leaf.cmd != nil {
				return m.helpText(splitRoute(leaf.cmd.Route))
			}
		}
		return "command not found. try /help"
	}

	// If node is a container without handler -> list subcommands
	if n.cmd == nil {
		lines := []string{"📚 */" + strings.Join(path, " ") + "* subcommands:"}
		for _, child := range n.childNames() {
			cn, _ := n.child(child)
			desc := ""
			if cn.cmd != nil && cn.cmd.Description != "" {
				desc = cn.cmd.Description
			}
			if desc != "" {
				lines = append(lines, "- /"+path[0]+" "+child+" — "+desc)
			} else {
				lines = append(lines, "- /"+path[0]+" "+child)
			}
		}
		lines = append(lines, "Tip: /help "+strings.Join(path, " ")+" <subcommand>")
		return strings.Join(lines, "\n")
	}

	cmd := n.cmd
	lines := []string{"📌 *" + cmd.Route + "*", cmd.Description}
	if cmd.Usage != "" {
		lines = append(lines, "Usage: `"+cmd.Usage+"`")
	}
	if len(cmd.Aliases) > 0 {
		lines = append(lines, "Aliases: /"+strings.Join(cmd.Aliases, ", /"))
	}
	if len(n.children) > 0 {
		lines = append(lines, "", "Subcommands:")
		for _, child := range n.childNames() {
			cn, _ := n.child(child)
			desc := ""
			if cn.cmd != nil && cn.cmd.Description != "" {
				desc = cn.cmd.Description
			}
			if desc != "" {
				lines = append(lines, "- "+child+" — "+desc)
			} else {
				lines = append(lines, "- "+child)
			}
		}
	}
	return strings.Join(filterEmpty(lines), "\n")
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
