package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// The peer check (plan 027 §3.8, A6): the server runs Options.PeerCheck on
// every accepted connection before a byte of it is read, and a refusal closes
// it unread. These tests install internal/rundir's own check — the real one,
// or the real rule over an injected lookup — on a real server over a real
// Unix socket.

// withPeerCheck installs check as the server's Options.PeerCheck.
func withPeerCheck(check func(*net.UnixConn) (int, int, error)) hostOpt {
	return func(c *hostConfig) { c.opts.PeerCheck = check }
}

// readerGrace is how long an injected lookup, holding the check once the
// client's bytes are in the socket, watches them for a read that must not
// happen. It only gives a reader that should not be running a chance to show
// itself: with the check where it belongs the bytes stay put, and the test
// passes whatever the grace.
const readerGrace = 100 * time.Millisecond

// pending is how many bytes wait unread in uc's receive buffer, up to
// limit+1, peeked (MSG_PEEK) so that none is taken; -1 when the peek fails.
func pending(uc *net.UnixConn, limit int) int {
	raw, err := uc.SyscallConn()
	if err != nil {
		return -1
	}
	buf := make([]byte, limit+1)
	n := -1
	cerr := raw.Control(func(fd uintptr) {
		var err error
		n, _, err = syscall.Recvfrom(int(fd), buf, syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		switch {
		case errors.Is(err, syscall.EAGAIN):
			n = 0
		case err != nil && n <= 0:
			// A failed receive. (An error with bytes peeked is the sender's
			// address failing to decode, which says nothing about them.)
			n = -1
		}
	})
	if cerr != nil {
		return -1
	}
	return n
}

// connNotes closes the journal w — what was noted before is written — and
// returns its control_conn notes' fields, in order.
func connNotes(t *testing.T, w *journal.Writer) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	_ = w.Close(ctx)
	if err := w.WaitFlushed(ctx, journal.MaxSeq); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("waiting for the journal writer to finish: %v", err)
	}
	raw, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	var notes []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("journal line %q: %v", line, err)
		}
		if obj["type"] == "diag" && obj["kind"] == journal.DiagControlConn {
			fields, _ := obj["fields"].(map[string]any)
			notes = append(notes, fields)
		}
	}
	return notes
}

// logged is every line the host's Options.Log has had.
func (h *host) logged() []string {
	h.logs.mu.Lock()
	defer h.logs.mu.Unlock()
	return append([]string(nil), h.logs.lines...)
}

// num is a JSON number decoded into a map as an int, or -1 when absent.
func num(v any) int {
	f, ok := v.(float64)
	if !ok {
		return -1
	}
	return int(f)
}

// refusal is what one refused connection left behind.
type refusal struct {
	line  string           // the refusal's Options.Log line
	notes []map[string]any // every control_conn note in the journal
}

// refuseAHello runs one connection against a host whose peer check is
// rundir's rule for this process's uid over an injected lookup answering
// (pid, uid, lookupErr) — but only once the client has written a whole hello
// line, so there are bytes the server could read, and only after watching
// them stay unread. It fails unless the client reads nothing before the
// close: no reply, EOF or a reset. It checks that the connection was never
// counted while its check ran, that the check saw the whole line still
// unread, that nothing was noted open, that nothing is counted after, and
// that no client id was ever minted on the engine.
func refuseAHello(t *testing.T, pid, uid int, lookupErr error) refusal {
	t.Helper()
	w, lo := testJournal(t)
	line := requestLine(t, "1", protocol.MethodHello, helloParams(nil))
	entered := make(chan struct{})
	written := make(chan struct{})
	peeked := make(chan int, 1)
	lookup := func(uc *net.UnixConn) (int, int, error) {
		close(entered)
		<-written
		n := pending(uc, len(line))
		for end := time.Now().Add(readerGrace); n == len(line) && time.Now().Before(end); n = pending(uc, len(line)) {
			time.Sleep(5 * time.Millisecond)
		}
		peeked <- n
		return pid, uid, lookupErr
	}
	h := newHost(t, withLog(lo), withPeerCheck(rundir.PeerCheckWith(os.Geteuid(), lookup)))
	// Registered after the server's close, so it runs before it: a test that
	// fails with the check held lets it go, or the accept loop — and so the
	// server's close — would wait on it forever.
	t.Cleanup(func() { closeOnce(written) })

	c := h.dial()
	await(t, entered, "the peer check to run")
	if n := h.srv.OpenConns(); n != 0 {
		t.Errorf("%d connections counted while the peer check runs, want 0", n)
	}
	c.write(line)
	closeOnce(written)
	select {
	case n := <-peeked:
		if n != len(line) {
			t.Errorf("the check saw %d bytes unread of the %d-byte hello the client wrote: something read the connection before its check", n, len(line))
		}
	case <-time.After(watchdog):
		t.Fatal("the peer check never answered")
	}

	// One read: the close, as EOF or a reset (the line it left unread), or
	// the first bytes of an answer.
	_ = c.nc.SetReadDeadline(time.Now().Add(watchdog))
	buf := make([]byte, 4096)
	n, err := c.nc.Read(buf)
	if n != 0 {
		t.Fatalf("a refused connection was answered: %q", buf[:n])
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("want the host to close the connection unanswered; read: %v", err)
	}

	r := refusal{line: h.logs.wait(t, "refused")}
	for _, l := range h.logged() {
		if strings.Contains(l, " open") {
			t.Errorf("a refused connection was noted open: %q", l)
		}
	}
	if n := h.srv.OpenConns(); n != 0 {
		t.Errorf("%d connections counted after the refusal, want 0", n)
	}
	if id := h.eng.NewClientID(); id != "c-1" {
		t.Errorf("the engine's first client id is %s, want c-1: the refused connection minted one", id)
	}
	r.notes = connNotes(t, w)
	if len(r.notes) != 1 || r.notes[0]["event"] != "refused" {
		t.Fatalf("control_conn notes %v, want the one refusal", r.notes)
	}
	return r
}

