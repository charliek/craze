package messagesapi

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// EventKind is what a stream Event reports.
type EventKind int

const (
	// BlockStart: a content block began (Index; Block's Type, and a tool
	// use's ID and Name). A block's start may carry content already; it
	// follows as one TextDelta or ThinkingDelta, so a reader that adds up
	// the deltas holds the block's text. A block of a type craze does not
	// read is never reported, nor are its deltas or its stop.
	BlockStart EventKind = iota + 1
	// TextDelta: more of a text block's text (Index, Delta).
	TextDelta
	// ThinkingDelta: more of a thinking block's text (Index, Delta). A
	// signature arrives in pieces too, never reported until BlockStop.
	ThinkingDelta
	// InputDelta: more of a tool use's input JSON (Index, Delta), for
	// display; the call's input is BlockStop's.
	InputDelta
	// BlockStop: a content block, finished (Index, Block whole). Blocks are
	// reported in the order the stream stopped them — its wire order — so a
	// tool call is known at its own stop, before any later block (P1).
	BlockStop
	// MessageDelta: the message's stop reason, stop details and usage so far
	// (message_delta); each field holds its latest value.
	MessageDelta
	// Completed: the message completed (StopReason, StopDetails, Usage). It
	// is the last event, and the only success.
	Completed
)

// Event is one step of a message as the stream reports it.
type Event struct {
	Kind        EventKind
	Index       int // the content block's index
	Block       ContentBlock
	Delta       string
	StopReason  string
	StopDetails StopDetails
	Usage       Usage
}

// ContentBlock is one content block of a response: text, thinking,
// redacted thinking, or a tool use (Type, one of TypeText, TypeThinking,
// TypeRedactedThinking and TypeToolUse).
type ContentBlock struct {
	Type string
	// Text is a text block's text, or a thinking block's thinking, which may
	// be empty for a signed block (display omitted sends exactly that).
	Text string
	// Signature is a thinking block's signature: its signature_delta
	// fragments, joined. Replayed with Text, byte for byte (Thinking).
	Signature string
	// Data is a redacted thinking block's opaque data.
	Data string
	// ID and Name are a tool use's.
	ID, Name string
	// Input is a tool use's input: the input its start carried, when that
	// was an object other than {} (compacted), else its input_json_delta
	// fragments joined, or "{}" when there were none or they were all empty.
	// It is not validated: a normal finish has Fantasy validate and repair
	// it, and an abnormal one runs nothing (P1).
	Input string
}

// StopDetails is message_delta's stop_details: why a refusal refused.
type StopDetails struct {
	// Category is the refusal's category ("cyber", "bio", …), kept only as
	// an identifier; "" when the server named none.
	Category string
}

// UsageFields says which of a Usage's counts the stream reported.
type UsageFields uint8

const (
	HasInput UsageFields = 1 << iota
	HasCacheCreation
	HasCacheRead
	HasOutput
	HasThinking
)

// Usage is what a message reported it used, as it reported it, and which
// counts it reported (Present). InputTokens excludes the cache: the input
// was InputTokens + CacheCreationInputTokens + CacheReadInputTokens.
// OutputTokens includes ThinkingTokens.
//
// message_start sets every count it has; each message_delta overwrites only
// the counts it has, so an output-only delta keeps the input and cache
// counts (P4).
type Usage struct {
	InputTokens              int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	OutputTokens             int64
	ThinkingTokens           int64
	Present                  UsageFields
}

// Has reports whether the stream reported every count f names.
func (u Usage) Has(f UsageFields) bool { return u.Present&f == f }

// InputTransformation is one entry of message_start's
// input_transformations: something the API changed in the request before
// running it — "thinking_dropped" at "messages.1.content.0" for
// "prefix_binding_mismatch" (LF-4, LF-11).
type InputTransformation struct {
	Type, Path, Reason string
}

// Stream is a message being read. Next moves to the next event; Err is nil
// only once Completed has been read — every other end is an error: an
// *Error for an error event, ErrIncomplete (or a violation unwrapping to it)
// for a stream that stopped early or broke the protocol, a read failure, or
// the context's error once it is done.
type Stream struct {
	ctx  context.Context
	body io.ReadCloser
	sse  *sseReader
	sec  *secret
	// reply is what the stream's response headers said, for an error event
	// to carry (replyFacts): its request id, x-should-retry and retry-after.
	reply Error

	started     bool // message_start was read
	id, model   string
	transforms  []InputTransformation
	usage       Usage
	stopReason  string
	stopDetails StopDetails

	blocks map[int]*blockState // every index started
	open   int                 // blocks started and not stopped

	queue []Event // events made and not yet read
	cur   Event
	err   error
	ended bool
}

