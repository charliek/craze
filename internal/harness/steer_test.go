package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/store"
)

// Interject (plan 019 §3.10, §7.12): Steer merges a user message into the
// running turn, the turn takes it up before its next step, and the step that
// saw it writes it down. What no step persisted comes back in
// Result.Unanswered.

// steerText is the interjection every test here sends, distinctive enough to
// find in a request, a transcript line or an event.
const steerText = "also look at b.txt"

// unanswered is a Result's steers as one line, for an assertion message.
func unansweredOf(res Result) string { return strings.Join(res.Unanswered, " | ") }

// requestLine is request n's message at index i, "user: …" style, or a note
// saying there is no such message.
func requestLine(t *testing.T, m *scripted, n, i int) string {
	t.Helper()
	calls := m.requests()
	if n >= len(calls) {
		t.Fatalf("the model saw %d requests, want at least %d", len(calls), n+1)
	}
	lines := promptOf(calls[n])
	if i >= len(lines) {
		return fmt.Sprintf("<request %d has only %d messages>", n, len(lines))
	}
	return lines[i]
}

// TestSteerDuringAToolStep is the main path (§7.12): a steer accepted while
// the turn's first tool step streams is taken up before the second, read by
// the model there and at every step after it — at the same index each time —
// written by the step that first saw it, and replayed at that same index next
// turn. It is reported once, by the turn, before the step that took it up
// finishes, and it is not unanswered.
//
// The control is the same script with no steer: its requests carry no user
// message past the prompt, its transcript has none, and the message that sits
// at the steer's index is the next step's answer instead.
func TestSteerDuringAToolStep(t *testing.T) {
	// The index the steer lands at: the system prompt, the turn's prompt, and
	// the first tool step's assistant message and results are ahead of it.
	const at = 4
	for _, steer := range []bool{true, false} {
		t.Run(fmt.Sprintf("steer=%v", steer), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			f.put("a.txt", "alpha\n")
			readIn := input(t, map[string]any{"filePath": "a.txt"})
			a := f.models["test/a"]
			g := newGate()
			a.push(
				g.hold(callParts("c1", "read", readIn), finish(fantasy.FinishReasonToolCalls)),
				callStep(callParts("c2", "read", readIn)),
				answerWith("done"),
				answerWith("welcome"), // the next turn, which replays it all
			)

			var ev events
			out := start(context.Background(), s, "what is in a.txt?", ev.sink)
			await(t, g.reached, "the first step to reach its gate")
			if steer {
				if err := sendSteer(s, steerText); err != nil {
					t.Fatalf("Steer during the first step: %v", err)
				}
			}
			close(g.release)
			got := await(t, out, "the turn to end")
			if got.err != nil || got.res.StopReason != StopEndTurn {
				t.Fatalf("Run = %+v, %v; want end_turn", got.res, got.err)
			}
			if len(got.res.Unanswered) != 0 {
				t.Fatalf("Unanswered = %q; a steer a step persisted is answered", unansweredOf(got.res))
			}
			run(t, s, "thanks")

			// The first request went out before the steer was accepted, so it
			// never carries one; every request after it does, at one index —
			// the second and third steps of the turn, and the replay of the
			// turn after it.
			sizes := []int{2, 4, 6, 8} // the messages each request carried
			if steer {
				sizes = []int{2, 5, 7, 9} // one more, from the second on
				for _, n := range []int{1, 2, 3} {
					if line := requestLine(t, a, n, at); line != "user: "+steerText {
						t.Errorf("request %d's message %d is %q, want the steer", n+1, at, line)
					}
				}
			}
			for n, size := range sizes {
				if got := len(promptOf(a.requests()[n])); got != size {
					t.Errorf("request %d carried %d messages, want %d", n+1, got, size)
				}
			}

			lines := entries(transcript(t, s))
			steerLine := "user test/a high: " + steerText
			if !steer {
				// The control: nothing anywhere is the steer — no request, no
				// transcript line, no event — and every request is one message
				// shorter from the second on.
				if len(lines) != 8 {
					t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
				}
				for i, c := range a.requests() {
					if text := strings.Join(promptOf(c), "\n"); strings.Contains(text, steerText) {
						t.Fatalf("request %d carries the steer with no Steer called:\n%s", i+1, text)
					}
				}
				if evs := of[Steered](ev.list()); len(evs) != 0 {
					t.Fatalf("Steered events with no Steer called: %+v", evs)
				}
				if slicesContains(lines, steerLine) {
					t.Fatalf("the control's transcript holds a steer:\n%s", strings.Join(lines, "\n"))
				}
				return
			}
			if len(lines) != 9 || lines[3] != steerLine {
				t.Fatalf("transcript line 3 is not the steer:\n%s", strings.Join(lines, "\n"))
			}
			// Reported once, by the turn, before the step that took it up
			// finished — and so before anything that ends the turn.
			order := seq(ev.list())
			steerAt, stepTwo := indexOf(order, "steered "+steerText), indexOf(order, "step 2 tool-calls tool_use saved=true")
			if steerAt < 0 || stepTwo < 0 || steerAt > stepTwo {
				t.Fatalf("the steer was not reported before step 2 finished:\n%s", strings.Join(order, "\n"))
			}
			if n := strings.Count(strings.Join(order, "\n"), "steered "); n != 1 {
				t.Errorf("the steer was reported %d times, want once:\n%s", n, strings.Join(order, "\n"))
			}
		})
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

func slicesContains(ss []string, want string) bool { return indexOf(ss, want) >= 0 }

// TestSteerWithNoTurn: Steer is ErrNotInTurn before a turn, after one, and
// after Close, and ErrEmptyPrompt for whitespace; none of them writes
// anything. The control is the same call during a turn, which is accepted.
func TestSteerWithNoTurn(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	g := newGate()
	f.models["test/a"].push(g.hold(openText("thinking it over"), finishText()), answerWith("done"))

	if err := sendSteer(s, steerText); !errors.Is(err, ErrNotInTurn) {
		t.Fatalf("Steer while idle = %v, want ErrNotInTurn", err)
	}
	var ev events
	out := start(context.Background(), s, "go", ev.sink)
	await(t, g.reached, "the step to reach its gate")
	// The control: the same text, the same session, a turn running.
	if err := sendSteer(s, steerText); err != nil {
		t.Fatalf("control: Steer during a turn = %v, want it accepted", err)
	}
	if err := sendSteer(s, "   \n\t "); !errors.Is(err, ErrEmptyPrompt) {
		t.Fatalf("Steer of whitespace = %v, want ErrEmptyPrompt", err)
	}
	close(g.release)
	got := await(t, out, "the turn to end")
	if got.err != nil || got.res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v", got.res, got.err)
	}

	if err := sendSteer(s, steerText); !errors.Is(err, ErrNotInTurn) {
		t.Fatalf("Steer after the turn = %v, want ErrNotInTurn", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sendSteer(s, steerText); !errors.Is(err, ErrNotInTurn) {
		t.Fatalf("Steer after Close = %v, want ErrNotInTurn", err)
	}
	// The accepted one had no later step to reach, so it is unanswered and
	// unwritten — and the two refusals left nothing behind either.
	if len(got.res.Unanswered) != 1 || got.res.Unanswered[0] != steerText {
		t.Fatalf("Unanswered = %q, want the one accepted steer", unansweredOf(got.res))
	}
	for _, l := range entries(transcript(t, s)) {
		if strings.Contains(l, steerText) {
			t.Fatalf("the transcript holds a steer no step took up: %q", l)
		}
	}
}

// TestSteerTokenNamesItsTurn: the token names the turn the text was typed
// into, so a caller that read it while one turn was live — and lost its own
// lock while that turn ended and the next began — is refused rather than
// having the next turn take the text up. This is the two halves of the
// adapter's Interject held apart: it reads the token under its lock, releases
// it, and only then calls Steer.
//
// The control is the same token used inside the turn it names, which is
// accepted; and the turn after it neither reports, sends nor writes the text.
func TestSteerTokenNamesItsTurn(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	first, second := newGate(), newGate()
	f.models["test/a"].push(
		first.hold(openText("one"), finishText()),
		second.hold(openText("two"), finishText()),
	)

	if tok := s.SteerToken(); tok != 0 {
		t.Fatalf("SteerToken while idle = %d, want 0", tok)
	}
	var ev events
	out := start(context.Background(), s, "first", ev.sink)
	await(t, first.reached, "the first turn's gate")
	tok := s.SteerToken()
	if tok == 0 {
		t.Fatal("SteerToken during a turn is 0")
	}
	// The control: the same token, used while the turn it names is live.
	if err := s.Steer(tok, "into the first turn"); err != nil {
		t.Fatalf("control: Steer with the live turn's token = %v", err)
	}
	close(first.release)
	got := await(t, out, "the first turn")
	if got.err != nil || len(got.res.Unanswered) != 1 {
		t.Fatalf("the first turn = %+v, %v; want the control steer back", got.res, got.err)
	}

	// The second turn opens, and the token the caller is still holding is now
	// stale: the text was typed into a turn that has ended.
	out = start(context.Background(), s, "second", ev.sink)
	await(t, second.reached, "the second turn's gate")
	if next := s.SteerToken(); next == tok || next == 0 {
		t.Fatalf("the second turn's token is %d, the first's was %d; want a new one", next, tok)
	}
	if err := s.Steer(tok, steerText); !errors.Is(err, ErrNotInTurn) {
		t.Fatalf("Steer with the previous turn's token = %v, want ErrNotInTurn", err)
	}
	close(second.release)
	got = await(t, out, "the second turn")
	if got.err != nil || len(got.res.Unanswered) != 0 {
		t.Fatalf("the second turn = %+v, %v; want nothing of the stale steer", got.res, got.err)
	}
	for _, e := range of[Steered](ev.list()) {
		if e.Text == steerText {
			t.Fatal("the stale steer was reported to the sink")
		}
	}
	for _, c := range f.models["test/a"].requests() {
		if strings.Contains(strings.Join(promptOf(c), "\n"), steerText) {
			t.Fatal("the stale steer reached the model")
		}
	}
	for _, l := range entries(transcript(t, s)) {
		if strings.Contains(l, steerText) {
			t.Fatalf("the stale steer was written: %q", l)
		}
	}
}

// TestSteerLimit: a turn takes steerCap interjections and refuses the next
// with ErrTooManySteers, having changed nothing — so every turn's unanswered
// list, and with it the adapter's requeue, is bounded. The control is the
// steerCap'th, which is accepted, and the next turn, which starts a fresh
// budget.
func TestSteerLimit(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	g, next := newGate(), newGate()
	f.models["test/a"].push(g.hold(openText("one"), finishText()), next.hold(openText("two"), finishText()))

	out := start(context.Background(), s, "go", nil)
	await(t, g.reached, "the gate")
	for i := range steerCap {
		// The control is the last of these: the cap is reached, not passed.
		if err := sendSteer(s, fmt.Sprintf("steer %02d", i)); err != nil {
			t.Fatalf("steer %d of %d = %v, want it accepted", i+1, steerCap, err)
		}
	}
	if err := sendSteer(s, steerText); !errors.Is(err, ErrTooManySteers) {
		t.Fatalf("steer %d = %v, want ErrTooManySteers", steerCap+1, err)
	}
	close(g.release)
	got := await(t, out, "the turn")
	if got.err != nil {
		t.Fatal(got.err)
	}
	if len(got.res.Unanswered) != steerCap {
		t.Fatalf("Unanswered holds %d, want exactly the %d accepted", len(got.res.Unanswered), steerCap)
	}
	for _, text := range got.res.Unanswered {
		if text == steerText {
			t.Fatal("the refused steer was kept")
		}
	}
	// The next turn starts with a fresh budget.
	out = start(context.Background(), s, "again", nil)
	await(t, next.reached, "the second turn's gate")
	if err := sendSteer(s, steerText); err != nil {
		t.Fatalf("control: the next turn's first steer = %v, want it accepted", err)
	}
	close(next.release)
	if got := await(t, out, "the second turn"); got.err != nil {
		t.Fatal(got.err)
	}
}

// TestSteerDuringTheFinalStep (§7.12): a steer accepted while the turn's last
// step streams has no step left to take it up. It is still reported, before
// the turn ends, and comes back unanswered rather than being written; several
// come back in the order they were accepted. The control is the same steers
// accepted one step earlier, which a step does take up and write.
func TestSteerDuringTheFinalStep(t *testing.T) {
	texts := []string{steerText, "and c.txt", "and d.txt"}
	for _, final := range []bool{true, false} {
		t.Run(fmt.Sprintf("final=%v", final), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			f.put("a.txt", "alpha\n")
			readIn := input(t, map[string]any{"filePath": "a.txt"})
			g := newGate()
			first, last := callStep(callParts("c1", "read", readIn)), g.hold(openText("all done"), finishText())
			if !final {
				// The control gates the tool step instead, so the same steers
				// land one step earlier, with a step still to come.
				first, last = g.hold(callParts("c1", "read", readIn), finish(fantasy.FinishReasonToolCalls)), answerWith("all done")
			}
			f.models["test/a"].push(first, last)

			var ev events
			out := start(context.Background(), s, "read it", ev.sink)
			await(t, g.reached, "the gated step")
			for _, text := range texts {
				if err := sendSteer(s, text); err != nil {
					t.Fatalf("Steer(%q): %v", text, err)
				}
			}
			close(g.release)
			got := await(t, out, "the turn to end")
			if got.err != nil || got.res.StopReason != StopEndTurn {
				t.Fatalf("Run = %+v, %v", got.res, got.err)
			}

			// Every accepted steer is reported exactly once, whichever step it
			// reached, and every report is before Run returned.
			var reported []string
			for _, e := range of[Steered](ev.list()) {
				reported = append(reported, e.Text)
			}
			equal(t, "the steers reported", reported, texts)

			lines := entries(transcript(t, s))
			if final {
				equal(t, "unanswered", got.res.Unanswered, texts)
				for _, l := range lines {
					for _, text := range texts {
						if strings.Contains(l, text) {
							t.Fatalf("a steer no step took up was written: %q", l)
						}
					}
				}
				return
			}
			// The control: a step did take them up, so they are written, in
			// order, ahead of that step's answer, and none is unanswered.
			if len(got.res.Unanswered) != 0 {
				t.Fatalf("control: Unanswered = %q, want none", unansweredOf(got.res))
			}
			var want []string
			for _, text := range texts {
				want = append(want, "user test/a high: "+text)
			}
			if len(lines) < 3+len(want) {
				t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
			}
			equal(t, "the steers in the transcript", lines[3:3+len(want)], want)
		})
	}
}

// TestSteerIntoAStepThatNeverPersists (§7.12): a steer taken into a step whose
// stream then fails, and one taken into a step whose append fails, are
// unanswered — not lost and not written — and the turn still reports its
// failure. The control is the same script with nothing failing: the step
// persists and the steer is written instead.
func TestSteerIntoAStepThatNeverPersists(t *testing.T) {
	cases := []struct {
		name    string
		failure func(t *testing.T, s *Session, ev *events) func(Event)
		wantErr func(error) bool
	}{
		{"the stream fails", nil, func(err error) bool {
			var pe *ProviderError
			return errors.As(err, &pe)
		}},
		{"the append fails", func(t *testing.T, s *Session, ev *events) func(Event) {
			return func(e Event) {
				ev.sink(e)
				if d, ok := e.(StepDone); ok && d.Step == 1 {
					_ = s.store.Close() // the next step's append will fail
				}
			}
		}, func(err error) bool { return errors.Is(err, store.ErrClosed) }},
		{"control: nothing fails", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			f.put("a.txt", "alpha\n")
			readIn := input(t, map[string]any{"filePath": "a.txt"})
			g := newGate()
			second := answerWith("done")
			if c.name == "the stream fails" {
				second = reply(errorPart(errors.New("upstream gone")))
			}
			f.models["test/a"].push(g.hold(callParts("c1", "read", readIn), finish(fantasy.FinishReasonToolCalls)), second)

			var ev events
			sink := ev.sink
			if c.failure != nil {
				sink = c.failure(t, s, &ev)
			}
			out := start(context.Background(), s, "read it", sink)
			await(t, g.reached, "the tool step's gate")
			if err := sendSteer(s, steerText); err != nil {
				t.Fatalf("Steer: %v", err)
			}
			close(g.release)
			got := await(t, out, "the turn to end")

			if c.wantErr == nil {
				if got.err != nil || len(got.res.Unanswered) != 0 {
					t.Fatalf("control: Run = %+v, %v; want a clean turn with nothing unanswered", got.res, got.err)
				}
				if l := entries(transcript(t, s))[3]; l != "user test/a high: "+steerText {
					t.Fatalf("control: transcript line 3 is %q, want the steer", l)
				}
				return
			}
			if got.err == nil || !c.wantErr(got.err) {
				t.Fatalf("Run's error = %v; not the failure this case arranged", got.err)
			}
			if len(got.res.Unanswered) != 1 || got.res.Unanswered[0] != steerText {
				t.Fatalf("Unanswered = %q, want the steer the failed step took up", unansweredOf(got.res))
			}
			// The model did read it: it was in the request the step sent.
			if line := requestLine(t, f.models["test/a"], 1, 4); line != "user: "+steerText {
				t.Fatalf("the failed step's request message 4 is %q, want the steer", line)
			}
			if c.name == "the append fails" {
				// The store is closed, so nothing more can be read from it;
				// what matters is that the steer came back instead.
				return
			}
			for _, l := range entries(transcript(t, s)) {
				if strings.Contains(l, steerText) {
					t.Fatalf("a steer whose step never persisted was written: %q", l)
				}
			}
		})
	}
}

