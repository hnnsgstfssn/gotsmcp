package source

import (
	"container/list"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	ts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// File is a parsed Go source file.
type File struct {
	Path string
	Src  []byte
	Tree *ts.Tree
}

// Loader resolves selectors and parses Go files, caching parse trees. It is
// safe for concurrent use.
type Loader struct {
	root string
	lang *ts.Language

	// gotreesitter parsers are explicitly not safe for concurrent use, so
	// hand each caller its own.
	parsers sync.Pool

	mu       sync.Mutex
	cache    map[string]*list.Element
	lru      *list.List
	bytes    int64
	maxBytes int64

	// parseGate lets a failed parse retry in isolation: normal parses hold it
	// shared, a retry holds it exclusively and so waits for every in-flight
	// parse to finish first.
	parseGate sync.RWMutex

	qmu     sync.Mutex
	queries map[string]*ts.Query

	maxWorkers int
}

type entry struct {
	path  string
	size  int64
	mtime int64
	file  *File
}

// DefaultCacheBudget bounds the estimated memory held by cached parse trees.
//
// At roughly 8 MB per tree this retains about 32 files, which covers a
// read-edit-verify working set. Process memory runs higher than the budget:
// sweeping a large repository leaves 150-200 MB of arena high-water that the
// runtime holds for reuse, so expect resident memory near 200 MB plus the
// budget.
const DefaultCacheBudget = 256 << 20

// treeCost estimates the heap a retained tree holds for a file of n bytes.
//
// Budgeting by source bytes, the obvious approach, is meaningless here.
// Measured against a real 826-file, 11 MB repository, retaining every tree cost
// 4.26 GB, about 386x the source, and the cost barely varies with file size
// below roughly 40 KB: a 3 KB file and a 31 KB file each held about 2.9 MB of
// tree. Source bytes would let 11 MB of code pin 4 GB of heap.
//
// The constants come from a budget sweep on that repository, fitting held heap
// against entry count: about 8 MB marginal per cached tree, rising with source
// size only for large files (a 54 KB file held 15 MB).
func treeCost(n int) int64 {
	const floor = 8 << 20
	const perByte = 280
	if c := int64(n) * perByte; c > floor {
		return c
	}
	return floor
}

// Option configures a Loader.
type Option func(*Loader)

// WithCacheBudget sets the parse-tree memory budget in bytes.
func WithCacheBudget(bytes int64) Option {
	return func(l *Loader) {
		if bytes > 0 {
			l.maxBytes = bytes
		}
	}
}

// WithMaxWorkers overrides the parse concurrency. Zero uses the default.
func WithMaxWorkers(n int) Option {
	return func(l *Loader) { l.maxWorkers = n }
}

// NewLoader returns a Loader rooted at dir. All selectors and paths are
// resolved relative to dir and may not escape it.
func NewLoader(dir string, opts ...Option) (*Loader, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	// Canonicalise up front so that containment checks compare like with like.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("root %s is not a directory", abs)
	}

	lang := grammars.GoLanguage()
	l := &Loader{
		root:     abs,
		lang:     lang,
		parsers:  sync.Pool{New: func() any { return ts.NewParser(lang) }},
		cache:    make(map[string]*list.Element),
		lru:      list.New(),
		maxBytes: DefaultCacheBudget,
		queries:  make(map[string]*ts.Query),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l, nil
}

// Root reports the directory the loader is confined to.
func (l *Loader) Root() string { return l.root }

// Lang returns the Go grammar, needed to interpret node types and compile queries.
func (l *Loader) Lang() *ts.Language { return l.lang }

// Rel renders path relative to the root for display.
func (l *Loader) Rel(path string) string {
	if rel, err := filepath.Rel(l.root, path); err == nil {
		return rel
	}
	return path
}

// File parses path, returning a cached tree when the file is unchanged on disk.
// The returned File must be treated as read-only: it is shared between callers.
func (l *Loader) File(path string) (*File, error) {
	abs, err := l.contain(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", l.Rel(abs), err)
	}

	l.mu.Lock()
	if el, ok := l.cache[abs]; ok {
		if e := el.Value.(*entry); e.size == info.Size() && e.mtime == info.ModTime().UnixNano() {
			l.lru.MoveToFront(el)
			l.mu.Unlock()
			return e.file, nil
		}
		l.evict(el)
	}
	l.mu.Unlock()

	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", l.Rel(abs), err)
	}
	tree, err := l.Parse(src)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", l.Rel(abs), err)
	}
	file := &File{Path: abs, Src: src, Tree: tree}

	l.mu.Lock()
	defer l.mu.Unlock()
	// A concurrent caller may have populated the same entry; either copy is
	// equally valid, so keep whichever is already installed.
	if el, ok := l.cache[abs]; ok {
		l.lru.MoveToFront(el)
		return el.Value.(*entry).file, nil
	}
	el := l.lru.PushFront(&entry{path: abs, size: info.Size(), mtime: info.ModTime().UnixNano(), file: file})
	l.cache[abs] = el
	l.bytes += treeCost(len(src))
	for l.bytes > l.maxBytes && l.lru.Len() > 1 {
		l.evict(l.lru.Back())
	}
	return file, nil
}

