package acp

import (
	"encoding/json"
	"testing"
	"time"
)

const grokAskParams = `{"sessionId":"s1","toolCallId":"tc-q","questions":[{"question":"Pick one","options":[{"label":"A"},{"label":"B"}]},{"question":"Pick any","multiSelect":true,"options":[{"label":"X"},{"label":"Y"}]}],"mode":"default"}`

const grokAskWrappedParams = `{"method":"x.ai/ask_user_question","params":` + grokAskParams + `}`

const grokPlanParams = `{"sessionId":"s1","toolCallId":"tc-p","planContent":"## Steps\n\n- read main.go\n"}`

const grokPlanWrappedParams = `{"method":"x.ai/exit_plan_mode","params":` + grokPlanParams + `}`

func TestCursorMethodNotFoundOnGrokAsk(t *testing.T) {
	p := newRawPipe(t)
	p.send(t, 1, MethodGrokAskUserQuestion, grokAskParams)
	msg := p.read(t)
	if msg.Error == nil || msg.Error.Code != CodeMethodNotFound {
		t.Fatalf("got %+v", msg.Error)
	}
}

func TestGrokMethodNotFoundOnCursorAsk(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.send(t, 1, MethodCursorAskQuestion, `{"toolCallId":"t","questions":[{"id":"q1","prompt":"?","options":[{"id":"a","label":"A"}]}]}`)
	msg := p.read(t)
	if msg.Error == nil || msg.Error.Code != CodeMethodNotFound {
		t.Fatalf("got %+v", msg.Error)
	}
}

func TestGrokAskDirectAndWrapped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		params string
	}{
		{"direct", MethodGrokAskUserQuestion, grokAskParams},
		{"wrapped", MethodGrokAskUserQuestionWrapped, grokAskWrappedParams},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, DialectGrok)
			var got AskQuestionRequest
			p.client.SetAskHandler(func(_ Arrival, req AskQuestionRequest) AskDecision {
				got = req
				return AskDecision{Answers: map[string][]string{"Pick one": {"B"}, "Pick any": {"Y"}}}
			})
			p.send(t, 1, tc.method, tc.params)
			msg := p.readWithin(t, 3*time.Second, "grok ask reply")
			if msg.Error != nil {
				t.Fatalf("error %+v", msg.Error)
			}
			want := `{"outcome":"accepted","answers":{"Pick one":["B"],"Pick any":["Y"]}}`
			if string(msg.Result) != want {
				t.Fatalf("reply %s", msg.Result)
			}
			if got.Questions[0].ID != "Pick one" || got.Questions[0].Prompt != "Pick one" {
				t.Fatalf("question mapping %+v", got.Questions[0])
			}
			if got.Questions[0].Options[0].ID != "A" || got.Questions[0].Options[0].Label != "A" {
				t.Fatalf("option mapping %+v", got.Questions[0].Options)
			}
			if !got.Questions[1].AllowMultiple {
				t.Fatal("multiSelect")
			}
		})
	}
}

func TestGrokAskSkipAndHeadlessAuto(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.client.SetAskHandler(func(Arrival, AskQuestionRequest) AskDecision { return AskDecision{Skip: true} })
	p.send(t, 1, MethodGrokAskUserQuestion, grokAskParams)
	if got := string(p.read(t).Result); got != `{"outcome":"skip_interview"}` {
		t.Fatalf("skip %s", got)
	}

	p2 := newRawPipeDialect(t, DialectGrok)
	p2.send(t, 2, MethodGrokAskUserQuestion, grokAskParams)
	got := string(p2.read(t).Result)
	want := `{"outcome":"accepted","answers":{"Pick one":["A"],"Pick any":["X"]}}`
	if got != want {
		t.Fatalf("auto %s", got)
	}
}

func TestGrokAskCursorEnvelopeIsNotSent(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.send(t, 1, MethodGrokAskUserQuestion, grokAskParams)
	msg := p.read(t)
	if json.Valid(msg.Result) && string(msg.Result) == `{"outcome":{"outcome":"answered","answers":[{"questionId":"Pick one","selectedOptionIds":["A"]},{"questionId":"Pick any","selectedOptionIds":["X"]}]}}` {
		t.Fatal("grok ask must not use the nested cursor answered envelope")
	}
	var flat struct {
		Outcome string              `json:"outcome"`
		Answers map[string][]string `json:"answers"`
	}
	if err := json.Unmarshal(msg.Result, &flat); err != nil || flat.Outcome != "accepted" {
		t.Fatalf("want flat accepted, got %s (%v)", msg.Result, err)
	}
}

func TestGrokPlanDirectAndWrapped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		params string
	}{
		{"direct", MethodGrokExitPlanMode, grokPlanParams},
		{"wrapped", MethodGrokExitPlanModeWrapped, grokPlanWrappedParams},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, DialectGrok)
			var got CreatePlanRequest
			p.client.SetPlanHandler(func(_ Arrival, req CreatePlanRequest) PlanDecision {
				got = req
				return PlanDecision{Accept: true}
			})
			p.send(t, 1, tc.method, tc.params)
			msg := p.readWithin(t, 3*time.Second, "grok plan reply")
			if string(msg.Result) != `{"outcome":"approved"}` {
				t.Fatalf("reply %s", msg.Result)
			}
			if got.PlanText() != "## Steps\n\n- read main.go\n" {
				t.Fatalf("plan %q", got.PlanText())
			}
		})
	}
}

