package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	body, _, err := encodeEvent(ev)
	return body, err
}

// encodeEvent is EncodeEvent that also hands back the codec's view of ev.Err:
// the *RemoteError a decoder of the body builds — the message, class and code
// taken by the one call to Error() and the one classification the encoding
// makes (toWireError), with the message as the JSON carries it
// (remoteError). It is nil when ev.Err is nil, and when the encoding stopped
// before it read Err at all (an event with no type); it is set even when the
// encoding failed after reading it, so a caller that must never read an error
// twice — the event log, for its observer — never has to.
func encodeEvent(ev Event) (string, *RemoteError, error) {
	if ev.Type == "" {
		return "", nil, errors.New("agent: encode event: no type")
	}
	e := eventEncoders.Get().(*eventEncoder)
	defer e.release()
	e.w = toWireEvent(ev)
	remote := remoteError(e.w.Err)
	if err := e.enc.Encode(&e.w); err != nil {
		return "", remote, fmt.Errorf("agent: encode %s event: %w", ev.Type, err)
	}
	// The body is the builder's own buffer, which release gives up rather
	// than reuses, so it shares storage with no later encode. Encode ends it
	// with a newline.
	body := e.out.String()
	return body[:len(body)-1], remote, nil
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
	IndexErr string           `json:"indexErr,omitempty"`
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
	w.Queue = toWireQueued(ev.Queue)
	if c := ev.Command; c != nil {
		w.Command = &wireCommand{PluginCommand: wirePluginCommand(c.PluginCommand), Path: c.Path, Text: c.Text}
	}
	if s := ev.State; s != nil {
		w.State = &wireState{
			Title: s.Title, Mode: s.Mode, Model: s.Model,
			SendNow: (*wireSendNow)(s.SendNow), Reason: s.Reason, Detail: s.Detail,
			IndexErr: s.IndexErr,
		}
		w.State.Config = toWireConfig(s.Config)
		w.State.Commands = toWireCommands(s.Commands)
		w.State.Plugins = toWirePlugins(s.Plugins)
	}
	w.Err = toWireError(ev.Err)
	return w
}

// toWireQueued is a QueuedPrompt on the wire, nil for nil: an EventQueue's
// payload and a transcript snapshot's queue row (EncodeQueuedPrompt).
func toWireQueued(q *QueuedPrompt) *wireQueued {
	if q == nil {
		return nil
	}
	return &wireQueued{ID: q.ID, Text: q.Text, QueuedAt: q.QueuedAt.UTC(), Version: q.Version}
}

// The three list sections on the wire, nil for nil: a StateDelta's and a
// transcript snapshot's (EncodeConfigState and its siblings).
func toWireConfig(c *ConfigState) *wireConfig {
	if c == nil {
		return nil
	}
	return &wireConfig{Options: convertSlice(c.Options, toWireConfigOption)}
}

func toWireCommands(c *CommandsState) *wireCommands {
	if c == nil {
		return nil
	}
	return &wireCommands{Commands: convertSlice(c.Commands, func(c CommandInfo) wireCommandInfo {
		return wireCommandInfo(c)
	})}
}

func toWirePlugins(p *PluginsState) *wirePluginsList {
	if p == nil {
		return nil
	}
	return &wirePluginsList{Plugins: convertSlice(p.Plugins, func(p PluginCommand) wirePluginCommand {
		return wirePluginCommand(p)
	})}
}

// toWireError is an Event.Err on the wire, nil for nil: its class and code
// (classifyEventErr, which walks the chain — Unwrap, Is, As) and its message
// (Error). This is the one place the codec runs an error's own methods, once
// per encoding.
func toWireError(err error) *wireError {
	if err == nil {
		return nil
	}
	class, code := classifyEventErr(err)
	return &wireError{Message: err.Error(), Class: class, Code: code}
}

