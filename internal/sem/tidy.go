package sem

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/packages"
)

// TidyResult reports disagreement between go.mod and what the code imports.
type TidyResult struct {
	Module string `json:"module"`
	// UnusedRequires are direct requirements nothing imports any more, which
	// is what a refactor leaves behind.
	UnusedRequires []string `json:"unused_requires,omitempty"`
	// MissingRequires are modules the code imports with no requirement.
	MissingRequires []string `json:"missing_requires,omitempty"`
	DirectRequires  int      `json:"direct_requires"`
	ImportsSeen     int      `json:"imports_seen"`
	Clean           bool     `json:"clean"`
	Note            string   `json:"note"`
}

// Tidy compares go.mod against the imports the code actually has.
//
// It deliberately does not run "go mod tidy": that mutates go.mod and go.sum
// and reaches the network, and any caller with a shell can run it. What is
// missing without it is the question, answered offline in a second, of whether
// running it would change anything and why. A refactor that drops the last use
// of a dependency removes the import statement and leaves the requirement.
//
// Attribution is by longest module-path prefix, which is exactly how the go
// command maps an import path to a module.
func Tidy(ctx context.Context, root string) (*TidyResult, error) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, err
	}
	mf, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, err
	}

	res := &TidyResult{
		Note: "compares go.mod against imports offline; run go mod tidy to apply, which may also adjust versions and indirect requirements",
	}
	if mf.Module != nil {
		res.Module = mf.Module.Mod.Path
	}

	// Cheap load: names and imports only, no types.
	cfg := &packages.Config{
		Mode:    packages.NeedName | packages.NeedImports,
		Dir:     root,
		Context: ctx,
		Tests:   true,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, err
	}

	// Only this module's own packages count. A dependency's imports are that
	// dependency's business, and both halves of the direct/indirect
	// distinction are defined by what this module's code imports.
	imported := make(map[string]bool)
	for _, p := range pkgs {
		for dep := range p.Imports {
			imported[cleanPath(dep)] = true
		}
	}
	res.ImportsSeen = len(imported)

	// Two sets, for two different questions. A requirement is unused when no
	// first-party import needs it, which only makes sense for direct ones. An
	// import is satisfied by any requirement at all, indirect included, so
	// checking it against the direct set alone reports the entire transitive
	// dependency graph as missing.
	direct := make(map[string]bool)
	all := make(map[string]bool)
	for _, r := range mf.Require {
		all[r.Mod.Path] = true
		if r.Indirect {
			continue
		}
		direct[r.Mod.Path] = true
		res.DirectRequires++
		if !anyImportUnder(imported, r.Mod.Path) {
			res.UnusedRequires = append(res.UnusedRequires, r.Mod.Path)
		}
	}

	// The reverse: an import belonging to no requirement and no local package.
	seenMissing := make(map[string]bool)
	for path := range imported {
		if isStdlib(path) || strings.HasPrefix(path, res.Module) {
			continue
		}
		if moduleFor(path, all) != "" {
			continue
		}
		// Fall back to the module path heuristic: host/owner/repo.
		mod := guessModule(path)
		if mod == "" || seenMissing[mod] {
			continue
		}
		seenMissing[mod] = true
		res.MissingRequires = append(res.MissingRequires, mod)
	}

	slices.Sort(res.UnusedRequires)
	slices.Sort(res.MissingRequires)
	res.Clean = len(res.UnusedRequires) == 0 && len(res.MissingRequires) == 0
	return res, nil
}

// anyImportUnder reports whether any import path belongs to a module.
func anyImportUnder(imported map[string]bool, mod string) bool {
	for path := range imported {
		if path == mod || strings.HasPrefix(path, mod+"/") {
			return true
		}
	}
	return false
}

// moduleFor returns the required module owning an import path, longest first.
func moduleFor(path string, required map[string]bool) string {
	best := ""
	for mod := range required {
		if path == mod || strings.HasPrefix(path, mod+"/") {
			if len(mod) > len(best) {
				best = mod
			}
		}
	}
	return best
}

// guessModule takes the first three segments of an import path, which is the
// usual shape of a module root. It is a heuristic, and only used to name a
// suspected missing requirement.
func guessModule(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		return path
	}
	return strings.Join(parts[:3], "/")
}
