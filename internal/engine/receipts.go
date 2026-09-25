package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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
// A command id is namespaced by its client (Command.Client) and is otherwise
// the client's own counter, which the doc on Command says counts from 1. The
// canonical spelling is the positive decimal integer that counter mints —
// fmt.Sprintf("%d", n), exactly what the TUI's nextCmd and craze prompt's
// engine.Command{} (the zero value, which never reaches this table at all)
// already do — and it is ENFORCED: an id that is not a positive base-10
// integer, or one spelled some other way for the same number ("01", "+1",
// " 1"), is ErrBadRequest rather than a command run with no idempotency. The
// table's eviction depends on being able to place an id on a line ("has this
// client's counter passed it"), a spelling that cannot be placed on that line
// cannot be tracked as one, and two spellings of one number would otherwise
// alias two different causes onto one entry. Bad requests do not mutate.
//
// The client half is minted HERE, by newClient (Engine.NewClientID), and a
// command naming a client this table never minted is ErrBadRequest for the
// same reason: the per-client high-water marks below are what make "never a
// re-execution" true past the table's bound, and a mark can only be kept for a
// client the table knows about. An invented client id would have no mark, so
// every id it ever used would look unseen and run again.
//
// # The table
//
// receiptTable is one table for the whole engine, shared by every client and
// every mutating Control method: the fifteen entry hooks below are its only
// callers. Its mutex is a LEAF — it is never held across a command's own work,
// a hook, a provider call or anything else that can block, and it is never
// taken while e.mu, s.mu or registry.mu is held (engine.go's struct comment
// says the same from the other side). Every command has the same shape
// whatever it is:
//
//	under rt.mu: look up an entry, or reserve one and own it
//	rt.mu RELEASED: run the command
//	under rt.mu: complete the entry — store the result and close done
//
// which is what makes the file I/O C12 puts inside Submit and SetTitle safe: a
// stalled index write blocks nobody but a same-id duplicate.
//
// What a duplicate gets depends on what it finds:
//
//   - A COMPLETED entry is replayed — exactly what the first call returned,
//     its error included — with no wait at all, WHATEVER the duplicate's own
//     ctx: it is answered, never raced against that ctx ending.
//   - An OPEN reservation of a BLOCKING command (Cancel, Stop, Set, Interject)
//     is waited on: the duplicate parks on the reservation's done channel,
//     honouring its own ctx, and disturbs nothing if its ctx loses the race —
//     the owner's entry is untouched and finishes exactly as it would have.
//   - An OPEN reservation of a SYNCHRONOUS command (Submit, Disarm, GiveUp,
//     GiveUpDrain, the queue verbs, Answer, CancelSubagent, SetTitle: the
//     Control methods documented as "wait on nothing") is ErrCommandInProgress at once —
//     "this id is reserved and its command is still running; resend the SAME
//     id" — and never a wait, because "a duplicate of a synchronous command
//     never waits" is kept literally. It stores nothing and changes nothing,
//     so the id stays exactly as it was and the client may resend it.
//     In-process this is unreachable for a single client — the TUI makes all
//     fifteen calls from its one Update goroutine — and it exists for S2's
//     socket, where two connections of one client can be in flight at once.
//
// Either way, a resend whose payload hash does not match the reservation's is
// ErrBadRequest immediately, on any goroutine that finds the mismatch, never
// a wait and never a re-execution (05-protocol.md's "Errors").
//
// # Completion is not optional
//
// The goroutine that reserved an id completes it on EVERY path out, a panic
// included: the completion is installed in a defer the moment the id is
// reserved, and a panic stores ErrCommandAborted ("the command's outcome is
// unknown") as the result and closes done on the way through, so a duplicate
// already parked on it is answered rather than stranded for good. That result
// is STORED and never forgotten: a command that panicked may already have
// mutated state, so its id must never run a second time. A client that wants
// another attempt sends a NEW id, which is the same rule ErrBadRequest and
// ErrUnknownCommand leave it with.
//
// # Bound and eviction
//
// The table keeps the last receiptCap results or receiptAge, whichever is
// less. Only COMPLETED receipts are subject to either bound, and each is
// timestamped when it COMPLETES:
//
//   - An OPEN reservation is in no eviction order and no bound applies to it.
//     It stays discoverable until its owner completes, however long the
//     command takes, so a duplicate always finds it and a gate refusal after a
//     very long call still leaves the id retryable rather than permanently
//     unknown. What bounds open reservations is that each one is a command in
//     flight on some goroutine.
//   - The COUNT bound is enforced after every store, so the table is never
//     over it: the oldest completed entry is dropped as soon as the one past
//     the cap is kept. An attempt that stores nothing — a gate refusal — drops
//     nothing.
//   - The AGE bound is measured on the table's own clock (time.Now, a test's
//     through receiptHooks), never the session's event clock, which a test may
//     freeze or a golden may pin: a receipt's lifetime is real elapsed time.
//     The pass looks at every completed entry rather than stopping at the
//     front, so a clock that went backwards (only a test's can) cannot park a
//     future-dated entry in front of entries that have expired.
//
// RetryHorizon on State is exactly {Commands: receiptCap, Age: receiptAge}:
// everything a client needs to know about when a resend stops being answerable
// from the table.
//
// Because ids are per-client counters, an evicted entry is remembered as a
// per-client high-water mark: the highest id that client has ever had evicted.
// A later id at or below that mark has no entry and never will —
// ErrUnknownCommand, not a fresh execution — while an id above it that the
// table has never seen executes normally, because a client's own pre-minted,
// never-sent ids (the TUI's nextCmds) are supposed to leave gaps. A mark is
// never forgotten while its client is live: forgetting one would make that
// client's old ids executable again, which is the one thing this table exists
// to prevent.
//
// No CLIENT is ever retired: a minted client's mark lives as long as the
// engine, however many clients are minted after it. Retiring a live identity
// would refuse a client's OWN commands while it is still attached — the
// TUI's, in particular, since it mints exactly one client for its own life
// and would have every Submit answered ErrUnknownCommand for good the moment
// enough other clients existed, whatever their own ids or whether the table
// ever saw them. The cost is one counter per minted client, and in this
// in-process phase nothing comes near a size where that cost matters — the
// TUI mints one client for its own life and craze prompt mints none, every
// call it makes carrying the zero Command.
//
// S2, which will mint a client PER CONNECTION rather than once per engine,
// must add its own lifecycle on top of this rather than reuse it unchanged:
// an explicit release when a connection disconnects, and retirement — if S2
// wants one at all — only of a RELEASED client, never one merely idle, and it
// must bind a server-minted client id to its own connection rather than trust
// a client-supplied "c-N", which is predictable and would let one connection
// claim another's identity (r26 finding 2).
//
// # What is stored, and what is left retryable
//
// A stored result is exactly what the owning call returned — its error
// included — so a resend gets back exactly what the first call got, a
// refusal included, with one exception: a GATE refusal — the engine simply
// not admitting anything at all right now, whatever the command — is never
// stored. gateRefusal (derived from classify, control.go, per r28 finding 1)
// names the sentinels this covers: ErrNotAccepting (closed, stopped, still
// starting, replaying, or — Cancel's specific spelling of it — nothing of
// craze's own for a no-turn cancel to act on) and agent.ErrNotInTurn (an
// Interject with no turn to merge into — the same fact, spelled at the other
// seam); ErrUnavailable, agent.ErrSetUnavailable and agent.ErrAskUnavailable
// (the log's outbox has no room, spelled once per seam); and errNotRun
// (settings.go) — a Set answered without running because its own ctx was
// already dead when takeSet or runSet looked, so nothing was asked of the
// provider either. None of these is an answer about the command's own
// arguments or a resource it named; each is a fact about the engine's door
// being shut, or nothing having run, for a moment, and a client that retries
// the SAME id once that passes wants a genuine attempt, not a cached echo. So
// a command that hits one has its reservation FORGOTTEN — removed from the
// table, in the same locked section that publishes the refusal to anyone
// already waiting on it — which leaves the id exactly as unseen as it was
// before the attempt, for the next call to attempt fresh.
//
// Every other refusal a command reaches only after being hashed and
// reserved — ErrStaleTurn, ErrBadAnswer, ErrAlreadyResolved, ErrUnknownAsk,
// ErrStaleVersion, ErrAlreadyPending, ErrUnknownRow, agent.ErrQueueFull,
// agent.ErrQueueTextTooLong, agent.ErrForeignTurn, an unclassified
// provider/RPC error ("failed", classify's own default), … — is a genuine,
// stable answer about THIS request or the specific resource it named (a turn
// id, a row id, an ask id, the one send-now slot), or about the command
// having run and failed, and is stored like any other result: a resend must
// be able to replay it exactly, refusal included, rather than have a later,
// unrelated change in engine state quietly turn yesterday's "that row is
// gone" into today's silent success.
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
// fifteen methods' results) a fresh copy on the way in and on every way back
// out, so the queue's own row and every caller's view of it stay
// independent.

