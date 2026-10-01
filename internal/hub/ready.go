package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/rundir"
)

// The hub's ready handshake (plan 032 §3.5), the host's (plan 030 §3.4) with
// a line of its own: a hub its spawner started (Ensure) is handed the write
// end of a pipe as fd 3, CRAZE_READY_FD=3, and HubChildEnv=1, and answers on
// it with exactly one JSON line (ReadyLine), then closes it:
//
//   - ok — {"ok":true,"hubId","ns","socket"} — once its socket is bound and
//     its record written, so a spawner that reads ok dials the socket, and a
//     client that reads the record finds the same hub;
//   - held — {"held":{"pid","hubId"}} — from a hub that lost the namespace's
//     lock to another (rundir.LockHub): the holder its lock's line names,
//     or nothing while the holder has not written its line yet. The loser
//     exits 0, and its spawner waits for the holder's record (Ensure's
//     rendezvous);
//   - not ok — {"ok":false,"error"} — a hub that could not come up, and says
//     why.
//
// The descriptor is close-on-exec from the hub's first instant
// (TakeReadyPipe), so no host the hub spawns holds its spawner's pipe. A hub
// run by hand has no pipe and answers nothing.

// ReadyLine is the one line a spawned hub writes on its ready pipe.
type ReadyLine struct {
	OK bool `json:"ok"`
	// HubID, NS and Socket are an ok hub's: its id (its hello's
	// endpoint.hostId), the namespace it serves — which its spawner checks is
	// its own — and its socket's absolute path.
	HubID  string `json:"hubId,omitempty"`
	NS     string `json:"ns,omitempty"`
	Socket string `json:"socket,omitempty"`
	// Held is a loser's: who holds the namespace's lock.
	Held *ReadyHeld `json:"held,omitempty"`
	// Error is why a hub that is neither could not come up.
	Error string `json:"error,omitempty"`
}

// ReadyHeld is the hub that holds the namespace's lock, as the lock's holder
// line names it: its pid and id, both zero while the holder has not written
// the line (the instant after its flock), and when the line names a pid no
// process has (rundir.HubHeldError).
type ReadyHeld struct {
	PID   int    `json:"pid,omitempty"`
	HubID string `json:"hubId,omitempty"`
}

// invalid says why a decoded line is not a hub's ready line, "" when it is
// one: ok names a hub id, an 8-hex-digit namespace and an absolute socket,
// with no refusal beside them; held names a pid that can be one and a hub id
// in a hub id's form, or neither; any other line says why it is not ok. A
// member it does not know is ignored: the hub may be a newer craze than its
// spawner.
func (l ReadyLine) invalid() string {
	switch {
	case l.OK && (l.Held != nil || l.Error != ""):
		return "ok, and a refusal too"
	case l.OK && !rundir.ValidHostID(l.HubID):
		return fmt.Sprintf("ok names no hub id (%q)", l.HubID)
	case l.OK && !validNS(l.NS):
		return fmt.Sprintf("ok names no namespace (%q)", l.NS)
	case l.OK && !filepath.IsAbs(l.Socket):
		return fmt.Sprintf("ok names no socket (%q)", l.Socket)
	case l.OK:
		return ""
	case l.Held != nil && (l.Held.PID < 0 || l.Held.PID > 1<<31-1):
		return fmt.Sprintf("held names pid %d", l.Held.PID)
	case l.Held != nil && l.Held.HubID != "" && !rundir.ValidHostID(l.Held.HubID):
		return fmt.Sprintf("held names no hub id (%q)", l.Held.HubID)
	case l.Held == nil && l.Error == "":
		return "not ok, and no reason"
	}
	return ""
}

// validNS reports whether ns is a namespace's form: 8 lowercase hex digits
// (rundir.Namespace).
func validNS(ns string) bool {
	if len(ns) != 8 {
		return false
	}
	for i := range len(ns) {
		if c := ns[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// parseReady decodes one ready line (hostspawn.ReadLine's) and holds it to
// invalid: the line, or why it is not one.
func parseReady(raw []byte) (ReadyLine, string) {
	var l ReadyLine
	if err := json.Unmarshal(raw, &l); err != nil {
		return ReadyLine{}, err.Error()
	}
	if why := l.invalid(); why != "" {
		return ReadyLine{}, why
	}
	return l, ""
}

// ReadyPipe is a spawned hub's end of the handshake: one line, then closed.
// Every method is safe on a nil *ReadyPipe — a hub run by hand has none — and
// once the line is sent, or the pipe closed, every later call does nothing.
type ReadyPipe struct {
	mu sync.Mutex
	f  *os.File // nil once sent or closed
}

// TakeReadyPipe is the pipe CRAZE_READY_FD names, nil without one. It runs
// first thing in craze hub: HubChildEnv and CRAZE_READY_FD leave this
// process's environment — which every host it spawns starts from — and the
// descriptor is marked close-on-exec at once. A value that does not name an
// open pipe is an error: the hub would otherwise write its line into whatever
// the number happens to be.
func TakeReadyPipe() (*ReadyPipe, error) {
	_ = os.Unsetenv(HubChildEnv)
	raw, ok := os.LookupEnv(hostspawn.ReadyFDEnv)
	if !ok {
		return nil, nil
	}
	_ = os.Unsetenv(hostspawn.ReadyFDEnv)
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("%s=%q does not name a descriptor", hostspawn.ReadyFDEnv, raw)
	}
	syscall.CloseOnExec(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil || uint32(st.Mode)&syscall.S_IFMT != syscall.S_IFIFO {
		return nil, fmt.Errorf("%s=%d is not a pipe", hostspawn.ReadyFDEnv, fd)
	}
	return &ReadyPipe{f: os.NewFile(uintptr(fd), "craze-hub-ready")}, nil
}

// NewReadyPipe is a ReadyPipe over f, for a caller that made the pipe itself
// (a test).
func NewReadyPipe(f *os.File) *ReadyPipe { return &ReadyPipe{f: f} }

// Send writes line and closes the pipe: an error is a spawner that did not
// take it — gone, or done waiting (a closed read end is EPIPE here, never a
// signal: the runtime raises SIGPIPE only for stdout and stderr).
func (p *ReadyPipe) Send(line ReadyLine) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f == nil {
		return errors.New("the ready line was sent already")
	}
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	_, werr := p.f.Write(append(b, '\n'))
	cerr := p.f.Close()
	p.f = nil
	return errors.Join(werr, cerr)
}

// Close closes the pipe unwritten, if it is still open.
func (p *ReadyPipe) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f != nil {
		_ = p.f.Close()
		p.f = nil
	}
}
