// Package signinlog is the ChatGPT plan's sign-in log (plan 034 §3.3, Q5–Q7;
// D-83): every sign-in attempt and its outcome, from /connect and from craze
// auth login chatgpt, as one JSON line each in <native>/logs/signin.log.
//
// # Value-free, enforced at the write
//
// A record holds what chatgptauth.Event holds — kinds, steps, classes and the
// rest from chatgptauth's fixed sets, HTTP statuses, an allowlisted OAuth code
// or "unrecognised", a port, counts, a duration, the attempt's random id and
// the catalog's client_version pin — plus the time and the surface (tui, cli).
// Never a URL or query, a pasted line, a code, a state, a nonce, a PKCE value,
// the host id, a token, an email, a subject, a client id, an
// error_description, or any error's text. The rule is not left to the
// emitter: Record validates every field against chatgptauth's exported sets
// and bounds, and writes a value outside them as "invalid" — a categorical
// value not in its set, an attempt id that is not 8 lowercase hex digits, a
// client_version that is not dotted numeric, a number out of its range. The
// line is built by an explicit serializer, field by field from validated
// values, never by marshalling a struct that could grow a field.
//
// # Files, and more than one writer
//
// The log is 0600 in <native>/logs, 0700, capped at 1 MiB with one rotation
// to signin.log.1. The TUI, the CLI and a second TUI may all write it, so no
// writer keeps it open or trusts a size it remembers: each record takes the
// lock on <logs>/signin.log.lock (atomicfile.FlockWithin), opens signin.log
// O_APPEND, checks it, rotates it by rename when the line would take it past
// the cap, writes the line in one write(2), closes it and lets go of the lock.
// Everything is reached through an os.Root of the native directory, so no
// path the log opens leaves it. logs/ is refused when it is a symlink, is not
// a directory, is writable by group or others, or is another user's; a file
// is refused when it is a symlink, not a regular file (a FIFO), another
// user's, or has another name (a hard link), and made 0600 when it is wider.
//
// # Never in a sign-in's way
//
// Open touches no file: it answers the Log at once and starts its writer, one
// goroutine per Log, which makes the directories and checks the files as
// every record will (setup) and then writes the queue (plan 034 review r3 #4):
// a file system that hangs — a mount gone away — or a lock another craze
// holds stalls the writer, never the sign-in. Record never blocks and never
// fails: it renders the line and hands it to a bounded queue (64); a full
// queue, or a lock another process holds past its bound — at setup, at the
// first record or at any other — drops the record, and the count of dropped
// records goes with the next record written. A refusal or a failed write
// stops the log for good (Failure says why, and Stopped is closed then, for
// the surface to mention it once, as it happens: a transcript note in the
// TUI, a stderr line in the CLI) — a sign-in is never failed or delayed by
// it. Close flushes the queue for at most a second, and a writer stuck in the
// file system past that is left behind, never waited for.
package signinlog

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/paths"
)

// The surfaces a record comes from: Record's surface.
const (
	SurfaceTUI = "tui"
	SurfaceCLI = "cli"
)

// The log's files, in <native>/logs (paths.LogsName).
const (
	FileName    = "signin.log"
	rotatedName = FileName + ".1"
	lockName    = FileName + ".lock"
)

// DefaultMax is the size signin.log is rotated at: a line that would take it
// past this goes to a fresh file, the old one becoming signin.log.1.
const DefaultMax = 1 << 20

const (
	// queueLen bounds the records waiting for the writer.
	queueLen = 64
	// closeWait bounds Close's flush.
	closeWait = time.Second
	// invalid is what a value outside its set or bounds is written as.
	invalid = "invalid"
)

// The seams: lockWait bounds a record's wait for another writer's lock (a
// record that waits longer is dropped and counted); uid is the user the log's
// directory and files must belong to; now stamps a record. afterLogsCheck runs
// between the look at logs and its open, and beforeCreate before a file is
// created, each nil in production: a test swaps a name there, as another
// process could. Tests change them.
var (
	lockWait       = time.Second
	uid            = os.Getuid
	now            = time.Now
	afterLogsCheck func()
	beforeCreate   func(name string)
)

