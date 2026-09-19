package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

// These tests build files line by line, so each can say exactly what is on
// disk: a tree with a branch, a torn tail, damage in the middle.

var fixedTime = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

func headerText(t *testing.T) string {
	t.Helper()
	b, err := encodeHeader(Header{Version: FormatVersion, ID: "sid", Timestamp: fixedTime, Cwd: testWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// msgLine is a message entry's line; parent "" is a root.
func msgLine(t *testing.T, id, parent string, m fantasy.Message, model Model) string {
	t.Helper()
	b, err := encodeEntry(Entry{
		Type: TypeMessage, ID: id, ParentID: parent, Timestamp: fixedTime,
		MessageEntry: MessageEntry{Message: m, Model: model},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func userLine(t *testing.T, id, parent, text string) string {
	return msgLine(t, id, parent, fantasy.NewUserMessage(text), kimi)
}

func replyLine(t *testing.T, id, parent, reasoning, text string, model Model) string {
	return msgLine(t, id, parent, assistantMsg(reasoning, text), model)
}

// writeFile writes raw as a session file and returns its path.
func writeFile(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// lines joins lines into a file body, each ending in a newline.
func lines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

func mustLoad(t *testing.T, raw string) *Transcript {
	t.Helper()
	tr, err := Load(writeFile(t, raw))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return tr
}

func TestLoadRequiresAHeader(t *testing.T) {
	good := headerText(t)
	for name, raw := range map[string]string{
		"empty file":           "",
		"not JSON":             lines("hello"),
		"torn header":          good[:len(good)/2],
		"entry first":          lines(userLine(t, "00000001", "", "q"), good),
		"future version":       lines(strings.Replace(good, `"version":1`, `"version":2`, 1)),
		"no session id":        lines(strings.Replace(good, `"id":"sid"`, `"id":""`, 1)),
		"unparseable time":     lines(strings.Replace(good, `"timestamp":"2026-09-18T12:00:00.000Z"`, `"timestamp":"noon"`, 1)),
		"header then messages": lines("{}", userLine(t, "00000001", "", "q")),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeFile(t, raw))
			if !errors.Is(err, ErrNoHeader) {
				t.Fatalf("Load = %v, want ErrNoHeader", err)
			}
		})
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load of a missing file = %v, want fs.ErrNotExist", err)
	}
}

// TestLoadSkipsATornLastLine: a crash mid-append leaves a prefix of the last
// write, or, on some filesystems, zero bytes where it should be. Only the
// last line can be torn that way, and it is dropped.
func TestLoadSkipsATornLastLine(t *testing.T) {
	u1 := userLine(t, "00000001", "", "q1")
	a1 := replyLine(t, "00000002", "00000001", "", "a1", kimi)
	u2 := userLine(t, "00000003", "00000002", "q2")
	for name, raw := range map[string]string{
		"a prefix of a line":              lines(headerText(t), u1, a1) + u2[:len(u2)/2],
		"zero bytes":                      lines(headerText(t), u1, a1) + "\x00\x00\x00\x00",
		"a newline-ended garbage":         lines(headerText(t), u1, a1, "{not json"),
		"a whole last line, unterminated": lines(headerText(t), u1) + a1,
	} {
		t.Run(name, func(t *testing.T) {
			tr := mustLoad(t, raw)
			if got := messageTexts(tr.Context(kimi)); !reflect.DeepEqual(got, []string{"user: q1", "assistant: a1"}) {
				t.Fatalf("context = %q", got)
			}
		})
	}
}

// TestLoadRefusesDamageBeforeTheLastLine: a torn append can only damage the
// tail, so a bad line anywhere else is corruption, and the file is refused
// rather than read around.
func TestLoadRefusesDamageBeforeTheLastLine(t *testing.T) {
	u1 := userLine(t, "00000001", "", "q1")
	a1 := replyLine(t, "00000002", "00000001", "", "a1", kimi)
	for name, tc := range map[string]struct {
		raw  string
		line string
	}{
		"not JSON":       {lines(headerText(t), u1, "{not json", a1), "line 3"},
		"blank line":     {lines(headerText(t), u1, "", a1), "line 3"},
		"repeated id":    {lines(headerText(t), u1, userLine(t, "00000001", "", "again"), a1), "line 3"},
		"unknown parent": {lines(headerText(t), replyLine(t, "00000009", "0000000f", "", "orphan", kimi), u1, a1), "line 2"},
		"parent later":   {lines(headerText(t), replyLine(t, "00000002", "00000001", "", "a1", kimi), u1, a1), "line 2"},
		"no id":          {lines(headerText(t), strings.Replace(u1, `"id":"00000001"`, `"id":""`, 1), a1), "line 2"},
		"message that does not decode": {
			lines(headerText(t), strings.Replace(u1, `"type":"text"`, `"type":"hologram"`, 1), a1), "line 2",
		},
		"empty parent": {lines(headerText(t), strings.Replace(u1, `"parentId":null`, `"parentId":""`, 1), a1), "line 2"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeFile(t, tc.raw))
			if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), tc.line) {
				t.Fatalf("Load = %v, want ErrCorrupt at %s", err, tc.line)
			}
		})
	}
}

