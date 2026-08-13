// Package render turns a dependency graph into an Atlantis repo-level
// configuration file.
package render

import (
	"bytes"
	"fmt"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/siffletdata/atlantis-config-generator/internal/config"
	"github.com/siffletdata/atlantis-config-generator/internal/graph"
)

// Project is one entry of the generated projects list. Field order here is the
// field order in the output.
type Project struct {
	Name                string   `yaml:"name,omitempty"`
	Dir                 string   `yaml:"dir"`
	Workspace           string   `yaml:"workspace,omitempty"`
	TerraformVersion    string   `yaml:"terraform_version,omitempty"`
	ExecutionOrderGroup *int     `yaml:"execution_order_group,omitempty"`
	Autoplan            Autoplan `yaml:"autoplan"`
	Workflow            string   `yaml:"workflow,omitempty"`
}

// Autoplan is a project's autoplan block.
type Autoplan struct {
	WhenModified []string `yaml:"when_modified"`
	Enabled      *bool    `yaml:"enabled,omitempty"`
}

// Options configures a render.
type Options struct {
	Config *config.Config
	Graph  *graph.Graph
	// RepoRoot is used to resolve terraform_version_file.
	RepoRoot string
}

// Result is a rendered configuration plus the projects that went into it.
type Result struct {
	YAML     []byte
	Projects []Project
	// Warnings describe directories that look like they were meant to be
	// projects but were not emitted as one.
	Warnings []string
}

// Render builds the Atlantis configuration for every root module that passes
// the configured include and exclude filters.
func Render(opts Options) (*Result, error) {
	cfg := opts.Config

	terraformVersion, err := cfg.ResolveTerraformVersion(opts.RepoRoot)
	if err != nil {
		return nil, err
	}

	include := cfg.ProjectInclude()
	exclude := cfg.ProjectExclude()

	var projects []Project
	for _, root := range opts.Graph.Roots() {
		if len(include) > 0 && !include.Match(root.Key) {
			continue
		}
		if exclude.Match(root.Key) {
			continue
		}
		projects = append(projects, buildProject(cfg, opts.Graph, root.Key, terraformVersion))
	}

	sort.Slice(projects, func(i, j int) bool { return projects[i].Dir < projects[j].Dir })

	warnings := demotedProjects(opts.Graph, include, exclude)

	document, err := buildDocument(cfg, projects)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	if header := cfg.HeaderComment; strings.TrimSpace(header) != "" {
		for line := range strings.SplitSeq(strings.TrimRight(header, "\n"), "\n") {
			if line == "" {
				fmt.Fprintln(&out, "#")
			} else {
				fmt.Fprintf(&out, "# %s\n", line)
			}
		}
	}

	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("encode yaml: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encode yaml: %w", err)
	}

	return &Result{YAML: out.Bytes(), Projects: projects, Warnings: warnings}, nil
}

// demotedProjects reports directories that pass the project filters and declare
// providers, but were classified as library modules because something calls
// them as a module.
//
// The filters are what keep this quiet. A library module that declares a
// provider is common and unremarkable, so warning about every one of them
// would be noise; a directory sitting in the part of the tree reserved for root
// modules that nonetheless gets called as a module is not. That is the shape a
// misconfigured shared source produces: its module paths are resolved in the
// wrong frame of reference, the invented edge demotes a real root module, and
// the project would otherwise vanish from the output with nothing to show
// for it.
func demotedProjects(g *graph.Graph, include, exclude config.Globs) []string {
	var warnings []string
	for _, key := range sortedNodeKeys(g) {
		node := g.Nodes[key]
		switch {
		case node.Kind != graph.Local, node.SharedSource,
			!node.HasProvider, len(node.ReferencedBy) == 0:
			continue
		case len(include) > 0 && !include.Match(key), exclude.Match(key):
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s declares providers and matches the project filters, but is called as a module by %s, "+
				"so it is treated as a library and will not become a project",
			key, strings.Join(node.ReferencedBy, ", ")))
	}
	return warnings
}

