package sem

import (
	"fmt"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/refactor/satisfy"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/rewrite"
)

// Conflict is a reason the type checker considers a rename unsafe.
type Conflict struct {
	Kind     string `json:"kind"`
	Message  string `json:"message"`
	Position string `json:"position,omitempty"`
}

// Rename is a computed, not-yet-applied rename.
type Rename struct {
	Symbol    Symbol         `json:"symbol"`
	NewName   string         `json:"new_name"`
	Edits     []rewrite.Edit `json:"-"`
	Conflicts []Conflict     `json:"conflicts,omitempty"`
	Warnings  []string       `json:"warnings,omitempty"`
	Sites     int            `json:"sites"`
	Files     []string       `json:"files"`
}

// Rename computes the edits that rename obj to newName everywhere the loaded
// packages refer to it, along with any conflicts that make doing so unsafe.
//
// Conflicts are returned rather than raised so the caller can show them and let
// a human decide. Nothing is written here.
func (s *Snapshot) Rename(obj types.Object, newName string) (*Rename, error) {
	if err := validIdent(newName); err != nil {
		return nil, err
	}
	if newName == obj.Name() {
		return nil, fmt.Errorf("%w: %q is already the name", ErrInvalidName, newName)
	}

	r := &Rename{Symbol: s.Describe(obj), NewName: newName}
	targets := s.targetSet(obj)
	sites := s.sites(targets)
	if len(sites) == 0 {
		return nil, fmt.Errorf("%w: %s has no occurrences in the loaded packages", ErrNotFound, obj.Name())
	}

	files := make(map[string]bool)
	for _, st := range sites {
		note := "reference"
		switch {
		case !samePos(st.via, obj):
			note = "implicit field of embedded " + obj.Name()
		case st.isDef:
			note = "declaration"
		}
		r.Edits = append(r.Edits, rewrite.Edit{
			Path:  st.path,
			Start: uint32(st.start),
			End:   uint32(st.end),
			New:   newName,
			Note:  note,
		})
		// Read every file the rename touches so its content hash is recorded
		// on the snapshot. A changeset built without them cannot detect that
		// the file moved under it between preview and apply.
		if _, err := s.Source(st.path); err != nil {
			return nil, fmt.Errorf("read %s: %w", s.Rel(st.path), err)
		}
		files[st.path] = true
	}
	r.Sites = len(sites)
	for f := range files {
		r.Files = append(r.Files, filepath.ToSlash(s.Rel(f)))
	}
	slices.Sort(r.Files)

	r.Conflicts = append(r.Conflicts, s.scopeConflicts(obj, newName)...)
	r.Conflicts = append(r.Conflicts, s.captureConflicts(obj, newName, sites)...)
	r.Conflicts = append(r.Conflicts, s.interfaceConflicts(obj, newName)...)
	r.Warnings = append(r.Warnings, s.renameWarnings(obj, newName)...)

	return r, nil
}

// targetSet expands obj into every object that shares its spelling and must
// change with it.
//
// The case that matters is the embedded field. Given "type Server struct{
// Config }", the identifier Config both references the type and declares a
// field named Config, and "s.Config" resolves to the field, not the type. Miss
// the field and the rename leaves selectors pointing at a name that no longer
// exists.
func (s *Snapshot) targetSet(obj types.Object) *targets {
	t := newTargets(obj)

	tn := typeNameFor(obj)
	if tn == nil {
		return t
	}
	t.add(tn, obj)

	s.unique(func(p *packages.Package) {
		for _, o := range p.TypesInfo.Defs {
			v, ok := o.(*types.Var)
			if !ok || !v.IsField() || !v.Embedded() {
				continue
			}
			if named := namedOf(v.Type()); named != nil && samePos(named.Obj(), tn) {
				t.add(v, v)
			}
		}
	})
	return t
}

// targets is a set of objects keyed by declaration position rather than
// pointer.
//
// Loading with Tests:true type-checks a package, its test variant, and its
// external test package separately, producing distinct *types.Object values for
// one declaration. Comparing pointers would make demo.Load look ambiguous and,
// far worse, would skip every reference living only in a _test.go file, because
// those resolve to the variant's copy of the object. Positions come from one
// shared FileSet, so they identify a declaration uniquely across variants.
type targets struct {
	primary types.Object
	byPos   map[token.Pos]types.Object
}

func newTargets(primary types.Object) *targets {
	t := &targets{primary: primary, byPos: make(map[token.Pos]types.Object, 4)}
	t.add(primary, primary)
	return t
}

// add records decl, attributing matches to rep for reporting.
func (t *targets) add(decl, rep types.Object) {
	if decl == nil || !decl.Pos().IsValid() {
		return
	}
	if _, ok := t.byPos[decl.Pos()]; !ok {
		t.byPos[decl.Pos()] = rep
	}
}

