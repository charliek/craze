package store

import (
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/tool"
)

// These tests cover H7's additions to the entries (plan 028 §3.2): turn on
// the user entry a turn opens with, todos on a tool entry, the resume entry,
// and the rule that a whole line breaking them is corrupt, never torn (P14).

// withTurn is e with turn n.
func withTurn(e MessageEntry, n int) MessageEntry {
	e.Turn = n
	return e
}

// withTodos is the tool entry e carrying list.
func withTodos(e *MessageEntry, list []Todo) *MessageEntry {
	e.Todos = &list
	return e
}

// TestTurnAndTodosRoundTrip: a turn and a todo list read back as written,
// and "no todos" (nil) stays apart from "the list was emptied" (a pointer to
// an empty list), which is written as [] and never as null. An entry with
// neither is written without either key, byte for byte as before H7.
func TestTurnAndTodosRoundTrip(t *testing.T) {
	s := newStore(t, testOptions(t))
	if err := s.AppendUser(withTurn(user("plan the work", kimi), 1)); err != nil {
		t.Fatal(err)
	}
	list := []Todo{{ID: "read", Content: "read the notes", Status: "completed"}, {ID: "fix", Content: "fix the bug", Status: "in_progress"}}
	step(t, s, nil, calls(kimi, "call_a"), withTodos(results(kimi, "call_a"), list))
	step(t, s, nil, calls(kimi, "call_b"), results(kimi, "call_b"))                      // no todos
	step(t, s, nil, calls(kimi, "call_c"), withTodos(results(kimi, "call_c"), []Todo{})) // emptied
	step(t, s, nil, calls(kimi, "call_d"), withTodos(results(kimi, "call_d"), nil))      // emptied, nil slice
	step(t, s, nil, answer("", "done", kimi), nil)

	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	e := tr.Entries
	if e[0].Turn != 1 {
		t.Fatalf("the user entry's turn read back as %d, want 1", e[0].Turn)
	}
	for i, want := range map[int]*[]Todo{2: &list, 4: nil, 6: {}, 8: {}} {
		got := e[i].Todos
		if (got == nil) != (want == nil) || (got != nil && !reflect.DeepEqual(*got, *want)) {
			t.Errorf("entry %d's todos read back as %v, want %v", i, got, want)
		}
	}
	for _, i := range []int{1, 3, 5, 7, 9} {
		if e[i].Turn != 0 || e[i].Todos != nil {
			t.Errorf("entry %d (%s) grew a turn or todos: %+v", i, e[i].Message.Role, e[i].MessageEntry)
		}
	}

	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	fileLines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if !strings.HasSuffix(fileLines[1], `,"turn":1}`) {
		t.Fatalf("the user entry's line does not end with its turn: %s", fileLines[1])
	}
	for i, want := range map[int]string{
		3: `,"todos":[{"id":"read","content":"read the notes","status":"completed"},{"id":"fix","content":"fix the bug","status":"in_progress"}]}`,
		7: `,"todos":[]}`, 9: `,"todos":[]}`,
	} {
		if !strings.HasSuffix(fileLines[i], want) {
			t.Errorf("line %d does not end with %s: %s", i+1, want, fileLines[i])
		}
	}
	for _, i := range []int{2, 4, 5, 6, 8, 10} {
		if strings.Contains(fileLines[i], `"turn"`) || strings.Contains(fileLines[i], `"todos"`) {
			t.Errorf("line %d carries a key it has no value for: %s", i+1, fileLines[i])
		}
	}
}