// TestLoadKeepsUnknownEntryTypes: a newer craze's entry types stay in the
// tree, so a parent chain through one is not broken, and never reach the
// context.
func TestLoadKeepsUnknownEntryTypes(t *testing.T) {
	label := `{"type":"label","id":"0000000a","parentId":"00000001","timestamp":"2026-09-18T12:00:00.000Z","text":"checkpoint"}`
	tr := mustLoad(t, lines(
		headerText(t),
		userLine(t, "00000001", "", "q1"),
		label,
		replyLine(t, "00000002", "0000000a", "", "a1", kimi),
	))
	if tr.Entries[1].Type != "label" {
		t.Fatalf("entry 2 is %q, want the label kept", tr.Entries[1].Type)
	}
	if got := messageTexts(tr.Context(kimi)); !reflect.DeepEqual(got, []string{"user: q1", "assistant: a1"}) {
		t.Fatalf("context = %q", got)
	}
}

// TestContextWalksLeafToRoot builds a tree with two branches off the first
// answer and shows the context follows parentId from the leaf, not file
// order: the default leaf is the last line, and an explicit leaf picks its
// own branch.
func TestContextWalksLeafToRoot(t *testing.T) {
	tr := mustLoad(t, lines(
		headerText(t),
		userLine(t, "00000001", "", "q1"),
		replyLine(t, "00000002", "00000001", "", "a1", kimi),
		userLine(t, "00000003", "00000002", "q2 (branch A)"),
		replyLine(t, "00000004", "00000003", "", "a2 (branch A)", kimi),
		`{"type":"model_change","id":"00000005","parentId":"00000002","timestamp":"2026-09-18T12:00:00.000Z","provider":"fireworks","model":"fireworks/kimi-k3","wire_model":"accounts/fireworks/models/kimi-k3"}`,
		userLine(t, "00000006", "00000005", "q2 (branch B)"),
		replyLine(t, "00000007", "00000006", "", "a2 (branch B)", kimi),
	))
	if tr.Leaf() != "00000007" {
		t.Fatalf("Leaf = %q, want the last entry", tr.Leaf())
	}
	branchB := []string{"user: q1", "assistant: a1", "user: q2 (branch B)", "assistant: a2 (branch B)"}
	if got := messageTexts(tr.Context(kimi)); !reflect.DeepEqual(got, branchB) {
		t.Fatalf("Context = %q, want %q", got, branchB)
	}
	got, err := tr.ContextAt("00000004", kimi)
	if err != nil {
		t.Fatal(err)
	}
	branchA := []string{"user: q1", "assistant: a1", "user: q2 (branch A)", "assistant: a2 (branch A)"}
	if texts := messageTexts(got); !reflect.DeepEqual(texts, branchA) {
		t.Fatalf("ContextAt(branch A) = %q, want %q", texts, branchA)
	}
	if _, err := tr.ContextAt("0000beef", kimi); !errors.Is(err, ErrUnknownEntry) {
		t.Fatalf("ContextAt(unknown) = %v, want ErrUnknownEntry", err)
	}
	if got := mustLoad(t, lines(headerText(t))).Context(kimi); len(got) != 0 {
		t.Fatalf("an empty transcript's context = %v", got)
	}
}

