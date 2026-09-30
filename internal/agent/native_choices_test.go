package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// Which models a native session offers, and in what order (plan 031 §3.6,
// owner decision Q4, P7, P9), from the adapter's side: the snapshot's Models
// are the table's models whose provider has a key — nokey/d's never has one
// in the fixture — plus the running model, the remembered ones first with
// their rank, judged once when the session starts.

// modelRanks is each model's id, "id#rank" for a remembered one.
func modelRanks(ms []ModelInfo) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.Recent > 0 {
			out = append(out, m.ID+"#"+strconv.Itoa(m.Recent))
			continue
		}
		out = append(out, m.ID)
	}
	return out
}

// TestNativeAdvertisesConnectedModelsInMemoryOrder (§3.6, A3): a new session
// offers the remembered models first, newest first, each with its rank —
// nokey/d, remembered but unfunded, is passed over with no gap in the ranks
// and not offered at all — then the rest by name; the model it starts on,
// the newest funded remembered one, is the dialog's first row, and no row
// carries a label (the names are the table's own).
func TestNativeAdvertisesConnectedModelsInMemoryOrder(t *testing.T) {
	f := newNativeFixture(t)
	seedMemory(t, f.dir, "other/c@", "nokey/d@", "test/b@medium") // oldest first
	s := f.started(Options{})
	snap := s.Snapshot()
	want := []ModelInfo{
		{ID: "test/b", Name: "Model B", Recent: 1},
		{ID: "other/c", Name: "other/c", Recent: 2},
		{ID: "test/a", Name: "Model A"},
	}
	if !reflect.DeepEqual(snap.Models, want) {
		t.Fatalf("Models =\n%+v\nwant\n%+v", snap.Models, want)
	}
	if snap.CurrentModel != "test/b" {
		t.Fatalf("started on %s; want test/b, the newest funded remembered model", snap.CurrentModel)
	}
	if got := modelRanks(OrderModels(snap)); !slices.Equal(got, []string{"test/b#1", "other/c#2", "test/a"}) {
		t.Fatalf("the dialog's order is %v", got)
	}
}

// TestNativeOrderIsTheStartsOwn (§3.4, §3.6): the order is judged once, at
// start. A switch made in the session is remembered, but the session's own
// list keeps the ranks it started with — the dialog puts the model it is now
// on first whatever its rank — and the next session is the one that offers
// the new order. A resume orders its picker by the memory too, read when it
// starts, while its model stays the transcript's.
func TestNativeOrderIsTheStartsOwn(t *testing.T) {
	f := newNativeFixture(t)
	ws := t.TempDir()
	seedMemory(t, f.dir, "test/a@high", "other/c@")
	s := f.started(Options{Workspace: ws})
	before := s.Snapshot().Models
	if got := modelRanks(before); !slices.Equal(got, []string{"other/c#1", "test/a#2", "test/b"}) {
		t.Fatalf("at start the list is %v", got)
	}
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if !reflect.DeepEqual(snap.Models, before) {
		t.Fatalf("a switch changed the session's own list:\n%+v\nwas\n%+v", snap.Models, before)
	}
	if got := modelRanks(OrderModels(snap)); !slices.Equal(got, []string{"test/b", "other/c#1", "test/a#2"}) {
		t.Fatalf("after the switch the dialog's order is %v; want the current model first, then the start's ranks", got)
	}
	f.models["test/b"].push(answer("ok"))
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	id := snap.SessionID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	next := f.started(Options{})
	if got := modelRanks(next.Snapshot().Models); !slices.Equal(got, []string{"test/b#1", "other/c#2", "test/a#3"}) {
		t.Fatalf("the next session's list is %v; want the switch first", got)
	}

	// A resume: its model is the transcript's, and its picker the memory's.
	seedMemory(t, f.dir, "test/a@low")
	resumed := f.session(Options{Workspace: ws, LoadSessionID: id})
	if _, err := startLoad(t, resumed); err != nil {
		t.Fatalf("Start: %v", err)
	}
	rsnap := resumed.Snapshot()
	if rsnap.CurrentModel != "test/b" {
		t.Fatalf("resumed on %s; want the transcript's test/b", rsnap.CurrentModel)
	}
	if got := modelRanks(rsnap.Models); !slices.Equal(got, []string{"test/a#1", "test/b#2", "other/c#3"}) {
		t.Fatalf("the resumed session's list is %v; want the memory's order", got)
	}
}

// TestNativeHiddenModels (§3.6): a model whose provider has no key is not
// offered, and a switch to it — a typed /model, a client's set — fails
// exactly as a switch to an alias the table does not have fails, changing
// nothing and remembering nothing. --model at start still resolves against
// the whole table, so it is refused for the key it lacks (noKeyText), not as
// unknown. And a switch to an offered model whose key has gone from the
// environment since is refused by the harness, with the key's variable named,
// and not remembered either.
func TestNativeHiddenModels(t *testing.T) {
	f := newNativeFixture(t)
	var otherGone atomic.Bool
	f.getenv = func(name string) string {
		if name == "NATIVE_OTHER_KEY" && otherGone.Load() {
			return ""
		}
		return f.env[name]
	}
	s := f.started(Options{})
	if slices.ContainsFunc(s.Snapshot().Models, func(m ModelInfo) bool { return m.ID == "nokey/d" }) {
		t.Fatalf("an unfunded model is offered: %+v", s.Snapshot().Models)
	}

	_, unknown := s.SetModel(context.Background(), "", "nope/z")
	_, hidden := s.SetModel(context.Background(), "", "nokey/d")
	if unknown == nil || hidden == nil || hidden.Error() != strings.ReplaceAll(unknown.Error(), "nope/z", "nokey/d") {
		t.Fatalf("a switch to a hidden model = %v; want what an unknown one gives: %v", hidden, unknown)
	}
	if got := s.Snapshot().CurrentModel; got != "test/a" {
		t.Fatalf("a refused switch moved the model to %q", got)
	}

	otherGone.Store(true)
	_, err := s.SetModel(context.Background(), "", "other/c")
	if !errors.Is(err, harness.ErrNoAPIKey) || !strings.Contains(err.Error(), "NATIVE_OTHER_KEY") {
		t.Fatalf("a switch to an offered model whose key has gone = %v; want the harness's no-key refusal", err)
	}
	if got := s.Snapshot().CurrentModel; got != "test/a" {
		t.Fatalf("a refused switch moved the model to %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.dir, modeltable.RecentFile)); !os.IsNotExist(err) {
		t.Fatalf("a refused switch wrote the memory: %v", err)
	}

	start := f.session(Options{Model: "nokey/d"})
	err = start.Start(context.Background())
	if !errors.Is(err, harness.ErrNoAPIKey) || !strings.Contains(err.Error(), `run "craze auth login nokey"`) {
		t.Fatalf("--model of an unfunded model = %v; want the no-key text", err)
	}
}