// lookup reports the representative object for obj, if it is a target.
func (t *targets) lookup(obj types.Object) (types.Object, bool) {
	if obj == nil || !obj.Pos().IsValid() {
		return nil, false
	}
	rep, ok := t.byPos[obj.Pos()]
	return rep, ok
}

func samePos(a, b types.Object) bool {
	return a != nil && b != nil && a.Pos().IsValid() && a.Pos() == b.Pos()
}

// typeNameFor returns the TypeName whose spelling obj shares, which is obj
// itself for a type, or the embedded type for an implicit field.
func typeNameFor(obj types.Object) *types.TypeName {
	switch o := obj.(type) {
	case *types.TypeName:
		return o
	case *types.Var:
		if o.IsField() && o.Embedded() {
			if named := namedOf(o.Type()); named != nil {
				return named.Obj()
			}
		}
	}
	return nil
}

func namedOf(t types.Type) *types.Named {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	named, _ := types.Unalias(t).(*types.Named)
	return named
}

// scopeConflicts reports an existing declaration that the new name would
// collide with in the same scope, struct, or method set.
func (s *Snapshot) scopeConflicts(obj types.Object, newName string) []Conflict {
	var out []Conflict

	if parent := obj.Parent(); parent != nil {
		if prev := parent.Lookup(newName); prev != nil && prev != obj {
			out = append(out, Conflict{
				Kind:     "scope",
				Message:  fmt.Sprintf("%s %q already declared in the same scope", kindOf(prev), newName),
				Position: s.posString(prev.Pos()),
			})
		}
	}

	// Fields and methods live in a type's namespace, not a lexical scope.
	if recv := receiverNamed(obj); recv != nil {
		if prev, _, _ := types.LookupFieldOrMethod(recv, true, obj.Pkg(), newName); prev != nil && prev != obj {
			out = append(out, Conflict{
				Kind: "method-set",
				Message: fmt.Sprintf("%s already has a %s named %q",
					recv.Obj().Name(), kindOf(prev), newName),
				Position: s.posString(prev.Pos()),
			})
		}
	}

	if v, ok := obj.(*types.Var); ok && v.IsField() {
		if owner, prev := s.fieldOwner(v, newName); prev != nil {
			out = append(out, Conflict{
				Kind:     "struct-field",
				Message:  fmt.Sprintf("struct %s already has a member named %q", owner, newName),
				Position: s.posString(prev.Pos()),
			})
		}
	}
	return out
}

// receiverNamed returns the named receiver type of a method, or the owning type
// of a field, so its whole member namespace can be checked.
func receiverNamed(obj types.Object) *types.Named {
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return nil
	}
	return namedOf(sig.Recv().Type())
}

// fieldOwner locates the struct declaring v and reports a member already using
// newName.
func (s *Snapshot) fieldOwner(v *types.Var, newName string) (string, types.Object) {
	var ownerName string
	var clash types.Object
	s.unique(func(p *packages.Package) {
		if clash != nil {
			return
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			st, ok := types.Unalias(tn.Type().Underlying()).(*types.Struct)
			if !ok {
				continue
			}
			for f := range st.Fields() {
				if f != v {
					continue
				}
				if prev, _, _ := types.LookupFieldOrMethod(tn.Type(), true, tn.Pkg(), newName); prev != nil {
					ownerName, clash = tn.Name(), prev
				}
				return
			}
		}
	})
	return ownerName, clash
}

// captureConflicts reports use sites where the new name is already bound to
// something else, so the renamed reference would silently resolve to the wrong
// object.
//
// Only lexical references are checked. A selector like s.Addr is unaffected by
// a package-level Address, so including selectors would produce false alarms on
// most method renames.
func (s *Snapshot) captureConflicts(obj types.Object, newName string, sites []site) []Conflict {
	switch kindOf(obj) {
	case "field", "method":
		return nil
	}

	var out []Conflict
	seen := make(map[string]bool)
	s.unique(func(p *packages.Package) {
		for _, st := range sites {
			if st.isSel || st.pkg != p {
				continue
			}
			scope := p.Types.Scope().Innermost(st.pos)
			if scope == nil {
				continue
			}
			_, found := scope.LookupParent(newName, st.pos)
			if found == nil || found == obj {
				continue
			}
			pos := s.posString(st.pos)
			if seen[pos] {
				continue
			}
			seen[pos] = true
			out = append(out, Conflict{
				Kind: "shadow",
				Message: fmt.Sprintf("%q is already visible here as a %s declared at %s; the renamed reference would bind to it",
					newName, kindOf(found), s.posString(found.Pos())),
				Position: pos,
			})
		}
	})
	return out
}

