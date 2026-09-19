package opencode

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// The file tools' shared refusals, word for word (plan 019 §3.8, §3.9).
const (
	credentialsText = "Cannot access craze's credentials file"
	changedText     = "File changed on disk since it was read. Read it again before editing."
	markerText      = "This text contains a redaction marker, not the file's real content. Re-read the file around the change and leave the credential untouched."
)

const (
	// newFileMode is a new file's mode. opencode writes with Node's
	// default, 0666 less the umask — 0644 under the usual one; a
	// rename-based write sets the mode itself.
	newFileMode fs.FileMode = 0o644
	// dirMode is what missing parent directories are created with, before
	// atomicfile.Write would create them 0700.
	dirMode fs.FileMode = 0o755
	// keptModeBits are the bits of an existing file's mode a rewrite keeps.
	keptModeBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
	// maxLinks bounds the dangling symlinks realPath follows, as the
	// kernel's own ELOOP limit does.
	maxLinks = 40
)

// failure is a file tool's refusal: the text the model reads, and its class.
type failure struct {
	class tool.ErrorClass
	text  string
}

func (f *failure) Error() string { return f.text }

func fail(class tool.ErrorClass, text string) error { return &failure{class, text} }

// errorResult is the result for a file tool's error: a failure as it says,
// a cancelled call as aborted, and anything else — an OS error, whose text
// names the path and the reason — as a tool error.
func errorResult(err error) tool.Result {
	var f *failure
	switch {
	case errors.As(err, &f):
		return tool.Result{Text: f.text, IsError: true, Class: f.class}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return tool.Result{Text: tool.AbortedText, IsError: true, Class: tool.ClassAborted}
	}
	return tool.Result{Text: err.Error(), IsError: true, Class: tool.ClassToolError}
}

// refuseMarker refuses text holding the redaction marker (plan 019 §3.8).
// The model saw a credential redacted in a file it read; writing that text
// back would put the marker on disk in place of the real key.
func refuseMarker(texts ...string) error {
	for _, s := range texts {
		if strings.Contains(s, redact.Marker) {
			return fail(tool.ClassInvalidInput, markerText)
		}
	}
	return nil
}

// title is a path as a call's card shows it: relative to the workspace, as
// opencode's is relative to its worktree.
func title(env tool.Env, abs string) string {
	if rel, err := filepath.Rel(env.Workspace, abs); err == nil {
		return rel
	}
	return abs
}

// realPath returns abs with every symlink resolved, as far as abs exists. A
// path that exists is filepath.EvalSymlinks's answer. For one that does not,
// the deepest ancestor that exists is resolved and the rest joined on, and a
// dangling symlink is followed to where its target would be: so a write
// through a link creates the link's target and leaves the link, and the
// credentials check sees the file a call would really touch.
func realPath(abs string) (string, error) {
	p := abs
	for range maxLinks {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			return r, nil
		}
		if !missing(err) {
			return "", err
		}
		dir, err := realPath(filepath.Dir(p))
		if err != nil {
			return "", err
		}
		q := filepath.Join(dir, filepath.Base(p))
		info, err := os.Lstat(q)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			// Absent, or not there to resolve: the open that follows
			// reports whatever is wrong with it.
			return q, nil
		}
		link, err := os.Readlink(q)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(dir, link)
		}
		p = link
	}
	return "", &fs.PathError{Op: "resolve", Path: abs, Err: syscall.ELOOP}
}

// missing reports whether err says a path does not exist: it, or a
// directory on the way to it, is absent, or that directory is a file.
func missing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// isCredentials reports whether real, a path realPath resolved, is the
// harness's key file (plan 019 §3.8): by resolved path, or, when real
// exists (info is its), by identity, so a hard link to it is refused too.
func isCredentials(env tool.Env, real string, info fs.FileInfo) bool {
	cred := filepath.Join(env.Home, CredentialsFile)
	if r, err := realPath(cred); err == nil {
		cred = r
	}
	if real == cred {
		return true
	}
	if info == nil {
		return false
	}
	ci, err := os.Stat(cred)
	return err == nil && os.SameFile(info, ci)
}

