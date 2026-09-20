package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// The shell mode through the real program (plan 022 §3.6). Nothing here moves
// an existing golden: every frame below is one a draft starting with `!`
// produced, and nothing else in craze reads that byte.

// binSh pins the shell for a frame whose golden is the output of a command.
// $SHELL is the developer's, and a golden must not be.
func binSh(t *testing.T) {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh")
}

// TestFrameGoldenShellComposer100x30 is the whole visible change: the two rules
// take the shell colour and the top one says what Enter and Esc do, in the
// space the session title had. No row was added for it.
func TestFrameGoldenShellComposer100x30(t *testing.T) {
	plain, raw := runStubFrameRaw(t, 100, 30, "<wait:idle>!ls /usr")
	assertGolden(t, "shell-composer-100x30", 100, 30, plain)
	if !strings.Contains(plain, shellRuleTitle) {
		t.Fatalf("the rule does not say what the mode is:\n%s", plain)
	}
	if strings.Contains(plain, " craze ─") {
		t.Fatalf("the session title should have given way to the mode:\n%s", plain)
	}
	th := Preset("tokyo-night")
	if !strings.Contains(raw, ansiFG(string(th.Shell))) {
		t.Fatal("the composer's rules are not painted in the shell colour")
	}
	if th.Shell == th.Rule {
		t.Fatal("the shell colour must differ from the ordinary rule")
	}
	// One `!` away, the composer is the composer again — there is no mode flag
	// to leave behind.
	back := runStubFrame(t, 100, 30, "<wait:idle>!ls /usr<backspace><backspace><backspace><backspace><backspace><backspace><backspace><backspace>")
	if !strings.Contains(back, " craze ─") || strings.Contains(back, shellRuleTitle) {
		t.Fatalf("deleting the draft did not leave shell mode:\n%s", back)
	}
}

// TestFrameGoldenShellRow100x30 is the transcript half, from a real command run
// by the real program: the mark, the command, and its output underneath.
func TestFrameGoldenShellRow100x30(t *testing.T) {
	binSh(t)
	got := runStubFrame(t, 100, 30, "<wait:idle>!echo one; echo two<enter><wait:text:✓ !>")
	assertGolden(t, "shell-row-100x30", 100, 30, got)
	for _, want := range []string{"✓ ! echo one; echo two", "  one", "  two", " craze ─"} {
		if !strings.Contains(got, want) {
			t.Fatalf("frame is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, shellRuleTitle) {
		t.Fatalf("the draft went with the command, so the mode did too:\n%s", got)
	}
	if strings.Contains(got, "❯ echo one") {
		t.Fatalf("a command is not a prompt and gets no user row:\n%s", got)
	}
}

// TestFrameShellModeHidesTheAgentsOwnCatalog is the menu suppression against a
// catalog that really would have matched: the fake advertises `zulu-tool`, and
// `!ls /zulu` must not offer it.
func TestFrameShellModeHidesTheAgentsOwnCatalog(t *testing.T) {
	// The catalog arrives after session/new replies, so the script waits for a
	// row of it before the assertion can mean anything, then clears the draft.
	const landed = "/zulu<wait:text:zulu-tool>" + "<backspace><backspace><backspace><backspace><backspace>"
	got := runFakeFrame(t, "commands", 100, 30, "<wait:idle>"+landed+"!ls /zulu")
	if !strings.Contains(got, shellRuleTitle) {
		t.Fatalf("the frame is not in shell mode:\n%s", got)
	}
	if strings.Contains(got, "zulu-tool") {
		t.Fatalf("the slash menu opened over a shell draft:\n%s", got)
	}
}

// TestShellColourIsDerivedForEveryPreset: the new slot is derived from the
// eleven hand-picked colours, so no preset row changed for it — which is why
// the theme dialog's golden could not move.
func TestShellColourIsDerivedForEveryPreset(t *testing.T) {
	for _, name := range ThemeNames() {
		th := Preset(name)
		var spec paletteSpec
		for _, p := range palettes {
			if p.name == name {
				spec = p
			}
		}
		if want := blend(spec.border, spec.purple, shellMix); th.Shell != want {
			t.Fatalf("%s: Shell = %s, want Border blended %d%% towards Purple (%s)", name, th.Shell, shellMix, want)
		}
		// It has to read as a different line from the one it replaces, and it
		// must not vanish into the background it is drawn on.
		for _, other := range []struct {
			name string
			c    lipgloss.Color
		}{{"Rule", th.Rule}, {"Border", th.Border}, {"BG", th.BG}} {
			if th.Shell == other.c {
				t.Fatalf("%s: Shell must not equal %s", name, other.name)
			}
		}
	}
}
