package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// Models picked up live (native_models.go, plan 034 §3.4; §7 A16–A24, the
// parts provable without the wire). Every case runs the native adapter on the
// fixture's directory — its table written as files and loaded by Start, so a
// reload reads what a reload in production reads — and changes those files as
// `craze auth`, /connect or a sign-in would.

// noReloads stops s's reloads of its model table, for a case about something
// a reload would also do — the key learning's own stamp rule, say.
func noReloads(s *nativeSession) {
	s.modelsMu.Lock()
	s.models.reloadable = false
	s.modelsMu.Unlock()
}

// offeredIDs are a model list's ids, in order.
func offeredIDs(models []ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// hasCatalog reports whether ev carries a catalog section.
func hasCatalog(ev Event) bool {
	return ev.Type == EventMeta && ev.State != nil && ev.State.Catalog != nil
}

// catalogsOf are the catalog sections evs carry, in order.
func catalogsOf(evs []Event) []*CatalogState {
	var out []*CatalogState
	for _, ev := range evs {
		if hasCatalog(ev) {
			out = append(out, ev.State.Catalog)
		}
	}
	return out
}

// saveTable writes table as the fixture's directory's files.
func saveTable(t *testing.T, dir string, table *modeltable.Table) {
	t.Helper()
	if err := modeltable.Save(dir, table); err != nil {
		t.Fatalf("saving the table: %v", err)
	}
}

// refresh is RefreshModels, failing the test on its error.
func refresh(t *testing.T, s *nativeSession, dir string) ModelsRefresh {
	t.Helper()
	r, err := s.RefreshModels(context.Background(), dir)
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	return r
}

// refreshApplied is refresh, failing the test unless the reload published.
func refreshApplied(t *testing.T, s *nativeSession, dir string) {
	t.Helper()
	if r := refresh(t, s, dir); r.Status != ModelsApplied {
		t.Fatalf("RefreshModels = %+v; want applied", r)
	}
}

// everyOfferedSwitches holds A24's invariant on s: every model its list
// offers is one SetModel switches to. It switches back to where it started.
func everyOfferedSwitches(t *testing.T, s *nativeSession) {
	t.Helper()
	snap := s.Snapshot()
	for _, m := range snap.Models {
		if _, err := s.SetModel(context.Background(), "", m.ID); err != nil {
			t.Fatalf("the list offers %s, and SetModel refuses it: %v", m.ID, err)
		}
	}
	if _, err := s.SetModel(context.Background(), "", snap.CurrentModel); err != nil {
		t.Fatalf("switching back to %s: %v", snap.CurrentModel, err)
	}
}

// TestNativeRefreshListsAKeySavedWhileIdle (A16): a key saved into the
// session's directory while it is idle — what /connect and `craze auth login`
// do — is taken up by RefreshModels: the provider's model is offered, said in
// one catalog delta with the whole list and its revision, and a switch to it
// and a turn on it work. A second call with nothing changed is current;
// nativeDir is compared with the directory the session reads (sameDir). The
// control: before the save the model is not offered and the call is current.
// Negative control: a reload that does not swap the snapshot's list leaves
// nokey/d unoffered.
func TestNativeRefreshListsAKeySavedWhileIdle(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	if got := offeredIDs(s.Snapshot().Models); slices.Contains(got, "nokey/d") {
		t.Fatalf("premise: nokey/d offered before its key (%v)", got)
	}
	if r := refresh(t, s, ""); r.Status != ModelsCurrent || r.Revision != 0 || r.SameDir != nil {
		t.Fatalf("control: with nothing changed RefreshModels = %+v; want current, revision 0", r)
	}
	if evs := deltaSettled(t, s); len(catalogsOf(evs)) != 0 {
		t.Fatal("control: a current refresh published a catalog")
	}

	if err := modeltable.SetKey(f.dir, "nokey", "sk-nokey-saved-0201"); err != nil {
		t.Fatal(err)
	}
	r := refresh(t, s, f.dir)
	if r.Status != ModelsApplied || r.Revision == 0 || r.SameDir == nil || !*r.SameDir {
		t.Fatalf("RefreshModels after the save = %+v; want applied, a revision, the same directory", r)
	}
	snap := s.Snapshot()
	if !slices.Contains(offeredIDs(snap.Models), "nokey/d") || snap.CatalogRevision != r.Revision {
		t.Fatalf("after the refresh the list is %v at revision %d; want nokey/d at %d", offeredIDs(snap.Models), snap.CatalogRevision, r.Revision)
	}
	cats := catalogsOf(deltaSettled(t, s))
	if len(cats) != 1 || cats[0].Revision != r.Revision || !slices.Equal(offeredIDs(cats[0].Models), offeredIDs(snap.Models)) {
		t.Fatalf("the refresh published %+v; want one catalog of the snapshot's list at revision %d", cats, r.Revision)
	}

	if _, err := s.SetModel(context.Background(), "", "nokey/d"); err != nil {
		t.Fatalf("a switch to the newly offered model: %v", err)
	}
	f.models["nokey/d"].push(answer("from d"))
	if _, err := s.Prompt(context.Background(), "hello d"); err != nil {
		t.Fatalf("a turn on the newly offered model: %v", err)
	}
	if got := joined(drained(s), EventText); got != "from d" {
		t.Fatalf("the turn said %q", got)
	}
	if r := refresh(t, s, filepath.Join(t.TempDir(), "elsewhere")); r.Status != ModelsCurrent || r.SameDir == nil || *r.SameDir {
		t.Fatalf("a refresh naming another directory = %+v; want current and not the same directory", r)
	}
}

// TestNativeTurnStartTakesUpAStoredKey (Q14 a): a key stored between two
// turns is taken up at the next turn's start, before the turn says anything:
// the catalog delta is in the stream ahead of the turn's text. Negative
// control: a turn that does not reload at its start leaves nokey/d unoffered
// after it.
func TestNativeTurnStartTakesUpAStoredKey(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	storeInPlace(t, f.dir, "nokey", "sk-nokey-stored-0202")
	f.models["test/a"].push(answer("hi"))
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	evs := drained(s)
	if !slices.Contains(offeredIDs(s.Snapshot().Models), "nokey/d") {
		t.Fatalf("after the turn the list is %v; want nokey/d", offeredIDs(s.Snapshot().Models))
	}
	cat, text := -1, -1
	for i, ev := range evs {
		switch {
		case hasCatalog(ev) && cat < 0:
			cat = i
		case ev.Type == EventText && text < 0:
			text = i
		}
	}
	if cat < 0 || text < 0 || cat > text {
		t.Fatalf("the catalog delta is event %d and the turn's first text %d; want the catalog first", cat, text)
	}
}

// TestNativeRefreshFollowsASignInAndASignOut (A17): a ChatGPT sign-in written
// into the session's directory while it runs — the registration, the model
// list and the token file, as the sign-in leaves them — is taken up by a
// refresh, which offers the plan's model; a sign-out, which removes only the
// token file, is taken up the same way and takes it away. The control: the
// session started with no plan model. Negative control: stamping four inputs
// and not the token file answers the sign-out current and keeps the model.
func TestNativeRefreshFollowsASignInAndASignOut(t *testing.T) {
	noOpenAI(t)
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	dir := filepath.Join(home, "native")
	s := newNative(Options{Workspace: t.TempDir(), ContentHome: t.TempDir()}, func(o *harness.Options) {
		o.Getenv = func(name string) string {
			if name == "FIREWORKS_API_KEY" {
				return "fw-dummy-key-0203"
			}
			return ""
		}
	})
	closeAtCleanup(t, s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	takeStartDelta(t, s.log)
	const plan = "chatgpt/gpt-5.6-sol"
	if got := offeredIDs(s.Snapshot().Models); slices.Contains(got, plan) {
		t.Fatalf("control: the plan's model offered before the sign-in (%v)", got)
	}

	writePlanAccount(t, dir)
	writePlanTokens(t, dir, planAccess, planRefresh, "inc-1", 1)
	refreshApplied(t, s, dir)
	if got := offeredIDs(s.Snapshot().Models); !slices.Contains(got, plan) {
		t.Fatalf("after the sign-in the list is %v; want %s", got, plan)
	}

	if err := os.Remove(chatgptauth.TokenFile(dir)); err != nil {
		t.Fatal(err)
	}
	refreshApplied(t, s, dir)
	if got := offeredIDs(s.Snapshot().Models); slices.Contains(got, plan) {
		t.Fatalf("after the sign-out the list is %v; want no plan model", got)
	}
	if cats := catalogsOf(deltaSettled(t, s)); len(cats) != 2 || cats[0].Revision >= cats[1].Revision {
		t.Fatalf("the two refreshes published %+v; want two catalogs, in revision order", cats)
	}
}

// TestNativeFetchedListIsTakenUp (Q14 c): the background fetch of the plan's
// list, which a session's open starts for a list that is stale, writes a list
// that names a model the open did not have; its completion reloads the table
// through the same transaction, and the session offers the model — a session
// now sees its own fetch. The control: the open did not offer it — read
// while the fixture holds the fetch's answer back, so the fetch cannot have
// been taken up before the test looks (r9 #10). Negative control: a fetch
// that does not reload leaves the list as it opened.
func TestNativeFetchedListIsTakenUp(t *testing.T) {
	api, _ := noOpenAI(t)
	captured := make(chan struct{})
	t.Cleanup(func() { closeOnce(captured) })
	api.queue(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-captured:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-6.1-sol","display_name":"GPT-6.1 Sol","visibility":"list","priority":0,`+
			`"context_window":272000,"input_modalities":["text"],"supported_reasoning_levels":[{"effort":"low"}],"default_reasoning_level":"low"}]}`)
	})
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	dir := filepath.Join(home, "native")
	writePlanAccount(t, dir)
	writePlanTokens(t, dir, planAccess, planRefresh, "inc-1", 1)
	staleModels(t, dir)
	s := newNative(Options{Workspace: t.TempDir(), ContentHome: t.TempDir()}, func(o *harness.Options) {
		o.Getenv = func(string) string { return "" }
	})
	closeAtCleanup(t, s)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	takeStartDelta(t, s.log)
	s.mu.Lock()
	refreshed := s.refreshed
	opened := offeredIDs(s.snap.Models)
	s.mu.Unlock()
	close(captured)
	if refreshed == nil {
		t.Fatal("a stale list started no fetch")
	}
	await(t, refreshed, "the model list's fetch")
	if slices.Contains(opened, "chatgpt/gpt-6.1-sol") {
		t.Fatalf("control: the open already offered the fetched model (%v)", opened)
	}
	if got := offeredIDs(s.Snapshot().Models); !slices.Contains(got, "chatgpt/gpt-6.1-sol") {
		t.Fatalf("after the fetch the list is %v; want the fetched model", got)
	}
}