// Options are a test's: the zero value is production's.
type Options struct {
	// Max is the size signin.log rotates at; 0 is DefaultMax.
	Max int64
	// Stall, when set, holds the writer before it touches a file until it is
	// closed: a file system that hangs (plan 034 review r3 #4), for a test of
	// a surface that must not wait on the log.
	Stall <-chan struct{}
}

// Log is the sign-in log of one native directory, for one process. Its
// methods are safe for concurrent use, and a nil *Log — the log a surface
// could not open — takes every call and does nothing.
type Log struct {
	dir   string
	max   int64
	stall <-chan struct{} // Options.Stall

	// The seams as they stood at open: the writer reads these, never the
	// package variables, which a test may set again while a writer abandoned
	// by Close still runs (Close never joins it).
	lockWait       time.Duration
	uid            func() int
	afterLogsCheck func()
	beforeCreate   func(name string)

	queue chan []byte   // rendered records, waiting for the writer
	ready chan struct{} // closed once the writer's setup is over, passed or not
	done  chan struct{} // closed once the writer has returned

	mu     sync.Mutex // orders Record's send with Close's close of queue
	closed bool

	dropped atomic.Int64
	off     atomic.Bool

	failMu  sync.Mutex
	fail    error
	stopped chan struct{} // closed by disable, with fail set
}

// Open is OpenWith with production's options.
func Open(nativeDir string) (*Log, error) { return OpenWith(nativeDir, Options{}) }

// OpenWith answers nativeDir's sign-in log at once, and starts its writer,
// which Close stops. It touches no file (plan 034 review r3 #4): the writer
// makes the native directory and its logs directory when missing and checks
// the log as every record will — the directory, the lock and signin.log
// itself (setup) — so a refusal, like a write that fails later, stops the log
// on the writer's goroutine and is told by Stopped and Failure, never by
// OpenWith. Its one error is a native directory that is not named.
func OpenWith(nativeDir string, o Options) (*Log, error) {
	if nativeDir == "" {
		return nil, errors.New("signinlog: there is no native directory to keep the sign-in log in")
	}
	if o.Max <= 0 {
		o.Max = DefaultMax
	}
	l := &Log{
		dir: nativeDir, max: o.Max, stall: o.Stall,
		lockWait: lockWait, uid: uid, afterLogsCheck: afterLogsCheck, beforeCreate: beforeCreate,
		queue: make(chan []byte, queueLen), ready: make(chan struct{}), done: make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go l.run()
	return l, nil
}

// Record queues ev, reported by surface (SurfaceTUI or SurfaceCLI), to be
// written. Every field is validated as the line is rendered (render). It never
// blocks: a full queue drops the record and counts it, and a closed or
// stopped log drops it silently.
func (l *Log) Record(ev chatgptauth.Event, surface string) {
	if l == nil || l.off.Load() {
		return
	}
	rec := render(ev, surface, now())
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	select {
	case l.queue <- rec:
	default:
		l.dropped.Add(1)
	}
}

// Failure is why the log stopped — refused as it was set up, or broken by a
// record — or nil while it is writing. It is sticky; the surface mentions it
// once.
func (l *Log) Failure() error {
	if l == nil {
		return nil
	}
	l.failMu.Lock()
	defer l.failMu.Unlock()
	return l.fail
}

// Stopped is closed when the log stops, Failure saying why, and never
// otherwise: a surface waits on it to mention the failure as it happens —
// after the last record of a sign-in too (plan 034 review r3 #8c). A nil
// Log's is nil: it never stops.
func (l *Log) Stopped() <-chan struct{} {
	if l == nil {
		return nil
	}
	return l.stopped
}

// errFlush is Close's answer when the writer had not finished within
// closeWait: the records still queued may be lost.
var errFlush = errors.New("signinlog: the log closed before every record was written")

// Close stops taking records and waits up to a second for the queued ones to
// be written: a writer still at it then — stuck in the file system, or behind
// another craze's lock — is left to finish or not on its own, never joined.
// It is safe to call more than once.
func (l *Log) Close() error { return l.closeWithin(closeWait) }

func (l *Log) closeWithin(d time.Duration) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	close(l.queue)
	l.mu.Unlock()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-l.done:
		return nil
	case <-t.C:
		return errFlush
	}
}

