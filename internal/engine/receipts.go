package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// Command ids (plan 021 §3.8, A16, panel astra finding 16).
//
// # Why
//
// A session host is born detached from S4 on and the TUI becomes a socket
// client for good (SD-33): a client that can lose its connection has to be
// able to ask "did my prompt go through?" by sending the command again, and a
// resend must never become a second execution.
//
// # Identity
//
// A command id is namespaced by its client (Command.Client, minted by
// NewClientID, unique in the incarnation) and is otherwise the client's own
// counter, which the doc on Command says counts from 1. The canonical
// spelling is the positive decimal integer that counter mints —
// fmt.Sprintf("%d", n), exactly what the TUI's nextCmd and craze prompt's
// engine.Command{} (the zero value, which never reaches this table at all)
// already do. An id that is not a positive base-10 integer — empty, signed,
// non-numeric, or zero — is ErrBadRequest rather than a command run with no
// idempotency: the table's eviction depends on being able to place an id on
// a line ("has this client's counter passed it"), and a spelling that cannot
// be placed on that line cannot be tracked as one, ever, by any client. Bad
// requests do not mutate.
//
// # The table
//
// receiptTable is one table for the whole engine, shared by every client and
// every mutating Control method: the fourteen entry hooks below are its only
// callers. It has its own mutex, receiptTable.mu, never held while e.mu or
// registry.mu (internal/agent's AskRegistry) is also held, and never taken
// from inside a section that holds either of them — so it adds no edge to the
// engine's existing "e.mu and registry.mu never nest" rule. What it may be
// held across differs by how the command it is guarding behaves:
//
//   - A synchronous command (Submit, Disarm, GiveUp, GiveUpDrain, the queue
//     verbs, Answer, SetTitle: the Control methods already documented as
//     "wait on nothing") holds the table's mutex across its ENTIRE call —
//     reservation, the command's own work (which may itself take e.mu or
//     registry.mu, released well inside this window), and storing the
//     result — because nothing in it blocks. This is what makes "a duplicate
//     of a synchronous command never blocks except on the table's own mutex,
//     and that mutex is held for a call that itself waits on nothing" true:
//     a genuinely concurrent duplicate contends on an ordinary Go mutex held
//     for an in-memory operation, exactly the way two callers already
//     contend on e.mu today, and by the time it acquires the table's mutex
//     the owner's result is always there to hand back.
//   - A blocking command (Cancel, Stop, Set, Interject) cannot hold the
//     table's mutex across its call — that call reaches a provider or the
//     session's own blocking Cancel, and the table's mutex may never be held
//     across a blocking call. So the table's mutex is taken only twice: once
//     to reserve the id (insert an entry with an open done channel) and once,
//     implicitly, when the owner writes the stored result and closes that
//     channel (no lock needed there — see receipt.done). A duplicate that
//     finds the reservation still open waits on that channel, honouring its
//     own ctx, and disturbs nothing if its ctx loses the race: the owner's
//     entry is untouched and finishes exactly as it would have.
//
// Either way, a resend whose payload hash does not match the reservation's is
// ErrBadRequest immediately, on any goroutine that finds the mismatch, never
// a wait and never a re-execution (05-protocol.md's "Errors").
//
// # Bound and eviction
//
// The table keeps the last receiptCap results or receiptAge, whichever is
// less: an insert first evicts from the front (oldest first — insertion
// order is age order, because the session's own clock is read once per entry
// at reservation and does not run backwards within one incarnation) while the
// table holds more than receiptCap entries, then again while the oldest
// entry is older than receiptAge. RetryHorizon on State is exactly
// {Commands: receiptCap, Age: receiptAge}: everything a client needs to know
// about when a resend stops being answerable from the table.
//
// Because ids are per-client counters, an evicted entry is remembered as a
// per-client high-water mark: the highest id that client has ever had
// evicted. A later id at or below that mark has no entry and never will —
// ErrUnknownCommand, not a fresh execution — while an id above it that the
// table has never seen executes normally, because a client's own pre-minted,
// never-sent ids (the TUI's nextCmds) are supposed to leave gaps. The marks
// table is itself bounded (maxClientMarks, FIFO): clients are minted by
// NewClientID, and in this in-process phase there are at most a handful of
// them per engine (the TUI mints exactly one, for its own life; craze
// prompt mints none — every call it makes carries the zero Command and never
// reaches this table). The cap is a defensive bound for whatever mints many
// client ids — a test, or S2's socket clients later — and forgetting the
// oldest client's mark can only widen what a resend from that client does
// (an id far past any advertised horizon runs fresh instead of being told
// ErrUnknownCommand), never narrow it into a silent re-execution: the entry
// itself is what a real re-execution is checked against, and marks are only
// ever consulted for an id the table has no entry for at all.
//
// # What is stored, and what is left retryable
//
// A stored result is exactly what the owning call returned — its error
// included — so a resend gets back exactly what the first call got, a
// refusal included, with one exception: a GATE refusal — the engine simply
// not admitting anything at all right now, whatever the command — is never
// stored. gateRefusal names the four sentinels this covers: ErrNotAccepting
// (closed, stopped, still starting, replaying, or — Cancel's specific
// spelling of it — nothing of craze's own for a no-turn cancel to act on),
// ErrUnavailable, agent.ErrSetUnavailable and agent.ErrAskUnavailable (the
// same "the log's outbox has no room" fact, spelled once per seam). None of
// these is an answer about the command's own arguments or a resource it
// named; each is a fact about the engine's door being shut for a moment, and
// a client that retries the SAME id once that door reopens wants a genuine
// attempt, not a cached echo of finding it shut. So: a synchronous command
// that hits one never reserves an entry at all, and a blocking command's
// entry is forgotten (removed from the table, though any goroutine already
// waiting on it still gets the refusal — see waitReceipt) the moment its
// call returns one, in both cases leaving the id exactly as unseen as it was
// before the attempt.
//
// Every other refusal a command reaches only after being hashed and
// reserved — ErrStaleTurn, ErrBadAnswer, ErrAlreadyResolved, ErrUnknownAsk,
// ErrStaleVersion, ErrAlreadyPending, ErrUnknownRow, agent.ErrQueueFull,
// agent.ErrQueueTextTooLong, agent.ErrForeignTurn, … — is a genuine, stable
// answer about THIS request or the specific resource it named (a turn id, a
// row id, an ask id, the one send-now slot), and is stored like any other
// result: a resend must be able to replay it exactly, refusal included,
// rather than have a later, unrelated change in engine state quietly turn
// yesterday's "that row is gone" into today's silent success.
//
// Request malformation that a method rejects before it even computes a
// payload hash — a bad SubmitMode, Setting.validate — never reaches the
// table either, for the same reason a gate refusal does not: it says nothing
// about whether this command ran, so there is nothing for a resend to
// replay, and a client that fixes its request and resends the SAME id gets
// the fixed attempt rather than a stale ErrBadRequest.
//
// A stored result never shares memory with engine state or with another
// caller's copy of the same result: cloneReceiptResult gives SubmitResult's
// one pointer field (Queued *agent.QueuedPrompt — the only pointer among the
// fourteen methods' results) a fresh copy on the way in and on every way back
// out, so the queue's own row and every caller's view of it stay
// independent.

