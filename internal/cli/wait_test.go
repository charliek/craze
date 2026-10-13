package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// craze wait, cancel and stop as commands (wait.go, cancel_stop.go; plan 041
// §3.1, §7 W1): run in this process through the root command, against the
// waiter's rig (turnwait_test.go), a fake host, or a real craze serve over
// the fake ACP agent.

// crazeRun is one craze command line run on a goroutine of its own.
type crazeRun struct {
	done           chan struct{}
	stdout, stderr string
	code           int
}

func goCraze(argv ...string) *crazeRun {
	r := &crazeRun{done: make(chan struct{})}
	go func() {
		r.stdout, r.stderr, r.code = executeErr(argv)
		close(r.done)
	}()
	return r
}

// wait is the run's output and exit code, within stubWatchdog.
func (r *crazeRun) wait(t *testing.T) (stdout, stderr string, code int) {
	t.Helper()
	return r.within(t, stubWatchdog)
}

// within is the run's output and exit code, within d: wait's, for a run whose
// hub and host start take their time on a loaded machine.
func (r *crazeRun) within(t *testing.T, d time.Duration) (stdout, stderr string, code int) {
	t.Helper()
	select {
	case <-r.done:
		return r.stdout, r.stderr, r.code
	case <-time.After(d):
		t.Fatalf("craze did not return within %v", d)
		return "", "", 0
	}
}

// withWaitSignals hands the waiting commands a signal channel of the test's.
func withWaitSignals(t *testing.T) chan os.Signal {
	t.Helper()
	ch := make(chan os.Signal, 2)
	prev := waitSignals
	waitSignals = func() (<-chan os.Signal, func()) { return ch, func() {} }
	t.Cleanup(func() { waitSignals = prev })
	return ch
}

// withAttachedBarrier is each craze wait's attach answered, one receive a
// run (waitAttachedHook): what a test sends a signal after when it means the
// wait's, not the attach's.
func withAttachedBarrier(t *testing.T) func() {
	t.Helper()
	ch := make(chan struct{}, 4)
	prev := waitAttachedHook
	waitAttachedHook = func() { ch <- struct{}{} }
	t.Cleanup(func() { waitAttachedHook = prev })
	return func() {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(stubWatchdog):
			t.Fatalf("craze wait was not attached within %v", stubWatchdog)
		}
	}
}

// withTimeoutAfterTheAttach hands a waiting command's --timeout (timeoutAt) a
// channel of the test's: attached is each waiter's attachment reply folded
// (waitTaken) — its target met — one receive a run, and fire ends --timeout.
// Its wall clock decides nothing then: one a loaded runner runs past before
// the reply is folded is a wait with no target (exit 1). fire may be called
// more than once. A test that starts a command registers its join (joinAt)
// after this, so a failure ends the command before the hooks are restored.
func withTimeoutAfterTheAttach(t *testing.T) (attached, fire func()) {
	t.Helper()
	ch, taken := make(chan time.Time), make(chan struct{}, 4)
	var once sync.Once
	prevAt, prevTaken := timeoutAt, waitTaken
	timeoutAt = func(deadline time.Time) (<-chan time.Time, func()) {
		if deadline.IsZero() {
			return nil, func() {}
		}
		return ch, func() {}
	}
	waitTaken = func(p pumped) {
		if p.err == nil && p.it.Kind == remote.KindAttached {
			select {
			case taken <- struct{}{}:
			default:
			}
		}
	}
	t.Cleanup(func() { timeoutAt, waitTaken = prevAt, prevTaken })
	return func() {
		t.Helper()
		select {
		case <-taken:
		case <-time.After(stubWatchdog):
			t.Fatalf("craze wait did not fold its attachment within %v", stubWatchdog)
		}
	}, func() { once.Do(func() { close(ch) }) }
}

// joinAt ends run when the test does — fire, then wait for its exit — before
// the cleanups registered earlier (withTimeoutAfterTheAttach's restore of the
// hooks the command reads) run: cleanups run last-registered first.
func joinAt(t *testing.T, run *crazeRun, fire func()) {
	t.Cleanup(func() {
		fire()
		select {
		case <-run.done:
		case <-time.After(stubWatchdog):
		}
	})
}

// decodeResult is craze wait --json's one object.
func decodeResult(t *testing.T, stdout string) map[string]any {
	t.Helper()
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "}\n") {
		t.Fatalf("--json printed %q, want one object on one line", stdout)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatalf("--json printed %q: %v", stdout, err)
	}
	return m
}

