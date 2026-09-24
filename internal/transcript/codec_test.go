package transcript

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// The snapshot codec's completeness guard (plan 024 §3.5, A7), in the shape of
// internal/agent/eventcodec_test.go's: a reflection filler sets every field
// reachable from a Snapshot to its own distinct value, the codec must give it
// back under the equivalence codec.go defines, and so must a zero pass (every
// pointer non-nil at its zero value, every collection empty) and a one-hot pass
// per bool. A field added to any type a snapshot reaches is filled and
// compared without anyone touching this file.

var (
	snapTimeType  = reflect.TypeFor[time.Time]()
	snapErrorType = reflect.TypeFor[error]()
	snapKindType  = reflect.TypeFor[Kind]()
	snapEntryType = reflect.TypeFor[Entry]()
)

// snapEpoch is where the filler's times start: in a zone that is not UTC, so
// every time the test round-trips also shows its location replaced.
var snapEpoch = time.Date(2026, 9, 22, 8, 0, 0, 0, time.FixedZone("UTC-7", -7*3600))

// snapFiller sets every field reachable from a value, each from one counter so
// no two hold the same value.
type snapFiller struct {
	t    *testing.T
	n    int
	zero bool
	// boolOn says which bools are true, by their draw; bools lists the draws.
	boolOn func(draw int) bool
	bools  []int
	// plainErrs fills an error with errors.New rather than a RemoteError, so
	// the codec's classification of an error it did not make is exercised.
	plainErrs bool
}

func (f *snapFiller) next() int {
	f.n++
	return f.n
}

func (f *snapFiller) fill(path string, v reflect.Value) {
	f.t.Helper()
	switch v.Type() {
	case snapTimeType:
		if !f.zero {
			n := f.next()
			v.Set(reflect.ValueOf(snapEpoch.Add(time.Duration(n)*time.Second + time.Duration(n))))
		}
		return
	case snapErrorType:
		if f.zero {
			v.Set(reflect.ValueOf(errors.New("")))
			return
		}
		n := f.next()
		if f.plainErrs {
			v.Set(reflect.ValueOf(fmt.Errorf("e%d", n)))
			return
		}
		v.Set(reflect.ValueOf(error(&agent.RemoteError{Message: fmt.Sprintf("e%d", n), Class: agent.EventErrClass(fmt.Sprintf("c%d", n)), Code: n})))
		return
	case snapKindType:
		// A kind is one of the seven there are; the zero pass leaves it zero.
		if !f.zero {
			v.SetInt(int64(KindUser) + int64(f.next()%int(KindError)))
		}
		return
	}
	switch v.Kind() {
	case reflect.String:
		if !f.zero {
			v.SetString(fmt.Sprintf("v%d", f.next()))
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if !f.zero {
			v.SetInt(int64(f.next()))
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if !f.zero {
			v.SetUint(uint64(f.next()))
		}
	case reflect.Bool:
		if !f.zero {
			n := f.next()
			f.bools = append(f.bools, n)
			v.SetBool(f.boolOn(n))
		}
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		f.fill(path, p.Elem())
		v.Set(p)
	case reflect.Slice:
		if f.zero {
			v.Set(reflect.MakeSlice(v.Type(), 0, 0))
			return
		}
		s := reflect.MakeSlice(v.Type(), 2, 2)
		for i := range 2 {
			f.fill(fmt.Sprintf("%s[%d]", path, i), s.Index(i))
		}
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		if !f.zero {
			for range 2 {
				k := reflect.New(v.Type().Key()).Elem()
				f.fill(path+"{key}", k)
				e := reflect.New(v.Type().Elem()).Elem()
				f.fill(fmt.Sprintf("%s[%v]", path, k.Interface()), e)
				m.SetMapIndex(k, e)
			}
		}
		v.Set(m)
	case reflect.Struct:
		typ := v.Type()
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				f.t.Fatalf("%s.%s is unexported: the codec cannot carry it, and the filler cannot set it", path, field.Name)
			}
			f.fill(path+"."+field.Name, v.Field(i))
		}
	default:
		f.t.Fatalf("%s: the filler has no case for %s (%s); teach it one, and the codec to carry it", path, v.Kind(), v.Type())
	}
}

