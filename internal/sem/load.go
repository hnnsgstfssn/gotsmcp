package sem

import (
	"context"
	"fmt"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
)

// Error is the package's error identity type.
type Error string

func (e Error) Error() string { return string(e) }

const (
	// ErrNotFound reports a symbol that does not resolve.
	ErrNotFound Error = "symbol not found"
	// ErrAmbiguous reports a symbol spec matching more than one object.
	ErrAmbiguous Error = "symbol is ambiguous"
	// ErrConflict reports a rename that the type checker says is unsafe.
	ErrConflict Error = "rename conflicts"
	// ErrInvalidName reports a proposed name that is not a Go identifier.
	ErrInvalidName Error = "invalid Go identifier"
	// ErrLoad reports a workspace that could not be type-checked at all.
	ErrLoad Error = "no packages could be type-checked"
	// ErrEnvironment reports a toolchain the process cannot use, as distinct
	// from anything wrong with the code or the request.
	ErrEnvironment Error = "the Go toolchain is not usable from this process"
)

// loadMode requests syntax and type info for the module's own packages, and
// types only for its dependencies.
//
// NeedDeps is deliberately absent. With it, go/packages parses and records
// full type info for every transitive dependency: measured on an 827-file
// module that meant 2156 packages with TypesInfo and 2.39 GB of heap, against
// 318 packages and 412 MB without it. Dependency types still arrive via export
// data, which is all that resolving a cross-package reference needs, and no
// reference to a symbol in this module can exist inside the standard library.
const loadMode = packages.NeedName |
	packages.NeedFiles |
	packages.NeedCompiledGoFiles |
	packages.NeedImports |
	packages.NeedSyntax |
	packages.NeedTypes |
	packages.NeedTypesInfo

// Snapshot is a type-checked view of a selector's packages.
type Snapshot struct {
	Packages []*packages.Package
	Fset     *token.FileSet
	// TypeErrors holds diagnostics from packages that did not check cleanly.
	// A rename over such a package is incomplete, so callers must surface this.
	TypeErrors []string
	// Ignored lists Go files excluded by build constraints. They are invisible
	// to the type checker and therefore never edited.
	Ignored []string

	root string

	// srcMu guards src. A snapshot is shared by every concurrent caller
	// holding the same selector, and an unsynchronised map write here is not a
	// data race that merely corrupts a value: the runtime throws "concurrent
	// map writes", which kills the process outright and cannot be recovered.
	// Eight concurrent reference queries were enough to do it.
	srcMu sync.RWMutex
	src   map[string][]byte
}

// Load type-checks the packages matched by selector.
func Load(ctx context.Context, root, selector string) (*Snapshot, error) {
	if strings.TrimSpace(selector) == "" {
		selector = "./..."
	}
	// Positions from go/packages are absolute, so the root must be too or
	// every Rel and every file lookup silently fails to match.
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Mode:    loadMode,
		Dir:     root,
		Context: ctx,
		Fset:    fset,
		Tests:   true,
	}
	pkgs, err := packages.Load(cfg, selector)
	if err != nil {
		return nil, fmt.Errorf("load %q: %w", selector, err)
	}

	s := &Snapshot{Packages: pkgs, Fset: fset, root: root, src: make(map[string][]byte)}

	var checked int
	seenErr := make(map[string]bool)
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if p.Types != nil && p.TypesInfo != nil {
			checked++
		}
		for _, e := range p.Errors {
			msg := e.Error()
			if !seenErr[msg] {
				seenErr[msg] = true
				s.TypeErrors = append(s.TypeErrors, msg)
			}
		}
	})
	if checked == 0 {
		// "selector matched nothing" is the wrong answer when the toolchain
		// could not run: it sends the caller to debug a selector that was
		// fine. Ask the environment before blaming the input.
		if tc := InspectToolchain(ctx, root); !tc.CanTypeChk {
			return nil, fmt.Errorf("%w: %s. %s. Syntactic tools (go_read, go_query, go_search_symbols) are unaffected",
				ErrEnvironment, tc.Cause, tc.Remedy)
		}
		detail := strings.Join(s.TypeErrors, "; ")
		if detail == "" {
			detail = "the selector matched no packages"
		}
		return nil, fmt.Errorf("%w for %q: %s", ErrLoad, selector, detail)
	}

	for _, p := range pkgs {
		for _, f := range p.IgnoredFiles {
			if strings.HasSuffix(f, ".go") {
				s.Ignored = append(s.Ignored, f)
			}
		}
	}
	slices.Sort(s.Ignored)
	s.Ignored = slices.Compact(s.Ignored)
	slices.Sort(s.TypeErrors)

	return s, nil
}

// Rel renders path relative to the snapshot root.
func (s *Snapshot) Rel(path string) string {
	if r, err := filepath.Rel(s.root, path); err == nil {
		return r
	}
	return path
}

// Source returns the bytes of path, cached, for computing edit offsets and the
// content hashes a changeset records.
func (s *Snapshot) Source(path string) ([]byte, error) {
	s.srcMu.RLock()
	b, ok := s.src[path]
	s.srcMu.RUnlock()
	if ok {
		return b, nil
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	s.srcMu.Lock()
	defer s.srcMu.Unlock()
	// Another caller may have read the same file meanwhile; either copy is
	// equally valid, so keep whichever is installed to preserve identity.
	if existing, ok := s.src[path]; ok {
		return existing, nil
	}
	s.src[path] = b
	return b, nil
}

// Sources returns a copy of every file read so far, for recording in a
// changeset. It is a copy because the caller would otherwise be reading a map
// that concurrent Source calls are still writing to.
func (s *Snapshot) Sources() map[string][]byte {
	s.srcMu.RLock()
	defer s.srcMu.RUnlock()
	return maps.Clone(s.src)
}

// offset converts a position to a byte offset in its file.
func (s *Snapshot) offset(pos token.Pos) (path string, off int, ok bool) {
	p := s.Fset.Position(pos)
	if !p.IsValid() {
		return "", 0, false
	}
	return p.Filename, p.Offset, true
}

// eachTypesPackage visits every reachable types.Package, including
// dependencies whose types come from export data and so carry no TypesInfo.
//
// Resolution has to reach these: io.Writer is a legitimate target for
// go_implements even though nothing in the module parses the io package.
func (s *Snapshot) eachTypesPackage(fn func(*types.Package)) {
	seen := make(map[*types.Package]bool)
	var visit func(p *types.Package)
	visit = func(p *types.Package) {
		if p == nil || seen[p] {
			return
		}
		seen[p] = true
		fn(p)
		for _, imp := range p.Imports() {
			visit(imp)
		}
	}
	packages.Visit(s.Packages, nil, func(p *packages.Package) { visit(p.Types) })
}

// unique iterates the loaded packages once per import path.
//
// Tests:true makes go/packages return a package, its test variant, and its
// external test package, which share files. Type-checking each separately means
// the same identifier appears more than once; callers that build edits must not
// emit it twice.
func (s *Snapshot) unique(fn func(p *packages.Package)) {
	seen := make(map[*packages.Package]bool)
	packages.Visit(s.Packages, nil, func(p *packages.Package) {
		if p.TypesInfo == nil || seen[p] {
			return
		}
		seen[p] = true
		fn(p)
	})
}