// TestWaitPlainReportsTheLastTurn is §7 W1 3 for a session nothing runs in:
// plain craze wait reports its last turn at once, partial — and a turn that
// wrote no text is message "" (plain: `(no final message)`), never the turn
// before's, for a turn of craze's own and for one the agent ran itself.
func TestWaitPlainReportsTheLastTurn(t *testing.T) {
	r := newWaitRig(t, 0, answerTurn(textEv("an earlier reply")), answerTurn(toolEv("t1", "execute", "completed")))
	r.submit("one")
	r.waitIdle()
	stdout, stderr, code := executeErr([]string{"wait", waitRigShort, "--json"})
	if code != 0 {
		t.Fatalf("craze wait exited %d: %s%s", code, stdout, stderr)
	}
	m := decodeResult(t, stdout)
	if m["status"] != "completed" || m["turn"] != "turn-1" || m["message"] != "an earlier reply" || m["partial"] != true {
		t.Fatalf("the first turn's result: %v", m)
	}
	r.submit("two")
	waitFor(t, "the second turn's end", func() bool {
		lt := r.eng.State().LastTurn
		return lt != nil && lt.TurnID == "turn-2"
	})
	stdout, _, code = executeErr([]string{"wait", waitRigShort, "--json"})
	m = decodeResult(t, stdout)
	if code != 0 || m["turn"] != "turn-2" || m["message"] != "" || m["toolCalls"] != float64(1) || m["stopReason"] != nil {
		t.Fatalf("a turn with no text: exit %d, %v", code, m)
	}
	stdout, _, code = executeErr([]string{"wait", waitRigShort})
	if code != 0 || !strings.HasPrefix(stdout, "(no final message)\n---\ncompleted (partial) · ") ||
		!strings.HasSuffix(stdout, "\nsession 0000a001 · craze attach --session 0000a001\n") {
		t.Fatalf("plain: exit %d:\n%s", code, stdout)
	}
	r.s.setForeign(true)
	r.s.emit(toolEv("f1", "read", "completed"))
	r.s.setForeign(false)
	waitFor(t, "the agent's own turn's end", func() bool {
		lt := r.eng.State().LastTurn
		return lt != nil && lt.TurnID == "interject-fallback-1"
	})
	stdout, _, code = executeErr([]string{"wait", waitRigShort, "--json"})
	m = decodeResult(t, stdout)
	if code != 0 || m["status"] != "completed" || m["turn"] != "interject-fallback-1" || m["message"] != "" {
		t.Fatalf("the agent's own turn with no text: exit %d, %v", code, m)
	}
}

// TestWaitNeverPromptedIsIdle: a session that has had no turn is idle, exit
// 0, with nothing but the session's facts.
func TestWaitNeverPromptedIsIdle(t *testing.T) {
	newWaitRig(t, 0)
	stdout, stderr, code := executeErr([]string{"wait", waitRigShort, "--json"})
	m := decodeResult(t, stdout)
	if code != 0 || m["status"] != "idle" || m["turn"] != nil || m["message"] != nil || m["session"] != waitRigID ||
		m["short"] != waitRigShort || m["attach"] != "craze attach --session 0000a001" {
		t.Fatalf("craze wait on a session never prompted: exit %d, %v, %s", code, m, stderr)
	}
}

