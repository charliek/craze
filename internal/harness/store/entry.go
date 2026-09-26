package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"charm.land/fantasy"
)

// FormatVersion is the one header version this package writes and reads. A
// file with another version is refused rather than half-understood.
const FormatVersion = 1

// The line types. Every line after the header carries one of these, or a
// type from a newer craze that this version keeps in the tree but otherwise
// ignores (see Entry).
const (
	typeSession      = "session"
	TypeMessage      = "message"
	TypeModelChange  = "model_change"
	TypeEffortChange = "effort_change"
	TypeModeChange   = "mode_change"
	// TypeResume opens each later incarnation of a reopened session (Open):
	// the contract it runs under, as information (plan 028 §3.2).
	TypeResume = "resume"
)

// knownType reports whether this craze writes and reads entries of type typ.
// Any other type is a newer craze's: kept in the tree, never in the context,
// and never trimmed away by Open (ErrNewerTranscript).
func knownType(typ string) bool {
	switch typ {
	case TypeMessage, TypeModelChange, TypeEffortChange, TypeModeChange, TypeResume:
		return true
	}
	return false
}

// errInvalid marks a line that decodes — whole JSON, a good envelope, a
// payload of the right shape for its type — but breaks a rule on the fields
// H7 added (checkFields). No crash writes one: a torn append leaves a prefix
// of a line, never a whole line, so, like a break of the pairing invariant, it
// is ErrCorrupt wherever it is, the last line included, and never skipped as a
// torn tail (plan 028 P14).
var errInvalid = errors.New("invalid entry")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInvalid, fmt.Sprintf(format, args...))
}

// timeLayout is every timestamp's format: UTC with milliseconds, as pi (and
// JavaScript's toISOString) write it. Fixed-width, so lines sort and diff
// cleanly.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// Model names the model a message was sent to or a model_change switched to:
// the model table's values as they were when the entry was written, so a
// transcript stays interpretable after an alias is deleted or re-pointed.
// Provider and WireModel are what the reasoning filter compares (Context).
type Model struct {
	Provider  string // the model table's provider id
	Alias     string // the model table's alias; the JSON "model"
	WireModel string // the model id sent on the wire
}

func (m Model) validate(what string) error {
	if m.Provider == "" || m.Alias == "" || m.WireModel == "" {
		return fmt.Errorf("store: %s needs a provider, model and wire model, got %+v", what, m)
	}
	return nil
}

