package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/modeltable"
)

// Plan 047 D1 and D2 from the adapter's side (§7 A1, A2): a switch lands on
// the effort last used on the new model — this session's memory view first,
// then recent.json read at the switch, then the model's default — never the
// effort the session was on; the memory's writes land in the order their
// switches were confirmed; and every list the session publishes states each
// model's effort facts, published again in the switch's own delta whenever
// a switch or an effort change changes them. The fixture's test/a offers low
// and high (default high), test/b medium and high (default medium), and
// other/c none.

// rowOf is models' row for id.
func rowOf(t *testing.T, models []ModelInfo, id string) ModelInfo {
	t.Helper()
	for _, m := range models {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("%s is not offered: %v", id, offeredIDs(models))
	return ModelInfo{}
}

// TestNativeASwitchLandsOnTheRememberedEffortElseTheDefault (A1, O2): from
// test/a at high, a switch to test/b lands on test/b's own remembered effort,
// else its default — never test/a's high, which test/b offers too, unless
// test/b's memory says high — and a remembered effort test/b no longer
// offers is its default. Negative control: a switch that carries the current
// effort (the harness's SetModel) lands the first case on high.
func TestNativeASwitchLandsOnTheRememberedEffortElseTheDefault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		memory []string
		want   string
	}{
		{"nothing remembered: the default", nil, "medium"},
		{"its memory says high", []string{"test/b@high"}, "high"},
		{"its memory says medium", []string{"test/b@medium"}, "medium"},
		{"its memory is no longer offered", []string{"test/b@turbo"}, "medium"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t)
			s := f.started(Options{Model: "test/a", Effort: "high"})
			seedMemory(t, f.dir, tc.memory...)
			out, err := s.SetModel(context.Background(), "", "test/b")
			if err != nil {
				t.Fatal(err)
			}
			if snap := s.Snapshot(); snap.CurrentModel != "test/b" || effortOf(snap) != tc.want || out.Value != "test/b" {
				t.Fatalf("the switch landed on %s at %q (answered %q); want test/b at %q", snap.CurrentModel, effortOf(snap), out.Value, tc.want)
			}
			if got := memoryOf(t, f.dir); len(got) == 0 || got[0] != "test/b@"+tc.want {
				t.Fatalf("the memory is %v; want test/b@%s newest", got, tc.want)
			}
		})
	}
}

// TestNativeTheSameModelKeepsItsEffort (A1): the model the session is on,
// asked for again, keeps the effort it is at — here --effort's low, which is
// in no memory — whatever recent.json says for it (high). Negative control: a
// harness re-key that takes the landing effort moves it to high.
func TestNativeTheSameModelKeepsItsEffort(t *testing.T) {
	f := newNativeFixture(t)
	seedMemory(t, f.dir, "test/a@high")
	s := f.started(Options{Effort: "low"})
	if snap := s.Snapshot(); snap.CurrentModel != "test/a" || effortOf(snap) != "low" {
		t.Fatalf("control: started on %s at %q; want test/a at --effort's low", snap.CurrentModel, effortOf(snap))
	}
	if _, err := s.SetModel(context.Background(), "", "test/a"); err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); snap.CurrentModel != "test/a" || effortOf(snap) != "low" {
		t.Fatalf("test/a asked for again is at %q; want its own low", effortOf(snap))
	}
}

