package modeltable

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
)

// The model memory (plan 031 §3.4, §3.5, §7 A1): recent.json's reading and
// writing, and the start rules every reader of it goes through. No test here
// waits on a clock: the concurrent writers are ordered by the lock itself,
// seen through recentIO's seams.

// recentTable is a table for the start rules: two funded providers and one
// with no key, models with and without effort control, and two aliases
// sharing one identity (the second sorts after the first).
func recentTable() *Table {
	return &Table{
		NoCatalog:    true,
		DefaultModel: "p/default",
		Providers: map[string]Provider{
			"p":  {Driver: DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"RECENT_P_KEY"}},
			"q":  {Driver: DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"RECENT_Q_KEY"}},
			"nk": {Driver: DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"RECENT_NK_KEY"}},
		},
		Models: map[string]Model{
			"p/default": {Provider: "p", WireModel: "w-default", Efforts: []string{"low", "high"}, DefaultEffort: "low"},
			"p/effort":  {Provider: "p", WireModel: "w-effort", Efforts: []string{"low", "medium", "xhigh"}, DefaultEffort: "medium"},
			"q/plain":   {Provider: "q", WireModel: "w-plain"},
			"q/twin-a":  {Provider: "q", WireModel: "w-twin"},
			"q/twin-b":  {Provider: "q", WireModel: "w-twin"},
			"nk/dry":    {Provider: "nk", WireModel: "w-dry", Efforts: []string{"high"}, DefaultEffort: "high"},
		},
	}
}

// fundedEnv is a getenv that funds exactly the named providers of
// recentTable, each with a dummy key of MinKeyLen bytes or more.
func fundedEnv(providers ...string) func(string) string {
	env := map[string]string{}
	known := recentTable().Providers
	for _, p := range providers {
		env[known[p].EnvKeys[0]] = "sk-recent-dummy-" + p + "-0001"
	}
	return fakeEnv(env)
}

// entry is a recent.json entry for recentTable's alias, carrying its identity.
func entry(t *testing.T, alias, effort string) RecentEntry {
	t.Helper()
	m, ok := recentTable().Models[alias]
	if !ok {
		t.Fatalf("recentTable has no %s", alias)
	}
	return RecentEntry{Alias: alias, Provider: m.Provider, WireModel: m.WireModel, Effort: effort}
}

// aliases is the Alias of each entry, in order.
func aliases(entries []RecentEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Alias)
	}
	return out
}

// testIO is recentIO with the real lock and a bound no correct run waits out.
func testIO() recentIO {
	return recentIO{lock: atomicfile.LockWithin, wait: 10 * time.Second}
}

// TestRememberWritesTheFile: the first Remember creates the directory 0700
// (before the lock is opened in it) and recent.json 0600, in exactly the shape
// §3.4 pins — version 1, one entry with the alias as "model", its identity,
// its effort ("" kept, for a model with no effort control) and the time in UTC
// to the second — and ReadRecent reads back what was written.
func TestRememberWritesTheFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh", "native")
	at := time.Date(2026, 9, 30, 12, 12, 0, 987654321, time.FixedZone("CEST", 2*60*60))
	if err := Remember(dir, entry(t, "q/plain", ""), at.Add(-time.Hour)); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if err := Remember(dir, entry(t, "p/effort", "xhigh"), at); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, RecentFile): 0o600} {
		if got := mode(t, path); got != want {
			t.Errorf("%s has mode %o, want %o", path, got, want)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, RecentFile))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("recent.json is not JSON: %v\n%s", err, b)
	}
	want := map[string]any{
		"version": float64(1),
		"recent": []any{
			map[string]any{"model": "p/effort", "provider": "p", "wire_model": "w-effort", "effort": "xhigh", "at": "2026-09-30T10:12:00Z"},
			map[string]any{"model": "q/plain", "provider": "q", "wire_model": "w-plain", "effort": "", "at": "2026-09-30T09:12:00Z"},
		},
	}
	if !reflect.DeepEqual(doc, want) {
		t.Fatalf("recent.json = %s\nwant %v", b, want)
	}
	got := ReadRecent(dir)
	if !slices.Equal(aliases(got), []string{"p/effort", "q/plain"}) || got[0].Effort != "xhigh" ||
		!got[0].At.Equal(time.Date(2026, 9, 30, 10, 12, 0, 0, time.UTC)) {
		t.Fatalf("ReadRecent = %+v", got)
	}
}

