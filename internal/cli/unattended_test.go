package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/sessions"
)

// The unattended host (plan 041 §3.2, P2, P10–P11; unattended.go): craze
// serve --unattended builds its session with Interactive false and craze
// prompt's approval policy, writes its start settings on the session's index
// row, and every other host — craze serve without it, the TUI — is as it was.

// servedOptions is the agent.Options craze serve builds with the session
// flags argv, over the fake agent's echo script, once its host serves.
func servedOptions(t *testing.T, argv ...string) agent.Options {
	t.Helper()
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	built := recordBuilt(t)
	r := runServeIn(t, hostEnv{}, append([]string{"--agent-bin", fakeAgentPath(t), "--workspace", ws}, argv...)...)
	o := builtOptions(t, built)
	r.waitServing(t, env, true)
	return o
}

// TestServeUnattendedIsCrazePromptsPolicy (P2, the hunt's "Interactive: false
// must not leak"): --unattended is Interactive false and craze prompt's own
// approval policy — permissions allowed under force, parked under --no-force,
// questions answered with their first option — but for an ACP provider (here
// cursor) a plan returned, not accepted (X28); and craze serve without it
// builds the interactive session it always has, with no policy of its own
// (EffectiveApproval's derived one). The rest of the policy is
// headlessApproval's, the one craze prompt runs (TestPromptsPolicyIsHeadless).
func TestServeUnattendedIsCrazePromptsPolicy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		argv        []string
		interactive bool
		approval    *agent.ApprovalPolicy
	}{
		{"attended", nil, true, nil},
		{"unattended", []string{"--unattended"}, false,
			&agent.ApprovalPolicy{Permission: agent.ApprovalAllow, Question: agent.ApprovalFirstOption, Plan: agent.ApprovalReturn}},
		{"unattended, no force", []string{"--unattended", "--no-force"}, false,
			&agent.ApprovalPolicy{Permission: agent.ApprovalAsk, Question: agent.ApprovalFirstOption, Plan: agent.ApprovalReturn}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := servedOptions(t, tc.argv...)
			if o.Interactive != tc.interactive || (o.Approval == nil) != (tc.approval == nil) ||
				tc.approval != nil && *o.Approval != *tc.approval {
				t.Fatalf("interactive %v, approval %+v; want %v, %+v", o.Interactive, o.Approval, tc.interactive, tc.approval)
			}
		})
	}
}

// TestPromptsPolicyIsHeadless: craze prompt's session runs headlessApproval's
// policy — the one unattendedPolicy hands an unattended host — so the two
// cannot drift apart: whole for a native session, and for an ACP one (cursor,
// grok, gx; nil is cursor) all but the plan, which an unattended host returns
// (X28) where craze prompt accepts it (unchanged: v2 parity).
func TestPromptsPolicyIsHeadless(t *testing.T) {
	native, cursor := agent.NativeProvider(), agent.CursorProvider()
	grok, gx := agent.GrokProvider(), agent.GxProvider()
	for _, force := range []bool{true, false} {
		o := (&promptOpts{force: force}).agentOptions("/w", headlessApproval(force), nil)
		if o.Approval == nil || o.Approval.Plan != agent.ApprovalAccept {
			t.Fatalf("craze prompt's plan policy changed: %+v", o.Approval)
		}
		for _, p := range []*agent.Provider{nil, &cursor, &grok, &gx, &native} {
			u := agent.Options{Force: force, Provider: p}
			unattendedPolicy(&u)
			want := *o.Approval
			if p == nil || !p.InProcess() {
				want.Plan = agent.ApprovalReturn
			}
			if u.Approval == nil || *u.Approval != want || u.Interactive {
				t.Fatalf("force %v, provider %v: unattended %+v (interactive %v), want %+v", force, p, u.Approval, u.Interactive, want)
			}
		}
	}
}

