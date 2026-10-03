package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/tool"
)

// The table a running session takes (SetTable, plan 034 §3.4, Q13–Q16;
// §7 A20, A21, A23, A24). Every case opens the fixture's table and hands the
// session another, loaded from files as the native adapter loads one.

// tableFrom loads a model table from providers and models TOML, as the
// adapter's reload does: a fresh directory each time.
func tableFrom(t *testing.T, providers, models string) *modeltable.Table {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, modeltable.ProvidersFile), []byte(providers), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, modeltable.ModelsFile), []byte(models), 0o644); err != nil {
		t.Fatal(err)
	}
	tbl, err := modeltable.Load(dir)
	if err != nil {
		t.Fatalf("loading a table: %v", err)
	}
	return tbl
}

// fixtureTOML is the fixture's own two files, for a case to edit.
func fixtureTOML(baseURL string) (providers, models string) {
	return fmt.Sprintf(providersTOML, baseURL), modelsTOML
}

// withModelE is models with a fifth model, test/e, on the funded provider.
func withModelE(models string) string {
	return models + `
[models."test/e"]
provider = "test"
wire_model = "wire-e"
name = "Model E"
efforts = ["low"]
default_effort = "low"
`
}

// withoutModel is models with alias's table taken out.
func withoutModel(t *testing.T, models, alias string) string {
	t.Helper()
	head := fmt.Sprintf("\n[models.%q]\n", alias)
	i := strings.Index(models, head)
	if i < 0 {
		t.Fatalf("no %s in the models file", alias)
	}
	rest := models[i+len(head):]
	if j := strings.Index(rest, "\n[models."); j >= 0 {
		return models[:i] + rest[j:]
	}
	return models[:i] + "\n"
}

// withDefault is models with its default_model set to alias.
func withDefault(models, alias string) string {
	return strings.Replace(models, `default_model = "test/a"`, fmt.Sprintf("default_model = %q", alias), 1)
}

// passesVar reports whether a command the session starts now is handed the
// variable name (toolset.environ).
func (ts *toolset) passesVar(name string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return slices.ContainsFunc(ts.environ, func(kv string) bool { return strings.HasPrefix(kv, name+"=") })
}

// swapMidSwitch has s's next SetModel meet SetTable(next) once it has built
// the new model's client and before it installs it (setModelBuilt).
func swapMidSwitch(t *testing.T, s *Session, next *modeltable.Table) {
	var once sync.Once
	s.setModelBuilt = func() {
		once.Do(func() {
			if err := s.SetTable(next, nil); err != nil {
				t.Errorf("SetTable: %v", err)
			}
		})
	}
}

// aliasesOf are a session's model aliases as Models lists them.
func aliasesOf(s *Session) []string {
	var out []string
	for _, m := range s.Models() {
		out = append(out, m.Alias)
	}
	return out
}

