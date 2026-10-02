package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/version"
)

// The session list's roster through the hub (plan 032 §3.13, P3): what the
// TUI's list reads (tui.SessionRoster) when it opens. The hub feeds the
// running rows — the TUI holds no connection to any host while the list is
// open, so opening it no longer leaves a note in every session's journal —
// and the list falls back to its own poller (roster.Open) when the hub cannot
// be had. Everything else the list does — open, stop, cancel, dispatch — goes
// to the hosts directly, as before (P3): a row's socket is resolved from the
// registry by its host id when it is acted on (internal/cli's sessionList).
//
// # The roster's life
//
// Roster returns at once — nothing waits on the TUI's Update — and the roster
// runs on a goroutine of its own, handing its Snapshots to the same
// latest-value slot the poller fills (Updates):
//
//   - Seeding (ModeSeeding): a first Snapshot at once — every host the
//     registry lists, connecting, as the poller's first one has them (a file
//     read, no connection), and the saved rows less those — then Ensure and
//     sessions.subscribe, all within seedWait. Answered: Hub. A failure, a
//     reply that says the hub's roster was cut (truncated: the hosts past
//     RosterRowsMax would be missing, r28 2), one with a reachable row the
//     list cannot draw (readRows), or no answer within seedWait: Poller. The bound is the list's own timer as well as the reach's
//     context: a reach that does not return at its deadline cannot hold the
//     list (r28 3); its late outcome is taken, and closed, when it comes.
//   - Hub (ModeHub): the running rows are the subscription's — its reply's
//     rows, then each roster notification's net change — each as the hub has
//     it (roster.FromRosterRow: no socket). A connecting or unreachable row
//     with no body to decode is named by its host's registry entry (its
//     provider session id and incarnation, which the saved half's legacy
//     rows are matched by, r28 4; never its socket), a change of that name
//     published on the saved half's tick (r30 3). A reachable row with no
//     body to decode — dropped over the hub's size bound, or one this build
//     cannot read — is a session that answers and that nothing the hub sent
//     says the state of (r30 4): the roster closes the subscription and runs
//     its poller, which reads every host's whole row. The saved half
//     is this CRAZE_HOME's, kept here on a ticker of its own (savedEvery) by
//     the index's file stamp, as the poller keeps it, whatever the hub sends
//     or does not; the running rows are taken out of it as they move. A lost
//     subscription — its connection's end, any reset (slow_consumer,
//     omitted, hub_closing), a line that does not read — is a loss: the
//     roster reaches the hub again through Ensure and subscribes afresh, its
//     reply reseeding every running row (a new hub's epoch included), within
//     reseedWait. lossMax losses within lossWindow, or a reseed that does
//     not come within reseedWait (or comes as the seed's would not be taken):
//     Poller. While it
//     reconnects the rows are the last the hub sent, and the saved half
//     keeps its ticker.
//   - Poller (ModePoller): the hub's connection closed and its reader
//     joined, the list's own poller (roster.Open) runs for the rest of this
//     opening, its Snapshots handed on as they come, whole. Once the hub's
//     rows were published, the hub's last whole Snapshot stands in their
//     place until the poller's latest has read the registry and heard from
//     every session the hub listed — at most listHoldCap for the second —
//     with the poller's registry error on it while its reads fail (poll).
//
// Close ends whichever is running and returns once every goroutine the
// roster started has finished: the connect in flight (cancelled: Ensure, the
// dial and the exchange are all bounded by the roster's context), the
// subscription's reader, the poller (its own Close).
//
// A Go test binary has no hub unless a test installs one (Command, or this
// file's seams): Ensure is ErrNoHub at once, so a test of the production list
// runs its poller.

// Mode says where a ListRoster's running rows come from now.
type Mode int32

const (
	// ModeSeeding: the hub is being reached; the running rows are the
	// registry's, connecting.
	ModeSeeding Mode = iota
	// ModeHub: the running rows are the hub's roster subscription's.
	ModeHub
	// ModePoller: the hub could not be had, or kept; the list's own poller
	// runs for the rest of this opening.
	ModePoller
)

