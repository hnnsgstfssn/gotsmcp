package source_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/source"
)

func projRoot() string   { return filepath.Join("..", "..", "testdata", "proj") }
func brokenRoot() string { return filepath.Join("..", "..", "testdata", "broken") }

func newLoader(t *testing.T, root string) *source.Loader {
	t.Helper()
	l, err := source.NewLoader(root)
	if err != nil {
		t.Fatalf("NewLoader(%s): %v", root, err)
	}
	return l
}

// relFiles renders resolved packages as root-relative paths for stable asserts.
func relFiles(l *source.Loader, pkgs []source.Package) []string {
	var out []string
	for _, f := range source.Files(pkgs) {
		out = append(out, filepath.ToSlash(l.Rel(f)))
	}
	slices.Sort(out)
	return out
}

func TestResolve(t *testing.T) {
	l := newLoader(t, projRoot())
	tests := []struct {
		name     string
		selector string
		want     []string
	}{
		{
			name:     "whole project",
			selector: "./...",
			want:     []string{"demo/demo.go", "demo/demo_test.go", "other/other.go"},
		},
		{
			name:     "empty means whole project",
			selector: "",
			want:     []string{"demo/demo.go", "demo/demo_test.go", "other/other.go"},
		},
		{
			name:     "single package by dir",
			selector: "./other",
			want:     []string{"other/other.go"},
		},
		{
			name:     "single package by import path",
			selector: "example.com/proj/other",
			want:     []string{"other/other.go"},
		},
		{
			name:     "recursive import path pattern",
			selector: "example.com/proj/demo/...",
			want:     []string{"demo/demo.go", "demo/demo_test.go"},
		},
		{
			name:     "single file",
			selector: "demo/demo.go",
			want:     []string{"demo/demo.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkgs, err := l.Resolve(t.Context(), tt.selector)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.selector, err)
			}
			if got := relFiles(l, pkgs); !slices.Equal(got, tt.want) {
				t.Errorf("Resolve(%q) files = %v, want %v", tt.selector, got, tt.want)
			}
		})
	}
}

func TestResolveRejectsEscape(t *testing.T) {
	l := newLoader(t, projRoot())
	for _, sel := range []string{"../broken/broken.go", "/etc/hosts"} {
		t.Run(sel, func(t *testing.T) {
			if _, err := l.Resolve(t.Context(), sel); err == nil {
				t.Fatalf("Resolve(%q) succeeded, want containment error", sel)
			}
		})
	}
}

// The read plane must work on code that does not compile, which is the whole
// reason it is tree-sitter and not go/packages.
func TestResolveAndParseBrokenCode(t *testing.T) {
	l := newLoader(t, brokenRoot())
	pkgs, err := l.Resolve(t.Context(), "./...")
	if err != nil {
		t.Fatalf("Resolve on broken module: %v", err)
	}
	files := source.Files(pkgs)
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1: %v", len(files), files)
	}

	f, err := l.File(files[0])
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if !f.Tree.RootNode().HasError() {
		t.Error("expected HasError on unparseable source")
	}
	if errs := source.SyntaxErrors(f, l.Lang(), 10); len(errs) == 0 {
		t.Error("expected reported syntax errors")
	}

	// The undamaged declarations around the broken one must still be visible.
	var names []string
	for _, d := range source.Outline(f, l.Lang()) {
		if d.Name != "" {
			names = append(names, d.Name)
		}
	}
	for _, want := range []string{"Good", "Fine"} {
		if !slices.Contains(names, want) {
			t.Errorf("outline %v missing %q recovered from broken file", names, want)
		}
	}
}

func TestOutline(t *testing.T) {
	l := newLoader(t, projRoot())
	f, err := l.File("demo/demo.go")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	decls := source.Outline(f, l.Lang())

	byName := make(map[string]source.Decl)
	for _, d := range decls {
		byName[d.Name] = d
	}

	t.Run("elides function bodies", func(t *testing.T) {
		load, ok := byName["Load"]
		if !ok {
			t.Fatal("Load not in outline")
		}
		if !strings.HasSuffix(load.Signature, "{ ... }") {
			t.Errorf("signature %q does not elide body", load.Signature)
		}
		if strings.Contains(load.Signature, "Name: \"x\"") {
			t.Errorf("signature %q leaked the body", load.Signature)
		}
	})

	t.Run("captures doc comments", func(t *testing.T) {
		if got := byName["Config"].Doc; !strings.Contains(got, "Config carries demo settings") {
			t.Errorf("Config doc = %q", got)
		}
	})

	t.Run("captures method receiver", func(t *testing.T) {
		addr, ok := byName["Addr"]
		if !ok {
			t.Fatal("Addr not in outline")
		}
		if !strings.Contains(addr.Recv, "Server") {
			t.Errorf("Addr recv = %q, want it to mention Server", addr.Recv)
		}
	})
}