// TestAnotherUIDIsRefusedBeforeAByteIsRead (A6): a peer the lookup names as
// another user is refused at accept, before a byte of its hello is read: no
// reply, no connection opened, no client id minted, and the refusal noted
// with both uids and the pid the lookup named.
func TestAnotherUIDIsRefusedBeforeAByteIsRead(t *testing.T) {
	other := os.Geteuid() + 1
	r := refuseAHello(t, 4242, other, nil)
	want := fmt.Sprintf("peer uid %d, want %d", other, os.Geteuid())
	if !strings.Contains(r.line, want) {
		t.Errorf("the refusal's log line %q does not name both uids (%q)", r.line, want)
	}
	note := r.notes[0]
	if reason, _ := note["reason"].(string); !strings.Contains(reason, want) {
		t.Errorf("the refusal's note gives the reason %q, want both uids (%q)", reason, want)
	}
	if num(note["uid"]) != other || num(note["pid"]) != 4242 {
		t.Errorf("the refusal's note names the peer (pid %v, uid %v), want (4242, %d)", note["pid"], note["uid"], other)
	}
}

// TestAFailedCredentialLookupRejects (A6): a peer whose credentials cannot be
// read is refused exactly as another user is — before a byte is read — and
// the refusal names no pid or uid, only the lookup's failure.
func TestAFailedCredentialLookupRejects(t *testing.T) {
	r := refuseAHello(t, 4242, os.Geteuid(), errors.New("injected: no credentials"))
	if !strings.Contains(r.line, "injected: no credentials") {
		t.Errorf("the refusal's log line %q does not give the lookup's failure", r.line)
	}
	note := r.notes[0]
	if reason, _ := note["reason"].(string); !strings.Contains(reason, "injected: no credentials") {
		t.Errorf("the refusal's note gives the reason %q, want the lookup's failure", reason)
	}
	if _, ok := note["uid"]; ok {
		t.Errorf("a failed lookup's refusal names a uid: %v", note)
	}
	if _, ok := note["pid"]; ok {
		t.Errorf("a failed lookup's refusal names a pid: %v", note)
	}
}

// TestThisUserIsServedAndNamed: with the real check, rundir.PeerCheck(euid),
// this process — the host's own user — is served, and its connection's open
// note (and log line) carries this process's pid and uid.
func TestThisUserIsServedAndNamed(t *testing.T) {
	w, lo := testJournal(t)
	h := newHost(t, withLog(lo), withPeerCheck(rundir.PeerCheck(os.Geteuid())))
	a := h.dial()
	if res := a.sayHello(nil); res.ClientID == "" {
		t.Fatalf("hello answered no client id: %+v", res)
	}
	h.logs.wait(t, "conn 1 open", fmt.Sprintf("pid %d uid %d", os.Getpid(), os.Geteuid()))
	notes := connNotes(t, w)
	if len(notes) == 0 || notes[0]["event"] != "open" {
		t.Fatalf("control_conn notes %v, want the open first", notes)
	}
	if open := notes[0]; num(open["conn"]) != 1 || num(open["pid"]) != os.Getpid() || num(open["uid"]) != os.Geteuid() {
		t.Fatalf("the open note %v, want conn 1, pid %d, uid %d", open, os.Getpid(), os.Geteuid())
	}
}

// TestNoPeerCheckNamesNoPeer: a server with no PeerCheck — PR 1's tests, the
// fake host — serves the connection and notes no pid or uid.
func TestNoPeerCheckNamesNoPeer(t *testing.T) {
	w, lo := testJournal(t)
	h := newHost(t, withLog(lo))
	h.dial().sayHello(nil)
	line := h.logs.wait(t, "conn 1 open")
	if strings.Contains(line, "uid") {
		t.Errorf("an unchecked connection's log line names a uid: %q", line)
	}
	notes := connNotes(t, w)
	if len(notes) == 0 || notes[0]["event"] != "open" {
		t.Fatalf("control_conn notes %v, want the open first", notes)
	}
	if _, ok := notes[0]["uid"]; ok {
		t.Errorf("an unchecked connection's open note names a uid: %v", notes[0])
	}
	if _, ok := notes[0]["pid"]; ok {
		t.Errorf("an unchecked connection's open note names a pid: %v", notes[0])
	}
}
