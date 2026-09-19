package store

import (
	"errors"
	"fmt"
	"slices"

	"charm.land/fantasy"
)

// ErrUnpaired is a break of the pairing invariant: AppendStep's refusal of a
// step that breaks it (a bug in the caller, not a condition), and, wrapped
// in ErrCorrupt, Load's error for a file that does.
var ErrUnpaired = errors.New("store: tool calls and results do not pair")

// The pairing invariant. A model that calls tools is sent their results in
// the next message, and a strict provider refuses every later request whose
// history holds a call with no result or a result with no call. So:
//
//   - The tool calls of an assistant message and the tool results of the
//     tool message right after it form an ordered bijection on non-empty,
//     unique ids: the same ids, in the same order.
//   - That tool message is the very next line of the file and is parented
//     on the assistant entry, and no other entry names the assistant entry
//     as its parent, so every path through one goes through the other
//     (ContextAt replays paths, not file order).
//   - A tool message follows nothing else.
//
// It holds per adjacent pair, never across the file: call ids are provider
// text, and a provider that numbers them per response (Fantasy's openai
// provider invents tool-call-<n> when the upstream sends none) repeats them
// from one step to the next. Calls and results the provider executed itself
// (a hosted web search, say) sit together inside the assistant message
// (Fantasy's toResponseMessages) and are exempt. Other tool calls appear
// only in assistant messages and other results only in tool messages, and
// such a result has an output that survives replay (checkOutput).
//
// The store writes a step's assistant and tool messages in one append, and
// checks the invariant on the entries as they will read back, before any
// byte is written; Load checks every line. See Load for the one break a
// crash can cause.

// openCalls is the ids of the calls in m that the next message must answer,
// in order: its tool calls the provider did not execute.
func openCalls(m fantasy.Message) []string {
	var ids []string
	for _, p := range m.Content {
		if c, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok && !c.ProviderExecuted {
			ids = append(ids, c.ToolCallID)
		}
	}
	return ids
}

// resultIDs is the ids of m's results the provider did not execute, in
// order.
func resultIDs(m fantasy.Message) []string {
	var ids []string
	for _, p := range m.Content {
		if r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p); ok && !r.ProviderExecuted {
			ids = append(ids, r.ToolCallID)
		}
	}
	return ids
}

// hasOpenCalls reports whether e is an assistant message whose calls the
// next line must answer.
func hasOpenCalls(e *Entry) bool {
	return e != nil && e.Type == TypeMessage && e.Message.Role == fantasy.MessageRoleAssistant &&
		len(openCalls(e.Message)) > 0
}

func unpaired(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnpaired, fmt.Sprintf(format, args...))
}

// checkParts checks one message on its own: its tool parts are where they
// belong, its calls or its results have non-empty ids that do not repeat
// within it, a tool message holds results and nothing else, and every
// result an OpenAI-compatible provider would replay can be replayed. It
// runs on messages as they read back from JSON, so every part is a value of
// one of Fantasy's own part types.
func checkParts(m fantasy.Message) error {
	seen := map[string]bool{}
	for i, p := range m.Content {
		var id string
		switch p.GetType() {
		case fantasy.ContentTypeToolCall:
			c, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p)
			switch {
			case !ok:
				return fmt.Errorf("part %d is a %T, not a tool call", i, p)
			case m.Role != fantasy.MessageRoleAssistant:
				return unpaired("part %d is a tool call in a %s message", i, m.Role)
			case c.ProviderExecuted:
				continue
			}
			id = c.ToolCallID
		case fantasy.ContentTypeToolResult:
			r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p)
			switch {
			case !ok:
				return fmt.Errorf("part %d is a %T, not a tool result", i, p)
			case r.ProviderExecuted && m.Role != fantasy.MessageRoleAssistant:
				return unpaired("part %d is a provider-executed tool result in a %s message", i, m.Role)
			case r.ProviderExecuted:
				continue
			case m.Role != fantasy.MessageRoleTool:
				return unpaired("part %d is a tool result in a %s message", i, m.Role)
			}
			if err := checkOutput(r.Output); err != nil {
				return fmt.Errorf("part %d (tool call %q): %w", i, r.ToolCallID, err)
			}
			id = r.ToolCallID
		default:
			if m.Role == fantasy.MessageRoleTool {
				return unpaired("part %d is %s content in a tool message", i, p.GetType())
			}
			continue
		}
		switch {
		case id == "":
			return unpaired("part %d has no tool call id", i)
		case seen[id]:
			return unpaired("part %d repeats tool call id %q", i, id)
		}
		seen[id] = true
	}
	if m.Role == fantasy.MessageRoleTool && len(seen) == 0 {
		return unpaired("a tool message with no results")
	}
	return nil
}

// checkOutput refuses a result that could not be replayed. Fantasy writes
// an error result as its text and reads an empty text back as a nil error,
// and the OpenAI-compatible provider replays a result by calling its
// output's GetType and an error's Error method: either nil would panic
// there on every later request.
func checkOutput(o fantasy.ToolResultOutputContent) error {
	if o == nil {
		return errors.New("a tool result with no output")
	}
	if e, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](o); ok &&
		(e.Error == nil || e.Error.Error() == "") {
		return errors.New("an error result with no text")
	}
	return nil
}

// pairing carries the invariant from one line to the next, in file order:
// open is the line before when that line's calls are still to be answered.
type pairing struct {
	open *Entry
}

// pairingAfter is the state after the line e (nil for none).
func pairingAfter(e *Entry) pairing {
	if hasOpenCalls(e) {
		return pairing{open: e}
	}
	return pairing{}
}

// next checks e, the next line in file order, whose parent entry is parent
// (nil for a root), against the line before it, and advances.
func (p *pairing) next(e Entry, parent *Entry) error {
	open := p.open
	*p = pairingAfter(&e)
	if e.Type == TypeMessage {
		if err := checkParts(e.Message); err != nil {
			return fmt.Errorf("entry %q: %w", e.ID, err)
		}
	}
	isTool := e.Type == TypeMessage && e.Message.Role == fantasy.MessageRoleTool
	switch {
	case open != nil && !isTool:
		return unpaired("%s entry %q comes between the tool calls of assistant entry %q and their results",
			e.Type, e.ID, open.ID)
	case open != nil && e.ParentID != open.ID:
		return unpaired("tool entry %q is parented on %q, not on assistant entry %q, whose calls it follows",
			e.ID, e.ParentID, open.ID)
	case open != nil:
		if got, want := resultIDs(e.Message), openCalls(open.Message); !slices.Equal(got, want) {
			return unpaired("tool entry %q answers %q, but assistant entry %q called %q", e.ID, got, open.ID, want)
		}
	case isTool:
		return unpaired("tool entry %q follows no tool calls", e.ID)
	case hasOpenCalls(parent):
		return unpaired("entry %q branches off assistant entry %q between its tool calls and their results",
			e.ID, parent.ID)
	}
	return nil
}
