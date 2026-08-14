package sem_test

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/sem"
)

func projRoot() string     { return filepath.Join("..", "..", "testdata", "proj") }
func refactorRoot() string { return filepath.Join("..", "..", "testdata", "refactor") }

func load(t *testing.T, root string) *sem.Snapshot {
	t.Helper()
	s, err := sem.Load(t.Context(), root, "./...")
	if err != nil {
		t.Fatalf("Load(%s): %v", root, err)
	}
	return s
}

func TestResolve(t *testing.T) {
	s := load(t, projRoot())
	tests := []struct {
		name     string
		spec     string
		wantKind string
		wantPos  string
	}{
		{name: "qualified import path", spec: "example.com/proj/demo.Config", wantKind: "type", wantPos: "demo/demo.go:10:6"},
		{name: "short package name", spec: "demo.Load", wantKind: "func", wantPos: "demo/demo.go:15:6"},
		{name: "method on a type", spec: "demo.Server.Addr", wantKind: "method", wantPos: "demo/demo.go:23:18"},
		{name: "struct field", spec: "demo.Server.Config", wantKind: "field", wantPos: "demo/demo.go:19:2"},
		{name: "decoy in another package", spec: "other.Config", wantKind: "type", wantPos: "other/other.go:4:6"},
		{name: "by position", spec: "demo/demo.go:10:6", wantKind: "type", wantPos: "demo/demo.go:10:6"},
		{name: "by position on a reference", spec: "demo/demo.go:15:14", wantKind: "type", wantPos: "demo/demo.go:10:6"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj, err := s.Resolve(tt.spec)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.spec, err)
			}
			got := s.Describe(obj)
			if got.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if filepath.ToSlash(got.Position) != tt.wantPos {
				t.Errorf("position = %q, want %q", got.Position, tt.wantPos)
			}
		})
	}
}

func TestResolveErrors(t *testing.T) {
	s := load(t, projRoot())
	for _, spec := range []string{"demo.Nope", "nosuchpkg.Thing", "demo/demo.go:999:1", ""} {
		t.Run(spec, func(t *testing.T) {
			if obj, err := s.Resolve(spec); err == nil {
				t.Fatalf("Resolve(%q) = %v, want error", spec, obj)
			}
		})
	}
}

// This is the case that motivates the whole semantic plane. The token "Config"
// appears eleven times across the fixture as six different things; exactly four
// of them are the type being renamed.
func TestRenameDistinguishesIdenticalSpellings(t *testing.T) {
	s := load(t, projRoot())
	obj, err := s.Resolve("example.com/proj/demo.Config")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	r, err := s.Rename(obj, "Settings")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}

	if r.Sites != 4 {
		t.Errorf("Sites = %d, want 4", r.Sites)
	}
	if !slices.Equal(r.Files, []string{"demo/demo.go"}) {
		t.Errorf("Files = %v, want only demo/demo.go", r.Files)
	}
	if len(r.Conflicts) != 0 {
		t.Errorf("unexpected conflicts: %+v", r.Conflicts)
	}

	// Spell out the lines that must and must not change.
	lines := make(map[int]bool)
	for _, e := range r.Edits {
		lines[lineOfEdit(t, s, e.Path, e.Start)] = true
	}
	for _, want := range []int{10, 15, 19} { // type decl, Load's two refs, Server field type
		if !lines[want] {
			t.Errorf("expected an edit on line %d; got lines %v", want, sortedKeys(lines))
		}
	}
	for _, forbidden := range []int{26, 27, 28} { // local var, other.Config, anon struct field
		if lines[forbidden] {
			t.Errorf("edit on line %d renames something that is not demo.Config", forbidden)
		}
	}
}

func TestRenameDecoyPackageIsIndependent(t *testing.T) {
	s := load(t, projRoot())
	obj, err := s.Resolve("other.Config")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	r, err := s.Rename(obj, "Options")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	// Its declaration plus the single cross-package use in demo.
	if r.Sites != 2 {
		t.Errorf("Sites = %d, want 2", r.Sites)
	}
	want := []string{"demo/demo.go", "other/other.go"}
	if !slices.Equal(r.Files, want) {
		t.Errorf("Files = %v, want %v", r.Files, want)
	}
}

