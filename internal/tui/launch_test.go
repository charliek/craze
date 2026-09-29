package tui

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/sessions"
)

// The launch flow's model (plan 030 §3.5, launch.go): a TUI whose session is
// spawned rather than built — Config.NewBackend and Config.LoadBackend, the
// backend twins of NewSession and LoadSession. Its frame is up before any
// session, in the starting state; Init spawns a session it knows, a picker's
// choice spawns one it does not, and the answer is adopted and started as a
// Config.Backend is. A failure is the start failing, but for a refusal of a
// picker's choice, which puts the picker back.

// spawnCall is one call the model made of a launch closure.
type spawnCall struct {
	provider string
	explicit bool
	row      *sessions.Row
}

// spawnRec records a launch Config's calls and answers each with answer's
// backend and error.
type spawnRec struct {
	mu     sync.Mutex
	calls  []spawnCall
	answer func() (backend.Backend, error)
}

func (r *spawnRec) newBackend(p agent.Provider, explicit bool) (backend.Backend, error) {
	r.mu.Lock()
	r.calls = append(r.calls, spawnCall{provider: p.Name(), explicit: explicit})
	r.mu.Unlock()
	return r.answer()
}

func (r *spawnRec) loadBackend(p agent.Provider, row sessions.Row) (backend.Backend, error) {
	r.mu.Lock()
	r.calls = append(r.calls, spawnCall{provider: p.Name(), row: &row})
	r.mu.Unlock()
	return r.answer()
}

func (r *spawnRec) made() []spawnCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]spawnCall(nil), r.calls...)
}

// closeCount is a backend that counts its closes.
type closeCount struct {
	backend.Backend
	closed atomic.Int32
}

func (c *closeCount) Close() error {
	c.closed.Add(1)
	return c.Backend.Close()
}

// launchConfig is a launch Config in ws around rec, with every in-process
// piece set too — each counting its use in *used — which the launch must never
// read (Config.launching).
func launchConfig(t *testing.T, ws string, rec *spawnRec, used *int) Config {
	t.Helper()
	isolateSkillsHome(t)
	count := func() { *used++ }
	return Config{
		Theme:        "tokyo-night",
		Workspace:    ws,
		Yolo:         true,
		Provider:     agent.CursorProvider(),
		NewBackend:   rec.newBackend,
		LoadBackend:  rec.loadBackend,
		NewSession:   func(agent.Provider) agent.Session { count(); return NewStub() },
		LoadSession:  func(agent.Provider, sessions.Row) agent.Session { count(); return NewStub() },
		ClaimSession: func(sessions.Row) (string, func(), error) { count(); return "0199-x", func() {}, nil },
		OnEngine:     func(*engine.Engine) { count() },
		SessionIndex: &recIndex{},
	}
}

// sizedModel is New(cfg) sized to 80×24.
func sizedModel(cfg Config) Model {
	tm, _ := New(cfg).Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return tm.(Model)
}

// firstMsg runs cmd, and the first member of every batch it answers, down to
// a message that is not a batch: the command a handler issued ahead of the
// wrapper's tick chain, and ahead of the reader it batches after a start.
func firstMsg(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	for {
		if cmd == nil {
			t.Fatal("no command")
		}
		msg := cmd()
		batch, ok := msg.(tea.BatchMsg)
		if !ok {
			return msg
		}
		if len(batch) == 0 {
			t.Fatal("an empty batch")
		}
		cmd = batch[0]
	}
}

// spawnedBy is the spawnedMsg cmd answers, within a step: a spawn runs off the
// Update, so it is run here on a goroutine of its own and bounded.
func spawnedBy(t *testing.T, cmd tea.Cmd) spawnedMsg {
	t.Helper()
	got := make(chan tea.Msg, 1)
	go func() { got <- firstMsg(t, cmd) }()
	select {
	case msg := <-got:
		sm, ok := msg.(spawnedMsg)
		if !ok {
			t.Fatalf("the command answered %T, want a spawnedMsg", msg)
		}
		return sm
	case <-time.After(10 * time.Second):
		t.Fatal("the spawn's command did not answer")
		return spawnedMsg{}
	}
}

