package sem

import (
	"fmt"
	"go/ast"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/rewrite"
)

// MoveDecls relocates a set of declarations, in the same package or a different
// one.
//
// Everything is judged on the aggregate. A declaration that uses an unexported
// symbol is fine as long as that symbol is moving too, which is the normal case
// when splitting a package and the case a per-declaration check gets wrong.
func (s *Snapshot) MoveDecls(req MoveRequest) (*Move, error) {
	ms, err := s.resolveMoveSet(req)
	if err != nil {
		return nil, err
	}
	pulled := s.expand(ms, req.IncludeDeps)

	destAbs := req.To
	if !filepath.IsAbs(destAbs) {
		destAbs = filepath.Join(s.root, destAbs)
	}
	destAbs = filepath.Clean(destAbs)

	// A directory destination keeps each source file's base name, which is
	// what makes "move these three files" behave the way it reads.
	perFile := !strings.HasSuffix(destAbs, ".go")
	destDir := destAbs
	if !perFile {
		destDir = filepath.Dir(destAbs)
	}

	srcDir := filepath.Dir(ms.decls[0].file)
	samePkg := destDir == srcDir

	probe := destAbs
	if perFile {
		probe = filepath.Join(destDir, filepath.Base(ms.decls[0].file))
	}
	dest, err := s.resolveDest(ms.pkg, ms.decls[0].file, probe)
	if err != nil {
		return nil, err
	}
	if samePkg {
		dest.name, dest.importPath, dest.exists, dest.pkg = ms.pkg.Name, cleanPath(ms.pkg.PkgPath), true, ms.pkg
	}

	m := &Move{
		Symbol:  s.Describe(ms.decls[0].objs[0]),
		From:    filepath.ToSlash(s.Rel(ms.decls[0].file)),
		To:      filepath.ToSlash(s.Rel(destAbs)),
		SamePkg: samePkg,
	}
	if len(ms.decls) > 1 {
		m.Warnings = append(m.Warnings,
			fmt.Sprintf("moving %d declarations: %s", len(ms.decls), strings.Join(ms.names(), ", ")))
	}
	if len(pulled) > 0 {
		m.Warnings = append(m.Warnings,
			fmt.Sprintf("pulled in as required dependencies: %s", strings.Join(pulled, ", ")))
	}

	if !samePkg {
		if err := s.checkSetMovable(ms, dest); err != nil {
			return nil, err
		}
		if err := s.validateSet(ms, dest, req.IncludeDeps); err != nil {
			return nil, err
		}
	}

	uses := s.setUses(ms)
	needsSource := !samePkg && len(uses.sourcePkg) > 0
	needsDest := false
	for _, d := range ms.decls {
		for _, obj := range d.objs {
			for _, r := range s.setRefSites(ms, obj) {
				if cleanPath(r.pkg.PkgPath) == cleanPath(ms.pkg.PkgPath) {
					needsDest = true
				}
			}
		}
	}
	if !samePkg {
		if err := s.checkCycle(ms.pkg, dest, needsSource, needsDest); err != nil {
			return nil, err
		}
	}

	// Cut each declaration and group the text by destination file.
	bodies := make(map[string][]string)
	for _, d := range ms.decls {
		srcBytes, err := s.Source(d.file)
		if err != nil {
			return nil, err
		}
		start, cutEnd := s.declSpan(d.decl, srcBytes)
		m.Edits = append(m.Edits, rewrite.Edit{
			Path:  d.file,
			Start: uint32(start),
			End:   uint32(cutEnd),
			Note:  "moved out of " + filepath.ToSlash(s.Rel(d.file)),
		})

		body, err := s.setBody(ms, d, samePkg)
		if err != nil {
			return nil, err
		}
		target := destAbs
		if perFile {
			target = filepath.Join(destDir, filepath.Base(d.file))
		}
		bodies[target] = append(bodies[target], body)
	}

	// Write each destination file, creating it when it does not exist.
	for _, target := range slices.Sorted(maps.Keys(bodies)) {
		text := strings.Join(bodies[target], "\n\n")
		if existing, err := s.Source(target); err == nil {
			m.Edits = append(m.Edits, rewrite.Edit{
				Path:  target,
				Start: uint32(len(existing)),
				End:   uint32(len(existing)),
				New:   "\n" + text + "\n",
				Note:  "moved into " + filepath.ToSlash(s.Rel(target)),
			})
			continue
		}
		var imports []string
		if needsSource {
			imports = append(imports, cleanPath(ms.pkg.PkgPath))
		}
		imports = append(imports, uses.external...)
		m.Edits = append(m.Edits, rewrite.Edit{
			Path: target,
			New:  renderNewFile(dest.name, imports, text),
			Note: "moved into a new " + filepath.ToSlash(s.Rel(target)),
		})
	}

	if samePkg {
		m.Warnings = append(m.Warnings,
			"imports are recalculated on apply; run go_check afterwards to confirm the result compiles")
		return m, nil
	}

	// Requalify every reference and add the imports that creates.
	needImport := make(map[string]string)
	for _, d := range ms.decls {
		for _, obj := range d.objs {
			refs := s.setRefSites(ms, obj)
			edits, added, err := s.requalify(refs, ms.pkg, dest, obj.Name())
			if err != nil {
				return nil, err
			}
			m.Edits = append(m.Edits, edits...)
			m.Requalified += len(refs)
			for f, p := range added {
				needImport[f] = p
			}
		}
	}
	importEdits, err := s.importFixups(needImport, dest, ms.pkg, needsSource && dest.exists)
	if err != nil {
		return nil, err
	}
	m.Edits = append(m.Edits, importEdits...)

	if needsSource {
		m.Warnings = append(m.Warnings, fmt.Sprintf(
			"the moved code still uses %s from %s, so %s now imports it",
			strings.Join(uses.sourcePkg, ", "), ms.pkg.Name, dest.importPath))
	}
	m.Warnings = append(m.Warnings,
		"imports are recalculated on apply; run go_check afterwards to confirm the result compiles")
	return m, nil
}

