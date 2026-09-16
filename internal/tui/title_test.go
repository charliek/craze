package tui

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
)

// TestWindowTitleStates is the table over every state §3.10 pins: the text
// half before and after a title exists, and the mark for each of the four
// states in isolation.
func TestWindowTitleStates(t *testing.T) {
	cases := []struct {
		name string
		m    Model
		want string
	}{
		{"pre-title idle", Model{}, "✦ craze"},
		{"idle with a title", Model{snap: agent.Snapshot{Title: "fix flaky pty test"}}, "✦ fix flaky pty test"},
		{
			"working: status", Model{status: statusWorking, snap: agent.Snapshot{Title: "fix flaky pty test"}},
			"❖ fix flaky pty test",
		},
		{
			"working: replaying", Model{replaying: true, snap: agent.Snapshot{Title: "fix flaky pty test"}},
			"❖ fix flaky pty test",
		},
		{
			"working: foreign turn",
			Model{snap: agent.Snapshot{Title: "fix flaky pty test", ForeignTurn: true}},
			"❖ fix flaky pty test",
		},
		{
			"error", Model{status: statusError, snap: agent.Snapshot{Title: "fix flaky pty test"}},
			"✕ fix flaky pty test",
		},
		{
			"needs you: card open",
			Model{cards: []card{{kind: cardPermission}}, snap: agent.Snapshot{Title: "fix flaky pty test"}},
			"⚠ fix flaky pty test",
		},
		{
			"pre-start picker is idle",
			Model{pickingProvider: true},
			"✦ craze",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.windowTitle(); got != tc.want {
				t.Fatalf("windowTitle() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWindowTitlePrecedence holds the priority order §3.10 pins — needs you
// over error over working over idle — by turning states on cumulatively and
// checking the mark never regresses to a lower-priority one.
func TestWindowTitlePrecedence(t *testing.T) {
	m := Model{}
	if got := m.windowTitle(); !strings.HasPrefix(got, titleMarkIdle) {
		t.Fatalf("baseline mark = %q, want idle", got)
	}

	m.status = statusWorking
	if got := m.windowTitle(); !strings.HasPrefix(got, titleMarkWorking) {
		t.Fatalf("working mark = %q, want %q", got, titleMarkWorking)
	}

	m.status = statusError
	if got := m.windowTitle(); !strings.HasPrefix(got, titleMarkError) {
		t.Fatalf("error mark = %q, want %q — error must outrank working", got, titleMarkError)
	}

	m.cards = []card{{kind: cardQuestion}}
	if got := m.windowTitle(); !strings.HasPrefix(got, titleMarkNeedsYou) {
		t.Fatalf("needs-you mark = %q, want %q — a card must outrank error and working", got, titleMarkNeedsYou)
	}
}

// TestWindowTitleStrongSendIsNotNeedsYou is the one explicit non-obviousness
// in §3.10: the strong-send confirm line is the user's own keystroke landing
// on its own dialog, not the agent asking for something, so it must not raise
// the warning mark the way an open card does.
func TestWindowTitleStrongSendIsNotNeedsYou(t *testing.T) {
	m := Model{confirm: &strongSend{text: "go"}}
	if got := m.windowTitle(); !strings.HasPrefix(got, titleMarkIdle) {
		t.Fatalf("a strong-send confirm raised the warning mark: %q", got)
	}
}

// TestWindowTitleCutsAt40 is the truncation half of §3.10: over the cap, an
// ellipsis lands at exactly 40 display cells; at or under, the title is
// carried whole.
func TestWindowTitleCutsAt40(t *testing.T) {
	long := strings.Repeat("a", 60)
	m := Model{snap: agent.Snapshot{Title: long}}
	got := m.windowTitle()
	text := strings.TrimPrefix(got, titleMarkIdle+" ")
	if n := len([]rune(text)); n != titleTextCap {
		t.Fatalf("truncated text is %d runes, want %d: %q", n, titleTextCap, text)
	}
	if !strings.HasSuffix(text, "…") {
		t.Fatalf("truncated text should end in an ellipsis: %q", text)
	}

	short := strings.Repeat("b", titleTextCap)
	m = Model{snap: agent.Snapshot{Title: short}}
	got = m.windowTitle()
	if want := titleMarkIdle + " " + short; got != want {
		t.Fatalf("a title exactly at the cap should not be cut: %q, want %q", got, want)
	}
}

// TestWindowTitleSanitizesControlCharacters is the same guarantee
// sanitizeLine gives the composer title: escape sequences and control
// characters cannot ride into the terminal's own title bar through an
// agent-chosen name.
func TestWindowTitleSanitizesControlCharacters(t *testing.T) {
	m := Model{snap: agent.Snapshot{Title: "fix\x1b[31m flaky\tpty\ntest"}}
	got := m.windowTitle()
	if strings.ContainsAny(got, "\x1b\t\n") {
		t.Fatalf("control characters survived into the title: %q", got)
	}
	if want := "✦ fix flaky pty test"; got != want {
		t.Fatalf("windowTitle() = %q, want %q", got, want)
	}
}

// pipeSyncWriter is a *syncWriter over a real *os.File pair, so Run's exit
// clear is asserted through the exact type it writes with (§3.10): a
// syncWriter needs an *os.File to satisfy Fd(), which a plain bytes.Buffer
// cannot give it.
func pipeSyncWriter(t *testing.T) (*syncWriter, func() []byte) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return newSyncWriter(w), func() []byte {
		_ = w.Close()
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
}

// TestClearWindowTitleWritesOSCReset is Run's exit-path clear: once a title
// was ever set, the same writer bubbletea rendered into gets OSC 2 with an
// empty title, so Ghostty and roost fall back to their own derived tab name
// instead of a stale "✦ craze" outliving the process.
func TestClearWindowTitleWritesOSCReset(t *testing.T) {
	sw, drain := pipeSyncWriter(t)
	clearWindowTitle(sw, Model{lastTitle: "✦ craze"})
	if got, want := drain(), []byte(ansi.SetWindowTitle("")); string(got) != string(want) {
		t.Fatalf("clear wrote %q, want %q", got, want)
	}
}

// TestClearWindowTitleSkippedWhenNeverSet covers both reasons lastTitle can
// still be "": the off switch (Config.TerminalTitle false, as every existing
// golden and unit-test model leaves it) and a run that ended before its first
// Update. Neither needs the terminal to fall back to anything, so neither
// should write a byte.
func TestClearWindowTitleSkippedWhenNeverSet(t *testing.T) {
	sw, drain := pipeSyncWriter(t)
	clearWindowTitle(sw, Model{})
	if got := drain(); len(got) != 0 {
		t.Fatalf("clear wrote %q when no title was ever set", got)
	}
}
