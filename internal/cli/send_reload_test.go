package cli

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// craze send's reload past a host that is closing the session, and its
// refusals (plan 041 §3.4), with real hosts spawned as new_wait_test.go's are.

// TestSendReloadsASessionWhoseHostIsClosing is §3.4's "a live host whose
// attach is refused while closing": the session's host has taken a stop and
// refuses every attach closing while it is still listed; craze send waits it
// out — looking again until it has gone, spawning nothing meanwhile — and
// then reloads the session in a host of its own and runs its prompt there.
func TestSendReloadsASessionWhoseHostIsClosing(t *testing.T) {
	env, ws, kids := unattendedHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	id := "0199c0de-7777-7000-8000-000000007007"
	seedIndexRow(t, sessions.Row{SessionID: "fake-session-1", Provider: "cursor", CWD: absDir(ws), CrazeID: id,
		Unattended: &sessions.Unattended{PermissionMode: sessions.PermissionBypass}})
	h, err := fakehost.New(fakehost.Options{HostID: "0000000000c7", CrazeSessionID: id, Workspace: ws, Stop: true})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := h.Register(env)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- h.Serve(reg.Listener()) }()
	var once sync.Once
	end := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), serveStep)
			defer cancel()
			_ = h.Close(ctx)
			<-served
			_ = reg.Close()
		})
	}
	t.Cleanup(end)
	if stdout, stderr, code := executeErr([]string{"stop", "00007007"}); code != 0 {
		t.Fatalf("craze stop: exit %d, %q %q", code, stdout, stderr)
	}
	closing := make(chan struct{}, 1)
	prev := reloadClosing
	reloadClosing = func() {
		select {
		case closing <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { reloadClosing = prev })
	prevWait := reloadWait
	reloadWait = serveStep
	t.Cleanup(func() { reloadWait = prevWait })

	run := goCraze("send", "00007007", "--wait", "--json", "once it has gone")
	select {
	case <-closing:
	case <-time.After(serveStep):
		t.Fatal("craze send never met the closing host")
	}
	if n := kids.count(); n != 0 {
		t.Fatalf("%d hosts spawned while the closing host was listed", n)
	}
	end()
	stdout, stderr, code := run.wait(t)
	m := decodeResult(t, stdout)
	if code != 0 || m["status"] != "completed" || m["message"] != "echo: once it has gone" || m["session"] != id {
		t.Fatalf("craze send past the closing host: exit %d, %v, %q", code, m, stderr)
	}
	checkArgv(t, childArgv(t, kids.cmd(t, 0)), []string{"--load=" + id, "--unattended"}, nil)
}

// TestSendResolvesEndedSessionsOverTheIndex is §3.4's resolution: the newest
// row of each craze id, by the whole id, its last eight characters or a
// prefix of four or more; two matching is exit 1 naming both; a row with no
// craze id, and one of a provider craze cannot load again, are not
// addressable — `no session <id>`.
func TestSendResolvesEndedSessionsOverTheIndex(t *testing.T) {
	serveHome(t)
	a, b := "0199c0de-aaaa-7000-8000-0000aaaa1111", "0199c0de-bbbb-7000-8000-0000bbbb1111"
	seedIndexRow(t, sessions.Row{SessionID: "s-a-old", Provider: "cursor", CWD: "/w", CrazeID: a, Title: "older"})
	seedIndexRow(t, sessions.Row{SessionID: "s-a", Provider: "cursor", CWD: "/w", CrazeID: a, Title: "newer"})
	seedIndexRow(t, sessions.Row{SessionID: "s-b", Provider: "grok", CWD: "/v", CrazeID: b})
	// A provider craze cannot load again (unresumable): the hidden test one.
	_, unregister := agent.RegisterHiddenProviderForTest("unresumable-test", "Unresumable")
	t.Cleanup(unregister)
	seedIndexRow(t, sessions.Row{SessionID: "s-u", Provider: "unresumable-test", CWD: "/w", CrazeID: "0199c0de-cccc-7000-8000-0000cccc2222"})
	seedIndexRow(t, sessions.Row{SessionID: "legacy-1", Provider: "cursor", CWD: "/w"})
	for _, id := range []string{a, "aaaa1111", "0199c0de-aaaa"} {
		row, err := sendIndexRow(id, "")
		if err != nil || row.CrazeID != a || row.SessionID != "s-a" {
			t.Fatalf("%s: %+v, %v; want a's newest row", id, row, err)
		}
	}
	if _, err := sendIndexRow("1111", ""); err == nil ||
		err.Error() != "craze send: 2 ended sessions match 1111: aaaa1111 (cursor, /w), bbbb1111 (grok, /v)" {
		t.Fatalf("two matches: %v", err)
	}
	if row, err := sendIndexRow("1111", b); err != nil || row.CrazeID != b {
		t.Fatalf("one id taken: %+v, %v", row, err)
	}
	for _, id := range []string{"cccc2222", "legacy-1", "aaa"} {
		if _, err := sendIndexRow(id, ""); err == nil || err.Error() != "craze send: no session "+id {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

// TestSendToASessionThatCannotBeLoaded is §3.4's refusals: a session whose
// workspace is no longer a directory is exit 1 with the host's own words —
// one JSON object first under --json — and nothing is left running; an id
// neither a live host nor the index knows is `no session <id>`.
func TestSendToASessionThatCannotBeLoaded(t *testing.T) {
	env, _, _ := unattendedHome(t)
	gone := filepath.Join(t.TempDir(), "gone")
	id := "0199c0de-8888-7000-8000-000000008008"
	seedIndexRow(t, sessions.Row{SessionID: "fake-session-1", Provider: "cursor", CWD: gone, CrazeID: id})
	stdout, stderr, code := executeErr([]string{"send", "00008008", "--json", "hello"})
	want := "that session ran in " + gone + ", which is no longer a directory"
	if code != 1 || stdout != `{"error":{"message":"`+want+`"}}`+"\n" || stderr != "craze send: "+want+"\n" {
		t.Fatalf("craze send to a gone workspace: exit %d, %q %q", code, stdout, stderr)
	}
	if entries, err := rundir.Hosts(env); err != nil || len(entries) != 0 {
		t.Fatalf("the registry: %+v, %v", entries, err)
	}
	if stdout, stderr, code = executeErr([]string{"send", "nosuchid", "hello"}); code != 1 || stderr != "craze send: no session nosuchid\n" {
		t.Fatalf("craze send to no session: exit %d, %q %q", code, stdout, stderr)
	}
}
