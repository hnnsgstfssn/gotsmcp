package source

import (
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

// Decl is one top-level declaration.
type Decl struct {
	Kind      string `json:"kind"`
	Name      string `json:"name,omitempty"`
	Recv      string `json:"recv,omitempty"`
	Line      int    `json:"line"`
	EndLine   int    `json:"end_line"`
	Signature string `json:"signature"`
	Doc       string `json:"doc,omitempty"`
}

// Outline renders a file as its declarations with function bodies elided.
//
// This is the default read mode because it is the token-efficient one: a
// 3000-line file collapses to its API surface, and an agent that needs a body
// can ask for the specific declaration afterwards.
func Outline(f *File, lang *ts.Language) []Decl {
	root := f.Tree.RootNode()
	var out []Decl
	for i := range root.ChildCount() {
		n := root.Child(i)
		if n == nil || !n.IsNamed() {
			continue
		}
		kind := n.Type(lang)
		switch kind {
		case "comment":
			continue // consumed as doc by the declaration that follows
		case "package_clause", "import_declaration":
			out = append(out, Decl{
				Kind:      strings.TrimSuffix(kind, "_declaration"),
				Line:      int(n.StartPoint().Row) + 1,
				EndLine:   int(n.EndPoint().Row) + 1,
				Signature: n.Text(f.Src),
				// The package doc sits above the package clause and is often
				// the most useful line in the file.
				Doc: docFor(f, lang, n),
			})
		case "function_declaration", "method_declaration":
			out = append(out, funcDecl(f, lang, n, kind))
		case "type_declaration", "const_declaration", "var_declaration":
			d := Decl{
				Kind:      strings.TrimSuffix(kind, "_declaration"),
				Name:      declName(f, lang, n),
				Line:      int(n.StartPoint().Row) + 1,
				EndLine:   int(n.EndPoint().Row) + 1,
				Signature: n.Text(f.Src),
				Doc:       docFor(f, lang, n),
			}
			out = append(out, d)
		}
	}
	return out
}

func funcDecl(f *File, lang *ts.Language, n *ts.Node, kind string) Decl {
	d := Decl{
		Kind:    strings.TrimSuffix(kind, "_declaration"),
		Line:    int(n.StartPoint().Row) + 1,
		EndLine: int(n.EndPoint().Row) + 1,
		Doc:     docFor(f, lang, n),
	}
	if name := n.ChildByFieldName("name", lang); name != nil {
		d.Name = name.Text(f.Src)
	}
	if recv := n.ChildByFieldName("receiver", lang); recv != nil {
		d.Recv = recv.Text(f.Src)
	}
	// Signature is everything up to the body, so the reader sees params and
	// results without the implementation.
	end := n.EndByte()
	if body := n.ChildByFieldName("body", lang); body != nil {
		end = body.StartByte()
	}
	d.Signature = strings.TrimRight(string(f.Src[n.StartByte():end]), " \t\n") + " { ... }"
	return d
}

func declName(f *File, lang *ts.Language, n *ts.Node) string {
	var names []string
	ts.Walk(n, func(c *ts.Node, _ int) ts.WalkAction {
		switch c.Type(lang) {
		case "type_spec", "const_spec", "var_spec":
			if name := c.ChildByFieldName("name", lang); name != nil {
				names = append(names, name.Text(f.Src))
			}
		}
		return ts.WalkContinue
	})
	return strings.Join(names, ", ")
}

// docFor collects the run of comment lines immediately above n, stopping at the
// first blank line.
func docFor(f *File, lang *ts.Language, n *ts.Node) string {
	var lines []string
	prev, cur := n.PrevSibling(), n
	for prev != nil && prev.Type(lang) == "comment" {
		if int(cur.StartPoint().Row)-int(prev.EndPoint().Row) > 1 {
			break
		}
		lines = append([]string{prev.Text(f.Src)}, lines...)
		cur, prev = prev, prev.PrevSibling()
	}
	return strings.Join(lines, "\n")
}

// Render turns an outline back into Go-shaped text, which reads more naturally
// to a model than a JSON array of declarations.
func Render(decls []Decl) string {
	var b strings.Builder
	for i, d := range decls {
		if i > 0 {
			b.WriteString("\n")
		}
		if d.Doc != "" {
			b.WriteString(d.Doc)
			b.WriteByte('\n')
		}
		b.WriteString(d.Signature)
		b.WriteByte('\n')
	}
	return b.String()
}
