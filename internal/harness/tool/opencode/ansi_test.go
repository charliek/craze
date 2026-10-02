package opencode

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/redact"
)

// stripCases are the escape-sequence stripper's cases: input, and what is
// left of it (plan 033 §3.6). The ones that keep their input are the
// negative controls: text the stripper must not touch.
var stripCases = []struct {
	name, in, want string
}{
	{"plain text is kept", "plain\ttext\r\nline two\n", "plain\ttext\r\nline two\n"},
	{"SGR colour", "\x1b[31mred\x1b[0m and \x1b[1;38;5;208mbold\x1b[m\n", "red and bold\n"},
	{"cursor and erase", "\x1b[?25l\x1b[2K\r\x1b[1;31Hx\x1b[?25h", "\rx"},
	{"OSC ended by BEL", "\x1b]0;a title\x07after", "after"},
	{"OSC 8 hyperlink ended by ST", "\x1b]8;;https://example.invalid/a\x1b\\link\x1b]8;;\x1b\\.", "link."},
	{"OSC payload in UTF-8 holding 0x9C bytes", "\x1b]0;✜ title ✜\x07ok", "ok"},
	{"DCS", "\x1bPq#0;2;0;0;0#0~~@@\x1b\\after", "after"},
	{"DCS passthrough holding ESC", "\x1bP\x1b[31m\x1b\\x", "x"},
	{"APC", "\x1b_Gf=100;AAAA\x1b\\x", "x"},
	{"SOS and PM", "\x1bXsos\x1b\\\x1b^pm\x1b\\y", "y"},
	{"charset and keypad modes", "\x1b(B\x1b=x\x1b>", "x"},
	{"C1 CSI as a code point", "\u009b31mred\u009b0m", "red"},
	{"C1 OSC ended by C1 ST", "\u009d0;t\u009cz", "z"},
	{"other C1 controls", "a\u0085b\u0084c\u008ed", "abcd"},
	{"0x9B inside a valid character", "Û[31m ✛[0m", "Û[31m ✛[0m"},
	{"0x9C inside a valid character", "✜ x", "✜ x"},
	{"a raw 0x9B byte is no CSI", "a\x9b31mb", "a\x9b31mb"},
	{"a raw 0x9D byte is no OSC", "a\x9d0;tb\x07c", "a\x9d0;tb\x07c"},
	{"C0 controls are kept", "a\x07b\x08c\x00d\x7f", "a\x07b\x08c\x00d\x7f"},
	{"a C0 control inside a CSI is kept", "\x1b[3\n1mX", "\nX"},
	{"a character ends a CSI it interrupts", "\x1b[31é", "é"},
	{"a character in an OSC is payload", "\x1b]0;é\x07", ""},
	{"invalid bytes are kept", "a\xffb\xe2\x82c", "a\xffb\xe2\x82c"},
	{"a lone 0xC2 cannot join a stray byte into C1", "\xc2\x1b[m\x9b", "�\x9b"},
	{"a sequence open at the end is dropped", "ok\x1b[31", "ok"},
	{"an OSC open at the end is dropped", "ok\x1b]0;tit", "ok"},
	{"a character open at the end is kept", "ok\xe2\x82", "ok\xe2\x82"},
	{"ESC ESC", "\x1b\x1b[31mx", "x"},
	{"CAN aborts a sequence and is kept", "\x1b[3\x18x", "\x18x"},
	// The bound: a sequence still open after maxSequence bytes is
	// abandoned, and what follows is text. One byte shorter, the OSC
	// completes and goes whole.
	{"an OSC within the bound", "\x1b]" + strings.Repeat("x", maxSequence-3) + "\x07after", "after"},
	{"an OSC past the bound", "\x1b]" + strings.Repeat("x", maxSequence-2) + "\x07after", "\x07after"},
	{"an unterminated OSC costs the bound", "\x1b]" + strings.Repeat("x", maxSequence-2) + "tail\n", "tail\n"},
	{"a CSI past the bound", "\x1b[" + strings.Repeat("1", 300) + "mX", strings.Repeat("1", 300-(maxSequence-2)) + "mX"},
}

