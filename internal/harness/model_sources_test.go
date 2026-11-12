package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
)

// Plan 050 C1 in the harness: the context default (compaction's threshold and
// the usage report's window), the agent tool's model menu and its "Unknown
// model" list held to the sub-agent choice set, and a child's model named
// as "<provider id>/<alias>".

// deprecate marks alias of models deprecated, replaced by by, as a catalog
// would.
func deprecate(models map[string]modeltable.Model, alias, by string) {
	m := models[alias]
	m.Deprecated, m.ReplacedBy = true, by
	models[alias] = m
}

// TestCompactionThresholdHonoursTheContextDefault: the threshold is the share
// of the context in force (default_context below the window, else the
// window), held to the true window less the output ceiling; a default with
// no known window is no automatic compaction, as an unknown window always
// was. On an OpenAI seed (a 1,048,576 window, a 128,000 ceiling, 272,000 in
// force) a session compacts at 231,200 instead of 891,289.
func TestCompactionThresholdHonoursTheContextDefault(t *testing.T) {
	for _, tc := range []struct {
		name                string
		window, maxOut, def int
		inForce             int
		threshold           int64
	}{
		{"the OpenAI seeds", 1_048_576, 128_000, 272_000, 272_000, 231_200},
		{"no default: the window", 1_048_576, 128_000, 0, 1_048_576, 891_289},
		{"a default at the window: the window", 200_000, 0, 200_000, 200_000, 170_000},
		{"a default above a lowered window: the window", 100_000, 0, 272_000, 100_000, 85_000},
		{"the window less the ceiling still caps it", 300_000, 128_000, 272_000, 272_000, 172_000},
		{"a default and no window: none", 0, 0, 272_000, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := modeltable.Resolved{ContextWindow: tc.window, MaxOutputTokens: tc.maxOut, DefaultContext: tc.def}
			if got := r.ContextInForce(); got != tc.inForce {
				t.Errorf("ContextInForce = %d, want %d", got, tc.inForce)
			}
			if got := compactionThreshold(r, 85); got != tc.threshold {
				t.Errorf("compactionThreshold = %d, want %d", got, tc.threshold)
			}
		})
	}
}

// TestUsageReportsTheContextInForce: a session on a model with a context
// default reports that default as the usage's window, so a share of it means
// what the threshold means; the text form's budget and the ceiling sent stay
// the true window's. A child on another model works within its own model's
// default, not the parent's.
func TestUsageReportsTheContextInForce(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	windowed(f, "test/a", 1_048_576, 128_000)
	a := f.table.Models["test/a"]
	a.DefaultContext = 272_000
	f.table.Models["test/a"] = a
	windowed(f, "other/c", 400_000, 0)
	c := f.table.Models["other/c"]
	c.DefaultContext = 100_000
	f.table.Models["other/c"] = c

	s := f.open(f.options())
	if got := compactionThreshold(s.cur.r, 85); got != 231_200 {
		t.Fatalf("the session's threshold = %d, want 231,200", got)
	}
	if got := textFormBudget(s.cur.r, 0); got != 1_048_576*70/100-128_000 {
		t.Fatalf("the text form's budget = %d, want the true window's", got)
	}
	f.models["test/a"].push(answerWith("ok"))
	var live events
	if _, err := s.Run(context.Background(), "hi", live.sink); err != nil {
		t.Fatal(err)
	}
	if got := lastSpent(t, live.list()).ContextWindow; got != 272_000 {
		t.Fatalf("the usage's window = %d, want the context in force, 272,000", got)
	}

	copts := childOf(f, s, ChildOptions{AllTools: true})
	copts.Model = "other/c"
	child := f.open(copts)
	if got := compactionThreshold(child.cur.r, 85); got != 85_000 {
		t.Fatalf("the child's threshold = %d, want its own model's default's share, 85,000", got)
	}
}

