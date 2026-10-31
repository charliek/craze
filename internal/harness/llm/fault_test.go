package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// h2Server speaks HTTP/2 in the clear, so that a handler's abort is the real
// RST_STREAM INTERNAL_ERROR a provider's proxy sends, read by the real
// transport of the real driver.
func h2Server(t *testing.T, h http.HandlerFunc) (*httptest.Server, *http.Client) {
	t.Helper()
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("a request came over HTTP/%d", r.ProtoMajor)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		h(w, r)
	}))
	srv.Config.Protocols = &p
	srv.Start()
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: &http.Transport{Protocols: &p}}
	t.Cleanup(client.CloseIdleConnections)
	return srv, client
}

// abortAfter writes what reply writes, then resets the stream.
func abortAfter(reply http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reply(w, r)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
}

// aDriver is one of the drivers whose stream code is its own, as New builds it.
type aDriver struct {
	name    string
	model   func(t *testing.T, url string, client *http.Client) fantasy.LanguageModel
	partial http.HandlerFunc // a response that has streamed text "partial"
}

func theDrivers() []aDriver {
	build := func(t *testing.T, r modeltable.Resolved, opts ...Option) fantasy.LanguageModel {
		t.Helper()
		lm, err := New(r, opts...)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return lm
	}
	rxPartial := rxEvents(rxAdded(0, metaMessage("rs_m", "")), rxText(0, "rs_m", "partial"))
	return []aDriver{
		{name: "openai-compat", model: func(t *testing.T, url string, c *http.Client) fantasy.LanguageModel {
			return build(t, resolved(url+"/v1"), WithHTTPClient(c))
		}, partial: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", textChunk("partial"))
		}},
		{name: "anthropic", model: func(t *testing.T, url string, c *http.Client) fantasy.LanguageModel {
			return build(t, claude(), WithHTTPClient(c), WithAnthropicAPIBase(func() (string, error) { return url, nil }))
		}, partial: anReply(nil, anStart(anUsage(5, 0, 0, 1)), anBlockOpen(0, "partial"))},
		{name: "responses, the key route", model: func(t *testing.T, url string, c *http.Client) fantasy.LanguageModel {
			return build(t, keyRoute(url+"/v1"), WithHTTPClient(c))
		}, partial: rxPartial},
		{name: "responses, the ChatGPT route", model: func(t *testing.T, url string, c *http.Client) fantasy.LanguageModel {
			return build(t, chatgpt(), WithHTTPClient(c), WithSignIn(newSignInAuth(), func() (string, error) { return url, nil }))
		}, partial: rxPartial},
	}
}

// stepOn runs one step of lm and returns the text it streamed and how it ended.
func stepOn(t *testing.T, ctx context.Context, lm fantasy.LanguageModel) (string, error) {
	t.Helper()
	stream, err := lm.Stream(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("go")}})
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeError:
			return text.String(), part.Error
		}
	}
	return text.String(), nil
}

// retried says err is one Fantasy retries, by what it reads: a ProviderError's
// own verdict.
func retried(err error) bool {
	var pe *fantasy.ProviderError
	return errors.As(err, &pe) && pe.IsRetryable()
}

// TestAStreamResetIsRetriedThroughEveryDriver: a real HTTP/2 stream reset after
// output, through each driver's own stream code, is a retryable error with the
// key nowhere in it when the caller retracts, and final when it does not.
func TestAStreamResetIsRetriedThroughEveryDriver(t *testing.T) {
	for _, d := range theDrivers() {
		t.Run(d.name, func(t *testing.T) {
			srv, client := h2Server(t, abortAfter(d.partial))
			lm := d.model(t, srv.URL, client)

			text, err := stepOn(t, WithRetraction(context.Background()), lm)
			if text != "partial" || !retried(err) {
				t.Fatalf("with retraction: text %q, err %v (%T); want the partial text and a retryable error", text, err, err)
			}
			if strings.Contains(fmt.Sprintf("%+v", err), canary) {
				t.Fatalf("the key is in the error: %v", err)
			}

			text, err = stepOn(t, context.Background(), lm)
			var mse *MidStreamError
			if text != "partial" || !errors.As(err, &mse) || retried(err) {
				t.Fatalf("without retraction: text %q, err %v (%T); want a *MidStreamError", text, err, err)
			}
		})
	}
}

