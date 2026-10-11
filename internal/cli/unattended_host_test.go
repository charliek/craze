package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/rundir"
)

// The unattended host's own rules after the C3 review and the live checks:
// it never saves its provider (X17), and on an ACP provider it returns a plan
// rather than accept it, ending the turn there (X28, P15's fallback moved to
// the host).

// TestAnUnattendedHostNeverSavesItsProvider is X17: an unattended host's
// start leaves config.toml's provider as it was — a delegation is not a
// choice of the next plain craze's provider — where an ordinary host's start
// of the same provider saves it. Each host is stopped before the file is
// read: its stop joins its start, and the save with it (serveHost.stop).
func TestAnUnattendedHostNeverSavesItsProvider(t *testing.T) {
	for _, tc := range []struct {
		name  string
		argv  []string
		saves bool
	}{{"unattended", []string{"--unattended"}, false}, {"attended", nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			_, ws := serveHome(t)
			path := writeCrazeConfig(t, "host_idle_exit = \"30s\"\n")
			env := rundir.ProcessEnv()
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
			r := runServeIn(t, hostEnv{}, append([]string{"--agent-bin", fakeAgentPath(t), "--workspace", ws, "--provider", "cursor"}, tc.argv...)...)
			e := r.waitServing(t, env, true)
			stopOver(t, dialServe(t, e), "1")
			if err := r.result(t, serveStep); err != nil {
				t.Fatalf("craze serve: %v", err)
			}
			cfg, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if saved := regexp.MustCompile(`(?m)^provider = "cursor"$`).Match(cfg); saved != tc.saves {
				t.Fatalf("the provider saved: %v, want %v; config.toml:\n%s", saved, tc.saves, cfg)
			}
		})
	}
}

// planReturnHome is unattendedHome with the fake agent as cursor and as grok
// (each its own dialect: the cursor-shaped and grok-shaped plan scripts), and
// every call the agent reads dumped to the file it answers.
func planReturnHome(t *testing.T) (rundir.Env, string, *childCmds, string) {
	t.Helper()
	_, ws := serveHome(t)
	fake := fakeAgentPath(t)
	writeCrazeConfig(t, "host_idle_exit = \"30s\"\n\n[agents]\ncursor = \""+fake+"\"\ngrok = \""+fake+"\"\n")
	env := rundir.ProcessEnv()
	kids := spawnAsChild(t, nil)
	spawnBounds(t, serveStep, serveStep, serveStep)
	t.Cleanup(func() { stopEveryHost(t, env) })
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("CRAZE_FAKE_DUMP_CALLS", calls)
	return env, ws, kids, calls
}

// TestAnUnattendedACPHostReturnsItsPlan is X28 (P15's fallback, on the host):
// an unattended host on an ACP provider never accepts a plan. The fake agent's
// plan scripts — cursor's cursor/create_plan, grok's x.ai/exit_plan_mode —
// each carry on with `planned:<outcome>` (their stand-in for the work an
// accepted plan starts) unless their request is answered cancelled. Here the
// request is answered cancelled and the turn cancelled (session/cancel): no
// `planned:` ever, the turn ends, and craze new --wait --plan reports
// `completed` with the plan, its stop reason as the agent sent it, and the
// plan auto-answered `returned`.
func TestAnUnattendedACPHostReturnsItsPlan(t *testing.T) {
	for _, tc := range []struct {
		provider, script, plan string
		argv                   []string
	}{
		{"cursor", "plan", "## Steps\n\n- read main.go\n- edit main.go\n", []string{"--plan"}},
		{"grok", "grok-plan", "## Steps\n\n- read main.go\n- edit main.go\n", nil},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			_, ws, _, calls := planReturnHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", tc.script)
			argv := append([]string{"new", "--wait", "--json", "--provider", tc.provider, "-C", ws}, tc.argv...)
			stdout, stderr, code := executeErr(append(argv, "plan the change"))
			m := decodeResult(t, stdout)
			auto, _ := json.Marshal(m["autoAnswered"])
			if code != 0 || m["status"] != "completed" || m["plan"] != tc.plan || m["stopReason"] != "cancelled" ||
				!strings.Contains(string(auto), `"choice":"returned"`) || strings.Contains(stdout, "planned:") {
				t.Fatalf("craze new --wait over a plan: exit %d, %v, %q", code, m, stderr)
			}
			got, err := os.ReadFile(calls)
			if err != nil || !slices.Contains(strings.Split(strings.TrimSpace(string(got)), "\n"), "session/cancel") {
				t.Fatalf("the agent was not told to stop: %q, %v", got, err)
			}
		})
	}
}

