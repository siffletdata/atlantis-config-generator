// Package tfparse extracts the minimum amount of information the dependency
// graph needs out of a Terraform/OpenTofu directory: which modules it calls,
// whether it declares any providers, which of its files are symlinks, and which
// files it reads through functions such as file() and templatefile().
//
// It deliberately understands only "module" and "provider" blocks, plus calls
// to the file-reading functions wherever they appear. Everything else in the
// configuration is ignored, which keeps the parser robust against syntax it
// has no opinion about.
package tfparse

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// Dir is the parse result for a single directory.
type Dir struct {
	// Path is the directory, relative to the repository root.
	Path string
	// Modules maps a module block's label to its raw source string, with
	// override files already applied.
	Modules map[string]string
	// HasProvider reports whether the directory declares at least one
	// provider block. Root modules do; well-behaved library modules do not.
	HasProvider bool
	// Symlinks holds the resolved targets of any symlinked files directly in
	// the directory, relative to the repository root. A change to the target
	// is a change to this directory.
	Symlinks []string
	// FileRefs holds the paths read by file-reading function calls, relative
	// to the module directory, sorted. A fileset() call contributes its path
	// and pattern joined, so an entry may be a glob. Only arguments that can
	// be evaluated statically, with path.module as the sole variable, are
	// recorded.
	FileRefs []string
}

// ModuleLabels returns the module labels in a stable order.
func (d *Dir) ModuleLabels() []string {
	labels := make([]string, 0, len(d.Modules))
	for label := range d.Modules {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels
}

// Warner receives non-fatal problems encountered while parsing. Parsing never
// fails on malformed configuration: a file we cannot read contributes nothing
// and the walk continues.
type Warner func(format string, args ...any)

// bodySchema matches the two block types we care about. Using a partial schema
// means unknown top-level blocks are returned as leftovers rather than errors.
var bodySchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "module", LabelNames: []string{"name"}},
		{Type: "provider", LabelNames: []string{"name"}},
	},
}

// moduleSchema reads only the source argument of a module block. Every other
// argument and nested block is left over and ignored, which is what keeps this
// from tripping over provider aliases, depends_on, count, and so on.
var moduleSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{{Name: "source"}},
}

// ParseDir parses the Terraform files directly inside dir (no recursion).
//
// fsRoot is the repository root; dir is relative to it. Both returned module
// sources and symlink targets are left for the caller to resolve, because
// Terraform resolves relative module sources against the directory it is run
// in, which is not necessarily where the file physically lives.
func ParseDir(fsRoot, dir string, warn Warner) (*Dir, error) {
	if warn == nil {
		warn = func(string, ...any) {}
	}

	abs := filepath.Join(fsRoot, dir)
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}

	result := &Dir{Path: dir, Modules: map[string]string{}}

	// Terraform applies override files after all regular files, so collect the
	// two sets separately and parse the regular ones first.
	var regular, overrides []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !isTerraformFile(name) {
			continue
		}
		if isOverrideFile(name) {
			overrides = append(overrides, name)
		} else {
			regular = append(regular, name)
		}
	}
	sort.Strings(regular)
	sort.Strings(overrides)

	parser := hclparse.NewParser()
	for _, name := range append(regular, overrides...) {
		parseFile(parser, filepath.Join(abs, name), filepath.Join(dir, name), result, warn)
	}

	result.Symlinks = SymlinkTargets(fsRoot, dir, warn)
	sort.Strings(result.FileRefs)
	result.FileRefs = slices.Compact(result.FileRefs)

	return result, nil
}

// SymlinkTargets returns the repository-relative targets of the symlinked
// files directly inside dir, sorted.
//
// A symlinked file makes the directory depend on wherever the link points: the
// link itself never changes in a diff, its target does.
func SymlinkTargets(fsRoot, dir string, warn Warner) []string {
	if warn == nil {
		warn = func(string, ...any) {}
	}

	abs := filepath.Join(fsRoot, dir)
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil
	}

	var targets []string
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		target, err := filepath.EvalSymlinks(filepath.Join(abs, entry.Name()))
		if err != nil {
			warn("%s: cannot resolve symlink: %v", filepath.Join(dir, entry.Name()), err)
			continue
		}
		rel, err := relativeTo(fsRoot, target)
		if err != nil {
			// Points outside the repository; nothing in a diff can match it.
			continue
		}
		targets = append(targets, rel)
	}
	sort.Strings(targets)
	return targets
}

// parseFile merges one file's module and provider blocks into result.
// Diagnostics are reported as warnings; HCL still hands back whatever it could
// parse, so a single malformed block does not discard the rest of the file.
func parseFile(parser *hclparse.Parser, abs, rel string, result *Dir, warn Warner) {
	var file *hcl.File
	var diags hcl.Diagnostics
	if strings.HasSuffix(abs, ".json") {
		file, diags = parser.ParseJSONFile(abs)
	} else {
		file, diags = parser.ParseHCLFile(abs)
	}
	if diags.HasErrors() {
		warn("%s: %s", rel, diags.Error())
	}
	if file == nil || file.Body == nil {
		return
	}

	// JSON configuration embeds expressions in plain strings, which are not
	// worth interpreting here; only native syntax is searched for file reads.
	if body, ok := file.Body.(*hclsyntax.Body); ok {
		result.FileRefs = append(result.FileRefs, fileRefs(body)...)
	}

	content, _, diags := file.Body.PartialContent(bodySchema)
	if diags.HasErrors() {
		warn("%s: %s", rel, diags.Error())
	}
	if content == nil {
		return
	}

	for _, block := range content.Blocks {
		switch block.Type {
		case "provider":
			result.HasProvider = true
		case "module":
			label := block.Labels[0]
			source, ok := moduleSource(block, rel, warn)
			if !ok {
				continue
			}
			result.Modules[label] = source
		}
	}
}

