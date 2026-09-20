package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/harness"
)

// codecExcluded names the fields the codec deliberately does not carry, as
// "DeclaringType.Field", each with its reason. The completeness filler sets
// them like every other field and the comparison requires them to decode as
// zero, so an exclusion is one documented line here rather than a hole in the
// test: a codec that began carrying one fails it.
var codecExcluded = map[string]string{
	"Event.Seq": "the record envelope's (Record.Seq, the journal line's seq), not the codec object's; Record.Event sets it back",
}

// codecExclusion is why typ's field is one of codecExcluded, or "".
func codecExclusion(typ reflect.Type, field string) string {
	return codecExcluded[typ.Name()+"."+field]
}

var (
	codecTimeType  = reflect.TypeFor[time.Time]()
	codecErrorType = reflect.TypeFor[error]()
)

// codecEpoch is where the filler's times start: inside the years JSON can
// write, and in a zone that is not UTC, so every time the test round-trips
// also shows its location being replaced.
var codecEpoch = time.Date(2026, 9, 19, 8, 0, 0, 0, time.FixedZone("UTC-7", -7*3600))

// codecFiller sets every field reachable from an Event, by reflection, so a
// field added to any type the event reaches is filled — and then checked —
// without anyone touching this test. Every value is drawn from one counter,
// so no two fields hold the same value: a mapping that writes one field's
// value into another fails the comparison.
type codecFiller struct {
	t *testing.T
	n int
	// zero is the second pass: scalars left zero, every pointer non-nil and
	// every collection empty but non-nil.
	zero bool
	// boolOn says which bools are true, by the draw each one was given. A
	// counter cannot make two bools distinct, so the distinct pass sets all
	// of them and a one-hot pass per bool then catches two swapped.
	boolOn func(draw int) bool
	// bools is every draw that went to a bool, in order.
	bools []int
}

func (f *codecFiller) next() int {
	f.n++
	return f.n
}

