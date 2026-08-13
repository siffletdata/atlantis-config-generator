package render

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/siffletdata/atlantis-config-generator/internal/config"
	"github.com/siffletdata/atlantis-config-generator/internal/graph"
)

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

// renderTree runs the whole pipeline over a synthetic repository.
func renderTree(t *testing.T, files map[string]string, configYAML string) *Result {
	t.Helper()
	root := writeTree(t, files)

	cfg, err := config.Parse([]byte(configYAML))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	g, err := graph.Build(graph.Options{Root: root, Exclude: cfg.ScanExclude()})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	result, err := Render(Options{Config: cfg, Graph: g, RepoRoot: root})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return result
}

func TestRelPath(t *testing.T) {
	tests := []struct {
		from, to, want string
	}{
		{"envs/prod", "modules/vpc", "../../modules/vpc"},
		{"a/b/c/d", "a/b/x", "../../x"},
		{"envs/prod", "envs/prod/sub", "sub"},
		{"deep/a/b/c/d/e", "lib", "../../../../../../lib"},
		{"a", "a", "."},
	}
	for _, tc := range tests {
		got, err := relPath(tc.from, tc.to)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("relPath(%q, %q) = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestRenderWhenModified(t *testing.T) {
	result := renderTree(t, map[string]string{
		"envs/prod/main.tf": `provider "aws" {}
module "a" { source = "../../modules/a" }`,
		"modules/a/main.tf": `module "b" { source = "../b" }`,
		"modules/b/main.tf": `variable "x" {}`,
	}, `{}`)

	if len(result.Projects) != 1 {
		t.Fatalf("expected one project, got %d", len(result.Projects))
	}
	want := []string{"**", "../../modules/a/**", "../../modules/b/**"}
	if got := result.Projects[0].Autoplan.WhenModified; !reflect.DeepEqual(got, want) {
		t.Errorf("when_modified = %v, want %v", got, want)
	}
}

func TestRenderCustomPatterns(t *testing.T) {
	result := renderTree(t, map[string]string{
		"envs/prod/main.tf": `provider "aws" {}
module "a" { source = "../../modules/a" }`,
		"modules/a/main.tf": `variable "x" {}`,
	}, `
projects:
  when_modified:
    self: ["*.tf", "*.tfvars"]
    dependency: ["*.tf", "*.yaml"]
`)

	// Self patterns are emitted verbatim; dependency patterns are prefixed with
	// the relative path from the project to the dependency.
	want := []string{"*.tf", "*.tfvars", "../../modules/a/*.tf", "../../modules/a/*.yaml"}
	if got := result.Projects[0].Autoplan.WhenModified; !reflect.DeepEqual(got, want) {
		t.Errorf("when_modified = %v, want %v", got, want)
	}
}

func TestRenderRulesApply(t *testing.T) {
	result := renderTree(t, map[string]string{
		"envs/prod/base/main.tf":       `provider "aws" {}`,
		"envs/prod/kubernetes/main.tf": `provider "kubernetes" {}`,
	}, `
projects:
  path_prefix: "./"
  terraform_version: "1.11.1"
rules:
  - match: '/base$'
    workflow: cluster_base
    extra_when_modified: ["*.s3.tfbackend"]
  - match: '.*'
    workflow: default
    terraform_version: "1.9.0"
output:
  workflows:
    cluster_base: {}
    default: {}
`)

	byDir := map[string]Project{}
	for _, p := range result.Projects {
		byDir[p.Dir] = p
	}

	base := byDir["./envs/prod/base"]
	if base.Workflow != "cluster_base" {
		t.Errorf("base workflow = %q, want cluster_base", base.Workflow)
	}
	if base.TerraformVersion != "1.11.1" {
		t.Errorf("base terraform_version = %q, want 1.11.1", base.TerraformVersion)
	}
	if got := base.Autoplan.WhenModified; !reflect.DeepEqual(got, []string{"**", "*.s3.tfbackend"}) {
		t.Errorf("base when_modified = %v", got)
	}

	k8s := byDir["./envs/prod/kubernetes"]
	if k8s.Workflow != "default" {
		t.Errorf("kubernetes workflow = %q, want default", k8s.Workflow)
	}
	if k8s.TerraformVersion != "1.9.0" {
		t.Errorf("kubernetes terraform_version = %q, want the rule override", k8s.TerraformVersion)
	}
}

func TestRenderIncludeExclude(t *testing.T) {
	result := renderTree(t, map[string]string{
		"envs/prod/main.tf":     `provider "aws" {}`,
		"envs/legacy/main.tf":   `provider "aws" {}`,
		"scratch/thing/main.tf": `provider "aws" {}`,
	}, `
projects:
  include: ["envs/**"]
  exclude: ["**/legacy"]
`)

	var dirs []string
	for _, p := range result.Projects {
		dirs = append(dirs, p.Dir)
	}
	if want := []string{"envs/prod"}; !reflect.DeepEqual(dirs, want) {
		t.Errorf("projects = %v, want %v", dirs, want)
	}
}

func TestRenderDeterministicOrdering(t *testing.T) {
	files := map[string]string{
		"envs/c/main.tf": `provider "aws" {}`,
		"envs/a/main.tf": `provider "aws" {}`,
		"envs/b/main.tf": `provider "aws" {}`,
	}

	first := renderTree(t, files, `{}`)
	second := renderTree(t, files, `{}`)

	if string(first.YAML) != string(second.YAML) {
		t.Error("two runs over the same tree produced different output")
	}
	var dirs []string
	for _, p := range first.Projects {
		dirs = append(dirs, p.Dir)
	}
	if want := []string{"envs/a", "envs/b", "envs/c"}; !reflect.DeepEqual(dirs, want) {
		t.Errorf("projects = %v, want them sorted", dirs)
	}
}

// TestRenderOutputKeyOrder pins the shape of the generated document: the
// top-level Atlantis keys lead, workflows and projects follow.
func TestRenderOutputKeyOrder(t *testing.T) {
	result := renderTree(t, map[string]string{
		"envs/prod/main.tf": `provider "aws" {}`,
	}, `
output:
  version: 3
  parallel_plan: true
  parallel_apply: true
  automerge: false
  workflows:
    default:
      apply:
        steps: [apply]
`)

	var doc yaml.Node
	if err := yaml.Unmarshal(result.YAML, &doc); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for i := 0; i < len(doc.Content[0].Content); i += 2 {
		keys = append(keys, doc.Content[0].Content[i].Value)
	}

	want := []string{"version", "automerge", "parallel_plan", "parallel_apply", "workflows", "projects"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("top-level keys = %v, want %v", keys, want)
	}
}

func TestRenderHeaderComment(t *testing.T) {
	result := renderTree(t, map[string]string{
		"envs/prod/main.tf": `provider "aws" {}`,
	}, "header_comment: |\n  DO NOT EDIT.\n\n  Generated automatically.\n")

	want := "# DO NOT EDIT.\n#\n# Generated automatically.\nversion: 3\n"
	if !strings.HasPrefix(string(result.YAML), want) {
		t.Errorf("output starts with:\n%q\nwant prefix:\n%q", string(result.YAML)[:80], want)
	}
}

// TestRenderSymlinkedProject covers the shared-template shape end to end: the
// template directory and the module it calls both become triggers, and the
// template itself is not a project.
func TestRenderSymlinkedProject(t *testing.T) {
	result := renderTree(t, map[string]string{
		"common/base/main.source.tf": `provider "aws" {}
module "base" { source = "../../modules/base" }`,
		"envs/prod/main.tf":    "->common/base/main.source.tf",
		"envs/staging/main.tf": "->common/base/main.source.tf",
		"modules/base/main.tf": `variable "x" {}`,
	}, `{}`)

	if len(result.Projects) != 2 {
		t.Fatalf("expected two projects, got %d", len(result.Projects))
	}
	want := []string{"**", "../../common/base/**", "../../modules/base/**"}
	if got := result.Projects[0].Autoplan.WhenModified; !reflect.DeepEqual(got, want) {
		t.Errorf("when_modified = %v, want %v", got, want)
	}
}

// TestRenderWarnsOnDemotedProject covers the one failure mode the tool cannot
// repair on its own. A stale template that nothing symlinks is indistinguishable
// from a root module — Terraform files, a provider block, referenced by nobody —
// so its module paths get resolved where it sits rather than where its consumers
// sit. When such an invented path lands on a real root module, that project is
// silently demoted to a library and disappears from the output.
func TestRenderWarnsOnDemotedProject(t *testing.T) {
	// The template's source is written for a consumer two levels deeper; from
	// the template's own location it lands on a genuine root module instead.
	files := map[string]string{
		"templates/tenants/main.source.tf": `provider "aws" {}
module "net" { source = "../../envs/prod" }`,
		"envs/prod/main.tf": `provider "aws" {}`,
	}

	t.Run("without the hint the project vanishes, but loudly", func(t *testing.T) {
		result := renderTree(t, files, `{}`)

		var dirs []string
		for _, p := range result.Projects {
			dirs = append(dirs, p.Dir)
		}
		if want := []string{"templates/tenants"}; !reflect.DeepEqual(dirs, want) {
			t.Fatalf("projects = %v, want %v", dirs, want)
		}
		if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "envs/prod") {
			t.Errorf("expected a warning naming the demoted project, got %v", result.Warnings)
		}
	})

	// projects.exclude cannot fix this: it filters at render time, while the
	// damage is done during graph construction. Excluding the template leaves
	// the invented edge in place, so the real project stays demoted and the
	// output ends up empty.
	t.Run("projects.exclude does not fix it", func(t *testing.T) {
		result := renderTree(t, files, "projects:\n  exclude: [\"templates/**\"]\n")

		if len(result.Projects) != 0 {
			t.Errorf("expected exclude to leave no projects at all, got %d", len(result.Projects))
		}
	})

}

// TestRenderDoesNotWarnOnOrdinaryLibraryModules keeps the demotion warning
// quiet for the common, legitimate case: a library module that declares a
// provider block and lives outside the part of the tree reserved for projects.
func TestRenderDoesNotWarnOnOrdinaryLibraryModules(t *testing.T) {
	result := renderTree(t, map[string]string{
		"envs/prod/main.tf": `provider "aws" {}
module "lib" { source = "../../modules/kubernetes" }`,
		"modules/kubernetes/main.tf": `provider "helm" {}`,
	}, "projects:\n  include: [\"envs/**\"]\n")

	if len(result.Warnings) != 0 {
		t.Errorf("a library module with a provider block should not warn, got %v", result.Warnings)
	}
}

func TestRenderNoProjects(t *testing.T) {
	result := renderTree(t, map[string]string{
		"modules/lib/main.tf": `variable "x" {}`,
	}, `{}`)

	if len(result.Projects) != 0 {
		t.Fatalf("expected no projects, got %d", len(result.Projects))
	}
	// The key must still be present and be an empty list, not null, so that
	// Atlantis does not fall back to automatic project discovery.
	if !strings.Contains(string(result.YAML), "projects: []") {
		t.Errorf("expected an empty projects list, got:\n%s", result.YAML)
	}
}