// remoteError is the *RemoteError that decoding w yields, built without a
// round trip, nil for nil. The message is taken as the JSON carries it: the
// encoder writes each byte that is not part of valid UTF-8 as U+FFFD
// (encoding/json's rule, per byte), so the decoded message differs from
// Error()'s whenever Error() was not valid UTF-8 — a path in an os error,
// say; craze's own messages and a provider's (oneLine) always are. Making the
// same replacement here keeps this value byte for byte what every decoder of
// the body holds (TestEncodeEventHandsBackWhatTheBodyDecodesTo). The check is
// one pass over the message, and the copy only happens for an invalid one.
func remoteError(w *wireError) *RemoteError {
	if w == nil {
		return nil
	}
	return &RemoteError{Message: asJSONCarriesIt(w.Message), Class: w.Class, Code: w.Code}
}

// asJSONCarriesIt is s as encoding/json writes it and reads it back: s itself
// when it is valid UTF-8, else each invalid byte replaced by U+FFFD.
func asJSONCarriesIt(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 2*utf8.UTFMax)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString("\uFFFD")
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
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
	ev.Queue = w.Queue.queued()
	if c := w.Command; c != nil {
		ev.Command = &ExpandedCommand{PluginCommand: PluginCommand(c.PluginCommand), Path: c.Path, Text: c.Text}
	}
	if s := w.State; s != nil {
		ev.State = &StateDelta{
			Title: s.Title, Mode: s.Mode, Model: s.Model,
			SendNow: (*SendNowState)(s.SendNow), Reason: s.Reason, Detail: s.Detail,
			IndexErr: s.IndexErr,
		}
		ev.State.Config = s.Config.config()
		ev.State.Commands = s.Commands.commands()
		ev.State.Plugins = s.Plugins.plugins()
	}
	if e := w.Err; e != nil {
		ev.Err = &RemoteError{Message: e.Message, Class: e.Class, Code: e.Code}
	}
	return ev
}

// queued is a wireQueued back as a QueuedPrompt, nil for nil.
func (w *wireQueued) queued() *QueuedPrompt {
	if w == nil {
		return nil
	}
	return &QueuedPrompt{ID: w.ID, Text: w.Text, QueuedAt: w.QueuedAt.UTC(), Version: w.Version}
}

// config, commands and plugins are the list sections back, nil for nil.
func (w *wireConfig) config() *ConfigState {
	if w == nil {
		return nil
	}
	return &ConfigState{Options: convertSlice(w.Options, configOptionOf)}
}

func (w *wireCommands) commands() *CommandsState {
	if w == nil {
		return nil
	}
	return &CommandsState{Commands: convertSlice(w.Commands, func(c wireCommandInfo) CommandInfo {
		return CommandInfo(c)
	})}
}

func (w *wirePluginsList) plugins() *PluginsState {
	if w == nil {
		return nil
	}
	return &PluginsState{Plugins: convertSlice(w.Plugins, func(p wirePluginCommand) PluginCommand {
		return PluginCommand(p)
	})}
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

// ------------------------------------------------------------- leaf wrappers
//
// The leaf types another codec carries — the transcript snapshot's (plan 024
// §3.5) — each go through the wire twin EncodeEvent uses for it, by the same
// conversion, so a field added to an agent type reaches every codec by the one
// line that adds it to its twin and the shapes cannot drift:
// EncodeToolEvent(t) is byte for byte the "tool" value of
// EncodeEvent(Event{Type: EventTool, Tool: t}), and likewise for each
// (TestLeafWrappersWriteWhatTheEventCarries). The rules are the event codec's:
// times in UTC, RFC 3339 with nanoseconds; nil and empty collections decode
// nil; a pointer's presence is kept; HTML is not escaped. A nil pointer
// encodes to nil — no value, which a caller omits — and no value or null
// decodes to nil. An encoding fails only where EncodeEvent's would: a time
// JSON cannot write.

// encodeLeaf is v as one compact JSON value, HTML unescaped, as the event
// encoder writes it.
func encodeLeaf(what string, v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("agent: encode %s: %w", what, err)
	}
	// Encode ends the value with a newline.
	return json.RawMessage(b.Bytes()[:b.Len()-1]), nil
}