// statusRow1Text is status row 1 as plain text.
func statusRow1Text(m Model) string { return statusText(m.statusRow1()) }

// TestALaunchSpawnsItsKnownSessionAndAdoptsIt: with the provider known, the
// model builds nothing — no picker, no session, no in-process piece used —
// and its frame is the starting state; Init spawns the session once, a new
// one of that provider (not an explicit picker choice), and the backend it
// answers is adopted as the model's own, owned, read, and started as Init
// starts one: its Start is the start, and the session comes up.
func TestALaunchSpawnsItsKnownSessionAndAdoptsIt(t *testing.T) {
	ws := t.TempDir()
	b := grokBackend(t, ws)
	rec := &spawnRec{answer: func() (backend.Backend, error) { return b, nil }}
	used := 0
	cfg := launchConfig(t, ws, rec, &used)
	cfg.Provider, cfg.ProviderLocked = agent.GrokProvider(), true
	m := sizedModel(cfg)
	switch {
	case m.picking() || m.dialog != dialogNone:
		t.Fatalf("a picker is up (dialog %v)", m.dialog)
	case m.eng != nil || m.owner.current() != nil || m.reading:
		t.Fatalf("the model holds a session before its spawn: %T, reading %v", m.eng, m.reading)
	case !strings.Contains(statusRow1Text(m), "starting…"):
		t.Fatalf("status row 1 while spawning: %q", statusRow1Text(m))
	}
	msg := spawnedBy(t, m.Init())
	if calls := rec.made(); len(calls) != 1 || calls[0] != (spawnCall{provider: "grok"}) {
		t.Fatalf("the spawns made: %+v, want one new grok session", calls)
	}
	tm, cmd := m.Update(msg)
	m = tm.(Model)
	switch {
	case m.eng != b || m.owner.current() != b:
		t.Fatalf("the spawned backend was not adopted: eng %T, owner %T", m.eng, m.owner.current())
	case !m.reading || m.spawnWaiting != 0:
		t.Fatalf("after the adopt: reading %v, waiting %d", m.reading, m.spawnWaiting)
	}
	started, ok := firstMsg(t, cmd).(startedMsg)
	if !ok || started.eng != b {
		t.Fatalf("the adopt's first command answered %T, want its backend's start", started)
	}
	// startedLikeInit's order, with the start the adopt itself issued.
	tm, _ = applyPending(t, m).Update(started)
	m = tm.(Model)
	if !m.started || m.startErr != nil || m.snap.Provider.Name != "grok" {
		t.Fatalf("the adopted session did not come up: started %v, err %v, provider %q", m.started, m.startErr, m.snap.Provider.Name)
	}
	if used != 0 {
		t.Fatalf("the launch used %d of the in-process path's pieces", used)
	}
}

