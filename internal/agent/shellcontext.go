package agent

import (
	"strconv"
	"strings"
)

// The shell context block (plan 022 §3.6).
//
// A command the user ran in the composer's shell mode travels to the agent in
// front of the next thing they send, as one element:
//
//	<shell_context>
//	<command exit="0">git status --short</command>
//	<output>
//	…
//	</output>
//	</shell_context>
//
//	summarise that
//
// Both directions of that format live here, in one file, for two reasons. The
// first is that they have to agree exactly: what the splitter cannot find, the
// screen shows — and the block is **wire content, never display content**
// (§3.6). The second is that the reader is not only the TUI. This package
// strips the block itself, for a native session's title and for the reference
// scanners, and internal/cli cannot import internal/tui nor the other way
// round, so the one place both can reach is here.
//
// Only the TUI builds a block. Everything else is given a prompt's text and
// has to answer "what did the user actually write?", which is SplitShellContext
// and nothing more.

const (
	// shellContextTag names the element, and with it the two lines that
	// bracket a block: a line of exactly "<shell_context>" opens one and a
	// line of exactly "</shell_context>" closes it.
	shellContextTag = "shell_context"
	// shellCommandTag and shellOutputTag are the two elements inside it. The
	// command is one element per finished command and the output is its own,
	// so the model can tell what was run from what it printed.
	shellCommandTag = "command"
	shellOutputTag  = "output"

	shellContextOpen  = "<" + shellContextTag + ">"
	shellContextClose = "</" + shellContextTag + ">"
	// shellCommandOpen leads every entry, and is therefore what says that a
	// well-formed block starts here rather than a prompt that merely begins
	// with the same line.
	shellCommandOpen = "<" + shellCommandTag + " exit=\""
)

// ShellResult is one finished command as the block carries it: what was run,
// how it ended, and what it printed. It is the TUI's shellResult with the parts
// the agent has no use for — how it was killed, whether it started at all —
// left behind.
type ShellResult struct {
	Command string
	Exit    int
	Output  string
}

// ShellContextBlock renders the results as the block that leads the user's next
// message, ending in the blank line that separates the two. No results is the
// empty string, so a caller can always write block+text.
//
// Every closing tag the block uses is escaped inside both the command and the
// output by escapeClosingTag — the same helper that keeps a plugin body from
// closing the element it is inside. It matters more here than there: the
// output is whatever a command printed, so a file that talks about
// </shell_context> would otherwise end the block early, and SplitShellContext
// would then hand the rest of that output to the screen as though the user had
// typed it.
func ShellContextBlock(results []ShellResult) string {
	if len(results) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(shellContextOpen)
	b.WriteByte('\n')
	for _, r := range results {
		b.WriteString(shellCommandOpen)
		b.WriteString(strconv.Itoa(r.Exit))
		b.WriteString("\">")
		// Verbatim, newlines and all: a multi-line paste is one script and the
		// agent is owed the script that ran, not a folded version of it.
		b.WriteString(escapeShellContext(r.Command))
		b.WriteString("</" + shellCommandTag + ">\n")
		b.WriteString("<" + shellOutputTag + ">\n")
		if out := escapeShellContext(r.Output); out != "" {
			b.WriteString(out)
			if !strings.HasSuffix(out, "\n") {
				b.WriteByte('\n')
			}
		}
		b.WriteString("</" + shellOutputTag + ">\n")
	}
	b.WriteString(shellContextClose)
	// Two newlines: the block's own, and the blank line that keeps it from
	// running into the sentence the user wrote.
	b.WriteString("\n\n")
	return b.String()
}

// escapeShellContext escapes every closing tag the block is made of, so no
// content inside one can end an element it sits in. All three, not only the
// outer one: a line of exactly "</output>" in a command's output would not
// close the block for SplitShellContext, which reads the frame alone, but it
// would read as the end of the output to the model, which is the same lie one
// layer up.
func escapeShellContext(s string) string {
	s = escapeClosingTag(s, shellContextTag)
	s = escapeClosingTag(s, shellOutputTag)
	return escapeClosingTag(s, shellCommandTag)
}

// SplitShellContext separates a shell context block leading text from the text
// itself: the block (with its trailing blank line) and the rest, which together
// are exactly the string handed in. Text that does not start with a well-formed
// block comes back unchanged as the rest, with an empty context.
//
// It is fed arbitrary text — a prompt another client wrote, a transcript
// replayed from an agent, a message the user typed that merely looks like a
// block — so it is total and it validates the frame before it takes anything
// away:
//
//   - the first line is exactly "<shell_context>";
//   - the line after it opens an entry ("<command exit=\"");
//   - some later line is exactly "</shell_context>".
//
// The frame, and not the entries inside it. Validating the interior would make
// the splitter a second parser that could disagree with the builder, and a
// disagreement in that direction is the bug this helper exists to prevent: a
// block the splitter does not recognise is a block that reaches the screen.
// Against the other direction — hiding text somebody actually typed — three
// exact lines in that order is already far more than prose stumbles into, and
// the text is the sender's own either way.
func SplitShellContext(text string) (block, rest string) {
	if !strings.HasPrefix(text, shellContextOpen+"\n") {
		return "", text
	}
	at := len(shellContextOpen) + 1
	if !strings.HasPrefix(text[at:], shellCommandOpen) {
		return "", text
	}
	for at < len(text) {
		line, next := text[at:], len(text)
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			line, next = line[:nl], at+nl+1
		}
		if line == shellContextClose {
			if next < len(text) && text[next] == '\n' {
				// The blank line the builder writes belongs to the block: the
				// user's text starts where they started typing.
				next++
			}
			return text[:next], text[next:]
		}
		// next is always past at — a line plus its newline, or the end of the
		// string — so the walk ends whatever the text holds.
		at = next
	}
	return "", text
}
