package remote_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/tui"
)

// The effort facts over the socket (plan 047 D2, §7 A2): a session that
// states every model's effort facts says so — the session capability
// effortFacts, mapped back to backend.SessionInfo.EffortFacts — and every
// model of its catalogs carries them, in the attach's document, in a catalog
// delta, after a reconnect and in a reset's restore. A session that states
// none, the plain Stub — every ACP session's document, and an older host's —
// says EffortFacts false.

// factModels is a list whose models state their effort facts, a model with
// no effort control among them.
func factModels(switchTo string) []agent.ModelInfo {
	return []agent.ModelInfo{
		{ID: "muse-wonder-1.0", Name: "Muse Wonder 1.0 (Meta)", Recent: 1,
			Efforts: []string{"minimal", "low", "medium", "high", "xhigh", "max"}, DefaultEffort: "high", SwitchEffort: switchTo},
		{ID: "glm-5.3", Name: "GLM 5.3 (Z.AI)", Efforts: []string{"low", "high", "max"}, DefaultEffort: "max", SwitchEffort: "max"},
		{ID: "plain", Name: "No effort control"},
	}
}

// withEffortFacts serves a Stub that states effort facts, its models
// factModels("max") and, with dir set, taking up models while it runs
// (tui.RefreshingStub) from dir.
func withEffortFacts(dir string) hostOpt {
	return func(c *hostConfig) {
		c.session = func(s *tui.Stub) agent.Session {
			s.PublishEffortFacts = true
			s.SetModels(factModels("max"))
			if dir == "" {
				return s
			}
			s.SetNativeDir(dir)
			return tui.RefreshingStub{Stub: s}
		}
	}
}

// TestTheEffortFactsComeBackOverTheSocket (A2): the attach's document and
// its restore say effortFacts and carry every model's facts, as the host's
// session states them; Info's list is the caller's own, its levels too; a
// refresh's catalog delta carries the new facts; a reconnect's re-attach
// replays the next one with them, and Info keeps its facts and the
// capability. A plain Stub says EffortFacts false and states none. Negative
// control: a sessionInfo that leaves the facts out of its models fails the
// first check.
func TestTheEffortFactsComeBackOverTheSocket(t *testing.T) {
	h := newHost(t, withEffortFacts("/n"))
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	s, restore := started(t, h, tp, remote.SessionOptions{})
	if !reflect.DeepEqual(s.Info().Models, factModels("max")) || !reflect.DeepEqual(restore.Info.Models, factModels("max")) {
		t.Fatalf("the attach's models are %+v (restore %+v), want the host's %+v", s.Info().Models, restore.Info.Models, factModels("max"))
	}
	if !s.Info().EffortFacts || !restore.Info.EffortFacts {
		t.Fatalf("a session that states effort facts: Info %v, the restore's %v", s.Info().EffortFacts, restore.Info.EffortFacts)
	}
	info := s.Info()
	info.Models[0].Efforts[0] = "changed"
	if got := s.Info().Models[0].Efforts[0]; got != "minimal" {
		t.Fatalf("a change to Info's levels reached the Session's own: %q", got)
	}

	// A refresh while connected: its delta carries the new facts.
	if r := hostRefresh(t, h, factModels("low")); r.Status != agent.ModelsApplied || r.Revision != 1 {
		t.Fatalf("the refresh: %+v", r)
	}
	if c := catalogOf(readItem(t, s)); c == nil || c.Revision != 1 || !reflect.DeepEqual(c.Models, factModels("low")) {
		t.Fatalf("the refresh's delta carried %+v; want revision 1's facts", c)
	}

	// A reconnect: the host takes up revision 2 while the client is away, and
	// the re-attach's replay and document both carry its facts.
	held := tp.holdDials()
	tp.kill()
	await(t, held, "the redial")
	if r := hostRefresh(t, h, factModels("xhigh")); r.Revision != 2 {
		t.Fatalf("revision 2: %+v", r)
	}
	tp.releaseDials()
	if c := catalogOf(readItem(t, s)); c == nil || c.Revision != 2 || !reflect.DeepEqual(c.Models, factModels("xhigh")) {
		t.Fatalf("the reconnect's replay carried %+v; want revision 2's facts", c)
	}
	// Info is the document the Session holds — a cursor re-attach's replay is
	// what moves the list on (TestAReconnectNeverRestoresAnOlderCatalog) — and
	// it keeps its facts and the capability.
	if info := s.Info(); !info.EffortFacts || info.CatalogRevision > 2 || !reflect.DeepEqual(info.Models[1:], factModels("xhigh")[1:]) ||
		len(info.Models[0].Efforts) != 6 || info.Models[0].SwitchEffort == "" {
		t.Fatalf("after the reconnect Info is %+v (effortFacts %v); want the facts kept", info.Models, info.EffortFacts)
	}

	plain := newHost(t)
	p, restore := started(t, plain, newTap(t), remote.SessionOptions{})
	if p.Info().EffortFacts || restore.Info.EffortFacts {
		t.Fatalf("a session that states none: Info %v, restore %v", p.Info().EffortFacts, restore.Info.EffortFacts)
	}
	if slices.ContainsFunc(p.Info().Models, func(m agent.ModelInfo) bool { return m.Efforts != nil || m.SwitchEffort != "" }) {
		t.Fatalf("a session that states none carried facts: %+v", p.Info().Models)
	}
}

// TestAResetsRestoreKeepsTheEffortFacts (A2): a reset — a record too large
// to journal, reset{omitted} — is answered by a re-attach whose Restore
// carries the document again, effortFacts and every model's facts with it.
func TestAResetsRestoreKeepsTheEffortFacts(t *testing.T) {
	h := newHost(t, withLog(agent.EventLogOptions{MaxRecordBytes: 4 << 10}))
	h.stub.PublishEffortFacts = true
	h.stub.SetModels(factModels("max"))
	s, first := started(t, h, newTap(t), remote.SessionOptions{})
	if !first.Info.EffortFacts {
		t.Fatal("control: the first restore does not say effortFacts")
	}
	h.text(big)
	items := readUntil(t, s, func(it backend.Item) bool { return it.Kind == backend.ItemRestore })
	r := items[len(items)-1]
	if !r.Info.EffortFacts || !reflect.DeepEqual(r.Info.Models, factModels("max")) {
		t.Fatalf("the reset's restore says effortFacts %v, models %+v; want the facts kept", r.Info.EffortFacts, r.Info.Models)
	}
}
