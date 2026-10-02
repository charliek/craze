package responsesapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// EventKind is what a stream Event reports.
type EventKind int

const (
	// ItemAdded: an output item began (Index, Item as it began: its type,
	// id, and for a function call its call id and name). Every item's first
	// event; one is made up when a delta or the item's end arrives for an
	// index the server never announced.
	ItemAdded EventKind = iota + 1
	// TextDelta: more of a message's text (Index, Delta).
	TextDelta
	// ReasoningDelta: more of a reasoning item's summary (Index, Delta).
	// Summary parts are joined by a blank line (SummarySeparator).
	ReasoningDelta
	// ArgumentsDelta: more of a function call's arguments (Index, Delta),
	// for display; the call's arguments are the ItemDone item's.
	ArgumentsDelta
	// ItemDone: an output item, finished (Index, Item as
	// response.output_item.done carried it). These come in output_index
	// order, whatever order the server finished the items in.
	ItemDone
	// Completed: the response completed (Usage). It is the last event, and
	// the only success.
	Completed
)

// SummarySeparator joins a reasoning item's summary parts in its
// ReasoningDelta stream.
const SummarySeparator = "\n\n"

// Event is one step of a response as the stream reports it.
type Event struct {
	Kind  EventKind
	Index int // the item's output_index
	Item  OutputItem
	Delta string
	Usage Usage
}

// OutputItem is one item of a response's output: a message, a reasoning
// item, a function call, or another type (Type alone), which craze does not
// use.
type OutputItem struct {
	Type string // "message", "reasoning", "function_call", …
	ID   string
	// Text is a message's text: its output_text parts, and any refusal,
	// joined.
	Text string
	// Summary and EncryptedContent are a reasoning item's: the summary parts,
	// and the sealed reasoning — taken from the item's end, never its start,
	// whose blob is incomplete (spike: 1,380 bytes at the start, 1,996 at
	// the end).
	Summary          []string
	EncryptedContent string
	// CallID, Name, Namespace and Arguments are a function call's.
	CallID, Name, Namespace, Arguments string
}

// Usage is what a response reported it used, as it reported it. Input
// includes cached and cache-write tokens, and Output includes reasoning
// tokens.
type Usage struct {
	InputTokens      int64
	CachedTokens     int64
	CacheWriteTokens int64
	OutputTokens     int64
	ReasoningTokens  int64
	TotalTokens      int64
}

// Stream is a response being read. Next moves to the next event; Err is
// nil only once Completed has been read — every other end is an error: an
// *Error for a failure the server reported (with the usage it reported, if
// any), ErrIncomplete for a stream that stopped before saying, or the
// context's error once it is done.
//
// A message's text and a reasoning item's summary stream as deltas, and the
// item at its end is what the response holds: when the end carries text the
// deltas did not, the rest is sent as one more delta before ItemDone, so a
// reader that adds up the deltas holds the item's text, and none of it
// twice. (When the deltas are not a prefix of the item's text, which no
// server should send, the deltas stand: they are what was shown.)
type Stream struct {
	ctx  context.Context
	body io.ReadCloser
	sse  *sseReader

	items map[int]*itemState // every index seen
	held  map[int]OutputItem // finished items waiting for a lower index to finish
	next  int                // the lowest index not yet reported done
	queue []Event            // events made and not yet read
	cur   Event
	err   error
	ended bool
}

type itemState struct {
	typ     string
	id      string
	emitted strings.Builder // the text or summary deltas sent so far
	summary int             // the summary_index the last summary delta was for; -1 before any
	done    bool
}

func newStream(ctx context.Context, resp *http.Response) *Stream {
	return &Stream{
		ctx:   ctx,
		body:  resp.Body,
		sse:   newSSEReader(resp.Body),
		items: map[int]*itemState{},
		held:  map[int]OutputItem{},
	}
}

// Next moves to the next event, reading the stream as it needs to; false
// once the stream has ended (see Err).
func (s *Stream) Next() bool {
	for {
		if len(s.queue) > 0 {
			s.cur, s.queue = s.queue[0], s.queue[1:]
			return true
		}
		if s.ended {
			return false
		}
		s.read()
	}
}

// Event is the event Next moved to.
func (s *Stream) Event() Event { return s.cur }

// Err is why the stream ended: nil after Completed.
func (s *Stream) Err() error { return s.err }

// Close closes the response body. It may be called more than once.
func (s *Stream) Close() error { return s.body.Close() }

