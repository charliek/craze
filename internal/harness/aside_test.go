package harness

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
)

// Side questions (aside.go, plan 040 §3.4, §7.5): the request a side question
// sends is the next turn's up to its new user message; earlier pairs follow
// in order, wrapped the same way each time; no two user messages in a row;
// the answer streams and is the streamed text; a tool call, the refusal
// state, a child and a closed session are refused or answered as errors; one
// retry, only before any text and only for a transient failure; the context
// is the attached turn's generation, read under one store lock; usage counts
// in the session's spend and no turn's; and Close cancels and joins a side
// question, which writes nothing to the transcript.

// asideOutcome is what an Aside returned, with the text pieces it streamed.
type asideOutcome struct {
	a      AsideAnswer
	deltas []string
	err    error
}

// askCtx is Aside with ctx, collecting the pieces it streams.
func askCtx(ctx context.Context, s *Session, prior []AsidePair, q string) asideOutcome {
	var mu sync.Mutex
	var deltas []string
	a, err := s.Aside(ctx, prior, q, func(d string) {
		mu.Lock()
		defer mu.Unlock()
		deltas = append(deltas, d)
	})
	mu.Lock()
	defer mu.Unlock()
	return asideOutcome{a: a, deltas: deltas, err: err}
}

// ask is askCtx on the test's goroutine, with a background context.
func ask(s *Session, prior []AsidePair, q string) asideOutcome {
	return askCtx(context.Background(), s, prior, q)
}

// startAside is askCtx on its own goroutine.
func startAside(ctx context.Context, s *Session, prior []AsidePair, q string) <-chan asideOutcome {
	out := make(chan asideOutcome, 1)
	go func() { out <- askCtx(ctx, s, prior, q) }()
	return out
}

// pauseAsideRead makes the next Aside stop between its reading of the turn
// link and its reading of the store (Session.asideRead): reached is closed
// when it has stopped, and it goes on once the test calls proceed. The test
// acts on its own goroutine meanwhile.
func pauseAsideRead(s *Session) (reached chan struct{}, proceed func()) {
	reached, gone := make(chan struct{}), make(chan struct{})
	s.asideRead = func() {
		close(reached)
		<-gone
	}
	return reached, func() { close(gone) }
}

// withoutPrompt is c with no messages: what a side question's request and a
// turn's share field for field.
func withoutPrompt(c fantasy.Call) fantasy.Call {
	c.Prompt = nil
	return c
}

// userRuns reports the first place two user messages follow each other in
// msgs, or -1.
func userRuns(msgs []fantasy.Message) int {
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == fantasy.MessageRoleUser && msgs[i-1].Role == fantasy.MessageRoleUser {
			return i
		}
	}
	return -1
}

// textCount is how many text parts in msgs are exactly text.
func textCount(msgs []fantasy.Message, text string) int {
	n := 0
	for _, m := range msgs {
		for _, p := range m.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok && tp.Text == text {
				n++
			}
		}
	}
	return n
}

// TestAsideRequestIsTheNextTurnsPrefix (§7.5): with no turn running, the
// side request's system prompt, tool definitions, effort options, headers,
// output ceiling and history are the next turn's request up to that turn's
// new user message, and the side question is a user message after them.
func TestAsideRequestIsTheNextTurnsPrefix(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(answerWith("hi"), answerWith("there"))
	run(t, s, "hello")
	run(t, s, "world")

	a.push(answerWith("you said hello"), answerWith("next answer"))
	o := ask(s, nil, "what did I say?")
	if o.err != nil {
		t.Fatalf("Aside: %v", o.err)
	}
	run(t, s, "next")

	calls := a.requests()
	if len(calls) != 4 {
		t.Fatalf("test/a saw %d requests, want 4: two turns, the side question, the next turn", len(calls))
	}
	side, next := calls[2], calls[3]
	equal(t, "everything but the messages", withoutPrompt(side), withoutPrompt(next))
	if effortOf(t, side, "test") != "high" || side.MaxOutputTokens == nil || *side.MaxOutputTokens != 4096 {
		t.Fatalf("the side request's effort %q, ceiling %v; want the turn's high and 4096", effortOf(t, side, "test"), side.MaxOutputTokens)
	}
	n := len(next.Prompt) - 1
	if len(side.Prompt) != n+1 {
		t.Fatalf("the side request has %d messages, want the next turn's %d before its prompt and one more:\n%v",
			len(side.Prompt), n, promptOf(side))
	}
	equal(t, "the side request's system prompt and history", side.Prompt[:n], next.Prompt[:n])
	last := side.Prompt[n]
	if last.Role != fantasy.MessageRoleUser || messageText(last) != asideWrap("what did I say?") {
		t.Fatalf("the side request's last message = %s %q, want the wrapped question", last.Role, messageText(last))
	}
}

