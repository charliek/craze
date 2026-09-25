package protocol_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
)

// jsonOf is v as the wire writes it (MarshalLine, without its newline).
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := protocol.MarshalLine(v)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

var (
	instanceTime = time.Date(2026, 9, 25, 10, 30, 45, 123456789, time.UTC)
	queuedRow    = json.RawMessage(`{"id":"q-1","text":"next","queuedAt":"2026-09-25T10:30:45.123456789Z","version":1}`)
	permBody     = json.RawMessage(`{"permission":{"id":"perm-1","tool":"Shell","options":[{"optionId":"allow-once","name":"Allow","kind":"allow_once"},{"optionId":"allow-once-2","name":"Allow, and stop asking","kind":"allow_once"}]}}`)
	snapshotBody = json.RawMessage(`{"version":1,"incarnation":"inc-1","seq":7,"queue":[{"id":"q-1","text":"next"}],"main":{"entries":[{"id":"4.0","kind":"user","text":"fix it"}]},"subs":[{"id":"sub-1","omitted":[[12,"t-1"],[3]]}]}`)
	configBody   = json.RawMessage(`{"options":[{"id":"effort","name":"Effort","type":"select","current":"high","selectValues":[{"value":"high","name":"high"}]}]}`)
)

func sessionInfo() protocol.SessionInfo {
	return protocol.SessionInfo{
		SessionID:         "0190ab12-cd34-7ef0-8123-456789abcdef",
		ProviderSessionID: "prov-1",
		Incarnation:       "inc-1",
		HostID:            "0190ab12cd34",
		Workspace:         "/work",
		Provider:          protocol.Provider{Name: "grok", Label: "Grok"},
		Catalogs: protocol.Catalogs{
			Models: []protocol.CatalogModel{{ID: "grok-4.6", Name: "Grok 4.6"}},
			Modes:  []protocol.CatalogMode{{ID: "agent", Name: "Agent", Description: "edits files"}},
		},
		Capabilities: protocol.SessionCapabilities{Interject: true, Modes: true, Cancel: true, Approvals: true, HistoryCursor: true},
		RetryHorizon: protocol.RetryHorizon{Commands: 4096, AgeMs: 600000},
	}
}

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }

// TestInstancesValidate holds a handful of hand-built instances per method
// and notification to the schema: each built from the Go types (so the value
// the server will marshal is what the schema accepts) or written out, and the
// invalid ones — an unknown params field, a bad enum, a missing member, a null
// where a list belongs, a reason under the wrong code — refused.
func TestInstancesValidate(t *testing.T) {
	info := sessionInfo()
	emptyCatalogs := info
	emptyCatalogs.Catalogs = protocol.Catalogs{Models: []protocol.CatalogModel{}, Modes: []protocol.CatalogMode{}}
	nilCatalogs := info
	nilCatalogs.Catalogs = protocol.Catalogs{}
	row := protocol.SessionRow{SessionInfo: info, Title: "fix it", Activity: protocol.ActivityWorking, PendingAsks: 1,
		HeadAsk: &protocol.HeadAsk{ID: "perm-1", Kind: "permission", Label: "permission Shell"}}
	state := protocol.StateResult{
		Activity: protocol.ActivityWorking, Turn: "turn-3", Queue: []json.RawMessage{queuedRow}, PendingAsks: 0,
		SendNow:  &protocol.ArmedSend{Text: "now", FromRow: "q-2", Turn: "turn-3", Cause: "c-1/4"},
		Settings: protocol.Settings{Model: "grok-4.6", Mode: "agent", Config: configBody},
	}
	stateNilQueue := state
	stateNilQueue.Queue = nil
	stateNoConfig := state
	stateNoConfig.Settings.Config = json.RawMessage(`{}`)
	hello := protocol.HelloResult{
		Protocol: protocol.ProtocolVersion,
		Endpoint: protocol.Endpoint{Kind: protocol.EndpointHost, HostID: "0190ab12cd34", CrazeVersion: "0.1.0", PID: 4242},
		ClientID: "c-3", Token: "00112233445566778899aabbccddeeff", Resumed: true,
		Capabilities: protocol.HostCapabilities(),
		Codecs:       protocol.Codecs{Event: 1, Snapshot: 1},
		Limits:       protocol.HostLimits(),
		RetryHorizon: protocol.RetryHorizon{Commands: 4096, AgeMs: 600000},
	}
	fresh := hello
	fresh.Resumed = false
	hub := protocol.HubHelloResult{
		Protocol:     protocol.ProtocolVersion,
		Endpoint:     protocol.Endpoint{Kind: protocol.EndpointHub, HostID: "hub-1", CrazeVersion: "0.4.0", PID: 77},
		Capabilities: protocol.ConnectionCapabilities{RosterSubscribe: true, SessionCreate: true, Connect: true},
		Codecs:       protocol.Codecs{Event: 1, Snapshot: 1},
		Limits:       protocol.HostLimits(),
	}
	hubAsHost := hub
	hubAsHost.Endpoint.Kind = protocol.EndpointHost
	// without drops one member from a JSON object written by jsonOf.
	without := func(doc, member string) string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatal(err)
		}
		delete(m, member)
		return jsonOf(t, m)
	}
	// with adds one member to a JSON object written by jsonOf.
	with := func(doc, member, value string) string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatal(err)
		}
		m[member] = json.RawMessage(value)
		return jsonOf(t, m)
	}
	record := protocol.AskRecord{ID: "perm-1", Kind: "permission", Status: protocol.AskResolved, Outcome: "answered", By: "client",
		Answer: &protocol.Answer{OptionID: "allow-once-2"}, Body: permBody, OpenedAt: instanceTime, ResolvedAt: instanceTime.Add(time.Second)}
	openRecord := protocol.AskRecord{ID: "perm-2", Kind: "permission", Status: protocol.AskOpen, Body: permBody, OpenedAt: instanceTime, Truncated: true}
	errResp := func(code protocol.Code, reason protocol.Reason) string {
		return jsonOf(t, protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(`7`),
			Error: &protocol.Error{Code: protocol.RPCRefused, Message: "refused", Data: protocol.ErrorData{Code: code, Reason: reason}}})
	}

	type inst struct {
		name, file, def, json string
		ok                    bool
	}
	params := func(file string) string { return file }
	cases := []inst{
		// hello: tolerant params, strict result.
		{"hello", "hello.json", "params", jsonOf(t, protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{
			Kind: "shed", Name: "shed-mobile", Version: "0.4.0", Capabilities: &protocol.ClientCapabilities{}},
			Resume: &protocol.Resume{ClientID: "c-3", Token: "00112233445566778899aabbccddeeff"}}), true},
		{"hello, fields it does not know at every depth", "hello.json", "params",
			`{"protocols":[1,2],"client":{"kind":"tui","platform":"ios","capabilities":{"future":true}},"resume":{"clientId":"c-1","token":"t","why":1},"later":{}}`, true},
		{"hello, reserved members null", "hello.json", "params", `{"protocols":[1],"client":{"kind":"tui"},"auth":null,"via":null}`, true},
		{"hello with auth set", "hello.json", "params", `{"protocols":[1],"client":{"kind":"tui"},"auth":{"token":"x"}}`, false},
		{"hello with no protocols", "hello.json", "params", `{"client":{"kind":"tui"}}`, false},
		{"hello with an empty protocols list", "hello.json", "params", `{"protocols":[],"client":{"kind":"tui"}}`, false},
		{"hello with no client", "hello.json", "params", `{"protocols":[1]}`, false},
		// hello's result: a host's or a hub's, by endpoint.kind (X5).
		{"a host's hello, resumed", "hello.json", "result", jsonOf(t, hello), true},
		{"a host's hello, not resumed: resumed false is written", "hello.json", "result", jsonOf(t, fresh), true},
		{"a host's hello missing clientId", "hello.json", "result", without(jsonOf(t, hello), "clientId"), false},
		{"a host's hello missing token", "hello.json", "result", without(jsonOf(t, hello), "token"), false},
		{"a host's hello missing resumed", "hello.json", "result", without(jsonOf(t, fresh), "resumed"), false},
		{"a host's hello missing retryHorizon", "hello.json", "result", without(jsonOf(t, hello), "retryHorizon"), false},
		{"a host's hello missing all four host members", "hello.json", "result", jsonOf(t, hubAsHost), false},
		{"a host's hello calling itself a hub", "hello.json", "result", strings.Replace(jsonOf(t, hello), `"kind":"host"`, `"kind":"hub"`, 1), false},
		{"a host's hello with a field it does not define", "hello.json", "result", with(jsonOf(t, hello), "extra", "1"), false},
		{"a hub's hello", "hello.json", "result", jsonOf(t, hub), true},
		{"a hub's hello carrying clientId", "hello.json", "result", with(jsonOf(t, hub), "clientId", `"c-1"`), false},
		{"a hub's hello carrying resumed", "hello.json", "result", with(jsonOf(t, hub), "resumed", "false"), false},
		{"a hub's hello carrying retryHorizon", "hello.json", "result",
			with(jsonOf(t, hub), "retryHorizon", `{"commands":1,"ageMs":1}`), false},
		{"a hello from an endpoint of neither kind", "hello.json", "result", strings.Replace(jsonOf(t, hub), `"kind":"hub"`, `"kind":"relay"`, 1), false},
		{"a hello with no endpoint", "hello.json", "result", without(jsonOf(t, hub), "endpoint"), false},
		{"hello's protocol_version result", "hello.json", "errorResult", jsonOf(t, protocol.HelloErrorResult{Supported: protocol.SupportedProtocols()}), true},

		{"sessions.list", "sessions.list.json", "params", `{}`, true},
		{"sessions.list with a field", "sessions.list.json", "params", `{"sessionId":"s"}`, false},
		{"the roster", "sessions.list.json", "result", jsonOf(t, protocol.SessionsListResult{Epoch: "0190ab12cd34", Cursor: 42, Sessions: []protocol.SessionRow{row}}), true},
		{"an empty roster", "sessions.list.json", "result", jsonOf(t, protocol.SessionsListResult{Epoch: "h", Sessions: []protocol.SessionRow{}}), true},
		{"a roster row with a field neither half defines", "sessions.list.json", "result",
			strings.Replace(jsonOf(t, protocol.SessionsListResult{Epoch: "h", Sessions: []protocol.SessionRow{row}}), `"title"`, `"cwd":"/x","title"`, 1), false},
		{"a roster row with a bad activity", "sessions.list.json", "result",
			strings.Replace(jsonOf(t, protocol.SessionsListResult{Epoch: "h", Sessions: []protocol.SessionRow{row}}), `"working"`, `"busy"`, 1), false},
		{"a roster row with no hostId", "sessions.list.json", "result",
			strings.Replace(jsonOf(t, protocol.SessionsListResult{Epoch: "h", Sessions: []protocol.SessionRow{row}}), `"hostId":"0190ab12cd34",`, ``, 1), false},

		{"sessions.subscribe", "sessions.subscribe.json", "params", `{}`, true},
		{"sessions.subscribe has no result a host sends", "sessions.subscribe.json", "result", `{}`, false},
		{"session.connect", "session.connect.json", "params", jsonOf(t, protocol.ConnectParams{SessionID: "s"}), true},
		{"session.connect with no session", "session.connect.json", "params", `{}`, false},
		{"session.connect's reply", "session.connect.json", "result", jsonOf(t, protocol.Empty{}), true},

		{"session.attach with a cursor, now, a budget", "session.attach.json", "params", jsonOf(t, protocol.AttachParams{SessionID: "s",
			Cursor: &protocol.Cursor{Incarnation: "inc-1", Seq: 42}, When: protocol.WhenNow, Budget: &protocol.AttachBudget{MaxItems: 64}}), true},
		{"session.attach, the defaults", "session.attach.json", "params", `{"sessionId":"s"}`, true},
		{"session.attach with a bad when", "session.attach.json", "params", `{"sessionId":"s","when":"later"}`, false},
		{"session.attach with a zero budget", "session.attach.json", "params", `{"sessionId":"s","budget":{"maxItems":0}}`, false},
		{"session.attach with half a cursor", "session.attach.json", "params", `{"sessionId":"s","cursor":{"incarnation":"i"}}`, false},
		{"session.attach with an unknown field", "session.attach.json", "params", `{"sessionId":"s","follow":true}`, false},
		{"the attach reply, with a snapshot", "session.attach.json", "result", jsonOf(t, protocol.AttachResult{Subscription: "s-1", Session: info,
			Ready: true, After: protocol.Cursor{Incarnation: "inc-1", Seq: 7}, Snapshot: snapshotBody, Reset: protocol.CursorForeignIncarnation}), true},
		{"the attach reply before readiness", "session.attach.json", "result", jsonOf(t, protocol.AttachResult{Subscription: "s-1", Session: emptyCatalogs,
			After: protocol.Cursor{Incarnation: "inc-1"}}), true},
		{"the attach reply with catalogs null", "session.attach.json", "result", jsonOf(t, protocol.AttachResult{Subscription: "s-1", Session: nilCatalogs,
			After: protocol.Cursor{Incarnation: "inc-1"}}), false},
		{"the attach reply with a snapshot of another version", "session.attach.json", "result", jsonOf(t, protocol.AttachResult{Subscription: "s-1",
			Session: info, After: protocol.Cursor{Incarnation: "inc-1"}, Snapshot: json.RawMessage(`{"version":2,"main":{}}`)}), false},
		{"the attach reply with a bad reset", "session.attach.json", "result", jsonOf(t, protocol.AttachResult{Subscription: "s-1",
			Session: info, After: protocol.Cursor{Incarnation: "inc-1"}, Reset: "gone"}), false},

		{"session.detach", "session.detach.json", "params", jsonOf(t, protocol.DetachParams{SessionID: "s", Subscription: "s-1"}), true},
		{"session.detach with no subscription", "session.detach.json", "params", `{"sessionId":"s"}`, false},
		{"session.state", "session.state.json", "params", jsonOf(t, protocol.StateParams{SessionID: "s"}), true},
		{"the state", "session.state.json", "result", jsonOf(t, state), true},
		{"the state with no config options", "session.state.json", "result", jsonOf(t, stateNoConfig), true},
		{"the state with its queue null", "session.state.json", "result", jsonOf(t, stateNilQueue), false},
		{"the state with a bad activity", "session.state.json", "result", strings.Replace(jsonOf(t, state), `"working"`, `"sleeping"`, 1), false},
		{"the state with a config that is not the codec's", "session.state.json", "result", strings.Replace(jsonOf(t, state), `{"options"`, `{"opts"`, 1), false},

		{"session.snapshot of a child", "session.snapshot.json", "params", jsonOf(t, protocol.SnapshotParams{SessionID: "s", AgentID: "sub-1",
			Budget: &protocol.SnapshotBudget{SnapshotBytes: 65536}}), true},
		{"session.snapshot with an attach budget", "session.snapshot.json", "params", `{"sessionId":"s","budget":{"maxItems":3}}`, false},
		{"a snapshot", "session.snapshot.json", "result", jsonOf(t, protocol.SnapshotResult{Snapshot: snapshotBody}), true},
		{"a snapshot whose ledger record is malformed", "session.snapshot.json", "result", `{"snapshot":{"version":1,"main":{"omitted":[["12"]]}}}`, false},
		{"session.sync", "session.sync.json", "params", jsonOf(t, protocol.SyncParams{SessionID: "s"}), true},
		{"the sync reply", "session.sync.json", "result", jsonOf(t, protocol.SyncResult{Seq: 9}), true},

		{"session.prompt", "session.prompt.json", "params", jsonOf(t, protocol.PromptParams{SessionID: "s", CommandID: "1", Text: "fix it", Mode: protocol.PromptQueue}), true},
		{"session.prompt from a row, send now", "session.prompt.json", "params", jsonOf(t, protocol.PromptParams{SessionID: "s", CommandID: "12", FromRow: "q-1", Mode: protocol.PromptSendNow}), true},
		{"session.prompt with a bad mode", "session.prompt.json", "params", `{"sessionId":"s","commandId":"1","text":"x","mode":"later"}`, false},
		{"session.prompt with no mode", "session.prompt.json", "params", `{"sessionId":"s","commandId":"1","text":"x"}`, false},
		{"session.prompt with a leading zero", "session.prompt.json", "params", `{"sessionId":"s","commandId":"07","mode":"queue"}`, false},
		{"session.prompt with command 0", "session.prompt.json", "params", `{"sessionId":"s","commandId":"0","mode":"queue"}`, false},
		{"session.prompt with a numeric command id", "session.prompt.json", "params", `{"sessionId":"s","commandId":7,"mode":"queue"}`, false},
		{"session.prompt with no command id", "session.prompt.json", "params", `{"sessionId":"s","mode":"queue"}`, false},
		{"a prompt that started a turn", "session.prompt.json", "result", jsonOf(t, protocol.PromptResult{Turn: "turn-3", Text: strp("fix it")}), true},
		{"a prompt that started a turn with no text", "session.prompt.json", "result", jsonOf(t, protocol.PromptResult{Turn: "turn-3", Text: strp("")}), true},
		{"a prompt that was queued", "session.prompt.json", "result", jsonOf(t, protocol.PromptResult{Queued: queuedRow}), true},
		{"a prompt that was armed", "session.prompt.json", "result", jsonOf(t, protocol.PromptResult{Armed: true}), true},
		{"an interjection", "session.prompt.json", "result", jsonOf(t, protocol.PromptResult{}), true},
		{"a turn without its text", "session.prompt.json", "result", `{"turn":"turn-3"}`, false},
		{"a turn and armed", "session.prompt.json", "result", `{"turn":"turn-3","text":"x","armed":true}`, false},
		{"text and a row", "session.prompt.json", "result", `{"text":"x","queued":{"id":"q-1"}}`, false},
		{"armed false", "session.prompt.json", "result", `{"armed":false}`, false},

		{"session.cancel of a named turn", "session.cancel.json", "params", jsonOf(t, protocol.CancelParams{SessionID: "s", CommandID: "3", TurnID: "turn-3"}), true},
		{"a cancel's result", "session.cancel.json", "result", jsonOf(t, protocol.CancelResult{Outcome: protocol.CancelRequested, Turn: "turn-3"}), true},
		{"a cancel's result with no turn", "session.cancel.json", "result", jsonOf(t, protocol.CancelResult{Outcome: protocol.CancelSettled}), true},
		{"a cancel's result beside its error", "session.cancel.json", "errorResult", jsonOf(t, protocol.CancelResult{Outcome: protocol.CancelUnknown, Reported: true}), true},
		{"a cancel's bad outcome", "session.cancel.json", "result", `{"outcome":"maybe","turn":"","reported":false}`, false},
		{"session.disarm", "session.disarm.json", "params", jsonOf(t, protocol.DisarmParams{SessionID: "s", CommandID: "4"}), true},

		{"session.queue.add", "session.queue.add.json", "params", jsonOf(t, protocol.QueueAddParams{SessionID: "s", CommandID: "5", Text: "later"}), true},
		{"session.queue.add with no text", "session.queue.add.json", "params", `{"sessionId":"s","commandId":"5"}`, false},
		{"the added row", "session.queue.add.json", "result", jsonOf(t, protocol.QueueAddResult{Row: queuedRow}), true},
		{"an added row that is not the codec's", "session.queue.add.json", "result", `{"row":{"id":"q-1","body":"x"}}`, false},
		{"session.queue.edit on version 0", "session.queue.edit.json", "params", jsonOf(t, protocol.QueueEditParams{SessionID: "s", CommandID: "6",
			RowID: "q-1", Text: "edited", ExpectedVersion: intp(0)}), true},
		{"session.queue.edit on a negative version", "session.queue.edit.json", "params", `{"sessionId":"s","commandId":"6","rowId":"q-1","text":"x","expectedVersion":-1}`, false},
		{"session.queue.remove", "session.queue.remove.json", "params", jsonOf(t, protocol.QueueRemoveParams{SessionID: "s", CommandID: "7", RowID: "q-1"}), true},
		{"the removed row", "session.queue.remove.json", "result", jsonOf(t, protocol.QueueRemoveResult{Row: queuedRow}), true},
		{"session.queue.clear", "session.queue.clear.json", "params", jsonOf(t, protocol.QueueClearParams{SessionID: "s", CommandID: "8"}), true},
		{"the cleared rows", "session.queue.clear.json", "result", jsonOf(t, protocol.QueueClearResult{Removed: []json.RawMessage{queuedRow, queuedRow}}), true},
		{"an empty queue cleared", "session.queue.clear.json", "result", jsonOf(t, protocol.QueueClearResult{Removed: []json.RawMessage{}}), true},
		{"cleared rows null", "session.queue.clear.json", "result", jsonOf(t, protocol.QueueClearResult{}), false},

		{"session.set of a bound config option", "session.set.json", "params", jsonOf(t, protocol.SetParams{SessionID: "s", CommandID: "9",
			Setting: protocol.Setting{Kind: protocol.SettingConfig, ID: "effort", Value: "high", ForModel: "grok-4.6"}}), true},
		{"session.set of an empty effort", "session.set.json", "params", jsonOf(t, protocol.SetParams{SessionID: "s", CommandID: "9",
			Setting: protocol.Setting{Kind: protocol.SettingConfig, ID: "effort"}}), true},
		{"session.set of a bad kind", "session.set.json", "params", `{"sessionId":"s","commandId":"9","setting":{"kind":"theme","value":"dark"}}`, false},
		{"session.set with no value", "session.set.json", "params", `{"sessionId":"s","commandId":"9","setting":{"kind":"model"}}`, false},
		{"a set's result", "session.set.json", "result", jsonOf(t, protocol.SetResult{Value: "high", Rev: 12}), true},
		{"session.setTitle", "session.setTitle.json", "params", jsonOf(t, protocol.SetTitleParams{SessionID: "s", CommandID: "10", Title: "fix it"}), true},
		{"session.subagent.cancel", "session.subagent.cancel.json", "params", jsonOf(t, protocol.SubagentCancelParams{SessionID: "s", CommandID: "11", AgentID: "sub-1"}), true},
		{"session.stop", "session.stop.json", "params", jsonOf(t, protocol.StopParams{SessionID: "s", CommandID: "12"}), true},

		{"asks.list", "asks.list.json", "params", jsonOf(t, protocol.AsksListParams{SessionID: "s"}), true},
		{"the open asks", "asks.list.json", "result", jsonOf(t, protocol.AsksListResult{Asks: []protocol.AskSummary{
			{ID: "perm-1", Kind: "permission", Label: "permission Shell", OpenedAt: instanceTime}}}), true},
		{"an open ask summary carrying its body", "asks.list.json", "result",
			`{"asks":[{"id":"perm-1","kind":"permission","label":"x","openedAt":"2026-09-25T10:30:45Z","body":{}}]}`, false},
		{"asks.get", "asks.get.json", "params", jsonOf(t, protocol.AsksGetParams{SessionID: "s", AskID: "perm-1"}), true},
		{"an answered ask", "asks.get.json", "result", jsonOf(t, protocol.AsksGetResult{Ask: record}), true},
		{"an open ask, cut", "asks.get.json", "result", jsonOf(t, protocol.AsksGetResult{Ask: openRecord}), true},
		{"an ask with a bad status", "asks.get.json", "result", strings.Replace(jsonOf(t, protocol.AsksGetResult{Ask: openRecord}), `"open"`, `"done"`, 1), false},
		{"an ask whose body is not the codec's", "asks.get.json", "result",
			`{"ask":{"id":"a","kind":"question","status":"open","body":{"question":{"prompt":"x"}},"openedAt":"2026-09-25T10:30:45Z","truncated":false}}`, false},
		{"asks.answer with the exact option", "asks.answer.json", "params", jsonOf(t, protocol.AsksAnswerParams{SessionID: "s", CommandID: "13",
			AskID: "perm-1", Answer: protocol.Answer{OptionID: "allow-once-2"}}), true},
		{"asks.answer to a question, a null list and an empty one", "asks.answer.json", "params", jsonOf(t, protocol.AsksAnswerParams{SessionID: "s",
			CommandID: "14", AskID: "ask-1", Answer: protocol.Answer{Answers: map[string][]string{"q1": {"a"}, "q2": nil, "q3": {}}}}), true},
		{"asks.answer to a question that asks nothing", "asks.answer.json", "params", `{"sessionId":"s","commandId":"15","askId":"ask-2","answer":{}}`, true},
		{"asks.answer by kind", "asks.answer.json", "params", `{"sessionId":"s","commandId":"16","askId":"perm-1","answer":{"kind":"allow_once"}}`, false},
		{"asks.answer with a numeric option", "asks.answer.json", "params", `{"sessionId":"s","commandId":"16","askId":"perm-1","answer":{"optionId":7}}`, false},
		{"asks.answer with no answer", "asks.answer.json", "params", `{"sessionId":"s","commandId":"16","askId":"perm-1"}`, false},
		{"an answer's reply", "asks.answer.json", "result", `{}`, true},
		{"an answer's reply with something in it", "asks.answer.json", "result", `{"ok":true}`, false},
	}
	// Every method's params with a field it does not define is refused,
	// hello's aside (and session.create's, which has no schema).
	for _, m := range protocol.Methods() {
		if m.Tolerant || m.Reserved {
			continue
		}
		cases = append(cases, inst{m.Name + " with an unknown params field", params(protocol.MethodSchema(m.Name)), "params",
			`{"sessionId":"s","commandId":"1","surprise":true}`, false})
	}
	// Notifications.
	cases = append(cases,
		inst{"an event", "notification.event.json", "params", jsonOf(t, protocol.EventParams{Subscription: "s-1", Seq: 8,
			Event: json.RawMessage(`{"type":"text","text":"a && b <c>","at":"2026-09-25T10:30:45.123456789Z"}`)}), true},
		inst{"an event of a kind the codec does not write", "notification.event.json", "params", `{"subscription":"s-1","seq":8,"event":{"type":"hologram"}}`, false},
		inst{"an event with no seq", "notification.event.json", "params", `{"subscription":"s-1","event":{"type":"done"}}`, false},
		inst{"synchronized", "notification.synchronized.json", "params", jsonOf(t, protocol.SynchronizedParams{Subscription: "s-1", Seq: 7}), true},
		inst{"a successful start", "notification.ready.json", "params", jsonOf(t, protocol.ReadyParams{Subscription: "s-1", Session: info}), true},
		inst{"a failed start", "notification.ready.json", "params", jsonOf(t, protocol.ReadyParams{Subscription: "s-1", Session: emptyCatalogs,
			StartFailed: true, Err: "agent exited"}), true},
		inst{"a reset", "notification.reset.json", "params", jsonOf(t, protocol.ResetParams{Subscription: "s-1", Reason: protocol.ResetSlowConsumer}), true},
		inst{"a reset for a reason there is none of", "notification.reset.json", "params", `{"subscription":"s-1","reason":"tired"}`, false},
	)
	// The envelope.
	cases = append(cases,
		inst{"a request", "envelope.json", "request", jsonOf(t, protocol.Request{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(`1`),
			Method: protocol.MethodHello, Params: json.RawMessage(`{"protocols":[1],"client":{"kind":"tui"}}`)}), true},
		inst{"a request with a string id and no params", "envelope.json", "request", `{"jsonrpc":"2.0","id":"a-1","method":"sessions.list"}`, true},
		inst{"a request with no id", "envelope.json", "request", `{"jsonrpc":"2.0","method":"sessions.list"}`, false},
		inst{"a request with positional params", "envelope.json", "request", `{"jsonrpc":"2.0","id":1,"method":"sessions.list","params":[]}`, false},
		inst{"a request of JSON-RPC 1", "envelope.json", "request", `{"jsonrpc":"1.0","id":1,"method":"hello"}`, false},
		inst{"a result", "envelope.json", "response", jsonOf(t, protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(`"a-1"`),
			Result: json.RawMessage(`{}`)}), true},
		inst{"an error", "envelope.json", "response", errResp(protocol.CodeNotAccepting, protocol.ReasonNotInTurn), true},
		inst{"a line too long", "envelope.json", "response", jsonOf(t, protocol.Response{JSONRPC: protocol.JSONRPCVersion,
			Error: &protocol.Error{Code: protocol.RPCInvalidRequest, Message: "line too long",
				Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonLineTooLong}}}), true},
		inst{"a cancel's error with its result", "envelope.json", "response", jsonOf(t, protocol.Response{JSONRPC: protocol.JSONRPCVersion,
			ID: json.RawMessage(`9`), Error: &protocol.Error{Code: protocol.RPCRefused, Message: "cancel failed",
				Data: protocol.ErrorData{Code: protocol.CodeFailed, Reason: protocol.ReasonFailed,
					Result: json.RawMessage(jsonOf(t, protocol.CancelResult{Outcome: protocol.CancelUnknown, Reported: true}))}}}), true},
		inst{"an index write failure with its cause", "envelope.json", "response", jsonOf(t, protocol.Response{JSONRPC: protocol.JSONRPCVersion,
			ID: json.RawMessage(`10`), Error: &protocol.Error{Code: protocol.RPCRefused, Message: "index",
				Data: protocol.ErrorData{Code: protocol.CodeIndexWrite, Reason: protocol.ReasonIndexWrite, Cause: "disk full"}}}), true},
		inst{"a result and an error", "envelope.json", "response",
			`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":-32000,"message":"x","data":{"code":"failed","reason":"failed"}}}`, false},
		inst{"an error with no data", "envelope.json", "response", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"x"}}`, false},
		inst{"an error of a JSON-RPC integer it never sends", "envelope.json", "response",
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"x","data":{"code":"failed","reason":"failed"}}}`, false},
		inst{"a client-side reason from a host", "envelope.json", "response", errResp(protocol.CodeAborted, protocol.ReasonResumeLost), false},
		inst{"a reason under another code", "envelope.json", "response", errResp(protocol.CodeUnavailable, protocol.ReasonNotInTurn), false},
		inst{"a code outside the closed set", "envelope.json", "response", errResp("teapot", "teapot"), false},
		inst{"a notification", "envelope.json", "notification", jsonOf(t, protocol.Notification{JSONRPC: protocol.JSONRPCVersion,
			Method: protocol.NotifyReset, Params: json.RawMessage(`{"subscription":"s-1","reason":"session_closed"}`)}), true},
		inst{"a notification with an id", "envelope.json", "notification", `{"jsonrpc":"2.0","id":1,"method":"event","params":{}}`, false},
		inst{"any line: a request", "envelope.json", "", `{"jsonrpc":"2.0","id":1,"method":"hello","params":{}}`, true},
		inst{"any line: a notification", "envelope.json", "", `{"jsonrpc":"2.0","method":"ready","params":{}}`, true},
		inst{"any line: a batch", "envelope.json", "", `[{"jsonrpc":"2.0","id":1,"method":"hello"}]`, false},
	)

	c := newCompiler()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frag := ""
			if tc.def != "" {
				frag = "#/$defs/" + tc.def
			}
			s := compiled(t, c, protocol.SchemaURI(tc.file, frag))
			r := s.Validate([]byte(tc.json))
			if r.IsValid() != tc.ok {
				t.Fatalf("%s%s: valid %v, want %v\ninstance: %s\nerrors: %v", tc.file, frag, r.IsValid(), tc.ok, tc.json, r.Errors)
			}
		})
	}
}

// TestEveryHostReasonValidatesUnderItsCodeAlone crosses every reason a host
// sends with every code: an error's data validates exactly when the reason
// is one the table lets a host send beside that code (§3.2).
func TestEveryHostReasonValidatesUnderItsCodeAlone(t *testing.T) {
	s := compiled(t, newCompiler(), protocol.SchemaURI(protocol.SchemaEnvelope, "#/$defs/errorData"))
	for _, r := range protocol.Reasons() {
		for _, code := range protocol.Codes() {
			data := jsonOf(t, protocol.ErrorData{Code: code, Reason: r.Reason})
			want := !r.ClientSide && r.Code == code
			if got := s.Validate([]byte(data)).IsValid(); got != want {
				t.Errorf("%s: valid %v, want %v", data, got, want)
			}
		}
	}
}
