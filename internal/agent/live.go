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
	if s.opts.Mode != "" {
		modeID, ok := ResolveMode(s.opts.Mode, availableModeIDs(sess.Modes))
		if !ok {
			_ = s.Close()
			return fmt.Errorf("agent: session did not advertise mode %q", s.opts.Mode)
		}
		if err := client.SetMode(ctx, modeID); err != nil {
			_ = s.Close()
			return err
		}
	}
	if s.opts.Model != "" {
		if err := client.SetModel(ctx, s.opts.Model); err != nil {
			_ = s.Close()
			return err
		}
	}
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
	return s.client.SetModel(ctx, modelID)
}

func (s *session) SetMode(ctx context.Context, modeID string) error {
	if s.client == nil {
		return fmt.Errorf("agent: session not started")
	}
	return s.client.SetMode(ctx, modeID)
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

func (s *session) onUpdate(n acp.SessionNotification) {
	var u acp.SessionUpdate
	if err := json.Unmarshal(n.Update, &u); err != nil {
		return
	}
	switch u.SessionUpdate {
	case acp.UpdateAgentMessage:
		text := ""
		if u.Content != nil {
			text = u.Content.Text
		}
		s.emit(Event{Type: EventText, Text: text})
	case acp.UpdateAgentThought:
		text := ""
		if u.Content != nil {
			text = u.Content.Text
		}
		s.emit(Event{Type: EventThought, Text: text})
	case acp.UpdateToolCall, acp.UpdateToolCallUpd:
		s.emit(Event{Type: EventTool, Tool: &ToolEvent{
			ID:     u.ToolCallID,
			Name:   u.Title,
			Status: u.Status,
		}})
	}
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
