package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var (
	// ErrBusy is Open's refusal of a transcript another Store holds: another
	// craze has the session open, and two writers would interleave their
	// steps (plan 028 §3.2).
	ErrBusy = errors.New("store: busy")

	// ErrNewerTranscript is Open's refusal to cut the tail off a transcript
	// when what it would cut holds an entry of a type this craze does not
	// know: a newer craze wrote it, maybe as a whole unit this one cannot
	// tell from a cut step, and a repair must never destroy that.
	ErrNewerTranscript = errors.New("store: written by a newer craze; not trimming")

	// ErrNotResumable is Find's and Open's refusal of a sub-agent's
	// transcript: a child is never reopened (plan 028 §3.2, §3.3).
	ErrNotResumable = errors.New("store: not resumable")
)

// The steps of Open's repair, in the order it takes them, as the fsStep
// seam names them.
const (
	stepTornWrite = "torn-write" // create <stem>.torn-<stamp>, write the cut bytes to it
	stepTornSync  = "torn-sync"  // fsync it
	stepDirSync   = "dir-sync"   // fsync the directory, so its name is durable too
	stepTruncate  = "truncate"   // ftruncate the transcript to what is kept
	stepNewline   = "newline"    // end a kept last line that has no newline
	stepSync      = "sync"       // fsync the transcript
)

// Open reopens the transcript at path for appending: the same file, grown by
// the same rules as New's (plan 028 §3.2). Find locates it by session id.
//
//  1. It opens path O_RDWR|O_APPEND (close-on-exec, as Go opens every file,
//     so no child inherits the lock) and takes flock(LOCK_EX|LOCK_NB) on that
//     descriptor, held until Close. A transcript another Store holds is
//     ErrBusy.
//  2. It reads the file through the locked descriptor exactly as Load does:
//     ErrNoHeader, ErrCorrupt, and the tolerated torn tail are Load's. A
//     sub-agent's transcript is ErrNotResumable. When opts names a
//     SessionID or a Workspace, the header must name the same, or it is
//     ErrCorrupt.
//  3. When the file extends past the end of the last complete step — a torn
//     last line, or a cut step — it cuts the file back to that end, the
//     transcript Load would have returned. If anything cut is an entry of a
//     type this craze does not know, it refuses with ErrNewerTranscript
//     instead. Otherwise the cut bytes are first written to
//     <stem>.torn-<UTC yyyymmddThhmmssZ> beside the transcript (0600, never
//     over an existing file), which is fsynced, and so is the directory;
//     only then is the transcript truncated. A failure before the truncate
//     refuses the open with the transcript untouched (and the copy
//     removed). A kept last line with no newline gets one. The transcript is
//     fsynced after any repair.
//  4. It holds a resume entry recording opts' contract (Contract), written
//     ahead of the first step after this one, like a change; a session closed
//     before one writes nothing.
//
// Every failure closes the descriptor, which releases the lock. Open uses
// opts' Now, CrazeVersion, SystemPrompt, ToolProfile and Tools; Home and the
// parent fields are New's, and it ignores them.
func Open(opts Options, path string) (_ *Store, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer func() {
		if err != nil {
			_ = f.Close() // and the lock with it
		}
	}()
	if err := flockNB(f.Fd()); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: native session %s is open in another process", ErrBusy, idFromPath(path))
		}
		return nil, fmt.Errorf("store: lock %s: %w", path, err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("store: read %s: %w", path, err)
	}
	p, err := parse(data, path)
	if err != nil {
		return nil, err
	}
	h := p.t.Header
	switch {
	case h.ParentSession != "":
		return nil, fmt.Errorf("%w: session %s is a sub-agent's (of session %s)", ErrNotResumable, h.ID, h.ParentSession)
	case opts.SessionID != "" && h.ID != opts.SessionID:
		return nil, fmt.Errorf("%w: session %s's file belongs to %s: %s", ErrCorrupt, opts.SessionID, h.ID, path)
	case opts.Workspace != "" && h.Cwd != filepath.Clean(opts.Workspace):
		return nil, fmt.Errorf("%w: session %s's file belongs to %s: %s", ErrCorrupt, h.ID, h.Cwd, path)
	}

	s := newBare(opts)
	read := p.t.Entries // every entry that decoded, before the roll back
	keep := p.t.dropIncompleteTurn()
	if err := s.repair(f, path, data, p.keptLen(keep), read[keep:]); err != nil {
		return nil, err
	}
	s.path, s.t, s.f = path, p.t, f
	s.resume = &Entry{Type: TypeResume, Timestamp: s.stamp(), Contract: contractOf(opts)}
	return s, nil
}

