package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	subTestParent = "parent-1"
	subTestChild  = "sub-1"
	subTestChild2 = "sub-2"
)

func spawnedParams(outer, child, attempt string) string {
	return fmt.Sprintf(`{"sessionId":%q,"update":{"sessionUpdate":"subagent_spawned","subagent_id":%q,"attempt_id":%q,"parent_session_id":%q,"child_session_id":%q,"subagent_type":"explore","description":"List files","model":"grok-4.6"}}`,
		outer, child, attempt, outer, child)
}

func finishedParams(outer, child, status string) string {
	return fmt.Sprintf(`{"sessionId":%q,"update":{"sessionUpdate":"subagent_finished","subagent_id":%q,"attempt_id":"at1","child_session_id":%q,"status":%q}}`,
		outer, child, child, status)
}

func progressParams(outer, child string) string {
	return fmt.Sprintf(`{"sessionId":%q,"update":{"sessionUpdate":"subagent_progress","subagent_id":%q,"attempt_id":"at1","child_session_id":%q,"tokens_used":100}}`,
		outer, child, child)
}

func childUpdateParams(child, kind, text string) string {
	return fmt.Sprintf(`{"sessionId":%q,"update":{"sessionUpdate":%q,"content":{"type":"text","text":%q}}}`, child, kind, text)
}

type notifyCapture struct {
	mu       sync.Mutex
	updates  []SessionNotification
	subagent []SubagentNotification
}

func (c *notifyCapture) addUpdate(n SessionNotification) {
	c.mu.Lock()
	c.updates = append(c.updates, n)
	c.mu.Unlock()
}

func (c *notifyCapture) addSubagent(n SubagentNotification) {
	c.mu.Lock()
	c.subagent = append(c.subagent, n)
	c.mu.Unlock()
}

func (c *notifyCapture) snapshot() ([]SessionNotification, []SubagentNotification) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]SessionNotification(nil), c.updates...), append([]SubagentNotification(nil), c.subagent...)
}

func (c *notifyCapture) waitFor(t *testing.T, updates, subagents int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		u, s := len(c.updates), len(c.subagent)
		c.mu.Unlock()
		if u >= updates && s >= subagents {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.mu.Lock()
	u, s := len(c.updates), len(c.subagent)
	c.mu.Unlock()
	t.Fatalf("timed out waiting for %d updates/%d subagents, have %d/%d", updates, subagents, u, s)
}

func grokPipe(t *testing.T) (*rawPipe, *notifyCapture) {
	t.Helper()
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession(subTestParent)
	cap := &notifyCapture{}
	p.client.SetUpdateHandler(cap.addUpdate)
	p.client.SetSubagentHandler(cap.addSubagent)
	return p, cap
}

func TestSubagentParserBothMethodNames(t *testing.T) {
	raw := spawnedParams(subTestParent, subTestChild, "at1")
	for _, tc := range []struct {
		name   string
		method string
		params string
	}{
		{"direct", MethodGrokSessionNotification, raw},
		{"wrapped", MethodGrokSessionNotificationWrapped, raw},
		{"ext-nested", MethodGrokSessionNotificationWrapped, `{"method":"x.ai/session_notification","params":` + raw + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, cap := grokPipe(t)
			p.send(t, nil, tc.method, tc.params)
			cap.waitFor(t, 0, 1)
			_, subs := cap.snapshot()
			n := subs[0]
			if n.Kind != SubagentSpawned || n.ChildSessionID != subTestChild || n.AttemptID != "at1" {
				t.Fatalf("parsed %+v", n)
			}
			if n.SessionID != subTestParent || n.SubagentType != "explore" || n.Model != "grok-4.6" {
				t.Fatalf("fields %+v", n)
			}
		})
	}
}