type blockState struct {
	typ     string // "" for a block of a type craze does not read: skipped
	stopped bool
	text    strings.Builder
	sig     strings.Builder
	input   strings.Builder
	// startInput is a tool use whose start carried its input whole (review
	// c1 F4): a fragment after it would make the joined input no JSON.
	startInput bool
	data       string
	id         string
	name       string
}

// newStream reads resp's body; sec is the key the request carried, taken
// out of every error the stream reports.
func newStream(ctx context.Context, resp *http.Response, sec *secret) *Stream {
	return &Stream{
		ctx:    ctx,
		body:   resp.Body,
		sse:    newSSEReader(resp.Body),
		sec:    sec,
		reply:  replyFacts(resp.Header, sec),
		blocks: map[int]*blockState{},
	}
}

// Next moves to the next event, reading the stream as it needs to; false
// once the stream has ended (see Err).
//
// The context is checked before every event, queued or still to be read,
// Completed included: a reply already buffered whole has nothing left to
// read that a cancel would fail, so it would otherwise run on to Completed
// with Err nil. Once the context is done no further event is reported, and
// Err is the context's error — unless Completed was already read, which
// stands.
func (s *Stream) Next() bool {
	for {
		if s.ended && len(s.queue) == 0 {
			return false
		}
		if err := s.ctx.Err(); err != nil {
			s.queue = nil
			s.fail(err)
			return false
		}
		if len(s.queue) > 0 {
			// Deleted in place, so the queue's array is reused by the next
			// read rather than a new one made for every event.
			s.cur = s.queue[0]
			s.queue = slices.Delete(s.queue, 0, 1)
			return true
		}
		s.read()
	}
}

// Event is the event Next moved to.
func (s *Stream) Event() Event { return s.cur }

// Err is why the stream ended: nil after Completed.
func (s *Stream) Err() error { return s.err }

// Close closes the response body. It may be called more than once. Its
// failure is the transport's, scrubbed as every other (secret.err; review
// c1r2 3).
func (s *Stream) Close() error { return s.sec.err(s.body.Close()) }

// ID is message_start's message id; "" before it.
func (s *Stream) ID() string { return s.id }

// Model is the model message_start says answered; "" before it.
func (s *Stream) Model() string { return s.model }

// InputTransformations are message_start's input_transformations: what the
// API changed in the request — a thinking block dropped for a prefix it was
// not bound to, above all.
func (s *Stream) InputTransformations() []InputTransformation { return s.transforms }

// RequestID is the response's request-id header.
func (s *Stream) RequestID() string { return s.reply.RequestID }

// Usage is the usage reported so far: none (Present 0) before
// message_start. A stream that fails after it still spent it.
func (s *Stream) Usage() Usage { return s.usage }

func (s *Stream) fail(err error) {
	s.ended, s.err = true, err
}

func (s *Stream) emit(e Event) { s.queue = append(s.queue, e) }

// read reads one event from the wire and turns it into events.
//
// The event's type is decoded first, alone: ping, and a type the stream
// does not know, are passed over whatever their other fields hold — the API
// may add events (forward compatibility) — and only the types it reads are
// decoded whole, where an event not of the shape craze reads is a protocol
// violation. The type is the data's own, or the SSE event's name when the
// data names none.
func (s *Stream) read() {
	ev, err := s.sse.next()
	if err != nil {
		switch {
		case s.ctx.Err() != nil:
			s.fail(s.ctx.Err())
		case errors.Is(err, io.EOF):
			s.fail(ErrIncomplete)
		case errors.Is(err, errEventTooLarge):
			s.fail(err) // the reader's own refusal, no text of the server's
		default:
			s.fail(s.sec.err(fmt.Errorf("messagesapi: reading the stream: %w", err)))
		}
		return
	}
	typ, err := typeOf(ev.data)
	if err != nil {
		s.fail(broke("a stream event is not a JSON object"))
		return
	}
	if err := s.handle(cmp.Or(typ, ev.name), ev.data); err != nil {
		s.fail(err)
	}
}

