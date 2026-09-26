package store

import (
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// These tests cover the compaction entry (plan 028 §3.2, §3.9): written alone
// by AppendCompaction, kept by Load and Open at the tail, checked outside the
// torn tail, and, when it succeeded, the start of every later context — its
// summary message, its tail of whole steps, and what came after it — which
// Steps groups and Cut cuts.

// compactRenderer renders a reminder as testRenderer does, and a summary
// message as a user message naming the summary, the way the harness renders
// one: the store never sees the text.
var compactRenderer = Renderer{
	Reminder: testRenderer.Reminder,
	Summary:  func(c Compaction) fantasy.Message { return fantasy.NewUserMessage("<summary " + c.Summary + ">") },
}

// compacted is opts with the compaction renderer.
func compacted(opts Options) Options {
	opts.Render = compactRenderer
	return opts
}

// compactUsage is what every compaction here spent.
var compactUsage = Usage{Input: 12, Output: 34, Reasoning: 5, CacheRead: 6789, CacheCreation: 1}

// success is a successful automatic compaction whose tail starts at first
// ("" for none).
func success(summary, first string) Compaction {
	return Compaction{Summary: summary, FirstKeptID: first, TokensBefore: 90000, TokensAfter: 21000,
		Reason: CompactionAuto, Segment: SegmentName(1)}
}

// failure is a failed automatic compaction.
func failure(msg string) Compaction {
	return Compaction{Error: msg, TokensBefore: 90000, TokensAfter: 90000, Reason: CompactionAuto}
}

// compact appends c in turn n and fails the test on error.
func compact(t *testing.T, s *Store, n int, c Compaction) string {
	t.Helper()
	id, err := s.AppendCompaction(n, kimi, compactUsage, c)
	if err != nil {
		t.Fatalf("AppendCompaction: %v", err)
	}
	return id
}

// toolTurn writes a turn of one step that calls tools: the user entry, the
// assistant entry with a read call per id, and the tool entry answering them.
func toolTurn(t *testing.T, s *Store, prompt string, ids ...string) {
	t.Helper()
	s.DiscardHeldUsers()
	if err := s.AppendUser(user(prompt, kimi)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendStep(nil, calls(kimi, ids...), results(kimi, ids...)); err != nil {
		t.Fatal(err)
	}
}

// firsts is each step's first entry id.
func firsts(steps []Step) []string {
	var out []string
	for _, st := range steps {
		out = append(out, st.First)
	}
	return out
}

// entryByText is the id of the message entry whose text is text.
func entryByText(t *testing.T, s *Store, text string) string {
	t.Helper()
	for _, e := range s.Transcript().Entries {
		if e.Type == TypeMessage && strings.HasSuffix(messageTexts([]fantasy.Message{e.Message})[0], ": "+text) {
			return e.ID
		}
	}
	t.Fatalf("no entry holds %q", text)
	return ""
}

// TestCompactionEntryRoundTrips: a compaction's line holds every field of
// §3.2's table in a fixed order, the optional ones only when set, and reads
// back whole; a success and a failure alike, and it is a known type.
func TestCompactionEntryRoundTrips(t *testing.T) {
	usage := Usage{Input: 1, Output: 2, Reasoning: 0, CacheRead: 3, CacheCreation: 4}
	for _, tc := range []struct {
		name string
		turn int
		c    Compaction
		want string
	}{
		{"a manual success with a tail", 3, Compaction{
			Summary: "so far", FirstKeptID: "00000005", TokensBefore: 890000, TokensAfter: 21000,
			Reason: CompactionManual, Command: "/compact keep the API", Segment: "segment_001.md",
		}, `"turn":3,"summary":"so far","firstKeptId":"00000005","tokensBefore":890000,"tokensAfter":21000,"reason":"manual","command":"/compact keep the API",` +
			`"provider":"fireworks","model":"fireworks/kimi-k3","wire_model":"accounts/fireworks/models/kimi-k3",` +
			`"usage":{"input":1,"output":2,"reasoning":0,"cache_read":3,"cache_creation":4},"segment":"segment_001.md"}`},
		{"an overflow success with no tail", 1, Compaction{
			Summary: "all of it", TokensBefore: 300000, TokensAfter: 900, Reason: CompactionOverflow, Segment: "segment_002.md",
		}, `"turn":1,"summary":"all of it","tokensBefore":300000,"tokensAfter":900,"reason":"overflow",` +
			`"provider":"fireworks","model":"fireworks/kimi-k3","wire_model":"accounts/fireworks/models/kimi-k3",` +
			`"usage":{"input":1,"output":2,"reasoning":0,"cache_read":3,"cache_creation":4},"segment":"segment_002.md"}`},
		{"a failure", 2, Compaction{
			TokensBefore: 5, TokensAfter: 5, Reason: CompactionAuto, Error: "summarizer: 3 attempts failed",
		}, `"turn":2,"tokensBefore":5,"tokensAfter":5,"reason":"auto",` +
			`"provider":"fireworks","model":"fireworks/kimi-k3","wire_model":"accounts/fireworks/models/kimi-k3",` +
			`"usage":{"input":1,"output":2,"reasoning":0,"cache_read":3,"cache_creation":4},"error":"summarizer: 3 attempts failed"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := usage
			e := Entry{Type: TypeCompaction, ID: "0000000a", ParentID: "00000009", Timestamp: fixedTime, Compaction: tc.c,
				MessageEntry: MessageEntry{Turn: tc.turn, Model: kimi, Usage: &u}}
			line, err := encodeEntry(e)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"type":"compaction","id":"0000000a","parentId":"00000009","timestamp":"2026-09-18T12:00:00.000Z",` + tc.want
			if string(line) != want {
				t.Fatalf("compaction line =\n%s\nwant\n%s", line, want)
			}
			back, err := decodeEntry(line)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back, e) {
				t.Fatalf("compaction entry read back as %+v, want %+v", back, e)
			}
			if back.Compaction.Succeeded() != (tc.c.Error == "") {
				t.Fatalf("Succeeded() = %v for %+v", back.Compaction.Succeeded(), tc.c)
			}
		})
	}
	if !knownType(TypeCompaction) {
		t.Fatal("knownType does not know compaction")
	}
}

