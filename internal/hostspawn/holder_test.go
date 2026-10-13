package hostspawn

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/craze/internal/rundir"
)

// The stopping-aware wait for a session's holder (holder.go, plan 046 B3):
// what names a holder, when it has gone, and AwaitHolder's every answer, over
// a registry of the test's own whose entries are bound in this process.

// holderEnv is a registry of the test's own: a 0700 HOME and a short runtime
// directory.
func holderEnv(t *testing.T) rundir.Env {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeDir, err := os.MkdirTemp("/tmp", "czh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	return rundir.Env{Home: home, CrazeDir: filepath.Join(home, ".craze"), CrazeRuntimeDir: runtimeDir, EUID: os.Geteuid()}
}

// bindEntry registers host id serving session sid (provider p, provider
// session psid) in env, unregistered when the test ends.
func bindEntry(t *testing.T, env rundir.Env, id, sid, p, psid string) *rundir.Host {
	t.Helper()
	h, err := rundir.Bind(env, id, rundir.Entry{StartedAt: time.Now(), CrazeSessionID: sid, Provider: p, ProviderSessionID: psid})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// sleeping is a `sleep 60` of the test's own: its pid and its start, and
// end, which kills it and reaps it.
func sleeping(t *testing.T) (pid int, start uint64, end func()) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	end = func() {
		select {
		case <-done:
		default:
			_ = cmd.Process.Kill()
			<-done
		}
	}
	t.Cleanup(end)
	id, err := rundir.ProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid, id.Start, end
}

// TestAHolderServesItsSession: an entry serves a holder's session by its craze
// id, or by its provider and provider session id, whatever its mark; an entry
// that names no session yet serves none.
func TestAHolderServesItsSession(t *testing.T) {
	byID := Holder{CrazeSessionID: "s-1"}
	legacy := Holder{Provider: "grok", ProviderSessionID: "p-1"}
	for _, tc := range []struct {
		h    Holder
		e    rundir.Entry
		want bool
	}{
		{byID, rundir.Entry{CrazeSessionID: "s-1"}, true},
		{byID, rundir.Entry{CrazeSessionID: "s-1", Stopping: true}, true},
		{byID, rundir.Entry{CrazeSessionID: "s-2"}, false},
		{legacy, rundir.Entry{CrazeSessionID: "s-9", Provider: "grok", ProviderSessionID: "p-1"}, true},
		{legacy, rundir.Entry{CrazeSessionID: "s-9", Provider: "cursor", ProviderSessionID: "p-1"}, false},
		{legacy, rundir.Entry{CrazeSessionID: "s-9", Provider: "grok"}, false},
		{legacy, rundir.Entry{Provider: "grok", ProviderSessionID: "p-1"}, false},
		{Holder{}, rundir.Entry{CrazeSessionID: "s-1"}, false},
	} {
		if got := tc.h.Serves(tc.e); got != tc.want {
			t.Errorf("%+v serves %+v: %v, want %v", tc.h, tc.e, got, tc.want)
		}
	}
}

// TestAHolderIsGoneOnceItsProcessIs: a holder with no pid is never gone; a
// live one is not; one whose pid no process has, whose process is a zombie,
// or whose pid another process started at another instant has, is.
func TestAHolderIsGoneOnceItsProcessIs(t *testing.T) {
	pid, start, end := sleeping(t)
	if (Holder{}).Gone() || (Holder{PID: pid}).Gone() || (Holder{PID: pid, PIDStart: start}).Gone() {
		t.Fatal("a holder with no pid, or a live one, is gone")
	}
	if !(Holder{PID: pid, PIDStart: start + 1}).Gone() {
		t.Fatal("a holder whose pid another process has is not gone")
	}
	end()
	if !(Holder{PID: pid}).Gone() {
		t.Fatal("a reaped holder is not gone")
	}
	zombie := exec.Command("true")
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = zombie.Wait() })
	deadline := time.Now().Add(testStep)
	for !(Holder{PID: zombie.Process.Pid}).Gone() {
		if time.Now().After(deadline) {
			t.Fatal("an exited, unreaped holder is not gone")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := (Holder{HostID: "0123456789ab", PID: 77}).Name(); got != "host 0123456789ab (pid 77)" {
		t.Fatalf("Name = %q", got)
	}
	if got := (Holder{}).Name(); got != "a host" {
		t.Fatalf("an unnamed holder's Name = %q", got)
	}
}

// TestAwaitHolder (plan 046 B3): AwaitHolder answers a live entry serving the
// session and not marked stopping; waits out one marked stopping, and a
// leaving holder's own entry marked or not, until the holder's process has
// gone (ErrHolderGone), or the deadline passes (ErrHolderStill); answers
// another host's serving entry while it waits; gives an unnamed holder a
// short wait of its own; and stops at its context's end.
func TestAwaitHolder(t *testing.T) {
	env := holderEnv(t)
	ctx := context.Background()
	soon := func() time.Time { return time.Now().Add(testStep) }

	bindEntry(t, env, "00000000000a", "s-1", "cursor", "p-1")
	if e, err := AwaitHolder(ctx, env, Holder{CrazeSessionID: "s-1"}, soon()); err != nil || e.HostID != "00000000000a" {
		t.Fatalf("a serving entry: %+v, %v", e, err)
	}

	stopping := bindEntry(t, env, "00000000000b", "s-2", "cursor", "p-2")
	if err := stopping.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	pid, start, end := sleeping(t)
	if _, err := AwaitHolder(ctx, env, Holder{CrazeSessionID: "s-2", PID: pid, PIDStart: start}, time.Now().Add(200*time.Millisecond)); !errors.Is(err, ErrHolderStill) {
		t.Fatalf("a stopping holder through the deadline: %v, want ErrHolderStill", err)
	}
	polled := make(chan struct{}, 1)
	prev := AwaitPolled
	AwaitPolled = func() {
		select {
		case polled <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { AwaitPolled = prev })
	got := make(chan error, 1)
	go func() {
		_, err := AwaitHolder(ctx, env, Holder{CrazeSessionID: "s-2", PID: pid, PIDStart: start}, soon())
		got <- err
	}()
	<-polled
	end()
	if err := <-got; !errors.Is(err, ErrHolderGone) {
		t.Fatalf("a stopping holder that went: %v, want ErrHolderGone", err)
	}

	// A leaving holder's own entry, unmarked, is never the answer.
	pid, start, end = sleeping(t)
	bindEntry(t, env, "00000000000c", "s-3", "cursor", "p-3")
	leaving := Holder{CrazeSessionID: "s-3", HostID: "00000000000c", PID: pid, PIDStart: start, Leaving: true}
	if _, err := AwaitHolder(ctx, env, leaving, time.Now().Add(200*time.Millisecond)); !errors.Is(err, ErrHolderStill) {
		t.Fatalf("a leaving holder's own entry: %v, want ErrHolderStill", err)
	}
	if e, err := AwaitHolder(ctx, env, Holder{CrazeSessionID: "s-3", HostID: "00000000000c", PID: pid, PIDStart: start}, soon()); err != nil || e.HostID != "00000000000c" {
		t.Fatalf("the same entry for a holder not leaving: %+v, %v", e, err)
	}
	// Another host serving the session ends the wait: it claimed it.
	bindEntry(t, env, "00000000000d", "s-3", "cursor", "p-3")
	if e, err := AwaitHolder(ctx, env, leaving, soon()); err != nil || e.HostID != "00000000000d" {
		t.Fatalf("another host serving the session: %+v, %v", e, err)
	}
	end()

	prevUnnamed := unnamedHolderWait
	unnamedHolderWait = 50 * time.Millisecond
	t.Cleanup(func() { unnamedHolderWait = prevUnnamed })
	if _, err := AwaitHolder(ctx, env, Holder{CrazeSessionID: "s-9"}, soon()); !errors.Is(err, ErrHolderGone) {
		t.Fatalf("an unnamed holder with no entry: %v, want ErrHolderGone", err)
	}

	pid, start, _ = sleeping(t)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := AwaitHolder(cctx, env, Holder{CrazeSessionID: "s-9", PID: pid, PIDStart: start}, soon()); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait: %v", err)
	}
}

// TestTransientRefusals: a host's index busy or changed is a claim to try
// again; any other refusal is not.
func TestTransientRefusals(t *testing.T) {
	for msg, want := range map[string]bool{
		"craze: " + IndexBusy:         true,
		"craze: " + IndexChanged:      true,
		"craze serve: no session s-1": false,
		"":                            false,
	} {
		if got := TransientRefusal(msg); got != want {
			t.Errorf("TransientRefusal(%q) = %v, want %v", msg, got, want)
		}
	}
}

// TestAHeldHolderTakesItsStartFromItsEntry (C7 review, item 4): a holder
// named by a held answer — its pid alone, no start — whose own entry (the
// same host id and pid, marked stopping) records its start takes that start:
// a pid now another process's, started at another instant, is the holder
// gone, ErrHolderGone at once rather than a wait to the deadline. The control:
// an entry of another host id, or another pid, lends no start, and the live
// process at the pid keeps the wait to its deadline.
func TestAHeldHolderTakesItsStartFromItsEntry(t *testing.T) {
	env := holderEnv(t)
	ctx := context.Background()
	pid, start, _ := sleeping(t)
	h := bindEntry(t, env, "0000000000e1", "s-7", "cursor", "p-7")
	if err := h.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	// The entry says the holder started at another instant than the process
	// with its pid now: the holder exited, and its pid was given again.
	setEntry(t, env, "0000000000e1", pid, start+1, time.Now())
	for _, tc := range []struct {
		name string
		h    Holder
		want error
	}{
		{"its own entry", Holder{CrazeSessionID: "s-7", HostID: "0000000000e1", PID: pid}, ErrHolderGone},
		{"another host's entry", Holder{CrazeSessionID: "s-7", HostID: "0000000000e2", PID: pid}, ErrHolderStill},
		{"another pid's entry", Holder{CrazeSessionID: "s-7", HostID: "0000000000e1", PID: os.Getpid()}, ErrHolderStill},
	} {
		if _, err := AwaitHolder(ctx, env, tc.h, time.Now().Add(300*time.Millisecond)); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestAStaleHolderWaitsForItsEntryToChange (C7 review r2, item 1): a Stale
// holder's own entry, read as it was, is never the answer — the wait runs to
// its deadline while it reads the same (ErrHolderStill), whatever the poll
// count — and its changing ends the wait: rewritten to another session,
// marked stopping, or gone, ErrHolderChanged; rewritten to another
// incarnation of the session, that entry the answer. Another host serving the
// session is the answer, the holder's entry unchanged.
func TestAStaleHolderWaitsForItsEntryToChange(t *testing.T) {
	env := holderEnv(t)
	ctx := context.Background()
	short := func() time.Time { return time.Now().Add(200 * time.Millisecond) }
	soon := func() time.Time { return time.Now().Add(testStep) }
	// bind lists host id serving s-1, and is the holder its entry, read now,
	// names: Stale, as read.
	bind := func(id string) (*rundir.Host, Holder) {
		t.Helper()
		h, err := rundir.Bind(env, id, rundir.Entry{StartedAt: time.Now(), CrazeSessionID: "s-1", Incarnation: "inc-1",
			Provider: "cursor", ProviderSessionID: "p-1"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close() })
		entries, err := rundir.Hosts(env)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.HostID == id {
				holder := HolderOfEntry(e)
				holder.Stale = &e
				return h, holder
			}
		}
		t.Fatalf("host %s is not listed", id)
		return nil, Holder{}
	}
	h, stale := bind("0000000000f1")
	if _, err := AwaitHolder(ctx, env, stale, short()); !errors.Is(err, ErrHolderStill) {
		t.Fatalf("its entry as it was read: %v, want ErrHolderStill", err)
	}
	if err := h.Update(func(e *rundir.Entry) { e.Incarnation = "inc-2" }); err != nil {
		t.Fatal(err)
	}
	if e, err := AwaitHolder(ctx, env, stale, soon()); err != nil || e.HostID != "0000000000f1" || e.Incarnation != "inc-2" {
		t.Fatalf("its entry rewritten to another incarnation of the session: %+v, %v", e, err)
	}
	if err := h.Update(func(e *rundir.Entry) { e.CrazeSessionID, e.Incarnation, e.ProviderSessionID = "s-2", "inc-3", "p-2" }); err != nil {
		t.Fatal(err)
	}
	if _, err := AwaitHolder(ctx, env, stale, soon()); !errors.Is(err, ErrHolderChanged) {
		t.Fatalf("its entry rewritten to another session: %v, want ErrHolderChanged", err)
	}

	h, stale = bind("0000000000f2")
	if err := h.MarkStopping(); err != nil {
		t.Fatal(err)
	}
	if _, err := AwaitHolder(ctx, env, stale, soon()); !errors.Is(err, ErrHolderChanged) {
		t.Fatalf("its entry marked stopping: %v, want ErrHolderChanged", err)
	}

	h, stale = bind("0000000000f3")
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := AwaitHolder(ctx, env, stale, soon()); !errors.Is(err, ErrHolderChanged) {
		t.Fatalf("its entry gone: %v, want ErrHolderChanged", err)
	}

	_, stale = bind("0000000000f4")
	bindEntry(t, env, "0000000000f5", "s-1", "cursor", "p-1")
	if e, err := AwaitHolder(ctx, env, stale, soon()); err != nil || e.HostID != "0000000000f5" {
		t.Fatalf("another host serving the session: %+v, %v", e, err)
	}
}

// TestAWaitNeverAnswersAnEntryItsWaiterSkips (C7 review r3): an entry Skip
// says its waiter would not take — a reopen's host looked past as it was, or
// one it knows is leaving — is never AwaitHolder's answer, so the wait polls
// to its deadline rather than ending at once on it (ErrHolderStill), whatever
// kind of holder it waits for; the same entry not skipped is the answer (the
// control). And a Stale holder's own entry that a read cannot read says
// nothing: the wait polls on (ErrHolderStill), never ErrHolderChanged.
func TestAWaitNeverAnswersAnEntryItsWaiterSkips(t *testing.T) {
	env := holderEnv(t)
	ctx := context.Background()
	short := func() time.Time { return time.Now().Add(200 * time.Millisecond) }
	bindEntry(t, env, "0000000000a1", "s-1", "cursor", "p-1")
	bindEntry(t, env, "0000000000a2", "s-1", "cursor", "p-1")
	listed, err := rundir.Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	var stale rundir.Entry
	for _, e := range listed {
		if e.HostID == "0000000000a2" {
			stale = e
		}
	}
	skip := func(e rundir.Entry) bool { return e.HostID == "0000000000a1" || e.HostID == "0000000000a2" }
	pid, start, _ := sleeping(t)
	for _, h := range []Holder{
		{CrazeSessionID: "s-1", HostID: "0000000000a9", PID: pid, PIDStart: start, Leaving: true},
		{CrazeSessionID: "s-1", HostID: "0000000000a9", PID: pid, PIDStart: start},
		{CrazeSessionID: "s-1", HostID: stale.HostID, PID: pid, PIDStart: start, Stale: &stale},
	} {
		h.Skip = skip
		if e, err := AwaitHolder(ctx, env, h, short()); !errors.Is(err, ErrHolderStill) {
			t.Errorf("a skipped entry, waiting for %+v: %+v, %v; want ErrHolderStill", h, e, err)
		}
	}
	if e, err := AwaitHolder(ctx, env, Holder{CrazeSessionID: "s-1", PID: pid, PIDStart: start,
		Skip: func(e rundir.Entry) bool { return e.HostID == "0000000000a2" }}, short()); err != nil || e.HostID != "0000000000a1" {
		t.Fatalf("the entry not skipped: %+v, %v", e, err)
	}

	h := bindEntry(t, env, "0000000000a3", "s-3", "cursor", "p-3")
	listed, err = rundir.Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	var own rundir.Entry
	for _, e := range listed {
		if e.HostID == h.ID() {
			own = e
		}
	}
	holder := HolderOfEntry(own)
	holder.Stale = &own
	if err := os.WriteFile(filepath.Join(env.Home, ".cache", "craze", "hosts", h.ID()+".json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AwaitHolder(ctx, env, holder, short()); !errors.Is(err, ErrHolderStill) {
		t.Fatalf("a Stale holder whose entry cannot be read: %v, want ErrHolderStill", err)
	}
}
