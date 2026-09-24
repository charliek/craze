package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The composer's shell mode (plan 022 §3.6).
//
// A draft whose first byte is `!` is a command for the user's own shell rather
// than a prompt for the agent. There is no flag and no toggle: the mode is a
// function of what is in the composer, so it can never be left on, and the one
// state a flag would have carried — a command actually running — is separate,
// because a run outlives the draft that started it.
//
// The escape hatch is a leading space. `send` trims the draft before it goes,
// so " !important" is an ordinary prompt: the mode reads the byte the user
// typed and the agent reads the text they meant.
const (
	// shellRuleTitle is the composer's top rule in shell mode. It replaces the
	// session title rather than adding a row: there is no hint line to put it
	// on — composerHint is the textarea's placeholder, which only an empty
	// draft shows, and a shell draft is never empty.
	shellRuleTitle = "shell · enter run · esc clear"
	// shellMark opens the transcript row, the way ❯ opens a user one.
	shellMark = "! "
	// shellPreviewLines is how much output a collapsed row shows before it
	// defers to Ctrl+O, matching a tool row's own preview.
	shellPreviewLines = outputPreviewLines
)

// shellMode reports that the composer is in shell mode: the draft's first byte
// is `!` and no queued row is being edited.
//
// The queue editor is the whole of the second half. The composer holds somebody
// else's text there — a message already accepted for the agent — and Enter
// saves it; a row that happens to start with `!` is a message about a command,
// not a command.
func (m Model) shellMode() bool {
	return m.queueEdit == "" && strings.HasPrefix(m.input.Value(), "!")
}

// shellDraft is the command in the draft: everything after the `!`, trimmed.
// A multi-line paste is one script and not a line at a time, so nothing here
// splits it. A bare `!` yields "", which is the mode with nothing to run.
func (m Model) shellDraft() string {
	if !m.shellMode() {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(m.input.Value(), "!"))
}

// shellRunning reports a command still going. It reads the controller every
// copy of the model shares, not a field of this copy, because the run belongs
// to the program and not to the Update that started it.
func (m Model) shellRunning() bool { return m.shell.running() }

// runShellDraft is Enter in shell mode, and the only way a command ever runs:
// never from a queued or drained row, the queue editor, a plan offer, a replay
// or a send-now. Those all carry text that was accepted as a prompt, and
// `craze prompt "!x"` keeps sending it as one.
//
// The two refusals that keep the draft — a card on screen, a session that is
// still restoring — are handleEnter's own gate, which this sits behind. The
// third is here: one command at a time, because a second would have nowhere to
// draw and nothing would say which of them Esc stopped.
func (m Model) runShellDraft() (tea.Model, tea.Cmd) {
	if m.shellRunning() {
		return m, nil
	}
	script := m.shellDraft()
	if script == "" {
		return m, nil
	}
	gen, run := m.shell.start(script, m.cwd)
	m.addShell(gen, script)
	// The draft goes exactly as it does on a send: accepted, so it is gone.
	m.input.SetValue("")
	m.resetSlash()
	return m, run
}

// killShell stops whatever the composer is running and returns at once. The row
// is settled by the shellDoneMsg the runner sends on its way out.
func (m Model) killShell() { m.shell.cancel() }

// finishShell settles the row one command left behind, and keeps what it
// printed for the next message the user sends (shell_context.go): the command
// was run to be asked about, and the asking is the message after it.
//
// Unless the run has been disowned, which is a session change: the row is still
// settled — it is in the transcript the user watched it open in — but what it
// printed is not context for a message to some other agent, in some other
// workspace (shellController.disown).
func (m *Model) finishShell(msg shellDoneMsg) {
	if m.shell.keepsContext(msg.gen) {
		m.keepShellResult(msg.cmd, msg.res)
	}
	t := m.main
	for i := len(t.rows) - 1; i >= 0; i-- {
		e := t.rows[i]
		if e.shell == nil || e.shell.gen != msg.gen {
			continue
		}
		e.text = msg.res.out
		e.shell.done = true
		e.shell.exit = msg.res.exit
		e.shell.why = msg.res.why
		e.shell.start = msg.res.start
		e.dirty = true
		t.dirty = true
		return
	}
	// The row is gone — /clear took it, or the entry cap trimmed it — and the
	// output is still the answer to something the user asked for, so it is
	// written again rather than dropped.
	t.appendEntry(entry{
		kind: entryShell,
		text: msg.res.out,
		shell: &shellEntry{
			gen: msg.gen, cmd: msg.cmd, done: true,
			exit: msg.res.exit, why: msg.res.why, start: msg.res.start,
		},
	}, m.now())
}