func (m Mode) String() string {
	switch m {
	case ModeSeeding:
		return "seeding"
	case ModeHub:
		return "hub"
	case ModePoller:
		return "poller"
	}
	return "mode(?)"
}

// The list roster's rules (§3.13).
const (
	// listSeedWait bounds a reach of the hub — Ensure, the dial, hello and
	// sessions.subscribe — the first and every reseed after a loss.
	listSeedWait = 2 * time.Second
	// listLossMax losses within listLossWindow send the roster to the
	// poller.
	listLossMax    = 3
	listLossWindow = 10 * time.Second
	// listSavedEvery is how often the saved half is brought up to date: a
	// stat of the index's file, a read when it moved.
	listSavedEvery = time.Second
	// listHoldCap bounds how long a handover to the poller waits for the
	// sessions the hub listed to be heard from (poll), whatever they say.
	listHoldCap = 3 * time.Second
)

// listClient is who the list roster's hello says it is: the TUI's session
// list, as its poller's hello to a host says.
var listClient = protocol.ClientInfo{Kind: "tui", Name: "craze sessions", Version: version.Version}

// listPoller is the list's own poller as the roster runs it: roster.Open's.
type listPoller interface {
	Updates() <-chan roster.Snapshot
	Close()
}

// listOptions are a list roster's rules and seams: production's are
// listDefaults', and a test changes what it needs.
type listOptions struct {
	// ensure finds or starts the hub (Ensure).
	ensure func(ctx context.Context, env rundir.Env, need protocol.ConnectionCapabilities) (string, error)
	// dial connects to the hub's socket, its peer checked (dialHub).
	dial func(ctx context.Context, socket string) (net.Conn, error)
	// hosts reads the registry: the seed's running rows.
	hosts func() ([]rundir.Entry, error)
	// poller opens the list's own poller (roster.Open).
	poller func() listPoller
	// indexPath is the index file whose stamp says it changed
	// (roster.NewSaved's; nil: paths.SessionsPath).
	indexPath func() string
	// savedTicks, when set, stands in for the saved half's ticker.
	savedTicks <-chan time.Time
	// now is the clock losses are counted on.
	now func() time.Time
	// holdCap is the cap on a handover's wait for the sessions the hub listed
	// (poll): it fires listHoldCap after it is called, in production.
	holdCap func() <-chan time.Time

	// seedWait bounds the first reach of the hub, reseedWait each one after a
	// loss: both listSeedWait in production, apart so a test can hold one.
	seedWait, reseedWait   time.Duration
	lossWindow, savedEvery time.Duration
	lossMax                int

	// A test's hooks, nil in production, each run on the roster's goroutine:
	// moved is told each mode the roster enters; seeded each reply that
	// (re)seeds the running rows, with its epoch; published each Snapshot
	// as it is put in the slot.
	moved     func(Mode)
	seeded    func(epoch string)
	published func(roster.Snapshot)
}

func listDefaults(env rundir.Env, index roster.Index) listOptions {
	return listOptions{
		ensure:  Ensure,
		dial:    dialHub,
		hosts:   func() ([]rundir.Entry, error) { return hostsRead(env) },
		poller:  func() listPoller { return roster.Open(env, index) },
		now:     time.Now,
		holdCap: func() <-chan time.Time { return time.After(listHoldCap) },

		seedWait: listSeedWait, reseedWait: listSeedWait, lossWindow: listLossWindow, savedEvery: listSavedEvery,
		lossMax: listLossMax,
	}
}

// ListRoster is the session list's roster through the hub (§3.13): a
// tui.SessionRoster — Updates and Close — whose running rows are the hub's
// roster subscription's, or the list's own poller's when the hub cannot be
// had (the file's comment).
type ListRoster struct {
	env    rundir.Env
	o      listOptions
	ctx    context.Context
	cancel context.CancelFunc
	out    chan roster.Snapshot
	done   chan struct{}
	mode   atomic.Int32
	// joins counts the connects' goroutines until each has finished.
	joins sync.WaitGroup
	// last is the last Snapshot published: the roster's goroutine's alone.
	last roster.Snapshot
}

