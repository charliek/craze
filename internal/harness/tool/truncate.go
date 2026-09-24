package tool

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/harness/redact"
)

// The limits on what a model sees of one result, opencode's (truncate.ts:14-15).
const (
	MaxLines = 2000
	MaxBytes = 50 * 1024
)

const (
	// SpillDir is the directory under the harness's home that holds full
	// results the model saw only part of.
	SpillDir = "tool-output"
	// spillPrefix starts every spill file's name, and is how Sweep knows
	// its own files.
	spillPrefix = "tool_"
	// spillRetention is how long a spill file is kept (truncate.ts:12).
	spillRetention = 7 * 24 * time.Hour
	// spillAttempts bounds the suffixed names OpenSpill tries after the
	// plain one is taken.
	spillAttempts = 4
)

// limits are Truncate's caps; tests shrink them.
type limits struct{ lines, bytes int }

// truncatedHint starts the sentence that follows a truncation notice.
const truncatedHint = "The tool call succeeded but the output was truncated. "

// Truncate returns text as the model should see it. Text within MaxLines
// lines and MaxBytes bytes comes back as it is. Longer text keeps whole lines
// from the head or the tail, up to both limits, with opencode's notice of
// what was cut; the full text is written to a spill file under home, named
// from id, and the notice says where (truncate.ts:87-139, less its Task-tool
// variant, which tells the model to have an explore agent read the spill
// file: craze has sub-agents now, the agent tool, and the variant stays out
// all the same, so a truncated result never invites a child just to read it —
// plan 026 §3.3). If the spill file cannot be written the text is cut all the
// same, and the notice says the rest was not saved: failing the call instead
// could make the model repeat a command that already had its effect.
//
// A single line longer than MaxBytes keeps nothing of itself, as in
// opencode. dir must be Head or Tail; None returns text as it is.
//
// Truncate redacts nothing: text should be redacted already, and the spill
// path it adds is not. The Dispatcher, which calls the same code, redacts
// that path where it is added.
func Truncate(home, id, text string, dir Direction) (string, Truncation) {
	return truncate(home, id, text, dir, limits{MaxLines, MaxBytes}, nil)
}

// truncate is Truncate, with the path it shows — in the notice and in
// Truncation.Spill — passed through red. Everything else it adds is fixed
// text, and whole lines are kept, so a key in redacted text is never cut
// into a fragment redaction would miss.
func truncate(home, id, text string, dir Direction, lim limits, red *redact.Replacer) (string, Truncation) {
	total := strings.Count(text, "\n") + 1
	t := Truncation{KeptBytes: len(text), TotalBytes: len(text), KeptLines: total, TotalLines: total}
	if dir == None || (total <= lim.lines && len(text) <= lim.bytes) {
		return text, t
	}

	kept, size, hitBytes := keepLines(text, dir, lim)
	preview := text[:size]
	if dir == Tail {
		preview = text[len(text)-size:]
	}
	t.KeptBytes, t.KeptLines = size, kept

	removed, unit := total-kept, "lines"
	if hitBytes {
		removed, unit = len(text)-size, "bytes"
	}
	notice := "..." + strconv.Itoa(removed) + " " + unit + " truncated..."

	hint := truncatedHint + "The full output could not be saved."
	if path, err := writeSpill(home, id, text); err == nil {
		// A key in the harness's home would otherwise reach the model here,
		// after the result's own text was redacted.
		t.Spill = red.String(path)
		hint = truncatedHint + "Full output saved to: " + t.Spill +
			"\nUse Grep to search the full content or Read with offset/limit to view specific sections."
	}
	// The notice and hint join the result after its redaction pass, so they
	// get their own: a key that happens to be one of their words must not
	// reach the model either. The spill path in the hint is already redacted,
	// and a second pass changes nothing (MarkerOverlaps keys are refused).
	added := red.String(notice + "\n\n" + hint)
	if dir == Head {
		return preview + "\n\n" + added, t
	}
	return added + "\n\n" + preview, t
}

