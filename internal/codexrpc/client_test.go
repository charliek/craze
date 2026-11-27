package codexrpc

import (
	"bufio"
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
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/errkind"
)

func TestDecoderMissingJSONRPCAndZeroID(t *testing.T) {
	raw := strings.Join([]string{
		`{"id": 1, "result": {"userAgent": "codex/0.172.0"}}`,
		`{"method": "item/commandExecution/requestApproval", "id": 0, "params": {"command": "ls"}}`,
		`{"method": "turn/started", "params": {"threadId": "t1"}, "emittedAtMs": 1234}`,
	}, "\n") + "\n"

	dec := NewDecoder(strings.NewReader(raw))

	m1, err := dec.ReadMessage()
	if err != nil {
		t.Fatalf("read 1: %v", err)
	}
	if !m1.IsResponse() || m1.ID == nil || string(*m1.ID) != "1" {
		t.Fatalf("m1 = %+v, want response id=1", m1)
	}

	m2, err := dec.ReadMessage()
	if err != nil {
		t.Fatalf("read 2: %v", err)
	}
	if !m2.IsRequest() || m2.ID == nil || string(*m2.ID) != "0" {
		t.Fatalf("m2 = %+v, want request id=0", m2)
	}

	m3, err := dec.ReadMessage()
	if err != nil {
		t.Fatalf("read 3: %v", err)
	}
	if !m3.IsNotification() || m3.ID != nil || m3.Method != "turn/started" {
		t.Fatalf("m3 = %+v, want notification turn/started", m3)
	}

	if _, err := dec.ReadMessage(); !errors.Is(err, io.EOF) {
		t.Fatalf("read 4 = %v, want EOF", err)
	}
}

func TestDecoderEnforcesLineLimitWhileReading(t *testing.T) {
	oversized := `{"method":"x","params":{"pad":"` + strings.Repeat("a", 2048) + `"}}` + "\n"
	dec := NewDecoderWithLimit(strings.NewReader(oversized), 512)
	_, err := dec.ReadMessage()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("ReadMessage = %v, want ErrLineTooLong", err)
	}
}

func TestDecoderOpenPipeOverCapWithoutNewline(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()

	const limit = 128
	dec := NewDecoderWithLimit(r, limit)
	done := make(chan error, 1)
	go func() {
		_, err := dec.ReadMessage()
		done <- err
	}()
	// Send exactly limit+1 bytes with no newline and keep writer open.
	if _, err := w.Write(bytes.Repeat([]byte("a"), limit+1)); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrLineTooLong) {
			t.Fatalf("ReadMessage = %v, want ErrLineTooLong", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Decoder blocked waiting for more bytes after exceeding maxBytes")
	}
}

func TestAsyncServerRequestDoesNotBlockInterruptReply(t *testing.T) {
	srvInR, srvInW := io.Pipe()
	srvOutR, srvOutW := io.Pipe()

	client := NewInProcess(srvOutR, srvInW)
	reqParked := make(chan struct{})
	reqRelease := make(chan struct{})
	var reqIDSeen string
	client.SetRequestHandler(func(ctx context.Context, msg *Message) bool {
		if msg.ID != nil {
			reqIDSeen = string(*msg.ID)
		}
		close(reqParked)
		select {
		case <-reqRelease:
			_ = client.Reply(*msg.ID, ApprovalDecisionReply{Decision: "decline"})
		case <-ctx.Done():
		}
		return true
	})
	client.Start()
	defer func() {
		close(reqRelease)
		_ = client.Close()
		_ = srvInR.Close()
		_ = srvOutW.Close()
	}()

	// Server sends a request with integer id=0 that parks in the handler.
	_, _ = io.WriteString(srvOutW, `{"method":"item/commandExecution/requestApproval","id":0,"params":{"threadId":"t1","turnId":"turn1","itemId":"c1"}}`+"\n")
	<-reqParked

	// Meanwhile, client sends turn/interrupt; server reads it and replies.
	srvDec := NewDecoder(srvInR)
	done := make(chan error, 1)
	go func() {
		done <- client.TurnInterrupt(context.Background(), "t1", "turn1")
	}()

	got, err := srvDec.ReadMessage()
	if err != nil {
		t.Fatalf("server read turn/interrupt: %v", err)
	}
	if got.Method != MethodTurnInterrupt || got.ID == nil {
		t.Fatalf("got %+v, want turn/interrupt request", got)
	}
	_, _ = io.WriteString(srvOutW, `{"id":`+string(*got.ID)+`,"result":{}}`+"\n")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("TurnInterrupt failed while approval parked: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("TurnInterrupt deadlocked behind parked server request handler")
	}
	if reqIDSeen != "0" {
		t.Fatalf("reqIDSeen = %q, want \"0\"", reqIDSeen)
	}
}