// TestSetTableRefusedWhileATurnRuns (A24's premise, A21): a table handed to a
// session while a turn holds it is refused with ErrTurnRunning, changing
// nothing — the turn, and every sub-agent it starts, reads one table — and
// taken once the turn has ended. After Close it is ErrClosed. Negative
// control: SetTable without the running check takes the table mid-turn and
// fails the first assertion.
func TestSetTableRefusedWhileATurnRuns(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	providers, models := fixtureTOML("http://127.0.0.1:1/v1")
	next := tableFrom(t, providers, withModelE(models))
	g := newGate()
	f.models["test/a"].push(g.hold(openText("working"), cat(finishText())))
	out := start(context.Background(), s, "go", nil)
	<-g.reached
	if err := s.SetTable(next, nil); !errors.Is(err, ErrTurnRunning) {
		t.Fatalf("SetTable during a turn = %v; want ErrTurnRunning", err)
	}
	if s.view().table != f.table || slices.Contains(aliasesOf(s), "test/e") {
		t.Fatal("a refused SetTable changed the table")
	}
	close(g.release)
	if o := await(t, out, "the turn"); o.err != nil {
		t.Fatalf("the turn: %v", o.err)
	}
	if err := s.SetTable(next, nil); err != nil {
		t.Fatalf("SetTable once the turn ended: %v", err)
	}
	if !slices.Contains(aliasesOf(s), "test/e") {
		t.Fatalf("after SetTable the session lists %v; want test/e", aliasesOf(s))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTable(tableFrom(t, providers, models), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetTable after Close = %v; want ErrClosed", err)
	}
}

// TestSetTableCarriesTheRunningModel (Q16, A20): the model a session runs on
// stays in its table, with its metadata, when the table it is handed has
// dropped its alias or no longer funds its provider; its client is not
// rebuilt, so the next turn runs on it as before. The opened table's
// compaction and sub-agent settings are carried too (Q13). The controls: a
// model the new table dropped that is not the running one is gone, and the
// new table's own model is there. Negative control: SetTable without the
// carry loses test/a from Models.
func TestSetTableCarriesTheRunningModel(t *testing.T) {
	providers, models := fixtureTOML("http://127.0.0.1:1/v1")
	for _, tc := range []struct {
		name, providers, models string
	}{
		{"its alias gone", providers, withDefault(withoutModel(t, withoutModel(t, withModelE(models), "test/a"), "test/b"), "test/e")},
		{"its key gone", strings.Replace(providers, `env_keys = ["TEST_API_KEY"]`, `env_keys = ["GONE_API_KEY"]`, 1),
			withoutModel(t, withModelE(models), "test/b")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			tiers := map[string]string{"opus": "test/b"}
			f.table.Subagents = modeltable.Subagents{Model: "test/b", Tiers: tiers}
			percent := 60
			f.table.Compaction.ThresholdPercentSet = &percent
			opened := f.table.Models["test/a"]
			s := f.open(f.options())
			next := tableFrom(t, tc.providers, tc.models)
			if err := s.SetTable(next, nil); err != nil {
				t.Fatalf("SetTable: %v", err)
			}
			if got := aliasesOf(s); !slices.Contains(got, "test/a") || slices.Contains(got, "test/b") || !slices.Contains(got, "test/e") {
				t.Fatalf("Models = %v; want the running test/a carried, test/b gone and test/e there", got)
			}
			if got := s.view().table.Models["test/a"]; !reflect.DeepEqual(got, opened) {
				t.Fatalf("the carried entry = %+v; want the opened one %+v", got, opened)
			}
			for _, m := range s.Models() {
				if m.Alias == "test/a" && (m.Name != "Model A" || !slices.Equal(m.Efforts, []string{"low", "high"}) || m.DefaultEffort != "high") {
					t.Fatalf("Models lists test/a as %+v; want its opened metadata", m)
				}
			}
			if model, effort := s.Current(); model != "test/a" || effort != "high" {
				t.Fatalf("Current = %s %s; want test/a high", model, effort)
			}
			if got := s.view().table.Subagents; got.Model != "test/b" || got.Tiers["opus"] != "test/b" {
				t.Fatalf("the sub-agent settings = %+v; want the opened table's", got)
			}
			tiers["opus"] = "mutated"
			if s.view().table.Subagents.Tiers["opus"] != "test/b" {
				t.Fatal("the carried tier map is the opened table's own")
			}
			if got := s.compactionConfig().ThresholdPercent(); got != 60 {
				t.Fatalf("the compaction threshold = %d; want the opened table's 60", got)
			}
			f.models["test/a"].push(answerWith("still here"))
			run(t, s, "go on")
			if built := f.built; len(built) != 1 {
				t.Fatalf("models built %v; want test/a's one client, untouched by the swap", built)
			}
		})
	}
}

// TestSetModelIsJudgedAgainstTheTableItInstalls (A24): a switch builds its
// client outside the lock, and a SetTable that lands meanwhile sends it round
// again against the new table: a model the new table dropped is refused as
// unknown and the session stays on a model its table lists; one the new
// table kept is switched to, built again from it. Negative control: SetModel
// that installs without checking the table it built from switches to test/b
// in the first case, which the table no longer lists.
func TestSetModelIsJudgedAgainstTheTableItInstalls(t *testing.T) {
	providers, models := fixtureTOML("http://127.0.0.1:1/v1")
	t.Run("the model dropped", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(f.options())
		swapMidSwitch(t, s, tableFrom(t, providers, withoutModel(t, models, "test/b")))
		if err := s.SetModel("test/b"); !errors.Is(err, ErrUnknownModel) {
			t.Fatalf("SetModel(test/b) across a swap that dropped it = %v; want ErrUnknownModel", err)
		}
		model, _ := s.Current()
		if model != "test/a" || !slices.Contains(aliasesOf(s), model) {
			t.Fatalf("the session runs on %s, and its table lists %v", model, aliasesOf(s))
		}
	})
	t.Run("the model kept", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(f.options())
		swapMidSwitch(t, s, tableFrom(t, providers, withModelE(models)))
		if err := s.SetModel("test/b"); err != nil {
			t.Fatalf("SetModel(test/b) across a swap that kept it: %v", err)
		}
		if model, _ := s.Current(); model != "test/b" {
			t.Fatalf("Current = %s; want test/b", model)
		}
		if got := f.built; !slices.Equal(got, []string{"test/a", "test/b", "test/b"}) {
			t.Fatalf("built %v; want test/b built again from the new table", got)
		}
	})
}