// Usage is one assistant step's token counts. It is craze's own shape rather
// than fantasy.Usage so the file does not change when Fantasy's does; it
// keeps the cache counters, so prompt-cache hits are observable from the
// first transcript (plan 018 §3.6).
type Usage struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	Reasoning     int64 `json:"reasoning"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

// UsageOf converts Fantasy's usage to the stored shape. The total is dropped:
// it is derivable, and providers disagree on whether it includes cache reads.
func UsageOf(u fantasy.Usage) *Usage {
	return &Usage{
		Input:         u.InputTokens,
		Output:        u.OutputTokens,
		Reasoning:     u.ReasoningTokens,
		CacheRead:     u.CacheReadTokens,
		CacheCreation: u.CacheCreationTokens,
	}
}

// ModelUsage is what sub-agents spent on one model: the model table's provider
// id and alias and the provider's wire id, as a message entry names its own
// model, and the usage summed over every child of the entry's step that ran on
// it (plan 026 §3.7). A child can run on another model than its parent, whose
// model stamps the entry, so its usage is kept apart, per model, rather than
// folded into a plain usage the parent's rate would price.
type ModelUsage struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	WireModel string `json:"wire_model"`
	Usage     Usage  `json:"usage"`
}

// Todo is one item of a session's todo list as a tool entry records it (plan
// 028 §3.2): the harness's own item in the store's shape, so the file does not
// change when the harness's type does, and the store needs nothing of the
// harness to read it. Status is one of todoStatuses.
type Todo struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

// todoStatuses are the four statuses todo_write's schema promises (the
// harness's tool.TodoStatus; a test keeps the two in step). A list holding any
// other is not one this craze wrote.
var todoStatuses = map[string]bool{"pending": true, "in_progress": true, "completed": true, "cancelled": true}

// MessageEntry is what a caller hands AppendUser or AppendStep: the message
// in Fantasy's own shape and what it was sent to. Usage and StopReason belong
// to assistant messages. Interrupted belongs to an assistant message, and to
// the tool message the runner's cancel defence writes with one (results it
// made up for calls a cancel cut off). Effort is optional on every role.
//
// SubagentUsage is the usage of the sub-agents whose results the entry holds,
// one row per model (plan 026 §3.7): on the tool message of a step whose agent
// calls ran children. A tool message never carries a plain Usage. A session's
// cost is the usage on its own assistant entries, each priced by that entry's
// model, plus these rows on every entry that owns them, whatever its role,
// each priced by its row's model; a child's own transcript is a record, never
// added into its parent's cost, so nothing is counted twice.
//
// SubagentResults marks a user entry that holds the results of background
// sub-agents (plan 026 §3.11) rather than what a person typed: a child wrote
// its text, so a replay redacts it with the turn's redactor as it redacts a
// tool result (the harness's history), where a person's prompt is replayed as
// it was written. Such an entry carries its results' SubagentUsage too.
//
// Turn and Todos are H7's (plan 028 §3.2), both additive and omitted when
// unset, so an older craze ignores them and an entry without them is written
// byte for byte as before. Turn is the number of the turn a user entry opens:
// set on the entry a Run or a Wake opens turn N with (AppendUser), never on a
// steer or on mid-turn results (AppendStep refuses one there), and 0 on every
// other entry and on every entry written before H7. Todos is the session's
// todo list after the step, on the tool entry of a step whose calls changed
// it: nil when they did not, and a pointer to an empty list when they emptied
// it, which is how a resume tells "cleared" from "never set".
type MessageEntry struct {
	Message         fantasy.Message
	Model           Model
	Effort          string
	Usage           *Usage
	StopReason      string
	Interrupted     bool // a partial step, cut short by a cancel or an error
	SubagentUsage   []ModelUsage
	SubagentResults bool
	Turn            int
	Todos           *[]Todo
}

// checkFields checks H7's fields against the message's role (plan 028 §3.2):
// a turn only on a user message and never negative; todos only on a tool
// message, each item with a non-empty id no other item has and one of the four
// statuses. AppendUser and AppendStep run it on what they are handed, and
// decodeEntry on every line it reads (errInvalid).
func checkFields(e MessageEntry) error {
	switch {
	case e.Turn < 0:
		return fmt.Errorf("turn %d is negative", e.Turn)
	case e.Turn != 0 && e.Message.Role != fantasy.MessageRoleUser:
		return fmt.Errorf("a %s message carries turn %d; only a user message opens a turn", e.Message.Role, e.Turn)
	case e.Todos != nil && e.Message.Role != fantasy.MessageRoleTool:
		return fmt.Errorf("a %s message carries todos; only a tool message does", e.Message.Role)
	case e.Todos == nil:
		return nil
	}
	seen := make(map[string]bool, len(*e.Todos))
	for i, it := range *e.Todos {
		switch {
		case it.ID == "":
			return fmt.Errorf("todo %d has no id", i)
		case seen[it.ID]:
			return fmt.Errorf("todo %d repeats id %q", i, it.ID)
		case !todoStatuses[it.Status]:
			return fmt.Errorf("todo %q has status %q", it.ID, it.Status)
		}
		seen[it.ID] = true
	}
	return nil
}

// Contract is what one incarnation of a session ran under: the header's
// fields for the session's first, and a resume entry's for each later one
// (Open). It is information: nothing refuses a session whose contract
// changed, and the harness decides what a change means (plan 028 §3.2, §3.3).
type Contract struct {
	CrazeVersion       string
	SystemPromptSHA256 string // hex
	ToolProfile        string // "" for none
	ToolsSHA256        string // hex; "" for none
}

// Entry is one line after the header, as written or read back. Type selects
// which of the fields mean anything: the embedded MessageEntry for a message,
// its Model for a model_change, its Effort for an effort_change, Mode for a
// mode_change, Contract for a resume, none for a type from a newer craze,
// which is kept only so the parent chain through it stays whole.
type Entry struct {
	Type      string
	ID        string // 8 hex chars, unique within the file
	ParentID  string // "" for a root entry (JSON null)
	Timestamp time.Time
	// Mode is the mode a mode_change switched to (plan 023 §3.1). It is on
	// the Entry rather than on MessageEntry because no message carries one:
	// the mode is a fact about the turn's rules, not about what was sent.
	Mode string
	// Contract is what a resume entry records: the contract of the
	// incarnation that reopened the session (Open).
	Contract Contract
	MessageEntry
}

// envelope is the part of every non-header line the tree needs.
type envelope struct {
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	ParentID  *string `json:"parentId"`
	Timestamp string  `json:"timestamp"`
}

// messageLine is a message entry on disk. The message is Fantasy's official
// JSON (its parts' own MarshalJSON, content_json.go), which keeps every
// part's provider metadata; craze's envelope carries the rest. Field order
// is the struct's, so the bytes are deterministic.
type messageLine struct {
	envelope
	Message     fantasy.Message `json:"message"`
	Provider    string          `json:"provider"`
	Model       string          `json:"model"`
	WireModel   string          `json:"wire_model"`
	Effort      string          `json:"effort,omitempty"`
	Usage       *Usage          `json:"usage,omitempty"`
	StopReason  string          `json:"stopReason,omitempty"`
	Interrupted bool            `json:"interrupted,omitempty"`
	// SubagentUsage is additive and omitted when empty, so every entry
	// without it is written byte for byte as before and an older craze
	// reading one with it ignores the key (plan 026 §3.7).
	SubagentUsage []ModelUsage `json:"subagent_usage,omitempty"`
	// SubagentResults is additive in the same way (plan 026 §3.11): only an
	// entry of background results carries it, and an older craze reading one
	// ignores it, replaying the entry as a plain user message.
	SubagentResults bool `json:"subagent_results,omitempty"`
	// Turn and Todos are additive in the same way (plan 028 §3.2). They are
	// kept raw here so that decodeEntry checks their shape itself: a whole
	// line whose turn or todos is the wrong JSON type is errInvalid, like one
	// whose todo has no id, not a decoding failure that the last line would
	// be forgiven as a torn tail (P14).
	Turn  json.RawMessage `json:"turn,omitempty"`
	Todos json.RawMessage `json:"todos,omitempty"`
}

// encodeTurn is a turn as the line carries it: nothing for 0.
func encodeTurn(n int) json.RawMessage {
	if n == 0 {
		return nil
	}
	return strconv.AppendInt(nil, int64(n), 10)
}

// encodeTodos is a list as the line carries it: nothing for nil, and "[]" for
// an empty list, never "null".
func encodeTodos(todos *[]Todo) (json.RawMessage, error) {
	if todos == nil {
		return nil, nil
	}
	items := *todos
	if items == nil {
		items = []Todo{}
	}
	return json.Marshal(items)
}

// decodeTurn and decodeTodos read the raw fields back: absent is the zero
// value, and anything but an integer, or an array of objects, is errInvalid.
func decodeTurn(raw json.RawMessage) (int, error) {
	if raw == nil {
		return 0, nil
	}
	var n int
	if string(raw) == "null" || json.Unmarshal(raw, &n) != nil {
		return 0, invalid("turn %s is not an integer", raw)
	}
	return n, nil
}

func decodeTodos(raw json.RawMessage) (*[]Todo, error) {
	if raw == nil {
		return nil, nil
	}
	if string(raw) == "null" {
		return nil, invalid("todos is null, not a list")
	}
	var items []Todo
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, invalid("todos: %v", err)
	}
	if items == nil {
		items = []Todo{}
	}
	return &items, nil
}

// resumeLine is a resume entry: the contract's fields, named and ordered as
// the header's, and the optional two omitted when empty as the header's are.
type resumeLine struct {
	envelope
	CrazeVersion       string `json:"craze_version"`
	SystemPromptSHA256 string `json:"system_prompt_sha256"`
	ToolProfile        string `json:"tool_profile,omitempty"`
	ToolsSHA256        string `json:"tools_sha256,omitempty"`
}

type modelChangeLine struct {
	envelope
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	WireModel string `json:"wire_model"`
}

// effortChangeLine always carries effort: "" is a real value (a switch to a
// model with no effort control), not an absent one.
type effortChangeLine struct {
	envelope
	Effort string `json:"effort"`
}

// modeChangeLine is a switch to another mode (agent, plan, ask). The header
// carries no mode, so a session that opened in one records the switch to it
// like any other (plan 023 §3.1).
type modeChangeLine struct {
	envelope
	Mode string `json:"mode"`
}

// Header is the file's first line. The system prompt is never stored, only
// its hash, so H7 can tell whether a resumed session would see the prompt it
// was written under. The tool profile and the tools array's hash do the same
// for the tool contract; both are optional, and absent for a session with no
// tools, so a header without them reads as it always has.
//
// A sub-agent's header also links it to its parent: the parent's session id,
// the harness id of the agent call that started it, the agent type it ran as
// and the persona file that defined it (plan 026 §3.2). All four are
// optional, absent for every other session, and a header written before them
// reads as it always has.
type Header struct {
	Version            int
	ID                 string // the session id: a UUID, or the one a sub-agent's runner minted
	Timestamp          time.Time
	Cwd                string
	CrazeVersion       string
	SystemPromptSHA256 string // hex
	ToolProfile        string // "" for none
	ToolsSHA256        string // hex; "" for none
	ParentSession      string // "" for a session no other started
	ParentToolCall     string // the parent's harness call id; "" for none
	SubagentType       string // "" for none
	PersonaPath        string // "" for none, and for a built-in agent type
}

type headerLine struct {
	Type               string `json:"type"`
	Version            int    `json:"version"`
	ID                 string `json:"id"`
	Timestamp          string `json:"timestamp"`
	Cwd                string `json:"cwd"`
	CrazeVersion       string `json:"craze_version"`
	SystemPromptSHA256 string `json:"system_prompt_sha256"`
	ToolProfile        string `json:"tool_profile,omitempty"`
	ToolsSHA256        string `json:"tools_sha256,omitempty"`
	ParentSession      string `json:"parent_session,omitempty"`
	ParentToolCall     string `json:"parent_tool_call,omitempty"`
	SubagentType       string `json:"subagent_type,omitempty"`
	PersonaPath        string `json:"persona_path,omitempty"`
}

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func encodeHeader(h Header) ([]byte, error) {
	return json.Marshal(headerLine{
		Type:               typeSession,
		Version:            h.Version,
		ID:                 h.ID,
		Timestamp:          formatTime(h.Timestamp),
		Cwd:                h.Cwd,
		CrazeVersion:       h.CrazeVersion,
		SystemPromptSHA256: h.SystemPromptSHA256,
		ToolProfile:        h.ToolProfile,
		ToolsSHA256:        h.ToolsSHA256,
		ParentSession:      h.ParentSession,
		ParentToolCall:     h.ParentToolCall,
		SubagentType:       h.SubagentType,
		PersonaPath:        h.PersonaPath,
	})
}

func decodeHeader(line []byte) (Header, error) {
	var hl headerLine
	if err := json.Unmarshal(line, &hl); err != nil {
		return Header{}, err
	}
	if hl.Type != typeSession {
		return Header{}, fmt.Errorf("type is %q, want %q", hl.Type, typeSession)
	}
	if hl.Version != FormatVersion {
		return Header{}, fmt.Errorf("version %d, this craze reads %d", hl.Version, FormatVersion)
	}
	if hl.ID == "" {
		return Header{}, errors.New("no session id")
	}
	ts, err := time.Parse(time.RFC3339Nano, hl.Timestamp)
	if err != nil {
		return Header{}, fmt.Errorf("timestamp: %w", err)
	}
	return Header{
		Version:            hl.Version,
		ID:                 hl.ID,
		Timestamp:          ts,
		Cwd:                hl.Cwd,
		CrazeVersion:       hl.CrazeVersion,
		SystemPromptSHA256: hl.SystemPromptSHA256,
		ToolProfile:        hl.ToolProfile,
		ToolsSHA256:        hl.ToolsSHA256,
		ParentSession:      hl.ParentSession,
		ParentToolCall:     hl.ParentToolCall,
		SubagentType:       hl.SubagentType,
		PersonaPath:        hl.PersonaPath,
	}, nil
}

// encodeEntry is an entry's line, without its newline.
func encodeEntry(e Entry) ([]byte, error) {
	env := envelope{Type: e.Type, ID: e.ID, Timestamp: formatTime(e.Timestamp)}
	if e.ParentID != "" {
		p := e.ParentID
		env.ParentID = &p
	}
	switch e.Type {
	case TypeMessage:
		todos, err := encodeTodos(e.Todos)
		if err != nil {
			return nil, err
		}
		return json.Marshal(messageLine{
			envelope:        env,
			Message:         e.Message,
			Provider:        e.Model.Provider,
			Model:           e.Model.Alias,
			WireModel:       e.Model.WireModel,
			Effort:          e.Effort,
			Usage:           e.Usage,
			StopReason:      e.StopReason,
			Interrupted:     e.Interrupted,
			SubagentUsage:   e.SubagentUsage,
			SubagentResults: e.SubagentResults,
			Turn:            encodeTurn(e.Turn),
			Todos:           todos,
		})
	case TypeModelChange:
		return json.Marshal(modelChangeLine{
			envelope:  env,
			Provider:  e.Model.Provider,
			Model:     e.Model.Alias,
			WireModel: e.Model.WireModel,
		})
	case TypeEffortChange:
		return json.Marshal(effortChangeLine{envelope: env, Effort: e.Effort})
	case TypeModeChange:
		return json.Marshal(modeChangeLine{envelope: env, Mode: e.Mode})
	case TypeResume:
		return json.Marshal(resumeLine{
			envelope:           env,
			CrazeVersion:       e.Contract.CrazeVersion,
			SystemPromptSHA256: e.Contract.SystemPromptSHA256,
			ToolProfile:        e.Contract.ToolProfile,
			ToolsSHA256:        e.Contract.ToolsSHA256,
		})
	default:
		return nil, fmt.Errorf("store: cannot write entry type %q", e.Type)
	}
}

// decodeEntry parses one non-header line. It checks the line on its own; the
// tree checks id uniqueness and the parent (Transcript.check). An error that
// wraps errInvalid is a whole line that breaks checkFields; any other is a
// line that does not decode.
func decodeEntry(line []byte) (Entry, error) {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return Entry{}, err
	}
	if env.Type == "" || env.Type == typeSession {
		return Entry{}, fmt.Errorf("entry type %q", env.Type)
	}
	if env.ID == "" {
		return Entry{}, errors.New("entry has no id")
	}
	ts, err := time.Parse(time.RFC3339Nano, env.Timestamp)
	if err != nil {
		return Entry{}, fmt.Errorf("timestamp: %w", err)
	}
	e := Entry{Type: env.Type, ID: env.ID, Timestamp: ts}
	if env.ParentID != nil {
		if *env.ParentID == "" {
			return Entry{}, errors.New(`parentId is "", want an id or null`)
		}
		e.ParentID = *env.ParentID
	}
	switch env.Type {
	case TypeMessage:
		var ml messageLine
		if err := json.Unmarshal(line, &ml); err != nil {
			return Entry{}, err
		}
		if ml.Message.Role == "" {
			return Entry{}, errors.New("message has no role")
		}
		turn, err := decodeTurn(ml.Turn)
		if err != nil {
			return Entry{}, err
		}
		todos, err := decodeTodos(ml.Todos)
		if err != nil {
			return Entry{}, err
		}
		e.MessageEntry = MessageEntry{
			Message:         ml.Message,
			Model:           Model{Provider: ml.Provider, Alias: ml.Model, WireModel: ml.WireModel},
			Effort:          ml.Effort,
			Usage:           ml.Usage,
			StopReason:      ml.StopReason,
			Interrupted:     ml.Interrupted,
			SubagentUsage:   ml.SubagentUsage,
			SubagentResults: ml.SubagentResults,
			Turn:            turn,
			Todos:           todos,
		}
		if err := checkFields(e.MessageEntry); err != nil {
			return Entry{}, fmt.Errorf("%w: %v", errInvalid, err)
		}
	case TypeModelChange:
		var mc modelChangeLine
		if err := json.Unmarshal(line, &mc); err != nil {
			return Entry{}, err
		}
		e.Model = Model{Provider: mc.Provider, Alias: mc.Model, WireModel: mc.WireModel}
	case TypeEffortChange:
		var ec effortChangeLine
		if err := json.Unmarshal(line, &ec); err != nil {
			return Entry{}, err
		}
		e.Effort = ec.Effort
	case TypeModeChange:
		var mc modeChangeLine
		if err := json.Unmarshal(line, &mc); err != nil {
			return Entry{}, err
		}
		e.Mode = mc.Mode
	case TypeResume:
		var rl resumeLine
		if err := json.Unmarshal(line, &rl); err != nil {
			return Entry{}, err
		}
		e.Contract = Contract{
			CrazeVersion:       rl.CrazeVersion,
			SystemPromptSHA256: rl.SystemPromptSHA256,
			ToolProfile:        rl.ToolProfile,
			ToolsSHA256:        rl.ToolsSHA256,
		}
	}
	return e, nil
}
