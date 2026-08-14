// Package tools exposes the read, semantic, and edit planes as MCP tools.
//
// The tool descriptions are part of the product. An agent that cannot tell
// go_query from go_refs will reach for grep, so each description states what
// the tool is for and, where it matters, which tool to use instead.
package tools

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server holds the state shared by every tool.
//
// It is deliberately not bound to one module. A client registered globally is
// launched once and used against whatever project the user happens to be in, so
// the module is resolved per call and cached in a small registry.
type Server struct {
	// defaultRoot is used when a call omits root. It is the process working
	// directory, which for a stdio server launched by an editor or agent is
	// the project the user is working in.
	defaultRoot string
	// allowed restricts which directories may be touched. Empty means any.
	allowed []string
	cacheMB int

	idleTTL time.Duration
	// semaphore bounds concurrent semantic work. Each in-flight semantic call
	// pins a type-checked snapshot, so eight at once on a large module is
	// several gigabytes for no gain: they contend on one CPU-bound loader and,
	// when they share a selector, do identical work.
	semaphore chan struct{}

	mu     sync.Mutex
	spaces map[string]*workspace
	order  []string

	// Only one workspace may hold a type-checked snapshot at a time. Three
	// snapshots of a large module is several gigabytes, and an agent is only
	// ever working in one.
	snapMu    sync.Mutex
	snapOwner *workspace
	snapTimer *time.Timer
}

// noteActivity restarts the idle countdown without claiming a snapshot, so any
// tool call keeps caches alive and any pause releases them.
func (s *Server) noteActivity() {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if s.snapTimer != nil {
		s.snapTimer.Stop()
	}
	s.snapTimer = time.AfterFunc(s.idleTTL, s.releaseIdle)
}

// releaseIdle drops every cache and hands the memory back.
func (s *Server) releaseIdle() {
	s.snapMu.Lock()
	s.snapOwner = nil
	s.snapMu.Unlock()

	s.mu.Lock()
	spaces := slices.Collect(maps.Values(s.spaces))
	s.mu.Unlock()

	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	for _, ws := range spaces {
		ws.release()
	}
	debug.FreeOSMemory()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	slog.Debug("released idle caches",
		"workspaces", len(spaces),
		"heap_before_mb", before.HeapAlloc>>20,
		"heap_after_mb", after.HeapAlloc>>20)
}

// holdSnapshot records that ws is the workspace whose snapshot is live, evicts
// any other, and restarts the idle countdown.
func (s *Server) holdSnapshot(ws *workspace) {
	s.snapMu.Lock()
	prev := s.snapOwner
	s.snapOwner = ws
	if s.snapTimer != nil {
		s.snapTimer.Stop()
	}
	s.snapTimer = time.AfterFunc(s.idleTTL, s.releaseIdle)
	s.snapMu.Unlock()

	if prev != nil && prev != ws {
		prev.release()
	}
}

// Close releases every cache. Callers use it at shutdown.
func (s *Server) Close() {
	s.snapMu.Lock()
	if s.snapTimer != nil {
		s.snapTimer.Stop()
	}
	s.snapMu.Unlock()
	s.mu.Lock()
	spaces := slices.Collect(maps.Values(s.spaces))
	s.mu.Unlock()
	for _, ws := range spaces {
		ws.release()
	}
	debug.FreeOSMemory()
}

// Config configures a Server.
type Config struct {
	// DefaultRoot is used when a tool call omits root. Empty means the process
	// working directory.
	DefaultRoot string
	// Allowed restricts operations to these directory subtrees. Empty means
	// any directory is permitted, which is the default so that one global
	// registration works across projects.
	Allowed []string
	// CacheMB is the parse-tree budget per workspace. Zero uses the default.
	CacheMB int
	// IdleTTL is how long an unused type-checked snapshot is kept. Zero uses
	// DefaultIdleTTL; negative keeps it indefinitely.
	IdleTTL time.Duration
	// MaxSemantic bounds concurrent type-checking tool calls. Zero uses 2.
	MaxSemantic int
}

// New builds a tool server.
func New(cfg Config) (*Server, error) {
	root := cfg.DefaultRoot
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("determine working directory: %w", err)
		}
		root = wd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	allowed := make([]string, 0, len(cfg.Allowed))
	for _, a := range cfg.Allowed {
		p, err := filepath.Abs(a)
		if err != nil {
			return nil, fmt.Errorf("allowed root %s: %w", a, err)
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		allowed = append(allowed, p)
	}
	slices.Sort(allowed)

	ttl := cfg.IdleTTL
	if ttl == 0 {
		ttl = DefaultIdleTTL
	}
	if ttl < 0 {
		ttl = 1<<62 - 1 // effectively never
	}
	limit := cfg.MaxSemantic
	if limit <= 0 {
		limit = 2
	}
	return &Server{
		semaphore:   make(chan struct{}, limit),
		defaultRoot: abs,
		allowed:     allowed,
		cacheMB:     cfg.CacheMB,
		idleTTL:     ttl,
		spaces:      make(map[string]*workspace),
	}, nil
}

// DefaultRoot reports the directory used when a call omits root.
func (s *Server) DefaultRoot() string { return s.defaultRoot }

// Allowed reports the configured allowlist, empty if unrestricted.
func (s *Server) Allowed() []string { return s.allowed }

// Register adds every tool to an MCP server.
// semanticTools type-check, so their concurrency is bounded.
var semanticTools = map[string]bool{
	"go_symbol": true, "go_refs": true, "go_callers": true, "go_implements": true,
	"go_rename": true, "go_check": true, "go_move": true, "go_extract": true,
	"go_signature": true, "go_seam": true, "go_tests_for": true,
	"go_implement": true, "go_untested": true,
}

// limitSemantic serialises type-checking calls down to a small number in
// flight.
//
// An agent issuing eight reference queries at once is reasonable behaviour and
// used to take the server down. Bounding it here rather than asking callers to
// self-limit keeps the failure impossible instead of merely discouraged.
func (s *Server) limitSemantic(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		call, ok := req.(*mcp.CallToolRequest)
		if !ok || call.Params == nil || !semanticTools[call.Params.Name] {
			return next(ctx, method, req)
		}
		select {
		case s.semaphore <- struct{}{}:
			defer func() { <-s.semaphore }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return next(ctx, method, req)
	}
}

func (s *Server) Register(m *mcp.Server) {
	m.AddReceivingMiddleware(recoverPanics, s.limitSemantic)
	s.registerRead(m)
	s.registerSearch(m)
	s.registerSem(m)
	s.registerEdit(m)
	s.registerRefactor(m)
	s.registerMoves(m)
	s.registerAnalysis(m)
	s.registerFormat(m)
	s.registerHygiene(m)
}

// recoverPanics turns a panic in a handler into an error on that one request.
//
// Without it a single bad input kills the process and the client loses its
// session, any previewed changesets, and the warm caches. That is a poor trade
// for a bug in one tool, and it happened: go_implements dereferenced the nil
// package of a universe-scope type and took the whole server down. The panic is
// still logged to stderr, so it does not become invisible.
func recoverPanics(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("handler panicked",
					"method", method, "panic", r, "stack", string(debug.Stack()))
				result = nil
				err = fmt.Errorf("internal error in %s: %v", method, r)
			}
		}()
		return next(ctx, method, req)
	}
}

// text builds a result whose content is plain text rather than JSON, which
// reads far better for source listings and tree dumps.
func text(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

// cmpOr returns v when it is positive, otherwise def.
func cmpOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}