func TestSubagentParserKindsAndMalformed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params string
		ok     bool
		drop   bool
		kind   string
	}{
		{"spawned", spawnedParams("p", "c", "a"), true, false, SubagentSpawned},
		{"progress", progressParams("p", "c"), true, false, SubagentProgress},
		{"finished", finishedParams("p", "c", "completed"), true, false, SubagentFinished},
		{"unknown kind ignored", `{"sessionId":"p","update":{"sessionUpdate":"hook_execution","subagent_id":"c","child_session_id":"c"}}`, false, false, ""},
		{"missing child dropped", `{"sessionId":"p","update":{"sessionUpdate":"subagent_spawned","subagent_id":"c"}}`, false, true, SubagentSpawned},
		{"missing subagent id dropped", `{"sessionId":"p","update":{"sessionUpdate":"subagent_finished","child_session_id":"c"}}`, false, true, SubagentFinished},
		{"missing outer dropped", `{"update":{"sessionUpdate":"subagent_spawned","subagent_id":"c","child_session_id":"c"}}`, false, true, SubagentSpawned},
		{"null params dropped", `null`, false, true, ""},
		{"non-object dropped", `[1]`, false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, ok, drop := parseSubagentNotification(json.RawMessage(tc.params))
			if ok != tc.ok || drop != tc.drop {
				t.Fatalf("ok=%v drop=%v want %v/%v (%+v)", ok, drop, tc.ok, tc.drop, n)
			}
			if (ok || tc.kind != "") && n.Kind != tc.kind {
				t.Fatalf("kind %q want %q", n.Kind, tc.kind)
			}
		})
	}
}

func TestSubagentProgressFinishedFields(t *testing.T) {
	raw := `{"sessionId":"p","update":{"sessionUpdate":"subagent_finished","subagent_id":"c","attempt_id":"a","child_session_id":"c","parent_session_id":"p","status":"failed","error":"boom","output":"out","duration_ms":2873,"tool_calls":1,"turns":2,"tokens_used":100,"tools_used":["list_dir"],"will_wake":true}}`
	n, ok, drop := parseSubagentNotification(json.RawMessage(raw))
	if !ok || drop {
		t.Fatal("must parse")
	}
	if n.Status != "failed" || n.Error != "boom" || n.Output != "out" || n.DurationMs != 2873 {
		t.Fatalf("fields %+v", n)
	}
	if n.ToolCalls != 1 || n.Turns != 2 || n.TokensUsed != 100 || len(n.ToolsUsed) != 1 || !n.WillWake {
		t.Fatalf("counters %+v", n)
	}
}

func TestProgressCountAliases(t *testing.T) {
	raw := `{"sessionId":"p","update":{"sessionUpdate":"subagent_progress","subagent_id":"c","attempt_id":"a","child_session_id":"c","turn_count":1,"tool_call_count":1,"tokens_used":4740,"tools_used":["list_dir"]}}`
	n, ok, drop := parseSubagentNotification(json.RawMessage(raw))
	if !ok || drop {
		t.Fatal("must parse")
	}
	if n.Turns != 1 || n.ToolCalls != 1 || n.TokensUsed != 4740 {
		t.Fatalf("progress aliases %+v", n)
	}
}

func TestSpawnedRegistersAndChildForwarded(t *testing.T) {
	p, cap := grokPipe(t)
	p.send(t, nil, MethodGrokSessionNotificationWrapped, spawnedParams(subTestParent, subTestChild, "at1"))
	p.send(t, nil, MethodSessionUpdate, childUpdateParams(subTestChild, UpdateAgentMessage, "hi"))
	// Main-session update keeps Child == "".
	p.send(t, nil, MethodSessionUpdate, childUpdateParams(subTestParent, UpdateAgentMessage, "main"))
	cap.waitFor(t, 2, 1)
	updates, subs := cap.snapshot()
	if subs[0].Kind != SubagentSpawned {
		t.Fatalf("kinds %v", subs)
	}
	byChild := map[string]SessionNotification{}
	for _, u := range updates {
		byChild[u.Child] = u
	}
	child, ok := byChild[subTestChild]
	if !ok {
		t.Fatalf("no routed child update in %v", updates)
	}
	var upd SessionUpdate
	if err := json.Unmarshal(child.Update, &upd); err != nil || upd.Content == nil || upd.Content.Text != "hi" {
		t.Fatalf("child update %+v %v", upd, err)
	}
	if _, ok := byChild[""]; !ok {
		t.Fatal("main update must forward with Child == \"\"")
	}
	if got := p.client.DroppedUpdates(); got != 0 {
		t.Fatalf("dropped %d", got)
	}
}

