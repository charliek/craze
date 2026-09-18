package acp

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

func startGrokPrompt(t *testing.T, p *rawPipe, text string) (<-chan struct {
	res *PromptResult
	err error
}, *Message) {
	t.Helper()
	done := make(chan struct {
		res *PromptResult
		err error
	}, 1)
	go func() {
		res, err := p.client.Prompt(t.Context(), text)
		done <- struct {
			res *PromptResult
			err error
		}{res, err}
	}()
	req := p.readWithin(t, 3*time.Second, "session/prompt")
	if req.Method != MethodSessionPrompt {
		t.Fatalf("method %q", req.Method)
	}
	return done, req
}

func replyPrompt(t *testing.T, p *rawPipe, id json.RawMessage, stop string) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"stopReason": stop})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: id, Result: raw}); err != nil {
		t.Fatal(err)
	}
}

func waitPrompt(t *testing.T, done <-chan struct {
	res *PromptResult
	err error
}) *PromptResult {
	t.Helper()
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("prompt: %v", out.err)
		}
		return out.res
	case <-time.After(3 * time.Second):
		t.Fatal("prompt hung")
		return nil
	}
}

func TestPromptCompleteNotifyFirstHangRPC(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, _ := startGrokPrompt(t, p, "hi")
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","stopReason":"max_tokens"}`)
	res := waitPrompt(t, done)
	if res.StopReason != "max_tokens" {
		t.Fatalf("stop %q", res.StopReason)
	}
	if p.client.PromptInFlight() {
		t.Fatal("inPrompt must clear after the notify win")
	}
}

func TestPromptCompleteRPCFirst(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, req := startGrokPrompt(t, p, "hi")
	replyPrompt(t, p, req.ID, StopEndTurn)
	res := waitPrompt(t, done)
	if res.StopReason != StopEndTurn {
		t.Fatalf("stop %q", res.StopReason)
	}
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","stopReason":"max_tokens"}`)
	time.Sleep(50 * time.Millisecond)
	if p.client.PromptInFlight() {
		t.Fatal("late notify must not reopen a prompt")
	}
}

// TestGrokSentMayRunAfterPromptBlocksReturned is the late call sent's contract
// warns about. prompt_complete settles a grok turn on its own, and PromptBlocks
// then abandons the goroutine still writing the request; here that write is
// held in the unbuffered pipe, so the turn is over before its bytes are out.
// sent has not run when PromptBlocks returns, and it runs — once — when the
// write finally completes: a caller has to expect it after the fact.
func TestGrokSentMayRunAfterPromptBlocksReturned(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	// Ahead of the client's own Close: should an assertion fail with the
	// request still held in the pipe, the held write fails instead of keeping
	// Close's cancel waiting behind it.
	t.Cleanup(func() { _ = p.serverR.Close() })
	p.setSession("s1")
	var calls atomic.Int32
	fired := make(chan struct{})
	sent := func() {
		if calls.Add(1) == 1 {
			close(fired)
		}
	}
	done := goPromptBlocks(p, []ContentBlock{{Type: "text", Text: "hi"}}, nil, sent)
	// The completion is only routed to a turn that is open.
	waitFor(t, p.client.PromptInFlight, "the turn to open")
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","stopReason":"end_turn"}`)
	if res := waitPrompt(t, done); res.StopReason != StopEndTurn {
		t.Fatalf("stop %q", res.StopReason)
	}
	select {
	case <-fired:
		t.Fatal("sent ran while the request was still unread")
	default:
	}
	p.readWithin(t, 3*time.Second, "session/prompt")
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("the abandoned writer never reported its write")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("sent ran %d times", n)
	}
}

func TestPromptCompleteDuplicateDoesNotDouble(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, _ := startGrokPrompt(t, p, "hi")
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1"}`)
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","stopReason":"max_tokens"}`)
	res := waitPrompt(t, done)
	if res.StopReason != StopEndTurn {
		t.Fatalf("first notify wins with default end_turn, got %q", res.StopReason)
	}
	select {
	case extra := <-done:
		t.Fatalf("second completion %v", extra)
	default:
	}
}

func TestPromptCompleteForeignSessionIgnored(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, _ := startGrokPrompt(t, p, "hi")
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"other","stopReason":"end_turn"}`)
	select {
	case out := <-done:
		t.Fatalf("foreign session completed the prompt: %+v", out.res)
	case <-time.After(120 * time.Millisecond):
	}
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1"}`)
	res := waitPrompt(t, done)
	if res.StopReason != StopEndTurn {
		t.Fatalf("stop %q", res.StopReason)
	}
}

func TestPromptCompleteFollowUpAfterNotifyFirst(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, _ := startGrokPrompt(t, p, "one")
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1"}`)
	_ = waitPrompt(t, done)

	done2, req2 := startGrokPrompt(t, p, "two")
	replyPrompt(t, p, req2.ID, StopEndTurn)
	res := waitPrompt(t, done2)
	if res.StopReason != StopEndTurn {
		t.Fatalf("follow-up stop %q", res.StopReason)
	}
}

