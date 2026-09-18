package host

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Reporter delivers statuses to one host.
//
// Both calls MUST honour ctx, and a reporter checks ctx.Err() before each
// wire line it writes, not only before the first. Every Report context is
// derived from the Hub's base context, which Close cancels before anything
// else, so a Report that Close interrupts between its state line and a
// metadata line writes nothing more and returns; its worker calls Release
// only after that return. Together that is how Release stays the last wire
// write per reporter (plan 015 §3.2). So a line with its own shorter
// deadline derives that context from ctx, and nothing is written from a
// goroutine that outlives the call.
type Reporter interface {
	// Name labels the reporter in the one warning its failures produce.
	Name() string
	// Report sends s. seq is strictly increasing across every call on this
	// Hub and never below the µs clock at publish time.
	Report(ctx context.Context, s Status, seq uint64) error
	// Release hands the pane or tab back. It is called at most once, at
	// Close, only after a Report was attempted — even one that panicked —
	// with a seq above every seq Report was given.
	Release(ctx context.Context, seq uint64) error
}

// Timer is the idle debounce timer, injectable so a test fires it by hand.
// It has time.Timer's shape: one value on C when it fires, and Stop to cancel.
// The Hub never relies on Stop to suppress a value — a held Idle is matched
// against the timer it was started with — so a fake need not either.
//
// Both methods are only ever called with the Hub mutex held (Publish, Close),
// so they must not block and must not call back into the Hub.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// HubOptions tunes a Hub. Zero values take the defaults beside each field.
type HubOptions struct {
	// Now and NewTimer run under the Hub mutex, inside Publish and Close, so
	// neither may block or call back into the Hub; Publish's promise never to
	// block the TUI rests on it. The defaults, time.Now and a time.Timer
	// adapter, do neither.
	Now          func() time.Time          // seq floor; time.Now
	NewTimer     func(time.Duration) Timer // idle debounce; a time.Timer
	IdleDelay    time.Duration             // 250 ms
	SendTimeout  time.Duration             // 500 ms per Report and per Release
	CloseTimeout time.Duration             // 1 s for the whole Close
	// Warn must not block; the CLI passes a buffered stderr writer. Close
	// waits for a Warn already in progress, so that nothing lands after the
	// caller flushes that buffer, which means a Warn that blocks holds Close
	// past its budget.
	Warn func(string) // may be nil
}

const (
	defaultIdleDelay   = 250 * time.Millisecond
	defaultSendTimeout = 500 * time.Millisecond
)

// DefaultCloseTimeout is Close's whole budget when HubOptions leaves it unset.
// It is exported so the caller's own bound on Close is the same second.
const DefaultCloseTimeout = time.Second

// queueCap bounds each reporter's backlog. A reporter slower than the TUI's
// transitions loses the least informative entries first (see push).
const queueCap = 8

// Hub fans statuses out to one worker goroutine per reporter. Publish never
// blocks the TUI: it only mutates the per-reporter queues under mu and nudges
// the workers without waiting. The workers do every send.
type Hub struct {
	opts    HubOptions
	workers []*worker

	// base is every Report context's parent; Close cancels it first, so an
	// in-flight send stops before its worker releases.
	base   context.Context
	cancel context.CancelFunc
	// closeDone is closed when the first Close returns; a later Close waits
	// on it, so no Close returns while a release could still warn.
	closeDone chan struct{}

	// mu guards these fields and each worker's shared ones. Nothing that can
	// block — a send, Warn — runs under it.
	mu         sync.Mutex
	closed     bool
	published  bool
	last       Status
	seq        uint64
	releaseSeq uint64

	// warnMu serialises Warn (the CLI's writer is a plain io.Writer) and
	// guards silenced, which Close sets on its way out so nothing is written
	// after the caller flushes its stderr buffer. It is never taken with mu
	// held, so a slow Warn cannot stall Publish.
	warnMu   sync.Mutex
	silenced bool
}

type entry struct {
	status Status
	seq    uint64
}

// worker is one reporter's goroutine state. queue, held, timer, timerC and
// dead are shared with Publish and Close and guarded by Hub.mu; name,
// attempted and warned are touched by the worker goroutine alone.
type worker struct {
	r    Reporter
	wake chan struct{} // cap 1: a pending nudge, never a count
	done chan struct{} // closed when the goroutine exits

	queue  []entry
	held   entry // the debounced Idle, meaningful while timerC != nil
	timer  Timer
	timerC <-chan time.Time
	// dead is set when the reporter panicked: Publish skips it, and its
	// worker only waits for Close to release.
	dead bool

	name      string
	attempted bool // a Report was called, so the host may hold our state
	warned    bool
}