// TestAResetBeforeOutputIsRetriedAsEver: the same reset before any output, on a
// caller that takes nothing back, is retried: nothing reached anyone.
func TestAResetBeforeOutputIsRetriedAsEver(t *testing.T) {
	for _, d := range theDrivers() {
		t.Run(d.name, func(t *testing.T) {
			srv, client := h2Server(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			})
			if _, err := stepOn(t, context.Background(), d.model(t, srv.URL, client)); !retried(err) {
				t.Fatalf("err = %v (%T), want a retryable error", err, err)
			}
		})
	}
}

// TestInBandFailuresAfterOutput is the classification per driver, each through
// the real driver: an event naming the server's fault is retried after output,
// and one naming anything of the request or the account stays final.
func TestInBandFailuresAfterOutput(t *testing.T) {
	compat := func(event string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", textChunk("partial"))
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}
	failed := func(code, typ string) string {
		return rx(map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed",
			"error": map[string]any{"code": code, "type": typ, "message": "boom"}, "usage": rxUsage(5, 0, 0, 0)}})
	}
	rxFailed := func(code, typ string) http.HandlerFunc {
		return rxEvents(rxAdded(0, metaMessage("rs_m", "")), rxText(0, "rs_m", "partial"), failed(code, typ))
	}
	anFailed := func(typ string, header ...string) http.HandlerFunc {
		return anReply(header, anStart(anUsage(5, 0, 0, 1)), anBlockOpen(0, "partial"), anErrorEvent(typ, "boom"))
	}
	type want int
	const (
		retries want = iota
		midStream
		final
	)
	drivers := map[string]aDriver{}
	for _, d := range theDrivers() {
		drivers[d.name] = d
	}
	for _, tc := range []struct {
		name, driver string
		reply        http.HandlerFunc
		want         want
	}{
		{"server_error", "openai-compat", compat(`{"error":{"type":"server_error","message":"boom"}}`), retries},
		{"server_error as a code", "openai-compat", compat(`{"error":{"code":"server_error","message":"boom"}}`), retries},
		{"a 502 as OpenRouter's event says it", "openai-compat", compat(`{"error":{"code":502,"message":"boom"}}`), retries},
		{"a 429 as OpenRouter's event says it", "openai-compat", compat(`{"error":{"code":429,"message":"boom"}}`), midStream},
		{"a content filter", "openai-compat", compat(`{"error":{"code":"content_filter","message":"boom"}}`), midStream},
		{"a schema refusal", "openai-compat", compat(`{"error":{"type":"invalid_request_error","message":"boom"}}`), midStream},
		{"a context too large", "openai-compat", compat(`{"error":{"code":"context_length_exceeded","message":"boom"}}`), midStream},
		{"a rate limit", "openai-compat", compat(`{"error":{"type":"rate_limit_error","message":"boom"}}`), midStream},

		{"overloaded_error", "anthropic", anFailed("overloaded_error"), retries},
		{"api_error", "anthropic", anFailed("api_error"), retries},
		{"a request refused", "anthropic", anFailed("invalid_request_error"), midStream},
		{"a rate limit", "anthropic", anFailed("rate_limit_error"), midStream},
		{"overloaded, marked not to retry", "anthropic", anFailed("overloaded_error", "x-should-retry", "false"), final},

		{"response.failed with a server error", "responses, the key route", rxFailed("server_error", ""), retries},
		{"response.failed with an api error", "responses, the key route", rxFailed("", "api_error"), retries},
		{"a request refused", "responses, the key route", rxFailed("invalid_prompt", "invalid_request_error"), midStream},
		{"a rate limit", "responses, the key route", rxFailed("rate_limit_exceeded", ""), midStream},

		{"response.failed with a server error", "responses, the ChatGPT route", rxFailed("server_error", ""), retries},
		{"a rate limit, final", "responses, the ChatGPT route", rxFailed("rate_limit_exceeded", ""), final},
		{"a code not to repeat", "responses, the ChatGPT route", rxFailed("chatpass_v2_scope_not_authorized", ""), final},
		{"usage that could not be checked", "responses, the ChatGPT route", rxFailed("subscription_sharing_usage_unavailable", ""), midStream},
	} {
		t.Run(tc.driver+"/"+tc.name, func(t *testing.T) {
			srv, client := h2Server(t, tc.reply)
			text, err := stepOn(t, WithRetraction(context.Background()), drivers[tc.driver].model(t, srv.URL, client))
			if text != "partial" {
				t.Fatalf("text %q, want partial", text)
			}
			var mse *MidStreamError
			var fe *FinalError
			switch tc.want {
			case retries:
				if !retried(err) {
					t.Fatalf("err = %v (%T), want a retryable error", err, err)
				}
			case midStream:
				if !errors.As(err, &mse) || retried(err) {
					t.Fatalf("err = %v (%T), want a *MidStreamError", err, err)
				}
			case final:
				if !errors.As(err, &fe) || retried(err) {
					t.Fatalf("err = %v (%T), want a *FinalError", err, err)
				}
			}
		})
	}
}

