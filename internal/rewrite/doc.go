// Package rewrite turns byte-range edits into files on disk.
//
// Every mutation in this server funnels through here, whichever plane produced
// it: a tree-sitter query rewrite, an explicit node replacement, or a
// type-checked rename. Centralising it means the safety rules are stated once.
//
// The rules are:
//
//   - Edits within a file may not overlap. Overlap means the caller's model of
//     the file disagrees with reality, so it is an error rather than a
//     last-writer-wins race.
//   - A changeset records the SHA-256 of every file it read. Applying verifies
//     those hashes, so a preview that has gone stale is rejected instead of
//     clobbering someone else's write.
//   - The result of an edit must parse. If a file parsed before the edit and
//     does not after, the whole changeset is refused. Files that were already
//     broken are exempt, because repairing them is a legitimate use.
//   - Writes are temp-file plus rename within the same directory, so a reader
//     sees either the old file or the new one.
//
// Atomicity across files is best-effort: all files are validated before any is
// written, but a failure partway through the write phase can leave a partial
// application. Applied and failed paths are both reported.
package rewrite
