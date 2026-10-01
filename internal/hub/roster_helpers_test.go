package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/control/wiretest"
	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
)

// The roster's tests' rig (roster_test.go). The hosts are in memory
// (memHosts: the hub's registry read and its poll's dial are the test's, each
// host answering on a pipe with the row it holds now), or — where the
// registry itself is the point, a crash its sweep finds — real: fake hosts
// bound in a registry of the test's, in process or as a child of this test
// binary (hostTestChild). The poll's tick and clock, and a subscription's
// flush spacing, are the test's; each Snapshot the roster takes is noted.

// ------------------------------------------------------------- the rig

// rosterRig is a hub in process with the roster's seams.
type rosterRig struct {
	t     *testing.T
	rn    *running
	h     *hub
	sock  string
	ticks chan time.Time
	clk   *rigClock
	flush *flushGate
	snaps *snapLog
}

// rigOpts says which seams a rig takes: the test's clock (frozen, moved by
// hand) or the real one, the flush spacing's gate or real timers, and hk's
// other hooks.
type rigOpts struct {
	clock bool
	gate  bool
	hk    func(*hooks)
}

func newRosterRig(t *testing.T, env rundir.Env, o rigOpts) *rosterRig {
	t.Helper()
	rg := &rosterRig{t: t, ticks: make(chan time.Time), snaps: &snapLog{note: make(chan struct{}, 1)}}
	hk := quiet()
	hk.rosterTicks = rg.ticks
	hk.rosterApplied = rg.snaps.add
	// An attempt's budget is the step's: a host the test holds, or a starved
	// scheduler, never fails an attempt the test did not mean to.
	hk.rosterBudget = step
	if o.clock {
		rg.clk = &rigClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
		hk.rosterNow = rg.clk.now
	}
	if o.gate {
		rg.flush = &flushGate{fire: make(chan time.Time)}
		hk.flushTimer = rg.flush.timer
	}
	if o.hk != nil {
		o.hk(hk)
	}
	rg.rn = runIn(t, env, hk)
	rg.sock = rg.rn.line(t).Socket
	rg.h = rg.rn.serving(t)
	return rg
}

// tick hands the poll one tick: it has taken it once the send returns.
func (rg *rosterRig) tick() {
	rg.t.Helper()
	select {
	case rg.ticks <- time.Now():
	case <-time.After(step):
		rg.t.Fatal("the roster's poll took no tick")
	}
}

// applied waits for a Snapshot the roster took that pred holds for, from
// the from-th on (snapLog.mark), and answers it.
func (rg *rosterRig) applied(what string, from int, pred func(roster.Snapshot) bool) roster.Snapshot {
	rg.t.Helper()
	return rg.snaps.wait(rg.t, what, from, pred)
}

// cursor is the roster's cursor now.
func (rg *rosterRig) cursor() uint64 {
	rg.h.rs.mu.Lock()
	defer rg.h.rs.mu.Unlock()
	return rg.h.rs.cursor
}

// rigClock is the roster's clock, moved by hand.
type rigClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *rigClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *rigClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// flushGate is a test's hand on every subscription's flush spacing: each
// spaced flush waits for a fire (open). A test opens it only once the roster
// has taken the change it means to be flushed (rosterRig.applied): the take
// that follows then holds it, whether the flusher was parked already or
// parks next.
type flushGate struct {
	fire chan time.Time
}

func (g *flushGate) timer(time.Duration) (<-chan time.Time, func()) {
	return g.fire, func() {}
}

// open fires the gate once: the parked flusher flushes.
func (g *flushGate) open(t *testing.T) {
	t.Helper()
	select {
	case g.fire <- time.Now():
	case <-time.After(step):
		t.Fatal("no flusher was parked on the gate")
	}
}

// snapLog is every Snapshot the roster took (hooks.rosterApplied).
type snapLog struct {
	mu    sync.Mutex
	snaps []roster.Snapshot
	note  chan struct{}
}

