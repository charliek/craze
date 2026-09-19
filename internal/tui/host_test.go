package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/host"
)

// ---------------------------------------------------------------- fixtures

// recHost is Config.Host as a synchronous recorder: Publish runs on the test's
// own goroutine, inside Update, so the slice is the exact sequence the Update
// wrapper handed over. The hub's debounce and coalescing are internal/host's to
// test (plan 015 §3.5); this holds only what the TUI decides to publish.
type recHost struct {
	statuses []host.Status
	closes   int
}

func (r *recHost) Publish(s host.Status) { r.statuses = append(r.statuses, s) }
func (r *recHost) Close(context.Context) { r.closes++ }

// hostModel is a model with a recording host, sized and not yet started. The
// provider is grok and the stub's own snapshot names no provider, so every
// Provider a status carries can only have come from Config.Provider.
func hostModel(t *testing.T, loading bool) (Model, *Stub, *recHost) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })
	rec := &recHost{}
	m := New(Config{
		Session:   stub,
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Model:     "grok",
		Yolo:      true,
		Provider:  agent.GrokProvider(),
		Loading:   loading,
		Host:      rec,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return tm.(Model), stub, rec
}

// startedHostModel is hostModel with the session up, and the recorder emptied
// of the ready status every case starts from — asserted once, here.
func startedHostModel(t *testing.T) (Model, *Stub, *recHost) {
	t.Helper()
	m, stub, rec := hostModel(t, false)
	m = deliver(t, m, startedMsg{})
	assertStatuses(t, rec, idleStatus(host.DetailReady))
	rec.statuses = nil
	return m, stub, rec
}

// hostStatus is a status as the stub session carries it: its session id, the
// grok provider hostModel configures and the stub's current model.
func hostStatus(kind host.Kind, detail, msg string) host.Status {
	return host.Status{
		Kind:      kind,
		Message:   msg,
		Detail:    detail,
		SessionID: "stub-session-1",
		Provider:  "grok",
		Model:     "grok",
	}
}

func idleStatus(detail string) host.Status { return hostStatus(host.Idle, detail, "") }
func workingStatus() host.Status           { return hostStatus(host.Working, host.DetailPrompt, "") }

func assertStatuses(t *testing.T, rec *recHost, want ...host.Status) {
	t.Helper()
	if len(rec.statuses) != len(want) {
		t.Fatalf("published %d statuses, want %d:\n got %s\nwant %s",
			len(rec.statuses), len(want), fmtStatuses(rec.statuses), fmtStatuses(want))
	}
	for i := range want {
		if rec.statuses[i] != want[i] {
			t.Fatalf("status %d:\n got %+v\nwant %+v\n all %s", i, rec.statuses[i], want[i], fmtStatuses(rec.statuses))
		}
	}
}

func fmtStatuses(ss []host.Status) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = fmt.Sprintf("%s/%s/%q", s.Kind, s.Detail, s.Message)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// send types a prompt and presses Enter; the command is never run, so the
// turn's endings are whatever the test delivers next.
func send(t *testing.T, m Model, text string) Model {
	t.Helper()
	m, _ = typeAndEnter(t, m, text)
	if m.status != statusWorking {
		t.Fatalf("setup: the send did not start a turn (status %v)", m.status)
	}
	return m
}

// endTurn delivers both of a turn's endings, EventDone first.
func endTurn(t *testing.T, m Model, stop string) Model {
	t.Helper()
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: stop})
	return deliver(t, m, promptDoneMsg{res: agent.Result{StopReason: stop}})
}

// ------------------------------------------------------------------ cases

