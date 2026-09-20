package acp

import (
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

// rawPipe wires a Client to a raw Encoder/Decoder pair so a test can send an
// exact envelope and read the exact reply bytes back. serverR is the agent's
// end of the client-write direction: closing it fails the client's next write
// while leaving the other direction, and the connection, open.
type rawPipe struct {
	client  *Client
	enc     *Encoder
	dec     *Decoder
	serverR *io.PipeReader
}

func newRawPipe(t *testing.T) *rawPipe {
	t.Helper()
	return newRawPipeDialect(t, DialectCursor)
}

func newRawPipeDialect(t *testing.T, d DialectID) *rawPipe {
	t.Helper()
	return newRawPipeWriter(t, d, nil)
}

// newRawPipeWriter is newRawPipeDialect with the client's writer wrapped, so a
// test can watch the client's writes from inside them.
func newRawPipeWriter(t *testing.T, d DialectID, wrap func(io.Writer) io.Writer) *rawPipe {
	t.Helper()
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	t.Cleanup(func() {
		_ = clientR.Close()
		_ = clientW.Close()
		_ = serverR.Close()
		_ = serverW.Close()
	})
	var out io.Writer = clientW
	if wrap != nil {
		out = wrap(clientW)
	}
	c := DialWithDialect(clientR, out, d)
	t.Cleanup(func() { _ = c.Close() })
	return &rawPipe{client: c, enc: NewEncoder(serverW), dec: NewDecoder(serverR), serverR: serverR}
}

func (p *rawPipe) setSession(sid string) {
	p.client.mu.Lock()
	p.client.sessionID = sid
	p.client.mu.Unlock()
}

func (p *rawPipe) send(t *testing.T, id any, method string, params string) {
	t.Helper()
	msg := &Message{JSONRPC: jsonrpcVersion, Method: method, Params: json.RawMessage(params)}
	if id != nil {
		raw, err := json.Marshal(id)
		if err != nil {
			t.Fatal(err)
		}
		msg.ID = raw
	}
	if err := p.enc.WriteMessage(msg); err != nil {
		t.Fatal(err)
	}
}

func (p *rawPipe) read(t *testing.T) *Message {
	t.Helper()
	msg, err := p.dec.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// readWithin fails instead of hanging when a reply never arrives.
func (p *rawPipe) readWithin(t *testing.T, d time.Duration, what string) *Message {
	t.Helper()
	type res struct {
		msg *Message
		err error
	}
	ch := make(chan res, 1)
	go func() {
		msg, err := p.dec.ReadMessage()
		ch <- res{msg, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("%s: %v", what, r.err)
		}
		return r.msg
	case <-time.After(d):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// noReply asserts nothing comes back within a short window.
func (p *rawPipe) noReply(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_, _ = p.dec.ReadMessage()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("unexpected reply to a notification")
	case <-time.After(120 * time.Millisecond):
	}
}

const todosParams = `{"toolCallId":"call-a-0\ntc","todos":[{"id":"1","content":"Read main.go","status":"in_progress"}],"merge":false}`

func TestUpdateTodosRequestReplyBytes(t *testing.T) {
	p := newRawPipe(t)
	var got UpdateTodosRequest
	p.client.SetTodosHandler(func(req UpdateTodosRequest) []TodoItem {
		got = req
		return req.Todos
	})
	p.send(t, 7, MethodCursorUpdateTodos, todosParams)
	msg := p.read(t)
	if msg.Error != nil {
		t.Fatalf("error reply %+v", msg.Error)
	}
	want := `{"outcome":{"outcome":"accepted","todos":[{"id":"1","content":"Read main.go","status":"in_progress"}]}}`
	if string(msg.Result) != want {
		t.Fatalf("reply\n got %s\nwant %s", msg.Result, want)
	}
	if got.ToolCallID != "call-a-0\ntc" || got.Merge {
		t.Fatalf("handler saw %+v", got)
	}
}

func TestUpdateTodosNotificationRoutesWithoutReply(t *testing.T) {
	p := newRawPipe(t)
	seen := make(chan UpdateTodosRequest, 1)
	p.client.SetTodosHandler(func(req UpdateTodosRequest) []TodoItem {
		seen <- req
		return req.Todos
	})
	p.send(t, nil, MethodCursorUpdateTodos, todosParams)
	select {
	case req := <-seen:
		if len(req.Todos) != 1 || req.Todos[0].ID != "1" {
			t.Fatalf("req %+v", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification never reached the handler")
	}
	p.noReply(t)
}

const taskParams = `{"toolCallId":"call-t-0\ntc","description":"Count lines","prompt":"go","model":"m1","agentId":"a1","durationMs":8010}`

func TestTaskRequestReplyBytes(t *testing.T) {
	p := newRawPipe(t)
	var got TaskRequest
	p.client.SetTaskHandler(func(req TaskRequest) { got = req })
	p.send(t, 3, MethodCursorTask, taskParams)
	msg := p.read(t)
	if msg.Error != nil {
		t.Fatalf("error reply %+v", msg.Error)
	}
	want := `{"outcome":{"outcome":"completed","agentId":"a1","durationMs":8010}}`
	if string(msg.Result) != want {
		t.Fatalf("reply\n got %s\nwant %s", msg.Result, want)
	}
	if got.Model != "m1" || got.DurationMs != 8010 {
		t.Fatalf("handler saw %+v", got)
	}
}

func TestTaskReplyEchoesEmptyReceiptFields(t *testing.T) {
	p := newRawPipe(t)
	p.send(t, 4, MethodCursorTask, `{"toolCallId":"x"}`)
	msg := p.read(t)
	want := `{"outcome":{"outcome":"completed","agentId":"","durationMs":0}}`
	if string(msg.Result) != want {
		t.Fatalf("reply\n got %s\nwant %s", msg.Result, want)
	}
}

func TestTaskNotificationRoutesWithoutReply(t *testing.T) {
	p := newRawPipe(t)
	seen := make(chan TaskRequest, 1)
	p.client.SetTaskHandler(func(req TaskRequest) { seen <- req })
	p.send(t, nil, MethodCursorTask, taskParams)
	select {
	case req := <-seen:
		if req.AgentID != "a1" {
			t.Fatalf("req %+v", req)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification never reached the handler")
	}
	p.noReply(t)
}

func TestExtensionMalformedRequestIsInvalidParams(t *testing.T) {
	cases := []struct {
		name   string
		method string
		params string
	}{
		{"todos", MethodCursorUpdateTodos, `{"todos":"not-a-list"}`},
		{"todos not an object", MethodCursorUpdateTodos, `[1,2,3]`},
		{"task", MethodCursorTask, `{"durationMs":"soon"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipe(t)
			called := false
			p.client.SetTodosHandler(func(UpdateTodosRequest) []TodoItem {
				called = true
				return nil
			})
			p.client.SetTaskHandler(func(TaskRequest) { called = true })
			p.send(t, 11, tc.method, tc.params)
			msg := p.read(t)
			if msg.Error == nil || msg.Error.Code != CodeInvalidParams {
				t.Fatalf("want InvalidParams, got %+v (result %s)", msg.Error, msg.Result)
			}
			if called {
				t.Fatal("handler ran on malformed params; state must be untouched")
			}
		})
	}
}

func TestExtensionMalformedNotificationIgnored(t *testing.T) {
	p := newRawPipe(t)
	called := false
	p.client.SetTodosHandler(func(UpdateTodosRequest) []TodoItem {
		called = true
		return nil
	})
	p.send(t, nil, MethodCursorUpdateTodos, `{"todos":42}`)
	p.noReply(t)
	if called {
		t.Fatal("handler ran on a malformed notification")
	}
}

func TestUnknownExtensionRequestStillMethodNotFound(t *testing.T) {
	p := newRawPipe(t)
	p.send(t, 5, "cursor/generate_image", `{}`)
	msg := p.read(t)
	if msg.Error == nil || msg.Error.Code != CodeMethodNotFound {
		t.Fatalf("got %+v", msg.Error)
	}
}

// TestCancelAnswersEveryBlockingKindOnce blocks a permission, a question and a
// plan request at once, then cancels: each must get exactly one cancelled
// reply, and the handlers returning afterwards must not produce a second.
func TestCancelAnswersEveryBlockingKindOnce(t *testing.T) {
	p := newRawPipe(t)
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	var enteredMu sync.Mutex
	entered := 0
	block := func() {
		defer wg.Done()
		enteredMu.Lock()
		entered++
		enteredMu.Unlock()
		<-release
	}
	p.client.SetPermissionHandler(func(int, PermissionRequest) PermissionDecision {
		block()
		return PermissionDecision{OptionID: "yes"}
	})
	p.client.SetAskHandler(func(int, AskQuestionRequest) AskDecision {
		block()
		return AskDecision{Answers: map[string][]string{"q1": {"opt-a"}}}
	})
	p.client.SetPlanHandler(func(int, CreatePlanRequest) PlanDecision {
		block()
		return PlanDecision{Accept: true}
	})

	p.send(t, 1, MethodRequestPermission, `{"sessionId":"s1","toolCall":{"toolCallId":"t1"},"options":[{"optionId":"yes","name":"Yes","kind":"allow_once"}]}`)
	p.send(t, 2, MethodCursorAskQuestion, `{"toolCallId":"t2","questions":[{"id":"q1","prompt":"?","options":[{"id":"opt-a","label":"A"}]}]}`)
	p.send(t, 3, MethodCursorCreatePlan, `{"name":"P","plan":"do it"}`)

	waitUntil(t, func() bool {
		p.client.incomingMu.Lock()
		defer p.client.incomingMu.Unlock()
		return len(p.client.incoming) == 3
	})
	// Registered is not the same as running: register happens on the read
	// loop, but each handler runs on its own goroutine, and runIncoming
	// deliberately skips a handler whose request a cancel already answered.
	// Cancelling as soon as all three are registered therefore races the
	// scheduler -- a handler that had not started yet never runs, never
	// reaches its deferred wg.Done, and wg.Wait below blocks until the test
	// binary's own timeout kills the package. Wait for all three to be parked
	// inside their handler, which is the premise this test is about anyway.
	waitUntil(t, func() bool {
		enteredMu.Lock()
		defer enteredMu.Unlock()
		return entered == 3
	})

	// Cancel writes its replies inline and the pipe is unbuffered, so the
	// reader below has to be running while it does.
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- p.client.Cancel(t.Context()) }()
	seen := map[string]int{}
	for i := 0; i < 3; i++ {
		msg := p.read(t)
		if !msg.IsResponse() {
			t.Fatalf("want a response, got %+v", msg)
		}
		if string(msg.Result) != `{"outcome":{"outcome":"cancelled"}}` {
			t.Fatalf("reply for id %s = %s", msg.ID, msg.Result)
		}
		seen[string(msg.ID)]++
	}
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 {
		t.Fatalf("ids answered %v", seen)
	}
	close(release)
	waitDone(t, &wg)
	p.noReply(t)
}

func TestTwoQueuedQuestionsAnsweredIndependently(t *testing.T) {
	p := newRawPipe(t)
	gate := make(chan struct{})
	p.client.SetAskHandler(func(_ int, req AskQuestionRequest) AskDecision {
		<-gate
		return AskDecision{Answers: map[string][]string{req.Questions[0].ID: {req.Questions[0].Options[0].ID}}}
	})
	p.send(t, 1, MethodCursorAskQuestion, `{"toolCallId":"t1","questions":[{"id":"q1","prompt":"?","options":[{"id":"opt-a","label":"A"}]}]}`)
	p.send(t, 2, MethodCursorAskQuestion, `{"toolCallId":"t2","questions":[{"id":"q2","prompt":"?","options":[{"id":"opt-b","label":"B"}]}]}`)
	waitUntil(t, func() bool {
		p.client.incomingMu.Lock()
		defer p.client.incomingMu.Unlock()
		return len(p.client.incoming) == 2
	})
	close(gate)
	got := map[string]string{}
	for i := 0; i < 2; i++ {
		msg := p.read(t)
		got[string(msg.ID)] = string(msg.Result)
	}
	if got["1"] != `{"outcome":{"outcome":"answered","answers":[{"questionId":"q1","selectedOptionIds":["opt-a"]}]}}` {
		t.Fatalf("q1 reply %s", got["1"])
	}
	if got["2"] != `{"outcome":{"outcome":"answered","answers":[{"questionId":"q2","selectedOptionIds":["opt-b"]}]}}` {
		t.Fatalf("q2 reply %s", got["2"])
	}
}

func TestAskOutcomeDropsOptionIDsNotOffered(t *testing.T) {
	p := newRawPipe(t)
	p.client.SetAskHandler(func(int, AskQuestionRequest) AskDecision {
		return AskDecision{Answers: map[string][]string{"q1": {"invented"}}}
	})
	p.send(t, 1, MethodCursorAskQuestion, `{"toolCallId":"t1","questions":[{"id":"q1","prompt":"?","options":[{"id":"opt-a","label":"A"}]}]}`)
	msg := p.read(t)
	want := `{"outcome":{"outcome":"answered","answers":[{"questionId":"q1","selectedOptionIds":[]}]}}`
	if string(msg.Result) != want {
		t.Fatalf("reply %s", msg.Result)
	}
}

func TestAskSkipAndPlanRejectOutcomes(t *testing.T) {
	p := newRawPipe(t)
	p.client.SetAskHandler(func(int, AskQuestionRequest) AskDecision { return AskDecision{Skip: true} })
	p.client.SetPlanHandler(func(int, CreatePlanRequest) PlanDecision { return PlanDecision{} })
	p.send(t, 1, MethodCursorAskQuestion, `{"toolCallId":"t1","questions":[{"id":"q1","prompt":"?","options":[]}]}`)
	if got := string(p.read(t).Result); got != `{"outcome":{"outcome":"skipped"}}` {
		t.Fatalf("skip reply %s", got)
	}
	p.send(t, 2, MethodCursorCreatePlan, `{"name":"P","plan":"x"}`)
	if got := string(p.read(t).Result); got != `{"outcome":{"outcome":"rejected"}}` {
		t.Fatalf("reject reply %s", got)
	}
}

func TestCreatePlanLenientParsing(t *testing.T) {
	cases := []struct {
		params   string
		wantName string
		wantPlan string
	}{
		{`{"name":"N","title":"T","plan":"markdown"}`, "N", "markdown"},
		{`{"title":"T","plan":"markdown"}`, "T", "markdown"},
		{`{"name":"N","plan":{"steps":1},"unknown":true}`, "N", "map[steps:1]"},
		{`{"name":"N"}`, "N", ""},
	}
	for _, tc := range cases {
		var req CreatePlanRequest
		if err := json.Unmarshal([]byte(tc.params), &req); err != nil {
			t.Fatalf("%s: %v", tc.params, err)
		}
		if req.DisplayName() != tc.wantName || req.PlanText() != tc.wantPlan {
			t.Fatalf("%s -> %q %q", tc.params, req.DisplayName(), req.PlanText())
		}
	}
}

// A cancel that lands before the handler goroutine has run must still answer
// the request: onRequest registers it on the read loop, so there is no window
// in which the request exists on the wire but not in the incoming map.
func TestCancelAnswersRequestsWhoseHandlerHasNotRunYet(t *testing.T) {
	p := newRawPipe(t)
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	p.client.SetAskHandler(func(int, AskQuestionRequest) AskDecision {
		<-gate
		return AskDecision{Skip: true}
	})

	const n = 64
	params := json.RawMessage(`{"toolCallId":"t","questions":[{"id":"q1","prompt":"?","options":[{"id":"opt-a","label":"A"}]}]}`)
	for i := 1; i <= n; i++ {
		id, err := json.Marshal(i)
		if err != nil {
			t.Fatal(err)
		}
		p.client.onRequest(&Message{JSONRPC: jsonrpcVersion, ID: id, Method: MethodCursorAskQuestion, Params: params})
	}
	go func() { _ = p.client.Cancel(t.Context()) }()

	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		msg := p.readWithin(t, 3*time.Second, "cancelled reply")
		if string(msg.Result) != `{"outcome":{"outcome":"cancelled"}}` {
			t.Fatalf("reply for id %s = %s", msg.ID, msg.Result)
		}
		seen[string(msg.ID)] = true
	}
	if len(seen) != n {
		t.Fatalf("answered %d of %d requests", len(seen), n)
	}
}

// A cancel is a cancel of what was held when it was made. A caller that bounds
// its cancel makes it on another goroutine and can give it up; the turn can then
// end, another begin, and that turn ask for a permission, all before the
// abandoned cancel has run. It must answer only the requests it was made for:
// Held is taken when the cancel is made, and CancelHeld, however late, leaves a
// request registered afterwards alone.
func TestALateCancelLeavesALaterTurnsRequestAlone(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("sess-1")
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	p.client.SetAskHandler(func(int, AskQuestionRequest) AskDecision {
		<-gate
		return AskDecision{Skip: true}
	})
	params := json.RawMessage(`{"toolCallId":"t","questions":[{"id":"q1","prompt":"?","options":[{"id":"opt-a","label":"A"}]}]}`)
	ask := func(id int) {
		raw, err := json.Marshal(id)
		if err != nil {
			t.Fatal(err)
		}
		p.client.onRequest(&Message{JSONRPC: jsonrpcVersion, ID: raw, Method: MethodCursorAskQuestion, Params: params})
	}

	ask(1)
	held := p.client.Held()
	// The cancel is given up here, and the next turn's request arrives before it
	// runs.
	ask(2)
	go func() { _ = p.client.CancelHeld(t.Context(), held) }()

	reply := p.readWithin(t, 3*time.Second, "the held request's cancelled reply")
	if string(reply.ID) != "1" || string(reply.Result) != `{"outcome":{"outcome":"cancelled"}}` {
		t.Fatalf("reply %s = %s, want request 1 cancelled", reply.ID, reply.Result)
	}
	// The next frame is the cancel notification itself: nothing was written for
	// request 2, which is still held and still the next turn's to answer.
	note := p.readWithin(t, 3*time.Second, "session/cancel")
	if note.Method != MethodSessionCancel {
		t.Fatalf("the frame after the reply is %q (id %s), want %s", note.Method, note.ID, MethodSessionCancel)
	}
	// Request 1 was answered on the wire, above. Request 2 is still registered
	// and still unanswered: its handler is parked at the gate, and nothing but
	// its own turn's cancel or its own answer may end it.
	p.client.incomingMu.Lock()
	second := p.client.incoming["2"]
	answered := second != nil && second.replied
	p.client.incomingMu.Unlock()
	if second == nil || answered {
		t.Fatalf("request 2 after a late cancel: held %v, answered %v — a cancel answers only what it was made for", second != nil, answered)
	}
}

// The other half of the window above: the handler goroutine the runtime never
// scheduled eventually wakes up, and by then the request it was dispatched for
// can be answered already — or its whole turn can be over and another one
// running. Running the handler then is what publishes a card for a turn that
// has ended, so it does not run; the request is answered cancelled, which is a
// no-op for one a cancel already answered. runIncoming is called by hand here
// because that is precisely what a delayed goroutine does: run the body late.
func TestDelayedHandlerDoesNotRunForARequestThatIsOver(t *testing.T) {
	planParams := json.RawMessage(`{"name":"P","plan":"do it"}`)

	t.Run("already answered", func(t *testing.T) {
		p := newRawPipe(t)
		in := p.client.register(&Message{
			JSONRPC: jsonrpcVersion,
			ID:      json.RawMessage(`7`),
			Method:  MethodCursorCreatePlan,
			Params:  planParams,
		})
		// The cancel's own reply goes out on the unbuffered pipe, so it has to
		// be read while the cancel is writing it.
		go p.client.completeIncomingCancelled()
		msg := p.readWithin(t, 3*time.Second, "the cancel's reply")
		if string(msg.Result) != `{"outcome":{"outcome":"cancelled"}}` {
			t.Fatalf("cancel replied %s", msg.Result)
		}

		ran := false
		p.client.runIncoming(in, func(*pendingReq) { ran = true })
		if ran {
			t.Fatal("the handler ran for a request the cancel had already answered")
		}
		p.noReply(t)
	})

	t.Run("turn is over", func(t *testing.T) {
		p := newRawPipe(t)
		in := p.client.register(&Message{
			JSONRPC: jsonrpcVersion,
			ID:      json.RawMessage(`8`),
			Method:  MethodCursorCreatePlan,
			Params:  planParams,
		})
		if !p.client.TurnLive(in.turn) {
			t.Fatal("the turn a request arrived in must be live to begin with")
		}
		// A turn of the user's own, started while the goroutine was waiting.
		p.client.mu.Lock()
		p.client.turn++
		p.client.mu.Unlock()
		if p.client.TurnLive(in.turn) {
			t.Fatal("a turn that has been superseded is not live")
		}

		ran := make(chan struct{})
		go func() {
			p.client.runIncoming(in, func(*pendingReq) { close(ran) })
		}()
		msg := p.readWithin(t, 3*time.Second, "the stale request's reply")
		if string(msg.Result) != `{"outcome":{"outcome":"cancelled"}}` {
			t.Fatalf("stale request replied %s", msg.Result)
		}
		select {
		case <-ran:
			t.Fatal("the handler ran for a request whose turn was over")
		default:
		}
	})
}

// A handler for the turn that is running does run: the guard above is not
// simply "never run a delayed handler".
func TestHandlerRunsForTheRunningTurn(t *testing.T) {
	p := newRawPipe(t)
	in := p.client.register(&Message{
		JSONRPC: jsonrpcVersion,
		ID:      json.RawMessage(`9`),
		Method:  MethodCursorCreatePlan,
		Params:  json.RawMessage(`{"name":"P","plan":"do it"}`),
	})
	ran := make(chan struct{})
	go p.client.runIncoming(in, func(*pendingReq) {
		close(ran)
		p.client.replyIncoming(in, planOutcome(DialectCursor, PlanDecision{Accept: true}))
	})
	msg := p.readWithin(t, 3*time.Second, "the handler's reply")
	if string(msg.Result) != `{"outcome":{"outcome":"accepted"}}` {
		t.Fatalf("handler replied %s", msg.Result)
	}
	select {
	case <-ran:
	default:
		t.Fatal("the handler never ran")
	}
}

func TestNullParamsRejected(t *testing.T) {
	cases := []struct {
		name   string
		method string
		params string
	}{
		{"todos null params", MethodCursorUpdateTodos, `null`},
		{"todos null list", MethodCursorUpdateTodos, `{"toolCallId":"t","todos":null}`},
		{"todos missing list", MethodCursorUpdateTodos, `{"toolCallId":"t"}`},
		{"task null params", MethodCursorTask, `null`},
		{"task no toolCallId", MethodCursorTask, `{"description":"x"}`},
		{"permission null params", MethodRequestPermission, `null`},
		{"ask null params", MethodCursorAskQuestion, `null`},
		{"plan null params", MethodCursorCreatePlan, `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipe(t)
			called := false
			p.client.SetTodosHandler(func(UpdateTodosRequest) []TodoItem { called = true; return nil })
			p.client.SetTaskHandler(func(TaskRequest) { called = true })
			p.client.SetPermissionHandler(func(int, PermissionRequest) PermissionDecision {
				called = true
				return PermissionDecision{Cancelled: true}
			})
			p.client.SetAskHandler(func(int, AskQuestionRequest) AskDecision { called = true; return AskDecision{} })
			p.client.SetPlanHandler(func(int, CreatePlanRequest) PlanDecision { called = true; return PlanDecision{} })

			p.send(t, 9, tc.method, tc.params)
			msg := p.readWithin(t, 3*time.Second, "InvalidParams reply")
			if msg.Error == nil || msg.Error.Code != CodeInvalidParams {
				t.Fatalf("want InvalidParams, got error %+v result %s", msg.Error, msg.Result)
			}
			if called {
				t.Fatal("handler ran; state must be untouched")
			}
		})
	}
}

func TestNullParamsNotificationIgnored(t *testing.T) {
	for _, params := range []string{`null`, `{"toolCallId":"t","todos":null}`} {
		p := newRawPipe(t)
		called := false
		p.client.SetTodosHandler(func(UpdateTodosRequest) []TodoItem { called = true; return nil })
		p.send(t, nil, MethodCursorUpdateTodos, params)
		p.noReply(t)
		if called {
			t.Fatalf("params %s reached the handler", params)
		}
	}
}

// An explicitly empty list is a real instruction and must still be applied.
func TestEmptyTodosListAccepted(t *testing.T) {
	p := newRawPipe(t)
	got := make(chan UpdateTodosRequest, 1)
	p.client.SetTodosHandler(func(req UpdateTodosRequest) []TodoItem {
		got <- req
		return req.Todos
	})
	p.send(t, 1, MethodCursorUpdateTodos, `{"toolCallId":"t","todos":[]}`)
	msg := p.readWithin(t, 3*time.Second, "accepted reply")
	if string(msg.Result) != `{"outcome":{"outcome":"accepted","todos":[]}}` {
		t.Fatalf("reply %s", msg.Result)
	}
	if req := <-got; req.Todos == nil || len(req.Todos) != 0 {
		t.Fatalf("handler saw %+v", req.Todos)
	}
}

// writeLine puts one captured JSONL line back on the wire verbatim, so the
// client sees it through its own framing and dispatch.
func (p *rawPipe) writeLine(t *testing.T, line string) {
	t.Helper()
	var msg Message
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	if err := p.enc.WriteMessage(&msg); err != nil {
		t.Fatal(err)
	}
}
