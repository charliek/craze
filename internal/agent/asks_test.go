package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The ask registry's tests (plan 021 §3.6, A-X3 and A-X4). Every one of them
// runs over a real EventLog, because half of what the registry promises is
// about the record it writes: one ending per ask, an opening always before its
// ending, and a self-contained ending for an ask nobody ever saw raised.
//
// Nothing here sleeps. A test knows where another goroutine is from a channel
// it closed, a hook the log calls, or a barrier of its own; the only clock is
// the package's watchdog (logWatchdog), which turns a deadlock into a failure
// instead of a hung run.

var askTestTime = time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)

// askOptions are a permission's options as a provider really offers them: two
// of the same kind, which is grok's shape and the reason an answer names an
// option id and never a kind (SD-25).
var askOptions = []PermissionOption{
	{OptionID: "allow-once", Name: "Allow", Kind: "allow_once"},
	{OptionID: "enable-always-approve", Name: "Always allow", Kind: "allow_once"},
	{OptionID: "reject-once", Name: "Reject", Kind: "reject_once"},
}

// newAskRegistry is a registry over a log of its own, on a fixed clock.
func newAskRegistry(t *testing.T) (*AskRegistry, *EventLog) {
	t.Helper()
	l := newTestLog(t, EventLogOptions{})
	return NewAskRegistry(l, func() time.Time { return askTestTime }), l
}

func permReq() AskRequest {
	return AskRequest{Kind: AskPermission, Body: AskBody{Permission: &PermissionEvent{
		Tool: "Edit a.go", Options: slices.Clone(askOptions)}}}
}

func questionReq() AskRequest {
	return AskRequest{Kind: AskQuestion, Body: AskBody{Question: &QuestionEvent{
		Title: "Question",
		Questions: []Question{
			{ID: "q1", Prompt: "Pick one", Options: []Option{{ID: "opt-a", Label: "A"}, {ID: "opt-b", Label: "B"}}},
			// The option craze invents for a question with none of its own,
			// whose empty id is a real offered id (live.go's
			// questionsFromRequest).
			{ID: "q2", Prompt: "Anything else?", Options: []Option{{Label: "OK"}}},
		},
	}}}
}

func planReq() AskRequest {
	return AskRequest{Kind: AskPlan, Body: AskBody{Plan: &PlanEvent{
		Name: "Refactor", Overview: "why", Plan: "# Plan\n\n1. do it\n"}}}
}

func reqFor(kind AskKind) AskRequest {
	switch kind {
	case AskQuestion:
		return questionReq()
	case AskPlan:
		return planReq()
	default:
		return permReq()
	}
}

// mustOpen opens an ask, failing the test on the one error Open has.
func mustOpen(t *testing.T, r *AskRegistry, ctx context.Context, token TurnToken, req AskRequest) *Ask {
	t.Helper()
	a, err := r.Open(ctx, token, req)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return a
}

// askEvents is everything the log has committed since the last call, in order.
// The Flush is the barrier — the outbox publishes on a goroutine of its own —
// and the primary, which nothing reads in these tests, is where the log keeps
// what it committed. It fails rather than blocks if a test enqueues more than
// the primary holds.
func askEvents(t *testing.T, l *EventLog) []Event {
	t.Helper()
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return drainPrimary(l)
}

// eventKinds is a run of events as a failure message names them.
func eventKinds(evs []Event) string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = string(ev.Type)
		if ev.Ask != nil {
			out[i] += "{" + ev.Ask.ID + ":" + string(ev.Ask.Outcome) + "}"
		}
	}
	return "[" + strings.Join(out, " ") + "]"
}

// askEndings is every ending in evs.
func askEndings(evs []Event) []*AskUpdate {
	var out []*AskUpdate
	for _, ev := range evs {
		if ev.Type == EventAsk {
			out = append(out, ev.Ask)
		}
	}
	return out
}

// oneEnding fails unless evs holds exactly one ending, and answers with it.
func oneEnding(t *testing.T, evs []Event) *AskUpdate {
	t.Helper()
	ends := askEndings(evs)
	if len(ends) != 1 {
		t.Fatalf("%d endings in %s, want exactly 1", len(ends), eventKinds(evs))
	}
	return ends[0]
}

// assertEnding fails unless u is an ending with this id, outcome and author.
func assertEnding(t *testing.T, u *AskUpdate, id string, outcome AskOutcome, by string) {
	t.Helper()
	if u.ID != id || u.Outcome != outcome || u.By != by {
		t.Fatalf("ending %+v, want id %q %s by %s", u, id, outcome, by)
	}
}

// assertResolved fails unless the record says it ended this way.
func assertResolved(t *testing.T, rec AskRecord, outcome AskOutcome, by string) {
	t.Helper()
	if rec.Status != AskResolved || rec.Outcome != outcome || rec.By != by {
		t.Fatalf("record %s is %s/%s by %q, want resolved %s by %s", rec.ID, rec.Status, rec.Outcome, rec.By, outcome, by)
	}
	if rec.ResolvedAt.IsZero() {
		t.Fatalf("record %s has no ResolvedAt", rec.ID)
	}
}

// waitOn runs Ask.Wait on a goroutine, so a test can assert that it returns
// without hanging the run if it does not.
func waitOn(a *Ask) <-chan AskRecord {
	out := make(chan AskRecord, 1)
	go func() { out <- a.Wait() }()
	return out
}

