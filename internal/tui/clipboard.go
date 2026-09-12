package tui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/atotto/clipboard"
	"github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
)

const (
	// clipboardMax is how much of a selection craze will hand to the terminal.
	// OSC 52 payloads are base64 and terminals cap them; 64 KiB is generous for
	// a transcript selection and small enough that a stray whole-buffer copy
	// cannot stall the renderer.
	clipboardMax = 64 << 10
	// clipboardTruncated is appended to the note when the cap bites.
	clipboardTruncated = "… (truncated)"
	// copyNoteLinger is how long the status row says what was copied.
	copyNoteLinger = 2 * time.Second
	// copyPreview is how much of a one-line copy the note quotes back.
	copyPreview = 30
)

// syncWriter is the terminal with craze's writes serialised. bubbletea's
// renderer and the OSC 52 copy are two goroutines writing the same fd, and a
// copy landing inside a frame would tear it, so both take this lock.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
	// tty is the same writer when it is the terminal. bubbletea only finds the
	// window size through an output it can take a descriptor from, so handing
	// it a plain io.Writer would leave craze without a WindowSizeMsg — hence
	// the descriptor half, kept separately so a test can stand in for the
	// writer. os.File's methods are nil-safe, which is what makes that work.
	tty *os.File
}

// bubbletea's x/term.File, restated so losing it is a compile error.
var _ interface {
	io.ReadWriteCloser
	Fd() uintptr
} = (*syncWriter)(nil)

func newSyncWriter(f *os.File) *syncWriter { return &syncWriter{w: f, tty: f} }

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

func (w *syncWriter) Read(p []byte) (int, error) { return w.tty.Read(p) }
func (w *syncWriter) Close() error               { return w.tty.Close() }
func (w *syncWriter) Fd() uintptr                { return w.tty.Fd() }

// clipboardMu guards the three lower seams. systemCopy and systemPaste run on a
// command goroutine, and bubbletea never waits for one: an interactive Run can
// return with a copy still in flight, and a RunFrameScript later in the same
// process would then install its recorder concurrently with that read.
// Serialising frame runs cannot help — the racing command belongs to a session
// that has already ended — so the seams are read and written under this lock,
// which also means a restore waits for any OSC 52 write it is replacing.
var clipboardMu sync.Mutex

// clipboardOut is where the OSC 52 sequence goes: the synchronised writer Run
// gave bubbletea. It starts as io.Discard so nothing but a real craze session
// can write to a terminal — a test or `craze frame` that forgot to install a
// seam still cannot print escape bytes to somebody's stdout.
var clipboardOut io.Writer = io.Discard

// nativeCopy is the second, best-effort write: the platform clipboard tool.
// It is a variable so a test can watch it without running xclip.
var nativeCopy = clipboard.WriteAll

// nativePaste is the read half, and the only way text comes back off the
// system clipboard. bubbles' own Ctrl+V calls clipboard.ReadAll directly, which
// would shell out to xclip / wl-paste from under a test or a frame script, so
// craze binds its own paste and routes it through here.
var nativePaste = clipboard.ReadAll

// clipboardWrite is the one place a copy leaves craze. Both writes go through
// it, so a test (and `craze frame`) can record exactly what was copied and be
// certain no bytes reached a terminal and no clipboard tool ran.
var clipboardWrite = systemCopy

// clipboardRead is the paste seam, the mirror of clipboardWrite.
var clipboardRead = systemPaste

// setClipboardOut installs the OSC 52 writer alone and returns the previous
// one, which is all a real session changes.
func setClipboardOut(w io.Writer) io.Writer {
	clipboardMu.Lock()
	defer clipboardMu.Unlock()
	prev := clipboardOut
	clipboardOut = w
	return prev
}

// swapClipboardSeams installs all three lower seams and returns the previous
// set. Together with setClipboardOut it is the only writer of any of them, so
// every access takes the lock.
func swapClipboardSeams(out io.Writer, native func(string) error, paste func() (string, error)) (io.Writer, func(string) error, func() (string, error)) {
	clipboardMu.Lock()
	defer clipboardMu.Unlock()
	prevOut, prevNative, prevPaste := clipboardOut, nativeCopy, nativePaste
	clipboardOut, nativeCopy, nativePaste = out, native, paste
	return prevOut, prevNative, prevPaste
}

// systemCopy is the real writer: OSC 52 first, because it is the one that works
// over ssh and inside tmux with `set-clipboard on`, then the native clipboard
// for terminals that ignore OSC 52. The sequence is bare on purpose — tmux
// forwards it as-is, while go-osc52's tmux wrapper needs `allow-passthrough`.
func systemCopy(text string) error {
	clipboardMu.Lock()
	native := nativeCopy
	// The OSC 52 write stays inside the lock: it is the one that reaches a
	// terminal, so a swap must not be able to slip past it and let this
	// session's escape bytes land in the next one's output.
	_, err := osc52.New(text).WriteTo(clipboardOut)
	clipboardMu.Unlock()
	// The native write is a bonus: a headless box has no clipboard tool at all,
	// and the OSC 52 sequence has already gone out. It runs outside the lock
	// because it shells out — a clipboard tool that hangs must not hang the
	// restore, and it writes to the system clipboard, not to the screen.
	_ = native(text)
	return err
}

