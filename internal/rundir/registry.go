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
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/charliek/craze/internal/protocol"
)

// Entry is a host's registry entry, hosts/<hostId>.json: what a resolver
// reads to find a session without connecting to anything (plan 027 §3.8).
// shed reads these members, so the set is a one-way door: none is renamed or
// removed. Members may be added; a reader ignores those it does not know.
type Entry struct {
	// Protocol is the control protocol the host speaks
	// (protocol.ProtocolVersion).
	Protocol int `json:"protocol"`
	// HostID is the host's id, the entry's file name.
	HostID string `json:"hostId"`
	// PID is the host process's pid.
	PID int `json:"pid"`
	// StartedAt is when the host started.
	StartedAt time.Time `json:"startedAt"`
	// Socket is the control socket's absolute, canonical path.
	Socket string `json:"socket"`
	// CrazeSessionID is craze's own id for the session.
	CrazeSessionID string `json:"crazeSessionId"`
	// ProviderSessionID is the provider's id for the session, known once the
	// engine is ready ("" before).
	ProviderSessionID string `json:"providerSessionId"`
	// Incarnation is the engine's incarnation (the journal's UUIDv7).
	Incarnation string `json:"incarnation"`
	// Provider is the provider's name.
	Provider string `json:"provider"`
	// Workspace is the session's working directory.
	Workspace string `json:"workspace"`
	// Ready is whether the engine has started.
	Ready bool `json:"ready"`
	// RequestID is the idempotency id of the hub's session.create that
	// spawned this host (plan 032 §3.10), so a hub that restarts still
	// recognises a retried create; "" — and absent from the file — for any
	// other host. Added in plan 032, after the original members.
	RequestID string `json:"requestId,omitempty"`
	// RequestHash is that create's hash of its normalized params, which a
	// retry with the same RequestID must match; "" and absent like RequestID.
	RequestHash string `json:"requestHash,omitempty"`
}

// ErrClosed is Update on a closed Host.
var ErrClosed = errors.New("rundir: the host is closed")

// lstatBound is the lstat of the socket Bind has just bound: os.Lstat, which
// a test replaces (never in parallel) to fail it.
var lstatBound = os.Lstat

// Host is a bound control socket and everything registered for it: the
// listener, the host's lifetime lock (hosts/<hostId>.lock, held until Close)
// and its registry entry. It holds the registry directory open for its life
// (held.go): every rewrite and unlink of its entry and lock is relative to
// that descriptor, never to a path walked again.
type Host struct {
	id     string
	ns     string
	socket string
	ln     *net.UnixListener

	mu       sync.Mutex // serialises rewrites and Close
	closed   bool
	hosts    *dir     // the registry directory; nil once closed
	lock     *os.File // nil once released
	sockID   FileID
	hasSock  bool
	entry    Entry
	entryID  FileID
	hasEntry bool
}

// entryName and lockName are the host's files in the registry directory.
func (h *Host) entryName() string { return h.id + ".json" }
func (h *Host) lockName() string  { return h.id + ".lock" }