// TestAPickersChoiceIsSpawned: with the provider not known, the picker is up
// and Init spawns nothing; its choice is spawned — Enter on a row an explicit
// choice, Esc the default and not one — and a resume picker's choice is spawned
// as a load of its row, never claimed here, the model restoring meanwhile.
func TestAPickersChoiceIsSpawned(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []tea.KeyMsg
		want spawnCall
	}{
		{"enter on a row", []tea.KeyMsg{{Type: tea.KeyDown}, enter()}, spawnCall{provider: "grok", explicit: true}},
		{"esc", []tea.KeyMsg{{Type: tea.KeyEsc}}, spawnCall{provider: "cursor"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			b := grokBackend(t, ws)
			rec := &spawnRec{answer: func() (backend.Backend, error) { return b, nil }}
			used := 0
			m := sizedModel(launchConfig(t, ws, rec, &used))
			if !m.pickingProvider || m.Init() != nil {
				t.Fatalf("picking %v: the provider picker must be up, and Init spawn nothing", m.pickingProvider)
			}
			var cmd tea.Cmd
			for _, k := range tc.keys {
				var tm tea.Model
				tm, cmd = m.Update(k)
				m = tm.(Model)
			}
			if m.picking() || m.eng != nil || !strings.Contains(statusRow1Text(m), "starting…") {
				t.Fatalf("after the choice: picking %v, eng %T, row %q", m.picking(), m.eng, statusRow1Text(m))
			}
			msg := spawnedBy(t, cmd)
			if calls := rec.made(); len(calls) != 1 || calls[0] != tc.want {
				t.Fatalf("the spawns made: %+v, want %+v", calls, tc.want)
			}
			m = deliver(t, m, msg)
			if m.eng != b || used != 0 {
				t.Fatalf("adopted %T; in-process pieces used %d", m.eng, used)
			}
		})
	}

	t.Run("resume", func(t *testing.T) {
		ws := t.TempDir()
		b := grokBackend(t, ws)
		rec := &spawnRec{answer: func() (backend.Backend, error) { return b, nil }}
		used := 0
		cfg := launchConfig(t, ws, rec, &used)
		row := sessions.Row{SessionID: "s-1", Provider: "grok", CWD: ws, Title: "a row", CrazeID: "0199-a", UpdatedAt: time.Now()}
		cfg.Resume = []sessions.Row{row}
		m := sizedModel(cfg)
		if !m.pickingResume || m.Init() != nil {
			t.Fatal("the resume picker must be up, and Init spawn nothing")
		}
		tm, cmd := m.Update(enter())
		m = tm.(Model)
		if m.picking() || !m.replaying || !strings.Contains(statusRow1Text(m), "restoring…") {
			t.Fatalf("after the choice: picking %v, replaying %v, row %q", m.picking(), m.replaying, statusRow1Text(m))
		}
		msg := spawnedBy(t, cmd)
		calls := rec.made()
		if len(calls) != 1 || calls[0].provider != "grok" || calls[0].row == nil || *calls[0].row != row {
			t.Fatalf("the spawns made: %+v, want the row's load", calls)
		}
		m = deliver(t, m, msg)
		if m.eng != b || used != 0 {
			t.Fatalf("adopted %T; in-process pieces used %d (the claim is the host's)", m.eng, used)
		}
	})
}

// TestContinueIsSpawnedFromInit: --continue's row (Config.Continue) is Init's
// spawn, a load of that row, with the model restoring until the load's replay
// ends — the in-process --continue's starting state.
func TestContinueIsSpawnedFromInit(t *testing.T) {
	ws := t.TempDir()
	b := grokBackend(t, ws)
	rec := &spawnRec{answer: func() (backend.Backend, error) { return b, nil }}
	used := 0
	cfg := launchConfig(t, ws, rec, &used)
	row := sessions.Row{SessionID: "s-2", Provider: "grok", CWD: ws, CrazeID: "0199-b"}
	cfg.Provider, cfg.ProviderLocked, cfg.Loading, cfg.Continue = agent.GrokProvider(), true, true, &row
	m := sizedModel(cfg)
	if m.picking() || !strings.Contains(statusRow1Text(m), "restoring…") {
		t.Fatalf("picking %v, row %q", m.picking(), statusRow1Text(m))
	}
	msg := spawnedBy(t, m.Init())
	calls := rec.made()
	if len(calls) != 1 || calls[0].provider != "grok" || calls[0].row == nil || *calls[0].row != row {
		t.Fatalf("the spawns made: %+v, want --continue's row loaded", calls)
	}
	m = deliver(t, m, msg)
	if m.eng != b || !m.replaying {
		t.Fatalf("adopted %T, replaying %v", m.eng, m.replaying)
	}
}

