package agent

import (
	"context"
	"io"

	"github.com/charliek/craze/internal/acp"
)

var ErrPromptInFlight = acp.ErrPromptInFlight

type EventType string

const (
	EventText       EventType = "text"
	EventThought    EventType = "thought"
	EventTool       EventType = "tool"
	EventPermission EventType = "permission"
	EventDone       EventType = "done"
	EventError      EventType = "error"
	EventMeta       EventType = "meta"
)

type ModelInfo struct {
	ID   string
	Name string
}

type ModeInfo struct {
	ID   string
	Name string
}

type CommandInfo struct {
	Name        string
	Description string
}

type Snapshot struct {
	Models       []ModelInfo
	Modes        []ModeInfo
	Commands     []CommandInfo
	Config       []ConfigOption
	Tools        []ToolEvent
	CurrentModel string
	CurrentMode  string
}

type ConfigOption struct {
	ID           string
	Name         string
	Category     string
	Type         string
	Current      string
	SelectValues []SelectValue
}

type SelectValue struct {
	Value string
	Name  string
}

type Event struct {
	Type       EventType
	Text       string
	Tool       *ToolEvent
	Permission *PermissionEvent
	Err        error
	StopReason string
}

type ToolEvent struct {
	ID          string
	Name        string
	Status      string
	Kind        string
	Title       string
	RawInput    string
	ContentText string
	Locations   []string
}

type PermissionOption struct {
	OptionID string
	Name     string
	Kind     string
}

type PermissionEvent struct {
	ID      string
	Tool    string
	Options []PermissionOption
}

type Result struct {
	StopReason string
}

type Options struct {
	Binary    string
	ExtraArgs []string
	Workspace string
	Force     bool
	Model     string
	Mode      string
	Stderr    io.Writer
	Env       []string
}

type Session interface {
	Start(ctx context.Context) error
	Prompt(ctx context.Context, text string) (Result, error)
	Events() <-chan Event
	Cancel(ctx context.Context) error
	AnswerPermission(id, optionID string) error
	SetModel(ctx context.Context, modelID string) error
	SetMode(ctx context.Context, modeID string) error
	SetConfig(ctx context.Context, id, value string) error
	Snapshot() Snapshot
	Close() error
}