// Roster opens the session list's roster over env — the registry and the
// hub of this HOME and CRAZE_HOME — and index, this CRAZE_HOME's session
// index (nil: no saved rows), whose file is paths.SessionsPath's. It returns
// at once; its first Snapshot follows as soon as the registry and the index
// are read, before the hub is reached.
func Roster(env rundir.Env, index roster.Index) *ListRoster {
	return openList(env, index, listDefaults(env, index))
}

func openList(env rundir.Env, index roster.Index, o listOptions) *ListRoster {
	ctx, cancel := context.WithCancel(context.Background())
	r := &ListRoster{env: env, o: o, ctx: ctx, cancel: cancel, out: make(chan roster.Snapshot, 1), done: make(chan struct{})}
	go r.run(roster.NewSaved(index, o.indexPath))
	return r
}

// Updates is the latest-snapshot slot: capacity one, the newest Snapshot
// replacing one not yet taken, never waited on by the roster. It is closed
// once the roster has stopped (Close).
func (r *ListRoster) Updates() <-chan roster.Snapshot { return r.out }

// Close stops the roster and returns once every goroutine it started has
// finished: the connect in flight, the subscription's reader, the poller.
// It is idempotent.
func (r *ListRoster) Close() {
	r.cancel()
	<-r.done
}

// Mode is where the running rows come from now.
func (r *ListRoster) Mode() Mode { return Mode(r.mode.Load()) }

func (r *ListRoster) setMode(m Mode) {
	if Mode(r.mode.Swap(int32(m))) != m && r.o.moved != nil {
		r.o.moved(m)
	}
}

// publish puts s in the slot, replacing a Snapshot not yet taken. The
// roster's goroutine is the slot's only sender, so once it has emptied it the
// send cannot wait.
func (r *ListRoster) publish(s roster.Snapshot) {
	r.last = s
	if r.o.published != nil {
		r.o.published(s)
	}
	select {
	case <-r.out:
	default:
	}
	r.out <- s
}

// dialed is a connect's outcome: the subscription, or why there is none.
type dialed struct {
	sub *listSub
	err error
}

// run is the roster's goroutine: the seed, then the hub, or the poller (the
// file's comment).
func (r *ListRoster) run(saved *roster.Saved) {
	defer close(r.done)
	defer close(r.out)
	defer r.joins.Wait()
	l := &listRows{saved: saved, hosts: r.o.hosts, rows: map[string]hubRow{}, idents: map[string]rundir.Entry{}}
	l.seed()
	r.publish(l.snapshot())
	ticks := r.o.savedTicks
	if ticks == nil {
		t := time.NewTicker(r.o.savedEvery)
		defer t.Stop()
		ticks = t.C
	}
	var (
		sub    *listSub
		reach  *reaching
		losses []time.Time
		// hubbed: the hub's rows have been published, so a poller taking
		// over holds them until it has read the registry (poll, r34 2).
		hubbed bool
	)
	reach = r.connect(r.o.seedWait)
	for {
		var (
			events  <-chan listEvent
			pending <-chan dialed
			bound   <-chan time.Time
		)
		if sub != nil {
			events = sub.events
		}
		if reach != nil {
			pending, bound = reach.result, reach.timer.C
		}
		select {
		case <-r.ctx.Done():
			reach.abandon()
			sub.close()
			return
		case d := <-pending:
			reach.done()
			reach = nil
			if d.err != nil {
				r.poll(nil, hubbed)
				return
			}
			sub = d.sub
			l.reseed(sub.rows)
			r.publish(l.snapshot())
			hubbed = true
			if r.o.seeded != nil {
				r.o.seeded(sub.reply.Epoch)
			}
			r.setMode(ModeHub)
		case <-bound:
			// The reach outlived its bound. Its context ended with it, but
			// what it called may not have returned yet — an Ensure ending a
			// contender, say — and the list does not wait for it: the poller
			// takes over now, and takes the reach's late outcome when it
			// comes (r28 3).
			reach.cancel()
			r.poll(reach, hubbed)
			return
		case <-ticks:
			// A repaired identity is published whether or not the saved
			// half moved with it (r30 3).
			named := l.identify()
			if l.saved.Read(l.running()) || named {
				r.publish(l.snapshot())
			}
		case ev := <-events:
			if ev.roster != nil {
				rows, ok := readRows(ev.roster.Upserts)
				if !ok {
					// A session that answers, with no row the list can draw
					// it by: the poller reads every host's whole row
					// (r30 4).
					sub.close()
					r.poll(nil, hubbed)
					return
				}
				l.apply(ev.roster.Removes, rows)
				r.publish(l.snapshot())
				continue
			}
			sub.close()
			sub = nil
			now := r.o.now()
			losses = append(within(losses, now, r.o.lossWindow), now)
			if len(losses) >= r.o.lossMax {
				r.poll(nil, hubbed)
				return
			}
			reach = r.connect(r.o.reseedWait)
		}
	}
}

