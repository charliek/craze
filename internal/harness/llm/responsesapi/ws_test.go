package responsesapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const wsTestDeadline = 10 * time.Second

type wsTestServer struct {
	url      string
	upgrades atomic.Int32
	posts    atomic.Int32
}

func newWSTestServer(t *testing.T, upgrade func(http.ResponseWriter, *http.Request, int)) *wsTestServer {
	t.Helper()
	s := new(wsTestServer)
	var handlers sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPost {
			s.posts.Add(1)
			events(completed(1, 0, 1, 0))(w, r)
			return
		}
		n := int(s.upgrades.Add(1))
		upgrade(w, r, n)
	}))
	s.url = srv.URL + "/v1"
	t.Cleanup(func() {
		srv.Close()
		handlers.Wait()
	})
	return s
}

type wsPeer struct {
	t    *testing.T
	ctx  context.Context
	conn *websocket.Conn
}

func wsScript(t *testing.T, script func(*wsPeer, int)) func(http.ResponseWriter, *http.Request, int) {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request, n int) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(t.Context(), wsTestDeadline)
		defer cancel()
		script(&wsPeer{t: t, ctx: ctx, conn: conn}, n)
	}
}

type wsTurn struct {
	Type               string            `json:"type"`
	PreviousResponseID string            `json:"previous_response_id"`
	Input              []json.RawMessage `json:"input"`
	Tools              []json.RawMessage `json:"tools"`
	ClientMetadata     map[string]string `json:"client_metadata"`
}

func (p *wsPeer) read() wsTurn {
	p.t.Helper()
	var req wsTurn
	typ, data, err := p.conn.Read(p.ctx)
	if err != nil {
		p.t.Errorf("read response.create: %v", err)
		return req
	}
	if err = json.Unmarshal(data, &req); err != nil || typ != websocket.MessageText || req.Type != "response.create" {
		p.t.Errorf("response.create type = %v, decode = %v, message type = %q", typ, err, req.Type)
	}
	return req
}

func (p *wsPeer) expect(prev string, input int, state string) wsTurn {
	p.t.Helper()
	req := p.read()
	if req.PreviousResponseID != prev || len(req.Input) != input || req.ClientMetadata[turnStateHeader] != state {
		p.t.Errorf("request previous_response_id = %q, input length = %d, turn state = %q; want %q, %d, %q",
			req.PreviousResponseID, len(req.Input), req.ClientMetadata[turnStateHeader], prev, input, state)
	}
	return req
}

func (p *wsPeer) send(frames ...string) {
	p.t.Helper()
	for _, f := range frames {
		if err := p.conn.Write(p.ctx, websocket.MessageText, []byte(f)); err != nil {
			p.t.Errorf("send frame: %v", err)
			return
		}
	}
}

func (p *wsPeer) waitForClose() {
	p.t.Helper()
	_, _, err := p.conn.Read(p.ctx)
	if err == nil || p.ctx.Err() == context.DeadlineExceeded {
		p.t.Errorf("waiting for socket close: %v", err)
	}
}

func wsCreated(id string) string {
	return ev(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
}

func wsTestRequest() Request {
	return Request{Model: "gpt-test", Input: []Item{UserMessage(InputText("hi"))},
		Tools: []Tool{{Name: "lookup"}}, Effort: "low"}
}

func wsTestReplay(items []OutputItem) ([]Item, bool) {
	var out []Item
	for _, it := range items {
		switch it.Type {
		case "message":
			out = append(out, AssistantMessagePhase(it.Text, it.Phase))
		case "function_call":
			out = append(out, FunctionCall(it.CallID, it.Name, it.Arguments))
		default:
			return nil, false
		}
	}
	return out, true
}

func wsTestClient(t *testing.T, baseURL string, mode TransportMode, opts ...func(*Config)) *Client {
	t.Helper()
	cfg := Config{Route: RouteKey, APIKey: "test-key", BaseURL: baseURL,
		Transport: mode, WebSocketEligible: true, ReplayOutput: wsTestReplay}
	for _, opt := range opts {
		opt(&cfg)
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func wsTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), wsTestDeadline)
	t.Cleanup(cancel)
	return ctx
}

func openWSStream(t *testing.T, c *Client, req Request) *Stream {
	t.Helper()
	s, err := c.Stream(wsTestContext(t), req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func finishWSStream(t *testing.T, s *Stream) {
	t.Helper()
	for s.Next() {
	}
	if s.Err() != nil || s.Event().Kind != Completed {
		t.Fatalf("finish: last event = %+v, err = %v", s.Event(), s.Err())
	}
}

func runWSTurn(t *testing.T, c *Client, req Request) {
	t.Helper()
	finishWSStream(t, openWSStream(t, c, req))
}

func TestWSIncrementalToolLoop(t *testing.T) {
	srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, _ int) {
		for i := range 3 {
			prev := ""
			size := 2 // developer and user
			if i > 0 {
				prev, size = fmt.Sprintf("r%d", i-1), 1
			}
			req := p.expect(prev, size, "")
			if len(req.Tools) != 1 {
				t.Errorf("step %d tools = %v", i, req.Tools)
			}
			if i > 0 && len(req.Input) == 1 && !bytes.Contains(req.Input[0], []byte(`"type":"function_call_output"`)) {
				t.Errorf("step %d delta = %s", i, req.Input[0])
			}
			out := call("fc", fmt.Sprintf("call-%d", i), "lookup", "{}")
			p.send(wsCreated(fmt.Sprintf("r%d", i)), itemDone(0, out), completed(1, 0, 1, 0))
		}
	}))
	c := wsTestClient(t, srv.url, TransportWebSocket, func(cfg *Config) { cfg.DeveloperInstructions = true })
	req := wsTestRequest()
	req.Instructions = "Use tools"
	for i := range 3 {
		runWSTurn(t, c, req)
		id := fmt.Sprintf("call-%d", i)
		req.Input = append(req.Input, FunctionCall(id, "lookup", "{}"), FunctionCallOutput(id, "result"))
	}
	if got := c.Stats(); got != (Stats{WSDials: 1, WSRequests: 3, WSIncremental: 2}) {
		t.Fatalf("stats = %+v", got)
	}
}

