package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/journal"
)

// The ask registry (plan 021 §3.6): one place where every request that blocks
// on a decision — a permission, a question, a plan — is opened, answered and
// ended, with a sequenced event for each ending.
//
// Before it, asks were a private map in live.go and most ways one could end
// emitted nothing at all: a valid answer, a cancel, a turn that ended
// underneath a parked request, a close, and every automatic resolution were
// invisible to the stream (§2.3's table). A socket client cannot be shown what
// was never recorded, and a second client cannot answer what it cannot list, so
// the registry is a provider-neutral component in internal/agent, beside
// EventLog: the goroutines that block on a decision live below the provider
// seam, and the clients above it answer through the engine.
//
// What it guarantees:
//
//   - One ending per ask, exactly once. Whatever resolves an ask first wins —
//     an answer, the asking call's context, a cancel, its turn's end, the
//     close — and every later attempt is ErrAlreadyResolved with nothing
//     emitted.
//   - An opening always precedes its ending. Every event it writes goes
//     through EventLog.Enqueue *under its own mutex*, so the order of its
//     events is the order of its state.
//   - A validated answer never loses the ask. An answer that does not fit is
//     ErrBadAnswer, emits nothing and leaves the ask open — where the baseline
//     cancelled the request before it validated the option id.
//   - Any number of asks may be open at once, from any goroutine, and no ask
//     costs a goroutine of its own: the asking call's context is watched inside
//     Wait (the caller's own goroutine), never by a watcher the registry starts.
//
// # Locks
//
// registry.mu guards everything. It is never held across a call that blocks — a
// provider call, Publish, Flush, a channel send that can block, file I/O — and
// it is never nested with a session's or the engine's mutex in either
// direction, because the registry calls nothing but its log and its clock. The
// one lock it takes under its own is the log's outbox mutex, a strict leaf
// (EventLog's comment), so the order is registry.mu → outboxMu. A waiter is
// woken by closing a channel under registry.mu, which blocks on nothing.
//
// # Turns
//
// The registry owns the ask-side turn lifecycle. A session notifies it, in
// order, with BeginTurn, CancelTurn and EndTurn, from the places it moves its
// own notion of the current turn, and Open consults **only registry state** —
// it calls neither the session nor the transport, so the check and the insert
// are one atomic section. That is what the old two-step park could not do: it
// read the ACP client's turn counter and the session's cancelled-turn mark
// under the session's lock, which is not the lock the insert happens under.

const (
	// keptAsks is how many terminal records the registry keeps — endings, not
	// ids, because one id can end more than once (keepLocked). Past it the
	// oldest is evicted and an answer naming it is ErrAlreadyResolved rather
	// than ErrUnknownAsk, decided from what its namespace issued: its counter,
	// or a reservation (05-protocol.md pins that distinction: unknown means the
	// id was never issued).
	keptAsks = 256
	// keptReservations is how many ids adopted AHEAD of their counter the
	// registry remembers (accountForLocked). A reservation is two promises: a
	// mint steps over that number when its counter reaches it, and an answer
	// naming it is already_resolved rather than unknown even once its record
	// has been evicted.
	//
	// How many a caller adopts is the caller's choice, so the list is bounded
	// like every other one here: past the bound the oldest reservation is
	// forgotten and its id reads as a custom one — known only while its record
	// is kept. Nothing is corrupted by forgetting one, because a mint still
	// skips an id an OPEN ask holds, and an id whose ask has ended is free to
	// be adopted — and so to be minted — again.
	keptReservations = 256
	// maxAskNumber is the largest number an id can spell and still be one this
	// registry could have minted (canonicalID). Above it a numeric id is a
	// CUSTOM id, exactly like "card-1": adopted as a name, accounted for by no
	// counter, and known only while its record is kept.
	//
	// The line is drawn where a counter stops being able to account for a
	// number. A counter starts at zero and only ever advances by one, and each
	// step is one ask really created — an entry, a channel, and at least one
	// event through the log — so 2^62 steps is more than a century at one per
	// nanosecond. No mint can reach the line, which is what makes "no later
	// mint reproduces an adopted id" a promise the registry can keep, and
	// uint64 keeps a further 3·2^62 above it so no arithmetic here can wrap.
	// Without the line, adopting the signed maximum overflowed the counter and
	// the next mint published "ask--9223372036854775808".
	maxAskNumber = uint64(1) << 62
	// keptTurns is how many retired turn tokens keep their exact ending. A
	// token older than that reads as ended, which is what every retired token
	// is; only the choice between "cancelled" and "turn_ended" is lost, and
	// only for a handler goroutine delayed past keptTurns turns.
	keptTurns = 64
	// hiddenMark separates the two id namespaces every kind has.
	//
	// An ask whose opening **is** published takes the next VISIBLE number —
	// perm-7, ask-7, plan-7 — which is the id a user, a golden and `craze
	// prompt --json` see. An ask whose opening is **never** published takes the
	// next HIDDEN number, spelled perm-x7, from a counter of its own.
	//
	// The split is what keeps `--json` byte-identical to the baseline's apart
	// from seq: the baseline spent a visible number exactly when it published an
	// opening — it numbered a request only after it had decided to park one, and
	// its Force path numbered nothing at all — so a request nobody was ever
	// shown must not renumber the ones they were. A question delayed until ACP called it stale would otherwise
	// consume ask-1 in an EventAsk that --json drops, and print the next
	// ordinary question as ask-2 where the baseline printed ask-1.
	hiddenMark = "x"
)

var (
	// ErrBadAnswer is an answer that does not fit its ask: an option the
	// request never offered, an empty permission answer that does not say it
	// cancels, a question answer naming a question or an option the request
	// does not have, an empty answer to a question that did ask something, a
	// plan answered with neither accept nor reject, or any answer carrying
	// another kind's fields. Nothing is emitted and the ask stays open, so the
	// agent is not left waiting for an Esc because a client mis-addressed one
	// call (05: bad_request).
	ErrBadAnswer = errors.New("agent: that answer does not fit the ask")
	// ErrAlreadyResolved answers an ask that already has an ending: answered,
	// cancelled, ended with its turn or closed — and an id whose terminal
	// record has been evicted, which is the same fact with the details
	// forgotten (05: already_resolved).
	ErrAlreadyResolved = errors.New("agent: that ask is already resolved")
	// ErrUnknownAsk is an id this incarnation never issued (05: unknown_ask).
	// Ask ids are scoped to the incarnation, as sequence numbers are (SD-22).
	ErrUnknownAsk = errors.New("agent: no such ask")
	// ErrAskUnavailable refuses an Answer before it mutates anything because
	// the event log's outbox has no room for the event it would cause — the
	// rejectable-admission rule of §3.3, which the engine spells
	// engine.ErrUnavailable and maps to the code "unavailable". internal/agent
	// cannot see that sentinel (the engine imports this package, never the
	// reverse), so this is its own, and engine.Code has an arm of its own for
	// it answering the same "unavailable".
	ErrAskUnavailable = errors.New("agent: the event log is backed up")
	// ErrAskIDInUse refuses an Open that adopts an id another open ask already
	// holds. It is a caller's own bug — a test emitting two cards with one id —
	// and it is refused rather than allowed to steal the first ask's answers:
	// nothing is minted, nothing is enqueued, and the ask holding that id is
	// untouched. An id whose ask has ENDED is free again, which is what lets
	// tui.Stub's tests raise a second card with the id the first one had.
	ErrAskIDInUse = errors.New("agent: that ask id is already open")
)

// AskKind is what is being asked. The three are the three requests a provider
// can block on today; a fourth would be a new opening event as well as a new
// kind here.
type AskKind string

const (
	AskPermission AskKind = "permission"
	AskQuestion   AskKind = "question"
	AskPlan       AskKind = "plan"
)