func (l *snapLog) add(s roster.Snapshot) {
	l.mu.Lock()
	l.snaps = append(l.snaps, s)
	l.mu.Unlock()
	select {
	case l.note <- struct{}{}:
	default:
	}
}

// mark is how many Snapshots were taken so far: a later wait's from.
func (l *snapLog) mark() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.snaps)
}

func (l *snapLog) wait(t *testing.T, what string, from int, pred func(roster.Snapshot) bool) roster.Snapshot {
	t.Helper()
	deadline := time.After(step)
	for {
		l.mu.Lock()
		snaps := slices.Clone(l.snaps[min(from, len(l.snaps)):])
		l.mu.Unlock()
		for _, s := range snaps {
			if pred(s) {
				return s
			}
		}
		select {
		case <-l.note:
		case <-deadline:
			t.Fatalf("%s: no such Snapshot taken within %v (%d taken since %d)", what, step, len(snaps), from)
		}
	}
}

// listedIn is a Snapshot predicate: every id listed, reachable and polled,
// and — exact — no other host.
func listedIn(exact bool, ids ...string) func(roster.Snapshot) bool {
	return func(s roster.Snapshot) bool {
		if exact && len(s.Running) != len(ids) {
			return false
		}
		for _, id := range ids {
			r := snapRow(s, id)
			if r == nil || r.Status != roster.Reachable || !r.Polled {
				return false
			}
		}
		return true
	}
}

// snapRow is id's row in s, nil when it has none.
func snapRow(s roster.Snapshot, id string) *roster.Row {
	for i := range s.Running {
		if s.Running[i].Host.ID == id {
			return &s.Running[i]
		}
	}
	return nil
}

// ------------------------------------------------------- hosts in memory

// memHosts is a registry of hosts that live in memory (installMem): its
// entries are the hub's registry read, and its dial — the hub's poll's —
// answers hello and sessions.list on a pipe, each host with the row it holds
// at that instant.
type memHosts struct {
	t  *testing.T
	mu sync.Mutex
	// entries is the registry; rows each host's row, raw.
	entries map[string]rundir.Entry
	rows    map[string]string
	// down refuses a host's dials; hold makes its answers wait until closed.
	down map[string]bool
	hold map[string]chan struct{}
	// conns is each host's open pipes (its end), for drop.
	conns map[string][]net.Conn
	reads int
	dials int
}

// memSocket is a memory host's socket path, as its entry names it.
func memSocket(id string) string { return "mem:" + id }

// installMem makes m the hub's registry and poll for one test.
func installMem(t *testing.T, hk *hooks, m *memHosts) {
	t.Helper()
	setVar(t, &hostsRead, m.read)
	hk.rosterDial = m.dial
	hk.rosterCheck = nil
}

func newMemHosts(t *testing.T) *memHosts {
	return &memHosts{t: t, entries: map[string]rundir.Entry{}, rows: map[string]string{}, down: map[string]bool{},
		hold: map[string]chan struct{}{}, conns: map[string][]net.Conn{}}
}

// memRow is host n's row: its session, and title.
func memRow(n int, title string) string {
	return `{"sessionId":"` + sessionOf(n) + `","activity":"idle","title":` + strconv.Quote(title) + `}`
}

// hostOf and sessionOf are host n's ids.
func hostOf(n int) string    { return fmt.Sprintf("%012x", n) }
func sessionOf(n int) string { return fmt.Sprintf("session-%d", n) }

// add lists host n with its row.
func (m *memHosts) add(n int, row string) string {
	id := hostOf(n)
	m.put(rundir.Entry{Protocol: 1, HostID: id, PID: 1000 + n, Socket: memSocket(id), CrazeSessionID: sessionOf(n),
		Provider: "cursor", Workspace: "/work/" + id, Ready: true, StartedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)}, row)
	return id
}