// receiptCap and receiptAge are the table's bound: the last 1024 results or
// 10 minutes, whichever is less (plan 021 §3.8) — whichever bound an entry
// crosses first evicts it. RetryHorizon reports both. No bound applies to the
// CLIENTS the table keeps marks for; see the package doc above ("Bound and
// eviction") for why (r26 finding 2).
const (
	receiptCap = 1024
	receiptAge = 10 * time.Minute
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

func (k receiptKey) String() string { return k.client + "/" + strconv.FormatUint(k.id, 10) }

// receipt is one command's reservation and, once the command it names has
// run, the result it returned.
//
// completed, result, err and at are written exactly once, under the table's
// mutex, by the goroutine that owns the reservation, and done is closed in
// that same locked section — so "completed" and "done is closed" are one
// fact, with no window between them for anyone to see half of. A reader that
// has held the table's mutex since completed was set, or that has received
// from done (a channel close happens-before the corresponding receive
// completes), may read result and err without any further lock.
type receipt struct {
	hash string
	done chan struct{}

	completed bool
	result    any
	err       error
	// at is when this entry COMPLETED, read from the receiptTable's own clock:
	// the age half of the table's bound is measured from here, so a command
	// that took an hour is kept for its ten minutes after it returned rather
	// than being born expired.
	at time.Time
}

// receiptClient is one minted client: the highest id of its the table has
// evicted. No client is ever retired (r26 finding 2), so this is the entirety
// of what a live client costs the table to keep.
type receiptClient struct {
	// mark is the highest id of this client's the table has evicted — 0 until
	// one is. Everything at or below it is unknowable, for good.
	mark uint64
}

// receiptHooks are the table's seams, nil in every build but a test's. They
// are given to newReceiptTable and never assigned afterwards (plan 021 X15).
type receiptHooks struct {
	// now is the clock the age bound is measured on; time.Now when unset.
	now func() time.Time
	// foundOpen is called by a duplicate that found an OPEN reservation, with
	// the table's mutex released and before it waits (a blocking command) or is
	// refused (a synchronous one): the barrier a test needs to prove a
	// duplicate really did reach the wait its name claims.
	foundOpen func()
	// afterForget is called by the owner of a gate-refused reservation once
	// that reservation is out of the table and everyone waiting on it has been
	// answered, and before the owner returns: the window in which the id is
	// retryable and the first attempt has not yet come back.
	afterForget func()
}

// receiptTable is the command-id table engine.go builds one of, shared by
// every client and every mutating Control method. See the package-level doc
// above for its lock, its bound and eviction, and what it stores.
type receiptTable struct {
	hooks receiptHooks

	mu    sync.Mutex
	order []receiptKey // COMPLETED entries, in completion order (oldest first)
	byKey map[receiptKey]*receipt

	// clients holds one entry for every client newClient has ever minted, for
	// as long as the engine lives: no client is ever retired (r26 finding 2).
	// clientSeq is the mint counter clientName spells.
	clients   map[string]*receiptClient
	clientSeq uint64

	cap int
	age time.Duration
}

// newReceiptTable builds an empty table. h is nil outside tests, and the
// table's clock is then time.Now: a receipt's lifetime is real elapsed time,
// never the session's event clock (which a test freezes and a golden pins).
func newReceiptTable(h *receiptHooks) *receiptTable {
	rt := &receiptTable{
		byKey:   map[receiptKey]*receipt{},
		clients: map[string]*receiptClient{},
		cap:     receiptCap,
		age:     receiptAge,
	}
	if h != nil {
		rt.hooks = *h
	}
	if rt.hooks.now == nil {
		rt.hooks.now = time.Now
	}
	return rt
}

func (rt *receiptTable) now() time.Time { return rt.hooks.now() }

// horizon is RetryHorizon's answer; cap and age are fixed at construction and
// never mutate afterward, so this needs no lock.
func (rt *receiptTable) horizon() RetryHorizon {
	return RetryHorizon{Commands: rt.cap, Age: rt.age}
}

// newClient mints a client id unique in this table's life (Engine.NewClientID)
// and starts keeping a mark for it, for as long as the engine lives: minting
// never fails and no client is ever retired (the package doc's "Bound and
// eviction", r26 finding 2).
func (rt *receiptTable) newClient() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.clientSeq++
	name := clientName(rt.clientSeq)
	rt.clients[name] = &receiptClient{}
	return name
}

