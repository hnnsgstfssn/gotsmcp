package source

import (
	"fmt"
	"strconv"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// TreeOptions controls S-expression rendering.
type TreeOptions struct {
	// MaxDepth limits nesting below the starting node. Zero means unlimited.
	MaxDepth int
	// Anonymous includes unnamed tokens such as "func" and "{". Queries can
	// match them, but they roughly triple the output size.
	Anonymous bool
	// Text appends the source text of leaf nodes, truncated.
	Text bool
	// MaxNodes caps output size. Zero applies defaultMaxNodes.
	MaxNodes int
}

// The median Go file in a real repository is around 6 KB, which is several
// thousand nodes at full depth, so 2000 truncated the typical file.
const (
	defaultMaxNodes = 6000
	maxLeafText     = 48
)

// RenderTree writes node as an indented S-expression annotated with field names
// and 1-based line:column spans.
//
// This is the grammar-discovery tool: tree-sitter queries are written against
// exact node type names, and an agent that guesses "function_decl" instead of
// "function_declaration" gets a silently empty result set. Showing the real
// tree for the code in question removes the guess.
func RenderTree(f *File, lang *ts.Language, node *ts.Node, opt TreeOptions) string {
	if node == nil {
		node = f.Tree.RootNode()
	}
	if opt.MaxNodes <= 0 {
		opt.MaxNodes = defaultMaxNodes
	}
	r := &treeRenderer{src: f.Src, lang: lang, opt: opt}
	r.node(node, "", "", 0)
	if r.truncated {
		fmt.Fprintf(&r.b, "\n; output truncated at %d nodes; narrow the range or lower max_depth\n", opt.MaxNodes)
	}
	return r.b.String()
}

type treeRenderer struct {
	b         strings.Builder
	src       []byte
	lang      *ts.Language
	opt       TreeOptions
	count     int
	truncated bool
}

func (r *treeRenderer) node(n *ts.Node, indent, field string, depth int) {
	if r.truncated {
		return
	}
	if r.count >= r.opt.MaxNodes {
		r.truncated = true
		return
	}
	r.count++

	if r.b.Len() > 0 {
		r.b.WriteByte('\n')
	}
	r.b.WriteString(indent)
	if field != "" {
		r.b.WriteString(field)
		r.b.WriteString(": ")
	}

	kind := n.Type(r.lang)
	if !n.IsNamed() {
		// Anonymous tokens are their own type name; quote them so they read
		// the way they must be written in a query.
		kind = strconv.Quote(kind)
	}
	fmt.Fprintf(&r.b, "(%s %s", kind, span(n))

	switch {
	case n.IsMissing():
		r.b.WriteString(" MISSING")
	case n.IsError():
		r.b.WriteString(" ERROR")
	}

	children := r.visibleChildren(n)
	if len(children) == 0 {
		if r.opt.Text {
			if t := leafText(n, r.src); t != "" {
				r.b.WriteByte(' ')
				r.b.WriteString(t)
			}
		}
		r.b.WriteByte(')')
		return
	}

	if r.opt.MaxDepth > 0 && depth >= r.opt.MaxDepth {
		fmt.Fprintf(&r.b, " ...%d children)", len(children))
		return
	}

	for _, c := range children {
		r.node(c.node, indent+"  ", c.field, depth+1)
	}
	r.b.WriteByte(')')
}

type child struct {
	node  *ts.Node
	field string
}

func (r *treeRenderer) visibleChildren(n *ts.Node) []child {
	out := make([]child, 0, n.ChildCount())
	for i := range n.ChildCount() {
		c := n.Child(i)
		if c == nil || (!c.IsNamed() && !r.opt.Anonymous) {
			continue
		}
		out = append(out, child{node: c, field: n.FieldNameForChild(i, r.lang)})
	}
	return out
}

func span(n *ts.Node) string {
	s, e := n.StartPoint(), n.EndPoint()
	if s.Row == e.Row {
		return fmt.Sprintf("[%d:%d-%d]", s.Row+1, s.Column+1, e.Column+1)
	}
	return fmt.Sprintf("[%d:%d-%d:%d]", s.Row+1, s.Column+1, e.Row+1, e.Column+1)
}

func leafText(n *ts.Node, src []byte) string {
	t := n.Text(src)
	if t == "" {
		return ""
	}
	if len(t) > maxLeafText {
		t = t[:maxLeafText] + "..."
	}
	return strconv.Quote(t)
}

// NodeAt returns the smallest named node covering a 1-based line and column.
func NodeAt(f *File, line, col int) (*ts.Node, error) {
	if line < 1 {
		return nil, fmt.Errorf("line %d: lines are 1-based", line)
	}
	if col < 1 {
		col = 1
	}
	p := ts.Point{Row: uint32(line - 1), Column: uint32(col - 1)}
	n := f.Tree.RootNode().NamedDescendantForPointRange(p, p)
	if n == nil {
		return nil, fmt.Errorf("no node at %s:%d:%d", f.Path, line, col)
	}
	return n, nil
}

// NodeForLines returns the smallest named node spanning an inclusive 1-based
// line range, which is how a caller asks for "the tree of this function".
func NodeForLines(f *File, startLine, endLine int) (*ts.Node, error) {
	if startLine < 1 {
		startLine = 1
	}
	if endLine < startLine {
		endLine = startLine
	}
	start := ts.Point{Row: uint32(startLine - 1), Column: 0}
	end := ts.Point{Row: uint32(endLine - 1), Column: ^uint32(0) >> 1}
	n := f.Tree.RootNode().NamedDescendantForPointRange(start, end)
	if n == nil {
		return nil, fmt.Errorf("no node covering lines %d-%d", startLine, endLine)
	}
	return n, nil
}

// SyntaxErrors reports parse errors as line:column messages. tree-sitter
// recovers rather than failing, so a tree is always produced; this is how a
// caller learns the tree is partly guesswork.
func SyntaxErrors(f *File, lang *ts.Language, limit int) []string {
	if !f.Tree.RootNode().HasError() {
		return nil
	}
	var out []string
	ts.Walk(f.Tree.RootNode(), func(n *ts.Node, _ int) ts.WalkAction {
		if len(out) >= limit {
			return ts.WalkStop
		}
		switch {
		case n.IsMissing():
			out = append(out, fmt.Sprintf("%s: missing %s", span(n), n.Type(lang)))
		case n.IsError():
			out = append(out, fmt.Sprintf("%s: unexpected input near %s", span(n), leafText(n, f.Src)))
		}
		return ts.WalkContinue
	})
	return out
}
