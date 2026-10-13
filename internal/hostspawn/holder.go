package hostspawn

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charliek/craze/internal/rundir"
)

// A session's holder, and the wait for it to settle (plan 046 B3). One host
// serves a session: the one holding its claim (rundir.ClaimSession) — but for
// a TUI-hosted session whose claim could not take its lock file, which runs
// unclaimed (internal/cli's sessionClaims).
// A spawner that wants the session running meets that host one of two ways —
// its registry entry, found serving the session, or a held answer from a host
// it spawned, which names the claim's holder — and either way the holder may
// be on its way out. A stopping host marks its entry first (Entry.Stopping,
// plan 046 B4), unlinks it a moment later, and releases its claim only after
// that (craze serve's stop sequence: the start's join comes between, up to
// seconds, and a join that times out keeps the claim until the process
// exits) — so neither the entry's mark nor its absence says the session can
// be claimed again. The holder's process having gone does (the kernel drops
// a dead process's locks). AwaitHolder waits for exactly that, or for a host
// that serves the session — the holder, or whoever claimed it once the holder
// let go — whichever comes first.

// Holder is the host holding a session, as far as its waiter knows it: the
// session, by its craze id or, for an index row from before craze ids, its
// provider and provider session id (each "" when unknown); and the holder by
// its host id, its pid and its pid's start (rundir.ProcIdentity.Start) — each
// zero when unknown: a held answer's lock names a holder's host id and pid,
// and nothing of the holder at all in the instant between its claim's flock
// and its line (ReadyHeld).
//
// Leaving says the holder itself is known to be on its way out — its entry
// marked stopping, or its attach refused closing (a host from before the
// mark, which never writes one) — so its own entry is never AwaitHolder's
// answer, marked or not: only its going, or another host serving the
// session, ends the wait.
//
// Stale, when set, is the holder's own registry entry as its waiter read it,
// naming the session while its host was found not to serve it (a TUI-hosted
// session's engine changed, its entry's rewrite not landed yet): that entry,
// while it reads the same (SameEngine), is never AwaitHolder's answer, and
// its changing — rewritten, marked stopping, or gone — ends the wait
// (ErrHolderChanged), unless it then serves the session (the answer).
//
// Skip, when set, says an entry is not one the waiter would take: such an
// entry is never AwaitHolder's answer, so an entry the wait answers is one
// its waiter acts on, never one it would wait on again at once (a reopen's:
// a host it has looked past whose entry is as it was, a host it knows is
// leaving).
type Holder struct {
	CrazeSessionID              string
	Provider, ProviderSessionID string
	HostID                      string
	PID                         int
	PIDStart                    uint64
	Leaving                     bool
	Stale                       *rundir.Entry
	Skip                        func(rundir.Entry) bool
}

// SameEngine says a and b, two reads of one host's registry entry, describe
// the same engine as it was: every member a rewrite of the engine's identity
// writes — its craze id, incarnation, provider, provider session id and
// readiness — and its stopping mark, alike.
func SameEngine(a, b rundir.Entry) bool {
	return a.HostID == b.HostID && a.CrazeSessionID == b.CrazeSessionID && a.Incarnation == b.Incarnation &&
		a.Provider == b.Provider && a.ProviderSessionID == b.ProviderSessionID && a.Ready == b.Ready && a.Stopping == b.Stopping
}

// HolderOfEntry is the holder a registry entry names: its session, its host
// and its process.
func HolderOfEntry(e rundir.Entry) Holder {
	return Holder{CrazeSessionID: e.CrazeSessionID, Provider: e.Provider, ProviderSessionID: e.ProviderSessionID,
		HostID: e.HostID, PID: e.PID, PIDStart: e.PIDStart}
}

// HolderOfHeld is the holder a held answer names: the session by its craze
// id, the holder by the host id and pid its lock holds (zero for none).
func HolderOfHeld(held *ReadyHeld) Holder {
	return Holder{CrazeSessionID: held.CrazeSessionID, HostID: held.HostID, PID: held.PID}
}

// Serves says e is a registry entry of a host serving h's session — by its
// craze id, or by its provider and provider session id — whatever its mark:
// a stopping host still serves it until it has gone (rundir.Serving says
// whether a roster lists it). An entry that names no session yet (between
// its bind and its identity) serves none. The provider key is best effort: a
// host's entry carries its provider session id only once its engine is
// ready.
func (h Holder) Serves(e rundir.Entry) bool {
	if e.CrazeSessionID == "" {
		return false
	}
	if h.CrazeSessionID != "" && e.CrazeSessionID == h.CrazeSessionID {
		return true
	}
	return h.ProviderSessionID != "" && e.ProviderSessionID == h.ProviderSessionID && e.Provider == h.Provider
}

// Gone reports whether the holder's process has exited: no process has its
// pid, or the one that has it is a zombie (its locks are released as it
// exits) or — its start known — started at another instant than the holder
// did (its pid reused). A holder with no pid, and one whose identity cannot
// be read, are not gone: nothing says they are.
func (h Holder) Gone() bool {
	if h.PID <= 0 {
		return false
	}
	id, err := rundir.ProcessIdentity(h.PID)
	switch {
	case errors.Is(err, rundir.ErrNoProcess):
		return true
	case err != nil:
		return false
	}
	return id.Zombie || (h.PIDStart != 0 && id.Start != h.PIDStart)
}

// Name is the holder as a refusal names it: "host <id> (pid <pid>)", either
// part left out when unknown, "a host" when both are.
func (h Holder) Name() string {
	var b strings.Builder
	if h.HostID != "" {
		b.WriteString("host " + h.HostID)
	}
	if h.PID > 0 {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("(pid " + strconv.Itoa(h.PID) + ")")
	}
	if b.Len() == 0 {
		return "a host"
	}
	return b.String()
}

