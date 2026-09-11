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
	mu     sync.Mutex
	events chan agent.Event
	closed chan struct{}
	cancel chan struct{}
	hang   bool
	n      int
}

func NewStub() *Stub {
	return &Stub{
		events: make(chan agent.Event, 256),
		closed: make(chan struct{}),
		cancel: make(chan struct{}),
	}
}

func (s *Stub) HangNext() {
	s.mu.Lock()
	s.hang = true
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

func (s *Stub) SetModel(context.Context, string) error { return nil }
func (s *Stub) SetMode(context.Context, string) error  { return nil }

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
