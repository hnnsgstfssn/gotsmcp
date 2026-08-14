package sem

import (
	"fmt"
	"go/ast"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/rewrite"
)

// destination describes where a cross-package move is going.
type destination struct {
	dir        string
	file       string
	name       string // package name
	importPath string
	pkg        *packages.Package // nil when the package does not exist yet
	exists     bool
}

// resolveDest works out the package a destination file belongs to, deriving the
// import path from the source package rather than re-reading go.mod: the two
// directories share a module, so the relative path between them is also the
// relative path between their import paths.
func (s *Snapshot) resolveDest(srcPkg *packages.Package, srcPath, destAbs string) (destination, error) {
	d := destination{dir: filepath.Dir(destAbs), file: destAbs}

	s.unique(func(p *packages.Package) {
		if d.exists || len(p.CompiledGoFiles) == 0 {
			return
		}
		if filepath.Dir(p.CompiledGoFiles[0]) != d.dir {
			return
		}
		// Skip the synthetic external test package for the same directory.
		if strings.HasSuffix(p.Name, "_test") {
			return
		}
		d.pkg, d.name, d.importPath, d.exists = p, p.Name, cleanPath(p.PkgPath), true
	})
	if d.exists {
		return d, nil
	}

	rel, err := filepath.Rel(filepath.Dir(srcPath), d.dir)
	if err != nil {
		return d, fmt.Errorf("%w: cannot relate %s to the source package", ErrNotMovable, s.Rel(d.dir))
	}
	d.importPath = path.Clean(path.Join(cleanPath(srcPkg.PkgPath), filepath.ToSlash(rel)))
	d.name = packageNameFor(d.dir)
	if !isValidIdent(d.name) {
		return d, fmt.Errorf("%w: cannot derive a package name from directory %s", ErrNotMovable, filepath.Base(d.dir))
	}
	return d, nil
}

// renderNewFile writes a fresh file. Unused imports are dropped by goimports on
// apply, so listing more than strictly necessary is safe and guessing less so.
func renderNewFile(pkgName string, imports []string, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n", pkgName)
	if seen := dedupeStrings(imports); len(seen) > 0 {
		b.WriteString("\nimport (\n")
		for _, p := range seen {
			fmt.Fprintf(&b, "\t%q\n", p)
		}
		b.WriteString(")\n")
	}
	fmt.Fprintf(&b, "\n%s\n", body)
	return b.String()
}

func packageNameFor(dir string) string {
	base := filepath.Base(dir)
	base = strings.ReplaceAll(base, "-", "")
	return strings.ReplaceAll(base, ".", "")
}

func isValidIdent(name string) bool { return validIdent(name) == nil }

// declRefs is one reference to the symbol being moved.
type declRefs struct {
	pkg   *packages.Package
	path  string
	start int
	end   int
	// qualified marks a reference written as pkg.Foo, whose whole selector
	// expression is replaced rather than just the identifier.
	qualified bool
}

// requalify rewrites each reference for its new home and reports, per file, the
// import path that reference now needs.
func (s *Snapshot) requalify(refs []declRefs, srcPkg *packages.Package, dest destination, name string) ([]rewrite.Edit, map[string]string, error) {
	var edits []rewrite.Edit
	needImport := make(map[string]string) // file -> import path

	for _, r := range refs {
		refPkg := cleanPath(r.pkg.PkgPath)
		switch {
		case refPkg == dest.importPath:
			// Already inside the destination: drop the qualifier.
			if !r.qualified {
				continue // an unqualified use in the destination already works
			}
			edits = append(edits, rewrite.Edit{
				Path: r.path, Start: uint32(r.start), End: uint32(r.end),
				New: name, Note: "now local to " + dest.name,
			})
		case refPkg == cleanPath(srcPkg.PkgPath):
			// Was unqualified in the source package; now needs the new one.
			edits = append(edits, rewrite.Edit{
				Path: r.path, Start: uint32(r.start), End: uint32(r.end),
				New: dest.name + "." + name, Note: "requalified to " + dest.name,
			})
			needImport[r.path] = dest.importPath
		default:
			// Third-party reference: swap one qualifier for the other.
			edits = append(edits, rewrite.Edit{
				Path: r.path, Start: uint32(r.start), End: uint32(r.end),
				New: dest.name + "." + name, Note: "requalified to " + dest.name,
			})
			needImport[r.path] = dest.importPath
		}
	}
	return edits, needImport, nil
}

