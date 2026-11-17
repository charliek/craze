package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// Background work and children end to end (plan 052 C4), over the fake
// adapter's claude_bg.go scripts: the session's foreign-turn brackets, its
// claude children's rows, transcripts, asks and stop.

// shortForeign shortens the foreign-turn limits for the sessions a test
// starts: a 100 ms grace, and silence limits of silence and four times it.
func shortForeign(t *testing.T, silence time.Duration) {
	t.Helper()
	restore := SetClaudeForeignTimingForTest(acp.ForeignTiming{Grace: 100 * time.Millisecond, Silence: silence, SilenceToolOpen: 4 * silence})
	t.Cleanup(restore)
}

// foreignBrackets is the session's foreign-turn events, in order.
func foreignBrackets(evs []Event) []ForeignTurnInfo {
	var out []ForeignTurnInfo
	for _, ev := range evs {
		if ev.Type == EventForeignTurn && ev.ForeignTurn != nil {
			out = append(out, *ev.ForeignTurn)
		}
	}
	return out
}

// waitBrackets waits for n foreign-turn events.
func (e *eventLog) waitBrackets(t *testing.T, n int) []ForeignTurnInfo {
	t.Helper()
	waitFor(t, "the foreign-turn brackets", func() bool { return len(foreignBrackets(e.snapshot())) >= n })
	return foreignBrackets(e.snapshot())
}

func isForeign(running bool) func(Event) bool {
	return func(ev Event) bool {
		return ev.Type == EventForeignTurn && ev.ForeignTurn != nil && ev.ForeignTurn.Running == running
	}
}

func isText(text string) func(Event) bool {
	return func(ev Event) bool { return ev.Type == EventText && ev.Agent == "" && ev.Text == text }
}

func startClaude(t *testing.T, script string, opts Options, env ...string) (*claudeRig, *eventLog) {
	t.Helper()
	r := newClaudeRig(t, script, opts, env...)
	log := collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return r, log
}

// TestClaudeForeignSessionWake (AC17): a background command's wake after the
// prompt has ended is one background foreign turn, bracketing its content,
// and the session is idle again after the grace.
func TestClaudeForeignSessionWake(t *testing.T) {
	shortForeign(t, 5*time.Second)
	r, log := startClaude(t, "claude-wake", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "BG run it"); err != nil {
		t.Fatal(err)
	}
	b := log.waitBrackets(t, 2)
	if len(b) != 2 || !b[0].Running || b[0].Reason != ReasonBackground || b[1].Running || b[1].ID != b[0].ID || b[1].Err != nil || b[1].Cancelled {
		t.Fatalf("brackets %+v; want one background turn, done", b)
	}
	evs := log.snapshot()
	if open, text, end := firstIndex(evs, isForeign(true)), firstIndex(evs, isText("bg-done")), firstIndex(evs, isForeign(false)); open >= text || text >= end {
		t.Fatalf("start %d, wake text %d, end %d; want the text inside the brackets", open, text, end)
	}
	if r.s.ForeignTurn() {
		t.Fatal("the session still says foreign after the ending")
	}
}

// TestClaudeForeignSessionWakeEarly (AC17): what the adapter sends before
// session/new returns opens nothing; root content after setup does.
func TestClaudeForeignSessionWakeEarly(t *testing.T) {
	shortForeign(t, 5*time.Second)
	_, log := startClaude(t, "claude-wake-early", Options{Interactive: true})
	b := log.waitBrackets(t, 2)
	evs := log.snapshot()
	if before, open := firstIndex(evs, isText("before the reply")), firstIndex(evs, isForeign(true)); before < 0 || open < before || len(b) != 2 {
		t.Fatalf("the early text at %d, the start at %d, %d brackets; want the early text outside", before, open, len(b))
	}
}

