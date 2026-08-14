package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/rewrite"
	"github.com/hnnsgstfssn/treesitter-mcp/internal/sem"
	"github.com/hnnsgstfssn/treesitter-mcp/internal/source"
)

// ChangeOutput is the common shape of every mutating tool.
type ChangeOutput struct {
	ChangeSetID string               `json:"changeset_id,omitempty"`
	Applied     bool                 `json:"applied"`
	Blocked     bool                 `json:"blocked,omitempty"`
	Sites       int                  `json:"sites"`
	Files       []string             `json:"files"`
	Changes     []rewrite.FileChange `json:"changes,omitempty"`
	Conflicts   []sem.Conflict       `json:"conflicts,omitempty"`
	Warnings    []string             `json:"warnings,omitempty"`
	NextStep    string               `json:"next_step,omitempty"`
}

const (
	maxHunksPerFile = 40
	// A 2000-site rewrite would otherwise render a preview nobody can read.
	maxHunksTotal = 300
)

func (s *Server) registerEdit(m *mcp.Server) {
	mcp.AddTool(m, &mcp.Tool{
		Name: "go_rewrite",
		Description: `Rewrite every node matching a tree-sitter query, using a template.

This is the structural replacement for a sed loop. Matching happens on the
syntax tree, so it cannot fire inside a string or comment, byte ranges come from
the parser rather than a regex, and the result is gofmt'd and re-parsed before
anything is written. If an edit would leave the file unparseable, the whole
changeset is refused.

The template substitutes captures as ${name}; write $$ for a literal dollar.
Referencing a capture the query did not bind is an error, not an empty string.

  query:    (call_expression
              function: (selector_expression
                operand: (identifier) @pkg (#eq? @pkg "log")
                field: (field_identifier) @fn (#eq? @fn "Printf"))
              arguments: (argument_list) @args) @call
  target:   call
  template: slog.Info${args}

Workflow: go_tree to learn node names, go_query with count_only to check the
match set, then go_rewrite. Imports are fixed automatically, so a rewrite that
introduces or orphans a package leaves the import block correct.

For renaming a symbol, use go_rename instead. This tool matches shapes and
cannot tell two identically spelled symbols apart.`,
	}, s.rewriteQuery)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_edit",
		Description: `Replace explicit line/column ranges in Go files.

The escape hatch for when you already know exactly what to change, typically
from go_query output, and writing a matching query would be more work than
listing the ranges. Ranges are 1-based; end_col is exclusive.

Same guarantees as go_rewrite: overlapping edits are refused, the result is
formatted and re-parsed, and nothing is written unless it parses.`,
	}, s.editRanges)

	mcp.AddTool(m, &mcp.Tool{
		Name: "go_apply",
		Description: `Write a previewed changeset to disk.

Verifies that every file still has the contents it had when the preview was
made. If anything changed underneath, the apply is refused and you should re-run
the operation to get a fresh preview.`,
	}, s.apply)
}

// stageRequest carries a computed set of edits into the shared preview/apply path.
type stageRequest struct {
	summary string
	// plainFormat runs gofmt instead of goimports on apply, for a caller that
	// asked not to have its import block rewritten.
	plainFormat bool
	edits       []rewrite.Edit
	sources     map[string][]byte
	conflicts   []sem.Conflict
	warnings    []string
	apply       bool
	blocked     bool
}

