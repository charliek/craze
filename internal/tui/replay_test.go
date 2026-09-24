package tui

import (
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/sessions"
)

// ---------------------------------------------------------------- fixtures

// fakeIndex is Config.SessionIndex as a recorder. It is the whole of what the
// TUI knows about the index — one method — which is why a unit test can hold
// every write moment in §3.2 without a file, a lock or a HOME.
//
// It is concurrency-safe and it SIGNALS, because the index moved into the
// engine (plan 021 C12) and the event-driven writes — the agent's title, the
// touch at a turn's end, a loaded row coming up — run on the engine's index
// worker rather than on the model's own goroutine. A test that wants one waits
// on waitRows or waitAttempts; nothing here ever sleeps.
type fakeIndex struct {
	mu   sync.Mutex
	rows []sessions.Row
	err  error
	// attempts counts every call, a failing one included, which is what a test
	// of the retry rule has to be able to see.
	attempts int
	// wrote carries one token per completed call. It is buffered well past
	// anything a test writes so that Upsert never blocks on it.
	wrote chan struct{}
}

func (f *fakeIndex) Upsert(row sessions.Row) error {
	f.mu.Lock()
	f.attempts++
	err := f.err
	if err == nil {
		f.rows = append(f.rows, row)
	}
	if f.wrote == nil {
		f.wrote = make(chan struct{}, 64)
	}
	signal := f.wrote
	f.mu.Unlock()
	select {
	case signal <- struct{}{}:
	default:
	}
	return err
}

func (f *fakeIndex) last() sessions.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rows) == 0 {
		return sessions.Row{}
	}
	return f.rows[len(f.rows)-1]
}

// seedRow is the first-prompt row: the one write that carries a fallback
// title. A test about what a title SAYS wants this one and not last(), because
// the turn's own end touches the row afterwards with no title at all — and
// since the index moved into the engine that touch is the index worker's, so it
// can land at any point after the event is committed (plan 021 §3.8).
func (f *fakeIndex) seedRow(t *testing.T) sessions.Row {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.TitleKind == sessions.TitleKindFallback {
			return r
		}
	}
	t.Fatalf("no first-prompt row was written: %+v", f.rows)
	return sessions.Row{}
}

// seeds is how many first-prompt rows were written, which is the "later sends
// do not rewrite the file for a title no rule would keep" guarantee as
// something a test can count without racing a turn's end.
func (f *fakeIndex) seeds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.rows {
		if r.TitleKind == sessions.TitleKindFallback {
			n++
		}
	}
	return n
}

func (f *fakeIndex) all() []sessions.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sessions.Row(nil), f.rows...)
}

func (f *fakeIndex) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func (f *fakeIndex) tries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *fakeIndex) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// signalChan is the channel Upsert posts to, created on first use by whichever
// of a waiter and a writer gets there first.
func (f *fakeIndex) signalChan() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wrote == nil {
		f.wrote = make(chan struct{}, 64)
	}
	return f.wrote
}

// waitRows blocks until the index holds at least n rows. It is a barrier on
// the writer's own signal, never a sleep; the deadline is the suite's
// deadlock watchdog.
func waitRows(t *testing.T, f *fakeIndex, n int) {
	t.Helper()
	waitIndex(t, f, n, f.count, "rows")
}

// waitAttempts is waitRows for calls rather than rows: what a test of a
// FAILING index has to wait on, since a failed write records nothing.
func waitAttempts(t *testing.T, f *fakeIndex, n int) {
	t.Helper()
	waitIndex(t, f, n, f.tries, "attempts")
}

func waitIndex(t *testing.T, f *fakeIndex, n int, have func() int, what string) {
	t.Helper()
	signal := f.signalChan()
	timeout := deadline()
	for have() < n {
		select {
		case <-signal:
		case <-timeout:
			t.Fatalf("the index reached %d %s, want %d: %+v", have(), what, n, f.all())
		}
	}
}

