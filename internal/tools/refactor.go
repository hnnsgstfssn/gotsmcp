package tools

import (
	"bytes"
	"context"
	"fmt"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/mod/modfile"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/rewrite"
	"github.com/hnnsgstfssn/treesitter-mcp/internal/sem"
	"github.com/hnnsgstfssn/treesitter-mcp/internal/source"
)

// WorkspaceInput asks about a project directory.
type WorkspaceInput struct {
	Root string `json:"root,omitempty" jsonschema:"Directory to describe. Defaults to the server's working directory"`
}

// WorkspaceOutput describes the resolved project.
type WorkspaceOutput struct {
	Root        string        `json:"root"`
	Module      string        `json:"module,omitempty"`
	GoVersion   string        `json:"go_version,omitempty"`
	Packages    int           `json:"packages"`
	Files       int           `json:"files"`
	DefaultRoot string        `json:"default_root"`
	Allowed     []string      `json:"allowed_roots,omitempty"`
	Toolchain   sem.Toolchain `json:"toolchain"`
	Note        string        `json:"note,omitempty"`
}

// DepsInput selects a package and a direction.
type DepsInput struct {
	Package   string `json:"package,omitempty" jsonschema:"Import path, a path/... prefix, or a bare package name. Empty describes every package in scope"`
	Selector  string `json:"selector,omitempty" jsonschema:"Scope to scan for the reverse relation. Defaults to ./..., which is what makes imported_by complete"`
	Root      string `json:"root,omitempty" jsonschema:"Project directory. Defaults to the server's working directory"`
	MaxResult int    `json:"max_packages,omitempty" jsonschema:"Cap on packages described (default 100)"`
}

func (s *Server) registerRefactor(m *mcp.Server) {
	mcp.AddTool(m, &mcp.Tool{
		Name: "go_workspace",
		Description: `Describe the project this server will operate on.

Call this first when you are unsure what root the other tools will default to.
The server is not bound to one module: every tool takes an optional root, and
when omitted it uses the directory the server process was started in, which for
a client launched inside a project is that project.

Reports the resolved module root, module path, Go version, and package and file
counts, plus any configured restriction on which directories may be touched.

It also reports whether the semantic tools can run at all. They shell out to the
Go toolchain, so a process launched without access to the build cache keeps
answering syntactic questions while every semantic one fails; this says so
directly rather than leaving it to be inferred.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.workspaceInfo)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_deps",
		Description: `Report what a package imports and what imports it.

The reverse direction is the point. Before changing a package's API you want to
know who depends on it, and that relation is written down nowhere in the package
itself. imported_by is computed over the whole selector, so leave the selector
at its default if you want it to be complete.

Standard-library imports are reported separately so they do not drown the
imports that matter for a refactor.

Loads names and imports only, not types, so it is much cheaper than the other
semantic tools and tolerates a module that does not fully compile.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.deps)
}

func (s *Server) workspaceInfo(ctx context.Context, _ *mcp.CallToolRequest, in WorkspaceInput) (*mcp.CallToolResult, WorkspaceOutput, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, WorkspaceOutput{}, err
	}
	out := WorkspaceOutput{
		Root:        ws.root,
		DefaultRoot: s.defaultRoot,
		Allowed:     s.allowed,
	}
	if len(s.allowed) == 0 {
		out.Note = "unrestricted: any directory may be used as a root"
	}

	if data, err := os.ReadFile(filepath.Join(ws.root, "go.mod")); err == nil {
		out.Module = modfile.ModulePath(data)
		if f, err := modfile.Parse("go.mod", data, nil); err == nil && f.Go != nil {
			out.GoVersion = f.Go.Version
		}
	} else {
		out.Note = strings.TrimSpace(out.Note + " no go.mod found; treating the directory itself as the root")
	}

	pkgs, err := ws.loader.Resolve(ctx, "./...")
	if err == nil {
		out.Packages = len(pkgs)
		out.Files = len(source.Files(pkgs))
	}
	out.Toolchain = sem.InspectToolchain(ctx, ws.root)

	var b strings.Builder
	fmt.Fprintf(&b, "root:    %s\n", out.Root)
	if out.Module != "" {
		fmt.Fprintf(&b, "module:  %s (go %s)\n", out.Module, out.GoVersion)
	}
	fmt.Fprintf(&b, "content: %d packages, %d Go files\n", out.Packages, out.Files)
	if out.Toolchain.CanTypeChk {
		fmt.Fprintf(&b, "go:      %s, semantic tools available\n", out.Toolchain.Version)
	} else {
		fmt.Fprintf(&b, "go:      SEMANTIC TOOLS UNAVAILABLE: %s\n         %s\n",
			out.Toolchain.Cause, out.Toolchain.Remedy)
	}
	fmt.Fprintf(&b, "default: %s\n", out.DefaultRoot)
	if len(out.Allowed) > 0 {
		fmt.Fprintf(&b, "allowed: %s\n", strings.Join(out.Allowed, ", "))
	}
	if out.Note != "" {
		fmt.Fprintf(&b, "note:    %s\n", out.Note)
	}
	return text("%s", b.String()), out, nil
}

