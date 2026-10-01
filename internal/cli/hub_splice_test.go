package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
)

// The hub's splice with real processes (plan 032 §3.18; A8, A9, A10): craze
// serve hosts and a craze hub, each a process of its own (serve_child_test.go's
// re-executed test binary; the hub spawned only through the seam a test
// installs, hubAsChild), and a client in this process that reaches its session
// through the hub with hub.Dialer — the hub killed under it, a host killed under
// the hub, and an S2-era host beside a current one. Every wait is bounded on
// its own (serveStep), and each test here is also run under a 5% CPU quota.

// serveHosts starts n craze serve hosts over the fake agent — each with a
// workspace and a provider session id of its own — and waits for all n to be
// listed in env's registry, started; it answers their entries in host-id
// order, and the children.
func serveHosts(t *testing.T, env rundir.Env, n int) ([]rundir.Entry, map[string]*crazeChild) {
	t.Helper()
	agentBin := fakeAgentPath(t)
	kids := map[string]*crazeChild{}
	for i := range n {
		t.Setenv("CRAZE_FAKE_SESSION_ID", fmt.Sprintf("fake-session-%d", i+1))
		id := rundir.NewHostID()
		ws := filepath.Join(t.TempDir(), fmt.Sprintf("ws%d", i+1))
		if err := os.Mkdir(ws, 0o700); err != nil {
			t.Fatal(err)
		}
		kids[id] = startCrazeChild(t, "serve", "--agent-bin", agentBin, "--workspace", ws, "--host-id", id)
	}
	deadline := time.Now().Add(serveStep)
	for {
		entries, err := rundir.Hosts(env)
		ready := 0
		for _, e := range entries {
			if e.CrazeSessionID != "" && e.Ready {
				ready++
			}
		}
		if err == nil && ready == n && len(entries) == n {
			slices.SortFunc(entries, func(a, b rundir.Entry) int { return cmpString(a.HostID, b.HostID) })
			return entries, kids
		}
		for id, k := range kids {
			if !k.running() {
				t.Fatalf("host %s exited: %s", id, k.output)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d hosts started within %v: %+v (%v)", ready, n, serveStep, entries, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// hubConn is a raw connection to the hub at sock that has said hello.
type hubConn struct {
	t  *testing.T
	nc net.Conn
	lr *protocol.LineReader
	id int
}

func dialHub(t *testing.T, sock string) *hubConn {
	t.Helper()
	nc, err := net.DialTimeout("unix", sock, serveStep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	hc := &hubConn{t: t, nc: nc, lr: protocol.NewLineReader(nc, protocol.OutboundLineMax)}
	var res protocol.HubHelloResult
	hc.call(protocol.MethodHello, protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{Kind: "test"}}, &res)
	return hc
}

// call sends a request and decodes its reply — the next line — into result.
func (hc *hubConn) call(method string, params, result any) {
	hc.t.Helper()
	hc.id++
	_ = hc.nc.SetDeadline(time.Now().Add(serveStep))
	defer func() { _ = hc.nc.SetDeadline(time.Time{}) }()
	if err := protocol.WriteLine(hc.nc, map[string]any{"jsonrpc": "2.0", "id": hc.id, "method": method, "params": params}); err != nil {
		hc.t.Fatalf("%s: %v", method, err)
	}
	line, err := hc.lr.ReadLine()
	if err != nil {
		hc.t.Fatalf("%s: %v", method, err)
	}
	var resp protocol.Response
	if err := json.Unmarshal(line, &resp); err != nil || resp.Error != nil {
		hc.t.Fatalf("%s: %s (%v)", method, line, err)
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		hc.t.Fatalf("%s: %v", method, err)
	}
}

// next is the next line the hub writes, within d: a notification's method
// and params.
func (hc *hubConn) next(d time.Duration) (string, json.RawMessage) {
	hc.t.Helper()
	_ = hc.nc.SetReadDeadline(time.Now().Add(d))
	defer func() { _ = hc.nc.SetReadDeadline(time.Time{}) }()
	line, err := hc.lr.ReadLine()
	if err != nil {
		hc.t.Fatalf("the hub's next line: %v", err)
	}
	var n protocol.Notification
	if err := json.Unmarshal(line, &n); err != nil {
		hc.t.Fatal(err)
	}
	return n.Method, n.Params
}

// rosterOf is the hub at sock's roster: each listed host's session, by host
// id, and the rows.
func rosterOf(t *testing.T, sock string) (map[string]string, []protocol.RosterRow) {
	t.Helper()
	var res protocol.HubSessionsListResult
	dialHub(t, sock).call(protocol.MethodSessionsList, struct{}{}, &res)
	m := map[string]string{}
	for _, r := range res.Sessions {
		m[r.HostID] = r.SessionID
	}
	return m, res.Sessions
}

// throughHub dials sessionID's host through the hub at sock with hub.Dialer
// — its reconnect window a step, not its 10 s default: starved, a hub child's
// start takes seconds — and closes it when the test ends.
func throughHub(t *testing.T, env rundir.Env, sock, sessionID string) *remote.Client {
	t.Helper()
	c, err := remote.Dial(stepCtx(t), sock, remote.Options{
		Client:       protocol.ClientInfo{Kind: "test", Name: "through the hub"},
		Connect:      sessionID,
		Dial:         hub.Dialer(env),
		RedialWindow: serveStep,
	})
	if err != nil {
		t.Fatalf("dial %s through the hub: %v", sessionID, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// streamNext is s's next item, within serveStep.
func streamNext(t *testing.T, s *remote.Stream) remote.Item {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), serveStep)
	defer cancel()
	it, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("the stream: %v", err)
	}
	return it
}

// streamUntil reads s up to and including the first item stop holds for.
func streamUntil(t *testing.T, s *remote.Stream, what string, stop func(agent.Event) bool) []remote.Item {
	t.Helper()
	var items []remote.Item
	for {
		it := streamNext(t, s)
		items = append(items, it)
		switch it.Kind {
		case remote.KindEnd, remote.KindError:
			t.Fatalf("the stream stopped before %s: %+v", what, it)
		case remote.KindEvent:
			ev, err := agent.DecodeEvent(string(it.Body))
			if err != nil {
				t.Fatalf("event %d: %v", it.Seq, err)
			}
			if stop(ev) {
				return items
			}
		}
	}
}

// TestAHubKilledMidTurnLosesNothing (plan 032 §3.18, A8, and A9's real-process
// leg): three craze serve hosts and a craze hub. A client reaches one host's
// session through the hub (hub.Dialer), attaches and prompts it into a long
// turn, held running at its first step (the fake agent's gate). The hub is
// killed with SIGKILL, and the turn is let go with no client attached: it
// completes on its host. The client's reconnect redials the dead hub's socket;
// the Dialer starts a hub in its place, which resolves the session from the
// registry at once, and the client's resume hello reaches the same host:
// resumed, its re-attach silent from its cursor — the rest of the turn, no
// gap, no duplicate, no restore. The new hub lists the same hosts and
// sessions. Then a host is killed with SIGKILL, and a subscriber of the new
// hub is told of its row's removal (A9: within one round; the round's bound
// is the in-process TestACrashedHostLeavesWithinOneRound's, with its ticks in
// the test's hands — here the latency is logged). The negative controls: the
// turn was held when the hub died (its end had not been read), the client's
// first hello was not a resume, and the removed host was listed before its
// kill.
func TestAHubKilledMidTurnLosesNothing(t *testing.T) {
	env, _ := serveHome(t)
	gate := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_SCRIPT", "long-turn")
	t.Setenv("CRAZE_FAKE_GATE", gate)
	t.Setenv("CRAZE_FAKE_STEP", "1ms")
	entries, hosts := serveHosts(t, env, 3)
	kids := hubAsChild(t)
	sock, rec := ensureHub(t, env)
	listed, _ := rosterOf(t, sock)
	for _, e := range entries {
		if listed[e.HostID] != e.CrazeSessionID {
			t.Fatalf("the hub lists %v; the registry %+v", listed, entries)
		}
	}

	target := entries[0]
	c := throughHub(t, env, sock, target.CrazeSessionID)
	first := c.Hello()
	if first.Endpoint.HostID != target.HostID || first.Resumed {
		t.Fatalf("the hello through the hub: %+v", first)
	}
	s, err := c.Attach(stepCtx(t), remote.AttachOptions{SessionID: target.CrazeSessionID})
	if err != nil {
		t.Fatal(err)
	}
	att := streamNext(t, s)
	if att.Kind != remote.KindAttached || att.Reply == nil {
		t.Fatalf("the first item: %+v", att)
	}
	if it := streamNext(t, s); it.Kind != remote.KindSynchronized {
		t.Fatalf("after the attach: %+v", it)
	}
	var pr protocol.PromptResult
	if _, err := c.Command(stepCtx(t), protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: target.CrazeSessionID,
		Text: "the long one", Mode: protocol.PromptQueue}, &pr, remote.CommandOptions{}); err != nil || pr.Turn == "" {
		t.Fatalf("the prompt: %+v, %v", pr, err)
	}
	items := streamUntil(t, s, "the first tool", func(ev agent.Event) bool {
		return ev.Type == agent.EventTool && ev.Tool != nil && ev.Tool.ID == "call-t1-step-1"
	})

	// The hub dies mid-turn.
	if err := syscall.Kill(rec.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killed := time.Now()
	waitHubGone(t, rec.PID, hubLog(env))
	// The turn goes on, on its host: both of long-turn's steps let go (each
	// waits for one byte of CRAZE_FAKE_GATE, plan 033 C3r's shared reader).
	released := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(gate, os.O_WRONLY, 0)
		if err == nil {
			_, err = f.Write([]byte{1, 1})
			_ = f.Close()
		}
		released <- err
	}()
	select {
	case err := <-released:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(serveStep):
		t.Fatal("the turn was not at its gate")
	}
	if lt := waitLastTurn(t, target); lt.Outcome != protocol.TurnDone || lt.TurnID != pr.Turn {
		t.Fatalf("the turn's ending on its host: %+v, want %s done", lt, pr.Turn)
	}

	// The client resumes through the hub the Dialer brought back.
	items = append(items, streamUntil(t, s, "the turn's end", func(ev agent.Event) bool {
		return ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.Phase == agent.TurnEnded
	})...)
	t.Logf("the turn's end reached the client through a new hub %v after the kill", time.Since(killed))
	done := false
	want := att.Reply.After.Seq + 1
	for _, it := range items {
		switch it.Kind {
		case remote.KindRestore:
			t.Fatal("a restore: the resume through the new hub was not silent")
		case remote.KindEvent:
			if it.Seq != want {
				t.Fatalf("event %d, want %d: a gap or a duplicate", it.Seq, want)
			}
			want++
			if ev, _ := agent.DecodeEvent(string(it.Body)); ev.Type == agent.EventText && ev.Text == "DONE step1 step2" {
				done = true
			}
		}
	}
	if !done {
		t.Fatal("the turn's reply never reached the client")
	}
	if hc := c.Hello(); !hc.Resumed || hc.ClientID != first.ClientID || hc.Endpoint.HostID != target.HostID {
		t.Fatalf("the hello after the hub's death: %+v, want a resume of %s on %s", hc, first.ClientID, target.HostID)
	}
	second, _, err := rundir.ReadHubRecord(env)
	if err != nil {
		t.Fatal(err)
	}
	if second.PID == rec.PID || second.HubID == rec.HubID || len(kids.pids()) != 2 {
		t.Fatalf("the hub after the kill: %+v (the killed one %+v; spawned %v)", second, rec, kids.pids())
	}
	relisted, _ := rosterOf(t, second.Socket)
	if fmt.Sprint(relisted) != fmt.Sprint(listed) {
		t.Fatalf("the new hub lists %v, the old one listed %v", relisted, listed)
	}

	// A host dies under the new hub: its row leaves.
	sub := dialHub(t, second.Socket)
	var sr protocol.SessionsSubscribeResult
	sub.call(protocol.MethodSessionsSubscribe, struct{}{}, &sr)
	crashed := entries[2]
	if !slices.ContainsFunc(sr.Sessions, func(r protocol.RosterRow) bool { return r.HostID == crashed.HostID }) {
		t.Fatalf("the subscription's roster lacks host %s: %+v", crashed.HostID, sr.Sessions)
	}
	if err := hosts[crashed.HostID].cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for {
		method, params := sub.next(serveStep)
		if method != protocol.NotifyRoster {
			continue
		}
		var rp protocol.RosterParams
		if err := json.Unmarshal(params, &rp); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(rp.Removes, crashed.HostID) {
			break
		}
	}
	t.Logf("the killed host's row left the roster %v after its SIGKILL", time.Since(start))
}

// TestAnOldAndANewHostBehindOneHub (plan 032 §3.18, A10): behind one craze
// hub, an S2-era host — the fake host's defaults: no rowFacts, no stop (and no
// presence, which no host has yet) — and a current craze serve host. Both are
// listed, each row the host's own, its capabilities as the host said them;
// both are spliced to through the hub, each client's hello answered by its
// host; and through each splice the host answers as itself: the S2 host
// refuses session.stop stop_unsupported. The negative control is the current
// host's row, which carries the row facts and stop the S2 host's lacks.
func TestAnOldAndANewHostBehindOneHub(t *testing.T) {
	env, _ := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	entries, _ := serveHosts(t, env, 1)
	current := entries[0]
	const oldID, oldSession = "00000000005e", "session-s2-era"
	old, err := fakehost.New(fakehost.Options{HostID: oldID, CrazeSessionID: oldSession, Workspace: "/work/s2"})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := old.Register(env)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- old.Serve(reg.Listener()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), serveStep)
		defer cancel()
		_ = old.Close(ctx)
		<-served
		_ = reg.Close()
	})
	hubAsChild(t)
	sock, _ := ensureHub(t, env)

	_, rows := rosterOf(t, sock)
	caps := map[string]protocol.SessionCapabilities{}
	for _, r := range rows {
		row, ok, err := r.SessionRow()
		if !ok || err != nil || r.Status != protocol.RosterReachable {
			t.Fatalf("host %s's roster row: %+v (%v)", r.HostID, r, err)
		}
		caps[r.HostID] = row.Capabilities
	}
	if len(caps) != 2 {
		t.Fatalf("the hub lists %d hosts, want the two: %+v", len(caps), rows)
	}
	if c := caps[oldID]; c.Stop || c.RowFacts {
		t.Fatalf("the S2-era host's row says stop %v, rowFacts %v", c.Stop, c.RowFacts)
	}
	if c := caps[current.HostID]; !c.Stop || !c.RowFacts {
		t.Fatalf("the current host's row says stop %v, rowFacts %v", c.Stop, c.RowFacts)
	}

	for _, h := range []struct{ hostID, sessionID string }{{oldID, oldSession}, {current.HostID, current.CrazeSessionID}} {
		c := throughHub(t, env, sock, h.sessionID)
		if hc := c.Hello(); hc.Endpoint.Kind != protocol.EndpointHost || hc.Endpoint.HostID != h.hostID {
			t.Fatalf("the hello through the hub to %s: %+v", h.sessionID, hc.Endpoint)
		}
		var list protocol.SessionsListResult
		if err := c.Call(stepCtx(t), protocol.MethodSessionsList, protocol.SessionsListParams{}, &list); err != nil ||
			len(list.Sessions) != 1 || list.Sessions[0].SessionID != h.sessionID {
			t.Fatalf("sessions.list through the splice to %s: %+v, %v", h.sessionID, list, err)
		}
	}
	c := throughHub(t, env, sock, oldSession)
	_, err = c.Command(stepCtx(t), protocol.MethodSessionStop, protocol.StopParams{SessionID: oldSession}, nil, remote.CommandOptions{})
	var e *remote.Error
	if !errors.As(err, &e) || e.Reason != protocol.ReasonStopUnsupported {
		t.Fatalf("session.stop through the splice to the S2-era host: %v, want stop_unsupported", err)
	}
}
