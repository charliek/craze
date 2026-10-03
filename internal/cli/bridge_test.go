package cli

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// craze bridge (plan 027 C14, §3.10): resolution against the registry, the
// one stderr-line error contract for every failure, and the pump itself
// against a real Unix listener.

// -------------------------------------------------------------- resolution

func TestResolveTargetByEachID(t *testing.T) {
	target := rundir.Entry{CrazeSessionID: "craze-1", ProviderSessionID: "prov-1", HostID: "aaaaaaaaaaaa",
		Provider: "cursor", Workspace: "/ws/one"}
	other := rundir.Entry{CrazeSessionID: "craze-2", ProviderSessionID: "prov-2", HostID: "bbbbbbbbbbbb",
		Provider: "grok", Workspace: "/ws/two"}
	entries := []rundir.Entry{target, other}

	for _, id := range []string{"craze-1", "prov-1", "aaaaaaaaaaaa"} {
		got, err := resolveTarget(entries, id)
		if err != nil {
			t.Fatalf("--session %s: %v", id, err)
		}
		if got != target {
			t.Fatalf("--session %s: got %+v, want %+v", id, got, target)
		}
	}
}

func TestResolveTargetNoSessionOfThatID(t *testing.T) {
	entries := []rundir.Entry{{CrazeSessionID: "craze-1", HostID: "aaaaaaaaaaaa"}}
	_, err := resolveTarget(entries, "nope")
	assertBridgeError(t, err, 1, "craze bridge: no session nope")
}

func TestResolveTargetSeveralMatchTheSameID(t *testing.T) {
	// Ids do not collide in practice, but resolveTarget does not assume it:
	// a hostId that happens to equal another entry's craze session id is
	// still an error naming both, never a silent pick of one.
	entries := []rundir.Entry{
		{CrazeSessionID: "x", HostID: "aaaaaaaaaaaa", Provider: "cursor", Workspace: "/a"},
		{CrazeSessionID: "aaaaaaaaaaaa", HostID: "bbbbbbbbbbbb", Provider: "grok", Workspace: "/b"},
	}
	_, err := resolveTarget(entries, "aaaaaaaaaaaa")
	assertBridgeError(t, err, 1,
		"craze bridge: 2 sessions match --session aaaaaaaaaaaa: x (cursor, /a), aaaaaaaaaaaa (grok, /b)")
}

func TestResolveTargetNoFlagZeroHosts(t *testing.T) {
	_, err := resolveTarget(nil, "")
	assertBridgeError(t, err, 1, "craze bridge: no session running")
}

func TestResolveTargetNoFlagOneHost(t *testing.T) {
	entry := rundir.Entry{CrazeSessionID: "craze-1", HostID: "aaaaaaaaaaaa"}
	got, err := resolveTarget([]rundir.Entry{entry}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != entry {
		t.Fatalf("got %+v, want %+v", got, entry)
	}
}

func TestResolveTargetNoFlagSeveralHosts(t *testing.T) {
	entries := []rundir.Entry{
		{CrazeSessionID: "craze-1", HostID: "aaaaaaaaaaaa", Provider: "cursor", Workspace: "/a"},
		{CrazeSessionID: "craze-2", HostID: "bbbbbbbbbbbb", Provider: "grok", Workspace: "/b"},
	}
	_, err := resolveTarget(entries, "")
	assertBridgeError(t, err, 1,
		"craze bridge: 2 sessions running; pass --session <id>: craze-1 (cursor, /a), craze-2 (grok, /b)")
}

func TestResolveTargetEntryIDFallsBackToHostID(t *testing.T) {
	// Before the engine is ready an entry's craze session id is still "":
	// its host id names it instead (§3.8's ordering: the socket is bound
	// before the id is known).
	entry := rundir.Entry{HostID: "aaaaaaaaaaaa", Provider: "cursor", Workspace: "/a"}
	_, err := resolveTarget([]rundir.Entry{entry, entry}, "")
	// Two entries with the same identity collapse to one message mentioning
	// the host id, proving the fallback ran.
	assertBridgeError(t, err, 1,
		"craze bridge: 2 sessions running; pass --session <id>: aaaaaaaaaaaa (cursor, /a), aaaaaaaaaaaa (cursor, /a)")
}

// TestBridgeSessionValidatedBeforeARegistryRead: an invalid --session is
// refused before rundir.Hosts ever reads the registry (§3.10). Proven by
// making a registry read fail hard (HOME cleared, so rundir.Env.Home is "":
// rundir.cacheDir's own error, not "no hosts") and checking runBridge still
// answers with the invalid-token message, not that one.
func TestBridgeSessionValidatedBeforeARegistryRead(t *testing.T) {
	t.Setenv("HOME", "")
	err := runBridge(&cobra.Command{}, "not a valid token!", true)
	assertBridgeError(t, err, 1, `craze bridge: --session "not a valid token!" is not a valid session id`)
}

// TestBridgeExplicitEmptySessionIsInvalid: an explicitly empty --session is
// still an explicit session id, not "no --session given" -- it must fail
// ValidToken like any other invalid id, rather than silently falling through
// to the no-flag resolution and picking the one running host (review items
// 2+5).
func TestBridgeExplicitEmptySessionIsInvalid(t *testing.T) {
	t.Setenv("HOME", "")
	err := runBridge(&cobra.Command{}, "", true)
	assertBridgeError(t, err, 1, `craze bridge: --session "" is not a valid session id`)
}

// TestBridgeFlagNotGivenSkipsValidation: with no --session at all (explicit
// false), the zero-value "" is "no --session", not an invalid one -- it
// reaches resolveTarget's no-flag branch instead of being refused.
func TestBridgeFlagNotGivenSkipsValidation(t *testing.T) {
	t.Setenv("HOME", "")
	err := runBridge(&cobra.Command{}, "", false)
	assertBridgeError(t, err, 1, "craze bridge: rundir: no home directory for the craze cache tree")
}

func assertBridgeError(t *testing.T, err error, wantCode int, wantMsg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want %q", wantMsg)
	}
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("got %v (%T), want an *exitError", err, err)
	}
	if ee.code != wantCode || ee.msg != wantMsg {
		t.Fatalf("got code %d msg %q, want code %d msg %q", ee.code, ee.msg, wantCode, wantMsg)
	}
}

