package acp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestAsideRoundTrip(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		text, err := p.client.Aside(t.Context(), "what is going on?")
		ch <- result{text, err}
	}()
	req := p.readWithin(t, 3*time.Second, "btw")
	if req.Method != MethodGrokBtwWrapped {
		t.Fatalf("method %q", req.Method)
	}
	var got map[string]any
	if err := json.Unmarshal(req.Params, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["sessionId"] != "s1" || got["question"] != "what is going on?" {
		t.Fatalf("params %v", got)
	}
	p.answerWith(t, req, `{"result":{"answer":"a reply"}}`)
	r := <-ch
	if r.err != nil || r.text != "a reply" {
		t.Fatalf("answer %q, %v", r.text, r.err)
	}
}

func TestAsideMethodNotFoundIsUnsupported(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	ch := make(chan error, 1)
	go func() { _, err := p.client.Aside(t.Context(), "q"); ch <- err }()
	req := p.readWithin(t, 3*time.Second, "btw")
	if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: req.ID, Error: MethodNotFound(req.Method)}); err != nil {
		t.Fatal(err)
	}
	if err := <-ch; !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err %v", err)
	}
}

func TestAsideOtherErrorsAreNotUnsupported(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	ch := make(chan error, 1)
	go func() { _, err := p.client.Aside(t.Context(), "q"); ch <- err }()
	req := p.readWithin(t, 3*time.Second, "btw")
	if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: req.ID, Error: &RPCError{Code: -32000, Message: "boom"}}); err != nil {
		t.Fatal(err)
	}
	err := <-ch
	if err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("err %v", err)
	}
}

func TestAsideEmptyAnswer(t *testing.T) {
	for _, body := range []string{`{"result":{"answer":""}}`, `{"result":{"answer":"  \n"}}`, `{}`} {
		p := newRawPipeDialect(t, DialectGrok)
		p.setSession("s1")
		ch := make(chan error, 1)
		go func() { _, err := p.client.Aside(t.Context(), "q"); ch <- err }()
		req := p.readWithin(t, 3*time.Second, "btw")
		p.answerWith(t, req, body)
		if err := <-ch; !errors.Is(err, ErrEmptyAnswer) {
			t.Fatalf("%s: err %v", body, err)
		}
	}
}

func TestAsideUnsupportedOnCursorWritesNothing(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := p.client.Aside(ctx, "q"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err %v", err)
	}
	p.noReply(t)
}

func TestAsideNeedsASession(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := p.client.Aside(ctx, "q"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("err %v", err)
	}
	p.noReply(t)
}
