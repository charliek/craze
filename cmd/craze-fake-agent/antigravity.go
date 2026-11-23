package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// The antigravity scripts (craze plan 053 § B6) stand in for agy 1.5.14's
// `agy acp`, shaped after its real frames (internal/acp/testdata/
// antigravity-*; the probe's frames-*.jsonl): four auth methods craze never
// calls authenticate for, string ids on every request the agent sends, a
// session/new answering a legacy models block of suffixed ids beside the
// model, thought_level and mode config options, session/set_config_option
// answered with the whole catalog after a config_option_update (a suffixed
// model split into model and level; a level the model lacks refused -32602),
// and a session/load that replays the transcript and comes back on the
// default model and level.
//
//	antigravity-basic      the above; a prompt's directive (agyTurn) shapes its
//	                       turn, anything else is echoed
//	antigravity-loggedout  session/new and session/load answer -32000
//	                       "Authentication required", as a logged-out agy does
//
// A prompt's directives, the first word of its text:
//
//	ASK, ASK2       ask_question and its elicitation/create, one question or
//	                two; says "ask answer: <reply>"
//	RUN <kind>      run_command: header (the default), crlf, noheader, empty,
//	                literal ("Command output:" later in the output), exit2;
//	                a session/request_permission first when the argv has no
//	                --dangerously-skip-permissions, saying "permission: <reply>"
//	SLEEP           run_command held until session/cancel; the turn ends
//	                cancelled and the call failed with an empty output
//	EDITS           edit_file and write_file in the workspace, a write that
//	                fails, and a write into another session's brain directory
//	PLAN            the plan file (RequestFeedback true) and the todo list
//	                (RequestFeedback false) written into this session's brain
//	SUBAGENTS       two invoke_subagent calls, each completed at once
//	SUBAGENT <how>  one invoke_subagent, then: fail (the call fails), hold (the
//	                turn waits for session/cancel), error (the prompt fails
//	                -32603), die (the agent exits), late (a completed update
//	                for the call after the turn's reply)
//	AUTH            the prompt fails -32000 "Authentication required"
//	ERR32000        the prompt fails -32000 "Internal error"
//
// Environment:
//
//	CRAZE_FAKE_AGY_REFUSE      comma-separated values a set refuses -32602:
//	                           model ids or thought levels
//	CRAZE_FAKE_AGY_PUSH_AFTER  1: a set's config_option_update follows its
//	                           reply instead of preceding it
//	CRAZE_FAKE_AGY_REPLAY_RUNNING  1: a load's replay also carries an
//	                           invoke_subagent still in progress
//	CRAZE_FAKE_AGY_PUSH_ON_NEW  a model id: session/new's catalog gains it in
//	                           an update sent ahead of the reply, which
//	                           still lists the nine
//	CRAZE_FAKE_AGY_EXIT_AFTER_LOAD  a Go duration: the agent exits that long
//	                           after answering session/load
//	CRAZE_FAKE_SET_REFUSE, CRAZE_FAKE_SET_GATE as for every script

func antigravityScript(script string) bool { return strings.HasPrefix(script, "antigravity-") }

// agyStaleID is a session agy no longer has: its load answers an empty session
// and no error, as agy's does for any id it does not know (probe § 6).
const agyStaleID = "stale-session"

// agyModel is one entry of the fake's catalog: its base id, its name and the
// thought levels it offers (none for gpt-oss and the orchestrators).
type agyModel struct {
	id, name string
	levels   []string
}

// agyModels are agy 1.5.14's nine base models, in its order (frames-01).
var agyModels = []agyModel{
	{"gemini-4-neon", "Gemini 4 Neon", []string{"low", "medium", "high", "xhigh"}},
	{"gemini-4-argon", "Gemini 4 Argon", []string{"low", "medium", "high"}},
	{"gemini-4-hydrogen", "Gemini 4 Hydrogen", []string{"low", "medium", "high", "xhigh"}},
	{"gemini-pro-orchestrator-neon", "Orchestrator (Neon)", nil},
	{"gemini-pro-orchestrator-g1-pro", "Orchestrator (Argon)", nil},
	{"gemini-3.8-flash", "Gemini 3.8 Flash", []string{"low", "medium", "high"}},
	{"claude-opus-5-5", "Claude Opus 5.5", []string{"low", "medium", "high"}},
	{"claude-sonnet-5-5", "Claude Sonnet 5.5", []string{"low", "medium", "high"}},
	{"gpt-oss-120b-medium", "GPT-OSS 120B (Medium)", nil},
}

