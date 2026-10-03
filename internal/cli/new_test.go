package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// craze new (plan 032 §3.12): its own refusals, and a session created through
// a real hub child whose hosts are real craze serve children (cliChildHubHosts)
// with the fake agent.

// TestNewRefusesBeforeTheHub: --fast with --no-fast is a usage error, a -C
// that is no directory exit 1, both before a hub is asked for; with no hub to
// be had — this test binary spawns none (hub.ErrNoHub) — exit 1, in one line.
func TestNewRefusesBeforeTheHub(t *testing.T) {
	serveHome(t)
	for _, tc := range []struct {
		argv []string
		code int
		want string
	}{
		{[]string{"new", "--fast", "--no-fast"}, 2, "craze new: --fast and --no-fast are mutually exclusive\n"},
		{[]string{"new", "-C", "/no/such/dir"}, 1, "craze new: /no/such/dir is not a directory\n"},
	} {
		stdout, stderr, code := executeErr(tc.argv)
		if code != tc.code || stdout != "" || stderr != tc.want {
			t.Fatalf("%v exited %d: stdout %q, stderr %q; want %d, %q", tc.argv, code, stdout, stderr, tc.code, tc.want)
		}
	}
	stdout, stderr, code := executeErr([]string{"new", "-C", t.TempDir(), "hello"})
	if code != 1 || stdout != "" || !regexp.MustCompile(`^craze new: hub: no hub in a test binary[^\n]*\n$`).MatchString(stderr) {
		t.Fatalf("craze new with no hub exited %d: stdout %q, stderr %q", code, stdout, stderr)
	}
}