// within is the losses that happened within window before now.
func within(losses []time.Time, now time.Time, window time.Duration) []time.Time {
	return slices.DeleteFunc(losses, func(t time.Time) bool { return !t.After(now.Add(-window)) })
}

// poll is the Poller mode, for the rest of the roster's life: the list's own
// poller, its Snapshots handed on as they come, closed — and joined — as the
// roster stops. The hub's connection is closed and its reader joined before
// it starts (run). late, when not nil, is a reach of the hub that outlived
// its bound: its outcome is taken when it comes — a subscription it made
// closed and its reader joined — and waited for as the roster stops. A
// roster already stopping starts no poller.
//
// hold says the hub's rows were published: the poller is new, and until it
// has read the registry and heard from the sessions the hub listed, what it
// would show is less than the list already has — nothing running after a
// failed read (r34 2), or the hub's sessions connecting, a session that
// needs you no longer saying so, after a read that succeeded (r39 1). So the
// hub's last Snapshot — the last the list published, whole: its running rows
// and its saved half as they were — stands in the poller's place while the
// poller's latest Snapshot
//
//   - failed its registry read (RegistryErr: every one of the poller's
//     Snapshots follows a read, and RegistryErr is nil exactly when the last
//     read succeeded): the hub's last is published with the poller's error on
//     it — again only when that error is new or changed — so the list says the
//     running sessions could not be read under the rows it last had, as a
//     poller that has read the registry before does on a failed read; or
//   - still shows a host the hub's last listed with a session as connecting,
//     with none (waiting): the poller has not heard from it yet. A host the
//     registry no longer lists is not in the Snapshot, and does not count.
//     Nor is a Snapshot that lists saved a session its own running rows
//     serve taken (roster.Snapshot.SavesRunning): the poller takes its saved
//     half at its tick, and an answer that came since — a provider session id
//     the registry does not carry — leaves a legacy saved row beside its
//     running session until the next (r42 3). This wait ends holdCap after
//     the handover began, whatever the Snapshots say, so a host that never
//     answers cannot keep the list from moving.
//
// The condition is the latest Snapshot's alone — no state is kept of any one
// host — so a Snapshot the poller's slot replaced before the list took it
// loses nothing (r42 1). On release the latest is published, and every one
// after it, whole: the hub's rows are never mixed into the poller's, so the
// running rows and the saved half shown are always one source's (r42 3).
// Without hold — the seed's rows, read from the registry by the same rule the
// poller reads it by — every Snapshot is handed on as it comes.
func (r *ListRoster) poll(late *reaching, hold bool) {
	if r.ctx.Err() != nil {
		late.abandon()
		return
	}
	p := r.o.poller()
	defer p.Close()
	r.setMode(ModePoller)
	held := r.last
	heldErr := ""
	// listed is the hosts the hub's last listed with a session, until the cap
	// (capped) ends the wait for them.
	var (
		listed map[string]bool
		capped <-chan time.Time
		latest roster.Snapshot
		have   bool
	)
	if hold {
		listed = map[string]bool{}
		for _, row := range held.Running {
			if row.Session != nil {
				listed[row.Host.ID] = true
			}
		}
		capped = r.o.holdCap()
	}
	for {
		var pending <-chan dialed
		if late != nil {
			pending = late.result
		}
		select {
		case <-r.ctx.Done():
			late.abandon()
			return
		case d := <-pending:
			late.done()
			late = nil
			d.sub.close()
		case <-capped:
			capped, listed = nil, nil
			if hold && have && latest.RegistryErr == nil {
				hold = false
				r.publish(latest)
			}
		case s, ok := <-p.Updates():
			if !ok {
				late.abandon()
				return
			}
			if !hold {
				r.publish(s)
				continue
			}
			latest, have = s, true
			switch {
			case s.RegistryErr != nil:
				if why := s.RegistryErr.Error(); why != heldErr {
					heldErr = why
					h := held
					h.RegistryErr = s.RegistryErr
					r.publish(h)
				}
			case listed != nil && (waiting(s, listed) || s.SavesRunning()):
				if heldErr != "" {
					// Read now: the note goes, the hub's rows stay.
					heldErr = ""
					r.publish(held)
				}
			default:
				hold, capped = false, nil
				r.publish(s)
			}
		}
	}
}

