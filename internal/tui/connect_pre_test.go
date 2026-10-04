package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/roster"
)

// The pre-session connect dialog (plan 036 §3.6, A7) as unit tests, every one
// through Model.Update and View: opened from the startup picker and from the
// session list's /provider on native needing setup, and every way out of it
// back to where it came from — Esc, a click outside, a save that succeeds,
// fails or does not answer, a sign-in that finishes (its notice a step of its
// own), fails or is declined, a late answer, a quit — with native's state
// recomputed from the store, no backend call and no transcript change, the
// list opened from a live session included. The store is a fixture directory
// under a home of its own with nothing funded — a fresh CRAZE_HOME, Plan 034's
// case — and the pickers' states are judged from it the way the check judges
// native (Load, then StartModel): the TUI's seams on the same directory, so a
// key the dialog saves is a key the next answer sees. Every test that opens
// the dialog closes the sign-in log before its temp directories go (034 X79).

// preFixture is a native directory with nativePickerTable (catalog off) and
// Delta — a provider with no model, whose key funds nothing — and, with
// chatgpt, the ChatGPT plan's provider; with registered, a registration whose
// account did not grant plan usage (a re-login, which begins with no network
// and funds nothing). Its environment answers HOME alone: no provider has a
// key.
func preFixture(t *testing.T, chatgpt, registered bool) (dir string, getenv func(string) string) {
	t.Helper()
	home := t.TempDir()
	dir = filepath.Join(home, ".craze", "native")
	table := nativePickerTable()
	table.Providers["delta"] = modeltable.Provider{Name: "Delta", Driver: modeltable.DriverOpenAICompat,
		BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"PICKER_DELTA_KEY"}}
	if chatgpt {
		table.Providers["chatgpt"] = modeltable.Provider{Name: "ChatGPT plan", Driver: modeltable.DriverChatGPT}
	}
	if err := modeltable.Save(dir, table); err != nil {
		t.Fatal(err)
	}
	if registered {
		if err := os.MkdirAll(chatgptauth.AuthDir(dir), 0o700); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(chatgptauth.Client{ClientID: "oaiapp_fixture000000000001", Subject: "user-fixture", Email: signInEmail})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(chatgptauth.ClientFile(dir), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
}

// preNative is native's state in dir, judged as the check judges it (plan 036
// §3.1): ready once extra says so (a stand-in sign-in, which writes no
// tokens), else by modeltable.Load and StartModel with no memory.
func preNative(dir string, getenv func(string) string, extra func() bool) ProviderAvail {
	if extra != nil && extra() {
		return ProviderAvail{ID: "native", State: AvailReady}
	}
	table, err := modeltable.Load(dir)
	if err != nil {
		return ProviderAvail{ID: "native", State: AvailUnavailable, Reason: "providers.toml could not be read", Fix: `run "craze auth list" for the error`}
	}
	if _, _, err := table.StartModel(nil, getenv); err != nil {
		return ProviderAvail{ID: "native", State: AvailNeedsSetup, Reason: availNativeReason, Fix: availNativeFix}
	}
	return ProviderAvail{ID: "native", State: AvailReady}
}

// preStates is the pickers' answer over dir: cursor and grok ready, native as
// the store says (preNative).
func preStates(dir string, getenv func(string) string, extra func() bool) func() []ProviderAvail {
	return func() []ProviderAvail {
		return []ProviderAvail{{ID: "cursor", State: AvailReady}, {ID: "grok", State: AvailReady}, preNative(dir, getenv, extra)}
	}
}

// prePicker is the startup picker at 100x30 over dir — def its default, the
// built-in rows, sessions built logged — its states preStates, the first
// answer taken.
func prePicker(t *testing.T, def agent.Provider, dir string, getenv func(string) string, extra func() bool) (Model, *builtLog) {
	t.Helper()
	log := &builtLog{}
	cfg := availPickerConfig(t, def, preStates(dir, getenv, extra), nil, log.build)
	cfg.NativeDir, cfg.Getenv = dir, getenv
	m := availPicker(t, cfg, 100, 30)
	t.Cleanup(func() { _ = m.signIns.closeLog() })
	if m.providerChoice(agent.NativeProvider()).a.State != AvailNeedsSetup {
		t.Fatal("fixture: native does not need setup")
	}
	return m, log
}

// toNative moves the picker's cursor onto native's row.
func toNative(t *testing.T, m Model) Model {
	t.Helper()
	for range m.providers {
		if m.providers[m.providerCursor].Name() == "native" {
			return m
		}
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	t.Fatal("the picker has no native row")
	return m
}

// toListNative moves the selection in /provider's popup onto native's
// candidate, as far as its candidates go: a caller that finds the dialog
// unopened says so.
func toListNative(m Model) Model {
	for range 4 {
		if it, ok := m.sessList.in.cmd.selected(); ok && it.Value == "provider:native" {
			return m
		}
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyUp})
	}
	return m
}

// countingBackend is a backend that records every call the TUI makes of it —
// every one but the stream's read and the facts a frame reads (Info, the
// client id, the epoch) and the session's own start and close — in its
// callLog, answering each from the backend behind it.
type countingBackend struct {
	backend.Backend
	callLog
}

func (c *countingBackend) engine() *engine.Engine { return engineBehind(c.Backend) }

func (c *countingBackend) Stop(ctx context.Context, cmd engine.Command) error {
	c.add("Stop")
	return c.Backend.Stop(ctx, cmd)
}

func (c *countingBackend) Submit(ctx context.Context, cmd engine.Command, text string, mode engine.SubmitMode, fromRow string) (engine.SubmitResult, error) {
	c.add("Submit")
	return c.Backend.Submit(ctx, cmd, text, mode, fromRow)
}

func (c *countingBackend) Answer(ctx context.Context, cmd engine.Command, id string, a agent.AskAnswer) error {
	c.add("Answer")
	return c.Backend.Answer(ctx, cmd, id, a)
}

func (c *countingBackend) Unqueue(ctx context.Context, cmd engine.Command, id string) (agent.QueuedPrompt, error) {
	c.add("Unqueue")
	return c.Backend.Unqueue(ctx, cmd, id)
}

func (c *countingBackend) EditQueued(ctx context.Context, cmd engine.Command, id, text string, v *int) error {
	c.add("EditQueued")
	return c.Backend.EditQueued(ctx, cmd, id, text, v)
}

func (c *countingBackend) ClearQueue(ctx context.Context, cmd engine.Command) ([]agent.QueuedPrompt, error) {
	c.add("ClearQueue")
	return c.Backend.ClearQueue(ctx, cmd)
}

func (c *countingBackend) Disarm(ctx context.Context, cmd engine.Command) error {
	c.add("Disarm")
	return c.Backend.Disarm(ctx, cmd)
}

func (c *countingBackend) Interject(ctx context.Context, cmd engine.Command, text string) error {
	c.add("Interject")
	return c.Backend.Interject(ctx, cmd, text)
}

func (c *countingBackend) SetTitle(ctx context.Context, cmd engine.Command, title string) error {
	c.add("SetTitle")
	return c.Backend.SetTitle(ctx, cmd, title)
}

func (c *countingBackend) Set(ctx context.Context, cmd engine.Command, s engine.Setting) (engine.SetResult, error) {
	c.add("Set")
	return c.Backend.Set(ctx, cmd, s)
}

func (c *countingBackend) Cancel(ctx context.Context, cmd engine.Command, turn string) (engine.CancelResult, error) {
	c.add("Cancel")
	return c.Backend.Cancel(ctx, cmd, turn)
}

func (c *countingBackend) CancelSubagent(ctx context.Context, cmd engine.Command, id string) error {
	c.add("CancelSubagent")
	return c.Backend.CancelSubagent(ctx, cmd, id)
}

func (c *countingBackend) Ask(ctx context.Context, id string) (agent.AskRecord, bool, error) {
	c.add("Ask")
	return c.Backend.Ask(ctx, id)
}

func (c *countingBackend) Settings(ctx context.Context) (backend.Settings, error) {
	c.add("Settings")
	return c.Backend.Settings(ctx)
}

func (c *countingBackend) RefreshModels(ctx context.Context, dir string) (agent.ModelsRefresh, error) {
	c.add("RefreshModels")
	return c.Backend.RefreshModels(ctx, dir)
}

func (c *countingBackend) LastTurn(ctx context.Context) (*engine.LastTurn, error) {
	c.add("LastTurn")
	return c.Backend.LastTurn(ctx)
}