// TestHostStatusNothingBeforeTheSessionIsUp: a model that has only been built
// and sized has no session to speak for, so a host hears nothing — no idle
// with an empty model, no status for the pre-start frame (plan 015 §3.1).
func TestHostStatusNothingBeforeTheSessionIsUp(t *testing.T) {
	m, _, rec := hostModel(t, false)
	m = deliver(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	_ = deliver(t, m, refreshSnapMsg{})
	assertStatuses(t, rec)
}

// TestHostStatusPermissionTurn is D2: session up, a prompt, a permission card,
// its answer and the turn's end publish exactly Idle(ready), Working,
// Blocked(permission_prompt), Working, Idle(stop), carrying the provider from
// Config.Provider and the session's id and model.
func TestHostStatusPermissionTurn(t *testing.T) {
	m, stub, rec := hostModel(t, false)
	if len(rec.statuses) != 0 {
		t.Fatalf("published before the session was up: %s", fmtStatuses(rec.statuses))
	}
	m = deliver(t, m, startedMsg{})
	m = send(t, m, "run it")
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(false)})
	blocked := hostStatus(host.Blocked, host.DetailPermissionPrompt, "permission Shell")
	if !strings.Contains(plainView(m), blocked.Message) {
		t.Fatalf("the message is not the header the card draws, %q:\n%s", blocked.Message, plainView(m))
	}
	m, _ = press(m, runeKey('a'))
	if m.cardOpen() {
		t.Fatal("setup: the answer did not close the card")
	}
	// The stream ending alone does not settle the turn, so it publishes nothing.
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	if n := len(rec.statuses); n != 4 {
		t.Fatalf("EventDone alone published: %s", fmtStatuses(rec.statuses))
	}
	_ = deliver(t, m, promptDoneMsg{res: agent.Result{StopReason: "end_turn"}})

	assertStatuses(t, rec,
		idleStatus(host.DetailReady),
		workingStatus(),
		blocked,
		workingStatus(),
		idleStatus(host.DetailStop),
	)
}

// TestHostStatusQuestionAndPlanCards: the other two cards block with the
// header their card draws — plain `question`, not the card's 1/2 counter, and
// `plan <name>`.
func TestHostStatusQuestionAndPlanCards(t *testing.T) {
	t.Run("question", func(t *testing.T) {
		m, stub, rec := startedHostModel(t)
		m = send(t, m, "ask me")
		_ = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
		assertStatuses(t, rec, workingStatus(), hostStatus(host.Blocked, host.DetailQuestion, "question"))
	})
	t.Run("plan", func(t *testing.T) {
		m, stub, rec := startedHostModel(t)
		m = send(t, m, "plan it")
		m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent()})
		blocked := hostStatus(host.Blocked, host.DetailPlan, "plan Fake Plan")
		assertStatuses(t, rec, workingStatus(), blocked)
		if !strings.Contains(plainView(m), blocked.Message) {
			t.Fatalf("the message is not the header the card draws, %q:\n%s", blocked.Message, plainView(m))
		}
	})
}

// TestHostStatusErrorThenNextPrompt is D3: a turn ending in EventError is
// Failed with the error's first line, sanitised and bounded, and the next
// prompt is Working again. The prompt's own error, which follows EventError on
// the live session, changes nothing a host sees.
func TestHostStatusErrorThenNextPrompt(t *testing.T) {
	long := strings.Repeat("x", 300)
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"multi-line", errors.New("agent exited: status 1\npanic: boom\n\tgoroutine 1"), "agent exited: status 1"},
		{"leading blank line", errors.New("\n  rate limited  \nretry later"), "rate limited"},
		{"ansi and controls", errors.New("\x1b[31mauth\x1b[0m\tfailed\x07 \x1b]2;evil\x07now\nsecond"), "auth failed now"},
		{"over the cap", errors.New(long), ansi.Truncate(long, 200, "…")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, rec := startedHostModel(t)
			m = send(t, m, "go")
			m = feed(t, m, agent.Event{Type: agent.EventError, Err: tc.err})
			m = deliver(t, m, promptDoneMsg{err: tc.err})
			_ = send(t, m, "again")
			failed := hostStatus(host.Failed, host.DetailError, tc.want)
			assertStatuses(t, rec, workingStatus(), failed, workingStatus())
			if w := ansi.StringWidth(failed.Message); w > 200 {
				t.Fatalf("message is %d cells", w)
			}
		})
	}

	t.Run("no text", func(t *testing.T) {
		m, _, rec := startedHostModel(t)
		m = send(t, m, "go")
		_ = feed(t, m, agent.Event{Type: agent.EventError})
		assertStatuses(t, rec, workingStatus(), hostStatus(host.Failed, host.DetailError, "turn failed"))
	})
}