// TestASpawnFailureIsAStartFailure: a spawn that found no session is the
// session's start failing — the error row with the spawn's own words (which
// name the opt-out), the failed status, the failure the run exits with — and
// no picker comes back for it, even for a picker's choice. A refusal of a
// picker's choice (*Refusal) is that picker's error row instead, without
// craze's prefix, the picker up for another choice; with no picker — Init's
// own spawn, --continue's — a refusal is a start failure too.
func TestASpawnFailureIsAStartFailure(t *testing.T) {
	failed := errors.New("craze: the session host exited before it was ready (exit status 1); its log: /h/x.log; CRAZE_DETACH=0 runs sessions inside craze instead")
	refused := &Refusal{Err: errors.New("craze: the session index is busy — try again")}
	for _, tc := range []struct {
		name   string
		from   string // "init", "provider" or "resume"
		err    error
		repick bool
	}{
		{"init", "init", failed, false},
		{"provider picker", "provider", failed, false},
		{"resume picker", "resume", failed, false},
		{"init refused", "init", refused, false},
		{"provider picker refused", "provider", refused, true},
		{"resume picker refused", "resume", refused, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			rec := &spawnRec{answer: func() (backend.Backend, error) { return nil, tc.err }}
			used := 0
			cfg := launchConfig(t, ws, rec, &used)
			row := sessions.Row{SessionID: "s-3", Provider: "cursor", CWD: ws, Title: "t", UpdatedAt: time.Now()}
			switch tc.from {
			case "init":
				cfg.ProviderLocked = true
			case "resume":
				cfg.Resume = []sessions.Row{row}
			}
			m := sizedModel(cfg)
			cmd := m.Init()
			if tc.from != "init" {
				var tm tea.Model
				tm, cmd = m.Update(enter())
				m = tm.(Model)
			}
			m = deliver(t, m, spawnedBy(t, cmd))
			// Wide enough that no row the failure draws is wrapped.
			m = deliver(t, m, tea.WindowSizeMsg{Width: 400, Height: 24})
			view := plainView(m)
			if tc.repick {
				want := "the session index is busy — try again"
				errRow := m.providerErr
				if tc.from == "resume" {
					errRow = m.resumeErr
				}
				switch {
				case !m.picking() || m.startErr != nil || m.status == statusError:
					t.Fatalf("a refused choice: picking %v, startErr %v, status %v", m.picking(), m.startErr, m.status)
				case errRow != want || !strings.Contains(view, want):
					t.Fatalf("the picker's error row %q, want %q:\n%s", errRow, want, view)
				case tc.from == "resume" && m.replaying:
					t.Fatal("the resume picker is back, and the model still restoring")
				}
				// The picker takes another choice, which is spawned again.
				tm, cmd := m.Update(enter())
				m = tm.(Model)
				spawnedBy(t, cmd)
				if n := len(rec.made()); n != 2 {
					t.Fatalf("%d spawns, want the second choice spawned", n)
				}
				return
			}
			switch {
			case m.picking():
				t.Fatalf("a failed spawn brought a picker back (dialog %v)", m.dialog)
			case !errors.Is(m.startErr, tc.err) || m.status != statusError || m.err != tc.err.Error():
				t.Fatalf("startErr %v, status %v, err %q: want the spawn's failure", m.startErr, m.status, m.err)
			case !strings.Contains(strings.Join(strings.Fields(view), " "), tc.err.Error()):
				t.Fatalf("the failure is not on screen as it was said:\n%s", view)
			case m.eng != nil:
				t.Fatalf("a failed spawn left a backend %T", m.eng)
			}
		})
	}
}

