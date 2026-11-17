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
	if err := r.s.Interject(t.Context(), "x"); !errors.Is(err, ErrNotInTurn) {
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
