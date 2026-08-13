package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tinyRepo writes the smallest repository that yields one project.
func tinyRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	dir := filepath.Join(root, "envs", "prod")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "provider \"aws\" {}\nmodule \"lib\" { source = \"../../modules/lib\" }\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(root, "modules", "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "main.tf"), []byte("variable \"x\" {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
		check   func(*testing.T, *options)
	}{
		{
			name: "long flags",
			args: []string{"--config", "cfg.yaml", "--output", "out.yaml", "--debug", "/repo"},
			check: func(t *testing.T, o *options) {
				if o.configPath != "cfg.yaml" || o.outputPath != "out.yaml" || !o.debug || o.repoRoot != "/repo" {
					t.Errorf("options = %+v", o)
				}
			},
		},
		{
			name: "short flags",
			args: []string{"-c", "cfg.yaml", "-o", "out.yaml", "/repo"},
			check: func(t *testing.T, o *options) {
				if o.configPath != "cfg.yaml" || o.outputPath != "out.yaml" {
					t.Errorf("options = %+v", o)
				}
			},
		},
		{
			name:    "no repo root",
			args:    nil,
			wantErr: "repository root is required",
		},
		{
			name:    "two repo roots",
			args:    []string{"/a", "/b"},
			wantErr: "expected exactly one repository root",
		},
		{
			name:    "bad debug format",
			args:    []string{"--debug-format", "svg", "/repo"},
			wantErr: "invalid -debug-format",
		},
		{
			name:    "check without output",
			args:    []string{"--check", "/repo"},
			wantErr: "-check requires -output",
		},
		{
			name: "version needs no repo root",
			args: []string{"--version"},
			check: func(t *testing.T, o *options) {
				if !o.showVersion {
					t.Error("showVersion not set")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseFlags(tc.args, io.Discard)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tc.check(t, opts)
		})
	}
}

func TestRunWritesToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(&options{repoRoot: tinyRepo(t)}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "dir: envs/prod") {
		t.Errorf("stdout = %s", stdout.String())
	}
}

func TestRunWritesToFile(t *testing.T) {
	root := tinyRepo(t)
	out := filepath.Join(t.TempDir(), "atlantis.yaml")

	var stdout, stderr bytes.Buffer
	if err := run(&options{repoRoot: root, outputPath: out}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty when writing to a file, got %q", stdout.String())
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "dir: envs/prod") {
		t.Errorf("file = %s", written)
	}
}

// TestRunCheckMode covers the three states of --check: in sync, drifted, and
// target missing. Drift must be distinguishable from a general failure so CI
// can report it as "the file is stale" rather than "the tool crashed".
func TestRunCheckMode(t *testing.T) {
	root := tinyRepo(t)
	out := filepath.Join(t.TempDir(), "atlantis.yaml")

	var stdout, stderr bytes.Buffer
	if err := run(&options{repoRoot: root, outputPath: out}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	t.Run("in sync", func(t *testing.T) {
		if err := run(&options{repoRoot: root, outputPath: out, check: true}, &stdout, &stderr); err != nil {
			t.Errorf("expected no drift, got %v", err)
		}
	})

	t.Run("drifted", func(t *testing.T) {
		if err := os.WriteFile(out, []byte("version: 3\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := run(&options{repoRoot: root, outputPath: out, check: true}, &stdout, &stderr)
		if !errors.Is(err, errDrift) {
			t.Errorf("error = %v, want errDrift", err)
		}
	})

	t.Run("does not rewrite the file", func(t *testing.T) {
		before, _ := os.ReadFile(out)
		_ = run(&options{repoRoot: root, outputPath: out, check: true}, &stdout, &stderr)
		after, _ := os.ReadFile(out)
		if !bytes.Equal(before, after) {
			t.Error("--check must not write")
		}
	})

	t.Run("missing target", func(t *testing.T) {
		err := run(&options{repoRoot: root, outputPath: filepath.Join(t.TempDir(), "nope.yaml"), check: true}, &stdout, &stderr)
		if !errors.Is(err, errDrift) {
			t.Errorf("error = %v, want errDrift", err)
		}
	})
}

func TestRunDebugGoesToStderr(t *testing.T) {
	root := tinyRepo(t)

	for _, format := range []string{"text", "dot"} {
		t.Run(format, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			opts := &options{repoRoot: root, debug: true, debugFormat: format}
			if err := run(opts, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			// Stdout must stay clean so the output can be piped.
			if strings.Contains(stdout.String(), "modules/lib") &&
				!strings.Contains(stdout.String(), "when_modified") {
				t.Error("debug output leaked into stdout")
			}
			if !strings.Contains(stderr.String(), "modules/lib") {
				t.Errorf("stderr missing the graph:\n%s", stderr.String())
			}
			if format == "dot" && !strings.Contains(stderr.String(), "digraph") {
				t.Errorf("dot output missing the digraph header:\n%s", stderr.String())
			}
		})
	}
}

func TestRunRejectsBadRepoRoot(t *testing.T) {
	var stdout, stderr bytes.Buffer

	t.Run("missing", func(t *testing.T) {
		err := run(&options{repoRoot: filepath.Join(t.TempDir(), "nope")}, &stdout, &stderr)
		if err == nil {
			t.Error("expected an error for a missing repository root")
		}
	})

	t.Run("not a directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file.txt")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := run(&options{repoRoot: file}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("error = %v", err)
		}
	})
}

// TestRunPicksUpDefaultConfig checks that the config file is found in the
// repository root without being named explicitly, which is what makes the
// pre-commit hook invocation short.
func TestRunPicksUpDefaultConfig(t *testing.T) {
	root := tinyRepo(t)
	config := "projects:\n  path_prefix: \"./\"\n  terraform_version: \"9.9.9\"\n"
	if err := os.WriteFile(filepath.Join(root, defaultConfigName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := run(&options{repoRoot: root}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "terraform_version: 9.9.9") {
		t.Errorf("default config was not applied:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "dir: ./envs/prod") {
		t.Errorf("path_prefix was not applied:\n%s", stdout.String())
	}
}

func TestRunWarnsWhenNoProjectsMatch(t *testing.T) {
	root := tinyRepo(t)
	config := "projects:\n  include: [\"nothing/**\"]\n"
	if err := os.WriteFile(filepath.Join(root, defaultConfigName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := run(&options{repoRoot: root}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "no projects matched") {
		t.Errorf("expected a warning, stderr = %q", stderr.String())
	}
}
