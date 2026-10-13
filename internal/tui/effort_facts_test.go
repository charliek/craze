package tui

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/harness"
	tea "github.com/charmbracelet/bubbletea"
)

// The effort facts in the TUI's reads (plan 047 D2, §7 A2): the engine
// backend's Info says whether the session states them, and carries them on
// its models; the mirror's model list — whichever of Info's and the fold's
// is newer by revision — keeps them; and a real native session's facts reach
// the TUI in process and over the socket. (What the TUI shows of them is
// C2's.)

// TestTheEngineBackendSaysWhetherTheSessionStatesEffortFacts (A2): in
// process, Info().EffortFacts is the engine's PublishesEffortFacts — what
// the info document says over the socket — so a plain Stub, even one naming
// the native provider, says false and a PublishEffortFacts Stub says true,
// and Info's models carry the facts the session states. Negative control:
// an Info that leaves EffortFacts unset fails the third case.
func TestTheEngineBackendSaysWhetherTheSessionStatesEffortFacts(t *testing.T) {
	facts := []agent.ModelInfo{{ID: "m", Name: "M", Efforts: []string{"low", "max"}, DefaultEffort: "low", SwitchEffort: "max"}}
	for _, tc := range []struct {
		name    string
		native  bool
		publish bool
	}{{"a plain Stub", false, false}, {"a Stub naming native", true, false}, {"a PublishEffortFacts Stub", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			stub := NewStub()
			stub.PublishEffortFacts = tc.publish
			stub.SetModels(facts)
			if tc.native {
				stub.SetProvider(agent.NativeProvider())
			}
			eng, err := engine.New(stub, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = eng.Close() })
			if err := eng.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			info := newEngineBackend(eng, "").Info()
			if info.EffortFacts != tc.publish {
				t.Fatalf("Info().EffortFacts is %v, want %v", info.EffortFacts, tc.publish)
			}
			if !reflect.DeepEqual(info.Models, facts) {
				t.Fatalf("Info's models are %+v, want %+v", info.Models, facts)
			}
		})
	}
}

// TestTheMirrorKeepsTheEffortFacts (A2): the mirror's list keeps every
// model's facts whichever list it shows — Info's, newer, or the fold's
// catalog section, newer — and a delta's levels changed after its fold do
// not reach it. Negative control: a fold that copies a catalog's models
// shallowly shows the change made after the fold.
func TestTheMirrorKeepsTheEffortFacts(t *testing.T) {
	m := sized(t)
	stub := stubOf(t, m)
	model := func(switchTo string) []agent.ModelInfo {
		return []agent.ModelInfo{{ID: "test/a", Name: "A", Efforts: []string{"low", "high"}, DefaultEffort: "high", SwitchEffort: switchTo}}
	}
	stub.mu.Lock()
	stub.snap.Models, stub.snap.CatalogRevision = model("low"), 2
	stub.mu.Unlock()
	m.recompute()
	if !reflect.DeepEqual(m.snap.Models, model("low")) {
		t.Fatalf("Info's list, newer, is shown as %+v; want its facts", m.snap.Models)
	}
	delta := catalogDelta(101, 3, model("high"))
	m = feed(t, m, delta)
	if !reflect.DeepEqual(m.snap.Models, model("high")) {
		t.Fatalf("the fold's list, newer, is shown as %+v; want its facts", m.snap.Models)
	}
	delta.State.Catalog.Models[0].Efforts[0] = "changed"
	m.recompute()
	if !slices.Equal(m.snap.Models[0].Efforts, []string{"low", "high"}) {
		t.Fatalf("a delta's levels changed after its fold reached the mirror: %v", m.snap.Models[0].Efforts)
	}
}

// liveNative is a real native session on the shipped catalog, Fireworks
// funded, its Home home: what the live sign-in test runs (liveModel answers,
// nothing reaches a network).
func liveNative(t *testing.T, home string) agent.Session {
	t.Helper()
	getenv := func(k string) string {
		if k == "FIREWORKS_API_KEY" {
			return "fw-live-dummy-key-0471"
		}
		return ""
	}
	sess := agent.NewNative(agent.Options{Workspace: t.TempDir(), ContentHome: t.TempDir(), Interactive: true, Model: "fireworks/ember-1"},
		func(o *harness.Options) {
			o.Home, o.Getenv, o.NewModel = home, getenv, liveModel
			o.Now = func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }
		})
	return sess
}

