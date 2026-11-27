package tui

import (
	"os"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
	"github.com/charmbracelet/x/term"
	"github.com/rivo/uniseg"
	"golang.org/x/sys/unix"
)

// Bubble Tea's decoder consumes printable ASCII before it checks for a
// grapheme. For example, it would deliver a keycap one as the permission
// shortcut "1", followed by its combining marks. Send an ASCII-led composed
// character as one literal text insertion. A plain shortcut is immediate.
// Escape sequences and existing bracketed pastes pass through untouched.
type graphemeInput struct {
	*os.File
	parser     ansi.Parser
	pasting    bool
	endMatch   int
	mouseBytes int
	tail       []byte
}

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

func terminalInput() (*graphemeInput, func(), error) {
	if term.IsTerminal(os.Stdin.Fd()) {
		return newGraphemeInput(os.Stdin), func() {}, nil
	}
	// Mirror Bubble Tea's default when stdin is redirected. The controlling
	// terminal needs the same input semantics as a terminal on stdin.
	in, out, err := tea.OpenTTY()
	if err != nil {
		return nil, nil, err
	}
	_ = out.Close()
	return newGraphemeInput(in), func() { _ = in.Close() }, nil
}

func newGraphemeInput(f *os.File) *graphemeInput {
	in := &graphemeInput{File: f}
	// Two parameters distinguish an exact bracketed-paste opener from a
	// modified CSI key. OSC and DCS payloads never need to be stored.
	in.parser.SetParamsSize(2)
	in.parser.SetHandler(ansi.Handler{HandleCsi: in.handleCSI})
	return in
}

func (in *graphemeInput) handleCSI(cmd ansi.Cmd, params ansi.Params) {
	switch cmd {
	case 'M':
		// An X10 mouse report follows CSI M with three opaque bytes.
		if len(params) == 0 {
			in.mouseBytes = 3
		}
	case '~':
		in.pasting = len(params) == 1 && params[0] == 200
	}
}

func (in *graphemeInput) Read(p []byte) (int, error) {
	// The shortest affected cluster is three bytes; its two markers add
	// twelve. Reserve the maximum expansion in this read, so cancelreader
	// can continue to poll the underlying terminal before every Read.
	room := len(p) / 5
	if room <= utf8.UTFMax {
		// The terminal scanner supplies a 4 KiB buffer. Honour smaller
		// buffers too, without replaying a held suffix out of order.
		if len(in.tail) > 0 {
			n := copy(p, in.tail)
			in.tail = in.tail[n:]
			return n, nil
		}
		return in.File.Read(p)
	}
	raw := p[len(p)-room:]
	held := copy(raw, in.tail)
	in.tail = in.tail[:0]
	n, err := in.File.Read(raw[held:])
	n += held
	if n == room {
		// Keep a possible ASCII base with the start of its UTF-8 mark
		// when more of this burst is queued. A standalone shortcut never
		// has to wait for the next keystroke.
		start := asciiBaseAtEnd(raw[:n])
		if start < n && in.hasMore() {
			in.tail = append(in.tail, raw[start:n]...)
			n = start
		}
	}
	return len(in.literalClusters(p[:0], raw[:n])), err
}

// asciiBaseAtEnd finds a printable ASCII byte followed by no bytes or by an
// incomplete UTF-8 encoding. A complete non-ASCII rune is already available
// to the grapheme parser.
func asciiBaseAtEnd(raw []byte) int {
	for i := len(raw) - 1; i >= max(0, len(raw)-utf8.UTFMax); i-- {
		if raw[i] < utf8.RuneSelf {
			if raw[i] >= ' ' && raw[i] < 0x7f && (i+1 == len(raw) || !utf8.FullRune(raw[i+1:])) {
				return i
			}
			break
		}
	}
	return len(raw)
}

func (in *graphemeInput) hasMore() bool {
	fds := []unix.PollFd{{Fd: int32(in.Fd()), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, 0)
		if err != unix.EINTR {
			return err == nil && n > 0 && fds[0].Revents&unix.POLLIN != 0
		}
	}
}

func (in *graphemeInput) literalClusters(out, raw []byte) []byte {
	for len(raw) > 0 {
		// An X10 mouse report follows CSI M with three opaque bytes.
		// Its button/coordinate bytes can look exactly like composed text.
		if in.mouseBytes > 0 {
			n := min(in.mouseBytes, len(raw))
			out = append(out, raw[:n]...)
			raw = raw[n:]
			in.mouseBytes -= n
			continue
		}
		if in.pasting {
			b := raw[0]
			raw = raw[1:]
			out = append(out, b)
			if b == pasteEnd[in.endMatch] {
				in.endMatch++
				if in.endMatch == len(pasteEnd) {
					in.pasting = false
					in.endMatch = 0
				}
			} else {
				in.endMatch = 0
				if b == pasteEnd[0] {
					in.endMatch = 1
				}
			}
			continue
		}
		// The ordinary keyboard path is a cheap ASCII test. Grapheme
		// segmentation is only needed when a non-ASCII rune follows.
		if in.parser.State() == parser.GroundState && raw[0] >= ' ' && raw[0] < 0x7f && len(raw) > 1 && raw[1] >= utf8.RuneSelf {
			cluster, _, _, _ := uniseg.FirstGraphemeCluster(raw, -1)
			if len(cluster) > 1 {
				out = append(out, pasteStart...)
				out = append(out, cluster...)
				out = append(out, pasteEnd...)
				raw = raw[len(cluster):]
				continue
			}
		}
		// The streaming parser remembers the kind of control string between
		// reads, including an OSC terminated by BEL in a later read.
		in.parser.Advance(raw[0])
		out = append(out, raw[0])
		raw = raw[1:]
	}
	return out
}
