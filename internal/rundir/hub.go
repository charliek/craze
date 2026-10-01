package rundir

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/charliek/craze/internal/protocol"
)

// The hub's files (plan 032 §3.4, P2). One hub runs per user, HOME and
// CRAZE_HOME namespace (Namespace). Its lock and record live in the cache
// tree beside the registry, where an SSH exec finds them by HOME alone; its
// socket lives in its namespace's runtime directory, beside its hosts':
//
//   - <Home>/.cache/craze/hubs/<ns>.lock is the hub's lifetime flock
//     (LockHub). It is taken without blocking before anything else the hub
//     does, its holder line "<pid> <hubId>\n" written at once, and held until
//     the hub has removed its record and socket. It is never unlinked: prox
//     unlinked its pidfile after unlocking it, and a second instance that had
//     opened the old file by then locked an inode nobody else could see, so
//     two daemons ran; roost never unlinks its lock. A stale lock file is
//     harmless — the flock, not the file, is the hub's liveness.
//   - <Home>/.cache/craze/hubs/<ns>.json is the hub's record (HubRecord):
//     where its socket is and who it is, written by the lock's holder alone
//     (HubLock.WriteRecord), atomically, and removed by it only while it is
//     still the file it wrote. A reader (ReadHubRecord) gets the record with
//     its (dev, ino).
//   - <base>/<ns>/hub.sock is the hub's socket (HubSocket): the base its
//     namespace's hosts choose, measured for the longest socket it holds.
//
// The hubs tree takes the rest of the cache tree's rules (cacheDir), the
// euid's own group-writable ancestor included — a user-private group's 0775
// ~/.cache under umask 002, which the registry and the session locks accept
// too — so a hub starts on such a system as its hosts do. LockHub checks after
// its flock that the lock's name still names the file it locked, and refuses
// otherwise: a lock replaced at its name between its open and its flock is
// nobody's singleton.
//
// Residual (accepted): a member of the user's group who can rename an
// ancestor of the cache tree — ~/.cache, made 0775 — can move ~/.cache/craze
// aside while a hub holds its lock, so a second hub makes a fresh hubs/ and
// locks another inode at the same name: a second hub lock for the namespace.
// Held by descriptor, the first hub does not notice. The second hub would also
// need another CRAZE_RUNTIME_DIR to bind its socket: the shared runtime base
// is validated by the strict rule (no group-writable ancestor), and the live
// hub's hub.sock there refuses a second bind (ClearStaleSocket leaves a live
// socket). And with a user-private group the group has no other member.
//
// The lock is a singleton on one machine only: flock on a network filesystem
// (an NFS home whose client handles flock locally) does not exclude another
// machine, which can run its own hub under the same HOME and namespace. Its
// hub would rewrite the record; each machine's hub would serve its own hosts.
//
// The lock's descriptor is O_CLOEXEC (openat's), so no child the hub spawns
// inherits it, and Release unlocks explicitly before it closes: a lock
// belongs to the open file description every inherited descriptor shares.

// ErrReleased is HubLock's error once its lock has been released.
var ErrReleased = errors.New("rundir: the hub lock is released")

// hubLockTaken runs in LockHub between the flock and the check that the lock
// is still the file at its name, before the holder line's write: nothing in
// production, and in a test (never in parallel) the pause that lets a loser
// read a winner that has not written its line yet, or the lock's replacement
// at its name.
var hubLockTaken = func() {}

// NewHubID mints a hub id: 12 random lowercase hex digits, never reused —
// minted as a host id is (NewHostID), and checked as one (ValidHostID).
func NewHubID() string { return NewHostID() }

// HubSocket validates the runtime tree for env's namespace — the base its
// hosts choose (socketBase) and <base>/<ns>, both leaves made 0700 where
// missing — and returns the hub's socket path in it, <base>/<ns>/hub.sock,
// canonical. Nothing is bound, probed or removed (ClearStaleSocket).
func HubSocket(env Env) (string, error) {
	ns, err := Namespace(env.CrazeDir)
	if err != nil {
		return "", err
	}
	dir, err := env.socketDir(ns)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, hubSocketName), nil
}

