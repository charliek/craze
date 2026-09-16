package tui

import (
	"encoding/hex"
	"io"

	"github.com/charmbracelet/x/ansi"
)

// terminalColors sets the terminal's own default background and foreground to
// the theme's while craze runs, and puts them back on the way out (§3.3).
//
// bubbletea v1 has no background command and a per-row Background would touch
// every renderer, the viewport and the textarea — and still miss the cells the
// alt screen clears. OSC 11 recolours every default-background and erased cell
// in one write; OSC 10 does the same for the default foreground, so any cell
// craze leaves at the terminal default still matches the theme. OSC 110/111
// reset both to the terminal's *configured* defaults: the previous colours are
// deliberately not restored, because reading them back means an OSC 11 `?`
// query whose reply lands on stdin, racing bubbletea's input reader.
//
// The writer is the model's, not a package global: Model carries one of these
// as a pointer (so it survives bubbletea copying Model by value) and New gives
// it io.Discard, so every test, `craze frame` and any direct caller emits
// nothing. Only Run attaches the real terminal, and only when cfg.Background.
type terminalColors struct {
	// out is io.Discard unless Run attached the terminal.
	out io.Writer
	// set records that a set pair has been written, so a reset is owed.
	set bool
}

// newTerminalColors is nil- and io.Discard-safe: a nil writer becomes
// io.Discard, so the zero-cost path needs no branch at the call sites.
func newTerminalColors(out io.Writer) *terminalColors {
	if out == nil {
		out = io.Discard
	}
	return &terminalColors{out: out}
}

// apply sets the terminal's default background and foreground to the theme's.
//
// The pair is one Write on purpose: the writer Run passes is the syncWriter
// whose lock is held for the whole call, so a bubbletea frame can never land
// between the two halves of a pair. The sequences come from x/ansi, as the
// window title's do (title.go), rather than being spelled out here.
func (t *terminalColors) apply(th Theme) {
	if t == nil {
		return
	}
	bg, okBG := oscRGB(string(th.BG))
	fg, okFG := oscRGB(string(th.FG))
	if !okBG || !okFG {
		// A malformed palette is not a reason to write garbage at the
		// terminal. Every preset's BG/FG is a well-formed #rrggbb today.
		return
	}
	_, _ = io.WriteString(t.out, ansi.SetBackgroundColor("rgb:"+bg)+ansi.SetForegroundColor("rgb:"+fg))
	t.set = true
}

// reset hands the terminal's defaults back. It writes nothing unless a set
// pair was written, so a second reset — or a reset on a run that never set
// anything — is silent.
func (t *terminalColors) reset() {
	if t == nil || !t.set {
		return
	}
	_, _ = io.WriteString(t.out, ansi.ResetBackgroundColor+ansi.ResetForegroundColor)
	t.set = false
}

// oscRGB turns a lipgloss "#rrggbb" into the "rr/gg/bb" half of xterm's
// XParseColor rgb: form (§3.3). Anything else reports false and the caller
// writes nothing.
func oscRGB(c string) (string, bool) {
	if len(c) != 7 || c[0] != '#' {
		return "", false
	}
	if _, err := hex.DecodeString(c[1:]); err != nil {
		return "", false
	}
	return c[1:3] + "/" + c[3:5] + "/" + c[5:7], true
}