// -------------------------------------------------------- live registry

// bridgeTestEnv is an isolated rundir.Env for a test that binds real hosts
// with rundir.Bind and then drives runBridge (via executeErr) against them:
// HOME and CRAZE_RUNTIME_DIR are set process-wide (t.Setenv), so
// rundir.ProcessEnv() -- what runBridge itself calls -- resolves against the
// same registry and socket base the test's own Bind calls used.
func bridgeTestEnv(t *testing.T) rundir.Env {
	t.Helper()
	env := serveEnv(t)
	t.Setenv("HOME", env.Home)
	t.Setenv("CRAZE_RUNTIME_DIR", env.CrazeRuntimeDir)
	return env
}

// bindLiveHost binds a real host under env (closed at cleanup): a live
// registry entry runBridge's own rundir.Hosts(rundir.ProcessEnv()) call will
// see.
func bindLiveHost(t *testing.T, env rundir.Env, entry rundir.Entry) {
	t.Helper()
	h, err := rundir.Bind(env, rundir.NewHostID(), entry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
}

// TestBridgeControlCharsNeverBreakTheOneLineContract (review item 1, major):
// a workspace path containing control characters must not turn "one craze
// bridge: ... line" into several. Two live hosts with no --session forces
// resolveTarget's several-sessions error, which names both on the one line
// through formatEntries -- exactly the path a workspace value takes -- so
// this proves bridgeLine's sanitizeLine runs on the real end-to-end path
// (Execute's own diagnose), not just against a synthetic string.
func TestBridgeControlCharsNeverBreakTheOneLineContract(t *testing.T) {
	env := bridgeTestEnv(t)
	bindLiveHost(t, env, rundir.Entry{Provider: "cursor", Workspace: "/ws/one", CrazeSessionID: "s-1"})
	bindLiveHost(t, env, rundir.Entry{Provider: "grok", Workspace: "/ws/two\r\nrm -rf /", CrazeSessionID: "s-2"})

	stdout, stderr, code := executeErr([]string{"bridge"})
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if n := strings.Count(stderr, "\n"); n != 1 {
		t.Fatalf("stderr = %q, want exactly one line, got %d newlines", stderr, n)
	}
	if !strings.HasPrefix(stderr, bridgePrefix) {
		t.Fatalf("stderr = %q, want prefix %q", stderr, bridgePrefix)
	}
}

// TestBridgeExplicitEmptySessionRejectedWithALiveHost (review items 2+5,
// major): an explicitly empty --session must not be treated as "no
// --session" and silently connect to the one running host -- it is refused
// before rundir.Hosts is ever consulted, with a live host present to prove
// the bypass really would have had something to connect to.
func TestBridgeExplicitEmptySessionRejectedWithALiveHost(t *testing.T) {
	env := bridgeTestEnv(t)
	bindLiveHost(t, env, rundir.Entry{Provider: "cursor", Workspace: "/ws", CrazeSessionID: "s-1"})

	stdout, stderr, code := executeErr([]string{"bridge", "--session", ""})
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	want := `craze bridge: --session "" is not a valid session id` + "\n"
	if stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

// ------------------------------------------------------------ dial failure

func TestDialAndPumpUnreachableSocket(t *testing.T) {
	dir := shortRuntimeDir(t)
	socket := filepath.Join(dir, "gone.sock")
	err := dialAndPump("craze-1", socket, strings.NewReader(""), &bytes.Buffer{}, rundir.DialCheck(os.Geteuid()))
	if err == nil {
		t.Fatal("got no error dialling a socket that was never bound")
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("got %v, want an *exitError with code 1", err)
	}
	const want = "craze bridge: session craze-1 is unreachable: "
	if !strings.HasPrefix(ee.msg, want) {
		t.Fatalf("got %q, want a prefix of %q", ee.msg, want)
	}
}

// ------------------------------------------------------------------- pump

// pumpListener is a real Unix listener for the pump tests, in a short
// directory (never t.TempDir: sun_path).
func pumpListener(t *testing.T) (*net.UnixListener, string) {
	t.Helper()
	dir := shortRuntimeDir(t)
	path := filepath.Join(dir, "p.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.(*net.UnixListener), path
}

func dialPump(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// recordingWriter is an io.Writer that keeps each Write call's bytes
// separately, so a test can tell a chunk was flushed on its own from one
// that was coalesced with another. onWrite, when set, is notified
// (non-blocking) after each Write is recorded: a deterministic way for
// another goroutine to wait until a chunk has actually reached this writer,
// instead of a sleep and a hope that scheduling did not coalesce two reads.
type recordingWriter struct {
	mu      sync.Mutex
	writes  [][]byte
	onWrite chan struct{}
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	cp := append([]byte(nil), p...)
	w.writes = append(w.writes, cp)
	w.mu.Unlock()
	if w.onWrite != nil {
		select {
		case w.onWrite <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func (w *recordingWriter) all() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []byte
	for _, c := range w.writes {
		out = append(out, c...)
	}
	return out
}

func (w *recordingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.writes)
}

// TestPumpBothDirectionsVerbatim: bytes flow both ways unmodified, binary
// bytes included, and a message the server writes in two separate Write
// calls arrives at stdout as two separate Write calls too — the pump never
// waits to coalesce reads before writing (§3.10). This is proven
// deterministically (review item 7): the server's second write happens only
// once it has confirmation (stdout.onWrite) that the pump's stdout already
// received the first — so the two chunks cannot possibly reach the socket
// close enough together for one Read to collect both, on any schedule, with
// no sleep involved.
func TestPumpBothDirectionsVerbatim(t *testing.T) {
	ln, path := pumpListener(t)
	stdout := &recordingWriter{onWrite: make(chan struct{}, 1)}
	serverGot := make(chan []byte, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			serverGot <- nil
			return
		}
		defer nc.Close()
		buf := make([]byte, 4096)
		var got []byte
		for {
			n, err := nc.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				break
			}
		}
		serverGot <- got
		// Two separate writes; the second is sent only once the first is
		// confirmed to have reached the pump's stdout, so it cannot exist on
		// the wire before that point.
		_, _ = nc.Write([]byte("HELLO "))
		<-stdout.onWrite
		_, _ = nc.Write([]byte("WORLD\x00\x01\x02\n"))
	}()

	conn := dialPump(t, path)
	clientInput := []byte("from the client\x00\x01\xff")
	stdin := bytes.NewReader(clientInput)

	done := make(chan error, 1)
	go func() { done <- pump(stdin, stdout, conn) }()

	// The server closes once it has written both chunks; wait for the pump
	// to see that as a clean socket EOF.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pump: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pump never returned")
	}

	if got := <-serverGot; !bytes.Equal(got, clientInput) {
		t.Fatalf("the server read %q, want %q", got, clientInput)
	}
	if got, want := stdout.all(), []byte("HELLO WORLD\x00\x01\x02\n"); !bytes.Equal(got, want) {
		t.Fatalf("stdout got %q, want %q", got, want)
	}
	if n := stdout.count(); n < 2 {
		t.Fatalf("stdout.Write was called %d times, want at least 2 (one per socket read)", n)
	}
}

// blockingReader never returns until told to, then answers EOF: a stdin
// that is still open when the socket ends.
type blockingReader struct{ unblock chan struct{} }

func (r *blockingReader) Read([]byte) (int, error) {
	<-r.unblock
	return 0, io.EOF
}

// TestPumpSocketEOFExitsAtOnceWithStdinStillOpen: the socket ending is
// the exit, whatever stdin is doing (§3.10) — the pump does not wait on a
// stdin that never reaches EOF.
func TestPumpSocketEOFExitsAtOnceWithStdinStillOpen(t *testing.T) {
	ln, path := pumpListener(t)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = nc.Write([]byte("bye"))
		_ = nc.Close()
	}()

	conn := dialPump(t, path)
	stdin := &blockingReader{unblock: make(chan struct{})}
	t.Cleanup(func() { close(stdin.unblock) })
	stdout := &recordingWriter{}

	done := make(chan error, 1)
	go func() { done <- pump(stdin, stdout, conn) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pump: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pump waited on stdin instead of exiting on the socket's EOF")
	}
	if got := stdout.all(); string(got) != "bye" {
		t.Fatalf("stdout got %q, want %q", got, "bye")
	}
}

// TestPumpStdinEOFHalfClosesAndKeepsReading: stdin's EOF only half-closes
// the socket (CloseWrite); the server sees EOF on its read side, can still
// write, and the pump relays that and exits 0 once the server itself closes
// (§3.10, A18's TestARequestThenEOFStillGetsItsReply).
func TestPumpStdinEOFHalfClosesAndKeepsReading(t *testing.T) {
	ln, path := pumpListener(t)
	serverSawEOF := make(chan bool, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			serverSawEOF <- false
			return
		}
		defer nc.Close()
		buf := make([]byte, 64)
		_, err = nc.Read(buf) // "ping"
		n2, err2 := nc.Read(buf)
		serverSawEOF <- n2 == 0 && errors.Is(err2, io.EOF) && err == nil
		// Still writable after the client's half-close.
		_, _ = nc.Write([]byte("pong"))
		_ = nc.Close()
	}()

	conn := dialPump(t, path)
	stdin := bytes.NewReader([]byte("ping"))
	stdout := &recordingWriter{}

	done := make(chan error, 1)
	go func() { done <- pump(stdin, stdout, conn) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pump: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pump never returned")
	}
	if !<-serverSawEOF {
		t.Fatal("the server never saw a clean EOF on its read side after the client's half-close")
	}
	if got := stdout.all(); string(got) != "pong" {
		t.Fatalf("stdout got %q, want %q", got, "pong")
	}
}

