package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/harness"
)

// The lossless codec is agent.Event as one JSON object (plan 020 §3.3): what
// the session's event log keeps in its ring, what the journal writes to disk,
// and what a subscriber replays. internal/cli/events.go is a different thing
// on purpose — a projection that caps, drops and renames for a headless
// caller — and is not built on this.
//
// The wire is its own set of structs rather than tags on the agent types, so
// the shape on disk changes only when this file does, and a field added to an
// agent type reaches the wire by a line here — which the completeness test
// (eventcodec_test.go) refuses to let anyone forget. The flat leaf types
// (Todo, ToolDiff, PermissionOption, Option, ToolOutput, PluginCommand,
// ForeignTurnInfo, ReplayInfo, TurnInfo) are converted rather than copied
// field by field: their wire twins have the same fields in the same order, so
// a field added to one of them stops this file compiling until the twin has
// it too. Held by pointer, they convert as pointers, and a nil one converts
// to nil.
//
// JSON cannot carry every Go value bit for bit, so "lossless" is defined, and
// the tests hold the codec to exactly this:
//
//   - A nil and an empty slice or map are the same event, and decode yields
//     nil. Nothing in internal/tui or internal/cli branches on the
//     difference. The one exception is a slice held as a map value
//     (QuestionEvent.Answers): craze prompt --json writes a nil answer list
//     as null and an empty one as [], and acp.AskAutoAnswers produces nil
//     for a question with no options, so there the difference is kept.
//   - A pointer's presence is kept: a non-nil pointer to a zero value (an
//     ExitCode of 0, a bare &TaskInfo{} that makes ToolEvent.IsTask true)
//     decodes non-nil.
//   - A time is written as RFC 3339 with nanoseconds, in UTC. It decodes as
//     the same instant (time.Time.Equal) in UTC; the monotonic reading and
//     the location are gone. A zero time decodes zero. A year outside
//     0000–9999 cannot be written and is an encode error.
//   - An error keeps its message, its EventErrClass and its code (below),
//     and decodes as a *RemoteError that errors.Is matches against its
//     class's sentinel.
//   - A string is carried as JSON carries it: invalid UTF-8 becomes U+FFFD.
//     What the agent sent has already been through a JSON decoder or
//     sanitizeText, which make the same replacement. The rest — the user's
//     own text, a plugin file's body in ExpandedCommand.Text — reached the
//     agent as JSON too, so the replaced text is what the agent was given.
//
// Every other field is carried exactly, uncapped, except Event.Seq: the
// sequence number belongs to the record envelope (the event log's Record.Seq,
// the journal line's seq), not to the event's object, so the body is the
// same whatever number the event took.

// EventCodecVersion is the version of the wire shape EncodeEvent writes. A
// decoder ignores keys it does not know, so adding a field is compatible and
// does not bump it; renaming or removing a key, or changing what one means,
// does.
const EventCodecVersion = 1

// EventErrClass is what kind of error an Event.Err was, kept beside its
// message so a decoded event can still be told apart by errors.Is. These are
// the classes of the errors that actually reach Event.Err — the ending of a
// failed turn — and not the refusals a prompt returns instead of running
// (ErrPromptInFlight, ErrPromptCancelled, the queue's errors), which emit
// nothing. The EventErr prefix keeps them apart from any second table of
// classes, such as one for a prompt's own returned error.
type EventErrClass string

const (
	// EventErrRPC is the agent answering a request with a JSON-RPC error.
	// Code is its JSON-RPC code.
	EventErrRPC EventErrClass = "rpc"
	// EventErrClosed is the ACP connection closed under a turn: craze's own
	// Close, or an agent that exited 0 mid-turn.
	EventErrClosed EventErrClass = "closed"
	// EventErrAgentExited is acp.ErrAgentExited wrapped with what the reaper
	// recorded ("…: exit 0"). Today only Close returns it, never a turn; it
	// has a class so an S1b event that reports a close keeps its meaning.
	EventErrAgentExited EventErrClass = "agent_exited"
	// EventErrAgentExitStatus is the agent process ending non-zero or on a
	// signal under a turn: the reaper's "acp: agent exited: exit status N",
	// which wraps the *exec.ExitError and not acp.ErrAgentExited, so it
	// matches no sentinel. Code is the exit status, -1 for a signal.
	EventErrAgentExitStatus EventErrClass = "agent_exit_status"
	// EventErrCanceled and EventErrDeadline are the prompt caller's own
	// context ending the turn: live.go returns ctx.Err() as it is, and the
	// native adapter wraps it (callerEnded).
	EventErrCanceled EventErrClass = "canceled"
	EventErrDeadline EventErrClass = "deadline_exceeded"
	// EventErrAuth, EventErrModelNotFound and EventErrContextTooLarge are the
	// native harness's typed provider failures. Code is the HTTP status, 0
	// when the provider sent none.
	EventErrAuth            EventErrClass = "auth"
	EventErrModelNotFound   EventErrClass = "model_not_found"
	EventErrContextTooLarge EventErrClass = "context_too_large"
	// EventErrEmptyStep is a model that finished a step having sent nothing.
	EventErrEmptyStep EventErrClass = "empty_step"
	// EventErrEmptyPrompt is the harness refusing a prompt of only
	// whitespace. It reaches the stream as a failed native turn, because the
	// adapter opens the turn before the harness looks at the text.
	EventErrEmptyPrompt EventErrClass = "empty_prompt"
	// EventErrHarnessClosed is the native session closed under its turn.
	EventErrHarnessClosed EventErrClass = "harness_closed"
	// EventErrProvider is any other native provider failure: an HTTP error
	// with no typed meaning, a stream error event, a connection that failed.
	// Code is the HTTP status, 0 when there was none.
	EventErrProvider EventErrClass = "provider"
	// EventErrOther is everything else: a write to an agent whose stdin has
	// gone, a frame the decoder could not read, a result that was not JSON,
	// a journal that could not be saved. The message is all there is.
	EventErrOther EventErrClass = "other"
	// EventErrPromptCancelled, EventErrPromptInFlight and EventErrForeignTurn
	// are TurnInfo.ErrClass's own (plan 021 §3.4): the three endings the wire
	// never reports as an Event.Err, because session.go's ErrPromptCancelled,
	// ErrPromptInFlight and ErrForeignTurn are returned from Prompt, never
	// emitted. An engine-authored Synthetic ending carries one of these so a
	// consumer can still tell the endings apart; ClassifyEventErr is how it
	// gets one. The strings are 05's protocol codes.
	EventErrPromptCancelled EventErrClass = "prompt_cancelled"
	EventErrPromptInFlight  EventErrClass = "prompt_in_flight"
	EventErrForeignTurn     EventErrClass = "foreign_turn"
)

