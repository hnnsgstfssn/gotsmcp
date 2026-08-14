package sem

import (
	"fmt"
	"go/ast"
	"go/types"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/rewrite"
)

// ErrBadSignature reports a signature change that cannot be applied safely.
const ErrBadSignature Error = "signature change is not applicable"

// ParamChange describes one parameter of the new signature.
//
// An entry either carries over an existing parameter by index, introduces a new
// one with a value to pass at existing call sites, or is a rest entry standing
// for whatever the first two do not claim.
type ParamChange struct {
	// From is the zero-based index of the parameter to carry over. Absent, or
	// negative, means a parameter being introduced. It is a pointer because
	// zero is a meaningful index, so it cannot double as "unset".
	From  *int   `json:"from,omitempty"`
	Name  string `json:"name,omitempty"`
	Type  string `json:"type,omitempty"`
	Value string `json:"value,omitempty"`
	// Rest stands for every original parameter not named elsewhere, in order.
	//
	// Without it a change cannot be expressed once and applied to functions of
	// differing arity, which is exactly what threading a context through a
	// call chain requires.
	Rest bool `json:"rest,omitempty"`
}

// carries reports the original parameter this entry keeps, if any.
func (p ParamChange) carries() (int, bool) {
	if p.From == nil || *p.From < 0 {
		return 0, false
	}
	return *p.From, true
}

// Signature is a computed signature change.
type Signature struct {
	Symbol    Symbol         `json:"symbol"`
	Old       string         `json:"old"`
	New       string         `json:"new"`
	CallSites int            `json:"call_sites"`
	Edits     []rewrite.Edit `json:"-"`
	Conflicts []Conflict     `json:"conflicts,omitempty"`
	Warnings  []string       `json:"warnings,omitempty"`
}

// ChangeSignature rewrites a function's parameter list and every call site.
//
// The parameter list is given as a permutation of the old one: each entry
// either carries over an existing parameter by index or introduces a new one
// with a value to pass at existing calls. Expressing it that way rather than as
// free text is what makes the call-site rewrite mechanical, because every
// argument's destination is known.
//
// Variadic parameters, interface methods, and functions used as values are
// refused: each needs argument handling this cannot verify.
// ChangeSignatures applies one parameter transformation to several functions.
//
// The rest marker makes this possible: each function expands it against its own
// parameter list, so "put a context first and keep everything else" is one
// request rather than one per arity.
func (s *Snapshot) ChangeSignatures(objs []types.Object, params []ParamChange) (*Signature, error) {
	if len(objs) == 1 {
		return s.ChangeSignature(objs[0], params)
	}
	merged := &Signature{}
	var names []string
	for _, obj := range objs {
		one, err := s.ChangeSignature(obj, params)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", obj.Name(), err)
		}
		names = append(names, one.Symbol.Name)
		merged.Edits = append(merged.Edits, one.Edits...)
		merged.Conflicts = append(merged.Conflicts, one.Conflicts...)
		merged.Warnings = append(merged.Warnings, one.Warnings...)
		merged.CallSites += one.CallSites
		merged.Symbol = one.Symbol
	}
	merged.Edits = dedupeEdits(merged.Edits)
	if err := checkNoNestedEdits(merged.Edits); err != nil {
		return nil, err
	}
	merged.Old = fmt.Sprintf("%d functions", len(objs))
	merged.New = strings.Join(names, ", ")
	merged.Warnings = dedupeStrings(merged.Warnings)
	return merged, nil
}

// checkNoNestedEdits refuses a batch whose call-site rewrites overlap.
//
// When f(g(x)) has both f and g changing, the outer rewrite copies the argument
// text verbatim, which still contains the old inner call, and the inner rewrite
// lands inside the range the outer one replaces. Splicing would either corrupt
// the call or fail with an opaque overlap error, so say so precisely instead.
func checkNoNestedEdits(edits []rewrite.Edit) error {
	for i := 1; i < len(edits); i++ {
		prev, cur := edits[i-1], edits[i]
		if prev.Path != cur.Path || cur.Start >= prev.End {
			continue
		}
		return fmt.Errorf(
			"%w: two changing calls are nested at %s (byte %d is inside %d-%d). "+
				"Change them in separate operations, innermost first",
			ErrBadSignature, prev.Path, cur.Start, prev.Start, prev.End)
	}
	return nil
}