// TestAskAnsweredOnceEndsOnce is A-X4's first rows: a valid answer, an
// explicit cancel and a skip each write exactly one ending, the waiting call
// gets that decision, and a second answer is ErrAlreadyResolved with nothing
// further written.
func TestAskAnsweredOnceEndsOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    AskKind
		answer  AskAnswer
		outcome AskOutcome
		check   func(t *testing.T, u *AskUpdate)
	}{
		{"a permission answered with an offered option", AskPermission,
			AskAnswer{OptionID: "enable-always-approve"}, AskAnswered,
			func(t *testing.T, u *AskUpdate) {
				if u.OptionID != "enable-always-approve" || u.Label != "Always allow" {
					t.Fatalf("ending %+v, want the option and its offered name", u)
				}
			}},
		{"a permission cancelled deliberately", AskPermission,
			AskAnswer{Cancel: true}, AskCancelled,
			func(t *testing.T, u *AskUpdate) {
				if u.OptionID != "" || u.Label != "" {
					t.Fatalf("ending %+v, want no option at all", u)
				}
			}},
		{"a question answered", AskQuestion,
			AskAnswer{Answers: map[string][]string{"q1": {"opt-b"}, "q2": {""}}}, AskAnswered,
			func(t *testing.T, u *AskUpdate) {
				if len(u.Answers) != 2 || u.Answers["q1"][0] != "opt-b" || u.Answers["q2"][0] != "" {
					t.Fatalf("ending %+v, want both answers, the invented OK option included", u)
				}
				if u.Skip {
					t.Fatal("an answered question is not a skipped one")
				}
			}},
		{"a question skipped", AskQuestion,
			AskAnswer{Skip: true}, AskAnswered,
			func(t *testing.T, u *AskUpdate) {
				if !u.Skip || len(u.Answers) != 0 {
					t.Fatalf("ending %+v, want a skip and no answers", u)
				}
			}},
		{"a plan accepted", AskPlan,
			AskAnswer{Accept: true}, AskAnswered,
			func(t *testing.T, u *AskUpdate) {
				if !u.Accepted {
					t.Fatalf("ending %+v, want accepted", u)
				}
			}},
		{"a plan rejected", AskPlan,
			AskAnswer{Reject: true}, AskAnswered,
			func(t *testing.T, u *AskUpdate) {
				if u.Accepted {
					t.Fatalf("ending %+v, want not accepted", u)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, l := newAskRegistry(t)
			token := r.BeginTurn()
			a := mustOpen(t, r, t.Context(), token, reqFor(tc.kind))
			waited := waitOn(a)

			opened := askEvents(t, l)
			if len(opened) != 1 || askEndings(opened) != nil {
				t.Fatalf("opening wrote %s, want one opening and no ending", eventKinds(opened))
			}
			if got := r.Asks(); len(got) != 1 || got[0].ID != a.ID() || got[0].Status != AskOpen {
				t.Fatalf("Asks() is %+v, want the one open ask", got)
			}

			rec, err := r.Answer("tui-1/7", a.ID(), tc.answer)
			if err != nil {
				t.Fatalf("Answer: %v", err)
			}
			assertResolved(t, rec, tc.outcome, AskByClient)

			evs := askEvents(t, l)
			u := oneEnding(t, evs)
			assertEnding(t, u, a.ID(), tc.outcome, AskByClient)
			if u.Kind != tc.kind {
				t.Fatalf("ending kind %q, want %q", u.Kind, tc.kind)
			}
			if u.Body != nil {
				t.Fatalf("ending carries a body although its opening was published: %+v", u.Body)
			}
			if evs[0].Cause != "tui-1/7" {
				t.Fatalf("ending cause %q, want the answering command's", evs[0].Cause)
			}
			tc.check(t, u)

			got := <-waited
			assertResolved(t, got, tc.outcome, AskByClient)
			if !got.Consumed {
				t.Fatal("Wait returned a decision without recording that it was taken")
			}
			if len(r.Asks()) != 0 {
				t.Fatalf("a resolved ask is still open: %+v", r.Asks())
			}

			if _, err := r.Answer("tui-1/8", a.ID(), tc.answer); !errors.Is(err, ErrAlreadyResolved) {
				t.Fatalf("the second answer returned %v, want ErrAlreadyResolved", err)
			}
			if evs := askEvents(t, l); len(evs) != 0 {
				t.Fatalf("the second answer wrote %s, want nothing", eventKinds(evs))
			}
		})
	}
}

// TestAskBadAnswerLeavesItOpen is A-X3 and A-X4's "invalid then valid" row: an
// answer that does not fit is refused with nothing written and the ask still
// answerable — where today's AnswerPermission cancels the request before it
// validates the option id (live.go's :1295-1298).
func TestAskBadAnswerLeavesItOpen(t *testing.T) {
	r, l := newAskRegistry(t)
	token := r.BeginTurn()
	a := mustOpen(t, r, t.Context(), token, permReq())
	askEvents(t, l)

	if _, err := r.Answer("", a.ID(), AskAnswer{OptionID: "no-such-option"}); !errors.Is(err, ErrBadAnswer) {
		t.Fatalf("an option the request never offered returned %v, want ErrBadAnswer", err)
	}
	if evs := askEvents(t, l); len(evs) != 0 {
		t.Fatalf("a refused answer wrote %s, want nothing", eventKinds(evs))
	}
	if got := r.Asks(); len(got) != 1 || got[0].Status != AskOpen {
		t.Fatalf("Asks() is %+v, want the ask still open", got)
	}

	if _, err := r.Answer("", a.ID(), AskAnswer{OptionID: "allow-once"}); err != nil {
		t.Fatalf("the valid answer that followed: %v", err)
	}
	assertEnding(t, oneEnding(t, askEvents(t, l)), a.ID(), AskAnswered, AskByClient)
}

// TestAskAnswerRefusesWhatDoesNotFit walks every shape validation refuses. Each
// one leaves the ask open and writes nothing, so one mis-addressed call can
// never leave the agent waiting for an Esc.
func TestAskAnswerRefusesWhatDoesNotFit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   AskKind
		answer AskAnswer
	}{
		{"a permission with no option and no explicit cancel", AskPermission, AskAnswer{}},
		{"a permission cancelled and answered at once", AskPermission, AskAnswer{OptionID: "allow-once", Cancel: true}},
		{"a permission answered as a plan", AskPermission, AskAnswer{Accept: true}},
		{"a permission answered as a question", AskPermission, AskAnswer{Skip: true}},
		{"a question with neither answers nor a skip", AskQuestion, AskAnswer{}},
		{"a question skipped and answered at once", AskQuestion, AskAnswer{Skip: true, Answers: map[string][]string{"q1": {"opt-a"}}}},
		{"a question answered as a permission", AskQuestion, AskAnswer{OptionID: "opt-a"}},
		{"a question naming a question it never asked", AskQuestion, AskAnswer{Answers: map[string][]string{"q9": {"opt-a"}}}},
		{"a question naming an option it never offered", AskQuestion, AskAnswer{Answers: map[string][]string{"q1": {"opt-z"}}}},
		{"a plan with neither accept nor reject", AskPlan, AskAnswer{}},
		{"a plan accepted and rejected at once", AskPlan, AskAnswer{Accept: true, Reject: true}},
		{"a plan answered as a question", AskPlan, AskAnswer{Skip: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, l := newAskRegistry(t)
			token := r.BeginTurn()
			a := mustOpen(t, r, t.Context(), token, reqFor(tc.kind))
			askEvents(t, l)

			if _, err := r.Answer("", a.ID(), tc.answer); !errors.Is(err, ErrBadAnswer) {
				t.Fatalf("Answer returned %v, want ErrBadAnswer", err)
			}
			if evs := askEvents(t, l); len(evs) != 0 {
				t.Fatalf("a refused answer wrote %s, want nothing", eventKinds(evs))
			}
			if got := r.Asks(); len(got) != 1 || got[0].Status != AskOpen {
				t.Fatalf("Asks() is %+v, want the ask still open", got)
			}
		})
	}
}