// TestHostStatusCancelEndings is D4's first half: both of a cancelled turn's
// endings publish Idle(cancelled), and the next send clears it, so the turn
// after is an ordinary stop.
func TestHostStatusCancelEndings(t *testing.T) {
	t.Run("EventDone cancelled", func(t *testing.T) {
		m, _, rec := startedHostModel(t)
		m = send(t, m, "go")
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
		m = endTurn(t, m, stopCancelled)
		m = send(t, m, "again")
		_ = endTurn(t, m, "end_turn")
		assertStatuses(t, rec,
			workingStatus(), idleStatus(host.DetailCancelled),
			workingStatus(), idleStatus(host.DetailStop),
		)
	})
	t.Run("ErrPromptCancelled", func(t *testing.T) {
		m, _, rec := startedHostModel(t)
		m = send(t, m, "go")
		m = deliver(t, m, promptDoneMsg{err: agent.ErrPromptCancelled})
		m = send(t, m, "again")
		_ = endTurn(t, m, "end_turn")
		assertStatuses(t, rec,
			workingStatus(), idleStatus(host.DetailCancelled),
			workingStatus(), idleStatus(host.DetailStop),
		)
	})
}

// TestHostStatusLoadedSessionWaitsForReplay is D4's second half: a loaded
// session is replaying from before its first event, and publishes nothing —
// not at Start, not for any replayed turn — until the replay ends, which is
// when a host first hears from it, as ready.
func TestHostStatusLoadedSessionWaitsForReplay(t *testing.T) {
	m, _, rec := hostModel(t, true)
	m = deliver(t, m, startedMsg{})
	m = feed(t, m, replayEvent(agent.ReplayStart))
	for _, ev := range replayTranscript() {
		m = feed(t, m, replayed(ev))
	}
	m = feed(t, m, replayed(agent.Event{Type: agent.EventDone, StopReason: "end_turn"}))
	if len(rec.statuses) != 0 {
		t.Fatalf("published during the replay: %s", fmtStatuses(rec.statuses))
	}
	_ = feed(t, m, replayEvent(agent.ReplayEnd))
	assertStatuses(t, rec, idleStatus(host.DetailReady))
}

// TestHostStatusStartFailure: a session that never came up is Failed with
// start_failed and the error's first line — the one status published without
// the session being ready.
func TestHostStatusStartFailure(t *testing.T) {
	m, _, rec := hostModel(t, false)
	_ = deliver(t, m, errMsg{errors.New("authentication failed: no key\nsee cursor-agent login")})
	assertStatuses(t, rec, hostStatus(host.Failed, host.DetailStartFailed, "authentication failed: no key"))
}

// TestHostStatusPublishesOnChangeOnly is D6 at the TUI level: an Update that
// changes nothing a host sees — a tick, a resize, a snapshot refresh — hands
// the hub nothing, and a change of model alone is a change.
func TestHostStatusPublishesOnChangeOnly(t *testing.T) {
	m, stub, rec := startedHostModel(t)
	m = deliver(t, m, tickMsg{gen: m.tickGen})
	m = deliver(t, m, tea.WindowSizeMsg{Width: 90, Height: 28})
	m = deliver(t, m, refreshSnapMsg{})
	if len(rec.statuses) != 0 {
		t.Fatalf("an unchanged status was republished: %s", fmtStatuses(rec.statuses))
	}
	if err := stub.SetModel(context.Background(), "fast"); err != nil {
		t.Fatal(err)
	}
	m = deliver(t, m, refreshSnapMsg{})
	_ = deliver(t, m, tickMsg{gen: m.tickGen})
	want := idleStatus(host.DetailReady)
	want.Model = "fast"
	assertStatuses(t, rec, want)
}

// TestHostStatusProviderIsTheOneStarted: with the picker, the provider a host
// is told is the one the user started, not Config.Provider's preselection. The
// stubs name no provider in their snapshot, so the choice is the only place
// "grok" can come from.
func TestHostStatusProviderIsTheOneStarted(t *testing.T) {
	isolateSkillsHome(t)
	rec := &recHost{}
	var built []*Stub
	m := New(Config{
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Yolo:      true,
		Provider:  agent.CursorProvider(),
		Providers: []agent.Provider{agent.CursorProvider(), agent.GrokProvider()},
		NewSession: func(agent.Provider) agent.Session {
			s := NewStub()
			built = append(built, s)
			return s
		},
		Host: rec,
	})
	t.Cleanup(func() {
		for _, s := range built {
			_ = s.Close()
		}
	})
	m = deliver(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	if !m.pickingProvider {
		t.Fatal("setup: expected the provider picker")
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyDown})
	m, _ = press(m, enter())
	_ = deliver(t, m, startedMsg{})
	assertStatuses(t, rec, idleStatus(host.DetailReady))
}

// ------------------------------------------------------------- finishRun

// orderLog records the exit tail's effects in the order they happened.
type orderLog struct{ events []string }

