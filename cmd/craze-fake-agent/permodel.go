package main

// The permodel scripts: cursor as it answers today (Plan 025, re-captured
// 2026-09-21 with craze's own initialize meta), where every model has an option
// catalog of its own. grok-4.6 advertises effort and fast, composer-2.5 fast
// alone, claude-opus-5 thinking, context, effort and fast, and glm-5.2 a
// reasoning select — so a client that keeps the catalog it started on offers a
// control the model it switched to does not have.
//
// The wire, as probed (the capture, and plan 025 §1.2 for the mode push):
//
//   - session/set_config_option(model, X) switches the model and ANSWERS with
//     X's full catalog, then pushes nothing. An unknown X is -32602
//     "Invalid params", data.message "Invalid model value: X".
//   - session/set_config_option(id, v) for one of the current model's options
//     answers the full catalog and pushes nothing — except mode, after which a
//     current_mode_update is pushed (the reason a "no-op" mode write is not
//     one). An id the current model does not advertise is -32602
//     "Invalid params", data.message "Unknown model config option: <id>"; only
//     data.message tells the two refusals apart.
//   - session/set_model(X) switches the model — and, server side, the catalog —
//     but answers {} and pushes nothing, so a client learns no catalog from it.
//     No probe sent it an unknown model; the fake refuses one with the model
//     option's own "Invalid model value".
//
// Every value a model's options hold persists across switches, as cursor's do
// (cursor keeps them on the account).
//
// The variants are that same server with one behaviour changed:
//
//	permodel-empty           every set_config_option that succeeds answers
//	                         {"configOptions": []}; the state still changes,
//	                         and a mode write still pushes its mode update
//	permodel-noreply         set_config_option(model, X) switches but answers {}
//	permodel-refuse          set_config_option(model, …) is refused -32602
//	                         "Unknown model config option: model" although the
//	                         catalog advertises the model option: an agent whose
//	                         config path does not take the model, so a client's
//	                         set_model fallback runs; set_model switches as above
//	permodel-nomodel         no catalog carries a model option (mode and the
//	                         knobs only), so set_model is the only way to switch
//	permodel-pushbefore      set_config_option(model, X) writes an unsolicited
//	                         config_option_update immediately BEFORE its reply
//	permodel-pushafter       the same push, immediately AFTER the reply
//	permodel-pushmodel-after after the reply, a push that also moves the model
//
// The three push variants are the ordering barrier: the push and the reply are
// written one after the other by the fake's read loop, which is where every
// request is handled, so the order they reach a client's read loop is fixed by
// construction and no clock is involved. What each push carries is fixed too,
// so a test can tell by value which of the two it is looking at:
//
//   - pushbefore and pushafter push X's catalog with its LAST knob moved to its
//     next value, wrapping — every model's last knob has exactly two values, so
//     that is its other one. From a fresh session:
//
//     X             reply's last knob   push's last knob
//     grok-4.6      fast=true           fast=false
//     composer-2.5  fast=false          fast=true
//     claude-opus-5 fast=false          fast=true
//     glm-5.2       reasoning=high      reasoning=max
//
//     Every other option in the push, the model among them, is the reply's.
//   - pushmodel-after pushes the catalog of the model AFTER X in the list
//     (grok-4.6 → composer-2.5 → claude-opus-5 → glm-5.2 → grok-4.6), its
//     model option on that model: the agent moving the model on its own.
//
// The fake's own state afterwards is whichever of the two it wrote last, so a
// later reply or load agrees with what a correct client ends up holding: after
// pushbefore the reply's catalog, after pushafter the knob stays moved, and
// after pushmodel-after the session is on the next model.

import (
	"encoding/json"

	"github.com/charliek/craze/internal/acp"
)

// pmValue is one value of a select, in cursor's key order. Only the mode
// option's values carry a description.
type pmValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// pmOption is one config option exactly as cursor writes it: every field
// present and in cursor's key order, so a fake reply and a capture read alike.
type pmOption struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	Category     string    `json:"category"`
	Type         string    `json:"type"`
	CurrentValue string    `json:"currentValue"`
	Options      []pmValue `json:"options"`
}

