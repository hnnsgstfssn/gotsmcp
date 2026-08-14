// Package source is the read plane: it resolves Go package selectors to files,
// parses those files with tree-sitter, and answers structural questions about
// them.
//
// Nothing here type-checks. That is deliberate. Parsing a large repository with
// tree-sitter costs milliseconds where [golang.org/x/tools/go/packages] with
// type information costs seconds, and tree-sitter still produces a usable tree
// for code that does not compile. Anything that needs name resolution or type
// identity belongs in the sem package instead.
package source