// TestRememberOrderDedupeAndCap: the newest choice is first; choosing a model
// again moves it to the front with its new effort rather than adding a second
// entry; and the list keeps RecentCap entries, dropping the oldest.
func TestRememberOrderDedupeAndCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	var want []string
	for i := range RecentCap + 2 {
		alias := fmt.Sprintf("m%02d", i)
		if err := Remember(dir, RecentEntry{Alias: alias, Provider: "p", WireModel: alias, Effort: "low"}, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		want = append([]string{alias}, want...)
	}
	got := ReadRecent(dir)
	if !slices.Equal(aliases(got), want[:RecentCap]) {
		t.Fatalf("after %d choices the memory is %v, want the newest %d: %v", RecentCap+2, aliases(got), RecentCap, want[:RecentCap])
	}
	// m05 again, at another effort: to the front, once.
	if err := Remember(dir, RecentEntry{Alias: "m05", Provider: "p", WireModel: "m05", Effort: "high"}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got = ReadRecent(dir)
	wantAgain := append([]string{"m05"}, slices.DeleteFunc(slices.Clone(want[:RecentCap]), func(a string) bool { return a == "m05" })...)
	if !slices.Equal(aliases(got), wantAgain) || got[0].Effort != "high" {
		t.Fatalf("after choosing m05 again the memory is %+v, want %v with m05 at high", got, wantAgain)
	}
}

// TestRememberTwoWritersLoseNeither (A1): a writer that finds another inside
// its read-modify-write waits for it, and then reads what it wrote — so two
// sessions switching at once both keep their choice. The schedule is pinned
// without a clock: A reads and holds inside the locked section (afterRead);
// B is then started, and A is let go only once B has found the lock busy.
// Were Remember's section not under the lock, B would read the file before A
// had written it and enter the section while A held it — which the test sees
// (bIn) before it would ever see B find the lock busy — and B's write would
// have dropped A's choice.
func TestRememberTwoWritersLoseNeither(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	aIn, releaseA := make(chan struct{}), make(chan struct{})
	bBusy, bIn := make(chan struct{}), make(chan struct{})
	var busyOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseA) }) }
	t.Cleanup(release) // a failed run must not leave A parked
	errs := make(chan error, 2)

	aIO := testIO()
	aIO.afterRead = func() {
		close(aIn)
		<-releaseA
	}
	go func() { errs <- remember(dir, entry(t, "p/effort", "xhigh"), now, aIO) }()
	<-aIn

	bIO := testIO()
	// B's lock tries once on its own first, so that a busy lock is seen and
	// said before B waits for it the ordinary way.
	bIO.lock = func(path string, d time.Duration) (func(), error) {
		unlock, err := atomicfile.LockWithin(path, 0)
		if errors.Is(err, atomicfile.ErrLockBusy) {
			busyOnce.Do(func() { close(bBusy) })
			return atomicfile.LockWithin(path, d)
		}
		return unlock, err
	}
	bIO.afterRead = func() { close(bIn) }
	go func() { errs <- remember(dir, entry(t, "q/plain", ""), now.Add(time.Minute), bIO) }()

	select {
	case <-bBusy:
	case <-bIn:
		t.Fatal("the second writer entered the read-modify-write while the first held it")
	}
	release()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Remember: %v", err)
		}
	}
	if got := aliases(ReadRecent(dir)); !slices.Equal(got, []string{"q/plain", "p/effort"}) {
		t.Fatalf("after two concurrent writers the memory is %v; want both, the later first", got)
	}
}

