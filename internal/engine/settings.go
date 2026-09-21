package engine

import (
	"context"
	"fmt"

	"github.com/charliek/craze/internal/agent"
)

// SettingKind is which of the session's settings a Setting names.
type SettingKind string

const (
	// SettingModel is the model the next turn runs on.
	SettingModel SettingKind = "model"
	// SettingMode is the agent's mode — plan, ask, agent, whatever it
	// advertises.
	SettingMode SettingKind = "mode"
	// SettingConfig is one of the provider's config options, named by ID.
	SettingConfig SettingKind = "config"
)

// Setting is one settings change: what to set, and what to. For SettingConfig,
// ID is the option's id; the other two kinds name nothing but their value.
type Setting struct {
	Kind  SettingKind
	ID    string
	Value string
}

// A setting's kind is also the StateDelta section it changes, which is what a
// client keeps its revision check per. Config is one section whatever option it
// names: a config delta carries every option in full, so one of them changing
// is the whole section changing.
func (s Setting) validate() error {
	switch s.Kind {
	case SettingModel, SettingMode:
		if s.ID != "" {
			return fmt.Errorf("%w: a %s setting names no option", ErrBadRequest, s.Kind)
		}
		return nil
	case SettingConfig:
		if s.ID == "" {
			return fmt.Errorf("%w: a config setting needs an option id", ErrBadRequest)
		}
		return nil
	default:
		return fmt.Errorf("%w: setting kind %q", ErrBadRequest, s.Kind)
	}
}

// SetResult is a settings change the provider took: the value it is now at, and
// the revision that value was committed with.
type SetResult struct {
	// Value is the confirmed value — what the provider accepted, which for
	// every setting craze has today is what was asked for.
	Value string
	// Rev is the Seq of the StateDelta the session enqueued for this change:
	// the change's revision (plan 021 §3.8). A client that applies a delayed
	// reply compares it against the revision of the last delta it applied for
	// that section, and so cannot undo a newer change with an older answer.
	//
	// It is 0 when the revision could not be learned: the log was closing, or
	// the caller's context ended before the delta was committed. A client reads
	// 0 as "no revision", never as one older than every other.
	Rev uint64
}

// setReq is one Set waiting for the settings worker.
type setReq struct {
	ctx   context.Context
	c     Command
	s     Setting
	reply chan setAnswer
}

type setAnswer struct {
	res SetResult
	err error
}

// Set changes one of the session's settings and answers with the confirmed
// value and its revision. It blocks — on the provider, and on the barrier that
// learns the revision — and belongs on a goroutine that is not the primary's
// reader.
//
// # One at a time, in arrival order
//
// Every Set goes through one FIFO worker: the provider call, the session's
// locked mutate-and-enqueue, the flush that learns the delta's Seq, then the
// answer. Two clients changing the mode at once are therefore applied one after
// the other, in the order they arrived — which is the order they took e.mu to
// join the queue — and never interleaved, so the last delta by Seq is the value
// the session is left at. Without the worker, two provider calls in flight
// could be answered in either order and the deltas could be enqueued in the
// other one.
//
// The session is the author of the delta, never the worker: it is enqueued
// under the session's own lock, in the section that mutates the snapshot, so it
// is ordered against the agent's own updates on the read loop as well as
// against the other Sets (plan 021 §3.8, panel astra 14 / CodeRabbit 7).
//
// # What it refuses
//
//   - Before the session is up, while a session/load replay runs, after Stop
//     and after Close: ErrNotAccepting. A turn working, an ask pending and a
//     turn of the agent's own are all fine — the TUI has always allowed a mode
//     click mid-turn, and a setting is not a prompt.
//   - When the log's outbox has no room for the delta the change would
//     publish: ErrUnavailable, checked in the worker immediately **before** the
//     provider is asked, because that is the last moment at which nothing has
//     changed yet. A Set that waited in the queue is judged by the room there
//     is when its turn comes.
//   - A refusal from the provider is returned as it came, with nothing mutated
//     and no event published.
//
// A ctx that ends while the Set is still waiting its turn returns the context's
// error, having changed nothing; one that ends after the worker has taken it
// returns whatever the session makes of it, because by then the provider has
// the call.
func (e *Engine) Set(ctx context.Context, c Command, s Setting) (SetResult, error) {
	if err := s.validate(); err != nil {
		return SetResult{}, err
	}
	hash := receiptHash("Set", string(s.Kind), s.ID, s.Value)
	return withBlockingReceipt(ctx, e.receipts, c, hash, func() (SetResult, error) {
		r := &setReq{ctx: ctx, c: c, s: s, reply: make(chan setAnswer, 1)}
		if err := e.queueSet(r); err != nil {
			return SetResult{}, err
		}
		var done <-chan struct{}
		if ctx != nil {
			done = ctx.Done()
		}
		select {
		case a := <-r.reply:
			return a.res, a.err
		case <-done:
			if e.dropSet(r) {
				// Still waiting its turn, and now out of the queue: nothing was
				// asked of the provider and nothing changed.
				return SetResult{}, ctx.Err()
			}
			// The worker has it. Its answer is the truth about what the provider
			// was told, and it is coming: the provider call and the flush both take
			// this same ctx, so neither outlives it by much.
			a := <-r.reply
			return a.res, a.err
		}
	})
}