// erroringWriter always fails: a stand-in for stdout once the SSH channel is
// gone (EPIPE), without needing a real broken pipe.
type erroringWriter struct{ err error }

func (w erroringWriter) Write([]byte) (int, error) { return 0, w.err }

// TestPumpStdoutWriteFailureExitsWithAnError: a stdout write failure ends
// the pump with that error — an ordinary Go error the caller turns into
// exit 1, never a signal (§3.10).
func TestPumpStdoutWriteFailureExitsWithAnError(t *testing.T) {
	ln, path := pumpListener(t)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		defer nc.Close()
		_, _ = nc.Write([]byte("data the client cannot write to its stdout"))
		time.Sleep(200 * time.Millisecond)
	}()

	conn := dialPump(t, path)
	stdout := erroringWriter{err: os.ErrClosed}
	err := pump(strings.NewReader(""), stdout, conn)
	if err == nil {
		t.Fatal("got no error from a stdout that always fails to write")
	}
	if !strings.Contains(err.Error(), "write stdout") {
		t.Fatalf("got %v, want it to name the stdout write", err)
	}
}

// ------------------------------------------------- the stdout probe (SF-123)

// probeWait bounds a wait on the probe or the server that is not itself a
// timing assertion: generous, for a starved CPU.
const probeWait = 10 * time.Second

