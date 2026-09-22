package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
		c.SetPermissionHandler(func(_ Arrival, req PermissionRequest) PermissionDecision {
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
		c.SetPermissionHandler(func(_ Arrival, req PermissionRequest) PermissionDecision {
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
	c.SetPermissionHandler(func(Arrival, PermissionRequest) PermissionDecision {
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
		// c.PromptInFlight flips true before c.Prompt ever writes session/prompt
		// (PromptBlocks sets c.inPrompt, then unlocks, then writes the RPC), so
		// waiting on it alone — as this subtest used to — lets Cancel write
		// session/cancel before the fake has read the prompt at all. The fake's
		// session/prompt handler clears its own cancelled flag on purpose the
		// instant it reads a prompt, discarding a cancel that beat it there,
		// and "hang" then waits forever for a second cancel that never comes:
		// the same ordering inversion behind the CI hang in
		// TestCancelWaitsUntilPromptReturns (internal/agent/session_test.go).
		// "hang-ack" sends one chunk the instant it reads the prompt, so
		// waiting for that update is proof of the read rather than a hope
		// about which goroutine the runtime scheduled first.
		c := spawnScript(t, "hang-ack")
		up := attachUpdates(c)
		handshake(t, c)
		errCh := make(chan error, 1)
		var res *PromptResult
		go func() {
			var err error
			res, err = c.Prompt(t.Context(), "wait")
			errCh <- err
		}()
		waitUntil(t, func() bool { return textFrom(up.snapshot()) == "ack: wait" })
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

// TestClientCloseReportsWhetherTheAgentExitedFirst is issue #23's §3.7.2/
// §3.7.3: Close's non-blocking probe of child.waitCh has to tell an agent
// that ended on its own — including the exit-0 case Conn.Err() cannot see,
// since it reads ErrClosed either way — from a close craze asked for, and
// the answer must not flip on a second call, because something already
// calls Close twice (spawnScript's own t.Cleanup, in the third case below).
func TestClientCloseReportsWhetherTheAgentExitedFirst(t *testing.T) {
	bin := fakeAgentPath(t)
	t.Run("exits non-zero on its own", func(t *testing.T) {
		// The unknown-script exit (main.go): the fake never reaches the ACP
		// loop at all.
		c, err := Spawn(SpawnOptions{
			Binary: bin,
			Args:   []string{"-script=nope", "--force", "acp"},
			Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		<-c.child.waitCh // deterministic: the reaper has already published the exit
		first := c.Close()
		if !errors.Is(first, ErrAgentExited) {
			t.Fatalf("Close() = %v, want an error wrapping ErrAgentExited", first)
		}
		if second := c.Close(); second != first {
			t.Fatalf("second Close() = %v, want the same value as the first, %v", second, first)
		}
	})
	t.Run("exits zero on its own", func(t *testing.T) {
		// -h prints usage and exits 0 — the case Conn.Err() alone cannot tell
		// apart from craze's own close, since both read ErrClosed.
		c, err := Spawn(SpawnOptions{
			Binary: bin,
			Args:   []string{"-h"},
			Stderr: io.Discard,
		})
		if err != nil {
			t.Fatal(err)
		}
		<-c.child.waitCh
		first := c.Close()
		if !errors.Is(first, ErrAgentExited) {
			t.Fatalf("Close() = %v, want an error wrapping ErrAgentExited (exit 0 included)", first)
		}
		if second := c.Close(); second != first {
			t.Fatalf("second Close() = %v, want the same value as the first, %v", second, first)
		}
	})
	t.Run("craze closes a normal session", func(t *testing.T) {
		c := spawnScript(t, "echo")
		handshake(t, c)
		first := c.Close()
		if first != nil {
			t.Fatalf("Close() = %v, want nil", first)
		}
		if second := c.Close(); second != first {
			t.Fatalf("second Close() = %v, want the same value as the first, %v", second, first)
		}
		// spawnScript's own t.Cleanup closes a third time: the regression
		// this case exists for. Without the stored result, Child.Shutdown's
		// SIGTERM above would have left waitCh closed by the time cleanup
		// runs, and a naive probe would misreport this craze-initiated close
		// as a self-exit on its third call.
	})
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

	client.SetPermissionHandler(func(_ Arrival, req PermissionRequest) PermissionDecision {
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
	if _, err := client.SetConfig(ctx, "effort", "high"); err != nil {
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

// goPromptBlocks sends a multi-block prompt on its own goroutine and leaves
// the request unread, so a test can hold the write in the pipe.
func goPromptBlocks(p *rawPipe, blocks []ContentBlock, accepted, sent func()) <-chan struct {
	res *PromptResult
	err error
} {
	done := make(chan struct {
		res *PromptResult
		err error
	}, 1)
	go func() {
		res, err := p.client.PromptBlocks(context.Background(), blocks, accepted, sent)
		done <- struct {
			res *PromptResult
			err error
		}{res, err}
	}()
	return done
}

// startPromptBlocks sends a multi-block prompt on its own goroutine and hands
// back the request the client wrote, so a test can read the exact bytes.
func startPromptBlocks(t *testing.T, p *rawPipe, blocks []ContentBlock, accepted, sent func()) (<-chan struct {
	res *PromptResult
	err error
}, *Message) {
	t.Helper()
	done := goPromptBlocks(p, blocks, accepted, sent)
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
	done, req := startPromptBlocks(t, p, blocks, nil, nil)
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
		_, err := p.client.PromptBlocks(context.Background(), blocks, func() { hookAt <- order.Add(1) }, nil)
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
		}, nil)
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
// opening one would fire the hooks and leave the session in flight over a
// request carrying "prompt": null.
func TestPromptBlocksRefusesAnEmptyPrompt(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	ran, sentRan := false, false
	if _, err := p.client.PromptBlocks(context.Background(), nil, func() { ran = true }, func() { sentRan = true }); err == nil {
		t.Fatal("an empty prompt was accepted")
	}
	if ran {
		t.Fatal("the hook ran on an empty prompt")
	}
	if sentRan {
		t.Fatal("sent ran on an empty prompt")
	}
	p.client.mu.Lock()
	inFlight := p.client.inPrompt
	p.client.mu.Unlock()
	if inFlight {
		t.Fatal("an empty prompt opened a turn")
	}
}

// TestPromptBlocksHookSkippedOnRefusal: a prompt the client refuses never
// reached the wire, so nothing happened and nothing may be reported — and
// nothing was sent, so sent never says otherwise.
func TestPromptBlocksHookSkippedOnRefusal(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	ran, sentRan := false, false
	hook := func() { ran = true }
	sent := func() { sentRan = true }

	done, req := startPromptBlocks(t, p, []ContentBlock{{Type: "text", Text: "first"}}, nil, nil)
	if _, err := p.client.PromptBlocks(context.Background(), []ContentBlock{{Type: "text", Text: "second"}}, hook, sent); !errors.Is(err, ErrPromptInFlight) {
		t.Fatalf("second prompt: %v", err)
	}
	if ran {
		t.Fatal("the hook ran on ErrPromptInFlight")
	}
	if sentRan {
		t.Fatal("sent ran on ErrPromptInFlight")
	}
	replyPrompt(t, p, req.ID, StopEndTurn)
	if out := <-done; out.err != nil {
		t.Fatal(out.err)
	}

	// A turn the agent started for itself refuses the same way.
	p.client.mu.Lock()
	p.client.foreignID = "interject-fallback-1"
	p.client.mu.Unlock()
	if _, err := p.client.PromptBlocks(context.Background(), []ContentBlock{{Type: "text", Text: "third"}}, hook, sent); !errors.Is(err, ErrForeignTurn) {
		t.Fatalf("prompt during a foreign turn: %v", err)
	}
	if ran {
		t.Fatal("the hook ran on ErrForeignTurn")
	}
	if sentRan {
		t.Fatal("sent ran on ErrForeignTurn")
	}
}

// stampWriter is the client's end of the pipe, watched from inside Write. It
// says when a session/prompt frame's Write has begun, and when that Write has
// returned it stamps order — on the writing goroutine, before control goes
// back to the encoder. sent stamps the same counter from that same goroutine,
// so the two stamps are ordered by program order rather than by whichever side
// of the pipe the scheduler woke first.
type stampWriter struct {
	w       io.Writer
	order   atomic.Int32
	wrote   atomic.Int32
	entered chan struct{}
}

func (s *stampWriter) Write(b []byte) (int, error) {
	prompt := bytes.Contains(b, []byte(`"method":"session/prompt"`))
	if prompt {
		select {
		case s.entered <- struct{}{}:
		default:
		}
	}
	n, err := s.w.Write(b)
	if prompt && err == nil {
		s.wrote.Store(s.order.Add(1))
	}
	return n, err
}

// TestPromptBlocksSentRunsAfterTheWrite pins the one promise a cancel leans
// on: sent runs only once the session/prompt bytes are out, so anything the
// caller writes after it lands behind the prompt. The pipe is unbuffered, so
// while nothing reads the request its Write physically cannot return — and sent
// must not have run by then. Once the request is read, sent runs, and its stamp
// comes after the write's. Both dialects: cursor writes inline, grok on a
// goroutine of its own, and sent runs on whichever one wrote.
func TestPromptBlocksSentRunsAfterTheWrite(t *testing.T) {
	for _, d := range []DialectID{DialectCursor, DialectGrok} {
		t.Run(string(d), func(t *testing.T) {
			w := &stampWriter{entered: make(chan struct{}, 1)}
			p := newRawPipeWriter(t, d, func(out io.Writer) io.Writer {
				w.w = out
				return w
			})
			// Ahead of the client's own Close: should an assertion fail with
			// the request still held in the pipe, the held write fails
			// instead of keeping Close's cancel waiting behind it.
			t.Cleanup(func() { _ = p.serverR.Close() })
			p.setSession("s1")
			var sentAt atomic.Int32
			fired := make(chan struct{})
			// A second call closes fired twice and panics: sent runs once.
			sent := func() {
				sentAt.Store(w.order.Add(1))
				close(fired)
			}
			done := goPromptBlocks(p, []ContentBlock{{Type: "text", Text: "hi"}}, nil, sent)

			select {
			case <-w.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("the request was never written")
			}
			// The write has begun and nothing has read it, so it cannot have
			// finished: a sent that fires in this window fired before the bytes
			// were out.
			select {
			case <-fired:
				t.Fatal("sent ran while the request was still unread")
			case <-time.After(150 * time.Millisecond):
			}

			req := p.readWithin(t, 3*time.Second, "session/prompt")
			select {
			case <-fired:
			case <-time.After(3 * time.Second):
				t.Fatal("sent never ran after the request was read")
			}
			if wrote, at := w.wrote.Load(), sentAt.Load(); wrote == 0 || at <= wrote {
				t.Fatalf("write stamped %d, sent %d: sent must come after the write returned", wrote, at)
			}

			replyPrompt(t, p, req.ID, StopEndTurn)
			waitPrompt(t, done)
		})
	}
}

// TestPromptBlocksSentSkippedOnAFailedWrite: a request whose write failed never
// reached the agent, so sent must not say it did. Only the client-write
// direction is closed — the agent's end of it — so the connection itself stays
// open and this is the write failing, not the closed-connection check that
// comes before it.
func TestPromptBlocksSentSkippedOnAFailedWrite(t *testing.T) {
	for _, d := range []DialectID{DialectCursor, DialectGrok} {
		t.Run(string(d), func(t *testing.T) {
			p := newRawPipeDialect(t, d)
			p.setSession("s1")
			_ = p.serverR.Close()
			var sentRan atomic.Bool
			_, err := p.client.PromptBlocks(context.Background(), []ContentBlock{{Type: "text", Text: "hi"}}, nil, func() { sentRan.Store(true) })
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("prompt over a closed write direction: %v", err)
			}
			if sentRan.Load() {
				t.Fatal("sent ran on a failed write")
			}
			select {
			case <-p.client.Conn().Done():
				t.Fatal("the connection closed: this was not the write failing")
			default:
			}
		})
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

// waitDone waits for wg, but gives up rather than hanging. An unbounded Wait
// on a WaitGroup a bug never satisfies blocks until the test binary's own
// timeout fires, which kills every other test in the package and reports a
// goroutine dump instead of a failure. The deadline is far longer than any of
// these waits legitimately needs, so it only ever fires on a real bug -- and
// then it names the test and the line.
func waitDone(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the goroutines to finish")
	}
}

// TestCloseSignalsWhatAnExitedAgentLeftBehind: an agent that exits on its own
// can leave something running in its process group — a tool's subprocess —
// and the first Close must still signal that group, or it is orphaned. Here
// the "agent" is a shell that starts a sleep in its own group and exits.
func TestCloseSignalsWhatAnExitedAgentLeftBehind(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "left-behind.pid")
	c, err := Spawn(SpawnOptions{
		Binary: "/bin/sh",
		Args:   []string{"-c", "sleep 60 & echo $! > " + pidFile + "; exit 0"},
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-c.child.waitCh
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the left-behind process was not running before Close: %v", err)
	}

	if err := c.Close(); !errors.Is(err, ErrAgentExited) {
		t.Fatalf("Close = %v, want ErrAgentExited", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("Close left the exited agent's process group running")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCloseKillsWhatIgnoresSIGTERM: the grace is the process group's, not only
// the agent's. A member that ignores SIGTERM outlives an agent that exits on
// it, and must still be killed when the grace runs out. Here the "agent" is a
// shell that starts a sleep ignoring SIGTERM in its own group and exits.
func TestCloseKillsWhatIgnoresSIGTERM(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "stubborn.pid")
	c, err := Spawn(SpawnOptions{
		Binary: "/bin/sh",
		Args:   []string{"-c", "trap '' TERM; sleep 60 & echo $! > " + pidFile + "; exit 0"},
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-c.child.waitCh
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	_ = c.Close()
	deadline := time.Now().Add(shutdownGrace + 3*time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("a process group member that ignores SIGTERM survived Close")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCloseDoesNotWaitOnAWedgedStdin: with a prompt's write stuck in a pipe
// nobody reads — an agent that stopped reading its stdin — Close's own cancel
// cannot be written either, and it must not keep Close from shutting down.
func TestCloseDoesNotWaitOnAWedgedStdin(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	_ = goPromptBlocks(p, []ContentBlock{{Type: "text", Text: "never read"}}, nil, nil)
	waitFor(t, p.client.PromptInFlight, "the prompt to be in flight")

	closed := make(chan error, 1)
	go func() { closed <- p.client.Close() }()
	select {
	case <-closed:
	case <-time.After(closeCancelWait + 5*time.Second):
		// Unwedge before failing, or the cleanup's own Close would wait on
		// this one and hang the package instead of reporting.
		_ = p.serverR.Close()
		t.Fatal("Close waited on a cancel an agent that stopped reading stdin could never take")
	}
}

// TestCloseDoesNotWaitToAnswerOnAWedgedStdin: Close answers every request the
// agent is blocked on before it shuts the agent down, and with nobody reading
// the agent's stdin that answer cannot be written. It must not keep Close
// from shutting down either.
func TestCloseDoesNotWaitToAnswerOnAWedgedStdin(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	asked := make(chan struct{}, 1)
	release := make(chan struct{})
	p.client.SetAskHandler(func(Arrival, AskQuestionRequest) AskDecision {
		asked <- struct{}{}
		<-release
		return AskDecision{Skip: true}
	})
	t.Cleanup(func() { close(release) })
	p.send(t, 1, MethodGrokAskUserQuestion, grokAskParams)
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the ask never reached its handler")
	}

	closed := make(chan error, 1)
	go func() { closed <- p.client.Close() }()
	select {
	case <-closed:
	case <-time.After(closeCancelWait + 5*time.Second):
		// Unwedge before failing, or the cleanup's own Close would wait on
		// this one and hang the package instead of reporting.
		_ = p.serverR.Close()
		t.Fatal("Close waited to answer a request on a stdin nobody reads")
	}
}

// The settings calls' replies (plan 025 design 1). A reply's catalog is read on
// the read goroutine and handed to the settings handler before the reply is
// delivered, so it is ordered against the agent's pushes by the wire; its
// configOptions keeps its presence; and a call whose reply hook ran always
// answers with that reply.

// answerWith writes a successful reply to req carrying result verbatim.
func (p *rawPipe) answerWith(t *testing.T, req *Message, result string) {
	t.Helper()
	if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: req.ID, Result: json.RawMessage(result)}); err != nil {
		t.Fatal(err)
	}
}

// pushUpdate writes a session/update notification for session sid.
func (p *rawPipe) pushUpdate(t *testing.T, sid, update string) {
	t.Helper()
	params := `{"sessionId":"` + sid + `","update":` + update + `}`
	if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, Method: MethodSessionUpdate, Params: json.RawMessage(params)}); err != nil {
		t.Fatal(err)
	}
}

// roundTrip is the wire barrier the settings tests use: one more request, and
// its -32601 written by the test, so everything the test wrote before it has
// been read — and dispatched, because the read loop handles one frame at a
// time — by the time it returns.
func (p *rawPipe) roundTrip(t *testing.T) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.client.Conn().Call(context.Background(), "craze/barrier", nil, nil) }()
	req := p.readWithin(t, 3*time.Second, "the barrier request")
	if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: req.ID, Error: MethodNotFound(req.Method)}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
			t.Fatalf("the barrier answered %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the barrier never came back")
	}
}

type settingsAnswer struct {
	cat ConfigCatalog
	err error
}

// awaitSettings is a settings call's answer, with a watchdog.
func awaitSettings(t *testing.T, done <-chan settingsAnswer) settingsAnswer {
	t.Helper()
	select {
	case a := <-done:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("the call never came back")
		return settingsAnswer{}
	}
}

// TestASettingsReplyKeepsItsCatalogsPresence is panel astra 14 at the wire: a
// reply with no configOptions (or null) carries no catalog, an array — the
// empty one too — is the whole catalog, and anything else is an error that
// installs nothing, for set_config_option and set_model alike (CodeRabbit 15:
// set_model's reply is decoded too).
func TestASettingsReplyKeepsItsCatalogsPresence(t *testing.T) {
	const opts = `[{"id":"fast","name":"Fast","category":"model_config","type":"select","currentValue":"true","options":[]}]`
	cases := []struct {
		name    string
		result  string
		present bool
		options string
		bad     bool
	}{
		{"no field", `{}`, false, "", false},
		{"a null result", `null`, false, "", false},
		{"another field only", `{"_meta":{"x.ai/model":"grok-4.6"}}`, false, "", false},
		{"a null field", `{"configOptions":null}`, false, "", false},
		{"an empty list clears", `{"configOptions":[]}`, true, `[]`, false},
		{"a list", `{"configOptions":` + opts + `}`, true, opts, false},
		{"an object", `{"configOptions":{"fast":"true"}}`, false, "", true},
		{"a string", `{"configOptions":"fast"}`, false, "", true},
		{"a number", `{"configOptions":3}`, false, "", true},
	}
	calls := []struct {
		method string
		call   func(c *Client) (ConfigCatalog, error)
		id     string
		value  string
	}{
		{MethodSessionSetConfig, func(c *Client) (ConfigCatalog, error) {
			return c.SetConfig(context.Background(), "fast", "true")
		}, "fast", "true"},
		{MethodSessionSetModel, func(c *Client) (ConfigCatalog, error) {
			return c.SetModel(context.Background(), "composer-2.5")
		}, "", "composer-2.5"},
	}
	for _, call := range calls {
		for _, tc := range cases {
			t.Run(call.method+"/"+tc.name, func(t *testing.T) {
				p := newRawPipe(t)
				p.setSession("s1")
				var mu sync.Mutex
				var got []SettingsReply
				p.client.SetSettingsHandler(func(r SettingsReply) error {
					mu.Lock()
					got = append(got, r)
					mu.Unlock()
					return nil
				})
				done := make(chan settingsAnswer, 1)
				go func() {
					cat, err := call.call(p.client)
					done <- settingsAnswer{cat, err}
				}()
				req := p.readWithin(t, 3*time.Second, call.method)
				if req.Method != call.method {
					t.Fatalf("the call wrote %q", req.Method)
				}
				p.answerWith(t, req, tc.result)
				a := awaitSettings(t, done)
				mu.Lock()
				defer mu.Unlock()
				if tc.bad {
					if !errors.Is(a.err, ErrBadCatalog) {
						t.Fatalf("a configOptions that is not a list answered (%+v, %v), want ErrBadCatalog", a.cat, a.err)
					}
					if len(got) != 0 {
						t.Fatalf("a malformed reply reached the settings handler: %+v", got)
					}
					return
				}
				if a.err != nil {
					t.Fatalf("the call failed: %v", a.err)
				}
				if a.cat.Present != tc.present || string(a.cat.Options) != tc.options {
					t.Fatalf("the call answered %+v (%s), want present=%v %s", a.cat, a.cat.Options, tc.present, tc.options)
				}
				if len(got) != 1 {
					t.Fatalf("the settings handler ran %d times, want once", len(got))
				}
				r := got[0]
				if r.Method != call.method || r.ConfigID != call.id || r.Value != call.value {
					t.Fatalf("the handler was told %+v", r)
				}
				if r.Catalog.Present != tc.present || string(r.Catalog.Options) != tc.options {
					t.Fatalf("the handler's catalog is %+v (%s)", r.Catalog, r.Catalog.Options)
				}
			})
		}
	}
}

// TestTheSettingsHandlerRunsInWireOrder is panel astra 2 at the ACP boundary: a
// catalog pushed ahead of a settings reply, the reply, and a catalog pushed
// behind it reach their handlers in exactly that order, because the reply's is
// run on the read loop as the reply is read — and the call has not returned
// before it has.
func TestTheSettingsHandlerRunsInWireOrder(t *testing.T) {
	p := newRawPipe(t)
	p.setSession("s1")
	var mu sync.Mutex
	var seen []string
	note := func(s string) {
		mu.Lock()
		seen = append(seen, s)
		mu.Unlock()
	}
	p.client.SetUpdateHandler(func(n SessionNotification) {
		var u struct {
			Tag string `json:"tag"`
		}
		_ = json.Unmarshal(n.Update, &u)
		note("update " + u.Tag)
	})
	p.client.SetSettingsHandler(func(r SettingsReply) error { note("reply " + r.ConfigID); return nil })
	done := make(chan settingsAnswer, 1)
	go func() {
		cat, err := p.client.SetConfig(context.Background(), "model", "composer-2.5")
		// Recorded on the caller's goroutine the moment the call returns.
		note("returned")
		done <- settingsAnswer{cat, err}
	}()
	req := p.readWithin(t, 3*time.Second, "set_config_option")
	p.pushUpdate(t, "s1", `{"sessionUpdate":"config_option_update","tag":"before","configOptions":[]}`)
	p.answerWith(t, req, `{"configOptions":[]}`)
	if a := awaitSettings(t, done); a.err != nil || !a.cat.Present {
		t.Fatalf("the call answered (%+v, %v)", a.cat, a.err)
	}
	p.pushUpdate(t, "s1", `{"sessionUpdate":"config_option_update","tag":"after","configOptions":[]}`)
	p.roundTrip(t)
	mu.Lock()
	defer mu.Unlock()
	// The first two are fixed by the wire, and the reply's handler has run
	// before the call returned. Only "returned" and the push behind the reply
	// may land either way round: once the reply is handed over, the caller's
	// goroutine and the read loop race — the race a setter's section tolerates
	// by reading the snapshot as it finds it, never the reply.
	if len(seen) != 4 || seen[0] != "update before" || seen[1] != "reply model" {
		t.Fatalf("the handlers ran %q, want the push ahead of the reply, then the reply, then the rest", seen)
	}
	if rest := strings.Join(seen[2:], ","); rest != "returned,update after" && rest != "update after,returned" {
		t.Fatalf("the handlers ran %q", seen)
	}
}

// TestACallWhoseReplyHookRanAnswersWithTheReply is rule R1 (plan 025 C1): once
// deliver has taken a request's reply — and so may have run its hook, which
// installs a catalog — the call returns that reply, never its context's error
// and never the connection's close. Before, callRaw's select could pick either
// with the reply already buffered, and a setter would then report as failed a
// change the read loop had already installed, owing a delta it never wrote.
//
// The schedule is forced from inside it: the hook is held after the reply was
// taken, the call is made to give up — its context ended, or the connection
// closed — and takenWait says the call has found its request already taken
// before the hook is let go.
func TestACallWhoseReplyHookRanAnswersWithTheReply(t *testing.T) {
	for _, tc := range []struct {
		name   string
		giveUp func(cancel context.CancelFunc, c *Conn)
	}{
		{"its context ends", func(cancel context.CancelFunc, _ *Conn) { cancel() }},
		{"the connection closes", func(_ context.CancelFunc, c *Conn) { _ = c.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRawPipe(t)
			conn := p.client.Conn()
			waiting := make(chan struct{})
			conn.takenWait = func() { close(waiting) }
			entered, release := make(chan struct{}), make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type answer struct {
				raw json.RawMessage
				err error
			}
			done := make(chan answer, 1)
			go func() {
				raw, err := conn.callReply(ctx, "test/settle", nil, func(json.RawMessage) error {
					close(entered)
					<-release
					return nil
				})
				done <- answer{raw, err}
			}()
			req := p.readWithin(t, 3*time.Second, "the request")
			p.answerWith(t, req, `{"settled":true}`)
			await := func(ch <-chan struct{}, what string) {
				t.Helper()
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatalf("timed out waiting for %s", what)
				}
			}
			await(entered, "the reply hook")
			tc.giveUp(cancel, conn)
			await(waiting, "the call to find its request taken")
			close(release)
			var a answer
			select {
			case a = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the call never came back")
			}
			if a.err != nil || string(a.raw) != `{"settled":true}` {
				t.Fatalf("a call whose reply hook ran answered (%s, %v), want the reply", a.raw, a.err)
			}
		})
	}
}

// TestACallThatGaveUpFirstNeverRunsItsReplyHook is R1's other half: a call
// whose context ended while its request was still pending answers with that
// error, and the reply that comes later is dropped whole — its hook never runs,
// so nothing is installed that nobody will announce.
func TestACallThatGaveUpFirstNeverRunsItsReplyHook(t *testing.T) {
	p := newRawPipe(t)
	conn := p.client.Conn()
	var ran atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := conn.callReply(ctx, "test/settle", nil, func(json.RawMessage) error {
			ran.Store(true)
			return nil
		})
		done <- err
	}()
	req := p.readWithin(t, 3*time.Second, "the request")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a call that gave up with its request pending answered %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call never came back")
	}
	p.answerWith(t, req, `{"settled":true}`)
	p.roundTrip(t)
	if ran.Load() {
		t.Fatal("the reply hook ran for a call that had already given up")
	}
}

// TestASettingsReplyCarriesItsCallsWord is astra r2 items 4 and 7 at the wire.
// Item 4: whether a reply answers a model change is what the CALL said when it
// was made — set_config_option is a model change when it goes out through
// SetModelOption and not through SetConfig, and set_model always is — so the
// handler never has to work it out from the catalogs it finds. SetModelOption
// is SetConfig on the wire, byte for byte. Item 7: the handler may refuse the
// reply, and its error is then the call's answer, wrapped with the method — the
// way a member that is not an option, which only internal/agent's parser can
// judge, becomes an error that installs nothing, like a configOptions that is
// not a list at all (TestASettingsReplyKeepsItsCatalogsPresence).
func TestASettingsReplyCarriesItsCallsWord(t *testing.T) {
	refusal := fmt.Errorf("%w: member 0 is not an option", ErrBadCatalog)
	for _, call := range []struct {
		name, method, id, value string
		model                   bool
		call                    func(c *Client) (ConfigCatalog, error)
	}{
		{"SetConfig", MethodSessionSetConfig, "fast", "true", false, func(c *Client) (ConfigCatalog, error) {
			return c.SetConfig(context.Background(), "fast", "true")
		}},
		{"SetModelOption", MethodSessionSetConfig, "model", "composer-2.5", true, func(c *Client) (ConfigCatalog, error) {
			return c.SetModelOption(context.Background(), "model", "composer-2.5")
		}},
		{"SetModel", MethodSessionSetModel, "", "composer-2.5", true, func(c *Client) (ConfigCatalog, error) {
			return c.SetModel(context.Background(), "composer-2.5")
		}},
	} {
		for _, refuse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/refused=%v", call.name, refuse), func(t *testing.T) {
				p := newRawPipe(t)
				p.setSession("s1")
				var mu sync.Mutex
				var got []SettingsReply
				p.client.SetSettingsHandler(func(r SettingsReply) error {
					mu.Lock()
					got = append(got, r)
					mu.Unlock()
					if refuse {
						return refusal
					}
					return nil
				})
				done := make(chan settingsAnswer, 1)
				go func() {
					cat, err := call.call(p.client)
					done <- settingsAnswer{cat, err}
				}()
				req := p.readWithin(t, 3*time.Second, call.method)
				if req.Method != call.method {
					t.Fatalf("the call wrote %q", req.Method)
				}
				if call.method == MethodSessionSetConfig {
					var params SetConfigParams
					if err := json.Unmarshal(req.Params, &params); err != nil || params.SessionID != "s1" ||
						params.ConfigID != call.id || params.Value != call.value {
						t.Fatalf("the call wrote %s", req.Params)
					}
				}
				p.answerWith(t, req, `{"configOptions":[]}`)
				a := awaitSettings(t, done)
				mu.Lock()
				defer mu.Unlock()
				if len(got) != 1 {
					t.Fatalf("the settings handler ran %d times, want once", len(got))
				}
				r := got[0]
				if r.Method != call.method || r.ConfigID != call.id || r.Value != call.value || r.ModelChange != call.model {
					t.Fatalf("the handler was told %+v, want a model change: %v", r, call.model)
				}
				if !refuse {
					if a.err != nil || !a.cat.Present {
						t.Fatalf("the call answered (%+v, %v)", a.cat, a.err)
					}
					return
				}
				if !errors.Is(a.err, ErrBadCatalog) || !strings.Contains(a.err.Error(), call.method) {
					t.Fatalf("a refused reply answered (%+v, %v), want the handler's error with the method", a.cat, a.err)
				}
				if a.cat.Present {
					t.Fatalf("a refused reply answered with a catalog: %+v", a.cat)
				}
			})
		}
	}
}

// TestRPCErrorDataMessage reads both shapes the agents put a refusal's detail
// in: cursor's {message} (X1.3) and grok's bare string (X2).
func TestRPCErrorDataMessage(t *testing.T) {
	for _, tc := range []struct {
		data, want string
	}{
		{`{"message":"Unknown model config option: model"}`, "Unknown model config option: model"},
		{`"unknown model id"`, "unknown model id"},
		{``, ""},
		{`null`, ""},
		{`3`, ""},
		{`{"detail":"x"}`, ""},
	} {
		e := &RPCError{Code: CodeInvalidParams, Message: "Invalid params", Data: json.RawMessage(tc.data)}
		if got := e.DataMessage(); got != tc.want {
			t.Errorf("DataMessage(%s) = %q, want %q", tc.data, got, tc.want)
		}
	}
}
