package tui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

// TestTheAsidingStubAnswersItsScript (plan 040 §3.8): AsidingStub streams
// StubAsidePieces of the request, one onDelta each, and answers their
// concatenation with the current model's display name and usage that grows
// with the side conversation; every request is recorded (Asked), its prior
// included. RefreshingAsidingStub is both of the native verbs, and the plain
// Stub neither. Negative control: an answer whose text is not the pieces'
// concatenation fails the text's comparison.
func TestTheAsidingStubAnswersItsScript(t *testing.T) {
	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })
	a := AsidingStub{Stub: stub}
	req := agent.AsideRequest{Question: "why?", Prior: []agent.AsideTurn{{Question: "what?", Answer: "that"}}}
	var got []string
	r, err := a.Aside(context.Background(), req, func(s string) { got = append(got, s) })
	if err != nil {
		t.Fatal(err)
	}
	pieces := StubAsidePieces(req)
	if !reflect.DeepEqual(got, pieces) || !strings.Contains(pieces[2], "side question 2") {
		t.Fatalf("the pieces: %q, want %q", got, pieces)
	}
	if r.Text != strings.Join(pieces, "") || r.Model != "Grok" || r.Usage == nil || *r.Usage != (agent.AsideUsage{Input: 2000, Output: 4, CacheRead: 1600}) {
		t.Fatalf("the answer: %+v", r)
	}
	if asked := stub.Asked(); len(asked) != 1 || !reflect.DeepEqual(asked[0], req) {
		t.Fatalf("Asked: %+v", asked)
	}
	var s agent.Session = RefreshingAsidingStub{Stub: stub}
	if _, ok := s.(agent.ModelsRefresher); !ok {
		t.Fatal("RefreshingAsidingStub does not refresh models")
	}
	if as, ok := s.(agent.Asider); !ok || !as.Asides() {
		t.Fatal("RefreshingAsidingStub answers no side questions")
	}
	if _, ok := agent.Session(stub).(agent.Asider); ok {
		t.Fatal("the plain Stub answers side questions: every fixture and golden would change")
	}
}

// TestAHeldAsideWaitsForReleaseOrItsEnd (plan 040 §3.8): HoldNextAside parks
// the next answer once its first piece is out — held closes then — until
// release, which finishes the answer; the ctx's end, which answers its error
// with nothing more streamed; or the Stub's close, which answers
// context.Canceled, as a session's end cancels its side questions. A closed
// Stub answers agent.ErrAsideNotReady. Each hold takes up one aside only.
// Negative control: a hold that parks before the first piece leaves the
// pieces empty when held closes.
func TestAHeldAsideWaitsForReleaseOrItsEnd(t *testing.T) {
	const wait = 10 * time.Second
	type answer struct {
		r   agent.AsideResult
		err error
	}
	run := func(ctx context.Context, a AsidingStub, pieces chan<- string) <-chan answer {
		ch := make(chan answer, 1)
		go func() {
			r, err := a.Aside(ctx, agent.AsideRequest{Question: "q"}, func(s string) { pieces <- s })
			ch <- answer{r, err}
		}()
		return ch
	}
	awaitHeld := func(held <-chan struct{}, pieces <-chan string) {
		t.Helper()
		select {
		case <-held:
		case <-time.After(wait):
			t.Fatal("the aside was never held")
		}
		if len(pieces) != 1 {
			t.Fatalf("held with %d pieces out, want the first one", len(pieces))
		}
	}
	recvAnswer := func(ch <-chan answer) answer {
		t.Helper()
		select {
		case a := <-ch:
			return a
		case <-time.After(wait):
			t.Fatal("the aside never answered")
			return answer{}
		}
	}

	stub := NewStub()
	a := AsidingStub{Stub: stub}
	held, release := stub.HoldNextAside()
	pieces := make(chan string, 8)
	done := run(context.Background(), a, pieces)
	awaitHeld(held, pieces)
	release()
	if got := recvAnswer(done); got.err != nil || len(pieces) != 4 {
		t.Fatalf("a released aside: %+v, %v, %d pieces", got.r, got.err, len(pieces))
	}

	held, _ = stub.HoldNextAside()
	ctx, cancel := context.WithCancel(context.Background())
	pieces = make(chan string, 8)
	done = run(ctx, a, pieces)
	awaitHeld(held, pieces)
	cancel()
	if got := recvAnswer(done); !errors.Is(got.err, context.Canceled) || len(pieces) != 1 {
		t.Fatalf("a cancelled aside: %v, %d pieces", got.err, len(pieces))
	}

	// The hold is spent: the next one answers at once.
	pieces = make(chan string, 8)
	if got := recvAnswer(run(context.Background(), a, pieces)); got.err != nil || len(pieces) != 4 {
		t.Fatalf("an aside after the hold was spent: %v, %d pieces", got.err, len(pieces))
	}

	held, _ = stub.HoldNextAside()
	pieces = make(chan string, 8)
	done = run(context.Background(), a, pieces)
	awaitHeld(held, pieces)
	_ = stub.Close()
	if got := recvAnswer(done); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("an aside its session's close cut off: %v, want context.Canceled", got.err)
	}
	if got := recvAnswer(run(context.Background(), a, make(chan string, 8))); !errors.Is(got.err, agent.ErrAsideNotReady) {
		t.Fatalf("an aside to a closed Stub: %v, want agent.ErrAsideNotReady", got.err)
	}
}

// TestTheEngineBackendAsksTheSession (plan 040 §3.7): in process the
// backend's Info says aside exactly where the engine does — an AsidingStub's
// engine, not a plain Stub's — and Aside is the engine's, its pieces handed
// to onDelta and its answer returned; a plain Stub's answers
// agent.ErrAsideUnsupported. Negative control: an Info that leaves Aside
// unset fails the first check.
func TestTheEngineBackendAsksTheSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		sess func(*Stub) agent.Session
		want bool
	}{
		{"a plain Stub", func(s *Stub) agent.Session { return s }, false},
		{"an AsidingStub", func(s *Stub) agent.Session { return AsidingStub{Stub: s} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := NewStub()
			eng, err := engine.New(tc.sess(stub), engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = eng.Close() })
			if err := eng.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			b := newEngineBackend(eng, "")
			if b.Info().Aside != tc.want {
				t.Fatalf("Info().Aside is %v, want %v", b.Info().Aside, tc.want)
			}
			req := agent.AsideRequest{Question: "q"}
			var got []string
			r, err := b.Aside(dispatchCtx(b), req, func(s string) { got = append(got, s) })
			if !tc.want {
				if !errors.Is(err, agent.ErrAsideUnsupported) {
					t.Fatalf("a plain Stub's aside: %v, want agent.ErrAsideUnsupported", err)
				}
				return
			}
			if err != nil || r.Text != strings.Join(StubAsidePieces(req), "") || !reflect.DeepEqual(got, StubAsidePieces(req)) {
				t.Fatalf("the aside: %+v, %v, pieces %q", r, err, got)
			}
		})
	}
}
