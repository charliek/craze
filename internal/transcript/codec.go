package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// The snapshot codec is a Snapshot as one JSON object (plan 024 §3.5): what S2
// sends a client that attaches over the socket, and what a snapshot's byte
// budget measures. It is lossless the way the event codec is
// (internal/agent/eventcodec.go), and its leaf types — tool, plan and ask
// payloads, todos, roster rows, queue rows, the settings sections, a foreign
// turn — go through the event codec's own exported wrappers over its wire
// twins, so a field added to an agent type reaches both codecs by one line and
// the two cannot drift. Defined, and held by TestSnapshotCodecCarriesEveryField:
//
//   - A nil and an empty slice or map are the same snapshot, and decode yields
//     nil (a model holds its empty lists as nil, X10). A pointer's presence is
//     kept.
//   - A time is RFC 3339 with nanoseconds, in UTC, and decodes as the same
//     instant in UTC. A string is carried as JSON carries it.
//   - An entry's Err is carried as its text: the entry's Text when the fold
//     knew it, else the error's message, beside the event codec's class and
//     code for it (agent.RemoteErrorOf, called here and never under a lock).
//     It decodes as a *agent.RemoteError with that message, class and code,
//     and the entry's Text as that message.
//   - Entry.Bytes is not carried: it is the model's accounting, and Restore
//     recomputes it by the model's own rule.
//   - Version is written as SnapshotVersion whatever the value holds, and a
//     decoder refuses any other. A key it does not know is ignored.
//
// The object is the header — the version, the envelope and the mandatory
// sections with their truncation marks (ItemCap) — then "main" and "subs",
// each transcript an object of its continuation members and its "entries". The transcripts are written member
// by member (appendTranscript: appendScalars, appendToolMember, encodeEntry)
// rather than through a struct, so Snapshot's window can count the length each
// entry and member adds, with the same functions, and hold the encoding to its
// byte budget exactly.

// jsonWriter encodes values as the event codec does — compact, HTML
// unescaped — reusing one buffer. The bytes it returns are its buffer's until
// the next call.
type jsonWriter struct {
	buf bytes.Buffer
	enc *json.Encoder
}

func newJSONWriter() *jsonWriter {
	w := &jsonWriter{}
	w.enc = json.NewEncoder(&w.buf)
	w.enc.SetEscapeHTML(false)
	return w
}

func (w *jsonWriter) marshal(v any) ([]byte, error) {
	w.buf.Reset()
	if err := w.enc.Encode(v); err != nil {
		return nil, err
	}
	out := w.buf.Bytes()
	return out[:len(out)-1], nil // Encode ends the value with a newline
}

// ---------------------------------------------------------------- the wire

type wireHeader struct {
	Version     int               `json:"version"`
	Incarnation string            `json:"incarnation,omitempty"`
	Seq         uint64            `json:"seq,omitempty"`
	Local       uint32            `json:"local,omitempty"`
	FinishSeq   uint64            `json:"finishSeq,omitempty"`
	Agents      []wireAgentRow    `json:"agents,omitempty"`
	Todos       []json.RawMessage `json:"todos,omitempty"`
	TodosCut    bool              `json:"todosTruncated,omitempty"`
	Asks        []wireAsk         `json:"asks,omitempty"`
	Ended       []wireAskEnding   `json:"ended,omitempty"`
	Turn        *wireTurn         `json:"turn,omitempty"`
	Replaying   bool              `json:"replaying,omitempty"`
	Settings    *wireSettings     `json:"settings,omitempty"`
	Queue       []json.RawMessage `json:"queue,omitempty"`
	QueueCut    []string          `json:"truncatedQueue,omitempty"`
}