// TestSteerOnACancelledTurn: a steer accepted during a step that persists,
// where the caller then cancels, has no step left to take it up — a cancelled
// turn makes no further request — so it is never sent and never written, and
// comes back unanswered. The control is the same steer on the same script
// without the cancel, which the next step takes up and writes.
func TestSteerOnACancelledTurn(t *testing.T) {
	for _, cancelled := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancelled=%v", cancelled), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			f.put("a.txt", "alpha\n")
			readIn := input(t, map[string]any{"filePath": "a.txt"})
			g := newGate()
			f.models["test/a"].push(g.hold(callParts("c1", "read", readIn), finish(fantasy.FinishReasonToolCalls)), answerWith("done"))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var ev events
			// The cancel lands once the tool step is safely persisted, which
			// is the case worth pinning: the steer was accepted into a turn
			// that then had nowhere to put it.
			out := start(ctx, s, "read it", func(e Event) {
				ev.sink(e)
				if d, ok := e.(StepDone); ok && d.Step == 1 && cancelled {
					cancel()
				}
			})
			await(t, g.reached, "the tool step's gate")
			if err := sendSteer(s, steerText); err != nil {
				t.Fatalf("Steer: %v", err)
			}
			close(g.release)
			got := await(t, out, "the turn to end")

			want := StopEndTurn
			if cancelled {
				want = StopCancelled
			}
			if got.err != nil || got.res.StopReason != want {
				t.Fatalf("Run = %+v, %v; want %s", got.res, got.err, want)
			}
			lines := entries(transcript(t, s))
			if !cancelled {
				if len(got.res.Unanswered) != 0 {
					t.Fatalf("control: Unanswered = %q, want none", unansweredOf(got.res))
				}
				if len(lines) != 5 || lines[3] != "user test/a high: "+steerText {
					t.Fatalf("control: the next step did not write the steer:\n%s", strings.Join(lines, "\n"))
				}
				return
			}
			if len(got.res.Unanswered) != 1 || got.res.Unanswered[0] != steerText {
				t.Fatalf("Unanswered = %q, want the steer back", unansweredOf(got.res))
			}
			if n := len(f.models["test/a"].requests()); n != 1 {
				t.Fatalf("%d requests, want 1: a cancelled turn asks nothing more", n)
			}
			if len(lines) != 3 {
				t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
			}
			for _, l := range lines {
				if strings.Contains(l, steerText) {
					t.Fatalf("a cancelled turn wrote a steer: %q", l)
				}
			}
		})
	}
}