// TestNativeABackAndForthFindsItsOwnEffort (A1: A → B → A in one session,
// the memory view, no file lag): with recent.json unwritable — every write
// fails, so the file never holds a choice of this session's — test/a set to
// low, a switch to test/b and a switch back land test/a on its low again, and
// test/b on its high after it was raised; each failed write is a note and
// never undoes its switch. Negative control: a switch that does not consult
// the memory view lands test/a on its default, high.
func TestNativeABackAndForthFindsItsOwnEffort(t *testing.T) {
	f := newNativeFixture(t)
	if err := os.Mkdir(filepath.Join(f.dir, modeltable.RecentFile), 0o700); err != nil {
		t.Fatal(err)
	}
	diag := &lockedDiag{}
	s := f.started(Options{Diag: diag})
	steps := []struct {
		model, effort string // an effort is SetConfig; else SetModel
		want          string // model@effort after it
	}{
		{effort: "low", want: "test/a@low"},
		{model: "test/b", want: "test/b@medium"},
		{effort: "high", want: "test/b@high"},
		{model: "test/a", want: "test/a@low"},
		{model: "other/c", want: "other/c@"},
		{model: "test/b", want: "test/b@high"},
	}
	for i, step := range steps {
		var err error
		if step.effort != "" {
			_, err = s.SetConfig(context.Background(), "", nativeEffortID, step.effort, "")
		} else {
			_, err = s.SetModel(context.Background(), "", step.model)
		}
		if err != nil {
			t.Fatalf("step %d: %v; want the switch, whatever the file", i+1, err)
		}
		snap := s.Snapshot()
		if got := snap.CurrentModel + "@" + effortOf(snap); got != step.want {
			t.Fatalf("step %d: the session is on %s; want %s", i+1, got, step.want)
		}
	}
	if n := strings.Count(diag.String(), "not saving the model choice"); n != len(steps) {
		t.Fatalf("%d notes for %d failed writes: %q", n, len(steps), diag.String())
	}
}

// TestNativeAnotherCrazesChoiceIsSeenByATypedSwitch (A1, D1 step 2): a choice
// another craze writes to recent.json after this session started — one this
// session never confirmed — is what a typed /model to that model lands on,
// since the switch reads the file then. Negative control: a switch that
// judges by the session's own reading from its start lands on medium.
func TestNativeAnotherCrazesChoiceIsSeenByATypedSwitch(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	seedMemory(t, f.dir, "test/b@high") // another craze's switch, since
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); effortOf(snap) != "high" {
		t.Fatalf("test/b landed at %q; want the other craze's high", effortOf(snap))
	}
}

// TestNativeMemoryWritesLandInConfirmationOrder (A1, P2): switches confirmed
// in one order whose writes reach the file in the other land in confirmation
// order — the older writes, arriving after the newest, are skipped, so the
// file's newest entry is the session's newest choice. A skipped write never
// reaches the file, and the session's own choice still stands (C1 review F2:
// the effort, never the rank): test/b, set to high before the switch to
// other/c, keeps high in a reload's list and in the memory view, and a
// switch back to it lands on high, which is written then. Negative controls:
// a remember that writes whatever its confirmation's number puts test/b
// newest; a reload that judges by the file alone puts test/b's switch effort
// at its default, medium.
func TestNativeMemoryWritesLandInConfirmationOrder(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	s.modelsMu.Lock()
	first, err1 := s.setModelLocked("", "test/b")
	raised, err2 := s.setConfigLocked("", "high", "")
	second, err3 := s.setModelLocked("", "other/c")
	s.modelsMu.Unlock()
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatalf("the switches: %v, %v, %v", err1, err2, err3)
	}
	if first.seq >= raised.seq || raised.seq >= second.seq {
		t.Fatalf("the confirmations are numbered %d, %d, %d; want them in the order they took", first.seq, raised.seq, second.seq)
	}
	s.remember(second)
	s.remember(raised) // the older writes, after the newer
	s.remember(first)
	if got := memoryOf(t, f.dir); !slices.Equal(got, []string{"other/c@"}) {
		t.Fatalf("the memory is %v; want other/c alone, the newer choice, newest", got)
	}
	if err := modeltable.SetKey(f.dir, "nokey", "sk-nokey-saved-0475"); err != nil {
		t.Fatal(err)
	}
	refreshApplied(t, s, "")
	if b := rowOf(t, s.Snapshot().Models, "test/b"); b.SwitchEffort != "high" {
		t.Fatalf("after a reload the skipped test/b's row says %q; want the session's high", b.SwitchEffort)
	}
	if e, ok := confirmedEffortOf(s, "test/b"); !ok || e != "high" {
		t.Fatalf("after a reload the memory view holds test/b at %q (%v); want high", e, ok)
	}
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); effortOf(snap) != "high" {
		t.Fatalf("the switch back to test/b landed at %q; want the session's high", effortOf(snap))
	}
	if got := memoryOf(t, f.dir); !slices.Equal(got, []string{"test/b@high", "other/c@"}) {
		t.Fatalf("after the switch back the memory is %v", got)
	}
}