// receiptCap and receiptAge are the table's bound: the last 1024 results or
// 10 minutes, whichever is less (plan 021 §3.8) — whichever bound an entry
// crosses first evicts it. RetryHorizon reports both.
const (
	receiptCap = 1024
	receiptAge = 10 * time.Minute
	// maxClientMarks bounds the per-client high-water-mark table; see the
	// package doc above ("Bound and eviction").
	maxClientMarks = 4096
)

// RetryHorizon is the receipts table's bound, as State advertises it: within
// it, a resend is answered from the table and never re-executes; past it, the
// answer is the honest ErrUnknownCommand rather than a silent re-execution or
// a silent no-op.
type RetryHorizon struct {
	// Commands is how many results the table keeps at most.
	Commands int
	// Age is how long a result is kept at most.
	Age time.Duration
}

// receiptKey is one command's identity in the table: the client that sent it,
// and its own number for it, parsed to the integer a client's counter mints —
// never the raw string, which is not something two ids can be ordered by.
type receiptKey struct {
	client string
	id     uint64
}

// receipt is one command's reservation and, once the command it names has
// run, the result it returned.
//
// done is closed exactly once, by whichever goroutine runs the command this
// receipt reserves, after result and err are set: every field read after a
// receive from done is safe without any further lock, because a channel
// close happens-before the corresponding receive completes (the Go memory
// model), the same guarantee sync.Once itself rests on. A synchronous
// command's receipt never leaves done open where another goroutine could
// observe it: the table's mutex is held from reservation to storing the
// result, so nothing else can even look. A blocking command's receipt can:
// done is what a duplicate waits on.
type receipt struct {
	hash   string
	done   chan struct{}
	result any
	err    error
	// at is when this entry was reserved, read from the receiptTable's own
	// clock (never time.Now): the age half of the table's bound is measured
	// from here.
	at time.Time
}

