package store

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

// heldTexts is every user message's first text part in msgs, in order, with
// its mark beside it.
func heldTexts(msgs []fantasy.Message, marks []bool) []string {
	var out []string
	for i, m := range msgs {
		if m.Role != fantasy.MessageRoleUser || len(m.Content) == 0 {
			continue
		}
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](m.Content[0]); ok {
			out = append(out, fmt.Sprintf("%s:%v", tp.Text, marks[i]))
		}
	}
	return out
}

// TestContextWithHeldTakesOnlyItsTurnsHeldEntries (plan 040 §3.4 step 4): the
// written context, then the held user entries whose Turn is the one asked
// for — a results entry marked as one — and never another turn's held entry,
// nor any for turn 0.
func TestContextWithHeldTakesOnlyItsTurnsHeldEntries(t *testing.T) {
	s := newStore(t, testOptions(t))
	first := user("one", kimi)
	first.Turn = 1
	if err := s.AppendUser(first); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAssistant(answer("", "ok", kimi)); err != nil {
		t.Fatal(err)
	}
	// Turn 2's prompt and a results entry of its own, held; and a held entry
	// of turn 3, which a turn 2 caller must never see.
	prompt := user("two", kimi)
	prompt.Turn = 2
	results := user("results", kimi)
	results.Turn, results.SubagentResults = 2, true
	other := user("three", kimi)
	other.Turn = 3
	for _, e := range []MessageEntry{prompt, results, other} {
		if err := s.AppendUser(e); err != nil {
			t.Fatal(err)
		}
	}

	written, writtenMarks := s.ContextWithResults(kimi)
	for _, tc := range []struct {
		turn int
		want []string
	}{
		{0, []string{"one:false"}},
		{2, []string{"one:false", "two:false", "results:true"}},
		{3, []string{"one:false", "three:false"}},
		{4, []string{"one:false"}},
	} {
		msgs, marks := s.ContextWithHeld(kimi, tc.turn)
		if got := heldTexts(msgs, marks); fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("ContextWithHeld(turn %d)'s user messages = %v, want %v", tc.turn, got, tc.want)
		}
		if len(msgs) < len(written) {
			t.Fatalf("ContextWithHeld(turn %d) has %d messages, fewer than the written context's %d", tc.turn, len(msgs), len(written))
		}
		for i := range written {
			if msgs[i].Role != written[i].Role || marks[i] != writtenMarks[i] {
				t.Fatalf("ContextWithHeld(turn %d) message %d differs from the written context's", tc.turn, i)
			}
		}
	}
	// The read changed nothing: the held entries are all still held, and
	// written with the next step.
	if _, err := s.AppendStep(nil, answer("", "done", kimi), nil); err != nil {
		t.Fatal(err)
	}
	if got := heldTexts(s.ContextWithHeld(kimi, 0)); len(got) != 4 {
		t.Fatalf("after the step the written context's user messages are %v, want all four", got)
	}
}

// TestContextWithHeldAgainstAConcurrentStep (plan 040 §3.4 step 4): a side
// question reading turn k's context while the turn's first step is appended
// sees turn k's prompt exactly once — held before the append, written after
// it, never neither and never both — because the written context and the
// held entries are read under one lock. A reader that took them in two
// sections could land between an append's write and its letting go of the
// held entries, and see the prompt in neither.
func TestContextWithHeldAgainstAConcurrentStep(t *testing.T) {
	s := newStore(t, testOptions(t))
	const turns = 300
	var current atomic.Int64 // the turn whose prompt is held or written, 0 before the first
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for k := 1; k <= turns; k++ {
			e := user(fmt.Sprintf("prompt %d", k), kimi)
			e.Turn = k
			if err := s.AppendUser(e); err != nil {
				t.Error(err)
				return
			}
			current.Store(int64(k))
			if _, err := s.AppendStepLed(nil, answer("", fmt.Sprintf("answer %d", k), kimi), nil); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	readers := 4
	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				k := int(current.Load())
				if k == 0 {
					continue
				}
				msgs, _ := s.ContextWithHeld(kimi, k)
				want := fmt.Sprintf("prompt %d", k)
				n := 0
				for _, m := range msgs {
					if m.Role != fantasy.MessageRoleUser {
						continue
					}
					if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](m.Content[0]); ok && tp.Text == want {
						n++
					}
				}
				if n != 1 {
					t.Errorf("turn %d's context holds its prompt %d times, want exactly once", k, n)
					return
				}
			}
		}()
	}
	wg.Wait()
}
