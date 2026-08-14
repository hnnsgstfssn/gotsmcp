package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/rewrite"
	"github.com/hnnsgstfssn/treesitter-mcp/internal/sem"
	"github.com/hnnsgstfssn/treesitter-mcp/internal/source"
)

// Error is the package's error identity type.
type Error string

func (e Error) Error() string { return string(e) }

const (
	// ErrNoRoot reports a call with no usable working directory.
	ErrNoRoot Error = "no project root"
	// ErrNotAllowed reports a root outside the configured allowlist.
	ErrNotAllowed Error = "root is not allowed"
)

// maxWorkspaces bounds how many modules stay resident.
//
// Each carries its own parse cache, so this multiplies the memory budget. An
// agent works in one module at a time in practice.
const maxWorkspaces = 2

// DefaultIdleTTL is how long a type-checked snapshot survives without use.
//
// A snapshot of an 827-file module holds around 400 MB. Keeping it makes a
// second semantic call almost free, and keeping it forever makes a long-running
// background server look like a leak, which is what happened: two servers sat
// at nearly 4 GB each. Dropping it on idle gives back both.
const DefaultIdleTTL = 90 * time.Second

// workspace is one module's state: its parse cache, its type-checked snapshot,
// and its pending changesets.
type workspace struct {
	srv    *Server
	root   string
	loader *source.Loader
	store  *rewrite.Store

	mu       sync.Mutex
	selector string
	snap     *sem.Snapshot
	stamps   map[string]stamp
}

// Workspace resolves a root to its module and returns the state for it.
//
// The MCP roots protocol would have been the obvious channel for this, but it
// is deprecated as of protocol 2026-07-28 (SEP-2577), which directs servers to
// take paths as tool parameters instead. So every tool accepts an optional root
// and this turns it into a workspace.
func (s *Server) workspace(root string) (*workspace, error) {
	dir, err := s.resolveRoot(root)
	if err != nil {
		return nil, err
	}

	// Every tool call, syntactic or semantic, counts as activity. Arming the
	// idle timer only from the semantic path meant a long sweep of syntactic
	// calls never triggered a release, and the resident high-water climbed for
	// the life of the process.
	defer s.noteActivity()

	s.mu.Lock()
	defer s.mu.Unlock()
	if ws, ok := s.spaces[dir]; ok {
		s.touch(dir)
		return ws, nil
	}

	l, err := source.NewLoader(dir, source.WithCacheBudget(int64(s.cacheMB)<<20))
	if err != nil {
		return nil, err
	}
	ws := &workspace{srv: s, root: dir, loader: l, store: rewrite.NewStore(32)}
	s.spaces[dir] = ws
	s.order = append(s.order, dir)
	for len(s.order) > maxWorkspaces {
		delete(s.spaces, s.order[0])
		s.order = s.order[1:]
	}
	return ws, nil
}

func (s *Server) touch(dir string) {
	s.order = slices.DeleteFunc(s.order, func(x string) bool { return x == dir })
	s.order = append(s.order, dir)
}

// resolveRoot turns a caller-supplied root into an absolute module directory.
//
// An empty root means the process working directory, which is what a client
// launched inside a project gives us and is the reason a single global server
// registration works across projects.
func (s *Server) resolveRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		root = s.defaultRoot
	}
	if root == "" {
		return "", fmt.Errorf("%w: pass root, or start the server inside a Go module", ErrNoRoot)
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("root %s: %w", root, err)
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}

	// Prefer the enclosing module so that "./..." means the whole module even
	// when the caller pointed at a subdirectory.
	if mod, ok := moduleRoot(abs); ok {
		abs = mod
	}
	if err := s.allow(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// allow enforces the -root allowlist, when one was configured.
func (s *Server) allow(dir string) error {
	if len(s.allowed) == 0 {
		return nil
	}
	for _, a := range s.allowed {
		if dir == a || strings.HasPrefix(dir, a+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is not under any of %s", ErrNotAllowed, dir, strings.Join(s.allowed, ", "))
}

// moduleRoot walks up looking for go.mod.
//
// It is best-effort: a directory of Go files with no module is still a valid
// place to read from, and refusing would break exactly the broken-repository
// case the syntactic tools exist to serve.
func moduleRoot(dir string) (string, bool) {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}

// snapshot returns a type-checked view of selector within this workspace,
// reusing the cached one when nothing on disk has moved.
func (w *workspace) snapshot(ctx context.Context, selector string) (*sem.Snapshot, error) {
	snap, err := w.loadSnapshot(ctx, selector)
	if err != nil {
		return nil, err
	}
	// Deliberately outside w.mu: holding it here means taking another
	// workspace's lock while holding this one, and two workspaces alternating
	// would deadlock.
	w.srv.holdSnapshot(w)
	return snap, nil
}

func (w *workspace) loadSnapshot(ctx context.Context, selector string) (*sem.Snapshot, error) {
	if selector == "" {
		selector = "./..."
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.snap != nil && w.selector == selector && freshStamps(w.stamps) {
		return w.snap, nil
	}
	snap, err := sem.Load(ctx, w.root, selector)
	if err != nil {
		return nil, err
	}
	w.selector = selector
	w.snap = snap
	w.stamps = stampFiles(snap)
	return snap, nil
}

func (w *workspace) invalidate() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.snap = nil
	w.stamps = nil
}

// release drops everything reclaimable: the snapshot and the parse trees.
func (w *workspace) release() {
	w.invalidate()
	w.loader.Clear()
}

// abs turns a display path back into an absolute one.
func (w *workspace) abs(rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(w.root, filepath.FromSlash(rel))
}

type stamp struct {
	size  int64
	mtime int64
}

func freshStamps(stamps map[string]stamp) bool {
	for path, want := range stamps {
		info, err := os.Stat(path)
		if err != nil || info.Size() != want.size || info.ModTime().UnixNano() != want.mtime {
			return false
		}
	}
	return true
}

func stampFiles(snap *sem.Snapshot) map[string]stamp {
	out := make(map[string]stamp)
	for _, p := range snap.Packages {
		for _, f := range p.CompiledGoFiles {
			if info, err := os.Stat(f); err == nil {
				out[f] = stamp{size: info.Size(), mtime: info.ModTime().UnixNano()}
			}
		}
	}
	return out
}

// findChangeset locates a pending changeset across every live workspace, so
// go_apply does not need to be told which module it belongs to.
func (s *Server) findChangeset(id string) (*workspace, *rewrite.ChangeSet, error) {
	s.mu.Lock()
	spaces := make([]*workspace, 0, len(s.spaces))
	for _, ws := range s.spaces {
		spaces = append(spaces, ws)
	}
	s.mu.Unlock()

	for _, ws := range spaces {
		if cs, err := ws.store.Get(id); err == nil {
			return ws, cs, nil
		}
	}
	return nil, nil, fmt.Errorf("%w: %s", rewrite.ErrNotFound, id)
}
