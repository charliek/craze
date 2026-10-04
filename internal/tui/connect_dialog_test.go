package tui

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// /connect and the connect row (plan 031 §3.6, §3.9) as unit tests: a Stub
// that names the native provider, and the TUI's seams (Config.NativeDir,
// Config.Getenv) on a fixture directory and environment — never the shipped
// catalog, the developer's environment or their ~/.craze. The frames are
// native_connect_golden_test.go's.

// connectCanary is the dummy key the canaries look for: long enough to be a
// key (modeltable.MinKeyLen) and clear of the redaction marker. A failure
// message never quotes it (connectLeak).
const connectCanary = "sk-connect-canary-7f3a9e"

// connectFixture is a native directory under a home of its own holding
// nativePickerTable — catalog = false, so none of the shipped providers is in
// it — with keys stored as given (provider id → key), and the TUI's
// environment over it: nativePickerEnv's variables (alpha and beta funded,
// gamma not) and HOME, so the key field's hint reads
// ~/.craze/native/providers.toml on every machine.
func connectFixture(t *testing.T, stored map[string]string) (dir string, getenv func(string) string) {
	t.Helper()
	home := t.TempDir()
	dir = filepath.Join(home, ".craze", "native")
	if err := modeltable.Save(dir, nativePickerTable()); err != nil {
		t.Fatal(err)
	}
	for id, k := range stored {
		if err := modeltable.SetKey(dir, id, k); err != nil {
			t.Fatal(err)
		}
	}
	return dir, func(k string) string {
		if k == "HOME" {
			return home
		}
		return nativePickerEnv(k)
	}
}

// connectModel is a started model over stub, sized 100x30, with the TUI's
// seams on dir and getenv. The stub names the native provider unless the test
// has named another.
func connectModel(t *testing.T, stub *Stub, dir string, getenv func(string) string) Model {
	t.Helper()
	return connectModelOf(t, Config{Session: stub, NativeDir: dir, Getenv: getenv})
}

// connectModelOf is connectModel over cfg — its session, seams and, for a
// test that needs one, its session list (Config.Sessions) — with the rest of
// the fixture's Config filled in.
func connectModelOf(t *testing.T, cfg Config) Model {
	t.Helper()
	isolateSkillsHome(t)
	cfg.Theme, cfg.Workspace, cfg.Model, cfg.Yolo = "tokyo-night", t.TempDir(), "grok", true
	tm, _ := New(cfg).Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return startedLikeInit(t, tm.(Model))
}

// nativeStub is a Stub whose session is a native one, as its facts name it.
func nativeStub() *Stub {
	s := NewStub()
	s.SetProvider(agent.NativeProvider())
	return s
}

// typeCommand types text into the composer and presses Enter.
func typeCommand(t *testing.T, m Model, text string) (Model, tea.Cmd) {
	t.Helper()
	m = draft(m, text)
	return press(m, enter())
}

// connectKeyStep is connectModel with /connect typed and Enter pressed on the
// provider step one opens on: the key field up.
func connectKeyStep(t *testing.T, stub *Stub, dir string, getenv func(string) string) Model {
	t.Helper()
	m := connectModel(t, stub, dir, getenv)
	m, _ = typeCommand(t, m, "/connect")
	return pressKey(t, m, tea.KeyEnter)
}

// pasteKey is a terminal's bracketed paste of text.
func pasteKey(text string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text), Paste: true}
}

// connectPiece is the shortest piece of a key the canaries look for: a
// clipped field, a wrapped row or a cut error shows a key in pieces, and a
// piece this long (modeltable.MinKeyLen, the shortest key) is the key's own. A
// key shorter than that is looked for whole.
const connectPiece = modeltable.MinKeyLen

// connectShown reports whether text shows key, whole or any piece of it
// connectPiece bytes long.
func connectShown(text, key string) bool {
	if key == "" {
		return false
	}
	n := min(connectPiece, len(key))
	for i := 0; i+n <= len(key); i++ {
		if strings.Contains(text, key[i:i+n]) {
			return true
		}
	}
	return false
}

// connectLeak fails the test when a key is anywhere the user or a log could
// read it: the frame, the transcript's entries (its notes and error rows), the
// composer, what the session was sent, the key field's refusal, and the
// dialog's own state once it has closed. A test calls it before any assertion
// of its own that would print one of them, so a leak fails here, first. The
// failure names where, never the key (astra r4 2): <CANARY> stands for it. It
// looks for connectCanary as well as the keys it is given.
func connectLeak(t *testing.T, m Model, stub *Stub, keys ...string) {
	t.Helper()
	places := map[string]string{
		"the frame":         plainView(m),
		"the composer":      m.input.Value(),
		"the field's error": m.cdlg.keyErr,
	}
	if stub != nil {
		places["the prompts"] = strings.Join(stub.Prompts(), "\n")
	}
	for _, e := range m.main.entries() {
		places["the transcript"] += e.text + "\n"
	}
	if m.dialog != dialogConnect {
		places["the closed dialog's field"] = m.cdlg.key.Value()
	}
	for _, key := range append([]string{connectCanary}, keys...) {
		for where, text := range places {
			if connectShown(text, key) {
				t.Fatalf("the key (<CANARY>) reached %s", where)
			}
		}
	}
}

// connectView is m's frame, as plainView draws it, once it has been checked
// for connectCanary and keys (connectShown): every frame a /connect test
// prints or compares is one this has passed, so a masking that regressed fails
// with a line that names no key, never with the frame that shows it.
func connectView(t *testing.T, m Model, keys ...string) string {
	t.Helper()
	view := plainView(m)
	for _, key := range append([]string{connectCanary}, keys...) {
		if connectShown(view, key) {
			t.Fatal("the frame shows the key (<CANARY>)")
		}
	}
	return view
}