// strip runs in through a stripper in pieces of the given sizes (cycling;
// none means all at once), then closes it, checking at each Write that it
// holds back no more than a character's start.
func strip(t testing.TB, in string, sizes []int) string {
	t.Helper()
	var out bytes.Buffer
	s := newANSIStripper(&out)
	for i, rest := 0, []byte(in); len(rest) > 0; i++ {
		n := len(rest)
		if len(sizes) > 0 {
			n = min(sizes[i%len(sizes)], n)
		}
		if got, err := s.Write(rest[:n]); got != n || err != nil {
			t.Fatalf("Write = %d, %v; want %d, nil", got, err, n)
		}
		if len(s.pending) >= utf8.UTFMax {
			t.Fatalf("the stripper holds back %d bytes, more than a character's start", len(s.pending))
		}
		rest = rest[n:]
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestANSIStripper(t *testing.T) {
	for _, tc := range stripCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strip(t, tc.in, nil); got != tc.want {
				t.Fatalf("strip(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Every cut in two, and every cut into single bytes, gives the
			// same.
			for i := 1; i < len(tc.in); i++ {
				if got := strip(t, tc.in, []int{i, len(tc.in)}); got != tc.want {
					t.Fatalf("cut at %d: %q, want %q", i, got, tc.want)
				}
			}
			if got := strip(t, tc.in, []int{1}); got != tc.want {
				t.Fatalf("byte by byte: %q, want %q", got, tc.want)
			}
		})
	}
}

// TestANSIStripperRandomCuts: random text over the bytes escape sequences
// are made of, cut at random, strips as it does whole. The fuzz target
// explores further with -fuzz; this runs with every go test.
func TestANSIStripperRandomCuts(t *testing.T) {
	alphabet := []string{"\x1b", "[", "]", "P", "X", "^", "_", "\\", "\x07", "m", "1", ";", "a", "\n", "é", "Û",
		"\u009b", "\u009c", "\u009d", "\xc2", "\x9b", "\xe2\x82", "\x18", "(", "B"}
	rng := rand.New(rand.NewPCG(33, 8))
	for range 3000 {
		var b strings.Builder
		for range rng.IntN(80) {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		in := b.String()
		want := strip(t, in, nil)
		sizes := make([]int, 1+rng.IntN(4))
		for i := range sizes {
			sizes[i] = 1 + rng.IntN(7)
		}
		if got := strip(t, in, sizes); got != want {
			t.Fatalf("%q cut %v: %q, whole: %q", in, sizes, got, want)
		}
		checkStripped(t, in, want)
	}
}

// checkStripped asserts what holds of any stripper output: no ESC, no C1
// control, valid UTF-8 for valid input, and the input itself when it held
// neither ESC nor a C1 control — raw 0x80-0x9F bytes, inside characters or
// not, included.
func checkStripped(t testing.TB, in, out string) {
	t.Helper()
	if strings.IndexByte(out, 0x1b) >= 0 {
		t.Fatalf("%q: the output holds ESC: %q", in, out)
	}
	for _, r := range out {
		if r >= 0x80 && r <= 0x9f {
			t.Fatalf("%q: the output holds the C1 control %U: %q", in, r, out)
		}
	}
	if utf8.ValidString(in) && !utf8.ValidString(out) {
		t.Fatalf("valid UTF-8 %q became invalid: %q", in, out)
	}
	clean := strings.IndexByte(in, 0x1b) < 0 && !strings.ContainsFunc(in, func(r rune) bool { return r >= 0x80 && r <= 0x9f })
	if clean && !loneC2(in) && out != in {
		t.Fatalf("%q holds nothing to strip, and became %q", in, out)
	}
}

// loneC2 reports whether s holds a 0xC2 that starts no character: the one
// byte the stripper changes (to U+FFFD).
func loneC2(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 && s[i] == 0xc2 {
			return true
		}
		i += size
	}
	return false
}

// FuzzANSIStripper: for any input and any way of cutting it, the stream
// equals the whole-value pass, never panics, holds back at most a
// character's start, and leaves no ESC and no C1 control. Run with -fuzz to
// explore; the seeds run with every go test.
func FuzzANSIStripper(f *testing.F) {
	for _, tc := range stripCases {
		f.Add([]byte(tc.in), []byte{3, 7})
	}
	f.Add([]byte("\xc2"), []byte{1})
	f.Add([]byte("x\xe2\x9c\x9b\x1b]\xe2\x9c\x9c\x07"), []byte{1, 2})
	f.Fuzz(func(t *testing.T, in, cuts []byte) {
		if len(in) > 1<<16 {
			return
		}
		sizes := make([]int, 0, len(cuts))
		for _, c := range cuts {
			sizes = append(sizes, 1+int(c%32))
		}
		want := strip(t, string(in), nil)
		if got := strip(t, string(in), sizes); got != want {
			t.Fatalf("cut %v: %q, whole: %q", sizes, got, want)
		}
		checkStripped(t, string(in), want)
	})
}

// TestANSIStripperHoldsNoSequence: an endless escape sequence costs the
// stripper nothing but its state — none of its bytes are kept — and the
// output buffer a Write reuses never grows past that Write.
func TestANSIStripperHoldsNoSequence(t *testing.T) {
	var out bytes.Buffer
	s := newANSIStripper(&out)
	chunk := []byte("\x1b]" + strings.Repeat("y", 4094))
	for range 256 { // 1 MiB in all
		if _, err := s.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if len(s.pending) != 0 || cap(s.out) > 2*len(chunk) {
			t.Fatalf("pending %d bytes, out buffer %d bytes", len(s.pending), cap(s.out))
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Each chunk's OSC is abandoned at the bound, and the rest of its y's
	// are text: what is left is the y's past the bound, chunk by chunk.
	if want := strings.Repeat(strings.Repeat("y", len(chunk)-maxSequence), 256); out.String() != want {
		t.Fatalf("output is %d bytes, want %d", out.Len(), len(want))
	}
}

// failingWriter fails every Write.
type failingWriter struct{ n int }

func (f *failingWriter) Write([]byte) (int, error) { f.n++; return 0, errors.New("full") }

// TestANSIStripperKeepsItsError: an underlying writer's error is returned,
// and again by every later Write and Close; a Write after Close is refused.
func TestANSIStripperKeepsItsError(t *testing.T) {
	w := &failingWriter{}
	s := newANSIStripper(w)
	if n, err := s.Write([]byte("abc")); n != 0 || err == nil {
		t.Fatalf("Write = %d, %v; want 0 and the writer's error", n, err)
	}
	if _, err := s.Write([]byte("\x1b[mabc")); err == nil || w.n != 1 {
		t.Fatalf("a later Write = %v after %d writes; want the kept error, no new write", err, w.n)
	}
	if err := s.Close(); err == nil {
		t.Fatal("Close lost the error")
	}

	// A character's start a Write ends inside is held back (the escape
	// before it keeps the Write off the fast path), and Close writes it.
	var out bytes.Buffer
	s = newANSIStripper(&out)
	if _, err := s.Write([]byte("\x1b[ma\xe2")); err != nil {
		t.Fatal(err)
	}
	if len(s.pending) != 1 || out.String() != "a" {
		t.Fatalf("after the Write: pending %q, output %q; want the 0xE2 held back", s.pending, out.String())
	}
	if err := s.Close(); err != nil || out.String() != "a\xe2" {
		t.Fatalf("Close = %v with %q, want the held-back byte written", err, out.String())
	}
	if err := s.Close(); err != nil {
		t.Fatalf("a second Close = %v", err)
	}
	if _, err := s.Write([]byte("b")); !errors.Is(err, errStripperClosed) {
		t.Fatalf("Write after Close = %v", err)
	}
}

// streamed runs text through a modelStream over r in pieces of the given
// sizes (cycling), then closes it.
func streamed(t testing.TB, r *redact.Replacer, text string, sizes []int) string {
	t.Helper()
	var out bytes.Buffer
	m := newModelStream(r, &out)
	for i, rest := 0, []byte(text); len(rest) > 0; i++ {
		n := min(sizes[i%len(sizes)], len(rest))
		if _, err := m.Write(rest[:n]); err != nil {
			t.Fatal(err)
		}
		rest = rest[n:]
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// planOrder is the stream the plan drew (plan 033 §3.6): the stripper, then
// one redactor. modelStream adds a redactor before the stripper; the tests
// below show why, with this as their control.
func planOrder(t testing.TB, r *redact.Replacer, text string) string {
	t.Helper()
	var out bytes.Buffer
	keys := r.NewWriter(&out)
	s := newANSIStripper(keys)
	if _, err := s.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(s.Close(), keys.Close()); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// TestModelStreamRedactsAcrossEscapes: a key an escape sequence splits is
// redacted once the stripper has joined it, however the stream is cut. Its
// control: stripped but not redacted, the key is whole in the output, so
// the stripper really joined it.
func TestModelStreamRedactsAcrossEscapes(t *testing.T) {
	r := redact.New(keyA)
	split := "before sk-canary-\x1b[0malpha-0001 after\n"
	want := "before " + redact.Marker + " after\n"
	for _, sizes := range [][]int{{len(split)}, {1}, {3, 7}, {12, 1}, {17, 2, 5}} {
		if got := streamed(t, r, split, sizes); got != want {
			t.Fatalf("cut %v: %q, want %q", sizes, got, want)
		}
	}
	if got := strip(t, split, nil); !strings.Contains(got, keyA) {
		t.Fatalf("control: stripping %q does not join the key: %q", split, got)
	}
}

// TestModelStreamRedactsBeforeStripping: a sequence can end on a key's first
// character — "\x1b" then "s" is a complete ESC sequence, and so is "\x1b["
// then "s" — so stripping alone would leave "k-canary-alpha-0001", which no
// redactor matches. modelStream redacts before it strips, and nothing of the
// key is left. The control is the plan's order, which leaks the rest of the
// key.
func TestModelStreamRedactsBeforeStripping(t *testing.T) {
	r := redact.New(keyA)
	rest := keyA[1:]
	for _, text := range []string{"x\x1b" + keyA + "\n", "x\x1b[" + keyA + "\n", "x\u009b" + keyA + "\n"} {
		for _, sizes := range [][]int{{len(text)}, {1}, {2, 5}} {
			if got := streamed(t, r, text, sizes); strings.Contains(got, rest) || strings.Contains(got, "canary") {
				t.Fatalf("%q cut %v leaks the key: %q", text, sizes, got)
			}
		}
		if got := planOrder(t, r, text); !strings.Contains(got, rest) {
			t.Fatalf("control: the plan's order does not leak %q from %q: %q", rest, text, got)
		}
	}
}

// FuzzModelStream: whatever the text and its cuts, what leaves the stream
// holds no key, and the stream equals itself uncut.
func FuzzModelStream(f *testing.F) {
	f.Add("sk-canary-\x1b[0malpha-0001", uint8(3), uint8(7))
	f.Add("\x1b"+keyA+"\x1b]0;"+keyA+"\x07"+keyA, uint8(1), uint8(1))
	f.Add("sk-canary-\u009b0malpha-0001\x1b[", uint8(2), uint8(5))
	r := redact.New(keyA, "aaaaaaaa")
	f.Fuzz(func(t *testing.T, text string, a, b uint8) {
		whole := streamed(t, r, text, []int{len(text) + 1})
		if got := streamed(t, r, text, []int{1 + int(a%32), 1 + int(b%32)}); got != whole {
			t.Fatalf("cut: %q, whole: %q", got, whole)
		}
		if strings.Contains(whole, keyA) || strings.Contains(whole, "aaaaaaaa") {
			t.Fatalf("the output holds a key: %q", whole)
		}
	})
}

// TestModelStreamWidensBothStages (plan 033 C10r, review r7 finding 9): a
// stream widened as it runs — a bash job's, when the session learns a key —
// redacts the new key after the Widen as if it had known it from the start:
// split by an escape sequence (the second redactor's case), with a sequence
// eating its first byte (the first one's), and split across writes. The
// controls widen one stage only, and each leaks one of the cases: both stages
// must take the key, together.
func TestModelStreamWidensBothStages(t *testing.T) {
	texts := []string{
		"before sk-canary-\x1b[0malpha-0001 after\n", // joined by the stripper: the second stage's
		"x\x1b" + keyA + "\n",                        // its "s" eaten with the escape: the first stage's
		"k=" + keyA + "\n",
	}
	run := func(text string, widen func(*modelStream, *redact.Replacer)) string {
		var out bytes.Buffer
		m := newModelStream(redact.New(), &out) // knew no key when it started
		if _, err := m.Write([]byte("started\n")); err != nil {
			t.Fatal(err)
		}
		widen(m, redact.New(keyA))
		for _, cut := range []string{text[:len(text)/2], text[len(text)/2:]} {
			if _, err := m.Write([]byte(cut)); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	leaks := func(out string) bool { return strings.Contains(out, keyA[1:]) || strings.Contains(out, "canary") }
	for _, text := range texts {
		got := run(text, (*modelStream).Widen)
		if leaks(got) || got != "started\n"+streamed(t, redact.New(keyA), text, []int{len(text)}) {
			t.Fatalf("%q after a Widen: %q; want it as a stream that knew the key", text, got)
		}
	}
	rawOnly := func(m *modelStream, r *redact.Replacer) { m.raw.Widen(r) }
	keysOnly := func(m *modelStream, r *redact.Replacer) { m.keys.Widen(r) }
	if got := run(texts[0], rawOnly); !leaks(got) {
		t.Fatalf("control: widening the first stage alone caught a key the stripper joined: %q", got)
	}
	if got := run(texts[1], keysOnly); !leaks(got) {
		t.Fatalf("control: widening the second stage alone caught a key an escape ate the start of: %q", got)
	}
}

// TestModelStreamWidenDuringWrites: Widen comes from the session's goroutines
// while the reader writes; under -race this is the proof that the stream's
// lock covers both. Every key widened in before a write began is redacted in
// it.
func TestModelStreamWidenDuringWrites(t *testing.T) {
	var out bytes.Buffer // written under the stream's lock, read once it is closed
	m := newModelStream(redact.New(), &out)
	m.Widen(redact.New(keyA))
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 300 {
			if _, err := m.Write([]byte("tick sk-canary-\x1b[0malpha-0001\n")); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Go(func() {
		for i := range 100 {
			m.Widen(redact.New(keyA, fmt.Sprintf("zq-learned-key-%04d", i)))
		}
	})
	wg.Wait()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "canary") {
		t.Fatal("a key known before every write survived")
	}
}