// TestWaitTimeoutThenFrom is §7 W1 4 as commands: --timeout on a longer turn
// is `running`, exit 5, with the handle and the exact command that follows
// it; `wait --from` that handle after the turn ended reports it (partial:
// plain wait met it mid-turn — its attach's snapshot shows it running, and
// --timeout ends once the waiter has folded that).
func TestWaitTimeoutThenFrom(t *testing.T) {
	r := newWaitRig(t, 0)
	g := r.gate()
	r.script(heldTurn(g, textEv("late"), textEv(" but whole")))
	r.submit("go")
	r.waitStartedInSnapshot("turn-1")
	attached, fire := withTimeoutAfterTheAttach(t)
	run := goCraze("wait", waitRigShort, "--timeout", "1h")
	joinAt(t, run, fire)
	attached()
	fire()
	stdout, stderr, code := run.wait(t)
	if code != 5 {
		t.Fatalf("craze wait --timeout exited %d: %s%s", code, stdout, stderr)
	}
	m := regexp.MustCompile(`(?m)^next: craze wait 0000a001 --from (\S+)$`).FindStringSubmatch(stdout)
	if m == nil || !strings.HasPrefix(stdout, "(no final message)\n---\nrunning (partial) · ") {
		t.Fatalf("the running result:\n%s", stdout)
	}
	h, err := parseHandle(m[1])
	if err != nil || h.target != (waitTarget{kind: targetTurn, id: "turn-1"}) {
		t.Fatalf("the handle %q: %+v %v", m[1], h, err)
	}
	g.open()
	r.waitIdle()
	stdout, stderr, code = executeErr([]string{"wait", waitRigShort, "--from", m[1], "--json"})
	res := decodeResult(t, stdout)
	if code != 0 || res["status"] != "completed" || res["message"] != "late but whole" || res["turn"] != "turn-1" ||
		res["handle"] != nil || res["stopReason"] != "end_turn" {
		t.Fatalf("craze wait --from after the end: exit %d, %v, %s", code, res, stderr)
	}
}

// TestWaitFromOwnsItsTarget is §7 W1 8 through the command: `wait --from`'s
// signal cancels the handle's turn by its id (cancelled, exit 3); plain
// `wait`'s is exit 1 and the turn goes on.
func TestWaitFromOwnsItsTarget(t *testing.T) {
	r := signalRig(t)
	sigs := withWaitSignals(t)
	attached := withAttachedBarrier(t)
	r.submit("someone else's")
	r.waitStarted("turn-1")

	plain := goCraze("wait", waitRigShort)
	attached()
	sigs <- syscall.SIGTERM
	stdout, stderr, code := plain.wait(t)
	// The handle is named once the waiter has met the turn, which the
	// signal can beat here (TestWaitTurnSignals pins it, with a barrier).
	if code != 1 || stdout != "" || !regexp.MustCompile(`^craze wait: interrupted; the turn goes on(; craze wait 0000a001 --from \S+ follows it)?\n$`).MatchString(stderr) {
		t.Fatalf("plain wait's signal: exit %d, %q %q", code, stdout, stderr)
	}
	if st := r.eng.State(); st.Turn != "turn-1" || r.s.cancelsReturned() != 0 {
		t.Fatalf("plain wait touched the session: %+v", st)
	}

	handle := turnHandle{incarnation: r.eng.State().Incarnation, seq: 0, target: waitTarget{kind: targetTurn, id: "turn-1"}}
	owned := goCraze("wait", waitRigShort, "--from", handle.String(), "--json")
	attached()
	sigs <- syscall.SIGINT
	stdout, stderr, code = owned.wait(t)
	m := decodeResult(t, stdout)
	if code != 3 || m["status"] != "cancelled" || m["turn"] != "turn-1" || m["stopReason"] != "cancelled" || stderr != "" {
		t.Fatalf("wait --from's signal: exit %d, %v, %q", code, m, stderr)
	}
	if lt := r.eng.State().LastTurn; lt == nil || lt.Outcome != "cancelled" {
		t.Fatalf("the turn ended %+v", lt)
	}
}