// storedKey is what dir's providers.toml stores for id, and whether it
// stores anything.
func storedKey(t *testing.T, dir, id string) (string, bool) {
	t.Helper()
	keys, err := modeltable.StoredKeys(dir)
	if err != nil {
		t.Fatalf("reading the stored keys: %v", err)
	}
	for _, k := range keys {
		if k.Provider == id {
			return string(k.Key), true
		}
	}
	return "", false
}

// TestConnectIsANativeBuiltin (§3.9, CR 9): /connect is in a native session's
// slash catalog, right after /model, and a typed /connect opens the dialog;
// in an ACP session it is in neither the catalog nor the builtins' dispatch,
// and a typed /connect goes to the agent as the text it is.
func TestConnectIsANativeBuiltin(t *testing.T) {
	dir, getenv := connectFixture(t, nil)

	m := connectModel(t, nativeStub(), dir, getenv)
	var names []string
	for _, it := range m.slashCatalog() {
		names = append(names, it.Name)
	}
	if i := slices.Index(names, "connect"); i < 0 || names[i-1] != "model" {
		t.Fatalf("native's catalog is %v: /connect missing, or not right after /model", names)
	}
	m, _ = typeCommand(t, m, "/connect")
	if m.dialog != dialogConnect || m.input.Value() != "" {
		t.Fatalf("a typed /connect on native: dialog %v, draft %q", m.dialog, m.input.Value())
	}

	cursor := NewStub()
	c := connectModel(t, cursor, dir, getenv)
	for _, it := range c.slashCatalog() {
		if it.Name == "connect" {
			t.Fatal("an ACP session lists /connect")
		}
	}
	c, _ = typeCommand(t, c, "/connect")
	if c.dialog != dialogNone {
		t.Fatalf("a typed /connect on cursor opened dialog %v", c.dialog)
	}
	assertSent(t, c, "/connect")
}

// TestConnectRefusedWhileWorkRuns (§3.9, R1): a turn, the agent's own turn or
// a background sub-agent still running refuses /connect with one error row,
// the draft consumed and no dialog opened; the same session once idle opens
// it.
func TestConnectRefusedWhileWorkRuns(t *testing.T) {
	dir, getenv := connectFixture(t, nil)
	for _, tc := range []struct {
		name string
		busy func(t *testing.T, m Model, stub *Stub) Model
	}{
		{"a turn", func(_ *testing.T, m Model, _ *Stub) Model {
			m.status = statusWorking
			return m
		}},
		{"the agent's own turn", func(t *testing.T, m Model, stub *Stub) Model {
			stub.SetForeignTurn(agent.ForeignTurnInfo{ID: "wake-1", Running: true})
			return applyPending(t, m)
		}},
		{"a background sub-agent", func(t *testing.T, m Model, stub *Stub) Model {
			kid := stopKid("kid-1", "scan", agent.SubagentRunning)
			kid.Background = true
			stub.SetSubagents([]agent.SubagentInfo{kid})
			tm, _ := m.Update(eventMsg{ev: agent.Event{Type: agent.EventSubagent, Subagent: &kid, SubagentChange: agent.SubagentChangeSpawned}})
			return tm.(Model)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := nativeStub()
			m := tc.busy(t, connectModel(t, stub, dir, getenv), stub)
			if !m.connectBusy() {
				t.Fatal("fixture: the session is not busy")
			}
			m, _ = typeCommand(t, m, "/connect")
			if m.dialog != dialogNone {
				t.Fatalf("/connect opened dialog %v while busy", m.dialog)
			}
			if got := texts(m, entryError); !slices.Equal(got, []string{connectBusyText}) {
				t.Fatalf("error rows %q, want the refusal", got)
			}
			if m.input.Value() != "" {
				t.Fatalf("the draft was kept: %q", m.input.Value())
			}
			if len(stub.Prompts()) != 0 {
				t.Fatalf("/connect was sent to the agent: %q", stub.Prompts())
			}
		})
	}
}