// probeLook is one of the pump's probe's looks at stdout: rundir.PeerGone's
// answer.
type probeLook struct{ gone, supported bool }

// probeWatch is the pump's probe as one test sees it (readerProbed,
// readerProbeEnded): each look on looks, and ended closed once it stopped.
type probeWatch struct {
	looks chan probeLook
	ended chan struct{}
}

// watchProbe installs a probeWatch for one test (never in parallel), the
// seams restored when it ends.
func watchProbe(t *testing.T) *probeWatch {
	t.Helper()
	pw := &probeWatch{looks: make(chan probeLook, 64), ended: make(chan struct{})}
	prevProbed, prevEnded := readerProbed, readerProbeEnded
	var once sync.Once
	readerProbed = func(gone, supported bool) {
		select {
		case pw.looks <- probeLook{gone, supported}:
		default:
		}
	}
	readerProbeEnded = func() { once.Do(func() { close(pw.ended) }) }
	t.Cleanup(func() { readerProbed, readerProbeEnded = prevProbed, prevEnded })
	return pw
}

// look checks the probe's next look found (gone, supported), within
// probeWait.
func (pw *probeWatch) look(t *testing.T, gone, supported bool) {
	t.Helper()
	select {
	case l := <-pw.looks:
		if l.gone != gone || l.supported != supported {
			t.Fatalf("the probe's look found (gone %v, supported %v), want (%v, %v)", l.gone, l.supported, gone, supported)
		}
	case <-time.After(probeWait):
		t.Fatalf("the probe took no look within %v", probeWait)
	}
}

// acceptPump is the server's end of the connection the pump dialled.
func acceptPump(t *testing.T, ln *net.UnixListener) *net.UnixConn {
	t.Helper()
	_ = ln.SetDeadline(time.Now().Add(probeWait))
	sc, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return sc
}

