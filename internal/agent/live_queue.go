package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/charliek/craze/internal/acp"
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
	// A steer is model work like a prompt (review C2c 2): it waits for a
	// claude fast reset in flight, and a stopped session refuses it with its
	// own error. The client's prompt gate checks again just before the write.
	if err := s.awaitFastFix(ctx); err != nil {
		return err
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
	// Text only (plan 033 P7): an envelope becomes [Image #N: <path>] lines.
	// The TUI sends none here; a socket client's mode "interject" can.
	err := client.Interject(ctx, interjectText(s.log, text), id)
	switch {
	case errors.Is(err, acp.ErrSteerIdle):
		// claude found no running turn (plan 052 § B7): the text was not
		// taken, and is to go as the next prompt, which the engine sees to.
		return ErrPromptRequired
	case errors.Is(err, acp.ErrSteerStartedTurn):
		// idleBehavior promptRequired should rule this out: the adapter
		// took the text as a turn of its own. It was taken, so it is no
		// failure; its frames arrive with no prompt in flight.
		s.log.Note(journal.DiagNote{Kind: "claude_steer_started_turn"})
		return nil
	}
	if err != nil {
		if f := s.fatalErr(); f != nil {
			// The session stopped under the steer: that is why, whatever the
			// closed connection said, as for a prompt.
			return f
		}
	}
	return err
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

// onACPForeignTurn is the client's foreign-turn bracket, as the session
// records it: claude's reason, a cancelled ending, and a failed one with its
// kind (plan 052 § E1, § E2). A turn the silence rule ended leaves a note.
func (s *session) onACPForeignTurn(f acp.ForeignTurn) {
	info := ForeignTurnInfo{ID: f.ID, Text: f.Text, Reason: f.Reason, Running: f.Running, Cancelled: f.Cancelled}
	if f.Err != nil {
		info.Err = &TurnError{Kind: ErrorInfo(f.Err).Kind, Message: f.Err.Error()}
	}
	if f.Silent {
		s.noteForeignSilence(f)
	}
	s.onForeignTurn(info)
}

// noteForeignSilence is the transcript note a foreign turn the silence rule
// ended leaves (§ E1 (b)): the journal's diagnostic and the diagnostic lane's
// line, as C2's and C3's notes are.
func (s *session) noteForeignSilence(f acp.ForeignTurn) {
	s.log.Note(journal.DiagNote{Kind: "claude_foreign_silence", Fields: map[string]any{"turn": f.ID, "reason": f.Reason}})
	if w := diagWriter(s.opts); w != nil {
		fmt.Fprintf(w, "craze: claude went quiet during a turn of its own (%s); treating it as over\n", sanitizeLine(f.ID))
	}
}

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