// preSessions is a session list that can start sessions and says which
// providers they could run: preStates over the fixture, read afresh each
// call, so an answer sees a key the dialog saved.
type preSessions struct {
	*startSessions
	avail func() []ProviderAvail
}

var _ ProviderAvailabilitySource = (*preSessions)(nil)

func (p *preSessions) ProviderAvailability() []ProviderAvail { return p.avail() }

// preList is a started in-process session — a live one, its backend behind a
// countingBackend — whose session list (preSessions over dir) is open on
// richSnapshot, with its recent directories and its own first availability
// answer taken; the TUI's native seams are on dir.
func preList(t *testing.T, dir string, getenv func(string) string, extra func() bool) (Model, *countingBackend) {
	t.Helper()
	m, fs, _ := newSessModel(t, 100, 30)
	t.Cleanup(func() { _ = m.signIns.closeLog() })
	m.sessions = &preSessions{startSessions: fs, avail: preStates(dir, getenv, extra)}
	m.nativeDir, m.nativeEnv = dir, getenv
	cb := &countingBackend{Backend: m.eng}
	m.eng = cb
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyLeft})
	if !m.sessList.open || !m.sessList.in.on {
		t.Fatalf("← did not open a list with an input:\n%s", plainView(m))
	}
	m = listSnap(t, m, richSnapshot(m.sessList.home, m.hereKey()))
	m = listRecents(t, m, fs)
	m = takeOpenRead(t, m, cmd)
	if c, ok := m.sessProviderChoice(agent.NativeProvider()); !ok || c.a.State != AvailNeedsSetup {
		t.Fatal("fixture: the list's native does not need setup")
	}
	return m, cb
}

// listShown is what the list showed as the dialog closed it: what the dialog
// must give back.
type listShown struct {
	value  string
	cursor int
	sel    sessKey
	pick   sessPick
}

func listShownOf(m Model) listShown {
	v, cur := inputOf(m)
	return listShown{value: v, cursor: cur, sel: m.sessList.sel, pick: m.sessPick}
}

// preOpen opens the pre-session dialog from origin — the picker: Enter on
// native's row; the list: `/provider ` typed, Enter on native in its popup —
// and answers the providers' read its opening returned, as the read's plain
// command answers it. It answers what the list showed (zero for the picker).
func preOpen(t *testing.T, origin connectReturn, m Model) (Model, listShown) {
	t.Helper()
	var shown listShown
	var cmd tea.Cmd
	if origin == returnPicker {
		m = toNative(t, m)
		m, cmd = press(m, enter())
	} else {
		m = toListNative(providersLoaded(t, clearInput(t, m), "/provider "))
		shown = listShownOf(m)
		m, cmd = press(m, enter())
	}
	if !m.preConnectOpen() || m.cdlg.returnTo != origin || m.sessList.open {
		t.Fatalf("native: connect dialog %v (back to %v), list open %v — want the pre-session dialog going back to %v",
			m.preConnectOpen(), m.cdlg.returnTo, m.sessList.open, origin)
	}
	if !strings.Contains(plainView(m), connectDialogTitle) {
		t.Fatalf("the dialog is not on screen:\n%s", plainView(m))
	}
	msg, ok := runWatched(t, mustCmd(t, cmd, "readConnectProviders")).(connectLoadedMsg)
	if !ok {
		t.Fatalf("the read answered %T", msg)
	}
	m = applyMsg(t, m, msg)
	if !m.cdlg.loaded || m.cdlg.loadErr != "" {
		t.Fatalf("the providers' read: loaded %v, error %q", m.cdlg.loaded, m.cdlg.loadErr)
	}
	return m, shown
}

// preSelect moves step one's selection onto the provider id.
func preSelect(t *testing.T, m Model, id string) Model {
	t.Helper()
	for range m.cdlg.providers {
		if p, ok := m.cdlg.provider(); ok && p.ID == id {
			return m
		}
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyDown})
	}
	t.Fatalf("step one has no %s", id)
	return m
}

// preSubmit stores connectCanary for the provider id from step one: its key
// step opened, the canary pasted, Enter — answering the save's command.
func preSubmit(t *testing.T, m Model, id string) (Model, tea.Cmd) {
	t.Helper()
	m = pressKey(t, preSelect(t, m, id), tea.KeyEnter)
	m, _ = press(m, pasteKey(connectCanary))
	return press(m, enter())
}

// preQuiet fails the test where the pre-session dialog reached a session: a
// call of the backend, a gated call open or a message held behind one, or a
// change to the transcript.
func preQuiet(t *testing.T, how string, m Model, cb *countingBackend, transcript string) {
	t.Helper()
	if cb != nil {
		if made := cb.seen(); len(made) != 0 {
			t.Fatalf("%s: the backend was called: %v", how, made)
		}
	}
	if m.gate != nil || len(m.held) != 0 {
		t.Fatalf("%s: a gated call is open (%v) or messages are held (%v)", how, m.gate != nil, heldKinds(m))
	}
	if got := transcriptText(m); got != transcript {
		t.Fatalf("%s: the transcript changed:\n%s\nwas:\n%s", how, got, transcript)
	}
}

// preBack checks m is back at origin with back said there, its states asked
// again and the answer taken — native recomputed from the store — and
// answers the model with the answer applied. shown is what the list showed.
func preBack(t *testing.T, how string, origin connectReturn, m Model, cmd tea.Cmd, back connectBack, shown listShown) Model {
	t.Helper()
	if m.preConnectOpen() || m.cdlg.gen != 0 || m.cdlg.key.Value() != "" {
		t.Fatalf("%s: the dialog is still up (%v) or its state lives on (gen %d)", how, m.preConnectOpen(), m.cdlg.gen)
	}
	if origin == returnPicker {
		if !m.pickingProvider || m.dialog != dialogProvider || m.eng != nil || m.providerErr != "" {
			t.Fatalf("%s: picking %v, dialog %v, engine %v, error row %q — want the picker back, nothing started",
				how, m.pickingProvider, m.dialog, m.eng != nil, m.providerErr)
		}
		if m.providerBack != back {
			t.Fatalf("%s: the picker says %+v, want %+v", how, m.providerBack, back)
		}
		if back.text != "" {
			inner, budget := m.lay.Dialog.W-dialogBorder, m.lay.Dialog.H-dialogBorder
			first := m.sessNoteStyle(back.kind).Render(clampWidth(dialogWrap(back.text, inner)[0], inner))
			if !slices.Contains(m.providerDialogBody(inner, budget), first) {
				t.Fatalf("%s: the picker does not draw %q in its colour:\n%s", how, back.text, plainView(m))
			}
		}
		msg, ok := runWatched(t, mustCmd(t, cmd, "providerAvailCmd")).(providerAvailMsg)
		if !ok || msg.seq != m.availSeq {
			t.Fatalf("%s: the picker's states were not asked again (%T)", how, msg)
		}
		return applyMsg(t, m, msg)
	}
	if !m.sessList.open || !m.sessList.in.on || m.dialog != dialogNone {
		t.Fatalf("%s: list open %v, dialog %v — want the list back", how, m.sessList.open, m.dialog)
	}
	if got := listShownOf(m); got.value != shown.value || got.cursor != shown.cursor || got.sel != shown.sel ||
		!reflect.DeepEqual(got.pick, shown.pick) {
		t.Fatalf("%s: the list came back as %+v, want it as it was: %+v", how, got, shown)
	}
	if m.sessList.note != back.text || (back.text != "" && m.sessList.noteKind != back.kind) {
		t.Fatalf("%s: the hint line says %q (%v), want %+v", how, m.sessList.note, m.sessList.noteKind, back)
	}
	if back.text != "" && !strings.Contains(plainView(m), back.text[:min(len(back.text), 30)]) {
		t.Fatalf("%s: the note is not on the hint line:\n%s", how, plainView(m))
	}
	msg, ok := runWatched(t, mustCmd(t, cmd, "readSessAvail")).(sessAvailMsg)
	if !ok || msg.gen != m.sessList.gen {
		t.Fatalf("%s: the reopened list did not read its states afresh (%T)", how, msg)
	}
	return applyMsg(t, m, msg)
}

// nativeState is native's state as origin's picker now holds it.
func nativeState(origin connectReturn, m Model) AvailState {
	if origin == returnPicker {
		return m.providerChoice(agent.NativeProvider()).a.State
	}
	c, _ := m.sessProviderChoice(agent.NativeProvider())
	return c.a.State
}

// preOrigins are the two ways in.
var preOrigins = []struct {
	name   string
	origin connectReturn
}{{"from the picker", returnPicker}, {"from the list", returnList}}

