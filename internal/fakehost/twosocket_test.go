package fakehost

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// The stand-in's identity: a second fake host, distinct from every fixture
// Host's default.
const (
	standInHostID  = "00000000000b"
	standInSession = "session-fake-b"
)

// twoSocketScript is a two-socket fixture's lines with nothing recorded:
// conn 1 says hello to the hub's socket and asks it for sessions.list, conn 2
// says hello to the host's — conn 1's second line leaves its socket out, which
// its first line decided. With hub false the same lines carry no socket at
// all: a one-socket fixture.
func twoSocketScript(hub bool) string {
	sock := `"sock":"hub",`
	if !hub {
		sock = ""
	}
	hello := `{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocols":[1],"client":{"kind":"test","name":"two-socket"}}}`
	return `{"conn":1,` + sock + `"dir":"c2s","msg":` + hello + "}\n" +
		`{"conn":1,` + sock + `"dir":"s2c"}` + "\n" +
		`{"conn":2,"dir":"c2s","msg":` + hello + "}\n" +
		`{"conn":2,"dir":"s2c"}` + "\n" +
		`{"conn":2,"dir":"c2s","msg":{"jsonrpc":"2.0","id":2,"method":"sessions.list"}}` + "\n" +
		`{"conn":2,"dir":"s2c"}` + "\n"
}

