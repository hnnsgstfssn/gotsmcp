package sem

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Symbol describes a resolved object for display.
type Symbol struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Package  string `json:"package,omitempty"`
	Type     string `json:"type,omitempty"`
	Position string `json:"position"`
	Exported bool   `json:"exported"`
}

// Resolve finds the object named by spec, which is either a position
// ("demo/demo.go:10:6") or a dotted path ("example.com/proj/demo.Config",
// "demo.Server.Addr", or the short form "demo.Config").
//
// Positions are the reliable form and are what the query tools emit, so an
// agent can chain a structural search into a rename without guessing at names.
func (s *Snapshot) Resolve(spec string) (types.Object, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("%w: empty symbol", ErrNotFound)
	}
	if path, line, col, ok := parsePosition(spec); ok {
		return s.resolvePosition(path, line, col)
	}
	return s.resolveDotted(spec)
}

// parsePosition recognises "path:line:col" and "path:line".
func parsePosition(spec string) (path string, line, col int, ok bool) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return "", 0, 0, false
	}
	line, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, 0, false
	}
	col = 1
	if len(parts) == 3 {
		if col, err = strconv.Atoi(parts[2]); err != nil {
			return "", 0, 0, false
		}
	}
	return parts[0], line, col, true
}

func (s *Snapshot) resolvePosition(path string, line, col int) (types.Object, error) {
	want := path
	if !filepath.IsAbs(want) {
		want = filepath.Join(s.root, path)
	}
	want = filepath.Clean(want)

	pos := s.posAt(want, line, col)
	if pos == token.NoPos {
		return nil, fmt.Errorf("%w: no source at %s:%d:%d", ErrNotFound, path, line, col)
	}

	var found types.Object
	s.unique(func(p *packages.Package) {
		if found != nil {
			return
		}
		for _, f := range p.Syntax {
			if f.Pos() > pos || pos > f.End() {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if found != nil {
					return false
				}
				id, ok := n.(*ast.Ident)
				if !ok || id.Pos() > pos || pos >= id.End() {
					return true
				}
				if obj := p.TypesInfo.Uses[id]; obj != nil {
					found = obj
				} else if obj := p.TypesInfo.Defs[id]; obj != nil {
					found = obj
				}
				return false
			})
		}
	})
	if found == nil {
		return nil, fmt.Errorf("%w: no identifier at %s:%d:%d", ErrNotFound, path, line, col)
	}
	return found, nil
}

// posAt converts a 1-based line and column in a known file to a token.Pos.
func (s *Snapshot) posAt(path string, line, col int) token.Pos {
	var out token.Pos
	s.Fset.Iterate(func(f *token.File) bool {
		if filepath.Clean(f.Name()) != path {
			return true
		}
		if line < 1 || line > f.LineCount() {
			return false
		}
		start := f.LineStart(line)
		p := start + token.Pos(col-1)
		if p > token.Pos(f.Base()+f.Size()) {
			p = start
		}
		out = p
		return false
	})
	return out
}

func (s *Snapshot) resolveDotted(spec string) (types.Object, error) {
	pkgPart, names := splitSpec(spec)
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: %q needs a package and a symbol, e.g. demo.Config", ErrNotFound, spec)
	}

	var candidates []types.Object
	s.unique(func(p *packages.Package) {
		if p.Types == nil || !matchPackage(p, pkgPart) {
			return
		}
		obj := p.Types.Scope().Lookup(names[0])
		if obj == nil {
			return
		}
		if len(names) == 1 {
			candidates = append(candidates, obj)
			return
		}
		if member := lookupMember(obj, names[1]); member != nil {
			candidates = append(candidates, member)
		}
	})

	// Dependencies have types but no syntax, so the pass above misses them.
	// Falling back keeps io.Writer and friends addressable.
	if len(candidates) == 0 {
		s.eachTypesPackage(func(tp *types.Package) {
			if !matchTypesPackage(tp, pkgPart) {
				return
			}
			obj := tp.Scope().Lookup(names[0])
			if obj == nil {
				return
			}
			if len(names) == 1 {
				candidates = append(candidates, obj)
				return
			}
			if member := lookupMember(obj, names[1]); member != nil {
				candidates = append(candidates, member)
			}
		})
	}

	// The same object can surface through a package and its test variant.
	candidates = dedupeObjects(candidates)
	switch len(candidates) {
	case 0:
		return nil, fmt.Errorf("%w: %q", ErrNotFound, spec)
	case 1:
		return candidates[0], nil
	default:
		var where []string
		for _, c := range candidates {
			where = append(where, s.Describe(c).Position)
		}
		return nil, fmt.Errorf("%w: %q matches %s; use file:line:col instead",
			ErrAmbiguous, spec, strings.Join(where, ", "))
	}
}