var agyLevelNames = map[string]string{"low": "Low", "medium": "Medium", "high": "High", "xhigh": "Extra"}

func agyModelByID(id string) (agyModel, bool) {
	i := slices.IndexFunc(agyModels, func(m agyModel) bool { return m.id == id })
	if i < 0 {
		return agyModel{}, false
	}
	return agyModels[i], true
}

// defaultLevel is the level a switch to m lands on: high on the two that offer
// xhigh, medium on the rest (frames-03).
func (m agyModel) defaultLevel() string {
	if len(m.levels) == 0 {
		return ""
	}
	if slices.Contains(m.levels, "xhigh") {
		return "high"
	}
	return "medium"
}

// agyModes are agy's four modes.
var agyModes = []map[string]string{
	{"id": "default", "name": "Default", "description": "Ask before edits and commands, per your permission settings."},
	{"id": "accept-edits", "name": "Accept edits"},
	{"id": "auto", "name": "Auto", "description": "Approve edits and safe commands; ask before irreversible actions."},
	{"id": "plan", "name": "Plan", "description": "Plan carefully before executing a task."},
}

// agyState is an antigravity script's session.
type agyState struct {
	mu                 sync.Mutex
	sid                string
	model, level, mode string
	// extra is a model the catalog offers beyond agy's nine, once
	// CRAZE_FAKE_AGY_PUSH_ON_NEW has added it.
	extra string
}

func newAgyState() *agyState {
	return &agyState{sid: fakeSessionID, model: "gemini-4-neon", level: "high", mode: "default"}
}

// catalog is the configOptions list as agy sends it: model, thought_level
// (absent for a model without levels) and mode.
func (a *agyState) catalog() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	models := make([]map[string]string, 0, len(agyModels))
	for _, m := range agyModels {
		models = append(models, map[string]string{"value": m.id, "name": m.name})
	}
	if a.extra != "" {
		models = append(models, map[string]string{"value": a.extra, "name": "Extra " + a.extra})
	}
	out := []map[string]any{{"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": a.model, "options": models}}
	if m, _ := agyModelByID(a.model); len(m.levels) > 0 {
		levels := make([]map[string]string, 0, len(m.levels))
		for _, l := range m.levels {
			levels = append(levels, map[string]string{"value": l, "name": agyLevelNames[l]})
		}
		out = append(out, map[string]any{"id": "thought_level", "name": "Thinking Level", "category": "thought_level", "type": "select", "currentValue": a.level, "options": levels})
	}
	modes := make([]map[string]string, 0, len(agyModes))
	for _, m := range agyModes {
		modes = append(modes, map[string]string{"value": m["id"], "name": m["name"]})
	}
	return append(out, map[string]any{"id": "mode", "name": "Mode", "category": "mode", "type": "select", "currentValue": a.mode, "options": modes})
}

// legacyModels is the models block: one suffixed id per model and level.
func (a *agyState) legacyModels() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	var avail []map[string]string
	for _, m := range agyModels {
		if len(m.levels) == 0 {
			avail = append(avail, map[string]string{"modelId": m.id, "name": m.name})
			continue
		}
		for _, l := range m.levels {
			avail = append(avail, map[string]string{"modelId": m.id + "-" + l, "name": m.name + " (" + agyLevelNames[l] + ")"})
		}
	}
	current := a.model
	if a.level != "" {
		current += "-" + a.level
	}
	return map[string]any{"currentModelId": current, "availableModels": avail}
}

func (a *agyState) setupResult(sid string) map[string]any {
	a.mu.Lock()
	mode := a.mode
	a.mu.Unlock()
	return map[string]any{
		"sessionId":     sid,
		"models":        a.legacyModels(),
		"modes":         map[string]any{"currentModeId": mode, "availableModes": agyModes},
		"configOptions": a.catalog(),
		"_meta":         map[string]any{"antigravity": map[string]any{"tier": map[string]string{"name": "Google AI Pro", "id": "g1-pro-tier"}}},
	}
}

