package core

import (
	"sort"
	"strings"
)

type cmdNode struct {
	name     string
	cmd      *Command
	children map[string]*cmdNode
}

func newRoot() *cmdNode {
	return &cmdNode{name: "", children: map[string]*cmdNode{}}
}

func splitRoute(route string) []string {
	route = strings.TrimSpace(route)
	if route == "" {
		return nil
	}
	return strings.Fields(route)
}

func (r *cmdNode) add(route []string, c Command) {
	cur := r
	for _, tok := range route {
		if cur.children == nil {
			cur.children = map[string]*cmdNode{}
		}
		n, ok := cur.children[tok]
		if !ok {
			n = &cmdNode{name: tok, children: map[string]*cmdNode{}}
			cur.children[tok] = n
		}
		cur = n
	}
	cur.cmd = &c
}

func (r *cmdNode) find(path []string) *cmdNode {
	cur := r
	for _, tok := range path {
		n, ok := cur.children[tok]
		if !ok {
			return nil
		}
		cur = n
	}
	return cur
}

func (r *cmdNode) child(name string) (*cmdNode, bool) {
	n, ok := r.children[name]
	return n, ok
}

func (r *cmdNode) childNames() []string {
	out := make([]string, 0, len(r.children))
	for k := range r.children {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