// clientName is how a minted client is spelled: the table's own format, and
// the only place a client id is built.
func clientName(seq uint64) string { return "c-" + strconv.FormatUint(seq, 10) }

// clientLocked is the client half of a command's identity: the live client's
// own record, or ErrBadRequest for one this table never minted. No client is
// ever retired, so a name this table did mint always has one.
func (rt *receiptTable) clientLocked(name string) (*receiptClient, error) {
	if st, ok := rt.clients[name]; ok {
		return st, nil
	}
	return nil, fmt.Errorf("%w: client %q was not minted by this engine", ErrBadRequest, name)
}

// parseCommand is c's identity in the table, or the ErrBadRequest a malformed
// id is: see the package doc's "Identity". The client is checked against the
// minted ones under the table's mutex (clientLocked), not here.
func parseCommand(c Command) (receiptKey, error) {
	if c.Client == "" {
		return receiptKey{}, fmt.Errorf("%w: a command needs a client id", ErrBadRequest)
	}
	n, err := strconv.ParseUint(c.ID, 10, 64)
	if err != nil || n == 0 || c.ID != strconv.FormatUint(n, 10) {
		return receiptKey{}, fmt.Errorf("%w: command id %q is not a positive decimal integer", ErrBadRequest, c.ID)
	}
	return receiptKey{client: c.Client, id: n}, nil
}