// run is the log's writer: its setup, then each queued record written with
// the count of those dropped before it (write). A lock held past lockWait
// drops the record and counts it — the first record's as any other's (plan
// 034 review r3 #3b); any other failure stops the log (disable).
func (l *Log) run() {
	defer close(l.done)
	if l.stall != nil {
		<-l.stall
	}
	l.setup()
	close(l.ready)
	for rec := range l.queue {
		if l.off.Load() {
			continue
		}
		switch err := l.write(rec); {
		case err == nil:
		case errors.Is(err, atomicfile.ErrLockBusy):
			l.dropped.Add(1)
		default:
			l.disable(err)
		}
	}
}

// setup makes the native directory, when missing, and checks the log as every
// record will (write with no record): a log craze cannot keep stops before
// the first record, so the surface can say so as a sign-in begins. Another
// writer's lock held past lockWait is no refusal — the lock is busy, not
// unsafe — so setup leaves the log on and the first record tries again (r3
// #3b). The native directory is opened as "<dir>/.", so a FIFO put where it
// goes fails the open at once rather than waiting for a writer (r3 #2).
func (l *Log) setup() {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		l.disable(fmt.Errorf("signinlog: %w", err))
		return
	}
	switch err := l.write(nil); {
	case err == nil, errors.Is(err, atomicfile.ErrLockBusy):
	default:
		l.disable(err)
	}
}

// disable stops the log for good, keeping the first reason, and closes
// Stopped once.
func (l *Log) disable(err error) {
	l.off.Store(true)
	l.failMu.Lock()
	defer l.failMu.Unlock()
	if l.fail == nil {
		l.fail = err
		close(l.stopped)
	}
}

// write appends rec, a rendered record, to signin.log under the lock, as one
// line carrying the count of the records dropped before it — taken once the
// lock is held, so the records dropped while this one waited for it ride on
// it too — rotating first when the line would take the file past the cap.
// With rec nil it makes and checks everything and writes nothing (setup).
// Everything is opened afresh, through an os.Root of the native directory,
// and closed before it returns. The native directory is opened as "<dir>/.":
// the kernel takes every name but the last as a directory or fails, so
// whatever has been put where it goes — a FIFO, which an open would wait on —
// fails at once.
func (l *Log) write(rec []byte) error {
	root, err := os.OpenRoot(l.dir + string(os.PathSeparator) + ".")
	if err != nil {
		return fmt.Errorf("signinlog: %w", err)
	}
	defer func() { _ = root.Close() }()
	logs, err := l.openLogs(root)
	if err != nil {
		return err
	}
	defer func() { _ = logs.Close() }()
	lf, err := l.openFile(logs, lockName, os.O_RDWR)
	if err != nil {
		return err
	}
	defer func() { _ = lf.Close() }()
	unlock, err := atomicfile.FlockWithin(lf, l.lockWait)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := l.openFile(logs, FileName, os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return err
	}
	if rec == nil {
		return f.Close()
	}
	b := line(rec, l.dropped.Swap(0))
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("signinlog: %w", err)
	}
	if st.Size() > 0 && st.Size()+int64(len(b)) > l.max {
		_ = f.Close()
		if err := logs.Rename(FileName, rotatedName); err != nil {
			return fmt.Errorf("signinlog: rotating %s: %w", l.path(FileName), err)
		}
		if f, err = l.openFile(logs, FileName, os.O_WRONLY|os.O_APPEND); err != nil {
			return err
		}
	}
	n, err := f.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("signinlog: writing %s: %w", l.path(FileName), err)
	}
	return nil
}