// eventErrSentinels pairs each class that has a sentinel with it, in the
// order an error is tested against them. It serves both directions: encode
// classifies by errors.Is down this list, and RemoteError.Is answers from it,
// so a decoded error matches exactly the sentinel its class names. An error
// that matched two of these (none that reaches an event does) keeps the
// first.
var eventErrSentinels = []struct {
	class    EventErrClass
	sentinel error
}{
	{EventErrClosed, acp.ErrClosed},
	{EventErrAgentExited, acp.ErrAgentExited},
	{EventErrCanceled, context.Canceled},
	{EventErrDeadline, context.DeadlineExceeded},
	{EventErrAuth, harness.ErrAuth},
	{EventErrModelNotFound, harness.ErrModelNotFound},
	{EventErrContextTooLarge, harness.ErrContextTooLarge},
	{EventErrEmptyStep, harness.ErrEmptyStep},
	{EventErrEmptyPrompt, harness.ErrEmptyPrompt},
	{EventErrHarnessClosed, harness.ErrClosed},
	// These three never reach classifyEventErr through Event.Err — the
	// refusals they stand for are returned from Prompt and emit nothing — so
	// appending them here cannot change how any error that does reach it
	// classifies today; they exist for ClassifyEventErr to answer when the
	// engine turns one of these refusals into a TurnInfo.ErrClass instead.
	{EventErrPromptCancelled, ErrPromptCancelled},
	{EventErrPromptInFlight, ErrPromptInFlight},
	{EventErrForeignTurn, ErrForeignTurn},
}

// RemoteError is an Event.Err that came back through the codec: the error's
// message, its class and its code, which is everything the codec keeps of an
// error. Error returns the original message unchanged, so what a user reads is
// never reduced to a bare sentinel's text. Is matches the sentinel the class
// stands for, so errors.Is answers on a decoded event the way it did on the
// live one. errors.As for a concrete type (*acp.RPCError,
// *harness.ProviderError) does not: Class and Code are what stands in for it.
type RemoteError struct {
	Message string
	Class   EventErrClass
	Code    int
}

func (e *RemoteError) Error() string {
	if e == nil {
		return "agent: remote error"
	}
	return e.Message
}

// Is reports whether target is the sentinel e's class stands for. A class
// with no sentinel (rpc, agent_exit_status, provider, other), or one a later
// craze wrote that this build does not know, matches nothing.
func (e *RemoteError) Is(target error) bool {
	if e == nil {
		return false
	}
	for _, c := range eventErrSentinels {
		if c.class == e.Class {
			return target == c.sentinel
		}
	}
	return false
}

// classifyEventErr is err's class and code. A *RemoteError keeps its own, so
// an event decoded and encoded again is written exactly as it was first — a
// class this build does not know included. Then the sentinels, in table
// order, and then the typed errors that carry a code but match no sentinel.
func classifyEventErr(err error) (EventErrClass, int) {
	var remote *RemoteError
	if errors.As(err, &remote) && remote != nil {
		return remote.Class, remote.Code
	}
	typed, code := typedEventErr(err)
	for _, c := range eventErrSentinels {
		if errors.Is(err, c.sentinel) {
			return c.class, code
		}
	}
	return typed, code
}

// ClassifyEventErr is classifyEventErr's class alone, exported for a
// component above the seam to call (plan 021 §3.4): the engine has no
// Event.Err to encode when it turns a refusal — ErrPromptCancelled,
// ErrPromptInFlight, ErrForeignTurn — into a TurnInfo's Synthetic ending, only
// the error value itself, and this is how it gets the same class the codec
// would have given it.
func ClassifyEventErr(err error) EventErrClass {
	class, _ := classifyEventErr(err)
	return class
}