// TestFieldsAreCheckedOnTheWayIn: a turn or todos where the store's rules put
// none, or todos of the wrong shape, are the caller's bug and refused with
// nothing written; so is a steer carrying a turn, since only the entry a turn
// opens with records one. The controls are the same calls, well formed.
func TestFieldsAreCheckedOnTheWayIn(t *testing.T) {
	s := newStore(t, testOptions(t))
	good := []Todo{{ID: "a", Content: "x", Status: "pending"}}
	for name, err := range map[string]error{
		"a negative turn":     s.AppendUser(withTurn(user("q", kimi), -1)),
		"a turn on an answer": s.AppendAssistant(withTurn(answer("", "a", kimi), 1)),
		"a turn on the tool":  stepErr(s, nil, calls(kimi, "c"), &MessageEntry{Message: resultsMsg("c"), Model: kimi, Turn: 1}),
		"a turn on a steer":   stepErr(s, []MessageEntry{withTurn(user("steer", kimi), 2)}, answer("", "a", kimi), nil),
		"todos on an answer":  stepErr(s, nil, MessageEntry{Message: assistantMsg("", "a"), Model: kimi, Todos: &good}, nil),
		"todos on a user":     s.AppendUser(MessageEntry{Message: user("q", kimi).Message, Model: kimi, Todos: &good}),
		"a todo with no id":   stepErr(s, nil, calls(kimi, "c"), withTodos(results(kimi, "c"), []Todo{{Content: "x", Status: "pending"}})),
		"a repeated todo id":  stepErr(s, nil, calls(kimi, "c"), withTodos(results(kimi, "c"), []Todo{{ID: "a", Status: "pending"}, {ID: "a", Status: "completed"}})),
		"an unknown status":   stepErr(s, nil, calls(kimi, "c"), withTodos(results(kimi, "c"), []Todo{{ID: "a", Status: "blocked"}})),
		"an empty status":     stepErr(s, nil, calls(kimi, "c"), withTodos(results(kimi, "c"), []Todo{{ID: "a"}})),
		"AppendAnswer's steer": func() error {
			_, err := s.AppendAnswer([]MessageEntry{withTurn(user("s", kimi), 3)}, answer("", "a", kimi))
			return err
		}(),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := os.Stat(s.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused call wrote the transcript (stat err %v)", err)
	}
	// The controls.
	if err := s.AppendUser(withTurn(user("q", kimi), 1)); err != nil {
		t.Fatal(err)
	}
	step(t, s, []MessageEntry{user("steer", kimi)}, calls(kimi, "c"), withTodos(results(kimi, "c"), good))
}

func stepErr(s *Store, steers []MessageEntry, a MessageEntry, tool *MessageEntry) error {
	_, err := s.AppendStep(steers, a, tool)
	return err
}

// TestAWholeLineBreakingTheFieldsIsCorrupt (P14): a turn or todos of the
// wrong shape can only be in a whole line — a torn append leaves a prefix
// of one, which does not decode — so such a line is ErrCorrupt even as the
// last line, never forgiven as a torn tail, by Load and by Open alike, and
// Open leaves the file as it was. The control is each line cut in half,
// which is a torn tail: Load drops it.
func TestAWholeLineBreakingTheFieldsIsCorrupt(t *testing.T) {
	u1 := userLine(t, "00000001", "", "q1")
	a1 := replyLine(t, "00000002", "00000001", "", "a1", kimi)
	c1 := msgLine(t, "00000002", "00000001", callsMsg("", "", "c"), kimi)
	tool := msgLine(t, "00000003", "00000002", resultsMsg("c"), kimi)
	u2 := userLine(t, "00000003", "00000002", "q2")
	withKey := func(line, key string) string { return strings.TrimSuffix(line, "}") + "," + key + "}" }
	for name, tc := range map[string]struct{ before, last string }{
		"todos with no id":        {u1 + "\n" + c1, withKey(tool, `"todos":[{"content":"x","status":"pending"}]`)},
		"todos with a bad status": {u1 + "\n" + c1, withKey(tool, `"todos":[{"id":"a","content":"x","status":"blocked"}]`)},
		"a repeated todo id":      {u1 + "\n" + c1, withKey(tool, `"todos":[{"id":"a","status":"pending"},{"id":"a","status":"pending"}]`)},
		"todos not a list":        {u1 + "\n" + c1, withKey(tool, `"todos":"none"`)},
		"todos a list of numbers": {u1 + "\n" + c1, withKey(tool, `"todos":[1,2]`)},
		"todos null":              {u1 + "\n" + c1, withKey(tool, `"todos":null`)},
		"todos on an answer":      {u1, withKey(a1, `"todos":[]`)},
		"a turn on an answer":     {u1, withKey(a1, `"turn":1`)},
		"a negative turn":         {u1 + "\n" + a1, withKey(u2, `"turn":-2`)},
		"a turn that is a string": {u1 + "\n" + a1, withKey(u2, `"turn":"2"`)},
		"a fractional turn":       {u1 + "\n" + a1, withKey(u2, `"turn":1.5`)},
	} {
		t.Run(name, func(t *testing.T) {
			// The control first: the same file with the key's line whole
			// but without the key loads.
			if _, err := Load(writeFile(t, lines(headerText(t), tc.before, tc.last[:strings.LastIndex(tc.last, `,"t`)]+"}"))); err != nil {
				t.Fatalf("control: the line without the key: %v", err)
			}
			raw := lines(headerText(t), tc.before, tc.last)
			path := writeFile(t, raw)
			if _, err := Load(path); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "invalid entry") {
				t.Fatalf("Load = %v, want ErrCorrupt for an invalid entry", err)
			}
			if _, err := Open(testOptions(t), path); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Open = %v, want ErrCorrupt", err)
			}
			if got, _ := os.ReadFile(path); string(got) != raw {
				t.Fatal("a refused Open changed the file")
			}
			// The control: the same line torn is a torn tail.
			torn := lines(headerText(t), tc.before) + tc.last[:len(tc.last)/2]
			if _, err := Load(writeFile(t, torn)); err != nil {
				t.Fatalf("control: Load of the line torn in half = %v", err)
			}
		})
	}
}

