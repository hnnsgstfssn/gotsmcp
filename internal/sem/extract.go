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

	"github.com/hnnsgstfssn/treesitter-mcp/internal/rewrite"
)

// ErrNotExtractable reports a statement range that cannot become a function.
const ErrNotExtractable Error = "range cannot be extracted"

// Extract is a computed extraction.
type Extract struct {
	Name      string         `json:"name"`
	File      string         `json:"file"`
	FromLine  int            `json:"from_line"`
	ToLine    int            `json:"to_line"`
	Signature string         `json:"signature"`
	Params    []string       `json:"params"`
	Results   []string       `json:"results"`
	Edits     []rewrite.Edit `json:"-"`
	Warnings  []string       `json:"warnings,omitempty"`
}

// ExtractFunc turns an inclusive range of statements into a new function.
//
// Parameters are the variables the range reads but does not declare; results
// are the ones it assigns that are still used afterwards. Both come from the
// type checker's definition and use records, which is the only way to get them
// right: a textual guess would miss a variable read through a closure and would
// treat a shadowed name as the outer one.
func (s *Snapshot) ExtractFunc(file string, fromLine, toLine int, name string) (*Extract, error) {
	if err := validIdent(name); err != nil {
		return nil, err
	}
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.root, path)
	}
	path = filepath.Clean(path)

	pkg, astFile, err := s.astFile(path)
	if err != nil {
		return nil, err
	}
	fn, stmts, err := s.statementsIn(astFile, path, fromLine, toLine)
	if err != nil {
		return nil, err
	}
	if err := checkControlFlow(stmts); err != nil {
		return nil, err
	}

	first, last := stmts[0], stmts[len(stmts)-1]
	lo := s.Fset.Position(first.Pos())
	hi := s.Fset.Position(last.End())

	params, results := s.dataFlow(pkg, fn, stmts, first.Pos(), last.End())

	// Build the call and the function.
	paramNames := make([]string, len(params))
	paramDecls := make([]string, len(params))
	for i, v := range params {
		paramNames[i] = v.Name()
		paramDecls[i] = v.Name() + " " + types.TypeString(v.Type(), s.qualifier(pkg))
	}
	resultNames := make([]string, len(results))
	resultTypes := make([]string, len(results))
	for i, v := range results {
		resultNames[i] = v.Name()
		resultTypes[i] = types.TypeString(v.Type(), s.qualifier(pkg))
	}

	sig := fmt.Sprintf("func %s(%s)", name, strings.Join(paramDecls, ", "))
	switch len(resultTypes) {
	case 0:
	case 1:
		sig += " " + resultTypes[0]
	default:
		sig += " (" + strings.Join(resultTypes, ", ") + ")"
	}

	srcBytes, err := s.Source(path)
	if err != nil {
		return nil, err
	}
	bodyText := string(srcBytes[lo.Offset:hi.Offset])

	var fnBody strings.Builder
	fnBody.WriteString(sig)
	fnBody.WriteString(" {\n")
	fnBody.WriteString(bodyText)
	if len(resultNames) > 0 {
		fnBody.WriteString("\n\treturn " + strings.Join(resultNames, ", "))
	}
	fnBody.WriteString("\n}\n")

	call := fmt.Sprintf("%s(%s)", name, strings.Join(paramNames, ", "))
	if len(resultNames) > 0 {
		call = strings.Join(resultNames, ", ") + " = " + call
		if allDeclaredInRange(results, stmts, s) {
			call = strings.Join(resultNames, ", ") + " := " + name + "(" + strings.Join(paramNames, ", ") + ")"
		}
	}

	ex := &Extract{
		Name:      name,
		File:      filepath.ToSlash(s.Rel(path)),
		FromLine:  lo.Line,
		ToLine:    s.Fset.Position(last.End()).Line,
		Signature: sig,
		Params:    paramDecls,
		Results:   resultTypes,
	}
	ex.Edits = []rewrite.Edit{
		{
			Path:  path,
			Start: uint32(lo.Offset),
			End:   uint32(hi.Offset),
			New:   call,
			Note:  "replaced with a call to " + name,
		},
		{
			// Insert after the enclosing function so the new declaration is a
			// sibling rather than nested.
			Path:  path,
			Start: uint32(s.Fset.Position(fn.End()).Offset),
			End:   uint32(s.Fset.Position(fn.End()).Offset),
			New:   "\n\n" + fnBody.String(),
			Note:  "new function " + name,
		},
	}
	if len(results) > 0 {
		ex.Warnings = append(ex.Warnings,
			"the extracted function returns assigned variables; check the call site reads them as you intend")
	}
	return ex, nil
}