// Bind validates both trees, takes the host's lock, binds its socket and
// registers it (plan 027 §3.8):
//  1. the socket base with its <ns>, then the cache tree, are validated (and
//     their leaves created) — the base first, so that a socket path too long
//     for sun_path is refused before either tree is touched — and the
//     registry directory is held from here to Close;
//  2. hosts/<hostID>.lock is opened without truncating, its modification
//     time set to now, flocked without blocking, checked to be still the
//     file at its name (taken afresh when it is not: takeLock), then
//     truncated to "<pid> <hostId>";
//  3. the socket is bound at <base>/<ns>/<hostID>.sock — no probe and no
//     unlink, the id is fresh — and the listener is told at once never to
//     unlink it (Close's guarded unlink is the only one);
//  4. its (dev, ino) is recorded, and it is chmod-ed 0600;
//  5. the registry entry is written (0600) and its (dev, ino) recorded.
//
// entry's Protocol, HostID, PID and Socket are Bind's to fill. Its RequestID
// and RequestHash — a hub-created host's create request (plan 032 §3.10) —
// are the caller's, given here or never: every rewrite keeps them (Update). A
// failure part way unwinds what was built, identity-checked, and returns the
// error.
func Bind(env Env, hostID string, entry Entry) (*Host, error) {
	if !ValidHostID(hostID) {
		return nil, fmt.Errorf("rundir: host id %q is not 12 lowercase hex digits", hostID)
	}
	ns, err := Namespace(env.CrazeDir)
	if err != nil {
		return nil, err
	}
	// The socket base first: it measures the socket path against sun_path
	// before it creates anything, so a path too long is refused with nothing
	// created in either tree.
	dir, err := env.socketDir(ns)
	if err != nil {
		return nil, err
	}
	hosts, err := env.cacheDir(hostsName, true)
	if err != nil {
		return nil, err
	}
	h := &Host{id: hostID, ns: ns, socket: filepath.Join(dir, hostID+sockSuffix), hosts: hosts}
	if err := h.bind(entry); err != nil {
		_ = h.teardown()
		return nil, err
	}
	return h, nil
}

// hostLockOpened runs in Bind between the open of the host's lock (and its
// modification time's refresh) and its flock, with the lock's path: nothing
// in production, and in a test (never in parallel) a sweep in that window.
var hostLockOpened = func(string) {}

// lockTries bounds takeLock's attempts at a lock that is still the file at
// its name once flocked.
const lockTries = 3

// bind is Bind's steps 2–5; whatever it built is on h for teardown.
func (h *Host) bind(entry Entry) error {
	if err := h.takeLock(); err != nil {
		return err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: h.socket, Net: "unix"})
	if err != nil {
		return fmt.Errorf("rundir: bind the control socket: %w", err)
	}
	// UnixListener.Close unlinks the path it bound — a replacement's
	// included — unless told not to. Close's identity check must be the only
	// unlink.
	ln.SetUnlinkOnClose(false)
	h.ln = ln
	fi, err := lstatBound(h.socket)
	if err != nil {
		// Unlinked by name, as there is no identity yet to check it against,
		// because nothing else would ever remove it: no sweep removes a
		// socket. The name is safe to trust: it is in <base>/<ns>,
		// just validated as a leaf — the euid's own directory, mode 0700 — so
		// since ListenUnix made it only this uid (or root) can have put
		// anything else there.
		_ = unlinkFile(h.socket)
		return fmt.Errorf("rundir: stat the control socket: %w", err)
	}
	if fi.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("rundir: %s is not the socket just bound", h.socket)
	}
	h.sockID, h.hasSock = idOf(fi), true
	if err := os.Chmod(h.socket, 0o600); err != nil {
		return fmt.Errorf("rundir: chmod the control socket: %w", err)
	}

	entry.Protocol = protocol.ProtocolVersion
	entry.HostID = h.id
	entry.PID = os.Getpid()
	entry.Socket = h.socket
	return h.writeEntry(entry)
}