// TestConnectStoresAKeyAndSaysSo is the whole flow (§3.9, A3, A7): step one
// lists the fixture's providers — its own, never the shipped catalog's — by
// display name, the connected ones marked, and opens on the first without a
// key; Enter opens that provider's masked field, whose hint names the file;
// a pasted key and Enter store it in the fixture's providers.toml, close the
// box and write the notice. The key reaches nothing the user or a log reads,
// nor the session.
func TestConnectStoresAKeyAndSaysSo(t *testing.T) {
	dir, getenv := connectFixture(t, nil)
	stub := nativeStub()
	m := connectModel(t, stub, dir, getenv)
	m, _ = typeCommand(t, m, "/connect")

	view := connectView(t, m)
	for _, want := range []string{"Connect a provider", "Alpha", "Beta", "> Gamma", connectPickHint} {
		if !strings.Contains(view, want) {
			t.Fatalf("step one is missing %q:\n%s", want, view)
		}
	}
	for _, shipped := range []string{"Fireworks", "OpenRouter", "Meta"} {
		if strings.Contains(view, shipped) {
			t.Fatalf("step one lists the shipped catalog's %s, not the fixture's providers:\n%s", shipped, view)
		}
	}
	marks := map[string]string{}
	for _, p := range m.cdlg.providers {
		marks[p.Name] = connectMark(p)
	}
	if want := map[string]string{"Alpha": "✓", "Beta": "✓", "Gamma": ""}; !maps.Equal(marks, want) {
		t.Fatalf("marks %v, want %v", marks, want)
	}

	m = pressKey(t, m, tea.KeyEnter)
	view = connectView(t, m)
	for _, want := range []string{"Gamma API key", "Stored in ~/.craze/native/providers.toml.", connectKeyHint} {
		if !strings.Contains(view, want) {
			t.Fatalf("step two is missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "is set in this environment") {
		t.Fatalf("gamma has no variable set, yet the hint says one wins:\n%s", view)
	}

	tm, _ := m.Update(pasteKey(connectCanary + "\n"))
	m = tm.(Model)
	connectLeak(t, m, stub)
	if view := connectView(t, m); !strings.Contains(view, strings.Repeat(string(connectMask), len(connectCanary))) {
		t.Fatalf("the field does not show the key masked:\n%s", view)
	}

	m = pressKey(t, m, tea.KeyEnter)
	connectLeak(t, m, stub)
	if m.dialog != dialogNone || m.cdlg.gen != 0 || m.cdlg.key.Value() != "" {
		t.Fatalf("the box is still open after the save: dialog %v", m.dialog)
	}
	if got, ok := storedKey(t, dir, "gamma"); !ok || got != connectCanary {
		t.Fatalf("gamma's key was not stored as pasted (stored: %v)", ok)
	}
	want := "Connected Gamma. New sessions offer its models; to use them in this conversation, /exit and run craze -c."
	if got := texts(m, entryNote); !slices.Equal(got, []string{want}) {
		t.Fatalf("notes %q, want the notice", got)
	}
	if got := texts(m, entryError); len(got) != 0 {
		t.Fatalf("error rows after a save: %q", got)
	}
}

// TestConnectEnvHint (§3.9): a provider one of whose variables is set in the
// TUI's environment says, in its key field, that the variable is used before
// the stored key — the environment the seam gives, not the process's.
func TestConnectEnvHint(t *testing.T) {
	dir, getenv := connectFixture(t, nil)
	m := connectModel(t, nativeStub(), dir, getenv)
	m, _ = typeCommand(t, m, "/connect")
	m = pressKey(t, m, tea.KeyUp) // Beta, above Gamma
	m = pressKey(t, m, tea.KeyEnter)
	view := connectView(t, m)
	for _, want := range []string{"Beta API key", "PICKER_BETA_KEY is set in this environment; craze", "uses it before the stored key."} {
		if !strings.Contains(view, want) {
			t.Fatalf("beta's field is missing %q:\n%s", want, view)
		}
	}
}

// TestConnectRefusesKeysTheStoreWouldNot (§3.9): an empty key, one too short
// to be a key and one the redaction marker overlaps are refused in the field
// by the rule — never quoting the key — with the field emptied and nothing
// written; the box stays open on the same provider.
func TestConnectRefusesKeysTheStoreWouldNot(t *testing.T) {
	for _, tc := range []struct{ key, why string }{
		{"", "Not saved: the key is empty."},
		{"  ", "Not saved: the key is empty."},
		{"sk-7a", "Not saved: a key is at least 8 bytes."},
		{"credential", "Not saved: the key overlaps craze's redaction marker."},
		{strings.Repeat("k", connectKeyMax+1), "Not saved: the key is longer than 8 KiB."},
	} {
		dir, getenv := connectFixture(t, nil)
		stub := nativeStub()
		m := connectKeyStep(t, stub, dir, getenv)
		if tc.key != "" {
			tm, _ := m.Update(pasteKey(tc.key))
			m = tm.(Model)
		}
		m = pressKey(t, m, tea.KeyEnter)
		var keys []string
		if strings.TrimSpace(tc.key) != "" {
			keys = append(keys, tc.key)
		}
		connectLeak(t, m, stub, keys...)
		if m.dialog != dialogConnect || m.cdlg.step != connectKey {
			t.Fatalf("%s: the refusal closed the field", tc.why)
		}
		if m.cdlg.keyErr != tc.why || !strings.Contains(connectView(t, m, keys...), "Not saved: ") {
			t.Fatalf("the refusal reads %q, want %q", m.cdlg.keyErr, tc.why)
		}
		if m.cdlg.key.Value() != "" {
			t.Fatalf("%s: the refused key is still in the field", tc.why)
		}
		if _, ok := storedKey(t, dir, "gamma"); ok {
			t.Fatalf("%s: a refused key was stored", tc.why)
		}
		// Typing again takes the refusal down.
		m = pressKey(t, m, tea.KeyEnter)
		tm, _ := m.Update(runeKey('x'))
		if m = tm.(Model); m.cdlg.keyErr != "" {
			t.Fatalf("%s: the refusal outlived the next key", tc.why)
		}
	}
}

// TestConnectSaveRefusedOnceWorkStarts (§3.9, R1): work that starts while the
// box is open refuses the save in the field, keeping the key for a later
// Enter; nothing is written until then.
func TestConnectSaveRefusedOnceWorkStarts(t *testing.T) {
	dir, getenv := connectFixture(t, nil)
	stub := nativeStub()
	m := connectKeyStep(t, stub, dir, getenv)
	tm, _ := m.Update(pasteKey(connectCanary))
	m = tm.(Model)
	m.status = statusWorking
	m = pressKey(t, m, tea.KeyEnter)
	connectLeak(t, m, stub)
	if m.dialog != dialogConnect || m.cdlg.keyErr != connectBusySaveText || m.cdlg.key.Value() != connectCanary {
		t.Fatalf("a save while busy: dialog %v, refusal %q, key kept %v", m.dialog, m.cdlg.keyErr, m.cdlg.key.Value() == connectCanary)
	}
	if _, ok := storedKey(t, dir, "gamma"); ok {
		t.Fatal("a key was stored while work ran")
	}
	m.status = statusIdle
	m = pressKey(t, m, tea.KeyEnter)
	if got, ok := storedKey(t, dir, "gamma"); !ok || got != connectCanary {
		t.Fatalf("the Enter after the work ended stored nothing (stored: %v)", ok)
	}
}

// TestConnectClipboardPasteLandsOnlyInItsField (astra 15, CR 23): Ctrl+V in
// the key field reads the clipboard through craze's seam and tags the answer
// with that field; it lands there while the field is open, and nowhere — not
// the composer, not a field opened since — once it is not.
func TestConnectClipboardPasteLandsOnlyInItsField(t *testing.T) {
	prevRead := clipboardRead
	clipboardRead = func() (string, error) { return connectCanary, nil }
	t.Cleanup(func() { clipboardRead = prevRead })

	dir, getenv := connectFixture(t, nil)
	var stub *Stub
	open := func(t *testing.T) Model {
		t.Helper()
		stub = nativeStub()
		return connectKeyStep(t, stub, dir, getenv)
	}
	ctrlV := func(t *testing.T, m Model) (Model, pasteMsg) {
		t.Helper()
		m, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlV})
		msg, ok := runCmd(cmd).(pasteMsg)
		if !ok || msg.key != m.cdlg.openField() || msg.key == (keyField{}) {
			t.Fatalf("ctrl+v in the key field answered %T tagged %v, want the open field %v", runCmd(cmd), msg.key, m.cdlg.openField())
		}
		return m, msg
	}

	t.Run("into the open field", func(t *testing.T) {
		m, msg := ctrlV(t, open(t))
		tm, _ := m.Update(msg)
		if m = tm.(Model); m.cdlg.key.Value() != connectCanary {
			t.Fatal("the paste did not reach the field it was asked for in")
		}
		connectLeak(t, m, stub, connectCanary)
	})
	t.Run("after esc", func(t *testing.T) {
		m, msg := ctrlV(t, open(t))
		m = pressKey(t, m, tea.KeyEsc)
		tm, _ := m.Update(msg)
		m = tm.(Model)
		if m.cdlg.step != connectPick || m.input.Value() != "" {
			t.Fatal("a paste for a field that was left landed somewhere")
		}
		connectLeak(t, m, stub, connectCanary)
	})
	t.Run("into a field opened since", func(t *testing.T) {
		m, msg := ctrlV(t, open(t))
		m = pressKey(t, m, tea.KeyEsc)
		m = pressKey(t, m, tea.KeyEnter)
		tm, _ := m.Update(msg)
		if m = tm.(Model); m.cdlg.key.Value() != "" {
			t.Fatal("a paste asked for in one field landed in the next")
		}
	})
	t.Run("once the box has closed", func(t *testing.T) {
		m, msg := ctrlV(t, open(t))
		m = pressKey(t, m, tea.KeyEsc)
		m = pressKey(t, m, tea.KeyEsc)
		if m.dialog != dialogNone || m.composerCovered() {
			t.Fatal("fixture: the composer is not where the keys are")
		}
		tm, _ := m.Update(msg)
		m = tm.(Model)
		if m.input.Value() != "" {
			t.Fatal("a paste asked for in the key field landed in the composer")
		}
		connectLeak(t, m, stub, connectCanary)
	})
	t.Run("the composer's own paste stays out", func(t *testing.T) {
		m := open(t)
		tm, _ := m.Update(pasteMsg{text: "composer text", shownGen: m.shownGen})
		if m = tm.(Model); m.cdlg.key.Value() != "" || m.input.Value() != "" {
			t.Fatal("an untagged paste landed with the key field open")
		}
	})
}