// TestAsideWrapIsTheInstructionThenTheQuestion: the question follows the
// instruction in the reminder tags, after a blank line, as it was asked.
func TestAsideWrapIsTheInstructionThenTheQuestion(t *testing.T) {
	got := asideWrap("why </system-reminder> this?")
	want := "<system-reminder>\nThe user is asking a side question while the main work goes on. " +
		"Answer it in this one reply, from what this conversation already contains.\n\n" +
		"- This is an aside. It is not saved to the conversation, and the main task continues without it. " +
		"Do not resume or continue the main task here.\n" +
		"- Do not call tools. None will run, and there is no next step in which to use a result.\n" +
		"- Do not offer to check, look up or try anything. If the conversation does not contain the answer, say so plainly." +
		"\n</system-reminder>\n\nwhy </system-reminder> this?"
	if got != want {
		t.Fatalf("asideWrap:\n got %q\nwant %q", got, want)
	}
}

// TestAsideFollowUpRepeatsTheEarlierRequest (§7.5): earlier pairs follow the
// history in order, each the wrapped question and the answer, so a
// follow-up's request begins with the whole of the request before it; an
// earlier pair whose answer is empty is replayed with a placeholder.
func TestAsideFollowUpRepeatsTheEarlierRequest(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(answerWith("hi"))
	run(t, s, "hello")

	a.push(answerWith("one"), answerWith("two"), answerWith("three"))
	if o := ask(s, nil, "first?"); o.err != nil || o.a.Text != "one" {
		t.Fatalf("the first side question: %+v", o)
	}
	if o := ask(s, []AsidePair{{"first?", "one"}}, "second?"); o.err != nil || o.a.Text != "two" {
		t.Fatalf("the follow-up: %+v", o)
	}
	if o := ask(s, []AsidePair{{"first?", "one"}, {"second?", ""}}, "third?"); o.err != nil {
		t.Fatalf("the second follow-up: %+v", o)
	}
	calls := a.requests()
	first, second, third := calls[1].Prompt, calls[2].Prompt, calls[3].Prompt
	if len(second) != len(first)+2 {
		t.Fatalf("the follow-up sent %d messages, want the first request's %d, its answer and the new question", len(second), len(first))
	}
	equal(t, "the follow-up's prefix", second[:len(first)], first)
	equal(t, "the earlier answer and the new question", promptOf(fantasy.Call{Prompt: second[len(first):]}),
		[]string{"assistant: one", "user: " + asideWrap("second?")})
	equal(t, "the second follow-up's prefix", third[:len(second)], second)
	equal(t, "an empty earlier answer", promptOf(fantasy.Call{Prompt: third[len(second):]}),
		[]string{"assistant: " + asideNoAnswer, "user: " + asideWrap("third?")})
}

// TestAsideDuringAHeldTurn (§7.5): a side question while a turn's first step
// streams sees the turn's held prompt exactly once, and never sends two user
// messages in a row: the first question goes into the held prompt's message,
// and a follow-up repeats that request's prefix.
func TestAsideDuringAHeldTurn(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(answerWith("hi"))
	run(t, s, "hello")

	g := newGate()
	a.push(g.hold(openText("working"), finishText()))
	turn := start(context.Background(), s, "do the thing", nil)
	await(t, g.reached, "the turn's step")

	a.push(answerWith("side one"), answerWith("side two"))
	if o := ask(s, nil, "q1"); o.err != nil {
		t.Fatalf("the side question: %v", o.err)
	}
	if o := ask(s, []AsidePair{{"q1", "side one"}}, "q2"); o.err != nil {
		t.Fatalf("the follow-up: %v", o.err)
	}
	close(g.release)
	if o := await(t, turn, "the turn"); o.err != nil {
		t.Fatal(o.err)
	}

	calls := a.requests()
	side1, side2 := calls[2].Prompt, calls[3].Prompt
	for i, p := range [][]fantasy.Message{side1, side2} {
		if at := userRuns(p); at >= 0 {
			t.Fatalf("side request %d has two user messages in a row at %d:\n%v", i+1, at, promptOf(fantasy.Call{Prompt: p}))
		}
		if n := textCount(p, "do the thing"); n != 1 {
			t.Fatalf("side request %d holds the turn's prompt %d times, want once", i+1, n)
		}
	}
	last := side1[len(side1)-1]
	equal(t, "the held prompt's message", messageText(last), "do the thing "+asideWrap("q1"))
	equal(t, "the follow-up's prefix", side2[:len(side1)], side1)
	equal(t, "the follow-up's tail", promptOf(fantasy.Call{Prompt: side2[len(side1):]}),
		[]string{"assistant: side one", "user: " + asideWrap("q2")})
	// The turn's own request was not touched: its prompt is its own message.
	if got := messageText(calls[1].Prompt[len(calls[1].Prompt)-1]); got != "do the thing" {
		t.Fatalf("the turn's own prompt became %q", got)
	}
}