// filledSnapshot is one pass of the filler over a whole Snapshot. The version
// is the one value the filler does not choose: the codec writes its own.
func filledSnapshot(t *testing.T, f *snapFiller) *Snapshot {
	t.Helper()
	s := &Snapshot{}
	f.fill("Snapshot", reflect.ValueOf(s).Elem())
	s.Version = SnapshotVersion
	return s
}

// snapDiff compares a snapshot with what the codec gave back for it, field by
// field, under codec.go's rules: nil and empty collections match and the
// decoded one is nil — but for a slice held as a map value (a question's
// answer list), which keeps nil apart from empty; times match on the instant
// and decode in UTC; an entry's Bytes decodes as zero, and its Err as a
// *agent.RemoteError whose message is the entry's text — its Text, else the
// error's message — with the event codec's class and code, and its Text as
// that message.
func snapDiff(want, got *Snapshot) []string {
	var d []string
	snapCompare("Snapshot", reflect.ValueOf(*want), reflect.ValueOf(*got), &d)
	return d
}

func snapCompare(path string, want, got reflect.Value, d *[]string) {
	diff := func(format string, args ...any) {
		*d = append(*d, path+": "+fmt.Sprintf(format, args...))
	}
	switch want.Type() {
	case snapTimeType:
		w, g := want.Interface().(time.Time), got.Interface().(time.Time)
		if !w.Equal(g) {
			diff("time %v, want %v", g, w)
		}
		if g.Location() != time.UTC {
			diff("decoded time is in %v, want UTC", g.Location())
		}
		return
	case snapEntryType:
		snapCompareEntry(path, want.Interface().(Entry), got.Interface().(Entry), d)
		return
	}
	switch want.Kind() {
	case reflect.Pointer:
		if want.IsNil() != got.IsNil() {
			diff("nil pointer %v, want %v", got.IsNil(), want.IsNil())
			return
		}
		if !want.IsNil() {
			snapCompare(path, want.Elem(), got.Elem(), d)
		}
	case reflect.Slice:
		if want.Len() == 0 {
			if !got.IsNil() {
				diff("an empty or nil collection decoded as a non-nil %v, want nil", got.Interface())
			}
			return
		}
		if want.Len() != got.Len() {
			diff("length %d, want %d", got.Len(), want.Len())
			return
		}
		for i := range want.Len() {
			snapCompare(fmt.Sprintf("%s[%d]", path, i), want.Index(i), got.Index(i), d)
		}
	case reflect.Map:
		if want.Len() == 0 {
			if !got.IsNil() {
				diff("an empty or nil map decoded as a non-nil %v, want nil", got.Interface())
			}
			return
		}
		if want.Len() != got.Len() {
			diff("%d keys, want %d", got.Len(), want.Len())
			return
		}
		keys := want.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		for _, k := range keys {
			w, g := want.MapIndex(k), got.MapIndex(k)
			if !g.IsValid() {
				diff("key %v is missing", k.Interface())
				continue
			}
			if w.Kind() == reflect.Slice && w.Len() == 0 {
				if w.IsNil() != g.IsNil() {
					diff("[%v] nil %v, want %v: a slice held as a map value keeps nil apart from empty", k.Interface(), g.IsNil(), w.IsNil())
				}
				continue
			}
			snapCompare(fmt.Sprintf("%s[%v]", path, k.Interface()), w, g, d)
		}
	case reflect.Struct:
		typ := want.Type()
		for i := range typ.NumField() {
			snapCompare(path+"."+typ.Field(i).Name, want.Field(i), got.Field(i), d)
		}
	case reflect.Interface:
		diff("no comparison for the interface %s; teach snapCompare one", want.Type())
	default:
		if !want.Equal(got) {
			diff("%#v, want %#v", got.Interface(), want.Interface())
		}
	}
}

