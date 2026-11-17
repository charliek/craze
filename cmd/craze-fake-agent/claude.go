package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// The claude scripts (craze plan 052 C2) stand in for claude-agent-acp
// 0.107.3, shaped after its real frames (internal/acp/testdata/claude-basic):
// initialize with no auth methods and the steering flag, the adapter's
// _auth/status_update after it, a session/new that applies its configOptions
// in order — skipping, silently as the adapter does, any it refuses — and
// answers {sessionId, modes, configOptions}, a session/load that does the same
// after a short replay, and session/set_config_option answered with the whole
// catalog. A prompt is echoed unless it carries a directive (claudePrompt).
//
//	claude-basic  the above; loadSession false
//	claude-load   the above; loadSession true, and session/new refused
//	claude-ask, claude-ask-odd, claude-plan, claude-plan-odd, claude-steer
//	              claude-basic with a card turn of their own (claude_cards.go)
//	claude-wake*, claude-subagent*, claude-child-ask, claude-held-bg-subagent,
//	claude-unattended-bg, claude-replay-load, claude-notice-idle,
//	claude-compaction-idle
//	              background work and children (claude_bg.go)
//
// Environment (each read when the frame it shapes is sent):
//
//	CRAZE_FAKE_CLAUDE_AUTH        the first _auth/status_update: account (the
//	                              default, plan max), api_key, enterprise,
//	                              oauth, none (kind none), or silent (no push)
//	CRAZE_FAKE_CLAUDE_AUTH_DELAY  how long after initialize it is sent (a Go
//	                              duration; default at once)
//	CRAZE_FAKE_CLAUDE_MODE        the mode a session starts in before its
//	                              configOptions (default auto, the adapter's)
//	CRAZE_FAKE_CLAUDE_FAST        on: the fast option starts on
//	CRAZE_FAKE_CLAUDE_STICKY      comma-separated config ids whose value
//	                              never changes: an initial option skipped, a
//	                              set answered with the catalog unchanged
//	CRAZE_FAKE_CLAUDE_SKIP_INITIAL comma-separated config ids skipped as
//	                              initial options only; a later set applies
//	CRAZE_FAKE_CLAUDE_REFUSE_MODEL a model value refused: skipped as an
//	                              initial option, -32602 to a set
//	CRAZE_FAKE_SET_REFUSE         a config id whose session/set_config_option
//	                              is refused -32602, and CRAZE_FAKE_SET_GATE,
//	                              as for the other scripts
//	CRAZE_FAKE_CLAUDE_LOAD_FAST_HIDDEN  1: a load turns fast mode on and pushes
//	                              it during the replay, and its reply — built
//	                              before — still says off
//	CRAZE_FAKE_DUMP_SETUP         every session/new and session/load params
//	                              object is appended to this file, one line
//	                              each
//
// Every claude script also answers `--version` with
// CRAZE_FAKE_ADAPTER_VERSION (default 0.107.3) and exits, as the adapter
// does, and writes CRAZE_FAKE_DUMP_ENV at startup (dumpEnv).

// fakeAdapterVersion is the claude-agent-acp version the scripts stand in for.
const fakeAdapterVersion = "0.107.3"

func claudeScript(script string) bool { return strings.HasPrefix(script, "claude-") }

// claudeState is a claude script's session: its options and mode.
type claudeState struct {
	mu   sync.Mutex
	opts []map[string]any
	// sid is the session the script streams on: session/new's, or the one
	// a load named.
	sid string
	// steer is the turn claude-steer holds open for a _session/steering,
	// under the server's mu; nil when none is (claude_cards.go).
	steer chan string
	// race is the prompt STEER-RACE holds, under the server's mu: the next
	// steer answers it first, then startedNewTurn (claude_bg.go).
	race json.RawMessage
}

func claudeSelect(id, name, category, current string, values ...string) map[string]any {
	opts := make([]map[string]string, 0, len(values))
	for _, v := range values {
		opts = append(opts, map[string]string{"value": v, "name": v})
	}
	m := map[string]any{"id": id, "name": name, "type": "select", "currentValue": current, "options": opts}
	if category != "" {
		m["category"] = category
	}
	return m
}

// claudeModes is the adapter's five modes, in its order.
var claudeModes = []string{"default", "acceptEdits", "plan", "auto", "bypassPermissions"}