// receiptTable is the command-id table engine.go builds one of, shared by
// every client and every mutating Control method. See the package-level doc
// above for its lock, its bound and eviction, and what it stores.
type receiptTable struct {
	now func() time.Time

	mu    sync.Mutex
	order []receiptKey // oldest first; also age order (see the package doc)
	byKey map[receiptKey]*receipt

	marks      map[string]uint64 // client -> its highest evicted id
	markOrder  []string          // oldest first, for the maxClientMarks cap
	maxClients int
	cap        int
	age        time.Duration
}

// newReceiptTable builds an empty table. now is the engine's own clock
// (e.now), already resolved to the session's Clocked implementation where
// there is one: the table never calls time.Now itself.
func newReceiptTable(now func() time.Time) *receiptTable {
	return &receiptTable{
		now:        now,
		byKey:      map[receiptKey]*receipt{},
		marks:      map[string]uint64{},
		maxClients: maxClientMarks,
		cap:        receiptCap,
		age:        receiptAge,
	}
}

// horizon is RetryHorizon's answer; cap and age are fixed at construction and
// never mutate afterward, so this needs no lock.
func (rt *receiptTable) horizon() RetryHorizon {
	return RetryHorizon{Commands: rt.cap, Age: rt.age}
}

// parseCommand is c's identity in the table, or the ErrBadRequest a malformed
// id is: see the package doc's "Identity".
func parseCommand(c Command) (receiptKey, error) {
	if c.Client == "" {
		return receiptKey{}, fmt.Errorf("%w: a command needs a client id", ErrBadRequest)
	}
	n, err := strconv.ParseUint(c.ID, 10, 64)
	if err != nil || n == 0 {
		return receiptKey{}, fmt.Errorf("%w: command id %q is not a positive decimal integer", ErrBadRequest, c.ID)
	}
	return receiptKey{client: c.Client, id: n}, nil
}

// lookupLocked is the table's answer to "has this command already gone
// through, or would starting it now collide with something": the entry for
// key if there is one — whatever it carries, done or not — ErrUnknownCommand
// for an id the client's evicted high-water mark already covers, or (nil,
// nil) for an id that has never been seen and may go ahead.
func (rt *receiptTable) lookupLocked(key receiptKey, hash string) (*receipt, error) {
	if r, ok := rt.byKey[key]; ok {
		if r.hash != hash {
			return nil, ErrBadRequest
		}
		return r, nil
	}
	if mark, seen := rt.marks[key.client]; seen && key.id <= mark {
		return nil, ErrUnknownCommand
	}
	return nil, nil
}

// evictLocked prunes the table to its bound, from the oldest entry, before
// every lookup and every reservation: by count first, then by age, using now
// (the table's own clock, read once by the caller so a single pass is
// self-consistent).
func (rt *receiptTable) evictLocked(now time.Time) {
	for len(rt.order) > rt.cap {
		rt.evictOldestLocked()
	}
	for len(rt.order) > 0 {
		oldest := rt.byKey[rt.order[0]]
		if now.Sub(oldest.at) <= rt.age {
			break
		}
		rt.evictOldestLocked()
	}
}

// evictOldestLocked drops the table's oldest entry and remembers it as its
// client's high-water mark.
func (rt *receiptTable) evictOldestLocked() {
	key := rt.order[0]
	rt.order = rt.order[1:]
	delete(rt.byKey, key)
	rt.markEvictedLocked(key)
}