func (l *orderLog) add(e string) { l.events = append(l.events, e) }

// orderWriter names the two escape writes the exit tail makes and records any
// other write verbatim, so an unexpected byte shows up in the order too.
type orderWriter struct{ log *orderLog }

func (w orderWriter) Write(p []byte) (int, error) {
	switch string(p) {
	case ansi.SetWindowTitle(""):
		w.log.add("title clear")
	case resetPair:
		w.log.add("term reset")
	default:
		w.log.add(fmt.Sprintf("write %q", p))
	}
	return len(p), nil
}

// orderHost records its closes into the log. deadline is the time the first
// Close had left on its context: the one that has to release the pane.
type orderHost struct {
	log      *orderLog
	deadline time.Duration
	closed   bool
}

func (h *orderHost) Publish(host.Status) { h.log.add("host publish") }
func (h *orderHost) Close(ctx context.Context) {
	if d, ok := ctx.Deadline(); ok && !h.closed {
		h.deadline = time.Until(d)
	}
	h.closed = true
	h.log.add("host close")
}

type orderSession struct {
	*Stub
	log *orderLog
	// name tells a second session's close apart from the first's in the same
	// log. The session a model starts with leaves it empty.
	name string
	// exited makes Close report the agent exited on its own instead of nil —
	// the shape session.Close has after issue #23 when the child's exit was
	// reaped before craze's own Close on it.
	exited bool
}

func (s orderSession) Close() error {
	if s.name == "" {
		s.log.add("sess close")
	} else {
		s.log.add("sess close " + s.name)
	}
	_ = s.Stub.Close()
	if s.exited {
		return agent.ErrAgentExited
	}
	return nil
}

// exitTailModel is a model the way Run leaves it: colours applied through the
// recording writer (their set is written before the log starts) and a tab
// title that was set. It is also a provider picker nobody has answered yet: its
// Config carries a NewSession, without which confirmProvider swaps nothing,
// and the session that swap builds logs its close as "sess close picked".
func exitTailModel(t *testing.T) (Model, *orderLog, orderWriter) {
	t.Helper()
	isolateSkillsHome(t)
	log := &orderLog{}
	w := orderWriter{log: log}
	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })
	m := New(Config{
		Session: orderSession{Stub: stub, log: log},
		NewSession: func(p agent.Provider) agent.Session {
			picked := NewStub()
			picked.SetProvider(p)
			t.Cleanup(func() { _ = picked.Close() })
			return orderSession{Stub: picked, log: log, name: "picked"}
		},
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
	})
	m.term = newTerminalColors(w)
	m.term.apply(m.theme)
	m.lastTitle = "✦ craze"
	log.events = nil
	return m, log, w
}

func assertOrder(t *testing.T, log *orderLog, want ...string) {
	t.Helper()
	if strings.Join(log.events, ", ") != strings.Join(want, ", ") {
		t.Fatalf("exit tail order:\n got %q\nwant %q", log.events, want)
	}
}

