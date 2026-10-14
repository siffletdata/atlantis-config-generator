package main

import (
	"bytes"
	"flag"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/moby/patternmatcher"
	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// fixture is a synthetic monorepo plus the configuration to generate from it.
// A file whose content starts with "->" is created as a symlink to another
// path in the same tree.
type fixture struct {
	name   string
	files  map[string]string
	config string
}

func (f fixture) write(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	for rel, content := range f.files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if target, ok := strings.CutPrefix(content, "->"); ok {
			if err := os.Symlink(filepath.Join(root, filepath.FromSlash(target)), abs); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if f.config != "" {
		if err := os.WriteFile(filepath.Join(root, defaultConfigName), []byte(f.config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// generate runs the CLI end to end and returns stdout.
func (f fixture) generate(t *testing.T, root string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := run(&options{repoRoot: root}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}
	if warnings := stderr.String(); warnings != "" {
		t.Logf("stderr:\n%s", warnings)
	}
	return stdout.String()
}

// fixtures cover each way the parser or the graph can go wrong. The names map
// to golden files under testdata/.
var fixtures = []fixture{
	{
		// Two roots sharing one library module.
		name: "simple",
		files: map[string]string{
			"envs/prod/providers.tf": `provider "aws" { region = "eu-west-1" }`,
			"envs/prod/main.tf":      `module "vpc" { source = "../../modules/vpc" }`,
			"envs/dev/providers.tf":  `provider "aws" { region = "eu-west-1" }`,
			"envs/dev/main.tf":       `module "vpc" { source = "../../modules/vpc" }`,
			"modules/vpc/main.tf":    `variable "cidr" {}`,
		},
		config: `
projects:
  path_prefix: "./"
  terraform_version: "1.11.1"
output:
  version: 3
  parallel_plan: true
  parallel_apply: true
`,
	},
	{
		// A chain four deep. The point of the whole tool is that lib_c ends up
		// in the root's when_modified.
		name: "transitive",
		files: map[string]string{
			"envs/prod/main.tf": `provider "aws" {}
module "a" { source = "../../modules/lib_a" }`,
			"modules/lib_a/main.tf":      `module "b" { source = "../lib_b" }`,
			"modules/lib_b/main.tf":      `module "c" { source = "../lib_c" }`,
			"modules/lib_c/main.tf":      `variable "x" {}`,
			"modules/lib_c/values.yaml":  "replicas: 2",
			"modules/lib_c/charts/x.tpl": "template",
		},
		config: "projects:\n  terraform_version: \"1.11.1\"\n",
	},
	{
		// The shared-template shape: the only Terraform file in each root is a
		// symlink, and the template's module paths are written for consumers.
		name: "symlink-common",
		files: map[string]string{
			"common/clusters/base/main.source.tf": `provider "aws" {}
module "base" { source = "../../../modules/base" }`,
			"envs/clusters/prod/main.tf":        "->common/clusters/base/main.source.tf",
			"envs/clusters/prod/cluster.tfvars": `name = "prod"`,
			"envs/clusters/staging/main.tf":     "->common/clusters/base/main.source.tf",
			"modules/base/main.tf":              `module "inner" { source = "../inner" }`,
			"modules/inner/main.tf":             `variable "x" {}`,
		},
		config: "projects:\n  terraform_version: \"1.11.1\"\n",
	},
	{
		// An override file corrects a module path that the shared template
		// gets wrong for this one consumer, which nests one level deeper.
		name: "override",
		files: map[string]string{
			"common/main.source.tf": `provider "aws" {}
module "common" { source = "../../modules/common" }`,
			"envs/a/b/main.tf":                 "->common/main.source.tf",
			"envs/a/b/module_path_override.tf": `module "common" { source = "../../../modules/common" }`,
			"modules/common/main.tf":           `variable "x" {}`,
		},
		config: "projects:\n  terraform_version: \"1.11.1\"\n",
	},
	{
		// Registry and git sources belong in the graph for completeness but
		// never in when_modified: nothing in a diff can match them.
		name: "registry",
		files: map[string]string{
			"envs/prod/main.tf": `provider "aws" {}
module "vpc" { source = "terraform-aws-modules/vpc/aws" }
module "endpoints" { source = "terraform-aws-modules/vpc/aws//modules/vpc-endpoints" }
module "remote" { source = "git::https://example.com/mod.git?ref=v1" }
module "local" { source = "../..//modules/local" }`,
			"modules/local/main.tf": `variable "x" {}`,
		},
		config: "projects:\n  terraform_version: \"1.11.1\"\n",
	},
	{
		// Include and exclude globs, plus a library module that declares a
		// provider block: being referenced still disqualifies it.
		name: "exclusions",
		files: map[string]string{
			"envs/prod/main.tf": `provider "aws" {}
module "lib" { source = "../../modules/with_provider" }`,
			"envs/legacy/main.tf":             `provider "aws" {}`,
			"envs/global/azure/thing/main.tf": `provider "azurerm" {}`,
			"modules/with_provider/main.tf":   `provider "kubernetes" {}`,
			"scratch/experiment/main.tf":      `provider "aws" {}`,
			"envs/prod/.terraform/modules/v/main.tf": `provider "aws" {}
module "escape" { source = "../../../../../modules/with_provider" }`,
		},
		config: `
projects:
  include: ["envs/**"]
  exclude:
    - "**/legacy"
    - "**/azure/**"
  terraform_version: "1.11.1"
`,
	},
	{
		// A library module reads a manifest that lives outside every module
		// directory; changing it must still plan the root that uses it.
		name: "file-functions",
		files: map[string]string{
			"provisioning/envs/prod/main.tf": `provider "kubernetes" {}
module "k8s" { source = "../../tf_modules/kubernetes" }`,
			"provisioning/tf_modules/kubernetes/main.tf": `resource "kubernetes_manifest" "crd" {
  manifest = yamldecode(file(
    "${path.module}/../../../tools/operator/manifests/crd.yml")
  )
}
locals {
  values = templatefile("${path.module}/values.tpl", {})
}`,
			"provisioning/tf_modules/kubernetes/values.tpl": "x",
			"tools/operator/manifests/crd.yml":              "kind: CustomResourceDefinition",
			"tools/operator/manifests/other.yml":            "kind: Deployment",
		},
		config: "projects:\n  terraform_version: \"1.11.1\"\n",
	},
	{
		// Rules assign workflows and extra patterns by directory.
		name: "workflows",
		files: map[string]string{
			"envs/clusters/a/base/main.tf":       `provider "aws" {}`,
			"envs/clusters/a/kubernetes/main.tf": `provider "kubernetes" {}`,
			"envs/global/ops/main.tf":            `provider "aws" {}`,
		},
		config: `
header_comment: |
  DO NOT EDIT.
  Generated by atlantis-config-generator.
projects:
  path_prefix: "./"
  terraform_version: "1.11.1"
  when_modified:
    self: ["*.tf", "*.tfvars", ".terraform.lock.hcl"]
    dependency: ["**"]
rules:
  - match: '/clusters/[^/]+/base$'
    workflow: cluster_base
    extra_when_modified: ["*.s3.tfbackend"]
    execution_order_group: 0
  - match: '/clusters/[^/]+/kubernetes$'
    workflow: cluster_kubernetes
    execution_order_group: 1
  - match: '.*'
    workflow: default
output:
  version: 3
  parallel_plan: true
  parallel_apply: true
  workflows:
    default:
      apply:
        steps:
          - run: ./scripts/check-apply-allowed.sh
          - apply
    cluster_base:
      plan:
        steps:
          - init:
              extra_args: ["-backend-config=cluster.s3.tfbackend"]
          - plan
    cluster_kubernetes:
      plan:
        steps:
          - init:
              extra_args: ["-backend-config=cluster.s3.tfbackend"]
          - plan
`,
	},
}

func TestIntegrationGolden(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			got := f.generate(t, f.write(t))
			golden := filepath.Join("testdata", f.name+".golden.yaml")

			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run: go test ./... -update)", err)
			}
			if got != string(want) {
				t.Errorf("output differs from %s\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
			}
		})
	}
}

// TestIntegrationTransitiveDependencyReaches asserts the property the tool
// exists for, independently of the golden files.
func TestIntegrationTransitiveDependencyReaches(t *testing.T) {
	f := fixtureByName(t, "transitive")
	cfg := parseGenerated(t, f.generate(t, f.write(t)))

	project := cfg.projectFor(t, "envs/prod")
	if !slices.Contains(project.Autoplan.WhenModified, "../../modules/lib_c/**") {
		t.Errorf("the deepest dependency is missing: %v", project.Autoplan.WhenModified)
	}
}

// TestIntegrationRegistryModulesExcluded pins the rule that remote sources
// never reach the generated configuration.
func TestIntegrationRegistryModulesExcluded(t *testing.T) {
	f := fixtureByName(t, "registry")
	cfg := parseGenerated(t, f.generate(t, f.write(t)))

	project := cfg.projectFor(t, "envs/prod")
	for _, pattern := range project.Autoplan.WhenModified {
		if strings.Contains(pattern, "terraform-aws-modules") || strings.Contains(pattern, "example.com") {
			t.Errorf("a remote module reached when_modified: %q", pattern)
		}
	}
	if !slices.Contains(project.Autoplan.WhenModified, "../../modules/local/**") {
		t.Errorf("the local module is missing: %v", project.Autoplan.WhenModified)
	}
}

// TestWhenModifiedMatchesAtlantisSemantics is the test that de-risks the whole
// tool. It reproduces what Atlantis does with when_modified — join each
// pattern onto the project directory, then match with Docker's .dockerignore
// matcher — and asserts that a change deep in the dependency graph really does
// select the project.
//
// Both the joining and the matcher are Atlantis's, not ours: patternmatcher is
// the same library, at the same version Atlantis depends on.
func TestWhenModifiedMatchesAtlantisSemantics(t *testing.T) {
	f := fixtureByName(t, "transitive")
	cfg := parseGenerated(t, f.generate(t, f.write(t)))
	project := cfg.projectFor(t, "envs/prod")

	matcher := atlantisMatcher(t, project)

	tests := []struct {
		changed string
		want    bool
		why     string
	}{
		{"envs/prod/main.tf", true, "the project's own configuration"},
		{"envs/prod/terraform.tfvars", true, "any file in the project directory"},
		{"modules/lib_a/main.tf", true, "a direct dependency"},
		{"modules/lib_b/main.tf", true, "a dependency one level down"},
		{"modules/lib_c/main.tf", true, "a dependency three levels down"},
		{"modules/lib_c/values.yaml", true, "a non-Terraform asset in a deep dependency"},
		{"modules/lib_c/charts/x.tpl", true, "a nested asset in a deep dependency"},
		{"modules/unrelated/main.tf", false, "a module nothing depends on"},
		{"docs/README.md", false, "a file outside the graph"},
	}

	for _, tc := range tests {
		got, err := matcher.MatchesOrParentMatches(tc.changed)
		if err != nil {
			t.Fatalf("%s: %v", tc.changed, err)
		}
		if got != tc.want {
			t.Errorf("changing %s selects the project = %v, want %v (%s)", tc.changed, got, tc.want, tc.why)
		}
	}
}

// TestWhenModifiedCoversFilesReadOutsideModules checks that a file a
// dependency reads with file() selects the project, and only that file.
func TestWhenModifiedCoversFilesReadOutsideModules(t *testing.T) {
	f := fixtureByName(t, "file-functions")
	cfg := parseGenerated(t, f.generate(t, f.write(t)))
	matcher := atlantisMatcher(t, cfg.projectFor(t, "provisioning/envs/prod"))

	tests := []struct {
		changed string
		want    bool
	}{
		{"tools/operator/manifests/crd.yml", true},
		{"provisioning/tf_modules/kubernetes/values.tpl", true},
		{"tools/operator/manifests/other.yml", false},
	}
	for _, tc := range tests {
		got, err := matcher.MatchesOrParentMatches(tc.changed)
		if err != nil {
			t.Fatalf("%s: %v", tc.changed, err)
		}
		if got != tc.want {
			t.Errorf("changing %s selects the project = %v, want %v", tc.changed, got, tc.want)
		}
	}
}

// TestWhenModifiedIsolatesSiblingProjects checks that a shared template
// triggers every consumer while a project-local change triggers only one.
func TestWhenModifiedIsolatesSiblingProjects(t *testing.T) {
	f := fixtureByName(t, "symlink-common")
	cfg := parseGenerated(t, f.generate(t, f.write(t)))

	prod := atlantisMatcher(t, cfg.projectFor(t, "envs/clusters/prod"))
	staging := atlantisMatcher(t, cfg.projectFor(t, "envs/clusters/staging"))

	tests := []struct {
		changed               string
		wantProd, wantStaging bool
	}{
		{"common/clusters/base/main.source.tf", true, true},
		{"modules/inner/main.tf", true, true},
		{"envs/clusters/prod/cluster.tfvars", true, false},
	}

	for _, tc := range tests {
		gotProd, _ := prod.MatchesOrParentMatches(tc.changed)
		gotStaging, _ := staging.MatchesOrParentMatches(tc.changed)
		if gotProd != tc.wantProd || gotStaging != tc.wantStaging {
			t.Errorf("changing %s selects prod=%v staging=%v, want prod=%v staging=%v",
				tc.changed, gotProd, gotStaging, tc.wantProd, tc.wantStaging)
		}
	}
}

// atlantisMatcher builds the matcher exactly as Atlantis does in
// server/events/project_finder.go: each pattern is joined onto the project
// directory, preserving a leading "!" negation.
func atlantisMatcher(t *testing.T, project generatedProject) *patternmatcher.PatternMatcher {
	t.Helper()

	dir := strings.TrimPrefix(project.Dir, "./")
	patterns := make([]string, 0, len(project.Autoplan.WhenModified))
	for _, pattern := range project.Autoplan.WhenModified {
		if negated, ok := strings.CutPrefix(pattern, "!"); ok {
			patterns = append(patterns, "!"+path.Join(dir, negated))
			continue
		}
		patterns = append(patterns, path.Join(dir, pattern))
	}

	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		t.Fatalf("patternmatcher.New(%v): %v", patterns, err)
	}
	return matcher
}

type generatedProject struct {
	Name     string `yaml:"name"`
	Dir      string `yaml:"dir"`
	Workflow string `yaml:"workflow"`
	Autoplan struct {
		WhenModified []string `yaml:"when_modified"`
	} `yaml:"autoplan"`
}

type generatedConfig struct {
	Version  int                `yaml:"version"`
	Projects []generatedProject `yaml:"projects"`
}

func (c generatedConfig) projectFor(t *testing.T, dir string) generatedProject {
	t.Helper()
	for _, p := range c.Projects {
		if strings.TrimPrefix(p.Dir, "./") == dir {
			return p
		}
	}
	var dirs []string
	for _, p := range c.Projects {
		dirs = append(dirs, p.Dir)
	}
	sort.Strings(dirs)
	t.Fatalf("no project for %q; have %v", dir, dirs)
	return generatedProject{}
}

func parseGenerated(t *testing.T, data string) generatedConfig {
	t.Helper()
	var cfg generatedConfig
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("parse generated config: %v\n%s", err, data)
	}
	if cfg.Version != 3 {
		t.Errorf("version = %d, want 3", cfg.Version)
	}
	return cfg
}

func fixtureByName(t *testing.T, name string) fixture {
	t.Helper()
	if i := slices.IndexFunc(fixtures, func(f fixture) bool { return f.name == name }); i >= 0 {
		return fixtures[i]
	}
	t.Fatalf("no fixture named %q", name)
	return fixture{}
}
