package tools_test

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/tools"
)

// harness runs the real MCP server over an in-memory transport against a
// writable copy of a fixture module.
//
// Going through the protocol rather than calling handlers directly means the
// tool schemas, argument unmarshalling, and result encoding are all covered.
// Those are the parts an agent actually touches, and the parts that break
// silently if a struct tag is wrong.
type harness struct {
	t       *testing.T
	dir     string
	session *mcp.ClientSession
}

func newHarness(t *testing.T, fixture string) *harness {
	t.Helper()
	dir := copyTree(t, filepath.Join("..", "..", "testdata", fixture))

	srv, err := tools.New(tools.Config{DefaultRoot: dir})
	if err != nil {
		t.Fatalf("tools.New: %v", err)
	}
	m := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"},
		&mcp.ServerOptions{Instructions: tools.Instructions})
	srv.Register(m)

	clientT, serverT := mcp.NewInMemoryTransports()
	ctx := t.Context()
	if _, err := m.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil).
		Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	return &harness{t: t, dir: dir, session: session}
}

// call invokes a tool and returns its text content, failing on a tool error.
func (h *harness) call(name string, args map[string]any) string {
	h.t.Helper()
	res := h.callRaw(name, args)
	if res.IsError {
		h.t.Fatalf("%s returned an error: %s", name, contentText(res))
	}
	return contentText(res)
}

// callExpectError invokes a tool that is supposed to fail.
func (h *harness) callExpectError(name string, args map[string]any) string {
	h.t.Helper()
	res := h.callRaw(name, args)
	if !res.IsError {
		h.t.Fatalf("%s succeeded, want an error:\n%s", name, contentText(res))
	}
	return contentText(res)
}

func (h *harness) callRaw(name string, args map[string]any) *mcp.CallToolResult {
	h.t.Helper()
	res, err := h.session.CallTool(h.t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

// structured decodes a tool's structured output into v.
func (h *harness) structured(name string, args map[string]any, v any) {
	h.t.Helper()
	res := h.callRaw(name, args)
	if res.IsError {
		h.t.Fatalf("%s returned an error: %s", name, contentText(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		h.t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		h.t.Fatalf("decode structured content %s: %v", b, err)
	}
}

// buildsCleanly is the real acceptance criterion for a refactor.
func (h *harness) buildsCleanly() error {
	h.t.Helper()
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = h.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &buildError{output: string(out)}
	}
	return nil
}

type buildError struct{ output string }

func (e *buildError) Error() string { return e.output }

func (h *harness) read(rel string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, rel))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func contentText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// copyTree duplicates a fixture into a temp dir so tests can write to it.
//
// The destination is canonicalised because the server resolves symlinks, and on
// darwin t.TempDir hands back a /var path that is really /private/var.
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dst); err == nil {
		dst = resolved
	}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

// allTools is the registered surface; the instructions test asserts each is
// documented, so adding a tool without documenting it fails the build.
var allTools = []string{
	"go_read", "go_tree", "go_query", "go_search_symbols", "go_search_text",
	"go_symbol", "go_refs", "go_callers", "go_implements", "go_rename", "go_check",
	"go_rewrite", "go_edit", "go_apply",
	"go_workspace", "go_deps", "go_move", "go_extract", "go_signature",
	"go_seam", "go_tests_for", "go_implement", "go_format", "go_tidy", "go_untested",
}

var wantToolCount = len(allTools)

func TestToolsAreRegistered(t *testing.T) {
	h := newHarness(t, "proj")
	res, err := h.session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := make(map[string]bool)
	for _, tool := range res.Tools {
		got[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %s has no input schema", tool.Name)
		}
	}
	for _, want := range allTools {
		if !got[want] {
			t.Errorf("tool %s not registered; have %v", want, keys(got))
		}
	}
}

func TestReadModes(t *testing.T) {
	h := newHarness(t, "proj")

	t.Run("outline elides bodies", func(t *testing.T) {
		out := h.call("go_read", map[string]any{"selector": "demo/demo.go"})
		if !strings.Contains(out, "func Load() *Config { ... }") {
			t.Errorf("outline missing elided Load signature:\n%s", out)
		}
		if strings.Contains(out, `Name: "x"`) {
			t.Errorf("outline leaked a function body:\n%s", out)
		}
		if !strings.Contains(out, "// Config carries demo settings.") {
			t.Errorf("outline dropped doc comments:\n%s", out)
		}
	})

	t.Run("full returns source", func(t *testing.T) {
		out := h.call("go_read", map[string]any{"selector": "demo/demo.go", "mode": "full"})
		if !strings.Contains(out, `Name: "x"`) {
			t.Errorf("full mode missing body:\n%s", out)
		}
	})

	t.Run("name expands one declaration", func(t *testing.T) {
		out := h.call("go_read", map[string]any{"selector": "demo/demo.go", "name": "Load"})
		if !strings.Contains(out, `return &Config{Name: "x"}`) {
			t.Errorf("named read missing the body:\n%s", out)
		}
		if strings.Contains(out, "func logf") {
			t.Errorf("named read returned other declarations:\n%s", out)
		}
	})

	t.Run("whole project by pattern", func(t *testing.T) {
		out := h.call("go_read", map[string]any{"selector": "./..."})
		if !strings.Contains(out, "; 3 file(s) in 2 package(s)") {
			t.Errorf("header missing counts:\n%s", firstLines(out, 3))
		}
	})

	// A client that prefers structuredContent must still see source. Returning
	// a typed output here made go_read show byte counts and nothing else.
	t.Run("returns source, not a summary object", func(t *testing.T) {
		res := h.callRaw("go_read", map[string]any{"selector": "demo/demo.go"})
		if res.StructuredContent != nil {
			t.Errorf("go_read must not emit structured content, got %v", res.StructuredContent)
		}
		if !strings.Contains(contentText(res), "func Load()") {
			t.Errorf("source missing from content:\n%s", contentText(res))
		}
	})
}

func TestTreeExposesGrammarNames(t *testing.T) {
	h := newHarness(t, "proj")
	out := h.call("go_tree", map[string]any{
		"file": "demo/demo.go", "start_line": 15, "end_line": 15, "max_depth": 4,
	})
	for _, want := range []string{"function_declaration", "parameter_list", "pointer_type"} {
		if !strings.Contains(out, want) {
			t.Errorf("tree missing %q:\n%s", want, out)
		}
	}
}

func TestQueryFindsStructuralMatches(t *testing.T) {
	h := newHarness(t, "proj")

	var res struct {
		Total    int `json:"total"`
		FilesHit int `json:"files_hit"`
		Matches  []struct {
			File     string `json:"file"`
			Captures []struct {
				Name string `json:"name"`
				Text string `json:"text"`
			} `json:"captures"`
		} `json:"matches"`
	}
	h.structured("go_query", map[string]any{
		"query": `(call_expression function: (selector_expression
                    operand: (identifier) @pkg (#eq? @pkg "fmt")
                    field: (field_identifier) @fn))`,
	}, &res)

	if res.Total != 2 {
		t.Errorf("total = %d, want 2 (Println and Printf)", res.Total)
	}
	var fns []string
	for _, m := range res.Matches {
		for _, c := range m.Captures {
			if c.Name == "fn" {
				fns = append(fns, c.Text)
			}
		}
	}
	if len(fns) != 2 {
		t.Errorf("captured fns = %v, want two", fns)
	}
}

// The headline case: a rename that a textual tool gets wrong.
func TestRenamePreviewThenApply(t *testing.T) {
	h := newHarness(t, "proj")

	var preview tools.ChangeOutput
	h.structured("go_rename", map[string]any{
		"symbol": "example.com/proj/demo.Config", "new_name": "Settings",
	}, &preview)

	if preview.Applied {
		t.Fatal("rename applied without being asked to")
	}
	if preview.ChangeSetID == "" {
		t.Fatal("no changeset id returned")
	}
	if preview.Sites != 4 {
		t.Errorf("sites = %d, want 4", preview.Sites)
	}
	if len(preview.Conflicts) != 0 {
		t.Errorf("unexpected conflicts: %+v", preview.Conflicts)
	}

	// Nothing may have been written yet.
	if got := h.read("demo/demo.go"); !strings.Contains(got, "type Config struct") {
		t.Fatal("preview modified the file on disk")
	}

	var applied tools.ChangeOutput
	h.structured("go_apply", map[string]any{"changeset_id": preview.ChangeSetID}, &applied)
	if !applied.Applied {
		t.Fatal("apply did not report success")
	}

	got := h.read("demo/demo.go")
	for _, want := range []string{
		"type Settings struct",
		"func Load() *Settings",
		"return &Settings{Name: \"x\"}",
		"Config *Settings", // the field keeps its name, its type changes
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q after rename:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"Settings := s.Settings", // the local variable must not be renamed
		"other.Settings",         // the other package's type must not be renamed
		"struct{ Settings int }", // the anonymous field must not be renamed
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("renamed something unrelated (%q):\n%s", forbidden, got)
		}
	}
	if other := h.read("other/other.go"); !strings.Contains(other, "type Config struct") {
		t.Errorf("the decoy package was modified:\n%s", other)
	}

	if err := h.buildsCleanly(); err != nil {
		t.Errorf("module does not build after rename:\n%v", err)
	}
}

