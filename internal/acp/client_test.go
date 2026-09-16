package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	fakeOnce sync.Once
	fakeBin  string
	fakeErr  error
)

func fakeAgentPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("CRAZE_FAKE_AGENT_BIN"); p != "" {
		return p
	}
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "craze-fake-agent-")
		if err != nil {
			fakeErr = err
			return
		}
		fakeBin = filepath.Join(dir, "craze-fake-agent")
		cmd := exec.Command("go", "build", "-o", fakeBin, "github.com/charliek/craze/cmd/craze-fake-agent")
		out, err := cmd.CombinedOutput()
		if err != nil {
			fakeErr = err
			fakeBin = ""
			tlog := string(out)
			fakeErr = errWithOutput(err, tlog)
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return fakeBin
}

func errWithOutput(err error, out string) error {
	if out == "" {
		return err
	}
	return &buildError{err: err, out: out}
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string {
	return e.err.Error() + "\n" + e.out
}

func (e *buildError) Unwrap() error { return e.err }

func spawnScript(t *testing.T, script string) *Client {
	t.Helper()
	opts := SpawnOptions{
		Binary: fakeAgentPath(t),
		Args:   []string{"-script=" + script, "--force", "acp"},
		Stderr: io.Discard,
	}
	if strings.HasPrefix(script, "grok-") {
		opts.Dialect = DialectGrok
	}
	c, err := Spawn(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func handshake(t *testing.T, c *Client) {
	t.Helper()
	ctx := t.Context()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Authenticate(ctx, AuthCursorLogin, nil); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if _, err := c.NewSession(ctx, cwd); err != nil {
		t.Fatal(err)
	}
}

type updateLog struct {
	mu   sync.Mutex
	list []SessionUpdate
}

func (u *updateLog) add(n SessionNotification) {
	var upd SessionUpdate
	_ = json.Unmarshal(n.Update, &upd)
	u.mu.Lock()
	u.list = append(u.list, upd)
	u.mu.Unlock()
}

func (u *updateLog) snapshot() []SessionUpdate {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]SessionUpdate, len(u.list))
	copy(out, u.list)
	return out
}

func attachUpdates(c *Client) *updateLog {
	u := &updateLog{}
	c.SetUpdateHandler(u.add)
	return u
}

func textFrom(updates []SessionUpdate) string {
	var b stringsBuilder
	for _, u := range updates {
		if u.SessionUpdate == UpdateAgentMessage && u.Content != nil {
			b.WriteString(u.Content.Text)
		}
	}
	return b.String()
}

type stringsBuilder struct{ b []byte }

func (s *stringsBuilder) WriteString(v string) { s.b = append(s.b, v...) }
func (s *stringsBuilder) String() string       { return string(s.b) }

func TestPromptAndStreamChunks(t *testing.T) {
	c := spawnScript(t, "echo")
	up := attachUpdates(c)
	handshake(t, c)
	res, err := c.Prompt(t.Context(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != StopEndTurn {
		t.Fatalf("stopReason %q", res.StopReason)
	}
	got := textFrom(up.snapshot())
	if got != "echo: hello" {
		t.Fatalf("chunks %q", got)
	}
	if !containsUpdate(up.snapshot(), UpdateAgentMessage) {
		t.Fatal("expected streamed agent_message_chunk before prompt return")
	}
}

func containsUpdate(updates []SessionUpdate, kind string) bool {
	for _, u := range updates {
		if u.SessionUpdate == kind {
			return true
		}
	}
	return false
}

func TestFollowUp(t *testing.T) {
	c := spawnScript(t, "followup")
	up := attachUpdates(c)
	handshake(t, c)
	if _, err := c.Prompt(t.Context(), "one"); err != nil {
		t.Fatal(err)
	}
	first := textFrom(up.snapshot())
	if first != "first reply" {
		t.Fatalf("first %q", first)
	}
	if _, err := c.Prompt(t.Context(), "two"); err != nil {
		t.Fatal(err)
	}
	all := textFrom(up.snapshot())
	if all != "first replysecond reply" {
		t.Fatalf("follow-up %q", all)
	}
}

func TestToolUpdate(t *testing.T) {
	c := spawnScript(t, "tool")
	up := attachUpdates(c)
	handshake(t, c)
	if _, err := c.Prompt(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range up.snapshot() {
		if u.SessionUpdate == UpdateToolCall && u.ToolCallID == "call-1" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing tool_call")
	}
}

func TestAuthFail(t *testing.T) {
	c := spawnScript(t, "authfail")
	if _, err := c.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	err := c.Authenticate(t.Context(), AuthCursorLogin, nil)
	if err == nil {
		t.Fatal("expected authenticate error")
	}
	rpc, ok := err.(*RPCError)
	if !ok {
		t.Fatalf("got %T %v", err, err)
	}
	if rpc.Message == "" {
		t.Fatal("empty error message")
	}
}

func TestPermissionAllowAndReject(t *testing.T) {
	t.Run("allow", func(t *testing.T) {
		c := spawnScript(t, "permission")
		up := attachUpdates(c)
		var used string
		c.SetPermissionHandler(func(_ int, req PermissionRequest) PermissionDecision {
			id, ok := PickKind(req.Options, KindAllowOnce)
			if !ok {
				t.Error("no allow_once in request")
				return PermissionDecision{Cancelled: true}
			}
			used = id
			return PermissionDecision{OptionID: id}
		})
		handshake(t, c)
		res, err := c.Prompt(t.Context(), "need it")
		if err != nil {
			t.Fatal(err)
		}
		if res.StopReason != StopEndTurn {
			t.Fatalf("stopReason %q", res.StopReason)
		}
		if used != "opt-once" {
			t.Fatalf("used %q, want request optionId opt-once", used)
		}
		if textFrom(up.snapshot()) != "decision:opt-once" {
			t.Fatalf("text %q", textFrom(up.snapshot()))
		}
	})

	t.Run("reject", func(t *testing.T) {
		c := spawnScript(t, "permission")
		up := attachUpdates(c)
		c.SetPermissionHandler(func(_ int, req PermissionRequest) PermissionDecision {
			id, ok := PickKind(req.Options, KindRejectOnce)
			if !ok {
				t.Error("no reject_once")
				return PermissionDecision{Cancelled: true}
			}
			return PermissionDecision{OptionID: id}
		})
		handshake(t, c)
		res, err := c.Prompt(t.Context(), "need it")
		if err != nil {
			t.Fatal(err)
		}
		if res.StopReason != StopEndTurn {
			t.Fatalf("stopReason %q", res.StopReason)
		}
		if textFrom(up.snapshot()) != "decision:opt-reject" {
			t.Fatalf("text %q", textFrom(up.snapshot()))
		}
	})
}

func TestPermissionNeverInventsOptionID(t *testing.T) {
	c := spawnScript(t, "permission")
	c.SetPermissionHandler(func(int, PermissionRequest) PermissionDecision {
		return PermissionDecision{OptionID: "invented-not-in-request"}
	})
	handshake(t, c)
	res, err := c.Prompt(t.Context(), "need it")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != StopCancelled {
		t.Fatalf("invented optionId should be treated as cancelled, got %q", res.StopReason)
	}
}

func TestCancelSemantics(t *testing.T) {
	t.Run("cancel is notification", func(t *testing.T) {
		var buf bytes.Buffer
		enc := NewEncoder(&buf)
		if err := enc.WriteMessage(&Message{
			JSONRPC: jsonrpcVersion,
			Method:  MethodSessionCancel,
			Params:  json.RawMessage(`{"sessionId":"s1"}`),
		}); err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["id"]; ok {
			t.Fatalf("session/cancel must not include id: %s", buf.Bytes())
		}
		if string(raw["method"]) != `"session/cancel"` {
			t.Fatalf("method %s", raw["method"])
		}
	})

	t.Run("hang unblocks", func(t *testing.T) {
		c := spawnScript(t, "hang")
		handshake(t, c)
		errCh := make(chan error, 1)
		var res *PromptResult
		go func() {
			var err error
			res, err = c.Prompt(t.Context(), "wait")
			errCh <- err
		}()
		waitUntil(t, c.PromptInFlight)
		if err := c.Cancel(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatal(err)
			}
			if res.StopReason != StopCancelled {
				t.Fatalf("stopReason %q", res.StopReason)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("prompt did not return after cancel")
		}
	})
}

func TestChildCrash(t *testing.T) {
	c := spawnScript(t, "echo")
	handshake(t, c)
	if c.child == nil || c.child.cmd.Process == nil {
		t.Fatal("missing child")
	}
	if err := c.child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, err := c.Prompt(t.Context(), "hello")
	if err == nil {
		t.Fatal("expected error after child crash")
	}
}

func TestUnknownRequestMethodNotFound(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	t.Cleanup(func() {
		_ = clientR.Close()
		_ = clientW.Close()
		_ = serverR.Close()
		_ = serverW.Close()
	})
	client := Dial(clientR, clientW)
	t.Cleanup(func() { _ = client.Close() })

	enc := NewEncoder(serverW)
	dec := NewDecoder(serverR)
	id, _ := json.Marshal(99)
	if err := enc.WriteMessage(&Message{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Method:  "no/such/method",
		Params:  json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	msg, err := dec.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !msg.IsResponse() || msg.Error == nil || msg.Error.Code != CodeMethodNotFound {
		t.Fatalf("got %+v", msg)
	}
}

func TestUnknownNotificationIgnored(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	t.Cleanup(func() {
		_ = clientR.Close()
		_ = clientW.Close()
		_ = serverR.Close()
		_ = serverW.Close()
	})
	client := Dial(clientR, clientW)
	t.Cleanup(func() { _ = client.Close() })

	enc := NewEncoder(serverW)
	if err := enc.WriteMessage(&Message{
		JSONRPC: jsonrpcVersion,
		Method:  "cursor/generate_image",
		Params:  json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		dec := NewDecoder(serverR)
		_, _ = dec.ReadMessage()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("unexpected response to unknown notification")
	case <-time.After(80 * time.Millisecond):
	}
}

func TestPermissionRepliesUseRequestOptionID(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	t.Cleanup(func() {
		_ = clientR.Close()
		_ = clientW.Close()
		_ = serverR.Close()
		_ = serverW.Close()
	})
	client := Dial(clientR, clientW)
	t.Cleanup(func() { _ = client.Close() })

	srv := NewConn(serverR, serverW)
	var gotOption string
	srv.SetRequestHandler(func(msg *Message) {
		switch msg.Method {
		case MethodInitialize:
			_ = srv.Reply(msg.ID, map[string]any{"protocolVersion": 1})
		case MethodAuthenticate:
			_ = srv.Reply(msg.ID, map[string]any{})
		case MethodSessionNew:
			_ = srv.Reply(msg.ID, map[string]any{"sessionId": "s1"})
		case MethodSessionPrompt:
			go func() {
				params := PermissionRequest{
					SessionID: "s1",
					ToolCall:  ToolCall{ToolCallID: "t1", Title: "Shell"},
					Options: []PermissionOption{
						{OptionID: "yes-this-time", Name: "Yes", Kind: KindAllowOnce},
						{OptionID: "no-thanks", Name: "No", Kind: KindRejectOnce},
					},
				}
				var result struct {
					Outcome struct {
						Outcome  string `json:"outcome"`
						OptionID string `json:"optionId"`
					} `json:"outcome"`
				}
				if err := srv.Call(context.Background(), MethodRequestPermission, params, &result); err != nil {
					t.Errorf("permission call: %v", err)
				}
				gotOption = result.Outcome.OptionID
				_ = srv.Reply(msg.ID, map[string]any{"stopReason": StopEndTurn})
			}()
		default:
			_ = srv.ReplyErr(msg.ID, MethodNotFound(msg.Method))
		}
	})
	srv.Start()

	client.SetPermissionHandler(func(_ int, req PermissionRequest) PermissionDecision {
		id, ok := PickYoloAllow(req.Options)
		if !ok {
			return PermissionDecision{Cancelled: true}
		}
		return PermissionDecision{OptionID: id}
	})
	ctx := t.Context()
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Authenticate(ctx, AuthCursorLogin, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.NewSession(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Prompt(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	if gotOption != "yes-this-time" {
		t.Fatalf("optionId %q, want yes-this-time (from the request)", gotOption)
	}
}

func waitUntil(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting")
}

func TestNumericJSONRPCIds(t *testing.T) {
	agentR, clientW := io.Pipe()
	clientR, agentW := io.Pipe()
	t.Cleanup(func() {
		_ = clientR.Close()
		_ = clientW.Close()
		_ = agentR.Close()
		_ = agentW.Close()
	})
	client := Dial(clientR, clientW)
	t.Cleanup(func() { _ = client.Close() })

	got := make(chan *Message, 1)
	go func() {
		dec := NewDecoder(agentR)
		msg, err := dec.ReadMessage()
		if err != nil {
			got <- nil
			return
		}
		got <- msg
		_ = NewEncoder(agentW).WriteMessage(&Message{
			JSONRPC: jsonrpcVersion,
			ID:      msg.ID,
			Result:  json.RawMessage(`{"protocolVersion":1,"authMethods":[]}`),
		})
	}()

	if _, err := client.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	msg := <-got
	if msg == nil {
		t.Fatal("failed to read initialize")
	}
	if string(msg.ID) != "1" {
		t.Fatalf("initialize id %s, want numeric 1", msg.ID)
	}
}

func TestOffersCursorLogin(t *testing.T) {
	if (InitializeResult{}).OffersCursorLogin() {
		t.Fatal("empty authMethods should skip login")
	}
	if !(InitializeResult{AuthMethods: []AuthMethod{{ID: AuthCursorLogin}}}).OffersCursorLogin() {
		t.Fatal("cursor_login should be detected")
	}
}

func TestSetConfigJSONShape(t *testing.T) {
	b, err := json.Marshal(SetConfigParams{
		SessionID: "s1",
		ConfigID:  "effort",
		Value:     "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["type"]; ok {
		t.Fatalf("must not include type: %s", b)
	}
	if m["sessionId"] != "s1" || m["configId"] != "effort" {
		t.Fatalf("%s", b)
	}
	if _, ok := m["value"].(string); !ok || m["value"] != "high" {
		t.Fatalf("value must be string: %s", b)
	}

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	t.Cleanup(func() {
		_ = clientR.Close()
		_ = clientW.Close()
		_ = serverR.Close()
		_ = serverW.Close()
	})
	client := Dial(clientR, clientW)
	t.Cleanup(func() { _ = client.Close() })

	srv := NewConn(serverR, serverW)
	got := make(chan json.RawMessage, 1)
	srv.SetRequestHandler(func(msg *Message) {
		switch msg.Method {
		case MethodInitialize:
			_ = srv.Reply(msg.ID, map[string]any{"protocolVersion": 1})
		case MethodAuthenticate:
			_ = srv.Reply(msg.ID, map[string]any{})
		case MethodSessionNew:
			_ = srv.Reply(msg.ID, map[string]any{"sessionId": "s1"})
		case MethodSessionSetConfig:
			got <- append(json.RawMessage(nil), msg.Params...)
			_ = srv.Reply(msg.ID, map[string]any{})
		default:
			_ = srv.ReplyErr(msg.ID, MethodNotFound(msg.Method))
		}
	})
	srv.Start()

	ctx := t.Context()
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Authenticate(ctx, AuthCursorLogin, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.NewSession(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := client.SetConfig(ctx, "effort", "high"); err != nil {
		t.Fatal(err)
	}
	params := <-got
	if err := json.Unmarshal(params, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["type"]; ok {
		t.Fatalf("wire must not include type: %s", params)
	}
	if _, ok := m["value"].(string); !ok || m["value"] != "high" {
		t.Fatalf("wire value must be string: %s", params)
	}
	if m["configId"] != "effort" || m["sessionId"] != "s1" {
		t.Fatalf("wire %s", params)
	}
}

// startPromptBlocks sends a multi-block prompt on its own goroutine and hands
// back the request the client wrote, so a test can read the exact bytes.
func startPromptBlocks(t *testing.T, p *rawPipe, blocks []ContentBlock, accepted func()) (<-chan struct {
	res *PromptResult
	err error
}, *Message) {
	t.Helper()
	done := make(chan struct {
		res *PromptResult
		err error
	}, 1)
	go func() {
		res, err := p.client.PromptBlocks(context.Background(), blocks, accepted)
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

// TestPromptBlocksSendsEveryBlockInOrder is the wire shape the whole plugin
// expansion rests on: the draft first and craze's own blocks after it, in one
// session/prompt.
func TestPromptBlocksSendsEveryBlockInOrder(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	blocks := []ContentBlock{
		{Type: "text", Text: "/watch-pr 12"},
		{Type: "text", Text: "block one"},
		{Type: "text", Text: "block two"},
	}
	done, req := startPromptBlocks(t, p, blocks, nil)
	want := `{"sessionId":"s1","prompt":[` +
		`{"type":"text","text":"/watch-pr 12"},` +
		`{"type":"text","text":"block one"},` +
		`{"type":"text","text":"block two"}]}`
	if string(req.Params) != want {
		t.Fatalf("params\n got %s\nwant %s", req.Params, want)
	}
	// The correlation text grok reads is block 1 and nothing else.
	p.client.mu.Lock()
	got := p.client.promptText
	p.client.mu.Unlock()
	if got != "/watch-pr 12" {
		t.Fatalf("promptText %q", got)
	}
	replyPrompt(t, p, req.ID, StopEndTurn)
	if out := <-done; out.err != nil {
		t.Fatal(out.err)
	}
}

// TestPromptSendsOneBlock pins that the one-argument wrapper is exactly what it
// always was: one text block, nothing else on the wire.
func TestPromptSendsOneBlock(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	done := make(chan error, 1)
	go func() {
		_, err := p.client.Prompt(context.Background(), "hello")
		done <- err
	}()
	req := p.readWithin(t, 3*time.Second, "session/prompt")
	if want := `{"sessionId":"s1","prompt":[{"type":"text","text":"hello"}]}`; string(req.Params) != want {
		t.Fatalf("params\n got %s\nwant %s", req.Params, want)
	}
	replyPrompt(t, p, req.ID, StopEndTurn)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestPromptBlocksHookRunsBeforeTheRequest pins the ordering the command events
// depend on: the hook has already run when the request reaches the agent, so
// nothing the turn produces can precede what the hook emitted. The two are
// stamped from one counter rather than merely both observed, so an
// implementation that wrote the request first would fail here even if the
// transport happened to hold the bytes.
func TestPromptBlocksHookRunsBeforeTheRequest(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	var order atomic.Int32
	hookAt, reqAt := make(chan int32, 1), make(chan int32, 1)
	reqCh := make(chan *Message, 1)
	go func() {
		msg, err := p.dec.ReadMessage()
		if err != nil {
			return
		}
		reqAt <- order.Add(1)
		reqCh <- msg
	}()
	done := make(chan error, 1)
	go func() {
		blocks := []ContentBlock{{Type: "text", Text: "draft"}, {Type: "text", Text: "block"}}
		_, err := p.client.PromptBlocks(context.Background(), blocks, func() { hookAt <- order.Add(1) })
		done <- err
	}()
	var hook, req int32
	select {
	case hook = <-hookAt:
	case <-time.After(3 * time.Second):
		t.Fatal("the hook never ran")
	}
	select {
	case req = <-reqAt:
	case <-time.After(3 * time.Second):
		t.Fatal("the request never reached the agent")
	}
	if hook >= req {
		t.Fatalf("hook ran %d, request %d: the hook must come first", hook, req)
	}
	replyPrompt(t, p, (<-reqCh).ID, StopEndTurn)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestPromptBlocksCopiesTheCallersBlocks: the hook hands control back to the
// caller for a moment, and a caller that rewrote its slice there must not
// change what goes out — or make promptText disagree with block 1.
func TestPromptBlocksCopiesTheCallersBlocks(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	blocks := []ContentBlock{{Type: "text", Text: "draft"}, {Type: "text", Text: "block"}}
	done := make(chan error, 1)
	go func() {
		_, err := p.client.PromptBlocks(context.Background(), blocks, func() {
			blocks[0] = ContentBlock{Type: "text", Text: "rewritten"}
			blocks[1] = ContentBlock{Type: "text", Text: "rewritten too"}
		})
		done <- err
	}()
	req := p.readWithin(t, 3*time.Second, "session/prompt")
	var params PromptParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatal(err)
	}
	if len(params.Prompt) != 2 || params.Prompt[0].Text != "draft" || params.Prompt[1].Text != "block" {
		t.Fatalf("sent %+v, want the blocks as they were passed", params.Prompt)
	}
	replyPrompt(t, p, req.ID, StopEndTurn)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestPromptBlocksRefusesAnEmptyPrompt: nothing to say is not a turn, and
// opening one would fire the hook and leave the session in flight over a
// request carrying "prompt": null.
func TestPromptBlocksRefusesAnEmptyPrompt(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	ran := false
	if _, err := p.client.PromptBlocks(context.Background(), nil, func() { ran = true }); err == nil {
		t.Fatal("an empty prompt was accepted")
	}
	if ran {
		t.Fatal("the hook ran on an empty prompt")
	}
	p.client.mu.Lock()
	inFlight := p.client.inPrompt
	p.client.mu.Unlock()
	if inFlight {
		t.Fatal("an empty prompt opened a turn")
	}
}

// TestPromptBlocksHookSkippedOnRefusal: a prompt the client refuses never
// reached the wire, so nothing happened and nothing may be reported.
func TestPromptBlocksHookSkippedOnRefusal(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	ran := false
	hook := func() { ran = true }

	done, req := startPromptBlocks(t, p, []ContentBlock{{Type: "text", Text: "first"}}, nil)
	if _, err := p.client.PromptBlocks(context.Background(), []ContentBlock{{Type: "text", Text: "second"}}, hook); !errors.Is(err, ErrPromptInFlight) {
		t.Fatalf("second prompt: %v", err)
	}
	if ran {
		t.Fatal("the hook ran on ErrPromptInFlight")
	}
	replyPrompt(t, p, req.ID, StopEndTurn)
	if out := <-done; out.err != nil {
		t.Fatal(out.err)
	}

	// A turn the agent started for itself refuses the same way.
	p.client.mu.Lock()
	p.client.foreignID = "interject-fallback-1"
	p.client.mu.Unlock()
	if _, err := p.client.PromptBlocks(context.Background(), []ContentBlock{{Type: "text", Text: "third"}}, hook); !errors.Is(err, ErrForeignTurn) {
		t.Fatalf("prompt during a foreign turn: %v", err)
	}
	if ran {
		t.Fatal("the hook ran on ErrForeignTurn")
	}
}

// --- session/load -------------------------------------------------------

func TestInitializeLoadSessionCapability(t *testing.T) {
	cases := []struct {
		name string
		caps string
		want bool
	}{
		{"true", `{"loadSession":true}`, true},
		{"false", `{"loadSession":false}`, false},
		{"absent", `{"promptCapabilities":{"image":true}}`, false},
		{"empty object", `{}`, false},
		{"no capabilities at all", ``, false},
		{"null", `null`, false},
		{"not an object", `["loadSession"]`, false},
		{"malformed", `{"loadSession":`, false},
		{"wrong type", `{"loadSession":"yes"}`, false},
		{"whitespace", "  \n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := InitializeResult{AgentCapabilities: json.RawMessage(tc.caps)}
			if got := res.LoadSession(); got != tc.want {
				t.Fatalf("LoadSession() = %v for %q, want %v", got, tc.caps, tc.want)
			}
		})
	}
}

// loadServer is a pipe-backed agent whose session/load is the test's own
// function and whose every other method is -32601. It returns the client and
// the agent side of the connection, so a test can write notifications itself.
func loadServer(t *testing.T, onLoad func(srv *Conn, msg *Message)) (*Client, *Conn) {
	t.Helper()
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	t.Cleanup(func() {
		_ = clientR.Close()
		_ = clientW.Close()
		_ = serverR.Close()
		_ = serverW.Close()
	})
	client := Dial(clientR, clientW)
	t.Cleanup(func() { _ = client.Close() })

	srv := NewConn(serverR, serverW)
	srv.SetRequestHandler(func(msg *Message) {
		if msg.Method == MethodSessionLoad {
			onLoad(srv, msg)
			return
		}
		_ = srv.ReplyErr(msg.ID, MethodNotFound(msg.Method))
	})
	srv.Start()
	t.Cleanup(func() { _ = srv.Close() })
	return client, srv
}

func notifyChunk(srv *Conn, sid, text string) {
	_ = srv.Notify(context.Background(), MethodSessionUpdate, SessionNotification{
		SessionID: sid,
		Update:    json.RawMessage(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + text + `"}}`),
	})
}

// recorder collects the text of every update the client routed live.
type recorder struct {
	mu   sync.Mutex
	text []string
}

func (r *recorder) add(n SessionNotification) {
	var upd SessionUpdate
	_ = json.Unmarshal(n.Update, &upd)
	r.mu.Lock()
	if upd.Content != nil {
		r.text = append(r.text, upd.Content.Text)
	}
	r.mu.Unlock()
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.text...)
}

// TestLoadSessionRoutesReplayLive is the §3.3 invariant: the id is installed
// before the call, so a replay notification routes through the live path, and
// the read loop dispatches notifications synchronously before it delivers an
// RPC result, so the notification has reached the update handler by the time
// LoadSession returns. No sleeping, no idle gap.
func TestLoadSessionRoutesReplayLive(t *testing.T) {
	var client *Client
	idDuringCall := make(chan string, 1)
	client, _ = loadServer(t, func(srv *Conn, msg *Message) {
		client.mu.Lock()
		idDuringCall <- client.sessionID
		client.mu.Unlock()
		notifyChunk(srv, "s-load", "replayed")
		_ = srv.Reply(msg.ID, map[string]any{"models": map[string]any{"currentModelId": "m1"}})
	})
	rec := &recorder{}
	client.SetUpdateHandler(rec.add)

	res, err := client.LoadSession(t.Context(), "s-load", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := <-idDuringCall; got != "s-load" {
		t.Fatalf("sessionID during session/load = %q, want it set to s-load before the call", got)
	}
	if res.SessionID != "s-load" {
		t.Fatalf("result sessionId %q, want the id the client asked for", res.SessionID)
	}
	if got := rec.seen(); len(got) != 1 || got[0] != "replayed" {
		t.Fatalf("replay must reach the update handler before LoadSession returns, handler saw %v", got)
	}
	client.mu.Lock()
	pending := len(client.pendingUpdates)
	client.mu.Unlock()
	if pending != 0 {
		t.Fatalf("replay must route live, not buffer: %d pendingUpdates", pending)
	}
}

// TestLoadSessionResetsSessionState: a load resets exactly what NewSession
// resets and discards the pre-session buffer, which can only hold noise from
// before the session existed.
func TestLoadSessionResetsSessionState(t *testing.T) {
	var client *Client
	client, _ = loadServer(t, func(srv *Conn, msg *Message) {
		notifyChunk(srv, "s-load", "replayed")
		_ = srv.Reply(msg.ID, map[string]any{})
	})
	rec := &recorder{}
	client.SetUpdateHandler(rec.add)

	client.mu.Lock()
	client.sessionID = "s-old"
	client.children = map[string]struct{}{"child-1": {}}
	client.childOrder = []string{"child-1"}
	client.foreignID = "prompt-9"
	client.foreignText = "a turn of the agent's own"
	client.foreignSeen = true
	client.interjectSeen = map[string]struct{}{"i-1": {}}
	client.interjectOrder = []string{"i-1"}
	client.pendingUpdates = []SessionNotification{{
		SessionID: "s-load",
		Update:    json.RawMessage(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"pre-session noise"}}`),
	}}
	client.mu.Unlock()

	if _, err := client.LoadSession(t.Context(), "s-load", t.TempDir()); err != nil {
		t.Fatal(err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if client.sessionID != "s-load" {
		t.Fatalf("sessionID %q", client.sessionID)
	}
	if len(client.children) != 0 || len(client.childOrder) != 0 {
		t.Fatalf("child allowlist survived the load: %v %v", client.children, client.childOrder)
	}
	if client.foreignID != "" || client.foreignText != "" || client.foreignSeen {
		t.Fatalf("foreign-turn state survived the load: %q %q %v", client.foreignID, client.foreignText, client.foreignSeen)
	}
	if len(client.interjectSeen) != 0 || len(client.interjectOrder) != 0 {
		t.Fatalf("interjection state survived the load: %v %v", client.interjectSeen, client.interjectOrder)
	}
	if len(client.pendingUpdates) != 0 {
		t.Fatalf("pendingUpdates survived the load: %v", client.pendingUpdates)
	}
	if got := rec.seen(); len(got) != 1 || got[0] != "replayed" {
		t.Fatalf("handler saw %v, want the replay alone — the pre-session buffer is discarded, not flushed", got)
	}
}

// TestLoadSessionErrorClearsSessionID: a refused load leaves no active
// session, so anything still streaming goes back to the pre-session buffer.
// That is only safe because Start closes the client on this error.
func TestLoadSessionErrorClearsSessionID(t *testing.T) {
	var client *Client
	client, srv := loadServer(t, func(srv *Conn, msg *Message) {
		_ = srv.ReplyErr(msg.ID, &RPCError{Code: CodeInvalidParams, Message: "Session not found"})
	})
	rec := &recorder{}
	client.SetUpdateHandler(rec.add)

	_, err := client.LoadSession(t.Context(), "s-load", t.TempDir())
	if err == nil {
		t.Fatal("expected the agent's error")
	}
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != CodeInvalidParams || rpc.Message != "Session not found" {
		t.Fatalf("error %v, want the agent's -32602", err)
	}
	client.mu.Lock()
	sid := client.sessionID
	client.mu.Unlock()
	if sid != "" {
		t.Fatalf("a failed load must clear the session id, got %q", sid)
	}

	// An update arriving after the failure buffers again. The unanswerable
	// request that follows it is the barrier: the read loop is sequential, so
	// a reply to it proves the notification before it was dispatched.
	notifyChunk(srv, "s-load", "still streaming")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := srv.Call(ctx, "probe/unknown", map[string]any{}, nil); err == nil {
		t.Fatal("the barrier request should have been refused")
	}
	client.mu.Lock()
	pending := len(client.pendingUpdates)
	client.mu.Unlock()
	if pending != 1 {
		t.Fatalf("after a failed load an update must buffer again, got %d pendingUpdates", pending)
	}
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("handler saw %v after a failed load, want nothing routed", got)
	}
}

// TestLoadSessionTimesOut drives the script that never answers session/load.
func TestLoadSessionTimesOut(t *testing.T) {
	t.Cleanup(SetLoadSessionTimeout(150 * time.Millisecond))
	c := spawnScript(t, "load-hang")
	res, err := c.Initialize(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !res.LoadSession() {
		t.Fatal("load-hang must advertise loadSession")
	}
	if err := c.Authenticate(t.Context(), AuthCursorLogin, nil); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.LoadSession(t.Context(), "s-load", t.TempDir()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("load error %v, want the deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("load took %s, the deadline did not apply", elapsed)
	}
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid != "" {
		t.Fatalf("a timed-out load must clear the session id, got %q", sid)
	}
}

func TestLoadSessionTimeoutHookRestores(t *testing.T) {
	restore := SetLoadSessionTimeout(5 * time.Millisecond)
	if got := loadSessionDeadline(); got != 5*time.Millisecond {
		t.Fatalf("deadline %s", got)
	}
	restore()
	if got := loadSessionDeadline(); got != loadSessionTimeout {
		t.Fatalf("deadline %s after restore, want %s", got, loadSessionTimeout)
	}
}

func TestLoadSessionCursorScript(t *testing.T) {
	c := spawnScript(t, "load")
	up := attachUpdates(c)
	init, err := c.Initialize(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !init.LoadSession() {
		t.Fatal("the load script must advertise loadSession")
	}
	if err := c.Authenticate(t.Context(), AuthCursorLogin, nil); err != nil {
		t.Fatal(err)
	}
	res, err := c.LoadSession(t.Context(), "s-restored", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Modes) == 0 || len(res.Models) == 0 {
		t.Fatalf("cursor's load result is {modes, models}, got %+v", res)
	}
	if len(res.ConfigOptions) != 0 {
		t.Fatalf("cursor's load result carries no configOptions, got %s", res.ConfigOptions)
	}

	var kinds []string
	var toolIDs []string
	for _, u := range up.snapshot() {
		kinds = append(kinds, u.SessionUpdate)
		if u.ToolCallID != "" {
			toolIDs = append(toolIDs, u.ToolCallID+":"+u.Status)
		}
	}
	want := []string{
		"user_message_chunk", "user_message_chunk", UpdateAgentThought,
		UpdateToolCall, UpdateToolCallUpd, UpdateAgentMessage,
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("replay kinds %v, want %v", kinds, want)
	}
	// Cursor's synthetic replay ids are ordinary tool ids on this path.
	if strings.Join(toolIDs, ",") != "replay-0-1:pending,replay-0-1:completed" {
		t.Fatalf("replay tool ids %v", toolIDs)
	}

	// A prompt after the load is answered on the loaded session.
	if _, err := c.Prompt(t.Context(), "again"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(textFrom(up.snapshot()), "echo: again") {
		t.Fatalf("prompt after a load: %q", textFrom(up.snapshot()))
	}
}

func TestLoadSessionGrokScript(t *testing.T) {
	c := spawnScript(t, "grok-load")
	up := attachUpdates(c)
	var submu sync.Mutex
	var subs []string
	c.SetSubagentHandler(func(n SubagentNotification) {
		submu.Lock()
		subs = append(subs, n.Kind)
		submu.Unlock()
	})
	init, err := c.Initialize(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !init.LoadSession() {
		t.Fatal("grok-load must advertise loadSession")
	}
	if err := c.Authenticate(t.Context(), AuthXAIAPIKey, nil); err != nil {
		t.Fatal(err)
	}
	res, err := c.LoadSession(t.Context(), "s-restored", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// grok's load result: models alone.
	if len(res.Models) == 0 {
		t.Fatal("grok's load result carries models")
	}
	if len(res.Modes) != 0 || len(res.ConfigOptions) != 0 {
		t.Fatalf("grok's load result has no modes and no configOptions, got %s / %s", res.Modes, res.ConfigOptions)
	}
	completedTool := false
	for _, u := range up.snapshot() {
		if u.SessionUpdate == UpdateToolCall && u.Status == "completed" {
			completedTool = true
		}
	}
	if !completedTool {
		t.Fatal("grok replays a tool_call already completed")
	}
	submu.Lock()
	got := strings.Join(subs, ",")
	submu.Unlock()
	if got != SubagentSpawned+","+SubagentFinished {
		t.Fatalf("replayed sub-agent lifecycle %q", got)
	}
}

func TestLoadSessionLongReplay(t *testing.T) {
	c := spawnScript(t, "load-long")
	up := attachUpdates(c)
	if _, err := c.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Authenticate(t.Context(), AuthCursorLogin, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LoadSession(t.Context(), "s-restored", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if n := len(up.snapshot()); n != 600 {
		t.Fatalf("replayed %d updates, want 600 before the result", n)
	}
}

// TestLoadScriptsRefuseSessionNew: every load script errors on session/new, so
// a later test can prove no client fell back to it.
func TestLoadScriptsRefuseSessionNew(t *testing.T) {
	for _, script := range []string{"load", "grok-load", "load-missing", "load-hang", "load-long"} {
		t.Run(script, func(t *testing.T) {
			c := spawnScript(t, script)
			if _, err := c.Initialize(t.Context()); err != nil {
				t.Fatal(err)
			}
			method := AuthCursorLogin
			if strings.HasPrefix(script, "grok-") {
				method = AuthXAIAPIKey
			}
			if err := c.Authenticate(t.Context(), method, nil); err != nil {
				t.Fatal(err)
			}
			_, err := c.NewSession(t.Context(), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), "session/new must not be called") {
				t.Fatalf("session/new error %v", err)
			}
		})
	}
}

func TestLoadSessionMissingScript(t *testing.T) {
	c := spawnScript(t, "load-missing")
	if _, err := c.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Authenticate(t.Context(), AuthCursorLogin, nil); err != nil {
		t.Fatal(err)
	}
	_, err := c.LoadSession(t.Context(), "s-gone", t.TempDir())
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != CodeInvalidParams || rpc.Message != "Session not found" {
		t.Fatalf("load error %v, want -32602 Session not found", err)
	}
}

// TestSessionLoadRefusedWithoutCapability: every other script is an agent
// without the capability and answers -32601, which is the live wire.
func TestSessionLoadRefusedWithoutCapability(t *testing.T) {
	c := spawnScript(t, "echo")
	res, err := c.Initialize(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.LoadSession() {
		t.Fatal("echo must keep loadSession false")
	}
	if err := c.Authenticate(t.Context(), AuthCursorLogin, nil); err != nil {
		t.Fatal(err)
	}
	_, err = c.LoadSession(t.Context(), "s-load", t.TempDir())
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != CodeMethodNotFound {
		t.Fatalf("load error %v, want -32601", err)
	}
}

func TestLoadSessionRejectsEmptyID(t *testing.T) {
	var client *Client
	client, _ = loadServer(t, func(srv *Conn, msg *Message) {
		t.Error("session/load must not reach the wire without an id")
		_ = srv.Reply(msg.ID, map[string]any{})
	})
	if _, err := client.LoadSession(t.Context(), "", t.TempDir()); err == nil {
		t.Fatal("expected an error for an empty session id")
	}
}