// TestAskWrongKindAnswerIsRefused is today's TestAnswerWrongKindRejected
// through the registry: a question's id answered as a plan or a permission is
// ErrBadAnswer, the ask survives it, and its own kind's answer still works.
func TestAskWrongKindAnswerIsRefused(t *testing.T) {
	r, l := newAskRegistry(t)
	token := r.BeginTurn()
	a := mustOpen(t, r, t.Context(), token, questionReq())
	askEvents(t, l)

	if _, err := r.Answer("", a.ID(), AskAnswer{Accept: true}); !errors.Is(err, ErrBadAnswer) {
		t.Fatalf("a question id answering a plan returned %v, want ErrBadAnswer", err)
	}
	if _, err := r.Answer("", a.ID(), AskAnswer{OptionID: "opt-a"}); !errors.Is(err, ErrBadAnswer) {
		t.Fatalf("a question id answering a permission returned %v, want ErrBadAnswer", err)
	}
	if _, err := r.Answer("", a.ID(), AskAnswer{Skip: true}); err != nil {
		t.Fatalf("the question's own answer: %v", err)
	}
	assertEnding(t, oneEnding(t, askEvents(t, l)), a.ID(), AskAnswered, AskByClient)
}

// TestAskUnknownIDsAndEviction pins 05's two error codes and the line between
// them: an id this incarnation never issued is unknown_ask, and one it issued
// is already_resolved even after its record has been evicted, because the
// kind's own counter still accounts for it.
func TestAskUnknownIDsAndEviction(t *testing.T) {
	// NoPrimary: this test writes more events than a primary nobody reads
	// holds, and what it asserts is the registry's bookkeeping, not the record.
	l := newTestLog(t, EventLogOptions{NoPrimary: true})
	r := NewAskRegistry(l, func() time.Time { return askTestTime })

	if _, err := r.Answer("", "perm-1", AskAnswer{Cancel: true}); !errors.Is(err, ErrUnknownAsk) {
		t.Fatalf("an id nothing ever issued returned %v, want ErrUnknownAsk", err)
	}
	if _, err := r.Answer("", "not-an-ask-id", AskAnswer{Cancel: true}); !errors.Is(err, ErrUnknownAsk) {
		t.Fatalf("an id no kind spells returned %v, want ErrUnknownAsk", err)
	}

	// One more than the registry keeps, each answered, so every one of them has
	// a terminal record and the first is evicted by the last.
	ids := make([]string, 0, keptAsks+1)
	for range keptAsks + 1 {
		a := mustOpen(t, r, t.Context(), TurnToken{}, permReq())
		if _, err := r.Answer("", a.ID(), AskAnswer{Cancel: true}); err != nil {
			t.Fatalf("Answer(%s): %v", a.ID(), err)
		}
		ids = append(ids, a.ID())
	}
	if _, kept := r.Record(ids[0]); kept {
		t.Fatalf("%s is still kept after %d more asks", ids[0], keptAsks)
	}
	if _, kept := r.Record(ids[len(ids)-1]); !kept {
		t.Fatalf("%s is not kept although it is the newest record", ids[len(ids)-1])
	}
	if _, err := r.Answer("", ids[0], AskAnswer{Cancel: true}); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("an evicted id returned %v, want ErrAlreadyResolved: its counter still accounts for it", err)
	}
	// One past the counter was never issued, however close it looks.
	next := fmt.Sprintf("perm-%d", len(ids)+1)
	if _, err := r.Answer("", next, AskAnswer{Cancel: true}); !errors.Is(err, ErrUnknownAsk) {
		t.Fatalf("%s returned %v, want ErrUnknownAsk", next, err)
	}
}

// TestAskAdoptedIDIsOnlyKnownWhileItsRecordIs is the comment asks.go carries
// about an adopted id: it has no counter behind it, so once its record has been
// evicted nothing is left to say it was ever issued.
func TestAskAdoptedIDIsOnlyKnownWhileItsRecordIs(t *testing.T) {
	l := newTestLog(t, EventLogOptions{NoPrimary: true})
	r := NewAskRegistry(l, func() time.Time { return askTestTime })

	adopted := mustOpen(t, r, t.Context(), TurnToken{}, AskRequest{
		Kind: AskPermission, ID: "card-7", Body: permReq().Body})
	if _, err := r.Answer("", "card-7", AskAnswer{Cancel: true}); err != nil {
		t.Fatalf("Answer(card-7): %v", err)
	}
	if _, err := r.Answer("", adopted.ID(), AskAnswer{Cancel: true}); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("a kept adopted record returned %v, want ErrAlreadyResolved", err)
	}
	for range keptAsks {
		a := mustOpen(t, r, t.Context(), TurnToken{}, permReq())
		if _, err := r.Answer("", a.ID(), AskAnswer{Cancel: true}); err != nil {
			t.Fatalf("Answer(%s): %v", a.ID(), err)
		}
	}
	if _, err := r.Answer("", "card-7", AskAnswer{Cancel: true}); !errors.Is(err, ErrUnknownAsk) {
		t.Fatalf("an evicted adopted id returned %v, want ErrUnknownAsk", err)
	}
}

// TestAskAdoptedIDsSurviveAndRefuseACollision is plan item 22's half of the
// registry: tui.Stub routes a test's card through Open with the test's own id,
// which must reach the opening unchanged, spend no counter, and never be
// allowed to steal an open ask's answers.
func TestAskAdoptedIDsSurviveAndRefuseACollision(t *testing.T) {
	r, l := newAskRegistry(t)
	token := r.BeginTurn()

	a := mustOpen(t, r, t.Context(), token, AskRequest{Kind: AskQuestion, ID: "card-1", Body: questionReq().Body})
	if a.ID() != "card-1" {
		t.Fatalf("Open minted %q over an adopted id", a.ID())
	}
	evs := askEvents(t, l)
	if len(evs) != 1 || evs[0].Question == nil || evs[0].Question.ID != "card-1" {
		t.Fatalf("the opening is %s, want one question carrying the adopted id", eventKinds(evs))
	}

	if _, err := r.Open(t.Context(), token, AskRequest{Kind: AskQuestion, ID: "card-1", Body: questionReq().Body}); !errors.Is(err, ErrAskIDInUse) {
		t.Fatalf("a colliding adopted id returned %v, want ErrAskIDInUse", err)
	}
	if evs := askEvents(t, l); len(evs) != 0 {
		t.Fatalf("a refused adoption wrote %s, want nothing", eventKinds(evs))
	}
	if got := r.Asks(); len(got) != 1 || got[0].ID != "card-1" {
		t.Fatalf("Asks() is %+v, want the first ask untouched", got)
	}

	// The counter is untouched by an adopted id, so the next minted one is
	// still the first of its kind.
	minted := mustOpen(t, r, t.Context(), token, questionReq())
	if minted.ID() != "ask-1" {
		t.Fatalf("the first minted question is %q, want ask-1: an adopted id spends no number", minted.ID())
	}
	// Once the first has ended, its id may be adopted again.
	if _, err := r.Answer("", "card-1", AskAnswer{Skip: true}); err != nil {
		t.Fatalf("Answer(card-1): %v", err)
	}
	askEvents(t, l)
	if _, err := r.Open(t.Context(), token, AskRequest{Kind: AskQuestion, ID: "card-1", Body: questionReq().Body}); err != nil {
		t.Fatalf("re-adopting a resolved id: %v", err)
	}
}