// snapCompareEntry is the entry rule: Bytes is not carried, and an error is
// carried as its text beside its class and code.
func snapCompareEntry(path string, want, got Entry, d *[]string) {
	text := want.Text
	var wantErr *agent.RemoteError
	if want.Err != nil {
		re := agent.RemoteErrorOf(want.Err)
		if text == "" {
			text = re.Message
		}
		wantErr = &agent.RemoteError{Message: text, Class: re.Class, Code: re.Code}
	}
	if got.Text != text {
		*d = append(*d, fmt.Sprintf("%s.Text: %q, want %q", path, got.Text, text))
	}
	switch {
	case wantErr == nil && got.Err != nil:
		*d = append(*d, fmt.Sprintf("%s.Err: %v, want none", path, got.Err))
	case wantErr != nil:
		if re, ok := got.Err.(*agent.RemoteError); !ok || *re != *wantErr {
			*d = append(*d, fmt.Sprintf("%s.Err: %#v, want %#v", path, got.Err, wantErr))
		}
	}
	if got.Bytes != 0 {
		*d = append(*d, fmt.Sprintf("%s.Bytes: decoded as %d, want 0: the codec does not carry it (Restore recomputes it)", path, got.Bytes))
	}
	typ := snapEntryType
	wv, gv := reflect.ValueOf(want), reflect.ValueOf(got)
	for i := range typ.NumField() {
		switch name := typ.Field(i).Name; name {
		case "Text", "Err", "Bytes": // the rule above
		default:
			snapCompare(path+"."+name, wv.Field(i), gv.Field(i), d)
		}
	}
}

// snapRoundTrip is DecodeSnapshot(EncodeSnapshot(s)), failing on either
// error, and on a difference unless allowed.
func snapRoundTrip(t *testing.T, what string, s *Snapshot) (*Snapshot, []byte) {
	t.Helper()
	b, err := EncodeSnapshot(s)
	if err != nil {
		t.Fatalf("%s: EncodeSnapshot: %v", what, err)
	}
	got, err := DecodeSnapshot(b)
	if err != nil {
		t.Fatalf("%s: DecodeSnapshot(%s): %v", what, b, err)
	}
	if d := snapDiff(s, got); len(d) > 0 {
		t.Fatalf("%s did not survive the codec:\n  %s\nbody: %s", what, strings.Join(d, "\n  "), b)
	}
	return got, b
}

// TestSnapshotCodecCarriesEveryField (A7): every field reachable from a
// Snapshot, each with a distinct value, survives the codec; so does a
// snapshot whose pointers are all non-nil zero values and whose collections
// are all empty; a one-hot pass per bool catches two bools swapped; an error
// the codec did not make is carried as its text and class; and the version is
// written first, whatever the value holds.
func TestSnapshotCodecCarriesEveryField(t *testing.T) {
	all := func(int) bool { return true }
	f := &snapFiller{t: t, boolOn: all}
	s := filledSnapshot(t, f)
	snapRoundTrip(t, "the distinct pass (every bool true)", s)

	if len(f.bools) < 10 {
		t.Fatalf("the filler reached %d bools; it is not walking the snapshot", len(f.bools))
	}
	for _, on := range f.bools {
		one := filledSnapshot(t, &snapFiller{t: t, boolOn: func(n int) bool { return n == on }})
		snapRoundTrip(t, fmt.Sprintf("the one-hot pass for the bool drawn %d", on), one)
	}

	plain := filledSnapshot(t, &snapFiller{t: t, boolOn: all, plainErrs: true})
	snapRoundTrip(t, "the distinct pass with errors the codec did not make", plain)

	zero := filledSnapshot(t, &snapFiller{t: t, zero: true})
	e := &zero.Main.Entries
	*e = append(*e, Entry{Tool: &agent.ToolEvent{}, Plan: &agent.PlanEvent{}, Err: errors.New("")})
	zero.Turn.Foreign = &agent.ForeignTurnInfo{}
	got, _ := snapRoundTrip(t, "the zero pass (non-nil pointers to zero values, empty collections)", zero)
	if len(got.Main.Entries) != 1 {
		t.Fatalf("the zero pass's entry: %+v", got.Main.Entries)
	}
	ze := got.Main.Entries[0]
	if ze.Tool == nil || ze.Plan == nil || ze.Err == nil || got.Turn.Foreign == nil {
		t.Fatalf("a zero-value pointer decoded nil: %+v, foreign %v", ze, got.Turn.Foreign)
	}

	// The version is the first key, and the one the codec writes.
	unversioned := *s
	unversioned.Version = 0
	b, err := EncodeSnapshot(&unversioned)
	if err != nil || !strings.HasPrefix(string(b), fmt.Sprintf(`{"version":%d,`, SnapshotVersion)) {
		t.Fatalf("the version is not written first: %.40s…, %v", b, err)
	}
	if back, err := DecodeSnapshot(b); err != nil || back.Version != SnapshotVersion {
		t.Fatalf("decoded version %v, %v", back, err)
	}
}