// keepLines counts the whole lines, taken from text's head or tail, that fit
// within lim, and the bytes they span with the newlines between them: the
// preview is that many bytes from that end of text, so it is sliced, never
// rebuilt, and a large output is not split into lines. hitBytes says the
// byte limit, not the line limit, is what stopped it.
func keepLines(text string, dir Direction, lim limits) (lines, size int, hitBytes bool) {
	rest := text
	for lines < lim.lines {
		var line string
		var more bool
		if dir == Head {
			line, rest, more = strings.Cut(rest, "\n")
		} else {
			i := strings.LastIndexByte(rest, '\n')
			line, rest, more = rest[i+1:], rest[:max(i, 0)], i >= 0
		}
		n := len(line)
		if lines > 0 {
			n++ // the newline that joins it to what is kept
		}
		if size+n > lim.bytes {
			return lines, size, true
		}
		lines, size = lines+1, size+n
		if !more {
			break
		}
	}
	return lines, size, false
}

// writeSpill writes text to a new spill file for id and returns its path. A
// file it could not finish is removed: half an output under a name that
// promises all of it would mislead the model.
func writeSpill(home, id, text string) (string, error) {
	root, err := openSpillDir(home, true)
	if err != nil {
		return "", err
	}
	defer root.Close()
	f, name, err := createSpill(root, id)
	if err != nil {
		return "", err
	}
	_, werr := f.WriteString(text)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = root.Remove(name)
		return "", err
	}
	return f.Name(), nil
}

// OpenSpill creates a new, empty spill file for the call id under
// <home>/tool-output and returns it open for writing; its path is f.Name().
// A tool that truncates itself (Direction None) writes its full output here.
//
// The model can reach the directory through bash, so nothing there is
// trusted. The directory is created 0700 (and tightened to it), refused if it
// is a symlink or not a directory, and then used only through an os.Root
// opened on it (openSpillDir), so swapping it for a symlink cannot send a
// file elsewhere. The file is created 0600 with O_EXCL and O_NOFOLLOW, so an
// existing file is never overwritten and a symlink planted at the name is
// never followed. The name is tool_<id>. Ids are unique within a session but
// not across sessions, so the name can be taken — by another session's call
// with the same id, or by a planted file — and then a random suffix is
// tried, tool_<id>-<8 hex digits>, a few times before giving up.
func OpenSpill(home, id string) (*os.File, error) {
	root, err := openSpillDir(home, true)
	if err != nil {
		return nil, err
	}
	defer root.Close() // the file stays open on its own descriptor
	f, _, err := createSpill(root, id)
	return f, err
}

// createSpill creates the spill file for id in root: see OpenSpill. It
// returns the file and its name in root.
func createSpill(root *os.Root, id string) (*os.File, string, error) {
	if !validID(id) {
		return nil, "", fmt.Errorf("tool: call id %q is not safe as a file name", id)
	}
	name := spillPrefix + id
	for attempt := 0; ; attempt++ {
		f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
		if err == nil {
			return f, name, nil
		}
		// EEXIST is what O_EXCL gives for anything at the name, a symlink
		// included (os.Root maps a platform's ELOOP to it). Either way the
		// name is taken, not the directory broken.
		if !errors.Is(err, fs.ErrExist) && !errors.Is(err, syscall.ELOOP) {
			return nil, "", fmt.Errorf("tool: %w", err)
		}
		if attempt == spillAttempts {
			return nil, "", fmt.Errorf("tool: no free spill file name for call %s in %s", id, root.Name())
		}
		var b [4]byte
		_, _ = rand.Read(b[:]) // never fails (crypto/rand)
		name = spillPrefix + id + "-" + hex.EncodeToString(b[:])
	}
}

// spillDirChecked runs between openSpillDir's check of the directory and its
// open of it: the window a swap would use. Tests make the swap there.
var spillDirChecked = func() {}

