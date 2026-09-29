package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// craze serve in the foreground (plan 030 C2, §3.3, §3.18 PR 1): the headless
// host with the fake agent, in this process — runServe over a signal channel
// of the test's own — and, where a real signal or a real exit code is the
// point, in a child process (serve_child_test.go). Every wait is bounded on
// its own (serveStep), never one deadline for a whole test, and every test
// here is also run under a 5% CPU quota.

// serveStep bounds each step of a craze serve lifecycle test: one wait. It is
// generous because these tests are run starved too, where a fake agent's spawn
// takes seconds.
const serveStep = 30 * time.Second

// serveHome isolates a craze serve run: its own 0700 HOME (so its own cache
// tree — the registry, the session locks and the host logs), its own
// CRAZE_HOME (config, index), a short CRAZE_RUNTIME_DIR (sun_path), no
// journal, and no provider or agent binary from the developer's environment.
// It answers the rundir.Env the host reads and a workspace.
func serveHome(t *testing.T) (rundir.Env, string) {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CRAZE_PROVIDER", "")
	t.Setenv("CRAZE_JOURNAL", "0")
	t.Setenv("CRAZE_AGENT_BIN", "")
	t.Setenv("CRAZE_FAKE_STDERR", "")
	crazeHome(t)
	t.Setenv("CRAZE_RUNTIME_DIR", shortRuntimeDir(t))
	return rundir.ProcessEnv(), t.TempDir()
}

// parseServeFlags runs craze serve's flag set over argv without running
// anything, which gives Changed the values the real command line would.
func parseServeFlags(t *testing.T, argv ...string) (*cobra.Command, *serveFlags) {
	t.Helper()
	f := &serveFlags{tuiFlags: tuiFlags{force: true}}
	cmd := &cobra.Command{Use: "serve", RunE: func(*cobra.Command, []string) error { return nil }}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	registerServeFlags(cmd, f)
	cmd.SetArgs(argv)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	return cmd, f
}

// serveRun is one craze serve running in this process.
type serveRun struct {
	sigs     chan os.Signal
	stderr   *lockedBuffer
	finished chan struct{}
	err      error // runServe's, once finished is closed
}

// runServeIn runs craze serve over argv in this process, env its agent's
// environment's source (hostEnv{}: none of the multiplexers', so the agent
// inherits the test's own), its stderr — the log without --log — captured. A
// run still serving when the test ends is sent SIGTERM and waited for.
func runServeIn(t *testing.T, env hostEnv, argv ...string) *serveRun {
	t.Helper()
	cmd, f := parseServeFlags(t, argv...)
	r := &serveRun{sigs: make(chan os.Signal, 4), stderr: &lockedBuffer{}, finished: make(chan struct{})}
	cmd.SetErr(r.stderr)
	go func() {
		r.err = runServe(cmd, f, env, r.sigs)
		close(r.finished)
	}()
	t.Cleanup(func() {
		select {
		case <-r.finished:
			return
		default:
		}
		select {
		case r.sigs <- syscall.SIGTERM:
		default:
		}
		select {
		case <-r.finished:
		case <-time.After(serveStep):
			t.Errorf("craze serve did not return after SIGTERM; stderr: %s", r.stderr)
		}
	})
	return r
}

// result is runServe's answer, within d.
func (r *serveRun) result(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case <-r.finished:
		return r.err
	case <-time.After(d):
		t.Fatalf("craze serve has not returned after %v; stderr: %s", d, r.stderr)
		return nil
	}
}

// waitServing is the one live host in env once its registry entry carries its
// session's identity — and, with ready, once the session has started — within
// serveStep. A run that returned first fails the test at once.
func (r *serveRun) waitServing(t *testing.T, env rundir.Env, ready bool) rundir.Entry {
	t.Helper()
	return waitServingEntry(t, env, ready, func() error {
		select {
		case <-r.finished:
			if r.err == nil {
				return errors.New("craze serve returned nil")
			}
			return r.err
		default:
			return nil
		}
	}, r.stderr)
}

// waitServingEntry polls the registry for the one live host with an identity (and,
// with ready, started), within serveStep; gone reports a host that can no
// longer get there.
func waitServingEntry(t *testing.T, env rundir.Env, ready bool, gone func() error, out *lockedBuffer) rundir.Entry {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for {
		entries, err := rundir.Hosts(env)
		if err == nil && len(entries) == 1 && entries[0].CrazeSessionID != "" && (!ready || entries[0].Ready) {
			return entries[0]
		}
		if err := gone(); err != nil {
			t.Fatalf("the host never served: %v; output: %s", err, out)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no serving host after %v: %+v, %v; output: %s", serveStep, entries, err, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stepCtx is a context for one step.
func stepCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), serveStep)
	t.Cleanup(cancel)
	return ctx
}

