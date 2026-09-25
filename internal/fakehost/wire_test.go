package fakehost

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/charliek/craze/internal/control/wiretest"
	"github.com/charliek/craze/internal/protocol"
)

// update re-records every fixture's s2c lines from its scripted c2s and op
// lines: go test ./internal/fakehost/... -run TestWireFixtures -update. The
// committed fixtures must pass TestWireFixtures without it.
var update = flag.Bool("update", false, "re-record the wire fixtures' s2c lines")

// fixtureTimeout bounds every wait the runner makes for a line the host owes:
// long enough for CI, and the runner never sleeps-and-hopes instead of
// reading with this deadline.
const fixtureTimeout = 10 * time.Second

// fixtureLine is one line of a wire fixture (plan 027 §3.11's format, as C9's
// brief narrows it to internal/fakehost/testdata/wire/): a wire message
// ({"conn": N, "dir": "c2s"|"s2c", "msg": {…}}) or a host-side script step
// ({"dir": "op", "op": {…}}), which is not a wire message at all — it says
// how the fake host itself was driven (Host.Do). An s2c line's msg may be
// absent (a bare {"conn": N, "dir": "s2c"} marker): -update fills it in from
// what the host actually sends; a replay without one first is an error, not a
// silent skip.
type fixtureLine struct {
	Conn int             `json:"conn,omitempty"`
	Dir  string          `json:"dir"`
	Msg  json.RawMessage `json:"msg,omitempty"`
	Op   json.RawMessage `json:"op,omitempty"`
	// Invalid marks a c2s line that is deliberately not a well-formed
	// request of a method protocol 1 defines with today's params (fixture
	// 10's "unknown method" and "unknown params" sub-cases): the runner
	// sends it as it stands and does not hold it to the request schema,
	// which such a line is designed never to pass. Every other line, c2s and
	// s2c alike, is schema-checked.
	Invalid bool `json:"invalid,omitempty"`
}

// rawFixtureLine is one line of the file, its own bytes kept beside its
// parse: an unchanged line (c2s, op, or a replay match) is written back
// verbatim, byte for byte, so a fixture's formatting is never perturbed by
// -update touching an unrelated line.
type rawFixtureLine struct {
	raw    []byte
	parsed fixtureLine
}

func readFixtureLines(t *testing.T, path string) []rawFixtureLine {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var out []rawFixtureLine
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var fl fixtureLine
		if err := json.Unmarshal(line, &fl); err != nil {
			t.Fatalf("%s: %v: %s", path, err, line)
		}
		out = append(out, rawFixtureLine{raw: append([]byte(nil), line...), parsed: fl})
	}
	return out
}

// incarnations maps a fake host's real, randomly minted incarnations to
// their fixture placeholders (INCARNATION-1, INCARNATION-2 after a restart,
// …) and back: the one thing a Host cannot pin (host.go's doc comment).
// Substitution is exact string replacement of values the host itself
// reported, never a guess at their shape.
type incarnations struct {
	toPlaceholder map[string]string
	toReal        map[string]string
}

func newIncarnations() *incarnations {
	return &incarnations{toPlaceholder: map[string]string{}, toReal: map[string]string{}}
}

// learn records real as the next placeholder (INCARNATION-1, then
// INCARNATION-2, …), if it is not known already — a restart may be followed
// by a script that never again needs the old incarnation, but the map keeps
// it anyway, since a cursor naming an old incarnation is exactly fixture 3.
func (m *incarnations) learn(real string) {
	if _, ok := m.toPlaceholder[real]; ok {
		return
	}
	ph := fmt.Sprintf("INCARNATION-%d", len(m.toPlaceholder)+1)
	m.toPlaceholder[real] = ph
	m.toReal[ph] = real
}

// toWire replaces every known real incarnation in b with its placeholder,
// for a line the host produced (s2c) on its way into the fixture.
func (m *incarnations) toWire(b []byte) []byte {
	for real, ph := range m.toPlaceholder {
		b = bytes.ReplaceAll(b, []byte(real), []byte(ph))
	}
	return b
}

// fromWire replaces every known placeholder in b with its real incarnation,
// for a fixture's c2s line on its way to the host.
func (m *incarnations) fromWire(b []byte) []byte {
	for ph, real := range m.toReal {
		b = bytes.ReplaceAll(b, []byte(ph), []byte(real))
	}
	return b
}

// fixtureConn is one connection the runner opened, named by the fixture's
// conn number: a raw NDJSON socket, and this connection's own book of which
// method each of its outstanding request ids named — a response carries no
// method, so this is the only way to check one against its schema
// (host_test.go's client.methods, the same idea).
type fixtureConn struct {
	nc      *net.UnixConn
	lr      *protocol.LineReader
	methods map[string]string
}