// TestAnAttendedHostStillParksItsPlan: the return is an unattended host's
// alone — an ordinary host on the same provider parks the plan for a person,
// as it always has (needs_input), and craze prompt still accepts it (its
// policy is unchanged: v2 parity).
func TestAnAttendedHostStillParksItsPlan(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "plan")
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--provider", "cursor")
	e := r.waitServing(t, env, true)
	promptUnattached(t, e, "plan it")
	waitFor(t, "the plan to park", func() bool { return listRow(t, e).PendingAsks == 1 })
	if h := listRow(t, e).HeadAsk; h == nil || h.Kind != string(agent.AskPlan) {
		t.Fatalf("the open ask is %+v, want the plan", h)
	}
	if lt := listRow(t, e).LastTurn; lt != nil {
		t.Fatalf("the turn ended %+v while its plan waited", lt)
	}
	if p := headlessApproval(true); p.Plan != agent.ApprovalAccept {
		t.Fatalf("craze prompt's plan policy is %q", p.Plan)
	}
}

// TestAReturnedPlansCancelIsCompleted is the waiter's rule for X28, both
// ways: a turn that ended cancelled after a plan answered automatically and
// not accepted (only the return policy writes one) is `completed`, with its
// plan; a cancelled turn with no plan, and one whose automatic plan was
// accepted, are `cancelled` — a person's craze cancel stays what it is.
func TestAReturnedPlansCancelIsCompleted(t *testing.T) {
	cancelled := func(emit ...agent.Event) stubTurn {
		return stubTurn{
			emit: append(emit, agent.Event{Type: agent.EventDone, StopReason: "cancelled"}),
			res:  agent.Result{StopReason: "cancelled"},
		}
	}
	plan := func(accepted bool) agent.Event {
		return agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{Name: "the plan", Plan: "## Steps", Auto: true, Accepted: accepted}}
	}
	for _, tc := range []struct {
		name   string
		turn   stubTurn
		status string
		choice string
	}{
		{"returned", cancelled(textEv("thinking"), plan(false)), statusCompleted, "returned"},
		{"no plan", cancelled(textEv("thinking")), statusCancelled, ""},
		{"accepted", cancelled(plan(true)), statusCancelled, "accepted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWaitRig(t, 0, tc.turn)
			conn := r.dial(nil)
			res, f := goWait(conn, waitStart{submit: submitPrompt(conn, "go")}, waitOptions{}).result(t)
			res = wantResult(t, res, f, tc.status)
			if res.StopReason != "cancelled" || (tc.choice != "") != (len(res.AutoAnswered) == 1) ||
				tc.choice != "" && (res.AutoAnswered[0].Choice != tc.choice || res.Plan != "## Steps") {
				t.Fatalf("the result %+v", res)
			}
			if tc.status == statusCompleted && strings.Contains(res.plain(), "stopped:") {
				t.Fatalf("a returned plan's footer says the turn stopped:\n%s", res.plain())
			}
		})
	}
}

// TestAReturnedPlanCarriesNoWireChange: the returned plan travels as the
// automatic plan opening every host already sends — accepted absent — and its
// automatic ending: nothing on the wire a client could not read before.
func TestAReturnedPlanCarriesNoWireChange(t *testing.T) {
	raw, err := agent.EncodeEvent(agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{ID: "plan-1", Name: "p", Plan: "x", Auto: true}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "accepted") || strings.Contains(raw, "returned") || !strings.Contains(raw, `"auto":true`) {
		t.Fatalf("a returned plan's opening: %s", raw)
	}
}
