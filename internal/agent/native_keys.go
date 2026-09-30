package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// A running native session learns the keys stored in its providers.toml after
// it opened, so that it can redact them (plan 031 §3.8, P8). The session's
// model table, its model list, its efforts and its tools' environment stay
// what they were at Start: a provider connected meanwhile is offered by the
// next session, not this one, and its key is never one this session sends. It
// is one this session can meet, though — a tool that reads the file, a
// command that prints the key, a prompt it is pasted into — and nothing else
// would ever redact it.
//
// So every turn this adapter runs takes a look first, before anything of the
// turn is redacted or sent: an ordinary prompt, a /compact, and a wake (every
// path on which the harness takes a redactor up, its begin). The look is a
// stat of providers.toml in the Home the adapter opened the harness with —
// never paths.NativeDir() again, which a test's seam or a later environment
// can point elsewhere (panel astra 12) — and, when the file's stamp (size,
// modification time, inode) differs from the one of the last successful
// reading, a reading of the file's inline keys alone (modeltable.StoredKeys:
// no catalog, no models.toml, no merge), every one of them handed to the
// harness's LearnKeys, which judges each, adds the new ones to the session's
// redactor from this turn on, and refuses every turn from here if one is in
// the session's frozen prompt (harness.ErrStoredKeyFrozen). The first turn's
// look always reads. The keys the file held when the table was loaded are
// known already — learning them again changes nothing but what a sub-agent
// opened later starts with — all but one the merge dropped with its entry (an
// incomplete provider of the user's own, say), which is never sent either and
// which this is how the session first learns to redact.
//
// A shell or a sub-agent already running keeps the redactor it began with
// (R1), and a key stored during a turn is learned at the next: the residual
// window the configuration reference documents. So is a stamp that cannot see
// a change — a rewrite in place, to the same size, within one tick of the
// file system's clock of the last reading — which waits for the file's next
// change.

// storedKeys is the session's look at its providers.toml. mu serializes the
// looks — a turn's and a wake's are already one at a time behind the claim,
// and this does not lean on that — and guards the rest. It is never taken
// under a lock of the adapter's, and nothing under it takes one: only the
// harness's own leaf locks, through LearnKeys and Redact.
type storedKeys struct {
	mu sync.Mutex
	// home is the Home the harness was opened with (open(), after the seam):
	// the directory whose providers.toml is watched. "" watches nothing.
	home string
	// stamp is the file as it was stat'ed before the last reading that
	// succeeded, and read says there was one. The stamp is taken BEFORE the
	// reading, so a write that lands between the two leaves the recorded
	// stamp older than the file, and the next look reads again; taken after,
	// that write's stamp would be recorded with the older content's keys, and
	// its key never learned.
	stamp fileStamp
	read  bool
	// problem is the last diagnostic said, so a file that stays unreadable is
	// said once rather than at every turn; a reading that succeeds clears it.
	problem string
}

// fileStamp is what a look compares: a file's size, its modification time in
// nanoseconds and its inode — which an atomic replace (write a temporary file,
// rename it over) always changes, whatever it does to the other two.
type fileStamp struct {
	size  int64
	mtime int64
	ino   uint64
}

// stampOf is path's stamp, following a symlink to the file it names: a
// providers.toml that is a link is read through it, so its target's changes
// are what count. The inode comes from syscall.Stat_t, which Linux and macOS —
// the two systems craze is built for — both give os.Stat's Sys.
func stampOf(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	st := fileStamp{size: fi.Size(), mtime: fi.ModTime().UnixNano()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.ino = sys.Ino // uint64 on Linux and on macOS
	}
	return st, nil
}

// watch sets the directory the looks read, once the harness's Home is known.
func (k *storedKeys) watch(home string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.home = home
}

// learnStoredKeys is one look (the file's comment), made at a turn's start
// with no lock of the adapter's held, before the turn redacts or sends
// anything: hs is the harness the turn runs on. Every diagnostic it writes is
// one line through s.note that names a file, a provider id or a rule — never
// a value — redacted by the session's own redactor after this look's learning
// and sanitized, since a provider id is text from a file.
func (s *nativeSession) learnStoredKeys(hs *harness.Session) {
	k := &s.keys
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.home == "" {
		return
	}
	path := filepath.Join(k.home, modeltable.ProvidersFile)
	note := func(msg string) { s.note(nativeSafe{red: hs.Redact}.line(msg)) }
	say := func(msg string) {
		if msg == k.problem {
			return
		}
		k.problem = msg
		note(msg)
	}
	stamp, err := stampOf(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Nothing is stored, and a file created later is read whatever its
		// stamp: a removed file's inode can be the next file's.
		k.read = false
		return
	case err != nil:
		say(fmt.Sprintf("cannot check %s for keys stored since this session started: %v", path, err))
		return
	case k.read && stamp == k.stamp:
		return
	}
	stored, err := modeltable.StoredKeys(k.home)
	if s.keysSeam != nil {
		s.keysSeam()
	}
	if err != nil {
		// One value-free line (modeltable's errors name a file and a line,
		// table or key, never a value), and nothing learned: the stamp stays
		// unrecorded, so the next look reads the file again.
		say(fmt.Sprintf("%s changed, but its keys cannot be read (%v); this session learns none of them until it is fixed", path, err))
		return
	}
	k.stamp, k.read, k.problem = stamp, true, ""
	keys := make([]modeltable.Secret, len(stored))
	for i, sk := range stored {
		keys[i] = sk.Key
	}
	skipped, err := hs.LearnKeys(keys)
	for i, problem := range skipped {
		if problem == nil {
			continue
		}
		rule := fmt.Sprintf("is shorter than %d bytes", modeltable.MinKeyLen)
		if errors.Is(problem, modeltable.ErrKeyOverlapsMarker) {
			rule = "overlaps craze's redaction marker"
		}
		note(fmt.Sprintf(
			"%s: provider %q: its api_key %s, so it cannot be an API key; this session ignores it, and the next one will refuse the file until it is fixed",
			path, stored[i].Provider, rule))
	}
	if errors.Is(err, harness.ErrStoredKeyFrozen) {
		note(fmt.Sprintf(
			"a key stored in %s since this session started appears in its frozen prompt; every turn from now on is refused — start a new session",
			path))
	}
}