type wireAgentRow struct {
	Info      json.RawMessage `json:"info"`
	Finish    uint64          `json:"finish,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

// wireAsk is an open ask; its body keeps the shape an ask ending's body has in
// the event codec: each opening payload as its own event carries it.
type wireAsk struct {
	ID        string        `json:"id,omitempty"`
	Kind      agent.AskKind `json:"kind,omitempty"`
	Body      *wireAskBody  `json:"body,omitempty"`
	At        time.Time     `json:"at,omitzero"`
	Truncated bool          `json:"truncated,omitempty"`
}

type wireAskBody struct {
	Permission json.RawMessage `json:"permission,omitempty"`
	Question   json.RawMessage `json:"question,omitempty"`
	Plan       json.RawMessage `json:"plan,omitempty"`
}

type wireAskEnding struct {
	ID      string           `json:"id,omitempty"`
	Kind    agent.AskKind    `json:"kind,omitempty"`
	Outcome agent.AskOutcome `json:"outcome,omitempty"`
	By      string           `json:"by,omitempty"`
	At      time.Time        `json:"at,omitzero"`
}

type wireTurn struct {
	ID        string          `json:"id,omitempty"`
	Text      string          `json:"text,omitempty"`
	Origin    string          `json:"origin,omitempty"`
	At        time.Time       `json:"at,omitzero"`
	Foreign   json.RawMessage `json:"foreign,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

// wireSettings keeps each list section under the event codec's own section
// shape ({"options":[…]} and its siblings); an empty list is absent.
type wireSettings struct {
	Title    string          `json:"title,omitempty"`
	Mode     string          `json:"mode,omitempty"`
	Model    string          `json:"model,omitempty"`
	Config   json.RawMessage `json:"config,omitempty"`
	Commands json.RawMessage `json:"commands,omitempty"`
	Plugins  json.RawMessage `json:"plugins,omitempty"`
	SendNow  json.RawMessage `json:"sendNow,omitempty"`
	// Truncated is Settings.Truncated, absent when no section is marked.
	Truncated *wireSettingsTruncated `json:"truncated,omitempty"`
}

type wireSettingsTruncated struct {
	Title    bool `json:"title,omitempty"`
	Mode     bool `json:"mode,omitempty"`
	Model    bool `json:"model,omitempty"`
	Config   bool `json:"config,omitempty"`
	Commands bool `json:"commands,omitempty"`
	Plugins  bool `json:"plugins,omitempty"`
	SendNow  bool `json:"sendNow,omitempty"`
}

type wireEntry struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind,omitempty"`
	Text      string          `json:"text,omitempty"`
	Tool      json.RawMessage `json:"tool,omitempty"`
	Plan      json.RawMessage `json:"plan,omitempty"`
	Err       *wireEntryErr   `json:"err,omitempty"`
	At        time.Time       `json:"at,omitzero"`
	End       time.Time       `json:"end,omitzero"`
	Open      bool            `json:"open,omitempty"`
	Interject bool            `json:"interject,omitempty"`
	Streaming bool            `json:"streaming,omitempty"`
}

// wireEntryErr is an entry's error beside its text, which is the message: the
// event codec's class and code, both always written.
type wireEntryErr struct {
	Class agent.EventErrClass `json:"class"`
	Code  int                 `json:"code"`
}

// wireSnapshotIn is the whole object as a decoder reads it.
type wireSnapshotIn struct {
	wireHeader
	Main wireTranscriptIn `json:"main"`
	Subs []wireSubIn      `json:"subs"`
}

type wireTranscriptIn struct {
	Trimmed      bool              `json:"trimmed"`
	Windowed     bool              `json:"windowed"`
	Dropped      int               `json:"dropped"`
	StreamOpen   bool              `json:"streamOpen"`
	TailCut      bool              `json:"tailCut"`
	TodoPlanned  int               `json:"todoPlanned"`
	TodoDone     bool              `json:"todoDone"`
	OmittedRun   string            `json:"omittedRun"`
	OmittedTools map[string]string `json:"omittedTools"`
	Entries      []wireEntry       `json:"entries"`
}

type wireSubIn struct {
	ID string `json:"id"`
	wireTranscriptIn
}

// ---------------------------------------------------------------- encoding