// takeLock is Bind's step 2: it takes hosts/<id>.lock in the held registry
// directory and writes the holder line, leaving the lock on h.
//
// The hub's sweep (SweepOrphans) takes a lock with no entry beside it that
// has not been modified for OrphanAge, and unlinks it. A host between its
// open and its flock holds nothing, so the open is followed at once by a
// refresh of the file's modification time through the descriptor (touch):
// a lock file this host reopens — an old one left at a reused --host-id —
// is then as young as one it creates. That is a grace, not a guarantee: a
// host stopped between its open and its flock for longer than OrphanAge, or
// a sweep that read the old time just before the refresh, can still have the
// file unlinked under it, and its flock would then take an inode no reader
// can find. So the name is opened again — the lock made afresh where the
// sweep unlinked it — up to lockTries times, whenever a sweep is seen at
// work:
//   - the exclusive create found the file there, and it was gone by the
//     open that followed (openLock: an error wrapping fs.ErrNotExist);
//   - the flock is refused, and the file refused is no longer the one at its
//     name (dir.sameFile): a sweep holds it, having unlinked it;
//   - the flock is taken, and the file is no longer the one at its name.
//
// A flock refused on the file still at its name is another holder of this
// host's lock, and is refused as before: a host is never bound under a lock
// someone else holds. (So is the instant in which a sweep holds an old lock
// it has not unlinked yet; the host's spawner sees a failed start.)
func (h *Host) takeLock() error {
	path := h.hosts.join(h.lockName())
	for range lockTries {
		lock, err := openLock(h.hosts, h.lockName())
		if errors.Is(err, fs.ErrNotExist) {
			continue // unlinked between the create and the open
		}
		if err != nil {
			return fmt.Errorf("rundir: open the host lock: %w", err)
		}
		// Best effort: a refresh that fails leaves the identity check below.
		_ = touch(lock)
		hostLockOpened(path)
		taken, err := tryLock(lock)
		if err != nil {
			_ = lock.Close()
			return fmt.Errorf("rundir: lock %s: %w", path, err)
		}
		named := h.hosts.sameFile(lock, h.lockName())
		if !taken {
			_ = lock.Close() // not ours: never unlinked
			if named {
				return fmt.Errorf("rundir: lock %s: another process holds it", path)
			}
			continue // held by a sweep that has unlinked it
		}
		if !named {
			_ = unlock(lock)
			_ = lock.Close()
			continue
		}
		h.lock = lock
		if err := writeHolder(lock, h.id); err != nil {
			return fmt.Errorf("rundir: write %s: %w", path, err)
		}
		return nil
	}
	return fmt.Errorf("rundir: lock %s: it was unlinked or replaced under its open or its flock %d times", path, lockTries)
}

// touch sets f's access and modification times to now, through its
// descriptor (futimes).
func touch(f *os.File) error {
	tv := unix.NsecToTimeval(time.Now().UnixNano())
	return unix.Futimes(int(f.Fd()), []unix.Timeval{tv, tv})
}

// writeEntry writes e as the registry entry, atomically and relative to the
// held registry directory (dir.replace), and records the new file's
// (dev, ino), so Close always names the current file. On an error nothing
// was renamed onto the entry, and the recorded entry and identity are
// unchanged.
func (h *Host) writeEntry(e Entry) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	id, err := h.hosts.replace(h.entryName(), append(b, '\n'), 0o600)
	if err != nil {
		return fmt.Errorf("rundir: write the registry entry: %w", err)
	}
	h.entry, h.entryID, h.hasEntry = e, id, true
	return nil
}

// Listener is the bound socket's listener. Closing it never unlinks the
// socket; Close does, identity-checked.
func (h *Host) Listener() *net.UnixListener { return h.ln }

// Socket is the socket's absolute, canonical path.
func (h *Host) Socket() string { return h.socket }

// ID is the host id.
func (h *Host) ID() string { return h.id }

// Namespace is the socket's namespace (Namespace of the craze directory).
func (h *Host) Namespace() string { return h.ns }

// Entry is the registry entry as last written.
func (h *Host) Entry() Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.entry
}

// Update rewrites the registry entry with fn's changes — when the engine
// becomes ready, and when it changes — and records the new file's identity.
// Protocol, HostID, PID and Socket are kept as Bind wrote them, as are
// RequestID and RequestHash: the request that made a host is fixed for its
// life. After Close it returns ErrClosed and writes nothing. The new file's
// identity is taken from the descriptor it was written through, before the
// rename, so a rename that succeeds always records the file it installed.
func (h *Host) Update(fn func(*Entry)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	e := h.entry
	fn(&e)
	e.Protocol, e.HostID, e.PID, e.Socket = h.entry.Protocol, h.entry.HostID, h.entry.PID, h.entry.Socket
	e.RequestID, e.RequestHash = h.entry.RequestID, h.entry.RequestHash
	return h.writeEntry(e)
}

