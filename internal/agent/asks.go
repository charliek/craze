package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
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
//     ErrBadAnswer, emits nothing and leaves the ask open — where today's
//     AnswerPermission cancels the request before it validates the option id.
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
// are one atomic section. That is what today's park could not do: it read the
// ACP client's turn counter and the session's cancelled-turn mark under the
// session's lock, which is not the lock the insert happens under (panel: astra
// 11).

const (
	// keptAsks is how many terminal records the registry keeps. Past it the
	// oldest is evicted and an answer naming it is ErrAlreadyResolved rather
	// than ErrUnknownAsk, decided from the id's counter (05-protocol.md pins
	// that distinction: unknown means the id was never issued).
	keptAsks = 256
	// keptTurns is how many retired turn tokens keep their exact ending. A
	// token older than that reads as ended, which is what every retired token
	// is; only the choice between "cancelled" and "turn_ended" is lost, and
	// only for a handler goroutine delayed past keptTurns turns.
	keptTurns = 64
)

var (
	// ErrBadAnswer is an answer that does not fit its ask: an option the
	// request never offered, an empty permission answer that does not say it
	// cancels, a question answer naming a question or an option the request
	// does not have, a plan answered with neither accept nor reject, or any
	// answer carrying another kind's fields. Nothing is emitted and the ask
	// stays open, so the agent is not left waiting for an Esc because a client
	// mis-addressed one call (05: bad_request).
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
	// reverse), so this is its own, and engine.Code answers "unavailable" for it
	// through its default arm.
	ErrAskUnavailable = errors.New("agent: the event log is backed up")
	// ErrAskIDInUse refuses an Open that adopts an id another open ask already
	// holds. It is a caller's own bug — a test emitting two cards with one id —
	// and it is refused rather than allowed to steal the first ask's answers:
	// nothing is minted, nothing is enqueued, and the ask holding that id is
	// untouched.
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
	// open ask, whatever turn it belongs to — today's cancelWaiting, and 05's
	// rule that a cancel is accepted whenever an ask is pending.
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
// **The zero value is the no-turn token**, and it is what a request that
// belongs to no turn of craze's own is opened against: one that arrived between
// turns, or during a turn the agent started itself. Such an ask ends by an
// answer, a cancel, its asking call's context or the close — never by EndTurn,
// which is what "turn_ended applies only to asks opened against a token whose
// EndTurn has been notified" means (§3.6).
type TurnToken struct{ n uint64 }

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
	// registry and the test's id survives. An adopted id spends no counter,
	// and one that does not spell "<kind>-N" is only known for as long as its
	// record is kept — past the eviction bound an answer naming it is
	// ErrUnknownAsk rather than ErrAlreadyResolved, because nothing is left to
	// say it was ever issued.
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
//     option (live.go's questionsFromRequest) — or Skip skips the lot.
//   - plan: exactly one of Accept and Reject.
//
// The zero AskAnswer is not a valid answer to anything, which is deliberate: a
// plan's reject is the explicit Reject and not the absence of Accept, so a
// client that sends an empty answer gets ErrBadAnswer instead of silently
// rejecting the plan it meant to accept.
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
	// note, never a second ending, and it changes no outcome (panel: astra 13).
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

// AskRegistry is one session's asks (plan 021 §3.6). One is built beside the
// session's event log, and the session, the provider's handlers and the
// clients above the seam all meet here.
type AskRegistry struct {
	log *EventLog
	now func() time.Time

	mu    sync.Mutex
	seq   map[AskKind]int
	open  map[string]*askEntry
	order []string
	done  map[string]*askEntry
	// doneIDs is the eviction order of the terminal records, oldest first.
	doneIDs []string
	turnSeq uint64
	turns   map[uint64]*turnState
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
		log:   log,
		now:   now,
		seq:   map[AskKind]int{},
		open:  map[string]*askEntry{},
		done:  map[string]*askEntry{},
		turns: map[uint64]*turnState{},
	}
}

// BeginTurn mints the token for a turn that is starting. The session calls it
// where it takes its own turn up; every call is a new token, so a request from
// a turn that has been and gone can never be mistaken for one of the turn
// running now — which counter equality (ACP's TurnLive) cannot promise.
func (r *AskRegistry) BeginTurn() TurnToken {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turnSeq++
	t := TurnToken{n: r.turnSeq}
	r.turns[t.n] = &turnState{}
	return t
}