// EncodeSnapshot is s as one JSON object. It fails on what JSON cannot carry —
// a time outside the years 0000–9999, an entry kind this build does not know —
// and on a nil snapshot. Its length is what Snapshot's byte budget bounds.
func EncodeSnapshot(s *Snapshot) ([]byte, error) {
	if s == nil {
		return nil, errors.New("transcript: encode snapshot: nil")
	}
	jw := newJSONWriter()
	hdr, err := encodeHeader(jw, s)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(hdr)+4096)
	out = append(out, hdr[:len(hdr)-1]...)
	out = append(out, `,"main":`...)
	if out, err = appendTranscript(jw, out, transcriptScalarsOf("", false, &s.Main), &s.Main); err != nil {
		return nil, err
	}
	if len(s.Subs) > 0 {
		out = append(out, `,"subs":[`...)
		for i := range s.Subs {
			if i > 0 {
				out = append(out, ',')
			}
			sub := &s.Subs[i]
			if out, err = appendTranscript(jw, out, transcriptScalarsOf(sub.ID, true, &sub.TranscriptSnap), &sub.TranscriptSnap); err != nil {
				return nil, err
			}
		}
		out = append(out, ']')
	}
	return append(out, '}'), nil
}

// encodeHeader is the header object: the version, the envelope and the
// mandatory sections. The bytes are the caller's.
func encodeHeader(jw *jsonWriter, s *Snapshot) ([]byte, error) {
	h := wireHeader{
		Version:     SnapshotVersion,
		Incarnation: s.Incarnation,
		Seq:         s.Seq,
		Local:       s.Local,
		FinishSeq:   s.FinishSeq,
		Replaying:   s.Replaying,
		TodosCut:    s.TodosTruncated,
		QueueCut:    s.TruncatedQueue,
	}
	var err error
	fail := func(what string, e error) error {
		return fmt.Errorf("transcript: encode snapshot %s: %w", what, e)
	}
	for _, r := range s.Agents {
		info, e := agent.EncodeSubagentInfo(&r.Info)
		if e != nil {
			return nil, fail("roster", e)
		}
		h.Agents = append(h.Agents, wireAgentRow{Info: info, Finish: r.Finish, Truncated: r.Truncated})
	}
	for _, td := range s.Todos {
		raw, e := agent.EncodeTodo(td)
		if e != nil {
			return nil, fail("todos", e)
		}
		h.Todos = append(h.Todos, raw)
	}
	for i := range s.Asks {
		a := &s.Asks[i]
		wa := wireAsk{ID: a.ID, Kind: a.Kind, At: a.At.UTC(), Truncated: a.Truncated}
		if b := a.Body; b.Permission != nil || b.Question != nil || b.Plan != nil {
			wa.Body = &wireAskBody{}
			if wa.Body.Permission, err = agent.EncodePermissionEvent(b.Permission); err == nil {
				if wa.Body.Question, err = agent.EncodeQuestionEvent(b.Question); err == nil {
					wa.Body.Plan, err = agent.EncodePlanEvent(b.Plan)
				}
			}
			if err != nil {
				return nil, fail("asks", err)
			}
		}
		h.Asks = append(h.Asks, wa)
	}
	for _, e := range s.Ended {
		h.Ended = append(h.Ended, wireAskEnding{ID: e.ID, Kind: e.Kind, Outcome: e.Outcome, By: e.By, At: e.At.UTC()})
	}
	if t := s.Turn; t.ID != "" || t.Text != "" || t.Origin != "" || !t.At.IsZero() || t.Foreign != nil || t.Truncated {
		h.Turn = &wireTurn{ID: t.ID, Text: t.Text, Origin: t.Origin, At: t.At.UTC(), Truncated: t.Truncated}
		if h.Turn.Foreign, err = agent.EncodeForeignTurnInfo(t.Foreign); err != nil {
			return nil, fail("turn", err)
		}
	}
	if h.Settings, err = encodeSettings(&s.Settings); err != nil {
		return nil, fail("settings", err)
	}
	for _, q := range s.Queue {
		raw, e := agent.EncodeQueuedPrompt(q)
		if e != nil {
			return nil, fail("queue", e)
		}
		h.Queue = append(h.Queue, raw)
	}
	b, err := jw.marshal(&h)
	if err != nil {
		return nil, fail("header", err)
	}
	return slices.Clone(b), nil
}