// importFixups adds the imports the rewrites created.
//
// Removal is left to goimports on apply, which drops an import that no longer
// has a use; addition is done explicitly because guessing which package a bare
// qualifier refers to is exactly the kind of ambiguity worth avoiding.
func (s *Snapshot) importFixups(needed map[string]string, dest destination, srcPkg *packages.Package, destNeedsSource bool) ([]rewrite.Edit, error) {
	var edits []rewrite.Edit
	for file, importPath := range needed {
		e, ok, err := s.addImport(file, importPath)
		if err != nil {
			return nil, err
		}
		if ok {
			edits = append(edits, e)
		}
	}
	if destNeedsSource && dest.exists {
		e, ok, err := s.addImport(dest.file, cleanPath(srcPkg.PkgPath))
		if err != nil {
			return nil, err
		}
		if ok {
			edits = append(edits, e)
		}
	}
	slices.SortFunc(edits, func(a, b rewrite.Edit) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return int(a.Start) - int(b.Start)
	})
	return edits, nil
}

// addImport produces an edit inserting an import into a file, if absent.
//
// A separate "import" statement is emitted rather than splicing into an
// existing block: several import declarations are legal Go, and goimports
// merges and sorts them on apply, which is more robust than trying to place a
// line inside a block whose formatting is unknown.
func (s *Snapshot) addImport(file, importPath string) (rewrite.Edit, bool, error) {
	pkg, f, err := s.astFile(file)
	if err != nil {
		return rewrite.Edit{}, false, nil // a file being created gets its imports from goimports
	}
	_ = pkg
	for _, spec := range f.Imports {
		if strings.Trim(spec.Path.Value, `"`) == importPath {
			return rewrite.Edit{}, false, nil
		}
	}
	// Insert immediately after the package clause.
	off := s.Fset.Position(f.Name.End()).Offset
	return rewrite.Edit{
		Path:  file,
		Start: uint32(off),
		End:   uint32(off),
		New:   fmt.Sprintf("\n\nimport %q", importPath),
		Note:  "import " + importPath,
	}, true, nil
}

// declUse records what the moved declaration depends on.
type declUse struct {
	sourcePkg  []string // exported symbols of the source package
	unexported []string // unexported symbols of the source package: fatal
	external   []string // import paths used from other packages
}

// checkCycle refuses moves that would make two packages import each other.
func (s *Snapshot) checkCycle(srcPkg *packages.Package, dest destination, destNeedsSource, sourceNeedsDest bool) error {
	switch {
	case destNeedsSource && sourceNeedsDest:
		return fmt.Errorf(
			"%w: the declaration uses symbols from %s and %s still references the declaration, "+
				"so the move would make the two packages import each other",
			ErrNotMovable, srcPkg.Name, srcPkg.Name)

	case sourceNeedsDest && dest.exists && importsTransitively(dest.pkg, cleanPath(srcPkg.PkgPath)):
		return fmt.Errorf(
			"%w: %s already imports %s, and %s would have to import %s to reach the moved declaration",
			ErrNotMovable, dest.importPath, cleanPath(srcPkg.PkgPath), cleanPath(srcPkg.PkgPath), dest.importPath)

	case destNeedsSource && dest.exists && importsTransitively(srcPkg, dest.importPath):
		return fmt.Errorf(
			"%w: %s already imports %s, and %s would have to import %s for the moved declaration",
			ErrNotMovable, cleanPath(srcPkg.PkgPath), dest.importPath, dest.importPath, cleanPath(srcPkg.PkgPath))
	}
	return nil
}

// importsTransitively reports whether from reaches target through imports.
func importsTransitively(from *packages.Package, target string) bool {
	if from == nil {
		return false
	}
	seen := make(map[string]bool)
	var walk func(p *packages.Package) bool
	walk = func(p *packages.Package) bool {
		for _, dep := range p.Imports {
			path := cleanPath(dep.PkgPath)
			if path == target {
				return true
			}
			if seen[path] {
				continue
			}
			seen[path] = true
			if walk(dep) {
				return true
			}
		}
		return false
	}
	return walk(from)
}

// declSpan returns the byte range to cut, including the trailing blank line the
// declaration would otherwise leave behind.
func (s *Snapshot) declSpan(decl ast.Decl, src []byte) (start, end int) {
	from := decl.Pos()
	if doc := docComment(decl); doc != nil {
		from = doc.Pos()
	}
	start = s.Fset.Position(from).Offset
	end = s.Fset.Position(decl.End()).Offset
	for end < len(src) && (src[end] == '\n' || src[end] == '\r') {
		end++
	}
	return start, end
}