// TestRememberManyWritersAllSurvive: writers started together through the
// real Remember, released by one channel, each keep their choice — the same
// guarantee as above, through the production lock and bound.
func TestRememberManyWritersAllSurvive(t *testing.T) {
	dir := t.TempDir()
	const n = RecentCap
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			<-start
			alias := fmt.Sprintf("w%02d", i)
			errs <- Remember(dir, RecentEntry{Alias: alias, Provider: "p", WireModel: alias}, time.Now())
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Remember: %v", err)
		}
	}
	got := aliases(ReadRecent(dir))
	slices.Sort(got)
	var want []string
	for i := range n {
		want = append(want, fmt.Sprintf("w%02d", i))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("after %d concurrent writers the memory holds %v; want every one", n, got)
	}
}

// TestRememberLockTimeout: a lock another writer holds for the whole bound is
// ErrLockBusy, naming the file, with nothing written — the caller's one note,
// never a failed switch. The bound is the seam's zero (one try); production's
// is RecentLockWait.
func TestRememberLockTimeout(t *testing.T) {
	dir := t.TempDir()
	if err := Remember(dir, entry(t, "q/plain", ""), time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, RecentFile))
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := atomicfile.Lock(filepath.Join(dir, recentLockName))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	io := testIO()
	io.wait = 0
	err = remember(dir, entry(t, "p/effort", "low"), time.Now(), io)
	if !errors.Is(err, atomicfile.ErrLockBusy) || !strings.Contains(err.Error(), filepath.Join(dir, RecentFile)) {
		t.Fatalf("Remember under a held lock = %v; want ErrLockBusy naming the file", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, RecentFile)); !bytes.Equal(after, before) {
		t.Fatalf("a Remember that could not lock changed the file:\n%s", after)
	}
	if RecentLockWait != 2*time.Second {
		t.Fatalf("RecentLockWait = %v; the plan's bound is 2s", RecentLockWait)
	}
}

// TestReadRecentNeverFails: every file that cannot be the memory reads as no
// memory, and a file that can is read entry by entry — one that does not
// decode, or names no model, is skipped, and a model named twice counts once,
// at its newer place.
func TestReadRecentNeverFails(t *testing.T) {
	good := `{"model": "q/plain", "provider": "q", "wire_model": "w-plain", "effort": "", "at": "2026-09-30T10:12:00Z"}`
	for _, tc := range []struct {
		name string
		file func(t *testing.T, path string)
		want []string
	}{
		{name: "missing", file: func(*testing.T, string) {}},
		{name: "no directory", file: nil},
		{name: "unreadable (a directory)", file: func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "not JSON", file: writeRecent(`{"version": 1, "recent": [`)},
		{name: "not an object", file: writeRecent(`[1, 2]`)},
		{name: "no version", file: writeRecent(`{"recent": [` + good + `]}`)},
		{name: "version 0", file: writeRecent(`{"version": 0, "recent": [` + good + `]}`)},
		{name: "a newer version", file: writeRecent(`{"version": 2, "recent": [` + good + `]}`)},
		{name: "a version that is not a number", file: writeRecent(`{"version": "1", "recent": [` + good + `]}`)},
		{name: "entries skipped and deduplicated", file: writeRecent(`{"version": 1, "future": true, "recent": [
			{"model": "p/effort", "provider": "p", "wire_model": "w-effort", "effort": "low", "at": "not a time"},
			{"model": "", "provider": "p", "wire_model": "w-effort"},
			"just a string",
			` + good + `,
			{"model": "p/default", "provider": "p", "wire_model": "w-default", "extra": 1},
			{"model": "q/plain", "provider": "q", "wire_model": "w-plain", "effort": "old"}
		]}`), want: []string{"q/plain", "p/default"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.file == nil {
				dir = filepath.Join(dir, "nowhere")
			} else {
				tc.file(t, filepath.Join(dir, RecentFile))
			}
			got := ReadRecent(dir)
			if !slices.Equal(aliases(got), tc.want) {
				t.Fatalf("ReadRecent = %+v; want %v", got, tc.want)
			}
			if len(got) > 0 && got[0].Effort != "" {
				t.Fatalf("the first mention's effort was not kept: %+v", got[0])
			}
		})
	}
	if got := ReadRecent(""); got != nil {
		t.Fatalf(`ReadRecent("") = %v`, got)
	}
}