// typeOf is data's type ("" when it names none as a string), decoded alone —
// an event's in read, a block's or a delta's in its UnmarshalJSON — or the
// error for data that is no JSON object: null included, which would
// otherwise decode as an object with nothing in it (review c1 F2).
func typeOf(data []byte) (string, error) {
	if d := bytes.TrimSpace(data); len(d) == 0 || d[0] != '{' {
		return "", errNotObject
	}
	var head struct {
		Type json.RawMessage `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return "", err
	}
	return jsonString(head.Type), nil
}

var errNotObject = errors.New("messagesapi: not a JSON object")

// handle turns one wire event into stream events, or the error that ends
// the stream. A type it does not read — ping among them, wherever it
// arrives — is passed over unread.
func (s *Stream) handle(typ string, data []byte) error {
	var step func([]byte) error
	switch typ {
	case "error":
		var w struct {
			Error *wireError `json:"error"`
		}
		_ = json.Unmarshal(data, &w) // an error event the stream cannot read is an error all the same
		return streamError(w.Error, s.reply, s.sec)
	case "message_start":
		return s.messageStart(data)
	case "content_block_start":
		step = s.blockStart
	case "content_block_delta":
		step = s.blockDelta
	case "content_block_stop":
		step = s.blockStop
	case "message_delta":
		step = s.messageDelta
	case "message_stop":
		step = s.messageStop
	default:
		return nil
	}
	if !s.started {
		return broke("a %s event before message_start", typ)
	}
	return step(data)
}

func (s *Stream) messageStart(data []byte) error {
	if s.started {
		return broke("a second message_start")
	}
	var w struct {
		Message *struct {
			ID                   json.RawMessage           `json:"id"`
			Model                json.RawMessage           `json:"model"`
			Usage                *wireUsage                `json:"usage"`
			InputTransformations []wireInputTransformation `json:"input_transformations"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &w); err != nil || w.Message == nil {
		return broke("a malformed message_start")
	}
	s.started = true
	m := w.Message
	s.id = requestID(jsonString(m.ID))
	s.model = keep(modelIDPattern, jsonString(m.Model))
	s.usage.merge(m.Usage)
	for _, t := range m.InputTransformations {
		s.transforms = append(s.transforms, InputTransformation{
			Type:   ident(jsonString(t.Type)),
			Path:   keep(pathPattern, jsonString(t.Path)),
			Reason: ident(jsonString(t.Reason)),
		})
	}
	return nil
}

func (s *Stream) blockStart(data []byte) error {
	var w struct {
		Index        *int       `json:"index"`
		ContentBlock *wireBlock `json:"content_block"`
	}
	if err := json.Unmarshal(data, &w); err != nil || w.Index == nil || *w.Index < 0 || w.ContentBlock == nil {
		return broke("a malformed content_block_start")
	}
	idx, b := *w.Index, w.ContentBlock
	if _, seen := s.blocks[idx]; seen {
		return broke("a second content_block_start for block %d", idx)
	}
	st := &blockState{}
	s.blocks[idx] = st
	s.open++
	switch b.Type {
	case TypeText:
		st.typ = b.Type
		s.emit(Event{Kind: BlockStart, Index: idx, Block: ContentBlock{Type: b.Type}})
		s.addDelta(idx, &st.text, TextDelta, b.Text)
	case TypeThinking:
		st.typ = b.Type
		st.sig.WriteString(b.Signature)
		s.emit(Event{Kind: BlockStart, Index: idx, Block: ContentBlock{Type: b.Type}})
		s.addDelta(idx, &st.text, ThinkingDelta, b.Thinking)
	case TypeRedactedThinking:
		// The block is its data, replayed as it came: one with none is no
		// block to replay (review c1 F2).
		if b.Data == "" {
			return broke("a redacted_thinking start for block %d with no data", idx)
		}
		st.typ, st.data = b.Type, b.Data
		s.emit(Event{Kind: BlockStart, Index: idx, Block: ContentBlock{Type: b.Type}})
	case TypeToolUse:
		// A call without its identity is no call to run (review c1 F2).
		if b.ID == "" || b.Name == "" {
			return broke("a tool_use start for block %d with no id or no name", idx)
		}
		input, err := startInput(b.Input)
		if err != nil {
			return broke("a tool_use start for block %d whose input is not an object", idx)
		}
		st.typ, st.id, st.name, st.startInput = b.Type, b.ID, b.Name, input != ""
		s.emit(Event{Kind: BlockStart, Index: idx, Block: ContentBlock{Type: b.Type, ID: b.ID, Name: b.Name}})
		s.addDelta(idx, &st.input, InputDelta, input)
	}
	return nil
}

