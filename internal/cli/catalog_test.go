package cli

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/modelcache"
	"github.com/charliek/craze/internal/rundir"
)

// craze serve's model catalog cache (plan 030 §3.14): what a host records,
// when, and where — the catalog its ACP agent advertises, after each install,
// in its HOME's cache tree whatever CRAZE_HOME says — and a real host
// recording the fake agent's.

// catalogStep bounds each wait here on its own.
const catalogStep = 10 * time.Second

// fakeCatalog is a session's snapshot a test moves.
type fakeCatalog struct {
	mu   sync.Mutex
	snap agent.Snapshot
}

func (f *fakeCatalog) set(models ...agent.ModelInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = agent.Snapshot{Models: models}
}

func (f *fakeCatalog) snapshot() agent.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

// catalogEnv is a cache tree of the test's own: a 0700 HOME.
func catalogEnv(t *testing.T) rundir.Env {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return rundir.Env{Home: home, CrazeDir: t.TempDir(), EUID: os.Geteuid()}
}

// cached is env's cached catalog of provider once one is there with the
// models want, within catalogStep.
func cached(t *testing.T, env rundir.Env, provider string, want ...string) modelcache.Catalog {
	t.Helper()
	deadline := time.Now().Add(catalogStep)
	var last modelcache.Catalog
	var lastErr error
	for {
		if dir, err := rundir.CatalogDir(env, false); err == nil {
			last, lastErr = modelcache.Read(dir, provider)
			if lastErr == nil && slices.Equal(ids(last), want) {
				return last
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s catalog of %v after %v: %+v, %v", provider, want, catalogStep, last, lastErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ids(c modelcache.Catalog) []string {
	out := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		out = append(out, m.ID)
	}
	return out
}

// recorderClock is a recorder's clock a test steps: the recorder asks it the
// time right after reading the snapshot, and waits for the test's answer —
// so a test knows where the recorder is, and which snapshot it read.
type recorderClock struct {
	t      *testing.T
	asked  chan struct{}
	answer chan time.Time
}

func stepRecorder(t *testing.T, r *catalogRecorder) *recorderClock {
	c := &recorderClock{t: t, asked: make(chan struct{}), answer: make(chan time.Time)}
	r.now = func() time.Time {
		c.asked <- struct{}{}
		return <-c.answer
	}
	return c
}

// reading waits for the recorder to have read a snapshot and ask the time.
func (c *recorderClock) reading() {
	c.t.Helper()
	select {
	case <-c.asked:
	case <-time.After(catalogStep):
		c.t.Fatalf("the recorder did not read a snapshot within %v", catalogStep)
	}
}

// at answers the recorder's question with at.
func (c *recorderClock) at(at time.Time) {
	c.t.Helper()
	select {
	case c.answer <- at:
	case <-time.After(catalogStep):
		c.t.Fatalf("the recorder did not take the time within %v", catalogStep)
	}
}

// TestTheRecorderWritesEachNewCatalog: a kick records the session's catalog
// as its snapshot stands, with the time it was read; a kick whose catalog is
// the one recorded writes nothing; a changed catalog is written with its own,
// later time; a snapshot with no models writes nothing; and a kick pending
// as the recorder closes is still recorded. Each step is held where the
// recorder has read its snapshot (recorderClock), so which snapshot each kick
// read is the test's, not the scheduler's.
func TestTheRecorderWritesEachNewCatalog(t *testing.T) {
	env := catalogEnv(t)
	fc := &fakeCatalog{}
	var log lockedBuffer
	r := newCatalogRecorder(env, agent.CursorProvider(), fc.snapshot, &log)
	if r == nil {
		t.Fatal("cursor has no recorder")
	}
	clock := stepRecorder(t, r)
	t1 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	at := func(n int) time.Time { return t1.Add(time.Duration(n) * time.Minute) }

	fc.set(agent.ModelInfo{ID: "grok-4.6", Name: "Grok 4.6"}, agent.ModelInfo{ID: "composer", Name: "composer"})
	r.changed()
	clock.reading()
	clock.at(at(0))
	got := cached(t, env, "cursor", "grok-4.6", "composer")
	if !got.ObservedAt.Equal(at(0)) || got.Models[0].Name != "Grok 4.6" || got.Models[1].Name != "" {
		t.Fatalf("recorded %+v", got)
	}

	// The same catalog again. The next kick's reading proves that one done:
	// it wrote nothing, and the file keeps its time.
	r.changed()
	clock.reading()
	clock.at(at(1))
	fc.set(agent.ModelInfo{ID: "grok-4.6", Name: "Grok 4.6"})
	r.changed()
	clock.reading()
	if got := cached(t, env, "cursor", "grok-4.6", "composer"); !got.ObservedAt.Equal(at(0)) {
		t.Fatalf("the same catalog was written again, at %v", got.ObservedAt)
	}
	// A changed one: written, at its own reading's time.
	clock.at(at(2))
	if got := cached(t, env, "cursor", "grok-4.6"); !got.ObservedAt.Equal(at(2)) {
		t.Fatalf("the changed catalog was recorded at %v", got.ObservedAt)
	}

	// No models: nothing written.
	fc.set()
	r.changed()
	clock.reading()
	clock.at(at(3))

	// A kick pending as the recorder closes: held reading one catalog, a
	// second kick comes and the close begins — both ready when the first is
	// done, whichever the recorder takes, the second catalog is recorded
	// before it returns.
	fc.set(agent.ModelInfo{ID: "third", Name: "third"})
	r.changed()
	clock.reading()
	fc.set(agent.ModelInfo{ID: "last", Name: "last"})
	r.changed()
	closed := make(chan struct{})
	go func() { r.close(); close(closed) }()
	select {
	case <-r.stop:
	case <-time.After(catalogStep):
		t.Fatalf("the close did not begin within %v", catalogStep)
	}
	clock.at(at(4))
	clock.reading()
	clock.at(at(5))
	select {
	case <-closed:
	case <-time.After(catalogStep):
		t.Fatalf("the recorder did not close within %v", catalogStep)
	}
	if got := cached(t, env, "cursor", "last"); !got.ObservedAt.Equal(at(5)) {
		t.Fatalf("the pending catalog was recorded at %v", got.ObservedAt)
	}
	r.close()
	r.changed() // after the close: a kick nobody takes, and no panic
	if log.String() != "" {
		t.Fatalf("the recorder logged %q", log.String())
	}
}

// TestTheRecorderRefusesACatalogWithABadModel (C15r, sol r31-c16 2): a
// catalog with one model the cache cannot hold — a name that is not one line
// — is not recorded at all, where it used to be cached without that model
// (its good models alone) and then offered as though whole. The catalog recorded before it stays, its
// time too; the host's log says which model, once — the same catalog again
// says nothing more; and a catalog whose models are all good is recorded
// again. Each kick is held where the recorder has read its snapshot
// (recorderClock).
func TestTheRecorderRefusesACatalogWithABadModel(t *testing.T) {
	env := catalogEnv(t)
	fc := &fakeCatalog{}
	var log lockedBuffer
	r := newCatalogRecorder(env, agent.CursorProvider(), fc.snapshot, &log)
	clock := stepRecorder(t, r)
	t1 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	at := func(n int) time.Time { return t1.Add(time.Duration(n) * time.Minute) }
	good := agent.ModelInfo{ID: "composer", Name: "Composer"}
	bad := agent.ModelInfo{ID: "two-lines", Name: "Two\nLines"}

	fc.set(good)
	r.changed()
	clock.reading()
	clock.at(at(0))
	cached(t, env, "cursor", "composer")

	// A bad model beside two good ones (one new): nothing written — not
	// the good two alone. The next kick's reading proves that one done.
	fc.set(good, agent.ModelInfo{ID: "grok-4.6", Name: "Grok 4.6"}, bad)
	r.changed()
	clock.reading()
	clock.at(at(1))
	r.changed()
	clock.reading()
	if got := cached(t, env, "cursor", "composer"); !got.ObservedAt.Equal(at(0)) {
		t.Fatalf("a catalog with a bad model was recorded: the cache was written at %v", got.ObservedAt)
	}
	want := "craze serve: the model catalog cache: not recorded: model 3 of 3 (id \"two-lines\", name \"Two\\nLines\") cannot be cached\n"
	if log.String() != want {
		t.Fatalf("the log after the bad catalog: %q, want %q", log.String(), want)
	}
	// The same bad catalog again (the kick just read): nothing more said.
	clock.at(at(2))

	// All good again: recorded.
	fc.set(good, agent.ModelInfo{ID: "two-lines", Name: "Two Lines"})
	r.changed()
	clock.reading()
	clock.at(at(3))
	if got := cached(t, env, "cursor", "composer", "two-lines"); !got.ObservedAt.Equal(at(3)) {
		t.Fatalf("the good catalog was recorded at %v", got.ObservedAt)
	}
	r.close()
	if log.String() != want {
		t.Fatalf("the log at the end: %q, want the one line", log.String())
	}
}

// TestTheRecorderIsOnlyAnACPProviders: native's models are its table — no
// recorder — and a nil recorder's methods do nothing.
func TestTheRecorderIsOnlyAnACPProviders(t *testing.T) {
	if r := newCatalogRecorder(catalogEnv(t), agent.NativeProvider(), nil, nil); r != nil {
		t.Fatal("native has a catalog recorder")
	}
	for _, p := range []agent.Provider{agent.CursorProvider(), agent.GrokProvider(), agent.GxProvider()} {
		r := newCatalogRecorder(catalogEnv(t), p, func() agent.Snapshot { return agent.Snapshot{} }, nil)
		if r == nil {
			t.Fatalf("%s has no catalog recorder", p.Name())
		}
		r.close()
	}
	var none *catalogRecorder
	none.changed()
	none.close()
}

// TestTheRecorderSaysWhatItCouldNotWrite: a cache tree it cannot use is one
// line on the host's log per catalog — and the catalog is tried again at the
// next install.
func TestTheRecorderSaysWhatItCouldNotWrite(t *testing.T) {
	env := catalogEnv(t)
	// A cache directory group-writable at the leaf: refused, never repaired.
	if err := os.MkdirAll(env.Home+"/.cache/craze/catalogs", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(env.Home+"/.cache/craze/catalogs", 0o770); err != nil {
		t.Fatal(err)
	}
	fc := &fakeCatalog{}
	fc.set(agent.ModelInfo{ID: "a", Name: "a"})
	var log lockedBuffer
	r := newCatalogRecorder(env, agent.GrokProvider(), fc.snapshot, &log)
	r.changed()
	r.close()
	if !strings.HasPrefix(log.String(), "craze serve: the model catalog cache: ") || strings.Count(log.String(), "\n") != 1 {
		t.Fatalf("the log: %q", log.String())
	}
	if err := os.Chmod(env.Home+"/.cache/craze/catalogs", 0o700); err != nil {
		t.Fatal(err)
	}
	r = newCatalogRecorder(env, agent.GrokProvider(), fc.snapshot, &log)
	r.changed()
	r.close()
	cached(t, env, "grok", "a")
}

// TestServeRecordsItsAgentsCatalog: a real craze serve, over the fake
// cursor, records the catalog session/new installed — in HOME's cache tree,
// not under CRAZE_HOME — and a stop leaves it there for the next list.
func TestServeRecordsItsAgentsCatalog(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws)
	r.waitServing(t, env, true)
	got := cached(t, env, "cursor", "default", "composer")
	if got.Models[0].Name != "Default" || got.Models[1].Name != "Composer" {
		t.Fatalf("recorded %+v", got)
	}
	if _, err := os.Stat(os.Getenv("CRAZE_HOME") + "/catalogs"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("CRAZE_HOME holds a catalog cache: %v", err)
	}
	r.sigs <- syscall.SIGTERM
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve after SIGTERM: %v", err)
	}
	cached(t, env, "cursor", "default", "composer")
}