// idPrefix is the kind's half of an ask id. The spellings are today's and are
// pinned by goldens and by the --json fixtures: perm-N, ask-N, plan-N. A kind
// the registry does not know spells its own name, so an id is always
// "<something>-N" and never "-N".
func (k AskKind) idPrefix() string {
	switch k {
	case AskPermission:
		return "perm"
	case AskQuestion:
		return "ask"
	case AskPlan:
		return "plan"
	default:
		return string(k)
	}
}

// kindForPrefix is idPrefix backwards, for reading an id the registry no
// longer holds a record for.
func kindForPrefix(prefix string) (AskKind, bool) {
	switch prefix {
	case "perm":
		return AskPermission, true
	case "ask":
		return AskQuestion, true
	case "plan":
		return AskPlan, true
	default:
		return "", false
	}
}

// AskStatus is whether an ask is still waiting for a decision.
type AskStatus string

const (
	AskOpen     AskStatus = "open"
	AskResolved AskStatus = "resolved"
)

// AskOutcome is how an ask ended. Every ask gets exactly one.
type AskOutcome string

const (
	// AskAnswered: a client answered it through Answer, skip included.
	AskAnswered AskOutcome = "answered"
	// AskCancelled: it was cancelled rather than answered — by a client's
	// explicit cancel, by the turn being cancelled, by the asking call's own
	// context ending, by policy that had nothing to choose, or because the log
	// had no room to raise it at all. By says which.
	AskCancelled AskOutcome = "cancelled"
	// AskTurnEnded: the turn it belonged to ended underneath it.
	AskTurnEnded AskOutcome = "turn_ended"
	// AskClosing: the session is closing.
	AskClosing AskOutcome = "closing"
	// AskAutomatic: craze's approval policy answered it without a user
	// (ApprovalPolicy).
	AskAutomatic AskOutcome = "automatic"
)

// Who ended an ask, on AskUpdate.By and AskRecord.By. The outcome says what
// happened; this says whose doing it was, which is the part a transcript and a
// journal reader need to tell "the user said no" from "the turn went away".
const (
	// AskByClient: a client's Answer, whether it chose an option, skipped, or
	// cancelled deliberately.
	AskByClient = "client"
	// AskByCall: the asking call's own context ended, so the request is gone
	// whatever anyone answers.
	AskByCall = "call"
	// AskByCancel: the turn was cancelled (CancelTurn), which answers every
	// open ask, whatever turn it belongs to, and 05's rule that a cancel is
	// accepted whenever an ask is pending.
	AskByCancel = "cancel"
	// AskByTurn: the ask's own turn ended (EndTurn).
	AskByTurn = "turn"
	// AskByClose: the registry closed.
	AskByClose = "close"
	// AskByPolicy: craze's approval policy (ApprovalPolicy) decided it.
	AskByPolicy = "policy"
	// AskByProvider: the provider answered the request before any handler of
	// craze's ran, and reported it afterwards (AnsweredEarly).
	AskByProvider = "provider"
	// AskByUnavailable: the log's outbox had no room to open it, so it was
	// never raised and the provider sees a cancel (§3.3's rejectable
	// admissions).
	AskByUnavailable = "unavailable"
)

// Approval is what craze does with one kind of ask when it arrives.
type Approval string

const (
	// ApprovalAsk parks the request until it is answered, the turn ends, a
	// cancel lands or the session closes — with no client too, which is the
	// point of separating policy from frontend presence (SD-26).
	ApprovalAsk Approval = "ask"
	// ApprovalAllow answers a permission with the option that allows it, and
	// cancels when the request offers none. Permissions only.
	ApprovalAllow Approval = "allow"
	// ApprovalFirstOption answers each of a question's questions with its
	// first option. Questions only.
	ApprovalFirstOption Approval = "first_option"
	// ApprovalAccept accepts a plan. Plans only.
	ApprovalAccept Approval = "accept"
)

// ApprovalPolicy is what the session does with each kind of ask, independently
// of whether a client is attached (SD-26). A headless host parks asks for a
// phone to answer later; `craze prompt` answers them itself, and says so
// explicitly rather than by having no frontend.
//
// A section left empty reads as ApprovalAsk: the conservative default is to
// wait for a human, never to decide for one.
type ApprovalPolicy struct {
	Permission Approval
	Question   Approval
	Plan       Approval
}

// EffectiveApproval is o's approval policy: the one it carries, or — for the
// callers that have not been given one — exactly today's behaviour derived
// from Force and Interactive (live.go's onPermission, onAskQuestion and
// onCreatePlan). Force allows permissions; a session with no frontend answers
// questions with their first option and accepts plans; everything else parks.
func EffectiveApproval(o Options) ApprovalPolicy {
	if p := o.Approval; p != nil {
		return ApprovalPolicy{
			Permission: orAsk(p.Permission),
			Question:   orAsk(p.Question),
			Plan:       orAsk(p.Plan),
		}
	}
	p := ApprovalPolicy{Permission: ApprovalAsk, Question: ApprovalAsk, Plan: ApprovalAsk}
	if o.Force {
		p.Permission = ApprovalAllow
	}
	if !o.Interactive {
		p.Question = ApprovalFirstOption
		p.Plan = ApprovalAccept
	}
	return p
}

func orAsk(a Approval) Approval {
	if a == "" {
		return ApprovalAsk
	}
	return a
}

// TurnToken names one turn to the registry. It is provider-neutral on purpose:
// the registry mints them itself (BeginTurn), so no session can hand it a
// counter it reuses, and no two turns can ever be the same token.
//
// **A nonzero token carries the identity of the registry that minted it**, so
// one session's turn can never name another's: two registries both mint turn 1,
// and without that identity `B.EndTurn(tokenFromA)` would end B's own turn-1
// asks and `B.Open(ctx, tokenFromA, …)` would park against A's live state. A
// foreign token is refused rather than looked up.
//
// **The zero value is the no-turn token**, and it is what a request that
// belongs to no turn of craze's own is opened against: one that arrived between
// turns, or during a turn the agent started itself. Such an ask ends by an
// answer, a cancel, its asking call's context or the close — never by EndTurn,
// which is what "turn_ended applies only to asks opened against a token whose
// EndTurn has been notified" means (§3.6). It belongs to no registry in
// particular: every registry reads it as its own no-turn token, which is what
// makes it the token a caller with no turn of its own passes.
type TurnToken struct{ reg, n uint64 }

// NoTurn reports whether t is the no-turn token.
func (t TurnToken) NoTurn() bool { return t.n == 0 }

// String is the token as a test or a diagnostic prints it.
func (t TurnToken) String() string {
	if t.n == 0 {
		return "no-turn"
	}
	return "turn-token-" + strconv.FormatUint(t.n, 10)
}

// AskBody is one ask's opening exactly as it was offered, kept verbatim —
// duplicate option kinds included, because grok really does offer two
// allow_once options and craze must answer with the id the agent named
// (SD-25). Exactly one of the three is set, the one that matches the ask's
// kind.
type AskBody struct {
	Permission *PermissionEvent
	Question   *QuestionEvent
	Plan       *PlanEvent
}

// clone is a deep copy: the registry keeps one of its own, so nothing a caller
// still holds can change what was offered, and hands out copies for the same
// reason.
func (b AskBody) clone() AskBody {
	return AskBody{
		Permission: clonePermissionEvent(b.Permission),
		Question:   cloneQuestionEvent(b.Question),
		Plan:       clonePlanEvent(b.Plan),
	}
}

func clonePermissionEvent(p *PermissionEvent) *PermissionEvent {
	if p == nil {
		return nil
	}
	c := *p
	c.Options = slices.Clone(p.Options)
	return &c
}

func cloneQuestionEvent(q *QuestionEvent) *QuestionEvent {
	if q == nil {
		return nil
	}
	c := *q
	if q.Questions != nil {
		c.Questions = make([]Question, len(q.Questions))
		for i, one := range q.Questions {
			one.Options = slices.Clone(one.Options)
			c.Questions[i] = one
		}
	}
	c.Answers = cloneAnswers(q.Answers)
	return &c
}