// TestSnapshotCodecFillerReachesEveryField pins the filler: a leaf of the
// distinct pass left zero would let a field the codec drops through.
func TestSnapshotCodecFillerReachesEveryField(t *testing.T) {
	s := filledSnapshot(t, &snapFiller{t: t, boolOn: func(int) bool { return true }})
	var zeros []string
	var walk func(path string, v reflect.Value)
	walk = func(path string, v reflect.Value) {
		if v.Type() == snapTimeType || v.Type() == snapErrorType {
			if v.IsZero() {
				zeros = append(zeros, path)
			}
			return
		}
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() {
				zeros = append(zeros, path)
				return
			}
			walk(path, v.Elem())
		case reflect.Slice:
			if v.Len() == 0 {
				zeros = append(zeros, path)
			}
			for i := range v.Len() {
				walk(fmt.Sprintf("%s[%d]", path, i), v.Index(i))
			}
		case reflect.Map:
			if v.Len() == 0 {
				zeros = append(zeros, path)
			}
			for _, k := range v.MapKeys() {
				walk(fmt.Sprintf("%s[%v]", path, k), v.MapIndex(k))
			}
		case reflect.Struct:
			for i := range v.NumField() {
				walk(path+"."+v.Type().Field(i).Name, v.Field(i))
			}
		default:
			if v.IsZero() {
				zeros = append(zeros, path)
			}
		}
	}
	walk("Snapshot", reflect.ValueOf(*s))
	if len(zeros) > 0 {
		t.Fatalf("the filler left these zero: %v", zeros)
	}
}

// TestSnapshotCodecRefusesWhatItCannotRead: malformed JSON, a version this
// build does not read (or none), an entry kind or id it cannot parse, and a
// payload a leaf wrapper refuses are errors, never a panic; an entry kind this
// build does not know cannot be written either.
func TestSnapshotCodecRefusesWhatItCannotRead(t *testing.T) {
	for _, body := range []string{
		``,
		`{`,
		`[]`,
		`{"main":{}}`,
		`{"version":2,"main":{}}`,
		`{"version":1,"main":{"entries":[{"id":"1.0","kind":"hologram"}]}}`,
		`{"version":1,"main":{"entries":[{"id":"one"}]}}`,
		`{"version":1,"main":{"entries":[{"id":"1.x"}]}}`,
		`{"version":1,"main":{"omittedTools":{"t":"7"}}}`,
		`{"version":1,"main":{"omittedRun":"hologram"}}`,
		`{"version":1,"main":{"entries":[{"id":"1.0","kind":"tool","tool":{"id":7}}]}}`,
		`{"version":1,"subs":[{"id":"c","entries":[{"id":"2"}]}],"main":{}}`,
		`{"version":1,"todos":[{"id":1}],"main":{}}`,
		`{"version":1,"agents":[{"info":{"startedAt":"yesterday"}}],"main":{}}`,
	} {
		if s, err := DecodeSnapshot([]byte(body)); err == nil {
			t.Fatalf("DecodeSnapshot(%q) = %+v, want an error", body, s)
		}
	}
	if _, err := EncodeSnapshot(&Snapshot{Version: SnapshotVersion, Main: TranscriptSnap{Entries: []Entry{{ID: EntryID{Seq: 1}, Kind: Kind(99)}}}}); err == nil {
		t.Fatal("an unknown entry kind was written")
	}
	if _, err := EncodeSnapshot(&Snapshot{Version: SnapshotVersion, Main: TranscriptSnap{OmittedRun: Kind(99)}}); err == nil {
		t.Fatal("an unknown omitted run kind was written")
	}
	if _, err := EncodeSnapshot(nil); err == nil {
		t.Fatal("a nil snapshot was written")
	}
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := EncodeSnapshot(&Snapshot{Version: SnapshotVersion, Main: TranscriptSnap{Entries: []Entry{{ID: EntryID{Seq: 1}, At: far}}}}); err == nil {
		t.Fatal("a time JSON cannot carry was written")
	}
	// Keys this build does not know are ignored.
	if s, err := DecodeSnapshot([]byte(`{"version":1,"future":{"x":1},"main":{"future":true}}`)); err != nil || s.Seq != 0 {
		t.Fatalf("unknown keys: %+v, %v", s, err)
	}
}

