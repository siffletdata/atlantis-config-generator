package graph

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// WriteTree renders the graph as an indented tree, one section per root module,
// followed by any nodes that were never reached from a root.
//
// A node already expanded elsewhere in the same branch is marked "(seen)" so
// that shared and cyclic dependencies terminate.
func (g *Graph) WriteTree(w io.Writer) error {
	roots := g.Roots()

	printed := map[string]bool{}
	var walk func(key string, depth int, onPath map[string]bool) error
	walk = func(key string, depth int, onPath map[string]bool) error {
		node := g.Nodes[key]
		indent := strings.Repeat("  ", depth)

		label := key
		var tags []string
		if node == nil || node.Kind == Remote {
			tags = append(tags, "remote")
		} else if node.SharedSource {
			tags = append(tags, "shared source")
		}
		if onPath[key] {
			tags = append(tags, "cycle")
		}
		suffix := ""
		if len(tags) > 0 {
			suffix = " [" + strings.Join(tags, ", ") + "]"
		}

		if _, err := fmt.Fprintf(w, "%s%s%s\n", indent, label, suffix); err != nil {
			return err
		}
		printed[key] = true

		if node == nil || node.Kind == Remote || onPath[key] {
			return nil
		}

		for _, file := range node.Files {
			if _, err := fmt.Fprintf(w, "%s  %s (file)\n", indent, file); err != nil {
				return err
			}
		}

		onPath[key] = true
		defer delete(onPath, key)

		for _, dep := range node.Deps {
			via := ""
			if node.SymlinkDeps[dep] {
				via = " (symlink)"
			}
			if via != "" {
				if _, err := fmt.Fprintf(w, "%s  %s%s\n", indent, dep, via); err != nil {
					return err
				}
				printed[dep] = true
				continue
			}
			if err := walk(dep, depth+1, onPath); err != nil {
				return err
			}
		}
		return nil
	}

	for _, root := range roots {
		if err := walk(root.Key, 0, map[string]bool{}); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	var orphans []string
	for key, node := range g.Nodes {
		if node.Kind == Local && !printed[key] {
			orphans = append(orphans, key)
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		if _, err := fmt.Fprintln(w, "unreferenced (not reachable from any root module):"); err != nil {
			return err
		}
		for _, key := range orphans {
			reason := "no provider block"
			if len(g.Nodes[key].ReferencedBy) > 0 {
				reason = fmt.Sprintf("library module, used by %d", len(g.Nodes[key].ReferencedBy))
			}
			if _, err := fmt.Fprintf(w, "  %s (%s)\n", key, reason); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteDot renders the graph in Graphviz DOT format. Root modules are boxed,
// remote sources are dashed, and symlink edges are drawn dotted.
func (g *Graph) WriteDot(w io.Writer) error {
	if _, err := fmt.Fprintln(w, "digraph terraform {"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "  rankdir=LR;"); err != nil {
		return err
	}

	isRoot := map[string]bool{}
	for _, root := range g.Roots() {
		isRoot[root.Key] = true
	}

	for _, key := range sortedKeys(g.Nodes) {
		node := g.Nodes[key]
		switch {
		case node.Kind == Remote:
			fmt.Fprintf(w, "  %q [shape=note, style=dashed];\n", key)
		case isRoot[key]:
			fmt.Fprintf(w, "  %q [shape=box, style=bold];\n", key)
		default:
			fmt.Fprintf(w, "  %q [shape=ellipse];\n", key)
		}
	}

	for _, key := range sortedKeys(g.Nodes) {
		node := g.Nodes[key]
		for _, dep := range node.Deps {
			style := ""
			if node.SymlinkDeps[dep] {
				style = " [style=dotted, label=\"symlink\"]"
			}
			if _, err := fmt.Fprintf(w, "  %q -> %q%s;\n", key, dep, style); err != nil {
				return err
			}
		}
	}

	_, err := fmt.Fprintln(w, "}")
	return err
}
