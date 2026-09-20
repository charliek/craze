package acp

import (
	"encoding/json"
	"testing"
	"time"
)

// The three request shapes these tests register, one per blocking kind.
const (
	reportPermParams = `{"sessionId":"s1","toolCall":{"toolCallId":"t1","title":"run it"},"options":[{"optionId":"yes","name":"Yes","kind":"allow_once"}]}`
	reportAskParams  = `{"toolCallId":"t2","questions":[{"id":"q1","prompt":"?","options":[{"id":"opt-a","label":"A"}]}]}`
	reportPlanParams = `{"name":"P","plan":"do it"}`
)

const cancelledReply = `{"outcome":{"outcome":"cancelled"}}`

// watchEarly installs the early-answer handler and hands back the channel it
// records on. The channel is buffered so the hook never blocks the goroutine it
// runs on, which is the request's own.
func watchEarly(c *Client) chan EarlyAnswer {
	early := make(chan EarlyAnswer, 4)
	c.SetEarlyAnswerHandler(func(ea EarlyAnswer) { early <- ea })
	return early
}

// oneEarlyAnswer is the single early answer the caller has already driven to
// completion: every caller below either ran runIncoming on its own goroutine or
// waited for it, so a second call could not still be coming.
func oneEarlyAnswer(t *testing.T, early chan EarlyAnswer) EarlyAnswer {
	t.Helper()
	if len(early) != 1 {
		t.Fatalf("the early-answer handler ran %d times, want exactly once", len(early))
	}
	return <-early
}

// oneDisposition is the single Replied call a handler goroutine made. The
// goroutine dropping its request from the incoming map is the barrier for "it
// has finished" — dropIncoming is runIncoming's deferred last act, after the
// reply and after Replied — so once the map is empty the count is final.
func oneDisposition(t *testing.T, p *rawPipe, disp chan ReplyDisposition) ReplyDisposition {
	t.Helper()
	waitUntil(t, func() bool {
		p.client.incomingMu.Lock()
		defer p.client.incomingMu.Unlock()
		return len(p.client.incoming) == 0
	})
	if len(disp) != 1 {
		t.Fatalf("Replied ran %d times, want exactly once", len(disp))
	}
	return <-disp
}

// A cancel can answer a blocking request before its handler goroutine has run
// at all (TestCancelAnswersRequestsWhoseHandlerHasNotRunYet is that race).
// Nothing above the client would otherwise hear that the agent had asked:
// no handler runs, so nothing is published and nothing can be answered. The
// early-answer handler is told instead, with the reason and the request itself.
// runIncoming is called by hand for the reason its neighbours in
// extension_test.go give: that is exactly what a goroutine the runtime
// scheduled late does.
func TestEarlyAnswerByCancelCarriesTheRequest(t *testing.T) {
	p := newRawPipe(t)
	var req PermissionRequest
	if err := json.Unmarshal([]byte(reportPermParams), &req); err != nil {
		t.Fatal(err)
	}
	in := p.client.register(&Message{
		JSONRPC: jsonrpcVersion,
		ID:      json.RawMessage(`11`),
		Method:  MethodRequestPermission,
		Params:  json.RawMessage(reportPermParams),
	}, RequestParams{Permission: &req})
	early := watchEarly(p.client)

	// The cancel's reply goes out on the unbuffered pipe, so it has to be read
	// while the cancel is writing it. There is no session id, so the cancel
	// writes the reply and nothing else.
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- p.client.Cancel(t.Context()) }()
	if got := string(p.readWithin(t, 3*time.Second, "the cancel's reply").Result); got != cancelledReply {
		t.Fatalf("the cancel replied %s", got)
	}
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}

	ran := false
	p.client.runIncoming(in, func(*pendingReq) { ran = true })
	if ran {
		t.Fatal("the handler ran for a request the cancel had already answered")
	}
	ea := oneEarlyAnswer(t, early)
	if ea.Reason != EarlyCancelled {
		t.Fatalf("reason %q, want %q", ea.Reason, EarlyCancelled)
	}
	if ea.Turn != in.turn {
		t.Fatalf("turn %d, want the turn the request arrived in (%d)", ea.Turn, in.turn)
	}
	if ea.Params.Permission == nil || ea.Params.Ask != nil || ea.Params.Plan != nil {
		t.Fatalf("params %+v, want the permission request alone", ea.Params)
	}
	if got := ea.Params.Permission.ToolCall.ToolCallID; got != "t1" {
		t.Fatalf("tool call %q, want the one the agent asked about", got)
	}
	if len(ea.Params.Permission.Options) != 1 || ea.Params.Permission.Options[0].OptionID != "yes" {
		t.Fatalf("options %+v, want the options exactly as offered", ea.Params.Permission.Options)
	}
	p.noReply(t)
}