// AwaitHolder's answers other than an entry serving the session, and its
// caller's context's error.
var (
	// ErrHolderStill is a holder still there at the wait's deadline: still
	// stopping, or alive with no entry serving the session (its socket never
	// bound, or its stop between its entry's unlink and its claim's release
	// taking that long).
	ErrHolderStill = errors.New("the session's holder is still there")
	// ErrHolderChanged is a Stale holder's entry changed — rewritten, marked
	// stopping, or gone — with no entry serving the session: its waiter looks
	// again.
	ErrHolderChanged = errors.New("the session's holder's entry has changed")
)

// AwaitPolled is told each read of the registry AwaitHolder makes: a seam
// that lets a test act while the wait is known to be waiting; a no-op in
// production.
var AwaitPolled = func() {}

// unnamedHolderWait bounds AwaitHolder's wait for a holder it cannot name — a
// held answer read in the instant between the holder's flock and its line,
// with no pid to watch — before it answers ErrHolderGone, so its caller
// spawns again and is answered a holder it can watch (or claims the session
// itself). A variable only so a test can shorten it (never in parallel).
var unnamedHolderWait = 500 * time.Millisecond

// AwaitHolder waits for a session's holder h to settle (the file's comment),
// reading the registry every rendezvousPoll, until deadline:
//
//   - a live host's entry serving the session and not marked stopping
//     (rundir.Serving) — the holder's, unless it is Leaving, or that of
//     whoever claimed the session once it let go — and not one Skip says the
//     caller would not take, is the answer: the caller joins it;
//   - the holder's process gone (Holder.Gone) with no such entry is
//     ErrHolderGone: the claim is free, and the caller may spawn to take it;
//   - a holder it cannot name (no pid) that no serving entry has shown for
//     unnamedHolderWait is ErrHolderGone too: its caller asks again;
//   - a Stale holder's own entry no longer as it was read (SameEngine) —
//     rewritten, marked stopping, or gone — and none serving the session is
//     ErrHolderChanged: its caller looks again;
//   - the deadline passing first is ErrHolderStill; ctx ending first, ctx's
//     error.
//
// An entry marked stopping is never the answer: a host on its way out is
// waited out. A holder named with no start (a held answer's: its lock names
// a pid alone) takes its start from its own entry — the same host id and pid
// — the first poll that reads one, so that its pid reused once it has gone
// does not keep the wait alive. A registry read that fails is retried at the
// next poll, and says nothing of a Stale holder's entry; nor does a read of
// the registry that finds that entry unreadable. Every answer but an entry
// serving the session follows a change in the world — a process gone, an
// entry changed — or the deadline: else the wait sleeps a poll. It never
// signals anything, and never claims the session.
func AwaitHolder(ctx context.Context, env rundir.Env, h Holder, deadline time.Time) (rundir.Entry, error) {
	unnamed := time.Now().Add(unnamedHolderWait)
	for {
		AwaitPolled()
		entries, unreadable, err := rundir.HostsScan(env)
		var found rundir.Entry
		ok := false
		// changed says a Stale holder's own entry is no longer as it was
		// read: until a read finds it as it was, or cannot read it, it has
		// changed.
		changed := h.Stale != nil && err == nil && !slices.Contains(unreadable, h.Stale.HostID)
		for _, e := range entries {
			if h.PIDStart == 0 && h.PID > 0 && h.HostID != "" && e.HostID == h.HostID && e.PID == h.PID {
				// The holder's own entry, stopping or not, says when its
				// process started: from now on a later process given its pid
				// is not taken for it (Gone).
				h.PIDStart = e.PIDStart
			}
			if h.Leaving && h.HostID != "" && e.HostID == h.HostID {
				continue
			}
			if h.Stale != nil && e.HostID == h.Stale.HostID && SameEngine(*h.Stale, e) {
				changed = false
				continue
			}
			if h.Skip != nil && h.Skip(e) {
				continue
			}
			if h.Serves(e) && rundir.Serving(e) && (!ok || e.StartedAt.After(found.StartedAt)) {
				found, ok = e, true
			}
		}
		switch {
		case ok:
			return found, nil
		case h.Gone():
			return rundir.Entry{}, ErrHolderGone
		case changed:
			return rundir.Entry{}, ErrHolderChanged
		case h.PID <= 0 && !time.Now().Before(unnamed):
			return rundir.Entry{}, ErrHolderGone
		case !time.Now().Before(deadline):
			return rundir.Entry{}, ErrHolderStill
		}
		t := time.NewTimer(min(rendezvousPoll, time.Until(deadline)))
		select {
		case <-ctx.Done():
			t.Stop()
			return rundir.Entry{}, ctx.Err()
		case <-t.C:
		}
	}
}

// The words of a host's transient refusals of a load (craze serve's ready
// line, Refused): the session index locked by another writer, or changed
// between the read of a row and its claim. Each is a claim to try again,
// not a refusal of the session: a spawner that tells them apart
// (TransientRefusal) answers "busy", never "cannot be resumed". internal/cli's
// refusal words them, so the two cannot drift.
const (
	IndexBusy    = "the session index is busy — try again"
	IndexChanged = "the session index changed — try again"
)

// TransientRefusal says a not-ok ready line's error is one of a host's
// transient refusals (IndexBusy, IndexChanged): try again.
func TransientRefusal(msg string) bool {
	return strings.Contains(msg, IndexBusy) || strings.Contains(msg, IndexChanged)
}