// TestAskIDsAreNumberedPerKind pins the spellings goldens and the --json
// fixtures rest on, and that every mint spends a number whatever became of the
// ask: today's nextID does, and a golden that depended on how an ask ended
// would be a trap.
func TestAskIDsAreNumberedPerKind(t *testing.T) {
	r, l := newAskRegistry(t)
	token := r.BeginTurn()

	first := mustOpen(t, r, t.Context(), token, permReq())
	question := mustOpen(t, r, t.Context(), token, questionReq())
	plan := mustOpen(t, r, t.Context(), token, planReq())
	if first.ID() != "perm-1" || question.ID() != "ask-1" || plan.ID() != "plan-1" {
		t.Fatalf("ids %q %q %q, want perm-1 ask-1 plan-1", first.ID(), question.ID(), plan.ID())
	}
	// An automatic resolution spends one.
	r.Automatic(token, permReq(), AskAnswer{OptionID: "allow-once"})
	// So does a refused open: this one arrives for a turn that has ended.
	ended := r.BeginTurn()
	r.EndTurn(ended)
	refused, err := r.Open(t.Context(), ended, permReq())
	if err != nil {
		t.Fatalf("Open on an ended turn: %v", err)
	}
	if refused.ID() != "perm-3" {
		t.Fatalf("the refused ask is %q, want perm-3: an automatic and a refusal each spend a number", refused.ID())
	}
	next := mustOpen(t, r, t.Context(), token, permReq())
	if next.ID() != "perm-4" {
		t.Fatalf("the next permission is %q, want perm-4", next.ID())
	}
	askEvents(t, l)
}

// TestAskCancelTurnEndsEveryOpenAsk is today's cancelWaiting: a cancel answers
// every parked request whatever turn it belongs to — 05's table accepts a
// cancel whenever an ask is pending — and it marks its own turn, so a request
// that arrives for that turn afterwards is refused rather than parked forever.
// A later turn parks normally (TestNextTurnParksAgain's property).
func TestAskCancelTurnEndsEveryOpenAsk(t *testing.T) {
	r, l := newAskRegistry(t)
	token := r.BeginTurn()
	inTurn := mustOpen(t, r, t.Context(), token, permReq())
	between := mustOpen(t, r, t.Context(), TurnToken{}, questionReq())
	waitedIn, waitedBetween := waitOn(inTurn), waitOn(between)
	askEvents(t, l)

	r.CancelTurn(token)

	evs := askEvents(t, l)
	ends := askEndings(evs)
	if len(ends) != 2 || len(evs) != 2 {
		t.Fatalf("the cancel wrote %s, want one ending for each open ask", eventKinds(evs))
	}
	assertEnding(t, ends[0], inTurn.ID(), AskCancelled, AskByCancel)
	assertEnding(t, ends[1], between.ID(), AskCancelled, AskByCancel)
	assertResolved(t, <-waitedIn, AskCancelled, AskByCancel)
	assertResolved(t, <-waitedBetween, AskCancelled, AskByCancel)

	// A request that arrives for the cancelled turn afterwards is refused, and
	// says it was a cancel.
	late, err := r.Open(t.Context(), token, planReq())
	if err != nil {
		t.Fatalf("Open on a cancelled turn: %v", err)
	}
	u := oneEnding(t, askEvents(t, l))
	assertEnding(t, u, late.ID(), AskCancelled, AskByCancel)
	if u.Body == nil || u.Body.Plan == nil {
		t.Fatalf("a refused open wrote %+v, want its body", u)
	}

	// The next turn is a new token, so it parks as it always did.
	next := r.BeginTurn()
	fresh := mustOpen(t, r, t.Context(), next, permReq())
	if got := askEvents(t, l); len(got) != 1 || got[0].Type != EventPermission {
		t.Fatalf("the next turn's ask wrote %s, want its opening", eventKinds(got))
	}
	if rec, ok := r.Record(fresh.ID()); !ok || rec.Status != AskOpen {
		t.Fatalf("the next turn's ask is %+v, want it open", rec)
	}
}

// TestAskCancelWithNoTurnAnswersWhatIsPending is 05's "idle with an ask
// pending" row: a cancel against no turn of craze's own still answers
// everything parked, and marks nothing.
func TestAskCancelWithNoTurnAnswersWhatIsPending(t *testing.T) {
	r, l := newAskRegistry(t)
	token := r.BeginTurn()
	a := mustOpen(t, r, t.Context(), TurnToken{}, permReq())
	askEvents(t, l)

	r.CancelTurn(TurnToken{})

	assertEnding(t, oneEnding(t, askEvents(t, l)), a.ID(), AskCancelled, AskByCancel)
	// The running turn was not marked by a cancel that named no turn.
	next := mustOpen(t, r, t.Context(), token, permReq())
	if rec, ok := r.Record(next.ID()); !ok || rec.Status != AskOpen {
		t.Fatalf("the turn's own ask is %+v, want it open", rec)
	}
}

// TestAskEndTurnEndsOnlyItsOwnAsks is §3.6's rule that turn_ended applies only
// to asks opened against the token whose EndTurn was notified: another turn's
// and the no-turn token's are untouched.
func TestAskEndTurnEndsOnlyItsOwnAsks(t *testing.T) {
	r, l := newAskRegistry(t)
	first := r.BeginTurn()
	second := r.BeginTurn()
	inFirst := mustOpen(t, r, t.Context(), first, permReq())
	inSecond := mustOpen(t, r, t.Context(), second, questionReq())
	between := mustOpen(t, r, t.Context(), TurnToken{}, planReq())
	waited := waitOn(inFirst)
	askEvents(t, l)

	r.EndTurn(first)

	assertEnding(t, oneEnding(t, askEvents(t, l)), inFirst.ID(), AskTurnEnded, AskByTurn)
	assertResolved(t, <-waited, AskTurnEnded, AskByTurn)
	open := r.Asks()
	if len(open) != 2 || open[0].ID != inSecond.ID() || open[1].ID != between.ID() {
		t.Fatalf("Asks() is %+v, want the other turn's and the no-turn ask, in opening order", open)
	}

	// A request that arrives for the ended turn afterwards is refused as ended.
	late, err := r.Open(t.Context(), first, permReq())
	if err != nil {
		t.Fatalf("Open on an ended turn: %v", err)
	}
	u := oneEnding(t, askEvents(t, l))
	assertEnding(t, u, late.ID(), AskTurnEnded, AskByTurn)
	if u.Body == nil || u.Body.Permission == nil {
		t.Fatalf("a refused open wrote %+v, want its body", u)
	}
}

