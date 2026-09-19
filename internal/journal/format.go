package journal

import (
	"bytes"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// FormatVersion is the header's "format". A reader refuses any other value:
// old journals must stay loadable, so a change that an older reader would
// misread bumps it.
const FormatVersion = 1

// Line types, the "type" key every line carries. Everything but an event is
// journal-only: no seq, never delivered to a subscriber, ordered by its
// position in the file.
const (
	typeHeader    = "header"
	typeSession   = "session"
	typeEvent     = "event"
	typeGap       = "gap"
	typePrompt    = "prompt"
	typePromptEnd = "prompt_end"
	typeDiag      = "diag"
)

// MaxSeq stands for "no upper end": the To of an open-ended gap (a failed
// writer lost everything from From on), and the to argument of ReadRange and
// ReadFile meaning "through the last complete record".
const MaxSeq uint64 = math.MaxUint64

// Record is one event line's envelope: the session's sequence number, the
// event's own time and type, and either the lossless codec's JSON object or
// an Omitted marker saying why there is none. The journal never looks inside
// Body beyond checking that it is one JSON object; internal/agent owns the
// codec and converts its own records to and from this type, which keeps this
// package free of every craze import.
type Record struct {
	Seq       uint64    // assigned by the session's event log; starts at 1
	At        time.Time // the event's own time, not the line's
	EventType string    // the event's type name, so a reader can filter without decoding
	// Body is the codec's JSON object, embedded raw as the line's "event".
	// It is a string so nothing can mutate it after Append; the journal
	// keeps a reference, never a copy.
	Body string
	// Omitted is non-nil when the event has no body, and then Body is
	// ignored. An omitted record still takes its seq, so the sequence stays
	// contiguous and ring replay and file replay agree (plan 020 §3.3).
	Omitted *Omitted
}

// Omitted records why an event's body is not in the journal.
type Omitted struct {
	Reason string // OmittedEncodeError or OmittedOversized
	Bytes  int    // the body's size when it was too large; 0 when there was none
	Error  string // what failed, for an encode error
}

// Omitted reasons.
const (
	// OmittedEncodeError: the codec could not encode the event (a
	// programming error), or the journal was handed a body that is not one
	// JSON object.
	OmittedEncodeError = "encode_error"
	// OmittedOversized: the body is larger than MaxRecordBytes, a
	// legitimate large event (plan text is not capped).
	OmittedOversized = "oversized"
)

// maxOmittedError caps an Omitted's Error at acceptance: it is a message
// about a failure, and a pathological one must not be what makes a record
// line large.
const maxOmittedError = 4 << 10

// maxIdentifier caps, in bytes, the strings that name something rather than
// say it: an event's type, an omitted record's reason, a diag's kind and its
// field names. Each is a short word in craze; the cap is what keeps a
// pathological one from being what makes a line too long for the reader.
const maxIdentifier = 256

// eventTypeTooLong is the event type a record is journaled under when its
// own is over maxIdentifier. The record keeps its seq, as an encode_error
// omitted record with no body.
const eventTypeTooLong = "<event type too long>"

// PromptKind is a prompt note's kind: what the user sent.
type PromptKind string

// Prompt kinds.
const (
	PromptKindPrompt    PromptKind = "prompt"    // a turn of its own
	PromptKindInterject PromptKind = "interject" // steering sent into a running turn
)

// Diag kinds S1a writes (plan 020 §3.4). The field names inside each are the
// writer's; the journal does not interpret them (DiagNote says which values
// it keeps).
const (
	DiagStartFailed        = "start_failed"         // Start failed after the log existed
	DiagAgentStderr        = "agent_stderr"         // one line of the agent child's stderr
	DiagAgentStderrDropped = "agent_stderr_dropped" // stderr past the session's budget, as a count
	DiagSubscriberDropped  = "subscriber_dropped"   // a subscription ended as a slow consumer
	DiagRecordOmitted      = "record_omitted"       // an event journaled without its body
	DiagClosing            = "closing"              // the session's log is closing
)

// diagNoteTooLarge replaces a note that stays over MaxRecordBytes even with
// its large field emptied, so no line is ever larger than the cap. Nothing in
// craze writes one in practice; it exists so the bound holds for any input.
const diagNoteTooLarge = "note_too_large"

// Note is a journal-only line: a session id learned, a prompt sent and
// ended, or a diagnostic. It is one of SessionNote, PromptNote,
// PromptEndNote or DiagNote; the interface is sealed so the writer knows
// every shape it can be handed. The writer stamps a note's ts when it
// accepts it.
type Note interface {
	// line is the note's line as a JSON value, ts and type first.
	line(ts string) any
	// size is roughly how many bytes the note holds, for the queue's byte
	// budget.
	size() int
	// cut returns a copy with f applied to its one large field, marked
	// truncated; false when it has no such field left to give.
	cut(f func(string) string) (Note, bool)
}

// SessionNote records the provider's session id once Start learns it.
type SessionNote struct {
	ProviderSessionID string
	LoadedFrom        string // the id resumed from; "" for a fresh session
	// AgentBinary is the binary the spawn resolved, where the header holds
	// the one requested; "" when there is none (the native provider).
	AgentBinary string
}

// PromptNote records a prompt as the user typed it, before any plugin
// expansion (the expansion is an event of its own). Attempt ties it to its
// PromptEndNote.
type PromptNote struct {
	Attempt   string
	Kind      PromptKind
	Text      string
	truncated bool
}

// PromptEndNote records how a prompt attempt ended: a stop reason, or an
// error's class and message.
type PromptEndNote struct {
	Attempt    string
	StopReason string
	ErrClass   string
	ErrMessage string
	Duration   time.Duration
	truncated  bool
}

// DiagNote is a diagnostic that is not transcript. Fields is encoded when
// the note is accepted, so the caller may reuse the map as soon as Note
// returns.
//
// Note runs inside the session's ordering boundary, so encoding a diag must
// never call code the journal does not own, block, or cost more than the
// note cap. Fields' values are therefore restricted to plain JSON values of
// exactly these dynamic types: nil, string, bool, the int, uint and float
// kinds, and []string. Any other value, including a named type over one of
// those (a time.Duration, a json.Number) and anything implementing
// json.Marshaler, fmt.Stringer or error, is written as "<unsupported>"
// without any of its methods being called: convert it first (d.Milliseconds(),
// err.Error()). A non-finite float is written as the string "NaN", "+Inf" or
// "-Inf", which JSON has no number for.
//
// Every string (and each []string element) is cut, between runes, to its
// share of the note cap before anything is encoded: scalars and field names
// are served first, so a long text cannot crowd out a count or a flag, and
// the texts split what is left evenly. A cut marks the line truncated. A
// field name is cut at 256 bytes and a diag's Kind likewise. A diag with
// more than 64 fields keeps only their count, as {"omittedFields": n}.
type DiagNote struct {
	Kind   string
	Fields map[string]any
}

// encodedDiag is a DiagNote after acceptance: its fields frozen as JSON.
type encodedDiag struct {
	kind      string
	fields    json.RawMessage
	truncated bool
}

// emptyFields is the fields value of a diag with none, so every diag line
// has an object there for jq.
var emptyFields = json.RawMessage(`{}`)

// unsupportedField is what a diag field holds in place of a value whose type
// DiagNote does not allow.
const unsupportedField = "<unsupported>"

// maxDiagFields bounds how many fields a diag keeps. A map with more is
// replaced by its size: keeping some would mean choosing which, and ranging
// over all of them would make the work the caller's map's size.
const maxDiagFields = 64

// diagEnvelope is what a diag line needs besides its fields: ts, type, a
// kind at maxIdentifier with every byte escaped, "truncated" and the keys.
// Fields get the note cap less this.
const diagEnvelope = 2 << 10

// Costs a diag field is charged beyond its name's and its text's encoded
// bytes: a name's quotes, colon and comma; a string's quotes; an element's
// quotes and comma; and any scalar, at most (a float64's longest form is 24
// bytes).
const (
	fieldCost   = 4
	stringCost  = 2
	elementCost = 3
	scalarCost  = 32
)

// encodeDiag freezes n. It runs on the caller's goroutine, before the
// queue's lock, so the map is read before Note returns, and its work is
// bounded by maxBytes and maxDiagFields whatever the map holds: values are
// sanitized (diagScalar, fitEncoded) before any encoding, so no method of a
// caller's type runs and no text is encoded past the cap.
func encodeDiag(n DiagNote, maxBytes int) encodedDiag {
	d := encodedDiag{kind: n.Kind, fields: emptyFields}
	if len(d.kind) > maxIdentifier {
		d.kind, d.truncated = strings.Clone(cutUTF8(d.kind, maxIdentifier)), true
	}
	switch {
	case len(n.Fields) == 0:
		return d
	case len(n.Fields) > maxDiagFields:
		d.fields, _ = marshal(map[string]int{"omittedFields": len(n.Fields)})
		d.truncated = true
		return d
	}
	keys := slices.Sorted(maps.Keys(n.Fields))
	fields := make(map[string]any, len(keys))
	budget := maxBytes - diagEnvelope
	// Two passes over at most 64 names: scalars first, then the texts, in
	// name order, each offered an even share of what is left, so a short one
	// leaves more for the next and a long one cannot starve the rest.
	texts := 0
	for _, k := range keys {
		v, text := diagScalar(n.Fields[k])
		if text {
			texts++
			continue
		}
		name, cost := fitEncoded(k, maxIdentifier)
		cost += fieldCost + scalarCost
		if len(name) < len(k) || cost > budget {
			d.truncated = true
		}
		if cost > budget {
			continue
		}
		fields[name] = v
		budget -= cost
	}
	for _, k := range keys {
		var v any
		var used int
		var cut bool
		name, cost := fitEncoded(k, maxIdentifier)
		cost += fieldCost + stringCost
		share := budget / max(texts, 1)
		switch t := n.Fields[k].(type) {
		case string:
			var s string
			s, used = fitEncoded(t, share-cost)
			v, cut = s, len(s) < len(t)
		case []string:
			v, used, cut = fitStrings(t, share-cost)
		default:
			continue
		}
		texts--
		cost += used
		if len(name) < len(k) || cut || cost > budget {
			d.truncated = true
		}
		if cost > budget {
			continue
		}
		fields[name] = v
		budget -= cost
	}
	raw, err := marshal(fields)
	if err != nil {
		// Unreachable: every value is a plain scalar, a string or a
		// []string, and non-finite floats are strings. Kept so the bound
		// holds even if that changes.
		raw, _ = marshal(map[string]string{"encodeError": err.Error()})
	}
	if len(raw) > maxBytes {
		raw, _ = marshal(map[string]int{"omittedBytes": len(raw)})
		d.truncated = true
	}
	d.fields = raw
	return d
}

// diagScalar is v as a diag field may hold it, found by its concrete type
// alone and never by calling a method on it: a caller's type could marshal
// itself slowly, block or panic, and Note runs inside the session's ordering
// boundary. text reports a string or a []string, which encodeDiag cuts to
// its budget itself; anything DiagNote does not allow is the unsupported
// marker.
func diagScalar(v any) (_ any, text bool) {
	switch v := v.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr:
		return v, false
	case float32:
		if f := float64(v); math.IsNaN(f) || math.IsInf(f, 0) {
			return strconv.FormatFloat(f, 'g', -1, 32), false
		}
		return v, false
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return strconv.FormatFloat(v, 'g', -1, 64), false
		}
		return v, false
	case string, []string:
		return v, true
	}
	return unsupportedField, false
}

