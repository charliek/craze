package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// The roadmap's S1b exit criteria, as integration tests driven through
// engine.Control alone (plan 021 §7). What each of them is about is the WHOLE
// engine under a schedule a client can really produce — two clients at once, a
// session with no client at all, a settings race — rather than one verb's
// contract, which the tests beside them pin.
//
// Where a clause is already proved completely by a test elsewhere it is NOT
// repeated here; C13's report names the test that has it. A-X5 is
// cancel_test.go's TestACancelNamingAStaleTurnIsRefused and
// TestACancelHeldBeforeTheSessionAdmitsNothing; A7's status half is
// internal/tui's TestHostStatusNoIdleAcrossACancelledTurnWithAQueuedRow and
// TestHostStatusNoIdleBeforeAFiredSendNow; A16 is receipts_test.go's
// TestTwoClientsBothNumberingFromOneNeverCollide.
//
// The rules the plan sets for them all: barriers, never sampling; wall-clock
// only as a deadlock watchdog; and a test that pins the order of the record
// calls Sync before it lets the agent speak, because an engine event trails the
// state it describes (X11).

// record reads sub until it has seen endings turn endings, and answers with
// everything it read, in order. It is how two clients' subscriptions are
// compared at all: each is a budgeted subscription of its own, and two readers
// of Events() would steal from each other.
func record(t *testing.T, sub *agent.Subscription, endings int) []agent.Event {
	t.Helper()
	var got []agent.Event
	seen := 0
	for seen < endings {
		select {
		case rec, ok := <-sub.Records():
			if !ok {
				t.Fatalf("the subscription ended after %d of %d endings: %v", seen, endings, sub.Err())
			}
			ev, err := rec.Event()
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, ev)
			if ended("")(ev) {
				seen++
			}
		case <-time.After(watchdog):
			t.Fatalf("only %d of %d endings arrived in %s: %s", seen, endings, watchdog, describe(got))
		}
	}
	return got
}

// startsOf is every turn that started, as "id text", in order.
func startsOf(evs []agent.Event) []string {
	var out []string
	for _, ev := range evs {
		if started("")(ev) {
			out = append(out, ev.Turn.ID+" "+ev.Turn.Text)
		}
	}
	return out
}

// queuedOf is every row that entered the queue, by text, in the order the queue
// took them.
func queuedOf(evs []agent.Event) []string {
	var out []string
	for _, ev := range evs {
		if ev.Type == agent.EventQueue && ev.QueueChange == agent.QueueQueued {
			out = append(out, ev.Queue.Text)
		}
	}
	return out
}

// askEvents is every ask opening and ending in evs for this id: the openings as
// "opened", the endings as their outcome. Exactly one of each is what a resolved
// ask owes, and exactly one ending is what A-X4's matrix counts.
func askEvents(evs []agent.Event, id string) []string {
	var out []string
	for _, ev := range evs {
		switch {
		case ev.Type == agent.EventPermission && ev.Permission != nil && ev.Permission.ID == id:
			out = append(out, "opened")
		case ev.Type == agent.EventAsk && ev.Ask != nil && ev.Ask.ID == id:
			out = append(out, string(ev.Ask.Outcome)+" by "+ev.Ask.By)
		}
	}
	return out
}