// splitSpec separates the package part from the dotted member names. Import
// paths contain dots, so the split happens after the final slash.
func splitSpec(spec string) (pkgPart string, names []string) {
	head, tail := "", spec
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		head, tail = spec[:i+1], spec[i+1:]
	}
	parts := strings.Split(tail, ".")
	return head + parts[0], parts[1:]
}

func matchPackage(p *packages.Package, want string) bool {
	path := cleanPath(p.PkgPath)
	return path == want || p.Name == want || strings.HasSuffix(path, "/"+want)
}

func matchTypesPackage(p *types.Package, want string) bool {
	path := cleanPath(p.Path())
	return path == want || p.Name() == want || strings.HasSuffix(path, "/"+want)
}

func cleanPath(p string) string {
	base, _, _ := strings.Cut(p, " ")
	return base
}

// lookupMember finds a method or field named name on the type denoted by obj.
func lookupMember(obj types.Object, name string) types.Object {
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return nil
	}
	t := tn.Type()
	if m, _, _ := types.LookupFieldOrMethod(t, true, tn.Pkg(), name); m != nil {
		return m
	}
	if m, _, _ := types.LookupFieldOrMethod(types.NewPointer(t), true, tn.Pkg(), name); m != nil {
		return m
	}
	return nil
}

// dedupeObjects collapses objects by declaration position.
//
// A package and its test variant are type-checked separately, so one
// declaration yields two distinct *types.Object values. Deduplicating by
// pointer would report every symbol in a package that has tests as ambiguous.
func dedupeObjects(objs []types.Object) []types.Object {
	seen := make(map[token.Pos]bool, len(objs))
	out := objs[:0]
	for _, o := range objs {
		if o.Pos().IsValid() && seen[o.Pos()] {
			continue
		}
		seen[o.Pos()] = true
		out = append(out, o)
	}
	return out
}

// Describe renders an object for display.
func (s *Snapshot) Describe(obj types.Object) Symbol {
	sym := Symbol{
		Name:     obj.Name(),
		Kind:     kindOf(obj),
		Exported: obj.Exported(),
		Position: s.posString(obj.Pos()),
	}
	if obj.Pkg() != nil {
		sym.Package = obj.Pkg().Path()
	}
	if t := obj.Type(); t != nil {
		sym.Type = types.TypeString(t, func(p *types.Package) string { return p.Name() })
	}
	return sym
}

func (s *Snapshot) posString(pos token.Pos) string {
	p := s.Fset.Position(pos)
	if !p.IsValid() {
		return "<unknown>"
	}
	return fmt.Sprintf("%s:%d:%d", filepath.ToSlash(s.Rel(p.Filename)), p.Line, p.Column)
}

func kindOf(obj types.Object) string {
	switch o := obj.(type) {
	case *types.TypeName:
		return "type"
	case *types.Func:
		if sig, ok := o.Type().(*types.Signature); ok && sig.Recv() != nil {
			return "method"
		}
		return "func"
	case *types.Var:
		switch {
		case o.IsField():
			return "field"
		case o.Parent() == nil || o.Parent() == types.Universe:
			return "var"
		case o.Parent().Parent() == types.Universe:
			return "var"
		default:
			return "local"
		}
	case *types.Const:
		return "const"
	case *types.PkgName:
		return "import"
	case *types.Label:
		return "label"
	case *types.Builtin:
		return "builtin"
	case *types.Nil:
		return "nil"
	}
	return "object"
}
