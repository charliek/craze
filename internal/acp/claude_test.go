package acp

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/version"
)

// outboundOnly is every Method constant the client only ever sends: none of
// them arrives from an agent, so none needs a dispatch decision.
var outboundOnly = []string{
	MethodInitialize, MethodAuthenticate, MethodSessionNew, MethodSessionLoad, MethodSessionPrompt,
	MethodSessionCancel, MethodSessionSetModel, MethodSessionSetMode, MethodSessionSetConfig,
	MethodGrokInterject, MethodGrokInterjectWrapped, MethodGrokBtw, MethodGrokBtwWrapped,
	MethodClaudeSteering,
}

// dialects is every dialect the client speaks, each held to a decision on
// every method (dialectHandles).
func dialects() []DialectID { return []DialectID{DialectCursor, DialectGrok, DialectClaude} }

// notUpdateKinds is every Update constant that is not a session/update kind:
// turn_completed rides x.ai/session_notification.
var notUpdateKinds = []string{UpdateTurnCompleted}

// packageConstants walks this package's own source (not its tests) for every
// string constant whose name starts with prefix, and returns their values.
func packageConstants(t *testing.T, prefix string) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !strings.HasPrefix(strings.ToLower(name.Name), strings.ToLower(prefix)) || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out[name.Name] = v
				}
			}
		}
	}
	return out
}

// TestDialectHandlesEveryMethod is plan 052 B1's exhaustiveness check: every
// method and session/update kind constant this package declares is either one
// the client only sends, or has a decision in every dialect. A new constant
// with no row fails here, naming it.
func TestDialectHandlesEveryMethod(t *testing.T) {
	methods := packageConstants(t, "Method")
	if len(methods) < 30 {
		t.Fatalf("the walk found only %d Method constants: %v", len(methods), methods)
	}
	for name, m := range methods {
		if slices.Contains(outboundOnly, m) {
			continue
		}
		if !slices.Contains(inboundMethods, m) {
			t.Errorf("%s (%q) is neither outbound only nor in inboundMethods", name, m)
		}
	}
	kinds := packageConstants(t, "Update")
	for name, k := range kinds {
		if slices.Contains(notUpdateKinds, k) || strings.HasPrefix(name, "Updates") {
			continue
		}
		if !slices.Contains(inboundMethods, updateMethod(k)) {
			t.Errorf("%s (%q) is a session/update kind with no row in inboundMethods", name, k)
		}
	}
	for _, d := range dialects() {
		for _, m := range inboundMethods {
			if dialectHandles(d, m) == decisionNone {
				t.Errorf("dialect %s has no decision for %q", d, m)
			}
		}
	}
	// A method nobody knows has none, in every dialect: the walk above would
	// pass vacuously if the table answered everything.
	for _, d := range dialects() {
		if got := dialectHandles(d, "made/up"); got != decisionNone {
			t.Fatalf("%s: made/up = %v", d, got)
		}
	}
}

// TestDialectDecisionsPinned holds the decisions the plan names (B1's table):
// claude refuses or ignores cursor/* and x.ai/*, reads its auth push, and
// handles elicitation/create, the form it declares (C3); cursor and grok keep
// their own families and refuse it.
func TestDialectDecisionsPinned(t *testing.T) {
	for _, tc := range []struct {
		d      DialectID
		method string
		want   decision
	}{
		{DialectCursor, MethodCursorAskQuestion, decisionHandle},
		{DialectGrok, MethodCursorAskQuestion, decisionRefuse},
		{DialectClaude, MethodCursorAskQuestion, decisionRefuse},
		{DialectClaude, MethodCursorUpdateTodos, decisionRefuse},
		{DialectClaude, MethodCursorTask, decisionRefuse},
		{DialectGrok, MethodGrokExitPlanModeWrapped, decisionHandle},
		{DialectCursor, MethodGrokExitPlanMode, decisionRefuse},
		{DialectClaude, MethodGrokAskUserQuestion, decisionRefuse},
		{DialectClaude, MethodGrokPromptComplete, decisionIgnore},
		{DialectClaude, MethodGrokQueueChanged, decisionIgnore},
		{DialectClaude, MethodGrokSessionNotification, decisionIgnore},
		{DialectClaude, MethodClaudeAuthStatus, decisionHandle},
		{DialectCursor, MethodClaudeAuthStatus, decisionIgnore},
		{DialectGrok, MethodClaudeAuthStatus, decisionIgnore},
		{DialectClaude, MethodAuthStatusUpdate, decisionIgnore},
		{DialectClaude, MethodElicitationCreate, decisionHandle},
		{DialectCursor, MethodElicitationCreate, decisionRefuse},
		{DialectGrok, MethodElicitationCreate, decisionRefuse},
		{DialectClaude, MethodRequestPermission, decisionHandle},
	} {
		if got := dialectHandles(tc.d, tc.method); got != tc.want {
			t.Errorf("%s %s = %v, want %v", tc.d, tc.method, got, tc.want)
		}
	}
}