// TestLoadDropsAnIncompleteTurn: a crash can persist any prefix of a turn's
// one write, including one that ends on a line boundary. Whatever follows
// the last assistant message is that incomplete turn, and Load drops it from
// the entries, not just from the context.
func TestLoadDropsAnIncompleteTurn(t *testing.T) {
	u1 := userLine(t, "00000001", "", "q1")
	a1 := replyLine(t, "00000002", "00000001", "", "a1", kimi)
	mc := `{"type":"model_change","id":"00000003","parentId":"00000002","timestamp":"2026-09-18T12:00:00.000Z","provider":"openrouter","model":"openrouter/minimax-m3","wire_model":"minimax/minimax-m3"}`
	ec := `{"type":"effort_change","id":"00000004","parentId":"00000003","timestamp":"2026-09-18T12:00:00.000Z","effort":"high"}`
	u2 := userLine(t, "00000005", "00000004", "q2")
	u2direct := userLine(t, "00000005", "00000002", "q2")
	answered := []string{"user: q1", "assistant: a1"}
	for name, tc := range map[string]struct {
		raw  string
		want []string // the kept entries' messages, which are also the context
	}{
		"header only": {lines(headerText(t)), nil},
		// The first write, torn after the header and the user entry.
		"header and user only":            {lines(headerText(t), u1), nil},
		"a later unanswered user":         {lines(headerText(t), u1, a1, u2direct), answered},
		"a change with no turn after it":  {lines(headerText(t), u1, a1, mc), answered},
		"both changes":                    {lines(headerText(t), u1, a1, mc, ec), answered},
		"changes and the user, no answer": {lines(headerText(t), u1, a1, mc, ec, u2), answered},
		"the same, the answer torn":       {lines(headerText(t), u1, a1, mc, ec, u2) + `{"type":"mess`, answered},
	} {
		t.Run(name, func(t *testing.T) {
			tr := mustLoad(t, tc.raw)
			var msgs []fantasy.Message
			for _, e := range tr.Entries {
				if e.Type != TypeMessage {
					t.Fatalf("a %s entry was kept after the last answer", e.Type)
				}
				msgs = append(msgs, e.Message)
			}
			if got := messageTexts(msgs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("entries = %q, want %q", got, tc.want)
			}
			if got := messageTexts(tr.Context(kimi)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("context = %q, want %q", got, tc.want)
			}
			if tc.want != nil && tr.Leaf() != "00000002" {
				t.Fatalf("Leaf = %q, want the last answer", tr.Leaf())
			}
			dropped := []string{"00000003", "00000004", "00000005"}
			if tc.want == nil {
				dropped = append(dropped, "00000001")
			}
			for _, id := range dropped {
				if _, err := tr.ContextAt(id, kimi); !errors.Is(err, ErrUnknownEntry) {
					t.Fatalf("ContextAt(%s) = %v; a dropped entry must be unknown", id, err)
				}
			}
		})
	}
}

// TestContextAtAUserLeafEndsBeforeIt: ContextAt from a leaf that is a user
// message returns the history before it, never the unanswered prompt.
func TestContextAtAUserLeafEndsBeforeIt(t *testing.T) {
	tr := mustLoad(t, lines(
		headerText(t),
		userLine(t, "00000001", "", "q1"),
		replyLine(t, "00000002", "00000001", "", "a1", kimi),
		userLine(t, "00000003", "00000002", "q2"),
		replyLine(t, "00000004", "00000003", "", "a2", kimi),
	))
	got, err := tr.ContextAt("00000003", kimi)
	if err != nil {
		t.Fatal(err)
	}
	if texts := messageTexts(got); !reflect.DeepEqual(texts, []string{"user: q1", "assistant: a1"}) {
		t.Fatalf("ContextAt(a user leaf) = %q", texts)
	}
}