// dialServe is a remote client of the host e names, peer-checked as craze
// attach's is; closed when the test ends.
func dialServe(t *testing.T, e rundir.Entry) *remote.Session {
	t.Helper()
	s, err := remote.DialSession(stepCtx(t), e.Socket, remote.SessionOptions{
		Client:    remote.Options{PeerCheck: rundir.DialCheck(os.Geteuid())},
		SessionID: e.CrazeSessionID,
		Provider:  e.Provider,
		Workspace: e.Workspace,
	})
	if err != nil {
		t.Fatalf("dial %s: %v", e.Socket, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// promptUnattached sends one prompt from a client that never attaches, and
// closes it: the turn then runs with no client at all.
func promptUnattached(t *testing.T, e rundir.Entry, text string) {
	t.Helper()
	s := dialServe(t, e)
	if _, err := s.Submit(stepCtx(t), engine.Command{Client: s.ClientID(), ID: "1"}, text, engine.SubmitQueue, ""); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close the prompting client: %v", err)
	}
}

// listRow is the host's one sessions.list row, read by a client that never
// attaches.
func listRow(t *testing.T, e rundir.Entry) protocol.SessionRow {
	t.Helper()
	c, err := remote.Dial(stepCtx(t), e.Socket, remote.Options{PeerCheck: rundir.DialCheck(os.Geteuid())})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	var list protocol.SessionsListResult
	if err := c.Call(stepCtx(t), protocol.MethodSessionsList, protocol.SessionsListParams{}, &list); err != nil {
		t.Fatalf("sessions.list: %v", err)
	}
	if len(list.Sessions) != 1 {
		t.Fatalf("sessions.list: %+v", list)
	}
	return list.Sessions[0]
}

// waitLastTurn is how the session's last turn ended, once one has, polled
// over sessions.list — which attaches nothing — within serveStep.
func waitLastTurn(t *testing.T, e rundir.Entry) *protocol.LastTurn {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for {
		if lt := listRow(t, e).LastTurn; lt != nil {
			return lt
		}
		if time.Now().After(deadline) {
			t.Fatalf("no turn ended within %v", serveStep)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// attachedTranscript attaches a new client and answers its first item's
// snapshot as JSON — the whole transcript a client joining now is given — and
// the client, still attached.
func attachedTranscript(t *testing.T, e rundir.Entry) (string, *remote.Session) {
	t.Helper()
	s := dialServe(t, e)
	if err := s.Attach(stepCtx(t)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	it, err := s.Read(stepCtx(t))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if it.Kind != backend.ItemRestore || it.Snapshot == nil {
		t.Fatalf("the first item is %+v, want a restore", it)
	}
	b, err := json.Marshal(it.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), s
}

// stopOver stops the session over s: session.stop, a receipt.
func stopOver(t *testing.T, s *remote.Session, id string) {
	t.Helper()
	if err := s.Stop(stepCtx(t), engine.Command{Client: s.ClientID(), ID: id}); err != nil {
		t.Fatalf("session.stop: %v", err)
	}
}

// assertHostGone: the host's registry entry, its socket and its host lock are
// gone, and nothing else of any host is left in the registry.
func assertHostGone(t *testing.T, env rundir.Env, e rundir.Entry) {
	t.Helper()
	for _, p := range []string{
		entryPath(env, e.HostID),
		filepath.Join(env.Home, ".cache", "craze", "hosts", e.HostID+".lock"),
		e.Socket,
	} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is still there after the stop: %v", p, err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(env.Home, ".cache", "craze", "hosts", "*")); len(left) != 0 {
		t.Fatalf("the registry still holds %q", left)
	}
}

// indexRowByID is the index's row carrying crazeID.
func indexRowByID(t *testing.T, crazeID string) sessions.Row {
	t.Helper()
	rows, err := (&sessions.Store{}).ByCrazeID()
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows[crazeID]
	if !ok {
		t.Fatalf("no index row carries %s: %+v", crazeID, rows)
	}
	return row
}

// seedIndexRow writes row into this test's index as a past run would have,
// its title as the agent's.
func seedIndexRow(t *testing.T, row sessions.Row) {
	t.Helper()
	row.TitleKind = sessions.TitleKindAgent
	if err := (&sessions.Store{}).Upsert(row); err != nil {
		t.Fatal(err)
	}
}

// recordBuilt captures the agent.Options of every session craze serve builds
// in this test.
func recordBuilt(t *testing.T) <-chan agent.Options {
	t.Helper()
	ch := make(chan agent.Options, 4)
	prev := serveBuilt
	serveBuilt = func(o agent.Options) { ch <- o }
	t.Cleanup(func() { serveBuilt = prev })
	return ch
}

func builtOptions(t *testing.T, ch <-chan agent.Options) agent.Options {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(serveStep):
		t.Fatal("craze serve built no session")
		return agent.Options{}
	}
}

// TestServeRunsALongTurnWithNoClient is §3.18 PR 1's first leg for a live
// turn: a turn of 600 message chunks — well past the 256 events a primary
// channel holds — prompted by a client that closes at once, so it runs with no
// client at all; it still ends (NoPrimary: nothing waits on a reader who is not
// there), a client attaching afterwards is given the whole transcript, and
// session.stop then ends the host: runServe returns nil (exit 0), the registry
// entry, the socket and the host lock are gone, and the session's index row
// stays.
func TestServeRunsALongTurnWithNoClient(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "long-reply")
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
	e := r.waitServing(t, env, true)
	if !listRow(t, e).Capabilities.Stop {
		t.Fatal("a craze serve host does not advertise stop")
	}

	promptUnattached(t, e, "go long")
	if lt := waitLastTurn(t, e); lt.Outcome != protocol.TurnDone {
		t.Fatalf("the turn ended %+v, want done", lt)
	}
	snap, s := attachedTranscript(t, e)
	for _, want := range []string{`line 1\n`, `line 257\n`, `line 600\n`, "go long"} {
		if !strings.Contains(snap, want) {
			t.Fatalf("the transcript a new client is given lacks %q", want)
		}
	}

	stopOver(t, s, "1")
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve after session.stop: %v", err)
	}
	assertHostGone(t, env, e)
	if row := indexRowByID(t, e.CrazeSessionID); row.CWD != absDir(ws) || row.Title != "go long" {
		t.Fatalf("the index row is %+v", row)
	}
}

// TestServeReplaysALongLoadWithNoClient is its replay leg: a session/load
// whose replay is 600 chunks, before the load's own answer, with no client
// attached — by the session's craze id, and for a legacy row by
// <provider>:<sessionId>, whose craze id the host assigns under its own claim
// (the index row gains it, and the registry entry names it). A client
// attaching afterwards is given the whole replay; session.stop ends the host
// as for a live turn.
func TestServeReplaysALongLoadWithNoClient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
	}{{"by craze id", false}, {"a legacy row by its key", true}} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "load-long")
			row := sessions.Row{SessionID: "long-1", Provider: "cursor", CWD: absDir(ws), Title: "the long one"}
			id := "cursor:long-1"
			if !tc.legacy {
				row.CrazeID = "0199aaaa-bbbb-7ccc-8ddd-000000000001"
				id = row.CrazeID
			}
			seedIndexRow(t, row)
			r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--load", id)
			e := r.waitServing(t, env, true)
			stored, ok, err := (&sessions.Store{}).Find("cursor", "long-1")
			if err != nil || !ok {
				t.Fatalf("the row: %v, %v", ok, err)
			}
			switch {
			case stored.CrazeID == "":
				t.Fatal("the loaded row has no craze id")
			case e.CrazeSessionID != stored.CrazeID:
				t.Fatalf("the host serves %s, the row is %s", e.CrazeSessionID, stored.CrazeID)
			case !tc.legacy && stored.CrazeID != row.CrazeID:
				t.Fatalf("a row's craze id changed: %s", stored.CrazeID)
			}

			snap, s := attachedTranscript(t, e)
			for _, want := range []string{`line 1\n`, `line 257\n`, `line 600\n`} {
				if !strings.Contains(snap, want) {
					t.Fatalf("the replay a new client is given lacks %q", want)
				}
			}
			stopOver(t, s, "1")
			if err := r.result(t, serveStep); err != nil {
				t.Fatalf("craze serve after session.stop: %v", err)
			}
			assertHostGone(t, env, e)
			indexRowByID(t, e.CrazeSessionID)
		})
	}
}