// writeRecent writes body as the recent.json at path.
func writeRecent(body string) func(t *testing.T, path string) {
	return func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRememberOverAFileItCannotRead: a file that is not the memory at all is
// replaced — there is nothing in it any craze reads — while one a newer craze
// wrote (ErrRecentNewer), and one that exists but cannot be read, are left
// exactly as they are and the write refused, since replacing either would
// lose what this craze could not see.
func TestRememberOverAFileItCannotRead(t *testing.T) {
	t.Run("corrupt is replaced", func(t *testing.T) {
		dir := t.TempDir()
		writeRecent(`{"version": 1, "recent": [`)(t, filepath.Join(dir, RecentFile))
		if err := Remember(dir, entry(t, "q/plain", ""), time.Now()); err != nil {
			t.Fatalf("Remember over a corrupt file: %v", err)
		}
		if got := aliases(ReadRecent(dir)); !slices.Equal(got, []string{"q/plain"}) {
			t.Fatalf("the replaced memory is %v", got)
		}
	})
	t.Run("newer is kept", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, RecentFile)
		body := `{"version": 2, "recent": [], "favourites": ["x"]}`
		writeRecent(body)(t, path)
		err := Remember(dir, entry(t, "q/plain", ""), time.Now())
		if !errors.Is(err, ErrRecentNewer) || !strings.Contains(err.Error(), path) {
			t.Fatalf("Remember over a newer file = %v; want ErrRecentNewer naming it", err)
		}
		if b, _ := os.ReadFile(path); string(b) != body {
			t.Fatalf("the newer file was changed to %s", b)
		}
	})
	t.Run("unreadable is kept", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, RecentFile)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := Remember(dir, entry(t, "q/plain", ""), time.Now()); err == nil {
			t.Fatal("Remember over an unreadable recent.json succeeded")
		}
		if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
			t.Fatalf("the unreadable recent.json was replaced: %v, %v", fi, err)
		}
	})
	t.Run("no directory or alias", func(t *testing.T) {
		if err := Remember("", entry(t, "q/plain", ""), time.Now()); err == nil {
			t.Fatal(`Remember("") succeeded`)
		}
		dir := t.TempDir()
		if err := Remember(dir, RecentEntry{Alias: " "}, time.Now()); err == nil {
			t.Fatal("Remember of a blank alias succeeded")
		}
		if _, err := os.Stat(filepath.Join(dir, RecentFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused Remember wrote recent.json: %v", err)
		}
	})
}