func TestWSChainResetsOnSameConnection(t *testing.T) {
	normal := itemDone(0, message("msg", "answer"))
	for _, tc := range []struct {
		name    string
		output  []string
		change  func(*Request)
		decline bool
	}{
		{name: "unknown item", output: []string{itemDone(0, map[string]any{"type": "future_item"})}},
		{name: "replay declined", decline: true},
		{name: "non-prefix", output: []string{textDelta(0, "msg", "different"), normal}},
		{name: "index gap", output: []string{itemDone(1, message("msg", "answer"))}},
		{name: "empty delta", change: func(r *Request) { r.Input = r.Input[:2] }},
		{name: "model", change: func(r *Request) { r.Model = "other" }},
		{name: "tools", change: func(r *Request) { r.Tools = []Tool{{Name: "other"}} }},
		{name: "effort", change: func(r *Request) { r.Effort = "high" }},
		{name: "parallel nil to false", change: func(r *Request) { r.ParallelToolCalls = new(bool) }},
		{name: "history", change: func(r *Request) { r.Input[0] = UserMessage(InputText("rewrite")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := wsTestRequest()
			next := req
			next.Input = append([]Item{req.Input[0]}, AssistantMessage("answer"), FunctionCallOutput("call", "result"))
			if tc.name == "unknown item" {
				next.Input = append([]Item{req.Input[0]}, FunctionCallOutput("call", "result"))
			}
			if tc.change != nil {
				tc.change(&next)
			}
			srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, _ int) {
				p.expect("", 1, "")
				if tc.output == nil {
					p.send(normal)
				} else {
					p.send(tc.output...)
				}
				p.send(completed(1, 0, 1, 0))
				p.expect("", len(next.Input), "")
				p.send(completed(1, 0, 1, 0))
			}))
			replayCalls := 0
			c := wsTestClient(t, srv.url, TransportWebSocket, func(cfg *Config) {
				cfg.ReplayOutput = func(items []OutputItem) ([]Item, bool) {
					replayCalls++
					if tc.decline {
						return nil, false
					}
					return wsTestReplay(items)
				}
			})
			runWSTurn(t, c, req)
			if (tc.name == "non-prefix" || tc.name == "index gap") && replayCalls != 0 {
				t.Fatal("integrity failure reached ReplayOutput")
			}
			runWSTurn(t, c, next)
			if got := c.Stats(); got != (Stats{WSDials: 1, WSRequests: 2}) {
				t.Fatalf("stats = %+v", got)
			}
		})
	}
}

func TestWSAddedPhaseAndCompletedID(t *testing.T) {
	srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, _ int) {
		p.expect("", 1, "")
		begin := message("msg", "")
		begin["phase"] = "commentary"
		p.send(added(0, begin), itemDone(0, message("msg", "answer")), completed(1, 0, 1, 0))
		p.expect("resp_1", 1, "")
		p.send(completed(1, 0, 1, 0))
	}))
	c := wsTestClient(t, srv.url, TransportWebSocket)
	req := wsTestRequest()
	s := openWSStream(t, c, req)
	var phase string
	for s.Next() {
		if s.Event().Kind == ItemDone {
			phase = s.Event().Item.Phase
		}
	}
	if phase != "commentary" || s.Err() != nil {
		t.Fatalf("phase = %q, err = %v", phase, s.Err())
	}
	req.Input = append(req.Input, AssistantMessagePhase("answer", "commentary"), FunctionCallOutput("call", "result"))
	runWSTurn(t, c, req)
}

type wsTestCredentials struct {
	generation atomic.Uint64
	invalid    atomic.Int32
	refuse     atomic.Bool
}

var errWSFixtureCredential = errors.New("fixture refuses credentials")

func (c *wsTestCredentials) Token(context.Context) (string, uint64, error) {
	if c.refuse.Load() {
		return "", 0, errWSFixtureCredential
	}
	gen := c.generation.Load()
	return fmt.Sprintf("token-%d", gen), gen, nil
}

func (c *wsTestCredentials) Invalidate(_ context.Context, gen uint64) error {
	c.invalid.Add(1)
	c.generation.CompareAndSwap(gen, gen+1)
	return nil
}

func wsChatGPT(creds Credentials) func(*Config) {
	return func(cfg *Config) { cfg.Route, cfg.APIKey, cfg.Credentials = RouteChatGPT, "", creds }
}

func TestWSTurnState(t *testing.T) {
	meta := func(typ, state string) string {
		return ev(map[string]any{"type": typ, "headers": map[string]string{"X-Codex-Turn-State": state}})
	}
	srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, _ int) {
		p.expect("", 1, "")
		p.send(meta("codex.response.metadata", "from-peek"), wsCreated("one"), completed(1, 0, 1, 0))
		p.expect("one", 1, "from-peek")
		p.send(wsCreated("two"), meta("response.metadata", "from-stream"), completed(1, 0, 1, 0))
		p.expect("two", 1, "from-stream")
		p.send(wsCreated("three"), completed(1, 0, 1, 0))
		p.expect("three", 1, "")
		p.send(meta("response.metadata", "new-user-turn"), completed(1, 0, 1, 0))
		p.expect("", 5, "")
		p.send(completed(1, 0, 1, 0))
	}))
	c := wsTestClient(t, srv.url, TransportWebSocket, wsChatGPT(new(wsTestCredentials)))
	req := wsTestRequest()
	runWSTurn(t, c, req)
	req.Input = append(req.Input, FunctionCallOutput("call", "result"))
	runWSTurn(t, c, req)
	req.Input = append(req.Input, FunctionCallOutput("second", "result"))
	runWSTurn(t, c, req)
	req.Input = append(req.Input, UserMessage(InputText("next turn")))
	runWSTurn(t, c, req)
	req.Input = append(req.Input, FunctionCallOutput("other", "result"))
	req.Effort = "high"
	runWSTurn(t, c, req)
	if got := c.Stats(); got.WSDials != 1 || got.WSIncremental != 3 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestWSIdleRecovery(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(fmt.Sprintf("ageExpired=%t", expire), func(t *testing.T) {
			srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, dial int) {
				if dial == 1 {
					p.expect("", 1, "")
					p.send(completed(1, 0, 1, 0))
					if !expire {
						p.expect("resp_1", 1, "")
					}
					p.waitForClose()
				} else {
					p.expect("", 2, "")
					p.send(completed(1, 0, 1, 0))
				}
			}))
			c := wsTestClient(t, srv.url, TransportAuto)
			c.firstFrameTimeout = 120 * time.Millisecond
			now := time.Now()
			c.now = func() time.Time { return now }
			req := wsTestRequest()
			runWSTurn(t, c, req)
			if expire {
				now = now.Add(c.maxIdleAge + time.Second)
			}
			req.Input = append(req.Input, FunctionCallOutput("call", "result"))
			runWSTurn(t, c, req)
			want := Stats{WSDials: 2, WSRequests: 2}
			if !expire {
				want.WSRequests, want.WSIncremental, want.IdleSocketRetries = 3, 1, 1
			}
			if got := c.Stats(); got != want {
				t.Fatalf("stats = %+v, want %+v", got, want)
			}
		})
	}
}