// pmModel is one model and the options it advertises beyond mode and model, in
// the order cursor answers them. Each knob's CurrentValue is what the model
// holds now; permodelModels gives the defaults.
type pmModel struct {
	ID    string
	Name  string
	Knobs []pmOption
}

// The option ids the client and the fake both key on.
const (
	pmModeID  = "mode"
	pmModelID = "model"
)

// pmModes are cursor's three modes as its mode option lists them.
var pmModes = []pmValue{
	{Value: "agent", Name: "Agent", Description: "Full agent capabilities with tool access"},
	{Value: "plan", Name: "Plan", Description: "Read-only mode for planning and designing before implementation"},
	{Value: "ask", Name: "Ask", Description: "Q&A mode - no edits or command execution"},
}

// permodelModels are the four models, in cursor's own list order, each with
// cursor's option objects from the capture: ids, names, descriptions,
// categories and values verbatim, grok-4.6's "on" label with its two U+200B
// included. The defaults are the capture's currentValues except three: the
// capture carries the owner's persisted choices (cursor keeps them on the
// account), so grok-4.6's effort and claude-opus-5's effort start on high, and
// claude-opus-5's context on the smaller window, 300k.
func permodelModels() []pmModel {
	effortDesc := "Effort the model uses to generate its response."
	offOn := func(on string) []pmValue {
		return []pmValue{{Value: "false", Name: "Off"}, {Value: "true", Name: on}}
	}
	return []pmModel{
		{ID: "grok-4.6", Name: "Grok 4.6", Knobs: []pmOption{
			{
				ID: "effort", Name: "Effort", Description: effortDesc,
				Category: "thought_level", Type: "select", CurrentValue: "high",
				Options: []pmValue{
					{Value: "low", Name: "Low"},
					{Value: "medium", Name: "Medium"},
					{Value: "high", Name: "High"},
					{Value: "xhigh", Name: "Extra High"},
				},
			},
			{
				ID: "fast", Name: "Fast", Description: "Significantly faster but consumes more usage",
				Category: "model_config", Type: "select", CurrentValue: "true",
				Options: offOn("Fast\u200b\u200b"),
			},
		}},
		{ID: "composer-2.5", Name: "Composer 2.5", Knobs: []pmOption{
			{
				ID: "fast", Name: "Fast", Description: "Significantly faster but consumes more usage",
				Category: "model_config", Type: "select", CurrentValue: "false",
				Options: offOn("Fast"),
			},
		}},
		{ID: "claude-opus-5", Name: "Claude Opus 5", Knobs: []pmOption{
			{
				// thought_level, like effort: the category is no way to tell
				// the two apart, only the id and the name are.
				ID: "thinking", Name: "Thinking", Description: "Does the model use thinking to generate its response?",
				Category: "thought_level", Type: "select", CurrentValue: "true",
				Options: offOn("On"),
			},
			{
				ID: "context", Name: "Context", Description: "Context size the model has available.",
				Category: "model_config", Type: "select", CurrentValue: "300k",
				Options: []pmValue{{Value: "300k", Name: "300K"}, {Value: "1m", Name: "1M"}},
			},
			{
				ID: "effort", Name: "Effort", Description: effortDesc,
				Category: "thought_level", Type: "select", CurrentValue: "high",
				Options: []pmValue{
					{Value: "low", Name: "Low"},
					{Value: "medium", Name: "Medium"},
					{Value: "high", Name: "High"},
					{Value: "xhigh", Name: "Extra High"},
					{Value: "max", Name: "Max"},
				},
			},
			{
				ID: "fast", Name: "Fast", Description: "2x more expensive, but significantly faster speeds.",
				Category: "model_config", Type: "select", CurrentValue: "false",
				Options: offOn("Fast"),
			},
		}},
		{ID: "glm-5.2", Name: "GLM 5.2", Knobs: []pmOption{
			{
				ID: "reasoning", Name: "Reasoning", Description: "Reasoning effort the model uses to generate its response.",
				Category: "thought_level", Type: "select", CurrentValue: "high",
				Options: []pmValue{{Value: "high", Name: "High"}, {Value: "max", Name: "Max"}},
			},
		}},
	}
}

