package roster

import (
	"cmp"
	"context"
	"encoding/json"
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
	// nil when it was; Running is then what the last good read listed —
	// nothing, before any. A Snapshot is published only after a read, so nil
	// says a read succeeded (what the hub's list roster waits for before it
	// hands a new poller's Snapshots on, plan 032 r34 2). IndexErr is the
	// same for the index and Saved.
	RegistryErr error
	IndexErr    error
	// Run is the poll's run the Snapshot is from: a roster OpenHub opened
	// polls only while a run is open (Resume), and each run is its own
	// number; the list's roster (Open) is one run, 1, from Open to Close.
	Run uint64
}

// Status is whether a running session's host answers.
type Status int

const (
	// Connecting: no attempt of the roster's has finished yet — none since
	// the host's registry entry last named another session included — or the
	// host has not published its session yet (its registry entry names
	// none), so there is nothing to ask it for.
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
	// answer of the session its entry names, and kept as it was while the
	// host does not answer.
	Session *Session
	// IndexTitle is the index's title for the session — its newest row with
	// the host's craze id — "" when there is none: what a row shows before
	// its host has answered.
	IndexTitle string

	// Raw is the host's last sessions.list row as the JSON value it sent,
	// compacted — every member kept, those this build does not know
	// included, at any depth — beside Session, its decoding; nil until the
	// first answer of the session its entry names, kept as it was while the
	// host does not answer, and nil for a row that is not an object. Only a
	// roster OpenHub opened keeps it (plan 032 §3.6, P4).
	Raw json.RawMessage
	// ReadAt is when the host's row was last read — its last answered
	// attempt, by the roster's clock — zero before the first of the session
	// its entry names.
	ReadAt time.Time
	// Polled says the row is this run's (Snapshot.Run): the host's last
	// attempt to finish started in it, or the host is waiting out the
	// backoff of a failed one — its Unreachable stands until then. A host
	// whose attempt has not come back yet in the run, a new one, and one
	// with no session to ask for are not.
	Polled bool
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
	// Protocol is the control protocol the registry entry says the host
	// speaks.
	Protocol int
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
	return Host{ID: e.HostID, PID: e.PID, Socket: e.Socket, StartedAt: e.StartedAt, Protocol: e.Protocol,
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
	// listBudget its sessions.list: one attempt's budget is the two
	// together, from its start — each share bounded by its own too, and a
	// kept connection's attempt, which dials nothing, spending listBudget of
	// it. A kept connection found closed spends the rest on its one redial
	// (attempt), never a fresh budget.
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

// TickEvery is how often a round runs: the registry read, and every host
// asked once. A host gone from the registry leaves the roster within one
// (the hub's A9 bound is one tick and one flush).
const TickEvery = tickEvery

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
	// client is who the roster's hello says it is.
	client protocol.ClientInfo

	// hub is the hub's roster (OpenHub; plan 032 §3.6): each host's row kept
	// as the JSON value it sent (Row.Raw), no index, every Snapshot handed to
	// publish — in order, on the poller's goroutine — in place of the slot,
	// one published whenever a host's read or run state moves too
	// (Row.ReadAt, Row.Polled), and a poll only while a run is open (Resume,
	// Pause), which starts closed.
	hub     bool
	publish func(Snapshot)
	// The tests' barriers, nil in production: attempting runs on the poller
	// as an attempt is started, with its host's id — after its budget's clock
	// has started, and before its goroutine exists, so a tick's barrier
	// (ticked) has counted every attempt the tick started; attempted runs on
	// the attempt's own goroutine after it has sent its result, the last thing
	// the goroutine does before Close's join counts it done; ticked on the
	// poller once a tick has started what was due; applied on the poller once
	// it has taken an attempt's result.
	attempting func(hostID string)
	attempted  func(hostID string)
	ticked     func()
	applied    func(hostID string, err error)
	// paused runs on the poller once a Pause has taken effect (the hub's
	// roster's tests).
	paused func()
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
		client:    clientInfo,
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

	// want is the run Resume asked for, 0 after Pause (wantMu), and wake
	// tells the poller it moved: a latest-value slot, so neither ever waits.
	wantMu sync.Mutex
	want   uint64
	wake   chan struct{}
}

// Index is the session index the saved rows come from: *sessions.Store.
type Index interface {
	// All is every row, newest first (sessions.Store.All).
	All() ([]sessions.Row, error)
}

// Open starts a roster over env's registry — running hosts are this user's,
// per HOME — and index, this CRAZE_HOME's session index (nil: no saved rows),
// whose file is paths.SessionsPath's. The first tick is at once, and its
// Snapshot is published whatever it holds — an empty one too.
func Open(env rundir.Env, index Index) *Roster {
	return open(index, defaults(env))
}

// HubOptions are the hub's roster's (OpenHub). Hosts and Publish are
// required; every other field's zero value is production's.
type HubOptions struct {
	// Hosts reads the registry: the hub's read, which its idle rule also
	// takes (plan 032 §3.5) — rundir.Hosts at its heart, which sweeps the
	// dead, so a crashed host leaves at the next round.
	Hosts func() ([]rundir.Entry, error)
	// Publish is handed every Snapshot, in order, on the poller's goroutine:
	// it must not block, and must not call back into the roster but for
	// Resume and Pause. No slot is filled (Updates only closes, at Close).
	Publish func(Snapshot)
	// Client is who the roster's hello says it is.
	Client protocol.ClientInfo

	// A test's seams, zero in production: Ticks stands in for the ticker;
	// Now is the clock of the backoff and of Row.ReadAt; Dial opens a host's
	// socket, and Check — only with Dial set, nil for none — vets its peer
	// (production's is rundir.DialCheck); Budget replaces both shares of an
	// attempt's budget (dialBudget, listBudget); Attempting is told each
	// attempt as the poller starts it, on its goroutine, before the
	// Snapshot that follows is published; Paused once a Pause has taken
	// effect, on its goroutine.
	Ticks      <-chan time.Time
	Now        func() time.Time
	Dial       func(ctx context.Context, path string) (net.Conn, error)
	Check      func(*net.UnixConn) error
	Budget     time.Duration
	Attempting func(hostID string)
	Paused     func()
}

// OpenHub starts the hub's roster (plan 032 §3.6, P1): the list's poller —
// the same tick, budgets, cap and backoff, and the same kept connections —
// with no index, each host's row kept as the JSON value it sent (Row.Raw),
// and its Snapshots handed to o.Publish. It polls only while a run is open:
// none is at first; Resume opens one, which reads the registry and asks every
// host at once and then at every tick, and Pause closes it, keeping every
// connection and every host's state for the next. Close stops it as Open's.
func OpenHub(o HubOptions) *Roster {
	opts := defaults(rundir.Env{})
	opts.hosts, opts.publish, opts.hub = o.Hosts, o.Publish, true
	opts.indexPath = func() string { return "" }
	if o.Client.Kind != "" {
		opts.client = o.Client
	}
	if o.Ticks != nil {
		opts.ticks = o.Ticks
	}
	if o.Now != nil {
		opts.now = o.Now
	}
	if o.Dial != nil {
		opts.dial, opts.check = o.Dial, o.Check
	}
	if o.Budget > 0 {
		opts.dialBudget, opts.listBudget = o.Budget, o.Budget
	}
	opts.attempting, opts.paused = o.Attempting, o.Paused
	return open(nil, opts)
}

func open(index Index, o options) *Roster {
	ctx, stop := context.WithCancel(context.Background())
	r := &Roster{o: o, index: index, ctx: ctx, stop: stop, out: make(chan Snapshot, 1), done: make(chan struct{}),
		wake: make(chan struct{}, 1)}
	// Dirty from the start: the first tick publishes what it found, however
	// little. An empty registry and an empty index change nothing, and a list
	// that waited for a change to draw would draw nothing — not even that
	// nothing runs — until one came (C12r2, r27-pr2 2).
	p := &poller{r: r, o: o, hosts: map[string]*hostState{}, results: make(chan result, o.maxInFlight), dirty: true}
	if !o.hub {
		// The list's roster is one run, open from the start.
		p.run, p.active = 1, true
	}
	go p.loop()
	return r
}

// Resume opens run — a number above every run before it — on a roster
// OpenHub opened: at once the poller reads the registry and asks every host,
// as a tick does, and it goes on at every tick; Snapshot.Run says which run a
// Snapshot is from, and Row.Polled whether a row is that run's. It never
// waits: the poller takes the latest of Resume and Pause.
func (r *Roster) Resume(run uint64) { r.ask(run) }

// Pause closes the run: the poller asks no host and reads no registry until
// the next Resume. Attempts in flight finish and are taken, and every kept
// connection is kept. It never waits.
func (r *Roster) Pause() { r.ask(0) }

func (r *Roster) ask(run uint64) {
	r.wantMu.Lock()
	r.want = run
	r.wantMu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Roster) wanted() uint64 {
	r.wantMu.Lock()
	defer r.wantMu.Unlock()
	return r.want
}

// Updates is the latest-snapshot slot: capacity one, the newest Snapshot
// replacing one not yet taken, and never waited on by the roster — a list
// that reads slowly reads the latest. It is closed once the roster has
// stopped (Close).
func (r *Roster) Updates() <-chan Snapshot { return r.out }

// Close stops the roster: every attempt in flight is cancelled and joined,
// every connection closed, and Updates closed. It returns once every
// goroutine the roster started has finished — each attempt's goroutine
// joined as a goroutine, not only its result taken — and is idempotent.
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
	// raw is the hub's roster's (options.hub): the last row's JSON value.
	// readAt is when the last answer came (the roster's clock).
	raw    json.RawMessage
	readAt time.Time
	// startRun is the run the host's last attempt started in, and doneRun
	// the run of the last attempt to finish: Row.Polled's.
	startRun, doneRun uint64
}