// preModel is origin's model over a fresh fixture: the picker with grok its
// default, or the list over a live session (with its counting backend).
func preModel(t *testing.T, origin connectReturn, chatgpt bool, extra func() bool) (Model, string, *countingBackend, *builtLog) {
	t.Helper()
	dir, getenv := preFixture(t, chatgpt, false)
	if origin == returnPicker {
		m, log := prePicker(t, agent.GrokProvider(), dir, getenv, extra)
		return m, dir, nil, log
	}
	m, cb := preList(t, dir, getenv, extra)
	return m, dir, cb, nil
}

// TestPreConnectOpensOnNative (A5's native bullet): on native needing setup,
// Enter on its row, Esc when it is the default and a click outside the box
// when it is the default each open the pre-session connect dialog over the
// picker — the picker still up under it, nothing built, no error row — and
// the selected native row's fix line is the TUI's (X23).
func TestPreConnectOpensOnNative(t *testing.T) {
	opened := func(t *testing.T, how string, m Model, log *builtLog) {
		t.Helper()
		if !m.preConnectOpen() || m.cdlg.returnTo != returnPicker || !m.pickingProvider || m.providerErr != "" || len(log.got()) != 0 {
			t.Fatalf("%s: connect dialog %v (back to %v), picking %v, error row %q, built %v — want the dialog over the picker",
				how, m.preConnectOpen(), m.cdlg.returnTo, m.pickingProvider, m.providerErr, log.got())
		}
		if view := plainView(m); !strings.Contains(view, connectDialogTitle) || !strings.Contains(view, "Reading the providers…") {
			t.Fatalf("%s: the dialog is not on screen:\n%s", how, view)
		}
	}
	dir, getenv := preFixture(t, false, false)
	m, log := prePicker(t, agent.GrokProvider(), dir, getenv, nil)
	onNative := toNative(t, m)
	if view := plainView(onNative); !strings.Contains(view, `pick it to connect one, or run "craze auth login"`) {
		t.Fatalf("native's fix line is not the TUI's:\n%s", view)
	}
	next, _ := press(onNative, enter())
	opened(t, "Enter on native", next, log)

	def, defLog := prePicker(t, agent.NativeProvider(), dir, getenv, nil)
	def, _ = press(def, tea.KeyMsg{Type: tea.KeyUp}) // off the default
	next, _ = press(def, tea.KeyMsg{Type: tea.KeyEsc})
	opened(t, "Esc to the native default", next, defLog)
	tm, _ := def.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: 0})
	opened(t, "a click outside the box to the native default", tm.(Model), defLog)
}

// TestPreConnectPutsTheCursorOnNative (plan 036 decision 5, X21): the dialog
// opened by Esc to the native default, the cursor on grok, comes back with
// the cursor where it was until native's state is in; the answer that says
// native is ready now puts the cursor on it, and Enter starts it — nothing
// starts on its own. One that says native still needs setup leaves the cursor
// on grok, and so does an answer that lands after the user moved the cursor.
func TestPreConnectPutsTheCursorOnNative(t *testing.T) {
	// back opens the dialog by Esc to the native default with the cursor
	// on grok, stores a key for id, and answers the model back on the
	// picker and the request for its states.
	back := func(t *testing.T, id string) (Model, tea.Cmd, *builtLog) {
		t.Helper()
		dir, getenv := preFixture(t, false, false)
		m, log := prePicker(t, agent.NativeProvider(), dir, getenv, nil)
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyUp})
		if m.providers[m.providerCursor].Name() != "grok" {
			t.Fatalf("fixture: the cursor is on %s", m.providers[m.providerCursor].Name())
		}
		m, cmd := press(m, tea.KeyMsg{Type: tea.KeyEsc})
		if !m.preConnectOpen() {
			t.Fatal("fixture: Esc to the native default did not open the dialog")
		}
		m = applyMsg(t, m, runWatched(t, mustCmd(t, cmd, "readConnectProviders")))
		m, cmd = preSubmit(t, m, id)
		tm, cmd := m.Update(runWatched(t, mustCmd(t, cmd, "preStampedSave")))
		m = tm.(Model)
		if !m.pickingProvider || m.dialog != dialogProvider || m.providers[m.providerCursor].Name() != "grok" {
			t.Fatalf("back on the picker: picking %v, dialog %v, cursor on %s — want the cursor where it was until the answer",
				m.pickingProvider, m.dialog, m.providers[m.providerCursor].Name())
		}
		return m, mustCmd(t, cmd, "providerAvailCmd"), log
	}
	t.Run("ready", func(t *testing.T) {
		m, ask, log := back(t, "gamma")
		m = applyMsg(t, m, runWatched(t, ask))
		if m.providers[m.providerCursor].Name() != "native" || len(log.got()) != 0 {
			t.Fatalf("the answer: cursor on %s, built %v — want the cursor on native, nothing started",
				m.providers[m.providerCursor].Name(), log.got())
		}
		if !strings.Contains(plainView(m), "> native") {
			t.Fatalf("the frame does not show the cursor on native:\n%s", plainView(m))
		}
		m, _ = press(m, enter())
		if m.pickingProvider || !slices.Equal(log.got(), []string{"native"}) {
			t.Fatalf("Enter: picking %v, built %v — want native started", m.pickingProvider, log.got())
		}
	})
	t.Run("still needing setup", func(t *testing.T) {
		m, ask, log := back(t, "delta")
		m = applyMsg(t, m, runWatched(t, ask))
		if m.providers[m.providerCursor].Name() != "grok" || len(log.got()) != 0 {
			t.Fatalf("the answer: cursor on %s, built %v — want it left on grok", m.providers[m.providerCursor].Name(), log.got())
		}
	})
	t.Run("the cursor moved before the answer", func(t *testing.T) {
		m, ask, _ := back(t, "gamma")
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyUp})
		at := m.providerCursor
		if m.providerBack.text != "" {
			t.Fatal("moving the cursor kept the dialog's note")
		}
		m = applyMsg(t, m, runWatched(t, ask))
		if m.providerCursor != at {
			t.Fatalf("the answer moved the cursor the user had moved (%d → %d)", at, m.providerCursor)
		}
	})
}