// TestFinishRunOrder is B4: title clear, terminal reset, host close, session
// close, in that order — and with the nil final a recovered panic hands back,
// the host still closes, from the argument and not from final, and the session
// that closes is the one the program ended with, not the one it started with.
// It is also the chain's seam for issue #23 (§3.7): the "the agent exited on
// its own" case proves the bool finishRun returns follows the session's
// close, not just startErr.
func TestFinishRunOrder(t *testing.T) {
	t.Run("final is the model", func(t *testing.T) {
		m, log, w := exitTailModel(t)
		h := &orderHost{log: log}
		startErr := errors.New("never started")
		final := m
		final.startErr = startErr
		showAgentDiag, got := finishRun(w, final, m, h)
		if !errors.Is(got, startErr) {
			t.Fatalf("finishRun returned %v, want the start failure", got)
		}
		if !showAgentDiag {
			t.Fatal("finishRun did not report the run as failed for a start error")
		}
		assertOrder(t, log, "title clear", "term reset", "host close", "sess close")
		if h.deadline <= 0 || h.deadline > host.DefaultCloseTimeout {
			t.Fatalf("host close deadline %v, want within %v", h.deadline, host.DefaultCloseTimeout)
		}
	})
	t.Run("final is not a model", func(t *testing.T) {
		// nil, because that is what p.Run returns on a recovered Update or
		// View panic: its recover sets only the error, and the model it
		// returns keeps its zero value. TestRecoveredPanicClosesThePickedSession
		// pins that against the real program.
		m, log, w := exitTailModel(t)
		h := &orderHost{log: log}
		showAgentDiag, err := finishRun(w, nil, m, h)
		if err != nil {
			t.Fatalf("finishRun returned %v", err)
		}
		// A nil final leaves started false, which alone makes this a failed
		// run: the recovered-panic case must show the agent's stderr even
		// with no start error of its own.
		if !showAgentDiag {
			t.Fatal("finishRun did not report the run as failed for a nil final")
		}
		// No title clear: without a Model there is no lastTitle to say one
		// was set, exactly as before the tail was extracted.
		assertOrder(t, log, "term reset", "host close", "sess close")
	})
	t.Run("the picker swapped the session", func(t *testing.T) {
		// Issue #19. The session the picker builds lives only in the model
		// copies made after the swap, and a recovered panic hands back none
		// of them: finishRun gets a nil final and the model Run started with.
		// Passing the swapped model instead would prove nothing — reading its
		// own session was never the bug.
		initial, log, w := exitTailModel(t)
		h := &orderHost{log: log}
		updated, cmd := initial.confirmProvider(agent.GrokProvider(), true)
		_ = cmd // the new session's Start: nothing here runs it
		// The picker closes the session it replaces itself. That close is the
		// swap's, not the exit tail's, so the log starts again after it.
		assertOrder(t, log, "sess close")
		log.events = nil
		if updated.(Model).sess == initial.sess {
			t.Fatal("setup: confirmProvider did not swap the session")
		}
		if _, err := finishRun(w, nil, initial, h); err != nil {
			t.Fatalf("finishRun returned %v", err)
		}
		// Exact: the picked session closes, last, and the initial one is not
		// closed again — a plain "sess close" here would be it, with the
		// picked session and its agent left running.
		assertOrder(t, log, "term reset", "host close", "sess close picked")
	})
	t.Run("no host", func(t *testing.T) {
		m, log, w := exitTailModel(t)
		_, _ = finishRun(w, m, m, nil)
		assertOrder(t, log, "title clear", "term reset", "sess close")
	})
	t.Run("the agent exited on its own", func(t *testing.T) {
		// An orderSession-style stub whose Close reports agent.ErrAgentExited
		// — the shape session.Close has after issue #23 — with no start error
		// at all: showAgentDiag must still come back true, and startErr must
		// stay nil and unaffected, proving the two terms are separate.
		// started is set explicitly: exitTailModel's own New() never sends
		// startedMsg, and leaving started false would let !started alone
		// carry showAgentDiag, masking whether agentExited was folded in at
		// all.
		m, log, w := exitTailModel(t)
		m.setSession(orderSession{Stub: NewStub(), log: log, exited: true})
		m.started = true
		h := &orderHost{log: log}
		showAgentDiag, startErr := finishRun(w, m, m, h)
		if startErr != nil {
			t.Fatalf("finishRun returned a start error %v, want nil", startErr)
		}
		if !showAgentDiag {
			t.Fatal("finishRun did not report the agent's own exit")
		}
		assertOrder(t, log, "title clear", "term reset", "host close", "sess close")
	})
}

// swapThenPanic drives a real program through issue #19's two steps: the
// provider picker's swap on one message, then a panic inside Update on the
// next. The swap's own Cmd is dropped, so no Start runs; the returned Cmd is
// what delivers the panic, so the order does not depend on scheduling.
type swapThenPanic struct{ inner Model }

type (
	swapSessionMsg struct{}
	panicUpdateMsg struct{}
)

func (s swapThenPanic) Init() tea.Cmd {
	return func() tea.Msg { return swapSessionMsg{} }
}

func (s swapThenPanic) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case swapSessionMsg:
		updated, cmd := s.inner.confirmProvider(agent.GrokProvider(), true)
		_ = cmd
		s.inner = updated.(Model)
		return s, func() tea.Msg { return panicUpdateMsg{} }
	case panicUpdateMsg:
		panic("swapThenPanic: the Update panic issue #19 is about")
	}
	return s, nil
}

func (s swapThenPanic) View() string { return "" }

