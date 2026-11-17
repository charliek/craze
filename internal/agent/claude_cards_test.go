package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// The claude cards end to end (plan 052 C3), over the fake adapter's card
// scripts (cmd/craze-fake-agent/claude_cards.go), which say in the turn the
// answer each request got.

// unattendedClaude is craze new --wait's policy (cli.unattendedPolicy): no
// frontend, force, first option, and an ACP plan returned.
func unattendedClaude() Options {
	return Options{Force: true, Unattended: true, Approval: &ApprovalPolicy{
		Permission: ApprovalAllow, Question: ApprovalFirstOption, Plan: ApprovalReturn,
	}}
}

// waitText waits for want in the turn's text.
func (e *eventLog) waitText(t *testing.T, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := texts(e.snapshot()); strings.Contains(got, want) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %q", want, texts(e.snapshot()))
	return ""
}

func waitOpenAsk(t *testing.T, s *session, kind AskKind) AskRecord {
	t.Helper()
	var rec AskRecord
	waitFor(t, "the "+string(kind)+" card", func() bool {
		for _, a := range s.asks.Asks() {
			if a.Kind == kind && a.Status == AskOpen {
				rec = a
				return true
			}
		}
		return false
	})
	return rec
}

// TestClaudeAskCard (AC6): AskUserQuestion is a question card whose question
// takes free text and whose options carry their descriptions. A pick with
// text, text alone, and a skip go back as accept with question_0_custom,
// accept with it alone, and decline; a cancelled turn answers cancel.
func TestClaudeAskCard(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer *AskAnswer
		want   string
	}{
		{"pick and text", &AskAnswer{Answers: map[string][]string{"question_0": {"Blue"}}, Text: map[string]string{"question_0": "teal-ish"}},
			`ask answer: {"action":"accept","content":{"question_0":"Blue","question_0_custom":"teal-ish"}}`},
		{"text alone", &AskAnswer{Answers: map[string][]string{"question_0": {}}, Text: map[string]string{"question_0": "green"}},
			`ask answer: {"action":"accept","content":{"question_0_custom":"green"}}`},
		{"skip", &AskAnswer{Skip: true}, `ask answer: {"action":"decline"}`},
		// The card dismissed (Esc on claude): cancel, which aborts the tool.
		{"dismissed", &AskAnswer{Cancel: true}, `ask answer: {"action":"cancel"}`},
		{"cancelled turn", nil, `ask answer: {"action":"cancel"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newClaudeRig(t, "claude-ask", Options{Interactive: true})
			log := collect(t, r.s)
			if err := r.s.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := r.s.Prompt(context.Background(), "ask me"); done <- err }()
			rec := waitOpenAsk(t, r.s, AskQuestion)
			q := rec.Body.Question
			if q == nil || len(q.Questions) != 1 || !q.Questions[0].AllowText || q.Questions[0].Prompt != "Do you prefer red or blue?" ||
				q.Questions[0].Options[0].Description != "Choose red" {
				t.Fatalf("question %+v", q)
			}
			if tc.answer == nil {
				if _, err := r.s.Cancel(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else if _, err := r.s.asks.Answer("", rec.ID, *tc.answer); err != nil {
				t.Fatal(err)
			}
			log.waitText(t, tc.want)
			<-done
		})
	}
}

// TestClaudeAskRefusesTextItDoesNotTake: free text for a question that takes
// none, or past MaxAnswerText, is a bad answer that leaves the card open.
func TestClaudeAskRefusesTextItDoesNotTake(t *testing.T) {
	body := AskBody{Question: &QuestionEvent{Questions: []Question{
		{ID: "a", Options: []Option{{ID: "x"}}},
		{ID: "b", AllowText: true, Options: []Option{{ID: "y"}}},
	}}}
	for _, a := range []AskAnswer{
		{Text: map[string]string{"a": "no"}},
		{Text: map[string]string{"c": "no"}},
		{Text: map[string]string{"b": strings.Repeat("x", MaxAnswerText+1)}},
		{Skip: true, Text: map[string]string{"b": "no"}},
	} {
		if _, _, err := validateAnswer(AskQuestion, body, a); !errors.Is(err, ErrBadAnswer) {
			t.Fatalf("%+v: %v", a, err)
		}
	}
	got, _, err := validateAnswer(AskQuestion, body, AskAnswer{Text: map[string]string{"b": "fine", "a": ""}})
	if err != nil || got.Text["b"] != "fine" || len(got.Text) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}

// TestClaudeAskUnattendedFirstOption (AC6): with no one to ask, the first
// option goes back, as craze's question policy answers every provider's.
func TestClaudeAskUnattendedFirstOption(t *testing.T) {
	r := newClaudeRig(t, "claude-ask", unattendedClaude())
	log := collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.Prompt(t.Context(), "ask me MULTI"); err != nil {
		t.Fatal(err)
	}
	log.waitText(t, `ask answer: {"action":"accept","content":{"question_0":"Red","question_1":["S"]}}`)
}

// TestClaudeAskOdd (AC6): elicitations craze cannot show raise no card; a
// url-mode one is declined and a form of another shape cancelled, each with
// a note in the journal and on the diag lane.
func TestClaudeAskOdd(t *testing.T) {
	r := newClaudeRig(t, "claude-ask-odd", Options{Interactive: true})
	log := collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	got := log.waitText(t, "odd no-options:")
	for _, want := range []string{
		`odd url: {"action":"decline"}`, `odd extra-field: {"action":"cancel"}`,
		`odd refusal-fallback: {"action":"cancel"}`, `odd no-options: {"action":"cancel"}`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if n := len(r.s.asks.Asks()); n != 0 {
		t.Fatalf("%d asks raised", n)
	}
	waitFor(t, "the diag notes", func() bool {
		d := r.diag.String()
		return strings.Count(d, "craze: claude asked a question craze cannot show") == 3 && strings.Contains(d, "elicitation mode")
	})
}

// TestClaudePlanCard (AC7): a plan exit is the plan card, under force too —
// force never sees it. Approve sends exit-plan-default and the turn goes on to
// its end; keep planning sends reject and the turn ends cancelled.
func TestClaudePlanCard(t *testing.T) {
	for _, tc := range []struct {
		name   string
		accept bool
		want   string
		stop   string
	}{
		{"approve", true, "plan answer: exit-plan-default", acp.StopEndTurn},
		{"keep planning", false, "plan answer: reject", acp.StopCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newClaudeRig(t, "claude-plan", Options{Force: true, Interactive: true, Mode: "plan"})
			log := collect(t, r.s)
			if err := r.s.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			type res struct {
				r   Result
				err error
			}
			done := make(chan res, 1)
			go func() { got, err := r.s.Prompt(context.Background(), "plan it"); done <- res{got, err} }()
			rec := waitOpenAsk(t, r.s, AskPlan)
			if p := rec.Body.Plan; p == nil || !strings.HasPrefix(p.Plan, "# Plan: create plan.txt") {
				t.Fatalf("plan %+v", rec.Body.Plan)
			}
			if _, err := r.s.asks.Answer("", rec.ID, AskAnswer{Accept: tc.accept, Reject: !tc.accept}); err != nil {
				t.Fatal(err)
			}
			log.waitText(t, tc.want)
			got := <-done
			if got.err != nil || got.r.StopReason != tc.stop {
				t.Fatalf("result %+v %v, want stop %s", got.r, got.err, tc.stop)
			}
			// Keep planning's cancelled ending says it was the plan's
			// (review C3 item 9); an approval's end_turn says nothing.
			if d := log.waitType(t, EventDone); d.KeptPlanning != !tc.accept {
				t.Fatalf("done %+v", d)
			}
			for _, a := range r.s.asks.Asks() {
				if a.Kind == AskPermission {
					t.Fatalf("a plan exit raised a permission %+v", a)
				}
			}
		})
	}
}

// TestClaudePlanUnattendedReturnsThePlan (AC7; C2's deferred gap): under
// craze new --wait's policy — force on — a plan exit is never approved: it is
// answered reject at once, recorded as the returned plan (the automatic
// answer that does not accept), and no session/cancel is written at all, so
// none precedes the answer.
func TestClaudePlanUnattendedReturnsThePlan(t *testing.T) {
	r := newClaudeRig(t, "claude-plan", func() Options { o := unattendedClaude(); o.Mode = "plan"; return o }())
	log := collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	res, err := r.s.Prompt(t.Context(), "plan it")
	if err != nil || res.StopReason != acp.StopCancelled {
		t.Fatalf("result %+v %v", res, err)
	}
	got := log.waitText(t, "plan answer: reject")
	if strings.Contains(got, "exit-plan-default") {
		t.Fatalf("the plan was approved: %q", got)
	}
	var plan *PlanEvent
	for _, ev := range log.snapshot() {
		if ev.Type == EventPlan {
			plan = ev.Plan
		}
	}
	if plan == nil || !plan.Auto || plan.Accepted || !strings.HasPrefix(plan.Plan, "# Plan: create plan.txt") {
		t.Fatalf("the returned plan's record %+v", plan)
	}
	if calls := r.read(t, "calls"); strings.Contains(calls, "session/cancel") {
		t.Fatalf("a session/cancel was written:\n%s", calls)
	}
}

// TestClaudePlanOddNeverBypasses (AC7): for every shape of claude-plan-odd,
// under force with a frontend and under the unattended policy, the answer is
// never an allow_always option nor any exit-plan-* — exit-plan-default
// included — and no card is raised.
func TestClaudePlanOddNeverBypasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"force", Options{Force: true, Interactive: true}},
		{"unattended", unattendedClaude()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newClaudeRig(t, "claude-plan-odd", tc.opts)
			log := collect(t, r.s)
			if err := r.s.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			// Bounded: a card raised for an odd shape would wait for ever.
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if _, err := r.s.Prompt(ctx, "go"); err != nil {
				t.Fatal(err)
			}
			got := log.waitText(t, "odd allow-only:")
			lines := 0
			for _, ln := range strings.Split(got, "\n") {
				if !strings.HasPrefix(ln, "odd ") {
					continue
				}
				lines++
				if strings.Contains(ln, "allow_always") || strings.Contains(ln, "exit-plan-") || strings.Contains(ln, "allow_once") {
					t.Fatalf("an odd plan exit was allowed: %q", ln)
				}
			}
			if lines != 9 {
				t.Fatalf("%d shapes answered, want 9:\n%s", lines, got)
			}
			for _, want := range []string{"odd no-reject: cancelled", "odd allow-only: cancelled", "odd missing-default: reject (reject_once)"} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q:\n%s", want, got)
				}
			}
			if n := len(r.s.asks.Asks()); n != 0 {
				t.Fatalf("%d asks raised", n)
			}
			waitFor(t, "the notes", func() bool {
				return strings.Count(r.diag.String(), "the plan approval had an unexpected shape") == 9
			})
		})
	}
}

// TestClaudeInterjectSteers (AC9): a message sent mid-turn goes out as
// _session/steering, injected into the turn, and shows in the transcript as
// the interjection it is.
func TestClaudeInterjectSteers(t *testing.T) {
	r := newClaudeRig(t, "claude-steer", Options{Interactive: true})
	log := collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := r.s.Prompt(context.Background(), "wait"); done <- err }()
	log.waitText(t, "waiting")
	if err := r.s.Interject(t.Context(), "banana"); err != nil {
		t.Fatal(err)
	}
	log.waitText(t, "steered: banana")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range log.snapshot() {
		if ev.Type == EventUser && ev.Interjection && ev.Text == "banana" {
			found = true
		}
	}
	if !found {
		t.Fatal("no interjection in the transcript")
	}
	if !strings.Contains(r.read(t, "calls"), acp.MethodClaudeSteering) {
		t.Fatal("no _session/steering on the wire")
	}
}

// TestClaudeInterjectIdle (AC9): with no turn the session refuses before the
// wire; with a turn the adapter has already settled, its promptRequired is the
// same "no turn" refusal, which the TUI keeps the draft on (it queues).
func TestClaudeInterjectIdle(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{Interactive: true})
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.s.Interject(t.Context(), "x"); !errors.Is(err, ErrNotInTurn) {
		t.Fatalf("idle: %v", err)
	}
	run := r.s.Begin("HANG")
	done := make(chan error, 1)
	go func() { _, err := run(context.Background()); done <- err }()
	waitFor(t, "the turn", func() bool { return r.s.promptInFlight() })
	if err := r.s.Interject(t.Context(), "x"); !errors.Is(err, ErrPromptRequired) || !errors.Is(err, ErrNotInTurn) {
		t.Fatalf("promptRequired: %v", err)
	}
	if _, err := r.s.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-done
	if !strings.Contains(r.read(t, "calls"), acp.MethodClaudeSteering) {
		t.Fatal("the in-turn steer never reached the wire")
	}
}

// TestFreeTextSurvivesTheCodec (AC6): a question's allowText and an answer's
// text come back through the event codec — what the journal, its replay, a
// reconnect's stream and the snapshot's open asks are all written in — and
// both are left out when unset, so no other provider's bytes move.
func TestFreeTextSurvivesTheCodec(t *testing.T) {
	q := &QuestionEvent{ID: "ask-1", Questions: []Question{{ID: "question_0", Prompt: "p", AllowText: true, Options: []Option{{ID: "a", Label: "a"}}}}}
	for _, ev := range []Event{
		{Type: EventQuestion, Question: q},
		{Type: EventAsk, Ask: &AskUpdate{ID: "ask-1", Kind: AskQuestion, Outcome: AskAnswered, By: AskByClient,
			Answers: map[string][]string{"question_0": {}}, Text: map[string]string{"question_0": "teal"}}},
	} {
		body, err := EncodeEvent(ev)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeEvent(body)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case ev.Question != nil && (got.Question == nil || !got.Question.Questions[0].AllowText || !strings.Contains(body, `"allowText":true`)):
			t.Fatalf("allowText lost: %s", body)
		case ev.Ask != nil && (got.Ask == nil || got.Ask.Text["question_0"] != "teal" || !strings.Contains(body, `"text":{"question_0":"teal"}`)):
			t.Fatalf("text lost: %s", body)
		}
	}
	q.Questions[0].AllowText = false
	body, _ := EncodeEvent(Event{Type: EventQuestion, Question: q})
	if strings.Contains(body, "allowText") {
		t.Fatalf("an unset allowText is written: %s", body)
	}
}

// TestQuestionAnswerShapes covers review C3 items 4, 5 and 7 at the registry:
// a single-select takes one pick, on every provider; a required question must
// get a pick or text and its ask cannot be skipped; and a question can be
// cancelled — claude's Esc — but a cancel carries no answers.
func TestQuestionAnswerShapes(t *testing.T) {
	body := AskBody{Question: &QuestionEvent{Questions: []Question{
		{ID: "one", Options: []Option{{ID: "a"}, {ID: "b"}}},
		{ID: "many", AllowMultiple: true, Options: []Option{{ID: "x"}, {ID: "y"}}},
	}}}
	required := AskBody{Question: &QuestionEvent{Questions: []Question{
		{ID: "r", Required: true, AllowText: true, AllowMultiple: true, Options: []Option{{ID: "x"}}},
	}}}
	for _, tc := range []struct {
		name string
		body AskBody
		a    AskAnswer
		ok   bool
	}{
		{"two picks on a single-select", body, AskAnswer{Answers: map[string][]string{"one": {"a", "b"}}}, false},
		{"one pick each", body, AskAnswer{Answers: map[string][]string{"one": {"a"}, "many": {"x", "y"}}}, true},
		{"required left empty", required, AskAnswer{Answers: map[string][]string{"r": {}}}, false},
		{"required skipped", required, AskAnswer{Skip: true}, false},
		{"required picked", required, AskAnswer{Answers: map[string][]string{"r": {"x"}}}, true},
		{"required typed", required, AskAnswer{Text: map[string]string{"r": "own"}}, true},
		{"required cancelled", required, AskAnswer{Cancel: true}, true},
		{"cancel with answers", body, AskAnswer{Cancel: true, Answers: map[string][]string{"one": {"a"}}}, false},
		{"cancel with skip", body, AskAnswer{Cancel: true, Skip: true}, false},
	} {
		got, _, err := validateAnswer(AskQuestion, tc.body, tc.a)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrBadAnswer)) {
			t.Fatalf("%s: %+v %v", tc.name, got, err)
		}
	}
	if d := askDecision(AskRecord{Outcome: AskAnswered, Answer: AskAnswer{Cancel: true}}); !d.Cancelled {
		t.Fatalf("a cancelled question's decision %+v", d)
	}
}

// TestAWithdrawnRequestEndsAsWithdrawn (review C3 item 6): a request the agent
// withdrew ($/cancel_request, the Arrival's Call ended with
// acp.ErrRequestWithdrawn) ends its parked card as withdrawn — whether it
// lands while the card is up or before it was raised — and an early answer
// the client reports as withdrawn says so too.
func TestAWithdrawnRequestEndsAsWithdrawn(t *testing.T) {
	req := acp.AskQuestionRequest{Questions: []acp.AskQuestion{{ID: "question_0", Prompt: "p", Options: []acp.AskOption{{ID: "a", Label: "a"}}}}}
	withdrawn := func() (context.Context, context.CancelCauseFunc) {
		return context.WithCancelCause(context.Background())
	}
	t.Run("while the card is up", func(t *testing.T) {
		s := newAskSession(t, Options{Interactive: true})
		ctx, cancel := withdrawn()
		done := make(chan acp.AskDecision, 1)
		go func() { done <- s.onAskQuestion(acp.Arrival{Call: ctx}, req) }()
		waitFor(t, "the card", func() bool { return len(s.asks.Asks()) == 1 })
		cancel(acp.ErrRequestWithdrawn)
		if d := <-done; !d.Cancelled {
			t.Fatalf("decision %+v", d)
		}
		if r := s.asks.Resolved(); len(r) != 1 || r[0].Outcome != AskCancelled || r[0].By != AskByWithdrawn {
			t.Fatalf("resolved %+v", r)
		}
	})
	t.Run("before it was raised", func(t *testing.T) {
		s := newAskSession(t, Options{Interactive: true})
		ctx, cancel := withdrawn()
		cancel(acp.ErrRequestWithdrawn)
		s.onAskQuestion(acp.Arrival{Call: ctx}, req)
		if r := s.asks.Resolved(); len(r) != 1 || r[0].By != AskByWithdrawn {
			t.Fatalf("resolved %+v", r)
		}
	})
	t.Run("early", func(t *testing.T) {
		s := newAskSession(t, Options{Interactive: true})
		s.onEarlyAnswer(acp.EarlyAnswer{Reason: acp.EarlyWithdrawn, Params: acp.RequestParams{Ask: &req}})
		if r := s.asks.Resolved(); len(r) != 1 || r[0].Outcome != AskCancelled || r[0].By != AskByWithdrawn {
			t.Fatalf("resolved %+v", r)
		}
	})
	t.Run("a cancel is still the call's", func(t *testing.T) {
		s := newAskSession(t, Options{Interactive: true})
		ctx, cancel := withdrawn()
		cancel(nil)
		s.onAskQuestion(acp.Arrival{Call: ctx}, req)
		if r := s.asks.Resolved(); len(r) != 1 || r[0].By != AskByCall {
			t.Fatalf("resolved %+v", r)
		}
	})
}

// TestKeptPlanningIsClaudesAndYieldsToACancel (review C3 item 9): the mark is
// set for claude's turn alone, is that turn's, and a cancel asked for after the
// rejection takes it away — the ending is then the user's cancel.
func TestKeptPlanningIsClaudesAndYieldsToACancel(t *testing.T) {
	p := ClaudeProvider()
	s := newAskSession(t, Options{Provider: &p})
	turn := s.asks.BeginTurn()
	s.notePlanKept(turn)
	if !s.keptPlanning(turn) {
		t.Fatal("the rejected turn's ending is not marked")
	}
	if s.keptPlanning(turn) {
		t.Fatal("the mark outlived the ending it was for")
	}
	s.notePlanKept(turn)
	if s.keptPlanning(s.asks.BeginTurn()) {
		t.Fatal("another turn's ending was marked")
	}
	s.notePlanKept(turn)
	if _, err := s.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.keptPlanning(turn) {
		t.Fatal("a cancel after the rejection still reads as kept planning")
	}
	g := GrokProvider()
	gs := newAskSession(t, Options{Provider: &g})
	gt := gs.asks.BeginTurn()
	gs.notePlanKept(gt)
	if gs.keptPlanning(gt) {
		t.Fatal("grok's rejection marked its ending")
	}
}
