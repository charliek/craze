package fakehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// toWire replaces the real incarnation named by every JSON member literally
// called "incarnation" — at any depth: the attach reply's after.incarnation,
// a cursor's, the info document's, a snapshot's — with its placeholder, for
// a line the host produced (s2c) on its way into the fixture. It never
// touches any other byte of the line (C9a review item 4): a fixture's own
// event text that happens to spell a real incarnation's UUID, or the literal
// string "INCARNATION-1", is carried through unchanged, since neither is the
// value of a member named "incarnation".
func (m *incarnations) toWire(b []byte) []byte {
	return m.rewrite(b, func(raw []byte) []byte {
		for real, ph := range m.toPlaceholder {
			raw = bytes.ReplaceAll(raw, []byte(real), []byte(ph))
		}
		return raw
	})
}

// fromWire replaces every known placeholder named by an "incarnation" member
// with its real incarnation, for a fixture's c2s line on its way to the
// host — the same confinement as toWire, and for the same reason: a c2s
// line's own params carry a cursor's incarnation (fixtures 2–4), never
// anywhere else a placeholder could appear.
func (m *incarnations) fromWire(b []byte) []byte {
	return m.rewrite(b, func(raw []byte) []byte {
		for ph, real := range m.toReal {
			raw = bytes.ReplaceAll(raw, []byte(ph), []byte(real))
		}
		return raw
	})
}

// rewrite runs rewriteIncarnations and panics on its error: every line this
// package ever hands it is one JSON value the host itself wrote or the
// fixture is about to send (both schema-checked besides), so a parse
// failure here is a bug in this file, not a fixture's — panicking finds it
// far faster than a silent pass-through would.
func (m *incarnations) rewrite(b []byte, sub func([]byte) []byte) []byte {
	out, err := rewriteIncarnations(b, sub)
	if err != nil {
		panic(fmt.Sprintf("fakehost: rewriting incarnations: %v", err))
	}
	return out
}

// rewriteIncarnations parses b as one JSON value (compact, as
// protocol.MarshalLine produces: no whitespace, but none is assumed) and
// returns a copy with sub applied to the raw bytes (quotes included) of
// every string value of an object member literally named "incarnation", at
// any depth — an exact structural match, never a substring match against the
// whole line (C9a review item 4). Every other byte, member names, other
// values, punctuation, is copied verbatim, so a line with no "incarnation"
// member anywhere comes back byte-identical, and one substitution changes
// only the bytes inside its own pair of quotes: byte-exact replay survives
// the round trip (toWire then fromWire, or the reverse) even though the
// value itself changes length (a UUID is 36 bytes, "INCARNATION-1" is 14).
func rewriteIncarnations(b []byte, sub func([]byte) []byte) ([]byte, error) {
	w := &jsonWalker{b: b, sub: sub}
	if err := w.parseValue(); err != nil {
		return nil, err
	}
	w.skipWS()
	if w.i != len(w.b) {
		return nil, fmt.Errorf("trailing bytes at %d", w.i)
	}
	return w.out.Bytes(), nil
}

// jsonWalker is rewriteIncarnations' one recursive-descent pass over b: it
// copies every byte to out as it goes, except that object's method
// substitutes a member named "incarnation"'s string value through sub
// instead of copying it. It does not interpret JSON otherwise — a string's
// content is never unescaped, only scanned for its own closing quote (so an
// escaped backslash or quote is skipped two bytes at a time, correctly,
// without decoding it) — since every byte not itself substituted must come
// back exactly as it went in.
type jsonWalker struct {
	b   []byte
	i   int
	out bytes.Buffer
	sub func([]byte) []byte
}

func (w *jsonWalker) skipWS() {
	for w.i < len(w.b) {
		switch w.b[w.i] {
		case ' ', '\t', '\n', '\r':
			w.out.WriteByte(w.b[w.i])
			w.i++
		default:
			return
		}
	}
}