// markEvictedLocked records that key.id (and everything at or below it, by
// construction: a client's ids are evicted in the order it minted them) is
// now unknowable for key.client.
func (rt *receiptTable) markEvictedLocked(key receiptKey) {
	cur, seen := rt.marks[key.client]
	if !seen {
		rt.markOrder = append(rt.markOrder, key.client)
		rt.evictMarksLocked()
	}
	if !seen || key.id > cur {
		rt.marks[key.client] = key.id
	}
}

// evictMarksLocked bounds the marks table itself; see the package doc's
// "Bound and eviction".
func (rt *receiptTable) evictMarksLocked() {
	for len(rt.markOrder) > rt.maxClients {
		oldest := rt.markOrder[0]
		rt.markOrder = rt.markOrder[1:]
		delete(rt.marks, oldest)
	}
}

// reserveLocked inserts a new, already-resolved entry: the whole of a
// synchronous command's bookkeeping, done under the one lock that also ran
// the command.
func (rt *receiptTable) reserveLocked(key receiptKey, hash string, result any, err error) {
	rt.byKey[key] = &receipt{hash: hash, result: result, err: err, at: rt.now()}
	rt.order = append(rt.order, key)
}

// openLocked inserts a new, not-yet-resolved entry: a blocking command's
// reservation, made before the call that will resolve it.
func (rt *receiptTable) openLocked(key receiptKey, hash string) *receipt {
	r := &receipt{hash: hash, done: make(chan struct{}), at: rt.now()}
	rt.byKey[key] = r
	rt.order = append(rt.order, key)
	return r
}

// forgetLocked undoes a blocking command's reservation when its call ended in
// a gate refusal (gateRefusal): the id goes back to being unseen, for the
// next call to attempt fresh, exactly as a synchronous command's gate refusal
// never reserved one in the first place. It only ever removes r itself — never
// a different reservation a race might have put at key since — and any
// goroutine already waiting on r (waitReceipt) still gets r's result: this
// only stops a FUTURE lookup from finding it.
func (rt *receiptTable) forgetLocked(key receiptKey, r *receipt) {
	if rt.byKey[key] != r {
		return
	}
	delete(rt.byKey, key)
	for i, k := range rt.order {
		if k == key {
			rt.order = append(rt.order[:i], rt.order[i+1:]...)
			return
		}
	}
}

// gateRefusal reports whether err is a refusal about the engine simply not
// admitting anything right now — the log's outbox has no room, or the door is
// shut for a reason that has nothing to do with this command's own arguments
// — rather than an answer about the specific command: see the package doc's
// "What is stored, and what is left retryable".
func gateRefusal(err error) bool {
	return errors.Is(err, ErrNotAccepting) || errors.Is(err, ErrUnavailable) ||
		errors.Is(err, agent.ErrSetUnavailable) || errors.Is(err, agent.ErrAskUnavailable)
}

// receiptHash is one command's payload hash: its method name, so that no two
// different Control methods can ever collide on the same client/id even if a
// client reused one (a resend and the record it matches are always for the
// same method, which is what makes the type assertion in withSyncReceipt and
// waitReceipt safe without a runtime panic), followed by every argument that
// makes the request what it is, each already rendered to text by the caller
// — never a raw %v, which would let a pointer argument's address rather than
// its value decide the hash (EditQueued's expectedVersion).
func receiptHash(method string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(method))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// versionSpelling is EditQueued's expectedVersion as the hash sees it.
func versionSpelling(v *int) string {
	if v == nil {
		return "*"
	}
	return strconv.Itoa(*v)
}

// answerSpelling is Answer's AskAnswer as the hash sees it: every field,
// including Answers' keys in sorted order (fmt would sort them too, but this
// makes the order the hash's own promise rather than fmt's).
func answerSpelling(a agent.AskAnswer) string {
	keys := make([]string, 0, len(a.Answers))
	for k := range a.Answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "opt=%s cancel=%v skip=%v accept=%v reject=%v", a.OptionID, a.Cancel, a.Skip, a.Accept, a.Reject)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, a.Answers[k])
	}
	return b.String()
}

// cloneReceiptResult gives v its own copy of anything a stored result would
// otherwise share with the call that produced it or with another resend:
// SubmitResult.Queued is the one pointer field among the fourteen commands'
// results, so it is the one case handled here. Called once on the way into
// the table and once on every way back out, so the table's own copy and
// every caller's copy are all independent.
func cloneReceiptResult[T any](v T) T {
	switch r := any(v).(type) {
	case SubmitResult:
		if r.Queued != nil {
			q := *r.Queued
			r.Queued = &q
		}
		return any(r).(T)
	default:
		return v
	}
}

