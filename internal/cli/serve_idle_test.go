package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// craze serve's lifetime end to end (plan 030 §3.6, C5): the idle exit, the
// socket-lost exit, a start that failed kept listable and attachable, a stop
// ending every attached client, a killed client process — each over the fake
// agent, the watcher's ticks and clock the test's own (idleTicks), so every
// look is one the test asked for. Every wait is bounded on its own.

// idleDriver is the idle watcher's ticks and clock for one craze serve.
type idleDriver struct {
	ticks  chan time.Time
	looks  chan idleLook
	base   time.Time
	offset atomic.Int64
}

// driveIdle makes the next craze serve's watcher tick when the test says and
// read the test's clock, and reports every look it makes.
func driveIdle(t *testing.T) *idleDriver {
	t.Helper()
	d := &idleDriver{ticks: make(chan time.Time), looks: make(chan idleLook, 64), base: time.Now()}
	prevTicks, prevLooked := idleTicks, idleLooked
	idleTicks = func() (<-chan time.Time, func() time.Time, func()) { return d.ticks, d.now, func() {} }
	idleLooked = func(l idleLook) { d.looks <- l }
	t.Cleanup(func() { idleTicks, idleLooked = prevTicks, prevLooked })
	return d
}

func (d *idleDriver) now() time.Time { return d.base.Add(time.Duration(d.offset.Load())) }

func (d *idleDriver) advance(dur time.Duration) { d.offset.Add(int64(dur)) }

// look makes the watcher look once and answers what it decided, each within
// serveStep.
func (d *idleDriver) look(t *testing.T, r *serveRun) idleLook {
	t.Helper()
	select {
	case d.ticks <- d.now():
	case <-r.finished:
		t.Fatalf("craze serve returned before the look: %v; stderr: %s", r.err, r.stderr)
	case <-time.After(serveStep):
		t.Fatalf("the idle watcher took no tick within %v; stderr: %s", serveStep, r.stderr)
	}
	select {
	case l := <-d.looks:
		return l
	case <-time.After(serveStep):
		t.Fatalf("the idle watcher made no look within %v; stderr: %s", serveStep, r.stderr)
		return idleLook{}
	}
}

// TestServeExitsWhenIdle (AC4): host_idle_exit = "2s", a session prompted by
// a client that then leaves: the host stays through the grace and the limit,
// and at 2 s of nothing it stops — the ordinary stop sequence, exit 0, the
// host gone, its cause in its log — and the session's index row stays.
func TestServeExitsWhenIdle(t *testing.T) {
	env, ws := serveHome(t)
	writeCrazeConfig(t, "host_idle_exit = \"2s\"\n")
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	d := driveIdle(t)
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
	e := r.waitServing(t, env, true)
	promptUnattached(t, e, "hi there")
	if lt := waitLastTurn(t, e); lt.Outcome != protocol.TurnDone {
		t.Fatalf("the turn ended %+v", lt)
	}
	if l := d.look(t, r); l.Armed || l.Stop != "" {
		t.Fatalf("within the grace: %+v", l)
	}
	d.advance(hostStartupGrace)
	if l := d.look(t, r); !l.Armed || l.Stop != "" {
		t.Fatalf("the grace over: %+v", l)
	}
	d.advance(2 * time.Second)
	if l := d.look(t, r); !l.Fenced || !strings.Contains(l.Stop, "idle for 2s") {
		t.Fatalf("2 s idle: %+v", l)
	}
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve: %v", err)
	}
	assertHostGone(t, env, e)
	if out := r.stderr.String(); !strings.Contains(out, "craze serve: stopping: idle for 2s with no client attached") {
		t.Fatalf("the log: %s", out)
	}
	if row := indexRowByID(t, e.CrazeSessionID); row.CWD != absDir(ws) {
		t.Fatalf("the index row: %+v", row)
	}
}

