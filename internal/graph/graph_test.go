package graph

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// globs is a minimal Matcher for tests, matching on a path prefix.
type globs []string

func (g globs) Match(path string) bool {
	for _, prefix := range g {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// writeTree materialises a map of relative path to content under a temp dir.
// A value of the form "->target" creates a symlink to another tree path.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	for rel, content := range files {
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
	return root
}

func build(t *testing.T, root string, opts ...func(*Options)) *Graph {
	t.Helper()
	o := Options{Root: root}
	for _, fn := range opts {
		fn(&o)
	}
	g, err := Build(o)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return g
}

func rootKeys(g *Graph) []string {
	var keys []string
	for _, node := range g.Roots() {
		keys = append(keys, node.Key)
	}
	return keys
}

// TestBuildTransitiveClosure is the heart of the tool: a change three modules
// deep must still reach the root that ultimately consumes it.
func TestBuildTransitiveClosure(t *testing.T) {
	root := writeTree(t, map[string]string{
		"envs/prod/main.tf": `provider "aws" {}
module "a" { source = "../../modules/a" }`,
		"modules/a/main.tf": `module "b" { source = "../b" }`,
		"modules/b/main.tf": `module "c" { source = "../c" }`,
		"modules/c/main.tf": `variable "x" {}`,
	})

	g := build(t, root)

	if want := []string{"envs/prod"}; !reflect.DeepEqual(rootKeys(g), want) {
		t.Fatalf("Roots = %v, want %v", rootKeys(g), want)
	}
	want := []string{"modules/a", "modules/b", "modules/c"}
	if got := g.Dependencies("envs/prod"); !reflect.DeepEqual(got, want) {
		t.Errorf("Dependencies = %v, want %v", got, want)
	}
}

func TestBuildDiamondDependency(t *testing.T) {
	root := writeTree(t, map[string]string{
		"envs/prod/main.tf": `provider "aws" {}
module "left" { source = "../../modules/left" }
module "right" { source = "../../modules/right" }`,
		"modules/left/main.tf":   `module "shared" { source = "../shared" }`,
		"modules/right/main.tf":  `module "shared" { source = "../shared" }`,
		"modules/shared/main.tf": `variable "x" {}`,
	})

	g := build(t, root)

	want := []string{"modules/left", "modules/right", "modules/shared"}
	if got := g.Dependencies("envs/prod"); !reflect.DeepEqual(got, want) {
		t.Errorf("Dependencies = %v, want %v", got, want)
	}
	// The shared module is reached twice but must appear once.
	if got := len(g.Nodes["modules/shared"].ReferencedBy); got != 2 {
		t.Errorf("shared ReferencedBy = %d, want 2", got)
	}
}

// TestBuildCycleTerminates makes sure a malformed repository is reported
// rather than hanging the tool.
func TestBuildCycleTerminates(t *testing.T) {
	root := writeTree(t, map[string]string{
		"envs/prod/main.tf": `provider "aws" {}
module "a" { source = "../../modules/a" }`,
		"modules/a/main.tf": `module "b" { source = "../b" }`,
		"modules/b/main.tf": `module "a" { source = "../a" }`,
	})

	g := build(t, root)

	want := []string{"modules/a", "modules/b"}
	if got := g.Dependencies("envs/prod"); !reflect.DeepEqual(got, want) {
		t.Errorf("Dependencies = %v, want %v", got, want)
	}
	if got := g.Cycles(); !reflect.DeepEqual(got, want) {
		t.Errorf("Cycles = %v, want %v", got, want)
	}
}

// TestBuildRemoteModulesRecordedNotFollowed covers the requirement that
// registry modules appear in the graph for completeness but never reach the
// generated configuration, since nothing in the repository changes with them.
func TestBuildRemoteModulesRecordedNotFollowed(t *testing.T) {
	root := writeTree(t, map[string]string{
		"envs/prod/main.tf": `provider "aws" {}
module "vpc" { source = "terraform-aws-modules/vpc/aws" }
module "endpoints" { source = "terraform-aws-modules/vpc/aws//modules/vpc-endpoints" }
module "local" { source = "../../modules/local" }`,
		"modules/local/main.tf": `variable "x" {}`,
	})

	g := build(t, root)

	for _, key := range []string{"terraform-aws-modules/vpc/aws", "terraform-aws-modules/vpc/aws//modules/vpc-endpoints"} {
		node, ok := g.Nodes[key]
		if !ok {
			t.Fatalf("remote module %q missing from the graph", key)
		}
		if node.Kind != Remote {
			t.Errorf("%q Kind = %v, want Remote", key, node.Kind)
		}
	}
	// Dependencies is what feeds when_modified, so remotes must not appear.
	if got, want := g.Dependencies("envs/prod"), []string{"modules/local"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Dependencies = %v, want %v", got, want)
	}
}

// TestBuildDoubleSlashLocalPath covers a local source written with a stray
// double separator, which must not be mistaken for a registry subdirectory.
func TestBuildDoubleSlashLocalPath(t *testing.T) {
	root := writeTree(t, map[string]string{
		"envs/prod/main.tf":   `provider "aws" {}` + "\n" + `module "lib" { source = "../..//modules/lib" }`,
		"modules/lib/main.tf": `variable "x" {}`,
	})

	g := build(t, root)

	if got, want := g.Dependencies("envs/prod"), []string{"modules/lib"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Dependencies = %v, want %v", got, want)
	}
}

// TestBuildSymlinkedSharedSource is the shape used throughout the repository:
// a root module whose only Terraform file is a symlink into a shared template.
// The template's module paths are written for the consumer, so they must be
// resolved there, and the template directory itself must never be a project.
func TestBuildSymlinkedSharedSource(t *testing.T) {
	root := writeTree(t, map[string]string{
		"common/base/main.source.tf": `provider "aws" {}
module "base" { source = "../../modules/base" }`,
		"envs/prod/main.tf":    "->common/base/main.source.tf",
		"envs/staging/main.tf": "->common/base/main.source.tf",
		"modules/base/main.tf": `variable "x" {}`,
	})

	g := build(t, root)

	if want := []string{"envs/prod", "envs/staging"}; !reflect.DeepEqual(rootKeys(g), want) {
		t.Fatalf("Roots = %v, want %v", rootKeys(g), want)
	}
	// Both the template directory (where a diff lands) and the module the
	// template calls (resolved from the consumer) must be dependencies.
	want := []string{"common/base", "modules/base"}
	if got := g.Dependencies("envs/prod"); !reflect.DeepEqual(got, want) {
		t.Errorf("Dependencies = %v, want %v", got, want)
	}
	if !g.Nodes["common/base"].SharedSource {
		t.Error("the template directory should be marked as a shared source")
	}
	if g.Nodes["common/base"].HasProvider {
		t.Error("a shared source must not carry a provider block of its own")
	}
}

// TestBuildOrphanSharedSourceLooksLikeARoot pins a known limitation. A template
// that nothing symlinks is indistinguishable from a root module: Terraform
// files, a provider block, referenced by nobody. It is therefore parsed in
// place, its consumer-relative module paths do not resolve, and it is offered
// as a project. Callers filter it out with projects.exclude; the renderer warns
// if the invented edges demote anything real.
func TestBuildOrphanSharedSourceLooksLikeARoot(t *testing.T) {
	root := writeTree(t, map[string]string{
		"common/unused/main.source.tf": `provider "aws" {}
module "x" { source = "../../../outside/repo" }`,
	})

	g := build(t, root)

	if want := []string{"common/unused"}; !reflect.DeepEqual(rootKeys(g), want) {
		t.Errorf("Roots = %v, want %v", rootKeys(g), want)
	}
	if len(g.Warnings) == 0 {
		t.Error("expected a warning about the unresolvable module path")
	}
}

// TestBuildReferencedDirIsNotARoot covers a library module that declares a
// provider block anyway. Being referenced disqualifies it.
func TestBuildReferencedDirIsNotARoot(t *testing.T) {
	root := writeTree(t, map[string]string{
		"envs/prod/main.tf": `provider "aws" {}
module "lib" { source = "../../modules/lib" }`,
		"modules/lib/main.tf": `provider "kubernetes" {}`,
	})

	g := build(t, root)

	if want := []string{"envs/prod"}; !reflect.DeepEqual(rootKeys(g), want) {
		t.Errorf("Roots = %v, want %v", rootKeys(g), want)
	}
}

// TestBuildNoProviderIsNotARoot covers an unreferenced library module. Without
// a provider block it cannot be planned on its own.
func TestBuildNoProviderIsNotARoot(t *testing.T) {
	root := writeTree(t, map[string]string{
		"modules/orphan/main.tf": `variable "x" {}`,
		"envs/prod/main.tf":      `provider "aws" {}`,
	})

	g := build(t, root)

	if want := []string{"envs/prod"}; !reflect.DeepEqual(rootKeys(g), want) {
		t.Errorf("Roots = %v, want %v", rootKeys(g), want)
	}
}

// TestBuildSkipsVendoredModules guards against .terraform, which holds copies
// of registry modules whose own relative sources would pollute the graph.
func TestBuildSkipsVendoredModules(t *testing.T) {
	root := writeTree(t, map[string]string{
		"envs/prod/main.tf": `provider "aws" {}`,
		"envs/prod/.terraform/modules/vpc/main.tf": `provider "aws" {}
module "escape" { source = "../../../../../../etc" }`,
	})

	g := build(t, root)

	for key := range g.Nodes {
		if strings.Contains(key, ".terraform") {
			t.Errorf("vendored directory entered the graph: %q", key)
		}
	}
}

func TestBuildExcludeSkipsSubtree(t *testing.T) {
	root := writeTree(t, map[string]string{
		"envs/prod/main.tf":    `provider "aws" {}`,
		"vendor/thing/main.tf": `provider "aws" {}`,
	})

	g := build(t, root, func(o *Options) { o.Exclude = globs{"vendor"} })

	if _, ok := g.Nodes["vendor/thing"]; ok {
		t.Error("excluded directory entered the graph")
	}
}

// TestBuildMissingModuleTargetWarns covers a source pointing at a directory
// that does not exist: worth reporting, but not worth an invented edge.
func TestBuildMissingModuleTargetWarns(t *testing.T) {
	root := writeTree(t, map[string]string{
		"envs/prod/main.tf": `provider "aws" {}
module "gone" { source = "../../modules/gone" }`,
	})

	g := build(t, root)

	if len(g.Warnings) == 0 {
		t.Error("expected a warning about the missing module directory")
	}
	if got := g.Dependencies("envs/prod"); len(got) != 0 {
		t.Errorf("Dependencies = %v, want none", got)
	}
}

// TestBuildRecordsExternalFiles checks that files read from outside a module
// directory are recorded, and reach the roots that depend on the module.
func TestBuildRecordsExternalFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"envs/prod/main.tf": `provider "aws" {}
module "k8s" { source = "../../modules/k8s" }
locals { own = file("${path.module}/values.yaml") }`,
		"envs/prod/values.yaml": "x: 1",
		"modules/k8s/main.tf": `locals {
  crd     = file("${path.module}/../../tools/crd.yml")
  inside  = file("${path.module}/files/a.txt")
  missing = file("${path.module}/../../tools/missing.yml")
  escape  = file("${path.module}/../../../outside.txt")
}`,
		"modules/k8s/files/a.txt": "a",
		"tools/crd.yml":           "kind: CustomResourceDefinition",
	})

	g := build(t, root)

	if want := []string{"tools/crd.yml"}; !reflect.DeepEqual(g.Nodes["modules/k8s"].Files, want) {
		t.Errorf("modules/k8s Files = %v, want %v", g.Nodes["modules/k8s"].Files, want)
	}
	if got := g.Nodes["envs/prod"].Files; len(got) != 0 {
		t.Errorf("envs/prod Files = %v, want none: its file is inside the directory", got)
	}
	if want := []string{"tools/crd.yml"}; !reflect.DeepEqual(g.Files("envs/prod"), want) {
		t.Errorf("Files(envs/prod) = %v, want %v", g.Files("envs/prod"), want)
	}
	if !slices.ContainsFunc(g.Warnings, func(w string) bool { return strings.Contains(w, "tools/missing.yml") }) {
		t.Errorf("expected a warning about the missing file, got %v", g.Warnings)
	}
}
