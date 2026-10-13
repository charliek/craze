package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
)

// session.reopen (plan 046 B3): "ensure this saved session is running". Its
// id is a sessions.saved id — the session's craze id, or
// <provider>:<providerSessionId> for an index row with none — and its answer
// is the session's roster row once its start is done, and whether the hub
// joined a host that served it already (joined) or started one to load it.
//
// # Retry safety, by session
//
// One host serves a session: the one holding its claim (rundir.ClaimSession),
// which every loader — this hub, another hub, the TUI, craze send, a direct
// craze serve --load — takes before it loads it (the one exception: a
// TUI-hosted session whose claim could not take its lock file runs unclaimed,
// internal/cli's sessionClaims). A
// reopen joins that host, or spawns one that takes the claim; a spawn that
// loses the claim is answered held, and the reopen joins the winner. So two
// reopens at once, on one hub or two, end on one host, and a client that lost
// its answer sends the same id again and ends on the host serving the session
// then: the host id and joined may differ across tries (the first try's host
// may have idled out meanwhile), and a try after the session was stopped
// loads it again, by design. There is no requestId, and nothing is shared
// between reopens in flight: the claim reconciles them.
//
// # A reopen, in order (reopening.run)
//
//  1. Admitted as a create is (creator.admitReopen): counted among the
//     creates in flight (createsMax, busy past it; closing once the hub is),
//     which hold off the hub's idle exit, and run on a goroutine of its own
//     (creator.run), so a waiter that goes does not cancel it; the
//     teardown's cut cuts it as it cuts a create (its waiter's connection
//     closes unanswered). Everything below runs on that goroutine.
//  2. The id resolved (Creates.Reopen, CLI code reading the hub's own index)
//     to the row a load of it loads — a craze id's newest row; for a legacy
//     id whose row has been given a craze id since, that craze id's —
//     unknown_session for an id no row has, not_resumable for a row that
//     cannot be resumed, never an older row in its place. Nothing is stat'ed.
//  3. The holder resolved, in one loop bounded by the reopen's deadline, the
//     holder allowance (reopenHolderWait, spent only waiting for holders), at
//     most reopenSpawnsMax spawns and as many hosts looked past, each host
//     counted once however often it is (reopening.lookedPast):
//     a. The registry scanned, the whole $HOME's, any namespace (hostsScan):
//     an entry serving the session — by craze id, or by the legacy key, best
//     effort (an entry carries its provider session id only once its engine
//     is ready; the claim catches what the scan misses, at b) — is its
//     holder. A registry that cannot be read, or a live host's entry in it,
//     is unavailable, reason host_unreachable: fail closed, as a create's
//     recovery does. Of several, the reopen takes one to join before one on
//     its way out, and that before one it has looked past whose entry reads
//     as it did then (rank). A holder serving is joined (c); one on its way
//     out — marked stopping, or held by the reopen as leaving (c) — is
//     waited for until its process has gone (hostspawn.AwaitHolder), or,
//     held leaving only tentatively, until the reopen has waited a poll and
//     can try it again; one looked
//     past as it was (hostspawn.SameEngine: its rewrite not landed yet),
//     taken only when there is no other, is waited for until that entry
//     changes or its process has gone, never joined again; either wait,
//     then a again; none, a spawn (b). No wait answers an entry the scan
//     would not join (await): every turn of the loop joins, spawns, counts
//     a move, or waits a poll charged to the holder allowance.
//     b. A spawn: the workspace checked (Reopening.Runnable: no longer a
//     directory is not_accepting, reason not_resumable, in its words), then
//     craze serve --load <id> with the row's reload settings
//     (Reopening's) — never --provider, --workspace, --title, --fast or
//     --no-fast, and --model, --effort and --no-force only as those settings
//     say — in the workspace, through the create's launch (launchReady:
//     its reservation, its owned host, its hub's-craze-gone refusal). Its
//     ready line: ok is the session's host (d); held is a claim another
//     host holds — this host exits by itself, settled and reaped, the
//     holder never touched — waited for until it serves the session or its
//     process has gone (AwaitHolder: a holder between its entry's unlink and
//     its claim's release included), then a again; refused transient (the
//     index busy or changed) is unavailable, reason busy; refused otherwise
//     not_accepting, reason not_resumable, in the host's words; any other
//     not-ok line spawn_failed.
//     c. A join (reopening.join) — never this hub's to stop, on any branch:
//     the host dialled and attached with when: ready and no prompt, its
//     start waited for within the start budget, and its row read naming the
//     session asked for (sessionRow, exact): a host that no longer serves it
//     — its row or its entry naming another session (a TUI-hosted session's
//     engine changed), or its attach refused unknown_session — is looked
//     past, a again. An attach refused closing — a
//     stop whose receipt went out before the scan read its mark, a host
//     from before the mark that never writes one, or only an idle check's
//     fence (control's FenceClose), which lifts if the host stays — is a
//     holder on its way out: for good if its entry has gone or been marked
//     stopping by then, else tentatively, tried again once the reopen has
//     waited rendezvousPoll (reopening.leaving); waited for, then a again.
//     So is a dial or a connection lost whose entry has since gone or been
//     marked stopping, for good. Any other failure
//     is the answer, the host left exactly as it is — never a session.stop,
//     never a signal: a start that failed or ran past the budget is
//     not_accepting, reason start_failed; a host that cannot be reached
//     unavailable, reason host_unreachable. The answer says joined.
//     d. The session's own host: attended as a create's is, its start within
//     the start budget, and on any failure stopped (abandonHost) as a
//     create stops its own; its row read as a join's is, and a host of its
//     own that no longer serves the session stopped so too, then looked
//     past. The answer says not joined.
//     The holder allowance running out in a wait is unavailable, reason
//     busy, naming the holder; so is a fourth spawn wanted, or a fourth host
//     looked past; and so is the reopen's deadline passing (spent).
//  4. The row: the session's own, read fresh after the hub has let go of it
//     (sessionRow, exact) — approximate only as any roster row is — or, when
//     that read fails, the registry's, approximate, the answer a success all
//     the same (create's X49 rule); either naming the session asked for, or
//     the host is looked past (c, d).
//
// A joined host's workspace is never stat'ed: one whose directory has gone
// since serves on, and is joined.
//
// # Budgets
//
// reopenWait, 180 s, bounds the whole reopen from its admission (deadline):
// every phase is cut to it — the holder waits, which take reopenHolderWait
// (20 s) in all, counting only the time spent waiting; each spawn's ready
// line, hostspawn.ReadyWait (10 s); each dial, createDialWait; each start —
// initialize, which has no deadline of its own, the agent's session/load
// (90 s, acp's) and the catalog's wait — reopenStartWait from its dial's end;
// and the row's read — and a reopen whose deadline passes before the session
// runs is busy (spent), its own host stopped, a joined one left alone. The
// teardown's cut alone is closing (ctx). The index read and the workspace's
// stat are not bounded, as a create's world check is not. A client allows at
// least 200 s, and uses one short connection per reopen: a hub answers a
// connection's requests in order, and a reopen holds its connection that
// long.
//
// # Shutdown
//
// A reopen in flight when the hub tears down is a create in the same phase:
// cut, its waiter's connection closed unanswered; a launch that completes
// once the hub is closing is killed (creator.launch); a join is cut and its
// host left alone.

