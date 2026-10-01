package opencode

import (
	"bytes"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi/parser"

	"github.com/charliek/craze/internal/harness/redact"
)

// The bash tool's escape-sequence stripper (plan 033 §3.6). A command's output
// reaches the model, the progress snapshots and the spill file without its
// terminal escape sequences: they cost tokens, carry nothing a model can use,
// and — left in — can split a provider key the redactor would otherwise have
// caught (`sk-…\x1b[0m…`). The environment already asks commands for no colour
// (TERM=dumb, NO_COLOR=1: noPrompt in bash.go); this is what a command that
// ignores it still meets.
//
// It is x/ansi's DEC parser (github.com/charmbracelet/x/ansi/parser: the
// VT500 transition table ansi.Strip runs, which the TUI's own display paths
// use), driven one character at a time instead of one byte at a time, made
// streaming, and bounded:
//
//   - What goes: every CSI, OSC, DCS, SOS, PM and APC sequence, whole; every
//     other ESC sequence (a charset, a keypad mode, ST); and every C1 control.
//     C0 controls stay, as ansi.Strip keeps them — \n, \t and \r among them,
//     also when one arrives inside a CSI or ESC sequence, where a terminal
//     executes it too.
//   - C1 controls are code points (U+0080–U+009F, two bytes in UTF-8), never
//     raw bytes. A byte from 0x80 to 0x9F is a continuation byte in UTF-8, so
//     the byte table would read the 0x9B inside "Û" (C3 9B) as a CSI and the
//     0x9C inside "✜" (E2 9C 9C) as the end of an OSC. The stripper decodes
//     each character first: a C1 code point is fed to the table as its byte,
//     printable ASCII and the C0 controls as themselves, and anything else —
//     a character from U+00A0 up, or one byte that is not valid UTF-8 — is
//     text: written as it is in the ground state, part of the payload inside
//     an OSC, DCS, SOS, PM or APC string, and the end of any other sequence it
//     interrupts, which is dropped while the character is kept (ansi.Strip
//     does the same). Invalid bytes are kept as they are, but for a 0xC2
//     that starts no character, which becomes U+FFFD (text says why): so the
//     output never holds a C1 control, whatever the input.
//   - Streaming: the parser's state carries from one Write to the next, so a
//     sequence that a read of the pipe cuts in two is still stripped whole,
//     and the output is the same however the input is cut. The bytes of a
//     sequence are dropped as they are read, never held, so the only bytes
//     held back are the start of a character a Write ends inside — at most
//     three (utf8.UTFMax-1), since only the whole character says whether it
//     is a C1 control. Close writes them out, each as an invalid byte,
//     before the redactor's Close (modelStream).
//   - Bounded: a sequence still open after maxSequence bytes is abandoned —
//     those bytes are dropped and the parser returns to the ground state, so
//     what follows is text again. A stray "\x1b]" in a file, or an OSC whose
//     BEL never comes, therefore costs at most maxSequence bytes of output,
//     not the rest of it. The price: an escape sequence longer than that — a
//     long OSC 8 hyperlink, an inline image — shows its tail as text; with
//     TERM=dumb and no terminal on the pipe, commands do not send those.
//
// Its state is a few bytes and the held-back character, whatever the input,
// so it allocates only its output buffer, which a Write reuses: at most the
// Write's input plus the three held-back bytes.
type ansiStripper struct {
	w       io.Writer
	state   parser.State // the DEC parser's state: parser.GroundState outside a sequence
	seq     int          // bytes of the open sequence read so far; 0 in the ground state
	pending []byte       // the start of a character the last Write ended inside: at most utf8.UTFMax-1 bytes
	out     []byte       // reused for each Write's output
	err     error
	closed  bool
}

// maxSequence is the most bytes an escape sequence may hold before the
// stripper abandons it (above). CSI, ESC and the short OSC sequences a
// command writes — colours, cursor moves, titles, short hyperlinks — are far
// shorter.
const maxSequence = 256