// TestAskOnTheNoTurnTokenSurvivesAWholeTurn is A-X4's "ask opened between
// turns, then a turn starts and ends" row: no ending until somebody answers it.
func TestAskOnTheNoTurnTokenSurvivesAWholeTurn(t *testing.T) {
	r, l := newAskRegistry(t)
	a := mustOpen(t, r, t.Context(), TurnToken{}, permReq())
	askEvents(t, l)

	token := r.BeginTurn()
	r.EndTurn(token)

	if evs := askEvents(t, l); len(evs) != 0 {
		t.Fatalf("a whole turn wrote %s for an ask that is not its own, want nothing", eventKinds(evs))
	}
	if got := r.Asks(); len(got) != 1 || got[0].Status != AskOpen {
		t.Fatalf("Asks() is %+v, want the ask still open", got)
	}
	if _, err := r.Answer("", a.ID(), AskAnswer{OptionID: "allow-once"}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	assertEnding(t, oneEnding(t, askEvents(t, l)), a.ID(), AskAnswered, AskByClient)
}

// TestAskCloseEndsEverythingAndRefusesAfter is the close row of A-X4, and what
// V3 reads a journal for: every ask has an ending, the session's close
// included.
func TestAskCloseEndsEverythingAndRefusesAfter(t *testing.T) {
	r, l := newAskRegistry(t)
	token := r.BeginTurn()
	a := mustOpen(t, r, t.Context(), token, permReq())
	b := mustOpen(t, r, t.Context(), TurnToken{}, planReq())
	waited := waitOn(a)
	askEvents(t, l)

	r.Close()

	ends := askEndings(askEvents(t, l))
	if len(ends) != 2 {
		t.Fatalf("%d endings at close, want one for each open ask", len(ends))
	}
	assertEnding(t, ends[0], a.ID(), AskClosing, AskByClose)
	assertEnding(t, ends[1], b.ID(), AskClosing, AskByClose)
	assertResolved(t, <-waited, AskClosing, AskByClose)

	late, err := r.Open(t.Context(), token, questionReq())
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	u := oneEnding(t, askEvents(t, l))
	assertEnding(t, u, late.ID(), AskClosing, AskByClose)
	if u.Body == nil || u.Body.Question == nil {
		t.Fatalf("a refused open wrote %+v, want its body", u)
	}
	// Wait on a refused open returns at once, with that ending.
	assertResolved(t, late.Wait(), AskClosing, AskByClose)

	// A second Close is a no-op.
	r.Close()
	if evs := askEvents(t, l); len(evs) != 0 {
		t.Fatalf("a second Close wrote %s, want nothing", eventKinds(evs))
	}
}

// TestAskContextEndingResolvesItOnce is §3.6's "Open takes the asking call's
// own context": when that context ends the ask is cancelled by the call and the
// waiter returns at once — and an ending already made wins over a context that
// ends with it, so a decision is never thrown away for a deadline.
func TestAskContextEndingResolvesItOnce(t *testing.T) {
	t.Run("the context ends while it is parked", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()
		ctx, cancel := context.WithCancel(t.Context())
		a := mustOpen(t, r, t.Context(), token, permReq())
		b, err := r.Open(ctx, token, permReq())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		waited := waitOn(b)
		askEvents(t, l)

		cancel()

		assertResolved(t, <-waited, AskCancelled, AskByCall)
		assertEnding(t, oneEnding(t, askEvents(t, l)), b.ID(), AskCancelled, AskByCall)
		// Only its own ask: a context is one call's, not the session's.
		if got := r.Asks(); len(got) != 1 || got[0].ID != a.ID() {
			t.Fatalf("Asks() is %+v, want the other ask still open", got)
		}
		if _, err := r.Answer("", b.ID(), AskAnswer{Cancel: true}); !errors.Is(err, ErrAlreadyResolved) {
			t.Fatalf("answering the cancelled ask returned %v, want ErrAlreadyResolved", err)
		}
	})

	t.Run("an answer that was already made wins", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()
		ctx, cancel := context.WithCancel(t.Context())
		a, err := r.Open(ctx, token, permReq())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := r.Answer("", a.ID(), AskAnswer{OptionID: "allow-once"}); err != nil {
			t.Fatalf("Answer: %v", err)
		}
		cancel()
		// Both are ready when Wait runs; the answer is what it must return.
		rec := a.Wait()
		assertResolved(t, rec, AskAnswered, AskByClient)
		if rec.Answer.OptionID != "allow-once" {
			t.Fatalf("Wait returned %+v, want the option that was chosen", rec.Answer)
		}
		evs := askEvents(t, l)
		if len(askEndings(evs)) != 1 {
			t.Fatalf("the cancelled context added an ending: %s", eventKinds(evs))
		}
	})
}

// TestAskCostsNoGoroutine is §3.6's "no goroutine may leak per ask": the
// asking call's context is watched inside Wait, on the waiting call's own
// goroutine. What follows from that is also pinned here — an ask nobody waits
// on is not resolved by its context, and is bounded by the turn's lifecycle
// instead.
func TestAskCostsNoGoroutine(t *testing.T) {
	l := newTestLog(t, EventLogOptions{NoPrimary: true})
	r := NewAskRegistry(l, func() time.Time { return askTestTime })
	token := r.BeginTurn()

	// The first open starts the log's drainer, which is the one goroutine any
	// of this adds; the baseline is taken after it so the count is exact.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := mustOpen(t, r, ctx, token, permReq())
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	baseline := runtime.NumGoroutine()

	const asks = 64
	waited := make([]*Ask, 0, asks/2)
	unwaited := make([]*Ask, 0, asks/2)
	for i := range asks {
		a := mustOpen(t, r, ctx, token, permReq())
		if i%2 == 0 {
			waited = append(waited, a)
		} else {
			unwaited = append(unwaited, a)
		}
	}
	if got := runtime.NumGoroutine(); got > baseline {
		t.Fatalf("%d goroutines after %d more asks, up from %d: an ask must cost none", got, asks, baseline)
	}

	answers := make([]<-chan AskRecord, 0, len(waited)+1)
	for _, a := range append([]*Ask{first}, waited...) {
		answers = append(answers, waitOn(a))
	}
	cancel()
	for i, ch := range answers {
		select {
		case rec := <-ch:
			assertResolved(t, rec, AskCancelled, AskByCall)
		case <-time.After(logWatchdog):
			t.Fatalf("waiter %d never returned after its context ended", i)
		}
	}
	settleGoroutines(t, "internal/agent.(*Ask).Wait(", 0)

	// The asks nobody waited on are still open: the context is watched by the
	// waiter, so what bounds an abandoned ask is the turn, not the context.
	if got := len(r.Asks()); got != len(unwaited) {
		t.Fatalf("%d asks open, want the %d nobody waited on", got, len(unwaited))
	}
	r.EndTurn(token)
	if got := r.Asks(); len(got) != 0 {
		t.Fatalf("%d asks survived their turn's end", len(got))
	}
}

