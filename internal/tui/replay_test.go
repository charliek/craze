package tui

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/sessions"
)

// ---------------------------------------------------------------- fixtures

// fakeIndex is Config.SessionIndex as a recorder. It is the whole of what the
// TUI knows about the index — one method — which is why a unit test can hold
// every write moment in §3.2 without a file, a lock or a HOME.
type fakeIndex struct {
	rows []sessions.Row
	err  error
}

func (f *fakeIndex) Upsert(row sessions.Row) error {
	if f.err != nil {
		return f.err
	}
	f.rows = append(f.rows, row)
	return nil
}

func (f *fakeIndex) last() sessions.Row {
	if len(f.rows) == 0 {
		return sessions.Row{}
	}
	return f.rows[len(f.rows)-1]
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
	if m.replaying || m.loading || !m.sessionReady() {
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

// TestReplayLeavesTheTurnAlone: a replay is history, not a turn. Nothing may
// bump turnSeq — a turn start retires the last turn's plan offer and its
// evidence, and a replay retiring them would be the restored transcript
// answering for a session it is not part of.
func TestReplayLeavesTheTurnAlone(t *testing.T) {
	m, _ := loadedStub(t, nil)
	seq := m.turnSeq
	m = feed(t, m, replayEvent(agent.ReplayStart))
	for _, ev := range replayTranscript() {
		m = feed(t, m, replayed(ev))
	}
	m = feed(t, m, replayEvent(agent.ReplayEnd))
	if m.turnSeq != seq {
		t.Fatalf("turnSeq moved from %d to %d during a replay", seq, m.turnSeq)
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
	if m.main.streamOpen {
		t.Fatal("the replay left a stream open under the restored note")
	}
	for _, e := range m.main.entries {
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
	m.replaying, m.loading = false, false
	m = deliver(t, m, startedMsg{})
	if !m.sessionReady() {
		t.Fatal("the session never came up")
	}
	if len(idx.rows) != 0 {
		t.Fatalf("a session with no prompt wrote %d rows: %+v", len(idx.rows), idx.rows)
	}
}

func TestLoadedSessionTouchesItsRowWhenItComesUp(t *testing.T) {
	idx := &fakeIndex{}
	m, _ := loadedStub(t, idx)
	m = deliver(t, m, startedMsg{})
	if len(idx.rows) != 0 {
		t.Fatalf("the row was touched before the replay ended: %+v", idx.rows)
	}
	m = feed(t, m, replayEvent(agent.ReplayEnd))
	if len(idx.rows) != 1 {
		t.Fatalf("a loaded session wrote %d rows, want one touch", len(idx.rows))
	}
	row := idx.last()
	if row.TitleKind != sessions.TitleKindNone || row.Title != "" {
		t.Fatalf("the touch carried a title: %+v", row)
	}
	if row.SessionID != m.snap.SessionID || row.SessionID == "" || row.CWD != m.cwd {
		t.Fatalf("touch row %+v, want the session's own id and cwd %q", row, m.cwd)
	}
}

// TestFirstSendWritesTheFallbackTitle is §3.2's creation moment: the row exists
// once there is something to show, and the first line of the first prompt is
// what a picker shows until the agent or the user names the session.
func TestFirstSendWritesTheFallbackTitle(t *testing.T) {
	idx := &fakeIndex{}
	m, _ := loadedStub(t, idx)
	m.replaying, m.loading = false, false
	m = deliver(t, m, startedMsg{})

	long := strings.Repeat("é", 200)
	m, cmd := typeAndEnter(t, m, "  first line of the prompt  \n second line \n"+long)
	if cmd == nil {
		t.Fatal("the send was refused")
	}
	if len(idx.rows) != 1 {
		t.Fatalf("the first send wrote %d rows", len(idx.rows))
	}
	row := idx.last()
	if row.TitleKind != sessions.TitleKindFallback {
		t.Fatalf("title kind %v, want fallback", row.TitleKind)
	}
	if row.Title != "first line of the prompt" {
		t.Fatalf("fallback title %q, want the sanitised first line", row.Title)
	}
	if row.Pinned {
		t.Fatal("the fallback title is not a pin")
	}

	// A second send changes nothing a title rule would keep, so it does not
	// rewrite the file.
	m.status = statusIdle
	m.promptEndSeq, m.streamEndSeq = m.turnSeq, m.turnSeq
	if _, cmd := typeAndEnter(t, m, "second prompt"); cmd == nil {
		t.Fatal("the second send was refused")
	}
	if len(idx.rows) != 1 {
		t.Fatalf("the second send wrote again: %+v", idx.rows)
	}
}

func TestFallbackTitleIsCappedAt120Runes(t *testing.T) {
	got := fallbackTitle(strings.Repeat("é", 200) + "\nsecond line")
	if n := len([]rune(got)); n != titleRuneCap {
		t.Fatalf("title is %d runes, want %d", n, titleRuneCap)
	}
	if strings.Contains(got, "second line") {
		t.Fatalf("the fallback took more than the first line: %q", got)
	}
}

// TestAgentTitleFillsTheRowAndAPinBeatsIt covers the two title rules the TUI
// owns the moments for: session_info_update arrives as an EventMeta carrying a
// title, and /rename pins — after which the session itself refuses a later agent
// title, so the index is never asked to overwrite one.
func TestAgentTitleFillsTheRowAndAPinBeatsIt(t *testing.T) {
	idx := &fakeIndex{}
	m, stub := loadedStub(t, idx)
	m.replaying, m.loading = false, false
	m = deliver(t, m, startedMsg{})

	stub.AgentTitle("list the working directory")
	m = feed(t, m, agent.Event{Type: agent.EventMeta, Text: "list the working directory"})
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
	n := len(idx.rows)
	m = feed(t, m, agent.Event{Type: agent.EventMeta})
	if len(idx.rows) != n {
		t.Fatalf("a title-less EventMeta wrote a row: %+v", idx.last())
	}
}

func TestEventDoneTouchesTheRow(t *testing.T) {
	idx := &fakeIndex{}
	m, _ := loadedStub(t, idx)
	m.replaying, m.loading = false, false
	m = deliver(t, m, startedMsg{})

	// No row yet, so the turn a foreign agent could run before craze ever
	// prompted must not conjure a titleless one.
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	if len(idx.rows) != 0 {
		t.Fatalf("EventDone created a row out of nothing: %+v", idx.rows)
	}

	m, _ = typeAndEnter(t, m, "hello")
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	if len(idx.rows) != 2 {
		t.Fatalf("rows %+v, want the creation and the touch", idx.rows)
	}
	if got := idx.last(); got.TitleKind != sessions.TitleKindNone || got.Title != "" {
		t.Fatalf("the touch carried a title: %+v", got)
	}
}

// TestIndexErrorsAreTranscriptLines: craze not being able to remember a session
// is not a reason to stop running it, so a failed write reads like a failed
// SaveTheme — an error line and nothing else.
func TestIndexErrorsAreTranscriptLines(t *testing.T) {
	idx := &fakeIndex{err: errFakeIndex}
	m, _ := loadedStub(t, idx)
	m.replaying, m.loading = false, false
	m = deliver(t, m, startedMsg{})
	m, cmd := typeAndEnter(t, m, "a prompt")
	if cmd == nil {
		t.Fatal("the send was refused because the index failed")
	}
	if got := texts(m, entryError); len(got) != 1 || !strings.Contains(got[0], "no home directory") {
		t.Fatalf("error entries %q", got)
	}
	if m.indexRow {
		t.Fatal("a failed write must not claim the row exists")
	}
}

// TestFirstSendRetriesAFailedIndexWrite: the first prompt is what creates the
// row, so a write that failed must not retire the attempt — a session whose
// very first write lost a race for the lock would otherwise never appear in a
// picker again, however many turns it went on to run.
func TestFirstSendRetriesAFailedIndexWrite(t *testing.T) {
	idx := &fakeIndex{err: errFakeIndex}
	m, _ := loadedStub(t, idx)
	m.replaying, m.loading = false, false
	m = deliver(t, m, startedMsg{})

	m, _ = typeAndEnter(t, m, "the first prompt")
	if len(idx.rows) != 0 {
		t.Fatalf("a failing index recorded %d rows", len(idx.rows))
	}
	if m.indexSeeded {
		t.Fatal("a failed write must not retire the first-prompt title")
	}

	// The next send finds the index working again.
	idx.err = nil
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	m = deliver(t, m, promptDoneMsg{})
	if m.status != statusIdle {
		t.Fatalf("the turn did not settle: status %v", m.status)
	}
	m, _ = typeAndEnter(t, m, "the second prompt")
	if len(idx.rows) != 1 {
		t.Fatalf("the retry wrote %d rows, want 1", len(idx.rows))
	}
	if got := idx.last(); got.Title != "the second prompt" || got.TitleKind != sessions.TitleKindFallback {
		t.Fatalf("the retry wrote %+v", got)
	}
	if !m.indexSeeded || !m.indexRow {
		t.Fatalf("after a successful retry: seeded=%v row=%v", m.indexSeeded, m.indexRow)
	}
}

// TestNilSessionIndexWritesNothing is the isolation §3.2 pins: a nil index is
// what every TUI unit test and every golden runs with, so nothing here can
// reach a developer's real ~/.craze/sessions.jsonl.
func TestNilSessionIndexWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRAZE_CONFIG", "")

	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })
	m := New(Config{Session: stub, Theme: "tokyo-night", Workspace: t.TempDir(), Model: "grok", Yolo: true, Loading: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	if m.sessionIndex != nil {
		t.Fatal("a Config without a SessionIndex must leave the model without one")
	}
	m = deliver(t, m, startedMsg{})
	m = feed(t, m, replayEvent(agent.ReplayEnd))
	m, _ = typeAndEnter(t, m, "a prompt that would create a row")
	m = feed(t, m,
		agent.Event{Type: agent.EventMeta, Text: "an agent title"},
		agent.Event{Type: agent.EventDone, StopReason: "end_turn"},
	)
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
	if len(idx.rows) != 0 {
		t.Fatalf("a refused rename wrote %+v", idx.rows)
	}
	if m.input.Value() != "" {
		t.Fatalf("the draft survived a refused rename: %q", m.input.Value())
	}
}

func TestRenameWithNoTitleIsAUsageError(t *testing.T) {
	m, stub := loadedStub(t, &fakeIndex{})
	m.replaying, m.loading = false, false
	m = deliver(t, m, startedMsg{})
	for _, args := range []string{"", "   ", "\a"} {
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
	m.replaying, m.loading = false, false
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