// permodelScript names the permodel family. It is a list and not a prefix so
// a name main does not know is still refused there.
func permodelScript(script string) bool {
	switch script {
	case "permodel", "permodel-empty", "permodel-noreply", "permodel-refuse", "permodel-nomodel",
		"permodel-pushbefore", "permodel-pushafter", "permodel-pushmodel-after":
		return true
	}
	return false
}

// permodelState is the fake cursor's session state: the model it is on, the
// session-wide mode, and every model's knobs with the values they hold. It is
// guarded by server.mu, and only the read loop touches it.
type permodelState struct {
	script string
	models []pmModel
	model  string
	mode   string
}

func newPermodelState(script string) *permodelState {
	models := permodelModels()
	return &permodelState{script: script, models: models, model: models[0].ID, mode: pmModes[0].Value}
}

// hasModelOption reports whether this script's catalogs advertise the model
// option at all; permodel-nomodel's do not.
func (st *permodelState) hasModelOption() bool { return st.script != "permodel-nomodel" }

func (st *permodelState) find(model string) *pmModel {
	for i := range st.models {
		if st.models[i].ID == model {
			return &st.models[i]
		}
	}
	return nil
}

// modelAfter is the model after id in the list, wrapping: where
// permodel-pushmodel-after's push moves the session.
func (st *permodelState) modelAfter(id string) string {
	for i := range st.models {
		if st.models[i].ID == id {
			return st.models[(i+1)%len(st.models)].ID
		}
	}
	return st.models[0].ID
}

// catalogOf is model m's full option list as cursor answers it: mode, model
// (on m, listing the four), then m's knobs. The knobs are copies, so a catalog
// already handed out never sees a later change.
func (st *permodelState) catalogOf(m *pmModel) []pmOption {
	out := []pmOption{{
		ID: pmModeID, Name: "Mode", Description: "Controls how the agent executes tasks",
		Category: "mode", Type: "select", CurrentValue: st.mode, Options: pmModes,
	}}
	if st.hasModelOption() {
		values := make([]pmValue, 0, len(st.models))
		for _, mm := range st.models {
			values = append(values, pmValue{Value: mm.ID, Name: mm.Name})
		}
		out = append(out, pmOption{
			ID: pmModelID, Name: "Model", Description: "Controls which model is used for responses",
			Category: "model", Type: "select", CurrentValue: m.ID, Options: values,
		})
	}
	return append(out, m.Knobs...)
}

func (st *permodelState) catalog() []pmOption { return st.catalogOf(st.find(st.model)) }

// sessionResult is session/new's result without the id: cursor's modes and
// models blocks, plus the current model's catalog.
func (st *permodelState) sessionResult() map[string]any {
	modes := make([]map[string]string, 0, len(pmModes))
	for _, m := range pmModes {
		modes = append(modes, map[string]string{"id": m.Value, "name": m.Name})
	}
	models := make([]map[string]string, 0, len(st.models))
	for _, m := range st.models {
		models = append(models, map[string]string{"modelId": m.ID, "name": m.Name})
	}
	return map[string]any{
		"modes":         map[string]any{"currentModeId": st.mode, "availableModes": modes},
		"models":        map[string]any{"currentModelId": st.model, "availableModels": models},
		"configOptions": st.catalog(),
	}
}

// nextValue is opt's value after its current one in its own list, wrapping.
func nextValue(opt pmOption) string {
	for i, v := range opt.Options {
		if v.Value == opt.CurrentValue {
			return opt.Options[(i+1)%len(opt.Options)].Value
		}
	}
	return opt.Options[0].Value
}

