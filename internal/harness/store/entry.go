package store

import (
	"encoding/json"
	"errors"
	"fmt"
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
)

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

// MessageEntry is what a caller hands AppendUser or AppendAssistant: the
// message in Fantasy's own shape and what it was sent to. Usage, StopReason
// and Interrupted belong to assistant messages; Effort is optional on both.
type MessageEntry struct {
	Message     fantasy.Message
	Model       Model
	Effort      string
	Usage       *Usage
	StopReason  string
	Interrupted bool // a partial answer, cut short by a cancel or an error
}

// Entry is one line after the header, as written or read back. Type selects
// which of the embedded MessageEntry's fields mean anything: all of them for
// a message, Model for a model_change, Effort for an effort_change, none for
// a type from a newer craze, which is kept only so the parent chain through
// it stays whole.
type Entry struct {
	Type      string
	ID        string // 8 hex chars, unique within the file
	ParentID  string // "" for a root entry (JSON null)
	Timestamp time.Time
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

// Header is the file's first line. The system prompt is never stored, only
// its hash, so H7 can tell whether a resumed session would see the prompt it
// was written under.
type Header struct {
	Version            int
	ID                 string // the session id, a UUID
	Timestamp          time.Time
	Cwd                string
	CrazeVersion       string
	SystemPromptSHA256 string // hex
}

type headerLine struct {
	Type               string `json:"type"`
	Version            int    `json:"version"`
	ID                 string `json:"id"`
	Timestamp          string `json:"timestamp"`
	Cwd                string `json:"cwd"`
	CrazeVersion       string `json:"craze_version"`
	SystemPromptSHA256 string `json:"system_prompt_sha256"`
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
		return json.Marshal(messageLine{
			envelope:    env,
			Message:     e.Message,
			Provider:    e.Model.Provider,
			Model:       e.Model.Alias,
			WireModel:   e.Model.WireModel,
			Effort:      e.Effort,
			Usage:       e.Usage,
			StopReason:  e.StopReason,
			Interrupted: e.Interrupted,
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
	default:
		return nil, fmt.Errorf("store: cannot write entry type %q", e.Type)
	}
}

// decodeEntry parses one non-header line. It checks the line on its own; the
// tree checks id uniqueness and the parent (Transcript.add).
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
		e.MessageEntry = MessageEntry{
			Message:     ml.Message,
			Model:       Model{Provider: ml.Provider, Alias: ml.Model, WireModel: ml.WireModel},
			Effort:      ml.Effort,
			Usage:       ml.Usage,
			StopReason:  ml.StopReason,
			Interrupted: ml.Interrupted,
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
	}
	return e, nil
}