func (c *fixtureConn) readLine(t *testing.T) []byte {
	t.Helper()
	if err := c.nc.SetReadDeadline(time.Now().Add(fixtureTimeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	line, err := c.lr.ReadLine()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return line
}

// noteRequest records id's method from a c2s line the runner is about to
// send, so a later response on this connection can be checked against it.
func (c *fixtureConn) noteRequest(line []byte) {
	var req protocol.Request
	if json.Unmarshal(line, &req) != nil || req.Method == "" {
		return
	}
	if len(req.ID) == 0 {
		return
	}
	c.methods[string(req.ID)] = req.Method
}

// methodFor is line's method for wiretest.Server: line's own if it is a
// notification, or this connection's note for its id if it is a response.
func (c *fixtureConn) methodFor(line []byte) string {
	var probe map[string]json.RawMessage
	if json.Unmarshal(line, &probe) != nil {
		return ""
	}
	if m, ok := probe["method"]; ok {
		var s string
		_ = json.Unmarshal(m, &s)
		return s
	}
	if id, ok := probe["id"]; ok {
		return c.methods[string(id)]
	}
	return ""
}

// fixtureRunner drives one fixture file against one fresh Host.
type fixtureRunner struct {
	t      *testing.T
	h      *Host
	socket string
	conns  map[int]*fixtureConn
	inc    *incarnations
}

func newFixtureRunner(t *testing.T) *fixtureRunner {
	t.Helper()
	h, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	// A short temp dir: t.TempDir() can overflow sun_path on macOS
	// (internal/control's own tests avoid it the same way).
	dir, err := os.MkdirTemp("", "czfh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- h.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
		defer cancel()
		_ = h.Close(ctx)
		<-served
	})
	inc := newIncarnations()
	inc.learn(h.Incarnation())
	return &fixtureRunner{t: t, h: h, socket: socket, conns: map[int]*fixtureConn{}, inc: inc}
}

func (r *fixtureRunner) conn(n int) *fixtureConn {
	if c, ok := r.conns[n]; ok {
		return c
	}
	nc, err := net.Dial("unix", r.socket)
	if err != nil {
		r.t.Fatalf("dial conn %d: %v", n, err)
	}
	c := &fixtureConn{nc: nc.(*net.UnixConn), lr: protocol.NewLineReader(nc, protocol.OutboundLineMax), methods: map[string]string{}}
	r.conns[n] = c
	r.t.Cleanup(func() { _ = c.nc.Close() })
	return c
}

// run walks lines in file order: c2s lines are sent, op lines run, s2c lines
// read and checked (or, with update, recorded). It returns the file's bytes
// to write back with -update; run without update returns nil.
func (r *fixtureRunner) run(lines []rawFixtureLine, update bool) []byte {
	var out bytes.Buffer
	for i, rl := range lines {
		fl := rl.parsed
		switch fl.Dir {
		case "op":
			r.runOp(fl.Op)
			out.Write(rl.raw)
			out.WriteByte('\n')
		case "c2s":
			c := r.conn(fl.Conn)
			line := r.inc.fromWire(append([]byte(nil), fl.Msg...))
			if !fl.Invalid {
				if err := wiretest.Default().Request(line); err != nil {
					r.t.Fatalf("line %d: c2s off the schema: %v", i+1, err)
				}
			}
			c.noteRequest(line)
			if _, err := c.nc.Write(append(line, '\n')); err != nil {
				r.t.Fatalf("line %d: write: %v", i+1, err)
			}
			out.Write(rl.raw)
			out.WriteByte('\n')
		case "s2c":
			c := r.conn(fl.Conn)
			got := c.readLine(r.t)
			got = r.inc.toWire(got)
			if err := wiretest.Default().Server(c.methodFor(got), got); err != nil {
				r.t.Fatalf("line %d: s2c off the schema: %v (%s)", i+1, err, got)
			}
			if update {
				// protocol.MarshalLine, not json.Marshal: encoding/json's
				// default HTML-escaping pass runs over the whole buffer,
				// json.RawMessage included, and would rewrite a literal "&"
				// in got as "&" — the wrapper would then hold bytes the
				// real server never wrote (plan 027 X6's escaping rule, the
				// same reason the server itself never hand-builds JSON
				// around embedded codec output).
				b, err := protocol.MarshalLine(fixtureLine{Conn: fl.Conn, Dir: "s2c", Msg: json.RawMessage(got)})
				if err != nil {
					r.t.Fatalf("line %d: %v", i+1, err)
				}
				out.Write(b)
				continue
			}
			if len(fl.Msg) == 0 {
				r.t.Fatalf("line %d: no recorded s2c line; run with -update first", i+1)
			}
			if !bytes.Equal(got, fl.Msg) {
				r.t.Fatalf("line %d: s2c mismatch:\n got  %s\n want %s", i+1, got, fl.Msg)
			}
			out.Write(rl.raw)
			out.WriteByte('\n')
		default:
			r.t.Fatalf("line %d: unknown dir %q", i+1, fl.Dir)
		}
	}
	return out.Bytes()
}

func (r *fixtureRunner) runOp(raw json.RawMessage) {
	var probe struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		r.t.Fatalf("op: %v", err)
	}
	if err := r.h.Do(raw); err != nil {
		r.t.Fatalf("op %s: %v", probe.Name, err)
	}
	if probe.Name == "restart" {
		r.inc.learn(r.h.Incarnation())
	}
}

// runFixture runs one fixture file by name (testdata/wire/<name>.ndjson).
func runFixture(t *testing.T, name string) {
	t.Helper()
	path := filepath.Join("testdata", "wire", name+".ndjson")
	lines := readFixtureLines(t, path)
	r := newFixtureRunner(t)
	out := r.run(lines, *update)
	if *update {
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
}

// TestWireFixtures replays every wire fixture against a fresh, in-process
// fake host, byte for byte, with every line schema-checked (plan 027 A10,
// §3.11). Run with -update to re-record every fixture's s2c lines from its
// scripted c2s and op lines; the committed fixtures pass without it.
func TestWireFixtures(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "wire"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".ndjson" {
			continue
		}
		names = append(names, e.Name()[:len(e.Name())-len(".ndjson")])
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no fixtures under testdata/wire")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) { runFixture(t, name) })
	}
}