// readToEnd is everything the server reads up to the pump's half-close
// (stdin's EOF), within probeWait.
func readToEnd(t *testing.T, sc *net.UnixConn) string {
	t.Helper()
	_ = sc.SetReadDeadline(time.Now().Add(probeWait))
	b, err := io.ReadAll(sc)
	if err != nil {
		t.Fatalf("the server's read up to the pump's half-close: %v", err)
	}
	return string(b)
}

// stdoutPipe is an os.Pipe for the pump's stdout: closed, both ends, when
// the test ends.
func stdoutPipe(t *testing.T) (r, w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	return r, w
}

// pumpReturns is the pump's error once it returns, within d of now.
func pumpReturns(t *testing.T, done <-chan error, d time.Duration, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("the pump did not return within %v %s", d, what)
		return nil
	}
}

// wantEPIPE checks the server's next write fails with EPIPE: the pump closed
// the socket.
func wantEPIPE(t *testing.T, sc *net.UnixConn) {
	t.Helper()
	_ = sc.SetWriteDeadline(time.Now().Add(probeWait))
	if _, err := sc.Write([]byte("anyone?\n")); !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("the server's write after the pump ended: %v, want EPIPE (the socket closed)", err)
	}
}

// TestPumpEndsOnceStdoutsReaderGoes (plan 035 C9, SF-123): after stdin's
// EOF, a stdout pipe whose reader closes (an SSH connection dropped) ends the
// pump within 3 s of the close though the session says nothing at all: it
// returns errReaderGone, and it closed the socket, so the server's next write
// fails with EPIPE. The probe's first look, before the close, found the
// reader there. The negative control: with the probe off, nothing ever ends
// this pump, and the bound fails.
func TestPumpEndsOnceStdoutsReaderGoes(t *testing.T) {
	pw := watchProbe(t)
	ln, path := pumpListener(t)
	conn := dialPump(t, path)
	r, w := stdoutPipe(t)
	done := make(chan error, 1)
	go func() { done <- pump(strings.NewReader("ping"), w, conn) }()
	sc := acceptPump(t, ln)
	if got := readToEnd(t, sc); got != "ping" {
		t.Fatalf("the server read %q, want %q", got, "ping")
	}
	pw.look(t, false, true)

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	err := pumpReturns(t, done, 3*time.Second, "of its stdout's reader closing")
	if !errors.Is(err, errReaderGone) {
		t.Fatalf("the pump returned %v, want errReaderGone", err)
	}
	wantEPIPE(t, sc)
}

// TestPumpKeepsALiveReaderToTheEnd (plan 035 C9): a stdout pipe whose reader
// is still there keeps the pump past stdin's EOF while the session is quiet,
// the probe looking twice and finding it there each time, and the pump then
// relays what the session says and exits 0 at its end. The negative control:
// a probe that took any revent (POLLOUT, room to write) for a reader gone
// ends the pump at its first look.
func TestPumpKeepsALiveReaderToTheEnd(t *testing.T) {
	pw := watchProbe(t)
	ln, path := pumpListener(t)
	conn := dialPump(t, path)
	r, w := stdoutPipe(t)
	read := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		read <- b
	}()
	done := make(chan error, 1)
	go func() { done <- pump(strings.NewReader("ping"), w, conn) }()
	sc := acceptPump(t, ln)
	if got := readToEnd(t, sc); got != "ping" {
		t.Fatalf("the server read %q, want %q", got, "ping")
	}
	pw.look(t, false, true)
	pw.look(t, false, true)

	if _, err := sc.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	_ = sc.Close()
	if err := pumpReturns(t, done, probeWait, "of the socket's end"); err != nil {
		t.Fatalf("pump: %v", err)
	}
	_ = w.Close()
	if got := <-read; string(got) != "pong" {
		t.Fatalf("stdout's reader got %q, want %q", got, "pong")
	}
}

// TestPumpOnOneSocketAsStdinAndStdout (plan 035 C9, A9): some sshd builds
// hand a command one socket as its stdin and its stdout (here two descriptors
// for it, as fd 0 and fd 1 are). A peer that only shut its writing half
// (SHUT_WR) has ended the bridge's stdin, and still reads: the probe finds it
// there, look after look, and it gets what the session says after. Its close
// then ends the pump within 3 s, errReaderGone, and the server's next write
// fails with EPIPE. The negative controls: a probe that took POLLOUT for a
// reader gone ends the pump before "pong" reaches the peer; one that ignored
// POLLHUP never ends it, and the bound fails.
func TestPumpOnOneSocketAsStdinAndStdout(t *testing.T) {
	pw := watchProbe(t)
	ln, path := pumpListener(t)
	conn := dialPump(t, path)
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	stdin := os.NewFile(uintptr(fds[0]), "stdin")
	t.Cleanup(func() { _ = stdin.Close() })
	dup, err := unix.Dup(fds[0])
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.NewFile(uintptr(dup), "stdout")
	t.Cleanup(func() { _ = stdout.Close() })
	pf := os.NewFile(uintptr(fds[1]), "peer")
	fc, err := net.FileConn(pf)
	_ = pf.Close()
	if err != nil {
		t.Fatal(err)
	}
	peer := fc.(*net.UnixConn)
	t.Cleanup(func() { _ = peer.Close() })

	if _, err := peer.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := peer.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- pump(stdin, stdout, conn) }()
	sc := acceptPump(t, ln)
	if got := readToEnd(t, sc); got != "ping" {
		t.Fatalf("the server read %q, want %q", got, "ping")
	}
	pw.look(t, false, true)
	pw.look(t, false, true)
	if _, err := sc.Write([]byte("pong\n")); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(probeWait))
	got, err := protocol.NewLineReader(peer, 0).ReadLine()
	if err != nil || string(got) != "pong" {
		t.Fatalf("the half-closed peer read %q (%v), want %q", got, err, "pong")
	}

	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	err = pumpReturns(t, done, 3*time.Second, "of its stdin-and-stdout socket's peer closing")
	if !errors.Is(err, errReaderGone) {
		t.Fatalf("the pump returned %v, want errReaderGone", err)
	}
	wantEPIPE(t, sc)
}

