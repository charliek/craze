package tool

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
)

// PathLocks serializes craze's own calls on a file: one mutex per path,
// keyed on the resolved absolute path, so two edits of one file — whichever
// names it through which symlink — never interleave their read and rename
// (plan 019 §3.9, opencode's per-file semaphore, edit.ts:35-45). It says
// nothing to an editor or a formatter outside craze; the edit tool's
// external-change check covers those.
//
// Entries are reference-counted and removed when the last holder or waiter
// is done, so a session that touches many files does not keep a mutex for
// each. The zero value is ready to use; a PathLocks must not be copied.
type PathLocks struct {
	mu    sync.Mutex
	paths map[string]*pathLock
}

type pathLock struct {
	sem  chan struct{} // a one-slot semaphore: a mutex that a select can wait on
	refs int           // holders and waiters
}

// Lock takes the lock for path, waiting until it is free or ctx is done, and
// returns the function that releases it (safe to call more than once). path
// must be absolute: the caller resolves symlinks first, so that every name
// of one file shares one lock. It is cleaned here.
func (l *PathLocks) Lock(ctx context.Context, path string) (unlock func(), err error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("tool: lock path %q is not absolute", path)
	}
	path = filepath.Clean(path)
	// A select with both cases ready picks one at random; a call already
	// cancelled must not sometimes take the lock.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	l.mu.Lock()
	if l.paths == nil {
		l.paths = make(map[string]*pathLock)
	}
	pl := l.paths[path]
	if pl == nil {
		pl = &pathLock{sem: make(chan struct{}, 1)}
		l.paths[path] = pl
	}
	pl.refs++
	l.mu.Unlock()

	select {
	case pl.sem <- struct{}{}:
	case <-ctx.Done():
		l.release(path, pl)
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-pl.sem
			l.release(path, pl)
		})
	}, nil
}

// release drops one reference and forgets the entry with its last.
func (l *PathLocks) release(path string, pl *pathLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	pl.refs--
	if pl.refs == 0 {
		delete(l.paths, path)
	}
}
