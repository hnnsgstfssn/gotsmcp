package rewrite

import (
	"errors"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/tools/imports"
)

// Options controls how a changeset is realised.
type Options struct {
	// FixImports runs goimports over each edited file, adding imports the new
	// code needs and dropping ones it orphaned. This is the payoff over a
	// text substitution: rewriting errors.New(fmt.Sprintf(..)) to fmt.Errorf
	// leaves the import block correct without a second pass.
	FixImports bool
	// MaxHunks caps hunks per file in a preview. Zero means unlimited.
	MaxHunks int
}

// FileChange is the computed outcome for one file.
type FileChange struct {
	Path       string `json:"path"`
	Edits      int    `json:"edits"`
	Hunks      []Hunk `json:"hunks,omitempty"`
	TotalHunks int    `json:"total_hunks"`
	// WasBroken records that the file did not parse before the edit, which
	// suspends the "must still parse" gate.
	WasBroken bool `json:"was_broken,omitempty"`
	// Created marks a file that does not exist yet, which go_move needs when
	// relocating a declaration into a new file.
	Created bool `json:"created,omitempty"`

	newContent []byte
	mode       os.FileMode
}

// Plan is a validated changeset: every file has been spliced, formatted, and
// checked, but nothing has been written.
type Plan struct {
	ID       string       `json:"id"`
	Summary  string       `json:"summary"`
	Changes  []FileChange `json:"changes"`
	Warnings []string     `json:"warnings,omitempty"`
}

// Rel is the path-shortening function used for display.
type Rel func(string) string

// Compute validates a changeset against the current contents of disk and
// produces the resulting bytes for every file, without writing anything.
func Compute(cs *ChangeSet, rel Rel, opt Options) (*Plan, error) {
	plan := &Plan{ID: cs.ID, Summary: cs.Summary, Warnings: slices.Clone(cs.Warnings)}
	groups := byFile(cs.Edits)

	for _, path := range cs.Files {
		edits := groups[path]
		current, err := os.ReadFile(path)
		created := false
		if err != nil {
			// A changeset may legitimately create a file, but only if it was
			// also absent when the preview was taken; otherwise something
			// deleted it underneath us and the preview is stale.
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("read %s: %w", rel(path), err)
			}
			if want, ok := cs.Hashes[path]; ok && want != hash(nil) {
				return nil, fmt.Errorf("%w: %s was deleted after the preview", ErrStale, rel(path))
			}
			current, created = nil, true
		}
		if want, ok := cs.Hashes[path]; ok && !created && hash(current) != want {
			return nil, fmt.Errorf("%w: %s; re-run the operation to get a fresh preview", ErrStale, rel(path))
		}

		wasBroken := !created && !parses(path, current)

		spliced, err := Splice(current, edits)
		if err != nil {
			return nil, err
		}

		final, ferr := finalize(path, spliced, opt.FixImports)
		if ferr != nil {
			if !wasBroken {
				return nil, fmt.Errorf("%w: %s: %v", ErrBroken, rel(path), ferr)
			}
			// The file was already broken; keep the raw splice so the caller
			// can still make progress repairing it.
			final = spliced
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("%s: still does not parse after edit (%v)", rel(path), ferr))
		}

		info, err := os.Stat(path)
		mode := os.FileMode(0o644)
		if err == nil {
			mode = info.Mode().Perm()
		}

		hunks, total := Preview(rel(path), current, edits, opt.MaxHunks)
		plan.Changes = append(plan.Changes, FileChange{
			Path:       rel(path),
			Edits:      len(edits),
			Hunks:      hunks,
			TotalHunks: total,
			WasBroken:  wasBroken,
			Created:    created,
			newContent: final,
			mode:       mode,
		})
	}
	return plan, nil
}

// Apply writes a computed plan to disk. Every file was validated during
// Compute, so a failure here is an I/O fault rather than a bad edit.
//
// Files are written one at a time; the paths that landed are reported even when
// a later write fails, because a caller that cannot tell which half applied has
// no way to recover.
func Apply(plan *Plan, abs func(string) string) (written []string, err error) {
	for _, c := range plan.Changes {
		path := abs(c.Path)
		if werr := writeAtomic(path, c.newContent, c.mode); werr != nil {
			return written, fmt.Errorf("write %s (%d of %d files already written): %w",
				c.Path, len(written), len(plan.Changes), werr)
		}
		written = append(written, c.Path)
	}
	return written, nil
}

// Format returns the canonical form of a Go source file.
//
// With fixImports it is goimports: gofmt plus adding what the file references
// and dropping what it no longer does. Without, it is plain gofmt, which leaves
// the import block alone.
func Format(path string, src []byte, fixImports bool) ([]byte, error) {
	return finalize(path, src, fixImports)
}

// finalize formats the spliced source and, optionally, repairs its imports.
func finalize(path string, src []byte, fixImports bool) ([]byte, error) {
	if fixImports {
		out, err := imports.Process(path, src, &imports.Options{
			Comments:  true,
			TabIndent: true,
			TabWidth:  8,
		})
		if err == nil {
			return out, nil
		}
		// Fall through: goimports also fails on unresolvable packages, which
		// is not the same as invalid syntax, so let gofmt give the verdict.
	}
	return format.Source(src)
}

func parses(path string, src []byte) bool {
	_, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	return err == nil
}

// writeAtomic writes via a temp file in the same directory so readers never
// observe a partial file, and a full disk fails before the original is touched.
func writeAtomic(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	// A move into a new package creates the directory as well as the file.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()

	if _, err = f.Write(content); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	// Durability before rename: a crash must not leave a renamed-but-empty file.
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// IsStale reports whether err is a stale-preview failure, which callers surface
// as a retry hint rather than a bug.
func IsStale(err error) bool { return errors.Is(err, ErrStale) }