func newClaudeState() *claudeState {
	mode := envOr("CRAZE_FAKE_CLAUDE_MODE", "auto")
	fast := "off"
	if os.Getenv("CRAZE_FAKE_CLAUDE_FAST") == "on" {
		fast = "on"
	}
	fastOpt := claudeSelect("fast", "Fast mode", "model_config", fast)
	fastOpt["options"] = []map[string]string{{"value": "on", "name": "On"}, {"value": "off", "name": "Off"}}
	return &claudeState{sid: fakeSessionID, opts: []map[string]any{
		claudeSelect("mode", "Mode", "mode", mode, claudeModes...),
		claudeSelect("model", "Model", "model", "opus", "default", "opus", "sonnet", "haiku"),
		claudeSelect("effort", "Effort", "thought_level", "high", "default", "low", "medium", "high", "xhigh", "max"),
		fastOpt,
		claudeSelect("ultracode", "Ultracode", "model_config", "off", "on", "off"),
	}}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func sticky(id string) bool {
	return slices.Contains(strings.Split(os.Getenv("CRAZE_FAKE_CLAUDE_STICKY"), ","), id)
}

// set applies one option value the adapter's way: an unknown id, a value the
// option does not offer, a sticky option or the refused model is not applied
// (refused is true for the last two of those the client should hear about).
func (c *claudeState) set(id, value string) (applied, refused bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == "model" && value == os.Getenv("CRAZE_FAKE_CLAUDE_REFUSE_MODEL") {
		return false, true
	}
	for _, o := range c.opts {
		if o["id"] != id {
			continue
		}
		vs, _ := o["options"].([]map[string]string)
		if !slices.ContainsFunc(vs, func(v map[string]string) bool { return v["value"] == value }) {
			return false, true
		}
		if sticky(id) {
			return false, false
		}
		o["currentValue"] = value
		return true, false
	}
	return false, true
}

// force sets an option's value whatever CRAZE_FAKE_CLAUDE_STICKY says: the
// adapter's own change (FAST-ON), not a client's set.
func (c *claudeState) force(id, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, o := range c.opts {
		if o["id"] == id {
			o["currentValue"] = value
		}
	}
}

func (c *claudeState) current(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, o := range c.opts {
		if o["id"] == id {
			v, _ := o["currentValue"].(string)
			return v
		}
	}
	return ""
}

// catalog is the options as the wire carries them.
func (c *claudeState) catalog() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, 0, len(c.opts))
	for _, o := range c.opts {
		cp := make(map[string]any, len(o))
		for k, v := range o {
			cp[k] = v
		}
		out = append(out, cp)
	}
	return out
}

func (c *claudeState) setupResult(sid string) map[string]any {
	avail := make([]map[string]string, 0, len(claudeModes))
	for _, m := range claudeModes {
		avail = append(avail, map[string]string{"id": m, "name": m})
	}
	return map[string]any{
		"sessionId":     sid,
		"modes":         map[string]any{"currentModeId": c.current("mode"), "availableModes": avail},
		"configOptions": c.catalog(),
	}
}

// claudeAuthStatus is the _auth/status_update payload for spec.
func claudeAuthStatus(spec string) map[string]any {
	var st map[string]any
	switch spec {
	case "api_key":
		st = map[string]any{"kind": "api_key", "label": "Anthropic API key", "detail": "apiKeyHelper"}
	case "enterprise":
		st = map[string]any{"kind": "account", "label": "Claude Enterprise", "account": map[string]any{"plan": "enterprise"}}
	case "oauth":
		st = map[string]any{"kind": "account", "label": "Claude subscription", "detail": "OAuth token"}
	case "none":
		st = map[string]any{"kind": "none", "label": "Not signed in"}
	default:
		st = map[string]any{"kind": "account", "label": "Claude Max", "account": map[string]any{"plan": "max"}}
	}
	return map[string]any{"authStatus": st}
}

func (s *server) sendAuthStatus(spec string) {
	_ = s.conn.Notify(context.Background(), acp.MethodClaudeAuthStatus, claudeAuthStatus(spec))
}

