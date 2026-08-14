package sem

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// MoveRequest describes what to relocate and where.
type MoveRequest struct {
	// Symbols names individual declarations.
	Symbols []string
	// Files names whole files whose every declaration moves. This is the
	// natural unit for splitting a package.
	Files []string
	// To is a destination file when it ends in .go, otherwise a directory, in
	// which case each source file keeps its base name.
	To string
	// IncludeDeps pulls in the unexported symbols the moving set needs, rather
	// than refusing because they would be left behind.
	IncludeDeps bool
}

// movedDecl is one declaration being relocated, with everything it declares.
//
// A declaration can introduce several objects: "var a, b = 1, 2" is one
// GenDecl and two Vars, and moving it moves both.
type movedDecl struct {
	decl ast.Decl
	objs []types.Object
	file string
}

// moveSet is the aggregate being moved, which is the unit validation has to
// work on.
//
// Validating one declaration at a time is what made this tool useless for the
// job it is most needed for. Splitting a package means moving a cluster of
// declarations that necessarily reference each other's unexported symbols;
// judged individually every one of them looks like it is abandoning a
// dependency, when in fact the dependency is moving too.
type moveSet struct {
	pkg   *packages.Package
	decls []movedDecl
	// byPos indexes every moved object by declaration position, which is how
	// membership is tested across package variants.
	byPos map[token.Pos]bool
	// refs caches the single reference pass; see setRefSites.
	refs map[token.Pos][]declRefs
}

func (ms *moveSet) has(obj types.Object) bool {
	return obj != nil && obj.Pos().IsValid() && ms.byPos[obj.Pos()]
}

func (ms *moveSet) add(d movedDecl) {
	// Growing the set invalidates any cached reference pass.
	ms.refs = nil
	ms.decls = append(ms.decls, d)
	for _, o := range d.objs {
		if o.Pos().IsValid() {
			ms.byPos[o.Pos()] = true
		}
	}
}

func (ms *moveSet) hasDecl(d ast.Decl) bool {
	return slices.ContainsFunc(ms.decls, func(m movedDecl) bool { return m.decl == d })
}

// covers reports whether pos falls inside any moved declaration, so references
// between moving declarations are left alone: they travel together.
func (ms *moveSet) covers(pos token.Pos) bool {
	for _, d := range ms.decls {
		from := d.decl.Pos()
		if doc := docComment(d.decl); doc != nil {
			from = doc.Pos()
		}
		if pos >= from && pos < d.decl.End() {
			return true
		}
	}
	return false
}

func (ms *moveSet) names() []string {
	var out []string
	for _, d := range ms.decls {
		for _, o := range d.objs {
			out = append(out, o.Name())
		}
	}
	slices.Sort(out)
	return out
}

// resolveMoveSet turns a request into the set of declarations to move.
func (s *Snapshot) resolveMoveSet(req MoveRequest) (*moveSet, error) {
	ms := &moveSet{byPos: make(map[token.Pos]bool)}
	seen := make(map[ast.Decl]bool)

	addDecl := func(decl ast.Decl, pkg *packages.Package, file string) error {
		if seen[decl] {
			return nil
		}
		if ms.pkg != nil && cleanPath(ms.pkg.PkgPath) != cleanPath(pkg.PkgPath) {
			return fmt.Errorf("%w: everything moved together must start in one package, got %s and %s",
				ErrNotMovable, cleanPath(ms.pkg.PkgPath), cleanPath(pkg.PkgPath))
		}
		ms.pkg = pkg
		seen[decl] = true
		ms.add(movedDecl{decl: decl, objs: declObjects(decl, pkg), file: file})
		return nil
	}

	for _, spec := range req.Symbols {
		obj, err := s.Resolve(spec)
		if err != nil {
			return nil, err
		}
		decl, _, pkg, err := s.declFor(obj)
		if err != nil {
			return nil, err
		}
		if err := addDecl(decl, pkg, s.Fset.Position(decl.Pos()).Filename); err != nil {
			return nil, err
		}
	}

	for _, name := range req.Files {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(s.root, path)
		}
		path = filepath.Clean(path)
		pkg, f, err := s.astFile(path)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			// Import declarations belong to the file, not the package.
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
				continue
			}
			if err := addDecl(d, pkg, path); err != nil {
				return nil, err
			}
		}
	}

	if len(ms.decls) == 0 {
		return nil, fmt.Errorf("%w: no declarations selected; pass symbols or files", ErrNotMovable)
	}
	return ms, nil
}