// encodeSettings is the settings object, nil when every section is empty.
func encodeSettings(s *Settings) (*wireSettings, error) {
	w := &wireSettings{Title: s.Title, Mode: s.Mode, Model: s.Model}
	var err error
	if len(s.Config) > 0 {
		if w.Config, err = agent.EncodeConfigState(&agent.ConfigState{Options: s.Config}); err != nil {
			return nil, err
		}
	}
	if len(s.Commands) > 0 {
		if w.Commands, err = agent.EncodeCommandsState(&agent.CommandsState{Commands: s.Commands}); err != nil {
			return nil, err
		}
	}
	if len(s.Plugins) > 0 {
		if w.Plugins, err = agent.EncodePluginsState(&agent.PluginsState{Plugins: s.Plugins}); err != nil {
			return nil, err
		}
	}
	if s.SendNow != (agent.SendNowState{}) {
		if w.SendNow, err = agent.EncodeSendNowState(&s.SendNow); err != nil {
			return nil, err
		}
	}
	if t := s.Truncated; t != (SettingsTruncated{}) {
		w.Truncated = &wireSettingsTruncated{Title: t.Title, Mode: t.Mode, Model: t.Model,
			Config: t.Config, Commands: t.Commands, Plugins: t.Plugins, SendNow: t.SendNow}
	}
	if w.Title == "" && w.Mode == "" && w.Model == "" &&
		w.Config == nil && w.Commands == nil && w.Plugins == nil && w.SendNow == nil && w.Truncated == nil {
		return nil, nil
	}
	return w, nil
}

// encodeEntry is one entry's object, in jw's buffer: valid until jw's next
// use, which is as long as either caller — the encoder appending it, the
// window counting it — needs it.
func encodeEntry(jw *jsonWriter, e *Entry) ([]byte, error) {
	kind, err := kindName(e.Kind)
	if err != nil {
		return nil, err
	}
	w := wireEntry{
		ID: e.ID.String(), Kind: kind, Text: e.Text,
		At: e.At.UTC(), End: e.End.UTC(),
		Open: e.Open, Interject: e.Interject, Streaming: e.Streaming,
	}
	if w.Tool, err = agent.EncodeToolEvent(e.Tool); err != nil {
		return nil, fmt.Errorf("transcript: encode entry %v: %w", e.ID, err)
	}
	if w.Plan, err = agent.EncodePlanEvent(e.Plan); err != nil {
		return nil, fmt.Errorf("transcript: encode entry %v: %w", e.ID, err)
	}
	if e.Err != nil {
		re := agent.RemoteErrorOf(e.Err)
		if w.Text == "" {
			w.Text = re.Message
		}
		w.Err = &wireEntryErr{Class: re.Class, Code: re.Code}
	}
	b, err := jw.marshal(&w)
	if err != nil {
		return nil, fmt.Errorf("transcript: encode entry %v: %w", e.ID, err)
	}
	return b, nil
}

// transcriptScalars is a transcript object's scalar members: its id (a
// child's) and its continuation and window fields.
type transcriptScalars struct {
	id          string
	sub         bool
	trimmed     bool
	windowed    bool
	dropped     int
	streamOpen  bool
	tailCut     bool
	todoPlanned int
	todoDone    bool
	omittedRun  Kind
}

func transcriptScalarsOf(id string, sub bool, ts *TranscriptSnap) transcriptScalars {
	return transcriptScalars{
		id: id, sub: sub,
		trimmed: ts.Trimmed, windowed: ts.Windowed, dropped: ts.Dropped,
		streamOpen: ts.StreamOpen, tailCut: ts.TailCut,
		todoPlanned: ts.TodoPlanned, todoDone: ts.TodoDone,
		omittedRun: ts.OmittedRun,
	}
}

