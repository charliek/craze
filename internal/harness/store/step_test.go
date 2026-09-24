package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
)

// step writes one step and fails the test on error.
func step(t *testing.T, s *Store, steers []MessageEntry, assistant MessageEntry, tool *MessageEntry) []string {
	t.Helper()
	ids, err := s.AppendStep(steers, assistant, tool)
	if err != nil {
		t.Fatalf("AppendStep: %v", err)
	}
	return ids
}

func entryIDs(es []Entry) []string {
	var ids []string
	for _, e := range es {
		ids = append(ids, e.ID)
	}
	return ids
}

// TestToolStepWithASteerIsOneAppend: a step is one write however many lines
// it has, and AppendStep returns the ids of exactly the entries it wrote.
// The write log counts Write calls on the descriptor, so a store that wrote
// a step's entries one at a time would show one write per line here, and one
// that lost the steer would show two lines, not three.
func TestToolStepWithASteerIsOneAppend(t *testing.T) {
	opts := testOptions(t)
	var log writeLog
	opts.openFile = log.opener()
	s := newStore(t, opts)
	if err := s.AppendUser(user("fix the build", kimi)); err != nil {
		t.Fatal(err)
	}
	first := step(t, s, nil, calls(kimi, "call_a", "call_b"), results(kimi, "call_a", "call_b"))
	// The same call ids again: providers that number calls per response
	// repeat them, and the invariant holds per step, not across the file.
	second := step(t, s, []MessageEntry{user("also run the tests", kimi)}, calls(kimi, "call_a"), results(kimi, "call_a"))

	writes := log.all()
	if len(writes) != 2 {
		t.Fatalf("two steps took %d writes, want 2", len(writes))
	}
	if n := bytes.Count(writes[1], []byte("\n")); n != 3 {
		t.Fatalf("the step with a steer wrote %d lines, want 3 (steer, assistant, tool):\n%s", n, writes[1])
	}
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := first, entryIDs(tr.Entries[:3]); !reflect.DeepEqual(got, want) {
		t.Fatalf("the first step returned ids %q; it wrote %q", got, want)
	}
	if got, want := second, entryIDs(tr.Entries[3:]); !reflect.DeepEqual(got, want) {
		t.Fatalf("the second step returned ids %q; it wrote %q", got, want)
	}
	var roles []string
	for _, e := range tr.Entries[3:] {
		roles = append(roles, string(e.Message.Role))
	}
	if want := []string{"user", "assistant", "tool"}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("the step's lines are %v, want %v", roles, want)
	}
	if steer := tr.Entries[3]; steer.ParentID != tr.Entries[2].ID {
		t.Fatalf("the steer's parent is %q, want the previous step's tool entry %q", steer.ParentID, tr.Entries[2].ID)
	}
	want := []string{
		"user: fix the build",
		`assistant: [call call_a read {"filePath":"call_a.go"}] [call call_b read {"filePath":"call_b.go"}]`,
		"tool: [result call_a: body of call_a] [result call_b: body of call_b]",
		"user: also run the tests",
		`assistant: [call call_a read {"filePath":"call_a.go"}]`,
		"tool: [result call_a: body of call_a]",
	}
	for name, got := range map[string][]fantasy.Message{"store": s.Context(kimi), "loaded": tr.Context(kimi)} {
		if texts := messageTexts(got); !reflect.DeepEqual(texts, want) {
			t.Fatalf("%s context = %q, want %q", name, texts, want)
		}
		if err := unpairedIn(got); err != nil {
			t.Fatalf("%s context: %v", name, err)
		}
	}
}

// TestHeldUsersAllPersist: user entries held for one step all go out ahead
// of it, in order. The control is DiscardHeldUsers, the runner's way to get
// H1's replacement back: after it, only the later entry is written.
func TestHeldUsersAllPersist(t *testing.T) {
	s := newStore(t, testOptions(t))
	for _, q := range []string{"q1", "q2"} {
		if err := s.AppendUser(user(q, kimi)); err != nil {
			t.Fatal(err)
		}
	}
	if ids := step(t, s, nil, answer("", "both", kimi), nil); len(ids) != 3 {
		t.Fatalf("the step wrote %d entries, want 3", len(ids))
	}

	if err := s.AppendUser(user("q3, never answered", kimi)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendModelChange(minimax); err != nil {
		t.Fatal(err)
	}
	s.DiscardHeldUsers()
	if err := s.AppendUser(user("q4", minimax)); err != nil {
		t.Fatal(err)
	}
	step(t, s, nil, answer("", "four", minimax), nil)

	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"user: q1", "user: q2", "assistant: both", "user: q4", "assistant: four"}
	if got := messageTexts(tr.Context(minimax)); !reflect.DeepEqual(got, want) {
		t.Fatalf("context = %q, want %q", got, want)
	}
	// Discarding users leaves a held change held.
	if got := fileTypes(t, s.Path()); !reflect.DeepEqual(got, []string{"session", "message", "message", "message", "model_change", "message", "message"}) {
		t.Fatalf("line types = %v", got)
	}
}

