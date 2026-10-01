package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// craze bridge --hub (plan 032 §3.12, P8): the same pump, to this machine's
// hub — found, or started — instead of a session's host; --hub and --session
// together are refused in the bridge's one-line contract.

// TestBridgeHubAndSessionAreExclusive: --hub with --session — any value, ""
// included, in either order — is one stderr line, exit 1, nothing on stdout,
// before any hub is looked for. The negative control: --hub alone looks for
// one (and, with none in this test binary, says so).
func TestBridgeHubAndSessionAreExclusive(t *testing.T) {
	serveHome(t)
	const refusal = "craze bridge: --hub and --session cannot be used together: the hub's session.connect names the session\n"
	for _, argv := range [][]string{
		{"bridge", "--hub", "--session", "0192f0aa-1111-7000-8000-00000000a001"},
		{"bridge", "--session=", "--hub"},
	} {
		stdout, stderr, code := executeErr(argv)
		if code != 1 || stdout != "" || stderr != refusal {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", argv, code, stdout, stderr)
		}
	}
	stdout, stderr, code := executeErr([]string{"bridge", "--hub"})
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "craze bridge: no hub: hub: no hub in a test binary") ||
		strings.Count(stderr, "\n") != 1 {
		t.Fatalf("bridge --hub with no hub: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// TestBridgeHubPumpsTheHubsHello: craze bridge --hub starts the hub (a child
// of this test, hubAsChild's seam) and pumps: the client's hello on stdin
// reaches the hub, whose answer — the hub's own endpoint — is on stdout, and
// stdin's end half-closes the connection, which the hub ends: exit 0.
func TestBridgeHubPumpsTheHubsHello(t *testing.T) {
	env, _ := serveHome(t)
	hubAsChild(t)
	hello, err := protocol.MarshalLine(map[string]any{"jsonrpc": "2.0", "id": 1, "method": protocol.MethodHello,
		"params": protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{Kind: "test", Name: "bridge --hub"}}})
	if err != nil {
		t.Fatal(err)
	}
	root := NewRootCmd()
	var out, errBuf bytes.Buffer
	root.SetIn(bytes.NewReader(hello))
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"bridge", "--hub"})
	if _, err := root.ExecuteC(); err != nil {
		t.Fatalf("craze bridge --hub: %v (stderr %q)", err, errBuf.String())
	}
	rec, _, err := rundir.ReadHubRecord(env)
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		ID     json.RawMessage         `json:"id"`
		Result protocol.HubHelloResult `json:"result"`
	}
	if strings.Count(out.String(), "\n") != 1 || json.Unmarshal(out.Bytes(), &resp) != nil || string(resp.ID) != "1" ||
		resp.Result.Endpoint.Kind != protocol.EndpointHub || resp.Result.Endpoint.HostID != rec.HubID {
		t.Fatalf("stdout %q; the hub is %s", out.String(), rec.HubID)
	}
	if errBuf.Len() != 0 {
		t.Fatalf("stderr %q", errBuf.String())
	}
}