// NewHub starts one worker per reporter. The caller must Close it.
func NewHub(rs []Reporter, o HubOptions) *Hub {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewTimer == nil {
		o.NewTimer = newRealTimer
	}
	if o.IdleDelay <= 0 {
		o.IdleDelay = defaultIdleDelay
	}
	if o.SendTimeout <= 0 {
		o.SendTimeout = defaultSendTimeout
	}
	if o.CloseTimeout <= 0 {
		o.CloseTimeout = DefaultCloseTimeout
	}
	h := &Hub{opts: o, closeDone: make(chan struct{})}
	h.base, h.cancel = context.WithCancel(context.Background())
	for _, r := range rs {
		w := &worker{r: r, wake: make(chan struct{}, 1), done: make(chan struct{})}
		h.workers = append(h.workers, w)
		go h.run(w)
	}
	return h
}

// Publish hands s to every reporter. It never blocks: a status identical to
// the last one is dropped, an Idle is held for IdleDelay so a queue drain's
// momentary idle never reaches a host (plan 015 §3.2), and anything else is
// queued for the worker, which is woken without waiting for it.
func (h *Hub) Publish(s Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	if h.published && s == h.last {
		return
	}
	h.published, h.last = true, s
	e := entry{status: s, seq: h.nextSeqLocked()}
	for _, w := range h.workers {
		if w.dead {
			continue
		}
		// Either way a held Idle is superseded: a new Idle restarts the delay
		// with its own seq, and anything else cancels it.
		w.stopTimerLocked()
		if s.Kind == Idle {
			w.held = e
			w.timer = h.opts.NewTimer(h.opts.IdleDelay)
			w.timerC = w.timer.C()
		} else {
			w.push(e)
		}
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

// Close stops the Hub and releases every reporter that may hold state, in
// parallel, bounded by the earlier of ctx's deadline and CloseTimeout. A worker
// still stuck when the budget is spent is abandoned: it may still release once
// its send returns, but it never calls Warn again.
//
// It is idempotent: a later call does no work of its own. It waits for the
// first call to return, or for its own ctx, so that no Close returns while a
// release still in progress could warn after it.
func (h *Hub) Close(ctx context.Context) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		select {
		case <-h.closeDone:
		case <-ctx.Done():
		}
		return
	}
	defer close(h.closeDone)
	h.closed = true
	// Stamped from the same counter after the last Publish, so it is above
	// every seq any Report was given: herdr ignores a release at or below the
	// last report seq and would leave the pane sticky (plan 015 §2.1).
	h.releaseSeq = h.nextSeqLocked()
	for _, w := range h.workers {
		w.queue = nil
		w.stopTimerLocked()
	}
	h.mu.Unlock()
	h.cancel()

	// The workers release concurrently, so the budget is spent on the
	// slowest of them, not on their sum.
	join, cancel := context.WithTimeout(ctx, h.opts.CloseTimeout)
	defer cancel()
	h.join(join)

	h.warnMu.Lock()
	h.silenced = true
	h.warnMu.Unlock()
}

// join waits for every worker to exit, or for ctx.
func (h *Hub) join(ctx context.Context) {
	for _, w := range h.workers {
		select {
		case <-w.done:
		case <-ctx.Done():
			return
		}
	}
}

// nextSeqLocked is max(prev+1, unix µs): monotonic within the Hub, and never
// below a predecessor craze's seqs in the same pane, because herdr keeps one
// seq counter per source across processes (plan 015 §3.2).
func (h *Hub) nextSeqLocked() uint64 {
	seq := h.seq + 1
	if us := h.opts.Now().UnixMicro(); us > 0 && uint64(us) > seq {
		seq = uint64(us)
	}
	h.seq = seq
	return seq
}

// run is one reporter's goroutine: it serves the queue until Close, then
// releases. A panic in the reporter ends the serving, never the TUI, but not
// the release: a Report that panicked may already have written host state, and
// herdr leaves a pane sticky until it is released (plan 015 §2.1).
func (h *Hub) run(w *worker) {
	defer close(w.done)
	h.guard(w, func() { h.serve(w) })
	// serve returns once it has seen Close; after a panic this is where the
	// dead worker waits for it.
	<-h.base.Done()
	h.mu.Lock()
	seq := h.releaseSeq
	h.mu.Unlock()
	h.guard(w, func() { h.release(w, seq) })
}