// qualifier renders types relative to the package being edited, so a local type
// is written T rather than pkg.T.
func (s *Snapshot) qualifier(pkg *packages.Package) types.Qualifier {
	return func(other *types.Package) string {
		if other == pkg.Types {
			return ""
		}
		return other.Name()
	}
}

// dataFlow computes the parameters and results of an extracted range.
func (s *Snapshot) dataFlow(pkg *packages.Package, fn *ast.FuncDecl, stmts []ast.Stmt, _, to token.Pos) (params, results []*types.Var) {
	declaredInside := make(map[types.Object]bool)
	readInside := make(map[types.Object]bool)
	writtenInside := make(map[types.Object]bool)

	for _, st := range stmts {
		ast.Inspect(st, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if obj := pkg.TypesInfo.Defs[id]; obj != nil {
				declaredInside[obj] = true
				return true
			}
			if obj := pkg.TypesInfo.Uses[id]; obj != nil {
				readInside[obj] = true
			}
			return true
		})
		// Assignment targets count as writes even when also reads.
		ast.Inspect(st, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range as.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					if obj := pkg.TypesInfo.Uses[id]; obj != nil {
						writtenInside[obj] = true
					}
					if obj := pkg.TypesInfo.Defs[id]; obj != nil {
						writtenInside[obj] = true
					}
				}
			}
			return true
		})
	}

	// Parameters: read inside, declared outside the range but inside the
	// function. Package-level objects need no parameter.
	for obj := range readInside {
		v, ok := obj.(*types.Var)
		if !ok || declaredInside[obj] {
			continue
		}
		if !isLocalTo(v, fn) {
			continue
		}
		params = append(params, v)
	}

	// Results: written inside and still used after the range.
	usedAfter := make(map[types.Object]bool)
	ast.Inspect(fn, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Pos() < to {
			return true
		}
		if obj := pkg.TypesInfo.Uses[id]; obj != nil {
			usedAfter[obj] = true
		}
		return true
	})
	for obj := range writtenInside {
		v, ok := obj.(*types.Var)
		if !ok || !usedAfter[obj] || !isLocalTo(v, fn) {
			continue
		}
		results = append(results, v)
	}

	byName := func(a, b *types.Var) int { return strings.Compare(a.Name(), b.Name()) }
	slices.SortFunc(params, byName)
	slices.SortFunc(results, byName)
	return params, results
}

// isLocalTo reports whether v is declared inside fn rather than at package level.
func isLocalTo(v *types.Var, fn *ast.FuncDecl) bool {
	return v.Pos() >= fn.Pos() && v.Pos() < fn.End()
}

func allDeclaredInRange(results []*types.Var, stmts []ast.Stmt, s *Snapshot) bool {
	if len(stmts) == 0 {
		return false
	}
	from, to := stmts[0].Pos(), stmts[len(stmts)-1].End()
	for _, v := range results {
		if v.Pos() < from || v.Pos() >= to {
			return false
		}
	}
	return true
}

