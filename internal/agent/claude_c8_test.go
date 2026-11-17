package agent

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// The C8 fixes (plan 052, the branch-level review): each test drives the exact
// sequence its finding names, and fails on the code before its fix.

func claudeCatalogJSON(fast string, withFast bool) json.RawMessage {
	opts := []map[string]any{
		{"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": "opus",
			"options": []map[string]string{{"value": "opus", "name": "opus"}, {"value": "haiku", "name": "haiku"}}},
	}
	if withFast {
		opts = append(opts, map[string]any{"id": "fast", "name": "Fast mode", "category": "model_config", "type": "select", "currentValue": fast,
			"options": []map[string]string{{"value": "on", "name": "On"}, {"value": "off", "name": "Off"}}})
	}
	raw, _ := json.Marshal(opts)
	return raw
}

// TestClaudeFastOnInASettingsReplyIsReset (C8 K4): a reset that ran while a
// model switch hid the fast option ends — the option is absent — and the
// switch back's reply returns fast on: that reply starts a reset of its own,
// so nothing is left believing fast is off.
func TestClaudeFastOnInASettingsReplyIsReset(t *testing.T) {
	s, _ := claudeUnit(t)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	t.Cleanup(func() { _ = cr.Close(); _ = cw.Close(); _ = sr.Close(); _ = sw.Close() })
	client := acp.DialWithDialect(cr, cw, acp.DialectClaude)
	t.Cleanup(func() { _ = client.Close() })
	client.SetSettingsHandler(s.onSettingsReply)
	s.mu.Lock()
	s.client = client
	// A started session: its install is done.
	s.installPending = false
	s.snap.Config = parseConfigOptions(claudeCatalogJSON("off", true))
	s.mu.Unlock()
	enc, dec := acp.NewEncoder(sw), acp.NewDecoder(sr)
	readSet := func(what string) *acp.Message {
		t.Helper()
		got := make(chan *acp.Message, 1)
		go func() {
			if m, err := dec.ReadMessage(); err == nil {
				got <- m
			}
		}()
		select {
		case m := <-got:
			if m.Method != acp.MethodSessionSetConfig || !strings.Contains(string(m.Params), `"value":"off"`) {
				t.Fatalf("%s: wrote %s %s", what, m.Method, m.Params)
			}
			return m
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no fast-off set was written", what)
		}
		return nil
	}
	// A fast-on update starts a reset; its set's reply comes back from a model
	// with no fast option at all: the reset ends.
	raw, _ := json.Marshal(map[string]any{"sessionUpdate": acp.UpdateConfigOption, "configOptions": json.RawMessage(claudeCatalogJSON("on", true))})
	s.onUpdate(acp.SessionNotification{SessionID: "root", Update: raw})
	set := readSet("the first reset")
	if err := enc.WriteMessage(&acp.Message{JSONRPC: "2.0", ID: set.ID, Result: json.RawMessage(`{"configOptions":` + string(claudeCatalogJSON("", false)) + `}`)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first reset to end", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.fastFix == nil
	})
	// The switch back: its reply carries fast on.
	if err := s.onSettingsReply(acp.SettingsReply{Method: acp.MethodSessionSetConfig, ConfigID: "model", Value: "opus", ModelChange: true,
		Catalog: acp.ConfigCatalog{Present: true, Options: claudeCatalogJSON("on", true)}}); err != nil {
		t.Fatal(err)
	}
	readSet("the reply's reset")
}

// TestClaudeAnAgentGoneEndsItsForeignTurn (C8 K2): the adapter exits during a
// wake with a tool call open; the turn ends failed — the agent stopped — at
// once, not after the silence rule, and its tool call fails with it.
func TestClaudeAnAgentGoneEndsItsForeignTurn(t *testing.T) {
	shortForeign(t, 30*time.Second)
	r, log := startClaude(t, "claude-wake-nousage", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "BG EXIT"); err != nil {
		t.Fatal(err)
	}
	b := log.waitBrackets(t, 2)
	if b[1].Running || b[1].Err == nil || !strings.Contains(b[1].Err.Message, agentStopped) {
		t.Fatalf("the ending %+v; want failed, the agent stopped", b[1])
	}
	waitFor(t, "the dying wake's tool to fail", func() bool {
		for _, tl := range r.s.Snapshot().Tools {
			if tl.ID == "toolu_dying" {
				return tl.Status == toolFailed
			}
		}
		return false
	})
}