func (s *Server) deps(ctx context.Context, _ *mcp.CallToolRequest, in DepsInput) (*mcp.CallToolResult, *sem.DepsResult, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, nil, err
	}
	res, err := sem.Deps(ctx, ws.root, in.Package, in.Selector)
	if err != nil {
		return nil, nil, err
	}
	if limit := cmpOr(in.MaxResult, 100); len(res.Packages) > limit {
		res.Packages = res.Packages[:limit]
	}
	return nil, res, nil
}

func moveSummary(mv *sem.Move, req sem.MoveRequest) string {
	what := mv.Symbol.Kind + " " + mv.Symbol.Name
	if n := len(req.Symbols) + len(req.Files); n > 1 || len(req.Files) > 0 {
		what = fmt.Sprintf("%d selection(s)", n)
	}
	return fmt.Sprintf("move %s to %s (%d reference(s) requalified)", what, mv.To, mv.Requalified)
}

// MoveInput relocates one or more declarations.
type MoveInput struct {
	Symbol  string   `json:"symbol,omitempty" jsonschema:"A single declaration to move: a position (internal/x/y.go:42:6) or a dotted path (pkg.Name)"`
	Symbols []string `json:"symbols,omitempty" jsonschema:"Several declarations to move together. Moving a cluster as one unit is what makes a package split work: a declaration may use another's unexported symbols as long as both are in the set"`
	Files   []string `json:"files,omitempty" jsonschema:"Whole files whose every declaration moves. The natural unit for splitting a package"`
	To      string   `json:"to,omitempty" jsonschema:"Destination. A path ending in .go is one file; anything else is a directory, and each source file keeps its base name. Created if absent"`
	ToFile  string   `json:"to_file,omitempty" jsonschema:"Deprecated alias for to"`
	// IncludeDeps is the answer to the common refusal, so it is worth
	// surfacing rather than making the caller rerun with a longer symbol list.
	IncludeDeps bool   `json:"include_dependencies,omitempty" jsonschema:"Also move the unexported symbols the selection needs, instead of refusing because they would be left behind"`
	Apply       bool   `json:"apply,omitempty" jsonschema:"Write immediately instead of returning a preview"`
	Root        string `json:"root,omitempty" jsonschema:"Project directory. Defaults to the server's working directory"`
}

// ExtractInput turns statements into a function.
type ExtractInput struct {
	File     string `json:"file" jsonschema:"File containing the statements, relative to the project root"`
	FromLine int    `json:"from_line" jsonschema:"First line to extract (1-based, inclusive). Must start a statement"`
	ToLine   int    `json:"to_line" jsonschema:"Last line to extract (1-based, inclusive). Must end a statement"`
	Name     string `json:"name" jsonschema:"Name for the new function"`
	Apply    bool   `json:"apply,omitempty" jsonschema:"Write immediately instead of returning a preview"`
	Root     string `json:"root,omitempty" jsonschema:"Project directory. Defaults to the server's working directory"`
}

