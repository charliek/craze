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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control/wiretest"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/tui"
	"github.com/charliek/craze/internal/version"
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
	Conn int `json:"conn,omitempty"`
	// Sock is which socket a wire line's connection is to, in a two-socket
	// fixture (plan 032 §3.15): "hub" for the hub's, "" (or "host") for the
	// fake host's. A connection's first line decides it for good — a later
	// line of the same connection may leave it out, and may not name the
	// other — and a fixture with any hub line is a two-socket one
	// (fixtureNeedsHub).
	Sock string          `json:"sock,omitempty"`
	Dir  string          `json:"dir"`
	Msg  json.RawMessage `json:"msg,omitempty"`
	Op   json.RawMessage `json:"op,omitempty"`
	// Host is a fixture's first line when its Host is built with plan
	// 030's opt-ins ({"dir": "host", "host": {…}}; fixtureHost): not a wire
	// message, and not a step — how the Host itself was built. A fixture
	// without one runs against Options{}, exactly as every fixture before
	// plan 030 does (X1).
	Host *fixtureHost `json:"host,omitempty"`
	// Invalid marks a c2s line that is deliberately not a well-formed
	// request of a method protocol 1 defines with today's params (fixture
	// 10's "unknown method" and "unknown params" sub-cases): the runner
	// sends it as it stands and does not hold it to the request schema,
	// which such a line is designed never to pass. Every other line, c2s and
	// s2c alike, is schema-checked.
	Invalid bool `json:"invalid,omitempty"`
}

// fixtureHost is a fixture's host line: which of Options' plan 030 opt-ins
// its Host is built with (Options.Stop, PermissionMode, StartedAt, RowFacts),
// and plan 031's model catalog (Options.Models).
type fixtureHost struct {
	// HubCreates is the hub's, in a two-socket fixture: one that serves
	// session.create (hub.Options.Creates), its hosts this test binary run
	// as a spawned fake host (hub_fixture_test.go). The fixture Host is
	// built as the rest of the line says.
	HubCreates     bool                    `json:"hubCreates,omitempty"`
	Stop           bool                    `json:"stop,omitempty"`
	PermissionMode protocol.PermissionMode `json:"permissionMode,omitempty"`
	StartedAt      bool                    `json:"startedAt,omitempty"`
	RowFacts       bool                    `json:"rowFacts,omitempty"`
	// Models is written as the catalog's own models are: id, name and, for
	// a remembered one, recent.
	Models []protocol.CatalogModel `json:"models,omitempty"`
}

// options is the Options a host line asks for; nil is the zero value.
func (fh *fixtureHost) options() Options {
	if fh == nil {
		return Options{}
	}
	var models []agent.ModelInfo
	for _, m := range fh.Models {
		models = append(models, agent.ModelInfo{ID: m.ID, Name: m.Name, Recent: m.Recent})
	}
	return Options{Stop: fh.Stop, PermissionMode: fh.PermissionMode, StartedAt: fh.StartedAt, RowFacts: fh.RowFacts, Models: models}
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
	return parseFixtureLines(t, path, b)
}

// parseFixtureLines is a fixture's lines from its bytes; path names it in an
// error.
func parseFixtureLines(t *testing.T, path string, b []byte) []rawFixtureLine {
	t.Helper()
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
	return rewriteMembers(b, func(key string, raw []byte) []byte {
		if key == "incarnation" && raw[0] == '"' {
			return sub(raw)
		}
		return raw
	})
}

// rewriteMembers is rewriteIncarnations' pass, for any member: sub is handed
// the name and the raw bytes (a string's quotes included) of every object
// member whose value is a scalar — a string, number, true, false or null —
// at any depth, and what it returns is written in their place (raw itself
// leaves them); every other byte is copied verbatim.
func rewriteMembers(b []byte, sub func(key string, raw []byte) []byte) ([]byte, error) {
	return rewritePaths(b, func(path []string, raw []byte) []byte { return sub(path[len(path)-1], raw) })
}

