package tui

import (
	"context"
	"reflect"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

// TestTheEngineBackendSaysWhetherTheSessionPublishesSlash (plan 043 P2): in
// process, Info().SlashCatalog is the engine's PublishesSlashCatalog — what
// the info document says over the socket — so a plain Stub, which publishes no
// catalog, says false and a PublishSlash Stub says true. Negative control: an
// Info that leaves SlashCatalog unset fails the second case.
func TestTheEngineBackendSaysWhetherTheSessionPublishesSlash(t *testing.T) {
	for _, tc := range []struct {
		name    string
		publish bool
	}{{"a plain Stub", false}, {"a PublishSlash Stub", true}} {
		t.Run(tc.name, func(t *testing.T) {
			stub := NewStub()
			stub.PublishSlash = tc.publish
			eng, err := engine.New(stub, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = eng.Close() })
			if err := eng.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := newEngineBackend(eng, "").Info().SlashCatalog; got != tc.publish {
				t.Fatalf("Info().SlashCatalog is %v, want %v", got, tc.publish)
			}
		})
	}
}

// TestTheMirrorCarriesTheFoldsSlashCatalog (plan 043 P6): the TUI's mirror
// reads the slash catalog from its own fold, like every settings section —
// m.snap.Slash is the last slash section folded, a copy the delta it came in
// cannot reach, and an empty section clears it. (The menu does not read it
// yet; plan 043 C3 projects it.) Negative control: a recompute that leaves
// Slash out of the snapshot it builds fails the first check.
func TestTheMirrorCarriesTheFoldsSlashCatalog(t *testing.T) {
	m := sized(t)
	items := agent.SlashCatalogFor(stubOf(t, m).Snapshot(), []agent.Skill{{Name: "deploy", Description: "ship it"}}).Items
	delta := &agent.SlashCatalog{Items: append([]agent.SlashItem(nil), items...)}
	m = feed(t, m, agent.Event{Type: agent.EventMeta, Seq: 101, State: &agent.StateDelta{Slash: delta}})
	if !reflect.DeepEqual(m.snap.Slash, items) {
		t.Fatalf("the mirror's catalog is %+v, want the folded %+v", m.snap.Slash, items)
	}
	delta.Items[0].Name = "written after the fold"
	m.recompute()
	if !reflect.DeepEqual(m.snap.Slash, items) {
		t.Fatalf("the delta reached the mirror's catalog: %+v", m.snap.Slash)
	}
	m = feed(t, m, agent.Event{Type: agent.EventMeta, Seq: 102, State: &agent.StateDelta{Slash: &agent.SlashCatalog{}}})
	if m.snap.Slash != nil {
		t.Fatalf("an empty section left the mirror's catalog %+v", m.snap.Slash)
	}
}