// path is name's path in the logs directory, for messages.
func (l *Log) path(name string) string {
	return filepath.Join(l.dir, paths.LogsName, name)
}

// openLogs opens the logs directory in root, making it 0700 when missing, and
// refuses it when it is a symlink, not a directory, writable by group or
// others, or another user's — as the name looks, and then as the directory
// opened is (plan 034 review r3 #2): the descriptor is checked (checkLogs) and
// must be the directory looked at, and it is what every record's files are
// reached through.
//
// The name is opened as "logs/.": an os.Root opens every name but the last
// with O_DIRECTORY|O_NOFOLLOW, and the last, ".", is that directory itself.
// So whatever was put at the name since the look — a FIFO, which a plain open
// would wait on for a writer forever — fails the open at once (ENOTDIR); a
// symlink there is followed only within the native directory, and only to a
// directory, which the identity check then refuses.
func (l *Log) openLogs(root *os.Root) (*os.Root, error) {
	path := filepath.Join(l.dir, paths.LogsName)
	if err := root.Mkdir(paths.LogsName, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("signinlog: making %s: %w", path, err)
	}
	li, err := root.Lstat(paths.LogsName)
	if err != nil {
		return nil, fmt.Errorf("signinlog: %w", err)
	}
	if err := checkLogs(li, path, l.uid()); err != nil {
		return nil, err
	}
	if l.afterLogsCheck != nil {
		l.afterLogsCheck()
	}
	logs, err := root.OpenRoot(paths.LogsName + "/.")
	if err != nil {
		return nil, fmt.Errorf("signinlog: opening %s: %w", path, err)
	}
	di, err := logs.Stat(".")
	if err == nil {
		err = checkLogs(di, path, l.uid())
	}
	if err == nil && !os.SameFile(li, di) {
		err = fmt.Errorf("signinlog: %s changed as it was opened", path)
	}
	if err != nil {
		_ = logs.Close()
		return nil, err
	}
	return logs, nil
}

// checkLogs refuses fi, the logs directory at path — its name's look, or the
// directory opened — when it is a symlink, not a directory, writable by group
// or others, or another user's.
func checkLogs(fi os.FileInfo, path string, uid int) error {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("signinlog: %s is a symbolic link; the sign-in log is kept only in a directory of its own", path)
	case !fi.IsDir():
		return fmt.Errorf("signinlog: %s is not a directory", path)
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("signinlog: %s is writable by other users (mode %04o)", path, fi.Mode().Perm())
	case owner(fi) != uid:
		return fmt.Errorf("signinlog: %s belongs to another user", path)
	}
	return nil
}

// openTries bounds openFile's opens of a name that keeps going and coming
// back between its two opens.
const openTries = 3

// openFile opens name in logs for flag, creating it 0600 when it does not
// exist, and checks it (checkFile). An existing name is opened without
// O_CREATE, so a symlink there — which an os.Root follows within the native
// directory — creates nothing before it is refused; a missing one is created
// with O_EXCL, which never follows a symlink. A create that finds the name
// made since — another craze making the same file at the same moment — opens
// what it made instead, and checks it as any other (plan 034 review r3 #3a),
// up to openTries times. O_NONBLOCK keeps a FIFO from blocking the open (it
// fails, or is refused as not a regular file), and is cleared for the write.
func (l *Log) openFile(logs *os.Root, name string, flag int) (*os.File, error) {
	path := l.path(name)
	// A symlink is refused by name first, for the message; checkFile refuses
	// one that appears between this look and the open.
	if li, err := logs.Lstat(name); err == nil && li.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("signinlog: %s is a symbolic link", path)
	}
	var f *os.File
	var err error
	for range openTries {
		f, err = logs.OpenFile(name, flag|syscall.O_NONBLOCK, 0)
		if !errors.Is(err, fs.ErrNotExist) {
			break
		}
		if l.beforeCreate != nil {
			l.beforeCreate(name)
		}
		f, err = logs.OpenFile(name, flag|os.O_CREATE|os.O_EXCL|syscall.O_NONBLOCK, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("signinlog: opening %s: %w", path, err)
	}
	if err := checkFile(logs, name, path, f, l.uid()); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := syscall.SetNonblock(int(f.Fd()), false); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("signinlog: %w", err)
	}
	return f, nil
}

