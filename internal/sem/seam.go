package sem

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Inbound is one package's references into the selection.
type Inbound struct {
	Package string   `json:"package"`
	Symbols []string `json:"symbols"`
	Count   int      `json:"count"`
}

// SeamReport describes the coupling across a proposed package boundary.
type SeamReport struct {
	Package string   `json:"package"`
	Moving  []string `json:"moving"`
	Files   []string `json:"files"`
	// PulledMethods are methods dragged along by their receiver type.
	PulledMethods []string `json:"pulled_methods,omitempty"`
	// MissingDeps are unexported symbols the selection uses that would be
	// left behind. These are the blockers, and usually the whole answer.
	MissingDeps []string `json:"missing_deps,omitempty"`
	// Stranded are unexported symbols the selection would take away from
	// declarations that stay.
	Stranded []string `json:"stranded,omitempty"`
	// NeedsFromSource are exported symbols the selection would keep using,
	// which become an import back to the old package.
	NeedsFromSource []string `json:"needs_from_source,omitempty"`
	// Inbound is who refers to the selection from outside it.
	Inbound []Inbound `json:"inbound,omitempty"`
	// External are the third-party imports the selection carries with it.
	External []string `json:"external_imports,omitempty"`
	Cycle    string   `json:"cycle,omitempty"`
	Verdict  string   `json:"verdict"`
	Advice   string   `json:"advice,omitempty"`
}

// Seam reports what couples a proposed selection to the rest of its package,
// without moving anything.
//
// This is the analysis MoveDecls runs and then discards behind a refusal. The
// question it answers, "what actually couples these?", is worth asking on its
// own: it is the deciding input for whether a split is worth attempting, and
// getting it should not require attempting the split, nor a codebase that
// still compiles afterwards.
func (s *Snapshot) Seam(req MoveRequest) (*SeamReport, error) {
	ms, err := s.resolveMoveSet(req)
	if err != nil {
		return nil, err
	}
	pulled := s.expand(ms, req.IncludeDeps)

	r := &SeamReport{
		Package:       cleanPath(ms.pkg.PkgPath),
		Moving:        ms.names(),
		PulledMethods: pulled,
		MissingDeps:   s.missingDeps(ms),
		Stranded:      s.strandedNames(ms),
	}
	files := make(map[string]bool)
	for _, d := range ms.decls {
		files[filepath.ToSlash(s.Rel(d.file))] = true
	}
	for f := range files {
		r.Files = append(r.Files, f)
	}
	slices.Sort(r.Files)

	uses := s.setUses(ms)
	r.NeedsFromSource = uses.sourcePkg
	r.External = uses.external

	// Who reaches into the selection, and with which symbols.
	byPkg := make(map[string]map[string]bool)
	for _, d := range ms.decls {
		for _, obj := range d.objs {
			for _, ref := range s.setRefSites(ms, obj) {
				p := cleanPath(ref.pkg.PkgPath)
				if byPkg[p] == nil {
					byPkg[p] = make(map[string]bool)
				}
				byPkg[p][obj.Name()] = true
			}
		}
	}
	for p, syms := range byPkg {
		in := Inbound{Package: p}
		for sym := range syms {
			in.Symbols = append(in.Symbols, sym)
		}
		slices.Sort(in.Symbols)
		in.Count = len(in.Symbols)
		r.Inbound = append(r.Inbound, in)
	}
	slices.SortFunc(r.Inbound, func(a, b Inbound) int { return strings.Compare(a.Package, b.Package) })

	// A move needs an import back to the source when it still uses it, and the
	// source needs an import forward when it still references the selection.
	needsSource := len(r.NeedsFromSource) > 0
	needsDest := slices.ContainsFunc(r.Inbound, func(i Inbound) bool { return i.Package == r.Package })
	if needsSource && needsDest {
		r.Cycle = fmt.Sprintf(
			"the selection uses %s from %s while %s still references the selection, so the two would import each other",
			strings.Join(r.NeedsFromSource, ", "), ms.pkg.Name, ms.pkg.Name)
	}

	r.Verdict, r.Advice = seamVerdict(r)
	return r, nil
}

// seamVerdict turns the coupling into a decision and a next step.
func seamVerdict(r *SeamReport) (verdict, advice string) {
	switch {
	case r.Cycle != "":
		return "blocked", "break the cycle first: stop the old package referencing the selection, or take the symbols it needs with it"
	case len(r.Stranded) > 0:
		return "blocked", fmt.Sprintf(
			"declarations staying behind use %s; export them, or move their users too",
			strings.Join(r.Stranded, ", "))
	case len(r.MissingDeps) > 0:
		return "needs_dependencies", fmt.Sprintf(
			"the only coupling is %s; add them to the selection or pass include_dependencies",
			strings.Join(r.MissingDeps, ", "))
	case len(r.NeedsFromSource) > 0:
		return "clean_with_import", fmt.Sprintf(
			"nothing blocks the split; the new package would import the old one for %s",
			strings.Join(r.NeedsFromSource, ", "))
	}
	return "clean", "the selection is self-contained; the split needs no import back"
}

// Render writes the report as text, which reads better than the object for a
// question whose answer is usually one line.
func (r *SeamReport) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "seam analysis for %d declaration(s) in %s\n", len(r.Moving), r.Package)
	fmt.Fprintf(&b, "files:   %s\n", strings.Join(r.Files, ", "))
	fmt.Fprintf(&b, "moving:  %s\n", strings.Join(r.Moving, ", "))
	if len(r.PulledMethods) > 0 {
		fmt.Fprintf(&b, "pulled:  %s\n", strings.Join(r.PulledMethods, ", "))
	}
	b.WriteByte('\n')

	if len(r.MissingDeps) > 0 {
		fmt.Fprintf(&b, "BLOCKING  unexported symbols left behind: %s\n", strings.Join(r.MissingDeps, ", "))
	}
	if len(r.Stranded) > 0 {
		fmt.Fprintf(&b, "BLOCKING  unexported symbols taken away but still used: %s\n", strings.Join(r.Stranded, ", "))
	}
	if r.Cycle != "" {
		fmt.Fprintf(&b, "BLOCKING  %s\n", r.Cycle)
	}
	if len(r.NeedsFromSource) > 0 {
		fmt.Fprintf(&b, "import back to %s for: %s\n", r.Package, strings.Join(r.NeedsFromSource, ", "))
	}
	if len(r.External) > 0 {
		fmt.Fprintf(&b, "carries imports: %s\n", strings.Join(r.External, ", "))
	}
	if len(r.Inbound) > 0 {
		b.WriteString("\nreferenced from:\n")
		for _, in := range r.Inbound {
			fmt.Fprintf(&b, "  %-50s %s\n", in.Package, strings.Join(in.Symbols, ", "))
		}
	}
	fmt.Fprintf(&b, "\nverdict: %s\n%s\n", r.Verdict, r.Advice)
	return b.String()
}

var _ = packages.Visit
