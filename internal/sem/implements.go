package sem

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Implementation is one side of an implements relation.
type Implementation struct {
	Type      string `json:"type"`
	Kind      string `json:"kind"`
	Package   string `json:"package,omitempty"`
	Position  string `json:"position"`
	PointerRx bool   `json:"pointer_receiver,omitempty"`
}

// Implements answers both directions of "what implements what".
//
// Given an interface it returns the concrete types satisfying it; given a
// concrete type it returns the interfaces it satisfies. This is the question an
// agent has before touching a method set, and neither grep nor a syntax query
// can answer it: satisfaction in Go is structural, so nothing in the source
// text of a type says which interfaces it implements.
func (s *Snapshot) Implements(obj types.Object) (iface bool, out []Implementation) {
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return false, nil
	}
	target := tn.Type()
	if types.IsInterface(target) {
		return true, s.implementors(target)
	}
	return false, s.satisfiedInterfaces(target)
}

// implementors finds every named type in the loaded packages that satisfies I.
func (s *Snapshot) implementors(iface types.Type) []Implementation {
	under, ok := types.Unalias(iface.Underlying()).(*types.Interface)
	if !ok || under.NumMethods() == 0 {
		// Every type satisfies the empty interface; saying so is noise.
		return nil
	}

	var out []Implementation
	seen := make(map[string]bool)
	s.eachNamedType(func(named *types.Named, tn *types.TypeName) {
		if types.IsInterface(named) {
			return
		}
		valueOK := types.Implements(named, under)
		ptrOK := types.Implements(types.NewPointer(named), under)
		if !valueOK && !ptrOK {
			return
		}
		pkgPath := pkgPathOf(tn)
		key := pkgPath + "." + tn.Name()
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Implementation{
			Type:      tn.Name(),
			Kind:      "type",
			Package:   pkgPath,
			Position:  s.posString(tn.Pos()),
			PointerRx: !valueOK && ptrOK,
		})
	})
	sortImpls(out)
	return out
}

// satisfiedInterfaces finds the interfaces a concrete type implements.
func (s *Snapshot) satisfiedInterfaces(t types.Type) []Implementation {
	ptr := types.NewPointer(t)

	var out []Implementation
	seen := make(map[string]bool)
	s.eachNamedType(func(named *types.Named, tn *types.TypeName) {
		under, ok := types.Unalias(named.Underlying()).(*types.Interface)
		if !ok || under.NumMethods() == 0 {
			return
		}
		valueOK := types.Implements(t, under)
		ptrOK := types.Implements(ptr, under)
		if !valueOK && !ptrOK {
			return
		}
		pkgPath := pkgPathOf(tn)
		key := pkgPath + "." + tn.Name()
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Implementation{
			Type:      tn.Name(),
			Kind:      "interface",
			Package:   pkgPath,
			Position:  s.posString(tn.Pos()),
			PointerRx: !valueOK && ptrOK,
		})
	})
	sortImpls(out)
	return out
}

// eachNamedType visits every named type declared in the loaded packages and
// their dependencies, so stdlib interfaces such as error and io.Reader are
// considered too.
func (s *Snapshot) eachNamedType(fn func(*types.Named, *types.TypeName)) {
	s.eachTypesPackage(func(p *types.Package) {
		scope := p.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || tn.Pkg() == nil {
				continue
			}
			if named, ok := types.Unalias(tn.Type()).(*types.Named); ok {
				fn(named, tn)
			}
		}
	})
	// error lives in the universe scope, not any package.
	if tn, ok := types.Universe.Lookup("error").(*types.TypeName); ok {
		if named, ok := tn.Type().(*types.Named); ok {
			fn(named, tn)
		}
	}
}

// pkgPathOf reports a type's import path. Universe-scope types such as error
// have no package, and dereferencing it there is a nil pointer panic.
func pkgPathOf(tn *types.TypeName) string {
	if tn.Pkg() == nil {
		return "builtin"
	}
	return tn.Pkg().Path()
}

func sortImpls(out []Implementation) {
	slices.SortFunc(out, func(a, b Implementation) int {
		if c := strings.Compare(a.Package, b.Package); c != 0 {
			return c
		}
		return strings.Compare(a.Type, b.Type)
	})
}

// Call is one call site of a function or method.
type Call struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
	// In names the declaration containing the call, which is what makes a
	// caller list readable: "who calls this" wants function names, not
	// line numbers.
	In      string `json:"in"`
	Package string `json:"package,omitempty"`
	Text    string `json:"text,omitempty"`
}

// Callers finds call sites of a function or method.
//
// go_refs reports every reference, including the declaration and any use as a
// value. This reports only calls, and names the enclosing declaration, which is
// the form an impact review actually needs.
func (s *Snapshot) Callers(obj types.Object) []Call {
	if _, ok := obj.(*types.Func); !ok {
		return nil
	}
	targets := s.targetSet(obj)

	var out []Call
	seen := make(map[string]bool)
	s.unique(func(p *packages.Package) {
		for _, f := range p.Syntax {
			// Resolve the enclosing declaration by position rather than by
			// tracking a stack during the walk: ast.Inspect calls back with nil
			// on leaving every node, not just the ones being tracked, so a
			// naive push/pop drains immediately.
			decls := funcSpans(f)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id := calleeIdent(call.Fun)
				if id == nil {
					return true
				}
				if _, ok := targets.lookup(p.TypesInfo.Uses[id]); !ok {
					return true
				}
				pos := s.Fset.Position(id.Pos())
				key := pos.String()
				if seen[key] {
					return true
				}
				seen[key] = true
				out = append(out, Call{
					File:    strings.ReplaceAll(s.Rel(pos.Filename), "\\", "/"),
					Line:    pos.Line,
					Col:     pos.Column,
					In:      enclosing(decls, id.Pos()),
					Package: p.PkgPath,
					Text:    s.lineText(pos.Filename, pos.Line),
				})
				return true
			})
		}
	})
	slices.SortFunc(out, func(a, b Call) int {
		if c := strings.Compare(a.File, b.File); c != 0 {
			return c
		}
		return a.Line - b.Line
	})
	return out
}

// funcSpan is a top-level function or method and the byte range it covers.
type funcSpan struct {
	start, end token.Pos
	name       string
}

func funcSpans(f *ast.File) []funcSpan {
	var out []funcSpan
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			out = append(out, funcSpan{start: fd.Pos(), end: fd.End(), name: declName(fd)})
		}
	}
	return out
}

// enclosing names the declaration containing pos. Top-level declarations do not
// overlap, so the first containing span is the answer even for a call inside a
// function literal.
func enclosing(spans []funcSpan, pos token.Pos) string {
	for _, s := range spans {
		if pos >= s.start && pos < s.end {
			return s.name
		}
	}
	return "<file scope>"
}

func declName(d *ast.FuncDecl) string {
	if d.Name == nil {
		return "<anonymous>"
	}
	if d.Recv != nil && len(d.Recv.List) > 0 {
		return recvTypeName(d.Recv.List[0].Type) + "." + d.Name.Name
	}
	return d.Name.Name
}

func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	}
	return "?"
}

// calleeIdent extracts the identifier naming the function being called.
func calleeIdent(fun ast.Expr) *ast.Ident {
	switch f := fun.(type) {
	case *ast.Ident:
		return f
	case *ast.SelectorExpr:
		return f.Sel
	case *ast.IndexExpr: // instantiated generic
		return calleeIdent(f.X)
	case *ast.IndexListExpr:
		return calleeIdent(f.X)
	case *ast.ParenExpr:
		return calleeIdent(f.X)
	}
	return nil
}