// waiting says s still shows a host of listed connecting with no session: a
// host the poller has not heard from yet.
func waiting(s roster.Snapshot, listed map[string]bool) bool {
	for _, row := range s.Running {
		if listed[row.Host.ID] && row.Status == roster.Connecting && row.Session == nil {
			return true
		}
	}
	return false
}

// reaching is one reach of the hub in flight (connect): its outcome to come,
// its bound's own timer — the list's, apart from the reach's context, so a
// reach that does not return at its deadline cannot hold the list (r28 3) —
// and its context's cancel.
type reaching struct {
	result <-chan dialed
	timer  *time.Timer
	cancel context.CancelFunc
}

// done releases a reach whose outcome was taken. Nothing for nil.
func (h *reaching) done() {
	if h == nil {
		return
	}
	h.timer.Stop()
	h.cancel()
}

// abandon ends a reach whose outcome nobody wants — cancelled, its outcome
// waited for and a subscription it made closed. Nothing for nil.
func (h *reaching) abandon() {
	if h == nil {
		return
	}
	h.done()
	(<-h.result).sub.close()
}

// connect reaches the hub and subscribes (subscribe), within bound, on a
// goroutine of its own, which Close joins: its outcome comes on the reach's
// channel, which the roster always takes.
func (r *ListRoster) connect(bound time.Duration) *reaching {
	ch := make(chan dialed, 1)
	ctx, cancel := context.WithCancel(r.ctx)
	h := &reaching{result: ch, timer: time.NewTimer(bound), cancel: cancel}
	r.joins.Add(1)
	go func() {
		defer r.joins.Done()
		sub, err := r.subscribe(ctx, bound)
		ch <- dialed{sub: sub, err: err}
	}()
	return h
}

// subscribe is one reach of the hub, bounded by bound and by parent, the
// roster's life: Ensure — whose rounds only a deadline bounds (X34) — a
// connection to the socket it answers, hello and sessions.subscribe: the
// subscription, its reader started. The bound's end closes the connection
// mid-exchange, and is the error; so is a reply that says the roster was cut
// (errTruncated), or one with a session that answers and no row to draw it
// by (errUnreadable).
func (r *ListRoster) subscribe(parent context.Context, bound time.Duration) (*listSub, error) {
	ctx, cancel := context.WithTimeout(parent, bound)
	defer cancel()
	sock, err := r.o.ensure(ctx, r.env, protocol.ConnectionCapabilities{RosterSubscribe: true})
	if err != nil {
		return nil, err
	}
	nc, err := r.o.dial(ctx, sock)
	if err != nil {
		return nil, err
	}
	c := newHubConn(ctx, nc)
	res, err := subscribeOn(c, sock)
	if !c.detach() {
		// The bound ended mid-exchange, and closed the connection.
		_ = nc.Close()
		return nil, ctxOr(ctx, err)
	}
	if err == nil && res.Truncated {
		// The hub lists the first RosterRowsMax hosts and no more: the rest
		// would be missing from the list — and a saved row of one of theirs
		// shown saved while it runs (r28 2). Not a roster for the list: its
		// poller lists every host.
		err = errTruncated
	}
	var rows []hubRow
	if err == nil {
		var ok bool
		if rows, ok = readRows(res.Sessions); !ok {
			err = errUnreadable
		}
	}
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	s := &listSub{nc: nc, lr: c.lr, reply: res, rows: rows, events: make(chan listEvent), quit: make(chan struct{}),
		done: make(chan struct{})}
	go s.read()
	return s, nil
}