// TestToolCallsAreOutput: a step whose only content is tool calls is
// output; the same message without its call is not. AppendAssistant, the
// text-only path, still wants text.
func TestToolCallsAreOutput(t *testing.T) {
	opts := testOptions(t)
	s := newStore(t, opts)
	if err := s.AppendUser(user("q", kimi)); err != nil {
		t.Fatal(err)
	}
	thinking := MessageEntry{Message: callsMsg("only thinking", "", "a"), Model: kimi}
	thinking.Message.Content = thinking.Message.Content[:1] // the call removed
	if _, err := s.AppendStep(nil, thinking, nil); !errors.Is(err, ErrNoOutput) {
		t.Fatalf("AppendStep(reasoning only) = %v, want ErrNoOutput", err)
	}
	if err := s.AppendAssistant(calls(kimi, "a")); !errors.Is(err, ErrNoOutput) {
		t.Fatalf("AppendAssistant(calls, no text) = %v, want ErrNoOutput", err)
	}
	if _, err := os.Stat(opts.Home); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a step with no output created %s", opts.Home)
	}
	withCall := MessageEntry{Message: callsMsg("only thinking", "", "a"), Model: kimi}
	step(t, s, nil, withCall, results(kimi, "a"))
	if got := fileTypes(t, s.Path()); len(got) != 4 {
		t.Fatalf("line types = %v, want the header, the user entry and the step's two lines", got)
	}
}

// TestProviderExecutedPartsAreExempt: a call the provider ran itself sits
// with its result inside the assistant message and needs no tool message.
// The control is the same parts without the flag, which are refused.
func TestProviderExecutedPartsAreExempt(t *testing.T) {
	msg := func(executed bool) fantasy.Message {
		m := assistantMsg("", "searched")
		m.Content = append(m.Content,
			fantasy.ToolCallPart{ToolCallID: "ws_1", ToolName: "web_search", Input: `{"q":"peano"}`, ProviderExecuted: executed},
			fantasy.ToolResultPart{ToolCallID: "ws_1", Output: fantasy.ToolResultOutputContentText{Text: "3 hits"}, ProviderExecuted: executed},
		)
		return m
	}
	s := newStore(t, testOptions(t))
	if err := s.AppendUser(user("q", kimi)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendStep(nil, MessageEntry{Message: msg(false), Model: kimi}, nil); !errors.Is(err, ErrUnpaired) {
		t.Fatalf("a client call and result inside one assistant message = %v, want ErrUnpaired", err)
	}
	step(t, s, nil, MessageEntry{Message: msg(true), Model: kimi}, nil)
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tr.Context(kimi)); n != 2 {
		t.Fatalf("context has %d messages, want the user entry and the answer", n)
	}
}

// userWithCall is a user message carrying a tool call, which only an
// assistant message may.
func userWithCall() fantasy.Message {
	m := fantasy.NewUserMessage("q")
	m.Content = append(m.Content, fantasy.ToolCallPart{ToolCallID: "a", ToolName: "read", Input: "{}"})
	return m
}

func toolMessage(parts ...fantasy.MessagePart) *MessageEntry {
	return &MessageEntry{Message: fantasy.Message{Role: fantasy.MessageRoleTool, Content: parts}, Model: kimi}
}

func errorResult(id string, err error) fantasy.ToolResultPart {
	return fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentError{Error: err}}
}