// openFile opens real for reading without blocking and stats what it
// opened. A FIFO opened plainly waits for a writer, and a device can be read
// forever; neither heeds ctx, and either would hang the turn and Close (plan
// 019 §3.9). The caller checks the type before it reads, on the stat of the
// descriptor itself, so the file cannot be swapped between check and read.
// O_NOCTTY keeps a terminal named here from becoming craze's.
func openFile(real string) (*os.File, fs.FileInfo, error) {
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// The byte order mark, as opencode's util/bom.ts handles it.
const bom = "\uFEFF"

// splitBOM is opencode's Bom.split: s less one leading BOM, and whether it
// had one.
func splitBOM(s string) (string, bool) {
	if rest, ok := strings.CutPrefix(s, bom); ok {
		return rest, true
	}
	return s, false
}

// joinBOM is opencode's Bom.join: s less one leading BOM, with one put back
// when withBOM is set.
func joinBOM(s string, withBOM bool) string {
	s, _ = splitBOM(s)
	if withBOM {
		return bom + s
	}
	return s
}

// target is a file opened for rewriting: the one way the write and edit
// tools replace a file (plan 019 §3.9). From openTarget to release it holds
// craze's path lock on the resolved path, so craze's own calls on the file
// take turns; replace checks the file once more just before its rename,
// which is what catches everyone else.
//
// The use is: openTarget, then read, then replace, then release.
type target struct {
	Path   string // absolute, as the call named it: what results show
	Real   string // symlinks resolved: what is read, locked and replaced
	Exists bool
	Size   int64 // bytes on disk when opened; 0 for a new file

	f      *os.File    // open for reading; nil for a new file
	info   fs.FileInfo // the file as opened; nil for a new file
	unlock func()
}

// beforeCheck, when set, runs just before replace's last check. Tests use it
// to change the file at the worst moment.
var beforeCheck func(real string)

// openTarget resolves abs, waits for craze's lock on the resolved path, and
// opens what is there. It refuses the harness's key file, anything that
// exists and is not a regular file, and a file craze may not write (a
// rename would replace a read-only file that opencode's in-place write
// could not). A path that does not exist is a new file. The caller must
// release a target it got.
func openTarget(ctx context.Context, env tool.Env, abs string) (*target, error) {
	real, err := realPath(abs)
	if err != nil {
		return nil, err
	}
	unlock, err := env.Locks.Lock(ctx, real)
	if err != nil {
		return nil, err
	}
	t := &target{Path: abs, Real: real, unlock: unlock}
	f, info, err := openFile(real)
	switch {
	case missing(err):
		if isCredentials(env, real, nil) {
			t.release()
			return nil, fail(tool.ClassToolError, credentialsText)
		}
		return t, nil
	case err != nil:
		t.release()
		return nil, err
	}
	t.f, t.info, t.Exists, t.Size = f, info, true, info.Size()
	if err := t.check(env); err != nil {
		t.release()
		return nil, err
	}
	return t, nil
}

// check refuses an existing target the file tools may not rewrite.
func (t *target) check(env tool.Env) error {
	switch {
	case isCredentials(env, t.Real, t.info):
		return fail(tool.ClassToolError, credentialsText)
	case t.info.IsDir():
		// opencode's own words (edit.ts:125).
		return fail(tool.ClassToolError, "Path is a directory, not a file: "+t.Path)
	case !t.info.Mode().IsRegular():
		return fail(tool.ClassToolError, "Path is not a regular file: "+t.Path)
	}
	w, err := os.OpenFile(t.Real, os.O_WRONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return err
	}
	return w.Close()
}

// read returns the file's content, or nil for a new file.
func (t *target) read() ([]byte, error) {
	if t.f == nil {
		return nil, nil
	}
	return io.ReadAll(io.NewSectionReader(t.f, 0, math.MaxInt64))
}

// replace writes data to the file: to a temp file beside it, renamed over
// it, so a crash leaves the old content or the new and never half of either.
// A new file's missing parents are created 0755 and the file 0644; an
// existing file keeps its mode, and a symlink stays a symlink because the
// rename lands on its resolved target. Just before the rename the file must
// still be the one openTarget found — same inode, size and modification
// time, or still absent — or nothing is written: the path lock covers only
// craze's own calls, not an editor or a formatter.
//
// The rename gives the file a new inode, so hard links to it keep the old
// content, and ownership, ACLs and extended attributes are not carried over;
// nothing is fsynced. NOTICE records both.
func (t *target) replace(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	mode := newFileMode
	if t.Exists {
		mode = t.info.Mode() & keptModeBits
	} else if err := os.MkdirAll(filepath.Dir(t.Real), dirMode); err != nil {
		return err
	}
	return atomicfile.WriteChecked(t.Real, data, mode, t.unchanged)
}

// unchanged is replace's last check.
func (t *target) unchanged() error {
	if beforeCheck != nil {
		beforeCheck(t.Real)
	}
	now, err := os.Lstat(t.Real)
	switch {
	case !t.Exists && missing(err):
		return nil
	case !t.Exists || err != nil,
		!os.SameFile(now, t.info), now.Size() != t.info.Size(), !now.ModTime().Equal(t.info.ModTime()):
		return fail(tool.ClassToolError, changedText)
	}
	return nil
}

// release closes the file and drops the lock. It is safe to call twice.
func (t *target) release() {
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
	t.unlock()
}
