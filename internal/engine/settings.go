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
	// Value is the confirmed value: what the session is now at, captured in the
	// section that changed it (agent.SetOutcome) and NOT an echo of what was
	// asked for. The two differ wherever a provider resolves a request — the
	// native session turns an empty effort into the model's own default and a
	// model alias into its canonical id — and a client that was handed the
	// request back would disagree with the delta at Rev.
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
	// claimed is closed by the worker in the one locked section that has
	// established that this request WILL be put to the provider (runSet) —
	// never for one answered without running it, whether by takeSet's own
	// dead-context check or by runSet's re-check of the context, the engine's
	// refusal and the outbox's room. It means "the provider is about to be
	// asked", and nothing weaker: a caller whose context has ended reads it as
	// the difference between a request that changed nothing — owed the plain
	// context error or the plain refusal, both of which leave its command id
	// retryable — and one at the provider, which can only be answered honestly
	// with ErrSetOutcomeUnknown (r27 finding 3).
	claimed chan struct{}
}

// newSetReq is one Set's request, with both of its channels: reply is buffered
// for the one answer it will ever carry, so the worker never blocks sending it
// even when the caller has long since given up and gone.
func newSetReq(ctx context.Context, c Command, s Setting) *setReq {
	return &setReq{ctx: ctx, c: c, s: s, reply: make(chan setAnswer, 1), claimed: make(chan struct{})}
}