// errTruncated is a subscription whose reply says the hub's roster was cut at
// RosterRowsMax rows.
var errTruncated = fmt.Errorf("hub: the roster is cut at %d rows", protocol.RosterRowsMax)

// errUnreadable is a subscription whose roster has a session that answers
// and no row the list can draw it by (readRows).
var errUnreadable = errors.New("hub: a session that answers has no row the list can read")

// readRows is rrs as the list has them (listRowOf), or false when one is a
// row the hub reached with no body this build can decode — dropped over the
// hub's size bound, or one it cannot read (r30 4). Such a session answers,
// and nothing the hub sent says what it is doing — whether it waits for the
// user — so the list cannot draw it; the poller, which reads every host's
// whole row itself, can. A row with no body that is connecting or
// unreachable is listed as such.
func readRows(rrs []protocol.RosterRow) ([]hubRow, bool) {
	out := make([]hubRow, 0, len(rrs))
	for _, rr := range rrs {
		h := listRowOf(rr)
		if rr.Status == protocol.RosterReachable && h.row.Session == nil {
			return nil, false
		}
		out = append(out, h)
	}
	return out, true
}

// subscribeOn says hello on c and subscribes to the roster: the reply, its
// epoch the hub's own id.
func subscribeOn(c *hubConn, socket string) (protocol.SessionsSubscribeResult, error) {
	hello, err := c.hello(socket, listClient)
	if err != nil {
		return protocol.SessionsSubscribeResult{}, err
	}
	if !hello.Capabilities.RosterSubscribe {
		return protocol.SessionsSubscribeResult{}, &LacksError{Version: hello.Endpoint.CrazeVersion,
			Missing: missing(hello.Capabilities, protocol.ConnectionCapabilities{RosterSubscribe: true})}
	}
	b, err := c.call(protocol.MethodSessionsSubscribe, protocol.SessionsSubscribeParams{})
	if err != nil {
		return protocol.SessionsSubscribeResult{}, err
	}
	var res protocol.SessionsSubscribeResult
	if err := json.Unmarshal(b, &res); err != nil {
		return protocol.SessionsSubscribeResult{}, fmt.Errorf("hub: %s's result: %w", protocol.MethodSessionsSubscribe, err)
	}
	switch {
	case res.Subscription == "":
		return protocol.SessionsSubscribeResult{}, errors.New("hub: the roster subscription has no id")
	case res.Epoch != hello.Endpoint.HostID:
		return protocol.SessionsSubscribeResult{}, fmt.Errorf("hub: the roster's epoch %q is not the hub's id %q", res.Epoch, hello.Endpoint.HostID)
	}
	return res, nil
}

// listSub is the roster's subscription on its own connection: the reply it
// was answered, and a reader that hands each notification of it to the
// roster's goroutine (events) until the subscription is lost.
type listSub struct {
	nc    net.Conn
	lr    *protocol.LineReader
	reply protocol.SessionsSubscribeResult
	// rows is the reply's rows as the list has them (readRows).
	rows []hubRow
	// events carries each roster notification, then the loss that ends the
	// subscription, after which the reader has returned; quit tells the
	// reader nobody takes them any more; done is closed once it has
	// returned.
	events chan listEvent
	quit   chan struct{}
	done   chan struct{}
}

// listEvent is one roster notification of the subscription's, or — roster
// nil — its loss, and why.
type listEvent struct {
	roster *protocol.RosterParams
	lost   error
}

// resetError is a roster subscription the hub ended with a reset: why.
type resetError struct{ Reason protocol.ResetReason }

func (e *resetError) Error() string {
	return "hub: the roster subscription was reset: " + string(e.Reason)
}

// close ends the subscription — its connection closed — and joins its
// reader. Nothing for nil.
func (s *listSub) close() {
	if s == nil {
		return
	}
	close(s.quit)
	_ = s.nc.Close()
	<-s.done
}

