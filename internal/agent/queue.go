package agent

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// queueCap and queueTextCap bound craze's own queue. Both refuse rather
	// than truncate: a queued message is the user's text on its way to the
	// agent, not display metadata, and half of it is worse than none.
	queueCap     = 32
	queueTextCap = 32 << 10
)

var (
	// ErrQueueFull and ErrQueueTextTooLong refuse without mutating anything,
	// so the caller still has the draft it tried to queue.
	ErrQueueFull        = errors.New("agent: queue is full")
	ErrQueueTextTooLong = errors.New("agent: message too long")
	// ErrNotInTurn refuses an interjection with no running turn to merge it
	// into: idle, already done, or being cancelled. Those are exactly the
	// cases where grok would mint a turn of its own instead.
	ErrNotInTurn = errors.New("agent: no running turn to interject into")
)

// QueuedPrompt is one message waiting for the running turn to end. Version is
// bumped by every edit, so a UI can tell an edited row from a new one.
type QueuedPrompt struct {
	ID       string
	Text     string
	QueuedAt time.Time
	Version  int
}

// QueueChange is what happened to a queued message.
type QueueChange string

const (
	QueueQueued  QueueChange = "queued"
	QueueEdited  QueueChange = "edited"
	QueueRemoved QueueChange = "removed"
	// QueueSent is emitted when a row leaves the queue to be prompted. The
	// caller does the prompting; the queue only records that the row is gone.
	QueueSent QueueChange = "sent"
)

// QueueEvent is one change, with the position the row held when it happened.
// Position is carried on the event because it is a fact about the change, not
// about the queue afterwards.
type QueueEvent struct {
	Prompt QueuedPrompt
	Change QueueChange
	Pos    int
}

// PromptQueue is craze's own message queue. It is craze-side on purpose: grok
// has a server queue and cursor has none, and one code path that behaves the
// same on both is worth more than either.
//
// Every method is one transaction, and the events it returns are emitted by
// the caller. Callers hold their own transaction lock across the mutation and
// its events, so consumers rebuilding the queue from the event stream see the
// changes in the order they happened. The lock order is
// caller's transaction lock → session lock → this one; nothing here ever
// calls back into a caller, so it is the innermost.
type PromptQueue struct {
	mu    sync.Mutex
	seq   int
	items []QueuedPrompt
}

// Add appends text. A full queue or an oversized message is refused with
// nothing mutated.
func (q *PromptQueue) Add(text string, now time.Time) (QueuedPrompt, QueueEvent, error) {
	if len(text) > queueTextCap {
		return QueuedPrompt{}, QueueEvent{}, ErrQueueTextTooLong
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) >= queueCap {
		return QueuedPrompt{}, QueueEvent{}, ErrQueueFull
	}
	q.seq++
	p := QueuedPrompt{ID: fmt.Sprintf("q-%d", q.seq), Text: text, QueuedAt: now}
	q.items = append(q.items, p)
	return p, QueueEvent{Prompt: p, Change: QueueQueued, Pos: len(q.items) - 1}, nil
}

// PushFront puts text at the head, ahead of everything already waiting, and
// is the one way in that neither cap bounds. It carries text the user typed
// into a running turn that the turn could not answer — an interjection the
// harness accepted and no step wrote down (plan 019 §3.10) — and a cap must
// not be the reason typed text vanishes: it has nowhere else to go, since
// refusing here would drop it rather than hand it back. Nothing else uses it;
// everything a user queues by hand goes through Add.
//
// From the head the row is an ordinary queued message: the drain takes it as
// the next turn, an edit or a cancel reaches it, and an error clears it with
// the rest.
func (q *PromptQueue) PushFront(text string, now time.Time) QueueEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	p := QueuedPrompt{ID: fmt.Sprintf("q-%d", q.seq), Text: text, QueuedAt: now}
	q.items = append([]QueuedPrompt{p}, q.items...)
	return QueueEvent{Prompt: p, Change: QueueQueued, Pos: 0}
}

// Edit rewrites a row in place: the id and the position are the row's
// identity and do not move, and Version records that it changed.
func (q *PromptQueue) Edit(id, text string) (QueueEvent, error) {
	if len(text) > queueTextCap {
		return QueueEvent{}, ErrQueueTextTooLong
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].ID != id {
			continue
		}
		q.items[i].Text = text
		q.items[i].Version++
		return QueueEvent{Prompt: q.items[i], Change: QueueEdited, Pos: i}, nil
	}
	return QueueEvent{}, fmt.Errorf("agent: no queued message %q", id)
}

// Remove drops a row the user cancelled.
func (q *PromptQueue) Remove(id string) (QueueEvent, bool) {
	return q.take(id, QueueRemoved)
}

// Take drops a row the caller is about to prompt. It is the same transaction
// as Remove with a different name on the event, because what leaves the queue
// and why are two different facts.
func (q *PromptQueue) Take(id string) (QueueEvent, bool) {
	return q.take(id, QueueSent)
}

// Pop is Take of the head, which is the drain. It is one critical section:
// reading the head and removing it separately could report an empty queue
// because the head went, with rows still behind it.
func (q *PromptQueue) Pop() (QueueEvent, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return QueueEvent{}, false
	}
	p := q.items[0]
	q.items = append(q.items[:0], q.items[1:]...)
	return QueueEvent{Prompt: p, Change: QueueSent, Pos: 0}, true
}

func (q *PromptQueue) take(id string, change QueueChange) (QueueEvent, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].ID != id {
			continue
		}
		p := q.items[i]
		q.items = append(q.items[:i], q.items[i+1:]...)
		return QueueEvent{Prompt: p, Change: change, Pos: i}, true
	}
	return QueueEvent{}, false
}

// Clear empties the queue, head first, and returns one removed event per row.
// Every event carries position 0: each row was the head when it was dropped.
func (q *PromptQueue) Clear() []QueueEvent {
	q.mu.Lock()
	items := q.items
	q.items = nil
	q.mu.Unlock()
	out := make([]QueueEvent, 0, len(items))
	for _, p := range items {
		out = append(out, QueueEvent{Prompt: p, Change: QueueRemoved, Pos: 0})
	}
	return out
}

// List is the queue in send order, cloned.
func (q *PromptQueue) List() []QueuedPrompt {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil
	}
	return append([]QueuedPrompt(nil), q.items...)
}

// Len is how many messages are waiting.
func (q *PromptQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Event is the session event for one queue change.
func (e QueueEvent) Event() Event {
	p := e.Prompt
	return Event{Type: EventQueue, Queue: &p, QueueChange: e.Change, QueuePos: e.Pos}
}