func clonePlanEvent(p *PlanEvent) *PlanEvent {
	if p == nil {
		return nil
	}
	c := *p
	c.Todos = slices.Clone(p.Todos)
	return &c
}

// cloneAnswers copies an answer map, keeping a nil list apart from an empty
// one: the codec does too, and a question answered with nothing is not the
// same as one not answered at all.
func cloneAnswers(m map[string][]string) map[string][]string {
	if m == nil {
		return nil
	}
	c := make(map[string][]string, len(m))
	for k, v := range m {
		c[k] = slices.Clone(v)
	}
	return c
}

// AskRequest is one ask as its asking call offers it.
type AskRequest struct {
	// Kind is what is being asked; it must match Body.
	Kind AskKind
	// Body is the opening's payload, exactly as offered. The registry fills in
	// the id it minted and publishes that payload otherwise unchanged, so the
	// three opening events keep the shapes every consumer already renders.
	Body AskBody
	// ID adopts a caller-chosen id in place of minting one. It exists for
	// tui.Stub, whose tests emit card events with ids of their own at some
	// forty sites (plan item 22): the Stub routes the opening through the
	// registry and the test's id survives.
	//
	// An adopted id spends no counter of its own, but one that spells an id
	// this registry could have minted — "perm-7", "perm-x7" — is RESERVED in
	// that namespace (accountForLocked): the counter does not jump to it, and
	// a mint steps over it when the counter reaches it, so no later mint can
	// reproduce it. A reserved id counts as issued, so an answer naming it is
	// ErrAlreadyResolved once its record has been evicted — while a number
	// nobody adopted and no mint has reached is ErrUnknownAsk, however close to
	// the counter it looks.
	//
	// An id that spells neither — a name like "card-1", or a number past what
	// a counter could ever reach (maxAskNumber) — is only known for as long as
	// its record is kept: past the eviction bound an answer naming it is
	// ErrUnknownAsk rather than ErrAlreadyResolved, because nothing is left to
	// say it was ever issued.
	//
	// An id an OPEN ask already holds is ErrAskIDInUse; one whose ask has
	// ended may be adopted again.
	ID string
}

// AskAnswer is an answer to one ask. Each kind reads its own fields and
// refuses any other kind's, so a question's answer can never resolve a
// permission by accident:
//
//   - permission: OptionID names an option the request offered, or Cancel says
//     this is the deliberate cancel that today's empty option id means
//     (internal/cli/prompt.go's --permission-decision, and the TUI's Esc).
//   - question: Answers maps each question's id to option ids it offered — the
//     invented "OK" option's empty id included, since that is a real offered
//     option (live.go's questionsFromRequest) — or Skip skips the lot. A
//     question that asks nothing takes no answers at all.
//   - plan: exactly one of Accept and Reject.
//
// The zero AskAnswer is not a valid answer to anything but a question that
// asks nothing, which is deliberate: a plan's reject is the explicit Reject and
// not the absence of Accept, so a client that sends an empty answer gets
// ErrBadAnswer instead of silently rejecting the plan it meant to accept. A
// question with no questions of its own is the one ask an empty answer really
// answers — there is nothing else anyone could say about it.
type AskAnswer struct {
	OptionID string
	Cancel   bool
	Answers  map[string][]string
	Skip     bool
	Accept   bool
	Reject   bool
}

// AskUpdate is Event.Ask: one ask's ending, and the only event kind the
// registry adds. Every way an ask can end carries one, so a client that folds
// the stream — or reads the journal afterwards — sees what became of every
// request the agent made.
type AskUpdate struct {
	// ID is the ask's id, scoped to the log's incarnation (SD-22).
	ID string
	// Kind is what was asked.
	Kind AskKind
	// Outcome is how it ended and By whose doing.
	Outcome AskOutcome
	By      string
	// OptionID, Answers, Skip and Accepted are the answer, for an ending that
	// has one: the permission option chosen, the question's answers or its
	// skip, and whether a plan was accepted.
	OptionID string
	Answers  map[string][]string
	Skip     bool
	Accepted bool
	// Label is a short human label of what was chosen: the name of the
	// permission option, as it was offered. It is empty for every other kind
	// and every outcome that chose nothing — a client that wants to word a
	// cancelled question reads Kind and Outcome, not this.
	Label string
	// Body is the opening, set only when no opening was ever published: a
	// permission the policy allowed, a request the provider had already
	// answered, one the registry refused to raise at all. It makes such an
	// ending self-contained — the record says what was asked as well as what
	// became of it — without a forced `craze prompt --json` run gaining a
	// permission line it never had.
	Body *AskBody
}

// AskRecord is one ask as the registry holds it: what was asked, what became
// of it, and what became of the reply. Copies are what leave the registry;
// nothing hands out a pointer into its state.
type AskRecord struct {
	ID    string
	Kind  AskKind
	Token TurnToken
	// Body is the opening exactly as offered.
	Body AskBody
	// Status is open until an ending is written, resolved afterwards.
	Status  AskStatus
	Outcome AskOutcome
	By      string
	// Answer is the answer as it was validated, zero for an ask nobody
	// answered.
	Answer     AskAnswer
	OpenedAt   time.Time
	ResolvedAt time.Time
	// Consumed says the waiting call took the decision (Ask.Wait returned it).
	// Delivered says the provider's reply carried it, and Lost says why not
	// when something else — a cancelled reply that won the race — reached the
	// provider instead. The provider is the only one that can know the last
	// two, so it reports them (Report); a lost delivery is one journal diag
	// note, never a second ending, and it changes no outcome.
	Consumed  bool
	Delivered bool
	Lost      string
	// Incarnation is the log incarnation this ask's id belongs to (SD-22).
	Incarnation string
}

// clone is a deep copy of everything a caller could otherwise reach into.
func (r AskRecord) clone() AskRecord {
	c := r
	c.Body = r.Body.clone()
	c.Answer.Answers = cloneAnswers(r.Answer.Answers)
	return c
}

// AskReport is what became of a resolved ask's decision on its way back to the
// provider (Report). Fields are merged into the record: a false one says
// nothing, never "not any more".
type AskReport struct {
	// Consumed: the waiting call took the decision. Ask.Wait sets it itself,
	// so a caller has to report it only when it took the decision another way.
	Consumed bool
	// Delivered: the provider's reply carried the decision.
	Delivered bool
	// Lost: why the decision never reached the provider — ACP's own cancelled
	// reply won the race, the connection went — as text for the journal.
	Lost string
}

// AskSource is a session that has an ask registry: what a component above the
// provider seam needs in order to list and answer a session's asks. Like
// LogOwner and EventSource it is optional and found by a type assertion, so
// agent.Session itself does not change.
type AskSource interface {
	Asks() *AskRegistry
}

// askEntry is one ask inside the registry: the record, and the channel its
// waiters are woken by. The channel is closed, under the registry's mutex,
// exactly when the record becomes resolved.
type askEntry struct {
	rec  AskRecord
	done chan struct{}
	// published says an opening event was written for this ask, which is what
	// decides whether its ending carries the body.
	published bool
}

// turnState is what the registry knows about one minted turn token.
type turnState struct{ cancelled, ended bool }

// askRegistries numbers the registries built in this process, so every turn
// token can carry which one minted it (TurnToken). It is only ever compared,
// never published: the ids a client sees are the ask ids and the incarnation.
// It starts at one, so zero is no registry and the zero token is foreign to
// none.
var askRegistries atomic.Uint64