// stage validates edits, renders a preview, and optionally writes.
func (ws *workspace) stage(req stageRequest) (*mcp.CallToolResult, ChangeOutput, error) {
	if len(req.edits) == 0 {
		return text("no matches; nothing to change"),
			ChangeOutput{Warnings: req.warnings, Conflicts: req.conflicts}, nil
	}

	cs, err := rewrite.New(req.summary, req.edits, req.sources)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	plan, err := rewrite.Compute(cs, ws.loader.Rel, rewrite.Options{
		FixImports: !req.plainFormat,
		MaxHunks:   maxHunksPerFile,
	})
	if err != nil {
		return nil, ChangeOutput{}, err
	}

	out := ChangeOutput{
		ChangeSetID: cs.ID,
		Sites:       len(req.edits),
		Changes:     plan.Changes,
		Conflicts:   req.conflicts,
		Warnings:    append(req.warnings, plan.Warnings...),
		Blocked:     req.blocked,
	}
	for _, c := range plan.Changes {
		out.Files = append(out.Files, c.Path)
	}

	switch {
	case req.blocked:
		out.NextStep = "conflicts reported; fix them, or re-run with force=true to apply anyway"
	case req.apply:
		written, err := rewrite.Apply(plan, ws.abs)
		if err != nil {
			return nil, out, err
		}
		out.Applied = true
		out.Files = written
		ws.loader.Forget(mapAbs(ws, written)...)
		ws.invalidate()
	default:
		ws.store.Put(cs)
		out.NextStep = fmt.Sprintf("preview only; call go_apply with changeset_id=%q to write", cs.ID)
	}

	return text("%s", renderPlan(req.summary, plan, out)), out, nil
}

func mapAbs(ws *workspace, rels []string) []string {
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = ws.abs(r)
	}
	return out
}

func renderPlan(summary string, plan *rewrite.Plan, out ChangeOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", summary)
	switch {
	case out.Applied:
		fmt.Fprintf(&b, "APPLIED to %d file(s)\n", len(out.Files))
	case out.Blocked:
		b.WriteString("BLOCKED by conflicts; nothing written\n")
	default:
		fmt.Fprintf(&b, "PREVIEW only; changeset %s\n", out.ChangeSetID)
	}

	shown, elided := 0, 0
	for _, c := range plan.Changes {
		if shown >= maxHunksTotal {
			elided += c.TotalHunks
			continue
		}
		fmt.Fprintf(&b, "\n--- %s (%d edit(s))\n", c.Path, c.Edits)
		if c.WasBroken {
			b.WriteString("    note: this file did not parse before the edit\n")
		}
		for _, h := range c.Hunks {
			if shown >= maxHunksTotal {
				elided += c.TotalHunks - shown
				break
			}
			shown++
			for _, line := range strings.Split(h.Old, "\n") {
				fmt.Fprintf(&b, "  %4d - %s\n", h.Line, line)
			}
			for _, line := range strings.Split(h.New, "\n") {
				fmt.Fprintf(&b, "       + %s\n", line)
			}
		}
		if c.TotalHunks > len(c.Hunks) {
			fmt.Fprintf(&b, "  ... %d more hunk(s) in this file not shown\n", c.TotalHunks-len(c.Hunks))
		}
	}
	if elided > 0 {
		fmt.Fprintf(&b, "\n... %d further hunk(s) across %d file(s) not shown; the changeset still covers them\n",
			elided, len(plan.Changes))
	}

	for _, c := range out.Conflicts {
		fmt.Fprintf(&b, "\nCONFLICT [%s] %s", c.Kind, c.Message)
		if c.Position != "" {
			fmt.Fprintf(&b, " (%s)", c.Position)
		}
		b.WriteByte('\n')
	}
	for _, w := range out.Warnings {
		fmt.Fprintf(&b, "\nwarning: %s\n", w)
	}
	if out.NextStep != "" {
		fmt.Fprintf(&b, "\n%s\n", out.NextStep)
	}
	return b.String()
}

// RewriteInput drives a query-template rewrite.
type RewriteInput struct {
	Query    string `json:"query" jsonschema:"Tree-sitter query. Must bind at least one capture"`
	Template string `json:"template" jsonschema:"Replacement text for the target capture. Use ${name} to insert a capture, $$ for a literal dollar. Empty string deletes the node"`
	Target   string `json:"target,omitempty" jsonschema:"Name of the capture to replace, without the @. Defaults to the first capture in each match"`
	Selector string `json:"selector,omitempty" jsonschema:"Where to rewrite; same forms as go_read. Defaults to the whole project"`
	Apply    bool   `json:"apply,omitempty" jsonschema:"Write immediately instead of returning a preview"`
	MaxSites int    `json:"max_sites,omitempty" jsonschema:"Refuse if more than this many sites match (default 2000). A guard against a query that is broader than intended"`
	Root     string `json:"root,omitempty" jsonschema:"Project directory to operate in. Defaults to the server's working directory. Pass this to work on a different module; it resolves to the enclosing go.mod"`
}