// TestTableRecentMatchesByAliasThenIdentity: an entry is its alias when that
// alias is still the model it named; else the model with its identity, under
// the first alias that has it (a rename keeps the memory); else nothing — an
// alias re-pointed at another model is not the model that was picked, and an
// entry nothing matches is skipped, not an error. Two entries that land on one
// alias count once, at the newer place. The effort travels as remembered.
func TestTableRecentMatchesByAliasThenIdentity(t *testing.T) {
	table := recentTable()
	got := table.Recent([]RecentEntry{
		{Alias: "p/effort", Provider: "p", WireModel: "w-effort", Effort: "xhigh"},       // itself
		{Alias: "p/effort-old", Provider: "q", WireModel: "w-plain", Effort: "low"},      // renamed: q/plain now
		{Alias: "p/default", Provider: "p", WireModel: "w-retired", Effort: "high"},      // re-pointed: skipped
		{Alias: "gone/model", Provider: "gone", WireModel: "w-gone"},                     // nothing: skipped
		{Alias: "q/twin-b", Provider: "q", WireModel: "w-twin"},                          // itself, though a twin sorts first
		{Alias: "q/twin-old", Provider: "q", WireModel: "w-twin"},                        // identity: twin-a, first sorted
		{Alias: "q/plain", Provider: "q", WireModel: "w-plain", Effort: "ignored-later"}, // q/plain again: counted once
		{Alias: "nk/dry", Provider: "nk", WireModel: "w-dry", Effort: "high"},            // unfunded is still a model
	})
	want := []RecentEntry{
		{Alias: "p/effort", Provider: "p", WireModel: "w-effort", Effort: "xhigh"},
		{Alias: "q/plain", Provider: "q", WireModel: "w-plain", Effort: "low"},
		{Alias: "q/twin-b", Provider: "q", WireModel: "w-twin"},
		{Alias: "q/twin-a", Provider: "q", WireModel: "w-twin"},
		{Alias: "nk/dry", Provider: "nk", WireModel: "w-dry", Effort: "high"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Recent =\n%+v\nwant\n%+v", got, want)
	}
	if got := table.Recent(nil); got != nil {
		t.Fatalf("Recent(nil) = %v", got)
	}
}

// TestStartModel (§3.5): the newest remembered model that resolves, at its
// remembered effort while the model offers it; else the user's pin; else the
// first funded alias (recentTable has no provider order); else
// ErrNothingFunded. Every remembered entry that is unfunded, unknown or
// re-pointed is passed over; a renamed one is found under its new alias.
func TestStartModel(t *testing.T) {
	for _, tc := range []struct {
		name         string
		recent       []RecentEntry
		env          func(string) string
		alias, effrt string
	}{
		{name: "no memory is the pin at its default effort", env: fundedEnv("p", "q"),
			alias: "p/default", effrt: "low"},
		{name: "the newest remembered, at its effort",
			recent: []RecentEntry{entry(t, "p/effort", "xhigh"), entry(t, "q/plain", "")}, env: fundedEnv("p", "q"),
			alias: "p/effort", effrt: "xhigh"},
		{name: "a remembered effort no longer offered is the default effort",
			recent: []RecentEntry{entry(t, "p/effort", "max")}, env: fundedEnv("p"),
			alias: "p/effort", effrt: "medium"},
		{name: "a model with no effort control has none",
			recent: []RecentEntry{entry(t, "q/plain", "high")}, env: fundedEnv("q"),
			alias: "q/plain", effrt: ""},
		{name: "unfunded and unknown entries are passed over",
			recent: []RecentEntry{entry(t, "nk/dry", "high"), {Alias: "gone", Provider: "x", WireModel: "y"}, entry(t, "q/plain", "")},
			env:    fundedEnv("p", "q"), alias: "q/plain"},
		{name: "a renamed model is found by identity",
			recent: []RecentEntry{{Alias: "p/effort-2025", Provider: "p", WireModel: "w-effort", Effort: "low"}}, env: fundedEnv("p"),
			alias: "p/effort", effrt: "low"},
		{name: "no funded memory is the pin",
			recent: []RecentEntry{entry(t, "q/plain", "")}, env: fundedEnv("p"),
			alias: "p/default", effrt: "low"},
		{name: "an unfunded pin gives way to the first funded alias",
			recent: []RecentEntry{entry(t, "nk/dry", "high")}, env: fundedEnv("q"),
			alias: "q/plain", effrt: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alias, effort, err := recentTable().StartModel(tc.recent, tc.env)
			if err != nil || alias != tc.alias || effort != tc.effrt {
				t.Fatalf("StartModel = %q, %q, %v; want %q, %q", alias, effort, err, tc.alias, tc.effrt)
			}
		})
	}
	t.Run("nothing funded", func(t *testing.T) {
		alias, effort, err := recentTable().StartModel([]RecentEntry{entry(t, "q/plain", "")}, fundedEnv())
		if !errors.Is(err, ErrNothingFunded) || !errors.Is(err, ErrNoAPIKey) || alias != "" || effort != "" {
			t.Fatalf("StartModel with no key = %q, %q, %v; want ErrNothingFunded, also ErrNoAPIKey", alias, effort, err)
		}
	})
	t.Run("a pin that fails for another reason is left to Open", func(t *testing.T) {
		table := recentTable()
		table.Models["p/default"] = Model{Provider: "missing", WireModel: "w-default"}
		alias, _, err := table.StartModel(nil, fundedEnv("q"))
		if err != nil || alias != "p/default" {
			t.Fatalf("StartModel = %q, %v; want the default, whose own error Open reports", alias, err)
		}
	})
}

