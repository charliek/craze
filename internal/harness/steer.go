package harness

import (
	"context"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/store"
)

// Interjection: steering a turn that is already running (plan 019 §3.10,
// D-34).
//
// Steer hands the live turn a user message. Before each of its later steps
// the turn takes up whatever has been accepted (prepareStep), records each as
// a splice at the index it landed at, and rebuilds the step's input with every
// splice re-inserted. The rebuild is the whole trick: Fantasy composes a
// step's messages from initialPrompt + responseMessages and applies
// PrepareStep's list to that step alone (agent.go:943-970), so a list built by
// appending in place would show the model the steer once and lose it at the
// next step. Re-inserting at a fixed index keeps the model reading it, keeps
// the request's prefix — and with it the provider's cache — stable, and leaves
// the in-turn history identical to what the transcript replays next turn.
//
// A steer is written by the first step that saw it, as a leading user entry of
// that step's append (store.AppendStep). One that no step persisted — the turn
// ended before a step took it up, or that step's stream failed or was
// cancelled, so OnStepFinish never fired — comes back in Result.Unanswered for
// the caller to put where the user can still see it. Accepted text is written
// or returned, never both and never neither.

// steerCap bounds the steers one turn may accept. Every steer a turn does not
// answer becomes a queued row in the adapter, emitted as it goes in, so a turn
// that could accept without limit could requeue without limit: a burst big
// enough to fill a consumer's event channel by itself, which is what turns the
// queue's ordinary "one blocking send under a lock" into a wedge. It is the
// queue's own cap (agent.queueCap), restated here because the harness cannot
// import the adapter.
//
// Refusing loses nothing: the caller keeps the text, exactly as it keeps a
// message the queue was too full to take. The rule a cap must not break is
// that text already accepted always has somewhere to go, and it still holds.
const steerCap = 32

// SteerToken names the turn a steer is meant for. It changes with every turn
// and is zero when none is running, so text typed into one turn can never be
// taken up by the next: the caller reads the token in the same critical
// section in which it decides a turn is live, and hands it straight back to
// Steer, which refuses it once the turn has moved on.
//
// A token is unique within one Session and means nothing outside it. That is
// all the adapter needs, since a native session holds one harness session for
// its whole life and never swaps it.
type SteerToken uint64

// steerbox is the accept side: the turn a steer may be handed to, and the ones
// it has been handed and not yet taken up.
//
// It has its own lock, and that lock is a leaf — nothing is called and no
// other lock is taken while it is held, and it is never held across an emit, a
// store write or a request. So Steer never waits on the turn's goroutine,
// which spends its life inside the sink (under the turn's own lock), inside a
// provider's stream, or inside the store. That matters: the goroutine that
// interjects may be the very one draining the sink's events, and a Steer that
// could block on the turn would deadlock it.
type steerbox struct {
	mu sync.Mutex
	// live is the running turn's token, zero when no turn can take a steer;
	// issued is the last token handed out, so a token is never reused.
	live, issued SteerToken
	accepted     int      // steers this turn has taken, bounded by steerCap
	text         []string // accepted, not yet taken up by a step
}

// begin opens the box for a turn, under a token of its own. Anything left in
// it belongs to a turn that has already reported it unanswered, so it is
// dropped rather than carried into this one.
func (b *steerbox) begin() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.issued++
	b.live, b.accepted, b.text = b.issued, 0, nil
}

// token is the running turn's, or zero.
func (b *steerbox) token() SteerToken {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.live
}

// accept takes text for the turn tok names. It refuses with ErrNotInTurn when
// no turn is running or when the one that is, is not tok's — the turn tok
// named has ended, and text typed into it is not this one's to answer — and
// with ErrTooManySteers once that turn has taken steerCap of them.
func (b *steerbox) accept(tok SteerToken, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if tok == 0 || b.live != tok {
		return ErrNotInTurn
	}
	if b.accepted >= steerCap {
		return ErrTooManySteers
	}
	b.accepted++
	b.text = append(b.text, text)
	return nil
}

// take empties the box and leaves it open: the drain before a step.
func (b *steerbox) take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.text
	b.text = nil
	return out
}

// close empties the box and shuts it in one critical section, so an accept
// either lands in what it returns or is refused — never neither, and never
// both. It is idempotent: the turn closes the box, and the session closes it
// again for a Run that ended before it opened one.
func (b *steerbox) close() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.text
	b.live, b.text = 0, nil
	return out
}

// SteerToken is the token of the turn a steer would go to now, or zero when no
// turn is running. A caller reads it in the same critical section in which it
// decides that a turn is live and that the text belongs to that turn, and
// hands it to Steer.
//
// It is what closes the window between the two: without it, a caller that
// found a turn live, then lost its own lock while that turn ended and the next
// began, would have its text taken up by a turn the user never typed it into —
// written to that turn's transcript, and announced after the first turn's
// ending. With it, the stale token is simply refused.
func (s *Session) SteerToken() SteerToken { return s.steers.token() }

