package codexrpc

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

func TestAsyncServerRequestDoesNotBlockInterruptReply(t *testing.T) {
	srvInR, srvInW := io.Pipe()
	srvOutR, srvOutW := io.Pipe()

	client := NewInProcess(srvOutR, srvInW)
	reqParked := make(chan struct{})
	reqRelease := make(chan struct{})
	var reqIDSeen string
	client.SetRequestHandler(func(ctx context.Context, msg *Message) {
		if msg.ID != nil {
			reqIDSeen = string(*msg.ID)
		}
		close(reqParked)
		select {
		case <-reqRelease:
			_ = client.Reply(*msg.ID, ApprovalDecisionReply{Decision: "decline"})
		case <-ctx.Done():
		}
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