// forget drops what h said of its last session — its row, its read, its
// status and backoff, and whether it was polled — for a host whose entry now
// names another: it is Connecting again, due at once. Its connection and its
// version, the host process's, are kept.
func (h *hostState) forget() {
	h.status, h.session, h.raw, h.readAt = Connecting, nil, nil, time.Time{}
	h.failures, h.next, h.doneRun = 0, time.Time{}, 0
}

// result is one attempt's end: the connection to keep (nil on a failure,
// which has closed it), the host's row and hello's version, or why not; and
// the craze session the attempt asked for (its entry's).
type result struct {
	hostID  string
	session string
	conn    *client
	row     protocol.SessionRow
	raw     json.RawMessage
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
	// attempts counts the attempts' goroutines until each has finished,
	// which shutdown joins: a result taken says the attempt is over, not
	// that its goroutine is (sol r17-c9 3).
	attempts sync.WaitGroup
	// round counts the ticks.
	round uint64
	// run is the open run's number (the last opened, while paused), and
	// active whether one is open: the poller ticks only while one is.
	run    uint64
	active bool

	regErr error
	saved  savedRows
	dirty  bool
}

func (p *poller) loop() {
	defer close(p.r.done)
	defer close(p.r.out)
	ticks := p.o.ticks
	if ticks == nil {
		t := time.NewTicker(p.o.tick)
		defer t.Stop()
		ticks = t.C
	}
	if p.active {
		p.tick()
	}
	for {
		select {
		case <-p.r.ctx.Done():
			p.shutdown()
			return
		case <-p.r.wake:
			p.control()
		case <-ticks:
			if p.active {
				p.tick()
			}
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
			// tick has not asked yet — while a run is open: a paused poll
			// starts nothing, the rest of its round included.
			if p.active {
				p.launch()
			}
			p.publish()
		}
	}
}