// Renaming an embedded type has to move the implicit field and its selectors.
func TestRenameEmbeddedTypeStillCompiles(t *testing.T) {
	h := newHarness(t, "refactor")

	var out tools.ChangeOutput
	h.structured("go_rename", map[string]any{
		"symbol": "example.com/refactor/base.Config", "new_name": "Settings", "apply": true,
	}, &out)
	if !out.Applied {
		t.Fatalf("rename not applied: %+v", out)
	}

	app := h.read("app/app.go")
	if !strings.Contains(app, "base.Settings") {
		t.Errorf("embedded field declaration not renamed:\n%s", app)
	}
	if !strings.Contains(app, "s.Settings.Name") {
		t.Errorf("selector through the embedded field not renamed:\n%s", app)
	}
	if err := h.buildsCleanly(); err != nil {
		t.Errorf("module does not build after embedded rename:\n%v", err)
	}
}

func TestRenameBlockedByInterfaceConflict(t *testing.T) {
	h := newHarness(t, "refactor")

	var out tools.ChangeOutput
	h.structured("go_rename", map[string]any{
		"symbol":   "example.com/refactor/base.Config.String",
		"new_name": "Describe",
		"apply":    true,
	}, &out)

	if out.Applied {
		t.Error("rename applied despite breaking interface satisfaction")
	}
	if !out.Blocked {
		t.Errorf("rename not reported as blocked: %+v", out)
	}
	if len(out.Conflicts) == 0 {
		t.Error("no conflicts reported")
	}
	if got := h.read("base/base.go"); !strings.Contains(got, "func (c Config) String()") {
		t.Error("blocked rename still wrote to disk")
	}
	if err := h.buildsCleanly(); err != nil {
		t.Errorf("blocked rename left the module broken:\n%v", err)
	}
}

// Migrating every fmt call in a package to log is the shape of bulk edit that
// otherwise gets done with sed. Note that an argument_list node spans its own
// parentheses, so ${args} already carries them.
func TestRewriteByQueryTemplate(t *testing.T) {
	h := newHarness(t, "proj")

	var out tools.ChangeOutput
	h.structured("go_rewrite", map[string]any{
		"query": `(call_expression
                    function: (selector_expression
                      operand: (identifier) @pkg (#eq? @pkg "fmt")
                      field: (field_identifier) @fn)
                    arguments: (argument_list) @args) @call`,
		"target":   "call",
		"template": "log.${fn}${args}",
		"selector": "./demo",
		"apply":    true,
	}, &out)

	if !out.Applied {
		t.Fatalf("rewrite not applied: %+v", out)
	}
	if out.Sites != 2 {
		t.Errorf("sites = %d, want 2", out.Sites)
	}

	got := h.read("demo/demo.go")
	for _, want := range []string{
		"log.Println(Config, other.Config{}, other.Helper())",
		"log.Printf(format, args...)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q after rewrite:\n%s", want, got)
		}
	}

	// goimports must add log and drop fmt, which now has no uses left.
	if !strings.Contains(got, `"log"`) {
		t.Errorf("import for log was not added:\n%s", got)
	}
	if strings.Contains(got, `"fmt"`) {
		t.Errorf("orphaned fmt import was not removed:\n%s", got)
	}
	if err := h.buildsCleanly(); err != nil {
		t.Errorf("module does not build after rewrite:\n%v", err)
	}
}

func TestRewriteRefusesUnknownCapture(t *testing.T) {
	h := newHarness(t, "proj")
	msg := h.callExpectError("go_rewrite", map[string]any{
		"query":    `(function_declaration name: (identifier) @name)`,
		"template": "${nosuchcapture}",
	})
	if !strings.Contains(msg, "unknown capture") {
		t.Errorf("error should name the problem, got: %s", msg)
	}
}

func TestRewriteRefusesResultThatDoesNotParse(t *testing.T) {
	h := newHarness(t, "proj")
	before := h.read("demo/demo.go")

	msg := h.callExpectError("go_rewrite", map[string]any{
		"query":    `(function_declaration name: (identifier) @name (#eq? @name "logf"))`,
		"template": "func ((( ",
		"target":   "name",
		"selector": "./demo",
		"apply":    true,
	})
	if !strings.Contains(msg, "invalid Go") {
		t.Errorf("error should say the result is invalid Go, got: %s", msg)
	}
	if h.read("demo/demo.go") != before {
		t.Error("a rejected rewrite still modified the file")
	}
}

func TestEditExplicitRanges(t *testing.T) {
	h := newHarness(t, "proj")

	// other.go line 6: func Helper() string { return "other" }
	var out tools.ChangeOutput
	h.structured("go_edit", map[string]any{
		"edits": []map[string]any{{
			"file":       "other/other.go",
			"start_line": 6, "start_col": 6,
			"end_line": 6, "end_col": 12,
			"new":  "Assist",
			"note": "rename via explicit range",
		}},
		"apply": true,
	}, &out)

	if !out.Applied {
		t.Fatalf("edit not applied: %+v", out)
	}
	if got := h.read("other/other.go"); !strings.Contains(got, "func Assist() string") {
		t.Errorf("edit did not land:\n%s", got)
	}
}