// TestPromptCompleteCancelWaitsForAgent pins that Cancel sends session/cancel
// and leaves the prompt in flight until the agent ends the turn itself. Live
// grok answers a cancel with prompt_complete{cancelled} and then the RPC
// reply; ending the prompt early would let a follow-up prompt be ended by
// that late notification instead of its own.
func TestPromptCompleteCancelWaitsForAgent(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, req := startGrokPrompt(t, p, "hi")
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- p.client.Cancel(context.Background()) }()
	msg := p.readWithin(t, 3*time.Second, "session/cancel")
	if msg.Method != MethodSessionCancel {
		t.Fatalf("method %q", msg.Method)
	}
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		t.Fatalf("cancel must not end the prompt by itself: %+v %v", out.res, out.err)
	case <-time.After(50 * time.Millisecond):
	}
	if !p.client.PromptInFlight() {
		t.Fatal("prompt must stay in flight until the agent ends the turn")
	}
	p.send(t, nil, MethodGrokPromptCompleteWrapped, `{"sessionId":"s1","stopReason":"cancelled"}`)
	res := waitPrompt(t, done)
	if res.StopReason != StopCancelled {
		t.Fatalf("stop %q", res.StopReason)
	}
	// The RPC reply for the cancelled turn lands late and is ignored.
	replyPrompt(t, p, req.ID, StopCancelled)
	done2, req2 := startGrokPrompt(t, p, "next")
	replyPrompt(t, p, req2.ID, StopEndTurn)
	if waitPrompt(t, done2).StopReason != StopEndTurn {
		t.Fatal("a follow-up after cancel must still complete")
	}
}

func TestPromptCompleteBothMethodNamesAndWrapped(t *testing.T) {
	cases := []struct {
		name   string
		method string
		params string
	}{
		{"direct", MethodGrokPromptComplete, `{"sessionId":"s1","stopReason":"end_turn"}`},
		{"alt", MethodGrokPromptCompleteWrapped, `{"sessionId":"s1","stopReason":"end_turn"}`},
		{"wrapped", MethodGrokPromptCompleteWrapped, `{"method":"x.ai/session/prompt_complete","params":{"sessionId":"s1","stopReason":"max_tokens"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipeDialect(t, DialectGrok)
			p.setSession("s1")
			done, _ := startGrokPrompt(t, p, "hi")
			p.send(t, nil, tc.method, tc.params)
			res := waitPrompt(t, done)
			if res.StopReason == "" {
				t.Fatal("empty stopReason")
			}
		})
	}
}

func TestPromptCompleteLateRPCIgnored(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, req := startGrokPrompt(t, p, "hi")
	p.send(t, nil, MethodGrokPromptComplete, `{"sessionId":"s1","stopReason":"max_tokens"}`)
	res := waitPrompt(t, done)
	if res.StopReason != "max_tokens" {
		t.Fatalf("stop %q", res.StopReason)
	}
	replyPrompt(t, p, req.ID, StopEndTurn)
	time.Sleep(50 * time.Millisecond)
	if p.client.PromptInFlight() {
		t.Fatal("late RPC must not leave inPrompt set")
	}
}

// TestPromptCompleteStalePromptIDIgnored pins the RPC-wins case: the reply
// carries _meta.promptId, a follow-up prompt starts, and only then the first
// turn's prompt_complete lands. It names the finished promptId, so it must
// not end the second turn.
func TestPromptCompleteStalePromptIDIgnored(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, req := startGrokPrompt(t, p, "first")
	raw, err := json.Marshal(map[string]any{"stopReason": StopEndTurn, "_meta": map[string]string{"promptId": "p1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: req.ID, Result: raw}); err != nil {
		t.Fatal(err)
	}
	if waitPrompt(t, done).StopReason != StopEndTurn {
		t.Fatal("first turn")
	}
	done2, req2 := startGrokPrompt(t, p, "second")
	p.send(t, nil, MethodGrokPromptCompleteWrapped, `{"sessionId":"s1","promptId":"p1","stopReason":"end_turn"}`)
	select {
	case out := <-done2:
		t.Fatalf("stale prompt_complete ended the second turn: %+v %v", out.res, out.err)
	case <-time.After(100 * time.Millisecond):
	}
	// The second turn's own completion still lands.
	p.send(t, nil, MethodGrokPromptCompleteWrapped, `{"sessionId":"s1","promptId":"p2","stopReason":"max_tokens"}`)
	if waitPrompt(t, done2).StopReason != "max_tokens" {
		t.Fatal("second turn")
	}
	replyPrompt(t, p, req2.ID, StopEndTurn)
}