// replayTranscript is what a loaded session hands back: the user's earlier
// prompt (already coalesced out of its two wire chunks by the session, §3.4), a
// thought, a tool row and the answer.
func replayTranscript() []agent.Event {
	return []agent.Event{
		{Type: agent.EventUser, Text: "List the files in the current working directory in one line."},
		{Type: agent.EventThought, Text: "Recalling the earlier turn."},
		{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "replay-0-1", Title: "List Directory", Kind: "read", Status: "completed"}},
		{Type: agent.EventText, Text: "the workspace holds main.py and README.md"},
	}
}

// loadedModel is a model built the way runTUI builds one for --continue: the
// session is a load, so Config.Loading is set and the model is replaying before
// it has seen a single event. Nothing is started and no event is delivered —
// every test below decides the order those arrive in.
func loadedStub(t *testing.T, idx SessionIndex) (Model, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })
	m := New(Config{
		Session:      stub,
		Theme:        "tokyo-night",
		Workspace:    t.TempDir(),
		Model:        "grok",
		Yolo:         true,
		Loading:      true,
		SessionIndex: idx,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return tm.(Model), stub
}

func replayEvent(phase string) agent.Event {
	return agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: phase}}
}

func replayed(ev agent.Event) agent.Event {
	ev.Replayed = true
	return ev
}

// typeAndEnter drives a whole send through the composer, so the refusal under
// test is the one a user would meet rather than a direct call to sendText.
func typeAndEnter(t *testing.T, m Model, text string) (Model, tea.Cmd) {
	t.Helper()
	m.input.SetValue(text)
	tm, cmd := m.Update(enter())
	return tm.(Model), cmd
}

// ------------------------------------------------------- the deadlock fix

// TestInitStartsAndReadsEventsTogether is §3.5's deadlock fix: the event reader
// is armed in the same batch as Start, not by startedMsg, because a session/load
// replays its transcript from the client's read loop *while* Start is running.
// A reader armed afterwards can never drain a replay longer than the session's
// event channel, so the load result is never read and Start never returns.
func TestInitStartsAndReadsEventsTogether(t *testing.T) {
	m, stub := loadedStub(t, nil)
	msg := m.Init()()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init must return a batch of startCmd and waitEvent, got %T", msg)
	}
	if len(batch) != 2 {
		t.Fatalf("Init batch has %d members, want 2", len(batch))
	}
	// One member reads the channel and one starts the session. Which is which
	// is not ordered — that is the whole point — so the batch is identified by
	// what its members produce.
	stub.Emit(agent.Event{Type: agent.EventText, Text: "from the replay"})
	var sawEvent, sawStarted bool
	for _, cmd := range batch {
		switch msg := cmd().(type) {
		case eventMsg:
			sawEvent = msg.ev.Text == "from the replay"
		case startedMsg:
			sawStarted = true
		default:
			t.Fatalf("Init batch produced %T", msg)
		}
	}
	if !sawEvent || !sawStarted {
		t.Fatalf("Init batch: event reader %v, start %v", sawEvent, sawStarted)
	}
}

// TestStartedMsgDoesNotReArmTheReader: Init owns the first waitEvent and
// eventMsg owns every one after it. A second reader armed by startedMsg would
// take one event per Update out of turn and race the first.
func TestStartedMsgDoesNotReArmTheReader(t *testing.T) {
	m, _ := loadedStub(t, nil)
	// The command is never run: a second waitEvent would block on a channel
	// nothing is going to write to, and the failure is that it exists at all.
	if _, cmd := m.Update(startedMsg{}); cmd != nil {
		t.Fatal("startedMsg re-armed the event reader Init already owns")
	}
}

// ------------------------------------------------------- the two-key gate

func TestLoadedModelStartsRestoring(t *testing.T) {
	m, _ := loadedStub(t, nil)
	if !m.replaying || m.sessionReady() {
		t.Fatalf("a loaded model must be replaying before any event: replaying=%v ready=%v", m.replaying, m.sessionReady())
	}
	if got := statusText(m.statusRow1()); !strings.Contains(got, "restoring…") {
		t.Fatalf("status row before any event is %q, want restoring", got)
	}
}

func TestNewSessionIsNeverRestoring(t *testing.T) {
	isolateSkillsHome(t)
	m := startSized(t, t.TempDir())
	if m.replaying || !m.sessionReady() {
		t.Fatalf("a new session must come up on startedMsg alone: %+v", m)
	}
}