// TestPumpDoesNotProbeARegularFile (plan 035 C9): a regular file as stdout
// cannot be probed: the probe's first look says so (supported false) and the
// probe stops there, while the pump goes on to the socket's end and exits 0,
// every byte in the file. The negative controls: a probe that polled a
// regular file anyway (no fstat check) keeps looking and never stops; one
// that took "cannot tell" for "gone" ends the pump.
func TestPumpDoesNotProbeARegularFile(t *testing.T) {
	pw := watchProbe(t)
	ln, path := pumpListener(t)
	conn := dialPump(t, path)
	out := filepath.Join(t.TempDir(), "stdout")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	done := make(chan error, 1)
	go func() { done <- pump(strings.NewReader("ping"), f, conn) }()
	sc := acceptPump(t, ln)
	if got := readToEnd(t, sc); got != "ping" {
		t.Fatalf("the server read %q, want %q", got, "ping")
	}
	pw.look(t, false, false)
	select {
	case <-pw.ended:
	case <-time.After(probeWait):
		t.Fatal("the probe of a regular file did not stop at its first look")
	}
	select {
	case err := <-done:
		t.Fatalf("the pump returned (%v) with the socket still open", err)
	default:
	}

	if _, err := sc.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	_ = sc.Close()
	if err := pumpReturns(t, done, probeWait, "of the socket's end"); err != nil {
		t.Fatalf("pump: %v", err)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "pong" {
		t.Fatalf("the file holds %q (%v), want %q", b, err, "pong")
	}
}

// TestPumpStopsItsProbeWhenItReturns (plan 035 C9): the probe runs only while
// the pump does. A pump that returns, here at the socket's end with stdout's
// reader still there, has stopped its probe by then. The negative control: a
// pump that left its probe running returns with the probe still looking.
func TestPumpStopsItsProbeWhenItReturns(t *testing.T) {
	pw := watchProbe(t)
	ln, path := pumpListener(t)
	conn := dialPump(t, path)
	r, w := stdoutPipe(t)
	go func() { _, _ = io.Copy(io.Discard, r) }()
	done := make(chan error, 1)
	go func() { done <- pump(strings.NewReader("ping"), w, conn) }()
	sc := acceptPump(t, ln)
	if got := readToEnd(t, sc); got != "ping" {
		t.Fatalf("the server read %q, want %q", got, "ping")
	}
	pw.look(t, false, true)
	_ = sc.Close()
	if err := pumpReturns(t, done, probeWait, "of the socket's end"); err != nil {
		t.Fatalf("pump: %v", err)
	}
	select {
	case <-pw.ended:
	default:
		t.Fatal("the pump returned with its probe still running")
	}
}

// TestDialAndPumpPeerCheckRunsBeforeTheFirstByte: a refusing peer check
// closes the connection before pump ever runs, so the accept side reads
// nothing at all (§3.8's "the client checks the server after dial", §3.10).
func TestDialAndPumpPeerCheckRunsBeforeTheFirstByte(t *testing.T) {
	ln, path := pumpListener(t)
	type read struct {
		got []byte
		err error
	}
	reads := make(chan read, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			reads <- read{err: err}
			return
		}
		defer nc.Close()
		_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 4096)
		n, err := nc.Read(buf)
		reads <- read{got: buf[:n], err: err}
	}()

	refuse := func(*net.UnixConn) error { return errors.New("refused: another user") }
	err := dialAndPump("craze-1", path, strings.NewReader("should never be written"), &bytes.Buffer{}, refuse)
	if err == nil {
		t.Fatal("got no error from a refusing peer check")
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 || !strings.Contains(ee.msg, "is unreachable") {
		t.Fatalf("got %v, want a bridge unreachable error", err)
	}

	r := <-reads
	if len(r.got) != 0 {
		t.Fatalf("the listener read %q, want nothing: the peer check must run before the first byte", r.got)
	}
}