func TestWSStaleChainRecovery(t *testing.T) {
	for _, tc := range []struct {
		name       string
		frame      string
		chatGPT    bool
		closePeer  bool
		rejectNext bool
		unrelated  bool
	}{
		{name: "key coded open", frame: `{"type":"error","status":400,"error":{"code":"previous_response_not_found"}}`},
		{name: "ChatGPT codeless and closed", frame: `{"type":"error","status":400,"error":{"message":"Unknown PREVIOUS_RESPONSE_ID"}}`, chatGPT: true, closePeer: true},
		{name: "stale on retry", frame: `{"type":"error","status":400,"error":{"param":"previous_response_id"}}`, rejectNext: true},
		{name: "unrelated codeless 400", frame: `{"type":"error","status":400,"error":{"message":"Invalid model"}}`, unrelated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, dial int) {
				if dial == 1 {
					p.expect("", 1, "")
					p.send(completed(1, 0, 1, 0))
					p.expect("resp_1", 1, "")
					p.send(tc.frame)
					if !tc.closePeer {
						p.waitForClose()
					}
					return
				}
				p.expect("", 2, "")
				if tc.rejectNext {
					p.send(tc.frame)
					return
				}
				p.send(completed(1, 0, 1, 0))
			}))
			var opts []func(*Config)
			if tc.chatGPT {
				opts = append(opts, wsChatGPT(new(wsTestCredentials)))
			}
			c := wsTestClient(t, srv.url, TransportAuto, opts...)
			req := wsTestRequest()
			runWSTurn(t, c, req)
			req.Input = append(req.Input, FunctionCallOutput("call", "result"))
			want := Stats{WSDials: 2, WSRequests: 3, WSIncremental: 1, StaleChainRecoveries: 1}
			if tc.rejectNext || tc.unrelated {
				_, err := c.Stream(wsTestContext(t), req)
				var refusal *Error
				if !errors.As(err, &refusal) || refusal.StatusCode != 400 {
					t.Fatalf("error = %v", err)
				}
			} else {
				runWSTurn(t, c, req)
			}
			if tc.unrelated {
				want.WSDials, want.WSRequests, want.StaleChainRecoveries = 1, 2, 0
			}
			if got := c.Stats(); got != want {
				t.Fatalf("stats = %+v, want %+v", got, want)
			}
		})
	}
}

func TestWS401Renewal(t *testing.T) {
	for _, route := range []Route{RouteChatGPT, RouteKey} {
		for _, inSocket := range []bool{false, true} {
			t.Run(fmt.Sprintf("route=%d/inSocket=%t", route, inSocket), func(t *testing.T) {
				srv := newWSTestServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
					token := "test-key"
					if route == RouteChatGPT {
						token = fmt.Sprintf("token-%d", n-1)
					}
					if got := r.Header.Get("Authorization"); got != "Bearer "+token {
						t.Errorf("authorization = %q", got)
					}
					if n == 1 && !inSocket {
						w.WriteHeader(401)
						_, _ = fmt.Fprint(w, `{"error":{"code":"invalid_api_key"}}`)
						return
					}
					wsScript(t, func(p *wsPeer, n int) {
						p.expect("", 1, "")
						if n == 1 {
							p.send(`{"type":"error","status":401,"error":{"code":"invalid_api_key"}}`)
						} else {
							p.send(completed(1, 0, 1, 0))
						}
					})(w, r, n)
				})
				creds := new(wsTestCredentials)
				var opts []func(*Config)
				if route == RouteChatGPT {
					opts = append(opts, wsChatGPT(creds))
				}
				c := wsTestClient(t, srv.url, TransportWebSocket, opts...)
				if route == RouteChatGPT {
					runWSTurn(t, c, wsTestRequest())
					if srv.upgrades.Load() != 2 || creds.invalid.Load() != 1 {
						t.Fatalf("upgrades = %d, invalidations = %d", srv.upgrades.Load(), creds.invalid.Load())
					}
				} else {
					_, err := c.Stream(wsTestContext(t), wsTestRequest())
					var refusal *Error
					if !errors.As(err, &refusal) || refusal.StatusCode != 401 || refusal.Code != "invalid_api_key" || srv.upgrades.Load() != 1 {
						t.Fatalf("error = %v, upgrades = %d", err, srv.upgrades.Load())
					}
				}
			})
		}
	}
}

func TestWSConnectionLimitRecoveryIsBounded(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		t.Run(fmt.Sprint(repeat), func(t *testing.T) {
			srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, dial int) {
				p.expect("", 1, "")
				if dial == 1 || repeat {
					p.send(`{"type":"error","error":{"code":"websocket_connection_limit_reached"}}`)
				} else {
					p.send(completed(1, 0, 1, 0))
				}
			}))
			c := wsTestClient(t, srv.url, TransportAuto)
			if repeat {
				_, err := c.Stream(wsTestContext(t), wsTestRequest())
				var refusal *Error
				if !errors.As(err, &refusal) || refusal.Code != "websocket_connection_limit_reached" {
					t.Fatalf("err = %v", err)
				}
			} else {
				runWSTurn(t, c, wsTestRequest())
			}
			if got := c.Stats(); got != (Stats{WSDials: 2, WSRequests: 2}) {
				t.Fatalf("stats = %+v", got)
			}
		})
	}
}

func TestWS401RecoveryBoundary(t *testing.T) {
	for _, afterOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("afterOutput=%t", afterOutput), func(t *testing.T) {
			srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, _ int) {
				p.expect("", 1, "")
				if afterOutput {
					p.send(created(), textDelta(0, "msg", "partial"))
				}
				p.send(`{"type":"error","status":401,"headers":{"retry-after":60},"error":{"code":"invalid_api_key"}}`)
			}))
			creds := new(wsTestCredentials)
			c := wsTestClient(t, srv.url, TransportAuto, wsChatGPT(creds))
			s, err := c.Stream(wsTestContext(t), wsTestRequest())
			dials, renewals := uint64(2), int32(1)
			if afterOutput {
				if s == nil || err != nil {
					t.Fatalf("startup error = %v", err)
				}
				evs, streamErr := collect(t, s)
				err = streamErr
				if deltas(evs, TextDelta, 0) != "partial" {
					t.Fatalf("events = %v", kinds(evs))
				}
				dials, renewals = 1, 0
			}
			var refusal *Error
			if !errors.As(err, &refusal) || refusal.StatusCode != 401 || refusal.Header.Get("Retry-After") != "60" ||
				creds.invalid.Load() != renewals || c.Stats() != (Stats{WSDials: dials, WSRequests: dials}) {
				t.Fatalf("error = %v, invalidations = %d, stats = %+v", err, creds.invalid.Load(), c.Stats())
			}
		})
	}
}

