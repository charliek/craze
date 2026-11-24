package acp

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The antigravity dialect (plan 053 § B) against agy 1.5.14's real frames
// (testdata/antigravity-*, redacted: the session ids, the paths, the PATH a
// command printed) and the shapes the probe recorded.

// TestAntigravityDialectDecisionsPinned is B1's row: agy handles permission
// requests, every session update and elicitation/create; refuses cursor/*
// and x.ai's ask and plan as claude does; and ignores the rest of x.ai and
// claude's own extensions.
func TestAntigravityDialectDecisionsPinned(t *testing.T) {
	for _, tc := range []struct {
		method string
		want   decision
	}{
		{MethodRequestPermission, decisionHandle},
		{MethodSessionUpdate, decisionHandle},
		{updateMethod(UpdateToolCall), decisionHandle},
		{MethodElicitationCreate, decisionHandle},
		{MethodCursorAskQuestion, decisionRefuse},
		{MethodCursorCreatePlan, decisionRefuse},
		{MethodCursorUpdateTodos, decisionRefuse},
		{MethodCursorTask, decisionRefuse},
		{MethodGrokAskUserQuestion, decisionRefuse},
		{MethodGrokExitPlanModeWrapped, decisionRefuse},
		{MethodGrokPromptComplete, decisionIgnore},
		{MethodGrokSessionNotification, decisionIgnore},
		{MethodGrokQueueChanged, decisionIgnore},
		{MethodClaudeAuthStatus, decisionIgnore},
		{MethodAuthStatusUpdate, decisionIgnore},
		{MethodCancelRequest, decisionIgnore},
	} {
		if got := dialectHandles(DialectAntigravity, tc.method); got != tc.want {
			t.Errorf("antigravity %s = %v, want %v", tc.method, got, tc.want)
		}
	}
}

// TestAntigravityToolNameFromTopLevel (B3): agy names a tool by the update's
// top-level name; one that is not a string is no name; cursor's dialect
// leaves the field to rawInput._toolName, as ever.
func TestAntigravityToolNameFromTopLevel(t *testing.T) {
	for _, tc := range []struct {
		d      DialectID
		update string
		want   string
	}{
		{DialectAntigravity, `{"sessionUpdate":"tool_call","toolCallId":"c1","name":"run_command","status":"in_progress","rawInput":{"CommandLine":"ls"}}`, "run_command"},
		{DialectAntigravity, `{"sessionUpdate":"tool_call","toolCallId":"c1","name":7,"status":"in_progress"}`, ""},
		{DialectAntigravity, `{"sessionUpdate":"tool_call","toolCallId":"c1","status":"in_progress"}`, ""},
		{DialectCursor, `{"sessionUpdate":"tool_call","toolCallId":"c1","name":"run_command","status":"in_progress"}`, ""},
	} {
		p := newRawPipeDialect(t, tc.d)
		p.setSession("s-1")
		got := make(chan SessionNotification, 1)
		p.client.SetUpdateHandler(func(n SessionNotification) { got <- n })
		p.send(t, nil, MethodSessionUpdate, `{"sessionId":"s-1","update":`+tc.update+`}`)
		select {
		case n := <-got:
			if n.ToolName != tc.want {
				t.Fatalf("%s %s: tool name %q, want %q", tc.d, tc.update, n.ToolName, tc.want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no update")
		}
	}
}

// TestAntigravityBasicFrames drives agy's real frames through the dialect
// (frames-04, -06, -09): every tool row named from its top-level name, and the
// run_command permission request — numbered "1", a string — answered allow,
// byte for byte what the probe sent, under the id it came with.
func TestAntigravityBasicFrames(t *testing.T) {
	p := newRawPipeDialect(t, DialectAntigravity)
	p.setSession("SESSION")
	var mu sync.Mutex
	var tools []string
	p.client.SetUpdateHandler(func(n SessionNotification) {
		if n.ToolName != "" {
			mu.Lock()
			if !slices.Contains(tools, n.ToolName) {
				tools = append(tools, n.ToolName)
			}
			mu.Unlock()
		}
	})
	p.client.SetPermissionHandler(func(_ Arrival, req PermissionRequest) PermissionDecision {
		id, _ := PickYoloAllow(req.Options)
		return PermissionDecision{OptionID: id}
	})
	frames := stepFrames(t, "antigravity-basic", "tools")
	req := replayIn(t, p, frames)
	if req == nil || string(req.ID) != `"1"` || req.Method != MethodRequestPermission {
		t.Fatalf("the permission request %+v", req)
	}
	got := p.readWithin(t, 3*time.Second, "the permission answer")
	if string(got.ID) != `"1"` || string(got.Result) != outResult(t, frames) {
		t.Fatalf("answer id %s result %s, want \"1\" %s", got.ID, got.Result, outResult(t, frames))
	}
	for _, step := range []string{"plan", "subagents"} {
		replayIn(t, p, stepFrames(t, "antigravity-basic", step))
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(tools) == 5
	}, "every tool's name")
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"read_file", "write_file", "run_command", "edit_file", "invoke_subagent"}; !slices.Equal(tools, want) {
		t.Fatalf("tool names %v, want %v", tools, want)
	}
}

