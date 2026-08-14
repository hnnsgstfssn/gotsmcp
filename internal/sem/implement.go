package sem

import (
	"fmt"
	"go/ast"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/rewrite"
)

// ErrCannotImplement reports a conformance request that cannot be generated.
const ErrCannotImplement Error = "cannot generate the implementation"

// Stub is a generated set of interface methods.
type Stub struct {
	Type      string         `json:"type"`
	Interface string         `json:"interface"`
	Missing   []string       `json:"missing"`
	Receiver  string         `json:"receiver"`
	File      string         `json:"file"`
	Edits     []rewrite.Edit `json:"-"`
	Warnings  []string       `json:"warnings,omitempty"`
}

// ImplementInterface generates the methods a type is missing to satisfy an
// interface.
//
// go_implements reports the gap; this closes it. The bodies panic rather than
// returning zero values, because a silently-zero implementation is a bug that
// type-checks, which is the failure mode this whole server exists to avoid.
func (s *Snapshot) ImplementInterface(typeObj, ifaceObj types.Object, destFile string) (*Stub, error) {
	tn, ok := typeObj.(*types.TypeName)
	if !ok {
		return nil, fmt.Errorf("%w: %s is not a type", ErrCannotImplement, typeObj.Name())
	}
	in, ok := ifaceObj.(*types.TypeName)
	if !ok || !types.IsInterface(in.Type()) {
		return nil, fmt.Errorf("%w: %s is not an interface", ErrCannotImplement, ifaceObj.Name())
	}
	iface, ok := types.Unalias(in.Type().Underlying()).(*types.Interface)
	if !ok {
		return nil, fmt.Errorf("%w: %s is not an interface", ErrCannotImplement, in.Name())
	}
	if types.IsInterface(tn.Type()) {
		return nil, fmt.Errorf("%w: %s is itself an interface", ErrCannotImplement, tn.Name())
	}

	pkg, err := s.packageOf(tn)
	if err != nil {
		return nil, err
	}

	// Prefer whichever receiver form the type already uses, so generated code
	// matches the surrounding style instead of imposing one.
	ptr, existing := s.receiverStyle(pkg, tn)
	recvType := tn.Name()
	if ptr {
		recvType = "*" + tn.Name()
	}
	target := tn.Type()
	if ptr {
		target = types.NewPointer(tn.Type())
	}

	stub := &Stub{
		Type:      tn.Name(),
		Interface: in.Name(),
		Receiver:  recvType,
	}

	recvName := "x"
	if existing != "" {
		recvName = existing
	}

	var body strings.Builder
	qual := s.qualifier(pkg)
	for m := range iface.Methods() {
		if found, _, _ := types.LookupFieldOrMethod(target, true, tn.Pkg(), m.Name()); found != nil {
			if types.Identical(found.Type(), m.Type()) {
				continue
			}
			stub.Warnings = append(stub.Warnings, fmt.Sprintf(
				"%s already has %s with a different signature (%s); it is not generated",
				tn.Name(), m.Name(), types.TypeString(found.Type(), qual)))
			continue
		}
		sig, ok := m.Type().(*types.Signature)
		if !ok {
			continue
		}
		stub.Missing = append(stub.Missing, m.Name())
		fmt.Fprintf(&body, "\n// %s implements %s.\nfunc (%s %s) %s%s {\n\tpanic(\"not implemented\")\n}\n",
			m.Name(), in.Name(), recvName, recvType, m.Name(), renderSignature(sig, qual))
	}
	if len(stub.Missing) == 0 {
		return nil, fmt.Errorf("%w: %s already satisfies %s", ErrCannotImplement, tn.Name(), in.Name())
	}

	path := destFile
	if strings.TrimSpace(path) == "" {
		path = s.Fset.Position(tn.Pos()).Filename
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(s.root, path)
	}
	path = filepath.Clean(path)
	stub.File = filepath.ToSlash(s.Rel(path))

	src, err := s.Source(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCannotImplement, stub.File, err)
	}
	stub.Edits = []rewrite.Edit{{
		Path:  path,
		Start: uint32(len(src)),
		End:   uint32(len(src)),
		New:   body.String(),
		Note:  fmt.Sprintf("%d method(s) implementing %s", len(stub.Missing), in.Name()),
	}}
	stub.Warnings = append(stub.Warnings,
		"bodies panic; replace them before relying on the type")
	return stub, nil
}

// renderSignature writes a method signature without its receiver, naming any
// unnamed parameters so the generated body is valid to edit.
func renderSignature(sig *types.Signature, qual types.Qualifier) string {
	var b strings.Builder
	b.WriteByte('(')
	params := sig.Params()
	for i := range params.Len() {
		if i > 0 {
			b.WriteString(", ")
		}
		p := params.At(i)
		name := p.Name()
		if name == "" || name == "_" {
			name = fmt.Sprintf("a%d", i)
		}
		typ := types.TypeString(p.Type(), qual)
		if sig.Variadic() && i == params.Len()-1 {
			typ = "..." + strings.TrimPrefix(typ, "[]")
		}
		fmt.Fprintf(&b, "%s %s", name, typ)
	}
	b.WriteByte(')')

	results := sig.Results()
	switch results.Len() {
	case 0:
	case 1:
		fmt.Fprintf(&b, " %s", types.TypeString(results.At(0).Type(), qual))
	default:
		b.WriteString(" (")
		for i := range results.Len() {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(types.TypeString(results.At(i).Type(), qual))
		}
		b.WriteByte(')')
	}
	return b.String()
}

// receiverStyle reports whether the type's existing methods use a pointer
// receiver, and the name they give it.
func (s *Snapshot) receiverStyle(pkg *packages.Package, tn *types.TypeName) (ptr bool, name string) {
	ptrCount, valCount := 0, 0
	s.eachDeclIn(pkg, func(decl ast.Decl, _ string) {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
			return
		}
		field := fd.Recv.List[0]
		base := field.Type
		isPtr := false
		if star, ok := base.(*ast.StarExpr); ok {
			base, isPtr = star.X, true
		}
		if recvTypeName(base) != tn.Name() {
			return
		}
		if isPtr {
			ptrCount++
		} else {
			valCount++
		}
		if name == "" && len(field.Names) > 0 {
			name = field.Names[0].Name
		}
	})
	if ptrCount == 0 && valCount == 0 {
		// No precedent: a pointer receiver is the safer default, since it
		// works for a type that later gains mutating methods.
		return true, ""
	}
	return ptrCount >= valCount, name
}

// packageOf finds the loaded package declaring a type.
func (s *Snapshot) packageOf(tn *types.TypeName) (*packages.Package, error) {
	var out *packages.Package
	s.unique(func(p *packages.Package) {
		if out == nil && p.Types == tn.Pkg() {
			out = p
		}
	})
	if out == nil {
		return nil, fmt.Errorf("%w: %s is not declared in a loaded package", ErrCannotImplement, tn.Name())
	}
	return out, nil
}

// StubSources returns the file a stub touches.
func (s *Snapshot) StubSources(st *Stub) map[string][]byte {
	out := make(map[string][]byte)
	for _, e := range st.Edits {
		if b, err := s.Source(e.Path); err == nil {
			out[e.Path] = b
		}
	}
	return out
}
