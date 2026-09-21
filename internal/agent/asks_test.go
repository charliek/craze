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
// fixtures rest on, and the rule that decides which of a kind's two counters an
// id comes from (asks.go's hiddenMark): an ask whose opening IS published
// spends a visible number, and one whose opening is never published spends a
// hidden one. The baseline spent a number in exactly the first case — park
// called nextID only once it had decided to park, and Force never called it at
// all — so anything else renumbers the cards a user and `--json` see (review
// r17, finding 8).
func TestAskIDsAreNumberedPerKind(t *testing.T) {
	r, l := newAskRegistry(t)
	token := r.BeginTurn()

	first := mustOpen(t, r, t.Context(), token, permReq())
	question := mustOpen(t, r, t.Context(), token, questionReq())
	plan := mustOpen(t, r, t.Context(), token, planReq())
	if first.ID() != "perm-1" || question.ID() != "ask-1" || plan.ID() != "plan-1" {
		t.Fatalf("ids %q %q %q, want perm-1 ask-1 plan-1", first.ID(), question.ID(), plan.ID())
	}
	// A permission the policy answered publishes no opening, so it is hidden.
	if rec := r.Automatic(token, permReq(), AskAnswer{OptionID: "allow-once"}); rec.ID != "perm-x1" {
		t.Fatalf("the automatic permission is %q, want perm-x1: it raises no card", rec.ID)
	}
	// A question the policy answered publishes its Auto opening, so it is
	// visible — exactly the line today's headless `--json` run prints.
	if rec := r.Automatic(token, questionReq(), AskAnswer{Answers: map[string][]string{"q1": {"opt-a"}}}); rec.ID != "ask-2" {
		t.Fatalf("the automatic question is %q, want ask-2: its Auto opening is published", rec.ID)
	}
	// A refused open publishes nothing either: this one arrives for a turn that
	// has ended.
	ended := r.BeginTurn()
	r.EndTurn(ended)
	refused, err := r.Open(t.Context(), ended, permReq())
	if err != nil {
		t.Fatalf("Open on an ended turn: %v", err)
	}
	if refused.ID() != "perm-x2" {
		t.Fatalf("the refused ask is %q, want perm-x2: a refusal spends a hidden number", refused.ID())
	}
	// Nor does a request the provider answered before any handler ran.
	if rec := r.AnsweredEarly(token, planReq(), AskCancelled, AskByProvider); rec.ID != "plan-x1" {
		t.Fatalf("the early-answered ask is %q, want plan-x1", rec.ID)
	}
	// None of which moved the visible counter: the next card a user sees is
	// numbered as if the hidden ones had never happened.
	next := mustOpen(t, r, t.Context(), token, permReq())
	if next.ID() != "perm-2" {
		t.Fatalf("the next permission is %q, want perm-2: a hidden ask spends no visible number", next.ID())
	}
	askEvents(t, l)
}

// TestAskHiddenRecordsDoNotRenumberVisibleOnes is r17 finding 8's own schedule,
// at the registry: a question delayed until ACP called it stale is recorded
// between two ordinary ones, and because its EventAsk is all there is of it —
// and `--json` drops EventAsk — the two a user really sees must still be ask-1
// and ask-2.
func TestAskHiddenRecordsDoNotRenumberVisibleOnes(t *testing.T) {
	r, l := newAskRegistry(t)
	token := r.BeginTurn()

	firstSeen := r.Automatic(token, questionReq(), AskAnswer{Answers: map[string][]string{"q1": {"opt-a"}}})
	// The delayed one: ACP answered it where it lay, so no card was raised.
	hidden := r.AnsweredEarly(token, questionReq(), AskTurnEnded, AskByProvider)
	secondSeen := r.Automatic(token, questionReq(), AskAnswer{Answers: map[string][]string{"q1": {"opt-b"}}})

	if firstSeen.ID != "ask-1" || secondSeen.ID != "ask-2" {
		t.Fatalf("the questions a user sees are %q and %q, want ask-1 and ask-2", firstSeen.ID, secondSeen.ID)
	}
	if hidden.ID != "ask-x1" {
		t.Fatalf("the invisible question is %q, want ask-x1", hidden.ID)
	}
	// And the published openings — the only lines --json prints — carry those
	// two ids and no other.
	var openings []string
	for _, ev := range askEvents(t, l) {
		if ev.Type == EventQuestion {
			openings = append(openings, ev.Question.ID)
		}
	}
	if len(openings) != 2 || openings[0] != "ask-1" || openings[1] != "ask-2" {
		t.Fatalf("the openings are %v, want [ask-1 ask-2]", openings)
	}
}