// TestWaitNeedsInputOnAnOpenPermission is §7 W1 6 against a real craze
// serve: the fake ACP agent's permission script under --no-force parks a
// permission no client answers; craze wait is needs_input, exit 4, naming
// the ask, with a handle; a prompt queued behind it, followed from its
// handle, is needs_input blocked by the turn that holds the ask.
func TestWaitNeedsInputOnAnOpenPermission(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "permission")
	srv := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--no-force")
	e := srv.waitServing(t, env, true)
	short := psShort(e.CrazeSessionID)
	promptUnattached(t, e, "run the shell")
	waitFor(t, "the permission to open", func() bool { return listRow(t, e).PendingAsks == 1 })

	stdout, stderr, code := executeErr([]string{"wait", short, "--json"})
	m := decodeResult(t, stdout)
	ask, _ := m["ask"].(map[string]any)
	if code != 4 || m["status"] != "needs_input" || m["turn"] != "turn-1" || ask["kind"] != "permission" ||
		ask["summary"] != "Shell" || ask["id"] == "" || m["handle"] == nil || m["message"] != nil {
		t.Fatalf("craze wait on an open permission: exit %d, %v, %s", code, m, stderr)
	}
	stdout, _, code = executeErr([]string{"wait", short})
	if code != 4 || !strings.Contains(stdout, "\nask: permission Shell (") ||
		!strings.Contains(stdout, "\nnext: answer it in craze attach --session "+short+" (or craze cancel "+short+", craze stop "+short+"), then craze wait "+short+" --from ") {
		t.Fatalf("plain needs_input: exit %d:\n%s", code, stdout)
	}

	// A prompt queued behind the parked turn, and its handle: the row, from
	// the stream's place once it is queued.
	c, err := remote.Dial(stepCtx(t), e.Socket, remote.Options{PeerCheck: rundir.DialCheck(os.Geteuid())})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var pr protocol.PromptResult
	if _, err := c.Command(stepCtx(t), protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: e.CrazeSessionID,
		Text: "and then this", Mode: protocol.PromptQueue}, &pr, remote.CommandOptions{}); err != nil {
		t.Fatal(err)
	}
	row, err := agent.DecodeQueuedPrompt(pr.Queued)
	if err != nil {
		t.Fatalf("the prompt was not queued: %+v, %v", pr, err)
	}
	var sync protocol.SyncResult
	if err := c.Call(stepCtx(t), protocol.MethodSessionSync, protocol.SyncParams{SessionID: e.CrazeSessionID}, &sync); err != nil {
		t.Fatal(err)
	}
	inc := listRow(t, e).Incarnation
	h := turnHandle{incarnation: inc, seq: sync.Seq, target: waitTarget{kind: targetRow, id: row.ID}}
	stdout, stderr, code = executeErr([]string{"wait", short, "--from", h.String(), "--json"})
	m = decodeResult(t, stdout)
	if code != 4 || m["status"] != "needs_input" || m["row"] != row.ID || m["blockedBy"] != "turn-1" || m["turn"] != nil {
		t.Fatalf("the queued prompt's wait: exit %d, %v, %s", code, m, stderr)
	}
}

// TestCancelAndStop is §7 W1 9: cancel and stop take any id craze ps prints
// — a short id, a prefix — and the full id goes on the wire; cancel with
// nothing running is `nothing to cancel`, exit 0, and with a turn running
// cancels it, whoever started it; stop ends the session.
func TestCancelAndStop(t *testing.T) {
	r := signalRig(t)
	for _, id := range []string{waitRigShort, "0199c0de-1111", waitRigID} {
		stdout, stderr, code := executeErr([]string{"cancel", id})
		if code != 0 || stdout != "nothing to cancel\n" || stderr != "" {
			t.Fatalf("craze cancel %s on an idle session: exit %d, %q %q", id, code, stdout, stderr)
		}
	}
	r.submit("someone else's")
	r.waitStarted("turn-1")
	stdout, stderr, code := executeErr([]string{"cancel", waitRigShort})
	if code != 0 || stdout != "cancelled turn-1\n" || stderr != "" {
		t.Fatalf("craze cancel on a running turn: exit %d, %q %q", code, stdout, stderr)
	}
	waitFor(t, "the cancelled ending", func() bool {
		lt := r.eng.State().LastTurn
		return lt != nil && lt.Outcome == "cancelled"
	})

	h := serveFakeHost(t, r.env, fakehost.Options{HostID: "0000000000b2", CrazeSessionID: "0199c0de-2222-7000-8000-00000000b002", Stop: true})
	stdout, stderr, code = executeErr([]string{"stop", "0000b002"})
	if code != 0 || stdout != "stopped 0000b002\n" || stderr != "" {
		t.Fatalf("craze stop: exit %d, %q %q", code, stdout, stderr)
	}
	select {
	case <-h.StopHeard():
	case <-time.After(stubWatchdog):
		t.Fatal("the host never heard the stop")
	}
}