// TestAppendStepRefusesBrokenSteps: every break of the pairing invariant,
// and every result that could not be replayed, is refused with nothing
// written and the held user entry still held. The control, run on the same
// store after each refusal, is a paired step, which is written with that
// user entry.
func TestAppendStepRefusesBrokenSteps(t *testing.T) {
	withText := results(kimi, "a")
	withText.Message.Content = append([]fantasy.MessagePart{fantasy.TextPart{Text: "here you go"}}, withText.Message.Content...)
	resultInAnswer := answer("", "done", kimi)
	resultInAnswer.Message.Content = append(resultInAnswer.Message.Content, resultsMsg("a").Content...)
	asUser := results(kimi, "a")
	asUser.Message.Role = fantasy.MessageRoleUser

	for name, tc := range map[string]struct {
		steers    []MessageEntry
		assistant MessageEntry
		tool      *MessageEntry
		unpaired  bool // refused as ErrUnpaired, not only as some error
	}{
		"calls and no tool message":           {nil, calls(kimi, "a"), nil, true},
		"a tool message and no calls":         {nil, answer("", "done", kimi), results(kimi, "a"), true},
		"results for other calls":             {nil, calls(kimi, "a", "b"), results(kimi, "a", "c"), true},
		"results out of order":                {nil, calls(kimi, "a", "b"), results(kimi, "b", "a"), true},
		"a result missing":                    {nil, calls(kimi, "a", "b"), results(kimi, "a"), true},
		"a result extra":                      {nil, calls(kimi, "a"), results(kimi, "a", "b"), true},
		"a repeated call id":                  {nil, calls(kimi, "a", "a"), results(kimi, "a", "a"), true},
		"an empty call id":                    {nil, calls(kimi, ""), results(kimi, ""), true},
		"text in the tool message":            {nil, calls(kimi, "a"), withText, true},
		"a client result in the answer":       {nil, resultInAnswer, nil, true},
		"a tool call in a steer":              {[]MessageEntry{{Message: userWithCall(), Model: kimi}}, calls(kimi, "a"), results(kimi, "a"), true},
		"an error result with no text":        {nil, calls(kimi, "a"), toolMessage(errorResult("a", errors.New(""))), false},
		"an error result with a nil error":    {nil, calls(kimi, "a"), toolMessage(errorResult("a", nil)), false},
		"a result with no output":             {nil, calls(kimi, "a"), toolMessage(fantasy.ToolResultPart{ToolCallID: "a"}), false},
		"a tool entry that is a user message": {nil, calls(kimi, "a"), asUser, false},
	} {
		t.Run(name, func(t *testing.T) {
			opts := testOptions(t)
			s := newStore(t, opts)
			if err := s.AppendUser(user("q", kimi)); err != nil {
				t.Fatal(err)
			}
			_, err := s.AppendStep(tc.steers, tc.assistant, tc.tool)
			if err == nil || (tc.unpaired && !errors.Is(err, ErrUnpaired)) {
				t.Fatalf("AppendStep = %v, want a refusal (ErrUnpaired: %v)", err, tc.unpaired)
			}
			if _, err := os.Stat(opts.Home); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a refused step created %s", opts.Home)
			}
			step(t, s, nil, calls(kimi, "a", "b"), results(kimi, "a", "b"))
			if got := fileTypes(t, s.Path()); len(got) != 4 {
				t.Fatalf("after the refusal, a paired step wrote line types %v; want the held user entry and the step", got)
			}
		})
	}
}

// writeAndReload writes a turn of one step, checks that the store's own
// context and a reload's are the same, and returns the message at entry i
// as it read back.
func writeAndReload(t *testing.T, assistant MessageEntry, tool *MessageEntry, i int) fantasy.Message {
	t.Helper()
	s := newStore(t, testOptions(t))
	if err := s.AppendUser(user("q", kimi)); err != nil {
		t.Fatal(err)
	}
	step(t, s, nil, assistant, tool)
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.Context(kimi), tr.Context(kimi); !reflect.DeepEqual(got, want) {
		t.Fatalf("the store replays\n%#v\nbut a reload replays\n%#v", got, want)
	}
	return tr.Entries[i].Message
}

// TestErrorResultsReadBackByText: Fantasy writes an error result as its
// text and reads it back as errors.New of that text, so a wrapped error (the
// shape Fantasy gives an invalid tool call) is written, reads back with the
// same text, and the store replays what a reload does. The control shows the
// value read back is not the value written: had the store kept what it was
// handed instead of what reads back, its replay would differ from a reload's.
func TestErrorResultsReadBackByText(t *testing.T) {
	var v map[string]any
	syntaxErr := json.Unmarshal([]byte("{"), &v)
	invalid := fmt.Errorf("invalid JSON input: %w", syntaxErr)
	tool := toolMessage(errorResult("a", invalid))

	back := writeAndReload(t, calls(kimi, "a"), tool, 2)
	if got := messageTexts([]fantasy.Message{back}); !reflect.DeepEqual(got, []string{"tool: [result a: error: " + invalid.Error() + "]"}) {
		t.Fatalf("the error result read back as %q", got)
	}
	if reflect.DeepEqual(back, tool.Message) {
		t.Fatal("control: the error result read back as the same value")
	}
}

// TestProviderOptionsThatReadBackChangedAreKept: a number inside provider
// options held as map[string]any reads back as a float64, and an integer
// past 2^53 loses its last digits. H1 saves such an entry, and so does
// AppendStep: nothing compares what reads back with what was handed in. The
// store replays the value that reads back, as a reload does; the control
// shows it is not the value written.
func TestProviderOptionsThatReadBackChangedAreKept(t *testing.T) {
	m := assistantMsg("", "a")
	m.Content[0] = fantasy.TextPart{Text: "a", ProviderOptions: fantasy.ProviderOptions{
		openaicompat.Name: &openaicompat.ContentExtraFields{Fields: map[string]any{"seq": int64(9007199254740993)}},
	}}
	back := writeAndReload(t, MessageEntry{Message: m, Model: kimi}, nil, 1)
	if reflect.DeepEqual(back, m) {
		t.Fatal("control: the provider options read back as the same value")
	}
}

