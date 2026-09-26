package tui

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/sessions"
)

// The resume picker's claim (plan 027 §3.9, SQ16): with a Config.ClaimSession
// the picker claims the chosen row in a tea.Cmd and builds it only when the
// answer lands while it is still waiting for that attempt.

// fakeClaim is a ClaimSession that answers id (or err), counting its calls and
// the releases of what it handed out.
type fakeClaim struct {
	id       string
	err      error
	calls    atomic.Int32
	releases atomic.Int32
}

func (f *fakeClaim) claim(sessions.Row) (string, func(), error) {
	f.calls.Add(1)
	if f.err != nil {
		return "", nil, f.err
	}
	return f.id, func() { f.releases.Add(1) }, nil
}

// claimingPicker is newResumePicker with a ClaimSession.
func claimingPicker(t *testing.T, claim func(sessions.Row) (string, func(), error)) (Model, *[]sessions.Row) {
	t.Helper()
	isolateSkillsHome(t)
	loaded := &[]sessions.Row{}
	m := New(Config{
		Theme:        "tokyo-night",
		Workspace:    t.TempDir(),
		Model:        "grok",
		Yolo:         true,
		Provider:     agent.CursorProvider(),
		Resume:       threeResumeRows(),
		ClaimSession: claim,
		LoadSession: func(p agent.Provider, row sessions.Row) agent.Session {
			*loaded = append(*loaded, row)
			s := NewStub()
			s.SetProvider(p)
			return s
		},
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return tm.(Model), loaded
}

// pressEnter is Enter on the picker; it must return the claim's command and
// build nothing yet.
func pressEnter(t *testing.T, m Model, loaded *[]sessions.Row) (Model, tea.Cmd) {
	t.Helper()
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	out := tm.(Model)
	if cmd == nil {
		t.Fatal("Enter returned no command")
	}
	if len(*loaded) != 0 || out.eng != nil || !out.pickingResume {
		t.Fatal("Enter built the row before its claim answered")
	}
	return out, cmd
}

// TestAPickerClaimBuildsWithTheClaimedID: the answer builds the row, and the
// engine carries the id the claim was taken for — a legacy row's minted one
// here — so the claim and the engine agree. Nothing is released: the claim is
// the process's now.
func TestAPickerClaimBuildsWithTheClaimedID(t *testing.T) {
	f := &fakeClaim{id: "018f-claimed"}
	m, loaded := claimingPicker(t, f.claim)
	m, cmd := pressEnter(t, m, loaded)
	if f.calls.Load() != 0 {
		t.Fatal("Enter's Update ran the claim itself")
	}
	msg := runCmd(cmd)
	if f.calls.Load() != 1 {
		t.Fatalf("the command ran the claim %d times", f.calls.Load())
	}
	tm, start := m.Update(msg)
	out := tm.(Model)
	t.Cleanup(func() { _ = out.eng.Close() })
	if out.pickingResume || out.eng == nil || len(*loaded) != 1 || (*loaded)[0].SessionID != "s-1" {
		t.Fatalf("the answer did not build the row: picking %v, loaded %v", out.pickingResume, *loaded)
	}
	if (*loaded)[0].CrazeID != "018f-claimed" {
		t.Fatalf("the row was built as %q", (*loaded)[0].CrazeID)
	}
	if got := out.eng.State().CrazeSessionID; got != "018f-claimed" {
		t.Fatalf("the engine's craze id is %q, want the claimed one", got)
	}
	if f.releases.Load() != 0 {
		t.Fatal("a claim that built its row was released")
	}
	if start == nil {
		t.Fatal("the answer returned no start batch")
	}
	assertOwned(t, out)
}

// TestASecondEnterWhileClaimingDoesNothing: one attempt at a time.
func TestASecondEnterWhileClaimingDoesNothing(t *testing.T) {
	f := &fakeClaim{id: "018f-one"}
	m, loaded := claimingPicker(t, f.claim)
	m, _ = pressEnter(t, m, loaded)
	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || tm.(Model).resumeWaiting != m.resumeWaiting {
		t.Fatal("a second Enter while a claim is in flight started another")
	}
}

// TestAStalePickerClaimIsReleased: an answer for an attempt the picker is no
// longer waiting for — Esc quit while it was in flight, or a stamp that is not
// the current attempt's — builds nothing and releases its claim at once.
func TestAStalePickerClaimIsReleased(t *testing.T) {
	t.Run("after Esc", func(t *testing.T) {
		f := &fakeClaim{id: "018f-late"}
		m, loaded := claimingPicker(t, f.claim)
		m, cmd := pressEnter(t, m, loaded)
		tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		m = tm.(Model)
		if !m.quitting {
			t.Fatal("Esc did not quit while a claim was in flight")
		}
		tm, _ = m.Update(runCmd(cmd))
		out := tm.(Model)
		if f.releases.Load() != 1 {
			t.Fatalf("the late claim was released %d times, want once", f.releases.Load())
		}
		if out.eng != nil || len(*loaded) != 0 {
			t.Fatal("a late claim built its row after the picker quit")
		}
	})
	t.Run("a superseded attempt", func(t *testing.T) {
		f := &fakeClaim{id: "018f-old"}
		m, loaded := claimingPicker(t, f.claim)
		m, _ = pressEnter(t, m, loaded)
		released := 0
		tm, _ := m.Update(resumeClaimMsg{
			attempt: m.resumeWaiting - 1, row: m.resume[0], crazeID: "018f-old",
			release: func() { released++ },
		})
		out := tm.(Model)
		if released != 1 || out.eng != nil || len(*loaded) != 0 || out.resumeWaiting != m.resumeWaiting {
			t.Fatalf("a stale stamp: released %d, built %d, waiting %d", released, len(*loaded), out.resumeWaiting)
		}
	})
}

// TestAPickerRefusalShowsAnErrorRow: a refusal keeps the picker up with its
// text in an error row, builds nothing, lets the next Enter try again, and a
// cursor move clears the row.
func TestAPickerRefusalShowsAnErrorRow(t *testing.T) {
	f := &fakeClaim{err: errors.New("that session is open in another craze (pid 4242)")}
	m, loaded := claimingPicker(t, f.claim)
	m, cmd := pressEnter(t, m, loaded)
	tm, _ := m.Update(runCmd(cmd))
	m = tm.(Model)
	if !m.pickingResume || m.eng != nil || len(*loaded) != 0 {
		t.Fatal("a refusal closed the picker or built the row")
	}
	view := plainView(m)
	if !strings.Contains(view, "that session is open in another craze (pid 4242)") {
		t.Fatalf("no error row:\n%s", view)
	}
	for _, want := range []string{"fix: the flaky pty test", "port the docs site", resumeDialogHint} {
		if !strings.Contains(view, want) {
			t.Fatalf("the error row pushed %q out of a full-size box:\n%s", want, view)
		}
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil {
		t.Fatal("Enter after a refusal started no new attempt")
	}
	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if view := plainView(tm.(Model)); strings.Contains(view, "pid 4242") {
		t.Fatalf("the cursor moved and the error row stayed:\n%s", view)
	}
}

// TestTheErrorRowNeverTakesTheLastListRow: in a short box the error row goes
// before the footer does, and never before the row the cursor is on.
func TestTheErrorRowNeverTakesTheLastListRow(t *testing.T) {
	m, _ := claimingPicker(t, (&fakeClaim{}).claim)
	m.resumeErr = "the session index is busy — try again"
	m.resumeCursor = 2
	for budget, want := range map[int]struct {
		shown       int
		err, footer bool
	}{
		2: {shown: 1},
		3: {shown: 1, err: true},
		4: {shown: 1, err: true, footer: true},
		5: {shown: 2, err: true, footer: true},
	} {
		top, shown, footer := m.resumeDialogPlan(budget)
		if shown != want.shown || footer != want.footer || m.resumeErrShown(budget) != want.err {
			t.Fatalf("budget %d: shown %d footer %v err %v, want %+v", budget, shown, footer, m.resumeErrShown(budget), want)
		}
		if m.resumeCursor < top || m.resumeCursor >= top+shown {
			t.Fatalf("budget %d: the window [%d,%d) lost the cursor", budget, top, top+shown)
		}
		if body := m.resumeDialogBody(dialogMaxWidth-dialogBorder, budget); len(body) > budget {
			t.Fatalf("budget %d: %d rows", budget, len(body))
		}
	}
}

// TestOnEngineSeesEveryInstalledEngine: the hook is handed each engine
// setSession installs — New's, and the one a picker builds — after the owner
// holds it, and never nil.
func TestOnEngineSeesEveryInstalledEngine(t *testing.T) {
	isolateSkillsHome(t)
	var seen []*engine.Engine
	var owner *sessionOwner
	hook := func(e *engine.Engine) {
		if e == nil {
			t.Fatal("OnEngine was handed nil")
		}
		if owner != nil && owner.current() != e {
			t.Fatal("OnEngine ran before the owner held the engine")
		}
		seen = append(seen, e)
	}
	m := New(Config{
		Theme: "tokyo-night", Workspace: t.TempDir(), Provider: agent.CursorProvider(),
		ProviderLocked: true, Session: NewStub(), OnEngine: hook,
	})
	t.Cleanup(func() { _ = m.eng.Close() })
	if len(seen) != 1 || seen[0] != m.eng {
		t.Fatalf("New's engine: seen %d", len(seen))
	}

	seen = nil
	p := New(Config{
		Theme: "tokyo-night", Workspace: t.TempDir(), Provider: agent.CursorProvider(),
		NewSession: func(agent.Provider) agent.Session { return NewStub() }, OnEngine: hook,
	})
	owner = p.owner
	if len(seen) != 0 || !p.pickingProvider {
		t.Fatalf("setup: a provider picker with no engine yet (seen %d)", len(seen))
	}
	tm, _ := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	out := tm.(Model)
	t.Cleanup(func() { _ = out.eng.Close() })
	if len(seen) != 1 || seen[0] != out.eng {
		t.Fatalf("the picker's engine: seen %d", len(seen))
	}
}
