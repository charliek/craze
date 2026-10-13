package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
)

// sessions.saved (plan 046 B2, B-3): the hub holds the params to the
// method's form and serves the CLI's answer (Creates.Saved) as it stands,
// computed at each call with the hub's own log, or the refusal it returns; a
// hub without an answer refuses the method unsupported; nothing in its hello
// says it serves the method in this release.

// cannedSaved is a deterministic answer: a craze id's session and a legacy
// row's.
var cannedSaved = protocol.SavedResult{Sessions: []protocol.SavedSession{
	{ID: "0190ab12-cd34-7ef0-8123-456789abcdef", CrazeSessionID: "0190ab12-cd34-7ef0-8123-456789abcdef", Provider: "cursor",
		ProviderSessionID: "prov-1", Workspace: "/home/me/projects/lumen", Title: "fix the flaky test",
		CreatedAt: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)},
	{ID: "grok:prov-2", Provider: "grok", ProviderSessionID: "prov-2", Workspace: "/home/me/projects/shed",
		CreatedAt: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)},
}, More: true}

// savedAnswerer is a Creates.Saved that records every query it is asked and
// answers each with answer(n, q), n counted from 1.
type savedAnswerer struct {
	mu     sync.Mutex
	asked  []protocol.SavedParams
	answer func(n int, q protocol.SavedParams, logf func(string, ...any)) (protocol.SavedResult, *protocol.Error)
}

func (a *savedAnswerer) saved(q protocol.SavedParams, logf func(string, ...any)) (protocol.SavedResult, *protocol.Error) {
	a.mu.Lock()
	a.asked = append(a.asked, q)
	n := len(a.asked)
	a.mu.Unlock()
	return a.answer(n, q, logf)
}

func (a *savedAnswerer) queries() []protocol.SavedParams {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]protocol.SavedParams(nil), a.asked...)
}

// savedCreates is session.create's options with a's answer as
// sessions.saved's.
func savedCreates(a *savedAnswerer) *Creates {
	c := creates("cursor")
	c.Saved = a.saved
	return c
}

// TestSavedServesTheAnswer: a hub given an answer serves it as it stands —
// written whole, a nil list as [] — to params absent, {} or naming every
// member, the answer handed them in the method's form (the default limit
// filled in, the workspace cleaned) and the hub's log; a refusal the answer
// returns is the call's; the hello says nothing new (no capability in this
// release).
func TestSavedServesTheAnswer(t *testing.T) {
	a := &savedAnswerer{answer: func(n int, _ protocol.SavedParams, logf func(string, ...any)) (protocol.SavedResult, *protocol.Error) {
		logf("saved answer %d", n)
		switch n {
		case 3:
			return protocol.SavedResult{}, nil
		case 4:
			return protocol.SavedResult{}, &protocol.Error{Code: protocol.RPCRefused, Message: "the registry cannot be read",
				Data: protocol.ErrorData{Code: protocol.CodeUnavailable, Reason: protocol.ReasonHostUnreachable}}
		}
		return cannedSaved, nil
	}}
	rn := runWith(t, testEnv(t), quiet(), savedCreates(a))
	c := dial(t, rn.line(t).Socket)
	if hello := c.hello(t); hello.Capabilities != protocol.HubCapabilities() {
		t.Fatalf("hello's capabilities %+v, want the hub's: sessions.saved adds none", hello.Capabilities)
	}
	if resp := c.call(t, protocol.MethodSessionsSaved, nil); resp.Error != nil || string(resp.Result) != encoded(t, cannedSaved) {
		t.Fatalf("sessions.saved = %s (%v)\nwant %s", resp.Result, resp.Error, encoded(t, cannedSaved))
	}
	every := map[string]any{"workspace": "/home/me/projects/../projects/lumen/", "limit": 200, "subAgents": true}
	if resp := c.call(t, protocol.MethodSessionsSaved, every); resp.Error != nil || string(resp.Result) != encoded(t, cannedSaved) {
		t.Fatalf("sessions.saved naming every member = %s (%v)", resp.Result, resp.Error)
	}
	if resp := c.call(t, protocol.MethodSessionsSaved, map[string]any{}); resp.Error != nil ||
		string(resp.Result) != `{"sessions":[],"more":false}` {
		t.Fatalf("an answer with a nil list = %s (%v), want it written []", resp.Result, resp.Error)
	}
	resp := c.call(t, protocol.MethodSessionsSaved, map[string]any{"limit": 1})
	if resp.Error == nil || resp.Error.Code != protocol.RPCRefused || resp.Error.Data.Code != protocol.CodeUnavailable ||
		resp.Error.Data.Reason != protocol.ReasonHostUnreachable {
		t.Fatalf("the answer's refusal: %s %+v, want it as the call's answer", resp.Result, resp.Error)
	}
	want := []protocol.SavedParams{
		{Limit: protocol.SavedLimitDefault},
		{Workspace: "/home/me/projects/lumen", Limit: 200, SubAgents: true},
		{Limit: protocol.SavedLimitDefault},
		{Limit: 1},
	}
	if got := a.queries(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the answer was asked %+v, want %+v", got, want)
	}
	for i := 1; i <= 4; i++ {
		if line := fmt.Sprintf("craze hub: saved answer %d\n", i); !strings.Contains(rn.stderr.String(), line) {
			t.Fatalf("the hub's log lacks %q; it said:\n%s", line, rn.stderr)
		}
	}
}