// shellEntry is what a transcript row knows about one command. It is
// client-local: nothing here ever went to an agent or came back from one, which
// is why the row is a kind of its own rather than a note or a tool event.
//
// The output itself lives in entry.text, so it is counted by the transcript's
// own text budget like every other entry's.
type shellEntry struct {
	// gen names the run, so a result can find the row it belongs to.
	gen int
	cmd string
	// done is the whole of "is this row still moving": the spinner, and every
	// suffix, is drawn from it.
	done  bool
	exit  int
	why   shellEnding
	start error
}

// addShell opens the row for a command that has just started.
func (m *Model) addShell(gen int, cmd string) {
	m.appendEntry(entry{kind: entryShell, shell: &shellEntry{gen: gen, cmd: cmd}})
}

// shellSuffix is the right end of the row: what the command's ending was, where
// it was anything but a clean exit.
func (m Model) shellSuffix(s *shellEntry) (string, lipgloss.Style) {
	switch {
	case !s.done:
		return "", styleFG(m.theme.Dim)
	case s.start != nil:
		return sanitizeLine(s.start.Error()), styleFG(m.theme.Err)
	case s.why == shellTimedOut:
		return fmt.Sprintf("timed out after %ds", int(shellTimeout.Seconds())), styleFG(m.theme.Err)
	case s.why == shellKilled:
		return "killed", styleFG(m.theme.Dim)
	case s.why == shellAbandoned:
		return "would not stop", styleFG(m.theme.Err)
	case s.exit != 0:
		return fmt.Sprintf("exit %d", s.exit), styleFG(m.theme.Err)
	}
	return "", styleFG(m.theme.Dim)
}

// shellGlyph is the row's status mark: the spinner while it runs, and then the
// same three marks a tool row settles into.
//
// How it ended is asked before what it exited with, and that order is the whole
// of it: a signalled leader has no exit status and reports -1, so asking about
// the code first would draw every command the user stopped as one that failed,
// and leave this arm unreachable. Esc is not a failure — it is the user
// changing their mind, and the suffix beside the mark says "killed" in words.
// A command SIGKILL could not end is the exception: nothing about "would not
// stop" is cancelled.
func (m Model) shellGlyph(s *shellEntry) (string, lipgloss.Style) {
	switch {
	case !s.done:
		return m.spinnerGlyph(), styleFG(m.theme.Accent)
	case s.start != nil, s.why == shellAbandoned:
		return m.statusGlyph("failed")
	case s.why != shellExited:
		return m.statusGlyph("cancelled")
	case s.exit != 0:
		return m.statusGlyph("failed")
	}
	return m.statusGlyph("completed")
}

// shellRows draws one `! <cmd>` row and, once it has finished, its output:
// dimmed, because it is not the conversation, and collapsed past
// shellPreviewLines under the same Ctrl+O every long tool row answers to.
func (m *Model) shellRows(e *entry, key renderKey) []string {
	s := e.shell
	if s == nil {
		return nil
	}
	glyph, gst := m.shellGlyph(s)
	suffix, sufSt := m.shellSuffix(s)
	rows := []string{m.shellHead(glyph, gst, s.cmd, suffix, sufSt, key.width)}
	if !s.done || e.text == "" {
		return rows
	}
	lines := strings.Split(strings.TrimRight(e.text, "\n"), "\n")
	shown := lines
	if !key.expanded && len(shown) > shellPreviewLines {
		shown = shown[:shellPreviewLines]
	}
	for _, ln := range shown {
		rows = append(rows, m.dimRow("  "+plainLine(ln), key.width))
	}
	if len(shown) < len(lines) {
		rows = append(rows, m.dimRow(fmt.Sprintf("  … %d more lines · ⌃o expand", len(lines)-len(shown)), key.width))
	}
	return rows
}

// shellHead is "<glyph> ! <cmd>  <suffix>". The command is folded onto one line
// — a multi-line script is one row, not a block — and clamped first, so a long
// command never pushes the ending off the row.
func (m *Model) shellHead(glyph string, gst lipgloss.Style, cmd, suffix string, sufSt lipgloss.Style, width int) string {
	text := sanitizeLine(cmd)
	fixed := 2 + len(shellMark)
	if suffix != "" {
		fixed += 2 + lipgloss.Width(suffix)
	}
	if avail := width - fixed; avail >= 1 {
		text = clampWidth(text, avail)
	}
	segs := []seg{{glyph + " ", gst}, {shellMark, styleFG(m.theme.Shell)}, {text, styleFG(m.theme.FG)}}
	if suffix != "" {
		segs = append(segs, seg{"  " + suffix, sufSt})
	}
	return renderSegs(width, segs...)
}
