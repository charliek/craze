package cli

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

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
