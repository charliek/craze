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

// clipboardOut is where the OSC 52 sequence goes: the synchronised writer Run
// gave bubbletea. It starts as io.Discard so nothing but a real craze session
// can write to a terminal — a test or `craze frame` that forgot to install a
// seam still cannot print escape bytes to somebody's stdout.
var clipboardOut io.Writer = io.Discard

// nativeCopy is the second, best-effort write: the platform clipboard tool.
// It is a variable so a test can watch it without running xclip.
var nativeCopy = clipboard.WriteAll

// clipboardWrite is the one place a copy leaves craze. Both writes go through
// it, so a test (and `craze frame`) can record exactly what was copied and be
// certain no bytes reached a terminal and no clipboard tool ran.
var clipboardWrite = systemCopy

// systemCopy is the real writer: OSC 52 first, because it is the one that works
// over ssh and inside tmux with `set-clipboard on`, then the native clipboard
// for terminals that ignore OSC 52. The sequence is bare on purpose — tmux
// forwards it as-is, while go-osc52's tmux wrapper needs `allow-passthrough`.
func systemCopy(text string) error {
	_, err := osc52.New(text).WriteTo(clipboardOut)
	// The native write is a bonus: a headless box has no clipboard tool at all,
	// and the OSC 52 sequence has already gone out.
	_ = nativeCopy(text)
	return err
}

// clipboardDoneMsg carries the note back into Update, where the status row can
// show it — and, because it is a message, the frame bus sees a state change and
// `<wait:copied>` has something to match.
type clipboardDoneMsg struct{ note string }

// copyText caps the payload and hands it to the seam. The note is decided here
// rather than in the goroutine so it describes what was actually sent.
func copyText(text, note string) tea.Cmd {
	text, cut := capClipboard(text)
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
		_ = write(text)
		return clipboardDoneMsg{note: note}
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

// recordCopies points the seam at a buffer and returns the restore. It is what
// keeps `craze frame` honest: the runner prints its final frame to stdout, and
// an OSC 52 sequence in that stream would corrupt it.
func recordCopies() (*copyRecorder, func()) {
	rec := &copyRecorder{}
	prevWrite, prevOut, prevNative := clipboardWrite, clipboardOut, nativeCopy
	clipboardWrite = rec.write
	clipboardOut = io.Discard
	nativeCopy = func(string) error { return nil }
	return rec, func() {
		clipboardWrite, clipboardOut, nativeCopy = prevWrite, prevOut, prevNative
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

func (r *copyRecorder) copies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.text...)
}