func (s *Snapshot) ChangeSignature(obj types.Object, params []ParamChange) (*Signature, error) {
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil, fmt.Errorf("%w: %s is not a function", ErrBadSignature, obj.Name())
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return nil, fmt.Errorf("%w: %s has no signature", ErrBadSignature, obj.Name())
	}
	if sig.Variadic() {
		return nil, fmt.Errorf("%w: %s is variadic; the trailing parameter's arguments cannot be repositioned reliably",
			ErrBadSignature, fn.Name())
	}
	if sig.Recv() != nil && types.IsInterface(sig.Recv().Type()) {
		return nil, fmt.Errorf("%w: %s is an interface method; changing it silently breaks every implementation",
			ErrBadSignature, fn.Name())
	}

	old := sig.Params()
	params, err := expandRest(params, old.Len())
	if err != nil {
		return nil, err
	}
	for i, p := range params {
		idx, carries := p.carries()
		switch {
		case carries && idx >= old.Len():
			return nil, fmt.Errorf("%w: entry %d refers to parameter %d but the signature has %d",
				ErrBadSignature, i, idx, old.Len())
		case !carries && strings.TrimSpace(p.Type) == "":
			return nil, fmt.Errorf("%w: entry %d introduces a parameter but gives no type", ErrBadSignature, i)
		case !carries && strings.TrimSpace(p.Value) == "":
			return nil, fmt.Errorf("%w: entry %d introduces parameter %q but gives no value to pass at existing call sites",
				ErrBadSignature, i, p.Name)
		}
	}

	res := &Signature{Symbol: s.Describe(obj), CallSites: 0}
	res.Old = types.TypeString(sig, s.pkgQualifier())

	decl, err := s.funcDecl(fn)
	if err != nil {
		return nil, err
	}
	if decl.Body == nil {
		return nil, fmt.Errorf("%w: %s has no body in this module", ErrBadSignature, fn.Name())
	}

	// Interface satisfaction depends on the method set, so a method whose
	// signature changes can silently stop implementing something.
	if sig.Recv() != nil {
		res.Conflicts = append(res.Conflicts, s.interfaceConflicts(obj, fn.Name())...)
	}

	// Rewrite the declaration's parameter list.
	newList, err := s.renderParams(decl, params)
	if err != nil {
		return nil, err
	}
	declStart := s.Fset.Position(decl.Type.Params.Pos()).Offset
	declEnd := s.Fset.Position(decl.Type.Params.End()).Offset
	declPath := s.Fset.Position(decl.Pos()).Filename
	if _, err := s.Source(declPath); err != nil {
		return nil, err
	}
	res.Edits = append(res.Edits, rewrite.Edit{
		Path:  declPath,
		Start: uint32(declStart),
		End:   uint32(declEnd),
		New:   newList,
		Note:  "new parameter list",
	})
	res.New = fn.Name() + newList

	// Rewrite every call site.
	targets := s.targetSet(obj)
	var valueUse string
	s.unique(func(p *packages.Package) {
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					// A reference that is not a call means the function is
					// used as a value, and its type is about to change.
					if id, ok := n.(*ast.Ident); ok && valueUse == "" {
						if _, hit := targets.lookup(p.TypesInfo.Uses[id]); hit && !isCallee(f, id) {
							valueUse = s.posString(id.Pos())
						}
					}
					return true
				}
				id := calleeIdent(call.Fun)
				if id == nil {
					return true
				}
				if _, hit := targets.lookup(p.TypesInfo.Uses[id]); !hit {
					return true
				}
				edit, err := s.rewriteCall(call, params)
				if err != nil {
					res.Warnings = append(res.Warnings, err.Error())
					return true
				}
				res.CallSites++
				res.Edits = append(res.Edits, edit)
				return true
			})
		}
	})
	if valueUse != "" {
		res.Conflicts = append(res.Conflicts, Conflict{
			Kind:     "value-use",
			Message:  fmt.Sprintf("%s is used as a value, not called; its type changes and that use will not compile", fn.Name()),
			Position: valueUse,
		})
	}

	// Deduplicate: a file compiled into several package variants is visited more
	// than once, and two edits over the same bytes would be refused as overlap.
	res.Edits = dedupeEdits(res.Edits)
	res.Warnings = append(res.Warnings,
		"call sites in build-constrained files are not rewritten; run go_check afterwards")
	return res, nil
}

// expandRest replaces a rest entry with the original parameters that no other
// entry claims, in their original order.
func expandRest(params []ParamChange, arity int) ([]ParamChange, error) {
	at := -1
	claimed := make(map[int]bool)
	for i, p := range params {
		idx, carries := p.carries()
		switch {
		case p.Rest && at >= 0:
			return nil, fmt.Errorf("%w: only one rest entry is allowed", ErrBadSignature)
		case p.Rest:
			at = i
		case carries:
			claimed[idx] = true
		}
	}
	if at < 0 {
		return params, nil
	}
	var fill []ParamChange
	for i := range arity {
		if !claimed[i] {
			fill = append(fill, ParamChange{From: &[]int{i}[0]})
		}
	}
	out := slices.Clone(params[:at])
	out = append(out, fill...)
	return append(out, params[at+1:]...), nil
}

// renderParams builds the new parameter list text.
func (s *Snapshot) renderParams(decl *ast.FuncDecl, params []ParamChange) (string, error) {
	oldFields := flattenParams(decl.Type.Params)
	parts := make([]string, 0, len(params))
	for i, p := range params {
		if idx, carries := p.carries(); carries {
			if idx >= len(oldFields) {
				return "", fmt.Errorf("%w: entry %d refers to parameter %d, which does not exist",
					ErrBadSignature, i, idx)
			}
			f := oldFields[idx]
			name := cmpOrString(p.Name, f.name)
			typ := cmpOrString(p.Type, f.typ)
			parts = append(parts, strings.TrimSpace(name+" "+typ))
			continue
		}
		parts = append(parts, strings.TrimSpace(p.Name+" "+p.Type))
	}
	return "(" + strings.Join(parts, ", ") + ")", nil
}

