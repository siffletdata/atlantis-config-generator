// Package config defines the generator's configuration file: which parts of
// the repository to scan, how to turn dependencies into autoplan patterns, and
// what to copy verbatim into the generated Atlantis configuration.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// Config is the whole configuration file.
type Config struct {
	// HeaderComment is prepended to the output, with each line commented out.
	HeaderComment string `yaml:"header_comment"`
	// Scan controls which directories are visited.
	Scan Scan `yaml:"scan"`
	// Projects controls which root modules become Atlantis projects and how
	// each project entry is built.
	Projects Projects `yaml:"projects"`
	// Rules assign a workflow and other per-project settings by directory.
	// The first matching rule wins.
	Rules []Rule `yaml:"rules"`
	// Output holds the top-level Atlantis keys, copied verbatim. This is where
	// version, parallel_plan, parallel_apply and the workflows block live.
	Output map[string]any `yaml:"output"`
}

// Scan limits the directory walk.
type Scan struct {
	// Exclude holds doublestar patterns relative to the repository root.
	// A matching directory is not scanned at all, so it neither becomes a
	// project nor contributes edges to the graph.
	Exclude []string `yaml:"exclude"`
}

// Projects controls project selection and rendering.
type Projects struct {
	// Include, when non-empty, restricts projects to directories matching at
	// least one pattern. Directories outside it are still scanned, so they can
	// still be depended upon.
	Include []string `yaml:"include"`
	// Exclude removes directories from the project list even when they would
	// otherwise qualify.
	Exclude []string `yaml:"exclude"`
	// PathPrefix is prepended to the emitted dir and name, letting the output
	// use "./infra/..." rather than "infra/...".
	PathPrefix string `yaml:"path_prefix"`
	// TerraformVersion is emitted on every project unless a rule overrides it.
	TerraformVersion string `yaml:"terraform_version"`
	// TerraformVersionFile reads the version from a file in the repository,
	// such as ".opentofu-version". TerraformVersion takes precedence.
	TerraformVersionFile string `yaml:"terraform_version_file"`
	// WhenModified builds each project's autoplan patterns.
	WhenModified WhenModified `yaml:"when_modified"`
}

// WhenModified holds the glob patterns used to build autoplan.when_modified.
//
// Atlantis interprets these with Docker's .dockerignore matcher, relative to
// the project directory, so a trailing "**" means "anything underneath".
type WhenModified struct {
	// Self applies to the project's own directory.
	Self []string `yaml:"self"`
	// Dependency applies to each transitive dependency directory, prefixed
	// with the relative path from the project to that directory.
	Dependency []string `yaml:"dependency"`
}

// Rule assigns settings to projects whose directory matches Match.
type Rule struct {
	// Match is a regular expression tested against the repository-relative
	// project directory, without PathPrefix.
	Match string `yaml:"match"`
	// Workflow names a workflow from Output["workflows"].
	Workflow string `yaml:"workflow"`
	// TerraformVersion overrides Projects.TerraformVersion.
	TerraformVersion string `yaml:"terraform_version"`
	// Workspace overrides Atlantis's "default" workspace.
	Workspace string `yaml:"workspace"`
	// ExecutionOrderGroup sorts projects before planning and applying.
	ExecutionOrderGroup *int `yaml:"execution_order_group"`
	// AutoplanEnabled toggles automatic planning. A project with this set to
	// false is still planned by an explicit "atlantis plan".
	AutoplanEnabled *bool `yaml:"autoplan_enabled"`
	// ExtraWhenModified appends patterns to the computed list.
	ExtraWhenModified []string `yaml:"extra_when_modified"`

	compiled *regexp.Regexp
}

