package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// The roster read once (plan 032 §3.12, craze ps): List from a hub, Direct
// with none. The hosts are in-process fake hosts listed in a registry of the
// test's own (hostIn); the hub runs in process (newRosterRig). Each test here
// is also run under a 5% CPU quota.

var listWho = protocol.ClientInfo{Kind: "test", Name: "the roster read once"}

// TestListAnswersTheHubsRoster: List says hello and asks sessions.list, and
// answers the result as the hub wrote it — its epoch the hub's id, a row per
// listed host, in host-id order. The negative control is
// TestListRefusesWhatIsNotAHub.
func TestListAnswersTheHubsRoster(t *testing.T) {
	setVar(t, &listWait, step)
	env := testEnv(t)
	hostIn(t, env, 2)
	hostIn(t, env, 1)
	rg := newRosterRig(t, env, rigOpts{})
	raw, err := List(stepContext(t), rg.sock, listWho)
	if err != nil {
		t.Fatal(err)
	}
	var res protocol.HubSessionsListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Epoch != rg.h.id {
		t.Fatalf("the roster's epoch %q, want the hub's id %q", res.Epoch, rg.h.id)
	}
	var ids []string
	for _, r := range res.Sessions {
		ids = append(ids, r.HostID+"/"+r.SessionID+"/"+string(r.Status))
		if len(r.Row) == 0 {
			t.Errorf("host %s's row was not forwarded", r.HostID)
		}
	}
	want := []string{hostOf(1) + "/" + sessionOf(1) + "/reachable", hostOf(2) + "/" + sessionOf(2) + "/reachable"}
	if !slices.Equal(ids, want) {
		t.Fatalf("the roster lists %v, want %v", ids, want)
	}
}

// TestListRefusesWhatIsNotAHub: a session's host answers hello as a host,
// which List does not take for the hub; a socket that never answers ends at
// List's context.
func TestListRefusesWhatIsNotAHub(t *testing.T) {
	env := testEnv(t)
	hostIn(t, env, 1)
	_, err := List(stepContext(t), entryOf(t, env, hostOf(1)).Socket, listWho)
	if err == nil || !strings.Contains(err.Error(), `kind "host"`) {
		t.Fatalf("List of a host's socket = %v, want a refusal naming its kind", err)
	}

	sock := filepath.Join(env.CrazeRuntimeDir, "mute.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := List(ctx, sock, listWho); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("List of a socket that never answers = %v, want its context's end", err)
	}
	if took := time.Since(start); took > step {
		t.Fatalf("List outlived its context by far: %v", took)
	}
}

// TestDirectIsTheHubsRoster: with no hub, Direct reads the hosts as a hub
// would and lists the same rows — every member, the host's row byte for
// byte — with no epoch and no cursor; a registry entry with no craze session
// id is listed by neither. The negative control: a host started after the
// hub's answer is in Direct's alone.
func TestDirectIsTheHubsRoster(t *testing.T) {
	setVar(t, &listWait, step)
	setVar(t, &directBudget, step)
	env := testEnv(t)
	hostIn(t, env, 1)
	hostIn(t, env, 2)
	bare, err := rundir.Bind(env, rundir.NewHostID(), rundir.Entry{Ready: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bare.Close() })
	rg := newRosterRig(t, env, rigOpts{})
	raw, err := List(stepContext(t), rg.sock, listWho)
	if err != nil {
		t.Fatal(err)
	}
	var fromHub protocol.HubSessionsListResult
	if err := json.Unmarshal(raw, &fromHub); err != nil {
		t.Fatal(err)
	}
	direct, err := Direct(stepContext(t), env, listWho)
	if err != nil {
		t.Fatal(err)
	}
	if direct.Epoch != "" || direct.Cursor != 0 || direct.Truncated {
		t.Fatalf("Direct's roster says it is a hub's: %+v", direct)
	}
	same := func(a, b []protocol.RosterRow) bool {
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		return string(ja) == string(jb)
	}
	if len(direct.Sessions) != 2 || !same(direct.Sessions, fromHub.Sessions) {
		t.Fatalf("Direct lists\n%+v\nthe hub\n%+v", direct.Sessions, fromHub.Sessions)
	}

	hostIn(t, env, 3)
	direct, err = Direct(stepContext(t), env, listWho)
	if err != nil {
		t.Fatal(err)
	}
	if len(direct.Sessions) != 3 || direct.Sessions[2].HostID != hostOf(3) || same(direct.Sessions, fromHub.Sessions) {
		t.Fatalf("a host started since is not in Direct's roster: %+v", direct.Sessions)
	}
}

// TestDirectWaitsForEveryHostOnce: Direct answers once every host has been
// heard from — a host that fails its attempt (its socket accepts and closes
// at once) is heard from, unreachable and approximate, long before Direct's
// context ends — and a host whose attempt has not come back when the
// context ends (its socket accepts and says nothing) is listed as it stands,
// connecting. The negative control is the first host: answered, reachable.
func TestDirectWaitsForEveryHostOnce(t *testing.T) {
	setVar(t, &directBudget, step)
	env := testEnv(t)
	hostIn(t, env, 1)
	bound := func(n int, serve func(net.Conn)) {
		reg, err := rundir.Bind(env, hostOf(n), rundir.Entry{CrazeSessionID: sessionOf(n), Ready: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reg.Close() })
		ln := reg.Listener()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				serve(c)
			}
		}()
	}
	bound(9, func(c net.Conn) { _ = c.Close() })
	status := func(res protocol.HubSessionsListResult) map[string]string {
		m := map[string]string{}
		for _, r := range res.Sessions {
			m[r.HostID] = string(r.Status)
			if r.Status != protocol.RosterReachable && !r.Approximate {
				t.Errorf("a row not reachable is not approximate: %+v", r)
			}
		}
		return m
	}

	start := time.Now()
	res, err := Direct(stepContext(t), env, listWho)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took >= step {
		t.Fatalf("Direct waited out its context (%v) with every host heard from", took)
	}
	if got := status(res); got[hostOf(1)] != "reachable" || got[hostOf(9)] != "unreachable" || len(got) != 2 {
		t.Fatalf("Direct listed %v", got)
	}

	bound(8, func(c net.Conn) { t.Cleanup(func() { _ = c.Close() }) })
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if res, err = Direct(ctx, env, listWho); err != nil {
		t.Fatal(err)
	}
	if got := status(res); got[hostOf(8)] != "connecting" || len(got) != 3 {
		t.Fatalf("a host not heard from by the context's end: %v, want it connecting", got)
	}
}

// TestDirectWithNoRegistry: a registry that cannot be read is Direct's
// error, not an empty roster.
func TestDirectWithNoRegistry(t *testing.T) {
	env := testEnv(t)
	setVar(t, &hostsRead, func(rundir.Env) ([]rundir.Entry, error) { return nil, errors.New("no registry here") })
	if _, err := Direct(stepContext(t), env, listWho); err == nil || !strings.Contains(err.Error(), "no registry here") {
		t.Fatalf("Direct over an unreadable registry = %v", err)
	}
}
