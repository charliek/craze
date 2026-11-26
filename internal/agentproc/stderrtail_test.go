package agentproc

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

type failingWriter struct{ n int }

func (w failingWriter) Write(p []byte) (int, error) {
	return min(w.n, len(p)), errors.New("the sink is gone")
}

func TestTheStderrRingKeepsTheLastBytes(t *testing.T) {
	chunks := func(sizes ...int) [][]byte {
		var out [][]byte
		for i, n := range sizes {
			out = append(out, bytes.Repeat([]byte{'a' + byte(i%26)}, n))
		}
		return out
	}
	var many []int
	for range 100 {
		many = append(many, 37)
	}
	for _, tc := range []struct {
		name   string
		writes [][]byte
	}{
		{"nothing", nil},
		{"short writes", [][]byte{[]byte("one\n"), []byte("two\n")}},
		{"exactly full", chunks(StderrTailMax)},
		{"full, then one more byte", chunks(StderrTailMax, 1)},
		{"one write longer than the ring", chunks(3*StderrTailMax + 5)},
		{"many writes across its end", chunks(many...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sink bytes.Buffer
			r := newStderrTail(&sink)
			var all []byte
			for _, p := range tc.writes {
				if n, err := r.Write(p); n != len(p) || err != nil {
					t.Fatalf("Write(%d bytes) = %d, %v", len(p), n, err)
				}
				all = append(all, p...)
			}
			want := all[max(0, len(all)-StderrTailMax):]
			if got := r.tail(); !bytes.Equal(got, want) {
				t.Fatalf("the tail is %d bytes, want %d", len(got), len(want))
			}
			if !bytes.Equal(sink.Bytes(), all) {
				t.Fatalf("the sink got %d bytes, want %d", sink.Len(), len(all))
			}
		})
	}
	t.Run("the sink's answer is the write's", func(t *testing.T) {
		r := newStderrTail(failingWriter{n: 3})
		if n, err := r.Write([]byte("hello")); n != 3 || err == nil || err.Error() != "the sink is gone" {
			t.Fatalf("Write = %d, %v; want 3, the sink is gone", n, err)
		}
		if got := string(r.tail()); got != "hello" {
			t.Fatalf("the tail is %q, want %q", got, "hello")
		}
	})
	t.Run("a rune its end cut in two goes", func(t *testing.T) {
		r := newStderrTail(&bytes.Buffer{})
		_, _ = r.Write([]byte(strings.Repeat("日", 700)))
		if got, want := string(r.tail()), strings.Repeat("日", 682); got != want {
			t.Fatalf("the tail is %d bytes, want 682 whole runes", len(got))
		}
	})
	t.Run("an agent's own invalid bytes stay", func(t *testing.T) {
		r := newStderrTail(&bytes.Buffer{})
		_, _ = r.Write([]byte("\x80\x80oops"))
		if got := string(r.tail()); got != "\x80\x80oops" {
			t.Fatalf("the tail is %q, want invalid bytes unchanged", got)
		}
	})
}

func TestStderrWaitCanBeStretchedInATest(t *testing.T) {
	const bound = 500 * time.Millisecond
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"10s", 10 * time.Second},
		{"750ms", 750 * time.Millisecond},
		{"", bound},
		{"soon", bound},
		{"0s", bound},
		{"-1s", bound},
	} {
		t.Setenv(StderrWaitEnv, tc.env)
		if got := StderrWait(bound); got != tc.want {
			t.Fatalf("%s=%q: the wait is %v, want %v", StderrWaitEnv, tc.env, got, tc.want)
		}
	}
}