// TestResumeEntryRoundTrips: a resume entry's line carries the contract
// under the header's own key names, the two optional ones omitted when
// empty, reads back whole, and is a known type.
func TestResumeEntryRoundTrips(t *testing.T) {
	for _, c := range []Contract{
		{CrazeVersion: "v1.2.3", SystemPromptSHA256: "abc", ToolProfile: "opencode", ToolsSHA256: "def"},
		{CrazeVersion: "v1.2.3", SystemPromptSHA256: "abc"},
	} {
		e := Entry{Type: TypeResume, ID: "0000000a", ParentID: "00000009", Timestamp: fixedTime, Contract: c}
		line, err := encodeEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"type":"resume","id":"0000000a","parentId":"00000009","timestamp":"2026-09-18T12:00:00.000Z","craze_version":"v1.2.3","system_prompt_sha256":"abc"`
		if c.ToolProfile != "" {
			want += `,"tool_profile":"opencode","tools_sha256":"def"`
		}
		if want += "}"; string(line) != want {
			t.Fatalf("resume line =\n%s\nwant\n%s", line, want)
		}
		back, err := decodeEntry(line)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, e) {
			t.Fatalf("resume entry read back as %+v, want %+v", back, e)
		}
	}
	if !knownType(TypeResume) || knownType("compaction") {
		t.Fatal("knownType does not know resume, or knows a type this craze does not write")
	}
}

// TestTodoStatusesMatchTheTool: the store checks a stored list against the
// four statuses todo_write's schema promises, which the harness's tool
// package owns; the two lists must be the same four.
func TestTodoStatusesMatchTheTool(t *testing.T) {
	var fromTool []string
	for _, st := range []tool.TodoStatus{tool.TodoPending, tool.TodoInProgress, tool.TodoCompleted, tool.TodoCancelled} {
		if !st.Valid() {
			t.Fatalf("control: tool status %q is not valid", st)
		}
		fromTool = append(fromTool, string(st))
	}
	var stored []string
	for st := range todoStatuses {
		stored = append(stored, st)
		if !tool.TodoStatus(st).Valid() {
			t.Errorf("the store accepts status %q, which the tool does not", st)
		}
	}
	slices.Sort(fromTool)
	slices.Sort(stored)
	if !slices.Equal(fromTool, stored) {
		t.Fatalf("the store's statuses %q, the tool's %q", stored, fromTool)
	}
}

// TestBranchWalksThePath: Branch is the entries from the root to a leaf by
// parentId, not file order, and says so for an unknown leaf; the store's
// Transcript is a copy later appends do not reach.
func TestBranchWalksThePath(t *testing.T) {
	tr := mustLoad(t, lines(
		headerText(t),
		userLine(t, "00000001", "", "q1"),
		replyLine(t, "00000002", "00000001", "", "a1", kimi),
		userLine(t, "00000003", "00000002", "q2 (branch A)"),
		replyLine(t, "00000004", "00000003", "", "a2 (branch A)", kimi),
		userLine(t, "00000005", "00000002", "q2 (branch B)"),
		replyLine(t, "00000006", "00000005", "", "a2 (branch B)", kimi),
	))
	for leaf, want := range map[string][]string{
		"00000006": {"00000001", "00000002", "00000005", "00000006"},
		"00000004": {"00000001", "00000002", "00000003", "00000004"},
		"":         nil,
	} {
		got, err := tr.Branch(leaf)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(entryIDs(got), want) {
			t.Errorf("Branch(%q) = %q, want %q", leaf, entryIDs(got), want)
		}
	}
	if _, err := tr.Branch("0000beef"); !errors.Is(err, ErrUnknownEntry) {
		t.Fatalf("Branch(unknown) = %v, want ErrUnknownEntry", err)
	}

	s := newStore(t, testOptions(t))
	turn(t, s, "q1", "a1", kimi)
	snap := s.Transcript()
	turn(t, s, "q2", "a2", kimi)
	if n := len(snap.Entries); n != 2 {
		t.Fatalf("the copy has %d entries after a later append, want 2", n)
	}
	if got, _ := snap.Branch(snap.Leaf()); len(got) != 2 {
		t.Fatalf("the copy's branch has %d entries, want 2", len(got))
	}
	if got, _ := s.Transcript().Branch(s.Transcript().Leaf()); len(got) != 4 {
		t.Fatalf("a fresh copy's branch has %d entries, want 4", len(got))
	}
}

// TestACopySharesNothingMutable (astra r1-c0c1): the store's Transcript, and
// a Branch of any transcript, are copies a caller can write through — the
// todo list and its items, the usage, the sub-agent usage rows, a message's
// parts list — without reaching what they were copied from: the store's next
// copy, and the transcript the branch was taken from, read as before. Every
// field is compared by the entry's line, which holds them all.
func TestACopySharesNothingMutable(t *testing.T) {
	s := newStore(t, testOptions(t))
	if err := s.AppendUser(withTurn(user("q", kimi), 1)); err != nil {
		t.Fatal(err)
	}
	a := calls(kimi, "call_a")
	a.Usage = &Usage{Input: 10, Output: 5}
	tool := withTodos(results(kimi, "call_a"), []Todo{{ID: "read", Content: "read the notes", Status: "pending"}})
	tool.SubagentUsage = []ModelUsage{{Provider: "test", Model: "test/b", WireModel: "wire-b", Usage: Usage{Input: 1}}}
	step(t, s, nil, a, tool)
	step(t, s, nil, answer("", "done", kimi), nil)

	lines := func(es []Entry) []string {
		t.Helper()
		out := make([]string, len(es))
		for i, e := range es {
			b, err := encodeEntry(e)
			if err != nil {
				t.Fatal(err)
			}
			out[i] = string(b)
		}
		return out
	}
	scribble := func(es []Entry) {
		for i := range es {
			e := &es[i]
			if e.Todos != nil {
				(*e.Todos)[0].Status = "cancelled"
				*e.Todos = append(*e.Todos, Todo{ID: "added", Content: "x", Status: "pending"})
			}
			if e.Usage != nil {
				e.Usage.Input = 999
			}
			for j := range e.SubagentUsage {
				e.SubagentUsage[j].Usage.Input = 999
			}
			if len(e.Message.Content) > 0 {
				e.Message.Content[0] = fantasy.TextPart{Text: "rewritten"}
			}
		}
	}

	want := lines(s.Transcript().Entries)
	scribble(s.Transcript().Entries)
	if got := lines(s.Transcript().Entries); !slices.Equal(got, want) {
		t.Fatalf("a write through Transcript's copy reached the store:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	tr := s.Transcript()
	branch, err := tr.Branch(tr.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	scribble(branch)
	if got := lines(tr.Entries); !slices.Equal(got, want) {
		t.Fatalf("a write through a Branch reached its transcript:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := lines(s.Transcript().Entries); !slices.Equal(got, want) {
		t.Fatal("a write through a Branch reached the store")
	}
}
