package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// cardFrames is testdata/claude-cards/frames.jsonl's frames of one step: real
// claude-agent-acp 0.107.3 frames of L0's ask, plan and steer steps, redacted
// (the session id, the tool call ids, the paths).
func cardFrames(t *testing.T, step string) []claudeFrame {
	t.Helper()
	return stepFrames(t, "claude-cards", step)
}

// stepFrames is one step's frames of testdata/<dir>/frames.jsonl.
func stepFrames(t *testing.T, dir, step string) []claudeFrame {
	t.Helper()
	var out []claudeFrame
	for _, f := range fixtureFrames(t, dir) {
		if f.Step == step {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %s/%s frames", dir, step)
	}
	return out
}

// replayIn sends every incoming frame of a step but its replies, in order,
// and answers with the request (the one frame with both an id and a method).
func replayIn(t *testing.T, p *rawPipe, frames []claudeFrame) *Message {
	t.Helper()
	var req *Message
	for _, f := range frames {
		if f.Dir != "in" || f.Msg.Method == "" {
			continue
		}
		if len(f.Msg.ID) > 0 {
			m := f.Msg
			req = &m
		}
		var id any
		if len(f.Msg.ID) > 0 {
			id = json.RawMessage(f.Msg.ID)
		}
		p.send(t, id, f.Msg.Method, string(f.Msg.Params))
	}
	return req
}

func outResult(t *testing.T, frames []claudeFrame) string {
	t.Helper()
	for _, f := range frames {
		if f.Dir == "out" && f.Msg.Method == "" {
			return string(f.Msg.Result)
		}
	}
	t.Fatal("no reply frame")
	return ""
}

// TestClaudeAskFrames replays the real AskUserQuestion exchange (§ B4): the
// form becomes one question whose prompt is the request's message, whose
// options are the consts with their titles and descriptions, and which takes
// free text (question_0_custom). Answered Red with the text "blue", the reply
// is byte for byte what the real exchange sent.
func TestClaudeAskFrames(t *testing.T) {
	frames := cardFrames(t, "ask")
	p := newRawPipeDialect(t, DialectClaude)
	p.setSession("SESSION")
	asked := make(chan AskQuestionRequest, 1)
	p.client.SetAskHandler(func(_ Arrival, req AskQuestionRequest) AskDecision {
		asked <- req
		return AskDecision{Answers: map[string][]string{"question_0": {"Red"}}, Text: map[string]string{"question_0": "blue"}}
	})
	replayIn(t, p, frames)
	got := p.readWithin(t, 3*time.Second, "the elicitation's answer")
	if want := outResult(t, frames); string(got.Result) != want {
		t.Fatalf("answer\n got %s\nwant %s", got.Result, want)
	}
	req := <-asked
	want := AskQuestionRequest{ToolCallID: "toolu_ASK", Questions: []AskQuestion{{
		ID: "question_0", Prompt: "Do you prefer red or blue?", AllowText: true,
		Options: []AskOption{{ID: "Red", Label: "Red", Description: "Choose red"}, {ID: "Blue", Label: "Blue", Description: "Choose blue"}},
	}}}
	if !askRequestsEqual(req, want) {
		t.Fatalf("request %+v\nwant %+v", req, want)
	}
}

func askRequestsEqual(a, b AskQuestionRequest) bool {
	ja, _ := json.Marshal(struct {
		R AskQuestionRequest
		X []bool
		D [][]string
	}{a, allowTexts(a), descriptions(a)})
	jb, _ := json.Marshal(struct {
		R AskQuestionRequest
		X []bool
		D [][]string
	}{b, allowTexts(b), descriptions(b)})
	return string(ja) == string(jb)
}

func allowTexts(r AskQuestionRequest) []bool {
	var out []bool
	for _, q := range r.Questions {
		out = append(out, q.AllowText)
	}
	return out
}

func descriptions(r AskQuestionRequest) [][]string {
	var out [][]string
	for _, q := range r.Questions {
		var d []string
		for _, o := range q.Options {
			d = append(d, o.Description)
		}
		out = append(out, d)
	}
	return out
}

const twoQuestionForm = `{"mode":"form","sessionId":"s-1","toolCallId":"t1","message":"Please answer the following questions.","requestedSchema":{"type":"object","properties":{` +
	`"question_0":{"type":"string","title":"Color","description":"Which color?","oneOf":[{"const":"Red","title":"Red"},{"const":"Blue","title":"Blue"}]},` +
	`"question_0_custom":{"type":"string","title":"Other"},` +
	`"question_1":{"type":"array","title":"Sizes","description":"Which sizes?","items":{"anyOf":[{"const":"S","title":"Small"},{"const":"L","title":"Large"}]}},` +
	`"question_1_custom":{"type":"string","title":"Other"}}}}`

// TestClaudeElicitationAnswers covers the answers a form gets (§ B4, AC6):
// several questions, a multi-select as a list of consts, free text beside a
// pick or alone, and only offered options; Skip is decline, a cancel cancel,
// and with no handler — unattended — the first option of each question.
func TestClaudeElicitationAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		dec  *AskDecision
		want string
	}{
		{"picks and text", &AskDecision{
			Answers: map[string][]string{"question_0": {"Blue", "made-up"}, "question_1": {"S", "L"}},
			Text:    map[string]string{"question_1": "  XL  "},
		}, `{"action":"accept","content":{"question_0":"Blue","question_1":["S","L"],"question_1_custom":"XL"}}`},
		{"text alone", &AskDecision{Answers: map[string][]string{}, Text: map[string]string{"question_0": "green"}},
			`{"action":"accept","content":{"question_0_custom":"green"}}`},
		{"skip", &AskDecision{Skip: true}, `{"action":"decline"}`},
		{"cancel", &AskDecision{Cancelled: true}, `{"action":"cancel"}`},
		{"unattended", nil, `{"action":"accept","content":{"question_0":"Red","question_1":["S"]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, DialectClaude)
			p.setSession("s-1")
			got := make(chan AskQuestionRequest, 1)
			if tc.dec != nil {
				p.client.SetAskHandler(func(_ Arrival, req AskQuestionRequest) AskDecision { got <- req; return *tc.dec })
			}
			p.send(t, 5, MethodElicitationCreate, twoQuestionForm)
			reply := p.readWithin(t, 3*time.Second, "the answer")
			if string(reply.Result) != tc.want {
				t.Fatalf("answer\n got %s\nwant %s", reply.Result, tc.want)
			}
			if tc.dec == nil {
				return
			}
			req := <-got
			if req.Title != "Please answer the following questions." || len(req.Questions) != 2 {
				t.Fatalf("request %+v", req)
			}
			q0, q1 := req.Questions[0], req.Questions[1]
			if q0.ID != "question_0" || q0.Prompt != "Which color?" || q0.AllowMultiple || !q0.AllowText {
				t.Fatalf("question 0 %+v", q0)
			}
			if q1.ID != "question_1" || q1.Prompt != "Which sizes?" || !q1.AllowMultiple || !q1.AllowText || q1.Options[1].Label != "Large" {
				t.Fatalf("question 1 %+v", q1)
			}
		})
	}
}

// TestClaudeElicitationUnanswerable: a mode never declared is declined, and a
// form of any other shape is answered cancel — the tool aborted, never run on
// empty answers — with a note, and no card (§ B4).
func TestClaudeElicitationUnanswerable(t *testing.T) {
	q := func(props string) string {
		return `{"mode":"form","sessionId":"s-1","message":"m","requestedSchema":{"type":"object","properties":{` + props + `}}}`
	}
	for _, tc := range []struct {
		name, params, want, kind string
	}{
		{"url mode", `{"mode":"url","sessionId":"s-1","message":"m","url":"https://example.com","elicitationId":"e1"}`, `{"action":"decline"}`, RefusedElicitationMode},
		{"another property", q(`"question_0":{"type":"string","oneOf":[{"const":"a","title":"a"}]},"email":{"type":"string"}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"no options", q(`"question_0":{"type":"string","oneOf":[]}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"free string", q(`"question_0":{"type":"string"}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"number", q(`"question_0":{"type":"number"}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"custom alone", q(`"question_0_custom":{"type":"string"}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"custom not text", q(`"question_0":{"type":"string","oneOf":[{"const":"a","title":"a"}]},"question_0_custom":{"type":"boolean"}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"numeric const", q(`"question_0":{"type":"string","oneOf":[{"const":1,"title":"one"}]}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"duplicate const", q(`"question_0":{"type":"string","oneOf":[{"const":"a"},{"const":"a"}]}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"array of oneOf", q(`"question_0":{"type":"array","items":{"oneOf":[{"const":"a"}]}}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"empty form", q(``), `{"action":"cancel"}`, RefusedElicitationForm},
		{"refusal fallback", q(`"choice":{"type":"string","oneOf":[{"const":"retry_fallback","title":"Retry"},{"const":"cancelled","title":"Keep"}]}`), `{"action":"cancel"}`, RefusedElicitationForm},
		// A malformed envelope is cancelled, never declined (review C3 item
		// 3): a decline would let the tool run on empty answers.
		{"message not a string", `{"mode":"form","message":[],"requestedSchema":{"type":"object","properties":{"question_0":{"type":"string","oneOf":[{"const":"a"}]}}}}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"mode not a string", `{"mode":7,"message":"m"}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"mode empty", `{"mode":"","message":"m"}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"url mode malformed", `{"mode":"url","message":{"x":1},"url":"https://example.com"}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"toolCallId not a string", `{"mode":"form","toolCallId":3,"message":"m","requestedSchema":{"type":"object","properties":{"question_0":{"type":"string","oneOf":[{"const":"a"}]}}}}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"not an object", `[1,2]`, `{"action":"cancel"}`, RefusedElicitationForm},
		// null is not omission (review C3b 1).
		{"mode null", `{"mode":null,"message":"m","requestedSchema":{"type":"object","properties":{"question_0":{"type":"string","oneOf":[{"const":"a"}]}}}}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"message null", `{"mode":"form","message":null,"requestedSchema":{"type":"object","properties":{"question_0":{"type":"string","oneOf":[{"const":"a"}]}}}}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"url mode, message null", `{"mode":"url","message":null,"url":"https://example.com"}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"schema null", `{"mode":"form","message":"m","requestedSchema":null}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"toolCallId null", `{"mode":"form","toolCallId":null,"message":"m","requestedSchema":{"type":"object","properties":{"question_0":{"type":"string","oneOf":[{"const":"a"}]}}}}`, `{"action":"cancel"}`, RefusedElicitationForm},
		// Constraints the card cannot enforce (review C3 item 4).
		{"custom enum", q(`"question_0":{"type":"string","oneOf":[{"const":"a"}]},"question_0_custom":{"type":"string","enum":["No"]}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"custom pattern", q(`"question_0":{"type":"string","oneOf":[{"const":"a"}]},"question_0_custom":{"type":"string","pattern":"^x$"}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"custom maxLength", q(`"question_0":{"type":"string","oneOf":[{"const":"a"}]},"question_0_custom":{"type":"string","maxLength":3}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"custom format", q(`"question_0":{"type":"string","oneOf":[{"const":"a"}]},"question_0_custom":{"type":"string","format":"email"}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"array minItems", q(`"question_0":{"type":"array","minItems":1,"items":{"anyOf":[{"const":"a"}]}}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"array maxItems", q(`"question_0":{"type":"array","maxItems":1,"items":{"anyOf":[{"const":"a"},{"const":"b"}]}}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"items keyword", q(`"question_0":{"type":"array","items":{"type":"string","anyOf":[{"const":"a"}]}}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"single default", q(`"question_0":{"type":"string","default":"a","oneOf":[{"const":"a"}]}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"option keyword", q(`"question_0":{"type":"string","oneOf":[{"const":"a","default":true}]}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"schema keyword", `{"mode":"form","message":"m","requestedSchema":{"type":"object","additionalProperties":false,"properties":{"question_0":{"type":"string","oneOf":[{"const":"a"}]}}}}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"required free text", `{"mode":"form","message":"m","requestedSchema":{"type":"object","required":["question_0_custom"],"properties":{"question_0":{"type":"string","oneOf":[{"const":"a"}]},"question_0_custom":{"type":"string"}}}}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"required unknown", `{"mode":"form","message":"m","requestedSchema":{"type":"object","required":["question_1"],"properties":{"question_0":{"type":"string","oneOf":[{"const":"a"}]}}}}`, `{"action":"cancel"}`, RefusedElicitationForm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, DialectClaude)
			p.setSession("s-1")
			p.client.SetAskHandler(func(Arrival, AskQuestionRequest) AskDecision {
				t.Error("an unanswerable form raised a card")
				return AskDecision{}
			})
			refused := make(chan RefusedRequest, 1)
			p.client.SetRefusedRequestHandler(func(r RefusedRequest) { refused <- r })
			p.send(t, 3, MethodElicitationCreate, tc.params)
			if got := string(p.readWithin(t, 3*time.Second, "the answer").Result); got != tc.want {
				t.Fatalf("answer %s, want %s", got, tc.want)
			}
			select {
			case r := <-refused:
				if r.Kind != tc.kind || r.Detail == "" {
					t.Fatalf("refusal %+v, want kind %s with a detail", r, tc.kind)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no refusal note")
			}
		})
	}
}

// TestClaudeElicitationCancelHeld: an elicitation open when the turn is
// cancelled is answered cancel (§ B4's CancelHeld row).
func TestClaudeElicitationCancelHeld(t *testing.T) {
	p := newRawPipeDialect(t, DialectClaude)
	p.setSession("s-1")
	asked := make(chan Arrival, 1)
	p.client.SetAskHandler(func(a Arrival, _ AskQuestionRequest) AskDecision {
		asked <- a
		<-a.Call.Done()
		return AskDecision{Answers: map[string][]string{"question_0": {"a"}}}
	})
	p.send(t, 7, MethodElicitationCreate, `{"mode":"form","sessionId":"s-1","message":"m","requestedSchema":{"type":"object","properties":{"question_0":{"type":"string","oneOf":[{"const":"a","title":"a"}]}}}}`)
	<-asked
	go func() { _ = p.client.Cancel(t.Context()) }()
	if got := string(p.readWithin(t, 3*time.Second, "the cancel's answer").Result); got != `{"action":"cancel"}` {
		t.Fatalf("answer %s", got)
	}
	if m := p.readWithin(t, 3*time.Second, "the session/cancel"); m.Method != MethodSessionCancel {
		t.Fatalf("after the answer: %+v", m)
	}
	requestsDone(t, p)
	p.noReply(t)
}

// planRequest is a claude permission request; the empty strings leave a field
// out.
type planRequest struct {
	name, title, kind, metaName, plan, content string
	options                                    []PermissionOption
}

func (r planRequest) params() string {
	tc := map[string]any{"toolCallId": "toolu_PLAN", "status": "pending"}
	if r.name != "" {
		tc["name"] = r.name
	}
	if r.title != "" {
		tc["title"] = r.title
	}
	if r.kind != "" {
		tc["kind"] = r.kind
	}
	if r.metaName != "" {
		tc["_meta"] = map[string]any{"claudeCode": map[string]any{"toolName": r.metaName}}
	}
	if r.plan != "" {
		tc["rawInput"] = map[string]any{"plan": r.plan}
	}
	if r.content != "" {
		tc["content"] = []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": r.content}}}
	}
	raw, _ := json.Marshal(map[string]any{"sessionId": "s-1", "toolCall": tc, "options": r.options})
	return string(raw)
}

// realExitOptions are ExitPlanMode's options as 0.107.3 offers them.
func realExitOptions() []PermissionOption {
	return []PermissionOption{
		{OptionID: "exit-plan-default", Name: "Yes, manually approve edits", Kind: KindAllowOnce},
		{OptionID: "exit-plan-clear-auto", Name: "Yes, clear context and use auto mode", Kind: KindAllowAlways},
		{OptionID: "exit-plan-auto", Name: "Yes, and use auto mode", Kind: KindAllowAlways},
		{OptionID: "exit-plan-bypass", Name: "Yes, and bypass permissions", Kind: KindAllowAlways},
		{OptionID: "reject", Name: "No, keep planning", Kind: KindRejectOnce},
	}
}

func realExit() planRequest {
	return planRequest{name: "ExitPlanMode", title: "Approve Plan", kind: "switch_mode", plan: "# Plan\n1. do it", content: "# Plan\n1. do it", options: realExitOptions()}
}

func withOptions(r planRequest, opts ...PermissionOption) planRequest {
	r.options = opts
	return r
}

// claudePlanOdd is every adversarial plan-exit shape (§ B5's tests, AC7's
// claude-plan-odd): each is suspected, none is the safe shape, and each is
// answered with the option named, never a card. The fake agent's
// claude-plan-odd script sends the same shapes.
func claudePlanOdd() []struct {
	name    string
	tool    string // a preceding tool_call's stamped name, "" for none
	req     planRequest
	wantOpt string // "" is the cancelled outcome
} {
	r := realExit()
	return []struct {
		name    string
		tool    string
		req     planRequest
		wantOpt string
	}{
		{"missing exit-plan-default", "", withOptions(r, realExitOptions()[1:]...), "reject"},
		{"exit-plan-default allow_always", "", withOptions(r, PermissionOption{OptionID: "exit-plan-default", Kind: KindAllowAlways}, PermissionOption{OptionID: "reject", Kind: KindRejectOnce}), "reject"},
		{"duplicate ids", "", withOptions(r, PermissionOption{OptionID: "exit-plan-default", Kind: KindAllowOnce}, PermissionOption{OptionID: "exit-plan-default", Kind: KindAllowOnce}, PermissionOption{OptionID: "reject", Kind: KindRejectOnce}), "reject"},
		{"contradictory name", "", func() planRequest { x := r; x.name = "Write"; return x }(), "reject"},
		{"contradictory meta", "", func() planRequest { x := r; x.metaName = "Bash"; return x }(), "reject"},
		{"preceding call is another tool", "Write", r, "reject"},
		{"kind edit", "", func() planRequest { x := r; x.kind = "edit"; return x }(), "reject"},
		{"no plan", "", func() planRequest { x := r; x.plan, x.content = "", ""; return x }(), "reject"},
		{"no reject option", "", withOptions(r, realExitOptions()[:4]...), ""},
		// The id is what the adapter acts on: reject, whatever its kind; and
		// never another reject-kind id, which the adapter throws on.
		{"reject of kind allow", "", withOptions(r, PermissionOption{OptionID: "exit-plan-default", Kind: KindAllowOnce}, PermissionOption{OptionID: "reject", Kind: KindAllowAlways}, PermissionOption{OptionID: "deny", Kind: KindRejectOnce}), "reject"},
		{"deny alone", "", withOptions(r, PermissionOption{OptionID: "exit-plan-auto", Kind: KindAllowAlways}, PermissionOption{OptionID: "deny", Kind: KindRejectOnce}), ""},
		{"options only", "", planRequest{title: "Run tool?", kind: "execute", options: []PermissionOption{{OptionID: "exit-plan-bypass", Kind: KindAllowAlways}, {OptionID: "allow-once", Kind: KindAllowOnce}}}, ""},
		{"titled only", "", planRequest{title: "Approve Plan", options: []PermissionOption{{OptionID: "allow-once", Kind: KindAllowOnce}, {OptionID: "allow-with-updates", Kind: KindAllowAlways}, {OptionID: "reject", Kind: KindRejectOnce}}}, "reject"},
	}
}

// TestClaudePlanExitOddShapes (§ B5, AC7): no suspected exit of an odd shape
// reaches a card or the permission handler (so force never sees one), and the
// answer is never an allow_always option nor any exit-plan-* — exit-plan-default
// included.
func TestClaudePlanExitOddShapes(t *testing.T) {
	for _, tc := range claudePlanOdd() {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, DialectClaude)
			p.setSession("s-1")
			p.client.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
				t.Error("an odd plan exit reached the permission handler")
				return PermissionDecision{OptionID: "exit-plan-bypass"}
			})
			p.client.SetPlanHandler(func(Arrival, CreatePlanRequest) PlanDecision {
				t.Error("an odd plan exit raised the plan card")
				return PlanDecision{Accept: true}
			})
			refused := make(chan RefusedRequest, 1)
			p.client.SetRefusedRequestHandler(func(r RefusedRequest) { refused <- r })
			if tc.tool != "" {
				p.send(t, nil, MethodSessionUpdate, `{"sessionId":"s-1","update":{"_meta":{"claudeCode":{"toolName":"`+tc.tool+`"}},"toolCallId":"toolu_PLAN","sessionUpdate":"tool_call","status":"pending"}}`)
			}
			p.send(t, 4, MethodRequestPermission, tc.req.params())
			reply := p.readWithin(t, 3*time.Second, "the answer")
			assertNoPlanBypass(t, tc.req.options, reply.Result)
			want := cancelledReply
			if tc.wantOpt != "" {
				want = string(selectedOutcome(tc.wantOpt))
			}
			if string(reply.Result) != want {
				t.Fatalf("answer %s, want %s", reply.Result, want)
			}
			select {
			case r := <-refused:
				if r.Kind != RefusedPlanExitShape || r.Detail == "" {
					t.Fatalf("refusal %+v", r)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no note for an odd plan exit")
			}
		})
	}
}

// assertNoPlanBypass is AC7's invariant on one reply: no allow_always option
// was selected, and no exit-plan-* other than exit-plan-default.
func assertNoPlanBypass(t *testing.T, opts []PermissionOption, result json.RawMessage) {
	t.Helper()
	var r struct {
		Outcome struct {
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	_ = json.Unmarshal(result, &r)
	id := r.Outcome.OptionID
	if id == "" || id == ClaudeExitPlanReject {
		// reject is the adapter's keep planning, whatever kind it carries.
		return
	}
	if strings.HasPrefix(id, "exit-plan-") && id != ClaudeExitPlanDefault {
		t.Fatalf("selected %s", id)
	}
	for _, o := range opts {
		if o.OptionID == id && o.Kind == KindAllowAlways {
			t.Fatalf("selected allow_always option %s", id)
		}
	}
}

// TestClaudePlanExitSafeShapes: the safe shape raises the plan card whatever
// its options' order, with or without a preceding tool_call and its name, from
// rawInput.plan or else the content; approve sends exit-plan-default, keep
// planning reject, a cancel the cancelled outcome. The permission handler —
// and so force — never sees it.
func TestClaudePlanExitSafeShapes(t *testing.T) {
	r := realExit()
	reordered := slices.Clone(realExitOptions())
	slices.Reverse(reordered)
	noName := r
	noName.name = ""
	contentOnly := r
	contentOnly.plan = ""
	for _, tc := range []struct {
		name string
		tool string
		req  planRequest
		dec  PlanDecision
		want string
	}{
		{"approve", "ExitPlanMode", r, PlanDecision{Accept: true}, `{"outcome":{"outcome":"selected","optionId":"exit-plan-default"}}`},
		{"keep planning", "ExitPlanMode", r, PlanDecision{}, `{"outcome":{"outcome":"selected","optionId":"reject"}}`},
		{"cancelled", "ExitPlanMode", r, PlanDecision{Cancelled: true}, cancelledReply},
		{"reordered options", "", withOptions(r, reordered...), PlanDecision{Accept: true}, `{"outcome":{"outcome":"selected","optionId":"exit-plan-default"}}`},
		{"no preceding tool_call, no name", "", noName, PlanDecision{Accept: true}, `{"outcome":{"outcome":"selected","optionId":"exit-plan-default"}}`},
		{"plan from content", "", contentOnly, PlanDecision{}, `{"outcome":{"outcome":"selected","optionId":"reject"}}`},
		{"minimal options", "", withOptions(r, PermissionOption{OptionID: "reject", Kind: KindRejectOnce}, PermissionOption{OptionID: "exit-plan-default", Kind: KindAllowOnce}), PlanDecision{Accept: true}, `{"outcome":{"outcome":"selected","optionId":"exit-plan-default"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, DialectClaude)
			p.setSession("s-1")
			p.client.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
				t.Error("a plan exit reached the permission handler")
				return PermissionDecision{OptionID: "exit-plan-auto"}
			})
			plans := make(chan CreatePlanRequest, 1)
			p.client.SetPlanHandler(func(_ Arrival, req CreatePlanRequest) PlanDecision { plans <- req; return tc.dec })
			if tc.tool != "" {
				p.send(t, nil, MethodSessionUpdate, `{"sessionId":"s-1","update":{"_meta":{"claudeCode":{"toolName":"`+tc.tool+`"}},"toolCallId":"toolu_PLAN","sessionUpdate":"tool_call","status":"pending"}}`)
			}
			p.send(t, 4, MethodRequestPermission, tc.req.params())
			reply := p.readWithin(t, 3*time.Second, "the answer")
			if string(reply.Result) != tc.want {
				t.Fatalf("answer %s, want %s", reply.Result, tc.want)
			}
			assertNoPlanBypass(t, tc.req.options, reply.Result)
			if got := (<-plans).PlanText(); got != "# Plan\n1. do it" {
				t.Fatalf("plan %q", got)
			}
		})
	}
}

// TestClaudePlanFrames replays the real ExitPlanMode exchange: the request is
// the safe shape, the plan card gets rawInput.plan, and approving it sends
// what the real exchange sent.
func TestClaudePlanFrames(t *testing.T) {
	frames := cardFrames(t, "plan")
	p := newRawPipeDialect(t, DialectClaude)
	p.setSession("SESSION")
	plans := make(chan CreatePlanRequest, 1)
	p.client.SetPlanHandler(func(_ Arrival, req CreatePlanRequest) PlanDecision { plans <- req; return PlanDecision{Accept: true} })
	p.client.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
		t.Error("the real plan exit reached the permission handler")
		return PermissionDecision{}
	})
	replayIn(t, p, frames)
	got := p.readWithin(t, 3*time.Second, "the plan's answer")
	if want := outResult(t, frames); string(got.Result) != want {
		t.Fatalf("answer\n got %s\nwant %s", got.Result, want)
	}
	if plan := (<-plans).PlanText(); !strings.HasPrefix(plan, "# Plan: create plan.txt") {
		t.Fatalf("plan %q", plan)
	}
}

// TestClaudeEnterPlanModeIsAPermission: EnterPlanMode's request is of kind
// switch_mode too, but entering plan mode allows nothing: the real-shaped
// request (its own name and title EnterPlanMode, the adapter's allow-once and
// reject) is an ordinary permission, which force answers allow-once.
func TestClaudeEnterPlanModeIsAPermission(t *testing.T) {
	p := newRawPipeDialect(t, DialectClaude)
	p.setSession("s-1")
	perms := make(chan PermissionRequest, 1)
	p.client.SetPermissionHandler(func(_ Arrival, req PermissionRequest) PermissionDecision {
		perms <- req
		id, _ := PickKind(req.Options, KindAllowOnce)
		return PermissionDecision{OptionID: id}
	})
	p.client.SetPlanHandler(func(Arrival, CreatePlanRequest) PlanDecision {
		t.Error("EnterPlanMode raised the plan card")
		return PlanDecision{}
	})
	p.send(t, nil, MethodSessionUpdate, `{"sessionId":"s-1","update":{"_meta":{"claudeCode":{"toolName":"EnterPlanMode"}},"toolCallId":"toolu_PLAN","sessionUpdate":"tool_call","kind":"switch_mode","title":"EnterPlanMode"}}`)
	req := planRequest{name: "EnterPlanMode", title: "EnterPlanMode", kind: "switch_mode", options: []PermissionOption{
		{OptionID: "allow-once", Name: "Yes, enter plan mode", Kind: KindAllowOnce}, {OptionID: "reject", Name: "No, start implementing now", Kind: KindRejectOnce},
	}}
	p.send(t, 4, MethodRequestPermission, req.params())
	if got := string(p.readWithin(t, 3*time.Second, "the answer").Result); got != `{"outcome":{"outcome":"selected","optionId":"allow-once"}}` {
		t.Fatalf("answer %s", got)
	}
	if len(perms) != 1 {
		t.Fatal("the request did not reach the permission handler")
	}
}

// TestClaudeOpenExitIsNeverForgotten is review C3b item 2's sequence: the
// active session names t ExitPlanMode, then announces 256 other tool calls. t
// is still open, so it is not forgotten, and a malformed permission about it
// that calls itself EnterPlanMode and offers allow-once is a suspect: never
// force's. Once t has settled it may be forgotten like any other call.
func TestClaudeOpenExitIsNeverForgotten(t *testing.T) {
	p := newRawPipeDialect(t, DialectClaude)
	p.setSession("s-1")
	p.client.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
		t.Error("the malformed exit reached the permission handler (force)")
		return PermissionDecision{OptionID: "allow-once"}
	})
	tool := func(id, name, status string) {
		p.send(t, nil, MethodSessionUpdate, `{"sessionId":"s-1","update":{"_meta":{"claudeCode":{"toolName":"`+name+`"}},"toolCallId":"`+id+`","sessionUpdate":"tool_call","status":"`+status+`"}}`)
	}
	tool("t", "ExitPlanMode", "pending")
	for i := range claudeToolsCap {
		tool(fmt.Sprintf("other-%d", i), "Read", "pending")
	}
	waitUntil(t, func() bool { return p.client.claudeTool("s-1", fmt.Sprintf("other-%d", claudeToolsCap-1)).name != "" })
	if !p.client.claudeTool("s-1", "t").exit {
		t.Fatal("the open ExitPlanMode call was forgotten")
	}
	req := planRequest{name: "EnterPlanMode", kind: "switch_mode", options: []PermissionOption{{OptionID: "allow-once", Kind: KindAllowOnce}}}
	params := strings.Replace(req.params(), `"toolu_PLAN"`, `"t"`, 1)
	p.send(t, 4, MethodRequestPermission, params)
	if got := string(p.readWithin(t, 3*time.Second, "the answer").Result); got != cancelledReply {
		t.Fatalf("answer %s", got)
	}
	// Settled, it is forgettable: the next announcement evicts it first.
	tool("t", "ExitPlanMode", "completed")
	tool("one-more", "Read", "pending")
	waitUntil(t, func() bool { return p.client.claudeTool("s-1", "one-more").name != "" })
	if p.client.claudeTool("s-1", "t").name != "" {
		t.Fatal("a settled call was kept past the cap")
	}
}

// TestClaudePlanExitCancelBeforeHandler: a cancel that lands before a safe plan
// exit's handler has run answers it cancelled — never an exit-plan option —
// and the early answer carries the plan.
func TestClaudePlanExitCancelBeforeHandler(t *testing.T) {
	p := newRawPipeDialect(t, DialectClaude)
	early := watchEarly(p.client)
	msg := &Message{JSONRPC: jsonrpcVersion, ID: json.RawMessage(`12`), Method: MethodRequestPermission, Params: json.RawMessage(realExit().params())}
	var req PermissionRequest
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		t.Fatal(err)
	}
	pe := p.client.classifyPlanExit(msg.Params, req)
	if !pe.safe {
		t.Fatalf("the real shape is not safe: %s", pe.why)
	}
	plan, _ := json.Marshal(pe.plan)
	preq := CreatePlanRequest{Plan: plan}
	in := p.client.register(msg, RequestParams{Plan: &preq})
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- p.client.Cancel(t.Context()) }()
	if got := string(p.readWithin(t, 3*time.Second, "the cancel's reply").Result); got != cancelledReply {
		t.Fatalf("the cancel replied %s", got)
	}
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	ran := false
	p.client.runIncoming(in, func(in *pendingReq) { ran = true; p.client.handleClaudePlanExit(in, preq) })
	if ran {
		t.Fatal("the plan handler ran for a request the cancel had answered")
	}
	ea := oneEarlyAnswer(t, early)
	if ea.Params.Plan == nil || ea.Params.Plan.PlanText() != "# Plan\n1. do it" {
		t.Fatalf("early answer %+v", ea)
	}
	p.noReply(t)
}

// TestClaudePlanExitNoHandlerRejects: with no plan handler a plan exit is
// rejected; nothing approves a plan by default.
func TestClaudePlanExitNoHandlerRejects(t *testing.T) {
	p := newRawPipeDialect(t, DialectClaude)
	p.setSession("s-1")
	p.send(t, 4, MethodRequestPermission, realExit().params())
	if got := string(p.readWithin(t, 3*time.Second, "the answer").Result); got != `{"outcome":{"outcome":"selected","optionId":"reject"}}` {
		t.Fatalf("answer %s", got)
	}
}

// TestClaudeSteering (§ B7, AC9): with _meta.steering.supported, Interject is
// _session/steering with the text as one block and idleBehavior
// promptRequired; injected is success and tells the interjection handler;
// promptRequired is ErrSteerIdle with no handler call; startedNewTurn is
// ErrSteerStartedTurn. Without the flag, ErrUnsupported and nothing on the
// wire. The real exchange's reply is replayed.
func TestClaudeSteering(t *testing.T) {
	frames := cardFrames(t, "steer")
	var injected string
	for _, f := range frames {
		if f.Dir == "in" {
			injected = string(f.Msg.Result)
		}
	}
	for _, tc := range []struct {
		name, reply string
		want        error
		told        bool
	}{
		{"injected (real)", injected, nil, true},
		{"prompt required", `{"outcome":"promptRequired","reason":"noRunningTurn"}`, ErrSteerIdle, false},
		{"started new turn", `{"outcome":"startedNewTurn"}`, ErrSteerStartedTurn, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, DialectClaude)
			initDone := make(chan struct{})
			go func() { _, _ = p.client.Initialize(t.Context()); close(initDone) }()
			init := p.readWithin(t, 3*time.Second, "initialize")
			if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: init.ID, Result: json.RawMessage(`{"protocolVersion":1,"_meta":{"steering":{"supported":true}}}`)}); err != nil {
				t.Fatal(err)
			}
			<-initDone
			p.setSession("SESSION")
			var mu sync.Mutex
			var told []InterjectionNotification
			p.client.SetInterjectionHandler(func(n InterjectionNotification) { mu.Lock(); told = append(told, n); mu.Unlock() })
			errc := make(chan error, 1)
			go func() { errc <- p.client.Interject(t.Context(), "The secret word is banana.", "craze-1") }()
			req := p.readWithin(t, 3*time.Second, "the steer")
			if req.Method != MethodClaudeSteering {
				t.Fatalf("method %s", req.Method)
			}
			wantParams := `{"_meta":{"steering":{"idleBehavior":"promptRequired"}},"prompt":[{"type":"text","text":"The secret word is banana."}],"sessionId":"SESSION"}`
			if string(req.Params) != wantParams {
				t.Fatalf("params\n got %s\nwant %s", req.Params, wantParams)
			}
			if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: req.ID, Result: json.RawMessage(tc.reply)}); err != nil {
				t.Fatal(err)
			}
			if err := <-errc; err != tc.want {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.told != (len(told) == 1) || (tc.told && (told[0].Text != "The secret word is banana." || told[0].InterjectionID != "craze-1")) {
				t.Fatalf("interjection handler got %+v", told)
			}
		})
	}
	t.Run("not advertised", func(t *testing.T) {
		p := newRawPipeDialect(t, DialectClaude)
		p.setSession("SESSION")
		if err := p.client.Interject(t.Context(), "x", "craze-1"); err != ErrUnsupported {
			t.Fatalf("err %v, want ErrUnsupported", err)
		}
		p.noReply(t)
	})
}

// TestClaudeToolNamesAreTheSessionsOwn is review C3 item 1's sequence: a
// foreign session's tool call named EnterPlanMode (its update dropped) cannot
// lend that name to the active session's call of the same id, which the
// adapter named ExitPlanMode. A malformed exit about it — kind switch_mode, no
// name or title of its own, allow-once in place of exit-plan-default — is a
// suspect, and rejected; force never sees it.
func TestClaudeToolNamesAreTheSessionsOwn(t *testing.T) {
	malformed := planRequest{kind: "switch_mode", plan: "# Plan", options: []PermissionOption{
		{OptionID: "allow-once", Kind: KindAllowOnce}, {OptionID: "reject", Kind: KindRejectOnce},
	}}
	stamp := func(p *rawPipe, sid, name string) {
		p.send(t, nil, MethodSessionUpdate, `{"sessionId":"`+sid+`","update":{"_meta":{"claudeCode":{"toolName":"`+name+`"}},"toolCallId":"toolu_PLAN","sessionUpdate":"tool_call","kind":"switch_mode"}}`)
	}
	ownEnter := malformed
	ownEnter.name = "EnterPlanMode"
	for _, tc := range []struct {
		name  string
		stamp func(p *rawPipe)
		req   planRequest
	}{
		{"the review's sequence", func(p *rawPipe) { stamp(p, "foreign", "EnterPlanMode"); stamp(p, "s-1", "ExitPlanMode") }, malformed},
		// A remembered EnterPlanMode alone never makes a request ordinary:
		// the request does not say so itself.
		{"a remembered EnterPlanMode alone", func(p *rawPipe) { stamp(p, "s-1", "EnterPlanMode") }, malformed},
		// A call the adapter named twice is a suspect, though its latest name
		// and the request's own both say EnterPlanMode.
		{"renamed", func(p *rawPipe) { stamp(p, "s-1", "ExitPlanMode"); stamp(p, "s-1", "EnterPlanMode") }, ownEnter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, DialectClaude)
			p.setSession("s-1")
			p.client.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
				t.Error("the malformed exit reached the permission handler (force)")
				return PermissionDecision{OptionID: "allow-once"}
			})
			tc.stamp(p)
			p.send(t, 4, MethodRequestPermission, tc.req.params())
			if got := string(p.readWithin(t, 3*time.Second, "the answer").Result); got != `{"outcome":{"outcome":"selected","optionId":"reject"}}` {
				t.Fatalf("answer %s", got)
			}
		})
	}
}

// TestClaudeToolNamesForgottenOnANewSession: a tool call remembered from the
// session before a session/new names nothing in the new one.
func TestClaudeToolNamesForgottenOnANewSession(t *testing.T) {
	p := newRawPipeDialect(t, DialectClaude)
	p.setSession("s-1")
	// A dropped session's tool call is never remembered.
	p.send(t, nil, MethodSessionUpdate, `{"sessionId":"foreign","update":{"_meta":{"claudeCode":{"toolName":"EnterPlanMode"}},"toolCallId":"t1","sessionUpdate":"tool_call"}}`)
	p.send(t, nil, MethodSessionUpdate, `{"sessionId":"s-1","update":{"_meta":{"claudeCode":{"toolName":"ExitPlanMode"}},"toolCallId":"t1","sessionUpdate":"tool_call"}}`)
	waitUntil(t, func() bool { return p.client.claudeTool("s-1", "t1").name == "ExitPlanMode" })
	if got := p.client.claudeTool("foreign", "t1"); got.name != "" || p.client.claudeTool("s-1", "t1").conflicts {
		t.Fatalf("a dropped update was remembered: %+v", got)
	}
	go func() { _, _ = p.client.NewSession(t.Context(), t.TempDir(), SessionParams{}) }()
	req := p.readWithin(t, 3*time.Second, "session/new")
	if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: req.ID, Result: json.RawMessage(`{"sessionId":"s-1"}`)}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return p.client.claudeTool("s-1", "t1").name == "" })
}

// TestClaudeElicitationRequired (review C3 item 4): a form's `required` is the
// one constraint beyond the adapter's own form that the card takes, and it is
// carried to the question, which must then be answered.
func TestClaudeElicitationRequired(t *testing.T) {
	_, req, why := parseElicitation(claudeKeys, json.RawMessage(`{"mode":"form","message":"m","requestedSchema":{"type":"object","required":["question_1"],"properties":{`+
		`"question_0":{"type":"string","oneOf":[{"const":"a"}]},"question_1":{"type":"array","items":{"anyOf":[{"const":"b"}]}}}}}`))
	if why != "" || len(req.Questions) != 2 || req.Questions[0].Required || !req.Questions[1].Required {
		t.Fatalf("%q %+v", why, req)
	}
}

// TestClaudeCancelRequestWithdraws (review C3 item 6): the adapter's
// $/cancel_request withdraws the open request it names. The handler's context
// ends with ErrRequestWithdrawn, nothing is written to the agent afterwards —
// the decision that races the withdrawal included, which is reported lost as
// withdrawn — and a request nobody holds is ignored.
func TestClaudeCancelRequestWithdraws(t *testing.T) {
	for _, method := range []string{MethodElicitationCreate, MethodRequestPermission} {
		t.Run(method, func(t *testing.T) {
			p := newRawPipeDialect(t, DialectClaude)
			p.setSession("s-1")
			parked := make(chan Arrival, 1)
			disp := make(chan ReplyDisposition, 1)
			p.client.SetAskHandler(func(a Arrival, _ AskQuestionRequest) AskDecision {
				parked <- a
				<-a.Call.Done()
				return AskDecision{Answers: map[string][]string{"question_0": {"a"}}, Replied: func(d ReplyDisposition) { disp <- d }}
			})
			p.client.SetPermissionHandler(func(a Arrival, _ PermissionRequest) PermissionDecision {
				parked <- a
				<-a.Call.Done()
				return PermissionDecision{OptionID: "allow-once", Replied: func(d ReplyDisposition) { disp <- d }}
			})
			params := `{"mode":"form","sessionId":"s-1","message":"m","requestedSchema":{"type":"object","properties":{"question_0":{"type":"string","oneOf":[{"const":"a"}]}}}}`
			if method == MethodRequestPermission {
				params = `{"sessionId":"s-1","toolCall":{"toolCallId":"t1","name":"Write","kind":"edit","title":"Write x"},"options":[{"optionId":"allow-once","kind":"allow_once"},{"optionId":"reject","kind":"reject_once"}]}`
			}
			p.send(t, 7, method, params)
			a := <-parked
			p.send(t, nil, MethodCancelRequest, `{"requestId":99}`)
			p.send(t, nil, MethodCancelRequest, `{"requestId":7}`)
			select {
			case <-a.Call.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("the withdrawal did not end the request's context")
			}
			if !errors.Is(context.Cause(a.Call), ErrRequestWithdrawn) {
				t.Fatalf("cause %v", context.Cause(a.Call))
			}
			if d := <-disp; d.Delivered || d.Lost != ReplyLostWithdrawn {
				t.Fatalf("the racing decision %+v, want lost as withdrawn", d)
			}
			p.noReply(t)
		})
	}
}

// TestClaudeCancelRequestBeforeTheHandler: a request withdrawn before its
// handler ran never runs it; the early answer says withdrawn, and nothing is
// written.
func TestClaudeCancelRequestBeforeTheHandler(t *testing.T) {
	p := newRawPipeDialect(t, DialectClaude)
	early := watchEarly(p.client)
	req := AskQuestionRequest{Questions: []AskQuestion{{ID: "question_0", Options: []AskOption{{ID: "a"}}}}}
	in := p.client.register(&Message{JSONRPC: jsonrpcVersion, ID: json.RawMessage(`5`), Method: MethodElicitationCreate}, RequestParams{Ask: &req})
	p.send(t, nil, MethodCancelRequest, `{"requestId":5}`)
	waitUntil(t, func() bool { return in.ctx.Err() != nil })
	ran := false
	p.client.runIncoming(in, func(*pendingReq) { ran = true })
	if ran {
		t.Fatal("the handler ran for a withdrawn request")
	}
	if ea := oneEarlyAnswer(t, early); ea.Reason != EarlyWithdrawn || ea.Params.Ask == nil {
		t.Fatalf("early answer %+v", ea)
	}
	p.noReply(t)
}

// TestClaudeRequestsNeedTheirSession (review C3c 1): a claude permission or
// elicitation whose sessionId is missing, null, not a string, or names no
// session craze routes is malformed. The review's sequence — t remembered as
// ExitPlanMode in s-1, then a permission about t that calls itself
// EnterPlanMode and offers allow-once, with no session — never reaches force:
// with no session to look t up in, it reads as an EnterPlanMode and is
// cancelled, as anything else is; a suspected exit would get reject. Each gets
// a note.
func TestClaudeRequestsNeedTheirSession(t *testing.T) {
	exitish := `"toolCall":{"toolCallId":"t","name":"EnterPlanMode","kind":"switch_mode","title":"EnterPlanMode"},"options":[{"optionId":"allow-once","kind":"allow_once"},{"optionId":"reject","kind":"reject_once"}]`
	write := `"toolCall":{"toolCallId":"w","name":"Write","kind":"edit","title":"Write x"},"options":[{"optionId":"allow-once","kind":"allow_once"}]`
	form := `"mode":"form","message":"m","requestedSchema":{"type":"object","properties":{"question_0":{"type":"string","oneOf":[{"const":"a"}]}}}`
	for _, tc := range []struct {
		name, method, params, want string
	}{
		{"exit, no session", MethodRequestPermission, `{` + exitish + `}`, cancelledReply},
		{"exit, null session", MethodRequestPermission, `{"sessionId":null,` + exitish + `}`, cancelledReply},
		{"exit, empty session", MethodRequestPermission, `{"sessionId":"",` + exitish + `}`, cancelledReply},
		{"exit, unrouted session", MethodRequestPermission, `{"sessionId":"foreign",` + exitish + `}`, cancelledReply},
		{"write, no session", MethodRequestPermission, `{` + write + `}`, cancelledReply},
		{"write, unrouted session", MethodRequestPermission, `{"sessionId":"foreign",` + write + `}`, cancelledReply},
		{"form, unrouted session", MethodElicitationCreate, `{"sessionId":"foreign",` + form + `}`, `{"action":"cancel"}`},
		{"form, no session", MethodElicitationCreate, `{` + form + `}`, `{"action":"cancel"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, DialectClaude)
			p.setSession("s-1")
			p.client.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
				t.Error("a request with no session of craze's reached the permission handler (force)")
				return PermissionDecision{OptionID: "allow-once"}
			})
			p.client.SetAskHandler(func(Arrival, AskQuestionRequest) AskDecision {
				t.Error("a request with no session of craze's raised a card")
				return AskDecision{}
			})
			refused := make(chan RefusedRequest, 1)
			p.client.SetRefusedRequestHandler(func(r RefusedRequest) { refused <- r })
			p.send(t, nil, MethodSessionUpdate, `{"sessionId":"s-1","update":{"_meta":{"claudeCode":{"toolName":"ExitPlanMode"}},"toolCallId":"t","sessionUpdate":"tool_call","status":"pending"}}`)
			p.send(t, 4, tc.method, tc.params)
			if got := string(p.readWithin(t, 3*time.Second, "the answer").Result); got != tc.want {
				t.Fatalf("answer %s, want %s", got, tc.want)
			}
			select {
			case r := <-refused:
				if r.Kind != RefusedRequestSession {
					t.Fatalf("refusal %+v", r)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no note")
			}
		})
	}
}

// TestClaudeRequestWithANonStringSessionIsRefused: a sessionId that is not a
// string does not decode as a permission request at all: an invalid-params
// error, which reaches no handler.
func TestClaudeRequestWithANonStringSessionIsRefused(t *testing.T) {
	p := newRawPipeDialect(t, DialectClaude)
	p.setSession("s-1")
	p.client.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
		t.Error("reached the permission handler")
		return PermissionDecision{}
	})
	p.send(t, 4, MethodRequestPermission, `{"sessionId":7,"toolCall":{"toolCallId":"w","name":"Write","kind":"edit"},"options":[{"optionId":"allow-once","kind":"allow_once"}]}`)
	if m := p.readWithin(t, 3*time.Second, "the answer"); m.Error == nil || m.Error.Code != CodeInvalidParams {
		t.Fatalf("answer %+v", m)
	}
}