// TestTheChatGPTRoutesStatusesStayFinal: the route's 429 and 400 are final
// before any stream, whatever a retracting caller would do with them.
func TestTheChatGPTRoutesStatusesStayFinal(t *testing.T) {
	var d aDriver
	for _, c := range theDrivers() {
		if c.name == "responses, the ChatGPT route" {
			d = c
		}
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv, client := h2Server(t, rxJSON(status, `{"error":{"code":"nope","message":"refused"}}`, "Retry-After", "1"))
			_, err := stepOn(t, WithRetraction(context.Background()), d.model(t, srv.URL, client))
			var fe *FinalError
			if !errors.As(err, &fe) || fe.StatusCode != status || retried(err) {
				t.Fatalf("err = %v (%T), want a final %d", err, err, status)
			}
		})
	}
}

// TestAFailureAfterAProviderRanAToolIsFinal: an attempt that received the result
// of a tool the provider ran itself is not sent again, which would run it again,
// even for a reset. A call the provider only announced has run nothing.
func TestAFailureAfterAProviderRanAToolIsFinal(t *testing.T) {
	reset := errors.New("stream error: stream ID 1; INTERNAL_ERROR; received from peer")
	text := fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: "partial"}
	for _, tc := range []struct {
		name    string
		part    fantasy.StreamPart
		retried bool
	}{
		{"the provider's result", fantasy.StreamPart{Type: fantasy.StreamPartTypeToolResult, ID: "c", ToolCallName: "web_search", ProviderExecuted: true}, false},
		{"its call alone", fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "c", ToolCallName: "web_search", ProviderExecuted: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := []fantasy.StreamPart{text, tc.part, {Type: fantasy.StreamPartTypeError, Error: reset}}
			_, err := stepOn(t, WithRetraction(context.Background()), wrap(stubModel{parts: parts}, newScrubber(canary, nil)))
			if retried(err) != tc.retried {
				t.Fatalf("err = %v (%T), retried %v; want %v", err, err, retried(err), tc.retried)
			}
			var mse *MidStreamError
			if !tc.retried && (!errors.As(err, &mse) || fantasy.IsTransportError(err)) {
				t.Fatalf("err = %v (%T), want a *MidStreamError whose text no retry reads", err, err)
			}
		})
	}
}

// TestAFantasyRetryReentersWithTheWrappedError is Fantasy's own loop on the
// wrapper's error, end to end: a reset is sent again, OnRetry fires before the
// second request, and the second attempt's text is the step's.
func TestAFantasyRetryReentersWithTheWrappedError(t *testing.T) {
	for _, d := range theDrivers() {
		t.Run(d.name, func(t *testing.T) {
			var n atomic.Int32
			srv, client := h2Server(t, func(w http.ResponseWriter, r *http.Request) {
				if n.Add(1) == 1 {
					abortAfter(d.partial)(w, r)
				}
				switch d.name {
				case "anthropic":
					anAnswer("whole")(w, r)
				case "responses, the key route", "responses, the ChatGPT route":
					metaAnswer("whole")(w, r)
				default:
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", textChunk("whole"), finishChunk("stop", true))
				}
			})
			agent := fantasy.NewAgent(fastRetry{d.model(t, srv.URL, client)}, fantasy.WithMaxRetries(1))
			var deltas []string
			retries := 0
			res, err := agent.Stream(WithRetraction(context.Background()), fantasy.AgentStreamCall{
				Prompt: "go",
				OnTextDelta: func(_, delta string) error {
					deltas = append(deltas, delta)
					return nil
				},
				OnRetry: func(*fantasy.ProviderError, time.Duration) {
					retries++
					deltas = append(deltas, "|")
				},
			})
			if err != nil || retries != 1 || n.Load() != 2 {
				t.Fatalf("err %v, %d retries, %d requests; want a success, 1 and 2", err, retries, n.Load())
			}
			if got := strings.Join(deltas, ""); got != "partial|whole" || res.Response.Content.Text() != "whole" {
				t.Fatalf("deltas %q and the answer %q; want partial|whole and whole", got, res.Response.Content.Text())
			}
		})
	}
}

