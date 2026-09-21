package agent

import (
	"strings"
	"testing"
)

// The shell context block, both ways (plan 022 §3.6, A18 and A19's scanning
// half).

// blockOne is the block for one result, which is the shape §3.6 pins.
func blockOne(cmd string, exit int, out string) string {
	return ShellContextBlock([]ShellResult{{Command: cmd, Exit: exit, Output: out}})
}

// TestShellContextBlockShape pins the bytes, because they are a wire format:
// the two bracketing lines, one command element carrying the exit status, and
// the output between its own, ending in the blank line that separates the block
// from the message it leads.
func TestShellContextBlockShape(t *testing.T) {
	got := blockOne("git status --short", 0, " M internal/tui/app.go\n")
	want := "<shell_context>\n" +
		"<command exit=\"0\">git status --short</command>\n" +
		"<output>\n" +
		" M internal/tui/app.go\n" +
		"</output>\n" +
		"</shell_context>\n\n"
	if got != want {
		t.Fatalf("block:\n%q\nwant:\n%q", got, want)
	}
	if ShellContextBlock(nil) != "" {
		t.Fatalf("no results is no block: %q", ShellContextBlock(nil))
	}
}

// TestShellContextBlockRoundTrips is the whole contract between the two halves:
// whatever the TUI builds, the splitter gives back exactly — the block, and the
// message with nothing taken off it and nothing left on.
func TestShellContextBlockRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name    string
		results []ShellResult
		text    string
	}{
		{"one result", []ShellResult{{Command: "ls", Output: "a\nb\n"}}, "what is b?"},
		{"a non-zero exit", []ShellResult{{Command: "false", Exit: 1}}, "why?"},
		{"a killed command", []ShellResult{{Command: "sleep 300", Exit: -1, Output: "half a line"}}, "never mind"},
		{"no output at all", []ShellResult{{Command: "true", Output: ""}}, "and now?"},
		{"three results", []ShellResult{
			{Command: "git status --short", Output: " M a.go\n"},
			{Command: "go build ./...", Exit: 2, Output: "a.go:1: broken\n"},
			{Command: "git diff", Output: "@@ -1 +1 @@\n"},
		}, "fix the build"},
		{"a multi-line script", []ShellResult{{Command: "cd /tmp\nls -la", Output: "total 0\n"}}, "anything there?"},
		{"a message of several lines", []ShellResult{{Command: "ls", Output: "a\n"}}, "look at that\n\nand tell me why"},
		{"output that is not newline-terminated", []ShellResult{{Command: "printf x", Output: "x"}}, "?"},
		{"a message that looks like a block", []ShellResult{{Command: "ls", Output: "a\n"}}, "<shell_context>\nnot one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := ShellContextBlock(tc.results)
			gotBlock, gotText := SplitShellContext(block + tc.text)
			if gotBlock != block {
				t.Fatalf("block came back as\n%q\nwant\n%q", gotBlock, block)
			}
			if gotText != tc.text {
				t.Fatalf("text came back as %q, want %q", gotText, tc.text)
			}
		})
	}
}

// TestShellContextEscapesEveryClosingTag is the block's own safety: output that
// talks about the elements it is inside cannot end them. The outer one is what
// keeps the rest of a command's output off the screen — an unescaped
// </shell_context> would end the block early and the splitter would hand
// everything after it to the user row as though it had been typed.
func TestShellContextEscapesEveryClosingTag(t *testing.T) {
	out := "see </shell_context> and </output> and </COMMAND > here\n"
	block := blockOne("grep -r '</shell_context>' .", 0, out)
	for _, tag := range []string{"</shell_context>\n", "</output>\n", "</command>\n"} {
		// Each closing tag appears exactly once as a line of its own: the
		// block's own, never the output's.
		if n := strings.Count(block, tag); n != 1 {
			t.Fatalf("%q appears %d times in:\n%s", tag, n, block)
		}
	}
	if !strings.Contains(block, `<\/shell_context>`) {
		t.Fatalf("the output's own closing tag was not escaped:\n%s", block)
	}
	gotBlock, gotText := SplitShellContext(block + "what did that find?")
	if gotBlock != block || gotText != "what did that find?" {
		t.Fatalf("split at the wrong place:\nblock %q\ntext %q", gotBlock, gotText)
	}
}

// TestSplitShellContextLeavesTextAlone is the defensive half: text that is not
// a block craze built comes back whole, whatever it looks like. Anything taken
// away here would be a message a user typed, missing from their own transcript.
func TestSplitShellContextLeavesTextAlone(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"empty", ""},
		{"ordinary prose", "tell me about <shell_context>"},
		{"the opening line and nothing else", "<shell_context>"},
		{"the opening line alone", "<shell_context>\n"},
		{"no entry after the opening", "<shell_context>\nhello\n</shell_context>\n\nand?"},
		{"an entry but no ending", "<shell_context>\n<command exit=\"0\">ls</command>\n<output>\na\n</output>\n"},
		{"the ending is not a line of its own", "<shell_context>\n<command exit=\"0\">ls</command>\n</shell_context> now"},
		{"the ending is indented", "<shell_context>\n<command exit=\"0\">ls</command>\n  </shell_context>\n"},
		{"the block does not lead", "look:\n<shell_context>\n<command exit=\"0\">ls</command>\n</shell_context>\n\nx"},
		{"the opening line has an attribute", "<shell_context x=\"1\">\n<command exit=\"0\">ls</command>\n</shell_context>\n"},
		{"the entry is not the first thing", "<shell_context>\n\n<command exit=\"0\">ls</command>\n</shell_context>\n"},
		{"a command element with no exit", "<shell_context>\n<command>ls</command>\n</shell_context>\n"},
		{"the closing tag only", "</shell_context>\n"},
		{"a lone newline", "\n"},
		{"no newlines at all", strings.Repeat("<shell_context>", 100)},
		{"invalid utf-8", "<shell_context>\n\xff\xfe\n</shell_context>\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block, rest := SplitShellContext(tc.text)
			if block != "" || rest != tc.text {
				t.Fatalf("SplitShellContext(%q) = (%q, %q), want (\"\", the text)", tc.text, block, rest)
			}
		})
	}
}