func TestUnknownServerRequestGetsMethodNotFound(t *testing.T) {
	srvInR, srvInW := io.Pipe()
	srvOutR, srvOutW := io.Pipe()

	client := NewInProcess(srvOutR, srvInW)
	// No request handler registered.
	client.Start()
	defer func() {
		_ = client.Close()
		_ = srvInR.Close()
		_ = srvOutW.Close()
	}()

	_, _ = io.WriteString(srvOutW, `{"method":"unknown/request","id":0,"params":{}}`+"\n")

	srvDec := NewDecoder(srvInR)
	reply, err := srvDec.ReadMessage()
	if err != nil {
		t.Fatalf("server read reply: %v", err)
	}
	if reply.ID == nil || string(*reply.ID) != "0" {
		t.Fatalf("reply ID = %v, want 0", reply.ID)
	}
	if reply.Error == nil || reply.Error.Code != CodeMethodNotFound {
		t.Fatalf("reply Error = %+v, want -32601", reply.Error)
	}
}

func TestUnknownServerRequestGetsMethodNotFoundWithHandlerInstalled(t *testing.T) {
	srvInR, srvInW := io.Pipe()
	srvOutR, srvOutW := io.Pipe()

	client := NewInProcess(srvOutR, srvInW)
	client.SetRequestHandler(func(_ context.Context, msg *Message) bool {
		return msg.Method == RequestCommandApproval
	})
	client.Start()
	defer func() {
		_ = client.Close()
		_ = srvInR.Close()
		_ = srvOutW.Close()
	}()

	_, _ = io.WriteString(srvOutW, `{"method":"unhandled/method","id":0,"params":{}}`+"\n")

	srvDec := NewDecoder(srvInR)
	reply, err := srvDec.ReadMessage()
	if err != nil {
		t.Fatalf("server read reply: %v", err)
	}
	if reply.ID == nil || string(*reply.ID) != "0" {
		t.Fatalf("reply ID = %v, want 0", reply.ID)
	}
	if reply.Error == nil || reply.Error.Code != CodeMethodNotFound {
		t.Fatalf("reply Error = %+v, want -32601", reply.Error)
	}
}

func TestPolymorphicCodexErrorInfoAndClassification(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantKind errkind.Kind
		wantHTTP int
	}{
		{
			name:     "bare string unauthorized",
			raw:      `{"message":"auth failed","codexErrorInfo":"unauthorized"}`,
			wantKind: errkind.Auth,
		},
		{
			name:     "externally tagged 401",
			raw:      `{"message":"stream disconnected","codexErrorInfo":{"responseStreamDisconnected":{"httpStatusCode":401}}}`,
			wantKind: errkind.Auth,
			wantHTTP: 401,
		},
		{
			name:     "bare string usageLimitExceeded",
			raw:      `{"message":"limit reached","codexErrorInfo":"usageLimitExceeded"}`,
			wantKind: errkind.Quota,
		},
		{
			name:     "tagged object contextWindowExceeded",
			raw:      `{"message":"too long","codexErrorInfo":{"type":"contextWindowExceeded"}}`,
			wantKind: errkind.ContextTooLarge,
		},
		{
			name:     "externally tagged 429",
			raw:      `{"message":"rate limited","codexErrorInfo":{"httpConnectionFailed":{"httpStatusCode":429}}}`,
			wantKind: errkind.Quota,
			wantHTTP: 429,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var te TurnError
			if err := json.Unmarshal([]byte(tc.raw), &te); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			info := ClassifyTurnError(&te)
			if info.Kind != tc.wantKind || info.Status != tc.wantHTTP {
				t.Fatalf("ClassifyTurnError = %+v, want kind=%v status=%d", info, tc.wantKind, tc.wantHTTP)
			}
		})
	}
}

