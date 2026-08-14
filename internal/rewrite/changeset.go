package rewrite

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sync"
)

// ChangeSet is a proposed, not-yet-applied set of edits.
type ChangeSet struct {
	ID      string   `json:"id"`
	Summary string   `json:"summary"`
	Edits   []Edit   `json:"-"`
	Files   []string `json:"files"`
	// Hashes records the SHA-256 of every file as it was when the edits were
	// computed, keyed by absolute path.
	Hashes   map[string]string `json:"-"`
	Warnings []string          `json:"warnings,omitempty"`
}

// New builds a changeset from edits and the exact bytes they were computed
// against.
func New(summary string, edits []Edit, sources map[string][]byte) (*ChangeSet, error) {
	if len(edits) == 0 {
		return nil, ErrEmpty
	}
	cs := &ChangeSet{
		ID:      newID(),
		Summary: summary,
		Edits:   edits,
		Hashes:  make(map[string]string, len(sources)),
	}
	for path, src := range sources {
		cs.Hashes[path] = hash(src)
	}
	for path := range byFile(edits) {
		if _, ok := cs.Hashes[path]; !ok {
			return nil, fmt.Errorf("changeset: no source recorded for %s", path)
		}
		cs.Files = append(cs.Files, path)
	}
	slices.Sort(cs.Files)
	return cs, nil
}

// Warn attaches an advisory message, such as a build-constrained file the edit
// could not reach.
func (c *ChangeSet) Warn(format string, args ...any) {
	c.Warnings = append(c.Warnings, fmt.Sprintf(format, args...))
}

func hash(src []byte) string {
	sum := sha256.Sum256(src)
	return hex.EncodeToString(sum[:])
}

func newID() string {
	var b [8]byte
	rand.Read(b[:]) // crypto/rand.Read never returns an error as of Go 1.24
	return "cs_" + hex.EncodeToString(b[:])
}

// Store holds pending changesets between preview and apply. It is bounded so a
// long-running server cannot accumulate them without limit.
type Store struct {
	mu    sync.Mutex
	m     map[string]*ChangeSet
	order []string
	max   int
}

// NewStore returns a store retaining at most max changesets.
func NewStore(max int) *Store {
	if max <= 0 {
		max = 32
	}
	return &Store{m: make(map[string]*ChangeSet, max), max: max}
}

// Put records a changeset, evicting the oldest if the store is full.
func (s *Store) Put(cs *ChangeSet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[cs.ID] = cs
	s.order = append(s.order, cs.ID)
	for len(s.order) > s.max {
		delete(s.m, s.order[0])
		s.order = s.order[1:]
	}
}

// Get returns a changeset by id.
func (s *Store) Get(id string) (*ChangeSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cs, ok := s.m[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return cs, nil
}

// Delete drops a changeset, which callers do once it has been applied.
func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
	s.order = slices.DeleteFunc(s.order, func(x string) bool { return x == id })
}