// repair cuts data, the transcript read through f, back to its first kept
// bytes, and ends it in a newline; see Open, step 3. cut is the entries in
// the bytes it cuts that decoded: a torn last line never did, and holds no
// whole entry of any type.
func (s *Store) repair(f *os.File, path string, data []byte, kept int, cut []Entry) error {
	if kept == len(data) {
		if len(data) == 0 || data[len(data)-1] == '\n' {
			return nil
		}
		if err := runStep(s.fsStep, stepNewline, func() error { return writeAll(f, []byte("\n")) }); err != nil {
			return fmt.Errorf("store: end %s's last line: %w", path, err)
		}
		return s.syncTranscript(f, path)
	}

	for _, e := range cut {
		if !knownType(e.Type) {
			return fmt.Errorf("%w: %s: entry %q of type %q is past the last complete step", ErrNewerTranscript, path, e.ID, e.Type)
		}
	}
	torn := strings.TrimSuffix(path, ".jsonl") + ".torn-" + s.now().UTC().Format(fileStampLayout)
	created, err := s.saveTorn(torn, data[kept:])
	if err == nil {
		err = runStep(s.fsStep, stepTruncate, func() error { return f.Truncate(int64(kept)) })
	}
	if err != nil {
		// Only a copy this Open made goes: a file already at the name is an
		// earlier Open's copy, which O_EXCL refused to replace.
		if created {
			_ = os.Remove(torn)
		}
		return fmt.Errorf("store: cut %s's torn tail: %w", path, err)
	}
	// From here the transcript is cut, and the copy is the only place the
	// cut bytes are: it stays, whatever fails next.
	return s.syncTranscript(f, path)
}

// saveTorn writes the bytes Open cuts to a new file at torn, and makes both
// the file and its name durable before Open truncates anything. created
// reports whether it made the file, failing or not.
func (s *Store) saveTorn(torn string, cut []byte) (created bool, err error) {
	if err := runStep(s.fsStep, stepTornWrite, func() error {
		tf, err := os.OpenFile(torn, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		created = true
		if err := writeAll(tf, cut); err != nil {
			_ = tf.Close()
			return err
		}
		return tf.Close()
	}); err != nil {
		return created, err
	}
	if err := runStep(s.fsStep, stepTornSync, func() error { return syncPath(torn) }); err != nil {
		return created, err
	}
	return created, runStep(s.fsStep, stepDirSync, func() error { return syncPath(filepath.Dir(torn)) })
}

// syncTranscript fsyncs the repaired transcript, so the next step appends
// after a cut that is on the disk.
func (s *Store) syncTranscript(f *os.File, path string) error {
	if err := runStep(s.fsStep, stepSync, f.Sync); err != nil {
		return fmt.Errorf("store: sync %s: %w", path, err)
	}
	return nil
}

// syncPath fsyncs the file or directory at path.
func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// writeAll writes b to f in full, or fails.
func writeAll(f *os.File, b []byte) error {
	n, err := f.Write(b)
	if err == nil && n < len(b) {
		err = io.ErrShortWrite
	}
	return err
}

// idFromPath is the session id in a transcript's file name,
// <UTC stamp>_<session id>.jsonl, for an error that has nothing better to
// name the session by; the file's base name when it has no such shape.
func idFromPath(path string) string {
	base := filepath.Base(path)
	if _, id, ok := strings.Cut(strings.TrimSuffix(base, ".jsonl"), "_"); ok && id != "" {
		return id
	}
	return base
}

// flockNB takes the session's lock on fd: exclusive, and failing with
// EWOULDBLOCK rather than waiting when another descriptor holds it.
// syscall.Flock is on both platforms craze builds for, Linux and macOS.
func flockNB(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
}

// lockTemp takes the session's lock on f, the temporary file New's create
// has just opened at name, before the link publishes it. Production's f is an
// *os.File and is locked itself, and nothing else is returned. A test's
// openFile double that offers no Fd wraps a descriptor of its own on the same
// file, so the lock is taken on a second descriptor the store opens on name
// and returns: flock locks a file, whichever descriptor holds it, so the
// session is locked all the same, and the store keeps that descriptor open
// beside f until Close.
func lockTemp(f file, name string) (*os.File, error) {
	if d, ok := f.(interface{ Fd() uintptr }); ok {
		return nil, flockNB(d.Fd())
	}
	lk, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	if err := flockNB(lk.Fd()); err != nil {
		_ = lk.Close()
		return nil, err
	}
	return lk, nil
}
