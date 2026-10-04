package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rivo/uniseg"

	"github.com/charliek/craze/internal/agent"
)

// The row facts (plan 030 §3.8, §3.10): what a sessions.list row says beyond
// State, computed by the engine from its transcript model, its ask registry
// and the times it keeps of each change of row state.

// rowClock is a session clock a test moves by hand: every time the engine
// stamps, the fake session emits and the registry records reads it.
type rowClock struct {
	mu sync.Mutex
	t  time.Time
}

func newRowClock() *rowClock { return &rowClock{t: time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)} }

func (c *rowClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// step moves the clock on a minute and answers the new time.
func (c *rowClock) step() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Minute)
	return c.t
}

// newRowRig is newRig on clk — the fake session's clock and its registry's —
// set before the engine exists, so its birth is on it too. startErr fails the
// start; the rig is returned started either way.
func newRowRig(t *testing.T, clk *rowClock, startErr error) *rig {
	t.Helper()
	s := newFake(t, agent.EventLogOptions{NoPrimary: true})
	s.clock = clk.now
	s.asks = agent.NewAskRegistry(s.log, clk.now)
	s.startErr = startErr
	e, err := newEngine(s, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	sub, err := e.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	if err := e.Start(context.Background()); !errors.Is(err, startErr) {
		t.Fatalf("start: %v, want %v", err, startErr)
	}
	return &rig{t: t, s: s, e: e, sub: sub}
}

// facts is the row a sessions.list would answer now: State, once everything
// enqueued is committed and observed, and the facts computed from it.
func (r *rig) facts() (State, RowFacts) {
	r.t.Helper()
	r.sync()
	st := r.e.State()
	return st, r.e.RowFacts(st)
}

// wantRow fails unless the row is in state with since.
func (r *rig) wantRow(what string, state RowState, since time.Time) RowFacts {
	r.t.Helper()
	st, f := r.facts()
	if got := RowStateOf(st); got != state {
		r.t.Fatalf("%s: the row is in state %d, want %d (%+v)", what, got, state, st)
	}
	if !f.Since.Equal(since) {
		r.t.Fatalf("%s: since %s, want %s", what, f.Since.Format(time.TimeOnly), since.Format(time.TimeOnly))
	}
	return f
}

func TestRowLineIsOneBoundedLine(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"blank", " \n\t\n ", ""},
		{"the first non-blank line, trimmed", "\n\n  Fixed the test.  \nIt was the clock.", "Fixed the test."},
		{"a carriage return ends a line", "one\r\ntwo", "one"},
		{"tabs expanded", "a\tb", "a    b"},
		{"control characters dropped", "a\x1b[31mb\x7fc\u009bd", "a[31mbcd"},
		{"two hundred cells fit", strings.Repeat("x", 200), strings.Repeat("x", 200)},
		{"one more is cut", strings.Repeat("x", 201), strings.Repeat("x", 199) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RowLine(tc.in); got != tc.want {
				t.Fatalf("RowLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	t.Run("wide characters are cut by the cell", func(t *testing.T) {
		got := RowLine(strings.Repeat("漢", 150))
		if w := uniseg.StringWidth(got); w > RowTextCells || !strings.HasSuffix(got, "…") {
			t.Fatalf("%d cells, %q: want at most %d, cut", w, got, RowTextCells)
		}
		if want := strings.Repeat("漢", 99) + "…"; got != want {
			t.Fatalf("got %d runes, want 99 wide ones and the ellipsis", len([]rune(got)))
		}
	})
}

func TestRowStateOfIsTheListsTable(t *testing.T) {
	failed := &LastTurn{Outcome: TurnFailed, Err: "boom"}
	done := &LastTurn{Outcome: TurnDone}
	for _, tc := range []struct {
		name string
		st   State
		want RowState
	}{
		{"an open ask outranks everything", State{PendingAsks: 1, StartFailed: true, Activity: ActivityWorking}, RowNeedsYou},
		{"a failed start", State{StartFailed: true, Activity: ActivityError}, RowFailed},
		{"a failed last turn", State{Activity: ActivityError, LastTurn: failed}, RowFailed},
		{"a failed last turn restored to the queue (SF-21)", State{Activity: ActivityIdle, LastTurn: failed}, RowFailed},
		{"a failure whose ending is on its way", State{Activity: ActivityError}, RowFailed},
		{"a foreign turn after a failure", State{Activity: ActivityError, ForeignTurn: true}, RowWorking},
		{"a foreign turn's end after a failure", State{Activity: ActivityError, LastTurn: done}, RowIdle},
		{"starting", State{Activity: ActivityStarting}, RowWorking},
		{"replaying", State{Activity: ActivityReplaying}, RowWorking},
		{"working", State{Activity: ActivityWorking}, RowWorking},
		{"closing", State{Activity: ActivityClosing}, RowWorking},
		{"the agent's own turn", State{Activity: ActivityIdle, ForeignTurn: true}, RowWorking},
		{"idle", State{Activity: ActivityIdle, LastTurn: done}, RowIdle},
		{"never prompted", State{Activity: ActivityIdle}, RowIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RowStateOf(tc.st); got != tc.want {
				t.Fatalf("state %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRowFactsSayWhatAWorkingSessionIsDoing: Doing is the running turn's
// newest running tool, else Responding while its text streams, else Thinking
// — and nothing once it is idle, when the reply that ended it is the last
// reply, first line only; a foreign turn is working too.
func TestRowFactsSayWhatAWorkingSessionIsDoing(t *testing.T) {
	clk := newRowClock()
	r := newRowRig(t, clk, nil)
	if _, f := r.facts(); f.Doing != "" || f.LastReply != "" {
		t.Fatalf("a fresh session: %+v", f)
	}
	sc := r.s.script(&script{opened: make(chan struct{}), hold: make(chan struct{}), text: "\nall green"})
	res := r.submit("fix it")
	await(t, sc.opened, "the turn to open")
	r.until(started(res.Turn))
	if _, f := r.facts(); f.Doing != DoingThinking {
		t.Fatalf("a turn with nothing yet: doing %q", f.Doing)
	}
	r.s.emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t-1", Name: "shell", Title: "Run `go test\t./...`", Status: "in_progress"}})
	if _, f := r.facts(); f.Doing != "Run `go test    ./...`" {
		t.Fatalf("a tool running: doing %q", f.Doing)
	}
	r.s.emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t-1", Name: "shell", Status: "completed"}})
	r.s.emit(agent.Event{Type: agent.EventText, Text: "Fixed the flaky test."})
	if _, f := r.facts(); f.Doing != DoingResponding || f.LastReply != "" {
		t.Fatalf("text streaming: %+v", f)
	}
	sc.release()
	r.until(ended(res.Turn))
	if _, f := r.facts(); f.Doing != "" || f.LastReply != "Fixed the flaky test." {
		t.Fatalf("idle after the turn: %+v", f)
	}
	r.foreignTurn("wake-1", true)
	if _, f := r.facts(); f.Doing != DoingThinking || f.LastReply != "Fixed the flaky test." {
		t.Fatalf("the agent's own turn: %+v", f)
	}
	r.foreignTurn("wake-1", false)
	if _, f := r.facts(); f.Doing != "" {
		t.Fatalf("after the agent's own turn: doing %q", f.Doing)
	}
}

// TestRowFactsSummariseTheHeadAsk: the head ask's summary is a permission's
// tool title — the command a shell permission would run — and none with no ask
// open.
func TestRowFactsSummariseTheHeadAsk(t *testing.T) {
	r := newRowRig(t, newRowClock(), nil)
	sc := r.s.script(asking(held(), true))
	res := r.submit("fix it")
	await(t, sc.ask.opened, "the ask to open")
	st, f := r.facts()
	if st.PendingAsks != 1 || f.Summary != "Shell" {
		t.Fatalf("%d open, summary %q, want the permission's tool", st.PendingAsks, f.Summary)
	}
	if err := r.e.Answer(Command{}, st.HeadAsk.ID, agent.AskAnswer{OptionID: "allow-once"}); err != nil {
		t.Fatal(err)
	}
	sc.release()
	r.until(ended(res.Turn))
	if _, f := r.facts(); f.Summary != "" {
		t.Fatalf("no ask open: summary %q", f.Summary)
	}
}

func TestAskSummaryIsWhatTheAskIsAbout(t *testing.T) {
	for _, tc := range []struct {
		name string
		body agent.AskBody
		want string
	}{
		{"a permission's tool title", agent.AskBody{Permission: &agent.PermissionEvent{Tool: "`rm -rf build`"}}, "`rm -rf build`"},
		{"a question's first question", agent.AskBody{Question: &agent.QuestionEvent{Title: "Setup",
			Questions: []agent.Question{{ID: "q1", Prompt: "Which database?\nPick one."}, {ID: "q2", Prompt: "Which port?"}}}}, "Which database?"},
		{"a question's first that asks something", agent.AskBody{Question: &agent.QuestionEvent{Title: "Setup",
			Questions: []agent.Question{{ID: "q1", Prompt: " "}, {ID: "q2", Prompt: "Which port?"}}}}, "Which port?"},
		{"a question with none, by its title", agent.AskBody{Question: &agent.QuestionEvent{Title: "Confirm the plan"}}, "Confirm the plan"},
		{"a plan's name", agent.AskBody{Plan: &agent.PlanEvent{Name: "Migrate the index", Overview: "…"}}, "Migrate the index"},
		{"nothing", agent.AskBody{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RowLine(askSummary(agent.AskRecord{Body: tc.body})); got != tc.want {
				t.Fatalf("summary %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRowFactsOfAFailedStart: the start's error, first line only, and the
// row failed since the start failed.
func TestRowFactsOfAFailedStart(t *testing.T) {
	clk := newRowClock()
	at := clk.step()
	r := newRowRig(t, clk, errors.New("cursor-agent: not logged in\n  run cursor-agent login"))
	st, f := r.facts()
	switch {
	case !st.StartFailed || RowStateOf(st) != RowFailed:
		t.Fatalf("the row is %+v", st)
	case f.StartErr != "cursor-agent: not logged in":
		t.Fatalf("start error %q", f.StartErr)
	case !f.Since.Equal(at):
		t.Fatalf("since %s, want the start's %s", f.Since, at)
	}
}

// TestRowFactsSinceIsWhenTheRowEnteredItsState: each change of row state
// moves Since to its own time — the start, a turn, an ask opening and its
// answer, the turn's end, a failure, the agent's own turn and its end — a
// queued turn that follows another in one settlement keeps the first one's,
// and a change within a state does not move it.
func TestRowFactsSinceIsWhenTheRowEnteredItsState(t *testing.T) {
	clk := newRowClock()
	up := clk.step()
	r := newRowRig(t, clk, nil)
	r.wantRow("up", RowIdle, up)

	// A turn, an ask of its own opened later, answered later still.
	working := clk.step()
	sc := r.s.script(held())
	res := r.submit("fix it")
	await(t, sc.opened, "the turn to open")
	r.wantRow("working", RowWorking, working)
	clk.step()
	r.s.emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t-1", Title: "Read a.go", Status: "in_progress"}})
	r.wantRow("a tool within the turn", RowWorking, working)
	asked := clk.step()
	token := r.s.asks.BeginTurn()
	defer r.s.asks.EndTurn(token)
	if _, err := r.s.asks.Open(context.Background(), token, askRequest()); err != nil {
		t.Fatal(err)
	}
	st := r.e.State()
	r.wantRow("asking", RowNeedsYou, asked)
	answered := clk.step()
	if err := r.e.Answer(Command{}, st.HeadAsk.ID, agent.AskAnswer{OptionID: "allow-once"}); err != nil {
		t.Fatal(err)
	}
	r.wantRow("answered, the turn goes on", RowWorking, answered)
	settled := clk.step()
	sc.release()
	r.until(ended(res.Turn))
	r.wantRow("idle", RowIdle, settled)

	// A failure, and the agent's own turn after it.
	failedAt := clk.step()
	r.s.script(&script{fail: errors.New("the agent fell over")})
	failed := r.submit("again")
	r.until(ended(failed.Turn))
	r.wantRow("failed", RowFailed, failedAt)
	wake := clk.step()
	r.foreignTurn("wake-1", true)
	r.wantRow("the agent's own turn", RowWorking, wake)
	woke := clk.step()
	r.foreignTurn("wake-1", false)
	r.wantRow("after it", RowIdle, woke)

	// Two turns in a row, the second queued behind the first: one working run.
	chained := clk.step()
	first := r.s.script(held())
	one := r.submit("one")
	await(t, first.opened, "the first turn to open")
	second := r.s.script(held())
	r.queue("two")
	clk.step()
	first.release()
	r.until(ended(one.Turn))
	await(t, second.opened, "the queued turn to open")
	evs := r.until(started(""))
	two := evs[len(evs)-1].Turn.ID
	if f := r.wantRow("the queued turn that followed", RowWorking, chained); f.Doing != DoingThinking {
		t.Fatalf("the queued turn: doing %q", f.Doing)
	}
	chainEnd := clk.step()
	second.release()
	r.until(ended(two))
	r.wantRow("after the two", RowIdle, chainEnd)

	// Closing.
	closing := clk.step()
	if err := r.e.Close(); err != nil {
		t.Fatal(err)
	}
	if st := r.e.State(); RowStateOf(st) != RowWorking || !r.e.RowFacts(st).Since.Equal(closing) {
		t.Fatalf("closing: %+v", r.e.RowFacts(st))
	}
}

// TestRowFactsSinceIgnoresAsksThatWereNeverOpen (sol r17-c9 4): an ask that
// never made the row needs you ends without moving Since — a permission
// craze's policy answered (the bypass mode's every permission), a plan the
// policy decided under an Auto opening of its own, an ask refused as it
// opened: a working row stays working since its turn started, and an idle one
// idle since its turn ended. An ask that was open does move it when it ends
// (TestRowFactsSinceIsWhenTheRowEnteredItsState).
func TestRowFactsSinceIgnoresAsksThatWereNeverOpen(t *testing.T) {
	clk := newRowClock()
	r := newRowRig(t, clk, nil)
	working := clk.step()
	sc := r.s.script(held())
	res := r.submit("fix it")
	await(t, sc.opened, "the turn to open")
	r.wantRow("working", RowWorking, working)

	token := r.s.asks.BeginTurn()
	clk.step()
	if rec := r.s.asks.Automatic(token, askRequest(), agent.AskAnswer{OptionID: "allow-once"}); rec.Outcome != agent.AskAutomatic {
		t.Fatalf("the policy's permission ended %s", rec.Outcome)
	}
	r.wantRow("a permission the policy answered", RowWorking, working)
	clk.step()
	plan := agent.AskRequest{Kind: agent.AskPlan, Body: agent.AskBody{Plan: &agent.PlanEvent{Name: "Migrate the index"}}}
	if rec := r.s.asks.Automatic(token, plan, agent.AskAnswer{Accept: true}); rec.Outcome != agent.AskAutomatic {
		t.Fatalf("the policy's plan ended %s", rec.Outcome)
	}
	r.wantRow("a plan the policy accepted", RowWorking, working)
	r.s.asks.EndTurn(token)
	clk.step()
	refused, err := r.s.asks.Open(context.Background(), token, askRequest())
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := r.s.asks.Record(refused.ID()); !ok || rec.Outcome != agent.AskTurnEnded {
		t.Fatalf("an ask opened after its turn ended: %+v", rec)
	}
	r.wantRow("an ask refused as it opened", RowWorking, working)

	settled := clk.step()
	sc.release()
	r.until(ended(res.Turn))
	r.wantRow("idle", RowIdle, settled)
	clk.step()
	r.s.asks.Automatic(agent.TurnToken{}, askRequest(), agent.AskAnswer{OptionID: "allow-once"})
	r.wantRow("a permission the policy answered between turns", RowIdle, settled)
}

// TestRowFactsSinceOfALoad: a load's replay is part of the session coming up
// — working since the engine was built, however long the replay runs — and
// the row is idle from the replay's end.
func TestRowFactsSinceOfALoad(t *testing.T) {
	clk := newRowClock()
	born := clk.now()
	r := newRowRig(t, clk, nil)
	clk.step()
	r.s.emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}})
	r.wantRow("replaying", RowWorking, born)
	clk.step()
	r.s.emit(agent.Event{Type: agent.EventText, Text: "an old reply", Replayed: true})
	r.wantRow("replaying on", RowWorking, born)
	replayed := clk.step()
	r.s.emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayEnd}})
	f := r.wantRow("replayed", RowIdle, replayed)
	if f.LastReply != "an old reply" {
		t.Fatalf("last reply %q, want the replayed one", f.LastReply)
	}
}