// A mechanical migration across a large repository legitimately touches
// thousands of sites; this is a guard against a query broader than intended,
// not a product limit.
const defaultMaxSites = 2000

func (s *Server) rewriteQuery(ctx context.Context, _ *mcp.CallToolRequest, in RewriteInput) (*mcp.CallToolResult, ChangeOutput, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	pkgs, err := ws.loader.Resolve(ctx, in.Selector)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	q, err := ws.loader.Compile(in.Query)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	limit := cmpOr(in.MaxSites, defaultMaxSites)

	var edits []rewrite.Edit
	sources := make(map[string][]byte)
	var warnings []string

	for _, path := range source.Files(pkgs) {
		if err := ctx.Err(); err != nil {
			return nil, ChangeOutput{}, err
		}
		f, err := ws.loader.File(path)
		if err != nil {
			warnings = append(warnings, err.Error())
			continue
		}
		for _, match := range q.Execute(f.Tree) {
			if len(match.Captures) == 0 {
				continue
			}
			caps := make(map[string]string, len(match.Captures))
			for _, c := range match.Captures {
				caps[c.Name] = c.Text(f.Src)
			}

			target := match.Captures[0]
			if in.Target != "" {
				found := false
				for _, c := range match.Captures {
					if c.Name == in.Target {
						target, found = c, true
						break
					}
				}
				if !found {
					return nil, ChangeOutput{}, fmt.Errorf(
						"target capture @%s not bound by this match at %s; bound captures are %s",
						in.Target, ws.loader.Rel(path), strings.Join(captureNames(caps), ", "))
				}
			}

			replacement, err := rewrite.Expand(in.Template, caps)
			if err != nil {
				return nil, ChangeOutput{}, err
			}
			if len(edits) >= limit {
				return nil, ChangeOutput{}, fmt.Errorf(
					"more than %d sites match; narrow the query or the selector, or raise max_sites", limit)
			}
			edits = append(edits, rewrite.Edit{
				Path:  path,
				Start: target.Node.StartByte(),
				End:   target.Node.EndByte(),
				New:   replacement,
				Note:  "matched @" + target.Name,
			})
			sources[path] = f.Src
		}
	}

	return ws.stage(stageRequest{
		summary:  fmt.Sprintf("rewrite %d site(s) matching the query", len(edits)),
		edits:    edits,
		sources:  sources,
		warnings: warnings,
		apply:    in.Apply,
	})
}

func captureNames(caps map[string]string) []string {
	out := make([]string, 0, len(caps))
	for k := range caps {
		out = append(out, "@"+k)
	}
	return out
}

// EditSpec is one explicit range replacement.
type EditSpec struct {
	File      string `json:"file" jsonschema:"Path to the Go file, relative to the project root"`
	StartLine int    `json:"start_line" jsonschema:"1-based first line"`
	StartCol  int    `json:"start_col" jsonschema:"1-based first column, in bytes"`
	EndLine   int    `json:"end_line" jsonschema:"1-based last line"`
	EndCol    int    `json:"end_col" jsonschema:"1-based column just past the last byte to replace"`
	New       string `json:"new" jsonschema:"Replacement text. Empty deletes the range"`
	Note      string `json:"note,omitempty" jsonschema:"Why this edit exists; shown in the preview"`
}

