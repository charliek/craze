package rundir

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// deadHost writes a dead host's registry files — an entry that cannot be
// read and a lock no live host holds — and answers the lock's path.
func deadHost(t *testing.T, env Env, id string) (entry, lock string) {
	t.Helper()
	dir := cacheSubdir(t, env, hostsName)
	entry, lock = filepath.Join(dir, id+".json"), filepath.Join(dir, id+".lock")
	writeFile(t, entry, "{not json")
	writeFile(t, lock, "")
	return entry, lock
}

// TestAProbeWaitsOutAnotherProbersSweep (plan 040 C7): a Scan that meets a
// dead host's lock taken by another prober — a concurrent Scan sweeping that
// same host, which holds the lock for the instant of its sweep — does not
// call the dead host alive with an unreadable entry: it tries the lock again,
// finds it let go and the host swept, and lists nothing. A hub's create used
// to be refused host_unreachable over exactly this on CI. The control, with
// no retry budget (probeContendedFor 0), lists the swept host unreadable: the
// hub's symptom.
func TestAProbeWaitsOutAnotherProbersSweep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
		listed bool
	}{
		{"with the retry", probeContendedFor, false},
		{"control: no retry", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setProbeContendedFor(t, tc.budget)
			if listed := sweptByAnotherProber(t); listed != tc.listed {
				t.Fatalf("the dead host swept by another prober listed unreadable: %v, want %v", listed, tc.listed)
			}
		})
	}
}

// setProbeContendedFor sets the scan's retry budget for the test (never in
// parallel: the tests that call it are not t.Parallel, and a package's
// parallel tests run after its sequential ones).
func setProbeContendedFor(t *testing.T, d time.Duration) {
	t.Helper()
	was := probeContendedFor
	probeContendedFor = d
	t.Cleanup(func() { probeContendedFor = was })
}

// sweptByAnotherProber scans while another prober holds a dead host's lock
// for 30 ms and then sweeps it, and reports whether the scan listed the host
// unreadable.
func sweptByAnotherProber(t *testing.T) bool {
	t.Helper()
	env := testEnv(t)
	id := NewHostID()
	entry, lock := deadHost(t, env, id)

	locked, released := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(released)
		f, err := os.OpenFile(lock, os.O_RDWR, 0)
		if err != nil {
			t.Error(err)
			close(locked)
			return
		}
		defer f.Close()
		if taken, err := tryLock(f); err != nil || !taken {
			t.Errorf("the other prober could not take the lock: %v, %v", taken, err)
			close(locked)
			return
		}
		close(locked)
		// The other prober's sweep: the entry, then the lock file, unlinked
		// under the lock, then the lock let go.
		time.Sleep(30 * time.Millisecond)
		_ = os.Remove(entry)
		_ = os.Remove(lock)
		_ = unlock(f)
	}()
	<-locked

	live, unreadable, err := HostsScan(env)
	<-released
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("a dead host was listed live: %v", entries(live))
	}
	return slices.Contains(unreadable, id)
}

// TestAScanRetriesWithinOneBudget (C7 review): many held locks over entries
// that cannot be read cost a scan one retry window in all, not one each — a
// caller with a deadline (a hub's first list, `craze ps`) is not held N
// windows.
func TestAScanRetriesWithinOneBudget(t *testing.T) {
	setProbeContendedFor(t, 200*time.Millisecond)
	env := testEnv(t)
	const n = 5
	for range n {
		_, lock := deadHost(t, env, NewHostID())
		holdLock(t, lock)
	}
	start := time.Now()
	_, unreadable, err := HostsScan(env)
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	if len(unreadable) != n {
		t.Fatalf("%d of %d held locks over unreadable entries called alive", len(unreadable), n)
	}
	if took >= 2*probeContendedFor {
		t.Fatalf("the scan took %v for %d contended entries: more than one %v window", took, n, probeContendedFor)
	}
}

// TestAProbeStillCallsAHeldLockAlive: the control — a lock held for good (a
// live host mid-bind, its entry not readable yet) is still a live host's
// whose entry cannot be read, once the retry has run out, and the scan does
// not wait much past it.
func TestAProbeStillCallsAHeldLockAlive(t *testing.T) {
	env := testEnv(t)
	id := NewHostID()
	_, lock := deadHost(t, env, id)
	holdLock(t, lock)

	start := time.Now()
	_, unreadable, err := HostsScan(env)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(unreadable, id) {
		t.Fatalf("a held lock over an unreadable entry was not called alive: unreadable %v", unreadable)
	}
	if took := time.Since(start); took > probeContendedFor+2*time.Second {
		t.Fatalf("the scan took %v", took)
	}
}
