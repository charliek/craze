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
func TestForcedPermissionIsOneAutomaticEndingWithNoOpening(t *testing.T) {
	s := startScriptOpts(t, "permission", Options{Force: true})
	log := collect(t, s)
	if _, err := s.Prompt(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	u := log.waitAsk(t, "perm-1")
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
	u := log.waitAsk(t, "perm-1")
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

// A request ACP answered before any handler of craze's ran — a cancel or a
// close that got there first, or a turn that had gone stale. No handler runs,
// so nothing parks and, before this, the agent's question left no trace at all
// (§2.3, panel astra 12). Each becomes one self-contained ending, with the
// body and no opening.
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
			u := log.waitAsk(t, "perm-1")
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