// TestNewJSONFailures (SF-124, plan 035 P5, A10): with --json every exit-1
// path before the session started prints exactly one JSON object on stdout —
// {"error":{"message":…}} for an invalid -C and for no hub — the stderr line
// and the exit code as without --json; a usage error stays plain.
func TestNewJSONFailures(t *testing.T) {
	serveHome(t)
	type failure struct {
		Error struct {
			Message string `json:"message"`
			Data    *struct{}
		} `json:"error"`
	}
	for _, tc := range []struct {
		argv   []string
		stderr *regexp.Regexp
	}{
		{[]string{"new", "--json", "-C", "/no/such/dir"}, regexp.MustCompile(`^craze new: /no/such/dir is not a directory\n$`)},
		{[]string{"new", "--json", "-C", t.TempDir(), "hello"}, regexp.MustCompile(`^craze new: hub: no hub in a test binary[^\n]*\n$`)},
	} {
		stdout, stderr, code := executeErr(tc.argv)
		var got failure
		if code != 1 || !tc.stderr.MatchString(stderr) || strings.Count(stdout, "\n") != 1 ||
			json.Unmarshal([]byte(stdout), &got) != nil || got.Error.Message == "" ||
			stderr != "craze new: "+got.Error.Message+"\n" {
			t.Fatalf("%v exited %d: stdout %q, stderr %q", tc.argv, code, stdout, stderr)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal([]byte(stdout), &keys); err != nil || len(keys) != 1 {
			t.Fatalf("%v printed %q, want the one key \"error\"", tc.argv, stdout)
		}
	}
	stdout, stderr, code := executeErr([]string{"new", "--json", "--fast", "--no-fast"})
	if code != 2 || stdout != "" || stderr != "craze new: --fast and --no-fast are mutually exclusive\n" {
		t.Fatalf("the usage error exited %d: stdout %q, stderr %q", code, stdout, stderr)
	}
}

// TestNewFailureJSON: a protocol refusal is {"error": <protocol.Error>}, whole
// — code, message, data.code, data.reason, data.cause — a wrapped one too; any
// other error is {"error":{"message": msg}}.
func TestNewFailureJSON(t *testing.T) {
	perr := &protocol.Error{Code: -32000, Message: "the session did not start: boom",
		Data: protocol.ErrorData{Code: protocol.CodeNotAccepting, Reason: protocol.ReasonStartFailed, Cause: "boom"}}
	for _, err := range []error{perr, fmt.Errorf("create: %w", perr)} {
		want := `{"error":{"code":-32000,"message":"the session did not start: boom","data":{"code":"not_accepting","reason":"start_failed","cause":"boom"}}}`
		if got := string(newFailureJSON(err, "ignored")); got != want {
			t.Fatalf("a refusal: %s\nwant %s", got, want)
		}
	}
	if got, want := string(newFailureJSON(errors.New("x"), "hub: down")), `{"error":{"message":"hub: down"}}`; got != want {
		t.Fatalf("a plain failure: %s, want %s", got, want)
	}
	if got, want := string(newFailureJSON(nil, "bad")), `{"error":{"message":"bad"}}`; got != want {
		t.Fatalf("no error: %s, want %s", got, want)
	}
}

// TestNewJSONRefusedThroughAHub: a hub's refusal — no provider named and none
// configured (params.provider is required) — is {"error": <protocol.Error>}
// with data.code and data.reason, the stderr line and exit 1 unchanged.
func TestNewJSONRefusedThroughAHub(t *testing.T) {
	env, _ := serveHome(t)
	config := "host_idle_exit = \"30s\"\n"
	if err := os.WriteFile(filepath.Join(env.CrazeDir, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	hubAsChild(t, cliChildHubHosts+"=1")
	stdout, stderr, code := executeErr([]string{"new", "--json", "-C", t.TempDir(), "hello"})
	var got struct {
		Error protocol.Error `json:"error"`
	}
	if code != 1 || strings.Count(stdout, "\n") != 1 || json.Unmarshal([]byte(stdout), &got) != nil {
		t.Fatalf("craze new --json exited %d: stdout %q, stderr %q (hub log: %s)", code, stdout, stderr, hubLog(env)())
	}
	if got.Error.Data.Code != protocol.CodeBadRequest || !strings.HasPrefix(got.Error.Message, "params.provider is required") ||
		got.Error.Data.Reason == "" || stderr != "craze new: "+got.Error.Message+"\n" {
		t.Fatalf("the refusal: %+v, stderr %q", got.Error, stderr)
	}
}

// TestNewThroughAHubFromAnotherDirectory (plan 032 §3.5's environment
// contract, §3.18): craze new run in workspace A with a relative CRAZE_HOME
// creates a session in B through the hub it spawns. The hub and the host it
// spawns are handed CRAZE_HOME made absolute against A — so the host, though
// it runs in B, serves in A's namespace, writes its index row and persists
// its provider (owner decision 1) in A's craze directory, and no craze
// directory appears in B — and the host's agent binary is `[agents]`'s, the
// launch's CRAZE_AGENT_BIN removed. It answers once the session has started:
// `started <id> in <dir>`, the prompt taken; --json prints the hub's result.
// The negative control for the namespace is B's own: a CRAZE_HOME resolved in
// B would be another namespace.
func TestNewThroughAHubFromAnotherDirectory(t *testing.T) {
	env, _ := serveHome(t)
	agentBin := fakeAgentPath(t)
	a, b := t.TempDir(), t.TempDir()
	t.Chdir(a)
	t.Setenv("CRAZE_HOME", "rel-craze")
	t.Setenv("CRAZE_AGENT_BIN", "/nowhere/agent")
	crazeA := filepath.Join(a, "rel-craze")
	if err := os.MkdirAll(crazeA, 0o700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("host_idle_exit = \"30s\"\n\n[agents]\ncursor = %q\n", agentBin)
	if err := os.WriteFile(filepath.Join(crazeA, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	env = rundir.ProcessEnv()
	nsA, err := rundir.Namespace(crazeA)
	if err != nil {
		t.Fatal(err)
	}
	if ns, err := rundir.Namespace(env.CrazeDir); err != nil || ns != nsA {
		t.Fatalf("the relative craze directory %q is namespace %s (%v), want A's %s", env.CrazeDir, ns, err, nsA)
	}
	hubAsChild(t, cliChildHubHosts+"=1")

	stdout, stderr, code := executeErr([]string{"new", "-C", b, "--provider", "cursor", "fix", "the", "build"})
	if code != 0 || stderr != "" {
		t.Fatalf("craze new exited %d: stdout %q, stderr %q (hub log: %s)", code, stdout, stderr, hubLog(env)())
	}
	entries, err := rundir.Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	var created rundir.Entry
	for _, e := range entries {
		if e.RequestID != "" {
			created = e
		}
	}
	t.Cleanup(func() {
		if err := stopHost(created); err == nil {
			waitReaped(t, created.PID)
		}
	})
	if want := "started " + psShort(created.CrazeSessionID) + " in " + b + "\n"; stdout != want {
		t.Fatalf("craze new printed %q, want %q", stdout, want)
	}
	if created.Workspace != b || !strings.HasPrefix(created.RequestID, "new-") || !created.Ready {
		t.Fatalf("the created host's entry %+v", created)
	}
	nsB, err := rundir.Namespace(filepath.Join(b, "rel-craze"))
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(filepath.Dir(created.Socket)); got != nsA || got == nsB {
		t.Fatalf("the created host serves in namespace %s, want A's %s (B's is %s)", got, nsA, nsB)
	}
	if _, err := os.Stat(filepath.Join(b, "rel-craze")); !os.IsNotExist(err) {
		t.Fatalf("a craze directory appeared in B: %v", err)
	}
	waitFor(t, "the session's index row in A's craze directory", func() bool {
		raw, err := os.ReadFile(filepath.Join(crazeA, "sessions.jsonl"))
		return err == nil && strings.Contains(string(raw), created.CrazeSessionID)
	})
	// The host persists its provider just after its start publishes
	// readiness, which is what the create's answer waits for: the save can
	// follow the answer (r32 8, SF-117), so it is awaited, within 10 s.
	persisted := regexp.MustCompile(`(?m)^provider = "cursor"$`)
	var cfg []byte
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		cfg, err = os.ReadFile(filepath.Join(crazeA, "config.toml"))
		if err == nil && persisted.Match(cfg) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the created session did not persist its provider within 10s: %q (%v)", cfg, err)
		}
	}
	if runtime.GOOS == "linux" {
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", created.PID))
		if err != nil {
			t.Fatal(err)
		}
		vars := strings.Split(string(environ), "\x00")
		if !slices.Contains(vars, "CRAZE_HOME="+crazeA) || slices.ContainsFunc(vars, func(v string) bool {
			return strings.HasPrefix(v, "CRAZE_AGENT_BIN=")
		}) {
			t.Fatalf("the created host's environment: %q", vars)
		}
		if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", created.PID)); err != nil || cwd != b {
			t.Fatalf("the created host runs in %q (%v), want %s", cwd, err, b)
		}
	}

	// --json, no prompt: the hub's result as it wrote it.
	stdout, stderr, code = executeErr([]string{"new", "-C", b, "--provider", "cursor", "--json"})
	var res protocol.CreateResult
	if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &res) != nil || res.Prompt != protocol.CreatePromptNone ||
		res.Session.Approximate || res.Session.Host.Workspace != b || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("craze new --json exited %d: stdout %q, stderr %q", code, stdout, stderr)
	}
	second, ok := hostEntry(env, res.Session.HostID)
	if !ok {
		t.Fatalf("the second session's host %s is not listed", res.Session.HostID)
	}
	t.Cleanup(func() {
		if err := stopHost(second); err == nil {
			waitReaped(t, second.PID)
		}
	})
}

// TestServeTakesTheHubsRequest (plan 032 §3.10): craze serve's hidden
// --request-id and --request-hash — the hub's create, written into the host's
// registry entry — go together, the id in a requestId's form and the hash a
// token; anything else is a usage error before anything is created. Neither
// is the root's.
func TestServeTakesTheHubsRequest(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want hostRequest
		bad  string
	}{
		{nil, hostRequest{}, ""},
		{[]string{"--request-id=new-1", "--request-hash=sha256:ab"}, hostRequest{id: "new-1", hash: "sha256:ab"}, ""},
		{[]string{"--request-id=new-1"}, hostRequest{}, "go together"},
		{[]string{"--request-hash=sha256:ab"}, hostRequest{}, "go together"},
		{[]string{"--request-id=a b", "--request-hash=h"}, hostRequest{}, "--request-id"},
		{[]string{"--request-id=r", "--request-hash=h h"}, hostRequest{}, "--request-hash"},
	} {
		_, f := parseServeFlags(t, tc.argv...)
		got, err := f.request()
		if tc.bad != "" {
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(err.Error(), tc.bad) {
				t.Fatalf("%v: %v, want a usage error naming %s", tc.argv, err, tc.bad)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("%v: %+v, %v; want %+v", tc.argv, got, err, tc.want)
		}
	}
	serve, root := newServeCmd(), NewRootCmd()
	for _, name := range []string{"request-id", "request-hash"} {
		if fl := serve.Flags().Lookup(name); fl == nil || !fl.Hidden || root.Flags().Lookup(name) != nil {
			t.Fatalf("--%s: hidden on craze serve alone, got %+v", name, fl)
		}
	}
}