// checkFile refuses f, opened as name in logs, unless it is a regular file —
// not a symlink, not a FIFO — that the name still is, the user's own, with no
// other name (nlink 1). A file wider than 0600 is narrowed to it.
func checkFile(logs *os.Root, name, path string, f *os.File, uid int) error {
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("signinlog: %w", err)
	}
	li, err := logs.Lstat(name)
	if err != nil {
		return fmt.Errorf("signinlog: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case li.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("signinlog: %s is a symbolic link", path)
	case !li.Mode().IsRegular() || !fi.Mode().IsRegular():
		return fmt.Errorf("signinlog: %s is not a regular file", path)
	case !os.SameFile(fi, li):
		return fmt.Errorf("signinlog: %s changed as it was opened", path)
	case !ok:
		return fmt.Errorf("signinlog: %s cannot be checked", path)
	case int(st.Uid) != uid:
		return fmt.Errorf("signinlog: %s belongs to another user", path)
	case uint64(st.Nlink) != 1:
		return fmt.Errorf("signinlog: %s has another name (a hard link)", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := f.Chmod(0o600); err != nil {
			return fmt.Errorf("signinlog: %w", err)
		}
	}
	return nil
}

// owner is the user fi belongs to, or -1 when its stat says none.
func owner(fi os.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

// ---------------------------------------------------------------- the record

// The bounds of a record's numbers: a value outside its range is written as
// "invalid".
const (
	maxModels    = 10_000
	maxElapsedMS = 30 * 24 * 60 * 60 * 1000 // 30 days
	maxCount     = 1_000_000_000
)

// timeLayout is a record's time: UTC, to the millisecond, always the same
// width.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// The valid sets, from chatgptauth's own (plan 034 Q7), as the log validates
// against them.
var (
	surfaces      = map[string]bool{SurfaceTUI: true, SurfaceCLI: true}
	kinds         = setOf(chatgptauth.EventKinds())
	steps         = setOf(chatgptauth.Steps())
	classes       = setOf(chatgptauth.Classes())
	modes         = setOf(chatgptauth.Modes())
	reasons       = setOf(chatgptauth.Reasons())
	refusals      = setOf(chatgptauth.Refusals())
	parts         = setOf(chatgptauth.Parts())
	checks        = setOf(chatgptauth.Checks())
	vias          = setOf(chatgptauth.Vias())
	usages        = setOf(chatgptauth.Usages())
	registrations = setOf(chatgptauth.Registrations())
	codes         = setOf(chatgptauth.OAuthCodes())
)

func setOf[T ~string](xs []T) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[string(x)] = true
	}
	return m
}

// render is ev's record without its braces and its dropped count: the time,
// the surface, the attempt and the kind, then each field ev sets, in a fixed
// order, every one validated — a categorical value against its set, the
// attempt id and the client_version against their forms, a number against its
// bounds — and written as "invalid" when it fails. A field the Event leaves
// zero is not written; a model fetch's count always is.
func render(ev chatgptauth.Event, surface string, at time.Time) []byte {
	b := make([]byte, 0, 256)
	b = appendStr(b, "time", at.UTC().Format(timeLayout))
	b = appendStr(b, "surface", member(surface, surfaces))
	b = appendStr(b, "attempt", attemptID(ev.Attempt))
	b = appendStr(b, "event", member(string(ev.Kind), kinds))
	b = appendMember(b, "step", string(ev.Step), steps)
	b = appendNumber(b, "status", int64(ev.Status), 100, 599)
	b = appendMember(b, "code", ev.Code, codes)
	b = appendMember(b, "class", string(ev.Class), classes)
	b = appendMember(b, "mode", string(ev.Mode), modes)
	b = appendMember(b, "reason", string(ev.Reason), reasons)
	b = appendNumber(b, "port", int64(ev.Port), 1, 65535)
	b = appendMember(b, "refusal", string(ev.Refusal), refusals)
	b = appendMember(b, "part", string(ev.Part), parts)
	b = appendMember(b, "check", string(ev.Check), checks)
	b = appendMember(b, "via", string(ev.Via), vias)
	b = appendMember(b, "usage", string(ev.Usage), usages)
	b = appendMember(b, "registration", string(ev.Registration), registrations)
	if ev.Kind == chatgptauth.EventModelsFetched || ev.Models != 0 {
		b = appendInt(b, "models", int64(ev.Models), 0, maxModels)
	}
	if ev.ClientVersion != "" {
		b = appendStr(b, "client_version", clientVersion(ev.ClientVersion))
	}
	if ev.Elapsed != 0 {
		b = appendInt(b, "elapsed_ms", ev.Elapsed.Milliseconds(), 0, maxElapsedMS)
	}
	b = appendNumber(b, "count", int64(ev.Count), 1, maxCount)
	return b
}