// TestReasoningFilter: reasoning is replayed only to the model that produced
// it — same provider and same wire model — and the filter works on a copy.
func TestReasoningFilter(t *testing.T) {
	raw := lines(
		headerText(t),
		userLine(t, "00000001", "", "q1"),
		replyLine(t, "00000002", "00000001", "kimi thinks", "a1", kimi),
		userLine(t, "00000003", "00000002", "q2"),
		replyLine(t, "00000004", "00000003", "minimax thinks", "a2", minimax),
	)
	sameWireOtherProvider := Model{Provider: "together", Alias: "together/kimi-k3", WireModel: kimi.WireModel}
	for name, tc := range map[string]struct {
		current Model
		want    []string
	}{
		"kimi keeps its own": {kimi, []string{
			"user: q1", "assistant: (thinking: kimi thinks) a1", "user: q2", "assistant: a2",
		}},
		"minimax keeps its own": {minimax, []string{
			"user: q1", "assistant: a1", "user: q2", "assistant: (thinking: minimax thinks) a2",
		}},
		"same provider, other wire model": {kimiCode, []string{
			"user: q1", "assistant: a1", "user: q2", "assistant: a2",
		}},
		"same wire model, other provider": {sameWireOtherProvider, []string{
			"user: q1", "assistant: a1", "user: q2", "assistant: a2",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			tr := mustLoad(t, raw)
			if got := messageTexts(tr.Context(tc.current)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("context = %q, want %q", got, tc.want)
			}
			// The transcript itself still has every reasoning part.
			for _, i := range []int{1, 3} {
				if n := len(tr.Entries[i].Message.Content); n != 2 {
					t.Fatalf("entry %d has %d parts after Context, want 2: the filter changed the transcript", i+1, n)
				}
			}
		})
	}
}

// TestAssistantLeftEmptyIsDropped: an assistant message whose only part was
// another model's reasoning has nothing left to send, and is dropped rather
// than replayed with empty content. If it was the last answer, its user
// message is then unanswered and goes too.
func TestAssistantLeftEmptyIsDropped(t *testing.T) {
	u1 := userLine(t, "00000001", "", "q1")
	thinkingOnly := replyLine(t, "00000002", "00000001", "minimax thinks", "", minimax)
	u2 := userLine(t, "00000003", "00000002", "q2")
	a2 := replyLine(t, "00000004", "00000003", "", "a2", kimi)
	for name, tc := range map[string]struct {
		raw     string
		current Model
		want    []string
	}{
		"in the middle": {lines(headerText(t), u1, thinkingOnly, u2, a2), kimi, []string{
			"user: q1", "user: q2", "assistant: a2",
		}},
		"at the end": {lines(headerText(t), u1, thinkingOnly), kimi, nil},
		"kept by its own model": {lines(headerText(t), u1, thinkingOnly), minimax, []string{
			"user: q1", "assistant: (thinking: minimax thinks)",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := messageTexts(mustLoad(t, tc.raw).Context(tc.current)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("context = %q, want %q", got, tc.want)
			}
		})
	}
}

func callsLine(t *testing.T, id, parent string, ids ...string) string {
	return msgLine(t, id, parent, callsMsg("", "", ids...), kimi)
}

func resultsLine(t *testing.T, id, parent string, ids ...string) string {
	return msgLine(t, id, parent, resultsMsg(ids...), kimi)
}

// entryMessages is the message entries' messages, in order.
func entryMessages(es []Entry) []fantasy.Message {
	var msgs []fantasy.Message
	for _, e := range es {
		if e.Type == TypeMessage {
			msgs = append(msgs, e.Message)
		}
	}
	return msgs
}

