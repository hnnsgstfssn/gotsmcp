package sem

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Ref is one occurrence of a symbol.
type Ref struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

// site is an identifier that resolves to one of the target objects, located by
// byte offset so it can become an edit directly.
type site struct {
	path  string
	start int
	end   int
	pos   token.Pos
	isDef bool
	// isSel marks the Sel of a selector expression. Such a reference is
	// resolved through the operand's type, not lexical scope, so lexical
	// shadowing cannot capture it.
	isSel bool
	pkg   *packages.Package
	// via names the object actually matched, which differs from the primary
	// target for the implicit field of an embedded type.
	via types.Object
}

// sites finds every identifier in the snapshot that resolves to a target
// object, by pointer identity.
//
// Identity is the whole point. A textual scan for the name would also match the
// same spelling in another package, a struct field, a shadowing local, and a
// string literal. Comparing *types.Object pointers matches exactly the
// occurrences the compiler considers to be this symbol.
func (s *Snapshot) sites(want *targets) []site {
	var out []site
	seen := make(map[string]bool)

	s.unique(func(p *packages.Package) {
		for _, f := range p.Syntax {
			// Inspect is pre-order, so a SelectorExpr is always seen before
			// its own Sel identifier.
			selectors := make(map[*ast.Ident]bool)
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					selectors[sel.Sel] = true
					return true
				}
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				obj, isDef := p.TypesInfo.Defs[id], true
				if obj == nil {
					obj, isDef = p.TypesInfo.Uses[id], false
				}
				rep, ok := want.lookup(obj)
				if !ok {
					return true
				}
				path, start, ok := s.offset(id.Pos())
				if !ok {
					return true
				}
				// A file compiled into several package variants yields the
				// same identifier more than once.
				key := path + ":" + strconv.Itoa(start)
				if seen[key] {
					return true
				}
				seen[key] = true
				out = append(out, site{
					path:  path,
					start: start,
					end:   start + len(id.Name),
					pos:   id.Pos(),
					isDef: isDef,
					isSel: selectors[id],
					pkg:   p,
					via:   rep,
				})
				return true
			})
		}
	})

	slices.SortFunc(out, func(a, b site) int {
		if a.path != b.path {
			return strings.Compare(a.path, b.path)
		}
		return a.start - b.start
	})
	return out
}

// References lists every occurrence of obj, declaration first.
func (s *Snapshot) References(obj types.Object) []Ref {
	targets := s.targetSet(obj)
	sites := s.sites(targets)

	refs := make([]Ref, 0, len(sites))
	for _, st := range sites {
		p := s.Fset.Position(st.pos)
		kind := "reference"
		if st.isDef {
			kind = "declaration"
		}
		if !samePos(st.via, obj) {
			kind = "embedded-field"
		}
		refs = append(refs, Ref{
			File: filepath.ToSlash(s.Rel(st.path)),
			Line: p.Line,
			Col:  p.Column,
			Kind: kind,
			Text: s.lineText(st.path, p.Line),
		})
	}
	return refs
}

// lineText returns the trimmed source line for display.
func (s *Snapshot) lineText(path string, line int) string {
	src, err := s.Source(path)
	if err != nil {
		return ""
	}
	cur := 1
	start := 0
	for i := 0; i < len(src); i++ {
		if cur == line {
			end := i
			for end < len(src) && src[end] != '\n' {
				end++
			}
			return strings.TrimSpace(string(src[start:end]))
		}
		if src[i] == '\n' {
			cur++
			start = i + 1
		}
	}
	return ""
}
