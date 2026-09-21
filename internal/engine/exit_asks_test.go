package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/tui"
)

// A-X3 and the rows of A-X4's matrix that are reachable from the ENGINE, driven
// through engine.Control over tui.Stub — the session every golden runs on, and
// the one test session that keeps the whole ask lifecycle a client can observe:
// a turn's own token, a cancel that answers what the session was holding, and a
// close that ends what is left.
//
// What is NOT here, because it is pinned below the engine and repeating it would
// only mean two tests to change together: the registry's own matrix rows — an
// answer, a skip, a wrong-kind answer, eviction, a cancelled call context, the
// saturated outbox — are internal/agent/asks_test.go's, and the live session's
// automatic, forced and early-answered paths are asks_live_test.go's. C13's
// report maps each row to the test that has it.

// stubPermission is the request a test parks, with options a provider really
// offers: an answer names an option id, and one that does not fit is a bad
// answer rather than a cancel.
func stubPermission(id string) *agent.PermissionEvent {
	return &agent.PermissionEvent{
		ID:   id,
		Tool: "Shell",
		Options: []agent.PermissionOption{
			{OptionID: "allow-once", Name: "Allow", Kind: "allow_once"},
			{OptionID: "reject-once", Name: "Reject", Kind: "reject_once"},
		},
	}
}

// askEngine is an engine over a Stub whose primary nothing else reads: the
// committed record is read straight out of it, as the registry's own tests read
// theirs, and it survives the engine's close — the primary is never closed, and
// what was committed to it is still in its buffer.
func askEngine(t *testing.T) (*tui.Stub, *engine.Engine) {
	t.Helper()
	stub := tui.NewStub()
	e, err := engine.New(stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return stub, e
}

// committed is everything the log has put on the primary since the last call.
// Sync is the barrier: an engine event trails the state it describes, and the
// registry publishes through the same outbox. After Close there is nothing left
// to flush — the close committed what the outbox held — and the answer is what
// is in the buffer.
func committed(t *testing.T, e *engine.Engine) []agent.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()
	if err := e.Sync(ctx); err != nil && !errors.Is(err, agent.ErrLogClosing) && !errors.Is(err, agent.ErrClosed) {
		t.Fatalf("sync: %v", err)
	}
	var evs []agent.Event
	for {
		select {
		case ev := <-e.Events():
			evs = append(evs, ev)
		default:
			return evs
		}
	}
}

// readTo reads the primary until pred accepts an event, and answers with
// everything it read.
func readTo(t *testing.T, e *engine.Engine, what string, pred func(agent.Event) bool) []agent.Event {
	t.Helper()
	var got []agent.Event
	for {
		select {
		case ev := <-e.Events():
			got = append(got, ev)
			if pred(ev) {
				return got
			}
		case <-time.After(watchdog):
			t.Fatalf("no %s in %s (%d events read)", what, watchdog, len(got))
		}
	}
}

func turnEnded(ev agent.Event) bool {
	return ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnEnded
}

// askTrail is every event in evs about this ask: the opening as "opened", each
// ending as its outcome and author. Exactly one ending is what A-X4's matrix
// counts.
func askTrail(evs []agent.Event, id string) []string {
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

func wantTrail(t *testing.T, evs []agent.Event, id string, want ...string) {
	t.Helper()
	if got := askTrail(evs, id); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%s left the record %q, want %q", id, got, want)
	}
}

// parkedInATurn is a hung turn with a permission open against it: the barrier
// says the turn is open, so the card the test emits is parked against THAT
// turn's token and not against no turn at all.
func parkedInATurn(t *testing.T, stub *tui.Stub, e *engine.Engine, id string) string {
	t.Helper()
	hung := stub.HangNext()
	res, err := e.Submit(engine.Command{}, "run it", engine.SubmitQueue, "")
	if err != nil || res.Turn == "" {
		t.Fatalf("submit: %+v, %v", res, err)
	}
	select {
	case <-hung:
	case <-time.After(watchdog):
		t.Fatal("the turn never opened")
	}
	stub.Emit(agent.Event{Type: agent.EventPermission, Permission: stubPermission(id)})
	st := e.State()
	if st.PendingAsks != 1 || st.HeadAsk.ID != id || st.HeadAsk.Label != "permission Shell" {
		t.Fatalf("the engine reports %d pending asks, head %+v", st.PendingAsks, st.HeadAsk)
	}
	return res.Turn
}