// AskRegistry is one session's asks (plan 021 §3.6). One is built beside the
// session's event log, and the session, the provider's handlers and the
// clients above the seam all meet here.
type AskRegistry struct {
	log *EventLog
	now func() time.Time
	// id is this registry's identity, immutable and set at construction. Every
	// nonzero token it mints carries it, and a token carrying another's is
	// refused (foreign).
	id uint64

	mu sync.Mutex
	// seq and hiddenSeq are a kind's two id counters (hiddenMark): seq numbers
	// the asks whose opening is published, hiddenSeq the ones nobody is ever
	// shown. Both count unsigned and only ever advance by one (mintLocked), so
	// neither can be made to wrap by an id somebody adopted.
	seq       map[AskKind]uint64
	hiddenSeq map[AskKind]uint64
	open      map[string]*askEntry
	order     []string
	// done is the ask each kept id names NOW — the most recent ending of an id
	// that has had several, which is the one Record and Answer answer for.
	done map[string]*askEntry
	// doneOrder is the eviction order of the terminal records, oldest ending
	// first. It holds entries rather than ids because one id can end more than
	// once: an adopted id whose ask has ended may be adopted again (tui.Stub's
	// tests do), and both endings are history.
	doneOrder []*askEntry
	// reserved is every id adopted ahead of its counter, with reservedOrder
	// its eviction order, oldest first (keptReservations).
	reserved      map[askNumber]struct{}
	reservedOrder []askNumber
	turnSeq       uint64
	turns         map[uint64]*turnState
	// retired is the retirement order of the turn tokens that have ended or
	// been cancelled, oldest first, so their exact ending is kept for a bounded
	// while (keptTurns).
	retired []uint64
	closed  bool
}

// NewAskRegistry builds a registry publishing into log and stamping its events
// from now. log must not be nil: an ask nobody can record is not one craze
// will park. now is the session's own clock (Clocked), so a golden that shows a
// time shows the session's injected one; nil is time.Now. It is read under the
// registry's mutex, so it must wait on nothing and take no lock the registry
// could be called under — all three sessions answer from time.Now or from a
// field set before the session runs.
func NewAskRegistry(log *EventLog, now func() time.Time) *AskRegistry {
	if now == nil {
		now = time.Now
	}
	return &AskRegistry{
		log:       log,
		now:       now,
		id:        askRegistries.Add(1),
		seq:       map[AskKind]uint64{},
		hiddenSeq: map[AskKind]uint64{},
		open:      map[string]*askEntry{},
		done:      map[string]*askEntry{},
		reserved:  map[askNumber]struct{}{},
		turns:     map[uint64]*turnState{},
	}
}

// foreign reports whether token was minted by a different registry. The zero
// value is every registry's no-turn token and is never foreign; anything else
// carrying another registry's identity names a turn this one has never heard of
// and never will.
func (r *AskRegistry) foreign(token TurnToken) bool {
	return !token.NoTurn() && token.reg != r.id
}

// BeginTurn mints the token for a turn that is starting. The session calls it
// where it takes its own turn up; every call is a new token, so a request from
// a turn that has been and gone can never be mistaken for one of the turn
// running now — which counter equality (ACP's TurnLive) cannot promise.
func (r *AskRegistry) BeginTurn() TurnToken {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turnSeq++
	t := TurnToken{reg: r.id, n: r.turnSeq}
	r.turns[t.n] = &turnState{}
	return t
}

// CancelTurn ends **every** open ask as cancelled, whatever turn it belongs
// to, and marks token so a request that arrives for it afterwards is refused
// rather than parked. It drains every open ask, as the baseline did by swapping
// the whole waiting map: a cancel is accepted whenever an ask is pending, with
// a turn or without one
// (05's gate table), and an ask left parked through a cancel would leave the
// agent waiting on a reply nobody will send.
//
// A cancel with no turn of craze's own — the no-turn token — still answers
// every open ask; it marks nothing, so nothing is refused afterwards. A later
// BeginTurn is a new token either way, so the next turn parks normally.
//
// **A foreign token does nothing at all**, neither marking a turn nor draining
// this registry's asks. "Cancel everything" is what the no-turn token asks for,
// deliberately and in this registry's own name; a token another session minted
// says nothing about this session, and taking it as the stronger meaning would
// let one session's cancel answer another's cards.
func (r *AskRegistry) CancelTurn(token TurnToken) {
	if r.foreign(token) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.markLocked(token, true)
	r.resolveAllLocked(func(*askEntry) bool { return true }, AskCancelled, AskByCancel)
}

// EndTurn ends every ask opened against token, and nothing else: an ask on the
// no-turn token, or on another turn's, is untouched. The session calls it as
// its turn finishes.
//
// EndTurn on the no-turn token does nothing at all. Asks opened between turns
// are exactly the ones no turn's ending may take away. Neither does one on a
// foreign token: another registry's turn ending is not this registry's, and
// token equality alone would end whichever of its own turns happened to carry
// the same number.
func (r *AskRegistry) EndTurn(token TurnToken) {
	if token.NoTurn() || r.foreign(token) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.markLocked(token, false)
	r.resolveAllLocked(func(e *askEntry) bool { return e.rec.Token == token }, AskTurnEnded, AskByTurn)
}

// Close ends every open ask as closing and refuses everything after. The
// session calls it as it closes, before the log's own close, so the endings are
// in the record: the log commits what its outbox still holds rather than
// dropping it (§3.3's close phases). It is idempotent.
func (r *AskRegistry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	r.resolveAllLocked(func(*askEntry) bool { return true }, AskClosing, AskByClose)
}

// Open parks an ask and answers with the handle its asking call waits on.
//
// ctx is the asking call's own context: when it ends, the ask resolves as
// cancelled by the call and the waiter returns at once. The context is watched
// inside Wait, on the waiting call's own goroutine, so no ask costs a goroutine
// — and so an ask whose opener never waits is bounded by the turn's lifecycle
// (CancelTurn, EndTurn, Close) rather than by its context.
//
// token is the turn the request belongs to, or the no-turn token for one that
// arrived between turns or during a turn the agent started itself. The check
// and the insert are one section over registry state alone; nothing here calls
// the session or the transport.
//
// **A refused Open still mints an id and writes exactly one self-contained
// ending** — an EventAsk carrying the body, with no opening before it — and
// answers with an ask that is already resolved, so its caller reads the outcome
// through the same Wait as any other. Its id comes from the **hidden** counter
// (hiddenMark), because nobody is ever shown its opening and a refusal must not
// renumber the cards that are shown. It is refused when the registry has
// closed (closing), when the turn was cancelled (cancelled, by cancel) or has
// ended (turn_ended), and when the log's outbox has no room to raise it
// (cancelled, by unavailable — the provider sees a cancel). The ending itself
// is a mandatory completion and is never refused for room.
//
// The only error it returns is ErrAskIDInUse, for an adopted id another open
// ask holds: that is a caller's bug, not a lifecycle refusal, and nothing is
// minted or written for it.
func (r *AskRegistry) Open(ctx context.Context, token TurnToken, req AskRequest) (*Ask, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	body := req.Body.clone()
	r.mu.Lock()
	defer r.mu.Unlock()
	// Whether the opening will be published is decided first, in this same
	// section, because it is also what decides which of the kind's two counters
	// the id comes from (hiddenMark).
	outcome, by, refused := r.refusalLocked(token)
	id, err := r.reserveLocked(req.Kind, req.ID, refused)
	if err != nil {
		return nil, err
	}
	setBodyID(req.Kind, body, id)
	e := r.newEntryLocked(id, req.Kind, token, body)
	if refused {
		r.retireLocked(e, outcome, by)
	} else {
		r.open[id] = e
		r.order = append(r.order, id)
		e.published = true
		r.log.Enqueue(r.opening(req.Kind, body, false, AskAnswer{}))
	}
	return &Ask{r: r, id: id, ctx: ctx, e: e}, nil
}

