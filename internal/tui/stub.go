package tui

import (
	"context"
	"fmt"
	"sync"

	"github.com/charliek/craze/internal/agent"
)

// Stub is an in-process Session used by TUI chrome tests. It echoes each
// prompt as assistant text and does not spawn cursor-agent.
type Stub struct {
	mu        sync.Mutex
	events    chan agent.Event
	closed    chan struct{}
	cancel    chan struct{}
	hang      bool
	n         int
	failMode  bool
	failModel bool
	snap      agent.Snapshot
}

func NewStub() *Stub {
	return &Stub{
		events: make(chan agent.Event, 256),
		closed: make(chan struct{}),
		cancel: make(chan struct{}, 1),
		snap: agent.Snapshot{
			CurrentModel: "grok",
			CurrentMode:  "agent",
			Models: []agent.ModelInfo{
				{ID: "grok", Name: "Grok"},
				{ID: "fast", Name: "Fast"},
			},
			Modes: []agent.ModeInfo{
				{ID: "agent", Name: "Agent"},
				{ID: "plan", Name: "Plan"},
				{ID: "ask", Name: "Ask"},
			},
			Commands: []agent.CommandInfo{
				{Name: "research", Description: "Agent-advertised command"},
			},
		},
	}
}

func (s *Stub) HangNext() {
	s.mu.Lock()
	s.hang = true
	s.mu.Unlock()
}

// SetTools replaces Snapshot.Tools (copy-on-write). Tests send EventTool
// afterwards so the TUI refreshSnap() picks the in-flight set up.
func (s *Stub) SetTools(tools []agent.ToolEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Tools = cloneStubTools(tools)
}

func (s *Stub) FailNextSetMode() {
	s.mu.Lock()
	s.failMode = true
	s.mu.Unlock()
}

func (s *Stub) FailNextSetModel() {
	s.mu.Lock()
	s.failModel = true
	s.mu.Unlock()
}

func (s *Stub) Start(context.Context) error { return nil }

func (s *Stub) Events() <-chan agent.Event { return s.events }

func (s *Stub) Prompt(ctx context.Context, text string) (agent.Result, error) {
	s.mu.Lock()
	hang := s.hang
	s.hang = false
	s.n++
	n := s.n
	s.mu.Unlock()

	if hang {
		select {
		case <-s.cancel:
		case <-s.closed:
		case <-ctx.Done():
		}
		s.emit(agent.Event{Type: agent.EventDone, StopReason: "cancelled"})
		return agent.Result{StopReason: "cancelled"}, nil
	}

	reply := "echo: " + text
	if n >= 2 {
		reply = "follow-up: " + text
	}
	s.emit(agent.Event{Type: agent.EventText, Text: reply})
	s.emit(agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	return agent.Result{StopReason: "end_turn"}, nil
}

func (s *Stub) Cancel(context.Context) error {
	select {
	case s.cancel <- struct{}{}:
	default:
	}
	return nil
}

func (s *Stub) AnswerPermission(string, string) error {
	return fmt.Errorf("stub: no permission request")
}

func (s *Stub) SetModel(_ context.Context, id string) error {
	s.mu.Lock()
	fail := s.failModel
	s.failModel = false
	if fail {
		s.mu.Unlock()
		return fmt.Errorf("stub: set model failed")
	}
	s.snap.CurrentModel = id
	s.mu.Unlock()
	return nil
}

func (s *Stub) SetMode(_ context.Context, id string) error {
	s.mu.Lock()
	fail := s.failMode
	s.failMode = false
	if fail {
		s.mu.Unlock()
		return fmt.Errorf("stub: set mode failed")
	}
	s.snap.CurrentMode = id
	s.mu.Unlock()
	return nil
}

func (s *Stub) SetConfig(_ context.Context, id, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.snap.Config {
		if s.snap.Config[i].ID == id {
			s.snap.Config[i].Current = value
			break
		}
	}
	return nil
}

func (s *Stub) Snapshot() agent.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.snap
	out.Models = append([]agent.ModelInfo(nil), s.snap.Models...)
	out.Modes = append([]agent.ModeInfo(nil), s.snap.Modes...)
	out.Commands = append([]agent.CommandInfo(nil), s.snap.Commands...)
	out.Config = cloneStubConfig(s.snap.Config)
	out.Tools = cloneStubTools(s.snap.Tools)
	return out
}

func cloneStubConfig(in []agent.ConfigOption) []agent.ConfigOption {
	if in == nil {
		return nil
	}
	out := make([]agent.ConfigOption, len(in))
	for i, c := range in {
		out[i] = c
		if c.SelectValues != nil {
			out[i].SelectValues = append([]agent.SelectValue(nil), c.SelectValues...)
		}
	}
	return out
}

func cloneStubTools(in []agent.ToolEvent) []agent.ToolEvent {
	if in == nil {
		return nil
	}
	out := make([]agent.ToolEvent, len(in))
	for i, t := range in {
		out[i] = t
		if t.Locations != nil {
			out[i].Locations = append([]string(nil), t.Locations...)
		}
	}
	return out
}

func (s *Stub) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

func (s *Stub) emit(ev agent.Event) {
	select {
	case <-s.closed:
		return
	default:
	}
	select {
	case s.events <- ev:
	case <-s.closed:
	}
}

var _ agent.Session = (*Stub)(nil)
