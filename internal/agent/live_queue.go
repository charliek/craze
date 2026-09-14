package agent

import (
	"context"
	"fmt"
	"time"
)

// The queue's transactions are ordered by s.queueOp: a mutation and the
// events it produced go out together, so a consumer rebuilding the queue from
// the event stream never sees them interleaved with another transaction's.
// The lock order is queueOp → emitMu → s.mu → the queue's own lock, and
// Snapshot takes the last two in that order as well.

// queueTx runs one queue transaction. fn mutates the queue under queueOp and
// returns the events it produced; those are emitted after queueOp is released,
// because emit blocks while the event channel is full and a consumer that is
// waiting on something else holding queueOp — Prompt's error path clearing the
// queue, say — would wedge the session. emitMu is taken inside queueOp and
// held across the sends, so the transactions still reach the stream whole and
// in the order they happened. Lock order: queueOp → emitMu, never the reverse.
func (s *session) queueTx(fn func() []QueueEvent) {
	s.queueOp.Lock()
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	evs := func() []QueueEvent {
		// The mutation is the transaction; the emits are not, which is why
		// queueOp goes back before them.
		defer s.queueOp.Unlock()
		return fn()
	}()
	for _, ev := range evs {
		s.emit(ev.Event())
	}
}

// Queue appends a message to craze's own queue. The queue is craze-side on
// both providers: Client.Prompt keeps its one-in-flight rule and the drain is
// the only thing that ever starts a queued turn.
func (s *session) Queue(text string) (p QueuedPrompt, err error) {
	s.queueTx(func() []QueueEvent {
		var ev QueueEvent
		p, ev, err = s.queue.Add(text, time.Now())
		if err != nil {
			return nil
		}
		return []QueueEvent{ev}
	})
	return p, err
}

// EditQueued rewrites a row in place. The id and the position are the row's
// identity: an edit is not a cancel plus a re-queue.
func (s *session) EditQueued(id, text string) (err error) {
	s.queueTx(func() []QueueEvent {
		var ev QueueEvent
		ev, err = s.queue.Edit(id, text)
		if err != nil {
			return nil
		}
		return []QueueEvent{ev}
	})
	return err
}

func (s *session) Unqueue(id string) (p QueuedPrompt, ok bool) {
	s.queueTx(func() []QueueEvent {
		var ev QueueEvent
		ev, ok = s.queue.Remove(id)
		if !ok {
			return nil
		}
		p = ev.Prompt
		return []QueueEvent{ev}
	})
	return p, ok
}

// TakeQueued hands a row to the caller to prompt. It is a guard, not a
// driver: while a prompt is in flight or the agent is running a turn of its
// own there is nothing a caller could do with the row but lose it, so it is
// left in the queue. The guard and the removal are one critical section —
// a row that left the queue and could not then be prompted would simply be
// gone. Callers take only after Prompt has returned: EventDone alone is too
// early, because inPrompt clears after it is emitted.
func (s *session) TakeQueued(id string) (p QueuedPrompt, ok bool) {
	s.queueTx(func() []QueueEvent {
		ev, taken := s.takeGuarded(func() (QueueEvent, bool) { return s.queue.Take(id) })
		if !taken {
			return nil
		}
		p, ok = ev.Prompt, true
		return []QueueEvent{ev}
	})
	return p, ok
}

// PopQueue is TakeQueued of the head, under the same guard.
func (s *session) PopQueue() (p QueuedPrompt, ok bool) {
	s.queueTx(func() []QueueEvent {
		ev, taken := s.takeGuarded(s.queue.Pop)
		if !taken {
			return nil
		}
		p, ok = ev.Prompt, true
		return []QueueEvent{ev}
	})
	return p, ok
}

// takeGuarded is the guard and the removal as one critical section: a row that
// left the queue and could not then be prompted would simply be gone. The
// caller holds queueOp.
func (s *session) takeGuarded(take func() (QueueEvent, bool)) (QueueEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inPrompt || s.foreign {
		return QueueEvent{}, false
	}
	return take()
}

func (s *session) ClearQueue() int {
	n := 0
	s.queueTx(func() []QueueEvent {
		evs := s.queue.Clear()
		n = len(evs)
		return evs
	})
	return n
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
	s.ClearQueue()
}
