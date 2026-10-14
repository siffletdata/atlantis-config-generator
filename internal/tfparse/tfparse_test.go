package tfparse

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTree materialises a map of relative path to content under a new temp
// directory. A content value of the form "->target" creates a symlink instead.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	// Resolve the root itself, since t.TempDir sits behind /var -> /private/var
	// on macOS and the parser compares fully resolved paths.
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

func parse(t *testing.T, root, dir string) (*Dir, []string) {
	t.Helper()
	var warnings []string
	result, err := ParseDir(root, dir, func(format string, _ ...any) {
		warnings = append(warnings, format)
	})
	if err != nil {
		t.Fatalf("ParseDir(%s): %v", dir, err)
	}
	return result, warnings
}

func TestParseDirModuleSources(t *testing.T) {
	root := writeTree(t, map[string]string{
		"root/main.tf": `
module "local_relative" {
  source = "../modules/vpc"
}

module "local_double_slash" {
  source = "../modules//database"
}

module "local_dot" {
  source = "./nested"
}

module "registry" {
  source  = "terraform-aws-modules/s3-bucket/aws"
  version = "4.1.0"
}

module "registry_subdir" {
  source = "terraform-aws-modules/vpc/aws//modules/vpc-endpoints"
}

module "git" {
  source = "git::https://example.com/vpc.git?ref=v1.2.0"
}
`,
	})

	result, _ := parse(t, root, "root")

	want := map[string]string{
		"local_relative":     "../modules/vpc",
		"local_double_slash": "../modules//database",
		"local_dot":          "./nested",
		"registry":           "terraform-aws-modules/s3-bucket/aws",
		"registry_subdir":    "terraform-aws-modules/vpc/aws//modules/vpc-endpoints",
		"git":                "git::https://example.com/vpc.git?ref=v1.2.0",
	}
	if !reflect.DeepEqual(result.Modules, want) {
		t.Errorf("modules:\n got %#v\nwant %#v", result.Modules, want)
	}
}

// TestParseDirIgnoresNonModuleSources guards the parser against the many
// "source" arguments that are not module sources. Getting this wrong would
// flood the graph with hundreds of provider names.
func TestParseDirIgnoresNonModuleSources(t *testing.T) {
	root := writeTree(t, map[string]string{
		"root/providers.tf": `
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
    random = {
      source = "hashicorp/random"
    }
  }
}

provider "aws" {
  region = "eu-west-1"
}

data "archive_file" "lambda" {
  type        = "zip"
  source_dir  = "src"
  source      = var.file_source
}

resource "aws_cloudwatch_event_rule" "health" {
  event_pattern = jsonencode({ source = ["aws.health"] })
}
`,
	})

	result, _ := parse(t, root, "root")

	if len(result.Modules) != 0 {
		t.Errorf("expected no modules, got %#v", result.Modules)
	}
	if !result.HasProvider {
		t.Error("expected the provider block to be detected")
	}
}

func TestParseDirHasProvider(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "provider block",
			content: `provider "aws" { region = "eu-west-1" }`,
			want:    true,
		},
		{
			name: "required_providers only",
			content: `terraform {
  required_providers { aws = { source = "hashicorp/aws" } }
}`,
			want: false,
		},
		{
			name:    "library module with variables only",
			content: `variable "name" { type = string }`,
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, map[string]string{"dir/main.tf": tc.content})
			result, _ := parse(t, root, "dir")
			if result.HasProvider != tc.want {
				t.Errorf("HasProvider = %v, want %v", result.HasProvider, tc.want)
			}
		})
	}
}

// TestParseDirOverrideFiles covers Terraform's override semantics, which the
// repository relies on to correct a module path that a shared source file gets
// wrong for one particular consumer.
func TestParseDirOverrideFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"dir/main.tf": `
module "common" {
  source = "../../wrong/path"
}
module "untouched" {
  source = "../other"
}
`,
		"dir/module_path_override.tf": `
module "common" {
  source = "../../../correct/path"
}
`,
	})

	result, _ := parse(t, root, "dir")

	if got := result.Modules["common"]; got != "../../../correct/path" {
		t.Errorf("override did not win: got %q", got)
	}
	if got := result.Modules["untouched"]; got != "../other" {
		t.Errorf("unrelated module changed: got %q", got)
	}
}