// withSyncReceipt is the entry hook for a synchronous Control method (one
// documented as "waits on nothing"): c.IsZero() runs run and touches the
// table not at all, exactly today's behaviour for craze prompt, which never
// mints a command. Otherwise the table's mutex is held for the whole call —
// see the package doc's "The table" for why that is safe here and not for a
// blocking command.
func withSyncReceipt[T any](rt *receiptTable, c Command, hash string, run func() (T, error)) (T, error) {
	var zero T
	if c.IsZero() {
		return run()
	}
	key, err := parseCommand(c)
	if err != nil {
		return zero, err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.evictLocked(rt.now())
	r, err := rt.lookupLocked(key, hash)
	if err != nil {
		return zero, err
	}
	if r != nil {
		res, ok := r.result.(T)
		if !ok {
			// Unreachable: receiptHash embeds the method name, so a hash match
			// is always for the same T. Refused rather than panicked on, in
			// case that ever stops being true.
			return zero, ErrBadRequest
		}
		return cloneReceiptResult(res), r.err
	}
	res, rerr := run()
	if !gateRefusal(rerr) {
		rt.reserveLocked(key, hash, cloneReceiptResult(res), rerr)
	}
	return res, rerr
}

// withSyncReceiptErr is withSyncReceipt for a method whose only result is its
// error.
func withSyncReceiptErr(rt *receiptTable, c Command, hash string, run func() error) error {
	_, err := withSyncReceipt(rt, c, hash, func() (struct{}, error) { return struct{}{}, run() })
	return err
}

// withBlockingReceipt is the entry hook for a blocking Control method (Cancel,
// Stop, Set, Interject): c.IsZero() runs run untouched, as above. Otherwise the
// id is reserved under the table's mutex, released before run makes its
// provider or session call — the table's mutex is never held across a
// blocking call — and a duplicate that finds the reservation already there
// waits on it, honouring ctx, and returns the first call's result without
// disturbing it.
func withBlockingReceipt[T any](ctx context.Context, rt *receiptTable, c Command, hash string, run func() (T, error)) (T, error) {
	var zero T
	if c.IsZero() {
		return run()
	}
	key, err := parseCommand(c)
	if err != nil {
		return zero, err
	}
	rt.mu.Lock()
	rt.evictLocked(rt.now())
	r, err := rt.lookupLocked(key, hash)
	if err != nil {
		rt.mu.Unlock()
		return zero, err
	}
	if r != nil {
		rt.mu.Unlock()
		return waitReceipt[T](ctx, r)
	}
	r = rt.openLocked(key, hash)
	rt.mu.Unlock()

	res, rerr := run()
	// This goroutine is the only writer r.result/r.err will ever have; done's
	// close is what publishes them to anyone already waiting (receipt's doc
	// comment). The table's mutex is retaken only to forget a gate refusal —
	// briefly, and never across the call above.
	r.result, r.err = cloneReceiptResult(res), rerr
	if gateRefusal(rerr) {
		rt.mu.Lock()
		rt.forgetLocked(key, r)
		rt.mu.Unlock()
	}
	close(r.done)
	return res, rerr
}

// withBlockingReceiptErr is withBlockingReceipt for a method whose only
// result is its error.
func withBlockingReceiptErr(ctx context.Context, rt *receiptTable, c Command, hash string, run func() error) error {
	_, err := withBlockingReceipt(ctx, rt, c, hash, func() (struct{}, error) { return struct{}{}, run() })
	return err
}

// waitReceipt is a blocking command's duplicate, parked on r.done with its
// own ctx: whichever ends the wait first, the owner's entry is untouched.
func waitReceipt[T any](ctx context.Context, r *receipt) (T, error) {
	var zero T
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	select {
	case <-r.done:
		res, ok := r.result.(T)
		if !ok {
			// Unreachable: see withSyncReceipt's identical guard.
			return zero, ErrBadRequest
		}
		return cloneReceiptResult(res), r.err
	case <-done:
		return zero, ctx.Err()
	}
}