// TestClaudeAChildAdmittedAfterARefusalRoutes (C8 K3): a child the cap refused
// has no tool store; once the registry admits it, the row routes — its tool
// store and its transcript flag come back, on the event stream too — and its
// tool calls land.
func TestClaudeAChildAdmittedAfterARefusalRoutes(t *testing.T) {
	s, log := claudeUnit(t)
	refused := claudeChildUpdate("c65", acp.ChildRunning, nil)
	refused.Unrouted = true
	s.onSubagent(refused)
	if r, _ := claudeRowOf(s, "c65"); r.Transcript {
		t.Fatalf("a refused child claims a transcript: %+v", r)
	}
	s.onSubagent(claudeChildUpdate("c65", acp.ChildRunning, nil))
	if r, _ := claudeRowOf(s, "c65"); !r.Transcript {
		t.Fatalf("an admitted child claims no transcript: %+v", r)
	}
	raw, _ := json.Marshal(map[string]any{"sessionUpdate": acp.UpdateToolCall, "toolCallId": "ct", "status": "in_progress", "title": "Read", "kind": "read"})
	s.onUpdate(acp.SessionNotification{SessionID: "c65", Child: "c65", Update: raw})
	waitFor(t, "the admitted child's tool", func() bool {
		for _, ev := range log.snapshot() {
			if ev.Type == EventTool && ev.Agent == "c65" && ev.Tool != nil && ev.Tool.ID == "ct" {
				return true
			}
		}
		return false
	})
	// And a consumer of the stream sees it (review C8 item 2): the row it
	// folds from the events — the last one for c65, every one of them in by
	// the tool event that follows — says so too.
	var folded *SubagentInfo
	for _, ev := range log.snapshot() {
		if ev.Type == EventSubagent && ev.Subagent != nil && ev.Subagent.ID == "c65" {
			folded = ev.Subagent
		}
	}
	if folded == nil || !folded.Transcript {
		t.Fatalf("the stream's c65 row %+v; want the readmission's transcript flag", folded)
	}
}

// TestClaudeAFailedPromptClosesOnlyItsOwnTools (C8 K7): a prompted turn that
// fails closes the tool calls it opened, never one the agent's own turn —
// opened after the reply — still runs.
func TestClaudeAFailedPromptClosesOnlyItsOwnTools(t *testing.T) {
	s, _ := claudeUnit(t)
	tool := func(id string) {
		raw, _ := json.Marshal(map[string]any{"sessionUpdate": acp.UpdateToolCall, "toolCallId": id, "status": "in_progress", "title": id, "kind": "execute"})
		s.onUpdate(acp.SessionNotification{SessionID: "root", Update: raw})
	}
	s.mu.Lock()
	s.inPrompt, s.turn = true, 1
	s.mu.Unlock()
	tool("prompted")
	s.onForeignTurn(ForeignTurnInfo{ID: "claude-1", Reason: ReasonBackground, Running: true})
	tool("foreign")
	s.closeInFlightTools(toolFailed, 1)
	status := map[string]string{}
	for _, tl := range s.Snapshot().Tools {
		status[tl.ID] = tl.Status
	}
	if status["prompted"] != toolFailed || status["foreign"] != "in_progress" {
		t.Fatalf("tool statuses %v; want the prompt's failed and the foreign turn's running", status)
	}
}

// blockingWriter blocks every write until released.
type blockingWriter struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

// TestClaudeTheSilenceNoteNeverBlocksDispatch (C8 K5): the silence ending's
// diagnostic is written off the dispatch goroutine: a sink that blocks does
// not hold the ending, nor the dispatch lock the timer holds while it runs.
func TestClaudeTheSilenceNoteNeverBlocksDispatch(t *testing.T) {
	w := &blockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(w.release) })
	p := ClaudeProvider()
	s := newSession(Options{Provider: &p, Diag: w})
	t.Cleanup(func() { _ = s.Close() })
	log := collect(t, s)
	done := make(chan struct{})
	go func() {
		s.onACPForeignTurn(acp.ForeignTurn{ID: "claude-1", Reason: acp.ForeignReasonBackground, Silent: true})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the silence ending waited on its diagnostic's sink")
	}
	<-w.entered
	log.waitBrackets(t, 1)
}