// TestContextAppliesTheLatestCompaction (A14): from a successful compaction
// on, the context is its summary message (marked for redaction, P30), its
// tail — whole steps, from its FirstKeptID up to it — and the entries after
// it, each by today's rules. A later compaction replaces an earlier one, even
// when its tail reaches back past the earlier one's entry, whose summary is
// then not sent; one with no tail is its summary alone. A reopened store, and
// the store's copy, build the same context; one Load returns has no renderer,
// so the cut holds but no summary is sent.
func TestContextAppliesTheLatestCompaction(t *testing.T) {
	opts := compacted(testOptions(t))
	s := newStore(t, opts)
	turn(t, s, "q1", "a1", kimi)
	toolTurn(t, s, "q2", "call_a")
	if err := s.AppendAssistant(answer("", "a2", kimi)); err != nil { // a mid-turn step: no user entry leads it
		t.Fatal(err)
	}
	turn(t, s, "q3", "a3", kimi)

	steps := s.Steps(kimi)
	u1, u2, a2, u3 := entryByText(t, s, "q1"), entryByText(t, s, "q2"), entryByText(t, s, "a2"), entryByText(t, s, "q3")
	if got, want := firsts(steps), []string{u1, u2, a2, u3}; !slices.Equal(got, want) {
		t.Fatalf("the steps start at %q, want %q", got, want)
	}

	check := func(what string, want []string) {
		t.Helper()
		msgs, marks := s.ContextWithResults(kimi)
		if got := messageTexts(msgs); !slices.Equal(got, want) {
			t.Fatalf("%s: context = %q\nwant %q", what, got, want)
		}
		for i := range marks {
			if marks[i] != strings.HasPrefix(want[i], "user: <summary") {
				t.Fatalf("%s: marks = %v; only the summary message is marked", what, marks)
			}
		}
		if err := unpairedIn(msgs); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	compact(t, s, 3, success("one", a2))
	check("a tail from a mid-turn step", []string{"user: <summary one>", "assistant: a2", "user: q3", "assistant: a3"})
	if got, want := firsts(s.Steps(kimi)), []string{a2, u3}; !slices.Equal(got, want) {
		t.Fatalf("after the compaction the steps start at %q, want %q (its tail)", got, want)
	}

	turn(t, s, "q4", "a4", kimi)
	check("a step after it", []string{"user: <summary one>", "assistant: a2", "user: q3", "assistant: a3", "user: q4", "assistant: a4"})

	// A tail from q3 holds the first compaction's entry, which cuts nothing.
	compact(t, s, 4, success("two", u3))
	check("a second compaction's tail reaching past the first", []string{"user: <summary two>", "user: q3", "assistant: a3", "user: q4", "assistant: a4"})

	compact(t, s, 4, success("three", ""))
	check("no tail", []string{"user: <summary three>"})
	turn(t, s, "q5", "a5", kimi)
	want := []string{"user: <summary three>", "user: q5", "assistant: a5"}
	check("a turn after a compaction with no tail", want)

	if got := messageTexts(s.Transcript().Context(kimi)); !slices.Equal(got, want) {
		t.Fatalf("the copy's context = %q, want %q", got, want)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ropts := opts
	ropts.SessionID = ""
	if got := messageTexts(reopen(t, ropts, s.Path()).Context(kimi)); !slices.Equal(got, want) {
		t.Fatalf("the reopened store's context = %q, want %q", got, want)
	}
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := messageTexts(tr.Context(kimi)); !slices.Equal(got, want[1:]) {
		t.Fatalf("with no renderer the context = %q, want %q", got, want[1:])
	}
}

// TestAFailureEntryIsNotABoundary (A14, PD20): a failed compaction is written
// with its usage and no summary, and changes neither the context nor its
// steps — before any success, and after one, where the context stays the
// success's.
func TestAFailureEntryIsNotABoundary(t *testing.T) {
	s := newStore(t, compacted(testOptions(t)))
	turn(t, s, "q1", "a1", kimi)
	toolTurn(t, s, "q2", "call_a")
	before, beforeSteps := messageTexts(s.Context(kimi)), firsts(s.Steps(kimi))

	fid := compact(t, s, 2, failure("the summarizer answered 500 three times"))
	if got := messageTexts(s.Context(kimi)); !slices.Equal(got, before) {
		t.Fatalf("after a failure the context = %q, want it unchanged: %q", got, before)
	}
	if got := firsts(s.Steps(kimi)); !slices.Equal(got, beforeSteps) {
		t.Fatalf("after a failure the steps start at %q, want %q", got, beforeSteps)
	}
	tr := s.Transcript()
	f := tr.Entries[len(tr.Entries)-1]
	if f.ID != fid || f.Type != TypeCompaction || f.Compaction.Succeeded() || f.Usage == nil || *f.Usage != compactUsage {
		t.Fatalf("the failure entry = %+v; want its error and usage, and no summary", f)
	}

	u2 := entryByText(t, s, "q2")
	compact(t, s, 2, success("one", u2))
	after := []string{"user: <summary one>", "user: q2", callA, resultA}
	compact(t, s, 3, failure("cancelled after a paid attempt"))
	if got := messageTexts(s.Context(kimi)); !slices.Equal(got, after) {
		t.Fatalf("a failure after a success: context = %q, want %q", got, after)
	}
	if got, want := firsts(s.Steps(kimi)), []string{u2}; !slices.Equal(got, want) {
		t.Fatalf("a failure after a success: steps start at %q, want %q", got, want)
	}
	turn(t, s, "q3", "a3", kimi)
	if got, want := messageTexts(s.Context(kimi)), append(after, "user: q3", "assistant: a3"); !slices.Equal(got, want) {
		t.Fatalf("a turn after the failure: context = %q, want %q", got, want)
	}
}

// textSize is the test's size of a message: the bytes of its text as
// messageTexts shows it.
func textSize(m fantasy.Message) int64 { return int64(len(messageTexts([]fantasy.Message{m})[0])) }

// stepSizes is each step's size by textSize.
func stepSizes(steps []Step) []int64 {
	var out []int64
	for _, st := range steps {
		n := int64(0)
		for _, m := range st.Messages {
			n += textSize(m)
		}
		out = append(out, n)
	}
	return out
}

// TestCompactionTailIsWholeSteps (A14, P13): the cut walks a context's steps
// newest first and keeps each while their sizes together stay within the
// budget; the first step over it is not kept, and neither is anything older,
// small or not. A step goes with every entry its append wrote — a held
// change, the user entry, a reminder or a steer that led it — so a tail's
// first entry is a step's first, and a compaction that keeps it writes and
// reads back paired. An assistant-heavy or a result-heavy newest step alone
// over the budget is no tail, and so is a budget of 0 (tail_tokens = 0).
func TestCompactionTailIsWholeSteps(t *testing.T) {
	big := strings.Repeat("x", 4000)
	// A: a small turn. B: a turn whose read returns big. C: a mid-turn step
	// led by a reminder and a steer, answering with big. D: a small turn led
	// by a model change.
	build := func(t *testing.T) (*Store, []Step) {
		t.Helper()
		s := newStore(t, compacted(testOptions(t)))
		turn(t, s, "q1", "a1", kimi)
		if err := s.AppendUser(user("q2", kimi)); err != nil {
			t.Fatal(err)
		}
		heavy := MessageEntry{Model: kimi, Message: fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "call_a", Output: fantasy.ToolResultOutputContentText{Text: big}},
		}}}
		if _, err := s.AppendStep(nil, calls(kimi, "call_a"), &heavy); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendStepLed([]Lead{reminderLead("ask"), steerLead("a steer", kimi)}, answer("", big, kimi), nil); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendModelChange(kimi); err != nil {
			t.Fatal(err)
		}
		turn(t, s, "q3", "a3", kimi)
		return s, s.Steps(kimi)
	}
	s, steps := build(t)
	if len(steps) != 4 {
		t.Fatalf("%d steps, want 4", len(steps))
	}
	tr := s.Transcript()
	byID := func(id string) Entry { return tr.Entries[tr.index[id]] }
	if e := byID(steps[2].First); e.Type != TypeReminder {
		t.Fatalf("step C starts at a %s entry, want its reminder", e.Type)
	}
	if e := byID(steps[3].First); e.Type != TypeModelChange {
		t.Fatalf("step D starts at a %s entry, want its model change", e.Type)
	}
	size := stepSizes(steps)
	if size[1] < 4000 || size[2] < 4000 || size[0] > 100 || size[3] > 100 {
		t.Fatalf("step sizes %v; want B and C heavy, A and D light", size)
	}

	for _, tc := range []struct {
		name   string
		budget int64
		want   int // the tail's first step; 4 is none
	}{
		{"everything fits", size[0] + size[1] + size[2] + size[3], 0},
		{"the budget exactly", size[3], 3},
		{"a heavy step ends the walk, though an older light one would fit", size[3] + size[2] - 1, 3},
		{"the result-heavy step ends the walk", size[3] + size[2] + size[1] - 1, 2},
		{"a newest step over the budget", size[3] - 1, 4},
		{"tail_tokens = 0", 0, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Cut(steps, tc.budget, textSize); got != tc.want {
				t.Fatalf("Cut(budget %d) = %d, want %d (sizes %v)", tc.budget, got, tc.want, size)
			}
		})
	}

	// Every step's first entry can start a tail, and the context it leaves
	// is the summary and those whole steps, paired.
	for k := range steps {
		s, steps := build(t)
		compact(t, s, 3, success("s", steps[k].First))
		msgs := s.Context(kimi)
		var want []fantasy.Message
		for _, st := range steps[k:] {
			want = append(want, st.Messages...)
		}
		if got := messageTexts(msgs[1:]); !slices.Equal(got, messageTexts(want)) {
			t.Fatalf("a tail from step %d: context %q, want %q", k, got, messageTexts(want))
		}
		if err := unpairedIn(msgs); err != nil {
			t.Fatalf("a tail from step %d: %v", k, err)
		}
		if _, err := Load(s.Path()); err != nil {
			t.Fatalf("a tail from step %d: Load: %v", k, err)
		}
	}

	// A newest step alone over the budget, of either kind, is no tail.
	for name, last := range map[string]func(*Store) error{
		"assistant-heavy": func(s *Store) error { return s.AppendAssistant(answer("", big, kimi)) },
		"result-heavy": func(s *Store) error {
			heavy := MessageEntry{Model: kimi, Message: fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{ToolCallID: "call_b", Output: fantasy.ToolResultOutputContentText{Text: big}},
			}}}
			_, err := s.AppendStep(nil, calls(kimi, "call_b"), &heavy)
			return err
		},
	} {
		t.Run("a single "+name+" step", func(t *testing.T) {
			s := newStore(t, compacted(testOptions(t)))
			turn(t, s, "q1", "a1", kimi)
			if err := s.AppendUser(user("q2", kimi)); err != nil {
				t.Fatal(err)
			}
			if err := last(s); err != nil {
				t.Fatal(err)
			}
			steps := s.Steps(kimi)
			if got := Cut(steps, 1000, textSize); got != len(steps) {
				t.Fatalf("Cut = %d; the newest step is over the budget, so no tail (%d)", got, len(steps))
			}
		})
	}
}

