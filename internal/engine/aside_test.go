package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/tui"
)

// asider is the Stub as a session that answers side questions (plan 040
// §3.3): each Aside call is handed to the test on calls, and does what the
// test sends back on its step.
type asider struct {
	*tui.Stub
	serves bool
	calls  chan *asideCall
}

// asideCall is one Aside the session is in.
type asideCall struct {
	ctx     context.Context
	req     agent.AsideRequest
	onDelta func(string)
	step    chan asideStep
}

// asideStep is how the session ends a call: a result or an error, a panic,
// or — waitCtx — its context's end, answered with an error of the session's
// own that does not wrap it (a closed pipe).
type asideStep struct {
	res     agent.AsideResult
	err     error
	panics  bool
	waitCtx bool
}

func newAsider(serves bool) *asider {
	return &asider{Stub: tui.NewStubNoPrimary(), serves: serves, calls: make(chan *asideCall, 4)}
}

func (a *asider) Asides() bool { return a.serves }

func (a *asider) Aside(ctx context.Context, req agent.AsideRequest, onDelta func(string)) (agent.AsideResult, error) {
	c := &asideCall{ctx: ctx, req: req, onDelta: onDelta, step: make(chan asideStep)}
	a.calls <- c
	s := <-c.step
	switch {
	case s.panics:
		panic("the session panicked")
	case s.waitCtx:
		<-ctx.Done()
		return agent.AsideResult{}, errors.New("pipe closed")
	}
	return s.res, s.err
}

// call is the next Aside the session is in.
func (a *asider) call(t *testing.T) *asideCall {
	t.Helper()
	select {
	case c := <-a.calls:
		return c
	case <-time.After(watchdog):
		t.Fatal("no Aside reached the session")
		return nil
	}
}

// startedAsider is an engine over an asider, started.
func startedAsider(t *testing.T, serves bool) (*asider, *engine.Engine) {
	t.Helper()
	a := newAsider(serves)
	e, err := engine.New(a, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a, e
}

// asideOutcome is one Aside's return.
type asideOutcome struct {
	res agent.AsideResult
	err error
}

// goAside runs Aside on a goroutine of its own; its outcome comes on the
// channel, a panic recovered as an error.
func goAside(e *engine.Engine, ctx context.Context, q string, onDelta func(string)) <-chan asideOutcome {
	out := make(chan asideOutcome, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				out <- asideOutcome{err: errors.New("panicked")}
			}
		}()
		r, err := e.Aside(ctx, agent.AsideRequest{Question: q}, onDelta)
		out <- asideOutcome{r, err}
	}()
	return out
}

func outcome(t *testing.T, ch <-chan asideOutcome) asideOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(watchdog):
		t.Fatal("Aside did not return")
		return asideOutcome{}
	}
}

// wantCode fails unless err's protocol code and reason are code and reason.
func wantCode(t *testing.T, what string, err error, code, reason string) {
	t.Helper()
	if got, gotR := engine.Code(err), engine.Reason(err); got != code || gotR != reason {
		t.Fatalf("%s: %v is %s/%s, want %s/%s", what, err, got, gotR, code, reason)
	}
}