// typedEventErr is the class and code of the typed error in err's chain that
// carries a number: the JSON-RPC code, the provider's HTTP status, or the
// agent's exit status. A sentinel class takes the code too, which is how a
// native auth failure keeps its 401.
func typedEventErr(err error) (EventErrClass, int) {
	var rpc *acp.RPCError
	if errors.As(err, &rpc) && rpc != nil {
		return EventErrRPC, rpc.Code
	}
	var pe *harness.ProviderError
	if errors.As(err, &pe) && pe != nil {
		return EventErrProvider, pe.StatusCode
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit != nil {
		return EventErrAgentExitStatus, exit.ExitCode()
	}
	return EventErrOther, 0
}

// EncodeEvent is ev as one JSON object with a "type" discriminator, every
// field it reaches carried and nothing capped. It fails only on a programming
// error — an event with no type, a time JSON cannot write — and the caller
// (the event log) turns that into an omitted record rather than losing the
// event for its live consumer.
func EncodeEvent(ev Event) (string, error) {
	if ev.Type == "" {
		return "", errors.New("agent: encode event: no type")
	}
	e := eventEncoders.Get().(*eventEncoder)
	defer e.release()
	e.w = toWireEvent(ev)
	if err := e.enc.Encode(&e.w); err != nil {
		return "", fmt.Errorf("agent: encode %s event: %w", ev.Type, err)
	}
	// The body is the builder's own buffer, which release gives up rather
	// than reuses, so it shares storage with no later encode. Encode ends it
	// with a newline.
	body := e.out.String()
	return body[:len(body)-1], nil
}

// eventEncoder is the wire struct and the encoder that writes it out, reused.
// Encoding sits on every emit once the event log calls it (plan 020 §3.1).
// The encoder hands its whole body to out in one Write, so out holds no
// buffer between encodes and a large event leaves nothing behind in the pool.
type eventEncoder struct {
	w   wireEvent
	out strings.Builder
	enc *json.Encoder
}

var eventEncoders = sync.Pool{New: func() any {
	e := &eventEncoder{}
	e.enc = json.NewEncoder(&e.out)
	// The body is read by jq and by craze, never embedded in HTML, and a
	// shell command's && and <file are easier to mine unescaped.
	e.enc.SetEscapeHTML(false)
	return e
}}

// release returns e to the pool holding nothing of the event it encoded.
func (e *eventEncoder) release() {
	e.w = wireEvent{}
	e.out.Reset()
	eventEncoders.Put(e)
}

// DecodeEvent is EncodeEvent's inverse, under the equivalence the codec
// defines. Malformed input is an error, never a panic, and so is an object
// with no type.
//
// A type this build does not know is preserved, not refused: the event
// decodes with that Type and whatever fields it carried, and a consumer's
// switch drops it in its default arm, the way every consumer of the live
// stream already treats a type it does not handle. A journal written by a
// later craze therefore still replays in an older one. Keys this build does
// not know are ignored for the same reason.
func DecodeEvent(body string) (Event, error) {
	var w wireEvent
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		return Event{}, fmt.Errorf("agent: decode event: %w", err)
	}
	if w.Type == "" {
		return Event{}, errors.New("agent: decode event: no type")
	}
	return w.event(), nil
}

// wireEvent is Event on the wire. Every key is omitted at its zero value,
// which is lossless (absent decodes zero) and keeps a text delta small; the
// pointers keep their presence because a non-nil pointer is never omitted.
// Every time goes through .UTC() in both directions: written in UTC so the
// text on disk does not depend on the writer's zone, read back in UTC
// whatever offset the text had.
type wireEvent struct {
	Type           string           `json:"type"`
	Text           string           `json:"text,omitempty"`
	Mode           string           `json:"mode,omitempty"`
	Agent          string           `json:"agent,omitempty"`
	Tool           *wireTool        `json:"tool,omitempty"`
	Todos          []wireTodo       `json:"todos,omitempty"`
	Permission     *wirePermission  `json:"permission,omitempty"`
	Question       *wireQuestionEv  `json:"question,omitempty"`
	Plan           *wirePlan        `json:"plan,omitempty"`
	Subagent       *wireSubagent    `json:"subagent,omitempty"`
	SubagentChange string           `json:"subagentChange,omitempty"`
	Queue          *wireQueued      `json:"queue,omitempty"`
	QueueChange    string           `json:"queueChange,omitempty"`
	QueuePos       int              `json:"queuePos,omitempty"`
	Command        *wireCommand     `json:"command,omitempty"`
	Interjection   bool             `json:"interjection,omitempty"`
	ForeignTurn    *wireForeignTurn `json:"foreignTurn,omitempty"`
	Turn           *wireTurn        `json:"turn,omitempty"`
	Ask            *wireAsk         `json:"ask,omitempty"`
	State          *wireState       `json:"state,omitempty"`
	Replay         *wireReplay      `json:"replay,omitempty"`
	Replayed       bool             `json:"replayed,omitempty"`
	Cause          string           `json:"cause,omitempty"`
	Err            *wireError       `json:"err,omitempty"`
	StopReason     string           `json:"stopReason,omitempty"`
	At             time.Time        `json:"at,omitzero"`
}

