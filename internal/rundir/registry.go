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

	"github.com/charliek/craze/internal/protocol"
)

// Entry is a host's registry entry, hosts/<hostId>.json: what a resolver
// reads to find a session without connecting to anything (plan 027 §3.8).
// shed reads these members, so the set is a one-way door: none is renamed or
// removed.
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
	sockID   fileID
	hasSock  bool
	entry    Entry
	entryID  fileID
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
//  2. hosts/<hostID>.lock is opened without truncating, flocked without
//     blocking, then truncated to "<pid> <hostId>";
//  3. the socket is bound at <base>/<ns>/<hostID>.sock — no probe and no
//     unlink, the id is fresh — and the listener is told at once never to
//     unlink it (Close's guarded unlink is the only one);
//  4. its (dev, ino) is recorded, and it is chmod-ed 0600;
//  5. the registry entry is written (0600) and its (dev, ino) recorded.
//
// entry's Protocol, HostID, PID and Socket are Bind's to fill. A failure part
// way unwinds what was built, identity-checked, and returns the error.
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
	dir, err := env.socketDir(ns, hostID)
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

// bind is Bind's steps 2–5; whatever it built is on h for teardown.
func (h *Host) bind(entry Entry) error {
	lock, err := openLock(h.hosts, h.lockName())
	if err != nil {
		return fmt.Errorf("rundir: open the host lock: %w", err)
	}
	taken, err := tryLock(lock)
	if err != nil || !taken {
		_ = lock.Close() // not ours: never unlinked
		if err == nil {
			err = errors.New("another process holds it")
		}
		return fmt.Errorf("rundir: lock %s: %w", h.hosts.join(h.lockName()), err)
	}
	h.lock = lock
	if err := writeHolder(lock, h.id); err != nil {
		return fmt.Errorf("rundir: write %s: %w", h.hosts.join(h.lockName()), err)
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
		// because nothing else would ever remove it: no registry entry names
		// it for a sweep. The name is safe to trust: it is in <base>/<ns>,
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
// Protocol, HostID, PID and Socket are kept as Bind wrote them. After Close it
// returns ErrClosed and writes nothing. The new file's identity is taken
// from the descriptor it was written through, before the rename, so a rename
// that succeeds always records the file it installed.
func (h *Host) Update(fn func(*Entry)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	e := h.entry
	fn(&e)
	e.Protocol, e.HostID, e.PID, e.Socket = h.entry.Protocol, h.entry.HostID, h.entry.PID, h.entry.Socket
	return h.writeEntry(e)
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
func unlinkIfOurs(path string, want fileID) error {
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