// Automatic resolves an ask craze's own approval policy answered, without ever
// parking it, and answers with the record its caller replies from. Everything
// is journaled by virtue of being events:
//
//   - a question or a plan writes the Auto opening today's headless path
//     already writes — answers or accepted filled in — **and** one automatic
//     ending, as one batch, so nothing can land between them. That opening is
//     published, so its id is a VISIBLE one (hiddenMark), exactly as the
//     baseline's nextID gave the headless path;
//   - a permission writes no opening at all and one automatic ending carrying
//     the body, so a forced `craze prompt --json` run gains no permission line;
//     its id is a hidden one, because the baseline's Force path spent no
//     number at all;
//   - a permission the policy could not answer — Force with no allow option
//     offered — is cancelled, by policy, with the body. Pass AskAnswer{Cancel:
//     true}, or any answer that does not fit the request: a policy that cannot
//     answer its own ask is a cancel. A question or a plan that degrades this
//     way publishes no opening either, and so is hidden too.
//
// It is a mandatory completion: the decision has been made and the provider is
// about to be told it, so the record is written whatever the outbox holds
// (§3.3's list of rejectable admissions has Open, not this).
//
// It adopts req.ID like Open, and unlike Open it has no error to refuse one
// with. An adopted id an OPEN ask already holds is therefore **minted over**:
// the record is written under a freshly minted id, the caller replies from the
// record it is handed — which carries the id the registry actually used — and
// the open ask keeps its id, its answers and its waiters. Refusing instead
// would lose the one record a decision already taken is owed, and adopting
// anyway would overwrite an open ask's entry and strand its waiter.
func (r *AskRegistry) Automatic(token TurnToken, req AskRequest, a AskAnswer) AskRecord {
	body := req.Body.clone()
	r.mu.Lock()
	defer r.mu.Unlock()
	ans, label, bad := validateAnswer(req.Kind, body, a)
	outcome, by := AskAutomatic, AskByPolicy
	if bad != nil || ans.Cancel {
		outcome, ans, label = AskCancelled, AskAnswer{Cancel: true}, ""
	}
	// Whether the Auto opening is published decides the id's namespace, and it
	// is decided here, in the section that publishes it.
	publishes := req.Kind != AskPermission && outcome == AskAutomatic
	id, err := r.reserveLocked(req.Kind, req.ID, !publishes)
	if err != nil {
		id = r.mintLocked(req.Kind, !publishes)
	}
	setBodyID(req.Kind, body, id)
	e := r.newEntryLocked(id, req.Kind, token, body)
	var batch []Event
	if publishes {
		e.published = true
		batch = append(batch, r.opening(req.Kind, body, true, ans))
	}
	batch = append(batch, r.retire(e, outcome, by, "", ans, label))
	r.log.Enqueue(batch...)
	return e.rec.clone()
}

// AnsweredEarly records a request the provider answered before any handler of
// craze's ever ran — a cancel that completed it where it lay, a request from a
// turn that had already gone, a connection that closed (internal/acp's
// runIncoming). The session cannot see those: no handler runs, so nothing
// parks, and at the baseline they left no trace at all (§2.3's table). ACP
// reports them instead, and each becomes one self-contained ending with the
// body and no opening, so no card is ever raised for a request nobody can
// answer.
//
// outcome and by are the caller's: cancelled, turn_ended or closing, by the
// provider. It always mints — a request answered before any handler ran never
// had a craze id to adopt — and always from the **hidden** counter
// (hiddenMark), because it publishes no opening: at the baseline such a request
// never reached the session's counter at all, and letting it spend a visible
// number would renumber the next card a user really sees. It is a mandatory
// completion, like every other ending.
func (r *AskRegistry) AnsweredEarly(token TurnToken, req AskRequest, outcome AskOutcome, by string) AskRecord {
	if outcome == "" {
		outcome = AskCancelled
	}
	if by == "" {
		by = AskByProvider
	}
	body := req.Body.clone()
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.mintLocked(req.Kind, true)
	setBodyID(req.Kind, body, id)
	e := r.newEntryLocked(id, req.Kind, token, body)
	r.retireLocked(e, outcome, by)
	return e.rec.clone()
}

// Answer validates and claims an ask in one section, and answers with the
// record its caller draws its row from. cause is the answering client's
// command, as Event.Cause spells it ("client/id", engine.Command.Cause), so a
// client that has already applied its own answer skips exactly that echo.
//
// The order of its refusals is the order of what a caller can do about them:
//
//   - an id no ask holds is ErrAlreadyResolved when this incarnation issued it
//     (a kept terminal record, or an evicted one the counter still accounts
//     for) and ErrUnknownAsk otherwise;
//   - an answer that does not fit is ErrBadAnswer, **with nothing emitted and
//     the ask still open**, because a mis-addressed call must not leave the
//     agent waiting;
//   - a valid answer the log has no room for is ErrAskUnavailable, refused
//     before anything is mutated, so a retry is a retry and not a second
//     answer.
func (r *AskRegistry) Answer(cause, id string, a AskAnswer) (AskRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.open[id]
	if !ok {
		return AskRecord{}, r.unknownLocked(id)
	}
	ans, label, err := validateAnswer(e.rec.Kind, e.rec.Body, a)
	if err != nil {
		return AskRecord{}, err
	}
	if !r.log.OutboxRoom() {
		return AskRecord{}, fmt.Errorf("%w: %s is still open", ErrAskUnavailable, id)
	}
	outcome := AskAnswered
	if ans.Cancel {
		outcome = AskCancelled
	}
	r.log.Enqueue(r.retire(e, outcome, AskByClient, cause, ans, label))
	return e.rec.clone(), nil
}

// Report records what became of a resolved ask's decision: whether the waiting
// call took it, whether the provider's reply carried it, and why not when
// something else reached the provider instead. A lost delivery is one journal
// diag note and changes no outcome — the ask was answered, and the record says
// both that and that the answer never arrived.
//
// An id the registry no longer holds a record for still writes the note: the
// diagnostic is the point, and a record evicted 256 asks later is not.
func (r *AskRegistry) Report(id string, rep AskReport) { r.report(nil, id, rep) }

// report is Report against a known entry, or — with a nil one — against
// whatever entry that id names now.
func (r *AskRegistry) report(e *askEntry, id string, rep AskReport) {
	r.mu.Lock()
	kind, outcome := AskKind(""), AskOutcome("")
	if e == nil {
		e = r.entryLocked(id)
	}
	if e != nil {
		if rep.Consumed {
			e.rec.Consumed = true
		}
		if rep.Delivered {
			e.rec.Delivered = true
		}
		if rep.Lost != "" {
			e.rec.Lost = rep.Lost
		}
		kind, outcome = e.rec.Kind, e.rec.Outcome
	}
	r.mu.Unlock()
	if rep.Lost == "" {
		return
	}
	r.log.Note(journal.DiagNote{Kind: journal.DiagAskLostDelivery, Fields: map[string]any{
		"id":      id,
		"kind":    string(kind),
		"outcome": string(outcome),
		"reason":  rep.Lost,
	}})
}

// Resolved is every terminal record the registry still keeps (keptAsks),
// **oldest ending first**: the order the asks were resolved in, whatever order
// they were opened in. It is what a session rebuilds "every answer I was given,
// in order" from — tui.Stub's Calls() — without keeping a second list beside
// the registry's own. Every record is a copy.
//
// One ending per ask, and **every** ending: an id that was adopted again once
// its first ask had ended is two asks and two entries here, in the order they
// ended, not one row overwritten by the other.
func (r *AskRegistry) Resolved() []AskRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AskRecord, 0, len(r.doneOrder))
	for _, e := range r.doneOrder {
		out = append(out, e.rec.clone())
	}
	return out
}

// Asks is every open ask, in the order they were opened: what asks.list
// answers with, and what State.PendingAsks and its head ask are read from.
// Every record is a copy.
func (r *AskRegistry) Asks() []AskRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AskRecord, 0, len(r.order))
	for _, id := range r.order {
		if e := r.open[id]; e != nil {
			out = append(out, e.rec.clone())
		}
	}
	return out
}