func TestCollaborationModeAndSandboxJSONShapes(t *testing.T) {
	effort := "high"
	cm := CollaborationMode{
		Mode: "plan",
		Settings: CollaborationSettings{
			Model:                 "gpt-6.2-sol",
			ReasoningEffort:       &effort,
			DeveloperInstructions: nil,
		},
	}
	b, err := json.Marshal(cm)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`"mode":"plan"`,
		`"model":"gpt-6.2-sol"`,
		`"reasoning_effort":"high"`,
		`"developer_instructions":null`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("CollaborationMode JSON = %s, missing %s", s, want)
		}
	}

	sp := SandboxPolicy{Type: SandboxPolicyReadOnly}
	sb, err := json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	if string(sb) != `{"type":"readOnly"}` {
		t.Fatalf("SandboxPolicy JSON = %s, want {\"type\":\"readOnly\"}", sb)
	}
}

func TestFixtureReplay(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "session_frames.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	dec := NewDecoder(f)
	var responses, requests, notifs int
	for {
		msg, err := dec.ReadMessage()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("fixture decode error: %v", err)
		}
		switch {
		case msg.IsResponse():
			responses++
		case msg.IsRequest():
			requests++
			switch msg.Method {
			case RequestCommandApproval:
				var p CommandApprovalParams
				if err := json.Unmarshal(msg.Params, &p); err != nil {
					t.Fatalf("unmarshal command approval: %v", err)
				}
				if len(p.AvailableDecisions) != 3 {
					t.Fatalf("AvailableDecisions len = %d, want 3", len(p.AvailableDecisions))
				}
			case RequestUserInput:
				var p RequestUserInputParams
				if err := json.Unmarshal(msg.Params, &p); err != nil {
					t.Fatalf("unmarshal requestUserInput: %v", err)
				}
				if len(p.Questions) != 1 || p.Questions[0].ID != "greeting" {
					t.Fatalf("questions = %+v", p.Questions)
				}
			}
		case msg.IsNotification():
			notifs++
			if msg.Method == NotifyThreadTokenUsageUpdated {
				var p TokenUsageUpdatedParams
				if err := json.Unmarshal(msg.Params, &p); err != nil {
					t.Fatalf("unmarshal tokenUsage: %v", err)
				}
				if p.TokenUsage.Total.InputTokens != 16315 || p.TokenUsage.Total.ReasoningOutputTokens != 36 {
					t.Fatalf("unexpected tokenUsage: %+v", p.TokenUsage)
				}
			}
		}
	}
	if responses != 1 || requests != 2 || notifs != 9 {
		t.Fatalf("fixture counts = responses:%d requests:%d notifs:%d, want 1/2/9", responses, requests, notifs)
	}
}

func buildFakeAgent(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "craze-fake-agent")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/craze-fake-agent")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build craze-fake-agent: %v\n%s", err, out)
	}
	return bin
}