// onClaudeRequest is the claude scripts' request handler.
func (s *server) onClaudeRequest(msg *acp.Message) {
	switch msg.Method {
	case acp.MethodInitialize:
		s.reply(msg.ID, map[string]any{
			"protocolVersion": acp.ProtocolVersion,
			"agentInfo":       map[string]string{"name": "@agentclientprotocol/claude-agent-acp", "title": "Claude Agent", "version": envOr("CRAZE_FAKE_ADAPTER_VERSION", fakeAdapterVersion)},
			"authMethods":     []any{},
			"agentCapabilities": map[string]any{
				"loadSession":        s.script == "claude-load" || s.script == "claude-replay-load",
				"promptCapabilities": map[string]any{"image": true, "embeddedContext": true},
			},
			"_meta": map[string]any{"steering": map[string]any{"supported": true}},
		})
		spec := envOr("CRAZE_FAKE_CLAUDE_AUTH", "account")
		if spec == "silent" {
			return
		}
		if d := envDuration("CRAZE_FAKE_CLAUDE_AUTH_DELAY", 0); d > 0 {
			go func() { time.Sleep(d); s.sendAuthStatus(spec) }()
			return
		}
		s.sendAuthStatus(spec)
	case acp.MethodSessionNew:
		dumpSetup(msg.Params)
		if s.script == "claude-load" || s.script == "claude-replay-load" {
			_ = s.conn.ReplyErr(msg.ID, &acp.RPCError{Code: -32000, Message: "session/new must not be called"})
			return
		}
		s.claude.applyInitial(msg.Params)
		s.claudeSetupTraffic(fakeSessionID, false)
		s.reply(msg.ID, s.claude.setupResult(fakeSessionID))
		s.advertiseCommands()
		s.claudeSetupTraffic(fakeSessionID, true)
	case acp.MethodSessionLoad:
		dumpSetup(msg.Params)
		if s.script != "claude-load" && s.script != "claude-replay-load" {
			_ = s.conn.ReplyErr(msg.ID, acp.MethodNotFound(msg.Method))
			return
		}
		var p acp.LoadSessionParams
		_ = json.Unmarshal(msg.Params, &p)
		s.mu.Lock()
		s.loadedID = p.SessionID
		s.mu.Unlock()
		s.claude.sid = p.SessionID
		// The replay: the user's prompt and the reply, as the adapter replays.
		s.update(p.SessionID, map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]string{"type": "text", "text": "hello"}})
		s.update(p.SessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "hi there"}})
		if s.script == "claude-replay-load" {
			// A whole cycle in the replay: a tool call, its close, and the
			// result's usage — none of it a turn of today's.
			s.toolOn(p.SessionID, "toolu_replayed", acp.UpdateToolCall, "in_progress")
			s.toolOn(p.SessionID, "toolu_replayed", acp.UpdateToolCallUpd, "completed")
			s.usage(p.SessionID, true)
		}
		// A load restores the transcript's last mode before its options.
		s.claude.set("mode", envOr("CRAZE_FAKE_CLAUDE_MODE", "bypassPermissions"))
		s.claude.applyInitial(msg.Params)
		res := s.claude.setupResult(p.SessionID)
		if os.Getenv("CRAZE_FAKE_CLAUDE_LOAD_FAST_HIDDEN") == "1" {
			// The adapter turns fast mode on and says so while the replay is
			// still running, and its load reply, built a moment earlier, still
			// says off: the session is on, and the reply hides it.
			s.claude.force("fast", "on")
			s.update(p.SessionID, map[string]any{"sessionUpdate": acp.UpdateConfigOption, "configOptions": s.claude.catalog()})
		}
		s.reply(msg.ID, res)
		if os.Getenv("CRAZE_FAKE_CLAUDE_WAKE_AFTER_LOAD") == "1" {
			// Live, right behind the reply (claude_bg.go).
			s.wake(p.SessionID, "a live wake")
		}
	case acp.MethodSessionPrompt:
		dumpPrompt(msg.Params)
		s.cancelled.Store(false)
		go s.claudePrompt(msg)
	case acp.MethodClaudeSteering:
		s.onClaudeSteer(msg)
	case acp.MethodSessionSetConfig:
		// CRAZE_FAKE_SET_GATE and CRAZE_FAKE_SET_REFUSE as for every script
		// (setGated).
		s.setGated(msg, s.claudeSetConfig)
	default:
		_ = s.conn.ReplyErr(msg.ID, acp.MethodNotFound(msg.Method))
	}
}