func TestIsOverrideFile(t *testing.T) {
	tests := map[string]bool{
		"override.tf":             true,
		"override.tf.json":        true,
		"module_path_override.tf": true,
		"a_override.tofu":         true,
		"overrides.tf":            false,
		"main.tf":                 false,
		"override_something.tf":   false,
	}
	for name, want := range tests {
		if got := isOverrideFile(name); got != want {
			t.Errorf("isOverrideFile(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestParseDirMalformedIsNotFatal keeps a syntax error in one file from
// discarding the rest of the directory.
func TestParseDirMalformedIsNotFatal(t *testing.T) {
	root := writeTree(t, map[string]string{
		"dir/broken.tf": `module "x" { source = `,
		"dir/good.tf":   `module "good" { source = "../lib" }`,
	})

	result, warnings := parse(t, root, "dir")

	if len(warnings) == 0 {
		t.Error("expected a warning about the malformed file")
	}
	if got := result.Modules["good"]; got != "../lib" {
		t.Errorf("valid file was discarded: modules = %#v", result.Modules)
	}
}

func TestParseDirNonLiteralSourceWarns(t *testing.T) {
	root := writeTree(t, map[string]string{
		"dir/main.tf": `
module "dynamic" { source = var.module_path }
module "missing" { count = 1 }
`,
	})

	result, warnings := parse(t, root, "dir")

	if len(result.Modules) != 0 {
		t.Errorf("expected no usable modules, got %#v", result.Modules)
	}
	if len(warnings) < 2 {
		t.Errorf("expected a warning per unusable module, got %d", len(warnings))
	}
}

func TestParseDirSymlinks(t *testing.T) {
	root := writeTree(t, map[string]string{
		"common/main.source.tf": `provider "aws" {}
module "lib" { source = "../../modules/lib" }`,
		"envs/prod/main.tf":  "->common/main.source.tf",
		"envs/prod/vars.tf":  `variable "x" {}`,
		"modules/lib/lib.tf": `variable "y" {}`,
	})

	result, _ := parse(t, root, "envs/prod")

	if want := []string{"common/main.source.tf"}; !reflect.DeepEqual(result.Symlinks, want) {
		t.Errorf("Symlinks = %#v, want %#v", result.Symlinks, want)
	}
	// The symlinked file's content belongs to the directory that links it, so
	// its provider block and module call are attributed here.
	if !result.HasProvider {
		t.Error("provider block from the symlinked file was not seen")
	}
	if got := result.Modules["lib"]; got != "../../modules/lib" {
		t.Errorf("module from the symlinked file: got %q", got)
	}
}

func TestParseDirIgnoresNonTerraformFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"dir/main.tf":     `module "a" { source = "../a" }`,
		"dir/values.yaml": "module: not-terraform",
		"dir/README.md":   "# docs",
		"dir/main.tfvars": `x = "y"`,
	})

	result, warnings := parse(t, root, "dir")

	if len(result.Modules) != 1 {
		t.Errorf("expected exactly one module, got %#v", result.Modules)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

func TestParseDirTofuAndJSON(t *testing.T) {
	root := writeTree(t, map[string]string{
		"dir/main.tofu":   `module "tofu" { source = "../tofu-lib" }`,
		"dir/gen.tf.json": `{"module": {"json": {"source": "../json-lib"}}}`,
	})

	result, warnings := parse(t, root, "dir")

	if got := result.Modules["tofu"]; got != "../tofu-lib" {
		t.Errorf(".tofu module: got %q, warnings %v", got, warnings)
	}
	if got := result.Modules["json"]; got != "../json-lib" {
		t.Errorf(".tf.json module: got %q, warnings %v", got, warnings)
	}
}

func TestParseDirFileRefs(t *testing.T) {
	root := writeTree(t, map[string]string{
		"mod/main.tf": `
resource "kubernetes_manifest" "crd" {
  manifest = yamldecode(file(
    "${path.module}/../../tools/crd.yml")
  )
}

locals {
  values  = templatefile("${path.module}/templates/values.tpl", { x = 1 })
  plain   = file("../shared/plain.txt")
  hash    = filesha256("${path.module}/../shared/script.sh")
  core    = core::file("${path.module}/../shared/core.txt")
  dynamic = file(var.path)
  root    = file("${path.root}/x")
  abs     = file("/etc/hosts")
  globbed = fileset("${path.module}/../policies", "*.json")
  braces  = fileset("${path.module}/../braces", "{a,b}.json")
  again   = file("${path.module}/../../tools/crd.yml")
}
`,
		"mod/data.tf.json": `{"locals": {"json": "${file(\"../json/ignored.txt\")}"}}`,
	})

	result, _ := parse(t, root, "mod")

	want := []string{
		"../../tools/crd.yml",
		"../braces/**",
		"../policies/*.json",
		"../shared/core.txt",
		"../shared/plain.txt",
		"../shared/script.sh",
		"templates/values.tpl",
	}
	if !reflect.DeepEqual(result.FileRefs, want) {
		t.Errorf("file refs:\n got %#v\nwant %#v", result.FileRefs, want)
	}
}