// SignatureInput reshapes a parameter list.
type SignatureInput struct {
	Symbol  string            `json:"symbol,omitempty" jsonschema:"One function or method: a position or a dotted path (pkg.Name, pkg.Type.Method)"`
	Symbols []string          `json:"symbols,omitempty" jsonschema:"Several functions to change identically. Use with a rest entry so one request covers functions of differing arity"`
	Params  []sem.ParamChange `json:"params" jsonschema:"The new parameter list in order. Each entry carries over an existing parameter by its zero-based index in from, sets from to -1 to introduce one (needing type and value), or sets rest to true to stand for every original parameter not named elsewhere"`
	Apply   bool              `json:"apply,omitempty" jsonschema:"Write immediately instead of returning a preview"`
	Force   bool              `json:"force,omitempty" jsonschema:"Apply even though conflicts were reported"`
	Root    string            `json:"root,omitempty" jsonschema:"Project directory. Defaults to the server's working directory"`
}

func (s *Server) registerMoves(m *mcp.Server) {
	mcp.AddTool(m, &mcp.Tool{
		Name: "go_move",
		Description: `Move declarations to another file or package, one at a time or in bulk.

For splitting a package, move the cluster as one unit rather than declaration by
declaration:

  files:   ["internal/store/cache.go"]   every declaration in these files
  symbols: ["pkg.A", "pkg.B"]            a named cluster
  to:      "internal/cache"              a directory keeps each file's base
                                         name; a .go path merges into one file

Validation is on the whole selection, so a declaration using another's
unexported symbol is fine when both are moving. Methods always follow their
receiver type, since a method cannot be declared on a type from another package.
Pass include_dependencies=true to pull in the unexported symbols the selection
needs; without it the refusal names them so you can decide.

Carries doc comments and recalculates imports.

Within a package it is a text move: references resolve to the package, not the
file, so nothing else changes.

Across packages every reference is requalified in whichever direction applies:

  in the old package      Foo   -> new.Foo
  in the new package      old.Foo -> Foo
  everywhere else         old.Foo -> new.Foo

and uses inside the declaration itself of things left behind become old.X. The
destination package and directory are created if they do not exist, with the
import path derived from the source package.

Refused, with the reason, when the result could not compile:

  - unexported symbols the selection needs that are staying behind, named so
    you can add them or pass include_dependencies
  - unexported symbols the selection takes away that stayers still use
  - a method whose receiver type is not also moving
  - an unexported symbol referenced from outside the destination
  - any move that would make two packages import each other

Run go_check afterwards.`,
	}, s.move)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_extract",
		Description: `Extract a range of statements into a new function.

Parameters are the variables the range reads but does not declare; results are
the ones it assigns that are still read afterwards. Both are computed from the
type checker, which is the only way to get them right: a textual guess misses a
variable captured by a closure and mistakes a shadowed name for the outer one.

The range must cover whole statements at the top level of one function body.
Ranges containing return, defer, a labelled branch, or a break or continue whose
loop is outside the range are refused, because their meaning changes once moved.

Run go_check afterwards.`,
	}, s.extract)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_signature",
		Description: `Change a function's parameter list and update every call site.

Give the new list as a permutation of the old. Each entry either carries over an
existing parameter by its zero-based index, or introduces one with from=-1 plus
a type and the value to pass at existing calls. Expressing it that way is what
makes the call-site rewrite mechanical: every argument's destination is known.

  reorder (b, a):        [{"from":1},{"from":0}]
  drop the second:       [{"from":0}]
  add a leading ctx:     [{"from":-1,"name":"ctx","type":"context.Context","value":"context.TODO()"},{"from":0}]
  rename the first:      [{"from":0,"name":"count"}]

Grouped parameters flatten, so "a, b string" becomes "a string, b string":
indices have to line up one-to-one with call arguments.

Refused for variadic functions, interface methods, and functions used as values
rather than called, because arguments cannot be repositioned reliably in those
cases. Also refused when two changing calls are nested, as in f(g(x)) with both
changing, since the rewrites would overlap; do those in separate operations,
innermost first. Conflicts are reported and block the apply unless force=true.`,
	}, s.signature)
}