// TestTwoClientsSubmittingAndQueueingAtOnceAgreeOnEveryTurn is A-X1. Two
// clients, each minted by the engine and each numbering its commands from 1,
// submit and queue at the same time on ONE engine, and each folds a budgeted
// subscription of its own.
//
// What it pins: every prompt starts exactly one turn; the rows that went
// through the queue start in the order the queue took them; both clients see
// the same total order of started events, which is what makes the record one
// record and not one per reader; and no command id collides although both
// clients count from 1 (A16's own test is receipts_test.go's).
//
// It is the -race test of the plan's "two clients cannot double-drain":
// admission, the removal-plus-claim and the successor decision are one critical
// section in one engine, so no interleaving of the two clients can start a row
// twice or lose one.
func TestTwoClientsSubmittingAndQueueingAtOnceAgreeOnEveryTurn(t *testing.T) {
	r := newRig(t, Options{})
	second, err := r.e.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)

	const each = 12
	clients := []string{r.e.NewClientID(), r.e.NewClientID()}
	if clients[0] == clients[1] {
		t.Fatalf("two clients were minted the same id %q", clients[0])
	}
	var wg sync.WaitGroup
	for _, client := range clients {
		wg.Add(1)
		go func(client string) {
			defer wg.Done()
			for i := 1; i <= each; i++ {
				cmd := Command{Client: client, ID: fmt.Sprint(i)}
				text := fmt.Sprintf("%s-%d", client, i)
				var err error
				if i%2 == 1 {
					// Enter: start now if the engine is admitting, else queue.
					_, err = r.e.Submit(cmd, text, SubmitQueue, "")
				} else {
					// The queue verb, which never starts a turn.
					_, err = r.e.Queue(cmd, text)
				}
				if err != nil {
					t.Errorf("%s: %v", text, err)
				}
			}
		}(client)
	}
	wg.Wait()

	// Every command has been answered, so the whole set is in flight: 2*each
	// turns will start and end, in some order the two clients' interleaving
	// decided. Both subscriptions are read to the last of them.
	mine := record(t, r.sub, 2*each)
	theirs := record(t, second, 2*each)

	starts := startsOf(mine)
	if len(starts) != 2*each {
		t.Fatalf("%d turns started, want %d: %s", len(starts), 2*each, strings.Join(starts, ", "))
	}
	once := map[string]int{}
	for _, ev := range mine {
		if started("")(ev) {
			once[ev.Turn.Text]++
		}
	}
	if len(once) != 2*each {
		t.Fatalf("%d distinct prompts started, want %d", len(once), 2*each)
	}
	for text, n := range once {
		if n != 1 {
			t.Fatalf("%q started %d turns", text, n)
		}
	}
	// In queue order: every row that was queued started in the order the queue
	// took it. A prompt that started at once never entered the queue and is not
	// part of the claim — Submit is "start now, else queue".
	queued := queuedOf(mine)
	if len(queued) == 0 {
		t.Fatal("no row was ever queued: the claim about queue order would be vacuous")
	}
	inQueue := map[string]bool{}
	for _, text := range queued {
		inQueue[text] = true
	}
	var drained []string
	for _, ev := range mine {
		if started("")(ev) && inQueue[ev.Turn.Text] {
			drained = append(drained, ev.Turn.Text)
		}
	}
	if strings.Join(drained, "|") != strings.Join(queued, "|") {
		t.Fatalf("rows started in the order\n  %s\nand were queued in the order\n  %s",
			strings.Join(drained, ", "), strings.Join(queued, ", "))
	}
	// One record, two readers: the same starts, in the same order.
	if got, want := strings.Join(startsOf(theirs), "|"), strings.Join(starts, "|"); got != want {
		t.Fatalf("the second client saw the starts\n  %s\nand the first\n  %s", got, want)
	}
	if got := r.s.prompts(); len(got) != 2*each {
		t.Fatalf("the session was handed %d prompts, want %d", len(got), 2*each)
	}
}