// TestASpawnAnsweredTooLateIsClosed: a spawn whose answer lands once the model
// is quitting — or answers an attempt it no longer waits for — is never
// adopted, and the backend it carries is closed, off the Update.
func TestASpawnAnsweredTooLateIsClosed(t *testing.T) {
	for _, quit := range []bool{true, false} {
		ws := t.TempDir()
		b := &closeCount{Backend: grokBackend(t, ws)}
		rec := &spawnRec{answer: func() (backend.Backend, error) { return b, nil }}
		used := 0
		cfg := launchConfig(t, ws, rec, &used)
		cfg.ProviderLocked = true
		m := sizedModel(cfg)
		msg := spawnedBy(t, m.Init())
		if quit {
			tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
			m = tm.(Model)
		} else {
			msg.attempt++
		}
		tm, cmd := m.Update(msg)
		m = tm.(Model)
		if m.eng != nil || m.owner.current() != nil {
			t.Fatalf("quit %v: the late backend was adopted", quit)
		}
		if cmd == nil {
			t.Fatalf("quit %v: nothing closes the late backend", quit)
		}
		cmd()
		if n := b.closed.Load(); n != 1 {
			t.Fatalf("quit %v: the late backend closed %d times, want once", quit, n)
		}
	}
}

// TestKeysWhileSpawningNeedNoSession: until its session is adopted the model
// has no backend, and nothing the user can press then reaches for one — the
// composer takes text and keeps it on Enter (the session is not up), the
// mode key, Esc, the tasks and theme keys and a builtin are refused or
// answered locally — and the spawn it waits for is still the one adopted.
func TestKeysWhileSpawningNeedNoSession(t *testing.T) {
	ws := t.TempDir()
	b := grokBackend(t, ws)
	rec := &spawnRec{answer: func() (backend.Backend, error) { return b, nil }}
	used := 0
	cfg := launchConfig(t, ws, rec, &used)
	cfg.ProviderLocked = true
	m := sizedModel(cfg)
	cmd := m.Init()
	keys := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("hello")}, enter(), {Type: tea.KeyShiftTab}, {Type: tea.KeyEsc},
		{Type: tea.KeyCtrlT}, {Type: tea.KeyCtrlT}, {Type: tea.KeyCtrlO}, {Type: tea.KeyUp}, {Type: tea.KeyDown},
	}
	for _, k := range keys {
		tm, _ := m.Update(k)
		m = tm.(Model)
		_ = plainView(m)
	}
	if got := m.input.Value(); got != "hello" {
		t.Fatalf("the composer holds %q, want the draft kept", got)
	}
	m.input.SetValue("/model")
	tm, _ := m.Update(enter())
	m = tm.(Model)
	if m.eng != nil || m.quitting {
		t.Fatalf("eng %T, quitting %v", m.eng, m.quitting)
	}
	m = deliver(t, m, spawnedBy(t, cmd))
	if m.eng != b {
		t.Fatalf("adopted %T after the keys", m.eng)
	}
}

// TestAViewerSpawnsNothing: a viewer's Config is cleared of the launch as of
// every other host-owned piece (Config.viewing): its Backend is its session.
func TestAViewerSpawnsNothing(t *testing.T) {
	ws := t.TempDir()
	rec := &spawnRec{answer: func() (backend.Backend, error) { return nil, errors.New("spawned") }}
	used := 0
	cfg := launchConfig(t, ws, rec, &used)
	b := grokBackend(t, ws)
	cfg.Backend, cfg.Viewer = b, true
	row := sessions.Row{SessionID: "s-4", Provider: "grok", CWD: ws}
	cfg.Continue = &row
	m := sizedModel(cfg)
	if m.launch() || m.spawnWaiting != 0 || m.eng != b {
		t.Fatalf("a viewer launches: launch %v, waiting %d, eng %T", m.launch(), m.spawnWaiting, m.eng)
	}
	if _, ok := firstMsg(t, m.Init()).(startedMsg); !ok {
		t.Fatal("a viewer's Init does not start its backend")
	}
	if len(rec.made()) != 0 || used != 0 {
		t.Fatalf("a viewer spawned %d, used %d", len(rec.made()), used)
	}
}
