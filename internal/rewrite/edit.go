package rewrite

import (
	"fmt"
	"slices"
	"strings"
)

// Error is the package's error identity type.
type Error string

func (e Error) Error() string { return string(e) }

const (
	// ErrOverlap reports two edits competing for the same bytes.
	ErrOverlap Error = "overlapping edits"
	// ErrStale reports a file that changed between preview and apply.
	ErrStale Error = "file changed since preview"
	// ErrBroken reports an edit that would leave a previously valid file unparseable.
	ErrBroken Error = "edit produces invalid Go"
	// ErrNotFound reports an unknown changeset id.
	ErrNotFound Error = "changeset not found"
	// ErrEmpty reports a changeset with nothing to do.
	ErrEmpty Error = "no edits"
)

// Edit replaces the half-open byte range [Start, End) of a file. An insertion
// is an edit with Start == End.
type Edit struct {
	Path  string `json:"path"`
	Start uint32 `json:"start"`
	End   uint32 `json:"end"`
	New   string `json:"new"`
	// Note explains why this edit exists, so a reviewer reading a 60-site
	// rename can tell a declaration from a reference.
	Note string `json:"note,omitempty"`
}

// Splice applies edits to src. Edits are sorted internally; the caller need not
// pre-order them, but they must not overlap.
func Splice(src []byte, edits []Edit) ([]byte, error) {
	if len(edits) == 0 {
		return src, nil
	}
	ordered := slices.Clone(edits)
	slices.SortStableFunc(ordered, func(a, b Edit) int {
		if a.Start != b.Start {
			return int(a.Start) - int(b.Start)
		}
		return int(a.End) - int(b.End)
	})

	if err := checkBounds(src, ordered); err != nil {
		return nil, err
	}

	var b strings.Builder
	b.Grow(len(src))
	var prev uint32
	for _, e := range ordered {
		b.Write(src[prev:e.Start])
		b.WriteString(e.New)
		prev = e.End
	}
	b.Write(src[prev:])
	return []byte(b.String()), nil
}

func checkBounds(src []byte, ordered []Edit) error {
	n := uint32(len(src))
	var prevEnd uint32
	var prev Edit
	for i, e := range ordered {
		if e.End < e.Start {
			return fmt.Errorf("%s: edit end %d before start %d", e.Path, e.End, e.Start)
		}
		if e.End > n {
			return fmt.Errorf("%s: edit range [%d,%d) exceeds file length %d", e.Path, e.Start, e.End, n)
		}
		if i > 0 && e.Start < prevEnd {
			return fmt.Errorf("%w in %s: [%d,%d) and [%d,%d)",
				ErrOverlap, e.Path, prev.Start, prev.End, e.Start, e.End)
		}
		// Two insertions at the same offset have no defined order.
		if i > 0 && e.Start == e.End && prev.Start == prev.End && e.Start == prev.Start {
			return fmt.Errorf("%w in %s: two insertions at offset %d", ErrOverlap, e.Path, e.Start)
		}
		prevEnd, prev = e.End, e
	}
	return nil
}

// byFile groups edits by path, preserving a stable order.
func byFile(edits []Edit) map[string][]Edit {
	out := make(map[string][]Edit)
	for _, e := range edits {
		out[e.Path] = append(out[e.Path], e)
	}
	return out
}

// Hunk is a single reviewable change: the old lines and what replaces them.
type Hunk struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Old       string `json:"old"`
	New       string `json:"new"`
	Note      string `json:"note,omitempty"`
	EditsHere int    `json:"edits_here,omitempty"`
}

// Preview renders the effect of edits on one file as line-level hunks.
//
// This is generated from the known edit ranges rather than by diffing, so it is
// exact: there is no heuristic alignment that could mislead a reviewer about
// which bytes actually move.
func Preview(rel string, src []byte, edits []Edit, maxHunks int) ([]Hunk, int) {
	ordered := slices.Clone(edits)
	slices.SortStableFunc(ordered, func(a, b Edit) int { return int(a.Start) - int(b.Start) })

	starts := lineStarts(src)
	// Group edits that land on the same source line into one hunk so a
	// three-substitution line is reviewed once, not three times.
	type group struct {
		first, last int
		edits       []Edit
	}
	var groups []group
	for _, e := range ordered {
		fl, ll := lineOf(starts, e.Start), lineOf(starts, e.End)
		if n := len(groups); n > 0 && groups[n-1].last >= fl {
			groups[n-1].last = max(groups[n-1].last, ll)
			groups[n-1].edits = append(groups[n-1].edits, e)
			continue
		}
		groups = append(groups, group{first: fl, last: ll, edits: []Edit{e}})
	}

	total := len(groups)
	if maxHunks > 0 && len(groups) > maxHunks {
		groups = groups[:maxHunks]
	}

	hunks := make([]Hunk, 0, len(groups))
	for _, g := range groups {
		lo, hi := starts[g.first], endOfLine(src, starts, g.last)
		oldText := string(src[lo:hi])

		// Re-splice just this window, rebasing edit offsets onto it.
		local := make([]Edit, len(g.edits))
		for i, e := range g.edits {
			local[i] = Edit{Start: e.Start - lo, End: e.End - lo, New: e.New}
		}
		newText, err := Splice(src[lo:hi], local)
		if err != nil {
			continue
		}
		h := Hunk{File: rel, Line: g.first + 1, Old: oldText, New: string(newText), Note: g.edits[0].Note}
		if len(g.edits) > 1 {
			h.EditsHere = len(g.edits)
		}
		hunks = append(hunks, h)
	}
	return hunks, total
}

func lineStarts(src []byte) []uint32 {
	starts := []uint32{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, uint32(i)+1)
		}
	}
	return starts
}

// lineOf returns the 0-based line containing off.
func lineOf(starts []uint32, off uint32) int {
	i, _ := slices.BinarySearch(starts, off)
	if i == len(starts) || starts[i] > off {
		i--
	}
	return max(i, 0)
}

func endOfLine(src []byte, starts []uint32, line int) uint32 {
	if line+1 < len(starts) {
		return starts[line+1] - 1 // exclude the newline
	}
	return uint32(len(src))
}
