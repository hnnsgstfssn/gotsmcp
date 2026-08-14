package sem

import (
	"context"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Diagnostic is one compiler-level problem.
type Diagnostic struct {
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Col     int    `json:"col,omitempty"`
	Message string `json:"message"`
	Package string `json:"package,omitempty"`
	Kind    string `json:"kind"`
}

// CheckResult reports whether a selector type-checks.
type CheckResult struct {
	OK          bool         `json:"ok"`
	Packages    int          `json:"packages"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	Truncated   bool         `json:"truncated"`
}

// Check type-checks a selector and reports the errors.
//
// The edit tools validate that a result parses, which catches a mangled
// template but not a rewrite that produces syntactically fine, semantically
// wrong Go: a call with the wrong argument count, a renamed method that no
// longer satisfies an interface, a dropped import. Without this an agent
// applies an edit and has no way to find out it broke the build short of
// shelling out to go build, which it should not have to do.
func Check(ctx context.Context, root, selector string, maxDiagnostics int) (*CheckResult, error) {
	if strings.TrimSpace(selector) == "" {
		selector = "./..."
	}
	if maxDiagnostics <= 0 {
		maxDiagnostics = 100
	}

	cfg := &packages.Config{
		// Types alone is enough for diagnostics and is cheaper than also
		// retaining syntax and full type info for every dependency.
		Mode:    packages.NeedName | packages.NeedFiles | packages.NeedImports | packages.NeedDeps | packages.NeedTypes,
		Dir:     root,
		Context: ctx,
		Tests:   true,
	}
	pkgs, err := packages.Load(cfg, selector)
	if err != nil {
		return nil, err
	}

	res := &CheckResult{}
	seen := make(map[string]bool)
	counted := make(map[string]bool)
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if !counted[p.PkgPath] {
			counted[p.PkgPath] = true
			res.Packages++
		}
		for _, e := range p.Errors {
			d := toDiagnostic(e, p.PkgPath)
			key := d.File + ":" + d.Message
			if seen[key] {
				continue
			}
			seen[key] = true
			if len(res.Diagnostics) >= maxDiagnostics {
				res.Truncated = true
				continue
			}
			res.Diagnostics = append(res.Diagnostics, d)
		}
	})

	res.OK = len(res.Diagnostics) == 0 && !res.Truncated
	slices.SortFunc(res.Diagnostics, func(a, b Diagnostic) int {
		if c := strings.Compare(a.File, b.File); c != 0 {
			return c
		}
		return a.Line - b.Line
	})
	return res, nil
}

// toDiagnostic splits the "file:line:col: message" form go/packages produces.
func toDiagnostic(e packages.Error, pkgPath string) Diagnostic {
	d := Diagnostic{Message: e.Msg, Package: pkgPath, Kind: kindOfError(e.Kind)}
	parts := strings.Split(e.Pos, ":")
	switch len(parts) {
	case 3:
		d.File = parts[0]
		d.Line = atoi(parts[1])
		d.Col = atoi(parts[2])
	case 2:
		d.File = parts[0]
		d.Line = atoi(parts[1])
	default:
		d.File = e.Pos
	}
	return d
}

func kindOfError(k packages.ErrorKind) string {
	switch k {
	case packages.ListError:
		return "list"
	case packages.ParseError:
		return "parse"
	case packages.TypeError:
		return "type"
	}
	return "unknown"
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// Rel shortens diagnostic paths against a root.
func (r *CheckResult) Rel(rel func(string) string) {
	for i := range r.Diagnostics {
		if r.Diagnostics[i].File != "" {
			r.Diagnostics[i].File = strings.ReplaceAll(rel(r.Diagnostics[i].File), "\\", "/")
		}
	}
}