// declObjects lists everything a declaration introduces.
func declObjects(decl ast.Decl, pkg *packages.Package) []types.Object {
	var out []types.Object
	add := func(id *ast.Ident) {
		if id == nil || id.Name == "_" {
			return
		}
		if obj := pkg.TypesInfo.Defs[id]; obj != nil {
			out = append(out, obj)
		}
	}
	switch d := decl.(type) {
	case *ast.FuncDecl:
		add(d.Name)
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch sp := spec.(type) {
			case *ast.TypeSpec:
				add(sp.Name)
			case *ast.ValueSpec:
				for _, n := range sp.Names {
					add(n)
				}
			}
		}
	}
	return out
}

// expand grows the set to everything that must travel with it.
//
// Methods are not optional: a method cannot be declared on a type from another
// package, so moving a type without its methods produces code that cannot
// compile. Unexported dependencies are optional, because pulling them in is a
// judgement about how much of the package is moving.
func (s *Snapshot) expand(ms *moveSet, includeDeps bool) []string {
	var pulled []string
	for {
		before := len(ms.decls)

		// Methods follow their receiver type.
		s.eachDeclIn(ms.pkg, func(decl ast.Decl, file string) {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 || ms.hasDecl(decl) {
				return
			}
			recv := ms.pkg.TypesInfo.Defs[fd.Name]
			fn, ok := recv.(*types.Func)
			if !ok {
				return
			}
			sig, ok := fn.Type().(*types.Signature)
			if !ok || sig.Recv() == nil {
				return
			}
			named := namedOf(sig.Recv().Type())
			if named == nil || !ms.has(named.Obj()) {
				return
			}
			ms.add(movedDecl{decl: decl, objs: declObjects(decl, ms.pkg), file: file})
			pulled = append(pulled, "method "+fn.Name())
		})

		if includeDeps {
			for _, name := range s.missingDeps(ms) {
				obj := ms.pkg.Types.Scope().Lookup(name)
				if obj == nil {
					continue
				}
				decl, _, pkg, err := s.declFor(obj)
				if err != nil || ms.hasDecl(decl) {
					continue
				}
				ms.add(movedDecl{decl: decl, objs: declObjects(decl, pkg),
					file: s.Fset.Position(decl.Pos()).Filename})
				pulled = append(pulled, name)
			}
		}

		if len(ms.decls) == before {
			break
		}
	}
	slices.Sort(pulled)
	return slices.Compact(pulled)
}

// eachDeclIn visits every top-level declaration of a package.
func (s *Snapshot) eachDeclIn(pkg *packages.Package, fn func(ast.Decl, string)) {
	for _, f := range pkg.Syntax {
		file := s.Fset.Position(f.Pos()).Filename
		for _, d := range f.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
				continue
			}
			fn(d, file)
		}
	}
}

// missingDeps lists unexported package symbols the set uses but leaves behind.
func (s *Snapshot) missingDeps(ms *moveSet) []string {
	var out []string
	seen := make(map[string]bool)
	for _, d := range ms.decls {
		ast.Inspect(d.decl, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			obj := ms.pkg.TypesInfo.Uses[id]
			if !isPackageLevel(obj, ms.pkg) || obj.Exported() || ms.has(obj) || seen[obj.Name()] {
				return true
			}
			seen[obj.Name()] = true
			out = append(out, obj.Name())
			return true
		})
	}
	slices.Sort(out)
	return out
}

// strandedNames lists unexported symbols the set takes away that declarations
// left behind still use.
func (s *Snapshot) strandedNames(ms *moveSet) []string {
	var out []string
	seen := make(map[string]bool)
	s.eachDeclIn(ms.pkg, func(decl ast.Decl, _ string) {
		if ms.hasDecl(decl) {
			return
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			obj := ms.pkg.TypesInfo.Uses[id]
			if !isPackageLevel(obj, ms.pkg) || obj.Exported() || !ms.has(obj) || seen[obj.Name()] {
				return true
			}
			seen[obj.Name()] = true
			out = append(out, obj.Name())
			return true
		})
	})
	slices.Sort(out)
	return out
}

func isPackageLevel(obj types.Object, pkg *packages.Package) bool {
	return obj != nil && obj.Pkg() == pkg.Types &&
		obj.Parent() == pkg.Types.Scope()
}