// holdLoaders makes serveRowRead a barrier: every load that has read its row
// waits there until release (idempotent) is called, and arrived yields each
// row as its loader gets there.
func holdLoaders(t *testing.T) (arrived <-chan sessions.Row, release func()) {
	t.Helper()
	ch := make(chan sessions.Row, 4)
	rel := make(chan struct{})
	var once sync.Once
	prev := serveRowRead
	serveRowRead = func(row sessions.Row) {
		ch <- row
		<-rel
	}
	t.Cleanup(func() { serveRowRead = prev })
	return ch, func() { once.Do(func() { close(rel) }) }
}

// TestServeTwoLegacyLoadsAtOnceOneWins: two hosts asked to load one legacy row
// at the same moment agree on the craze id it is given (EnsureCrazeID, under
// the index's lock), and exactly one of them claims it: the other is exit 1,
// carrying the *rundir.HeldError that names the session — the held answer C3's
// ready line is made of — having bound nothing, and the winner serves the
// session under that id.
//
// The moment is forced (astra r3-c2 6): both loaders are held once they have
// read the row — each finding it with no craze id — and let go together, so
// both race its id's assignment and then its claim; neither can have assigned
// the id before the other read the row.
func TestServeTwoLegacyLoadsAtOnceOneWins(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	seedIndexRow(t, sessions.Row{SessionID: "legacy-1", Provider: "cursor", CWD: absDir(ws)})
	fake := fakeAgentPath(t)
	arrived, release := holdLoaders(t)
	a := runServeIn(t, hostEnv{}, "--agent-bin", fake, "--load", "cursor:legacy-1")
	b := runServeIn(t, hostEnv{}, "--agent-bin", fake, "--load", "cursor:legacy-1")
	// Before runServeIn's own cleanups: a test that fails while a loader is
	// held lets it go, so it can still return.
	t.Cleanup(release)
	for i := range 2 {
		select {
		case row := <-arrived:
			if row.SessionID != "legacy-1" || row.CrazeID != "" {
				t.Fatalf("a loader read %+v, want the legacy row with no craze id yet", row)
			}
		case <-time.After(serveStep):
			t.Fatalf("%d of the two loaders read the row within %v", i, serveStep)
		}
	}
	release()

	var loser, winner *serveRun
	select {
	case <-a.finished:
		loser, winner = a, b
	case <-b.finished:
		loser, winner = b, a
	case <-time.After(serveStep):
		t.Fatal("neither load was refused")
	}
	held := heldBy(loser.err)
	if held == nil {
		t.Fatalf("the loser's answer: %v", loser.err)
	}
	code, msg := exitCode(t, loser.err)
	if code != 1 || !strings.HasPrefix(msg, "craze serve: that session is open in another craze (pid ") {
		t.Fatalf("the loser: exit %d %q", code, msg)
	}
	e := winner.waitServing(t, env, true)
	if e.CrazeSessionID != held.CrazeID {
		t.Fatalf("the winner serves %s, the loser was refused %s", e.CrazeSessionID, held.CrazeID)
	}
	stored, _, _ := (&sessions.Store{}).Find("cursor", "legacy-1")
	if stored.CrazeID != held.CrazeID {
		t.Fatalf("the row's id %q, the claim's %q", stored.CrazeID, held.CrazeID)
	}
	winner.sigs <- syscall.SIGTERM
	if err := winner.result(t, serveStep); err != nil {
		t.Fatalf("the winner after SIGTERM: %v", err)
	}
	assertHostGone(t, env, e)
}

// readUntil reads s's stream until want accepts an item, each read bounded on
// its own, and answers that item.
func readUntil(t *testing.T, s *remote.Session, what string, want func(backend.Item) bool) backend.Item {
	t.Helper()
	for range 10000 {
		it, err := s.Read(stepCtx(t))
		if err != nil {
			t.Fatalf("reading for %s: %v", what, err)
		}
		if want(it) {
			return it
		}
	}
	t.Fatalf("no %s in 10000 items", what)
	return backend.Item{}
}

// turnItem accepts the turn event of phase.
func turnItem(phase string) func(backend.Item) bool {
	return func(it backend.Item) bool {
		return it.Kind == backend.ItemEvent && it.Event.Type == agent.EventTurn && it.Event.Turn != nil && it.Event.Turn.Phase == phase
	}
}

// TestServeStopDuringATurnAuthorsItsEnding is §3.6a's "the engine's own close,
// which authors the running turn's ending as today's quit does": a client
// attached through a turn the agent never finishes sees, after its
// session.stop's receipt, the turn end — synthetic, stopped closing — and then
// the stream's own end; the host returns nil and the prompt's index row stays.
func TestServeStopDuringATurnAuthorsItsEnding(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "hang")
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
	e := r.waitServing(t, env, true)
	s := dialServe(t, e)
	if err := s.Attach(stepCtx(t)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	readUntil(t, s, "the restore", func(it backend.Item) bool { return it.Kind == backend.ItemRestore })
	if _, err := s.Submit(stepCtx(t), engine.Command{Client: s.ClientID(), ID: "1"}, "hold on", engine.SubmitQueue, ""); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	started := readUntil(t, s, "the turn's start", turnItem(agent.TurnStarted))

	stopOver(t, s, "2")
	ended := readUntil(t, s, "the turn's end", turnItem(agent.TurnEnded))
	if tu := ended.Event.Turn; tu.ID != started.Event.Turn.ID || tu.StopReason != "closing" || !tu.Synthetic {
		t.Fatalf("the turn ended %+v, want %s ended synthetic, closing", tu, started.Event.Turn.ID)
	}
	end := readUntil(t, s, "the stream's end", func(it backend.Item) bool { return it.Kind == backend.ItemEnd })
	if end.Err != nil {
		t.Fatalf("the stream ended %v, want the session's own end", end.Err)
	}
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve: %v", err)
	}
	assertHostGone(t, env, e)
	if row := indexRowByID(t, e.CrazeSessionID); row.Title != "hold on" {
		t.Fatalf("the index row is %+v", row)
	}
}

// recordSteps makes teardownStep record every step of the stop sequence and
// hold the sequence at "fenced" until release is called (idempotent); atFence
// is closed when it gets there.
func recordSteps(t *testing.T) (steps func() []string, atFence <-chan struct{}, release func()) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	at := make(chan struct{})
	rel := make(chan struct{})
	var once, relOnce sync.Once
	prev := teardownStep
	teardownStep = func(s string) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
		if s == "fenced" {
			once.Do(func() { close(at) })
			<-rel
		}
	}
	t.Cleanup(func() { teardownStep = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}, at, func() { relOnce.Do(func() { close(rel) }) }
}

// serveStopSteps is the stop sequence's steps, each exactly once, in order.
var serveStopSteps = []string{"fenced", "engine closed", "flushing", "flushed", "server closed", "unlinked", "released"}

