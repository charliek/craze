package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/tui"
	"github.com/charliek/craze/internal/version"
)

// Upgrading under a live hub (plan 046 §3.C), the clients' half: craze new
// names the remedy for a hub of another craze version that cannot start a
// session (U2, C-3), and the TUI's own spawn of a craze an upgrade removed
// says what to do (U4, C-4).

// staleHubPID is the pid staleHubThen's hello names: never this process's, so a
// kill the words name is the hub's alone.
const staleHubPID = 4242

// staleHubThen plays env's running hub — craze v, its hello's pid staleHubPID —
// whose every session.create is refused unavailable, reason spawn_failed, in
// msg: its record where Ensure looks, its start token dropped (as
// inProcessHub's: a hub whose identity cannot be verified is never
// signalled), and a socket answering each connection's hello and creates.
// Ensure must find it: one it would spawn instead fails the test.
//
// With then set, the hub is replaced after the first connection (Ensure's):
// every later one — the create's — is a hub of craze then, pid
// staleHubPID+1, on the same socket. then "" replaces nothing.
func staleHubThen(t *testing.T, env rundir.Env, v, then, msg string) {
	t.Helper()
	sock := filepath.Join(shortRuntimeDir(t), "hub.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	lock, err := rundir.LockHub(env, rundir.NewHubID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	if _, err := lock.WriteRecord(rundir.HubRecord{Socket: sock, CrazeVersion: v}); err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	b, err := os.ReadFile(lock.RecordPath())
	if err != nil || json.Unmarshal(b, &rec) != nil {
		t.Fatalf("the hub's record %s: %v", lock.RecordPath(), err)
	}
	rec["startToken"] = ""
	if b, err = json.Marshal(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock.RecordPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	hello := protocol.HubHelloResult{Protocol: protocol.ProtocolVersion,
		Endpoint:     protocol.Endpoint{Kind: protocol.EndpointHub, HostID: lock.ID(), CrazeVersion: v, PID: staleHubPID},
		Capabilities: protocol.HubCapabilities(), Limits: protocol.HostLimits()}
	later := hello
	if then != "" {
		later.Endpoint.CrazeVersion, later.Endpoint.PID = then, staleHubPID+1
	}
	go func() {
		for n := 0; ; n++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hello := hello
			if n > 0 {
				hello = later
			}
			go func() {
				defer c.Close()
				lr := protocol.NewLineReader(c, 0)
				for {
					line, err := lr.ReadLine()
					if err != nil {
						return
					}
					var req protocol.Request
					if json.Unmarshal(line, &req) != nil {
						return
					}
					resp := protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: req.ID}
					switch req.Method {
					case protocol.MethodHello:
						resp.Result, _ = json.Marshal(hello)
					case protocol.MethodSessionCreate:
						resp.Error = &protocol.Error{Code: -32000, Message: msg,
							Data: protocol.ErrorData{Code: protocol.CodeUnavailable, Reason: protocol.ReasonSpawnFailed}}
					default:
						resp.Error = &protocol.Error{Code: -32000, Message: "not here",
							Data: protocol.ErrorData{Code: protocol.CodeUnsupported, Reason: protocol.ReasonUnsupported}}
					}
					if protocol.WriteLine(c, resp) != nil {
						return
					}
				}
			}()
		}
	}()
	prev := hub.Command
	hub.Command = func([]string) (*exec.Cmd, error) {
		t.Error("craze new spawned a hub beside the running one")
		return nil, errors.New("no hub is spawned here")
	}
	t.Cleanup(func() { hub.Command = prev })
}

