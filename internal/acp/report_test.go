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

// requestsDone is the barrier for "every request goroutine has finished".
// dropIncoming is runIncoming's deferred last act — after the reply, after
// Replied and after any early-answer hook — so once the incoming map is empty
// nothing else can run for those requests and a count of what they did is
// final. A reply read off the wire is NOT that barrier: the goroutine that
// wrote it can still be between the write and its own return.
func requestsDone(t *testing.T, p *rawPipe) {
	t.Helper()
	waitUntil(t, func() bool {
		p.client.incomingMu.Lock()
		defer p.client.incomingMu.Unlock()
		return len(p.client.incoming) == 0
	})
}

// oneDisposition is the single Replied call a handler goroutine made, counted
// once its request goroutine has finished.
func oneDisposition(t *testing.T, p *rawPipe, disp chan ReplyDisposition) ReplyDisposition {
	t.Helper()
	requestsDone(t, p)
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

// The two histories the turn counter alone cannot tell apart, and the flag that
// does (Arrival; plan 021, panel r16 finding 3). A request registered while a
// prompt of craze's own is in flight belongs to that turn; one registered after
// the prompt has returned belongs to no turn — and PromptBlocks clears inPrompt
// without moving the counter, so BOTH carry turn 1 and both pass TurnLive.
//
// The prompt's accepted hook is the barrier: it runs after the turn has been
// opened and before a byte goes out, which is exactly the window an in-turn
// request arrives in.
func TestArrivalRecordsTurnMembershipNotJustTheCounter(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	var req PermissionRequest
	if err := json.Unmarshal([]byte(reportPermParams), &req); err != nil {
		t.Fatal(err)
	}
	register := func(id string) *pendingReq {
		return p.client.register(&Message{
			JSONRPC: jsonrpcVersion,
			ID:      json.RawMessage(id),
			Method:  MethodRequestPermission,
			Params:  json.RawMessage(reportPermParams),
		}, RequestParams{Permission: &req})
	}

	var during *pendingReq
	done := make(chan error, 1)
	go func() {
		_, err := p.client.PromptBlocks(t.Context(),
			[]ContentBlock{{Type: "text", Text: "go"}},
			func() { during = register(`21`) }, nil)
		done <- err
	}()
	prompt := p.readWithin(t, 3*time.Second, "the prompt")
	p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: prompt.ID, Result: json.RawMessage(`{"stopReason":"end_turn"}`)})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after := register(`22`)

	if during.turn != after.turn {
		t.Fatalf("the counter is meant to be the same: %d and %d", during.turn, after.turn)
	}
	if !p.client.TurnLive(during.turn) || !p.client.TurnLive(after.turn) {
		t.Fatal("both are still the client's turn; the counter cannot separate them")
	}
	if !during.arrival().InTurn {
		t.Fatal("a request registered inside the prompt belongs to that turn")
	}
	if after.arrival().InTurn {
		t.Fatal("a request registered after the prompt returned belongs to no turn")
	}
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
//
// The reply proves runIncoming took the live path; the request goroutine
// FINISHING is what makes "and it reported nothing" an assertion rather than a
// coin toss (review r16, finding 7). Reading the reply and counting at once
// would pass against an implementation that reported early after its handler
// returned, because the goroutine can still be between the write and its own
// return.
func TestNoEarlyAnswerWhenTheHandlerRan(t *testing.T) {
	p := newRawPipe(t)
	early := watchEarly(p.client)
	p.client.SetAskHandler(func(Arrival, AskQuestionRequest) AskDecision { return AskDecision{Skip: true} })
	p.send(t, 1, MethodCursorAskQuestion, reportAskParams)
	if got := string(p.readWithin(t, 3*time.Second, "the handler's reply").Result); got != `{"outcome":{"outcome":"skipped"}}` {
		t.Fatalf("the handler replied %s", got)
	}
	requestsDone(t, p)
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
			p.client.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
				close(entered)
				<-release
				return PermissionDecision{Cancelled: true}
			})
			p.client.SetAskHandler(func(Arrival, AskQuestionRequest) AskDecision {
				close(entered)
				<-release
				return AskDecision{Cancelled: true}
			})
			p.client.SetPlanHandler(func(Arrival, CreatePlanRequest) PlanDecision {
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

// Review r17, finding 3: every arrival carries the request's own lifetime, and
// a reply written by anything but that request's handler ends it — in the same
// incomingMu section that records who answered. A handler still deciding, or
// already parked on a decision it will never get to give, learns at once.
//
// The handler parks, so nothing about the reply can be mistaken for the
// handler's own, and the wire is unchanged: the cancelled reply below is the
// same byte for byte as it has always been.
func TestAnArrivalsContextEndsWhenSomethingElseAnswers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(t *testing.T, p *rawPipe)
	}{
		{"a cancel", func(t *testing.T, p *rawPipe) {
			t.Helper()
			done := make(chan error, 1)
			go func() { done <- p.client.Cancel(t.Context()) }()
			if got := string(p.readWithin(t, 3*time.Second, "the cancel's reply").Result); got != cancelledReply {
				t.Fatalf("the cancel replied %s", got)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}},
		{"a close", func(t *testing.T, p *rawPipe) {
			t.Helper()
			done := make(chan error, 1)
			go func() { done <- p.client.Close() }()
			if got := string(p.readWithin(t, 3*time.Second, "the close's cancelled reply").Result); got != cancelledReply {
				t.Fatalf("close replied %s", got)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipe(t)
			arrived := make(chan Arrival, 1)
			release := make(chan struct{})
			p.client.SetPermissionHandler(func(a Arrival, _ PermissionRequest) PermissionDecision {
				arrived <- a
				<-release
				return PermissionDecision{Cancelled: true}
			})
			p.send(t, 1, MethodRequestPermission, reportPermParams)
			var a Arrival
			select {
			case a = <-arrived:
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for the handler to start")
			}
			defer close(release)

			if a.Call == nil {
				t.Fatal("an arrival off the read loop carries the request's own context")
			}
			if a.Ended() {
				t.Fatal("the request is live: nothing has answered it")
			}
			tc.answer(t, p)
			select {
			case <-a.Call.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("the request was answered and its context did not end")
			}
			if !a.Ended() {
				t.Fatal("Ended must say what the context says")
			}
		})
	}
}

// The other side of the same rule: a handler's OWN reply does not end its
// request's context. The Replied hook runs inside the request's goroutine,
// right after that reply, and a decision that reached the agent must not read
// back there as one something else answered.
func TestAHandlersOwnReplyDoesNotEndItsArrival(t *testing.T) {
	p := newRawPipe(t)
	type seen struct {
		disp  ReplyDisposition
		ended bool
	}
	got := make(chan seen, 4)
	p.client.SetPermissionHandler(func(a Arrival, _ PermissionRequest) PermissionDecision {
		return PermissionDecision{OptionID: "yes", Replied: func(d ReplyDisposition) {
			got <- seen{disp: d, ended: a.Ended()}
		}}
	})
	p.send(t, 1, MethodRequestPermission, reportPermParams)
	if reply := string(p.readWithin(t, 3*time.Second, "the handler's reply").Result); reply != `{"outcome":{"outcome":"selected","optionId":"yes"}}` {
		t.Fatalf("reply %s", reply)
	}
	requestsDone(t, p)
	if len(got) != 1 {
		t.Fatalf("Replied ran %d times, want exactly once", len(got))
	}
	s := <-got
	if !s.disp.Delivered || s.disp.Lost != "" {
		t.Fatalf("disposition %+v, want delivered", s.disp)
	}
	if s.ended {
		t.Fatal("the handler's own reply ended its request's context")
	}
}

// The ordinary case: the handler's decision is the reply the agent gets, and
// its Replied hook says so.
func TestReplyDispositionDelivered(t *testing.T) {
	p := newRawPipe(t)
	disp := make(chan ReplyDisposition, 4)
	p.client.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
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
	p.client.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
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
	p.client.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
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
	p.client.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
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
	p.client.SetPlanHandler(func(Arrival, CreatePlanRequest) PlanDecision {
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
	p.client.SetPlanHandler(func(Arrival, CreatePlanRequest) PlanDecision {
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
	p.client.SetAskHandler(func(Arrival, AskQuestionRequest) AskDecision { return AskDecision{Skip: true} })
	p.client.SetPlanHandler(func(Arrival, CreatePlanRequest) PlanDecision { return PlanDecision{Accept: true} })
	p.client.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
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