// setBody renders one declaration for its new home, qualifying references to
// what stayed behind. References to other moved declarations are left alone.
func (s *Snapshot) setBody(ms *moveSet, d movedDecl, samePkg bool) (string, error) {
	srcBytes, err := s.Source(d.file)
	if err != nil {
		return "", err
	}
	from := d.decl.Pos()
	if doc := docComment(d.decl); doc != nil {
		from = doc.Pos()
	}
	lo := s.Fset.Position(from).Offset
	hi := s.Fset.Position(d.decl.End()).Offset
	if samePkg {
		return strings.TrimRight(string(srcBytes[lo:hi]), " \t\n"), nil
	}

	var local []rewrite.Edit
	ast.Inspect(d.decl, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj := ms.pkg.TypesInfo.Uses[id]
		if !isPackageLevel(obj, ms.pkg) || !obj.Exported() || ms.has(obj) {
			return true
		}
		p := s.Fset.Position(id.Pos())
		if p.Offset < lo || p.Offset >= hi {
			return true
		}
		local = append(local, rewrite.Edit{
			Start: uint32(p.Offset - lo),
			End:   uint32(s.Fset.Position(id.End()).Offset - lo),
			New:   ms.pkg.Name + "." + id.Name,
		})
		return true
	})
	out, err := rewrite.Splice(srcBytes[lo:hi], local)
	if err != nil {
		return "", fmt.Errorf("qualify moved declaration: %w", err)
	}
	return strings.TrimRight(string(out), " \t\n"), nil
}

// checkSetMovable applies the per-declaration refusals that still hold for a set.
func (s *Snapshot) checkSetMovable(ms *moveSet, dest destination) error {
	if dest.exists && dest.pkg != nil && cleanPath(dest.pkg.PkgPath) == cleanPath(ms.pkg.PkgPath) {
		return fmt.Errorf("%w: the destination is the same package", ErrNotMovable)
	}
	// A method whose receiver stays behind cannot leave; expand already pulled
	// in the methods whose receivers are moving.
	for _, d := range ms.decls {
		fd, ok := d.decl.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
			continue
		}
		obj := ms.pkg.TypesInfo.Defs[fd.Name]
		named := receiverNamed(obj)
		if named == nil || ms.has(named.Obj()) {
			continue
		}
		return fmt.Errorf(
			"%w: %s is a method on %s, which is not moving. A method must live in the package declaring its receiver, "+
				"so move %s too or leave the method behind",
			ErrNotMovable, fd.Name.Name, named.Obj().Name(), named.Obj().Name())
	}
	return nil
}