// systemPaste is the real reader. There is no OSC 52 half: reading the
// clipboard back out of the terminal needs a reply craze's input is not
// listening for, so the native tool is all there is — and, as in systemCopy, it
// is run outside the lock.
func systemPaste() (string, error) {
	clipboardMu.Lock()
	paste := nativePaste
	clipboardMu.Unlock()
	return paste()
}

// clipboardDoneMsg carries the note back into Update, where the status row can
// show it — and, because it is a message, the frame bus sees a state change and
// `<wait:copied>` has something to match.
type clipboardDoneMsg struct{ note string }

// copyText caps the payload and hands it to the seam, with a note that does not
// depend on how much of it survived the cap.
func copyText(text, note string) tea.Cmd {
	sent, cut := capClipboard(text)
	return sendCopy(sent, cut, note)
}

// copyRows is the selection's copy. Its note counts the rows that actually
// went, not the rows that were selected: 1,000 rows capped at 64 KiB are about
// 655 rows on the clipboard, and a note saying 1,000 would be a lie.
func copyRows(text string) tea.Cmd {
	sent, cut := capClipboard(text)
	return sendCopy(sent, cut, copyNote(sent, lineCount(sent)))
}

// sendCopy is the command both copies return. The note is decided here rather
// than in the goroutine so it describes what was actually sent.
func sendCopy(sent string, cut bool, note string) tea.Cmd {
	if cut {
		note += " " + clipboardTruncated
	}
	// The seam is read here, on the Update goroutine, and not inside the
	// command: bubbletea leaks a command's goroutine until it returns, so a
	// command that read the package variable could still be running when the
	// frame runner puts the real writer back.
	write := clipboardWrite
	return func() tea.Msg {
		// A clipboard that refused the write is not worth an error line: the
		// note still says what craze tried to copy.
		_ = write(sent)
		return clipboardDoneMsg{note: note}
	}
}

// lineCount is how many transcript rows a payload holds: one per line break,
// plus the last row when the payload does not end on one. It is counted from
// the payload and not from the selection's endpoints so the cap is included —
// and it is not len(strings.Split(…)), because a selection that reached past
// the end of its last row copies a trailing newline that is not another row.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// pasteMsg carries the clipboard's text back into Update, where it is inserted
// as one bracketed paste.
type pasteMsg struct{ text string }

// pasteFromClipboard is Ctrl+V. bubbles' own binding calls clipboard.ReadAll
// from inside the textarea (textarea.go:1391), which would run xclip under a
// test or a frame script, so craze binds the key itself and reads through the
// seam. Like copyText, the seam is read on the Update goroutine.
func pasteFromClipboard() tea.Cmd {
	read := clipboardRead
	return func() tea.Msg {
		// A box with no clipboard tool pastes nothing, quietly: an error line
		// for a keystroke that had nothing to insert is noise.
		text, err := read()
		if err != nil {
			return pasteMsg{}
		}
		return pasteMsg{text: text}
	}
}

// capClipboard cuts text to clipboardMax bytes on a rune boundary, so a
// truncated copy is still valid UTF-8.
func capClipboard(text string) (string, bool) {
	if len(text) <= clipboardMax {
		return text, false
	}
	cut := clipboardMax
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], true
}

// copyNote is what the status row says about a copy: how many transcript rows
// went, or the start of the one row that did.
func copyNote(text string, lines int) string {
	if lines > 1 {
		return fmt.Sprintf("copied %d lines", lines)
	}
	return `copied "` + clampWidth(strings.TrimSpace(text), copyPreview) + `"`
}

// recordCopies points the seams at a buffer and returns the restore. It is what
// keeps `craze frame` honest: the runner prints its final frame to stdout, an
// OSC 52 sequence in that stream would corrupt it, and neither a script nor a
// test may run a clipboard tool. The recorder is the paste source too, so
// Ctrl+V in a script reads back what Ctrl+Y put there.
func recordCopies() (*copyRecorder, func()) {
	rec := &copyRecorder{}
	prevWrite, prevRead := clipboardWrite, clipboardRead
	clipboardWrite, clipboardRead = rec.write, rec.read
	prevOut, prevNative, prevPaste := swapClipboardSeams(
		io.Discard,
		func(string) error { return nil },
		func() (string, error) { return "", nil },
	)
	return rec, func() {
		clipboardWrite, clipboardRead = prevWrite, prevRead
		swapClipboardSeams(prevOut, prevNative, prevPaste)
	}
}

// copyRecorder is the recording seam. The copy runs on a tea.Cmd goroutine
// while the runner reads, so the mutex is not optional.
type copyRecorder struct {
	mu   sync.Mutex
	text []string
}

func (r *copyRecorder) write(text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.text = append(r.text, text)
	return nil
}

// read is the recorder's clipboard: the last thing copied into it.
func (r *copyRecorder) read() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.text) == 0 {
		return "", nil
	}
	return r.text[len(r.text)-1], nil
}

func (r *copyRecorder) copies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.text...)
}