// admit is the table's one decision about a command, made under its mutex and
// with the mutex released again before anything runs: either this caller owns
// a fresh reservation (mine), or it found an entry — running, or completed and
// ready to replay — or the id is refused.
func (rt *receiptTable) admit(key receiptKey, hash string) (found *receipt, mine, running bool, err error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.evictAgedLocked(rt.now())
	st, err := rt.clientLocked(key.client)
	if err != nil {
		return nil, false, false, err
	}
	if r, ok := rt.byKey[key]; ok {
		if r.hash != hash {
			return nil, false, false, fmt.Errorf("%w: command %s was already sent with a different payload", ErrBadRequest, key)
		}
		return r, false, !r.completed, nil
	}
	if key.id <= st.mark {
		return nil, false, false, fmt.Errorf("%w: command %s is past the retry horizon", ErrUnknownCommand, key)
	}
	r := &receipt{hash: hash, done: make(chan struct{})}
	rt.byKey[key] = r
	return r, true, false, nil
}

// finish completes a reservation whose command returned: the result is
// published and done closed in one locked section, and the entry is kept —
// or, for a gate refusal, forgotten, so the id goes back to being unseen (the
// package doc's "What is stored"). It reports whether the refusal was a gate
// one, so the CALLER can invoke afterForget itself, after marking its own
// reservation complete — publication (this method) and the post-publication
// test hook are two different things on purpose: a Goexit or a panic from
// afterForget must never find the reservation still open by the caller's own
// bookkeeping, or its deferred abort would close r.done a second time (r26
// finding 4).
func (rt *receiptTable) finish(key receiptKey, r *receipt, result any, err error) (gateRefused bool) {
	refused := gateRefusal(err)
	rt.mu.Lock()
	rt.publishLocked(key, r, result, err, !refused)
	rt.mu.Unlock()
	return refused
}

// abort completes a reservation whose command did not return at all — a panic
// (or a runtime.Goexit) on the way through. The outcome is stored, never
// forgotten: the command may already have mutated state, so the id must never
// run again, and a client that wants another attempt sends a new one. zero is
// the owning call's own zero result, so a replay's type assertion still holds.
func (rt *receiptTable) abort(key receiptKey, r *receipt, zero any) {
	rt.mu.Lock()
	rt.publishLocked(key, r, zero, ErrCommandAborted, true)
	rt.mu.Unlock()
}

