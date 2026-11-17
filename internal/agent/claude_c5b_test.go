package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/errkind"
)

// Fixes from the astra review of C5 (review C5 items 1–4, 6, 8, 9) and the L3
// live smoke (F1).

// blockedWriter is a diagnostic sink that never returns until released.
type blockedWriter struct{ release chan struct{} }

func (w *blockedWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

// TestClaudeOverageTripsUnderBackpressure (review C5 item 1): with the
// session's publication stalled — the event channel full, nothing draining —
// and its diagnostic sink blocked, a trip still writes the record, fences and
// kills the agent: nothing the trip does before the stop waits on a reader.
func TestClaudeOverageTripsUnderBackpressure(t *testing.T) {
	for _, where := range []string{"MID", "ERR"} {
		t.Run(where, func(t *testing.T) {
			record, stopped := overageRig(t, 150*time.Millisecond)
			script := "claude-overage"
			if where == "ERR" {
				script = "claude-errors"
			}
			r := newClaudeRig(t, script, Options{Interactive: true}, "CRAZE_FAKE_CLAUDE_OVERAGE=rejected")
			diag := &blockedWriter{release: make(chan struct{})}
			r.s.opts.Diag = diag
			var stall atomic.Bool
			release := make(chan struct{})
			r.s.log.hooks = &logHooks{outboxAdmitting: func(int) {
				if stall.Load() {
					<-release
				}
			}}
			var once sync.Once
			free := func() { once.Do(func() { close(release); close(diag.release) }) }
			t.Cleanup(free)
			if err := r.s.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			// Drain what the start published, and a turn's worth after it —
			// the adapter's setup traffic is read by then — then stall every
			// later delta.
			go func() {
				for range r.s.Events() {
				}
			}()
			// (claude-errors fails every turn: an unknown errorKind, no trip.)
			_, _ = r.s.Prompt(t.Context(), "hello")
			if err := r.s.log.Flush(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			stall.Store(true)
			prompt := "MID"
			if where == "ERR" {
				prompt = "ERR rate_limit TRIP"
			}
			go func() { _, _ = r.s.Prompt(context.Background(), prompt) }()
			waitStopped(t, r, stopped, 5*time.Second)
			readRecord(t, record)
			free()
		})
	}
}

// TestClaudeOverageRecordLockedAcrossWriters (review C5 item 2): the record's
// read, comparison and replacement are one transaction under a file lock, so
// a writer that read an older record cannot replace a later hold another
// writer has recorded meanwhile.
func TestClaudeOverageRecordLockedAcrossWriters(t *testing.T) {
	record, _ := overageRig(t, 0)
	now := time.Now().UTC().Truncate(time.Second)
	short := overageRecord{Until: now.Add(time.Hour), Window: "five_hour", At: now}
	long := overageRecord{Until: now.Add(7 * 24 * time.Hour), Window: "seven_day", At: now}
	parked := make(chan struct{})
	release := make(chan struct{})
	prev := testOverageAfterRead
	testOverageAfterRead = func(rec overageRecord) {
		if rec.Window == "five_hour" {
			close(parked)
			<-release
		}
	}
	t.Cleanup(func() { testOverageAfterRead = prev })
	first := make(chan error, 1)
	go func() { first <- writeOverageRecordAt(record, short) }()
	<-parked
	second := make(chan error, 1)
	go func() { second <- writeOverageRecordAt(record, long) }()
	select {
	case err := <-second:
		t.Fatalf("a second writer went through while the first held the record (%v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if got := readRecord(t, record); !got.Until.Equal(long.Until) || got.Window != "seven_day" {
		t.Fatalf("record %+v; want the week's hold", got)
	}
	// And a shorter one after it keeps the week's.
	if err := writeOverageRecordAt(record, overageRecord{Until: now.Add(2 * time.Hour), Window: "five_hour", At: now}); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(t, record); !got.Until.Equal(long.Until) {
		t.Fatalf("record %+v after a shorter trip; want the week's hold kept", got)
	}
}

// TestClaudeOverageRecordFallsBack (review C5 item 3): a craze directory that
// will not take the record sends it to the fallback, which every check reads,
// so a new session is refused all the same; with neither, the session that
// tripped still stops, and its error says the hold was not recorded.
func TestClaudeOverageRecordFallsBack(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, stopped := overageRig(t, 150*time.Millisecond)
	fallback := filepath.Join(t.TempDir(), "fallback.json")
	overageMu.Lock()
	prevPath, prevFallback := overagePath, overageFallback
	overagePath = func() string { return filepath.Join(notADir, "claude-overage.json") }
	overageFallback = func(string) string { return fallback }
	overageMu.Unlock()
	t.Cleanup(func() {
		overageMu.Lock()
		overagePath, overageFallback = prevPath, prevFallback
		overageMu.Unlock()
	})
	r, _ := startClaude(t, "claude-overage", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "MID"); !isQuota(err) {
		t.Fatalf("the trip: %v", err)
	}
	waitStopped(t, r, stopped, 5*time.Second)
	readRecord(t, fallback)
	n := newClaudeRig(t, "claude-basic", Options{})
	if err := n.s.Start(t.Context()); !isQuota(err) || !strings.Contains(err.Error(), fallback) {
		t.Fatalf("a start with the hold in the fallback: %v", err)
	}

	// Neither takes it.
	if err := os.Remove(fallback); err != nil {
		t.Fatal(err)
	}
	overageMu.Lock()
	// A fallback whose directory cannot be made (its parent is missing):
	// nowhere to write, and nothing there to read.
	overageFallback = func(string) string { return filepath.Join(t.TempDir(), "missing", "craze", "fallback.json") }
	overageMu.Unlock()
	stopped2 := overageStops(t)
	r2, _ := startClaude(t, "claude-overage", Options{Interactive: true})
	_, err := r2.s.Prompt(t.Context(), "MID")
	if !isQuota(err) || !strings.Contains(err.Error(), "could not record the hold") {
		t.Fatalf("a trip with nowhere to record it: %v", err)
	}
	waitStopped(t, r2, stopped2, 5*time.Second)
}

// TestClaudeOverageLaterTripExtendsTheHold (review C5 item 4): a trip with no
// reset time records an hour; a second one inside the stop's grace, whose
// reset is a week away, extends the record to it.
func TestClaudeOverageLaterTripExtendsTheHold(t *testing.T) {
	record, stopped := overageRig(t, 600*time.Millisecond)
	r, _ := startClaude(t, "claude-overage", Options{Interactive: true})
	start := time.Now()
	if _, err := r.s.Prompt(t.Context(), "TWO-TRIPS"); !isQuota(err) {
		t.Fatalf("the trips: %v", err)
	}
	waitStopped(t, r, stopped, 5*time.Second)
	if got := readRecord(t, record); got.Until.Before(start.Add(6 * 24 * time.Hour)) {
		t.Fatalf("record %+v; want the later trip's week", got)
	}
}

// processGone reports whether pid has ended: no such process, or a zombie
// waiting for its reaper.
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
		return true
	}
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return true
		}
		i := strings.LastIndexByte(string(b), ')')
		return i >= 0 && i+2 < len(b) && b[i+2] == 'Z'
	}
	out, err := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// TestClaudeOverageKillsChildrenThatLeftTheGroup (review C5 item 6): the