// TestInterruptedToolEntry: the flag round-trips on a tool entry, as the
// runner's cancel defence writes one, and is absent on one that was not
// cut short.
func TestInterruptedToolEntry(t *testing.T) {
	s := newStore(t, testOptions(t))
	if err := s.AppendUser(user("q", kimi)); err != nil {
		t.Fatal(err)
	}
	step(t, s, nil, calls(kimi, "a"), results(kimi, "a"))
	cut := calls(kimi, "b")
	cut.Interrupted, cut.StopReason = true, "cancelled"
	aborted := toolMessage(errorResult("b", errors.New("Tool execution aborted")))
	aborted.Interrupted = true
	step(t, s, nil, cut, aborted)

	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	var got []bool
	for _, e := range tr.Entries[1:] {
		got = append(got, e.Interrupted)
	}
	if want := []bool{false, false, true, true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("interrupted flags = %v, want %v", got, want)
	}
}

// TestSubagentUsageRoundTrips (plan 026 §3.7): a tool entry's subagent_usage
// rows — one per model a step's children ran on — are written under that key
// and read back as they were, in order; the entry carries no plain usage. The
// control is the step before it, with no rows: no key on its line at all, so
// every entry written before sub-agents reads, and is written, as it was.
func TestSubagentUsageRoundTrips(t *testing.T) {
	s := newStore(t, testOptions(t))
	if err := s.AppendUser(user("q", kimi)); err != nil {
		t.Fatal(err)
	}
	step(t, s, nil, calls(kimi, "a"), results(kimi, "a"))
	rows := []ModelUsage{
		{Provider: "test", Model: "test/b", WireModel: "wire-b", Usage: Usage{Input: 20, Output: 10, CacheRead: 8}},
		{Provider: "other", Model: "other/c", WireModel: "wire-c", Usage: Usage{Input: 10, Output: 5, Reasoning: 2}},
	}
	withRows := results(kimi, "b")
	withRows.SubagentUsage = rows
	step(t, s, nil, calls(kimi, "b"), withRows)

	b, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if n := strings.Count(string(b), `"subagent_usage"`); n != 1 || !strings.Contains(lines[len(lines)-1], `"subagent_usage":[{"provider":"test","model":"test/b","wire_model":"wire-b","usage":{`) {
		t.Fatalf("the key appears %d times; want once, on the last line:\n%s", n, b)
	}
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	last := tr.Entries[len(tr.Entries)-1]
	if !reflect.DeepEqual(last.SubagentUsage, rows) || last.Usage != nil {
		t.Fatalf("read back rows %+v and usage %+v; want %+v and none", last.SubagentUsage, last.Usage, rows)
	}
	if prev := tr.Entries[len(tr.Entries)-3]; prev.SubagentUsage != nil {
		t.Fatalf("control: the earlier tool entry read back rows %+v", prev.SubagentUsage)
	}
}

// TestHeaderRecordsTheToolContract: the header carries the tool profile and
// the tools array's hash, never the array, and they read back. The control
// is a session with no tools, whose header has neither field.
func TestHeaderRecordsTheToolContract(t *testing.T) {
	tools := []byte(`[{"name":"read"},{"name":"bash"}]`)
	sum := sha256.Sum256(tools)
	opts := testOptions(t)
	opts.ToolProfile, opts.Tools = "opencode", tools
	s := newStore(t, opts)
	turn(t, s, "q", "a", kimi)
	head := firstLine(t, s.Path())
	if want := `,"tool_profile":"opencode","tools_sha256":"` + hex.EncodeToString(sum[:]) + `"}`; !strings.HasSuffix(head, want) {
		t.Fatalf("header = %s, want it to end %s", head, want)
	}
	if strings.Contains(head, "bash") {
		t.Fatalf("the header holds the tools themselves: %s", head)
	}
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if tr.Header != s.Header() || tr.Header.ToolProfile != "opencode" || tr.Header.ToolsSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("header read back as %+v, want %+v", tr.Header, s.Header())
	}

	plain := newStore(t, testOptions(t))
	turn(t, plain, "q", "a", kimi)
	if head := firstLine(t, plain.Path()); strings.Contains(head, "tool_profile") || strings.Contains(head, "tools_sha256") {
		t.Fatalf("a session with no tools wrote tool fields: %s", head)
	}
}

func firstLine(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := strings.Cut(string(data), "\n")
	return head
}