func TestSpawnFakeCodexAppServerRoundTrip(t *testing.T) {
	bin := buildFakeAgent(t)
	var stderr bytes.Buffer
	client, err := Spawn(SpawnOptions{
		Binary: bin,
		Args:   []string{"app-server", "--listen", "stdio://"},
		Dir:    t.TempDir(),
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = client.Close() }()

	var (
		mu       sync.Mutex
		deltas   strings.Builder
		turnDone = make(chan string, 1)
	)
	client.SetNotifyHandler(func(msg *Message) {
		switch msg.Method {
		case NotifyAgentMessageDelta:
			var p ItemDeltaParams
			if json.Unmarshal(msg.Params, &p) == nil {
				mu.Lock()
				deltas.WriteString(p.Delta)
				mu.Unlock()
			}
		case NotifyTurnCompleted:
			var p TurnEventParams
			if json.Unmarshal(msg.Params, &p) == nil {
				select {
				case turnDone <- p.Turn.Status:
				default:
				}
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	initRes, err := client.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if initRes.UserAgent == "" {
		t.Fatal("empty userAgent")
	}

	acct, err := client.AccountRead(ctx, false)
	if err != nil || acct.Account == nil || acct.Account.Type != "chatgpt" {
		t.Fatalf("AccountRead = %+v, %v", acct, err)
	}

	models, err := client.ModelList(ctx, ModelListParams{IncludeHidden: true})
	if err != nil || len(models.Data) < 3 {
		t.Fatalf("ModelList = %+v, %v", models, err)
	}

	ephemeral := false
	tr, err := client.ThreadStart(ctx, ThreadStartParams{
		Cwd:            t.TempDir(),
		Sandbox:        SandboxWorkspaceWrite,
		ApprovalPolicy: ApprovalNever,
		Model:          "gpt-6.2-sol",
		Ephemeral:      &ephemeral,
	})
	if err != nil || tr.Thread.ID == "" {
		t.Fatalf("ThreadStart = %+v, %v", tr, err)
	}

	eff := "low"
	_, err = client.TurnStart(ctx, TurnStartParams{
		ThreadID:       tr.Thread.ID,
		SandboxPolicy:  tr.Sandbox,
		ApprovalPolicy: ApprovalNever,
		CollaborationMode: &CollaborationMode{
			Mode: "default",
			Settings: CollaborationSettings{
				Model:                 "gpt-6.2-sol",
				ReasoningEffort:       &eff,
				DeveloperInstructions: nil,
			},
		},
		Input: []UserInput{{Type: "text", Text: "hello codex"}},
	})
	if err != nil {
		t.Fatalf("TurnStart: %v", err)
	}

	select {
	case status := <-turnDone:
		if status != "completed" {
			t.Fatalf("turn status = %q, want completed", status)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for turn/completed")
	}

	mu.Lock()
	got := deltas.String()
	mu.Unlock()
	if !strings.Contains(got, "hello codex") {
		t.Fatalf("deltas = %q, want hello codex", got)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
}

func TestCodexErrorInfoReuseAndMultiKeyDeterminism(t *testing.T) {
	var info CodexErrorInfo
	if err := json.Unmarshal([]byte(`{"responseStreamDisconnected":{"httpStatusCode":401}}`), &info); err != nil {
		t.Fatal(err)
	}
	if info.Kind != "responseStreamDisconnected" || info.HTTPStatusCode != 401 {
		t.Fatalf("first decode = %+v", info)
	}
	// Reuse same receiver for bare string: HTTPStatusCode must reset to 0.
	if err := json.Unmarshal([]byte(`"contextWindowExceeded"`), &info); err != nil {
		t.Fatal(err)
	}
	if info.Kind != "contextWindowExceeded" || info.HTTPStatusCode != 0 {
		t.Fatalf("second decode = %+v, want Kind=contextWindowExceeded HTTPStatusCode=0", info)
	}
	// Multi-key object without "type" must not pick an arbitrary variant.
	if err := json.Unmarshal([]byte(`{"a":{"httpStatusCode":401},"b":{"httpStatusCode":429}}`), &info); err != nil {
		t.Fatal(err)
	}
	if info.Kind != "" || info.HTTPStatusCode != 0 {
		t.Fatalf("multi-key decode = %+v, want empty Kind and zero HTTPStatusCode", info)
	}
}

func TestEncoderDoesNotMutateInputOrEscapeRawID(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)

	shared := &Message{Method: "ping"}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = enc.WriteMessage(shared)
		}()
	}
	wg.Wait()
	if shared.JSONRPC != "" {
		t.Fatalf("WriteMessage mutated caller's Message.JSONRPC = %q", shared.JSONRPC)
	}

	// Verify raw ID 0 and string ID containing '<' serialize verbatim.
	buf.Reset()
	idZero := json.RawMessage(`0`)
	if err := enc.WriteMessage(&Message{ID: &idZero, Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"id":0`) {
		t.Fatalf("serialized zero ID = %s, want \"id\":0", buf.String())
	}

	buf.Reset()
	idLT := json.RawMessage(`"req<1>"`)
	if err := enc.WriteMessage(&Message{ID: &idLT, Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"id":"req<1>"`) {
		t.Fatalf("serialized string ID = %s, want verbatim \"id\":\"req<1>\"", buf.String())
	}
}

func TestStartCloseLifecycleAndCloseAsync(t *testing.T) {
	// Close before Start must not hang.
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	c1 := NewInProcess(r2, w1)
	done := make(chan error, 1)
	go func() { done <- c1.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close before Start = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close before Start hung")
	}
	_ = r1.Close()
	_ = w2.Close()

	// Double Start + CloseAsync from inside onNotify must not deadlock or panic.
	srvInR, srvInW := io.Pipe()
	srvOutR, srvOutW := io.Pipe()
	c2 := NewInProcess(srvOutR, srvInW)
	notified := make(chan struct{})
	c2.SetNotifyHandler(func(*Message) {
		c2.CloseAsync()
		close(notified)
	})
	c2.Start()
	c2.Start() // idempotent
	_, _ = io.WriteString(srvOutW, `{"method":"turn/started","params":{}}`+"\n")
	<-notified
	select {
	case <-c2.NotifyDone():
	case <-time.After(2 * time.Second):
		t.Fatal("CloseAsync from onNotify did not finish")
	}
	_ = c2.Close()
	_ = srvInR.Close()
	_ = srvOutW.Close()
}

func TestFramingErrorTearsDownTransportAndPreservesError(t *testing.T) {
	srvInR, srvInW := io.Pipe()
	srvOutR, srvOutW := io.Pipe()
	c := NewInProcess(srvOutR, srvInW)
	c.dec = NewDecoderWithLimit(srvOutR, 64)
	reqCtxDone := make(chan struct{})
	c.SetRequestHandler(func(ctx context.Context, _ *Message) bool {
		<-ctx.Done()
		close(reqCtxDone)
		return true
	})
	c.Start()
	defer func() {
		_ = c.Close()
		_ = srvInR.Close()
		_ = srvOutW.Close()
	}()

	_, _ = io.WriteString(srvOutW, `{"method":"req","id":1,"params":{}}`+"\n")
	// Trigger line-too-long framing error.
	_, _ = io.WriteString(srvOutW, strings.Repeat("x", 100)+"\n")

	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("transport did not tear down on framing error")
	}
	if !errors.Is(c.Err(), ErrLineTooLong) {
		t.Fatalf("c.Err() = %v, want ErrLineTooLong", c.Err())
	}
	select {
	case <-reqCtxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("request context was not cancelled on framing error")
	}
}

func TestCallContextCancelledBeforeAndDuringWrite(t *testing.T) {
	srvInR, srvInW := io.Pipe()
	srvOutR, srvOutW := io.Pipe()
	c := NewInProcess(srvOutR, srvInW)
	c.Start()
	defer func() {
		_ = c.Close()
		_ = srvInR.Close()
		_ = srvOutW.Close()
	}()

	// Pre-cancelled context must fail without writing to server.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Call(cancelledCtx, "initialize", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Call(cancelled) = %v, want context.Canceled", err)
	}

	// Blocking write (nobody reads srvInR) must honor ctx timeout and abort transport.
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer writeCancel()
	if err := c.Call(writeCtx, "initialize", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call(blocked write) = %v, want context.DeadlineExceeded", err)
	}
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("transport was not aborted after write cancellation")
	}
}

func TestNotifyQueueAndInFlightRequestBounds(t *testing.T) {
	t.Run("notify overflow", func(t *testing.T) {
		srvInR, srvInW := io.Pipe()
		srvOutR, srvOutW := io.Pipe()
		c := NewInProcess(srvOutR, srvInW)
		c.maxNotifyItems = 2
		block := make(chan struct{})
		c.SetNotifyHandler(func(*Message) { <-block })
		c.Start()
		defer func() {
			close(block)
			_ = c.Close()
			_ = srvInR.Close()
			_ = srvOutW.Close()
		}()
		for range 6 {
			if _, err := io.WriteString(srvOutW, `{"method":"n","params":{}}`+"\n"); err != nil {
				break
			}
		}
		select {
		case <-c.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("notify overflow did not fail transport")
		}
		if !errors.Is(c.Err(), ErrQueueOverflow) {
			t.Fatalf("c.Err() = %v, want ErrQueueOverflow", c.Err())
		}
	})

	t.Run("too many requests", func(t *testing.T) {
		srvInR, srvInW := io.Pipe()
		srvOutR, srvOutW := io.Pipe()
		c := NewInProcess(srvOutR, srvInW)
		c.reqSem = make(chan struct{}, 2)
		c.SetRequestHandler(func(ctx context.Context, _ *Message) bool {
			<-ctx.Done()
			return true
		})
		c.Start()
		defer func() {
			_ = c.Close()
			_ = srvInR.Close()
			_ = srvOutW.Close()
		}()
		for i := range 5 {
			if _, err := io.WriteString(srvOutW, `{"method":"r","id":`+strconv.Itoa(i)+`,"params":{}}`+"\n"); err != nil {
				break
			}
		}
		select {
		case <-c.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("request overflow did not fail transport")
		}
		if !errors.Is(c.Err(), ErrTooManyRequests) {
			t.Fatalf("c.Err() = %v, want ErrTooManyRequests", c.Err())
		}
	})
}

func TestChildRepliesAndExitsImmediately(t *testing.T) {
	for _, code := range []string{"0", "7"} {
		t.Run("exit_"+code, func(t *testing.T) {
			script := `read line; printf '{"id":1,"result":{"userAgent":"fast-exit"}}\n'; exit ` + code
			c, err := Spawn(SpawnOptions{
				Binary: "/bin/sh",
				Args:   []string{"-c", script},
				Dir:    t.TempDir(),
			})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			defer func() { _ = c.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var res InitializeResult
			if err := c.Call(ctx, MethodInitialize, struct{}{}, &res); err != nil {
				t.Fatalf("Call lost final reply on immediate exit %s: %v", code, err)
			}
			if res.UserAgent != "fast-exit" {
				t.Fatalf("UserAgent = %q, want fast-exit", res.UserAgent)
			}
		})
	}
}

func TestCloseKillsEscapedDescendant(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "escaped.pid")
	// Spawn a grandchild in its own session (setsid) so raw -pgid cannot reach it.
	var script string
	if _, err := exec.LookPath("setsid"); err == nil {
		script = `setsid sleep 60 </dev/null >/dev/null 2>&1 & echo $! > ` + pidFile + `; echo '{"method":"ready","params":{}}'; read line`
	} else {
		script = `sleep 60 </dev/null >/dev/null 2>&1 & echo $! > ` + pidFile + `; echo '{"method":"ready","params":{}}'; read line`
	}
	c, err := Spawn(SpawnOptions{
		Binary: "/bin/sh",
		Args:   []string{"-c", script},
		Dir:    dir,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	ready := make(chan struct{})
	c.SetNotifyHandler(func(msg *Message) {
		if msg.Method == "ready" {
			select {
			case <-ready:
			default:
				close(ready)
			}
		}
	})
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		_ = c.Close()
		t.Fatal("timed out waiting for ready notification")
	}
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			if n, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && n > 0 {
				pid = n
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		_ = c.Close()
		t.Fatal("failed to read escaped descendant pid")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	killDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(killDeadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("escaped descendant pid %d survived Client.Close", pid)
}

func TestFakeServerInterruptReleasesParkedApproval(t *testing.T) {
	bin := buildFakeAgent(t)
	env := append(os.Environ(), "CRAZE_FAKE_SCRIPT=codex-approval")
	client, err := Spawn(SpawnOptions{
		Binary: bin,
		Args:   []string{"app-server", "--listen", "stdio://"},
		Dir:    t.TempDir(),
		Env:    env,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = client.Close() }()

	approvalSeen := make(chan struct{}, 1)
	resolvedSeen := make(chan struct{}, 1)
	turnStatus := make(chan string, 1)

	client.SetRequestHandler(func(ctx context.Context, msg *Message) bool {
		if msg.Method == RequestCommandApproval {
			select {
			case approvalSeen <- struct{}{}:
			default:
			}
			// Deliberately do NOT reply; wait for ctx cancel or turn/interrupt.
			<-ctx.Done()
			return true
		}
		return false
	})
	client.SetNotifyHandler(func(msg *Message) {
		switch msg.Method {
		case NotifyServerRequestResolved:
			select {
			case resolvedSeen <- struct{}{}:
			default:
			}
		case NotifyTurnCompleted:
			var p TurnEventParams
			if json.Unmarshal(msg.Params, &p) == nil {
				select {
				case turnStatus <- p.Turn.Status:
				default:
				}
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	tr, err := client.ThreadStart(ctx, ThreadStartParams{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("ThreadStart: %v", err)
	}
	turnRes, err := client.TurnStart(ctx, TurnStartParams{
		ThreadID: tr.Thread.ID,
		Input:    []UserInput{{Type: "text", Text: "run touch"}},
	})
	if err != nil {
		t.Fatalf("TurnStart: %v", err)
	}

	select {
	case <-approvalSeen:
	case <-ctx.Done():
		t.Fatal("approval request never arrived")
	}
	if err := client.TurnInterrupt(ctx, tr.Thread.ID, turnRes.Turn.ID); err != nil {
		t.Fatalf("TurnInterrupt: %v", err)
	}
	select {
	case <-resolvedSeen:
	case <-ctx.Done():
		t.Fatal("serverRequest/resolved never arrived on interrupt")
	}
	select {
	case st := <-turnStatus:
		if st != "interrupted" {
			t.Fatalf("turn status = %q, want interrupted", st)
		}
	case <-ctx.Done():
		t.Fatal("turn/completed never arrived on interrupt")
	}
}

func TestFixtureRedactionNoProhibitedKeys(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "session_frames.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, forbidden := range []string{
		`"codexHome"`,
		`"installationId"`,
		`"cf-ray"`,
		`"serverName"`,
		`sk-`,
		`Bearer `,
		`/home/`,
		`/Users/`,
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("testdata/session_frames.jsonl contains prohibited token %q", forbidden)
		}
	}
}

// TestReplyCtxServerRequestResolvedDoesNotAbortTransport verifies that when the
// peer immediately emits serverRequest/resolved upon reading a reply (racing
// with writeFrame's completion and cancelling reqCtx), the shared transport
// stays healthy and subsequent RPC calls succeed.
func TestReplyCtxServerRequestResolvedDoesNotAbortTransport(t *testing.T) {
	srvToCliR, srvToCliW := io.Pipe()
	cliToSrvR, cliToSrvW := io.Pipe()
	defer srvToCliR.Close()
	defer srvToCliW.Close()
	defer cliToSrvR.Close()
	defer cliToSrvW.Close()

	c := newClient(srvToCliR, cliToSrvW, nil)
	replyErrCh := make(chan error, 1)
	c.SetRequestHandler(func(reqCtx context.Context, msg *Message) bool {
		if msg.ID == nil {
			return false
		}
		replyErrCh <- c.ReplyCtx(reqCtx, *msg.ID, map[string]any{"decision": "accept"})
		return true
	})
	c.Start()
	defer func() { _ = c.Close() }()

	// Simulated server: send request id 0, read reply, IMMEDIATELY send
	// serverRequest/resolved for id 0, then answer a follow-up account/read RPC.
	srvDone := make(chan error, 1)
	go func() {
		dec := bufio.NewReader(cliToSrvR)
		if _, err := io.WriteString(srvToCliW, `{"id":0,"method":"item/commandExecution/requestApproval","params":{"threadId":"t1","turnId":"u1","itemId":"i1"}}`+"\n"); err != nil {
			srvDone <- err
			return
		}
		if _, err := dec.ReadString('\n'); err != nil {
			srvDone <- err
			return
		}
		if _, err := io.WriteString(srvToCliW, `{"method":"serverRequest/resolved","params":{"threadId":"t1","requestId":0}}`+"\n"); err != nil {
			srvDone <- err
			return
		}
		// Read follow-up account/read request and reply cleanly.
		line, err := dec.ReadString('\n')
		if err != nil {
			srvDone <- err
			return
		}
		var req Message
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			srvDone <- err
			return
		}
		resp := fmt.Sprintf(`{"id":%s,"result":{"account":{"type":"chatgpt","planType":"pro"},"requiresOpenaiAuth":true}}`+"\n", string(*req.ID))
		_, err = io.WriteString(srvToCliW, resp)
		srvDone <- err
	}()

	select {
	case err := <-replyErrCh:
		if err != nil {
			t.Fatalf("ReplyCtx failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ReplyCtx")
	}

	// Follow-up RPC must succeed on the same healthy transport.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := c.AccountRead(ctx, false)
	if err != nil {
		t.Fatalf("follow-up AccountRead after serverRequest/resolved failed (transport aborted?): %v", err)
	}
	if res == nil || res.Account == nil || res.Account.PlanType != "pro" {
		t.Fatalf("unexpected AccountRead result: %+v", res)
	}
	if err := <-srvDone; err != nil {
		t.Fatalf("mock peer error: %v", err)
	}
}
