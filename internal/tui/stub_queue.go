package tui

import (
	"context"

	"github.com/charliek/craze/internal/agent"
)

// The Stub's queue is the real one: agent.PromptQueue is the whole of craze's
// queue state, so the chrome tests exercise the same transactions, bounds and
// events a live session does. Only the guards around it are the stub's own.

func (s *Stub) Queue(text string) (agent.QueuedPrompt, error) {
	s.queueOp.Lock()
	defer s.queueOp.Unlock()
	p, ev, err := s.queue.Add(text, s.now())
	if err != nil {
		return agent.QueuedPrompt{}, err
	}
	s.emit(ev.Event())
	return p, nil
}

func (s *Stub) EditQueued(id, text string) error {
	s.queueOp.Lock()
	defer s.queueOp.Unlock()
	ev, err := s.queue.Edit(id, text)
	if err != nil {
		return err
	}
	s.emit(ev.Event())
	return nil
}

func (s *Stub) Unqueue(id string) (agent.QueuedPrompt, bool) {
	s.queueOp.Lock()
	defer s.queueOp.Unlock()
	ev, ok := s.queue.Remove(id)
	if !ok {
		return agent.QueuedPrompt{}, false
	}
	s.emit(ev.Event())
	return ev.Prompt, true
}

func (s *Stub) TakeQueued(id string) (agent.QueuedPrompt, bool) {
	s.queueOp.Lock()
	defer s.queueOp.Unlock()
	s.mu.Lock()
	if s.inPrompt || s.foreign {
		s.mu.Unlock()
		return agent.QueuedPrompt{}, false
	}
	ev, ok := s.queue.Take(id)
	s.mu.Unlock()
	if !ok {
		return agent.QueuedPrompt{}, false
	}
	s.emit(ev.Event())
	return ev.Prompt, true
}

func (s *Stub) PopQueue() (agent.QueuedPrompt, bool) {
	s.queueOp.Lock()
	defer s.queueOp.Unlock()
	s.mu.Lock()
	if s.inPrompt || s.foreign {
		s.mu.Unlock()
		return agent.QueuedPrompt{}, false
	}
	ev, ok := s.queue.Pop()
	s.mu.Unlock()
	if !ok {
		return agent.QueuedPrompt{}, false
	}
	s.emit(ev.Event())
	return ev.Prompt, true
}

func (s *Stub) ClearQueue() int {
	s.queueOp.Lock()
	defer s.queueOp.Unlock()
	evs := s.queue.Clear()
	for _, ev := range evs {
		s.emit(ev.Event())
	}
	return len(evs)
}

// Interject follows the live guards: the provider has to have it, and there
// has to be a turn left to merge into.
func (s *Stub) Interject(_ context.Context, text string) error {
	if !s.snapProvider().Capabilities().Interject {
		return agent.ErrUnsupported
	}
	s.mu.Lock()
	live := s.inPrompt && !s.doneEmitted && !s.cancelling
	err := s.interjectErr
	s.interjectErr = nil
	s.mu.Unlock()
	if !live {
		return agent.ErrNotInTurn
	}
	if err != nil {
		return err
	}
	s.emit(agent.Event{Type: agent.EventUser, Text: text, Interjection: true})
	return nil
}

// FailNextInterject makes the next Interject fail the way a refused ack does.
func (s *Stub) FailNextInterject(err error) {
	s.mu.Lock()
	s.interjectErr = err
	s.mu.Unlock()
}

// SetForeignTurn drives the foreign-turn state a grok fallback produces.
func (s *Stub) SetForeignTurn(info agent.ForeignTurnInfo) {
	s.mu.Lock()
	s.foreign = info.Running
	s.mu.Unlock()
	cp := info
	s.emit(agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &cp})
}

func (s *Stub) snapProvider() agent.ProviderInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap.Provider
}
