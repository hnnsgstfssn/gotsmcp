package tools

// Instructions is server-level guidance sent to the client at initialise time.
//
// It lives here rather than being repeated across fourteen tool descriptions
// because the hard part is not what any single tool does, it is choosing
// between them: which of four search tools answers a given question, when the
// cheap syntactic answer is enough and when only the type checker will do, and
// what the preview/apply contract is. A tool description is read when that tool
// is already under consideration, which is too late for that decision.
const Instructions = `Tools for reading and refactoring Go code structurally.

# Which project

The server is not bound to one module. Every tool takes an optional root; when
omitted it uses the directory the server was started in, which for a client
launched inside a project is that project. Pass root explicitly to work on a
different module, and it resolves to the enclosing go.mod. go_workspace reports
what the default resolves to and whether any restriction is configured.

# Two planes, with different costs and guarantees

Syntactic (go_read, go_tree, go_query, go_search_symbols, go_search_text):
parses with tree-sitter. Milliseconds, and it works on code that does not
compile. It knows how code is spelled and shaped, not what names refer to.

Semantic (go_symbol, go_refs, go_callers, go_implements, go_rename, go_check):
type-checks with go/types. Seconds on a large module, and it needs code that
compiles. It knows exactly what each name refers to.

Prefer the syntactic plane. Escalate when the answer depends on identity rather
than spelling: two packages both declaring Config, a local shadowing a
package-level name, a method promoted through an embedded field.

# Choosing a search tool

  "where is Foo defined?"                  go_search_symbols
  "which functions match ^Test.*Handler$"  go_search_symbols, mode=regex
  "find TODOs" / "find SQL in strings"     go_search_text
  "every call to fmt.*" / shape matching   go_query
  "every use of THIS symbol"               go_refs
  "who calls this function"                go_callers
  "what implements this interface"         go_implements
  "did my edit break the build"            go_check
  "who depends on this package"            go_deps
  "what couples these files to the rest"   go_seam
  "which tests exercise this"              go_tests_for
  "what project am I in"                   go_workspace
  "is the tree formatted"                  go_format, check_only
  "would go mod tidy change anything"      go_tidy
  "what exported API has no test"          go_untested

go_search_symbols matches spelling, so two unrelated types named Config both
appear. When that distinction matters, feed a returned position (file:line:col)
into go_symbol or go_refs.

# Reading

go_read defaults to outline mode: signatures and doc comments with function
bodies elided. It is a small fraction of the tokens of full source and shows a
package's whole API at once. Reach for mode=full or name=Foo only after an
outline tells you which declaration you need.

# Writing a query

Tree-sitter queries match exact grammar node names. Guessing produces a query
that matches nothing and reports no error, so:

  1. go_tree on an example of the code you want to match, to read the real node
     names off the actual tree.
  2. go_query with count_only=true, to confirm the match set is what you meant.
  3. go_rewrite, which previews.
  4. go_apply with the returned changeset_id.

An argument_list node spans its own parentheses, so a ${args} capture already
carries them.

# Editing

Three ways to change code, in order of preference:

  go_rename   a symbol and everything referring to it, type-aware
  go_rewrite  every node matching a query, via a ${capture} template
  go_edit     explicit line/column ranges, when you already know them from
              go_query output and writing a matching query is more work

Before splitting a package, ask go_seam. It reports the coupling across a
proposed boundary without moving anything and without needing the result to
compile, which is cheaper and more certain than moving files and reading the
compiler errors.

Structural refactors, each type-aware and preview-first:

  go_move       declarations to another file or package, requalifying every
                reference. Takes files or a list of symbols, so a package split
                moves as one unit; validation is on the whole selection
  go_extract    a range of statements into a new function, parameters and
                results computed from data flow
  go_signature  a parameter list, updating every call site. Takes several
                symbols with a rest entry, so threading a context through a
                call chain is one request
  go_implement  the methods a type is missing to satisfy an interface

Every mutating tool previews by default and writes nothing until you call
go_apply, or re-run with apply=true. Guarantees on write:

  - Overlapping edits are refused rather than silently ordered.
  - The result must parse. A file that was already broken is exempt, so
    repairing broken code is possible.
  - Imports are fixed automatically, so a rewrite may add or drop imports.
    Files changed by any other means are not formatted; go_format covers those.
  - A preview records file hashes; if anything changed underneath, apply is
    refused and you should re-run to get a fresh preview.

Parsing is not compiling. go_rewrite can produce code that parses and does not
build, such as a call with the wrong argument count. Run go_check afterwards.

For renaming a symbol always use go_rename, never go_rewrite: go_rewrite matches
spelling and will happily rename an unrelated field that shares the name,
producing a change that compiles and is wrong. go_rename resolves types, follows
embedded fields, covers test files, and refuses renames that collide with an
existing declaration or break an interface.

# Ordering

Semantic tools need code that compiles. A refactor that breaks the build has to
be finished before the next semantic step: after a move, fix the callers, then
rename. go_check and every syntactic tool keep working in the meantime.

# Concurrency

Issue as many calls as you like. Type-checking calls are bounded server-side, so
a burst of semantic requests queues rather than competing; there is no need to
serialise them yourself. Syntactic calls are cheap and unbounded.

# Cost

Syntactic tools re-parse files not in cache, a few milliseconds each. A
project-wide sweep of a thousand files is a few seconds cold and milliseconds
warm. Semantic tools type-check the selector; narrowing the selector makes them
much faster but can miss references, so keep the default for renames and narrow
only when you know the symbol is package-local.`