// TestClaudeSuspectedExitWithNoSessionIsRejected: a request that is a
// suspected exit on its own (exit-plan-* options) and names no session gets
// the unsafe exit's answer, reject.
func TestClaudeSuspectedExitWithNoSessionIsRejected(t *testing.T) {
	p := newRawPipeDialect(t, DialectClaude)
	p.setSession("s-1")
	r := realExit()
	params := strings.Replace(r.params(), `"sessionId":"s-1",`, ``, 1)
	if strings.Contains(params, "sessionId") {
		t.Fatalf("setup: %s", params)
	}
	p.client.SetPlanHandler(func(Arrival, CreatePlanRequest) PlanDecision {
		t.Error("a sessionless exit raised the plan card")
		return PlanDecision{Accept: true}
	})
	p.send(t, 4, MethodRequestPermission, params)
	if got := string(p.readWithin(t, 3*time.Second, "the answer").Result); got != `{"outcome":{"outcome":"selected","optionId":"reject"}}` {
		t.Fatalf("answer %s", got)
	}
}

func toolCount(c *Client) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.claudeToolOrder)
}

// TestClaudeRememberedExitsAreBounded (review C3c 2): open plan exits are kept
// past the cap only while they can still be asked about. A prompted turn's end
// and a cancel settle every exit of that session, and a terminal update that
// names no tool settles its call; once settled, the next announcement brings
// the memory back to the cap, not one call lower.
func TestClaudeRememberedExitsAreBounded(t *testing.T) {
	announce := func(p *rawPipe, prefix string, n int, name string) {
		for i := range n {
			p.send(t, nil, MethodSessionUpdate, fmt.Sprintf(`{"sessionId":"s-1","update":{"_meta":{"claudeCode":{"toolName":%q}},"toolCallId":"%s-%d","sessionUpdate":"tool_call","status":"pending"}}`, name, prefix, i))
		}
		waitUntil(t, func() bool { return p.client.claudeTool("s-1", fmt.Sprintf("%s-%d", prefix, n-1)).name != "" })
	}
	t.Run("a prompted turn's end", func(t *testing.T) {
		p := newRawPipeDialect(t, DialectClaude)
		p.setSession("s-1")
		done := make(chan error, 1)
		go func() { _, err := p.client.Prompt(t.Context(), "plan"); done <- err }()
		prompt := p.readWithin(t, 3*time.Second, "session/prompt")
		announce(p, "exit", 2*claudeToolsCap, "ExitPlanMode")
		if n := toolCount(p.client); n != 2*claudeToolsCap {
			t.Fatalf("open exits kept %d, want all %d", n, 2*claudeToolsCap)
		}
		if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: prompt.ID, Result: json.RawMessage(`{"stopReason":"end_turn"}`)}); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		announce(p, "read", 1, "Read")
		if n := toolCount(p.client); n > claudeToolsCap {
			t.Fatalf("after the turn %d calls kept, want at most %d", n, claudeToolsCap)
		}
	})
	t.Run("a cancel", func(t *testing.T) {
		p := newRawPipeDialect(t, DialectClaude)
		p.setSession("s-1")
		announce(p, "exit", claudeToolsCap+10, "ExitPlanMode")
		go func() { _ = p.client.Cancel(t.Context()) }()
		p.readWithin(t, 3*time.Second, "session/cancel")
		announce(p, "read", 1, "Read")
		if n := toolCount(p.client); n > claudeToolsCap {
			t.Fatalf("after the cancel %d calls kept, want at most %d", n, claudeToolsCap)
		}
	})
	t.Run("a nameless terminal update", func(t *testing.T) {
		p := newRawPipeDialect(t, DialectClaude)
		p.setSession("s-1")
		announce(p, "exit", 1, "ExitPlanMode")
		p.send(t, nil, MethodSessionUpdate, `{"sessionId":"s-1","update":{"toolCallId":"exit-0","sessionUpdate":"tool_call_update","status":"failed"}}`)
		waitUntil(t, func() bool { return p.client.claudeTool("s-1", "exit-0").settled })
	})
}

