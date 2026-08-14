package sem

import (
	"fmt"
	"go/ast"
	"go/types"
	"path/filepath"

	"golang.org/x/tools/go/packages"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/rewrite"
)

// ErrNotMovable reports a declaration that cannot be relocated safely.
const ErrNotMovable Error = "declaration cannot be moved"

// Move is a computed relocation.
type Move struct {
	Symbol      Symbol         `json:"symbol"`
	From        string         `json:"from"`
	To          string         `json:"to"`
	SamePkg     bool           `json:"same_package"`
	Edits       []rewrite.Edit `json:"-"`
	Warnings    []string       `json:"warnings,omitempty"`
	Requalified int            `json:"requalified_references"`
}

// MoveDecl relocates a single declaration. It is MoveDecls with one symbol.
func (s *Snapshot) MoveDecl(obj types.Object, destFile string) (*Move, error) {
	return s.MoveDecls(MoveRequest{Symbols: []string{s.Describe(obj).Position}, To: destFile})
}

// declFor locates the top-level declaration that introduces obj.
func (s *Snapshot) declFor(obj types.Object) (ast.Decl, *ast.File, *packages.Package, error) {
	var (
		found ast.Decl
		inF   *ast.File
		inP   *packages.Package
	)
	s.unique(func(p *packages.Package) {
		if found != nil {
			return
		}
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				if obj.Pos() < d.Pos() || obj.Pos() >= d.End() {
					continue
				}
				// A grouped declaration holds several symbols; moving one out
				// of a group would need the group rewritten.
				if gd, ok := d.(*ast.GenDecl); ok && gd.Lparen.IsValid() && len(gd.Specs) > 1 {
					return
				}
				found, inF, inP = d, f, p
				return
			}
		}
	})
	if found == nil {
		return nil, nil, nil, fmt.Errorf(
			"%w: %s is not a standalone top-level declaration (a grouped const, var, or type block must be split first)",
			ErrNotMovable, obj.Name())
	}
	return found, inF, inP, nil
}

// fileFor confirms dest belongs to the same package as the declaration.
func (s *Snapshot) fileFor(dest string, pkg *packages.Package) (*ast.File, error) {
	var out *ast.File
	var wrongPkg string
	s.unique(func(p *packages.Package) {
		for _, f := range p.Syntax {
			if filepath.Clean(s.Fset.Position(f.Pos()).Filename) != dest {
				continue
			}
			if p.Name != pkg.Name {
				wrongPkg = p.Name
				return
			}
			out = f
		}
	})
	if wrongPkg != "" {
		return nil, fmt.Errorf("%w: %s is in package %s, not %s",
			ErrNotMovable, s.Rel(dest), wrongPkg, pkg.Name)
	}
	if out == nil {
		return nil, fmt.Errorf("%w: %s is not a Go file in this package", ErrNotMovable, s.Rel(dest))
	}
	return out, nil
}

// docComment returns the doc comment group attached to a declaration.
func docComment(d ast.Decl) *ast.CommentGroup {
	switch decl := d.(type) {
	case *ast.FuncDecl:
		return decl.Doc
	case *ast.GenDecl:
		return decl.Doc
	}
	return nil
}

// MoveSources returns the file contents a move touches, including a
// destination that does not exist yet.
func (s *Snapshot) MoveSources(m *Move) map[string][]byte {
	out := make(map[string][]byte, 2)
	for _, e := range m.Edits {
		if _, ok := out[e.Path]; ok {
			continue
		}
		b, err := s.Source(e.Path)
		if err != nil {
			b = nil // a file being created has no prior content
		}
		out[e.Path] = b
	}
	return out
}
