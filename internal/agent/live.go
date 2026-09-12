package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/charliek/craze/internal/acp"
)

type session struct {
	opts   Options
	client *acp.Client
	events chan Event
	done   chan struct{}

	mu         sync.Mutex
	started    bool
	closed     bool
	inPrompt   bool
	promptDone chan struct{}
	waiting    map[string]pendingPerm
	permSeq    int
	snap       Snapshot
	tools      map[string]ToolEvent
	toolOrder  []string
}

type pendingPerm struct {
	options []acp.PermissionOption
	decide  chan acp.PermissionDecision
}

func New(opts Options) Session {
	return newSession(opts)
}

func newSession(opts Options) *session {
	return &session{
		opts:    opts,
		events:  make(chan Event, 256),
		done:    make(chan struct{}),
		waiting: make(map[string]pendingPerm),
		tools:   make(map[string]ToolEvent),
	}
}

func (s *session) Events() <-chan Event {
	return s.events
}

func (s *session) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("agent: session already started")
	}
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("agent: session closed")
	}
	s.started = true
	s.mu.Unlock()

	args := append([]string{}, s.opts.ExtraArgs...)
	if s.opts.Force {
		args = append(args, "--force")
	}
	args = append(args, "--trust", "acp")

	cwd := s.opts.Workspace
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			s.unstart()
			return err
		}
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		s.unstart()
		return err
	}

	client, err := acp.Spawn(acp.SpawnOptions{
		Binary: s.opts.Binary,
		Args:   args,
		Dir:    cwd,
		Env:    s.opts.Env,
		Stderr: s.opts.Stderr,
	})
	if err != nil {
		s.unstart()
		return err
	}
	s.client = client
	client.SetUpdateHandler(s.onUpdate)
	client.SetPermissionHandler(s.onPermission)

	initRes, err := client.Initialize(ctx)
	if err != nil {
		_ = s.Close()
		return err
	}
	if initRes.OffersCursorLogin() {
		if err := client.Authenticate(ctx); err != nil {
			_ = s.Close()
			return fmt.Errorf("%w (run `agent login`)", err)
		}
	}
	sess, err := client.NewSession(ctx, cwd)
	if err != nil {
		_ = s.Close()
		return err
	}
	snap := snapshotFromNew(sess)
	if s.opts.Mode != "" {
		modeID, ok := ResolveMode(s.opts.Mode, modeIDs(snap.Modes))
		if !ok {
			_ = s.Close()
			return fmt.Errorf("agent: session did not advertise mode %q", s.opts.Mode)
		}
		if err := client.SetMode(ctx, modeID); err != nil {
			_ = s.Close()
			return err
		}
		snap.CurrentMode = modeID
	}
	if s.opts.Model != "" {
		if err := client.SetModel(ctx, s.opts.Model); err != nil {
			_ = s.Close()
			return err
		}
		snap.CurrentModel = s.opts.Model
	}
	s.mu.Lock()
	commands := s.snap.Commands
	s.snap = snap
	if len(s.snap.Commands) == 0 {
		s.snap.Commands = commands
	}
	s.mu.Unlock()
	return nil
}

func (s *session) unstart() {
	s.mu.Lock()
	s.started = false
	s.mu.Unlock()
}

func (s *session) Prompt(ctx context.Context, text string) (Result, error) {
	s.mu.Lock()
	if s.client == nil {
		s.mu.Unlock()
		return Result{}, fmt.Errorf("agent: session not started")
	}
	if s.inPrompt {
		s.mu.Unlock()
		return Result{}, acp.ErrPromptInFlight
	}
	s.inPrompt = true
	done := make(chan struct{})
	s.promptDone = done
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inPrompt = false
		close(done)
		s.mu.Unlock()
	}()

	res, err := s.client.Prompt(ctx, text)
	if err != nil {
		s.emit(Event{Type: EventError, Err: err})
		return Result{}, err
	}
	s.emit(Event{Type: EventDone, StopReason: res.StopReason})
	return Result{StopReason: res.StopReason}, nil
}