// TestAsideIsTheSessionsDoor (plan 040 §3.3, the refusal ladder): the engine
// adds only the door to the session's Aside. A request outside the protocol's
// bounds is ErrBadAside (bad_request) — a blank question, 21 prior pairs — or
// ErrAsideTooLong (text_too_long) — a question or prior question over 32 KiB,
// a prior answer over 64 KiB — and one at its bounds is asked; an engine not
// started, closed, or under a close fence refuses it (not_accepting, closing);
// a session with no side questions is aside_unsupported; a context already
// ended is aborted, context. None of those reaches the session. The session's
// own answers are translated: agent.ErrAsideNotReady is not_accepting,
// agent.ErrAsideUnsupported aside_unsupported, agent.ErrAsideEmpty failed, and
// an error of its own for a call whose context ended is the cancel (aborted,
// context). The answer and every streamed piece pass through as they are.
func TestAsideIsTheSessionsDoor(t *testing.T) {
	ctx := context.Background()

	t.Run("a session with no side questions", func(t *testing.T) {
		_, e, _ := newStubEngine(t)
		if e.Asides() {
			t.Fatal("the plain Stub says it answers side questions")
		}
		_, err := e.Aside(ctx, agent.AsideRequest{Question: "q"}, nil)
		if !errors.Is(err, agent.ErrAsideUnsupported) {
			t.Fatalf("got %v, want agent.ErrAsideUnsupported", err)
		}
		wantCode(t, "the plain Stub", err, "unsupported", "aside_unsupported")
		_, e = startedAsider(t, false)
		if _, err := e.Aside(ctx, agent.AsideRequest{Question: "q"}, nil); e.Asides() || !errors.Is(err, agent.ErrAsideUnsupported) {
			t.Fatalf("an Asider whose Asides is false: Asides %v, %v", e.Asides(), err)
		}
	})

	t.Run("the request's bounds", func(t *testing.T) {
		a, e := startedAsider(t, true)
		q32, a64 := strings.Repeat("q", protocol.AsideQuestionMax), strings.Repeat("a", protocol.AsideAnswerMax)
		prior := func(n int, q, ans string) []agent.AsideTurn {
			out := make([]agent.AsideTurn, n)
			for i := range out {
				out[i] = agent.AsideTurn{Question: q, Answer: ans}
			}
			return out
		}
		for _, tc := range []struct {
			name         string
			req          agent.AsideRequest
			code, reason string
			sentinel     error
		}{
			{"an empty question", agent.AsideRequest{}, "bad_request", "bad_request", engine.ErrBadAside},
			{"a blank question", agent.AsideRequest{Question: " \n\t"}, "bad_request", "bad_request", engine.ErrBadAside},
			{"21 prior pairs", agent.AsideRequest{Question: "q", Prior: prior(21, "q", "a")}, "bad_request", "bad_request", engine.ErrBadAside},
			{"a question over 32 KiB", agent.AsideRequest{Question: q32 + "q"}, "text_too_long", "text_too_long", engine.ErrAsideTooLong},
			{"a prior question over 32 KiB", agent.AsideRequest{Question: "q", Prior: prior(1, q32+"q", "a")}, "text_too_long", "text_too_long",
				engine.ErrAsideTooLong},
			{"a prior answer over 64 KiB", agent.AsideRequest{Question: "q", Prior: prior(1, "q", a64+"a")}, "text_too_long", "text_too_long",
				engine.ErrAsideTooLong},
		} {
			_, err := e.Aside(ctx, tc.req, nil)
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("%s: %v, want %v", tc.name, err, tc.sentinel)
			}
			wantCode(t, tc.name, err, tc.code, tc.reason)
		}
		// At every bound at once, it is asked.
		req := agent.AsideRequest{Question: q32, Prior: prior(protocol.AsidePriorMax, q32, a64)}
		done := make(chan asideOutcome, 1)
		go func() {
			r, err := e.Aside(ctx, req, nil)
			done <- asideOutcome{r, err}
		}()
		c := a.call(t)
		if len(c.req.Prior) != protocol.AsidePriorMax || c.req.Question != q32 {
			t.Fatalf("the session was asked %d bytes after %d pairs", len(c.req.Question), len(c.req.Prior))
		}
		c.step <- asideStep{res: agent.AsideResult{Text: "ok"}}
		if o := outcome(t, done); o.err != nil || o.res.Text != "ok" {
			t.Fatalf("at the bounds: %+v", o)
		}
	})

	t.Run("the engine's gate", func(t *testing.T) {
		a := newAsider(true)
		e, err := engine.New(a, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		if !e.Asides() {
			t.Fatal("an Asider says it answers no side questions")
		}
		q := agent.AsideRequest{Question: "q"}
		if _, err := e.Aside(ctx, q, nil); !errors.Is(err, engine.ErrNotAccepting) {
			t.Fatalf("before the start: %v, want ErrNotAccepting", err)
		}
		if err := e.Start(ctx); err != nil {
			t.Fatal(err)
		}
		release, _ := e.FenceClose()
		_, err = e.Aside(ctx, q, nil)
		release()
		if !errors.Is(err, engine.ErrClosing) {
			t.Fatalf("under a close fence: %v, want ErrClosing", err)
		}
		dead, cancel := context.WithCancel(ctx)
		cancel()
		_, err = e.Aside(dead, q, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("on an ended context: %v, want context.Canceled", err)
		}
		wantCode(t, "an ended context", err, "aborted", "context")
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Aside(ctx, q, nil); !errors.Is(err, engine.ErrNotAccepting) {
			t.Fatalf("after the close: %v, want ErrNotAccepting", err)
		}
		select {
		case c := <-a.calls:
			t.Fatalf("a refused aside reached the session: %+v", c.req)
		default:
		}
	})

	t.Run("the session's answers", func(t *testing.T) {
		a, e := startedAsider(t, true)
		var pieces []string
		done := goAside(e, ctx, "why?", func(s string) { pieces = append(pieces, s) })
		c := a.call(t)
		c.onDelta("be")
		c.onDelta("cause")
		usage := &agent.AsideUsage{Input: 10, Output: 2, CacheRead: 8}
		c.step <- asideStep{res: agent.AsideResult{Text: "because", Model: "M", Usage: usage}}
		o := outcome(t, done)
		if o.err != nil || o.res.Text != "because" || o.res.Model != "M" || o.res.Usage != usage ||
			strings.Join(pieces, "|") != "be|cause" || c.req.Question != "why?" {
			t.Fatalf("the answer: %+v, pieces %q", o, pieces)
		}
		for _, tc := range []struct {
			name         string
			err          error
			code, reason string
			sentinel     error
		}{
			{"not ready", agent.ErrAsideNotReady, "not_accepting", "not_accepting", engine.ErrNotAccepting},
			{"unsupported", agent.ErrAsideUnsupported, "unsupported", "aside_unsupported", agent.ErrAsideUnsupported},
			{"no answer", agent.ErrAsideEmpty, "failed", "failed", agent.ErrAsideEmpty},
			{"a provider's failure", errors.New("model overloaded"), "failed", "failed", nil},
		} {
			done := goAside(e, ctx, "q", nil)
			a.call(t).step <- asideStep{err: tc.err}
			o := outcome(t, done)
			if tc.sentinel != nil && !errors.Is(o.err, tc.sentinel) || !errors.Is(o.err, tc.err) {
				t.Fatalf("%s: %v", tc.name, o.err)
			}
			wantCode(t, tc.name, o.err, tc.code, tc.reason)
		}
		cctx, cancel := context.WithCancel(ctx)
		done = goAside(e, cctx, "q", nil)
		a.call(t).step <- asideStep{waitCtx: true}
		cancel()
		o = outcome(t, done)
		if !errors.Is(o.err, context.Canceled) {
			t.Fatalf("cancelled mid-call: %v, want context.Canceled", o.err)
		}
		wantCode(t, "cancelled mid-call", o.err, "aborted", "context")
	})
}

