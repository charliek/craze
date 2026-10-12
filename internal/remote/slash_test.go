package remote_test

import (
	"reflect"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/tui"
)

// withSlash serves a Stub that publishes the slash catalog
// (tui.Stub.PublishSlash; plan 043 C2), its install carrying it.
func withSlash() hostOpt {
	return func(c *hostConfig) {
		c.session = func(s *tui.Stub) agent.Session {
			s.InstallOnStart = true
			s.PublishSlash = true
			return s
		}
	}
}

// TestTheSlashCatalogComesBackOverTheSocket (plan 043 P2, P6): over the
// socket a session that publishes the slash catalog says so — the session
// capability slashCatalog mapped back to backend.SessionInfo.SlashCatalog, as
// aside is to Aside — its attach's restore carries the catalog it installed
// in the snapshot's settings, and a later change reaches the client as a
// delta carrying the section, decoded whole. A session that publishes none —
// the plain Stub, the document an older host sends too — says SlashCatalog
// false and restores no catalog. Negative control: sessionInfo leaving
// SlashCatalog unmapped fails the first check.
func TestTheSlashCatalogComesBackOverTheSocket(t *testing.T) {
	h := newHost(t, withSlash())
	s, restore := started(t, h, newTap(t), remote.SessionOptions{})
	if !s.Info().SlashCatalog || !restore.Info.SlashCatalog {
		t.Fatalf("a session that publishes the catalog: Info %v, the restore's %v", s.Info().SlashCatalog, restore.Info.SlashCatalog)
	}
	want := agent.SlashCatalogFor(h.stub.Snapshot(), nil).Items
	if restore.Snapshot == nil || len(want) == 0 || !reflect.DeepEqual(restore.Snapshot.Settings.Slash, want) {
		t.Fatalf("the restore's catalog is %+v, want the installed %+v", restore.Snapshot, want)
	}

	h.stub.SetSkills([]agent.Skill{{Name: "deploy", Description: "ship it"}})
	got := readUntil(t, s, func(it backend.Item) bool {
		return it.Kind == backend.ItemEvent && it.Event.State != nil && it.Event.State.Slash != nil
	})
	delta := got[len(got)-1].Event.State
	want = agent.SlashCatalogFor(h.stub.Snapshot(), []agent.Skill{{Name: "deploy", Description: "ship it"}}).Items
	if !reflect.DeepEqual(delta.Slash.Items, want) || delta.Commands != nil {
		t.Fatalf("the scan's delta: %+v, want the catalog alone %+v", delta, want)
	}

	plain := newHost(t)
	p, restore := started(t, plain, newTap(t), remote.SessionOptions{})
	if p.Info().SlashCatalog || restore.Info.SlashCatalog || restore.Snapshot.Settings.Slash != nil {
		t.Fatalf("a session that publishes no catalog: Info %v, restore %+v", p.Info().SlashCatalog, restore.Snapshot.Settings.Slash)
	}
}