// TestConnectFieldEmptiedOnEveryExit (§3.9, astra 15, astra r4 1): Esc, a
// card arriving, the model dialog opening over it, a save, a session switch —
// and the exits that are not the dialog's own: Ctrl+D, an idle Ctrl+C, a
// served session's quit (stopQuit), and the session ending, or its connection
// lost, whether craze goes back to the session list or, with none, quits —
// each closes the box and leaves no key in the model.
func TestConnectFieldEmptiedOnEveryExit(t *testing.T) {
	dir, getenv := connectFixture(t, nil)
	lost := errors.New("the socket went away")
	quitting := func(m Model) bool { return m.quitting }
	listed := func(m Model) bool { return m.sessList.open && !m.quitting }
	for _, tc := range []struct {
		name string
		// list gives the model a session list (Config.Sessions).
		list bool
		exit func(t *testing.T, m Model, stub *Stub) Model
		// happened, when set, says the exit took place: the premise.
		happened func(m Model) bool
	}{
		{"esc", false, func(t *testing.T, m Model, _ *Stub) Model { return pressKey(t, m, tea.KeyEsc) }, nil},
		{"a card", false, func(t *testing.T, m Model, stub *Stub) Model {
			return cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
		}, nil},
		{"another dialog", false, func(_ *testing.T, m Model, _ *Stub) Model { return m.openHelp() }, nil},
		{"a save", false, func(t *testing.T, m Model, _ *Stub) Model { return pressKey(t, m, tea.KeyEnter) }, nil},
		{"a session switch", false, func(_ *testing.T, m Model, _ *Stub) Model {
			return m.withSession(sessionSeed{workspace: m.cwd, provider: "native"})
		}, nil},
		{"ctrl+d", false, func(t *testing.T, m Model, _ *Stub) Model { return pressKey(t, m, tea.KeyCtrlD) }, quitting},
		{"an idle ctrl+c", false, func(t *testing.T, m Model, _ *Stub) Model { return pressKey(t, m, tea.KeyCtrlC) }, quitting},
		{"a served session's quit", false, func(t *testing.T, m Model, _ *Stub) Model {
			// A backend served elsewhere quits by asking its host to stop
			// the session (stopQuit); the command that asks is not run here.
			m.remote = true
			return pressKey(t, m, tea.KeyCtrlD)
		}, func(m Model) bool { return m.quitting && m.exit.stopping }},
		{"the session's end, back to the list", true, func(t *testing.T, m Model, _ *Stub) Model {
			return applyMsg(t, m, endMsg{})
		}, listed},
		{"the connection lost, back to the list", true, func(t *testing.T, m Model, _ *Stub) Model {
			return applyMsg(t, m, endMsg{err: lost})
		}, listed},
		{"the session's end, with no list", false, func(t *testing.T, m Model, _ *Stub) Model {
			return applyMsg(t, m, endMsg{})
		}, quitting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := nativeStub()
			var m Model
			if tc.list {
				m = connectModelOf(t, Config{Session: stub, NativeDir: dir, Getenv: getenv, Sessions: &fakeSessions{}})
				m, _ = typeCommand(t, m, "/connect")
				m = pressKey(t, m, tea.KeyEnter)
			} else {
				m = connectKeyStep(t, stub, dir, getenv)
			}
			tm, _ := m.Update(pasteKey(connectCanary))
			if m = tm.(Model); m.cdlg.key.Value() != connectCanary {
				t.Fatal("fixture: the key is not in the field")
			}
			m = tc.exit(t, m, stub)
			connectLeak(t, m, stub)
			if tc.happened != nil && !tc.happened(m) {
				t.Fatalf("fixture: %s did not happen", tc.name)
			}
			if m.cdlg.key.Value() != "" || m.cdlg.openField() != (keyField{}) {
				t.Fatalf("the key field outlived %s", tc.name)
			}
			// Esc goes back to step one; every other way out closes the box.
			if tc.name != "esc" && m.dialog == dialogConnect {
				t.Fatalf("the box outlived %s", tc.name)
			}
		})
	}
}