// TestPreConnectExits (A7, §3.6's table): from the picker and from the list —
// the list opened from a live session — every way out of the dialog but the
// sign-in's (TestPreConnectSignIn) goes back where it came from: Esc on step
// one or on the key field, unchanged; a click outside, which starts nothing;
// a key saved, `connected <provider>: it can be picked now`, native ready
// once the store says so and not before (a key for Delta funds no model); a
// save the store refuses, or that does not answer in time, with why. Native's
// state is asked again each time and the answer taken; the picker's cursor
// lands on native when it is ready (X21), and Enter starts it; in the list,
// the provider is left as it was and native is chosen again. No exit calls
// the backend, opens a gate or touches the transcript.
func TestPreConnectExits(t *testing.T) {
	for _, o := range preOrigins {
		t.Run(o.name, func(t *testing.T) {
			t.Run("esc on step one", func(t *testing.T) {
				m, _, cb, _ := preModel(t, o.origin, false, nil)
				transcript := transcriptText(m)
				m, shown := preOpen(t, o.origin, m)
				m, cmd := press(m, tea.KeyMsg{Type: tea.KeyEsc})
				preQuiet(t, "Esc on step one", m, cb, transcript)
				m = preBack(t, "Esc on step one", o.origin, m, cmd, connectBack{}, shown)
				if nativeState(o.origin, m) != AvailNeedsSetup {
					t.Fatalf("native recomputed as %q, want needs_setup", nativeState(o.origin, m))
				}
			})
			t.Run("esc on the key field", func(t *testing.T) {
				m, _, cb, _ := preModel(t, o.origin, false, nil)
				transcript := transcriptText(m)
				m, shown := preOpen(t, o.origin, m)
				m = pressKey(t, preSelect(t, m, "gamma"), tea.KeyEnter)
				m, _ = press(m, pasteKey(connectCanary))
				if m.cdlg.step != connectKey || m.cdlg.key.Value() != connectCanary {
					t.Fatal("fixture: the key field does not hold the paste")
				}
				m, cmd := press(m, tea.KeyMsg{Type: tea.KeyEsc})
				connectLeak(t, m, nil)
				preQuiet(t, "Esc on the key field", m, cb, transcript)
				preBack(t, "Esc on the key field", o.origin, m, cmd, connectBack{}, shown)
			})
			t.Run("a click outside", func(t *testing.T) {
				m, _, cb, log := preModel(t, o.origin, false, nil)
				transcript := transcriptText(m)
				m, shown := preOpen(t, o.origin, m)
				tm, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: 0})
				m = tm.(Model)
				preQuiet(t, "a click outside", m, cb, transcript)
				preBack(t, "a click outside", o.origin, m, cmd, connectBack{}, shown)
				if log != nil && len(log.got()) != 0 {
					t.Fatalf("a click outside the dialog started %v", log.got())
				}
			})
			t.Run("a save that succeeds", func(t *testing.T) {
				m, dir, cb, log := preModel(t, o.origin, false, nil)
				transcript := transcriptText(m)
				m, shown := preOpen(t, o.origin, m)
				m, cmd := preSubmit(t, m, "gamma")
				// The dialog stays up, on its save step, the field emptied.
				connectLeak(t, m, nil)
				if !m.preConnectOpen() || m.cdlg.step != connectSaving || m.cdlg.key.Value() != "" {
					t.Fatalf("Enter: dialog %v, step %d — want the save step up, the field empty", m.preConnectOpen(), m.cdlg.step)
				}
				if view := connectView(t, m); !strings.Contains(view, "Gamma API key") || !strings.Contains(view, preSavingText) {
					t.Fatalf("the save step is not on screen:\n%s", view)
				}
				preQuiet(t, "the save under way", m, cb, transcript)
				saved, ok := runWatched(t, mustCmd(t, cmd, "preStampedSave")).(connectSavedMsg)
				if !ok || !saved.pre || saved.gen != m.cdlg.gen || saved.err != "" {
					t.Fatalf("the save answered %+v", saved)
				}
				if got, ok := storedKey(t, dir, "gamma"); !ok || got != connectCanary {
					t.Fatal("gamma's key was not stored")
				}
				m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
				if m.cdlg.step != connectSaving || m.cdlg.key.Value() != "" {
					t.Fatal("a key typed on the save step was taken")
				}
				tm, cmd := m.Update(saved)
				m = tm.(Model)
				connectLeak(t, m, nil)
				preQuiet(t, "a save that succeeds", m, cb, transcript)
				m = preBack(t, "a save that succeeds", o.origin, m, cmd, connectBack{text: "connected Gamma: it can be picked now", kind: sessNoteOK}, shown)
				if nativeState(o.origin, m) != AvailReady {
					t.Fatalf("native recomputed as %q after the save, want ready", nativeState(o.origin, m))
				}
				if o.origin == returnPicker {
					// X21: the cursor is on native, and Enter starts it —
					// nothing started before.
					if m.providers[m.providerCursor].Name() != "native" || len(log.got()) != 0 {
						t.Fatalf("the cursor is on %s, built %v — want it on native, nothing started",
							m.providers[m.providerCursor].Name(), log.got())
					}
					started, _ := press(m, enter())
					if started.pickingProvider || !slices.Equal(log.got(), []string{"native"}) {
						t.Fatalf("Enter on native once ready: picking %v, built %v", started.pickingProvider, log.got())
					}
					return
				}
				// The list: the provider is as it was, and choosing native
				// now takes it.
				if m.sessPick.provSet && m.sessPick.prov.Name() == "native" {
					t.Fatal("the list chose native on its own")
				}
				typed := clearInput(t, m)
				typed, _ = typeList(t, typed, "/provider native")
				typed, _ = press(typed, tea.KeyMsg{Type: tea.KeyEsc})
				typed, _ = press(typed, enter())
				if typed.preConnectOpen() || !typed.sessPick.provSet || typed.sessPick.prov.Name() != "native" {
					t.Fatalf("/provider native once ready: dialog %v, pick %+v", typed.preConnectOpen(), typed.sessPick)
				}
			})
			t.Run("a save that funds no model", func(t *testing.T) {
				m, _, cb, log := preModel(t, o.origin, false, nil)
				transcript := transcriptText(m)
				m, shown := preOpen(t, o.origin, m)
				before := m.providerCursor
				m, cmd := preSubmit(t, m, "delta")
				tm, cmd := m.Update(runWatched(t, mustCmd(t, cmd, "preStampedSave")))
				m = tm.(Model)
				preQuiet(t, "a save that funds no model", m, cb, transcript)
				m = preBack(t, "a save that funds no model", o.origin, m, cmd, connectBack{text: "connected Delta: it can be picked now", kind: sessNoteOK}, shown)
				if nativeState(o.origin, m) != AvailNeedsSetup {
					t.Fatalf("native recomputed as %q, want needs_setup: Delta has no model", nativeState(o.origin, m))
				}
				if o.origin == returnPicker && (m.providerCursor != before || len(log.got()) != 0) {
					t.Fatalf("the cursor moved (%d → %d) or something started (%v)", before, m.providerCursor, log.got())
				}
			})
			t.Run("a save the store refuses", func(t *testing.T) {
				m, dir, cb, _ := preModel(t, o.origin, false, nil)
				transcript := transcriptText(m)
				m, shown := preOpen(t, o.origin, m)
				m = pressKey(t, preSelect(t, m, "gamma"), tea.KeyEnter)
				path := filepath.Join(dir, modeltable.ProvidersFile)
				target := filepath.Join(t.TempDir(), "elsewhere.toml")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				m, _ = press(m, pasteKey(connectCanary))
				m, cmd := press(m, enter())
				tm, cmd := m.Update(runWatched(t, mustCmd(t, cmd, "preStampedSave")))
				m = tm.(Model)
				connectLeak(t, m, nil)
				preQuiet(t, "a save the store refuses", m, cb, transcript)
				want := connectBack{text: "could not connect Gamma: providers.toml is a symlink", kind: sessNoteErr}
				got := m.providerBack
				if o.origin == returnList {
					got = connectBack{text: m.sessList.note, kind: sessNoteErr}
				}
				if !strings.HasPrefix(got.text, want.text) {
					t.Fatalf("the refusal said %q, want it to start %q", got.text, want.text)
				}
				want.text = got.text
				preBack(t, "a save the store refuses", o.origin, m, cmd, want, shown)
			})
			t.Run("a save that does not answer in time", func(t *testing.T) {
				prev := preConnectDeadline
				preConnectDeadline = time.Millisecond
				t.Cleanup(func() { preConnectDeadline = prev })
				m, _, cb, _ := preModel(t, o.origin, false, nil)
				transcript := transcriptText(m)
				m, shown := preOpen(t, o.origin, m)
				m, cmd := preSubmit(t, m, "gamma")
				save := mustCmd(t, cmd, "preStampedSave")
				late := preDeadline(t, cmd)
				tm, cmd := m.Update(late)
				m = tm.(Model)
				preQuiet(t, "the deadline", m, cb, transcript)
				m = preBack(t, "the deadline", o.origin, m, cmd, connectBack{text: "could not connect Gamma: " + preSaveLateText, kind: sessNoteErr}, shown)
				// The store's answer, late: dropped.
				before := preDigest(m)
				m = applyMsg(t, m, runWatched(t, save))
				if after := preDigest(m); after != before {
					t.Fatalf("the save's late answer changed the model:\n%s\nwas:\n%s", after, before)
				}
				preQuiet(t, "the save's late answer", m, cb, transcript)
			})
		})
	}
}

// preDeadline runs the deadline the pre-session dialog's last read or save
// is raced by (preConnectLate, at preConnectDeadline) among cmd's commands,
// and answers its message: the late answer.
func preDeadline(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	msg, ok := runWatched(t, mustCmd(t, cmd, "preConnectLate")).(connectAnswer)
	if !ok {
		t.Fatalf("the deadline answered %T", msg)
	}
	return msg
}

// preDigest is what a late answer must not change: where the TUI is, what
// the picker and the list say, the dialog's opening and step, and the
// transcript.
func preDigest(m Model) string {
	return fmt.Sprintf("dialog=%v picking=%v back=%+v err=%q list=%v note=%q gen=%d step=%d loaded=%v transcript=%q",
		m.dialog, m.pickingProvider, m.providerBack, m.providerErr, m.sessList.open, m.sessList.note,
		m.cdlg.gen, m.cdlg.step, m.cdlg.loaded, transcriptText(m))
}