// statementsIn finds the top-level statements of a function body that fall
// inside a line range.
func (s *Snapshot) statementsIn(f *ast.File, path string, fromLine, toLine int) (*ast.FuncDecl, []ast.Stmt, error) {
	if fromLine < 1 || toLine < fromLine {
		return nil, nil, fmt.Errorf("%w: invalid line range %d-%d", ErrNotExtractable, fromLine, toLine)
	}
	var (
		owner *ast.FuncDecl
		found []ast.Stmt
	)
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		start := s.Fset.Position(fn.Pos()).Line
		end := s.Fset.Position(fn.End()).Line
		if fromLine < start || toLine > end {
			continue
		}
		owner = fn
		for _, st := range fn.Body.List {
			l0 := s.Fset.Position(st.Pos()).Line
			l1 := s.Fset.Position(st.End()).Line
			if l0 >= fromLine && l1 <= toLine {
				found = append(found, st)
			}
		}
		break
	}
	switch {
	case owner == nil:
		return nil, nil, fmt.Errorf("%w: lines %d-%d are not inside a single function body",
			ErrNotExtractable, fromLine, toLine)
	case len(found) == 0:
		return nil, nil, fmt.Errorf(
			"%w: lines %d-%d do not cover whole statements at the top level of %s; "+
				"extend the range to complete statements",
			ErrNotExtractable, fromLine, toLine, owner.Name.Name)
	}
	return owner, found, nil
}

// checkControlFlow refuses ranges whose meaning would change once moved into
// another function.
func checkControlFlow(stmts []ast.Stmt) error {
	var bad string
	for _, st := range stmts {
		ast.Inspect(st, func(n ast.Node) bool {
			if bad != "" {
				return false
			}
			switch x := n.(type) {
			case *ast.ReturnStmt:
				bad = "return"
			case *ast.BranchStmt:
				// break and continue are fine if their loop is inside the
				// range; only a jump out of it is a problem.
				if x.Label != nil {
					bad = x.Tok.String() + " with a label"
				}
			case *ast.DeferStmt:
				bad = "defer, which would fire when the extracted function returns rather than the original"
			case *ast.FuncLit:
				return false // a nested function's returns belong to it
			}
			return true
		})
		if bad != "" {
			break
		}
	}
	if bad != "" {
		return fmt.Errorf("%w: the range contains %s, which changes meaning in a new function", ErrNotExtractable, bad)
	}
	// A bare break or continue whose loop is outside the range is equally bad.
	for _, st := range stmts {
		if err := checkLooseBranches(st); err != nil {
			return err
		}
	}
	return nil
}

// checkLooseBranches finds break or continue statements not enclosed by their
// own loop or switch within the extracted range.
func checkLooseBranches(root ast.Stmt) error {
	var err error
	var walk func(n ast.Node, depth int)
	walk = func(n ast.Node, depth int) {
		if n == nil || err != nil {
			return
		}
		switch x := n.(type) {
		case *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			for _, c := range children(x) {
				walk(c, depth+1)
			}
			return
		case *ast.BranchStmt:
			if depth == 0 && (x.Tok == token.BREAK || x.Tok == token.CONTINUE) {
				err = fmt.Errorf("%w: %s refers to a loop outside the range",
					ErrNotExtractable, x.Tok)
			}
		case *ast.FuncLit:
			return
		}
		for _, c := range children(n) {
			walk(c, depth)
		}
	}
	walk(root, 0)
	return err
}

// children lists the direct child nodes of n.
func children(n ast.Node) []ast.Node {
	var out []ast.Node
	ast.Inspect(n, func(c ast.Node) bool {
		if c == nil || c == n {
			return c == n
		}
		out = append(out, c)
		return false
	})
	return out
}

// astFile locates the parsed file for a path.
func (s *Snapshot) astFile(path string) (*packages.Package, *ast.File, error) {
	var (
		outP *packages.Package
		outF *ast.File
	)
	s.unique(func(p *packages.Package) {
		if outF != nil {
			return
		}
		for _, f := range p.Syntax {
			if filepath.Clean(s.Fset.Position(f.Pos()).Filename) == path {
				outP, outF = p, f
				return
			}
		}
	})
	if outF == nil {
		return nil, nil, fmt.Errorf("%w: %s is not a type-checked Go file", ErrNotFound, s.Rel(path))
	}
	return outP, outF, nil
}
