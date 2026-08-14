package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/sem"
	"github.com/hnnsgstfssn/treesitter-mcp/internal/source"
)

// SearchSymbolsInput looks up declarations by name.
type SearchSymbolsInput struct {
	Pattern       string   `json:"pattern" jsonschema:"Name to look for. Interpreted according to mode"`
	Mode          string   `json:"mode,omitempty" jsonschema:"substring (default, case-insensitive), exact, regex, or fuzzy. fuzzy matches a subsequence so cfgldr finds ConfigLoader"`
	Kinds         []string `json:"kinds,omitempty" jsonschema:"Restrict to these kinds: func, method, type, const, var, field. Empty means all"`
	ExportedOnly  bool     `json:"exported_only,omitempty" jsonschema:"Only declarations starting with an upper-case letter"`
	IncludeFields bool     `json:"include_fields,omitempty" jsonschema:"Also search struct field and interface method names. Off by default because it roughly triples results"`
	Selector      string   `json:"selector,omitempty" jsonschema:"Where to search; same forms as go_read. Defaults to the whole project"`
	MaxResults    int      `json:"max_results,omitempty" jsonschema:"Cap on results (default 200), best matches first"`
	Root          string   `json:"root,omitempty" jsonschema:"Project directory to operate in. Defaults to the server's working directory. Pass this to work on a different module; it resolves to the enclosing go.mod"`
}

// SearchTextInput runs a regex confined to chosen syntax.
type SearchTextInput struct {
	Pattern  string `json:"pattern" jsonschema:"Go regular expression"`
	In       string `json:"in,omitempty" jsonschema:"Where to look: comments (default), strings, identifiers, or any. Restricting to syntax is the point: a comment search will not hit the same words in code"`
	Selector string `json:"selector,omitempty" jsonschema:"Where to search; same forms as go_read"`
	MaxHits  int    `json:"max_hits,omitempty" jsonschema:"Cap on matches returned (default 1000)"`
	Root     string `json:"root,omitempty" jsonschema:"Project directory to operate in. Defaults to the server's working directory. Pass this to work on a different module; it resolves to the enclosing go.mod"`
}