// put lists e — whatever it says — with row behind it.
func (m *memHosts) put(e rundir.Entry, row string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[e.HostID] = e
	m.rows[e.HostID] = row
}

func (m *memHosts) setRow(id, row string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[id] = row
}

// remove takes id out of the registry, as its sweep or its exit would.
func (m *memHosts) remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, id)
}

// fail makes id refuse dials, and drops its open connections.
func (m *memHosts) fail(id string) {
	m.mu.Lock()
	m.down[id] = true
	conns := m.conns[id]
	m.conns[id] = nil
	m.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// holdAnswers makes id's answers wait until the returned func is called.
func (m *memHosts) holdAnswers(id string) func() {
	ch := make(chan struct{})
	m.mu.Lock()
	m.hold[id] = ch
	m.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	m.t.Cleanup(release)
	return release
}

func (m *memHosts) read(rundir.Env) ([]rundir.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	out := make([]rundir.Entry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b rundir.Entry) int { return strings.Compare(a.HostID, b.HostID) })
	return out, nil
}

func (m *memHosts) readCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reads
}

func (m *memHosts) dialCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dials
}

func (m *memHosts) dial(_ context.Context, path string) (net.Conn, error) {
	id := strings.TrimPrefix(path, "mem:")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dials++
	if m.down[id] {
		return nil, syscall.ECONNREFUSED
	}
	a, b := net.Pipe()
	m.conns[id] = append(m.conns[id], b)
	go m.serve(id, b)
	return a, nil
}

// serve answers one connection as host id.
func (m *memHosts) serve(id string, c net.Conn) {
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
		m.mu.Lock()
		hold, row := m.hold[id], m.rows[id]
		m.mu.Unlock()
		if hold != nil {
			<-hold
		}
		result := `{"protocol":1,"endpoint":{"kind":"host","hostId":"` + id + `","crazeVersion":"0.0.0-mem","pid":1},` +
			`"clientId":"c-1","token":"t","resumed":false,"capabilities":{},"codecs":{"event":1,"snapshot":1},` +
			`"limits":{"inboundLine":4194304,"outboundLine":16777216},"retryHorizon":{"commands":1,"ageMs":1}}`
		if req.Method == protocol.MethodSessionsList {
			result = `{"epoch":"` + id + `","cursor":1,"sessions":[` + row + `]}`
		}
		if _, err := c.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":` + result + "}\n")); err != nil {
			return
		}
	}
}

// ---------------------------------------------------- a hub's client

// peer is a raw connection to a hub that may hold a subscription: replies
// and notifications are read as they come, each held to the schema.
type peer struct {
	t       *testing.T
	nc      net.Conn
	lr      *protocol.LineReader
	id      int
	methods map[string]string
}

// msg is one line the hub wrote, decoded.
type msg struct {
	raw    []byte
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *protocol.Error `json:"error"`
	Params json.RawMessage `json:"params"`
}

// dialPeer connects to the hub at sock and says hello.
func dialPeer(t *testing.T, sock string) *peer {
	t.Helper()
	nc, err := net.DialTimeout("unix", sock, step)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	p := &peer{t: t, nc: nc, lr: protocol.NewLineReader(nc, protocol.OutboundLineMax), methods: map[string]string{}}
	if m := p.call(protocol.MethodHello, protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{Kind: "test"}}); m.Error != nil {
		t.Fatalf("hello: %+v", m.Error)
	}
	return p
}

// send writes a request: its id.
func (p *peer) send(method string, params any) string {
	p.t.Helper()
	p.id++
	id := strconv.Itoa(p.id)
	req := map[string]any{"jsonrpc": "2.0", "id": p.id, "method": method}
	if params != nil {
		req["params"] = params
	}
	p.methods[id] = method
	_ = p.nc.SetWriteDeadline(time.Now().Add(step))
	if err := protocol.WriteLine(p.nc, req); err != nil {
		p.t.Fatalf("%s: write: %v", method, err)
	}
	return id
}

