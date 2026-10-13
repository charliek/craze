package rundir

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// The stopping mark (plan 046 B4, #93): Host.MarkStopping, the registry
// entry's stopping member, the single-entry read (HostEntry) and the one rule
// every roster reader applies (Serving, ServingOnly).

// entryFileID is the (dev, ino) of host h's entry file now: every rewrite
// installs a new file, so an unchanged id is no rewrite.
func entryFileID(t *testing.T, env Env, h *Host) FileID {
	t.Helper()
	fi, err := os.Lstat(filepath.Join(hostsDir(env), h.ID()+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return idOf(fi)
}

// TestMarkStoppingIsWrittenAtOnceAndKept: the mark is in the entry file when
// MarkStopping returns — a reader of the registry reads it then, not after
// some writer gets to it — it is written once (a second call rewrites
// nothing), every later rewrite keeps it, one that tries to clear it
// included, and after Close nothing is written. The negative control: before
// the mark, the file says nothing of it, an older reader's view.
func TestMarkStoppingIsWrittenAtOnceAndKept(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h := bind(t, env)
	path := filepath.Join(hostsDir(env), h.ID()+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "stopping") {
		t.Fatalf("a serving host's entry mentions stopping: %s", b)
	}

	if err := h.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	if got := readEntryFile(t, path); !got.Stopping {
		t.Fatalf("the entry file after MarkStopping: %+v, want stopping", got)
	}
	if !h.Entry().Stopping {
		t.Fatal("the host's own copy of its entry is not stopping")
	}
	var m map[string]any
	b, _ = os.ReadFile(path)
	if err := json.Unmarshal(b, &m); err != nil || m["stopping"] != true {
		t.Fatalf("the entry file holds %s (%v), want stopping: true", b, err)
	}
	var old entryV1
	if err := json.Unmarshal(b, &old); err != nil || old.CrazeSessionID != "s-1" {
		t.Fatalf("an older reader decoded %+v, %v", old, err)
	}

	id := entryFileID(t, env, h)
	if err := h.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	if entryFileID(t, env, h) != id {
		t.Fatal("a second MarkStopping rewrote the entry")
	}

	if err := h.Update(func(e *Entry) { e.Stopping, e.Incarnation, e.Ready = false, "inc-2", true }); err != nil {
		t.Fatal(err)
	}
	if got := readEntryFile(t, path); !got.Stopping || got.Incarnation != "inc-2" || !got.Ready {
		t.Fatalf("a rewrite after the mark: %+v, want it applied and the mark kept", got)
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.MarkStopping(); !errors.Is(err, ErrClosed) {
		t.Fatalf("MarkStopping after Close = %v, want ErrClosed", err)
	}
	if exists(t, path) {
		t.Fatal("MarkStopping after Close wrote the entry")
	}
}

// TestTheMarkSurvivesRewritesRacingIt: rewrites racing the mark — a host's
// writer landing its engine's identity while the stop marks — leave the mark
// on, whichever lands last; each rewrite's own change lands too.
func TestTheMarkSurvivesRewritesRacingIt(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h := bind(t, env)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 10 {
				if err := h.Update(func(e *Entry) { e.Stopping, e.Incarnation = false, strconv.Itoa(i*100+j) }); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Go(func() {
		if err := h.MarkStopping(); err != nil {
			t.Error(err)
		}
	})
	wg.Wait()
	if err := h.Update(func(e *Entry) { e.Incarnation = "last" }); err != nil {
		t.Fatal(err)
	}
	got := readEntryFile(t, filepath.Join(hostsDir(env), h.ID()+".json"))
	if !got.Stopping || got.Incarnation != "last" {
		t.Fatalf("after the race and a last rewrite: %+v, want stopping and incarnation last", got)
	}
}

// TestTheMarkIsWrittenOnlyOverTheHostsOwnEntry: a host stopping because it
// was lost — its entry unlinked from under it, or another file put at its
// name — marks nothing: the gone entry is not brought back, and the other
// file is left as it is.
func TestTheMarkIsWrittenOnlyOverTheHostsOwnEntry(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	gone := bind(t, env)
	path := filepath.Join(hostsDir(env), gone.ID()+".json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := gone.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	if exists(t, path) {
		t.Fatal("MarkStopping brought back an entry gone from under it")
	}

	other := bind(t, env)
	path = filepath.Join(hostsDir(env), other.ID()+".json")
	const theirs = `{"hostId":"someone else's"}`
	// Renamed over the entry, made while the entry still exists: a file
	// written after a remove can be given the removed one's inode, and so
	// its (dev, ino) — which no identity check can tell apart.
	writeFile(t, path+".theirs", theirs)
	if err := os.Rename(path+".theirs", path); err != nil {
		t.Fatal(err)
	}
	if err := other.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != theirs {
		t.Fatalf("MarkStopping wrote over another file: %q, %v", b, err)
	}
}

// TestAStoppingHostsEntryIsNeverBroughtBack (review c3 2): the schedule a
// lost host's stop makes — its entry gone from under it, the mark finding
// nothing to mark, then a rewrite queued before the stop landing — brings
// nothing back: the rewrite writes no entry at all, never an unmarked one.
// The negative control is the same rewrite with no mark before it, which
// writes the entry again, as it always has.
func TestAStoppingHostsEntryIsNeverBroughtBack(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	for _, mark := range []bool{true, false} {
		h := bind(t, env)
		path := filepath.Join(hostsDir(env), h.ID()+".json")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if mark {
			if err := h.MarkStopping(); err != nil {
				t.Fatal(err)
			}
		}
		if err := h.Update(func(e *Entry) { e.Incarnation, e.Ready = "inc-ready", true }); err != nil {
			t.Fatal(err)
		}
		switch got, ok := readEntryIfThere(t, path); {
		case mark && ok:
			t.Fatalf("a rewrite after the mark brought the entry back: %+v", got)
		case !mark && (!ok || got.Stopping || !got.Ready):
			t.Fatalf("the control: a rewrite with no mark wrote %+v (exists %v), want the entry again, unmarked", got, ok)
		}
	}
}

// TestARewriteWritesAMarkThatFailedToLand: a mark whose write fails still
// makes the host stopping, so the next rewrite of its own entry writes the
// mark; before the mark, no rewrite can set it.
func TestARewriteWritesAMarkThatFailedToLand(t *testing.T) {
	env := testEnv(t)
	h := bind(t, env)
	path := filepath.Join(hostsDir(env), h.ID()+".json")
	if err := h.Update(func(e *Entry) { e.Stopping = true }); err != nil {
		t.Fatal(err)
	}
	if got := readEntryFile(t, path); got.Stopping {
		t.Fatalf("a rewrite before the mark set it: %+v", got)
	}
	old := fstatTemp
	t.Cleanup(func() { fstatTemp = old })
	fstatTemp = func(int, *unix.Stat_t) error { return unix.EIO }
	if err := h.MarkStopping(); !errors.Is(err, unix.EIO) {
		t.Fatalf("MarkStopping with a failing write = %v, want EIO", err)
	}
	fstatTemp = old
	if got := readEntryFile(t, path); got.Stopping {
		t.Fatalf("a failed mark landed: %+v", got)
	}
	if err := h.Update(func(e *Entry) { e.Ready = true }); err != nil {
		t.Fatal(err)
	}
	if got := readEntryFile(t, path); !got.Stopping || !got.Ready {
		t.Fatalf("the rewrite after a failed mark: %+v, want it marked", got)
	}
}

// readEntryIfThere is the entry file at path, ok false when there is none.
func readEntryIfThere(t *testing.T, path string) (Entry, bool) {
	t.Helper()
	if !exists(t, path) {
		return Entry{}, false
	}
	return readEntryFile(t, path), true
}

// TestBindClearsTheMark: a host is bound serving whatever entry its caller
// hands Bind — only MarkStopping marks it.
func TestBindClearsTheMark(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	h, err := Bind(env, NewHostID(), Entry{Workspace: "/w", Stopping: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if got := readEntryFile(t, filepath.Join(hostsDir(env), h.ID()+".json")); got.Stopping || h.Entry().Stopping {
		t.Fatalf("a bound host's entry is stopping: %+v", got)
	}
}

// TestHostEntryReadsOneHost: the single-entry read answers a registered
// host's entry as it stands — its mark included — and missing, with no
// error, for a host that never registered, one that has closed, and a
// registry tree that does not exist; an entry that names another host, one
// that does not decode, and a host id that is not one are errors. Hosts, the
// scan, lists a stopping host as it is: Serving is its readers' rule.
func TestHostEntryReadsOneHost(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	if _, ok, err := HostEntry(env, NewHostID()); ok || err != nil {
		t.Fatalf("no registry tree: ok %v, err %v; want missing", ok, err)
	}
	h := bind(t, env)
	got, ok, err := HostEntry(env, h.ID())
	if err != nil || !ok || got != h.Entry() {
		t.Fatalf("HostEntry = %+v, %v, %v; want %+v", got, ok, err, h.Entry())
	}
	if _, ok, err := HostEntry(env, NewHostID()); ok || err != nil {
		t.Fatalf("a host never registered: ok %v, err %v; want missing", ok, err)
	}
	if err := h.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := HostEntry(env, h.ID()); err != nil || !ok || !got.Stopping {
		t.Fatalf("after the mark: %+v, %v, %v; want stopping", got, ok, err)
	}
	listed, err := Hosts(env)
	if err != nil || len(listed) != 1 || !listed[0].Stopping {
		t.Fatalf("Hosts = %+v, %v; want the stopping host listed as it is", listed, err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := HostEntry(env, h.ID()); ok || err != nil {
		t.Fatalf("a closed host: ok %v, err %v; want missing", ok, err)
	}

	other := NewHostID()
	writeFile(t, filepath.Join(hostsDir(env), other+".json"), `{"hostId":"`+NewHostID()+`"}`)
	if _, _, err := HostEntry(env, other); err == nil || !strings.Contains(err.Error(), "names host") {
		t.Fatalf("an entry naming another host: %v, want an error", err)
	}
	broken := NewHostID()
	writeFile(t, filepath.Join(hostsDir(env), broken+".json"), `{"hostId":`)
	if _, _, err := HostEntry(env, broken); err == nil {
		t.Fatal("an entry that does not decode read as an entry")
	}
	if _, _, err := HostEntry(env, "../hosts/x"); err == nil {
		t.Fatal("a host id that is not one was read")
	}
}

// TestServingIsTheRostersRule: Serving lists every host but a stopping one.
func TestServingIsTheRostersRule(t *testing.T) {
	t.Parallel()
	if !Serving(Entry{HostID: "a"}) || Serving(Entry{HostID: "b", Stopping: true}) {
		t.Fatal("Serving: want a serving host listed and a stopping one not")
	}
}
