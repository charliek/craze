package engine_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/tui"
)

// rescanner is the Stub as a session that keeps a skill scan of its own
// (agent.SkillRescanner, plan 043 P5), counting the scans it is asked for.
type rescanner struct {
	*tui.Stub
	asked atomic.Int32
}

func (r *rescanner) RescanSkills() { r.asked.Add(1) }

var _ agent.SkillRescanner = (*rescanner)(nil)

// TestEveryAttachAsksForASkillScan is P5's attach trigger at the seam: each
// attach — with no cursor, and with one the log honours — asks the session for
// a scan of its disk skills, so a client opening an idle session learns of a
// skill created since the last turn ended. A session with no scan of its own
// attaches as it always did.
func TestEveryAttachAsksForASkillScan(t *testing.T) {
	r := &rescanner{Stub: tui.NewStubNoPrimary()}
	e, err := engine.New(r, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := r.asked.Load(); n != 0 {
		t.Fatalf("%d scans asked for before any attach", n)
	}
	a, err := e.Attach(t.Context(), engine.AttachOptions{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	a.Sub.Close()
	cursor := agent.Cursor{Incarnation: a.Snapshot.Incarnation, Seq: a.Snapshot.Seq}
	b, err := e.Attach(t.Context(), engine.AttachOptions{Cursor: &cursor})
	if err != nil {
		t.Fatalf("Attach with a cursor: %v", err)
	}
	if b.Snapshot != nil {
		t.Fatal("fixture: the cursor was not honoured")
	}
	b.Sub.Close()
	if n := r.asked.Load(); n != 2 {
		t.Fatalf("two attaches asked for %d scans, want one each", n)
	}

	plain, err := engine.New(tui.NewStubNoPrimary(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	if err := plain.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, err := plain.Attach(t.Context(), engine.AttachOptions{})
	if err != nil {
		t.Fatalf("a session with no scan of its own: Attach: %v", err)
	}
	c.Sub.Close()
}