// TestAsideStreamsTheWholeAnswer (§7.5): every text piece reaches onText in
// order, reasoning never does, and the answer is the pieces joined — not the
// response's first text part, which would leave out a second block.
func TestAsideStreamsTheWholeAnswer(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	second := []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextStart, ID: "1"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: " and c"},
		{Type: fantasy.StreamPartTypeTextEnd, ID: "1"},
	}
	f.models["test/a"].push(reply(reasoningParts("hmm"), textParts("a", "", "b"), second, finish(fantasy.FinishReasonStop)))
	o := ask(s, nil, "q")
	if o.err != nil {
		t.Fatal(o.err)
	}
	equal(t, "the pieces", o.deltas, []string{"a", "b", " and c"})
	equal(t, "the answer", o.a, AsideAnswer{Text: "ab and c", Model: "Model A", Usage: scriptedUsage})
}

// TestAsideWithNoText (§7.5): an answer that is only a tool call is
// ErrAsideToolCall, which is ErrAsideEmpty; one with no text at all, or only
// whitespace, is ErrAsideEmpty alone. The usage is noted all the same.
func TestAsideWithNoText(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(callStep(callParts("c1", "read", `{"path":"x"}`)), reply(finish(fantasy.FinishReasonStop)), answerWith(" \n "))

	o := ask(s, nil, "q")
	if !errors.Is(o.err, ErrAsideToolCall) || !errors.Is(o.err, ErrAsideEmpty) {
		t.Fatalf("a tool call answered %v, want ErrAsideToolCall", o.err)
	}
	if !strings.Contains(o.err.Error(), "tried to use a tool") {
		t.Fatalf("the tool call's error %q does not say so", o.err)
	}
	for _, what := range []string{"no text", "whitespace"} {
		o := ask(s, nil, "q")
		if !errors.Is(o.err, ErrAsideEmpty) || errors.Is(o.err, ErrAsideToolCall) {
			t.Fatalf("%s answered %v, want ErrAsideEmpty and not the tool call's", what, o.err)
		}
	}
	if got := len(a.requests()); got != 3 {
		t.Fatalf("three side questions sent %d requests, want 3", got)
	}
	sp, ok := s.IdleSpent()
	if !ok || sp.Session != unpricedSpend(addUsage(addUsage(scriptedUsage, scriptedUsage), scriptedUsage)) {
		t.Fatalf("IdleSpent = %+v, %v; want the three requests' usage in the session's spend", sp, ok)
	}
}