func (s *session) SetModel(ctx context.Context, modelID string) error {
	if s.client == nil {
		return fmt.Errorf("agent: session not started")
	}
	if err := s.client.SetModel(ctx, modelID); err != nil {
		return err
	}
	s.mu.Lock()
	s.snap.CurrentModel = modelID
	s.mu.Unlock()
	return nil
}

func (s *session) SetMode(ctx context.Context, modeID string) error {
	if s.client == nil {
		return fmt.Errorf("agent: session not started")
	}
	if err := s.client.SetMode(ctx, modeID); err != nil {
		return err
	}
	s.mu.Lock()
	s.snap.CurrentMode = modeID
	s.mu.Unlock()
	return nil
}

func (s *session) SetConfig(ctx context.Context, id, value string) error {
	if s.client == nil {
		return fmt.Errorf("agent: session not started")
	}
	if err := s.client.SetConfig(ctx, id, value); err != nil {
		return err
	}
	s.mu.Lock()
	for i := range s.snap.Config {
		if s.snap.Config[i].ID == id {
			s.snap.Config[i].Current = value
			break
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snap
	out.Models = append([]ModelInfo(nil), s.snap.Models...)
	out.Modes = append([]ModeInfo(nil), s.snap.Modes...)
	out.Commands = append([]CommandInfo(nil), s.snap.Commands...)
	out.Config = cloneConfig(s.snap.Config)
	out.Tools = snapshotTools(s.toolOrder, s.tools)
	return out
}

func (s *session) Cancel(ctx context.Context) error {
	if s.client == nil {
		return nil
	}
	s.cancelWaitingPerms()
	if err := s.client.Cancel(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	done := s.promptDone
	in := s.inPrompt
	s.mu.Unlock()
	if !in || done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *session) cancelWaitingPerms() {
	s.mu.Lock()
	pending := s.waiting
	s.waiting = make(map[string]pendingPerm)
	s.mu.Unlock()
	for _, p := range pending {
		select {
		case p.decide <- acp.PermissionDecision{Cancelled: true}:
		default:
		}
	}
}

func (s *session) AnswerPermission(id, optionID string) error {
	s.mu.Lock()
	p, ok := s.waiting[id]
	if ok {
		delete(s.waiting, id)
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("agent: unknown permission request %q", id)
	}
	if optionID == "" {
		select {
		case p.decide <- acp.PermissionDecision{Cancelled: true}:
			return nil
		case <-s.done:
			return fmt.Errorf("agent: session closed")
		}
	}
	found := false
	for _, o := range p.options {
		if o.OptionID == optionID {
			found = true
			break
		}
	}
	if !found {
		select {
		case p.decide <- acp.PermissionDecision{Cancelled: true}:
		default:
		}
		return fmt.Errorf("agent: optionId %q is not in the permission request", optionID)
	}
	select {
	case p.decide <- acp.PermissionDecision{OptionID: optionID}:
		return nil
	case <-s.done:
		return fmt.Errorf("agent: session closed")
	}
}

func (s *session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	client := s.client
	s.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
	return nil
}

func (s *session) onPermission(req acp.PermissionRequest) acp.PermissionDecision {
	if s.opts.Force {
		id, ok := acp.PickYoloAllow(req.Options)
		if !ok {
			return acp.PermissionDecision{Cancelled: true}
		}
		return acp.PermissionDecision{OptionID: id}
	}
	id := s.nextPermID()
	ch := make(chan acp.PermissionDecision, 1)
	s.mu.Lock()
	s.waiting[id] = pendingPerm{options: req.Options, decide: ch}
	s.mu.Unlock()

	opts := make([]PermissionOption, 0, len(req.Options))
	for _, o := range req.Options {
		opts = append(opts, PermissionOption{OptionID: o.OptionID, Name: o.Name, Kind: o.Kind})
	}
	s.emit(Event{
		Type: EventPermission,
		Permission: &PermissionEvent{
			ID:      id,
			Tool:    req.ToolCall.Title,
			Options: opts,
		},
	})
	select {
	case dec := <-ch:
		return dec
	case <-s.done:
		return acp.PermissionDecision{Cancelled: true}
	}
}

func (s *session) nextPermID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.permSeq++
	return fmt.Sprintf("perm-%d", s.permSeq)
}

type sessionUpdateWire struct {
	SessionUpdate     string                 `json:"sessionUpdate"`
	Content           json.RawMessage        `json:"content,omitempty"`
	ToolCallID        string                 `json:"toolCallId,omitempty"`
	Title             *string                `json:"title,omitempty"`
	Kind              *string                `json:"kind,omitempty"`
	Status            *string                `json:"status,omitempty"`
	RawInput          json.RawMessage        `json:"rawInput,omitempty"`
	Locations         json.RawMessage        `json:"locations,omitempty"`
	AvailableCommands []acp.AvailableCommand `json:"availableCommands,omitempty"`
	CurrentModeID     string                 `json:"currentModeId,omitempty"`
	ConfigOptions     json.RawMessage        `json:"configOptions,omitempty"`
}

func (s *session) onUpdate(n acp.SessionNotification) {
	var u sessionUpdateWire
	if err := json.Unmarshal(n.Update, &u); err != nil {
		return
	}
	switch u.SessionUpdate {
	case acp.UpdateAgentMessage:
		s.emit(Event{Type: EventText, Text: messageText(u.Content)})
	case acp.UpdateAgentThought:
		s.emit(Event{Type: EventThought, Text: messageText(u.Content)})
	case acp.UpdateToolCall, acp.UpdateToolCallUpd:
		delta, ok := toolDeltaFromWire(u)
		if !ok {
			return
		}
		tool, emit := s.mergeTool(delta)
		if emit {
			s.emit(Event{Type: EventTool, Tool: &tool})
		}
	case acp.UpdateAvailableCommands:
		s.mu.Lock()
		s.snap.Commands = commandsFromUpdate(u.AvailableCommands)
		s.mu.Unlock()
		s.emit(Event{Type: EventMeta})
	case acp.UpdateCurrentMode:
		if u.CurrentModeID != "" {
			s.mu.Lock()
			s.snap.CurrentMode = u.CurrentModeID
			s.mu.Unlock()
			s.emit(Event{Type: EventMeta})
		}
	case acp.UpdateConfigOption:
		cfg := parseConfigOptions(u.ConfigOptions)
		s.mu.Lock()
		s.snap.Config = cfg
		s.mu.Unlock()
		s.emit(Event{Type: EventMeta})
	}
}

func toolDeltaFromWire(u sessionUpdateWire) (toolDelta, bool) {
	if u.ToolCallID == "" {
		return toolDelta{}, false
	}
	d := toolDelta{
		id:     u.ToolCallID,
		title:  u.Title,
		kind:   u.Kind,
		status: u.Status,
	}
	if raw, ok := presentJSON(u.RawInput); ok {
		d.rawInput = raw
		d.hasRawInput = true
	}
	if raw, ok := presentJSON(u.Content); ok && raw[0] == '[' {
		d.content = raw
		d.hasContent = true
	}
	if raw, ok := presentJSON(u.Locations); ok {
		d.locations = raw
		d.hasLocations = true
	}
	return d, true
}

func messageText(raw json.RawMessage) string {
	raw, ok := presentJSON(raw)
	if !ok || raw[0] != '{' {
		return ""
	}
	var b acp.ContentBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		return ""
	}
	return b.Text
}

func (s *session) emit(ev Event) {
	select {
	case <-s.done:
		return
	default:
	}
	select {
	case s.events <- ev:
	case <-s.done:
	}
}

func (s *session) promptInFlight() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inPrompt
}