// HubRecord is the hub's record, hubs/<ns>.json: how a client finds the
// namespace's hub, and who it is.
type HubRecord struct {
	// Protocol is the control protocol the hub speaks
	// (protocol.ProtocolVersion).
	Protocol int `json:"protocol"`
	// HubID is the hub's id: 12 lowercase hex digits, never reused, and its
	// hello's endpoint.hostId.
	HubID string `json:"hubId"`
	// NS is the namespace (Namespace) the hub serves, the record's name.
	NS string `json:"ns"`
	// PID is the hub process's pid.
	PID int `json:"pid"`
	// StartToken is the hub process's start token (StartToken): its start
	// time scoped to its boot and PID namespace; with PID, what tells the hub
	// from a later process given its pid, in this boot or another
	// (CarriesStartToken).
	StartToken string `json:"startToken"`
	// Socket is the hub's socket's absolute, canonical path (HubSocket).
	Socket string `json:"socket"`
	// CrazeVersion is the hub binary's version.
	CrazeVersion string `json:"crazeVersion"`
	// StartedAt is when the hub started.
	StartedAt time.Time `json:"startedAt"`
}

// HubHeldError is LockHub's refusal: another process holds the namespace's
// hub lock. PID and HubID are what its holder line names, both zero while
// that process has not written the line yet (the instant after its flock) —
// and when the line names a pid no process has, the line of a hub that died
// holding the lock, which the next holder has not overwritten yet. The lock
// is held all the same. Find it with errors.As.
type HubHeldError struct {
	NS    string
	PID   int
	HubID string
}

func (e *HubHeldError) Error() string {
	pid := "?"
	if e.PID > 0 {
		pid = strconv.Itoa(e.PID)
	}
	s := "rundir: the hub of namespace " + e.NS + " is already running (pid " + pid
	if e.HubID != "" {
		s += ", hub " + e.HubID
	}
	return s + ")"
}

// HubLock is a hub's hold on its namespace: the open, flocked hubs/<ns>.lock,
// and the hubs directory held by descriptor from LockHub to Release, so that
// the record is written and removed relative to the directory whose lock was
// taken, wherever it has been moved since.
type HubLock struct {
	ns   string
	id   string
	path string // the lock's, canonical; for messages

	mu     sync.Mutex
	hubs   *dir     // nil once released
	f      *os.File // nil once released
	rec    HubRecord
	recID  FileID
	hasRec bool
}

// LockHub takes the hub lock of env's namespace, hubs/<ns>.lock, without
// blocking, and writes "<pid> <hubID>\n" into it. The cache tree is walked,
// validated and held as the registry is (cacheDir), the hubs directory made
// 0700 where it is missing, and the lock file opened relative to it,
// created 0600 when missing (openLock) and never truncated before its flock.
// A lock another process holds is a *HubHeldError naming the holder its line
// names. Once flocked, the lock must still be the file at its name
// (dir.sameFile): one unlinked or replaced since it was opened is nobody's
// singleton, and is refused. hubID must be a hub id (NewHubID).
func LockHub(env Env, hubID string) (*HubLock, error) {
	if !ValidHostID(hubID) {
		return nil, fmt.Errorf("rundir: hub id %q is not 12 lowercase hex digits", hubID)
	}
	ns, err := Namespace(env.CrazeDir)
	if err != nil {
		return nil, err
	}
	hubs, err := env.cacheDir(hubsName, true)
	if err != nil {
		return nil, err
	}
	name := ns + ".lock"
	path := hubs.join(name)
	f, err := openLock(hubs, name)
	if err != nil {
		_ = hubs.close()
		return nil, fmt.Errorf("rundir: open the hub lock: %w", err)
	}
	taken, err := tryLock(f)
	if err != nil || !taken {
		var holder Holder
		if err == nil {
			holder = readHolder(f)
			// A hub that died holding the lock left its line complete, and
			// the next holder truncates it only after its own flock: a loser
			// in between reads the dead one's. A pid no process has is
			// nobody's.
			if holder.PID > 0 && !alive(holder.PID) {
				holder = Holder{}
			}
		}
		_ = f.Close() // not ours: never unlinked
		_ = hubs.close()
		if err != nil {
			return nil, fmt.Errorf("rundir: lock %s: %w", path, err)
		}
		return nil, &HubHeldError{NS: ns, PID: holder.PID, HubID: holder.HostID}
	}
	hubLockTaken()
	fail := func(err error) (*HubLock, error) {
		_ = unlock(f)
		_ = f.Close()
		_ = hubs.close()
		return nil, err
	}
	if !hubs.sameFile(f, name) {
		return fail(fmt.Errorf("rundir: %s is no longer the hub lock just taken; another hub may hold the one there now", path))
	}
	if err := writeHolder(f, hubID); err != nil {
		return fail(fmt.Errorf("rundir: write %s: %w", path, err))
	}
	return &HubLock{ns: ns, id: hubID, path: path, hubs: hubs, f: f}, nil
}

