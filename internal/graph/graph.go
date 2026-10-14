// Package graph builds the module dependency graph of a Terraform/OpenTofu
// monorepo and identifies which directories are root modules.
package graph

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/siffletdata/atlantis-config-generator/internal/tfparse"
)

// Kind distinguishes directories inside the monorepo from module sources that
// point elsewhere (the public registry, git, an HTTP archive).
type Kind int

const (
	// Local is a directory inside the repository.
	Local Kind = iota
	// Remote is a module source we record for completeness but never follow.
	Remote
)

func (k Kind) String() string {
	if k == Remote {
		return "remote"
	}
	return "local"
}

// Node is one vertex of the dependency graph.
type Node struct {
	// Key identifies the node: a repo-relative directory for Local nodes, the
	// raw source string for Remote ones.
	Key  string
	Kind Kind
	// HasProvider reports whether the directory declares a provider block.
	// Always false for Remote nodes and for shared source directories.
	HasProvider bool
	// SharedSource marks a directory whose Terraform files are symlinked into
	// other directories rather than used where they live. Such a directory is
	// a dependency target only: it is never a root module, and its own module
	// sources are meaningless in isolation.
	SharedSource bool
	// Deps are the keys this node depends on, sorted.
	Deps []string
	// ReferencedBy are the keys that depend on this node, sorted. A local
	// directory that nothing references is a candidate root module.
	ReferencedBy []string
	// SymlinkDeps is the subset of Deps that came from a symlinked file rather
	// than a module block. Recorded for the debug output.
	SymlinkDeps map[string]bool
	// Files are repo-relative paths outside this directory that its
	// configuration reads with file(), templatefile() and the like, sorted.
	// An entry may be a glob when it comes from fileset(). Files inside the
	// directory are left out: they are already part of the directory.
	Files []string
}

// Graph is the whole dependency graph, keyed by Node.Key.
type Graph struct {
	Nodes map[string]*Node
	// Warnings collects every non-fatal problem seen while building.
	Warnings []string
}

// Options configures a graph build.
type Options struct {
	// Root is the repository root to scan.
	Root string
	// Exclude holds doublestar patterns, relative to Root. A directory that
	// matches is not scanned and never becomes a node.
	Exclude Matcher
}

// Matcher decides whether a repo-relative path is excluded.
type Matcher interface {
	Match(relPath string) bool
}

// alwaysSkip are directories that never contain first-party configuration.
// .terraform in particular holds vendored copies of registry modules whose own
// relative sources would otherwise pollute the graph.
var alwaysSkip = map[string]bool{
	".terraform": true,
	".git":       true,
}