// errStripperClosed is ansiStripper.Write's error after Close.
var errStripperClosed = errors.New("opencode: write to the escape-sequence stripper after close")

func newANSIStripper(w io.Writer) *ansiStripper {
	return &ansiStripper{w: w, state: parser.GroundState}
}

// Write strips p and writes what is left to the underlying writer. It
// reports len(p) once p is taken, although up to three bytes of it may be
// held back until the next Write or Close. An error from the underlying
// writer is returned, with 0, and every later Write and Close returns it
// again.
func (s *ansiStripper) Write(p []byte) (int, error) {
	switch {
	case s.err != nil:
		return 0, s.err
	case s.closed:
		return 0, errStripperClosed
	}
	// Most output holds no escape at all. In the ground state, with nothing
	// held back, only ESC and a C1 control (whose UTF-8 form starts with
	// 0xC2) change anything: every other byte, a character the Write cuts
	// in two included, is written as it came either way.
	if s.state == parser.GroundState && len(s.pending) == 0 &&
		bytes.IndexByte(p, 0x1b) < 0 && bytes.IndexByte(p, 0xc2) < 0 {
		if err := s.put(p); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	s.out = s.out[:0]
	rest := p
	// First finish the character the last Write ended inside, a byte at a
	// time (it needs at most three more). Its bytes may yet prove not to be
	// one character, and the last of them the start of another: whatever
	// feed cannot decode yet stays held back.
	for len(s.pending) > 0 && len(rest) > 0 {
		s.pending = append(s.pending, rest[0])
		rest = rest[1:]
		if utf8.FullRune(s.pending) {
			held := s.feed(s.pending, false)
			s.pending = append(s.pending[:0], s.pending[len(s.pending)-held:]...)
		}
	}
	if len(rest) > 0 {
		held := s.feed(rest, false)
		s.pending = append(s.pending, rest[len(rest)-held:]...)
	}
	if err := s.put(s.out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close writes out the held-back bytes, the stream having ended: a
// character's start that never got its end is written as the invalid bytes
// it is (text).
// A sequence still open is dropped, as all of its bytes already were. It
// does not close the underlying writer. A second Close returns what the
// first did.
func (s *ansiStripper) Close() error {
	if s.closed || s.err != nil {
		return s.err
	}
	s.closed = true
	if len(s.pending) == 0 {
		return nil
	}
	s.out = s.out[:0]
	s.feed(s.pending, true)
	s.pending = s.pending[:0]
	return s.put(s.out)
}

// feed strips b onto s.out and returns how many bytes at its end it could not
// decode yet: the start of a character that b ends inside. With final set
// there is no more input, and it returns 0: such bytes are decoded one by one,
// each as an invalid byte.
func (s *ansiStripper) feed(b []byte, final bool) (held int) {
	for i := 0; i < len(b); {
		if c := b[i]; c < utf8.RuneSelf {
			s.step(c, b[i:i+1])
			i++
			continue
		}
		if !final && !utf8.FullRune(b[i:]) {
			return len(b) - i
		}
		r, size := utf8.DecodeRune(b[i:])
		if r >= 0x80 && r <= 0x9f {
			s.step(byte(r), b[i:i+size]) // a C1 control: the table knows it by its byte
		} else {
			s.text(b[i : i+size]) // a character from U+00A0 up, or one invalid byte
		}
		i += size
	}
	return 0
}

// step feeds the table one ASCII byte or one C1 control (code), whose bytes
// in the input are raw. Printable ASCII in the ground state is written, and so
// is a C0 control the table executes, wherever it comes; nothing else is: a
// C1 control the table executes is dropped like the rest of a sequence.
func (s *ansiStripper) step(code byte, raw []byte) {
	next, action := parser.Table.Transition(s.state, code)
	switch action {
	case parser.PrintAction:
		s.out = append(s.out, raw...)
	case parser.ExecuteAction:
		if code < 0x80 {
			s.out = append(s.out, code)
		}
	}
	// ESC, or a C1 control, ends whatever sequence was open and starts the
	// next — except inside a string that takes it as payload (an OSC or DCS
	// string takes C1 controls; a DCS's first byte may be ESC).
	starts := (code == 0x1b || code >= 0x80) && action != parser.PutAction
	switch {
	case next == parser.GroundState:
		s.seq = 0
	case starts:
		s.seq = len(raw)
	default:
		s.seq += len(raw)
	}
	s.state = next
	s.bound()
}

// text takes one character from U+00A0 up, or one invalid byte: text in the
// ground state, payload inside a string, and the end of any other sequence,
// which is dropped while the character is kept.
func (s *ansiStripper) text(raw []byte) {
	switch s.state {
	case parser.OscStringState, parser.DcsStringState, parser.SosStringState, parser.PmStringState, parser.ApcStringState:
		s.seq += len(raw)
		s.bound()
		return
	case parser.GroundState:
	default:
		s.state, s.seq = parser.GroundState, 0
	}
	if len(raw) == 1 && raw[0] == 0xc2 {
		// The one byte changed: a 0xC2 that starts no character. Written as
		// it is, a stray continuation byte after a dropped sequence — "\xc2",
		// then "\x1b[m", then "\x9b" — would complete it into a C1 control
		// (U+009B) that the input never held. Only 0xC2 leads a C1
		// control's encoding, so this is the only such join.
		s.out = append(s.out, string(utf8.RuneError)...)
		return
	}
	s.out = append(s.out, raw...)
}

// bound abandons a sequence that has reached maxSequence bytes: what it read
// is dropped, and what follows is read from the ground state.
func (s *ansiStripper) bound() {
	if s.seq >= maxSequence {
		s.state, s.seq = parser.GroundState, 0
	}
}

// put writes b to the underlying writer, if there is anything to write, and
// keeps its error for every later call.
func (s *ansiStripper) put(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if _, err := s.w.Write(b); err != nil {
		s.err = err
		return err
	}
	return nil
}

// modelStream is the way a command's output takes to output (plan 033 §3.6):
// redacted as the command wrote it, stripped of escape sequences, and
// redacted again. The second pass is the plan's: stripping can join a key an
// escape sequence had split (`sk-…\x1b[0m…`), and only a redactor after the
// stripper sees it whole. The first is craze's addition: stripping can also
// break a key the raw output held whole, since a sequence can end on the key's
// first character — "\x1b" or "\x1b[" before "sk-…" is a complete escape
// sequence ending in "s", and the stripper drops it with the "s", leaving
// "k-…", which no redactor matches — so the key is caught before the stripper
// can touch it. What leaves holds no key either pass could see whole; a
// marker the stripper cuts the start off ("\x1b" before a marker reads as
// "\x1b[c") is left as the rest of the marker, which is no key.
//
// Each stage holds back what it must (the redactors the longest key's length
// less one byte each, the stripper at most three bytes), so the progress
// snapshots lag the command's output by that much. Close writes it all out,
// stage by stage, in order.
type modelStream struct {
	raw   *redact.Writer // keys as the command wrote them
	strip *ansiStripper
	keys  *redact.Writer // keys the stripping joined
}

func newModelStream(r *redact.Replacer, out io.Writer) *modelStream {
	keys := r.NewWriter(out)
	strip := newANSIStripper(keys)
	return &modelStream{raw: r.NewWriter(strip), strip: strip, keys: keys}
}

func (m *modelStream) Write(p []byte) (int, error) { return m.raw.Write(p) }

// Close writes out what each stage holds back, the first stage's into the
// second before the second's own, and returns the first error. It does not
// close out.
func (m *modelStream) Close() error {
	err := m.raw.Close()
	if e := m.strip.Close(); err == nil {
		err = e
	}
	if e := m.keys.Close(); err == nil {
		err = e
	}
	return err
}
