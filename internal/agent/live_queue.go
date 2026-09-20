package agent

import (
	"context"
	"fmt"

	"github.com/charliek/craze/internal/journal"
)

// Interject merges text into the running turn. It is refused in exactly the
// three states where grok would strand it and mint a turn of its own instead:
// no turn running, the turn already done, and a cancel in progress. Refusing
// here keeps the text in the caller's hands, which is where the user can see
// it.
//
// The check cannot be held across the wire, and it is not the last word
// either way: grok decides at its own next safe point, so a turn that ends
// while the request is in flight strands it regardless. That ending is the
// foreign turn craze already models and shows — this guard is what keeps the
// obvious cases from producing one.
//
// It is journaled like a prompt (plan 020 §3.5), and by the same helpers, so
// the refusals above are recorded rather than lost: the attempt id is the
// journal's own, not the craze-N id below, which only an accepted interjection
// ever spends.
func (s *session) Interject(ctx context.Context, text string) error {
	a := s.log.beginAttempt(journal.PromptKindInterject, text)
	err := s.interject(ctx, text)
	a.end("", err)
	return err
}

func (s *session) interject(ctx context.Context, text string) error {
	if !s.provider().Capabilities().Interject {
		return ErrUnsupported
	}
	s.mu.Lock()
	client := s.client
	live := s.inPrompt && !s.doneEmitted && !s.cancelling
	id := ""
	if client != nil && live {
		// The id correlates the ack and the broadcast with this request; a
		// refusal writes nothing, so it does not spend one.
		s.interjectSeq++
		id = fmt.Sprintf("craze-%d", s.interjectSeq)
	}
	s.mu.Unlock()
	if client == nil {
		return fmt.Errorf("agent: session not started")
	}
	if !live {
		return ErrNotInTurn
	}
	return client.Interject(ctx, text, id)
}

// onInterjection turns grok's broadcast into the transcript entry. The ack
// only says the text was accepted; the broadcast is what every client renders
// from, and grok sends one for the interjection it could not merge too.
func (s *session) onInterjection(n interjectionText) {
	s.emit(Event{Type: EventUser, Text: n.Text, Interjection: true})
}

// interjectionText is the shape onInterjection needs, spelled here so live.go
// is the only file that names the acp type.
type interjectionText struct{ Text string }

// onForeignTurn records a turn the agent started on its own and tells the
// callers, so the drain that was about to run waits and re-runs at its end.
func (s *session) onForeignTurn(info ForeignTurnInfo) {
	// A replayed turn_completed is history, not a turn craze is watching.
	// Today grok replays it on a method craze does not dispatch (plan 013
	// §2.1), so this guard is insurance against it moving: a foreign turn
	// opened or closed by the transcript would leave the drain waiting on a
	// turn that ended yesterday.
	if s.replaying.Load() {
		return
	}
	s.mu.Lock()
	s.foreign = info.Running
	s.mu.Unlock()
	cp := info
	s.emit(Event{Type: EventForeignTurn, ForeignTurn: &cp})
}
