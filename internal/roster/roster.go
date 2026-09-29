package roster

import (
	"cmp"
	"context"
	"errors"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// Snapshot is the session list's data at one moment (plan 030 §3.9): every
// running session the registry lists and what each host last said, and the
// saved sessions of the index that are not running. The roster never writes
// a Snapshot, or anything it points at, once it has handed it out — a later
// one may share a row's pointers — and a receiver writes through none of
// them.
type Snapshot struct {
	// Running is every host the registry lists, in host-id order: this
	// user's, whatever its CRAZE_HOME (the registry is per HOME).
	Running []Row
	// Saved is this CRAZE_HOME's index, newest first, at most savedMax rows:
	// one row per craze id (its newest), a row with none by its own provider
	// session id, of a provider craze can resume (the resume picker's rule),
	// and none a running host serves — a host that does not answer included:
	// unreachable is never shown as saved.
	Saved []sessions.Row
	// RegistryErr is why the registry could not be read at the last tick,
	// nil when it was; Running is then what the last good read listed.
	// IndexErr is the same for the index and Saved.
	RegistryErr error
	IndexErr    error
}

// Status is whether a running session's host answers.
type Status int

const (
	// Connecting: no attempt of the roster's has finished yet — or the host
	// has not published its session yet (its registry entry names none), so
	// there is nothing to ask it for.
	Connecting Status = iota
	// Reachable: its last attempt was answered.
	Reachable
	// Unreachable: its last attempt failed — the registry lists it, and its
	// socket does not answer, or answers no — and the next waits out the
	// backoff (1 s, doubling, at most 30 s).
	Unreachable
)

func (s Status) String() string {
	switch s {
	case Connecting:
		return "connecting"
	case Reachable:
		return "reachable"
	case Unreachable:
		return "unreachable"
	}
	return "status(?)"
}

// Row is one running session: its host as the registry names it, whether the
// host answers, and what it last said.
type Row struct {
	Host   Host
	Status Status
	// Version is the craze the host says it is (hello's
	// endpoint.crazeVersion), "" until a hello was answered: what the row of
	// an older host shows.
	Version string
	// Session is the host's last sessions.list row, nil until the first
	// answer, and kept as it was while the host does not answer.
	Session *Session
	// IndexTitle is the index's title for the session — its newest row with
	// the host's craze id — "" when there is none: what a row shows before
	// its host has answered.
	IndexTitle string
}

// Ref is this row's session as Open and Stop name it (tui.Sessions).
func (r Row) Ref() Ref { return Ref{Host: r.Host} }

// Host is a running session's host as its registry entry names it
// (rundir.Entry).
type Host struct {
	ID        string
	PID       int
	Socket    string
	StartedAt time.Time
	// CrazeSessionID, ProviderSessionID and Incarnation are the session the
	// host published last: "" before its engine is up.
	CrazeSessionID    string
	ProviderSessionID string
	Incarnation       string
	Provider          string
	Workspace         string
	Ready             bool
}

// HostOf is e's host.
func HostOf(e rundir.Entry) Host {
	return Host{ID: e.HostID, PID: e.PID, Socket: e.Socket, StartedAt: e.StartedAt,
		CrazeSessionID: e.CrazeSessionID, ProviderSessionID: e.ProviderSessionID, Incarnation: e.Incarnation,
		Provider: e.Provider, Workspace: e.Workspace, Ready: e.Ready}
}

// Entry is h as a registry entry: what a dial and a stop name a host by.
func (h Host) Entry() rundir.Entry {
	return rundir.Entry{Protocol: protocol.ProtocolVersion, HostID: h.ID, PID: h.PID, Socket: h.Socket,
		StartedAt: h.StartedAt, CrazeSessionID: h.CrazeSessionID, ProviderSessionID: h.ProviderSessionID,
		Incarnation: h.Incarnation, Provider: h.Provider, Workspace: h.Workspace, Ready: h.Ready}
}

// Ref names one session of the list for tui.Sessions' Open and Stop: a
// running session by its host, or a saved one by its index row.
type Ref struct {
	// Host is a running session's host; zero for a saved one.
	Host Host
	// Saved is a saved session's index row; nil for a running one.
	Saved *sessions.Row
}

// SavedRef is row's session as Open names it.
func SavedRef(row sessions.Row) Ref { return Ref{Saved: &row} }

// Session is what a host's sessions.list row said (protocol.SessionRow), in
// the engine's words.
type Session struct {
	// ID is the durable craze session id; Incarnation and
	// ProviderSessionID are as the host names them now.
	ID                string
	Incarnation       string
	ProviderSessionID string
	Provider          string
	ProviderLabel     string
	Workspace         string
	Title             string
	Activity          engine.Activity
	// ForeignTurn: the agent is running a turn of its own.
	ForeignTurn bool
	PendingAsks int
	// HeadAsk is the first open ask, nil when none is.
	HeadAsk *HeadAsk
	// LastTurn is how the last turn ended, nil while one runs, before any
	// has ended, and from an older host.
	LastTurn *engine.LastTurn
	// StartedAt is when the host started serving the session, zero from an
	// older host; PermissionMode how it spawned its agent.
	StartedAt      time.Time
	PermissionMode backend.PermissionMode
	// Stop says the host serves session.stop (its capability stop).
	Stop bool
	// RowFacts says the host's row carries the row facts below (its
	// capability rowFacts, plan 030 §3.8). False — an older host — they are
	// all zero and mean nothing.
	RowFacts    bool
	Doing       string
	LastReply   string
	Since       time.Time
	StartFailed bool
	StartErr    string
	Prompted    bool
}

// HeadAsk is the first open ask: its id, kind, the label its card draws, and
// — a row fact — what it is about.
type HeadAsk struct {
	ID, Kind, Label, Summary string
}

// sessionOf is a host's sessions.list row as a Session.
func sessionOf(r protocol.SessionRow) *Session {
	s := &Session{
		ID: r.SessionID, Incarnation: r.Incarnation, ProviderSessionID: r.ProviderSessionID,
		Provider: r.Provider.Name, ProviderLabel: r.Provider.Label, Workspace: r.Workspace,
		Title: r.Title, Activity: engine.Activity(r.Activity), ForeignTurn: r.ForeignTurn, PendingAsks: r.PendingAsks,
		StartedAt: r.StartedAt, PermissionMode: permissionMode(r.PermissionMode),
		Stop: r.Capabilities.Stop, RowFacts: r.Capabilities.RowFacts,
		Doing: r.Doing, LastReply: r.LastReply, Since: r.Since,
		StartFailed: r.StartFailed, StartErr: r.StartErr, Prompted: r.Prompted,
	}
	if a := r.HeadAsk; a != nil {
		s.HeadAsk = &HeadAsk{ID: a.ID, Kind: a.Kind, Label: a.Label, Summary: a.Summary}
	}
	if lt := r.LastTurn; lt != nil {
		s.LastTurn = &engine.LastTurn{Outcome: engine.TurnOutcome(lt.Outcome), Err: lt.Err, EndedAt: lt.EndedAt, TurnID: lt.TurnID}
	}
	return s
}

// permissionMode is the wire's permission mode in the Backend's words: a
// mode this build does not know, or none, is the host not saying.
func permissionMode(m protocol.PermissionMode) backend.PermissionMode {
	switch m {
	case protocol.PermissionBypass:
		return backend.PermissionBypass
	case protocol.PermissionPrompt:
		return backend.PermissionPrompt
	}
	return backend.PermissionUnsaid
}

// equal says two answers say the same thing: a poll that changes nothing
// publishes nothing.
func (s *Session) equal(o *Session) bool {
	switch {
	case s == nil || o == nil:
		return s == o
	case (s.HeadAsk == nil) != (o.HeadAsk == nil), s.HeadAsk != nil && *s.HeadAsk != *o.HeadAsk:
		return false
	case (s.LastTurn == nil) != (o.LastTurn == nil), s.LastTurn != nil && *s.LastTurn != *o.LastTurn:
		return false
	}
	a, b := *s, *o
	a.HeadAsk, a.LastTurn, b.HeadAsk, b.LastTurn = nil, nil, nil, nil
	return a == b
}

// The poll's rules (plan 030 §3.9, R2-8).
const (
	// tickEvery is how often the registry is read and each host asked.
	tickEvery = time.Second
	// dialBudget bounds a new connection's dial and hello together, and
	// listBudget its sessions.list: one attempt's budget.
	dialBudget = 500 * time.Millisecond
	listBudget = 500 * time.Millisecond
	// maxInFlight is how many attempts run at once, whatever the number of
	// hosts; a host is never in two.
	maxInFlight = 8
	// backoffMin and backoffMax bound a failing host's wait before its next
	// attempt: backoffMin after one failure, doubling, at most backoffMax.
	backoffMin = time.Second
	backoffMax = 30 * time.Second
	// savedMax is how many saved rows a Snapshot carries.
	savedMax = 50
)

// options are the poll's rules and its seams: production's are defaults(),
// and a test changes what it needs.
type options struct {
	tick                   time.Duration
	dialBudget, listBudget time.Duration
	maxInFlight            int
	backoffMin, backoffMax time.Duration
	savedMax               int

	// ticks, when set, stands in for the ticker: a tick is a send on it.
	ticks <-chan time.Time
	// now is the clock the backoff is kept on.
	now func() time.Time
	// hosts lists the registry (rundir.Hosts over the roster's env).
	hosts func() ([]rundir.Entry, error)
	// dial opens a host's socket, and check vets its peer before a byte is
	// written (rundir.DialCheck).
	dial  dialFunc
	check func(*net.UnixConn) error
	// indexPath is the index file whose modification says it changed.
	indexPath func() string
	// The tests' barriers, nil in production: attempting runs on an
	// attempt's own goroutine as it begins, with its host's id; ticked on the
	// poller once a tick has started what was due; applied on the poller once
	// it has taken an attempt's result.
	attempting func(hostID string)
	ticked     func()
	applied    func(hostID string, err error)
}

func defaults(env rundir.Env) options {
	return options{
		tick: tickEvery, dialBudget: dialBudget, listBudget: listBudget, maxInFlight: maxInFlight,
		backoffMin: backoffMin, backoffMax: backoffMax, savedMax: savedMax,
		now:       time.Now,
		hosts:     func() ([]rundir.Entry, error) { return rundir.Hosts(env) },
		dial:      dialUnix,
		check:     rundir.DialCheck(os.Geteuid()),
		indexPath: paths.SessionsPath,
	}
}

// Roster is the session list's poller (plan 030 §3.9): opened when the list
// opens, closed when it closes. Each tick it reads the registry and asks
// every host it lists for its row over a connection it keeps, and it hands
// the list its latest Snapshot through Updates.
type Roster struct {
	o     options
	index Index
	ctx   context.Context
	stop  context.CancelFunc
	out   chan Snapshot
	done  chan struct{}
	once  sync.Once
}

// Index is the session index the saved rows come from: *sessions.Store.
type Index interface {
	// All is every row, newest first (sessions.Store.All).
	All() ([]sessions.Row, error)
}

// Open starts a roster over env's registry — running hosts are this user's,
// per HOME — and index, this CRAZE_HOME's session index (nil: no saved rows),
// whose file is paths.SessionsPath's. The first tick is at once.
func Open(env rundir.Env, index Index) *Roster {
	return open(index, defaults(env))
}

func open(index Index, o options) *Roster {
	ctx, stop := context.WithCancel(context.Background())
	r := &Roster{o: o, index: index, ctx: ctx, stop: stop, out: make(chan Snapshot, 1), done: make(chan struct{})}
	p := &poller{r: r, o: o, hosts: map[string]*hostState{}, results: make(chan result, o.maxInFlight)}
	go p.run()
	return r
}

// Updates is the latest-snapshot slot: capacity one, the newest Snapshot
// replacing one not yet taken, and never waited on by the roster — a list
// that reads slowly reads the latest. It is closed once the roster has
// stopped (Close).
func (r *Roster) Updates() <-chan Snapshot { return r.out }

// Close stops the roster: every attempt in flight is cancelled and joined,
// every connection closed, and Updates closed. It returns once every
// goroutine the roster started has returned, and is idempotent.
func (r *Roster) Close() {
	r.once.Do(r.stop)
	<-r.done
}

// hostState is one registry host as the poller keeps it. Only the poller's
// goroutine touches it.
type hostState struct {
	entry rundir.Entry
	// conn is the kept connection, nil while there is none or while an
	// attempt holds it: an attempt owns the connection it was handed until
	// its result comes back.
	conn *client
	// busy says an attempt is in flight: none other starts for this host
	// until its result is back. gone says the registry no longer lists the
	// host while it is: its result closes the connection and forgets it.
	busy bool
	gone bool
	// failures counts the attempts failed in a row, and next is when the
	// next may start (the backoff). round is the tick its last attempt
	// started in: a host is asked once a tick, and the least recently asked
	// go first when the attempts are capped.
	failures int
	next     time.Time
	round    uint64
	status   Status
	version  string
	session  *Session
}

// result is one attempt's end: the connection to keep (nil on a failure,
// which has closed it), the host's row and hello's version, or why not.
type result struct {
	hostID  string
	conn    *client
	row     protocol.SessionRow
	version string
	err     error
}

// poller is the roster's one goroutine: it owns every hostState, starts the
// attempts and takes their results, and publishes.
type poller struct {
	r        *Roster
	o        options
	hosts    map[string]*hostState
	results  chan result
	inFlight int
	// round counts the ticks.
	round uint64

	regErr error
	saved  savedRows
	dirty  bool
}

func (p *poller) run() {
	defer close(p.r.done)
	defer close(p.r.out)
	ticks := p.o.ticks
	if ticks == nil {
		t := time.NewTicker(p.o.tick)
		defer t.Stop()
		ticks = t.C
	}
	p.tick()
	for {
		select {
		case <-p.r.ctx.Done():
			p.shutdown()
			return
		case <-ticks:
			p.tick()
		case res := <-p.results:
			p.apply(res)
			// Every other result already back goes into the same Snapshot.
			for more := true; more; {
				select {
				case res := <-p.results:
					p.apply(res)
				default:
					more = false
				}
			}
			// The attempts that ended leave their places to the hosts this
			// tick has not asked yet.
			p.launch()
			p.publish()
		}
	}
}

// tick reads the registry and the index, starts the attempts that are due
// and publishes what changed.
func (p *poller) tick() {
	p.round++
	entries, err := p.o.hosts()
	if err != nil {
		if p.regErr == nil || p.regErr.Error() != err.Error() {
			p.dirty = true
		}
		p.regErr = err
	} else {
		if p.regErr != nil {
			p.dirty = true
		}
		p.regErr = nil
		p.reconcile(entries)
	}
	if p.saved.read(p.r.index, p.o, p.hosts) {
		p.dirty = true
	}
	p.launch()
	p.publish()
	if h := p.o.ticked; h != nil {
		h()
	}
}

// reconcile brings the hosts to the registry's list: a new host is
// Connecting, a changed entry is taken, and a host gone from the registry has
// its connection closed — or, while an attempt holds it, closed by that
// attempt's result.
func (p *poller) reconcile(entries []rundir.Entry) {
	listed := make(map[string]bool, len(entries))
	for _, e := range entries {
		listed[e.HostID] = true
		h := p.hosts[e.HostID]
		switch {
		case h == nil:
			p.hosts[e.HostID] = &hostState{entry: e}
			p.dirty = true
		case h.gone || h.entry != e:
			h.gone, h.entry = false, e
			p.dirty = true
		}
	}
	for id, h := range p.hosts {
		if listed[id] || h.gone {
			continue
		}
		p.dirty = true
		if h.busy {
			h.gone = true
			continue
		}
		h.conn.close()
		delete(p.hosts, id)
	}
}

// launch starts the attempts that are due, least recently asked first, up to
// maxInFlight in flight: a host is due when it is not in an attempt, has
// published its session, has not been asked this tick and is past its
// backoff. It runs at each tick, and again as attempts end, so a tick asks
// every host however many there are, eight at a time.
func (p *poller) launch() {
	if p.inFlight >= p.o.maxInFlight {
		return
	}
	now := p.o.now()
	var due []*hostState
	for _, h := range p.hosts {
		if !h.busy && !h.gone && h.entry.CrazeSessionID != "" && h.round < p.round && !now.Before(h.next) {
			due = append(due, h)
		}
	}
	slices.SortFunc(due, func(a, b *hostState) int {
		if a.round != b.round {
			return cmp.Compare(a.round, b.round)
		}
		return strings.Compare(a.entry.HostID, b.entry.HostID)
	})
	for _, h := range due {
		if p.inFlight >= p.o.maxInFlight {
			return
		}
		p.start(h)
	}
}

// start hands h's kept connection, if any, to an attempt of its own.
func (p *poller) start(h *hostState) {
	h.busy, h.round = true, p.round
	p.inFlight++
	conn, e := h.conn, h.entry
	h.conn = nil
	go func() { p.results <- p.attempt(e, conn) }()
}

// attempt is one host's poll within its budget: sessions.list on the kept
// connection, or — with none, or one the host had closed meanwhile — a new
// connection's dial and hello first. A connection that fails is closed; one
// that answered is the result's, to keep.
func (p *poller) attempt(e rundir.Entry, c *client) result {
	if h := p.o.attempting; h != nil {
		h(e.HostID)
	}
	res := result{hostID: e.HostID}
	ctx := p.r.ctx
	if c != nil {
		rows, err := c.list(ctx, p.o.listBudget)
		if err == nil {
			return answered(res, c, rows, e)
		}
		c.close()
		if !stale(err) {
			res.err = err
			return res
		}
	}
	c, err := dialHello(ctx, p.o.dial, p.o.check, e.Socket, p.o.dialBudget)
	if err != nil {
		res.err = err
		return res
	}
	rows, err := c.list(ctx, p.o.listBudget)
	if err != nil {
		c.close()
		res.err = err
		return res
	}
	return answered(res, c, rows, e)
}

// answered is res for rows, c's answer: c kept with the row of the session e
// names — a host serves one — or, from a host that answered with none, closed
// and a failure.
func answered(res result, c *client, rows protocol.SessionsListResult, e rundir.Entry) result {
	if len(rows.Sessions) == 0 {
		c.close()
		res.err = errNoRow
		return res
	}
	res.conn, res.row, res.version = c, pick(rows, e), c.version
	return res
}

// errNoRow is a host that answered sessions.list with no session.
var errNoRow = errors.New("roster: the host answered sessions.list with no session")

// pick is the row of the session e names, else the first: rows has one.
func pick(rows protocol.SessionsListResult, e rundir.Entry) protocol.SessionRow {
	for _, r := range rows.Sessions {
		if r.SessionID == e.CrazeSessionID {
			return r
		}
	}
	return rows.Sessions[0]
}

// apply takes one attempt's result: an answer keeps its connection and
// resets the backoff; a failure marks the host unreachable and doubles it.
func (p *poller) apply(res result) {
	if h := p.o.applied; h != nil {
		defer h(res.hostID, res.err)
	}
	p.inFlight--
	h := p.hosts[res.hostID]
	if h == nil || h.gone {
		res.conn.close()
		if h != nil {
			delete(p.hosts, res.hostID)
		}
		return
	}
	h.busy = false
	if res.err != nil {
		h.failures++
		h.next = p.o.now().Add(p.backoff(h.failures))
		if h.status != Unreachable {
			h.status = Unreachable
			p.dirty = true
		}
		return
	}
	h.failures, h.next = 0, time.Time{}
	h.conn = res.conn
	if res.version != "" && res.version != h.version {
		h.version = res.version
		p.dirty = true
	}
	s := sessionOf(res.row)
	if h.status != Reachable || !h.session.equal(s) {
		h.status, h.session = Reachable, s
		p.dirty = true
	}
}

// backoff is the wait after n failures in a row: backoffMin, doubling, at
// most backoffMax.
func (p *poller) backoff(n int) time.Duration {
	d := p.o.backoffMin
	for i := 1; i < n && d < p.o.backoffMax; i++ {
		d *= 2
	}
	return min(d, p.o.backoffMax)
}

// publish puts the latest Snapshot in the slot when something changed since
// the last: the one waiting untaken, if any, is replaced. The poller is the
// slot's only sender, so once it has emptied it the send cannot wait.
func (p *poller) publish() {
	if !p.dirty {
		return
	}
	p.dirty = false
	snap := p.snapshot()
	select {
	case <-p.r.out:
	default:
	}
	p.r.out <- snap
}

// snapshot is the Snapshot of now: the hosts in host-id order, each row a
// copy.
func (p *poller) snapshot() Snapshot {
	s := Snapshot{RegistryErr: p.regErr, IndexErr: p.saved.err, Saved: slices.Clone(p.saved.rows)}
	for _, h := range p.hosts {
		if h.gone {
			continue
		}
		row := Row{Host: HostOf(h.entry), Status: h.status, Version: h.version, IndexTitle: p.saved.titles[h.entry.CrazeSessionID]}
		if h.session != nil {
			c := *h.session
			row.Session = &c
		}
		s.Running = append(s.Running, row)
	}
	slices.SortFunc(s.Running, func(a, b Row) int { return strings.Compare(a.Host.ID, b.Host.ID) })
	return s
}

// shutdown ends the poll: the roster's context has ended, which closes the
// connection of every attempt in flight, so each comes back at once; their
// connections and every kept one are closed.
func (p *poller) shutdown() {
	for p.inFlight > 0 {
		res := <-p.results
		p.inFlight--
		res.conn.close()
	}
	for id, h := range p.hosts {
		h.conn.close()
		delete(p.hosts, id)
	}
}