// publishLocked is the one place a reservation is resolved: the result is
// written, the entry kept (and the count bound re-applied) or forgotten, and
// done closed — all under the table's mutex, so nobody can see a completed
// receipt whose channel is still open, or an entry in the table whose result
// is not there yet.
func (rt *receiptTable) publishLocked(key receiptKey, r *receipt, result any, err error, keep bool) {
	r.result, r.err, r.completed = result, err, true
	switch {
	case !keep:
		rt.forgetLocked(key, r)
	case rt.byKey[key] == r:
		// Only the table's own entry is kept, never one a race might have put
		// at key since — the same care forgetLocked takes. Nothing removes an
		// open reservation but its owner, so this holds today; it is here so
		// that a receipt whose entry has gone is still completed for its
		// waiters rather than kept in an order nothing can look up.
		r.at = rt.now()
		rt.order = append(rt.order, key)
		rt.pruneCountLocked()
	}
	close(r.done)
}

// forgetLocked undoes a reservation when its call ended in a gate refusal
// (gateRefusal): the id goes back to being unseen, for the next call to
// attempt fresh. It only ever removes r itself — never a different reservation
// a race might have put at key since — and an open reservation is in no
// eviction order, so the entry is all there is to remove.
func (rt *receiptTable) forgetLocked(key receiptKey, r *receipt) {
	if rt.byKey[key] == r {
		delete(rt.byKey, key)
	}
}

// pruneCountLocked holds the count bound after a store: the oldest COMPLETED
// entry goes as soon as one past the cap is kept, so the table is never over
// its advertised bound (r24 finding 8).
func (rt *receiptTable) pruneCountLocked() {
	for len(rt.order) > rt.cap {
		rt.evictOldestLocked()
	}
}

// evictAgedLocked drops every COMPLETED entry past the age bound, on the
// table's own clock, read once by the caller so a single pass is
// self-consistent. It walks the whole order rather than stopping at the front:
// insertion order is completion order, and a clock that went backwards — only
// a test's can — would otherwise leave a future-dated entry in front of
// entries that really have expired (r24 finding 6).
func (rt *receiptTable) evictAgedLocked(now time.Time) {
	kept := rt.order[:0]
	for _, key := range rt.order {
		r, ok := rt.byKey[key]
		if !ok {
			continue
		}
		if now.Sub(r.at) > rt.age {
			delete(rt.byKey, key)
			rt.markEvictedLocked(key)
			continue
		}
		kept = append(kept, key)
	}
	rt.order = kept
}

// evictOldestLocked drops the table's oldest completed entry and remembers it
// as its client's high-water mark.
func (rt *receiptTable) evictOldestLocked() {
	key := rt.order[0]
	rt.order = rt.order[1:]
	delete(rt.byKey, key)
	rt.markEvictedLocked(key)
}

// markEvictedLocked records that key.id (and everything at or below it, by
// construction: a client's ids are evicted in the order it minted them) is
// now unknowable for key.client. A client that has been retired has no mark to
// keep — every id of its is already ErrUnknownCommand.
func (rt *receiptTable) markEvictedLocked(key receiptKey) {
	st, ok := rt.clients[key.client]
	if !ok {
		return
	}
	if key.id > st.mark {
		st.mark = key.id
	}
}

// gateRefusal reports whether err is a refusal about the engine simply not
// admitting anything right now — the log's outbox has no room, the door is
// shut for a reason that has nothing to do with this command's own
// arguments, or a Set never reached the provider at all because its ctx was
// already dead — rather than an answer about the specific command: see the
// package doc's "What is stored, and what is left retryable".
//
// It is DERIVED from classify (control.go), the one table Code is also built
// from, so the two can never again disagree about one error the way r28
// finding 1 found them disagreeing over agent.ErrNotInTurn and a Set's dead
// ctx: a gate refusal is exactly the errors classify marks !stored.
func gateRefusal(err error) bool {
	return err != nil && !classify(err).stored
}