// Matches reports whether the rule applies to a repository-relative directory.
func (r *Rule) Matches(dir string) bool {
	return r.compiled != nil && r.compiled.MatchString(dir)
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse validates configuration that has already been read into memory.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Default returns the configuration used when no file is supplied: scan
// everything, treat every root module as a project, and trigger on any change
// in the project or in any of its transitive dependencies.
func Default() *Config {
	cfg := &Config{}
	cfg.applyDefaults()
	return cfg
}

func (c *Config) applyDefaults() {
	if len(c.Projects.WhenModified.Self) == 0 {
		c.Projects.WhenModified.Self = []string{"**"}
	}
	if len(c.Projects.WhenModified.Dependency) == 0 {
		c.Projects.WhenModified.Dependency = []string{"**"}
	}
	if c.Output == nil {
		c.Output = map[string]any{}
	}
	if _, ok := c.Output["version"]; !ok {
		c.Output["version"] = 3
	}
}

func (c *Config) validate() error {
	known := c.workflowNames()

	for i := range c.Rules {
		rule := &c.Rules[i]
		if rule.Match == "" {
			return fmt.Errorf("rules[%d]: match is required", i)
		}
		compiled, err := regexp.Compile(rule.Match)
		if err != nil {
			return fmt.Errorf("rules[%d]: invalid match %q: %w", i, rule.Match, err)
		}
		rule.compiled = compiled

		// Catching this here beats letting Atlantis reject the whole file.
		if rule.Workflow != "" && known != nil && !known[rule.Workflow] {
			return fmt.Errorf("rules[%d]: workflow %q is not defined in output.workflows", i, rule.Workflow)
		}
	}

	for _, group := range [][]string{c.Scan.Exclude, c.Projects.Include, c.Projects.Exclude} {
		for _, pattern := range group {
			if !doublestar.ValidatePattern(pattern) {
				return fmt.Errorf("invalid glob pattern %q", pattern)
			}
		}
	}

	if version, ok := c.Output["version"]; ok {
		if n, isInt := version.(int); !isInt || n != 3 {
			return fmt.Errorf("output.version must be 3, got %v", version)
		}
	}

	return nil
}

// workflowNames returns the workflows declared in output, or nil when the key
// is absent or not a mapping. A nil result disables the rule check, since
// workflows may legitimately come from the Atlantis server-side config.
func (c *Config) workflowNames() map[string]bool {
	raw, ok := c.Output["workflows"]
	if !ok {
		return nil
	}
	mapping, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	names := map[string]bool{}
	for name := range mapping {
		names[name] = true
	}
	return names
}

// MatchRule returns the first rule matching a repository-relative project
// directory, or nil when none does.
func (c *Config) MatchRule(dir string) *Rule {
	for i := range c.Rules {
		if c.Rules[i].Matches(dir) {
			return &c.Rules[i]
		}
	}
	return nil
}

// ResolveTerraformVersion returns the repository-wide Terraform version,
// reading terraform_version_file when no literal version is configured.
func (c *Config) ResolveTerraformVersion(repoRoot string) (string, error) {
	if c.Projects.TerraformVersion != "" {
		return c.Projects.TerraformVersion, nil
	}
	if c.Projects.TerraformVersionFile == "" {
		return "", nil
	}
	data, err := os.ReadFile(repoRoot + "/" + c.Projects.TerraformVersionFile)
	if err != nil {
		return "", fmt.Errorf("read terraform_version_file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// Globs is a compiled set of doublestar patterns.
type Globs []string

// Match reports whether path matches any pattern. A pattern also matches
// anything beneath a directory it names, so "**/argocd" excludes the argocd
// directory and its whole subtree.
func (g Globs) Match(path string) bool {
	for _, pattern := range g {
		if ok, err := doublestar.Match(pattern, path); err == nil && ok {
			return true
		}
		// Let a pattern naming a directory cover that directory's contents.
		if ok, err := doublestar.Match(strings.TrimSuffix(pattern, "/**")+"/**", path); err == nil && ok {
			return true
		}
	}
	return false
}

// ScanExclude returns the compiled scan exclusions.
func (c *Config) ScanExclude() Globs { return Globs(c.Scan.Exclude) }

// ProjectInclude returns the compiled project inclusions.
func (c *Config) ProjectInclude() Globs { return Globs(c.Projects.Include) }

// ProjectExclude returns the compiled project exclusions.
func (c *Config) ProjectExclude() Globs { return Globs(c.Projects.Exclude) }