// rankedTable is recentTable over a catalog's provider order and starts
// (plan 038 §2): nk, then q, then p, each starting on one of its models — q
// on a model that does not sort first among its own — and no pin.
func rankedTable() *Table {
	t := recentTable()
	t.NoCatalog, t.DefaultModel = false, ""
	t.catalogOrder = []string{"nk", "q", "p"}
	t.starts = map[string][]string{"nk": {"nk/dry"}, "q": {"q/twin-b", "q/plain"}, "p": {"p/effort"}}
	return t
}

// TestStartPickSteps (plan 038 §2.3): each step of the order, over a table
// with a provider order. The pin beats the order; a pin with no key gives way
// to it, and one that fails for another reason is still the answer; the
// order takes the first ranked provider with a start that resolves, at the
// start's default effort; a user's ProviderOrder replaces the catalog's,
// passing over names that are no provider; and only the last step, the first
// funded alias by name, is a Fallback.
func TestStartPickSteps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Table)
		recent []RecentEntry
		env    func(string) string
		want   Start
	}{
		{name: "remembered beats everything", recent: []RecentEntry{entry(t, "p/default", "high")},
			mutate: func(t *Table) { t.DefaultModel = "q/plain" }, env: fundedEnv("p", "q"),
			want: Start{Alias: "p/default", Effort: "high"}},
		{name: "a funded pin beats the order", mutate: func(t *Table) { t.DefaultModel = "p/default" },
			env: fundedEnv("p", "q", "nk"), want: Start{Alias: "p/default", Effort: "low"}},
		{name: "an unfunded pin gives way to the order", mutate: func(t *Table) { t.DefaultModel = "p/default" },
			env: fundedEnv("q"), want: Start{Alias: "q/twin-b"}},
		{name: "a pin that fails for another reason is still the answer", mutate: func(t *Table) {
			t.DefaultModel = "p/default"
			t.Models["p/default"] = Model{Provider: "missing", WireModel: "w-default"}
		}, env: fundedEnv("q"), want: Start{Alias: "p/default"}},
		{name: "no pin: the first ranked provider funded, on its start", env: fundedEnv("p", "q"),
			want: Start{Alias: "q/twin-b"}},
		{name: "the first ranked provider wins over a later one", env: fundedEnv("p", "q", "nk"),
			want: Start{Alias: "nk/dry", Effort: "high"}},
		{name: "a start whose model moved provider is passed over", mutate: func(t *Table) {
			t.Models["q/twin-b"] = Model{Provider: "p", WireModel: "w-twin"}
		}, env: fundedEnv("q"), want: Start{Alias: "q/plain"}},
		{name: "the user's order replaces the catalog's", mutate: func(t *Table) { t.ProviderOrder = []string{"nope", "p", "q"} },
			env: fundedEnv("p", "q"), want: Start{Alias: "p/effort", Effort: "medium"}},
		{name: "nothing in the order funded: the first funded alias, a fallback",
			mutate: func(t *Table) { t.ProviderOrder = []string{"nk"} }, env: fundedEnv("p", "q"),
			want: Start{Alias: "p/default", Effort: "low", Fallback: true}},
		{name: "an empty order is no order", mutate: func(t *Table) { t.ProviderOrder = []string{} }, env: fundedEnv("q"),
			want: Start{Alias: "q/plain", Fallback: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := rankedTable()
			if tc.mutate != nil {
				tc.mutate(table)
			}
			got, err := table.StartPick(tc.recent, tc.env)
			if err != nil || got != tc.want {
				t.Fatalf("StartPick = %+v, %v; want %+v", got, err, tc.want)
			}
			alias, effort, err := table.StartModel(tc.recent, tc.env)
			if err != nil || alias != tc.want.Alias || effort != tc.want.Effort {
				t.Fatalf("StartModel = %q, %q, %v; want StartPick's %+v", alias, effort, err, tc.want)
			}
		})
	}
	t.Run("nothing funded", func(t *testing.T) {
		for _, pin := range []string{"", "p/default"} {
			table := rankedTable()
			table.DefaultModel = pin
			got, err := table.StartPick(nil, fundedEnv())
			if !errors.Is(err, ErrNothingFunded) || !errors.Is(err, ErrNoAPIKey) || got != (Start{}) {
				t.Fatalf("pin %q: StartPick with no key = %+v, %v; want ErrNothingFunded, also ErrNoAPIKey", pin, got, err)
			}
		}
	})
}

