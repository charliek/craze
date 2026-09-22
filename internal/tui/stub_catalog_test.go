package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// The Stub's per-model catalog (plan 025 F1): with a table installed a model
// change carries the destination's catalog in the same delta, as the live
// session's one-call switch will; without one it is the Stub every golden was
// drawn from.

// catalogIDs is a catalog as "id=value" pairs, in order.
func catalogIDs(cfg []agent.ConfigOption) string {
	out := make([]string, 0, len(cfg))
	for _, o := range cfg {
		out = append(out, o.ID+"="+o.Current)
	}
	return strings.Join(out, " ")
}

// oneModelDelta is the single delta a SetModel published.
func oneModelDelta(t *testing.T, s *Stub, model string) *agent.StateDelta {
	t.Helper()
	evs := stubDeltas(t, s)
	if len(evs) != 1 || evs[0].Type != agent.EventMeta || evs[0].State == nil {
		t.Fatalf("SetModel published %+v, want one delta", evs)
	}
	st := evs[0].State
	if st.Model == nil || *st.Model != model {
		t.Fatalf("the delta's model section is %v, want %q", st.Model, model)
	}
	return st
}

// TestStubSetModelWithNoTableKeepsTheStaticCatalog: today's Stub, exactly — the
// delta says the model and nothing else, and the static effort/fast catalog the
// `model-dialog-*` goldens are drawn from outlives the change.
func TestStubSetModelWithNoTableKeepsTheStaticCatalog(t *testing.T) {
	s := NewStub()
	t.Cleanup(func() { _ = s.Close() })
	const static = "effort=medium fast=false"
	if got := catalogIDs(s.Snapshot().Config); got != static {
		t.Fatalf("a new Stub's catalog is %s, want %s", got, static)
	}
	out, err := s.SetModel(context.Background(), "c-1", "fast")
	if err != nil || out.Value != "fast" {
		t.Fatalf("SetModel = %+v, %v", out, err)
	}
	if st := oneModelDelta(t, s, "fast"); st.Config != nil {
		t.Fatalf("with no table the delta carried a config section: %+v", st.Config)
	}
	if got := catalogIDs(s.Snapshot().Config); got != static {
		t.Fatalf("with no table the catalog moved to %s", got)
	}
}

// TestStubSetModelInstallsTheModelsCatalog: with a table the catalog is always
// the current model's — on install, and on every SetModel in the one delta that
// moves the model — a model the table does not name advertises nothing, a
// refused SetModel changes nothing, and a value set on one model is still there
// when the session comes back to it.
func TestStubSetModelInstallsTheModelsCatalog(t *testing.T) {
	s := NewStub()
	t.Cleanup(func() { _ = s.Close() })
	grok := s.Snapshot().Config
	table := map[string][]agent.ConfigOption{"grok": grok, "fast": stubFourSelectCatalog()}
	s.SetModelCatalogs(table)
	// The table is the Stub's own copy: a test's later edit reaches nothing.
	table["fast"][0].Current = "low"
	if n := len(stubDeltas(t, s)); n != 0 {
		t.Fatalf("installing the table published %d events", n)
	}
	if got := catalogIDs(s.Snapshot().Config); got != "effort=medium fast=false" {
		t.Fatalf("on install the catalog is %s, want grok's", got)
	}

	const four = "effort=high fast=false context=300k thinking=true"
	if _, err := s.SetModel(context.Background(), "c-1", "fast"); err != nil {
		t.Fatal(err)
	}
	st := oneModelDelta(t, s, "fast")
	if st.Config == nil || catalogIDs(st.Config.Options) != four {
		t.Fatalf("the delta's config section is %+v, want %s", st.Config, four)
	}
	if got := catalogIDs(s.Snapshot().Config); got != four {
		t.Fatalf("the snapshot's catalog is %s, want %s", got, four)
	}

	s.FailNextSetModel()
	if _, err := s.SetModel(context.Background(), "c-2", "grok"); err == nil {
		t.Fatal("the failed SetModel succeeded")
	}
	if evs := stubDeltas(t, s); len(evs) != 0 || catalogIDs(s.Snapshot().Config) != four {
		t.Fatalf("a refused SetModel published %d events and left %s", len(evs), catalogIDs(s.Snapshot().Config))
	}

	if _, err := s.SetModel(context.Background(), "c-3", "composer"); err != nil {
		t.Fatal(err)
	}
	if st := oneModelDelta(t, s, "composer"); st.Config == nil || len(st.Config.Options) != 0 {
		t.Fatalf("a model with no entry published %+v, want an empty config section", st.Config)
	}
	if got := s.Snapshot().Config; len(got) != 0 {
		t.Fatalf("a model with no entry advertises %s", catalogIDs(got))
	}

	// Per-model values persist, as cursor's do.
	if _, err := s.SetModel(context.Background(), "c-4", "fast"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetConfig(context.Background(), "c-5", "context", "1m"); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"grok", "fast"} {
		if _, err := s.SetModel(context.Background(), "c-6", m); err != nil {
			t.Fatal(err)
		}
	}
	if got := catalogIDs(s.Snapshot().Config); got != "effort=high fast=false context=1m thinking=true" {
		t.Fatalf("back on fast the catalog is %s: the context value did not persist", got)
	}
}

// TestStubFourSelectCatalogIsWhatTheHeuristicsSee: the four-select catalog's
// effort and fast are the ones EffortOption and FastOption find, and its two
// other tabs are named so that lowercasing gives "context" and "thinking".
func TestStubFourSelectCatalogIsWhatTheHeuristicsSee(t *testing.T) {
	snap := agent.Snapshot{Config: stubFourSelectCatalog()}
	if opt := agent.EffortOption(snap); opt == nil || opt.ID != "effort" {
		t.Fatalf("EffortOption found %+v", opt)
	}
	if opt := agent.FastOption(snap); opt == nil || opt.ID != "fast" {
		t.Fatalf("FastOption found %+v", opt)
	}
	var names []string
	for _, o := range snap.Config {
		if o.Type != "select" || len(o.SelectValues) < 2 {
			t.Fatalf("%s is not a select with values: %+v", o.ID, o)
		}
		names = append(names, strings.ToLower(o.Name))
	}
	if got := strings.Join(names, " "); got != "effort fast context thinking" {
		t.Fatalf("the lowercased names are %q", got)
	}
}