// TestConnectNotesOtherUnusableStoredKeys (§3.7 r2-7, X24): another
// provider's stored key that cannot be used is marked in step one, said in
// its own field, kept as it was by a save of another's, and named after that
// save — as craze auth login names it — by the rule, never the value.
func TestConnectNotesOtherUnusableStoredKeys(t *testing.T) {
	dir, _ := connectFixture(t, nil)
	path := filepath.Join(dir, modeltable.ProvidersFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const short = "k-9q"
	patched := strings.Replace(string(raw), "[providers.beta]\n", "[providers.beta]\napi_key = \""+short+"\"\n", 1)
	if patched == string(raw) {
		t.Fatalf("fixture: no [providers.beta] table in:\n%s", raw)
	}
	if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	// Nothing in the environment: beta's stored key is all it has.
	getenv := func(string) string { return "" }

	stub := nativeStub()
	m := connectModel(t, stub, dir, getenv)
	m, _ = typeCommand(t, m, "/connect")
	if view := connectView(t, m, short); !strings.Contains(view, "Beta") || !strings.Contains(view, connectUnusableMark) {
		t.Fatalf("beta's unusable key is not marked:\n%s", view)
	}
	m = pressKey(t, m, tea.KeyDown) // from Alpha, the first unconnected, to Beta
	m = pressKey(t, m, tea.KeyEnter)
	if view, want := connectView(t, m, short), "The stored key cannot be used: it is shorter than 8"; !strings.Contains(view, want) {
		t.Fatalf("beta's field does not say its stored key cannot be used:\n%s", view)
	}
	m = pressKey(t, m, tea.KeyEsc)
	m = pressKey(t, m, tea.KeyUp) // back to Alpha
	m = pressKey(t, m, tea.KeyEnter)
	tm, _ := m.Update(pasteKey(connectCanary))
	m = pressKey(t, tm.(Model), tea.KeyEnter)
	connectLeak(t, m, stub, short)

	want := `the stored Beta key cannot be used: it is shorter than 8 bytes; replace it with /connect, or remove it with "craze auth logout beta"`
	if got := texts(m, entryNote); len(got) != 2 || got[1] != want {
		t.Fatalf("notes %q, want the notice and %q", got, want)
	}
	if got, ok := storedKey(t, dir, "beta"); !ok || got != short {
		t.Fatal("the save changed beta's stored key")
	}
}

// TestConnectStoreErrorsNeverQuoteTheKey (§3.14, A7): a save the store
// refuses — providers.toml a symlink here — is an error row naming the
// problem, not the key; and a store error that did quote a key would be
// replaced whole (storeErrText).
func TestConnectStoreErrorsNeverQuoteTheKey(t *testing.T) {
	dir, getenv := connectFixture(t, nil)
	path := filepath.Join(dir, modeltable.ProvidersFile)
	target := filepath.Join(t.TempDir(), "elsewhere.toml")
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	stub := nativeStub()
	tm, _ := connectKeyStep(t, stub, dir, getenv).Update(pasteKey(connectCanary))
	m := pressKey(t, tm.(Model), tea.KeyEnter)
	connectLeak(t, m, stub)
	errs := texts(m, entryError)
	if len(errs) != 1 || !strings.HasPrefix(errs[0], "/connect: providers.toml is a symlink") {
		t.Fatalf("error rows %q, want the store's refusal", errs)
	}
	if len(texts(m, entryNote)) != 0 {
		t.Fatal("a refused save wrote the notice")
	}

	quoting := errors.New("modeltable: something about " + connectCanary)
	if got := storeErrText(quoting, "  "+connectCanary+"\n"); got != "the key was not saved" {
		t.Fatal("a store error quoting the key would reach the error row")
	}
}

// TestNativeModelDialogConnectRow (§3.6, Q4): native's /model list ends with
// "Connect a provider…" while some provider in the TUI's own table has no
// key, judged through the seams, and not once every one has; selecting the
// row opens /connect. An ACP session's dialog never has it, whatever the
// seams would say.
func TestNativeModelDialogConnectRow(t *testing.T) {
	models := []agent.ModelInfo{{ID: "alpha/one", Name: "Alpha One"}, {ID: "beta/fast", Name: "Beta Fast"}}
	open := func(t *testing.T, stub *Stub, stored map[string]string) Model {
		t.Helper()
		stub.SetModels(models)
		dir, getenv := connectFixture(t, stored)
		m := connectModel(t, stub, dir, getenv)
		m, _ = typeCommand(t, m, "/model")
		if m.dialog != dialogModel {
			t.Fatalf("fixture: /model opened dialog %v", m.dialog)
		}
		return m
	}
	lastRow := func(m Model) string {
		p := m.modelDialogPlan(1 << 10)
		if p.top+p.shown-1 < len(p.list) {
			return modelRowText(p.list[p.top+p.shown-1])
		}
		return connectRowText
	}

	m := open(t, nativeStub(), nil)
	if view := connectView(t, m); !m.mdlg.connect || lastRow(m) != connectRowText || !strings.Contains(view, connectRowText) {
		t.Fatalf("gamma has no key, and the list does not end with the connect row:\n%s", view)
	}
	m = pressKey(t, m, tea.KeyUp) // wraps from the current model to the last row
	if m.mdlg.sel != len(m.dialogModelList()) {
		t.Fatalf("↑ from the top lands on row %d, not the connect row", m.mdlg.sel)
	}
	m = pressKey(t, m, tea.KeyEnter)
	if m.dialog != dialogConnect {
		t.Fatalf("enter on the connect row opened dialog %v, not /connect", m.dialog)
	}

	const gammaKey = "sk-picker-gamma-dummy"
	m = open(t, nativeStub(), map[string]string{"gamma": gammaKey})
	if view := connectView(t, m, gammaKey); m.mdlg.connect || strings.Contains(view, connectRowText) {
		t.Fatalf("every provider has a key, and the list still ends with the connect row:\n%s", view)
	}

	m = open(t, NewStub(), nil)
	if view := connectView(t, m); m.mdlg.connect || strings.Contains(view, connectRowText) {
		t.Fatalf("an ACP session's /model has the connect row:\n%s", view)
	}
}

// TestConnectRowRefusedWhileBusy: the connect row is /connect, refusal
// included — the model dialog closes and the refusal is written.
func TestConnectRowRefusedWhileBusy(t *testing.T) {
	stub := nativeStub()
	stub.SetModels([]agent.ModelInfo{{ID: "alpha/one", Name: "Alpha One"}})
	dir, getenv := connectFixture(t, nil)
	m := connectModel(t, stub, dir, getenv)
	m, _ = typeCommand(t, m, "/model")
	m = pressKey(t, m, tea.KeyUp)
	m.status = statusWorking
	m = pressKey(t, m, tea.KeyEnter)
	if m.dialog != dialogNone || !slices.Equal(texts(m, entryError), []string{connectBusyText}) {
		t.Fatalf("the connect row while busy: dialog %v, errors %q", m.dialog, texts(m, entryError))
	}
}

// keyFieldCursor is where /connect's key field draws its cursor — the one
// cell of the field's row drawn in reverse video — as a column of the box's
// inner row (0 is the prompt's first cell), and whether it is drawn at all: a
// cursor bubbles drew past the field's width is clipped off the row with the
// cell under it, leaving the reverse-video switch with no cell behind it. The
// frame is screened for keys first (connectView).
func keyFieldCursor(t *testing.T, m Model, keys ...string) (col int, ok bool) {
	t.Helper()
	connectView(t, m, keys...)
	r := m.lay.Dialog
	if r.Empty() || m.dialog != dialogConnect || m.cdlg.step != connectKey {
		t.Fatalf("fixture: no key field on screen (dialog %v, box %+v)", m.dialog, r)
	}
	// The border, then the title: the field is the box's third row.
	row := strings.Split(m.View(), "\n")[r.Y+2]
	const reverse = "\x1b[7m"
	i := strings.Index(row, reverse)
	if i < 0 || i+len(reverse) >= len(row) || row[i+len(reverse)] == ansi.ESC {
		return 0, false
	}
	return ansi.StringWidth(row[:i]) - r.X - 1, true
}

// TestConnectLongKeyKeepsItsCursor (astra r4 3): a key longer than the field
// scrolls under the cursor, which stays on the row — at the paste's end, at
// the start after Home, where ← leaves it within the tail with the window
// held still, and across a resize that narrows the box and one that widens it
// back. The field holds the width it is drawn at (fitDialogFields), and the
// key is drawn masked, and nowhere else.
func TestConnectLongKeyKeepsItsCursor(t *testing.T) {
	long := "sk-long-" + strings.Repeat("0123456789", 9) + "ab"
	if len(long) != 100 {
		t.Fatalf("fixture: the key is %d bytes", len(long))
	}
	dir, getenv := connectFixture(t, nil)
	stub := nativeStub()
	m := connectKeyStep(t, stub, dir, getenv)
	tm, _ := m.Update(pasteKey(long))
	m = tm.(Model)
	connectLeak(t, m, stub, long)

	// at is the cursor's column, or a failure saying it is not drawn; inner
	// is the box's inner width now, whose last cell is the one after the
	// prompt and a full field.
	at := func(t *testing.T, m Model, when string) (col, inner int) {
		t.Helper()
		inner = m.lay.Dialog.W - dialogBorder
		if w := dialogFieldWidth(inner); m.cdlg.key.Width != w {
			t.Fatalf("%s: the field holds width %d, its row gives %d", when, m.cdlg.key.Width, w)
		}
		col, ok := keyFieldCursor(t, m, long)
		if !ok {
			t.Fatalf("%s: the cursor is not drawn: clipped off the box", when)
		}
		return col, inner
	}

	end, inner := at(t, m, "the paste")
	if end != inner-1 {
		t.Fatalf("after the paste the cursor is at column %d, want the row's last, %d", end, inner-1)
	}
	m = pressKey(t, m, tea.KeyHome)
	if col, _ := at(t, m, "home"); col != lipgloss.Width(modelFilterPrompt) {
		t.Fatalf("home puts the cursor at column %d, want the field's first, %d", col, lipgloss.Width(modelFilterPrompt))
	}
	m = pressKey(t, m, tea.KeyEnd)
	for range 10 {
		m = pressKey(t, m, tea.KeyLeft)
	}
	// The window stays where the end left it: the cursor moves ten cells
	// left on the row, the ten cells after it still drawn.
	if col, _ := at(t, m, "ten lefts from the end"); col != end-10 {
		t.Fatalf("ten lefts from the end put the cursor at column %d, want %d", col, end-10)
	}

	tm, _ = m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m = tm.(Model)
	col, narrow := at(t, m, "a resize to 40 columns")
	if narrow >= inner || col < 0 || col >= narrow {
		t.Fatalf("at 40 columns the cursor is at column %d of %d (was %d wide)", col, narrow, inner)
	}
	m = pressKey(t, m, tea.KeyRight)
	at(t, m, "a right at 40 columns")

	tm, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	if col, wide := at(t, m, "a resize back to 100 columns"); wide != inner || col < 0 || col >= wide {
		t.Fatalf("back at 100 columns the cursor is at column %d of %d", col, wide)
	}
	m = pressKey(t, m, tea.KeyEnd)
	if col, _ := at(t, m, "end at 100 columns"); col != end {
		t.Fatalf("end at 100 columns puts the cursor at column %d, want %d", col, end)
	}
	if m.cdlg.key.Value() != long {
		t.Fatal("moving and resizing changed the key in the field")
	}
	connectLeak(t, m, stub, long)
}

// TestConnectKeyPasteWithTheSessionListOpen (X45): the paste paths merged
// with plan 030's session list. A clipboard paste asked for in the key field
// that answers once the session has ended and craze is back on the list is
// dropped — it reaches neither the list's input nor the composer's draft the
// list covers; and one asked for in the list's input that answers once the
// list has closed and the key field is open never reaches the key field.
func TestConnectKeyPasteWithTheSessionListOpen(t *testing.T) {
	prevRead := clipboardRead
	clipboardRead = func() (string, error) { return connectCanary, nil }
	t.Cleanup(func() { clipboardRead = prevRead })
	dir, getenv := connectFixture(t, nil)
	listModel := func(t *testing.T, stub *Stub) Model {
		t.Helper()
		// A list that can start sessions has an input for a paste to land
		// in (plan 030 §3.13).
		fs := &startSessions{fakeSessions: &fakeSessions{}}
		return connectModelOf(t, Config{Session: stub, NativeDir: dir, Getenv: getenv, Sessions: fs})
	}

	for _, end := range []struct {
		name string
		msg  endMsg
	}{{"the session's end", endMsg{}}, {"the connection lost", endMsg{err: errors.New("the socket went away")}}} {
		t.Run("a key paste after "+end.name, func(t *testing.T) {
			stub := nativeStub()
			m, _ := typeCommand(t, listModel(t, stub), "/connect")
			m = pressKey(t, m, tea.KeyEnter)
			m, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlV})
			msg, ok := runCmd(cmd).(pasteMsg)
			if !ok || msg.key == (keyField{}) || msg.text == "" {
				t.Fatal("fixture: ctrl+v in the key field answered no paste tagged with the field")
			}
			m = applyMsg(t, m, end.msg)
			if !m.sessList.open || !m.sessList.in.on || m.dialog != dialogNone {
				t.Fatalf("fixture: %s did not go back to a list with an input (open %v, dialog %v)", end.name, m.sessList.open, m.dialog)
			}
			m = applyMsg(t, m, msg)
			connectLeak(t, m, stub)
			if v, _ := inputOf(m); v != "" || m.input.Value() != "" || m.cdlg.key.Value() != "" {
				t.Fatal("a paste asked for in the key field landed with the session list open")
			}
		})
	}

	t.Run("a list paste after the key field opened", func(t *testing.T) {
		stub := nativeStub()
		m := openList(t, listModel(t, stub))
		if !m.sessList.in.on {
			t.Fatal("fixture: the list has no input")
		}
		m, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlV})
		msg, ok := runCmd(mustCmd(t, cmd, "pasteFromClipboard")).(pasteMsg)
		if !ok || msg.listGen == 0 || msg.key != (keyField{}) {
			t.Fatal("fixture: ctrl+v in the list's input answered no paste tagged with the list")
		}
		m = pressKey(t, m, tea.KeyEsc)
		if m.sessList.open {
			t.Fatal("fixture: esc on the empty input did not leave the list")
		}
		m, _ = typeCommand(t, m, "/connect")
		m = pressKey(t, m, tea.KeyEnter)
		if m.cdlg.openField() == (keyField{}) {
			t.Fatal("fixture: the key field is not open")
		}
		m = applyMsg(t, m, msg)
		connectLeak(t, m, stub)
		if m.cdlg.key.Value() != "" || m.input.Value() != "" {
			t.Fatal("a paste asked for in the list's input landed in the key field or the composer")
		}
	})
}