// TestStartModelOverTheShippedCatalog (plan 038 §4): where a new session with
// nothing remembered starts on a home with the shipped catalog — Z.AI alone
// on glm-5.3, Fireworks alone on fireworks/ember-1, OpenRouter alone on
// openrouter/gemini-3.8-flash, Meta alone on muse-spark-1.3-contributor, and
// with every key the eval winner, Z.AI; never on deepseek. The user's pin in
// models.toml beats the order, the user's provider_order reorders it, and an
// order that leaves out the funded provider falls back to the first funded
// alias by name, which the caller notes. No key: ErrNothingFunded.
func TestStartModelOverTheShippedCatalog(t *testing.T) {
	const (
		zai = "ZHIPU_API_KEY"
		fw  = "FIREWORKS_API_KEY"
		or  = "OPENROUTER_API_KEY"
		mt  = "META_API_KEY"
	)
	for _, tc := range []struct {
		name   string
		models string
		keys   []string
		want   Start
	}{
		{"Z.AI alone", "", []string{zai}, Start{Alias: "glm-5.3", Effort: "max"}},
		{"Fireworks alone", "", []string{fw}, Start{Alias: "fireworks/ember-1", Effort: "high"}},
		{"OpenRouter alone", "", []string{or}, Start{Alias: "openrouter/gemini-3.8-flash", Effort: "medium"}},
		{"Meta alone", "", []string{mt}, Start{Alias: "muse-spark-1.3-contributor", Effort: "high"}},
		{"every key", "", []string{zai, fw, or, mt}, Start{Alias: "glm-5.3", Effort: "max"}},
		{"Fireworks and Meta", "", []string{fw, mt}, Start{Alias: "fireworks/ember-1", Effort: "high"}},
		{"a pin beats the order", `default_model = "fireworks/kimi-k3"`, []string{zai, fw},
			Start{Alias: "fireworks/kimi-k3", Effort: "high"}},
		{"an unfunded pin gives way to the order", `default_model = "fireworks/kimi-k3"`, []string{zai},
			Start{Alias: "glm-5.3", Effort: "max"}},
		{"the user's order", `provider_order = ["meta", "fireworks", "zai-coding-plan"]`, []string{zai, fw},
			Start{Alias: "fireworks/ember-1", Effort: "high"}},
		{"an order without the funded provider", `provider_order = ["meta"]`, []string{fw},
			Start{Alias: "fireworks/deepseek-v4p1-flash", Effort: "high", Fallback: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := ""
			if tc.models != "" {
				models = "version = 1\n" + tc.models + "\n"
			}
			tbl, err := Load(writeFiles(t, "", models))
			if err != nil {
				t.Fatal(err)
			}
			wantWarnings(t, tbl)
			env := map[string]string{}
			for _, k := range tc.keys {
				env[k] = "sk-start-dummy-0001"
			}
			got, err := tbl.StartPick(nil, fakeEnv(env))
			if err != nil || got != tc.want {
				t.Fatalf("StartPick = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
	t.Run("nothing funded", func(t *testing.T) {
		tbl, err := Load(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if got, err := tbl.StartPick(nil, fakeEnv(nil)); !errors.Is(err, ErrNothingFunded) || !errors.Is(err, ErrNoAPIKey) || got != (Start{}) {
			t.Fatalf("StartPick with no key = %+v, %v; want ErrNothingFunded, also ErrNoAPIKey", got, err)
		}
	})
}

// TestRememberedEffort (P6): an explicit model starts at the effort the
// memory holds for it — found by identity after a rename — while the model
// still offers it; otherwise "" (its default_effort), and "" for a model the
// memory does not hold or the table does not have.
func TestRememberedEffort(t *testing.T) {
	table := recentTable()
	recent := []RecentEntry{
		entry(t, "p/effort", "xhigh"),
		{Alias: "p/default-old", Provider: "p", WireModel: "w-default", Effort: "high"},
		entry(t, "nk/dry", "gone"),
		entry(t, "q/plain", ""),
	}
	for alias, want := range map[string]string{
		"p/effort":  "xhigh",
		"p/default": "high",
		"nk/dry":    "",
		"q/plain":   "",
		"q/twin-a":  "",
		"nope":      "",
	} {
		if got := table.RememberedEffort(recent, alias); got != want {
			t.Errorf("RememberedEffort(%q) = %q, want %q", alias, got, want)
		}
	}
}

// abnormalRecent makes recent.json in dir something other than a small regular
// file: a FIFO (skipped where the platform has none) or one past
// RecentMaxBytes.
func abnormalRecent(t *testing.T, kind string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, RecentFile)
	switch kind {
	case "fifo":
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Skipf("no FIFOs here: %v", err)
		}
	case "oversized":
		big := `{"version": 1, "recent": [], "pad": "` + strings.Repeat("x", RecentMaxBytes) + `"}`
		writeRecent(big)(t, path)
	}
	return path
}

// within fails the test, rather than hang it, when f does not return by the
// deadline; the goroutine of a blocked f is abandoned.
func within(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked: recent.json read did not return")
	}
}

// TestAbnormalRecentNeverBlocksOrGrows: a recent.json that is a FIFO or is
// larger than RecentMaxBytes reads as no memory and is refused by Remember
// (never replaced), without blocking (plan 031 X33, review r3).
func TestAbnormalRecentNeverBlocksOrGrows(t *testing.T) {
	for _, kind := range []string{"fifo", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			path := abnormalRecent(t, kind)
			dir := filepath.Dir(path)
			within(t, func() {
				if got := ReadRecent(dir); len(got) != 0 {
					t.Errorf("ReadRecent = %v; want none", got)
				}
			})
			var err error
			within(t, func() { err = Remember(dir, entry(t, "q/plain", ""), time.Now()) })
			if err == nil {
				t.Fatal("Remember over an abnormal recent.json succeeded")
			}
			fi, serr := os.Lstat(path)
			if serr != nil {
				t.Fatal(serr)
			}
			if kind == "fifo" && fi.Mode()&os.ModeNamedPipe == 0 {
				t.Fatalf("the FIFO was replaced: %v", fi.Mode())
			}
			if kind == "oversized" && fi.Size() <= RecentMaxBytes {
				t.Fatalf("the oversized file was replaced (%d bytes)", fi.Size())
			}
		})
	}
}