// TestACancelledAsideIsAbortedWhateverTheSessionSays (plan 040 §3.3, astra
// P2): an aside whose context ends while the session has it is the cancel —
// context.Canceled, aborted, reason context — whatever the session then
// answers: agent.ErrAsideNotReady (its shutdown, raced by the cancel, reached
// its readiness check first), which with the context live is not_accepting,
// agent.ErrAsideUnsupported, or an error of its own. Negative control: a
// translation that looks at agent.ErrAsideNotReady before the context answers
// not_accepting.
func TestACancelledAsideIsAbortedWhateverTheSessionSays(t *testing.T) {
	a, e := startedAsider(t, true)
	for _, said := range []error{agent.ErrAsideNotReady, agent.ErrAsideUnsupported, errors.New("pipe closed")} {
		ctx, cancel := context.WithCancel(context.Background())
		done := goAside(e, ctx, "q", nil)
		c := a.call(t)
		cancel()
		c.step <- asideStep{err: said}
		o := outcome(t, done)
		if !errors.Is(o.err, context.Canceled) || errors.Is(o.err, engine.ErrNotAccepting) {
			t.Fatalf("cancelled, the session saying %v: %v; want context.Canceled alone", said, o.err)
		}
		wantCode(t, "cancelled, the session saying "+said.Error(), o.err, "aborted", "context")
	}
	if n := e.AsidesInFlight(); n != 0 {
		t.Fatalf("%d in flight once every one returned, want 0", n)
	}
}

// TestAsidesInFlightAreCappedAndCountedOut (plan 040 §3.2, §3.3): two side
// questions may be in flight on a session; a third is ErrAsideBusy
// (unavailable, aside_busy) without reaching the session, and the count is
// back to 0 once each has returned — answered, failed, cancelled or panicked
// — so two more are admitted after every one of them. Negative control: a
// count-out that skipped the panic would leave one in flight, and the last
// pair would find the session busy.
func TestAsidesInFlightAreCappedAndCountedOut(t *testing.T) {
	a, e := startedAsider(t, true)
	ctx := context.Background()
	pair := func(first, second asideStep, cancelSecond bool) {
		t.Helper()
		sctx, cancel := context.WithCancel(ctx)
		defer cancel()
		d1 := goAside(e, ctx, "one", nil)
		c1 := a.call(t)
		d2 := goAside(e, sctx, "two", nil)
		c2 := a.call(t)
		if n := e.AsidesInFlight(); n != 2 {
			t.Fatalf("%d in flight with two asked, want 2", n)
		}
		_, err := e.Aside(ctx, agent.AsideRequest{Question: "three"}, nil)
		if !errors.Is(err, engine.ErrAsideBusy) {
			t.Fatalf("a third: %v, want ErrAsideBusy", err)
		}
		wantCode(t, "a third", err, "unavailable", "aside_busy")
		c1.step <- first
		outcome(t, d1)
		c2.step <- second
		if cancelSecond {
			cancel()
		}
		outcome(t, d2)
		if n := e.AsidesInFlight(); n != 0 {
			t.Fatalf("%d in flight once both returned, want 0", n)
		}
	}
	pair(asideStep{res: agent.AsideResult{Text: "a"}}, asideStep{err: errors.New("failed")}, false)
	pair(asideStep{panics: true}, asideStep{waitCtx: true}, true)
	pair(asideStep{res: agent.AsideResult{Text: "a"}}, asideStep{res: agent.AsideResult{Text: "b"}}, false)
	select {
	case c := <-a.calls:
		t.Fatalf("a refused aside reached the session: %+v", c.req)
	default:
	}
}