// ID is the hub id the lock's holder line names.
func (l *HubLock) ID() string { return l.id }

// Namespace is the namespace the lock is for.
func (l *HubLock) Namespace() string { return l.ns }

// Path is the lock file's canonical path.
func (l *HubLock) Path() string { return l.path }

// RecordPath is the record's canonical path, beside the lock.
func (l *HubLock) RecordPath() string { return filepath.Join(filepath.Dir(l.path), l.recordName()) }

func (l *HubLock) recordName() string { return l.ns + ".json" }

// WriteRecord writes r as the namespace's hub record, hubs/<ns>.json,
// atomically and relative to the held hubs directory (dir.replace: a
// temporary, then renameat), mode 0600, and records the new file's
// (dev, ino), so RemoveRecord and RecordLost always name the file last
// written. Protocol, HubID, NS, PID and StartToken are WriteRecord's to fill
// (this process's); Socket must be absolute, and a zero StartedAt is now. It
// answers the record as written. After Release it returns ErrReleased and
// writes nothing.
//
// A start token needs a trustworthy scope (StartToken: Linux's NStgid check,
// macOS's boot session UUID). Where there is none, the record is written all
// the same, with an empty StartToken — a hub that cannot prove its identity
// still registers — and no reader carries an empty token
// (CarriesStartToken), so such a hub is never terminated on its record's
// word, only reported. The caller tells it by the record answered's empty
// StartToken, and logs it (StartToken's error says why).
func (l *HubLock) WriteRecord(r HubRecord) (HubRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return HubRecord{}, ErrReleased
	}
	if !filepath.IsAbs(r.Socket) {
		return HubRecord{}, fmt.Errorf("rundir: the hub's socket %q is not absolute", r.Socket)
	}
	token, err := StartToken(os.Getpid())
	if err != nil {
		token = ""
	}
	r.Protocol = protocol.ProtocolVersion
	r.HubID = l.id
	r.NS = l.ns
	r.PID = os.Getpid()
	r.StartToken = token
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now().UTC()
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return HubRecord{}, err
	}
	id, err := l.hubs.replace(l.recordName(), append(b, '\n'), 0o600)
	if err != nil {
		return HubRecord{}, fmt.Errorf("rundir: write the hub record: %w", err)
	}
	l.rec, l.recID, l.hasRec = r, id, true
	return r, nil
}

// RecordLost says why the record WriteRecord wrote is no longer where a
// client looks for it — gone from its path, or another file there now — or
// "" while it is (plan 032 §3.5's Lost): compared by (dev, ino), and looked up
// by the path the hubs directory had when it was validated, as Host.Lost
// looks up its entry, so a directory moved away has lost the record for
// every reader. A stat that fails for another reason reports nothing, and so
// does a lock with no record written, or released.
func (l *HubLock) RecordLost() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil || !l.hasRec {
		return ""
	}
	p := l.hubs.join(l.recordName())
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "its record " + p + " is gone"
	case err == nil && idOf(fi) != l.recID:
		return "its record " + p + " is another file now"
	}
	return ""
}