// guard runs fn, recovering a panic: the reporter is marked dead, so Publish
// stops feeding it, and the panic is warned. A panic is a craze bug, so it is
// warned even when an earlier failure already was — a socket error must not
// hide it.
func (h *Hub) guard(w *worker, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			h.mu.Lock()
			w.dead = true
			w.queue = nil
			w.stopTimerLocked()
			h.mu.Unlock()
			h.warn(w, fmt.Sprintf("panic: %v", p))
		}
	}()
	fn()
}

// serve sends the queue until Close.
func (h *Hub) serve(w *worker) {
	w.name = w.r.Name()
	for {
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			return
		}
		if len(w.queue) > 0 {
			e := w.queue[0]
			w.queue = w.queue[1:]
			h.mu.Unlock()
			h.report(w, e)
			continue
		}
		// Only an idle worker watches the debounce timer, so a held Idle is
		// appended between sends: a status published while a send is still
		// in flight cancels it even if its delay has run out, which is the
		// debounce doing its job — that Idle was already superseded.
		tc := w.timerC
		h.mu.Unlock()

		select {
		case <-w.wake:
		case <-h.base.Done():
		case <-tc: // nil, and so never ready, while no Idle is held
			h.mu.Lock()
			// Matched by channel: a timer Publish or Close replaced or
			// cancelled may still deliver, and must not append an Idle that
			// is gone.
			if w.timerC == tc {
				w.timer, w.timerC = nil, nil
				w.push(w.held)
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) report(w *worker, e entry) {
	w.attempted = true
	ctx, cancel := context.WithTimeout(h.base, h.opts.SendTimeout)
	defer cancel()
	err := w.r.Report(ctx, e.status, e.seq)
	// A send Close cancelled is craze exiting, not the host failing; the
	// Release that follows still warns if the host really is unreachable.
	if err != nil && h.base.Err() == nil {
		h.fail(w, err)
	}
}

// release runs once Close has been seen, which is after the in-flight Report
// (if any) returned. Its context is fresh, not derived from the cancelled
// base, and a reporter that never attempted a Report has nothing to hand back.
func (h *Hub) release(w *worker, seq uint64) {
	if !w.attempted {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.opts.SendTimeout)
	defer cancel()
	err := w.r.Release(ctx, seq)
	if err != nil {
		h.fail(w, err)
	}
}

// fail warns on a reporter's first failure only. Retries happen when the next
// status is published, so a dead socket costs one line, not one per turn.
func (h *Hub) fail(w *worker, err error) {
	if w.warned {
		return
	}
	w.warned = true
	h.warn(w, err.Error())
}

func (h *Hub) warn(w *worker, msg string) {
	h.warnMu.Lock()
	defer h.warnMu.Unlock()
	if h.silenced || h.opts.Warn == nil {
		return
	}
	h.opts.Warn("host status: " + w.name + ": " + msg)
}

// push appends e under the queue invariant (plan 015 §3.2): entries stay in
// seq order; consecutive entries of one Kind collapse to the newest, so the
// newest message wins within a kind; different kinds never merge, so a
// Blocked is never lost behind a later Working. A full queue first drops its
// oldest Working or Idle — the states the next status supersedes anyway —
// and only then the oldest entry of any kind. The caller holds Hub.mu.
func (w *worker) push(e entry) {
	w.queue = collapseRuns(append(w.queue, e))
	if len(w.queue) > queueCap {
		// e is the one entry past the cap, so the search stops short of it:
		// a full queue makes room for e and never drops it.
		i := max(0, slices.IndexFunc(w.queue[:queueCap], supersedable))
		// A drop can leave two entries of one kind side by side, e included;
		// the older of them goes, as if they had collapsed on arrival.
		w.queue = collapseRuns(slices.Delete(w.queue, i, i+1))
	}
}

// collapseRuns keeps only the newest entry of each run of one Kind, in place.
func collapseRuns(q []entry) []entry {
	out := q[:0]
	for i, e := range q {
		if i+1 < len(q) && q[i+1].status.Kind == e.status.Kind {
			continue
		}
		out = append(out, e)
	}
	return out
}

func supersedable(e entry) bool {
	return e.status.Kind == Working || e.status.Kind == Idle
}

// stopTimerLocked drops the held Idle, if any. The caller holds Hub.mu.
func (w *worker) stopTimerLocked() {
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer, w.timerC, w.held = nil, nil, entry{}
}

type realTimer struct{ t *time.Timer }

func newRealTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

func (t realTimer) C() <-chan time.Time { return t.t.C }
func (t realTimer) Stop() bool          { return t.t.Stop() }
