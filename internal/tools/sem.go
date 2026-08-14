package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/sem"
)

// SymbolInput names a symbol and the scope to resolve it in.
type SymbolInput struct {
	Symbol   string `json:"symbol" jsonschema:"Either a position (internal/x/y.go:42:6) or a dotted path (pkg.Name, pkg.Type.Method, example.com/m/pkg.Name). Positions are unambiguous and are what go_query returns"`
	Selector string `json:"selector,omitempty" jsonschema:"Packages to type-check. Defaults to ./..., which is what codebase-wide correctness needs"`
	Root     string `json:"root,omitempty" jsonschema:"Project directory to operate in. Defaults to the server's working directory. Pass this to work on a different module; it resolves to the enclosing go.mod"`
}

// SymbolOutput describes a resolved symbol.
type SymbolOutput struct {
	Symbol     sem.Symbol `json:"symbol"`
	TypeErrors []string   `json:"type_errors,omitempty"`
}

// RefsOutput lists every occurrence of a symbol.
type RefsOutput struct {
	Symbol     sem.Symbol `json:"symbol"`
	References []sem.Ref  `json:"references"`
	Total      int        `json:"total"`
	TypeErrors []string   `json:"type_errors,omitempty"`
}

func (s *Server) registerSem(m *mcp.Server) {
	mcp.AddTool(m, &mcp.Tool{
		Name: "go_symbol",
		Description: `Resolve a symbol and report what it actually is: kind, full type, and
declaring position.

Use this to confirm you are pointing at the thing you think you are before
renaming it. Two identically spelled names in different packages, a type and a
struct field sharing a name, or a local shadowing a package-level declaration
are all distinguished here and cannot be distinguished by grep or go_query.

Type-checks the selector, so it needs code that compiles.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.symbol)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_refs",
		Description: `Find every reference to a symbol, resolved by type rather than by name.

This is the tool to use before changing anything: it reports exactly the
occurrences the compiler considers to be this symbol, across packages and
including test files, and excludes same-spelled things that are unrelated.

References through an embedded field are reported as kind embedded-field.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.refs)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_rename",
		Description: `Rename a symbol and every reference to it across the codebase.

Type-aware: it renames the declaration and its references, follows renames
through embedded fields, and covers test files. It will not touch a different
symbol that merely shares the name.

Reports conflicts rather than producing quietly broken code. A rename that would
collide with an existing declaration, be captured by a shadowing name, or break
an interface that some type currently satisfies is refused unless force=true.

Previews by default: nothing is written until you call go_apply with the
returned changeset_id, or re-run with apply=true.`,
	}, s.rename)
}

func (s *Server) symbol(ctx context.Context, _ *mcp.CallToolRequest, in SymbolInput) (*mcp.CallToolResult, SymbolOutput, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, SymbolOutput{}, err
	}
	snap, err := ws.snapshot(ctx, in.Selector)
	if err != nil {
		return nil, SymbolOutput{}, err
	}
	obj, err := snap.Resolve(in.Symbol)
	if err != nil {
		return nil, SymbolOutput{}, err
	}
	return nil, SymbolOutput{Symbol: snap.Describe(obj), TypeErrors: snap.TypeErrors}, nil
}

func (s *Server) refs(ctx context.Context, _ *mcp.CallToolRequest, in SymbolInput) (*mcp.CallToolResult, RefsOutput, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, RefsOutput{}, err
	}
	snap, err := ws.snapshot(ctx, in.Selector)
	if err != nil {
		return nil, RefsOutput{}, err
	}
	obj, err := snap.Resolve(in.Symbol)
	if err != nil {
		return nil, RefsOutput{}, err
	}
	refs := snap.References(obj)
	return nil, RefsOutput{
		Symbol:     snap.Describe(obj),
		References: refs,
		Total:      len(refs),
		TypeErrors: snap.TypeErrors,
	}, nil
}

// RenameInput is a rename request.
type RenameInput struct {
	Symbol   string `json:"symbol" jsonschema:"Symbol to rename: a position (internal/x/y.go:42:6) or a dotted path (pkg.Name, pkg.Type.Method)"`
	NewName  string `json:"new_name" jsonschema:"The new identifier"`
	Selector string `json:"selector,omitempty" jsonschema:"Packages to load. Defaults to ./.... Narrowing this makes the rename faster but can miss references, so only narrow when you are sure"`
	Apply    bool   `json:"apply,omitempty" jsonschema:"Write the changes immediately instead of returning a preview"`
	Force    bool   `json:"force,omitempty" jsonschema:"Apply even though conflicts were reported. The result will very likely not compile"`
	Root     string `json:"root,omitempty" jsonschema:"Project directory to operate in. Defaults to the server's working directory. Pass this to work on a different module; it resolves to the enclosing go.mod"`
}

func (s *Server) rename(ctx context.Context, _ *mcp.CallToolRequest, in RenameInput) (*mcp.CallToolResult, ChangeOutput, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	snap, err := ws.snapshot(ctx, in.Selector)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	obj, err := snap.Resolve(in.Symbol)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	r, err := snap.Rename(obj, in.NewName)
	if err != nil {
		return nil, ChangeOutput{}, err
	}

	summary := fmt.Sprintf("rename %s %s to %s (%d sites)",
		r.Symbol.Kind, r.Symbol.Name, in.NewName, r.Sites)

	blocked := len(r.Conflicts) > 0 && !in.Force
	return ws.stage(stageRequest{
		summary:   summary,
		edits:     r.Edits,
		sources:   snap.Sources(),
		conflicts: r.Conflicts,
		warnings:  append(r.Warnings, snap.TypeErrors...),
		apply:     in.Apply && !blocked,
		blocked:   blocked,
	})
}
