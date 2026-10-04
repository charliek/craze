package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// sessions.createOptions (plan 036 §3.4, A3, A4): the hub serves the CLI's
// answer (Creates.Options) as it stands, computed at each call with the hub's
// own log, and says so in its hello; a hub without an answer omits the
// capability and refuses the method unsupported; Find finds a hub and never
// starts one.

// cannedOptions is a deterministic answer, §3.4's example.
var cannedOptions = protocol.CreateOptionsResult{
	Providers: []protocol.ProviderOption{
		{ID: "cursor", Label: "cursor", State: protocol.ProviderUnavailable,
			Reason: "this craze runs outside the macOS login session (over ssh), where cursor may not reach the login keychain",
			Fix:    `kill 5150 (no session ends), then run "craze ps" in a terminal on the Mac`},
		{ID: "grok", Label: "grok", State: protocol.ProviderReady},
		{ID: "native", Label: "native", State: protocol.ProviderNeedsSetup, Reason: "no model provider has a key", Fix: "craze auth login"},
	},
	DefaultProvider: "grok",
	RecentDirs:      []protocol.RecentDir{{Dir: "/home/me/projects/lumen", UsedAt: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}},
}

// optionsCreates is session.create's options with answer as
// sessions.createOptions'.
func optionsCreates(answer func(logf func(string, ...any)) protocol.CreateOptionsResult) *Creates {
	c := creates("cursor")
	c.Options = answer
	return c
}

// encoded is v as the hub writes it.
func encoded(t *testing.T, v any) string {
	t.Helper()
	b, err := protocol.MarshalLine(v)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// TestCreateOptionsServesTheAnswer (A4): a hub given an answer says
// createOptions in its hello — the hub's capabilities otherwise — and
// answers sessions.createOptions with exactly what the answer returned,
// params absent or {}; the answer runs at every call (nothing is kept) and
// what it says through logf reaches the hub's log; a member in the params is
// unknown_field, naming it, and a list the answer left nil is written [].
func TestCreateOptionsServesTheAnswer(t *testing.T) {
	var calls atomic.Int32
	answer := func(logf func(string, ...any)) protocol.CreateOptionsResult {
		n := calls.Add(1)
		logf("createOptions answer %d", n)
		if n == 3 {
			return protocol.CreateOptionsResult{}
		}
		res := cannedOptions
		if n == 2 {
			res.DefaultProvider = "native"
		}
		return res
	}
	rn := runWith(t, testEnv(t), quiet(), optionsCreates(answer))
	sock := rn.line(t).Socket
	c := dial(t, sock)
	hello := c.hello(t)
	want := protocol.HubCapabilities()
	want.CreateOptions = true
	if hello.Capabilities != want {
		t.Fatalf("hello's capabilities %+v, want %+v", hello.Capabilities, want)
	}
	resp := c.call(t, protocol.MethodSessionsCreateOptions, nil)
	if resp.Error != nil || string(resp.Result) != encoded(t, cannedOptions) {
		t.Fatalf("sessions.createOptions = %s (%v)\nwant %s", resp.Result, resp.Error, encoded(t, cannedOptions))
	}
	again := cannedOptions
	again.DefaultProvider = "native"
	if resp := c.call(t, protocol.MethodSessionsCreateOptions, map[string]any{}); resp.Error != nil || string(resp.Result) != encoded(t, again) {
		t.Fatalf("a second sessions.createOptions = %s (%v), want the answer computed again: %s", resp.Result, resp.Error, encoded(t, again))
	}
	if resp := c.call(t, protocol.MethodSessionsCreateOptions, nil); resp.Error != nil || string(resp.Result) != `{"providers":[],"recentDirs":[]}` {
		t.Fatalf("an answer with nil lists = %s (%v), want both written []", resp.Result, resp.Error)
	}
	resp = c.call(t, protocol.MethodSessionsCreateOptions, map[string]any{"provider": "grok"})
	if resp.Error == nil || resp.Error.Code != protocol.RPCInvalidParams || resp.Error.Data.Code != protocol.CodeBadRequest ||
		resp.Error.Data.Reason != protocol.ReasonUnknownField || !strings.Contains(resp.Error.Message, "params.provider") {
		t.Fatalf("sessions.createOptions with a stray member: %+v, want -32602 unknown_field naming params.provider", resp.Error)
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("the answer ran %d times, want 3: once a served call, never for a refused one", n)
	}
	for i := 1; i <= 3; i++ {
		if line := fmt.Sprintf("craze hub: createOptions answer %d\n", i); !strings.Contains(rn.stderr.String(), line) {
			t.Fatalf("the hub's log lacks %q; it said:\n%s", line, rn.stderr)
		}
	}
}

// TestAHubWithoutAnAnswerRefusesCreateOptions (A3): a hub given Creates but
// no Options, and one given no Creates at all, omit createOptions from their
// hello — the member absent, not false — and answer sessions.createOptions
// unsupported, reason unsupported, whatever its params: the dispatch's
// fall-through.
func TestAHubWithoutAnAnswerRefusesCreateOptions(t *testing.T) {
	for name, creates := range map[string]*Creates{"no Options": creates("cursor"), "no Creates": nil} {
		t.Run(name, func(t *testing.T) {
			rn := runWith(t, testEnv(t), quiet(), creates)
			c := dial(t, rn.line(t).Socket)
			resp := c.call(t, protocol.MethodHello, protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{Kind: "test"}})
			if resp.Error != nil || strings.Contains(string(resp.Result), "createOptions") {
				t.Fatalf("hello = %s (%v), want no createOptions member", resp.Result, resp.Error)
			}
			for _, params := range []any{nil, map[string]any{}, map[string]any{"provider": "grok"}} {
				resp := c.call(t, protocol.MethodSessionsCreateOptions, params)
				if resp.Error == nil || resp.Error.Code != protocol.RPCRefused || resp.Error.Data.Code != protocol.CodeUnsupported ||
					resp.Error.Data.Reason != protocol.ReasonUnsupported {
					t.Fatalf("sessions.createOptions (params %v) = %s %+v, want unsupported/unsupported", params, resp.Result, resp.Error)
				}
			}
		})
	}
}