// parseValue copies one JSON value (object, array, string, or any other
// scalar — number, true, false, null) at the current position to out.
func (w *jsonWalker) parseValue() error {
	w.skipWS()
	if w.i >= len(w.b) {
		return errors.New("unexpected end of JSON")
	}
	switch w.b[w.i] {
	case '{':
		return w.parseObject()
	case '[':
		return w.parseArray()
	case '"':
		raw, err := w.scanString()
		if err != nil {
			return err
		}
		w.out.Write(raw)
		return nil
	default:
		return w.parseScalar()
	}
}

// scanString returns one string token's raw bytes (quotes included,
// contents still escaped) and advances past it, without writing to out: the
// caller decides whether to copy it verbatim or substitute it.
func (w *jsonWalker) scanString() ([]byte, error) {
	start := w.i
	if w.i >= len(w.b) || w.b[w.i] != '"' {
		return nil, fmt.Errorf("expected string at byte %d", w.i)
	}
	w.i++
	for {
		if w.i >= len(w.b) {
			return nil, errors.New("unterminated string")
		}
		switch w.b[w.i] {
		case '\\':
			// The escaped byte, whatever it is (including a \u escape's
			// leading 'u'): never '"' or '\\' itself, so skipping it two
			// bytes at a time can never mistake it for the closing quote,
			// and a \u escape's four hex digits are then read as ordinary
			// bytes next — never '"' or '\\' either.
			w.i += 2
		case '"':
			w.i++
			return w.b[start:w.i], nil
		default:
			w.i++
		}
	}
}

// parseScalar copies a number, true, false, or null verbatim: everything up
// to the next structural byte or whitespace.
func (w *jsonWalker) parseScalar() error {
	start := w.i
	for w.i < len(w.b) {
		switch w.b[w.i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			goto done
		}
		w.i++
	}
done:
	if w.i == start {
		return fmt.Errorf("empty value at byte %d", start)
	}
	w.out.Write(w.b[start:w.i])
	return nil
}

func (w *jsonWalker) parseArray() error {
	w.out.WriteByte('[')
	w.i++
	w.skipWS()
	if w.i < len(w.b) && w.b[w.i] == ']' {
		w.out.WriteByte(']')
		w.i++
		return nil
	}
	for {
		if err := w.parseValue(); err != nil {
			return err
		}
		w.skipWS()
		if w.i >= len(w.b) {
			return errors.New("unterminated array")
		}
		switch w.b[w.i] {
		case ',':
			w.out.WriteByte(',')
			w.i++
			w.skipWS()
		case ']':
			w.out.WriteByte(']')
			w.i++
			return nil
		default:
			return fmt.Errorf("expected , or ] at byte %d", w.i)
		}
	}
}

// parseObject copies an object, substituting through sub the string value of
// every member literally named "incarnation" — the one place this walker's
// pass differs from a byte-for-byte copy.
func (w *jsonWalker) parseObject() error {
	w.out.WriteByte('{')
	w.i++
	w.skipWS()
	if w.i < len(w.b) && w.b[w.i] == '}' {
		w.out.WriteByte('}')
		w.i++
		return nil
	}
	for {
		w.skipWS()
		keyRaw, err := w.scanString()
		if err != nil {
			return err
		}
		w.out.Write(keyRaw)
		var key string
		if err := json.Unmarshal(keyRaw, &key); err != nil {
			return fmt.Errorf("member name %s: %w", keyRaw, err)
		}
		w.skipWS()
		if w.i >= len(w.b) || w.b[w.i] != ':' {
			return fmt.Errorf("expected : at byte %d", w.i)
		}
		w.out.WriteByte(':')
		w.i++
		w.skipWS()
		if key == "incarnation" && w.i < len(w.b) && w.b[w.i] == '"' {
			raw, err := w.scanString()
			if err != nil {
				return err
			}
			w.out.Write(w.sub(raw))
		} else if err := w.parseValue(); err != nil {
			return err
		}
		w.skipWS()
		if w.i >= len(w.b) {
			return errors.New("unterminated object")
		}
		switch w.b[w.i] {
		case ',':
			w.out.WriteByte(',')
			w.i++
		case '}':
			w.out.WriteByte('}')
			w.i++
			return nil
		default:
			return fmt.Errorf("expected , or } at byte %d", w.i)
		}
	}
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
