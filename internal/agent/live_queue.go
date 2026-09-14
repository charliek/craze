package agent

import (
	"context"
	"fmt"
	"time"
)

// The queue's transactions are ordered by s.queueOp: a mutation and the
// events it produced go out together, so a consumer rebuilding the queue from
// the event stream never sees them interleaved with another transaction's.
// The lock order is queueOp → s.mu → the queue's own lock, and Snapshot takes
// the last two in that order as well.

// Queue appends a message to craze's own queue. The queue is craze-side on
// both providers: Client.Prompt keeps its one-in-flight rule and the drain is
// the only thing that ever starts a queued turn.
func (s *session) Queue(text string) (QueuedPrompt, error) {
	s.queueOp.Lock()
	defer s.queueOp.Unlock()
	p, ev, err := s.queue.Add(text, time.Now())
	if err != nil {
		return QueuedPrompt{}, err
	}
	s.emit(ev.Event())
	return p, nil
}

// EditQueued rewrites a row in place. The id and the position are the row's
// identity: an edit is not a cancel plus a re-queue.
func (s *session) EditQueued(id, text string) error {
	s.queueOp.Lock()
	defer s.queueOp.Unlock()
	ev, err := s.queue.Edit(id, text)
	if err != nil {
		return err
	}
	s.emit(ev.Event())
	return nil
}

func (s *session) Unqueue(id string) (QueuedPrompt, bool) {
	s.queueOp.Lock()
	defer s.queueOp.Unlock()
	ev, ok := s.queue.Remove(id)
	if !ok {
		return QueuedPrompt{}, false
	}
	s.emit(ev.Event())
	return ev.Prompt, true
}

// TakeQueued hands a row to the caller to prompt. It is a guard, not a
// driver: while a prompt is in flight or the agent is running a turn of its
// own there is nothing a caller could do with the row but lose it, so it is
// left in the queue. The guard and the removal are one critical section —
// a row that left the queue and could not then be prompted would simply be
// gone. Callers take only after Prompt has returned: EventDone alone is too
// early, because inPrompt clears after it is emitted.
func (s *session) TakeQueued(id string) (QueuedPrompt, bool) {
	s.queueOp.Lock()
	defer s.queueOp.Unlock()
	s.mu.Lock()
	if s.inPrompt || s.foreign {
		s.mu.Unlock()
		return QueuedPrompt{}, false
	}
	ev, ok := s.queue.Take(id)
	s.mu.Unlock()
	if !ok {
		return QueuedPrompt{}, false
	}
	s.emit(ev.Event())
	return ev.Prompt, true
}

// PopQueue is TakeQueued of the head, under the same guard.
func (s *session) PopQueue() (QueuedPrompt, bool) {
	s.queueOp.Lock()
	defer s.queueOp.Unlock()
	s.mu.Lock()
	if s.inPrompt || s.foreign {
		s.mu.Unlock()
		return QueuedPrompt{}, false
	}
	ev, ok := s.queue.Pop()
	s.mu.Unlock()
	if !ok {
		return QueuedPrompt{}, false
	}
	s.emit(ev.Event())
	return ev.Prompt, true
}

func (s *session) ClearQueue() int {
	s.queueOp.Lock()
	defer s.queueOp.Unlock()
	return s.clearQueueLocked()
}

// clearQueueLocked empties the queue and emits one removed event per row. The
// caller holds queueOp.
func (s *session) clearQueueLocked() int {
	evs := s.queue.Clear()
	for _, ev := range evs {
		s.emit(ev.Event())
	}
	return len(evs)
}

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
func (s *session) Interject(ctx context.Context, text string) error {
	if !s.provider().Capabilities().Interject {
		return ErrUnsupported
	}
	s.mu.Lock()
	client := s.client
	live := s.inPrompt && !s.doneEmitted && !s.cancelling
	s.interjectSeq++
	id := fmt.Sprintf("craze-%d", s.interjectSeq)
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
	s.mu.Lock()
	s.foreign = info.Running
	s.mu.Unlock()
	cp := info
	s.emit(Event{Type: EventForeignTurn, ForeignTurn: &cp})
}

// clearQueueOnError empties the queue when a turn ends in an error. A queue
// that survives an error would run behind the next prompt the user sends,
// long after they had stopped expecting it; an explicit clear with a note is
// the honest end.
func (s *session) clearQueueOnError() {
	s.queueOp.Lock()
	defer s.queueOp.Unlock()
	s.clearQueueLocked()
}