// TestConnectLateAnswers: an answer for an opening that is no longer the one
// on screen changes nothing on screen (plan 031 §3.9, X41). A providers' read
// that answers once its box has closed, or once another opening has read its
// own, is dropped; so is the connect row's answer for a model dialog opened
// again since. A save's answer — the save happened — writes its line whatever
// is open now, and leaves a box opened since, and the key being typed into
// it, as they are; one that answers after a switch to another session is
// that conversation's no longer, and is dropped.
func TestConnectLateAnswers(t *testing.T) {
	dir, getenv := connectFixture(t, nil)
	stub := nativeStub()
	m, _ := typeCommand(t, connectModel(t, stub, dir, getenv), "/connect")
	late := connectLoadedMsg{gen: m.cdlg.gen, err: "a read that answered late"}

	m = pressKey(t, m, tea.KeyEsc)
	if m = applyMsg(t, m, late); m.dialog != dialogNone || len(texts(m, entryError)) != 0 {
		t.Fatalf("a read for a closed box: dialog %v, errors %q", m.dialog, texts(m, entryError))
	}
	m, _ = typeCommand(t, m, "/connect")
	if m.cdlg.gen == late.gen || !m.cdlg.loaded {
		t.Fatal("fixture: the box opened again did not read its own providers")
	}
	again := m.cdlg
	if m = applyMsg(t, m, late); m.cdlg.loadErr != "" || m.cdlg.sel != again.sel || len(m.cdlg.providers) != len(again.providers) {
		t.Fatalf("the first opening's read changed the second's box: error %q, selection %d (was %d)", m.cdlg.loadErr, m.cdlg.sel, again.sel)
	}

	m = pressKey(t, m, tea.KeyEnter)
	tm, _ := m.Update(pasteKey(connectCanary))
	m = tm.(Model)
	field := m.cdlg.openField()
	m = applyMsg(t, m, connectSavedMsg{shownGen: m.shownGen, name: "Alpha"})
	m = applyMsg(t, m, connectSavedMsg{shownGen: m.shownGen, err: "the key store did not answer in time"})
	connectLeak(t, m, stub)
	if m.dialog != dialogConnect || m.cdlg.openField() != field || m.cdlg.key.Value() != connectCanary {
		t.Fatalf("a save's answer touched the box opened since: dialog %v", m.dialog)
	}
	if notes, errs := texts(m, entryNote), texts(m, entryError); !slices.Equal(notes, []string{connectedNote("Alpha", nil)}) ||
		!slices.Equal(errs, []string{"/connect: the key store did not answer in time"}) {
		t.Fatalf("a save's answers wrote notes %q, errors %q", notes, errs)
	}

	// Another session shown, as a switch makes it (openUnstarted, the
	// list's opens): the session's state afresh, under a shown generation of
	// its own.
	before := m.shownGen
	m = m.withSession(sessionSeed{workspace: m.cwd, provider: "native"})
	m.shownGen++
	if m = applyMsg(t, m, connectSavedMsg{shownGen: before, name: "Beta"}); slices.Contains(texts(m, entryNote), connectedNote("Beta", nil)) {
		t.Fatal("a save's answer from the conversation left behind was written in this one")
	}

	// The model dialog's connect row: gamma has no key, so each opening's
	// answer says to show it; the first opening's, false by fiat, lands on
	// the second and must change nothing.
	mstub := nativeStub()
	mstub.SetModels([]agent.ModelInfo{{ID: "alpha/one", Name: "Alpha One"}})
	md, _ := typeCommand(t, connectModel(t, mstub, dir, getenv), "/model")
	first := md.mdlg.gen
	md = pressKey(t, md, tea.KeyEsc)
	md, _ = typeCommand(t, md, "/model")
	if md.mdlg.gen == first || !md.mdlg.connect {
		t.Fatal("fixture: the model dialog opened again has no connect row of its own")
	}
	if md = applyMsg(t, md, connectRowMsg{gen: first, show: false}); !md.mdlg.connect {
		t.Fatal("the first model dialog's answer took the connect row off the second")
	}
}