// RemoveRecord removes the record WriteRecord wrote, relative to the held
// hubs directory, only while hubs/<ns>.json is still that file (fstatat,
// then unlinkat): a record put in its place since — another hub's, after
// this one lost it — is left alone, as is one already gone. Removing nothing
// is no error, and neither is a second call.
func (l *HubLock) RemoveRecord() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.removeRecord()
}

func (l *HubLock) removeRecord() error {
	if l.hubs == nil || !l.hasRec {
		return nil
	}
	l.hasRec = false
	return l.hubs.unlinkIfOurs(l.recordName(), l.recID)
}

// Release lets the namespace go, and is idempotent: the record, if one is
// still the file WriteRecord wrote, is removed (RemoveRecord) — so the
// record is gone before the lock is free, whatever the caller did first —
// then the holder line is cleared, then LOCK_UN, then the lock and the hubs
// directory are closed. The lock file is never unlinked.
func (l *HubLock) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	errs := []error{l.removeRecord()}
	f := l.f
	l.f = nil
	// Cleared under the lock, so the next holder's instant between its flock
	// and its write reads no holder, never this process's pid.
	_ = f.Truncate(0)
	errs = append(errs, unlock(f), f.Close(), l.hubs.close())
	l.hubs = nil
	return errors.Join(errs...)
}

// hubRecordMax bounds a hub record read: a real one is a few hundred bytes.
const hubRecordMax = 64 << 10

// ReadHubRecord reads env's namespace's hub record, hubs/<ns>.json, from the
// cache tree walked and held as the registry is (nothing is created), and
// answers it with the (dev, ino) of the very file it read (fstat-ed on the
// descriptor read through). A missing tree or record is an error wrapping
// fs.ErrNotExist. A record that does not name a hub of this namespace — no
// hub id, no pid, another namespace — is an error, not a record.
func ReadHubRecord(env Env) (HubRecord, FileID, error) {
	ns, err := Namespace(env.CrazeDir)
	if err != nil {
		return HubRecord{}, FileID{}, err
	}
	hubs, err := env.cacheDir(hubsName, false)
	if err != nil {
		return HubRecord{}, FileID{}, err
	}
	defer func() { _ = hubs.close() }()
	name := ns + ".json"
	f, err := hubs.openFile(name, os.O_RDONLY, 0)
	if err != nil {
		return HubRecord{}, FileID{}, err
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return HubRecord{}, FileID{}, &fs.PathError{Op: "stat", Path: hubs.join(name), Err: err}
	}
	b, err := io.ReadAll(io.LimitReader(f, hubRecordMax+1))
	if err != nil {
		return HubRecord{}, FileID{}, err
	}
	if len(b) > hubRecordMax {
		return HubRecord{}, FileID{}, fmt.Errorf("rundir: %s is over %d bytes", hubs.join(name), hubRecordMax)
	}
	var r HubRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return HubRecord{}, FileID{}, fmt.Errorf("rundir: %s: %w", hubs.join(name), err)
	}
	if !ValidHostID(r.HubID) || r.PID <= 0 || r.NS != ns {
		return HubRecord{}, FileID{}, fmt.Errorf("rundir: %s names no hub of namespace %s", hubs.join(name), ns)
	}
	return r, idOfStat(&st), nil
}

// tokenScope is the scope a start time is meaningful in — the boot, and the
// PID namespace its pid is a number in — read from the kernel
// (processTokenScope); a test replaces it (never in parallel) to play another
// boot or namespace.
var tokenScope = processTokenScope

// startTokenVersion begins every start token, so a token of any other shape —
// plan 032's first, a bare start time — is never carried.
const startTokenVersion = "st1"

