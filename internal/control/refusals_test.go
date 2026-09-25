package control_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/protocol"
)

// TestEveryRefusalPath (§3.2, §3.3): every refusal the protocol itself makes,
// with its JSON-RPC integer, code and reason — each reply held to the schema
// by the client — and the connection going on after each.
func TestEveryRefusalPath(t *testing.T) {
	h := newHost(t)
	c := h.dial()
	state := protocol.StateParams{SessionID: sid(h)}

	// Before hello.
	refusedWith(t, c.call(protocol.MethodSessionState, state), protocol.RPCRefused, protocol.CodeBadRequest, protocol.ReasonHelloRequired)
	refusedWith(t, c.call(protocol.MethodSessionStop, protocol.StopParams{SessionID: sid(h), CommandID: "1"}),
		protocol.RPCRefused, protocol.CodeBadRequest, protocol.ReasonHelloRequired)
	// An unknown method is -32601, hello or not.
	refusedWith(t, c.call("session.frobnicate", map[string]any{}), protocol.RPCMethodNotFound, protocol.CodeUnsupported, protocol.ReasonUnknownMethod)

	// hello: no version in common, with the host's; malformed params.
	e := refusedWith(t, c.call(protocol.MethodHello, protocol.HelloParams{Protocols: []int{2, 3}, Client: protocol.ClientInfo{Kind: "t"}}),
		protocol.RPCRefused, protocol.CodeBadRequest, protocol.ReasonProtocolVersion)
	var supported protocol.HelloErrorResult
	if err := json.Unmarshal(e.Data.Result, &supported); err != nil || len(supported.Supported) != 1 || supported.Supported[0] != 1 {
		t.Fatalf("data.result %s", e.Data.Result)
	}
	for _, params := range []any{
		map[string]any{"client": map[string]any{"kind": "t"}},
		map[string]any{"protocols": []int{}, "client": map[string]any{"kind": "t"}},
		map[string]any{"protocols": []int{1}, "client": map[string]any{}},
		map[string]any{"protocols": []int{1}, "client": map[string]any{"kind": "t"}, "auth": map[string]any{"key": "x"}},
		map[string]any{"protocols": "1", "client": map[string]any{"kind": "t"}},
	} {
		refusedWith(t, c.call(protocol.MethodHello, params), protocol.RPCInvalidParams, protocol.CodeBadRequest, protocol.ReasonBadRequest)
	}
	// hello is the one tolerant method: members it does not know, at every
	// depth, are ignored, and the highest version in common is chosen.
	r := ok[protocol.HelloResult](t, c.call(protocol.MethodHello, map[string]any{
		"protocols": []int{1, 7}, "client": map[string]any{"kind": "t", "future": true, "capabilities": map[string]any{"later": 1}},
		"newThing": map[string]any{"x": 1}, "auth": nil,
	}))
	if r.Protocol != 1 || r.ClientID == "" {
		t.Fatalf("a tolerant hello: %+v", r)
	}
	// A connection is bound once.
	refusedWith(t, c.call(protocol.MethodHello, helloParams(nil)), protocol.RPCRefused, protocol.CodeBadRequest, protocol.ReasonBadRequest)

	// Params are strict: an unknown member, at any depth, named.
	for field, params := range map[string]any{
		"params.bogus": map[string]any{"sessionId": sid(h), "bogus": 1},
		"params.setting.extra": map[string]any{"sessionId": sid(h), "commandId": "1",
			"setting": map[string]any{"kind": "mode", "value": "plan", "extra": true}},
		"params.SessionID": map[string]any{"SessionID": sid(h)},
	} {
		method := protocol.MethodSessionState
		if strings.Contains(field, "setting") {
			method = protocol.MethodSessionSet
		}
		e := refusedWith(t, c.call(method, params), protocol.RPCInvalidParams, protocol.CodeBadRequest, protocol.ReasonUnknownField)
		if !strings.Contains(e.Message, field) {
			t.Fatalf("the refusal does not name %s: %q", field, e.Message)
		}
	}
	// Malformed params: a missing member, a null, a wrong type, a bad enum, a
	// params that is not an object, and interject with a row.
	for name, call := range map[string]struct {
		method string
		params any
	}{
		"missing text":     {protocol.MethodQueueAdd, map[string]any{"sessionId": sid(h), "commandId": "1"}},
		"missing setting":  {protocol.MethodSessionSet, map[string]any{"sessionId": sid(h), "commandId": "1"}},
		"null setting":     {protocol.MethodSessionSet, map[string]any{"sessionId": sid(h), "commandId": "1", "setting": nil}},
		"null text":        {protocol.MethodQueueAdd, map[string]any{"sessionId": sid(h), "commandId": "1", "text": nil}},
		"number commandId": {protocol.MethodQueueAdd, map[string]any{"sessionId": sid(h), "commandId": 1, "text": "x"}},
		"bad mode": {protocol.MethodSessionPrompt, map[string]any{"sessionId": sid(h), "commandId": "1", "text": "x",
			"mode": "shout"}},
		"bad kind": {protocol.MethodSessionSet, map[string]any{"sessionId": sid(h), "commandId": "1",
			"setting": map[string]any{"kind": "colour", "value": "red"}}},
		"interject a row": {protocol.MethodSessionPrompt, map[string]any{"sessionId": sid(h), "commandId": "1",
			"fromRow": "q-1", "mode": "interject"}},
		"params an array":   {protocol.MethodSessionState, []int{1}},
		"missing sessionId": {protocol.MethodSessionState, map[string]any{}},
	} {
		if resp := c.call(call.method, call.params); resp.Error == nil || resp.Error.Code != protocol.RPCInvalidParams ||
			resp.Error.Data.Reason != protocol.ReasonBadRequest {
			t.Fatalf("%s: want -32602 bad_request, got %+v", name, resp.Error)
		}
	}
	// A commandId that is not a canonical positive decimal.
	for _, id := range []string{"0", "01", "-1", "+1", "1.0", "abc", "", " 1", "18446744073709551616"} {
		refusedWith(t, queueAdd(c, id, "x"), protocol.RPCInvalidParams, protocol.CodeBadRequest, protocol.ReasonBadRequest)
	}
	// A session this host does not serve.
	refusedWith(t, c.call(protocol.MethodSessionState, protocol.StateParams{SessionID: "not-this-one"}),
		protocol.RPCRefused, protocol.CodeUnknownSession, protocol.ReasonUnknownSession)
	refusedWith(t, c.call(protocol.MethodQueueAdd, protocol.QueueAddParams{SessionID: "not-this-one", CommandID: "1", Text: "x"}),
		protocol.RPCRefused, protocol.CodeUnknownSession, protocol.ReasonUnknownSession)

	// What a session host does not serve, whatever the params.
	for method, reason := range map[string]protocol.Reason{
		protocol.MethodSessionStop:       protocol.ReasonStopUnsupported,
		protocol.MethodSessionsSubscribe: protocol.ReasonRosterUnsupported,
		protocol.MethodSessionConnect:    protocol.ReasonHubOnly,
		protocol.MethodSessionCreate:     protocol.ReasonHubOnly,
	} {
		refusedWith(t, c.call(method, map[string]any{"anything": true}), protocol.RPCRefused, protocol.CodeUnsupported, reason)
	}
	refusedWith(t, c.call("session.frobnicate", map[string]any{}), protocol.RPCMethodNotFound, protocol.CodeUnsupported, protocol.ReasonUnknownMethod)

	// Lines that are not requests: answered with the id when it could be read
	// and null otherwise, and the connection goes on after each.
	for _, tc := range []struct {
		name, id, line string
		rpc            int
	}{
		{"not JSON", "", `{not json`, protocol.RPCParseError},
		// A line that starts like a batch and is not JSON is not JSON: the
		// batch check sees only the first byte, and validity comes first.
		{"a batch that is not JSON", "", `[garbage`, protocol.RPCParseError},
		{"an unclosed array", "", `[`, protocol.RPCParseError},
		{"a batch", "", `[{"jsonrpc":"2.0","id":1,"method":"session.state","params":{}}]`, protocol.RPCInvalidRequest},
		{"a valid array of anything", "", `[1, "two"]`, protocol.RPCInvalidRequest},
		{"not an object", "", `42`, protocol.RPCInvalidRequest},
		{"no id", "", `{"jsonrpc":"2.0","method":"session.state","params":{}}`, protocol.RPCInvalidRequest},
		{"an object id", "", `{"jsonrpc":"2.0","id":{"x":1},"method":"session.state"}`, protocol.RPCInvalidRequest},
		{"an unknown member", `"e1"`, `{"jsonrpc":"2.0","id":"e1","method":"session.state","params":{},"extra":1}`, protocol.RPCInvalidRequest},
		{"not 2.0", `"e2"`, `{"jsonrpc":"1.0","id":"e2","method":"session.state","params":{}}`, protocol.RPCInvalidRequest},
		{"no method", `7`, `{"jsonrpc":"2.0","id":7}`, protocol.RPCInvalidRequest},
	} {
		c.sendRaw(tc.id, "", tc.line)
		resp := c.read()
		want := tc.id
		if want == "" {
			want = "null"
		}
		if string(resp.ID) != want || resp.Error == nil || resp.Error.Code != tc.rpc ||
			resp.Error.Data.Code != protocol.CodeBadRequest || resp.Error.Data.Reason != protocol.ReasonBadRequest {
			t.Fatalf("%s: id %s, error %+v; want id %s and %d bad_request", tc.name, resp.ID, resp.Error, want, tc.rpc)
		}
	}
	// A blank line asks nothing and gets nothing: the next reply is the next
	// request's.
	c.sendRaw("", "", "   ")
	ok[protocol.StateResult](t, c.call(protocol.MethodSessionState, state))

	// A line over the inbound limit is discarded to its newline and answered
	// -32600 line_too_long with id null; the connection is still bound.
	long := `{"jsonrpc":"2.0","id":"L","method":"session.state","params":{"sessionId":"` +
		strings.Repeat("x", protocol.InboundLineMax) + `"}}`
	c.sendRaw("", "", long)
	resp := c.read()
	if string(resp.ID) != "null" {
		t.Fatalf("an over-long line answered to %s", resp.ID)
	}
	refusedWith(t, resp, protocol.RPCInvalidRequest, protocol.CodeBadRequest, protocol.ReasonLineTooLong)
	ok[protocol.StateResult](t, c.call(protocol.MethodSessionState, state))
}
