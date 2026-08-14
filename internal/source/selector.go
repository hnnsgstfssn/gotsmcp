package source

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Error is the package's error identity type.
type Error string

func (e Error) Error() string { return string(e) }

const (
	// ErrOutsideRoot reports a selector that escaped the project root.
	ErrOutsideRoot Error = "path is outside the project root"
	// ErrNoMatch reports a selector that matched no Go files.
	ErrNoMatch Error = "selector matched no Go files"
)

// Package is a resolved unit of Go source: the Go files in one directory.
type Package struct {
	ImportPath string   `json:"import_path,omitempty"`
	Name       string   `json:"name,omitempty"`
	Dir        string   `json:"dir"`
	Files      []string `json:"files"`
	// Ignored lists files excluded by build constraints for the current
	// GOOS/GOARCH/tags. They are reported so callers can warn that a
	// codebase-wide edit did not reach them; they are never edited.
	Ignored []string `json:"ignored,omitempty"`
}

// Resolve expands a selector into packages. Supported forms are a path to a
// single .go file, a directory, a Go package pattern ("./...", "all",
// "example.com/m/pkg/..."), or the empty string, which means "./...".
//
// Resolution prefers "go list" via [packages.Load] so that import paths and
// build-constraint exclusions are accurate. When that fails, which is the
// normal case for a module whose go.mod is broken, it falls back to walking the
// filesystem so that reads still work on a repository that cannot build.
func (l *Loader) Resolve(ctx context.Context, selector string) ([]Package, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		selector = "./..."
	}

	if strings.HasSuffix(selector, ".go") {
		path, err := l.contain(selector)
		if err != nil {
			return nil, err
		}
		return []Package{{
			Dir:   filepath.Dir(path),
			Name:  packageClause(path),
			Files: []string{path},
		}}, nil
	}

	pkgs, listErr := l.resolveList(ctx, selector)
	if listErr == nil && len(pkgs) > 0 {
		return pkgs, nil
	}

	pkgs, walkErr := l.resolveWalk(selector)
	switch {
	case walkErr != nil && listErr != nil:
		return nil, fmt.Errorf("resolve %q: go list: %v; filesystem walk: %w", selector, listErr, walkErr)
	case walkErr != nil:
		return nil, fmt.Errorf("resolve %q: %w", selector, walkErr)
	}
	return pkgs, nil
}

func (l *Loader) resolveList(ctx context.Context, selector string) ([]Package, error) {
	cfg := &packages.Config{
		Mode:    packages.NeedName | packages.NeedFiles,
		Dir:     l.root,
		Context: ctx,
		Tests:   true,
	}
	loaded, err := packages.Load(cfg, selector)
	if err != nil {
		return nil, err
	}

	// Tests:true yields up to three packages per directory (the package, its
	// test variant, and the external _test package). Merge them by directory.
	byDir := make(map[string]*Package, len(loaded))
	for _, p := range loaded {
		files := slices.Concat(p.GoFiles, p.OtherFiles, p.IgnoredFiles)
		if len(files) == 0 {
			continue
		}
		dir := filepath.Dir(files[0])
		pkg := byDir[dir]
		if pkg == nil {
			pkg = &Package{Dir: dir, Name: p.Name, ImportPath: cleanImportPath(p.PkgPath)}
			byDir[dir] = pkg
		}
		for _, f := range p.GoFiles {
			if abs, err := l.contain(f); err == nil {
				pkg.Files = append(pkg.Files, abs)
			}
		}
		for _, f := range p.IgnoredFiles {
			if !strings.HasSuffix(f, ".go") {
				continue
			}
			if abs, err := l.contain(f); err == nil {
				pkg.Ignored = append(pkg.Ignored, abs)
			}
		}
	}

	out := make([]Package, 0, len(byDir))
	for _, p := range byDir {
		p.Files = dedupe(p.Files)
		p.Ignored = dedupe(p.Ignored)
		if len(p.Files) > 0 || len(p.Ignored) > 0 {
			out = append(out, *p)
		}
	}
	slices.SortFunc(out, func(a, b Package) int { return strings.Compare(a.Dir, b.Dir) })
	return out, nil
}

// resolveWalk is the degraded path used when "go list" cannot run. It treats
// "..." as "everything below here" and ignores import-path semantics, since
// without a working module there is no import path to speak of.
func (l *Loader) resolveWalk(selector string) ([]Package, error) {
	base := strings.TrimSuffix(strings.TrimSuffix(selector, "..."), string(filepath.Separator))
	if base == "" || base == "all" {
		base = "."
	}
	recursive := strings.HasSuffix(selector, "...") || base == "."

	dir, err := l.contain(base)
	if err != nil {
		return nil, err
	}

	byDir := make(map[string][]string)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch {
			case path == dir:
				return nil
			case !recursive:
				return fs.SkipDir
			case skipDir(d.Name()):
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			byDir[filepath.Dir(path)] = append(byDir[filepath.Dir(path)], path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(byDir) == 0 {
		return nil, ErrNoMatch
	}

	out := make([]Package, 0, len(byDir))
	for d, files := range byDir {
		slices.Sort(files)
		out = append(out, Package{Dir: d, Name: packageClause(files[0]), Files: files})
	}
	slices.SortFunc(out, func(a, b Package) int { return strings.Compare(a.Dir, b.Dir) })
	return out, nil
}

// contain resolves path against the root and rejects anything that escapes it.
// Symlinks are evaluated first so that a root under /tmp on darwin, which is a
// symlink to /private/tmp, still contains its own files.
func (l *Loader) contain(path string) (string, error) {
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(l.root, abs)
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	rel, err := filepath.Rel(l.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s not under %s", ErrOutsideRoot, path, l.root)
	}
	return abs, nil
}

// packageClause reads just the package name, which parses even when the rest of
// the file does not.
func packageClause(path string) string {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly)
	if err != nil || f.Name == nil {
		return ""
	}
	return f.Name.Name
}

// cleanImportPath strips the "[...test]" suffix go list attaches to test variants.
func cleanImportPath(p string) string {
	base, _, _ := strings.Cut(p, " ")
	return base
}

func skipDir(name string) bool {
	return name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func dedupe(s []string) []string {
	slices.Sort(s)
	return slices.Compact(s)
}