// Record is one ask by id, open or resolved, for as long as its record is kept
// (keptAsks). It is a copy, and it is named for the record rather than for the
// ask because Ask is the handle an asking call holds.
func (r *AskRegistry) Record(id string) (AskRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entryLocked(id)
	if e == nil {
		return AskRecord{}, false
	}
	return e.rec.clone(), true
}

// Ask is one open ask, as its asking call holds it: the id craze gave it, and
// the wait for its decision.
type Ask struct {
	r   *AskRegistry
	id  string
	ctx context.Context
	e   *askEntry
}

// ID is the ask's id.
func (a *Ask) ID() string { return a.id }

// Record is the ask as it stands now, a copy.
func (a *Ask) Record() AskRecord {
	a.r.mu.Lock()
	defer a.r.mu.Unlock()
	return a.e.rec.clone()
}

// Wait blocks until the ask is resolved and answers with its record. It
// returns at once for an ask that is resolved already — a refused Open among
// them — and it watches the asking call's context itself: when that ends with
// the ask still open, the ask resolves as cancelled by the call. An ending that
// was already made wins over a context that ends in the same instant, so a
// decision is never thrown away for a deadline that arrived with it.
//
// Taking the decision is what Consumed records; several waiters on one ask all
// get the same record.
func (a *Ask) Wait() AskRecord {
	select {
	case <-a.e.done:
	default:
		select {
		case <-a.e.done:
		case <-a.ctx.Done():
			a.r.cancelByCall(a)
			<-a.e.done
		}
	}
	a.r.mu.Lock()
	defer a.r.mu.Unlock()
	a.e.rec.Consumed = true
	return a.e.rec.clone()
}

// Report is Registry.Report bound to this ask and not to its id, which is what
// a provider's delivery report has to be: a reply that comes back long after
// the ask was resolved must not land on whatever holds that id now. Only an
// adopted id can be opened twice (tui.Stub's tests choose their own), and then
// an id lookup would mark the wrong ask delivered or journal the wrong kind.
//
// The note it writes is the same one, for the same reason: a lost delivery is
// one journal diag note, never a second ending, and it changes no outcome.
func (a *Ask) Report(rep AskReport) { a.r.report(a.e, a.id, rep) }

// cancelByCall resolves the ask its asking call gave up on. An ask that has
// been resolved in the meantime keeps the ending it has: whatever ended it
// first wins.
func (r *AskRegistry) cancelByCall(a *Ask) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The ask itself, not whatever holds its id now: an adopted id can be
	// opened again once this one has ended.
	e, ok := r.open[a.id]
	if !ok || e != a.e {
		return
	}
	r.log.Enqueue(r.retire(e, AskCancelled, AskByCall, "", AskAnswer{}, ""))
}

// newEntryLocked is a fresh open record for id.
func (r *AskRegistry) newEntryLocked(id string, kind AskKind, token TurnToken, body AskBody) *askEntry {
	return &askEntry{
		done: make(chan struct{}),
		rec: AskRecord{
			ID:          id,
			Kind:        kind,
			Token:       token,
			Body:        body,
			Status:      AskOpen,
			OpenedAt:    r.now(),
			Incarnation: r.log.Incarnation(),
		},
	}
}

// reserveLocked is where **every** ask id comes from: Open, a refused Open,
// Automatic and AnsweredEarly all go through it, so the registry has one id
// namespace per kind and per visibility, and nothing can mint an id an open ask
// already holds. Before it, only an explicit req.ID was checked against the
// open map, so a later mint could land on an adopted id, overwrite its entry
// and strand its waiter for ever.
//
// An adopted id an OPEN ask holds is ErrAskIDInUse: nothing is minted, nothing
// is enqueued, and the ask holding it keeps its answers and its waiters. One
// whose ask has ENDED is free again — tui.Stub's tests raise a second card with
// the id the first one had — and one that spells an id this registry could have
// minted is reserved in that namespace, so a later mint steps over it and
// unknownLocked still knows it was issued once its record has been evicted.
//
// hidden picks the kind's counter: false for an ask that will publish its
// opening, true for one nobody will ever be shown (hiddenMark).
func (r *AskRegistry) reserveLocked(kind AskKind, adopt string, hidden bool) (string, error) {
	if adopt == "" {
		return r.mintLocked(kind, hidden), nil
	}
	if _, open := r.open[adopt]; open {
		return "", fmt.Errorf("%w: %s", ErrAskIDInUse, adopt)
	}
	r.accountForLocked(adopt)
	return adopt, nil
}

// accountForLocked records an adopted id in the namespace its spelling names,
// so the adopted and the minted halves of one namespace cannot collide.
//
// It does **not** move the counter. A counter says "every number at or below me
// was issued", and jumping it to an adopted perm-1000000 would claim the
// 999,996 numbers in between, none of which anybody ever asked for: answering
// perm-999999 would say already_resolved for an id that never existed. The
// adopted number is remembered as reserved instead —
// mintLocked steps over it when the counter arrives, and issuedLocked counts it
// as issued — which is the same protection with none of the claim.
//
// An id no counter could ever reach is a custom id and is not remembered at
// all (maxAskNumber), as is one this namespace has already issued: the counter
// is past it and that is what says it was issued.
func (r *AskRegistry) accountForLocked(id string) {
	num, ok := canonicalID(id)
	if !ok {
		return
	}
	if num.n <= r.counterLocked(num.hidden)[num.kind] {
		return
	}
	if _, already := r.reserved[num]; already {
		return
	}
	r.reserved[num] = struct{}{}
	r.reservedOrder = append(r.reservedOrder, num)
	for len(r.reservedOrder) > keptReservations {
		delete(r.reserved, r.reservedOrder[0])
		r.reservedOrder = r.reservedOrder[1:]
	}
}

// mintLocked is the next id of this kind's own counter, **skipping any number
// an adoption reserved and any id an open ask already holds**. Every mint
// spends a number of the counter it draws on — one that publishes an opening
// spends a visible number, exactly as live.go's nextID did, and one that never
// publishes spends a hidden one — and a number that is passed over is spent all
// the same, so the counter advances past it rather than a second ask being
// given a live id. With no adoption in play the visible sequence is exactly
// perm-1, perm-2, … as goldens and the --json fixtures pin it.
//
// A reservation is dropped as the counter reaches it: from there on the counter
// itself says that number was issued, which is what issuedLocked reads.
func (r *AskRegistry) mintLocked(kind AskKind, hidden bool) string {
	seq := r.counterLocked(hidden)
	for {
		seq[kind]++
		num := askNumber{kind: kind, hidden: hidden, n: seq[kind]}
		if _, taken := r.reserved[num]; taken {
			r.dropReservationLocked(num)
			continue
		}
		id := askID(num)
		if _, open := r.open[id]; !open {
			return id
		}
	}
}

// dropReservationLocked forgets a reservation the counter has reached.
func (r *AskRegistry) dropReservationLocked(num askNumber) {
	delete(r.reserved, num)
	if i := slices.Index(r.reservedOrder, num); i >= 0 {
		r.reservedOrder = slices.Delete(r.reservedOrder, i, i+1)
	}
}

// counterLocked is one of the two per-kind id counters (hiddenMark).
func (r *AskRegistry) counterLocked(hidden bool) map[AskKind]uint64 {
	if hidden {
		return r.hiddenSeq
	}
	return r.seq
}

// askNumber is one id of this registry's own, taken apart: the kind, which of
// the kind's two namespaces it belongs to (hiddenMark), and its number. It is
// what a counter counts and what a reservation names.
type askNumber struct {
	kind   AskKind
	hidden bool
	n      uint64
}