// ---------------------------------------------------------- error contract

// executeErr runs the root command with argv and returns what Execute would
// print on stderr and exit with, using diagnose directly so a test checks
// exactly what the real Execute does, without os.Exit.
func executeErr(argv []string) (stdout, stderr string, code int) {
	root := NewRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(argv)
	ranCmd, err := root.ExecuteC()
	if err == nil {
		return out.String(), errBuf.String(), 0
	}
	line, c := diagnose(ranCmd, err)
	stderrOut := errBuf.String()
	if line != "" {
		stderrOut += line + "\n"
	}
	return out.String(), stderrOut, c
}

func TestBridgeUnknownFlagIsOneLineExitOne(t *testing.T) {
	stdout, stderr, code := executeErr([]string{"bridge", "--nope"})
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.HasPrefix(stderr, bridgePrefix) || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("stderr = %q, want exactly one %q line", stderr, bridgePrefix)
	}
}

func TestBridgeExtraArgumentIsOneLineExitOne(t *testing.T) {
	stdout, stderr, code := executeErr([]string{"bridge", "extra"})
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.HasPrefix(stderr, bridgePrefix) || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("stderr = %q, want exactly one %q line", stderr, bridgePrefix)
	}
}

// TestBridgeStrayConfigEnvIsOneLineExitOne: the root's shared
// PersistentPreRunE refuses the removed config-file variable with a usage
// error (exit 2) everywhere else (TestRemovedConfigEnvIsAUsageErrorEverywhere);
// reaching craze bridge, it reads as a bridge error instead — exit 1, one
// line, the "craze: " prefix stripped (§3.10: a stray removed config-file
// variable in an SSH environment therefore still reads as a bridge error).
func TestBridgeStrayConfigEnvIsOneLineExitOne(t *testing.T) {
	removed := "CRAZE_" + "CONFIG"
	t.Setenv(removed, filepath.Join(t.TempDir(), "config.toml"))
	stdout, stderr, code := executeErr([]string{"bridge"})
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	want := bridgePrefix + removed + " is no longer supported; unset it and set CRAZE_HOME to the directory that holds config.toml instead (it defaults to ~/.craze)\n"
	if stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

func TestBridgeHelpIsNotAFailure(t *testing.T) {
	stdout, stderr, code := executeErr([]string{"bridge", "--help"})
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "craze bridge") {
		t.Fatalf("stdout = %q, want usage naming the command", stdout)
	}
}

// -------------------------------------------------- short ids (SF-115, P6)

// TestMatchSessionTiers is plan 035 P6: the first tier that matches decides —
// exact on any field of any entry, then a craze-id suffix, a craze-id prefix,
// a host-id prefix — and within it two entries are ambiguous.
func TestMatchSessionTiers(t *testing.T) {
	a := rundir.Entry{CrazeSessionID: "0193aaaa-1111-7000-8000-00000000abcd", ProviderSessionID: "prov-a", HostID: "h1h1h1h1h1h1"}
	b := rundir.Entry{CrazeSessionID: "0193bbbb-2222-7000-8000-00000000ef01", ProviderSessionID: "prov-b", HostID: "h2h2h2h2h2h2"}
	c := rundir.Entry{CrazeSessionID: "abcd0000-3333-7000-8000-0000000000cc", ProviderSessionID: "prov-c", HostID: "zz00zz00zz00"}
	entries := []rundir.Entry{a, b, c}
	for _, tc := range []struct {
		name, session string
		want          []rundir.Entry
	}{
		{"an 8-character suffix (craze ps's id)", "0000abcd", []rundir.Entry{a}},
		{"4 characters match a suffix", "abcd", []rundir.Entry{a}},
		{"3 characters match nothing", "bcd", nil},
		{"the full craze id", a.CrazeSessionID, []rundir.Entry{a}},
		{"a craze-id prefix", "0193bbbb-22", []rundir.Entry{b}},
		{"a host-id prefix", "h2h2h2", []rundir.Entry{b}},
		{"a host-id prefix under 4 characters", "h2h", nil},
		{"the whole host id", "h1h1h1h1h1h1", []rundir.Entry{a}},
		{"a provider id exactly", "prov-b", []rundir.Entry{b}},
		{"no partial provider id", "prov", nil},
		{"no partial provider id, long", "prov-", nil},
		{"a suffix beats a prefix", "abcd", []rundir.Entry{a}},
		{"nothing", "nonesuch", nil},
	} {
		if got := matchSession(entries, tc.session); !slices.Equal(got, tc.want) {
			t.Errorf("%s: --session %q matched %+v, want %+v", tc.name, tc.session, got, tc.want)
		}
	}
	// "abcd" is a's suffix and c's prefix: the suffix tier decides, so c is
	// not an ambiguity (the tier-order negative control).
	if got := matchSession(entries, "abcd"); len(got) != 1 || got[0] != a {
		t.Fatalf("abcd: %+v, want a alone", got)
	}
	// Two entries in the deciding tier are ambiguous.
	d := rundir.Entry{CrazeSessionID: "0193dddd-4444-7000-8000-00000000abcd", HostID: "h4h4h4h4h4h4"}
	if got := matchSession([]rundir.Entry{a, d}, "abcd"); len(got) != 2 {
		t.Fatalf("an ambiguous suffix matched %+v, want both", got)
	}
	// An entry whose ids are not known yet (an empty craze id or host id)
	// never matches a partial id; its known fields still do.
	blank := rundir.Entry{ProviderSessionID: "prov-x"}
	if got := matchSession([]rundir.Entry{blank, a}, "abcd"); len(got) != 1 || got[0] != a {
		t.Fatalf("an entry with empty ids matched a partial: %+v, want a alone", got)
	}
	if got := matchSession([]rundir.Entry{blank}, "prov-x"); len(got) != 1 {
		t.Fatalf("an entry with empty ids lost its exact provider id: %+v", got)
	}
	// One id a prefix of another: the whole id is exact and decides; a token
	// that is only a prefix of both is ambiguous.
	short := rundir.Entry{CrazeSessionID: "0193eeee", HostID: "h5h5h5h5h5h5"}
	long := rundir.Entry{CrazeSessionID: "0193eeee-5555", HostID: "h6h6h6h6h6h6"}
	if got := matchSession([]rundir.Entry{short, long}, "0193eeee"); len(got) != 1 || got[0] != short {
		t.Fatalf("the whole shorter id matched %+v, want it alone", got)
	}
	if got := matchSession([]rundir.Entry{short, long}, "0193ee"); len(got) != 2 {
		t.Fatalf("a prefix of both matched %+v, want both (ambiguous)", got)
	}
}