// EditInput is a batch of explicit edits.
type EditInput struct {
	Edits []EditSpec `json:"edits" jsonschema:"Edits to apply. They may span files but must not overlap"`
	Apply bool       `json:"apply,omitempty" jsonschema:"Write immediately instead of returning a preview"`
	Root  string     `json:"root,omitempty" jsonschema:"Project directory to operate in. Defaults to the server's working directory. Pass this to work on a different module; it resolves to the enclosing go.mod"`
}

func (s *Server) editRanges(_ context.Context, _ *mcp.CallToolRequest, in EditInput) (*mcp.CallToolResult, ChangeOutput, error) {
	ws, err := s.workspace(in.Root)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	if len(in.Edits) == 0 {
		return nil, ChangeOutput{}, fmt.Errorf("no edits supplied")
	}
	var edits []rewrite.Edit
	sources := make(map[string][]byte)

	for i, e := range in.Edits {
		f, err := ws.loader.File(e.File)
		if err != nil {
			return nil, ChangeOutput{}, fmt.Errorf("edit %d: %w", i, err)
		}
		start, err := byteOffset(f.Src, e.StartLine, e.StartCol)
		if err != nil {
			return nil, ChangeOutput{}, fmt.Errorf("edit %d start: %w", i, err)
		}
		end, err := byteOffset(f.Src, e.EndLine, e.EndCol)
		if err != nil {
			return nil, ChangeOutput{}, fmt.Errorf("edit %d end: %w", i, err)
		}
		if end < start {
			return nil, ChangeOutput{}, fmt.Errorf("edit %d: end is before start", i)
		}
		edits = append(edits, rewrite.Edit{
			Path:  f.Path,
			Start: uint32(start),
			End:   uint32(end),
			New:   e.New,
			Note:  e.Note,
		})
		sources[f.Path] = f.Src
	}

	return ws.stage(stageRequest{
		summary: fmt.Sprintf("apply %d explicit edit(s)", len(edits)),
		edits:   edits,
		sources: sources,
		apply:   in.Apply,
	})
}

// byteOffset converts a 1-based line and byte column to an offset.
func byteOffset(src []byte, line, col int) (int, error) {
	if line < 1 {
		return 0, fmt.Errorf("line %d: lines are 1-based", line)
	}
	if col < 1 {
		col = 1
	}
	cur, start := 1, 0
	for i := 0; i <= len(src); i++ {
		if cur == line {
			off := start + col - 1
			lineEnd := i
			for lineEnd < len(src) && src[lineEnd] != '\n' {
				lineEnd++
			}
			if off > lineEnd {
				return 0, fmt.Errorf("column %d is past the end of line %d", col, line)
			}
			return off, nil
		}
		if i < len(src) && src[i] == '\n' {
			cur++
			start = i + 1
		}
	}
	return 0, fmt.Errorf("line %d is past the end of the file", line)
}

// ApplyInput names a previewed changeset.
type ApplyInput struct {
	ChangeSetID string `json:"changeset_id" jsonschema:"The changeset_id returned by a preview"`
}

func (s *Server) apply(_ context.Context, _ *mcp.CallToolRequest, in ApplyInput) (*mcp.CallToolResult, ChangeOutput, error) {
	// A changeset knows its own workspace, so the caller does not have to
	// remember which module it previewed against.
	ws, cs, err := s.findChangeset(in.ChangeSetID)
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	plan, err := rewrite.Compute(cs, ws.loader.Rel, rewrite.Options{
		FixImports: true,
		MaxHunks:   maxHunksPerFile,
	})
	if err != nil {
		return nil, ChangeOutput{}, err
	}
	written, err := rewrite.Apply(plan, ws.abs)
	out := ChangeOutput{
		ChangeSetID: cs.ID,
		Applied:     err == nil,
		Sites:       len(cs.Edits),
		Files:       written,
		Warnings:    plan.Warnings,
	}
	if err != nil {
		return nil, out, err
	}
	ws.store.Delete(cs.ID)
	ws.loader.Forget(mapAbs(ws, written)...)
	ws.invalidate()
	return text("%s", renderPlan(cs.Summary, plan, out)), out, nil
}