// TestAsideRefusals (§7.5): a session in the refusal state, a sub-agent's
// session and a closed one each refuse, sending nothing; a caller's context
// that ends returns its error.
func TestAsideRefusals(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(answerWith("never sent"))

	s.tools.refusing.Store(true)
	if o := ask(s, nil, "q"); !errors.Is(o.err, ErrStoredKeyFrozen) {
		t.Fatalf("the refusal state answered %v, want ErrStoredKeyFrozen", o.err)
	}
	s.tools.refusing.Store(false)

	s.child = true
	if o := ask(s, nil, "q"); !errors.Is(o.err, errAsideChild) {
		t.Fatalf("a child answered %v, want errAsideChild", o.err)
	}
	s.child = false

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Aside(ctx, nil, "q", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("an ended context answered %v, want context.Canceled", err)
	}
	if n := len(a.requests()); n != 0 {
		t.Fatalf("the refusals sent %d requests, want none", n)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if o := ask(s, nil, "q"); !errors.Is(o.err, ErrClosed) {
		t.Fatalf("a closed session answered %v, want ErrClosed", o.err)
	}
	if n := len(a.requests()); n != 0 {
		t.Fatalf("a closed session sent %d requests, want none", n)
	}
}

// TestAsideGateRefusesAKeyLearnedBeforeTheRequest (§3.4 step 1, Codex review
// of C2): a key learned after Aside's own check of the refusal state and
// before its request goes out — here, between its reading of the turn link
// and of the store — is inside the frozen prompt the request would carry, so
// the gate at the moment of sending (asideGate) refuses it: nothing is sent,
// and the answer is ErrStoredKeyFrozen, not a provider's failure.
func TestAsideGateRefusesAKeyLearnedBeforeTheRequest(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(answerWith("never sent"))
	s.asideRead = func() { s.tools.refusing.Store(true) }
	o := ask(s, nil, "q")
	if n := len(a.requests()); n != 0 {
		t.Fatalf("a key learned before the request: %d requests sent, want none", n)
	}
	if !errors.Is(o.err, ErrStoredKeyFrozen) {
		t.Fatalf("a key learned before the request answered %v, want ErrStoredKeyFrozen", o.err)
	}
}

// TestAsideRetriesOnceBeforeAnyText (§7.5): a transient failure before any
// text is retried once, after asideRetryWait; one after text, a fatal one,
// and a second transient one are not.
func TestAsideRetriesOnceBeforeAnyText(t *testing.T) {
	transient := func() step { return errorStep(&fantasy.ProviderError{StatusCode: 500, Message: "boom"}) }
	fatal := func() step { return errorStep(&fantasy.ProviderError{StatusCode: 400, Message: "bad request"}) }
	afterText := func() step {
		return reply(openText("par"), errorPart(&fantasy.ProviderError{StatusCode: 500, Message: "boom"}))
	}
	for _, tc := range []struct {
		name     string
		steps    []step
		requests int
		ok       bool
		deltas   []string
	}{
		{"transient, then an answer", []step{transient(), answerWith("ok")}, 2, true, []string{"ok"}},
		{"transient twice", []step{transient(), transient(), answerWith("never")}, 2, false, nil},
		{"transient after text", []step{afterText(), answerWith("never")}, 1, false, []string{"par"}},
		{"fatal", []step{fatal(), answerWith("never")}, 1, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "http://unused")
			s := f.open(f.options())
			var waits []time.Duration
			s.sleep = func(_ context.Context, d time.Duration) { waits = append(waits, d) }
			a := f.models["test/a"]
			a.push(tc.steps...)
			o := ask(s, nil, "q")
			if got := len(a.requests()); got != tc.requests {
				t.Fatalf("sent %d requests, want %d (err %v)", got, tc.requests, o.err)
			}
			if (o.err == nil) != tc.ok {
				t.Fatalf("Aside's error = %v, want success %v", o.err, tc.ok)
			}
			if !tc.ok {
				var pe *ProviderError
				if !errors.As(o.err, &pe) {
					t.Fatalf("the failure %v is not classified", o.err)
				}
			}
			equal(t, "the pieces", o.deltas, tc.deltas)
			wantWaits := []time.Duration(nil)
			if tc.requests == 2 {
				wantWaits = []time.Duration{asideRetryWait}
			}
			equal(t, "the waits", waits, wantWaits)
		})
	}
}