func TestApplyRefusesStaleChangeset(t *testing.T) {
	h := newHarness(t, "proj")

	var preview tools.ChangeOutput
	h.structured("go_rename", map[string]any{
		"symbol": "example.com/proj/demo.Config", "new_name": "Settings",
	}, &preview)

	// Someone edits the file behind our back.
	path := filepath.Join(h.dir, "demo", "demo.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, []byte("\nvar Extra = 1\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	msg := h.callExpectError("go_apply", map[string]any{"changeset_id": preview.ChangeSetID})
	if !strings.Contains(msg, "changed since preview") {
		t.Errorf("stale apply should say so, got: %s", msg)
	}
}

func TestReadWorksOnCodeThatDoesNotCompile(t *testing.T) {
	h := newHarness(t, "broken")

	out := h.call("go_read", map[string]any{"selector": "./..."})
	if !strings.Contains(out, "syntax error") {
		t.Errorf("broken file should be flagged:\n%s", out)
	}
	for _, want := range []string{"func Good() int", "type Fine struct"} {
		if !strings.Contains(out, want) {
			t.Errorf("outline lost %q that survives the syntax error:\n%s", want, out)
		}
	}
}

func TestPathContainmentIsEnforced(t *testing.T) {
	h := newHarness(t, "proj")
	for _, sel := range []string{"../../../etc/hosts", "/etc/hosts"} {
		t.Run(sel, func(t *testing.T) {
			h.callExpectError("go_read", map[string]any{"selector": sel})
		})
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestReadRespectsByteCap(t *testing.T) {
	h := newHarness(t, "proj")
	out := h.call("go_read", map[string]any{"selector": "./...", "mode": "full", "max_bytes": 200})
	if !strings.Contains(out, "TRUNCATED") {
		t.Errorf("read under a 200 byte cap was not reported as truncated:\n%s", firstLines(out, 4))
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// go_tree is a text tool for the same reason go_read is.
func TestTreeReturnsText(t *testing.T) {
	h := newHarness(t, "proj")
	res := h.callRaw("go_tree", map[string]any{"file": "demo/demo.go", "max_depth": 2})
	if res.StructuredContent != nil {
		t.Errorf("go_tree must not emit structured content, got %v", res.StructuredContent)
	}
	if !strings.Contains(contentText(res), "source_file") {
		t.Errorf("tree missing from content:\n%s", contentText(res))
	}
}

// A test-only import is not a dependency of the package.
func TestDepsSeparatesTestImports(t *testing.T) {
	h := harnessWithFile(t, "proj", "other/other_test.go", `package other

import (
	"strings"
	"sync"
	"testing"
)

func TestHelper(t *testing.T) {
	if !strings.Contains(Helper(), "other") {
		t.Fatal("bad")
	}
}
`)
	var res struct {
		Packages []struct {
			Package       string   `json:"package"`
			Imports       []string `json:"imports"`
			TestImports   []string `json:"test_imports"`
			StdlibImports []string `json:"stdlib_imports"`
		} `json:"packages"`
	}
	h.structured("go_deps", map[string]any{"package": "example.com/proj/other"}, &res)
	if len(res.Packages) != 1 {
		t.Fatalf("packages = %d, want 1", len(res.Packages))
	}
	p := res.Packages[0]
	for _, testOnly := range []string{"testing", "strings"} {
		if slices.Contains(p.Imports, testOnly) || slices.Contains(p.StdlibImports, testOnly) {
			t.Errorf("test-only import %q reported as a package dependency: %v / %v",
				testOnly, p.Imports, p.StdlibImports)
		}
	}
}

func TestSearchSymbols(t *testing.T) {
	h := newHarness(t, "proj")

	type result struct {
		Symbols []struct {
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			Recv    string `json:"recv"`
			File    string `json:"file"`
			Line    int    `json:"line"`
			Package string `json:"package"`
		} `json:"symbols"`
		Total int `json:"total"`
	}

	t.Run("substring finds both packages", func(t *testing.T) {
		var r result
		h.structured("go_search_symbols", map[string]any{"pattern": "Config"}, &r)
		if r.Total != 2 {
			t.Fatalf("total = %d, want 2 (demo.Config and other.Config)\n%+v", r.Total, r.Symbols)
		}
		for _, s := range r.Symbols {
			if s.Kind != "type" {
				t.Errorf("kind = %q, want type", s.Kind)
			}
		}
	})

	t.Run("exact excludes near misses", func(t *testing.T) {
		var r result
		h.structured("go_search_symbols", map[string]any{"pattern": "Load", "mode": "exact"}, &r)
		if r.Total != 1 || r.Symbols[0].Name != "Load" {
			t.Errorf("exact Load -> %+v", r.Symbols)
		}
	})

	t.Run("fuzzy matches a subsequence", func(t *testing.T) {
		var r result
		h.structured("go_search_symbols", map[string]any{"pattern": "srvr", "mode": "fuzzy"}, &r)
		var names []string
		for _, s := range r.Symbols {
			names = append(names, s.Name)
		}
		if !slices.Contains(names, "Server") {
			t.Errorf("fuzzy srvr did not find Server; got %v", names)
		}
	})

	t.Run("regex mode", func(t *testing.T) {
		var r result
		h.structured("go_search_symbols", map[string]any{"pattern": "^Test", "mode": "regex"}, &r)
		if r.Total != 1 || r.Symbols[0].Name != "TestLoad" {
			t.Errorf("regex ^Test -> %+v", r.Symbols)
		}
	})

	t.Run("kind filter and receiver", func(t *testing.T) {
		var r result
		h.structured("go_search_symbols", map[string]any{"pattern": "Addr", "kinds": []string{"method"}}, &r)
		if r.Total != 1 {
			t.Fatalf("total = %d, want 1\n%+v", r.Total, r.Symbols)
		}
		if !strings.Contains(r.Symbols[0].Recv, "Server") {
			t.Errorf("recv = %q, want it to mention Server", r.Symbols[0].Recv)
		}
	})

	t.Run("exported only", func(t *testing.T) {
		var all, exported result
		h.structured("go_search_symbols", map[string]any{"pattern": "o", "kinds": []string{"func"}}, &all)
		h.structured("go_search_symbols", map[string]any{"pattern": "o", "kinds": []string{"func"}, "exported_only": true}, &exported)
		if exported.Total >= all.Total {
			t.Errorf("exported_only did not narrow: %d vs %d", exported.Total, all.Total)
		}
	})

	t.Run("works on code that does not compile", func(t *testing.T) {
		hb := newHarness(t, "broken")
		var r result
		hb.structured("go_search_symbols", map[string]any{"pattern": "Good"}, &r)
		if r.Total != 1 {
			t.Errorf("total = %d, want 1 in a file with a syntax error\n%+v", r.Total, r.Symbols)
		}
	})
}

func TestSearchTextScopesToSyntax(t *testing.T) {
	h := newHarness(t, "proj")
	var r struct {
		Total   int `json:"total"`
		Matches []struct {
			File     string `json:"file"`
			Captures []struct {
				Text string `json:"text"`
			} `json:"captures"`
		} `json:"matches"`
	}

	// "Config" appears in comments, in identifiers, and nowhere in strings.
	h.structured("go_search_text", map[string]any{"pattern": "Config", "in": "comments"}, &r)
	comments := r.Total
	if comments == 0 {
		t.Error("no comment hits for Config")
	}
	for _, m := range r.Matches {
		if !strings.HasPrefix(strings.TrimSpace(m.Captures[0].Text), "//") {
			t.Errorf("comment search returned a non-comment: %q", m.Captures[0].Text)
		}
	}

	h.structured("go_search_text", map[string]any{"pattern": "Config", "in": "strings"}, &r)
	if r.Total != 0 {
		t.Errorf("string search for Config = %d, want 0", r.Total)
	}

	h.structured("go_search_text", map[string]any{"pattern": "Config", "in": "identifiers"}, &r)
	if r.Total <= comments {
		t.Errorf("identifier hits (%d) should exceed comment hits (%d)", r.Total, comments)
	}

	h.callExpectError("go_search_text", map[string]any{"pattern": "x", "in": "nonsense"})
}

func TestImplements(t *testing.T) {
	h := newHarness(t, "refactor")

	var iface struct {
		IsInterface bool `json:"is_interface"`
		Results     []struct {
			Type    string `json:"type"`
			Kind    string `json:"kind"`
			Package string `json:"package"`
		} `json:"results"`
	}
	h.structured("go_implements", map[string]any{"symbol": "app.Namer"}, &iface)
	if !iface.IsInterface {
		t.Error("Namer not reported as an interface")
	}
	var types []string
	for _, r := range iface.Results {
		types = append(types, r.Type)
	}
	if !slices.Contains(types, "Config") {
		t.Errorf("base.Config not listed as implementing Namer; got %v", types)
	}

	// The reverse direction, including stdlib interfaces.
	var concrete struct {
		IsInterface bool `json:"is_interface"`
		Results     []struct {
			Type    string `json:"type"`
			Package string `json:"package"`
		} `json:"results"`
	}
	h.structured("go_implements", map[string]any{"symbol": "example.com/refactor/base.Config"}, &concrete)
	if concrete.IsInterface {
		t.Error("Config reported as an interface")
	}
	var ifaces []string
	for _, r := range concrete.Results {
		ifaces = append(ifaces, r.Type)
	}
	if !slices.Contains(ifaces, "Namer") {
		t.Errorf("Config should satisfy Namer; got %v", ifaces)
	}
	if !slices.Contains(ifaces, "Stringer") {
		t.Errorf("Config has String() so it should satisfy fmt.Stringer; got %v", ifaces)
	}

	h.callExpectError("go_implements", map[string]any{"symbol": "example.com/refactor/base.Config.String"})
}

func TestCallers(t *testing.T) {
	h := newHarness(t, "proj")
	var out struct {
		Calls []struct {
			File string `json:"file"`
			Line int    `json:"line"`
			In   string `json:"in"`
		} `json:"calls"`
		Total int `json:"total"`
	}
	h.structured("go_callers", map[string]any{"symbol": "demo.Load"}, &out)
	if out.Total != 1 {
		t.Fatalf("total = %d, want 1\n%+v", out.Total, out.Calls)
	}
	if out.Calls[0].In != "TestLoad" {
		t.Errorf("enclosing declaration = %q, want TestLoad", out.Calls[0].In)
	}
	if out.Calls[0].File != "demo/demo_test.go" {
		t.Errorf("file = %q", out.Calls[0].File)
	}

	// A method receiver must be named in the enclosing declaration.
	hr := newHarness(t, "refactor")
	var m struct {
		Calls []struct {
			In string `json:"in"`
		} `json:"calls"`
		Total int `json:"total"`
	}
	hr.structured("go_callers", map[string]any{"symbol": "example.com/refactor/base.Config.String"}, &m)
	if m.Total == 0 {
		t.Fatal("no callers found for Config.String")
	}
	if m.Calls[0].In != "use" {
		t.Errorf("enclosing declaration = %q, want use", m.Calls[0].In)
	}

	h.callExpectError("go_callers", map[string]any{"symbol": "demo.Config"})
}

func TestCheckDetectsBreakageThatParses(t *testing.T) {
	h := newHarness(t, "proj")

	var clean struct {
		OK       bool `json:"ok"`
		Packages int  `json:"packages"`
	}
	h.structured("go_check", map[string]any{}, &clean)
	if !clean.OK {
		t.Fatalf("fixture should type-check cleanly: %+v", clean)
	}

	// Change a call's argument count. This parses fine and passes every gate
	// the edit tools apply, but does not compile.
	var edit tools.ChangeOutput
	h.structured("go_rewrite", map[string]any{
		"query":    `(call_expression function: (identifier) @f (#eq? @f "Load")) @call`,
		"target":   "call",
		"template": "Load(1, 2, 3)",
		"selector": "./demo",
		"apply":    true,
	}, &edit)
	if !edit.Applied {
		t.Fatalf("rewrite not applied: %+v", edit)
	}

	var broken struct {
		OK          bool `json:"ok"`
		Diagnostics []struct {
			File    string `json:"file"`
			Line    int    `json:"line"`
			Message string `json:"message"`
		} `json:"diagnostics"`
	}
	h.structured("go_check", map[string]any{}, &broken)
	if broken.OK {
		t.Fatal("go_check reported OK after an edit that does not compile")
	}
	if len(broken.Diagnostics) == 0 {
		t.Fatal("no diagnostics reported")
	}
	if !strings.Contains(broken.Diagnostics[0].File, "demo") {
		t.Errorf("diagnostic should point at the edited file: %+v", broken.Diagnostics[0])
	}
}

// The instructions are the only place that explains which of several search
// tools answers which question, so a rename that leaves them stale is a real
// documentation regression.
func TestInstructionsReachTheClientAndNameEveryTool(t *testing.T) {
	h := newHarness(t, "proj")
	res, err := h.session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := h.session.InitializeResult().Instructions; got == "" {
		t.Fatal("server sent no instructions")
	} else if got != tools.Instructions {
		t.Error("instructions delivered to the client differ from the source")
	}
	for _, tool := range res.Tools {
		if !strings.Contains(tools.Instructions, tool.Name) {
			t.Errorf("tool %s is registered but never mentioned in the instructions", tool.Name)
		}
	}
}

// A universe-scope type such as error has no package. Dereferencing it panicked
// and killed the whole server; both the crash and the fragility are covered.
func TestImplementsHandlesBuiltinTypes(t *testing.T) {
	h := newHarness(t, "refactor")
	var out struct {
		Results []struct {
			Type    string `json:"type"`
			Package string `json:"package"`
		} `json:"results"`
	}
	// base.Config has an Error-shaped method set only via String, but the walk
	// still visits the universe error type on the way.
	h.structured("go_implements", map[string]any{"symbol": "example.com/refactor/base.Config"}, &out)

	// And a type that really does implement error must report it.
	hp := newHarness(t, "proj")
	var errOut struct {
		Results []struct {
			Type    string `json:"type"`
			Package string `json:"package"`
		} `json:"results"`
	}
	hp.structured("go_implements", map[string]any{"symbol": "demo.Config"}, &errOut)

	// The server must still be alive after both calls.
	if _, err := h.session.ListTools(t.Context(), nil); err != nil {
		t.Fatalf("server died: %v", err)
	}
}

func TestHandlerPanicDoesNotKillTheServer(t *testing.T) {
	h := newHarness(t, "proj")
	// Drive a few odd inputs through every tool, then confirm the session lives.
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"go_implements", map[string]any{"symbol": "builtin.error"}},
		{"go_implements", map[string]any{"symbol": "demo.Config", "selector": "./nonexistent/..."}},
		{"go_callers", map[string]any{"symbol": "demo.Load", "selector": "./..."}},
		{"go_tree", map[string]any{"file": "demo/demo.go", "start_line": 9999}},
		{"go_query", map[string]any{"query": "((((("}},
		{"go_search_symbols", map[string]any{"pattern": "[", "mode": "regex"}},
	} {
		h.callRaw(tc.tool, tc.args) // may error; must not crash
	}
	res, err := h.session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("server did not survive: %v", err)
	}
	if len(res.Tools) != wantToolCount {
		t.Errorf("tools = %d, want %d", len(res.Tools), wantToolCount)
	}
}

// A globally registered server is launched once and used against whatever
// project the user is in, so one server must serve several modules.
func TestOneServerServesSeveralModules(t *testing.T) {
	projDir := copyTree(t, filepath.Join("..", "..", "testdata", "proj"))
	refacDir := copyTree(t, filepath.Join("..", "..", "testdata", "refactor"))

	// Default root is somewhere else entirely, so every call must carry root.
	srv, err := tools.New(tools.Config{DefaultRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"},
		&mcp.ServerOptions{Instructions: tools.Instructions})
	srv.Register(m)
	clientT, serverT := mcp.NewInMemoryTransports()
	if _, err := m.Connect(t.Context(), serverT, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "v0"}, nil).
		Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	h := &harness{t: t, dir: projDir, session: session}

	t.Run("reads each module by root", func(t *testing.T) {
		a := h.call("go_read", map[string]any{"root": projDir, "selector": "./..."})
		b := h.call("go_read", map[string]any{"root": refacDir, "selector": "./..."})
		if !strings.Contains(a, "; 3 file(s)") {
			t.Errorf("proj read header: %s", firstLines(a, 1))
		}
		if !strings.Contains(b, "; 2 file(s)") {
			t.Errorf("refactor read header: %s", firstLines(b, 1))
		}
	})

	t.Run("workspace reports the resolved module", func(t *testing.T) {
		var w tools.WorkspaceOutput
		h.structured("go_workspace", map[string]any{"root": projDir}, &w)
		if w.Module != "example.com/proj" {
			t.Errorf("module = %q, want example.com/proj", w.Module)
		}
		h.structured("go_workspace", map[string]any{"root": refacDir}, &w)
		if w.Module != "example.com/refactor" {
			t.Errorf("module = %q, want example.com/refactor", w.Module)
		}
	})

	t.Run("a subdirectory resolves up to its module", func(t *testing.T) {
		var w tools.WorkspaceOutput
		h.structured("go_workspace", map[string]any{"root": filepath.Join(projDir, "demo")}, &w)
		if w.Root != projDir {
			t.Errorf("root = %q, want the module root %q", w.Root, projDir)
		}
	})

	t.Run("renames stay within their own module", func(t *testing.T) {
		var out tools.ChangeOutput
		h.structured("go_rename", map[string]any{
			"root": projDir, "symbol": "example.com/proj/demo.Config",
			"new_name": "Settings", "apply": true,
		}, &out)
		if !out.Applied {
			t.Fatalf("rename not applied: %+v", out)
		}
		got, err := os.ReadFile(filepath.Join(projDir, "demo", "demo.go"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "type Settings struct") {
			t.Error("rename did not land in proj")
		}
		other, err := os.ReadFile(filepath.Join(refacDir, "base", "base.go"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(other), "type Config struct") {
			t.Error("rename leaked into the other module")
		}
	})
}

func TestAllowlistRestrictsRoots(t *testing.T) {
	allowed := copyTree(t, filepath.Join("..", "..", "testdata", "proj"))
	forbidden := copyTree(t, filepath.Join("..", "..", "testdata", "refactor"))

	srv, err := tools.New(tools.Config{DefaultRoot: allowed, Allowed: []string{allowed}})
	if err != nil {
		t.Fatal(err)
	}
	m := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	srv.Register(m)
	clientT, serverT := mcp.NewInMemoryTransports()
	if _, err := m.Connect(t.Context(), serverT, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "v0"}, nil).
		Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	h := &harness{t: t, dir: allowed, session: session}

	h.call("go_read", map[string]any{"root": allowed, "selector": "./..."})
	msg := h.callExpectError("go_read", map[string]any{"root": forbidden, "selector": "./..."})
	if !strings.Contains(msg, "not allowed") {
		t.Errorf("error should say the root is not allowed, got: %s", msg)
	}
}

func TestDeps(t *testing.T) {
	h := newHarness(t, "proj")
	var res struct {
		Packages []struct {
			Package       string   `json:"package"`
			Imports       []string `json:"imports"`
			ImportedBy    []string `json:"imported_by"`
			StdlibImports []string `json:"stdlib_imports"`
		} `json:"packages"`
		Scanned int `json:"scanned"`
	}

	h.structured("go_deps", map[string]any{"package": "example.com/proj/other"}, &res)
	if len(res.Packages) != 1 {
		t.Fatalf("packages = %d, want 1\n%+v", len(res.Packages), res.Packages)
	}
	p := res.Packages[0]
	if !slices.Contains(p.ImportedBy, "example.com/proj/demo") {
		t.Errorf("imported_by = %v, want it to include the demo package", p.ImportedBy)
	}

	h.structured("go_deps", map[string]any{"package": "demo"}, &res)
	if len(res.Packages) == 0 {
		t.Fatal("bare package name did not match")
	}
	d := res.Packages[0]
	if !slices.Contains(d.Imports, "example.com/proj/other") {
		t.Errorf("imports = %v, want the other package", d.Imports)
	}
	if !slices.Contains(d.StdlibImports, "fmt") {
		t.Errorf("stdlib_imports = %v, want fmt separated out", d.StdlibImports)
	}
	if slices.Contains(d.Imports, "fmt") {
		t.Errorf("fmt should not appear in non-stdlib imports: %v", d.Imports)
	}
}

func TestMoveDeclaration(t *testing.T) {
	h := newHarness(t, "proj")

	t.Run("into a new file", func(t *testing.T) {
		var out tools.ChangeOutput
		h.structured("go_move", map[string]any{
			"symbol": "demo.Server", "to_file": "demo/server.go", "apply": true,
		}, &out)
		if !out.Applied {
			t.Fatalf("move not applied: %+v", out)
		}
		moved := h.read("demo/server.go")
		if !strings.Contains(moved, "package demo") {
			t.Errorf("new file missing package clause:\n%s", moved)
		}
		if !strings.Contains(moved, "type Server struct") {
			t.Errorf("declaration not in the new file:\n%s", moved)
		}
		if !strings.Contains(moved, "// Server holds a field") {
			t.Errorf("doc comment did not travel with the declaration:\n%s", moved)
		}
		if orig := h.read("demo/demo.go"); strings.Contains(orig, "type Server struct") {
			t.Errorf("declaration still in the source file:\n%s", orig)
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build after move:\n%v", err)
		}
	})

	// Load returns *Config and is called from demo's own test, so moving it to
	// other would need other -> demo and demo -> other at once.
	t.Run("a cross-package move that would cycle is refused", func(t *testing.T) {
		msg := h.callExpectError("go_move", map[string]any{
			"symbol": "demo.Load", "to_file": "other/load.go",
		})
		if !strings.Contains(msg, "import each other") {
			t.Errorf("error should name the cycle, got: %s", msg)
		}
	})
}

// harnessWithFile copies a fixture and adds one extra file, for tests that
// need a shape the shared fixture does not have.
func harnessWithFile(t *testing.T, fixture, rel, content string) *harness {
	t.Helper()
	dir := copyTree(t, filepath.Join("..", "..", "testdata", fixture))
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, err := tools.New(tools.Config{DefaultRoot: dir})
	if err != nil {
		t.Fatal(err)
	}
	m := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	srv.Register(m)
	clientT, serverT := mcp.NewInMemoryTransports()
	if _, err := m.Connect(t.Context(), serverT, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "v0"}, nil).Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return &harness{t: t, dir: dir, session: session}
}

func TestExtractFunction(t *testing.T) {
	h := harnessWithFile(t, "proj", "demo/compute.go", `package demo

func Compute(a, b int) int {
	sum := a + b
	doubled := sum * 2
	shifted := doubled + 1
	return shifted
}
`)

	t.Run("computes params and results", func(t *testing.T) {
		var out tools.ChangeOutput
		h.structured("go_extract", map[string]any{
			"file": "demo/compute.go", "from_line": 5, "to_line": 6, "name": "adjust", "apply": true,
		}, &out)
		if !out.Applied {
			t.Fatalf("extract not applied: %+v", out)
		}
		got := h.read("demo/compute.go")
		// sum is read but declared outside -> parameter.
		// shifted is assigned and read after -> result.
		if !strings.Contains(got, "func adjust(sum int) int") {
			t.Errorf("unexpected signature:\n%s", got)
		}
		if !strings.Contains(got, "shifted := adjust(sum)") && !strings.Contains(got, "shifted = adjust(sum)") {
			t.Errorf("call site not written:\n%s", got)
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build after extract:\n%v", err)
		}
	})

	t.Run("refuses a range containing return", func(t *testing.T) {
		msg := h.callExpectError("go_extract", map[string]any{
			"file": "demo/compute.go", "from_line": 4, "to_line": 20, "name": "nope",
		})
		if !strings.Contains(msg, "return") && !strings.Contains(msg, "not inside a single function") {
			t.Errorf("error should explain the refusal, got: %s", msg)
		}
	})
}

func TestChangeSignature(t *testing.T) {
	h := harnessWithFile(t, "proj", "demo/pair.go", `package demo

// Pair joins two strings.
func Pair(left string, right int) string {
	_ = right
	return left
}

func usePair() string {
	return Pair("a", 1) + Pair("b", 2)
}
`)

	t.Run("reorders parameters at every call site", func(t *testing.T) {
		var out tools.ChangeOutput
		h.structured("go_signature", map[string]any{
			"symbol": "demo.Pair",
			"params": []map[string]any{{"from": 1}, {"from": 0}},
			"apply":  true,
		}, &out)
		if !out.Applied {
			t.Fatalf("signature change not applied: %+v", out)
		}
		got := h.read("demo/pair.go")
		if !strings.Contains(got, "func Pair(right int, left string) string") {
			t.Errorf("declaration not reordered:\n%s", got)
		}
		if !strings.Contains(got, `Pair(1, "a")`) || !strings.Contains(got, `Pair(2, "b")`) {
			t.Errorf("call sites not reordered:\n%s", got)
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build after reorder:\n%v", err)
		}
	})

	t.Run("adds a leading parameter with a value for existing calls", func(t *testing.T) {
		var out tools.ChangeOutput
		h.structured("go_signature", map[string]any{
			"symbol": "demo.Load",
			"params": []map[string]any{
				{"from": -1, "name": "name", "type": "string", "value": `"x"`},
			},
			"apply": true,
		}, &out)
		if !out.Applied {
			t.Fatalf("signature change not applied: %+v", out)
		}
		got := h.read("demo/demo.go")
		if !strings.Contains(got, "func Load(name string)") {
			t.Errorf("declaration not updated:\n%s", got)
		}
		test := h.read("demo/demo_test.go")
		if !strings.Contains(test, `Load("x")`) {
			t.Errorf("call site in the test file not updated:\n%s", test)
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build after signature change:\n%v", err)
		}
	})

	t.Run("refuses a variadic function", func(t *testing.T) {
		msg := h.callExpectError("go_signature", map[string]any{
			"symbol": "demo.logf",
			"params": []map[string]any{{"from": 0}, {"from": 1}},
		})
		if !strings.Contains(msg, "variadic") {
			t.Errorf("error should mention variadic, got: %s", msg)
		}
	})
}

// Cross-package moves are the hard case: references change in three different
// directions and each has to compile afterwards.
func TestMoveAcrossPackages(t *testing.T) {
	t.Run("requalifies in all directions", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/movable.go", `package demo

// Widget is referenced from its own package and from outside it.
type Widget struct{ N int }

func useLocally() int { return Widget{N: 1}.N }
`)
		// A third package that reaches Widget through the demo qualifier.
		if err := os.MkdirAll(filepath.Join(h.dir, "consumer"), 0o755); err != nil {
			t.Fatal(err)
		}
		consumer := `package consumer

import "example.com/proj/demo"

func Use() int { return demo.Widget{N: 2}.N }
`
		if err := os.WriteFile(filepath.Join(h.dir, "consumer", "c.go"), []byte(consumer), 0o644); err != nil {
			t.Fatal(err)
		}

		var out tools.ChangeOutput
		h.structured("go_move", map[string]any{
			"symbol": "demo.Widget", "to_file": "other/widget.go", "apply": true,
		}, &out)
		if !out.Applied {
			t.Fatalf("move not applied: %+v", out)
		}

		// Landed in the destination package, unqualified.
		moved := h.read("other/widget.go")
		if !strings.Contains(moved, "package other") || !strings.Contains(moved, "type Widget struct") {
			t.Errorf("declaration not in the destination:\n%s", moved)
		}
		if !strings.Contains(moved, "// Widget is referenced") {
			t.Errorf("doc comment did not travel:\n%s", moved)
		}
		// Source package now qualifies it.
		src := h.read("demo/movable.go")
		if !strings.Contains(src, "other.Widget{N: 1}") {
			t.Errorf("source package reference not requalified:\n%s", src)
		}
		if strings.Contains(src, "type Widget struct") {
			t.Errorf("declaration still in the source file:\n%s", src)
		}
		// Third package swapped one qualifier for the other.
		c := h.read("consumer/c.go")
		if !strings.Contains(c, "other.Widget{N: 2}") {
			t.Errorf("third-party reference not requalified:\n%s", c)
		}
		if strings.Contains(c, "demo.Widget") {
			t.Errorf("stale qualifier left behind:\n%s", c)
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build after cross-package move:\n%v", err)
		}
	})

	t.Run("into a package that does not exist yet", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/lonely.go", `package demo

// Lonely has no dependencies of its own.
type Lonely struct{ A string }
`)
		var out tools.ChangeOutput
		h.structured("go_move", map[string]any{
			"symbol": "demo.Lonely", "to_file": "brandnew/lonely.go", "apply": true,
		}, &out)
		if !out.Applied {
			t.Fatalf("move not applied: %+v", out)
		}
		got := h.read("brandnew/lonely.go")
		if !strings.Contains(got, "package brandnew") {
			t.Errorf("new package clause wrong:\n%s", got)
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build after move into a new package:\n%v", err)
		}
	})

	t.Run("qualifies the moved body back to its old package", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/dep.go", `package demo

// Helper is exported and stays put.
func Helper() int { return 7 }

// Mover uses Helper, so after moving it must say demo.Helper.
func Mover() int { return Helper() }
`)
		var out tools.ChangeOutput
		// A package demo does not already import, so requiring the reverse
		// import is not a cycle.
		h.structured("go_move", map[string]any{
			"symbol": "demo.Mover", "to_file": "mover/mover.go", "apply": true,
		}, &out)
		if !out.Applied {
			t.Fatalf("move not applied: %+v", out)
		}
		got := h.read("mover/mover.go")
		if !strings.Contains(got, "demo.Helper()") {
			t.Errorf("body not requalified to the old package:\n%s", got)
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build:\n%v", err)
		}
	})
}

func TestMoveAcrossPackagesRefusals(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		content string
		symbol  string
		to      string
		want    string
	}{
		{
			name: "uses an unexported symbol of the source package",
			file: "demo/priv.go",
			content: `package demo

func secret() int { return 1 }

// Public cannot leave: it needs secret.
func Public() int { return secret() }
`,
			symbol: "demo.Public", to: "other/public.go",
			want: "unexported",
		},
		{
			name: "unexported and referenced from elsewhere",
			file: "demo/hidden.go",
			content: `package demo

func hidden() int { return 1 }

func usesHidden() int { return hidden() }
`,
			symbol: "demo.hidden", to: "other/hidden.go",
			want: "unexported",
		},
		{
			name: "would create an import cycle",
			file: "demo/cyc.go",
			content: `package demo

// Exported is used by the source package and needs the source package.
func Exported() int { return Also() }

func Also() int { return 2 }

func caller() int { return Exported() }
`,
			symbol: "demo.Exported", to: "other/cyc.go",
			want: "import each other",
		},
		{
			name: "a method cannot leave its receiver",
			file: "demo/meth.go",
			content: `package demo

type Holder struct{}

// Grab is a method.
func (h Holder) Grab() int { return 1 }
`,
			symbol: "demo.Holder.Grab", to: "other/grab.go",
			want: "method",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := harnessWithFile(t, "proj", tt.file, tt.content)
			before := h.read(tt.file)
			msg := h.callExpectError("go_move", map[string]any{
				"symbol": tt.symbol, "to_file": tt.to, "apply": true,
			})
			if !strings.Contains(msg, tt.want) {
				t.Errorf("error should mention %q, got: %s", tt.want, msg)
			}
			if h.read(tt.file) != before {
				t.Error("a refused move still modified the source file")
			}
			if err := h.buildsCleanly(); err != nil {
				t.Errorf("refused move left the module broken:\n%v", err)
			}
		})
	}
}

// The case that made an agent give up on the tool: splitting a package means
// moving a cluster whose members use each other's unexported symbols. Judged
// one declaration at a time every one looks like it abandons a dependency.
func TestMovePackageSplit(t *testing.T) {
	const cluster = `package demo

// Encoder is the exported entry point of the cluster.
type Encoder struct{ prefix string }

// NewEncoder builds one.
func NewEncoder(p string) *Encoder { return &Encoder{prefix: p} }

// Encode uses two unexported helpers and a method.
func (e *Encoder) Encode(s string) string { return e.prefix + normalize(pad(s)) }

func normalize(s string) string { return s + "!" }

func pad(s string) string { return " " + s }
`

	t.Run("whole file moves as one unit", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/encoder.go", cluster)
		var out tools.ChangeOutput
		h.structured("go_move", map[string]any{
			"files": []string{"demo/encoder.go"}, "to": "codec", "apply": true,
		}, &out)
		if !out.Applied {
			t.Fatalf("split not applied: %+v", out)
		}
		got := h.read("codec/encoder.go")
		for _, want := range []string{
			"package codec",
			"type Encoder struct",
			"func NewEncoder(",
			"func (e *Encoder) Encode(",
			"func normalize(", // the unexported helpers came along
			"func pad(",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in the split package:\n%s", want, got)
			}
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build after the split:\n%v", err)
		}
	})

	t.Run("naming the cluster explicitly works too", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/encoder.go", cluster)
		var out tools.ChangeOutput
		h.structured("go_move", map[string]any{
			"symbols": []string{"demo.Encoder", "demo.NewEncoder", "demo.normalize", "demo.pad"},
			"to":      "codec/encoder.go", "apply": true,
		}, &out)
		if !out.Applied {
			t.Fatalf("split not applied: %+v", out)
		}
		got := h.read("codec/encoder.go")
		// Encode is a method on Encoder, so it must have been pulled in.
		if !strings.Contains(got, "func (e *Encoder) Encode(") {
			t.Errorf("method did not follow its receiver:\n%s", got)
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build:\n%v", err)
		}
	})

	t.Run("include_dependencies pulls the helpers in", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/encoder.go", cluster)
		var out tools.ChangeOutput
		h.structured("go_move", map[string]any{
			"symbols":              []string{"demo.Encoder", "demo.NewEncoder"},
			"to":                   "codec/encoder.go",
			"include_dependencies": true,
			"apply":                true,
		}, &out)
		if !out.Applied {
			t.Fatalf("split not applied: %+v", out)
		}
		got := h.read("codec/encoder.go")
		for _, want := range []string{"func normalize(", "func pad("} {
			if !strings.Contains(got, want) {
				t.Errorf("dependency %q was not pulled in:\n%s", want, got)
			}
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build:\n%v", err)
		}
	})

	// Refusing is fine, but it has to say what to add.
	t.Run("refusal names the missing dependencies", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/encoder.go", cluster)
		msg := h.callExpectError("go_move", map[string]any{
			"symbols": []string{"demo.Encoder", "demo.NewEncoder"},
			"to":      "codec/encoder.go",
		})
		for _, want := range []string{"normalize", "pad", "include_dependencies"} {
			if !strings.Contains(msg, want) {
				t.Errorf("refusal should mention %q, got: %s", want, msg)
			}
		}
	})

	// The reverse: taking a helper away from code that stays.
	t.Run("refuses stranding a helper the old package still uses", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/shared.go", `package demo

func shared() int { return 1 }

func StaysBehind() int { return shared() }

func Leaves() int { return shared() }
`)
		msg := h.callExpectError("go_move", map[string]any{
			"symbols": []string{"demo.Leaves"}, "to": "codec/leaves.go",
			"include_dependencies": true,
		})
		if !strings.Contains(msg, "shared") || !strings.Contains(msg, "still use") {
			t.Errorf("refusal should name the stranded symbol, got: %s", msg)
		}
	})
}

// A method is reached through its receiver, never through a package qualifier.
// Requalifying one produced res.newpkg.Method, which the fixtures did not catch
// because none of them called a moved method from another package.
func TestMoveDoesNotRequalifyMethodCalls(t *testing.T) {
	h := harnessWithFile(t, "proj", "demo/report.go", `package demo

// Report is moved along with its method.
type Report struct{ N int }

// Describe is called from another package through a value.
func (r *Report) Describe() int { return r.N }

// Build makes one.
func Build() *Report { return &Report{N: 1} }
`)
	if err := os.MkdirAll(filepath.Join(h.dir, "caller"), 0o755); err != nil {
		t.Fatal(err)
	}
	caller := `package caller

import "example.com/proj/demo"

func Run() int {
	r := demo.Build()
	return r.Describe()
}
`
	if err := os.WriteFile(filepath.Join(h.dir, "caller", "c.go"), []byte(caller), 0o644); err != nil {
		t.Fatal(err)
	}

	var out tools.ChangeOutput
	h.structured("go_move", map[string]any{
		"files": []string{"demo/report.go"}, "to": "reporting", "apply": true,
	}, &out)
	if !out.Applied {
		t.Fatalf("move not applied: %+v", out)
	}

	got := h.read("caller/c.go")
	if !strings.Contains(got, "r.Describe()") {
		t.Errorf("method call through a receiver was rewritten:\n%s", got)
	}
	if strings.Contains(got, "r.reporting") {
		t.Errorf("method call gained a package qualifier:\n%s", got)
	}
	// The package-level function still needs requalifying.
	if !strings.Contains(got, "reporting.Build()") {
		t.Errorf("package-level call not requalified:\n%s", got)
	}
	if err := h.buildsCleanly(); err != nil {
		t.Errorf("module does not build after the move:\n%v", err)
	}
}

// A real codebase had 39 generic type declarations, and none of the fixtures
// exercised type parameters. These assert the semantic tools resolve through
// them rather than silently doing nothing.
func TestGenerics(t *testing.T) {
	h := harnessWithFile(t, "proj", "demo/generic.go", `package demo

// Box holds any value.
type Box[T any] struct{ V T }

// Unwrap returns the value.
func (b Box[T]) Unwrap() T { return b.V }

// Wrap builds a Box.
func Wrap[T any](v T) Box[T] { return Box[T]{V: v} }

func useBox() int {
	b := Wrap(3)
	return b.Unwrap()
}
`)

	t.Run("resolves a generic type", func(t *testing.T) {
		var out tools.SymbolOutput
		h.structured("go_symbol", map[string]any{"symbol": "demo.Box"}, &out)
		if out.Symbol.Kind != "type" || out.Symbol.Name != "Box" {
			t.Errorf("symbol = %+v", out.Symbol)
		}
	})

	t.Run("finds references through instantiation", func(t *testing.T) {
		var out struct {
			Total int `json:"total"`
		}
		h.structured("go_refs", map[string]any{"symbol": "demo.Box"}, &out)
		// declaration, the method receiver, and two uses in Wrap
		if out.Total < 4 {
			t.Errorf("refs = %d, want at least 4 for a generic type", out.Total)
		}
	})

	t.Run("callers of a generic function", func(t *testing.T) {
		var out struct {
			Total int `json:"total"`
			Calls []struct {
				In string `json:"in"`
			} `json:"calls"`
		}
		h.structured("go_callers", map[string]any{"symbol": "demo.Wrap"}, &out)
		if out.Total != 1 || out.Calls[0].In != "useBox" {
			t.Errorf("callers of a generic function = %+v", out)
		}
	})

	t.Run("renames a generic type everywhere", func(t *testing.T) {
		var out tools.ChangeOutput
		h.structured("go_rename", map[string]any{
			"symbol": "demo.Box", "new_name": "Holder", "apply": true,
		}, &out)
		if !out.Applied {
			t.Fatalf("rename not applied: %+v", out)
		}
		got := h.read("demo/generic.go")
		for _, want := range []string{
			"type Holder[T any] struct",
			"func (b Holder[T]) Unwrap()",
			"func Wrap[T any](v T) Holder[T]",
			"return Holder[T]{V: v}",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q after renaming a generic type:\n%s", want, got)
			}
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build after generic rename:\n%v", err)
		}
	})
}

func TestSeam(t *testing.T) {
	const cluster = `package demo

// Encoder is the exported entry point.
type Encoder struct{ prefix string }

func NewEncoder(p string) *Encoder { return &Encoder{prefix: p} }

func (e *Encoder) Encode(s string) string { return e.prefix + normalize(s) }

func normalize(s string) string { return s + "!" }
`

	t.Run("names the exact coupling without moving anything", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/enc.go", cluster)
		before := h.read("demo/enc.go")

		var rep struct {
			Moving      []string `json:"moving"`
			MissingDeps []string `json:"missing_deps"`
			Verdict     string   `json:"verdict"`
			Advice      string   `json:"advice"`
		}
		h.structured("go_seam", map[string]any{"symbols": []string{"demo.Encoder", "demo.NewEncoder"}}, &rep)

		if rep.Verdict != "needs_dependencies" {
			t.Errorf("verdict = %q, want needs_dependencies", rep.Verdict)
		}
		if !slices.Contains(rep.MissingDeps, "normalize") {
			t.Errorf("missing_deps = %v, want it to name normalize", rep.MissingDeps)
		}
		if !strings.Contains(rep.Advice, "normalize") {
			t.Errorf("advice should name the blocker: %s", rep.Advice)
		}
		// Encode is a method on Encoder, so it must be reported as coming along.
		if !slices.Contains(rep.Moving, "Encode") {
			t.Errorf("moving = %v, want the method pulled in", rep.Moving)
		}
		if h.read("demo/enc.go") != before {
			t.Error("go_seam modified the source")
		}
	})

	t.Run("clean verdict for a self-contained file", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/enc.go", cluster)
		var rep struct {
			Verdict  string   `json:"verdict"`
			Stranded []string `json:"stranded"`
			Inbound  []struct {
				Package string `json:"package"`
			} `json:"inbound"`
		}
		h.structured("go_seam", map[string]any{"files": []string{"demo/enc.go"}}, &rep)
		if rep.Verdict != "clean" {
			t.Errorf("verdict = %q, want clean for a self-contained file", rep.Verdict)
		}
		if len(rep.Stranded) != 0 {
			t.Errorf("stranded = %v, want none", rep.Stranded)
		}
	})

	t.Run("reports a cycle before anyone attempts it", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/cyc.go", `package demo

func Exported() int { return Also() }

func Also() int { return 2 }

func caller() int { return Exported() }
`)
		var rep struct {
			Cycle   string `json:"cycle"`
			Verdict string `json:"verdict"`
		}
		h.structured("go_seam", map[string]any{"symbols": []string{"demo.Exported"}}, &rep)
		if rep.Verdict != "blocked" || rep.Cycle == "" {
			t.Errorf("expected a blocked verdict naming the cycle, got %+v", rep)
		}
	})
}

func TestTestsFor(t *testing.T) {
	h := harnessWithFile(t, "proj", "demo/chain_test.go", `package demo

import "testing"

func TestTop(t *testing.T) {
	if middle() != 3 {
		t.Fatal("bad")
	}
}

func TestUnrelated(t *testing.T) {}
`)
	if err := os.WriteFile(filepath.Join(h.dir, "demo", "chain.go"), []byte(`package demo

func middle() int { return bottom() + 1 }

func bottom() int { return 2 }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Total int `json:"total"`
		Tests []struct {
			Test  string   `json:"test"`
			Chain []string `json:"chain"`
			Depth int      `json:"depth"`
		} `json:"tests"`
		Note string `json:"note"`
	}
	h.structured("go_tests_for", map[string]any{"symbol": "demo.bottom"}, &out)

	if out.Total != 1 {
		t.Fatalf("total = %d, want 1\n%+v", out.Total, out.Tests)
	}
	hit := out.Tests[0]
	if hit.Test != "TestTop" {
		t.Errorf("test = %q, want TestTop", hit.Test)
	}
	// Reached through middle, so depth 2 and a chain naming the hop.
	if hit.Depth != 2 {
		t.Errorf("depth = %d, want 2", hit.Depth)
	}
	if !slices.Contains(hit.Chain, "middle") {
		t.Errorf("chain = %v, want it to include middle", hit.Chain)
	}
	if !strings.Contains(out.Note, "under-reports") {
		t.Errorf("note should state the static-graph limit: %s", out.Note)
	}
}

func TestImplement(t *testing.T) {
	h := harnessWithFile(t, "proj", "demo/writer.go", `package demo

// Sink is a type that does not yet satisfy io.Writer.
type Sink struct{ n int }

// Count reports writes and fixes the receiver style as a pointer.
func (s *Sink) Count() int { return s.n }
`)

	t.Run("generates the missing method", func(t *testing.T) {
		var out tools.ChangeOutput
		h.structured("go_implement", map[string]any{
			"type": "demo.Sink", "interface": "io.Writer", "apply": true,
		}, &out)
		if !out.Applied {
			t.Fatalf("not applied: %+v", out)
		}
		got := h.read("demo/writer.go")
		// Receiver style must match the existing pointer method.
		if !strings.Contains(got, "func (s *Sink) Write(") {
			t.Errorf("generated method missing or wrong receiver:\n%s", got)
		}
		if !strings.Contains(got, "([]byte)") && !strings.Contains(got, "[]byte)") {
			t.Errorf("parameter type not rendered:\n%s", got)
		}
		if !strings.Contains(got, "(int, error)") {
			t.Errorf("results not rendered:\n%s", got)
		}
		if !strings.Contains(got, `panic("not implemented")`) {
			t.Errorf("body should panic rather than return zero values:\n%s", got)
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build after implementing:\n%v", err)
		}

		// And it now actually satisfies the interface.
		var impl struct {
			Results []struct {
				Type string `json:"type"`
			} `json:"results"`
		}
		h.structured("go_implements", map[string]any{"symbol": "demo.Sink"}, &impl)
		var names []string
		for _, r := range impl.Results {
			names = append(names, r.Type)
		}
		if !slices.Contains(names, "Writer") {
			t.Errorf("Sink still does not satisfy io.Writer; satisfies %v", names)
		}
	})

	t.Run("refuses when already satisfied", func(t *testing.T) {
		msg := h.callExpectError("go_implement", map[string]any{
			"type": "demo.Sink", "interface": "io.Writer",
		})
		if !strings.Contains(msg, "already satisfies") {
			t.Errorf("error should say it is already satisfied, got: %s", msg)
		}
	})
}

func TestBulkSignatureWithRest(t *testing.T) {
	h := harnessWithFile(t, "proj", "demo/svc.go", `package demo

// Alpha and Beta have different arities, which is why rest exists.
func Alpha(a int) int { return a }

func Beta(a, b string) string { return a + b }

func callThem() string {
	_ = Alpha(1)
	return Beta("x", "y")
}
`)

	t.Run("threads a context through functions of differing arity", func(t *testing.T) {
		var out tools.ChangeOutput
		h.structured("go_signature", map[string]any{
			"symbols": []string{"demo.Alpha", "demo.Beta"},
			"params": []map[string]any{
				{"from": -1, "name": "ctx", "type": "context.Context", "value": "context.TODO()"},
				{"rest": true},
			},
			"apply": true,
		}, &out)
		if !out.Applied {
			t.Fatalf("bulk change not applied: %+v", out)
		}
		got := h.read("demo/svc.go")
		for _, want := range []string{
			"func Alpha(ctx context.Context, a int) int",
			// Grouped parameters flatten: indices must line up one-to-one
			// with call arguments.
			"func Beta(ctx context.Context, a string, b string) string",
			"Alpha(context.TODO(), 1)",
			`Beta(context.TODO(), "x", "y")`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q:\n%s", want, got)
			}
		}
		if !strings.Contains(got, `"context"`) {
			t.Errorf("context import not added:\n%s", got)
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build after bulk signature change:\n%v", err)
		}
	})

	t.Run("refuses nested changing calls rather than corrupting them", func(t *testing.T) {
		h2 := harnessWithFile(t, "proj", "demo/nest.go", `package demo

func Inner(a int) int { return a }

func Outer(a int) int { return a }

func nested() int { return Outer(Inner(1)) }
`)
		msg := h2.callExpectError("go_signature", map[string]any{
			"symbols": []string{"demo.Inner", "demo.Outer"},
			"params": []map[string]any{
				{"from": -1, "name": "ctx", "type": "context.Context", "value": "context.TODO()"},
				{"rest": true},
			},
		})
		if !strings.Contains(msg, "nested") {
			t.Errorf("error should name the nesting, got: %s", msg)
		}
	})
}

func TestFormat(t *testing.T) {
	const ugly = "package demo\n\nimport (\n\t\"os\"\n\t\"fmt\"\n)\n\nfunc Ugly(  a int )    int {\nreturn a+1\n}\n\nvar _ = fmt.Sprint\n"

	t.Run("check_only reports without writing", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/ugly.go", ugly)
		out := h.call("go_format", map[string]any{"check_only": true})
		if !strings.Contains(out, "demo/ugly.go") {
			t.Errorf("unformatted file not reported:\n%s", out)
		}
		if h.read("demo/ugly.go") != ugly {
			t.Error("check_only modified the file")
		}
	})

	t.Run("clean tree says so", func(t *testing.T) {
		h := newHarness(t, "proj")
		out := h.call("go_format", map[string]any{"check_only": true})
		if !strings.Contains(out, "all formatted") {
			t.Errorf("clean tree not reported as clean:\n%s", out)
		}
	})

	t.Run("goimports fixes spacing and the unused import", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/ugly.go", ugly)
		var out tools.ChangeOutput
		h.structured("go_format", map[string]any{"selector": "demo/ugly.go", "apply": true}, &out)
		if !out.Applied {
			t.Fatalf("format not applied: %+v", out)
		}
		got := h.read("demo/ugly.go")
		if !strings.Contains(got, "func Ugly(a int) int {") {
			t.Errorf("spacing not normalised:\n%s", got)
		}
		if strings.Contains(got, `"os"`) {
			t.Errorf("unused import not dropped by goimports:\n%s", got)
		}
		if err := h.buildsCleanly(); err != nil {
			t.Errorf("module does not build after formatting:\n%v", err)
		}
	})

	t.Run("no_imports leaves the import block alone", func(t *testing.T) {
		h := harnessWithFile(t, "proj", "demo/ugly.go", ugly)
		var out tools.ChangeOutput
		h.structured("go_format", map[string]any{
			"selector": "demo/ugly.go", "no_imports": true, "apply": true,
		}, &out)
		if !out.Applied {
			t.Fatalf("format not applied: %+v", out)
		}
		got := h.read("demo/ugly.go")
		if !strings.Contains(got, "func Ugly(a int) int {") {
			t.Errorf("gofmt did not run:\n%s", got)
		}
		// The whole point of no_imports: the unused import survives.
		if !strings.Contains(got, `"os"`) {
			t.Errorf("no_imports still rewrote the import block:\n%s", got)
		}
	})

	t.Run("unparseable files are skipped, not fatal", func(t *testing.T) {
		hb := newHarness(t, "broken")
		out := hb.call("go_format", map[string]any{"check_only": true})
		if !strings.Contains(out, "does not parse") {
			t.Errorf("broken file should be reported as skipped:\n%s", out)
		}
	})
}

