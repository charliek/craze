package tui

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/harness"
)

// Models picked up live, the TUI's half (plan 034 §3.4 "TUI (C6)"): these are
// the unit tests, over the Stub that stands for a native session
// (RefreshingStub), of when the TUI asks for a refresh and what it says of the
// answer. The real session in both transports is native_live_golden_test.go's.

// refreshStubModels is the list a refreshing stub's session offers at the
// start, and what a staged refresh adds to it.
var (
	refreshBase   = []agent.ModelInfo{{ID: "alpha/one", Name: "Alpha One"}, {ID: "beta/fast", Name: "Beta Fast"}}
	refreshStaged = append(slices.Clone(refreshBase), agent.ModelInfo{ID: "gamma/big", Name: "Gamma Big"})
)

// refreshCounter is a backend that counts the RefreshModels calls the TUI
// makes, answering them from the backend behind it.
type refreshCounter struct {
	backend.Backend
	calls atomic.Int32
	dirs  []string
}

// engine names the engine behind the wrapped backend (engineBehind), so the
// pump and engineOf see through it.
func (c *refreshCounter) engine() *engine.Engine { return engineBehind(c.Backend) }

func (c *refreshCounter) RefreshModels(ctx context.Context, dir string) (agent.ModelsRefresh, error) {
	c.calls.Add(1)
	c.dirs = append(c.dirs, dir)
	return c.Backend.RefreshModels(ctx, dir)
}

// refreshModel is a started native model over a stub that takes up models
// while it runs (or, with plain, over the plain Stub, which does not),
// counting the refreshes the TUI asks for. The stub's session "reads" dir.
func refreshModel(t *testing.T, plain bool) (Model, *Stub, *refreshCounter, string, func(string) string) {
	t.Helper()
	dir, getenv := connectFixture(t, nil)
	stub := nativeStub()
	stub.SetModels(refreshBase)
	stub.SetNativeDir(dir)
	var sess agent.Session = stub
	if !plain {
		sess = RefreshingStub{Stub: stub}
	}
	t.Cleanup(func() { _ = stub.Close() })
	m := connectModelOf(t, Config{Session: sess, NativeDir: dir, Getenv: getenv})
	c := &refreshCounter{Backend: m.eng}
	m.eng = c
	return m, stub, c, dir, getenv
}

// saveGamma is /connect, Enter on gamma — the first provider with no key —
// the key pasted and saved, with the stub's deltas folded after it.
func saveGamma(t *testing.T, m Model, stub *Stub) Model {
	t.Helper()
	m, _ = typeCommand(t, m, "/connect")
	m = pressKey(t, m, tea.KeyEnter)
	tm, _ := m.Update(pasteKey(connectCanary))
	m, _ = press(tm.(Model), enter())
	return feed(t, m, stubDeltas(t, stub)...)
}

func allNotes(m Model) string { return strings.Join(texts(m, entryNote), "\n") }

// TestAKeySaveSaysWhatItsRefreshCameTo (A16's note, A18's, A26, A25's TUI
// side): after /connect stores a key, the session is asked to take up the
// models — with the TUI's native directory, once — and the notice says what
// came of it. applied: they are in /model now, and the picker lists them with
// no /exit; pending (a turn runs): after this turn; a host reading another
// craze directory: that, alone — not the old note's /exit and craze -c, which
// would not reach that host's files either (plan 034 C6r); current and
// failed: the old note.
func TestAKeySaveSaysWhatItsRefreshCameTo(t *testing.T) {
	const old = "Connected Gamma. New sessions offer its models; to use them in this conversation, /exit and run craze -c."
	for _, tc := range []struct {
		name  string
		setup func(*Stub)
		want  string
		lists bool
	}{
		{"applied", func(s *Stub) { s.StageModels(refreshStaged) },
			"Connected Gamma. Its models are in /model now.", true},
		{"pending", func(s *Stub) {
			s.StageModels(refreshStaged)
			s.mu.Lock()
			s.claimed = true // a turn another client began
			s.mu.Unlock()
		}, "Connected Gamma. Its models will be in /model after this turn.", false},
		{"another directory", func(s *Stub) { s.StageModels(refreshStaged); s.SetNativeDir("/somewhere/else/native") },
			"Connected Gamma in this craze directory, but this session's host reads another one, so it does not see its models.", true},
		{"current", func(*Stub) {}, old, false},
		{"failed", func(s *Stub) { s.StageModels(refreshStaged); s.FailNextRefresh() }, old, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stub, count, dir, _ := refreshModel(t, false)
			tc.setup(stub)
			m = saveGamma(t, m, stub)
			if got := allNotes(m); got != tc.want {
				t.Fatalf("notes:\n%s\nwant:\n%s", got, tc.want)
			}
			if count.calls.Load() != 1 || !slices.Equal(count.dirs, []string{dir}) {
				t.Fatalf("%d refreshes asked, for %q; want one, for the TUI's native directory", count.calls.Load(), count.dirs)
			}
			listed := slices.ContainsFunc(m.snap.Models, func(md agent.ModelInfo) bool { return md.ID == "gamma/big" })
			if listed != tc.lists {
				t.Fatalf("the picker lists gamma's model: %v, want %v (the catalog moves with the refresh, whatever the note says)", listed, tc.lists)
			}
		})
	}
}