// TestSavedRefusesBadParams (the wire negatives): an unknown member is
// unknown_field, naming the first in order; null params or members, a member
// of another type, an empty, relative or over-long workspace, a limit that is
// not an integer from 1 to 200, and a member given twice are bad_request —
// each -32602, and the answer never asked. Limits 1 and 200 are served (the
// negative control).
func TestSavedRefusesBadParams(t *testing.T) {
	a := &savedAnswerer{answer: func(int, protocol.SavedParams, func(string, ...any)) (protocol.SavedResult, *protocol.Error) {
		return cannedSaved, nil
	}}
	rn := runWith(t, testEnv(t), quiet(), savedCreates(a))
	c := dial(t, rn.line(t).Socket)
	c.hello(t)
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	resp := c.call(t, protocol.MethodSessionsSaved, raw(`{"workspace":"/w","provider":"grok","cwd":"/x"}`))
	if resp.Error == nil || resp.Error.Code != protocol.RPCInvalidParams || resp.Error.Data.Code != protocol.CodeBadRequest ||
		resp.Error.Data.Reason != protocol.ReasonUnknownField || resp.Error.Message != "unknown field params.cwd" {
		t.Fatalf("unknown members: %+v, want -32602 unknown_field naming params.cwd", resp.Error)
	}
	for _, params := range []string{
		`null`, `[]`, `"workspace"`, `7`,
		`{"workspace":null}`, `{"workspace":7}`, `{"workspace":["/w"]}`, `{"workspace":""}`, `{"workspace":"w"}`,
		`{"workspace":"./w"}`, `{"workspace":"/` + strings.Repeat("d", protocol.SavedWorkspaceMax) + `"}`,
		`{"limit":null}`, `{"limit":0}`, `{"limit":201}`, `{"limit":-1}`, `{"limit":1.5}`, `{"limit":"5"}`, `{"limit":1e2}`,
		`{"limit":true}`, `{"subAgents":null}`, `{"subAgents":"yes"}`, `{"subAgents":1}`,
	} {
		resp := c.call(t, protocol.MethodSessionsSaved, raw(params))
		if resp.Error == nil || resp.Error.Code != protocol.RPCInvalidParams || resp.Error.Data.Code != protocol.CodeBadRequest ||
			resp.Error.Data.Reason != protocol.ReasonBadRequest {
			t.Errorf("params %s: %s %+v, want -32602 bad_request", params, resp.Result, resp.Error)
		}
	}
	// A member given twice, whatever its values — an invalid one first,
	// which a map's decoding would drop, or two good ones — and however its
	// name is spelled.
	for params, member := range map[string]string{
		`{"limit":null,"limit":50}`:                                 "limit",
		`{"subAgents":1,"subAgents":false}`:                         "subAgents",
		`{"workspace":"relative","workspace":"/w"}`:                 "workspace",
		`{"limit":5,"limit":5}`:                                     "limit",
		`{"workspace":"/w","subAgents":true,"\u0077orkspace":"/w"}`: "workspace",
	} {
		resp := c.call(t, protocol.MethodSessionsSaved, raw(params))
		if resp.Error == nil || resp.Error.Code != protocol.RPCInvalidParams || resp.Error.Data.Code != protocol.CodeBadRequest ||
			resp.Error.Data.Reason != protocol.ReasonBadRequest || resp.Error.Message != "params."+member+" is given more than once" {
			t.Errorf("params %s: %s %+v, want -32602 bad_request naming params.%s given twice", params, resp.Result, resp.Error, member)
		}
	}
	if n := len(a.queries()); n != 0 {
		t.Fatalf("the answer was asked %d times for refused params", n)
	}
	for _, params := range []string{`{"limit":1}`, `{"limit":200}`, `{"workspace":"/` + strings.Repeat("d", protocol.SavedWorkspaceMax-1) + `"}`} {
		if resp := c.call(t, protocol.MethodSessionsSaved, raw(params)); resp.Error != nil {
			t.Errorf("params %s: refused %+v", params, resp.Error)
		}
	}
}