// TestSessionComesUpInEitherOrder is the ordering §3.5 exists for. tea.Batch
// promises nothing about which of startedMsg and EventReplay{end} lands first,
// and CodeRabbit's finding is that startedMsg normally lands *mid*-replay: Start
// returns the instant the RPC result is read, while the TUI has applied at most
// one queued event. Both orders have to leave the session refusing sends and the
// status row restoring until the second key lands.
func TestSessionComesUpInEitherOrder(t *testing.T) {
	for _, tc := range []struct{ name string }{{"started first"}, {"replay end first"}} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := loadedStub(t, nil)
			m = feed(t, m, replayEvent(agent.ReplayStart), replayed(replayTranscript()[0]))

			first, second := tea.Msg(startedMsg{}), tea.Msg(eventMsg{replayEvent(agent.ReplayEnd)})
			if tc.name == "replay end first" {
				first, second = second, first
			}

			m = deliver(t, m, first)
			// The other key has not landed and there are still replay events
			// queued behind it, which is exactly the state a one-key gate gets
			// wrong.
			if m.sessionReady() {
				t.Fatal("the session is up after one key")
			}
			// restoring… while the replay is the key still missing,
			// starting… once it has landed and Start has not returned: both
			// say the same thing, that the session is not up.
			want := "starting…"
			if m.replaying {
				want = "restoring…"
			}
			if got := statusText(m.statusRow1()); !strings.Contains(got, want) {
				t.Fatalf("status row after one key is %q, want %q", got, want)
			}
			if m.status == statusWorking {
				t.Fatal("nothing is working during a replay")
			}
			// The tail is the observable half of the gate: the elapsed
			// counter starts there, and nothing may start it on one key.
			if !m.sessStart.IsZero() {
				t.Fatal("the session-is-up tail ran on one key")
			}
			before := len(texts(m, entryUser))
			next, cmd := typeAndEnter(t, m, "a prompt that must not go out")
			if cmd != nil || len(texts(next, entryUser)) != before {
				t.Fatal("a send was accepted while the session was still restoring")
			}
			// The still-queued replay keeps arriving after the first key.
			m = feed(t, m, replayed(replayTranscript()[3]))

			m = deliver(t, m, second)
			if !m.sessionReady() || m.status != statusIdle {
				t.Fatalf("the session is not up after both keys: ready=%v status=%v", m.sessionReady(), m.status)
			}
			if m.sessStart.IsZero() {
				t.Fatal("the elapsed counter never started")
			}
			if got := statusText(m.statusRow1()); strings.Contains(got, "restoring") || strings.Contains(got, "starting") {
				t.Fatalf("status row once up is %q", got)
			}
			m, cmd = typeAndEnter(t, m, "now it may go out")
			if cmd == nil {
				t.Fatal("a send was refused after both keys landed")
			}
			if got := texts(m, entryUser); len(got) == 0 || got[len(got)-1] != "now it may go out" {
				t.Fatalf("user entries %q", got)
			}
		})
	}
}

// TestReplayLeavesTheTurnAlone: a replay is history, not a turn. Nothing in it
// may start one — a turn start retires the last turn's plan offer and its
// evidence, and a replay retiring them would be the restored transcript
// answering for a session it is not part of. What says no turn started is that
// no prompt reached the session and nothing on screen says one is running.
func TestReplayLeavesTheTurnAlone(t *testing.T) {
	m, stub := loadedStub(t, nil)
	m = feed(t, m, replayEvent(agent.ReplayStart))
	for _, ev := range replayTranscript() {
		m = feed(t, m, replayed(ev))
	}
	m = feed(t, m, replayEvent(agent.ReplayEnd))
	if n := turnsStarted(stub); n != 0 {
		t.Fatalf("the replay started %d turns", n)
	}
	if m.status == statusWorking || m.spinnerVisible() || m.tickFast {
		t.Fatalf("a replay started a turn: status=%v spinner=%v", m.status, m.spinnerVisible())
	}
}

// ---------------------------------------------------------- what it draws

