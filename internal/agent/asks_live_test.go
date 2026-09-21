package agent

import (
	"testing"

	"github.com/charliek/craze/internal/acp"
)

// A-X4's rows that only the live session can reach: the ones where the record
// exists because a handler, a policy or ACP itself resolved a request without
// anyone ever being shown a card. Each is "exactly N endings for this id", not
// "an ending arrived", because the registry's promise is one per ask.

// Force: the permission is answered by policy, no opening is ever published —
// so a forced `craze prompt --json` run gains no permission line — and the one
// ending carries the body, which is what makes the record self-contained.
//
// Its id is a hidden one (asks.go's hiddenMark): the baseline's Force path
// never called nextID, so an id a user never sees must not spend the number the
// next visible card would have had (review r17, finding 8).
func TestForcedPermissionIsOneAutomaticEndingWithNoOpening(t *testing.T) {
	s := startScriptOpts(t, "permission", Options{Force: true})
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	u := log.waitAsk(t, "perm-x1")
	if u.Outcome != AskAutomatic || u.By != AskByPolicy {
		t.Fatalf("ending %+v", u)
	}
	if u.Body == nil || u.Body.Permission == nil || u.Body.Permission.Tool != "Shell" {
		t.Fatalf("a forced allow owes a self-contained ending: %+v", u.Body)
	}
	if u.OptionID == "" {
		t.Fatalf("the ending records the option policy picked: %+v", u)
	}
	for _, ev := range log.snapshot() {
		if ev.Type == EventPermission {
			t.Fatalf("a forced allow published a card: %+v", ev.Permission)
		}
	}
}

// Force with nothing to allow: the same record, cancelled by policy, because a
// policy that cannot answer its own ask is a cancel. Before the registry this
// emitted nothing at all (§2.3).
func TestForcedPermissionWithNoAllowOptionIsCancelledByPolicy(t *testing.T) {
	s := newAskSession(t, Options{Force: true})
	log := collect(t, s)
	dec := s.onPermission(inTurn(), acp.PermissionRequest{
		Options: []acp.PermissionOption{{OptionID: "no", Name: "Reject", Kind: acp.KindRejectOnce}},
	})
	if !dec.Cancelled {
		t.Fatalf("decision %+v", dec)
	}
	u := log.waitAsk(t, "perm-x1")
	if u.Outcome != AskCancelled || u.By != AskByPolicy || u.Body == nil {
		t.Fatalf("ending %+v", u)
	}
}

// The causal barrier the automatic path owes (§3.6; panel astra 9, CodeRabbit
// 11): the Auto opening and its ending are BOTH in the record before the text
// the agent writes once it has been answered. The handler flushes the outbox
// before it hands the decision back, which is what makes the order this and
// not "whenever the drainer got there".
func TestAnAutomaticAnswerIsRecordedBeforeTheAgentsNextOutput(t *testing.T) {
	for _, tc := range []struct {
		script, text, id, reply string
	}{
		{"ask", "q", "ask-1", "asked:answered:q1=opt-a;q2=opt-x"},
		{"plan", "go", "plan-1", "planned:accepted"},
	} {
		t.Run(tc.script, func(t *testing.T) {
			s := startScriptOpts(t, tc.script, Options{Force: true})
			log := collect(t, s)
			if _, err := s.Prompt(t.Context(), tc.text); err != nil {
				t.Fatal(err)
			}
			log.waitTexts(t, tc.reply)
			var order []string
			for _, ev := range log.snapshot() {
				switch {
				case ev.Type == EventQuestion && ev.Question.ID == tc.id:
					order = append(order, "opening")
				case ev.Type == EventPlan && ev.Plan.ID == tc.id:
					order = append(order, "opening")
				case ev.Type == EventAsk && ev.Ask.ID == tc.id:
					order = append(order, "ending")
				case ev.Type == EventText && ev.Text == tc.reply:
					order = append(order, "reply")
				}
			}
			want := []string{"opening", "ending", "reply"}
			if len(order) != len(want) {
				t.Fatalf("order %v, want %v", order, want)
			}
			for i := range want {
				if order[i] != want[i] {
					t.Fatalf("order %v, want %v", order, want)
				}
			}
		})
	}
}