// TestSubagentMenuHoldsToTheChoiceSet: the agent tool's menu lists the
// offered models — a deprecated one is not — plus what the owner configured,
// a deprecated model among them, and the parent's own model; the "Unknown
// model" error names the same set.
func TestSubagentMenuHoldsToTheChoiceSet(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	deprecate(f.table.Models, "test/b", "test/a")
	deprecate(f.table.Models, "other/c", "test/a")
	red := redact.New()

	plain := renderSubagentModels(f.table, f.getenv, nil, red)
	if strings.Contains(plain, "test/b") || strings.Contains(plain, "other/c") || !strings.Contains(plain, "test/a") {
		t.Fatalf("the menu with nothing configured is\n%s\nwant test/a and no deprecated model", plain)
	}
	if got := renderSubagentModels(f.table, f.getenv, []string{"other/c"}, red); !strings.Contains(got, "other/c") {
		t.Fatalf("the menu of a session on other/c is\n%s\nwant its own model listed", got)
	}
	f.table.Subagents = modeltable.Subagents{Tiers: map[string]string{"sonnet": "test/b"}}
	tiered := renderSubagentModels(f.table, f.getenv, nil, red)
	if !strings.Contains(tiered, "- test/b") || !strings.Contains(tiered, "sonnet → test/b") || strings.Contains(tiered, "other/c") {
		t.Fatalf("the menu with a tier on test/b is\n%s\nwant the configured deprecated model listed, and no other", tiered)
	}

	s := resolverSession(t, map[string]string{"sonnet": "test/b"}, "", "", nil)
	deprecate(s.table.Models, "test/b", "test/a")
	deprecate(s.table.Models, "other/c", "test/a")
	_, err := s.resolveChildModel(s.view(), childModelInput{Call: "nope", ParentAlias: "test/a"}, nil)
	if err == nil || !strings.Contains(err.Error(), "Models: test/a, test/b;") {
		t.Fatalf("the unknown-model error = %v; want the choice set, test/a and the configured test/b", err)
	}
	// A deprecated model still runs when a call names it.
	if got, err := s.resolveChildModel(s.view(), childModelInput{Call: "other/c", ParentAlias: "test/a"}, nil); err != nil || got != "other/c" {
		t.Fatalf("a call naming deprecated other/c = %q, %v; want it to run", got, err)
	}
}

// TestChildModelByProvider: with no matcher, a call's model written
// "<provider id>/<alias>" finds the alias on that provider, exactly as
// written, and only after the plain match found nothing.
func TestChildModelByProvider(t *testing.T) {
	s := resolverSession(t, nil, "", "", nil)
	m := s.table.Models["test/a"]
	s.table.Models["plain"] = m
	for raw, want := range map[string]string{"test/plain": "plain", "test/test/a": "test/a", "test/a": "test/a"} {
		if got, err := s.resolveChildModel(s.view(), childModelInput{Call: raw, ParentAlias: "test/a"}, nil); err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"other/plain", "nope/plain", "TEST/plain"} {
		if got, err := s.resolveChildModel(s.view(), childModelInput{Call: raw, ParentAlias: "test/a"}, nil); err == nil {
			t.Errorf("%q = %q; want the unknown-model error", raw, got)
		}
	}
}

// TestResumedMenuListsTheResumedModel: a session resumed on a deprecated,
// funded model that nothing configured still lists it in the agent tool's
// menu as the parent's own model — the menu is fixed when the tools open,
// before the resume has settled its model, so it must already know the
// model the transcript names. A new session's menu lists the model it
// opened on the same way.
func TestResumedMenuListsTheResumedModel(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	opts := f.options()
	opts.tools.profiles = agentProfile
	id := storedTurn(t, f, opts, func(s *Session) {
		if err := s.SetModel("test/b"); err != nil {
			t.Fatal(err)
		}
	})
	deprecate(f.table.Models, "test/b", "test/a")
	if f.table.DefaultModel == "test/b" || f.table.Subagents.Model != "" || len(f.table.Subagents.Tiers) != 0 {
		t.Fatalf("premise: test/b is configured (pin %q, %+v)", f.table.DefaultModel, f.table.Subagents)
	}
	s := resumed(t, resumeOptions(opts, id))
	if m, _ := s.Current(); m != "test/b" {
		t.Fatalf("premise: resumed on %s, want test/b", m)
	}
	if d := specOf(t, s, "agent").Description; !strings.Contains(d, "\n- test/b") {
		t.Fatalf("the resumed session's agent tool lists\n%s\nwant its own model, test/b", d)
	}
	// Control: a session that is not on test/b does not list it.
	other := f.open(opts)
	if d := specOf(t, other, "agent").Description; strings.Contains(d, "\n- test/b") {
		t.Fatalf("a session on another model lists deprecated test/b:\n%s", d)
	}
}