// TestSessionIdsNoLiveHostServes is §3.1 "Ids": an id no live host serves is
// `the session has ended` when the session index has its row — craze wait
// adding how to reload it — and `no session <id>` when it has none; an id
// several live sessions match names them; none of it is a usage error.
func TestSessionIdsNoLiveHostServes(t *testing.T) {
	newWaitRig(t, 0)
	ended := "0199c0de-3333-7000-8000-00000000c003"
	now := time.Now()
	if err := (&sessions.Store{}).Upsert(sessions.Row{SessionID: "prov-3", Provider: "grok", CWD: "/w", CrazeID: ended,
		CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"wait", "0000c003"}, "craze wait: the session has ended (craze send 0000c003 <prompt> reloads it)\n"},
		{[]string{"cancel", "0000c003"}, "craze cancel: the session has ended\n"},
		{[]string{"stop", ended}, "craze stop: the session has ended\n"},
		{[]string{"wait", "nosuchsession"}, "craze wait: no session nosuchsession\n"},
		{[]string{"stop", "nosuchsession"}, "craze stop: no session nosuchsession\n"},
	} {
		stdout, stderr, code := executeErr(c.argv)
		if code != 1 || stdout != "" || stderr != c.want {
			t.Errorf("craze %q: exit %d, %q %q; want %q", c.argv, code, stdout, stderr, c.want)
		}
	}
	// --from's handle, parsed already, is named with the failure (review
	// round 2, N4): plain, and beside the error in --json's object.
	h := turnHandle{incarnation: "0199c0de-9999-7000-8000-000000000000", seq: 3, target: waitTarget{kind: targetTurn, id: "turn-1"}}
	ended3 := "the session has ended (craze send 0000c003 <prompt> reloads it)"
	stdout, stderr, code := executeErr([]string{"wait", "0000c003", "--from", h.String()})
	if code != 1 || stdout != "" || stderr != "craze wait: "+ended3+" (handle "+h.String()+")\n" {
		t.Errorf("wait --from an ended session: exit %d, %q %q", code, stdout, stderr)
	}
	stdout, stderr, code = executeErr([]string{"wait", "0000c003", "--from", h.String(), "--json"})
	m := decodeResult(t, stdout)
	errObj, _ := m["error"].(map[string]any)
	if code != 1 || errObj["message"] != ended3 || m["handle"] != h.String() || stderr != "craze wait: "+ended3+" (handle "+h.String()+")\n" {
		t.Errorf("wait --from an ended session --json: exit %d, %v, %q", code, m, stderr)
	}
	for _, argv := range [][]string{{"wait"}, {"cancel"}, {"stop"}, {"wait", "a", "b"}, {"wait", waitRigShort, "--from", "garbage"},
		{"wait", waitRigShort, "--timeout", "-1s"}, {"wait", waitRigShort, "--max-message", "-3"}, {"wait", "../x"}} {
		stdout, stderr, code := executeErr(argv)
		if code != 2 || stdout != "" || strings.Count(stderr, "\n") != 1 {
			t.Errorf("craze %q: exit %d, %q %q; want a usage error", argv, code, stdout, stderr)
		}
	}
}

// resultsFile is the file --max-message writes the whole result of r's turn
// to: <short>-<inc>-<turn>.md, inc the incarnation's last 8 characters (C8,
// 1), in the results directory as craze resolves it (resultsPath).
func (r *waitRig) resultsFile(turn string) string {
	return resultsPath(r.t, r.env, "0000a001-"+psShort(r.eng.State().Incarnation)+"-"+turn+".md")
}

// resultsPath is where craze writes a --max-message result named name under
// env, as craze itself resolves it (rundir.ResultsDir): the cache tree
// canonical, so on macOS, whose temp directories are reached through /var →
// /private/var, a path under /private/var — never a join on the unresolved
// HOME, which names the same file by another path and so never equals the
// one craze prints (C10). The resolver is the product's own, so its answer is
// first checked against the location worked out apart from it (C11): HOME,
// its links resolved, then .cache/craze/results — a product that moved the
// results elsewhere would otherwise agree with itself.
func resultsPath(t *testing.T, env rundir.Env, name string) string {
	t.Helper()
	dir, err := rundir.ResultsDir(env)
	if err != nil {
		t.Fatal(err)
	}
	home, err := filepath.EvalSymlinks(env.Home)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".cache", "craze", "results"); dir != want {
		t.Fatalf("craze's results directory is %s, want %s", dir, want)
	}
	return filepath.Join(dir, name)
}