// TestNoCapabilityNoRefresh (A25's TUI side): a session that cannot take up
// models while it runs — an ACP session, a host from before the method, a
// plain Stub — is never asked, and the notice is today's, on a key's save and
// on a sign-in. The control is the capable session of the test above, asked
// once.
func TestNoCapabilityNoRefresh(t *testing.T) {
	m, stub, count, _, _ := refreshModel(t, true)
	if m.canRefreshModels() {
		t.Fatal("a plain Stub says it takes up models while it runs")
	}
	m = saveGamma(t, m, stub)
	const old = "Connected Gamma. New sessions offer its models; to use them in this conversation, /exit and run craze -c."
	if got := allNotes(m); got != old {
		t.Fatalf("notes %q, want today's", got)
	}
	m = applyMsg(t, m, signInFinishedMsg{shownGen: m.shownGen, aliases: []string{"chatgpt/gpt-5.6-sol"}})
	if notes := texts(m, entryNote); notes[len(notes)-1] != signedInSessionNote(nil) || strings.Contains(notes[len(notes)-1], "/model") {
		t.Fatalf("the sign-in's last note is %q, want today's", notes[len(notes)-1])
	}
	m, _ = typeCommand(t, m, "/model")
	if m.dialog != dialogModel {
		t.Fatal("/model did not open")
	}
	if n := count.calls.Load(); n != 0 {
		t.Fatalf("%d refreshes asked of a session that cannot take them", n)
	}
}

// TestASignInSaysWhatItsRefreshCameTo (A17's note): the refresh follows the
// sign-in's model list — signInFinishedMsg, not the redirect's signInDoneMsg —
// and its note replaces the old last line, with the models noted before it.
func TestASignInSaysWhatItsRefreshCameTo(t *testing.T) {
	const old = "New sessions offer the ChatGPT plan's models; to use them in this conversation, /exit and run craze -c."
	for _, tc := range []struct {
		name  string
		setup func(*Stub)
		want  string
	}{
		{"applied", func(s *Stub) { s.StageModels(refreshStaged) }, "The plan's models are in /model now."},
		{"pending", func(s *Stub) {
			s.StageModels(refreshStaged)
			s.mu.Lock()
			s.claimed = true
			s.mu.Unlock()
		}, "The plan's models will be in /model after this turn."},
		{"another directory", func(s *Stub) { s.StageModels(refreshStaged); s.SetNativeDir("/somewhere/else/native") },
			"The sign-in is saved in this craze directory, but this session's host reads another one, so it does not see the plan's models."},
		{"current", func(*Stub) {}, old},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stub, count, _, _ := refreshModel(t, false)
			tc.setup(stub)
			// The redirect's own message asks for nothing: the list is not
			// written yet.
			m = applyMsg(t, m, signInFinishedMsg{shownGen: m.shownGen, aliases: []string{"chatgpt/gpt-5.6-sol"}})
			notes := texts(m, entryNote)
			if len(notes) != 2 || notes[0] != "ChatGPT plan models: chatgpt/gpt-5.6-sol" || notes[1] != tc.want {
				t.Fatalf("notes %q, want the models and %q", notes, tc.want)
			}
			if count.calls.Load() != 1 {
				t.Fatalf("%d refreshes asked, want one", count.calls.Load())
			}
		})
	}
	// signInDoneMsg is not the list written: no refresh before it.
	m, _, count, _, _ := refreshModel(t, false)
	_, _ = m.signedIn(signInResultFixture(), nil)
	if count.calls.Load() != 0 {
		t.Fatalf("%d refreshes asked at the redirect, before the list was written", count.calls.Load())
	}
}