func TestWSMessageLimits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     TransportMode
		outgoing bool
		after    bool
		limit    bool
		binary   bool
		want     error
		sticky   bool
	}{
		{name: "auto peer pre-output", mode: TransportAuto, sticky: true},
		{name: "auto peer post-output", mode: TransportAuto, after: true, want: ErrWSMessageTooBig, sticky: true},
		{name: "strict peer", mode: TransportWebSocket, want: ErrWSMessageTooBig},
		{name: "local read limit", mode: TransportAuto, limit: true, want: errEventTooLarge},
		{name: "binary protocol error", mode: TransportAuto, after: true, binary: true, want: errUnexpectedBinaryFrame},
		{name: "auto outgoing", mode: TransportAuto, outgoing: true},
		{name: "strict outgoing", mode: TransportWebSocket, outgoing: true, want: ErrWSMessageTooBig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newWSTestServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
				if tc.outgoing && n == 1 {
					w.Header().Set("X-Codex-Websocket-Max-Message-Bytes", "32")
				}
				wsScript(t, func(p *wsPeer, n int) {
					if tc.outgoing && n == 1 {
						p.waitForClose()
						return
					}
					p.expect("", 1, "")
					if n != 1 {
						p.send(completed(1, 0, 1, 0))
						return
					}
					if tc.after {
						p.send(created(), textDelta(0, "msg", "observable"))
					}
					switch {
					case tc.limit:
						_ = p.conn.Write(p.ctx, websocket.MessageText, make([]byte, wsReadLimit+1))
					case tc.binary:
						_ = p.conn.Write(p.ctx, websocket.MessageBinary, []byte("binary"))
					default:
						_ = p.conn.Close(websocket.StatusMessageTooBig, "peer limit")
					}
				})(w, r, n)
			})
			c := wsTestClient(t, srv.url, tc.mode)
			s, err := c.Stream(wsTestContext(t), wsTestRequest())
			if tc.after && (err != nil || s == nil) {
				t.Fatalf("failed before output: %v", err)
			}
			if s != nil {
				evs, streamErr := collect(t, s)
				err = streamErr
				if tc.after && deltas(evs, TextDelta, 0) != "observable" {
					t.Fatalf("events = %v", kinds(evs))
				}
				if err == nil && (len(evs) == 0 || evs[len(evs)-1].Kind != Completed) {
					t.Fatalf("stream did not complete: %v", kinds(evs))
				}
			}
			if !errors.Is(err, tc.want) || errors.Is(err, ErrIncomplete) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			wantFallback := uint64(0)
			if tc.want == nil {
				wantFallback = 1
			}
			if got := c.Stats(); got.SSEFallbacks != wantFallback || got.SSERequests != wantFallback || c.isSticky() != tc.sticky {
				t.Fatalf("stats = %+v, sticky = %v", got, c.isSticky())
			}
			runWSTurn(t, c, wsTestRequest())
			wantDials := uint64(2)
			if tc.sticky || (tc.outgoing && tc.mode == TransportAuto) {
				wantDials = 1
			}
			if got := c.Stats(); got.WSDials != wantDials {
				t.Fatalf("next-request stats = %+v, want WSDials = %d", got, wantDials)
			}
		})
	}
}

func TestWSFallbackAndStickyRegistry(t *testing.T) {
	for _, status := range []int{301, 400, 403, 404, 405, 426, 429, 501, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := newWSTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				if status == 301 {
					w.Header().Set("Location", "/redirect-must-not-run")
				}
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, `{"error":{"code":"fixture_refusal","message":"upgrade denied"}}`)
			})
			shared := NewStickyRegistry()
			opt := func(cfg *Config) { cfg.Sticky = shared }
			a := wsTestClient(t, srv.url, TransportAuto, opt)
			b := wsTestClient(t, srv.url, TransportAuto, opt)
			runWSTurn(t, a, wsTestRequest())
			immediate := status == 301 || status == 400 || status == 404 || status == 405 || status == 426 || status == 501
			if a.isSticky() != immediate {
				t.Fatalf("after first refusal, sticky = %v", a.isSticky())
			}
			runWSTurn(t, b, wsTestRequest())
			runWSTurn(t, a, wsTestRequest())
			wantUpgrades := int32(2)
			if immediate {
				wantUpgrades = 1
			}
			if srv.upgrades.Load() != wantUpgrades || srv.posts.Load() != 3 || !a.isSticky() || !b.isSticky() {
				t.Fatalf("upgrades = %d, posts = %d, sticky = %v/%v", srv.upgrades.Load(), srv.posts.Load(), a.isSticky(), b.isSticky())
			}
			if a.Stats().SSEFallbacks != 2 || b.Stats().SSEFallbacks != 1 {
				t.Fatalf("stats = %+v / %+v", a.Stats(), b.Stats())
			}
			strict := wsTestClient(t, srv.url, TransportWebSocket, opt)
			_, err := strict.Stream(wsTestContext(t), wsTestRequest())
			var refusal *Error
			if !errors.As(err, &refusal) || refusal.StatusCode != status || refusal.Code != "fixture_refusal" ||
				strict.Stats().SSERequests != 0 || srv.upgrades.Load() != wantUpgrades+1 {
				t.Fatalf("strict err = %v, stats = %+v, upgrades = %d", err, strict.Stats(), srv.upgrades.Load())
			}
		})
	}
}

type wsRoundTripper func(*http.Request) (*http.Response, error)

