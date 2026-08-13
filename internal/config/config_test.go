package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"**"}; !reflect.DeepEqual(cfg.Projects.WhenModified.Self, want) {
		t.Errorf("self patterns = %v, want %v", cfg.Projects.WhenModified.Self, want)
	}
	if want := []string{"**"}; !reflect.DeepEqual(cfg.Projects.WhenModified.Dependency, want) {
		t.Errorf("dependency patterns = %v, want %v", cfg.Projects.WhenModified.Dependency, want)
	}
	if cfg.Output["version"] != 3 {
		t.Errorf("version = %v, want 3", cfg.Output["version"])
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte("projcts:\n  include: []\n"))
	if err == nil {
		t.Fatal("expected a typo in a top-level key to be rejected")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "invalid regex",
			yaml:    "rules:\n  - match: '['\n",
			wantErr: "invalid match",
		},
		{
			name:    "missing match",
			yaml:    "rules:\n  - workflow: default\n",
			wantErr: "match is required",
		},
		{
			name: "workflow not defined",
			yaml: `
rules:
  - match: '.*'
    workflow: nope
output:
  workflows:
    default: {}
`,
			wantErr: `workflow "nope" is not defined`,
		},
		{
			name: "workflow defined",
			yaml: `
rules:
  - match: '.*'
    workflow: default
output:
  workflows:
    default: {}
`,
		},
		{
			// Workflows may come from the Atlantis server-side config, so a
			// rule naming one is only checked when the block is present.
			name: "workflow unchecked without a workflows block",
			yaml: "rules:\n  - match: '.*'\n    workflow: server_side\n",
		},
		{
			name:    "bad version",
			yaml:    "output:\n  version: 2\n",
			wantErr: "output.version must be 3",
		},
		{
			name:    "invalid glob",
			yaml:    "scan:\n  exclude: ['[']\n",
			wantErr: "invalid glob pattern",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected an error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestMatchRuleFirstWins(t *testing.T) {
	cfg, err := Parse([]byte(`
rules:
  - match: '/clusters/[^/]+/base$'
    workflow: cluster_base
  - match: '/clusters/'
    workflow: cluster_other
  - match: '.*'
    workflow: default
`))
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]string{
		"env/clusters/prod-a/base":       "cluster_base",
		"env/clusters/prod-a/kubernetes": "cluster_other",
		"env/global/thing":               "default",
	}
	for dir, want := range tests {
		rule := cfg.MatchRule(dir)
		if rule == nil {
			t.Fatalf("%s: no rule matched", dir)
		}
		if rule.Workflow != want {
			t.Errorf("%s: workflow = %q, want %q", dir, rule.Workflow, want)
		}
	}
}

func TestMatchRuleNoMatch(t *testing.T) {
	cfg, err := Parse([]byte("rules:\n  - match: 'never'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rule := cfg.MatchRule("something"); rule != nil {
		t.Errorf("expected no match, got %+v", rule)
	}
}

func TestResolveTerraformVersion(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".opentofu-version"), []byte("1.11.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("from file", func(t *testing.T) {
		cfg, _ := Parse([]byte("projects:\n  terraform_version_file: .opentofu-version\n"))
		got, err := cfg.ResolveTerraformVersion(root)
		if err != nil {
			t.Fatal(err)
		}
		if got != "1.11.1" {
			t.Errorf("version = %q, want 1.11.1", got)
		}
	})

	t.Run("literal wins over file", func(t *testing.T) {
		cfg, _ := Parse([]byte("projects:\n  terraform_version: 1.9.0\n  terraform_version_file: .opentofu-version\n"))
		got, _ := cfg.ResolveTerraformVersion(root)
		if got != "1.9.0" {
			t.Errorf("version = %q, want 1.9.0", got)
		}
	})

	t.Run("neither configured", func(t *testing.T) {
		cfg, _ := Parse([]byte(`{}`))
		got, err := cfg.ResolveTerraformVersion(root)
		if err != nil || got != "" {
			t.Errorf("got %q, %v; want empty and no error", got, err)
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		cfg, _ := Parse([]byte("projects:\n  terraform_version_file: nope\n"))
		if _, err := cfg.ResolveTerraformVersion(root); err == nil {
			t.Error("expected an error for a missing version file")
		}
	})
}

func TestGlobsMatch(t *testing.T) {
	tests := []struct {
		patterns []string
		path     string
		want     bool
	}{
		{[]string{"infra/environments/**"}, "infra/environments/clusters/a/base", true},
		{[]string{"infra/environments/**"}, "infra/modules/vpc", false},
		// A pattern naming a directory covers everything beneath it, so a
		// blocked subtree does not need a trailing wildcard.
		{[]string{"**/argocd"}, "env/global/aws/1/argocd", true},
		{[]string{"**/argocd"}, "env/global/aws/1/argocd/nested", true},
		{[]string{"**/azure/**"}, "env/global/azure/sub/thing", true},
		{[]string{"**/azure/**"}, "env/global/aws/thing", false},
		{nil, "anything", false},
	}

	for _, tc := range tests {
		if got := Globs(tc.patterns).Match(tc.path); got != tc.want {
			t.Errorf("Globs(%v).Match(%q) = %v, want %v", tc.patterns, tc.path, got, tc.want)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("expected an error for a missing config file")
	}
}
