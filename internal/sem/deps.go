package sem

import (
	"context"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// PackageDeps is the import relation for one package.
type PackageDeps struct {
	Package    string   `json:"package"`
	Dir        string   `json:"dir,omitempty"`
	Imports    []string `json:"imports,omitempty"`
	ImportedBy []string `json:"imported_by,omitempty"`
	// TestImports are reached only from _test.go files. Folding them into
	// Imports made a test-only dependency look like a layering violation.
	TestImports []string `json:"test_imports,omitempty"`
	// StdlibImports is separated out because it is rarely what a refactor
	// cares about and would otherwise dominate the list.
	StdlibImports []string `json:"stdlib_imports,omitempty"`
}

// DepsResult is the outcome of a dependency query.
type DepsResult struct {
	Packages []PackageDeps `json:"packages"`
	Scanned  int           `json:"scanned"`
}

// Deps reports what a package imports and what imports it.
//
// The reverse direction is the useful one and the one that is awkward to get
// any other way: before changing a package's API you want to know who depends
// on it, and that relation is not written down anywhere in the package itself.
//
// It loads names and imports only, not types, so it is far cheaper than the
// rest of this package and works on a module that does not fully compile.
func Deps(ctx context.Context, root, target, scope string) (*DepsResult, error) {
	if strings.TrimSpace(scope) == "" {
		scope = "./..."
	}
	cfg := &packages.Config{
		Mode:    packages.NeedName | packages.NeedFiles | packages.NeedImports,
		Dir:     root,
		Context: ctx,
		Tests:   true,
	}
	pkgs, err := packages.Load(cfg, scope)
	if err != nil {
		return nil, err
	}

	// Build the forward relation over the scope, then invert it.
	imports := make(map[string][]string)
	testImports := make(map[string][]string)
	dirs := make(map[string]string)
	reverse := make(map[string][]string)
	seen := make(map[string]bool)

	for _, p := range pkgs {
		path := cleanPath(p.PkgPath)
		if path == "" || strings.HasSuffix(path, ".test") {
			continue
		}
		// go list reports a package, its test variant, and its external test
		// package under names that differ only by suffix. Attribute all three
		// to the real package, but keep test-only imports separate: an import
		// reached solely from a _test.go file is not a dependency of the
		// package, and reporting it as one makes it look like a layering
		// problem that is not there.
		base, isTest := strings.CutSuffix(path, "_test")
		if !isTest {
			base = path
		}
		isTestVariant := isTest || p.ID != p.PkgPath

		if !seen[base] && !isTest {
			seen[base] = true
			if len(p.GoFiles) > 0 {
				dirs[base] = p.GoFiles[0]
			}
		}
		for dep := range p.Imports {
			dep = cleanPath(dep)
			if strings.HasSuffix(dep, ".test") {
				continue
			}
			if isTestVariant {
				testImports[base] = append(testImports[base], dep)
				continue
			}
			imports[base] = append(imports[base], dep)
			reverse[dep] = append(reverse[dep], base)
		}
	}

	match := targetMatcher(target)
	res := &DepsResult{Scanned: len(seen)}
	for path := range seen {
		if !match(path) {
			continue
		}
		d := PackageDeps{Package: path, Dir: dirs[path]}
		direct := dedupeStrings(imports[path])
		for _, dep := range direct {
			if isStdlib(dep) {
				d.StdlibImports = append(d.StdlibImports, dep)
			} else {
				d.Imports = append(d.Imports, dep)
			}
		}
		for _, dep := range dedupeStrings(testImports[path]) {
			if isStdlib(dep) || slices.Contains(direct, dep) || dep == path {
				continue
			}
			d.TestImports = append(d.TestImports, dep)
		}
		d.ImportedBy = dedupeStrings(reverse[path])
		res.Packages = append(res.Packages, d)
	}
	slices.SortFunc(res.Packages, func(a, b PackageDeps) int {
		return strings.Compare(a.Package, b.Package)
	})
	return res, nil
}

// targetMatcher accepts an exact import path, a /... prefix pattern, a bare
// package name, or empty meaning everything in scope.
func targetMatcher(target string) func(string) bool {
	target = strings.TrimSpace(target)
	switch {
	case target == "" || target == "./..." || target == "all":
		return func(string) bool { return true }
	case strings.HasSuffix(target, "/..."):
		prefix := strings.TrimSuffix(target, "/...")
		return func(p string) bool { return p == prefix || strings.HasPrefix(p, prefix+"/") }
	default:
		return func(p string) bool {
			return p == target ||
				strings.HasSuffix(p, "/"+target) ||
				p[strings.LastIndex(p, "/")+1:] == target
		}
	}
}

// isStdlib reports whether an import path belongs to the standard library.
//
// The standard library is exactly the set of import paths whose first segment
// contains no dot: a domain name is what makes a path external.
func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

func dedupeStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := slices.Clone(s)
	slices.Sort(out)
	return slices.Compact(out)
}