// TestServeStopsOnceWhenAStopAndASignalRace is §3.6a's "a second stop joins
// it", forced both ways round: the stop sequence is held at its first step
// while the other stop arrives — a session.stop during a SIGTERM's sequence
// (answered its receipt), a SIGTERM and a second client's session.stop during
// a session.stop's (the receipt again) — and a new attach meanwhile is refused
// closing. Released, the sequence runs once, every step once, and the host
// returns nil.
func TestServeStopsOnceWhenAStopAndASignalRace(t *testing.T) {
	for _, first := range []string{"SIGTERM", "session.stop"} {
		t.Run(first+" first", func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			steps, atFence, release := recordSteps(t)
			r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
			// Before runServeIn's own cleanup: a test that fails while the
			// sequence is held lets it go, so the host can still stop.
			t.Cleanup(release)
			e := r.waitServing(t, env, true)
			s := dialServe(t, e)
			late := dialServe(t, e)
			if err := late.Client().Call(stepCtx(t), protocol.MethodSessionsList, protocol.SessionsListParams{}, &protocol.SessionsListResult{}); err != nil {
				t.Fatal(err)
			}

			if first == "SIGTERM" {
				r.sigs <- syscall.SIGTERM
			} else {
				stopOver(t, s, "1")
			}
			select {
			case <-atFence:
			case <-time.After(serveStep):
				t.Fatal("the stop sequence never started")
			}
			if first == "SIGTERM" {
				stopOver(t, s, "1")
			} else {
				r.sigs <- syscall.SIGTERM
				stopOver(t, late, "1")
			}
			var e2 *remote.Error
			if err := late.Attach(stepCtx(t)); !errors.As(err, &e2) || e2.Reason != protocol.ReasonClosing {
				t.Fatalf("an attach during the stop: %v, want refused closing", err)
			}
			release()

			if err := r.result(t, serveStep); err != nil {
				t.Fatalf("craze serve: %v", err)
			}
			if got := steps(); !slices.Equal(got, serveStopSteps) {
				t.Fatalf("the stop ran %q, want %q once", got, serveStopSteps)
			}
			assertHostGone(t, env, e)
			if !strings.Contains(r.stderr.String(), "craze serve: stopping: ") {
				t.Fatalf("the log never said it was stopping: %s", r.stderr)
			}
		})
	}
}

// TestServeTakesEverySessionFlag is §3.18's option parity: every session flag
// reaches the session the host builds — the agent's options as serveBuilt sees
// them, the engine's craze id in the registry, and the permission mode in the
// info document — and a load is claimed and loaded as the root's --continue
// would: the newest row in the workspace, a row by its craze id in its own
// workspace, and a legacy row by its key, given its craze id by the host.
func TestServeTakesEverySessionFlag(t *testing.T) {
	fake := fakeAgentPath(t)
	type load struct {
		rows []sessions.Row
		// session is the provider session id the agent must be asked to load.
		session string
	}
	for _, tc := range []struct {
		name   string
		script string
		argv   func(ws string) []string
		load   *load
		check  func(t *testing.T, ws string, o agent.Options)
		mode   protocol.PermissionMode
	}{
		{
			name: "a new session", script: "echo",
			argv: func(ws string) []string {
				return []string{"--agent-bin", fake, "--workspace", ws, "--provider", "cursor", "--model", "m-1",
					"--ask", "--no-force", "--plugin-dir", "/p/one", "--plugin-dir", "/p/two"}
			},
			check: func(t *testing.T, ws string, o agent.Options) {
				if o.Binary != fake || o.Workspace != ws || o.Provider == nil || o.Provider.Name() != "cursor" ||
					o.Model != "m-1" || o.Mode != "ask" || o.Force || !slices.Equal(o.PluginDirs, []string{"/p/one", "/p/two"}) ||
					o.LoadSessionID != "" {
					t.Fatalf("options %+v", o)
				}
			},
			mode: protocol.PermissionPrompt,
		},
		{
			// gx speaks grok's dialect: the cursor-shaped echo script would
			// fail its start on auth.
			name: "plan, force, another provider", script: "grok-echo",
			argv: func(ws string) []string {
				return []string{"--agent-bin", fake, "--workspace", ws, "--provider", "gx", "--plan", "--force"}
			},
			check: func(t *testing.T, ws string, o agent.Options) {
				if o.Provider == nil || o.Provider.Name() != "gx" || o.Mode != "plan" || !o.Force || o.Model != "" {
					t.Fatalf("options %+v", o)
				}
			},
			mode: protocol.PermissionBypass,
		},
		{
			name: "--continue", script: "load",
			argv: func(ws string) []string { return []string{"--agent-bin", fake, "--workspace", ws, "-c"} },
			load: &load{session: "newer", rows: []sessions.Row{
				{SessionID: "older", Provider: "cursor", CrazeID: "0199aaaa-bbbb-7ccc-8ddd-000000000011", Title: "older"},
				{SessionID: "newer", Provider: "cursor", CrazeID: "0199aaaa-bbbb-7ccc-8ddd-000000000012", Title: "newer"},
			}},
			check: func(t *testing.T, ws string, o agent.Options) {
				if o.Workspace != ws || o.Title != "newer" {
					t.Fatalf("options %+v", o)
				}
			},
			mode: protocol.PermissionBypass,
		},
		{
			name: "--load a craze id, in its own workspace", script: "load",
			argv: func(string) []string {
				return []string{"--agent-bin", fake, "--load", "0199aaaa-bbbb-7ccc-8ddd-000000000021"}
			},
			load: &load{session: "by-id", rows: []sessions.Row{
				{SessionID: "by-id", Provider: "cursor", CrazeID: "0199aaaa-bbbb-7ccc-8ddd-000000000021", Title: "by id"},
			}},
			check: func(t *testing.T, ws string, o agent.Options) {
				if o.Workspace != absDir(ws) || o.Title != "by id" {
					t.Fatalf("options %+v", o)
				}
			},
			mode: protocol.PermissionBypass,
		},
		{
			name: "--load a legacy row by its key", script: "load",
			argv: func(ws string) []string {
				return []string{"--agent-bin", fake, "--workspace", ws, "--load", "cursor:legacy-2"}
			},
			load: &load{session: "legacy-2", rows: []sessions.Row{{SessionID: "legacy-2", Provider: "cursor", Title: "legacy"}}},
			check: func(t *testing.T, ws string, o agent.Options) {
				if o.Workspace != absDir(ws) || o.Title != "legacy" {
					t.Fatalf("options %+v", o)
				}
			},
			mode: protocol.PermissionBypass,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", tc.script)
			var want sessions.Row
			if tc.load != nil {
				for _, row := range tc.load.rows {
					row.CWD = absDir(ws)
					// Upsert stamps UpdatedAt, which orders --continue: each
					// row is written, and so is newer, after the one before.
					seedIndexRow(t, row)
					time.Sleep(2 * time.Millisecond)
					if row.SessionID == tc.load.session {
						want = row
					}
				}
			}
			built := recordBuilt(t)
			r := runServeIn(t, hostEnv{}, tc.argv(ws)...)
			o := builtOptions(t, built)
			if !o.NoPrimary || !o.Interactive {
				t.Fatalf("a served session is built NoPrimary and interactive: %+v", o)
			}
			tc.check(t, ws, o)
			// Ready, not only the identity: a session whose start failed
			// closes its log, and sessions.list below would race that.
			e := r.waitServing(t, env, true)
			if tc.load != nil {
				if o.LoadSessionID != tc.load.session {
					t.Fatalf("the agent is asked to load %q, want %q", o.LoadSessionID, tc.load.session)
				}
				stored, _, _ := (&sessions.Store{}).Find("cursor", tc.load.session)
				if stored.CrazeID == "" || e.CrazeSessionID != stored.CrazeID || (want.CrazeID != "" && want.CrazeID != stored.CrazeID) {
					t.Fatalf("the host serves %q; the row's id is %q (was %q)", e.CrazeSessionID, stored.CrazeID, want.CrazeID)
				}
			}
			if got := listRow(t, e).PermissionMode; got != tc.mode {
				t.Fatalf("permissionMode %q, want %q", got, tc.mode)
			}
			r.sigs <- syscall.SIGTERM
			if err := r.result(t, serveStep); err != nil {
				t.Fatalf("craze serve after SIGTERM: %v", err)
			}
			assertHostGone(t, env, e)
		})
	}
}

