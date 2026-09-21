package agent

import (
	"strings"
	"testing"
)

// The 8-bit escape introducers and the bidi formatting controls, spelled from
// their code points so no editor — and nothing that writes this file — can
// turn an escape into the invisible character itself (native_test.go's
// zeroWidthSpace is spelled the same way).
var (
	c1CSI   = string(rune(0x9b))   // CSI, the 8-bit form of ESC [
	c1OSC   = string(rune(0x9d))   // OSC, the 8-bit form of ESC ]
	c1DCS   = string(rune(0x90))   // DCS, the 8-bit form of ESC P
	c1ST    = string(rune(0x9c))   // ST, which ends a string sequence
	c1NEL   = string(rune(0x85))   // NEL: a C1 control that introduces nothing
	nbsp    = string(rune(0xa0))   // the byte just past C1: ordinary text
	bidiRLO = string(rune(0x202e)) // right-to-left override
	bidiLRI = string(rune(0x2066)) // left-to-right isolate
	bidiPDI = string(rune(0x2069)) // pop directional isolate
	bidiRLM = string(rune(0x200f)) // right-to-left mark
)

// TestSanitizeTextDropsC1AndBidi (plan 023 X14): the 8-bit spellings of the
// escape sequences go the way the 7-bit ones do — the introducer AND what it
// introduces — and the bidi formatting controls go with the zero-width
// runes. The fast path has to notice them too: a string
// whose only fault is one of these is pure ASCII plus one valid rune, which
// is exactly what `clean` used to wave through.
func TestSanitizeTextDropsC1AndBidi(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"c1 csi colour", c1CSI + "31mred" + c1CSI + "0m", "red"},
		{"c1 osc to st", c1OSC + "0;pwned" + c1ST + "title", "title"},
		{"c1 osc to bel", c1OSC + "0;pwned\x07title", "title"},
		{"c1 dcs to esc backslash", c1DCS + "q;stuff\x1b\\ok", "ok"},
		{"esc osc closed by the 8-bit st", "\x1b]0;pwned" + c1ST + "title", "title"},
		{"c1 csi swallows to its final byte, as esc does", "a" + c1CSI + "b", "a"},
		{"lone c1 control", "a" + c1NEL + "b", "ab"},
		{"lone string terminator", "a" + c1ST + "b", "ab"},
		{"c1 at the end", "ok" + c1CSI, "ok"},
		{"bidi override", "a" + bidiRLO + "b", "ab"},
		{"bidi isolates", bidiLRI + "path" + bidiPDI, "path"},
		{"bidi mark", "a" + bidiRLM + "b", "ab"},
		// U+00A0 is the code point just past the C1 block: ordinary text, and
		// a boundary the range check has to get right.
		{"keeps no-break space", "a" + nbsp + "b", "a" + nbsp + "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeText(tc.in); got != tc.want {
				t.Fatalf("sanitizeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// The fast path is part of the sanitiser: an input it calls clean
			// is returned untouched, so it must agree with the loop.
			if clean(tc.in) != (tc.in == tc.want) {
				t.Fatalf("clean(%q) = %v, but sanitizing gives %q", tc.in, clean(tc.in), tc.want)
			}
		})
	}
}

// TestSanitizeLineDropsC1AndBidi: the folded form is the sanitized one, so a
// label, a path or a heading is as safe as a document.
func TestSanitizeLineDropsC1AndBidi(t *testing.T) {
	got := sanitizeLine("  before " + c1OSC + "0;x" + c1ST + "\n" + bidiRLO + "after  ")
	if got != "before after" {
		t.Fatalf("sanitizeLine = %q, want %q", got, "before after")
	}
	if strings.ContainsFunc(got, isDropped) {
		t.Fatalf("sanitizeLine kept a rune it drops: %q", got)
	}
}