// TestLoadRollsBackACutStep cuts a real transcript at every line boundary,
// and inside every line, and loads each prefix. The file ends in two tool
// steps: the first led by a model and an effort change and the turn's
// prompt, the second by two steers, each with two parallel calls. Whatever
// the cut, the transcript loads back to the last complete step, the
// invariant holds on it and on the context for any model, and the cut-off
// entries are gone.
//
// The control: where a cut ends right after an assistant message with
// calls, H1's rule (keep everything up to the last assistant message) would
// have kept that message, and unpairedIn — whose own control is
// TestUnpairedInCatchesEachBreak — reports those entries as unpaired.
func TestLoadRollsBackACutStep(t *testing.T) {
	s := newStore(t, testOptions(t))
	turn(t, s, "q1", "a1", kimi) // lines 1-2
	if err := s.AppendModelChange(minimax); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEffortChange("high"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUser(user("q2", minimax)); err != nil {
		t.Fatal(err)
	}
	firstStep := MessageEntry{Message: callsMsg("look first", "", "call_a", "call_b"), Model: minimax, StopReason: "tool_use"}
	step(t, s, nil, firstStep, results(minimax, "call_a", "call_b")) // lines 3-7
	steers := []MessageEntry{user("steer one", minimax), user("steer two", minimax)}
	secondStep := MessageEntry{Message: callsMsg("", "and these", "call_a", "call_b"), Model: minimax, StopReason: "tool_use"}
	step(t, s, steers, secondStep, results(minimax, "call_a", "call_b")) // lines 8-11

	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	all := strings.SplitAfter(string(data), "\n") // the header, 11 lines, and ""
	full := mustLoad(t, string(data))
	if n := len(full.Entries); n != 11 || len(all) != 13 {
		t.Fatalf("the file has %d entries in %d pieces; the fixture is not what this test expects", n, len(all))
	}
	// Written by hand from the file above: the lines that complete a step
	// (the first answer, each tool message), and those that leave calls open.
	complete := []int{0, 2, 7, 11}
	open := map[int]bool{6: true, 10: true}

	for n := 0; n <= 11; n++ {
		keep := 0
		for _, c := range complete {
			if c <= n {
				keep = c
			}
		}
		for _, torn := range []bool{false, true} {
			if torn && n == 11 {
				continue
			}
			raw := strings.Join(all[:1+n], "")
			name := fmt.Sprintf("%d lines", n)
			if torn {
				next := all[1+n]
				raw += next[:len(next)/2]
				name += " and half the next"
			}
			t.Run(name, func(t *testing.T) {
				tr := mustLoad(t, raw)
				if got, want := entryIDs(tr.Entries), entryIDs(full.Entries[:keep]); !reflect.DeepEqual(got, want) {
					t.Fatalf("kept entries %q, want %q", got, want)
				}
				if err := unpairedIn(entryMessages(tr.Entries)); err != nil {
					t.Fatalf("the loaded entries: %v", err)
				}
				for _, m := range []Model{kimi, minimax} {
					if err := unpairedIn(tr.Context(m)); err != nil {
						t.Fatalf("the context for %s: %v", m.Alias, err)
					}
				}
				for _, e := range full.Entries[keep:] {
					if _, err := tr.ContextAt(e.ID, kimi); !errors.Is(err, ErrUnknownEntry) {
						t.Fatalf("ContextAt(%s) = %v; a rolled-back entry must be unknown", e.ID, err)
					}
				}
				if open[n] && !torn && unpairedIn(entryMessages(full.Entries[:n])) == nil {
					t.Fatal("control: the cut does not end on unanswered calls")
				}
			})
		}
	}
}

// TestLoadRefusesUnpairedLines: a line that breaks the pairing invariant is
// ErrCorrupt wherever it is — in the middle of the file, or as its last line,
// because a cut can only take whole lines off the end, never write a whole
// line that contradicts the one before it. Each case wraps ErrUnpaired (or
// names the unreplayable result), so it is the invariant that refused the
// file, not a decoding error. The control is the same shape, paired, which
// loads.
func TestLoadRefusesUnpairedLines(t *testing.T) {
	h := headerText(t)
	u1 := userLine(t, "00000001", "", "q1")
	a1 := replyLine(t, "00000002", "00000001", "", "a1", kimi)
	mc := `{"type":"model_change","id":"00000003","parentId":"00000002","timestamp":"2026-09-18T12:00:00.000Z","provider":"openrouter","model":"openrouter/minimax-m3","wire_model":"minimax/minimax-m3"}`
	toolLine := func(id, parent string, parts ...fantasy.MessagePart) string {
		return msgLine(t, id, parent, fantasy.Message{Role: fantasy.MessageRoleTool, Content: parts}, kimi)
	}
	textResult := resultsMsg("a").Content[0]
	done := func(id, parent string) string { return replyLine(t, id, parent, "", "done", kimi) }

	control := mustLoad(t, lines(h, u1, callsLine(t, "00000002", "00000001", "a", "b"),
		resultsLine(t, "00000003", "00000002", "a", "b"), done("00000004", "00000003")))
	if len(control.Entries) != 4 {
		t.Fatalf("control: the paired file kept %d entries, want 4", len(control.Entries))
	}

	for name, tc := range map[string]struct {
		raw      string
		line     string
		unpaired bool // wraps ErrUnpaired; otherwise the message names the result
	}{
		"results after an answer": {lines(h, u1, a1, resultsLine(t, "00000003", "00000002", "a"),
			userLine(t, "00000004", "00000003", "q2"), done("00000005", "00000004")), "line 4", true},
		"results after a user message": {lines(h, u1, resultsLine(t, "00000002", "00000001", "a"),
			done("00000003", "00000002")), "line 3", true},
		"results for other calls": {lines(h, u1, callsLine(t, "00000002", "00000001", "a", "b"),
			resultsLine(t, "00000003", "00000002", "a", "x"), done("00000004", "00000003")), "line 4", true},
		"results out of order": {lines(h, u1, callsLine(t, "00000002", "00000001", "a", "b"),
			resultsLine(t, "00000003", "00000002", "b", "a"), done("00000004", "00000003")), "line 4", true},
		"a result missing": {lines(h, u1, callsLine(t, "00000002", "00000001", "a", "b"),
			resultsLine(t, "00000003", "00000002", "a"), done("00000004", "00000003")), "line 4", true},
		"a result extra": {lines(h, u1, callsLine(t, "00000002", "00000001", "a"),
			resultsLine(t, "00000003", "00000002", "a", "b"), done("00000004", "00000003")), "line 4", true},
		"a repeated call id": {lines(h, u1, callsLine(t, "00000002", "00000001", "a", "a"),
			resultsLine(t, "00000003", "00000002", "a", "a"), done("00000004", "00000003")), "line 3", true},
		"an empty call id": {lines(h, u1, callsLine(t, "00000002", "00000001", ""),
			resultsLine(t, "00000003", "00000002", ""), done("00000004", "00000003")), "line 3", true},
		"a user message where the results belong": {lines(h, u1, callsLine(t, "00000002", "00000001", "a"),
			userLine(t, "00000003", "00000002", "q2"), done("00000004", "00000003")), "line 4", true},
		"a model change where the results belong": {lines(h, u1, callsLine(t, "00000002", "00000001", "a"), mc,
			resultsLine(t, "00000004", "00000003", "a"), done("00000005", "00000004")), "line 4", true},
		"results parented elsewhere": {lines(h, u1, callsLine(t, "00000002", "00000001", "a"),
			resultsLine(t, "00000003", "00000001", "a"), done("00000004", "00000003")), "line 4", true},
		"a branch between calls and results": {lines(h, u1, callsLine(t, "00000002", "00000001", "a"),
			resultsLine(t, "00000003", "00000002", "a"), done("00000004", "00000003"),
			userLine(t, "00000005", "00000002", "q2"), done("00000006", "00000005")), "line 6", true},
		"a tool call in a user message": {lines(h, msgLine(t, "00000001", "", userWithCall(), kimi),
			done("00000002", "00000001")), "line 2", true},
		"text in a tool message": {lines(h, u1, callsLine(t, "00000002", "00000001", "a"),
			toolLine("00000003", "00000002", fantasy.TextPart{Text: "here"}, textResult), done("00000004", "00000003")), "line 4", true},
		"a result with no output": {lines(h, u1, callsLine(t, "00000002", "00000001", "a"),
			toolLine("00000003", "00000002", fantasy.ToolResultPart{ToolCallID: "a"}), done("00000004", "00000003")), "line 4", false},
		"an error result with no text": {lines(h, u1, callsLine(t, "00000002", "00000001", "a"),
			toolLine("00000003", "00000002", errorResult("a", errors.New(""))), done("00000004", "00000003")), "line 4", false},
		// The same breaks as the file's last line.
		"results for other calls, last": {lines(h, u1, callsLine(t, "00000002", "00000001", "a", "b"),
			resultsLine(t, "00000003", "00000002", "a", "x")), "line 4", true},
		"results after an answer, last": {lines(h, u1, a1, resultsLine(t, "00000003", "00000002", "a")), "line 4", true},
		"a user message where the results belong, last": {lines(h, u1, callsLine(t, "00000002", "00000001", "a"),
			userLine(t, "00000003", "00000002", "q2")), "line 4", true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeFile(t, tc.raw))
			if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), tc.line) {
				t.Fatalf("Load = %v, want ErrCorrupt at %s", err, tc.line)
			}
			if tc.unpaired != errors.Is(err, ErrUnpaired) {
				t.Fatalf("Load = %v; wraps ErrUnpaired: %v, want %v", err, errors.Is(err, ErrUnpaired), tc.unpaired)
			}
			if !tc.unpaired && !strings.Contains(err.Error(), `tool call "a"`) {
				t.Fatalf("Load = %v, want it to name the result", err)
			}
		})
	}
}