func TestReplayRendersTheRestoredTranscript(t *testing.T) {
	m, _ := loadedStub(t, nil)
	m = feed(t, m, replayEvent(agent.ReplayStart))
	for _, ev := range replayTranscript() {
		m = feed(t, m, replayed(ev))
	}
	if got := texts(m, entryUser); len(got) != 1 || got[0] != replayTranscript()[0].Text {
		t.Fatalf("user entries %q, want the one replayed prompt", got)
	}
	if got := texts(m, entryThought); len(got) != 1 {
		t.Fatalf("thought entries %q", got)
	}
	if got := toolRows(m); len(got) != 1 || !strings.Contains(got[0], "List Directory") {
		t.Fatalf("tool rows %q", got)
	}
	// The last replayed thought must not be left open under the note, so the
	// bracket's end breaks the stream before it writes.
	m = feed(t, m, replayEvent(agent.ReplayEnd))
	if got := texts(m, entryNote); len(got) != 1 || got[0] != restoredNote {
		t.Fatalf("notes %q, want one %q", got, restoredNote)
	}
	if m.main.streamOpen() {
		t.Fatal("the replay left a stream open under the restored note")
	}
	for _, e := range m.main.entries() {
		if e.open {
			t.Fatalf("entry %q is still open after the replay ended", e.text)
		}
	}
}

// TestOneUserBlockPerReplayedPrompt: the agents replay a prompt as several
// user_message_chunks and the session coalesces them into one EventUser (§3.4),
// so the seam the TUI owns is one block per event — not one per chunk, and not
// none at all.
func TestOneUserBlockPerReplayedPrompt(t *testing.T) {
	m, _ := loadedStub(t, nil)
	m = feed(t, m,
		replayEvent(agent.ReplayStart),
		replayed(agent.Event{Type: agent.EventUser, Text: "List the files in the current working directory in one line."}),
		replayEvent(agent.ReplayEnd),
	)
	if got := texts(m, entryUser); len(got) != 1 || got[0] != "List the files in the current working directory in one line." {
		t.Fatalf("user entries %q, want one joined block", got)
	}
}

// TestLiveUserEchoIsStillDropped is the regression the replay case must not
// cause: grok and gx echo every prompt back as a main-session user chunk, and
// craze has already written that block from its own send.
func TestLiveUserEchoIsStillDropped(t *testing.T) {
	m := sized(t)
	before := len(texts(m, entryUser))
	m = feed(t, m, agent.Event{Type: agent.EventUser, Text: "the agent's echo of my prompt"})
	if got := texts(m, entryUser); len(got) != before {
		t.Fatalf("a live user echo reached the transcript: %q", got)
	}
	// An interjection is still the one user block craze does not write itself.
	m = feed(t, m, agent.Event{Type: agent.EventUser, Text: "merged into the turn", Interjection: true})
	if got := texts(m, entryUser); len(got) != before+1 {
		t.Fatalf("interjection entries %q", got)
	}
}

// ------------------------------------------------------------ index writes

func TestNoIndexRowOnStartAlone(t *testing.T) {
	idx := &fakeIndex{}
	m, _ := loadedStub(t, idx)
	// A *new* session: nothing loaded, so nothing to touch when it comes up.
	m.replaying = false
	m = deliver(t, m, startedMsg{})
	if !m.sessionReady() {
		t.Fatal("the session never came up")
	}
	if n := idx.count(); n != 0 {
		t.Fatalf("a session with no prompt wrote %d rows: %+v", n, idx.all())
	}
}

