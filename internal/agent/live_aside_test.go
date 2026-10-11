package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/acp"
)

// dumpedBtws is every x.ai/btw question the fake read, in arrival order.
func dumpedBtws(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var line struct {
			Btw *string `json:"btw"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line.Btw != nil {
			out = append(out, *line.Btw)
		}
	}
	return out
}

func startProviderScript(t *testing.T, p Provider, script string) *session {
	t.Helper()
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	s := newTestSession(t, Options{
		Binary:    fakeAgentPath(t),
		ExtraArgs: []string{"-script=" + script},
		Workspace: t.TempDir(),
		Force:     true,
		Provider:  &p,
		Stderr:    io.Discard,
	})
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAsideProviderBits(t *testing.T) {
	if !GrokProvider().Aside() || !GxProvider().Aside() || !NativeProvider().Aside() {
		t.Fatal("grok, gx and native should serve side questions")
	}
	if CursorProvider().Aside() {
		t.Fatal("cursor should not")
	}
}

func TestLiveAsideAnswersThroughTheWire(t *testing.T) {
	dump := promptDumpFile(t)
	s := startProviderScript(t, GrokProvider(), "grok-btw")
	if !s.Asides() {
		t.Fatal("a grok session reports no asides")
	}
	deltas := 0
	res, err := s.Aside(t.Context(), AsideRequest{Question: "q1"}, func(string) { deltas++ })
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "btw: q1" || res.Usage != nil || res.Model != "Grok 4.6" || deltas != 0 {
		t.Fatalf("result %+v, %d deltas", res, deltas)
	}
	res, err = s.Aside(t.Context(), AsideRequest{
		Question: "q2",
		Prior:    []AsideTurn{{Question: "q1", Answer: "btw: q1"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "Earlier in this side conversation:\n\nQ: q1\nA: btw: q1\n\nNow: q2"
	if got := dumpedBtws(t, dump); len(got) != 2 || got[0] != "q1" || got[1] != want {
		t.Fatalf("sent %q", got)
	}
	if res.Text != "btw: "+want {
		t.Fatalf("text %q", res.Text)
	}
}

func TestLiveAsideGxIsGrok(t *testing.T) {
	s := startProviderScript(t, GxProvider(), "grok-btw")
	if !s.Asides() {
		t.Fatal("a gx session reports no asides")
	}
	if res, err := s.Aside(t.Context(), AsideRequest{Question: "q"}, nil); err != nil || res.Text != "btw: q" {
		t.Fatalf("%+v, %v", res, err)
	}
}

func TestLiveAsideSanitizesTheAnswer(t *testing.T) {
	s := startProviderScript(t, GrokProvider(), "grok-btw-escape")
	// The agent's own answer carries the escape sequences, or the check below
	// proves nothing.
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if raw, err := client.Aside(t.Context(), "q"); err != nil || !strings.Contains(raw, "\x1b[31m") {
		t.Fatalf("raw answer %q, %v", raw, err)
	}
	res, err := s.Aside(t.Context(), AsideRequest{Question: "q"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "btw: q" {
		t.Fatalf("text %q", res.Text)
	}
}

func TestLiveAsideOlderGrokIsUnsupported(t *testing.T) {
	s := startProviderScript(t, GrokProvider(), "grok-btw-missing")
	_, err := s.Aside(t.Context(), AsideRequest{Question: "q"}, nil)
	if !errors.Is(err, ErrAsideUnsupported) || !strings.Contains(err.Error(), "update") {
		t.Fatalf("err %v", err)
	}
}

func TestLiveAsideCursorSendsNothing(t *testing.T) {
	dump := promptDumpFile(t)
	s := startProviderScript(t, CursorProvider(), "echo")
	if s.Asides() {
		t.Fatal("a cursor session reports asides")
	}
	if _, err := s.Aside(t.Context(), AsideRequest{Question: "q"}, nil); !errors.Is(err, ErrAsideUnsupported) {
		t.Fatalf("err %v", err)
	}
	if got := dumpedBtws(t, dump); len(got) != 0 {
		t.Fatalf("sent %q", got)
	}
}

func TestLiveAsideNotStarted(t *testing.T) {
	p := GrokProvider()
	s := newTestSession(t, Options{Binary: fakeAgentPath(t), Workspace: t.TempDir(), Provider: &p})
	if _, err := s.Aside(t.Context(), AsideRequest{Question: "q"}, nil); !errors.Is(err, ErrAsideNotReady) {
		t.Fatalf("err %v", err)
	}
}

func TestFoldAsideWithoutPriorIsTheQuestion(t *testing.T) {
	if got := foldAside(AsideRequest{Question: "just this"}); got != "just this" {
		t.Fatalf("%q", got)
	}
}

func TestFoldAsideFormat(t *testing.T) {
	got := foldAside(AsideRequest{Question: "q3", Prior: []AsideTurn{{"q1", "a1"}, {"q2", "a2"}}})
	want := "Earlier in this side conversation:\n\nQ: q1\nA: a1\n\nQ: q2\nA: a2\n\nNow: q3"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestFoldAsideBoundDropsTheOldestFirst(t *testing.T) {
	big := strings.Repeat("x", asideFoldAnswerMax+100)
	var prior []AsideTurn
	for i := range 20 {
		prior = append(prior, AsideTurn{Question: "q" + string(rune('a'+i)), Answer: big})
	}
	got := foldAside(AsideRequest{Question: "last", Prior: prior})
	if len(got) > AsideFoldMax {
		t.Fatalf("folded %d bytes, over %d", len(got), AsideFoldMax)
	}
	if !strings.HasSuffix(got, "Now: last") || !strings.Contains(got, "Q: qt\n") {
		t.Fatal("the newest pair or the question is missing")
	}
	if strings.Contains(got, "Q: qa\n") {
		t.Fatal("the oldest pair survived")
	}
	if !strings.Contains(got, asideTruncated) {
		t.Fatal("a long answer was not marked truncated")
	}
	// The kept pairs are a contiguous newest run, in order.
	if i, j := strings.Index(got, "Q: qs\n"), strings.Index(got, "Q: qt\n"); i < 0 || j < i {
		t.Fatal("pairs out of order or not contiguous")
	}
}

func TestFoldTruncateKeepsTheHeadOnARuneBoundary(t *testing.T) {
	s := "x" + strings.Repeat("é", asideFoldAnswerMax) // two bytes each, off by one
	got := foldTruncate(s)
	if !strings.HasSuffix(got, asideTruncated) {
		t.Fatal("no marker")
	}
	head := strings.TrimSuffix(got, asideTruncated)
	if len(head) > asideFoldAnswerMax || strings.ContainsRune(head, '�') || !strings.HasPrefix(s, head) {
		t.Fatalf("head of %d bytes", len(head))
	}
	if foldTruncate("short") != "short" {
		t.Fatal("a short answer changed")
	}
}

// TestLiveAsideCutShortByCloseIsInterrupted (plan 040 §3.3, branch review
// P2): a side question the session had taken, out on the wire when the
// session's own Close tears its client down, is ErrAsideInterrupted —
// context.Canceled, stopped and not refused — with the caller's context still
// live: not the refusal ErrAsideNotReady, and not the transport's
// acp.ErrClosed, which the engine would answer failed. One asked once the
// session has closed is still refused ErrAsideNotReady.
func TestLiveAsideCutShortByCloseIsInterrupted(t *testing.T) {
	dump := promptDumpFile(t)
	s := startProviderScript(t, GrokProvider(), "grok-btw")
	out := make(chan error, 1)
	go func() {
		_, err := s.Aside(context.Background(), AsideRequest{Question: "q HOLD-BTW"}, nil)
		out <- err
	}()
	waitUntil(t, "the agent holding the side question", func() bool { return len(dumpedBtws(t, dump)) == 1 })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	err := await(t, out, "the side question")
	if !errors.Is(err, ErrAsideInterrupted) || !errors.Is(err, context.Canceled) ||
		errors.Is(err, ErrAsideNotReady) || errors.Is(err, acp.ErrClosed) {
		t.Fatalf("a side question the session's Close cut short answered %v, want ErrAsideInterrupted alone", err)
	}
	if _, err := s.Aside(context.Background(), AsideRequest{Question: "q"}, nil); !errors.Is(err, ErrAsideNotReady) {
		t.Fatalf("after Close: %v, want ErrAsideNotReady", err)
	}
}

func TestLiveAsideClosedSessionIsNotReady(t *testing.T) {
	s := startProviderScript(t, GrokProvider(), "grok-btw")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Aside(t.Context(), AsideRequest{Question: "q"}, nil); !errors.Is(err, ErrAsideNotReady) {
		t.Fatalf("err %v", err)
	}
}
