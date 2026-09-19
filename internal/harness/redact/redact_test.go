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
// as it is written, less the longest key's length minus one, so a bash
// tool's live output lags by a few dozen bytes at most.
func TestWriterHoldsBackOnlyWhatItMust(t *testing.T) {
	r := New("short-key", keyA) // keyA is the longest: 20 bytes
	var buf bytes.Buffer
	w := r.NewWriter(&buf)
	text := strings.Repeat("plain output line\n", 10)
	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if want := len(text) - (len(keyA) - 1); buf.Len() != want {
		t.Fatalf("after one write, %d bytes reached the writer, want %d", buf.Len(), want)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.String() != text {
		t.Fatalf("after Close = %q, want the whole text", buf.String())
	}
	// Close is the end of the stream: a second one changes nothing and a
	// Write after it fails.
	if err := w.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close = %v, want ErrClosed", err)
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
