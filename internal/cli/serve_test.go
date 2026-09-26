package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/tui"
)

// The TUI process serves its session (plan 027 C13): the opt-out, the bind,
// the registry's rewrites, and the teardown's order.

// lockedBuffer is a diag writer the server's goroutines may write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// serveEnv is an isolated rundir.Env: its own 0700 home (so its own registry
// and session locks) and a short runtime directory of its own; the /run/user
// and /tmp candidates are off.
func serveEnv(t *testing.T) rundir.Env {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return rundir.Env{
		Home:            home,
		CrazeDir:        filepath.Join(home, ".craze"),
		CrazeRuntimeDir: shortRuntimeDir(t),
		EUID:            os.Geteuid(),
	}
}

// shortRuntimeDir is a fresh 0700 directory under the real /tmp, short enough
// for sun_path on macOS too (never t.TempDir, never $TMPDIR).
func shortRuntimeDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "czs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// servingHost is a run's host serving over env: claims and a bound socket,
// closed when the test ends if the test has not closed it itself.
func servingHost(t *testing.T, env rundir.Env, diag io.Writer) (*runHost, string) {
	t.Helper()
	hostID := rundir.NewHostID()
	rh := &runHost{claims: newSessionClaims(env, hostID, diag)}
	rh.ctl = serveControl(env, hostID, "/ws", diag)
	if rh.ctl == nil {
		t.Fatalf("serveControl refused: %s", diag)
	}
	t.Cleanup(rh.close)
	return rh, hostID
}