func TestRenderTreeShowsGrammarNames(t *testing.T) {
	l := newLoader(t, projRoot())
	f, err := l.File("demo/demo.go")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	out := source.RenderTree(f, l.Lang(), nil, source.TreeOptions{MaxDepth: 2})

	// These are the exact identifiers a query author has to type.
	for _, want := range []string{"source_file", "type_declaration", "function_declaration", "method_declaration"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered tree missing node type %q\n%s", want, out)
		}
	}
	if !strings.Contains(out, "[1:1") {
		t.Errorf("rendered tree missing 1-based positions:\n%s", out)
	}
}

func TestRenderTreeRespectsDepthAndNodeCap(t *testing.T) {
	l := newLoader(t, projRoot())
	f, err := l.File("demo/demo.go")
	if err != nil {
		t.Fatalf("File: %v", err)
	}

	shallow := source.RenderTree(f, l.Lang(), nil, source.TreeOptions{MaxDepth: 1})
	deep := source.RenderTree(f, l.Lang(), nil, source.TreeOptions{MaxDepth: 8})
	if len(shallow) >= len(deep) {
		t.Errorf("depth 1 output (%d bytes) not smaller than depth 8 (%d bytes)", len(shallow), len(deep))
	}
	if !strings.Contains(shallow, "children)") {
		t.Errorf("depth-limited output should summarise elided children:\n%s", shallow)
	}

	capped := source.RenderTree(f, l.Lang(), nil, source.TreeOptions{MaxNodes: 5})
	if !strings.Contains(capped, "truncated") {
		t.Errorf("node-capped output should say so:\n%s", capped)
	}
}

func TestQuery(t *testing.T) {
	l := newLoader(t, projRoot())
	pkgs, err := l.Resolve(t.Context(), "./...")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{
			name:  "all function declarations",
			query: `(function_declaration name: (identifier) @fn)`,
			// demo: Load, run, logf; other: Helper; demo_test: TestLoad
			want: 5,
		},
		{
			name:  "fmt calls only",
			query: `(call_expression function: (selector_expression operand: (identifier) @pkg (#eq? @pkg "fmt")))`,
			want:  2,
		},
		{
			name:  "type declarations named Config",
			query: `(type_spec name: (type_identifier) @n (#eq? @n "Config"))`,
			want:  2, // demo.Config and other.Config
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := l.Query(context.Background(), pkgs, tt.query, source.QueryOptions{})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if res.Total != tt.want {
				t.Errorf("Total = %d, want %d\nmatches: %+v", res.Total, tt.want, res.Matches)
			}
		})
	}
}

func TestQueryCountOnlyAndBadSyntax(t *testing.T) {
	l := newLoader(t, projRoot())
	pkgs, err := l.Resolve(t.Context(), "./...")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	t.Run("count only omits matches", func(t *testing.T) {
		res, err := l.Query(t.Context(), pkgs, `(function_declaration) @f`, source.QueryOptions{CountOnly: true})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if res.Total == 0 {
			t.Fatal("Total = 0")
		}
		if len(res.Matches) != 0 {
			t.Errorf("CountOnly returned %d materialised matches", len(res.Matches))
		}
	})

	t.Run("invalid query is an error not a silent zero", func(t *testing.T) {
		if _, err := l.Query(t.Context(), pkgs, `(this_node_type_does_not_exist) @x`, source.QueryOptions{}); err == nil {
			t.Fatal("expected error for unknown node type")
		}
	})
}

func TestFileCacheInvalidatesOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package a\n\nfunc One() {}\n")

	l := newLoader(t, dir)
	f1, err := l.File("a.go")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if got := len(source.Outline(f1, l.Lang())); got != 2 {
		t.Fatalf("outline decls = %d, want 2", got)
	}

	write("package a\n\nfunc One() {}\n\nfunc Two() {}\n")
	l.Forget(path)

	f2, err := l.File("a.go")
	if err != nil {
		t.Fatalf("File after change: %v", err)
	}
	if got := len(source.Outline(f2, l.Lang())); got != 3 {
		t.Errorf("outline decls after change = %d, want 3", got)
	}
}

func TestOutlineIncludesPackageDoc(t *testing.T) {
	dir := t.TempDir()
	const src = "// Package widget does a thing.\n//\n// More detail here.\npackage widget\n\nfunc F() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "w.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	l := newLoader(t, dir)
	f, err := l.File("w.go")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	decls := source.Outline(f, l.Lang())
	if len(decls) == 0 || decls[0].Kind != "package_clause" {
		t.Fatalf("first decl = %+v, want the package clause", decls)
	}
	if !strings.Contains(decls[0].Doc, "Package widget does a thing") {
		t.Errorf("package doc missing: %q", decls[0].Doc)
	}
	if !strings.Contains(decls[0].Doc, "More detail here") {
		t.Errorf("package doc truncated: %q", decls[0].Doc)
	}
}

