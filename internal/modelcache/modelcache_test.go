package modelcache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The model catalog cache (plan 030 §3.14, R2-12): a round trip, the
// freshness rule — the newer observation is the one left, whichever writer
// finishes last, forced in both orders — and every file Read ignores.

// step bounds each wait of these tests on its own: a writer that should get
// somewhere, one that should finish.
const step = 10 * time.Second

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func catalog(provider string, at time.Time, ids ...string) Catalog {
	c := Catalog{ObservedAt: at, Provider: provider}
	for _, id := range ids {
		c.Models = append(c.Models, Model{ID: id, Name: strings.ToUpper(id)})
	}
	return c
}

// read is dir's catalog of provider, or the test fails.
func read(t *testing.T, dir, provider string) Catalog {
	t.Helper()
	c, err := Read(dir, provider)
	if err != nil {
		t.Fatalf("read %s: %v", provider, err)
	}
	return c
}

// TestAWrittenCatalogReadsBack: what Write wrote Read answers, in the
// schema's shape — version 1, the provider, the time in UTC, the models in
// order — as a 0600 file beside its lock; another provider's is its own file.
func TestAWrittenCatalogReadsBack(t *testing.T) {
	dir := t.TempDir()
	want := catalog("cursor", t0, "grok-4.6", "composer-2.5")
	want.Models[1].Name = ""
	if ok, err := Write(dir, want); !ok || err != nil {
		t.Fatalf("write: %v, %v", ok, err)
	}
	want.Version = Version
	if got := read(t, dir, "cursor"); !reflect.DeepEqual(got, want) {
		t.Fatalf("read back %+v, want %+v", got, want)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "cursor.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `{"version":1,"observedAt":"2026-09-01T12:00:00Z","provider":"cursor","models":[{"id":"grok-4.6","name":"GROK-4.6"},{"id":"composer-2.5"}]}`) {
		t.Fatalf("the file is %s", raw)
	}
	for name, perm := range map[string]os.FileMode{"cursor.json": 0o600, "cursor.lock": 0o600} {
		if st, err := os.Stat(filepath.Join(dir, name)); err != nil || st.Mode().Perm() != perm {
			t.Fatalf("%s: %v, %v", name, st, err)
		}
	}
	if _, err := Read(dir, "grok"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("grok's catalog, never written: %v", err)
	}
}

// TestOnlyANewerObservationReplacesTheFile: an older or an equally old
// observation leaves the file as it is; a newer one replaces it — including
// a monotonic-clocked time against the file's wall time.
func TestOnlyANewerObservationReplacesTheFile(t *testing.T) {
	dir := t.TempDir()
	if ok, err := Write(dir, catalog("grok", t0, "a")); !ok || err != nil {
		t.Fatalf("first: %v, %v", ok, err)
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"older", t0.Add(-time.Second), false},
		{"as old", t0, false},
		{"as old, in another zone", t0.In(time.FixedZone("x", 3600)), false},
		{"newer", t0.Add(time.Nanosecond), true},
	} {
		ok, err := Write(dir, catalog("grok", tc.at, "b-"+strings.ReplaceAll(tc.name, " ", "-")))
		if err != nil || ok != tc.want {
			t.Fatalf("%s: written %v, %v; want %v", tc.name, ok, err, tc.want)
		}
	}
	if got := read(t, dir, "grok"); got.Models[0].ID != "b-newer" {
		t.Fatalf("the file holds %+v", got)
	}
	now := time.Now()
	if ok, err := Write(dir, catalog("grok", now, "live")); !ok || err != nil {
		t.Fatalf("a time read off the clock: %v, %v", ok, err)
	}
	if ok, _ := Write(dir, catalog("grok", now, "again")); ok {
		t.Fatal("the same clocked time replaced the file it wrote")
	}
}

type writeResult struct {
	written bool
	err     error
}

func write(dir string, c Catalog) <-chan writeResult {
	done := make(chan writeResult, 1)
	go func() {
		ok, err := Write(dir, c)
		done <- writeResult{ok, err}
	}()
	return done
}

func await(t *testing.T, done <-chan writeResult, what string) writeResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(step):
		t.Fatalf("%s did not finish within %v", what, step)
		return writeResult{}
	}
}

// notYet fails if done has answered: the writer is waiting on the lock.
// A short look, not a bound — the writer is proven blocked by what it
// answers once released.
func notYet(t *testing.T, done <-chan writeResult, what string) {
	t.Helper()
	select {
	case r := <-done:
		t.Fatalf("%s finished (%+v) while the other held the lock", what, r)
	case <-time.After(50 * time.Millisecond):
	}
}

// holdIn installs a replaceHook that holds the writer of the catalog whose
// first model is id inside the lock until release is closed.
func holdIn(t *testing.T, id string) (held, release chan struct{}) {
	t.Helper()
	held, release = make(chan struct{}), make(chan struct{})
	replaceHook = func(c Catalog) {
		if c.Models[0].ID == id {
			close(held)
			<-release
		}
	}
	t.Cleanup(func() { replaceHook = nil })
	return held, release
}