type paramField struct{ name, typ string }

// flattenParams expands grouped parameters, so "a, b int" becomes two entries
// and indices line up with call arguments.
func flattenParams(fl *ast.FieldList) []paramField {
	var out []paramField
	if fl == nil {
		return out
	}
	for _, f := range fl.List {
		typ := exprString(f.Type)
		if len(f.Names) == 0 {
			out = append(out, paramField{typ: typ})
			continue
		}
		for _, n := range f.Names {
			out = append(out, paramField{name: n.Name, typ: typ})
		}
	}
	return out
}

// rewriteCall reorders one call's arguments to match the new parameter list.
func (s *Snapshot) rewriteCall(call *ast.CallExpr, params []ParamChange) (rewrite.Edit, error) {
	pos := s.Fset.Position(call.Lparen)
	path := pos.Filename
	src, err := s.Source(path)
	if err != nil {
		return rewrite.Edit{}, err
	}
	if call.Ellipsis.IsValid() {
		return rewrite.Edit{}, fmt.Errorf("%s: call spreads a slice with ... and was left alone", s.posString(call.Pos()))
	}

	args := make([]string, 0, len(params))
	for _, p := range params {
		idx, carries := p.carries()
		if !carries {
			args = append(args, p.Value)
			continue
		}
		if idx >= len(call.Args) {
			return rewrite.Edit{}, fmt.Errorf("%s: call passes %d arguments but the change refers to argument %d",
				s.posString(call.Pos()), len(call.Args), idx)
		}
		a := call.Args[idx]
		lo := s.Fset.Position(a.Pos()).Offset
		hi := s.Fset.Position(a.End()).Offset
		args = append(args, string(src[lo:hi]))
	}

	lo := s.Fset.Position(call.Lparen).Offset
	hi := s.Fset.Position(call.Rparen).Offset + 1
	return rewrite.Edit{
		Path:  path,
		Start: uint32(lo),
		End:   uint32(hi),
		New:   "(" + strings.Join(args, ", ") + ")",
		Note:  "call site",
	}, nil
}

// funcDecl finds the declaration for a function object.
func (s *Snapshot) funcDecl(fn *types.Func) (*ast.FuncDecl, error) {
	var out *ast.FuncDecl
	s.unique(func(p *packages.Package) {
		if out != nil {
			return
		}
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Name == nil {
					continue
				}
				if fd.Name.Pos() == fn.Pos() {
					out = fd
					return
				}
			}
		}
	})
	if out == nil {
		return nil, fmt.Errorf("%w: no declaration found for %s", ErrBadSignature, fn.Name())
	}
	return out, nil
}

// isCallee reports whether id appears as the function of a call.
func isCallee(f *ast.File, id *ast.Ident) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok && calleeIdent(call.Fun) == id {
			found = true
			return false
		}
		return true
	})
	return found
}

func dedupeEdits(edits []rewrite.Edit) []rewrite.Edit {
	seen := make(map[string]bool, len(edits))
	out := edits[:0]
	for _, e := range edits {
		key := e.Path + ":" + strconv.Itoa(int(e.Start)) + ":" + strconv.Itoa(int(e.End))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b rewrite.Edit) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return int(a.Start) - int(b.Start)
	})
	return out
}

// exprString renders a type expression back to source form.
func exprString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + exprString(t.Elt)
		}
		return "[" + exprString(t.Len) + "]" + exprString(t.Elt)
	case *ast.MapType:
		return "map[" + exprString(t.Key) + "]" + exprString(t.Value)
	case *ast.Ellipsis:
		return "..." + exprString(t.Elt)
	case *ast.BasicLit:
		return t.Value
	case *ast.InterfaceType:
		if t.Methods == nil || len(t.Methods.List) == 0 {
			return "any"
		}
		return "interface{...}"
	case *ast.FuncType:
		return "func(...)"
	case *ast.ChanType:
		return "chan " + exprString(t.Value)
	case *ast.IndexExpr:
		return exprString(t.X) + "[" + exprString(t.Index) + "]"
	}
	return "any"
}

func (s *Snapshot) pkgQualifier() types.Qualifier {
	return func(p *types.Package) string { return p.Name() }
}

func cmpOrString(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// SignatureSources returns the file contents a signature change touches.
func (s *Snapshot) SignatureSources(sig *Signature) map[string][]byte {
	out := make(map[string][]byte)
	for _, e := range sig.Edits {
		if _, ok := out[e.Path]; ok {
			continue
		}
		if b, err := s.Source(e.Path); err == nil {
			out[e.Path] = b
		}
	}
	return out
}

// ExtractSources returns the file contents an extraction touches.
func (s *Snapshot) ExtractSources(e *Extract) map[string][]byte {
	out := make(map[string][]byte)
	for _, ed := range e.Edits {
		if _, ok := out[ed.Path]; ok {
			continue
		}
		if b, err := s.Source(ed.Path); err == nil {
			out[ed.Path] = b
		}
	}
	return out
}