func (s *Server) move(ctx context.Context, _ *mcp.CallToolRequest, in MoveInput) (*mcp.CallToolResult, ChangeOutput, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	snap, err := ws.snapshot(ctx, "./...")
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	req := sem.MoveRequest{
		Symbols:     in.Symbols,
		Files:       in.Files,
		To:          sem.CmpOrString(in.To, in.ToFile),
		IncludeDeps: in.IncludeDeps,
	}
	if in.Symbol != "" {
		req.Symbols = append(req.Symbols, in.Symbol)
	}
	if req.To == "" {
		return nil, ChangeOutput{}, fmt.Errorf("to is required: a destination file or directory")
	}
	if len(req.Symbols) == 0 && len(req.Files) == 0 {
		return nil, ChangeOutput{}, fmt.Errorf("pass symbol, symbols, or files")
	}
	mv, err := snap.MoveDecls(req)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	return ws.stage(stageRequest{
		summary:  moveSummary(mv, req),
		edits:    mv.Edits,
		sources:  snap.MoveSources(mv),
		warnings: mv.Warnings,
		apply:    in.Apply,
	})
}

func (s *Server) extract(ctx context.Context, _ *mcp.CallToolRequest, in ExtractInput) (*mcp.CallToolResult, ChangeOutput, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	snap, err := ws.snapshot(ctx, "./...")
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	ex, err := snap.ExtractFunc(in.File, in.FromLine, in.ToLine, in.Name)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	return ws.stage(stageRequest{
		summary:  fmt.Sprintf("extract lines %d-%d of %s into %s", ex.FromLine, ex.ToLine, ex.File, ex.Signature),
		edits:    ex.Edits,
		sources:  snap.ExtractSources(ex),
		warnings: ex.Warnings,
		apply:    in.Apply,
	})
}

func (s *Server) signature(ctx context.Context, _ *mcp.CallToolRequest, in SignatureInput) (*mcp.CallToolResult, ChangeOutput, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	snap, err := ws.snapshot(ctx, "./...")
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	specs := in.Symbols
	if in.Symbol != "" {
		specs = append(specs, in.Symbol)
	}
	if len(specs) == 0 {
		return nil, ChangeOutput{}, fmt.Errorf("pass symbol or symbols")
	}
	var objs []types.Object
	for _, spec := range specs {
		obj, err := snap.Resolve(spec)
		if err != nil {
			return nil, ChangeOutput{}, err
		}
		objs = append(objs, obj)
	}
	sig, err := snap.ChangeSignatures(objs, in.Params)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	blocked := len(sig.Conflicts) > 0 && !in.Force
	return ws.stage(stageRequest{
		summary: fmt.Sprintf("change %s to %s across %d call site(s)",
			sig.Symbol.Name, sig.New, sig.CallSites),
		edits:     sig.Edits,
		sources:   snap.SignatureSources(sig),
		conflicts: sig.Conflicts,
		warnings:  sig.Warnings,
		apply:     in.Apply && !blocked,
		blocked:   blocked,
	})
}

// SeamInput proposes a split to analyse.
type SeamInput struct {
	Symbols     []string `json:"symbols,omitempty" jsonschema:"Declarations that would move"`
	Files       []string `json:"files,omitempty" jsonschema:"Whole files that would move. The usual way to propose a package split"`
	IncludeDeps bool     `json:"include_dependencies,omitempty" jsonschema:"Analyse as if the unexported symbols the selection needs came with it"`
	Root        string   `json:"root,omitempty" jsonschema:"Project directory. Defaults to the server's working directory"`
}