func (rt wsRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

func TestWSNetworkAndStreamDropFallback(t *testing.T) {
	for _, tc := range []string{"network", "pre-output", "post-output"} {
		t.Run(tc, func(t *testing.T) {
			srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, _ int) {
				p.expect("", 1, "")
				if tc == "post-output" {
					p.send(created(), textDelta(0, "msg", "partial"))
				}
			}))
			var networkFailures atomic.Int32
			c := wsTestClient(t, srv.url, TransportAuto, func(cfg *Config) {
				cfg.HTTPClient = &http.Client{Transport: wsRoundTripper(func(r *http.Request) (*http.Response, error) {
					if tc == "network" && r.Method == http.MethodGet {
						networkFailures.Add(1)
						return nil, errors.New("fixture network failure")
					}
					return http.DefaultTransport.RoundTrip(r)
				})}
			})
			for i := range 2 {
				if tc == "post-output" {
					evs, err := collect(t, openWSStream(t, c, wsTestRequest()))
					if !errors.Is(err, ErrIncomplete) || deltas(evs, TextDelta, 0) != "partial" || srv.posts.Load() != 0 {
						t.Fatalf("events = %v, err = %v, posts = %d", kinds(evs), err, srv.posts.Load())
					}
				} else {
					runWSTurn(t, c, wsTestRequest())
				}
				if c.isSticky() != (i == 1) {
					t.Fatalf("attempt %d, sticky = %v", i+1, c.isSticky())
				}
			}
			runWSTurn(t, c, wsTestRequest())
			if attempts := srv.upgrades.Load() + networkFailures.Load(); attempts != 2 {
				t.Fatalf("upgrade attempts = %d", attempts)
			}
			if tc == "post-output" && c.Stats().SSEFallbacks != 1 {
				t.Fatalf("mid-stream fallback stats = %+v", c.Stats())
			}
		})
	}
}

func wsAwait[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatalf("waiting for fixture: %v", ctx.Err())
		var zero T
		return zero
	}
}

func (p *wsPeer) await(ch <-chan struct{}) {
	p.t.Helper()
	select {
	case <-ch:
	case <-p.ctx.Done():
		p.t.Errorf("waiting for fixture: %v", p.ctx.Err())
	}
}

func TestWSPreDialForegroundAdoption(t *testing.T) {
	srv := newWSTestServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Header.Get("Session-Id") != "session" {
			t.Errorf("session-id = %q", r.Header.Get("Session-Id"))
		}
		wsScript(t, func(p *wsPeer, _ int) {
			p.expect("", 1, "")
			p.send(completed(1, 0, 1, 0))
		})(w, r, n)
	})
	started, proceed := make(chan struct{}, 1), make(chan struct{})
	c := wsTestClient(t, srv.url, TransportWebSocket, wsChatGPT(new(wsTestCredentials)), func(cfg *Config) {
		cfg.HTTPClient = &http.Client{Transport: wsRoundTripper(func(r *http.Request) (*http.Response, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-proceed:
				return http.DefaultTransport.RoundTrip(r)
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})}
	})
	c.PreDial("session")
	ctx := wsTestContext(t)
	wsAwait(t, ctx, started)
	req := wsTestRequest()
	req.Headers = map[string]string{"session-id": "session"}
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err := c.Stream(waitCtx, req)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || c.Stats() != (Stats{}) {
		t.Fatalf("waiting stream err = %v, stats = %+v", err, c.Stats())
	}
	type result struct {
		s   *Stream
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		s, err := c.Stream(ctx, req)
		resultCh <- result{s, err}
	}()
	close(proceed)
	res := wsAwait(t, ctx, resultCh)
	if res.err != nil {
		t.Fatal(res.err)
	}
	defer res.s.Close()
	finishWSStream(t, res.s)
	if got := c.Stats(); got != (Stats{WSDials: 1, WSRequests: 1}) {
		t.Fatalf("stats = %+v", got)
	}
}

func TestWSCloseJoinsPreDial(t *testing.T) {
	for _, handshaken := range []bool{false, true} {
		t.Run(fmt.Sprintf("handshaken=%t", handshaken), func(t *testing.T) {
			srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, _ int) { p.waitForClose() }))
			paused, returned := make(chan struct{}), make(chan struct{})
			c := wsTestClient(t, srv.url, TransportAuto, func(cfg *Config) {
				cfg.HTTPClient = &http.Client{Transport: wsRoundTripper(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodPost {
						return http.DefaultTransport.RoundTrip(r)
					}
					var resp *http.Response
					var err error
					if handshaken {
						resp, err = http.DefaultTransport.RoundTrip(r)
					}
					close(paused)
					<-r.Context().Done()
					defer close(returned)
					if !handshaken {
						return nil, r.Context().Err()
					}
					return resp, err
				})}
			})
			c.PreDial("session")
			ctx := wsTestContext(t)
			wsAwait(t, ctx, paused)
			var closers sync.WaitGroup
			for range 2 {
				closers.Go(func() { _ = c.Close() })
			}
			closers.Wait()
			select {
			case <-returned:
			default:
				t.Fatal("Close returned before the dial")
			}
			c.wsMu.Lock()
			owned := c.ws != nil || c.preDialing || c.preDialDone != nil
			c.wsMu.Unlock()
			if owned {
				t.Fatal("connection published on closed client")
			}
			runWSTurn(t, c, wsTestRequest())
			if c.Stats().SSERequests != 1 || c.isSticky() {
				t.Fatalf("closed client stats = %+v, sticky = %v", c.Stats(), c.isSticky())
			}
		})
	}
}

func TestWSPreDial401IsSilent(t *testing.T) {
	srv := newWSTestServer(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n <= 2 {
			w.WriteHeader(http.StatusUnauthorized)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	creds := new(wsTestCredentials)
	c := wsTestClient(t, srv.url, TransportAuto, wsChatGPT(creds))
	for range 2 {
		c.PreDial("session")
		c.wsMu.Lock()
		done := c.preDialDone
		c.wsMu.Unlock()
		if done != nil {
			wsAwait(t, wsTestContext(t), done)
		}
	}
	if creds.invalid.Load() != 0 || srv.upgrades.Load() != 2 || c.isSticky() {
		t.Fatalf("invalidations = %d, upgrades = %d, sticky = %v", creds.invalid.Load(), srv.upgrades.Load(), c.isSticky())
	}
	runWSTurn(t, c, wsTestRequest())
	if c.isSticky() || c.Stats().SSEFallbacks != 1 {
		t.Fatalf("first foreground failure: sticky = %v, stats = %+v", c.isSticky(), c.Stats())
	}
}

func TestWSClientCloseDrainsActiveStream(t *testing.T) {
	proceed := make(chan struct{})
	peerClosed := make(chan struct{})
	srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, _ int) {
		p.expect("", 1, "")
		p.send(created())
		p.await(proceed)
		p.send(itemDone(0, message("msg", "after client close")), completed(1, 0, 1, 0))
		p.waitForClose()
		close(peerClosed)
	}))
	c := wsTestClient(t, srv.url, TransportWebSocket)
	s := openWSStream(t, c, wsTestRequest())
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	runWSTurn(t, c, wsTestRequest())
	close(proceed)
	evs, err := collect(t, s)
	if err != nil || deltas(evs, TextDelta, 0) != "after client close" {
		t.Fatalf("events = %v, err = %v", kinds(evs), err)
	}
	wsAwait(t, wsTestContext(t), peerClosed)
	if got := c.Stats(); got != (Stats{WSDials: 1, WSRequests: 1, SSERequests: 1}) {
		t.Fatalf("stats = %+v", got)
	}
}

