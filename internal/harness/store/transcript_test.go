package store

import (
	"errors"
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