// decodeLeaf is raw as the wire twin W, nil for no value or null.
func decodeLeaf[W any](what string, raw json.RawMessage) (*W, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	w := new(W)
	if err := json.Unmarshal(raw, w); err != nil {
		return nil, fmt.Errorf("agent: decode %s: %w", what, err)
	}
	return w, nil
}

// EncodeToolEvent is t as the event codec writes an EventTool's payload.
func EncodeToolEvent(t *ToolEvent) (json.RawMessage, error) {
	if t == nil {
		return nil, nil
	}
	return encodeLeaf("tool", toWireTool(t))
}

// DecodeToolEvent is EncodeToolEvent's inverse.
func DecodeToolEvent(raw json.RawMessage) (*ToolEvent, error) {
	w, err := decodeLeaf[wireTool]("tool", raw)
	if w == nil {
		return nil, err
	}
	return w.tool(), nil
}

// EncodePlanEvent is p as the event codec writes an EventPlan's payload.
func EncodePlanEvent(p *PlanEvent) (json.RawMessage, error) {
	if p == nil {
		return nil, nil
	}
	return encodeLeaf("plan", toWirePlan(p))
}

// DecodePlanEvent is EncodePlanEvent's inverse.
func DecodePlanEvent(raw json.RawMessage) (*PlanEvent, error) {
	w, err := decodeLeaf[wirePlan]("plan", raw)
	if w == nil {
		return nil, err
	}
	return w.plan(), nil
}

// EncodeTodo is t as the event codec writes one of an EventTodos's items.
func EncodeTodo(t Todo) (json.RawMessage, error) {
	return encodeLeaf("todo", toWireTodo(t))
}

// DecodeTodo is EncodeTodo's inverse; no value decodes as the zero Todo.
func DecodeTodo(raw json.RawMessage) (Todo, error) {
	w, err := decodeLeaf[wireTodo]("todo", raw)
	if w == nil {
		return Todo{}, err
	}
	return eventTodo(*w), nil
}

// EncodeSubagentInfo is a as the event codec writes an EventSubagent's
// payload.
func EncodeSubagentInfo(a *SubagentInfo) (json.RawMessage, error) {
	if a == nil {
		return nil, nil
	}
	return encodeLeaf("subagent", toWireSubagent(a))
}

// DecodeSubagentInfo is EncodeSubagentInfo's inverse.
func DecodeSubagentInfo(raw json.RawMessage) (*SubagentInfo, error) {
	w, err := decodeLeaf[wireSubagent]("subagent", raw)
	if w == nil {
		return nil, err
	}
	return w.subagent(), nil
}

// EncodePermissionEvent is p as the event codec writes an EventPermission's
// payload (and an ask body's permission).
func EncodePermissionEvent(p *PermissionEvent) (json.RawMessage, error) {
	if p == nil {
		return nil, nil
	}
	return encodeLeaf("permission", toWirePermission(p))
}

// DecodePermissionEvent is EncodePermissionEvent's inverse.
func DecodePermissionEvent(raw json.RawMessage) (*PermissionEvent, error) {
	w, err := decodeLeaf[wirePermission]("permission", raw)
	if w == nil {
		return nil, err
	}
	return w.permission(), nil
}

// EncodeQuestionEvent is q as the event codec writes an EventQuestion's
// payload: an answer list keeps null apart from [].
func EncodeQuestionEvent(q *QuestionEvent) (json.RawMessage, error) {
	if q == nil {
		return nil, nil
	}
	return encodeLeaf("question", toWireQuestionEv(q))
}

// DecodeQuestionEvent is EncodeQuestionEvent's inverse.
func DecodeQuestionEvent(raw json.RawMessage) (*QuestionEvent, error) {
	w, err := decodeLeaf[wireQuestionEv]("question", raw)
	if w == nil {
		return nil, err
	}
	return w.question(), nil
}