// set applies one set_config_option the agent's way, or says why it refuses.
func (a *agyState) set(id, value string) string {
	if slices.Contains(strings.Split(os.Getenv("CRAZE_FAKE_AGY_REFUSE"), ","), value) {
		return fmt.Sprintf("Refused value: %s", value)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch id {
	case "model":
		m, ok := agyModelByID(value)
		level := ""
		if !ok {
			// A suffixed id is a model and a level (probe § 2).
			if i := strings.LastIndex(value, "-"); i > 0 {
				if base, found := agyModelByID(value[:i]); found && slices.Contains(base.levels, value[i+1:]) {
					m, ok, level = base, true, value[i+1:]
				}
			}
		}
		if !ok {
			return "Unknown model ID: " + value
		}
		a.model = m.id
		a.level = m.defaultLevel()
		if level != "" {
			a.level = level
		}
	case "thought_level":
		m, _ := agyModelByID(a.model)
		if !slices.Contains(m.levels, value) {
			return fmt.Sprintf("Thinking level %q is not offered for model %q", value, m.name)
		}
		a.level = value
	case "mode":
		a.mode = value
	default:
		return "Unknown config option: " + id
	}
	return ""
}

// agyCommands is a few of the 13 built-ins agy advertises (frames-01).
var agyCommands = []acp.AvailableCommand{
	{Name: "goal", Description: "Run until the specified goal is completely finished."},
	{Name: "compact", Description: "Compact the conversation now (summarize history to free up the context window)."},
	{Name: "plan", Description: "Plan carefully before executing a task."},
}

func (s *server) agyCommands(sid string) {
	s.update(sid, map[string]any{"sessionUpdate": acp.UpdateAvailableCommands, "availableCommands": agyCommands})
}

// onAgyRequest is the antigravity scripts' request handler.
func (s *server) onAgyRequest(msg *acp.Message) {
	if s.script == "antigravity-loggedout" && (msg.Method == acp.MethodSessionNew || msg.Method == acp.MethodSessionLoad) {
		_ = s.conn.ReplyErr(msg.ID, &acp.RPCError{Code: -32000, Message: "Authentication required"})
		return
	}
	switch msg.Method {
	case acp.MethodInitialize:
		s.reply(msg.ID, map[string]any{
			"protocolVersion": acp.ProtocolVersion,
			"agentCapabilities": map[string]any{
				"loadSession":        true,
				"promptCapabilities": map[string]any{"image": true, "audio": false, "embeddedContext": true},
				"sessionCapabilities": map[string]any{
					"resume": map[string]any{}, "close": map[string]any{}, "load": map[string]any{}, "list": map[string]any{},
				},
			},
			"agentInfo": map[string]string{"name": "Antigravity ACP Server", "version": "1.5.14"},
			"authMethods": []map[string]string{
				{"id": "oauth-personal", "name": "Log in with Google"},
				{"id": "oauth-business", "name": "Log in with Gemini Enterprise"},
				{"id": "gemini-api-key", "name": "Gemini API key"},
				{"id": "agent-platform", "name": "Gemini Enterprise Agent Platform"},
			},
		})
	case acp.MethodAuthenticate:
		_ = s.conn.ReplyErr(msg.ID, &acp.RPCError{Code: -32000, Message: "authenticate must not be called"})
	case acp.MethodSessionNew:
		res := s.agy.setupResult(fakeSessionID)
		if extra := os.Getenv("CRAZE_FAKE_AGY_PUSH_ON_NEW"); extra != "" {
			// The catalog changes before the reply is read: the update is
			// the client's to keep over the reply's own list.
			s.agy.mu.Lock()
			s.agy.extra = extra
			s.agy.mu.Unlock()
			s.update(fakeSessionID, map[string]any{"sessionUpdate": acp.UpdateConfigOption, "configOptions": s.agy.catalog()})
		}
		s.reply(msg.ID, res)
		s.agyCommands(fakeSessionID)
		s.update(fakeSessionID, map[string]any{"sessionUpdate": acp.UpdateUsage, "used": 0, "size": 1048576})
	case acp.MethodSessionLoad:
		var p acp.LoadSessionParams
		_ = json.Unmarshal(msg.Params, &p)
		s.mu.Lock()
		s.loadedID = p.SessionID
		s.mu.Unlock()
		s.agy.mu.Lock()
		s.agy.sid = p.SessionID
		// A load comes back on the default model and level, whatever the
		// session ran on and whatever the request carried (frames-08, -14).
		s.agy.model, s.agy.level, s.agy.mode = "gemini-4-neon", "high", "default"
		s.agy.mu.Unlock()
		if p.SessionID != agyStaleID {
			s.agyReplay(p.SessionID)
		}
		s.reply(msg.ID, s.agy.setupResult(p.SessionID))
		s.agyCommands(p.SessionID)
		if d := envDuration("CRAZE_FAKE_AGY_EXIT_AFTER_LOAD", 0); d > 0 {
			// The agent dies with no turn in flight.
			go func() { time.Sleep(d); os.Exit(3) }()
		}
	case acp.MethodSessionPrompt:
		dumpPrompt(msg.Params)
		s.cancelled.Store(false)
		go s.agyPrompt(msg)
	case acp.MethodSessionSetConfig:
		s.setGated(msg, s.agySetConfig)
	case acp.MethodSessionSetMode:
		var p acp.SetModeParams
		_ = json.Unmarshal(msg.Params, &p)
		s.agy.set("mode", p.ModeID)
		s.reply(msg.ID, map[string]any{})
		s.update(s.agy.sid, map[string]any{"sessionUpdate": acp.UpdateCurrentMode, "currentModeId": p.ModeID})
		s.update(s.agy.sid, map[string]any{"sessionUpdate": acp.UpdateConfigOption, "configOptions": s.agy.catalog()})
	default:
		_ = s.conn.ReplyErr(msg.ID, acp.MethodNotFound(msg.Method))
	}
}

// agySetConfig is session/set_config_option: the catalog pushed, then the
// reply carrying it (or the other way round under CRAZE_FAKE_AGY_PUSH_AFTER).
func (s *server) agySetConfig(msg *acp.Message) {
	var p acp.SetConfigParams
	_ = json.Unmarshal(msg.Params, &p)
	if why := s.agy.set(p.ConfigID, p.Value); why != "" {
		_ = s.conn.ReplyErr(msg.ID, &acp.RPCError{Code: acp.CodeInvalidParams, Message: why})
		return
	}
	cfg := s.agy.catalog()
	push := func() {
		s.update(s.agy.sid, map[string]any{"sessionUpdate": acp.UpdateConfigOption, "configOptions": cfg})
	}
	after := os.Getenv("CRAZE_FAKE_AGY_PUSH_AFTER") == "1"
	if !after {
		push()
	}
	s.reply(msg.ID, map[string]any{"configOptions": cfg})
	if after {
		push()
	}
}

// agyBrain is session sid's brain directory, under the agent's own HOME.
func agyBrain(sid string) string {
	return filepath.Join(os.Getenv("HOME"), ".gemini", "antigravity-cli", "brain", sid)
}

// agyTool is one tool update as agy sends it: the name at the top level, the
// args duplicating rawInput, and the fields every update repeats.
func agyTool(kind, call, name, toolKind, title, status string, rawInput map[string]any, extra map[string]any) map[string]any {
	u := map[string]any{
		"sessionUpdate": kind, "toolCallId": call, "name": name, "args": rawInput,
		"status": status, "kind": toolKind, "title": title, "rawInput": rawInput,
	}
	for k, v := range extra {
		u[k] = v
	}
	return u
}

// agyReplay is a load's replay (frames-08): the prompt, each call once and
// completed, the reply — a plan file and a sub-agent among the calls.
func (s *server) agyReplay(sid string) {
	s.update(sid, map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]string{"type": "text", "text": "hello"}})
	plan := filepath.Join(agyBrain(sid), "replayed_plan.md")
	s.update(sid, agyTool(acp.UpdateToolCall, "call_replay_plan", "write_file", "edit", "File replayed_plan.md", "completed",
		map[string]any{"Path": plan, "Content": "# plan", "ArtifactMetadata": map[string]any{"RequestFeedback": true, "Summary": "plan", "UserFacing": true}},
		map[string]any{"locations": []map[string]string{{"path": plan}}, "rawOutput": map[string]any{}, "content": []map[string]any{{"type": "diff", "path": plan, "newText": "# plan"}}}))
	s.update(sid, agyTool(acp.UpdateToolCall, "call_replay_sub", "invoke_subagent", "other", "Run invoke_subagent", "completed",
		map[string]any{"Prompt": "count", "Role": "Replayed runner", "TypeName": "self"}, nil))
	if os.Getenv("CRAZE_FAKE_AGY_REPLAY_RUNNING") == "1" {
		// A call replayed short of its end: a row no turn owns.
		s.update(sid, agyTool(acp.UpdateToolCall, "call_replay_running", "invoke_subagent", "other", "Run invoke_subagent", "in_progress",
			map[string]any{"Prompt": "wait", "Role": "Unfinished runner", "TypeName": "self"}, nil))
	}
	s.update(sid, map[string]any{"sessionUpdate": acp.UpdateAgentMessage, "content": map[string]string{"type": "text", "text": "hi there"}})
}

