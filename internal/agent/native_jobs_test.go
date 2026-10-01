package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/tool"
)

// The native adapter's background bash jobs (plan 033 §3.8 "Display", "Wake",
// P12, P14; §7 A11–A13b). The mapping is driven through the sink by hand, as
// the harness reports a job; the wake, its cap and the stop key run a real
// interactive session whose jobs are real commands through the real bash
// tool, each held on a named pipe in the workspace so it ends exactly when the
// test opens the pipe (jobGate) — nothing sleeps to win a race.

// jobRows is the child tool rows published for job id, in order.
func jobRows(evs []Event, id string) []ToolEvent {
	var out []ToolEvent
	for _, ev := range evs {
		if ev.Type == EventTool && ev.Agent == id {
			out = append(out, *ev.Tool)
		}
	}
	return out
}

// stamped reports whether any of rows carries a child's id on its task: what
// subagentStarted's stamp of a call's row leaves.
func stamped(rows []ToolEvent) bool {
	for _, r := range rows {
		if r.Task != nil && r.Task.AgentID != "" {
			return true
		}
	}
	return false
}

// TestNativeJobRows (A11, P12): a job is a roster row — its id and its call's,
// the type "bash job", no model, background, a transcript, the command's first
// line (marked cut) as its description and the whole command as its prompt —
// and a child tool set of one execute row: started and called with the
// command, its live output as progress, and closed with what the command left
// and its exit code, before the row's finished. The bash call's own row is
// never stamped as a task — the negative control beside it: an agent call's
// row is, by its child's SubagentStarted.
func TestNativeJobRows(t *testing.T) {
	s, w := taskSink(t, nil)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cmd := "npm run dev \\\n  -- --port 3000"
	s.sink(harness.ToolStarted{ID: "t1.1.1", Step: 1, Tool: tool.BashTool, Kind: tool.KindExecute})
	s.sink(harness.ToolCalled{ID: "t1.1.1", Request: harness.ToolRequest{Tool: tool.BashTool, Kind: tool.KindExecute, Title: cmd, Command: cmd}})
	s.sink(harness.JobStarted{ID: "t1.1.1", CallID: "t1.1.1", Command: cmd, Workdir: "/work", Limit: 30 * time.Minute, At: at})
	s.sink(harness.ToolFinished{ID: "t1.1.1", Result: tool.Result{Text: "Started the command in the background as job `t1.1.1`.\n" + tool.JobMarker("t1.1.1")}})
	s.sink(harness.JobOutput{ID: "t1.1.1", Output: "ready on :3000\n"})
	w.wait("the job's progress", func(ev Event) bool {
		return ev.Type == EventTool && ev.Agent == "t1.1.1" && ev.Tool.Output != nil && ev.Tool.Output.Stdout == "ready on :3000\n"
	})
	s.sink(harness.JobFinished{ID: "t1.1.1", Status: tool.JobExited, ExitCode: 3, Error: "exit code 3",
		Output: "ready on :3000\nboom\n", Duration: 252 * time.Second, At: at.Add(4 * time.Minute)})
	w.wait("the job's finished row", func(ev Event) bool { return isRoster(ev, "t1.1.1", SubagentChangeFinished) })
	evs := w.events()

	spawned := indexWhere(evs, 0, func(ev Event) bool { return isRoster(ev, "t1.1.1", SubagentChangeSpawned) })
	if spawned < 0 {
		t.Fatalf("no spawned row: %s", strings.Join(w.kinds(), ", "))
	}
	if got, want := *evs[spawned].Subagent, (SubagentInfo{ID: "t1.1.1", ToolCallID: "t1.1.1", Description: "npm run dev \\ …",
		SubagentType: "bash job", Status: SubagentRunning, Prompt: cmd, StartedAt: at, Transcript: true, Background: true}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the spawned row is\n%+v\nwant\n%+v", got, want)
	}
	rows := jobRows(evs, "t1.1.1")
	if len(rows) != 4 {
		t.Fatalf("%d child rows; want started, called, progress, finished: %+v", len(rows), rows)
	}
	if r := rows[0]; r.Status != toolPending || r.Kind != string(tool.KindExecute) || r.ToolName != tool.BashTool {
		t.Fatalf("the started row is %+v", r)
	}
	if r := rows[1]; r.Status != toolInProgress || r.Title != cmd || r.RawInput != cmd {
		t.Fatalf("the called row is %+v", r)
	}
	fin := rows[3]
	if fin.Status != toolFailed || fin.Output == nil || fin.Output.ExitCode == nil || *fin.Output.ExitCode != 3 ||
		fin.Output.Stdout != "ready on :3000\nboom\n" || fin.Output.StderrHead != fin.Output.StdoutHead {
		t.Fatalf("the finished row is %+v (output %+v)", fin, fin.Output)
	}
	first := indexWhere(evs, 0, func(ev Event) bool { return ev.Type == EventTool && ev.Agent == "t1.1.1" })
	closed := lastWhere(evs, func(ev Event) bool { return ev.Type == EventTool && ev.Agent == "t1.1.1" })
	finished := indexWhere(evs, 0, func(ev Event) bool { return isRoster(ev, "t1.1.1", SubagentChangeFinished) })
	if !ascending(spawned, first, closed, finished) {
		t.Fatalf("spawned %d, first child row %d, last child row %d, finished %d; want them in that order", spawned, first, closed, finished)
	}
	if got := evs[finished].Subagent; got.Status != SubagentFailed || got.Error != "exit code 3" || got.Output != "ready on :3000\nboom\n" ||
		got.DurationMs != 252000 || !got.EndedAt.Equal(at.Add(4*time.Minute)) || got.Model != "" || !got.Background {
		t.Fatalf("the finished row is %+v", got)
	}

	// No stamp: the call's row keeps its receipt, never a task.
	if parent := rowsFor(evs, "t1.1.1"); len(parent) != 3 || stamped(parent) || s.isTaskRow("t1.1.1") {
		t.Fatalf("the bash call's rows are %+v; want its own three, none a task", parent)
	}
	// The control: a sub-agent's start does stamp its agent call's row.
	startAgentRow(t, s, "t1.2.1", "scan", "scan the tree")
	s.sink(harness.SubagentStarted{ID: "kid", CallID: "t1.2.1", Type: "explore", Description: "scan", Prompt: "scan the tree", Model: "test/a", At: at})
	w.waitCount("the agent row's stamp", 3, func(ev Event) bool { return ev.Type == EventTool && ev.Agent == "" && ev.Tool.ID == "t1.2.1" })
	if rows := rowsFor(w.events(), "t1.2.1"); stamped(rows[:2]) || !stamped(rows) {
		t.Fatal("control: the agent call's row was not stamped by its child's start, so the check above proves nothing")
	}
}