// line is a rendered record as the line written: its braces, the count of
// records dropped before it (when there were any), and its newline.
func line(rec []byte, dropped int64) []byte {
	b := make([]byte, 0, len(rec)+32)
	b = append(b, '{')
	b = append(b, rec...)
	if dropped > 0 {
		b = appendInt(b, "dropped", dropped, 1, maxCount)
	}
	return append(b, '}', '\n')
}

// member is v when set holds it, else invalid.
func member(v string, set map[string]bool) string {
	if set[v] {
		return v
	}
	return invalid
}

// attemptID is v when it is an attempt id — 8 lowercase hex digits — else
// invalid.
func attemptID(v string) string {
	if len(v) != 8 {
		return invalid
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return invalid
		}
	}
	return v
}

// clientVersion is v when it is a client_version — 1 to 4 dot-separated parts
// of 1 to 6 digits — else invalid.
func clientVersion(v string) string {
	parts, n := 1, 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c >= '0' && c <= '9':
			n++
			if n > 6 {
				return invalid
			}
		case c == '.' && n > 0 && parts < 4:
			parts, n = parts+1, 0
		default:
			return invalid
		}
	}
	if n == 0 {
		return invalid
	}
	return v
}

// appendMember writes key with v validated against set, when v is not empty.
func appendMember(b []byte, key, v string, set map[string]bool) []byte {
	if v == "" {
		return b
	}
	return appendStr(b, key, member(v, set))
}

// appendNumber writes key with v within [lo, hi], when v is not zero.
func appendNumber(b []byte, key string, v, lo, hi int64) []byte {
	if v == 0 {
		return b
	}
	return appendInt(b, key, v, lo, hi)
}

// appendInt writes key with v, or "invalid" when v is outside [lo, hi].
func appendInt(b []byte, key string, v, lo, hi int64) []byte {
	if v < lo || v > hi {
		return appendStr(b, key, invalid)
	}
	b = appendKey(b, key)
	return strconv.AppendInt(b, v, 10)
}

// appendStr writes key with the string v. v is a validated value — a set's
// member, an id, a version, a time, or "invalid" — so it needs no escaping;
// anything with a byte outside [A-Za-z0-9._:-] is written as "invalid" all the
// same, so no caller's mistake can put free text in a line.
func appendStr(b []byte, key, v string) []byte {
	if !plain(v) {
		v = invalid
	}
	b = appendKey(b, key)
	b = append(b, '"')
	b = append(b, v...)
	return append(b, '"')
}

// plain says v is made of [A-Za-z0-9._:-] alone: nothing JSON would escape,
// and nothing that could be free text.
func plain(v string) bool {
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// appendKey writes key and its colon, after a comma unless it is the first.
func appendKey(b []byte, key string) []byte {
	if len(b) > 0 && b[len(b)-1] != '{' {
		b = append(b, ',')
	}
	b = append(b, '"')
	b = append(b, key...)
	return append(b, '"', ':')
}