// read is the next line, within step.
func (p *peer) read() msg {
	p.t.Helper()
	m, ok := p.readWithin(step)
	if !ok {
		p.t.Fatalf("the hub wrote nothing within %v", step)
	}
	return m
}

// readWithin is the next line, or false when none came within d.
func (p *peer) readWithin(d time.Duration) (msg, bool) {
	p.t.Helper()
	_ = p.nc.SetReadDeadline(time.Now().Add(d))
	defer func() { _ = p.nc.SetReadDeadline(time.Time{}) }()
	line, err := p.lr.ReadLine()
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return msg{}, false
	}
	if err != nil {
		p.t.Fatalf("read: %v", err)
	}
	m := msg{raw: line}
	if err := json.Unmarshal(line, &m); err != nil {
		p.t.Fatalf("%q: %v", line, err)
	}
	method := m.Method
	if method == "" {
		method = p.methods[string(m.ID)]
	}
	if err := wiretest.Default().Server(method, line); err != nil {
		p.t.Fatalf("the hub's line is off the schema: %v (%s)", err, clip(line))
	}
	return m, true
}

// eof says the hub closed the connection: no line, the end, within step.
func (p *peer) eof() bool {
	p.t.Helper()
	_ = p.nc.SetReadDeadline(time.Now().Add(step))
	_, err := p.lr.ReadLine()
	return err != nil && !isTimeout(err)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// call sends a request and reads its reply, which must be the next line.
func (p *peer) call(method string, params any) msg {
	p.t.Helper()
	id := p.send(method, params)
	m := p.read()
	if string(m.ID) != id {
		p.t.Fatalf("%s: the next line is not its reply: %s", method, clip(m.raw))
	}
	return m
}

// list is the hub's sessions.list.
func (p *peer) list() protocol.HubSessionsListResult {
	p.t.Helper()
	m := p.call(protocol.MethodSessionsList, nil)
	if m.Error != nil {
		p.t.Fatalf("sessions.list: %+v", m.Error)
	}
	var res protocol.HubSessionsListResult
	if err := json.Unmarshal(m.Result, &res); err != nil {
		p.t.Fatal(err)
	}
	return res
}

// subscribe is the hub's sessions.subscribe, which must be answered.
func (p *peer) subscribe() protocol.SessionsSubscribeResult {
	p.t.Helper()
	m := p.call(protocol.MethodSessionsSubscribe, nil)
	if m.Error != nil {
		p.t.Fatalf("sessions.subscribe: %+v", m.Error)
	}
	var res protocol.SessionsSubscribeResult
	if err := json.Unmarshal(m.Result, &res); err != nil {
		p.t.Fatal(err)
	}
	return res
}

// roster is the next line, which must be a roster notification.
func (p *peer) roster() protocol.RosterParams {
	p.t.Helper()
	m := p.read()
	if m.Method != protocol.NotifyRoster {
		p.t.Fatalf("the next line is not a roster notification: %s", clip(m.raw))
	}
	var rp protocol.RosterParams
	if err := json.Unmarshal(m.Params, &rp); err != nil {
		p.t.Fatal(err)
	}
	return rp
}

// nothing checks the hub writes nothing for a short while.
func (p *peer) nothing(what string) {
	p.t.Helper()
	if m, ok := p.readWithin(150 * time.Millisecond); ok {
		p.t.Fatalf("%s: the hub wrote %s", what, clip(m.raw))
	}
}

// clip is a line short enough to print.
func clip(b []byte) string {
	if len(b) > 600 {
		return string(b[:600]) + "…"
	}
	return string(b)
}

// rowOf is the listed row of host id, nil when there is none.
func rowOf(rows []protocol.RosterRow, id string) *protocol.RosterRow {
	for i := range rows {
		if rows[i].HostID == id {
			return &rows[i]
		}
	}
	return nil
}

// ids is rows' host ids, in order.
func ids(rows []protocol.RosterRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.HostID
	}
	return out
}