// TestAsideTurnGenerations (§7.5, P5): the side question's context is the
// generation of the turn link it read — never a dead or the next turn's held
// prompt, never the prompt twice — whatever the turns do between its reading
// of the link and its reading of the store.
func TestAsideTurnGenerations(t *testing.T) {
	ctx := context.Background()

	// Read idle, before a turn begins; the store read after the turn has
	// begun, appended its prompt and is streaming: the prompt is the next
	// turn's, not the side question's.
	t.Run("idle, then a turn begins before the store read", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		s := f.open(f.options())
		a := f.models["test/a"]
		a.push(answerWith("hi"))
		run(t, s, "hello")

		reached, proceed := pauseAsideRead(s)
		side := startAside(ctx, s, nil, "q")
		await(t, reached, "the side question's basis")
		g := newGate()
		a.push(g.hold(openText("working"), finishText()), answerWith("side"))
		turn := start(ctx, s, "the next prompt", nil)
		await(t, g.reached, "the next turn's step")
		proceed()
		if o := await(t, side, "the side question"); o.err != nil {
			t.Fatal(o.err)
		}
		close(g.release)
		await(t, turn, "the turn")
		p := a.requests()[2].Prompt
		if n := textCount(p, "the next prompt"); n != 0 {
			t.Fatalf("the side request holds the next turn's prompt %d times, want none", n)
		}
		equal(t, "the side request", promptOf(fantasy.Call{Prompt: p})[1:],
			[]string{"user: hello", "assistant: hi", "user: " + asideWrap("q")})
	})

	// Read during turn 1, which ends — its step written — before the store
	// read: its prompt is in the history once, with its answer.
	t.Run("the turn ends before the store read", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		s := f.open(f.options())
		a := f.models["test/a"]
		g := newGate()
		a.push(g.hold(openText("work"), finishText()))
		turn := start(ctx, s, "the prompt", nil)
		await(t, g.reached, "the turn's step")

		reached, proceed := pauseAsideRead(s)
		side := startAside(ctx, s, nil, "q")
		await(t, reached, "the side question's basis")
		a.push(answerWith("side"))
		close(g.release)
		if o := await(t, turn, "the turn"); o.err != nil {
			t.Fatal(o.err)
		}
		proceed()
		if o := await(t, side, "the side question"); o.err != nil {
			t.Fatal(o.err)
		}
		p := a.requests()[1].Prompt
		equal(t, "the side request", promptOf(fantasy.Call{Prompt: p})[1:],
			[]string{"user: the prompt", "assistant: work", "user: " + asideWrap("q")})
	})

	// Read during turn 1; before the store read turn 1 ends and turn 2 is
	// admitted at once, its prompt held: the history is turn 1's, without
	// turn 2's prompt.
	t.Run("the next turn is admitted before the store read", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		s := f.open(f.options())
		a := f.models["test/a"]
		g1, g2 := newGate(), newGate()
		a.push(g1.hold(openText("one"), finishText()))
		turn1 := start(ctx, s, "prompt one", nil)
		await(t, g1.reached, "turn 1's step")

		reached, proceed := pauseAsideRead(s)
		side := startAside(ctx, s, nil, "q")
		await(t, reached, "the side question's basis")
		close(g1.release)
		await(t, turn1, "turn 1")
		a.push(g2.hold(openText("two"), finishText()), answerWith("side"))
		turn2 := start(ctx, s, "prompt two", nil)
		await(t, g2.reached, "turn 2's step")
		proceed()
		if o := await(t, side, "the side question"); o.err != nil {
			t.Fatal(o.err)
		}
		close(g2.release)
		await(t, turn2, "turn 2")
		p := a.requests()[2].Prompt
		equal(t, "the side request", promptOf(fantasy.Call{Prompt: p})[1:],
			[]string{"user: prompt one", "assistant: one", "user: " + asideWrap("q")})
	})

	// Read during turn 2, which is cancelled before any output and returns
	// before the store read: its prompt, never written, stays held until the
	// next turn begins. The link the side question read has gone, so the
	// history is read again with no held prompt — never the dead one.
	t.Run("the turn is cancelled with no output before the store read", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		s := f.open(f.options())
		a := f.models["test/a"]
		a.push(answerWith("hi"))
		run(t, s, "hello")
		g := newGate()
		a.push(g.hold(nil, nil))
		turnCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		turn := start(turnCtx, s, "the dead prompt", nil)
		await(t, g.reached, "turn 2's step")

		reached, proceed := pauseAsideRead(s)
		side := startAside(ctx, s, nil, "q")
		await(t, reached, "the side question's basis")
		a.push(answerWith("side"))
		cancel()
		if o := await(t, turn, "turn 2"); o.err != nil || o.res.StopReason != StopCancelled {
			t.Fatalf("turn 2 returned %+v, want cancelled", o)
		}
		proceed()
		if o := await(t, side, "the side question"); o.err != nil {
			t.Fatal(o.err)
		}
		p := a.requests()[2].Prompt
		if n := textCount(p, "the dead prompt"); n != 0 {
			t.Fatalf("the side request holds the cancelled turn's prompt %d times, want none", n)
		}
		equal(t, "the side request", promptOf(fantasy.Call{Prompt: p})[1:],
			[]string{"user: hello", "assistant: hi", "user: " + asideWrap("q")})
	})

	// A model switch during the turn: the side question asks the turn's
	// model, whose reasoning and cache the history is, not the new one.
	t.Run("a model switch mid-turn", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		s := f.open(f.options())
		a, b := f.models["test/a"], f.models["test/b"]
		g := newGate()
		a.push(g.hold(openText("work"), finishText()))
		turn := start(ctx, s, "the prompt", nil)
		await(t, g.reached, "the turn's step")
		if err := s.SetModel("test/b"); err != nil {
			t.Fatal(err)
		}
		a.push(answerWith("from a"))
		b.push(answerWith("from b"))
		o := ask(s, nil, "q")
		close(g.release)
		await(t, turn, "the turn")
		if o.err != nil || o.a.Text != "from a" || o.a.Model != "Model A" {
			t.Fatalf("the side question answered %+v; want test/a's answer", o)
		}
		if n := len(b.requests()); n != 0 {
			t.Fatalf("the newly chosen model got %d requests, want none", n)
		}
	})
}