// TestAnUnattendedHostRecordsItsStartSettings is §7 W2 15's first clause in
// the host: its index row carries `unattended` with the start's mode,
// permission mode, model and effort — and an attended host's row carries
// none.
func TestAnUnattendedHostRecordsItsStartSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want *sessions.Unattended
	}{
		{"unattended", []string{"--unattended", "--plan", "--no-force", "--model", "m-1", "--effort", "low"},
			&sessions.Unattended{Mode: "plan", PermissionMode: sessions.PermissionPrompt, Model: "m-1", Effort: "low"}},
		{"unattended, defaults", []string{"--unattended"}, &sessions.Unattended{PermissionMode: sessions.PermissionBypass}},
		{"attended", []string{"--plan"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			r := runServeIn(t, hostEnv{}, append([]string{"--agent-bin", fakeAgentPath(t), "--workspace", ws, "--provider", "cursor"}, tc.argv...)...)
			e := r.waitServing(t, env, true)
			promptUnattached(t, e, "a task")
			waitLastTurn(t, e)
			got := waitIndexRowByID(t, e.CrazeSessionID).Unattended
			if (got == nil) != (tc.want == nil) || tc.want != nil && *got != *tc.want {
				t.Fatalf("the row's record is %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestAResumeOfAMarkedSessionIsInteractive (P10, the hunt's provenance item):
// a person's load of a session the index marks unattended — craze serve
// --load with no --unattended, as craze -r and the session list spawn it — is
// an ordinary interactive host, and the row stays marked.
func TestAResumeOfAMarkedSessionIsInteractive(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	rec := sessions.Unattended{Mode: "plan", PermissionMode: sessions.PermissionPrompt, Model: "m-1"}
	crazeID := "0199c0de-4444-7000-8000-00000000d004"
	seedIndexRow(t, sessions.Row{SessionID: "fake-session-1", Provider: "cursor", CWD: absDir(ws), CrazeID: crazeID,
		Title: "a sub-agent's", Unattended: &rec})
	seeded := indexRowByID(t, crazeID).UpdatedAt
	built := recordBuilt(t)
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--load", crazeID)
	o := builtOptions(t, built)
	e := r.waitServing(t, env, true)
	if !o.Interactive || o.Approval != nil {
		t.Fatalf("a person's resume built interactive %v, approval %+v", o.Interactive, o.Approval)
	}
	promptUnattached(t, e, "mine now")
	waitLastTurn(t, e)
	// The host's own writes of the row — the load's touch, the turn's — come
	// off its index worker: the row is read once one has landed.
	waitFor(t, "the resumed host's write of the row", func() bool { return indexRowByID(t, crazeID).UpdatedAt.After(seeded) })
	if got := indexRowByID(t, crazeID).Unattended; got == nil || *got != rec {
		t.Fatalf("the resumed row's record is %+v, want %+v", got, rec)
	}
}

// TestPromptTextMaxIsTheQueuesBound: the most a prompt craze new --wait or
// craze send takes is exactly what the host's message queue admits.
func TestPromptTextMaxIsTheQueuesBound(t *testing.T) {
	var q agent.PromptQueue
	if _, _, err := q.Add(strings.Repeat("x", promptTextMax), time.Now()); err != nil {
		t.Fatalf("the queue refused promptTextMax bytes: %v", err)
	}
	if _, _, err := q.Add(strings.Repeat("x", promptTextMax+1), time.Now()); !errors.Is(err, agent.ErrQueueTextTooLong) {
		t.Fatalf("the queue took one byte past promptTextMax: %v", err)
	}
}

// ----------------------------------------------- §7 W2 14: native, foreground

// routedModel answers each Stream with the next step queued for the request's
// first user prompt: the prompt of the turn — the parent's, or a child's —
// that sent it, so a child's steps and its parent's never take each other's
// place whatever order they run in.
type routedModel struct {
	mu     sync.Mutex
	queues map[string][][]fantasy.StreamPart
}

func (m *routedModel) route(prompt string, steps ...[]fantasy.StreamPart) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queues[prompt] = append(m.queues[prompt], steps...)
}

func (m *routedModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	key := ""
	for _, msg := range call.Prompt {
		if msg.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, p := range msg.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
				key = tp.Text
				break
			}
		}
		break
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[key]
	if len(q) == 0 {
		return nil, fmt.Errorf("routedModel: no step queued for %q", key)
	}
	step := q[0]
	m.queues[key] = q[1:]
	return func(yield func(fantasy.StreamPart) bool) {
		for _, p := range step {
			if !yield(p) {
				return
			}
		}
	}, nil
}

func (m *routedModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("routedModel: Generate is not used")
}

func (m *routedModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("routedModel: GenerateObject is not used")
}

func (m *routedModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("routedModel: StreamObject is not used")
}

func (m *routedModel) Provider() string { return "test" }
func (m *routedModel) Model() string    { return "wire-a" }

// TestAnUnattendedNativeSessionRunsAgentCallsInTheForeground is §7 W2 14: a
// native session built with the options an unattended host builds (craze
// serve --unattended's, as serveBuilt sees them) runs an agent call that
// asks for the background in the foreground — the child finishes inside the
// call, nothing is marked background — and no wake turn follows its turn:
// the turn is whole when it ends (P2). A scripted model stands in for the
// provider (agent.NewNative's tweak, as native_json_test.go's).
func TestAnUnattendedNativeSessionRunsAgentCallsInTheForeground(t *testing.T) {
	host := servedOptions(t, "--unattended")
	ws := t.TempDir()
	model := &routedModel{queues: map[string][][]fantasy.StreamPart{}}
	args, err := json.Marshal(map[string]any{"description": "job", "prompt": "child work", "run_in_background": true})
	if err != nil {
		t.Fatal(err)
	}
	model.route("go", nativeJSONCall("a1", tool.AgentTool, string(args)), nativeJSONAnswer("parent", "done"))
	model.route("child work", nativeJSONAnswer("child", "did it"))
	table := &modeltable.Table{
		DefaultModel: "test/a",
		Providers: map[string]modeltable.Provider{
			"test": {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"UNATTENDED_TEST_KEY"}},
		},
		Models: map[string]modeltable.Model{"test/a": {Provider: "test", WireModel: "wire-a", Name: "A"}},
	}
	opts := agent.Options{Workspace: ws, ContentHome: t.TempDir(), Force: host.Force,
		Interactive: host.Interactive, Approval: host.Approval}
	sess := agent.NewNative(opts, func(o *harness.Options) {
		o.Home = t.TempDir()
		o.Table = table
		o.Getenv = func(k string) string {
			if k == "UNATTENDED_TEST_KEY" {
				return "test-key"
			}
			return ""
		}
		o.NewModel = func(modeltable.Resolved) (fantasy.LanguageModel, error) { return model, nil }
	})
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if _, err := sess.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	evs := drainNativeEvents(sess)
	finished, callDone := -1, -1
	for i, ev := range evs {
		switch {
		case ev.Type == agent.EventSubagent && ev.SubagentChange == agent.SubagentChangeFinished && finished < 0:
			finished = i
			if ev.Subagent.Background {
				t.Fatalf("the child was marked background: %+v", ev.Subagent)
			}
		case ev.Type == agent.EventTool && ev.Agent == "" && ev.Tool != nil && ev.Tool.Task != nil && ev.Tool.Status == "completed":
			// The parent's row for the call: the one carrying its task.
			callDone = i
			if ev.Tool.Task == nil || ev.Tool.Task.Background {
				t.Fatalf("the call's row was marked background: %+v", ev.Tool.Task)
			}
		case ev.Type == agent.EventForeignTurn:
			t.Fatalf("a wake turn ran in an unattended session: %+v", ev)
		}
	}
	if finished < 0 || callDone < 0 || finished > callDone {
		t.Fatalf("the child finished at %d and the call ended at %d; want the child inside the call", finished, callDone)
	}
}