// rewritePaths is rewriteMembers with each member's whole path in place of
// its name: the member names from the outermost object in, an array's
// element "[]" — {"a":[{"b":1}]}'s b is [a [] b]. sub must not keep path.
func rewritePaths(b []byte, sub func(path []string, raw []byte) []byte) ([]byte, error) {
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

// jsonWalker is rewritePaths' one recursive-descent pass over b: it copies
// every byte to out as it goes, except that object's method writes each
// member's scalar value through sub instead of copying it, path the way to
// it. It does not interpret JSON otherwise — a string's
// content is never unescaped, only scanned for its own closing quote (so an
// escaped backslash or quote is skipped two bytes at a time, correctly,
// without decoding it) — since every byte not itself substituted must come
// back exactly as it went in.
type jsonWalker struct {
	b    []byte
	i    int
	out  bytes.Buffer
	sub  func(path []string, raw []byte) []byte
	path []string
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
	raw, err := w.scanScalar()
	if err != nil {
		return err
	}
	w.out.Write(raw)
	return nil
}

// scanScalar returns a number, true, false or null's raw bytes and advances
// past them, without writing to out.
func (w *jsonWalker) scanScalar() ([]byte, error) {
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
		return nil, fmt.Errorf("empty value at byte %d", start)
	}
	return w.b[start:w.i], nil
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
		w.path = append(w.path, "[]")
		if err := w.parseValue(); err != nil {
			return err
		}
		w.path = w.path[:len(w.path)-1]
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

// parseObject copies an object, writing every member's scalar value through
// sub — the one place this walker's pass differs from a byte-for-byte copy.
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
		w.path = append(w.path, key)
		switch {
		case w.i < len(w.b) && w.b[w.i] == '"':
			raw, err := w.scanString()
			if err != nil {
				return err
			}
			w.out.Write(w.sub(w.path, raw))
		case w.i < len(w.b) && w.b[w.i] != '{' && w.b[w.i] != '[':
			raw, err := w.scanScalar()
			if err != nil {
				return err
			}
			w.out.Write(w.sub(w.path, raw))
		default:
			if err := w.parseValue(); err != nil {
				return err
			}
		}
		w.path = w.path[:len(w.path)-1]
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
	// sock is the socket it was dialed to (sockHost or sockHub).
	sock string
}

// The sockets a fixture's connections dial (fixtureLine.Sock).
const (
	sockHost = "host"
	sockHub  = "hub"
)

