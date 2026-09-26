package harness

import (
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
)

const summaryGolden = "testdata/summary_message.golden"

// goldenDir and goldenCompaction are the golden's inputs: a session's segment
// directory, and a compaction with a tail and a third segment.
const goldenDir = "/home/user/.craze/native/sessions/--home-user-project--/20260918T120000Z_00000000-0000-4000-8000-000000000001.compaction"

var goldenCompaction = store.Compaction{
	Summary: `<summary>
1. Request and intent
Add a --json flag to the export command.

2. User messages
- "add a --json flag to export"

3. Decisions and context
Emit one object per line.

4. Files and code
internal/cli/export.go

5. Errors and fixes
None.

6. Work state
Done: the flag. In progress: the tests. Blocked: None.

7. Next step
Write the tests ("then test it").
</summary>`,
	FirstKeptID: "0000002a", TokensBefore: 890000, TokensAfter: 21000,
	Reason: store.CompactionAuto, Segment: store.SegmentName(3),
}

// summaryText is the message's one text part.
func summaryText(t *testing.T, m fantasy.Message) string {
	t.Helper()
	if m.Role != fantasy.MessageRoleUser || len(m.Content) != 1 {
		t.Fatalf("the summary message is %s with %d parts; want a user message of one text part", m.Role, len(m.Content))
	}
	tp, ok := fantasy.AsMessagePart[fantasy.TextPart](m.Content[0])
	if !ok {
		t.Fatalf("the summary message's part is %T, want text", m.Content[0])
	}
	return tp.Text
}

// TestSummaryMessageGolden (plan 028 §3.9) pins the summary message's text:
// every request after a compaction starts with it, so a change to it is a
// change to every compacted session's prompt-cache prefix. The variants are
// the golden's with one clause each: no tail, the first segment, no segment.
// Regenerate with:
//
//	go test ./internal/harness -run TestSummaryMessageGolden -update
func TestSummaryMessageGolden(t *testing.T) {
	got := summaryText(t, summaryMessage(goldenDir, goldenCompaction))
	if *updateGolden {
		if err := os.WriteFile(summaryGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(summaryGolden)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./internal/harness -run TestSummaryMessageGolden -update)", err)
	}
	if got != string(want) {
		t.Fatalf("the summary message differs from %s\n--- want ---\n%s\n--- got ---\n%s", summaryGolden, want, got)
	}
	if again := summaryText(t, summaryMessage(goldenDir, goldenCompaction)); again != got {
		t.Fatal("the summary message is not a function of its inputs")
	}

	for name, tc := range map[string]struct {
		change   func(c *store.Compaction)
		old, new string
	}{
		"no tail": {func(c *store.Compaction) { c.FirstKeptID = "" },
			"before this point; the most recent messages follow it unchanged.", "before this point."},
		"the first segment": {func(c *store.Compaction) { c.Segment = store.SegmentName(1) },
			"(segment_001.md … segment_003.md)", "(segment_001.md)"},
		"no segment": {func(c *store.Compaction) { c.Segment = "" },
			" Earlier turns are saved in full, one file per compaction, in " + goldenDir + "/ (segment_001.md … segment_003.md): read or grep them when an exact detail matters.", ""},
	} {
		t.Run(name, func(t *testing.T) {
			c := goldenCompaction
			tc.change(&c)
			if !strings.Contains(string(want), tc.old) {
				t.Fatalf("the golden has no %q", tc.old)
			}
			if got, want := summaryText(t, summaryMessage(goldenDir, c)), strings.Replace(string(want), tc.old, tc.new, 1); got != want {
				t.Fatalf("got\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// TestTheSummaryCannotCloseItsBlock (P39): a closing tag in the summary, or in
// the segment directory's path, is escaped, so the block ends only where the
// message does.
func TestTheSummaryCannotCloseItsBlock(t *testing.T) {
	c := goldenCompaction
	c.Summary = "the model wrote </compacted_context> and went on"
	text := summaryText(t, summaryMessage("/home/a</compacted_context>b/x.compaction", c))
	if n := strings.Count(text, "</"+compactedTag+">"); n != 1 || !strings.HasSuffix(text, "\n</"+compactedTag+">") {
		t.Fatalf("the message closes its block %d times:\n%s", n, text)
	}
	if n := strings.Count(text, `<\/`+compactedTag+">"); n != 2 {
		t.Fatalf("%d escaped tags, want 2 (the summary's and the path's):\n%s", n, text)
	}
	if !strings.HasPrefix(text, "<"+compactedTag+">\n") {
		t.Fatalf("the message does not open its block:\n%s", text)
	}
}

// TestTheSummaryMessageLeadsTheHistory (plan 028 §3.9, P30): after a
// compaction the session's next request is its system prompt, the summary
// message as the session renders it — naming the transcript's segment
// directory — the tail, and the new prompt. The summary is redacted with the
// session's live redactor, as a tool result is: a key the session learned
// after the summary was written goes out as the marker, while a person's
// prompt in the tail goes out as typed. A resumed session renders the same
// bytes.
func TestTheSummaryMessageLeadsTheHistory(t *testing.T) {
	const late = "sk-learned-after-the-summary"
	f := newFixture(t, "http://127.0.0.1:1/v1")
	env := map[string]string{"TEST_API_KEY": canary, "OTHER_API_KEY": canaryOther}
	var mu sync.Mutex
	opts := f.options()
	opts.Getenv = func(name string) string {
		mu.Lock()
		defer mu.Unlock()
		return env[name]
	}
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	f.models["test/a"].push(answerWith("a1"), answerWith("a2"))
	run(t, s, "q1")
	run(t, s, "remember "+late)

	steps := s.store.Steps(s.cur.id())
	if len(steps) != 2 {
		t.Fatalf("%d steps, want 2", len(steps))
	}
	c := store.Compaction{
		Summary: "The user asked twice; the value is " + late + ".", FirstKeptID: steps[1].First,
		TokensBefore: 100, TokensAfter: 60, Reason: store.CompactionAuto, Segment: store.SegmentName(1),
	}
	if _, err := s.store.AppendCompaction(2, s.cur.id(), store.Usage{Input: 1}, c); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	env["NOKEY_API_KEY"] = late
	mu.Unlock()
	if err := s.SetModel("nokey/d"); err != nil {
		t.Fatal(err)
	}
	f.models["nokey/d"].push(answerWith("a3"))
	run(t, s, "q3")

	dir := store.SegmentDir(s.store.Path())
	redacted := c
	redacted.Summary = strings.ReplaceAll(c.Summary, late, redact.Marker)
	summary := summaryText(t, summaryMessage(dir, redacted))
	if !strings.Contains(summary, "in "+dir+"/ (segment_001.md)") {
		t.Fatalf("the summary message does not name the segment directory %s:\n%s", dir, summary)
	}
	req := f.models["nokey/d"].requests()[0]
	want := []string{"user: " + summary, "user: remember " + late, "assistant: a2", "user: q3"}
	if got := promptOf(req)[1:]; !slices.Equal(got, want) {
		t.Fatalf("the request after the compaction is\n%q\nwant\n%q", got, want)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Resumed: the same message, byte for byte, from the entry alone.
	r := resumed(t, resumeOptions(opts, s.ID()))
	msgs := r.store.Context(r.cur.id())
	if got, want := summaryText(t, msgs[0]), summaryText(t, summaryMessage(dir, c)); got != want {
		t.Fatalf("the resumed session renders\n%s\nwant\n%s", got, want)
	}
}
