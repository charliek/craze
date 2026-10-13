package hub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/version"
)

// Upgrading under a live hub (plan 046 §3.C, C-1..C-3): the hub whose own
// craze an upgrade removed says so, names its pid and serves on; a vanished
// workspace, which fails the same exec with the same ENOENT, is never taken
// for it; and a client names the remedy for a hub of another version.

// lacksWords is LacksError's text (plan 046 U3) for a hub of craze v, its pid
// pid, that cannot do what.
func lacksWords(v, what string, pid int) string {
	return fmt.Sprintf("this hub (craze %s) cannot %s; it exits once no session runs and no client is connected, "+
		"or stop it now with kill %d (no session ends)", v, what, pid)
}

// TestAHubWhoseCrazeIsGoneNamesTheRemedyAndStaysUp is C-1 (U1): a hub whose
// host command's path is gone — a Linuxbrew upgrade removed the versioned
// directory it re-executes — answers a create unavailable, reason
// spawn_failed, in words that name that path and its own pid to stop, and
// stays up: it serves on, a client that was connected before keeps its
// connection, the refused client keeps its own, and once the path is back a
// later create on that same connection, to that same hub, starts its session.
func TestAHubWhoseCrazeIsGoneNamesTheRemedyAndStaysUp(t *testing.T) {
	env := testEnv(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "cellar", "craze")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	s := hostsAsChildrenOf(t, env, link, always("ok"))
	rn := runWith(t, env, nil, creates("cursor"))
	sock := rn.line(t).Socket
	rn.serving(t)
	bystander := dial(t, sock)
	before := bystander.hello(t)
	c := dial(t, sock)
	c.hello(t)

	// The upgrade: the versioned directory goes, the link with it.
	if err := os.RemoveAll(filepath.Dir(link)); err != nil {
		t.Fatal(err)
	}
	e := refusedAs(t, createOn(t, c, map[string]any{"cwd": t.TempDir()}), protocol.CodeUnavailable, protocol.ReasonSpawnFailed)
	// The hub runs in this process: its pid, as its hello says it, is the
	// test's.
	want := fmt.Sprintf("this hub's craze (%s) was replaced by an upgrade: stop this hub with kill %d (no session ends) and run this again",
		link, before.Endpoint.PID)
	if before.Endpoint.PID != os.Getpid() || e.Message != want {
		t.Fatalf("the refusal says %q (hub pid %d), want %q", e.Message, before.Endpoint.PID, want)
	}
	if log := rn.stderr.String(); !strings.Contains(log, "craze at "+link+" is gone") || !strings.Contains(log, "no such file or directory") {
		t.Fatalf("the hub's log does not name the cause: %s", log)
	}

	// It stays up: still running, the bystander's connection answered, and a
	// new client meets the same hub.
	if !rn.isRunning() {
		t.Fatalf("the hub stopped (Run: %v); it said: %s", rn.err, rn.stderr)
	}
	if resp := bystander.call(t, protocol.MethodSessionsList, nil); resp.Error != nil {
		t.Fatalf("the bystander's sessions.list after the refusal: %v", resp.Error)
	}
	if again := dial(t, sock).hello(t); again.Endpoint.HostID != before.Endpoint.HostID || again.Endpoint.PID != before.Endpoint.PID {
		t.Fatalf("a new client met hub %s (pid %d), want the same %s (pid %d)",
			again.Endpoint.HostID, again.Endpoint.PID, before.Endpoint.HostID, before.Endpoint.PID)
	}

	// The path restored: a later create on the refused connection starts its
	// session on this same hub.
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	res := result(t, createOn(t, c, map[string]any{"cwd": t.TempDir()}))
	if res.Session.HostID == "" || res.Session.Status != protocol.RosterReachable {
		t.Fatalf("the create after the path came back answered %+v", res.Session)
	}
	if n := s.count(); n != 2 {
		t.Fatalf("%d host commands built, want 2 (the refused create's, then the one that started)", n)
	}
	if !rn.isRunning() {
		t.Fatalf("the hub stopped (Run: %v); it said: %s", rn.err, rn.stderr)
	}
}