// Parse parses src with a pooled parser. Callers that hold source in memory
// (a pending edit, say) use this instead of File.
//
// It uses ParseStrict rather than Parse. Under concurrent load the parser can
// abandon a parse and return a truncated tree with a nil error: measured on a
// real repository, running twelve workers made two or three files out of 827
// come back short, silently dropping about 85 query matches with no error
// reported anywhere. ParseStrict turns that into a failure. It still accepts
// genuinely malformed Go, which recovers into a tree with ERROR nodes and stops
// with reason "accepted", so error tolerance is preserved.
func (l *Loader) Parse(src []byte) (*ts.Tree, error) {
	tree, err := l.parseOnce(src)
	if err == nil {
		return tree, nil
	}
	// The failure is load-dependent rather than input-dependent, so the retry
	// waits for concurrent parses to drain and runs alone on a fresh parser.
	l.parseGate.Lock()
	defer l.parseGate.Unlock()
	tree, retryErr := parseWith(ts.NewParser(l.lang), src)
	if retryErr != nil {
		return nil, fmt.Errorf("parse failed twice: %v; retry: %w", err, retryErr)
	}
	return tree, nil
}

func (l *Loader) parseOnce(src []byte) (*ts.Tree, error) {
	l.parseGate.RLock()
	defer l.parseGate.RUnlock()
	p := l.parsers.Get().(*ts.Parser)
	tree, err := parseWith(p, src)
	if err != nil {
		// Do not return a parser that just failed to the pool; its state is
		// suspect and reusing it would spread the failure.
		return nil, err
	}
	l.parsers.Put(p)
	return tree, nil
}

func parseWith(p *ts.Parser, src []byte) (*ts.Tree, error) {
	tree, err := p.ParseStrict(src)
	if err != nil {
		return nil, err
	}
	if tree == nil {
		return nil, fmt.Errorf("parser returned no tree")
	}
	if tree.ParseStoppedEarly() {
		return nil, fmt.Errorf("parse stopped early: %v", tree.ParseStopReason())
	}
	return tree, nil
}

// Forget drops cached parses for the given paths, which callers do after
// writing to disk so the next read reflects the new bytes even within the
// filesystem's timestamp resolution.
func (l *Loader) Forget(paths ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range paths {
		abs, err := l.contain(p)
		if err != nil {
			continue
		}
		if el, ok := l.cache[abs]; ok {
			l.evict(el)
		}
	}
}

// evict removes el. The caller must hold l.mu.
//
// It deliberately does not call Tree.Release. Release recycles the tree's arena
// into a pool for reuse, which is only safe if nothing is still reading the
// tree, and File hands out *File values that outlive the cache entry: a
// concurrent sweep will happily evict a tree that another worker is midway
// through querying. Doing so panicked inside the parser's node materialisation.
//
// Dropping the reference instead lets the garbage collector reclaim the tree
// once the last reader is done. That trades prompt arena reuse for a guarantee
// no caller can observe freed memory, which is the right side of that trade.
func (l *Loader) evict(el *list.Element) {
	e := el.Value.(*entry)
	delete(l.cache, e.path)
	l.lru.Remove(el)
	l.bytes -= treeCost(len(e.file.Src))
}

// Files flattens resolved packages into a deduplicated, ordered file list.
func Files(pkgs []Package) []string {
	var out []string
	for _, p := range pkgs {
		out = append(out, p.Files...)
	}
	return dedupe(out)
}

// Clear drops every cached parse. Callers use it when a workspace goes idle,
// where holding trees is pure cost.
func (l *Loader) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.lru.Len() > 0 {
		l.evict(l.lru.Back())
	}
	clear(l.cache)
	l.bytes = 0
}

// CacheStats reports the number of cached trees and the estimated memory they
// hold. Exposed for measurement.
func (l *Loader) CacheStats() (entries int, estimated int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.cache), l.bytes
}