// askID spells one id in one of a kind's two namespaces: perm-7 visible,
// perm-x7 hidden. The two can never collide, whatever the counters stand at.
func askID(num askNumber) string {
	if num.hidden {
		return fmt.Sprintf("%s-%s%d", num.kind.idPrefix(), hiddenMark, num.n)
	}
	return fmt.Sprintf("%s-%d", num.kind.idPrefix(), num.n)
}

// canonicalID reads an id this registry could have minted: a known kind's
// prefix, the hidden mark or nothing, and the exact decimal spelling of a
// positive number no larger than maxAskNumber. "perm-007" and "perm-+7" parse
// as 7 and are ids craze never issued, so they are neither accounted for nor
// recognised; a number past maxAskNumber — the signed maximum among them — is a
// custom name that happens to look like an id, and is read as one.
func canonicalID(id string) (askNumber, bool) {
	prefix, rest, cut := strings.Cut(id, "-")
	if !cut {
		return askNumber{}, false
	}
	kind, known := kindForPrefix(prefix)
	if !known {
		return askNumber{}, false
	}
	hidden := false
	if after, marked := strings.CutPrefix(rest, hiddenMark); marked {
		rest, hidden = after, true
	}
	// ParseUint takes no sign and overflows to an error, so nothing here can
	// wrap; the round trip refuses every other spelling craze never writes.
	n, err := strconv.ParseUint(rest, 10, 64)
	if err != nil || n < 1 || n > maxAskNumber || strconv.FormatUint(n, 10) != rest {
		return askNumber{}, false
	}
	return askNumber{kind: kind, hidden: hidden, n: n}, true
}

// refusalLocked is why an Open cannot park, and "" when it can.
func (r *AskRegistry) refusalLocked(token TurnToken) (AskOutcome, string, bool) {
	if r.closed {
		return AskClosing, AskByClose, true
	}
	if !token.NoTurn() {
		if token.reg != r.id {
			// Another registry's turn. It is not one of this session's and
			// never will be, so turn_ended is the honest outcome: that turn is
			// over as far as anything here can ever know.
			return AskTurnEnded, AskByTurn, true
		}
		st, known := r.turns[token.n]
		switch {
		case !known:
			// Minted long enough ago that its state was evicted, which only
			// happens to a token that retired: the turn is over.
			return AskTurnEnded, AskByTurn, true
		case st.cancelled:
			return AskCancelled, AskByCancel, true
		case st.ended:
			return AskTurnEnded, AskByTurn, true
		}
	}
	if !r.log.OutboxRoom() {
		return AskCancelled, AskByUnavailable, true
	}
	return "", "", false
}

// markLocked retires a real turn token, keeping whichever end came first.
func (r *AskRegistry) markLocked(token TurnToken, cancelled bool) {
	if token.NoTurn() {
		return
	}
	st, known := r.turns[token.n]
	if !known || st.cancelled || st.ended {
		return
	}
	st.cancelled, st.ended = cancelled, !cancelled
	r.retired = append(r.retired, token.n)
	for len(r.retired) > keptTurns {
		delete(r.turns, r.retired[0])
		r.retired = r.retired[1:]
	}
}

// resolveAllLocked ends every open ask match accepts, in opening order, and
// enqueues their endings as one batch: they are one fact about the session, and
// a batch cannot be interleaved with anything else.
func (r *AskRegistry) resolveAllLocked(match func(*askEntry) bool, outcome AskOutcome, by string) {
	var batch []Event
	for _, id := range slices.Clone(r.order) {
		e := r.open[id]
		if e == nil || !match(e) {
			continue
		}
		batch = append(batch, r.retire(e, outcome, by, "", AskAnswer{}, ""))
	}
	if len(batch) > 0 {
		r.log.Enqueue(batch...)
	}
}

// retire resolves e and answers with the ending event to enqueue. The caller
// holds r.mu and enqueues in that same section, which is what makes an
// opening always precede its ending and the registry's event order its state
// order.
func (r *AskRegistry) retire(e *askEntry, outcome AskOutcome, by, cause string, ans AskAnswer, label string) Event {
	e.rec.Status = AskResolved
	e.rec.Outcome = outcome
	e.rec.By = by
	e.rec.Answer = ans
	e.rec.ResolvedAt = r.now()
	// By pointer, not by id alone: an adopted id can name a different ask that
	// is open now, and retiring this one must not take that one off the books.
	if r.open[e.rec.ID] == e {
		delete(r.open, e.rec.ID)
		if i := slices.Index(r.order, e.rec.ID); i >= 0 {
			r.order = slices.Delete(r.order, i, i+1)
		}
	}
	r.keepLocked(e)
	close(e.done)
	return Event{
		Type:  EventAsk,
		At:    e.rec.ResolvedAt,
		Cause: cause,
		Ask: &AskUpdate{
			ID:      e.rec.ID,
			Kind:    e.rec.Kind,
			Outcome: outcome,
			By:      by,
			// A copy of the answer map, never the one the record keeps: the
			// event and the registry must not share memory.
			OptionID: ans.OptionID,
			Answers:  cloneAnswers(ans.Answers),
			Skip:     ans.Skip,
			Accepted: ans.Accept,
			Label:    label,
			Body:     r.endingBody(e),
		},
	}
}

// retireLocked is retire for an ask that is resolved as it is opened: the one
// ending is enqueued here rather than handed back, because there is no batch
// for it to join. Such an ending has no client's cause, no answer and no label
// — nobody decided anything — so it takes none of retire's last three.
func (r *AskRegistry) retireLocked(e *askEntry, outcome AskOutcome, by string) {
	r.log.Enqueue(r.retire(e, outcome, by, "", AskAnswer{}, ""))
}

// endingBody is the body an ending carries: the opening, for an ask whose
// opening was never published, and nothing for one a consumer has already seen
// raised.
//
// It is a deep copy. A self-contained ending is the only place the registry
// hands out a whole body on an event, and a consumer that changes what it
// received must not be able to change what the record says was asked, or race
// Record() while it does.
func (r *AskRegistry) endingBody(e *askEntry) *AskBody {
	if e.published {
		return nil
	}
	body := e.rec.Body.clone()
	return &body
}

// keepLocked stores a resolved ask among the terminal records, evicting the
// oldest ending past keptAsks.
//
// **Every ending is kept, id reuse included.** An adopted id whose ask has
// ended may be adopted again, and the second ask's ending is not the first
// one's corrected: Resolved() — and so tui.Stub's Calls() — must hold both, in
// the order they ended, each with a retention of its own. Storing the order as
// ids meant the second ending replaced the first's record while sitting in the
// first's ring position, so the newer record was evicted early and the older
// one was never there at all.
//
// The id map still names the ask that id means NOW, which is the most recent:
// Record and Answer are about the ask somebody is holding, not about history.
// It is therefore cleared on eviction only when it still points at the entry
// being evicted.
func (r *AskRegistry) keepLocked(e *askEntry) {
	r.doneOrder = append(r.doneOrder, e)
	r.done[e.rec.ID] = e
	for len(r.doneOrder) > keptAsks {
		old := r.doneOrder[0]
		if r.done[old.rec.ID] == old {
			delete(r.done, old.rec.ID)
		}
		r.doneOrder[0] = nil
		r.doneOrder = r.doneOrder[1:]
	}
}

// entryLocked is the ask with this id, open or kept.
func (r *AskRegistry) entryLocked(id string) *askEntry {
	if e, ok := r.open[id]; ok {
		return e
	}
	if e, ok := r.done[id]; ok {
		return e
	}
	return nil
}

