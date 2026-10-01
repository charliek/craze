package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/roster"
)

// What answers after a switch (plan 030 C11r, astra r22-c11): the results of
// this terminal's own work for the session it left — the composer's shell's
// completion, a paste, a copy's note — never reach the session it shows now,
// delivered straight after the switch or from the held queue; every message
// of the first session, held through A→B→A, is dropped once A is back as
// another backend; and the band names a session by its list row's title.
// Every schedule is forced through the switch rig (switch_test.go), one
// message at a time, never by sleeping.

// shellIn runs script from the composer of the session the rig shows (`!`
// and enter), and answers the run's command, unrun: its completion is the
// test's to deliver.
func (r *switchRig) shellIn(script string) tea.Cmd {
	r.t.Helper()
	typeRunes(r, "!"+script)
	r.send(enter())
	if len(r.shells) != 1 || shellEntries(r.m) != 1 {
		r.t.Fatalf("fixture: `!%s` started %d runs, %d rows", script, len(r.shells), shellEntries(r.m))
	}
	return r.take(&r.shells, "the shell's run")
}

// noShellOf fails the test if the model shows anything of script: a row, a
// line of the frame, or its output kept for the next message.
func noShellOf(t *testing.T, m Model, script, out string) {
	t.Helper()
	view := plainView(m)
	if n := shellEntries(m); n != 0 || strings.Contains(view, script) || strings.Contains(view, out) {
		t.Fatalf("another session's command reached this one's transcript (%d rows):\n%s", n, view)
	}
	if len(m.shellCtx) != 0 {
		t.Fatalf("another session's command is context for this one's next message: %+v", m.shellCtx)
	}
}

// openAGate opens a gated call of the rig's session's own that answers only
// once the test closes the channel it answers.
func (r *switchRig) openAGate() chan struct{} {
	r.t.Helper()
	release := make(chan struct{})
	r.send(gateOpMsg{call: blockedCall(release, "done"), cont: noteCont("a's call")})
	if r.m.gate == nil {
		r.t.Fatal("fixture: no gate open")
	}
	return release
}

// TestAShellCompletionNeverCrossesASwitch (astra r22-c11 1): `!echo` run in A,
// and the switch to B made before its completion is applied — delivered after
// the switch (the switch killed the command), or held behind the dial's
// answer while a gate of A's was open and drained after the switch it
// carries. Either way nothing of it reaches B: no row, no output, no context
// for B's next message.
func TestAShellCompletionNeverCrossesASwitch(t *testing.T) {
	const script, out = "echo from-a", "from-a"
	t.Run("delivered after the switch", func(t *testing.T) {
		a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
		r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}})
		run := r.shellIn(script)
		r.switchTo(b, laneRow(b, "session b", time.Minute))
		r.up(b)
		done := runWatched(t, run)
		if _, ok := done.(shellDoneMsg); !ok {
			t.Fatalf("fixture: the run answered %#v", done)
		}
		// The command is over, so the fast beat its spinner wanted is not:
		// any Update's wrapper settles the tick chain first, so the
		// completion's own Update is all the next one does.
		r.send(sessRedrawMsg{})
		r.unapplied("A's shell completion", done)
		noShellOf(t, r.m, script, out)
	})

	t.Run("held across the switch", func(t *testing.T) {
		a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
		r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}})
		run := r.shellIn(script)
		r.openList(laneRow(b, "session b", time.Minute))
		r.enterOn(b)
		release := r.openAGate()
		r.send(r.pop(&r.opens, "dial"))
		// The command finishes while the switch is still held.
		done := runWatched(t, run)
		if d, ok := done.(shellDoneMsg); !ok || !strings.Contains(d.res.out, out) {
			t.Fatalf("fixture: the run answered %#v", done)
		}
		r.send(done)
		if got := heldKinds(r.m); !slices.Equal(got, []string{"tui.sessOpenedMsg", "tui.shellDoneMsg"}) {
			t.Fatalf("fixture: held %v", got)
		}
		close(release)
		r.send(r.pop(&r.calls, "A's call"))
		r.drainAll()
		if r.m.eng != b || len(r.m.held) != 0 {
			t.Fatalf("after the drain: switched to b %v, %d held", r.m.eng == b, len(r.m.held))
		}
		// Before B's first restore, which rebuilds its pane and so would
		// hide a row drawn there meanwhile — one the user would have seen.
		noShellOf(t, r.m, script, out)
		r.up(b)
		noShellOf(t, r.m, script, out)
	})
}