// TestAHubWithoutAnAnswerRefusesSaved (B-3): a hub given Creates but no
// Saved, and one given no Creates at all, answer sessions.saved unsupported,
// reason unsupported, whatever its params: the dispatch's fall-through, what
// a client takes for a hub that cannot list the saved.
func TestAHubWithoutAnAnswerRefusesSaved(t *testing.T) {
	for name, creates := range map[string]*Creates{"no Saved": creates("cursor"), "no Creates": nil} {
		t.Run(name, func(t *testing.T) {
			rn := runWith(t, testEnv(t), quiet(), creates)
			c := dial(t, rn.line(t).Socket)
			c.hello(t)
			for _, params := range []any{nil, map[string]any{}, map[string]any{"limit": 0}} {
				resp := c.call(t, protocol.MethodSessionsSaved, params)
				if resp.Error == nil || resp.Error.Code != protocol.RPCRefused || resp.Error.Data.Code != protocol.CodeUnsupported ||
					resp.Error.Data.Reason != protocol.ReasonUnsupported {
					t.Fatalf("sessions.saved (params %v) = %s %+v, want unsupported/unsupported", params, resp.Result, resp.Error)
				}
			}
		})
	}
}

// TestSavedClient: Saved sends the params and answers the hub's result as
// the hub wrote it; a hub that cannot answer is an error wrapping its
// unsupported refusal.
func TestSavedClient(t *testing.T) {
	who := protocol.ClientInfo{Kind: "test"}
	ctx, cancel := context.WithTimeout(context.Background(), step)
	defer cancel()
	a := &savedAnswerer{answer: func(int, protocol.SavedParams, func(string, ...any)) (protocol.SavedResult, *protocol.Error) {
		return cannedSaved, nil
	}}
	rn := runWith(t, testEnv(t), quiet(), savedCreates(a))
	q := protocol.SavedParams{Workspace: "/w", Limit: 200}
	raw, err := Saved(ctx, rn.line(t).Socket, who, q)
	if err != nil || string(raw) != encoded(t, cannedSaved) {
		t.Fatalf("Saved = %s, %v\nwant %s", raw, err, encoded(t, cannedSaved))
	}
	if got := a.queries(); len(got) != 1 || got[0] != q {
		t.Fatalf("the hub was asked %+v, want %+v", got, q)
	}

	older := runWith(t, testEnv(t), quiet(), creates("cursor"))
	_, err = Saved(ctx, older.line(t).Socket, who, q)
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Data.Code != protocol.CodeUnsupported {
		t.Fatalf("Saved of a hub that cannot answer = %v, want its unsupported refusal", err)
	}
}