// TestNativeRefreshDuringATurnIsTakenUpAtItsEnd (A18): a refresh while a turn
// holds the harness is pending — the list is as it was, and the harness kept
// its table — and the turn's end takes it up before the continuation
// returns, its catalog after the turn's EventDone. Negative control: a turn's
// end that does not take up the owed reload leaves nokey/d unoffered once
// the prompt has returned.
func TestNativeRefreshDuringATurnIsTakenUpAtItsEnd(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("working"), finishParts(fantasy.FinishReasonStop)))
	out := startPrompt(s, "go")
	await(t, h.reached, "the held step")
	if err := modeltable.SetKey(f.dir, "nokey", "sk-nokey-midturn-0204"); err != nil {
		t.Fatal(err)
	}
	r := refresh(t, s, "")
	if r.Status != ModelsPending || r.Revision != 0 {
		t.Fatalf("RefreshModels during a turn = %+v; want pending at revision 0", r)
	}
	if got := offeredIDs(s.Snapshot().Models); slices.Contains(got, "nokey/d") {
		t.Fatalf("a pending refresh changed the list mid-turn: %v", got)
	}
	close(h.release)
	if o := await(t, out, "the turn"); o.err != nil {
		t.Fatalf("the turn: %v", o.err)
	}
	if got := offeredIDs(s.Snapshot().Models); !slices.Contains(got, "nokey/d") {
		t.Fatalf("after the turn the list is %v; want nokey/d taken up at its end", got)
	}
	evs := deltaSettled(t, s)
	done, cat := -1, -1
	for i, ev := range evs {
		switch {
		case ev.Type == EventDone:
			done = i
		case hasCatalog(ev):
			cat = i
		}
	}
	if done < 0 || cat < done {
		t.Fatalf("the catalog is event %d and the turn's ending %d; want the catalog after the ending", cat, done)
	}
}

