package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control/wiretest"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/transcript"
	"github.com/charliek/craze/internal/version"
)

// craze providers (plan 036 §3.2, A2). Every case runs over a scratch PATH,
// HOME and CRAZE_HOME (t.Setenv, so none is parallel), with no
// CRAZE_PROVIDER or CRAZE_AGENT_BIN, and TestMain's login session (the
// GUI's): what the command says is the scratch machine's, never the owner's.

// providersMachine is the scratch machine the cases run on: grok on PATH and
// nothing else, an empty craze directory (no native key), and its
// config.toml's path, which cursor's fix names.
func providersMachine(t *testing.T) (config string) {
	t.Helper()
	availPath(t, "grok")
	isolateHome(t)
	home := crazeHome(t)
	t.Setenv("CRAZE_PROVIDER", "")
	t.Setenv(envAgentBin, "")
	return filepath.Join(home, "config.toml")
}

// TestProvidersTable (A2): the human table — a header, then one row per
// listed provider in the registry's order, its id, its state in words, and
// its reason and fix, "-" for a ready one's — and exit 0 although cursor is
// unavailable. gx, not installed, is not listed; nothing goes to stderr.
func TestProvidersTable(t *testing.T) {
	config := providersMachine(t)
	stdout, stderr, code := executeErr([]string{"providers"})
	if code != 0 {
		t.Fatalf("craze providers: exit %d %q", code, stderr)
	}
	row := func(cells ...string) string {
		return fmt.Sprintf("%-10s%-13s%-32s%s\n", cells[0], cells[1], cells[2], cells[3])
	}
	want := row("PROVIDER", "STATE", "REASON", "FIX") +
		row("cursor", "unavailable", "cursor-agent not found on PATH", "install cursor-agent, or set [agents].cursor in "+config) +
		row("grok", "ready", "-", "-") +
		row("native", "needs setup", "no model provider has a key", nativeNoKeyFix)
	if stdout != want {
		t.Fatalf("craze providers:\n%s\nwant:\n%s", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("stderr: %q", stderr)
	}
}

// TestProvidersJSON (A2): --json is one line, {"providers":[…]}, each entry
// the wire's providerOption — id, label, state in the wire's words, and a
// reason and a fix that are absent, not empty, when it is ready — and exit 0.
func TestProvidersJSON(t *testing.T) {
	config := providersMachine(t)
	stdout, stderr, code := executeErr([]string{"providers", "--json"})
	if code != 0 {
		t.Fatalf("craze providers --json: exit %d %q", code, stderr)
	}
	q := func(s string) string {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	want := `{"providers":[` +
		`{"id":"cursor","label":"cursor","state":"unavailable","reason":"cursor-agent not found on PATH","fix":` +
		q("install cursor-agent, or set [agents].cursor in "+config) + `},` +
		`{"id":"grok","label":"grok","state":"ready"},` +
		`{"id":"native","label":"native","state":"needs_setup","reason":"no model provider has a key","fix":` +
		q(nativeNoKeyFix) + `}]}` + "\n"
	if stdout != want {
		t.Fatalf("craze providers --json:\n%s\nwant:\n%s", stdout, want)
	}
}

// TestProvidersJSONIsTheWiresProviderOption (A2, C2): every provider craze
// providers --json prints is the wire's providerOption — the schema's own
// def, a ready one and the two other states among them.
func TestProvidersJSONIsTheWiresProviderOption(t *testing.T) {
	providersMachine(t)
	stdout, stderr, code := executeErr([]string{"providers", "--json"})
	if code != 0 {
		t.Fatalf("craze providers --json: exit %d %q", code, stderr)
	}
	var doc struct {
		Providers []json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || len(doc.Providers) != 3 {
		t.Fatalf("craze providers --json printed %q (%v)", stdout, err)
	}
	for _, p := range doc.Providers {
		if err := wiretest.Default().Document(protocol.MethodSchema(protocol.MethodSessionsCreateOptions), "#/$defs/providerOption", p); err != nil {
			t.Fatal(err)
		}
	}
}

// providersHubMachine is providersMachine with a namespace of its own for a
// hub — a 0700 HOME and a short runtime directory — and a config whose
// provider is grok.
func providersHubMachine(t *testing.T) (config string, env rundir.Env) {
	t.Helper()
	config = providersMachine(t)
	if err := os.Chmod(os.Getenv("HOME"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_RUNTIME_DIR", shortRuntimeDir(t))
	writeFileT(t, config, "provider = \"grok\"\n")
	return config, rundir.ProcessEnv()
}

// inProcessHub runs the hub — internal/hub's Run — in this process over env,
// given creates, until the test ends, and answers its socket once it serves.
// Its record's start token is then dropped, in place (the same file, so the
// hub does not take it as lost): a hub whose identity cannot be verified is
// never signalled, so a starved run whose two hellos time out reports the
// hub rather than having Find's step 3 send SIGTERM to its pid — this test
// process's.
func inProcessHub(t *testing.T, env rundir.Env, creates *hub.Creates) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- hub.Run(ctx, hub.Options{Env: env, Codecs: protocol.Codecs{Event: agent.EventCodecVersion, Snapshot: transcript.SnapshotVersion},
			Ready: hub.NewReadyPipe(w), IdleGrace: time.Hour, Creates: creates})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(serveStep):
			t.Errorf("the in-process hub did not stop within %v", serveStep)
		}
		_ = r.Close()
	})
	raw := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r) // one line, then the pipe closed
		raw <- b
	}()
	var line hub.ReadyLine
	select {
	case b := <-raw:
		if err := json.Unmarshal(b, &line); err != nil || !line.OK {
			t.Fatalf("the in-process hub did not come up: %q (%v)", b, err)
		}
	case <-time.After(serveStep):
		t.Fatalf("the in-process hub wrote no ready line within %v", serveStep)
	}
	ns, err := rundir.Namespace(env.CrazeDir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(env.Home, ".cache", "craze", "hubs", ns+".json")
	var rec map[string]any
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &rec) != nil {
		t.Fatalf("the hub's record %s: %v", path, err)
	}
	rec["startToken"] = ""
	if b, err = json.Marshal(rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return line.Socket
}