// unknownLocked says what an id no open ask holds means. A kept terminal
// record answers for itself; past the eviction bound the id's own counter does,
// because "perm-7" with the permission counter at 7 or more was issued by this
// incarnation and therefore ended (05 pins already_resolved for exactly that,
// never unknown_ask). Each namespace answers for itself: "perm-x7" is read
// against the hidden counter. An adopted id that spells neither has no counter
// to consult and is unknown once its record has gone.
//
// It reads the most recent record an id names, which for a re-used id is its
// latest ask: the older ones are history (Resolved), not something a client
// could still be answering.
func (r *AskRegistry) unknownLocked(id string) error {
	if _, kept := r.done[id]; kept {
		return fmt.Errorf("%w: %s", ErrAlreadyResolved, id)
	}
	if r.issuedLocked(id) {
		return fmt.Errorf("%w: %s, whose record has been evicted", ErrAlreadyResolved, id)
	}
	return fmt.Errorf("%w: %s", ErrUnknownAsk, id)
}

// issuedLocked reports whether this incarnation ever issued id: minted it —
// which is what "at or below its namespace's counter" means, and nothing
// else — or reserved it for somebody who adopted it ahead of that counter
// (accountForLocked). A number in neither is a hole nobody was ever given.
func (r *AskRegistry) issuedLocked(id string) bool {
	num, ok := canonicalID(id)
	if !ok {
		return false
	}
	if num.n <= r.counterLocked(num.hidden)[num.kind] {
		return true
	}
	_, reserved := r.reserved[num]
	return reserved
}

// opening is the ask's opening event: the three kinds and shapes every
// consumer already renders, with the id filled in and nothing else touched.
// auto marks the headless path, whose opening carries the answer craze sent.
//
// The event carries a **deep copy** of the body, never the registry's own: a
// consumer that changes an opening it received — an option id on a permission,
// a question's options — must not be able to change what Answer validates
// against, and must not race Record() while it does.
// Auto and its answers are therefore the event's alone too, which is also why
// the record keeps the request exactly as it was offered.
func (r *AskRegistry) opening(kind AskKind, body AskBody, auto bool, ans AskAnswer) Event {
	body = body.clone()
	ev := Event{At: r.now()}
	switch kind {
	case AskQuestion:
		ev.Type, ev.Question = EventQuestion, body.Question
		if auto && ev.Question != nil {
			ev.Question.Auto = true
			ev.Question.Answers = cloneAnswers(ans.Answers)
		}
	case AskPlan:
		ev.Type, ev.Plan = EventPlan, body.Plan
		if auto && ev.Plan != nil {
			ev.Plan.Auto = true
			ev.Plan.Accepted = ans.Accept
		}
	default:
		ev.Type, ev.Permission = EventPermission, body.Permission
	}
	return ev
}

// setBodyID stamps the minted id onto the body's payload, which is where every
// consumer of an opening reads it. The body is the registry's own copy by now,
// so the caller's value is untouched.
func setBodyID(kind AskKind, body AskBody, id string) {
	switch kind {
	case AskQuestion:
		if body.Question != nil {
			body.Question.ID = id
		}
	case AskPlan:
		if body.Plan != nil {
			body.Plan.ID = id
		}
	default:
		if body.Permission != nil {
			body.Permission.ID = id
		}
	}
}

// validateAnswer checks a against what kind's body offered and answers with the
// answer as it will be recorded, the label of what was chosen, and ErrBadAnswer
// for anything that does not fit. It is the whole of "first *valid* answer
// wins" (SD-25): nothing is claimed and nothing is emitted until it returns.
func validateAnswer(kind AskKind, body AskBody, a AskAnswer) (AskAnswer, string, error) {
	switch kind {
	case AskPermission:
		if a.Skip || a.Accept || a.Reject || len(a.Answers) > 0 {
			return AskAnswer{}, "", fmt.Errorf("%w: a permission takes an option id or an explicit cancel", ErrBadAnswer)
		}
		if a.Cancel {
			if a.OptionID != "" {
				return AskAnswer{}, "", fmt.Errorf("%w: a cancelled permission names no option", ErrBadAnswer)
			}
			return AskAnswer{Cancel: true}, "", nil
		}
		name, offered := permissionOption(body.Permission, a.OptionID)
		if !offered {
			return AskAnswer{}, "", fmt.Errorf("%w: %q is not an option this permission offered", ErrBadAnswer, a.OptionID)
		}
		return AskAnswer{OptionID: a.OptionID}, name, nil
	case AskQuestion:
		if a.OptionID != "" || a.Cancel || a.Accept || a.Reject {
			return AskAnswer{}, "", fmt.Errorf("%w: a question takes its answers or a skip", ErrBadAnswer)
		}
		if a.Skip {
			if len(a.Answers) > 0 {
				return AskAnswer{}, "", fmt.Errorf("%w: a skipped question carries no answers", ErrBadAnswer)
			}
			return AskAnswer{Skip: true}, "", nil
		}
		if len(a.Answers) == 0 {
			// A question that asks nothing — cursor really does send
			// ask_question with no questions — is answered with no answers, by
			// craze's policy and by a client alike: it is the only thing either
			// could say, and it was answered accepted with empty answers before
			// the registry existed. A question that asks anything still needs
			// its answers or a skip.
			if len(questionsOffered(body.Question)) > 0 {
				return AskAnswer{}, "", fmt.Errorf("%w: a question takes its answers or a skip", ErrBadAnswer)
			}
			return AskAnswer{Answers: cloneAnswers(a.Answers)}, "", nil
		}
		if err := checkQuestionAnswers(body.Question, a.Answers); err != nil {
			return AskAnswer{}, "", err
		}
		return AskAnswer{Answers: cloneAnswers(a.Answers)}, "", nil
	case AskPlan:
		if a.OptionID != "" || a.Cancel || a.Skip || len(a.Answers) > 0 {
			return AskAnswer{}, "", fmt.Errorf("%w: a plan takes an accept or a reject", ErrBadAnswer)
		}
		if a.Accept == a.Reject {
			return AskAnswer{}, "", fmt.Errorf("%w: a plan takes exactly one of accept and reject", ErrBadAnswer)
		}
		return AskAnswer{Accept: a.Accept, Reject: a.Reject}, "", nil
	default:
		return AskAnswer{}, "", fmt.Errorf("%w: %q is not an ask craze knows how to answer", ErrBadAnswer, kind)
	}
}

// permissionOption is the name of the option id offers, and whether it was
// offered at all. An empty id is never an option: the deliberate cancel says so
// with AskAnswer.Cancel instead, so a client that forgot to fill the id in
// cannot cancel a request by accident.
func permissionOption(p *PermissionEvent, id string) (string, bool) {
	if p == nil || id == "" {
		return "", false
	}
	for _, o := range p.Options {
		if o.OptionID == id {
			return o.Name, true
		}
	}
	return "", false
}

// checkQuestionAnswers refuses an answer naming a question the request does not
// have, or an option that question did not offer. The "OK" option craze invents
// for a question with no options of its own carries the empty id, and is an
// offered option like any other (live.go's questionsFromRequest).
func checkQuestionAnswers(q *QuestionEvent, answers map[string][]string) error {
	for id, chosen := range answers {
		one, found := questionByID(q, id)
		if !found {
			return fmt.Errorf("%w: %q is not a question this ask asked", ErrBadAnswer, id)
		}
		for _, pick := range chosen {
			if !offersOption(one, pick) {
				return fmt.Errorf("%w: %q is not an option question %q offered", ErrBadAnswer, pick, id)
			}
		}
	}
	return nil
}

// questionsOffered is what a question ask actually asked, nil for one with no
// body at all — which, like one with an empty list, asks nothing.
func questionsOffered(q *QuestionEvent) []Question {
	if q == nil {
		return nil
	}
	return q.Questions
}

func questionByID(q *QuestionEvent, id string) (Question, bool) {
	if q == nil {
		return Question{}, false
	}
	for _, one := range q.Questions {
		if one.ID == id {
			return one, true
		}
	}
	return Question{}, false
}

func offersOption(q Question, id string) bool {
	for _, o := range q.Options {
		if o.ID == id {
			return true
		}
	}
	return false
}