// TestAskRefusedOpenStillEnds is A-X4's refusal rows: every refusal mints an
// id and writes exactly one self-contained ending — no opening before it, the
// body on it — and its Wait returns at once, so the handler that asked replies
// to the provider from the same record as any other.
func TestAskRefusedOpenStillEnds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(r *AskRegistry) TurnToken
		outcome AskOutcome
		by      string
	}{
		{"a turn that has ended", func(r *AskRegistry) TurnToken {
			tok := r.BeginTurn()
			r.EndTurn(tok)
			return tok
		}, AskTurnEnded, AskByTurn},
		{"a turn that was cancelled", func(r *AskRegistry) TurnToken {
			tok := r.BeginTurn()
			r.CancelTurn(tok)
			return tok
		}, AskCancelled, AskByCancel},
		{"a registry that has closed", func(r *AskRegistry) TurnToken {
			tok := r.BeginTurn()
			r.Close()
			return tok
		}, AskClosing, AskByClose},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, l := newAskRegistry(t)
			token := tc.setup(r)
			askEvents(t, l)

			a, err := r.Open(t.Context(), token, permReq())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			evs := askEvents(t, l)
			if len(evs) != 1 {
				t.Fatalf("a refused open wrote %s, want one ending and no opening", eventKinds(evs))
			}
			u := oneEnding(t, evs)
			assertEnding(t, u, a.ID(), tc.outcome, tc.by)
			if u.Body == nil || u.Body.Permission == nil || len(u.Body.Permission.Options) != len(askOptions) {
				t.Fatalf("the ending is %+v, want the options exactly as they were offered", u)
			}
			if u.Body.Permission.ID != a.ID() {
				t.Fatalf("the body's id is %q, want the minted %q", u.Body.Permission.ID, a.ID())
			}
			assertResolved(t, a.Wait(), tc.outcome, tc.by)
			if len(r.Asks()) != 0 {
				t.Fatalf("a refused ask is open: %+v", r.Asks())
			}
		})
	}
}

// TestAskSaturationRefusesOpenAndAnswerAndStillEnds is A5 for the registry:
// with the outbox over its bound, Open and Answer refuse having changed
// nothing — the provider sees a cancel — while an ask that is already open
// still ends, because an ending is a mandatory completion.
func TestAskSaturationRefusesOpenAndAnswerAndStillEnds(t *testing.T) {
	l := newTestLog(t, EventLogOptions{})
	l.outboxMaxEvents = 1
	sending, atSend := sendingAt(primaryCap + 1)
	l.hooks = &logHooks{outboxSending: sending}
	fillPrimary(t, l)
	r := NewAskRegistry(l, func() time.Time { return askTestTime })
	token := r.BeginTurn()

	open := mustOpen(t, r, t.Context(), token, permReq())
	await(t, atSend, "the drainer to block on the full primary with the opening")
	if l.OutboxRoom() {
		t.Fatal("OutboxRoom is true at a bound of one event with the opening in the outbox")
	}

	refused, err := r.Open(t.Context(), token, planReq())
	if err != nil {
		t.Fatalf("Open with no room: %v", err)
	}
	assertResolved(t, refused.Wait(), AskCancelled, AskByUnavailable)

	if _, err := r.Answer("", open.ID(), AskAnswer{OptionID: "allow-once"}); !errors.Is(err, ErrAskUnavailable) {
		t.Fatalf("Answer with no room returned %v, want ErrAskUnavailable", err)
	}
	if got := r.Asks(); len(got) != 1 || got[0].ID != open.ID() || got[0].Status != AskOpen {
		t.Fatalf("a refused answer changed something: %+v", got)
	}

	// An open ask still ends, whatever the outbox holds.
	r.CancelTurn(token)

	const want = primaryCap + 3
	primary := collectPrimary(l, want)
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	evs := await(t, primary, "the primary's events")[primaryCap:]
	if evs[0].Type != EventPermission {
		t.Fatalf("the record starts %s, want the opening that was admitted", eventKinds(evs))
	}
	ends := askEndings(evs)
	if len(ends) != 2 {
		t.Fatalf("the record is %s, want the refusal's ending and the cancel's", eventKinds(evs))
	}
	assertEnding(t, ends[0], refused.ID(), AskCancelled, AskByUnavailable)
	if ends[0].Body == nil || ends[0].Body.Plan == nil {
		t.Fatalf("the refusal's ending is %+v, want its body", ends[0])
	}
	assertEnding(t, ends[1], open.ID(), AskCancelled, AskByCancel)
	if ends[1].Body != nil {
		t.Fatalf("an ask whose opening was published carries its body again: %+v", ends[1])
	}
}

// TestAskAutomaticResolutions is A-X4's policy rows: a permission the policy
// allows writes no opening and one automatic ending with the body — so a forced
// `craze prompt --json` run gains no permission line — while a question and a
// plan write the Auto opening today's headless path already writes and one
// automatic ending, in that order, as one batch.
func TestAskAutomaticResolutions(t *testing.T) {
	t.Run("a permission the policy allowed", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()

		rec := r.Automatic(token, permReq(), AskAnswer{OptionID: "allow-once"})
		assertResolved(t, rec, AskAutomatic, AskByPolicy)
		if rec.ID != "perm-1" {
			t.Fatalf("the automatic ask is %q, want perm-1", rec.ID)
		}

		evs := askEvents(t, l)
		if len(evs) != 1 {
			t.Fatalf("the policy wrote %s, want one ending and no opening", eventKinds(evs))
		}
		u := oneEnding(t, evs)
		assertEnding(t, u, "perm-1", AskAutomatic, AskByPolicy)
		if u.OptionID != "allow-once" || u.Label != "Allow" {
			t.Fatalf("the ending is %+v, want the option it chose and its name", u)
		}
		if u.Body == nil || u.Body.Permission == nil || u.Body.Permission.ID != "perm-1" {
			t.Fatalf("the ending is %+v, want the opening nobody saw", u)
		}
	})

	t.Run("a permission with no option the policy could choose", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()
		req := AskRequest{Kind: AskPermission, Body: AskBody{Permission: &PermissionEvent{Tool: "Edit a.go"}}}

		rec := r.Automatic(token, req, AskAnswer{Cancel: true})
		assertResolved(t, rec, AskCancelled, AskByPolicy)

		u := oneEnding(t, askEvents(t, l))
		assertEnding(t, u, "perm-1", AskCancelled, AskByPolicy)
		if u.Body == nil || u.Body.Permission == nil {
			t.Fatalf("the ending is %+v, want the opening nobody saw", u)
		}
	})

	t.Run("a question the policy answered", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()
		answers := map[string][]string{"q1": {"opt-a"}, "q2": {""}}

		rec := r.Automatic(token, questionReq(), AskAnswer{Answers: answers})
		assertResolved(t, rec, AskAutomatic, AskByPolicy)

		evs := askEvents(t, l)
		if len(evs) != 2 || evs[0].Type != EventQuestion || evs[1].Type != EventAsk {
			t.Fatalf("the policy wrote %s, want the auto opening then its ending", eventKinds(evs))
		}
		q := evs[0].Question
		if q.ID != "ask-1" || !q.Auto || len(q.Answers) != 2 || q.Answers["q1"][0] != "opt-a" {
			t.Fatalf("the opening is %+v, want today's Auto shape with the answers craze sent", q)
		}
		u := evs[1].Ask
		assertEnding(t, u, "ask-1", AskAutomatic, AskByPolicy)
		if u.Body != nil {
			t.Fatalf("the ending carries a body although its opening was published: %+v", u.Body)
		}
		if len(u.Answers) != 2 {
			t.Fatalf("the ending is %+v, want the answers on it too", u)
		}
	})

	t.Run("a plan the policy accepted", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()

		rec := r.Automatic(token, planReq(), AskAnswer{Accept: true})
		assertResolved(t, rec, AskAutomatic, AskByPolicy)

		evs := askEvents(t, l)
		if len(evs) != 2 || evs[0].Type != EventPlan || evs[1].Type != EventAsk {
			t.Fatalf("the policy wrote %s, want the auto opening then its ending", eventKinds(evs))
		}
		p := evs[0].Plan
		if p.ID != "plan-1" || !p.Auto || !p.Accepted {
			t.Fatalf("the opening is %+v, want today's Auto shape", p)
		}
		u := evs[1].Ask
		assertEnding(t, u, "plan-1", AskAutomatic, AskByPolicy)
		if !u.Accepted || u.Body != nil {
			t.Fatalf("the ending is %+v, want accepted and no body", u)
		}
	})

	t.Run("an answer the policy could not make is a cancel", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()

		rec := r.Automatic(token, permReq(), AskAnswer{OptionID: "no-such-option"})
		assertResolved(t, rec, AskCancelled, AskByPolicy)

		evs := askEvents(t, l)
		u := oneEnding(t, evs)
		if u.OptionID != "" || u.Body == nil {
			t.Fatalf("the ending is %+v, want a cancel carrying the body", u)
		}
	})
}

