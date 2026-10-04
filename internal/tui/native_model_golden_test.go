package tui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// Native's /model (plan 031 §3.6, owner decision Q4): only the models of
// providers that have a key, plus the one the session runs on; the current
// model first and pre-selected, then the remembered ones, newest first, then
// the rest by name — and no "current" or "recent" label anywhere. Every
// session here opens on a fixture table, a fixture environment and a memory
// in its own home, all through agent.NewNative's seam: never the shipped
// catalog, the developer's environment or their recent.json.

// nativePickerTable is three providers: alpha and beta have keys in
// nativePickerEnv, gamma has none. Names sort differently from the memory's
// order, so the frame shows the order came from the memory.
func nativePickerTable() *modeltable.Table {
	return &modeltable.Table{
		NoCatalog:    true,
		DefaultModel: "alpha/one",
		Providers: map[string]modeltable.Provider{
			"alpha": {Name: "Alpha", Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"PICKER_ALPHA_KEY"}},
			"beta":  {Name: "Beta", Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"PICKER_BETA_KEY"}},
			"gamma": {Name: "Gamma", Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"PICKER_GAMMA_KEY"}},
		},
		Models: map[string]modeltable.Model{
			"alpha/one": {Provider: "alpha", WireModel: "wire-one", Name: "Alpha One", Efforts: []string{"low", "high"}, DefaultEffort: "high"},
			"alpha/two": {Provider: "alpha", WireModel: "wire-two", Name: "Alpha Two"},
			"beta/fast": {Provider: "beta", WireModel: "wire-fast", Name: "Beta Fast"},
			"beta/slow": {Provider: "beta", WireModel: "wire-slow", Name: "Beta Slow"},
			"gamma/big": {Provider: "gamma", WireModel: "wire-big", Name: "Gamma Big"},
		},
	}
}

// nativePickerEnv funds alpha and beta, never gamma, with dummy keys.
func nativePickerEnv(k string) string {
	switch k {
	case "PICKER_ALPHA_KEY":
		return "sk-picker-alpha-dummy"
	case "PICKER_BETA_KEY":
		return "sk-picker-beta-dummy"
	}
	return ""
}

// nativePickerSession is a native session on nativePickerTable, started with
// --model alpha/one, whose home remembers — newest first — gamma/big (its
// provider has no key, so it is passed over), beta/slow and alpha/two.
func nativePickerSession(t *testing.T, ws string) agent.Session {
	t.Helper()
	return nativePickerSessionAt(t, ws, t.TempDir())
}

// nativePickerSessionAt is nativePickerSession with home as the directory the
// session reads its models from and remembers in: the TUI's own native
// directory (Config.NativeDir) for a fixture that stands for one process, as
// nativePickerConfig and signInConfig are, so a refresh's sameDir is true as it
// is in the real program (plan 034 §3.4, A26).
func nativePickerSessionAt(t *testing.T, ws, home string) agent.Session {
	t.Helper()
	table := nativePickerTable()
	at := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	for _, alias := range []string{"alpha/two", "beta/slow", "gamma/big"} { // oldest first
		m := table.Models[alias]
		if err := modeltable.Remember(home, modeltable.RecentEntry{Alias: alias, Provider: m.Provider, WireModel: m.WireModel}, at); err != nil {
			t.Fatal(err)
		}
	}
	// The package's own seam, with this table's environment and the
	// memory's clock in place of its fixed ones.
	tweak := nativeSessionTweak(home, table, &nativeScriptedModel{provider: "alpha", wire: "wire-one"})
	return agent.NewNative(
		agent.Options{Workspace: ws, ContentHome: t.TempDir(), Model: "alpha/one", Interactive: true, NoPrimary: frameNoPrimary},
		func(o *harness.Options) {
			tweak(o)
			o.Getenv = nativePickerEnv
			o.Now = func() time.Time { return at }
		})
}

// nativePickerConfig is the TUI over nativePickerSession, its own seams
// (Config.NativeDir, Config.Getenv) on connectFixture's directory and
// environment with the keys given stored: what /connect and the connect row
// read, never the shipped catalog or the developer's environment (plan 031
// §3.13).
func nativePickerConfig(t *testing.T, stored map[string]string) Config {
	t.Helper()
	ws := frameWorkspace(t)
	dir, getenv := connectFixture(t, stored)
	return Config{Session: nativePickerSessionAt(t, ws, dir), Theme: "tokyo-night", Workspace: ws, Yolo: true, NativeDir: dir, Getenv: getenv}
}

