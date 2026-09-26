package store

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
)

var (
	// ErrNoTranscript is Find's answer when no file in the workspace's
	// session directory is the session's, the directory itself missing
	// included — and only then: a candidate, an entry whose name ends in
	// _<id>.jsonl, is the session's transcript or an error (ErrNoHeader,
	// ErrCorrupt, ErrNotResumable), never this (plan 028 R2-15), because the
	// caller may open an empty session under the id on this error alone. The
	// one candidate passed over is another session's own file, that of a
	// session whose id ends in _<id>.
	ErrNoTranscript = errors.New("store: no transcript")

	// ErrBadSessionID is Find's refusal of an id that cannot be a stored
	// session's: empty, or holding a path separator, a control character or
	// a glob metacharacter (*, ?, [, \).
	ErrBadSessionID = errors.New("store: bad session id")
)

// maxHeaderBytes bounds how much of a file ReadHeader reads looking for its
// header line: far more than any header holds (a cwd and a persona path, each
// at most PATH_MAX, and a few hashes).
const maxHeaderBytes = 64 << 10

// Find is the path of session id's transcript in workspace cwd, under home,
// the harness's directory (plan 028 §3.2). It lists the workspace's session
// directory, <home>/sessions/<Slug(cwd)>, and reads the header of every
// candidate: every entry whose name ends in _<id>.jsonl, whatever the stamp
// before that holds. It never globs: the id is matched as a string.
//
// A candidate whose header names id is the session's, and, since slugs
// collide, its header must name cwd too. One whose header names another
// session, and whose name also ends in _<that id>.jsonl, is that session's
// own file — a session x_<id>'s — and is passed over, so it never stands in
// the way of id's. Any other is a file that is not where it belongs.
//
// The errors: ErrBadSessionID for an id that cannot be a session's
// (checkSessionID); ErrNoTranscript when no candidate is the session's and
// none was refused; ErrCorrupt for a candidate that is not a regular file,
// or whose header names another workspace, or another session that the file
// is not named for, and for two candidates that are the session's;
// ErrNoHeader for a candidate whose header does not decode; ErrNotResumable
// when the session is a sub-agent's. Find reads only headers, and takes no
// lock: Open does both properly.
func Find(home, cwd, id string) (string, error) {
	if err := checkSessionID(id); err != nil {
		return "", err
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("store: home %q is not an absolute path", home)
	}
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("store: workspace %q is not an absolute path", cwd)
	}
	cwd = filepath.Clean(cwd)
	dir := filepath.Join(filepath.Clean(home), "sessions", Slug(cwd))
	des, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("%w: session %s has no file in %s", ErrNoTranscript, id, dir)
	case err != nil:
		return "", fmt.Errorf("store: %w", err)
	}
	suffix := "_" + id + ".jsonl"
	var found []string
	var h Header // the header of found's last
	for _, de := range des {
		name := de.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		path := filepath.Join(dir, name)
		ch, err := ReadHeader(path)
		switch {
		case err != nil:
			return "", err
		case ch.ID == id && ch.Cwd != cwd:
			return "", fmt.Errorf("%w: session %s's file belongs to %s: %s", ErrCorrupt, id, ch.Cwd, path)
		case ch.ID == id:
			found, h = append(found, path), ch
		case strings.HasSuffix(name, "_"+ch.ID+".jsonl"):
			// Session ch.ID's own file, whose id ends in _<id>.
		default:
			return "", fmt.Errorf("%w: session %s's file belongs to %s: %s", ErrCorrupt, id, ch.ID, path)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%w: session %s has no file in %s", ErrNoTranscript, id, dir)
	case 1:
	default:
		return "", fmt.Errorf("%w: session %s has %d files in %s", ErrCorrupt, id, len(found), dir)
	}
	if h.ParentSession != "" {
		return "", fmt.Errorf("%w: session %s is a sub-agent's (of session %s)", ErrNotResumable, id, h.ParentSession)
	}
	return found[0], nil
}

// checkSessionID refuses an id that cannot be a stored session's: one that is
// empty, would move the file it names (a separator of either kind), puts a
// control character in its name, or would mean something else to a glob
// (*, ?, [, \) — Find never globs, but an id that could is not an id craze
// minted.
func checkSessionID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: it is empty", ErrBadSessionID)
	case strings.ContainsAny(id, `/\*?[`):
		return fmt.Errorf("%w: %q holds a path separator or a glob metacharacter", ErrBadSessionID, id)
	case strings.ContainsFunc(id, unicode.IsControl):
		return fmt.Errorf("%w: %q holds a control character", ErrBadSessionID, id)
	}
	return nil
}

// ReadHeader is the header of the transcript at path: its first line, which
// is all of it when it has no newline, decoded as Load decodes it, read
// without the rest of the file and without a lock — what Find reads of each
// candidate, and what a caller needs before it opens a session (the header's
// tool profile, which the tools a resume builds depend on). A first line that
// is not a valid header is ErrNoHeader; a path that is not a regular file (a
// directory, a named pipe) is ErrCorrupt, and is never read; a file that
// cannot be opened or read is its own error.
func ReadHeader(path string) (Header, error) {
	// Non-blocking, so that opening a named pipe does not wait for a writer;
	// a regular file, the one kind read, reads the same either way.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Header{}, fmt.Errorf("store: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return Header{}, fmt.Errorf("store: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return Header{}, fmt.Errorf("%w: %s is not a regular file (%v)", ErrCorrupt, path, fi.Mode().Type())
	}
	line, err := bufio.NewReader(io.LimitReader(f, maxHeaderBytes)).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Header{}, fmt.Errorf("store: read %s: %w", path, err)
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	if len(line) == 0 {
		return Header{}, fmt.Errorf("%w: %s is empty", ErrNoHeader, path)
	}
	h, err := decodeHeader(line)
	if err != nil {
		return Header{}, fmt.Errorf("%w: %s: line 1: %v", ErrNoHeader, path, err)
	}
	return h, nil
}
