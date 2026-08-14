// Command gotsmcp serves Go code reading and refactoring over MCP.
//
// It exposes two planes over one codebase. Reads and structural search are
// tree-sitter, so they are fast and work on code that does not compile.
// Renames and reference lookups are go/types, so they resolve names correctly
// instead of matching spelling. Every mutation previews before it writes.
//
// Usage:
//
//	gotsmcp
//
// It speaks MCP over stdio, so a client launches it as a subprocess. It is not
// bound to one module: every tool takes an optional root, and calls that omit
// it use the process working directory, which for a client launched inside a
// project is that project. So a single global registration works everywhere:
//
//	claude mcp add --scope user gots -- gotsmcp
//
// Pass -root one or more times to restrict which directories may be touched.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hnnsgstfssn/treesitter-mcp/internal/tools"
)

const version = "v0.1.0"

func main() {
	var allowed rootList
	flag.Var(&allowed, "root", "restrict operations to this directory subtree; repeatable. Default: unrestricted, with each call resolving its own module")
	dflt := flag.String("default-root", "", "directory used when a tool call omits root (default: the working directory)")
	cacheMB := flag.Int("cache-mb", 128, "parse-tree cache budget in MB per workspace; trees cost roughly 200x their source")
	idle := flag.Duration("idle-ttl", tools.DefaultIdleTTL, "drop the type-checked snapshot after this long unused; a large module holds ~400MB. Negative keeps it")
	memLimit := flag.Int64("memory-limit-mb", 0, "soft memory limit in MB; 0 disables. A ceiling for pathological cases, measured not to help normal use")
	maxSem := flag.Int("max-semantic", 2, "concurrent type-checking calls; each pins a snapshot, so this bounds peak memory")
	verbose := flag.Bool("v", false, "log at debug level")
	flag.Parse()

	if *memLimit > 0 {
		debug.SetMemoryLimit(*memLimit << 20)
	}
	cfg := tools.Config{DefaultRoot: *dflt, Allowed: allowed, CacheMB: *cacheMB, IdleTTL: *idle, MaxSemantic: *maxSem}
	if err := run(cfg, *verbose); err != nil {
		fmt.Fprintf(os.Stderr, "gotsmcp: %v\n", err)
		os.Exit(1)
	}
}

// rootList collects a repeatable -root flag.
type rootList []string

func (r *rootList) String() string     { return strings.Join(*r, ", ") }
func (r *rootList) Set(v string) error { *r = append(*r, v); return nil }

func run(cfg tools.Config, verbose bool) error {
	// stdout carries the protocol, so diagnostics must go to stderr.
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	// The tools package logs through the package-level logger, so install this
	// one as the default or its diagnostics are silently discarded.
	slog.SetDefault(log)

	srv, err := tools.New(cfg)
	if err != nil {
		return err
	}
	defer srv.Close()

	m := mcp.NewServer(&mcp.Implementation{
		Name:        "gotsmcp",
		Title:       "Go tree-sitter refactoring",
		Description: "Structural reading and type-aware refactoring for Go codebases.",
		Version:     version,
	}, &mcp.ServerOptions{Instructions: tools.Instructions})
	srv.Register(m)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("serving", "default_root", srv.DefaultRoot(), "allowed", srv.Allowed(), "version", version, "cache_mb", cfg.CacheMB, "idle_ttl", cfg.IdleTTL)
	if err := m.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return err
	}
	log.Debug("shut down")
	return nil
}