// TestPreConnectLateAnswers (§3.6's table): every answer the dialog can wait
// for — the providers' read, a save, the sign-in's wait, its finish, the
// notice's record — that lands once the dialog numbered gen has gone is
// dropped, from the picker and from the list; and dropped again by a dialog
// opened since, whose own state it must not touch.
func TestPreConnectLateAnswers(t *testing.T) {
	for _, o := range preOrigins {
		t.Run(o.name, func(t *testing.T) {
			m, _, cb, _ := preModel(t, o.origin, false, nil)
			transcript := transcriptText(m)
			m, _ = preOpen(t, o.origin, m)
			gen := m.cdlg.gen
			// Delta's key funds no model, so native still needs setup once
			// the late save has stored it, and opens the dialog again.
			m, cmd := preSubmit(t, m, "delta")
			save := mustCmd(t, cmd, "preStampedSave")
			m, cmd = press(m, tea.KeyMsg{Type: tea.KeyEsc})
			if o.origin == returnPicker {
				m = applyMsg(t, m, runWatched(t, mustCmd(t, cmd, "providerAvailCmd")))
			} else {
				m = applyMsg(t, m, runWatched(t, mustCmd(t, cmd, "readSessAvail")))
			}
			late := []tea.Msg{
				runWatched(t, save),
				connectLoadedMsg{gen: gen, err: "a read that answered late"},
				signInDoneMsg{signInStamp: signInStamp{gen: gen, run: gen + 1, pre: true}, res: chatgptauth.Result{Email: signInEmail, PlanUsage: true, ShowNotice: true}},
				signInDoneMsg{signInStamp: signInStamp{gen: gen, run: gen + 1, pre: true}, err: chatgptauth.ErrAccessDenied},
				signInFinishedMsg{signInStamp: signInStamp{gen: gen, run: gen + 1, pre: true}, shownGen: m.shownGen, aliases: []string{"chatgpt/x"}},
				preNoticeMarkedMsg{gen: gen},
			}
			before := preDigest(m)
			for _, msg := range late {
				m = applyMsg(t, m, msg)
				if after := preDigest(m); after != before {
					t.Fatalf("a late %T changed the model:\n%s\nwas:\n%s", msg, after, before)
				}
			}
			preQuiet(t, "the late answers", m, cb, transcript)

			// A dialog opened since keeps its own state.
			m, _ = preOpen(t, o.origin, m)
			if m.cdlg.gen == gen {
				t.Fatal("fixture: the dialog opened again has the first one's number")
			}
			again := preDigest(m)
			for _, msg := range late {
				m = applyMsg(t, m, msg)
				if after := preDigest(m); after != again {
					t.Fatalf("the first dialog's late %T changed the second:\n%s\nwas:\n%s", msg, after, again)
				}
			}
			preQuiet(t, "the late answers over a second dialog", m, cb, transcript)

			// The first dialog's save, late, over the second's own save step:
			// told apart by the dialog's number alone, and dropped.
			m, _ = preSubmit(t, m, "delta")
			if m.cdlg.step != connectSaving {
				t.Fatal("fixture: the second dialog is not saving")
			}
			saving := preDigest(m)
			if m = applyMsg(t, m, late[0]); preDigest(m) != saving {
				t.Fatalf("the first dialog's save ended the second's:\n%s\nwas:\n%s", preDigest(m), saving)
			}
		})
	}
}

// TestPreConnectSignIn (A7, §3.6): the ChatGPT plan's sign-in through the
// pre-session dialog, on a fresh home with no key (Plan 034's case): it
// begins, finishes — the dialog up, fetching the plan's models — shows the
// one-time notice as a step of its own, records it as shown on that step's
// Enter and on nothing earlier, and goes back with native recomputed (ready:
// the cursor on it, from the picker). An exit before the notice's Enter
// leaves it unshown; a sign-in without plan usage goes back with what to do,
// native still needing setup; one declined in the browser, or that cannot
// begin, goes back with why; Esc on the sign-in step goes back unchanged, its
// attempt ended. None writes to the transcript or calls the backend.
func TestPreConnectSignIn(t *testing.T) {
	type flow struct {
		m          Model
		cb         *countingBackend
		log        *builtLog
		s          *signInStandIns
		shown      listShown
		transcript string
	}
	// begin opens the dialog from origin over a fresh home whose table has
	// the ChatGPT plan, and begins its sign-in with a stand-in answering res
	// (or err) — native ready once the stand-in's model fetch has run.
	begin := func(t *testing.T, origin connectReturn, res chatgptauth.Result, err error) (flow, tea.Cmd) {
		t.Helper()
		s := standInSignIn(t, func() *fakeSignIn {
			f := newFakeSignIn(false, res)
			f.err = err
			return f
		})
		var f flow
		f.s = s
		f.m, _, f.cb, f.log = preModel(t, origin, true, func() bool { return s.fetches.Load() > 0 })
		f.transcript = transcriptText(f.m)
		f.m, f.shown = preOpen(t, origin, f.m)
		f.m = preSelect(t, f.m, "chatgpt")
		m, wait := beginStep(t, f.m)
		f.m = m
		if f.m.cdlg.signIn.att == nil || !f.m.preConnectOpen() {
			t.Fatal("fixture: the sign-in did not begin in the pre-session dialog")
		}
		return f, wait
	}
	// signIn pastes the redirect, runs the wait and applies its answer.
	signIn := func(t *testing.T, f flow, wait tea.Cmd) (Model, tea.Cmd) {
		t.Helper()
		done := runWait(t, f.m, wait)
		m, _ := press(f.m, pasteKey(signInPasted))
		m, _ = press(m, enter())
		msg, ok := awaitMsg(t, done).(signInDoneMsg)
		if !ok || !msg.pre {
			t.Fatalf("the wait answered %+v; want the pre-session run's", msg)
		}
		tm, cmd := m.Update(msg)
		return tm.(Model), cmd
	}
	withNotice := chatgptauth.Result{Email: signInEmail, PlanUsage: true, ShowNotice: true}
	const signedIn = "signed in to ChatGPT as " + signInEmail + ": it can be picked now; ChatGPT plan models: chatgpt/gpt-5.6-sol, chatgpt/gpt-6-astra"

	for _, o := range preOrigins {
		t.Run(o.name, func(t *testing.T) {
			t.Run("finished, the notice, recomputed", func(t *testing.T) {
				f, wait := begin(t, o.origin, withNotice, nil)
				m, cmd := signIn(t, f, wait)
				preQuiet(t, "signed in", m, f.cb, f.transcript)
				if !m.preConnectOpen() || m.cdlg.step != connectFinishing || f.s.marks.Load() != 0 {
					t.Fatalf("signed in: dialog %v, step %d, notice marked %d — want the dialog fetching the models, the notice not recorded",
						m.preConnectOpen(), m.cdlg.step, f.s.marks.Load())
				}
				if view := plainView(m); !strings.Contains(view, preFinishingText) || !strings.Contains(view, "Signed in to ChatGPT as "+signInEmail) {
					t.Fatalf("the finishing step is not on screen:\n%s", view)
				}
				finished, ok := runWatched(t, mustCmd(t, cmd, "finishSignInCmd")).(signInFinishedMsg)
				if !ok || !finished.pre || f.s.marks.Load() != 0 || f.s.fetches.Load() != 1 {
					t.Fatalf("the finish answered %+v, marks %d, fetches %d — want the models fetched, the notice not recorded",
						finished, f.s.marks.Load(), f.s.fetches.Load())
				}
				m = applyMsg(t, m, finished)
				preQuiet(t, "the models fetched", m, f.cb, f.transcript)
				if !m.preConnectOpen() || m.cdlg.step != connectNotice || f.s.marks.Load() != 0 {
					t.Fatalf("the models fetched: dialog %v, step %d, marks %d — want the notice step, unrecorded",
						m.preConnectOpen(), m.cdlg.step, f.s.marks.Load())
				}
				if view := plainView(m); !strings.Contains(view, chatgptauth.NoticeTitle) || !strings.Contains(view, "Eligible usage in this app") {
					t.Fatalf("the notice step is not on screen:\n%s", view)
				}
				m, cmd = press(m, enter())
				marked, ok := runWatched(t, mustCmd(t, cmd, "preNoticeEnter")).(preNoticeMarkedMsg)
				if !ok || f.s.marks.Load() != 1 {
					t.Fatalf("the notice's Enter: answered %T, marks %d — want it recorded once", marked, f.s.marks.Load())
				}
				m, _ = press(m, enter()) // a second Enter while it is recorded
				tm, cmd := m.Update(marked)
				m = tm.(Model)
				if f.s.marks.Load() != 1 {
					t.Fatalf("the notice was recorded %d times", f.s.marks.Load())
				}
				preQuiet(t, "the notice recorded", m, f.cb, f.transcript)
				m = preBack(t, "the notice's Enter", o.origin, m, cmd, connectBack{text: signedIn, kind: sessNoteOK}, f.shown)
				if nativeState(o.origin, m) != AvailReady {
					t.Fatalf("native recomputed as %q after the sign-in, want ready", nativeState(o.origin, m))
				}
				if o.origin == returnPicker && m.providers[m.providerCursor].Name() != "native" {
					t.Fatalf("the cursor is on %s, want native", m.providers[m.providerCursor].Name())
				}
			})
			t.Run("an exit before the notice's Enter", func(t *testing.T) {
				f, wait := begin(t, o.origin, withNotice, nil)
				m, cmd := signIn(t, f, wait)
				m = applyMsg(t, m, runWatched(t, mustCmd(t, cmd, "finishSignInCmd")))
				if m.cdlg.step != connectNotice {
					t.Fatal("fixture: the notice step is not up")
				}
				m, cmd = press(m, tea.KeyMsg{Type: tea.KeyEsc})
				if f.s.marks.Load() != 0 {
					t.Fatalf("Esc on the notice recorded it as shown (%d)", f.s.marks.Load())
				}
				preQuiet(t, "Esc on the notice", m, f.cb, f.transcript)
				preBack(t, "Esc on the notice", o.origin, m, cmd, connectBack{}, f.shown)
			})
			t.Run("an exit while the models are fetched", func(t *testing.T) {
				f, wait := begin(t, o.origin, withNotice, nil)
				m, cmd := signIn(t, f, wait)
				finish := mustCmd(t, cmd, "finishSignInCmd")
				m, cmd = press(m, tea.KeyMsg{Type: tea.KeyEsc})
				preQuiet(t, "Esc while fetching", m, f.cb, f.transcript)
				m = preBack(t, "Esc while fetching", o.origin, m, cmd, connectBack{}, f.shown)
				before := preDigest(m)
				m = applyMsg(t, m, runWatched(t, finish))
				if after := preDigest(m); after != before || f.s.marks.Load() != 0 {
					t.Fatalf("the finish, after the dialog went, changed the model (marks %d):\n%s\nwas:\n%s", f.s.marks.Load(), after, before)
				}
			})
			t.Run("plan usage off", func(t *testing.T) {
				f, wait := begin(t, o.origin, chatgptauth.Result{Email: signInEmail, PlanUsage: false}, nil)
				m, cmd := signIn(t, f, wait)
				if findCmd(cmd, "finishSignInCmd") != nil {
					t.Fatal("a sign-in without plan usage fetched the plan's models")
				}
				preQuiet(t, "plan usage off", m, f.cb, f.transcript)
				m = preBack(t, "plan usage off", o.origin, m, cmd,
					connectBack{text: "signed in to ChatGPT as " + signInEmail + prePlanOffTail, kind: sessNoteWarn}, f.shown)
				if nativeState(o.origin, m) != AvailNeedsSetup || f.s.marks.Load() != 0 || f.s.fetches.Load() != 0 {
					t.Fatalf("plan usage off: native %q, marks %d, fetches %d — want it still needing setup, nothing recorded or fetched",
						nativeState(o.origin, m), f.s.marks.Load(), f.s.fetches.Load())
				}
			})
			t.Run("declined in the browser", func(t *testing.T) {
				f, wait := begin(t, o.origin, chatgptauth.Result{}, chatgptauth.ErrAccessDenied)
				m, cmd := signIn(t, f, wait)
				preQuiet(t, "declined", m, f.cb, f.transcript)
				preBack(t, "declined", o.origin, m, cmd,
					connectBack{text: "the sign-in was declined in the browser; nothing was changed", kind: sessNoteErr}, f.shown)
			})
			t.Run("a sign-in that fails", func(t *testing.T) {
				f, wait := begin(t, o.origin, chatgptauth.Result{}, errors.New("chatgptauth: exchange refused (HTTP 400, invalid_grant)"))
				m, cmd := signIn(t, f, wait)
				preQuiet(t, "failed", m, f.cb, f.transcript)
				preBack(t, "failed", o.origin, m, cmd,
					connectBack{text: preSignInFailText + "exchange refused (HTTP 400, invalid_grant)", kind: sessNoteErr}, f.shown)
			})
			t.Run("a sign-in that cannot begin", func(t *testing.T) {
				s := standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(false, chatgptauth.Result{}) })
				s.beginErr = errors.New("chatgptauth: no registration could be made")
				m, _, cb, _ := preModel(t, o.origin, true, nil)
				transcript := transcriptText(m)
				m, shown := preOpen(t, o.origin, m)
				m, cmd := press(preSelect(t, m, "chatgpt"), enter())
				begun, ok := runWatched(t, mustCmd(t, cmd, "beginSignInCmd")).(signInBegunMsg)
				if !ok || begun.err == nil {
					t.Fatalf("the begin answered %+v", begun)
				}
				tm, cmd := m.Update(begun)
				m = tm.(Model)
				preQuiet(t, "the begin refused", m, cb, transcript)
				preBack(t, "the begin refused", o.origin, m, cmd,
					connectBack{text: preSignInStartText + "no registration could be made", kind: sessNoteErr}, shown)
			})
			t.Run("esc on the sign-in step", func(t *testing.T) {
				f, _ := begin(t, o.origin, withNotice, nil)
				att := f.m.cdlg.signIn.att.(*fakeSignIn)
				m, cmd := press(f.m, tea.KeyMsg{Type: tea.KeyEsc})
				if !att.isClosed() || !slices.Contains(att.closeReasons(), chatgptauth.CloseDialog) {
					t.Fatalf("Esc left the attempt open (reasons %v)", att.closeReasons())
				}
				preQuiet(t, "Esc on the sign-in", m, f.cb, f.transcript)
				preBack(t, "Esc on the sign-in", o.origin, m, cmd, connectBack{}, f.shown)
			})
		})
	}
}