// TestAskAdoptedAndMintedIDsShareOneNamespace is review r16's finding 1 on all
// four creation paths: minting may never land on an id an open ask already
// holds, however that ask came by it. Before the fix only an explicit req.ID
// was checked, so a later mint overwrote the adopted entry in the open map and
// stranded its waiter for ever.
func TestAskAdoptedAndMintedIDsShareOneNamespace(t *testing.T) {
	// Every case adopts the id the counter is about to reach, then creates an
	// ask the other way; the adopted ask must come through untouched and the
	// new id must be the number after it.
	for _, tc := range []struct {
		name string
		// create makes one ask that does NOT adopt, and answers with its id.
		create func(t *testing.T, r *AskRegistry) string
		// want is the id it must get with ask-1 adopted and open.
		want string
	}{
		{"a minted Open", func(t *testing.T, r *AskRegistry) string {
			return mustOpen(t, r, t.Context(), r.BeginTurn(), questionReq()).ID()
		}, "ask-2"},
		{"a refused Open", func(t *testing.T, r *AskRegistry) string {
			// A hidden id, which cannot collide with a visible one at all —
			// but the skip is still the rule that has to hold for it.
			ended := r.BeginTurn()
			r.EndTurn(ended)
			a, err := r.Open(t.Context(), ended, questionReq())
			if err != nil {
				t.Fatalf("Open on an ended turn: %v", err)
			}
			return a.ID()
		}, "ask-x1"},
		{"Automatic", func(t *testing.T, r *AskRegistry) string {
			return r.Automatic(r.BeginTurn(), questionReq(),
				AskAnswer{Answers: map[string][]string{"q1": {"opt-a"}}}).ID
		}, "ask-2"},
		{"AnsweredEarly", func(t *testing.T, r *AskRegistry) string {
			return r.AnsweredEarly(r.BeginTurn(), questionReq(), AskCancelled, AskByProvider).ID
		}, "ask-x1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestLog(t, EventLogOptions{NoPrimary: true})
			r := NewAskRegistry(l, func() time.Time { return askTestTime })
			token := r.BeginTurn()

			// An id that LOOKS minted, adopted while the counter is at zero —
			// which is exactly what tui.Stub's tests choose (ask-1, perm-1).
			adopted := mustOpen(t, r, t.Context(), token, AskRequest{
				Kind: AskQuestion, ID: "ask-1", Body: questionReq().Body})
			waited := waitOn(adopted)

			if got := tc.create(t, r); got != tc.want {
				t.Fatalf("the new ask is %q, want %q: it must not land on an open ask's id", got, tc.want)
			}
			// The adopted ask is still the one that id names, still open, and
			// still the ask its waiter is waiting on.
			rec, kept := r.Record("ask-1")
			if !kept || rec.Status != AskOpen {
				t.Fatalf("the adopted ask is %+v (kept %v), want it still open", rec, kept)
			}
			if !slices.ContainsFunc(r.Asks(), func(x AskRecord) bool { return x.ID == "ask-1" }) {
				t.Fatalf("Asks() is %+v, want the adopted ask among them", r.Asks())
			}
			select {
			case rec := <-waited:
				t.Fatalf("the adopted ask's waiter returned %+v; it was never resolved", rec)
			default:
			}
			if _, err := r.Answer("", "ask-1", AskAnswer{Skip: true}); err != nil {
				t.Fatalf("answering the adopted ask: %v", err)
			}
			assertResolved(t, <-waited, AskAnswered, AskByClient)
		})
	}
}