// withLastKnobMoved is catalog with its last option moved to its next value:
// what the pushbefore and pushafter scripts push. The last option is always a
// knob, never mode or model, because every model has at least one.
func withLastKnobMoved(catalog []pmOption) []pmOption {
	out := append([]pmOption(nil), catalog...)
	last := &out[len(out)-1]
	last.CurrentValue = nextValue(*last)
	return out
}

// pmSet is what one successful set_config_option writes, in the order it is
// written: the pushes before the reply, the reply, then the pushes after.
type pmSet struct {
	before []pmOption
	reply  any
	after  []pmOption
	// mode is the mode a mode write moved to, pushed as a current_mode_update
	// after everything else.
	mode string
}

// setConfig applies one set_config_option to the state and says what to write,
// or refuses it the way cursor does. It runs under server.mu; the writes are the
// caller's, outside it.
func (st *permodelState) setConfig(id, value string) (pmSet, *acp.RPCError) {
	var out pmSet
	switch {
	case id == pmModelID && st.hasModelOption() && st.script != "permodel-refuse":
		m := st.find(value)
		if m == nil {
			return pmSet{}, pmInvalidModel(value)
		}
		st.model = m.ID
		reply := st.catalog()
		out.reply = map[string]any{"configOptions": reply}
		switch st.script {
		case "permodel-noreply":
			out.reply = map[string]any{}
		case "permodel-pushbefore":
			out.before = withLastKnobMoved(reply)
		case "permodel-pushafter":
			// reply is a copy, so moving the knob here changes the push and
			// the state, never the reply.
			last := &m.Knobs[len(m.Knobs)-1]
			last.CurrentValue = nextValue(*last)
			out.after = st.catalog()
		case "permodel-pushmodel-after":
			st.model = st.modelAfter(m.ID)
			out.after = st.catalog()
		}
	case id == pmModeID:
		if !pmHasValue(pmModes, value) {
			return pmSet{}, pmInvalidValue(id, value)
		}
		st.mode = value
		out.reply = map[string]any{"configOptions": st.catalog()}
		out.mode = value
	default:
		// permodel-refuse's model option lands here too: advertised, and
		// refused as unknown all the same.
		knob := st.knob(id)
		if knob == nil {
			return pmSet{}, pmUnknownOption(id)
		}
		if !pmHasValue(knob.Options, value) {
			return pmSet{}, pmInvalidValue(id, value)
		}
		knob.CurrentValue = value
		out.reply = map[string]any{"configOptions": st.catalog()}
	}
	if st.script == "permodel-empty" {
		out.reply = map[string]any{"configOptions": []pmOption{}}
	}
	return out, nil
}

// knob is the current model's option id, or nil when this model has none.
func (st *permodelState) knob(id string) *pmOption {
	m := st.find(st.model)
	for i := range m.Knobs {
		if m.Knobs[i].ID == id {
			return &m.Knobs[i]
		}
	}
	return nil
}

func pmHasValue(values []pmValue, v string) bool {
	for _, x := range values {
		if x.Value == v {
			return true
		}
	}
	return false
}

// pmInvalidParams is cursor's refusal shape: every one is -32602
// "Invalid params", and only data.message says which refusal it is.
func pmInvalidParams(message string) *acp.RPCError {
	data, _ := json.Marshal(map[string]string{"message": message})
	return &acp.RPCError{Code: acp.CodeInvalidParams, Message: "Invalid params", Data: data}
}

// pmInvalidModel and pmUnknownOption are the two captured refusals, verbatim.
func pmInvalidModel(model string) *acp.RPCError {
	return pmInvalidParams("Invalid model value: " + model)
}

func pmUnknownOption(id string) *acp.RPCError {
	return pmInvalidParams("Unknown model config option: " + id)
}

// pmInvalidValue refuses a value an option does not list. It was not captured —
// no probe sent one — so its wording is the fake's own; the shape is cursor's,
// so a client treats it as the refusal it is.
func pmInvalidValue(id, value string) *acp.RPCError {
	return pmInvalidParams("Invalid value for config option " + id + ": " + value)
}