// Steer merges text into the turn tok names, without cancelling it: the model
// reads it as a user message at that turn's next step, and the transcript
// records it there too (plan 019 §3.10, D-34).
//
// It reports ErrNotInTurn when no turn is running, when the turn tok named has
// settled, or when tok is some other turn's (see SteerToken);
// ErrTooManySteers once that turn has taken steerCap of them; and
// ErrEmptyPrompt for text with nothing but whitespace in it, which no model
// has anything to do with. No refusal changes anything, so the caller still
// holds the text.
//
// Accepting is not a promise that the model will answer: a turn whose model is
// simply finished ends without another step to take the steer up. Every steer
// a turn accepted and did not persist comes back in Result.Unanswered, on the
// turn's failing paths too.
//
// It is safe from any goroutine, Run's sink included, and never waits on the
// turn: the one lock it takes is held by nothing else (see steerbox).
func (s *Session) Steer(tok SteerToken, text string) error {
	if strings.TrimSpace(text) == "" {
		return ErrEmptyPrompt
	}
	return s.steers.accept(tok, text)
}

// splice is one steer the turn has taken up: the text, the user message it
// becomes — byte for byte what Run sends for a prompt and what the store
// writes — and the index it is re-inserted at in every later step's input.
type splice struct {
	text string
	at   int
	msg  fantasy.Message
}

// prepareStep is the turn's PrepareStep. Before every step after the first it
// takes up whatever Steer has accepted — only while the turn's context is
// live, since a step that will not be sent can answer nothing — and hands the
// step an input with every steer this turn has taken up re-inserted at its own
// index.
//
// The first step is skipped: its input is the prompt Run was called with, and
// a steer accepted before it has a whole turn ahead of it to be taken up in.
func (t *turn) prepareStep(ctx context.Context, o fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
	if o.StepNumber == 0 {
		return ctx, fantasy.PrepareStepResult{}, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ctx.Err() == nil {
		for _, text := range t.steers.take() {
			t.spliced = append(t.spliced, splice{text: text, at: len(o.Messages), msg: fantasy.NewUserMessage(text)})
			// The turn's goroutine is the only one that reports a steer, and
			// it reports every one it accepted before the turn's own ending
			// (see finish): nothing shown for an interjection can follow the
			// turn's end, and no lock of the caller's is held to do it.
			t.emit(Steered{Text: text})
		}
	}
	if len(t.spliced) == 0 {
		return ctx, fantasy.PrepareStepResult{}, nil
	}
	return ctx, fantasy.PrepareStepResult{Messages: t.spliceInto(o.Messages)}, nil
}

// spliceInto is base with every steer re-inserted at the index it was recorded
// at, in the order they were taken up; base — Fantasy's own slice — is never
// written to. The indices are non-decreasing, because base only ever grows at
// its end, so one pass places them all. mu is held.
func (t *turn) spliceInto(base []fantasy.Message) []fantasy.Message {
	out := make([]fantasy.Message, 0, len(base)+len(t.spliced))
	next := 0
	for i := 0; i <= len(base); i++ {
		for next < len(t.spliced) && t.spliced[next].at <= i {
			out = append(out, t.spliced[next].msg)
			next++
		}
		if i < len(base) {
			out = append(out, base[i])
		}
	}
	// An index past the end cannot happen today (base grows); a steer would
	// still go out rather than be silently dropped from the request.
	for ; next < len(t.spliced); next++ {
		out = append(out, t.spliced[next].msg)
	}
	return out
}

// unwritten are the steers no append has written yet. They are always a
// suffix: a step's append writes every steer taken up before it, or writes
// none. mu is held.
func (t *turn) unwritten() []splice { return t.spliced[t.written:] }

// steerEntries are the unwritten steers as the store takes them: the leading
// user entries of the step about to be written (store.AppendStep). mu is held.
func (t *turn) steerEntries() []store.MessageEntry {
	pending := t.unwritten()
	if len(pending) == 0 {
		return nil
	}
	out := make([]store.MessageEntry, 0, len(pending))
	for _, sp := range pending {
		out = append(out, store.MessageEntry{Message: sp.msg, Model: t.model.id(), Effort: t.model.effort})
	}
	return out
}

// settleSteers ends the turn's side of Interject, under mu, as the first thing
// finish does. It closes the box — from here Steer refuses — reports every
// steer no step took up, so the caller sees each accepted one exactly once and
// always before the turn's ending, and returns the text of every steer no step
// persisted, in the order it was accepted. mu is held.
func (t *turn) settleSteers() []string {
	late := t.steers.close()
	var unanswered []string
	for _, sp := range t.unwritten() {
		unanswered = append(unanswered, sp.text)
	}
	for _, text := range late {
		unanswered = append(unanswered, text)
		t.emit(Steered{Text: text})
	}
	return unanswered
}