// TestAskAdoptionNeverStrandsAWaiter is finding 1's own schedule, end to end:
// with the counter at 1, adopt ask-3, wait on it, and go on minting. Before the
// fix the mint that reached 3 overwrote open["ask-3"], every later resolution
// reached the replacement, and not even cancelling the waiter's context could
// release it — cancelByCall rejected the entry-pointer mismatch and Wait
// blocked on a channel nothing would ever close.
//
// Now the adoption takes 3 out of the question counter as it happens
// (accountForLocked), so no mint can reach ask-3 at all, whether or not that
// ask is still open.
func TestAskAdoptionNeverStrandsAWaiter(t *testing.T) {
	l := newTestLog(t, EventLogOptions{NoPrimary: true})
	r := NewAskRegistry(l, func() time.Time { return askTestTime })
	token := r.BeginTurn()

	if got := mustOpen(t, r, t.Context(), token, questionReq()).ID(); got != "ask-1" {
		t.Fatalf("the first minted question is %q, want ask-1", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stranded, err := r.Open(ctx, token, AskRequest{Kind: AskQuestion, ID: "ask-3", Body: questionReq().Body})
	if err != nil {
		t.Fatalf("adopting ask-3: %v", err)
	}
	waited := waitOn(stranded)

	minted := []string{
		mustOpen(t, r, t.Context(), token, questionReq()).ID(),
		mustOpen(t, r, t.Context(), token, questionReq()).ID(),
		mustOpen(t, r, t.Context(), token, questionReq()).ID(),
	}
	if slices.Contains(minted, "ask-3") {
		t.Fatalf("a mint landed on the adopted ask-3: %v", minted)
	}
	if minted[0] != "ask-4" || minted[1] != "ask-5" || minted[2] != "ask-6" {
		t.Fatalf("the mints are %v, want ask-4 ask-5 ask-6: the adoption spent 3", minted)
	}

	// The displaced waiter is not displaced: its own context still releases it.
	cancel()
	select {
	case rec := <-waited:
		assertResolved(t, rec, AskCancelled, AskByCall)
		if rec.ID != "ask-3" {
			t.Fatalf("the waiter woke on %q, want its own ask-3", rec.ID)
		}
	case <-time.After(logWatchdog):
		t.Fatal("the adopted ask's waiter was stranded: its context never released it")
	}
	// And ask-3 may be adopted again now that it has ended.
	if _, err := r.Open(t.Context(), token, AskRequest{
		Kind: AskQuestion, ID: "ask-3", Body: questionReq().Body}); err != nil {
		t.Fatalf("re-adopting a resolved id: %v", err)
	}
}

// TestAskAdoptedCanonicalIDsAreAccountedFor is the other half of one namespace:
// an adopted id that spells one this registry could have minted takes its
// number out of that counter, so nothing mints it later and the unknown-vs-
// evicted rule still knows it was issued (asks.go's accountForLocked).
func TestAskAdoptedCanonicalIDsAreAccountedFor(t *testing.T) {
	l := newTestLog(t, EventLogOptions{NoPrimary: true})
	r := NewAskRegistry(l, func() time.Time { return askTestTime })

	// Adopted well above the counter, then answered, so nothing is open.
	adopted := mustOpen(t, r, t.Context(), TurnToken{}, AskRequest{
		Kind: AskPermission, ID: "perm-4", Body: permReq().Body})
	if _, err := r.Answer("", adopted.ID(), AskAnswer{Cancel: true}); err != nil {
		t.Fatalf("Answer(perm-4): %v", err)
	}
	// The next mint is past it, although perm-4 is no longer open.
	if got := mustOpen(t, r, t.Context(), TurnToken{}, permReq()).ID(); got != "perm-5" {
		t.Fatalf("the next minted permission is %q, want perm-5: perm-4 was issued", got)
	}
	// A hidden adoption is accounted for in the hidden counter, and neither
	// counter can see the other's.
	early := r.AnsweredEarly(TurnToken{}, permReq(), AskCancelled, AskByProvider)
	if early.ID != "perm-x1" {
		t.Fatalf("the first hidden id is %q, want perm-x1: a visible adoption is not its business", early.ID)
	}
	hidden := mustOpen(t, r, t.Context(), TurnToken{}, AskRequest{
		Kind: AskPermission, ID: "perm-x6", Body: permReq().Body})
	if _, err := r.Answer("", hidden.ID(), AskAnswer{Cancel: true}); err != nil {
		t.Fatalf("Answer(perm-x6): %v", err)
	}
	if got := r.AnsweredEarly(TurnToken{}, permReq(), AskCancelled, AskByProvider).ID; got != "perm-x7" {
		t.Fatalf("the next hidden id is %q, want perm-x7", got)
	}
	// Both are known as issued once their records have been evicted, which is
	// what already_resolved rather than unknown_ask rests on.
	for range keptAsks {
		a := mustOpen(t, r, t.Context(), TurnToken{}, permReq())
		if _, err := r.Answer("", a.ID(), AskAnswer{Cancel: true}); err != nil {
			t.Fatalf("Answer(%s): %v", a.ID(), err)
		}
	}
	for _, id := range []string{"perm-4", "perm-x6"} {
		if _, kept := r.Record(id); kept {
			t.Fatalf("%s is still kept; the eviction this asserts about never happened", id)
		}
		if _, err := r.Answer("", id, AskAnswer{Cancel: true}); !errors.Is(err, ErrAlreadyResolved) {
			t.Fatalf("the evicted adopted id %s returned %v, want ErrAlreadyResolved", id, err)
		}
	}
}

// TestAskMintNeverLandsOnAnOpenID pins the mint's own half of one namespace,
// directly, because accountForLocked keeps a canonical adopted id out of its
// counter's way and so the two braces cannot both be undone from outside: the
// only ids a mint could otherwise reach are ones belonging to a kind the
// registry has no prefix for, and such a kind has no opening to publish either.
// The invariant is "a mint never returns an id an open ask holds", and the
// number it passed over is spent, never handed to a second ask.
func TestAskMintNeverLandsOnAnOpenID(t *testing.T) {
	l := newTestLog(t, EventLogOptions{NoPrimary: true})
	r := NewAskRegistry(l, func() time.Time { return askTestTime })

	r.mu.Lock()
	r.seq[AskQuestion] = 1
	r.open["ask-2"] = &askEntry{done: make(chan struct{})}
	r.open["ask-3"] = &askEntry{done: make(chan struct{})}
	got := r.mintLocked(AskQuestion, false)
	seq := r.seq[AskQuestion]
	r.mu.Unlock()

	if got != "ask-4" {
		t.Fatalf("the mint returned %q, want ask-4: ask-2 and ask-3 are open", got)
	}
	if seq != 4 {
		t.Fatalf("the counter is at %d, want 4: a skipped number is spent, not reused", seq)
	}
}

// TestAskNoncanonicalIDsAreUnknown is review r16's finding 6: strconv.Atoi
// accepts spellings craze never issues, and an id it never issued is
// unknown_ask however well it parses.
func TestAskNoncanonicalIDsAreUnknown(t *testing.T) {
	l := newTestLog(t, EventLogOptions{NoPrimary: true})
	r := NewAskRegistry(l, func() time.Time { return askTestTime })
	// The permission counters well past 7, visible and hidden both.
	for range 9 {
		a := mustOpen(t, r, t.Context(), TurnToken{}, permReq())
		if _, err := r.Answer("", a.ID(), AskAnswer{Cancel: true}); err != nil {
			t.Fatalf("Answer(%s): %v", a.ID(), err)
		}
		r.AnsweredEarly(TurnToken{}, permReq(), AskCancelled, AskByProvider)
	}
	if _, err := r.Answer("", "perm-7", AskAnswer{Cancel: true}); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("perm-7 returned %v, want ErrAlreadyResolved: it was issued", err)
	}
	for _, id := range []string{"perm-007", "perm-+7", "perm-7 ", "perm- 7", "perm-x007", "perm-x+7", "perm-0", "perm--7"} {
		if _, err := r.Answer("", id, AskAnswer{Cancel: true}); !errors.Is(err, ErrUnknownAsk) {
			t.Fatalf("%q returned %v, want ErrUnknownAsk: craze never spells an id that way", id, err)
		}
	}
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

// TestAskTokensAreScopedToTheirRegistry is review r16's finding 4: two
// registries both mint turn 1, and without the registry's identity on the token
// each would answer for the other's turn — B.Open would park against A's live
// state, and B.EndTurn(tokenFromA) would end B's own turn-1 asks.
func TestAskTokensAreScopedToTheirRegistry(t *testing.T) {
	newPair := func(t *testing.T) (*AskRegistry, *EventLog, *AskRegistry, *EventLog) {
		t.Helper()
		a, la := newAskRegistry(t)
		b, lb := newAskRegistry(t)
		return a, la, b, lb
	}

	t.Run("Open against a foreign token is refused as turn_ended", func(t *testing.T) {
		a, _, b, lb := newPair(t)
		foreign := a.BeginTurn()
		own := b.BeginTurn()
		if foreign == own {
			t.Fatal("two registries minted the same token: the identity is not on it")
		}

		ask, err := b.Open(t.Context(), foreign, permReq())
		if err != nil {
			t.Fatalf("Open with a foreign token: %v", err)
		}
		// Refused, although A's turn is live and B's own turn 1 is too.
		assertResolved(t, ask.Wait(), AskTurnEnded, AskByTurn)
		u := oneEnding(t, askEvents(t, lb))
		assertEnding(t, u, ask.ID(), AskTurnEnded, AskByTurn)
		if u.Body == nil {
			t.Fatalf("a refused open owes its body: %+v", u)
		}
		if len(b.Asks()) != 0 {
			t.Fatalf("a foreign token parked something: %+v", b.Asks())
		}
	})

	t.Run("EndTurn with a foreign token ends nothing", func(t *testing.T) {
		a, _, b, lb := newPair(t)
		foreign := a.BeginTurn()
		own := b.BeginTurn()
		mine := mustOpen(t, b, t.Context(), own, permReq())
		askEvents(t, lb)

		b.EndTurn(foreign)

		if evs := askEvents(t, lb); len(evs) != 0 {
			t.Fatalf("a foreign EndTurn wrote %s, want nothing", eventKinds(evs))
		}
		if got := b.Asks(); len(got) != 1 || got[0].ID != mine.ID() {
			t.Fatalf("Asks() is %+v, want this registry's own ask still open", got)
		}
		// And B's own turn 1 was not marked: a request for it still parks.
		later := mustOpen(t, b, t.Context(), own, questionReq())
		if rec, ok := b.Record(later.ID()); !ok || rec.Status != AskOpen {
			t.Fatalf("the turn's next ask is %+v, want it open", rec)
		}
	})

	t.Run("CancelTurn with a foreign token resolves nothing", func(t *testing.T) {
		a, _, b, lb := newPair(t)
		foreign := a.BeginTurn()
		own := b.BeginTurn()
		inTurn := mustOpen(t, b, t.Context(), own, permReq())
		between := mustOpen(t, b, t.Context(), TurnToken{}, questionReq())
		askEvents(t, lb)

		// Not even the "cancel everything" half: a token another session minted
		// says nothing about this one, and the no-turn token is how a caller
		// with no turn of its own asks for that.
		b.CancelTurn(foreign)

		if evs := askEvents(t, lb); len(evs) != 0 {
			t.Fatalf("a foreign CancelTurn wrote %s, want nothing", eventKinds(evs))
		}
		if got := b.Asks(); len(got) != 2 || got[0].ID != inTurn.ID() || got[1].ID != between.ID() {
			t.Fatalf("Asks() is %+v, want both asks still open", got)
		}
		// The no-turn token still means everything, as it always did.
		b.CancelTurn(TurnToken{})
		if len(askEndings(askEvents(t, lb))) != 2 {
			t.Fatalf("the no-turn cancel left %d asks open", len(b.Asks()))
		}
	})

	t.Run("A's own token still works on A", func(t *testing.T) {
		a, la, b, _ := newPair(t)
		token := a.BeginTurn()
		mine := mustOpen(t, a, t.Context(), token, permReq())
		askEvents(t, la)
		// B saw that token and did nothing with it; A's own lifecycle is
		// untouched.
		b.EndTurn(token)
		b.CancelTurn(token)
		a.EndTurn(token)
		assertEnding(t, oneEnding(t, askEvents(t, la)), mine.ID(), AskTurnEnded, AskByTurn)
	})
}

// TestAskRetiredTokensAreEvictedPastTheBound is keptTurns: a token keeps its
// exact ending for a bounded while, and past it reads as ended — which is what
// every retired token is. Only the choice between "cancelled" and "turn_ended"
// is lost.
func TestAskRetiredTokensAreEvictedPastTheBound(t *testing.T) {
	l := newTestLog(t, EventLogOptions{NoPrimary: true})
	r := NewAskRegistry(l, func() time.Time { return askTestTime })

	cancelled := r.BeginTurn()
	r.CancelTurn(cancelled)
	// Inside the bound the cancel is still what a late request is told.
	late, err := r.Open(t.Context(), cancelled, permReq())
	if err != nil {
		t.Fatalf("Open on a cancelled turn: %v", err)
	}
	assertResolved(t, late.Wait(), AskCancelled, AskByCancel)

	// keptTurns more retirements push it out.
	for range keptTurns {
		tok := r.BeginTurn()
		r.EndTurn(tok)
	}
	evicted, err := r.Open(t.Context(), cancelled, permReq())
	if err != nil {
		t.Fatalf("Open on an evicted turn: %v", err)
	}
	assertResolved(t, evicted.Wait(), AskTurnEnded, AskByTurn)

	// A turn that has not retired at all is still live, however many have.
	live := r.BeginTurn()
	open := mustOpen(t, r, t.Context(), live, permReq())
	if rec, ok := r.Record(open.ID()); !ok || rec.Status != AskOpen {
		t.Fatalf("the live turn's ask is %+v, want it open", rec)
	}
}

// TestAskCancelAndEndInBothOrders is A-X4's pair of turn endings against each
// other: whichever retires the token first decides what a late request for it
// is told, and the second one changes neither that nor any ask it finds.
func TestAskCancelAndEndInBothOrders(t *testing.T) {
	for _, tc := range []struct {
		name    string
		second  func(r *AskRegistry, token TurnToken)
		outcome AskOutcome
		by      string
	}{
		{"cancel, then end", func(r *AskRegistry, token TurnToken) { r.EndTurn(token) }, AskCancelled, AskByCancel},
		{"end, then cancel", func(r *AskRegistry, token TurnToken) { r.CancelTurn(token) }, AskTurnEnded, AskByTurn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, l := newAskRegistry(t)
			token := r.BeginTurn()
			a := mustOpen(t, r, t.Context(), token, permReq())
			waited := waitOn(a)
			askEvents(t, l)

			if tc.outcome == AskCancelled {
				r.CancelTurn(token)
			} else {
				r.EndTurn(token)
			}
			evs := askEvents(t, l)
			assertEnding(t, oneEnding(t, evs), a.ID(), tc.outcome, tc.by)
			assertResolved(t, <-waited, tc.outcome, tc.by)

			// The second notification finds nothing to do, and writes nothing.
			tc.second(r, token)
			if evs := askEvents(t, l); len(evs) != 0 {
				t.Fatalf("the second notification wrote %s, want nothing", eventKinds(evs))
			}
			// And the first retirement is still what a late request is told.
			late, err := r.Open(t.Context(), token, planReq())
			if err != nil {
				t.Fatalf("Open on a retired turn: %v", err)
			}
			assertResolved(t, late.Wait(), tc.outcome, tc.by)
		})
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
// automatic ending, in that order, as one batch. The ids follow: hidden for the
// one nobody sees, visible for the two that publish an opening (hiddenMark).
func TestAskAutomaticResolutions(t *testing.T) {
	t.Run("a permission the policy allowed", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()

		rec := r.Automatic(token, permReq(), AskAnswer{OptionID: "allow-once"})
		assertResolved(t, rec, AskAutomatic, AskByPolicy)
		if rec.ID != "perm-x1" {
			t.Fatalf("the automatic ask is %q, want perm-x1", rec.ID)
		}

		evs := askEvents(t, l)
		if len(evs) != 1 {
			t.Fatalf("the policy wrote %s, want one ending and no opening", eventKinds(evs))
		}
		u := oneEnding(t, evs)
		assertEnding(t, u, "perm-x1", AskAutomatic, AskByPolicy)
		if u.OptionID != "allow-once" || u.Label != "Allow" {
			t.Fatalf("the ending is %+v, want the option it chose and its name", u)
		}
		if u.Body == nil || u.Body.Permission == nil || u.Body.Permission.ID != "perm-x1" {
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
		assertEnding(t, u, "perm-x1", AskCancelled, AskByPolicy)
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

	t.Run("a question the policy could not answer publishes nothing and is hidden", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()

		// A degraded automatic resolution: no Auto opening is published, so —
		// like a forced permission — it takes a hidden number and cannot
		// renumber the questions a user sees (review r17, finding 8).
		rec := r.Automatic(token, questionReq(), AskAnswer{Answers: map[string][]string{"q9": {"opt-a"}}})
		assertResolved(t, rec, AskCancelled, AskByPolicy)
		if rec.ID != "ask-x1" {
			t.Fatalf("the degraded question is %q, want ask-x1", rec.ID)
		}

		evs := askEvents(t, l)
		if len(evs) != 1 {
			t.Fatalf("a degraded question wrote %s, want one ending and no opening", eventKinds(evs))
		}
		if u := oneEnding(t, evs); u.Body == nil || u.Body.Question == nil {
			t.Fatalf("the ending is %+v, want the opening nobody saw", u)
		}
		// The next question the policy really answers is still the first
		// visible one.
		if next := r.Automatic(token, questionReq(), AskAnswer{Answers: map[string][]string{"q1": {"opt-a"}}}); next.ID != "ask-1" {
			t.Fatalf("the next question is %q, want ask-1", next.ID)
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

// TestAskAQuestionThatAsksNothingIsAnsweredWithNoAnswers is the one ask an
// empty answer really answers. cursor sends ask_question with no questions, and
// before the registry existed craze answered it accepted with empty answers and
// published the Auto question line that went with it. The registry's "the zero
// AskAnswer is never valid" rule turned that into a cancel, which nothing in
// the plan licenses; an answer with no answers is valid for a question that has
// none, and ErrBadAnswer for one that has any.
func TestAskAQuestionThatAsksNothingIsAnsweredWithNoAnswers(t *testing.T) {
	empty := func() AskRequest {
		return AskRequest{Kind: AskQuestion, Body: AskBody{Question: &QuestionEvent{Title: "Anything else?"}}}
	}

	t.Run("the policy answers it", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()

		// acp.AskAutoAnswers of a request with no questions is an empty map.
		rec := r.Automatic(token, empty(), AskAnswer{Answers: map[string][]string{}})
		assertResolved(t, rec, AskAutomatic, AskByPolicy)
		if rec.ID != "ask-1" {
			t.Fatalf("the automatic question is %q, want the visible ask-1: its opening is published", rec.ID)
		}

		evs := askEvents(t, l)
		if len(evs) != 2 || evs[0].Type != EventQuestion || evs[1].Type != EventAsk {
			t.Fatalf("the policy wrote %s, want the Auto opening then its ending", eventKinds(evs))
		}
		if q := evs[0].Question; !q.Auto || len(q.Answers) != 0 {
			t.Fatalf("the opening is %+v, want today's Auto shape with no answers", q)
		}
		u := evs[1].Ask
		assertEnding(t, u, "ask-1", AskAutomatic, AskByPolicy)
		if len(u.Answers) != 0 || u.Skip {
			t.Fatalf("the ending is %+v, want an answer with no answers and no skip", u)
		}
	})

	t.Run("a client answers it", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()
		a := mustOpen(t, r, t.Context(), token, empty())
		askEvents(t, l)

		// The zero answer, which is all a client could say about a question
		// that asked nothing.
		rec, err := r.Answer("tui-1/3", a.ID(), AskAnswer{})
		if err != nil {
			t.Fatalf("answering a question that asks nothing: %v", err)
		}
		assertResolved(t, rec, AskAnswered, AskByClient)
		u := oneEnding(t, askEvents(t, l))
		assertEnding(t, u, a.ID(), AskAnswered, AskByClient)
		if len(u.Answers) != 0 || u.Skip {
			t.Fatalf("the ending is %+v, want no answers and no skip", u)
		}
	})

	t.Run("a question that asked something still refuses it", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()
		a := mustOpen(t, r, t.Context(), token, questionReq())
		askEvents(t, l)

		if _, err := r.Answer("", a.ID(), AskAnswer{}); !errors.Is(err, ErrBadAnswer) {
			t.Fatalf("an empty answer to a real question returned %v, want ErrBadAnswer", err)
		}
		if got := r.Asks(); len(got) != 1 || got[0].Status != AskOpen {
			t.Fatalf("Asks() is %+v, want the ask still open", got)
		}
		// And the policy's degraded case is still a cancel for it.
		if rec := r.Automatic(token, questionReq(), AskAnswer{}); rec.Outcome != AskCancelled {
			t.Fatalf("an unanswerable automatic question is %+v, want a cancel", rec)
		}
	})
}

// TestAskEventsNeverShareMemoryWithTheRegistry is review r16's finding 2: an
// opening and a self-contained ending are deep copies, so a consumer that
// changes what it received cannot change what Answer validates against, what
// the record says was asked, or what another consumer reads — and cannot race
// Record() while it does. Under -race the concurrent half is the proof.
func TestAskEventsNeverShareMemoryWithTheRegistry(t *testing.T) {
	t.Run("an opening", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()
		a := mustOpen(t, r, t.Context(), token, permReq())

		evs := askEvents(t, l)
		if len(evs) != 1 || evs[0].Permission == nil {
			t.Fatalf("the opening is %s, want one permission", eventKinds(evs))
		}
		ev := evs[0].Permission
		ev.Options[0].OptionID = "tampered"
		ev.Options[0].Name = "tampered"
		ev.Tool = "tampered"

		rec, _ := r.Record(a.ID())
		if rec.Body.Permission.Options[0].OptionID != "allow-once" || rec.Body.Permission.Tool != "Edit a.go" {
			t.Fatalf("a consumer's edit reached the record: %+v", rec.Body.Permission)
		}
		// The option that was really offered still answers it, and the one the
		// consumer invented does not.
		if _, err := r.Answer("", a.ID(), AskAnswer{OptionID: "tampered"}); !errors.Is(err, ErrBadAnswer) {
			t.Fatalf("an option a consumer invented returned %v, want ErrBadAnswer", err)
		}
		if got, err := r.Answer("", a.ID(), AskAnswer{OptionID: "allow-once"}); err != nil {
			t.Fatalf("the option that was offered: %v", err)
		} else if got.Answer.OptionID != "allow-once" {
			t.Fatalf("the record is %+v, want the offered option", got.Answer)
		}
		if u := oneEnding(t, askEvents(t, l)); u.Label != "Allow" {
			t.Fatalf("the ending's label is %q, want the name as it was offered", u.Label)
		}
	})

	t.Run("a self-contained ending and its answers", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()

		rec := r.Automatic(token, questionReq(), AskAnswer{Answers: map[string][]string{"q1": {"opt-a"}}})
		evs := askEvents(t, l)
		u := evs[len(evs)-1].Ask
		u.Answers["q1"][0] = "tampered"
		u.Answers["invented"] = []string{"tampered"}
		if again, _ := r.Record(rec.ID); again.Answer.Answers["q1"][0] != "opt-a" || len(again.Answer.Answers) != 1 {
			t.Fatalf("a consumer's edit reached the recorded answer: %+v", again.Answer)
		}

		// A refused open's ending carries the whole body, which is the only
		// place a body leaves the registry on an event.
		r.Close()
		askEvents(t, l)
		refused, err := r.Open(t.Context(), token, permReq())
		if err != nil {
			t.Fatalf("Open after Close: %v", err)
		}
		body := oneEnding(t, askEvents(t, l)).Body
		body.Permission.Options[0].OptionID = "tampered"
		body.Permission.Tool = "tampered"
		kept, _ := r.Record(refused.ID())
		if kept.Body.Permission.Options[0].OptionID != "allow-once" || kept.Body.Permission.Tool != "Edit a.go" {
			t.Fatalf("a consumer's edit reached the refusal's record: %+v", kept.Body.Permission)
		}
	})

	t.Run("a consumer mutating while the registry reads", func(t *testing.T) {
		r, l := newAskRegistry(t)
		token := r.BeginTurn()
		a := mustOpen(t, r, t.Context(), token, permReq())
		ev := askEvents(t, l)[0].Permission

		const rounds = 200
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			for i := range rounds {
				ev.Options[0].OptionID = fmt.Sprintf("tampered-%d", i)
				ev.Tool = fmt.Sprintf("tampered-%d", i)
			}
		})
		wg.Go(func() {
			<-start
			for range rounds {
				if rec, ok := r.Record(a.ID()); !ok || rec.Body.Permission.Options[0].OptionID != "allow-once" {
					t.Errorf("the record moved under a consumer's edit: %+v", rec.Body.Permission)
					return
				}
				if _, err := r.Answer("", a.ID(), AskAnswer{OptionID: "no-such-option"}); !errors.Is(err, ErrBadAnswer) {
					t.Errorf("validation read a consumer's memory: %v", err)
					return
				}
			}
		})
		close(start)
		waitDone(t, &wg)
	})
}

// TestAskManyWaitersRaceTheContextAgainstEveryEnding is A-X3 for the wait side:
// several waiters on one ask, its asking call's context ending, and whatever
// else can end it, all at the same instant. Whichever wins, there is exactly
// one ending, every waiter is given that same one, and none is left blocked.
func TestAskManyWaitersRaceTheContextAgainstEveryEnding(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(r *AskRegistry, token TurnToken, id string)
	}{
		{"an answer", func(r *AskRegistry, _ TurnToken, id string) {
			_, _ = r.Answer("", id, AskAnswer{OptionID: "allow-once"})
		}},
		{"its turn's end", func(r *AskRegistry, token TurnToken, _ string) { r.EndTurn(token) }},
		{"the close", func(r *AskRegistry, _ TurnToken, _ string) { r.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for range 32 {
				r, l := newAskRegistry(t)
				token := r.BeginTurn()
				ctx, cancel := context.WithCancel(t.Context())
				a, err := r.Open(ctx, token, permReq())
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				askEvents(t, l)

				const waiters = 4
				got := make(chan AskRecord, waiters)
				start := make(chan struct{})
				var wg sync.WaitGroup
				for range waiters {
					wg.Go(func() {
						<-start
						got <- a.Wait()
					})
				}
				wg.Go(func() { <-start; cancel() })
				wg.Go(func() { <-start; tc.end(r, token, a.ID()) })
				close(start)
				waitDone(t, &wg)
				cancel()
				close(got)

				u := oneEnding(t, askEvents(t, l))
				var first AskRecord
				for i := 0; ; i++ {
					rec, ok := <-got
					if !ok {
						if i != waiters {
							t.Fatalf("%d of %d waiters returned", i, waiters)
						}
						break
					}
					if i == 0 {
						first = rec
						assertResolved(t, rec, u.Outcome, u.By)
						continue
					}
					if rec.Outcome != first.Outcome || rec.By != first.By || rec.ID != first.ID {
						t.Fatalf("waiter %d was given %s/%s, want the one ending %s/%s",
							i, rec.Outcome, rec.By, first.Outcome, first.By)
					}
				}
			}
		})
	}
}