// TestMatchSessionExactBeatsASuffix: an exact match on any field, even of
// another entry, decides before a suffix does.
func TestMatchSessionExactBeatsASuffix(t *testing.T) {
	suffixed := rundir.Entry{CrazeSessionID: "0193aaaa-1111-7000-8000-0000abcdabcd", HostID: "h1h1h1h1h1h1"}
	exact := rundir.Entry{CrazeSessionID: "other", ProviderSessionID: "abcdabcd", HostID: "h2h2h2h2h2h2"}
	got := matchSession([]rundir.Entry{suffixed, exact}, "abcdabcd")
	if len(got) != 1 || got[0] != exact {
		t.Fatalf("matched %+v, want the exact match alone", got)
	}
}

// TestMatchSessionCountsAnEntryOnce: an entry matching a tier by several
// fields is one match, not an ambiguity.
func TestMatchSessionCountsAnEntryOnce(t *testing.T) {
	both := rundir.Entry{CrazeSessionID: "same-id", ProviderSessionID: "same-id", HostID: "same-id"}
	if got := matchSession([]rundir.Entry{both, {CrazeSessionID: "x", HostID: "y"}}, "same-id"); len(got) != 1 || got[0] != both {
		t.Fatalf("matched %+v, want the one entry once", got)
	}
	e := rundir.Entry{CrazeSessionID: "abcd1234abcd1234", HostID: "abcd1234abcd"}
	if got := matchSession([]rundir.Entry{e}, "abcd1234"); len(got) != 1 {
		t.Fatalf("a prefix of the craze id and the host id matched %+v, want one", got)
	}
}

// TestResolveTargetShortIDs: craze bridge --session takes a short id, refuses
// an ambiguous one in the same words as an exact clash, and says the same
// no-match line.
func TestResolveTargetShortIDs(t *testing.T) {
	a := rundir.Entry{CrazeSessionID: "0193aaaa-1111-7000-8000-00000000abcd", HostID: "aaaaaaaaaaaa", Provider: "cursor", Workspace: "/a"}
	d := rundir.Entry{CrazeSessionID: "0193dddd-4444-7000-8000-00000000abcd", HostID: "dddddddddddd", Provider: "grok", Workspace: "/d"}
	_, err := resolveTarget([]rundir.Entry{a, d}, "0000abcd")
	assertBridgeError(t, err, 1, "craze bridge: 2 sessions match --session 0000abcd: "+
		a.CrazeSessionID+" (cursor, /a), "+d.CrazeSessionID+" (grok, /d)")
	if got, err := resolveTarget([]rundir.Entry{a}, "0000abcd"); err != nil || got != a {
		t.Fatalf("--session 0000abcd: %+v, %v; want %+v", got, err, a)
	}
	_, err = resolveTarget([]rundir.Entry{a}, "bcd")
	assertBridgeError(t, err, 1, "craze bridge: no session bcd")
}