// TestASubagentNamesANewlyListedModel (A21): a model only the table handed to
// the session has is one a sub-agent call can name, resolved through the
// matcher handed over with it. Negative control: SetTable that leaves the
// session's table as it was answers the call "Unknown model".
func TestASubagentNamesANewlyListedModel(t *testing.T) {
	rf := newRouted(t)
	s := rf.open(rf.options())
	providers, models := fixtureTOML("http://127.0.0.1:1/v1")
	next := tableFrom(t, providers, withModelE(models))
	rf.routers["test/e"] = &router{provider: "test", wire: "wire-e", queues: map[string][]step{}, calls: map[string][]fantasy.Call{}}
	matched := false
	match := func(raw string) (string, bool) {
		if strings.EqualFold(raw, "model e") {
			matched = true
			return "test/e", true
		}
		_, ok := next.Models[raw]
		return raw, ok
	}
	if err := s.SetTable(next, match); err != nil {
		t.Fatal(err)
	}
	rf.routers["test/a"].route("go", callStep(agentPart(t, "a1", task("look", "child e", "model", "Model E"))), answerWith("done"))
	rf.routers["test/e"].route("child e", answerWith("e did it"))
	var evs events
	runWith(t, s, "go", evs.sink)
	started := startedWith(t, evs.list(), "child e")
	if started.Model != "test/e" || !matched {
		t.Fatalf("the child started on %q (matched by the new matcher: %v); want test/e", started.Model, matched)
	}
	if fin := finishedOf(t, evs.list(), started.ID); !strings.Contains(fin.Text, "e did it") {
		t.Fatalf("the child answered %q", fin.Text)
	}
}

// TestAChildOperationReadsOneTable (A21): a sub-agent call resolves its model
// and effort and opens its child against one reading of the table, taken
// before it resolves. A SetTable during the call is refused — the parent's
// turn holds the session — and even a table swapped in under the lock between
// the resolution and the Open (the refusal's own failure, simulated) does not
// reach the child: it opens on the table its model was resolved against,
// with that table's matcher. Negative control: childOpenOptions reading the
// session's table again opens the child on the swapped one.
func TestAChildOperationReadsOneTable(t *testing.T) {
	rf := newRouted(t)
	s := rf.open(rf.options())
	providers, models := fixtureTOML("http://127.0.0.1:1/v1")
	resolved := tableFrom(t, providers, withModelE(models))
	swapped := tableFrom(t, providers, models)
	rf.routers["test/e"] = &router{provider: "test", wire: "wire-e", queues: map[string][]step{}, calls: map[string][]fantasy.Call{}}
	match := func(raw string) (string, bool) { _, ok := resolved.Models[raw]; return raw, ok }
	if err := s.SetTable(resolved, match); err != nil {
		t.Fatal(err)
	}
	var refused error
	s.subs.seams.acquired = func(tool.SubagentCall) {
		refused = s.SetTable(swapped, nil)
		s.mu.Lock()
		s.table, s.matchModel = swapped, nil
		s.mu.Unlock()
	}
	var openedOn *modeltable.Table
	var openedMatch func(string) (string, bool)
	s.subs.seams.open = func(o Options) (*Session, error) {
		openedOn, openedMatch = o.Table, o.MatchModel
		return Open(o)
	}
	rf.routers["test/a"].route("go", callStep(agentPart(t, "a1", task("look", "child e", "model", "test/e"))), answerWith("done"))
	rf.routers["test/e"].route("child e", answerWith("e did it"))
	var evs events
	runWith(t, s, "go", evs.sink)
	if !errors.Is(refused, ErrTurnRunning) {
		t.Fatalf("SetTable during the call = %v; want ErrTurnRunning", refused)
	}
	if openedOn != resolved || openedMatch == nil {
		t.Fatal("the child opened on another table than the one its model was resolved against")
	}
	if started := startedWith(t, evs.list(), "child e"); started.Model != "test/e" {
		t.Fatalf("the child started on %q; want test/e", started.Model)
	}
}

