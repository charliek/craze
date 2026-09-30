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
	isolateSkillsHome(t)
	m := New(Config{Session: stub, Theme: "tokyo-night", Workspace: t.TempDir(), Model: "grok", Yolo: true, NativeDir: dir, Getenv: getenv})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
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

// connectLeak fails the test when the canary is anywhere the user or a log
// could read it: the frame, the transcript's entries, the composer, what the
// session was sent, and the dialog's own state once it has closed. It names
// where, never the key.
func connectLeak(t *testing.T, m Model, stub *Stub, key string) {
	t.Helper()
	places := map[string]string{
		"the frame":    plainView(m),
		"the composer": m.input.Value(),
		"the prompts":  strings.Join(stub.Prompts(), "\n"),
	}
	for _, e := range m.main.entries() {
		places["the transcript"] += e.text + "\n"
	}
	if m.dialog != dialogConnect {
		places["the closed dialog's field"] = m.cdlg.key.Value()
	}
	for where, text := range places {
		if strings.Contains(text, key) {
			t.Fatalf("the key reached %s", where)
		}
	}
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

	view := plainView(m)
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
	view = plainView(m)
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
	if !strings.Contains(plainView(m), strings.Repeat(string(connectMask), len(connectCanary))) {
		t.Fatalf("the field does not show the key masked:\n%s", plainView(m))
	}
	connectLeak(t, m, stub, connectCanary)

	m = pressKey(t, m, tea.KeyEnter)
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
	connectLeak(t, m, stub, connectCanary)
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
	view := plainView(m)
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
		if m.dialog != dialogConnect || m.cdlg.step != connectKey {
			t.Fatalf("%s: the refusal closed the field", tc.why)
		}
		if m.cdlg.keyErr != tc.why || !strings.Contains(plainView(m), "Not saved: ") {
			t.Fatalf("the refusal reads %q, want %q", m.cdlg.keyErr, tc.why)
		}
		if m.cdlg.key.Value() != "" {
			t.Fatalf("%s: the refused key is still in the field", tc.why)
		}
		if _, ok := storedKey(t, dir, "gamma"); ok {
			t.Fatalf("%s: a refused key was stored", tc.why)
		}
		if strings.TrimSpace(tc.key) != "" {
			connectLeak(t, m, stub, tc.key)
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
	m := connectKeyStep(t, nativeStub(), dir, getenv)
	tm, _ := m.Update(pasteKey(connectCanary))
	m = tm.(Model)
	m.status = statusWorking
	m = pressKey(t, m, tea.KeyEnter)
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

// TestConnectFieldEmptiedOnEveryExit (§3.9, astra 15): Esc, a card arriving,
// the model dialog opening over it, a save and a session switch each leave no
// key in the model.
func TestConnectFieldEmptiedOnEveryExit(t *testing.T) {
	dir, getenv := connectFixture(t, nil)
	for _, tc := range []struct {
		name string
		exit func(t *testing.T, m Model, stub *Stub) Model
	}{
		{"esc", func(t *testing.T, m Model, _ *Stub) Model { return pressKey(t, m, tea.KeyEsc) }},
		{"a card", func(t *testing.T, m Model, stub *Stub) Model {
			return cardEvent(t, m, stub, agent.Event{Type: agent.EventQuestion, Question: stubQuestion()})
		}},
		{"another dialog", func(_ *testing.T, m Model, _ *Stub) Model { return m.openHelp() }},
		{"a save", func(t *testing.T, m Model, _ *Stub) Model { return pressKey(t, m, tea.KeyEnter) }},
		{"a session switch", func(_ *testing.T, m Model, _ *Stub) Model {
			return m.withSession(sessionSeed{workspace: m.cwd, provider: "native"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := nativeStub()
			tm, _ := connectKeyStep(t, stub, dir, getenv).Update(pasteKey(connectCanary))
			m := tc.exit(t, tm.(Model), stub)
			if m.cdlg.key.Value() != "" || m.cdlg.openField() != (keyField{}) {
				t.Fatalf("the key field outlived %s", tc.name)
			}
			connectLeak(t, m, stub, connectCanary)
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
	if !strings.Contains(plainView(m), "Beta") || !strings.Contains(plainView(m), connectUnusableMark) {
		t.Fatalf("beta's unusable key is not marked:\n%s", plainView(m))
	}
	m = pressKey(t, m, tea.KeyDown) // from Alpha, the first unconnected, to Beta
	m = pressKey(t, m, tea.KeyEnter)
	if want := "The stored key cannot be used: it is shorter than 8"; !strings.Contains(plainView(m), want) {
		t.Fatalf("beta's field does not say its stored key cannot be used:\n%s", plainView(m))
	}
	m = pressKey(t, m, tea.KeyEsc)
	m = pressKey(t, m, tea.KeyUp) // back to Alpha
	m = pressKey(t, m, tea.KeyEnter)
	tm, _ := m.Update(pasteKey(connectCanary))
	m = pressKey(t, tm.(Model), tea.KeyEnter)

	want := `the stored Beta key cannot be used: it is shorter than 8 bytes; replace it with /connect, or remove it with "craze auth logout beta"`
	if got := texts(m, entryNote); len(got) != 2 || got[1] != want {
		t.Fatalf("notes %q, want the notice and %q", got, want)
	}
	if got, ok := storedKey(t, dir, "beta"); !ok || got != short {
		t.Fatal("the save changed beta's stored key")
	}
	connectLeak(t, m, stub, connectCanary)
	connectLeak(t, m, stub, short)
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
	errs := texts(m, entryError)
	if len(errs) != 1 || !strings.HasPrefix(errs[0], "/connect: providers.toml is a symlink") {
		t.Fatalf("error rows %q, want the store's refusal", errs)
	}
	if len(texts(m, entryNote)) != 0 {
		t.Fatal("a refused save wrote the notice")
	}
	connectLeak(t, m, stub, connectCanary)

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
	if !m.mdlg.connect || lastRow(m) != connectRowText || !strings.Contains(plainView(m), connectRowText) {
		t.Fatalf("gamma has no key, and the list does not end with the connect row:\n%s", plainView(m))
	}
	m = pressKey(t, m, tea.KeyUp) // wraps from the current model to the last row
	if m.mdlg.sel != len(m.dialogModelList()) {
		t.Fatalf("↑ from the top lands on row %d, not the connect row", m.mdlg.sel)
	}
	m = pressKey(t, m, tea.KeyEnter)
	if m.dialog != dialogConnect {
		t.Fatalf("enter on the connect row opened dialog %v, not /connect", m.dialog)
	}

	m = open(t, nativeStub(), map[string]string{"gamma": "sk-picker-gamma-dummy"})
	if m.mdlg.connect || strings.Contains(plainView(m), connectRowText) {
		t.Fatalf("every provider has a key, and the list still ends with the connect row:\n%s", plainView(m))
	}

	m = open(t, NewStub(), nil)
	if m.mdlg.connect || strings.Contains(plainView(m), connectRowText) {
		t.Fatalf("an ACP session's /model has the connect row:\n%s", plainView(m))
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