// startInput is the input a tool use's start carried, compacted: "" for the
// placeholder {} the API starts every streamed call with (LF-3), or for no
// input field at all; an error for a value that is no object (review c1 F4)
// — null included, which is a value, not an absent field (review c1r2 5).
func startInput(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", nil
	}
	if raw[0] != '{' {
		return "", errNotObject
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", err
	}
	if buf.String() == "{}" {
		return "", nil
	}
	return buf.String(), nil
}

func (s *Stream) blockDelta(data []byte) error {
	var w struct {
		Index *int       `json:"index"`
		Delta *wireDelta `json:"delta"`
	}
	if err := json.Unmarshal(data, &w); err != nil || w.Index == nil || w.Delta == nil {
		return broke("a malformed content_block_delta")
	}
	idx, d := *w.Index, w.Delta
	st := s.blocks[idx]
	if st == nil || st.stopped {
		return broke("a delta for block %d, which is not open", idx)
	}
	dt, ok := deltaTypes[d.Type]
	if !ok || st.typ == "" {
		return nil // a delta of a type craze does not know, or for a skipped block
	}
	if st.typ != dt.block {
		return broke("a %s for block %d, a %s block", d.Type, idx, st.typ)
	}
	if !d.ok {
		return broke("a %s for block %d whose %s is not a string", d.Type, idx, dt.field)
	}
	switch d.Type {
	case "text_delta":
		s.addDelta(idx, &st.text, TextDelta, d.content)
	case "thinking_delta":
		s.addDelta(idx, &st.text, ThinkingDelta, d.content)
	case "signature_delta":
		st.sig.WriteString(d.content) // never reported until BlockStop
	case "input_json_delta":
		if st.startInput && d.content != "" {
			return broke("input_json_delta for block %d, whose start carried its input", idx)
		}
		s.addDelta(idx, &st.input, InputDelta, d.content)
	}
	return nil
}

// addDelta appends text to a block's content and reports it as a delta of
// kind — a start's initial content and a delta alike, so the deltas add up
// to the block. Empty text is no delta.
func (s *Stream) addDelta(idx int, content *strings.Builder, kind EventKind, text string) {
	content.WriteString(text)
	if text != "" {
		s.emit(Event{Kind: kind, Index: idx, Delta: text})
	}
}

// deltaTypes are the delta types craze reads: the block type each belongs
// to, and the field that carries its content.
var deltaTypes = map[string]struct{ block, field string }{
	"text_delta":       {TypeText, "text"},
	"thinking_delta":   {TypeThinking, "thinking"},
	"signature_delta":  {TypeThinking, "signature"},
	"input_json_delta": {TypeToolUse, "partial_json"},
}

func (s *Stream) blockStop(data []byte) error {
	var w struct {
		Index *int `json:"index"`
	}
	if err := json.Unmarshal(data, &w); err != nil || w.Index == nil {
		return broke("a malformed content_block_stop")
	}
	idx := *w.Index
	st := s.blocks[idx]
	if st == nil || st.stopped {
		return broke("a content_block_stop for block %d, which is not open", idx)
	}
	st.stopped = true
	s.open--
	if st.typ == "" {
		return nil
	}
	s.emit(Event{Kind: BlockStop, Index: idx, Block: st.block()})
	return nil
}

// messageStop is the message's end, and the only success: once no block is
// open and a stop reason has arrived (P4), and only for an object that says
// it is message_stop itself — not an SSE event's name standing in for data
// that names nothing (review c1 F2).
func (s *Stream) messageStop(data []byte) error {
	if typ, err := typeOf(data); err != nil || typ != "message_stop" {
		return broke("a message_stop whose data is not a message_stop object")
	}
	if s.open > 0 {
		return broke("message_stop with a content block still open")
	}
	if s.stopReason == "" {
		return broke("message_stop with no stop reason")
	}
	s.emit(Event{Kind: Completed, StopReason: s.stopReason, StopDetails: s.stopDetails, Usage: s.usage})
	s.ended = true
	return nil
}