// claudeSetConfig is session/set_config_option: the value applied the
// adapter's way, answered with the whole catalog.
func (s *server) claudeSetConfig(msg *acp.Message) {
	var p acp.SetConfigParams
	_ = json.Unmarshal(msg.Params, &p)
	if _, refused := s.claude.set(p.ConfigID, p.Value); refused {
		_ = s.conn.ReplyErr(msg.ID, pmInvalidParams("Invalid value for config option "+p.ConfigID+": "+p.Value))
		return
	}
	s.reply(msg.ID, map[string]any{"configOptions": s.claude.catalog()})
	if p.ConfigID == "mode" {
		s.update(s.claude.sid, acp.SessionUpdate{SessionUpdate: acp.UpdateCurrentMode, CurrentModeID: s.claude.current("mode")})
	}
}

// applyInitial applies a setup request's configOptions in order, skipping
// what it refuses without a word (initial-config-options.js).
func (c *claudeState) applyInitial(params json.RawMessage) {
	var p struct {
		ConfigOptions []acp.ConfigOptionValue `json:"configOptions"`
	}
	_ = json.Unmarshal(params, &p)
	skip := strings.Split(os.Getenv("CRAZE_FAKE_CLAUDE_SKIP_INITIAL"), ",")
	for _, e := range p.ConfigOptions {
		if slices.Contains(skip, e.ConfigID) {
			continue
		}
		c.set(e.ConfigID, e.Value)
	}
}

func dumpSetup(params json.RawMessage) {
	p := os.Getenv("CRAZE_FAKE_DUMP_SETUP")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(append([]byte(nil), params...), '\n'))
}

// claudePrompt answers one prompt. Directives, anywhere in its text:
//
//	STOP <reason>  end the turn with that stop reason
//	WRITE          a Write tool call with a permission round trip, the
//	               adapter's options (allow-once, allow-with-updates, reject);
//	               the reply names the option chosen
//	WRITE-NOONCE   the same with no allow_once option
//	FAST-ON        a config_option_update turning fast mode on
//	AUTH-LATER     an api_key _auth/status_update, then hold until cancelled
//	TOOL-FAIL      a tool call left open, then the prompt answered -32603
//	TOOL-EXIT      a tool call left open, then the process exits
//	STEER-RACE     hold until a _session/steering, which ends this turn and
//	               is answered startedNewTurn (claude_bg.go)
//	HANG           hold until cancelled
//
// Anything else is echoed.
func (s *server) claudePrompt(msg *acp.Message) {
	text := promptText(msg.Params)
	sid := s.claude.sid
	say := func(t string) {
		s.update(sid, acp.SessionUpdate{SessionUpdate: acp.UpdateAgentMessage, Content: &acp.ContentBlock{Type: "text", Text: t}})
	}
	fields := strings.Fields(text)
	switch {
	case len(fields) == 2 && fields[0] == "STOP":
		say("stopping")
		s.reply(msg.ID, map[string]any{"stopReason": fields[1]})
	case strings.Contains(text, "WRITE"):
		s.claudeWrite(msg.ID, sid, !strings.Contains(text, "WRITE-NOONCE"))
	case strings.Contains(text, "FAST-ON"):
		s.claude.force("fast", "on")
		s.update(sid, map[string]any{"sessionUpdate": acp.UpdateConfigOption, "configOptions": s.claude.catalog()})
		say("fast on")
		s.reply(msg.ID, map[string]any{"stopReason": acp.StopEndTurn})
	case strings.Contains(text, "STEER-RACE"):
		// Held for the steer that answers it (claudeStartedTurn).
		s.mu.Lock()
		s.claude.race = msg.ID
		s.mu.Unlock()
	case strings.Contains(text, "HANG"):
		s.hang(msg.ID)
	case strings.Contains(text, "AUTH-LATER"):
		say("working")
		s.sendAuthStatus("api_key")
		s.hang(msg.ID)
	case strings.Contains(text, "TOOL-FAIL"), strings.Contains(text, "TOOL-EXIT"):
		s.update(sid, map[string]any{"sessionUpdate": acp.UpdateToolCall, "toolCallId": "toolu_open", "title": "Terminal", "kind": "execute", "status": "in_progress",
			"_meta": map[string]any{"claudeCode": map[string]any{"toolName": "Bash"}}})
		if strings.Contains(text, "TOOL-EXIT") {
			time.Sleep(50 * time.Millisecond)
			os.Exit(3)
		}
		_ = s.conn.ReplyErr(msg.ID, &acp.RPCError{Code: -32603, Message: "Internal error", Data: json.RawMessage(`{"errorKind":"server_error"}`)})
	case s.claudeBGTurn(msg.ID, sid, text):
		// A background or children script's own turn (claude_bg.go).
	case s.claudeCardTurn(msg.ID, sid, text):
		// A card script's own turn (claude_cards.go).
	default:
		say("echo: " + text)
		s.reply(msg.ID, map[string]any{"stopReason": acp.StopEndTurn})
	}
}