// TestNativeLoadBracketHoldsAReload (A19): a load whose replay is slow, and a
// fetched list landing while it walks and again after the walk, before the
// end bracket — the window the harness no longer refuses a table in — publish
// no catalog inside the bracket: both are pending, and the load's end takes
// the reload up after the end bracket, unstamped. Negative control: a reload
// that does not hold back while the session is loading publishes its catalog
// before replay:end.
func TestNativeLoadBracketHoldsAReload(t *testing.T) {
	f := newNativeFixture(t)
	ws := t.TempDir()
	id := storeNativeSession(t, f, Options{Workspace: ws}, "test/a", []step{answer("one")})
	s := f.session(Options{Workspace: ws, LoadSessionID: id})
	var mid, after ModelsStatus
	var midOnce, afterOnce sync.Once
	s.sinkSeam = func(ev harness.Event) {
		if _, ok := ev.(harness.Prompted); ok {
			midOnce.Do(func() {
				storeInPlace(t, f.dir, "nokey", "sk-nokey-midload-0205")
				mid = s.reloadModels(reloadFetched)
			})
		}
	}
	s.loadReplayedSeam = func() { afterOnce.Do(func() { after = s.reloadModels(reloadFetched) }) }
	evs, err := startLoad(t, s)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if mid != ModelsPending || after != ModelsPending {
		t.Fatalf("the reloads inside the bracket answered %s and %s; want pending", mid, after)
	}
	lines := loadLines(evs)
	end := slices.Index(lines, "replay:end")
	if end < 0 {
		t.Fatalf("no end bracket in %q", lines)
	}
	for i, l := range lines {
		if strings.Contains(l, "catalog=") && i < end {
			t.Fatalf("a catalog inside the bracket: %q", lines)
		}
	}
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "meta catalog=") || strings.HasSuffix(last, "[r]") {
		t.Fatalf("the load ended %q; want the held reload's catalog after the end bracket, unstamped", lines)
	}
	if got := offeredIDs(s.Snapshot().Models); !slices.Contains(got, "nokey/d") {
		t.Fatalf("after the load the list is %v; want nokey/d", got)
	}
}

// TestNativeReloadKeepsTheRunningModel (Q16, A20): the model a session runs
// on stays offered, with its name and its effort option, when a reload's
// table has dropped its alias, and when its key is gone; a turn still runs on
// it. The controls: the dropped alias's sibling on the same provider goes
// with its key. Negative control: a reload that does not carry the running
// model's entry drops test/a from the list.
func TestNativeReloadKeepsTheRunningModel(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	effortOpt := func() *ConfigOption { return EffortOption(s.Snapshot()) }
	before := effortOpt()
	if before == nil {
		t.Fatal("premise: test/a has an effort option")
	}

	gone := nativeTestTable("http://127.0.0.1:9/v1")
	delete(gone.Models, "test/a")
	gone.DefaultModel = "test/b"
	saveTable(t, f.dir, gone)
	refreshApplied(t, s, "")
	snap := s.Snapshot()
	i := slices.IndexFunc(snap.Models, func(m ModelInfo) bool { return m.ID == "test/a" })
	if i < 0 || snap.Models[i].Name != "Model A" || snap.CurrentModel != "test/a" {
		t.Fatalf("with its alias gone the list is %+v on %s; want test/a, named Model A", snap.Models, snap.CurrentModel)
	}
	if after := effortOpt(); after == nil || !configEqual([]ConfigOption{*before}, []ConfigOption{*after}) {
		t.Fatalf("the effort option is %+v; want the running model's %+v", after, before)
	}
	if _, err := s.SetConfig(context.Background(), "", nativeEffortID, "low", ""); err != nil {
		t.Fatalf("an effort the running model offers: %v", err)
	}

	unfunded := nativeTestTable("http://127.0.0.1:9/v1")
	p := unfunded.Providers["test"]
	p.EnvKeys = []string{"NATIVE_GONE_KEY"}
	unfunded.Providers["test"] = p
	saveTable(t, f.dir, unfunded)
	refreshApplied(t, s, "")
	got := offeredIDs(s.Snapshot().Models)
	if !slices.Contains(got, "test/a") || slices.Contains(got, "test/b") {
		t.Fatalf("with its key gone the list is %v; want test/a kept and test/b gone", got)
	}
	f.models["test/a"].push(answer("still here"))
	if _, err := s.Prompt(context.Background(), "go on"); err != nil {
		t.Fatalf("a turn on the running model: %v", err)
	}
}