// TestAntigravityAskFrames replays the real ask_question exchanges (B4,
// frames-05): one question, two, a decline and a cancel. Each form becomes a
// card of q-keyed questions whose prompts are the fields' titles, whose
// options are the 1-based consts with their titles, and which take free text
// (q<i>_other); the answers go back byte for byte as the probe sent them,
// under the string ids the requests carried.
func TestAntigravityAskFrames(t *testing.T) {
	for _, tc := range []struct {
		step  string
		dec   AskDecision
		check func(t *testing.T, req AskQuestionRequest)
	}{
		{"ask", AskDecision{Answers: map[string][]string{"q1": {"2"}}}, func(t *testing.T, req AskQuestionRequest) {
			want := AskQuestionRequest{ToolCallID: "call_17552293310391795862", Questions: []AskQuestion{{
				ID: "q1", Prompt: "What is your favourite colour?", AllowText: true,
				Options: []AskOption{{ID: "1", Label: "(Recommended) Red"}, {ID: "2", Label: "Green"}, {ID: "3", Label: "Blue"}},
			}}}
			if !askRequestsEqual(req, want) {
				t.Fatalf("request %+v\nwant %+v", req, want)
			}
		}},
		{"ask2", AskDecision{Answers: map[string][]string{"q1": {"1"}, "q2": {"2"}}}, func(t *testing.T, req AskQuestionRequest) {
			if req.Title != "The agent has 2 questions." || len(req.Questions) != 2 {
				t.Fatalf("request %+v", req)
			}
			q1, q2 := req.Questions[0], req.Questions[1]
			if q1.ID != "q1" || q1.Prompt != "Which pet do you prefer?" || !q1.AllowText || q2.ID != "q2" || q2.Prompt != "Which drink do you prefer?" || !q2.AllowText {
				t.Fatalf("questions %+v %+v", q1, q2)
			}
		}},
		{"decline", AskDecision{Skip: true}, nil},
		{"cancel", AskDecision{Cancelled: true}, nil},
	} {
		t.Run(tc.step, func(t *testing.T) {
			frames := stepFrames(t, "antigravity-cards", tc.step)
			p := newRawPipeDialect(t, DialectAntigravity)
			p.setSession("SESSION")
			asked := make(chan AskQuestionRequest, 1)
			p.client.SetAskHandler(func(_ Arrival, req AskQuestionRequest) AskDecision { asked <- req; return tc.dec })
			req := replayIn(t, p, frames)
			got := p.readWithin(t, 3*time.Second, "the elicitation's answer")
			if string(got.ID) != string(req.ID) || !strings.HasPrefix(string(got.ID), `"`) {
				t.Fatalf("answered id %s, asked %s", got.ID, req.ID)
			}
			if want := outResult(t, frames); string(got.Result) != want {
				t.Fatalf("answer\n got %s\nwant %s", got.Result, want)
			}
			r := <-asked
			if tc.check != nil {
				tc.check(t, r)
			}
		})
	}
}

// agyForm is an elicitation/create as agy sends one, its properties props.
func agyForm(props string) string {
	return `{"sessionId":"s-1","toolCallId":"call_1","mode":"form","message":"The agent has a question.","requestedSchema":{"type":"object","properties":{` + props + `}}}`
}

const agyQ1 = `"q1":{"type":"string","title":"Colour?","oneOf":[{"const":"1","title":"Red"},{"const":"2","title":"Green"}]}`
const agyQ1Other = `"q1_other":{"type":"string","title":"Other","description":"Optional free-form answer to: Colour?"}`