// TestAsideWhileATurnStreams (§7.5): a side question runs to its end while a
// turn is held mid-stream on the same model client; the turn then finishes
// as it would have. Run under -race -count=20.
func TestAsideWhileATurnStreams(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(answerWith("hi"))
	run(t, s, "hello")

	g := newGate()
	a.push(g.hold(openText("str", "eam"), finishText()))
	var ev events
	turn := start(context.Background(), s, "go on", ev.sink)
	await(t, g.reached, "the turn's step")
	a.push(reply(textParts("side ", "answer"), finish(fantasy.FinishReasonStop)))
	side := startAside(context.Background(), s, nil, "what now?")
	o := await(t, side, "the side question")
	close(g.release)
	res := await(t, turn, "the turn")
	if o.err != nil || o.a.Text != "side answer" {
		t.Fatalf("the side question: %+v", o)
	}
	if res.err != nil || res.res.StopReason != StopEndTurn {
		t.Fatalf("the turn: %+v", res)
	}
	var text strings.Builder
	for _, d := range of[TextDelta](ev.list()) {
		text.WriteString(d.Text)
	}
	if text.String() != "stream" {
		t.Fatalf("the turn streamed %q, want its own text alone", text.String())
	}
}

// TestAsideUsageIsTheSessionsNotATurns (§7.5): a side question's usage is in
// the session's spend and in no turn's — idle, where IdleSpent reports it,
// and mid-turn, where the turn's next step's Spent carries it.
func TestAsideUsageIsTheSessionsNotATurns(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	a := f.models["test/a"]
	one := unpricedSpend(scriptedUsage)
	a.push(answerWith("hi"), answerWith("side"))
	run(t, s, "hello")
	if o := ask(s, nil, "q"); o.err != nil || o.a.Usage != scriptedUsage {
		t.Fatalf("the side question: %+v", o)
	}
	sp, ok := s.IdleSpent()
	if !ok {
		t.Fatal("IdleSpent reported nothing with no turn running")
	}
	equal(t, "idle: the last turn's spend", sp.Turn, one)
	equal(t, "idle: the session's spend", sp.Session, unpricedSpend(addUsage(scriptedUsage, scriptedUsage)))

	g := newGate()
	a.push(g.hold(openText("work"), finishText()), answerWith("side again"))
	var ev events
	turn := start(context.Background(), s, "more", ev.sink)
	await(t, g.reached, "turn 2's step")
	if o := ask(s, nil, "q2"); o.err != nil {
		t.Fatal(o.err)
	}
	close(g.release)
	await(t, turn, "turn 2")
	last := lastSpent(t, ev.list())
	equal(t, "mid-turn: turn 2's spend", last.Turn, one)
	equal(t, "mid-turn: the session's spend", last.Session,
		unpricedSpend(addUsage(addUsage(scriptedUsage, scriptedUsage), addUsage(scriptedUsage, scriptedUsage))))
}

// TestIdleSpentOnlyWhenIdle (§7.5): IdleSpent reports nothing while a turn
// runs, and the session's spend when none does, numbered after every report
// before it.
func TestIdleSpentOnlyWhenIdle(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	a := f.models["test/a"]
	g := newGate()
	a.push(g.hold(openText("work"), finishText()))
	var ev events
	turn := start(context.Background(), s, "go", ev.sink)
	await(t, g.reached, "the turn's step")
	if sp, ok := s.IdleSpent(); ok {
		t.Fatalf("IdleSpent reported %+v while a turn runs", sp)
	}
	close(g.release)
	await(t, turn, "the turn")
	sp, ok := s.IdleSpent()
	if !ok {
		t.Fatal("IdleSpent reported nothing once the turn had ended")
	}
	step := lastSpent(t, ev.list())
	if sp.Seq <= step.Seq {
		t.Fatalf("IdleSpent's Seq %d is not after the step's %d", sp.Seq, step.Seq)
	}
	step.Seq = sp.Seq
	equal(t, "IdleSpent after the turn", sp, step)
	if again, _ := s.IdleSpent(); again.Seq <= sp.Seq {
		t.Fatalf("a second IdleSpent's Seq %d is not after the first's %d", again.Seq, sp.Seq)
	}
}