// TestAskConcurrentOpenAndClose is the other side of Close's promise: asks
// opening from every direction while the registry closes. Every Open answers
// with an ask that is resolved once the close has run — parked and then ended,
// or refused outright — and nothing is left open behind it.
func TestAskConcurrentOpenAndClose(t *testing.T) {
	// NoPrimary: this writes more events than a primary nobody reads holds.
	l := newTestLog(t, EventLogOptions{NoPrimary: true})
	r := NewAskRegistry(l, func() time.Time { return askTestTime })
	token := r.BeginTurn()

	const openers, each = 6, 12
	asks := make(chan *Ask, openers*each)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range openers {
		wg.Go(func() {
			<-start
			for range each {
				a, err := r.Open(t.Context(), token, permReq())
				if err != nil {
					t.Errorf("Open: %v", err)
					return
				}
				asks <- a
			}
		})
	}
	wg.Go(func() { <-start; r.Close() })
	close(start)
	waitDone(t, &wg)
	close(asks)

	if got := r.Asks(); len(got) != 0 {
		t.Fatalf("%d asks survived the close: %+v", len(got), got)
	}
	n := 0
	for a := range asks {
		n++
		select {
		case <-a.e.done:
		case <-time.After(logWatchdog):
			t.Fatalf("%s was never resolved, although the registry has closed", a.ID())
		}
		rec := a.Wait()
		if rec.Status != AskResolved {
			t.Fatalf("%s is %s after the close", rec.ID, rec.Status)
		}
	}
	if n != openers*each {
		t.Fatalf("%d asks opened, want %d", n, openers*each)
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
				// The reversed order the seq comparison below cannot see: an
				// opening that arrives AFTER its own ending has no earlier
				// opening to compare against, so it has to be caught here.
				if at, ended := endedAt[ev.Permission.ID]; ended {
					t.Fatalf("%s was opened at seq %d, after its ending at %d", ev.Permission.ID, rec.Seq, at)
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
