package cli

import (
	"fmt"
	"strings"
	"syscall"
	"testing"

	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// A reload's host is never ended by the send that spawned it once it may be
// serving (C3 review r2, 1): not when that send gives up its handshake, and
// not by the host itself when its ready line goes untaken.

// TestASignalledReloadLeavesTheHostAnotherSendFound is the review's sequence:
// send A reloads an ended session; its host publishes itself and serves, its
// ready line not yet written (held: the child's "skip"), so A is still in its
// handshake. Send B finds the host in the registry and runs its prompt there.
// A is then signalled: it gives up its handshake — `cancelled`, nothing of
// its own sent — and leaves the host running: B's turn completed in it, and it
// still serves the session.
func TestASignalledReloadLeavesTheHostAnotherSendFound(t *testing.T) {
	_, ws := serveHome(t)
	writeCrazeConfig(t, fmt.Sprintf("host_idle_exit = \"30s\"\n\n[agents]\ncursor = %q\n", fakeAgentPath(t)))
	env := rundir.ProcessEnv()
	kids := spawnAsChild(t, func(n int) []string {
		if n == 1 {
			return []string{cliChildReady + "=skip"}
		}
		return nil
	})
	spawnBounds(t, serveStep, serveStep, serveStep)
	t.Cleanup(func() { stopEveryHost(t, env) })
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	id := "0199c0de-bbbb-7000-8000-00000000b00b"
	seedIndexRow(t, sessions.Row{SessionID: "fake-session-1", Provider: "cursor", CWD: absDir(ws), CrazeID: id,
		Unattended: &sessions.Unattended{PermissionMode: sessions.PermissionBypass}})
	sigs := withWaitSignals(t)

	a := goCraze("send", "0000b00b", "--wait", "--json", "from A")
	var e rundir.Entry
	waitFor(t, "A's host serving", func() bool {
		var ok bool
		e, ok = hostServing(env, id)
		return ok
	})
	stdout, stderr, code := executeErr([]string{"send", "0000b00b", "--wait", "--json", "from B"})
	m := decodeResult(t, stdout)
	if code != 0 || m["status"] != "completed" || m["message"] != "echo: from B" {
		t.Fatalf("send B in A's host: exit %d, %v, %q", code, m, stderr)
	}
	sigs <- syscall.SIGINT
	stdout, stderr, code = a.wait(t)
	m = decodeResult(t, stdout)
	if code != 3 || m["status"] != "cancelled" || m["error"] != "interrupted before the prompt was sent" {
		t.Fatalf("send A, signalled in its handshake: exit %d, %v, %q", code, m, stderr)
	}
	if !processAlive(e.PID) {
		t.Fatal("A's signal ended the host B's turn ran in")
	}
	if now, ok := hostServing(env, id); !ok || now.HostID != e.HostID {
		t.Fatalf("the session's host after A's signal: %+v %v", now, ok)
	}
	if lt := listRow(t, e).LastTurn; lt == nil || lt.TurnID != "turn-1" {
		t.Fatalf("B's turn in the host: %+v", lt)
	}
	if n := kids.count(); n != 1 {
		t.Fatalf("%d hosts spawned, want A's one", n)
	}
}

// TestAReloadHostServesOnWhenItsSpawnerGoes: a host spawned with
// --outlive-spawner (craze send's reload) whose spawner went before taking its
// ready line serves on — registered, its session there to attach to — where
// any other host stops (TestServeStopsWhenItsLauncherIsGone).
func TestAReloadHostServesOnWhenItsSpawnerGoes(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	r, _ := runServeReady(t, true, "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--outlive-spawner")
	e := r.waitServing(t, env, true)
	waitFor(t, "the untaken ready line", func() bool {
		return strings.Contains(r.stderr.String(), "craze serve: the launcher did not take the ready line")
	})
	if out := r.stderr.String(); strings.Contains(out, "stopping:") || !strings.Contains(out, "serving on") {
		t.Fatalf("the log: %s", out)
	}
	promptUnattached(t, e, "still here")
	if lt := waitLastTurn(t, e); lt.TurnID != "turn-1" {
		t.Fatalf("the host's turn: %+v", lt)
	}
	stopOver(t, dialServe(t, e), "1")
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve: %v", err)
	}
}
