package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestNativeInstallsCarryTheSlashCatalog is D2 and P3 for native: no scan —
// it is not a SkillRescanner, its skills are plugin rows — and both of its
// installs, a new session's and a load's, restate the catalog built from what
// they installed: /connect after /model, the modes, /compact, the plugin rows.
// The snapshot hands out a copy of its own (P6).
func TestNativeInstallsCarryTheSlashCatalog(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".claude", "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".claude", "commands", "ship.md"),
		[]byte("---\ndescription: ship it\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newNativeFixture(t)
	s := f.session(Options{Workspace: ws})
	if _, ok := Session(s).(SkillRescanner); ok {
		t.Fatal("the native session scans skills of its own")
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	install := takeStartDelta(t, s.log)
	snap := s.Snapshot()
	want := SlashCatalogFor(snap, nil)
	if st := install.State; st.Slash == nil || !slices.Equal(st.Slash.Items, want.Items) || !slices.Equal(snap.Slash, want.Items) {
		t.Fatalf("the install delta's catalog %+v, the snapshot's %+v, want %+v", install.State.Slash, snap.Slash, want.Items)
	}
	if got := strings.Join(slashNames(want), ","); got != "help,model,connect,clear,tasks,theme,rename,plan,ask,agent,btw,exit,compact,ship" {
		t.Fatalf("native's catalog is %s", got)
	}
	snap.Slash[0].Description = "changed"
	if s.Snapshot().Slash[0].Description == "changed" {
		t.Fatal("the snapshot shares its catalog with the session")
	}

	f.models["test/a"].push(answer("hi"))
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	id := s.Snapshot().SessionID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	loaded := f.session(Options{Workspace: ws, LoadSessionID: id})
	evs, err := startLoad(t, loaded)
	if err != nil {
		t.Fatalf("the load: %v", err)
	}
	var delta *StateDelta
	for _, ev := range evs {
		if ev.Type == EventMeta && ev.Replayed && ev.State != nil && ev.State.Commands != nil {
			delta = ev.State
		}
	}
	if delta == nil || delta.Slash == nil || !slices.Equal(delta.Slash.Items, want.Items) {
		t.Fatalf("the load's install delta carries the catalog %+v, want %+v", delta, want.Items)
	}
}