// interfaceConflicts reports renames that would break an implements relation
// the compiler currently checks.
//
// Renaming a method is the one rename that can break code nowhere near the
// method: if T.Read satisfies io.Reader somewhere, renaming it to Fetch turns a
// working assignment into a type error in an unrelated file.
func (s *Snapshot) interfaceConflicts(obj types.Object, newName string) []Conflict {
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return nil
	}
	name := fn.Name()

	var finder satisfy.Finder
	s.unique(func(p *packages.Package) {
		// Find panics on types it cannot handle in malformed programs.
		defer func() { _ = recover() }()
		finder.Find(p.TypesInfo, p.Syntax)
	})

	var out []Conflict
	seen := make(map[string]bool)
	add := func(kind, msg string) {
		if seen[msg] {
			return
		}
		seen[msg] = true
		out = append(out, Conflict{Kind: kind, Message: msg})
	}

	recvIsInterface := types.IsInterface(sig.Recv().Type())
	for c := range finder.Result {
		iface, ok := types.Unalias(c.LHS.Underlying()).(*types.Interface)
		if !ok {
			continue
		}
		imethod := lookupInterfaceMethod(iface, name)
		if imethod == nil {
			continue
		}
		concrete, _, _ := types.LookupFieldOrMethod(c.RHS, true, fn.Pkg(), name)

		switch {
		case !recvIsInterface && concrete == obj:
			add("interface", fmt.Sprintf(
				"%s satisfies %s only because of %s; renaming it to %s breaks that",
				types.TypeString(c.RHS, relTypeName), types.TypeString(c.LHS, relTypeName), name, newName))
		case recvIsInterface && imethod == obj && concrete != nil:
			add("interface", fmt.Sprintf(
				"%s implements %s.%s; renaming the interface method leaves the implementation behind",
				types.TypeString(c.RHS, relTypeName), types.TypeString(c.LHS, relTypeName), name))
		}

		// The new name colliding inside the interface is equally fatal.
		if imethod == obj && lookupInterfaceMethod(iface, newName) != nil {
			add("interface", fmt.Sprintf("%s already declares a method %s",
				types.TypeString(c.LHS, relTypeName), newName))
		}
	}
	slices.SortFunc(out, func(a, b Conflict) int { return strings.Compare(a.Message, b.Message) })
	return out
}

func lookupInterfaceMethod(iface *types.Interface, name string) *types.Func {
	for m := range iface.Methods() {
		if m.Name() == name {
			return m
		}
	}
	return nil
}

func relTypeName(p *types.Package) string { return p.Name() }

func (s *Snapshot) renameWarnings(obj types.Object, newName string) []string {
	var out []string

	oldExported, newExported := obj.Exported(), token.IsExported(newName)
	if oldExported != newExported {
		verb := "unexports"
		if newExported {
			verb = "exports"
		}
		out = append(out, fmt.Sprintf(
			"renaming %s to %s %s it; callers outside %s are affected and may not be in this selector",
			obj.Name(), newName, verb, packageName(obj)))
	}
	if len(s.Ignored) > 0 {
		out = append(out, fmt.Sprintf(
			"%d file(s) excluded by build constraints were not type-checked and are not renamed: %s",
			len(s.Ignored), strings.Join(s.relAll(s.Ignored), ", ")))
	}
	if len(s.TypeErrors) > 0 {
		out = append(out, fmt.Sprintf(
			"%d type error(s) in the loaded packages; the rename may be incomplete: %s",
			len(s.TypeErrors), first(s.TypeErrors, 3)))
	}
	out = append(out, "occurrences in comments, struct tags, and reflection are not renamed")
	return out
}

func packageName(obj types.Object) string {
	if obj.Pkg() == nil {
		return "the universe scope"
	}
	return obj.Pkg().Path()
}

func (s *Snapshot) relAll(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.ToSlash(s.Rel(p))
	}
	return out
}

func first(s []string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.Join(s, "; ")
}

// validIdent checks that name is something Go will accept as an identifier.
func validIdent(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidName)
	}
	for i, r := range name {
		switch {
		case r == '_':
		case unicode.IsLetter(r):
		case unicode.IsDigit(r) && i > 0:
		default:
			return fmt.Errorf("%w: %q contains %q at offset %d", ErrInvalidName, name, r, i)
		}
	}
	if token.Lookup(name).IsKeyword() {
		return fmt.Errorf("%w: %q is a Go keyword", ErrInvalidName, name)
	}
	if name == "_" {
		return fmt.Errorf("%w: cannot rename to the blank identifier", ErrInvalidName)
	}
	return nil
}