// claudeWrite is a Write call as the adapter streams it (probe2's write
// step): the tool_call, the update naming the file, the permission request,
// and the result once allowed.
func (s *server) claudeWrite(id json.RawMessage, sid string, once bool) {
	const call = "toolu_write_1"
	path := "out.txt"
	if wd, err := os.Getwd(); err == nil {
		path = wd + "/out.txt"
	}
	meta := map[string]any{"claudeCode": map[string]any{"toolName": "Write"}}
	s.update(sid, map[string]any{"_meta": meta, "toolCallId": call, "sessionUpdate": acp.UpdateToolCall, "name": "Write",
		"rawInput": map[string]any{}, "status": "pending", "title": "Preparing file…", "kind": "edit", "content": []any{}, "locations": []any{}})
	s.update(sid, map[string]any{"_meta": meta, "toolCallId": call, "sessionUpdate": acp.UpdateToolCallUpd,
		"rawInput": map[string]any{"file_path": path, "content": "ok"}, "title": "Write out.txt",
		"locations": []map[string]string{{"path": path}},
		"content":   []map[string]any{{"type": "diff", "path": path, "oldText": nil, "newText": "ok"}}})
	options := []acp.PermissionOption{
		{OptionID: "allow-once", Name: "Yes", Kind: acp.KindAllowOnce},
		{OptionID: "allow-with-updates", Name: "Yes, allow all edits during this session", Kind: acp.KindAllowAlways},
		{OptionID: "reject", Name: "No", Kind: acp.KindRejectOnce},
	}
	if !once {
		options = options[1:]
	}
	params := map[string]any{
		"sessionId": sid,
		"toolCall": map[string]any{"toolCallId": call, "name": "Write", "status": "pending", "title": "Write out.txt", "kind": "edit",
			"rawInput": map[string]any{"file_path": path, "content": "ok"}, "locations": []map[string]string{{"path": path}}},
		"options": options,
	}
	var result struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	err := s.conn.Call(context.Background(), acp.MethodRequestPermission, params, &result)
	if err != nil || result.Outcome.Outcome == "cancelled" {
		s.reply(id, map[string]any{"stopReason": acp.StopCancelled})
		return
	}
	if result.Outcome.OptionID == "reject" {
		s.update(sid, map[string]any{"_meta": meta, "toolCallId": call, "sessionUpdate": acp.UpdateToolCallUpd, "status": "failed"})
	} else {
		s.update(sid, map[string]any{"_meta": meta, "toolCallId": call, "sessionUpdate": acp.UpdateToolCallUpd, "status": "completed",
			"rawOutput": "File created successfully at: " + path})
	}
	s.update(sid, acp.SessionUpdate{SessionUpdate: acp.UpdateAgentMessage, Content: &acp.ContentBlock{Type: "text", Text: "permission: " + result.Outcome.OptionID}})
	s.reply(id, map[string]any{"stopReason": acp.StopEndTurn})
}

// envDumpValues are the variables CRAZE_FAKE_DUMP_ENV writes with their
// value; every other is written by name alone, so a test can prove a
// variable absent without a secret ever reaching the file.
var envDumpValues = []string{"CRAZE_SESSION_PROVIDER", "CLAUDE_AGENT_ACP_PROTOCOL", "CLAUDE_CODE_EXECUTABLE"}

// dumpEnv writes the agent's environment to CRAZE_FAKE_DUMP_ENV at startup,
// sorted, one variable a line: NAME=value for envDumpValues, NAME for the
// rest.
func dumpEnv() {
	p := os.Getenv("CRAZE_FAKE_DUMP_ENV")
	if p == "" {
		return
	}
	var lines []string
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if slices.Contains(envDumpValues, k) {
			lines = append(lines, k+"="+v)
			continue
		}
		lines = append(lines, k)
	}
	sort.Strings(lines)
	_ = os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// adapterVersion is a claude script's --version: the adapter's version and
// exit, as claude-agent-acp answers it.
func adapterVersion() {
	fmt.Println(envOr("CRAZE_FAKE_ADAPTER_VERSION", fakeAdapterVersion))
	os.Exit(0)
}