// StartToken is pid's start token, opaque to a reader, which only compares it
// (CarriesStartToken): "st1/<boot>/<pid namespace>/<start>". The start is
// the process's start time as the kernel keeps it (ProcessIdentity, the
// identity the spawner's agent-group records use), which tells it from a
// later process given its pid — but only within a boot, as a Linux start time
// counts ticks since boot and the record that carries the token outlives a
// reboot in ~/.cache, and only within a PID namespace, the one the pid is a
// number in. So the token carries both (tokenScope: Linux's boot_id and
// /proc/self/ns/pid, with /proc checked to be that namespace's; macOS's boot
// session UUID). A pid no process has is ErrNoProcess; a scope that cannot be
// read is an error, and no token.
func StartToken(pid int) (string, error) {
	boot, ns, err := tokenScope()
	if err != nil {
		return "", fmt.Errorf("rundir: a start token's scope: %w", err)
	}
	id, err := ProcessIdentity(pid)
	if err != nil {
		return "", err
	}
	return startTokenVersion + "/" + boot + "/" + ns + "/" + strconv.FormatUint(id.Start, 10), nil
}

// CarriesStartToken reports whether the process with this pid now, in this
// boot and this process's PID namespace, is the one token was taken from
// (StartToken): the token must be exactly the one StartToken makes now. So it
// is false when no process has the pid; when the one that has it started at
// another instant — a later process given a reused pid; when the token was
// taken in another boot or another PID namespace, whatever their pids and
// start times; for a token of any other shape (an older one, a bare start
// time); and whenever it cannot be told (an empty token, a pid that cannot be
// one, an identity or a scope that cannot be read). False is the side that
// never signals a stranger: P17 terminates a wedged hub only while its pid
// carries its record's token. What stays open is the instant between this
// read and a signal (plan 030 X22's residual).
func CarriesStartToken(pid int, token string) bool {
	if pid <= 0 || token == "" {
		return false
	}
	now, err := StartToken(pid)
	return err == nil && now == token
}

// SocketID is the (dev, ino) of the socket at path, lstat-ed: never through a
// link; anything but a socket is an error. The hub records its socket's
// identity right after binding it, as Bind records a host's.
func SocketID(path string) (FileID, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return FileID{}, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	if !isSocketStat(&st) {
		return FileID{}, fmt.Errorf("rundir: %s is not a socket", path)
	}
	return idOfStat(&st), nil
}

// UnlinkIfOurs removes the runtime-tree file at path — a socket — only while
// it is still the file whose identity was recorded (SocketID): a file put in
// its place since is left alone, as is a path already gone.
func UnlinkIfOurs(path string, want FileID) error { return unlinkIfOurs(path, want) }

func isSocketStat(st *unix.Stat_t) bool { return uint32(st.Mode)&unix.S_IFMT == unix.S_IFSOCK }

// SocketFate is what ClearStaleSocket found at a socket's path, and did.
type SocketFate int

const (
	// SocketAbsent: nothing is at the path (or it went before the unlink).
	SocketAbsent SocketFate = iota
	// SocketRemoved: a socket that refused a connection, unlinked.
	SocketRemoved
	// SocketLive: a socket that did not refuse — it connected, or failed
	// any other way (EAGAIN, a timeout) — or one that could not be told.
	SocketLive
	// SocketNotSocket: something other than a socket; never removed.
	SocketNotSocket
	// SocketReplaced: a refusing socket was replaced, at its path, between
	// the probe and the unlink; what is there now is left.
	SocketReplaced
)

func (f SocketFate) String() string {
	switch f {
	case SocketAbsent:
		return "absent"
	case SocketRemoved:
		return "removed"
	case SocketLive:
		return "live"
	case SocketNotSocket:
		return "not a socket"
	case SocketReplaced:
		return "replaced"
	}
	return "SocketFate(" + strconv.Itoa(int(f)) + ")"
}

// socketProbeTimeout bounds ClearStaleSocket's connect. A Unix socket's
// connect does not wait for an accept: it connects, or fails at once.
const socketProbeTimeout = time.Second