// TestAnAskAnsweredTwiceThroughControlEndsOnce is A-X3's first half: one
// EventAsk{answered} and one agent.ErrAlreadyResolved, whoever sends the second
// answer.
//
// The second answer is another client's, deliberately: a resend of the SAME
// command id is answered from the receipts table with the first call's result
// and never reaches the registry at all (receipts_test.go), and what this is
// about is the registry's own "first valid answer wins".
func TestAnAskAnsweredTwiceThroughControlEndsOnce(t *testing.T) {
	stub, e := askEngine(t)
	parkedInATurn(t, stub, e, "perm-1")

	first := engine.Command{Client: e.NewClientID(), ID: "1"}
	if err := e.Answer(first, "perm-1", agent.AskAnswer{OptionID: "allow-once"}); err != nil {
		t.Fatalf("the first answer: %v", err)
	}
	second := engine.Command{Client: e.NewClientID(), ID: "1"}
	err := e.Answer(second, "perm-1", agent.AskAnswer{OptionID: "reject-once"})
	if !errors.Is(err, agent.ErrAlreadyResolved) || engine.Code(err) != "already_resolved" {
		t.Fatalf("the second answer: %v (%s)", err, engine.Code(err))
	}
	evs := committed(t, e)
	wantTrail(t, evs, "perm-1", "opened", "answered by client")
	for _, ev := range evs {
		if ev.Type == agent.EventAsk && ev.Ask.ID == "perm-1" {
			if ev.Ask.OptionID != "allow-once" || ev.Cause != first.Cause() {
				t.Fatalf("the ending is %+v, cause %q: want the first answer's", ev.Ask, ev.Cause)
			}
		}
	}
	if rec, ok := e.Ask("perm-1"); !ok || rec.Status != agent.AskResolved || rec.Outcome != agent.AskAnswered {
		t.Fatalf("the record is %+v (%v)", rec, ok)
	}
	if st := e.State(); st.PendingAsks != 0 {
		t.Fatalf("%d asks still pending", st.PendingAsks)
	}
}

// TestAnInvalidAnswerThroughControlLeavesTheAskAnswerable is A-X3's second
// half: an answer that does not fit is agent.ErrBadAnswer, emits nothing at all,
// and leaves the ask exactly where it was — so the client that mis-addressed one
// call has not left the agent waiting for an Esc.
func TestAnInvalidAnswerThroughControlLeavesTheAskAnswerable(t *testing.T) {
	stub, e := askEngine(t)
	parkedInATurn(t, stub, e, "perm-1")
	// The opening, out of the way: what follows it is what the bad answer wrote.
	wantTrail(t, committed(t, e), "perm-1", "opened")

	cmd := engine.Command{Client: e.NewClientID(), ID: "1"}
	err := e.Answer(cmd, "perm-1", agent.AskAnswer{OptionID: "no-such-option"})
	if !errors.Is(err, agent.ErrBadAnswer) || engine.Code(err) != "bad_request" {
		t.Fatalf("an option the request never offered: %v (%s)", err, engine.Code(err))
	}
	if got := committed(t, e); len(askTrail(got, "perm-1")) != 0 {
		t.Fatalf("a refused answer wrote %q", askTrail(got, "perm-1"))
	}
	if st := e.State(); st.PendingAsks != 1 || st.HeadAsk.ID != "perm-1" {
		t.Fatalf("a refused answer left the engine %d pending, head %+v", st.PendingAsks, st.HeadAsk)
	}
	// Still answerable, with a new id of the same client's: the refusal ran
	// nothing, so nothing is owed a receipt either.
	again := engine.Command{Client: cmd.Client, ID: "2"}
	if err := e.Answer(again, "perm-1", agent.AskAnswer{OptionID: "allow-once"}); err != nil {
		t.Fatalf("the answer that fits: %v", err)
	}
	wantTrail(t, committed(t, e), "perm-1", "answered by client")
}

// TestTwoClientsRacingOneAnswerLeaveOneEnding is A-X3's race: two clients send
// the same valid answer at the same time, and exactly one wins — one nil, one
// agent.ErrAlreadyResolved, and one ending in the record. Answer validates and
// claims in one registry section, which is the whole of why.
func TestTwoClientsRacingOneAnswerLeaveOneEnding(t *testing.T) {
	stub, e := askEngine(t)
	parkedInATurn(t, stub, e, "perm-1")

	errs := make([]error, 2)
	clients := []string{e.NewClientID(), e.NewClientID()}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = e.Answer(engine.Command{Client: clients[i], ID: "1"}, "perm-1", agent.AskAnswer{OptionID: "allow-once"})
		}(i)
	}
	close(start)
	wg.Wait()

	won, lost := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, agent.ErrAlreadyResolved):
			lost++
		default:
			t.Fatalf("an answer came back with %v", err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("%d answers won and %d lost, want one of each", won, lost)
	}
	wantTrail(t, committed(t, e), "perm-1", "opened", "answered by client")
}