type wireTool struct {
	ID          string          `json:"id,omitempty"`
	Name        string          `json:"name,omitempty"`
	Status      string          `json:"status,omitempty"`
	Kind        string          `json:"kind,omitempty"`
	Title       string          `json:"title,omitempty"`
	ToolName    string          `json:"toolName,omitempty"`
	RawInput    string          `json:"rawInput,omitempty"`
	ContentText string          `json:"contentText,omitempty"`
	Locations   []string        `json:"locations,omitempty"`
	Output      *wireToolOutput `json:"output,omitempty"`
	Diffs       []wireDiff      `json:"diffs,omitempty"`
	Task        *wireTask       `json:"task,omitempty"`
	At          time.Time       `json:"at,omitzero"`
}

type wireToolOutput struct {
	ExitCode   *int   `json:"exitCode,omitempty"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	Content    string `json:"content,omitempty"`
	StdoutHead string `json:"stdoutHead,omitempty"`
	StderrHead string `json:"stderrHead,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type wireDiff struct {
	Path      string `json:"path,omitempty"`
	OldText   string `json:"oldText,omitempty"`
	NewText   string `json:"newText,omitempty"`
	Added     int    `json:"added,omitempty"`
	Removed   int    `json:"removed,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type wireTask struct {
	Description  string `json:"description,omitempty"`
	Prompt       string `json:"prompt,omitempty"`
	Model        string `json:"model,omitempty"`
	AgentID      string `json:"agentId,omitempty"`
	SubagentType string `json:"subagentType,omitempty"`
	DurationMs   int    `json:"durationMs,omitempty"`
	Receipt      bool   `json:"receipt,omitempty"`
	Status       string `json:"status,omitempty"`
	Background   bool   `json:"background,omitempty"`
}

type wireTodo struct {
	ID      string `json:"id,omitempty"`
	Content string `json:"content,omitempty"`
	Status  string `json:"status,omitempty"`
}

type wirePermission struct {
	ID      string                 `json:"id,omitempty"`
	Tool    string                 `json:"tool,omitempty"`
	Options []wirePermissionOption `json:"options,omitempty"`
}

type wirePermissionOption struct {
	OptionID string `json:"optionId,omitempty"`
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

// wireQuestionEv is QuestionEvent. Answers is the one collection whose
// values keep nil apart from empty (see the codec's rules above): it is
// written as the map it is, so a nil list is null and an empty one is [].
type wireQuestionEv struct {
	ID        string              `json:"id,omitempty"`
	Title     string              `json:"title,omitempty"`
	Questions []wireQuestion      `json:"questions,omitempty"`
	Auto      bool                `json:"auto,omitempty"`
	Answers   map[string][]string `json:"answers,omitempty"`
}

type wireQuestion struct {
	ID            string       `json:"id,omitempty"`
	Prompt        string       `json:"prompt,omitempty"`
	Options       []wireOption `json:"options,omitempty"`
	AllowMultiple bool         `json:"allowMultiple,omitempty"`
}

type wireOption struct {
	ID          string `json:"id,omitempty"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

type wirePlan struct {
	ID       string     `json:"id,omitempty"`
	Name     string     `json:"name,omitempty"`
	Overview string     `json:"overview,omitempty"`
	Plan     string     `json:"plan,omitempty"`
	Todos    []wireTodo `json:"todos,omitempty"`
	Auto     bool       `json:"auto,omitempty"`
	Accepted bool       `json:"accepted,omitempty"`
}

type wireSubagent struct {
	ID           string    `json:"id,omitempty"`
	AttemptID    string    `json:"attemptId,omitempty"`
	ParentID     string    `json:"parentId,omitempty"`
	ToolCallID   string    `json:"toolCallId,omitempty"`
	Description  string    `json:"description,omitempty"`
	SubagentType string    `json:"subagentType,omitempty"`
	Model        string    `json:"model,omitempty"`
	Status       string    `json:"status,omitempty"`
	Error        string    `json:"error,omitempty"`
	Prompt       string    `json:"prompt,omitempty"`
	Output       string    `json:"output,omitempty"`
	Activity     string    `json:"activity,omitempty"`
	StartedAt    time.Time `json:"startedAt,omitzero"`
	EndedAt      time.Time `json:"endedAt,omitzero"`
	DurationMs   int       `json:"durationMs,omitempty"`
	ToolCalls    int       `json:"toolCalls,omitempty"`
	Turns        int       `json:"turns,omitempty"`
	TokensUsed   int       `json:"tokensUsed,omitempty"`
	ToolsUsed    []string  `json:"toolsUsed,omitempty"`
	Transcript   bool      `json:"transcript,omitempty"`
	Background   bool      `json:"background,omitempty"`
}

type wireQueued struct {
	ID       string    `json:"id,omitempty"`
	Text     string    `json:"text,omitempty"`
	QueuedAt time.Time `json:"queuedAt,omitzero"`
	Version  int       `json:"version,omitempty"`
}

// wireCommand is ExpandedCommand with its embedded PluginCommand as a nested
// object of its own, so a key PluginCommand gains can never collide with
// ExpandedCommand's Path or Text.
type wireCommand struct {
	PluginCommand wirePluginCommand `json:"pluginCommand,omitzero"`
	Path          string            `json:"path,omitempty"`
	Text          string            `json:"text,omitempty"`
}

type wirePluginCommand struct {
	Plugin      string `json:"plugin,omitempty"`
	Bare        string `json:"bare,omitempty"`
	Display     string `json:"display,omitempty"`
	Qualified   string `json:"qualified,omitempty"`
	Description string `json:"description,omitempty"`
	Kind        string `json:"kind,omitempty"`
}

type wireForeignTurn struct {
	ID      string `json:"id,omitempty"`
	Text    string `json:"text,omitempty"`
	Running bool   `json:"running,omitempty"`
}

// wireTurn is TurnInfo, field for field in the same order, so it converts by
// a plain pointer cast like wireForeignTurn and wireReplay do.
type wireTurn struct {
	ID         string        `json:"id,omitempty"`
	Phase      string        `json:"phase,omitempty"`
	Text       string        `json:"text,omitempty"`
	Origin     string        `json:"origin,omitempty"`
	StopReason string        `json:"stopReason,omitempty"`
	ErrClass   EventErrClass `json:"errClass,omitempty"`
	Err        string        `json:"err,omitempty"`
	Synthetic  bool          `json:"synthetic,omitempty"`
	Next       string        `json:"next,omitempty"`
	Pending    int           `json:"pending,omitempty"`
}

// wireAsk is an AskUpdate. Its Body is the one part that is not a plain field
// copy: it holds the three opening payloads, each in the shape its own opening
// event already has on the wire, so an ending that carries its body and the
// opening it stands in for read the same.
type wireAsk struct {
	ID       string              `json:"id,omitempty"`
	Kind     AskKind             `json:"kind,omitempty"`
	Outcome  AskOutcome          `json:"outcome,omitempty"`
	By       string              `json:"by,omitempty"`
	OptionID string              `json:"optionId,omitempty"`
	Answers  map[string][]string `json:"answers,omitempty"`
	Skip     bool                `json:"skip,omitempty"`
	Accepted bool                `json:"accepted,omitempty"`
	Label    string              `json:"label,omitempty"`
	Body     *wireAskBody        `json:"body,omitempty"`
}

// wireAskBody is an AskBody: exactly one of the three is set on an ask craze
// made, and each keeps the shape its opening event has.
type wireAskBody struct {
	Permission *wirePermission `json:"permission,omitempty"`
	Question   *wireQuestionEv `json:"question,omitempty"`
	Plan       *wirePlan       `json:"plan,omitempty"`
}

// wireState is a StateDelta. Each section is a pointer, so a section the
// delta did not touch is absent and one it emptied is present and empty:
// omitempty on a pointer omits only nil, which is exactly the distinction the
// type is for.
type wireState struct {
	Title    *string          `json:"title,omitempty"`
	Mode     *string          `json:"mode,omitempty"`
	Model    *string          `json:"model,omitempty"`
	Config   *wireConfig      `json:"config,omitempty"`
	Commands *wireCommands    `json:"commands,omitempty"`
	Plugins  *wirePluginsList `json:"plugins,omitempty"`
	SendNow  *wireSendNow     `json:"sendNow,omitempty"`
	Reason   string           `json:"reason,omitempty"`
	Detail   string           `json:"detail,omitempty"`
}

// The three list sections. Each keeps its slice under a key of its own rather
// than being the section itself, so a section that is present and empty is
// {"options":null} and one that is absent is no key at all — the distinction
// StateDelta's pointers are for.
type wireConfig struct {
	Options []wireConfigOption `json:"options,omitempty"`
}

type wireCommands struct {
	Commands []wireCommandInfo `json:"commands,omitempty"`
}

type wirePluginsList struct {
	Plugins []wirePluginCommand `json:"plugins,omitempty"`
}

// wireConfigOption is a ConfigOption, wireSelectValue a SelectValue and
// wireCommandInfo a CommandInfo, each field for field in the same order.
type wireConfigOption struct {
	ID           string            `json:"id,omitempty"`
	Name         string            `json:"name,omitempty"`
	Category     string            `json:"category,omitempty"`
	Type         string            `json:"type,omitempty"`
	Current      string            `json:"current,omitempty"`
	SelectValues []wireSelectValue `json:"selectValues,omitempty"`
}

type wireSelectValue struct {
	Value string `json:"value,omitempty"`
	Name  string `json:"name,omitempty"`
}

type wireCommandInfo struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// wireSendNow is SendNowState, field for field in the same order, so it
// converts by a plain pointer cast like wireForeignTurn and wireTurn do.
type wireSendNow struct {
	Armed   bool   `json:"armed,omitempty"`
	Text    string `json:"text,omitempty"`
	FromRow string `json:"fromRow,omitempty"`
	Turn    string `json:"turn,omitempty"`
}

type wireReplay struct {
	Phase string `json:"phase,omitempty"`
}

// wireError is an Event.Err: always all three keys, so a jq filter over a
// journal can read .err.class without a default.
type wireError struct {
	Message string        `json:"message"`
	Class   EventErrClass `json:"class"`
	Code    int           `json:"code"`
}

// nilIfEmpty is the collection rule on decode: absent and [] both yield nil.
func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

// convertSlice is s with conv applied to each element, and nil for an empty
// s: the collection rule on decode, and the same wire on encode, where an
// empty slice is omitted either way.
func convertSlice[From, To any](s []From, conv func(From) To) []To {
	if len(s) == 0 {
		return nil
	}
	out := make([]To, len(s))
	for i, v := range s {
		out[i] = conv(v)
	}
	return out
}

func toWireTodo(t Todo) wireTodo { return wireTodo(t) }
func eventTodo(t wireTodo) Todo  { return Todo(t) }

func toWireConfigOption(c ConfigOption) wireConfigOption {
	return wireConfigOption{
		ID: c.ID, Name: c.Name, Category: c.Category, Type: c.Type, Current: c.Current,
		SelectValues: convertSlice(c.SelectValues, func(v SelectValue) wireSelectValue { return wireSelectValue(v) }),
	}
}

func configOptionOf(c wireConfigOption) ConfigOption {
	return ConfigOption{
		ID: c.ID, Name: c.Name, Category: c.Category, Type: c.Type, Current: c.Current,
		SelectValues: convertSlice(c.SelectValues, func(v wireSelectValue) SelectValue { return SelectValue(v) }),
	}
}

func toWireEvent(ev Event) wireEvent {
	w := wireEvent{
		Type:           string(ev.Type),
		Text:           ev.Text,
		Mode:           ev.Mode,
		Agent:          ev.Agent,
		Todos:          convertSlice(ev.Todos, toWireTodo),
		SubagentChange: ev.SubagentChange,
		QueueChange:    string(ev.QueueChange),
		QueuePos:       ev.QueuePos,
		Interjection:   ev.Interjection,
		ForeignTurn:    (*wireForeignTurn)(ev.ForeignTurn),
		Turn:           (*wireTurn)(ev.Turn),
		Replay:         (*wireReplay)(ev.Replay),
		Replayed:       ev.Replayed,
		Cause:          ev.Cause,
		StopReason:     ev.StopReason,
		At:             ev.At.UTC(),
	}
	if ev.Tool != nil {
		w.Tool = toWireTool(ev.Tool)
	}
	w.Permission = toWirePermission(ev.Permission)
	w.Question = toWireQuestionEv(ev.Question)
	w.Plan = toWirePlan(ev.Plan)
	if a := ev.Ask; a != nil {
		w.Ask = &wireAsk{
			ID:       a.ID,
			Kind:     a.Kind,
			Outcome:  a.Outcome,
			By:       a.By,
			OptionID: a.OptionID,
			Answers:  a.Answers,
			Skip:     a.Skip,
			Accepted: a.Accepted,
			Label:    a.Label,
		}
		if b := a.Body; b != nil {
			w.Ask.Body = &wireAskBody{
				Permission: toWirePermission(b.Permission),
				Question:   toWireQuestionEv(b.Question),
				Plan:       toWirePlan(b.Plan),
			}
		}
	}
	if a := ev.Subagent; a != nil {
		w.Subagent = toWireSubagent(a)
	}
	if q := ev.Queue; q != nil {
		w.Queue = &wireQueued{ID: q.ID, Text: q.Text, QueuedAt: q.QueuedAt.UTC(), Version: q.Version}
	}
	if c := ev.Command; c != nil {
		w.Command = &wireCommand{PluginCommand: wirePluginCommand(c.PluginCommand), Path: c.Path, Text: c.Text}
	}
	if s := ev.State; s != nil {
		w.State = &wireState{
			Title: s.Title, Mode: s.Mode, Model: s.Model,
			SendNow: (*wireSendNow)(s.SendNow), Reason: s.Reason, Detail: s.Detail,
		}
		if c := s.Config; c != nil {
			w.State.Config = &wireConfig{Options: convertSlice(c.Options, toWireConfigOption)}
		}
		if c := s.Commands; c != nil {
			w.State.Commands = &wireCommands{Commands: convertSlice(c.Commands, func(c CommandInfo) wireCommandInfo {
				return wireCommandInfo(c)
			})}
		}
		if p := s.Plugins; p != nil {
			w.State.Plugins = &wirePluginsList{Plugins: convertSlice(p.Plugins, func(p PluginCommand) wirePluginCommand {
				return wirePluginCommand(p)
			})}
		}
	}
	if ev.Err != nil {
		class, code := classifyEventErr(ev.Err)
		w.Err = &wireError{Message: ev.Err.Error(), Class: class, Code: code}
	}
	return w
}

// toWirePermission is a PermissionEvent on the wire, nil for nil: the same
// mapping for an opening's payload and for the copy an ask's ending carries
// when no opening was published.
func toWirePermission(p *PermissionEvent) *wirePermission {
	if p == nil {
		return nil
	}
	return &wirePermission{ID: p.ID, Tool: p.Tool,
		Options: convertSlice(p.Options, func(o PermissionOption) wirePermissionOption { return wirePermissionOption(o) })}
}

// toWirePlan is a PlanEvent on the wire, nil for nil.
func toWirePlan(p *PlanEvent) *wirePlan {
	if p == nil {
		return nil
	}
	return &wirePlan{
		ID:       p.ID,
		Name:     p.Name,
		Overview: p.Overview,
		Plan:     p.Plan,
		Todos:    convertSlice(p.Todos, toWireTodo),
		Auto:     p.Auto,
		Accepted: p.Accepted,
	}
}

// toWireTool shares the tool's string slices and its output with the wire
// rather than copying them: encoding only reads them, and the value is the
// publisher's own copy (cloneTool) by the time it gets here.
func toWireTool(t *ToolEvent) *wireTool {
	w := &wireTool{
		ID:          t.ID,
		Name:        t.Name,
		Status:      t.Status,
		Kind:        t.Kind,
		Title:       t.Title,
		ToolName:    t.ToolName,
		RawInput:    t.RawInput,
		ContentText: t.ContentText,
		Locations:   t.Locations,
		Output:      (*wireToolOutput)(t.Output),
		Diffs:       convertSlice(t.Diffs, func(d ToolDiff) wireDiff { return wireDiff(d) }),
		At:          t.At.UTC(),
	}
	if k := t.Task; k != nil {
		w.Task = &wireTask{
			Description:  k.Description,
			Prompt:       k.Prompt,
			Model:        k.Model,
			AgentID:      k.AgentID,
			SubagentType: k.SubagentType,
			DurationMs:   k.DurationMs,
			Receipt:      k.Receipt,
			Status:       string(k.Status),
			Background:   k.Background,
		}
	}
	return w
}

func toWireQuestionEv(q *QuestionEvent) *wireQuestionEv {
	if q == nil {
		return nil
	}
	return &wireQuestionEv{
		ID:    q.ID,
		Title: q.Title,
		Questions: convertSlice(q.Questions, func(qq Question) wireQuestion {
			return wireQuestion{
				ID:            qq.ID,
				Prompt:        qq.Prompt,
				Options:       convertSlice(qq.Options, func(o Option) wireOption { return wireOption(o) }),
				AllowMultiple: qq.AllowMultiple,
			}
		}),
		Auto:    q.Auto,
		Answers: q.Answers,
	}
}

func toWireSubagent(a *SubagentInfo) *wireSubagent {
	return &wireSubagent{
		ID:           a.ID,
		AttemptID:    a.AttemptID,
		ParentID:     a.ParentID,
		ToolCallID:   a.ToolCallID,
		Description:  a.Description,
		SubagentType: a.SubagentType,
		Model:        a.Model,
		Status:       string(a.Status),
		Error:        a.Error,
		Prompt:       a.Prompt,
		Output:       a.Output,
		Activity:     a.Activity,
		StartedAt:    a.StartedAt.UTC(),
		EndedAt:      a.EndedAt.UTC(),
		DurationMs:   a.DurationMs,
		ToolCalls:    a.ToolCalls,
		Turns:        a.Turns,
		TokensUsed:   a.TokensUsed,
		ToolsUsed:    a.ToolsUsed,
		Transcript:   a.Transcript,
		Background:   a.Background,
	}
}

func (w *wireEvent) event() Event {
	ev := Event{
		Type:           EventType(w.Type),
		Text:           w.Text,
		Mode:           w.Mode,
		Agent:          w.Agent,
		Todos:          convertSlice(w.Todos, eventTodo),
		SubagentChange: w.SubagentChange,
		QueueChange:    QueueChange(w.QueueChange),
		QueuePos:       w.QueuePos,
		Interjection:   w.Interjection,
		ForeignTurn:    (*ForeignTurnInfo)(w.ForeignTurn),
		Turn:           (*TurnInfo)(w.Turn),
		Replay:         (*ReplayInfo)(w.Replay),
		Replayed:       w.Replayed,
		Cause:          w.Cause,
		StopReason:     w.StopReason,
		At:             w.At.UTC(),
	}
	if w.Tool != nil {
		ev.Tool = w.Tool.tool()
	}
	ev.Permission = w.Permission.permission()
	ev.Question = w.Question.question()
	ev.Plan = w.Plan.plan()
	if a := w.Ask; a != nil {
		ev.Ask = &AskUpdate{
			ID:       a.ID,
			Kind:     a.Kind,
			Outcome:  a.Outcome,
			By:       a.By,
			OptionID: a.OptionID,
			Skip:     a.Skip,
			Accepted: a.Accepted,
			Label:    a.Label,
		}
		if len(a.Answers) > 0 {
			ev.Ask.Answers = a.Answers
		}
		if b := a.Body; b != nil {
			ev.Ask.Body = &AskBody{
				Permission: b.Permission.permission(),
				Question:   b.Question.question(),
				Plan:       b.Plan.plan(),
			}
		}
	}
	if a := w.Subagent; a != nil {
		ev.Subagent = a.subagent()
	}
	if q := w.Queue; q != nil {
		ev.Queue = &QueuedPrompt{ID: q.ID, Text: q.Text, QueuedAt: q.QueuedAt.UTC(), Version: q.Version}
	}
	if c := w.Command; c != nil {
		ev.Command = &ExpandedCommand{PluginCommand: PluginCommand(c.PluginCommand), Path: c.Path, Text: c.Text}
	}
	if s := w.State; s != nil {
		ev.State = &StateDelta{
			Title: s.Title, Mode: s.Mode, Model: s.Model,
			SendNow: (*SendNowState)(s.SendNow), Reason: s.Reason, Detail: s.Detail,
		}
		if c := s.Config; c != nil {
			ev.State.Config = &ConfigState{Options: convertSlice(c.Options, configOptionOf)}
		}
		if c := s.Commands; c != nil {
			ev.State.Commands = &CommandsState{Commands: convertSlice(c.Commands, func(c wireCommandInfo) CommandInfo {
				return CommandInfo(c)
			})}
		}
		if p := s.Plugins; p != nil {
			ev.State.Plugins = &PluginsState{Plugins: convertSlice(p.Plugins, func(p wirePluginCommand) PluginCommand {
				return PluginCommand(p)
			})}
		}
	}
	if e := w.Err; e != nil {
		ev.Err = &RemoteError{Message: e.Message, Class: e.Class, Code: e.Code}
	}
	return ev
}

func (w *wireTool) tool() *ToolEvent {
	t := &ToolEvent{
		ID:          w.ID,
		Name:        w.Name,
		Status:      w.Status,
		Kind:        w.Kind,
		Title:       w.Title,
		ToolName:    w.ToolName,
		RawInput:    w.RawInput,
		ContentText: w.ContentText,
		Locations:   nilIfEmpty(w.Locations),
		Output:      (*ToolOutput)(w.Output),
		Diffs:       convertSlice(w.Diffs, func(d wireDiff) ToolDiff { return ToolDiff(d) }),
		At:          w.At.UTC(),
	}
	if k := w.Task; k != nil {
		t.Task = &TaskInfo{
			Description:  k.Description,
			Prompt:       k.Prompt,
			Model:        k.Model,
			AgentID:      k.AgentID,
			SubagentType: k.SubagentType,
			DurationMs:   k.DurationMs,
			Receipt:      k.Receipt,
			Status:       SubagentStatus(k.Status),
			Background:   k.Background,
		}
	}
	return t
}

// permission is a wirePermission back as a PermissionEvent, nil for nil.
func (w *wirePermission) permission() *PermissionEvent {
	if w == nil {
		return nil
	}
	return &PermissionEvent{ID: w.ID, Tool: w.Tool,
		Options: convertSlice(w.Options, func(o wirePermissionOption) PermissionOption { return PermissionOption(o) })}
}

// plan is a wirePlan back as a PlanEvent, nil for nil.
func (w *wirePlan) plan() *PlanEvent {
	if w == nil {
		return nil
	}
	return &PlanEvent{
		ID:       w.ID,
		Name:     w.Name,
		Overview: w.Overview,
		Plan:     w.Plan,
		Todos:    convertSlice(w.Todos, eventTodo),
		Auto:     w.Auto,
		Accepted: w.Accepted,
	}
}

// question is the one decode that keeps a nil apart from an empty slice: the
// values of Answers come back as they were written, null as nil and [] as
// empty. The map itself follows the collection rule.
func (w *wireQuestionEv) question() *QuestionEvent {
	if w == nil {
		return nil
	}
	q := &QuestionEvent{
		ID:    w.ID,
		Title: w.Title,
		Questions: convertSlice(w.Questions, func(wq wireQuestion) Question {
			return Question{
				ID:            wq.ID,
				Prompt:        wq.Prompt,
				Options:       convertSlice(wq.Options, func(o wireOption) Option { return Option(o) }),
				AllowMultiple: wq.AllowMultiple,
			}
		}),
		Auto: w.Auto,
	}
	if len(w.Answers) > 0 {
		q.Answers = w.Answers
	}
	return q
}

func (w *wireSubagent) subagent() *SubagentInfo {
	return &SubagentInfo{
		ID:           w.ID,
		AttemptID:    w.AttemptID,
		ParentID:     w.ParentID,
		ToolCallID:   w.ToolCallID,
		Description:  w.Description,
		SubagentType: w.SubagentType,
		Model:        w.Model,
		Status:       SubagentStatus(w.Status),
		Error:        w.Error,
		Prompt:       w.Prompt,
		Output:       w.Output,
		Activity:     w.Activity,
		StartedAt:    w.StartedAt.UTC(),
		EndedAt:      w.EndedAt.UTC(),
		DurationMs:   w.DurationMs,
		ToolCalls:    w.ToolCalls,
		Turns:        w.Turns,
		TokensUsed:   w.TokensUsed,
		ToolsUsed:    nilIfEmpty(w.ToolsUsed),
		Transcript:   w.Transcript,
		Background:   w.Background,
	}
}