// ctxErr is the request's own cancellation, as the worker checks it when it
// claims the request — and nil for a Set made with no context at all.
func (r *setReq) ctxErr() error {
	if r.ctx == nil {
		return nil
	}
	return r.ctx.Err()
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
//     provider is asked and in the same locked section that claims the request,
//     because that is the last moment at which nothing has changed yet. A Set
//     that waited in the queue is judged by the room there is when its turn
//     comes, and one refused there is refused having run nothing at all, so its
//     command id stays retryable (runSet, r27 finding 3).
//   - A refusal from the provider is returned as it came, with nothing mutated
//     and no event published.
//
// # What a context that ends buys, and what it does not
//
// A Set whose ctx ends while it is still WAITING ITS TURN changed nothing and
// is told so with the context's error. Two goroutines can find that out — the
// caller, which takes it out of the queue itself (dropSet), and the worker,
// which checks the request's context in the very section that DEQUEUES it
// (takeSet) and answers a dead one there without running it. They take the same
// lock, so exactly one of them has the request and exactly one answer is ever
// sent; what the pair rules out is the schedule the other order allowed, where
// the worker dequeued a request whose caller had already given up and asked the
// provider for a change nobody was waiting for (r23 finding 1). A context that
// ends in the gap AFTER the dequeue is checked once more, in the section that
// claims the request (runSet), and answered there in the same way.
//
// Once the worker has CLAIMED it, this call is bounded by its context again,
// honestly: it waits for the worker's answer OR for its own context, whichever
// comes first, and a context that wins returns ErrSetOutcomeUnknown — wrapping
// that context's error, so errors.Is(err, context.DeadlineExceeded) still
// holds. That sentinel says exactly what is true: the provider has the change
// or is about to, internal/acp writes a request before it can look at a context
// at all, and nobody on this side can say whether it landed. The stream says —
// if the change lands, its delta is published like any other.
//
// It is never plain context.Canceled for a claimed request, which would read as
// "nothing happened", and never a success the caller has waited past its own
// deadline for: a Set from a UI is bounded by modeCallTimeout precisely so an
// agent that has stopped reading its stdin cannot pin a chip for ever, and a
// setter that ignored its caller's context would take that bound away.
//
// The worker carries on regardless — a change the provider has taken has taken,
// and abandoning the flush would only lose the revision — and its late answer
// goes into a buffered channel nobody need read. A ctx that ends during the
// flush of a change the caller is still waiting for leaves that change standing
// with Rev 0; see runSet.
func (e *Engine) Set(ctx context.Context, c Command, s Setting) (SetResult, error) {
	if err := s.validate(); err != nil {
		return SetResult{}, err
	}
	hash := receiptHash("Set", string(s.Kind), s.ID, s.Value)
	return withBlockingReceipt(ctx, e.receipts, c, hash, func() (SetResult, error) {
		r := newSetReq(ctx, c, s)
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
			if e.beforeDropSet != nil {
				// A test barrier, nil in every other build: it parks this
				// goroutine between its context ending and its own dequeue, so
				// the schedule where the WORKER claims a request whose caller
				// has already given up can be forced rather than hoped for.
				e.beforeDropSet()
			}
			if e.dropSet(r) {
				// Still waiting its turn, and now out of the queue: nothing was
				// asked of the provider and nothing changed.
				return SetResult{}, ctx.Err()
			}
			// The worker has it, and what that means is decided by the claim:
			//
			//   - claimed is still open: the worker has not yet decided to run
			//     the request, and the answer on its way is one of the three that
			//     mean nothing was asked of the provider — the plain context
			//     error, ErrNotAccepting or ErrUnavailable. The wait is bounded:
			//     between takeSet and the locked section that decides (runSet)
			//     the worker does nothing that can block, so one of these two
			//     channels is always about to be ready.
			//   - claimed is closed: the request is at the provider. This call's
			//     own deadline has passed and it says so, with the one answer
			//     that is true (ErrSetOutcomeUnknown).
			select {
			case a := <-r.reply:
				return a.res, a.err
			case <-r.claimed:
				// A reply that arrived in the same instant is preferred to the
				// sentinel: it is strictly more informative and equally true.
				select {
				case a := <-r.reply:
					return a.res, a.err
				default:
				}
				return SetResult{}, setOutcomeUnknown(ctx.Err())
			}
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
//
// A request whose context has ended is answered here too, with that context's
// error and WITHOUT being run: the check is in the same locked section that
// takes it out of the queue, which is what makes it airtight. A caller that
// gives up races dropSet against this dequeue, and whichever wins, the request
// is out of the queue exactly once and answered exactly once — and the provider
// is never asked for a change whose caller had already gone (r23 finding 1).
// The answers are sent under e.mu, which is safe because every reply channel is
// buffered for the one answer it will ever carry.
//
// It does NOT claim the request. Taking it out of the queue is not yet a
// promise that it will run — runSet still has to find the engine admitting and
// the outbox with room — and claiming it here made that promise for requests
// that were then refused without running, which left a stored "outcome unknown"
// where a retryable refusal belonged (r27 finding 3). The claim is runSet's.
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
	for len(e.sets) > 0 {
		r := e.sets[0]
		e.sets = e.sets[1:]
		if err := r.ctxErr(); err != nil {
			r.reply <- setAnswer{err: err}
			continue
		}
		return r, true
	}
	return nil, false
}

// setOutcomeUnknown is the answer a claimed request's caller gets when its own
// context ends first: the sentinel AND the context's error, so a client that
// matches on either is right (ErrSetOutcomeUnknown).
func setOutcomeUnknown(ctxErr error) error {
	if ctxErr == nil {
		// Unreachable: this is only ever built from a context that is done.
		return ErrSetOutcomeUnknown
	}
	return fmt.Errorf("%w: %w", ErrSetOutcomeUnknown, ctxErr)
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
// the last word on whether it runs at all, the claim, the provider, then the
// barrier that learns the delta's revision.
//
// # The claim
//
// Everything that can still refuse this request without running it is decided
// in ONE locked section, and only then is the request claimed (setReq.claimed).
// That is what makes the claim mean "the provider is about to be asked" and
// nothing weaker, so ErrSetOutcomeUnknown is never stored for a change that
// provably did not happen (r27 finding 3). Three things can refuse it:
//
//   - the engine's gate, which Close or Stop can have shut since this request
//     was queued: ErrNotAccepting;
//   - the log's outbox, which is checked HERE and not at queueing time, because
//     a Set that waited in the queue is judged by the room there is when its
//     turn comes, and because this is the last moment at which nothing has
//     changed yet: ErrUnavailable;
//   - the request's own context, re-read because it can have ended between
//     takeSet and this section: the plain context error, and nothing run (r23's
//     rule).
//
// The two GATE refusals are preferred to the context error when both are true,
// which is takeSet's own precedence for a closed engine — it answers everything
// queued with ErrNotAccepting whatever those requests' contexts say — and it is
// the answer that serves the caller better: a gate refusal is forgotten by the
// receipts table, so the command id stays retryable and a resend made once the
// door reopens is a genuine attempt, where a stored context error would be
// replayed for ever (receipts.go, "What is stored, and what is left retryable").
//
// # The flush
//
// The flush is what makes Rev answerable at all — the delta's number is
// assigned by the log's drainer, on another goroutine — and it is not a
// condition: a change the provider took has taken, so a flush that fails leaves
// the result standing with Rev 0 rather than turning a change that landed into
// an error. It passes no session done channel, as every flush of the engine's
// does: the session's own close closes the log, which is what frees it.
func (e *Engine) runSet(r *setReq) (SetResult, error) {
	if h := e.hooks; h != nil && h.beforeRunSet != nil {
		// A test barrier, nil in every other build: it parks the worker in the
		// one gap where a request is neither queued nor claimed, which is the
		// whole of what r27 finding 3 is about.
		h.beforeRunSet()
	}
	e.mu.Lock()
	refused := e.refusalLocked()
	room := e.log.OutboxRoom()
	ctxErr := r.ctxErr()
	switch {
	case refused != nil:
		e.mu.Unlock()
		return SetResult{}, refused
	case !room:
		e.mu.Unlock()
		return SetResult{}, ErrUnavailable
	case ctxErr != nil:
		e.mu.Unlock()
		return SetResult{}, ctxErr
	}
	// Nothing left that could refuse it: the claim is made here, in the section
	// that established that, and the lock released before the provider is asked.
	close(r.claimed)
	e.mu.Unlock()
	var (
		out agent.SetOutcome
		err error
	)
	cause := r.c.Cause()
	switch r.s.Kind {
	case SettingModel:
		out, err = e.sess.SetModel(r.ctx, cause, r.s.Value)
	case SettingMode:
		out, err = e.sess.SetMode(r.ctx, cause, r.s.Value)
	case SettingConfig:
		out, err = e.sess.SetConfig(r.ctx, cause, r.s.ID, r.s.Value)
	default:
		// Unreachable: Set validates before anything is queued.
		return SetResult{}, fmt.Errorf("%w: setting kind %q", ErrBadRequest, r.s.Kind)
	}
	if err != nil {
		return SetResult{}, err
	}
	_ = e.log.Flush(r.ctx, nil)
	// The session's confirmed value, never the one that was asked for: a
	// provider is free to resolve what it was sent — an empty effort to the
	// model's default, a model alias to its canonical id — and echoing the
	// request here would hand a client a value that contradicts the very delta
	// this Rev names (agent.SetOutcome, r23 finding 4).
	return SetResult{Value: out.Value, Rev: out.Ticket.Seq()}, nil
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
// Event.Text as well, and the index takes *that* as the agent's own name for
// the session and `craze prompt --json` prints it as a title line — which is
// exactly why a rename must not fill it (plan 021 correction 20).
//
// # The index row, and the one thing here that waits
//
// A rename is also a row in the session index, pinned, so that no agent title
// this session or a later --continue produces can take the name back. That
// write is file I/O and it runs HERE, on the caller's goroutine, outside e.mu
// — §3.2's other documented exception, and exactly where the TUI did it. Its
// failure is RETURNED, wrapped in ErrIndexWrite, because unlike every other
// index write this one has a caller still standing: the session HAS been
// renamed and pinned, and only the file write failed, so a client draws the
// failure and still says the rename happened.
//
// Because it is returned it is also STORED by the receipts table, like any
// other answer that is about this request rather than about the engine's door
// being shut — which is right: the title really was set, so a resend of the
// same command id must replay that answer rather than rename a second time.
// What retries the write is the next index write of any kind (the next turn's
// end touches the row, the next prompt seeds it if it never was), or a later
// /rename with a new command id.
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
		if err := e.sess.SetTitle(c.Cause(), title); err != nil {
			return err
		}
		if err := e.idx.rename(c.Cause(), title); err != nil {
			return &indexWriteError{err: err}
		}
		return nil
	})
}