// goldenTime is a time with nanoseconds, in a zone that is not UTC.
var goldenTime = time.Date(2026, 9, 22, 12, 30, 45, 123456789, time.FixedZone("CEST", 2*3600))

const goldenAt = `"2026-09-22T10:30:45.123456789Z"`

// TestSnapshotCodecPinsTheWireShape (A7) pins one body per section byte for
// byte: the header's envelope and each mandatory section (the roster, the
// todos, the asks and their bodies, the ended list, the turn, the settings,
// the queue) and each truncation mark, then a transcript's continuation
// members, an entry of every kind, and a child's window. Keys are camelCase, "version" comes first, zero
// values are absent, times are UTC with nanoseconds, HTML is not escaped, the
// leaf payloads have exactly the event codec's shape, and omitted tools are
// written in key order. A change here is a change to what S2 sends, and
// SnapshotVersion says whether it is compatible.
func TestSnapshotCodecPinsTheWireShape(t *testing.T) {
	code := 0
	for _, tc := range []struct {
		name string
		s    Snapshot
		want string
	}{
		{"an empty snapshot", Snapshot{},
			`{"version":1,"main":{}}`},
		{"the envelope", Snapshot{Incarnation: "inc-1", Seq: 42, Local: 3, FinishSeq: 7, Replaying: true},
			`{"version":1,"incarnation":"inc-1","seq":42,"local":3,"finishSeq":7,"replaying":true,"main":{}}`},
		{"the roster", Snapshot{Agents: []AgentRow{
			{Info: agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning, Prompt: "look <here> & there", StartedAt: goldenTime}},
			{Info: agent.SubagentInfo{ID: "sub-2", Status: agent.SubagentCompleted, ToolsUsed: []string{"Read"}}, Finish: 2, Truncated: true},
		}},
			`{"version":1,"agents":[{"info":{"id":"sub-1","status":"running","prompt":"look <here> & there","startedAt":` + goldenAt + `}},` +
				`{"info":{"id":"sub-2","status":"completed","toolsUsed":["Read"]},"finish":2,"truncated":true}],"main":{}}`},
		{"the todos", Snapshot{Todos: []agent.Todo{{ID: "1", Content: "write", Status: "completed"}, {ID: "2", Content: "test", Status: "pending"}}},
			`{"version":1,"todos":[{"id":"1","content":"write","status":"completed"},{"id":"2","content":"test","status":"pending"}],"main":{}}`},
		{"the todos, truncated", Snapshot{Todos: []agent.Todo{{ID: "1", Content: "wr"}}, TodosTruncated: true},
			`{"version":1,"todos":[{"id":"1","content":"wr"}],"todosTruncated":true,"main":{}}`},
		{"the asks", Snapshot{Asks: []Ask{
			{ID: "perm-1", Kind: agent.AskPermission, At: goldenTime, Body: agent.AskBody{Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell",
				Options: []agent.PermissionOption{{OptionID: "once", Name: "Allow", Kind: "allow_once"}}}}},
			{ID: "ask-1", Kind: agent.AskQuestion, Body: agent.AskBody{Question: &agent.QuestionEvent{
				ID:        "ask-1",
				Questions: []agent.Question{{ID: "q1", Prompt: "Pick", Options: []agent.Option{{ID: "a", Label: "A", Description: "first"}}}},
				Auto:      true,
				Answers:   map[string][]string{"q1": {"a"}, "q2": nil, "q3": {}},
			}}},
			{ID: "plan-1", Kind: agent.AskPlan, Truncated: true, Body: agent.AskBody{Plan: &agent.PlanEvent{ID: "plan-1", Name: "Refactor", Overview: "o", Plan: "## p"}}},
		}},
			`{"version":1,"asks":[` +
				`{"id":"perm-1","kind":"permission","body":{"permission":{"id":"perm-1","tool":"Shell","options":[{"optionId":"once","name":"Allow","kind":"allow_once"}]}},"at":` + goldenAt + `},` +
				`{"id":"ask-1","kind":"question","body":{"question":{"id":"ask-1","questions":[{"id":"q1","prompt":"Pick","options":[{"id":"a","label":"A","description":"first"}]}],` +
				`"auto":true,"answers":{"q1":["a"],"q2":null,"q3":[]}}}},` +
				`{"id":"plan-1","kind":"plan","body":{"plan":{"id":"plan-1","name":"Refactor","overview":"o","plan":"## p"}},"truncated":true}],"main":{}}`},
		{"the last-ended asks", Snapshot{Ended: []AskEnding{{ID: "ask-0", Kind: agent.AskQuestion, Outcome: agent.AskAnswered, By: agent.AskByClient, At: goldenTime}}},
			`{"version":1,"ended":[{"id":"ask-0","kind":"question","outcome":"answered","by":"client","at":` + goldenAt + `}],"main":{}}`},
		{"the turn", Snapshot{Turn: Turn{ID: "turn-3", Text: "go && see", Origin: agent.TurnOriginSubmit, At: goldenTime,
			Foreign: &agent.ForeignTurnInfo{ID: "f-1", Running: true}, Truncated: true}},
			`{"version":1,"turn":{"id":"turn-3","text":"go && see","origin":"submit","at":` + goldenAt + `,"foreign":{"id":"f-1","running":true},"truncated":true},"main":{}}`},
		{"the settings", Snapshot{Settings: Settings{Title: "fix it", Mode: "agent", Model: "grok-4.6",
			Config: []agent.ConfigOption{{ID: "effort", Name: "Effort", Type: "select", Current: "high",
				SelectValues: []agent.SelectValue{{Value: "high", Name: "high"}}}},
			Commands: []agent.CommandInfo{{Name: "review", Description: "look it over"}},
			Plugins:  []agent.PluginCommand{{Plugin: "p", Bare: "b", Qualified: "p:b", Kind: "skill"}},
			SendNow:  agent.SendNowState{Armed: true, Text: "now", FromRow: "q-2", Turn: "turn-3"}}},
			`{"version":1,"settings":{"title":"fix it","mode":"agent","model":"grok-4.6",` +
				`"config":{"options":[{"id":"effort","name":"Effort","type":"select","current":"high","selectValues":[{"value":"high","name":"high"}]}]},` +
				`"commands":{"commands":[{"name":"review","description":"look it over"}]},` +
				`"plugins":{"plugins":[{"plugin":"p","bare":"b","qualified":"p:b","kind":"skill"}]},` +
				`"sendNow":{"armed":true,"text":"now","fromRow":"q-2","turn":"turn-3"}},"main":{}}`},
		{"the settings' truncation marks", Snapshot{Settings: Settings{Truncated: SettingsTruncated{
			Title: true, Mode: true, Model: true, Config: true, Commands: true, Plugins: true, SendNow: true}}},
			`{"version":1,"settings":{"truncated":{"title":true,"mode":true,"model":true,"config":true,"commands":true,"plugins":true,"sendNow":true}},"main":{}}`},
		{"one section's truncation mark", Snapshot{Settings: Settings{Commands: []agent.CommandInfo{{Name: "review", Description: "lo"}},
			Truncated: SettingsTruncated{Commands: true}}},
			`{"version":1,"settings":{"commands":{"commands":[{"name":"review","description":"lo"}]},"truncated":{"commands":true}},"main":{}}`},
		{"the queue", Snapshot{Queue: []agent.QueuedPrompt{{ID: "q-1", Text: "next", QueuedAt: goldenTime, Version: 2}}},
			`{"version":1,"queue":[{"id":"q-1","text":"next","queuedAt":` + goldenAt + `,"version":2}],"main":{}}`},
		{"the queue, a row truncated", Snapshot{Queue: []agent.QueuedPrompt{{ID: "q-1", Text: "ne"}, {ID: "q-2", Text: "after"}}, TruncatedQueue: []string{"q-1"}},
			`{"version":1,"queue":[{"id":"q-1","text":"ne"},{"id":"q-2","text":"after"}],"truncatedQueue":["q-1"],"main":{}}`},
		{"the main transcript: its continuation and an entry of every kind", Snapshot{Main: TranscriptSnap{
			Trimmed: true, StreamOpen: true, TailCut: true, TodoPlanned: 3, TodoDone: true,
			Entries: []Entry{
				{ID: EntryID{Seq: 4}, Kind: KindUser, Text: "fix <it>", At: goldenTime, End: goldenTime, Interject: true, Bytes: 8},
				{ID: EntryID{Seq: 5}, Kind: KindThought, Text: "hm", At: goldenTime, End: goldenTime.Add(time.Second)},
				{ID: EntryID{Seq: 6}, Kind: KindTool, Tool: &agent.ToolEvent{ID: "call-1", Status: "completed", Output: &agent.ToolOutput{ExitCode: &code}}},
				{ID: EntryID{Seq: 7}, Kind: KindNote, Text: NoteCancelled},
				{ID: EntryID{Seq: 8}, Kind: KindPlan, Plan: &agent.PlanEvent{ID: "plan-1", Name: "P"}},
				{ID: EntryID{Seq: 9}, Kind: KindError, Text: "boom", Err: &agent.RemoteError{Message: "boom", Class: agent.EventErrRPC, Code: -32603}},
				{ID: EntryID{Seq: 9, N: 1}, Kind: KindError, Text: "index write failed"},
				{ID: EntryID{N: 2}, Kind: KindError, Err: errors.New("never read by the fold")},
				{ID: EntryID{Seq: 10}, Kind: KindThought, Text: "…still thinking", At: goldenTime, End: goldenTime, Open: true, Streaming: true},
			},
		}},
			`{"version":1,"main":{"trimmed":true,"streamOpen":true,"tailCut":true,"todoPlanned":3,"todoDone":true,"entries":[` +
				`{"id":"4.0","kind":"user","text":"fix <it>","at":` + goldenAt + `,"end":` + goldenAt + `,"interject":true},` +
				`{"id":"5.0","kind":"thought","text":"hm","at":` + goldenAt + `,"end":"2026-09-22T10:30:46.123456789Z"},` +
				`{"id":"6.0","kind":"tool","tool":{"id":"call-1","status":"completed","output":{"exitCode":0}}},` +
				`{"id":"7.0","kind":"note","text":"cancelled"},` +
				`{"id":"8.0","kind":"plan","plan":{"id":"plan-1","name":"P"}},` +
				`{"id":"9.0","kind":"error","text":"boom","err":{"class":"rpc","code":-32603}},` +
				`{"id":"9.1","kind":"error","text":"index write failed"},` +
				`{"id":"0.2","kind":"error","text":"never read by the fold","err":{"class":"other","code":0}},` +
				`{"id":"10.0","kind":"thought","text":"…still thinking","at":` + goldenAt + `,"end":` + goldenAt + `,"open":true,"streaming":true}]}}`},
		{"a child's window", Snapshot{Subs: []SubSnap{
			{ID: "sub-1", TranscriptSnap: TranscriptSnap{Windowed: true, Dropped: 12, StreamOpen: true, OmittedRun: KindAssistant,
				OmittedTools: map[string]EntryID{"t-b": {Seq: 20}, "t-a": {Seq: 31, N: 1}}}},
			{ID: "sub-2", TranscriptSnap: TranscriptSnap{Entries: []Entry{{ID: EntryID{Seq: 40}, Kind: KindAssistant, Text: "done"}}}},
			{ID: "sub-3"},
		}},
			`{"version":1,"main":{},"subs":[` +
				`{"id":"sub-1","windowed":true,"dropped":12,"streamOpen":true,"omittedRun":"assistant","omittedTools":{"t-a":"31.1","t-b":"20.0"}},` +
				`{"id":"sub-2","entries":[{"id":"40.0","kind":"assistant","text":"done"}]},` +
				`{"id":"sub-3"}]}`},
	} {
		tc.s.Version = SnapshotVersion
		got, err := EncodeSnapshot(&tc.s)
		if err != nil {
			t.Fatalf("%s: EncodeSnapshot: %v", tc.name, err)
		}
		if string(got) != tc.want {
			t.Fatalf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
		back, err := DecodeSnapshot(got)
		if err != nil {
			t.Fatalf("%s: DecodeSnapshot: %v", tc.name, err)
		}
		if d := snapDiff(&tc.s, back); len(d) > 0 {
			t.Fatalf("%s did not survive the codec:\n  %s", tc.name, strings.Join(d, "\n  "))
		}
	}
}