func (f *codecFiller) fill(path string, v reflect.Value) {
	f.t.Helper()
	switch v.Type() {
	case codecTimeType:
		if !f.zero {
			n := f.next()
			v.Set(reflect.ValueOf(codecEpoch.Add(time.Duration(n)*time.Second + time.Duration(n))))
		}
		return
	case codecErrorType:
		// A JSON-RPC error carries both a message and a code, so both are
		// drawn; the zero pass still sets an error, with nothing in it, to
		// show that presence survives an empty message.
		if f.zero {
			v.Set(reflect.ValueOf(errors.New("")))
			return
		}
		n := f.next()
		v.Set(reflect.ValueOf(error(&acp.RPCError{Code: n, Message: fmt.Sprintf("e%d", n)})))
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
		// Two elements, so an order the codec reversed shows too.
		s := reflect.MakeSlice(v.Type(), 2, 2)
		for i := 0; i < s.Len(); i++ {
			f.fill(fmt.Sprintf("%s[%d]", path, i), s.Index(i))
		}
		v.Set(s)
	case reflect.Map:
		if f.zero {
			v.Set(reflect.MakeMap(v.Type()))
			return
		}
		m := reflect.MakeMap(v.Type())
		for i := 0; i < 2; i++ {
			k := reflect.New(v.Type().Key()).Elem()
			f.fill(path+"{key}", k)
			e := reflect.New(v.Type().Elem()).Elem()
			f.fill(fmt.Sprintf("%s[%v]", path, k.Interface()), e)
			m.SetMapIndex(k, e)
		}
		v.Set(m)
	case reflect.Struct:
		typ := v.Type()
		for i := 0; i < typ.NumField(); i++ {
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

// filledEvent is one pass of the filler over a whole Event.
func filledEvent(t *testing.T, zero bool, boolOn func(int) bool) (Event, []int) {
	t.Helper()
	f := &codecFiller{t: t, zero: zero, boolOn: boolOn}
	var ev Event
	f.fill("Event", reflect.ValueOf(&ev).Elem())
	return ev, f.bools
}

// codecDiff compares an event with what the codec gave back for it, field by
// field, by reflection — so a field added to any type the event reaches is
// compared without being listed here. It is strict except where the codec's
// equivalence rules say otherwise (eventcodec.go): nil and empty collections
// match and the decoded one must be nil, except an empty slice held as a map
// value, which must keep nil apart from empty; times match on the instant
// and the decoded one must be UTC; errors match on message, class, code and
// errors.Is. It returns one line per difference.
func codecDiff(want, got Event) []string {
	var d []string
	codecCompare("Event", reflect.ValueOf(want), reflect.ValueOf(got), &d)
	return d
}

func codecCompare(path string, want, got reflect.Value, d *[]string) {
	diff := func(format string, args ...any) {
		*d = append(*d, path+": "+fmt.Sprintf(format, args...))
	}
	switch want.Type() {
	case codecTimeType:
		w, g := want.Interface().(time.Time), got.Interface().(time.Time)
		if !w.Equal(g) {
			diff("time %v, want %v", g, w)
		}
		if g.Location() != time.UTC {
			diff("decoded time is in %v, want UTC", g.Location())
		}
		return
	case codecErrorType:
		codecCompareErr(want, got, diff)
		return
	}
	switch want.Kind() {
	case reflect.Pointer:
		if want.IsNil() != got.IsNil() {
			diff("nil pointer %v, want %v", got.IsNil(), want.IsNil())
			return
		}
		if !want.IsNil() {
			codecCompare(path, want.Elem(), got.Elem(), d)
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
		for i := 0; i < want.Len(); i++ {
			codecCompare(fmt.Sprintf("%s[%d]", path, i), want.Index(i), got.Index(i), d)
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
			// The one exception to the collection rule: an empty slice held
			// as a map value keeps nil apart from empty.
			if w.Kind() == reflect.Slice && w.Len() == 0 {
				if w.IsNil() != g.IsNil() {
					diff("[%v] nil %v, want %v: a slice held as a map value keeps nil apart from empty", k.Interface(), g.IsNil(), w.IsNil())
				}
				continue
			}
			codecCompare(fmt.Sprintf("%s[%v]", path, k.Interface()), w, g, d)
		}
	case reflect.Struct:
		typ := want.Type()
		for i := 0; i < typ.NumField(); i++ {
			name := typ.Field(i).Name
			if why := codecExclusion(typ, name); why != "" {
				if g := got.Field(i); !g.IsZero() {
					*d = append(*d, fmt.Sprintf("%s.%s: decoded as %v, want zero: the codec does not carry it (%s)", path, name, g.Interface(), why))
				}
				continue
			}
			codecCompare(path+"."+name, want.Field(i), got.Field(i), d)
		}
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if !want.Equal(got) {
			diff("%#v, want %#v", got.Interface(), want.Interface())
		}
	default:
		diff("no comparison for %s (%s); teach codecCompare one", want.Kind(), want.Type())
	}
}

// codecCompareErr is the codec's error equivalence. The class and code are
// held to what classifyEventErr says of the original, which only proves they
// crossed the wire intact; TestEventCodecErrorsKeepMessageClassCodeAndIs is
// what pins each class to the real error it stands for.
func codecCompareErr(want, got reflect.Value, diff func(string, ...any)) {
	we, _ := want.Interface().(error)
	ge, _ := got.Interface().(error)
	if (we == nil) != (ge == nil) {
		diff("error %v, want %v", ge, we)
		return
	}
	if we == nil {
		return
	}
	remote, ok := ge.(*RemoteError)
	if !ok {
		diff("decoded error is a %T, want *RemoteError", ge)
		return
	}
	if remote.Error() != we.Error() {
		diff("error message %q, want %q", remote.Error(), we.Error())
	}
	class, code := classifyEventErr(we)
	if remote.Class != class || remote.Code != code {
		diff("error class %q code %d, want %q code %d", remote.Class, remote.Code, class, code)
	}
	for _, c := range eventErrSentinels {
		if w, g := errors.Is(we, c.sentinel), errors.Is(ge, c.sentinel); w != g {
			diff("errors.Is(%v) is %v, want %v", c.sentinel, g, w)
		}
	}
}

// roundTrip is Decode(Encode(ev)), failing the test on either error.
func roundTrip(t *testing.T, ev Event) (Event, string) {
	t.Helper()
	body, err := EncodeEvent(ev)
	if err != nil {
		t.Fatalf("EncodeEvent: %v", err)
	}
	got, err := DecodeEvent(body)
	if err != nil {
		t.Fatalf("DecodeEvent(%s): %v", body, err)
	}
	return got, body
}

// assertRoundTrips is roundTrip, failing the test unless the decoded event is
// equivalent to ev.
func assertRoundTrips(t *testing.T, what string, ev Event) (Event, string) {
	t.Helper()
	got, body := roundTrip(t, ev)
	if d := codecDiff(ev, got); len(d) > 0 {
		t.Fatalf("%s did not survive the codec:\n  %s\nbody: %s", what, strings.Join(d, "\n  "), body)
	}
	return got, body
}

// TestEventCodecCarriesEveryFieldReachableFromEvent is the guard for every
// field any later change adds (plan 020 §3.3): an Event with every reachable
// field set to its own distinct value must come back equivalent, and so must
// one with every pointer set to a zero value and every collection empty. A
// field the wire does not carry, or two fields the mapping swapped, fails it.
func TestEventCodecCarriesEveryFieldReachableFromEvent(t *testing.T) {
	ev, bools := filledEvent(t, false, func(int) bool { return true })
	assertRoundTrips(t, "the distinct pass (every bool true)", ev)

	if len(bools) < 2 {
		t.Fatalf("the filler reached %d bools; it is not walking the event", len(bools))
	}
	for _, on := range bools {
		ev, _ := filledEvent(t, false, func(n int) bool { return n == on })
		assertRoundTrips(t, fmt.Sprintf("the one-hot pass for the bool drawn %d", on), ev)
	}

	zero, _ := filledEvent(t, true, nil)
	// The discriminator is required: an event with no type is not an event.
	zero.Type = EventTool
	if zero.Tool == nil || zero.Tool.Output == nil || zero.Tool.Output.ExitCode == nil || zero.Tool.Task == nil {
		t.Fatalf("the zero pass did not set the pointers it is for: %+v", zero.Tool)
	}
	got, _ := assertRoundTrips(t, "the zero pass (non-nil pointers to zero values, empty collections)", zero)
	if !got.Tool.IsTask() || got.Tool.Output.ExitCode == nil || *got.Tool.Output.ExitCode != 0 {
		t.Fatalf("a bare TaskInfo or an ExitCode of 0 lost its presence: %+v", got.Tool)
	}
}

// TestEventCodecCompletenessFillerReachesEveryField pins the filler itself: a
// pass that left some field zero would let a dropped field through, so every
// leaf of the distinct pass must be set — the documented exclusions too, so
// the comparison's "decodes as zero" says something.
func TestEventCodecCompletenessFillerReachesEveryField(t *testing.T) {
	ev, _ := filledEvent(t, false, func(int) bool { return true })
	var zeros []string
	var walk func(path string, v reflect.Value)
	walk = func(path string, v reflect.Value) {
		if v.Type() == codecTimeType || v.Type() == codecErrorType {
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
			for i := 0; i < v.Len(); i++ {
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
			typ := v.Type()
			for i := 0; i < v.NumField(); i++ {
				walk(path+"."+typ.Field(i).Name, v.Field(i))
			}
		default:
			if v.IsZero() {
				zeros = append(zeros, path)
			}
		}
	}
	walk("Event", reflect.ValueOf(ev))
	if len(zeros) > 0 {
		t.Fatalf("the filler left these zero: %v", zeros)
	}
}

// codecTestTime is a time with nanoseconds, in a zone that is not UTC.
var codecTestTime = time.Date(2026, 9, 19, 12, 30, 45, 123456789, time.FixedZone("CEST", 2*3600))

// TestEventCodecRoundTripsWhatTheEmitSitesBuild covers, per EventType, an
// event shaped the way its emit site builds it (live.go, native.go,
// subagents.go, live_queue.go, queue.go), plus the collection, pointer and
// time corners the equivalence rules are about.
func TestEventCodecRoundTripsWhatTheEmitSitesBuild(t *testing.T) {
	at := codecTestTime
	shell := &ToolEvent{
		ID: "call-7", Name: "Run make test", Title: "Run make test", Kind: "execute", Status: "completed",
		ToolName: "shell", RawInput: "make test && echo <done>", ContentText: "ok\tall passed\n",
		Locations: []string{"/work/Makefile", "/work/internal/agent"},
		Output: &ToolOutput{
			ExitCode: ptr(2), Stdout: "…tail of stdout", Stderr: "FAIL", Content: "c",
			StdoutHead: "=== RUN", StderrHead: "FA", Truncated: true,
		},
		Diffs: []ToolDiff{{Path: "/work/a.go", OldText: "a\n", NewText: "b\nc\n", Added: 2, Removed: 1}},
		At:    at,
	}
	cases := []struct {
		name string
		ev   Event
	}{
		{"a text delta", Event{Type: EventText, Text: "hello, <world> & \"friends\"\n", At: at}},
		{"a sub-agent's thought", Event{Type: EventThought, Agent: "child-1", Text: "thinking…", At: at}},
		{"the user's echo", Event{Type: EventUser, Text: "fix the tests", At: at}},
		{"an interjection", Event{Type: EventUser, Text: "also the docs", Interjection: true, At: at}},
		{"a shell tool with output and a diff", Event{Type: EventTool, Tool: shell, At: at}},
		{"a tool that exited 0", Event{Type: EventTool, Tool: &ToolEvent{ID: "call-8", Status: "completed",
			Output: &ToolOutput{ExitCode: ptr(0)}}, At: at}},
		{"a task tool with a bare TaskInfo", Event{Type: EventTool, Tool: &ToolEvent{ID: "call-9", Title: "Task: explore",
			Task: &TaskInfo{}}, At: at}},
		{"a joined task tool", Event{Type: EventTool, Agent: "child-2", Tool: &ToolEvent{ID: "call-10", Title: "Task: x",
			Task: &TaskInfo{Description: "x", Prompt: "do x", Model: "m", AgentID: "child-2", SubagentType: "explore",
				DurationMs: 1200, Receipt: true, Status: SubagentCompleted, Background: true}}, At: at}},
		{"a tool with nil collections", Event{Type: EventTool, Tool: &ToolEvent{ID: "c", Locations: nil, Diffs: nil}}},
		{"a tool with empty collections", Event{Type: EventTool, Tool: &ToolEvent{ID: "c", Locations: []string{}, Diffs: []ToolDiff{}}}},
		{"a bare tool", Event{Type: EventTool, Tool: &ToolEvent{}}},
		{"todos", Event{Type: EventTodos, Todos: []Todo{
			{ID: "1", Content: "write the codec", Status: "completed"},
			{ID: "2", Content: "test it", Status: "in_progress"},
		}, At: at}},
		{"todos emptied", Event{Type: EventTodos, Todos: []Todo{}, At: at}},
		{"todos nil", Event{Type: EventTodos, At: at}},
		{"a permission", Event{Type: EventPermission, Permission: &PermissionEvent{ID: "perm-1", Tool: "Edit a.go",
			Options: []PermissionOption{
				{OptionID: "allow-once", Name: "Allow", Kind: "allow_once"},
				{OptionID: "reject-once", Name: "Reject", Kind: "reject_once"},
			}}, At: at}},
		{"a question auto-answered, with a nil and an empty answer list", Event{Type: EventQuestion, Question: &QuestionEvent{
			ID: "ask-1", Title: "Question",
			Questions: []Question{
				{ID: "q1", Prompt: "Pick one", Options: []Option{{ID: "opt-a", Label: "A"}, {ID: "opt-b", Label: "B"}}},
				{ID: "q2", Prompt: "Free text", AllowMultiple: true},
			},
			Auto:    true,
			Answers: map[string][]string{"q1": {"opt-a"}, "q2": nil, "q3": {}},
		}, At: at}},
		{"a question waiting on the user", Event{Type: EventQuestion, Question: &QuestionEvent{ID: "ask-2",
			Questions: []Question{{ID: "q1", Prompt: "Why?"}}}, At: at}},
		{"a question with an empty answer map", Event{Type: EventQuestion, Question: &QuestionEvent{ID: "ask-3",
			Answers: map[string][]string{}}}},
		{"a plan accepted headless", Event{Type: EventPlan, Plan: &PlanEvent{ID: "plan-1", Name: "Refactor",
			Overview: "why", Plan: "# Plan\n\n1. do it\n", Todos: []Todo{{ID: "t1", Content: "do it", Status: "pending"}},
			Auto: true, Accepted: true}, At: at}},
		{"done", Event{Type: EventDone, StopReason: "end_turn", At: at}},
		{"a bare meta (re-read the snapshot)", Event{Type: EventMeta, At: at}},
		{"a title", Event{Type: EventMeta, Text: "Session title", At: at}},
		{"a mode change", Event{Type: EventMeta, Mode: "plan", At: at}},
		{"a sub-agent spawned", Event{Type: EventSubagent, SubagentChange: SubagentChangeSpawned, Subagent: &SubagentInfo{
			ID: "child-1", AttemptID: "a1", ParentID: "", ToolCallID: "call-3", Description: "explore",
			SubagentType: "explore", Model: "grok-4", Status: SubagentRunning, Prompt: "look around",
			StartedAt: at, Background: true,
		}, At: at}},
		{"a sub-agent finished", Event{Type: EventSubagent, SubagentChange: SubagentChangeFinished, Subagent: &SubagentInfo{
			ID: "child-1", Status: SubagentFailed, Error: "boom", Output: "partial", Activity: "Read a.go",
			StartedAt: at, EndedAt: at.Add(3 * time.Second), DurationMs: 3000, ToolCalls: 4, Turns: 2,
			TokensUsed: 1234, ToolsUsed: []string{"read", "grep"}, Transcript: true,
		}, At: at}},
		{"a row queued", QueueEvent{Prompt: QueuedPrompt{ID: "q-1", Text: "next", QueuedAt: at}, Change: QueueQueued}.Event()},
		{"a row edited at position 2", QueueEvent{Prompt: QueuedPrompt{ID: "q-3", Text: "edited", QueuedAt: at, Version: 3},
			Change: QueueEdited, Pos: 2}.Event()},
		{"a row sent", QueueEvent{Prompt: QueuedPrompt{ID: "q-1", Text: "next", QueuedAt: at}, Change: QueueSent}.Event()},
		{"a plugin command expanded", Event{Type: EventCommand, Command: &ExpandedCommand{
			PluginCommand: PluginCommand{Plugin: "superpowers", Bare: "simplify", Display: "simplify",
				Qualified: "superpowers:simplify", Description: "Simplify the code", Kind: PluginKindSkill},
			Path: "/home/u/.claude/plugins/x/skills/simplify/SKILL.md",
			Text: "<command-name>simplify</command-name>\nbody",
		}, At: at}},
		{"a foreign turn started", Event{Type: EventForeignTurn, ForeignTurn: &ForeignTurnInfo{ID: "p-9", Text: "hi", Running: true}, At: at}},
		{"a foreign turn ended", Event{Type: EventForeignTurn, ForeignTurn: &ForeignTurnInfo{ID: "p-9"}, At: at}},
		{"the replay's start", Event{Type: EventReplay, Replay: &ReplayInfo{Phase: ReplayStart}, At: at}},
		{"a replayed text", Event{Type: EventText, Text: "from history", Replayed: true, At: at}},
		{"the replay's end", Event{Type: EventReplay, Replay: &ReplayInfo{Phase: ReplayEnd}, At: at}},
		{"an error", Event{Type: EventError, Err: &acp.RPCError{Code: -32603, Message: "Internal error"}, At: at}},
		{"a turn started, with its cause", Event{Type: EventTurn, Cause: "cli-1/7", Turn: &TurnInfo{
			ID: "turn-3", Phase: TurnStarted, Text: "fix the tests", Origin: TurnOriginSubmit,
		}, At: at}},
		{"a turn ended cleanly, with a successor", Event{Type: EventTurn, Cause: "tui-1/12", Turn: &TurnInfo{
			ID: "turn-3", Phase: TurnEnded, StopReason: "end_turn", Next: "turn-4", Pending: 2,
		}, At: at}},
		{"a turn ended synthetically, with no successor", Event{Type: EventTurn, Turn: &TurnInfo{
			ID: "turn-3", Phase: TurnEnded, StopReason: "cancelled", ErrClass: EventErrPromptCancelled,
			Err: "agent: prompt cancelled before it was sent", Synthetic: true,
		}, At: at}},
		{"a send-now armed against a row", Event{Type: EventMeta, Cause: "tui-1/14", State: &StateDelta{
			SendNow: &SendNowState{Armed: true, Text: "this one first", FromRow: "q-4", Turn: "turn-3"},
		}, At: at}},
		{"a send-now disarmed", Event{Type: EventMeta, Cause: "tui-1/14", State: &StateDelta{
			SendNow: &SendNowState{}, Reason: SendNowCancelFailed,
		}, At: at}},
		{"no time at all", Event{Type: EventText, Text: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertRoundTrips(t, tc.name, tc.ev)
		})
	}

	// The comparison above already holds each of these; the subtest says the
	// ones the equivalence rules exist for out loud, by case name.
	t.Run("the specific corners, said out loud", func(t *testing.T) {
		decoded := func(name string) Event {
			t.Helper()
			for _, tc := range cases {
				if tc.name == name {
					got, _ := roundTrip(t, tc.ev)
					return got
				}
			}
			t.Fatalf("no case named %q", name)
			return Event{}
		}
		ans := decoded("a question auto-answered, with a nil and an empty answer list").Question.Answers
		if ans["q2"] != nil || ans["q3"] == nil || len(ans["q3"]) != 0 || len(ans["q1"]) != 1 {
			t.Fatalf("answers %#v: want q2 nil and q3 empty but not nil", ans)
		}
		if code := decoded("a tool that exited 0").Tool.Output.ExitCode; code == nil || *code != 0 {
			t.Fatalf("an ExitCode of 0 came back as %v", code)
		}
		if !decoded("a task tool with a bare TaskInfo").Tool.IsTask() {
			t.Fatal("a bare TaskInfo lost its presence, so IsTask no longer holds")
		}
		if tool := decoded("a tool with empty collections").Tool; tool.Locations != nil || tool.Diffs != nil {
			t.Fatalf("empty collections decoded non-nil: %#v", tool)
		}
		if todos := decoded("todos emptied").Todos; todos != nil {
			t.Fatalf("an emptied todo list decoded as %#v, want nil", todos)
		}
		if at := decoded("no time at all").At; !at.IsZero() {
			t.Fatalf("a zero time came back as %v", at)
		}
		if !decoded("a replayed text").Replayed {
			t.Fatal("Replayed was lost")
		}
		if meta := decoded("a bare meta (re-read the snapshot)"); meta.Type != EventMeta || meta.Text != "" {
			t.Fatalf("a bare meta came back as %+v", meta)
		}
		if at := decoded("a text delta").At; at.Location() != time.UTC || !at.Equal(codecTestTime) {
			t.Fatalf("At came back as %v, want %v in UTC", at, codecTestTime)
		}
	})
}

// TestEventCodecPinsTheWireShape pins a few bodies byte for byte: the keys are
// camelCase, "type" comes first, zero values are absent, a time is UTC with
// nanoseconds, HTML is not escaped, the plugin command is a nested object, and
// an answer list keeps null apart from []. A change here is a change to what
// is on disk, and EventCodecVersion says whether it is compatible.
func TestEventCodecPinsTheWireShape(t *testing.T) {
	for _, tc := range []struct {
		ev   Event
		want string
	}{
		{Event{Type: EventText, Text: "a && b <c>", At: codecTestTime},
			`{"type":"text","text":"a && b <c>","at":"2026-09-19T10:30:45.123456789Z"}`},
		{Event{Type: EventMeta}, `{"type":"meta"}`},
		{Event{Type: EventTool, Tool: &ToolEvent{ID: "call-1", Output: &ToolOutput{ExitCode: ptr(0)}, Task: &TaskInfo{}}},
			`{"type":"tool","tool":{"id":"call-1","output":{"exitCode":0},"task":{}}}`},
		{Event{Type: EventError, Err: &acp.RPCError{Code: -32603, Message: "Internal error"}},
			`{"type":"error","err":{"message":"json-rpc error -32603: Internal error","class":"rpc","code":-32603}}`},
		{Event{Type: EventError, Err: acp.ErrClosed},
			`{"type":"error","err":{"message":"acp: connection closed","class":"closed","code":0}}`},
		{Event{Type: EventCommand, Command: &ExpandedCommand{
			PluginCommand: PluginCommand{Plugin: "p", Bare: "b", Display: "b", Qualified: "p:b", Description: "d", Kind: "skill"},
			Path:          "/x", Text: "t"}},
			`{"type":"command","command":{"pluginCommand":{"plugin":"p","bare":"b","display":"b","qualified":"p:b","description":"d","kind":"skill"},"path":"/x","text":"t"}}`},
		{Event{Type: EventQuestion, Question: &QuestionEvent{ID: "ask-1", Auto: true,
			Answers: map[string][]string{"q1": {"a"}, "q2": nil, "q3": {}}}},
			`{"type":"question","question":{"id":"ask-1","auto":true,"answers":{"q1":["a"],"q2":null,"q3":[]}}}`},
		{QueueEvent{Prompt: QueuedPrompt{ID: "q-1", Text: "next", QueuedAt: codecTestTime, Version: 1}, Change: QueueEdited, Pos: 1}.Event(),
			`{"type":"queue","queue":{"id":"q-1","text":"next","queuedAt":"2026-09-19T10:30:45.123456789Z","version":1},"queueChange":"edited","queuePos":1}`},
		{Event{Type: EventTurn, Cause: "cli-1/7", Turn: &TurnInfo{ID: "turn-3", Phase: TurnStarted, Text: "go", Origin: TurnOriginSubmit}},
			`{"type":"turn","turn":{"id":"turn-3","phase":"started","text":"go","origin":"submit"},"cause":"cli-1/7"}`},
		{Event{Type: EventTurn, Turn: &TurnInfo{ID: "turn-3", Phase: TurnEnded, StopReason: "cancelled", ErrClass: EventErrPromptCancelled, Err: "agent: prompt cancelled before it was sent", Synthetic: true}},
			`{"type":"turn","turn":{"id":"turn-3","phase":"ended","stopReason":"cancelled","errClass":"prompt_cancelled","err":"agent: prompt cancelled before it was sent","synthetic":true}}`},
		{Event{Type: EventMeta, Cause: "c-1/4", State: &StateDelta{SendNow: &SendNowState{Armed: true, Text: "now, please", FromRow: "q-2", Turn: "turn-3"}}},
			`{"type":"meta","state":{"sendNow":{"armed":true,"text":"now, please","fromRow":"q-2","turn":"turn-3"}},"cause":"c-1/4"}`},
		// A cleared section is present and empty, which is how it differs from
		// a section the delta did not touch: {"state":{"reason":…}} with no
		// sendNow key would be indistinguishable from an unrelated delta.
		{Event{Type: EventMeta, State: &StateDelta{SendNow: &SendNowState{}, Reason: SendNowRowGone}},
			`{"type":"meta","state":{"sendNow":{},"reason":"row_gone"}}`},
	} {
		got, err := EncodeEvent(tc.ev)
		if err != nil {
			t.Fatalf("EncodeEvent(%+v): %v", tc.ev, err)
		}
		if got != tc.want {
			t.Fatalf("EncodeEvent(%s event)\n got %s\nwant %s", tc.ev.Type, got, tc.want)
		}
	}
}

// nativeTurnErr runs one native turn and returns the error its EventError
// carried: the chain exactly as the adapter builds it (nativeError around the
// harness's typed error), not a hand-made imitation of it.
func nativeTurnErr(t *testing.T, text string, steps ...step) error {
	t.Helper()
	f := newNativeFixture(t)
	s := f.started(Options{})
	if len(steps) > 0 {
		f.models["test/a"].push(steps...)
	}
	if _, err := s.Prompt(context.Background(), text); err == nil {
		t.Fatal("the turn did not fail")
	}
	err := endings(t, drained(s), "")
	if err == nil {
		t.Fatal("the failed turn's EventError carried no error")
	}
	return err
}

// agentExitStatusErr is the reaper's error for an agent that exited non-zero
// under a turn (acp/spawn.go): a real *exec.ExitError, wrapped the same way.
func agentExitStatusErr(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 3").Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("sh -c 'exit 3' = %v, want an *exec.ExitError", err)
	}
	return fmt.Errorf("acp: agent exited: %w", err)
}

// callerEndedErr is the native adapter's failure for a turn its caller's
// context ended, from callerEnded itself.
func callerEndedErr(t *testing.T, ctx context.Context) error {
	t.Helper()
	err := (&nativeSession{}).callerEnded(ctx)
	if err == nil {
		t.Fatal("callerEnded saw no end of the caller's context")
	}
	return err
}

// TestEventCodecErrorsKeepMessageClassCodeAndIs covers each class of error
// that reaches Event.Err (the one live.go emits and the one native.go emits;
// refusals emit nothing): the decoded error is a *RemoteError with the
// original message, the class and code named here, and errors.Is answering
// exactly as it did on the original for every class's sentinel.
func TestEventCodecErrorsKeepMessageClassCodeAndIs(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel2 := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel2()
	for _, tc := range []struct {
		name  string
		err   func(t *testing.T) error
		class EventErrClass
		code  int
		is    []error
	}{
		{"the agent answered session/prompt with a JSON-RPC error",
			func(*testing.T) error {
				return &acp.RPCError{Code: -32603, Message: "Internal error", Data: json.RawMessage(`{"details":"overloaded"}`)}
			},
			EventErrRPC, -32603, nil},
		{"the connection closed under the turn",
			func(*testing.T) error { return acp.ErrClosed }, EventErrClosed, 0, []error{acp.ErrClosed}},
		{"the agent exited clean (Close's wrapped ErrAgentExited)",
			func(*testing.T) error { return fmt.Errorf("%w: exit 0", acp.ErrAgentExited) },
			EventErrAgentExited, 0, []error{acp.ErrAgentExited, ErrAgentExited}},
		{"the agent exited non-zero under the turn (the reaper's error)",
			agentExitStatusErr, EventErrAgentExitStatus, 3, nil},
		{"the live caller's context was cancelled",
			func(*testing.T) error { return cancelled.Err() }, EventErrCanceled, 0, []error{context.Canceled}},
		{"the live caller's deadline passed",
			func(*testing.T) error { return expired.Err() }, EventErrDeadline, 0, []error{context.DeadlineExceeded}},
		{"the native caller's context was cancelled",
			func(t *testing.T) error { return callerEndedErr(t, cancelled) }, EventErrCanceled, 0, []error{context.Canceled}},
		{"the native caller's deadline passed",
			func(t *testing.T) error { return callerEndedErr(t, expired) }, EventErrDeadline, 0, []error{context.DeadlineExceeded}},
		{"a native 401",
			func(t *testing.T) error {
				return nativeTurnErr(t, "hi", reply(errorParts(&fantasy.ProviderError{StatusCode: 401, Message: "who are you"})))
			},
			EventErrAuth, 401, []error{harness.ErrAuth}},
		{"a native 404",
			func(t *testing.T) error {
				return nativeTurnErr(t, "hi", reply(errorParts(&fantasy.ProviderError{StatusCode: 404, Message: "no such model"})))
			},
			EventErrModelNotFound, 404, []error{harness.ErrModelNotFound}},
		{"a native context too large",
			func(t *testing.T) error {
				return nativeTurnErr(t, "hi", reply(errorParts(&fantasy.ProviderError{StatusCode: 400, ContextTooLargeErr: true})))
			},
			EventErrContextTooLarge, 400, []error{harness.ErrContextTooLarge}},
		// 422, not a 5xx: the harness retries those, which would spend steps
		// the script does not have and seconds the test does not need.
		{"a native provider failure with no typed meaning",
			func(t *testing.T) error {
				return nativeTurnErr(t, "hi", reply(errorParts(&fantasy.ProviderError{StatusCode: 422, Message: "unprocessable"})))
			},
			EventErrProvider, 422, nil},
		{"a native stream error with no status",
			func(t *testing.T) error {
				return nativeTurnErr(t, "hi", reply(errorParts(errors.New("connection reset by peer"))))
			},
			EventErrProvider, 0, nil},
		{"a native empty step (classify's wrapping)",
			func(*testing.T) error {
				return phraseTurnError(fmt.Errorf("harness: model %q: %w", "test/a", harness.ErrEmptyStep))
			},
			EventErrEmptyStep, 0, []error{harness.ErrEmptyStep}},
		{"a native prompt of only whitespace",
			func(t *testing.T) error { return nativeTurnErr(t, "   ") },
			EventErrEmptyPrompt, 0, []error{harness.ErrEmptyPrompt}},
		{"the native session closed under its turn",
			func(*testing.T) error { return phraseTurnError(harness.ErrClosed) },
			EventErrHarnessClosed, 0, []error{harness.ErrClosed}},
		{"a write to an agent whose stdin has gone",
			func(*testing.T) error { return &fs.PathError{Op: "write", Path: "|1", Err: syscall.EPIPE} },
			EventErrOther, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := tc.err(t)
			for _, s := range tc.is {
				if !errors.Is(orig, s) {
					t.Fatalf("the original %q does not match %v: the case is wrong, not the codec", orig, s)
				}
			}
			// The comparison holds the decoded error to a *RemoteError with the
			// original's message, answering errors.Is for every class's
			// sentinel as the original did; the class and code are pinned here.
			got, body := assertRoundTrips(t, "the error", Event{Type: EventError, Err: orig, At: codecTestTime})
			remote := got.Err.(*RemoteError)
			if remote.Class != tc.class || remote.Code != tc.code {
				t.Fatalf("class %q code %d, want %q code %d (body %s)", remote.Class, remote.Code, tc.class, tc.code, body)
			}
			for _, s := range tc.is {
				if !errors.Is(remote, s) {
					t.Fatalf("errors.Is(decoded, %v) is false", s)
				}
			}
		})
	}
}

// TestClassifyEventErrKnowsTheThreeSyntheticEndings: session.go's three
// refusals never reach Event.Err (they are returned from Prompt, not
// emitted), so they are not in TestEventCodecErrorsKeepMessageClassCodeAndIs
// above; ClassifyEventErr is how the engine classifies them into a
// TurnInfo.ErrClass instead, and this pins that it does, without needing a
// live turn to produce one.
func TestClassifyEventErrKnowsTheThreeSyntheticEndings(t *testing.T) {
	for _, tc := range []struct {
		err   error
		class EventErrClass
	}{
		{ErrPromptCancelled, EventErrPromptCancelled},
		{ErrPromptInFlight, EventErrPromptInFlight},
		{ErrForeignTurn, EventErrForeignTurn},
	} {
		if got := ClassifyEventErr(tc.err); got != tc.class {
			t.Fatalf("ClassifyEventErr(%v) = %q, want %q", tc.err, got, tc.class)
		}
	}
}

// TestNewSentinelsDoNotReclassifyAnExistingError: adding the three synthetic
// classes to eventErrSentinels must not change how any error that already
// reaches Event.Err classifies (plan 021's C3a instruction) — every case in
// TestEventCodecErrorsKeepMessageClassCodeAndIs already proves this by
// construction (each case's class is pinned and none of them is one of the
// three new ones), so this only says the negative out loud: none of the
// errors that test covers is matched by one of the three new sentinels.
func TestNewSentinelsDoNotReclassifyAnExistingError(t *testing.T) {
	existing := []error{
		acp.ErrClosed, acp.ErrAgentExited, context.Canceled, context.DeadlineExceeded,
		harness.ErrAuth, harness.ErrModelNotFound, harness.ErrContextTooLarge,
		harness.ErrEmptyStep, harness.ErrEmptyPrompt, harness.ErrClosed,
	}
	for _, err := range existing {
		for _, synthetic := range []error{ErrPromptCancelled, ErrPromptInFlight, ErrForeignTurn} {
			if errors.Is(err, synthetic) {
				t.Fatalf("an existing sentinel %v now matches the new synthetic sentinel %v", err, synthetic)
			}
		}
		class, _ := classifyEventErr(err)
		if class == EventErrPromptCancelled || class == EventErrPromptInFlight || class == EventErrForeignTurn {
			t.Fatalf("classifyEventErr(%v) = %q, a synthetic class it must never take", err, class)
		}
	}
}

// TestEventCodecReencodesADecodedEventUnchanged: a decoded event encodes to
// the body it came from — a RemoteError keeps its class and code rather than
// being classified again as "other", and a class this build does not know is
// kept as it is and matches no sentinel.
func TestEventCodecReencodesADecodedEventUnchanged(t *testing.T) {
	bodies := []string{
		`{"type":"error","err":{"message":"json-rpc error -32603: Internal error","class":"rpc","code":-32603}}`,
		`{"type":"error","err":{"message":"native: provider \"x\" rejected the API key (HTTP 401)","class":"auth","code":401}}`,
		`{"type":"error","err":{"message":"a later craze's error","class":"quantum","code":7}}`,
		`{"type":"tool","tool":{"id":"call-1","output":{"exitCode":0},"task":{}},"at":"2026-09-19T10:30:45.123456789Z"}`,
	}
	for _, body := range bodies {
		ev, err := DecodeEvent(body)
		if err != nil {
			t.Fatalf("DecodeEvent(%s): %v", body, err)
		}
		again, err := EncodeEvent(ev)
		if err != nil {
			t.Fatalf("EncodeEvent: %v", err)
		}
		if again != body {
			t.Fatalf("re-encoded\n got %s\nwant %s", again, body)
		}
	}
	ev, _ := DecodeEvent(bodies[2])
	for _, c := range eventErrSentinels {
		if errors.Is(ev.Err, c.sentinel) {
			t.Fatalf("an unknown class matched %v", c.sentinel)
		}
	}
	auth, _ := DecodeEvent(bodies[1])
	if !errors.Is(auth.Err, harness.ErrAuth) {
		t.Fatal("a decoded auth error does not match harness.ErrAuth")
	}
}

// TestDecodeEventPreservesATypeItDoesNotKnow pins the choice DecodeEvent
// documents: an unknown type decodes, with its fields, and keys this build
// does not know are ignored — so a journal a later craze wrote still replays.
func TestDecodeEventPreservesATypeItDoesNotKnow(t *testing.T) {
	ev, err := DecodeEvent(`{"type":"hologram","text":"hi","futureKey":{"x":1},"at":"2026-09-19T10:30:45Z"}`)
	if err != nil {
		t.Fatalf("DecodeEvent: %v", err)
	}
	if ev.Type != "hologram" || ev.Text != "hi" || !ev.At.Equal(time.Date(2026, 9, 19, 10, 30, 45, 0, time.UTC)) {
		t.Fatalf("decoded %+v", ev)
	}
}

// TestDecodeEventRefusesMalformedInput: anything that is not one JSON object
// with a type is an error, never a panic and never a silently empty event. An
// offset time is not malformed: it decodes to the same instant in UTC.
func TestDecodeEventRefusesMalformedInput(t *testing.T) {
	for _, body := range []string{
		``,
		`not json`,
		`[]`,
		`null`,
		`"text"`,
		`{}`,
		`{"type":""}`,
		`{"text":"no type"}`,
		`{"type":"text","text":"hel`,
		`{"type":"text"} {"type":"text"}`,
		`{"type":"text","queuePos":"one"}`,
		`{"type":"text","at":"yesterday"}`,
		`{"type":"tool","tool":[]}`,
		`{"type":"error","err":"boom"}`,
		`{"type":"question","question":{"answers":{"q1":"a"}}}`,
		`{"type":7}`,
	} {
		if ev, err := DecodeEvent(body); err == nil {
			t.Fatalf("DecodeEvent(%q) = %+v, want an error", body, ev)
		}
	}
	ev, err := DecodeEvent(`{"type":"text","at":"2026-09-19T12:30:45+02:00"}`)
	if err != nil || ev.At.Location() != time.UTC || !ev.At.Equal(time.Date(2026, 9, 19, 10, 30, 45, 0, time.UTC)) {
		t.Fatalf("an offset time decoded as %v, %v; want 10:30:45 UTC", ev.At, err)
	}
}

// TestEncodeEventRefusesWhatItCannotWrite: an event with no type, and a time
// JSON cannot hold, are encode errors — the event log's omitted record, not a
// body that would not decode.
func TestEncodeEventRefusesWhatItCannotWrite(t *testing.T) {
	if body, err := EncodeEvent(Event{}); err == nil {
		t.Fatalf("EncodeEvent(Event{}) = %s, want an error", body)
	}
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, ev := range []Event{
		{Type: EventText, At: far},
		{Type: EventTool, Tool: &ToolEvent{At: far}},
		{Type: EventSubagent, Subagent: &SubagentInfo{EndedAt: far}},
		{Type: EventQueue, Queue: &QueuedPrompt{QueuedAt: far}},
	} {
		if body, err := EncodeEvent(ev); err == nil {
			t.Fatalf("EncodeEvent with a year-10000 time = %s, want an error", body)
		}
	}
}

// TestEncodeEventBodiesShareNothingUnderConcurrentUse: EncodeEvent reuses
// pooled encoders, and it will run on every publisher's goroutine at once.
// Each body must be its own: kept until every goroutine is done, every one
// still decodes to the event it was made from. Some events are large, so a
// body well past the encoder's own buffer sizes is exercised too.
func TestEncodeEventBodiesShareNothingUnderConcurrentUse(t *testing.T) {
	const workers, each = 8, 200
	type result struct {
		ev   Event
		body string
		err  error
	}
	results := make([][]result, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range each {
				text := fmt.Sprintf("worker %d event %d ", w, i)
				if i%50 == 0 {
					text += strings.Repeat("x", 64<<10)
				}
				ev := Event{Type: EventText, Text: text, At: codecTestTime.Add(time.Duration(i))}
				body, err := EncodeEvent(ev)
				results[w] = append(results[w], result{ev, body, err})
			}
		})
	}
	waitDone(t, &wg)
	for _, rs := range results {
		for _, r := range rs {
			if r.err != nil {
				t.Fatalf("EncodeEvent: %v", r.err)
			}
			got, err := DecodeEvent(r.body)
			if err != nil {
				t.Fatalf("DecodeEvent: %v", err)
			}
			if d := codecDiff(r.ev, got); len(d) > 0 {
				t.Fatalf("a body changed after it was returned: %v", d)
			}
		}
	}
}

// BenchmarkEncodeEventTextDelta is the common case on the emit path: one
// streamed chunk of text with its time.
func BenchmarkEncodeEventTextDelta(b *testing.B) {
	ev := Event{Type: EventText, Text: "Here is the next chunk of the answer, ", At: time.Now()}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := EncodeEvent(ev); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncodeEventLargeTool is the expensive case: a shell tool at its
// output caps with a diff.
func BenchmarkEncodeEventLargeTool(b *testing.B) {
	tail := strings.Repeat("output line <with> & some text\n", outputTailCap/32)
	ev := Event{Type: EventTool, At: time.Now(), Tool: &ToolEvent{
		ID: "call-1", Title: "Run make", Status: "completed", Kind: "execute", RawInput: "make",
		Output: &ToolOutput{ExitCode: ptr(1), Stdout: tail, StdoutHead: tail[:outputHeadCap], Truncated: true},
		Diffs:  []ToolDiff{{Path: "/a.go", OldText: tail, NewText: tail + "x", Added: 1, Removed: 0}},
		At:     time.Now(),
	}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := EncodeEvent(ev); err != nil {
			b.Fatal(err)
		}
	}
}
