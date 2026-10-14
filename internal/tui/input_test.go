package tui

import (
	"io"
	"os"
	"slices"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestRewriteShiftEnter: each Shift+Enter sequence becomes one \n wherever it
// sits among other input, and nothing else changes — not Ctrl+Enter's
// sequences, not half a sequence, not Enter.
func TestRewriteShiftEnter(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"\x1b[27;2;13~", "\n"},
		{"\x1b[13;2u", "\n"},
		{"ab\x1b[27;2;13~cd", "ab\ncd"},
		{"\x1b[13;2u\x1b[27;2;13~", "\n\n"},
		{"\x1b\x1b[13;2u", "\x1b\n"},
		{"\x1b[27;5;13~", "\x1b[27;5;13~"},
		{"\x1b[13;5u", "\x1b[13;5u"},
		{"\x1b[27;2;13", "\x1b[27;2;13"},
		{"\x1b[13;2", "\x1b[13;2"},
		{"\x1b", "\x1b"},
		{"\r", "\r"},
		{"plain text", "plain text"},
		{"", ""},
	} {
		in := &shiftEnterInput{}
		b := []byte(tc.in)
		if got := string(b[:in.rewrite(b, true)]); got != tc.want {
			t.Errorf("rewrite(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRewriteShiftEnterLeavesAPasteAlone: inside a bracketed paste the
// sequences are pasted text and pass as they came; after the paste ends they
// are Shift+Enter again. The markers count when a read cuts them in two, and
// when they arrive in a read passed on whole.
func TestRewriteShiftEnterLeavesAPasteAlone(t *testing.T) {
	type read struct {
		in   string
		may  bool
		want string
	}
	for _, tc := range []struct {
		name  string
		reads []read
	}{
		{"one read", []read{
			{"\x1b[200~a\x1b[13;2ub\x1b[201~\x1b[13;2u", true, "\x1b[200~a\x1b[13;2ub\x1b[201~\n"},
		}},
		{"markers cut in two", []read{
			{"\x1b[20", true, "\x1b[20"},
			{"0~x\x1b[27;2;13~", true, "0~x\x1b[27;2;13~"},
			{"y\x1b[2", true, "y\x1b[2"},
			{"01~\x1b[27;2;13~", true, "01~\n"},
		}},
		{"a Shift+Enter between a marker's halves makes no marker", []read{
			{"\x1b[20\x1b[13;2u0~", true, "\x1b[20\n0~"},
			{"\x1b[13;2u", true, "\n"},
		}},
		{"markers in reads passed on whole", []read{
			{"\x1b[200~ab", false, "\x1b[200~ab"},
			{"\x1b[13;2u", true, "\x1b[13;2u"},
			{"c\x1b[201~", false, "c\x1b[201~"},
			{"\x1b[13;2u", true, "\n"},
		}},
	} {
		in := &shiftEnterInput{}
		for i, r := range tc.reads {
			b := []byte(r.in)
			if got := string(b[:in.rewrite(b, r.may)]); got != r.want {
				t.Errorf("%s, read %d: rewrite(%q) = %q, want %q", tc.name, i, r.in, got, r.want)
			}
		}
	}
}

// TestShiftEnterInputLeavesAFullReadAlone: a read that fills the buffer is
// passed on as it came, since shortening it would tell bubbletea it ended on
// a key's boundary; one with room to spare is rewritten.
func TestShiftEnterInputLeavesAFullReadAlone(t *testing.T) {
	const seq = "\x1b[27;2;13~"
	r, w := pipe(t)
	in := &shiftEnterInput{f: r}
	for _, tc := range []struct {
		size int
		want string
	}{
		{len(seq), seq},
		{len(seq) + 1, "\n"},
	} {
		if _, err := w.WriteString(seq); err != nil {
			t.Fatal(err)
		}
		p := make([]byte, tc.size)
		n, err := in.Read(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(p[:n]); got != tc.want {
			t.Errorf("read into %d bytes = %q, want %q", tc.size, got, tc.want)
		}
	}
}

// TestShiftEnterReachesTheComposerAsANewline: Shift+Enter, in either form,
// read by bubbletea through shiftEnterInput as a terminal sends it — with the
// keys typed around it in the same read — is ctrl+j, and ctrl+j in the
// composer is a newline that sends nothing.
func TestShiftEnterReachesTheComposerAsANewline(t *testing.T) {
	for _, seq := range []string{"\x1b[27;2;13~", "\x1b[13;2u"} {
		keys := shiftEnterKeys(t, "hello"+seq+"world", 3)
		var got []string
		for _, k := range keys {
			got = append(got, k.String())
		}
		if want := []string{"hello", "ctrl+j", "world"}; !slices.Equal(got, want) {
			t.Fatalf("%q read as %q, want %q", seq, got, want)
		}
		m := sized(t)
		for _, k := range keys {
			tm, _ := m.Update(k)
			m = tm.(Model)
		}
		if m.status != statusIdle {
			t.Fatalf("%q sent a prompt, status %s", seq, m.status)
		}
		if n := turnsStarted(stubOf(t, m)); n != 0 {
			t.Fatalf("%q started %d turns", seq, n)
		}
		if got := m.input.Value(); got != "hello\nworld" {
			t.Fatalf("%q: composer %q, want %q", seq, got, "hello\nworld")
		}
	}
}

// shiftEnterKeys is the first want keys bubbletea reads from in, written in
// one piece to a pipe it reads through shiftEnterInput, as Run has it read
// the terminal.
func shiftEnterKeys(t *testing.T, in string, want int) []tea.KeyMsg {
	t.Helper()
	r, w := pipe(t)
	if _, err := w.WriteString(in); err != nil {
		t.Fatal(err)
	}
	rec := &keysRecorder{want: want}
	p := tea.NewProgram(rec, tea.WithInput(&shiftEnterInput{f: r}), tea.WithOutput(io.Discard),
		tea.WithoutRenderer(), tea.WithoutSignalHandler())
	done := make(chan struct{})
	go func() {
		_, _ = p.Run()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(keyBound):
		p.Kill()
		<-done
	}
	if len(rec.keys) != want {
		t.Fatalf("%q was read as %v, want %d keys", in, rec.keys, want)
	}
	return rec.keys
}

// pipe is an os.Pipe closed when the test ends.
func pipe(t *testing.T) (r, w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return r, w
}

// keysRecorder is a program that keeps the keys it is sent and quits at the
// want'th.
type keysRecorder struct {
	want int
	keys []tea.KeyMsg
}

func (r *keysRecorder) Init() tea.Cmd { return nil }

func (r *keysRecorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		r.keys = append(r.keys, k)
		if len(r.keys) == r.want {
			return r, tea.Quit
		}
	}
	return r, nil
}

func (r *keysRecorder) View() string { return "" }
