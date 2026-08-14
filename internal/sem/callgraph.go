package sem

import (
	"go/ast"
	"go/types"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// TestHit is one test function that reaches a symbol.
type TestHit struct {
	Test    string `json:"test"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Package string `json:"package,omitempty"`
	// Chain is the call path from the test down to the symbol, which is what
	// makes a long list actionable.
	Chain []string `json:"chain"`
	Depth int      `json:"depth"`
}

// TestsResult reports which tests exercise a symbol.
type TestsResult struct {
	Symbol  Symbol    `json:"symbol"`
	Tests   []TestHit `json:"tests"`
	Total   int       `json:"total"`
	Direct  int       `json:"direct"`
	Scanned int       `json:"functions_scanned"`
	Note    string    `json:"note"`
}

// caller is an edge in the static call graph: who calls, from where.
type caller struct {
	fn   types.Object
	name string
	file string
	line int
	pkg  string
	test bool
}

// TestsFor finds the test functions whose call graph reaches obj.
//
// The graph is static: an edge exists where the source names the callee
// directly. Calls made through an interface, a function value, or reflection
// are invisible, so this under-reports and must not be read as coverage. It is
// a starting point for "what should I run after changing this", not a proof
// that nothing else exercises it.
func (s *Snapshot) TestsFor(obj types.Object, maxDepth int) *TestsResult {
	if maxDepth <= 0 {
		maxDepth = 6
	}
	callers, scanned := s.callerGraph()

	res := &TestsResult{
		Symbol:  s.Describe(obj),
		Scanned: scanned,
		Note:    "static call graph: calls through interfaces, function values, or reflection are not followed, so this under-reports",
	}

	// Breadth-first up the graph, remembering how each function was reached so
	// the chain can be rebuilt.
	type node struct {
		obj   types.Object
		depth int
	}
	via := map[types.Object]caller{}
	seen := map[types.Object]bool{obj: true}
	queue := []node{{obj: obj}}
	seenTest := map[string]bool{}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= maxDepth {
			continue
		}
		for _, c := range callers[keyOf(cur.obj)] {
			if c.fn == nil || seen[c.fn] {
				continue
			}
			seen[c.fn] = true
			via[c.fn] = caller{fn: cur.obj, name: nameOf(cur.obj)}

			if c.test {
				key := c.file + ":" + c.name
				if seenTest[key] {
					continue
				}
				seenTest[key] = true
				hit := TestHit{
					Test: c.name, File: c.file, Line: c.line,
					Package: c.pkg, Depth: cur.depth + 1,
					Chain: chainFrom(c.name, c.fn, via, obj),
				}
				if cur.depth == 0 {
					res.Direct++
				}
				res.Tests = append(res.Tests, hit)
				continue
			}
			queue = append(queue, node{obj: c.fn, depth: cur.depth + 1})
		}
	}

	slices.SortFunc(res.Tests, func(a, b TestHit) int {
		if a.Depth != b.Depth {
			return a.Depth - b.Depth
		}
		return strings.Compare(a.Test, b.Test)
	})
	res.Total = len(res.Tests)
	return res
}

// chainFrom rebuilds the call path from a test down to the target.
func chainFrom(test string, from types.Object, via map[types.Object]caller, target types.Object) []string {
	chain := []string{test}
	cur := from
	for range 16 {
		chain = append(chain, nameOf(cur))
		if cur == target {
			break
		}
		next, ok := via[cur]
		if !ok || next.fn == nil {
			break
		}
		cur = next.fn
	}
	return chain
}

// Untested is one exported declaration no test reaches.
type Untested struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Package string `json:"package"`
	File    string `json:"file"`
	Line    int    `json:"line"`
}

// UntestedResult reports the exported API no test exercises.
type UntestedResult struct {
	Untested  []Untested `json:"untested"`
	Total     int        `json:"total"`
	Exported  int        `json:"exported_total"`
	Reached   int        `json:"reached_by_tests"`
	Truncated bool       `json:"truncated"`
	Note      string     `json:"note"`
}

// UntestedAPI finds exported functions and methods that no test reaches.
//
// It walks forward from every Test function once, rather than searching
// backwards from each symbol, because on a large module the exported surface is
// thousands of declarations and the backward search would repeat the same
// traversal for each.
//
// Same caveat as TestsFor, and it matters more here: the graph is static, so a
// function only ever called through an interface looks untested. Treat the
// output as a list to review, not a verdict.
func (s *Snapshot) UntestedAPI(maxResults int) *UntestedResult {
	if maxResults <= 0 {
		maxResults = 200
	}
	_, callees, _ := s.graphs()

	// Seed with every test and walk down.
	reached := make(map[string]bool)
	queue := s.testRoots(callees)
	for _, k := range queue {
		reached[k] = true
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range callees[cur] {
			if reached[next] {
				continue
			}
			reached[next] = true
			queue = append(queue, next)
		}
	}

	res := &UntestedResult{
		Note: "static call graph: a function reached only through an interface or a function value looks untested",
	}
	s.unique(func(p *packages.Package) {
		for _, f := range p.Syntax {
			path := s.Fset.Position(f.Pos()).Filename
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Name == nil || !fd.Name.IsExported() {
					continue
				}
				obj := p.TypesInfo.Defs[fd.Name]
				if obj == nil {
					continue
				}
				res.Exported++
				if reached[keyOf(obj)] {
					res.Reached++
					continue
				}
				res.Total++
				if len(res.Untested) >= maxResults {
					res.Truncated = true
					continue
				}
				kind := "func"
				if fd.Recv != nil {
					kind = "method"
				}
				pos := s.Fset.Position(fd.Pos())
				res.Untested = append(res.Untested, Untested{
					Name:    declName(fd),
					Kind:    kind,
					Package: cleanPath(p.PkgPath),
					File:    filepath.ToSlash(s.Rel(path)),
					Line:    pos.Line,
				})
			}
		}
	})
	slices.SortFunc(res.Untested, func(a, b Untested) int {
		if c := strings.Compare(a.Package, b.Package); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return res
}

// testRoots lists the graph keys of every Test function.
func (s *Snapshot) testRoots(callees map[string][]string) []string {
	var out []string
	s.unique(func(p *packages.Package) {
		for _, f := range p.Syntax {
			if !strings.HasSuffix(s.Fset.Position(f.Pos()).Filename, "_test.go") {
				continue
			}
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Recv != nil || fd.Name == nil {
					continue
				}
				if !strings.HasPrefix(fd.Name.Name, "Test") && !strings.HasPrefix(fd.Name.Name, "Benchmark") &&
					!strings.HasPrefix(fd.Name.Name, "Fuzz") && !strings.HasPrefix(fd.Name.Name, "Example") {
					continue
				}
				if obj := p.TypesInfo.Defs[fd.Name]; obj != nil {
					out = append(out, keyOf(obj))
				}
			}
		}
	})
	return out
}

// graphs builds both directions of the call graph in one traversal.
func (s *Snapshot) graphs() (callers map[string][]caller, callees map[string][]string, scanned int) {
	callers = make(map[string][]caller)
	callees = make(map[string][]string)
	scanned = 0

	s.unique(func(p *packages.Package) {
		for _, f := range p.Syntax {
			path := s.Fset.Position(f.Pos()).Filename
			isTestFile := strings.HasSuffix(path, "_test.go")
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				scanned++
				owner := p.TypesInfo.Defs[fd.Name]
				pos := s.Fset.Position(fd.Pos())
				c := caller{
					fn:   owner,
					name: declName(fd),
					file: filepath.ToSlash(s.Rel(path)),
					line: pos.Line,
					pkg:  cleanPath(p.PkgPath),
					test: isTestFile && strings.HasPrefix(fd.Name.Name, "Test") && fd.Recv == nil,
				}
				from := keyOf(owner)
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					id := calleeIdent(call.Fun)
					if id == nil {
						return true
					}
					callee := p.TypesInfo.Uses[id]
					if callee == nil {
						return true
					}
					if _, isFunc := callee.(*types.Func); !isFunc {
						return true
					}
					to := keyOf(callee)
					callers[to] = append(callers[to], c)
					callees[from] = append(callees[from], to)
					return true
				})
			}
		}
	})
	return callers, callees, scanned
}

// callerGraph is the reverse direction alone.
func (s *Snapshot) callerGraph() (map[string][]caller, int) {
	callers, _, scanned := s.graphs()
	return callers, scanned
}

// keyOf identifies an object across package variants by declaration position,
// the same way targets does.
func keyOf(obj types.Object) string {
	if obj == nil || !obj.Pos().IsValid() {
		return ""
	}
	return obj.Name() + "@" + strconv.Itoa(int(obj.Pos()))
}

func nameOf(obj types.Object) string {
	if obj == nil {
		return "?"
	}
	if fn, ok := obj.(*types.Func); ok {
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
			if named := namedOf(sig.Recv().Type()); named != nil {
				return named.Obj().Name() + "." + fn.Name()
			}
		}
	}
	return obj.Name()
}