// validateSet checks the aggregate rather than each declaration alone.
func (s *Snapshot) validateSet(ms *moveSet, dest destination, includeDeps bool) error {
	if missing := s.missingDeps(ms); len(missing) > 0 {
		hint := "add them to symbols, or pass include_dependencies=true to pull them in automatically"
		if includeDeps {
			hint = "they could not be resolved to movable declarations"
		}
		return fmt.Errorf(
			"%w: the selection uses unexported symbol(s) of %s that would be left behind: %s. %s",
			ErrNotMovable, ms.pkg.Name, strings.Join(missing, ", "), hint)
	}

	if stranded := s.strandedNames(ms); len(stranded) > 0 {
		return fmt.Errorf(
			"%w: unexported symbol(s) %s would move away while declarations staying in %s still use them. "+
				"Export them, or move their users too",
			ErrNotMovable, strings.Join(stranded, ", "), ms.pkg.Name)
	}

	// An unexported symbol that anything outside the destination references
	// cannot survive the move.
	for _, d := range ms.decls {
		for _, obj := range d.objs {
			if obj.Exported() {
				continue
			}
			for _, r := range s.setRefSites(ms, obj) {
				if cleanPath(r.pkg.PkgPath) != dest.importPath && cleanPath(r.pkg.PkgPath) != cleanPath(ms.pkg.PkgPath) {
					return fmt.Errorf(
						"%w: %s is unexported and referenced from %s, which could not see it after the move. "+
							"Rename it to an exported name first",
						ErrNotMovable, obj.Name(), cleanPath(r.pkg.PkgPath))
				}
			}
		}
	}
	return nil
}

// setRefSites finds references to obj that are outside every moved declaration.
//
// It reads from a single pre-computed pass. Walking the whole program once per
// moved object turned a 42-declaration analysis on a real repository into 42
// full-program walks and 44 seconds; one walk that buckets by target is the
// same work divided by the size of the selection.
func (s *Snapshot) setRefSites(ms *moveSet, obj types.Object) []declRefs {
	if ms.refs == nil {
		ms.refs = s.allRefSites(ms)
	}
	return ms.refs[obj.Pos()]
}

// allRefSites buckets every reference into the moved object it belongs to, in
// one traversal.
func (s *Snapshot) allRefSites(ms *moveSet) map[token.Pos][]declRefs {
	out := make(map[token.Pos][]declRefs)
	seen := make(map[string]bool)

	s.unique(func(p *packages.Package) {
		for _, f := range p.Syntax {
			selOf := make(map[*ast.Ident]*ast.SelectorExpr)
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					selOf[sel.Sel] = sel
				}
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if ms.covers(id.Pos()) {
					return true
				}
				used := p.TypesInfo.Uses[id]
				if used == nil || !ms.has(used) {
					return true
				}
				node := ast.Node(id)
				qualified := false
				if sel, ok := selOf[id]; ok {
					x, isIdent := sel.X.(*ast.Ident)
					_, isPkg := p.TypesInfo.Uses[x].(*types.PkgName)
					if isIdent && isPkg {
						node, qualified = sel, true
					} else {
						return true // reached through a value
					}
				}
				pos := s.Fset.Position(node.Pos())
				key := pos.String()
				if seen[key] {
					return true
				}
				seen[key] = true
				out[used.Pos()] = append(out[used.Pos()], declRefs{
					pkg:       p,
					path:      pos.Filename,
					start:     pos.Offset,
					end:       s.Fset.Position(node.End()).Offset,
					qualified: qualified,
				})
				return true
			})
		}
	})
	return out
}

// setUses reports what the whole set depends on outside itself.
func (s *Snapshot) setUses(ms *moveSet) declUse {
	var u declUse
	seenSrc := make(map[string]bool)
	seenExt := make(map[string]bool)

	for _, d := range ms.decls {
		ast.Inspect(d.decl, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			obj := ms.pkg.TypesInfo.Uses[id]
			if obj == nil || obj.Pkg() == nil || ms.has(obj) {
				return true
			}
			switch {
			case obj.Pkg() == ms.pkg.Types:
				if !isPackageLevel(obj, ms.pkg) || seenSrc[obj.Name()] {
					return true
				}
				seenSrc[obj.Name()] = true
				if obj.Exported() {
					u.sourcePkg = append(u.sourcePkg, obj.Name())
				} else {
					u.unexported = append(u.unexported, obj.Name())
				}
			default:
				if p := obj.Pkg().Path(); !seenExt[p] {
					seenExt[p] = true
					u.external = append(u.external, p)
				}
			}
			return true
		})
	}
	slices.Sort(u.sourcePkg)
	slices.Sort(u.unexported)
	slices.Sort(u.external)
	return u
}