// sockOf is a line's socket by name: "" is the host's.
func sockOf(fl fixtureLine) (string, error) {
	switch fl.Sock {
	case "", sockHost:
		return sockHost, nil
	case sockHub:
		return sockHub, nil
	}
	return "", fmt.Errorf("conn %d: no socket called %q (want %q or %q)", fl.Conn, fl.Sock, sockHost, sockHub)
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

// fixtureRunner drives one fixture file against one fresh Host — and, in a
// two-socket fixture, the hub in front of it.
type fixtureRunner struct {
	t *testing.T
	h *Host
	// sockets is each socket a connection may dial, by name: sockHost always,
	// sockHub in a two-socket fixture (newTwoSocketRunner).
	sockets map[string]string
	conns   map[int]*fixtureConn
	inc     *incarnations
	// hubID is the hub's id in a two-socket fixture ("" for none, or a hub
	// whose id is to be shown as it is): what hubToWire names HUB-ID.
	hubID string
	// env is a two-socket fixture's registry; created is every host a
	// session.create of the fixture's started, as the registry listed it
	// (learnCreated): their values hubToWire names by placeholder.
	env     rundir.Env
	created []rundir.Entry
}

// hubStarter serves a hub for a two-socket fixture (plan 032 §3.15): handed
// the registry the fixture's Host is listed in (rundir.Env, a HOME-like root
// and a runtime tree of the fixture's own), it starts a hub that finds the
// Host there — one that creates sessions when creates says so (the host
// line's hubCreates) — cleans it up through t, and returns the hub's socket
// and its id (hubToWire's HUB-ID; "" to name none).
type hubStarter func(t *testing.T, env rundir.Env, creates bool) (socket, hubID string)

// fixtureHub is the hub every two-socket fixture runs against: the real one,
// internal/hub's Run in this process (hub_fixture_test.go installs it); nil,
// a fixture that names the hub fails, saying why (fixtureHubFor).
var fixtureHub hubStarter

// What a two-socket fixture names by placeholder in the lines the hub writes
// (hubToWire), as the incarnation is named everywhere: the values of this run
// that no fixture can pin.
const (
	// hubIDPlaceholder is the hub's id — minted per hub process — as its
	// hello's endpoint.hostId and its roster's epoch.
	hubIDPlaceholder = `"HUB-ID"`
	// hubVersionPlaceholder is the hub's craze version (version.Version),
	// which moves with every release.
	hubVersionPlaceholder = `"HUB-VERSION"`
	// pidPlaceholder is this test process's pid where a hub line carries it:
	// the in-process hub's hello's endpoint.pid, and a roster row's host.pid
	// — the pid of the fixture Host's registry entry, which the Host was
	// bound by. It is no pid at all — above any pid_max (Linux's is at most
	// 2^22, macOS's 99998) — so no value the hub wrote can be mistaken for
	// it, the way HUB-ID can be no host id. A created session's row's
	// host.pid, its host's — a child's — is named by it too.
	pidPlaceholder = `999999999`
	// createdIDPlaceholder and createdIncPlaceholder are a host a
	// session.create started (plan 032 §3.10): its id, which the hub minted,
	// and its session's incarnation, which its Stub did — wherever a hub line
	// carries either. The id's is twelve hex digits, the schema's form for a
	// host id where a line carries one, and one no run of the hub's random
	// minting is ever expected to give a fixture's host.
	createdIDPlaceholder  = `"cccccccccccc"`
	createdIncPlaceholder = `"CREATED-INCARNATION"`
)

// learnCreated reads the fixture's registry for the hosts a session.create
// started — an entry carrying a requestId — and keeps each it has not seen
// (by host id): values the run itself reported, as the incarnation's are,
// read before every hub line is compared. A host gone from the registry since
// stays learnt.
func (r *fixtureRunner) learnCreated() {
	if r.env.Home == "" {
		return
	}
	entries, err := rundir.Hosts(r.env)
	if err != nil {
		r.t.Fatalf("the fixture's registry: %v", err)
	}
	for _, e := range entries {
		if e.RequestID == "" || slices.ContainsFunc(r.created, func(c rundir.Entry) bool { return c.HostID == e.HostID }) {
			continue
		}
		r.created = append(r.created, e)
	}
}

// createdValue is the placeholder of raw, a value at the end of path in a
// line answering or notifying method, when it is a created host's — its id
// (as any hostId), its incarnation (as any incarnation), its pid (as a
// session.create's result's host.pid) — and nil when it is none of them.
func (r *fixtureRunner) createdValue(path []string, raw []byte, method string) []byte {
	key, v := path[len(path)-1], string(raw)
	for _, e := range r.created {
		switch {
		case key == "hostId" && v == strconv.Quote(e.HostID):
			return []byte(createdIDPlaceholder)
		case key == "incarnation" && e.Incarnation != "" && v == strconv.Quote(e.Incarnation):
			return []byte(createdIncPlaceholder)
		case method == protocol.MethodSessionCreate && strings.Join(path, ".") == "result.session.host.pid" && v == strconv.Itoa(e.PID):
			return []byte(pidPlaceholder)
		}
	}
	return nil
}

// hubToWire names by placeholder, in a line the hub wrote — method being the
// method it answers or notifies (fixtureConn.methodFor) — the values a
// fixture cannot pin: every member named hostId or epoch whose value is the
// hub's id, crazeVersion whose value is version.Version, and, where this
// process's pid is the value, the hub's hello's endpoint.pid (endpoint.kind
// hub) and a roster row's host.pid (hubPIDAt) — exact values the run itself
// knows, never a guess at their shape, as the incarnation's rewrite is. Every
// other byte is the hub's: a pid anywhere else — a host's hello through a
// splice, a member of a forwarded row — is compared as it stands.
func (r *fixtureRunner) hubToWire(b []byte, method string) []byte {
	id, ver, pid := `"`+r.hubID+`"`, strconv.Quote(version.Version), strconv.Itoa(os.Getpid())
	var probe struct {
		Result *struct {
			Endpoint *struct {
				Kind string `json:"kind"`
			} `json:"endpoint"`
		} `json:"result"`
	}
	_ = json.Unmarshal(b, &probe) // a line it does not fit names no endpoint
	hubHello := method == protocol.MethodHello && probe.Result != nil && probe.Result.Endpoint != nil &&
		probe.Result.Endpoint.Kind == protocol.EndpointHub
	r.learnCreated()
	out, err := rewritePaths(b, func(path []string, raw []byte) []byte {
		if p := r.createdValue(path, raw, method); p != nil {
			return p
		}
		switch key, v := path[len(path)-1], string(raw); {
		case (key == "hostId" || key == "epoch") && r.hubID != "" && v == id:
			return []byte(hubIDPlaceholder)
		case key == "crazeVersion" && v == ver:
			return []byte(hubVersionPlaceholder)
		case v == pid && hubPIDAt(path, method, hubHello):
			return []byte(pidPlaceholder)
		}
		return raw
	})
	if err != nil {
		panic(fmt.Sprintf("fakehost: rewriting the hub's values: %v", err))
	}
	return out
}

// hubPIDAt says path is one of the two places a hub line carries a pid of
// the hub's run: its hello's endpoint.pid, and a roster row's host.pid in a
// sessions.list or sessions.subscribe reply or a roster notification's
// upserts.
func hubPIDAt(path []string, method string, hubHello bool) bool {
	switch p := strings.Join(path, "."); method {
	case protocol.MethodHello:
		return hubHello && p == "result.endpoint.pid"
	case protocol.MethodSessionsList, protocol.MethodSessionsSubscribe:
		return p == "result.sessions.[].host.pid"
	case protocol.NotifyRoster:
		return p == "params.upserts.[].host.pid"
	}
	return false
}

// fixtureNeedsHub reports whether any line of a fixture is to the hub's
// socket: a two-socket fixture.
func fixtureNeedsHub(lines []rawFixtureLine) bool {
	for _, rl := range lines {
		if rl.parsed.Sock == sockHub {
			return true
		}
	}
	return false
}

// fixtureHubFor is the hub a fixture runs against: none for a one-socket
// fixture, start for a two-socket one, and an error for a two-socket one when
// there is no hub to start.
func fixtureHubFor(lines []rawFixtureLine, start hubStarter) (hubStarter, error) {
	if !fixtureNeedsHub(lines) {
		return nil, nil
	}
	if start == nil {
		return nil, errors.New("a two-socket fixture needs a hub, and this build installs none (fixtureHub)")
	}
	return start, nil
}

func newFixtureRunner(t *testing.T, o Options) *fixtureRunner {
	t.Helper()
	return newHookedFixtureRunner(t, o, hostHooks{})
}

// newHookedFixtureRunner is newFixtureRunner over a Host built with a test's
// seams (hostHooks).
func newHookedFixtureRunner(t *testing.T, o Options, hooks hostHooks) *fixtureRunner {
	t.Helper()
	h, err := newHost(o, hooks)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(shortTempDir(t, "czfh-"), "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	serveFixtureHost(t, h, l, nil)
	return newRunnerFor(t, h, map[string]string{sockHost: socket})
}

// newTwoSocketRunner is newFixtureRunner for a two-socket fixture (plan 032
// §3.15): the Host is bound and listed in a registry of the fixture's own
// (Register: a HOME-like root and a runtime tree under one short directory),
// exactly where a hub looks for hosts, and start serves the hub that finds it
// there. A connection dials the Host's socket or the hub's, as its lines say
// (fixtureLine.Sock).
func newTwoSocketRunner(t *testing.T, o Options, creates bool, start hubStarter) *fixtureRunner {
	t.Helper()
	h, err := newHost(o, hostHooks{})
	if err != nil {
		t.Fatal(err)
	}
	env := fixtureRegistry(t)
	reg, err := h.Register(env)
	if err != nil {
		_ = h.Close(context.Background())
		t.Fatal(err)
	}
	serveFixtureHost(t, h, reg.Listener(), reg)
	sock, id := start(t, env, creates)
	r := newRunnerFor(t, h, map[string]string{sockHost: reg.Socket(), sockHub: sock})
	r.hubID, r.env = id, env
	return r
}

// fixtureRegistry is a registry of a fixture's own: a fresh 0700 HOME-like
// root (its cache tree, so its registry) with the craze directory and a short
// runtime tree under it.
func fixtureRegistry(t *testing.T) rundir.Env {
	t.Helper()
	home := shortTempDir(t, "czfr-")
	return rundir.Env{Home: home, CrazeDir: filepath.Join(home, ".craze"), CrazeRuntimeDir: filepath.Join(home, "run"), EUID: os.Geteuid()}
}

// shortTempDir is a fresh 0700 directory under /tmp itself, removed at the
// end: t.TempDir() and $TMPDIR can overflow sun_path on macOS
// (internal/control's own tests avoid it the same way).
func shortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serveFixtureHost serves h on l until the test ends, then closes it — and,
// for a registered Host, its registration last, which unlists it.
func serveFixtureHost(t *testing.T, h *Host, l net.Listener, reg *rundir.Host) {
	t.Helper()
	served := make(chan error, 1)
	go func() { served <- h.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
		defer cancel()
		_ = h.Close(ctx)
		<-served
		if reg != nil {
			_ = reg.Close()
		}
	})
}

func newRunnerFor(t *testing.T, h *Host, sockets map[string]string) *fixtureRunner {
	inc := newIncarnations()
	inc.learn(h.Incarnation())
	return &fixtureRunner{t: t, h: h, sockets: sockets, conns: map[int]*fixtureConn{}, inc: inc}
}

// connFor is fl's connection, dialed the first time its number is named, to
// the socket that line names; a later line of the same connection that names
// another socket, or a socket this runner does not serve, is an error.
func (r *fixtureRunner) connFor(fl fixtureLine) (*fixtureConn, error) {
	sock, err := sockOf(fl)
	if err != nil {
		return nil, err
	}
	if c, ok := r.conns[fl.Conn]; ok {
		if fl.Sock != "" && sock != c.sock {
			return nil, fmt.Errorf("conn %d is to the %s's socket, and this line names the %s's", fl.Conn, c.sock, sock)
		}
		return c, nil
	}
	path, ok := r.sockets[sock]
	if !ok {
		return nil, fmt.Errorf("conn %d: this fixture has no %s socket (a hub line makes it a two-socket fixture)", fl.Conn, sock)
	}
	nc, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial conn %d (%s): %w", fl.Conn, sock, err)
	}
	c := &fixtureConn{nc: nc.(*net.UnixConn), lr: protocol.NewLineReader(nc, protocol.OutboundLineMax), methods: map[string]string{}, sock: sock}
	r.conns[fl.Conn] = c
	r.t.Cleanup(func() { _ = c.nc.Close() })
	return c, nil
}

