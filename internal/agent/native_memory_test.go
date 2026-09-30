package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// The model memory from the adapter's side (plan 031 §3.4, §3.5, §7 A1): a
// switch made in a session is remembered in recent.json of the Home the
// session opened with, and a new session with no --model starts on the newest
// remembered model that is funded, at its remembered effort. --model starts on
// its model at the remembered effort and writes nothing; a resume keeps its
// transcript's model and effort. Every file lives in the fixture's own
// CRAZE_HOME (never the real one).

// memoryOf is the aliases and efforts dir's memory holds, newest first, each
// "alias@effort".
func memoryOf(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, e := range modeltable.ReadRecent(dir) {
		out = append(out, e.Alias+"@"+e.Effort)
	}
	return out
}

// seedMemory writes entries into dir's memory, oldest first, each named by
// its identity in the fixture's table.
func seedMemory(t *testing.T, dir string, entries ...string) {
	t.Helper()
	table := nativeTestTable("http://127.0.0.1:9/v1")
	for _, ae := range entries {
		alias, effort, _ := strings.Cut(ae, "@")
		m := table.Models[alias]
		if err := modeltable.Remember(dir, modeltable.RecentEntry{Alias: alias, Provider: m.Provider, WireModel: m.WireModel, Effort: effort}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

// effortOf is the snapshot's current effort, "" for a model with none.
func effortOf(snap Snapshot) string {
	if o := EffortOption(snap); o != nil {
		return o.Current
	}
	return ""
}

// TestNativeSwitchesAreRemembered (A1): each model switch and each effort
// change a session makes is remembered, newest first, one entry per model,
// with the model's identity in the session's table and the effort the harness
// confirmed after the change — a model switch carries the effort over when the
// new model has it (test/a's high on test/b) and lands on the new model's
// default otherwise (none on other/c, then medium on test/b); and the next
// session with no --model starts on the newest of them at its effort.
func TestNativeSwitchesAreRemembered(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	for _, step := range []struct {
		model, effort string // an effort is SetConfig on the current model; else SetModel
		want          []string
	}{
		{model: "Model A", want: []string{"test/a@high"}}, // the current one, by name
		{model: "test/b", want: []string{"test/b@high", "test/a@high"}},
		{model: "other/c", want: []string{"other/c@", "test/b@high", "test/a@high"}},
		{model: "test/b", want: []string{"test/b@medium", "other/c@", "test/a@high"}},
		{effort: "high", want: []string{"test/b@high", "other/c@", "test/a@high"}},
	} {
		var err error
		if step.effort != "" {
			_, err = s.SetConfig(context.Background(), "", nativeEffortID, step.effort, "")
		} else {
			_, err = s.SetModel(context.Background(), "", step.model)
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := memoryOf(t, f.dir); !slices.Equal(got, step.want) {
			t.Fatalf("the memory is %v, want %v", got, step.want)
		}
	}
	got := modeltable.ReadRecent(f.dir)[0]
	if got.Provider != "test" || got.WireModel != "wire-b" {
		t.Fatalf("the newest entry names %s/%s; want test's wire-b, from the session's table", got.Provider, got.WireModel)
	}
	// A switch that fails is not remembered: here to a model the session does
	// not offer (plan 031 §3.6); TestNativeHiddenModels has one the harness
	// refuses.
	if _, err := s.SetModel(context.Background(), "", "nokey/d"); err == nil {
		t.Fatal("a switch to an unfunded model succeeded")
	}
	if _, err := s.SetConfig(context.Background(), "", nativeEffortID, "max", ""); err == nil {
		t.Fatal("an effort test/b does not offer was taken")
	}
	if got := memoryOf(t, f.dir); len(got) != 3 || got[0] != "test/b@high" {
		t.Fatalf("a refused switch changed the memory: %v", got)
	}

	next := f.started(Options{})
	if snap := next.Snapshot(); snap.CurrentModel != "test/b" || effortOf(snap) != "high" {
		t.Fatalf("the next session starts on %s at %q; want the remembered test/b at high", snap.CurrentModel, effortOf(snap))
	}
	f.models["test/b"].push(answer("ok"))
	if _, err := next.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got := memoryOf(t, f.dir); len(got) != 3 || got[0] != "test/b@high" {
		t.Fatalf("starting and prompting changed the memory: %v", got)
	}
}

// TestNativeStartSkipsWhatItCannotUse: the newest remembered model whose
// provider has no key, and one the table no longer has, are passed over for
// the next remembered one; a remembered effort the model does not offer is
// its default; and starting on a remembered model while the default is
// unfunded is the owner's own choice, so it is not noted as a fall-back.
func TestNativeStartSkipsWhatItCannotUse(t *testing.T) {
	f := newNativeFixture(t)
	seedMemory(t, f.dir, "test/b@turbo", "other/c@", "nokey/d@")
	if err := modeltable.Remember(f.dir, modeltable.RecentEntry{Alias: "gone/e", Provider: "gone", WireModel: "wire-e"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	delete(f.env, "NATIVE_TEST_KEY") // test/a, the default, and test/b are unfunded
	var diag bytes.Buffer
	s := f.started(Options{Diag: &diag})
	if snap := s.Snapshot(); snap.CurrentModel != "other/c" {
		t.Fatalf("started on %s; want other/c, the newest funded remembered model", snap.CurrentModel)
	}
	if strings.Contains(diag.String(), "has no API key") {
		t.Fatalf("a start on a remembered model was noted as a fall-back: %q", diag.String())
	}

	f.env["NATIVE_TEST_KEY"] = nativeCanary
	seedMemory(t, f.dir, "test/b@turbo")
	if snap := f.started(Options{}).Snapshot(); snap.CurrentModel != "test/b" || effortOf(snap) != "medium" {
		t.Fatalf("started on %s at %q; want test/b at its default medium (turbo is not offered)", snap.CurrentModel, effortOf(snap))
	}
}

// TestNativeModelFlagTakesTheEffortWritesNothing (P6, A1): --model picks the
// model and the memory still picks its effort, when the model offers it; the
// start and its turn leave recent.json exactly as they found it — and a start
// with no memory creates none.
func TestNativeModelFlagTakesTheEffortWritesNothing(t *testing.T) {
	f := newNativeFixture(t)
	if s := f.started(Options{Model: "test/b"}); effortOf(s.Snapshot()) != "medium" {
		t.Fatalf("--model with no memory is at %q; want test/b's default, medium", effortOf(s.Snapshot()))
	}
	if _, err := os.Stat(filepath.Join(f.dir, modeltable.RecentFile)); !os.IsNotExist(err) {
		t.Fatalf("a --model start with no memory created recent.json: %v", err)
	}

	seedMemory(t, f.dir, "test/b@high", "test/a@low")
	path := filepath.Join(f.dir, modeltable.RecentFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := f.started(Options{Model: "test/b"})
	if snap := s.Snapshot(); snap.CurrentModel != "test/b" || effortOf(snap) != "high" {
		t.Fatalf("--model test/b started on %s at %q; want test/b at its remembered high", snap.CurrentModel, effortOf(snap))
	}
	f.models["test/b"].push(answer("ok"))
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
		t.Fatalf("a --model session changed recent.json:\n%s\nwas\n%s", after, before)
	}
}

// TestNativeResumeKeepsItsTranscriptsModelAndEffort (A1, r2-8): a load
// resumes on the transcript's model and effort whatever the memory says; an
// explicit --model on a load still overrides the model, and the effort then is
// the transcript's carried over, never the remembered one; and neither load
// writes the memory.
func TestNativeResumeKeepsItsTranscriptsModelAndEffort(t *testing.T) {
	f := newNativeFixture(t)
	ws := t.TempDir()
	// The stored session is test/b at high: --model test/b, at the effort
	// the memory held for it then (P6).
	seedMemory(t, f.dir, "test/b@high")
	id := storeNativeSession(t, f, Options{Workspace: ws, Model: "test/b"}, "test/b", []step{answer("one")})
	// Now the memory says test/b at medium, and test/a at low, newest.
	seedMemory(t, f.dir, "test/b@medium", "test/a@low")
	path := filepath.Join(f.dir, modeltable.RecentFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	s := f.session(Options{Workspace: ws, LoadSessionID: id})
	if _, err := startLoad(t, s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if snap := s.Snapshot(); snap.CurrentModel != "test/b" || effortOf(snap) != "high" {
		t.Fatalf("resumed on %s at %q; want the transcript's test/b at high", snap.CurrentModel, effortOf(snap))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// test/a offers high too, so the transcript's effort carries over to
	// the explicit model — not the remembered low.
	s = f.session(Options{Workspace: ws, LoadSessionID: id, Model: "test/a"})
	if _, err := startLoad(t, s); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if snap := s.Snapshot(); snap.CurrentModel != "test/a" || effortOf(snap) != "high" {
		t.Fatalf("resumed with --model test/a on %s at %q; want test/a at the transcript's high, not the remembered low", snap.CurrentModel, effortOf(snap))
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
		t.Fatalf("a load changed recent.json:\n%s", after)
	}
}

// TestNativeLoadThatOpensEmptyStartsOnTheMemory (§3.5, astra 9): a load whose
// session has no transcript opens a new session, so it starts where a new one
// would — the newest remembered model at its effort, or an explicit model at
// its remembered effort.
func TestNativeLoadThatOpensEmptyStartsOnTheMemory(t *testing.T) {
	const id = "0b8f3c1e-5d2a-4c7e-9f10-2a3b4c5d6e7f"
	f := newNativeFixture(t)
	seedMemory(t, f.dir, "test/a@low", "test/b@high")
	for _, tc := range []struct {
		model, want, effort string
	}{
		{"", "test/b", "high"},
		{"test/a", "test/a", "low"},
	} {
		s := f.session(Options{LoadSessionID: id, Model: tc.model})
		if _, err := startLoad(t, s); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if snap := s.Snapshot(); snap.SessionID != id || snap.CurrentModel != tc.want || effortOf(snap) != tc.effort {
			t.Fatalf("the empty open (--model %q) is %s on %s at %q; want %s on %s at %q",
				tc.model, snap.SessionID, snap.CurrentModel, effortOf(snap), id, tc.want, tc.effort)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNativeMemoryLivesInTheHomeItOpened (panel astra 12, A1): the memory is
// read from and written to the Home the harness was opened with — here a
// directory of the seam's own — and never paths.NativeDir(), not even when
// CRAZE_HOME moves while the session runs: the memory under paths.NativeDir()
// is not what the session starts on, and nothing there changes.
func TestNativeMemoryLivesInTheHomeItOpened(t *testing.T) {
	f := newNativeFixture(t)
	own := filepath.Join(t.TempDir(), "native")
	if err := modeltable.Save(own, nativeTestTable("http://127.0.0.1:9/v1")); err != nil {
		t.Fatal(err)
	}
	seedMemory(t, f.dir, "test/b@high") // paths.NativeDir()'s, which the session must not read
	f.edit = func(o *harness.Options) { o.Home = own }
	before := treeState(t, f.dir)
	s := f.started(Options{})
	if got := s.Snapshot().CurrentModel; got != "test/a" {
		t.Fatalf("started on %s; want the default test/a — the other home's memory is not this session's", got)
	}
	t.Setenv("CRAZE_HOME", t.TempDir()) // paths.NativeDir() now answers elsewhere again
	if _, err := s.SetModel(context.Background(), "", "other/c"); err != nil {
		t.Fatal(err)
	}
	if got := memoryOf(t, own); !slices.Equal(got, []string{"other/c@"}) {
		t.Fatalf("the session's own home remembers %v; want the switch", got)
	}
	if after := treeState(t, f.dir); after != before {
		t.Fatalf("the session wrote under the first paths.NativeDir():\n%v\nwas\n%v", after, before)
	}
	if got, _ := filepath.Glob(filepath.Join(os.Getenv("CRAZE_HOME"), "*")); len(got) != 0 {
		t.Fatalf("the session wrote under the later paths.NativeDir(): %v", got)
	}
}

// TestNativeMemoryFailureIsANote: a memory that cannot be written — here a
// recent.json that is a directory — costs one note per switch naming the
// file, and never the switch itself: the model and the effort change, are
// announced, and the setter answers with them.
func TestNativeMemoryFailureIsANote(t *testing.T) {
	f := newNativeFixture(t)
	if err := os.Mkdir(filepath.Join(f.dir, modeltable.RecentFile), 0o700); err != nil {
		t.Fatal(err)
	}
	diag := &lockedDiag{}
	s := f.started(Options{Diag: diag})
	out, err := s.SetModel(context.Background(), "", "test/b")
	if err != nil || out.Value != "test/b" || out.Ticket == nil {
		t.Fatalf("SetModel with an unwritable memory = %+v, %v; want the switch", out, err)
	}
	if n := strings.Count(diag.String(), "native: not saving the model choice: "); n != 1 {
		t.Fatalf("the diagnostics say %q; want one note", diag.String())
	}
	if !strings.Contains(diag.String(), filepath.Join(f.dir, modeltable.RecentFile)) {
		t.Fatalf("the note %q does not name the file", diag.String())
	}
	out, err = s.SetConfig(context.Background(), "", nativeEffortID, "high", "")
	if err != nil || out.Value != "high" {
		t.Fatalf("SetConfig with an unwritable memory = %+v, %v; want the change", out, err)
	}
	if snap := s.Snapshot(); snap.CurrentModel != "test/b" || effortOf(snap) != "high" {
		t.Fatalf("after the switches the session is on %s at %q", snap.CurrentModel, effortOf(snap))
	}
	if n := strings.Count(diag.String(), "not saving the model choice"); n != 2 {
		t.Fatalf("the diagnostics say %q; want one note per switch", diag.String())
	}
	if leaks := nativeLeaks(diag.String(), nativeCanary); len(leaks) > 0 {
		t.Fatalf("a key leaked into the notes at %v", leaks)
	}
}

// TestNativeTwoSessionsSwitchingAtOnce (A1): two sessions on one craze
// directory, switching at the same moment, each keep their choice.
func TestNativeTwoSessionsSwitchingAtOnce(t *testing.T) {
	f := newNativeFixture(t)
	a, b := f.started(Options{}), f.started(Options{})
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, sw := range []struct {
		s     *nativeSession
		model string
	}{{a, "test/b"}, {b, "other/c"}} {
		wg.Go(func() {
			<-start
			_, err := sw.s.SetModel(context.Background(), "", sw.model)
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got := memoryOf(t, f.dir)
	slices.Sort(got)
	if want := []string{"other/c@", "test/b@high"}; !slices.Equal(got, want) {
		t.Fatalf("after two sessions switched at once the memory is %v; want both %v", got, want)
	}
}