// TestIdleSpentSeqIsTakenWithWhatItReads (§3.4 step 8, Codex review of C2):
// IdleSpent numbers its report in the section of mu that reads the turn count
// and the model it reports on, so one paused after that reading while a whole
// turn runs and reports carries a Seq below every report of that turn, and a
// consumer that applies only a higher Seq (the native adapter's spent) drops
// it, however late it is computed.
func TestIdleSpentSeqIsTakenWithWhatItReads(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(answerWith("one"))
	run(t, s, "one")

	reached, gone := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.idleSpentRead = func() {
		once.Do(func() {
			close(reached)
			<-gone
		})
	}
	stale := make(chan Spent, 1)
	go func() {
		sp, ok := s.IdleSpent()
		if !ok {
			t.Error("IdleSpent reported nothing with no turn running")
		}
		stale <- sp
	}()
	await(t, reached, "IdleSpent's reading")
	a.push(callStep(callParts("c1", "read", input(t, map[string]any{"filePath": "a.txt"}))), answerWith("two"))
	var ev events
	if _, err := s.Run(context.Background(), "two", ev.sink); err != nil {
		t.Fatal(err)
	}
	close(gone)
	old := await(t, stale, "the paused IdleSpent")

	reports := of[Spent](ev.list())
	if len(reports) != 2 {
		t.Fatalf("turn 2 made %d reports, want one per step", len(reports))
	}
	last := reports[len(reports)-1]
	equal(t, "turn 2's spend in its last report", last.Turn, times(2, unpricedSpend(scriptedUsage)))
	equal(t, "turn 1's spend in the paused report", old.Turn, unpricedSpend(scriptedUsage))
	for _, r := range reports {
		if old.Seq >= r.Seq {
			t.Fatalf("the paused IdleSpent's Seq %d is not below turn 2's report's %d: its older data would replace the newer", old.Seq, r.Seq)
		}
	}
	// The consumer's rule, applied in the order the reports arrived: turn
	// 2's, then the paused one.
	var applied Spent
	for _, r := range append(reports, old) {
		if r.Seq > applied.Seq {
			applied = r
		}
	}
	equal(t, "the report applied last", applied, last)
}

// TestAnAsidesReportIsOwedPastATurnsLast (§3.4 step 8, Codex review of C2): a
// side question that notes its usage after a running turn's last report, and
// is refused IdleSpent because the turn has not yet ended, is owed its
// report: OwedSpent makes it once the turn has ended — once, numbered after
// the turn's, with the side question's usage in the session's spend — and
// never while the turn runs. A report the turn makes after the refusal counts
// that usage, and settles the debt.
func TestAnAsidesReportIsOwedPastATurnsLast(t *testing.T) {
	t.Run("after the turn's last report", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		s := f.open(f.options())
		a := f.models["test/a"]
		a.push(answerWith("work"), answerWith("side"))
		reached, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		var ev events
		// The turn is held in its sink as its last report goes out: from
		// here until the release it has no report left to make, and has not
		// ended.
		turn := start(context.Background(), s, "go", func(e Event) {
			ev.sink(e)
			if _, ok := e.(Spent); ok {
				once.Do(func() {
					close(reached)
					<-release
				})
			}
		})
		await(t, reached, "the turn's last report")
		if o := ask(s, nil, "q"); o.err != nil {
			t.Fatal(o.err)
		}
		if sp, ok := s.IdleSpent(); ok {
			t.Fatalf("IdleSpent reported %+v while the turn runs", sp)
		}
		if sp, ok := s.OwedSpent(); ok {
			t.Fatalf("OwedSpent reported %+v while the turn runs", sp)
		}
		close(release)
		if o := await(t, turn, "the turn"); o.err != nil {
			t.Fatal(o.err)
		}
		last := lastSpent(t, ev.list())
		equal(t, "the turn's last report: the session's spend", last.Session, unpricedSpend(scriptedUsage))
		owed, ok := s.OwedSpent()
		if !ok {
			t.Fatal("OwedSpent reported nothing once the turn had ended: the side question's usage is in no report")
		}
		if owed.Seq <= last.Seq {
			t.Fatalf("the owed report's Seq %d is not after the turn's %d", owed.Seq, last.Seq)
		}
		equal(t, "the owed report: the turn's spend", owed.Turn, last.Turn)
		equal(t, "the owed report: the session's spend", owed.Session, unpricedSpend(addUsage(scriptedUsage, scriptedUsage)))
		if sp, ok := s.OwedSpent(); ok {
			t.Fatalf("OwedSpent reported a second time: %+v", sp)
		}
	})

	t.Run("before the turn's next report", func(t *testing.T) {
		f := newFixture(t, "http://unused")
		s := f.open(f.options())
		a := f.models["test/a"]
		g := newGate()
		a.push(g.hold(openText("work"), finishText()), answerWith("side"))
		var ev events
		turn := start(context.Background(), s, "go", ev.sink)
		await(t, g.reached, "the turn's step")
		if o := ask(s, nil, "q"); o.err != nil {
			t.Fatal(o.err)
		}
		if sp, ok := s.IdleSpent(); ok {
			t.Fatalf("IdleSpent reported %+v while the turn runs", sp)
		}
		close(g.release)
		if o := await(t, turn, "the turn"); o.err != nil {
			t.Fatal(o.err)
		}
		equal(t, "the turn's report: the session's spend", lastSpent(t, ev.list()).Session,
			unpricedSpend(addUsage(scriptedUsage, scriptedUsage)))
		if sp, ok := s.OwedSpent(); ok {
			t.Fatalf("OwedSpent reported %+v, though the turn's own report counted the side question", sp)
		}
	})
}