// TestNativeJobRowStatuses (§3.8's table): each way a job ends is its row's
// status and word, and its execute row's status — an exit code only for one
// that exited.
func TestNativeJobRowStatuses(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name      string
		end       harness.JobFinished
		row       SubagentStatus
		word, set string
		code      *int
	}{
		{"exited 0", harness.JobFinished{Status: tool.JobExited, ExitCode: 0}, SubagentCompleted, "", toolCompleted, new(int)},
		{"stopped by the user", harness.JobFinished{Status: tool.JobStopped, By: tool.JobStoppedByUser, Error: "stopped by the user", ExitCode: -1}, SubagentCancelled, "stopped by the user", toolCancelled, nil},
		{"stopped by the agent", harness.JobFinished{Status: tool.JobStopped, By: tool.JobStoppedByYou, Error: "stopped by the agent", ExitCode: -1}, SubagentCancelled, "stopped by the agent", toolCancelled, nil},
		{"timed out", harness.JobFinished{Status: tool.JobTimedOut, Error: "time limit reached", ExitCode: -1}, SubagentCancelled, "time limit reached", toolCancelled, nil},
		{"failed", harness.JobFinished{Status: tool.JobFailed, Error: "failed", ExitCode: -1}, SubagentFailed, "failed", toolFailed, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, w := taskSink(t, nil)
			s.sink(harness.JobStarted{ID: "t1.1.1", CallID: "t1.1.1", Command: "make", At: at})
			end := c.end
			end.ID, end.Output, end.At = "t1.1.1", "out\n", at.Add(time.Second)
			s.sink(end)
			ev := w.wait("the finished row", func(ev Event) bool { return isRoster(ev, "t1.1.1", SubagentChangeFinished) })
			if ev.Subagent.Status != c.row || ev.Subagent.Error != c.word {
				t.Fatalf("the row ended %q %q; want %q %q", ev.Subagent.Status, ev.Subagent.Error, c.row, c.word)
			}
			rows := jobRows(w.events(), "t1.1.1")
			last := rows[len(rows)-1]
			if last.Status != c.set || last.Output == nil || last.Output.Stdout != "out\n" {
				t.Fatalf("the execute row ended %+v (output %+v); want %q", last, last.Output, c.set)
			}
			if (last.Output.ExitCode == nil) != (c.code == nil) || (c.code != nil && *last.Output.ExitCode != *c.code) {
				t.Fatalf("the execute row's exit code is %v; want %v", last.Output.ExitCode, c.code)
			}
		})
	}
}