// Reopening is a saved session as session.reopen resolves its id
// (Creates.Reopen): what the reopen joins or loads.
type Reopening struct {
	// Load is craze serve --load's value: the session's craze id, or
	// <provider>:<providerSessionId> for an index row that has none yet (its
	// host gives it one under its own claim).
	Load string
	// CrazeID is the session's craze id, "" for such a legacy row; Provider
	// and ProviderSessionID its provider and the provider's session id, the
	// legacy key a registry entry is matched by too.
	CrazeID                     string
	Provider, ProviderSessionID string
	// Workspace is the directory the session ran in, its index row's: the
	// spawned host's working directory, from which it loads the row.
	Workspace string
	// Model, Effort, NoForce, Ask, Plan and Unattended are the row's reload
	// settings (internal/cli's reloadFlags, as craze send reloads a session):
	// a sub-agent's (unattended) row's recorded start settings and
	// --unattended; all zero for a person's row, which loads as the plain
	// default — bypass, no model, no mode.
	Model, Effort                  string
	NoForce, Ask, Plan, Unattended bool
	// Runnable checks the row against the world now, on the spawn path alone
	// (internal/cli's savedRunnable, called in place): nil when a host can
	// load it here, else why not, in words a refusal carries — its workspace
	// no longer a directory. nil is a row always runnable.
	Runnable func() error
}

// session.reopen's bounds: variables only so a test can change them (never
// in parallel).
var (
	// reopenWait bounds a reopen, from its admission.
	reopenWait = 180 * time.Second
	// reopenHolderWait is a reopen's allowance for waits for holders — a
	// holder stopping, or one a held answer named — in all: only the time
	// spent waiting counts against it.
	reopenHolderWait = 20 * time.Second
	// reopenStartWait bounds a session's start in a reopen, or what is left
	// of reopenWait when that is less.
	reopenStartWait = 150 * time.Second
	// reopenSpawnsMax is how many hosts one reopen may spawn, and how many
	// it may look past (hosts found no longer serving the session, each
	// counted once).
	reopenSpawnsMax = 3
)

