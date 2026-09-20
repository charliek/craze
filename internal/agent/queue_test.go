package agent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestQueueOrderAndPositions(t *testing.T) {
	var q PromptQueue
	now := time.Now()
	var evs []QueueEvent
	for _, text := range []string{"a", "b", "c"} {
		_, ev, err := q.Add(text, now)
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	for i, ev := range evs {
		if ev.Pos != i || ev.Change != QueueQueued {
			t.Fatalf("event %d: %+v", i, ev)
		}
	}
	got := q.List()
	if len(got) != 3 || got[0].Text != "a" || got[2].Text != "c" {
		t.Fatalf("queue %+v", got)
	}
	if got[0].ID == got[1].ID {
		t.Fatal("ids must be distinct")
	}
}

func TestQueueEditKeepsIDAndPositionAndBumpsVersion(t *testing.T) {
	var q PromptQueue
	now := time.Now()
	_, _, _ = q.Add("a", now)
	second, _, _ := q.Add("b", now)
	_, _, _ = q.Add("c", now)
	ev, err := q.Edit(second.ID, "B!")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Change != QueueEdited || ev.Pos != 1 || ev.Prompt.ID != second.ID || ev.Prompt.Version != 1 {
		t.Fatalf("edit %+v", ev)
	}
	got := q.List()
	if got[1].Text != "B!" || got[0].Text != "a" || got[2].Text != "c" {
		t.Fatalf("queue %+v", got)
	}
	if _, err := q.Edit("nope", "x"); err == nil {
		t.Fatal("editing an unknown id must fail")
	}
}

func TestQueueRefusesA33rdEntryWithoutMutating(t *testing.T) {
	var q PromptQueue
	now := time.Now()
	for i := 0; i < queueCap; i++ {
		if _, _, err := q.Add("x", now); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
	if _, _, err := q.Add("one too many", now); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err %v", err)
	}
	if q.Len() != queueCap {
		t.Fatalf("len %d", q.Len())
	}
	for _, p := range q.List() {
		if p.Text != "x" {
			t.Fatalf("the refused text got in: %+v", p)
		}
	}
}

func TestQueueRefusesAnOversizedMessageWithoutTruncating(t *testing.T) {
	var q PromptQueue
	big := strings.Repeat("x", queueTextCap+1)
	if _, _, err := q.Add(big, time.Now()); !errors.Is(err, ErrQueueTextTooLong) {
		t.Fatalf("err %v", err)
	}
	if q.Len() != 0 {
		t.Fatalf("len %d: an oversized message must not be stored", q.Len())
	}
	// Exactly at the cap is allowed: the bound is a refusal, not a margin.
	if _, _, err := q.Add(strings.Repeat("x", queueTextCap), time.Now()); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
	// An edit is bounded the same way, and leaves the row alone.
	p := q.List()[0]
	if _, err := q.Edit(p.ID, big); !errors.Is(err, ErrQueueTextTooLong) {
		t.Fatalf("edit err %v", err)
	}
	if got := q.List()[0]; got.Version != 0 || len(got.Text) != queueTextCap {
		t.Fatalf("row changed: version %d len %d", got.Version, len(got.Text))
	}
}

func TestQueueTakeOfANonHeadRowKeepsTheOthers(t *testing.T) {
	var q PromptQueue
	now := time.Now()
	_, _, _ = q.Add("a", now)
	second, _, _ := q.Add("b", now)
	_, _, _ = q.Add("c", now)
	ev, ok := q.Take(second.ID)
	if !ok || ev.Change != QueueSent || ev.Pos != 1 || ev.Prompt.Text != "b" {
		t.Fatalf("take %+v %v", ev, ok)
	}
	got := q.List()
	if len(got) != 2 || got[0].Text != "a" || got[1].Text != "c" {
		t.Fatalf("queue %+v", got)
	}
	if _, ok := q.Take("nope"); ok {
		t.Fatal("taking an unknown id must fail")
	}
}

func TestQueuePopAndClear(t *testing.T) {
	var q PromptQueue
	now := time.Now()
	for _, text := range []string{"a", "b", "c"} {
		_, _, _ = q.Add(text, now)
	}
	ev, ok := q.Pop()
	if !ok || ev.Prompt.Text != "a" || ev.Change != QueueSent || ev.Pos != 0 {
		t.Fatalf("pop %+v %v", ev, ok)
	}
	evs := q.Clear()
	if len(evs) != 2 || evs[0].Prompt.Text != "b" || evs[1].Prompt.Text != "c" {
		t.Fatalf("clear %+v", evs)
	}
	for _, e := range evs {
		if e.Change != QueueRemoved || e.Pos != 0 {
			t.Fatalf("clear event %+v: each row was the head when it went", e)
		}
	}
	if q.Len() != 0 {
		t.Fatalf("len %d", q.Len())
	}
	if _, ok := q.Pop(); ok {
		t.Fatal("an empty queue pops nothing")
	}
	if evs := q.Clear(); len(evs) != 0 {
		t.Fatalf("clearing an empty queue: %+v", evs)
	}
}

func TestQueueListIsACloneInSendOrder(t *testing.T) {
	var q PromptQueue
	_, _, _ = q.Add("a", time.Now())
	got := q.List()
	got[0].Text = "mutated"
	if q.List()[0].Text != "a" {
		t.Fatal("List must clone")
	}
}

// TestQueuePushFrontIsExemptFromBothCaps: PushFront puts a row at the head of
// a queue that is already full, with text far over the size cap, and gives it
// an ordinary id, the position it landed at, and an ordinary row's behaviour —
// Edit, Remove and Pop all reach it. The control is Add, which refuses both.
func TestQueuePushFrontIsExemptFromBothCaps(t *testing.T) {
	var q PromptQueue
	now := time.Now()
	for i := range queueCap {
		if _, _, err := q.Add(strings.Repeat("a", i+1), now); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	huge := strings.Repeat("x", queueTextCap+1)
	// The control: neither cap lets Add through.
	if _, _, err := q.Add("one more", now); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("control: Add on a full queue = %v, want ErrQueueFull", err)
	}
	if _, _, err := q.Add(huge, now); !errors.Is(err, ErrQueueTextTooLong) && !errors.Is(err, ErrQueueFull) {
		t.Fatalf("control: Add of oversized text = %v, want a refusal", err)
	}

	ev := q.PushFront(huge, now)
	if ev.Change != QueueQueued || ev.Pos != 0 || ev.Prompt.Text != huge || ev.Prompt.ID == "" {
		t.Fatalf("PushFront event %+v", ev)
	}
	second := q.PushFront("ahead of it", now)
	if q.Len() != queueCap+2 {
		t.Fatalf("the queue holds %d rows, want %d", q.Len(), queueCap+2)
	}
	list := q.List()
	if list[0].Text != "ahead of it" || list[1].Text != huge || list[2].Text != "a" {
		t.Fatalf("queue order %q, %q, %q", list[0].Text, list[1].Text, list[2].Text)
	}
	if second.Prompt.ID == ev.Prompt.ID {
		t.Fatal("PushFront must mint a distinct id")
	}
	// From the head it is an ordinary row.
	if _, err := q.Edit(ev.Prompt.ID, "edited"); err != nil {
		t.Fatalf("Edit of a pushed row: %v", err)
	}
	if _, ok := q.Remove(second.Prompt.ID); !ok {
		t.Fatal("Remove of a pushed row")
	}
	if popped, ok := q.Pop(); !ok || popped.Prompt.Text != "edited" {
		t.Fatalf("Pop = %+v, %v; want the edited pushed row", popped, ok)
	}
}

func TestQueueEventCarriesItsPosition(t *testing.T) {
	ev := QueueEvent{Prompt: QueuedPrompt{ID: "q-2", Text: "hi", Version: 3}, Change: QueueEdited, Pos: 1}
	got := ev.Event()
	if got.Type != EventQueue || got.QueueChange != QueueEdited || got.QueuePos != 1 {
		t.Fatalf("event %+v", got)
	}
	if got.Queue == nil || got.Queue.ID != "q-2" || got.Queue.Version != 3 {
		t.Fatalf("queue %+v", got.Queue)
	}
}

// TestQueuePopIsOneTransaction: reading the head and removing it separately
// could report an empty queue because the head went, with rows still behind
// it — which a drain reads as "nothing left".
func TestQueuePopIsOneTransaction(t *testing.T) {
	var q PromptQueue
	now := time.Now()
	for i := 0; i < 200; i++ {
		_, _, _ = q.Add("x", now)
		_, _, _ = q.Add("y", now)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = q.Pop()
		}()
		_, _ = q.Pop()
		<-done
		if q.Len() != 0 {
			t.Fatalf("two pops left %d rows", q.Len())
		}
	}
}

// TestQueueConcurrentTransactionsKeepTheirInvariants: the data structure is
// safe under the concurrent use the session's transaction lock does not cover
// (Snapshot reads it while a mutation runs).
func TestQueueConcurrentReadsAndWrites(t *testing.T) {
	var q PromptQueue
	now := time.Now()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, p := range q.List() {
				if p.ID == "" {
					t.Error("a row with no id")
					return
				}
			}
			_ = q.Len()
		}
	}()
	for i := 0; i < 500; i++ {
		p, _, err := q.Add("x", now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.Edit(p.ID, "y"); err != nil {
			t.Fatal(err)
		}
		if _, ok := q.Remove(p.ID); !ok {
			t.Fatal("remove")
		}
	}
	close(stop)
	<-done
}