// adapter's real OS children — one in its process group, one in a session of
// its own, as a background command that detached would be — are dead once
// the stop has run.
func TestClaudeOverageKillsChildrenThatLeftTheGroup(t *testing.T) {
	_, stopped := overageRig(t, 150*time.Millisecond)
	pids := filepath.Join(t.TempDir(), "pids")
	r, _ := startClaude(t, "claude-overage", Options{Interactive: true}, "CRAZE_FAKE_CHILD_PIDS="+pids)
	if _, err := r.s.Prompt(t.Context(), "ESCAPE"); !isQuota(err) {
		t.Fatalf("the trip: %v", err)
	}
	waitStopped(t, r, stopped, 5*time.Second)
	raw, err := os.ReadFile(pids)
	if err != nil {
		t.Fatal(err)
	}
	children := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			n, _ := strconv.Atoi(f[1])
			children[f[0]] = n
			t.Cleanup(func() { _ = syscall.Kill(n, syscall.SIGKILL) })
		}
	}
	if children["group"] == 0 || children["escaped"] == 0 {
		t.Fatalf("children %v", children)
	}
	for name, pid := range children {
		deadline := time.Now().Add(5 * time.Second)
		for !processGone(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("the %s child (pid %d) outlived the stop", name, pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// TestClaudeOverageStopsTheSession (review C5 item 8): an idle trip says why
// with an error event once the agent is gone, and asks the session's host to
// stop, with the overage cause.
func TestClaudeOverageStopsTheSession(t *testing.T) {
	_, stopped := overageRig(t, 150*time.Millisecond)
	type stop struct{ cause, why string }
	asked := make(chan stop, 2)
	opts := Options{Interactive: true, Stopped: func(cause, why string) { asked <- stop{cause, why} }}
	r, log := startClaude(t, "claude-overage", opts, "CRAZE_FAKE_CLAUDE_IDLE_TRIP=100ms")
	waitStopped(t, r, stopped, 5*time.Second)
	select {
	case s := <-asked:
		if s.cause != StopCauseOverage || !strings.Contains(s.why, "extra usage") {
			t.Fatalf("the host was asked to stop with %+v", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the host was never asked to stop")
	}
	waitFor(t, "the error event", func() bool {
		return firstIndex(log.snapshot(), func(ev Event) bool {
			return ev.Type == EventError && ErrorInfo(ev.Err).Kind == errkind.Quota
		}) >= 0
	})
}

// TestClaudeOverageTripBeforeAFailedSetup (review C5 item 9): a trip an
// adapter sends before session/new's reply is recorded even though the setup
// then fails, and the start fails with it.
func TestClaudeOverageTripBeforeAFailedSetup(t *testing.T) {
	record, stopped := overageRig(t, 150*time.Millisecond)
	r := newClaudeRig(t, "claude-overage", Options{}, "CRAZE_FAKE_CLAUDE_SETUP_TRIP=1")
	if err := r.s.Start(t.Context()); !isQuota(err) {
		t.Fatalf("the start: %v", err)
	}
	readRecord(t, record)
	// The stop runs to its end (and is done with the test's seams).
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop never ran")
	}
}

// TestClaudeInUseModelFollowsTheOptions (L3 F1): the model in use is the last
// report's while the mode and model options are the ones it was reported
// under, and cleared — published so — when either changes.
func TestClaudeInUseModelFollowsTheOptions(t *testing.T) {
	_, _ = overageRig(t, 150*time.Millisecond)
	r, _ := startClaude(t, "claude-usage", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "count PLAN-SONNET"); err != nil {
		t.Fatal(err)
	}
	if u := r.s.Snapshot().Usage; u == nil || u.Model != "claude-sonnet-5-5" {
		t.Fatalf("usage %+v; want sonnet in use", u)
	}
	if _, err := r.s.SetConfig(t.Context(), "", "model", "haiku", ""); err != nil {
		t.Fatal(err)
	}
	if u := r.s.Snapshot().Usage; u == nil || u.Model != "" {
		t.Fatalf("usage %+v after the model changed; want no model in use", u)
	}
	if _, err := r.s.Prompt(t.Context(), "count PLAN-SONNET"); err != nil {
		t.Fatal(err)
	}
	if u := r.s.Snapshot().Usage; u == nil || u.Model != "claude-sonnet-5-5" {
		t.Fatalf("usage %+v; want sonnet in use under haiku", u)
	}
	if _, err := r.s.SetConfig(t.Context(), "", "mode", "plan", ""); err != nil {
		t.Fatal(err)
	}
	if u := r.s.Snapshot().Usage; u == nil || u.Model != "" {
		t.Fatalf("usage %+v after the mode changed; want no model in use", u)
	}
}

var _ = acp.ErrFenced