func TestLoadedSessionTouchesItsRowWhenItComesUp(t *testing.T) {
	idx := &fakeIndex{}
	m, stub := loadedStub(t, idx)
	m = deliver(t, m, startedMsg{})
	if n := idx.count(); n != 0 {
		t.Fatalf("the row was touched before the replay ended: %+v", idx.all())
	}
	// Emitted through the session's log rather than handed to Update, because
	// the touch is the engine's now: its observer runs where an event is
	// committed, and an event the model was simply given never went past the
	// engine at all (plan 021 §3.8).
	stub.Emit(replayEvent(agent.ReplayEnd))
	m = pumpUntil(t, m, func(m Model) bool { return m.sessionReady() })
	waitRows(t, idx, 1)
	if n := idx.count(); n != 1 {
		t.Fatalf("a loaded session wrote %d rows, want one touch", n)
	}
	row := idx.last()
	if row.TitleKind != sessions.TitleKindNone || row.Title != "" {
		t.Fatalf("the touch carried a title: %+v", row)
	}
	if row.SessionID != m.snap.SessionID || row.SessionID == "" || row.CWD != m.cwd {
		t.Fatalf("touch row %+v, want the session's own id and cwd %q", row, m.cwd)
	}
	if row.CrazeID == "" {
		t.Fatalf("the touch carried no durable craze id: %+v", row)
	}
}

// TestFirstSendWritesTheFallbackTitle is §3.2's creation moment: the row exists
// once there is something to show, and the first line of the first prompt is
// what a picker shows until the agent or the user names the session.
func TestFirstSendWritesTheFallbackTitle(t *testing.T) {
	idx := &fakeIndex{}
	m, _ := loadedStub(t, idx)
	m.replaying = false
	m = deliver(t, m, startedMsg{})

	long := strings.Repeat("é", 200)
	m = pumpEnter(t, m, "  first line of the prompt  \n second line \n"+long)
	if m.status != statusWorking {
		t.Fatalf("the send was refused: status %s", m.status)
	}
	// The seed is Submit's own, on this goroutine (plan 021 §3.2's documented
	// exception), so it has happened by the time Enter's Update has returned.
	row := idx.seedRow(t)
	if row.Title != "first line of the prompt" {
		t.Fatalf("fallback title %q, want the sanitised first line", row.Title)
	}
	if row.Pinned {
		t.Fatal("the fallback title is not a pin")
	}
	if row.CrazeID == "" {
		t.Fatalf("the first-prompt row carried no durable craze id: %+v", row)
	}

	// A second send changes nothing a title rule would keep, so it does not
	// write a second first-prompt title. Counting the seeds rather than the
	// rows is what makes that a claim about the seed alone: a turn's end
	// touches the row on the engine's worker, whenever the worker gets to it.
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	m = pumpEnter(t, m, "second prompt")
	if m.status != statusWorking {
		t.Fatalf("the second send was refused: status %s", m.status)
	}
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	if n := idx.seeds(); n != 1 {
		t.Fatalf("%d first-prompt rows were written: %+v", n, idx.all())
	}
}

// TestIndexTitleIsCappedAt120Runes is the TUI's half of the fallback rule: how
// a title is folded onto one row and capped. Taking the first line of a prompt
// is the engine's half now (engine's TestFallbackTitleIsTheFirstLine).
func TestIndexTitleIsCappedAt120Runes(t *testing.T) {
	got := indexTitleLine(strings.Repeat("é", 200))
	if n := len([]rune(got)); n != titleRuneCap {
		t.Fatalf("title is %d runes, want %d", n, titleRuneCap)
	}
}

// TestAgentTitleFillsTheRowAndAPinBeatsIt covers the two title rules the TUI
// owns the moments for: session_info_update arrives as an EventMeta carrying a
// title, and /rename pins — after which the session itself refuses a later agent
// title, so the index is never asked to overwrite one.
func TestAgentTitleFillsTheRowAndAPinBeatsIt(t *testing.T) {
	idx := &fakeIndex{}
	m, stub := loadedStub(t, idx)
	m.replaying = false
	m = deliver(t, m, startedMsg{})

	stub.AgentTitle("list the working directory")
	// Emitted through the log, not handed to Update: an agent title is an
	// event-driven write, so it is the engine's observer that has to see it,
	// and the index worker that makes it (plan 021 §3.8).
	stub.Emit(agent.Event{Type: agent.EventMeta, Text: "list the working directory"})
	waitRows(t, idx, 1)
	m = pumpDrained(t, m)
	row := idx.last()
	if row.TitleKind != sessions.TitleKindAgent || row.Title != "list the working directory" {
		t.Fatalf("agent title row %+v", row)
	}

	m = runSlash(t, m, "/rename fix the flaky pty test")
	row = idx.last()
	if row.TitleKind != sessions.TitleKindUser || row.Title != "fix the flaky pty test" {
		t.Fatalf("rename row %+v", row)
	}
	if got := m.snap.Title; got != "fix the flaky pty test" {
		t.Fatalf("composer title %q", got)
	}

	// The pin is the session's, so a later agent title never even reaches the
	// index: the session drops it and no EventMeta carrying one is emitted.
	stub.AgentTitle("a later agent title")
	if got := stub.Snapshot().Title; got != "fix the flaky pty test" {
		t.Fatalf("the pin did not hold: %q", got)
	}
	n := idx.count()
	stub.Emit(agent.Event{Type: agent.EventMeta})
	m = pumpSettled(t, m)
	if got := idx.count(); got != n {
		t.Fatalf("a title-less EventMeta wrote a row: %+v", idx.last())
	}
}