// TestTheNewerObservationWinsWhicheverWriterFinishesLast (R2-12): two hosts
// of one provider write at once — older, observed at t0, and newer, at t0+1s.
// Each schedule is forced, not left to chance: a writer is held inside the
// lock, after its comparison and before its replace, while the other is
// started.
//
//   - The older writer holds the lock: without it both would compare against
//     the empty file and the older, replacing last, would win. The newer one
//     waits for the lock, compares against what the older wrote, and
//     replaces it.
//   - The newer writer holds the lock, and the older one — finishing last —
//     compares against the newer file once it has the lock, and leaves it.
//
// Either way the file holds the newer observation.
func TestTheNewerObservationWinsWhicheverWriterFinishesLast(t *testing.T) {
	// The held writer keeps the other waiting for the lock for as long as
	// the test takes to look, whatever the machine's load.
	saved := lockWait
	lockWait = time.Minute
	t.Cleanup(func() { lockWait = saved })
	older, newer := catalog("cursor", t0, "older"), catalog("cursor", t0.Add(time.Second), "newer")

	t.Run("the older writer holds the lock", func(t *testing.T) {
		dir := t.TempDir()
		held, release := holdIn(t, "older")
		od := write(dir, older)
		select {
		case <-held:
		case <-time.After(step):
			t.Fatalf("the older writer did not reach its replace within %v", step)
		}
		nd := write(dir, newer)
		notYet(t, nd, "the newer writer")
		close(release)
		if r := await(t, od, "the older writer"); !r.written || r.err != nil {
			t.Fatalf("the older writer: %+v", r)
		}
		if r := await(t, nd, "the newer writer"); !r.written || r.err != nil {
			t.Fatalf("the newer writer, after the older's replace: %+v", r)
		}
		if got := read(t, dir, "cursor"); got.Models[0].ID != "newer" {
			t.Fatalf("the file holds %q, want the newer observation", got.Models[0].ID)
		}
	})

	t.Run("the older writer finishes last", func(t *testing.T) {
		dir := t.TempDir()
		held, release := holdIn(t, "newer")
		nd := write(dir, newer)
		select {
		case <-held:
		case <-time.After(step):
			t.Fatalf("the newer writer did not reach its replace within %v", step)
		}
		od := write(dir, older)
		notYet(t, od, "the older writer")
		close(release)
		if r := await(t, nd, "the newer writer"); !r.written || r.err != nil {
			t.Fatalf("the newer writer: %+v", r)
		}
		if r := await(t, od, "the older writer"); r.written || r.err != nil {
			t.Fatalf("the older writer, finishing last: %+v, want it to leave the newer file", r)
		}
		if got := read(t, dir, "cursor"); got.Models[0].ID != "newer" {
			t.Fatalf("the file holds %q, want the newer observation", got.Models[0].ID)
		}
	})
}

// TestABusyLockIsAnErrorNotAWait: a writer that cannot have the lock within
// its bound gives up and writes nothing; the writer holding it is not
// disturbed.
func TestABusyLockIsAnErrorNotAWait(t *testing.T) {
	saved := lockWait
	lockWait = 0 // one try each: the first finds the lock free
	t.Cleanup(func() { lockWait = saved })
	dir := t.TempDir()
	held, release := holdIn(t, "first")
	fd := write(dir, catalog("gx", t0, "first"))
	select {
	case <-held:
	case <-time.After(step):
		t.Fatalf("the first writer did not reach its replace within %v", step)
	}
	ok, err := Write(dir, catalog("gx", t0.Add(time.Second), "second"))
	close(release)
	if ok || err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("a writer that could not have the lock: %v, %v", ok, err)
	}
	if r := await(t, fd, "the first writer"); !r.written || r.err != nil {
		t.Fatalf("the first writer: %+v", r)
	}
	if got := read(t, dir, "gx"); got.Models[0].ID != "first" {
		t.Fatalf("the file holds %q", got.Models[0].ID)
	}
}