// TestClaudeRefusesOtherDialectsRequests is the behaviour behind the table: a
// cursor/ask_question or x.ai/exit_plan_mode request sent to a claude client
// is answered -32601 and raises nothing, and so is an elicitation/create sent
// to cursor or grok, which never declare the form capability.
func TestClaudeRefusesOtherDialectsRequests(t *testing.T) {
	p := newRawPipeDialect(t, DialectClaude)
	p.setSession("s-1")
	asked := false
	p.client.SetAskHandler(func(Arrival, AskQuestionRequest) AskDecision { asked = true; return AskDecision{} })
	p.client.SetPlanHandler(func(Arrival, CreatePlanRequest) PlanDecision { asked = true; return PlanDecision{} })
	for i, m := range []string{MethodCursorAskQuestion, MethodCursorCreatePlan, MethodGrokExitPlanModeWrapped} {
		p.send(t, i+1, m, `{"sessionId":"s-1","questions":[],"plan":"x"}`)
		msg := p.readWithin(t, 3*time.Second, m)
		if msg.Error == nil || msg.Error.Code != CodeMethodNotFound {
			t.Fatalf("%s: reply %+v", m, msg)
		}
	}
	for _, d := range []DialectID{DialectCursor, DialectGrok} {
		q := newRawPipeDialect(t, d)
		q.setSession("s-1")
		q.client.SetAskHandler(func(Arrival, AskQuestionRequest) AskDecision { asked = true; return AskDecision{} })
		q.send(t, 9, MethodElicitationCreate, `{"mode":"form","sessionId":"s-1","message":"m","requestedSchema":{"type":"object","properties":{"question_0":{"type":"string","oneOf":[{"const":"a","title":"a"}]}}}}`)
		msg := q.readWithin(t, 3*time.Second, string(d)+" elicitation/create")
		if msg.Error == nil || msg.Error.Code != CodeMethodNotFound {
			t.Fatalf("%s elicitation/create: reply %+v", d, msg)
		}
	}
	if asked {
		t.Fatal("a refused request reached a handler")
	}
}

// TestClaudeAuthStatusRead pins the push's real shape (probe2, adapter
// 0.107.3): the kind, the label and the account's plan are read; the email
// and organisation never are; grok and cursor ignore it; a payload with no
// authStatus is not a status.
func TestClaudeAuthStatusRead(t *testing.T) {
	const real = `{"authStatus":{"kind":"account","label":"Claude Max","account":{"plan":"max","email":"user@example.com","organization":"Example Org"}}}`
	for _, d := range dialects() {
		p := newRawPipeDialect(t, d)
		var mu sync.Mutex
		var got []AuthStatus
		p.client.SetAuthStatusHandler(func(st AuthStatus) { mu.Lock(); got = append(got, st); mu.Unlock() })
		p.send(t, nil, MethodClaudeAuthStatus, real)
		p.send(t, nil, MethodClaudeAuthStatus, `{"nothing":true}`)
		p.send(t, nil, MethodClaudeAuthStatus, `{"authStatus":{"kind":"api_key","label":"Anthropic API key","detail":"apiKeyHelper"}}`)
		// A marker the read loop handles after the three above.
		done := make(chan struct{})
		p.client.SetUpdateHandler(func(SessionNotification) { close(done) })
		p.setSession("s-1")
		p.send(t, nil, MethodSessionUpdate, `{"sessionId":"s-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}`)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("marker never arrived")
		}
		mu.Lock()
		statuses := append([]AuthStatus(nil), got...)
		mu.Unlock()
		if d != DialectClaude {
			if len(statuses) != 0 {
				t.Fatalf("%s read an auth push: %+v", d, statuses)
			}
			continue
		}
		want := []AuthStatus{
			{Kind: "account", Label: "Claude Max", Plan: "max"},
			{Kind: "api_key", Label: "Anthropic API key", Detail: "apiKeyHelper"},
		}
		if !slices.Equal(statuses, want) {
			t.Fatalf("statuses %+v, want %+v", statuses, want)
		}
	}
}