// TestWaitMaxMessageBoundsThePlanToo (C3 r3, 4; C8, 2): --max-message's
// characters bound the message and the plan together, the message first — a
// --plan sub-agent's output is its plan — and never more than that (X29's
// first-line allowance gone). The plan gets what the message left, cut back
// to its last whole line when one ends within it; nothing left, it gets
// none. Either way the whole result is in the file messageFile names, which
// plain output's marker names. Each wait replays the turn whole from its
// handle: an automatic plan is in no snapshot (P9).
func TestWaitMaxMessageBoundsThePlanToo(t *testing.T) {
	plan := agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{Name: "p", Plan: "## Steps\n1. one\n2. two", Auto: true, Accepted: true}}
	r := newWaitRig(t, 0, answerTurn(plan, textEv("0123456789")))
	r.submit("go")
	r.waitIdle()
	path := r.resultsFile("turn-1")
	h := turnHandle{incarnation: r.eng.State().Incarnation, target: waitTarget{kind: targetTurn, id: "turn-1"}}
	waitFrom := func(maxMessage ...string) (string, string, int) {
		return executeErr(append([]string{"wait", waitRigShort, "--from", h.String(), "--max-message"}, maxMessage...))
	}

	// The message (10) leaves 4: the plan's first 4 characters, no line
	// ending within them.
	stdout, stderr, code := waitFrom("14")
	if code != 0 || !strings.HasPrefix(stdout, "0123456789\n\nplan:\n## S\n[… cut: full result in "+path+"]\n---\ncompleted · ") {
		t.Fatalf("plain --max-message over a plan: exit %d, %q %q", code, stdout, stderr)
	}
	whole, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(whole), "0123456789\n\nplan:\n## Steps\n1. one\n2. two\n---\ncompleted · ") {
		t.Fatalf("the whole result's file: %q, %v", whole, err)
	}
	stdout, _, code = waitFrom("14", "--json")
	m := decodeResult(t, stdout)
	if code != 0 || m["message"] != "0123456789" || m["plan"] != "## S" || m["messageFile"] != path {
		t.Fatalf("--json --max-message over a plan: exit %d, %v", code, m)
	}
	whole, _ = os.ReadFile(path)
	if full := decodeResult(t, string(whole)); full["plan"] != "## Steps\n1. one\n2. two" || full["messageFile"] != nil {
		t.Fatalf("the whole JSON result's file: %v", full)
	}

	// It leaves 13: up to the end of the plan's last whole line within them.
	stdout, _, code = waitFrom("23", "--json")
	if m := decodeResult(t, stdout); code != 0 || m["plan"] != "## Steps" || m["messageFile"] != path {
		t.Fatalf("--json with 13 left for the plan: exit %d, %v", code, m)
	}

	// The message takes the whole budget: no plan, and the cut said.
	stdout, _, code = waitFrom("10", "--json")
	m = decodeResult(t, stdout)
	if _, hasPlan := m["plan"]; code != 0 || m["message"] != "0123456789" || hasPlan || m["messageFile"] != path {
		t.Fatalf("--json with nothing left for the plan: exit %d, %v", code, m)
	}
	stdout, _, code = waitFrom("10")
	if code != 0 || !strings.HasPrefix(stdout, "0123456789\n[… cut: full result in "+path+"]\n---\n") {
		t.Fatalf("plain with nothing left for the plan: exit %d, %q", code, stdout)
	}

	// Room for both: nothing cut, no file named.
	stdout, _, code = waitFrom("40", "--json")
	if m := decodeResult(t, stdout); code != 0 || m["plan"] != "## Steps\n1. one\n2. two" || m["messageFile"] != nil {
		t.Fatalf("a message and plan that fit: %v", m)
	}
}

// TestWaitMaxMessageIsAStrictBound (C8, 2, the branch review's case): at
// the plugin's --max-message 20000, a 19,999-character message and a plan of
// one 20,000-character line print 20,000 characters of the two — the message
// whole and one of the plan's — then the marker; and the whole result, both
// whole, is in the file the marker names. Under X29's first-line allowance
// the plan's whole line was printed, 39,999 characters, and no file written.
func TestWaitMaxMessageIsAStrictBound(t *testing.T) {
	msg, line := strings.Repeat("m", 19999), strings.Repeat("p", 20000)
	plan := agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{Name: "p", Plan: line, Auto: true, Accepted: true}}
	r := newWaitRig(t, 0, answerTurn(plan, textEv(msg)))
	r.submit("go")
	r.waitIdle()
	path := r.resultsFile("turn-1")
	h := turnHandle{incarnation: r.eng.State().Incarnation, target: waitTarget{kind: targetTurn, id: "turn-1"}}
	stdout, stderr, code := executeErr([]string{"wait", waitRigShort, "--from", h.String(), "--max-message", "20000"})
	body, _, _ := strings.Cut(stdout, "\n---\n")
	kept, marked := strings.CutSuffix(body, "\n[… cut: full result in "+path+"]")
	if code != 0 || !marked || kept != msg+"\n\nplan:\np" {
		t.Fatalf("plain --max-message 20000: exit %d, %d bytes before the footer, the marker %v; %q", code, len(body), marked, stderr)
	}
	whole, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(whole), msg+"\n\nplan:\n"+line+"\n---\n") {
		t.Fatalf("the whole result's file: %d bytes, %v", len(whole), err)
	}
}

