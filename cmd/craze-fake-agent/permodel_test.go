package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// The permodel scripts over an in-process pipe, read frame by frame: the order
// the fake writes in is the order a client's read loop sees, and that order is
// what the ordering variants exist to fix.

// pmWait bounds every read; a correct run never comes near it.
const pmWait = 10 * time.Second

// pmBarrierMethod is a method no script knows. The fake handles every request
// on its read loop, one at a time, so everything a request's handler writes is
// on the wire before the -32601 for the one after it: a barrier, not a timeout,
// is how "nothing was pushed" is observed.
const pmBarrierMethod = "craze/fake-barrier"

type pmWire struct {
	t      *testing.T
	enc    *acp.Encoder
	frames chan *acp.Message
	nextID int
}

// dialPermodel starts script on a pipe and returns the client end.
func dialPermodel(t *testing.T, script string) *pmWire {
	t.Helper()
	agentIn, clientOut := io.Pipe()
	clientIn, agentOut := io.Pipe()
	conn := acp.NewConn(agentIn, agentOut)
	newServer(conn, script)
	conn.Start()
	w := &pmWire{t: t, enc: acp.NewEncoder(clientOut), frames: make(chan *acp.Message, 64)}
	done := make(chan struct{})
	go func() {
		defer close(w.frames)
		dec := acp.NewDecoder(clientIn)
		for {
			msg, err := dec.ReadMessage()
			if err != nil {
				return
			}
			select {
			case w.frames <- msg:
			case <-done:
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(done)
		_ = clientOut.Close()
		_ = conn.Close()
	})
	return w
}

func (w *pmWire) send(method string, params any) json.RawMessage {
	w.t.Helper()
	w.nextID++
	id, _ := json.Marshal(w.nextID)
	raw, err := json.Marshal(params)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := w.enc.WriteMessage(&acp.Message{ID: id, Method: method, Params: raw}); err != nil {
		w.t.Fatalf("write %s: %v", method, err)
	}
	return id
}

func (w *pmWire) recv() *acp.Message {
	w.t.Helper()
	select {
	case msg, ok := <-w.frames:
		if !ok {
			w.t.Fatal("the fake closed the wire")
		}
		return msg
	case <-time.After(pmWait):
		w.t.Fatalf("no frame within %v", pmWait)
	}
	return nil
}

// call sends one request and reads up to its answer, returning the answer and
// every frame the fake wrote before it.
func (w *pmWire) call(method string, params any) (*acp.Message, []*acp.Message) {
	w.t.Helper()
	id := w.send(method, params)
	var before []*acp.Message
	for {
		msg := w.recv()
		if msg.IsResponse() && string(msg.ID) == string(id) {
			return msg, before
		}
		before = append(before, msg)
	}
}

// ok is call for a request that must succeed with nothing written before its
// answer.
func (w *pmWire) ok(method string, params any) json.RawMessage {
	w.t.Helper()
	resp, before := w.call(method, params)
	if resp.Error != nil {
		w.t.Fatalf("%s %v refused: %+v", method, params, resp.Error)
	}
	if len(before) != 0 {
		w.t.Fatalf("%s %v wrote %d frames before its answer: %s", method, params, len(before), frameNames(before))
	}
	return resp.Result
}

// barrier is every frame the fake wrote after the last answer.
func (w *pmWire) barrier() []*acp.Message {
	w.t.Helper()
	resp, before := w.call(pmBarrierMethod, map[string]any{})
	if resp.Error == nil || resp.Error.Code != acp.CodeMethodNotFound {
		w.t.Fatalf("the barrier was answered %+v", resp)
	}
	return before
}

// quiet fails unless the fake wrote nothing since the last answer.
func (w *pmWire) quiet(after string) {
	w.t.Helper()
	if rest := w.barrier(); len(rest) != 0 {
		w.t.Fatalf("after %s the fake pushed %s", after, frameNames(rest))
	}
}

// start is initialize, authenticate and session/new, the catalog advertisement
// session/new sends read off, and session/new's result.
func (w *pmWire) start() map[string]json.RawMessage {
	w.t.Helper()
	w.ok(acp.MethodInitialize, map[string]any{"protocolVersion": acp.ProtocolVersion})
	w.ok(acp.MethodAuthenticate, map[string]any{"methodId": acp.AuthCursorLogin})
	res := w.ok(acp.MethodSessionNew, map[string]any{"cwd": "/tmp", "mcpServers": []any{}})
	for _, f := range w.barrier() {
		if kind, _ := updateOf(w.t, f); kind != acp.UpdateAvailableCommands {
			w.t.Fatalf("session/new pushed %s", frameNames([]*acp.Message{f}))
		}
	}
	return fields(w.t, res)
}

func (w *pmWire) setConfig(id, value string) (*acp.Message, []*acp.Message) {
	w.t.Helper()
	return w.call(acp.MethodSessionSetConfig, map[string]string{"sessionId": fakeSessionID, "configId": id, "value": value})
}

// catalogReply is a successful set_config_option's catalog, with nothing
// written before it.
func (w *pmWire) catalogReply(id, value string) []pmOption {
	w.t.Helper()
	return replyCatalog(w.t, w.ok(acp.MethodSessionSetConfig,
		map[string]string{"sessionId": fakeSessionID, "configId": id, "value": value}))
}

// state reads the fake's current model and catalog through session/load,
// which answers with both on every permodel script.
func (w *pmWire) state() (model string, catalog []pmOption) {
	w.t.Helper()
	res := fields(w.t, w.ok(acp.MethodSessionLoad, map[string]any{"sessionId": fakeSessionID, "cwd": "/tmp", "mcpServers": []any{}}))
	var models struct {
		CurrentModelID string `json:"currentModelId"`
	}
	if err := json.Unmarshal(res["models"], &models); err != nil {
		w.t.Fatal(err)
	}
	if err := json.Unmarshal(res["configOptions"], &catalog); err != nil {
		w.t.Fatal(err)
	}
	return models.CurrentModelID, catalog
}

func fields(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("result %s: %v", raw, err)
	}
	return out
}