func TestSanitizeText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"keeps newline and tab", "a\n\tb", "a\n\tb"},
		{"drops carriage return", "a\rb", "ab"},
		{"drops nul and bell", "a\x00b\x07c", "abc"},
		{"csi colour", "\x1b[31mred\x1b[0m", "red"},
		{"csi cursor move", "x\x1b[2Jy", "xy"},
		{"osc title bel", "\x1b]0;pwned\x07title", "title"},
		{"osc title st", "\x1b]0;pwned\x1b\\title", "title"},
		{"dcs", "\x1bPq;stuff\x1b\\ok", "ok"},
		{"apc", "\x1b_payload\x1b\\ok", "ok"},
		{"two byte escape", "\x1b(Bok", "ok"},
		{"lone escape at end", "ok\x1b", "ok"},
		{"del", "a\x7fb", "ab"},
		{"invalid utf8", "a\xffb", "a�b"},
		{"keeps valid utf8", "héllo ✓", "héllo ✓"},
		// Cursor pads its fast label with two zero-width spaces.
		{"live fast label", "Fast\u200b\u200b", "Fast"},
		{"word joiner", "a\u2060b", "ab"},
		{"bom", "\ufeffhello", "hello"},
		// ZWNJ is a letter in Persian and ZWJ holds an emoji together, so
		// neither is a zero-width character craze may drop.
		{"persian zwnj", "می\u200cخواهم", "می\u200cخواهم"},
		{"zwj emoji", "👩\u200d💻", "👩\u200d💻"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeText(tc.in); got != tc.want {
				t.Fatalf("sanitizeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeEveryToolField(t *testing.T) {
	s := newSession(Options{})
	esc := "\x1b]0;x\x07\x1b[31m"
	title := esc + "Edit File"
	tool, _ := s.mergeTool(toolDelta{
		id:    "id-1",
		title: &title,
		rawInput: mustJSON(map[string]any{
			"_toolName":   "task",
			"prompt":      esc + "run",
			"description": esc + "desc",
		}),
		hasRawInput: true,
		content: mustJSON([]map[string]any{{
			"type":    "diff",
			"path":    esc + "/tmp/a.go",
			"oldText": "a\n",
			"newText": esc + "b\n",
		}}),
		hasContent: true,
		rawOutput: mustJSON(map[string]any{
			"exitCode": 1,
			"stdout":   esc + "out",
			"stderr":   esc + "err",
			"content":  esc + "file",
		}),
		hasRawOutput: true,
		locations:    mustJSON([]map[string]any{{"path": esc + "/tmp/a.go"}}),
		hasLocations: true,
	})
	if tool.Title != "Edit File" || tool.Name != "Edit File" {
		t.Fatalf("title %q", tool.Title)
	}
	if tool.RawInput != "run" {
		t.Fatalf("rawInput %q", tool.RawInput)
	}
	if tool.ToolName != "task" {
		t.Fatalf("toolName %q", tool.ToolName)
	}
	if tool.Task == nil || tool.Task.Prompt != "run" || tool.Task.Description != "desc" {
		t.Fatalf("task %+v", tool.Task)
	}
	if tool.Output.Stdout != "out" || tool.Output.Stderr != "err" || tool.Output.Content != "file" {
		t.Fatalf("output %+v", tool.Output)
	}
	if tool.Output.StdoutHead != "out" || tool.Output.StderrHead != "err" {
		t.Fatalf("heads %+v", tool.Output)
	}
	if len(tool.Diffs) != 1 || tool.Diffs[0].Path != "/tmp/a.go" || tool.Diffs[0].NewText != "b\n" {
		t.Fatalf("diffs %+v", tool.Diffs)
	}
	if len(tool.Locations) != 1 || tool.Locations[0] != "/tmp/a.go" {
		t.Fatalf("locations %+v", tool.Locations)
	}
	// The id is a map key, never rendered, so it is kept verbatim.
	if tool.ID != "id-1" {
		t.Fatalf("id %q", tool.ID)
	}
}

func TestToolIDKeptVerbatimWithNewline(t *testing.T) {
	s := newSession(Options{})
	id := "call-abc-0\nfc_xyz"
	title := "Read File"
	tool, _ := s.mergeTool(toolDelta{id: id, title: &title})
	if tool.ID != id {
		t.Fatalf("id %q", tool.ID)
	}
	if got := s.Snapshot().Tools[0].ID; got != id {
		t.Fatalf("snapshot id %q", got)
	}
}