// standIn is a hubStarter for the runner's own test, never for a fixture:
// there is no hub before plan 032 C11, so what answers the "hub" socket here
// is a second fake host with an id of its own — enough to tell which socket
// each connection reached. Before serving it, it checks that the fixture's
// Host is listed in the registry it is handed, as a hub would find it.
func standIn(t *testing.T, env rundir.Env) string {
	t.Helper()
	entries, err := rundir.Hosts(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the fixture's registry lists %d hosts, want the fixture's Host alone: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.HostID != "0123456789ab" || e.CrazeSessionID != "session-fake-1" || !e.Ready || e.ProviderSessionID == "" ||
		e.Workspace != "/work" || e.Incarnation == "" || !e.StartedAt.Equal(clockStart) || e.Protocol != protocol.ProtocolVersion {
		t.Fatalf("the fixture's Host is listed as %+v", e)
	}
	h, err := New(Options{HostID: standInHostID, CrazeSessionID: standInSession})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(shortTempDir(t, "czsi-"), "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	serveFixtureHost(t, h, l, nil)
	return socket
}

// recorded is each connection's s2c lines in out, a run's recorded fixture,
// in order — and each line's socket as recorded.
func recorded(t *testing.T, out []byte) (msgs map[int][]json.RawMessage, socks map[int][]string) {
	t.Helper()
	msgs, socks = map[int][]json.RawMessage{}, map[int][]string{}
	for _, rl := range parseFixtureLines(t, "recorded", out) {
		if rl.parsed.Dir == "s2c" {
			msgs[rl.parsed.Conn] = append(msgs[rl.parsed.Conn], rl.parsed.Msg)
			socks[rl.parsed.Conn] = append(socks[rl.parsed.Conn], rl.parsed.Sock)
		}
	}
	return msgs, socks
}

// helloHostID is a hello reply's endpoint.hostId.
func helloHostID(t *testing.T, line json.RawMessage) string {
	t.Helper()
	var resp struct {
		Result protocol.HubHelloResult `json:"result"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	return resp.Result.Endpoint.HostID
}

// TestATwoSocketFixtureDialsEachConnectionsSocket (plan 032 §3.15): a
// two-socket fixture's Host is listed in a registry of the fixture's own,
// where the hub is handed it, and each connection reaches the socket its
// first line names — conn 1 the hub's (here the stand-in), conn 2 the Host's —
// its later lines staying there; -update keeps every recorded line's socket,
// and the recording replays byte for byte. The negative control: the same
// lines with no socket named are a one-socket fixture, and both connections
// reach the Host.
func TestATwoSocketFixtureDialsEachConnectionsSocket(t *testing.T) {
	lines := parseFixtureLines(t, "two-socket", []byte(twoSocketScript(true)))
	if !fixtureNeedsHub(lines) {
		t.Fatal("a fixture with a hub line is not a two-socket one")
	}
	out := newTwoSocketRunner(t, Options{}, standIn).run(lines, true)
	msgs, socks := recorded(t, out)
	if got := helloHostID(t, msgs[1][0]); got != standInHostID {
		t.Errorf("conn 1 (the hub's socket) was answered by %q, want the stand-in %q", got, standInHostID)
	}
	if got := helloHostID(t, msgs[2][0]); got != "0123456789ab" {
		t.Errorf("conn 2 (the host's socket) was answered by %q, want the fixture's Host", got)
	}
	if want := []string{sockHub}; strings.Join(socks[1], ",") != strings.Join(want, ",") {
		t.Errorf("conn 1's recorded lines name sockets %q, want %q: -update dropped the socket", socks[1], want)
	}
	if !strings.Contains(string(msgs[2][1]), `"sessionId":"session-fake-1"`) || !strings.Contains(string(msgs[2][1]), `"incarnation":"INCARNATION-1"`) {
		t.Errorf("conn 2's roster is not the fixture's Host's, its incarnation by placeholder: %s", msgs[2][1])
	}
	// The recording replays, byte for byte, against a fresh pair.
	newTwoSocketRunner(t, Options{}, standIn).run(parseFixtureLines(t, "recorded", out), false)

	// Negative control: no socket named, one socket — conn 1 reaches the Host.
	plain := parseFixtureLines(t, "one-socket", []byte(twoSocketScript(false)))
	if fixtureNeedsHub(plain) {
		t.Fatal("a fixture with no hub line is a two-socket one")
	}
	msgs, _ = recorded(t, newFixtureRunner(t, Options{}).run(plain, true))
	if got := helloHostID(t, msgs[1][0]); got != "0123456789ab" {
		t.Fatalf("with no socket named, conn 1 was answered by %q: the control does not tell the two sockets apart", got)
	}
}

// TestAFixtureConnectionKeepsItsSocket: a connection's socket is its first
// line's, so a later line naming the other is refused, as is a socket nobody
// called by that name and a hub line in a fixture with no hub; and a fixture
// that names the hub needs one to start — refused, saying why, with none.
func TestAFixtureConnectionKeepsItsSocket(t *testing.T) {
	r := newTwoSocketRunner(t, Options{}, standIn)
	if _, err := r.connFor(fixtureLine{Conn: 1, Sock: sockHub, Dir: "c2s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.connFor(fixtureLine{Conn: 1, Dir: "s2c"}); err != nil {
		t.Fatalf("a later line leaving its socket out: %v", err)
	}
	for _, tc := range []struct {
		name string
		r    *fixtureRunner
		fl   fixtureLine
		want string
	}{
		{"a later line naming the other socket", r, fixtureLine{Conn: 1, Sock: sockHost, Dir: "c2s"}, "is to the hub's socket"},
		{"a socket of no such name", r, fixtureLine{Conn: 3, Sock: "relay", Dir: "c2s"}, `no socket called "relay"`},
		{"a hub line with no hub", newFixtureRunner(t, Options{}), fixtureLine{Conn: 1, Sock: sockHub, Dir: "c2s"}, "has no hub socket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.r.connFor(tc.fl)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("connFor = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}

	two := parseFixtureLines(t, "two-socket", []byte(twoSocketScript(true)))
	one := parseFixtureLines(t, "one-socket", []byte(twoSocketScript(false)))
	if start, err := fixtureHubFor(one, nil); start != nil || err != nil {
		t.Errorf("a one-socket fixture with no hub: %v, %v; want neither", start != nil, err)
	}
	if start, err := fixtureHubFor(two, standIn); start == nil || err != nil {
		t.Errorf("a two-socket fixture with a hub: %v, %v; want the hub", start != nil, err)
	}
	if _, err := fixtureHubFor(two, nil); err == nil || !strings.Contains(err.Error(), "needs a hub") {
		t.Errorf("a two-socket fixture with no hub to start: %v, want refused", err)
	}
}

// TestARegisteredHostIsListedAndFollowsItsRestarts: Register lists the Host
// where a hub (or `craze attach`) finds hosts — its id, session, incarnation,
// provider, workspace and start — and serves on the socket the entry names;
// a restart's new incarnation is written into the entry; closing the
// registration unlists it. Two Hosts with distinct ids are listed side by
// side in one registry; the negative control is a second Host with the same
// id, which cannot register beside the first (its lifetime lock is held), and
// a Host registered twice.
func TestARegisteredHostIsListedAndFollowsItsRestarts(t *testing.T) {
	env := fixtureRegistry(t)
	a, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	regA, err := a.Register(env)
	if err != nil {
		t.Fatal(err)
	}
	serveFixtureHost(t, a, regA.Listener(), regA)
	b, err := New(Options{HostID: standInHostID, CrazeSessionID: standInSession, Workspace: "/elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	regB, err := b.Register(env)
	if err != nil {
		t.Fatal(err)
	}
	serveFixtureHost(t, b, regB.Listener(), regB)

	byID := func() map[string]rundir.Entry {
		t.Helper()
		entries, err := rundir.Hosts(env)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]rundir.Entry{}
		for _, e := range entries {
			out[e.HostID] = e
		}
		return out
	}
	listed := byID()
	if len(listed) != 2 || listed["0123456789ab"].CrazeSessionID != "session-fake-1" || listed[standInHostID].CrazeSessionID != standInSession ||
		listed[standInHostID].Workspace != "/elsewhere" {
		t.Fatalf("the registry lists %+v, want both Hosts by their own ids", listed)
	}
	st := a.currentEngine().State()
	if e := listed["0123456789ab"]; e.Incarnation != a.Incarnation() || e.Socket != regA.Socket() || !e.Ready ||
		e.Provider != st.Provider.Name || e.ProviderSessionID != st.SessionID || !e.StartedAt.Equal(clockStart) {
		t.Fatalf("Host a is listed as %+v", e)
	}

	// The entry's socket is the Host's.
	nc, err := net.Dial("unix", listed[standInHostID].Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nc.Close() }()
	if err := protocol.WriteLine(nc, protocol.Request{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(`1`), Method: protocol.MethodHello,
		Params: json.RawMessage(`{"protocols":[1],"client":{"kind":"test"}}`)}); err != nil {
		t.Fatal(err)
	}
	line, err := protocol.NewLineReader(nc, protocol.OutboundLineMax).ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if got := helloHostID(t, line); got != standInHostID {
		t.Fatalf("the socket host %s's entry names is answered by %q", standInHostID, got)
	}

	// A restart's incarnation follows into the entry.
	before := a.Incarnation()
	if err := a.Restart(); err != nil {
		t.Fatal(err)
	}
	if e := byID()["0123456789ab"]; e.Incarnation == before || e.Incarnation != a.Incarnation() {
		t.Fatalf("after a restart the entry says incarnation %q; the Host's is %q (was %q)", e.Incarnation, a.Incarnation(), before)
	}

	// Negative controls: the same id twice, and one Host twice.
	dup, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dup.Close(context.Background()) })
	if reg, err := dup.Register(env); err == nil {
		_ = reg.Close()
		t.Fatal("a second Host registered under an id another holds")
	}
	if reg, err := b.Register(env); err == nil {
		_ = reg.Close()
		t.Fatal("a Host registered twice")
	}

	// Closing a registration unlists the Host.
	if err := regB.Close(); err != nil {
		t.Fatal(err)
	}
	if listed := byID(); len(listed) != 1 || listed["0123456789ab"].HostID == "" {
		t.Fatalf("after host b's registration closed the registry lists %+v", listed)
	}
}