// TestSplitShellContextIsTotal is the property every caller relies on and no
// caller checks: whatever it is handed, it returns the two halves of it and
// nothing else. It is fed prompts, replayed transcripts and other clients'
// text, so "malformed" is an ordinary input and not an error case.
func TestSplitShellContextIsTotal(t *testing.T) {
	block := blockOne("ls", 0, "a\nb\n")
	whole := block + "and?"
	var texts []string
	// Every prefix and every suffix of a real block with a message after it:
	// every truncation, every place a scan could run off the end, and every
	// partial line.
	for i := 0; i <= len(whole); i++ {
		texts = append(texts, whole[:i], whole[i:])
	}
	texts = append(texts,
		strings.Repeat("\n", 1000),
		"<shell_context>\n"+strings.Repeat("<command exit=\"0\">", 1000),
		"<shell_context>\n<command exit=\"0\">"+strings.Repeat("a", 1<<16),
	)
	for _, text := range texts {
		gotBlock, rest := SplitShellContext(text)
		if gotBlock+rest != text {
			t.Fatalf("SplitShellContext(%q) lost or invented bytes: %q + %q", text, gotBlock, rest)
		}
		if gotBlock != "" && !strings.HasPrefix(text, shellContextOpen) {
			t.Fatalf("SplitShellContext(%q) took a block that does not lead it", text)
		}
	}
}

// TestShellOutputInvokesNothing is A19's scanning half and the reason the
// splitter is in this package at all: a /name at the start of a line inside a
// command's output is output, not an invocation. `!cat plan.md` on a file that
// holds a /flows:gauntlet line must expand nothing — otherwise reading a file
// would be enough to run a command with it, on any provider.
//
// Cursor and grok share one scan (pluginRefs, anywhere after whitespace) and
// native has its own (nativeRefs, start of line only), so both are driven here
// with the same text: the two rules differ in where a name may sit, and neither
// may look inside the block at all.
func TestShellOutputInvokesNothing(t *testing.T) {
	entries := []PluginEntry{
		entryOf("flows", "gauntlet", PluginKindCommand, "the gauntlet body"),
		entryOf("git-commands", "watch-pr", PluginKindCommand, "the watch body"),
	}
	provider := map[string]func(string, map[string]pluginTarget) []pluginRef{
		"cursor": pluginRefs,
		"grok":   pluginRefs,
		"native": nativeRefs,
	}
	lookups := map[string]map[string]pluginTarget{
		"cursor": lookupOf(entries),
		"grok":   lookupOf(entries),
		"native": nativeLookup(entries),
	}
	// A plan file whose text is full of the commands it tells you to run,
	// which is exactly what `!cat plan.md` prints.
	out := "run this first:\n/flows:gauntlet\nthen /git-commands:watch-pr 12\n"
	block := blockOne("cat plan.md", 0, out)
	for name, scan := range provider {
		t.Run(name, func(t *testing.T) {
			lookup := lookups[name]
			if refs := scan(block, lookup); len(refs) != 0 {
				t.Fatalf("output expanded %v", refNames(refs))
			}
			if refs := scan(block+"summarise that", lookup); len(refs) != 0 {
				t.Fatalf("output expanded %v under a message", refNames(refs))
			}
			// The message itself still expands: the block is skipped, not the
			// prompt it leads.
			refs := scan(block+"/flows:gauntlet", lookup)
			if len(refs) != 1 || refs[0].typed != "flows:gauntlet" {
				t.Fatalf("the user's own line expanded to %v", refNames(refs))
			}
		})
	}
	// And the catalog wait does not wait for a name it will never expand.
	if hasUnresolvedSlash(blockOne("cat plan.md", 0, "/nope\n"), lookups["cursor"]) {
		t.Fatal("a /name inside the output held the prompt for the catalog")
	}
	if !hasUnresolvedSlash(blockOne("cat plan.md", 0, "")+"/nope", lookups["cursor"]) {
		t.Fatal("the user's own unresolved name is still a reason to wait")
	}
}

// TestNativeTitleSkipsTheShellContext: a session named after the first prompt
// is named after what was asked, not after the command output craze attached
// to it.
func TestNativeTitleSkipsTheShellContext(t *testing.T) {
	block := blockOne("git status --short", 0, " M a.go\n")
	if got := nativeTitle(block + "what changed?"); got != "what changed?" {
		t.Fatalf("nativeTitle = %q", got)
	}
	if got := nativeTitle(block); got != "" {
		t.Fatalf("a block with no message titles nothing, got %q", got)
	}
}