// TestClaudeToolNameFromMeta: the claude dialect names a tool by
// _meta.claudeCode.toolName (the real Write frame's shape); cursor leaves it
// to rawInput._toolName, as ever.
func TestClaudeToolNameFromMeta(t *testing.T) {
	const upd = `{"sessionId":"s-1","update":{"_meta":{"claudeCode":{"toolName":"Write"}},"toolCallId":"toolu_1","sessionUpdate":"tool_call","name":"Write","rawInput":{},"status":"pending","title":"Preparing file…","kind":"edit","content":[],"locations":[]}}`
	for _, tc := range []struct {
		d    DialectID
		want string
	}{{DialectClaude, "Write"}, {DialectCursor, ""}} {
		p := newRawPipeDialect(t, tc.d)
		p.setSession("s-1")
		got := make(chan SessionNotification, 1)
		p.client.SetUpdateHandler(func(n SessionNotification) { got <- n })
		p.send(t, nil, MethodSessionUpdate, upd)
		select {
		case n := <-got:
			if n.ToolName != tc.want {
				t.Fatalf("%s: tool name %q, want %q", tc.d, n.ToolName, tc.want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no update")
		}
	}
}

// TestInitializeAndSessionParamsBytes pins cursor's and grok's initialize,
// session/new and session/load params byte for byte (plan 052 AC5): the
// SessionParams seam leaves them exactly as they were. Claude's initialize
// adds the elicitation form capability (C3); its session/new carries
// configOptions and _meta.
func TestInitializeAndSessionParamsBytes(t *testing.T) {
	cwd := t.TempDir()
	quotedCWD, _ := json.Marshal(cwd)
	wantInit := `{"protocolVersion":1,"clientInfo":{"name":"craze","version":` + strconv.Quote(version.Version) +
		`},"clientCapabilities":{"_meta":{"parameterizedModelPicker":true},"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false}}`
	wantNew := `{"cwd":` + string(quotedCWD) + `,"mcpServers":[]}`
	wantLoad := `{"sessionId":"s-load","cwd":` + string(quotedCWD) + `,"mcpServers":[]}`
	for _, d := range dialects() {
		p := newRawPipeDialect(t, d)
		go func() { _, _ = p.client.Initialize(t.Context()) }()
		msg := p.readWithin(t, 3*time.Second, "initialize")
		want := wantInit
		if d == DialectClaude {
			want = strings.TrimSuffix(wantInit, "}}") + `,"elicitation":{"form":{}}}}`
		}
		if string(msg.Params) != want {
			t.Fatalf("%s initialize\n got %s\nwant %s", d, msg.Params, want)
		}
		if d == DialectClaude {
			continue
		}
		go func() { _, _ = p.client.NewSession(t.Context(), cwd, SessionParams{}) }()
		msg = p.readWithin(t, 3*time.Second, "session/new")
		if string(msg.Params) != wantNew {
			t.Fatalf("%s session/new\n got %s\nwant %s", d, msg.Params, wantNew)
		}
		go func() { _, _ = p.client.LoadSession(t.Context(), "s-load", cwd, SessionParams{}) }()
		msg = p.readWithin(t, 3*time.Second, "session/load")
		if string(msg.Params) != wantLoad {
			t.Fatalf("%s session/load\n got %s\nwant %s", d, msg.Params, wantLoad)
		}
	}
	p := newRawPipeDialect(t, DialectClaude)
	sp := SessionParams{
		ConfigOptions: []ConfigOptionValue{{ConfigID: "mode", Value: "default"}, {ConfigID: "fast", Value: "off"}},
		Meta:          map[string]any{"claudeCode": map[string]any{"options": map[string]any{"disallowedTools": []string{"ScheduleWakeup"}}}},
	}
	go func() { _, _ = p.client.NewSession(t.Context(), cwd, sp) }()
	msg := p.readWithin(t, 3*time.Second, "claude session/new")
	want := `{"cwd":` + string(quotedCWD) + `,"mcpServers":[],"configOptions":[{"configId":"mode","value":"default"},{"configId":"fast","value":"off"}],"_meta":{"claudeCode":{"options":{"disallowedTools":["ScheduleWakeup"]}}}}`
	if string(msg.Params) != want {
		t.Fatalf("claude session/new\n got %s\nwant %s", msg.Params, want)
	}
}

// claudeFrame is one line of testdata/claude-basic/frames.jsonl: a real
// claude-agent-acp 0.107.3 frame (L0's capture, redacted: the session id, the
// paths, the account's email and organisation), in or out of the client.
type claudeFrame struct {
	Dir string  `json:"dir"`
	Msg Message `json:"msg"`
}

func claudeFrames(t *testing.T) []claudeFrame {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "claude-basic", "frames.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []claudeFrame
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var f claudeFrame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

// TestClaudeBasicFrames drives the claude dialect through a real session's
// frames: initialize, the two auth pushes, session/new, and a Write with its
// permission round trip. The auth push is read as the gate reads it, the tool
// name comes from _meta.claudeCode.toolName, and the option the handler
// picked goes back byte for byte as the real exchange had it.
func TestClaudeBasicFrames(t *testing.T) {
	frames := claudeFrames(t)
	in := func(method string, id string) *Message {
		for _, f := range frames {
			if f.Dir == "in" && f.Msg.Method == method && (id == "" || string(f.Msg.ID) == id) {
				m := f.Msg
				return &m
			}
		}
		t.Fatalf("no frame %s %s", method, id)
		return nil
	}
	p := newRawPipeDialect(t, DialectClaude)
	var mu sync.Mutex
	var auth []AuthStatus
	var tools []string
	p.client.SetAuthStatusHandler(func(st AuthStatus) { mu.Lock(); auth = append(auth, st); mu.Unlock() })
	p.client.SetUpdateHandler(func(n SessionNotification) {
		if n.ToolName != "" {
			mu.Lock()
			tools = append(tools, n.ToolName)
			mu.Unlock()
		}
	})
	p.client.SetPermissionHandler(func(_ Arrival, req PermissionRequest) PermissionDecision {
		id, _ := PickKind(req.Options, KindAllowOnce)
		return PermissionDecision{OptionID: id}
	})
	reply := func(req *Message, result json.RawMessage) {
		if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: req.ID, Result: result}); err != nil {
			t.Fatal(err)
		}
	}
	initDone := make(chan *InitializeResult, 1)
	go func() { r, _ := p.client.Initialize(t.Context()); initDone <- r }()
	reply(p.readWithin(t, 3*time.Second, "initialize"), frames[0].Msg.Result)
	res := <-initDone
	if res == nil || len(res.AuthMethods) != 0 || !res.LoadSession() {
		t.Fatalf("initialize %+v", res)
	}
	for _, f := range frames {
		if f.Dir == "in" && (f.Msg.Method == MethodClaudeAuthStatus || f.Msg.Method == MethodAuthStatusUpdate) {
			p.send(t, nil, f.Msg.Method, string(f.Msg.Params))
		}
	}
	newDone := make(chan *NewSessionResult, 1)
	go func() { r, _ := p.client.NewSession(t.Context(), t.TempDir(), SessionParams{}); newDone <- r }()
	reply(p.readWithin(t, 3*time.Second, "session/new"), in("", "2").Result)
	if r := <-newDone; r == nil || r.SessionID != "SESSION" {
		t.Fatalf("session/new %+v", r)
	}
	promptDone := make(chan *PromptResult, 1)
	go func() { r, _ := p.client.Prompt(t.Context(), "write"); promptDone <- r }()
	prompt := p.readWithin(t, 3*time.Second, "session/prompt")
	for _, f := range frames {
		if f.Dir == "in" && f.Msg.Method == MethodSessionUpdate {
			p.send(t, nil, f.Msg.Method, string(f.Msg.Params))
		}
	}
	perm := in(MethodRequestPermission, "")
	p.send(t, 0, perm.Method, string(perm.Params))
	answer := p.readWithin(t, 3*time.Second, "the permission answer")
	var want string
	for _, f := range frames {
		if f.Dir == "out" && f.Msg.Method == "" {
			want = string(f.Msg.Result)
		}
	}
	if string(answer.Result) != want {
		t.Fatalf("permission answer\n got %s\nwant %s", answer.Result, want)
	}
	reply(prompt, in("", "6").Result)
	if r := <-promptDone; r == nil || r.StopReason != StopEndTurn {
		t.Fatalf("prompt %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(auth, []AuthStatus{{Kind: "account", Label: "Claude Ultra", Plan: "max"}}) {
		t.Fatalf("auth %+v", auth)
	}
	if len(tools) < 4 || slices.ContainsFunc(tools, func(n string) bool { return n != "Write" }) {
		t.Fatalf("tool names %v", tools)
	}
}
