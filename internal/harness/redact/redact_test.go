package redact

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The keys are obviously not secrets; each is long enough to clear
// modeltable's 8-byte floor, as every real key will.
const (
	keyA = "sk-canary-alpha-0001"
	keyB = "xai-canary-bravo-02"
)

// reference is the rule spelled out the slow way: find every occurrence of
// every key, overlapping ones included, merge the ones that overlap, and
// print one Marker per merged run. Replacer and Writer must agree with it.
func reference(keys []string, s string) string {
	type span struct{ start, end int }
	var spans []span
	for _, k := range keys {
		if k == "" {
			continue
		}
		for i := 0; i+len(k) <= len(s); i++ {
			if s[i:i+len(k)] == k {
				spans = append(spans, span{i, i + len(k)})
			}
		}
	}
	slices.SortFunc(spans, func(a, b span) int { return a.start - b.start })
	var out strings.Builder
	pos, runEnd := 0, -1
	for _, sp := range spans {
		if sp.start < runEnd { // overlaps the open run
			runEnd = max(runEnd, sp.end)
			continue
		}
		if runEnd >= 0 {
			pos = runEnd
		}
		out.WriteString(s[pos:sp.start])
		out.WriteString(Marker)
		runEnd = sp.end
	}
	if runEnd >= 0 {
		pos = runEnd
	}
	out.WriteString(s[pos:])
	return out.String()
}