// Close answers what the client is still blocked on before it takes the agent
// down. That is the same silence, and a different reason: a request answered on
// the way out was never the user's to answer.
func TestEarlyAnswerByCloseIsReported(t *testing.T) {
	p := newRawPipe(t)
	req := CreatePlanRequest{Name: "P", Plan: json.RawMessage(`"do it"`)}
	in := p.client.register(&Message{
		JSONRPC: jsonrpcVersion,
		ID:      json.RawMessage(`12`),
		Method:  MethodCursorCreatePlan,
		Params:  json.RawMessage(reportPlanParams),
	}, RequestParams{Plan: &req})
	early := watchEarly(p.client)

	closed := make(chan error, 1)
	go func() { closed <- p.client.Close() }()
	if got := string(p.readWithin(t, 3*time.Second, "the close's cancelled reply").Result); got != cancelledReply {
		t.Fatalf("close replied %s", got)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}

	ran := false
	p.client.runIncoming(in, func(*pendingReq) { ran = true })
	if ran {
		t.Fatal("the handler ran for a request the close had already answered")
	}
	ea := oneEarlyAnswer(t, early)
	if ea.Reason != EarlyClosed {
		t.Fatalf("reason %q, want %q", ea.Reason, EarlyClosed)
	}
	if ea.Params.Plan == nil || ea.Params.Plan.DisplayName() != "P" {
		t.Fatalf("params %+v, want the plan request", ea.Params)
	}
}

// The third cause: nothing had answered it, but the turn it arrived in was over
// by the time its handler goroutine ran. runIncoming answers it cancelled
// itself, and that answer is the only one the agent ever gets.
func TestEarlyAnswerByStaleTurnIsReported(t *testing.T) {
	p := newRawPipe(t)
	req := AskQuestionRequest{
		ToolCallID: "t2",
		Questions:  []AskQuestion{{ID: "q1", Prompt: "?", Options: []AskOption{{ID: "opt-a", Label: "A"}}}},
	}
	in := p.client.register(&Message{
		JSONRPC: jsonrpcVersion,
		ID:      json.RawMessage(`13`),
		Method:  MethodCursorAskQuestion,
		Params:  json.RawMessage(reportAskParams),
	}, RequestParams{Ask: &req})
	early := watchEarly(p.client)
	// A turn of the user's own, started while the goroutine was waiting.
	p.client.mu.Lock()
	p.client.turn++
	p.client.mu.Unlock()

	ran := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.client.runIncoming(in, func(*pendingReq) { close(ran) })
	}()
	if got := string(p.readWithin(t, 3*time.Second, "the stale request's reply").Result); got != cancelledReply {
		t.Fatalf("the stale request was replied %s", got)
	}
	<-done
	select {
	case <-ran:
		t.Fatal("the handler ran for a request whose turn was over")
	default:
	}
	ea := oneEarlyAnswer(t, early)
	if ea.Reason != EarlyStaleTurn {
		t.Fatalf("reason %q, want %q", ea.Reason, EarlyStaleTurn)
	}
	if ea.Turn != in.turn || p.client.TurnLive(ea.Turn) {
		t.Fatalf("turn %d, want the request's own turn, which is over", ea.Turn)
	}
	if ea.Params.Ask == nil || len(ea.Params.Ask.Questions) != 1 || ea.Params.Ask.Questions[0].ID != "q1" {
		t.Fatalf("params %+v, want the question as asked", ea.Params)
	}
	p.noReply(t)
}

