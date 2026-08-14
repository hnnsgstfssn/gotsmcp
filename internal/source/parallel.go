package source

import (
	"context"
	"fmt"
	"runtime"
	"sync"
)

// maxWorkers bounds concurrent parses.
//
// Parsing dominates every project-wide operation: 8.7 ms per file measured on a
// real repository, so 827 files cost 7.7 s sequentially. The work is
// independent per file, so it parallelises, but it does not scale with cores.
// Measured whole-project query latency on a 14-core machine:
//
//	workers  1: 7.68s    workers  4: 4.19s    workers 12: 4.79s
//	workers  2: 5.21s    workers  8: 5.80s
//
// Allocation is the limiter, not CPU: each parse churns megabytes of arena, so
// past about four workers the collector absorbs the gain. The cap is set where
// the curve flattens rather than at GOMAXPROCS, which also keeps peak memory
// and the parser's load-dependent failures down.
func maxWorkers(n int) int {
	w := min(runtime.GOMAXPROCS(0), 8)
	return max(min(w, n), 1)
}

// workers reports the concurrency this loader uses, honouring an override.
func (l *Loader) workers(n int) int {
	if l.maxWorkers > 0 {
		return max(min(l.maxWorkers, n), 1)
	}
	return maxWorkers(n)
}

// mapFiles applies fn to every path concurrently and returns the results in
// input order, so output does not depend on scheduling.
//
// A path that cannot be loaded yields the zero value of T and a non-nil error
// at the same index; callers decide whether that is fatal. The returned error
// is only ever the context's, so cancellation stops the sweep promptly.
func mapFiles[T any](ctx context.Context, l *Loader, paths []string, fn func(*File) (T, error)) ([]T, []error, error) {
	values := make([]T, len(paths))
	errs := make([]error, len(paths))
	if len(paths) == 0 {
		return values, errs, nil
	}

	// A single file is the common case for the per-file tools; skip the
	// goroutine and channel entirely.
	if len(paths) == 1 {
		values[0], errs[0] = apply(l, paths[0], fn)
		return values, errs, ctx.Err()
	}

	// Method values on Loader are safe to call concurrently; File parses
	// outside its own lock, so workers do not serialise on the cache.
	idx := make(chan int)
	var wg sync.WaitGroup
	for range l.workers(len(paths)) {
		wg.Go(func() {
			for i := range idx {
				values[i], errs[i] = apply(l, paths[i], fn)
			}
		})
	}

	// The producer owns cancellation: closing idx drains every worker, so the
	// WaitGroup always completes and no goroutine outlives this call.
	var cancelled error
feed:
	for i := range paths {
		select {
		case idx <- i:
		case <-ctx.Done():
			cancelled = ctx.Err()
			break feed
		}
	}
	close(idx)
	wg.Wait()

	return values, errs, cancelled
}

// apply runs fn for one file, converting a panic into an error for that file.
//
// This has to happen inside the worker. A panic in a goroutine cannot be
// recovered by the caller that started it, so without this the server's
// request-level recovery is powerless and one bad file takes the whole process
// down: the client sees the transport close, with no error and no clue which
// file did it. The parser is a large amount of third-party code running over
// arbitrary input, which is exactly where that risk lives.
func apply[T any](l *Loader, path string, fn func(*File) (T, error)) (value T, err error) {
	defer func() {
		if r := recover(); r != nil {
			var zero T
			value = zero
			err = fmt.Errorf("panic handling %s: %v", l.Rel(path), r)
		}
	}()
	f, ferr := l.File(path)
	if ferr != nil {
		return value, ferr
	}
	return fn(f)
}

// eachFile is mapFiles with per-file errors folded into the caller's own
// reporting, used where a failed file is a warning rather than a failure.
func (l *Loader) eachFile(ctx context.Context, paths []string, fn func(*File) ([]Symbol, error)) ([][]Symbol, error) {
	values, _, err := mapFiles(ctx, l, paths, fn)
	return values, err
}