// TestD33InTheTail (A14): a tail is sent by today's rules for models. A step
// another model answered loses its reasoning, and one that holds only that
// model's own provider-executed call goes altogether — in the context and in
// the step's messages, which is also the size the cut weighs it by, so a step
// whose reasoning is over the budget for its own model is within it for
// another.
func TestD33InTheTail(t *testing.T) {
	thinking := strings.Repeat("t", 3000)
	s := newStore(t, compacted(testOptions(t)))
	turn(t, s, "q1", "a1", kimi)
	if err := s.AppendUser(user("q2", kimiCode)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAssistant(answer(thinking, "a2", kimiCode)); err != nil {
		t.Fatal(err)
	}
	hosted := MessageEntry{Model: minimax, StopReason: "end_turn", Message: fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.ToolCallPart{ToolCallID: "ws_1", ToolName: "web_search", Input: `{"q":"x"}`, ProviderExecuted: true},
		fantasy.ToolResultPart{ToolCallID: "ws_1", Output: fantasy.ToolResultOutputContentText{Text: "found"}, ProviderExecuted: true},
	}}}
	if _, err := s.AppendStep(nil, hosted, nil); err != nil {
		t.Fatal(err)
	}
	turn(t, s, "q3", "a3", kimi)

	u2 := entryByText(t, s, "q2")
	own, other := s.Steps(kimiCode), s.Steps(kimi)
	sizes := stepSizes(other)
	budget := sizes[1] + sizes[2] + sizes[3]
	if n := stepSizes(own)[1]; n < 3000 || Cut(own, budget, textSize) != 2 {
		t.Fatalf("to its own model the second step weighs %d with its reasoning, over a budget of %d", n, budget)
	}
	if sizes[1] > 100 || Cut(other, budget, textSize) != 1 || other[1].First != u2 {
		t.Fatalf("to another model the second step weighs %d without its reasoning, and is in the tail", sizes[1])
	}
	if len(other[2].Messages) != 0 {
		t.Fatalf("the provider-executed step sends %q to another model, want nothing", messageTexts(other[2].Messages))
	}

	compact(t, s, 3, success("s", u2))
	want := []string{"user: <summary s>", "user: q2", "assistant: a2", "user: q3", "assistant: a3"}
	if got := messageTexts(s.Context(kimi)); !slices.Equal(got, want) {
		t.Fatalf("the tail to another model = %q, want %q", got, want)
	}
	want = []string{"user: <summary s>", "user: q2", "assistant: (thinking: " + thinking + ") a2", "user: q3", "assistant: a3"}
	if got := messageTexts(s.Context(kimiCode)); !slices.Equal(got, want) {
		t.Fatalf("the tail to its own model keeps the reasoning: %q", got)
	}
}