func TestTidy(t *testing.T) {
	t.Run("clean module reports agreement", func(t *testing.T) {
		h := newHarness(t, "proj")
		out := h.call("go_tidy", map[string]any{})
		if !strings.Contains(out, "agrees with the code") {
			t.Errorf("clean module not reported clean:\n%s", out)
		}
	})

	t.Run("names a requirement nothing imports", func(t *testing.T) {
		h := newHarness(t, "proj")
		// A refactor that removed the last use leaves exactly this behind.
		gomod := filepath.Join(h.dir, "go.mod")
		b, err := os.ReadFile(gomod)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gomod, append(b,
			[]byte("\nrequire example.com/ghost v1.2.3\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		var res struct {
			UnusedRequires []string `json:"unused_requires"`
			Clean          bool     `json:"clean"`
		}
		h.structured("go_tidy", map[string]any{}, &res)
		if res.Clean {
			t.Error("module with an unused requirement reported clean")
		}
		if !slices.Contains(res.UnusedRequires, "example.com/ghost") {
			t.Errorf("unused_requires = %v, want the ghost requirement", res.UnusedRequires)
		}
	})
}

func TestUntested(t *testing.T) {
	h := harnessWithFile(t, "proj", "demo/api.go", `package demo

// Covered is reached from a test.
func Covered() int { return helper() }

func helper() int { return 1 }

// Orphan is exported and nothing tests it.
func Orphan() int { return 2 }

// Deep is reached only through Covered, so it counts as tested.
func Deep() int { return 3 }
`)
	if err := os.WriteFile(filepath.Join(h.dir, "demo", "api_test.go"), []byte(`package demo

import "testing"

func TestCovered(t *testing.T) {
	if Covered() != 1 {
		t.Fatal("bad")
	}
	_ = Deep()
}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	var res struct {
		Untested []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"untested"`
		Exported int    `json:"exported_total"`
		Reached  int    `json:"reached_by_tests"`
		Note     string `json:"note"`
	}
	h.structured("go_untested", map[string]any{}, &res)

	var names []string
	for _, u := range res.Untested {
		names = append(names, u.Name)
	}
	if !slices.Contains(names, "Orphan") {
		t.Errorf("untested = %v, want it to include Orphan", names)
	}
	for _, covered := range []string{"Covered", "Deep"} {
		if slices.Contains(names, covered) {
			t.Errorf("%s is reached from a test but reported untested: %v", covered, names)
		}
	}
	if res.Reached == 0 {
		t.Error("nothing reported as reached by tests")
	}
	if !strings.Contains(res.Note, "static") {
		t.Errorf("note should state the static-graph limit: %s", res.Note)
	}
}

// A process that cannot reach the Go build cache keeps answering syntactic
// questions and fails every semantic one. Reporting that as "selector matched
// nothing" sends the reader to debug a selector that was correct.
func TestBrokenToolchainIsDiagnosedNotMisattributed(t *testing.T) {
	dir := copyTree(t, filepath.Join("..", "..", "testdata", "proj"))
	t.Setenv("GOCACHE", "off")

	srv, err := tools.New(tools.Config{DefaultRoot: dir})
	if err != nil {
		t.Fatal(err)
	}
	m := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	srv.Register(m)
	clientT, serverT := mcp.NewInMemoryTransports()
	if _, err := m.Connect(t.Context(), serverT, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "v0"}, nil).Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	h := &harness{t: t, dir: dir, session: session}

	t.Run("syntactic tools keep working", func(t *testing.T) {
		out := h.call("go_read", map[string]any{"selector": "demo/demo.go"})
		if !strings.Contains(out, "func Load()") {
			t.Errorf("syntactic read broke along with the toolchain:\n%s", out)
		}
	})

	t.Run("semantic failure names the environment", func(t *testing.T) {
		msg := h.callExpectError("go_refs", map[string]any{"symbol": "demo.Config"})
		if strings.Contains(msg, "matched nothing") {
			t.Errorf("blamed the selector for an environment fault: %s", msg)
		}
		for _, want := range []string{"GOCACHE", "Syntactic tools"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error should mention %q, got: %s", want, msg)
			}
		}
	})

	t.Run("go_workspace reports it up front", func(t *testing.T) {
		out := h.call("go_workspace", map[string]any{})
		if !strings.Contains(out, "SEMANTIC TOOLS UNAVAILABLE") {
			t.Errorf("workspace should flag the broken toolchain:\n%s", out)
		}
	})
}