// Lost reports why the host can no longer be found where it registered — its
// socket gone from its path, or its registry entry from the registry — or ""
// while both are the files it made (plan 030 §3.3, SF-66): each is compared
// by (dev, ino) with what Bind and the last rewrite recorded, so a file put in
// either place since — another socket bound at the path, an entry written
// over it — is not the host's either. Both are looked up by path, the entry
// in the registry directory as its path now names it, not through the
// descriptor held since Bind: a registry directory moved away or removed has
// lost the entry for every reader of the registry, whatever that descriptor
// still sees. A stat that fails for any other reason than the file being gone
// reports nothing: only a vanished or replaced file is lost. A closed host
// reports nothing. It serialises with Update, so it never reads the entry
// between a rewrite's rename and the identity it records.
func (h *Host) Lost() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ""
	}
	gone := func(what, path string, want FileID) string {
		fi, err := os.Lstat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return what + " " + path + " is gone"
		case err == nil && idOf(fi) != want:
			return what + " " + path + " is another file now"
		}
		return ""
	}
	if h.hasSock {
		if why := gone("its control socket", h.socket, h.sockID); why != "" {
			return why
		}
	}
	if h.hasEntry && h.hosts != nil {
		if why := gone("its registry entry", h.hosts.join(h.entryName()), h.entryID); why != "" {
			return why
		}
	}
	return ""
}

// Close unregisters the host, and is idempotent: it closes the listener if
// it is still open (which unlinks nothing); unlinks the registry entry and
// then the socket, each only while its (dev, ino) is still what was
// recorded; unlinks the host's lock file while still holding it, then
// LOCK_UN, then close; and closes the registry directory last. The entry and
// the lock are unlinked relative to that directory's descriptor (fstatat,
// then unlinkat), so they are the files in the directory Bind validated,
// wherever it has been moved since; the socket, in the runtime tree, by its
// path. The caller may have closed the listener already (the control
// server's Close does).
func (h *Host) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	return h.teardown()
}

// teardown is Close's work, and Bind's unwinding: each step only for what
// was built.
func (h *Host) teardown() error {
	var errs []error
	if h.ln != nil {
		if err := h.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	if h.hasEntry {
		errs = append(errs, h.hosts.unlinkIfOurs(h.entryName(), h.entryID))
	}
	if h.hasSock {
		errs = append(errs, unlinkIfOurs(h.socket, h.sockID))
	}
	if h.lock != nil {
		// Unlinked while held: host ids are never reused, so nobody but a
		// sweeper ever opens another host's lock, and a sweeper checks that
		// the lock it took is still the file at the name (dir.sameFile).
		if h.hosts.sameFile(h.lock, h.lockName()) {
			errs = append(errs, h.hosts.unlink(h.lockName()))
		}
		errs = append(errs, unlock(h.lock), h.lock.Close())
		h.lock = nil
	}
	if h.hosts != nil {
		errs = append(errs, h.hosts.close())
		h.hosts = nil
	}
	return errors.Join(errs...)
}

// unlinkIfOurs removes path — the runtime tree's socket — only while it is
// still the file whose identity was recorded: a file put in its place since
// is left alone, as is a path already gone.
func unlinkIfOurs(path string, want FileID) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if idOf(fi) != want {
		return nil
	}
	return unlinkFile(path)
}

// entryMax bounds a registry entry read: a real one is a few hundred bytes.
const entryMax = 64 << 10

// readEntry reads the registry entry name in the registry directory d
// (dir.openFile: O_NOFOLLOW, a regular file).
func readEntry(d *dir, name string) (Entry, error) {
	f, err := d.openFile(name, os.O_RDONLY, 0)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, entryMax+1))
	if err != nil {
		return Entry{}, err
	}
	if len(b) > entryMax {
		return Entry{}, fmt.Errorf("%s is over %d bytes", d.join(name), entryMax)
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return Entry{}, fmt.Errorf("%s: %w", d.join(name), err)
	}
	return e, nil
}