func TestGrokPlanEmptyAndRejectAndCancel(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.client.SetPlanHandler(func(_ Arrival, req CreatePlanRequest) PlanDecision {
		if req.PlanText() != GrokEmptyPlanMarkdown {
			t.Errorf("empty plan %q", req.PlanText())
		}
		return PlanDecision{}
	})
	p.send(t, 1, MethodGrokExitPlanMode, `{"sessionId":"s1","toolCallId":"t"}`)
	if got := string(p.read(t).Result); got != `{"outcome":"abandoned"}` {
		t.Fatalf("reject %s", got)
	}

	p2 := newRawPipeDialect(t, DialectGrok)
	p2.client.SetPlanHandler(func(Arrival, CreatePlanRequest) PlanDecision { return PlanDecision{Cancelled: true} })
	p2.send(t, 2, MethodGrokExitPlanMode, grokPlanParams)
	if got := string(p2.read(t).Result); got != `{"outcome":"cancelled"}` {
		t.Fatalf("cancel %s", got)
	}
}

func TestGrokCompleteIncomingCancelledIsFlat(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	gate := make(chan struct{})
	p.client.SetAskHandler(func(Arrival, AskQuestionRequest) AskDecision {
		<-gate
		return AskDecision{Answers: map[string][]string{"Pick one": {"A"}}}
	})
	p.client.SetPlanHandler(func(Arrival, CreatePlanRequest) PlanDecision {
		<-gate
		return PlanDecision{Accept: true}
	})
	p.send(t, 1, MethodGrokAskUserQuestion, grokAskParams)
	p.send(t, 2, MethodGrokExitPlanMode, grokPlanParams)
	waitUntil(t, func() bool {
		p.client.incomingMu.Lock()
		defer p.client.incomingMu.Unlock()
		return len(p.client.incoming) == 2
	})
	go func() { _ = p.client.Cancel(t.Context()) }()
	seen := map[string]string{}
	for i := 0; i < 2; i++ {
		msg := p.readWithin(t, 3*time.Second, "cancelled reply")
		if string(msg.Result) != `{"outcome":"cancelled"}` {
			t.Fatalf("reply %s = %s", msg.ID, msg.Result)
		}
		seen[string(msg.ID)] = string(msg.Result)
	}
	if len(seen) != 2 {
		t.Fatalf("ids %v", seen)
	}
	close(gate)
}

// TestPickKindPrefersSpelledID pins the grok permission shape: the daemon
// prepends "enable-always-approve" (kind allow_once) to every request, so a
// plain allow-once must land on the option that spells allow-once and not on
// the first allow_once by position.
func TestPickKindPrefersSpelledID(t *testing.T) {
	opts := []PermissionOption{
		{OptionID: "enable-always-approve", Name: "Always approve", Kind: KindAllowOnce},
		{OptionID: "allow-once", Name: "Allow once", Kind: KindAllowOnce},
		{OptionID: "reject-once", Name: "Reject", Kind: KindRejectOnce},
	}
	if id, ok := PickKind(opts, KindAllowOnce); !ok || id != "allow-once" {
		t.Fatalf("allow_once picked %q %v", id, ok)
	}
	if id, ok := PickKind(opts, KindRejectOnce); !ok || id != "reject-once" {
		t.Fatalf("reject_once picked %q %v", id, ok)
	}
	// Without a spelled id the first of the kind still wins, as before.
	if id, ok := PickYoloAllow(opts); !ok || id != "allow-once" {
		t.Fatalf("yolo picked %q %v", id, ok)
	}
	cursor := []PermissionOption{{OptionID: "opt-1", Kind: KindAllowAlways}, {OptionID: "opt-2", Kind: KindAllowOnce}}
	if id, ok := PickYoloAllow(cursor); !ok || id != "opt-1" {
		t.Fatalf("cursor yolo picked %q %v", id, ok)
	}
	if _, ok := PickKind(cursor, KindRejectAlways); ok {
		t.Fatal("a kind the request did not offer is not picked")
	}
}

// TestUnwrapExtParamsShapes pins the three envelopes an x.ai ext method can
// arrive in: flat (live grok 1.0.30), the protocol crate's {"params":{...}},
// and a relay's {"method":..., "params":{...}}.
func TestUnwrapExtParamsShapes(t *testing.T) {
	inner := `{"sessionId":"s1","stopReason":"end_turn"}`
	for _, tc := range []struct{ name, raw string }{
		{"flat", inner},
		{"crate-nested", `{"params":` + inner + `}`},
		{"relay-nested", `{"method":"x.ai/session/prompt_complete","params":` + inner + `}`},
	} {
		n, ok := parseGrokPromptComplete(json.RawMessage(tc.raw))
		if !ok || n.SessionID != "s1" || n.StopReason != "end_turn" {
			t.Fatalf("%s: %+v %v", tc.name, n, ok)
		}
	}
	// A params key that is not an object is part of the payload, not an envelope.
	if got := string(unwrapExtParams(json.RawMessage(`{"params":"x","sessionId":"s1"}`))); got != `{"params":"x","sessionId":"s1"}` {
		t.Fatalf("scalar params must pass through, got %s", got)
	}
}