// TestAContextMayEndInTheSummary (A14): a compaction with no tail and nothing
// after it leaves a context of its summary message alone — a user message the
// trailing trim never removes, which a request then sends with no prompt of
// its own — while a held prompt is still not in it.
func TestAContextMayEndInTheSummary(t *testing.T) {
	s := newStore(t, compacted(testOptions(t)))
	turn(t, s, "q1", "a1", kimi)
	toolTurn(t, s, "q2", "call_a")
	id := compact(t, s, 2, success("all of it", ""))

	want := []string{"user: <summary all of it>"}
	msgs, marks := s.ContextWithResults(kimi)
	if got := messageTexts(msgs); !slices.Equal(got, want) || !slices.Equal(marks, []bool{true}) {
		t.Fatalf("context = %q (marks %v), want %q, marked", got, marks, want)
	}
	tr := s.Transcript()
	if got, err := tr.ContextAt(id, kimi); err != nil || !slices.Equal(messageTexts(got), want) {
		t.Fatalf("ContextAt(the compaction) = %q, %v", messageTexts(got), err)
	}
	if steps := s.Steps(kimi); len(steps) != 0 {
		t.Fatalf("the context has %d steps, want none", len(steps))
	}
	if err := s.AppendUser(withTurn(user("q3", kimi), 3)); err != nil {
		t.Fatal(err)
	}
	if got := messageTexts(s.Context(kimi)); !slices.Equal(got, want) {
		t.Fatalf("with a held prompt the context = %q, want %q", got, want)
	}
}