// CancelTurn ends **every** open ask as cancelled, whatever turn it belongs
// to, and marks token so a request that arrives for it afterwards is refused
// rather than parked. It is today's cancelWaiting, which swaps the whole map:
// a cancel is accepted whenever an ask is pending, with a turn or without one
// (05's gate table), and an ask left parked through a cancel would leave the
// agent waiting on a reply nobody will send.
//
// A cancel with no turn of craze's own — the no-turn token — still answers
// every open ask; it marks nothing, so nothing is refused afterwards. A later
// BeginTurn is a new token either way, so the next turn parks normally.
func (r *AskRegistry) CancelTurn(token TurnToken) {
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
// are exactly the ones no turn's ending may take away.
func (r *AskRegistry) EndTurn(token TurnToken) {
	if token.NoTurn() {
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
// through the same Wait as any other. It is refused when the registry has
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
	if req.ID != "" {
		if _, open := r.open[req.ID]; open {
			return nil, fmt.Errorf("%w: %s", ErrAskIDInUse, req.ID)
		}
	}
	id := r.mintLocked(req)
	setBodyID(req.Kind, body, id)
	if outcome, by, refused := r.refusalLocked(token); refused {
		e := r.newEntryLocked(id, req.Kind, token, body)
		r.retireLocked(e, outcome, by, "", AskAnswer{}, "")
		return &Ask{r: r, id: id, ctx: ctx, e: e}, nil
	}
	e := r.newEntryLocked(id, req.Kind, token, body)
	r.open[id] = e
	r.order = append(r.order, id)
	e.published = true
	r.log.Enqueue(r.opening(req.Kind, body, false, AskAnswer{}))
	return &Ask{r: r, id: id, ctx: ctx, e: e}, nil
}

// Automatic resolves an ask craze's own approval policy answered, without ever
// parking it, and answers with the record its caller replies from. Everything
// is journaled by virtue of being events:
//
//   - a question or a plan writes the Auto opening today's headless path
//     already writes — answers or accepted filled in — **and** one automatic
//     ending, as one batch, so nothing can land between them;
//   - a permission writes no opening at all and one automatic ending carrying
//     the body, so a forced `craze prompt --json` run gains no permission line;
//   - a permission the policy could not answer — Force with no allow option
//     offered — is cancelled, by policy, with the body. Pass AskAnswer{Cancel:
//     true}, or any answer that does not fit the request: a policy that cannot
//     answer its own ask is a cancel.
//
// It is a mandatory completion: the decision has been made and the provider is
// about to be told it, so the record is written whatever the outbox holds
// (§3.3's list of rejectable admissions has Open, not this).
func (r *AskRegistry) Automatic(token TurnToken, req AskRequest, a AskAnswer) AskRecord {
	body := req.Body.clone()
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.mintLocked(req)
	setBodyID(req.Kind, body, id)
	e := r.newEntryLocked(id, req.Kind, token, body)
	ans, label, err := validateAnswer(req.Kind, body, a)
	outcome, by := AskAutomatic, AskByPolicy
	if err != nil || ans.Cancel {
		outcome, ans, label = AskCancelled, AskAnswer{Cancel: true}, ""
	}
	var batch []Event
	if req.Kind != AskPermission && outcome == AskAutomatic {
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
// parks, and today they leave no trace at all (§2.3's table, panel: astra 12).
// ACP reports them instead, and each becomes one self-contained ending with the
// body and no opening, so no card is ever raised for a request nobody can
// answer.
//
// outcome and by are the caller's: cancelled, turn_ended or closing, by the
// provider. It always mints — a request answered before any handler ran never
// had a craze id to adopt — and it is a mandatory completion, like every other
// ending.
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
	id := r.mintLocked(AskRequest{Kind: req.Kind})
	setBodyID(req.Kind, body, id)
	e := r.newEntryLocked(id, req.Kind, token, body)
	r.retireLocked(e, outcome, by, "", AskAnswer{}, "")
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
// both that and that the answer never arrived (panel: astra 13).
//
// An id the registry no longer holds a record for still writes the note: the
// diagnostic is the point, and a record evicted 256 asks later is not.
func (r *AskRegistry) Report(id string, rep AskReport) {
	r.mu.Lock()
	kind, outcome := AskKind(""), AskOutcome("")
	if e := r.entryLocked(id); e != nil {
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

// mintLocked is the ask's id: the caller's adopted one, or the next of this
// kind's own counter. Every mint spends a number — an ask that opened, one
// that was refused, one policy answered — exactly as live.go's nextID does, so
// the numbering a golden pins never depends on how an ask ended.
func (r *AskRegistry) mintLocked(req AskRequest) string {
	if req.ID != "" {
		return req.ID
	}
	r.seq[req.Kind]++
	return fmt.Sprintf("%s-%d", req.Kind.idPrefix(), r.seq[req.Kind])
}

// refusalLocked is why an Open cannot park, and "" when it can.
func (r *AskRegistry) refusalLocked(token TurnToken) (AskOutcome, string, bool) {
	if r.closed {
		return AskClosing, AskByClose, true
	}
	if !token.NoTurn() {
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
			ID:       e.rec.ID,
			Kind:     e.rec.Kind,
			Outcome:  outcome,
			By:       by,
			OptionID: ans.OptionID,
			Answers:  ans.Answers,
			Skip:     ans.Skip,
			Accepted: ans.Accept,
			Label:    label,
			Body:     r.endingBody(e),
		},
	}
}

// retireLocked is retire for an ask that is resolved as it is opened: the one
// ending is enqueued here rather than handed back, because there is no batch
// for it to join.
func (r *AskRegistry) retireLocked(e *askEntry, outcome AskOutcome, by, cause string, ans AskAnswer, label string) {
	r.log.Enqueue(r.retire(e, outcome, by, cause, ans, label))
}

// endingBody is the body an ending carries: the opening, for an ask whose
// opening was never published, and nothing for one a consumer has already seen
// raised.
func (r *AskRegistry) endingBody(e *askEntry) *AskBody {
	if e.published {
		return nil
	}
	body := e.rec.Body
	return &body
}

// keepLocked stores a resolved ask among the terminal records, evicting the
// oldest past keptAsks.
func (r *AskRegistry) keepLocked(e *askEntry) {
	if _, kept := r.done[e.rec.ID]; !kept {
		r.doneIDs = append(r.doneIDs, e.rec.ID)
	}
	r.done[e.rec.ID] = e
	for len(r.doneIDs) > keptAsks {
		delete(r.done, r.doneIDs[0])
		r.doneIDs = r.doneIDs[1:]
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
// never unknown_ask). An adopted id that does not spell "<kind>-N" has no
// counter to consult and is unknown once its record has gone.
func (r *AskRegistry) unknownLocked(id string) error {
	if _, kept := r.done[id]; kept {
		return fmt.Errorf("%w: %s", ErrAlreadyResolved, id)
	}
	if r.issuedLocked(id) {
		return fmt.Errorf("%w: %s, whose record has been evicted", ErrAlreadyResolved, id)
	}
	return fmt.Errorf("%w: %s", ErrUnknownAsk, id)
}

// issuedLocked reports whether this incarnation ever minted id.
func (r *AskRegistry) issuedLocked(id string) bool {
	prefix, n, ok := strings.Cut(id, "-")
	if !ok {
		return false
	}
	kind, known := kindForPrefix(prefix)
	if !known {
		return false
	}
	num, err := strconv.Atoi(n)
	if err != nil {
		return false
	}
	return num >= 1 && num <= r.seq[kind]
}

// opening is the ask's opening event: the three kinds and shapes every
// consumer already renders, with the id filled in and nothing else touched.
// auto marks the headless path, whose opening carries the answer craze sent.
func (r *AskRegistry) opening(kind AskKind, body AskBody, auto bool, ans AskAnswer) Event {
	ev := Event{At: r.now()}
	switch kind {
	case AskQuestion:
		ev.Type, ev.Question = EventQuestion, body.Question
		if auto && ev.Question != nil {
			ev.Question.Auto = true
			ev.Question.Answers = ans.Answers
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
			return AskAnswer{}, "", fmt.Errorf("%w: a question takes its answers or a skip", ErrBadAnswer)
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