// TestPreConnectOpensFromTheList (§3.3, §3.6): in the list's /provider,
// native needing setup opens the pre-session dialog however it is chosen —
// Enter or Tab on its candidate, or `/provider native` typed with the popup
// put away — taking nothing; a ready provider is still taken.
func TestPreConnectOpensFromTheList(t *testing.T) {
	base, _, cb, _ := preModel(t, returnList, false, nil)
	transcript := transcriptText(base)
	pick := base.sessPick
	opened := func(t *testing.T, how string, m Model) {
		t.Helper()
		if !m.preConnectOpen() || m.cdlg.returnTo != returnList || m.sessList.open || !reflect.DeepEqual(m.sessPick, pick) {
			t.Fatalf("%s: connect dialog %v (back to %v), list open %v, pick %+v — want the dialog, nothing taken",
				how, m.preConnectOpen(), m.cdlg.returnTo, m.sessList.open, m.sessPick)
		}
		preQuiet(t, how, m, cb, transcript)
	}
	onNative := toListNative(providersLoaded(t, base, "/provider "))
	next, _ := press(onNative, enter())
	opened(t, "Enter on native", next)
	next, _ = press(onNative, tea.KeyMsg{Type: tea.KeyTab})
	opened(t, "Tab on native", next)
	typed, _ := typeList(t, base, "/provider native")
	typed, _ = press(typed, tea.KeyMsg{Type: tea.KeyEsc})
	if typed.sessList.in.cmd.visible() {
		t.Fatal("fixture: esc left the popup up")
	}
	next, _ = press(typed, enter())
	opened(t, "/provider native typed", next)
	grok, _ := typeList(t, base, "/provider grok")
	grok, _ = press(grok, tea.KeyMsg{Type: tea.KeyEsc})
	grok, _ = press(grok, enter())
	if grok.preConnectOpen() || grok.sessPick.prov.Name() != "grok" {
		t.Fatalf("/provider grok: dialog %v, pick %+v", grok.preConnectOpen(), grok.sessPick)
	}
}

// TestPreConnectPasteOverTheList (§3.6's paste test): with the dialog open
// over the list, a terminal's bracketed paste and Ctrl+V's clipboard paste
// both land in the masked key field — never in the list's input, which comes
// back as it was, nor the composer — and no frame shows either.
func TestPreConnectPasteOverTheList(t *testing.T) {
	const clip = "sk-clipboard-canary-5e1d0b"
	prevRead := clipboardRead
	clipboardRead = func() (string, error) { return clip, nil }
	t.Cleanup(func() { clipboardRead = prevRead })
	m, _, cb, _ := preModel(t, returnList, false, nil)
	transcript, composer := transcriptText(m), m.input.Value()
	m, shown := preOpen(t, returnList, m)
	m = pressKey(t, preSelect(t, m, "gamma"), tea.KeyEnter)
	m, _ = press(m, pasteKey(connectCanary))
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlV})
	paste, ok := runCmd(cmd).(pasteMsg)
	if !ok || paste.key != m.cdlg.openField() || paste.key == (keyField{}) || paste.listGen != 0 {
		t.Fatalf("Ctrl+V asked for %+v; want the clipboard for the key field", paste)
	}
	m = applyMsg(t, m, paste)
	connectView(t, m, clip)
	if m.cdlg.key.Value() != connectCanary+clip {
		t.Fatal("the pastes did not both land in the key field")
	}
	if v, _ := inputOf(m); v != "" || m.input.Value() != composer {
		t.Fatal("a paste landed in the list's input or the composer")
	}
	m, cmd = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	connectLeak(t, m, nil, clip)
	preQuiet(t, "the pastes", m, cb, transcript)
	preBack(t, "Esc after the pastes", returnList, m, cmd, connectBack{}, shown)
}