// EncodeQueuedPrompt is q as the event codec writes an EventQueue's payload.
func EncodeQueuedPrompt(q QueuedPrompt) (json.RawMessage, error) {
	return encodeLeaf("queued prompt", toWireQueued(&q))
}

// DecodeQueuedPrompt is EncodeQueuedPrompt's inverse; no value decodes as the
// zero QueuedPrompt.
func DecodeQueuedPrompt(raw json.RawMessage) (QueuedPrompt, error) {
	w, err := decodeLeaf[wireQueued]("queued prompt", raw)
	if w == nil {
		return QueuedPrompt{}, err
	}
	return *w.queued(), nil
}

// EncodeConfigState is c as the event codec writes a StateDelta's Config
// section.
func EncodeConfigState(c *ConfigState) (json.RawMessage, error) {
	if c == nil {
		return nil, nil
	}
	return encodeLeaf("config", toWireConfig(c))
}

// DecodeConfigState is EncodeConfigState's inverse.
func DecodeConfigState(raw json.RawMessage) (*ConfigState, error) {
	w, err := decodeLeaf[wireConfig]("config", raw)
	if w == nil {
		return nil, err
	}
	return w.config(), nil
}

// EncodeCommandsState is c as the event codec writes a StateDelta's Commands
// section.
func EncodeCommandsState(c *CommandsState) (json.RawMessage, error) {
	if c == nil {
		return nil, nil
	}
	return encodeLeaf("commands", toWireCommands(c))
}

// DecodeCommandsState is EncodeCommandsState's inverse.
func DecodeCommandsState(raw json.RawMessage) (*CommandsState, error) {
	w, err := decodeLeaf[wireCommands]("commands", raw)
	if w == nil {
		return nil, err
	}
	return w.commands(), nil
}

// EncodePluginsState is p as the event codec writes a StateDelta's Plugins
// section.
func EncodePluginsState(p *PluginsState) (json.RawMessage, error) {
	if p == nil {
		return nil, nil
	}
	return encodeLeaf("plugins", toWirePlugins(p))
}

// DecodePluginsState is EncodePluginsState's inverse.
func DecodePluginsState(raw json.RawMessage) (*PluginsState, error) {
	w, err := decodeLeaf[wirePluginsList]("plugins", raw)
	if w == nil {
		return nil, err
	}
	return w.plugins(), nil
}

// EncodeSendNowState is s as the event codec writes a StateDelta's SendNow
// section.
func EncodeSendNowState(s *SendNowState) (json.RawMessage, error) {
	if s == nil {
		return nil, nil
	}
	return encodeLeaf("send-now", (*wireSendNow)(s))
}

// DecodeSendNowState is EncodeSendNowState's inverse.
func DecodeSendNowState(raw json.RawMessage) (*SendNowState, error) {
	w, err := decodeLeaf[wireSendNow]("send-now", raw)
	return (*SendNowState)(w), err
}

// EncodeForeignTurnInfo is f as the event codec writes an EventForeignTurn's
// payload.
func EncodeForeignTurnInfo(f *ForeignTurnInfo) (json.RawMessage, error) {
	if f == nil {
		return nil, nil
	}
	return encodeLeaf("foreign turn", (*wireForeignTurn)(f))
}

// DecodeForeignTurnInfo is EncodeForeignTurnInfo's inverse.
func DecodeForeignTurnInfo(raw json.RawMessage) (*ForeignTurnInfo, error) {
	w, err := decodeLeaf[wireForeignTurn]("foreign turn", raw)
	return (*ForeignTurnInfo)(w), err
}

// RemoteErrorOf is what the event codec keeps of err: the *RemoteError a
// decoder of an event carrying err holds — its message as the JSON carries it,
// its class and its code — and nil for nil. It runs err's own methods (Error,
// and the chain walk that classifies it) once, as the event encoder does, so a
// caller that may not run an error's code under a lock calls it outside that
// lock, as the transcript snapshot's encoder does for an error entry (plan 024
// §3.2). A *RemoteError keeps its own class and code.
func RemoteErrorOf(err error) *RemoteError {
	return remoteError(toWireError(err))
}
