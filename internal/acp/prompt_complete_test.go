package acp

import (
	"context"
	"encoding/json"
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

func TestPromptCompleteCancelCleansWaiters(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, _ := startGrokPrompt(t, p, "hi")
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- p.client.Cancel(context.Background()) }()
	msg := p.readWithin(t, 3*time.Second, "session/cancel")
	if msg.Method != MethodSessionCancel {
		t.Fatalf("method %q", msg.Method)
	}
	res := waitPrompt(t, done)
	if res.StopReason != StopCancelled {
		t.Fatalf("stop %q", res.StopReason)
	}
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}
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
