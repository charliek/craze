package cli

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/tui"
)

// The idle watcher (plan 030 §3.6): a detached host's lifecycle goroutine
// beside its coordinator (hostLifecycle), which asks it for the stop sequence
// when the host has had, for the whole of `host_idle_exit`, no client
// attached and nothing in flight — or when the host can no longer be reached.
//
// Each tick (hostIdleTick) it looks, in order:
//
//  1. Socket lost (SF-66, narrowly): the host's socket file and its registry
//     entry are still the files it made (rundir.Host.Lost, by dev/ino). One
//     that has vanished — /run/user removed at the last logout, a registry
//     swept by hand — is a stop, the ordinary one, so the session is saved
//     and resumable rather than left running where no client can find it.
//  2. Readiness: nothing is timed before the session's start has run.
//  3. The startup grace: the clock starts only once a client has attached or
//     hostStartupGrace has passed since the host started, so a host spawned
//     for a client that has not dialled yet is not reaped first.
//  4. Attached and in flight: a client attached (an attachments listener,
//     control.Server.AddAttachmentsListener — which never counts a list
//     poller, nor a peer that has half-closed) or
//     anything in flight (engine.Engine.Busy) restarts the clock, and so does
//     a turn that ended since it last looked (State.LastTurn): a sample's gap
//     does not hide a turn that began and ended between two ticks.
//  5. The limit: host_idle_exit (tui.ConfigHostIdleExit), or for a host started
//     --unattended host_idle_exit_unattended, fixed as it starts; min(that,
//     hostNeverPromptedIdle) for a session that has had no prompt — it has
//     nothing to resume; 0 for one whose start failed — it has nothing at
//     all, and the grace above is what let its launcher attach and be told;
//     and none for "never", which the start-failed rule outranks. A session
//     under its exit fence (engine.Engine.SubmitExitAfterTurn), whose waiter may
//     be gone, has at most host_idle_exit_unattended, whichever host it is on.
//  6. The decision, once the clock has run out, is atomic (R2-1): the host's
//     close fence (control.Server.FenceClose) — its attach fence, then its
//     engine's — and, under both, the count of attachments and whether
//     anything is in flight. None and nothing: the stop, both fences left up.
//     Anything: both released, and the clock starts again.
//
// An open ask is in flight, so it pins its host for as long as it is open (the
// owner's rule) — but for an unattended host's, and one under its exit fence,
// which nobody is there to answer: its clock restarts at the first look that sees
// the ask (seeAsk; engine.Options.AsksDoNotPin; asks have no timeout of their own).
// A stopped or expired session that received a prompt is in the
// session index already (the engine writes it); one that never did leaves
// nothing there.

// The idle watcher's bounds: variables only so that tests can shorten them
// (never in parallel).
var (
	// hostIdleTick is how often the watcher looks.
	hostIdleTick = time.Second
	// hostStartupGrace is how long after the host starts its clock waits for
	// a first attach.
	hostStartupGrace = 60 * time.Second
	// hostNeverPromptedIdle caps host_idle_exit for a session that has had no
	// prompt.
	hostNeverPromptedIdle = 5 * time.Minute
)

// idleTicks is where a host's idle watcher gets its ticks and its clock: a
// time.Ticker at hostIdleTick and time.Now in production, a test's own
// channel and clock where a schedule is forced. stop ends the ticks.
var idleTicks = func() (ticks <-chan time.Time, now func() time.Time, stop func()) {
	t := time.NewTicker(hostIdleTick)
	return t.C, time.Now, t.Stop
}

// idleLooked is told what each of the watcher's ticks came to, once it has
// acted on it: a seam for the idle watcher's tests, a no-op in production.
var idleLooked = func(idleLook) {}

// idleLook is one tick of the watcher's, as it decided.
type idleLook struct {
	// Armed says the clock is running: the session started, and the startup
	// grace over.
	Armed bool
	// Idle is how long the host has had nobody attached and nothing in
	// flight, and Limit how long it may; Timed is false for "never".
	Idle, Limit time.Duration
	Timed       bool
	// Fenced says the clock ran out and the close fence was raised; Stop is
	// why the host stops, "" when it stays, and Cause what began it: the idle
	// exit, or the socket lost (agent.TurnInfo.Cause).
	Fenced bool
	Stop   string
	Cause  string
}

// idleEngine is what the watcher reads of the engine: an interface only so
// that the watcher's schedules can be driven over a session a test holds.
type idleEngine interface {
	Ready() <-chan struct{}
	State() engine.State
	Busy() bool
}

// idleServer is what the watcher asks of the control server.
type idleServer interface {
	AddAttachmentsListener(func(int)) (remove func())
	FenceClose() (attached int, busy bool, release func())
}

// idleWatcher is one host's idle watcher. Everything below mu is written by
// its attachments listener on the server; the rest is the watcher's
// goroutine's own.
type idleWatcher struct {
	srv    idleServer
	eng    idleEngine
	lost   func() string
	policy tui.HostIdleExit
	// fenced is host_idle_exit_unattended, whichever host this is: an exit
	// fence's limit.
	fenced tui.HostIdleExit
	now    func() time.Time
	start  time.Time

	mu sync.Mutex
	// attached is the server's count, as last reported; ever says one was
	// ever attached; left is when the count last fell to 0.
	attached int
	ever     bool
	left     time.Time

	armed     bool
	idleSince time.Time
	// askSeen is the open ask this clock last restarted for, "" for none: an ask
	// that does not pin the host gets the whole limit from the first look at it.
	askSeen string
}