// TestWaitMaxMessageAndJSONFailures is §7 W1 10: --max-message cuts a long
// message to its first N characters with the pointer to a file holding the
// whole result (plain: message and footer; --json: the object), and --json's
// exit 1 is one {"error": {"message": …}} object — with the handle beside it
// when there is one — while a usage error stays plain.
func TestWaitMaxMessageAndJSONFailures(t *testing.T) {
	r := newWaitRig(t, 0, answerTurn(textEv("0123456789abcdefghij")))
	r.submit("go")
	r.waitIdle()
	path := r.resultsFile("turn-1")

	stdout, stderr, code := executeErr([]string{"wait", waitRigShort, "--max-message", "10"})
	if code != 0 || !strings.HasPrefix(stdout, "0123456789\n[… cut: full result in "+path+"]\n---\ncompleted (partial) · ") {
		t.Fatalf("plain --max-message: exit %d, %q %q", code, stdout, stderr)
	}
	whole, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(whole), "0123456789abcdefghij\n---\ncompleted (partial) · ") {
		t.Fatalf("the whole result's file: %q, %v", whole, err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the file's mode: %v %v", st, err)
	}

	stdout, _, code = executeErr([]string{"wait", waitRigShort, "--max-message", "10", "--json"})
	m := decodeResult(t, stdout)
	if code != 0 || m["message"] != "0123456789" || m["messageFile"] != path {
		t.Fatalf("--json --max-message: exit %d, %v", code, m)
	}
	whole, _ = os.ReadFile(path)
	full := decodeResult(t, string(whole))
	if full["message"] != "0123456789abcdefghij" || full["messageFile"] != nil {
		t.Fatalf("the whole JSON result's file: %v", full)
	}
	stdout, _, code = executeErr([]string{"wait", waitRigShort, "--max-message", "20", "--json"})
	if m := decodeResult(t, stdout); code != 0 || m["message"] != "0123456789abcdefghij" || m["messageFile"] != nil {
		t.Fatalf("a message that fits: %v", m)
	}

	stdout, stderr, code = executeErr([]string{"wait", "nosuchsession", "--json"})
	if code != 1 || stdout != `{"error":{"message":"no session nosuchsession"}}`+"\n" || stderr != "craze wait: no session nosuchsession\n" {
		t.Fatalf("--json's exit 1: %d, %q %q", code, stdout, stderr)
	}
	foreign := turnHandle{incarnation: "0199c0de-9999-7000-8000-000000000000", seq: 3, target: waitTarget{kind: targetTurn, id: "turn-1"}}
	stdout, stderr, code = executeErr([]string{"wait", waitRigShort, "--from", foreign.String(), "--json"})
	m = decodeResult(t, stdout)
	errObj, _ := m["error"].(map[string]any)
	if code != 1 || errObj["message"] != "the turn's ending was not observed" || m["handle"] != foreign.String() ||
		stderr != "craze wait: the turn's ending was not observed (handle "+foreign.String()+")\n" {
		t.Fatalf("--json's exit 1 with a handle: %d, %v, %q", code, m, stderr)
	}
	stdout, stderr, code = executeErr([]string{"wait", "--json"})
	if code != 2 || stdout != "" || stderr == "" {
		t.Fatalf("a usage error under --json: %d, %q %q", code, stdout, stderr)
	}
}

