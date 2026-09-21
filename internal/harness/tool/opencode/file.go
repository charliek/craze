package opencode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
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

// realPath is tool.RealPath: abs with every symlink resolved, as far as abs
// exists, so the credentials check sees the file a call would really touch. It
// lives in the tool package because the mode gate judges Request.Targets with
// the same walk, and the two must agree on what a path means (plan 023 §3.1).
func realPath(abs string) (string, error) { return tool.RealPath(abs) }

// targetPath is realPath for Request.Targets: what an edit-kind call will
// write to, as the gate judges it. A path that will not resolve — a symlink
// loop — is left as the call spelled it: the open is going to fail anyway, and
// a mode gate comparing it against the plan file then refuses rather than
// allows.
func targetPath(abs string) string {
	if real, err := realPath(abs); err == nil {
		return real
	}
	return abs
}

// missing reports whether err says a path does not exist: it, or a
// directory on the way to it, is absent, or that directory is a file.
func missing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// isCredentials reports whether real, a path realPath resolved, is the
// harness's key file (plan 019 §3.8): by name and directory, or, when real
// exists (info is its), by identity, so a hard link to it is refused too.
//
// The name is compared ignoring case and the directory by identity. On a
// case-insensitive file system (macOS's APFS, by default) a file has many
// spellings and a resolved path keeps the one it was given, so while the
// key file does not exist yet, <Home>/PROVIDERS.TOML — or the file under
// another spelling of Home — would otherwise create it. Refusing those
// names on a case-sensitive file system costs nothing.
func isCredentials(env tool.Env, real string, info fs.FileInfo) bool {
	cred := filepath.Join(env.Home, CredentialsFile)
	if r, err := realPath(cred); err == nil {
		cred = r
	}
	if strings.EqualFold(filepath.Base(real), filepath.Base(cred)) && sameDir(filepath.Dir(real), filepath.Dir(cred)) {
		return true
	}
	if info == nil {
		return false
	}
	ci, err := os.Stat(cred)
	return err == nil && os.SameFile(info, ci)
}

// sameDir reports whether a and b name one directory: by identity when both
// exist, so that any two spellings of it agree; by name, ignoring case, when
// neither does; and not when only one does.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ai, errA := os.Stat(a)
	bi, errB := os.Stat(b)
	switch {
	case errA == nil && errB == nil:
		return os.SameFile(ai, bi)
	case errA != nil && errB != nil:
		return strings.EqualFold(a, b)
	}
	return false
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
	data   []byte      // what read returned, which unchanged checks is still there
	isRead bool        // read has returned data
	unlock func()
}

// beforeCheck, when set, runs just before replace's last check, and
// afterReplace just after its rename. Tests use them to change the file, or
// cancel the call, at the worst moments.
var beforeCheck, afterReplace func(real string)

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
	// On a case-insensitive file system two spellings of one file take two
	// locks. Within a session that never matters: edit and write are not
	// Parallel, so Fantasy runs them one at a time and this lock is never
	// contended there. Across sessions, replace's last check is what
	// catches the other writer, as it catches an editor.
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

// read returns the file's content, or nil for a new file. It reads one byte
// more than the size openTarget saw and no further: a file that grew since
// then cannot be written anyway — replace's check refuses it — and reading
// all of it could cost any amount of memory. The caller sees the extra byte
// and can say what it likes about a file that outgrew its own limit.
func (t *target) read() ([]byte, error) {
	if t.f == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.NewSectionReader(t.f, 0, t.Size+1))
	if err != nil {
		return nil, err
	}
	t.data, t.isRead = b, true
	return b, nil
}

// startsWithBOM reports whether the file begins with a byte order mark,
// reading only those bytes: all a rewrite needs of a file too large to
// load.
func (t *target) startsWithBOM() (bool, error) {
	if t.f == nil {
		return false, nil
	}
	head := make([]byte, len(bom))
	n, err := t.f.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return string(head[:n]) == bom, nil
}

// replace writes data to the file: to a temp file beside it, renamed over
// it, so a crash leaves the old content or the new and never half of either.
// A new file's missing parents are created 0755 and the file 0644; an
// existing file keeps its mode, and a symlink stays a symlink because the
// rename lands on its resolved target. Just before the rename the file must
// still be the one openTarget found — same inode, size and modification
// time, and the content read returned, or still absent — or nothing is
// written: the path lock covers only craze's own calls, not an editor or a
// formatter (plan 019 §3.9). What is left is the moment between that check
// and the rename: a write in it is lost, and a rename-based write cannot
// close it.
//
// A cancel is honoured up to the same moment: the context is the last thing
// checked before the rename, so a call cancelled by then writes nothing and
// reports aborted. Once the rename is done the file has changed, and the
// call reports that, whatever the context says after; a call is never
// aborted for a change it made.
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
	err := atomicfile.WriteChecked(t.Real, data, mode, func() error {
		if err := t.unchanged(); err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		return err
	}
	if afterReplace != nil {
		afterReplace(t.Real)
	}
	return nil
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
		!os.SameFile(now, t.info), now.Size() != t.info.Size(), !now.ModTime().Equal(t.info.ModTime()),
		!t.sameContent():
		return fail(tool.ClassToolError, changedText)
	}
	return nil
}

// sameContent reports whether the file still holds what read returned, when
// read was called. The inode, size and time above miss a rewrite in place
// that keeps the size inside one tick of a coarse file-system clock, which
// leaves the modification time as it was; an edit computed from the old
// content would then quietly undo it. The inode is the one opened (checked
// above), so the open descriptor reads the file as it is now.
//
// A caller that did not read the content — write, over a file too large to
// hold — has only the inode, size and time, and NOTICE says so.
func (t *target) sameContent() bool {
	if !t.isRead {
		return true
	}
	now := make([]byte, len(t.data)+1)
	n, err := t.f.ReadAt(now, 0)
	if err != nil && err != io.EOF {
		return false
	}
	return bytes.Equal(now[:n], t.data)
}

// release closes the file and drops the lock. It is safe to call twice.
func (t *target) release() {
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
	t.unlock()
}