// TestServeRefusesAsTheRootRefuses: the session flags' refusals are the
// root's, word for word and code for code — the same functions give them — and
// craze serve's own flags are refused before anything is read or bound. No
// refusal leaves a host behind.
func TestServeRefusesAsTheRootRefuses(t *testing.T) {
	env, ws := serveHome(t)
	fake := fakeAgentPath(t)
	empty := t.TempDir()
	nativeWS := t.TempDir()
	other := t.TempDir()
	seedIndexRow(t, sessions.Row{SessionID: "n-1", Provider: "native", CWD: absDir(nativeWS), CrazeID: "0199aaaa-bbbb-7ccc-8ddd-000000000031"})
	seedIndexRow(t, sessions.Row{SessionID: "c-1", Provider: "cursor", CWD: absDir(ws), CrazeID: "0199aaaa-bbbb-7ccc-8ddd-000000000032"})
	built := recordBuilt(t)

	for _, argv := range [][]string{
		{"--provider", "native", "--agent-bin", fake},
		{"--ask", "--plan"},
		{"--provider", "bogus"},
		{"--continue", "--workspace", empty},
		{"--continue", "--workspace", nativeWS, "--agent-bin", fake},
		{"--workspace", filepath.Join(empty, "missing")},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			cmd, f := parseTUIFlags(t, argv...)
			rootCode, rootMsg := exitCode(t, runTUI(cmd, f, hostEnv{}))
			r := runServeIn(t, hostEnv{}, argv...)
			code, msg := exitCode(t, r.result(t, serveStep))
			if code != rootCode || msg != rootMsg {
				t.Fatalf("craze serve: exit %d %q; the root: exit %d %q", code, msg, rootCode, rootMsg)
			}
		})
	}

	for _, tc := range []struct {
		argv []string
		code int
		msg  string
	}{
		{[]string{"--continue", "--load", "x"}, 2, "craze serve: --continue and --load are mutually exclusive"},
		{[]string{"--load", ""}, 2, `craze serve: --load "" is neither a craze session id nor <provider>:<session id>`},
		{[]string{"--load", "a b"}, 2, `craze serve: --load "a b" is neither a craze session id nor <provider>:<session id>`},
		{[]string{"--load", "nope:x"}, 2, `craze serve: --load "nope:x": unknown provider "nope" (want ` + providerIDs + ")"},
		{[]string{"--load", "cursor: "}, 2, `craze serve: --load "cursor: " names no session id`},
		{[]string{"--load", "0199aaaa-bbbb-7ccc-8ddd-0000000000ff"}, 1, "craze serve: no session 0199aaaa-bbbb-7ccc-8ddd-0000000000ff"},
		{[]string{"--load", "cursor:c-1", "--provider", "grok"}, 1, "craze serve: no session cursor:c-1 for provider grok"},
		{[]string{"--load", "0199aaaa-bbbb-7ccc-8ddd-000000000032", "--workspace", other}, 2,
			"craze serve: --load names a session that ran in " + absDir(ws) + ", not in --workspace " + absDir(other)},
		{[]string{"--load", "0199aaaa-bbbb-7ccc-8ddd-000000000031", "--agent-bin", fake}, 2,
			"craze: --agent-bin cannot be used with provider native, which runs inside craze"},
	} {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			r := runServeIn(t, hostEnv{}, tc.argv...)
			code, msg := exitCode(t, r.result(t, serveStep))
			if code != tc.code || msg != tc.msg {
				t.Fatalf("exit %d %q, want exit %d %q", code, msg, tc.code, tc.msg)
			}
		})
	}

	select {
	case o := <-built:
		t.Fatalf("a refused run built a session: %+v", o)
	default:
	}
	if entries, _ := rundir.Hosts(env); len(entries) != 0 {
		t.Fatalf("a refusal left hosts behind: %+v", entries)
	}
	for _, id := range []string{"0199aaaa-bbbb-7ccc-8ddd-000000000031", "0199aaaa-bbbb-7ccc-8ddd-000000000032"} {
		if p := filepath.Join(env.Home, ".cache", "craze", "locks", id+".lock"); fileExists(p) && lockHeldBy(t, p, os.Getpid()) {
			t.Fatalf("a refused load still holds %s", id)
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// countingListener is a unix socket that counts the connections made to it.
type countingListener struct {
	ln net.Listener
	n  atomic.Int32
}

func listenCounting(t *testing.T, path string) *countingListener {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l := &countingListener{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.n.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return l
}

// TestServeStripsTheHostHookGatesFromItsAgent is §3.3's provider environment:
// with herdr's and roost's gates met in the host's environment, the agent child
// is given it less exactly HERDR_ENV and ROOST_AGENT_HOOK — the pane and tab
// ids stay — as the TUI's launch gives it (agentEnv), and the host itself
// reports nothing to either: neither socket is ever dialled.
func TestServeStripsTheHostHookGatesFromItsAgent(t *testing.T) {
	env, ws := serveHome(t)
	socks := shortRuntimeDir(t)
	herdr := listenCounting(t, filepath.Join(socks, "h"))
	roost := listenCounting(t, filepath.Join(socks, "r"))
	vars := map[string]string{
		"HERDR_ENV": "1", "HERDR_SOCKET_PATH": filepath.Join(socks, "h"), "HERDR_PANE_ID": "w9:p9",
		"ROOST_SOCKET": filepath.Join(socks, "r"), "ROOST_TAB_ID": "7", "ROOST_AGENT_HOOK": "/opt/roost/roostctl",
	}
	r := runServeIn(t, fakeHostEnv(vars, "CRAZE_FAKE_SCRIPT=env", "PATH="+os.Getenv("PATH")),
		"--agent-bin", fakeAgentPath(t), "--workspace", ws)
	e := r.waitServing(t, env, true)
	promptUnattached(t, e, "which gates")
	if lt := waitLastTurn(t, e); lt.Outcome != protocol.TurnDone {
		t.Fatalf("the turn ended %+v", lt)
	}
	snap, s := attachedTranscript(t, e)
	if !strings.Contains(snap, "envset: ROOST_TAB_ID HERDR_PANE_ID :end") {
		t.Fatalf("the agent's environment: %s", snap)
	}
	stopOver(t, s, "1")
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve: %v", err)
	}
	if h, ro := herdr.n.Load(), roost.n.Load(); h != 0 || ro != 0 {
		t.Fatalf("the host reported to the multiplexers: herdr %d, roost %d connections", h, ro)
	}
}

// TestServeBindFailureIsFatal: a runtime directory craze refuses to bind in is
// exit 1 for craze serve — unlike the TUI's warning, since a headless host is
// reachable through nothing else — and nothing is left: no registry entry or
// host lock, the load's claim released, no session built, and the refusal in
// the log.
func TestServeBindFailureIsFatal(t *testing.T) {
	env, ws := serveHome(t)
	runtimeDir := shortRuntimeDir(t)
	if err := os.Chmod(runtimeDir, 0o770); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_RUNTIME_DIR", runtimeDir)
	id := "0199aaaa-bbbb-7ccc-8ddd-000000000041"
	seedIndexRow(t, sessions.Row{SessionID: "b-1", Provider: "cursor", CWD: absDir(ws), CrazeID: id})
	built := recordBuilt(t)
	logPath := filepath.Join(t.TempDir(), "host.log")
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--load", id, "--log", logPath)
	code, msg := exitCode(t, r.result(t, serveStep))
	if code != 1 || !strings.HasPrefix(msg, "craze serve: the control socket: ") {
		t.Fatalf("exit %d %q", code, msg)
	}
	select {
	case o := <-built:
		t.Fatalf("a host that could not bind built a session: %+v", o)
	default:
	}
	if left, _ := filepath.Glob(filepath.Join(env.Home, ".cache", "craze", "hosts", "*")); len(left) != 0 {
		t.Fatalf("a failed bind left %q", left)
	}
	if p := filepath.Join(env.Home, ".cache", "craze", "locks", id+".lock"); lockHeldBy(t, p, os.Getpid()) {
		t.Fatal("the load's claim is still held")
	}
	if b, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(b), msg) {
		t.Fatalf("the log: %q, %v; want %q in it", b, err, msg)
	}
}

// TestServeRefusesWithTheControlSocketOff: control_socket = false, or
// CRAZE_CONTROL_SOCKET false, is exit 2 for craze serve (§3.3's last bullet),
// before anything is created — not even the cache tree.
func TestServeRefusesWithTheControlSocketOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(t *testing.T)
		why  string
	}{
		{"config", func(t *testing.T) { writeCrazeConfig(t, "control_socket = false\n") }, "control_socket = false in config.toml, or CRAZE_CONTROL_SOCKET is false"},
		{"environment", func(t *testing.T) { t.Setenv("CRAZE_CONTROL_SOCKET", "0") }, "control_socket = false in config.toml, or CRAZE_CONTROL_SOCKET is false"},
		{"unreadable", func(t *testing.T) { t.Setenv("CRAZE_CONTROL_SOCKET", "maybe") }, `CRAZE_CONTROL_SOCKET="maybe" is not a bool`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws := serveHome(t)
			tc.set(t)
			r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
			code, msg := exitCode(t, r.result(t, serveStep))
			want := "craze serve: the control socket is off (" + tc.why + "), and a headless host is reachable only through it"
			if code != 2 || msg != want {
				t.Fatalf("exit %d %q, want exit 2 %q", code, msg, want)
			}
			if _, err := os.Lstat(filepath.Join(env.Home, ".cache")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the refusal made the cache tree: %v", err)
			}
		})
	}
}