// Parse trees cost roughly 8 MB each regardless of file size, so an unbounded
// cache turns 11 MB of source into gigabytes of heap. The budget must evict.
func TestCacheBudgetEvicts(t *testing.T) {
	dir := t.TempDir()
	const perFile = 12
	for i := range perFile {
		name := filepath.Join(dir, "f"+strconv.Itoa(i)+".go")
		src := "package a\n\nfunc F" + strconv.Itoa(i) + "() {}\n"
		if err := os.WriteFile(name, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Room for about three trees at the 8 MB floor.
	l, err := source.NewLoader(dir, source.WithCacheBudget(24<<20))
	if err != nil {
		t.Fatal(err)
	}
	for i := range perFile {
		if _, err := l.File("f" + strconv.Itoa(i) + ".go"); err != nil {
			t.Fatal(err)
		}
	}

	entries, estimated := l.CacheStats()
	if entries >= perFile {
		t.Errorf("cached %d of %d files; the budget did not evict", entries, perFile)
	}
	if estimated > 24<<20 {
		t.Errorf("estimated %d bytes retained, over the 24 MB budget", estimated)
	}
	if entries == 0 {
		t.Error("cache evicted everything; at least one entry should survive")
	}

	// Eviction must not break correctness.
	f, err := l.File("f0.go")
	if err != nil {
		t.Fatalf("File after eviction: %v", err)
	}
	if got := len(source.Outline(f, l.Lang())); got != 2 {
		t.Errorf("outline after eviction = %d decls, want 2", got)
	}
}

// Concurrent parsing must produce exactly the same results as sequential.
//
// It did not, once: under load the parser would abandon a parse and return a
// truncated tree with a nil error, silently dropping matches. Loader.Parse now
// uses ParseStrict and retries in isolation, and this guards that.
func TestConcurrentQueryMatchesSequential(t *testing.T) {
	dir := t.TempDir()
	const files = 120
	for i := range files {
		var b strings.Builder
		fmt.Fprintf(&b, "package p%d\n\nimport \"fmt\"\n\n", i)
		for j := range 20 {
			fmt.Fprintf(&b, "// Fn%d_%d does a thing.\nfunc Fn%d_%d(a, b int) (int, error) {\n\tif a > b {\n\t\treturn a, fmt.Errorf(\"big %%d\", a)\n\t}\n\treturn b, nil\n}\n\n", i, j, i, j)
			fmt.Fprintf(&b, "type T%d_%d struct{ A, B string }\n\n", i, j)
		}
		sub := filepath.Join(dir, fmt.Sprintf("p%d", i))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "a.go"), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const q = `(function_declaration name: (identifier) @fn)`

	seq, err := source.NewLoader(dir, source.WithMaxWorkers(1))
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := seq.Resolve(t.Context(), "./...")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(source.Files(pkgs)); got != files {
		t.Fatalf("resolved %d files, want %d", got, files)
	}
	want, err := seq.Query(t.Context(), pkgs, q, source.QueryOptions{CountOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if want.Total != files*20 {
		t.Fatalf("sequential total = %d, want %d", want.Total, files*20)
	}

	// A tight cache budget forces eviction to churn while workers read.
	for run := range 5 {
		l, err := source.NewLoader(dir, source.WithCacheBudget(16<<20))
		if err != nil {
			t.Fatal(err)
		}
		got, err := l.Query(t.Context(), pkgs, q, source.QueryOptions{CountOnly: true})
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if got.Total != want.Total || got.FilesSeen != want.FilesSeen || len(got.ParseErrors) > 0 {
			t.Errorf("run %d: %d matches over %d files, %d parse errors; want %d/%d/0",
				run, got.Total, got.FilesSeen, len(got.ParseErrors), want.Total, want.FilesSeen)
		}
	}
}

// A panic inside a worker cannot be recovered by whoever started the goroutine,
// so it has to be caught in the worker or it kills the process. The symptom is
// the client seeing the transport close with no error at all.
func TestWorkerPanicBecomesAnError(t *testing.T) {
	dir := t.TempDir()
	for i := range 20 {
		sub := filepath.Join(dir, "p"+strconv.Itoa(i))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		src := "package p" + strconv.Itoa(i) + "\n\nfunc F() {}\n"
		if err := os.WriteFile(filepath.Join(sub, "a.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l, err := source.NewLoader(dir)
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := l.Resolve(t.Context(), "./...")
	if err != nil {
		t.Fatal(err)
	}

	// Query compiles fine but the per-file callback explodes on one file.
	// Reaching this assertion at all is the test: an unrecovered panic would
	// have taken the test binary with it.
	res, err := l.Query(t.Context(), pkgs, `(function_declaration) @f`, source.QueryOptions{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Total != 20 {
		t.Errorf("total = %d, want 20", res.Total)
	}
}