// TestRecoveredPanicClosesThePickedSession is issue #19 end to end on a real
// bubbletea program, built the way the frame runner builds one. It pins the
// third-party fact the fix rests on — an Update panic makes p.Run return a nil
// model and an error wrapping tea.ErrProgramPanic — so an upgrade that changes
// it fails here rather than quietly reopening the leak; then finishRun, handed
// only the model Run started with, still closes the session the picker built.
//
// bubbletea's recover prints "Caught panic:" and a stack trace to the test's
// output. That is the recovery working, not the suite breaking.
func TestRecoveredPanicClosesThePickedSession(t *testing.T) {
	initial, log, w := exitTailModel(t)
	h := &orderHost{log: log}
	p := tea.NewProgram(swapThenPanic{inner: initial}, tea.WithoutRenderer(), tea.WithInput(nil))
	final, err := p.Run()
	if final != nil {
		t.Fatalf("p.Run returned a %T after an Update panic, want nil", final)
	}
	if !errors.Is(err, tea.ErrProgramPanic) {
		t.Fatalf("p.Run returned %v, want an error wrapping tea.ErrProgramPanic", err)
	}
	// Update ran on this goroutine, inside p.Run, so the picker's own close of
	// the initial session is already in the log.
	assertOrder(t, log, "sess close")
	log.events = nil
	if _, err := finishRun(w, final, initial, h); err != nil {
		t.Fatalf("finishRun returned %v", err)
	}
	assertOrder(t, log, "term reset", "host close", "sess close picked")
}

// beginPanics is a session whose Begin panics. sendText calls Begin inside
// Update, where it claims the turn, so a prompt typed at it is an Update panic
// with no hook in production code. closes counts Close calls from any copy.
type beginPanics struct {
	*Stub
	closes *atomic.Int32
}

func (s beginPanics) Begin(string) func(context.Context) (agent.Result, error) {
	panic("beginPanics: the Update panic issue #19 is about")
}

func (s beginPanics) Close() error {
	s.closes.Add(1)
	return s.Stub.Close()
}

// TestFramePanicClosesThePickedSession is issue #19 in the frame runner, which
// had the same hole: it closed the session of the model p.Run handed back, and
// a recovered panic hands back nil. The model starts as a provider picker with
// no session at all, Enter builds one, and a prompt panics inside Update. The
// runner has nothing to read the session from but the owner, so it is the
// owner's that must close. On a quit the old path already found the right
// session; only the panic tells the two apart. That p.Run's model is nil here
// is TestRecoveredPanicClosesThePickedSession's to pin; this sees the error it
// comes with.
func TestFramePanicClosesThePickedSession(t *testing.T) {
	isolateSkillsHome(t)
	var built, closes atomic.Int32
	_, _, err := RunFrameScript(Config{
		Theme:     "tokyo-night",
		Workspace: frameWorkspace(t),
		Model:     "grok",
		Yolo:      true,
		NewSession: func(p agent.Provider) agent.Session {
			built.Add(1)
			s := NewStub()
			s.SetProvider(p)
			return beginPanics{Stub: s, closes: &closes}
		},
	}, 80, 24, "<enter><wait:idle>hi<enter>", FrameOpts{Timeout: 5 * time.Second})
	if !errors.Is(err, tea.ErrProgramPanic) {
		t.Fatalf("RunFrameScript returned %v, want the program's panic", err)
	}
	if n := built.Load(); n != 1 {
		t.Fatalf("the picker built %d sessions, want 1", n)
	}
	if n := closes.Load(); n != 1 {
		t.Fatalf("the picked session was closed %d times, want once", n)
	}
}

// ------------------------------------------------------------------ quits

// quitModel is a started model whose session and host record into one log, so
// what a quit does to each can be ordered against the other.
func quitModel(t *testing.T) (Model, *orderLog, *orderHost) {
	t.Helper()
	isolateSkillsHome(t)
	log := &orderLog{}
	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })
	h := &orderHost{log: log}
	m := New(Config{
		Session:   orderSession{Stub: stub, log: log},
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Model:     "grok",
		Yolo:      true,
		Host:      h,
	})
	m = deliver(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	return deliver(t, m, startedMsg{}), log, h
}

// handlerMsg runs the handler's own command and returns its message. The
// Update wrapper batches its own commands in behind the handler's, so the
// handler's is always the first member, however deeply batched; nothing else
// is run, so no tick waits out a real timer.
func handlerMsg(cmd tea.Cmd) tea.Msg {
	for cmd != nil {
		msg := cmd()
		batch, ok := msg.(tea.BatchMsg)
		if !ok {
			return msg
		}
		cmd = nil
		if len(batch) > 0 {
			cmd = batch[0]
		}
	}
	return nil
}