// TestServeStopsWhenItsSocketOrEntryIsGone (SF-66, narrowly): the host's
// socket file, or its registry entry, removed under it — /run/user at the
// last logout, a registry swept by hand — is a stop at the watcher's next
// look, whatever else holds the host (a client attached, "never"): the
// ordinary stop sequence, so a prompted session stays resumable from its
// index row.
func TestServeStopsWhenItsSocketOrEntryIsGone(t *testing.T) {
	for _, gone := range []string{"socket", "entry"} {
		t.Run(gone, func(t *testing.T) {
			env, ws := serveHome(t)
			writeCrazeConfig(t, "host_idle_exit = \"never\"\n")
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			d := driveIdle(t)
			r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
			e := r.waitServing(t, env, true)
			promptUnattached(t, e, "keep me")
			waitLastTurn(t, e)
			_, attached := attachedTranscript(t, e)
			if l := d.look(t, r); l.Stop != "" {
				t.Fatalf("nothing gone yet: %+v", l)
			}
			path := e.Socket
			if gone == "entry" {
				path = entryPath(env, e.HostID)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if l := d.look(t, r); !strings.Contains(l.Stop, path+" is gone") {
				t.Fatalf("the look after the %s went: %+v", gone, l)
			}
			if err := r.result(t, serveStep); err != nil {
				t.Fatalf("craze serve: %v", err)
			}
			readUntil(t, attached, "the attached client's end", func(it backend.Item) bool { return it.Kind == backend.ItemEnd })
			if left, _ := filepath.Glob(filepath.Join(env.Home, ".cache", "craze", "hosts", "*")); len(left) != 0 {
				t.Fatalf("the registry still holds %q", left)
			}
			if row := indexRowByID(t, e.CrazeSessionID); row.CWD != absDir(ws) {
				t.Fatalf("the index row: %+v", row)
			}
		})
	}
}

// TestServeKeepsAFailedStartListedAndAttachable (plan 030 C5, and X23's root
// cause): a new session whose start fails — the agent refuses session/new —
// leaves its host up, its session listed (sessions.list answers, the row's
// activity error) and attachable: a client attached "now", while the start
// ran, is sent the failure (a ready notification's failed form) and not the
// stream's end, and stays attached; one attaching after it is refused
// start_failed and attaches nothing. The host exits once its one client has
// gone — a failed start keeps nothing — and not before.
func TestServeKeepsAFailedStartListedAndAttachable(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "load") // refuses session/new
	d := driveIdle(t)
	held, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	prevGroup := serveAgentGroup
	serveAgentGroup = func(int, error) {
		if once.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	}
	t.Cleanup(func() { serveAgentGroup = prevGroup })
	var released atomic.Bool
	let := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	r, rd := runServeReady(t, false, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
	t.Cleanup(let)
	if line := readyFrom(t, rd); !line.OK {
		t.Fatalf("the ready line: %+v", line)
	}
	select {
	case <-held:
	case <-time.After(serveStep):
		t.Fatalf("the agent was never spawned; stderr: %s", r.stderr)
	}
	e := waitServingEntry(t, env, false, func() error { return nil }, r.stderr)

	// Attached now, while the start is held after the agent's spawn.
	now, err := remote.DialSession(stepCtx(t), e.Socket, remote.SessionOptions{
		Client:    remote.Options{PeerCheck: rundir.DialCheck(os.Geteuid())},
		SessionID: e.CrazeSessionID, When: protocol.WhenNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = now.Close() })
	if err := now.Attach(stepCtx(t)); err != nil {
		t.Fatalf("an attach now while the start runs: %v", err)
	}
	let()
	var se *remote.StartError
	if err := now.Start(stepCtx(t)); !errors.As(err, &se) || !strings.Contains(se.Text, "session/new must not be called") {
		t.Fatalf("the now-attached client's start: %v, want the start's failure", err)
	}
	select {
	case <-now.Ended():
		t.Fatal("the failed start ended the attached client's stream")
	default:
	}

	row := listRow(t, e)
	if row.Activity != protocol.ActivityError {
		t.Fatalf("the failed session's row: %+v", row)
	}
	late := dialServe(t, e)
	var re *remote.Error
	if err := late.Attach(stepCtx(t)); !errors.As(err, &re) || re.Reason != protocol.ReasonStartFailed {
		t.Fatalf("an attach after the failure: %v, want refused start_failed", err)
	}

	d.advance(time.Hour)
	if l := d.look(t, r); l.Stop != "" {
		t.Fatalf("the failed session's client is still attached: %+v", l)
	}
	if err := now.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(serveStep)
	for {
		l := d.look(t, r)
		if strings.Contains(l.Stop, "did not start") {
			break
		}
		if l.Stop != "" || time.Now().After(deadline) {
			t.Fatalf("its client gone: %+v", l)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve: %v", err)
	}
	assertHostGone(t, env, e)
}

// TestServeStopEndsEveryAttachedClient (AC3): two clients attached, and one
// stops the session: its stop is taken, both see the session's end — its own
// End answering the stop, the other's the end it did not ask for — and the
// host goes, the session's index row staying.
func TestServeStopEndsEveryAttachedClient(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
	e := r.waitServing(t, env, true)
	promptUnattached(t, e, "remember me")
	waitLastTurn(t, e)
	_, a := attachedTranscript(t, e)
	_, b := attachedTranscript(t, e)
	if err := a.Stop(stepCtx(t), engine.Command{Client: a.ClientID(), ID: "1"}); err != nil {
		t.Fatalf("session.stop: %v", err)
	}
	for _, s := range []*remote.Session{a, b} {
		end := readUntil(t, s, "the session's end", func(it backend.Item) bool { return it.Kind == backend.ItemEnd })
		if it := end; it.Err != nil {
			t.Fatalf("an attached client's end: %v", it.Err)
		}
	}
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve: %v", err)
	}
	assertHostGone(t, env, e)
	if row := indexRowByID(t, e.CrazeSessionID); row.CWD != absDir(ws) {
		t.Fatalf("the index row: %+v", row)
	}
}

// TestAKilledClientProcessDoesNotPinTheHost is C1's half-close hazard with a
// real process: a craze bridge child attaches the session over its stdin and
// is killed (SIGKILL) — its socket closed by the kernel, with nothing ever
// written to it again on an idle session — and the host's watcher no longer
// counts it: at host_idle_exit it exits.
func TestAKilledClientProcessDoesNotPinTheHost(t *testing.T) {
	env, ws := serveHome(t)
	writeCrazeConfig(t, "host_idle_exit = \"2s\"\n")
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	d := driveIdle(t)
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
	e := r.waitServing(t, env, true)
	promptUnattached(t, e, "then leave")
	waitLastTurn(t, e)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv, err := json.Marshal([]string{"bridge", "--session", e.CrazeSessionID})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), cliChildEnv+"="+string(argv))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
	})
	lines := protocol.NewLineReader(stdout, protocol.OutboundLineMax)
	send := func(id int, method string, params any) {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		line, err := protocol.MarshalLine(protocol.Request{JSONRPC: protocol.JSONRPCVersion,
			ID: json.RawMessage(strconv.Itoa(id)), Method: method, Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stdin.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	replyTo := func(id string) {
		deadline := time.Now().Add(serveStep)
		for time.Now().Before(deadline) {
			b, err := lines.ReadLine()
			if err != nil {
				t.Fatalf("the bridge's stdout: %v", err)
			}
			var resp protocol.Response
			if json.Unmarshal(b, &resp) == nil && string(resp.ID) == id {
				if resp.Error != nil {
					t.Fatalf("reply %s: %v", id, resp.Error)
				}
				return
			}
		}
		t.Fatalf("no reply %s within %v", id, serveStep)
	}
	send(1, protocol.MethodHello, protocol.HelloParams{Protocols: []int{protocol.ProtocolVersion},
		Client: protocol.ClientInfo{Kind: "test", Name: "killed"}})
	replyTo("1")
	send(2, protocol.MethodSessionAttach, protocol.AttachParams{SessionID: e.CrazeSessionID})
	replyTo("2")

	d.advance(hostStartupGrace)
	d.advance(time.Hour)
	if l := d.look(t, r); !l.Armed || l.Stop != "" {
		t.Fatalf("the bridge attached: %+v", l)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	<-waited
	deadline := time.Now().Add(serveStep)
	for {
		d.advance(2 * time.Second)
		l := d.look(t, r)
		if strings.Contains(l.Stop, "idle for") {
			break
		}
		if l.Stop != "" || time.Now().After(deadline) {
			t.Fatalf("the killed client still keeps the host: %+v", l)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve: %v", err)
	}
	assertHostGone(t, env, e)
}

// TestALoadReplaysProgressivelyToANowAttach (plan 030 C5, undoing X23's
// cost): a client attached "now" to a host loading a session — as the launch
// attaches — is handed the replay as the agent sends it, event by event, and
// its ready after: the transcript streams in rather than arriving whole once
// the start is over. Forced: the host's start is held after the agent's spawn,
// before its load, until the client has attached.
func TestALoadReplaysProgressivelyToANowAttach(t *testing.T) {
	env, _ := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "load-long")
	ws := t.TempDir()
	row := sessions.Row{SessionID: "long-1", Provider: "cursor", CWD: absDir(ws), Title: "the long one",
		CrazeID: "0199aaaa-bbbb-7ccc-8ddd-000000000051"}
	seedIndexRow(t, row)
	held, release := make(chan struct{}), make(chan struct{})
	var once, released atomic.Bool
	prevGroup := serveAgentGroup
	serveAgentGroup = func(int, error) {
		if once.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	}
	t.Cleanup(func() { serveAgentGroup = prevGroup })
	let := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	r, rd := runServeReady(t, false, "--agent-bin", fakeAgentPath(t), "--load", row.CrazeID)
	t.Cleanup(let)
	if line := readyFrom(t, rd); !line.OK {
		t.Fatalf("the ready line: %+v", line)
	}
	select {
	case <-held:
	case <-time.After(serveStep):
		t.Fatalf("the agent was never spawned; stderr: %s", r.stderr)
	}
	e := waitServingEntry(t, env, false, func() error { return nil }, r.stderr)
	s, err := remote.DialSession(stepCtx(t), e.Socket, remote.SessionOptions{
		Client:    remote.Options{PeerCheck: rundir.DialCheck(os.Geteuid())},
		SessionID: e.CrazeSessionID, When: protocol.WhenNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Attach(stepCtx(t)); err != nil {
		t.Fatal(err)
	}
	let()
	replayed := 0
	for {
		it, err := s.Read(stepCtx(t))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if it.Kind == backend.ItemEvent && it.Event.Replayed && it.Event.Text != "" {
			replayed++
		}
		if it.Kind == backend.ItemReady {
			if it.Err != nil {
				t.Fatalf("the load failed: %v", it.Err)
			}
			break
		}
	}
	if replayed < 600 {
		t.Fatalf("%d replayed chunks came before the ready, want the load's 600, streamed", replayed)
	}
	if err := s.Stop(stepCtx(t), engine.Command{Client: s.ClientID(), ID: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve: %v", err)
	}
}

// TestServeJournalsWhyItStopped (plan 030 C5): every stop writes one
// host_stop diag with its cause to the session's journal, before the engine
// closes — a signal's (which wrote nothing before), and a session.stop's
// beside its connection's own control_conn stop note.
func TestServeJournalsWhyItStopped(t *testing.T) {
	for _, how := range []string{"SIGTERM", "session.stop"} {
		t.Run(how, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_JOURNAL", "1")
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
			e := r.waitServing(t, env, true)
			if how == "SIGTERM" {
				r.sigs <- syscall.SIGTERM
			} else {
				s := dialServe(t, e)
				stopOver(t, s, "1")
			}
			if err := r.result(t, serveStep); err != nil {
				t.Fatalf("craze serve: %v", err)
			}
			files, err := filepath.Glob(filepath.Join(os.Getenv("CRAZE_HOME"), "journal", "*", "*.jsonl"))
			if err != nil || len(files) != 1 {
				t.Fatalf("the host's journal: %v, %v", files, err)
			}
			b, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var stops []map[string]any
			for _, line := range strings.Split(string(b), "\n") {
				var m map[string]any
				if json.Unmarshal([]byte(line), &m) != nil || m["type"] != "diag" || m["kind"] != "host_stop" {
					continue
				}
				stops = append(stops, m)
			}
			want := "SIGTERM"
			if how == "session.stop" {
				want = "session.stop from client "
			}
			if len(stops) != 1 {
				t.Fatalf("%d host_stop notes, want one: %s", len(stops), b)
			}
			fields, _ := stops[0]["fields"].(map[string]any)
			if cause, _ := fields["cause"].(string); !strings.HasPrefix(cause, want) {
				t.Fatalf("the host_stop note's cause is %q, want %q…", cause, want)
			}
		})
	}
}