// TestPreConnectOverAWorkingSessionWithACard (§3.6): the list opened from a
// session whose turn is running and whose card is waiting — the state the
// in-session /connect refuses under, and a card that owns the keyboard and
// the mouse — opens the dialog all the same: keys and a click go to the
// dialog, a card arriving meanwhile leaves it up, and the way out gives the
// list back with the card still waiting behind it, unanswered.
func TestPreConnectOverAWorkingSessionWithACard(t *testing.T) {
	dir, getenv := preFixture(t, false, false)
	m, fs, stub := newSessModel(t, 100, 30)
	t.Cleanup(func() { _ = m.signIns.closeLog() })
	t.Cleanup(func() { _ = stub.Close() })
	ps := &preSessions{startSessions: fs, avail: preStates(dir, getenv, nil)}
	m.sessions = ps
	m.nativeDir, m.nativeEnv = dir, getenv
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(false)})
	m.status = statusWorking
	if !m.cardOpen() || !m.connectBusy() {
		t.Fatal("fixture: no card open, or /connect not refused in the session")
	}
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyLeft})
	m = listSnap(t, m, richSnapshot(m.sessList.home, m.hereKey()))
	m = takeOpenRead(t, m, cmd)
	m, shown := preOpen(t, returnList, m)
	if m.connectBusy() {
		t.Fatal("the pre-session dialog is refused as busy")
	}
	// The card owns neither the keyboard nor the mouse while the dialog is up.
	m = pressKey(t, preSelect(t, m, "gamma"), tea.KeyEnter)
	if m.cdlg.step != connectKey {
		t.Fatal("Enter went to the card, not the dialog")
	}
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
	if !m.preConnectOpen() || m.cdlg.step != connectKey {
		t.Fatal("a card arriving closed the pre-session dialog")
	}
	m, _ = press(m, pasteKey(connectCanary))
	if m.cdlg.key.Value() != connectCanary {
		t.Fatal("the paste went to the card, not the key field")
	}
	tm, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: 0})
	m = tm.(Model)
	connectLeak(t, m, stub)
	m = preBack(t, "a click outside over a card", returnList, m, cmd, connectBack{}, shown)
	if !m.cardOpen() {
		t.Fatal("the card behind the list was answered or dropped")
	}
}

// TestPreConnectQuitClosesTheLogInOrder (§3.6's quit row, 034 X79): Ctrl+C
// on the pre-session dialog's sign-in step quits — from the picker as the
// picker quits, from the list as the list quits (every session left running:
// nothing stopped or cancelled) — and ends the attempt at once, its listener
// closed, for the shutdown; finishRun then writes its outcome before it
// flushes and closes the sign-in log, which holds the begin and the
// shutdown's cancel. The attempt is chatgptauth's own (a re-login of the
// fixture's registration), which logs through the run's observer.
func TestPreConnectQuitClosesTheLogInOrder(t *testing.T) {
	for _, o := range preOrigins {
		t.Run(o.name, func(t *testing.T) {
			newFakeIssuer(t)
			dir, getenv := preFixture(t, true, true)
			var initial Model
			var cb *countingBackend
			if o.origin == returnPicker {
				initial, _ = prePicker(t, agent.GrokProvider(), dir, getenv, nil)
			} else {
				initial, cb = preList(t, dir, getenv, nil)
				// Its turn is running: the session's own Ctrl+C would stop
				// it, which the dialog's must not.
				initial.status = statusWorking
			}
			m, _ := preOpen(t, o.origin, initial)
			m, wait := beginStep(t, preSelect(t, m, "chatgpt"))
			done := runWait(t, m, wait)
			s := m.cdlg.signIn
			if s.att == nil || !s.listening {
				t.Fatal("the control: the attempt is not listening before the quit")
			}
			m, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlC})
			if !m.quitting || m.preConnectOpen() || m.sessList.open || cmd == nil {
				t.Fatalf("Ctrl+C: quitting %v, dialog %v, list %v — want craze quitting, nothing reopened",
					m.quitting, m.preConnectOpen(), m.sessList.open)
			}
			if _, ok := awaitMsg(t, done).(signInDoneMsg); !ok {
				t.Fatal("the quit left the attempt's wait running")
			}
			if cb != nil {
				if made := cb.seen(); len(made) != 0 {
					t.Fatalf("the list's quit called the session behind it: %v", made)
				}
			}
			_, _ = finishRun(io.Discard, nil, initial, nil)
			recs, _ := tuiSignInLog(t, dir)
			if got, want := recordKinds(recs), []string{"begin", "cancelled"}; !slices.Equal(got, want) || recs[1]["reason"] != "shutdown" {
				t.Fatalf("the sign-in log holds %v (%v); want the begin and the shutdown's cancel", got, recs)
			}
		})
	}
}

// TestPreConnectLeavesTheLogNoteToTheExit (§3.6: nothing to the transcript):
// a sign-in log that cannot be kept — its directory a symlink — is said
// nowhere while the pre-session dialog, or the picker under it, is up: its
// note is left to finishRun, which prints it once craze's screen is gone, as
// it prints any failure of the log no Update said.
func TestPreConnectLeavesTheLogNoteToTheExit(t *testing.T) {
	standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(false, chatgptauth.Result{}) })
	dir, getenv := preFixture(t, true, false)
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "logs")); err != nil {
		t.Fatal(err)
	}
	initial, _ := prePicker(t, agent.GrokProvider(), dir, getenv, nil)
	transcript := transcriptText(initial)
	m, _ := preOpen(t, returnPicker, initial)
	m, _, logWait := beginStepLog(t, preSelect(t, m, "chatgpt"))
	if logWait == nil {
		t.Fatal("fixture: the first begin armed no wait for the log")
	}
	off, ok := runWatched(t, logWait).(signInLogOffMsg)
	if !ok {
		t.Fatalf("the log's wait answered %T; want its refusal", off)
	}
	m = applyMsg(t, m, off)
	preQuiet(t, "the log's refusal, the dialog up", m, nil, transcript)
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyEsc})
	m = preBack(t, "Esc", returnPicker, m, cmd, connectBack{}, listShown{})
	m = applyMsg(t, m, off)
	preQuiet(t, "the log's refusal, the picker up", m, nil, transcript)
	var out strings.Builder
	_, _ = finishRun(&out, nil, initial, nil)
	if !strings.Contains(out.String(), "craze: the sign-in log is off: ") {
		t.Fatalf("finishRun did not say the log was off: %q", out.String())
	}
}

// TestPreConnectTheSessionBehindEnds (§3.6: every way out reopens the list):
// the session behind the list ending while the dialog is up — its own end,
// or its connection lost — takes the dialog down, its sign-in ended, and
// gives the list back as it was, the end taken as the list takes any end of
// the session behind it.
func TestPreConnectTheSessionBehindEnds(t *testing.T) {
	for _, end := range []struct {
		name string
		msg  endMsg
		note string
	}{
		{"its own end", endMsg{}, sessEndedNote},
		{"its connection lost", endMsg{err: errors.New("the socket went away")}, sessLostNote + ": the socket went away"},
	} {
		t.Run(end.name, func(t *testing.T) {
			s := standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(false, chatgptauth.Result{}) })
			m, _, cb, _ := preModel(t, returnList, true, nil)
			transcript := transcriptText(m)
			m, shown := preOpen(t, returnList, m)
			m, _ = beginStep(t, preSelect(t, m, "chatgpt"))
			tm, cmd := m.Update(end.msg)
			m = tm.(Model)
			if made := s.all(); len(made) != 1 || !made[0].isClosed() {
				t.Fatal("the session's end left the sign-in's attempt open")
			}
			preQuiet(t, end.name, m, cb, transcript)
			if m.quitting || m.preConnectOpen() {
				t.Fatalf("%s: quitting %v, dialog %v — want the list, craze running", end.name, m.quitting, m.preConnectOpen())
			}
			preBack(t, end.name, returnList, m, cmd, connectBack{text: end.note, kind: sessNoteWarn}, shown)
		})
	}
}