// TestServeSweepsOldHostLogsAtStart: a host starting removes the logs and
// process-group records of hosts gone longer than a week, and keeps a newer
// one and its own.
func TestServeSweepsOldHostLogsAtStart(t *testing.T) {
	env, ws := serveHome(t)
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	gone, recent := rundir.NewHostID(), rundir.NewHostID()
	old := time.Now().Add(-8 * 24 * time.Hour)
	var stale []string
	for _, name := range []string{gone + ".log", gone + ".log.1", gone + ".pgids"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		stale = append(stale, p)
	}
	kept := filepath.Join(dir, recent+".log")
	if err := os.WriteFile(kept, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(dir, "own.log")
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--log", own)
	e := r.waitServing(t, env, false)
	for _, p := range stale {
		if fileExists(p) {
			t.Fatalf("%s was not swept", filepath.Base(p))
		}
	}
	if !fileExists(kept) || !fileExists(own) {
		t.Fatal("the sweep took a log it should have kept")
	}
	r.sigs <- syscall.SIGTERM
	if err := r.result(t, serveStep); err != nil {
		t.Fatal(err)
	}
	assertHostGone(t, env, e)
}

// TestServeFlagsAreTheRootsSessionFlags: the session flags are one
// registration on both commands — the same names, shorthands, defaults, types
// and usage — and neither command has the other's own flags.
func TestServeFlagsAreTheRootsSessionFlags(t *testing.T) {
	root, serve := NewRootCmd(), newServeCmd()
	shared := []string{"workspace", "provider", "model", "agent-bin", "force", "no-force", "ask", "plan", "plugin-dir", "continue"}
	for _, name := range shared {
		r, s := root.Flags().Lookup(name), serve.Flags().Lookup(name)
		if r == nil || s == nil {
			t.Fatalf("--%s: root %v, serve %v", name, r != nil, s != nil)
		}
		if r.Usage != s.Usage || r.Shorthand != s.Shorthand || r.DefValue != s.DefValue || r.Value.Type() != s.Value.Type() {
			t.Fatalf("--%s differs: root %+v, serve %+v", name, r, s)
		}
	}
	for _, name := range []string{"theme", "no-mouse", "no-background", "no-host-status", "resume"} {
		if serve.Flags().Lookup(name) == nil && root.Flags().Lookup(name) != nil {
			continue
		}
		t.Fatalf("--%s: the TUI's own, and only the root's", name)
	}
	for _, name := range []string{"load", "log"} {
		if serve.Flags().Lookup(name) != nil && root.Flags().Lookup(name) == nil {
			continue
		}
		t.Fatalf("--%s: craze serve's own, and only serve's", name)
	}
}

// TestServeInAProcessOfItsOwn is the real process: signals as the OS delivers
// them and the exit code craze's Execute gives. SIGHUP — a terminal closing on
// a foreground host — is caught and dropped (the log says so, and the host
// still answers), never inherited as ignored by the agent; SIGTERM then runs
// the stop sequence and the process exits 0, its registry entry, socket and
// host lock gone. The log is --log's, in the cache tree's host-logs directory
// (made 0700), the file 0600, holding craze's own lines and the agent's
// stderr.
func TestServeInAProcessOfItsOwn(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	t.Setenv("CRAZE_FAKE_STDERR", "fake-agent-says-hello")
	logPath := filepath.Join(env.Home, ".cache", "craze", "host-logs", "fg.log")
	c := startCrazeChild(t, "serve", "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--log", logPath)
	gone := func() error {
		if c.running() {
			return nil
		}
		return errors.New("the child exited")
	}
	e := waitServingEntry(t, env, true, gone, c.output)
	logHas := func(want string) {
		t.Helper()
		deadline := time.Now().Add(serveStep)
		for {
			b, _ := os.ReadFile(logPath)
			if strings.Contains(string(b), want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the log never said %q: %s", want, b)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	logHas("fake-agent-says-hello")

	if err := c.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	logHas("craze serve: hangup ignored")
	if !c.running() {
		t.Fatalf("SIGHUP ended the host: %s", c.output)
	}
	if row := listRow(t, e); row.SessionID != e.CrazeSessionID {
		t.Fatalf("after SIGHUP the host answers %+v", row)
	}

	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := c.wait(t, serveStep); code != 0 {
		t.Fatalf("exit %d after SIGTERM; output: %s", code, c.output)
	}
	assertHostGone(t, env, e)
	logHas("craze serve: stopping: SIGTERM")
	fi, err := os.Stat(logPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the log is %v, %v; want 0600", fi.Mode(), err)
	}
	di, err := os.Stat(filepath.Dir(logPath))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("host-logs is %v, %v; want 0700", di.Mode(), err)
	}
	if out := c.output.String(); out != "" {
		t.Fatalf("with --log the host wrote to its stderr: %q", out)
	}
}

// unusableLocks makes the session locks' directory one craze refuses — 0755,
// where every leaf of the cache tree must be 0700 — while the registry beside
// it stays usable: a claim cannot even be attempted, and a bind can.
func unusableLocks(t *testing.T, env rundir.Env) {
	t.Helper()
	if _, err := rundir.HostLogDir(env); err != nil {
		t.Fatal(err)
	}
	locks := filepath.Join(env.Home, ".cache", "craze", "locks")
	if err := os.Mkdir(locks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locks, 0o755); err != nil {
		t.Fatal(err)
	}
	if c, err := rundir.ClaimSession(env, "0199aaaa-bbbb-7ccc-8ddd-0000000000aa", rundir.NewHostID()); err == nil {
		_ = c.Release()
		t.Fatal("fixture: the locks directory is still usable")
	}
}

// TestServeDoesNotRunUnclaimed (astra r3-c2 1): a session craze serve cannot
// claim — here a locks directory craze refuses, with the registry beside it
// usable — is exit 1 for serve, where the TUI warns and runs unclaimed (X30,
// TestOnlyARequiredClaimRefusesAnUnusableLockTree): a new session, a load by
// craze id, a legacy row and --continue alike. Nothing is left behind: no
// registry entry, host lock or socket (a new session bound its socket before
// its engine minted the id to claim), no load built, no new session started
// (the index holds no row of it), and the refusal is in the log.
func TestServeDoesNotRunUnclaimed(t *testing.T) {
	const id = "0199aaaa-bbbb-7ccc-8ddd-000000000051"
	for _, tc := range []struct {
		name string
		argv []string
		rows []sessions.Row
	}{
		{name: "a new session", argv: []string{"--provider", "cursor"}},
		{name: "--load a craze id", argv: []string{"--load", id},
			rows: []sessions.Row{{SessionID: "u-1", Provider: "cursor", CrazeID: id}}},
		{name: "--load a legacy row", argv: []string{"--load", "cursor:u-2"},
			rows: []sessions.Row{{SessionID: "u-2", Provider: "cursor"}}},
		{name: "--continue", argv: []string{"--continue"},
			rows: []sessions.Row{{SessionID: "u-3", Provider: "cursor", CrazeID: id}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			for _, row := range tc.rows {
				row.CWD = absDir(ws)
				seedIndexRow(t, row)
			}
			unusableLocks(t, env)
			built := recordBuilt(t)
			logPath := filepath.Join(t.TempDir(), "host.log")
			argv := append([]string{"--agent-bin", fakeAgentPath(t), "--workspace", ws, "--log", logPath}, tc.argv...)
			r := runServeIn(t, hostEnv{}, argv...)
			code, msg := exitCode(t, r.result(t, serveStep))
			if code != 1 || !strings.HasPrefix(msg, "craze serve: the session cannot be claimed: ") {
				t.Fatalf("exit %d %q, want exit 1, the session cannot be claimed", code, msg)
			}
			if left, _ := filepath.Glob(filepath.Join(env.Home, ".cache", "craze", "hosts", "*")); len(left) != 0 {
				t.Fatalf("an unclaimed host left %q", left)
			}
			if left, _ := filepath.Glob(filepath.Join(os.Getenv("CRAZE_RUNTIME_DIR"), "*", "*.sock")); len(left) != 0 {
				t.Fatalf("an unclaimed host left its socket: %q", left)
			}
			select {
			case o := <-built:
				if tc.rows != nil {
					t.Fatalf("an unclaimed load built its session: %+v", o)
				}
			default:
				if tc.rows == nil {
					t.Fatal("a new session is claimed once its engine has minted its id: none was built")
				}
			}
			if tc.rows == nil {
				if _, ok, err := (&sessions.Store{}).Latest(absDir(ws), ""); ok || err != nil {
					t.Fatalf("an unclaimed new session wrote an index row: %v, %v", ok, err)
				}
			}
			if b, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(b), msg) {
				t.Fatalf("the log: %q, %v; want %q in it", b, err, msg)
			}
		})
	}
}

// failingIndex is a session index that cannot give a row its craze id.
type failingIndex struct{}

func (failingIndex) EnsureCrazeID(sessions.Row, time.Duration) (string, error) {
	return "", errors.New("the index is read-only")
}

// TestOnlyARequiredClaimRefusesAnUnusableLockTree: the claim a lock tree craze
// refuses cannot take, and the legacy row an index that cannot be written
// cannot give an id, are the TUI's warnings — it runs unclaimed (X30), as it
// always has — and craze serve's refusals (sessionClaims.required): an
// *unclaimedError, saying nothing on the log itself. A new session's id is
// refused the same way (require).
func TestOnlyARequiredClaimRefusesAnUnusableLockTree(t *testing.T) {
	env, ws := serveHome(t)
	unusableLocks(t, env)
	withID := sessions.Row{SessionID: "r-1", Provider: "cursor", CWD: absDir(ws), CrazeID: "0199aaaa-bbbb-7ccc-8ddd-000000000061"}
	legacy := sessions.Row{SessionID: "r-2", Provider: "cursor", CWD: absDir(ws)}
	seedIndexRow(t, withID)
	seedIndexRow(t, legacy)
	claims := func(required bool, index crazeIDIndex) (*sessionClaims, *lockedBuffer) {
		diag := &lockedBuffer{}
		c := newSessionClaims(env, rundir.NewHostID(), diag)
		c.required = required
		if index != nil {
			c.index = index
		}
		t.Cleanup(c.releaseAll)
		return c, diag
	}
	for _, tc := range []struct {
		name  string
		row   sessions.Row
		index crazeIDIndex
		// rootID is the id the TUI loads the row under, unclaimed.
		rootID string
	}{
		{"a row with its id", withID, nil, withID.CrazeID},
		{"a legacy row given its id", legacy, nil, "(minted)"},
		{"a legacy row the index cannot give one", legacy, failingIndex{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, rootDiag := claims(false, tc.index)
			id, release, err := root.claimRow(tc.row)
			if err != nil || release == nil || (tc.rootID != "(minted)" && id != tc.rootID) || (tc.rootID == "(minted)" && id == "") {
				t.Fatalf("the TUI's claim: %q, %v; want it to run unclaimed under %q", id, err, tc.rootID)
			}
			if !strings.HasPrefix(rootDiag.String(), "craze: session lock not taken: ") {
				t.Fatalf("the TUI's claim said %q, want its warning", rootDiag)
			}
			serve, serveDiag := claims(true, tc.index)
			if _, _, err := serve.claimRow(tc.row); unclaimedBy(err) == nil {
				t.Fatalf("craze serve's claim: %v, want an *unclaimedError", err)
			}
			if serveDiag.String() != "" {
				t.Fatalf("craze serve's refused claim warned too: %q", serveDiag)
			}
		})
	}
	serve, _ := claims(true, nil)
	if err := serve.require("0199aaaa-bbbb-7ccc-8ddd-000000000062"); err == nil {
		t.Fatal("craze serve's new session was claimed on a lock tree craze refuses")
	}
}

// TestServeNeverSweepsItsOwnLog (astra r3-c2 3): a --log in the host logs'
// directory that a gone host left a week and a day ago — a host id's name,
// that host long dead — is the host's own log from its start: its own sweep,
// which runs before the log opens, keeps it and the host appends to it; and
// another host's sweep while it serves keeps it too (it is opened fresh:
// TestHostLogIsOpenedFresh pins the instant before its first line). The
// host's last line lands in that very file.
func TestServeNeverSweepsItsOwnLog(t *testing.T) {
	env, ws := serveHome(t)
	dir, err := rundir.HostLogDir(env)
	if err != nil {
		t.Fatal(err)
	}
	reused := filepath.Join(dir, rundir.NewHostID()+".log")
	if err := os.WriteFile(reused, []byte("a week and a day ago\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(reused, old, old); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--log", reused)
	e := r.waitServing(t, env, false)
	if !fileExists(reused) {
		t.Fatal("the host's start-up sweep removed its own log")
	}
	// Another host starting now sweeps as this one did.
	if _, err := rundir.SweepHostLogs(env, time.Now(), hostLogKeep, ""); err != nil {
		t.Fatal(err)
	}
	if !fileExists(reused) {
		t.Fatal("another host's sweep removed a log in use")
	}
	r.sigs <- syscall.SIGTERM
	if err := r.result(t, serveStep); err != nil {
		t.Fatal(err)
	}
	assertHostGone(t, env, e)
	b, err := os.ReadFile(reused)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); !strings.HasPrefix(got, "a week and a day ago\n") ||
		!strings.Contains(got, "craze serve: host "+e.HostID+" serving session") ||
		!strings.Contains(got, "craze serve: stopping: SIGTERM") {
		t.Fatalf("the reused log holds %q", got)
	}
}

// TestServeCrashOutputSurvivesAPanic (astra r3-c2 4): a panic on craze serve's
// own goroutine — forced in a process of its own, once the host serves — is
// printed into its log, which a spawned host's stderr (/dev/null) never is:
// the log is not closed on a panic's way out, so the runtime's crash output is
// still on it when the panic is printed. The same after a rotation, which
// moves the crash output to the new log.
func TestServeCrashOutputSurvivesAPanic(t *testing.T) {
	const forced = "craze test: a forced panic"
	for _, rotated := range []bool{false, true} {
		name := "a fresh log"
		if rotated {
			name = "after a rotation"
		}
		t.Run(name, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			t.Setenv(cliChildPanic, forced)
			dir, err := rundir.HostLogDir(env)
			if err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(dir, "crash.log")
			if rotated {
				// A log holding a line already, and a cap its host's first line
				// passes: that line rotates it.
				t.Setenv(cliChildLogMax, "64")
				if err := os.WriteFile(logPath, []byte("before the host\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c := startCrazeChild(t, "serve", "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--provider", "cursor", "--log", logPath)
			if code := c.wait(t, serveStep); code != 2 {
				t.Fatalf("exit %d, want 2 for a panic; output: %s", code, c.output)
			}
			if !strings.Contains(c.output.String(), "panic: "+forced) {
				t.Fatalf("the panic is not on stderr either: %s", c.output)
			}
			got := readLog(t, logPath)
			if !strings.Contains(got, "craze serve: host ") || !strings.Contains(got, "panic: "+forced) || !strings.Contains(got, "goroutine ") {
				t.Fatalf("the log holds %q; want the host's line, then the panic and its goroutines", got)
			}
			if rotation := readLog(t, logPath+".1"); rotated != (rotation == "before the host\n") {
				t.Fatalf("the rotation holds %q (rotated %v)", rotation, rotated)
			}
		})
	}
}