// TestConnectDoubleEnterSavesOnce: Enter on the key field closes the box as
// it issues the save, so a second Enter typed before the save answers — held
// by the command gate while the store is written — lands on the empty
// composer, not on a field: one key stored, one notice, nothing sent to the
// agent. A /connect typed behind them opens after the save's notice, and its
// own read shows the provider connected.
func TestConnectDoubleEnterSavesOnce(t *testing.T) {
	dir, getenv := connectFixture(t, nil)
	stub := nativeStub()
	t.Cleanup(func() { _ = stub.Close() })
	m := connectKeyStep(t, stub, dir, getenv)
	tm, _ := m.Update(pasteKey(connectCanary))
	r := newGateRig(t, asyncGate(t, tm.(Model)))

	r.send(enter())
	if r.m.gate == nil || len(r.calls) != 1 || r.m.dialog != dialogNone {
		t.Fatalf("the save: gate open %v, %d calls, dialog %v — want the box closed, the save out", r.m.gate != nil, len(r.calls), r.m.dialog)
	}
	r.send(enter())
	for _, k := range "/connect" {
		r.send(runeKey(k))
	}
	r.send(enter())
	if len(r.m.held) != len("/connect")+2 {
		t.Fatalf("held %v while the save was out", heldKinds(r.m))
	}
	r.answer()
	r.drainAll()
	// The /connect behind the second Enter opened the box, whose read is a
	// gated call of its own.
	for len(r.calls) > 0 {
		r.answer()
		r.drainAll()
	}
	connectLeak(t, r.m, stub)
	if got, ok := storedKey(t, dir, "gamma"); !ok || got != connectCanary {
		t.Fatalf("gamma's key was not stored as pasted (stored: %v)", ok)
	}
	if notes := texts(r.m, entryNote); !slices.Equal(notes, []string{connectedNote("Gamma", nil)}) {
		t.Fatalf("notes %q, want one notice", notes)
	}
	if errs := texts(r.m, entryError); len(errs) != 0 || len(stub.Prompts()) != 0 {
		t.Fatalf("errors %q, prompts sent %d", errs, len(stub.Prompts()))
	}
	if r.m.dialog != dialogConnect || !r.m.cdlg.loaded || r.m.cdlg.step != connectPick {
		t.Fatalf("the /connect typed behind the save: dialog %v, loaded %v", r.m.dialog, r.m.cdlg.loaded)
	}
	for _, p := range r.m.cdlg.providers {
		if p.ID == "gamma" && connectMark(p) != connectConnectedMark {
			t.Fatal("the box opened after the save does not show gamma connected")
		}
	}
}

// TestConnectedNoticeKeepsTheFlagWhole (plan 031 C7r, verification V3): at
// 110 columns the notice for Meta fills its first row up to `craze`, and
// `-c.` moves to the next row whole — never `craze` / ` -` / `c.`, which the
// transcript's word wrap made of a word that starts with a hyphen
// (holdFlagHyphens).
func TestConnectedNoticeKeepsTheFlagWhole(t *testing.T) {
	dir, getenv := connectFixture(t, nil)
	m := connectModel(t, nativeStub(), dir, getenv)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	m = applyMsg(t, tm.(Model), connectSavedMsg{shownGen: tm.(Model).shownGen, name: "Meta"})
	var rows []string
	for _, ln := range strings.Split(connectView(t, m), "\n") {
		rows = append(rows, strings.TrimRight(ln, " "))
	}
	i := slices.Index(rows, "Connected Meta. New sessions offer its models; to use them in this conversation, /exit and run craze")
	if i < 0 || i+1 >= len(rows) || rows[i+1] != "-c." {
		t.Fatalf("the notice at 110 columns is not `… run craze` / `-c.`:\n%s", strings.Join(rows, "\n"))
	}
}
