package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// /connect's frames (plan 031 §3.9): a real native session — nativePickerSession,
// or frameSubagents' for a turn held open — with the TUI's seams on
// connectFixture's directory and environment (nativePickerConfig): alpha and
// beta have keys in that environment, gamma none. Never the shipped catalog,
// the developer's environment or their ~/.craze. The providers' read is a gated
// call (connectCall), so a wait for a provider's name is a wait for the read.

// runConnectFrame runs keys over nativePickerConfig at cols x rows, in every
// transport and gate mode, and answers the frame they agree on — each run's
// frame and error screened for secrets first (connectScreen), so none is
// compared, printed or held against a golden before it has passed.
func runConnectFrame(t *testing.T, cols, rows int, keys string, secrets ...string) string {
	t.Helper()
	isolateSkillsHome(t)
	got, _, err := runFrameModesScreened(t, func() Config {
		return nativePickerConfig(t, nil)
	}, cols, rows, keys, FrameOpts{Timeout: 20 * time.Second}, connectScreen(t, secrets...))
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	return got
}

// connectScreen is a frameScreen that fails the test when a run's frame,
// plain or raw, or its error shows connectCanary or one of keys, whole or in
// a piece (connectShown), naming the run and never the key (astra r4 2): it is
// told each run as it ends, before the runs are compared or a golden is
// compared or written, -update included.
func connectScreen(t *testing.T, keys ...string) frameScreen {
	return func(run, plain, raw string, err error) {
		t.Helper()
		places := []struct{ where, text string }{{"frame", plain}, {"raw frame", raw}, {"error", fmt.Sprint(err)}}
		for _, key := range append([]string{connectCanary}, keys...) {
			for _, p := range places {
				if connectShown(p.text, key) {
					t.Fatalf("the %s run's %s shows the key (<CANARY>)", run, p.where)
				}
			}
		}
	}
}

// TestFrameGoldenNativeConnect is step one: every provider of the TUI's table
// by display name, alpha and beta marked connected, the box open on gamma —
// the first with no key.
func TestFrameGoldenNativeConnect(t *testing.T) {
	for _, size := range []struct{ cols, rows int }{{80, 24}, {100, 30}} {
		got := runConnectFrame(t, size.cols, size.rows, "<wait:idle>/connect<enter><wait:text:Gamma>")
		assertFrameGolden(t, fmt.Sprintf("native-connect-%dx%d", size.cols, size.rows), size.cols, size.rows, got,
			[]string{"Connect a provider", "  Alpha", "  Beta", "> Gamma", "✓", connectPickHint},
			[]string{"Fireworks", "OpenRouter", "Z.AI"})
	}
}

// TestFrameGoldenNativeConnectKey is step two, a key pasted into the field —
// masked, never drawn — with the file it goes to named: beta's at 100x30,
// whose variable is set in the TUI's environment and says it wins; gamma's at
// 80x24, which has none and says nothing of one.
func TestFrameGoldenNativeConnectKey(t *testing.T) {
	const key = "sk-connect-golden-dummy-key"
	for _, tc := range []struct {
		cols, rows int
		name       string
		keys       string
		want, not  []string
	}{
		{100, 30, "native-connect-key-100x30",
			"<wait:idle>/connect<enter><wait:text:Gamma><up><enter><wait:text:Beta API key><paste:" + key + ">",
			[]string{"Beta API key", "Stored in ~/.craze/native/providers.toml.", "PICKER_BETA_KEY is set", connectKeyHint},
			nil},
		{80, 24, "native-connect-key-80x24",
			"<wait:idle>/connect<enter><wait:text:Gamma><enter><wait:text:Gamma API key><paste:" + key + ">",
			[]string{"Gamma API key", "Stored in ~/.craze/native/providers.toml.", connectKeyHint},
			[]string{"is set in this environment"}},
	} {
		got := runConnectFrame(t, tc.cols, tc.rows, tc.keys, key)
		assertFrameGolden(t, tc.name, tc.cols, tc.rows, got,
			append(tc.want, "❯ "+strings.Repeat(string(connectMask), len(key))), tc.not)
	}
}

// TestFrameGoldenNativeConnectBusy is /connect typed while a turn runs: the
// refusal row, no dialog, the draft gone, the turn going on.
func TestFrameGoldenNativeConnectBusy(t *testing.T) {
	isolateSkillsHome(t)
	got, _, err := runFrameModes(t, func() Config {
		ws := frameWorkspace(t)
		echo, two := newFrameRouter("test", "wire-echo"), newFrameRouter("test", "wire-two")
		echo.hold("go", frameOpenText("working on it"))
		dir, getenv := connectFixture(t, nil)
		return Config{
			Session:   frameSubagents(t, ws, echo, two, &frameClock{}, nil),
			Theme:     "tokyo-night",
			Workspace: ws,
			Yolo:      true,
			NativeDir: dir,
			Getenv:    getenv,
		}
	}, 80, 24, "<wait:idle>go<enter><wait:text:working on it>/connect<enter><wait:text:"+connectBusyText+">",
		FrameOpts{Timeout: 20 * time.Second, Freeze: true})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "native-connect-busy-80x24", 80, 24, got,
		[]string{connectBusyText, "working on it"},
		[]string{"Connect a provider", "❯ /connect"})
}

// TestFrameNativeConnectSaves is the save, through the real program in every
// transport and gate mode: the notice, and the key in the fixture's
// providers.toml — nowhere on screen.
func TestFrameNativeConnectSaves(t *testing.T) {
	isolateSkillsHome(t)
	const key = "sk-connect-frame-save-key"
	var dirs []string
	got, _, err := runFrameModesScreened(t, func() Config {
		cfg := nativePickerConfig(t, nil)
		dirs = append(dirs, cfg.NativeDir)
		return cfg
	}, 100, 30, "<wait:idle>/connect<enter><wait:text:Gamma><enter><wait:text:Gamma API key><paste:"+key+"><enter>"+
		"<wait:text:Connected Gamma.>", FrameOpts{Timeout: 20 * time.Second}, connectScreen(t, key))
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "", 100, 30, got,
		[]string{"Connected Gamma. New sessions offer its models; to use them in this conversation, /exit and run", "craze -c."},
		[]string{"Connect a provider", "Gamma API key"})
	if len(dirs) == 0 {
		t.Fatal("no run built its Config")
	}
	for _, dir := range dirs {
		if stored, ok := storedKey(t, dir, "gamma"); !ok || stored != key {
			t.Fatalf("a run's providers.toml does not hold gamma's key as pasted (stored: %v)", ok)
		}
	}
}