// onPermodelRequest is the permodel scripts' request handler. It takes the
// session and settings methods, and hands the rest to onRequest, whose answers
// for them are the cursor scripts' own: initialize, authenticate, the echo
// turn a session/prompt gets, and -32601 for a method the fake does not know.
func (s *server) onPermodelRequest(msg *acp.Message) {
	switch msg.Method {
	case acp.MethodSessionNew:
		s.mu.Lock()
		res := s.pm.sessionResult()
		s.mu.Unlock()
		res["sessionId"] = fakeSessionID
		s.reply(msg.ID, res)
		s.advertiseCommands()
	case acp.MethodSessionLoad:
		s.permodelLoad(msg)
	case acp.MethodSessionSetConfig:
		s.permodelSetConfig(msg)
	case acp.MethodSessionSetModel:
		var p acp.SetModelParams
		_ = json.Unmarshal(msg.Params, &p)
		s.mu.Lock()
		known := s.pm.find(p.ModelID) != nil
		if known {
			s.pm.model = p.ModelID
		}
		s.mu.Unlock()
		if !known {
			_ = s.conn.ReplyErr(msg.ID, pmInvalidModel(p.ModelID))
			return
		}
		// The catalog moved with the model, and the reply says nothing of it.
		s.reply(msg.ID, map[string]any{})
	case acp.MethodSessionSetMode:
		// The cursor scripts' answer, {} and then the mode update, but on the
		// session this fake is serving: after a load that is the loaded id,
		// where onRequest's would push onto the fixed one.
		var p acp.SetModeParams
		_ = json.Unmarshal(msg.Params, &p)
		if p.ModeID != "" {
			s.mu.Lock()
			s.pm.mode = p.ModeID
			s.mu.Unlock()
		}
		s.reply(msg.ID, map[string]any{})
		if p.ModeID != "" {
			s.pushMode(p.ModeID)
		}
	default:
		s.onRequest(msg)
	}
}

// permodelLoad answers session/load with the session/new result, the current
// model's catalog included, and replays nothing. Live cursor's load result
// carries no configOptions (load.go's cursorLoadResult), so a resumed cursor
// session has no catalog until one arrives; this one does, which is what lets
// a resume `--model` take the one-call path in a test. It answers on the read
// loop: with no replay there is nothing to block on.
func (s *server) permodelLoad(msg *acp.Message) {
	var p acp.LoadSessionParams
	_ = json.Unmarshal(msg.Params, &p)
	sid := p.SessionID
	if sid == "" {
		sid = fakeSessionID
	}
	s.mu.Lock()
	s.loadedID = sid
	res := s.pm.sessionResult()
	s.mu.Unlock()
	s.reply(msg.ID, res)
}

// permodelSetConfig is session/set_config_option. The state changes under
// s.mu; every frame it answers with is written here, on the read loop, in the
// order setConfig gave — which is the whole of the ordering barrier.
func (s *server) permodelSetConfig(msg *acp.Message) {
	var p acp.SetConfigParams
	_ = json.Unmarshal(msg.Params, &p)
	s.mu.Lock()
	out, rerr := s.pm.setConfig(p.ConfigID, p.Value)
	s.mu.Unlock()
	if rerr != nil {
		_ = s.conn.ReplyErr(msg.ID, rerr)
		return
	}
	if out.before != nil {
		s.pushCatalog(out.before)
	}
	s.reply(msg.ID, out.reply)
	if out.after != nil {
		s.pushCatalog(out.after)
	}
	if out.mode != "" {
		s.pushMode(out.mode)
	}
}

func (s *server) pushCatalog(catalog []pmOption) {
	s.update(s.mainID(), map[string]any{
		"sessionUpdate": acp.UpdateConfigOption,
		"configOptions": catalog,
	})
}

func (s *server) pushMode(mode string) {
	s.update(s.mainID(), acp.SessionUpdate{
		SessionUpdate: acp.UpdateCurrentMode,
		CurrentModeID: mode,
	})
}