func (s *server) agySay(text string) {
	s.update(s.agy.sid, map[string]any{"sessionUpdate": acp.UpdateAgentMessage, "content": map[string]string{"type": "text", "text": text}})
}

func (s *server) agyEnd(id json.RawMessage, stop string) {
	s.reply(id, map[string]any{"stopReason": stop, "usage": map[string]int{"inputTokens": 10, "outputTokens": 2, "totalTokens": 12}})
}

// agyPrompt runs one prompt's turn by its directive (the usage above).
func (s *server) agyPrompt(msg *acp.Message) {
	text := promptText(msg.Params)
	fields := strings.Fields(text)
	directive, arg := "", ""
	if len(fields) > 0 {
		directive = fields[0]
	}
	if len(fields) > 1 {
		arg = fields[1]
	}
	id := msg.ID
	switch directive {
	case "ASK", "ASK2":
		s.agyAsk(id, directive == "ASK2")
	case "RUN":
		s.agyRun(id, arg)
	case "SLEEP":
		s.agySleep(id)
	case "EDITS":
		s.agyEdits(id)
	case "PLAN":
		s.agyPlan(id)
	case "SUBAGENTS":
		s.agySubagent("call_sub_a", "Runner A", "completed")
		s.agySubagent("call_sub_b", "Runner B", "completed")
		s.agySay("launched")
		s.agyEnd(id, acp.StopEndTurn)
	case "SUBAGENT":
		s.agySubagentTurn(id, arg)
	case "AUTH":
		_ = s.conn.ReplyErr(id, &acp.RPCError{Code: -32000, Message: "Authentication required"})
	case "ERR32000":
		_ = s.conn.ReplyErr(id, &acp.RPCError{Code: -32000, Message: "Internal error"})
	default:
		s.agySay(text)
		s.agyEnd(id, acp.StopEndTurn)
	}
}