// helloBeforeCreateOptions is a hub's hello result as a client built before
// plan 036 decodes it: the six connection capabilities, nothing of
// createOptions.
type helloBeforeCreateOptions struct {
	Protocol     int               `json:"protocol"`
	Endpoint     protocol.Endpoint `json:"endpoint"`
	Capabilities struct {
		RosterSubscribe bool `json:"rosterSubscribe"`
		SessionCreate   bool `json:"sessionCreate"`
		Multiplex       bool `json:"multiplex"`
		Connect         bool `json:"connect"`
		Snapshot        bool `json:"snapshot"`
		AttachWhenNow   bool `json:"attachWhenNow"`
	} `json:"capabilities"`
	Codecs protocol.Codecs `json:"codecs"`
	Limits protocol.Limits `json:"limits"`
}

// TestAnOlderClientReadsANewHello (plan 036 §3.4's compatibility): the hello
// of a hub that serves sessions.createOptions carries a member a client
// built before it does not know, and such a client — which ignores a result
// member it does not know (protocol.md) — reads the rest as it always has.
func TestAnOlderClientReadsANewHello(t *testing.T) {
	rn := runWith(t, testEnv(t), quiet(), optionsCreates(func(func(string, ...any)) protocol.CreateOptionsResult { return cannedOptions }))
	c := dial(t, rn.line(t).Socket)
	resp := c.call(t, protocol.MethodHello, protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{Kind: "old"}})
	if resp.Error != nil || !strings.Contains(string(resp.Result), `"createOptions":true`) {
		t.Fatalf("hello = %s (%v), want createOptions true in it", resp.Result, resp.Error)
	}
	var old helloBeforeCreateOptions
	if err := json.Unmarshal(resp.Result, &old); err != nil {
		t.Fatalf("an older client's decode of %s: %v", resp.Result, err)
	}
	caps := old.Capabilities
	if old.Protocol != 1 || old.Endpoint.Kind != protocol.EndpointHub || !caps.RosterSubscribe || !caps.SessionCreate ||
		caps.Multiplex || !caps.Connect || caps.Snapshot || caps.AttachWhenNow || old.Limits != protocol.HostLimits() {
		t.Fatalf("an older client reads %+v", old)
	}
}