// read reads one event from the wire and turns it into events.
func (s *Stream) read() {
	ev, err := s.sse.next()
	if err != nil {
		switch {
		case s.ctx.Err() != nil:
			s.fail(s.ctx.Err())
		case errors.Is(err, io.EOF):
			s.fail(ErrIncomplete)
		default:
			s.fail(fmt.Errorf("responsesapi: reading the stream: %w", err))
		}
		return
	}
	if string(ev.data) == "[DONE]" {
		s.fail(ErrIncomplete)
		return
	}
	var w wireEvent
	if err := json.Unmarshal(ev.data, &w); err != nil {
		s.fail(fmt.Errorf("responsesapi: a stream event is not JSON: %w", err))
		return
	}
	typ := w.Type
	if typ == "" {
		typ = ev.name
	}
	s.handle(typ, &w)
}

func (s *Stream) fail(err error) {
	s.ended, s.err = true, err
}

func (s *Stream) emit(e Event) { s.queue = append(s.queue, e) }

// handle turns one wire event into stream events. Events the stream has no
// use for — the response's created and in-progress snapshots, content parts
// opening and closing, each part's own done event (the item's end carries
// it all) — are passed over, as is any type it does not know.
func (s *Stream) handle(typ string, w *wireEvent) {
	switch typ {
	case "response.output_item.added":
		idx, ok := w.index(s)
		if !ok || w.Item == nil {
			return
		}
		if _, seen := s.items[idx]; seen {
			return
		}
		item := w.Item.output()
		s.items[idx] = &itemState{typ: item.Type, id: item.ID, summary: -1}
		s.emit(Event{Kind: ItemAdded, Index: idx, Item: item})

	case "response.output_text.delta", "response.refusal.delta":
		if st, idx := s.state(w, "message"); st != nil && w.Delta != "" {
			st.emitted.WriteString(w.Delta)
			s.emit(Event{Kind: TextDelta, Index: idx, Delta: w.Delta})
		}

	case "response.reasoning_summary_text.delta":
		st, idx := s.state(w, "reasoning")
		if st == nil || w.Delta == "" {
			return
		}
		if st.summary != w.SummaryIndex {
			if st.summary >= 0 {
				st.emitted.WriteString(SummarySeparator)
				s.emit(Event{Kind: ReasoningDelta, Index: idx, Delta: SummarySeparator})
			}
			st.summary = w.SummaryIndex
		}
		st.emitted.WriteString(w.Delta)
		s.emit(Event{Kind: ReasoningDelta, Index: idx, Delta: w.Delta})

	case "response.function_call_arguments.delta":
		if st, idx := s.state(w, "function_call"); st != nil && w.Delta != "" {
			s.emit(Event{Kind: ArgumentsDelta, Index: idx, Delta: w.Delta})
		}

	case "response.output_item.done":
		idx, ok := w.index(s)
		if !ok || w.Item == nil || idx < s.next {
			return
		}
		item := w.Item.output()
		st := s.items[idx]
		if st == nil {
			st = &itemState{typ: item.Type, id: item.ID, summary: -1}
			s.items[idx] = st
			s.emit(Event{Kind: ItemAdded, Index: idx, Item: item})
		}
		if st.done {
			return
		}
		st.done = true
		switch item.Type {
		case "message":
			s.rest(st, idx, TextDelta, item.Text)
		case "reasoning":
			s.rest(st, idx, ReasoningDelta, strings.Join(item.Summary, SummarySeparator))
		}
		s.held[idx] = item
		s.release(false)

	case "response.completed":
		s.release(true)
		var u Usage
		if w.Response != nil {
			u = w.Response.Usage.usage()
		}
		s.emit(Event{Kind: Completed, Usage: u})
		s.ended = true

	case "response.failed", "response.incomplete":
		e := &Error{Event: typ}
		if r := w.Response; r != nil {
			if r.Error != nil {
				e.setError(r.Error)
			}
			if r.IncompleteDetails != nil {
				e.Reason = ident(r.IncompleteDetails.Reason)
			}
			if r.Usage != nil {
				u := r.Usage.usage()
				e.Usage = &u
			}
		}
		s.fail(e)

	case "error":
		e := &Error{Event: typ}
		if w.Error != nil {
			e.setError(w.Error)
		} else {
			e.setError(&wireError{Code: w.Code, Param: w.Param, Message: w.Message})
		}
		s.fail(e)
	}
}