// TestsForInput names a symbol to trace back to tests.
type TestsForInput struct {
	Symbol   string `json:"symbol" jsonschema:"Function or method: a position or a dotted path (pkg.Name, pkg.Type.Method)"`
	MaxDepth int    `json:"max_depth,omitempty" jsonschema:"How many call levels to walk up (default 6)"`
	Selector string `json:"selector,omitempty" jsonschema:"Packages to load. Defaults to ./..., which is what makes the answer complete"`
	Root     string `json:"root,omitempty" jsonschema:"Project directory. Defaults to the server's working directory"`
}

// ImplementInput asks for the methods a type is missing.
type ImplementInput struct {
	Type      string `json:"type" jsonschema:"The concrete type to add methods to: a position or a dotted path"`
	Interface string `json:"interface" jsonschema:"The interface to satisfy: a position or a dotted path, including one from a dependency such as io.Reader"`
	File      string `json:"file,omitempty" jsonschema:"Where to write the methods. Defaults to the file declaring the type"`
	Apply     bool   `json:"apply,omitempty" jsonschema:"Write immediately instead of returning a preview"`
	Root      string `json:"root,omitempty" jsonschema:"Project directory. Defaults to the server's working directory"`
}

func (s *Server) registerAnalysis(m *mcp.Server) {
	mcp.AddTool(m, &mcp.Tool{
		Name: "go_seam",
		Description: `Report what couples a proposed set of declarations to the rest of its
package, without moving anything.

Ask this before a package split. It answers "what actually couples these?" in
one call, on code that does not have to survive the move, and tells you whether
the split is clean, needs a few more symbols, or is blocked:

  missing_deps       unexported symbols the selection uses and would leave
  stranded           unexported symbols it takes away that stayers still need
  needs_from_source  exported symbols it keeps using, an import back
  inbound            who references the selection, and with which symbols
  cycle              whether the two packages would import each other

The verdict is one of clean, clean_with_import, needs_dependencies, or blocked,
each with the specific next step. Nothing is written.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.seam)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_tests_for",
		Description: `Find the test functions whose calls reach a symbol.

Use it before changing something risky, to know what to run. Each hit includes
the call chain from the test down to the symbol, so a long list stays readable.

The graph is static: an edge exists only where the caller names the callee
directly. Calls through an interface, a function value, or reflection are not
followed, so this under-reports. Treat it as a starting point, not as coverage.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.testsFor)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_implement",
		Description: `Generate the methods a type needs to satisfy an interface.

go_implements reports that a type is three methods short; this writes them, with
the right parameter and result types, qualified for the target package, and a
receiver matching whichever form the type's existing methods already use.

Bodies panic rather than returning zero values: a silently-zero implementation
compiles and is wrong, which is the failure this server exists to prevent.

Previews by default.`,
	}, s.implement)
}

func (s *Server) seam(ctx context.Context, _ *mcp.CallToolRequest, in SeamInput) (*mcp.CallToolResult, *sem.SeamReport, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, nil, err
	}
	snap, err := ws.snapshot(ctx, "./...")
	if err != nil {
		return nil, nil, err
	}
	if len(in.Symbols) == 0 && len(in.Files) == 0 {
		return nil, nil, fmt.Errorf("pass symbols or files describing what would move")
	}
	rep, err := snap.Seam(sem.MoveRequest{
		Symbols: in.Symbols, Files: in.Files, IncludeDeps: in.IncludeDeps,
	})
	if err != nil {
		return nil, nil, err
	}
	return text("%s", rep.Render()), rep, nil
}

func (s *Server) testsFor(ctx context.Context, _ *mcp.CallToolRequest, in TestsForInput) (*mcp.CallToolResult, *sem.TestsResult, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, nil, err
	}
	snap, err := ws.snapshot(ctx, in.Selector)
	if err != nil {
		return nil, nil, err
	}
	obj, err := snap.Resolve(in.Symbol)
	if err != nil {
		return nil, nil, err
	}
	return nil, snap.TestsFor(obj, in.MaxDepth), nil
}