func TestWSStatelessBusyAndOldStreamClosePreserveChain(t *testing.T) {
	proceed := make(chan struct{})
	srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, _ int) {
		p.expect("", 1, "")
		p.send(wsCreated("one"), completed(1, 0, 1, 0))
		p.expect("one", 1, "")
		p.send(wsCreated("two"))
		p.await(proceed)
		p.send(completed(1, 0, 1, 0))
		p.expect("two", 1, "")
		p.send(completed(1, 0, 1, 0))
	}))
	c := wsTestClient(t, srv.url, TransportAuto)
	req := wsTestRequest()
	old := openWSStream(t, c, req)
	finishWSStream(t, old)
	bypass := req
	bypass.Stateless = true
	runWSTurn(t, c, bypass)
	req.Input = append(req.Input, FunctionCallOutput("one", "result"))
	active := openWSStream(t, c, req)
	var calls sync.WaitGroup
	calls.Go(func() { _ = old.Close() })
	busyDone := make(chan error, 1)
	busyCtx := wsTestContext(t)
	calls.Go(func() {
		s, err := c.Stream(busyCtx, wsTestRequest())
		if err == nil {
			defer s.Close()
			for s.Next() {
			}
			err = s.Err()
		}
		busyDone <- err
	})
	calls.Wait()
	if err := <-busyDone; err != nil {
		t.Fatal(err)
	}
	close(proceed)
	finishWSStream(t, active)
	req.Input = append(req.Input, FunctionCallOutput("two", "result"))
	runWSTurn(t, c, req)
	if got := c.Stats(); got != (Stats{WSDials: 1, WSRequests: 3, WSIncremental: 2, SSERequests: 2}) {
		t.Fatalf("stats = %+v", got)
	}
}

func TestWSUpgradeIdentityAndCredentialChecks(t *testing.T) {
	srv := newWSTestServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		ua := "ua-one"
		if n >= 3 {
			ua = "ua-two"
		}
		wantAuth := "Bearer token-0"
		if n >= 5 {
			wantAuth = "Bearer token-1"
		}
		if h := r.Header; h.Get("Authorization") != wantAuth || h.Get("User-Agent") != ua ||
			h.Get("OpenAI-Beta") != "responses_websockets=2026-02-06" || h.Get("Cookie") != "" ||
			h.Get("Sec-WebSocket-Extensions") != "" || h.Get("X-Allowed") != "value" ||
			h.Get("Sec-WebSocket-Protocol") != "" || h.Get("Upgrade") != "websocket" {
			t.Errorf("unexpected upgrade headers: %v", h)
		}
		wsScript(t, func(p *wsPeer, dial int) {
			p.expect("", 1, "")
			p.send(completed(1, 0, 1, 0))
			if dial == 1 {
				p.expect("resp_1", 1, "")
				p.send(completed(1, 0, 1, 0))
			}
			p.waitForClose()
		})(w, r, n)
	})
	creds := new(wsTestCredentials)
	c := wsTestClient(t, srv.url, TransportAuto, wsChatGPT(creds))
	req := wsTestRequest()
	req.UserAgent = "ua-one"
	req.Headers = map[string]string{
		"x-allowed": "value", "session-id": "one", "AUTHORIZATION": "attacker", "COOKIE": "cookie",
		"sEC-WeBSoCKet-extensions": "permessage-deflate", "sec-websocket-protocol": "bogus",
		"openai-beta": "override", "upgrade": "override",
	}
	runWSTurn(t, c, req)
	req.Headers = map[string]string{"X-ALLOWED": "value", "SESSION-ID": "one"}
	req.Input = append(req.Input, FunctionCallOutput("call", "result"))
	runWSTurn(t, c, req)
	req.Input = req.Input[:1]
	req.Headers["SESSION-ID"] = "two"
	runWSTurn(t, c, req)
	req.UserAgent = "ua-two"
	runWSTurn(t, c, req)
	req.Headers["X-Other"] = "new"
	runWSTurn(t, c, req)
	creds.generation.Add(1)
	runWSTurn(t, c, req)
	creds.refuse.Store(true)
	_, err := c.Stream(wsTestContext(t), req)
	if !errors.Is(err, errWSFixtureCredential) {
		t.Fatalf("credential refusal = %v", err)
	}
	if got := c.Stats(); got != (Stats{WSDials: 5, WSRequests: 6, WSIncremental: 1}) {
		t.Fatalf("stats = %+v", got)
	}
}

func TestWSInternalTimeouts(t *testing.T) {
	for _, op := range []string{"dial", "write", "first read", "mid-stream read"} {
		t.Run(op, func(t *testing.T) {
			srv := newWSTestServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
				if op == "dial" {
					<-r.Context().Done()
					return
				}
				wsScript(t, func(p *wsPeer, _ int) {
					if op == "write" {
						// Leave the large client frame unread so the write blocks.
						<-p.ctx.Done()
						return
					}
					p.expect("", 1, "")
					if op == "mid-stream read" {
						p.send(created(), textDelta(0, "msg", "partial"))
					}
					p.waitForClose()
				})(w, r, n)
			})
			c := wsTestClient(t, srv.url, TransportWebSocket)
			req := wsTestRequest()
			switch op {
			case "dial":
				c.dialTimeout = 100 * time.Millisecond
			case "write":
				c.writeTimeout = 150 * time.Millisecond
				req.Input = []Item{UserMessage(InputText(strings.Repeat("x", 16<<20)))}
			default:
				c.idleReadTimeout = 150 * time.Millisecond
			}
			s, err := c.Stream(wsTestContext(t), req)
			if op == "mid-stream read" {
				if err != nil {
					t.Fatal(err)
				}
				evs, streamErr := collect(t, s)
				err = streamErr
				if deltas(evs, TextDelta, 0) != "partial" {
					t.Fatalf("events = %v", kinds(evs))
				}
			}
			var timeout wsTimeoutError
			midStream := op == "mid-stream read"
			if !errors.As(err, &timeout) || !timeout.Timeout() || timeout.Temporary() ||
				errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrIncomplete) != midStream {
				t.Fatalf("error = %T %v", err, err)
			}
		})
	}
}

