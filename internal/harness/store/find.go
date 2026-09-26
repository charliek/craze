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
	"unicode"
)

var (
	// ErrNoTranscript is Find's answer when no file in the workspace's
	// session directory is named for the session, the directory itself
	// missing included — and only then: a file named for it that is not its
	// transcript is ErrNoHeader, ErrCorrupt or ErrNotResumable, never this
	// (plan 028 R2-15), because the caller may open an empty session under
	// the id on this error alone.
	ErrNoTranscript = errors.New("store: no transcript")

	// ErrBadSessionID is Find's refusal of an id that cannot be a stored
	// session's: empty, or holding a path separator, a control character or
	// a glob metacharacter (*, ?, [, \).
	ErrBadSessionID = errors.New("store: bad session id")
)

// maxHeaderBytes bounds how much of a file Find reads looking for its header
// line: far more than any header holds (a cwd and a persona path, each at
// most PATH_MAX, and a few hashes).
const maxHeaderBytes = 64 << 10

// Find is the path of session id's transcript in workspace cwd, under home,
// the harness's directory (plan 028 §3.2). It lists the workspace's session
// directory, <home>/sessions/<Slug(cwd)>, and takes the one file named
// <stamp>_<id>.jsonl — a file whose name ends in _<id>.jsonl but has another
// '_' before that is another session's, one whose id ends in _<id>. It never
// globs: the id is matched as a string.
//
// Slugs collide, so the file's header must name both id and cwd. The errors:
// ErrBadSessionID for an id that cannot be a session's (checkSessionID);
// ErrNoTranscript when no file is named for the session; ErrCorrupt when two
// are, or when the one there names another session or another workspace in
// its header; ErrNoHeader when its header does not decode; ErrNotResumable
// when it is a sub-agent's. Find reads only the header, and takes no lock:
// Open does both properly.
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
	for _, de := range des {
		stamp, ok := strings.CutSuffix(de.Name(), suffix)
		if ok && !strings.Contains(stamp, "_") && !de.IsDir() {
			found = append(found, filepath.Join(dir, de.Name()))
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%w: session %s has no file in %s", ErrNoTranscript, id, dir)
	case 1:
	default:
		return "", fmt.Errorf("%w: session %s has %d files in %s", ErrCorrupt, id, len(found), dir)
	}
	path := found[0]
	h, err := readHeader(path)
	if err != nil {
		return "", err
	}
	switch {
	case h.ID != id:
		return "", fmt.Errorf("%w: session %s's file belongs to %s: %s", ErrCorrupt, id, h.ID, path)
	case h.Cwd != cwd:
		return "", fmt.Errorf("%w: session %s's file belongs to %s: %s", ErrCorrupt, id, h.Cwd, path)
	case h.ParentSession != "":
		return "", fmt.Errorf("%w: session %s is a sub-agent's (of session %s)", ErrNotResumable, id, h.ParentSession)
	}
	return path, nil
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

// readHeader decodes the first line of the file at path, which is all of it
// when it has no newline. A line that is not a valid header is ErrNoHeader;
// a file that cannot be read is its own error.
func readHeader(path string) (Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, fmt.Errorf("store: %w", err)
	}
	defer func() { _ = f.Close() }()
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