// TestCreateOptionsClient: CreateOptions answers the hub's result as the hub
// wrote it; against a hub whose hello does not say createOptions it is a
// *LacksError in the plan's words, and nothing is asked.
func TestCreateOptionsClient(t *testing.T) {
	who := protocol.ClientInfo{Kind: "test"}
	ctx, cancel := context.WithTimeout(context.Background(), step)
	defer cancel()
	rn := runWith(t, testEnv(t), quiet(), optionsCreates(func(func(string, ...any)) protocol.CreateOptionsResult { return cannedOptions }))
	raw, err := CreateOptions(ctx, rn.line(t).Socket, who)
	if err != nil || string(raw) != encoded(t, cannedOptions) {
		t.Fatalf("CreateOptions = %s, %v\nwant %s", raw, err, encoded(t, cannedOptions))
	}

	older := runWith(t, testEnv(t), quiet(), creates("cursor"))
	_, err = CreateOptions(ctx, older.line(t).Socket, who)
	var lacks *LacksError
	if !errors.As(err, &lacks) || err.Error() != "this hub (craze "+lacks.Version+") cannot list what it can create; it exits when idle" {
		t.Fatalf("CreateOptions of a hub that cannot answer it = %v", err)
	}
}

// TestFindNeverStartsAHub (plan 036 §3.2): Find answers the hub its record
// names when it answers with what the caller needs, a *LacksError when it
// answers without it, and ErrNoHub when there is no record or its hub does
// not answer — and never spawns one (noCommand fails the test if it does).
func TestFindNeverStartsAHub(t *testing.T) {
	find := func(t *testing.T, env rundir.Env, need protocol.ConnectionCapabilities) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), step)
		defer cancel()
		return Find(ctx, env, need)
	}
	needOptions := protocol.ConnectionCapabilities{CreateOptions: true}

	t.Run("no record", func(t *testing.T) {
		noCommand(t)
		if sock, err := find(t, testEnv(t), needOptions); !errors.Is(err, ErrNoHub) || sock != "" {
			t.Fatalf("Find with no hub = %q, %v; want ErrNoHub", sock, err)
		}
	})
	t.Run("a hub that does not answer", func(t *testing.T) {
		noCommand(t)
		env := testEnv(t)
		pid := sleeper(t)
		hubID := rundir.NewHubID()
		d := newDecoy(t, decoyEOF, protocol.HubHelloResult{})
		writeRecord(t, env, rundir.HubRecord{HubID: hubID, PID: pid, StartToken: token(t, pid), Socket: d.path})
		if sock, err := find(t, env, needOptions); !errors.Is(err, ErrNoHub) || sock != "" {
			t.Fatalf("Find with a hub that closes its hello = %q, %v; want ErrNoHub", sock, err)
		}
		if !alive(pid) {
			t.Fatal("Find signalled the hub")
		}
	})
	t.Run("an older hub", func(t *testing.T) {
		noCommand(t)
		env := testEnv(t)
		pid := sleeper(t)
		hubID := rundir.NewHubID()
		d := newDecoy(t, decoyAnswer, hubResult(hubID, "0.9.1", protocol.HubCapabilities()))
		writeRecord(t, env, rundir.HubRecord{HubID: hubID, PID: pid, StartToken: token(t, pid), Socket: d.path})
		_, err := find(t, env, needOptions)
		var lacks *LacksError
		if !errors.As(err, &lacks) || err.Error() != "this hub (craze 0.9.1) cannot list what it can create; it exits when idle" {
			t.Fatalf("Find of an older hub = %v; want it reported", err)
		}
		if sock, err := find(t, env, protocol.ConnectionCapabilities{}); err != nil || sock != d.path {
			t.Fatalf("Find needing nothing = %q, %v; want the older hub, %s", sock, err, d.path)
		}
	})
	t.Run("a hub that answers", func(t *testing.T) {
		noCommand(t)
		env := testEnv(t)
		rn := runWith(t, env, quiet(), optionsCreates(func(func(string, ...any)) protocol.CreateOptionsResult { return cannedOptions }))
		line := rn.line(t)
		if sock, err := find(t, env, needOptions); err != nil || sock != line.Socket {
			t.Fatalf("Find = %q, %v; want the live hub's %s", sock, err, line.Socket)
		}
	})
}