// A cursor/ask_question carrying no questions at all, answered by policy. The
// agent gets the accepted reply with empty answers it always got, and the Auto
// question line it always had is still published: the registry's "the zero
// AskAnswer is never valid" rule briefly turned this into cancelled-by-policy,
// which nothing in plan 021 licenses (asks.go's validateAnswer).
func TestAnAutomaticQuestionWithNoQuestionsIsAnsweredNotCancelled(t *testing.T) {
	// No Force and not Interactive is the policy `craze prompt` runs under:
	// questions are answered with each question's first option.
	s := newAskSession(t, Options{})
	if p := EffectiveApproval(s.opts); p.Question != ApprovalFirstOption {
		t.Fatalf("the policy is %+v, want first_option for a question", p)
	}
	log := collect(t, s)

	dec := s.onAskQuestion(inTurn(), acp.AskQuestionRequest{ToolCallID: "t1"})

	if dec.Cancelled {
		t.Fatalf("decision %+v, want the accepted reply an empty question always got", dec)
	}
	if len(dec.Answers) != 0 || dec.Skip {
		t.Fatalf("decision %+v, want empty answers and no skip", dec)
	}
	// The line is still the Auto question with no answers, and its ending is
	// automatic rather than a cancel.
	q := log.waitQuestion(t)
	if !q.Auto || len(q.Questions) != 0 || len(q.Answers) != 0 {
		t.Fatalf("the opening is %+v, want the Auto shape with nothing in it", q)
	}
	u := log.waitAsk(t, q.ID)
	if u.Outcome != AskAutomatic || u.By != AskByPolicy {
		t.Fatalf("ending %+v, want automatic by policy", u)
	}
}

// A request ACP answered before any handler of craze's ran — a cancel or a
// close that got there first, or a turn that had gone stale. No handler runs,
// so nothing parks and, before this, the agent's question left no trace at all
// (§2.3, panel astra 12). Each becomes one self-contained ending, with the
// body and no opening — and a hidden id, because a request no card was ever
// raised for must not renumber the cards that are (review r17, finding 8).
func TestEarlyAnsweredRequestsBecomeOneSelfContainedEnding(t *testing.T) {
	for _, tc := range []struct {
		reason  acp.EarlyAnswerReason
		outcome AskOutcome
	}{
		{acp.EarlyCancelled, AskCancelled},
		{acp.EarlyStaleTurn, AskTurnEnded},
		{acp.EarlyClosed, AskClosing},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			s := newAskSession(t, Options{Interactive: true})
			log := collect(t, s)
			s.onEarlyAnswer(acp.EarlyAnswer{
				Reason: tc.reason,
				Params: acp.RequestParams{Permission: &acp.PermissionRequest{
					ToolCall: acp.ToolCall{Title: "Shell"},
					Options:  []acp.PermissionOption{{OptionID: "yes", Kind: acp.KindAllowOnce}},
				}},
			})
			u := log.waitAsk(t, "perm-x1")
			if u.Outcome != tc.outcome || u.By != AskByProvider {
				t.Fatalf("ending %+v", u)
			}
			if u.Body == nil || u.Body.Permission == nil || u.Body.Permission.Tool != "Shell" {
				t.Fatalf("the record must say what was asked: %+v", u.Body)
			}
			for _, ev := range log.snapshot() {
				if ev.Type == EventPermission {
					t.Fatalf("an early answer raised a card: %+v", ev.Permission)
				}
			}
			if left := s.asks.Asks(); len(left) != 0 {
				t.Fatalf("an early answer parked something: %+v", left)
			}
		})
	}
}

// An answer the client accepted whose reply lost to ACP's own cancelled reply
// (panel astra 13). The ask was answered and stays answered — one ending, and
// no second one — and the record carries both facts: the answer, and that the
// agent never heard it. The reply's own disposition comes from internal/acp
// (C8a); this is the session wiring it to the record.
func TestAnAnsweredAskWhoseReplyWasLostSaysSo(t *testing.T) {
	s := newAskSession(t, Options{Interactive: true})
	log := collect(t, s)
	token := beginTurn(s)
	a := openAsk(t, s, token, AskPermission)
	// The option the body offers, so the answer validates.
	s.asks.Report(a.ID(), AskReport{}) // no-op, and proves reporting nothing changes nothing
	if err := answerAsk(s, a.ID(), AskAnswer{Cancel: true}); err != nil {
		t.Fatal(err)
	}
	askReported(a)(acp.ReplyDisposition{Lost: acp.ReplyLostCancelled})
	rec, ok := s.asks.Record(a.ID())
	if !ok {
		t.Fatal("the record is gone")
	}
	if rec.Outcome != AskCancelled || rec.Delivered || rec.Lost != acp.ReplyLostCancelled {
		t.Fatalf("record %+v", rec)
	}
	// waitAsk fails on a second ending, which is the property: a lost delivery
	// is one journal note and never another EventAsk.
	if u := log.waitAsk(t, a.ID()); u.Outcome != AskCancelled {
		t.Fatalf("ending %+v", u)
	}
}