func TestWSCallerCancellationAndSuccessResetTally(t *testing.T) {
	started := make(chan struct{})
	srv := newWSTestServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		switch n {
		case 1, 4:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			close(started)
			<-r.Context().Done()
		default:
			wsScript(t, func(p *wsPeer, _ int) {
				p.expect("", 1, "")
				p.send(completed(1, 0, 1, 0))
			})(w, r, n)
		}
	})
	c := wsTestClient(t, srv.url, TransportAuto)
	req := wsTestRequest()
	runWSTurn(t, c, req)
	ctx, cancel := context.WithCancel(wsTestContext(t))
	defer cancel()
	out := make(chan error, 1)
	go func() {
		s, err := c.Stream(ctx, req)
		if s != nil {
			_ = s.Close()
		}
		out <- err
	}()
	wait := wsTestContext(t)
	wsAwait(t, wait, started)
	cancel()
	err := wsAwait(t, wait, out)
	if err != context.Canceled || c.isSticky() {
		t.Fatalf("err = %v, sticky = %v", err, c.isSticky())
	}
	_, err = c.Stream(ctx, req)
	if err != context.Canceled || srv.upgrades.Load() != 2 {
		t.Fatalf("already canceled: err = %v, upgrades = %d", err, srv.upgrades.Load())
	}
	runWSTurn(t, c, req) // Resets the first failure.
	// Force the next round to dial and fail, proving completion reset the tally.
	c.wsMu.Lock()
	idle := c.ws
	c.wsMu.Unlock()
	_ = idle.conn.CloseNow()
	runWSTurn(t, c, req)
	if c.isSticky() || c.Stats().SSEFallbacks != 2 || srv.upgrades.Load() != 4 {
		t.Fatalf("sticky = %v, stats = %+v, upgrades = %d", c.isSticky(), c.Stats(), srv.upgrades.Load())
	}
}

func TestWSLateCloseAfterLeaseReplacement(t *testing.T) {
	srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, dial int) {
		p.expect("", 1, "")
		p.send(created(), completed(1, 0, 1, 0))
		if dial == 2 {
			p.expect("resp_1", 1, "")
			p.send(completed(1, 0, 1, 0))
		}
		p.waitForClose()
	}))
	c := wsTestClient(t, srv.url, TransportWebSocket)
	req := wsTestRequest()
	old := openWSStream(t, c, req)
	finishWSStream(t, old)
	req.UserAgent = "forces a separate lease"
	replacement := openWSStream(t, c, req)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	finishWSStream(t, replacement)
	req.Input = append(req.Input, FunctionCallOutput("call", "result"))
	runWSTurn(t, c, req)
	if got := c.Stats(); got != (Stats{WSDials: 2, WSRequests: 3, WSIncremental: 1}) {
		t.Fatalf("stats = %+v", got)
	}
}

func TestWSCoreErrorFrames(t *testing.T) {
	for _, tc := range []struct {
		frame   string
		status  int
		after   string
		afterMS string
		stale   bool
	}{
		{`{"status":400,"headers":{"retry-after":2,"rEtRy-AfTeR-Ms":"150","other":"discard"},"error":{"code":"previous_response_not_found","headers":{"Retry-After":3}}}`, 400, "2", "150", true},
		{`{"status":429,"headers":[0,1],"error":{"headers":{"Retry-After":false,"Retry-After-Ms":250.5}}}`, 429, "", "250.5", false},
		{`{"status":{"unexpected":4},"headers":{"Retry-After":null,"Retry-After-Ms":["8"]},"error":{"headers":true}}`, 0, "", "", false},
		{`{"status":400,"headers":null,"error":{"param":"previous_response_id","headers":{"Retry-After":"60"}}}`, 400, "60", "", true},
		{`{"status_code":400,"error":{"code":"previous_response_not_found","message":"gone"}}`, 400, "", "", true},
		{`{"error":{"code":"previous_response_not_found","message":"gone"}}`, 0, "", "", true},
	} {
		var w wireEvent
		if err := json.Unmarshal([]byte(tc.frame), &w); err != nil {
			t.Fatal(err)
		}
		e := w.streamError(RouteKey, true)
		if e.StatusCode != tc.status || e.Header.Get("Retry-After") != tc.after || e.Header.Get("Retry-After-Ms") != tc.afterMS ||
			e.Header.Get("Other") != "" || isStalePreviousResponse(e, true) != tc.stale || isStalePreviousResponse(e, false) {
			t.Fatalf("error fields = %+v, retry headers = %v for %s", *e, e.Header, tc.frame)
		}
	}
}

func TestWSCoreWireNormalization(t *testing.T) {
	req := wsTestRequest()
	req.Instructions = "system <prompt>"
	req.ParallelToolCalls = new(bool)
	req.MaxOutputTokens = 500
	req.CacheKey = "session"
	req.Input = append(req.Input, FunctionCall("call", "lookup", "{}"), AssistantMessagePhase("answer", "final_answer"))
	for _, route := range []Route{RouteKey, RouteChatGPT} {
		built, err := buildWireRequest(req, route, true)
		if err != nil {
			t.Fatal(err)
		}
		wsJSON, err := encode((&wsConnState{}).request(built, route))
		if err != nil {
			t.Fatal(err)
		}
		sseJSON, err := req.bodyWith(route, true)
		if err != nil {
			t.Fatal(err)
		}
		ws, sse := topLevel(t, wsJSON), topLevel(t, sseJSON)
		if string(ws["type"]) != `"response.create"` {
			t.Fatalf("type = %s", ws["type"])
		}
		delete(ws, "type")
		if !reflect.DeepEqual(ws, sse) {
			t.Fatalf("WS = %s, SSE = %s", wsJSON, sseJSON)
		}
	}
}

func TestWSCoreTransportRouting(t *testing.T) {
	for _, tc := range []struct {
		name                string
		mode                TransportMode
		eligible, stateless bool
	}{
		{name: "SSE zero value", eligible: true},
		{name: "ineligible", mode: TransportWebSocket},
		{name: "stateless", mode: TransportAuto, eligible: true, stateless: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := wsTestClient(t, "https://api.openai.com/v1", tc.mode, func(cfg *Config) {
				cfg.WebSocketEligible = tc.eligible
				cfg.HTTPClient = &http.Client{Transport: wsRoundTripper(func(r *http.Request) (*http.Response, error) {
					if r.Method != "POST" {
						return nil, fmt.Errorf("unexpected method %s", r.Method)
					}
					return &http.Response{
						StatusCode: 200, Header: make(http.Header),
						Body: io.NopCloser(strings.NewReader("data: " + completed(1, 0, 1, 0) + "\n\n")),
					}, nil
				})}
			})
			req := wsTestRequest()
			req.Stateless = tc.stateless
			// These paths bypass the WebSocket lane even while its lock is held.
			c.wsMu.Lock()
			defer c.wsMu.Unlock()
			runWSTurn(t, c, req)
			if got := c.Stats(); got != (Stats{SSERequests: 1}) {
				t.Fatalf("stats = %+v", got)
			}
		})
	}
}