// run walks lines in file order: c2s lines are sent, op lines run, s2c lines
// read and checked (or, with update, recorded). It returns the file's bytes
// to write back with -update; run without update returns nil.
func (r *fixtureRunner) run(lines []rawFixtureLine, update bool) []byte {
	var out bytes.Buffer
	for i, rl := range lines {
		fl := rl.parsed
		switch fl.Dir {
		case "host":
			// Read before the Host was built (runFixture); only a first line
			// may say how it was.
			if i != 0 {
				r.t.Fatalf("line %d: a host line is a fixture's first line or none", i+1)
			}
			out.Write(rl.raw)
			out.WriteByte('\n')
		case "op":
			r.runOp(fl.Op)
			out.Write(rl.raw)
			out.WriteByte('\n')
		case "c2s":
			c, err := r.connFor(fl)
			if err != nil {
				r.t.Fatalf("line %d: %v", i+1, err)
			}
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
			c, err := r.connFor(fl)
			if err != nil {
				r.t.Fatalf("line %d: %v", i+1, err)
			}
			got := c.readLine(r.t)
			got = r.inc.toWire(got)
			if c.sock == sockHub {
				got = r.hubToWire(got, c.methodFor(got))
			}
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
				b, err := protocol.MarshalLine(fixtureLine{Conn: fl.Conn, Sock: fl.Sock, Dir: "s2c", Msg: json.RawMessage(got)})
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

// fixturePath is a fixture file's path by name (testdata/wire/<name>.ndjson).
func fixturePath(name string) string { return filepath.Join("testdata", "wire", name+".ndjson") }

// fixtureHubCreates is whether a fixture's host line asks for a hub that
// creates sessions.
func fixtureHubCreates(lines []rawFixtureLine) bool {
	return len(lines) > 0 && lines[0].parsed.Dir == "host" && lines[0].parsed.Host != nil && lines[0].parsed.Host.HubCreates
}

// fixtureOptions is the Options a fixture's host line asks for (Options{}
// without one).
func fixtureOptions(lines []rawFixtureLine) Options {
	var host *fixtureHost
	if len(lines) > 0 && lines[0].parsed.Dir == "host" {
		host = lines[0].parsed.Host
	}
	return host.options()
}

// runFixture runs one fixture file by name (testdata/wire/<name>.ndjson): a
// one-socket fixture against the Host alone, a two-socket one against the
// Host and the hub in front of it (fixtureHub).
func runFixture(t *testing.T, name string) {
	t.Helper()
	path := fixturePath(name)
	lines := readFixtureLines(t, path)
	hub, err := fixtureHubFor(lines, fixtureHub)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var r *fixtureRunner
	if hub != nil {
		r = newTwoSocketRunner(t, fixtureOptions(lines), fixtureHubCreates(lines), hub)
	} else {
		r = newFixtureRunner(t, fixtureOptions(lines))
	}
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

// heldStub is the Stub as a test's engine drives it (hostHooks.session), with
// every prompt's continuation held back: every method is the Stub's own — the
// optional interfaces engine.New looks for included, promoted through the
// embedded pointer — but Begin, whose continuation runs hold before the Stub's
// own run, so before the Stub opens the turn. The claim is still the Stub's, on
// the engine's goroutine under its lock: the engine has the turn current and
// its started published, and the Stub has not opened it — the window a loaded
// scheduler leaves between the two (WF16), held open for as long as the test
// likes.
type heldStub struct {
	*tui.Stub
	hold func()
}

func (s heldStub) Begin(text string) func(context.Context) (agent.Result, error) {
	run := s.Stub.Begin(text)
	return func(ctx context.Context) (agent.Result, error) {
		s.hold()
		return run(ctx)
	}
}

// TestWireFixturesWaitForTheHungTurnToOpen replays every fixture that hangs a
// prompt (hang_next: 9, 16 and 17) with that prompt's continuation held back
// until the end op has to wait for it (hostHooks.turnOpening) — through every
// line between, fixture 17's tool, text and permission ops included, far
// longer than any scheduler holds one — and each still replays byte for byte
// (WF16). CI's -race run caught fixture 16 once with the end op's cancel
// landing before the Stub had opened the hung turn: the prompt withdrew
// instead — no done, a synthetic ending — and sessions.list's cursor said 3
// where the script says 4. The end op's cancel releases the hold too, so an end
// that no longer waits reproduces exactly that failure rather than a hang.
func TestWireFixturesWaitForTheHungTurnToOpen(t *testing.T) {
	for _, name := range []string{"09-prompt-seen-by-both", "16-last-turn", "17-row-facts"} {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			open := func() { once.Do(func() { close(release) }) }
			var waited atomic.Bool
			hooks := hostHooks{
				session: func(s *tui.Stub) agent.Session {
					return heldStub{Stub: s, hold: func() {
						select {
						case <-release:
						case <-time.After(fixtureTimeout):
						}
					}}
				},
				turnOpening: func() {
					waited.Store(true)
					open()
				},
				cancelled: open,
			}
			lines := readFixtureLines(t, fixturePath(name))
			r := newHookedFixtureRunner(t, fixtureOptions(lines), hooks)
			// Registered after the runner's own cleanup, so it runs first: a
			// replay that fails with the continuation still held never leaves
			// the Host's close waiting out the hold.
			t.Cleanup(open)
			r.run(lines, false)
			if !waited.Load() {
				t.Fatal("the end op never waited for the held turn to open: the replay never met the window it guards")
			}
		})
	}
}

// TestHubToWireNamesOnlyTheHubsPIDs (r19 7): the pid placeholder can be no
// pid — above any pid_max — and hubToWire writes it only where this
// process's pid stands in the hub's hello's endpoint.pid or a roster row's
// host.pid: a pid that is not this process's there is kept, so a hub that
// put another pid in either is a fixture mismatch, and this process's pid
// anywhere else — a host's hello through a splice, a forwarded row's own
// member — is kept as well. The negative controls are the lines kept
// byte for byte.
func TestHubToWireNamesOnlyTheHubsPIDs(t *testing.T) {
	ph, err := strconv.Atoi(pidPlaceholder)
	if err != nil {
		t.Fatal(err)
	}
	// Linux's PID_MAX_LIMIT (2^22) bounds every pid_max; macOS's pids stop at
	// 99998.
	if ph <= 1<<22 || ph == os.Getpid() {
		t.Fatalf("the pid placeholder %d could be a real pid", ph)
	}
	if b, err := os.ReadFile("/proc/sys/kernel/pid_max"); err == nil {
		if max, err := strconv.Atoi(strings.TrimSpace(string(b))); err != nil || ph <= max {
			t.Fatalf("the pid placeholder %d is not above this kernel's pid_max %q", ph, b)
		}
	}
	r := &fixtureRunner{t: t, hubID: "0a1b2c3d4e5f"}
	pid := strconv.Itoa(os.Getpid())
	hostRow := `{"hostId":"0123456789ab","sessionId":"s","host":{"pid":` + pid + `,"crazeVersion":"0.0.0-fakehost","protocol":1},` +
		`"status":"reachable","approximate":false,"row":{"sessionId":"s","pid":` + pid + `,"host":{"pid":` + pid + `}}}`
	named := strings.Replace(hostRow, `"host":{"pid":`+pid, `"host":{"pid":`+pidPlaceholder, 1)
	for _, tc := range []struct {
		name, method, line, want string
	}{
		{"the hub's hello", protocol.MethodHello,
			`{"jsonrpc":"2.0","id":"1","result":{"protocol":1,"endpoint":{"kind":"hub","hostId":"0a1b2c3d4e5f","crazeVersion":"x","pid":` + pid + `}}}`,
			`{"jsonrpc":"2.0","id":"1","result":{"protocol":1,"endpoint":{"kind":"hub","hostId":"HUB-ID","crazeVersion":"x","pid":` + pidPlaceholder + `}}}`},
		{"the hub's hello with another pid", protocol.MethodHello,
			`{"jsonrpc":"2.0","id":"1","result":{"endpoint":{"kind":"hub","pid":4242}}}`,
			`{"jsonrpc":"2.0","id":"1","result":{"endpoint":{"kind":"hub","pid":4242}}}`},
		{"a host's hello through a splice", protocol.MethodHello,
			`{"jsonrpc":"2.0","id":"3","result":{"endpoint":{"kind":"host","pid":` + pid + `}}}`,
			`{"jsonrpc":"2.0","id":"3","result":{"endpoint":{"kind":"host","pid":` + pid + `}}}`},
		{"a list's rows", protocol.MethodSessionsList,
			`{"jsonrpc":"2.0","id":"2","result":{"epoch":"e","cursor":1,"sessions":[` + hostRow + `]}}`,
			`{"jsonrpc":"2.0","id":"2","result":{"epoch":"e","cursor":1,"sessions":[` + named + `]}}`},
		{"a subscription's rows", protocol.MethodSessionsSubscribe,
			`{"jsonrpc":"2.0","id":"2","result":{"subscription":"r-1","sessions":[` + hostRow + `]}}`,
			`{"jsonrpc":"2.0","id":"2","result":{"subscription":"r-1","sessions":[` + named + `]}}`},
		{"a notification's upserts", protocol.NotifyRoster,
			`{"jsonrpc":"2.0","method":"roster","params":{"upserts":[` + hostRow + `],"removes":[]}}`,
			`{"jsonrpc":"2.0","method":"roster","params":{"upserts":[` + named + `],"removes":[]}}`},
		{"a row's host pid copied from hello", protocol.MethodSessionsList,
			`{"jsonrpc":"2.0","id":"2","result":{"sessions":[{"host":{"pid":4242}}]}}`,
			`{"jsonrpc":"2.0","id":"2","result":{"sessions":[{"host":{"pid":4242}}]}}`},
		{"a host's own list through a splice", protocol.MethodSessionsList,
			`{"jsonrpc":"2.0","id":"5","result":{"sessions":[{"sessionId":"s","pid":` + pid + `}]}}`,
			`{"jsonrpc":"2.0","id":"5","result":{"sessions":[{"sessionId":"s","pid":` + pid + `}]}}`},
	} {
		if got := string(r.hubToWire([]byte(tc.line), tc.method)); got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}