// The reopen's seams, no-ops in production: a test sets one (never in
// parallel) to force a schedule. reopenScanned is told the registry scan's
// holder — the entry, and whether there was one — before the reopen acts on
// it; reopenJoining each holder the reopen is about to dial; reopenSpawning
// each spawn the reopen is about to launch, with the reopen's deadline,
// which it may move (to have it pass during the spawn's ready read, whatever
// the scheduling before it).
var (
	reopenScanned  = func(rundir.Entry, bool) {}
	reopenJoining  = func(rundir.Entry) {}
	reopenSpawning = func(*time.Time) {}
)

// reopenAnswer is the answer reopen takes, nil for a hub that does not serve
// it (dispatch falls through to unsupported).
func (h *hub) reopenAnswer() func(string, func(string, ...any)) (Reopening, *protocol.Error) {
	if h.cr == nil {
		return nil
	}
	return h.cr.o.Reopen
}

// reopen answers session.reopen (the file's comment) on the connection's own
// goroutine, which waits for the reopen's answer — the reopen itself runs on
// its own (creator.run), so a waiter that goes does not cancel it.
func (c *conn) reopen(req *request) bool {
	id, perr := reopenParams(req.params)
	if perr != nil {
		return c.replyErr(req.id, perr)
	}
	cr := c.s.h.cr
	call, perr := cr.admitReopen(id)
	if perr != nil {
		return c.replyErr(req.id, perr)
	}
	ans, ok := call.wait(cr.cut)
	if !ok {
		return false
	}
	if ans.err != nil {
		return c.replyErr(req.id, ans.err)
	}
	return c.reply(req.id, ans.reopened)
}

// reopenMembers is every member session.reopen's params define.
var reopenMembers = []string{"id"}