// fitStrings is the longest prefix of ss whose JSON array body fits budget
// bytes, its last element cut between runes if it does not fit whole (and
// left out if nothing of it fits), and the bytes that body takes. It reads
// no further into ss than the budget reaches. A nil slice stays nil (null),
// as encoding/json would write it.
func fitStrings(ss []string, budget int) (_ []string, used int, cut bool) {
	if ss == nil {
		return nil, len("null") - stringCost, false
	}
	out := []string{}
	for _, s := range ss {
		if used+elementCost > budget {
			return out, used, true
		}
		part, n := fitEncoded(s, budget-used-elementCost)
		if len(part) < len(s) {
			if part != "" {
				out = append(out, part)
				used += elementCost + n
			}
			return out, used, true
		}
		out = append(out, part)
		used += elementCost + n
	}
	return out, used, false
}

// marshal is json.Marshal as the lines are written: without HTML escaping,
// so a "<" in a field reads as itself, as it does everywhere else in a line.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// The lines, one struct each so the key order is fixed: ts, then type, then
// the table's fields in plan 020 §3.4's order. A ts (and an event's at) is
// absent only when the time has no form a reader can parse back (stamp).

type headerLine struct {
	TS           string `json:"ts,omitempty"`
	Type         string `json:"type"`
	Format       int    `json:"format"`
	EventCodec   int    `json:"eventCodec"`
	Incarnation  string `json:"incarnation"`
	CrazeVersion string `json:"crazeVersion"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Provider     string `json:"provider"`
	AgentBinary  string `json:"agentBinary"`
	Cwd          string `json:"cwd"`
	Force        bool   `json:"force"`
	Interactive  bool   `json:"interactive"`
	Mode         string `json:"mode"`
	PID          int    `json:"pid"`
}

type sessionLine struct {
	TS                string `json:"ts,omitempty"`
	Type              string `json:"type"`
	ProviderSessionID string `json:"providerSessionId"`
	LoadedFrom        string `json:"loadedFrom,omitempty"`
	AgentBinary       string `json:"agentBinary,omitempty"`
	Truncated         bool   `json:"truncated,omitempty"`
}

type eventLine struct {
	TS        string          `json:"ts,omitempty"`
	Type      string          `json:"type"`
	Seq       uint64          `json:"seq"`
	At        string          `json:"at,omitempty"`
	EventType string          `json:"eventType"`
	Event     json.RawMessage `json:"event,omitempty"`
	Omitted   *omittedJSON    `json:"omitted,omitempty"`
}

type omittedJSON struct {
	Reason string `json:"reason"`
	Bytes  int    `json:"bytes,omitempty"`
	Error  string `json:"error,omitempty"`
}

// gapLine's fromSeq and toSeq are absent for a note-only gap: seq 0 is
// never assigned, so omitempty is exact.
type gapLine struct {
	TS            string `json:"ts,omitempty"`
	Type          string `json:"type"`
	FromSeq       uint64 `json:"fromSeq,omitempty"`
	ToSeq         uint64 `json:"toSeq,omitempty"`
	DroppedEvents int    `json:"droppedEvents"`
	DroppedNotes  int    `json:"droppedNotes"`
	Error         string `json:"error"`
}

type promptLine struct {
	TS        string     `json:"ts,omitempty"`
	Type      string     `json:"type"`
	Attempt   string     `json:"attempt"`
	Text      string     `json:"text"`
	Kind      PromptKind `json:"kind"`
	Truncated bool       `json:"truncated,omitempty"`
}

type promptEndLine struct {
	TS         string `json:"ts,omitempty"`
	Type       string `json:"type"`
	Attempt    string `json:"attempt"`
	StopReason string `json:"stopReason,omitempty"`
	ErrClass   string `json:"errClass,omitempty"`
	ErrMessage string `json:"errMessage,omitempty"`
	DurationMs int64  `json:"durationMs"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type diagLine struct {
	TS        string          `json:"ts,omitempty"`
	Type      string          `json:"type"`
	Kind      string          `json:"kind"`
	Fields    json.RawMessage `json:"fields"`
	Truncated bool            `json:"truncated,omitempty"`
}

// gapQueueFull is the error a gap line carries: the only way a gap line
// reaches the file is an overflow, since a failed writer writes nothing more.
const gapQueueFull = "journal queue full"

// stamp is how every time in a line is written: RFC3339Nano in UTC. A time
// whose year is outside 0000–9999 has no such form a reader can parse back:
// Format writes "10000-…" or "-0001-…", and time.Time's UnmarshalJSON
// refuses both, which would make the line unreadable. stamp returns "" for
// one, and the line leaves the key out; the reader sees the zero time.
func stamp(t time.Time) string {
	t = t.UTC()
	if y := t.Year(); y < 0 || y > 9999 {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func (n SessionNote) line(ts string) any {
	return sessionLine{TS: ts, Type: typeSession, ProviderSessionID: n.ProviderSessionID,
		LoadedFrom: n.LoadedFrom, AgentBinary: n.AgentBinary}
}

func (n SessionNote) size() int {
	return len(n.ProviderSessionID) + len(n.LoadedFrom) + len(n.AgentBinary)
}

// cut has no large field to give: a session note is ids and a path, so one
// over the cap is replaced by a note_too_large diag.
func (n SessionNote) cut(func(string) string) (Note, bool) { return n, false }

func (n PromptNote) line(ts string) any {
	return promptLine{TS: ts, Type: typePrompt, Attempt: n.Attempt, Text: n.Text, Kind: n.Kind, Truncated: n.truncated}
}

func (n PromptNote) size() int { return len(n.Attempt) + len(n.Kind) + len(n.Text) }

func (n PromptNote) cut(f func(string) string) (Note, bool) {
	if n.Text == "" {
		return n, false
	}
	n.Text = f(n.Text)
	n.truncated = true
	return n, true
}

func (n PromptEndNote) line(ts string) any {
	return promptEndLine{TS: ts, Type: typePromptEnd, Attempt: n.Attempt, StopReason: n.StopReason,
		ErrClass: n.ErrClass, ErrMessage: n.ErrMessage, DurationMs: n.Duration.Milliseconds(), Truncated: n.truncated}
}

func (n PromptEndNote) size() int {
	return len(n.Attempt) + len(n.StopReason) + len(n.ErrClass) + len(n.ErrMessage)
}

func (n PromptEndNote) cut(f func(string) string) (Note, bool) {
	if n.ErrMessage == "" {
		return n, false
	}
	n.ErrMessage = f(n.ErrMessage)
	n.truncated = true
	return n, true
}

// A DiagNote is never queued as itself: Note freezes it into an encodedDiag
// first, which is what the writer encodes and cuts. These methods exist so
// a DiagNote satisfies Note, and give the frozen form's answers at the
// default cap.
func (n DiagNote) line(ts string) any { return encodeDiag(n, DefaultMaxRecordBytes).line(ts) }
func (n DiagNote) size() int          { return encodeDiag(n, DefaultMaxRecordBytes).size() }
func (n DiagNote) cut(f func(string) string) (Note, bool) {
	return encodeDiag(n, DefaultMaxRecordBytes).cut(f)
}

func (d encodedDiag) line(ts string) any {
	return diagLine{TS: ts, Type: typeDiag, Kind: d.kind, Fields: d.fields, Truncated: d.truncated}
}

func (d encodedDiag) size() int { return len(d.kind) + len(d.fields) }

// cut gives up the fields whole rather than apply f: they are one opaque
// object, and cutting JSON would not leave JSON. What remains says how
// large they were.
func (d encodedDiag) cut(func(string) string) (Note, bool) {
	if d.truncated {
		return d, false
	}
	d.fields, _ = marshal(map[string]int{"omittedBytes": len(d.fields)})
	d.truncated = true
	return d, true
}

// cutUTF8 is s's longest prefix of at most n bytes that does not split a
// rune, so a truncated text stays valid UTF-8 (when it was).
func cutUTF8(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutEncoded is s's longest prefix, cut between runes, whose JSON encoding
// is at least by bytes shorter than s's. The writer cuts a note's text this
// way because escaping can make a byte cost six (a control character is
// \u00XX), and cutting by raw bytes would then cut far more than needed.
func cutEncoded(s string, by int) string {
	_, total := fitEncoded(s, math.MaxInt)
	cut, _ := fitEncoded(s, total-by)
	return cut
}

// fitEncoded is s's longest prefix, cut between runes, whose JSON string
// body (quotes excluded, HTML escaping off) is at most budget bytes, and the
// size of that body. It reads no further into s than the budget reaches, so
// a huge s costs no more than one at the budget.
func fitEncoded(s string, budget int) (string, int) {
	used := 0
	for i := 0; i < len(s); {
		e, n := encodedRune(s, i)
		if used+e > budget {
			return s[:i], used
		}
		used += e
		i += n
	}
	return s, used
}

// encodedRune is how many bytes encoding/json (HTML escaping off) writes for
// the rune at s[i], and how many bytes of s that rune is. It mirrors the
// encoder's escapes; the writer re-checks the encoded line all the same.
func encodedRune(s string, i int) (encoded, size int) {
	if c := s[i]; c < utf8.RuneSelf {
		switch {
		case c == '"' || c == '\\' || c == '\n' || c == '\r' || c == '\t' || c == '\b' || c == '\f':
			return 2, 1
		case c < 0x20:
			return 6, 1
		default:
			return 1, 1
		}
	}
	r, n := utf8.DecodeRuneInString(s[i:])
	if r == utf8.RuneError && n == 1 || r == '\u2028' || r == '\u2029' {
		return 6, n // \ufffd, \u2028, \u2029
	}
	return n, n
}

// State is the journal's health at a glance.
type State string

// Journal states.
const (
	// StateOK: everything accepted has been written or is queued to be.
	StateOK State = "ok"
	// StateGap: entries were dropped and the gap line recording them is not
	// yet written. It returns to ok once that line is in the file; Gaps
	// keeps the range for good.
	StateGap State = "gap"
	// StateFailed: a write, sync or creation failed. Nothing more is ever
	// written, and Gaps ends in an open-ended range.
	StateFailed State = "failed"
	// StateOff: there is no journal, which is what a nil *Writer reports.
	StateOff State = "off"
)

// maxHealthGaps bounds Health.Gaps. Past it, the two neighbors with the
// smallest distance between them are merged, which only ever widens what is
// reported missing, and Truncated is set.
const maxHealthGaps = 64

// SeqRange is an inclusive range of sequence numbers. To is MaxSeq for an
// open-ended range.
type SeqRange struct {
	From, To uint64
}

// Health is a copy of the journal's state, readable outside the journal
// (plan 020 §3.4): Subscribe consults Gaps before serving a range from the
// file, and a UI can show State.
type Health struct {
	State State
	// Gaps are the sequence numbers the file does not hold, cumulative and
	// in order: ranges dropped into gap lines, and after a failure one
	// open-ended range from the first seq not written.
	Gaps []SeqRange
	// Truncated is set once Gaps has been merged down to its bound.
	Truncated bool
	// Omitted counts event lines written as omitted records.
	Omitted int
	// DroppedEvents and DroppedNotes count entries accepted by Append or
	// Note and never written: dropped into a gap, left unwritten by a
	// failure, or refused after one or after Close.
	DroppedEvents int
	DroppedNotes  int
	// Err is the failure's message when State is failed.
	Err string
}

// Overlaps reports whether any seq in [from, to] is in a recorded gap.
func (h Health) Overlaps(from, to uint64) bool {
	for _, g := range h.Gaps {
		if g.From <= to && g.To >= from {
			return true
		}
	}
	return false
}

// addGap appends r, which starts after every range already recorded, and
// merges down to the bound.
func (h *Health) addGap(r SeqRange) {
	h.Gaps = append(h.Gaps, r)
	for len(h.Gaps) > maxHealthGaps {
		best := 0
		for i := 1; i+1 < len(h.Gaps); i++ {
			if h.Gaps[i+1].From-h.Gaps[i].To < h.Gaps[best+1].From-h.Gaps[best].To {
				best = i
			}
		}
		h.Gaps[best].To = h.Gaps[best+1].To
		h.Gaps = slices.Delete(h.Gaps, best+1, best+2)
		h.Truncated = true
	}
}

// extendLastGap widens the newest range to reach to: a later event dropped
// into the same gap marker.
func (h *Health) extendLastGap(to uint64) {
	if n := len(h.Gaps); n > 0 {
		h.Gaps[n-1].To = max(h.Gaps[n-1].To, to)
	}
}

// addOpenGap records that nothing from `from` on will be written, absorbing
// any recorded range that touches or follows it.
func (h *Health) addOpenGap(from uint64) {
	for n := len(h.Gaps); n > 0 && h.Gaps[n-1].To >= from-1; n = len(h.Gaps) {
		from = min(from, h.Gaps[n-1].From)
		h.Gaps = h.Gaps[:n-1]
	}
	h.addGap(SeqRange{From: from, To: MaxSeq})
}
