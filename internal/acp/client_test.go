package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