// TestARealNativeSessionsFactsReachTheTUI (A2): a real native session states
// its models' effort facts — Fireworks' GLM 5.3 offers low to max, at max —
// and they reach the TUI in process, through the engine backend, and
// over the socket, through the attach's document, each with the capability;
// and the list a switch publishes in its own delta reaches the in-process
// TUI's mirror with the facts it changed: after Ember-1 is set to low and
// the session switches to GLM 5.3, Ember-1's row says a switch back lands on
// low, ranked after GLM 5.3. Negative
// control: a control server that leaves effortFacts out of its document
// fails the socket's check.
func TestARealNativeSessionsFactsReachTheTUI(t *testing.T) {
	isolateSkillsHome(t)
	noOpenAIForLive(t)
	wantGLM := func(t *testing.T, where string, models []agent.ModelInfo) {
		t.Helper()
		i := slices.IndexFunc(models, func(m agent.ModelInfo) bool { return m.ID == "fireworks/glm-5p3" })
		if i < 0 {
			t.Fatalf("%s: fireworks/glm-5p3 is not offered: %v", where, offeredModelIDs(models))
		}
		if m := models[i]; !slices.Equal(m.Efforts, []string{"low", "medium", "high", "xhigh", "max"}) || m.DefaultEffort != "max" || m.SwitchEffort != "max" {
			t.Fatalf("%s: fireworks/glm-5p3 is %+v; want low to max, at max", where, m)
		}
	}

	t.Run("in process", func(t *testing.T) {
		home := t.TempDir()
		sess := liveNative(t, home)
		if err := sess.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = sess.Close() })
		m := New(Config{Session: sess, Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true, ProviderLocked: true, NativeDir: home})
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		tm, _ = tm.(Model).Update(startedMsg{})
		m = tm.(Model)
		drainPublished(t, &m, sess)
		if !m.info().EffortFacts {
			t.Fatal("in process, Info does not say the native session states effort facts")
		}
		wantGLM(t, "Info", m.info().Models)
		wantGLM(t, "the mirror", m.snap.Models)
		if m.snap.CurrentModel != "fireworks/ember-1" {
			t.Fatalf("control: the session is on %s, offering %v", m.snap.CurrentModel, offeredModelIDs(m.snap.Models))
		}
		eng := engineOf(t, m)
		if _, err := eng.Set(context.Background(), engine.Command{}, engine.Setting{Kind: engine.SettingConfig, ID: "effort", Value: "low", ForModel: "fireworks/ember-1"}); err != nil {
			t.Fatalf("the effort change: %v", err)
		}
		if _, err := eng.Set(context.Background(), engine.Command{}, engine.Setting{Kind: engine.SettingModel, Value: "fireworks/glm-5p3"}); err != nil {
			t.Fatalf("the switch: %v", err)
		}
		drainPublished(t, &m, sess)
		if m.snap.CurrentModel != "fireworks/glm-5p3" {
			t.Fatalf("after the switch the mirror is on %s", m.snap.CurrentModel)
		}
		row := m.snap.Models[slices.IndexFunc(m.snap.Models, func(md agent.ModelInfo) bool { return md.ID == "fireworks/ember-1" })]
		if row.SwitchEffort != "low" || row.Recent != 2 {
			t.Fatalf("after the switch Ember-1's row is %+v; want it ranked second, landing back on its low", row)
		}
	})

	t.Run("over the socket", func(t *testing.T) {
		sess := liveNative(t, t.TempDir())
		h := newAttachHostWith(t, sess, nil, true, control.Options{Workspace: "/work"})
		s := dialHost(t, h, t.TempDir())
		ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
		defer cancel()
		if err := s.Start(ctx); err != nil {
			t.Fatalf("start: %v", err)
		}
		info := s.Info()
		if !info.EffortFacts {
			t.Fatal("over the socket, Info does not say the native session states effort facts")
		}
		wantGLM(t, "the attach's document", info.Models)
	})
}