// TestModelSwitchKeepsToolPairs replays a tool history to every kind of
// model: its own, one on the same provider, one on another provider. Each
// context stays paired: an assistant message whose reasoning is stripped
// keeps its calls, text or no text, and the tool message stays after it.
// The control is the reasoning-only answer at the end, which the same filter
// does drop for another model (and its unanswered prompt with it).
func TestModelSwitchKeepsToolPairs(t *testing.T) {
	tr := mustLoad(t, lines(
		headerText(t),
		userLine(t, "00000001", "", "q1"),
		msgLine(t, "00000002", "00000001", callsMsg("kimi plans", "", "a", "b"), kimi),
		resultsLine(t, "00000003", "00000002", "a", "b"),
		replyLine(t, "00000004", "00000003", "kimi sums up", "done", kimi),
		userLine(t, "00000005", "00000004", "q2"),
		msgLine(t, "00000006", "00000005", callsMsg("minimax plans", "", "c"), minimax),
		msgLine(t, "00000007", "00000006", resultsMsg("c"), minimax),
		replyLine(t, "00000008", "00000007", "", "ok", minimax),
		userLine(t, "00000009", "00000008", "q3"),
		replyLine(t, "0000000a", "00000009", "minimax only thinks", "", minimax),
	))
	sameWireOtherProvider := Model{Provider: "together", Alias: "together/kimi-k3", WireModel: kimi.WireModel}
	callA, callB, callC := `[call a read {"filePath":"a.go"}]`, `[call b read {"filePath":"b.go"}]`, `[call c read {"filePath":"c.go"}]`
	stripped := []string{
		"user: q1", "assistant: " + callA + " " + callB, "tool: [result a: body of a] [result b: body of b]",
		"assistant: done", "user: q2", "assistant: " + callC, "tool: [result c: body of c]", "assistant: ok",
	}
	for name, tc := range map[string]struct {
		current Model
		want    []string
	}{
		"its own, for the first half": {kimi, []string{
			"user: q1", "assistant: (thinking: kimi plans) " + callA + " " + callB, "tool: [result a: body of a] [result b: body of b]",
			"assistant: (thinking: kimi sums up) done", "user: q2", "assistant: " + callC, "tool: [result c: body of c]", "assistant: ok",
		}},
		"its own, for the second half": {minimax, []string{
			"user: q1", "assistant: " + callA + " " + callB, "tool: [result a: body of a] [result b: body of b]",
			"assistant: done", "user: q2", "assistant: (thinking: minimax plans) " + callC, "tool: [result c: body of c]", "assistant: ok",
			"user: q3", "assistant: (thinking: minimax only thinks)",
		}},
		"same provider, other wire model": {kimiCode, stripped},
		"same wire model, other provider": {sameWireOtherProvider, stripped},
	} {
		t.Run(name, func(t *testing.T) {
			got := tr.Context(tc.current)
			if texts := messageTexts(got); !reflect.DeepEqual(texts, tc.want) {
				t.Fatalf("context = %q, want %q", texts, tc.want)
			}
			if err := unpairedIn(got); err != nil {
				t.Fatalf("context: %v", err)
			}
		})
	}
}

