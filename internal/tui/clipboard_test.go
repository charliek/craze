package tui

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// swapClipboard points the OSC 52 writer at a buffer and the native write at a
// recorder, and restores both. Nothing here may be parallel: the seam is
// package-level on purpose, so `-race` has something to check.
func swapClipboard(t *testing.T) (*bytes.Buffer, *[]string) {
	t.Helper()
	var buf bytes.Buffer
	var native []string
	prevOut, prevNative, prevWrite := clipboardOut, nativeCopy, clipboardWrite
	clipboardOut = &buf
	nativeCopy = func(s string) error {
		native = append(native, s)
		return nil
	}
	clipboardWrite = systemCopy
	t.Cleanup(func() {
		clipboardOut, nativeCopy, clipboardWrite = prevOut, prevNative, prevWrite
	})
	return &buf, &native
}

// TestCopyWritesBareOSC52: the exact bytes, with no tmux or screen wrapper.
// tmux with `set-clipboard on` forwards the bare sequence; the wrapper needs
// `allow-passthrough` and would be worse than nothing.
func TestCopyWritesBareOSC52(t *testing.T) {
	buf, native := swapClipboard(t)
	const text = "alpha bravo"
	msg := copyText(text, "copied")()
	if got, want := msg, (clipboardDoneMsg{note: "copied"}); got != want {
		t.Fatalf("msg %+v, want %+v", got, want)
	}
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
	if got := buf.String(); got != want {
		t.Fatalf("wrote %q, want %q", got, want)
	}
	// No DCS wrapper, either flavour.
	if strings.Contains(buf.String(), "\x1bPtmux") || strings.Contains(buf.String(), "\x1bP\x1b]52") {
		t.Fatalf("the sequence is wrapped: %q", buf.String())
	}
	// And the native clipboard saw the same text.
	if len(*native) != 1 || (*native)[0] != text {
		t.Fatalf("native clipboard got %q", *native)
	}
}