// TestModelOpenAsksForARefresh: /model, and the status row's click, open the
// dialog and ask a capable session to take up what was funded since it
// opened — once per opening — and the picker lists it when the catalog delta
// lands, with no reopening. A session that cannot is not asked (the test
// above).
func TestModelOpenAsksForARefresh(t *testing.T) {
	m, stub, count, _, _ := refreshModel(t, false)
	stub.StageModels(refreshStaged)
	m, _ = typeCommand(t, m, "/model")
	if m.dialog != dialogModel || count.calls.Load() != 1 {
		t.Fatalf("dialog %v, %d refreshes asked at /model", m.dialog, count.calls.Load())
	}
	m = feed(t, m, stubDeltas(t, stub)...)
	var ids []string
	for _, md := range m.dialogModelList() {
		ids = append(ids, md.ID)
	}
	if !slices.Contains(ids, "gamma/big") {
		t.Fatalf("the open dialog does not list what the refresh found: %q", ids)
	}
	if notes := texts(m, entryNote); len(notes) != 0 {
		t.Fatalf("opening /model wrote notes %q", notes)
	}
	m = pressKey(t, m, tea.KeyEsc)
	m, _ = typeCommand(t, m, "/model")
	if count.calls.Load() != 2 {
		t.Fatalf("a second opening asked %d times in all, want 2", count.calls.Load())
	}
}