// TestClaudeForeignSessionPlaceholder (AC17, R2-3): a placeholder's usage
// followed by content within the grace is one turn; after the grace, two.
func TestClaudeForeignSessionPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		script, gap string
		turns       int
	}{
		{"claude-wake-placeholder", "20ms", 1},
		{"claude-wake-placeholder-late", "500ms", 2},
	} {
		t.Run(tc.script, func(t *testing.T) {
			shortForeign(t, 5*time.Second)
			r, log := startClaude(t, tc.script, Options{Interactive: true}, "CRAZE_FAKE_CLAUDE_GAP="+tc.gap)
			if _, err := r.s.Prompt(t.Context(), "BG go"); err != nil {
				t.Fatal(err)
			}
			log.waitText(t, "the follow-up")
			log.waitBrackets(t, 2*tc.turns)
			time.Sleep(300 * time.Millisecond)
			if b := foreignBrackets(log.snapshot()); len(b) != 2*tc.turns {
				t.Fatalf("%d brackets %+v; want %d turns", len(b), b, tc.turns)
			}
		})
	}
}

// TestClaudeForeignSessionOpenTool (AC17): the candidate waits for the open
// tool call's close, even past the grace.
func TestClaudeForeignSessionOpenTool(t *testing.T) {
	shortForeign(t, 5*time.Second)
	r, log := startClaude(t, "claude-wake-opentool", Options{Interactive: true}, "CRAZE_FAKE_CLAUDE_GAP=400ms")
	if _, err := r.s.Prompt(t.Context(), "BG go"); err != nil {
		t.Fatal(err)
	}
	b := log.waitBrackets(t, 2)
	evs := log.snapshot()
	closed := firstIndex(evs, func(ev Event) bool {
		return ev.Type == EventTool && ev.Tool != nil && ev.Tool.ID == "toolu_wake" && ev.Tool.Status == "completed"
	})
	if end := firstIndex(evs, isForeign(false)); closed < 0 || end < closed || len(b) != 2 {
		t.Fatalf("the close at %d, the end at %d; want the turn to outlast its tool call", closed, end)
	}
}

// TestClaudeForeignSessionNoUsage (AC17): a wake that sends no usage ends by
// silence, with a note.
func TestClaudeForeignSessionNoUsage(t *testing.T) {
	shortForeign(t, 300*time.Millisecond)
	r, log := startClaude(t, "claude-wake-nousage", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "BG go"); err != nil {
		t.Fatal(err)
	}
	b := log.waitBrackets(t, 2)
	if b[1].Running || b[1].Cancelled || b[1].Err != nil {
		t.Fatalf("the ending %+v", b[1])
	}
	if !strings.Contains(r.diag.String(), "went quiet") {
		t.Fatalf("no silence note: %q", r.diag.String())
	}
}