// TestRelaySignalsHurry (C8, 3): relaySignals' hurry, armed as a piece of
// work answering the first signal (or --timeout's end) begins, closes at the
// next signal from then on, and at once when two have come already; every
// signal still reaches out, none taken by the hurry.
func TestRelaySignalsHurry(t *testing.T) {
	closed := func(c <-chan struct{}) bool {
		select {
		case <-c:
			return true
		default:
			return false
		}
	}
	sigs := make(chan os.Signal)
	out, _, stop, hurry := relaySignals(sigs, func() {})
	defer stop()
	got := func() {
		t.Helper()
		select {
		case <-out:
		case <-time.After(stubWatchdog):
			t.Fatal("a signal never reached out")
		}
	}

	afterNone := hurry()
	if closed(afterNone) {
		t.Fatal("a hurry armed before any signal is closed")
	}
	sigs <- syscall.SIGINT
	got()
	afterOne := hurry()
	if !closed(afterNone) {
		t.Fatal("the first signal did not close the hurry armed before it")
	}
	if closed(afterOne) {
		t.Fatal("a hurry armed after the first signal is closed by it")
	}
	sigs <- syscall.SIGINT
	got()
	if !closed(afterOne) {
		t.Fatal("the second signal did not close the hurry armed after the first")
	}
	if !closed(hurry()) {
		t.Fatal("a hurry armed after two signals is not closed at once")
	}
}

// TestAHurryArmedByTheFirstSignalsWorkIsNotClosedByIt (C8 r1, 1): the work
// the first signal's cancel wakes — a reparented spawn's abandon — may arm a
// hurry before the relay has done anything more; armed from inside that
// cancel, here, it is the earliest it can be. That one signal must not close
// it: only a second does. (The relay counted the signal after the cancel, so
// a hurry armed in that gap was closed by the signal that woke it.)
func TestAHurryArmedByTheFirstSignalsWorkIsNotClosedByIt(t *testing.T) {
	var (
		hurry    func() <-chan struct{}
		inCancel []<-chan struct{}
	)
	cancel := func() { inCancel = append(inCancel, hurry()) }
	sigs := make(chan os.Signal)
	out, _, stop, h := relaySignals(sigs, cancel)
	defer stop()
	hurry = h
	got := func() {
		t.Helper()
		select {
		case <-out:
		case <-time.After(stubWatchdog):
			t.Fatal("a signal never reached out")
		}
	}
	closed := func(c <-chan struct{}) bool {
		select {
		case <-c:
			return true
		default:
			return false
		}
	}
	sigs <- syscall.SIGINT
	got()
	if len(inCancel) != 1 || closed(inCancel[0]) {
		t.Fatalf("the hurry the first signal's own cancel armed is closed by that signal (%d armed)", len(inCancel))
	}
	sigs <- syscall.SIGINT
	got()
	if !closed(inCancel[0]) {
		t.Fatal("the second signal did not close the hurry the first one's work armed")
	}
}

// TestWaitMaxMessageWhenTheFullResultCannotBeSaved (C8 r1, 2): with the
// results directory unusable — a regular file where it goes — a 20-character
// result under --max-message 10 is cut all the same, its bound kept (X35):
// plain output prints the first 10 and a marker saying the full result could
// not be saved, and why; --json carries the cut message and no messageFile;
// stderr says so; the exit is the completed result's, 0. (It used to print
// the whole message, unmarked.)
func TestWaitMaxMessageWhenTheFullResultCannotBeSaved(t *testing.T) {
	r := newWaitRig(t, 0, answerTurn(textEv("0123456789abcdefghij")))
	r.submit("go")
	r.waitIdle()
	cache := filepath.Join(r.env.Home, ".cache", "craze")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "results"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const note = "craze wait: --max-message: the full result could not be saved: "

	stdout, stderr, code := executeErr([]string{"wait", waitRigShort, "--max-message", "10"})
	body, _, _ := strings.Cut(stdout, "\n---\n")
	kept, marker, _ := strings.Cut(body, "\n")
	if code != 0 || kept != "0123456789" || !strings.HasPrefix(marker, "[… cut: the full result could not be saved: ") ||
		!strings.HasSuffix(marker, "]") || strings.Contains(marker, "\n") || !strings.HasPrefix(stderr, note) {
		t.Fatalf("plain --max-message with no results directory: exit %d, %q %q", code, stdout, stderr)
	}

	stdout, stderr, code = executeErr([]string{"wait", waitRigShort, "--max-message", "10", "--json"})
	m := decodeResult(t, stdout)
	if _, named := m["messageFile"]; code != 0 || m["message"] != "0123456789" || named || !strings.HasPrefix(stderr, note) {
		t.Fatalf("--json --max-message with no results directory: exit %d, %v, %q", code, m, stderr)
	}
}