// TestProviderExecutedPartsStayWithTheirModel: a call the provider ran
// itself, and its result, replay only to the model that made them, like
// reasoning. For another model they go; the client call beside them and its
// tool message stay, and a message holding nothing else is dropped. The
// control is the model's own context, which keeps every part.
func TestProviderExecutedPartsStayWithTheirModel(t *testing.T) {
	search := func(id string) []fantasy.MessagePart {
		return []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: id, ToolName: "web_search", Input: `{"q":"peano"}`, ProviderExecuted: true},
			fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentText{Text: "3 hits"}, ProviderExecuted: true},
		}
	}
	mixed := fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: append(append(
		[]fantasy.MessagePart{fantasy.TextPart{Text: "searching"}}, search("ws_1")...),
		fantasy.ToolCallPart{ToolCallID: "a", ToolName: "read", Input: `{"filePath":"a.go"}`})}
	searchOnly := fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: search("ws_2")}
	tr := mustLoad(t, lines(
		headerText(t),
		userLine(t, "00000001", "", "q1"),
		msgLine(t, "00000002", "00000001", mixed, kimi),
		resultsLine(t, "00000003", "00000002", "a"),
		msgLine(t, "00000004", "00000003", searchOnly, kimi),
		userLine(t, "00000005", "00000004", "q2"),
		replyLine(t, "00000006", "00000005", "", "ok", minimax),
	))
	callA, ws1, ws2 := `[call a read {"filePath":"a.go"}]`, `[call ws_1 web_search {"q":"peano"}] [result ws_1: 3 hits]`, `[call ws_2 web_search {"q":"peano"}] [result ws_2: 3 hits]`
	for name, tc := range map[string]struct {
		current Model
		want    []string
	}{
		"another model": {minimax, []string{
			"user: q1", "assistant: searching " + callA, "tool: [result a: body of a]", "user: q2", "assistant: ok",
		}},
		"control: its own model": {kimi, []string{
			"user: q1", "assistant: searching " + ws1 + " " + callA, "tool: [result a: body of a]",
			"assistant: " + ws2, "user: q2", "assistant: ok",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			got := tr.Context(tc.current)
			if texts := messageTexts(got); !reflect.DeepEqual(texts, tc.want) {
				t.Fatalf("context = %q, want %q", texts, tc.want)
			}
			if err := unpairedIn(got); err != nil {
				t.Fatalf("context: %v", err)
			}
		})
	}
}