// receiptHash is one command's payload hash: its method name, so that no two
// different Control methods can ever collide on the same client/id even if a
// client reused one (a resend and the record it matches are always for the
// same method, which is what makes the type assertion in replayReceipt safe
// without a runtime panic), followed by every argument that makes the request
// what it is, each already rendered to text by the caller — never a raw %v,
// which would let a pointer argument's address rather than its value decide
// the hash (EditQueued's expectedVersion).
//
// The encoding is INJECTIVE: every part is tagged and length-prefixed, and the
// number of parts is written too, so no two different requests can spell
// themselves the same way. A separator alone cannot do that — with one,
// Set(config, id="a", value="b\x00c") and Set(config, id="a\x00b", value="c")
// are the same bytes, and the second would be answered as a resend of the
// first rather than with the ErrBadRequest it is (r24 finding 4). Parts whose
// own shape is structured (answerSpelling) encode themselves with the same
// primitives, so the property holds all the way down.
//
// nil and empty are EQUIVALENT everywhere in this encoding: a nil map hashes
// as an empty one and a nil slice as an empty one. A resend that has been
// through a codec on the way back in (S2's socket) may turn one into the
// other, and a client that changed nothing must not be told ErrBadRequest.
func receiptHash(method string, parts ...string) string {
	h := sha256.New()
	hashString(h, method)
	hashCount(h, len(parts))
	for _, p := range parts {
		hashString(h, p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashString, hashCount and hashBool are the encoding's three primitives: a
// type tag, then the value, with every string's length in front of it. Writes
// to a hash.Hash and a strings.Builder never fail, which is why the error is
// dropped here and nowhere else.
func hashString(w io.Writer, s string) {
	_, _ = fmt.Fprintf(w, "s%d:%s", len(s), s)
}

func hashCount(w io.Writer, n int) {
	_, _ = fmt.Fprintf(w, "n%d;", n)
}

func hashBool(w io.Writer, b bool) {
	_, _ = fmt.Fprintf(w, "b%t;", b)
}

// versionSpelling is EditQueued's expectedVersion as the hash sees it: no
// decimal is spelled "*", so nil is distinguishable from every version.
func versionSpelling(v *int) string {
	if v == nil {
		return "*"
	}
	return strconv.Itoa(*v)
}

// answerSpelling is Answer's AskAnswer as the hash sees it: every field, in
// receiptHash's own encoding, with Answers' keys in sorted order (the hash's
// own promise, not fmt's) and the COUNT of the keys and of each key's values
// written beside them. Without those counts {"q": {"a b"}} and
// {"q": {"a", "b"}} spell themselves the same way, and one client's answer
// would be replayed for another's (r24 finding 4).
func answerSpelling(a agent.AskAnswer) string {
	keys := make([]string, 0, len(a.Answers))
	for k := range a.Answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	hashString(&b, a.OptionID)
	hashBool(&b, a.Cancel)
	hashBool(&b, a.Skip)
	hashBool(&b, a.Accept)
	hashBool(&b, a.Reject)
	hashCount(&b, len(keys))
	for _, k := range keys {
		hashString(&b, k)
		vals := a.Answers[k]
		hashCount(&b, len(vals))
		for _, v := range vals {
			hashString(&b, v)
		}
	}
	return b.String()
}

// cloneReceiptResult gives v its own copy of anything a stored result would
// otherwise share with the call that produced it or with another resend:
// SubmitResult.Queued is the one pointer field among the fifteen commands'
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

// replayReceipt is a completed entry as a duplicate gets it: the first call's
// own result and error, with anything shared copied out.
func replayReceipt[T any](r *receipt) (T, error) {
	var zero T
	res, ok := r.result.(T)
	if !ok {
		// Unreachable: receiptHash embeds the method name, so a hash match is
		// always for the same T. Refused rather than panicked on, in case that
		// ever stops being true.
		return zero, fmt.Errorf("%w: that command id belongs to another command", ErrBadRequest)
	}
	return cloneReceiptResult(res), r.err
}

// withSyncReceipt is the entry hook for a synchronous Control method (one
// documented as "waits on nothing"): c.IsZero() runs run and touches the
// table not at all, exactly today's behaviour for craze prompt, which never
// mints a command. Otherwise the id is reserved under the table's mutex,
// which is RELEASED while run makes the command — so a stalled command blocks
// nobody but a same-id duplicate, which is refused at once rather than made to
// wait (the package doc's "The table").
func withSyncReceipt[T any](rt *receiptTable, c Command, hash string, run func() (T, error)) (T, error) {
	var zero T
	if c.IsZero() {
		return run()
	}
	key, err := parseCommand(c)
	if err != nil {
		return zero, err
	}
	r, mine, running, err := rt.admit(key, hash)
	if err != nil {
		return zero, err
	}
	if !mine {
		if running {
			if rt.hooks.foundOpen != nil {
				rt.hooks.foundOpen()
			}
			// Never a wait: this method is one a client may call from the
			// primary's own reader, and the answer has to come back now. It is
			// retryable and nothing is stored, so the id is exactly as it was.
			// ErrCommandInProgress — never ErrUnavailable — because "resend the
			// SAME id" is a different instruction from a gate refusal's "same id
			// or a new one, nothing ran" (Command's own doc, r26 finding 1).
			return zero, fmt.Errorf("%w: command %s is still running", ErrCommandInProgress, key)
		}
		return replayReceipt[T](r)
	}
	// The completion is installed before anything can go wrong: a panic must
	// resolve this reservation rather than strand it (the package doc's
	// "Completion is not optional").
	completed := false
	defer func() {
		if !completed {
			rt.abort(key, r, zero)
		}
	}()
	res, rerr := run()
	refused := rt.finish(key, r, cloneReceiptResult(res), rerr)
	// completed is set the instant publication is done, BEFORE the
	// post-publication test hook runs: a Goexit or panic from afterForget must
	// find the reservation already resolved, or this defer would call abort
	// and close r.done a second time (r26 finding 4).
	completed = true
	if refused && rt.hooks.afterForget != nil {
		rt.hooks.afterForget()
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
// Stop, Set, Interject): c.IsZero() runs run untouched, as above. Otherwise
// the id is reserved under the table's mutex, released before run makes its
// provider or session call — the table's mutex is never held across anything —
// and a duplicate that finds the reservation still open waits on it, honouring
// its ctx, and returns the first call's result without disturbing it.
func withBlockingReceipt[T any](ctx context.Context, rt *receiptTable, c Command, hash string, run func() (T, error)) (T, error) {
	var zero T
	if c.IsZero() {
		return run()
	}
	key, err := parseCommand(c)
	if err != nil {
		return zero, err
	}
	r, mine, running, err := rt.admit(key, hash)
	if err != nil {
		return zero, err
	}
	if !mine {
		if !running {
			// A COMPLETED entry replays AT ONCE, whatever the duplicate's own
			// ctx: waitReceipt's select is for an OPEN reservation only, never a
			// completed one — r.done and a duplicate's own already-canceled ctx
			// both being closed would otherwise let select nondeterministically
			// hand back context.Canceled instead of the stored result (r26
			// finding 3).
			return replayReceipt[T](r)
		}
		if rt.hooks.foundOpen != nil {
			rt.hooks.foundOpen()
		}
		return waitReceipt[T](ctx, r)
	}
	completed := false
	defer func() {
		if !completed {
			rt.abort(key, r, zero)
		}
	}()
	res, rerr := run()
	refused := rt.finish(key, r, cloneReceiptResult(res), rerr)
	// See withSyncReceipt's identical comment: completed is set BEFORE
	// afterForget runs, so a Goexit or panic from that hook cannot make this
	// defer double-close r.done (r26 finding 4).
	completed = true
	if refused && rt.hooks.afterForget != nil {
		rt.hooks.afterForget()
	}
	return res, rerr
}

// withBlockingReceiptErr is withBlockingReceipt for a method whose only
// result is its error.
func withBlockingReceiptErr(ctx context.Context, rt *receiptTable, c Command, hash string, run func() error) error {
	_, err := withBlockingReceipt(ctx, rt, c, hash, func() (struct{}, error) { return struct{}{}, run() })
	return err
}

// waitReceipt is a blocking command's duplicate against an OPEN reservation,
// parked on r.done with its own ctx: whichever ends the wait first, the
// owner's entry is untouched. Callers only reach it for a reservation that is
// still running — withBlockingReceipt replays a completed one directly,
// never through here (r26 finding 3).
//
// A wait that ends on the DUPLICATE's own context is ErrCommandInProgress
// wrapping that context's error (r31 finding 2), never the bare context error.
// This invocation ran nothing, mutated nothing and stored nothing, and the
// owner's reservation is still open: the instruction that fits is
// in_progress's — "resend THE SAME id" — where a bare context error classifies
// `aborted`, which tells a client the opposite (its command may already have
// happened; use a NEW id if it still wants it), and a new id here would layer a
// second Interject or Set on top of the one the owner is still executing.
// classify matches ErrCommandInProgress ahead of its context case, so the code
// is in_progress, and the wrap keeps errors.Is(err, context.Canceled /
// DeadlineExceeded) true for a caller that matches on those.
func waitReceipt[T any](ctx context.Context, r *receipt) (T, error) {
	var zero T
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	select {
	case <-r.done:
		return replayReceipt[T](r)
	case <-done:
		return zero, fmt.Errorf("%w: %w", ErrCommandInProgress, ctx.Err())
	}
}