// The hook is for the requests nobody else can see. A request whose handler ran
// is the session's own business, and reporting it too would give it two
// endings.
func TestNoEarlyAnswerWhenTheHandlerRan(t *testing.T) {
	p := newRawPipe(t)
	early := watchEarly(p.client)
	p.client.SetAskHandler(func(int, AskQuestionRequest) AskDecision { return AskDecision{Skip: true} })
	p.send(t, 1, MethodCursorAskQuestion, reportAskParams)
	// The handler's own reply is the barrier: runIncoming took the live path.
	if got := string(p.readWithin(t, 3*time.Second, "the handler's reply").Result); got != `{"outcome":{"outcome":"skipped"}}` {
		t.Fatalf("the handler replied %s", got)
	}
	if n := len(early); n != 0 {
		t.Fatalf("the early-answer handler ran %d times for a request its handler answered", n)
	}
}

// What makes the reports above possible: dispatch registers the request it just
// decoded, so the typed request no longer lives in the run closure alone. Each
// kind is driven through onRequest, the way the agent sends it.
func TestDispatchRegistersTheDecodedRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dialect DialectID
		method  string
		params  string
		check   func(*testing.T, RequestParams)
	}{
		{"permission", DialectCursor, MethodRequestPermission, reportPermParams, func(t *testing.T, got RequestParams) {
			t.Helper()
			if got.Permission == nil || got.Permission.ToolCall.ToolCallID != "t1" {
				t.Fatalf("permission params %+v", got)
			}
		}},
		{"cursor ask", DialectCursor, MethodCursorAskQuestion, reportAskParams, func(t *testing.T, got RequestParams) {
			t.Helper()
			if got.Ask == nil || got.Ask.ToolCallID != "t2" {
				t.Fatalf("ask params %+v", got)
			}
		}},
		{"cursor plan", DialectCursor, MethodCursorCreatePlan, reportPlanParams, func(t *testing.T, got RequestParams) {
			t.Helper()
			if got.Plan == nil || got.Plan.DisplayName() != "P" {
				t.Fatalf("plan params %+v", got)
			}
		}},
		{"grok ask", DialectGrok, MethodGrokAskUserQuestion, grokAskParams, func(t *testing.T, got RequestParams) {
			t.Helper()
			if got.Ask == nil || len(got.Ask.Questions) != 2 || got.Ask.Questions[0].ID != "Pick one" {
				t.Fatalf("grok ask params %+v", got)
			}
		}},
		{"grok plan", DialectGrok, MethodGrokExitPlanMode, grokPlanParams, func(t *testing.T, got RequestParams) {
			t.Helper()
			if got.Plan == nil || got.Plan.PlanText() == "" {
				t.Fatalf("grok plan params %+v", got)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, tc.dialect)
			entered := make(chan struct{})
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			// Exactly one of these runs, and it parks: the request is then
			// registered, its handler is running, and neither has finished.
			p.client.SetPermissionHandler(func(int, PermissionRequest) PermissionDecision {
				close(entered)
				<-release
				return PermissionDecision{Cancelled: true}
			})
			p.client.SetAskHandler(func(int, AskQuestionRequest) AskDecision {
				close(entered)
				<-release
				return AskDecision{Cancelled: true}
			})
			p.client.SetPlanHandler(func(int, CreatePlanRequest) PlanDecision {
				close(entered)
				<-release
				return PlanDecision{Cancelled: true}
			})
			p.send(t, 1, tc.method, tc.params)
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for the handler to start")
			}
			p.client.incomingMu.Lock()
			in := p.client.incoming["1"]
			p.client.incomingMu.Unlock()
			if in == nil {
				t.Fatal("the running handler's request is not registered")
			}
			tc.check(t, in.params)
		})
	}
}