// TestAVanishedWorkspaceIsNoUpgrade is C-2, the negative control for U1's
// stat: a create whose directory goes after its check, before its host's exec,
// fails that exec with the same ENOENT a gone executable gives — Setsid makes
// os/exec skip its own check of the directory — and is answered plain
// spawn_failed, never the upgrade's words, since the executable's path is
// still there.
func TestAVanishedWorkspaceIsNoUpgrade(t *testing.T) {
	env := testEnv(t)
	rn, s, sock := creating(t, env, nil, always("ok"))
	// The directory goes in the instant between the create's check and its
	// exec.
	setVar(t, &createStart, func(cmd *exec.Cmd, marker, groups, log string) (*hostspawn.Child, *os.File, error) {
		if err := os.Remove(cmd.Dir); err != nil {
			return nil, nil, fmt.Errorf("the test could not remove %s: %w", cmd.Dir, err)
		}
		return hostspawn.StartCmd(cmd, marker, groups, log)
	})
	c := dial(t, sock)
	c.hello(t)
	work := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	e := refusedAs(t, createOn(t, c, map[string]any{"cwd": work}), protocol.CodeUnavailable, protocol.ReasonSpawnFailed)
	if e.Message != "the session host cannot be started" {
		t.Fatalf("a vanished workspace is answered %q, want the plain spawn_failed", e.Message)
	}
	if s.count() != 1 {
		t.Fatalf("%d host commands built, want 1", s.count())
	}
	// The exec did fail on the directory, with ENOENT, and the hub took it
	// for nothing else.
	if log := rn.stderr.String(); !strings.Contains(log, "start the session host: fork/exec") ||
		!strings.Contains(log, "no such file or directory") || strings.Contains(log, "is gone") {
		t.Fatalf("the hub's log: %s", log)
	}
	if !rn.isRunning() {
		t.Fatalf("the hub stopped (Run: %v); it said: %s", rn.err, rn.stderr)
	}
}

// TestUpgradeRemedy is U2's rule: a spawn_failed from a hub of another craze
// version gains the remedy, naming the hub's pid and both versions; nothing
// else does — a hub of this craze's version (which names it itself), an
// answer that already names it (a later hub's U1), another refusal, a failure
// that is no refusal, and a hello with no pid or no version.
func TestUpgradeRemedy(t *testing.T) {
	older := hubResult(rundir.NewHubID(), "0.1.0", protocol.HubCapabilities())
	older.Endpoint.PID = 4242
	same := older
	same.Endpoint.CrazeVersion = version.Version
	noPID := older
	noPID.Endpoint.PID = 0
	noVersion := older
	noVersion.Endpoint.CrazeVersion = ""
	spawn := spawnFailed("the session host cannot be started")
	u1 := spawnFailed(hubReplaced, "/home/linuxbrew/.linuxbrew/Cellar/craze/0.2.0/bin/craze", 4242)
	want := "the hub (pid 4242) runs craze 0.1.0 and this is craze " + version.Version +
		": if craze was upgraded, stop that hub with kill 4242 (no session ends) and run this again"
	for _, tc := range []struct {
		name  string
		err   error
		hello protocol.HubHelloResult
		want  string
	}{
		{"an older hub's spawn_failed", spawn, older, want},
		{"wrapped", fmt.Errorf("hub: session.create: %w", spawn), older, want},
		{"a hub of this version", spawn, same, ""},
		{"an answer naming the remedy", u1, older, ""},
		{"another refusal", startFailed("the agent could not start"), older, ""},
		{"no refusal", errors.New("dial: no such file"), older, ""},
		{"no pid", spawn, noPID, ""},
		{"no version", spawn, noVersion, ""},
		{"no hello", spawn, protocol.HubHelloResult{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := UpgradeRemedy(tc.err, tc.hello); got != tc.want {
				t.Fatalf("UpgradeRemedy = %q, want %q", got, tc.want)
			}
		})
	}
	if !strings.Contains(u1.Message, hubReplacedMark) {
		t.Fatalf("the hub's own remedy %q does not carry the mark %q a client tells it by", u1.Message, hubReplacedMark)
	}
}

// TestEnsureHelloAnswersTheHubsHello (U2's plumbing): EnsureHello answers the
// socket Ensure would and the hello of the hub found there — its version and
// its pid, which may not be its record's; with an error, none.
func TestEnsureHelloAnswersTheHubsHello(t *testing.T) {
	env := processEnv(t)
	noCommand(t)
	pid := sleeper(t)
	hubID := rundir.NewHubID()
	hello := hubResult(hubID, "0.9.1", protocol.HubCapabilities())
	hello.Endpoint.PID = 5150
	d := newDecoy(t, decoyAnswer, hello)
	writeRecord(t, env, rundir.HubRecord{HubID: hubID, PID: pid, StartToken: token(t, pid), Socket: d.path})
	ctx, cancel := context.WithTimeout(context.Background(), step)
	defer cancel()
	sock, got, err := EnsureHello(ctx, env, protocol.ConnectionCapabilities{SessionCreate: true})
	if err != nil || sock != d.path || got.Endpoint.CrazeVersion != "0.9.1" || got.Endpoint.PID != 5150 || got.Endpoint.HostID != hubID {
		t.Fatalf("EnsureHello = %q, %+v, %v; want %s and the decoy's hello", sock, got.Endpoint, err, d.path)
	}
	_, got, err = EnsureHello(ctx, env, protocol.ConnectionCapabilities{CreateOptions: true})
	var lacks *LacksError
	if !errors.As(err, &lacks) || lacks.PID != 5150 || got != (protocol.HubHelloResult{}) {
		t.Fatalf("EnsureHello of a hub that lacks the need = %+v, %v; want LacksError with its pid, and no hello", got, err)
	}
}