// state is the item a delta event is for, and its index, made up (with an
// ItemAdded) when the server never announced it; nil when the event names
// no index the stream can place, or an item of another type than typ.
func (s *Stream) state(w *wireEvent, typ string) (*itemState, int) {
	idx, ok := w.index(s)
	if !ok {
		return nil, 0
	}
	st := s.items[idx]
	if st == nil {
		st = &itemState{typ: typ, id: w.ItemID, summary: -1}
		s.items[idx] = st
		s.emit(Event{Kind: ItemAdded, Index: idx, Item: OutputItem{Type: typ, ID: w.ItemID}})
	}
	if st.typ != typ || st.done {
		return nil, 0
	}
	return st, idx
}

// rest sends, as one more delta, the part of an item's final text its
// deltas did not: all of it when none came, nothing when they were all of it
// or were not its prefix.
func (s *Stream) rest(st *itemState, idx int, kind EventKind, final string) {
	sent := st.emitted.String()
	if rest, ok := strings.CutPrefix(final, sent); ok && rest != "" {
		st.emitted.WriteString(rest)
		s.emit(Event{Kind: kind, Index: idx, Delta: rest})
	}
}

// release reports finished items in output_index order: each one whose
// lower indices have all been reported, or, at the response's end (all),
// every one still held, in order — an index no event ever named would
// otherwise hold the rest for good.
func (s *Stream) release(all bool) {
	for {
		item, ok := s.held[s.next]
		if !ok {
			break
		}
		delete(s.held, s.next)
		s.emit(Event{Kind: ItemDone, Index: s.next, Item: item})
		s.next++
	}
	if !all {
		return
	}
	for _, idx := range slices.Sorted(maps.Keys(s.held)) {
		s.emit(Event{Kind: ItemDone, Index: idx, Item: s.held[idx]})
		delete(s.held, idx)
	}
}

// wireEvent is a stream event's data: the fields of every event type craze
// reads, each type using its own.
type wireEvent struct {
	Type         string        `json:"type"`
	OutputIndex  *int          `json:"output_index"`
	ItemID       string        `json:"item_id"`
	SummaryIndex int           `json:"summary_index"`
	Delta        string        `json:"delta"`
	Item         *wireItem     `json:"item"`
	Response     *wireResponse `json:"response"`

	// An error event's own fields, or the error object it nests.
	Code    json.RawMessage `json:"code"`
	Param   json.RawMessage `json:"param"`
	Message string          `json:"message"`
	Error   *wireError      `json:"error"`
}

// index is the event's output_index, or, when it has none, the index of the
// item its item_id names.
func (w *wireEvent) index(s *Stream) (int, bool) {
	if w.OutputIndex != nil {
		return *w.OutputIndex, *w.OutputIndex >= 0
	}
	if w.ItemID != "" {
		for idx, st := range s.items {
			if st.id == w.ItemID {
				return idx, true
			}
		}
	}
	return 0, false
}

type wireItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Content []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
	EncryptedContent string `json:"encrypted_content"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Namespace        string `json:"namespace"`
	Arguments        string `json:"arguments"`
}

func (w *wireItem) output() OutputItem {
	o := OutputItem{
		Type:             w.Type,
		ID:               w.ID,
		EncryptedContent: w.EncryptedContent,
		CallID:           w.CallID,
		Name:             w.Name,
		Namespace:        w.Namespace,
		Arguments:        w.Arguments,
	}
	var text strings.Builder
	for _, c := range w.Content {
		switch c.Type {
		case "output_text":
			text.WriteString(c.Text)
		case "refusal":
			text.WriteString(c.Refusal)
		}
	}
	o.Text = text.String()
	for _, p := range w.Summary {
		o.Summary = append(o.Summary, p.Text)
	}
	return o
}

type wireResponse struct {
	Error             *wireError `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *wireUsage `json:"usage"`
}

type wireUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int64 `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int64 `json:"total_tokens"`
}

func (u *wireUsage) usage() Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		InputTokens:      u.InputTokens,
		CachedTokens:     u.InputTokensDetails.CachedTokens,
		CacheWriteTokens: u.InputTokensDetails.CacheWriteTokens,
		OutputTokens:     u.OutputTokens,
		ReasoningTokens:  u.OutputTokensDetails.ReasoningTokens,
		TotalTokens:      u.TotalTokens,
	}
}