// TestQuitReleasesTheHostBeforeClosingTheSession holds plan 015 §3.2's exit
// order on every quit craze asks for itself — /exit, Ctrl+D, and each Ctrl+C
// that quits — end to end: the quit command, and then the exit tail p.Run hands
// off to. sess.Close may block, so the host must already be released by the
// time the session is first closed; the TestFinishRunOrder recorders start
// after the quit command has run and cannot see this.
func TestQuitReleasesTheHostBeforeClosingTheSession(t *testing.T) {
	pressKey := func(k tea.KeyType) func(*testing.T, Model) (Model, tea.Cmd) {
		return func(_ *testing.T, m Model) (Model, tea.Cmd) {
			tm, cmd := m.Update(tea.KeyMsg{Type: k})
			return tm.(Model), cmd
		}
	}
	cases := []struct {
		name string
		quit func(*testing.T, Model) (Model, tea.Cmd)
	}{
		{"/exit", func(_ *testing.T, m Model) (Model, tea.Cmd) {
			m.input.SetValue("/exit")
			tm, cmd := m.Update(enter())
			return tm.(Model), cmd
		}},
		{"ctrl+d", pressKey(tea.KeyCtrlD)},
		{"ctrl+c idle", pressKey(tea.KeyCtrlC)},
		{"ctrl+c after an error", func(t *testing.T, m Model) (Model, tea.Cmd) {
			m = send(t, m, "go")
			m = feed(t, m, agent.Event{Type: agent.EventError, Err: errors.New("boom")})
			return pressKey(tea.KeyCtrlC)(t, m)
		}},
		{"ctrl+c twice while working", func(t *testing.T, m Model) (Model, tea.Cmd) {
			now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
			m.clock = func() time.Time { return now }
			m = send(t, m, "go")
			m, _ = pressKey(tea.KeyCtrlC)(t, m) // cancels; its command is not an exit
			if m.quitting {
				t.Fatal("setup: the first ctrl+c while working quit")
			}
			now = now.Add(ctrlCWindow / 2)
			return pressKey(tea.KeyCtrlC)(t, m)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, log, h := quitModel(t)
			m, cmd := tc.quit(t, m)
			if !m.quitting || cmd == nil {
				t.Fatalf("setup: %s did not quit", tc.name)
			}
			log.events = nil // what happened before the quit is not the exit
			if msg := handlerMsg(cmd); msg != (tea.QuitMsg{}) {
				t.Fatalf("the quit command returned %T, want tea.QuitMsg", msg)
			}
			_, _ = finishRun(io.Discard, m, m, h)

			hostAt, sessAt := slices.Index(log.events, "host close"), slices.Index(log.events, "sess close")
			if hostAt < 0 || sessAt < 0 || hostAt > sessAt {
				t.Fatalf("the host must be released before the session is first closed: %q", log.events)
			}
			if h.deadline <= 0 || h.deadline > host.DefaultCloseTimeout {
				t.Fatalf("the first host close had deadline %v, want within %v", h.deadline, host.DefaultCloseTimeout)
			}
		})
	}
}

// TestRunErrAfterHangup: once a hangup has ended the program, what the dead
// terminal made bubbletea report is not a failure — a hangup exits 0 like
// SIGTERM — but a recovered panic still is, and without a hangup nothing
// changes.
func TestRunErrAfterHangup(t *testing.T) {
	eio := fmt.Errorf("%w: error reading input: %w", tea.ErrProgramKilled, syscall.EIO)
	panicked := fmt.Errorf("%w: %w", tea.ErrProgramKilled, tea.ErrProgramPanic)
	cases := []struct {
		name   string
		err    error
		hungUp bool
		want   error
	}{
		{"a clean quit", nil, false, nil},
		{"a clean hangup", nil, true, nil},
		{"an input error without a hangup is kept", eio, false, eio},
		{"the dead terminal after a hangup is dropped", eio, true, nil},
		{"a panic is kept without a hangup", panicked, false, panicked},
		{"a panic is kept after a hangup too", panicked, true, panicked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runErrAfterHangup(tc.err, tc.hungUp); !errors.Is(got, tc.want) || (got == nil) != (tc.want == nil) {
				t.Fatalf("runErrAfterHangup(%v, %v) = %v, want %v", tc.err, tc.hungUp, got, tc.want)
			}
		})
	}
}
