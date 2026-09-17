package host

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Status is compared with == by the TUI and by Publish; a slice or map field
// would stop this compiling.
var _ = Status{} == Status{}

func TestDeriveGatesOnReady(t *testing.T) {
	for name, in := range map[string]Input{
		"nothing":            {},
		"card before ready":  {Card: PermissionCard, CardLabel: "permission x"},
		"error before ready": {Errored: true, Err: "boom"},
		"working in replay":  {Working: true},
		"foreign in replay":  {ForeignTurn: true},
		"idle with identity": {NoTurnYet: true, SessionID: "s", Provider: "cursor", Model: "m"},
	} {
		if s, ok := Derive(in); ok {
			t.Errorf("%s: published %+v before the session was ready", name, s)
		}
	}
	// A start failure is the one status a session that never came up has.
	s, ok := Derive(Input{StartFailed: true, Err: "spawn: no such file", Provider: "grok"})
	want := Status{Kind: Failed, Message: "spawn: no such file", Detail: DetailStartFailed, Provider: "grok"}
	if !ok || s != want {
		t.Fatalf("start failed: ok=%v %+v, want %+v", ok, s, want)
	}
}

// TestDerivePriority pins every pair of the four states, highest first:
// a card over an error over a turn over idle (the title's order).
func TestDerivePriority(t *testing.T) {
	card := func(in Input) Input { in.Card, in.CardLabel = QuestionCard, "question"; return in }
	errored := func(in Input) Input { in.Errored, in.Err = true, "boom"; return in }
	startFailed := func(in Input) Input { in.StartFailed, in.Err = true, "boom"; return in }
	working := func(in Input) Input { in.Working = true; return in }
	foreign := func(in Input) Input { in.ForeignTurn = true; return in }
	ready := Input{Ready: true}

	for _, tc := range []struct {
		name string
		in   Input
		kind Kind
	}{
		{"idle alone", ready, Idle},
		{"working alone", working(ready), Working},
		{"foreign alone", foreign(ready), Working},
		{"error alone", errored(ready), Failed},
		{"start failed alone", startFailed(Input{}), Failed},
		{"card alone", card(ready), Blocked},

		{"card over error", card(errored(ready)), Blocked},
		{"card over start failed", card(startFailed(Input{})), Blocked},
		{"card over working", card(working(ready)), Blocked},
		{"card over foreign", card(foreign(ready)), Blocked},
		{"error over working", errored(working(ready)), Failed},
		{"error over foreign", errored(foreign(ready)), Failed},
		{"start failed over working", startFailed(working(Input{})), Failed},
		{"start failed over foreign", startFailed(foreign(Input{})), Failed},

		{"everything", card(errored(startFailed(working(foreign(ready))))), Blocked},
	} {
		s, ok := Derive(tc.in)
		if !ok || s.Kind != tc.kind {
			t.Errorf("%s: ok=%v kind %v, want %v", tc.name, ok, s.Kind, tc.kind)
		}
	}
}

func TestDeriveDetailsAndMessages(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Input
		want Status
	}{
		{"ready before any turn", Input{Ready: true, NoTurnYet: true},
			Status{Kind: Idle, Detail: DetailReady}},
		{"ready outranks a cancel flag", Input{Ready: true, NoTurnYet: true, Cancelled: true},
			Status{Kind: Idle, Detail: DetailReady}},
		{"stop after a turn", Input{Ready: true},
			Status{Kind: Idle, Detail: DetailStop}},
		{"cancelled turn", Input{Ready: true, Cancelled: true},
			Status{Kind: Idle, Detail: DetailCancelled}},

		{"prompt", Input{Ready: true, Working: true},
			Status{Kind: Working, Detail: DetailPrompt}},
		{"foreign turn only", Input{Ready: true, ForeignTurn: true},
			Status{Kind: Working, Detail: DetailForeignTurn}},
		{"prompt and foreign turn", Input{Ready: true, Working: true, ForeignTurn: true},
			Status{Kind: Working, Detail: DetailPrompt}},

		{"permission card", Input{Ready: true, Card: PermissionCard, CardLabel: "permission Shell"},
			Status{Kind: Blocked, Message: "permission Shell", Detail: DetailPermissionPrompt}},
		{"question card", Input{Ready: true, Card: QuestionCard, CardLabel: "question"},
			Status{Kind: Blocked, Message: "question", Detail: DetailQuestion}},
		{"plan card", Input{Ready: true, Card: PlanCard, CardLabel: "plan refactor"},
			Status{Kind: Blocked, Message: "plan refactor", Detail: DetailPlan}},

		{"error keeps its first line", Input{Ready: true, Errored: true, Err: "  rate limited  \nretry after 30s\n"},
			Status{Kind: Failed, Message: "rate limited", Detail: DetailError}},
		{"error after blank lines", Input{Ready: true, Errored: true, Err: "\r\n\n boom\r\nstack"},
			Status{Kind: Failed, Message: "boom", Detail: DetailError}},
		{"empty error falls back", Input{Ready: true, Errored: true},
			Status{Kind: Failed, Message: "turn failed", Detail: DetailError}},
		{"blank error falls back", Input{Ready: true, Errored: true, Err: " \n\t\n"},
			Status{Kind: Failed, Message: "turn failed", Detail: DetailError}},
		{"start failed detail wins over error", Input{StartFailed: true, Errored: true},
			Status{Kind: Failed, Message: "turn failed", Detail: DetailStartFailed}},

		{"identity carried and trimmed", Input{Ready: true, SessionID: " sess-1 ", Provider: "  cursor\t", Model: " gpt-5 "},
			Status{Kind: Idle, Detail: DetailStop, SessionID: " sess-1 ", Provider: "cursor", Model: "gpt-5"}},
	} {
		s, ok := Derive(tc.in)
		if !ok || s != tc.want {
			t.Errorf("%s: ok=%v\n got  %+v\n want %+v", tc.name, ok, s, tc.want)
		}
	}
}

func TestDeriveTruncates(t *testing.T) {
	check := func(name, got string, limit int) {
		t.Helper()
		if w := ansi.StringWidth(got); w > limit {
			t.Errorf("%s: %d cells, cap %d", name, w, limit)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("%s: truncated without an ellipsis: %q", name, got)
		}
	}

	long := strings.Repeat("x", 300)
	wide := strings.Repeat("界", 150) // 300 cells

	s, _ := Derive(Input{Ready: true, Errored: true, Err: long + "\nsecond line"})
	check("error", s.Message, MessageCap)
	if ansi.StringWidth(s.Message) != MessageCap {
		t.Errorf("an ASCII error fills the cap exactly: %d cells", ansi.StringWidth(s.Message))
	}
	s, _ = Derive(Input{Ready: true, Errored: true, Err: wide})
	check("wide error", s.Message, MessageCap)
	s, _ = Derive(Input{Ready: true, Card: PermissionCard, CardLabel: "permission " + long})
	check("card label", s.Message, MessageCap)
	s, _ = Derive(Input{Ready: true, Provider: "  " + long + "  ", Model: wide})
	check("provider", s.Provider, MetaCap)
	check("model", s.Model, MetaCap)

	// Exactly at the cap is not truncated.
	exact := strings.Repeat("y", MessageCap)
	if s, _ := Derive(Input{Ready: true, Errored: true, Err: exact}); s.Message != exact {
		t.Errorf("a message at the cap was changed: %q", s.Message)
	}
}