func TestString(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		in   string
		want string
	}{
		{"no keys", nil, "a " + keyA, "a " + keyA},
		{"only empty keys", []string{"", ""}, "a " + keyA, "a " + keyA},
		{"no occurrence", []string{keyA}, "nothing to see", "nothing to see"},
		{"one near miss", []string{keyA}, "sk-canary-alpha-0002", "sk-canary-alpha-0002"},
		{"whole text", []string{keyA}, keyA, Marker},
		{"at the start", []string{keyA}, keyA + " tail", Marker + " tail"},
		{"at the very end", []string{keyA}, "head " + keyA, "head " + Marker},
		{"repeated", []string{keyA}, keyA + " and " + keyA, Marker + " and " + Marker},
		{"touching occurrences are two runs", []string{keyA}, keyA + keyA, Marker + Marker},
		{"two keys", []string{keyA, keyB}, keyB + "=" + keyA, Marker + "=" + Marker},
		{"a prefix at the end is no key", []string{keyA}, "x " + keyA[:len(keyA)-1], "x " + keyA[:len(keyA)-1]},
		{"duplicate keys collapse", []string{keyA, keyA, ""}, "k=" + keyA, "k=" + Marker},
		// The shorter key is inside the longer one: the longer wins, and no
		// fragment of it is left beside a marker.
		{"contained key", []string{"canary-alpha", keyA}, "k=" + keyA + ";c=canary-alpha", "k=" + Marker + ";c=" + Marker},
		// Two keys overlapping in the text: one run, one marker, and neither
		// key's tail survives — leftmost-first matching would leave "-0001".
		{"overlapping keys", []string{"abcdefgh", "efghijkl"}, "<abcdefghijkl>", "<" + Marker + ">"},
		{"overlapping keys either order", []string{"efghijkl", "abcdefgh"}, "<abcdefghijkl>", "<" + Marker + ">"},
		{"self-overlapping key", []string{"aaaaaaaa"}, "b" + strings.Repeat("a", 20) + "b", "b" + Marker + "b"},
		{"multibyte", []string{"ключ-секрет"}, "é ключ-секрет é", "é " + Marker + " é"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := New(tc.keys...).String(tc.in)
			if got != tc.want {
				t.Fatalf("String(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if ref := reference(tc.keys, tc.in); ref != tc.want {
				t.Fatalf("the reference says %q: the case's want is wrong", ref)
			}
			if got := streamed(New(tc.keys...), tc.in, []int{1}); got != tc.want {
				t.Fatalf("a byte-at-a-time Writer = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNilReplacerRedactsNothing(t *testing.T) {
	var r *Replacer
	if got := r.String(keyA); got != keyA {
		t.Fatalf("nil String = %q", got)
	}
	var buf bytes.Buffer
	w := r.NewWriter(&buf)
	if _, err := w.Write([]byte(keyA)); err != nil {
		t.Fatal(err)
	}
	// Nothing to hold back for: the bytes are through before Close.
	if buf.String() != keyA {
		t.Fatalf("nil Writer held back or changed the stream: %q", buf.String())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// streamed writes s to a Writer over r in chunks whose sizes cycle through
// sizes, and returns what reached the underlying writer after Close.
func streamed(r *Replacer, s string, sizes []int) string {
	var buf bytes.Buffer
	w := r.NewWriter(&buf)
	for i, n := 0, 0; i < len(s); n++ {
		size := min(sizes[n%len(sizes)], len(s)-i)
		if _, err := w.Write([]byte(s[i : i+size])); err != nil {
			panic(err)
		}
		i += size
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.String()
}

// writeParts writes each part as one Write and returns the output after
// Close.
func writeParts(t *testing.T, r *Replacer, parts ...string) string {
	t.Helper()
	var buf bytes.Buffer
	w := r.NewWriter(&buf)
	for _, p := range parts {
		n, err := w.Write([]byte(p))
		if err != nil || n != len(p) {
			t.Fatalf("Write(%q) = %d, %v", p, n, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestWriterEverySplitPoint cuts texts holding keys at every point into two
// writes, and at every pair of points into three, and requires the stream to
// come out exactly as String has it — in particular, with no key in it.
func TestWriterEverySplitPoint(t *testing.T) {
	r := New(keyA, keyB)
	texts := []string{
		keyA,
		"export KEY=" + keyA + "\n",
		"a" + keyA + keyB + "z",
		keyB + keyA,
		"head " + keyA[:5] + " " + keyA, // a false start, then the key
		"tail " + keyA,
		"tail " + keyA[:len(keyA)-1], // a prefix at EOF, emitted as it is
	}
	for _, text := range texts {
		want := r.String(text)
		for i := 0; i <= len(text); i++ {
			if got := writeParts(t, r, text[:i], text[i:]); got != want {
				t.Fatalf("%q split at %d: got %q, want %q", text, i, got, want)
			}
			for j := i; j <= len(text); j++ {
				if got := writeParts(t, r, text[:i], text[i:j], text[j:]); got != want {
					t.Fatalf("%q split at %d and %d: got %q, want %q", text, i, j, got, want)
				}
			}
		}
		if strings.Contains(want, keyA) || strings.Contains(want, keyB) {
			t.Fatalf("String(%q) = %q still holds a key", text, want)
		}
	}

	// The negative control: redacting each write on its own, which is what
	// a stream without a hold-back amounts to, leaks a key split in two.
	// Without this, the loop above could pass against a Writer that
	// buffered everything until Close.
	half := len(keyA) / 2
	if naive := r.String("k="+keyA[:half]) + r.String(keyA[half:]); !strings.Contains(naive, keyA) {
		t.Fatalf("the control did not leak (%q): the split test proves nothing", naive)
	}
}

// TestWriterHoldsBackOnlyWhatItMust: the stream reaches the underlying writer
// as it is written, less only a tail that could still begin a key (held): so
// a bash tool's live output — a background job's reads included (plan 033
// §3.7) — shows a command's last line at once unless that line ends in the
// first bytes of a key. A tail that is a key's prefix is held exactly, and a
// longer prefix of a longer key wins. Close writes the rest.
func TestWriterHoldsBackOnlyWhatItMust(t *testing.T) {
	r := New("short-key", keyA) // keyA is the longest: 20 bytes
	cases := []struct {
		text string
		held int
	}{
		{strings.Repeat("plain output line\n", 10), 0},
		{"listening on :3000", 0},
		{"no key here, but s", 1},                    // "s" could begin either key
		{"maybe sk-can", len("sk-can")},              // keyA's start
		{"maybe short-", len("short-")},              // short-key's start
		{"maybe sk-canary-alpha-000", len(keyA) - 1}, // all but keyA's last byte
		{"whole " + keyA + " then sh", len("sh")},    // a key whole, then another's start
		{"x" + keyA[:len(keyA)-1] + "x", 0},          // a prefix broken off: nothing to hold
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		w := r.NewWriter(&buf)
		if _, err := w.Write([]byte(tc.text)); err != nil {
			t.Fatal(err)
		}
		if want := r.String(tc.text[:len(tc.text)-tc.held]); buf.String() != want {
			t.Fatalf("%q: after one write %q reached the writer, want %q (%d bytes held)", tc.text, buf.String(), want, tc.held)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if buf.String() != r.String(tc.text) {
			t.Fatalf("%q: after Close = %q, want the whole text, redacted", tc.text, buf.String())
		}
	}
	// Close is the end of the stream: a second one changes nothing and a
	// Write after it fails.
	var buf bytes.Buffer
	w := r.NewWriter(&buf)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close = %v, want ErrClosed", err)
	}
}

// TestHead (plan 033 C14r2): Head is what a Writer given the text would have
// written before its Close — the start of a line whose rest a reader never
// held, redacted as far as it can be decided — so a key the text's end halves
// leaves none of itself, a key whole in it is replaced, and text that begins
// no key is kept as it is. Every prefix of a line holding a key is cut, as a
// reader's window could cut it: none shows a byte of the key. The negative
// control is String, which shows the halved key's first bytes; and a nil
// Replacer's Head is the text.
func TestHead(t *testing.T) {
	r := New("short-key", keyA)
	for _, tc := range []struct{ text, want string }{
		{"", ""},
		{"plain output line", "plain output line"},
		{"export KEY=" + keyA, "export KEY=" + Marker},
		{"cut at sk-canary-al", "cut at "},
		{"cut at " + keyA[:len(keyA)-1], "cut at "},
		{"whole " + keyA + " then sh", "whole " + Marker + " then "},
		{"x" + keyA[:len(keyA)-1] + "x", "x" + keyA[:len(keyA)-1] + "x"}, // a prefix broken off: no key
	} {
		if got := r.Head(tc.text); got != tc.want {
			t.Fatalf("Head(%q) = %q, want %q", tc.text, got, tc.want)
		}
		var buf bytes.Buffer
		if _, err := r.NewWriter(&buf).Write([]byte(tc.text)); err != nil {
			t.Fatal(err)
		}
		if buf.String() != tc.want {
			t.Fatalf("%q: a Writer wrote %q before its Close, Head %q", tc.text, buf.String(), tc.want)
		}
	}

	head := "a line of output, then "
	line := head + keyA + " and more"
	for i := 0; i <= len(line); i++ {
		got := r.Head(line[:i])
		switch {
		case i < len(head):
			if got != line[:i] {
				t.Fatalf("Head of %d bytes = %q, want them as they are", i, got)
			}
		case i < len(head)+len(keyA):
			if got != head {
				t.Fatalf("Head of %d bytes, %d of the key = %q, want none of it", i, i-len(head), got)
			}
		case !strings.HasPrefix(got, head+Marker) || strings.Contains(got, keyA[:3]):
			t.Fatalf("Head of %d bytes, the key whole = %q", i, got)
		}
	}
	if cut := line[:len(head)+10]; !strings.Contains(r.String(cut), keyA[:10]) {
		t.Fatalf("control: String(%q) = %q shows no half of the key", cut, r.String(cut))
	}
	var none *Replacer
	if got := none.Head(line[:len(head)+10]); got != line[:len(head)+10] {
		t.Fatalf("a nil Replacer's Head = %q", got)
	}
}

func TestWriterManySmallWrites(t *testing.T) {
	r := New(keyA, keyB)
	text := strings.Repeat("line with "+keyA+" and "+keyB+"\n", 50)
	for _, sizes := range [][]int{{1}, {2}, {3, 1, 4, 1, 5}, {7}, {19}, {20}, {21}} {
		if got, want := streamed(r, text, sizes), r.String(text); got != want {
			t.Fatalf("chunk sizes %v: got %q, want %q", sizes, got, want)
		}
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	f.n++
	return 0, errors.New("disk full")
}

func TestWriterErrorIsSticky(t *testing.T) {
	fw := &failWriter{}
	w := New(keyA).NewWriter(fw)
	big := strings.Repeat("x", 100)
	if _, err := w.Write([]byte(big)); err == nil {
		t.Fatal("Write did not report the underlying error")
	}
	if _, err := w.Write([]byte(big)); err == nil || fw.n != 1 {
		t.Fatalf("second Write = %v after %d underlying writes, want the same error and no retry", err, fw.n)
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close lost the error")
	}
}

// TestRandomSplitsMatchStringAndReference drives small alphabets, so keys
// overlap, repeat and touch often, through String, the reference, and a
// Writer with random chunk sizes. The seeds are fixed: a failure reproduces.
func TestRandomSplitsMatchStringAndReference(t *testing.T) {
	for seed := range uint64(2000) {
		rng := rand.New(rand.NewPCG(seed, 19))
		alphabet := "abc"[:1+rng.IntN(3)]
		randString := func(n int) string {
			b := make([]byte, n)
			for i := range b {
				b[i] = alphabet[rng.IntN(len(alphabet))]
			}
			return string(b)
		}
		keys := make([]string, 1+rng.IntN(3))
		for i := range keys {
			keys[i] = randString(1 + rng.IntN(6))
		}
		text := randString(rng.IntN(60))
		r := New(keys...)
		want := reference(keys, text)
		if got := r.String(text); got != want {
			t.Fatalf("seed %d keys %q text %q: String = %q, reference = %q", seed, keys, text, got, want)
		}
		sizes := make([]int, 1+rng.IntN(4))
		for i := range sizes {
			sizes[i] = 1 + rng.IntN(8)
		}
		if got := streamed(r, text, sizes); got != want {
			t.Fatalf("seed %d keys %q text %q sizes %v: Writer = %q, reference = %q", seed, keys, text, sizes, got, want)
		}
	}
}

// FuzzWriterMatchesString: for any text and any way of cutting it, the
// stream equals the whole-value pass. Run with -fuzz to explore; the seeds
// run with every go test.
func FuzzWriterMatchesString(f *testing.F) {
	f.Add("export KEY="+keyA+"\n", uint8(3), uint8(7))
	f.Add(keyA+keyB+keyA[:4], uint8(1), uint8(1))
	f.Add("aaaaaaaaaaaaaaaaaaaaaaaa", uint8(2), uint8(5))
	f.Add("", uint8(1), uint8(1))
	r := New(keyA, keyB, "aaaaaaaa", "canary-alpha")
	f.Fuzz(func(t *testing.T, text string, a, b uint8) {
		sizes := []int{1 + int(a%32), 1 + int(b%32)}
		want := r.String(text)
		if got := streamed(r, text, sizes); got != want {
			t.Fatalf("sizes %v: Writer = %q, String = %q", sizes, got, want)
		}
		if ref := reference(r.keys, text); ref != want {
			t.Fatalf("String = %q, reference = %q", want, ref)
		}
	})
}

// TestConcurrentWritersShareAReplacer: a Replacer is shared by every call's
// Writer; run under -race, this is the proof that it holds no mutable state.
func TestConcurrentWritersShareAReplacer(t *testing.T) {
	r := New(keyA, keyB)
	text := strings.Repeat("out "+keyA+" err "+keyB+"\n", 200)
	want := r.String(text)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			if got := streamed(r, text, []int{1 + i, 3}); got != want {
				errs <- fmt.Errorf("writer %d diverged", i)
			}
			if got := r.String(text); got != want {
				errs <- fmt.Errorf("String %d diverged", i)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestMarkerOverlaps(t *testing.T) {
	cases := map[string]bool{
		"redacted":                 true, // inside the marker
		"credential":               true,
		"craze:redacted":           true,
		Marker:                     true,
		"x" + Marker + "y":         true, // holds it
		"ential]-live-key":         true, // starts with its end
		"sk-live-[craze":           true, // ends with its start
		"]abcdefg":                 true,
		"abcdefg[":                 true,
		keyA:                       false,
		keyB:                       false,
		"credentialx":              false, // holds a piece of it, overlaps neither end
		"sk-[craze-live":           false, // "[craze" not at the end
		"":                         false,
		"redacted-credential-0001": false,
	}
	for key, want := range cases {
		if got := MarkerOverlaps(key); got != want {
			t.Errorf("MarkerOverlaps(%q) = %v, want %v", key, got, want)
		}
	}
	// The negative control: an overlapping key does come back out.
	if out := New("credential").String("pw=credential"); !strings.Contains(out, "credential") {
		t.Fatalf("redacting gave %q: the marker no longer prints the key back", out)
	}
}

// TestNoKeySurvivesUnlessItOverlapsTheMarker is the claim MarkerOverlaps
// documents: with keys that do not overlap the marker, redacted text holds
// none of them, and redacting it again changes nothing. Texts and keys are
// drawn from the marker's own bytes, so near-collisions are common; keys
// that do overlap are counted to show the property is not vacuous.
func TestNoKeySurvivesUnlessItOverlapsTheMarker(t *testing.T) {
	alphabet := "[]cr:ed-ntial"
	reappeared := 0
	for seed := range uint64(3000) {
		rng := rand.New(rand.NewPCG(seed, 27))
		pick := func(n int) string {
			b := make([]byte, n)
			for i := range b {
				b[i] = alphabet[rng.IntN(len(alphabet))]
			}
			return string(b)
		}
		var safe, overlapping []string
		for range 1 + rng.IntN(3) {
			k := pick(1 + rng.IntN(6))
			if rng.IntN(3) == 0 { // a slice of the marker, which may reach past it
				s := pick(2) + Marker + pick(2)
				i := rng.IntN(len(s) - 1)
				k = s[i:min(len(s), i+2+rng.IntN(8))]
			}
			if MarkerOverlaps(k) {
				overlapping = append(overlapping, k)
			} else {
				safe = append(safe, k)
			}
		}
		text := pick(rng.IntN(40))
		for _, k := range append(safe, overlapping...) {
			if rng.IntN(2) == 0 {
				at := rng.IntN(len(text) + 1)
				text = text[:at] + k + text[at:]
			}
		}
		if len(safe) > 0 {
			r := New(safe...)
			out := r.String(text)
			for _, k := range safe {
				if strings.Contains(out, k) {
					t.Fatalf("seed %d: key %q survives in %q (from %q)", seed, k, out, text)
				}
			}
			if again := r.String(out); again != out {
				t.Fatalf("seed %d: a second pass changed %q to %q", seed, out, again)
			}
		}
		if len(overlapping) > 0 {
			out := New(overlapping...).String(text)
			for _, k := range overlapping {
				if strings.Contains(text, k) && strings.Contains(out, k) {
					reappeared++
				}
			}
		}
	}
	if reappeared == 0 {
		t.Fatal("no overlapping key ever came back out: the draw does not exercise the rule")
	}
}

// TestUnion: a Union redacts every key of either side; it builds nothing when
// one side holds the other's keys — it hands back the side that does — and
// its order does not matter, so two widenings that cross on their way to one
// stream end on the same set (plan 033 C10r).
func TestUnion(t *testing.T) {
	a, b, ab := New(keyA), New(keyB), New(keyB, keyA)
	text := "a=" + keyA + " b=" + keyB
	for _, tc := range []struct {
		name string
		got  *Replacer
		same *Replacer // the Replacer it must be, nil for a new one
	}{
		{"nil and nil", (*Replacer)(nil).Union(nil), nil},
		{"r with nil", a.Union(nil), a},
		{"nil with o", (*Replacer)(nil).Union(a), a},
		{"empty with o", New().Union(a), a},
		{"r with a subset", ab.Union(a), ab},
		{"r with a superset", a.Union(ab), ab},
		{"the same keys", a.Union(New(keyA)), a},
		{"disjoint", a.Union(b), nil},
	} {
		if tc.same != nil && tc.got != tc.same {
			t.Errorf("%s: built a new Replacer; want the side that holds every key", tc.name)
		}
	}
	for _, u := range []*Replacer{a.Union(b), b.Union(a), a.Union(b).Union(a), b.Union(ab)} {
		if got, want := u.String(text), ab.String(text); got != want {
			t.Fatalf("a union redacts %q; want %q", got, want)
		}
	}
	if got := (*Replacer)(nil).Union(nil).String(text); got != text {
		t.Fatalf("the union of none redacts %q", got)
	}
}

// widenAt writes parts to a Writer over r, widens it to o after the first
// widenAfter parts, writes the rest, and returns what reached the underlying
// writer after Close, and what had before the Widen.
func widenAt(t *testing.T, r, o *Replacer, widenAfter int, parts ...string) (out, before string) {
	t.Helper()
	var buf bytes.Buffer
	w := r.NewWriter(&buf)
	for i, p := range parts {
		if i == widenAfter {
			before = buf.String()
			w.Widen(o)
		}
		if _, err := w.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if widenAfter >= len(parts) {
		before = buf.String()
		w.Widen(o)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), before
}

// TestWriterWidenKeepsWhatItHolds (plan 033 C10r, review r7 finding 9): a
// Writer widened while it runs redacts the new key in everything it had not
// yet decided — a key printed whole after the Widen, one split across two
// writes after it, and one whose first bytes it was holding back as the start
// of a key it already knew when the Widen came — and drops, repeats or
// reorders nothing. What it had passed on before the Widen stays passed on.
//
// The negative controls: a Writer never widened leaks each of them, and one
// "widened" by writing out what it holds and starting over — the obvious way
// to swap a Replacer — leaks the held prefix of the last one.
func TestWriterWidenKeepsWhatItHolds(t *testing.T) {
	const learned = "sk-canary-learned-0042" // shares "sk-canary-" with keyA, a key the Writer knew
	old, wide := New(keyA), New(keyA, learned)
	half := len(learned) / 2
	cases := []struct {
		name   string
		before []string // written before the Widen
		after  []string // written after it
	}{
		{"whole, after", []string{"start\n"}, []string{"k=" + learned + "\n"}},
		{"split across two writes, after", []string{"start\n"}, []string{"k=" + learned[:half], learned[half:] + "\n"}},
		{"its start held as keyA's when the Widen came", []string{"k=sk-canary-"}, []string{"learned-0042\n"}},
		{"the Widen before any write", nil, []string{learned[:3], learned[3:]}},
		{"a key it knew, split around the Widen", []string{"k=" + keyA[:7]}, []string{keyA[7:] + "\n"}},
	}
	for _, tc := range cases {
		parts := append(slices.Clone(tc.before), tc.after...)
		text := strings.Join(parts, "")
		out, before := widenAt(t, old, wide, len(tc.before), parts...)
		if strings.Contains(out, learned) || strings.Contains(out, keyA) {
			t.Fatalf("%s: %q holds a key", tc.name, out)
		}
		if want := wide.String(text); out != want {
			t.Fatalf("%s: the stream came out %q; want %q, the whole text under the wider set", tc.name, out, want)
		}
		if !strings.HasPrefix(out, before) {
			t.Fatalf("%s: %q does not begin with what was written before the Widen, %q", tc.name, out, before)
		}
		// The control: never widened, the new key leaks.
		if never, _ := widenAt(t, old, nil, len(tc.before), parts...); strings.Contains(old.String(text), learned) && !strings.Contains(never, learned) {
			t.Fatalf("%s: control: a Writer never widened did not leak (%q), so the Widen proves nothing", tc.name, never)
		}
	}

	// The control for keeping what it holds: write it out, then widen — the
	// held "sk-canary-" goes out on its own, and the key's rest after it.
	var buf bytes.Buffer
	w := old.NewWriter(&buf)
	_, _ = w.Write([]byte("k=sk-canary-"))
	w.emit(len(w.buf)) // what a swap that starts the stream over would do
	w.Widen(wide)
	_, _ = w.Write([]byte("learned-0042\n"))
	_ = w.Close()
	if !strings.Contains(buf.String(), learned) {
		t.Fatalf("control: writing out the held bytes before widening did not leak (%q): the held case proves nothing", buf.String())
	}

	// A Widen after Close changes nothing and breaks nothing.
	var closed bytes.Buffer
	cw := old.NewWriter(&closed)
	_ = cw.Close()
	cw.Widen(wide)
	if _, err := cw.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close and Widen = %v, want ErrClosed", err)
	}
}

// TestWriterWidenAtEverySplitPoint: for texts holding the old key and a new
// one, cut into three writes at every pair of points and widened between any
// two of them, the stream comes out as String under the wider set whenever
// every byte of the new key's occurrences came after the Widen — the case a
// key learned before the command printed it is — and holds no key then.
// Small alphabets would make overlaps common; these texts put the keys
// against, inside and across each other on purpose.
func TestWriterWidenAtEverySplitPoint(t *testing.T) {
	const learned = "sk-canary-learned-0042"
	old, wide := New(keyA), New(keyA, learned)
	texts := []string{
		"x" + learned + "y",
		keyA + learned,
		learned + keyA,
		"sk-canary-" + learned, // a false start of both, then the new key
		"sk-canary-alpha-00" + learned,
		learned[:10] + keyA + learned,
	}
	checked := 0
	for _, text := range texts {
		first := strings.Index(text, learned) // the earliest occurrence of the new key
		want := wide.String(text)
		for i := 0; i <= len(text); i++ {
			for j := i; j <= len(text); j++ {
				parts := []string{text[:i], text[i:j], text[j:]}
				for at, cut := range []int{0, i, j, len(text)} {
					if cut > first {
						continue // a byte of the new key came before the Widen: out of the claim
					}
					out, _ := widenAt(t, old, wide, at, parts...)
					if out != want {
						t.Fatalf("%q cut at %d,%d, widened after %d bytes: %q, want %q", text, i, j, cut, out, want)
					}
					checked++
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no case was checked")
	}
}

// TestWidenRacesWithWriteUnderALock is the Writer's half of how a bash job's
// stream is widened from the session's goroutine (opencode's modelStream): a
// caller that serializes Widen with Write under its own lock may widen while
// another goroutine writes. Run under -race.
func TestWidenRacesWithWriteUnderALock(t *testing.T) {
	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)
	w := New(keyA).NewWriter(&buf)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			mu.Lock()
			_, _ = w.Write([]byte("out " + keyA + "\n"))
			mu.Unlock()
		}
	})
	wg.Go(func() {
		for i := range 50 {
			mu.Lock()
			w.Widen(New(fmt.Sprintf("zq-learned-key-%04d", i)))
			mu.Unlock()
		}
	})
	wg.Wait()
	_ = w.Close()
	if strings.Contains(buf.String(), keyA) {
		t.Fatal("a key survived")
	}
}