// appendScalars writes the scalar members, each followed by a comma, and
// says how many it wrote. A zero value is absent. It is the one writer of
// these members, for the encoder and for the window that counts them. An
// omittedRun this build cannot name is written as its number, which the
// decoder refuses — an encoding checks it first (appendTranscript).
func appendScalars(b []byte, sc transcriptScalars) ([]byte, int) {
	n := 0
	member := func(key string) {
		b = append(b, '"')
		b = append(b, key...)
		b = append(b, `":`...)
		n++
	}
	if sc.sub {
		member("id")
		b = appendJSONString(b, sc.id)
		b = append(b, ',')
	}
	flag := func(key string, on bool) {
		if on {
			member(key)
			b = append(b, "true,"...)
		}
	}
	number := func(key string, v int) {
		if v != 0 {
			member(key)
			b = strconv.AppendInt(b, int64(v), 10)
			b = append(b, ',')
		}
	}
	flag("trimmed", sc.trimmed)
	flag("windowed", sc.windowed)
	number("dropped", sc.dropped)
	flag("streamOpen", sc.streamOpen)
	flag("tailCut", sc.tailCut)
	number("todoPlanned", sc.todoPlanned)
	flag("todoDone", sc.todoDone)
	if sc.omittedRun != 0 {
		member("omittedRun")
		name, err := kindName(sc.omittedRun)
		if err != nil {
			name = strconv.Itoa(int(sc.omittedRun))
		}
		b = appendJSONString(b, name)
		b = append(b, ',')
	}
	return b, n
}