// TestASessionWithNoClientRunsEveryTurnAndParksAnAskUntilItIsAnswered is A-X2:
// a session nobody is reading — the log has no primary, no subscription exists
// while it runs, and nothing calls Events() — given one Submit and three queued
// rows, runs all four turns to idle, and then parks an ask until a client
// answers it through Control.
//
// Nobody reads while it runs, so what it stands on is State() and a
// subscription taken AFTER the fact, which replays the whole record from the
// ring. The barrier is the engine's own: each turn's continuation coming back,
// which is reported with the pass its return allowed already over.
func TestASessionWithNoClientRunsEveryTurnAndParksAnAskUntilItIsAnswered(t *testing.T) {
	returned := make(chan string, 16)
	s := newFake(t, agent.EventLogOptions{NoPrimary: true})
	e, err := newEngine(s, Options{}, &hooks{turnReturned: func(id string) { returned <- id }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	first := s.script(held())
	if _, err := e.Submit(Command{}, "one", SubmitQueue, ""); err != nil {
		t.Fatalf("submit: %v", err)
	}
	await(t, first.opened, "the first turn to open")
	for _, text := range []string{"two", "three", "four"} {
		if _, err := e.Queue(Command{}, text); err != nil {
			t.Fatalf("queue %q: %v", text, err)
		}
	}
	first.release()
	for i := 1; i <= 4; i++ {
		awaitTurn(t, returned, fmt.Sprintf("turn-%d", i))
	}
	if got := s.prompts(); strings.Join(got, "|") != "one|two|three|four" {
		t.Fatalf("the session was handed %q, want every row in queue order", got)
	}
	st := e.State()
	if st.Activity != ActivityIdle || st.Turn != "" || len(st.Queue) != 0 || !st.Prompted {
		t.Fatalf("four turns with nobody reading left the engine %+v", st)
	}

	// The ask half: a turn whose agent stops on a blocking request. Nothing
	// resolves it but a client, and the turn cannot finish until it is answered.
	parked := s.script(asking(&script{}, true))
	if _, err := e.Submit(Command{}, "five", SubmitQueue, ""); err != nil {
		t.Fatalf("submit: %v", err)
	}
	await(t, parked.ask.opened, "the ask to park")
	st = e.State()
	if st.Activity != ActivityWorking || st.Turn != "turn-5" {
		t.Fatalf("a parked ask left the engine %+v", st)
	}
	if st.PendingAsks != 1 || st.HeadAsk.Label != "permission Shell" {
		t.Fatalf("the engine reports %d pending asks, head %+v", st.PendingAsks, st.HeadAsk)
	}
	select {
	case id := <-returned:
		t.Fatalf("%s came back with its ask still open", id)
	default:
	}
	cmd := Command{Client: e.NewClientID(), ID: "1"}
	if err := e.Answer(cmd, st.HeadAsk.ID, agent.AskAnswer{OptionID: "allow-once"}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	awaitTurn(t, returned, "turn-5")
	askID := st.HeadAsk.ID
	st = e.State()
	if st.Activity != ActivityIdle || st.PendingAsks != 0 || st.Turn != "" {
		t.Fatalf("the answered turn left the engine %+v", st)
	}

	// Only now is there a reader: a subscription taken after the fact, which
	// replays the whole record from the ring.
	sub, err := e.Subscribe(agent.SubscribeOptions{After: &agent.Cursor{Incarnation: st.Incarnation}})
	if err != nil {
		t.Fatalf("subscribe after the fact: %v", err)
	}
	t.Cleanup(sub.Close)
	evs := record(t, sub, 5)
	var chain []string
	for _, ev := range evs {
		switch {
		case started("")(ev):
			chain = append(chain, "started "+ev.Turn.ID+" "+ev.Turn.Origin)
		case ended("")(ev):
			chain = append(chain, "ended "+ev.Turn.ID+" "+ev.Turn.StopReason)
		}
	}
	want := []string{
		"started turn-1 submit", "ended turn-1 end_turn",
		"started turn-2 drain", "ended turn-2 end_turn",
		"started turn-3 drain", "ended turn-3 end_turn",
		"started turn-4 drain", "ended turn-4 end_turn",
		"started turn-5 submit", "ended turn-5 end_turn",
	}
	if strings.Join(chain, "|") != strings.Join(want, "|") {
		t.Fatalf("the replayed record is\n  %s\nwant\n  %s", strings.Join(chain, ", "), strings.Join(want, ", "))
	}
	if got := askEvents(evs, askID); strings.Join(got, "|") != "opened|answered by client" {
		t.Fatalf("the ask's record is %q, want one opening and one answered ending", got)
	}
	for _, ev := range evs {
		if ev.Type == agent.EventAsk && ev.Ask != nil && ev.Ask.ID == askID && ev.Cause != cmd.Cause() {
			t.Fatalf("the ending names %q, want the answering command %q", ev.Cause, cmd.Cause())
		}
	}
}

// TestATurnThatEndsWithAnAskOpenEndsItOnce is A-X4's turn_ended row at the
// engine level: a turn whose agent left a request open ends, and that request
// gets exactly one ending, ordered before the turn's own — the session ends the
// turn for the registry and waits for the outbox before it publishes anything
// terminal.
//
// The other rows of the matrix that are reachable from here are in
// exit_asks_test.go, over the Stub; the rest are the registry's own
// (internal/agent/asks_test.go) and the live session's
// (internal/agent/asks_live_test.go).
func TestATurnThatEndsWithAnAskOpenEndsItOnce(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(asking(held(), false))
	r.submit("one")
	await(t, turn.ask.opened, "the ask to park")
	st := r.e.State()
	if st.PendingAsks != 1 {
		t.Fatalf("the engine reports %d pending asks while one is parked", st.PendingAsks)
	}
	askID := st.HeadAsk.ID
	turn.release()
	got := r.until(lastEnding)
	if ends := askEvents(got, askID); strings.Join(ends, "|") != "opened|turn_ended by turn" {
		t.Fatalf("the ask's record is %q, want one opening and one turn_ended", ends)
	}
	// Ordered in front of everything the turn's end produced.
	order := map[string]int{}
	for i, ev := range got {
		switch {
		case ev.Type == agent.EventAsk && ev.Ask != nil && ev.Ask.ID == askID:
			order["ask"] = i
		case ev.Type == agent.EventDone:
			order["done"] = i
		case ended("turn-1")(ev):
			order["ended"] = i
		}
	}
	if order["ask"] >= order["done"] || order["done"] >= order["ended"] {
		t.Fatalf("the ask ended at %d, the session's done at %d, the turn's ending at %d: %s",
			order["ask"], order["done"], order["ended"], describe(got))
	}
	if st := r.e.State(); st.PendingAsks != 0 || st.Activity != ActivityIdle {
		t.Fatalf("state after the turn that held it: %+v", st)
	}
}

// TestTwoClientsTwoSetsAndAProviderUpdateAgreeOnTheLastDelta is A-X6 as an
// integration: two CLIENTS, each folding a subscription of its own, two
// concurrent Set calls and an update the provider made itself racing one of
// them — and both subscribers and State() end on the same value, the last delta
// by Seq.
//
// It is settings_test.go's property with the sampling taken out. The schedule is
// forced with the barriers that are already there: the first Set is held at the
// provider, the second joins the worker's FIFO behind it, and the agent's own
// update lands in the window between "the provider has taken the change" and the
// locked section that writes it — the exact window a non-atomic mutate-and-emit
// would lose. So the order the deltas commit in is decided by the test, and the
// only thing left for the engine and the session to get right is that every one
// of them is enqueued in the section that mutated the snapshot.
func TestTwoClientsTwoSetsAndAProviderUpdateAgreeOnTheLastDelta(t *testing.T) {
	r := newRig(t, Options{})
	second, err := r.e.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	a := Command{Client: r.e.NewClientID(), ID: "1"}
	b := Command{Client: r.e.NewClientID(), ID: "1"}

	release := r.s.holdNextSets()
	first := make(chan SetResult, 1)
	go func() {
		res, err := r.e.Set(context.Background(), a, modeSetting("plan"))
		if err != nil {
			t.Errorf("the first client's set: %v", err)
		}
		first <- res
	}()
	// Held at the provider: the change has been taken and nothing has been
	// written yet.
	waitFor(t, func() bool { return r.heldSets() })

	next := make(chan SetResult, 1)
	go func() {
		res, err := r.e.Set(context.Background(), b, modeSetting("agent"))
		if err != nil {
			t.Errorf("the second client's set: %v", err)
		}
		next <- res
	}()
	// Queued behind it: one worker, arrival order.
	waitFor(t, func() bool { return r.queuedSets() == 1 })

	// The agent's own update, whole — mutation, delta, flush — while the first
	// client's change is at the provider.
	r.s.providerMode("ask")
	release()
	awaitSet(t, first)
	last := awaitSet(t, next)

	// Three mode deltas and no more: the two Sets and the provider's own. Each
	// subscriber is read until it has all three, because a commit puts a record
	// in a subscription's buffer and its owner hands it over afterwards.
	mine := lastModeOf(t, r.sub, 3)
	theirs := lastModeOf(t, second, 3)
	if mine != theirs {
		t.Fatalf("the two clients folded to %+v and %+v", mine, theirs)
	}
	if mine.mode != "agent" || mine.seq != last.Rev {
		t.Fatalf("the last delta by Seq is %+v, want the second client's %q at rev %d", mine, "agent", last.Rev)
	}
	if got := r.e.State().CurrentMode; got != mine.mode {
		t.Fatalf("State says %q, the last delta by Seq says %q", got, mine.mode)
	}
}
