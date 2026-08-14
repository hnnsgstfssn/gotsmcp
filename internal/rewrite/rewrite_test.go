package rewrite_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/rewrite"
)

func TestSplice(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		edits []rewrite.Edit
		want  string
		err   error
	}{
		{
			name:  "single replacement",
			src:   "hello world",
			edits: []rewrite.Edit{{Start: 6, End: 11, New: "gophers"}},
			want:  "hello gophers",
		},
		{
			name: "out of order edits are sorted",
			src:  "aaa bbb ccc",
			edits: []rewrite.Edit{
				{Start: 8, End: 11, New: "CCC"},
				{Start: 0, End: 3, New: "AAA"},
			},
			want: "AAA bbb CCC",
		},
		{
			name:  "insertion",
			src:   "ab",
			edits: []rewrite.Edit{{Start: 1, End: 1, New: "X"}},
			want:  "aXb",
		},
		{
			name:  "deletion",
			src:   "abc",
			edits: []rewrite.Edit{{Start: 1, End: 2, New: ""}},
			want:  "ac",
		},
		{
			name: "adjacent edits are allowed",
			src:  "abcd",
			edits: []rewrite.Edit{
				{Start: 0, End: 2, New: "X"},
				{Start: 2, End: 4, New: "Y"},
			},
			want: "XY",
		},
		{
			name: "overlapping edits are refused",
			src:  "abcd",
			edits: []rewrite.Edit{
				{Start: 0, End: 3, New: "X"},
				{Start: 2, End: 4, New: "Y"},
			},
			err: rewrite.ErrOverlap,
		},
		{
			name: "two insertions at one offset are refused",
			src:  "ab",
			edits: []rewrite.Edit{
				{Start: 1, End: 1, New: "X"},
				{Start: 1, End: 1, New: "Y"},
			},
			err: rewrite.ErrOverlap,
		},
		{
			name:  "range past end of file is refused",
			src:   "ab",
			edits: []rewrite.Edit{{Start: 1, End: 99, New: "X"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rewrite.Splice([]byte(tt.src), tt.edits)
			switch {
			case tt.err != nil:
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v, want %v", err, tt.err)
				}
				return
			case tt.want == "" && err != nil:
				return // expected some error
			case err != nil:
				t.Fatalf("Splice: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Splice = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExpand(t *testing.T) {
	caps := map[string]string{"fn": "Println", "args": `"a", b`}
	tests := []struct {
		name string
		tmpl string
		want string
		err  bool
	}{
		{name: "single capture", tmpl: "log.${fn}()", want: "log.Println()"},
		{name: "several captures", tmpl: "x.${fn}(${args})", want: `x.Println("a", b)`},
		{name: "no captures", tmpl: "plain text", want: "plain text"},
		{name: "escaped dollar", tmpl: "cost is $$5", want: "cost is $5"},
		{name: "unknown capture is an error", tmpl: "${nope}", err: true},
		{name: "unterminated brace is an error", tmpl: "${fn", err: true},
		{name: "bare dollar is an error", tmpl: "a $ b", err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rewrite.Expand(tt.tmpl, caps)
			if tt.err {
				if err == nil {
					t.Fatalf("Expand(%q) = %q, want error", tt.tmpl, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Expand(%q): %v", tt.tmpl, err)
			}
			if got != tt.want {
				t.Errorf("Expand(%q) = %q, want %q", tt.tmpl, got, tt.want)
			}
		})
	}
}

// fixture writes a file and returns its path plus a changeset over it.
func fixture(t *testing.T, content string, edits ...rewrite.Edit) (string, *rewrite.ChangeSet) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := range edits {
		edits[i].Path = path
	}
	cs, err := rewrite.New("test", edits, map[string][]byte{path: []byte(content)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return path, cs
}

func rel(p string) string { return filepath.Base(p) }

func TestComputeRejectsStalePreview(t *testing.T) {
	const src = "package a\n\nfunc One() {}\n"
	path, cs := fixture(t, src, rewrite.Edit{Start: 16, End: 19, New: "Two"})

	// Someone else writes to the file between preview and apply.
	if err := os.WriteFile(path, []byte(src+"\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := rewrite.Compute(cs, rel, rewrite.Options{})
	if !rewrite.IsStale(err) {
		t.Fatalf("Compute err = %v, want stale", err)
	}
}

func TestComputeRefusesEditThatBreaksValidFile(t *testing.T) {
	const src = "package a\n\nfunc One() {}\n"
	// Replace the body's closing brace with junk.
	_, cs := fixture(t, src, rewrite.Edit{Start: 11, End: 24, New: "func ((("})

	_, err := rewrite.Compute(cs, rel, rewrite.Options{})
	if !errors.Is(err, rewrite.ErrBroken) {
		t.Fatalf("Compute err = %v, want ErrBroken", err)
	}
}

// Repairing a file that was already broken must remain possible, otherwise the
// safety gate blocks the one edit that fixes things.
func TestComputeAllowsEditingAlreadyBrokenFile(t *testing.T) {
	const src = "package a\n\nfunc Busted( {\n"
	_, cs := fixture(t, src, rewrite.Edit{Start: 11, End: 25, New: "func Fixed() {}"})

	plan, err := rewrite.Compute(cs, rel, rewrite.Options{})
	if err != nil {
		t.Fatalf("Compute on broken file: %v", err)
	}
	if len(plan.Changes) != 1 {
		t.Fatalf("changes = %d, want 1", len(plan.Changes))
	}
	if !plan.Changes[0].WasBroken {
		t.Error("WasBroken not reported for a file that did not parse")
	}
}

func TestApplyWritesAndFormats(t *testing.T) {
	// Deliberately ugly spacing: the formatter must clean it up.
	const src = "package a\n\nfunc One()    int {return 1}\n"
	path, cs := fixture(t, src, rewrite.Edit{Start: 16, End: 19, New: "Two"})

	plan, err := rewrite.Compute(cs, rel, rewrite.Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	written, err := rewrite.Apply(plan, func(string) string { return path })
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("written = %v, want 1 file", written)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "package a\n\nfunc Two() int { return 1 }\n"
	if string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestApplyPreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	const src = "package a\n\nfunc One() {}\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := rewrite.New("test",
		[]rewrite.Edit{{Path: path, Start: 16, End: 19, New: "Two"}},
		map[string][]byte{path: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := rewrite.Compute(cs, rel, rewrite.Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if _, err := rewrite.Apply(plan, func(string) string { return path }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %v, want 0600", got)
	}
}

// The import fixup is the concrete advantage over text substitution: an edit
// that orphans an import should not leave the file failing to compile.
func TestFixImportsDropsOrphanedImport(t *testing.T) {
	const src = `package a

import (
	"fmt"
	"os"
)

func One() { fmt.Println(os.Args) }
`
	_, cs := fixture(t, src, rewrite.Edit{
		Start: uint32(strings.Index(src, "fmt.Println(os.Args)")),
		End:   uint32(strings.Index(src, "fmt.Println(os.Args)") + len("fmt.Println(os.Args)")),
		New:   "println(1)",
	})

	plan, err := rewrite.Compute(cs, rel, rewrite.Options{FixImports: true})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	path := cs.Files[0]
	if _, err := rewrite.Apply(plan, func(string) string { return path }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{`"fmt"`, `"os"`} {
		if strings.Contains(string(got), gone) {
			t.Errorf("orphaned import %s still present:\n%s", gone, got)
		}
	}
}

func TestPreviewHunks(t *testing.T) {
	src := []byte("package a\n\nfunc One() {}\n\nfunc Two() {}\n")
	edits := []rewrite.Edit{
		{Start: 16, End: 19, New: "Uno", Note: "first"},
		{Start: 31, End: 34, New: "Dos", Note: "second"},
	}
	hunks, total := rewrite.Preview("a.go", src, edits, 0)
	if total != 2 || len(hunks) != 2 {
		t.Fatalf("total = %d, hunks = %d, want 2 and 2", total, len(hunks))
	}
	if hunks[0].Line != 3 {
		t.Errorf("first hunk line = %d, want 3", hunks[0].Line)
	}
	if hunks[0].Old != "func One() {}" || hunks[0].New != "func Uno() {}" {
		t.Errorf("hunk 0 = %q -> %q", hunks[0].Old, hunks[0].New)
	}
	if hunks[1].Note != "second" {
		t.Errorf("hunk 1 note = %q", hunks[1].Note)
	}

	t.Run("edits on one line collapse into one hunk", func(t *testing.T) {
		src := []byte("package a\n\nvar x, y = 1, 2\n")
		edits := []rewrite.Edit{
			{Start: 15, End: 16, New: "a"},
			{Start: 18, End: 19, New: "b"},
		}
		hunks, total := rewrite.Preview("a.go", src, edits, 0)
		if total != 1 || len(hunks) != 1 {
			t.Fatalf("total = %d, hunks = %d, want 1 and 1", total, len(hunks))
		}
		if hunks[0].New != "var a, b = 1, 2" {
			t.Errorf("hunk = %q", hunks[0].New)
		}
		if hunks[0].EditsHere != 2 {
			t.Errorf("EditsHere = %d, want 2", hunks[0].EditsHere)
		}
	})

	t.Run("hunks are capped but total is honest", func(t *testing.T) {
		hunks, total := rewrite.Preview("a.go", src, edits, 1)
		if len(hunks) != 1 {
			t.Errorf("hunks = %d, want 1", len(hunks))
		}
		if total != 2 {
			t.Errorf("total = %d, want 2 despite the cap", total)
		}
	})
}