// control takes the latest of Resume and Pause: a run asked for that is not
// the open one opens — dirty, so its first Snapshot says so whatever else
// moved — and ticks at once; 0 closes the open one.
func (p *poller) control() {
	switch run := p.r.wanted(); {
	case run == 0:
		p.active = false
		if f := p.o.paused; f != nil {
			f()
		}
	case run != p.run || !p.active:
		if run != p.run {
			p.run = run
			p.dirty = true
		}
		p.active = true
		p.tick()
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
	// The hub's roster has no index: no saved rows, and no running set to
	// build for them.
	if p.r.index != nil && p.saved.read(p.r.index, p.o.indexPath, p.o.savedMax, runningOfHosts(p.hosts)) {
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
// attempt's result. An entry that names another craze session than before is
// a host whose row is not yet read: what it said of the last session is
// dropped, and it is Connecting until an attempt made for the new one comes
// back (r19 2).
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
			if h.entry.CrazeSessionID != e.CrazeSessionID {
				h.forget()
			}
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

// start hands h's kept connection, if any, to an attempt of its own, on a
// goroutine shutdown joins. The attempt's budget starts here: its one
// deadline, dialBudget and listBudget from now, is fixed before the goroutine
// exists and carried through everything the attempt does.
func (p *poller) start(h *hostState) {
	h.busy, h.round, h.startRun = true, p.round, p.run
	p.inFlight++
	conn, e := h.conn, h.entry
	h.conn = nil
	deadline := time.Now().Add(p.o.dialBudget + p.o.listBudget)
	if f := p.o.attempting; f != nil {
		f(e.HostID)
	}
	p.attempts.Add(1)
	go func() {
		defer p.attempts.Done()
		if f := p.o.attempted; f != nil {
			defer f(e.HostID)
		}
		p.results <- p.attempt(e, conn, deadline)
	}()
}

// attempt is one host's poll within its budget, which ends at deadline:
// sessions.list on the kept connection, or — with none, or one the host had
// closed meanwhile — a new connection's dial and hello first. Each share is
// bounded by its own budget and by deadline both (share), so a kept
// connection the host closed late in its list leaves its redial only what is
// left of the one budget (sol r17-c9 2): an attempt never outlasts
// dialBudget + listBudget, however its host behaves. A connection that fails
// is closed; one that answered is the result's, to keep.
func (p *poller) attempt(e rundir.Entry, c *client, deadline time.Time) result {
	res := result{hostID: e.HostID, session: e.CrazeSessionID}
	ctx := p.r.ctx
	if c != nil {
		rows, raws, err := c.list(ctx, share(p.o.listBudget, deadline), p.o.hub)
		if err == nil {
			return answered(res, c, rows, raws, e)
		}
		c.close()
		if !stale(err) {
			res.err = err
			return res
		}
	}
	c, err := dialHello(ctx, p.o.dial, p.o.check, p.o.client, e.Socket, share(p.o.dialBudget, deadline))
	if err != nil {
		res.err = err
		return res
	}
	rows, raws, err := c.list(ctx, share(p.o.listBudget, deadline), p.o.hub)
	if err != nil {
		c.close()
		res.err = err
		return res
	}
	return answered(res, c, rows, raws, e)
}

// share is the deadline of one share of an attempt that starts now: budget
// from now, and never past the attempt's own deadline.
func share(budget time.Duration, deadline time.Time) time.Time {
	if d := time.Now().Add(budget); d.Before(deadline) {
		return d
	}
	return deadline
}

// answered is res for rows, c's answer: c kept with the row of the session e
// names — a host serves one — and, when raws holds the rows' JSON values
// (the hub's roster), that row's; or, from a host that answered with none,
// closed and a failure.
func answered(res result, c *client, rows protocol.SessionsListResult, raws []json.RawMessage, e rundir.Entry) result {
	if len(rows.Sessions) == 0 {
		c.close()
		res.err = errNoRow
		return res
	}
	i := pick(rows, e)
	res.conn, res.row, res.version = c, rows.Sessions[i], c.version
	if i < len(raws) {
		res.raw = raws[i]
	}
	return res
}

// errNoRow is a host that answered sessions.list with no session.
var errNoRow = errors.New("roster: the host answered sessions.list with no session")

// pick is the index of the row of the session e names, else the first's:
// rows has one.
func pick(rows protocol.SessionsListResult, e rundir.Entry) int {
	for i, r := range rows.Sessions {
		if r.SessionID == e.CrazeSessionID {
			return i
		}
	}
	return 0
}

// apply takes one attempt's result: an answer keeps its connection and
// resets the backoff; a failure marks the host unreachable and doubles it;
// either, made for a session the host's entry no longer names, is not
// taken.
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
	if res.session != h.entry.CrazeSessionID {
		// Made for the session the host's entry named before it changed
		// (reconcile): what it read is the last session's, and is not taken.
		// Its connection, if it answered, is kept, and the host is asked
		// again at once — launch, after this: the attempt started a round
		// before the one whose registry read named the new session.
		h.conn = res.conn
		return
	}
	if p.o.hub && h.doneRun != h.startRun {
		// Row.Polled may have moved.
		p.dirty = true
	}
	h.doneRun = h.startRun
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
	h.readAt = p.o.now()
	if p.o.hub {
		// Every answer moves Row.ReadAt — what the hub's roster tells a fresh
		// row by — and may move Raw where Session is unchanged: a newer
		// host's member this build does not decode.
		h.raw = res.raw
		p.dirty = true
	}
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
//
// The hub's roster (options.hub) fills no slot: each Snapshot is handed to
// its publish, in order, on this goroutine.
func (p *poller) publish() {
	if !p.dirty {
		return
	}
	p.dirty = false
	snap := p.snapshot()
	if p.o.publish != nil {
		p.o.publish(snap)
		return
	}
	select {
	case <-p.r.out:
	default:
	}
	p.r.out <- snap
}

// snapshot is the Snapshot of now: the hosts in host-id order, each row a
// copy.
func (p *poller) snapshot() Snapshot {
	s := Snapshot{RegistryErr: p.regErr, IndexErr: p.saved.err, Saved: slices.Clone(p.saved.rows), Run: p.run}
	now := p.o.now()
	for _, h := range p.hosts {
		if h.gone {
			continue
		}
		row := Row{Host: HostOf(h.entry), Status: h.status, Version: h.version, IndexTitle: p.saved.titles[h.entry.CrazeSessionID],
			Raw: h.raw, ReadAt: h.readAt,
			Polled: p.run != 0 && h.doneRun == p.run || h.failures > 0 && now.Before(h.next)}
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
// connections and every kept one are closed, and every attempt's goroutine is
// joined — a result sent is not a goroutine finished, and Close answers for
// the goroutines.
func (p *poller) shutdown() {
	for p.inFlight > 0 {
		res := <-p.results
		p.inFlight--
		res.conn.close()
	}
	p.attempts.Wait()
	for id, h := range p.hosts {
		h.conn.close()
		delete(p.hosts, id)
	}
}
