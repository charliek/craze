package tui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/transcript"
)

// The TUI on a restore (plan 027 §3.14, PR 4, C27): a session served elsewhere
// (Config.Backend), whose stream hands up a Restore, a Ready and an End beside
// the events.

// ------------------------------------------------------------ a real host

// attachHost is a real host, as internal/remote's own tests have one: the
// control server (internal/control) in front of an engine over a Stub with no
// primary — nothing in the host's process reads it — on a SHORT socket path,
// under /tmp itself (t.TempDir() and $TMPDIR overflow sun_path on macOS).
type attachHost struct {
	stub *Stub
	eng  *engine.Engine
	path string
}

// newAttachHost is a host, its engine started when start says so. It serves
// no session.stop — a TUI-hosted session's socket, or an older host's.
func newAttachHost(t *testing.T, start bool) *attachHost {
	t.Helper()
	return newAttachHostStopping(t, start, false)
}

// newAttachHostStopping is newAttachHost whose server, with stops, serves
// session.stop (plan 030 §3.6a) as craze serve does: the stop's sequence
// closes the engine off the handler's goroutine.
func newAttachHostStopping(t *testing.T, start, stops bool) *attachHost {
	t.Helper()
	if !stops {
		return newAttachHostStop(t, start, nil)
	}
	var h *attachHost
	h = newAttachHostStop(t, start, func(control.StopRequest) { go func() { _ = h.eng.Close() }() })
	return h
}

