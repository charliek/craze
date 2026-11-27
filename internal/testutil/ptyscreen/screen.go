// Package ptyscreen reads the visible terminal cells for the Go PTY tests.
package ptyscreen

import (
	"io"
	"os"
	"regexp"
	"testing"

	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

type Screen struct {
	cols, rows int
	output     []byte
	text       string
}

func New(t *testing.T, master *os.File) *Screen {
	t.Helper()
	// x/vt implements xterm's insert-character command. With TERM=dumb,
	// the renderer instead uses insert mode, which this emulator lacks.
	t.Setenv("TERM", "xterm-256color")
	size, err := pty.GetsizeFull(master)
	if err != nil {
		t.Fatal(err)
	}
	return &Screen{cols: int(size.Cols), rows: int(size.Rows)}
}

func (s *Screen) String() string { return s.text }

// x/vt v0.1.0 mistakes a UTF-8 continuation byte in the ✦ window title
// for the C1 string terminator. Titles occupy no terminal cells. Omit them
// through BEL, ST, or the current end of an incomplete title.
var windowTitle = regexp.MustCompile(`(?s)\x1b\]2;.*?(?:\x07|\x1b\\|\z)`)

// Feed replays the captured bytes together because x/vt flushes its last
// grapheme on each Write. A PTY read can split a single displayed character.
func (s *Screen) Feed(t *testing.T, data []byte) {
	if len(data) == 0 {
		return
	}
	t.Helper()
	s.output = append(s.output, data...)
	em := vt.NewEmulator(s.cols, s.rows)
	// This terminal observes output. It has no reader for capability replies.
	if err := em.InputPipe().(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := em.Write(windowTitle.ReplaceAll(s.output, nil)); err != nil {
		t.Fatal(err)
	}
	s.text = em.String()
}