// Eight concurrent semantic queries used to kill the process: the shared
// snapshot's source cache was an unsynchronised map, and the runtime throws on
// a concurrent map write, which no recover can catch. The client saw only the
// transport close.
func TestConcurrentSemanticCallsDoNotCrash(t *testing.T) {
	h := newHarness(t, "proj")

	symbols := []string{
		"demo.Config", "demo.Load", "demo.Server", "demo.Server.Addr",
		"other.Config", "other.Helper", "demo.Config", "demo.Load",
	}
	type result struct {
		Total int `json:"total"`
	}

	var wg sync.WaitGroup
	errs := make([]error, len(symbols))
	totals := make([]int, len(symbols))
	for i, sym := range symbols {
		wg.Go(func() {
			res, err := h.session.CallTool(t.Context(), &mcp.CallToolParams{
				Name: "go_refs", Arguments: map[string]any{"symbol": sym},
			})
			if err != nil {
				errs[i] = err
				return
			}
			if res.IsError {
				errs[i] = fmt.Errorf("%s", contentText(res))
				return
			}
			var r result
			b, _ := json.Marshal(res.StructuredContent)
			json.Unmarshal(b, &r)
			totals[i] = r.Total
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent go_refs(%s): %v", symbols[i], err)
		}
	}
	// Identical queries must agree, which they cannot if the shared cache is
	// being corrupted underneath them.
	if totals[0] != totals[6] || totals[1] != totals[7] {
		t.Errorf("identical concurrent queries disagreed: %v", totals)
	}

	// Mixed planes at once, since that is what an agent actually does.
	var wg2 sync.WaitGroup
	for range 8 {
		wg2.Go(func() {
			h.callRaw("go_refs", map[string]any{"symbol": "demo.Config"})
			h.callRaw("go_search_symbols", map[string]any{"pattern": "Config"})
			h.callRaw("go_read", map[string]any{"selector": "./..."})
			h.callRaw("go_check", map[string]any{})
		})
	}
	wg2.Wait()

	if _, err := h.session.ListTools(t.Context(), nil); err != nil {
		t.Fatalf("server did not survive concurrent load: %v", err)
	}
}