// TestSetTableKeepsANewProvidersVariableFromCommands (A23): a variable the
// handed table names as a provider's key — a provider configured while the
// session runs, whose key the process's environment already held — reaches no
// command the session starts from then on, nor a running sub-agent's; one a
// table the session held named stays out after a later table stops naming
// it, and a sub-agent opened then starts without it too. The control: the
// turn before the swap printed it. Negative controls: SetTable without its
// narrowing prints the value in the second turn; openChild without dropEnv
// leaves the child its variable.
func TestSetTableKeepsANewProvidersVariableFromCommands(t *testing.T) {
	const value = "sk-newprov-in-env-0001"
	t.Setenv("C4_NEWPROV_KEY", value)
	t.Setenv("OTHER_API_KEY", canaryOther)
	rf := newRouted(t)
	s := rf.open(rf.options())
	printenv := input(t, map[string]any{"command": "printenv C4_NEWPROV_KEY || echo unset"})
	a := rf.routers["test/a"]
	// Both turns are routed under the first prompt: it is every later
	// request's first user message too.
	a.route("before", callStep(callParts("b1", "bash", printenv)), answerWith("one"),
		callStep(callParts("b2", "bash", printenv)), answerWith("two"))
	bashOut := func(evs []Event) string {
		t.Helper()
		for _, f := range of[ToolFinished](evs) {
			return f.Result.Text
		}
		t.Fatal("no bash result")
		return ""
	}

	// A running sub-agent, registered and attached as the runner does.
	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	h, mode, ok := s.subs.register("child-running", cancel)
	if !ok {
		t.Fatal("register refused")
	}
	running, err := s.subs.openChild(s.view(), h, tool.SubagentCall{ID: "t0.1.1"}, tool.Persona{Name: "general", AllTools: true}, "test/a", "", mode)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = running.Close() })
	h.attachChild(running)

	var first events
	runWith(t, s, "before", first.sink)
	if out := bashOut(first.list()); !strings.Contains(out, value) {
		t.Fatalf("control: before the swap the command printed %q; the variable did not reach it, so its absence later proves nothing", out)
	}

	providers, models := fixtureTOML("http://127.0.0.1:1/v1")
	providers += `
[providers.newprov]
driver = "openai-compat"
base_url = "http://127.0.0.1:1/v1"
env_keys = ["C4_NEWPROV_KEY"]
`
	models += `
[models."newprov/x"]
provider = "newprov"
wire_model = "wire-x"
`
	if err := s.SetTable(tableFrom(t, providers, models), nil); err != nil {
		t.Fatal(err)
	}
	var second events
	runWith(t, s, "before", second.sink)
	if out := bashOut(second.list()); strings.Contains(out, value) || !strings.Contains(out, "unset") {
		t.Fatalf("after the swap the command printed %q; want the variable unset", out)
	}
	if running.tools.passesVar("C4_NEWPROV_KEY") {
		t.Fatal("a sub-agent running at the swap keeps the new provider's variable for its commands")
	}

	// A later table that names neither provider any more.
	plainProviders, plain := fixtureTOML("http://127.0.0.1:1/v1")
	noOther := strings.Replace(plainProviders, "[providers.other]\ndriver = \"openai-compat\"\nbase_url = \"http://127.0.0.1:1/v1\"\nenv_keys = [\"OTHER_API_KEY\"]\n", "", 1)
	if err := s.SetTable(tableFrom(t, noOther, withoutModel(t, plain, "other/c")), nil); err != nil {
		t.Fatal(err)
	}
	child, err := s.subs.openChild(s.view(), &childHandle{id: "child-after"}, tool.SubagentCall{ID: "t3.1.1"}, tool.Persona{Name: "general", AllTools: true}, "test/a", "", modeAgent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Close() })
	for _, name := range []string{"C4_NEWPROV_KEY", "OTHER_API_KEY"} {
		if s.tools.passesVar(name) {
			t.Fatalf("the session's commands get %s again after a table stopped naming it", name)
		}
		if child.tools.passesVar(name) {
			t.Fatalf("a sub-agent opened on the later table gets %s", name)
		}
	}
}

// TestSpendIsUnchangedAcrossASwap (A23): what a session spent is priced by
// the current table, and an identity it does not price by the table the
// session opened with — so handing it a table that dropped the priced model
// it spent on, once it has switched off it, changes neither the turn's spend
// nor the session's. The control: the spend was priced (non-zero, not
// Unpriced). Negative control: a pricer without the open-time fallback marks
// the spend Unpriced and loses its cost.
func TestSpendIsUnchangedAcrossASwap(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	in, out := 2.0, 8.0
	m := f.table.Models["test/a"]
	m.Cost = &modeltable.Cost{Input: &in, Output: &out}
	f.table.Models["test/a"] = m
	s := f.open(f.options())
	f.models["test/a"].push(answerWith("priced"))
	run(t, s, "spend something")
	turn, before := s.spend(1)
	if before.CostPicoUSD == 0 || before.Unpriced {
		t.Fatalf("control: the spend before the swap is %+v; want it priced", before)
	}
	if err := s.SetModel("test/b"); err != nil {
		t.Fatal(err)
	}
	providers, models := fixtureTOML("http://127.0.0.1:1/v1")
	if err := s.SetTable(tableFrom(t, providers, withDefault(withoutModel(t, models, "test/a"), "test/b")), nil); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(aliasesOf(s), "test/a") {
		t.Fatal("premise: the new table still lists test/a")
	}
	turnAfter, after := s.spend(1)
	if after != before || turnAfter != turn {
		t.Fatalf("spend after the swap = %+v (turn %+v); want %+v (turn %+v)", after, turnAfter, before, turn)
	}
}
