package tui

import (
	"strings"
	"testing"

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