var _ idleServer = (*control.Server)(nil)
var _ idleEngine = (*engine.Engine)(nil)

// newIdleWatcher builds a host's watcher, its clock at now, and has the server
// report its attachments to it from here on. unattended is the host's --unattended,
// which picks its limit once and for all. log takes the one line a limit craze
// cannot read deserves.
func newIdleWatcher(srv idleServer, eng idleEngine, lost func() string, now func() time.Time, unattended bool, log io.Writer) *idleWatcher {
	policy, why := tui.ConfigHostIdleExit()
	fenced, fencedWhy := tui.ConfigHostIdleExitUnattended()
	if unattended {
		policy, why = fenced, fencedWhy
	} else if fencedWhy != "" && fencedWhy != why {
		fmt.Fprintf(log, "craze serve: %s; the limit is the default, %s\n", fencedWhy, fenced.After)
	}
	if why != "" {
		fmt.Fprintf(log, "craze serve: %s; the idle exit is the default, %s\n", why, policy.After)
	}
	w := &idleWatcher{srv: srv, eng: eng, lost: lost, policy: policy, fenced: fenced, now: now, start: now()}
	// For the host's life: the watcher is never removed, and the listener
	// beside it — none on a detached host — hears every change too.
	srv.AddAttachmentsListener(w.attachments)
	return w
}

// attachments is the watcher's attachments listener
// (control.Server.AddAttachmentsListener): called in order, under the
// server's locks, so it only records.
func (w *idleWatcher) attachments(n int) {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.attached = n
	if n > 0 {
		w.ever = true
	} else {
		w.left = now
	}
}

// run looks at every tick until the host is stopping, and asks for the stop
// (request) the first time a look says so.
func (w *idleWatcher) run(ticks <-chan time.Time, stopping <-chan struct{}, request func(cause, why string)) {
	for {
		select {
		case <-stopping:
			return
		case <-ticks:
		}
		look := w.look()
		idleLooked(look)
		if look.Stop != "" {
			request(look.Cause, look.Stop)
			return
		}
	}
}

// look is one tick (the file's doc comment, steps 1–6).
func (w *idleWatcher) look() idleLook {
	if why := w.lost(); why != "" {
		return idleLook{Stop: why, Cause: agent.StopCauseLostOwner}
	}
	select {
	case <-w.eng.Ready():
	default:
		return idleLook{}
	}
	now := w.now()
	w.mu.Lock()
	attached, ever, left := w.attached, w.ever, w.left
	w.mu.Unlock()
	if !w.armed {
		if !ever && now.Sub(w.start) < hostStartupGrace {
			return idleLook{}
		}
		w.armed, w.idleSince = true, now
	}
	if attached > 0 || w.eng.Busy() {
		w.idleSince = now
		return idleLook{Armed: true}
	}
	// Read after the sample: an ask that made it read idle is in this state.
	st := w.eng.State()
	w.seeAsk(st, now)
	if left.After(w.idleSince) {
		w.idleSince = left
	}
	if lt := st.LastTurn; lt != nil && lt.EndedAt.After(w.idleSince) && !lt.EndedAt.After(now) {
		w.idleSince = lt.EndedAt
	}
	look := idleLook{Armed: true, Idle: now.Sub(w.idleSince)}
	look.Limit, look.Timed = w.limit(st)
	if !look.Timed || look.Idle < look.Limit {
		return look
	}
	look.Fenced = true
	n, busy, release := w.srv.FenceClose()
	if n == 0 && !busy && !w.askSince() {
		look.Stop, look.Cause = w.why(st, look.Idle), agent.StopCauseIdle
		return look
	}
	release()
	w.idleSince = w.now()
	return look
}

// seeAsk restarts the clock for an open ask it has not seen. Only an ask that does
// not pin the host reaches here: one that does reads busy.
func (w *idleWatcher) seeAsk(st engine.State, now time.Time) {
	switch {
	case st.PendingAsks == 0:
		w.askSeen = ""
	case st.HeadAsk.ID != w.askSeen:
		w.askSeen, w.idleSince = st.HeadAsk.ID, now
	}
}

// askSince says, with the close fence up, that an ask this look did not count has
// opened since: a turn that started and asked between its sample and the fence.
func (w *idleWatcher) askSince() bool {
	st := w.eng.State()
	return st.PendingAsks > 0 && st.HeadAsk.ID != w.askSeen
}

// limit is how long this session may be idle, and false for never.
func (w *idleWatcher) limit(st engine.State) (time.Duration, bool) {
	switch {
	case st.StartFailed:
		return 0, true
	case st.ExitFenced && !w.fenced.Never:
		if w.policy.Never {
			return w.fenced.After, true
		}
		return min(w.policy.After, w.fenced.After), true
	case w.policy.Never:
		return 0, false
	case !st.Prompted:
		return min(w.policy.After, hostNeverPromptedIdle), true
	}
	return w.policy.After, true
}

// why is the stop's cause, for the host's log and its journal.
func (w *idleWatcher) why(st engine.State, idle time.Duration) string {
	switch {
	case st.StartFailed:
		return "its session did not start and no client is attached"
	case !st.Prompted:
		return fmt.Sprintf("idle for %s with no client attached, never prompted", idle.Round(time.Millisecond))
	case st.PendingAsks > 0:
		return fmt.Sprintf("idle for %s with no client attached and an ask nobody answered", idle.Round(time.Millisecond))
	}
	return fmt.Sprintf("idle for %s with no client attached", idle.Round(time.Millisecond))
}