// TestTheModelDialogKeepsItsSelectionWhenModelsAreAdded: a list that gains a
// model while the box is open (another terminal's key, a refresh) leaves the
// selection on the model it was on — or on the connect row — not on whatever
// the old index now names. Reordering alone (the current model first) keeps
// the index, as it always has.
func TestTheModelDialogKeepsItsSelectionWhenModelsAreAdded(t *testing.T) {
	m, stub, _, _, _ := refreshModel(t, false)
	stub.SetModels([]agent.ModelInfo{{ID: "m1", Name: "Mango"}, {ID: "m3", Name: "Plum"}})
	m = feed(t, m, stubDeltas(t, stub)...)
	m, _ = typeCommand(t, m, "/model")
	m = feed(t, m, stubDeltas(t, stub)...)
	m = pressKey(t, m, tea.KeyDown)
	sel := func() string { return m.dialogModelList()[m.mdlg.sel].ID }
	before := sel()
	// A model arrives that sorts ahead of the selection.
	stub.StageModels([]agent.ModelInfo{{ID: "m0", Name: "Apple"}, {ID: "m1", Name: "Mango"}, {ID: "m3", Name: "Plum"}})
	if _, err := (RefreshingStub{Stub: stub}).RefreshModels(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	m = feed(t, m, stubDeltas(t, stub)...)
	if got := sel(); got != before {
		t.Fatalf("the selection moved from %q to %q when a model was added", before, got)
	}
	// On the connect row, it stays there.
	m.mdlg.connect = true
	m.mdlg.sel = len(m.dialogModelList())
	rows := len(m.dialogModelList())
	stub.StageModels(append(slices.Clone(m.snap.Models), agent.ModelInfo{ID: "m9", Name: "Zed"}))
	if _, err := (RefreshingStub{Stub: stub}).RefreshModels(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	m = feed(t, m, stubDeltas(t, stub)...)
	if m.mdlg.sel != len(m.dialogModelList()) || len(m.dialogModelList()) != rows+1 {
		t.Fatalf("the selection left the connect row: %d of %d models", m.mdlg.sel, len(m.dialogModelList()))
	}
}

// TestAConnectDuringATurnIsRefused (A18's other half): /connect stays refused
// while a turn runs, and a refresh at /model then is pending — the list
// unchanged — until the turn ends, when the catalog delta lands in the open
// box with no reopening.
func TestAConnectDuringATurnIsRefused(t *testing.T) {
	m, stub, _, _, _ := refreshModel(t, false)
	open := stub.HangNext()
	m = pumpEnter(t, m, "go")
	awaitBarrier(t, open, "the turn opening")
	m = pumpDrained(t, m)

	m, _ = typeCommand(t, m, "/connect")
	if m.dialog != dialogNone || !slices.Contains(texts(m, entryError), connectBusyText) {
		t.Fatalf("/connect during a turn: dialog %v, errors %q", m.dialog, texts(m, entryError))
	}

	stub.StageModels(refreshStaged)
	// The status row's click: a typed /model would queue behind the turn.
	tm, cmd := m.showModelDialog()
	m = tm.(Model)
	if cmd != nil {
		m = pumpCmd(t, m, cmd)
	}
	m = pumpDrained(t, m)
	if m.dialog != dialogModel {
		t.Fatalf("/model did not open mid-turn (dialog %v)", m.dialog)
	}
	has := func() bool {
		return slices.ContainsFunc(m.dialogModelList(), func(md agent.ModelInfo) bool { return md.ID == "gamma/big" })
	}
	if has() {
		t.Fatal("a refresh during a turn put the models in the list at once")
	}
	if _, err := stub.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	m = pumpUntil(t, m, func(m Model) bool {
		return slices.ContainsFunc(m.dialogModelList(), func(md agent.ModelInfo) bool { return md.ID == "gamma/big" })
	})
	if m.dialog != dialogModel || !has() {
		t.Fatal("the turn's end did not put the models in the open dialog")
	}
}

// drainPublished is drainSessionEvents once the session's outbox is
// committed. A reload's catalog delta is not published inline, as a turn's
// events are (drainSessionEvents' premise): the native session enqueues it
// (agent.EventLog's outbox, plan 021 §3.3), and the log's own goroutine puts
// it on the stream a moment after RefreshModels has answered — on one CPU,
// after a drain that found the channel empty. The engine's Sync is the barrier
// that waits for it: everything enqueued before the call is on the stream when
// it returns (the socket server's reply barrier, engine.SyncSeq). In the TUI
// the gate applies a refresh's answer before any event held behind it, in
// both transports, so the note is written first and the list follows: what
// this test waits for is the fold, not a different order.
func drainPublished(t *testing.T, m *Model, sess agent.Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := engineOf(t, *m).Sync(ctx); err != nil {
		t.Fatalf("sync the session's outbox: %v", err)
	}
	drainSessionEvents(t, m, sess)
}

// signInResultFixture is a finished sign-in with plan usage, for the redirect.
func signInResultFixture() chatgptauth.Result {
	return chatgptauth.Result{Email: signInEmail, PlanUsage: true, Registered: true}
}

// TestLiveSignInAndSignOutThroughARealSession (A17): a real native session on
// the shipped catalog — fireworks funded, the plan not — takes the plan's
// models up when a sign-in's list is written and the TUI asks, and drops them
// when the token file goes (what `craze auth logout` does: only that file
// changes) and /model opens. The sign-in's files are what chatgptauth writes
// (writeSignedInPlan); nothing reaches OpenAI.
func TestLiveSignInAndSignOutThroughARealSession(t *testing.T) {
	isolateSkillsHome(t)
	noOpenAIForLive(t)
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "FIREWORKS_API_KEY" {
			return "fw-live-dummy-key-0001"
		}
		return ""
	}
	ws := t.TempDir()
	sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir(), Interactive: true}, func(o *harness.Options) {
		o.Home, o.Getenv, o.NewModel = home, getenv, liveModel
		o.Now = func() time.Time { return time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC) }
	})
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	m := New(Config{Session: sess, Theme: "tokyo-night", Workspace: ws, Yolo: true, ProviderLocked: true, NativeDir: home, Getenv: getenv})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	tm, _ = tm.(Model).Update(startedMsg{})
	m = tm.(Model)
	if !m.canRefreshModels() {
		t.Fatal("a native session does not say it takes up models while it runs")
	}
	planListed := func() bool {
		return slices.ContainsFunc(m.snap.Models, func(md agent.ModelInfo) bool { return strings.Contains(md.Name, "(ChatGPT plan)") })
	}
	if planListed() {
		t.Fatal("fixture: the plan's models are listed before any sign-in")
	}

	// A sign-in: its files written, the TUI told the list is there.
	if _, err := writeSignedInPlan(home); err != nil {
		t.Fatal(err)
	}
	m = applyMsg(t, m, signInFinishedMsg{shownGen: m.shownGen, aliases: []string{"chatgpt/gpt-5.6-sol", "chatgpt/gpt-6-astra"}})
	drainPublished(t, &m, sess)
	if notes := texts(m, entryNote); notes[len(notes)-1] != "The plan's models are in /model now." {
		t.Fatalf("notes %q, want the plan's models in /model now", notes)
	}
	if !planListed() {
		t.Fatalf("the plan's models are not listed after the sign-in: %v", m.snap.Models)
	}

	// Signing out touches the token file alone; /model opening takes the
	// change up.
	if err := os.Remove(chatgptauth.TokenFile(home)); err != nil {
		t.Fatal(err)
	}
	m, _ = typeCommand(t, m, "/model")
	drainPublished(t, &m, sess)
	if planListed() {
		t.Fatalf("the plan's models are still listed after the sign-out: %v", m.snap.Models)
	}
}