// TestNativeReloadWithholdsAFrozenKeyProvider (A22): a provider configured
// while the session runs, whose key is inside what the session sends with
// every request — the frozen prompt names the working directory, and the
// provider's variable holds that path — is not offered, with one note naming
// the provider and never the key; a later reload says nothing more. The
// session is not put in its refusal state: a switch and a turn still work.
// Negative control: a reload that does not withhold offers frozen/x, and the
// key it learns refuses the turn.
func TestNativeReloadWithholdsAFrozenKeyProvider(t *testing.T) {
	ws := t.TempDir()
	if err := modeltable.KeyProblem(ws); err != nil {
		t.Fatalf("the workspace path cannot be a key (%v), so the case proves nothing", err)
	}
	f := newNativeFixture(t)
	// Set before Start: no table the session opened with names the variable,
	// so nothing reads it until a reload's table does.
	f.env["NATIVE_FROZEN_KEY"] = ws
	var diag lockedDiag
	s := f.started(Options{Workspace: ws, Diag: &diag})
	table := nativeTestTable("http://127.0.0.1:9/v1")
	table.Providers["frozen"] = modeltable.Provider{Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"NATIVE_FROZEN_KEY"}}
	table.Models["frozen/x"] = modeltable.Model{Provider: "frozen", WireModel: "wire-x"}
	saveTable(t, f.dir, table)
	refreshApplied(t, s, "")
	if got := offeredIDs(s.Snapshot().Models); slices.Contains(got, "frozen/x") {
		t.Fatalf("the list offers %v; want frozen/x withheld", got)
	}
	table.Models["test/b"] = modeltable.Model{Provider: "test", WireModel: "wire-b", Name: "B again", Efforts: []string{"medium"}, DefaultEffort: "medium"}
	saveTable(t, f.dir, table)
	refreshApplied(t, s, "")
	d := diag.String()
	if strings.Count(d, `provider "frozen" is not offered`) != 1 || strings.Contains(d, ws) {
		t.Fatalf("Diag = %q; want one note naming the provider, never the key", d)
	}
	if _, err := s.SetModel(context.Background(), "", "test/b"); err != nil {
		t.Fatalf("a switch after the withheld reload: %v", err)
	}
	f.models["test/b"].push(answer("fine"))
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("a turn after the withheld reload: %v", err)
	}
	everyOfferedSwitches(t, s)
}

// TestNativeSwitchesAndReloadsTakeTurns (A24): a switch's reading of what
// the session offers, its harness switch and its announcement are one
// modelsMu section, so a reload that would drop the model being switched to
// waits for the switch, and then carries it: the switch is accepted for the
// model the list offered, and the list after the reload offers the model the
// session runs on. The same holds for an effort: a reload that changes the
// running model's efforts in its files leaves the option as the running
// model has it, and the effort taken. Every model the list offers after either
// is one SetModel switches to. Negative control: a SetModel or SetConfig that
// does not hold modelsMu across its window lets the probe take it there.
func TestNativeSwitchesAndReloadsTakeTurns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    func(*modeltable.Table)
		switch_ func(s *nativeSession) (SetOutcome, error)
		check   func(t *testing.T, s *nativeSession)
	}{
		{
			name: "SetModel",
			edit: func(tb *modeltable.Table) { delete(tb.Models, "test/b") },
			switch_: func(s *nativeSession) (SetOutcome, error) {
				return s.SetModel(context.Background(), "", "test/b")
			},
			check: func(t *testing.T, s *nativeSession) {
				snap := s.Snapshot()
				if snap.CurrentModel != "test/b" || !slices.Contains(offeredIDs(snap.Models), "test/b") {
					t.Fatalf("after the switch and the reload the session runs on %s and offers %v; want test/b, offered", snap.CurrentModel, offeredIDs(snap.Models))
				}
			},
		},
		{
			name: "SetConfig",
			edit: func(tb *modeltable.Table) {
				m := tb.Models["test/a"]
				m.Efforts, m.DefaultEffort = []string{"max"}, "max"
				tb.Models["test/a"] = m
			},
			switch_: func(s *nativeSession) (SetOutcome, error) {
				return s.SetConfig(context.Background(), "", nativeEffortID, "low", "")
			},
			check: func(t *testing.T, s *nativeSession) {
				opt := EffortOption(s.Snapshot())
				if opt == nil || opt.Current != "low" || len(opt.SelectValues) != 2 {
					t.Fatalf("after the effort and the reload the option is %+v; want the running model's two levels, on low", opt)
				}
				if _, err := s.SetConfig(context.Background(), "", nativeEffortID, "max", ""); err == nil {
					t.Fatal("an effort the option does not offer was taken")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t)
			s := f.started(Options{})
			table := nativeTestTable("http://127.0.0.1:9/v1")
			tc.edit(table)
			saveTable(t, f.dir, table)
			var once sync.Once
			var probed bool
			reloaded := make(chan ModelsRefresh, 1)
			s.mu.Lock()
			s.setModelSeam = func() {
				once.Do(func() {
					if s.modelsMu.TryLock() {
						probed = true
						s.modelsMu.Unlock()
					}
					go func() {
						r, _ := s.RefreshModels(context.Background(), "")
						reloaded <- r
					}()
				})
			}
			s.mu.Unlock()
			if _, err := tc.switch_(s); err != nil {
				t.Fatalf("the switch: %v", err)
			}
			if probed {
				t.Fatal("modelsMu was free inside the switch's window: a reload could land between what the list offered and the switch")
			}
			if r := await(t, reloaded, "the reload"); r.Status != ModelsApplied {
				t.Fatalf("the reload = %+v; want applied", r)
			}
			tc.check(t, s)
			s.mu.Lock()
			s.setModelSeam = nil
			s.mu.Unlock()
			everyOfferedSwitches(t, s)
		})
	}
}