func TestReferences(t *testing.T) {
	s := load(t, projRoot())
	obj, err := s.Resolve("demo.Load")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	refs := s.References(obj)
	if len(refs) != 2 {
		t.Fatalf("refs = %d, want 2 (declaration and the test call)\n%+v", len(refs), refs)
	}
	if refs[0].Kind != "declaration" {
		t.Errorf("first ref kind = %q, want declaration", refs[0].Kind)
	}
	// The reference lives in demo_test.go, which proves test files are loaded.
	var files []string
	for _, r := range refs {
		files = append(files, r.File)
	}
	if !slices.Contains(files, "demo/demo_test.go") {
		t.Errorf("references %v missing the test file", files)
	}
}

// Renaming an embedded type must also move the implicit field name and every
// selector that reads through it, or the result does not compile.
func TestRenameEmbeddedTypeUpdatesImplicitField(t *testing.T) {
	s := load(t, refactorRoot())
	obj, err := s.Resolve("example.com/refactor/base.Config")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	r, err := s.Rename(obj, "Settings")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}

	want := []string{"app/app.go", "base/base.go"}
	if !slices.Equal(r.Files, want) {
		t.Errorf("Files = %v, want %v", r.Files, want)
	}

	var notes []string
	for _, e := range r.Edits {
		notes = append(notes, e.Note)
	}
	if !slices.Contains(notes, "implicit field of embedded Config") {
		t.Errorf("no edit was attributed to the embedded field; notes = %v", notes)
	}

	// s.Config must become s.Settings.
	refs := s.References(obj)
	var sawSelector bool
	for _, ref := range refs {
		if ref.File == "app/app.go" && strings.Contains(ref.Text, "s.Config.Name") {
			sawSelector = true
		}
	}
	if !sawSelector {
		t.Errorf("selector through the embedded field was not found in references:\n%+v", refs)
	}
}

func TestRenameConflicts(t *testing.T) {
	tests := []struct {
		name     string
		root     string
		symbol   string
		newName  string
		wantKind string
		wantText string
	}{
		{
			name:     "collides with another package-level declaration",
			root:     projRoot(),
			symbol:   "demo.Config",
			newName:  "Server",
			wantKind: "scope",
			wantText: "already declared",
		},
		{
			name:     "collides with another method on the same receiver",
			root:     refactorRoot(),
			symbol:   "example.com/refactor/base.Config.String",
			newName:  "Rename",
			wantKind: "method-set",
			wantText: "already has a method",
		},
		{
			name:     "breaks interface satisfaction",
			root:     refactorRoot(),
			symbol:   "example.com/refactor/base.Config.String",
			newName:  "Describe",
			wantKind: "interface",
			wantText: "satisfies",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := load(t, tt.root)
			obj, err := s.Resolve(tt.symbol)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.symbol, err)
			}
			r, err := s.Rename(obj, tt.newName)
			if err != nil {
				t.Fatalf("Rename: %v", err)
			}
			var kinds []string
			var found bool
			for _, c := range r.Conflicts {
				kinds = append(kinds, c.Kind)
				if c.Kind == tt.wantKind && strings.Contains(c.Message, tt.wantText) {
					found = true
				}
			}
			if !found {
				t.Errorf("no %q conflict containing %q; got %v\n%+v",
					tt.wantKind, tt.wantText, kinds, r.Conflicts)
			}
		})
	}
}

func TestRenameRejectsBadNames(t *testing.T) {
	s := load(t, projRoot())
	obj, err := s.Resolve("demo.Config")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, name := range []string{"", "func", "1abc", "has space", "has-dash", "_", "Config"} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Rename(obj, name); err == nil {
				t.Fatalf("Rename to %q succeeded, want rejection", name)
			} else if name != "Config" && !errors.Is(err, sem.ErrInvalidName) {
				t.Errorf("err = %v, want ErrInvalidName", err)
			}
		})
	}
}

func TestRenameWarnsOnExportednessChange(t *testing.T) {
	s := load(t, projRoot())
	obj, err := s.Resolve("demo.Config")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	r, err := s.Rename(obj, "config")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	var found bool
	for _, w := range r.Warnings {
		if strings.Contains(w, "unexports") {
			found = true
		}
	}
	if !found {
		t.Errorf("no unexport warning in %v", r.Warnings)
	}
}

func lineOfEdit(t *testing.T, s *sem.Snapshot, path string, off uint32) int {
	t.Helper()
	src, err := s.Source(path)
	if err != nil {
		t.Fatalf("Source(%s): %v", path, err)
	}
	line := 1
	for i := range int(off) {
		if src[i] == '\n' {
			line++
		}
	}
	return line
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
