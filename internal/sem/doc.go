// Package sem is the semantic plane: everything that needs to know what a name
// refers to rather than merely how it is spelled.
//
// The distinction matters. Given "Config", tree-sitter can report that it is a
// type_identifier under a pointer_type. Only the type checker can report that
// it is the *types.TypeName declared at demo/demo.go:10 and not the struct
// field, the local variable, or the identically named type in another package.
// Renaming on the first kind of answer produces edits that compile and are
// wrong, which is worse than edits that fail loudly.
//
// So every operation here resolves to a [types.Object] and compares by pointer
// identity. Loading is correspondingly expensive: type-checking a large module
// takes seconds where parsing it takes milliseconds. Callers that only need to
// read or search should use the source package instead.
//
// Known limits, all of which are reported rather than hidden:
//
//   - Files excluded by build constraints are not type-checked and therefore
//     not renamed. Resolve reports them so the caller can say so.
//   - Identifiers named in comments, struct tags, //go:generate directives, and
//     reflection are invisible to the type checker.
//   - Packages that fail to type-check yield no edits for their own files.
package sem
