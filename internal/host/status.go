// Package host reports craze's state to the terminal multiplexer it runs in
// (plan 015): herdr's pane.report_agent and roost's tab.agent_report. The TUI
// derives one Status per transition and hands it to a Hub, which fans it out to
// one worker per host without ever blocking the TUI.
//
// The package imports nothing from internal/tui or internal/acp: the TUI maps
// its own model onto Input, so the priority rule and the wire shapes can be
// tested without a Model or a session.
package host

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Kind is the one host-visible state. The order of the switch in Derive, not
// the numeric order here, is the priority.
type Kind int

const (
	Idle Kind = iota
	Working
	Blocked
	Failed
)

func (k Kind) String() string {
	switch k {
	case Idle:
		return "idle"
	case Working:
		return "working"
	case Blocked:
		return "blocked"
	case Failed:
		return "failed"
	}
	return "unknown"
}

// Card is which card, if any, is waiting on the user.
type Card int

const (
	NoCard Card = iota
	PermissionCard
	QuestionCard
	PlanCard
)

// The Detail values Derive produces. Reporters key off them (roost's table
// in plan 015 §3.4 is per Detail), so they are named once here.
const (
	DetailReady            = "ready"
	DetailPrompt           = "prompt"
	DetailForeignTurn      = "foreign_turn"
	DetailPermissionPrompt = "permission_prompt"
	DetailQuestion         = "question"
	DetailPlan             = "plan"
	DetailError            = "error"
	DetailStartFailed      = "start_failed"
	DetailCancelled        = "cancelled"
	DetailStop             = "stop"
)

// failedFallback is the Failed message for an error with no text: roost
// rejects attention=set with an empty body (plan 015 §3.1).
const failedFallback = "turn failed"

// messageCap is the widest Message a host is sent, in cells. metaCap is
// herdr's token value cap, applied to Provider and Model.
const (
	messageCap = 200
	metaCap    = 80
)

// Input is the TUI's state reduced to what the host status depends on. The
// TUI fills it from its own model; nothing here reads a tui type.
type Input struct {
	Ready       bool   // sessionReady(): started && !replaying
	StartFailed bool   // errMsg landed (the session never came up)
	Working     bool   // statusWorking
	Errored     bool   // statusError
	Err         string // Model.err; Derive keeps its first line
	Card        Card
	CardLabel   string // the card's own header text
	ForeignTurn bool
	Cancelled   bool // the last turn ended cancelled (either ending)
	// NoTurnYet is true until the session's first prompt is sent: the Idle
	// the session comes up with is "ready", not a turn that stopped. It is
	// its own field rather than a turn counter because the TUI's turnSeq
	// starts at 1, not 0, and that mapping belongs to the TUI.
	NoTurnYet                  bool
	SessionID, Provider, Model string
}

// Status is one host-visible state. It is comparable with ==, which is how
// both the TUI and the Hub drop a transition that changes nothing a host sees.
type Status struct {
	Kind    Kind
	Message string // one line, ≤ 200 cells; "" for Idle and Working
	// Detail is one of the Detail constants.
	Detail                     string
	SessionID, Provider, Model string
}

// Derive computes the host status for in. ok=false means publish nothing:
// until the session is ready (or failed to start) a host hears nothing, which
// keeps a pre-start Idle with no provider or model off the wire and keeps a
// session/load replay from ever raising a false "Turn complete" (plan 015
// §3.1).
//
// The priority is the tab title's (internal/tui/title.go): a card waiting on
// the user outranks an error, which outranks a turn in progress, which
// outranks idle.
func Derive(in Input) (Status, bool) {
	if !in.Ready && !in.StartFailed {
		return Status{}, false
	}
	s := Status{
		SessionID: in.SessionID,
		Provider:  capMeta(in.Provider),
		Model:     capMeta(in.Model),
	}
	switch {
	case in.Card != NoCard:
		s.Kind = Blocked
		s.Message = capMessage(in.CardLabel)
		s.Detail = cardDetail(in.Card)
	case in.Errored || in.StartFailed:
		s.Kind = Failed
		s.Message = capMessage(firstLine(in.Err))
		if s.Message == "" {
			s.Message = failedFallback
		}
		s.Detail = DetailError
		if in.StartFailed {
			s.Detail = DetailStartFailed
		}
	case in.Working || in.ForeignTurn:
		s.Kind = Working
		s.Detail = DetailPrompt
		if !in.Working {
			s.Detail = DetailForeignTurn
		}
	default:
		s.Kind = Idle
		switch {
		case in.NoTurnYet:
			s.Detail = DetailReady
		case in.Cancelled:
			s.Detail = DetailCancelled
		default:
			s.Detail = DetailStop
		}
	}
	return s, true
}

func cardDetail(c Card) string {
	switch c {
	case PermissionCard:
		return DetailPermissionPrompt
	case QuestionCard:
		return DetailQuestion
	case PlanCard:
		return DetailPlan
	}
	return ""
}

// firstLine is the first non-blank line of s, trimmed: an error's headline,
// not a leading blank line and not the stack of detail under it.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// capMessage bounds a message in display cells. The TUI sanitises controls
// before Input, but the bound is here as well so no caller can hand a host an
// unbounded line.
func capMessage(s string) string {
	return ansi.Truncate(s, messageCap, "…")
}

func capMeta(s string) string {
	return ansi.Truncate(strings.TrimSpace(s), metaCap, "…")
}