// TestNewNamesTheRemedyForAnOlderHub is C-3 (U2): a running hub of another
// craze version whose create is refused spawn_failed — a hub from before U1,
// re-executing a craze an upgrade removed, says no more than that — gets
// craze new's remedy after its words: the hub's pid from its hello, both
// versions and the kill, exit 1; with --json the stdout object is still the
// wire's error, whole, and stderr says the same line. The negative controls:
// a hub of this craze's version (one that has U1, and says it itself) and a
// later hub's own U1 answer get nothing added.
func TestNewNamesTheRemedyForAnOlderHub(t *testing.T) {
	const plain = "the session host cannot be started"
	remedy := fmt.Sprintf("; the hub (pid %d) runs craze 0.1.0 and this is craze %s: if craze was upgraded, "+
		"stop that hub with kill %d (no session ends) and run this again", staleHubPID, version.Version, staleHubPID)
	u1 := fmt.Sprintf("this hub's craze (/home/linuxbrew/.linuxbrew/Cellar/craze/0.2.0/bin/craze) was replaced by an "+
		"upgrade: stop this hub with kill %d (no session ends) and run this again", staleHubPID)
	for _, tc := range []struct {
		name, version, msg, want string
	}{
		{"an older hub", "0.1.0", plain, "craze new: " + plain + remedy + "\n"},
		{"a hub of this version", version.Version, plain, "craze new: " + plain + "\n"},
		{"a hub that names the remedy itself", "0.1.0", u1, "craze new: " + u1 + "\n"},
		// Ensure meets an older hub, which then ends; a hub of this version
		// binds the socket and refuses the create: its refusal is explained
		// by its own hello, so nothing names the older hub's pid.
		{"a hub replaced after Ensure", "0.1.0", plain, "craze new: " + plain + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, env := providersHubMachine(t)
			then := ""
			if tc.name == "a hub replaced after Ensure" {
				then = version.Version
			}
			staleHubThen(t, env, tc.version, then, tc.msg)
			stdout, stderr, code := executeErr([]string{"new", "-C", t.TempDir(), "--provider", "cursor", "hello"})
			if code != 1 || stdout != "" || stderr != tc.want {
				t.Fatalf("craze new: exit %d, stdout %q, stderr %q; want exit 1 and %q", code, stdout, stderr, tc.want)
			}
			stdout, stderr, code = executeErr([]string{"new", "--json", "-C", t.TempDir(), "--provider", "cursor"})
			var got struct {
				Error protocol.Error `json:"error"`
			}
			if code != 1 || stderr != tc.want || json.Unmarshal([]byte(stdout), &got) != nil ||
				got.Error.Message != tc.msg || got.Error.Data.Reason != protocol.ReasonSpawnFailed {
				t.Fatalf("craze new --json: exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
		})
	}
}

// TestALaunchWhoseCrazeIsGoneSaysSo is C-4 (U4): the TUI's own spawn of a
// host, when this craze's executable is gone from its path — an upgrade
// removed it after this craze started — is a start failure in words that
// name the path and say what to do instead, never the opt-out, which names a
// switch this craze cannot act on either; exit 1.
func TestALaunchWhoseCrazeIsGoneSaysSo(t *testing.T) {
	_, ws := serveHome(t)
	t.Setenv(detachEnv, "")
	spawnBounds(t, serveStep, serveStep, serveStep)
	gone := filepath.Join(t.TempDir(), "cellar", "0.1.0", "bin", "craze")
	prev := hostspawn.Command
	hostspawn.Command = func(argv []string) (*exec.Cmd, error) { return exec.Command(gone, argv...), nil }
	t.Cleanup(func() { hostspawn.Command = prev })
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		_, err := cfg.NewBackend(cfg.Provider, false)
		var refused *tui.Refusal
		if err == nil || errors.As(err, &refused) {
			t.Fatalf("a launch whose craze is gone answered %v, want a start failure", err)
		}
		return tui.Result{StartErr: err}, err
	})
	err := runTUI(nil, launchFlags(t, ws), hostEnv{})
	want := "craze was upgraded or removed since this craze started (" + gone + " is gone): quit and start craze again"
	if msg, code := diagnose(nil, err); code != 1 || msg != want || strings.Contains(msg, detachEnv) {
		t.Fatalf("the failure: exit %d, %q; want %q", code, msg, want)
	}
}