// openSpillDir returns <home>/tool-output as an os.Root, creating it (and
// home) first when create is set. Every later spill-file operation goes
// through the Root, relative to the directory's own descriptor, so none can
// be redirected outside it by renaming the directory or a symlink appearing
// in its place.
//
// Opening it is check, open, compare. The entry must be a real directory,
// not a symlink (Lstat, relative to home). It is then opened as a Root, and
// the directory that opened must be the one checked (os.SameFile): a swap
// between the two either escapes home, which os.Root refuses, or lands on
// another directory, which the comparison refuses. Its mode is tightened to
// 0700 through the opened descriptor, never by path.
//
// home itself is trusted as configured and may be a symlink; a missing home
// or directory is an error matching fs.ErrNotExist.
func openSpillDir(home string, create bool) (*os.Root, error) {
	if !filepath.IsAbs(home) {
		return nil, fmt.Errorf("tool: the spill directory needs an absolute home, not %q", home)
	}
	if create {
		if err := os.MkdirAll(home, 0o700); err != nil {
			return nil, fmt.Errorf("tool: %w", err)
		}
	}
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		return nil, fmt.Errorf("tool: %w", err)
	}
	defer homeRoot.Close()
	if create {
		if err := homeRoot.Mkdir(SpillDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("tool: %w", err)
		}
	}
	checked, err := homeRoot.Lstat(SpillDir)
	if err != nil {
		return nil, fmt.Errorf("tool: %w", err)
	}
	if !checked.IsDir() {
		return nil, fmt.Errorf("tool: %s is not a directory", filepath.Join(home, SpillDir))
	}
	spillDirChecked()
	root, err := homeRoot.OpenRoot(SpillDir)
	if err != nil {
		return nil, fmt.Errorf("tool: %w", err)
	}
	if err := sameAndPrivate(root, checked); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

// sameAndPrivate checks that root is the directory checked describes, and
// makes it 0700 through its own descriptor.
func sameAndPrivate(root *os.Root, checked fs.FileInfo) error {
	d, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("tool: %w", err)
	}
	defer d.Close()
	info, err := d.Stat()
	if err != nil {
		return fmt.Errorf("tool: %w", err)
	}
	if !os.SameFile(checked, info) {
		return fmt.Errorf("tool: %s changed while it was being opened; not used", root.Name())
	}
	if info.Mode().Perm() != 0o700 {
		if err := d.Chmod(0o700); err != nil {
			return fmt.Errorf("tool: %w", err)
		}
	}
	return nil
}

// validID reports whether id is safe as part of a file name: one to 64
// letters, digits, dots, dashes and underscores, starting with a letter or a
// digit — so never "", ".", "..", or anything with a slash. The harness's own
// ids ("t3.2.1") always pass.
func validID(id string) bool {
	return len(id) <= 64 && allBytes(id, func(c byte) bool { return wordByte(c) || c == '.' || c == '-' }) &&
		id[0] != '_' && id[0] != '.' && id[0] != '-'
}

// Sweep removes spill files older than seven days from <home>/tool-output
// and returns how many it removed. The session calls it when it opens;
// nothing sweeps on a timer. It opens the directory as OpenSpill does, so a
// symlinked or swapped directory is an error and nothing outside it is
// listed or removed. It removes only regular files named like its own,
// never follows a symlink, and treats a file that vanishes under it —
// another session sweeping too — as no error. The first other error is
// returned after every entry has been tried.
func Sweep(home string, now time.Time) (int, error) {
	root, err := openSpillDir(home, false)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, nil
	case err != nil:
		return 0, err
	}
	defer root.Close()
	d, err := root.Open(".")
	if err != nil {
		return 0, fmt.Errorf("tool: %w", err)
	}
	entries, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return 0, fmt.Errorf("tool: %w", err)
	}
	cutoff := now.Add(-spillRetention)
	removed := 0
	var first error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), spillPrefix) {
			continue
		}
		// Through the Root, not the DirEntry: its Info would lstat by path.
		fi, err := root.Lstat(e.Name())
		if err != nil || !fi.Mode().IsRegular() || !fi.ModTime().Before(cutoff) {
			continue
		}
		err = root.Remove(e.Name())
		switch {
		case err == nil:
			removed++
		case errors.Is(err, fs.ErrNotExist):
		case first == nil:
			first = fmt.Errorf("tool: %w", err)
		}
	}
	return removed, first
}