func (s *Server) registerSearch(m *mcp.Server) {
	mcp.AddTool(m, &mcp.Tool{
		Name: "go_search_symbols",
		Description: `Find declarations by name anywhere in the project.

Answers "where is Foo defined?" without knowing its package, which is usually
the first thing you need. Returns kind, receiver, package, position, and a
one-line signature, ranked best match first.

Modes: substring (default), exact, regex, and fuzzy. Fuzzy matches a
subsequence and scores word boundaries, so "cfgldr" finds ConfigLoader.

Parses with tree-sitter and does not type-check, so it is fast and works on code
that does not compile. It matches on spelling, so two unrelated types both named
Config both appear; feed a returned position to go_symbol or go_refs when you
need to tell them apart.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.searchSymbols)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_search_text",
		Description: `Regex search confined to one kind of syntax.

The safe replacement for grep. Restricting the search to comments, string
literals, or identifiers means a search for "password" in strings will not also
hit a variable named password or the word in a comment, and a TODO sweep will
not match the letters inside an identifier.

  in=comments     TODO, FIXME, deprecation notes, doc text
  in=strings      literals: SQL, URLs, error text, format strings
  in=identifiers  names, when you want spelling rather than declarations
  in=any          all three

For declarations specifically use go_search_symbols; for structural shapes use
go_query.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.searchText)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_implements",
		Description: `Report the implements relation for a type, in whichever direction applies.

Given an interface, lists the concrete types satisfying it. Given a concrete
type, lists the interfaces it satisfies, including ones from dependencies and
the standard library such as error and io.Reader.

Nothing in a Go type's source text says which interfaces it implements, because
satisfaction is structural. So this cannot be answered by grep or by go_query,
and it is what you want before changing a method set: go_rename will refuse a
method rename that breaks satisfaction, and this shows you what would break.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.implements)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_callers",
		Description: `Find call sites of a function or method, naming the enclosing declaration.

go_refs reports every reference, including the declaration itself and any use as
a value. This reports only calls and says which function each one is inside,
which is the form an impact review needs.

Resolved by type, so it will not confuse two identically named methods on
different types.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.callers)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_check",
		Description: `Type-check the project and report compiler errors.

Run this after applying edits. The edit tools guarantee the result parses, which
catches a mangled template, but not code that is syntactically fine and
semantically wrong: a call with the wrong argument count, a method rename that
broke an interface, a type mismatch introduced by a rewrite.

This is the verification step. A rewrite that previews cleanly and applies
cleanly can still break the build, and this is how you find out without shelling
out to go build.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.check)
}

func (s *Server) searchSymbols(ctx context.Context, _ *mcp.CallToolRequest, in SearchSymbolsInput) (*mcp.CallToolResult, *source.SearchResult, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, nil, err
	}
	pkgs, err := ws.loader.Resolve(ctx, in.Selector)
	if err != nil {
		return nil, nil, err
	}
	res, err := ws.loader.Search(ctx, pkgs, in.Pattern, source.SearchOptions{
		Mode:          source.MatchMode(in.Mode),
		Kinds:         in.Kinds,
		ExportedOnly:  in.ExportedOnly,
		IncludeFields: in.IncludeFields,
		MaxResults:    in.MaxResults,
	})
	if err != nil {
		return nil, nil, err
	}
	return nil, res, nil
}

// nodeKindsFor maps the friendly "in" values onto grammar node types.
var nodeKindsFor = map[string][]string{
	"comments":    {"comment"},
	"strings":     {"interpreted_string_literal", "raw_string_literal"},
	"identifiers": {"identifier", "type_identifier", "field_identifier", "package_identifier"},
	"any":         {"comment", "interpreted_string_literal", "raw_string_literal", "identifier", "type_identifier", "field_identifier", "package_identifier"},
}

func (s *Server) searchText(ctx context.Context, _ *mcp.CallToolRequest, in SearchTextInput) (*mcp.CallToolResult, *source.QueryResult, error) {
	where := strings.ToLower(strings.TrimSpace(in.In))
	if where == "" {
		where = "comments"
	}
	kinds, ok := nodeKindsFor[where]
	if !ok {
		return nil, nil, fmt.Errorf("unknown 'in' value %q; use comments, strings, identifiers, or any", in.In)
	}
	if strings.TrimSpace(in.Pattern) == "" {
		return nil, nil, fmt.Errorf("pattern is empty")
	}

	// Build one alternation of node types, each guarded by the same regex.
	var b strings.Builder
	for _, k := range kinds {
		fmt.Fprintf(&b, "((%s) @hit (#match? @hit %q))\n", k, in.Pattern)
	}

	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, nil, err
	}
	pkgs, err := ws.loader.Resolve(ctx, in.Selector)
	if err != nil {
		return nil, nil, err
	}
	res, err := ws.loader.Query(ctx, pkgs, b.String(), source.QueryOptions{MaxMatches: in.MaxHits})
	if err != nil {
		return nil, nil, err
	}
	return nil, res, nil
}

// ImplementsOutput reports an implements relation.
type ImplementsOutput struct {
	Symbol      sem.Symbol           `json:"symbol"`
	IsInterface bool                 `json:"is_interface"`
	Results     []sem.Implementation `json:"results"`
	Total       int                  `json:"total"`
}

func (s *Server) implements(ctx context.Context, _ *mcp.CallToolRequest, in SymbolInput) (*mcp.CallToolResult, ImplementsOutput, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, ImplementsOutput{}, err
	}
	snap, err := ws.snapshot(ctx, in.Selector)
	if err != nil {
		return nil, ImplementsOutput{}, err
	}
	obj, err := snap.Resolve(in.Symbol)
	if err != nil {
		return nil, ImplementsOutput{}, err
	}
	isIface, results := snap.Implements(obj)
	sym := snap.Describe(obj)
	if sym.Kind != "type" {
		return nil, ImplementsOutput{}, fmt.Errorf("%s is a %s; go_implements needs a type or interface", sym.Name, sym.Kind)
	}
	return nil, ImplementsOutput{Symbol: sym, IsInterface: isIface, Results: results, Total: len(results)}, nil
}

// CallersOutput lists call sites.
type CallersOutput struct {
	Symbol sem.Symbol `json:"symbol"`
	Calls  []sem.Call `json:"calls"`
	Total  int        `json:"total"`
}

func (s *Server) callers(ctx context.Context, _ *mcp.CallToolRequest, in SymbolInput) (*mcp.CallToolResult, CallersOutput, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, CallersOutput{}, err
	}
	snap, err := ws.snapshot(ctx, in.Selector)
	if err != nil {
		return nil, CallersOutput{}, err
	}
	obj, err := snap.Resolve(in.Symbol)
	if err != nil {
		return nil, CallersOutput{}, err
	}
	sym := snap.Describe(obj)
	if sym.Kind != "func" && sym.Kind != "method" {
		return nil, CallersOutput{}, fmt.Errorf("%s is a %s; go_callers needs a func or method", sym.Name, sym.Kind)
	}
	calls := snap.Callers(obj)
	return nil, CallersOutput{Symbol: sym, Calls: calls, Total: len(calls)}, nil
}

// CheckInput selects what to type-check.
type CheckInput struct {
	Selector       string `json:"selector,omitempty" jsonschema:"Packages to check; same forms as go_read. Defaults to the whole project"`
	MaxDiagnostics int    `json:"max_diagnostics,omitempty" jsonschema:"Cap on errors reported (default 100)"`
	Root           string `json:"root,omitempty" jsonschema:"Project directory to operate in. Defaults to the server's working directory. Pass this to work on a different module; it resolves to the enclosing go.mod"`
}

func (s *Server) check(ctx context.Context, _ *mcp.CallToolRequest, in CheckInput) (*mcp.CallToolResult, *sem.CheckResult, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, nil, err
	}
	res, err := sem.Check(ctx, ws.loader.Root(), in.Selector, in.MaxDiagnostics)
	if err != nil {
		return nil, nil, err
	}
	res.Rel(ws.loader.Rel)

	var b strings.Builder
	if res.OK {
		fmt.Fprintf(&b, "OK: %d package(s) type-check cleanly\n", res.Packages)
	} else {
		fmt.Fprintf(&b, "%d error(s) across %d package(s)\n\n", len(res.Diagnostics), res.Packages)
		for _, d := range res.Diagnostics {
			if d.Line > 0 {
				fmt.Fprintf(&b, "  %s:%d:%d: %s\n", d.File, d.Line, d.Col, d.Message)
			} else {
				fmt.Fprintf(&b, "  %s: %s\n", d.File, d.Message)
			}
		}
		if res.Truncated {
			b.WriteString("\n  ... more errors not shown\n")
		}
	}
	return text("%s", b.String()), res, nil
}