// TestContextAtStopsBeforeUnansweredCalls: from a leaf that is an assistant
// message whose results come later, the context ends before it, and before
// the prompt it answers. The control is the leaf one step on, the tool
// message, where the pair is replayed.
func TestContextAtStopsBeforeUnansweredCalls(t *testing.T) {
	tr := mustLoad(t, lines(
		headerText(t),
		userLine(t, "00000001", "", "q1"),
		replyLine(t, "00000002", "00000001", "", "a1", kimi),
		userLine(t, "00000003", "00000002", "q2"),
		callsLine(t, "00000004", "00000003", "a"),
		resultsLine(t, "00000005", "00000004", "a"),
		replyLine(t, "00000006", "00000005", "", "done", kimi),
	))
	for leaf, want := range map[string][]string{
		"00000004": {"user: q1", "assistant: a1"},
		"00000005": {"user: q1", "assistant: a1", "user: q2", `assistant: [call a read {"filePath":"a.go"}]`, "tool: [result a: body of a]"},
	} {
		got, err := tr.ContextAt(leaf, kimi)
		if err != nil {
			t.Fatal(err)
		}
		if texts := messageTexts(got); !reflect.DeepEqual(texts, want) {
			t.Fatalf("ContextAt(%s) = %q, want %q", leaf, texts, want)
		}
	}
}

// TestToolMessageGoesWithItsAssistant covers ContextAt's defence against a
// transcript Load would refuse (built here without Load): a tool message
// after an assistant message that replays as nothing goes with it, so no
// result is replayed without its call. The control is the same transcript
// for the model whose reasoning it is, where the assistant message is kept
// and the tool message with it.
func TestToolMessageGoesWithItsAssistant(t *testing.T) {
	tr := newTranscript(Header{Version: FormatVersion, ID: "sid", Timestamp: fixedTime, Cwd: testWorkspace})
	add := func(id, parent string, m fantasy.Message) {
		tr.add(Entry{Type: TypeMessage, ID: id, ParentID: parent, Timestamp: fixedTime, MessageEntry: MessageEntry{Message: m, Model: kimi}})
	}
	add("00000001", "", fantasy.NewUserMessage("q1"))
	add("00000002", "00000001", assistantMsg("kimi thinks", ""))
	add("00000003", "00000002", resultsMsg("a"))
	add("00000004", "00000003", fantasy.NewUserMessage("q2"))
	add("00000005", "00000004", assistantMsg("", "a2"))

	got := tr.Context(minimax)
	if texts := messageTexts(got); !reflect.DeepEqual(texts, []string{"user: q1", "user: q2", "assistant: a2"}) {
		t.Fatalf("context for another model = %q", texts)
	}
	if err := unpairedIn(got); err != nil {
		t.Fatalf("context for another model: %v", err)
	}
	if texts := messageTexts(tr.Context(kimi)); !reflect.DeepEqual(texts, []string{
		"user: q1", "assistant: (thinking: kimi thinks)", "tool: [result a: body of a]", "user: q2", "assistant: a2",
	}) {
		t.Fatalf("control: context for its own model = %q", texts)
	}
}