// TestNativeReloadsApplyInOrder (A24): two reloads never publish out of
// order. The first, holding the transaction with a table read from the files
// as they were, keeps the second — started once the files had moved on —
// waiting; the second then reads the newer files and publishes them after,
// at a higher revision, and the harness holds the same table the list was
// built from. Negative control: reloads that do not take modelsMu let the
// probe take it inside the first.
func TestNativeReloadsApplyInOrder(t *testing.T) {
	f := newNativeFixture(t)
	s := f.session(Options{})
	v1 := nativeTestTable("http://127.0.0.1:9/v1")
	v1.Models["test/e"] = modeltable.Model{Provider: "test", WireModel: "wire-e", Name: "Model E"}
	v2 := nativeTestTable("http://127.0.0.1:9/v1")
	v2.Models["test/e"] = v1.Models["test/e"]
	v2.Models["test/f"] = modeltable.Model{Provider: "test", WireModel: "wire-f", Name: "Model F"}
	var once sync.Once
	var probed bool
	second := make(chan ModelsStatus, 1)
	s.reloadSeam = func(stage string) {
		if stage != "computed" {
			return
		}
		once.Do(func() {
			saveTable(t, f.dir, v2)
			go func() { second <- s.reloadModels(reloadAsked) }()
			if s.modelsMu.TryLock() {
				probed = true
				s.modelsMu.Unlock()
			}
		})
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	takeStartDelta(t, s.log)
	saveTable(t, f.dir, v1)
	if got := s.reloadModels(reloadAsked); got != ModelsApplied {
		t.Fatalf("the first reload = %s", got)
	}
	if probed {
		t.Fatal("modelsMu was free inside a reload: a second could publish between its reading and its swap")
	}
	if got := await(t, second, "the second reload"); got != ModelsApplied {
		t.Fatalf("the second reload = %s", got)
	}
	snap := s.Snapshot()
	if got := offeredIDs(snap.Models); !slices.Contains(got, "test/f") {
		t.Fatalf("the list is %v; want the newer files' test/f", got)
	}
	var harnessHas []string
	for _, m := range s.hs.Models() {
		harnessHas = append(harnessHas, m.Alias)
	}
	if !slices.Contains(harnessHas, "test/f") {
		t.Fatalf("the harness holds %v; want the table the list was built from", harnessHas)
	}
	cats := catalogsOf(deltaSettled(t, s))
	if len(cats) != 2 || cats[0].Revision >= cats[1].Revision || cats[1].Revision != snap.CatalogRevision ||
		slices.Contains(offeredIDs(cats[0].Models), "test/f") || !slices.Contains(offeredIDs(cats[1].Models), "test/f") {
		t.Fatalf("the reloads published %+v; want the older list, then the newer at a higher revision", cats)
	}
}

// TestNativeReloadFailureIsOneValueFreeNote: a models.toml that will not load
// fails the refresh, leaves the list as it was, and is journaled once — the
// same failure again is not — naming the file and never a value; no stamp is
// recorded, so the file fixed is taken up at once. Negative control: a failed
// reload that records its stamps answers the fixed file current.
func TestNativeReloadFailureIsOneValueFreeNote(t *testing.T) {
	f := newNativeFixture(t)
	jdir := filepath.Join(t.TempDir(), "journal")
	s := f.started(Options{JournalDir: jdir})
	jw, inc := journalOf(t, s.log), s.Incarnation()
	before := offeredIDs(s.Snapshot().Models)
	mpath := filepath.Join(f.dir, modeltable.ModelsFile)
	good, err := os.ReadFile(mpath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mpath, append(good, []byte("\n[models.\"broken\"\nprovider = \"sk-not-a-key-in-a-note-0206\"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if r := refresh(t, s, ""); r.Status != ModelsFailed || r.Revision != 0 {
			t.Fatalf("refresh %d of a broken file = %+v; want failed", i+1, r)
		}
	}
	if got := offeredIDs(s.Snapshot().Models); !slices.Equal(got, before) {
		t.Fatalf("a failed reload changed the list to %v", got)
	}
	if err := os.WriteFile(mpath, good, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := modeltable.SetKey(f.dir, "nokey", "sk-nokey-fixed-0207"); err != nil {
		t.Fatal(err)
	}
	refreshApplied(t, s, "")
	closeJournaled(t, s, jw)
	lines := assertOneJournal(t, jdir, jw, inc)
	notes := diags(lines, diagModelsReload)
	if len(notes) != 1 || !strings.Contains(fmt.Sprint(notes[0]["error"]), modeltable.ModelsFile) {
		t.Fatalf("the reload's diags = %v; want one, naming the file", notes)
	}
	if l := nativeLeaks(lines, "sk-not-a-key-in-a-note-0206"); len(l) > 0 {
		t.Fatalf("the file's text reached the journal at %v", l)
	}
}

// eventIndex is the index of the first event of evs ok reports, -1 for none.
func eventIndex(evs []Event, ok func(Event) bool) int {
	return slices.IndexFunc(evs, ok)
}

// TestNativeTheUnfundedCarry (A20, A24; plan 034 C4r, r9 #1): the model
// the session runs on, asked for again while the table still resolves it, is
// built again from the table — so a rotated key is taken up: the build the
// switch makes carries it. Kept listed by a reload whose table no longer
// funds it (the carry), it is offered, and asking for it again is a
// successful no-op that keeps its client: nothing is built, which would fail
// for its missing key, and a turn still runs on it. A switch away to a funded
// model takes it off the list in the switch's own delta, at a higher
// revision; every model the list offers then is one SetModel switches to,
// and the carry left behind is not one. The control: the carry was listed
// before the switch away. Negative controls: a reselect that never builds
// leaves the rotated key unused; one that always builds fails the carry with
// its missing key; a switch away that does not drop the unfunded carry
// leaves test/a offered.
func TestNativeTheUnfundedCarry(t *testing.T) {
	const rotated = "sk-test-rotated-0344"
	f := newNativeFixture(t)
	var mu sync.Mutex
	var builds []bool // each build of test/a: whether it carried the rotated key
	f.edit = func(o *harness.Options) {
		build := o.NewModel
		o.NewModel = func(r modeltable.Resolved) (fantasy.LanguageModel, error) {
			if r.Alias == "test/a" {
				mu.Lock()
				builds = append(builds, r.APIKey.Reveal() == rotated)
				mu.Unlock()
			}
			return build(r)
		}
	}
	built := func() []bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(builds)
	}
	s := f.started(Options{})
	if b := built(); len(b) != 1 || b[0] {
		t.Fatalf("premise: Start built test/a %d times (%v); want once, on the key it opened with", len(b), b)
	}

	// Resolved: asked for again, it is built again, and takes up the
	// rotated key.
	f.env["NATIVE_TEST_KEY"] = rotated
	if out, err := s.SetModel(context.Background(), "", "test/a"); err != nil || out.Value != "test/a" {
		t.Fatalf("reselecting the running model = %+v, %v", out, err)
	}
	if b := built(); len(b) != 2 || !b[1] {
		t.Fatalf("after the reselect test/a was built %d times (%v); want once more, on the rotated key", len(b), b)
	}

	// Unresolved, the carry: a no-op that keeps its client.
	unfunded := nativeTestTable("http://127.0.0.1:9/v1")
	p := unfunded.Providers["test"]
	p.EnvKeys = []string{"NATIVE_GONE_KEY"}
	unfunded.Providers["test"] = p
	saveTable(t, f.dir, unfunded)
	refreshApplied(t, s, "")
	snap := s.Snapshot()
	if !slices.Contains(offeredIDs(snap.Models), "test/a") || snap.CurrentModel != "test/a" {
		t.Fatalf("control: the carry is not listed (%v on %s)", offeredIDs(snap.Models), snap.CurrentModel)
	}
	_ = deltaSettled(t, s)

	out, err := s.SetModel(context.Background(), "", "test/a")
	if err != nil || out.Value != "test/a" {
		t.Fatalf("reselecting the unfunded carry = %+v, %v; want a successful no-op", out, err)
	}
	if b := built(); len(b) != 2 {
		t.Fatalf("reselecting the carry built test/a again (%d builds); want its client kept", len(b))
	}
	if snap := s.Snapshot(); !slices.Contains(offeredIDs(snap.Models), "test/a") || snap.CurrentModel != "test/a" {
		t.Fatalf("after reselecting it the list is %v on %s; want test/a, current and listed", offeredIDs(snap.Models), snap.CurrentModel)
	}
	f.models["test/a"].push(answer("still a"))
	if _, err := s.Prompt(context.Background(), "go on"); err != nil {
		t.Fatalf("a turn on the reselected carry: %v", err)
	}
	rev := s.Snapshot().CatalogRevision
	_ = deltaSettled(t, s)

	if _, err := s.SetModel(context.Background(), "", "other/c"); err != nil {
		t.Fatalf("a switch to a funded model: %v", err)
	}
	snap = s.Snapshot()
	if slices.Contains(offeredIDs(snap.Models), "test/a") || snap.CatalogRevision <= rev {
		t.Fatalf("after the switch away the list is %v at revision %d; want test/a gone, past %d", offeredIDs(snap.Models), snap.CatalogRevision, rev)
	}
	evs := deltaSettled(t, s)
	i := eventIndex(evs, func(ev Event) bool { return hasCatalog(ev) && ev.State.Model != nil })
	if i < 0 || *evs[i].State.Model != "other/c" || evs[i].State.Catalog.Revision != snap.CatalogRevision ||
		slices.Contains(offeredIDs(evs[i].State.Catalog.Models), "test/a") {
		t.Fatalf("the switch published %+v; want one delta with the model and the list without test/a", evs)
	}
	everyOfferedSwitches(t, s)
	if _, err := s.SetModel(context.Background(), "", "test/a"); err == nil {
		t.Fatal("a switch back to the carry left behind was taken")
	}
}

// TestNativeRefreshAtATurnsEdgesIsOwed (A18; plan 034 C4r, r9 #4): a refresh
// while a turn holds the session's claim is pending even where the harness
// holds no turn — between the turn's own start reload and the harness's
// turn, and between the harness's turn returning and the turn's ending going
// out — so its catalog is never published inside the turn: it is taken up at
// the turn's end, after EventDone. Negative control: a reload that reads only
// the harness's refusal publishes in either window — applied, its catalog
// ahead of the ending in the second.
func TestNativeRefreshAtATurnsEdgesIsOwed(t *testing.T) {
	for _, stage := range []string{"started", "ran"} {
		t.Run(stage, func(t *testing.T) {
			f := newNativeFixture(t)
			s := f.session(Options{})
			var status ModelsStatus
			var once sync.Once
			s.turnSeam = func(at string) {
				if at != stage {
					return
				}
				once.Do(func() {
					if err := modeltable.SetKey(f.dir, "nokey", "sk-nokey-at-the-edge-0342"); err != nil {
						t.Error(err)
					}
					r, err := s.RefreshModels(context.Background(), "")
					if err != nil {
						t.Error(err)
					}
					status = r.Status
				})
			}
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			takeStartDelta(t, s.log)
			f.models["test/a"].push(answer("hi"))
			if _, err := s.Prompt(context.Background(), "hello"); err != nil {
				t.Fatalf("the turn: %v", err)
			}
			if status != ModelsPending {
				t.Errorf("a refresh at the turn's %s edge = %q; want pending", stage, status)
			}
			if got := offeredIDs(s.Snapshot().Models); !slices.Contains(got, "nokey/d") {
				t.Fatalf("after the turn the list is %v; want nokey/d taken up at its end", got)
			}
			evs := deltaSettled(t, s)
			done, cat := eventIndex(evs, func(ev Event) bool { return ev.Type == EventDone }), eventIndex(evs, hasCatalog)
			if done < 0 || cat < done {
				t.Fatalf("the catalog is event %d and the turn's ending %d; want the catalog after the ending", cat, done)
			}
		})
	}
}

// TestNativeACatalogPublishedUnderAFreshClaimGoesFirst (plan 034 C4r, r9 #4,
// the start's other side): a reload that read the claim free just before a
// prompt took it publishes — the turn's start reload waits for it on modelsMu
// — and its catalog is flushed by that start reload, so it reaches the stream
// ahead of the turn's first word even with the outbox's drainer held until
// something flushes. Negative control: a start reload that flushes only its
// own catalog lets the turn's text overtake it.
func TestNativeACatalogPublishedUnderAFreshClaimGoesFirst(t *testing.T) {
	f := newNativeFixture(t)
	s := f.session(Options{})
	var armed atomic.Bool
	gate := make(chan struct{})
	var opened sync.Once
	open := func() { opened.Do(func() { close(gate) }) }
	t.Cleanup(open)
	s.log.hooks = &logHooks{
		outboxAdmitting: func(int) {
			if armed.Load() {
				<-gate
			}
		},
		flushParked: func(uint64) {
			if armed.Load() {
				open()
			}
		},
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var paused sync.Once
	s.reloadSeam = func(stage string) {
		if stage == "unclaimed" {
			paused.Do(func() { close(reached); <-release })
		}
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	takeStartDelta(t, s.log)
	if err := modeltable.SetKey(f.dir, "nokey", "sk-nokey-fresh-claim-0343"); err != nil {
		t.Fatal(err)
	}
	refreshed := make(chan ModelsRefresh, 1)
	go func() {
		r, _ := s.RefreshModels(context.Background(), "")
		refreshed <- r
	}()
	await(t, reached, "the reload past the claim")
	run := s.Begin("hello") // the claim, taken after the reload read it free
	armed.Store(true)
	close(release)
	if r := await(t, refreshed, "the reload"); r.Status != ModelsApplied {
		t.Fatalf("premise: the reload that read the claim free = %+v; want applied", r)
	}
	f.models["test/a"].push(answer("hi"))
	if _, err := run(context.Background()); err != nil {
		t.Fatalf("the turn: %v", err)
	}
	evs := drained(s)
	cat, text := eventIndex(evs, hasCatalog), eventIndex(evs, func(ev Event) bool { return ev.Type == EventText })
	if cat < 0 || text < 0 || cat > text {
		t.Fatalf("the catalog is event %d and the turn's first text %d; want the catalog first", cat, text)
	}
}

// TestNativeAReloadMidTurnLearnsNoKey (plan 034 C4r, r9 #7a): the reviewer's
// interleaving — a turn on test/a, whose frozen prompt names the working
// directory; mid-turn a switch to other/c for the next turn, and other's key
// stored as that very path; a refresh — is pending, and learns nothing while
// the turn runs: the session neither redacts the path nor refuses, and the
// turn, whose every request has carried the path in its prompt since the
// session opened, ends as it would have had the key not been stored (the
// timing before C4: a key stored mid-turn was learned at the next turn). The
// turn's end takes the reload up after its ending: the key is learned, one
// note says every turn is refused from now on, and the next turn is refused
// before anything is sent. Negative control: a reload that learns its keys
// before it decides it is owed learns the path mid-turn, and the harness's
// step boundary ends the turn with the refusal.
func TestNativeAReloadMidTurnLearnsNoKey(t *testing.T) {
	ws := t.TempDir()
	if err := modeltable.KeyProblem(ws); err != nil {
		t.Fatalf("the workspace path cannot be a key (%v), so the case proves nothing", err)
	}
	f := newNativeFixture(t)
	var diag lockedDiag
	s := f.started(Options{Workspace: ws, Diag: &diag})
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("working"), finishParts(fantasy.FinishReasonStop)))
	out := startPrompt(s, "go")
	await(t, h.reached, "the held step")
	if _, err := s.SetModel(context.Background(), "", "other/c"); err != nil {
		t.Fatalf("a switch for the next turn: %v", err)
	}
	if err := modeltable.SetKey(f.dir, "other", ws); err != nil {
		t.Fatal(err)
	}
	if r := refresh(t, s, ""); r.Status != ModelsPending {
		t.Fatalf("RefreshModels mid-turn = %+v; want pending", r)
	}
	if s.hs.Redact(ws) != ws {
		t.Error("the pending reload taught the session the key mid-turn")
	}
	close(h.release)
	if o := await(t, out, "the turn"); o.err != nil {
		t.Fatalf("the turn = %v; want it to end as it would have", o.err)
	}
	if s.hs.Redact(ws) == ws {
		t.Fatal("the turn's end did not take the reload up: the key is not learned")
	}
	if d := diag.String(); strings.Count(d, "appears in its frozen prompt") != 1 || strings.Contains(d, ws) {
		t.Fatalf("Diag = %q; want one note of the refusal, never the key", d)
	}
	f.models["other/c"].push(answer("never sent"))
	if _, err := s.Prompt(context.Background(), "next"); err == nil || !strings.Contains(err.Error(), "frozen prompt") {
		t.Fatalf("the next turn = %v; want the frozen-key refusal", err)
	}
	if n := f.models["other/c"].callCount(); n != 0 {
		t.Fatalf("other/c was sent %d requests; want none", n)
	}
}

// TestNativeAFetchHeldBackIsReadAtTheTurnsEnd (Q14 c, plan 034 C4r): a
// fetched list's reload reads the files whatever their stamps say — a list
// rewritten within one tick of the file system's clock can leave them so —
// and one a turn held back keeps that when the turn's end takes it up
// (forced): the files are read again and published, though no stamp moved.
// The control: an ordinary refresh with nothing changed is current. Negative
// control: an owed reload that trusts the stamps answers current at the
// turn's end, and nothing is published after the ending.
func TestNativeAFetchHeldBackIsReadAtTheTurnsEnd(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	if r := refresh(t, s, ""); r.Status != ModelsCurrent {
		t.Fatalf("control: with nothing changed RefreshModels = %+v; want current", r)
	}
	h := newHeld(t)
	f.models["test/a"].push(h.step(textParts("working"), finishParts(fantasy.FinishReasonStop)))
	out := startPrompt(s, "go")
	await(t, h.reached, "the held step")
	if got := s.reloadModels(reloadFetched); got != ModelsPending {
		t.Fatalf("a fetched list's reload mid-turn = %s; want pending", got)
	}
	close(h.release)
	if o := await(t, out, "the turn"); o.err != nil {
		t.Fatalf("the turn: %v", o.err)
	}
	evs := deltaSettled(t, s)
	done, cat := eventIndex(evs, func(ev Event) bool { return ev.Type == EventDone }), eventIndex(evs, hasCatalog)
	if done < 0 || cat < done {
		t.Fatalf("the catalog is event %d and the turn's ending %d; want the held fetch's catalog after the ending", cat, done)
	}
}

// TestSameNativeDirComparesWhereTheFilesAre (plan 034 §3.4, A26's sameDir):
// two spellings of one directory are the same — a trailing slash, a "..", a
// relative path from the process's own directory, and a symlink to it, which
// is resolved only once the cleaned paths differ — and another directory, one
// that does not exist, and "" are not. Negative control: a comparison of
// cleaned paths alone calls the symlink another directory.
func TestSameNativeDirComparesWhereTheFilesAre(t *testing.T) {
	root := t.TempDir()
	native := filepath.Join(root, "home", ".craze", "native")
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "home"), link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other", ".craze", "native")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, native)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		a    string
		same bool
	}{
		{native, true},
		{native + "/", true},
		{filepath.Join(native, "..", "native"), true},
		{rel, true},
		{filepath.Join(link, ".craze", "native"), true},
		{other, false},
		{filepath.Join(root, "nowhere", ".craze", "native"), false},
		{"", false},
	} {
		if got := SameNativeDir(tc.a, native); got != tc.same {
			t.Errorf("SameNativeDir(%q, the native dir) = %v, want %v", tc.a, got, tc.same)
		}
	}
	if SameNativeDir(native, "") {
		t.Error("a directory is the same as none")
	}
}