// newAttachHostStop is newAttachHost whose server serves session.stop with
// stop as its coordinator (nil: it serves none) — one that ends nothing, a
// host whose stop hangs, included.
func newAttachHostStop(t *testing.T, start bool, stop control.StopFunc) *attachHost {
	t.Helper()
	h := &attachHost{stub: NewStubNoPrimary()}
	var err error
	h.eng, err = engine.New(h.stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.eng.Close() })
	if start {
		if err := h.eng.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := os.MkdirTemp("/tmp", "czt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	h.path = filepath.Join(dir, "s")
	srv := control.New(control.Options{Workspace: "/work", Stop: stop})
	srv.SetEngine(h.eng)
	l, err := net.Listen("unix", h.path)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
		defer cancel()
		if err := srv.Close(ctx); err != nil {
			t.Errorf("server close: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return h
}

// attachSession is a remote.Session to h, attaching with when ("" waits for
// the host's start), closed when the test ends — before the host, whose
// cleanups were registered first.
func attachSession(t *testing.T, h *attachHost, when protocol.When) *remote.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	s, err := remote.DialSession(ctx, h.path, remote.SessionOptions{
		Client: remote.Options{Client: protocol.ClientInfo{Kind: "test", Name: "tui_test"}},
		When:   when,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// attachedModel is a TUI over s, sized and not started.
func attachedModel(t *testing.T, s *remote.Session) Model {
	t.Helper()
	isolateSkillsHome(t)
	m := New(Config{Backend: s, Theme: "tokyo-night", Workspace: frameWorkspace(t), Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return tm.(Model)
}

// nextStreamMsg lands the rig's outstanding read: whatever the stream hands
// up next, as its message.
func (r *gateRig) nextStreamMsg() tea.Msg {
	r.t.Helper()
	if len(r.reads) == 0 {
		r.t.Fatal("no read of the stream is outstanding")
	}
	cmd := r.reads[0]
	r.reads = r.reads[1:]
	return runWatched(r.t, cmd)
}

// landed is the rig's outstanding read landing with msg, a stream item the
// test built by hand: the read is spent, never run.
func (r *gateRig) landed() {
	r.t.Helper()
	if len(r.reads) == 0 {
		r.t.Fatal("no read of the stream is outstanding")
	}
	r.reads = r.reads[1:]
}

// namedCmds is every command cmd hands back whose function name contains
// part, a batch's members included — to run one the rig does not (a cancel).
func namedCmds(cmd tea.Cmd, part string) []tea.Cmd {
	if cmd == nil {
		return nil
	}
	fn := cmdFuncName(cmd)
	if strings.HasPrefix(fn, teaPkg+"compactCmds") || strings.HasPrefix(fn, teaPkg+"Batch") {
		var out []tea.Cmd
		if b, ok := cmd().(tea.BatchMsg); ok {
			for _, c := range b {
				out = append(out, namedCmds(c, part)...)
			}
		}
		return out
	}
	if strings.Contains(fn, part) {
		return []tea.Cmd{cmd}
	}
	return nil
}

// waitHost polls the host until cond holds, or fails the test.
func waitHost(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(pumpWatchdog)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("the host never reached %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAttachMidTurnWithAnOpenQuestionCanAnswerAtOnce (§3.14, astra 16): a TUI
// attached over the socket while a turn is running and a question is open is
// initialised whole from the first attach's snapshot — the card, the working
// status, the turn a cancel is asked against — so it answers the question at
// once and cancels the turn at once, with no event of the stream applied in
// between: the answer and the cancel both reach the host. In both gate modes.
func TestAttachMidTurnWithAnOpenQuestionCanAnswerAtOnce(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "gateSync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			h := newAttachHost(t, true)
			open := h.stub.HangNext()
			other := engine.Command{Client: h.eng.NewClientID(), ID: "1"}
			if _, err := h.eng.Submit(other, "go", engine.SubmitQueue, ""); err != nil {
				t.Fatal(err)
			}
			awaitBarrier(t, open, "the host's turn opening")
			h.stub.Emit(agent.Event{Type: agent.EventQuestion, Question: oneQuestion("ask-1")})
			turn := h.eng.State().Turn
			if turn == "" || len(h.eng.Asks()) != 1 {
				t.Fatalf("fixture: the host's turn %q, its asks %+v", turn, h.eng.Asks())
			}

			m := attachedModel(t, attachSession(t, h, ""))
			if async {
				m = asyncGate(t, m)
			}
			r := newGateRig(t, m)
			started := runWatched(t, r.m.startCmd())
			if _, ok := started.(startedMsg); !ok {
				t.Fatalf("the start answered %#v", started)
			}
			first := r.nextStreamMsg()
			restore, ok := first.(restoreMsg)
			if !ok {
				t.Fatalf("the stream's first item is %#v, want the attach's restore", first)
			}
			// The restore first and the start after it: the session coming up
			// must not end the turn the snapshot says is running.
			r.send(restore)
			r.send(started)
			if r.m.status != statusWorking || r.m.turnID != turn {
				t.Fatalf("attached mid-turn, the model is %s on turn %q, want working on %q", r.m.status, r.m.turnID, turn)
			}
			if c, ok := r.m.headCard(); !ok || c.ask == nil || c.ask.ID != "ask-1" {
				t.Fatalf("no card for the open question: %+v", r.m.cards)
			}
			if v := plainView(r.m); !strings.Contains(v, "Pick one") {
				t.Fatalf("the question is not on screen:\n%s", v)
			}
			seq := r.m.foldedSeq()

			// The answer, at once.
			r.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
			if async {
				r.answer()
			}
			if r.m.cardOpen() {
				t.Fatalf("the card is still up after its answer: %+v", r.m.cards)
			}
			waitHost(t, "the answer", func() bool { return len(h.eng.Asks()) == 0 })
			if calls := h.stub.Calls(); len(calls) != 1 || calls[0].ID != "ask-1" {
				t.Fatalf("the host's answers: %+v", calls)
			}

			// The cancel, at once: Esc names the turn the snapshot said runs.
			tm, cmd := r.m.gated(tea.KeyMsg{Type: tea.KeyEsc}, Model.update)
			r.m = tm.(Model)
			cancels := namedCmds(cmd, "cancelTurn")
			if len(cancels) != 1 {
				t.Fatalf("Esc sent %d cancels, want one", len(cancels))
			}
			if msg := runWatched(t, cancels[0]); msg != nil {
				t.Fatalf("the cancel answered %#v", msg)
			}
			waitHost(t, "the cancelled turn's end", func() bool { return h.eng.State().Turn == "" })
			if h.stub.CancelsSent() != 1 {
				t.Fatalf("the host's session saw %d cancels", h.stub.CancelsSent())
			}
			if got := r.m.foldedSeq(); got != seq {
				t.Fatalf("the model folded up to %d after the restore's %d: an event was applied in between", got, seq)
			}
		})
	}
}

// restoreOf is a restore of snap as m's stream would hand it: the item's
// facts are the ones m's backend holds, as the snapshot's incarnation's.
func restoreOf(m Model, snap *transcript.Snapshot, gen uint64) restoreMsg {
	info := m.eng.Info()
	info.Incarnation = snap.Incarnation
	return restoreMsg{info: info, snap: snap, gen: gen}
}

// ------------------------------------------------------ restores by hand

// restoreAt is the time every hand-built stream event carries.
var restoreAt = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// seqd is evs numbered from first, each stamped at restoreAt plus its seq.
func seqd(first uint64, evs ...agent.Event) []agent.Event {
	out := make([]agent.Event, len(evs))
	for i, ev := range evs {
		ev.Seq = first + uint64(i)
		if ev.At.IsZero() {
			ev.At = restoreAt.Add(time.Duration(ev.Seq) * time.Second)
		}
		out[i] = ev
	}
	return out
}

// snapshotFolded is a snapshot of incarnation inc with evs folded — what a host
// whose log carried them would hand an attach.
func snapshotFolded(t *testing.T, inc string, evs ...agent.Event) *transcript.Snapshot {
	t.Helper()
	sm := transcript.New(transcript.Options{Incarnation: inc})
	for _, ev := range evs {
		sm.Fold(ev)
	}
	s, err := sm.Snapshot(0)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return s
}

// restoredWith is m after the restore of a snapshot of incarnation inc with
// evs folded, as the stream's generation gen.
func restoredWith(t *testing.T, m Model, inc string, gen uint64, evs ...agent.Event) Model {
	t.Helper()
	return deliver(t, m, restoreOf(m, snapshotFolded(t, inc, evs...), gen))
}

// rowKindNames names the row kinds mainRows spells.
var rowKindNames = map[entryKind]string{
	entryUser: "user", entryAssistant: "assistant", entryThought: "thought", entryTool: "tool",
	entryNote: "note", entryPlan: "plan", entryError: "error",
}

// mainRows is the main pane's rows, each as "kind:text", local rows marked.
func mainRows(m Model) []string {
	var out []string
	for _, r := range m.main.rows {
		s := rowKindNames[r.kind] + ":" + r.text
		if r.local {
			s = "local " + s
		}
		out = append(out, s)
	}
	return out
}

// TestARestoreDuringAHoldFoldsNothingTwice (§3.14, CodeRabbit 6): a restore
// that arrives while a gate is open waits its turn in the held queue, in
// arrival order — and, as it is held, the events of the stream it replaces
// that were held before it are dropped: its snapshot holds them already, so
// they are never applied (a hidden ask they open is not answered again). The
// keys around it stay, in order; the reply runs its continuation as usual; the
// drain applies the restore — which drops the continuation's own row with every
// local one — and then the new stream's events. Every entry is shown once.
func TestARestoreDuringAHoldFoldsNothingTwice(t *testing.T) {
	m, _ := hiddenAsksModel(t)
	m = asyncGate(t, m)
	hello := seqd(1, agent.Event{Type: agent.EventText, Text: "hello"})
	m = restoredWith(t, m, "inc-1", 1, hello...)

	r := newGateRig(t, m)
	hidden := 0
	send := func(msg tea.Msg) {
		t.Helper()
		tm, cmd := r.m.gated(msg, r.handle)
		r.m = tm.(Model)
		hidden += len(hiddenAnswerCmds(cmd))
		r.sort(cmd)
	}
	release := make(chan struct{})
	send(gateOpMsg{call: blockedCall(release, "done"), cont: noteCont("gate")})
	if r.m.gate == nil {
		t.Fatal("fixture: no gate open")
	}

	// Held: two events of generation 1, a key, the restore of generation 2 —
	// whose snapshot holds both events — a key, and an event of generation 2.
	old := seqd(2,
		agent.Event{Type: agent.EventText, Text: " world"},
		agent.Event{Type: agent.EventQuestion, Question: oneQuestion("ask-9")},
	)
	stream := func(msg tea.Msg) {
		t.Helper()
		r.landed()
		send(msg)
	}
	for _, ev := range old {
		stream(eventMsg{ev: ev, gen: 1})
	}
	send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	snap := snapshotFolded(t, "inc-1", append(append([]agent.Event(nil), hello...), old...)...)
	stream(restoreOf(r.m, snap, 2))
	if got, want := heldKinds(r.m), []string{"tea.KeyMsg", "tui.restoreMsg"}; !slices.Equal(got, want) {
		t.Fatalf("held behind the gate: %v, want %v — the replaced stream's events dropped as the restore was held", got, want)
	}
	send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	later := seqd(4, agent.Event{Type: agent.EventText, Text: "!"})
	stream(eventMsg{ev: later[0], gen: 2})

	close(release)
	if rep := r.answer(); rep.result != "done" {
		t.Fatalf("the gate answered %+v", rep)
	}
	for r.drains > 0 {
		r.drains--
		send(drainMsg{})
	}
	if len(r.m.held) != 0 || r.m.gate != nil {
		t.Fatalf("still held: %v", heldKinds(r.m))
	}
	if hidden != 0 {
		t.Fatalf("%d hidden answers were sent for events the restore's snapshot already held", hidden)
	}
	if got := r.m.input.Value(); got != "xy" {
		t.Fatalf("the keys landed as %q, want \"xy\": in order, around the restore", got)
	}
	// The snapshot's entry — its run closed by the question's opening — shown
	// once, the reload noted under it, then the new stream's chunk as the
	// entry after it; the continuation's own row is gone with the stream it
	// was about.
	if got, want := mainRows(r.m), []string{"assistant:hello world", "local note:" + reloadedNote, "assistant:!"}; !slices.Equal(got, want) {
		t.Fatalf("the main pane: %q, want %q", got, want)
	}
	if got := r.m.foldedSeq(); got != 4 {
		t.Fatalf("the model folded up to %d, want 4", got)
	}
}

// TestARestoreClearsOverlaysAndEchoMarkers (§3.14): nothing this client laid
// over the stream a restore replaces survives it — the settings and result
// overlays, the echo markers (ownTurn, nextTurn, armedDraft, disarmed,
// askEchoes), the cancel mask, the plan offer, the local rows — and the
// revision guards stand at the snapshot's seq.
func TestARestoreClearsOverlaysAndEchoMarkers(t *testing.T) {
	m := sized(t)
	m.requestMode(1, "c-1", "plan")
	m.noteResult(resultEntry{kind: resultTitle, cause: "c-2", title: "mine"})
	m.noteResult(resultEntry{kind: resultArmed, cause: "c-3"})
	m.ownTurn, m.nextTurn = "t-own", "t-next"
	m.armedDraft, m.disarmed = "c-3", "c-4"
	m.askEchoes = []string{"c-5"}
	m.cardMask, m.cardMasking = "t-own", true
	m.planOfferSeq = m.turnSeq
	m.addNote("a note of this client's own")
	if !m.ov.mode.set || len(m.ov.results) != 2 {
		t.Fatalf("fixture: the overlays %+v", m.ov)
	}

	evs := seqd(1,
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "t-1", Phase: agent.TurnStarted, Text: "go", Origin: agent.TurnOriginSubmit}},
		agent.Event{Type: agent.EventText, Text: "done"},
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "t-1", Phase: agent.TurnEnded}},
	)
	m = restoredWith(t, m, "inc-1", 1, evs...)
	noOverlays(t, m, "after the restore")
	if m.ownTurn != "" || m.nextTurn != "" || m.armedDraft != "" || m.disarmed != "" || len(m.askEchoes) != 0 {
		t.Fatalf("echo markers survived the restore: own %q next %q armed %q disarmed %q echoes %v",
			m.ownTurn, m.nextTurn, m.armedDraft, m.disarmed, m.askEchoes)
	}
	if m.cardMasking || m.cardMask != "" || m.planArmed() {
		t.Fatalf("the cancel mask (%v, %q) or the plan offer (%v) survived the restore", m.cardMasking, m.cardMask, m.planArmed())
	}
	if m.modeRev != 3 || m.modelRev != 3 || m.configRev != 3 {
		t.Fatalf("the revision guards stand at %d/%d/%d, want the snapshot's 3", m.modeRev, m.modelRev, m.configRev)
	}
	if m.sendNowPending() || m.snap.Title == "mine" {
		t.Fatalf("a result overlay still shows: armed %v, title %q", m.sendNowPending(), m.snap.Title)
	}
	for _, row := range m.main.rows {
		if row.local {
			t.Fatalf("a local row survived the restore: %+v", shownOf(row))
		}
	}
}

// TestARestoreRaisesEveryOpenAsksCard (§3.14, astra 16): a card for every open
// ask the snapshot holds, in the order they opened, each as its opening would
// have raised it — a permission always, a question or a plan unless automatic
// or hidden by the session's capabilities — a truncated one saying so; and
// the UI makes way for them, as for an arriving card. A hidden kind raises no
// card and sends no answer.
func TestARestoreRaisesEveryOpenAsksCard(t *testing.T) {
	perm := &agent.PermissionEvent{ID: "perm-1", Tool: "Shell", Options: []agent.PermissionOption{
		{OptionID: "allow", Name: strings.Repeat("x", transcript.ItemCap+1), Kind: kindAllowOnce},
	}}
	auto := oneQuestion("auto-1")
	auto.Auto = true
	plan := &agent.PlanEvent{ID: "plan-1", Name: "the plan", Plan: "step"}
	evs := seqd(1,
		agent.Event{Type: agent.EventPermission, Permission: perm},
		agent.Event{Type: agent.EventQuestion, Question: auto},
		agent.Event{Type: agent.EventQuestion, Question: oneQuestion("ask-1")},
		agent.Event{Type: agent.EventPlan, Plan: plan},
	)
	ids := func(cards []card) []string {
		var out []string
		for _, c := range cards {
			out = append(out, cardAskID(c))
		}
		return out
	}

	t.Run("shown", func(t *testing.T) {
		m := sized(t).openThemePicker()
		if m.dialog == dialogNone || !m.showAsk() || !m.showPlan() {
			t.Fatal("fixture: no dialog up, or a provider that hides questions or plans")
		}
		m = restoredWith(t, m, "inc-1", 1, evs...)
		if got, want := ids(m.cards), []string{"perm-1", "ask-1", "plan-1"}; !slices.Equal(got, want) {
			t.Fatalf("the cards %v, want %v: every open ask's, in the order they opened", got, want)
		}
		if !m.cards[0].truncated || m.cards[1].truncated || m.cards[2].truncated {
			t.Fatalf("the truncation marks %v %v %v, want the permission's alone", m.cards[0].truncated, m.cards[1].truncated, m.cards[2].truncated)
		}
		if v := plainView(m); !strings.Contains(v, "permission Shell  "+truncatedTag) {
			t.Fatalf("the truncated permission does not say so:\n%s", v)
		}
		if m.dialog != dialogNone {
			t.Fatal("the dialog stayed up under the cards")
		}
	})
	t.Run("hidden", func(t *testing.T) {
		m, _ := hiddenAsksModel(t)
		tm, cmd := m.Update(restoreOf(m, snapshotFolded(t, "inc-1", evs...), 1))
		m = tm.(Model)
		if got, want := ids(m.cards), []string{"perm-1"}; !slices.Equal(got, want) {
			t.Fatalf("the cards %v, want %v: a hidden kind raises none", got, want)
		}
		if n := len(hiddenAnswerCmds(cmd)); n != 0 {
			t.Fatalf("the restore answered %d hidden asks", n)
		}
	})
}

// TestARestoreFromAnotherIncarnationMovesTheGeneration (§3.12, astra r2 13):
// a restore of another incarnation — the host restarted, the engine replaced
// — is another session: the session generation moves, and a result issued for
// the old one (a settings reply above all) is dropped where it lands. The
// first restore, and one of the same incarnation, keep it: their results are
// this session's. A start's answer is its backend's, not a session's, and is
// never dropped for a restore (ownStart).
func TestARestoreFromAnotherIncarnationMovesTheGeneration(t *testing.T) {
	type issuedResult struct {
		m   Model
		msg tea.Msg
	}
	issuedBy := func(tm tea.Model, cmd tea.Cmd) issuedResult { return issuedResult{tm.(Model), runCmd(cmd)} }
	for _, tc := range []struct {
		name  string
		issue func(t *testing.T) issuedResult
		// kept says the result survives another incarnation's restore.
		kept bool
	}{
		{name: "a mode change's answer (modeAppliedMsg)", issue: func(t *testing.T) issuedResult {
			return issuedBy(sized(t).applyMode("plan"))
		}},
		{name: "/model refused (revertModelMsg)", issue: func(t *testing.T) issuedResult {
			m, stub := perModelStub(t)
			stub.FailNextSetModel()
			m.input.SetValue("/model fast")
			return issuedBy(m.Update(enter()))
		}},
		{name: "a failed cancel (cancelFailedMsg)", issue: func(t *testing.T) issuedResult {
			m, sess := scriptedModel(t)
			m = startScripted(t, m, sess, "go", scriptHeld())
			sess.FailNextCancel(errors.New("the cancel never reached the agent"))
			return issuedBy(m.cancelTurn())
		}},
		{name: "a start's answer (startedMsg)", kept: true, issue: func(t *testing.T) issuedResult {
			isolateSkillsHome(t)
			stub := NewStub()
			t.Cleanup(func() { _ = stub.Close() })
			m := New(Config{Session: stub, Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true})
			return issuedResult{m, runCmd(m.startCmd())}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.issue(t)
			if res.msg == nil {
				t.Fatal("the site's command answered nothing")
			}
			gen := res.m.sessGen
			// restored is res.m after a restore of its own fold, labelled inc.
			restored := func(m Model, inc string) Model {
				s, err := m.shared.Snapshot(0)
				if err != nil {
					t.Fatal(err)
				}
				s.Incarnation = inc
				tm, _ := m.update(restoreOf(m, s, 1))
				return tm.(Model)
			}
			applies := func(m Model) bool {
				before := digestModel(&m)
				tm, _ := m.update(res.msg)
				after := tm.(Model)
				return len(before.diff(digestModel(&after))) != 0
			}

			// The first restore, then one of the same incarnation: the
			// generation stands, and the result is applied.
			same := restored(restored(res.m, "inc-a"), "inc-a")
			if same.sessGen != gen || same.outdated(res.msg) {
				t.Fatalf("a restore of the same session moved the generation %d → %d", gen, same.sessGen)
			}
			if !applies(same) {
				t.Fatalf("%T changes nothing on its own session after a same-incarnation restore", res.msg)
			}

			// Another incarnation: the generation moves.
			moved := restored(restored(res.m, "inc-a"), "inc-b")
			if moved.sessGen == gen {
				t.Fatal("another incarnation's restore did not move the generation")
			}
			if got := applies(moved); got != tc.kept {
				t.Fatalf("%T applied %v after another incarnation's restore, want %v", res.msg, got, tc.kept)
			}
		})
	}
}

// TestTheFirstRestoreIsInvisible (§3.14, §3.16): a TUI attached before its
// session started — as the socket goldens attach, when: "now" — is handed the
// first attach's snapshot of an empty session. Applying it changes no frame
// and nothing a frame or the parity watch reads: the model after it is the
// model that never restored, but for the incarnation its fold now knows.
func TestTheFirstRestoreIsInvisible(t *testing.T) {
	h := newAttachHost(t, false)
	s := attachSession(t, h, protocol.WhenNow)
	m := attachedModel(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	if err := s.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	r := newGateRig(t, m)
	restore, ok := r.nextStreamMsg().(restoreMsg)
	if !ok || restore.snap.Seq != 0 {
		t.Fatalf("the first item is not an empty session's restore: %+v", restore)
	}
	// The same model one Update on, with and without the restore; the facts
	// the attach reply brought are in the backend's Info for both.
	control := deliver(t, r.m, refreshSnapMsg{})
	restored := deliver(t, r.m, restore)
	if a, b := plainView(control), plainView(restored); a != b {
		t.Fatalf("the first restore moved the frame\n--- without\n%s\n--- with\n%s", a, b)
	}
	if got := digestModel(&control).diff(digestModel(&restored)); len(got) != 0 && !slices.Equal(got, []string{"shared"}) {
		t.Fatalf("the first restore moved %v", got)
	}
	cs, rs := control.shared, restored.shared
	if cs.Seq() != rs.Seq() || !reflect.DeepEqual(cs.State(), rs.State()) || !reflect.DeepEqual(cs.History(), rs.History()) ||
		!reflect.DeepEqual(cs.EndedAsks(), rs.EndedAsks()) || !slices.Equal(cs.Subs(), rs.Subs()) {
		t.Fatal("the restored fold is not the empty one it replaced")
	}
	if cs.Incarnation() != "" || rs.Incarnation() != h.eng.State().Incarnation {
		t.Fatalf("the incarnations %q and %q, want none and the host's", cs.Incarnation(), rs.Incarnation())
	}
	if restored.sessGen != control.sessGen || len(restored.main.rows) != 0 {
		t.Fatal("the first restore moved the generation or drew a row")
	}
}

// TestALaterRestoreNotesTranscriptReloaded (§3.14): every restore after the
// first says so — once, a local row under the restored transcript.
func TestALaterRestoreNotesTranscriptReloaded(t *testing.T) {
	evs := seqd(1, agent.Event{Type: agent.EventText, Text: "hello"})
	m := restoredWith(t, sized(t), "inc-1", 1, evs...)
	if got, want := mainRows(m), []string{"assistant:hello"}; !slices.Equal(got, want) {
		t.Fatalf("after the first restore: %q, want %q", got, want)
	}
	m = restoredWith(t, m, "inc-1", 2, evs...)
	if got, want := mainRows(m), []string{"assistant:hello", "local note:" + reloadedNote}; !slices.Equal(got, want) {
		t.Fatalf("after a later restore: %q, want %q", got, want)
	}
	m = restoredWith(t, m, "inc-2", 3, evs...)
	if got, want := mainRows(m), []string{"assistant:hello", "local note:" + reloadedNote}; !slices.Equal(got, want) {
		t.Fatalf("after another incarnation's: %q, want %q", got, want)
	}
}

// TestEndQuits (§3.14): the stream's End quits the program, and the final model
// says why — nothing for the session's own end, the transport's failure
// otherwise; no read of the stream follows it. Held behind a gate, it waits its
// turn like any other message.
func TestEndQuits(t *testing.T) {
	for _, why := range []error{nil, errors.New("the host went away")} {
		m := sized(t)
		tm, cmd := m.Update(endMsg{err: why})
		m = tm.(Model)
		if _, ok := runCmd(cmd).(tea.QuitMsg); !ok {
			t.Fatalf("an End (%v) did not quit", why)
		}
		if !m.ended || !errors.Is(m.endErr, why) || (why == nil) != (m.endErr == nil) {
			t.Fatalf("the final model says ended=%v err=%v, want true and %v", m.ended, m.endErr, why)
		}
		if read := m.readOn(); read != nil {
			t.Fatal("the stream is read again after its End")
		}
	}

	m, _ := gatedModel(t)
	r := newGateRig(t, m)
	release := make(chan struct{})
	r.send(gateOpMsg{call: blockedCall(release, nil), cont: noteCont("gate")})
	r.landed()
	r.send(endMsg{})
	if r.m.ended || !slices.Equal(heldKinds(r.m), []string{"tui.endMsg"}) {
		t.Fatalf("the End was not held behind the gate: ended=%v held %v", r.m.ended, heldKinds(r.m))
	}
	close(release)
	r.answer()
	r.drainAll()
	if !r.m.ended {
		t.Fatal("the End was never applied")
	}
}

// TestTheReaderDeliversEveryItem (§3.14): the reader hands up every item the
// stream carries, each as its message — an event with its generation, a
// restore, a ready, the end — and a stream that ended or failed as nothing.
func TestTheReaderDeliversEveryItem(t *testing.T) {
	ev := agent.Event{Type: agent.EventText, Text: "hello"}
	snap := snapshotFolded(t, "inc-1")
	info := backend.SessionInfo{Incarnation: "inc-1", Provider: "grok"}
	for _, tc := range []struct {
		name string
		item any
		want tea.Msg
	}{
		{"an event", backend.Item{Kind: backend.ItemEvent, Event: ev, Gen: 3}, eventMsg{ev: ev, gen: 3}},
		{"a restore", backend.Item{Kind: backend.ItemRestore, Info: info, Snapshot: snap, Gen: 2}, restoreMsg{info: info, snap: snap, gen: 2}},
		{"a ready", backend.Item{Kind: backend.ItemReady, Info: info}, readyMsg{info: info}},
		{"the end", backend.Item{Kind: backend.ItemEnd}, endMsg{}},
		{"a closed stream", backend.ErrClosed, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBackend{read: func(context.Context) (backend.Item, error) {
				if err, ok := tc.item.(error); ok {
					return backend.Item{}, err
				}
				return tc.item.(backend.Item), nil
			}}
			if got := waitEvent(b)(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the reader answered %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestARestoreIsChargedInTheHold (§3.12, X35 2): a restore held behind a gate
// is charged its snapshot's bytes, as an event its text's.
func TestARestoreIsChargedInTheHold(t *testing.T) {
	// Under the stream cap, which a streamed entry's text is held to.
	big := strings.Repeat("x", 60<<10)
	snap := snapshotFolded(t, "inc-1", seqd(1, agent.Event{Type: agent.EventText, Text: big})...)
	if got := payloadBytes(restoreMsg{snap: snap, gen: 1}); got < len(big) {
		t.Fatalf("a restore of %d bytes of text is charged %d", len(big), got)
	}
}

// TestAReadyRedrawsFromInfo (§3.14): the host's start completing hands up a
// Ready whose facts the backend already holds; the mirror reads them again.
func TestAReadyRedrawsFromInfo(t *testing.T) {
	h := newAttachHost(t, false)
	s := attachSession(t, h, protocol.WhenNow)
	m := attachedModel(t, s)
	r := newGateRig(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	if err := s.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	r.send(r.nextStreamMsg())
	if len(r.m.snap.Models) != 0 {
		t.Fatalf("fixture: the catalogs %+v before the start", r.m.snap.Models)
	}
	if err := h.eng.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for {
		msg := r.nextStreamMsg()
		r.send(msg)
		if _, ok := msg.(readyMsg); ok {
			break
		}
	}
	if len(r.m.snap.Models) == 0 {
		t.Fatal("the mirror did not read the catalogs the ready brought")
	}
}

// ------------------------------------------------------ outcome unknown

// unknownOutcomes is the in-process backend with the commands a test names
// answered as a socket answers one after a resume loss: outcome unknown
// (remote.OutcomeUnknownError, reason resume_lost, over the stale epoch it was
// refused for) — and not sent. It names its engine, so a test reaches it as it
// reaches any in-process model's (engineBehind).
type unknownOutcomes struct {
	backend.Backend
	eng  *engine.Engine
	mu   sync.Mutex
	fail map[string]bool
}

func (u *unknownOutcomes) engine() *engine.Engine { return u.eng }

// failing makes method's next calls answer outcome-unknown.
func (u *unknownOutcomes) failing(method string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.fail[method] = true
}

func (u *unknownOutcomes) unknown(method string, c engine.Command) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.fail[method] {
		return nil
	}
	return &remote.OutcomeUnknownError{Method: method, CommandID: c.ID, Reason: protocol.ReasonResumeLost, Err: backend.ErrStaleEpoch}
}

func (u *unknownOutcomes) Submit(ctx context.Context, c engine.Command, text string, mode engine.SubmitMode, from string) (engine.SubmitResult, error) {
	if err := u.unknown(protocol.MethodSessionPrompt, c); err != nil {
		return engine.SubmitResult{}, err
	}
	return u.Backend.Submit(ctx, c, text, mode, from)
}

func (u *unknownOutcomes) Answer(ctx context.Context, c engine.Command, id string, a agent.AskAnswer) error {
	if err := u.unknown("asks.answer", c); err != nil {
		return err
	}
	return u.Backend.Answer(ctx, c, id, a)
}

func (u *unknownOutcomes) Unqueue(ctx context.Context, c engine.Command, id string) (agent.QueuedPrompt, error) {
	if err := u.unknown("queue.remove", c); err != nil {
		return agent.QueuedPrompt{}, err
	}
	return u.Backend.Unqueue(ctx, c, id)
}

func (u *unknownOutcomes) SetTitle(ctx context.Context, c engine.Command, title string) error {
	if err := u.unknown("session.rename", c); err != nil {
		return err
	}
	return u.Backend.SetTitle(ctx, c, title)
}

func (u *unknownOutcomes) Cancel(ctx context.Context, c engine.Command, turn string) (engine.CancelResult, error) {
	if err := u.unknown("session.cancel", c); err != nil {
		return engine.CancelResult{}, err
	}
	return u.Backend.Cancel(ctx, c, turn)
}

func (u *unknownOutcomes) Set(ctx context.Context, c engine.Command, s engine.Setting) (engine.SetResult, error) {
	if err := u.unknown("session.set", c); err != nil {
		return engine.SetResult{}, err
	}
	return u.Backend.Set(ctx, c, s)
}

// unknownOutcomeModel is a started model over a Stub whose backend is
// Config.Backend's unknownOutcomes.
func unknownOutcomeModel(t *testing.T) (Model, *Stub, *unknownOutcomes) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	eng, err := engine.New(stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	ws := t.TempDir()
	u := &unknownOutcomes{Backend: newEngineBackend(eng, ws), eng: eng, fail: map[string]bool{}}
	m := New(Config{Backend: u, Theme: "tokyo-night", Workspace: ws, Model: "grok", Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return startedLikeInit(t, tm.(Model)), stub, u
}

// hasLocalRow reports whether the main pane holds a local row of kind and
// text.
func hasLocalRow(m Model, kind entryKind, text string) bool {
	return slices.ContainsFunc(m.main.rows, func(r *entry) bool { return r.local && r.kind == kind && r.text == text })
}

// TestAnOutcomeUnknownIsNoAnswer (PR 4, C27): a command whose outcome the
// backend says is unknown (backend.ErrOutcomeUnknown — a socket's resume loss)
// is, to every continuation, a call that did not answer (ErrNoAnswer): decided
// once, at the gate, so each family says what it says for an unanswered call —
// Submit keeps the draft, a card's answer keeps its card, a queue verb and
// /rename say what may have happened. A fire-and-forget command (a cancel) and
// a settings chain word it the same: "no answer from the session".
func TestAnOutcomeUnknownIsNoAnswer(t *testing.T) {
	t.Run("Submit", func(t *testing.T) {
		for _, async := range []bool{false, true} {
			m, _, u := unknownOutcomeModel(t)
			u.failing(protocol.MethodSessionPrompt)
			if async {
				m = asyncGate(t, m)
				m = pumpEnter(t, m, "hello")
				m = pumpDrained(t, m)
			} else {
				m.input.SetValue("hello")
				m = deliver(t, m, enter())
			}
			if m.copyNote != noAnswerSubmitNote || m.input.Value() != "hello" || m.status == statusWorking {
				t.Fatalf("async=%v: note %q, draft %q, status %s — want the unanswered Submit's note with the draft kept",
					async, m.copyNote, m.input.Value(), m.status)
			}
		}
	})
	t.Run("a card's answer", func(t *testing.T) {
		m, stub, u := unknownOutcomeModel(t)
		stub.Emit(agent.Event{Type: agent.EventQuestion, Question: oneQuestion("ask-1")})
		m = applyPending(t, m)
		if !m.cardOpen() {
			t.Fatal("fixture: no card")
		}
		u.failing("asks.answer")
		m = deliver(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
		if c, ok := m.headCard(); !ok || c.ask == nil || c.ask.ID != "ask-1" || !hasLocalRow(m, entryNote, noAnswerCardNote) {
			t.Fatalf("the card %+v, the rows %q — want the card kept and the unanswered answer's note", m.cards, mainRows(m))
		}
	})
	t.Run("a queue verb", func(t *testing.T) {
		m, stub, u := unknownOutcomeModel(t)
		open := stub.HangNext()
		m.input.SetValue("first")
		m = deliver(t, m, enter())
		awaitBarrier(t, open, "the turn opening")
		m.input.SetValue("second")
		m = deliver(t, m, enter())
		m = applyPending(t, m)
		if len(m.queue) != 1 {
			t.Fatalf("fixture: the queue %+v", m.queue)
		}
		u.failing("queue.remove")
		m, _ = m.dropQueuedRow(m.queue[0].ID, linkDone)
		if m.copyNote != noAnswerRowNote || len(m.queue) != 1 {
			t.Fatalf("note %q, queue %+v — want the unanswered verb's note over the fold's band", m.copyNote, m.queue)
		}
	})
	t.Run("/rename", func(t *testing.T) {
		m, _, u := unknownOutcomeModel(t)
		u.failing("session.rename")
		m.input.SetValue("/rename a better title")
		m = deliver(t, m, enter())
		if !hasLocalRow(m, entryNote, noAnswerRenameNote) {
			t.Fatalf("the rows %q, want the unanswered rename's note", mainRows(m))
		}
	})
	t.Run("a cancel", func(t *testing.T) {
		m, stub, u := unknownOutcomeModel(t)
		open := stub.HangNext()
		m.input.SetValue("go")
		m = deliver(t, m, enter())
		awaitBarrier(t, open, "the turn opening")
		u.failing("session.cancel")
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		m = tm.(Model)
		cancels := namedCmds(cmd, "cancelTurn")
		if len(cancels) != 1 {
			t.Fatalf("Esc sent %d cancels", len(cancels))
		}
		m = deliver(t, m, cancels[0]())
		if !hasLocalRow(m, entryError, ErrNoAnswer.Error()) {
			t.Fatalf("the rows %q, want the cancel's failure as no answer", mainRows(m))
		}
	})
	t.Run("a settings chain", func(t *testing.T) {
		m, _, u := unknownOutcomeModel(t)
		u.failing("session.set")
		tm, cmd := m.applyMode("plan")
		m = deliver(t, tm.(Model), runCmd(cmd))
		if !hasLocalRow(m, entryError, ErrNoAnswer.Error()) {
			t.Fatalf("the rows %q, want the mode change's failure as no answer", mainRows(m))
		}
	})
}

// ------------------------------------------------------------ C27a

// queuedRow is a queue event that queues row id with text.
func queuedRow(id, text string) agent.Event {
	return agent.Event{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: id, Text: text}, QueueChange: agent.QueueQueued}
}

// TestAQueueEditAcrossARestore (C27a, astra r63 1): queue ids start again at
// q-1 in every incarnation, so an edit begun on incarnation A's q-1 ends when a
// restore brings another incarnation — whatever its queue holds under that id
// — as an edit whose row left the queue ends: the draft back, the note said.
// On the same incarnation the edit stands while its row does, and ends when it
// does not (syncQueue).
func TestAQueueEditAcrossARestore(t *testing.T) {
	editing := func(t *testing.T) Model {
		t.Helper()
		m := restoredWith(t, sized(t), "inc-a", 1, seqd(1, queuedRow("q-1", "A's row"))...)
		if len(m.queue) != 1 || m.queue[0].ID != "q-1" {
			t.Fatalf("fixture: the queue %+v", m.queue)
		}
		m.input.SetValue("my draft")
		m.startQueueEdit(m.queue[0])
		m.input.SetValue("edited")
		return m
	}
	t.Run("another incarnation", func(t *testing.T) {
		m := restoredWith(t, editing(t), "inc-b", 2, seqd(1, queuedRow("q-1", "B's row"))...)
		if m.queueEdit != "" || m.input.Value() != "my draft" || m.copyNote != editGoneNote {
			t.Fatalf("editing %q, composer %q, note %q — want the edit ended, the draft back and %q",
				m.queueEdit, m.input.Value(), m.copyNote, editGoneNote)
		}
	})
	t.Run("the same incarnation, the row still queued", func(t *testing.T) {
		m := restoredWith(t, editing(t), "inc-a", 2, seqd(1, queuedRow("q-1", "A's row"))...)
		if m.queueEdit != "q-1" || m.input.Value() != "edited" {
			t.Fatalf("editing %q, composer %q — want the edit standing", m.queueEdit, m.input.Value())
		}
	})
	t.Run("the same incarnation, the row gone", func(t *testing.T) {
		m := restoredWith(t, editing(t), "inc-a", 2, seqd(1, agent.Event{Type: agent.EventText, Text: "hi"})...)
		if m.queueEdit != "" || m.input.Value() != "my draft" || m.copyNote != editGoneNote {
			t.Fatalf("editing %q, composer %q, note %q — want the edit ended", m.queueEdit, m.input.Value(), m.copyNote)
		}
	})
	// C27b: the row survived with a version another client moved it to. The
	// edit stands, and its save is the check-and-edit it always was (C22):
	// refused stale_version, the text kept, the version refreshed to the row's
	// as the restored band shows it — the next Enter saves over it knowingly.
	t.Run("the same incarnation, the row changed", func(t *testing.T) {
		isolateSkillsHome(t)
		stub := NewStub()
		t.Cleanup(func() { _ = stub.Close() })
		m := startStub(t, stub, t.TempDir(), 80, 24)
		eng := engineOf(t, m)
		open := stub.HangNext()
		m.input.SetValue("go")
		m = deliver(t, m, enter())
		awaitBarrier(t, open, "the turn opening")
		m.input.SetValue("queued")
		m = deliver(t, m, enter())
		// The host's own snapshot of itself: this session, as a restore
		// carries it.
		hostRestore := func(m Model, gen uint64) restoreMsg {
			t.Helper()
			if _, err := eng.SyncSeq(context.Background()); err != nil {
				t.Fatal(err)
			}
			snap, err := eng.TranscriptSnapshot("", 0)
			if err != nil {
				t.Fatal(err)
			}
			return restoreOf(m, snap, gen)
		}
		m = deliver(t, m, hostRestore(m, 1))
		if len(m.queue) != 1 || m.queue[0].Version != 0 {
			t.Fatalf("fixture: the queue %+v", m.queue)
		}
		row := m.queue[0]
		m.startQueueEdit(row)
		m.input.SetValue("mine")
		v := row.Version
		if err := eng.EditQueued(otherClient(t, m), row.ID, "theirs", &v); err != nil {
			t.Fatalf("fixture: the other client's edit: %v", err)
		}
		m = deliver(t, m, hostRestore(m, 2))
		if m.queueEdit != row.ID || m.queue[0].Version != 1 {
			t.Fatalf("fixture: editing %q over the restored row %+v", m.queueEdit, m.queue[0])
		}
		m = deliver(t, m, enter())
		if m.queueEdit != row.ID || m.input.Value() != "mine" || m.copyNote != staleEditNote || m.queueEditVer != 1 {
			t.Fatalf("editing %q, composer %q, note %q, version %d — want the save refused stale, the text kept, the version refreshed to 1",
				m.queueEdit, m.input.Value(), m.copyNote, m.queueEditVer)
		}
	})
}

// TestAPreStartRestoreKeepsTheReplayGuard (C27a, astra r63 5; C27b): a TUI
// attached when: "now" before its host starts a load restores the empty
// session, then folds the load's replay; Start's answer can reach it before it
// has read the replay's start, inside the replay, or after its end. Whichever,
// no send is admitted while the replay drains — the guard Config.Loading set
// holds across the empty restore, and the replay's start sets it where nothing
// did — and the session-is-up tail runs, once.
func TestAPreStartRestoreKeepsTheReplayGuard(t *testing.T) {
	for _, loading := range []bool{false, true} {
		for _, order := range []string{"before the replay", "inside the replay", "after the replay"} {
			name := fmt.Sprintf("loading=%v/started %s", loading, order)
			t.Run(name, func(t *testing.T) {
				h := newAttachHost(t, false)
				s := attachSession(t, h, protocol.WhenNow)
				isolateSkillsHome(t)
				m := New(Config{Backend: s, Theme: "tokyo-night", Workspace: frameWorkspace(t), Yolo: true, Loading: loading})
				tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
				m = tm.(Model)
				var tick int64
				m.clock = func() time.Time { tick++; return restoreAt.Add(time.Duration(tick) * time.Second) }
				ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
				defer cancel()
				if err := s.Attach(ctx); err != nil {
					t.Fatal(err)
				}
				r := newGateRig(t, m)
				r.send(r.nextStreamMsg()) // the empty session's restore
				if r.m.replaying != loading {
					t.Fatalf("the empty restore left the guard %v, want Config.Loading's %v", r.m.replaying, loading)
				}

				// The host's load: its replay, then its start.
				h.stub.Emit(replayEvent(agent.ReplayStart))
				h.stub.Emit(replayed(agent.Event{Type: agent.EventUser, Text: "an earlier prompt"}))
				h.stub.Emit(replayEvent(agent.ReplayEnd))
				if err := h.eng.Start(ctx); err != nil {
					t.Fatal(err)
				}
				started := runWatched(t, r.m.startCmd())
				if _, ok := started.(startedMsg); !ok {
					t.Fatalf("the start answered %#v", started)
				}
				refused := func(when string) {
					t.Helper()
					if r.m.sessionReady() {
						t.Fatalf("%s: the session is up", when)
					}
					r.m.input.SetValue("too soon")
					r.send(enter())
				}
				var upAt time.Time
				if order == "before the replay" {
					r.send(started)
					if loading {
						refused("the start's answer before the replay, the load announced")
					} else if !r.m.sessionReady() || r.m.sessStart.IsZero() {
						t.Fatal("the start's answer, nothing announced: the session is not up, or its tail did not run")
					}
					upAt = r.m.sessStart
				}
				replayStart, ok := r.nextStreamMsg().(eventMsg)
				if !ok || replayStart.ev.Type != agent.EventReplay {
					t.Fatalf("the stream's next item is %#v, want the replay's start", replayStart)
				}
				r.send(replayStart)
				if order == "inside the replay" {
					r.send(started)
				}
				refused("inside the replay")
				for {
					msg := r.nextStreamMsg()
					r.send(msg)
					if ev, ok := msg.(eventMsg); ok && ev.ev.Type == agent.EventReplay {
						break
					}
					refused("inside the replay")
				}
				if order == "after the replay" {
					refused("the replay over, Start's answer still out")
					r.send(started)
				}
				if !r.m.sessionReady() || r.m.sessStart.IsZero() {
					t.Fatalf("the replay ended and Start answered: ready %v, the tail ran at %v", r.m.sessionReady(), r.m.sessStart)
				}
				if !upAt.IsZero() && r.m.sessStart != upAt {
					t.Fatalf("the session-is-up tail ran again at the replay's end (%v, first %v)", r.m.sessStart, upAt)
				}
				// The rest of the start, its Ready included: the tail stays run.
				upAt = r.m.sessStart
				for {
					msg := r.nextStreamMsg()
					r.send(msg)
					if _, ok := msg.(readyMsg); ok {
						break
					}
				}
				if r.m.sessStart != upAt || !r.m.sessionReady() {
					t.Fatalf("after the start's Ready: the tail ran again (%v, first %v), or the session is down", r.m.sessStart, upAt)
				}
				if p := h.stub.Prompts(); len(p) != 0 {
					t.Fatalf("a send reached the host during the replay: %q", p)
				}
			})
		}
	}
}

// TestARestoreFromAnotherIncarnationTakesItsReplayGuard (C27b, astra r65 2):
// a model whose session was replaying when its host was replaced restores the
// replacement's empty, pre-start snapshot: that session is not the one the
// guard was set for, so the snapshot's word — no replay — stands. The new,
// non-loading session comes up on Start's answer alone: sends are admitted, and
// the tail runs, once.
func TestARestoreFromAnotherIncarnationTakesItsReplayGuard(t *testing.T) {
	m, _ := loadedStub(t, nil)
	var tick int64
	m.clock = func() time.Time { tick++; return restoreAt.Add(time.Duration(tick) * time.Second) }
	started := runCmd(m.startCmd())
	if _, ok := started.(startedMsg); !ok {
		t.Fatalf("the start answered %#v", started)
	}
	m = restoredWith(t, m, "inc-a", 1, seqd(1, replayEvent(agent.ReplayStart))...)
	if !m.replaying {
		t.Fatal("fixture: A is not replaying")
	}
	m = deliver(t, m, restoreOf(m, snapshotFolded(t, "inc-b"), 2))
	if m.replaying {
		t.Fatal("B's empty snapshot kept A's replay guard")
	}
	m = deliver(t, m, started)
	if !m.sessionReady() || m.sessStart.IsZero() {
		t.Fatalf("B's start: ready %v, the tail ran at %v", m.sessionReady(), m.sessStart)
	}
	upAt := m.sessStart
	m = deliver(t, m, readyMsg{})
	next, cmd := typeAndEnter(t, m, "hello")
	if cmd == nil || next.status != statusWorking {
		t.Fatalf("a send on B was refused: status %s", next.status)
	}
	if next.sessStart != upAt {
		t.Fatalf("the tail ran again (%v, first %v)", next.sessStart, upAt)
	}
}

// TestARestoreRetiresTheTurnsTransients (C27a, astra r63): the send-now
// confirm raised against the turn the model held, and the Ctrl+C window, are
// retired by a restore as that turn's own ending retires them — Enter then
// sends nothing now against the turn the restore brought, and Ctrl+C cancels it
// rather than quitting.
func TestARestoreRetiresTheTurnsTransients(t *testing.T) {
	m, stub := func() (Model, *Stub) {
		isolateSkillsHome(t)
		stub := NewStub()
		t.Cleanup(func() { _ = stub.Close() })
		return startStub(t, stub, t.TempDir(), 80, 24), stub
	}()
	open := stub.HangNext()
	m.input.SetValue("go")
	m = deliver(t, m, enter())
	awaitBarrier(t, open, "turn A opening")
	tm, _ := m.askStrongSend("NOW", "")
	m = tm.(Model)
	m.ctrlCDeadline = m.now().Add(time.Hour)
	if m.confirm == nil {
		t.Fatal("fixture: no confirm up")
	}
	turnB := seqd(1, agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-b", Phase: agent.TurnStarted, Text: "b", Origin: agent.TurnOriginSubmit}})
	m = restoredWith(t, m, "inc-1", 1, turnB...)
	if m.confirm != nil || !m.ctrlCDeadline.IsZero() || m.copyNote != "send now dropped" {
		t.Fatalf("confirm %+v, Ctrl+C window %v, note %q — want both retired and the drop noted", m.confirm, m.ctrlCDeadline, m.copyNote)
	}
	m = deliver(t, m, enter())
	if stub.CancelsSent() != 0 || m.sendNowPending() {
		t.Fatalf("Enter after the restore sent now against turn B: %d cancels, armed %v", stub.CancelsSent(), m.sendNowPending())
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if m = tm.(Model); m.quitting {
		t.Fatal("Ctrl+C after the restore quit on a window armed for turn A")
	}
}

// TestAnotherIncarnationsSameNumberedTurnIsANewTurn (C27a, astra r63): an
// engine numbers its turns from turn-1, so another incarnation's turn-1 is a
// new turn whatever its id: its start, its identity (turnSeq, which the plan
// evidence is keyed to) and its cancellation are its own.
func TestAnotherIncarnationsSameNumberedTurnIsANewTurn(t *testing.T) {
	started := func(sec int) agent.Event {
		return agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "go", Origin: agent.TurnOriginSubmit},
			At: restoreAt.Add(time.Duration(sec) * time.Second)}
	}
	m := restoredWith(t, sized(t), "inc-a", 1, seqd(1, started(1))...)
	m = deliver(t, m, eventMsg{ev: seqd(2, agent.Event{Type: agent.EventText, Text: "words"})[0], gen: 1})
	m.cancelled = true
	seq := m.turnSeq
	if m.turnID != "turn-1" || m.sawAssistantSeq != seq || !m.turnStart.Equal(restoreAt.Add(time.Second)) {
		t.Fatalf("fixture: turn %q, evidence %d of %d, start %v", m.turnID, m.sawAssistantSeq, seq, m.turnStart)
	}
	m = restoredWith(t, m, "inc-b", 2, seqd(1, started(100))...)
	if m.turnSeq == seq || m.sawAssistantSeq == m.turnSeq || m.cancelled || !m.turnStart.Equal(restoreAt.Add(100*time.Second)) {
		t.Fatalf("another incarnation's turn-1 kept the old one's: seq %d (was %d), evidence %d, cancelled %v, start %v",
			m.turnSeq, seq, m.sawAssistantSeq, m.cancelled, m.turnStart)
	}
}

// laterInfo is a backend whose Info already holds the facts of a later
// restore than the one the model is applying, as a socket backend's does once
// it has received that restore.
type laterInfo struct {
	backend.Backend
	eng  *engine.Engine
	info backend.SessionInfo
}

func (l *laterInfo) engine() *engine.Engine    { return l.eng }
func (l *laterInfo) Info() backend.SessionInfo { return l.info }

// TestARestoreReadsItsOwnInfo (C27a, astra r63): a restore decides with the
// facts its own item carries — which kinds raise a card, the provider the
// mirror names — never with the backend's, which a socket may already have
// replaced with a later restore's.
func TestARestoreReadsItsOwnInfo(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	eng, err := engine.New(stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	ws := t.TempDir()
	inner := newEngineBackend(eng, ws)
	own := inner.Info()
	own.Incarnation = "inc-a"
	later := own
	later.Incarnation, later.Provider, later.Label = "inc-b", "grok", "Grok"
	later.Capabilities.AskCards = false
	b := &laterInfo{Backend: inner, eng: eng, info: later}
	m := New(Config{Backend: b, Theme: "tokyo-night", Workspace: ws, Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = startedLikeInit(t, tm.(Model))
	if !own.Capabilities.AskCards || own.Provider == "grok" {
		t.Fatalf("fixture: the restore's own facts %+v", own)
	}
	snap := snapshotFolded(t, "inc-a", seqd(1, agent.Event{Type: agent.EventQuestion, Question: oneQuestion("ask-1")})...)
	m = deliver(t, m, restoreMsg{info: own, snap: snap, gen: 1})
	if !m.cardOpen() {
		t.Fatal("the restore judged its question by the later restore's capabilities: no card")
	}
	if m.snap.Provider.Name != own.Provider {
		t.Fatalf("the mirror names %q, want the restore's own %q", m.snap.Provider.Name, own.Provider)
	}
}

// TestALateForeignCancelFindsNoRestoredEpisode (C27a, astra r63): the answer
// to a cancel of the agent's own turn A, still on its way when a restore
// brings A's ending and the agent's turn B, neither marks B cancelled nor keeps
// B's own cancel from drawing its note.
func TestALateForeignCancelFindsNoRestoredEpisode(t *testing.T) {
	foreign := func(id string, running bool) agent.Event {
		return agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: id, Running: running}}
	}
	withA := seqd(1, foreign("f-a", true))
	withB := seqd(1, foreign("f-a", true), foreign("f-a", false), foreign("f-b", true))
	cancelNotes := func(m Model) int {
		n := 0
		for _, r := range m.main.rows {
			if r.local && r.kind == entryNote && r.text == stopCancelled {
				n++
			}
		}
		return n
	}
	lateFor := func(m Model) foreignCancelledMsg {
		return foreignCancelledMsg{issued: m.issue(), episode: m.foreignEpisode(), seq: m.turnSeq}
	}

	t.Run("a late answer does not mark B", func(t *testing.T) {
		m := restoredWith(t, sized(t), "inc-1", 1, withA...)
		if !m.snap.ForeignTurn {
			t.Fatal("fixture: A is not running")
		}
		late := lateFor(m)
		m = restoredWith(t, m, "inc-1", 2, withB...)
		m = deliver(t, m, late)
		if m.cancelled {
			t.Fatal("A's late answer marked B cancelled")
		}
	})
	t.Run("A's note does not silence B's", func(t *testing.T) {
		m := restoredWith(t, sized(t), "inc-1", 1, withA...)
		m = deliver(t, m, lateFor(m))
		if cancelNotes(m) != 1 {
			t.Fatalf("fixture: A's cancel drew %d notes", cancelNotes(m))
		}
		m = restoredWith(t, m, "inc-1", 2, withB...)
		if m.cancelled {
			t.Fatal("B was restored cancelled: A's cancel leaked into it")
		}
		m = deliver(t, m, lateFor(m))
		if cancelNotes(m) != 1 || !m.cancelled {
			t.Fatalf("B's own cancel drew %d notes, cancelled %v — want its note, and B cancelled", cancelNotes(m), m.cancelled)
		}
	})
}

// TestARestoreRetiresAPendingPlanImplementation (C27b, astra r65 3): a client
// attached with no turn of its own sees the agent's own turn earn the plan
// offer, and Enter dispatches its implementation — a mode change, then the
// prompt. A same-incarnation restore replaces the transcript before the mode
// change answers: its answer then implements nothing — a success sends no
// prompt, and a failure puts no offer back.
func TestARestoreRetiresAPendingPlanImplementation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("the mode change failed=%v", fail), func(t *testing.T) {
			m := sized(t)
			stub := stubOf(t, m)
			planID := ""
			for _, md := range m.snap.Modes {
				if m.snap.Provider.Kind(md.ID) == agent.ModePlan {
					planID = md.ID
				}
			}
			if planID == "" || m.implementModeID() == "" || m.snap.Provider.ImplementPrompt() == "" {
				t.Fatal("fixture: the provider offers no plan to implement")
			}
			inPlan := seqd(1, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Mode: &planID}})
			m = restoredWith(t, m, "inc-1", 1, inPlan...)
			foreign := func(running bool) agent.Event {
				return agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "f-1", Running: running}}
			}
			for _, ev := range seqd(2, foreign(true), agent.Event{Type: agent.EventText, Text: "the plan"},
				agent.Event{Type: agent.EventDone, StopReason: "end_turn"}, foreign(false)) {
				m = deliver(t, m, eventMsg{ev: ev, gen: 1})
			}
			if !m.planOffering() {
				t.Fatal("fixture: the agent's own turn earned no offer")
			}
			if fail {
				stub.FailNextSetMode()
			}
			tm, cmd := m.Update(enter())
			m = tm.(Model)
			impl := namedCmds(cmd, "implementPlan")
			if len(impl) != 1 {
				t.Fatalf("Enter dispatched %d implementations", len(impl))
			}
			answer := impl[0]()
			m = restoredWith(t, m, "inc-1", 2, inPlan...)
			m = deliver(t, m, answer)
			sent := slices.ContainsFunc(m.main.rows, func(r *entry) bool { return r.local && r.kind == entryUser })
			if m.status == statusWorking || sent {
				t.Fatalf("the retired implementation sent its prompt: status %s, rows %q", m.status, mainRows(m))
			}
			if m.planArmed() {
				t.Fatal("the retired implementation's failure put the offer back")
			}
		})
	}
}

// TestARestoreClearsTheCancelledFlag (C27b, astra r65 6): the cancelled flag
// stands only for the very engine turn it was set for, restored running on
// the same incarnation; the agent's own turn a restore brings starts
// uncancelled, whatever this client cancelled before it.
func TestARestoreClearsTheCancelledFlag(t *testing.T) {
	foreign := func(id string, running bool) agent.Event {
		return agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: id, Running: running}}
	}
	t.Run("the agent's own turn B after A's cancel", func(t *testing.T) {
		m := restoredWith(t, sized(t), "inc-1", 1, seqd(1, foreign("f-a", true))...)
		m = deliver(t, m, foreignCancelledMsg{issued: m.issue(), episode: m.foreignEpisode(), seq: m.turnSeq})
		if !m.cancelled {
			t.Fatal("fixture: A's cancel did not mark it")
		}
		m = restoredWith(t, m, "inc-1", 2, seqd(1, foreign("f-a", true), foreign("f-a", false), foreign("f-b", true))...)
		if m.cancelled {
			t.Fatal("B was restored cancelled")
		}
	})
	started := func(id string) agent.Event {
		return agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: id, Phase: agent.TurnStarted, Text: "go", Origin: agent.TurnOriginSubmit}}
	}
	for _, tc := range []struct {
		name, inc, turn string
		kept            bool
	}{
		{"the same engine turn, restored running", "inc-1", "turn-1", true},
		{"another engine turn", "inc-1", "turn-2", false},
		{"the same id, another incarnation", "inc-2", "turn-1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := restoredWith(t, sized(t), "inc-1", 1, seqd(1, started("turn-1"))...)
			m.cancelled = true
			m = restoredWith(t, m, tc.inc, 2, seqd(1, started(tc.turn))...)
			if m.cancelled != tc.kept {
				t.Fatalf("cancelled %v after the restore, want %v", m.cancelled, tc.kept)
			}
		})
	}
}

// TestANewSessionIsANewStart (C27b): a session replacing the one the model
// held owes its own session-is-up tail.
func TestANewSessionIsANewStart(t *testing.T) {
	m := sized(t)
	if !m.upDone {
		t.Fatal("fixture: the session's tail has not run")
	}
	next := NewStub()
	t.Cleanup(func() { _ = next.Close() })
	if b := m.eng; b != nil {
		t.Cleanup(func() { _ = b.Close() })
	}
	m.setSession(next, "")
	if m.upDone {
		t.Fatal("the new session's tail is counted as run")
	}
}