// moduleSource extracts the literal source string of a module block. Terraform
// requires this to be a literal, so anything we cannot evaluate statically is
// reported and skipped rather than guessed at.
func moduleSource(block *hcl.Block, rel string, warn Warner) (string, bool) {
	content, _, diags := block.Body.PartialContent(moduleSchema)
	if diags.HasErrors() {
		warn("%s: module %q: %s", rel, block.Labels[0], diags.Error())
	}
	if content == nil {
		return "", false
	}
	attr, ok := content.Attributes["source"]
	if !ok {
		warn("%s: module %q has no source", rel, block.Labels[0])
		return "", false
	}
	value, diags := attr.Expr.Value(nil)
	if diags.HasErrors() || value.IsNull() || !value.IsKnown() || value.Type() != cty.String {
		warn("%s: module %q: source is not a literal string", rel, block.Labels[0])
		return "", false
	}
	return value.AsString(), true
}

// fileFunctions are the functions whose first argument is a path the module
// reads. fileset is handled separately, since it takes a pattern as well.
var fileFunctions = map[string]bool{
	"file":             true,
	"filebase64":       true,
	"filebase64sha256": true,
	"filebase64sha512": true,
	"filemd5":          true,
	"filesha1":         true,
	"filesha256":       true,
	"filesha512":       true,
	"templatefile":     true,
}

// pathModuleContext evaluates path arguments with path.module set to the
// module directory itself, so "${path.module}/../x" comes out as "./../x".
// path.root and path.cwd are left undefined: they name the root module the
// configuration is eventually run from, which a library module cannot know.
var pathModuleContext = &hcl.EvalContext{
	Variables: map[string]cty.Value{
		"path": cty.ObjectVal(map[string]cty.Value{"module": cty.StringVal(".")}),
	},
}

// fileRefs returns the paths read by file-reading function calls anywhere in
// body, relative to the module directory. Calls whose path depends on
// anything other than path.module, such as a variable, cannot be resolved
// statically and are skipped: they are routine, so reporting them would be
// noise.
func fileRefs(body *hclsyntax.Body) []string {
	var refs []string
	_ = hclsyntax.VisitAll(body, func(node hclsyntax.Node) hcl.Diagnostics {
		call, ok := node.(*hclsyntax.FunctionCallExpr)
		if !ok {
			return nil
		}
		name := strings.TrimPrefix(call.Name, "core::")
		switch {
		case fileFunctions[name] && len(call.Args) >= 1:
			if ref, ok := staticPath(call.Args[0]); ok {
				refs = append(refs, ref)
			}
		case name == "fileset" && len(call.Args) == 2:
			dir, ok := staticPath(call.Args[0])
			if !ok {
				return nil
			}
			pattern, ok := staticPath(call.Args[1])
			// Brace alternatives have no equivalent in the .dockerignore
			// syntax Atlantis matches with; fall back to the whole directory.
			if !ok || strings.ContainsAny(pattern, "{}") {
				pattern = "**"
			}
			refs = append(refs, path.Join(dir, pattern))
		}
		return nil
	})
	return refs
}

// staticPath evaluates expr as a relative path, or reports false when it
// depends on more than path.module or is absolute.
func staticPath(expr hclsyntax.Expression) (string, bool) {
	value, diags := expr.Value(pathModuleContext)
	if diags.HasErrors() || value.IsNull() || !value.IsKnown() || value.Type() != cty.String {
		return "", false
	}
	ref := filepath.ToSlash(value.AsString())
	if ref == "" || path.IsAbs(ref) || filepath.IsAbs(ref) {
		return "", false
	}
	return path.Clean(ref), true
}

func isTerraformFile(name string) bool {
	switch {
	case strings.HasSuffix(name, ".tf"):
		return true
	case strings.HasSuffix(name, ".tf.json"):
		return true
	case strings.HasSuffix(name, ".tofu"):
		return true
	case strings.HasSuffix(name, ".tofu.json"):
		return true
	default:
		return false
	}
}

// isOverrideFile reports whether name has Terraform's override semantics:
// "override.tf" itself, or anything ending in "_override.tf".
func isOverrideFile(name string) bool {
	base := name
	for _, suffix := range []string{".tf.json", ".tofu.json", ".tf", ".tofu"} {
		if strings.HasSuffix(name, suffix) {
			base = strings.TrimSuffix(name, suffix)
			break
		}
	}
	return base == "override" || strings.HasSuffix(base, "_override")
}

// relativeTo returns target relative to root, failing if target escapes root.
func relativeTo(root, target string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	// The root may itself sit behind a symlink (/tmp on macOS, for one), so
	// compare fully resolved paths.
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolved
	}
	rel, err := filepath.Rel(absRoot, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside %s", target, absRoot)
	}
	return filepath.ToSlash(rel), nil
}
