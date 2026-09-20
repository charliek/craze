package tui

import (
	"context"

	"github.com/charliek/craze/internal/agent"
)

// The Stub's queue is the engine's, like every other session's (plan 021
// §3.5): the Stub itself holds none, and the chrome tests that queue a row do
// it through the engine that drives the Stub, not through any method here.

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