// TestAPasteNeverCrossesASwitch (astra r22-c11 2): ctrl+v in A, its clipboard
// read answering once the user has switched to B — straight into B's
// Update, held across the switch behind the dial's answer, or arriving while
// B's own gate is open — is never B's: B's composer keeps its own draft, and
// nothing of the paste is held to drain into it.
func TestAPasteNeverCrossesASwitch(t *testing.T) {
	const pasted = "from a's clipboard"
	pasteIn := func(r *switchRig) tea.Cmd {
		r.t.Helper()
		rec := captureCopies(r.t)
		_ = rec.write(pasted)
		r.send(tea.KeyMsg{Type: tea.KeyCtrlV})
		return r.take(&r.pastes, "A's paste")
	}
	answered := func(t *testing.T, paste tea.Cmd) tea.Msg {
		t.Helper()
		msg := runWatched(t, paste)
		if p, ok := msg.(pasteMsg); !ok || p.text != pasted {
			t.Fatalf("fixture: the paste answered %#v", msg)
		}
		return msg
	}

	t.Run("delivered after the switch", func(t *testing.T) {
		a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
		r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}})
		paste := pasteIn(r)
		r.switchTo(b, laneRow(b, "session b", time.Minute))
		r.up(b)
		typeRunes(r, "b's own")
		r.dropped("A's paste", answered(t, paste))
		if got := r.m.input.Value(); got != "b's own" {
			t.Fatalf("B's composer holds %q", got)
		}
	})

	t.Run("held across the switch", func(t *testing.T) {
		a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
		r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}})
		paste := pasteIn(r)
		r.openList(laneRow(b, "session b", time.Minute))
		r.enterOn(b)
		release := r.openAGate()
		r.send(r.pop(&r.opens, "dial"))
		r.send(answered(t, paste))
		if got := heldKinds(r.m); !slices.Equal(got, []string{"tui.sessOpenedMsg", "tui.pasteMsg"}) {
			t.Fatalf("fixture: held %v", got)
		}
		close(release)
		r.send(r.pop(&r.calls, "A's call"))
		r.drainAll()
		if r.m.eng != b || len(r.m.held) != 0 || r.m.input.Value() != "" {
			t.Fatalf("after the drain: switched to b %v, held %v, composer %q", r.m.eng == b, heldKinds(r.m), r.m.input.Value())
		}
		r.up(b)
		if got := r.m.input.Value(); got != "" {
			t.Fatalf("B's composer holds %q", got)
		}
	})

	t.Run("behind the next session's gate", func(t *testing.T) {
		a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
		r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}})
		paste := pasteIn(r)
		r.switchTo(b, laneRow(b, "session b", time.Minute))
		r.up(b)
		typeRunes(r, "to b")
		r.send(enter())
		if r.m.gate == nil {
			t.Fatal("fixture: B's prompt opened no gate")
		}
		r.dropped("A's paste", answered(t, paste))
		if len(r.m.held) != 0 {
			t.Fatalf("A's paste was held behind B's gate: %v", heldKinds(r.m))
		}
		b.answer <- engine.SubmitResult{Turn: "turn-1", Text: "to b"}
		r.send(r.pop(&r.calls, "B's call"))
		r.drainAll()
		if got := r.m.input.Value(); got != "" || !slices.Equal(texts(r.m, entryUser), []string{"to b"}) {
			t.Fatalf("after B's prompt: composer %q, user rows %q", got, texts(r.m, entryUser))
		}
	})
}