// queueSet puts r at the back of the settings queue and wakes the worker. The
// gate and the append are one section, and Close closes the engine under the
// same lock, so a request is either queued before the engine closed — and then
// the worker answers it, even if that answer is "not accepting" — or refused
// here. It can never be left in a queue nobody will look at again.
func (e *Engine) queueSet(r *setReq) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refusalLocked(); err != nil {
		return err
	}
	e.sets = append(e.sets, r)
	select {
	case e.setWake <- struct{}{}:
	default:
	}
	return nil
}

// dropSet takes r out of the settings queue, and reports whether it was still
// in it: false means the worker has already taken it, and the caller must wait
// for its answer rather than decide anything itself.
func (e *Engine) dropSet(r *setReq) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, q := range e.sets {
		if q == r {
			e.sets = append(e.sets[:i], e.sets[i+1:]...)
			return true
		}
	}
	return false
}

// takeSet is the next request for the worker, or false when there is none. A
// closed engine has none ever again: everything still queued is answered here,
// so no caller is left waiting on a worker that is about to exit.
func (e *Engine) takeSet() (*setReq, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		for _, r := range e.sets {
			r.reply <- setAnswer{err: ErrNotAccepting}
		}
		e.sets = nil
		return nil, false
	}
	if len(e.sets) == 0 {
		return nil, false
	}
	r := e.sets[0]
	e.sets = e.sets[1:]
	return r, true
}

// serveSets is the settings worker: one goroutine, one request at a time, in
// the order they were queued. It is counted in the engine's WaitGroup and
// joined by Close, which closes done after the session has closed — so a
// request in flight is freed by that close (the provider call fails, the flush
// returns) rather than held by this loop.
func (e *Engine) serveSets() {
	defer e.wg.Done()
	for {
		for {
			r, ok := e.takeSet()
			if !ok {
				break
			}
			res, err := e.runSet(r)
			r.reply <- setAnswer{res: res, err: err}
		}
		select {
		case <-e.setWake:
		case <-e.done:
			// One last pass, so a request queued in the instant before the
			// close is answered rather than abandoned.
			for {
				if _, ok := e.takeSet(); !ok {
					return
				}
			}
		}
	}
}

// runSet is one settings change, start to finish, on the worker's goroutine:
// the room for its delta, the provider, then the barrier that learns the
// delta's revision.
//
// The flush is what makes Rev answerable at all — the delta's number is
// assigned by the log's drainer, on another goroutine — and it is not a
// condition: a change the provider took has taken, so a flush that fails leaves
// the result standing with Rev 0 rather than turning a change that landed into
// an error. It passes no session done channel, as every flush of the engine's
// does: the session's own close closes the log, which is what frees it.
func (e *Engine) runSet(r *setReq) (SetResult, error) {
	e.mu.Lock()
	refused := e.refusalLocked()
	room := e.log.OutboxRoom()
	e.mu.Unlock()
	if refused != nil {
		return SetResult{}, refused
	}
	if !room {
		return SetResult{}, ErrUnavailable
	}
	var (
		t   *agent.Ticket
		err error
	)
	cause := r.c.Cause()
	switch r.s.Kind {
	case SettingModel:
		t, err = e.sess.SetModel(r.ctx, cause, r.s.Value)
	case SettingMode:
		t, err = e.sess.SetMode(r.ctx, cause, r.s.Value)
	case SettingConfig:
		t, err = e.sess.SetConfig(r.ctx, cause, r.s.ID, r.s.Value)
	default:
		// Unreachable: Set validates before anything is queued.
		return SetResult{}, fmt.Errorf("%w: setting kind %q", ErrBadRequest, r.s.Kind)
	}
	if err != nil {
		return SetResult{}, err
	}
	_ = e.log.Flush(r.ctx, nil)
	return SetResult{Value: r.s.Value, Rev: t.Seq()}, nil
}

// SetTitle renames the session — craze's own name for it, since ACP has no
// rename verb — and pins that name against any title the agent invents later.
// It **waits on nothing**: the session sets and pins the title and enqueues the
// Title delta in one locked section, with no provider to ask, so a UI may call
// it from its own Update as it always has.
//
// It is rejectable, and the session refuses it: agent.ErrSetUnavailable when
// the log's outbox has no room for the delta, decided in the very section that
// would mutate, with nothing renamed and nothing pinned. Code answers
// "unavailable" for it, as it does for the engine's own ErrUnavailable.
//
// The delta carries the title in Event.State alone. An agent's own title fills
// Event.Text as well, and a client writes *that* to the session index as an
// agent title and prints it as `craze prompt --json`'s title line — which is
// exactly why a rename must not fill it (plan 021 correction 20).
//
// Writing the index row for a rename is still the client's, until the index
// moves into the engine (C12).
func (e *Engine) SetTitle(c Command, title string) error {
	hash := receiptHash("SetTitle", title)
	return withSyncReceiptErr(e.receipts, c, hash, func() error {
		e.mu.Lock()
		refused := e.refusalLocked()
		e.mu.Unlock()
		if refused != nil {
			return refused
		}
		// Outside e.mu: the engine calls exactly two things on the seam with its
		// own lock held (Begin and ForeignTurn, engine.go), and this needs to be
		// neither of them — it takes the session's lock and the outbox's, both
		// briefly, and waits on nothing either way.
		return e.sess.SetTitle(c.Cause(), title)
	})
}
