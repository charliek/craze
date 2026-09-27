package harness

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
)

// Segment files (plan 028 §3.10): one Markdown file per successful
// compaction, holding the part of the conversation its summary stands for —
// the previous tail and everything written since, less this compaction's own
// tail — so consecutive segments never overlap. store.WriteSegment (segment.go
// in package store) writes it, temp file then rename under the store's lock,
// before the compaction entry that names it (compactSucceeded).

// maxSegmentBytes caps a segment file (plan 028 §3.10, grok-build XCT:20-23).
const maxSegmentBytes = 512 * 1024

// resultByteCap is how much of one tool result a segment keeps (plan 028
// §3.10): its first 8 KiB, then a note of how much was left out.
const resultByteCap = 8 * 1024

// segmentContent is segment n's Markdown content: a heading naming the
// session and the segment, a metadata line (the entries' ids and time range,
// and the model that ran them), the compaction's own summary under "##
// Summary", then "## Transcript" — discarded's steps, oldest first, each
// message through the live redactor (P30). Over maxSegmentBytes, the OLDEST
// turns are dropped first, with a line naming the transcript so an exact
// detail can still be found there — a line the fit itself now counts
// (review r1-c9 finding 11): the loop drops one more turn than it used to
// wherever the notice's own bytes would otherwise have tipped the file over
// the cap. If the header and the summary alone still do not fit once every
// turn is gone, the summary itself is cut, with its own marker, rather than
// left to overflow the cap on its own.
func (s *Session) segmentContent(id string, n int, discarded []store.Step, summary string, red *redact.Replacer) []byte {
	head := func(sum string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "# craze session %s — segment %03d (historical; do not edit)\n", id, n)
		if line, ok := segmentMetaLine(discarded); ok {
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("\n## Summary\n\n")
		b.WriteString(sum)
		b.WriteString("\n\n## Transcript\n\n")
		return b.String()
	}

	blocks := make([]string, len(discarded))
	for i, st := range discarded {
		blocks[i] = segmentStepBlock(st, red)
	}

	dropped := 0
	body := strings.Join(blocks, "")
	notice := ""
	fits := func() bool { return len(head(summary))+len(notice)+len(body) <= maxSegmentBytes }
	for !fits() && len(blocks) > 0 {
		blocks = blocks[1:]
		dropped++
		body = strings.Join(blocks, "")
		notice = omissionNotice(dropped, s.store.Path())
	}
	if !fits() {
		// Every turn is already gone (body is "" here) and the header plus
		// the summary alone still do not fit: cut the summary itself, with
		// its own marker, rather than let the file overflow the cap.
		budget := maxSegmentBytes - len(head("")) - len(notice)
		summary = truncateForSegment(summary, budget)
	}

	var out strings.Builder
	out.WriteString(head(summary))
	out.WriteString(notice)
	out.WriteString(body)
	return []byte(out.String())
}

// omissionNotice is the line segmentContent prepends to the transcript body
// once dropped turns were needed to fit the cap.
func omissionNotice(dropped int, transcriptPath string) string {
	return fmt.Sprintf("[%d earlier turn(s) omitted to fit the %d KiB segment cap; see the transcript %s]\n\n",
		dropped, maxSegmentBytes/1024, filepath.Base(transcriptPath))
}

// summaryTruncatedMarker is appended to a summary segmentContent had to cut
// to fit the cap on its own (no transcript turn was left to drop instead).
const summaryTruncatedMarker = "\n\n[summary truncated to fit the segment cap]"

// truncateForSegment is summary cut to at most budget bytes, on a rune
// boundary, with summaryTruncatedMarker appended; budget at or under the
// marker's own length keeps no summary text at all, just the marker.
func truncateForSegment(summary string, budget int) string {
	room := budget - len(summaryTruncatedMarker)
	if room <= 0 {
		return strings.TrimPrefix(summaryTruncatedMarker, "\n\n")
	}
	cut := min(room, len(summary))
	for cut > 0 && !utf8.RuneStart(summary[cut]) {
		cut--
	}
	return summary[:cut] + summaryTruncatedMarker
}

// segmentMetaLine is the metadata line naming discarded's first and last
// entry ids, their time range, and the model that ran them (the last
// message entry's, which is every ordinary step's own): "" when discarded
// holds nothing (a compaction with an empty tail to summarize — the prior
// summary alone, re-summarized).
func segmentMetaLine(discarded []store.Step) (string, bool) {
	var first, last string
	var start, end time.Time
	var model store.Model
	for _, st := range discarded {
		for _, e := range st.Entries {
			if first == "" {
				first, start = e.ID, e.Timestamp
			}
			last, end = e.ID, e.Timestamp
			if e.Model.Alias != "" {
				model = e.Model
			}
		}
	}
	if first == "" {
		return "", false
	}
	return fmt.Sprintf("entries %s–%s, %s – %s, model %s/%s (%s)",
		first, last, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339),
		model.Provider, model.Alias, model.WireModel), true
}

// segmentStepBlock renders one step's messages under "### User"/"###
// Assistant" (without reasoning), "#### Tool call <name>" and "#### Result",
// each result capped at resultByteCap. Every message is redacted again with
// red — harmless for one already redacted at rest, and needed for the two
// kinds a step's stored form does not cover on its own: a reminder
// (rendered fresh here, from no stored text at all) and, at the head of the
// very first step after an earlier compaction, that compaction's own
// summary message (rendered fresh from its entry too) — both live text a key
// learned since must still be kept out of (P30, mirroring redactHistory's
// rule for the two kinds ContextWithResults marks).
func segmentStepBlock(st store.Step, red *redact.Replacer) string {
	var b strings.Builder
	for _, m := range st.Messages {
		m = liveRedact(red, m)
		switch m.Role {
		case fantasy.MessageRoleUser:
			if t := textOf(m); t != "" {
				b.WriteString("### User\n\n" + t + "\n\n")
			}
		case fantasy.MessageRoleAssistant:
			if t := textOf(m); t != "" {
				b.WriteString("### Assistant\n\n" + t + "\n\n")
			}
			for _, p := range m.Content {
				if c, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](p); ok && !c.ProviderExecuted {
					fmt.Fprintf(&b, "#### Tool call %s\n\n```json\n%s\n```\n\n", c.ToolName, c.Input)
				}
			}
		case fantasy.MessageRoleTool:
			for _, p := range m.Content {
				if r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p); ok && !r.ProviderExecuted {
					b.WriteString("#### Result\n\n" + capResultText(resultText(r)) + "\n\n")
				}
			}
		}
	}
	return b.String()
}

// capResultText is a result's text, cut to resultByteCap with a note of how
// much was left out (plan 028 §3.10). The cut backs up to a rune boundary so
// a multi-byte character straddling the cap is never split, keeping the
// segment's Markdown valid UTF-8; the omitted count reflects the actual cut.
func capResultText(s string) string {
	if len(s) <= resultByteCap {
		return s
	}
	cut := resultByteCap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s\n[… %d bytes omitted]", s[:cut], len(s)-cut)
}