// TestAntigravityElicitationAnswers (B4): free text goes back as q<i>_other,
// beside a pick or alone; a multi-select (array + anyOf, not captured live)
// as a list of consts; with no handler — unattended — the first option.
func TestAntigravityElicitationAnswers(t *testing.T) {
	multi := agyForm(agyQ1 + `,` + agyQ1Other + `,"q2":{"type":"array","title":"Sizes?","items":{"anyOf":[{"const":"1","title":"S"},{"const":"2","title":"L"}]}}`)
	for _, tc := range []struct {
		name, form string
		dec        *AskDecision
		want       string
	}{
		{"pick and text", agyForm(agyQ1 + `,` + agyQ1Other), &AskDecision{Answers: map[string][]string{"q1": {"2"}}, Text: map[string]string{"q1": " teal "}},
			`{"action":"accept","content":{"q1":"2","q1_other":"teal"}}`},
		{"text alone", agyForm(agyQ1 + `,` + agyQ1Other), &AskDecision{Answers: map[string][]string{}, Text: map[string]string{"q1": "teal"}},
			`{"action":"accept","content":{"q1_other":"teal"}}`},
		{"text where none is taken", agyForm(agyQ1), &AskDecision{Answers: map[string][]string{"q1": {"1"}}, Text: map[string]string{"q1": "teal"}},
			`{"action":"accept","content":{"q1":"1"}}`},
		{"multi-select", multi, &AskDecision{Answers: map[string][]string{"q1": {"1"}, "q2": {"1", "2", "9"}}},
			`{"action":"accept","content":{"q1":"1","q2":["1","2"]}}`},
		{"skip", agyForm(agyQ1), &AskDecision{Skip: true}, `{"action":"decline"}`},
		{"unattended", multi, nil, `{"action":"accept","content":{"q1":"1","q2":["1"]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, DialectAntigravity)
			p.setSession("s-1")
			if tc.dec != nil {
				p.client.SetAskHandler(func(Arrival, AskQuestionRequest) AskDecision { return *tc.dec })
			}
			p.send(t, "7", MethodElicitationCreate, tc.form)
			reply := p.readWithin(t, 3*time.Second, "the answer")
			if string(reply.ID) != `"7"` || string(reply.Result) != tc.want {
				t.Fatalf("answer %s %s, want \"7\" %s", reply.ID, reply.Result, tc.want)
			}
		})
	}
}

// TestAntigravityElicitationRequired: a required question is one the card
// insists on; a required free text, or one the form does not ask, is a
// constraint the card cannot enforce and the form is cancelled.
func TestAntigravityElicitationRequired(t *testing.T) {
	_, req, why := parseElicitation(antigravityKeys, json.RawMessage(`{"mode":"form","message":"m","requestedSchema":{"type":"object","required":["q2"],"properties":{`+
		agyQ1+`,"q2":{"type":"string","title":"Size?","oneOf":[{"const":"1","title":"S"}]}}}}`))
	if why != "" || len(req.Questions) != 2 || req.Questions[0].Required || !req.Questions[1].Required {
		t.Fatalf("why %q questions %+v", why, req.Questions)
	}
	for _, required := range []string{`"q1_other"`, `"q3"`, `"question_0"`} {
		_, _, why := parseElicitation(antigravityKeys, json.RawMessage(`{"mode":"form","message":"m","requestedSchema":{"type":"object","required":[`+required+`],"properties":{`+agyQ1+`,`+agyQ1Other+`}}}`))
		if why == "" {
			t.Fatalf("required %s was accepted", required)
		}
	}
}

// TestAntigravityElicitationUnanswerable: the strict checks are claude's,
// whatever the scheme — a mode never declared is declined, a malformed
// envelope or a null field cancelled — and a form keyed for the other dialect
// (claude's question_<i> on agy, agy's q<i> on claude, a 0-based q0) is
// unrenderable and cancelled, with a note and no card.
func TestAntigravityElicitationUnanswerable(t *testing.T) {
	claudeForm := `{"mode":"form","sessionId":"s-1","message":"m","requestedSchema":{"type":"object","properties":{"question_0":{"type":"string","oneOf":[{"const":"a","title":"a"}]}}}}`
	for _, tc := range []struct {
		name   string
		d      DialectID
		params string
		want   string
		kind   string
	}{
		{"claude keys on antigravity", DialectAntigravity, claudeForm, `{"action":"cancel"}`, RefusedElicitationForm},
		{"q keys on claude", DialectClaude, agyForm(agyQ1 + `,` + agyQ1Other), `{"action":"cancel"}`, RefusedElicitationForm},
		{"q0", DialectAntigravity, agyForm(`"q0":{"type":"string","oneOf":[{"const":"1","title":"a"}]}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"other alone", DialectAntigravity, agyForm(agyQ1Other), `{"action":"cancel"}`, RefusedElicitationForm},
		{"claude's free text beside a q", DialectAntigravity, agyForm(agyQ1 + `,"q1_custom":{"type":"string"}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"another property", DialectAntigravity, agyForm(agyQ1 + `,"email":{"type":"string"}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"no options", DialectAntigravity, agyForm(`"q1":{"type":"string","oneOf":[]}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"numeric const", DialectAntigravity, agyForm(`"q1":{"type":"string","oneOf":[{"const":1,"title":"a"}]}`), `{"action":"cancel"}`, RefusedElicitationForm},
		{"url mode", DialectAntigravity, `{"mode":"url","sessionId":"s-1","message":"m","url":"https://example.com"}`, `{"action":"decline"}`, RefusedElicitationMode},
		{"mode null", DialectAntigravity, `{"mode":null,"sessionId":"s-1","message":"m","requestedSchema":{"type":"object","properties":{` + agyQ1 + `}}}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"message null", DialectAntigravity, `{"mode":"form","sessionId":"s-1","message":null,"requestedSchema":{"type":"object","properties":{` + agyQ1 + `}}}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"schema null", DialectAntigravity, `{"mode":"form","sessionId":"s-1","message":"m","requestedSchema":null}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"toolCallId not a string", DialectAntigravity, `{"mode":"form","sessionId":"s-1","toolCallId":3,"message":"m","requestedSchema":{"type":"object","properties":{` + agyQ1 + `}}}`, `{"action":"cancel"}`, RefusedElicitationForm},
		{"another session", DialectAntigravity, strings.Replace(agyForm(agyQ1), `"s-1"`, `"s-2"`, 1), `{"action":"cancel"}`, RefusedRequestSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, tc.d)
			p.setSession("s-1")
			p.client.SetAskHandler(func(Arrival, AskQuestionRequest) AskDecision {
				t.Error("an unanswerable form raised a card")
				return AskDecision{}
			})
			refused := make(chan RefusedRequest, 1)
			p.client.SetRefusedRequestHandler(func(r RefusedRequest) { refused <- r })
			p.send(t, "3", MethodElicitationCreate, tc.params)
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

// TestAntigravityElicitationCancelHeld: a question open when the turn is
// cancelled is answered cancel, under its string id, before the
// session/cancel goes out.
func TestAntigravityElicitationCancelHeld(t *testing.T) {
	p := newRawPipeDialect(t, DialectAntigravity)
	p.setSession("s-1")
	asked := make(chan Arrival, 1)
	p.client.SetAskHandler(func(a Arrival, _ AskQuestionRequest) AskDecision {
		asked <- a
		<-a.Call.Done()
		return AskDecision{Answers: map[string][]string{"q1": {"1"}}}
	})
	p.send(t, "1", MethodElicitationCreate, agyForm(agyQ1+`,`+agyQ1Other))
	<-asked
	go func() { _ = p.client.Cancel(t.Context()) }()
	got := p.readWithin(t, 3*time.Second, "the cancel's answer")
	if string(got.ID) != `"1"` || string(got.Result) != `{"action":"cancel"}` {
		t.Fatalf("answer %s %s", got.ID, got.Result)
	}
	if m := p.readWithin(t, 3*time.Second, "the session/cancel"); m.Method != MethodSessionCancel {
		t.Fatalf("after the answer: %+v", m)
	}
}

// TestAntigravityLoggedOutFrame pins the error a logged-out agy answers
// session/new with (frames-02): -32000 "Authentication required", no data —
// what the agent's auth classifier reads (plan 053 § A3).
func TestAntigravityLoggedOutFrame(t *testing.T) {
	f := stepFrames(t, "antigravity-basic", "loggedout")[0]
	if f.Msg.Error == nil || f.Msg.Error.Code != -32000 || f.Msg.Error.Message != "Authentication required" || len(f.Msg.Error.Data) != 0 {
		t.Fatalf("logged-out frame %+v", f.Msg.Error)
	}
	p := newRawPipeDialect(t, DialectAntigravity)
	done := make(chan error, 1)
	go func() { _, err := p.client.NewSession(t.Context(), t.TempDir(), SessionParams{}); done <- err }()
	req := p.readWithin(t, 3*time.Second, "session/new")
	if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: req.ID, Error: f.Msg.Error}); err != nil {
		t.Fatal(err)
	}
	var rpc *RPCError
	if err := <-done; err == nil || !errors.As(err, &rpc) || rpc.Code != -32000 {
		t.Fatalf("session/new = %v", err)
	}
}
