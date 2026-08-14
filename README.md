# gotsmcp

An MCP server that lets an agent read and refactor Go codebases structurally
instead of with `grep` and `sed`.

## Why two engines

Reading and refactoring want opposite things from a parser, so this uses both.

**Tree-sitter** ([gotreesitter](https://github.com/odvcencio/gotreesitter), pure
Go, no CGo) backs reads and structural search. Parsing a large module takes
milliseconds where type-checking takes seconds, and it produces a usable tree
for code that does not compile.

**go/types** backs anything that depends on what a name *refers to*. Tree-sitter
cannot do this, and the gap is not a matter of degree. Given this file:

```go
type Config struct{ Name string }              // the type
func Load() *Config { return &Config{} }       // two references
type Server struct{ Config *Config }           // a field named Config, and a reference
func run(s *Server) {
	Config := s.Config                          // a local, and a field selector
	_ = other.Config{}                          // another package's type
	_ = struct{ Config int }{}                  // an anonymous field
}
```

a tree-sitter query for `Config` returns 10 hits and can classify them only by
node type and parent. `s.Config` and `other.Config` are both a
`field_identifier` under a `selector_expression` and are indistinguishable. The
type checker resolves all of them and reports that exactly 4 are the type.

The failure that decides it: a syntax-only rename would also rewrite the
`Server.Config` field and its selector. **That still compiles.** It is a silent,
self-consistent, wrong refactor, which is worse than one that fails loudly.

So: tree-sitter decides *what the code looks like*, go/types decides *which
bytes are the symbol*, and every write goes through one validated path.

## Install

```
go install github.com/hnnsgstfssn/treesitter-mcp/cmd/gotsmcp@latest
claude mcp add --scope user gots -- gotsmcp
```

One registration works for every project. The server is not bound to a module:
every tool takes an optional `root`, and calls that omit it use the process
working directory, which for a stdio server launched by an editor or agent is
the project you are in. Pass `root` to work on a different module and it
resolves to the enclosing `go.mod`. `go_workspace` reports what the default
resolves to.

The MCP roots protocol would have been the natural channel for this, but it is
deprecated as of protocol 2026-07-28 (SEP-2577), which directs servers to take
paths as tool parameters instead.

Operations are unrestricted by default, though each one is confined to the
module root it resolved to. Pass `-root` one or more times to restrict which
subtrees may be touched:

```
gotsmcp -root ~/work -root ~/src        # nothing outside these
gotsmcp -default-root ~/work/api        # different default for bare calls
gotsmcp -cache-mb 512                   # parse-tree budget per workspace
```

At most three modules stay resident at once, each with its own parse cache, so
the memory ceiling is roughly three times `-cache-mb`.

## Tools

Nineteen tools across four groups. The server also ships MCP `instructions`
explaining which tool answers which question, so a client sees that guidance at
connect time rather than having to infer it from fourteen descriptions.

Syntactic, tree-sitter, milliseconds, works on code that does not compile:

| Tool | Purpose |
| --- | --- |
| `go_read` | Read by file, directory, package, or pattern. Outline mode elides function bodies. |
| `go_tree` | Dump the syntax tree as an S-expression with positions and field names. |
| `go_query` | Run a tree-sitter query across a selector. |
| `go_search_symbols` | Find declarations by name: substring, exact, regex, or fuzzy. |
| `go_search_text` | Regex confined to comments, strings, or identifiers. |

Semantic, go/types, seconds, needs code that compiles:

| Tool | Purpose |
| --- | --- |
| `go_symbol` | Resolve a symbol and report its kind, type, and declaration. |
| `go_refs` | Every reference to a symbol, by type identity, including test files. |
| `go_callers` | Call sites only, each naming the enclosing declaration. |
| `go_implements` | Interface to implementing types, or type to satisfied interfaces. |
| `go_rename` | Rename across the codebase, with conflict detection. |
| `go_check` | Type-check and report compiler errors. |

Edits, preview by default:

| Tool | Purpose |
| --- | --- |
| `go_rewrite` | Replace every node matching a query, using a `${capture}` template. |
| `go_edit` | Replace explicit line/column ranges. |
| `go_apply` | Write a previewed changeset. |
| `go_format` | goimports or gofmt over a selector, with a check-only mode. |

Structural refactors, type-aware, preview by default:

| Tool | Purpose |
| --- | --- |
| `go_move` | Move declarations, or whole files, to another file or package. |
| `go_extract` | Extract statements into a function, params and results from data flow. |
| `go_signature` | Change a parameter list and update every call site. |
| `go_deps` | Forward and reverse package dependencies. |
| `go_seam` | What couples a proposed split to the rest of its package. Read-only. |
| `go_tests_for` | Which test functions reach a symbol, with the call chain. |
| `go_implement` | Generate the methods a type needs to satisfy an interface. |
| `go_tidy` | Where go.mod disagrees with what the code imports. Offline. |
| `go_untested` | Exported API no test reaches. |
| `go_workspace` | Describe the resolved project, module, and any restriction. |

## Search

Five ways to find things, for different questions.

| Question | Tool |
| --- | --- |
| where is `Foo` defined? | `go_search_symbols` |
| which funcs match `^Test.*Handler$`? | `go_search_symbols`, `mode=regex` |
| find TODOs, or SQL in string literals | `go_search_text` |
| every call to `fmt.*`, or any shape | `go_query` |
| every use of *this* symbol | `go_refs` |
| who calls this function | `go_callers` |
| what implements this interface | `go_implements` |
| who depends on this package | `go_deps` |
| what couples these files to the rest | `go_seam` |
| which tests exercise this | `go_tests_for` |
| what exported API has no test | `go_untested` |
| would `go mod tidy` change anything | `go_tidy` |


**`go_query`** is structural search: it matches shapes in the syntax tree, so it
never fires inside a string literal or a comment. Predicates cover the
text-matching cases too, which makes it a safe replacement for `grep`:

| Predicate | Use |
| --- | --- |
| `#eq?` `#not-eq?` | Exact text, or compare two captures |
| `#any-of?` `#not-any-of?` | Text is one of several literals |
| `#match?` `#not-match?` | Regular expression over captured text |
| `#is-exported?` | Identifier starts upper-case |
| `#has-parent?` `#has-ancestor?` | Constrain by enclosing node type |

```
((function_declaration name: (identifier) @n) (#match? @n "^Test.*Handler$"))
((comment) @c (#match? @c "TODO|FIXME"))
((call_expression) @c (#has-ancestor? @c for_statement))
```

**`go_refs`** is semantic search: every occurrence of one symbol, resolved by
type identity across packages and test files. Use it when you mean "this exact
symbol", not "this spelling".

**`go_search_symbols`** finds declarations by name without knowing the package.
Fuzzy mode matches a subsequence and scores word boundaries, so `cfgldr` finds
`ConfigLoader`. It matches spelling, so two unrelated types named `Config` both
appear; feed a returned position into `go_symbol` or `go_refs` to disambiguate.

**`go_search_text`** is regex confined to one kind of syntax. Searching
`password` in `strings` will not also hit a variable of that name or the word in
a comment.

**`go_implements`** answers a question no textual tool can: satisfaction in Go is
structural, so nothing in a type's source says which interfaces it implements.
It works in both directions and includes stdlib interfaces such as `error`,
provided they are in the loaded package graph.

**`go_check`** type-checks and reports errors. Parsing is not compiling: a
rewrite can produce code that parses and does not build, and this is how you
find out.

## The rewrite loop

`go_tree` exists because tree-sitter queries match on exact grammar node names.
An agent that guesses `function_decl` instead of `function_declaration` gets an
empty result set and no error. The loop that avoids that:

1. `go_tree` on the code you want to match, to read the real node names.
2. `go_query` with `count_only: true`, to check the match set is what you meant.
3. `go_rewrite`, which returns a preview.
4. `go_apply` with the returned `changeset_id`.

Migrating every `fmt.*` call in a package to `log.*`:

```json
{
  "query": "(call_expression function: (selector_expression operand: (identifier) @pkg (#eq? @pkg \"fmt\") field: (field_identifier) @fn) arguments: (argument_list) @args) @call",
  "target": "call",
  "template": "log.${fn}${args}",
  "selector": "./internal/worker"
}
```

An `argument_list` node spans its own parentheses, so `${args}` already carries
them. `goimports` runs afterwards, so `log` is added and `fmt` is dropped if
nothing else uses it.

## Planning a split

`go_seam` answers "what actually couples these?" without moving anything and
without needing the result to compile. On a real 827-file repository, analysing
a 42-declaration split takes about 3 seconds cold and 1 second warm:

```
BLOCKING  unexported symbols left behind: columnDescriptorToProto, ...
BLOCKING  unexported symbols taken away but still used: handleQueryTableRows, ...
BLOCKING  the selection uses PlanRead, ReadOnlyTransaction ... from transport
          while transport still references the selection, so the two would
          import each other
verdict: blocked
```

The verdict is `clean`, `clean_with_import`, `needs_dependencies`, or `blocked`,
each with the next step. Getting this from the compiler instead means doing the
move, fixing the cascade, and inferring the boundary from what is left.

## Refactors

`go_signature` takes the new parameter list as a permutation of the old, which
is what makes the call-site rewrite mechanical rather than a guess: every
argument's destination is known.

```json
{"symbol": "pkg.Handle", "params": [
  {"from": -1, "name": "ctx", "type": "context.Context", "value": "context.TODO()"},
  {"from": 0}, {"from": 1}
]}
```

`from` is a zero-based index into the old parameters; `-1` introduces one, which
then needs a `type` and the `value` to pass at existing calls. Variadic
functions, interface methods, and functions used as values rather than called
are refused, because arguments cannot be repositioned reliably in those cases.

`go_signature` also takes several symbols at once with a `rest` entry standing
for the original parameters, which is what makes threading a context through a
call chain one request rather than one per arity:

```json
{"symbols": ["pkg.Alpha", "pkg.Beta"], "params": [
  {"name": "ctx", "type": "context.Context", "value": "context.TODO()"},
  {"rest": true}
]}
```

Grouped parameters flatten, since indices must line up with call arguments. Two
changing calls that nest, as in `f(g(x))`, are refused rather than spliced into
each other; do those innermost first.

`go_extract` derives parameters from variables the range reads but does not
declare, and results from variables it assigns that are read afterwards, both
from the type checker. Ranges containing `return`, `defer`, a labelled branch,
or a `break`/`continue` whose loop lies outside the range are refused.

`go_move` works on a set, not a single declaration. Splitting a package means
moving a cluster whose members use each other's unexported symbols, and judged
one at a time every one of them looks like it is abandoning a dependency:

```json
{"files": ["internal/store/cache.go"], "to": "internal/cache"}
{"symbols": ["pkg.Encoder", "pkg.NewEncoder"], "to": "internal/codec",
 "include_dependencies": true}
```

A directory destination keeps each source file's base name; a `.go` path merges
everything into one file. Methods always follow their receiver type. When it
does refuse, it names the symbols to add rather than just declining.

Across packages it requalifies references in whichever direction each one needs:

```
in the old package      Foo     -> new.Foo
in the new package      old.Foo -> Foo
everywhere else         old.Foo -> new.Foo
```

and prefixes the moved declaration's own uses of what it left behind. The
destination package is created if absent, with its import path derived from the
source package. It refuses, with the reason, when the result could not compile:
a method leaving its receiver's package, a body touching unexported symbols of
the old package, an unexported symbol referenced from outside the destination,
or any move that would make two packages import each other.

## Write safety

- Edits applied through the server are formatted with goimports automatically.
  Files changed by any other means are not; `go_format` covers those, and
  `check_only` answers whether a tree is clean without writing.
- Overlapping edits within a file are an error, not last-writer-wins.
- A changeset records the SHA-256 of every file it read. Applying re-verifies
  them, so a stale preview is refused rather than clobbering a concurrent write.
- The result of an edit must parse. If a file parsed before and does not after,
  the whole changeset is refused. Files that were *already* broken are exempt,
  because repairing them is a legitimate use.
- Writes are temp-file plus rename within the same directory.
- Renames report conflicts (scope collision, shadowing capture, broken interface
  satisfaction) and refuse to apply unless `force: true`.

Atomicity across files is best-effort: all files are validated before any is
written, but a failure partway through the write phase can leave a partial
application. Applied and failed paths are both reported.

## Defaults and memory

Caps are sized against a real 826-file, 118-package, 11 MB repository.

| Setting | Default | Why |
| --- | --- | --- |
| `go_read` max_bytes | 1 MB | Outline of that repo is 3.1 MB whole, 6 KB per package at the median and 578 KB at the worst. 1 MB returns any single package whole and still stops an 11 MB project-wide `mode=full` read. |
| `go_read` max_files | 1000 | Above the 826 files in the repo, so bytes bind first. |
| `go_query` max_matches | 1000 | A project-wide query there returns ~6000 function declarations. `total` is always the true count. |
| `go_tree` max_nodes | 6000 | The median 6 KB file is several thousand nodes at full depth. |
| `go_rewrite` max_sites | 2000 | A real migration touches thousands of sites; this guards against a query broader than intended. |
| `go_search_symbols` max_results | 200 | Ranked best-match first, so the tail is rarely wanted. |
| `-cache-mb` | 128 | Parse-tree budget per workspace. |
| `-idle-ttl` | 90s | Drop the type-checked snapshot after this long unused. |

The type-checked snapshot, not the parse cache, dominates. On an 827-file
module one snapshot holds about 400 MB, and it is kept only while in use:

| | |
| --- | --- |
| idle | ~25 MB |
| after a syntactic read | ~300 MB |
| after a semantic call | ~650 MB peak |
| after `-idle-ttl` elapses | heap back to ~10 MB |

Switching `selector` between calls rebuilds the snapshot, and an agent sweeping
a repository package by package does that on nearly every call. With full
dependency information that pattern climbed 468 -> 808 -> 1124 MB over three
switches and eventually got the process killed; it now runs flat at 54 -> 66 ->
79 MB.

Two caveats worth knowing. The snapshot loads syntax and type information for
the module's own packages only, taking dependency types from export data; with
full dependency information the same module needed 2.4 GB rather than 400 MB.
And on macOS, Activity Monitor keeps reporting the peak after the drop: Go
returns pages with a madvise that leaves them counted as resident until the
system wants them back. Repeated cycles plateau rather than climb, which is what
shows the memory is genuinely reused.

Parse trees are expensive. Retaining all 826 trees for that repository cost
**4.26 GB**, about 386x the source, and the cost barely varies with file size
below ~40 KB: a 3 KB file and a 31 KB file each held about 2.9 MB. The cache is
therefore budgeted by estimated tree memory, not source bytes, at roughly 8 MB
per tree. Expect resident memory near 200 MB plus the budget, since sweeping a
large repository leaves arena high-water the runtime holds for reuse.

The cache buys a lot when it hits: a whole-project `go_query` on that repository
takes 7.7 s cold and 128 ms warm. Raise `-cache-mb` if you have the memory and
run many project-wide queries.

At most two modules stay resident, and only one type-checked snapshot exists at
a time. `-memory-limit-mb` sets a soft heap ceiling if you would rather the
collector work harder than let the process grow.

Parsing runs on up to eight workers. It does not scale with cores, because
allocation rather than CPU is the limiter; measured on 14 cores, whole-project
query latency was 7.68 s at one worker, 4.19 s at four, and no better beyond
that. The cap is set where the curve flattens.

## What this deliberately does not do

It does not run `go test`, `go mod tidy`, or benchmarks. Those are one shell
command each, they mutate state or reach the network, and wrapping them adds a
layer without adding an answer. What was missing is the analysis around them,
which is what `go_tidy` and `go_untested` provide: whether tidying would change
anything and why, and which exported API no test reaches.

Benchmark comparison is the one wrapper that would earn its place, since running
a benchmark twice and computing meaningful deltas is real work. It is not built.

## Concurrency

A snapshot is shared by every caller holding the same selector, and concurrent
semantic calls read from it at once. Its source cache is mutex-guarded because
an unsynchronised map write is not a corrupted value but a runtime throw,
`concurrent map writes`, which kills the process and cannot be recovered: the
client sees the transport close with no error to report.

Type-checking calls are bounded by `-max-semantic` (default 2). Each in-flight
semantic call pins a snapshot, so eight at once on a large module is several
gigabytes for no gain, since they contend on one CPU-bound loader and, when they
share a selector, repeat identical work. Callers need not self-limit.

## Limits

These are reported by the tools rather than hidden, but they are real.

- Semantic tools need code that compiles. After a move that breaks the build,
  fix the callers before renaming. The syntactic tools and `go_check` still
  work on broken code.
- `go_search_symbols` over a whole project truncates rather than overflowing the
  client's response limit, and says so. Narrow with `selector`, `kinds`, or
  `exported_only`.

- Files excluded by build constraints are not type-checked and so are not
  renamed. `go_rename` warns and names them.
- Identifiers in comments, struct tags, `//go:generate` lines, and reflection
  are invisible to the type checker and are not renamed.
- A package that fails to type-check yields no semantic edits for its own files.
  Reads and `go_rewrite` still work on it.
- `go_rewrite` matches shapes, not symbols. For renaming, use `go_rename`.
- `go_implements` only reports interfaces present in the loaded package graph.
  If nothing in the module imports `fmt`, `fmt.Stringer` will not appear.
- The parser can abandon a parse under concurrent load and return a truncated
  tree. `Loader.Parse` uses `ParseStrict`, checks `ParseStoppedEarly`, and
  retries once in isolation, turning silent data loss into a reported error.

## Development

```
go test ./...          # unit and end-to-end tests
go test -race ./...
go vet ./...
```

End-to-end tests run the real server over an in-memory MCP transport against a
copy of a fixture module, and assert the module still builds after each
refactor.
