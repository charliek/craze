package harness

import (
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
)

// Replay hands sink the session's stored conversation, as the events a turn
// would have emitted for it (plan 028 §3.4): the transcript's path walked once,
// root to leaf, before the session's first turn. It is how a resumed session
// is shown again; a new one has nothing to replay and sends nothing.
//
//   - A user entry is a Prompted, Steer set for a steer (turnReader): one with
//     no turn, or, in a transcript written before turns were recorded, one
//     after a tool entry or another steer. A results entry — background
//     sub-agents' results, which a child wrote — is nothing: what the model
//     answered to it follows under the prompt before it.
//   - An assistant entry is a ThoughtDelta per reasoning part and a TextDelta
//     per text part, and per tool call a ToolStarted and a ToolCalled, the
//     call described by the dispatcher from its stored name and arguments —
//     title, kind, paths, command — as a live call is, and never run. A call
//     to a tool the session does not have is kind "other", titled by its
//     name. Calls the provider executed are its own, and shown as they are
//     live: not at all.
//   - A tool entry is a ToolFinished per result, converted for display
//     (ToolFinished.Replayed, P9): the stored text as the card's content, an
//     error still an error. An agent call's result is what the model read —
//     a foreground child's final text, or a background call's launch receipt,
//     whose result came in a results entry — and no sub-agent's own events
//     are replayed, as no ACP load rebuilds them.
//   - Changes of model, effort and mode, resume entries, and entries of a
//     type this craze does not know are nothing: the session's state now is
//     what the caller reads from it (Current, Mode).
//   - Last, a Todos with the list the resume restored, when it has any item.
//
// A replayed call's id is "<entry id>.<k>": the id of the assistant entry
// that made it, and k its place among that entry's calls, from 0 — unique in
// the transcript, and never a live id ("t<turn>.<step>.<n>"). Everything text
// in the events — the prompts, the answer, the thinking, the calls and their
// results — is redacted with the session's redactor as it is now (Redact),
// the replay's own form of what a turn's history does (run): a key learned
// since the entry was written is not shown again.
//
// No lock of the harness's is held while sink runs, so it may call the
// session's other methods; it runs on the caller's goroutine, before Replay
// returns. A Run while Replay runs is ErrInTurn, as another Run's would be.
// Replay is ErrReplayed after a first one, ErrInTurn once a turn has begun or
// while one runs, and ErrClosed after Close.
func (s *Session) Replay(sink func(Event)) error {
	if sink == nil {
		sink = func(Event) {}
	}
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return ErrClosed
	case s.replayed:
		s.mu.Unlock()
		return ErrReplayed
	case s.running || s.begun:
		s.mu.Unlock()
		return ErrInTurn
	}
	s.replayed, s.running = true, true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	tr := s.store.Transcript()
	path, err := tr.Branch(tr.Leaf())
	if err != nil { // the leaf is always known
		return fmt.Errorf("harness: %w", err)
	}
	r := replayer{s: s, red: s.redactor(), sink: sink}
	for i := range path {
		r.entry(&path[i])
	}
	if s.tools.todos != nil {
		if items := s.tools.todos.snapshot(); len(items) > 0 {
			sink(Todos{Items: items})
		}
	}
	return nil
}

// replayer is one Replay's walk: the session, the redactor it took at the
// start, and where it is on the path.
type replayer struct {
	s    *Session
	red  *redact.Replacer
	sink func(Event)

	turns turnReader
	step  int      // the assistant entries since the last prompt: the step a call is shown in
	calls []string // the replayed ids of the last assistant entry's calls, which its tool entry answers in order
}

// entry replays one entry of the path.
func (r *replayer) entry(e *store.Entry) {
	steer, results := r.turns.read(e)
	if e.Type != store.TypeMessage {
		// model_change, effort_change, mode_change, resume, and a newer
		// craze's types: nothing. (Plan 028 PR 2's compaction entry is replayed
		// here, as Compacted — C9.)
		return
	}
	switch e.Message.Role {
	case fantasy.MessageRoleUser:
		if results {
			if e.Turn > 0 { // a wake's: the turn is its own
				r.step = 0
			}
			return
		}
		if !steer {
			r.step = 0
		}
		r.sink(Prompted{Text: r.red.String(textOf(e.Message)), Steer: steer})
	case fantasy.MessageRoleAssistant:
		r.step++
		r.assistant(e)
	case fantasy.MessageRoleTool:
		r.results(e)
	}
}

// assistant replays an answer: its thinking and text, and its calls, each
// described and never run.
func (r *replayer) assistant(e *store.Entry) {
	r.calls = r.calls[:0]
	for _, p := range e.Message.Content {
		switch p.GetType() {
		case fantasy.ContentTypeReasoning:
			if rp, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](p); ok && rp.Text != "" {
				r.sink(ThoughtDelta{Text: r.red.String(rp.Text)})
			}
		case fantasy.ContentTypeText:
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok && tp.Text != "" {
				r.sink(TextDelta{Text: r.red.String(tp.Text)})
			}
		case fantasy.ContentTypeToolCall:
			c, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p)
			if !ok || c.ProviderExecuted {
				continue
			}
			id := fmt.Sprintf("%s.%d", e.ID, len(r.calls))
			r.calls = append(r.calls, id)
			req := r.s.replayedCall(id, c, r.red)
			r.sink(ToolStarted{ID: id, Step: r.step, Tool: req.Tool, Kind: req.Kind, ReadOnly: req.ReadOnly})
			r.sink(ToolCalled{ID: id, CallID: r.red.String(c.ToolCallID), Request: req, At: e.Timestamp})
		}
	}
}

// results replays a tool entry: each result, in the order the calls it
// answers were made (the store's pairing), converted for display.
func (r *replayer) results(e *store.Entry) {
	k := 0
	for _, p := range e.Message.Content {
		res, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p)
		if !ok || res.ProviderExecuted {
			continue
		}
		id := fmt.Sprintf("%s.%d", e.ID, k) // unreachable: pairing gives every result its call
		if k < len(r.calls) {
			id = r.calls[k]
		}
		k++
		r.sink(ToolFinished{ID: id, Result: replayedResult(r.red, res.Output), At: e.Timestamp, Replayed: true})
	}
}

// textOf is a message's text parts, joined: a user entry's text, as Run and
// Steer made it (fantasy.NewUserMessage).
func textOf(m fantasy.Message) string {
	var b strings.Builder
	for _, p := range m.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String()
}