func TestEventDoneTouchesTheRow(t *testing.T) {
	idx := &fakeIndex{}
	m, stub := loadedStub(t, idx)
	m.replaying = false
	m = deliver(t, m, startedMsg{})

	// No row yet, so the turn a foreign agent could run before craze ever
	// prompted must not conjure a titleless one. Emitted through the log,
	// because a turn's end is the engine's observer's to see.
	stub.Emit(agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	m = pumpSettled(t, m)
	if n := idx.count(); n != 0 {
		t.Fatalf("EventDone created a row out of nothing: %+v", idx.all())
	}

	// Real turns: the first send creates the row, and a turn's own ending
	// touches it. The creation is Submit's own; the touch is the worker's.
	//
	// Two turns, because the FIRST turn's touch is not owed: Submit launches the
	// turn before it writes the seed, so a turn that ends at once can have its
	// touch taken while there is still no row — and a touch never conjures one
	// (CI found a test that counted on it). By the second turn's end the row
	// exists, so that touch always lands.
	m = pumpEnter(t, m, "hello")
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	waitRows(t, idx, 1)
	m = pumpEnter(t, m, "again")
	m = pumpUntil(t, m, isIdle)
	_ = pumpSettled(t, m)
	waitRows(t, idx, 2)
	rows := idx.all()
	if n := idx.seeds(); n != 1 || rows[0].Title != "hello" {
		t.Fatalf("rows %+v, want one creation, first", rows)
	}
	for _, got := range rows[1:] {
		if got.TitleKind != sessions.TitleKindNone || got.Title != "" {
			t.Fatalf("a turn's end wrote %+v, want a touch: %+v", got, rows)
		}
	}
}

// TestIndexErrorsAreTranscriptLines: craze not being able to remember a session
// is not a reason to stop running it, so a failed write reads like a failed
// SaveTheme — an error line and nothing else.
func TestIndexErrorsAreTranscriptLines(t *testing.T) {
	idx := &fakeIndex{err: errFakeIndex}
	m, _ := loadedStub(t, idx)
	m.replaying = false
	m = deliver(t, m, startedMsg{})
	m = pumpEnter(t, m, "a prompt")
	if m.status != statusWorking {
		t.Fatalf("the send was refused because the index failed: status %s", m.status)
	}
	// The seed ran on this goroutine, but its failure cannot come back as
	// Submit's answer — the turn is the answer — so it arrives as a
	// StateDelta{IndexErr} the model draws the same row from (plan 021 §3.8).
	m = pumpUntil(t, m, func(m Model) bool { return len(texts(m, entryError)) > 0 })
	if got := texts(m, entryError); len(got) != 1 || !strings.Contains(got[0], "no home directory") {
		t.Fatalf("error entries %q", got)
	}

	// A failed write does not claim the row exists, which is observable: a
	// turn's end touches nothing when there is nothing to touch. The index is
	// let work again first, so the only reason for silence is the missing row.
	tries := idx.tries()
	idx.setErr(nil)
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	m = pumpSettled(t, m)
	if got := idx.tries(); got != tries {
		t.Fatalf("a failed write left %d attempts and the touch made %d: %+v", tries, got, idx.all())
	}
}

// TestFirstSendRetriesAFailedIndexWrite: the first prompt is what creates the
// row, so a write that failed must not retire the attempt — a session whose
// very first write lost a race for the lock would otherwise never appear in a
// picker again, however many turns it went on to run.
func TestFirstSendRetriesAFailedIndexWrite(t *testing.T) {
	idx := &fakeIndex{err: errFakeIndex}
	m, _ := loadedStub(t, idx)
	m.replaying = false
	m = deliver(t, m, startedMsg{})

	m = pumpEnter(t, m, "the first prompt")
	waitAttempts(t, idx, 1)
	if n := idx.count(); n != 0 {
		t.Fatalf("a failing index recorded %d rows", n)
	}

	// The next send finds the index working again, and the fallback title is
	// tried once more: a failed write must not have retired it.
	idx.setErr(nil)
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	m = pumpEnter(t, m, "the second prompt")
	if got := idx.seedRow(t); got.Title != "the second prompt" {
		t.Fatalf("the retry wrote %+v", got)
	}

	// And now it IS retired: a third send writes no second first-prompt row,
	// and the row the retry created is there to be touched.
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	m = pumpEnter(t, m, "the third prompt")
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	if n := idx.seeds(); n != 1 {
		t.Fatalf("%d first-prompt rows were written: %+v", n, idx.all())
	}
	waitRows(t, idx, 2)
	if got := idx.last(); got.TitleKind != sessions.TitleKindNone {
		t.Fatalf("after the retry a turn's end wrote %+v, want a touch", got)
	}
}

// TestHiddenProviderSessionIsNeverIndexed is plan 018 §3.4: a hidden
// provider's sessions stay out of the shared index until they have a loader,
// so no write moment — the first send, an agent title, a turn's end, /rename —
// reaches Upsert. Skipping counts as done, so the first-prompt write is not
// retried, and it is not an error line. The provider is read from the
// snapshot, or from the resolved default before a session has reported one.
func TestHiddenProviderSessionIsNeverIndexed(t *testing.T) {
	hidden := plantHidden(t)
	for _, tc := range []struct {
		name      string
		configure func(stub *Stub) agent.Provider
	}{
		{"snapshot", func(stub *Stub) agent.Provider {
			stub.SetProvider(hidden)
			return agent.CursorProvider()
		}},
		{"resolved default", func(*Stub) agent.Provider { return hidden }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateSkillsHome(t)
			idx := &fakeIndex{}
			sess := newScriptedSession()
			def := tc.configure(sess.Stub)
			m := New(Config{
				Session:        sess,
				Theme:          "tokyo-night",
				Workspace:      t.TempDir(),
				Model:          "grok",
				Yolo:           true,
				Provider:       def,
				ProviderLocked: true,
				SessionIndex:   idx,
			})
			tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			m = deliver(t, tm.(Model), startedMsg{})
			if m.snap.SessionID == "" {
				t.Fatal("fixture: the session has no id, so nothing would be written anyway")
			}

			// The turn is a real one, open and then ended cleanly, so the agent's
			// own title lands inside it and the turn's end runs the touch: every
			// write moment — the first send, the agent title, the turn's end,
			// /rename — is reached, and none of them may write.
			sc := scriptHeld()
			m = startScripted(t, m, sess, "the first prompt", sc)
			sess.AgentTitle("an agent title")
			sess.Emit(agent.Event{Type: agent.EventMeta, Text: "an agent title"})
			m = pumpUntil(t, m, viewHas("an agent title"))
			sc.Release()
			m = pumpUntil(t, m, isIdle)
			m = pumpSettled(t, m)
			m = runSlash(t, m, "/rename a user title")
			if n := idx.tries(); n != 0 {
				t.Fatalf("a hidden provider's session was indexed: %+v", idx.all())
			}
			if got := texts(m, entryError); len(got) != 0 {
				t.Fatalf("skipping the index is not an error: %q", got)
			}
		})
	}
}