// agyAsk is ask_question (frames-05): its call in progress then pending, the
// form keyed q<i>/q<i>_other with 1-based string consts, and the call
// completed whatever the answer.
func (s *server) agyAsk(id json.RawMessage, two bool) {
	const call = "call_ask"
	questions := []map[string]any{{"is_multi_select": false, "options": []string{"(Recommended) Red", "Green", "Blue"}, "question": "What is your favourite colour?"}}
	props := map[string]any{
		"q1": map[string]any{"type": "string", "title": "What is your favourite colour?", "oneOf": []map[string]string{
			{"const": "1", "title": "(Recommended) Red"}, {"const": "2", "title": "Green"}, {"const": "3", "title": "Blue"},
		}},
		"q1_other": map[string]any{"type": "string", "title": "Other", "description": "Optional free-form answer to: What is your favourite colour?"},
	}
	message := "The agent has a question."
	if two {
		questions = append(questions, map[string]any{"is_multi_select": false, "options": []string{"(Recommended) Tea", "Coffee"}, "question": "Which drink do you prefer?"})
		props["q2"] = map[string]any{"type": "string", "title": "Which drink do you prefer?", "oneOf": []map[string]string{
			{"const": "1", "title": "(Recommended) Tea"}, {"const": "2", "title": "Coffee"},
		}}
		props["q2_other"] = map[string]any{"type": "string", "title": "Other", "description": "Optional free-form answer to: Which drink do you prefer?"}
		message = "The agent has 2 questions."
	}
	input := map[string]any{"questions": questions}
	s.update(s.agy.sid, agyTool(acp.UpdateToolCall, call, "ask_question", "other", "Run ask_question", "in_progress", input, nil))
	s.update(s.agy.sid, agyTool(acp.UpdateToolCallUpd, call, "ask_question", "other", "Run ask_question", "pending", input, nil))
	answer := s.callRaw(acp.MethodElicitationCreate, map[string]any{
		"sessionId": s.agy.sid, "toolCallId": call, "mode": "form", "message": message,
		"requestedSchema": map[string]any{"type": "object", "properties": props},
	})
	s.update(s.agy.sid, agyTool(acp.UpdateToolCallUpd, call, "ask_question", "other", "Run ask_question", "completed", input, nil))
	s.agySay("ask answer: " + answer)
	s.agyEnd(id, acp.StopEndTurn)
}