// fastRetry asks Fantasy for a millisecond's wait before a retry, as a provider
// that said Retry-After does, so that a test need not sit out its 5 s backoff.
type fastRetry struct{ fantasy.LanguageModel }

func (m fastRetry) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	inner, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for part := range inner {
			var pe *fantasy.ProviderError
			if part.Type == fantasy.StreamPartTypeError && errors.As(part.Error, &pe) {
				pe.ResponseHeaders = map[string]string{"retry-after-ms": "1"}
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

// TestStreamFaultReadsTheFailure is the classifier's table, on the shapes no
// driver test above can produce on demand.
func TestStreamFaultReadsTheFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"an HTTP/2 reset's text", errors.New("stream error: stream ID 7; INTERNAL_ERROR; received from peer"), true},
		{"the standard library's GOAWAY", errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=1, ErrCode=NO_ERROR, debug=""`), true},
		{"a lost connection", errors.New("http2: client connection lost"), true},
		{"an unexpected EOF", fmt.Errorf("reading: %w", io.ErrUnexpectedEOF), true},
		{"a reset connection", fmt.Errorf("read: %w", syscall.ECONNRESET), true},
		{"a reset connection's text", errors.New("read tcp: connection reset by peer"), true},
		{"a network error", &net.OpError{Op: "read", Err: errors.New("no route")}, true},
		{"Fantasy's incomplete stream", fantasy.NewIncompleteStreamError(), true},
		{"Fantasy's transport error", fantasy.NewTransportError(errors.New("stream error: x")), true},
		{"a provider's 503", &fantasy.ProviderError{StatusCode: 503, Message: "busy"}, true},
		{"a provider's 429", &fantasy.ProviderError{StatusCode: 429, Message: "slow", TransientError: true}, false},
		{"a provider's 400", &fantasy.ProviderError{StatusCode: 400, Message: "no"}, false},
		{"a 500 that says the context is too large", &fantasy.ProviderError{StatusCode: 500, ContextTooLargeErr: true}, false},
		{"an auth failure", &fantasy.ProviderError{AuthError: true, Title: transportTitle}, false},
		{"an in-band event of unknown name", &fantasy.ProviderError{Title: "stream error", ResponseBody: []byte(`{"error":{"code":"weird"}}`)}, false},
		{"an in-band event that names no failure, with a retry's words", &fantasy.ProviderError{
			Title: "stream error", ResponseBody: []byte(`{"error":{"code":"content_filter","message":"stream error: no"}}`), Cause: errors.New("stream error: no")}, false},
		{"anything else", errors.New("the response was not JSON"), false},
		{"the empty step", ErrEmptyStep, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamFault(tc.err); got != tc.want {
				t.Fatalf("streamFault(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A sign-in's own failure says something of the account, whatever carried it.
func TestASignInsFailureIsNoFault(t *testing.T) {
	sentinel := errors.New("signed out")
	s := newScrubber(canary, nil)
	s.sentinels = append(s.sentinels, sentinel)
	if s.fault(fmt.Errorf("%w: connection reset", sentinel)) {
		t.Fatal("a sign-in's failure read as a transport's")
	}
	if !s.fault(errors.New("connection reset")) {
		t.Fatal("the control: a transport's failure")
	}
}

// TestAWaitAStreamAskedForIsHonoured: a stream's own Retry-After is kept across
// the wrapper's rebuild, over Fantasy's backoff.
func TestAWaitAStreamAskedForIsHonoured(t *testing.T) {
	var n atomic.Int32
	reply := anReply([]string{"Retry-After-Ms", "7"}, anStart(anUsage(5, 0, 0, 1)), anBlockOpen(0, "partial"), anErrorEvent("overloaded_error", "busy"))
	srv, client := h2Server(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			reply(w, r)
			return
		}
		anAnswer("whole")(w, r)
	})
	lm, err := New(claude(), WithHTTPClient(client), WithAnthropicAPIBase(func() (string, error) { return srv.URL, nil }))
	if err != nil {
		t.Fatal(err)
	}
	var waited []time.Duration
	_, err = fantasy.NewAgent(lm, fantasy.WithMaxRetries(1)).Stream(WithRetraction(context.Background()), fantasy.AgentStreamCall{
		Prompt:  "go",
		OnRetry: func(_ *fantasy.ProviderError, d time.Duration) { waited = append(waited, d) },
	})
	if err != nil || len(waited) != 1 || waited[0] != 7*time.Millisecond {
		t.Fatalf("err %v, waits %v; want one of 7ms", err, waited)
	}
}

// TestAResetOnAnEndedContextIsNotReplayed: a step whose context has ended by the
// time its stream fails is the cancel's, and is not sent again: a transport can
// report the cancel's own reset before the context's error.
func TestAResetOnAnEndedContextIsNotReplayed(t *testing.T) {
	ctx, cancel := context.WithCancel(WithRetraction(context.Background()))
	parts := []fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: "partial"},
		{Type: fantasy.StreamPartTypeError, Error: errors.New("stream error: stream ID 1; CANCEL; received from peer")}}
	lm := wrap(stubModel{parts: parts}, newScrubber(canary, nil))
	cancel()
	if _, err := stepOn(t, ctx, lm); retried(err) {
		t.Fatalf("err = %v (%T); want no retry", err, err)
	}
}

// TestAMalformedResponseAfterOutputStaysFinal: what a parser found wrong with the
// response is the response's, whatever words or unwrapping it shares with a
// transport's failure. A real truncation, in the same driver, is still retried.
func TestAMalformedResponseAfterOutputStaysFinal(t *testing.T) {
	drivers := map[string]aDriver{}
	for _, d := range theDrivers() {
		drivers[d.name] = d
	}
	compat := func(line string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", textChunk("partial"))
			if line != "" {
				fmt.Fprintf(w, "data: %s\n\n", line)
			}
		}
	}
	for _, tc := range []struct {
		name, driver string
		reply        http.HandlerFunc
		retries      bool
	}{
		{"a delta a hook cannot unmarshal", "openai-compat",
			compat(`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":7},"finish_reason":null}]}`), false},
		{"a stream that just stops", "openai-compat", compat(""), true},
		{"a text delta that is not a string", "anthropic", anReply(nil, anStart(anUsage(5, 0, 0, 1)), anBlockOpen(0, "partial"),
			[]string{anDelta(0, map[string]any{"type": "text_delta", "text": 7})}), false},
		{"a stream that just stops", "anthropic", anReply(nil, anStart(anUsage(5, 0, 0, 1)), anBlockOpen(0, "partial")), true},
		{"an event that is not JSON", "responses, the key route", rxEvents(rxAdded(0, metaMessage("rs_m", "")), rxText(0, "rs_m", "partial"), "{not json"), false},
		{"a stream that just stops", "responses, the key route", rxEvents(rxAdded(0, metaMessage("rs_m", "")), rxText(0, "rs_m", "partial")), true},
	} {
		t.Run(tc.driver+"/"+tc.name, func(t *testing.T) {
			srv, client := h2Server(t, tc.reply)
			text, err := stepOn(t, WithRetraction(context.Background()), drivers[tc.driver].model(t, srv.URL, client))
			if text != "partial" || err == nil {
				t.Fatalf("text %q, err %v", text, err)
			}
			var mse *MidStreamError
			if tc.retries {
				if !retried(err) {
					t.Fatalf("err = %v (%T); want a retryable truncation", err, err)
				}
				return
			}
			if !errors.As(err, &mse) || retried(err) || fantasy.IsTransportError(err) {
				t.Fatalf("err = %v (%T); want a final *MidStreamError", err, err)
			}
		})
	}
}

