_Disclaimer and warning: this project was almost entirely written by a coding agent, with very little human review._

# atlantis-config-generator

Generate an [Atlantis](https://www.runatlantis.io/) repo-level configuration from the
module dependency graph of a Terraform/OpenTofu monorepo.

Atlantis decides which projects to plan by matching a pull request's changed files against
each project's `autoplan.when_modified` patterns. Maintaining those patterns by hand does
not scale: to detect when a root module calls a library module that calls another library module,
the `when_modified` key needs to be manually maintained with the whole dependency chain.


This tool builds the module dependency graph and outputs an Atlantis config file,
so that a single `atlantis plan` covers every root module a change actually affects, however deep
the change is.

## Install

```console
go install github.com/siffletdata/atlantis-config-generator@latest
```

Or download a binary from the [releases page](https://github.com/siffletdata/atlantis-config-generator/releases).

## Usage

```console
atlantis-config-generator [flags] <repo-root>
```

| Flag | Description |
| --- | --- |
| `-c`, `--config PATH` | Configuration file. Defaults to `.atlantis-generator.yaml` in the repo root, if present. |
| `-o`, `--output PATH` | Write to a file instead of stdout. |
| `--check` | Exit with code `3` if the file at `--output` differs from what would be generated. Writes nothing. |
| `--debug` | Dump the dependency graph to stderr. |
| `--debug-format` | `text` (default) or `dot` for Graphviz. |
| `-v`, `--version` | Print the version. |

Exit codes: `0` success, `1` error, `3` `--check` found drift.

```console
# Print to stdout
atlantis-config-generator .

# Regenerate in place
atlantis-config-generator -c examples/monorepo.yaml -o atlantis.yaml .

# Inspect the graph
atlantis-config-generator --debug . > /dev/null

# Render the graph as a picture
atlantis-config-generator --debug --debug-format dot . 2>&1 >/dev/null | dot -Tsvg > graph.svg
```

## How projects are found

A directory becomes an Atlantis project when all of the following hold:

1. it contains at least one `.tf`, `.tf.json`, `.tofu` or `.tofu.json` file;
2. it declares at least one `provider` block — a root module configures providers, a
   library module should not;
3. nothing else in the repository references it as a module `source` — being called by
   another module makes it a library, not a root;
4. it passes `projects.include` and `projects.exclude` (config values, see below).

Everything else in the repository is still scanned, because a library module that is not a
project can still be a *dependency* of one.

Only `module` and `provider` blocks, and calls to file-reading functions (see below), are
parsed. Everything else in the Terraform configuration is ignored.

## How `when_modified` is built

For each project, the generated patterns are:

- the `when_modified.self` patterns; then
- for every directory in the project's **full transitive dependency closure**, the
  `when_modified.dependency` patterns prefixed with the relative path to that directory;
- finally, every file read from outside a module (see
  [below](#files-read-outside-a-module)).

With the defaults (`["**"]` for both) a project that depends on `modules/a`, which depends
on `modules/b`, gets:

```yaml
autoplan:
  when_modified:
    - '**'
    - ../../modules/a/**
    - ../../modules/b/**
```

Atlantis joins each pattern onto the project directory and matches with Docker's
`.dockerignore` syntax, so `../../modules/b/**` resolves to `modules/b/**` against the
repository root.

### Files read outside a module

A module that reads a file from elsewhere in the repository depends on that file:

```hcl
manifest = yamldecode(file("${path.module}/../../../tools/operator/crd.yml"))
```

Calls to `file`, `templatefile`, `filebase64`, `filemd5`, `filesha1`, `filesha256`,
`filesha512`, `filebase64sha256`, `filebase64sha512` and `fileset` are found anywhere in a
module. When the path points outside the module's directory, it is added, relative to
the project directory, to the `when_modified` of every project that has the module in its
dependencies:

```yaml
    - ../../../tools/operator/crd.yml
```

`fileset(dir, pattern)` contributes `dir/pattern` (or `dir/**` when the pattern uses
`{a,b}` alternatives, which Atlantis's matcher does not support).

#### Limitations

Only paths that can be evaluated statically are followed: string literals and templates
whose only variable is `path.module`. Paths built from variables, locals, `path.root` or
`path.cwd`, or other functions are skipped, as are `.tf.json` files and paths
outside the repository. A path without `path.module` is resolved against the module's own
directory, which is what Terraform does for a root module. A referenced file that does not
exist is reported as a warning and skipped.

## Configuration

Every key is optional. Without a configuration file the tool scans everything, treats every
root module as a project, and triggers on any change in the project or its dependencies.

```yaml
# Prepended to the output, one "# " per line.
header_comment: |
  DO NOT EDIT.
  This file is generated by atlantis-config-generator. Edits will be lost.

scan:
  # Directories not to scan at all. They neither become projects nor contribute
  # dependency edges. ".terraform" and ".git" are always skipped.
  exclude:
    - "infra/vendor/**"

projects:
  # When set, only directories matching one of these can become projects.
  # Directories outside it are still scanned, so they can still be depended on.
  include: ["infra/environments/**"]

  # Removes directories that would otherwise qualify. A pattern naming a
  # directory also covers everything beneath it.
  exclude:
    - "**/000000000000/**"
    - "**/argocd"

  # Prepended to the emitted "dir" and "name", so the output can read
  # "./infra/..." rather than "infra/...".
  path_prefix: "./"

  # Emitted on every project. Use terraform_version_file to read it from a file
  # in the repository instead; the literal takes precedence.
  terraform_version: "1.11.1"
  terraform_version_file: "infra/.opentofu-version"

  when_modified:
    self: ["**"]         # patterns for the project's own directory
    dependency: ["**"]   # patterns for each transitive dependency directory

# Project configuration override rules.
# Ordered; the first rule whose regex matches the repo-relative project
# directory wins. The regex is matched against the path without path_prefix.
rules:
  - match: '/clusters/[^/]+/base$'
    workflow: cluster_base
    extra_when_modified: ["*.s3.tfbackend"]
    execution_order_group: 0
    terraform_version: "1.9.0"
    workspace: "default"
    autoplan_enabled: true
  - match: '.*'
    workflow: default

# Copied verbatim into the generated file. This is where the top-level Atlantis
# keys and the whole workflows block live; the tool only generates "projects".
output:
  version: 3
  parallel_plan: true
  parallel_apply: true
  workflows:
    default:
      apply:
        steps:
          - apply
    cluster_base:
      plan:
        steps:
          - init:
              extra_args: ["-backend-config=cluster.s3.tfbackend"]
          - plan
```

### Rule keys

| Key | Effect |
| --- | --- |
| `match` | **Required.** Regex tested against the repo-relative project directory. |
| `workflow` | Sets the project's `workflow`. Validated against `output.workflows` when that block is present. |
| `terraform_version` | Overrides `projects.terraform_version` for matching projects. |
| `workspace` | Sets the project's Terraform workspace. |
| `execution_order_group` | Sets `execution_order_group`. |
| `autoplan_enabled` | Sets `autoplan.enabled`. Disabling it only suppresses the automatic plan; an explicit `atlantis plan` still runs. |
| `extra_when_modified` | Appended to the computed patterns. |

## Symlinked shared sources

One way to keep root modules DRY is to write one template and symlink it into every
environment:

```
infra/templates/clusters/base/main.source.tf
infra/environments/clusters/prod-a/base/main.tf -> ../../../../templates/clusters/base/main.source.tf
```

This tool handles this pattern:

1. **A change to the template must plan every consumer.** Git records the change on
   `main.source.tf`, not on the symlink, so the template's directory is added as a
   dependency of every directory that links it.
2. **The template's module paths belong to the consumer.** Terraform resolves a relative
   `source` against the directory it runs in, so `../../../../modules/foo` in
   `main.source.tf` is written for the environment directory, not for `templates/`.

Terraform's [override file](https://developer.hashicorp.com/terraform/language/files/override)
semantics are applied too, so a consumer at a different nesting depth can correct a path
with `module_path_override.tf`.

## Pre-commit

This repo also defines two hooks, to be used with e.g [pre-commit](https://pre-commit.com/) or
[prek](https://github.com/j178/prek).

Use `atlantis-config` to regenerate the file, or
`atlantis-config-check` in CI to fail when it is stale:

```yaml
repos:
  - repo: https://github.com/siffletdata/atlantis-config-generator
    rev: v0.1.0
    hooks:
      - id: atlantis-config
        args: ["-c", ".atlantis-generator.yaml", "-o", "atlantis.yaml", "."]
        files: '(infra/.*)|(atlantis\.yaml)'
```

## Development

```console
go test -race ./...          # unit and integration tests
go test . -update            # refresh the golden files under testdata/
golangci-lint run
goreleaser release --snapshot --clean
```

Integration tests build a synthetic repo in a temp directory, run the CLI end to end,
and compare against a golden file. One test goes further and validates the generated
patterns against `github.com/moby/patternmatcher` — the same matcher Atlantis uses — by
reproducing the transformation Atlantis applies and asserting that a change deep in the
dependency graph really does select the project.