// agyCommandOutput is run_command's rawOutput.output for kind.
func agyCommandOutput(kind string) (output string, code int) {
	header := func(code int) string {
		return fmt.Sprintf("Created At: 2026-11-22T22:16:51-06:00\nCompleted At: 2026-11-22T22:16:51-06:00\nCommand exited with code %d.\n\nCommand output:\n", code)
	}
	switch kind {
	case "noheader":
		return "plain output\n", 0
	case "empty":
		return "", 0
	case "literal":
		return header(0) + "first\nCommand output:\nsecond\n", 0
	case "exit2":
		return header(2) + "boom\r\n", 2
	}
	return header(0) + "line one\r\nline two\r\n", 0
}

// agyPermission asks before a command when the agent was not started with
// --dangerously-skip-permissions, as agy does for run_command alone (frames-04).
func (s *server) agyPermission(call string, input map[string]any) {
	if slices.Contains(os.Args[1:], "--dangerously-skip-permissions") {
		return
	}
	s.update(s.agy.sid, agyTool(acp.UpdateToolCallUpd, call, "run_command", "execute", "echo hi", "pending", input, nil))
	answer := s.callRaw(acp.MethodRequestPermission, map[string]any{
		"sessionId": s.agy.sid,
		"toolCall":  agyTool(acp.UpdateToolCall, call, "run_command", "execute", "echo hi", "pending", input, nil),
		"options": []map[string]string{
			{"optionId": "allow", "name": "Allow", "kind": acp.KindAllowOnce},
			{"optionId": "deny", "name": "Deny", "kind": acp.KindRejectOnce},
		},
	})
	s.agySay("permission: " + answer + "\n")
}

func (s *server) agyRun(id json.RawMessage, kind string) {
	const call = "call_run"
	input := map[string]any{"CommandLine": "echo hi", "WaitMsBeforeAsync": 5000, "toolSummary": "Say hi"}
	s.update(s.agy.sid, agyTool(acp.UpdateToolCall, call, "run_command", "execute", "echo hi", "in_progress", input, nil))
	s.agyPermission(call, input)
	output, code := agyCommandOutput(kind)
	raw := map[string]any{"output": output, "exit_code": code}
	if kind == "empty" {
		raw = map[string]any{"output": output}
	}
	s.update(s.agy.sid, agyTool(acp.UpdateToolCallUpd, call, "run_command", "execute", "echo hi", "completed", input, map[string]any{
		"rawOutput": raw,
		"content":   []map[string]any{{"type": "content", "content": map[string]string{"type": "text", "text": output}}},
	}))
	s.agyEnd(id, acp.StopEndTurn)
}

// agySleep is frames-07: a command running until session/cancel, the turn's
// reply cancelled at once, and the call failed after it with no output.
func (s *server) agySleep(id json.RawMessage) {
	const call = "call_sleep"
	input := map[string]any{"CommandLine": "sleep 30; echo finished", "WaitMsBeforeAsync": 5000}
	s.update(s.agy.sid, agyTool(acp.UpdateToolCall, call, "run_command", "execute", "sleep 30; echo finished", "in_progress", input, nil))
	if !s.waitCancelled(id) {
		return
	}
	s.agyEnd(id, acp.StopCancelled)
	s.update(s.agy.sid, agyTool(acp.UpdateToolCallUpd, call, "run_command", "execute", "sleep 30; echo finished", "failed", input, map[string]any{
		"rawOutput": map[string]any{"output": ""},
		"content":   []map[string]any{{"type": "content", "content": map[string]string{"type": "text", "text": ""}}},
	}))
}

