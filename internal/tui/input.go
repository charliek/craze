package tui

import (
	"bytes"
	"os"
)

// shiftEnterSeqs are the two Shift+Enter sequences craze reads: xterm's
// modifyOtherKeys form, which Ghostty — and Roost, built on libghostty — sends
// for Shift+Enter whether or not the program asked for anything, and the kitty
// keyboard protocol's form. A terminal that sends neither (most send a bare
// \r, unless the program turns a keyboard protocol on or the user remaps the
// key) gives craze no Shift+Enter to tell from Enter.
var shiftEnterSeqs = [][]byte{[]byte("\x1b[27;2;13~"), []byte("\x1b[13;2u")}

// The bracketed paste markers: what a terminal sends around pasted text once
// the program turns bracketed paste on, as bubbletea does.
var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// shiftEnterInput is the terminal as bubbletea reads it, with each Shift+Enter
// sequence typed made a ctrl+j (\n) before bubbletea parses it: ctrl+j is the
// newline key everything that reads one (isNewlineKey, the textarea's
// InsertNewline) already takes, so Shift+Enter does what ctrl+j does,
// wherever the keyboard is.
//
// bubbletea v1 has no Shift+Enter key. It reports a CSI sequence it does not
// know as an unexported message and makes no key of it, so the key was lost
// without a trace. That message cannot be caught on its way to Update instead:
// its bytes are a slice of the read loop's one buffer (bubbletea key.go
// readAnsiInputs), which the next read overwrites as soon as the message is
// handed over — before Update has necessarily looked at it.
//
// Raw mode and cancelreader still see the terminal: bubbletea puts the
// descriptor in raw mode and restores it (tty_unix.go), and cancelreader waits
// on it with epoll or kqueue before it calls Read. tea.ExecProcess would not:
// os/exec hands a child's stdin through a pipe unless it is an *os.File, so a
// command run that way must be given os.Stdin itself.
type shiftEnterInput struct {
	f *os.File
	// pasting is whether the bytes read so far leave a bracketed paste open:
	// a paste is text, and its bytes reach bubbletea as they were pasted.
	pasting bool
	// recent is the last len(pasteStart) bytes passed on, across reads, so a
	// marker a read cut in two is still seen; a rewritten Shift+Enter counts
	// as the \n bubbletea gets for it.
	recent [6]byte
}

func (in *shiftEnterInput) Read(p []byte) (int, error) {
	n, err := in.f.Read(p)
	if n > 0 {
		// A read that filled p is passed on whole. It may end partway into a
		// key's sequence, which bubbletea finishes only when the read was
		// full (canHaveMoreData); shortening it would tell bubbletea the read
		// ended on a key's boundary when it may not have. Such a read is input
		// that piled up faster than craze took it, so a Shift+Enter inside
		// one, like one a read cut in two, is lost as it always was.
		n = in.rewrite(p[:n], n < len(p))
	}
	return n, err
}

func (in *shiftEnterInput) Write(p []byte) (int, error) { return in.f.Write(p) }
func (in *shiftEnterInput) Close() error                { return in.f.Close() }
func (in *shiftEnterInput) Fd() uintptr                 { return in.f.Fd() }
func (in *shiftEnterInput) Name() string                { return in.f.Name() }

// rewrite replaces each Shift+Enter sequence in b that is outside a paste
// with \n, in place, when may, and returns the length left. It follows the
// paste markers whether or not it may rewrite, so a read passed on whole
// still moves a paste's state.
func (in *shiftEnterInput) rewrite(b []byte, may bool) int {
	n := 0
	for i := 0; i < len(b); {
		if may && !in.pasting && b[i] == 0x1b {
			if l := shiftEnterAt(b[i:]); l > 0 {
				in.see('\n')
				b[n] = '\n'
				n++
				i += l
				continue
			}
		}
		in.see(b[i])
		b[n] = b[i]
		n++
		i++
	}
	return n
}

// see moves the paste state on by one byte read.
func (in *shiftEnterInput) see(c byte) {
	copy(in.recent[:], in.recent[1:])
	in.recent[len(in.recent)-1] = c
	switch {
	case bytes.Equal(in.recent[:], pasteStart):
		in.pasting = true
	case bytes.Equal(in.recent[:], pasteEnd):
		in.pasting = false
	}
}

// shiftEnterAt is the length of the Shift+Enter sequence b starts with, or 0.
func shiftEnterAt(b []byte) int {
	for _, seq := range shiftEnterSeqs {
		if bytes.HasPrefix(b, seq) {
			return len(seq)
		}
	}
	return 0
}