// The ordinary case: the handler's decision is the reply the agent gets, and
// its Replied hook says so.
func TestReplyDispositionDelivered(t *testing.T) {
	p := newRawPipe(t)
	disp := make(chan ReplyDisposition, 4)
	p.client.SetPermissionHandler(func(int, PermissionRequest) PermissionDecision {
		return PermissionDecision{OptionID: "yes", Replied: func(d ReplyDisposition) { disp <- d }}
	})
	p.send(t, 1, MethodRequestPermission, reportPermParams)
	if got := string(p.readWithin(t, 3*time.Second, "the handler's reply").Result); got != `{"outcome":{"outcome":"selected","optionId":"yes"}}` {
		t.Fatalf("reply %s", got)
	}
	if d := oneDisposition(t, p, disp); !d.Delivered || d.Lost != "" {
		t.Fatalf("disposition %+v, want delivered", d)
	}
}

// A deliberate cancel is delivered as the cancelled outcome. That outcome *is*
// that decision, so it is a delivery and not a loss.
func TestReplyDispositionCancelledDecisionIsDelivered(t *testing.T) {
	p := newRawPipe(t)
	disp := make(chan ReplyDisposition, 4)
	p.client.SetPermissionHandler(func(int, PermissionRequest) PermissionDecision {
		return PermissionDecision{Cancelled: true, Replied: func(d ReplyDisposition) { disp <- d }}
	})
	p.send(t, 1, MethodRequestPermission, reportPermParams)
	if got := string(p.readWithin(t, 3*time.Second, "the handler's reply").Result); got != cancelledReply {
		t.Fatalf("reply %s", got)
	}
	if d := oneDisposition(t, p, disp); !d.Delivered || d.Lost != "" {
		t.Fatalf("disposition %+v, want delivered", d)
	}
}

// An option id the request never offered is replaced by the cancelled outcome —
// today's bytes, unchanged. The reply reached the agent; the decision did not,
// and the handler is told which.
func TestReplyDispositionInvalidOption(t *testing.T) {
	p := newRawPipe(t)
	disp := make(chan ReplyDisposition, 4)
	p.client.SetPermissionHandler(func(int, PermissionRequest) PermissionDecision {
		return PermissionDecision{OptionID: "never-offered", Replied: func(d ReplyDisposition) { disp <- d }}
	})
	p.send(t, 1, MethodRequestPermission, reportPermParams)
	if got := string(p.readWithin(t, 3*time.Second, "the replaced reply").Result); got != cancelledReply {
		t.Fatalf("reply %s", got)
	}
	if d := oneDisposition(t, p, disp); d.Delivered || d.Lost != ReplyLostInvalidOption {
		t.Fatalf("disposition %+v, want lost to %q", d, ReplyLostInvalidOption)
	}
}

// The race the report exists for (panel: astra 13): the handler takes an
// accepted decision, a cancel answers the request cancelled in the window
// before that decision is written, and the handler's reply is dropped. The
// agent heard the cancel, so the session must not believe its answer arrived.
func TestReplyLostToACancelBetweenTheDecisionAndTheReply(t *testing.T) {
	p := newRawPipe(t)
	took := make(chan struct{})
	release := make(chan struct{})
	disp := make(chan ReplyDisposition, 4)
	p.client.SetPermissionHandler(func(int, PermissionRequest) PermissionDecision {
		dec := PermissionDecision{OptionID: "yes", Replied: func(d ReplyDisposition) { disp <- d }}
		close(took)
		<-release
		return dec
	})
	p.send(t, 1, MethodRequestPermission, reportPermParams)
	<-took

	cancelDone := make(chan error, 1)
	go func() { cancelDone <- p.client.Cancel(t.Context()) }()
	if got := string(p.readWithin(t, 3*time.Second, "the cancel's reply").Result); got != cancelledReply {
		t.Fatalf("the cancel replied %s", got)
	}
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	close(release)

	if d := oneDisposition(t, p, disp); d.Delivered || d.Lost != ReplyLostCancelled {
		t.Fatalf("disposition %+v, want lost to %q", d, ReplyLostCancelled)
	}
	// The accepted decision never reached the wire: the cancelled reply above
	// was the one and only answer to this request.
	p.noReply(t)
}