func replyCatalog(t *testing.T, raw json.RawMessage) []pmOption {
	t.Helper()
	cfg, ok := fields(t, raw)["configOptions"]
	if !ok {
		t.Fatalf("the reply %s carries no configOptions", raw)
	}
	var out []pmOption
	if err := json.Unmarshal(cfg, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// updateOf is a session/update's kind and the whole update.
func updateOf(t *testing.T, msg *acp.Message) (string, map[string]json.RawMessage) {
	t.Helper()
	if msg.Method != acp.MethodSessionUpdate {
		t.Fatalf("frame %s is not a session/update", frameNames([]*acp.Message{msg}))
	}
	var n struct {
		SessionID string                     `json:"sessionId"`
		Update    map[string]json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(msg.Params, &n); err != nil {
		t.Fatal(err)
	}
	var kind string
	_ = json.Unmarshal(n.Update["sessionUpdate"], &kind)
	return kind, n.Update
}

// pushedCatalog is a config_option_update's catalog.
func pushedCatalog(t *testing.T, msg *acp.Message) []pmOption {
	t.Helper()
	kind, upd := updateOf(t, msg)
	if kind != acp.UpdateConfigOption {
		t.Fatalf("the push is a %s, not a config_option_update", kind)
	}
	var out []pmOption
	if err := json.Unmarshal(upd["configOptions"], &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func frameNames(msgs []*acp.Message) string {
	var out []string
	for _, m := range msgs {
		switch {
		case m.Method == acp.MethodSessionUpdate:
			var n struct {
				Update struct {
					SessionUpdate string `json:"sessionUpdate"`
				} `json:"update"`
			}
			_ = json.Unmarshal(m.Params, &n)
			out = append(out, "update:"+n.Update.SessionUpdate)
		case m.Method != "":
			out = append(out, m.Method)
		default:
			out = append(out, "reply:"+string(m.ID))
		}
	}
	return "[" + strings.Join(out, " ") + "]"
}

// ids is a catalog as "id=value" pairs, in order: the whole of what a test
// compares, since the ids say which options there are and the values say which
// catalog it is.
func ids(catalog []pmOption) string {
	out := make([]string, 0, len(catalog))
	for _, o := range catalog {
		out = append(out, o.ID+"="+o.CurrentValue)
	}
	return strings.Join(out, " ")
}

func option(t *testing.T, catalog []pmOption, id string) pmOption {
	t.Helper()
	for _, o := range catalog {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("no %s option in %s", id, ids(catalog))
	return pmOption{}
}

// The four models' catalogs as a fresh session answers them.
const (
	pmGrok     = "mode=agent model=grok-4.6 effort=high fast=true"
	pmComposer = "mode=agent model=composer-2.5 fast=false"
	pmOpus     = "mode=agent model=claude-opus-5 thinking=true context=300k effort=high fast=false"
	pmGLM      = "mode=agent model=glm-5.2 reasoning=high"
)

// TestPermodelStartsOnGrokWithCursorsShapes: initialize advertises loadSession,
// session/new answers cursor's modes and models blocks on grok-4.6, and its
// catalog is cursor's option objects: every field, the four models in the model
// option, and grok-4.6's fast label with both of its U+200B.
func TestPermodelStartsOnGrokWithCursorsShapes(t *testing.T) {
	w := dialPermodel(t, "permodel")
	initRes := fields(t, w.ok(acp.MethodInitialize, map[string]any{"protocolVersion": acp.ProtocolVersion}))
	if !strings.Contains(string(initRes["agentCapabilities"]), `"loadSession":true`) {
		t.Fatalf("initialize advertises %s", initRes["agentCapabilities"])
	}
	w.ok(acp.MethodAuthenticate, map[string]any{"methodId": acp.AuthCursorLogin})
	res := fields(t, w.ok(acp.MethodSessionNew, map[string]any{"cwd": "/tmp", "mcpServers": []any{}}))
	w.barrier()

	if got := string(res["sessionId"]); got != `"`+fakeSessionID+`"` {
		t.Fatalf("sessionId %s", got)
	}
	const models = `{"availableModels":[{"modelId":"grok-4.6","name":"Grok 4.6"},{"modelId":"composer-2.5","name":"Composer 2.5"},` +
		`{"modelId":"claude-opus-5","name":"Claude Opus 5"},{"modelId":"glm-5.2","name":"GLM 5.2"}],"currentModelId":"grok-4.6"}`
	if got := string(res["models"]); got != models {
		t.Fatalf("models %s", got)
	}
	if !strings.Contains(string(res["modes"]), `"currentModeId":"agent"`) {
		t.Fatalf("modes %s", res["modes"])
	}
	var catalog []pmOption
	if err := json.Unmarshal(res["configOptions"], &catalog); err != nil {
		t.Fatal(err)
	}
	if got := ids(catalog); got != pmGrok {
		t.Fatalf("session/new's catalog is %s, want %s", got, pmGrok)
	}
	model := option(t, catalog, "model")
	if len(model.Options) != 4 || model.Category != "model" {
		t.Fatalf("the model option %+v", model)
	}
	fast := option(t, catalog, "fast")
	if fast.Options[1].Name != "Fast\u200b\u200b" || fast.Category != "model_config" {
		t.Fatalf("grok-4.6's fast %+v", fast)
	}
	// The raw bytes, key order and all: cursor's object, not a re-spelling.
	const effort = `{"id":"effort","name":"Effort","description":"Effort the model uses to generate its response.",` +
		`"category":"thought_level","type":"select","currentValue":"high","options":[{"value":"low","name":"Low"},` +
		`{"value":"medium","name":"Medium"},{"value":"high","name":"High"},{"value":"xhigh","name":"Extra High"}]}`
	if !strings.Contains(string(res["configOptions"]), effort) {
		t.Fatalf("grok-4.6's effort is not cursor's object: %s", res["configOptions"])
	}
}

// TestPermodelAModelSwitchAnswersTheCatalogAndPushesNothing is the capture:
// set_config_option(model, X) answers X's full catalog and nothing is written
// after it, for each of the four models.
func TestPermodelAModelSwitchAnswersTheCatalogAndPushesNothing(t *testing.T) {
	w := dialPermodel(t, "permodel")
	w.start()
	for _, tc := range []struct{ model, want string }{
		{"composer-2.5", pmComposer},
		{"claude-opus-5", pmOpus},
		{"glm-5.2", pmGLM},
		{"grok-4.6", pmGrok},
	} {
		if got := ids(w.catalogReply("model", tc.model)); got != tc.want {
			t.Fatalf("switching to %s answered %s, want %s", tc.model, got, tc.want)
		}
		w.quiet("the switch to " + tc.model)
	}
}

// TestPermodelValuesPersistPerModel: a value set on one model is still set when
// the session comes back to it, and another model's defaults are untouched.
func TestPermodelValuesPersistPerModel(t *testing.T) {
	w := dialPermodel(t, "permodel")
	w.start()
	if got := ids(w.catalogReply("effort", "low")); got != "mode=agent model=grok-4.6 effort=low fast=true" {
		t.Fatalf("effort=low answered %s", got)
	}
	w.quiet("effort=low")
	w.catalogReply("model", "claude-opus-5")
	w.catalogReply("context", "1m")
	w.catalogReply("model", "composer-2.5")
	if got := ids(w.catalogReply("model", "grok-4.6")); got != "mode=agent model=grok-4.6 effort=low fast=true" {
		t.Fatalf("back on grok-4.6: %s", got)
	}
	if got := ids(w.catalogReply("model", "claude-opus-5")); got != "mode=agent model=claude-opus-5 thinking=true context=1m effort=high fast=false" {
		t.Fatalf("back on claude-opus-5: %s", got)
	}
}

// TestPermodelRefusals: both of cursor's refusals, verbatim — -32602
// "Invalid params" with only data.message telling them apart — and a refused
// write changes nothing.
func TestPermodelRefusals(t *testing.T) {
	w := dialPermodel(t, "permodel")
	w.start()
	w.catalogReply("model", "composer-2.5")
	for _, tc := range []struct {
		method string
		params map[string]string
		data   string
	}{
		{acp.MethodSessionSetConfig, map[string]string{"configId": "model", "value": "nope-model"}, `{"message":"Invalid model value: nope-model"}`},
		{acp.MethodSessionSetConfig, map[string]string{"configId": "effort", "value": "high"}, `{"message":"Unknown model config option: effort"}`},
		{acp.MethodSessionSetConfig, map[string]string{"configId": "no-such-option", "value": "x"}, `{"message":"Unknown model config option: no-such-option"}`},
		{acp.MethodSessionSetModel, map[string]string{"modelId": "nope-model"}, `{"message":"Invalid model value: nope-model"}`},
	} {
		tc.params["sessionId"] = fakeSessionID
		resp, before := w.call(tc.method, tc.params)
		if len(before) != 0 || resp.Error == nil {
			t.Fatalf("%s %v: %s then %+v", tc.method, tc.params, frameNames(before), resp)
		}
		if resp.Error.Code != -32602 || resp.Error.Message != "Invalid params" || string(resp.Error.Data) != tc.data {
			t.Fatalf("%s %v refused %d %q %s, want -32602 \"Invalid params\" %s",
				tc.method, tc.params, resp.Error.Code, resp.Error.Message, resp.Error.Data, tc.data)
		}
		w.quiet(tc.method + " refused")
	}
	if model, catalog := w.state(); model != "composer-2.5" || ids(catalog) != pmComposer {
		t.Fatalf("after the refusals the fake is on %s with %s", model, ids(catalog))
	}
}

// TestPermodelSetModelAnswersNothingButSwitches: session/set_model answers {}
// and pushes nothing — a client learns no catalog from it — while the fake's
// catalog has moved with the model, as cursor's does server side.
func TestPermodelSetModelAnswersNothingButSwitches(t *testing.T) {
	w := dialPermodel(t, "permodel")
	w.start()
	if got := string(w.ok(acp.MethodSessionSetModel, map[string]string{"sessionId": fakeSessionID, "modelId": "composer-2.5"})); got != "{}" {
		t.Fatalf("set_model answered %s", got)
	}
	w.quiet("set_model")
	if got := ids(w.catalogReply("fast", "true")); got != "mode=agent model=composer-2.5 fast=true" {
		t.Fatalf("the next write answered %s: the catalog did not move with the model", got)
	}
}

// TestPermodelModeWritesPushTheMode: set_config_option(mode, …) answers the
// catalog and THEN pushes a current_mode_update — the capture's reason a
// "no-op" mode write is not one — and session/set_mode is the cursor scripts'
// {} and push, both on the session being served.
func TestPermodelModeWritesPushTheMode(t *testing.T) {
	w := dialPermodel(t, "permodel")
	w.start()
	if got := ids(w.catalogReply("mode", "plan")); got != "mode=plan model=grok-4.6 effort=high fast=true" {
		t.Fatalf("mode=plan answered %s", got)
	}
	assertModePush(t, w.barrier(), "plan")
	if got := string(w.ok(acp.MethodSessionSetMode, map[string]string{"sessionId": fakeSessionID, "modeId": "ask"})); got != "{}" {
		t.Fatalf("set_mode answered %s", got)
	}
	assertModePush(t, w.barrier(), "ask")
	if got := ids(w.catalogReply("model", "glm-5.2")); got != "mode=ask model=glm-5.2 reasoning=high" {
		t.Fatalf("the catalog after set_mode is %s", got)
	}
}

func assertModePush(t *testing.T, frames []*acp.Message, mode string) {
	t.Helper()
	if len(frames) != 1 {
		t.Fatalf("after the mode write the fake wrote %s, want one current_mode_update", frameNames(frames))
	}
	kind, upd := updateOf(t, frames[0])
	if kind != acp.UpdateCurrentMode || string(upd["currentModeId"]) != `"`+mode+`"` {
		t.Fatalf("the push is %s %s, want current_mode_update %s", kind, upd["currentModeId"], mode)
	}
}

// TestPermodelLoadAnswersTheCurrentCatalog: session/load replays nothing and
// answers session/new's shape for the model the session is on, catalog
// included, and the turn after it streams on the loaded id.
func TestPermodelLoadAnswersTheCurrentCatalog(t *testing.T) {
	w := dialPermodel(t, "permodel")
	w.start()
	w.catalogReply("model", "claude-opus-5")
	res := fields(t, w.ok(acp.MethodSessionLoad, map[string]any{"sessionId": "loaded-7", "cwd": "/tmp", "mcpServers": []any{}}))
	if _, ok := res["sessionId"]; ok {
		t.Fatalf("the load result echoes a sessionId: %v", res)
	}
	if !strings.Contains(string(res["models"]), `"currentModelId":"claude-opus-5"`) || res["modes"] == nil {
		t.Fatalf("the load result is %v", res)
	}
	var catalog []pmOption
	if err := json.Unmarshal(res["configOptions"], &catalog); err != nil {
		t.Fatal(err)
	}
	if got := ids(catalog); got != pmOpus {
		t.Fatalf("the load result's catalog is %s, want %s", got, pmOpus)
	}
	w.quiet("session/load")

	// The echo turn, on the loaded session: every frame before the answer is
	// an update, and the text on loaded-7 is "echo: hi".
	resp, before := w.call(acp.MethodSessionPrompt, map[string]any{
		"sessionId": "loaded-7", "prompt": []map[string]string{{"type": "text", "text": "hi"}},
	})
	if resp.Error != nil || !strings.Contains(string(resp.Result), acp.StopEndTurn) {
		t.Fatalf("the prompt ended %+v", resp)
	}
	var text string
	for _, f := range before {
		var n struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		_ = json.Unmarshal(f.Params, &n)
		if n.SessionID == "loaded-7" {
			text += n.Update.Content.Text
		}
	}
	if text != "echo: hi" {
		t.Fatalf("the turn on the loaded session said %q", text)
	}
}

// TestPermodelVariants: each variant is the same server with one behaviour
// changed, and the model switch still happens where the variant says it does.
func TestPermodelVariants(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		w := dialPermodel(t, "permodel-empty")
		w.start()
		for _, set := range [][2]string{{"model", "composer-2.5"}, {"fast", "true"}} {
			if got := string(w.ok(acp.MethodSessionSetConfig, map[string]string{"sessionId": fakeSessionID, "configId": set[0], "value": set[1]})); got != `{"configOptions":[]}` {
				t.Fatalf("%s=%s answered %s", set[0], set[1], got)
			}
			w.quiet(set[0])
		}
		if model, catalog := w.state(); model != "composer-2.5" || ids(catalog) != "mode=agent model=composer-2.5 fast=true" {
			t.Fatalf("the writes did not take: %s %s", model, ids(catalog))
		}
		// Refusals are still refusals.
		if resp, _ := w.setConfig("effort", "high"); resp.Error == nil {
			t.Fatalf("effort on composer-2.5 answered %s", resp.Result)
		}
	})
	t.Run("noreply", func(t *testing.T) {
		w := dialPermodel(t, "permodel-noreply")
		w.start()
		if got := string(w.ok(acp.MethodSessionSetConfig, map[string]string{"sessionId": fakeSessionID, "configId": "model", "value": "composer-2.5"})); got != "{}" {
			t.Fatalf("the model switch answered %s", got)
		}
		w.quiet("the model switch")
		if got := ids(w.catalogReply("fast", "true")); got != "mode=agent model=composer-2.5 fast=true" {
			t.Fatalf("a knob write answered %s", got)
		}
	})
	t.Run("refuse", func(t *testing.T) {
		w := dialPermodel(t, "permodel-refuse")
		cat := w.start()["configOptions"]
		if !strings.Contains(string(cat), `"id":"model"`) {
			t.Fatalf("permodel-refuse must still advertise the model option: %s", cat)
		}
		resp, _ := w.setConfig("model", "composer-2.5")
		if resp.Error == nil || string(resp.Error.Data) != `{"message":"Unknown model config option: model"}` {
			t.Fatalf("the model write answered %+v", resp)
		}
		if model, _ := w.state(); model != "grok-4.6" {
			t.Fatalf("a refused model write moved the model to %s", model)
		}
		if got := string(w.ok(acp.MethodSessionSetModel, map[string]string{"sessionId": fakeSessionID, "modelId": "composer-2.5"})); got != "{}" {
			t.Fatalf("set_model answered %s", got)
		}
		w.quiet("set_model")
		if model, catalog := w.state(); model != "composer-2.5" || ids(catalog) != pmComposer {
			t.Fatalf("set_model left %s %s", model, ids(catalog))
		}
	})
	t.Run("nomodel", func(t *testing.T) {
		w := dialPermodel(t, "permodel-nomodel")
		var catalog []pmOption
		if err := json.Unmarshal(w.start()["configOptions"], &catalog); err != nil {
			t.Fatal(err)
		}
		if got := ids(catalog); got != "mode=agent effort=high fast=true" {
			t.Fatalf("session/new's catalog is %s", got)
		}
		resp, _ := w.setConfig("model", "composer-2.5")
		if resp.Error == nil || string(resp.Error.Data) != `{"message":"Unknown model config option: model"}` {
			t.Fatalf("the model write answered %+v", resp)
		}
		w.ok(acp.MethodSessionSetModel, map[string]string{"sessionId": fakeSessionID, "modelId": "claude-opus-5"})
		if got := ids(w.catalogReply("fast", "true")); got != "mode=agent thinking=true context=300k effort=high fast=true" {
			t.Fatalf("after set_model a knob write answered %s", got)
		}
	})
}

// TestPermodelPushOrder is the ordering barrier: on set_config_option(model,
// X) the push lands immediately before or immediately after the reply, never
// anywhere else, and carries the value the file documents, so "the later one
// wins" can be asserted by value. The fake's state afterwards is the later of
// the two.
func TestPermodelPushOrder(t *testing.T) {
	for _, tc := range []struct {
		model string
		reply string // the reply's catalog
		moved string // pushbefore's and pushafter's push
		next  string // pushmodel-after's push
	}{
		{"grok-4.6", pmGrok, "mode=agent model=grok-4.6 effort=high fast=false", pmComposer},
		{"composer-2.5", pmComposer, "mode=agent model=composer-2.5 fast=true", pmOpus},
		{"claude-opus-5", pmOpus, "mode=agent model=claude-opus-5 thinking=true context=300k effort=high fast=true", pmGLM},
		{"glm-5.2", pmGLM, "mode=agent model=glm-5.2 reasoning=max", pmGrok},
	} {
		t.Run("pushbefore/"+tc.model, func(t *testing.T) {
			w := dialPermodel(t, "permodel-pushbefore")
			w.start()
			resp, before := w.setConfig("model", tc.model)
			if resp.Error != nil || len(before) != 1 {
				t.Fatalf("the switch wrote %s then %+v, want one push then the reply", frameNames(before), resp)
			}
			if got := ids(pushedCatalog(t, before[0])); got != tc.moved {
				t.Fatalf("the push before carries %s, want %s", got, tc.moved)
			}
			if got := ids(replyCatalog(t, resp.Result)); got != tc.reply {
				t.Fatalf("the reply carries %s, want %s", got, tc.reply)
			}
			w.quiet("the reply")
			if _, catalog := w.state(); ids(catalog) != tc.reply {
				t.Fatalf("the fake holds %s, want the reply's %s", ids(catalog), tc.reply)
			}
		})
		t.Run("pushafter/"+tc.model, func(t *testing.T) {
			w := dialPermodel(t, "permodel-pushafter")
			w.start()
			if got := ids(w.catalogReply("model", tc.model)); got != tc.reply {
				t.Fatalf("the reply carries %s, want %s", got, tc.reply)
			}
			after := w.barrier()
			if len(after) != 1 {
				t.Fatalf("after the reply the fake wrote %s, want one push", frameNames(after))
			}
			if got := ids(pushedCatalog(t, after[0])); got != tc.moved {
				t.Fatalf("the push after carries %s, want %s", got, tc.moved)
			}
			if _, catalog := w.state(); ids(catalog) != tc.moved {
				t.Fatalf("the fake holds %s, want the push's %s", ids(catalog), tc.moved)
			}
		})
		t.Run("pushmodel-after/"+tc.model, func(t *testing.T) {
			w := dialPermodel(t, "permodel-pushmodel-after")
			w.start()
			if got := ids(w.catalogReply("model", tc.model)); got != tc.reply {
				t.Fatalf("the reply carries %s, want %s", got, tc.reply)
			}
			after := w.barrier()
			if len(after) != 1 {
				t.Fatalf("after the reply the fake wrote %s, want one push", frameNames(after))
			}
			if got := ids(pushedCatalog(t, after[0])); got != tc.next {
				t.Fatalf("the push after carries %s, want %s", got, tc.next)
			}
			if _, catalog := w.state(); ids(catalog) != tc.next {
				t.Fatalf("the fake holds %s, want the push's %s", ids(catalog), tc.next)
			}
		})
	}
	// A knob write and set_model push nothing on the ordering variants: only
	// the model option's write is a barrier.
	w := dialPermodel(t, "permodel-pushafter")
	w.start()
	w.catalogReply("effort", "low")
	w.quiet("a knob write")
	w.ok(acp.MethodSessionSetModel, map[string]string{"sessionId": fakeSessionID, "modelId": "composer-2.5"})
	w.quiet("set_model")
}