// TestACompactionIsWrittenAlone: AppendCompaction writes its one line at
// once, and the entries held for the next step — a change, a turn's prompt,
// and a reopened store's resume entry — stay held and go out with it, after
// the compaction and parented on it.
func TestACompactionIsWrittenAlone(t *testing.T) {
	opts := compacted(testOptions(t))
	s := newStore(t, opts)
	turn(t, s, "q1", "a1", kimi)
	if err := s.AppendModelChange(kimiCode); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUser(withTurn(user("q2", kimiCode), 2)); err != nil {
		t.Fatal(err)
	}
	cid := compact(t, s, 2, success("before q2", ""))
	if got, want := fileTypes(t, s.Path()), []string{"session", "message", "message", "compaction"}; !slices.Equal(got, want) {
		t.Fatalf("after the compaction the file's lines are %q, want %q", got, want)
	}
	ids, err := s.AppendStep(nil, answer("", "a2", kimiCode), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fileTypes(t, s.Path()), []string{"session", "message", "message", "compaction", "model_change", "message", "message"}; !slices.Equal(got, want) {
		t.Fatalf("after the step the file's lines are %q, want %q", got, want)
	}
	tr := s.Transcript()
	if e := tr.Entries[tr.index[ids[0]]]; e.Type != TypeModelChange || e.ParentID != cid {
		t.Fatalf("the step's first entry is %+v; want the held change, parented on the compaction %s", e, cid)
	}
	if got, want := messageTexts(s.Context(kimiCode)), []string{"user: <summary before q2>", "user: q2", "assistant: a2"}; !slices.Equal(got, want) {
		t.Fatalf("context = %q, want %q", got, want)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	ropts := opts
	ropts.SessionID = ""
	r := reopen(t, ropts, s.Path())
	cid = compact(t, r, 3, success("again", ""))
	if got := fileTypes(t, s.Path()); got[len(got)-1] != "compaction" || got[len(got)-2] != "message" {
		t.Fatalf("the reopened store's compaction was not written alone: %q", got)
	}
	turn(t, r, "q3", "a3", kimi)
	got := fileTypes(t, s.Path())
	if want := []string{"compaction", "resume", "message", "message"}; !slices.Equal(got[len(got)-4:], want) {
		t.Fatalf("the file ends %q, want %q: the resume entry held past the compaction", got[len(got)-4:], want)
	}
	tr = r.Transcript()
	if e := tr.Entries[len(tr.Entries)-3]; e.Type != TypeResume || e.ParentID != cid {
		t.Fatalf("the resume entry = %+v, want it parented on the compaction %s", e, cid)
	}
}

// TestAppendCompactionRefuses: a compaction that breaks its rule, whose tail
// is not a step before it, that would come between a call and its results
// (ErrUnpaired), or that has no conversation to stand for
// (ErrNothingToCompact) is refused with nothing written, and the store stays
// usable; after Close every append is ErrClosed.
func TestAppendCompactionRefuses(t *testing.T) {
	empty := newStore(t, compacted(testOptions(t)))
	if _, err := empty.AppendCompaction(1, kimi, compactUsage, success("s", "")); !errors.Is(err, ErrNothingToCompact) {
		t.Fatalf("an empty store's compaction = %v, want ErrNothingToCompact", err)
	}
	if _, err := os.Stat(empty.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused compaction created the transcript (stat %v)", err)
	}

	s := newStore(t, compacted(testOptions(t)))
	turn(t, s, "q1", "a1", kimi)
	if err := s.AppendUser(user("q2", kimi)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendStepLed([]Lead{reminderLead("ask"), steerLead("a steer", kimi)}, calls(kimi, "call_a"), results(kimi, "call_a")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	tr := s.Transcript()
	idOf := func(typ string, role fantasy.MessageRole, text string) string {
		for _, e := range tr.Entries {
			if e.Type == typ && (typ != TypeMessage || e.Message.Role == role && strings.Contains(messageTexts([]fantasy.Message{e.Message})[0], text)) {
				return e.ID
			}
		}
		t.Fatalf("no %s %s entry holding %q", typ, role, text)
		return ""
	}
	good := success("s", "")
	for name, tc := range map[string]struct {
		turn int
		m    Model
		u    Usage
		c    func(c *Compaction)
	}{
		"turn 0":                {0, kimi, compactUsage, nil},
		"no wire model":         {1, Model{Provider: "p", Alias: "a"}, compactUsage, nil},
		"negative usage":        {1, kimi, Usage{Input: -1}, nil},
		"negative tokens":       {1, kimi, compactUsage, func(c *Compaction) { c.TokensAfter = -1 }},
		"an unknown reason":     {1, kimi, compactUsage, func(c *Compaction) { c.Reason = "tidy" }},
		"no reason":             {1, kimi, compactUsage, func(c *Compaction) { c.Reason = "" }},
		"a command on an auto":  {1, kimi, compactUsage, func(c *Compaction) { c.Command = "/compact" }},
		"a summary and error":   {1, kimi, compactUsage, func(c *Compaction) { c.Error = "and failed" }},
		"neither":               {1, kimi, compactUsage, func(c *Compaction) { c.Summary = "" }},
		"a failure with a tail": {1, kimi, compactUsage, func(c *Compaction) { *c = failure("x"); c.FirstKeptID = tr.Entries[0].ID }},
		"a failure's segment":   {1, kimi, compactUsage, func(c *Compaction) { *c = failure("x"); c.Segment = "segment_001.md" }},
		"a segment path":        {1, kimi, compactUsage, func(c *Compaction) { c.Segment = "../segment_001.md" }},
		"a segment of ..":       {1, kimi, compactUsage, func(c *Compaction) { c.Segment = ".." }},
		"an unknown tail":       {1, kimi, compactUsage, func(c *Compaction) { c.FirstKeptID = "ffffffff" }},
		"a tail from the results": {1, kimi, compactUsage, func(c *Compaction) {
			c.FirstKeptID = idOf(TypeMessage, fantasy.MessageRoleTool, "call_a")
		}},
		"a tail from the calls": {1, kimi, compactUsage, func(c *Compaction) {
			c.FirstKeptID = idOf(TypeMessage, fantasy.MessageRoleAssistant, "call_a")
		}},
		"a tail from a reminder inside a step": {1, kimi, compactUsage, func(c *Compaction) { c.FirstKeptID = idOf(TypeReminder, "", "") }},
		"a tail from a steer inside a step": {1, kimi, compactUsage, func(c *Compaction) {
			c.FirstKeptID = idOf(TypeMessage, fantasy.MessageRoleUser, "a steer")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			c := good
			if tc.c != nil {
				tc.c(&c)
			}
			if _, err := s.AppendCompaction(tc.turn, tc.m, tc.u, c); err == nil {
				t.Fatal("accepted")
			}
			if got, _ := os.ReadFile(s.Path()); string(got) != string(before) {
				t.Fatal("a refused compaction wrote to the transcript")
			}
		})
	}

	// Between a call and its results: the store never holds that state, so
	// it is planted in memory, as a cut step would leave it before Open.
	openCall := s.Transcript().Entries
	s.mu.Lock()
	s.t.add(Entry{Type: TypeMessage, ID: "0000ffff", ParentID: openCall[len(openCall)-1].ID, Timestamp: fixedTime,
		MessageEntry: calls(kimi, "call_z")})
	s.mu.Unlock()
	if _, err := s.AppendCompaction(2, kimi, compactUsage, good); !errors.Is(err, ErrUnpaired) {
		t.Fatalf("a compaction between a call and its results = %v, want ErrUnpaired", err)
	}
	if got, _ := os.ReadFile(s.Path()); string(got) != string(before) {
		t.Fatal("the unpaired compaction wrote to the transcript")
	}

	// The controls: a tail from the root, and a manual one with its command.
	ctl := newStore(t, compacted(testOptions(t)))
	turn(t, ctl, "q1", "a1", kimi)
	first := ctl.Transcript().Entries[0].ID
	if _, err := ctl.AppendCompaction(1, kimi, compactUsage, success("s", first)); err != nil {
		t.Fatalf("control: %v", err)
	}
	manual := success("m", "")
	manual.Reason, manual.Command = CompactionManual, "/compact keep the API"
	if _, err := ctl.AppendCompaction(2, kimi, Usage{}, manual); err != nil {
		t.Fatalf("control, a manual one with a command and no usage spent: %v", err)
	}
	if err := ctl.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.AppendCompaction(2, kimi, compactUsage, good); !errors.Is(err, ErrClosed) {
		t.Fatalf("after Close = %v, want ErrClosed", err)
	}
}

// TestAFailedCompactionWriteIsAStepsFailure: the compaction's line goes out
// through a step's write, with a step's failures — a write that wrote part of
// it fails the store (ErrFailed) for good, and Load drops the torn line; one
// that wrote nothing fails only itself.
func TestAFailedCompactionWriteIsAStepsFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		empty  bool
		sticky bool
	}{
		{"part of the line", false, true},
		{"nothing", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &writeLog{failOn: 2, failEmpty: tc.empty}
			opts := compacted(testOptions(t))
			opts.openFile = log.opener()
			s := newStore(t, opts)
			turn(t, s, "q1", "a1", kimi) // write 1
			_, err := s.AppendCompaction(1, kimi, compactUsage, success("s", ""))
			if err == nil || errors.Is(err, ErrFailed) != tc.sticky {
				t.Fatalf("the failed compaction = %v; want an error, ErrFailed %v", err, tc.sticky)
			}
			if n := len(s.Transcript().Entries); n != 2 {
				t.Fatalf("the store holds %d entries after the failure, want 2", n)
			}
			_, err = s.AppendCompaction(1, kimi, compactUsage, success("s", ""))
			if errors.Is(err, ErrFailed) != tc.sticky || (!tc.sticky && err != nil) {
				t.Fatalf("the next append = %v; want ErrFailed %v", err, tc.sticky)
			}
			tr, err := Load(s.Path())
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if !tc.sticky {
				want = 3
			}
			if len(tr.Entries) != want {
				t.Fatalf("Load kept %d entries, want %d", len(tr.Entries), want)
			}
		})
	}
}

// TestCompactionEntrySurvivesLoadAtTheTail (A14, plan 028 §3.2): a compaction
// entry is complete on its own line, so a file ending in one — a success or a
// failure, with its newline or without — loads with it as the leaf, Open cuts
// nothing (it only ends the line), and the next step goes after it. A cut
// step after one goes and the compaction stays; a torn compaction line goes
// like any torn line.
func TestCompactionEntrySurvivesLoadAtTheTail(t *testing.T) {
	opts := compacted(testOptions(t))
	// written is a session of one tool turn and then c, closed: its path, the
	// compaction's id, and the file's bytes.
	written := func(t *testing.T, c Compaction) (string, string, []byte) {
		t.Helper()
		o := opts
		o.Home = t.TempDir()
		s := newStore(t, o)
		toolTurn(t, s, "q1", "call_a")
		id := compact(t, s, 1, c)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(s.Path())
		if err != nil {
			t.Fatal(err)
		}
		return s.Path(), id, raw
	}
	ropts := opts
	ropts.SessionID = ""

	for _, tc := range []struct {
		name    string
		c       Compaction
		newline bool
	}{
		{"a success", success("s", ""), true},
		{"a failure", failure("x"), true},
		{"a success with no newline", success("s", ""), false},
		{"a failure with no newline", failure("x"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, id, raw := written(t, tc.c)
			if !tc.newline {
				raw = raw[:len(raw)-1]
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tr, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if tr.Leaf() != id || len(tr.Entries) != 4 {
				t.Fatalf("Load kept %d entries ending at %s, want 4 ending at the compaction %s", len(tr.Entries), tr.Leaf(), id)
			}
			rec := &steps{}
			o := ropts
			o.fsStep = rec.seam
			r := reopen(t, o, path)
			got, _ := os.ReadFile(path)
			wantNames := []string(nil)
			if !tc.newline {
				wantNames = []string{stepNewline, stepSync}
			}
			if string(got) != string(raw)+map[bool]string{true: "", false: "\n"}[tc.newline] || !slices.Equal(rec.names, wantNames) || len(tornCopies(t, path)) != 0 {
				t.Fatalf("Open repaired %q and left the file\n%s\nwant nothing cut", rec.names, got)
			}
			turn(t, r, "q2", "a2", kimi)
			rt := r.Transcript()
			if e := rt.Entries[4]; e.Type != TypeResume || e.ParentID != id {
				t.Fatalf("the next step's first entry = %+v, want the resume entry parented on %s", e, id)
			}
		})
	}

	t.Run("a cut step after it", func(t *testing.T) {
		path, id, raw := written(t, success("s", ""))
		cut := userLine(t, "0000000a", id, "q2") + "\n" + callsLine(t, "0000000b", "0000000a", "call_b") + "\n"
		if err := os.WriteFile(path, append(append([]byte(nil), raw...), cut...), 0o600); err != nil {
			t.Fatal(err)
		}
		if tr, err := Load(path); err != nil || tr.Leaf() != id {
			t.Fatalf("Load = %v; want the cut step dropped and the compaction the leaf", err)
		}
		r := reopen(t, ropts, path)
		if got, _ := os.ReadFile(path); string(got) != string(raw) {
			t.Fatalf("Open left\n%s\nwant it cut back to the compaction", got)
		}
		if copies := tornCopies(t, path); len(copies) != 1 {
			t.Fatalf("copies %q, want one holding the cut step", copies)
		} else if got, _ := os.ReadFile(copies[0]); string(got) != cut {
			t.Fatalf("the copy holds %q, want %q", got, cut)
		}
		if got := messageTexts(r.Context(kimi)); !slices.Equal(got, []string{"user: <summary s>"}) {
			t.Fatalf("the reopened context = %q, want the summary alone", got)
		}
	})

	t.Run("a torn compaction line", func(t *testing.T) {
		path, id, raw := written(t, success("s", ""))
		i := strings.LastIndex(strings.TrimSuffix(string(raw), "\n"), "\n") + 1
		torn := raw[:i+(len(raw)-i)/2]
		if err := os.WriteFile(path, torn, 0o600); err != nil {
			t.Fatal(err)
		}
		tr, err := Load(path)
		if err != nil || tr.has(id) || len(tr.Entries) != 3 {
			t.Fatalf("Load = %v; want the torn compaction dropped", err)
		}
		reopen(t, ropts, path)
		if got, _ := os.ReadFile(path); string(got) != string(raw[:i]) || len(tornCopies(t, path)) != 1 {
			t.Fatalf("Open left\n%s\nwant the torn line cut and kept aside", got)
		}
	})
}

// TestABadFirstKeptIdOnTheLastLineIsCorrupt (A14, P14): a compaction's line is
// checked outside the torn tail. Its tail must start a step before it on its
// path, and its fields must have their shapes; a whole last line that breaks
// either — with its newline or without — is ErrCorrupt, never dropped as torn,
// by Load and by Open alike, and Open leaves the file as it was. The
// controls: good lines load, and the same bad line cut in half is a torn tail.
func TestABadFirstKeptIdOnTheLastLineIsCorrupt(t *testing.T) {
	// 1 q1, 2 calls, 3 results | 4 compaction (no tail) | 5 q2, 6 a2 | on
	// another branch from 3: 7 q3, 8 a3 | 9 q4 (after 6), 10 a4.
	base := []string{
		headerText(t),
		userLine(t, "00000001", "", "q1"),
		callsLine(t, "00000002", "00000001", "call_a"),
		resultsLine(t, "00000003", "00000002", "call_a"),
		compactionText(t, "00000004", "00000003", `"summary":"s","reason":"auto"`),
		userLine(t, "00000005", "00000004", "q2"),
		replyLine(t, "00000006", "00000005", "", "a2", kimi),
		userLine(t, "00000007", "00000003", "q3"),
		replyLine(t, "00000008", "00000007", "", "a3", kimi),
		userLine(t, "00000009", "00000006", "q4"),
		replyLine(t, "0000000a", "00000009", "", "a4", kimi),
	}
	last := func(fields string) string { return compactionText(t, "0000000b", "0000000a", fields) }
	tail := func(id string) string { return `"summary":"s","firstKeptId":"` + id + `","reason":"auto"` }

	for name, fields := range map[string]string{
		"the root":                          tail("00000001"),
		"a step after a compaction":         tail("00000005"),
		"a step after an answer":            tail("00000009"),
		"no tail":                           `"summary":"s","reason":"auto"`,
		"a failure":                         `"error":"x","reason":"auto"`,
		"a manual one":                      `"summary":"s","reason":"manual","command":"/compact"`,
		"an unknown field of a newer craze": `"summary":"s","reason":"auto","priority":7`,
	} {
		t.Run("control: "+name, func(t *testing.T) {
			tr, err := Load(writeFile(t, lines(append(slices.Clone(base), last(fields))...)))
			if err != nil || tr.Leaf() != "0000000b" {
				t.Fatalf("Load = %v; want the line kept", err)
			}
		})
	}

	for name, fields := range map[string]string{
		"a tail from no entry":            tail("ffffffff"),
		"a tail from itself":              tail("0000000b"),
		"a tail on another branch":        tail("00000007"),
		"a tail from the results":         tail("00000003"),
		"a tail from the calls":           tail("00000002"),
		"a tail from an answer":           tail("0000000a"),
		"a tail from a compaction":        tail("00000004"),
		"a tail that is a number":         `"summary":"s","firstKeptId":4,"reason":"auto"`,
		"a failure with a tail":           `"error":"x","firstKeptId":"00000005","reason":"auto"`,
		"a failure with a segment":        `"error":"x","segment":"segment_001.md","reason":"auto"`,
		"a summary and an error":          `"summary":"s","error":"x","reason":"auto"`,
		"neither a summary nor an error":  `"reason":"auto"`,
		"a summary that is not a string":  `"summary":["s"],"reason":"auto"`,
		"a null error":                    `"summary":"s","error":null,"reason":"auto"`,
		"an unknown reason":               `"summary":"s","reason":"tidy"`,
		"no reason":                       `"summary":"s"`,
		"a command on an automatic one":   `"summary":"s","reason":"auto","command":"/compact"`,
		"a segment path":                  `"summary":"s","reason":"auto","segment":"../x.md"`,
		"a turn of 0":                     `"summary":"s","reason":"auto"` + `,"turn":0`,
		"a turn that is a string":         `"summary":"s","reason":"auto"` + `,"turn":"2"`,
		"negative tokens":                 `"summary":"s","reason":"auto"` + `,"tokensAfter":-1`,
		"tokens that are not integers":    `"summary":"s","reason":"auto"` + `,"tokensBefore":1.5`,
		"usage that is not an object":     `"summary":"s","reason":"auto"` + `,"usage":7`,
		"a null usage":                    `"summary":"s","reason":"auto"` + `,"usage":null`,
		"a usage count that is a string":  `"summary":"s","reason":"auto"` + `,"usage":{"input":"12"}`,
		"negative usage":                  `"summary":"s","reason":"auto"` + `,"usage":{"output":-1}`,
		"no provider":                     `"summary":"s","reason":"auto"` + `,"provider":""`,
		"a model that is not a string":    `"summary":"s","reason":"auto"` + `,"model":1`,
		"no usage, turn or tokens at all": "",
	} {
		for _, newline := range []bool{true, false} {
			t.Run(name+map[bool]string{true: "", false: ", no newline"}[newline], func(t *testing.T) {
				line := last(fields)
				if fields == "" {
					line = `{"type":"compaction","id":"0000000b","parentId":"0000000a","timestamp":"2026-09-18T12:00:00.000Z","summary":"s","reason":"auto"}`
				}
				raw := lines(append(slices.Clone(base), line)...)
				if !newline {
					raw = strings.TrimSuffix(raw, "\n")
				}
				path := writeFile(t, raw)
				if _, err := Load(path); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "invalid entry") {
					t.Fatalf("Load = %v, want ErrCorrupt for an invalid entry", err)
				}
				opts := testOptions(t)
				opts.SessionID = ""
				if _, err := Open(opts, path); !errors.Is(err, ErrCorrupt) {
					t.Fatalf("Open = %v, want ErrCorrupt", err)
				}
				if got, _ := os.ReadFile(path); string(got) != raw || len(tornCopies(t, path)) != 0 {
					t.Fatal("a refused Open changed the file")
				}
				if tr, err := Load(writeFile(t, lines(base...)+line[:len(line)/2])); err != nil || tr.Leaf() != "0000000a" {
					t.Fatalf("control: Load of the line torn in half = %v", err)
				}
			})
		}
	}
}

// compactionText is a compaction's line with the given fields; the ones it
// needs and fields does not set are a good compaction's (turn 1, the tokens,
// kimi, compactUsage). A later key in fields replaces an earlier default, as
// encoding/json reads a repeated key.
func compactionText(t *testing.T, id, parent, fields string) string {
	t.Helper()
	return `{"type":"compaction","id":"` + id + `","parentId":"` + parent + `","timestamp":"2026-09-18T12:00:00.000Z",` +
		`"turn":1,"tokensBefore":90000,"tokensAfter":21000,` +
		`"provider":"fireworks","model":"fireworks/kimi-k3","wire_model":"accounts/fireworks/models/kimi-k3",` +
		`"usage":{"input":12,"output":34,"reasoning":5,"cache_read":6789,"cache_creation":1},` + fields + `}`
}