// read is the subscription's reader: each roster notification of its
// subscription handed on, a notification of another subscription or of a
// kind this build does not know passed over, and the first reset of its
// subscription, end of the connection or line that does not read handed on
// as the loss — the last thing it does.
func (s *listSub) read() {
	defer close(s.done)
	for {
		line, err := s.lr.ReadLine()
		if err != nil {
			s.send(listEvent{lost: fmt.Errorf("hub: the roster subscription's connection: %w", err)})
			return
		}
		var m struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			s.send(listEvent{lost: fmt.Errorf("hub: a line of the roster subscription: %w", err)})
			return
		}
		switch m.Method {
		case protocol.NotifyRoster:
			var p protocol.RosterParams
			if err := json.Unmarshal(m.Params, &p); err != nil {
				s.send(listEvent{lost: fmt.Errorf("hub: a roster notification: %w", err)})
				return
			}
			if p.Subscription != s.reply.Subscription {
				continue
			}
			if p.Epoch != s.reply.Epoch {
				s.send(listEvent{lost: fmt.Errorf("hub: a roster notification of epoch %q, not %q", p.Epoch, s.reply.Epoch)})
				return
			}
			if !s.send(listEvent{roster: &p}) {
				return
			}
		case protocol.NotifyReset:
			var p protocol.ResetParams
			if err := json.Unmarshal(m.Params, &p); err != nil {
				s.send(listEvent{lost: fmt.Errorf("hub: a reset notification: %w", err)})
				return
			}
			if p.Subscription == s.reply.Subscription {
				s.send(listEvent{lost: &resetError{Reason: p.Reason}})
				return
			}
		}
	}
}

// send hands ev to the roster's goroutine: false once nobody takes it (the
// subscription closed).
func (s *listSub) send(ev listEvent) bool {
	select {
	case s.events <- ev:
		return true
	case <-s.quit:
		return false
	}
}

// listRows is the roster's rows, its goroutine's alone: the running ones by
// host id — each as the hub gave it, or the registry before the hub answers —
// the registry's entries that complete a row with no body, and the saved
// half.
type listRows struct {
	saved *roster.Saved
	// hosts reads the registry (listOptions.hosts).
	hosts func() ([]rundir.Entry, error)
	rows  map[string]hubRow
	// idents is the registry's entries by host id, as its last good read
	// listed them.
	idents map[string]rundir.Entry
	// regErr is the seed's registry read's failure, nil once the hub has
	// answered.
	regErr error
}

// hubRow is one running row, and whether no session body was decoded for
// it — a host the hub has not read yet, or one it lost before it read one this
// build can decode (a reachable one is the poller's, readRows) — so that what
// only a body says of its session comes from its host's registry entry
// instead (complete).
type hubRow struct {
	row      roster.Row
	bodiless bool
}

// seed is the first Snapshot's rows: every host the registry lists,
// connecting — a host as the poller's first Snapshot has it — and the saved
// rows less those (roster.Saved).
func (l *listRows) seed() {
	entries, err := l.hosts()
	l.regErr = err
	l.remember(entries, err)
	for _, e := range entries {
		l.rows[e.HostID] = hubRow{row: roster.Row{Host: roster.HostOf(e), Status: roster.Connecting}, bodiless: true}
	}
	l.saved.Read(l.running())
}

// remember keeps a registry read's entries: a read that failed keeps the
// last good one's.
func (l *listRows) remember(entries []rundir.Entry, err error) {
	if err != nil {
		return
	}
	l.idents = make(map[string]rundir.Entry, len(entries))
	for _, e := range entries {
		l.idents[e.HostID] = e
	}
}

// identify reads the registry again while a row has no body: what its
// session is besides its craze id — its provider session id, which a legacy
// saved row is matched by, and its incarnation, which the list keys the row
// by — only its registry entry says (r28 4). A file read, never a connection
// to a host. Each saved tick takes it, and it says whether the read changed
// what such a row is named by: a repair the list is told of whatever else
// moved (r30 3).
func (l *listRows) identify() bool {
	before := l.names()
	if len(before) == 0 {
		return false
	}
	l.remember(l.hosts())
	return !maps.Equal(before, l.names())
}

