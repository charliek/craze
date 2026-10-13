package tui

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The frames plan 047 C2 adds (§7 A3, A4): the model dialog's effort row on
// a highlighted model of a real native session, and on one with no levels;
// the /effort picker on that session; and the slash menu on `/ef`.

// nativeEffortTable is nativePickerTable with levels on more of its models:
// Alpha Two six of them, on a high default — the session remembers it at max
// — and Beta Slow three with no default and no memory; Alpha One keeps low
// and high, on a high default; Beta Fast has none.
func nativeEffortTable() *modeltable.Table {
	t := nativePickerTable()
	t.Models = maps.Clone(t.Models)
	two := t.Models["alpha/two"]
	two.Efforts, two.DefaultEffort = []string{"minimal", "low", "medium", "high", "xhigh", "max"}, "high"
	t.Models["alpha/two"] = two
	slow := t.Models["beta/slow"]
	slow.Efforts = []string{"low", "medium", "high"}
	t.Models["beta/slow"] = slow
	return t
}

// nativeEffortConfig is the TUI over a native session on nativeEffortTable,
// started with --model alpha/one, whose home remembers — newest first —
// gamma/big (unfunded, passed over), beta/slow and alpha/two at max; its own
// seams on connectFixture's directory, as nativePickerConfig's are.
func nativeEffortConfig(t *testing.T) Config {
	t.Helper()
	ws := frameWorkspace(t)
	dir, getenv := connectFixture(t, nil)
	table := nativeEffortTable()
	at := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	for _, r := range []struct{ alias, effort string }{{"alpha/two", "max"}, {"beta/slow", ""}, {"gamma/big", ""}} { // oldest first
		m := table.Models[r.alias]
		if err := modeltable.Remember(dir, modeltable.RecentEntry{Alias: r.alias, Provider: m.Provider, WireModel: m.WireModel, Effort: r.effort}, at); err != nil {
			t.Fatal(err)
		}
	}
	tweak := nativeSessionTweak(dir, table, &nativeScriptedModel{provider: "alpha", wire: "wire-one"})
	sess := agent.NewNative(
		agent.Options{Workspace: ws, ContentHome: t.TempDir(), Model: "alpha/one", Interactive: true, NoPrimary: frameNoPrimary},
		func(o *harness.Options) {
			tweak(o)
			o.Getenv = nativePickerEnv
			o.Now = func() time.Time { return at }
		})
	return Config{Session: sess, Theme: "tokyo-night", Workspace: ws, Yolo: true, NativeDir: dir, Getenv: getenv}
}

// TestFrameGoldenNativeModelDialogEffort (A3): the dialog with Alpha Two
// highlighted draws Alpha Two's six levels on the max it is remembered at,
// not Alpha One's; with Beta Fast highlighted, which has no levels, it draws
// no effort row, and its footer names no tab.
func TestFrameGoldenNativeModelDialogEffort(t *testing.T) {
	isolateSkillsHome(t)
	open := "<wait:idle>/model<enter><wait:text:Beta Fast>"
	for _, tc := range []struct {
		name, keys   string
		want, absent []string
	}{
		{"native-model-dialog-highlight-100x30", open + "<down><down>",
			[]string{"> Alpha Two", "  effort  minimal  low  medium  high  xhigh  [max]", "tab effort"},
			[]string{"[high]", "[low]"}},
		{"native-model-dialog-no-effort-100x30", open + "<down><down><down>",
			[]string{"> Beta Fast", modelDialogHintPlain},
			[]string{"effort", "[max]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := runFrameModes(t, func() Config { return nativeEffortConfig(t) }, 100, 30, tc.keys, FrameOpts{Timeout: 20 * time.Second})
			if err != nil {
				t.Fatalf("run frame script: %v", err)
			}
			assertFrameGolden(t, tc.name, 100, 30, got, tc.want, tc.absent)
		})
	}
}

// TestFrameGoldenEffortPicker (A4): /effort low, then a bare /effort, on the
// native session: the picker titled with the model, its two levels, the
// cursor on low, marked current, and high, Alpha One's default, marked
// default.
func TestFrameGoldenEffortPicker(t *testing.T) {
	isolateSkillsHome(t)
	got, _, err := runFrameModes(t, func() Config { return nativeEffortConfig(t) }, 100, 30,
		"<wait:idle>/effort low<enter><wait:text:Alpha One (low)>/effort<enter><wait:text:current>",
		FrameOpts{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "effort-picker-100x30", 100, 30, got,
		[]string{"effort · Alpha One", "> low", "current", "  high", "default", effortPickerHint},
		[]string{"[low]", "minimal"})
	for _, row := range rows(got) {
		if strings.Contains(row, "> low") && strings.Contains(row, "default") {
			t.Fatalf("low is marked default:\n%s", got)
		}
	}
}

// TestFrameGoldenSlashEffort (A4): `/ef` on a session whose model has an
// effort option lists /effort alone, with its sentence.
func TestFrameGoldenSlashEffort(t *testing.T) {
	got := runStubFrame(t, 100, 30, "<wait:idle>/ef")
	assertFrameGolden(t, "slash-effort-100x30", 100, 30, got,
		[]string{"❯ /effort  Change the current model's effort"},
		[]string{"/exit", "1/1"})
}