// appendJSONString writes s as encoding/json writes a string with HTML
// escaping off: as it is, quoted, when it is printable ASCII with no quote or
// backslash — every id craze mints — and through encoding/json itself
// otherwise.
func appendJSONString(b []byte, s string) []byte {
	if plainJSON(s) {
		b = append(b, '"')
		b = append(b, s...)
		return append(b, '"')
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	out := buf.Bytes()
	return append(b, out[:len(out)-1]...)
}

// jsonStringLen is len(appendJSONString(nil, s)).
func jsonStringLen(s string) int {
	if plainJSON(s) {
		return len(s) + 2
	}
	return len(appendJSONString(nil, s))
}

// plainJSON reports whether encoding/json writes s unescaped.
func plainJSON(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// appendTranscript writes one transcript's object: its scalar members, its
// omitted tools (keys sorted, as encoding/json writes a map) and its entries.
func appendTranscript(jw *jsonWriter, b []byte, sc transcriptScalars, ts *TranscriptSnap) ([]byte, error) {
	if _, err := kindName(sc.omittedRun); err != nil {
		return nil, err
	}
	b = append(b, '{')
	b, n := appendScalars(b, sc)
	if len(ts.OmittedTools) > 0 {
		b = append(b, `"omittedTools":{`...)
		ids := make([]string, 0, len(ts.OmittedTools))
		for tid := range ts.OmittedTools {
			ids = append(ids, tid)
		}
		slices.Sort(ids)
		for i, tid := range ids {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendToolMember(b, tid, ts.OmittedTools[tid])
		}
		b = append(b, "},"...)
		n++
	}
	if len(ts.Entries) > 0 {
		b = append(b, `"entries":[`...)
		for i := range ts.Entries {
			if i > 0 {
				b = append(b, ',')
			}
			enc, err := encodeEntry(jw, &ts.Entries[i])
			if err != nil {
				return nil, err
			}
			b = append(b, enc...)
		}
		b = append(b, "],"...)
		n++
	}
	if n == 0 {
		return append(b, '}'), nil
	}
	b[len(b)-1] = '}' // the last member's comma
	return b, nil
}

// appendToolMember writes one OmittedTools member: the tool id and the
// EntryID of the row the window dropped.
func appendToolMember(b []byte, tid string, eid EntryID) []byte {
	b = appendJSONString(b, tid)
	b = append(b, ':')
	return appendJSONString(b, eid.String())
}

// toolMemberLen is len(appendToolMember(nil, tid, eid)).
func toolMemberLen(tid string, eid EntryID) int {
	return jsonStringLen(tid) + 1 + jsonStringLen(eid.String())
}

// kindNames is Kind.String's inverse over the kinds there are.
var kindNames = func() map[string]Kind {
	m := make(map[string]Kind)
	for k := KindUser; k <= KindError; k++ {
		m[k.String()] = k
	}
	return m
}()

// kindName is k on the wire: "" for the zero Kind, its name for a kind there
// is, and an error for any other.
func kindName(k Kind) (string, error) {
	if k == 0 {
		return "", nil
	}
	if _, ok := kindNames[k.String()]; ok {
		return k.String(), nil
	}
	return "", fmt.Errorf("transcript: encode snapshot: entry kind %d is not one this build knows", int(k))
}

// ---------------------------------------------------------------- decoding

// DecodeSnapshot is EncodeSnapshot's inverse, under the equivalence the codec
// defines. Malformed input is an error, never a panic, and so is a version
// other than SnapshotVersion, an entry kind or id this build cannot read, and
// a payload a leaf wrapper refuses. Its peak memory is the decoded snapshot
// beside the input (plan 024 §3.5's fourth limit).
func DecodeSnapshot(data []byte) (*Snapshot, error) {
	var w wireSnapshotIn
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("transcript: decode snapshot: %w", err)
	}
	if w.Version != SnapshotVersion {
		return nil, fmt.Errorf("transcript: decode snapshot: version %d, this build reads %d", w.Version, SnapshotVersion)
	}
	fail := func(what string, err error) (*Snapshot, error) {
		return nil, fmt.Errorf("transcript: decode snapshot %s: %w", what, err)
	}
	s := &Snapshot{
		Version:     SnapshotVersion,
		Incarnation: w.Incarnation,
		Seq:         w.Seq,
		Local:       w.Local,
		FinishSeq:   w.FinishSeq,
		Replaying:   w.Replaying,
	}
	s.TodosTruncated = w.TodosCut
	if len(w.QueueCut) > 0 {
		s.TruncatedQueue = w.QueueCut
	}
	for _, r := range w.Agents {
		info, err := agent.DecodeSubagentInfo(r.Info)
		if err != nil {
			return fail("roster", err)
		}
		row := AgentRow{Finish: r.Finish, Truncated: r.Truncated}
		if info != nil {
			row.Info = *info
		}
		s.Agents = append(s.Agents, row)
	}
	for _, raw := range w.Todos {
		td, err := agent.DecodeTodo(raw)
		if err != nil {
			return fail("todos", err)
		}
		s.Todos = append(s.Todos, td)
	}
	for _, wa := range w.Asks {
		a := Ask{ID: wa.ID, Kind: wa.Kind, At: wa.At.UTC(), Truncated: wa.Truncated}
		if b := wa.Body; b != nil {
			var err error
			if a.Body.Permission, err = agent.DecodePermissionEvent(b.Permission); err == nil {
				if a.Body.Question, err = agent.DecodeQuestionEvent(b.Question); err == nil {
					a.Body.Plan, err = agent.DecodePlanEvent(b.Plan)
				}
			}
			if err != nil {
				return fail("asks", err)
			}
		}
		s.Asks = append(s.Asks, a)
	}
	for _, e := range w.Ended {
		s.Ended = append(s.Ended, AskEnding{ID: e.ID, Kind: e.Kind, Outcome: e.Outcome, By: e.By, At: e.At.UTC()})
	}
	if t := w.Turn; t != nil {
		s.Turn = Turn{ID: t.ID, Text: t.Text, Origin: t.Origin, At: t.At.UTC(), Truncated: t.Truncated}
		var err error
		if s.Turn.Foreign, err = agent.DecodeForeignTurnInfo(t.Foreign); err != nil {
			return fail("turn", err)
		}
	}
	if ws := w.Settings; ws != nil {
		st, err := decodeSettings(ws)
		if err != nil {
			return fail("settings", err)
		}
		s.Settings = st
	}
	for _, raw := range w.Queue {
		q, err := agent.DecodeQueuedPrompt(raw)
		if err != nil {
			return fail("queue", err)
		}
		s.Queue = append(s.Queue, q)
	}
	var err error
	if s.Main, err = w.Main.transcript(); err != nil {
		return fail("main transcript", err)
	}
	for i := range w.Subs {
		ts, err := w.Subs[i].transcript()
		if err != nil {
			return fail(fmt.Sprintf("child %q", w.Subs[i].ID), err)
		}
		s.Subs = append(s.Subs, SubSnap{ID: w.Subs[i].ID, TranscriptSnap: ts})
	}
	return s, nil
}

func decodeSettings(w *wireSettings) (Settings, error) {
	s := Settings{Title: w.Title, Mode: w.Mode, Model: w.Model}
	if t := w.Truncated; t != nil {
		s.Truncated = SettingsTruncated{Title: t.Title, Mode: t.Mode, Model: t.Model,
			Config: t.Config, Commands: t.Commands, Plugins: t.Plugins, SendNow: t.SendNow}
	}
	c, err := agent.DecodeConfigState(w.Config)
	if err != nil {
		return s, err
	}
	if c != nil {
		s.Config = c.Options
	}
	cm, err := agent.DecodeCommandsState(w.Commands)
	if err != nil {
		return s, err
	}
	if cm != nil {
		s.Commands = cm.Commands
	}
	p, err := agent.DecodePluginsState(w.Plugins)
	if err != nil {
		return s, err
	}
	if p != nil {
		s.Plugins = p.Plugins
	}
	sn, err := agent.DecodeSendNowState(w.SendNow)
	if err != nil {
		return s, err
	}
	if sn != nil {
		s.SendNow = *sn
	}
	return s, nil
}

func (w *wireTranscriptIn) transcript() (TranscriptSnap, error) {
	ts := TranscriptSnap{
		Trimmed:     w.Trimmed,
		Windowed:    w.Windowed,
		Dropped:     w.Dropped,
		StreamOpen:  w.StreamOpen,
		TailCut:     w.TailCut,
		TodoPlanned: w.TodoPlanned,
		TodoDone:    w.TodoDone,
	}
	var err error
	if ts.OmittedRun, err = parseKind(w.OmittedRun); err != nil {
		return ts, err
	}
	if len(w.OmittedTools) > 0 {
		ts.OmittedTools = make(map[string]EntryID, len(w.OmittedTools))
		for tid, raw := range w.OmittedTools {
			id, err := parseEntryID(raw)
			if err != nil {
				return ts, err
			}
			ts.OmittedTools[tid] = id
		}
	}
	if len(w.Entries) > 0 {
		ts.Entries = make([]Entry, len(w.Entries))
		for i := range w.Entries {
			if ts.Entries[i], err = w.Entries[i].entry(); err != nil {
				return ts, err
			}
		}
	}
	return ts, nil
}

func (w *wireEntry) entry() (Entry, error) {
	id, err := parseEntryID(w.ID)
	if err != nil {
		return Entry{}, err
	}
	kind, err := parseKind(w.Kind)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{
		ID: id, Kind: kind, Text: w.Text,
		At: w.At.UTC(), End: w.End.UTC(),
		Open: w.Open, Interject: w.Interject, Streaming: w.Streaming,
	}
	if e.Tool, err = agent.DecodeToolEvent(w.Tool); err != nil {
		return Entry{}, err
	}
	if e.Plan, err = agent.DecodePlanEvent(w.Plan); err != nil {
		return Entry{}, err
	}
	if w.Err != nil {
		e.Err = &agent.RemoteError{Message: w.Text, Class: w.Err.Class, Code: w.Err.Code}
	}
	return e, nil
}

// parseKind is kindName's inverse.
func parseKind(s string) (Kind, error) {
	if s == "" {
		return 0, nil
	}
	if k, ok := kindNames[s]; ok {
		return k, nil
	}
	return 0, fmt.Errorf("entry kind %q is not one this build knows", s)
}

// parseEntryID is EntryID.String's inverse: "seq.n".
func parseEntryID(s string) (EntryID, error) {
	seq, n, ok := strings.Cut(s, ".")
	if !ok {
		return EntryID{}, fmt.Errorf("entry id %q is not seq.n", s)
	}
	a, err := strconv.ParseUint(seq, 10, 64)
	if err != nil {
		return EntryID{}, fmt.Errorf("entry id %q: %w", s, err)
	}
	b, err := strconv.ParseUint(n, 10, 32)
	if err != nil {
		return EntryID{}, fmt.Errorf("entry id %q: %w", s, err)
	}
	return EntryID{Seq: a, N: uint32(b)}, nil
}