// socketProbed runs in ClearStaleSocket after a probe found a socket refusing
// and before its second lstat: nothing in production, and in a test (never
// in parallel) the replacement of the socket at its path, the instant the
// identity check exists for.
var socketProbed = func(string) {}

// ClearStaleSocket removes the socket at path when it is stale — roost's
// probe-before-unlink (plan 032 §3.5, §3.9): it is lstat-ed (never through a
// link) and its (dev, ino) recorded; anything but a socket is SocketNotSocket
// and left. It is connected to: only ECONNREFUSED is stale — a connection, or
// any other failure (EAGAIN on a full backlog, a timeout, anything else) is
// SocketLive and left, except ENOENT, a path that went while it was probed,
// which another lstat settles: SocketAbsent while nothing is there,
// SocketReplaced for another file. A refusing socket is lstat-ed again, and
// unlinked only while it is still a socket with the identity recorded; one
// replaced since is SocketReplaced and left. The residual (roost's,
// accepted): a socket put at the path between that second lstat and the
// unlink, which nothing short of an unlink-by-inode could exclude — or one
// put there after the first lstat that the filesystem gave the inode just
// freed, which (dev, ino) cannot tell from the first. Both need a process
// binding at that very path, and a host's is named for an id never reused. A
// probe is no authority on its own — a socket between its bind and its
// listen refuses too, and on Darwin a live listener whose backlog is full
// does (SweepOrphans) — so a caller decides first that the path is its to
// clear: the hub's own socket path, under its lock, or the sweep's old,
// unheld host sockets (SweepOrphans). An lstat that fails for another reason
// than the path being gone is an error, the socket left.
func ClearStaleSocket(path string) (SocketFate, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return SocketAbsent, nil
		}
		return SocketLive, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	if !isSocketStat(&st) {
		return SocketNotSocket, nil
	}
	return clearRefused(path, idOfStat(&st))
}

// clearRefused is ClearStaleSocket from its probe on, for the socket at path
// whose (dev, ino) the caller lstat-ed: first. A probe that finds the path
// gone (ENOENT: another cleanup's unlink, or its owner's) is settled by a
// third lstat: nothing there is SocketAbsent; another file there is
// SocketReplaced; the very socket probed — so the connect's ENOENT was not
// about it — is SocketLive. All are left.
func clearRefused(path string, first FileID) (SocketFate, error) {
	switch err := probeSocket(path); {
	case errors.Is(err, syscall.ECONNREFUSED):
	case errors.Is(err, syscall.ENOENT):
		if still, fate, err := settle(path, first); !still {
			return fate, err
		}
		return SocketLive, nil
	default:
		return SocketLive, nil // connected, or failed any other way
	}
	socketProbed(path)
	if still, fate, err := settle(path, first); !still {
		return fate, err
	}
	if err := unlinkFile(path); err != nil {
		return SocketLive, err
	}
	return SocketRemoved, nil
}

// settle lstats path again after a probe of the socket whose identity was
// first: still when it is that socket; otherwise what is there instead —
// SocketAbsent for nothing, SocketReplaced for another file, and SocketLive
// with the error when the lstat fails another way.
func settle(path string, first FileID) (still bool, fate SocketFate, err error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, SocketAbsent, nil
		}
		return false, SocketLive, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	if !isSocketStat(&st) || idOfStat(&st) != first {
		return false, SocketReplaced, nil
	}
	return true, 0, nil
}

// probeSocket connects to the socket at path and closes the connection made
// at once: nil when it connected, else the connect's error, kept whole so
// that ECONNREFUSED (nobody listens) and ENOENT (the path went) are told from
// everything else. A test replaces it (never in parallel) to have the socket
// go or be replaced while it is probed.
var probeSocket = func(path string) error {
	c, err := net.DialTimeout("unix", path, socketProbeTimeout)
	if err == nil {
		_ = c.Close()
	}
	return err
}