// confirmedEffortOf is the session's memory view of alias: the effort it
// last confirmed on it, and whether it confirmed one.
func confirmedEffortOf(s *nativeSession, alias string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return confirmedEffort(s.confirmed, alias)
}

// TestNativeCatalogStatesEffortFacts (A2): a native session says it states
// effort facts (EffortFactser), and every row of its list carries them. After
// an effort change on test/a (to low) and a switch to test/b, the switch's
// one delta carries the model, its effort option and the list ranked again,
// test/a's row saying it would land on low — the next switch's landing — and
// test/b's that it is test/b's medium; an effort change that changes no row
// publishes no list. Negative control: an announce that never publishes the
// list again leaves test/a's switch effort at high.
func TestNativeCatalogStatesEffortFacts(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	if !s.PublishesEffortFacts() {
		t.Fatal("a native session does not say it states effort facts")
	}
	if got := rowOf(t, s.Snapshot().Models, "test/a"); !slices.Equal(got.Efforts, []string{"low", "high"}) ||
		got.DefaultEffort != "high" || got.SwitchEffort != "high" {
		t.Fatalf("at start test/a's row is %+v", got)
	}
	deltaSettled(t, s)
	if _, err := s.SetConfig(context.Background(), "", nativeEffortID, "low", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	evs := ofType(deltaSettled(t, s), EventMeta)
	if len(evs) != 2 {
		t.Fatalf("the effort change and the switch published %d deltas; want 2", len(evs))
	}
	st := evs[1].State
	if st == nil || st.Model == nil || *st.Model != "test/b" || st.Config == nil || st.Catalog == nil {
		t.Fatalf("the switch's delta is %+v; want the model, its option and the list in one", st)
	}
	if a := rowOf(t, st.Catalog.Models, "test/a"); a.SwitchEffort != "low" || a.Recent != 2 {
		t.Fatalf("the switch's list has test/a as %+v; want switchEffort low, ranked second", a)
	}
	if b := rowOf(t, st.Catalog.Models, "test/b"); b.SwitchEffort != "medium" || b.Recent != 1 || !slices.Equal(b.Efforts, []string{"medium", "high"}) {
		t.Fatalf("the switch's list has test/b as %+v", b)
	}
	if snap := s.Snapshot(); snap.CatalogRevision != st.Catalog.Revision || !EqualModels(snap.Models, st.Catalog.Models) {
		t.Fatalf("the snapshot's list (revision %d) is not the delta's (revision %d)", snap.CatalogRevision, st.Catalog.Revision)
	}
	// test/b at medium again changes no row: its delta has no list.
	if _, err := s.SetConfig(context.Background(), "", nativeEffortID, "medium", ""); err != nil {
		t.Fatal(err)
	}
	if cats := catalogsOf(deltaSettled(t, s)); len(cats) != 0 {
		t.Fatalf("an effort change that changed no row published %+v", cats)
	}
}

// TestNativeAReloadKeepsTheMemoryView (A2; C1 review F2, a reload after a
// failed write): a reload's list judges every row's switch effort with the
// session's memory view, so a choice the file never held — its write failed
// — is still the row's: test/a set to low, a switch to test/b, then a
// refresh that takes up a key saved for nokey/d publishes test/a at
// switchEffort low, the memory view still holds it, and a switch back lands
// on it. (Its rank is the file's, which never got it: the accepted lag.)
// Negative control: a reload that judges by the file alone puts test/a's
// switch effort back at high.
func TestNativeAReloadKeepsTheMemoryView(t *testing.T) {
	f := newNativeFixture(t)
	if err := os.Mkdir(filepath.Join(f.dir, modeltable.RecentFile), 0o700); err != nil {
		t.Fatal(err)
	}
	s := f.started(Options{Diag: &lockedDiag{}})
	if _, err := s.SetConfig(context.Background(), "", nativeEffortID, "low", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	deltaSettled(t, s)
	if err := modeltable.SetKey(f.dir, "nokey", "sk-nokey-saved-0471"); err != nil {
		t.Fatal(err)
	}
	refreshApplied(t, s, "")
	cats := catalogsOf(deltaSettled(t, s))
	if len(cats) != 1 || !offers(cats[0].Models, "nokey/d") {
		t.Fatalf("control: the refresh published %+v; want one list offering nokey/d", cats)
	}
	if a := rowOf(t, cats[0].Models, "test/a"); a.SwitchEffort != "low" {
		t.Fatalf("the reload's list has test/a at switchEffort %q; want the memory view's low", a.SwitchEffort)
	}
	if e, ok := confirmedEffortOf(s, "test/a"); !ok || e != "low" {
		t.Fatalf("after the reload the memory view holds test/a at %q (%v); want low", e, ok)
	}
	if _, err := s.SetModel(context.Background(), "", "test/a"); err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); effortOf(snap) != "low" {
		t.Fatalf("the switch back to test/a landed at %q; want the session's low", effortOf(snap))
	}
}

// TestNativeSnapshotModelsAreTheCallersOwn (A2, P5): a snapshot's models, a
// delta's list and their levels are the holder's own: changing a row's
// levels in either reaches neither the session's next snapshot nor its
// list. Negative control: a Snapshot that copies the list shallowly hands out
// the session's own levels, and the second snapshot shows the change.
func TestNativeSnapshotModelsAreTheCallersOwn(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	snap := s.Snapshot()
	i := slices.IndexFunc(snap.Models, func(m ModelInfo) bool { return m.ID == "test/a" })
	snap.Models[i].Efforts[0] = "changed"
	if got := rowOf(t, s.Snapshot().Models, "test/a").Efforts; !slices.Equal(got, []string{"low", "high"}) {
		t.Fatalf("a snapshot's levels reached the session: test/a now offers %v", got)
	}
	deltaSettled(t, s)
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	cats := catalogsOf(deltaSettled(t, s))
	if len(cats) != 1 {
		t.Fatalf("the switch published %d lists; want 1", len(cats))
	}
	j := slices.IndexFunc(cats[0].Models, func(m ModelInfo) bool { return m.ID == "test/a" })
	cats[0].Models[j].Efforts[1] = "changed"
	if got := rowOf(t, s.Snapshot().Models, "test/a").Efforts; !slices.Equal(got, []string{"low", "high"}) {
		t.Fatalf("a delta's levels reached the session: test/a now offers %v", got)
	}
}

// TestCloneModelsOwnsEveryLevel (P5): CloneModels is a new list whose every
// row's levels are its own, and nil for an empty list.
func TestCloneModelsOwnsEveryLevel(t *testing.T) {
	in := []ModelInfo{{ID: "a", Efforts: []string{"low", "high"}}, {ID: "b"}}
	out := CloneModels(in)
	out[0].Efforts[0] = "changed"
	out[1].ID = "changed"
	if in[0].Efforts[0] != "low" || in[1].ID != "b" {
		t.Fatalf("a clone's change reached the original: %+v", in)
	}
	if CloneModels([]ModelInfo{}) != nil || CloneModels(nil) != nil {
		t.Fatal("an empty list's clone is not nil")
	}
	m := CloneModelInfo(in[0])
	m.Efforts[1] = "changed"
	if in[0].Efforts[1] != "high" {
		t.Fatal("CloneModelInfo shares the levels")
	}
}

// renamedTable is the fixture's table with test/a renamed test/c — the same
// provider and wire model, so the same model under another alias, as a
// release renames one — and test/b the default.
func renamedTable() *modeltable.Table {
	table := nativeTestTable("http://127.0.0.1:9/v1")
	table.Models["test/c"] = table.Models["test/a"]
	delete(table.Models, "test/a")
	table.DefaultModel = "test/b"
	return table
}

// TestNativeAReloadKeepsAConfirmationAcrossARename (C1 review F1a): with
// recent.json unwritable — so the file holds none of the session's choices —
// test/a set to low and a switch to test/b, then a release that renames
// test/a test/c (its provider and wire model unchanged) taken up by a
// refresh: the session's confirmation follows the model to its new alias, as
// recent.json's own entries do (modeltable.Table.RecentAlias). test/c's row
// says low, the memory view holds it under test/c and no longer under
// test/a, and a switch to it lands on low. (Its rank is the file's, which
// never got it: the accepted lag.) Negative control: a reload that keeps the
// confirmations under their old aliases puts test/c at its default, high.
func TestNativeAReloadKeepsAConfirmationAcrossARename(t *testing.T) {
	f := newNativeFixture(t)
	if err := os.Mkdir(filepath.Join(f.dir, modeltable.RecentFile), 0o700); err != nil {
		t.Fatal(err)
	}
	s := f.started(Options{Diag: &lockedDiag{}})
	if _, err := s.SetConfig(context.Background(), "", nativeEffortID, "low", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	f.models["test/c"] = &scriptedModel{provider: "test", wire: "wire-a"}
	saveTable(t, f.dir, renamedTable())
	refreshApplied(t, s, "")
	snap := s.Snapshot()
	if offers(snap.Models, "test/a") {
		t.Fatalf("control: the renamed alias is still offered: %v", offeredIDs(snap.Models))
	}
	if c := rowOf(t, snap.Models, "test/c"); c.SwitchEffort != "low" {
		t.Fatalf("after the rename test/c is %+v; want switchEffort low", c)
	}
	if e, ok := confirmedEffortOf(s, "test/c"); !ok || e != "low" {
		t.Fatalf("after the rename the memory view holds test/c at %q (%v); want low, under its new alias", e, ok)
	}
	if _, ok := confirmedEffortOf(s, "test/a"); ok {
		t.Fatal("after the rename the memory view still names test/a")
	}
	if _, err := s.SetModel(context.Background(), "", "test/c"); err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); effortOf(snap) != "low" {
		t.Fatalf("a switch to test/c landed at %q; want the session's low, confirmed on it as test/a", effortOf(snap))
	}
}

// TestNativeAReloadDropsTheConfirmationOfARepointedAlias (C1 review F1b):
// test/a set to low, a switch to test/b — both written — then test/a
// re-pointed at another wire model, one that offers low and defaults to
// high, taken up by a refresh: that alias names another model now, so
// neither the file's entry for it (Table.Recent rejects it) nor the session's
// confirmation carries over. test/a's row says high, unranked, and a switch
// to it lands on high. Negative control: a reload that keeps the
// confirmations as they were lands test/a on the old model's low.
func TestNativeAReloadDropsTheConfirmationOfARepointedAlias(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	if _, err := s.SetConfig(context.Background(), "", nativeEffortID, "low", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	if got := memoryOf(t, f.dir); !slices.Equal(got, []string{"test/b@medium", "test/a@low"}) {
		t.Fatalf("control: the memory is %v; want both choices written", got)
	}
	table := nativeTestTable("http://127.0.0.1:9/v1")
	m := table.Models["test/a"]
	m.WireModel = "wire-a2"
	table.Models["test/a"] = m
	saveTable(t, f.dir, table)
	refreshApplied(t, s, "")
	if a := rowOf(t, s.Snapshot().Models, "test/a"); a.SwitchEffort != "high" || a.Recent != 0 {
		t.Fatalf("after the re-pointing test/a is %+v; want its new model's default, high, unranked", a)
	}
	if _, err := s.SetModel(context.Background(), "", "test/a"); err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); effortOf(snap) != "high" {
		t.Fatalf("a switch to the re-pointed test/a landed at %q; want its default, high", effortOf(snap))
	}
}

// TestNativeALeftCarrysReloadKeepsTheSwitchsRank (C1 review F2): a session
// whose running test/a became a shadowing carry — a refresh took up a
// models.toml that changed its efforts — switches to test/b. The switch's
// own delta ranks test/b first, and the reload leaving the carry takes
// (reloadLeft) runs in the switch's own section, before the switch's write:
// handed the switch's own entry, its list — the files' own, test/a at the
// files' max — still ranks test/b first. Negative control: a reloadLeft
// handed no entry reads a file without the switch and publishes test/b
// unranked.
func TestNativeALeftCarrysReloadKeepsTheSwitchsRank(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	table := nativeTestTable("http://127.0.0.1:9/v1")
	m := table.Models["test/a"]
	m.Efforts, m.DefaultEffort = []string{"max"}, "max"
	table.Models["test/a"] = m
	saveTable(t, f.dir, table)
	refreshApplied(t, s, "")
	deltaSettled(t, s)
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	cats := catalogsOf(deltaSettled(t, s))
	if len(cats) != 2 {
		t.Fatalf("control: the switch published %d lists; want its own and the reload's", len(cats))
	}
	last := cats[1]
	if !slices.Equal(rowOf(t, last.Models, "test/a").Efforts, []string{"max"}) {
		t.Fatalf("control: the last list is not the reload's (test/a offers %v)", rowOf(t, last.Models, "test/a").Efforts)
	}
	if b := rowOf(t, last.Models, "test/b"); b.Recent != 1 {
		t.Fatalf("the reload's list ranks test/b %d; want 1, the switch it followed", b.Recent)
	}
	if snap := s.Snapshot(); snap.CatalogRevision != last.Revision || rowOf(t, snap.Models, "test/b").Recent != 1 {
		t.Fatalf("the session's list is at revision %d (test/b ranked %d); want the reload's %d, test/b first",
			snap.CatalogRevision, rowOf(t, snap.Models, "test/b").Recent, last.Revision)
	}
}

// TestNativeAReloadDuringAPendingWriteKeepsTheEffort (C1 review F2): test/b,
// switched to and written at medium, then raised to high — a confirmation
// whose write is held after it is published and before recent.json is
// written (memWriteSeam) — and a reload meanwhile, a refresh taking up a key
// saved for nokey/d: the reload reads a file that still says medium, never
// waits on the write, and its list says high for test/b, the memory view
// holds high, and once the write lands a switch away and back lands on
// high. (Its rank is the file's: the accepted lag.) Negative control: a
// reload that judges by the file alone says medium.
func TestNativeAReloadDuringAPendingWriteKeepsTheEffort(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	s.memWriteSeam = func() {
		close(entered)
		<-release
	}
	raised := make(chan error, 1)
	go func() {
		_, err := s.SetConfig(context.Background(), "", nativeEffortID, "high", "")
		raised <- err
	}()
	<-entered
	if got := memoryOf(t, f.dir); !slices.Equal(got, []string{"test/b@medium"}) {
		t.Fatalf("control: the file holds %v before the held write; want test/b@medium", got)
	}
	if err := modeltable.SetKey(f.dir, "nokey", "sk-nokey-saved-0472"); err != nil {
		t.Fatal(err)
	}
	refreshApplied(t, s, "")
	if b := rowOf(t, s.Snapshot().Models, "test/b"); b.SwitchEffort != "high" || !offers(s.Snapshot().Models, "nokey/d") {
		t.Fatalf("the reload during the held write says test/b at %q (nokey/d offered %v); want high", b.SwitchEffort, offers(s.Snapshot().Models, "nokey/d"))
	}
	if e, ok := confirmedEffortOf(s, "test/b"); !ok || e != "high" {
		t.Fatalf("after the reload the memory view holds test/b at %q (%v); want high", e, ok)
	}
	close(release)
	if err := <-raised; err != nil {
		t.Fatal(err)
	}
	s.memWriteSeam = nil
	for _, model := range []string{"test/a", "test/b"} {
		if _, err := s.SetModel(context.Background(), "", model); err != nil {
			t.Fatal(err)
		}
	}
	if snap := s.Snapshot(); effortOf(snap) != "high" {
		t.Fatalf("a switch away and back landed test/b at %q; want high", effortOf(snap))
	}
}

// TestNativeAReloadLeavesALandedChoiceToTheFile (C1 review F2): a reload
// ranks by the file as it is now, so another craze's choice newer than the
// session's own ranks above it. Here test/b's write lands, another craze
// then picks test/a, and a refresh ranks test/a first and test/b second.
// Negative control: a reload that ranks by the session's own view of the
// memory (s.recent) ranks test/b first.
func TestNativeAReloadLeavesALandedChoiceToTheFile(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	seedMemory(t, f.dir, "test/a@low") // another craze, after test/b's write
	if err := modeltable.SetKey(f.dir, "nokey", "sk-nokey-saved-0473"); err != nil {
		t.Fatal(err)
	}
	refreshApplied(t, s, "")
	snap := s.Snapshot()
	if a, b := rowOf(t, snap.Models, "test/a"), rowOf(t, snap.Models, "test/b"); a.Recent != 1 || b.Recent != 2 {
		t.Fatalf("the reload ranks test/a %d and test/b %d; want the file's 1 and 2", a.Recent, b.Recent)
	}
}

// TestRemapConfirmedFollowsTheFilesRule (C1 review F1): remapConfirmed keeps
// a confirmation under its alias while that alias names its model, follows a
// renamed model to its new alias, drops one whose alias names another model
// or whose model is gone, and keeps the newer of two that come to name one
// alias — modeltable.Table.RecentAlias's rule, the file's. Negative control:
// a remap with no same-alias check keeps the older test/c too.
func TestRemapConfirmedFollowsTheFilesRule(t *testing.T) {
	table := renamedTable()
	entry := func(alias, wire, effort string) modeltable.RecentEntry {
		return modeltable.RecentEntry{Alias: alias, Provider: "test", WireModel: wire, Effort: effort}
	}
	confs := []confirmed{
		{entry: entry("test/b", "wire-b", "high"), seq: 5},
		{entry: entry("test/a", "wire-a", "low"), seq: 4},    // renamed: test/c
		{entry: entry("test/c", "wire-a", "high"), seq: 3},   // the same model, older
		{entry: entry("test/d", "wire-gone", "low"), seq: 2}, // gone
	}
	got := remapConfirmed(table, confs)
	want := []confirmed{{entry: entry("test/b", "wire-b", "high"), seq: 5}, {entry: entry("test/c", "wire-a", "low"), seq: 4}}
	if !slices.Equal(got, want) {
		t.Fatalf("remapConfirmed = %+v, want %+v", got, want)
	}
	if confs[1].entry.Alias != "test/a" {
		t.Fatal("remapConfirmed wrote to the confirmations it was handed")
	}
}

// TestNativeAFailedWriteBeforeALandedOneKeepsItsEffort (C1 review F2): a
// switch to test/b and a raise to high whose writes fail (recent.json is a
// directory then), and a switch to other/c whose write lands (it is a file
// again) — that write read a file without test/b, so the file holds other/c
// alone. A reload's list still says high for test/b, the memory view holds
// it, and a switch to test/b lands on high. (Its rank is the file's until
// the next switch: the accepted lag; each failed write has its note.)
// Negative controls: a reload that judges by the file alone says medium; one
// that installs no confirmations lands the switch on medium.
func TestNativeAFailedWriteBeforeALandedOneKeepsItsEffort(t *testing.T) {
	f := newNativeFixture(t)
	recent := filepath.Join(f.dir, modeltable.RecentFile)
	if err := os.Mkdir(recent, 0o700); err != nil {
		t.Fatal(err)
	}
	diag := &lockedDiag{}
	s := f.started(Options{Diag: diag})
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetConfig(context.Background(), "", nativeEffortID, "high", ""); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(diag.String(), "not saving the model choice"); n != 2 {
		t.Fatalf("control: %d failed writes noted; want test/b's two: %q", n, diag.String())
	}
	if err := os.Remove(recent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetModel(context.Background(), "", "other/c"); err != nil {
		t.Fatal(err)
	}
	if got := memoryOf(t, f.dir); !slices.Equal(got, []string{"other/c@"}) {
		t.Fatalf("control: the file holds %v; want other/c alone", got)
	}
	if err := modeltable.SetKey(f.dir, "nokey", "sk-nokey-saved-0474"); err != nil {
		t.Fatal(err)
	}
	refreshApplied(t, s, "")
	if b := rowOf(t, s.Snapshot().Models, "test/b"); b.SwitchEffort != "high" {
		t.Fatalf("the reload's list says test/b at %q; want the session's high", b.SwitchEffort)
	}
	if e, ok := confirmedEffortOf(s, "test/b"); !ok || e != "high" {
		t.Fatalf("after the reload the memory view holds test/b at %q (%v); want high", e, ok)
	}
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); effortOf(snap) != "high" {
		t.Fatalf("the switch to test/b landed at %q; want the session's high", effortOf(snap))
	}
}

// maxOnlyCarry is a session whose running test/a is a shadowing carry: it
// started on test/a (low, high), and a refresh took up a models.toml whose
// test/a offers max alone, which the running model's carried entry stands
// over.
func maxOnlyCarry(t *testing.T) (*nativeFixture, *nativeSession) {
	t.Helper()
	f := newNativeFixture(t)
	s := f.started(Options{})
	table := nativeTestTable("http://127.0.0.1:9/v1")
	m := table.Models["test/a"]
	m.Efforts, m.DefaultEffort = []string{"max"}, "max"
	table.Models["test/a"] = m
	saveTable(t, f.dir, table)
	refreshApplied(t, s, "")
	if got := EffortOption(s.Snapshot()); got == nil || len(got.SelectValues) != 2 {
		t.Fatalf("control: the carry's option is %+v; want low and high", got)
	}
	return f, s
}

// TestNativeASwitchBackDuringTheFirstsWriteTakesTheFilesEntry (C1 final
// review 1): a switch away from a shadowing carry (test/a at low and high,
// over the files' max-only entry) whose write is held, and a direct caller's
// switch back to test/a meanwhile. The switch away's reload (reloadLeft) ran
// in its own modelsMu section, before its write, so the switch back is
// judged against the files' entry: test/a lands on max, its option offers
// max alone, and it still does once both writes have landed. Negative
// control: a reloadLeft run after the switch's write — outside its section —
// lets the switch back build test/a from the carry, which the reload then
// carries again: test/a keeps low and high.
func TestNativeASwitchBackDuringTheFirstsWriteTakesTheFilesEntry(t *testing.T) {
	f, s := maxOnlyCarry(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.memWriteSeam = func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	away, back := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := s.SetModel(context.Background(), "", "test/b")
		away <- err
	}()
	<-entered // the switch away is confirmed; its write is held
	go func() {
		_, err := s.SetModel(context.Background(), "", "test/a")
		back <- err
	}()
	// The switch back confirms under modelsMu and then waits on memMu for
	// its own write, behind the held one: wait for its confirmation.
	deadline := time.Now().Add(10 * time.Second)
	for s.Snapshot().CurrentModel != "test/a" {
		if time.Now().After(deadline) {
			t.Fatal("the switch back never took")
		}
		time.Sleep(time.Millisecond)
	}
	values := func() []string {
		var out []string
		if o := EffortOption(s.Snapshot()); o != nil {
			for _, v := range o.SelectValues {
				out = append(out, v.Value)
			}
		}
		return out
	}
	if got, at := values(), effortOf(s.Snapshot()); !slices.Equal(got, []string{"max"}) || at != "max" {
		t.Fatalf("the switch back, during the held write, is test/a at %q offering %v; want the files' entry, max alone", at, got)
	}
	close(release)
	for _, ch := range []chan error{away, back} {
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
	}
	if got, at := values(), effortOf(s.Snapshot()); !slices.Equal(got, []string{"max"}) || at != "max" {
		t.Fatalf("after both writes test/a is at %q offering %v; want max alone", at, got)
	}
	if got := memoryOf(t, f.dir); !slices.Equal(got, []string{"test/a@max", "test/b@medium"}) {
		t.Fatalf("the memory is %v; want both switches, in order", got)
	}
}
