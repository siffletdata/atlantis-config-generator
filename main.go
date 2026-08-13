// Command atlantis-config-generator parses a Terraform/OpenTofu monorepo,
// builds the dependency graph between root modules and library modules, and
// writes an Atlantis repo-level configuration in which every root module's
// autoplan patterns cover its full transitive dependency set.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/siffletdata/atlantis-config-generator/internal/config"
	"github.com/siffletdata/atlantis-config-generator/internal/graph"
	"github.com/siffletdata/atlantis-config-generator/internal/render"
)

// version is set at build time by goreleaser.
var version = "dev"

// exitDrift is returned by --check when the target file is out of date. It is
// distinct from a general failure so CI can tell the two apart.
const exitDrift = 3

// errDrift signals that --check found a difference.
var errDrift = errors.New("generated configuration differs from the existing file")

const defaultConfigName = ".atlantis-generator.yaml"

type options struct {
	configPath  string
	outputPath  string
	check       bool
	debug       bool
	debugFormat string
	showVersion bool
	repoRoot    string
}

func main() {
	opts, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if opts.showVersion {
		fmt.Println(version)
		return
	}

	if err := run(opts, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errDrift) {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(exitDrift)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags(args []string, stderr io.Writer) (*options, error) {
	opts := &options{}

	fs := flag.NewFlagSet("atlantis-config-generator", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, `atlantis-config-generator generates an Atlantis repo-level configuration
from the module dependency graph of a Terraform/OpenTofu monorepo.

Usage:
  atlantis-config-generator [flags] <repo-root>

Flags:
`)
		fs.PrintDefaults()
	}

	stringVar := func(target *string, value, usage string, names ...string) {
		for _, name := range names {
			fs.StringVar(target, name, value, usage)
		}
	}
	boolVar := func(target *bool, usage string, names ...string) {
		for _, name := range names {
			fs.BoolVar(target, name, false, usage)
		}
	}

	stringVar(&opts.configPath, "", "config file (default: "+defaultConfigName+" in the repo root, if present)", "config", "c")
	stringVar(&opts.outputPath, "", "write to this file instead of stdout", "output", "o")
	stringVar(&opts.debugFormat, "text", "debug graph format: text or dot", "debug-format")
	boolVar(&opts.check, "exit "+fmt.Sprint(exitDrift)+" if the output file is out of date; write nothing", "check")
	boolVar(&opts.debug, "dump the dependency graph to stderr", "debug")
	boolVar(&opts.showVersion, "print the version and exit", "version", "v")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if opts.showVersion {
		return opts, nil
	}

	switch fs.NArg() {
	case 0:
		fs.Usage()
		return nil, errors.New("a repository root is required")
	case 1:
		opts.repoRoot = fs.Arg(0)
	default:
		return nil, fmt.Errorf("expected exactly one repository root, got %d", fs.NArg())
	}

	if opts.debugFormat != "text" && opts.debugFormat != "dot" {
		return nil, fmt.Errorf("invalid -debug-format %q: want text or dot", opts.debugFormat)
	}
	if opts.check && opts.outputPath == "" {
		return nil, errors.New("-check requires -output")
	}

	return opts, nil
}

func run(opts *options, stdout, stderr io.Writer) error {
	repoRoot, err := filepath.Abs(opts.repoRoot)
	if err != nil {
		return err
	}
	info, err := os.Stat(repoRoot)
	if err != nil {
		return fmt.Errorf("repository root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("repository root %s is not a directory", repoRoot)
	}

	cfg, err := loadConfig(opts.configPath, repoRoot)
	if err != nil {
		return err
	}

	g, err := graph.Build(graph.Options{Root: repoRoot, Exclude: cfg.ScanExclude()})
	if err != nil {
		return fmt.Errorf("build graph: %w", err)
	}
	for _, warning := range g.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", warning)
	}
	if cycles := g.Cycles(); len(cycles) > 0 {
		fmt.Fprintf(stderr, "warning: dependency cycle involving: %v\n", cycles)
	}

	if opts.debug {
		var err error
		if opts.debugFormat == "dot" {
			err = g.WriteDot(stderr)
		} else {
			err = g.WriteTree(stderr)
		}
		if err != nil {
			return err
		}
	}

	result, err := render.Render(render.Options{Config: cfg, Graph: g, RepoRoot: repoRoot})
	if err != nil {
		return err
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", warning)
	}
	if len(result.Projects) == 0 {
		fmt.Fprintln(stderr, "warning: no projects matched; check projects.include and projects.exclude")
	}

	if opts.check {
		existing, err := os.ReadFile(opts.outputPath)
		if err != nil {
			return fmt.Errorf("%w: cannot read %s: %v", errDrift, opts.outputPath, err)
		}
		if !bytes.Equal(existing, result.YAML) {
			return fmt.Errorf("%w: %s", errDrift, opts.outputPath)
		}
		return nil
	}

	if opts.outputPath != "" {
		return os.WriteFile(opts.outputPath, result.YAML, 0o644)
	}
	_, err = stdout.Write(result.YAML)
	return err
}

// loadConfig reads the configuration from an explicit path, or from the
// default file in the repository root when it exists. With neither, the
// built-in defaults apply.
func loadConfig(configPath, repoRoot string) (*config.Config, error) {
	if configPath != "" {
		return config.Load(configPath)
	}
	candidate := filepath.Join(repoRoot, defaultConfigName)
	if _, err := os.Stat(candidate); err == nil {
		return config.Load(candidate)
	}
	return config.Default(), nil
}