// Build scans the repository and returns the dependency graph.
//
// Directories containing at least one Terraform file become nodes. Local
// module sources and symlinked files create edges; directories reachable only
// as a dependency are pulled in even if they sit outside the scanned subtree.
func Build(opts Options) (*Graph, error) {
	g := &Graph{Nodes: map[string]*Node{}}
	warn := func(format string, args ...any) {
		g.Warnings = append(g.Warnings, fmt.Sprintf(format, args...))
	}

	// Pass one: discover every directory holding Terraform files.
	var discovered []string
	err := filepath.WalkDir(opts.Root, func(abs string, entry fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is reported but does not abort the scan;
			// a git submodule that was never checked out looks like this.
			warn("%s: %v", abs, err)
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(opts.Root, abs)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if alwaysSkip[entry.Name()] {
			return fs.SkipDir
		}
		if opts.Exclude != nil && opts.Exclude.Match(rel) {
			return fs.SkipDir
		}
		if hasTerraformFiles(abs) {
			discovered = append(discovered, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(discovered)

	// Pass two: find the directories whose Terraform files are symlinked into
	// other directories. Terraform resolves a relative module source against
	// the directory it runs in, so a shared file like templates/base/
	// main.source.tf has module paths written for its consumers, not for
	// where it sits. Resolving them in place would invent bogus edges, so
	// these directories are recorded as dependency targets only.
	shared := map[string]bool{}
	for _, dir := range discovered {
		for _, target := range tfparse.SymlinkTargets(opts.Root, dir, warn) {
			if targetDir := path.Dir(target); targetDir != dir && targetDir != "." {
				shared[targetDir] = true
			}
		}
	}

	// Pass three: parse each discovered directory, following local dependencies
	// into directories the walk may not have visited.
	queue := slices.Clone(discovered)
	parsed := map[string]bool{}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if parsed[dir] {
			continue
		}
		parsed[dir] = true

		if shared[dir] {
			// A dependency target only. Its content reaches the graph through
			// the directories that symlink it, resolved in their context.
			g.node(dir, Local).SharedSource = true
			continue
		}

		result, err := tfparse.ParseDir(opts.Root, dir, warn)
		if err != nil {
			warn("%v", err)
			continue
		}

		node := g.node(dir, Local)
		node.HasProvider = result.HasProvider

		for _, label := range result.ModuleLabels() {
			source := result.Modules[label]
			target, local := resolveSource(dir, source)
			if !local {
				g.addEdge(node, g.node(source, Remote), false)
				continue
			}
			if !dirExists(opts.Root, target) {
				warn("%s: module %q points at %s, which does not exist", dir, label, target)
				continue
			}
			g.addEdge(node, g.node(target, Local), false)
			queue = append(queue, target)
		}

		node.Files = externalFiles(opts.Root, dir, result.FileRefs, warn)

		// A symlinked file makes this directory depend on the directory the
		// link points into, since that is where a diff records the change.
		for _, target := range result.Symlinks {
			targetDir := path.Dir(target)
			if targetDir == dir || targetDir == "." {
				continue
			}
			g.addEdge(node, g.node(targetDir, Local), true)
			queue = append(queue, targetDir)
		}
	}

	g.sortEdges()
	return g, nil
}

// resolveSource classifies a module source and, when it is a local path,
// resolves it against the calling directory.
//
// Classification happens before any cleaning because "//" means two different
// things: a harmless double separator in a local path, and the subdirectory
// separator in a registry address such as "org/name/aws//modules/sub".
func resolveSource(fromDir, source string) (target string, local bool) {
	if !strings.HasPrefix(source, "./") && !strings.HasPrefix(source, "../") {
		return "", false
	}
	// path.Join cleans the result, collapsing the stray double separators that
	// appear in sources like "../../modules//database".
	return path.Join(fromDir, source), true
}

// externalFiles resolves file references against dir and keeps those that
// point outside it but still inside the repository: a change to one of those
// files changes what the module does, yet no pattern covering the module
// directory would notice.
func externalFiles(root, dir string, refs []string, warn func(string, ...any)) []string {
	var files []string
	for _, ref := range refs {
		target := path.Join(dir, ref)
		if target == dir || strings.HasPrefix(target, dir+"/") {
			continue
		}
		if target == ".." || strings.HasPrefix(target, "../") {
			// Outside the repository; nothing in a diff can match it.
			continue
		}
		if !hasGlobMeta(target) {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(target))); err != nil {
				warn("%s: reads %s, which does not exist", dir, target)
				continue
			}
		}
		files = append(files, target)
	}
	return files
}

func hasGlobMeta(p string) bool {
	return strings.ContainsAny(p, "*?[")
}

// Files returns the external files read by key and by every local node it
// depends on, sorted and deduplicated.
func (g *Graph) Files(key string) []string {
	var out []string
	for _, k := range append([]string{key}, g.Dependencies(key)...) {
		if node, ok := g.Nodes[k]; ok {
			out = append(out, node.Files...)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// node returns the node for key, creating it if needed.
func (g *Graph) node(key string, kind Kind) *Node {
	if existing, ok := g.Nodes[key]; ok {
		return existing
	}
	n := &Node{Key: key, Kind: kind, SymlinkDeps: map[string]bool{}}
	g.Nodes[key] = n
	return n
}

func (g *Graph) addEdge(from, to *Node, viaSymlink bool) {
	if from == to {
		return
	}
	if !slices.Contains(from.Deps, to.Key) {
		from.Deps = append(from.Deps, to.Key)
	}
	if !slices.Contains(to.ReferencedBy, from.Key) {
		to.ReferencedBy = append(to.ReferencedBy, from.Key)
	}
	if viaSymlink {
		from.SymlinkDeps[to.Key] = true
	}
}

func (g *Graph) sortEdges() {
	for _, n := range g.Nodes {
		sort.Strings(n.Deps)
		sort.Strings(n.ReferencedBy)
	}
}

// Roots returns the local directories that nothing else references and that
// declare at least one provider block, sorted. These are the candidate root
// modules; callers still apply their own include and exclude filters.
func (g *Graph) Roots() []*Node {
	var roots []*Node
	for _, n := range g.Nodes {
		if n.Kind != Local || !n.HasProvider || n.SharedSource || len(n.ReferencedBy) > 0 {
			continue
		}
		roots = append(roots, n)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Key < roots[j].Key })
	return roots
}

// Dependencies returns every local node reachable from key, excluding key
// itself, sorted. Remote nodes are omitted: nothing in the repository changes
// when a registry module does, so they cannot trigger a plan.
//
// Cycles are tolerated. Terraform rejects them, but a half-finished refactor
// can produce one and we would rather report it than hang.
func (g *Graph) Dependencies(key string) []string {
	seen := map[string]bool{key: true}
	var out []string

	stack := []string{key}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		node, ok := g.Nodes[current]
		if !ok {
			continue
		}
		for _, dep := range node.Deps {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			depNode, ok := g.Nodes[dep]
			if !ok || depNode.Kind != Local {
				continue
			}
			out = append(out, dep)
			stack = append(stack, dep)
		}
	}

	sort.Strings(out)
	return out
}

// Cycles returns the keys involved in at least one dependency cycle, sorted.
func (g *Graph) Cycles() []string {
	const (
		white = 0 // unvisited
		grey  = 1 // on the current path
		black = 2 // fully explored
	)
	state := map[string]int{}
	inCycle := map[string]bool{}

	var visit func(key string, path []string)
	visit = func(key string, stack []string) {
		state[key] = grey
		node := g.Nodes[key]
		if node != nil {
			for _, dep := range node.Deps {
				switch state[dep] {
				case white:
					visit(dep, append(stack, key))
				case grey:
					// Everything from dep onwards on the current path is part
					// of the cycle.
					inCycle[dep] = true
					for i := len(stack) - 1; i >= 0; i-- {
						inCycle[stack[i]] = true
						if stack[i] == dep {
							break
						}
					}
					inCycle[key] = true
				}
			}
		}
		state[key] = black
	}

	for _, key := range sortedKeys(g.Nodes) {
		if state[key] == white {
			visit(key, nil)
		}
	}

	var out []string
	for key := range inCycle {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func hasTerraformFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tf.json") ||
			strings.HasSuffix(name, ".tofu") || strings.HasSuffix(name, ".tofu.json") {
			return true
		}
	}
	return false
}

func dirExists(root, rel string) bool {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil && info.IsDir()
}

func sortedKeys(m map[string]*Node) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