// reopenParams holds raw to session.reopen's params' form (strict, as
// savedParams holds sessions.saved's): an object whose one member is id, a
// string of 1 to protocol.ReopenIDMax bytes. Absent or null params, a member
// given twice, a null or a value of another type, and an empty or oversized
// id are bad_request; a member it does not define is unknown_field, naming
// it. The id is opaque here: Creates.Reopen resolves it.
func reopenParams(raw json.RawMessage) (string, *protocol.Error) {
	b := bytes.TrimSpace(raw)
	switch {
	case len(b) == 0:
		return "", badParams("params.id is required: the saved session to reopen (a sessions.saved id)")
	case string(b) == "null":
		return "", badParams("params must be an object, not null")
	}
	if k, ok := repeatedMember(b); ok {
		return "", badParams("params.%s is given more than once", k)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return "", badParams("params must be an object")
	}
	var unknown []string
	for k := range m {
		if !slices.Contains(reopenMembers, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return "", &protocol.Error{Code: protocol.RPCInvalidParams, Message: "unknown field params." + unknown[0],
			Data: protocol.ErrorData{Code: protocol.CodeBadRequest, Reason: protocol.ReasonUnknownField}}
	}
	v, ok := m["id"]
	if !ok {
		return "", badParams("params.id is required: the saved session to reopen (a sessions.saved id)")
	}
	var id string
	switch {
	case isNull(v) || json.Unmarshal(v, &id) != nil:
		return "", badParams("params.id must be a string")
	case id == "":
		return "", badParams("params.id must not be empty")
	case len(id) > protocol.ReopenIDMax:
		return "", badParams("params.id is longer than %d bytes", protocol.ReopenIDMax)
	}
	return id, nil
}

// admitReopen admits one reopen (the file's comment, step 1): counted in
// flight as a create is (lifecycle.beginCreate: busy past createsMax, closing
// once the hub is), and run on a goroutine of its own — through the create's
// call, never kept under any key: there is no requestId, and nothing is
// shared between reopens.
func (cr *creator) admitReopen(id string) (*createCall, *protocol.Error) {
	if perr := cr.h.life.beginCreate(); perr != nil {
		return nil, perr
	}
	admitted := time.Now()
	c := &createCall{done: make(chan struct{})}
	go cr.run(c, func(ctx context.Context) createAnswer { return cr.h.reopen(ctx, id, admitted) })
	return c, nil
}

// reopen is one reopen, from its id to its answer (the file's comment, steps
// 2–4), admitted at admitted.
func (h *hub) reopen(ctx context.Context, id string, admitted time.Time) createAnswer {
	tag := fmt.Sprintf("session.reopen %q", id)
	t, perr := h.cr.o.Reopen(id, func(format string, args ...any) { h.logf(tag+": "+format, args...) })
	if perr != nil {
		h.logf("%s: %s (%s)", tag, perr.Message, perr.Data.Reason)
		return createRefusal(perr)
	}
	r := &reopening{h: h, ctx: ctx, tag: tag, t: t, deadline: admitted.Add(reopenWait), holderLeft: reopenHolderWait,
		lookedPast: map[string]rundir.Entry{}, leaving: map[string]leavingMark{}}
	return r.run()
}

// reopening is one reopen's holder resolution (the file's comment, step 3).
type reopening struct {
	h   *hub
	ctx context.Context
	tag string
	t   Reopening
	// deadline is the whole reopen's: reopenWait from its admission. ctx is
	// the teardown's cut alone, so its end is always closing; the deadline
	// passing is spent, or a holder's busy.
	deadline time.Time
	// holderLeft is what is left of the holder waits' allowance
	// (reopenHolderWait): spent only while the reopen waits for a holder
	// (await), never by a join's or a spawn's own time.
	holderLeft time.Duration
	// spawns is how many hosts it has spawned.
	spawns int
	// lookedPast is every host it found no longer serving the session when
	// it looked at it, by host id: the host's entry as the reopen read it
	// then (a host of its own, its id alone). Each counts once against
	// reopenSpawnsMax, however often it is looked past: a host seen again
	// with its entry as it was is waited for (run), and one rewritten since
	// is looked at again.
	lookedPast map[string]rundir.Entry
	// leaving is every host the reopen has found on its way out though its
	// entry may not say so, by host id (rank). For good (sure) once its
	// entry had gone or been marked stopping when its dial or connection was
	// lost, or its attach refused closing: a host never comes back from its
	// stop. Tentatively when its attach was refused closing while its entry
	// still served the session, unmarked — a stop, or only an idle check's
	// fence (control's FenceClose), which lifts if the host stays: until the
	// reopen has waited rendezvousPoll since (waited), a poll of its wait
	// charged to the holder allowance, after which it is a host to join
	// again.
	leaving map[string]leavingMark
	// waitedFor is the time the reopen's waits for holders have taken, by
	// reopenClock (await); waitStart when the wait under way began, zero
	// between waits.
	waitedFor time.Duration
	waitStart time.Time
}

// leavingMark is a host in reopening.leaving: on its way out for good, or
// tentatively since the reopen had waited waited (reopening.waited).
type leavingMark struct {
	sure   bool
	waited time.Duration
}

// holderLook is what one look at the session's holder came to: the reopen's
// answer (done); a holder to wait for, then look again (await); or neither —
// the host looked at, past, no longer serves the session — to look again at
// once.
type holderLook struct {
	ans   createAnswer
	done  bool
	await *hostspawn.Holder
	past  rundir.Entry
}

// lookPast is the holderLook of a host that no longer serves the session:
// past, its entry as the reopen read it.
func lookPast(past rundir.Entry) holderLook { return holderLook{past: past} }

// answered is the holderLook that is the reopen's answer.
func answered(a createAnswer) holderLook { return holderLook{ans: a, done: true} }

// holder is the session as a registry entry is matched against it: its craze
// id — learned from a held answer when the row had none — and its legacy key.
func (r *reopening) holder() hostspawn.Holder {
	return hostspawn.Holder{CrazeSessionID: r.t.CrazeID, Provider: r.t.Provider, ProviderSessionID: r.t.ProviderSessionID}
}

// over says the reopen's whole deadline has passed.
func (r *reopening) over() bool { return !time.Now().Before(r.deadline) }

// run is the holder resolution's loop (the file's comment, step 3).
func (r *reopening) run() createAnswer {
	for {
		switch {
		case r.ctx.Err() != nil:
			return createRefusal(closingErr())
		case r.over():
			return r.spent()
		}
		e, found, perr := r.scan()
		reopenScanned(e, found)
		var s holderLook
		switch {
		case perr != nil:
			return createRefusal(perr)
		case found && r.rank(e) == rankLeaving:
			r.h.logf("%s: host %s (pid %d) serves the session and is on its way out: waiting for it to go", r.tag, e.HostID, e.PID)
			holder := leavingHolder(e)
			if rundir.Serving(e) && !r.leaving[e.HostID].sure {
				// Tentatively leaving: its own entry may answer the wait
				// once its time is up (rank), for the loop to try it again.
				holder = hostspawn.HolderOfEntry(e)
			}
			s.await = &holder
		case found && r.rank(e) == rankPast:
			// Looked past already, its entry as it was then, and no other
			// host serving the session: waited for until that entry changes,
			// within the holder allowance — never joined again on the same
			// entry, nor counted again.
			r.h.logf("%s: host %s (pid %d) was found not to serve the session, and its entry is as it was: waiting for it to change",
				r.tag, e.HostID, e.PID)
			holder := hostspawn.HolderOfEntry(e)
			holder.Stale = &e
			s.await = &holder
		case found:
			s = r.join(e)
		case r.spawns == reopenSpawnsMax:
			return r.changedHands(r.spawns)
		default:
			r.spawns++
			s = r.spawn()
		}
		switch {
		case s.done:
			return s.ans
		case s.await != nil:
			if perr := r.await(*s.await); perr != nil {
				return createRefusal(perr)
			}
		default:
			if _, again := r.lookedPast[s.past.HostID]; !again && len(r.lookedPast) == reopenSpawnsMax {
				return r.changedHands(reopenSpawnsMax + 1)
			}
			r.lookedPast[s.past.HostID] = s.past
		}
	}
}

// An entry serving the session, as the reopen ranks it (rank): to join; on
// its way out, to wait for; or looked past as it is, to wait for only when
// there is no other.
const (
	rankJoin = iota
	rankLeaving
	rankPast
)

// rank is where e, an entry serving the session, stands as the reopen chooses
// among them (scan) — and whether a wait may answer it (await: rankJoin
// alone). rankPast: a host the reopen has looked past whose entry is as it
// was then (hostspawn.SameEngine). rankLeaving: a host marked stopping, or
// one the reopen holds on its way out now (leavingNow). rankJoin: any other.
func (r *reopening) rank(e rundir.Entry) int {
	if past, ok := r.lookedPast[e.HostID]; ok && hostspawn.SameEngine(past, e) {
		return rankPast
	}
	if !rundir.Serving(e) || r.leavingNow(e.HostID) {
		return rankLeaving
	}
	return rankJoin
}

// leavingNow says the reopen holds host id on its way out now (leaving): for
// good, or tentatively until it has waited rendezvousPoll since — so a host
// whose attach a passing fence refused is tried again a poll later, and two
// such hosts cannot take turns without a poll's sleep between, charged to the
// holder allowance.
func (r *reopening) leavingNow(id string) bool {
	m, ok := r.leaving[id]
	return ok && (m.sure || r.waited()-m.waited < rendezvousPoll)
}

// waited is the time the reopen has waited for holders so far, the wait under
// way's included, by reopenClock.
func (r *reopening) waited() time.Duration {
	w := r.waitedFor
	if !r.waitStart.IsZero() {
		w += reopenClock().Sub(r.waitStart)
	}
	return w
}

// changedHands is the answer to a reopen whose session moved n times without
// one of its hosts serving it to the end — spawns answered held by holders
// that went, or n hosts no longer serving it when looked at: busy.
func (r *reopening) changedHands(n int) createAnswer {
	r.h.logf("%s: the session changed hands %d times: refused", r.tag, n)
	return createRefusal(refused(protocol.CodeUnavailable, protocol.ReasonBusy,
		"that session changed hands %d times while the hub reopened it; try again", n))
}

// leavingHolder is the holder e names, known to be on its way out: its own
// entry is never the wait's answer (hostspawn.Holder.Leaving).
func leavingHolder(e rundir.Entry) hostspawn.Holder {
	h := hostspawn.HolderOfEntry(e)
	h.Leaving = true
	return h
}

// scan is step a: the live host serving the session, from the registry — the
// whole $HOME's, any namespace — and whether there is one: the best ranked
// (rank: a host to join, then one on its way out, then one looked past as it
// was), and of several alike the newest. A registry that cannot be read, or a
// live host's entry in it (which may be the one), is unavailable, reason
// host_unreachable: its absence must be proven before a host is spawned to
// load it.
func (r *reopening) scan() (rundir.Entry, bool, *protocol.Error) {
	entries, perr := r.h.liveHosts(r.tag, "a host serves that session already")
	if perr != nil {
		return rundir.Entry{}, false, perr
	}
	h := r.holder()
	var found rundir.Entry
	ok := false
	for _, e := range entries {
		if !h.Serves(e) {
			continue
		}
		better := !ok || r.rank(e) < r.rank(found) || (r.rank(e) == r.rank(found) && e.StartedAt.After(found.StartedAt))
		if better {
			found, ok = e, true
		}
	}
	return found, ok, nil
}

// reopenClock is what the holder allowance is measured by (await): time.Now,
// and a test's seam to make a join's own time long without waiting it out
// (never in parallel).
var reopenClock = time.Now

// await waits for holder to settle (hostspawn.AwaitHolder) within what is
// left of the holder allowance and of the reopen's deadline, the session
// matched as the scan matches it (holder's keys) and no entry answered that
// the scan would not join (Skip: rank) — so a wait that ends at once has an
// entry for the loop to join, or a holder gone or changed: nil once a host
// serves the session, the holder has gone, or (Stale) its entry has changed
// — the loop looks again — the cut's closing,
// or, once the allowance is spent, busy, naming the holder (spent, once the
// deadline is). Only the time spent here is taken from the allowance, and an
// allowance spent already is busy at once: no wait after it, so a host that
// keeps refusing closing — tried again each time its tentative mark's poll
// has passed, which a wait finds before its deadline — is not tried until
// the reopen's deadline.
func (r *reopening) await(holder hostspawn.Holder) *protocol.Error {
	if r.holderLeft <= 0 {
		return r.holderBusy(holder)
	}
	keys := r.holder()
	holder.CrazeSessionID, holder.Provider, holder.ProviderSessionID = keys.CrazeSessionID, keys.Provider, keys.ProviderSessionID
	holder.Skip = func(e rundir.Entry) bool { return r.rank(e) != rankJoin }
	start := reopenClock()
	until := time.Now().Add(max(0, r.holderLeft))
	if r.deadline.Before(until) {
		until = r.deadline
	}
	r.waitStart = start
	_, err := hostspawn.AwaitHolder(r.ctx, r.h.o.Env, holder, until)
	took := reopenClock().Sub(start)
	r.waitStart = time.Time{}
	r.waitedFor += took
	r.holderLeft -= took
	switch {
	case err == nil, errors.Is(err, hostspawn.ErrHolderGone), errors.Is(err, hostspawn.ErrHolderChanged):
		return nil
	case r.ctx.Err() != nil:
		return closingErr()
	case r.over():
		return r.spent().err
	}
	return r.holderBusy(holder)
}

// holderBusy is the answer to a reopen whose holder allowance is spent,
// waiting for holder: busy, naming it.
func (r *reopening) holderBusy(holder hostspawn.Holder) *protocol.Error {
	r.h.logf("%s: %s held the session through the holder allowance (%v): refused", r.tag, holder.Name(), reopenHolderWait)
	why := "has not finished stopping"
	if holder.Stale != nil {
		// Found again as it was once it no longer served the session: not
		// stopping, its entry not yet rewritten for what it serves now.
		why = "is still moving to another session"
	}
	return refused(protocol.CodeUnavailable, protocol.ReasonBusy,
		"that session is held by %s, which %s; try again", holder.Name(), why)
}

// spent is the answer to a reopen whose whole deadline passed before the
// session was running: busy.
func (r *reopening) spent() createAnswer {
	r.h.logf("%s: its %v were spent before the session was running: refused", r.tag, reopenWait)
	return createRefusal(refused(protocol.CodeUnavailable, protocol.ReasonBusy,
		"that session was not running within the reopen's %v; try again", reopenWait))
}

// join is step c: the session's host e, found serving it, joined — dialled
// within the reopen's deadline, attached with no prompt, its start waited for
// within reopenStartWait from the dial's end or until the deadline, whichever
// comes first (startWindow), and the session's row read, naming the session
// the attach was for (sessionRow, exact) — and the answer, joined. Or: a
// holder on its way out (a dial or a connection lost, or an attach refused
// closing, whose entry has since gone or been marked stopping: for good), or
// tentatively so (an attach refused closing, its entry serving the session
// still, unmarked: leaving), for the loop to wait for; or a host that no longer serves the session (an attach refused
// unknown_session, an entry naming another session, a row of another
// session), for the loop to look past. A deadline that passes during it is
// spent. Whatever its start comes to, a joined host is never this hub's to
// stop: no session.stop, no signal, on any branch.
func (r *reopening) join(e rundir.Entry) holderLook {
	h := r.h
	reopenJoining(e)
	dctx, cancel := ctxUntil(r.ctx, r.deadline)
	sess, err := createDial(dctx, e.Socket, e.CrazeSessionID)
	cancel()
	if err != nil {
		h.logf("%s: host %s cannot be dialled: %v", r.tag, e.HostID, err)
		switch leaving, moved := r.whereNow(e); {
		case r.ctx.Err() != nil:
			return answered(createRefusal(closingErr()))
		case r.over():
			return answered(r.spent())
		case leaving:
			return r.surelyLeaving(e)
		case moved:
			return lookPast(e)
		}
		return answered(createRefusal(refused(protocol.CodeUnavailable, protocol.ReasonHostUnreachable,
			"the host serving that session (%s) cannot be reached", e.HostID)))
	}
	deadline, within := startWindow(reopenStartWait, r.deadline)
	at := h.attend(r.ctx, sess, "", deadline, within)
	switch at.kind {
	case attendCut:
		return answered(createRefusal(closingErr()))
	case attendClosing:
		switch leaving, moved := r.whereNow(e); {
		case leaving:
			h.logf("%s: host %s refused the attach closing: waiting for it to go", r.tag, e.HostID)
			return r.surelyLeaving(e)
		case moved:
			return lookPast(e)
		}
		// Its entry serving the session still, unmarked: a stop — a host's
		// from before the mark — or only an idle check's fence, which lifts
		// if the host stays. On its way out tentatively (leaving): tried
		// again once the reopen has waited a poll.
		h.logf("%s: host %s refused the attach closing, its entry unmarked: trying it again in a poll", r.tag, e.HostID)
		r.leaving[e.HostID] = leavingMark{waited: r.waited()}
		holder := hostspawn.HolderOfEntry(e)
		return holderLook{await: &holder}
	case attendStartFailed, attendStartTimeout:
		// Never stopped: it is not this hub's, and another client may be
		// attached to it, or about to be.
		h.logf("%s: host %s: the session did not start: %s; left as it is", r.tag, e.HostID, at.cause)
		if at.kind == attendStartTimeout && r.over() {
			return answered(r.spent())
		}
		return answered(createRefusal(startFailed(at.cause)))
	case attendLost:
		h.logf("%s: host %s went before its session started: %v", r.tag, e.HostID, at.err)
		if r.ctx.Err() != nil {
			return answered(createRefusal(closingErr()))
		}
		var re *remote.Error
		if errors.As(at.err, &re) && re.Code == protocol.CodeUnknownSession {
			return lookPast(e)
		}
		switch leaving, moved := r.whereNow(e); {
		case leaving:
			return r.surelyLeaving(e)
		case moved:
			return lookPast(e)
		}
		return answered(createRefusal(refused(protocol.CodeUnavailable, protocol.ReasonHostUnreachable,
			"the host serving that session (%s) went before the session started", e.HostID)))
	}
	line := hostspawn.ReadyLine{OK: true, HostID: e.HostID, Socket: e.Socket, CrazeSessionID: e.CrazeSessionID}
	rctx, rcancel := ctxUntil(r.ctx, r.deadline)
	row, ok := h.sessionRow(rctx, r.tag, e.HostID, line, e.PID, at.version, true)
	rcancel()
	if !ok {
		return lookPast(e)
	}
	h.logf("%s: host %s serving session %s, started; joined", r.tag, e.HostID, e.CrazeSessionID)
	return answered(createAnswer{reopened: &protocol.ReopenResult{Session: row, Joined: true}})
}

// surelyLeaving is the holderLook of host e found on its way out for good —
// its entry gone or marked stopping since the scan read it — held so
// (leaving), for the loop to wait for it to go.
func (r *reopening) surelyLeaving(e rundir.Entry) holderLook {
	r.leaving[e.HostID] = leavingMark{sure: true}
	holder := leavingHolder(e)
	return holderLook{await: &holder}
}

// whereNow is host e as the registry says it is now: leaving — its entry gone,
// or marked stopping — or moved — its entry naming another session than e
// did (a TUI-hosted session's engine changed). An entry that cannot be read
// says neither.
func (r *reopening) whereNow(e rundir.Entry) (leaving, moved bool) {
	cur, ok, err := hostEntry(r.h.o.Env, e.HostID)
	switch {
	case err != nil:
		return false, false
	case !ok || !rundir.Serving(cur):
		return true, false
	}
	return false, cur.CrazeSessionID != e.CrazeSessionID
}

// spawn is step b: one host spawned to load the session — its ready line read
// within the reopen's deadline, its start within reopenStartWait from its
// dial's end or the deadline, whichever comes first — and the answer; or the
// holder its claim met (held), for the loop to wait for; or the host serving
// another session than it was spawned to load by the time its row is read,
// stopped as a create stops its own (abandonHost) and looked past. A deadline
// that passes during it is spent, the host stopped.
func (r *reopening) spawn() holderLook {
	h := r.h
	t := r.t
	if t.Runnable != nil {
		if err := t.Runnable(); err != nil {
			h.logf("%s: %v: refused", r.tag, err)
			return answered(createRefusal(refused(protocol.CodeNotAccepting, protocol.ReasonNotResumable, "%s", firstLine(err.Error()))))
		}
	}
	if h.cr.stopping() {
		return answered(createRefusal(closingErr()))
	}
	dir, err := rundir.HostLogDir(h.o.Env)
	if err != nil {
		h.logf("%s: the host logs' directory: %v", r.tag, err)
		return answered(createRefusal(spawnFailed("the session host cannot be started: the host logs' directory cannot be used")))
	}
	hostID := rundir.NewHostID()
	spec := hostspawn.Spec{HostID: hostID, Log: filepath.Join(dir, hostID+".log"), Load: t.Load, NoHostStatus: true, HubPID: h.pid,
		Model: t.Model, Effort: t.Effort, NoForce: t.NoForce, Ask: t.Ask, Plan: t.Plan, Unattended: t.Unattended}
	reopenSpawning(&r.deadline)
	lctx, cancel := ctxUntil(r.ctx, r.deadline)
	host, line, perr := h.launchReady(lctx, r.tag, spec, t.Workspace, "session "+t.Load)
	cancel()
	if perr != nil {
		if r.ctx.Err() == nil && r.over() {
			// Its ready line was not read by the deadline: launchReady ended
			// the host as one it gave up on.
			return answered(r.spent())
		}
		return answered(createRefusal(perr))
	}
	defer h.cr.launches.Done()
	if !line.OK {
		// The host said why itself and is exiting: the grace to, and ended
		// if it takes longer. A holder it names is never touched.
		host.settle()
		why := firstLine(strings.TrimPrefix(strings.TrimPrefix(line.Error, "craze serve: "), "craze: "))
		switch {
		case line.Held != nil:
			holder := hostspawn.HolderOfHeld(line.Held)
			if r.t.CrazeID == "" {
				// A legacy row's host gave it its craze id under its claim:
				// the scans from now on match the session by it.
				r.t.CrazeID = line.Held.CrazeSessionID
			}
			holder.Provider, holder.ProviderSessionID = r.t.Provider, r.t.ProviderSessionID
			h.logf("%s: the session is held by %s: waiting for it to serve the session, or to go", r.tag, holder.Name())
			return holderLook{await: &holder}
		case line.Refused && hostspawn.TransientRefusal(line.Error):
			h.logf("%s: host %s: %s", r.tag, hostID, line.Error)
			return answered(createRefusal(refused(protocol.CodeUnavailable, protocol.ReasonBusy, "%s", why)))
		case line.Refused:
			h.logf("%s: host %s refused the session: %s", r.tag, hostID, line.Error)
			return answered(createRefusal(refused(protocol.CodeNotAccepting, protocol.ReasonNotResumable, "%s", why)))
		}
		h.logf("%s: host %s could not start: %s", r.tag, hostID, line.Error)
		return answered(createRefusal(spawnFailed("the session host could not start: %s", why)))
	}
	if line.HostID != hostID {
		host.terminate()
		h.logf("%s: host %s's ready line names host %q", r.tag, hostID, line.HostID)
		return answered(createRefusal(spawnFailed("the session host answered for another host")))
	}
	at, perr := h.startSpawned(r.ctx, r.tag, host, line, "", reopenStartWait, r.deadline)
	if perr != nil {
		if r.ctx.Err() == nil && r.over() {
			// Given up on at the deadline — its dial, or its start cut to
			// what was left — and stopped.
			return answered(r.spent())
		}
		return answered(createRefusal(perr))
	}
	rctx, rcancel := ctxUntil(r.ctx, r.deadline)
	row, ok := h.sessionRow(rctx, r.tag, hostID, line, host.child.PID(), at.version, true)
	rcancel()
	if !ok {
		// The reopen's own host, serving another session: stopped before it
		// is let go of, as every host of its own it does not answer with.
		h.abandonHost(host, line.Socket, line.CrazeSessionID)
		return lookPast(rundir.Entry{HostID: hostID})
	}
	h.logf("%s: host %s serving session %s, started", r.tag, hostID, line.CrazeSessionID)
	return answered(createAnswer{reopened: &protocol.ReopenResult{Session: row}})
}

// ------------------------------------------------------------ the client

// ReopenNeed is what a hub must say in its hello to take a reopen (Ensure's
// need, and Reopen's own check): reopen (plan 046 B1).
func ReopenNeed() protocol.ConnectionCapabilities {
	return protocol.ConnectionCapabilities{Reopen: true}
}

// Reopen asks the hub at socket to ensure the saved session id is running —
// hello as who, then session.reopen — and answers the result as the hub wrote
// it, not re-encoded (a protocol.ReopenResult), within ctx, with the hello of
// the hub that took the reopen's own connection (plan 046 U2: the hub whose
// refusal a client explains, as CreateHello's). A refusal is an error
// wrapping the hub's *protocol.Error; a connection that ends before the
// answer, one wrapping ErrUnanswered — the reopen may have run, and sending
// the same id again is safe (retry by session); a hub whose hello does not
// say reopen, a *LacksError, nothing asked. The hello is the zero value when
// the connection never got one.
func Reopen(ctx context.Context, socket string, who protocol.ClientInfo, id string) (json.RawMessage, protocol.HubHelloResult, error) {
	hc, hello, err := openHub(ctx, socket, who)
	if err != nil {
		return nil, protocol.HubHelloResult{}, err
	}
	defer hc.close()
	if missing := missing(hello.Capabilities, ReopenNeed()); len(missing) > 0 {
		return nil, hello, lacking(hello.Endpoint, missing)
	}
	res, err := hc.call(protocol.MethodSessionReopen, protocol.ReopenParams{ID: id})
	if err != nil {
		return nil, hello, err
	}
	var check protocol.ReopenResult
	if err := json.Unmarshal(res, &check); err != nil {
		return nil, hello, fmt.Errorf("hub: %s's result: %w", protocol.MethodSessionReopen, err)
	}
	return res, hello, nil
}
