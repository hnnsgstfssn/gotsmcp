package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/source"
)

// ReadInput selects Go source to read.
type ReadInput struct {
	Selector string `json:"selector" jsonschema:"What to read: a file (internal/x/y.go), a directory (./internal/x), a package pattern (./... or example.com/m/pkg/...), or empty for the whole project"`
	Mode     string `json:"mode,omitempty" jsonschema:"outline (default) lists declarations with bodies elided; full returns complete source"`
	Name     string `json:"name,omitempty" jsonschema:"Return only the declaration with this name, with its full body. Use after an outline to expand one function"`
	MaxFiles int    `json:"max_files,omitempty" jsonschema:"Cap on files returned (default 1000)"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"Cap on total output size in bytes (default 1048576). A project-wide mode=full read will hit this; narrow the selector or stay in outline mode"`
	Root     string `json:"root,omitempty" jsonschema:"Project directory to operate in. Defaults to the server's working directory, which is the project the client was launched in. Pass this to work on a different module; it resolves to the enclosing go.mod"`
}

// ReadOutput reports what was read.
//
// It is not returned as structured output. A client that prefers
// structuredContent over content would then show an agent "{files: 1, bytes:
// 703}" where it asked for source, which is what happened in practice. Tools
// whose product is text return text and fold their counts into it.
type ReadOutput struct {
	Files     int
	Packages  int
	Bytes     int
	Truncated bool
	Skipped   []string
}

func (s *Server) registerRead(m *mcp.Server) {
	mcp.AddTool(m, &mcp.Tool{
		Name: "go_read",
		Description: `Read Go source by file, directory, package, or package pattern.

Defaults to outline mode, which returns every declaration's signature and doc
comment with function bodies elided. That is usually what you want: it is a
fraction of the tokens of the full file and shows the whole API of a package at
once. Pass mode=full for complete source, or name=Foo to expand one declaration.

Parses with tree-sitter and does not type-check, so it works on code that does
not compile.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.read)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_tree",
		Description: `Show the tree-sitter syntax tree for a file or a range within it.

Use this before writing a go_query or go_rewrite pattern. Queries match on exact
grammar node names, so guessing produces a query that matches nothing and fails
silently. Dump the tree for code you want to match and read the node names off
it.

Output is an S-expression: (node_type [line:col-line:col] field: (child ...)).
Start with max_depth around 3 and narrow with start_line/end_line.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.tree)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_query",
		Description: `Run a tree-sitter query across Go files and return matching nodes.

This is structural search: it matches shapes in the syntax tree, so it never
hits an occurrence inside a string literal or comment the way grep does. Use it
to find call sites, declarations matching a pattern, or any construct you intend
to rewrite.

Example, every call to a function in package fmt:
  (call_expression
    function: (selector_expression
      operand: (identifier) @pkg
      field: (field_identifier) @fn)
    (#eq? @pkg "fmt"))

Supported predicates, all of which take a capture as the first argument:
  #eq? #not-eq?            exact text, or compare two captures
  #any-of? #not-any-of?    text is one of several literals
  #match? #not-match?      regular expression over the captured text
  #is-exported?            the identifier starts with an upper-case letter
  #has-parent?             parent node is one of the named node types
  #has-ancestor?           some ancestor is one of the named node types
  #any-eq? #any-match?     satisfied if any node of a quantified capture matches

So a regex search that cannot fire inside a string or comment is
  ((function_declaration name: (identifier) @n) (#match? @n "^Test.*Handler$"))
and TODO comments are
  ((comment) @c (#match? @c "TODO|FIXME"))

Set count_only=true first to check a query hits what you expect. To find every
use of a specific symbol rather than a shape, use go_refs instead: it resolves
types and will not confuse two identically named things.`,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.query)
}

// Sized against a real 826-file, 118-package, 11 MB repository. Outline mode
// there produces 3.1 MB for the whole project; per package it is 6 KB at the
// median, 58 KB at p90, and 578 KB at the worst (a generated protobuf package).
// A 1 MB budget therefore returns any single package whole while still stopping
// a project-wide mode=full read, which would be 11 MB.
const (
	defaultMaxFiles = 1000
	defaultMaxBytes = 1 << 20
)

func (s *Server) read(ctx context.Context, _ *mcp.CallToolRequest, in ReadInput) (*mcp.CallToolResult, any, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, nil, err
	}
	pkgs, err := ws.loader.Resolve(ctx, in.Selector)
	if err != nil {
		return nil, nil, err
	}
	limit := cmpOr(in.MaxFiles, defaultMaxFiles)
	byteLimit := cmpOr(in.MaxBytes, defaultMaxBytes)

	out := ReadOutput{Packages: len(pkgs)}
	var b strings.Builder
	for _, pkg := range pkgs {
		for _, path := range pkg.Files {
			if out.Files >= limit || b.Len() >= byteLimit {
				out.Truncated = true
				break
			}
			f, err := ws.loader.File(path)
			if err != nil {
				out.Skipped = append(out.Skipped, err.Error())
				continue
			}
			out.Files++

			fmt.Fprintf(&b, "===== %s", ws.loader.Rel(path))
			if pkg.ImportPath != "" {
				fmt.Fprintf(&b, "  (%s)", pkg.ImportPath)
			}
			b.WriteString("\n")
			if errs := source.SyntaxErrors(f, ws.loader.Lang(), 5); len(errs) > 0 {
				fmt.Fprintf(&b, "; %d syntax error(s), tree is partial: %s\n", len(errs), strings.Join(errs, "; "))
			}
			b.WriteString(ws.renderFile(f, in))
			b.WriteString("\n")
		}
	}
	if len(pkgs) > 0 && len(pkgs[0].Ignored) > 0 {
		fmt.Fprintf(&b, "; note: %d file(s) excluded by build constraints were not read\n", len(pkgs[0].Ignored))
	}
	if b.Len() == 0 {
		b.WriteString("; no Go files matched\n")
	}
	out.Bytes = b.Len()

	var head strings.Builder
	fmt.Fprintf(&head, "; %d file(s) in %d package(s), %d bytes\n", out.Files, out.Packages, out.Bytes)
	for _, sk := range out.Skipped {
		fmt.Fprintf(&head, "; skipped: %s\n", sk)
	}
	if out.Truncated {
		fmt.Fprintf(&head, "; TRUNCATED after %d file(s) / %d bytes; narrow the selector or use outline mode\n",
			out.Files, out.Bytes)
	}
	head.WriteByte('\n')
	return text("%s%s", head.String(), b.String()), nil, nil
}