// TestCopySurvivesAFailedWrite: neither write is allowed to swallow the note.
// A terminal that refuses OSC 52 and a box with no clipboard tool still owe the
// user the acknowledgement.
func TestCopySurvivesAFailedWrite(t *testing.T) {
	prevOut, prevNative, prevWrite := clipboardOut, nativeCopy, clipboardWrite
	t.Cleanup(func() { clipboardOut, nativeCopy, clipboardWrite = prevOut, prevNative, prevWrite })
	clipboardOut = errWriter{}
	nativeCopy = func(string) error { return errors.New("no clipboard tool") }
	clipboardWrite = systemCopy

	msg := copyText("alpha", "copied \"alpha\"")()
	done, ok := msg.(clipboardDoneMsg)
	if !ok {
		t.Fatalf("got %T, want clipboardDoneMsg", msg)
	}
	if done.note != `copied "alpha"` {
		t.Fatalf("note %q", done.note)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestCopyCapsAtSixtyFourKiB: the payload is cut on a rune boundary and the note
// says so.
func TestCopyCapsAtSixtyFourKiB(t *testing.T) {
	rec := captureCopies(t)
	// A three-byte rune repeated, so the cap lands mid-rune.
	text := strings.Repeat("日", clipboardMax)
	msg := copyText(text, "copied 1 lines")()
	done := msg.(clipboardDoneMsg)
	if !strings.HasSuffix(done.note, clipboardTruncated) {
		t.Fatalf("note %q should end with the truncation marker", done.note)
	}
	copies := rec.copies()
	if len(copies) != 1 {
		t.Fatalf("copies %d", len(copies))
	}
	got := copies[0]
	if len(got) > clipboardMax {
		t.Fatalf("payload is %d bytes, cap is %d", len(got), clipboardMax)
	}
	if len(got) < clipboardMax-4 {
		t.Fatalf("payload is %d bytes, want it close to the cap", len(got))
	}
	if !strings.HasSuffix(got, "日") {
		t.Fatal("the payload was cut inside a rune")
	}
	// Under the cap nothing is cut and nothing is said.
	short := copyText("alpha", "copied")().(clipboardDoneMsg)
	if short.note != "copied" {
		t.Fatalf("note %q", short.note)
	}
}

// TestCopyNoteWording is the two forms §3.5 pins: a row count, or the start of
// the one row that was copied.
func TestCopyNoteWording(t *testing.T) {
	for _, tc := range []struct {
		text  string
		lines int
		want  string
	}{
		{"alpha bravo", 1, `copied "alpha bravo"`},
		{"  alpha  ", 1, `copied "alpha"`},
		{strings.Repeat("x", 40), 1, `copied "` + strings.Repeat("x", 29) + `…"`},
		{"a\nb", 2, "copied 2 lines"},
		{"a\nb\nc", 3, "copied 3 lines"},
	} {
		if got := copyNote(tc.text, tc.lines); got != tc.want {
			t.Fatalf("copyNote(%q, %d) = %q, want %q", tc.text, tc.lines, got, tc.want)
		}
	}
}

// TestCopyNoteLingersTwoSeconds: the note is in status row 2 for two seconds,
// and the tick chain has to stay fast until it goes.
func TestCopyNoteLingersTwoSeconds(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	captureCopies(t)
	m := selModel(t, &now, twoRows)
	if m.wantFastTick() {
		t.Fatal("an idle model wants the slow chain")
	}
	tm, _ := m.Update(clipboardDoneMsg{note: "copied 2 lines"})
	m = tm.(Model)
	if !m.wantFastTick() {
		t.Fatal("the copy note has to keep the chain fast, or it never disappears")
	}
	if !strings.Contains(statusText(m.statusRow2(m.lay)), "copied 2 lines") {
		t.Fatalf("row 2 does not carry the note:\n%s", plainView(m))
	}
	now = now.Add(copyNoteLinger - time.Millisecond)
	if !strings.Contains(statusText(m.statusRow2(m.lay)), "copied 2 lines") {
		t.Fatal("the note left early")
	}
	now = now.Add(2 * time.Millisecond)
	if strings.Contains(statusText(m.statusRow2(m.lay)), "copied") {
		t.Fatalf("the note outlived its deadline:\n%s", plainView(m))
	}
	if m.wantFastTick() {
		t.Fatal("the chain should go slow again once the note has gone")
	}
}

// TestFrameOutputCarriesNoClipboardBytes: `craze frame` prints its final frame
// to stdout, so a copy must never put an OSC 52 sequence in that stream — and
// nothing under `make test` may reach for the developer's own clipboard.
func TestFrameOutputCarriesNoClipboardBytes(t *testing.T) {
	// The frame runner installs its own recorder; this one proves the default
	// writer is not what runs, by leaving a real writer in place around it.
	var buf bytes.Buffer
	prevOut, prevNative := clipboardOut, nativeCopy
	var native []string
	clipboardOut = &buf
	nativeCopy = func(s string) error { native = append(native, s); return nil }
	t.Cleanup(func() { clipboardOut, nativeCopy = prevOut, prevNative })

	plain, raw := runStubFrameRaw(t, 100, 30,
		"<wait:idle>hi<enter><wait:text:echo: hi><wait:idle><drag:0,0,7,1><wait:copied>")
	if !strings.Contains(plain, "copied 2 lines") {
		t.Fatalf("the drag did not copy:\n%s", plain)
	}
	for _, s := range []string{plain, raw} {
		if strings.Contains(s, "\x1b]52") {
			t.Fatalf("an OSC 52 sequence reached the frame output:\n%q", s)
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("the frame runner wrote %q to the terminal", buf.String())
	}
	if len(native) != 0 {
		t.Fatalf("the frame runner ran the native clipboard: %q", native)
	}
}

// TestSyncWriterSerialisesWrites is the point of the writer: frames and the
// OSC 52 copy come from different goroutines and must not interleave. The
// counting sink is deliberately unguarded — under -race it is the detector that
// reports a failure, and maxLive reports it without one.
func TestSyncWriterSerialisesWrites(t *testing.T) {
	sink := &countingWriter{}
	w := &syncWriter{w: sink}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = w.Write([]byte("0123456789"))
			}
		}()
	}
	wg.Wait()
	if sink.maxLive != 1 {
		t.Fatalf("%d writers were inside the writer at once", sink.maxLive)
	}
	if sink.n != 8*50*10 {
		t.Fatalf("wrote %d bytes, want %d", sink.n, 8*50*10)
	}
	// A nil tty is the test path, and it must not panic: bubbletea asks for the
	// descriptor before it knows whether there is one.
	if fd := w.Fd(); fd != ^uintptr(0) {
		t.Fatalf("a writer with no file reported fd %d", fd)
	}
}

type countingWriter struct {
	n       int
	live    int
	maxLive int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.live++
	if c.live > c.maxLive {
		c.maxLive = c.live
	}
	c.n += len(p)
	c.live--
	return len(p), nil
}

// TestCtrlYWorksWithNoMouse: the flag turns the mouse off, not the keyboard.
func TestCtrlYWorksWithNoMouse(t *testing.T) {
	isolateSkillsHome(t)
	rec := captureCopies(t)
	m := New(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: t.TempDir(),
		Model:     "grok",
		Yolo:      true,
		NoMouse:   true,
	})
	if m.mouseEnabled {
		t.Fatal("--no-mouse should be kept on the model")
	}
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	tm, _ = m.Update(eventMsg{agent.Event{Type: agent.EventText, Text: "the reply"}})
	m = tm.(Model)

	tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	m = tm.(Model)
	if msg := runCmd(cmd); msg != nil {
		tm, _ = m.Update(msg)
		m = tm.(Model)
	}
	if copies := rec.copies(); len(copies) != 1 || copies[0] != "the reply" {
		t.Fatalf("Ctrl+Y copied %q under --no-mouse", copies)
	}
}