// TestPreConnectTheGateTakesTheDialogDown (Plan 031's invariant, §3.6): a
// provider the startup picker starts takes every /connect state down with
// the picker, a sign-in's attempt closed — whatever left it there.
func TestPreConnectTheGateTakesTheDialogDown(t *testing.T) {
	standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(false, chatgptauth.Result{}) })
	m, _, _, log := preModel(t, returnPicker, true, nil)
	m, _ = preOpen(t, returnPicker, m)
	m, _ = beginStep(t, preSelect(t, m, "chatgpt"))
	att := m.cdlg.signIn.att.(*fakeSignIn)
	tm, _ := m.confirmProvider(agent.GrokProvider(), true)
	m = tm.(Model)
	if m.pickingProvider || !slices.Equal(log.got(), []string{"grok"}) {
		t.Fatalf("fixture: grok did not start (picking %v, built %v)", m.pickingProvider, log.got())
	}
	if m.cdlg.gen != 0 || m.cdlg.pre || m.cdlg.step != connectPick || !att.isClosed() {
		t.Fatalf("the start left /connect's state up (gen %d, pre %v, step %d) or its attempt open (%v)",
			m.cdlg.gen, m.cdlg.pre, m.cdlg.step, att.isClosed())
	}
}

// preFirstPrompt is an unstarted session's first prompt (plan 030 §3.13)
// whose session was spawned and adopted but is not up yet — its start run to
// its answer, failing with startErr when set, and held — with the list opened
// over it (its composer emptied, ←) and the pre-session dialog open over the
// list (plan 036 §3.6). It answers the held start's message and what the list
// showed.
func preFirstPrompt(t *testing.T, startErr error) (Model, tea.Msg, *hostBackend, *callLog, listShown) {
	t.Helper()
	dir, getenv := preFixture(t, false, false)
	m, fs, hb, log := openedUnstarted(t, 100, 30)
	t.Cleanup(func() { _ = m.signIns.closeLog() })
	m.sessions = &preSessions{startSessions: fs, avail: preStates(dir, getenv, nil)}
	m.nativeDir, m.nativeEnv = dir, getenv
	m = typeComposer(t, m, "go")
	m, cmd := press(m, enter())
	tm, cmd := m.Update(runWatched(t, mustCmd(t, cmd, "enterUnstarted")))
	m = tm.(Model)
	if m.eng != hb || m.first == nil {
		t.Fatalf("fixture: adopted %v, first %v", m.eng, m.first)
	}
	hb.startErr = startErr
	start := runWatched(t, mustCmd(t, cmd, "startCmd"))
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyBackspace})
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyBackspace})
	m, cmd = press(m, tea.KeyMsg{Type: tea.KeyLeft})
	if !m.sessList.open || m.sessList.none || m.sessList.here.zero() {
		t.Fatalf("fixture: ← over the adopted session: list %v, none %v, here %+v", m.sessList.open, m.sessList.none, m.sessList.here)
	}
	m = listSnap(t, m, roster.Snapshot{})
	m = takeOpenRead(t, m, cmd)
	m, shown := preOpen(t, returnList, m)
	if m.first == nil || !m.sessListUp() {
		t.Fatalf("fixture: first %v, the list up %v", m.first, m.sessListUp())
	}
	return m, start, hb, log, shown
}

// TestPreConnectFirstPromptFailsBehindTheDialog (X154 with plan 036 §3.6):
// the session an unstarted session's first prompt spawned fails to come up
// while the pre-session dialog stands in for the list over it. As with the
// list up, the failure is the session's — its error drawn in its transcript,
// its status failed — and the prompt is dropped, never sent; the session is
// not made unstarted again under the dialog. The dialog stays, and its way
// out opens the list again as it was.
func TestPreConnectFirstPromptFailsBehindTheDialog(t *testing.T) {
	m, start, hb, log, shown := preFirstPrompt(t, errors.New("authentication required"))
	failed, ok := start.(errMsg)
	if !ok {
		t.Fatalf("fixture: the failing start answered %T", start)
	}
	m = applyMsg(t, m, failed)
	switch {
	case m.first != nil || m.unstarted != nil || m.eng != hb:
		t.Fatalf("the failure behind the dialog: first %v, unstarted %v, backend %v — want the prompt dropped, the session the failed one",
			m.first, m.unstarted, m.eng)
	case !m.preConnectOpen() || m.cdlg.returnTo != returnList:
		t.Fatalf("the failure took the dialog down (dialog %v, back to %v)", m.preConnectOpen(), m.cdlg.returnTo)
	case m.status != statusError || m.startErr == nil || !slices.Contains(texts(m, entryError), "authentication required"):
		t.Fatalf("the failure is not the session's: status %v, start error %v, errors %q", m.status, m.startErr, texts(m, entryError))
	}
	if cmds, sent := hb.sent(); len(cmds) != 0 || slices.Contains(log.seen(), "submit") {
		t.Fatalf("the first prompt was sent: %v with %q", cmds, sent)
	}
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyEsc})
	m = preBack(t, "Esc after the failure", returnList, m, cmd, connectBack{}, shown)
	if m.unstarted != nil || m.sessList.none {
		t.Fatalf("the list came back over an unstarted session (unstarted %v, none %v)", m.unstarted, m.sessList.none)
	}
}

// TestPreConnectFirstPromptEndsBehindTheDialog (X154 with plan 036 §3.6, as
// TestPreConnectTheSessionBehindEnds): the session an unstarted session's
// first prompt spawned ends before it came up while the pre-session dialog
// stands in for the list over it. The dialog goes, as with any end of the
// session behind it, and the list opens again as it was; the end is then the
// list's, as with the list up: the session's row marked ended, craze going
// on, the prompt dropped — not made unstarted again — and a start answering
// after the end sends nothing.
func TestPreConnectFirstPromptEndsBehindTheDialog(t *testing.T) {
	m, start, hb, log, shown := preFirstPrompt(t, nil)
	started, ok := start.(startedMsg)
	if !ok {
		t.Fatalf("fixture: the start answered %T", start)
	}
	tm, cmd := m.Update(endMsg{bgen: m.bgen})
	m = tm.(Model)
	switch {
	case m.quitting || !m.ended || m.first != nil || m.unstarted != nil:
		t.Fatalf("the end behind the dialog: quitting %v, ended %v, first %v, unstarted %v — want the prompt dropped, craze on",
			m.quitting, m.ended, m.first, m.unstarted)
	case m.preConnectOpen():
		t.Fatal("the session's end left the dialog up")
	}
	m = preBack(t, "the end behind the dialog", returnList, m, cmd, connectBack{text: sessEndedNote, kind: sessNoteWarn}, shown)
	if row, ok := sessFind(m.sessLines(), m.sessList.here); !m.sessList.hereEnded || !ok || !row.ended {
		t.Fatalf("the session's row after its end: %+v (listed %v, marked %v)", row, ok, m.sessList.hereEnded)
	}
	m = applyMsg(t, m, started)
	if cmds, sent := hb.sent(); len(cmds) != 0 || slices.Contains(log.seen(), "submit") {
		t.Fatalf("the late start sent the first prompt to the ended session: %v with %q", cmds, sent)
	}
	if m.quitting || !m.sessList.open || m.unstarted != nil {
		t.Fatalf("after the late start: quitting %v, list open %v, unstarted %v", m.quitting, m.sessList.open, m.unstarted)
	}
}

// TestPreConnectComposerPasteLandsInTheDraft (the list's paste rule, X140,
// with plan 036 §3.6): a paste asked for in the composer before the list
// opened, answering while the pre-session dialog stands in for the list,
// lands in the composer's draft — where it lands with the list up, and where
// the user finds it on going back — never in the dialog's key field.
func TestPreConnectComposerPasteLandsInTheDraft(t *testing.T) {
	m, _, _, _ := preModel(t, returnList, false, nil)
	m, _ = preOpen(t, returnList, m)
	m = pressKey(t, preSelect(t, m, "gamma"), tea.KeyEnter)
	m = applyMsg(t, m, pasteMsg{text: "pasted before the list", shownGen: m.shownGen})
	if got := m.input.Value(); got != "pasted before the list" || m.cdlg.key.Value() != "" {
		t.Fatalf("the composer's paste: draft %q, key field %d bytes — want it in the draft alone", got, len(m.cdlg.key.Value()))
	}
}