// The same race against a close, which answers what it is holding on the way
// down.
func TestReplyLostToACloseBetweenTheDecisionAndTheReply(t *testing.T) {
	p := newRawPipe(t)
	took := make(chan struct{})
	release := make(chan struct{})
	disp := make(chan ReplyDisposition, 4)
	p.client.SetPlanHandler(func(int, CreatePlanRequest) PlanDecision {
		dec := PlanDecision{Accept: true, Replied: func(d ReplyDisposition) { disp <- d }}
		close(took)
		<-release
		return dec
	})
	p.send(t, 1, MethodCursorCreatePlan, reportPlanParams)
	<-took

	closed := make(chan error, 1)
	go func() { closed <- p.client.Close() }()
	if got := string(p.readWithin(t, 3*time.Second, "the close's cancelled reply").Result); got != cancelledReply {
		t.Fatalf("close replied %s", got)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	close(release)

	if d := oneDisposition(t, p, disp); d.Delivered || d.Lost != ReplyLostClosed {
		t.Fatalf("disposition %+v, want lost to %q", d, ReplyLostClosed)
	}
}

// A reply that was this request's to write and could not be written. The error
// is dropped, as it always was; that it happened is not.
func TestReplyDispositionWriteFailed(t *testing.T) {
	p := newRawPipe(t)
	took := make(chan struct{})
	release := make(chan struct{})
	disp := make(chan ReplyDisposition, 4)
	p.client.SetPlanHandler(func(int, CreatePlanRequest) PlanDecision {
		dec := PlanDecision{Accept: true, Replied: func(d ReplyDisposition) { disp <- d }}
		close(took)
		<-release
		return dec
	})
	p.send(t, 1, MethodCursorCreatePlan, reportPlanParams)
	<-took
	// The agent's end of the client's writer. Closing it fails the next write
	// outright rather than parking it on a reader that will never read.
	if err := p.serverR.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)

	if d := oneDisposition(t, p, disp); d.Delivered || d.Lost != ReplyLostWriteFailed {
		t.Fatalf("disposition %+v, want lost to %q", d, ReplyLostWriteFailed)
	}
}

// A decision with no Replied is every decision craze makes today: the reply is
// written and nothing is called.
func TestNilRepliedIsSafe(t *testing.T) {
	p := newRawPipe(t)
	p.client.SetAskHandler(func(int, AskQuestionRequest) AskDecision { return AskDecision{Skip: true} })
	p.client.SetPlanHandler(func(int, CreatePlanRequest) PlanDecision { return PlanDecision{Accept: true} })
	p.client.SetPermissionHandler(func(int, PermissionRequest) PermissionDecision {
		return PermissionDecision{OptionID: "yes"}
	})
	p.send(t, 1, MethodCursorAskQuestion, reportAskParams)
	if got := string(p.readWithin(t, 3*time.Second, "the ask reply").Result); got != `{"outcome":{"outcome":"skipped"}}` {
		t.Fatalf("ask reply %s", got)
	}
	p.send(t, 2, MethodCursorCreatePlan, reportPlanParams)
	if got := string(p.readWithin(t, 3*time.Second, "the plan reply").Result); got != `{"outcome":{"outcome":"accepted"}}` {
		t.Fatalf("plan reply %s", got)
	}
	p.send(t, 3, MethodRequestPermission, reportPermParams)
	if got := string(p.readWithin(t, 3*time.Second, "the permission reply").Result); got != `{"outcome":{"outcome":"selected","optionId":"yes"}}` {
		t.Fatalf("permission reply %s", got)
	}
}