// TestAskAnsweredEarlyIsOneSelfContainedEnding is A-X4's "ACP answers before
// any handler runs" row: the request is recorded with its body and no opening,
// so a card is never raised for something nobody can answer (panel: astra 12).
func TestAskAnsweredEarlyIsOneSelfContainedEnding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome AskOutcome
	}{
		{"a cancel that completed it where it lay", AskCancelled},
		{"a request from a turn that had already gone", AskTurnEnded},
		{"a connection that closed", AskClosing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, l := newAskRegistry(t)
			token := r.BeginTurn()

			rec := r.AnsweredEarly(token, permReq(), tc.outcome, AskByProvider)
			assertResolved(t, rec, tc.outcome, AskByProvider)

			evs := askEvents(t, l)
			if len(evs) != 1 {
				t.Fatalf("an early answer wrote %s, want one ending and no opening", eventKinds(evs))
			}
			u := oneEnding(t, evs)
			assertEnding(t, u, rec.ID, tc.outcome, AskByProvider)
			if u.Body == nil || u.Body.Permission == nil || u.Body.Permission.Tool != "Edit a.go" {
				t.Fatalf("the ending is %+v, want the request it stands for", u)
			}
			if len(r.Asks()) != 0 {
				t.Fatalf("an early answer parked something: %+v", r.Asks())
			}
		})
	}
}

// TestAskReportRecordsDeliveryAndNotesALoss is astra 13: taking the decision
// and delivering it are two facts, and a decision that lost its race to ACP's
// own cancelled reply is one journal note — never a second ending, and never a
// changed outcome.
func TestAskReportRecordsDeliveryAndNotesALoss(t *testing.T) {
	l, w := newJournaledLog(t, EventLogOptions{})
	keepDrained(t, l)
	r := NewAskRegistry(l, func() time.Time { return askTestTime })
	token := r.BeginTurn()

	delivered := mustOpen(t, r, t.Context(), token, permReq())
	lost := mustOpen(t, r, t.Context(), token, permReq())
	for _, a := range []*Ask{delivered, lost} {
		if _, err := r.Answer("", a.ID(), AskAnswer{OptionID: "allow-once"}); err != nil {
			t.Fatalf("Answer(%s): %v", a.ID(), err)
		}
		if rec := a.Wait(); !rec.Consumed {
			t.Fatalf("%s: Wait did not record that the decision was taken", a.ID())
		}
	}

	r.Report(delivered.ID(), AskReport{Delivered: true})
	r.Report(lost.ID(), AskReport{Lost: "the cancelled reply won"})

	rec, ok := r.Record(delivered.ID())
	if !ok || !rec.Delivered || rec.Lost != "" || !rec.Consumed {
		t.Fatalf("the delivered ask is %+v, want consumed and delivered", rec)
	}
	rec, ok = r.Record(lost.ID())
	if !ok || rec.Delivered || rec.Lost == "" {
		t.Fatalf("the lost ask is %+v, want a loss and no delivery", rec)
	}
	if rec.Outcome != AskAnswered {
		t.Fatalf("a lost delivery changed the outcome to %s", rec.Outcome)
	}

	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	closeLog(t, l, w)
	lines := fileLines(t, w)
	notes := diags(lines, "ask_lost_delivery")
	if len(notes) != 1 {
		t.Fatalf("%d ask_lost_delivery notes, want exactly 1", len(notes))
	}
	if notes[0]["id"] != lost.ID() || notes[0]["reason"] != "the cancelled reply won" || notes[0]["outcome"] != string(AskAnswered) {
		t.Fatalf("the note is %v, want the ask, its outcome and why the reply was lost", notes[0])
	}
	// One ending each, and nothing the report added.
	var endings int
	for _, line := range lines {
		if line["type"] == "event" && line["eventType"] == string(EventAsk) {
			endings++
		}
	}
	if endings != 2 {
		t.Fatalf("%d endings in the journal, want one per ask: a lost delivery is a note, not an ending", endings)
	}
}