// TestAViolationBeforeOutputIsStillRetried: nothing reached anyone, so a broken
// protocol there is what it was, an incomplete stream.
func TestAViolationBeforeOutputIsStillRetried(t *testing.T) {
	var d aDriver
	for _, c := range theDrivers() {
		if c.name == "anthropic" {
			d = c
		}
	}
	srv, client := h2Server(t, anReply(nil, anStart(anUsage(5, 0, 0, 1)), []string{anDelta(9, map[string]any{"type": "text_delta", "text": 7})}))
	if _, err := stepOn(t, context.Background(), d.model(t, srv.URL, client)); !retried(err) {
		t.Fatalf("err = %v (%T); want a retryable error", err, err)
	}
}

// TestALongRetryAfterAfterOutputStaysFinal: Fantasy ignores a wait of 60 s or more
// and comes back in 5, so such a step is not sent again; under 60 s the provider's
// wait stands. It is said by a stream's own headers on Anthropic's driver.
func TestALongRetryAfterAfterOutputStaysFinal(t *testing.T) {
	future := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC1123) }
	for _, tc := range []struct {
		name     string
		header   []string
		requests int32
		wait     time.Duration
	}{
		{"120 seconds", []string{"Retry-After", "120"}, 1, 0},
		{"exactly 60 seconds", []string{"Retry-After", "60"}, 1, 0},
		{"60000 ms", []string{"Retry-After-Ms", "60000"}, 1, 0},
		{"59 ms", []string{"Retry-After-Ms", "59"}, 2, 59 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int32
			reply := anReply(tc.header, anStart(anUsage(5, 0, 0, 1)), anBlockOpen(0, "partial"), anErrorEvent("overloaded_error", "busy"))
			srv, client := h2Server(t, func(w http.ResponseWriter, r *http.Request) {
				if n.Add(1) == 1 {
					reply(w, r)
					return
				}
				anAnswer("whole")(w, r)
			})
			lm, err := New(claude(), WithHTTPClient(client), WithAnthropicAPIBase(func() (string, error) { return srv.URL, nil }))
			if err != nil {
				t.Fatal(err)
			}
			var waited []time.Duration
			_, err = fantasy.NewAgent(lm, fantasy.WithMaxRetries(1)).Stream(WithRetraction(context.Background()), fantasy.AgentStreamCall{
				Prompt:  "go",
				OnRetry: func(_ *fantasy.ProviderError, d time.Duration) { waited = append(waited, d) },
			})
			if n.Load() != tc.requests {
				t.Fatalf("%d requests; want %d (err %v)", n.Load(), tc.requests, err)
			}
			if tc.wait == 0 {
				var mse *MidStreamError
				if !errors.As(err, &mse) || mse.StatusCode != 529 || len(waited) != 0 {
					t.Fatalf("err = %v (%T), waits %v; want a final *MidStreamError that still says 529", err, err, waited)
				}
				return
			}
			if err != nil || len(waited) != 1 || waited[0] != tc.wait {
				t.Fatalf("err %v, waits %v; want %v", err, waited, tc.wait)
			}
		})
	}
	t.Run("the reader", func(t *testing.T) {
		for _, tc := range []struct {
			h    map[string]string
			want bool
		}{
			{map[string]string{"retry-after": future(2 * time.Minute)}, true},
			{map[string]string{"retry-after": future(10 * time.Second)}, false},
			{map[string]string{"Retry-After": "90"}, true},
			{map[string]string{"retry-after-ms": "100", "retry-after": "90"}, false},
			{map[string]string{"retry-after": "soon"}, false},
			{nil, false},
		} {
			if got := asksLongWait(&fantasy.ProviderError{ResponseHeaders: tc.h}); got != tc.want {
				t.Errorf("asksLongWait(%v) = %v", tc.h, got)
			}
		}
	})
	t.Run("before output, Fantasy's own rule", func(t *testing.T) {
		parts := []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: &fantasy.ProviderError{Title: "stream error", Message: "busy",
			ResponseBody: []byte(`{"error":{"type":"server_error"}}`), ResponseHeaders: map[string]string{"retry-after": "120"}}}}
		_, err := stepOn(t, WithRetraction(context.Background()), wrap(stubModel{parts: parts}, newScrubber(canary, nil)))
		if !retried(err) {
			t.Fatalf("err = %v (%T); want a retryable error", err, err)
		}
	})
}