// TestClaudeForeignSessionOpensNothing (AC17): a trailing tool_call_update,
// a load's replay, idle notices and the rest of the idle traffic open nothing.
func TestClaudeForeignSessionOpensNothing(t *testing.T) {
	for _, tc := range []struct {
		script, prompt string
		opts           Options
	}{
		{"claude-wake-trailing", "BG go", Options{Interactive: true}},
		{"claude-replay-load", "", Options{Interactive: true, LoadSessionID: "s-loaded"}},
		{"claude-notice-idle", "", Options{Interactive: true}},
	} {
		t.Run(tc.script, func(t *testing.T) {
			shortForeign(t, 300*time.Millisecond)
			r, log := startClaude(t, tc.script, tc.opts)
			if tc.prompt != "" {
				if _, err := r.s.Prompt(t.Context(), tc.prompt); err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(400 * time.Millisecond)
			if b := foreignBrackets(log.snapshot()); len(b) != 0 {
				t.Fatalf("opened %+v", b)
			}
		})
	}
}

// TestClaudeForeignSessionCompaction (AC17): idle compaction is a
// compaction foreign turn.
func TestClaudeForeignSessionCompaction(t *testing.T) {
	shortForeign(t, 5*time.Second)
	_, log := startClaude(t, "claude-compaction-idle", Options{Interactive: true})
	b := log.waitBrackets(t, 2)
	if b[0].Reason != ReasonCompaction || b[1].Reason != ReasonCompaction {
		t.Fatalf("brackets %+v; want a compaction turn", b)
	}
}

// TestClaudeForeignSessionCancelAndRefusal (AC17): a prompt during a foreign
// turn is refused before the wire (the engine queues it), and Esc cancels the
// turn: session/cancel goes out and the turn ends cancelled.
func TestClaudeForeignSessionCancelAndRefusal(t *testing.T) {
	shortForeign(t, 10*time.Second)
	r, log := startClaude(t, "claude-wake-nousage", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "BG go"); err != nil {
		t.Fatal(err)
	}
	log.waitBrackets(t, 1)
	waitFor(t, "the session to say foreign", r.s.ForeignTurn)
	if _, err := r.s.Prompt(t.Context(), "next"); !errors.Is(err, ErrForeignTurn) {
		t.Fatalf("a prompt during the foreign turn = %v; want ErrForeignTurn", err)
	}
	if _, err := r.s.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	b := log.waitBrackets(t, 2)
	if !b[1].Cancelled {
		t.Fatalf("the ending %+v; want cancelled", b[1])
	}
	if !strings.Contains(r.read(t, "calls"), acp.MethodSessionCancel) {
		t.Fatal("no session/cancel on the wire")
	}
	if _, err := r.s.Prompt(t.Context(), "after"); err != nil {
		t.Fatalf("a prompt after the cancelled turn: %v", err)
	}
}

// TestClaudeForeignSessionStartedNewTurn (AC17, carried from C3): a steer the
// adapter answers startedNewTurn — its turn having ended just ahead of it — is
// a foreign turn, during which a prompt is refused.
func TestClaudeForeignSessionStartedNewTurn(t *testing.T) {
	shortForeign(t, 5*time.Second)
	r, log := startClaude(t, "claude-basic", Options{Interactive: true})
	done := make(chan error, 1)
	go func() { _, err := r.s.Prompt(context.Background(), "STEER-RACE"); done <- err }()
	waitFor(t, "the turn", func() bool { return r.s.promptInFlight() })
	if err := r.s.Interject(t.Context(), "keep going"); err != nil {
		t.Fatalf("the steer: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b := log.waitBrackets(t, 2)
	if b[0].Reason != ReasonBackground {
		t.Fatalf("brackets %+v", b)
	}
	evs := log.snapshot()
	if open, text := firstIndex(evs, isForeign(true)), firstIndex(evs, isText("steered: keep going")); open < 0 || text < open {
		t.Fatalf("start %d, steered text %d", open, text)
	}
}

// TestClaudeForeignSessionFailedByTheStop (E2): a foreign turn running when
// the session stops for a reason of its own ends failed, with the stop's kind.
func TestClaudeForeignSessionFailedByTheStop(t *testing.T) {
	shortForeign(t, 10*time.Second)
	r, log := startClaude(t, "claude-wake-nousage", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "BG go"); err != nil {
		t.Fatal(err)
	}
	log.waitBrackets(t, 1)
	r.s.stopFor(errNoLoginStatus())
	b := log.waitBrackets(t, 2)
	if b[1].Err == nil || b[1].Err.Kind != ErrorInfo(errNoLoginStatus()).Kind || b[1].Err.Message == "" {
		t.Fatalf("the ending %+v; want failed with the stop's kind", b[1])
	}
}

func childEvents(evs []Event, id string) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.Agent == id || (ev.Type == EventSubagent && ev.Subagent != nil && ev.Subagent.ID == id) {
			out = append(out, ev)
		}
	}
	return out
}

// waitRow waits for the child's latest row to satisfy ok, and answers it.
func (e *eventLog) waitRow(t *testing.T, id string, ok func(SubagentInfo) bool) SubagentInfo {
	t.Helper()
	var row *SubagentInfo
	waitFor(t, "the row of "+id, func() bool {
		row = lastRow(e.snapshot(), id)
		return row != nil && ok(*row)
	})
	return *row
}

func rowDone(r SubagentInfo) bool { return r.Status != SubagentRunning }

func lastRow(evs []Event, id string) *SubagentInfo {
	for i := len(evs) - 1; i >= 0; i-- {
		if ev := evs[i]; ev.Type == EventSubagent && ev.Subagent != nil && ev.Subagent.ID == id {
			return ev.Subagent
		}
	}
	return nil
}

// TestClaudeChildSessionRow (AC18): one row per child, from subagent_update,
// labelled with its title and role; its opening user block from
// session_message; its stream as its own; completed on idle end_turn; and the
// launching call makes no second row.
func TestClaudeChildSessionRow(t *testing.T) {
	r, log := startClaude(t, "claude-subagent", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "AGENT"); err != nil {
		t.Fatal(err)
	}
	row := log.waitRow(t, "child-1", rowDone)
	log.waitText(t, "the child said hello")
	evs := log.snapshot()
	if row.Status != SubagentCompleted || row.Description != "Read note.txt contents" || row.SubagentType != "general-purpose" ||
		row.ParentID != "" || row.ToolCallID != "toolu_launch_1" || row.Prompt != "read note.txt" || row.NoCancel {
		t.Fatalf("the child's row %+v", row)
	}
	kinds := []EventType{}
	for _, ev := range childEvents(evs, "child-1") {
		kinds = append(kinds, ev.Type)
	}
	want := []EventType{EventSubagent, EventSubagent, EventUser, EventTool, EventTool, EventText, EventSubagent}
	if !slices.Equal(kinds, want) {
		t.Fatalf("the child's events %v; want %v", kinds, want)
	}
	if snap := r.s.Snapshot(); len(snap.Subagents) != 1 {
		t.Fatalf("%d rows; want the one child's", len(snap.Subagents))
	}
	if b := foreignBrackets(evs); len(b) != 0 {
		t.Fatalf("the child opened a foreign turn: %+v", b)
	}
}

// TestClaudeChildSessionNested (AC19): root → child → grandchild registers,
// routes and nests by ParentID.
func TestClaudeChildSessionNested(t *testing.T) {
	r, log := startClaude(t, "claude-subagent-nested", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "AGENT"); err != nil {
		t.Fatal(err)
	}
	log.waitRow(t, "child-1", rowDone)
	log.waitRow(t, "grandchild-1", rowDone)
	evs := log.snapshot()
	c, g := lastRow(evs, "child-1"), lastRow(evs, "grandchild-1")
	if c.ParentID != "" || g.ParentID != "child-1" || c.Status != SubagentCompleted || g.Status != SubagentCompleted {
		t.Fatalf("rows %+v %+v", c, g)
	}
	if firstIndex(evs, func(ev Event) bool { return ev.Agent == "grandchild-1" && ev.Type == EventText && ev.Text == "deep" }) < 0 {
		t.Fatal("the grandchild's own text did not route to it")
	}
}

// TestClaudeChildSessionEarlyUpdate (AC18): a child's update before its
// announce is replayed after it, not dropped.
func TestClaudeChildSessionEarlyUpdate(t *testing.T) {
	r, log := startClaude(t, "claude-subagent-early-child", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "AGENT"); err != nil {
		t.Fatal(err)
	}
	log.waitRow(t, "child-1", rowDone)
	evs := log.snapshot()
	spawned := firstIndex(evs, func(ev Event) bool { return ev.Type == EventSubagent && ev.SubagentChange == SubagentChangeSpawned })
	early := firstIndex(evs, func(ev Event) bool { return ev.Agent == "child-1" && ev.Text == "before my announce" })
	if spawned < 0 || early < spawned {
		t.Fatalf("spawned %d, the early text %d; want it replayed after the announce", spawned, early)
	}
}

// TestClaudeChildSessionReuse (AC18): an idle child reused is a new attempt on
// the same row; idle with no stop reason completes.
func TestClaudeChildSessionReuse(t *testing.T) {
	r, log := startClaude(t, "claude-subagent-reuse", Options{Interactive: true})
	for range 2 {
		if _, err := r.s.Prompt(t.Context(), "AGENT"); err != nil {
			t.Fatal(err)
		}
	}
	row := log.waitRow(t, "child-1", func(r SubagentInfo) bool { return r.AttemptID == "2" && rowDone(r) })
	if row.AttemptID != "2" || row.Status != SubagentCompleted || row.Prompt != "second job" {
		t.Fatalf("the reused row %+v", row)
	}
	if snap := r.s.Snapshot(); len(snap.Subagents) != 1 {
		t.Fatalf("%d rows; want one", len(snap.Subagents))
	}
}

// TestClaudeChildSessionUnknown (AC18): unknown ends that child's row
// cancelled, no longer observable; the other row is untouched.
func TestClaudeChildSessionUnknown(t *testing.T) {
	r, log := startClaude(t, "claude-subagent-unknown", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "AGENT"); err != nil {
		t.Fatal(err)
	}
	lost, kept := log.waitRow(t, "child-1", rowDone), log.waitRow(t, "child-2", rowDone)
	if lost.Status != SubagentCancelled || lost.Activity != activityUnobservable || kept.Status != SubagentCompleted {
		t.Fatalf("rows %+v %+v", lost, kept)
	}
}

// TestClaudeChildSessionAskOutlivesTheRootTurn (AC18): a child's card is
// labelled with the child, survives the root turn's end, and its answer
// reaches the child.
func TestClaudeChildSessionAskOutlivesTheRootTurn(t *testing.T) {
	r, log := startClaude(t, "claude-child-ask", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "AGENT"); err != nil {
		t.Fatal(err)
	}
	rec := waitOpenAsk(t, r.s, AskPermission)
	if !strings.Contains(rec.Label(), "sub-agent Asker") {
		t.Fatalf("the card reads %q; want it labelled with the child", rec.Label())
	}
	row := log.waitRow(t, "child-1", func(r SubagentInfo) bool { return r.Activity == activityWaiting })
	if row.Status != SubagentRunning {
		t.Fatalf("the waiting child's row %+v", row)
	}
	if _, err := r.s.Asks().Answer("", rec.ID, AskAnswer{OptionID: "allow-once"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the child's answer", func() bool {
		return firstIndex(log.snapshot(), func(ev Event) bool { return ev.Agent == "child-1" && ev.Text == "permission: allow-once" }) >= 0
	})
	waitFor(t, "the child's ending", func() bool {
		row := lastRow(log.snapshot(), "child-1")
		return row != nil && row.Status == SubagentCompleted && row.Activity == ""
	})
}

// TestClaudeChildSessionCancel (AC18): stopping a row stops only that child —
// its own session/cancel — and the row ends cancelled, stopped by the user,
// while the root turn goes on to its own end.
func TestClaudeChildSessionCancel(t *testing.T) {
	r, log := startClaude(t, "claude-subagent", Options{Interactive: true})
	done := make(chan error, 1)
	go func() { _, err := r.s.Prompt(context.Background(), "AGENT-HOLD"); done <- err }()
	waitFor(t, "the child", func() bool { return lastRow(log.snapshot(), "child-1") != nil })
	if err := r.s.CancelSubagent("nobody"); !errors.Is(err, ErrNoSuchSubagent) {
		t.Fatalf("stopping an unknown child = %v", err)
	}
	if err := r.s.CancelSubagent("child-1"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	row := log.waitRow(t, "child-1", rowDone)
	if row.Status != SubagentCancelled || row.Error != stoppedByUser {
		t.Fatalf("the stopped row %+v", row)
	}
	// The fake stops a child only for a session/cancel naming it, and ends the
	// root's turn with this text only then: the root was not cancelled.
	log.waitText(t, "the child was stopped")
}

// TestClaudeChildSessionHeldTurn (AC19): the adapter holds the prompt's reply
// until its background child and the child's hand-back are done, so the
// prompted turn spans them and nothing is foreign.
func TestClaudeChildSessionHeldTurn(t *testing.T) {
	shortForeign(t, 5*time.Second)
	r, log := startClaude(t, "claude-held-bg-subagent", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	// The reply is the turn's last frame: everything before it is in the
	// stream once the session has published it.
	log.waitText(t, "the child reported: background result")
	evs := log.snapshot()
	row := lastRow(evs, "child-1")
	if row == nil || row.Status != SubagentCompleted {
		t.Fatalf("the prompt returned before its background child: row %+v", row)
	}
	time.Sleep(300 * time.Millisecond)
	if b := foreignBrackets(log.snapshot()); len(b) != 0 {
		t.Fatalf("the held turn opened %+v", b)
	}
}