// TestACopyNoteNeverCrossesASwitch (C11r's audit): ctrl+y in A, its note
// answering once the user has switched to B, is A's status row's and not B's:
// dropped, though the copy itself was made.
func TestACopyNoteNeverCrossesASwitch(t *testing.T) {
	a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
	r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}})
	rec := captureCopies(t)
	r.stream(a, backend.Item{Kind: backend.ItemEvent, Gen: 1,
		Event: seqd(1, agent.Event{Type: agent.EventText, Text: "a's reply"})[0]})
	r.send(tea.KeyMsg{Type: tea.KeyCtrlY})
	cp := r.take(&r.copies, "A's copy")
	r.switchTo(b, laneRow(b, "session b", time.Minute))
	r.up(b)
	msg := runWatched(t, cp)
	if d, ok := msg.(clipboardDoneMsg); !ok || d.note == "" {
		t.Fatalf("fixture: the copy answered %#v", msg)
	}
	r.dropped("A's copy note", msg)
	if r.m.copyNote != "" || !slices.Equal(rec.copies(), []string{"a's reply"}) {
		t.Fatalf("B's status row says %q; copied %q", r.m.copyNote, rec.copies())
	}
}

// launchLaneModel is the launch flow's TUI (plan 030 §3.5) with its session
// known — a new grok session, which spawns lane a — and a session list whose
// Open answers lanes, sized, its gated calls asynchronous: up with no backend,
// its composer taking keys, and the spawn Init makes returned unrun, for the
// test to deliver when it says.
func launchLaneModel(t *testing.T, a *laneBackend, lanes map[string][]*laneBackend) (*switchRig, tea.Cmd) {
	t.Helper()
	isolateSkillsHome(t)
	m := New(Config{
		Theme: "tokyo-night", Workspace: a.info.Workspace, Yolo: true,
		Provider: agent.GrokProvider(), ProviderLocked: true,
		NewBackend: func(agent.Provider, bool) (backend.Backend, error) { return a, nil },
		Sessions:   laneSessions(lanes),
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	r := newSwitchRig(t, asyncGate(t, tm.(Model)))
	if r.m.eng != nil || r.m.spawnWaiting == 0 || r.m.picking() {
		t.Fatalf("fixture: before its spawn the launch holds %T (waiting %d, picking %v)", r.m.eng, r.m.spawnWaiting, r.m.picking())
	}
	return r, r.m.Init()
}

// transcriptWord is the screen cell of word's first letter in the transcript
// the rig's model draws.
func transcriptWord(r *switchRig, word string) (x, y int) {
	r.t.Helper()
	top := r.m.lay.Region(regionTranscript).Top
	for i, line := range r.m.cur().plainRows() {
		if c := strings.Index(line, word); c >= 0 {
			return lipgloss.Width(line[:c]), top + i - r.m.vp.YOffset
		}
	}
	r.t.Fatalf("the transcript draws no %q:\n%s", word, plainView(r.m))
	return 0, 0
}

// TestWorkAskedBeforeTheFirstAdoptionIsThatSessions (C11r2, astra
// r24-fix1112): a launch whose session is known is up before any backend —
// the frame in its starting state, the composer taking keys while the spawn
// runs — so ctrl+v, or a copy of a row it drew meanwhile (`/rename`'s "session
// is still starting"), can be asked for before the session's backend is
// adopted. What answers is that session's and no other's: applied once A,
// the backend the spawn answered, is adopted and up; dropped once the user
// has switched from A to B — delivered straight into B's Update, or held
// across the switch behind the dial's answer — and dropped as well when the
// switch to B was made before A was ever adopted (the launch's spawn, then
// abandoned, is closed and never adopted).
func TestWorkAskedBeforeTheFirstAdoptionIsThatSessions(t *testing.T) {
	const pasted, copied = "from the launch's clipboard", `copied "starting"`
	// askFor asks, before any backend, for kind's work, and answers its
	// command unrun.
	askFor := func(r *switchRig, kind string) tea.Cmd {
		r.t.Helper()
		rec := captureCopies(r.t)
		if kind == "a paste" {
			_ = rec.write(pasted)
			r.send(tea.KeyMsg{Type: tea.KeyCtrlV})
			return r.take(&r.pastes, "the startup paste")
		}
		typeRunes(r, "/rename early")
		r.send(enter())
		x, y := transcriptWord(r, "starting")
		r.send(dblClickMsg{X: x, Y: y})
		return r.take(&r.copies, "the startup copy")
	}
	// answered runs the work, bounded, and answers its message undelivered.
	answered := func(t *testing.T, kind string, work tea.Cmd) tea.Msg {
		t.Helper()
		msg := runWatched(t, work)
		switch m := msg.(type) {
		case pasteMsg:
			if kind == "a paste" && m.text == pasted {
				return msg
			}
		case clipboardDoneMsg:
			if kind == "a copy's note" && m.note == copied {
				return msg
			}
		}
		t.Fatalf("fixture: %s answered %#v", kind, msg)
		return nil
	}
	// adoptA delivers the launch's spawn: A adopted, and up.
	adoptA := func(r *switchRig, a *laneBackend, spawn tea.Cmd) {
		r.t.Helper()
		r.send(spawnedBy(r.t, spawn))
		if r.m.eng != a {
			r.t.Fatalf("fixture: the spawn's answer was not adopted (%T)", r.m.eng)
		}
		r.up(a)
	}
	// nothingOf fails the test if B's composer or status row holds anything
	// of the work: its composer is its own draft, and it shows no note.
	nothingOf := func(t *testing.T, r *switchRig, draft string) {
		t.Helper()
		if got := r.m.input.Value(); got != draft || r.m.copyNote != "" {
			t.Fatalf("B's composer holds %q (want %q), its status row says %q", got, draft, r.m.copyNote)
		}
	}

	for _, kind := range []string{"a paste", "a copy's note"} {
		t.Run(kind+", delivered on A", func(t *testing.T) {
			a := newLane(t, "a", "alpha")
			r, spawn := launchLaneModel(t, a, map[string][]*laneBackend{})
			work := askFor(r, kind)
			adoptA(r, a, spawn)
			r.send(answered(t, kind, work))
			switch got := r.m.input.Value(); {
			case kind == "a paste" && got != pasted:
				t.Fatalf("A's composer holds %q, want the paste asked for before A was adopted", got)
			case kind == "a copy's note" && r.m.copyNote != copied:
				t.Fatalf("A's status row says %q, want the note of the copy asked for before A was adopted", r.m.copyNote)
			}
		})

		t.Run(kind+", delivered after A→B", func(t *testing.T) {
			a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
			r, spawn := launchLaneModel(t, a, map[string][]*laneBackend{"b": {b}})
			work := askFor(r, kind)
			adoptA(r, a, spawn)
			r.switchTo(b, laneRow(a, "session a", time.Minute), laneRow(b, "session b", 2*time.Minute))
			r.up(b)
			typeRunes(r, "b's own")
			r.dropped(kind+" asked for before A was adopted", answered(t, kind, work))
			nothingOf(t, r, "b's own")
		})

		t.Run(kind+", held across A→B", func(t *testing.T) {
			a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
			r, spawn := launchLaneModel(t, a, map[string][]*laneBackend{"b": {b}})
			work := askFor(r, kind)
			adoptA(r, a, spawn)
			r.openList(laneRow(a, "session a", time.Minute), laneRow(b, "session b", 2*time.Minute))
			r.enterOn(b)
			release := r.openAGate()
			r.send(r.pop(&r.opens, "dial"))
			msg := answered(t, kind, work)
			r.send(msg)
			if got := heldKinds(r.m); !slices.Equal(got, []string{"tui.sessOpenedMsg", fmt.Sprintf("%T", msg)}) {
				t.Fatalf("fixture: held %v", got)
			}
			close(release)
			r.send(r.pop(&r.calls, "A's call"))
			r.drainAll()
			if r.m.eng != b || len(r.m.held) != 0 {
				t.Fatalf("after the drain: switched to b %v, held %v", r.m.eng == b, heldKinds(r.m))
			}
			nothingOf(t, r, "")
			r.up(b)
			nothingOf(t, r, "")
		})

		t.Run(kind+", after a switch before A was adopted", func(t *testing.T) {
			a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
			r, spawn := launchLaneModel(t, a, map[string][]*laneBackend{"b": {b}})
			work := askFor(r, kind)
			r.switchTo(b, laneRow(b, "session b", time.Minute))
			r.up(b)
			typeRunes(r, "b's own")
			r.dropped(kind+" asked for before any backend", answered(t, kind, work))
			nothingOf(t, r, "b's own")
			// The launch's spawn, answering now, was abandoned by the switch:
			// its backend is closed, never adopted.
			r.send(spawnedBy(t, spawn))
			if r.m.eng != b || r.last == nil {
				t.Fatalf("the abandoned spawn's answer: the model holds %T, command %v", r.m.eng, r.last)
			}
			runWatched(t, r.last)
			if a.closes.Load() != 1 {
				t.Fatalf("the abandoned spawn's backend was closed %d times", a.closes.Load())
			}
		})
	}
}

// TestTheFirstAsMessagesAreDroppedBackOnA (plan 030 §3.11, R2-5; astra
// r22-c11 3): A→B→A, with one of A's own messages held through both
// switches and delivered only once A is back — as another backend, A2, of
// the same session and incarnation — so nothing but the generations tells it
// from A2's own: a stream item taken by the read A's model armed before the
// first switch, a gated call's reply or its gate's timeout, the start's
// success or failure from the real start command over a Start still running
// on A's host, the shell's completion, a paste, a copy's note. Each is
// dropped: A2 is up, idle, not ended, and its own stream goes on.
func TestTheFirstAsMessagesAreDroppedBackOnA(t *testing.T) {
	for _, kind := range []string{
		"an event", "a restore", "a ready", "the end", "a command reply", "a gate timeout",
		"a start's success", "a start's failure", "a shell completion", "a paste", "a copy's note",
	} {
		t.Run(kind, func(t *testing.T) {
			a, b, a2 := newLane(t, "a", "alpha"), newLane(t, "b", "bravo"), newLane(t, "a", "alpha")
			lanes := map[string][]*laneBackend{"b": {b}, "a": {a2}}
			rows := []roster.Row{laneRow(a, "session a", time.Minute), laneRow(b, "session b", 2*time.Minute)}
			var r *switchRig
			if strings.HasPrefix(kind, "a start's") {
				a.startWith = make(chan error, 1)
				r = startingLaneModel(t, a, lanes)
			} else {
				r, _ = laneModel(t, a, lanes)
			}
			gA := r.m.bgen

			// A's message, asked for now and produced — as the program would
			// produce it — only once A2 is adopted (original).
			var original func() tea.Msg
			switch kind {
			case "an event", "a restore", "a ready", "the end":
				original = func() tea.Msg {
					a.push(laneItem(t, kind, a))
					return r.readOf(gA)
				}
			case "a command reply", "a gate timeout":
				call, _, _ := r.unansweredPrompt("to a", kind == "a gate timeout")
				original = func() tea.Msg {
					if kind == "a command reply" {
						a.answer <- engine.SubmitResult{Turn: "turn-9", Text: "to a"}
					}
					return runWatched(t, call)
				}
			case "a start's success", "a start's failure":
				answer := later(t, r.take(&r.starts, "A's start"))
				r.stream(a, a.restoreItem(t, 1))
				original = func() tea.Msg { return startAnswer(t, a, answer, kind == "a start's failure") }
			case "a shell completion":
				run := r.shellIn("echo from-a")
				original = func() tea.Msg { return runWatched(t, run) }
			case "a paste":
				rec := captureCopies(t)
				_ = rec.write("from a's clipboard")
				r.send(tea.KeyMsg{Type: tea.KeyCtrlV})
				paste := r.take(&r.pastes, "A's paste")
				original = func() tea.Msg { return runWatched(t, paste) }
			case "a copy's note":
				captureCopies(t)
				r.stream(a, backend.Item{Kind: backend.ItemEvent, Gen: 1,
					Event: seqd(1, agent.Event{Type: agent.EventText, Text: "a's reply"})[0]})
				r.send(tea.KeyMsg{Type: tea.KeyCtrlY})
				cp := r.take(&r.copies, "A's copy")
				original = func() tea.Msg { return runWatched(t, cp) }
			}

			// A → B → A2.
			r.switchTo(b, rows...)
			r.up(b)
			r.switchTo(a2, rows...)
			r.up(a2)
			if r.m.bgen == gA {
				t.Fatalf("back on A the backend generation is A's own (%d)", gA)
			}

			msg := original()
			if msg == nil {
				t.Fatalf("%s of A's answered nothing", kind)
			}
			if kind == "a shell completion" {
				// Applied in Update, as every result is: the tick chain the
				// command's spinner kept fast settles first (see
				// TestAShellCompletionNeverCrossesASwitch).
				r.send(sessRedrawMsg{})
				r.unapplied("A's shell completion, back on A", msg)
				noShellOf(t, r.m, "echo from-a", "from-a")
			} else {
				r.dropped(kind+" of A's, back on A", msg)
			}
			if kind == "a gate timeout" {
				// The call lingering past its deadline is still waited for,
				// where its panic would be recovered: it ends with nothing.
				a.answer <- engine.SubmitResult{}
				if m := r.pop(&r.lingers, "the linger's wait"); m != nil {
					t.Fatalf("the lingering call answered %#v", m)
				}
			}

			// A2 is itself: up, idle, its composer empty, its own stream on.
			if !r.m.sessionReady() || r.m.startErr != nil || r.m.status != statusIdle || r.m.ended ||
				r.m.sessList.open || r.m.input.Value() != "" {
				t.Fatalf("A2 after A's %s: ready %v, startErr %v, status %s, ended %v, list %v, composer %q",
					kind, r.m.sessionReady(), r.m.startErr, r.m.status, r.m.ended, r.m.sessList.open, r.m.input.Value())
			}
			r.stream(a2, backend.Item{Kind: backend.ItemEvent, Gen: 1,
				Event: seqd(1, agent.Event{Type: agent.EventText, Text: "back on a"})[0]})
			if r.m.foldedSeq() != 1 || slices.Contains(texts(r.m, entryAssistant), "late words from a") {
				t.Fatalf("A2's own event: folded %d, assistant rows %q", r.m.foldedSeq(), texts(r.m, entryAssistant))
			}
		})
	}
}

// laneItem is one stream item of each kind a backend left behind can still
// hand up (TestASwitchDropsTheStreamItLeft, TestTheFirstAsMessagesAreDroppedBackOnA):
// an event, a restore of b's own incarnation, a ready carrying a start
// failure, and the end.
func laneItem(t *testing.T, kind string, b *laneBackend) backend.Item {
	t.Helper()
	switch kind {
	case "an event":
		return backend.Item{Kind: backend.ItemEvent, Gen: 1,
			Event: seqd(1, agent.Event{Type: agent.EventText, Text: "late words from " + b.info.CrazeSessionID})[0]}
	case "a restore":
		return b.restoreItem(t, 2, seqd(1, agent.Event{Type: agent.EventText, Text: "a late snapshot"})...)
	case "a ready":
		return backend.Item{Kind: backend.ItemReady, Info: b.info, Err: errors.New("a late start failure")}
	case "the end":
		return backend.Item{Kind: backend.ItemEnd}
	}
	t.Fatalf("no stream item %q", kind)
	return backend.Item{}
}

// TestTheBandNamesTheSessionAsItsRowDoes (C11r, found by C12; amends X106):
// the band's title is the list row's, by one rule (sessTitle) — the
// session's own title, else the session index's, else `new session`. A
// session whose agent names nothing is named by the index (its first
// prompt's line) in its row and, once opened, in its band: the session the
// list was opened from, one opened from its row (from the switch on, and
// once it is up), one resumed from its saved row, and the row a session that
// ended before it was listed is given. Its own title, once it has one,
// outranks the index's in both.
func TestTheBandNamesTheSessionAsItsRowDoes(t *testing.T) {
	band := func(r *switchRig) string {
		r.t.Helper()
		return strings.Split(plainView(r.m), "\n")[0]
	}
	rowTitle := func(r *switchRig, k sessKey) string {
		r.t.Helper()
		row, ok := sessFind(r.m.sessLines(), k)
		if !ok {
			r.t.Fatalf("no row %+v:\n%s", k, plainView(r.m))
		}
		return row.title
	}
	indexed := func(b *laneBackend, index string, ago time.Duration) roster.Row {
		row := laneRow(b, "", ago)
		row.IndexTitle = index
		return row
	}

	t.Run("opened from its row", func(t *testing.T) {
		a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
		r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}})
		rowA, rowB := indexed(a, "alpha one", time.Minute), indexed(b, "bravo one", 2*time.Minute)

		// The session the list was opened from.
		r.openList(rowA, rowB)
		if got := rowTitle(r, runKey("a")); got != "alpha one" {
			t.Fatalf("A's row reads %q", got)
		}
		r.send(tea.KeyMsg{Type: tea.KeyEsc})
		if got := band(r); !strings.HasPrefix(got, "─ alpha one · ") {
			t.Fatalf("back on A the band reads %q", got)
		}

		// Another, opened from its row.
		r.openList(rowA, rowB)
		if got := rowTitle(r, runKey("b")); got != "bravo one" {
			t.Fatalf("B's row reads %q", got)
		}
		r.enterOn(b)
		r.send(r.pop(&r.opens, "dial"))
		if got := band(r); !strings.HasPrefix(got, "─ bravo one · ") {
			t.Fatalf("switched to B the band reads %q", got)
		}
		r.up(b)
		if got := band(r); !strings.HasPrefix(got, "─ bravo one · ") {
			t.Fatalf("B up, the band reads %q", got)
		}

		// Its own title outranks the index's, in the band and the row.
		title := "named by its agent"
		r.stream(b, backend.Item{Kind: backend.ItemEvent, Gen: 1,
			Event: seqd(1, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Title: &title}})[0]})
		if got := band(r); !strings.HasPrefix(got, "─ "+title+" · ") {
			t.Fatalf("B named, the band reads %q", got)
		}
		rowB.Session.Title = title
		r.openList(rowA, rowB)
		if got := rowTitle(r, runKey("b")); got != title {
			t.Fatalf("B named, its row reads %q", got)
		}
	})

	t.Run("resumed from its saved row", func(t *testing.T) {
		a, s := newLane(t, "a", "alpha"), newLane(t, "s", "sierra")
		r, fs := laneModel(t, a, map[string][]*laneBackend{})
		fs.open = func(roster.Ref) (backend.Backend, error) { return s, nil }
		r.openSavedList([]roster.Row{laneRow(a, "session a", time.Minute)}, savedLane(s, "the saved one"))
		r.m = selectKey(t, r.m, savedKey("s"))
		r.send(enter())
		r.send(r.pop(&r.opens, "dial"))
		if got := band(r); !strings.HasPrefix(got, "─ the saved one · ") {
			t.Fatalf("resumed, the band reads %q", got)
		}
	})

	t.Run("ended before it was listed", func(t *testing.T) {
		a, b := newLane(t, "a", "alpha"), newLane(t, "b", "bravo")
		r, _ := laneModel(t, a, map[string][]*laneBackend{"b": {b}})
		r.switchTo(b, laneRow(a, "session a", time.Minute), indexed(b, "bravo one", 2*time.Minute))
		r.up(b)
		// B ends before the list it goes back to has listed it, and its host
		// is gone from the next listing: its row is the model's own.
		r.stream(b, backend.Item{Kind: backend.ItemEnd})
		r.send(sessSnapMsg{gen: r.m.sessList.gen, snap: roster.Snapshot{}})
		row, ok := sessFind(r.m.sessLines(), runKey("b"))
		if !ok || !row.ended || row.title != "bravo one" {
			t.Fatalf("the ended row: %+v (listed %v)", row, ok)
		}
	})
}