// TestNilSessionIndexWritesNothing is the isolation §3.2 pins: a nil index is
// what every TUI unit test and every golden runs with, so nothing here can
// reach a developer's real ~/.craze/sessions.jsonl.
func TestNilSessionIndexWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRAZE_HOME", "")

	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })
	m := New(Config{Session: stub, Theme: "tokyo-night", Workspace: t.TempDir(), Model: "grok", Yolo: true, Loading: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	if m.sessionIndex != nil {
		t.Fatal("a Config without a SessionIndex must leave the model without one")
	}
	m = deliver(t, m, startedMsg{})
	// Emitted through the log, so every write moment the ENGINE owns is really
	// reached — the loaded row's touch, the agent's title, a turn's end — and
	// not only the two a client's own command makes.
	stub.Emit(replayEvent(agent.ReplayEnd))
	m = pumpUntil(t, m, func(m Model) bool { return m.sessionReady() })
	m = pumpEnter(t, m, "a prompt that would create a row")
	stub.Emit(agent.Event{Type: agent.EventMeta, Text: "an agent title"})
	m = pumpUntil(t, m, isIdle)
	m = pumpSettled(t, m)
	m = runSlash(t, m, "/rename a user title")
	if got := texts(m, entryError); len(got) != 0 {
		t.Fatalf("a nil index produced errors: %q", got)
	}

	var found []string
	if err := filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(d.Name(), "sessions") {
			found = append(found, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("a nil index wrote %v", found)
	}
}

// ---------------------------------------------------------------- /rename

func TestRenameBeforeTheSessionIsUpIsRefused(t *testing.T) {
	idx := &fakeIndex{}
	m, stub := loadedStub(t, idx)
	// started, but the replay has not ended: the half-open state CodeRabbit's
	// finding is about.
	m = deliver(t, m, startedMsg{})
	m = runSlash(t, m, "/rename too early")
	if got := texts(m, entryError); len(got) != 1 || got[0] != "session is still starting" {
		t.Fatalf("error entries %q", got)
	}
	if stub.Snapshot().Title != "" {
		t.Fatalf("the session was renamed while restoring: %q", stub.Snapshot().Title)
	}
	if idx.count() != 0 {
		t.Fatalf("a refused rename wrote %+v", idx.all())
	}
	if m.input.Value() != "" {
		t.Fatalf("the draft survived a refused rename: %q", m.input.Value())
	}
}

func TestRenameWithNoTitleIsAUsageError(t *testing.T) {
	for _, args := range []string{"", "   ", "\a"} {
		m, stub := loadedStub(t, &fakeIndex{})
		m.replaying = false
		m = deliver(t, m, startedMsg{})
		next := runSlash(t, m, strings.TrimRight("/rename "+args, " "))
		if got := texts(next, entryError); len(got) != 1 || got[0] != "usage: /rename <title>" {
			t.Fatalf("/rename %q: error entries %q", args, got)
		}
		if stub.Snapshot().Title != "" {
			t.Fatalf("/rename %q renamed the session", args)
		}
	}
}

func TestRenameRunsWhileATurnIsWorking(t *testing.T) {
	if !runsWhileWorking("rename") {
		t.Fatal("/rename touches no wire, so a running turn must not refuse it")
	}
	idx := &fakeIndex{}
	m, stub := loadedStub(t, idx)
	m.replaying = false
	m = deliver(t, m, startedMsg{})
	m, _ = typeAndEnter(t, m, "a prompt")
	if m.status != statusWorking {
		t.Fatal("the turn is not working")
	}
	m = runSlash(t, m, "/rename mid-turn name")
	if stub.Snapshot().Title != "mid-turn name" {
		t.Fatalf("session title %q", stub.Snapshot().Title)
	}
	if got := texts(m, entryNote); len(got) == 0 || got[len(got)-1] != "renamed to mid-turn name" {
		t.Fatalf("notes %q", got)
	}
	if m.status != statusWorking {
		t.Fatal("/rename disturbed the running turn")
	}
}

// runSlash puts a slash line in the composer and presses Enter, so the command
// under test goes through the same dispatch a user's would.
func runSlash(t *testing.T, m Model, line string) Model {
	t.Helper()
	m.input.SetValue(line)
	tm, _ := m.Update(enter())
	return tm.(Model)
}

var errFakeIndex = fakeIndexError("craze: no home directory to save the session index in")

type fakeIndexError string

func (e fakeIndexError) Error() string { return string(e) }