func TestWSCoreInternalDialTimeout(t *testing.T) {
	c := wsTestClient(t, "https://api.openai.com/v1", TransportWebSocket, func(cfg *Config) {
		cfg.HTTPClient = &http.Client{Timeout: time.Nanosecond, Transport: wsRoundTripper(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}
	})
	if c.wsHTTP.Timeout != 0 || c.http.Timeout != time.Nanosecond {
		t.Fatalf("HTTP timeout = %s, WS timeout = %s", c.http.Timeout, c.wsHTTP.Timeout)
	}
	c.dialTimeout = time.Millisecond
	_, err := c.Stream(wsTestContext(t), wsTestRequest())
	var timeout wsTimeoutError
	if !errors.As(err, &timeout) || !timeout.Timeout() || timeout.Temporary() ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrIncomplete) {
		t.Fatalf("error = %T %v", err, err)
	}
}

func TestWSTrailingUndoneItemResetsChain(t *testing.T) {
	srv := newWSTestServer(t, wsScript(t, func(p *wsPeer, _ int) {
		p.expect("", 1, "")
		// Index 0 finishes, index 1 starts but never receives output_item.done.
		p.send(
			created(),
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_0"}}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_0","content":[{"type":"output_text","text":"hi"}]}}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1"}}`,
			completed(1, 0, 1, 0),
		)
		// Next turn must NOT chain (index 1 was never completed).
		p.expect("", 3, "")
		p.send(created(), completed(1, 0, 1, 0))
		p.waitForClose()
	}))
	c := wsTestClient(t, srv.url, TransportWebSocket)
	req := wsTestRequest()
	runWSTurn(t, c, req)
	req.Input = append(req.Input, AssistantMessage("hi"), UserMessage(InputText("next")))
	runWSTurn(t, c, req)
	if got := c.Stats(); got != (Stats{WSDials: 1, WSRequests: 2, WSIncremental: 0}) {
		t.Fatalf("stats = %+v", got)
	}
}

func TestWSAutoExhausted401DoesNotDoubleRenewOrStick(t *testing.T) {
	creds := new(wsTestCredentials)
	srv := newWSTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"bad token"}}`)
	})
	c := wsTestClient(t, srv.url, TransportAuto, wsChatGPT(creds))
	_, err := c.Stream(wsTestContext(t), wsTestRequest())
	var re *Error
	if !errors.As(err, &re) || re.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want 401 *Error", err)
	}
	if creds.invalid.Load() != 1 || srv.upgrades.Load() != 2 || srv.posts.Load() != 0 || c.isSticky() {
		t.Fatalf("invalid=%d upgrades=%d posts=%d sticky=%v", creds.invalid.Load(), srv.upgrades.Load(), srv.posts.Load(), c.isSticky())
	}
}

func TestWSAutoOversizeKeepsHealthySocket(t *testing.T) {
	srv := newWSTestServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: "+completed(1, 0, 1, 0)+"\n\n")
			return
		}
		w.Header().Set("X-Codex-Websocket-Max-Message-Bytes", "1024")
		wsScript(t, func(p *wsPeer, _ int) {
			p.expect("", 1, "")
			p.send(created(), completed(1, 0, 1, 0))
			// Third turn reuses the same healthy socket after turn 2 used SSE for oversize.
			p.expect("resp_1", 1, "")
			p.send(created(), completed(1, 0, 1, 0))
			p.waitForClose()
		})(w, r, n)
	})
	c := wsTestClient(t, srv.url, TransportAuto)
	req := wsTestRequest()
	runWSTurn(t, c, req)
	big := wsTestRequest()
	big.Input = []Item{UserMessage(InputText(strings.Repeat("a", 2048)))}
	runWSTurn(t, c, big)
	req.Input = append(req.Input, UserMessage(InputText("small again")))
	runWSTurn(t, c, req)
	if got := c.Stats(); got != (Stats{WSDials: 1, WSRequests: 2, WSIncremental: 1, SSERequests: 1, SSEFallbacks: 1}) {
		t.Fatalf("stats = %+v", got)
	}
}

func TestWSAutoBadFirstFrameFallsBackAndSticks(t *testing.T) {
	srv := newWSTestServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: "+completed(1, 0, 1, 0)+"\n\n")
			return
		}
		wsScript(t, func(p *wsPeer, _ int) {
			p.expect("", 1, "")
			_ = p.conn.Write(p.ctx, websocket.MessageBinary, []byte{0xff, 0x00})
			p.waitForClose()
		})(w, r, n)
	})
	c := wsTestClient(t, srv.url, TransportAuto)
	req := wsTestRequest()
	runWSTurn(t, c, req)
	runWSTurn(t, c, req)
	runWSTurn(t, c, req)
	if !c.isSticky() || srv.upgrades.Load() != 2 || c.Stats().SSERequests != 3 || c.Stats().SSEFallbacks != 3 {
		t.Fatalf("sticky=%v upgrades=%d stats=%+v", c.isSticky(), srv.upgrades.Load(), c.Stats())
	}
}

func TestWSSharedStickyDropsIdleSocket(t *testing.T) {
	sticky := NewStickyRegistry()
	srv := newWSTestServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: "+completed(1, 0, 1, 0)+"\n\n")
			return
		}
		if n == 2 {
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		wsScript(t, func(p *wsPeer, _ int) {
			p.expect("", 1, "")
			p.send(created(), completed(1, 0, 1, 0))
			p.waitForClose()
		})(w, r, n)
	})
	c1 := wsTestClient(t, srv.url, TransportAuto, func(cfg *Config) { cfg.Sticky = sticky })
	c2 := wsTestClient(t, srv.url, TransportAuto, func(cfg *Config) { cfg.Sticky = sticky })
	req := wsTestRequest()
	runWSTurn(t, c1, req) // c1 now holds an idle socket
	runWSTurn(t, c2, req) // c2 hits 426 and marks shared registry sticky
	runWSTurn(t, c1, req) // c1 sees sticky, uses SSE, and drops its idle socket
	c1.wsMu.Lock()
	idle := c1.ws
	c1.wsMu.Unlock()
	if idle != nil {
		t.Fatal("c1 kept idle WS socket after shared registry became sticky")
	}
}