// names is what every row with no body is named by as completed now
// (complete), by host id.
func (l *listRows) names() map[string][3]string {
	out := map[string][3]string{}
	for id, h := range l.rows {
		if !h.bodiless {
			continue
		}
		r := h.row
		e, ok := l.idents[id]
		complete(&r, e, ok)
		out[id] = [3]string{r.Host.ProviderSessionID, r.Host.Incarnation, r.Host.Provider}
	}
	return out
}

// identifyNew is identify for a row with no body whose host, or whose host's
// session, the last read did not list: a row the hub has just sent.
func (l *listRows) identifyNew() {
	for id, h := range l.rows {
		if e, ok := l.idents[id]; h.bodiless && (!ok || e.CrazeSessionID != h.row.Host.CrazeSessionID) {
			l.remember(l.hosts())
			return
		}
	}
}

// reseed makes the running rows rows, a subscription's reply's (readRows).
func (l *listRows) reseed(rows []hubRow) {
	l.rows = make(map[string]hubRow, len(rows))
	for _, h := range rows {
		l.rows[h.row.Host.ID] = h
	}
	l.regErr = nil
	l.identifyNew()
	l.saved.Subtract(l.running())
}

// apply takes a roster notification's net change: removes, the host ids
// gone, and upserts, its rows (readRows).
func (l *listRows) apply(removes []string, upserts []hubRow) {
	for _, id := range removes {
		delete(l.rows, id)
	}
	for _, h := range upserts {
		l.rows[h.row.Host.ID] = h
	}
	l.identifyNew()
	l.saved.Subtract(l.running())
}

// running is the running rows in host-id order, each without a body
// completed from its registry entry.
func (l *listRows) running() []roster.Row {
	out := make([]roster.Row, 0, len(l.rows))
	for id, h := range l.rows {
		r := h.row
		if h.bodiless {
			e, ok := l.idents[id]
			complete(&r, e, ok)
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b roster.Row) int { return strings.Compare(a.Host.ID, b.Host.ID) })
	return out
}

// complete fills in what r, a row with no session body, cannot say of its
// session from e, its host's registry entry — when ok and e names the same
// craze session — and never the socket, which an action resolves as it acts
// (internal/cli): its provider session id and incarnation, and its provider
// when the hub named none. Nothing is made up of what the session is doing:
// a row with no body is connecting or unreachable, as the hub says.
func complete(r *roster.Row, e rundir.Entry, ok bool) {
	if !ok || e.CrazeSessionID != r.Host.CrazeSessionID {
		return
	}
	if r.Host.ProviderSessionID == "" {
		r.Host.ProviderSessionID = e.ProviderSessionID
	}
	if r.Host.Incarnation == "" {
		r.Host.Incarnation = e.Incarnation
	}
	if r.Host.Provider == "" {
		r.Host.Provider = e.Provider
	}
}

// snapshot is the Snapshot of now: the running rows, each with its index
// title, and the saved half. The list's roster is one run (Snapshot.Run).
func (l *listRows) snapshot() roster.Snapshot {
	run := l.running()
	for i := range run {
		run[i].IndexTitle = l.saved.Title(run[i].Host.CrazeSessionID)
	}
	return roster.Snapshot{Running: run, Saved: l.saved.Rows(), RegistryErr: l.regErr, IndexErr: l.saved.Err(), Run: 1}
}

// listRowOf is a roster row as the list has it (roster.FromRosterRow), and
// whether it came without a session body. A body that is not a JSON object —
// null among them, which would decode into a session of zero values (r34 4)
// — is no body. A row this build cannot decode — a newer host's member of a
// type it does not expect — is a host whose answer cannot be read: listed
// unreachable, with no session, as the poller lists a host whose answer does
// not decode.
func listRowOf(rr protocol.RosterRow) hubRow {
	if !isObject(rr.Row) {
		rr.Row = nil
	}
	row, err := roster.FromRosterRow(rr)
	if err == nil {
		return hubRow{row: row, bodiless: row.Session == nil}
	}
	rr.Row = nil
	row, _ = roster.FromRosterRow(rr)
	row.Status = roster.Unreachable
	return hubRow{row: row, bodiless: true}
}