// ----------------------------------------------- real hosts, and crashes

// hostTestChild runs this test binary as a session host for the roster's
// tests: "<hostId> <sessionId>" ("-" for none yet). It binds and lists the
// host in the process's registry (rundir.ProcessEnv) — with a fake host
// serving its socket when it has a session — says "bound" on stdout, and
// waits for its end: a SIGKILL, a crash, or its parent's going.
const hostTestChild = "CRAZE_HUB_TEST_HOST"

func init() {
	v, ok := os.LookupEnv(hostTestChild)
	if !ok {
		return
	}
	_ = os.Unsetenv(hostTestChild)
	parent := os.Getppid()
	if n, err := strconv.Atoi(os.Getenv(hubTestParent)); err == nil && n > 1 {
		parent = n
	}
	go childWatchdog(parent)
	id, session, _ := strings.Cut(v, " ")
	env := rundir.ProcessEnv()
	if session == "-" {
		if _, err := rundir.Bind(env, id, rundir.Entry{StartedAt: time.Now().UTC(), Workspace: "/work"}); err != nil {
			fmt.Fprintln(os.Stderr, "host test child:", err)
			os.Exit(97)
		}
	} else {
		h, err := fakehost.New(fakehost.Options{HostID: id, CrazeSessionID: session})
		if err == nil {
			var reg *rundir.Host
			if reg, err = h.Register(env); err == nil {
				go func() { _ = h.Serve(reg.Listener()) }()
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "host test child:", err)
			os.Exit(97)
		}
	}
	fmt.Println("bound")
	select {}
}

// hostChild is a session host of the test's own in env's registry, in a
// child process: session "" binds it with none. It is SIGKILLed — and its
// exit waited for — when the test ends, or by crash.
type hostChild struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func startHostChild(t *testing.T, env rundir.Env, id, session string) *hostChild {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if session == "" {
		session = "-"
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), "HOME="+env.Home, "CRAZE_HOME="+env.CrazeDir, "CRAZE_RUNTIME_DIR="+env.CrazeRuntimeDir,
		hostTestChild+"="+id+" "+session, hubTestParent+"="+strconv.Itoa(os.Getpid()))
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	hc := &hostChild{cmd: cmd, done: make(chan struct{})}
	bound := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		bound <- line
		_, _ = io.Copy(io.Discard, out)
	}()
	go func() { _ = cmd.Wait(); close(hc.done) }()
	t.Cleanup(func() { hc.crash(t) })
	select {
	case line := <-bound:
		if strings.TrimSpace(line) != "bound" {
			t.Fatalf("host child %s said %q, not bound", id, line)
		}
	case <-time.After(step):
		t.Fatalf("host child %s did not bind within %v", id, step)
	}
	return hc
}

// crash SIGKILLs the host and waits for its exit: its lock is free, its
// registry entry left behind.
func (hc *hostChild) crash(t *testing.T) {
	t.Helper()
	_ = hc.cmd.Process.Kill()
	select {
	case <-hc.done:
	case <-time.After(step):
		t.Fatalf("host child %d did not exit within %v of SIGKILL", hc.cmd.Process.Pid, step)
	}
}

// hostIn serves an in-process fake host listed in env's registry as host n;
// close unlists it.
func hostIn(t *testing.T, env rundir.Env, n int) (*fakehost.Host, func()) {
	t.Helper()
	h, err := fakehost.New(fakehost.Options{HostID: hostOf(n), CrazeSessionID: sessionOf(n), Workspace: "/work/" + hostOf(n)})
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
	closeIt := func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), step)
			defer cancel()
			_ = h.Close(ctx)
			<-served
			_ = reg.Close()
		})
	}
	t.Cleanup(closeIt)
	return h, closeIt
}