// TestSteerRacesTheTurnEnd (§7.12, run under -race): steers arrive from many
// goroutines at the two moments that can interleave — one batch racing the
// drain before a step, one racing the turn's own ending. Every steer Steer
// accepted is accounted for exactly once, written to the transcript or
// returned unanswered, never both and never neither; every one it refused left
// nothing at all; and every accepted one reached the sink before Run returned.
// The control is the accounting itself, which fails on a steer in two places
// or in none, plus the requirement that the second batch be accepted at all.
func TestSteerRacesTheTurnEnd(t *testing.T) {
	const steerers = 8
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.put("a.txt", "alpha\n")
	readIn := input(t, map[string]any{"filePath": "a.txt"})
	tool, answer := newGate(), newGate()
	f.models["test/a"].push(
		tool.hold(callParts("c1", "read", readIn), finish(fantasy.FinishReasonToolCalls)),
		answer.hold(openText("all done"), finishText()),
	)

	var (
		mu       sync.Mutex
		accepted []string
		reported []string
		returned bool
		late     int
	)
	sink := func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		if st, ok := e.(Steered); ok {
			if returned {
				late++
			}
			reported = append(reported, st.Text)
		}
	}
	// storm launches a batch of steerers and, when it has a gate, lets the
	// turn go on at the same moment, so the accepts and whatever the turn does
	// next interleave. A batch with no gate is launched into a turn already on
	// its way out: those steers race the close itself, and each is accepted or
	// refused, never half of either.
	var wg sync.WaitGroup
	storm := func(batch string, g *gate) {
		for i := range steerers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				text := fmt.Sprintf("steer %s%02d", batch, i)
				switch err := sendSteer(s, text); {
				case err == nil:
					mu.Lock()
					accepted = append(accepted, text)
					mu.Unlock()
				case errors.Is(err, ErrNotInTurn):
				default:
					t.Errorf("Steer(%q) = %v, want nil or ErrNotInTurn", text, err)
				}
			}()
		}
		if g != nil {
			close(g.release)
		}
	}

	out := start(context.Background(), s, "read it", sink)
	await(t, tool.reached, "the tool step's gate")
	storm("a", tool) // races the drain before the answer step
	await(t, answer.reached, "the answer step's gate")
	storm("b", answer) // races the turn's own ending
	storm("c", nil)    // races the close, with the turn already ending
	got := await(t, out, "the turn to end")
	mu.Lock()
	returned = true
	mu.Unlock()
	wg.Wait()
	if got.err != nil {
		t.Fatalf("Run: %v", got.err)
	}

	mu.Lock()
	defer mu.Unlock()
	if late != 0 {
		t.Errorf("%d steers reached the sink after Run returned", late)
	}
	// Each accepted steer is in exactly one of the two places, and neither
	// place holds anything that was not accepted.
	seen := map[string]int{}
	for _, l := range entries(transcript(t, s)) {
		if text, ok := strings.CutPrefix(l, "user test/a high: "); ok && strings.HasPrefix(text, "steer ") {
			seen[text]++
		}
	}
	for _, text := range got.res.Unanswered {
		seen[text]++
	}
	batches := map[string]int{}
	for _, text := range accepted {
		if seen[text] != 1 {
			t.Errorf("%q is accounted for %d times, want once (written or unanswered)", text, seen[text])
		}
		delete(seen, text)
		batches[text[6:7]]++
	}
	if len(seen) != 0 {
		t.Errorf("text nobody accepted turned up: %v", seen)
	}
	if len(reported) != len(accepted) {
		t.Errorf("%d steers reported, %d accepted", len(reported), len(accepted))
	}
	if batches["a"] == 0 || batches["b"] == 0 {
		t.Fatalf("control: the batches were accepted %v, want both of them racing something", batches)
	}
}