// TestProvidersHub (A2, C2): --hub prints the running hub's answer — its
// providers as the table, its default and its recent directories, one line
// each — and --hub --json the result as the hub wrote it, which is
// sessions.createOptions' result; exit 0 although cursor is unavailable.
func TestProvidersHub(t *testing.T) {
	config, env := providersHubMachine(t)
	work, other := t.TempDir(), t.TempDir()
	writeFileT(t, filepath.Join(os.Getenv("CRAZE_HOME"), "sessions.jsonl"),
		indexRow("s-1", work, "2026-10-03T12:00:00Z")+indexRow("s-2", other, "2026-10-02T12:00:00Z"))
	inProcessHub(t, env, hubCreates())

	stdout, stderr, code := executeErr([]string{"providers", "--hub"})
	if code != 0 {
		t.Fatalf("craze providers --hub: exit %d %q", code, stderr)
	}
	row := func(cells ...string) string {
		return fmt.Sprintf("%-10s%-13s%-32s%s\n", cells[0], cells[1], cells[2], cells[3])
	}
	want := row("PROVIDER", "STATE", "REASON", "FIX") +
		row("cursor", "unavailable", "cursor-agent not found on PATH", "install cursor-agent, or set [agents].cursor in "+config) +
		row("grok", "ready", "-", "-") +
		row("native", "needs setup", "no model provider has a key", nativeNoKeyFix) +
		"default: grok\n" +
		"recent: " + work + "\n" +
		"recent: " + other + "\n"
	if stdout != want || stderr != "" {
		t.Fatalf("craze providers --hub:\n%s\nwant:\n%s\nstderr %q", stdout, want, stderr)
	}

	stdout, stderr, code = executeErr([]string{"providers", "--hub", "--json"})
	if code != 0 {
		t.Fatalf("craze providers --hub --json: exit %d %q", code, stderr)
	}
	if err := wiretest.Default().Document(protocol.MethodSchema(protocol.MethodSessionsCreateOptions), "#/$defs/result", []byte(stdout)); err != nil {
		t.Fatal(err)
	}
	var res protocol.CreateOptionsResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil || res.DefaultProvider != "grok" || len(res.Providers) != 3 ||
		len(res.RecentDirs) != 2 || res.RecentDirs[0].Dir != work {
		t.Fatalf("craze providers --hub --json printed %q (%v)", stdout, err)
	}
}

// TestProvidersHubFindsNoHub (A2, C2): with no hub running --hub says so,
// exit 1, and starts none — although this test binary would spawn one (its
// hub.Command installed, as hub.Ensure would use it): no child spawned, no
// hub record and no hub socket in the scratch namespace.
func TestProvidersHubFindsNoHub(t *testing.T) {
	_, env := providersHubMachine(t)
	kids := hubAsChild(t)
	stdout, stderr, code := executeErr([]string{"providers", "--hub"})
	if code != 1 || stdout != "" || stderr != "craze providers: "+noHubRunning+"\n" {
		t.Fatalf("craze providers --hub with no hub: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if pids := kids.pids(); len(pids) != 0 {
		t.Fatalf("craze providers --hub spawned hub children %v", pids)
	}
	if rec, _, err := rundir.ReadHubRecord(env); err == nil {
		t.Fatalf("a hub record appeared: %+v", rec)
	}
	sock, err := rundir.HubSocket(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("a hub socket appeared at %s (%v)", sock, err)
	}
}

// TestProvidersHubOfAnOlderHub (A2, C2): a running hub that cannot answer
// sessions.createOptions — given session.create's options but no answer, as
// an older craze's hub is — is reported in LacksError's words, exit 1, and
// left as it is.
func TestProvidersHubOfAnOlderHub(t *testing.T) {
	_, env := providersHubMachine(t)
	creates := hubCreates()
	creates.Options = nil
	inProcessHub(t, env, creates)
	stdout, stderr, code := executeErr([]string{"providers", "--hub"})
	want := "craze providers: this hub (craze " + version.Version + ") cannot list what it can create; it exits when idle\n"
	if code != 1 || stdout != "" || stderr != want {
		t.Fatalf("craze providers --hub of an older hub: exit %d, stdout %q, stderr %q; want %q", code, stdout, stderr, want)
	}
}

// TestProvidersUsageErrors (A2): an argument, or a flag it does not take, is
// a usage error, exit 2, and prints nothing.
func TestProvidersUsageErrors(t *testing.T) {
	providersMachine(t)
	for _, argv := range [][]string{{"providers", "cursor"}, {"providers", "--hub-of-nothing"}} {
		stdout, stderr, code := executeErr(argv)
		if code != 2 {
			t.Fatalf("craze %q: exit %d %q, want 2", argv, code, stderr)
		}
		if stdout != "" {
			t.Fatalf("craze %q printed %q", argv, stdout)
		}
	}
}