func (s *Server) implement(ctx context.Context, _ *mcp.CallToolRequest, in ImplementInput) (*mcp.CallToolResult, ChangeOutput, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	snap, err := ws.snapshot(ctx, "./...")
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	typeObj, err := snap.Resolve(in.Type)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	ifaceObj, err := snap.Resolve(in.Interface)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	stub, err := snap.ImplementInterface(typeObj, ifaceObj, in.File)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	return ws.stage(stageRequest{
		summary: fmt.Sprintf("add %d method(s) to %s for %s: %s",
			len(stub.Missing), stub.Type, stub.Interface, strings.Join(stub.Missing, ", ")),
		edits:    stub.Edits,
		sources:  snap.StubSources(stub),
		warnings: stub.Warnings,
		apply:    in.Apply,
	})
}

// FormatInput selects what to format.
type FormatInput struct {
	Selector   string `json:"selector,omitempty" jsonschema:"What to format: a file, a directory, or a package pattern. Defaults to the whole project"`
	CheckOnly  bool   `json:"check_only,omitempty" jsonschema:"List the files that are not formatted and write nothing. The cheap way to ask whether the tree is clean"`
	FixImports bool   `json:"fix_imports,omitempty" jsonschema:"Also add missing and drop unused imports, which is goimports rather than gofmt. Defaults to true"`
	NoImports  bool   `json:"no_imports,omitempty" jsonschema:"Plain gofmt: format without touching the import block"`
	Apply      bool   `json:"apply,omitempty" jsonschema:"Write immediately instead of returning a preview"`
	MaxFiles   int    `json:"max_files,omitempty" jsonschema:"Cap on files considered (default 2000)"`
	Root       string `json:"root,omitempty" jsonschema:"Project directory. Defaults to the server's working directory"`
}

// FormatOutput reports what is unformatted.
type FormatOutput struct {
	Unformatted []string `json:"unformatted"`
	Checked     int      `json:"checked"`
	Skipped     []string `json:"skipped,omitempty"`
	Clean       bool     `json:"clean"`
}

func (s *Server) registerFormat(m *mcp.Server) {
	mcp.AddTool(m, &mcp.Tool{
		Name: "go_format",
		Description: `Format Go files with goimports, or plain gofmt.

Edits made through this server are already formatted on apply. This is for
everything else: a file written with an ordinary editor tool, or a tree you want
to normalise before reading a diff.

check_only lists the files that are not formatted and writes nothing, which is
the cheap way to ask whether a tree is clean. Otherwise it previews like every
other mutating tool.

Defaults to goimports, so it adds imports the file references and drops ones it
no longer does. Pass no_imports for plain gofmt when the import block should be
left alone. Files that do not parse cannot be formatted and are reported as
skipped rather than failing the call.`,
	}, s.format)
}

func (s *Server) format(ctx context.Context, _ *mcp.CallToolRequest, in FormatInput) (*mcp.CallToolResult, ChangeOutput, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	pkgs, err := ws.loader.Resolve(ctx, in.Selector)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	fixImports := !in.NoImports

	var (
		edits    []rewrite.Edit
		sources  = map[string][]byte{}
		out      FormatOutput
		warnings []string
	)
	for _, path := range source.Files(pkgs) {
		if out.Checked >= cmpOr(in.MaxFiles, 2000) {
			warnings = append(warnings, "file cap reached; narrow the selector")
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, ChangeOutput{}, err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s: %v", ws.loader.Rel(path), err))
			continue
		}
		out.Checked++

		formatted, err := rewrite.Format(path, src, fixImports)
		if err != nil {
			// Unparseable source has no canonical form; that is a job for the
			// edit tools, not this one.
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s: does not parse", ws.loader.Rel(path)))
			continue
		}
		if bytes.Equal(src, formatted) {
			continue
		}
		out.Unformatted = append(out.Unformatted, ws.loader.Rel(path))
		edits = append(edits, rewrite.Edit{
			Path: path, Start: 0, End: uint32(len(src)),
			New: string(formatted), Note: "formatted",
		})
		sources[path] = src
	}
	out.Clean = len(out.Unformatted) == 0

	if in.CheckOnly || out.Clean {
		var b strings.Builder
		if out.Clean {
			fmt.Fprintf(&b, "%d file(s) checked, all formatted\n", out.Checked)
		} else {
			fmt.Fprintf(&b, "%d of %d file(s) are not formatted:\n", len(out.Unformatted), out.Checked)
			for _, f := range out.Unformatted {
				fmt.Fprintf(&b, "  %s\n", f)
			}
		}
		for _, sk := range out.Skipped {
			fmt.Fprintf(&b, "  skipped: %s\n", sk)
		}
		return text("%s", b.String()), ChangeOutput{
			Files: out.Unformatted, Sites: len(out.Unformatted),
			Warnings: append(warnings, out.Skipped...),
		}, nil
	}

	return ws.stage(stageRequest{
		summary:     fmt.Sprintf("format %d of %d file(s)", len(out.Unformatted), out.Checked),
		edits:       edits,
		sources:     sources,
		warnings:    append(warnings, out.Skipped...),
		apply:       in.Apply,
		plainFormat: in.NoImports,
	})
}