func (ws *workspace) renderFile(f *source.File, in ReadInput) string {
	if in.Name != "" {
		for _, d := range source.Outline(f, ws.loader.Lang()) {
			if d.Name != in.Name {
				continue
			}
			body := source.Lines(f.Src, d.Line, d.EndLine)
			if d.Doc != "" {
				return d.Doc + "\n" + body + "\n"
			}
			return body + "\n"
		}
		return fmt.Sprintf("; no declaration named %q in this file\n", in.Name)
	}
	if strings.EqualFold(in.Mode, "full") {
		return string(f.Src)
	}
	return source.Render(source.Outline(f, ws.loader.Lang()))
}

// TreeInput selects a file and an optional region.
type TreeInput struct {
	File      string `json:"file" jsonschema:"Path to one Go file, relative to the project root"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"First line of the region to show (1-based). Omit for the whole file"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"Last line of the region to show (1-based)"`
	MaxDepth  int    `json:"max_depth,omitempty" jsonschema:"Nesting levels to show; 0 is unlimited. Start around 3"`
	Anonymous bool   `json:"anonymous,omitempty" jsonschema:"Include unnamed tokens like func and {. Roughly triples the output"`
	Text      bool   `json:"text,omitempty" jsonschema:"Append the source text of leaf nodes"`
	MaxNodes  int    `json:"max_nodes,omitempty" jsonschema:"Cap on nodes rendered (default 6000)"`
	Root      string `json:"root,omitempty" jsonschema:"Project directory to operate in. Defaults to the server's working directory, which is the project the client was launched in. Pass this to work on a different module; it resolves to the enclosing go.mod"`
}

func (s *Server) tree(_ context.Context, _ *mcp.CallToolRequest, in TreeInput) (*mcp.CallToolResult, any, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, nil, err
	}
	f, err := ws.loader.File(in.File)
	if err != nil {
		return nil, nil, err
	}
	node := f.Tree.RootNode()
	if in.StartLine > 0 {
		end := cmpOr(in.EndLine, in.StartLine)
		if node, err = source.NodeForLines(f, in.StartLine, end); err != nil {
			return nil, nil, err
		}
	}

	rendered := source.RenderTree(f, ws.loader.Lang(), node, source.TreeOptions{
		MaxDepth:  in.MaxDepth,
		Anonymous: in.Anonymous,
		Text:      in.Text,
		MaxNodes:  in.MaxNodes,
	})
	var head strings.Builder
	fmt.Fprintf(&head, "; %s, root %s\n", ws.loader.Rel(f.Path), node.Type(ws.loader.Lang()))
	for _, e := range source.SyntaxErrors(f, ws.loader.Lang(), 10) {
		fmt.Fprintf(&head, "; syntax error: %s\n", e)
	}
	head.WriteByte('\n')
	return text("%s%s", head.String(), rendered), nil, nil
}

// QueryInput is a tree-sitter query over a selector.
type QueryInput struct {
	Query      string `json:"query" jsonschema:"Tree-sitter S-expression query. Captures are @name. Predicates #eq? #not-eq? #match? are supported"`
	Selector   string `json:"selector,omitempty" jsonschema:"Where to search; same forms as go_read. Defaults to the whole project"`
	CountOnly  bool   `json:"count_only,omitempty" jsonschema:"Return only the match count. Use this to validate a query before rewriting with it"`
	MaxMatches int    `json:"max_matches,omitempty" jsonschema:"Cap on matches returned (default 1000)"`
	MaxText    int    `json:"max_text,omitempty" jsonschema:"Cap on captured text length per capture (default 300)"`
	Root       string `json:"root,omitempty" jsonschema:"Project directory to operate in. Defaults to the server's working directory, which is the project the client was launched in. Pass this to work on a different module; it resolves to the enclosing go.mod"`
}

func (s *Server) query(ctx context.Context, _ *mcp.CallToolRequest, in QueryInput) (*mcp.CallToolResult, *source.QueryResult, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, nil, err
	}
	pkgs, err := ws.loader.Resolve(ctx, in.Selector)
	if err != nil {
		return nil, nil, err
	}
	res, err := ws.loader.Query(ctx, pkgs, in.Query, source.QueryOptions{
		MaxMatches:   in.MaxMatches,
		CountOnly:    in.CountOnly,
		MaxTextBytes: in.MaxText,
	})
	if err != nil {
		return nil, nil, err
	}
	return nil, res, nil
}