// TestClaudeWithdrawalEndsTheCallAtOnce (reviews C3c 4, C3d 1): the window
// between ending a withdrawn request's context and marking it withdrawn is
// parked. Inside it the call must already be over, with the withdrawal as its
// cause — which is what makes a first answer racing into that window an
// already_resolved one at the registry — and the request must not yet be
// answerable by anyone: nothing is written to the agent, then or after.
func TestClaudeWithdrawalEndsTheCallAtOnce(t *testing.T) {
	p := newRawPipeDialect(t, DialectClaude)
	req := AskQuestionRequest{Questions: []AskQuestion{{ID: "question_0", Options: []AskOption{{ID: "a"}}}}}
	in := p.client.register(&Message{JSONRPC: jsonrpcVersion, ID: json.RawMessage(`5`), Method: MethodElicitationCreate}, RequestParams{Ask: &req})
	parked := make(chan struct{})
	release := make(chan struct{})
	prev := testWithdrawWindow
	testWithdrawWindow = func(got *pendingReq) {
		if got != in {
			return
		}
		// Held in the window — the context ended, the request not yet
		// marked — until the test has raced both answers against it.
		close(parked)
		<-release
	}
	t.Cleanup(func() { testWithdrawWindow = prev })
	p.send(t, nil, MethodCancelRequest, `{"requestId":5}`)
	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("the withdrawal never ran")
	}
	// The registry's answer, raced in the window: the call's context is its
	// only view of the request, and it already says withdrawn, so no first
	// answer can be taken (AskRegistry.Answer).
	registry := make(chan error, 1)
	go func() { registry <- context.Cause(in.ctx) }()
	if cause := <-registry; !errors.Is(cause, ErrRequestWithdrawn) {
		t.Fatalf("in the window the call's cause was %v: a first answer could still be taken", cause)
	}
	// The handler's reply, raced in the window: it cannot be written while
	// the withdrawal holds the request.
	handler := make(chan ReplyDisposition, 1)
	go func() { handler <- p.client.replyIncoming(in, elicitationResult("accept", nil)) }()
	select {
	case d := <-handler:
		t.Fatalf("a handler reply went through in the window: %+v", d)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	// Marked withdrawn once the window closes, and answered by nobody: the
	// raced reply is lost, not written.
	select {
	case d := <-handler:
		if d.Delivered || d.Lost != ReplyLostWithdrawn {
			t.Fatalf("the raced handler reply %+v; want lost to the withdrawal", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the raced handler reply never returned")
	}
	p.client.incomingMu.Lock()
	marked := in.replied && in.repliedBy == ReplyLostWithdrawn
	p.client.incomingMu.Unlock()
	if !marked {
		t.Fatal("the request was not marked withdrawn")
	}
	p.noReply(t)
}