// TidyInput selects a module to check.
type TidyInput struct {
	Root string `json:"root,omitempty" jsonschema:"Project directory. Defaults to the server's working directory"`
}

// UntestedInput bounds a test-gap report.
type UntestedInput struct {
	Selector   string `json:"selector,omitempty" jsonschema:"Packages to analyse. Defaults to ./..., which is what makes the answer meaningful"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"Cap on declarations listed (default 200)"`
	Root       string `json:"root,omitempty" jsonschema:"Project directory. Defaults to the server's working directory"`
}

func (s *Server) registerHygiene(m *mcp.Server) {
	mcp.AddTool(m, &mcp.Tool{
		Name: "go_tidy",
		Description: `Report where go.mod disagrees with what the code imports.

Answers, offline and in about a second, whether running go mod tidy would change
anything and why:

  unused_requires   direct requirements nothing imports any more, which is
                    what a refactor that removed the last use leaves behind
  missing_requires  imports with no requirement

It does not run go mod tidy. That mutates go.mod and go.sum and reaches the
network, and any caller with a shell can run it; what is missing without this is
knowing whether it is worth running.

Indirect requirements are not reported: they exist for a dependency's benefit,
so the absence of a local import says nothing about them.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.tidy)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_untested",
		Description: `List exported functions and methods that no test reaches.

Walks forward from every Test, Benchmark, Fuzz, and Example function once and
reports the exported declarations left over. Use it to find where a test suite
has gaps, or to decide what to cover before a risky change.

The call graph is static, so a function only ever invoked through an interface
or a function value looks untested. This is a list to review, not a verdict, and
it is not a substitute for coverage.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.untested)
}

func (s *Server) tidy(ctx context.Context, _ *mcp.CallToolRequest, in TidyInput) (*mcp.CallToolResult, *sem.TidyResult, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, nil, err
	}
	res, err := sem.Tidy(ctx, ws.root)
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	if res.Clean {
		fmt.Fprintf(&b, "go.mod agrees with the code: %d direct requirement(s), %d import(s)\n",
			res.DirectRequires, res.ImportsSeen)
	} else {
		for _, u := range res.UnusedRequires {
			fmt.Fprintf(&b, "unused requirement  %s\n", u)
		}
		for _, mm := range res.MissingRequires {
			fmt.Fprintf(&b, "missing requirement %s\n", mm)
		}
		fmt.Fprintf(&b, "\n%d of %d direct requirement(s) unused\n", len(res.UnusedRequires), res.DirectRequires)
	}
	return text("%s", b.String()), res, nil
}

func (s *Server) untested(ctx context.Context, _ *mcp.CallToolRequest, in UntestedInput) (*mcp.CallToolResult, *sem.UntestedResult, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, nil, err
	}
	snap, err := ws.snapshot(ctx, in.Selector)
	if err != nil {
		return nil, nil, err
	}
	return nil, snap.UntestedAPI(in.MaxResults), nil
}