// TestCloseCancelsAndJoinsAnAside (§7.5): Close cancels a side question's
// request and returns only once the side question has: it is joined before
// the transcript closes.
func TestCloseCancelsAndJoinsAnAside(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	joining, closed := make(chan struct{}), make(chan struct{})
	s.sidesJoin = func() { close(joining) }
	var order, stepAt, closeAt atomic.Int32
	reached, cancelled := make(chan struct{}), make(chan struct{})
	f.models["test/a"].push(func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		for _, p := range openText("partial") {
			if !yield(p) {
				return
			}
		}
		close(reached)
		<-ctx.Done()
		close(cancelled)
		// On until Close is joining the side questions — or, were it not
		// to, has returned without them.
		select {
		case <-joining:
		case <-closed:
		}
		stepAt.Store(order.Add(1))
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
	})
	side := startAside(context.Background(), s, nil, "q")
	await(t, reached, "the side question's request")
	closeErr := make(chan error, 1)
	go func() {
		err := s.Close()
		closeAt.Store(order.Add(1))
		close(closed)
		closeErr <- err
	}()
	await(t, cancelled, "Close's cancel of the side question")
	if err := await(t, closeErr, "Close"); err != nil {
		t.Fatal(err)
	}
	o := await(t, side, "the side question")
	if !errors.Is(o.err, ErrClosed) {
		t.Fatalf("the cancelled side question answered %v, want ErrClosed", o.err)
	}
	if stepAt.Load() > closeAt.Load() {
		t.Fatalf("Close returned (%d) before the side question it cancelled had finished (%d)", closeAt.Load(), stepAt.Load())
	}
}

// TestAsideWritesNothing (§7.5): a side question — idle on a new session,
// idle after a turn, and mid-turn — writes nothing to the transcript.
func TestAsideWritesNothing(t *testing.T) {
	f := newFixture(t, "http://unused")
	s := f.open(f.options())
	a := f.models["test/a"]
	a.push(answerWith("SIDE-ANSWER-ONE"))
	if o := ask(s, nil, "SIDE-QUESTION-ONE"); o.err != nil {
		t.Fatal(o.err)
	}
	noTranscript(t, s)

	a.push(answerWith("hi"), answerWith("SIDE-ANSWER-TWO"))
	run(t, s, "hello")
	before := entries(transcript(t, s))
	if o := ask(s, nil, "SIDE-QUESTION-TWO"); o.err != nil {
		t.Fatal(o.err)
	}
	equal(t, "the entries after an idle side question", entries(transcript(t, s)), before)

	g := newGate()
	a.push(g.hold(openText("work"), finishText()), answerWith("SIDE-ANSWER-THREE"))
	turn := start(context.Background(), s, "more", nil)
	await(t, g.reached, "the turn's step")
	if o := ask(s, []AsidePair{{"SIDE-QUESTION-TWO", "SIDE-ANSWER-TWO"}}, "SIDE-QUESTION-THREE"); o.err != nil {
		t.Fatal(o.err)
	}
	close(g.release)
	await(t, turn, "the turn")
	got := entries(transcript(t, s))
	equal(t, "the entries after a mid-turn side question", got[:len(before)], before)
	equal(t, "the turn's own entries", got[len(before):], []string{"user test/a high: more", "assistant test/a high end_turn: work"})
	raw, err := os.ReadFile(s.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc([]string{"SIDE-QUESTION", "SIDE-ANSWER", "side question"}, func(n string) bool {
		return strings.Contains(string(raw), n)
	}); i >= 0 {
		t.Fatal("the transcript holds a side question's text")
	}
	s.mu.Lock()
	left := len(s.sides)
	s.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d side questions are still registered", left)
	}
}