// TestNativeJobRowsEvicted (S1c's rule, which the fold replays): finished job
// rows are evicted with the sub-agents' past the roster's cap, oldest finish
// first, and an evicted job's tool set goes with its row, so a late progress
// snapshot for it publishes nothing.
func TestNativeJobRowsEvicted(t *testing.T) {
	s, w := taskSink(t, nil)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	life := func(i int) {
		id := fmt.Sprintf("t1.%d.1", i)
		s.sink(harness.JobStarted{ID: id, CallID: id, Command: "job " + id, At: at})
		s.sink(harness.JobFinished{ID: id, Status: tool.JobExited, At: at})
	}
	for i := 1; i <= subagentFinishedCap; i++ {
		life(i)
	}
	if snap := s.Snapshot().Subagents; len(snap) != subagentFinishedCap || snap[0].ID != "t1.1.1" {
		t.Fatalf("control: at the cap the roster is %v; want all %d kept", ids(snap), subagentFinishedCap)
	}
	life(subagentFinishedCap + 1)
	snap := s.Snapshot().Subagents
	if len(snap) != subagentFinishedCap || snap[0].ID != "t1.2.1" {
		t.Fatalf("past the cap the roster is %v; want t1.1.1, the oldest finish, evicted", ids(snap))
	}
	s.toolMu.Lock()
	_, kept := s.childTools["t1.1.1"]
	_, live := s.childTools["t1.2.1"]
	s.toolMu.Unlock()
	if kept || !live {
		t.Fatalf("tool sets: evicted kept %v, live kept %v", kept, live)
	}
	marker := func(text string) {
		s.emit(Event{Type: EventText, Text: text})
		w.wait(text, func(ev Event) bool { return ev.Type == EventText && ev.Text == text })
	}
	marker("before")
	before := len(w.events())
	s.sink(harness.JobOutput{ID: "t1.1.1", Output: "late"})
	marker("after")
	for _, ev := range w.events()[before:] {
		if ev.Agent == "t1.1.1" {
			t.Fatalf("an evicted job published %+v", ev)
		}
	}
}