// TestReadIgnoresWhatItDidNotWrite: every file that is not a catalog this
// package writes is an error from Read — none to offer — and the next Write
// replaces it, whatever time it claims.
func TestReadIgnoresWhatItDidNotWrite(t *testing.T) {
	good := `{"version":1,"observedAt":"2026-09-30T12:00:00Z","provider":"cursor","models":[{"id":"a"}]}`
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"not JSON", "not json", ErrCorrupt},
		{"empty", "", ErrCorrupt},
		{"two objects", good + good, ErrCorrupt},
		{"an array", `[` + good + `]`, ErrCorrupt},
		{"another version", strings.Replace(good, `"version":1`, `"version":2`, 1), ErrCorrupt},
		{"no version", strings.Replace(good, `"version":1,`, ``, 1), ErrCorrupt},
		{"another provider's", strings.Replace(good, `"provider":"cursor"`, `"provider":"grok"`, 1), ErrCorrupt},
		{"no time", strings.Replace(good, `"observedAt":"2026-09-30T12:00:00Z",`, ``, 1), ErrCorrupt},
		{"a bad time", strings.Replace(good, `2026-09-30T12:00:00Z`, `yesterday`, 1), ErrCorrupt},
		{"no models", strings.Replace(good, `[{"id":"a"}]`, `[]`, 1), ErrCorrupt},
		{"a model with no id", strings.Replace(good, `{"id":"a"}`, `{"name":"A"}`, 1), ErrCorrupt},
		{"an id with a newline", strings.Replace(good, `{"id":"a"}`, `{"id":"a\nb"}`, 1), ErrCorrupt},
		{"a name with an escape", strings.Replace(good, `{"id":"a"}`, `{"id":"a","name":"\u001b[31mA"}`, 1), ErrCorrupt},
		{"oversized", `{"version":1,"x":"` + strings.Repeat("a", MaxBytes) + `"}`, ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "cursor.json"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(dir, "cursor"); !errors.Is(err, tc.want) {
				t.Fatalf("read: %v, want %v", err, tc.want)
			}
			// Replaced by a write however old its observation.
			if ok, err := Write(dir, catalog("cursor", time.Unix(1, 0), "fresh")); !ok || err != nil {
				t.Fatalf("a write over it: %v, %v", ok, err)
			}
			if got := read(t, dir, "cursor"); got.Models[0].ID != "fresh" {
				t.Fatalf("after the write: %+v", got)
			}
		})
	}
	if _, err := Read(t.TempDir(), "cursor"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no file: %v", err)
	}
}

// TestReadIgnoresAnythingButARegularFile: a symlink is never followed, a
// directory or a FIFO in the file's place is not read (and a FIFO never
// waited on).
func TestReadIgnoresAnythingButARegularFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if ok, err := Write(filepath.Dir(target), catalog("cursor", t0, "a")); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(target), "cursor.json"), filepath.Join(dir, "cursor.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir, "cursor"); err == nil {
		t.Fatal("a symlink was followed")
	}
	if err := os.Mkdir(filepath.Join(dir, "grok.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir, "grok"); err == nil {
		t.Fatal("a directory was read as a catalog")
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "gx.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Read(dir, "gx"); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("a FIFO: %v, want ErrCorrupt", err)
		}
	case <-time.After(step):
		t.Fatalf("a FIFO's read did not return within %v", step)
	}
}

// TestWriteRefusesWhatItWouldNotRead: a catalog Read would ignore is never
// written — no file, no lock — and a provider id that is not one cannot name
// a file at all.
func TestWriteRefusesWhatItWouldNotRead(t *testing.T) {
	big := catalog("cursor", t0)
	for i := 0; len(big.Models)*40 < MaxBytes; i++ {
		big.Models = append(big.Models, Model{ID: strings.Repeat("m", 30) + string(rune('a'+i%26)), Name: strings.Repeat("n", 10)})
	}
	for _, tc := range []struct {
		name string
		c    Catalog
		want error
	}{
		{"no models", catalog("cursor", t0), ErrCorrupt},
		{"no time", catalog("cursor", time.Time{}, "a"), ErrCorrupt},
		{"a model with no id", Catalog{ObservedAt: t0, Provider: "cursor", Models: []Model{{Name: "A"}}}, ErrCorrupt},
		{"a control character", Catalog{ObservedAt: t0, Provider: "cursor", Models: []Model{{ID: "a\tb"}}}, ErrCorrupt},
		{"invalid UTF-8", Catalog{ObservedAt: t0, Provider: "cursor", Models: []Model{{ID: "a\xffb"}}}, ErrCorrupt},
		{"a path for a provider", catalog("../cursor", t0, "a"), ErrCorrupt},
		{"an empty provider", catalog("", t0, "a"), ErrCorrupt},
		{"a capital in the provider", catalog("Cursor", t0, "a"), ErrCorrupt},
		{"larger than the bound", big, ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if ok, err := Write(dir, tc.c); ok || !errors.Is(err, tc.want) {
				t.Fatalf("written %v, %v; want %v", ok, err, tc.want)
			}
			if ents, _ := os.ReadDir(dir); len(ents) != 0 {
				t.Fatalf("a refused write left %v", ents)
			}
		})
	}
	for _, id := range []string{"", "..", "a/b", "-x", "X", strings.Repeat("a", maxProviderLen+1)} {
		if ValidProvider(id) {
			t.Errorf("%q is a provider id", id)
		}
		if _, err := Read(t.TempDir(), id); !errors.Is(err, ErrCorrupt) {
			t.Errorf("read %q: %v", id, err)
		}
	}
	for _, id := range []string{"cursor", "grok", "gx", "native", "a", "x-1_y"} {
		if !ValidProvider(id) {
			t.Errorf("%q is not a provider id", id)
		}
	}
}