// block is the finished block.
func (st *blockState) block() ContentBlock {
	b := ContentBlock{Type: st.typ}
	switch st.typ {
	case TypeText:
		b.Text = st.text.String()
	case TypeThinking:
		b.Text, b.Signature = st.text.String(), st.sig.String()
	case TypeRedactedThinking:
		b.Data = st.data
	case TypeToolUse:
		b.ID, b.Name, b.Input = st.id, st.name, cmp.Or(st.input.String(), "{}")
	}
	return b
}

func (s *Stream) messageDelta(data []byte) error {
	if s.open > 0 {
		return broke("message_delta with a content block still open")
	}
	var w struct {
		Delta *struct {
			StopReason  json.RawMessage `json:"stop_reason"`
			StopDetails json.RawMessage `json:"stop_details"`
		} `json:"delta"`
		Usage *wireUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return broke("a malformed message_delta")
	}
	if d := w.Delta; d != nil {
		// Like the usage, each is overwritten only when this delta has it: a
		// null stop_reason or stop_details keeps the one before.
		if r := ident(jsonString(d.StopReason)); r != "" {
			s.stopReason = r
		}
		var details *struct {
			Category json.RawMessage `json:"category"`
		}
		if json.Unmarshal(d.StopDetails, &details) == nil && details != nil {
			s.stopDetails.Category = ident(jsonString(details.Category))
		}
	}
	s.usage.merge(w.Usage)
	s.emit(Event{Kind: MessageDelta, StopReason: s.stopReason, StopDetails: s.stopDetails, Usage: s.usage})
	return nil
}

// wireBlock is a content_block_start's block. Its other fields are read
// only for the types craze reads: a block of another type — a server tool's
// use or result, a type added later — is its type alone, whatever its other
// fields hold.
type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	Data      string          `json:"data"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

func (w *wireBlock) UnmarshalJSON(data []byte) error {
	typ, err := typeOf(data)
	if err != nil {
		return err
	}
	switch typ {
	case TypeText, TypeThinking, TypeRedactedThinking, TypeToolUse:
		type plain wireBlock
		return json.Unmarshal(data, (*plain)(w))
	}
	*w = wireBlock{Type: typ}
	return nil
}

// wireDelta is a content_block_delta's delta, read the same way: a delta
// of a type craze does not read is its type alone; one craze reads is its
// type and its content, the field deltaTypes names for it. ok says that
// field was there as a JSON string — "" is content, an empty fragment the
// API does send (LF-3); absent, null or another type is not (review c1
// F2), so a mangled event can never pass for an empty one.
type wireDelta struct {
	Type    string
	content string
	ok      bool
}

func (w *wireDelta) UnmarshalJSON(data []byte) error {
	typ, err := typeOf(data)
	if err != nil {
		return err
	}
	*w = wireDelta{Type: typ}
	dt, known := deltaTypes[typ]
	if !known {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var v *string
	if json.Unmarshal(fields[dt.field], &v) == nil && v != nil {
		w.content, w.ok = *v, true
	}
	return nil
}

// wireUsage is a usage object, each count a pointer: nil is a count the
// event did not report (absent, or null).
type wireUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	OutputTokensDetails      *struct {
		ThinkingTokens *int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// merge overwrites u's counts with the ones w reported, and only those.
func (u *Usage) merge(w *wireUsage) {
	if w == nil {
		return
	}
	set := func(dst *int64, src *int64, f UsageFields) {
		if src != nil {
			*dst = *src
			u.Present |= f
		}
	}
	set(&u.InputTokens, w.InputTokens, HasInput)
	set(&u.CacheCreationInputTokens, w.CacheCreationInputTokens, HasCacheCreation)
	set(&u.CacheReadInputTokens, w.CacheReadInputTokens, HasCacheRead)
	set(&u.OutputTokens, w.OutputTokens, HasOutput)
	if d := w.OutputTokensDetails; d != nil {
		set(&u.ThinkingTokens, d.ThinkingTokens, HasThinking)
	}
}

type wireInputTransformation struct {
	Type   json.RawMessage `json:"type"`
	Path   json.RawMessage `json:"path"`
	Reason json.RawMessage `json:"reason"`
}

// pathPattern is what an input transformation's path must look like to be
// kept: a field path, "messages.1.content.0".
var pathPattern = regexp.MustCompile(`^[A-Za-z0-9_.\[\]-]{1,128}$`)