func sortedNodeKeys(g *graph.Graph) []string {
	keys := make([]string, 0, len(g.Nodes))
	for key := range g.Nodes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// buildProject assembles one project entry, applying the first matching rule.
func buildProject(cfg *config.Config, g *graph.Graph, dir, terraformVersion string) Project {
	display := cfg.Projects.PathPrefix + dir

	project := Project{
		Name:             display,
		Dir:              display,
		TerraformVersion: terraformVersion,
		Autoplan:         Autoplan{WhenModified: whenModified(cfg, g, dir)},
	}

	if rule := cfg.MatchRule(dir); rule != nil {
		project.Workflow = rule.Workflow
		project.Workspace = rule.Workspace
		project.ExecutionOrderGroup = rule.ExecutionOrderGroup
		project.Autoplan.Enabled = rule.AutoplanEnabled
		if rule.TerraformVersion != "" {
			project.TerraformVersion = rule.TerraformVersion
		}
		project.Autoplan.WhenModified = append(project.Autoplan.WhenModified, rule.ExtraWhenModified...)
	}

	return project
}

// whenModified builds the autoplan patterns: the self patterns first, then one
// group per transitive dependency, ordered by dependency path.
//
// Atlantis prepends the project directory to each pattern and matches with
// Docker's .dockerignore syntax, so "../../modules/foo/**" resolves to
// "modules/foo/**" against the repository root.
func whenModified(cfg *config.Config, g *graph.Graph, dir string) []string {
	patterns := make([]string, 0, len(cfg.Projects.WhenModified.Self))
	patterns = append(patterns, cfg.Projects.WhenModified.Self...)

	seen := map[string]bool{}
	for _, pattern := range patterns {
		seen[pattern] = true
	}

	for _, dep := range g.Dependencies(dir) {
		rel, err := relPath(dir, dep)
		if err != nil {
			continue
		}
		for _, pattern := range cfg.Projects.WhenModified.Dependency {
			joined := path.Join(rel, pattern)
			// path.Join drops a trailing "**" separator but keeps the segment;
			// guard against a pattern that cleans away to nothing.
			if joined == "" || joined == "." || seen[joined] {
				continue
			}
			seen[joined] = true
			patterns = append(patterns, joined)
		}
	}

	return patterns
}

// relPath returns to relative to from, using forward slashes. Both are
// repository-relative directories.
func relPath(from, to string) (string, error) {
	fromParts := strings.Split(path.Clean(from), "/")
	toParts := strings.Split(path.Clean(to), "/")

	common := 0
	for common < len(fromParts) && common < len(toParts) && fromParts[common] == toParts[common] {
		common++
	}

	var parts []string
	for range fromParts[common:] {
		parts = append(parts, "..")
	}
	parts = append(parts, toParts[common:]...)
	if len(parts) == 0 {
		return ".", nil
	}
	return strings.Join(parts, "/"), nil
}

// buildDocument merges the verbatim output keys with the generated projects
// into an ordered YAML mapping, so that version and the parallel flags lead and
// workflows and projects follow.
func buildDocument(cfg *config.Config, projects []Project) (*yaml.Node, error) {
	doc := &yaml.Node{Kind: yaml.MappingNode}

	// A stable, readable key order: the well-known Atlantis keys first, then
	// anything else the user configured, then the generated projects.
	preferred := []string{
		"version", "automerge", "parallel_plan", "parallel_apply",
		"abort_on_execution_order_fail", "delete_source_branch_on_merge",
		"emoji_reaction", "repo_locks", "silence_pr_comments",
		"allowed_regexp_prefixes", "autodiscover", "policies",
	}

	emitted := map[string]bool{"projects": true, "workflows": true}
	appendKey := func(key string, value any) error {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
		valueNode := &yaml.Node{}
		if err := valueNode.Encode(value); err != nil {
			return fmt.Errorf("encode %s: %w", key, err)
		}
		doc.Content = append(doc.Content, keyNode, valueNode)
		return nil
	}

	for _, key := range preferred {
		value, ok := cfg.Output[key]
		if !ok {
			continue
		}
		emitted[key] = true
		if err := appendKey(key, value); err != nil {
			return nil, err
		}
	}

	var rest []string
	for key := range cfg.Output {
		if !emitted[key] {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	for _, key := range rest {
		if err := appendKey(key, cfg.Output[key]); err != nil {
			return nil, err
		}
	}

	if workflows, ok := cfg.Output["workflows"]; ok {
		if err := appendKey("workflows", workflows); err != nil {
			return nil, err
		}
	}

	if projects == nil {
		projects = []Project{}
	}
	if err := appendKey("projects", projects); err != nil {
		return nil, err
	}

	return doc, nil
}