// TestAskRecordsAreCopies pins §3.6's "copies, never internal pointers": what
// a caller does to a record it was handed cannot reach the registry, the
// options it holds, or the record another caller reads.
func TestAskRecordsAreCopies(t *testing.T) {
	r, l := newAskRegistry(t)
	token := r.BeginTurn()
	a := mustOpen(t, r, t.Context(), token, permReq())
	askEvents(t, l)

	got := r.Asks()
	got[0].Body.Permission.Options[0].OptionID = "tampered"
	got[0].Body.Permission.Tool = "tampered"
	if rec, _ := r.Record(a.ID()); rec.Body.Permission.Options[0].OptionID != "allow-once" || rec.Body.Permission.Tool != "Edit a.go" {
		t.Fatalf("a caller's edit reached the registry: %+v", rec.Body.Permission)
	}
	// The body the caller handed to Open is its own, too.
	req := questionReq()
	b := mustOpen(t, r, t.Context(), token, req)
	req.Body.Question.Title = "tampered"
	if rec, _ := r.Record(b.ID()); rec.Body.Question.Title != "Question" {
		t.Fatalf("the registry kept the caller's own value: %+v", rec.Body.Question)
	}
	if req.Body.Question.ID != "" {
		t.Fatal("Open stamped its id onto the caller's own payload")
	}

	if _, err := r.Answer("", b.ID(), AskAnswer{Answers: map[string][]string{"q1": {"opt-a"}}}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	rec, _ := r.Record(b.ID())
	rec.Answer.Answers["q1"][0] = "tampered"
	if again, _ := r.Record(b.ID()); again.Answer.Answers["q1"][0] != "opt-a" {
		t.Fatalf("a caller's edit reached the recorded answer: %+v", again.Answer)
	}
}

// TestAskOpeningsPrecedeTheirEndingsUnderConcurrency is what "every registry
// event goes through Enqueue under registry.mu" is for: with asks opening,
// being answered, and being cancelled from several goroutines at once, the
// committed record has each opening before its own ending and exactly one
// ending per ask. Run under -race it also pins that the registry is safe from
// any goroutine.
func TestAskOpeningsPrecedeTheirEndingsUnderConcurrency(t *testing.T) {
	// NoPrimary: this writes more events than a primary nobody reads holds, and
	// a subscription is what reads the committed order.
	l := newTestLog(t, EventLogOptions{NoPrimary: true})
	r := NewAskRegistry(l, func() time.Time { return askTestTime })
	sub := mustSubscribe(t, l, SubscribeOptions{MaxItems: 4096, MaxBytes: 4 << 20})

	const openers, each = 6, 12
	var wg sync.WaitGroup
	token := r.BeginTurn()
	for i := range openers {
		wg.Go(func() {
			for j := range each {
				a, err := r.Open(t.Context(), token, permReq())
				if err != nil {
					t.Errorf("Open: %v", err)
					return
				}
				if j%2 == 0 {
					// Half are answered; the rest are left for the cancel.
					if _, err := r.Answer("", a.ID(), AskAnswer{OptionID: "allow-once"}); err != nil &&
						!errors.Is(err, ErrAlreadyResolved) {
						t.Errorf("Answer: %v", err)
						return
					}
				}
				if i == 0 && j%4 == 3 {
					r.CancelTurn(TurnToken{})
				}
			}
		})
	}
	waitDone(t, &wg)
	r.CancelTurn(token)
	if err := flushNow(t, l); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := r.Asks(); len(got) != 0 {
		t.Fatalf("%d asks survived the cancel", len(got))
	}

	// A sentinel the subscription is read up to, so the test waits for a
	// record rather than for a duration.
	if !l.Publish(context.Background(), nil, textEvent("end")) {
		t.Fatal("the sentinel was not published")
	}
	openedAt, endedAt := map[string]uint64{}, map[string]uint64{}
	deadline := time.After(logWatchdog)
	for done := false; !done; {
		select {
		case rec, ok := <-sub.Records():
			if !ok {
				t.Fatalf("the subscription ended: %v", sub.Err())
			}
			ev, err := rec.Event()
			if err != nil {
				t.Fatalf("seq %d: %v", rec.Seq, err)
			}
			switch {
			case ev.Type == EventPermission:
				if _, twice := openedAt[ev.Permission.ID]; twice {
					t.Fatalf("%s was opened twice", ev.Permission.ID)
				}
				openedAt[ev.Permission.ID] = rec.Seq
			case ev.Type == EventAsk:
				if _, twice := endedAt[ev.Ask.ID]; twice {
					t.Fatalf("%s ended twice", ev.Ask.ID)
				}
				endedAt[ev.Ask.ID] = rec.Seq
				if at, opened := openedAt[ev.Ask.ID]; opened && at > rec.Seq {
					t.Fatalf("%s ended at seq %d, before its opening at %d", ev.Ask.ID, rec.Seq, at)
				}
				if _, opened := openedAt[ev.Ask.ID]; !opened && ev.Ask.Body == nil {
					t.Fatalf("%s ended with no opening and no body", ev.Ask.ID)
				}
			case ev.Text == "end":
				done = true
			}
		case <-deadline:
			t.Fatalf("the sentinel never arrived: %d openings, %d endings", len(openedAt), len(endedAt))
		}
	}
	if len(endedAt) != openers*each {
		t.Fatalf("%d endings, want one per ask (%d)", len(endedAt), openers*each)
	}
	for id := range openedAt {
		if _, ended := endedAt[id]; !ended {
			t.Fatalf("%s was opened and never ended", id)
		}
	}
}

// TestAskRacingAnswersLeaveOneEnding is A-X3: two clients answering one ask at
// the same instant give one nil, one ErrAlreadyResolved, and exactly one
// ending.
func TestAskRacingAnswersLeaveOneEnding(t *testing.T) {
	for range 32 {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()
		a := mustOpen(t, r, t.Context(), token, permReq())
		askEvents(t, l)

		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for _, option := range []string{"allow-once", "reject-once"} {
			wg.Go(func() {
				<-start
				_, err := r.Answer("", a.ID(), AskAnswer{OptionID: option})
				errs <- err
			})
		}
		close(start)
		waitDone(t, &wg)
		close(errs)

		var ok, already int
		for err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrAlreadyResolved):
				already++
			default:
				t.Fatalf("Answer returned %v, want nil or ErrAlreadyResolved", err)
			}
		}
		if ok != 1 || already != 1 {
			t.Fatalf("%d answers accepted and %d refused, want exactly one of each", ok, already)
		}
		if u := oneEnding(t, askEvents(t, l)); u.Outcome != AskAnswered {
			t.Fatalf("the ending is %+v, want one answered", u)
		}
	}
}

// TestEffectiveApproval pins that a nil policy reproduces exactly today's
// behaviour — the ApprovalPolicy half of A13 — and that an explicit one wins
// with an empty section reading as ask.
func TestEffectiveApproval(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want ApprovalPolicy
	}{
		{"a TUI session", Options{Interactive: true},
			ApprovalPolicy{ApprovalAsk, ApprovalAsk, ApprovalAsk}},
		{"a TUI session with --force", Options{Interactive: true, Force: true},
			ApprovalPolicy{ApprovalAllow, ApprovalAsk, ApprovalAsk}},
		{"headless craze prompt, which forces by default", Options{Force: true},
			ApprovalPolicy{ApprovalAllow, ApprovalFirstOption, ApprovalAccept}},
		{"headless craze prompt --no-force, which parks its permissions", Options{},
			ApprovalPolicy{ApprovalAsk, ApprovalFirstOption, ApprovalAccept}},
		{"an explicit policy, which Force no longer decides", Options{Force: true, Approval: &ApprovalPolicy{
			Permission: ApprovalAsk, Question: ApprovalAsk, Plan: ApprovalAccept}},
			ApprovalPolicy{ApprovalAsk, ApprovalAsk, ApprovalAccept}},
		{"an explicit policy with a section left empty", Options{Interactive: true, Approval: &ApprovalPolicy{
			Permission: ApprovalAllow}},
			ApprovalPolicy{ApprovalAllow, ApprovalAsk, ApprovalAsk}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveApproval(tc.opts); got != tc.want {
				t.Fatalf("EffectiveApproval is %+v, want %+v", got, tc.want)
			}
		})
	}
}