// TestFrameGoldenNativeModelDialog is the dialog as it opens: Alpha One, the
// model the session runs on, first and pre-selected; Beta Slow and Alpha Two,
// the remembered ones, newest first; Beta Fast after them; and nothing of
// gamma's, remembered or not. The list's rows are the models' names alone, and
// it ends with "Connect a provider…": gamma has no key in the TUI's table
// either (plan 031 §3.6).
func TestFrameGoldenNativeModelDialog(t *testing.T) {
	isolateSkillsHome(t)
	got, _, err := runFrameModes(t, func() Config {
		return nativePickerConfig(t, nil)
	}, 100, 30, "<wait:idle>/model<enter><wait:text:Beta Fast>", FrameOpts{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "native-model-dialog-100x30", 100, 30, got,
		[]string{"> Alpha One", "Beta Slow", "Alpha Two", "Beta Fast", connectRowText},
		[]string{"Gamma", "current", "recent"})
	rest := got
	for _, row := range []string{"Alpha One", "Beta Slow", "Alpha Two", "Beta Fast", connectRowText} {
		i := strings.Index(rest, row)
		if i < 0 {
			t.Fatalf("the rows are not in the memory's order; %q is not after the ones before it:\n%s", row, got)
		}
		rest = rest[i+len(row):]
	}
}

// TestFrameGoldenNativeModelDialogAllConnected: once gamma's key is stored in
// the TUI's directory — as /connect or craze auth login would store it — no
// provider is left without one, and the list has no connect row. The running
// session still does not offer gamma's model: it keeps the table it started
// with (P8), which is what the notice after /connect says.
//
// The answer that decides the row is read in a gated call (connectCall), so no
// frame before it lands satisfies a wait: the frame here is the one after it.
func TestFrameGoldenNativeModelDialogAllConnected(t *testing.T) {
	isolateSkillsHome(t)
	got, _, err := runFrameModes(t, func() Config {
		return nativePickerConfig(t, map[string]string{"gamma": "sk-picker-gamma-dummy"})
	}, 100, 30, "<wait:idle>/model<enter><wait:text:Beta Fast>", FrameOpts{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "native-model-dialog-connected-100x30", 100, 30, got,
		[]string{"> Alpha One", "Beta Slow", "Alpha Two", "Beta Fast"},
		[]string{"Connect a provider", "Gamma", "sk-picker-gamma-dummy"})
}

// TestNativeHiddenModelIsUnknownToTheTUI (§3.6): the TUI's own list is the
// session's advertised one — ordered for the dialog as the frame above shows
// it — and a typed /model naming a model it hides fails exactly as one naming
// a model that does not exist, with no switch sent.
func TestNativeHiddenModelIsUnknownToTheTUI(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	sess := nativePickerSession(t, ws)
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	m := New(Config{Session: sess, Theme: "tokyo-night", Workspace: ws, Yolo: true})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	// The start's install delta, which says which model the session is on.
	// Start enqueues it without waiting for its delivery, so it is waited
	// for here rather than drained: at one CPU the log may not have
	// committed it yet.
	for m.snap.CurrentModel == "" {
		select {
		case ev := <-sess.Events():
			tm, _ = m.Update(eventMsg{ev: ev})
			m = tm.(Model)
		case <-time.After(10 * time.Second):
			t.Fatal("the session's install delta never arrived")
		}
	}

	var ids []string
	for _, md := range m.dialogModelList() {
		ids = append(ids, md.ID)
	}
	if want := []string{"alpha/one", "beta/slow", "alpha/two", "beta/fast"}; !slices.Equal(ids, want) {
		t.Fatalf("the dialog lists %v, want %v", ids, want)
	}
	_, _, unknown := resolveModelArgs(m.snap, "nope/z", true)
	_, _, hidden := resolveModelArgs(m.snap, "gamma/big", true)
	if unknown == nil || hidden == nil || hidden.Error() != `unknown model "gamma/big"` || unknown.Error() != `unknown model "nope/z"` {
		t.Fatalf("/model of a hidden model = %v; want what an unknown one gives (%v)", hidden, unknown)
	}
}