// jobGate makes a named pipe in dir: a command that reads it waits until the
// test opens it (openJobGate).
func jobGate(t *testing.T, dir, name string) string {
	t.Helper()
	path := dir + "/" + name
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// openJobGate writes a line to the named pipe path, waiting for its reader:
// the command reading it goes on, and ends.
func openJobGate(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("x\n")
	_ = f.Close()
}

// bgBashCall is one bash call with run_in_background.
func bgBashCall(t *testing.T, id, command string) []fantasy.StreamPart {
	t.Helper()
	return nativeCallParts(id, tool.BashTool, nativeArgs(t, map[string]any{"command": command, "run_in_background": true}))
}

// newJobRig is a wake rig whose workspace is ws, where the jobs' pipes are.
func newJobRig(t *testing.T) (*wakeRig, string) {
	t.Helper()
	ws := t.TempDir()
	return newWakeRig(t, Options{Workspace: ws}, nil), ws
}

// exitedBlock is §3.7's delivered result for a job that exited 0 on the
// session's fixed clock: the block a wake's request carries.
func exitedBlock(id, command, output string) string {
	return `<background_command id="` + id + `" status="exited" exit_code="0" duration="0s">` + "\n$ " + command + "\n" + output + "\n</background_command>"
}

// jobResultRequests are the parent's requests whose last user message
// carries a job's delivered result.
func (rig *wakeRig) jobResultRequests() []fantasy.Call {
	var out []fantasy.Call
	for _, c := range rig.a.requests() {
		m := c.Prompt[len(c.Prompt)-1]
		if m.Role == fantasy.MessageRoleUser && strings.Contains(messageTexts(m), "<background_command") {
			out = append(out, c)
		}
	}
	return out
}

// awaitClaim waits for the worker's recheck that claims, passing over the
// ones that stood down meanwhile (a fence up, a kick folded into another).
func (rig *wakeRig) awaitClaim(why string) {
	rig.t.Helper()
	for !await(rig.t, rig.decided, "a recheck that claims ("+why+")") {
	}
}

// TestNativeJobWakeReason (§3.8 "Wake", A11): a job's result finishing while
// nothing runs starts a wake whose brackets carry ReasonJobWake and whose
// opening reads "background command result", its request the job's block —
// and the job's own rows came and went without stamping the call's row. A
// wake that finds a sub-agent's result pending beside a job's is a sub-agent
// wake, as before: the negative control.
func TestNativeJobWakeReason(t *testing.T) {
	rig, ws := newJobRig(t)
	s, w := rig.s, rig.w
	g1, g2 := jobGate(t, ws, "g1"), jobGate(t, ws, "g2")
	rig.a.route("go", callsStep(bgBashCall(t, "c1", "read line < g1; echo got $line")), answer("started"),
		answer("noted"))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	w.waitType(EventDone)
	rig.awaitDecided(false, "the turn's release, the job running")
	spawned := w.wait("the job's row", func(ev Event) bool { return isRoster(ev, "t1.1.1", SubagentChangeSpawned) })
	if sub := spawned.Subagent; sub.SubagentType != BashJobType || !sub.Background || sub.Model != "" || sub.Description != "read line < g1; echo got $line" {
		t.Fatalf("the job's row is %+v", sub)
	}
	if stamped(rowsFor(w.events(), "t1.1.1")) {
		t.Fatal("the bash call's row was stamped as a task")
	}

	openJobGate(t, g1)
	rig.awaitPending("the job t1.1.1")
	rig.awaitDecided(true, "the job's result pending")
	rig.bracketAs(true, 1, "wake-1", ReasonJobWake, jobWakeText)
	rig.bracketAs(false, 1, "wake-1", ReasonJobWake, "")
	rig.awaitDecided(false, "the wake's ending")
	reqs := rig.jobResultRequests()
	if want := exitedBlock("t1.1.1", "read line < g1; echo got $line", "got x"); len(reqs) != 1 || lastUserTexts(t, reqs[0]) != want {
		t.Fatalf("%d requests carry a job's result; want the wake's one, ending %q", len(reqs), want)
	}
	if fin := w.wait("the job's finished row", func(ev Event) bool { return isRoster(ev, "t1.1.1", SubagentChangeFinished) }); fin.Subagent.Status != SubagentCompleted {
		t.Fatalf("the job's finished row is %+v", fin.Subagent)
	}

	// The control: a sub-agent's result and a job's, both pending when the
	// wake claims — the fence held it off meanwhile — is a sub-agent wake.
	child := newHeld(t)
	rig.a.route("go", callsStep(bgCall(t, "a1", "kid", "child work"), bgBashCall(t, "c2", "read line < g2")), answer("both"),
		answer("noted both"))
	rig.a.route("child work", child.step(openTextParts("did "), closeTextParts("work")))
	if _, err := s.Prompt(context.Background(), "more"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	w.waitCount("the second turn's end", 2, func(ev Event) bool { return ev.Type == EventDone })
	s.FenceUp()
	kid := spawnedWith(t, w.events(), "child work")
	await(t, child.reached, "the child's step")
	close(child.release)
	rig.awaitPending("the child " + kid)
	openJobGate(t, g2)
	rig.awaitPending("the job t3.1.2") // turn 2 was the wake
	if sub, jobs := s.hs.PendingKinds(); !sub || !jobs || s.ForeignTurn() {
		t.Fatalf("premise: pending sub-agent %v, job %v, foreign %v", sub, jobs, s.ForeignTurn())
	}
	s.FenceDown()
	rig.awaitClaim("the fence down")
	rig.bracketAs(true, 2, "wake-2", ReasonSubagentWake, wakeText)
	rig.bracketAs(false, 2, "wake-2", ReasonSubagentWake, "")
}

// TestNativeJobWakeChainCap (A13b, P14): three jobs finishing one after
// another while nothing runs wake the session three times; the fourth wakes
// nothing — its result is set aside, owed to nobody, so a detached host with
// nothing else running goes idle — and the next turn a person starts delivers
// it at its step 0, which starts the chain again: a fifth job's result wakes
// the session as the first did.
func TestNativeJobWakeChainCap(t *testing.T) {
	rig, ws := newJobRig(t)
	s, w := rig.s, rig.w
	var gates []string
	var calls [][]fantasy.StreamPart
	for i := 1; i <= 4; i++ {
		gates = append(gates, jobGate(t, ws, fmt.Sprintf("g%d", i)))
		calls = append(calls, bgBashCall(t, fmt.Sprintf("c%d", i), fmt.Sprintf("read line < g%d", i)))
	}
	rig.a.route("go", callsStep(calls...), answer("four started"))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	w.waitType(EventDone)
	rig.awaitDecided(false, "the turn's release, the jobs running")
	if !s.OwesWork() {
		t.Fatal("running jobs are not owed: a detached host would stop under them")
	}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("t1.1.%d", i)
		rig.a.route("go", answer(fmt.Sprintf("noted %d", i)))
		openJobGate(t, gates[i-1])
		rig.awaitPending("the job " + id)
		rig.awaitDecided(true, "job "+id+"'s result")
		wake := fmt.Sprintf("wake-%d", i)
		rig.bracketAs(true, i, wake, ReasonJobWake, jobWakeText)
		rig.bracketAs(false, i, wake, ReasonJobWake, "")
		rig.awaitDecided(false, "wake "+wake+"'s ending")
	}

	// The fourth: its row finishes, its result is set aside.
	openJobGate(t, gates[3])
	w.wait("the fourth job's finished row", func(ev Event) bool { return isRoster(ev, "t1.1.4", SubagentChangeFinished) })
	waitFor(t, "the fourth job's result set aside", func() bool { return !s.OwesWork() })
	running := false
	for _, row := range s.Snapshot().Subagents {
		running = running || row.Status == SubagentRunning
	}
	if s.ForeignTurn() || running || s.hs.HasPending() {
		t.Fatalf("after the cap: foreign %v, a row running %v, pending %v; want a host free to go idle", s.ForeignTurn(), running, s.hs.HasPending())
	}

	// The next turn a person starts delivers it at its step 0, and starts a
	// fifth job; the chain starts again, so its result wakes the session.
	g5 := jobGate(t, ws, "g5")
	rig.a.route("go", callsStep(bgBashCall(t, "c5", "read line < g5")), answer("back"), answer("noted 5"))
	if _, err := s.Prompt(context.Background(), "back"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	w.waitCount("the person's turn's end", 2, func(ev Event) bool { return ev.Type == EventDone })
	rig.awaitDecided(false, "the person's turn's release")
	var carried []string
	for _, c := range rig.jobResultRequests() {
		carried = append(carried, lastUserTexts(t, c))
	}
	fourth := exitedBlock("t1.1.4", "read line < g4", "(no output)")
	if len(carried) != 4 || carried[3] != fourth {
		t.Fatalf("the requests carrying a job's result end %q; want three wakes' and the person's turn's, the fourth job's", carried)
	}
	if n := w.count(EventForeignTurn); n != 6 {
		t.Fatalf("%d brackets; want three wakes' pairs, and no wake for the fourth job", n)
	}
	openJobGate(t, g5)
	rig.awaitPending("the fifth job, t5.1.1") // turns 2-4 were the wakes
	rig.awaitDecided(true, "the fifth job's result, the chain reset")
	rig.bracketAs(true, 4, "wake-4", ReasonJobWake, jobWakeText)
}

// TestNativeJobStopKey (A12, §3.8's table): the stop key on a job's row —
// CancelSubagent with its id, as the TUI's del sends it — stops the command;
// its row ends cancelled, "stopped by the user", and the wake tells the model
// `stopped by="the user"`. An id that names nothing running — an unknown one,
// or the job once it has ended — keeps the stored refusal.
func TestNativeJobStopKey(t *testing.T) {
	rig, ws := newJobRig(t)
	s, w := rig.s, rig.w
	jobGate(t, ws, "g1") // never opened: only the stop ends it
	rig.a.route("go", callsStep(bgBashCall(t, "c1", "read line < g1")), answer("started"), answer("it was stopped"))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	w.waitType(EventDone)
	rig.awaitDecided(false, "the turn's release, the job running")
	if err := s.CancelSubagent("t9.9.9"); !errors.Is(err, ErrNoSuchSubagent) {
		t.Fatalf("the stop key on an unknown id = %v; want ErrNoSuchSubagent", err)
	}
	if err := s.CancelSubagent("t1.1.1"); err != nil {
		t.Fatalf("the stop key on the running job = %v", err)
	}
	fin := w.wait("the job's finished row", func(ev Event) bool { return isRoster(ev, "t1.1.1", SubagentChangeFinished) })
	if fin.Subagent.Status != SubagentCancelled || fin.Subagent.Error != "stopped by the user" {
		t.Fatalf("the stopped job's row is %+v", fin.Subagent)
	}
	rig.awaitPending("the stopped job")
	rig.awaitDecided(true, "the stopped job's result")
	rig.bracketAs(true, 1, "wake-1", ReasonJobWake, jobWakeText)
	rig.bracketAs(false, 1, "wake-1", ReasonJobWake, "")
	reqs := rig.jobResultRequests()
	want := `<background_command id="t1.1.1" status="stopped" by="the user" duration="0s">` + "\n$ read line < g1\n(no output)\n</background_command>"
	if len(reqs) != 1 || lastUserTexts(t, reqs[0]) != want {
		t.Fatalf("%d requests carry a job's result; want the wake's one, ending %q", len(reqs), want)
	}
	if err := s.CancelSubagent("t1.1.1"); !errors.Is(err, ErrNoSuchSubagent) {
		t.Fatalf("the stop key on the ended job = %v; want ErrNoSuchSubagent", err)
	}
}

// TestNativeJobEndsWithTheSession (§3.8's table, owner decision 3): a job
// still running when the session closes is killed through the harness's
// Close — no wake, nothing delivered — and its row's finished, the one record
// of it, reaches the journal: cancelled, "stopped: the session closed". Close
// returns in its bound; the control is the row running before it.
func TestNativeJobEndsWithTheSession(t *testing.T) {
	ws := t.TempDir()
	dir := ws + "/journal"
	rig := newWakeRig(t, Options{Workspace: ws, JournalDir: dir}, nil)
	s, w := rig.s, rig.w
	jw := journalOf(t, s.log)
	inc := s.Incarnation()
	jobGate(t, ws, "g1") // never opened
	rig.a.route("go", callsStep(bgBashCall(t, "c1", "read line < g1")), answer("started"))
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	w.waitType(EventDone)
	rig.awaitDecided(false, "the turn's release, the job running")
	if rows := s.Snapshot().Subagents; len(rows) != 1 || rows[0].Status != SubagentRunning {
		t.Fatalf("control: the roster before Close is %+v; want the job running", rows)
	}

	closeJournaled(t, s, jw)
	if rows := s.Snapshot().Subagents; len(rows) != 1 || rows[0].Status != SubagentCancelled || rows[0].Error != "stopped: the session closed" {
		t.Fatalf("the roster after Close is %+v; want the job cancelled by the close", rows)
	}
	var finished []map[string]any
	for _, l := range assertOneJournal(t, dir, jw, inc) {
		ev, _ := l["event"].(map[string]any)
		if sub, _ := ev["subagent"].(map[string]any); l["type"] == "event" && ev["subagentChange"] == SubagentChangeFinished && sub["id"] == "t1.1.1" {
			finished = append(finished, sub)
		}
	}
	if len(finished) != 1 || finished[0]["status"] != string(SubagentCancelled) || finished[0]["error"] != "stopped: the session closed" {
		t.Fatalf("the journal's finished rows for the job are %v; want one, cancelled by the close", finished)
	}
	if n := w.count(EventForeignTurn); n != 0 {
		t.Fatalf("%d brackets: a job the close killed woke the session", n)
	}
}

// TestNativeJobRowStartsAtItsCommand (plan 033 C10r, V3 F4): a promoted job's
// roster row starts when its command did (JobStarted.Began), its foreground
// phase included — the time its finished row's duration counts from — not at
// the promotion that spawned it; a harness that names no start leaves the
// hand-over (At), the control.
func TestNativeJobRowStartsAtItsCommand(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	began := at.Add(-10 * time.Second)
	for _, c := range []struct {
		name  string
		began time.Time
		want  time.Time
	}{
		{"promoted", began, began},
		{"no start named", time.Time{}, at},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, w := taskSink(t, nil)
			s.sink(harness.JobStarted{ID: "t1.1.1", CallID: "t1.1.1", Command: "make", Limit: 30 * time.Minute, Promoted: true, At: at, Began: c.began})
			w.wait("the job's spawned row", func(ev Event) bool { return isRoster(ev, "t1.1.1", SubagentChangeSpawned) })
			evs := w.events()
			row := evs[indexWhere(evs, 0, func(ev Event) bool { return isRoster(ev, "t1.1.1", SubagentChangeSpawned) })].Subagent
			if !row.StartedAt.Equal(c.want) {
				t.Fatalf("the row starts %v; want %v", row.StartedAt, c.want)
			}
		})
	}
}