func TestPendingUpdatesFlushAfterSessionNew(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	cap := &notifyCapture{}
	p.client.SetUpdateHandler(cap.addUpdate)
	// No session yet: updates buffer.
	p.send(t, nil, MethodSessionUpdate, childUpdateParams("s-new", UpdateAgentMessage, "early"))
	time.Sleep(50 * time.Millisecond)
	p.client.mu.Lock()
	p.client.sessionID = "s-new"
	pending := p.client.pendingUpdates
	p.client.pendingUpdates = nil
	h := p.client.onUpdate
	p.client.mu.Unlock()
	flushSessionUpdates("s-new", pending, h)
	cap.waitFor(t, 1, 0)
	updates, _ := cap.snapshot()
	if updates[0].Child != "" {
		t.Fatalf("flushed main update must have Child == \"\", got %q", updates[0].Child)
	}
}

func TestNewSessionClearsChildAllowlist(t *testing.T) {
	p, cap := grokPipe(t)
	p.send(t, nil, MethodGrokSessionNotificationWrapped, spawnedParams(subTestParent, subTestChild, "at1"))
	cap.waitFor(t, 0, 1)
	done := make(chan error, 1)
	go func() {
		_, err := p.client.NewSession(t.Context(), t.TempDir())
		done <- err
	}()
	req := p.readWithin(t, 3*time.Second, "session/new")
	if req.Method != MethodSessionNew {
		t.Fatalf("method %q", req.Method)
	}
	raw, err := json.Marshal(map[string]string{"sessionId": "session-b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.enc.WriteMessage(&Message{JSONRPC: jsonrpcVersion, ID: req.ID, Result: raw}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("NewSession hung")
	}
	p.send(t, nil, MethodSessionUpdate, childUpdateParams(subTestChild, UpdateAgentMessage, "stale"))
	time.Sleep(100 * time.Millisecond)
	updates, _ := cap.snapshot()
	if len(updates) != 0 {
		t.Fatalf("stale child must not route after NewSession, got %v", updates)
	}
	if got := p.client.DroppedUpdates(); got < 1 {
		t.Fatalf("dropped %d", got)
	}
}

func TestMalformedNotificationCounted(t *testing.T) {
	p, cap := grokPipe(t)
	p.send(t, nil, MethodGrokSessionNotificationWrapped, `null`)
	time.Sleep(100 * time.Millisecond)
	if _, subs := cap.snapshot(); len(subs) != 0 {
		t.Fatalf("malformed must not reach the handler: %v", subs)
	}
	if got := p.client.DroppedUpdates(); got != 1 {
		t.Fatalf("dropped %d, want 1", got)
	}
}

func TestChildBeforeSpawnedDropped(t *testing.T) {
	p, cap := grokPipe(t)
	p.send(t, nil, MethodSessionUpdate, childUpdateParams(subTestChild, UpdateAgentMessage, "early"))
	time.Sleep(100 * time.Millisecond)
	if _, subs := cap.snapshot(); len(subs) != 0 {
		t.Fatal("no lifecycle expected")
	}
	updates, _ := cap.snapshot()
	if len(updates) != 0 {
		t.Fatalf("child-before-spawned must not forward, got %v", updates)
	}
	if got := p.client.DroppedUpdates(); got != 1 {
		t.Fatalf("dropped %d, want 1", got)
	}
}

func TestAfterFinishedDropped(t *testing.T) {
	p, cap := grokPipe(t)
	p.send(t, nil, MethodGrokSessionNotificationWrapped, spawnedParams(subTestParent, subTestChild, "at1"))
	p.send(t, nil, MethodGrokSessionNotificationWrapped, finishedParams(subTestParent, subTestChild, "completed"))
	cap.waitFor(t, 0, 2)
	p.send(t, nil, MethodSessionUpdate, childUpdateParams(subTestChild, UpdateAgentMessage, "late"))
	time.Sleep(100 * time.Millisecond)
	updates, subs := cap.snapshot()
	if len(updates) != 0 {
		t.Fatalf("after-finished must not forward, got %v", updates)
	}
	if subs[1].Kind != SubagentFinished {
		t.Fatalf("kinds %v", subs)
	}
	if got := p.client.DroppedUpdates(); got != 1 {
		t.Fatalf("dropped %d, want 1", got)
	}
}

func TestReRegisterAfterFinished(t *testing.T) {
	p, cap := grokPipe(t)
	p.send(t, nil, MethodGrokSessionNotificationWrapped, spawnedParams(subTestParent, subTestChild, "at1"))
	p.send(t, nil, MethodGrokSessionNotificationWrapped, finishedParams(subTestParent, subTestChild, "completed"))
	// New attempt re-registers the same child.
	p.send(t, nil, MethodGrokSessionNotificationWrapped, spawnedParams(subTestParent, subTestChild, "at2"))
	p.send(t, nil, MethodSessionUpdate, childUpdateParams(subTestChild, UpdateAgentMessage, "again"))
	cap.waitFor(t, 1, 3)
	updates, subs := cap.snapshot()
	if subs[2].AttemptID != "at2" {
		t.Fatalf("re-spawn %+v", subs[2])
	}
	if updates[0].Child != subTestChild {
		t.Fatalf("re-registered child must forward, got %+v", updates[0])
	}
}

func TestGrandchildSpawnedOnChildRegisters(t *testing.T) {
	p, cap := grokPipe(t)
	p.send(t, nil, MethodGrokSessionNotificationWrapped, spawnedParams(subTestParent, subTestChild, "at1"))
	// Outer id is the registered child: flat registration.
	p.send(t, nil, MethodGrokSessionNotificationWrapped, spawnedParams(subTestChild, "sub-1a", "at1"))
	p.send(t, nil, MethodSessionUpdate, childUpdateParams("sub-1a", UpdateAgentMessage, "nested"))
	cap.waitFor(t, 1, 2)
	updates, _ := cap.snapshot()
	if updates[0].Child != "sub-1a" {
		t.Fatalf("grandchild must route, got %+v", updates[0])
	}
}

func TestSixtyFifthChildRefusedNoneEvicted(t *testing.T) {
	p, cap := grokPipe(t)
	for i := 0; i < childRouteCap; i++ {
		p.send(t, nil, MethodGrokSessionNotificationWrapped, spawnedParams(subTestParent, fmt.Sprintf("sub-%d", i), "at1"))
	}
	cap.waitFor(t, 0, childRouteCap)
	// 65th: handler still runs (row can show) but the stream is not routed.
	p.send(t, nil, MethodGrokSessionNotificationWrapped, spawnedParams(subTestParent, "sub-65", "at1"))
	p.send(t, nil, MethodSessionUpdate, childUpdateParams("sub-65", UpdateAgentMessage, "refused"))
	p.send(t, nil, MethodSessionUpdate, childUpdateParams("sub-0", UpdateAgentMessage, "kept"))
	cap.waitFor(t, 1, childRouteCap+1)
	updates, subs := cap.snapshot()
	if subs[childRouteCap].ChildSessionID != "sub-65" {
		t.Fatalf("refused spawned must still reach the handler: %+v", subs[childRouteCap])
	}
	if len(updates) != 1 || updates[0].Child != "sub-0" {
		t.Fatalf("only the registered child routes, got %v", updates)
	}
	if got := p.client.DroppedUpdates(); got != 2 {
		t.Fatalf("dropped %d, want 2 (refused registration + refused stream)", got)
	}
}

func TestMissingChildSessionIDDropped(t *testing.T) {
	p, cap := grokPipe(t)
	p.send(t, nil, MethodGrokSessionNotificationWrapped, `{"sessionId":"parent-1","update":{"sessionUpdate":"subagent_spawned","subagent_id":"sub-1"}}`)
	time.Sleep(100 * time.Millisecond)
	if _, subs := cap.snapshot(); len(subs) != 0 {
		t.Fatalf("malformed spawned must not reach the handler: %v", subs)
	}
	if got := p.client.DroppedUpdates(); got != 1 {
		t.Fatalf("dropped %d, want 1", got)
	}
}

func TestMismatchedOuterParentRoutedByOuter(t *testing.T) {
	p, cap := grokPipe(t)
	// parent_session_id disagrees with the outer id; routing follows outer.
	raw := `{"sessionId":"parent-1","update":{"sessionUpdate":"subagent_spawned","subagent_id":"sub-1","attempt_id":"a","parent_session_id":"someone-else","child_session_id":"sub-1"}}`
	p.send(t, nil, MethodGrokSessionNotificationWrapped, raw)
	p.send(t, nil, MethodSessionUpdate, childUpdateParams(subTestChild, UpdateAgentMessage, "hi"))
	cap.waitFor(t, 1, 1)
}

func TestCursorIgnoresSessionNotification(t *testing.T) {
	p := newRawPipe(t) // cursor dialect
	p.setSession(subTestParent)
	seen := make(chan SubagentNotification, 1)
	p.client.SetSubagentHandler(func(n SubagentNotification) { seen <- n })
	got := make(chan SessionNotification, 4)
	p.client.SetUpdateHandler(func(n SessionNotification) { got <- n })
	p.send(t, nil, MethodGrokSessionNotificationWrapped, spawnedParams(subTestParent, subTestChild, "at1"))
	p.send(t, nil, MethodGrokSessionNotification, spawnedParams(subTestParent, subTestChild, "at1"))
	p.send(t, nil, MethodSessionUpdate, childUpdateParams(subTestChild, UpdateAgentMessage, "hi"))
	select {
	case n := <-seen:
		t.Fatalf("cursor client must ignore session_notification: %+v", n)
	case <-time.After(120 * time.Millisecond):
	}
	select {
	case n := <-got:
		t.Fatalf("cursor client must not route child updates: %+v", n)
	case <-time.After(120 * time.Millisecond):
	}
	if d := p.client.DroppedUpdates(); d != 1 {
		t.Fatalf("cursor drops the unrouted child update once, got %d", d)
	}
}

func TestForeignSessionDropped(t *testing.T) {
	p, cap := grokPipe(t)
	p.send(t, nil, MethodSessionUpdate, childUpdateParams("other-session", UpdateAgentMessage, "NOPE"))
	time.Sleep(100 * time.Millisecond)
	updates, _ := cap.snapshot()
	if len(updates) != 0 {
		t.Fatalf("foreign session must be dropped, got %v", updates)
	}
	if got := p.client.DroppedUpdates(); got != 1 {
		t.Fatalf("dropped %d, want 1", got)
	}
}

func TestSubagentHandlerOrder(t *testing.T) {
	p, cap := grokPipe(t)
	p.send(t, nil, MethodGrokSessionNotificationWrapped, spawnedParams(subTestParent, subTestChild, "at1"))
	p.send(t, nil, MethodSessionUpdate, childUpdateParams(subTestChild, UpdateAgentMessage, "one"))
	p.send(t, nil, MethodGrokSessionNotificationWrapped, progressParams(subTestParent, subTestChild))
	p.send(t, nil, MethodGrokSessionNotificationWrapped, finishedParams(subTestParent, subTestChild, "completed"))
	cap.waitFor(t, 1, 3)
	_, subs := cap.snapshot()
	want := []string{SubagentSpawned, SubagentProgress, SubagentFinished}
	for i, k := range want {
		if subs[i].Kind != k {
			t.Fatalf("order %v, want %v", subs, want)
		}
	}
}

func TestChildTurnCompletedDoesNotEndPrompt(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession("s1")
	done, _ := startGrokPrompt(t, p, "hi")
	// A child turn_completed on its own session id is not prompt_complete.
	p.send(t, nil, MethodGrokSessionNotificationWrapped, `{"sessionId":"sub-1","update":{"sessionUpdate":"turn_completed"}}`)
	select {
	case out := <-done:
		t.Fatalf("child turn_completed ended the prompt: %+v %v", out.res, out.err)
	case <-time.After(100 * time.Millisecond):
	}
	p.send(t, nil, MethodGrokPromptCompleteWrapped, `{"sessionId":"s1","stopReason":"end_turn"}`)
	if res := waitPrompt(t, done); res.StopReason != StopEndTurn {
		t.Fatalf("stop %q", res.StopReason)
	}
}

func TestToolNameNormalizedFromMeta(t *testing.T) {
	p, cap := grokPipe(t)
	p.send(t, nil, MethodSessionUpdate, `{"sessionId":"parent-1","update":{"sessionUpdate":"tool_call","toolCallId":"c","title":"spawn_subagent","_meta":{"x.ai/tool":{"name":"spawn_subagent","kind":"task"}}}}`)
	cap.waitFor(t, 1, 0)
	updates, _ := cap.snapshot()
	if updates[0].ToolName != "spawn_subagent" {
		t.Fatalf("ToolName %q", updates[0].ToolName)
	}
	// No _meta: empty, and cursor never stamps one.
	if got := grokToolName(json.RawMessage(`{"sessionUpdate":"tool_call"}`)); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
	q := newRawPipe(t)
	q.setSession("parent-1")
	seen := make(chan SessionNotification, 1)
	q.client.SetUpdateHandler(func(n SessionNotification) { seen <- n })
	q.send(t, nil, MethodSessionUpdate, `{"sessionId":"parent-1","update":{"sessionUpdate":"tool_call","toolCallId":"c","title":"t","_meta":{"x.ai/tool":{"name":"spawn_subagent"}}}}`)
	select {
	case n := <-seen:
		if n.ToolName != "" {
			t.Fatalf("cursor ToolName must stay empty, got %q", n.ToolName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("update never arrived")
	}
}

// TestNoChildEventsWithoutAllowlist is the §8 negative control: without any
// spawned registration the same wire carries nothing to either handler.
func TestNoChildEventsWithoutAllowlist(t *testing.T) {
	p := newRawPipeDialect(t, DialectGrok)
	p.setSession(subTestParent)
	updates := make(chan SessionNotification, 4)
	subs := make(chan SubagentNotification, 4)
	p.client.SetUpdateHandler(func(n SessionNotification) { updates <- n })
	p.client.SetSubagentHandler(func(n SubagentNotification) { subs <- n })
	p.send(t, nil, MethodSessionUpdate, childUpdateParams(subTestChild, UpdateAgentMessage, "hi"))
	p.send(t, nil, MethodSessionUpdate, childUpdateParams(subTestParent, UpdateAgentMessage, "main"))
	select {
	case n := <-updates:
		if n.Child != "" {
			t.Fatalf("no child may forward: %+v", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("main update must still forward")
	}
	select {
	case n := <-updates:
		t.Fatalf("child update forwarded without registration: %+v", n)
	case n := <-subs:
		t.Fatalf("lifecycle without wire notification: %+v", n)
	case <-time.After(120 * time.Millisecond):
	}
}

func TestSubagentFixturesParse(t *testing.T) {
	for _, name := range []string{"subagent.jsonl", "two.jsonl", "cancel.jsonl"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "grok-subagent", name))
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, l := range strings.Split(string(raw), "\n") {
			if l != "" {
				lines = append(lines, l)
			}
		}
		if len(lines) == 0 {
			t.Fatalf("%s: empty", name)
		}
		for _, l := range lines {
			var m struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			switch m.Method {
			case MethodSessionUpdate:
				var n SessionNotification
				if err := json.Unmarshal(m.Params, &n); err != nil || n.SessionID == "" {
					t.Fatalf("%s: bad session/update %s", name, l)
				}
			case MethodGrokSessionNotificationWrapped, MethodGrokSessionNotification:
				if _, ok, _ := parseSubagentNotification(m.Params); !ok {
					// cancel.jsonl carries the cancel tail (turn_completed,
					// prompt_complete, RPC reply): only lifecycle lines parse.
					var w struct {
						Update struct {
							SessionUpdate string `json:"sessionUpdate"`
						} `json:"update"`
					}
					if err := json.Unmarshal(unwrapExtParams(m.Params), &w); err != nil {
						t.Fatalf("%s: bad line %s", name, l)
					}
					switch w.Update.SessionUpdate {
					case SubagentSpawned, SubagentProgress, SubagentFinished:
						t.Fatalf("%s: lifecycle line must parse: %s", name, l)
					}
				}
			case MethodGrokPromptCompleteWrapped, MethodGrokPromptComplete:
			default:
				// The RPC reply line in cancel.jsonl has no method.
				var v map[string]any
				if err := json.Unmarshal([]byte(l), &v); err != nil {
					t.Fatalf("%s: bad line %s", name, l)
				}
			}
		}
	}
}

func waitForFinished(t *testing.T, cap *notifyCapture) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cap.mu.Lock()
		found := false
		for _, n := range cap.subagent {
			if n.Kind == SubagentFinished {
				found = true
				break
			}
		}
		cap.mu.Unlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("finished never arrived")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSubagentConcurrentHandlersRace(t *testing.T) {
	p, cap := grokPipe(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			child := fmt.Sprintf("race-%d", i)
			p.client.onNotify(&Message{JSONRPC: jsonrpcVersion, Method: MethodGrokSessionNotificationWrapped, Params: json.RawMessage(spawnedParams(subTestParent, child, "at1"))})
			p.client.onNotify(&Message{JSONRPC: jsonrpcVersion, Method: MethodSessionUpdate, Params: json.RawMessage(childUpdateParams(child, UpdateAgentMessage, "x"))})
		}(i)
	}
	wg.Wait()
	cap.waitFor(t, 8, 8)
	if got := p.client.DroppedUpdates(); got != 0 {
		t.Fatalf("dropped %d", got)
	}
}

// TestGrokSubagentScriptsEndToEnd runs every grok-subagent* fake through a
// real grok-dialect client: lifecycle reaches the subagent handler, child
// updates forward tagged, and the echo foreign other-session update is
// dropped. The cancel script drives session/cancel mid-turn.
func TestGrokSubagentScriptsEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name     string
		children []string
		cancel   bool
	}{
		{"grok-subagent", []string{"sub-1"}, false},
		{"grok-subagent-fail", []string{"sub-1"}, false},
		{"grok-subagent-two", []string{"sub-1", "sub-2"}, false},
		{"grok-subagent-nested", []string{"sub-1", "sub-1a"}, false},
		{"grok-subagent-late", []string{"sub-1"}, false},
		{"grok-subagent-cancel", []string{"sub-1"}, true},
		{"grok-subagent-cancel-early", []string{"sub-1"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := spawnScript(t, tc.name)
			cap := &notifyCapture{}
			c.SetUpdateHandler(cap.addUpdate)
			c.SetSubagentHandler(cap.addSubagent)
			ctx := t.Context()
			if _, err := c.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			if err := c.Authenticate(ctx, AuthCursorLogin, nil); err != nil {
				t.Fatal(err)
			}
			cwd := t.TempDir()
			res, err := c.NewSession(ctx, cwd)
			if err != nil {
				t.Fatal(err)
			}
			sid := res.SessionID
			// Drive session/prompt by hand so the test controls when the
			// prompt ends: U1 has no drain (that is U2), and Close would kill
			// the fake before the late 400 ms finish is sent.
			promptDone := make(chan struct{})
			go func() {
				defer close(promptDone)
				_, _ = c.Prompt(ctx, "run it")
			}()
			if tc.cancel {
				waitUntil(t, c.PromptInFlight)
				time.Sleep(500 * time.Millisecond)
				if err := c.Cancel(ctx); err != nil {
					t.Fatal(err)
				}
				select {
				case <-promptDone:
				case <-time.After(10 * time.Second):
					t.Fatal("cancelled prompt never returned")
				}
				// The non-early finish lands 400 ms after prompt_complete.
				deadline := time.Now().Add(5 * time.Second)
				for {
					cap.mu.Lock()
					n := len(cap.subagent)
					cap.mu.Unlock()
					if n >= 2 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("want spawned+finished, have %d lifecycle events", n)
					}
					time.Sleep(20 * time.Millisecond)
				}
			} else if tc.name == "grok-subagent-late" {
				// prompt_complete ends the prompt while the child still runs;
				// the late finish follows on the same conn.
				select {
				case <-promptDone:
				case <-time.After(10 * time.Second):
					t.Fatal("prompt never returned")
				}
				waitForFinished(t, cap)
			} else {
				select {
				case <-promptDone:
				case <-time.After(10 * time.Second):
					t.Fatal("prompt never returned")
				}
				cap.waitFor(t, 1, 2)
			}
			_ = sid
			updates, subs := cap.snapshot()
			if len(subs) < 2 || subs[0].Kind != SubagentSpawned {
				t.Fatalf("lifecycle %v", subs)
			}
			if subs[len(subs)-1].Kind != SubagentFinished {
				t.Fatalf("last lifecycle must be finished: %v", subs)
			}
			seenChild := map[string]bool{}
			for _, u := range updates {
				if u.Child != "" {
					seenChild[u.Child] = true
				}
				// The spawn tool's completed update carries content[]; only
				// chunk updates carry a single content block.
				var upd struct {
					Content *ContentBlock `json:"content"`
				}
				if err := json.Unmarshal(u.Update, &upd); err == nil && upd.Content != nil && upd.Content.Text == "NOPE" {
					t.Fatal("foreign other-session update forwarded")
				}
			}
			for _, want := range tc.children {
				if !seenChild[want] {
					t.Fatalf("no routed update for %s (saw %v)", want, seenChild)
				}
			}
		})
	}
}
