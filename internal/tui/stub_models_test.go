package tui

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// TestTheRefreshingStubKeepsTheNativeContract (plan 034 §3.4, C5): the Stub
// as a session that takes up models while it runs answers a refresh as a
// native session does — current with nothing staged, failed when the files
// cannot be read (the list as it was, the staged change kept), pending while
// a turn runs, the staged list taken up as that turn ends: its catalog delta,
// the whole list at revision 1, follows the turn's EventDone, and Snapshot's
// models and revision move together — sameDir by agent.SameNativeDir, and an
// error once closed. Negative control: a Stub that never takes up a pending
// refresh at the turn's end still offers the old list after it.
func TestTheRefreshingStubKeepsTheNativeContract(t *testing.T) {
	ctx := context.Background()
	s := NewStubNoPrimary()
	s.InstallOnStart = false
	s.SetNativeDir("/home/me/.craze/native")
	rs := RefreshingStub{Stub: s}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	sub, err := s.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	refresh := func(dir string) agent.ModelsRefresh {
		t.Helper()
		r, err := rs.RefreshModels(ctx, dir)
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		return r
	}

	if r := refresh(""); r.Status != agent.ModelsCurrent || r.Revision != 0 || r.SameDir != nil {
		t.Fatalf("nothing staged: %+v", r)
	}
	staged := []agent.ModelInfo{{ID: "grok", Name: "Grok"}, {ID: "native/new", Name: "New", Recent: 1}}
	s.StageModels(staged)
	s.FailNextRefresh()
	if r := refresh("/elsewhere"); r.Status != agent.ModelsFailed || r.Revision != 0 || r.SameDir == nil || *r.SameDir {
		t.Fatalf("a failed refresh: %+v", r)
	}

	hung := s.HangNext()
	done := make(chan error, 1)
	go func() {
		_, err := s.Prompt(ctx, "go")
		done <- err
	}()
	select {
	case <-hung:
	case <-time.After(stubEventWait):
		t.Fatal("the hung turn never opened")
	}
	if r := refresh("/home/me/.craze/native/"); r.Status != agent.ModelsPending || r.Revision != 0 || r.SameDir == nil || !*r.SameDir {
		t.Fatalf("a refresh during a turn: %+v; want pending at revision 0, sameDir true", r)
	}
	if got := s.Snapshot(); got.CatalogRevision != 0 || reflect.DeepEqual(got.Models, staged) {
		t.Fatalf("a pending refresh moved the list: %+v at revision %d", got.Models, got.CatalogRevision)
	}
	if _, err := s.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the turn: %v", err)
		}
	case <-time.After(stubEventWait):
		t.Fatal("the turn never ended")
	}

	var types []agent.EventType
	deadline := time.After(stubEventWait)
	for {
		var rec agent.Record
		select {
		case r, ok := <-sub.Records():
			if !ok {
				t.Fatalf("the subscription ended: %v", sub.Err())
			}
			rec = r
		case <-deadline:
			t.Fatalf("no catalog delta after %v", types)
		}
		ev, err := rec.Event()
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, ev.Type)
		if ev.State == nil || ev.State.Catalog == nil {
			continue
		}
		if want := (&agent.CatalogState{Models: staged, Revision: 1}); !reflect.DeepEqual(ev.State.Catalog, want) {
			t.Fatalf("the catalog delta: %+v, want %+v", ev.State.Catalog, want)
		}
		if n := len(types); n < 2 || types[n-2] != agent.EventDone {
			t.Fatalf("the catalog delta does not follow the turn's EventDone: %v", types)
		}
		break
	}
	if got := s.Snapshot(); got.CatalogRevision != 1 || !reflect.DeepEqual(got.Models, staged) {
		t.Fatalf("after the turn: %+v at revision %d", got.Models, got.CatalogRevision)
	}
	if r := refresh(""); r.Status != agent.ModelsCurrent || r.Revision != 1 {
		t.Fatalf("after the turn, nothing staged: %+v", r)
	}
	_ = s.Close()
	if _, err := rs.RefreshModels(ctx, ""); err == nil {
		t.Fatal("a refresh of a closed Stub answered")
	}
}