// agyWrite is one write_file or edit_file call, completed or failed.
func (s *server) agyWrite(call, name, path, status string, extraInput map[string]any) {
	input := map[string]any{"Path": path}
	if name == "write_file" {
		input["Content"] = "hi"
	} else {
		input["Target"], input["Replacement"] = "world", "there"
	}
	for k, v := range extraInput {
		input[k] = v
	}
	title := "File " + filepath.Base(path)
	locations := []map[string]string{{"path": path}}
	s.update(s.agy.sid, agyTool(acp.UpdateToolCall, call, name, "edit", title, "in_progress", input, map[string]any{"locations": locations}))
	extra := map[string]any{"locations": locations}
	if status == "completed" {
		extra["rawOutput"] = map[string]any{}
		diff := map[string]any{"type": "diff", "path": path, "newText": "hi"}
		if name == "edit_file" {
			diff["oldText"], diff["newText"] = "hello world\n", "hello there\n"
		}
		extra["content"] = []map[string]any{diff}
	}
	s.update(s.agy.sid, agyTool(acp.UpdateToolCallUpd, call, name, "edit", title, status, input, extra))
}

func (s *server) agyEdits(id json.RawMessage) {
	wd, _ := os.Getwd()
	s.agyWrite("call_edit", "edit_file", filepath.Join(wd, "hello.txt"), "completed", nil)
	s.agyWrite("call_write", "write_file", filepath.Join(wd, "new.txt"), "completed", nil)
	s.agyWrite("call_write_failed", "write_file", filepath.Join(wd, "denied.txt"), "failed", nil)
	s.agyWrite("call_other_brain", "write_file", filepath.Join(agyBrain("another-session"), "plan.md"), "completed",
		map[string]any{"ArtifactMetadata": map[string]any{"RequestFeedback": true}})
	s.agySay("edited")
	s.agyEnd(id, acp.StopEndTurn)
}

// agyPlan is a plan turn (frames-09, -10): the plan file asks for feedback,
// the todo list does not, both in this session's brain directory.
func (s *server) agyPlan(id json.RawMessage) {
	brain := agyBrain(s.agy.sid)
	s.agyWrite("call_plan", "write_file", filepath.Join(brain, "add_gamma_plan.md"), "completed",
		map[string]any{"ArtifactMetadata": map[string]any{"RequestFeedback": true, "Summary": "plan", "UserFacing": true}})
	s.agyWrite("call_todo", "write_file", filepath.Join(brain, "task_list.md"), "completed",
		map[string]any{"ArtifactMetadata": map[string]any{"RequestFeedback": false, "Summary": "todo", "UserFacing": true}})
	s.agySay("planned")
	s.agyEnd(id, acp.StopEndTurn)
}

// agySubagent is one invoke_subagent launch (frames-06): in progress, then
// status (completed, unless it fails) at once with no output and no child id.
func (s *server) agySubagent(call, role, status string) {
	s.agySubagentUpdate(acp.UpdateToolCall, call, role, "in_progress")
	s.agySubagentUpdate(acp.UpdateToolCallUpd, call, role, status)
}

// agySubagentUpdate is one update of invoke_subagent call, in role.
func (s *server) agySubagentUpdate(kind, call, role, status string) {
	input := map[string]any{"Prompt": "Run `echo A` and report its output.", "Role": role, "TypeName": "self"}
	s.update(s.agy.sid, agyTool(kind, call, "invoke_subagent", "other", "Run invoke_subagent", status, input, nil))
}

func (s *server) agySubagentTurn(id json.RawMessage, how string) {
	switch how {
	case "fail":
		s.agySubagent("call_sub_fail", "Failing runner", "failed")
		s.agyEnd(id, acp.StopEndTurn)
	case "hold":
		s.agySubagent("call_sub_hold", "Held runner", "completed")
		if s.waitCancelled(id) {
			s.agyEnd(id, acp.StopCancelled)
		}
	case "error":
		s.agySubagent("call_sub_err", "Erring runner", "completed")
		_ = s.conn.ReplyErr(id, &acp.RPCError{Code: -32603, Message: "Internal error"})
	case "die":
		s.agySubagent("call_sub_die", "Dying runner", "completed")
		// Every frame is written whole and unbuffered: exiting now is the
		// agent's process dying under the turn.
		time.Sleep(50 * time.Millisecond)
		os.Exit(3)
	case "late":
		s.agySubagent("call_sub_late", "Late runner", "completed")
		s.agyEnd(id, acp.StopEndTurn)
		time.Sleep(50 * time.Millisecond)
		s.agySubagentUpdate(acp.UpdateToolCallUpd, "call_sub_late", "Late runner, renamed", "completed")
	default:
		s.agySubagent("call_sub_1", "Runner", "completed")
		s.agyEnd(id, acp.StopEndTurn)
	}
}