// grokStubEngine is an engine over a Stub whose provider is grok.
func grokStubEngine(t *testing.T) *engine.Engine {
	t.Helper()
	s := tui.NewStubNoPrimary()
	s.SetProvider(agent.GrokProvider())
	eng, err := engine.New(s, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

func entryPath(env rundir.Env, hostID string) string {
	return filepath.Join(env.Home, ".cache", "craze", "hosts", hostID+".json")
}

func readEntryFile(t *testing.T, path string) (rundir.Entry, bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return rundir.Entry{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var e rundir.Entry
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return e, true
}

// waitEntry polls the registry entry until ok says it is the one wanted.
func waitEntry(t *testing.T, path string, ok func(rundir.Entry) bool) rundir.Entry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last rundir.Entry
	for time.Now().Before(deadline) {
		if e, exists := readEntryFile(t, path); exists {
			last = e
			if ok(e) {
				return e
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the registry entry never got there; last %+v", last)
	return last
}

// TestControlSocketOnResolution is the opt-out's matrix (plan 027 §4 item 6):
// either switch turns the socket off, neither turns it on against the other,
// and anything craze cannot read as a clear yes is off with exactly one line
// saying why — it is an access switch, so it fails closed.
func TestControlSocketOnResolution(t *testing.T) {
	envs := []struct {
		name  string
		value string
		unset bool
		on    bool
		why   string
	}{
		{name: "unset", unset: true, on: true},
		{name: "blank", value: "  ", on: true},
		{name: "1", value: "1", on: true},
		{name: "true", value: " true ", on: true},
		{name: "0", value: "0"},
		{name: "false", value: "false"},
		{name: "garbage", value: "maybe", why: `CRAZE_CONTROL_SOCKET="maybe" is not a bool`},
	}
	configs := []struct {
		name    string
		body    string
		none    bool
		confDir bool
		on      bool
		why     string
	}{
		{name: "no config", none: true, on: true},
		{name: "another key only", body: "theme = \"gruvbox\"\n", on: true},
		{name: "control_socket = true", body: "control_socket = true\n", on: true},
		{name: "control_socket = false", body: "control_socket = false\n"},
		{name: "a string", body: "control_socket = \"false\"\n", why: "config.toml control_socket is not a bool"},
		{name: "a number", body: "control_socket = 1\n", why: "config.toml control_socket is not a bool"},
		{name: "unparseable", body: "control_socket = \n", why: "config.toml could not be parsed"},
		{name: "unreadable", confDir: true, why: "config.toml could not be read"},
	}
	for _, e := range envs {
		for _, c := range configs {
			t.Run(e.name+"/"+c.name, func(t *testing.T) {
				home := crazeHome(t)
				switch {
				case c.confDir:
					if err := os.Mkdir(filepath.Join(home, "config.toml"), 0o700); err != nil {
						t.Fatal(err)
					}
				case !c.none:
					if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(c.body), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if e.unset {
					if err := os.Unsetenv(controlSocketEnv); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Unsetenv(controlSocketEnv) })
				} else {
					t.Setenv(controlSocketEnv, e.value)
				}
				var diag bytes.Buffer
				got := controlSocketOn(&diag)
				// The environment is read first: a false or unreadable one
				// decides alone, and the config is not consulted.
				want, why := e.on && c.on, e.why
				if e.on {
					why = c.why
				}
				if !e.on && e.why == "" {
					why = ""
				}
				if got != want {
					t.Fatalf("controlSocketOn = %v, want %v", got, want)
				}
				lines := diagLines(diag.String())
				switch {
				case why == "" && len(lines) != 0:
					t.Fatalf("said %q, want nothing", lines)
				case why != "":
					if len(lines) != 1 || !strings.HasPrefix(lines[0], "craze: control socket off: ") || !strings.Contains(lines[0], why) {
						t.Fatalf("said %q, want one line: craze: control socket off: … %s", lines, why)
					}
				}
			})
		}
	}
}

// TestAFailureToBindIsAWarning: a runtime directory craze will not use (group
// writable) is one line on the craze lane, no socket and no registry entry —
// and the claims are still the run's (plan 027 §3.8: the SQ16 lock does not
// depend on the socket).
func TestAFailureToBindIsAWarning(t *testing.T) {
	env := serveEnv(t)
	if err := os.Chmod(env.CrazeRuntimeDir, 0o770); err != nil {
		t.Fatal(err)
	}
	var diag lockedBuffer
	hostID := rundir.NewHostID()
	if h := serveControl(env, hostID, "/ws", &diag); h != nil {
		h.close()
		t.Fatal("serveControl bound in a group-writable runtime directory")
	}
	lines := diagLines(diag.String())
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "craze: control socket off: ") {
		t.Fatalf("said %q, want one `craze: control socket off:` line", lines)
	}
	if _, err := os.Stat(entryPath(env, hostID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused bind left a registry entry: %v", err)
	}
	rh := &runHost{claims: newSessionClaims(env, hostID, &diag)}
	defer rh.close()
	eng := grokStubEngine(t)
	rh.onEngine(eng)
	other := newSessionClaims(env, rundir.NewHostID(), io.Discard)
	defer other.releaseAll()
	var held *rundir.HeldError
	if _, err := other.claimSession(eng.State().CrazeSessionID); !errors.As(err, &held) {
		t.Fatalf("with no socket the new session is not claimed: %v", err)
	}
}

// TestTheRegistryFollowsTheEngine: the entry is rewritten when the engine is
// handed over (its craze id, incarnation and provider; not ready) and again
// once it is ready (the provider's session id, ready) — §3.8's two rewrites.
// The engine's new session is claimed on the way (§3.9).
func TestTheRegistryFollowsTheEngine(t *testing.T) {
	env := serveEnv(t)
	var diag lockedBuffer
	rh, hostID := servingHost(t, env, &diag)
	path := entryPath(env, hostID)
	first, ok := readEntryFile(t, path)
	if !ok || first.HostID != hostID || first.Workspace != "/ws" || first.Ready || first.CrazeSessionID != "" {
		t.Fatalf("the entry at bind: %+v (exists %v)", first, ok)
	}

	eng := grokStubEngine(t)
	rh.onEngine(eng)
	st := eng.State()
	got := waitEntry(t, path, func(e rundir.Entry) bool { return e.CrazeSessionID != "" })
	if got.CrazeSessionID != st.CrazeSessionID || got.Incarnation != st.Incarnation || got.Provider != "grok" ||
		got.Ready || got.ProviderSessionID != "" {
		t.Fatalf("the entry for the engine: %+v, state %+v", got, st)
	}
	if got.Socket != first.Socket || got.PID != os.Getpid() || !got.StartedAt.Equal(first.StartedAt) {
		t.Fatalf("a rewrite changed what Bind wrote: %+v then %+v", first, got)
	}

	other := newSessionClaims(env, rundir.NewHostID(), io.Discard)
	defer other.releaseAll()
	var held *rundir.HeldError
	if _, err := other.claimSession(st.CrazeSessionID); !errors.As(err, &held) || held.Holder.HostID != hostID {
		t.Fatalf("the new session's claim: %v (holder %+v)", err, held)
	}

	if err := eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ready := waitEntry(t, path, func(e rundir.Entry) bool { return e.Ready })
	if ready.ProviderSessionID != "stub-session-1" || ready.CrazeSessionID != st.CrazeSessionID {
		t.Fatalf("the entry at ready: %+v", ready)
	}
	if s := diag.String(); s != "" {
		t.Fatalf("a healthy run said %q", s)
	}
}

// TestAnEngineThatClosesBeforeItStartsIsNeverReady: Ready also closes when
// the engine closes, and a session that closed rather than started is not
// ready (control's watchReady reads it the same way).
func TestAnEngineThatClosesBeforeItStartsIsNeverReady(t *testing.T) {
	env := serveEnv(t)
	rh, hostID := servingHost(t, env, io.Discard)
	path := entryPath(env, hostID)
	eng := grokStubEngine(t)
	rh.onEngine(eng)
	waitEntry(t, path, func(e rundir.Entry) bool { return e.CrazeSessionID != "" })
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	<-eng.Ready()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if e, _ := readEntryFile(t, path); e.Ready {
			t.Fatalf("a closed, never-started engine was registered ready: %+v", e)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// rawConn is a control socket client that speaks raw NDJSON lines.
type rawConn struct {
	c net.Conn
	r *bufio.Reader
}

func dialRaw(t *testing.T, path string) *rawConn {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &rawConn{c: c, r: bufio.NewReader(c)}
}

func (c *rawConn) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(c.c, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

// read is the next line, decoded; io.EOF once the host has closed.
func (c *rawConn) read(t *testing.T) (map[string]any, error) {
	t.Helper()
	_ = c.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("a line that is not JSON: %q", line)
	}
	return m, nil
}

// reply reads up to the response with id, skipping notifications.
func (c *rawConn) reply(t *testing.T, id string) map[string]any {
	t.Helper()
	for {
		m, err := c.read(t)
		if err != nil {
			t.Fatalf("waiting for reply %s: %v", id, err)
		}
		if m["id"] == id {
			if m["error"] != nil {
				t.Fatalf("reply %s is an error: %v", id, m["error"])
			}
			return m["result"].(map[string]any)
		}
	}
}

// TestTheTeardownFlushesThenClosesThenUnlinks is §3.7's close order fit to
// X16: once the engine has closed (tui.Run's exit tail), an attached client
// still gets everything it was owed — the backlog, the final records and
// reset{session_closed} — then EOF; the server is closed only after that
// flush, the socket and entry are unlinked only after the server's close, and
// the session's claim is released last.
//
// The client reads nothing until the teardown starts, with megabytes still
// queued for it behind a full socket buffer, so the tail can only arrive if
// the teardown waits for it before it closes the server.
func TestTheTeardownFlushesThenClosesThenUnlinks(t *testing.T) {
	env := serveEnv(t)
	rh, hostID := servingHost(t, env, io.Discard)
	path := entryPath(env, hostID)
	s := tui.NewStubNoPrimary()
	s.SetProvider(agent.GrokProvider())
	eng, err := engine.New(s, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	rh.onEngine(eng)
	if err := eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry := waitEntry(t, path, func(e rundir.Entry) bool { return e.Ready })
	crazeID := eng.State().CrazeSessionID

	c := dialRaw(t, entry.Socket)
	c.send(t, `{"jsonrpc":"2.0","id":"1","method":"hello","params":{"protocols":[1],"client":{"kind":"test","name":"teardown"}}}`)
	hello := c.reply(t, "1")
	if ep, _ := hello["endpoint"].(map[string]any); ep["kind"] != "host" || ep["hostId"] != hostID {
		t.Fatalf("hello answered %v", hello)
	}
	c.send(t, `{"jsonrpc":"2.0","id":"2","method":"session.attach","params":{"sessionId":"`+crazeID+`"}}`)
	c.reply(t, "2")

	// A backlog well past any socket buffer and well inside every budget
	// (the subscription's 8 MiB, the writer's 32 MiB), which the client does
	// not read yet.
	const backlog = 16
	chunk := strings.Repeat("o", 256<<10)
	for range backlog {
		s.Emit(agent.Event{Type: agent.EventText, Text: chunk})
	}
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	other := newSessionClaims(env, rundir.NewHostID(), io.Discard)
	defer other.releaseAll()
	stillHeld := func() bool {
		release, err := other.claimSession(crazeID)
		if err == nil {
			release()
			return false
		}
		return true
	}
	exists := func(p string) bool { _, err := os.Lstat(p); return err == nil }

	// finishRun's step: the engine closes first.
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	type tail struct {
		lines []map[string]any
		err   error
	}
	got := make(chan tail, 1)
	var steps []string
	teardownStep = func(step string) {
		note := step
		switch step {
		case "flushing":
			select {
			case <-eng.Done():
			default:
				note += " before the engine's end"
			}
			// The client starts reading only now.
			go func() {
				var out tail
				_ = c.c.SetReadDeadline(time.Now().Add(30 * time.Second))
				for {
					line, err := c.r.ReadBytes('\n')
					if err != nil {
						if !errors.Is(err, io.EOF) {
							out.err = err
						}
						break
					}
					var m map[string]any
					if err := json.Unmarshal(line, &m); err != nil {
						out.err = err
						break
					}
					out.lines = append(out.lines, m)
				}
				got <- out
			}()
		case "server closed":
			if !exists(entry.Socket) || !exists(path) {
				note += " with the socket or entry already gone"
			}
		case "unlinked":
			if exists(entry.Socket) || exists(path) {
				note += " with the socket or entry left"
			}
			if !stillHeld() {
				note += " with the session already released"
			}
		case "released":
			if stillHeld() {
				note += " with the session still held"
			}
		}
		steps = append(steps, note)
	}
	t.Cleanup(func() { teardownStep = func(string) {} })
	rh.close()
	want := []string{"flushing", "flushed", "server closed", "unlinked", "released"}
	if strings.Join(steps, ", ") != strings.Join(want, ", ") {
		t.Fatalf("the teardown ran %q, want %q", steps, want)
	}

	var out tail
	select {
	case out = <-got:
	case <-time.After(30 * time.Second):
		t.Fatal("the client never reached EOF")
	}
	if out.err != nil {
		t.Fatalf("reading the tail: %v", out.err)
	}
	events := 0
	for _, m := range out.lines {
		if m["method"] == protocol.NotifyEvent {
			events++
		}
	}
	if events < backlog {
		t.Fatalf("the client got %d events of the %d-event backlog", events, backlog)
	}
	var last map[string]any
	if n := len(out.lines); n > 0 {
		last = out.lines[n-1]
	}
	params, _ := last["params"].(map[string]any)
	if last["method"] != protocol.NotifyReset || params["reason"] != string(protocol.ResetSessionClosed) {
		t.Fatalf("the attached client's last line is %v, want reset{session_closed}", last)
	}
	if exists(filepath.Join(env.Home, ".cache", "craze", "hosts", hostID+".lock")) {
		t.Fatal("the host lock outlived the teardown")
	}
	if !exists(filepath.Join(env.Home, ".cache", "craze", "locks", crazeID+".lock")) {
		t.Fatal("the session lock file was unlinked; it never is (§3.8)")
	}
}

// TestAClaimAfterTheTeardownIsReleased: a picker's claim that completes after
// the run released everything is refused and holds nothing.
func TestAClaimAfterTheTeardownIsReleased(t *testing.T) {
	env := serveEnv(t)
	c := newSessionClaims(env, rundir.NewHostID(), io.Discard)
	c.releaseAll()
	if _, err := c.claimSession("018f-late"); !errors.Is(err, errClaimsClosed) {
		t.Fatalf("a claim after releaseAll = %v, want errClaimsClosed", err)
	}
	other := newSessionClaims(env, rundir.NewHostID(), io.Discard)
	defer other.releaseAll()
	if _, err := other.claimSession("018f-late"); err != nil {
		t.Fatalf("the late claim held the session: %v", err)
	}
}

// TestHeldRefusalWording is the one wording both paths use, pid ? included.
func TestHeldRefusalWording(t *testing.T) {
	for _, tc := range []struct {
		pid  int
		want string
	}{
		{4242, "that session is open in another craze (pid 4242)"},
		{0, "that session is open in another craze (pid ?)"},
	} {
		err := error(&rundir.HeldError{CrazeID: "x", Holder: rundir.Holder{PID: tc.pid}})
		if got := refusal(err); got != tc.want {
			t.Fatalf("refusal = %q, want %q", got, tc.want)
		}
	}
	if got := refusal(errors.New("x: " + strconv.Quote("y"))); got != `x: "y"` {
		t.Fatalf("refusal of another error = %q", got)
	}
}
