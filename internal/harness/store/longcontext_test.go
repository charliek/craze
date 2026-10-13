package store

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The long-context subsets (plan 047 D10): a subagent_usage row's and a
// compaction entry's, each written under long_context only when it holds
// something, read back as written, and skipped by a reader that does not
// know the key — an older craze's.

// longRows are two sub-agent rows: one with a long-context subset, one with
// none.
var longRows = []ModelUsage{
	{Provider: "test", Model: "test/b", WireModel: "wire-b", Usage: Usage{Input: 120_010, Output: 15, CacheRead: 4},
		LongContext: Usage{Input: 120_000, Output: 10}},
	{Provider: "other", Model: "other/c", WireModel: "wire-c", Usage: Usage{Input: 10, Output: 5}},
}

// TestLongContextRoundTrips: a tool entry's rows and a compaction entry carry
// their long-context subsets under long_context — on the row that has one,
// after usage, and nowhere else — and read back as written, through Load
// and the store's own copy alike. TestCompactionEntryRoundTrips and
// TestSubagentUsageRoundTrips are the control: a usage with no subset is
// written byte for byte as before.
func TestLongContextRoundTrips(t *testing.T) {
	s := newStore(t, compacted(testOptions(t)))
	if err := s.AppendUser(user("q", kimi)); err != nil {
		t.Fatal(err)
	}
	withRows := results(kimi, "a")
	withRows.SubagentUsage = longRows
	step(t, s, nil, calls(kimi, "a"), withRows)
	c := failure("summarizer: 3 attempts failed")
	c.LongContext = Usage{Input: 101_000, Output: 30, Reasoning: 5, CacheRead: 6000, CacheCreation: 1}
	compact(t, s, 1, c)

	b, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if n := strings.Count(string(b), `"long_context"`); n != 2 {
		t.Fatalf("long_context appears %d times; want twice, once on the row that has one and once on the compaction:\n%s", n, b)
	}
	if want := `"usage":{"input":120010,"output":15,"reasoning":0,"cache_read":4,"cache_creation":0},"long_context":{"input":120000,"output":10,"reasoning":0,"cache_read":0,"cache_creation":0}},{"provider":"other","model":"other/c","wire_model":"wire-c","usage":{"input":10,"output":5,"reasoning":0,"cache_read":0,"cache_creation":0}}]`; !strings.Contains(lines[len(lines)-2], want) {
		t.Fatalf("the tool entry's line =\n%s\nwant it to hold\n%s", lines[len(lines)-2], want)
	}
	if want := `"usage":{"input":12,"output":34,"reasoning":5,"cache_read":6789,"cache_creation":1},"long_context":{"input":101000,"output":30,"reasoning":5,"cache_read":6000,"cache_creation":1},"error":`; !strings.Contains(lines[len(lines)-1], want) {
		t.Fatalf("the compaction's line =\n%s\nwant it to hold\n%s", lines[len(lines)-1], want)
	}

	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]*Transcript{"Load": tr, "the store's own": s.Transcript()} {
		tool, comp := got.Entries[len(got.Entries)-2], got.Entries[len(got.Entries)-1]
		if !reflect.DeepEqual(tool.SubagentUsage, longRows) {
			t.Fatalf("%s: rows read back %+v, want %+v", name, tool.SubagentUsage, longRows)
		}
		if comp.Compaction != c || *comp.Usage != compactUsage {
			t.Fatalf("%s: compaction read back %+v (usage %+v), want %+v", name, comp.Compaction, *comp.Usage, c)
		}
	}
}

// TestAnOlderReaderSkipsLongContext: a reader that does not know long_context
// — an older craze, whose rows and compaction lines have no such field —
// reads a line that carries it as the same entry without it: the store's
// decoder skips a key it does not know, in a subagent_usage row and at the top
// of a compaction line alike. The key is renamed to one this craze does not
// know either, which is how this decoder reads the older one's view.
// Negative control: a decoder that refuses unknown fields (json.Decoder's
// DisallowUnknownFields) fails both lines.
func TestAnOlderReaderSkipsLongContext(t *testing.T) {
	u := compactUsage
	comp := Entry{Type: TypeCompaction, ID: "0000000a", ParentID: "00000009", Timestamp: fixedTime,
		Compaction: Compaction{Error: "failed", TokensBefore: 5, TokensAfter: 5, Reason: CompactionAuto,
			LongContext: Usage{Input: 12, Output: 34}},
		MessageEntry: MessageEntry{Turn: 2, Model: kimi, Usage: &u}}
	tool := Entry{Type: TypeMessage, ID: "0000000b", ParentID: "0000000a", Timestamp: fixedTime,
		MessageEntry: *results(kimi, "a")}
	tool.SubagentUsage = longRows
	for _, e := range []Entry{comp, tool} {
		line, err := encodeEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(line), `"long_context":`) {
			t.Fatalf("premise: the line carries no long_context: %s", line)
		}
		older := strings.ReplaceAll(string(line), `"long_context":`, `"long_context_unknown_to_this_reader":`)
		back, err := decodeEntry([]byte(older))
		if err != nil {
			t.Fatalf("a %s line with a key this reader does not know: %v\n%s", e.Type, err, older)
		}
		want := e
		want.Compaction.LongContext = Usage{}
		if e.SubagentUsage != nil {
			want.SubagentUsage = []ModelUsage{longRows[0], longRows[1]}
			want.SubagentUsage[0].LongContext = Usage{}
		}
		if !reflect.DeepEqual(back, want) {
			t.Fatalf("the %s line read back as\n%+v\nwant every other field as written\n%+v", e.Type, back, want)
		}
	}
}

// TestALongContextOfTheWrongTypeIsCorrupt: a compaction line whose
// long_context is not an object is a whole line that breaks the format
// (errInvalid), as every other field of the line is, never a torn tail. Its
// values are not checked against the usage: the spend distrusts a subset
// that is not inside it.
func TestALongContextOfTheWrongTypeIsCorrupt(t *testing.T) {
	u := compactUsage
	e := Entry{Type: TypeCompaction, ID: "0000000a", ParentID: "00000009", Timestamp: fixedTime,
		Compaction:   Compaction{Error: "failed", TokensBefore: 5, TokensAfter: 5, Reason: CompactionAuto},
		MessageEntry: MessageEntry{Turn: 2, Model: kimi, Usage: &u}}
	line, err := encodeEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"x"`, `null`, `[1]`} {
		broken := strings.Replace(string(line), `,"error":`, `,"long_context":`+bad+`,"error":`, 1)
		if _, err := decodeEntry([]byte(broken)); !errors.Is(err, errInvalid) {
			t.Errorf("long_context %s: decodeEntry = %v, want errInvalid", bad, err)
		}
	}
	above := strings.Replace(string(line), `,"error":`, `,"long_context":{"input":999999,"output":-1},"error":`, 1)
	back, err := decodeEntry([]byte(above))
	if err != nil || back.Compaction.LongContext != (Usage{Input: 999999, Output: -1}) {
		t.Fatalf("a subset outside its usage read back as %+v, %v; want it as written", back.Compaction.LongContext, err)
	}
}