// TestACancelWhileAnAskIsParkedEndsItOnce is A-X4's "Cancel while parked" row at
// the engine level: the cancel a client makes through Control reaches the
// session, which answers every request it was holding, and the ask gets exactly
// one ending — cancelled, by cancel — before the turn's own.
func TestACancelWhileAnAskIsParkedEndsItOnce(t *testing.T) {
	stub, e := askEngine(t)
	turn := parkedInATurn(t, stub, e, "perm-1")

	res, err := e.Cancel(context.Background(), engine.Command{}, turn)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if res.Turn != turn {
		t.Fatalf("the cancel was held against %q, want %q", res.Turn, turn)
	}
	evs := readTo(t, e, "the turn's ending", turnEnded)
	wantTrail(t, evs, "perm-1", "opened", "cancelled by cancel")
	last := evs[len(evs)-1].Turn
	if last.ID != turn || last.StopReason != "cancelled" {
		t.Fatalf("the turn ended %+v, want cancelled", last)
	}
	if st := e.State(); st.PendingAsks != 0 || st.Activity != engine.ActivityIdle || !st.Cancelled {
		t.Fatalf("state after the cancel: %+v", st)
	}
	if rec, ok := e.Ask("perm-1"); !ok || rec.Outcome != agent.AskCancelled || rec.By != agent.AskByCancel {
		t.Fatalf("the record is %+v (%v)", rec, ok)
	}
}

// TestCloseWhileAnAskIsParkedEndsItOnce is A-X4's "Close while parked" row at
// the engine level: engine.Close closes the session, which ends every parked ask
// as closing BEFORE the log's own close, so the ending is in the record rather
// than dropped with the outbox.
//
// The ask is opened between turns, which is also the only kind a close can find
// parked once a turn has ended: a turn's own end would have taken it.
func TestCloseWhileAnAskIsParkedEndsItOnce(t *testing.T) {
	stub, e := askEngine(t)
	stub.Emit(agent.Event{Type: agent.EventPermission, Permission: stubPermission("perm-1")})
	wantTrail(t, committed(t, e), "perm-1", "opened")
	if st := e.State(); st.PendingAsks != 1 {
		t.Fatalf("the engine reports %d pending asks", st.PendingAsks)
	}

	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	wantTrail(t, committed(t, e), "perm-1", "closing by close")
	if rec, ok := e.Ask("perm-1"); !ok || rec.Outcome != agent.AskClosing || rec.By != agent.AskByClose {
		t.Fatalf("the record is %+v (%v)", rec, ok)
	}
	if err := e.Answer(engine.Command{}, "perm-1", agent.AskAnswer{OptionID: "allow-once"}); !errors.Is(err, agent.ErrAlreadyResolved) {
		t.Fatalf("an answer after the close: %v", err)
	}
}

// TestAnAskOpenedBetweenTurnsSurvivesAWholeTurn is A-X4's "ask opened between
// turns" row at the engine level: a request that arrived when no turn of craze's
// own was running belongs to no turn, so a turn that starts and ends after it
// takes nothing away — it has no ending at all until a client answers it.
func TestAnAskOpenedBetweenTurnsSurvivesAWholeTurn(t *testing.T) {
	stub, e := askEngine(t)
	stub.Emit(agent.Event{Type: agent.EventPermission, Permission: stubPermission("perm-1")})
	wantTrail(t, committed(t, e), "perm-1", "opened")

	if _, err := e.Submit(engine.Command{}, "one", engine.SubmitQueue, ""); err != nil {
		t.Fatalf("submit: %v", err)
	}
	evs := readTo(t, e, "the turn's ending", turnEnded)
	wantTrail(t, evs, "perm-1")
	st := e.State()
	if st.PendingAsks != 1 || st.HeadAsk.ID != "perm-1" {
		t.Fatalf("the turn took the between-turns ask with it: %d pending, head %+v", st.PendingAsks, st.HeadAsk)
	}
	if st.Activity != engine.ActivityIdle {
		t.Fatalf("activity %s after the turn", st.Activity)
	}

	cmd := engine.Command{Client: e.NewClientID(), ID: "1"}
	if err := e.Answer(cmd, "perm-1", agent.AskAnswer{Cancel: true}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	wantTrail(t, committed(t, e), "perm-1", "cancelled by client")
	if st := e.State(); st.PendingAsks != 0 {
		t.Fatalf("%d asks still pending", st.PendingAsks)
	}
}
