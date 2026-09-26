package harness

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/store"
)

// Modes and the reminders that carry them (plan 023 §3.1, §3.3).
//
// A session runs in agent, plan or ask mode. The mode is enforced by the tool
// gate (tool.ModeGate), which refuses the calls the mode does not allow, and
// it is told to the model by a reminder: a synthetic user message, wrapped in
// <system-reminder> tags as grok-build wraps its own, spliced into the step's
// input right after the user's prompt.
//
// A reminder is not part of the conversation a person sees. It is never
// emitted as an Event and never reported in Result.Unanswered; it lives in a
// collection of its own, separate from the steers (steer.go), so nothing that
// reads those can see one. The model reads it in every request of the turn
// that composed it — and in every later turn's too: the finished step whose
// request carried it writes a reminder entry at its place, after the step's
// user entries and before its steers, as spliceInto orders them, and the
// history the next turn rebuilds from the transcript renders it again there,
// byte for byte what was sent (plan 028 §3.15). So a turn's first request
// begins with the whole of the last request before it, and the provider's
// prefix cache holds across turns in plan and ask mode as it does in agent
// mode (H5's R3 measured a full miss on every plan-mode turn while reminders
// were not stored). The entry names the text's variant and never holds the text (plan
// 023 owner decision 5): every text a reminder can carry is exactly one
// variant (reminderVariants), and a variant and the plan file's path are all
// it takes to write the text again — whether the plan file had anything in it
// is part of the variant, so rendering one never reads the file, and a plan
// written since changes nothing the history says was sent.
//
// What the model is told is a function of the mode and of what it was told
// last. A mode it has not been told about produces a transition notice at the
// next step boundary — the step the request goes out on, whether that is the
// turn's first or its fifth — and, in plan and ask mode, every later turn
// opens with the mode's standing reminder. "Told" means told in a step that
// finished and whose output the transcript kept: a notice sent in a turn that
// failed, was cancelled or only thought is not told for good, and it goes out
// again (§3.3). Plan mode's alternates between a full text and a sparse one,
// as grok-build's does, and both name the plan file, so that a sparse one
// never leaves the model without the path (panel correction 8).
//
// So a step a cancel or a failure cut short is saved without the reminder its
// request carried (plan 028 X29, astra r1-c7): a reminder entry is only ever
// one a finished step's request carried, which is what "told" means, and the
// transcript and the told state say the same thing. Written with the partial
// answer, a reminder would be history the told state knows nothing of —
// cancelled in ask mode and switched back to agent, the next turn would
// compose no exit notice (told is still agent) while its history replayed
// ask's "every such call is denied"; and a resume, which seeds told from the
// transcript's last mode_change (which only a finished step writes), would do
// the same. The cost is the next turn's first request, which is not the cut
// request plus more: one prefix-cache miss after a cancel or a failure.

// reminderTag wraps every reminder. grok-build's tag, so a model that has
// seen one recognizes it.
const reminderTag = "system-reminder"

// The modes: the tool framework's ModeAgent, ModePlan and ModeAsk, restated
// because this file shapes Fantasy messages and toolbridge.go is the one file
// of the harness that may import both Fantasy and that framework (Seam 1,
// plan 019 §3.1). A test pins the two sets equal, as steerCap is a
// restatement a comment pins.
const (
	modeAgent = "agent"
	modePlan  = "plan"
	modeAsk   = "ask"
)

// modeGate is the enforcement side of a mode: tool.ModeGate, named here as
// the two methods the session calls on it, for the same seam.
type modeGate interface {
	SetMode(mode string)
	SetPlanPath(path string)
}

// The reminder texts. Plan mode's are grok-build's (plan_mode.rs:325-371)
// with craze's tool names and the absolute plan path spliced in; ask mode's
// and the two exits are craze's own, in the same voice. Every one of them
// says what the rule is, because the model has no other way to learn it: the
// system prompt is frozen and says nothing about modes (D-30), exactly as
// grok-build's template does not.
const (
	// planReminderSparse is the alternate turn's, which saves the tokens the
	// full text costs but keeps the path (panel correction 8).
	planReminderSparse = "Plan mode is still active. Do not make any edits or writes to the system except for the plan file (`%s`)."

	// planReminderFull is the standing reminder, with the plan file's two
	// cases: one already written, and none yet.
	planReminderFull = `Plan mode is active. Do not make any edits or writes to the system.

## Plan File:
%s

You should build your plan by writing to or editing this file. Note that this is the only file you are allowed to edit.

Your turn should only end with either ask_user_question to clarify requirements or exit_plan_mode to present your plan to the user.`

	// The two halves of the full text's plan-file line. craze splits
	// grok-build's one edit tool in two: a plan that exists is edited, and one
	// that does not is written.
	planFileWritten = "A plan file exists at `%s`. You can read it and make edits using the edit tool."
	planFileEmpty   = "No plan written yet. Write your plan to `%s` using the write tool."

	// planReminderReentry is plan mode entered again in a session that
	// already has a plan file with something in it.
	planReminderReentry = `## Returning to Plan Mode

You are entering plan mode again after having previously exited it. A plan file exists at ` + "`%s`" + ` from your previous planning session.

Your turn should only end with either ask_user_question to clarify requirements or exit_plan_mode to present your plan to the user.`

	// planReminderExit is plan mode left for agent mode. It names the file so
	// the model can implement what it planned.
	planReminderExit = "You have exited plan mode. The plan is at `%s`. You can now make edits, run tools, and take actions."

	// askReminder is ask mode's standing reminder. It names the shell too:
	// bash is not read-only, so ask mode denies it, and a model that learns
	// that from a refused call has wasted a step.
	askReminder = "Ask mode is active: answer from what you can read. Do not edit or write files, and do not run shell commands — every such call is denied."

	// askReminderExit is ask mode left for agent mode, the plan exit's twin.
	askReminderExit = "You have exited ask mode. You can now make edits, run tools, and take actions."

	// childPlanReminder is a sub-agent's plan mode, craze's own (plan 026
	// §3.5). A child has no plan file, so every text above that names one
	// would hand it "``", and none of them may reach it: it replaces the
	// transition notice, the standing reminder and their alternation alike.
	// It names bash's rule too, because in plan mode the gate lets a command
	// through and the reminder is the only thing that carries the rule.
	childPlanReminder = "The agent that started you is in plan mode: do not edit or write any file (every such call is denied). " +
		"Read, search, and run only commands that change nothing."
)

// The reminder variants (plan 028 §3.15): one name for each text a reminder
// can carry, which is what a reminder entry stores in its place. The two full
// plan texts are two variants because they differ by the plan file's state
// when they were composed, which a later render must not read.
const (
	variantPlanFullEmpty   = "plan_full_empty"   // the full plan text: no plan written yet
	variantPlanFullWritten = "plan_full_written" // the full plan text: a plan file exists
	variantPlanSparse      = "plan_sparse"
	variantPlanReentry     = "plan_reentry"
	variantPlanExit        = "plan_exit"
	variantAsk             = "ask"
	variantAskExit         = "ask_exit"
	variantChildPlan       = "child_plan"
)

// reminderVariant is one variant's meaning: the mode its text speaks for, and
// the text itself, a function of the plan file's path and nothing else.
type reminderVariant struct {
	mode string
	text func(plan string) string
}

// reminderVariants are every variant, and the one place each text is
// written: a reminder the turn composes (reminderFor) and one a history
// renders again (render) both come from here, so the two are the same bytes.
// A test composes every text reminderFor can and holds each to exactly one of
// these.
var reminderVariants = map[string]reminderVariant{
	variantPlanFullEmpty: {modePlan, func(plan string) string {
		return fmt.Sprintf(planReminderFull, fmt.Sprintf(planFileEmpty, plan))
	}},
	variantPlanFullWritten: {modePlan, func(plan string) string {
		return fmt.Sprintf(planReminderFull, fmt.Sprintf(planFileWritten, plan))
	}},
	variantPlanSparse:  {modePlan, func(plan string) string { return fmt.Sprintf(planReminderSparse, plan) }},
	variantPlanReentry: {modePlan, func(plan string) string { return fmt.Sprintf(planReminderReentry, plan) }},
	variantPlanExit:    {modeAgent, func(plan string) string { return fmt.Sprintf(planReminderExit, plan) }},
	variantAsk:         {modeAsk, func(string) string { return askReminder }},
	variantAskExit:     {modeAgent, func(string) string { return askReminderExit }},
	variantChildPlan:   {modePlan, func(string) string { return childPlanReminder }},
}

// modes is a session's mode: what the gate enforces, what the model has been
// told, and where the plan file is.
//
// Its lock is a leaf — nothing is called and no other lock is taken while it
// is held, and it is held across no I/O — so the turn can read it at a step
// boundary and a user can switch modes mid-turn without either waiting on the
// other. The gate's own copy of the mode is an atomic it reads from Fantasy's
// tool goroutines; the two are set together, so a call judged after a switch
// is judged under the mode the next boundary will announce.
type modes struct {
	planPath string   // the session's plan file; fixed at Open, "" for a sub-agent
	gate     modeGate // the enforcement side (tool.ModeGate)
	// child marks a sub-agent's modes (newChildModes): its mode never
	// changes, it has no plan file, and in plan mode it reads
	// childPlanReminder and nothing that names a plan path.
	child bool

	mu    sync.Mutex
	mode  string // the mode now: what the gate judges by
	told  string // the mode the model has been told about, durably (heard)
	turns int    // plan reminders already sent, for the full/sparse alternation
	// gen counts the resets: every set bumps it, and a reminder carries the
	// one it was composed at. Equality of turns alone cannot tell a reset from
	// no reset — a set puts turns back to 0, so a reminder composed at 0 and a
	// reset to 0 compare equal — and a stale commit would then advance the
	// counter the reset meant to hold at the full text (plan 023 §3.3).
	gen int
}

// newModes starts a session in mode, with plan the path of its plan file. The
// model has been told nothing, which for agent mode is the truth — there is
// nothing to say — and for the other two is why a session opened in one
// announces it at its first step boundary, like any other switch.
func newModes(mode, plan string, gate modeGate) *modes {
	m := &modes{planPath: plan, gate: gate, mode: mode, told: modeAgent}
	gate.SetPlanPath(plan)
	gate.SetMode(mode)
	return m
}

// resumedModes starts a resumed session in mode, having told the model of
// told already (plan 028 §3.3, P29): the mode of the transcript's last
// mode_change, which is written only with the step whose request carried the
// notice for it, so it is exactly what the conversation says the model heard
// (recordMode, heard). A session resumed in that mode is not told it again —
// in plan mode it reads the standing reminder, never the re-entry notice's
// "previously exited" — and one resumed in another gets the transition.
func resumedModes(mode, told, plan string, gate modeGate) *modes {
	m := newModes(mode, plan, gate)
	m.told = told
	return m
}

// newChildModes starts a sub-agent in mode, its parent's, with no plan file:
// the gate's plan path stays "", so in plan mode every edit is refused (plan
// 026 §3.2). The model is told the mode at its first step boundary, as any
// session opened in plan or ask mode is.
func newChildModes(mode string, gate modeGate) *modes {
	m := newModes(mode, "", gate)
	m.child = true
	return m
}

// current is the mode the session is in.
func (m *modes) current() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode
}

// set switches the mode. The gate is set in the same critical section, so the
// mode a call is judged under and the mode the next boundary announces can
// never be two different things; it is an atomic store, which waits on
// nothing. The alternation restarts: the model is about to be told the mode
// afresh, and the full text is what it should read first.
func (m *modes) set(mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode, m.turns, m.gen = mode, 0, m.gen+1
	m.gate.SetMode(mode)
}

// pendingReminder is one composed reminder: its variant and text, the mode it
// speaks for, the reset generation it was composed at, and whether the
// alternation advances once the request carrying it has gone out.
type pendingReminder struct {
	variant string
	text    string
	mode    string
	gen     int  // the reset generation it was composed at (see modes.gen)
	counts  bool // a plan-mode reminder: the alternation advances with it
}

// compose is the reminder of variant, composed at reset generation gen: its
// text for this session's plan file, and the mode it speaks for.
func (m *modes) compose(variant string, gen int, counts bool) pendingReminder {
	v := reminderVariants[variant]
	return pendingReminder{variant: variant, text: v.text(m.planPath), mode: v.mode, gen: gen, counts: counts}
}

// render is the message a reminder entry of variant stands for in this
// session's history (store.Renderer, plan 028 §3.15): the one the request
// that carried it sent, from the same text and the same wrapper. It reads
// nothing but the plan file's path, fixed at Open. ok is false for a variant
// this craze does not know — a newer one's — which the history then leaves
// out.
func (m *modes) render(variant string) (fantasy.Message, bool) {
	v, ok := reminderVariants[variant]
	if !ok {
		return fantasy.Message{}, false
	}
	return reminderMessage(v.text(m.planPath)), true
}

// reminder is the reminder step's request should carry, if any: the mode's
// transition notice when the model has not been told about the mode it is
// in, and otherwise, at a turn's first step alone, plan or ask mode's
// standing reminder. Agent mode with nothing to announce composes nothing, so
// an agent-mode session's requests are the bytes they have always been.
//
// carried is the mode this turn's own requests already announce, "" when it
// has announced none. It stands in for told within the turn: told is recorded
// only once a step of the turn has persisted output (§3.3, heard), and until
// then the notice already in t.reminders is in every request of this turn, so
// composing it a second time would say the same thing twice.
//
// It reads the mode under the lock and composes outside it (the plan file's
// state is a stat): a switch that lands in between is simply the next
// boundary's transition, since what is recorded is the mode this text spoke
// for and not whatever the mode is by then.
func (m *modes) reminderFor(step int, carried string) (pendingReminder, bool) {
	m.mu.Lock()
	mode, told, parity, gen := m.mode, m.told, m.turns, m.gen
	m.mu.Unlock()
	if carried != "" {
		told = carried
	}

	switch {
	case m.child && mode == modePlan:
		// A sub-agent's plan mode bypasses transition, planEntry and
		// planStanding, every one of which would name a plan file it does not
		// have. The text never alternates, so nothing counts; otherwise it is
		// composed when any plan reminder would be.
		if mode == told && step > 0 {
			return pendingReminder{}, false
		}
		return m.compose(variantChildPlan, gen, false), true
	case mode != told:
		return m.compose(m.transition(mode, told), gen, mode == modePlan), true
	case step > 0:
		// Nothing has changed since the last boundary, and the turn's own
		// reminder is already in every request of it.
		return pendingReminder{}, false
	case mode == modePlan:
		return m.compose(m.planStanding(parity), gen, true), true
	case mode == modeAsk:
		return m.compose(variantAsk, gen, false), true
	}
	return pendingReminder{}, false
}

// restartReminder is the reminder a segment's first request after a restart
// carries, if any (plan 028 §3.11 table, R2-3, R3-2, R4-1): the request
// replays a history rebuilt from the store — from a summary on, or the same
// one after a compaction that failed — and what that history holds decides,
// never the summary's prose. A transition is composed exactly as reminderFor
// composes one, by the told state (carried standing in for it, as there):
// the mode differs from the one the model was last told, so it is said. With
// nothing to announce, the mode's standing reminder is sent again — the
// turn's first request sent one, and a summary may have swallowed it —
// unless last, the variant of the last reminder the request's history holds
// (the retained history's, or a reminder carried uncommitted from a request
// that never completed, C12), is a standing one for this mode: then it is
// already there, and nothing is composed. A re-send never advances the
// alternation: it is the turn's reminder said again, not a turn's own.
func (m *modes) restartReminder(carried, last string) (pendingReminder, bool) {
	m.mu.Lock()
	mode, told, parity, gen := m.mode, m.told, m.turns, m.gen
	m.mu.Unlock()
	if carried != "" {
		told = carried
	}
	if m.child && mode == modePlan {
		if mode == told && last == variantChildPlan {
			return pendingReminder{}, false
		}
		return m.compose(variantChildPlan, gen, false), true
	}
	if mode != told {
		return m.compose(m.transition(mode, told), gen, mode == modePlan), true
	}
	if standingFor(last) == mode {
		return pendingReminder{}, false
	}
	switch mode {
	case modePlan:
		return m.compose(m.planStanding(parity), gen, false), true
	case modeAsk:
		return m.compose(variantAsk, gen, false), true
	}
	return pendingReminder{}, false
}

// standingFor is the mode variant is the standing reminder of — plan mode's
// full and sparse texts, ask mode's, a sub-agent's plan text — and "" for a
// transition notice (the re-entry notice and the two exits) or no variant:
// what suppresses a restart's re-send (restartReminder). A standing text is
// what the model reads at every turn of the mode; a transition says the mode
// changed, once.
func standingFor(variant string) string {
	switch variant {
	case variantPlanFullEmpty, variantPlanFullWritten, variantPlanSparse, variantChildPlan:
		return modePlan
	case variantAsk:
		return modeAsk
	}
	return ""
}

// transition is the variant the model reads when the mode changed under it:
// the mode it is in now, and, for a return to agent mode, which mode it has
// left. A plan-to-ask switch announces ask's restrictions rather than plan's
// exit, because ask is where the model now is.
func (m *modes) transition(mode, told string) string {
	switch {
	case mode == modePlan:
		return m.planEntry()
	case mode == modeAsk:
		return variantAsk
	case told == modePlan:
		return variantPlanExit
	default:
		return variantAskExit
	}
}

// planEntry is plan mode's variant on entry: grok-build's re-entry notice
// when a plan was written in this session already, and the full reminder
// otherwise.
func (m *modes) planEntry() string {
	if planHasContent(m.planPath) {
		return variantPlanReentry
	}
	return m.planStanding(0)
}

// planStanding is the standing plan reminder's variant at parity: the full
// text on even turns and the sparse one on odd, as grok-build alternates
// them. Which full text it is — a plan written or none yet — is read from the
// plan file here, at composition, and never again.
func (m *modes) planStanding(parity int) string {
	switch {
	case parity%2 == 1:
		return variantPlanSparse
	case planHasContent(m.planPath):
		return variantPlanFullWritten
	}
	return variantPlanFullEmpty
}

// sent records that the request carrying r has gone out, which is all one
// request can settle: a plan reminder the model read advances the alternation
// — but only if nothing reset it meanwhile, since a SetMode since the compose
// wants the full text next, and its generation is how that is told from a
// counter that happens to have come back round to the same number.
//
// It deliberately does not record told. A request that went out can still end
// in nothing the conversation keeps — a failure, a cancel, a step that only
// thought — and the next turn's history, rebuilt from a transcript that holds
// no reminder of that request's (an interrupted save writes none), would then
// leave the model with a mode it was told about in a turn that left no trace
// (plan 023 §3.3). heard is that half.
func (m *modes) sent(r pendingReminder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.counts && m.gen == r.gen {
		m.turns++
	}
}

// heard records that a step whose request carried the notice for mode has
// persisted output: from here the conversation itself holds that step, so the
// model has been told about the mode and no later turn announces it again
// (plan 023 §3.3).
func (m *modes) heard(mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.told = mode
}

// toldMode is the mode the model has been told of for good (heard).
func (m *modes) toldMode() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.told
}

// planHasContent reports whether the plan file holds anything. A file that
// cannot be read counts as empty: the reminder then tells the model to write
// the plan, which is what it would do anyway.
func planHasContent(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

// reminder is one reminder the turn shows the model: the message, the index
// it is re-inserted at in every later step's input, and its variant, which the
// append of the step that first carried it writes at that place (plan 028
// §3.15). It is deliberately not a splice (steer.go): nothing that reads the
// steers may see a reminder, and two types are how that stays true.
type reminder struct {
	at      int
	msg     fantasy.Message
	variant string
}

// remind composes the reminder this step's request will carry, if any, and
// records it at the end of the step's own messages — right after the user's
// prompt at step 0, and at the boundary where it first appears for a
// transition notice, which never moves or replaces one sent earlier. A
// turn's first request, and every later step of a segment, compose as they
// always have (reminderFor: R3-2 keeps the per-turn alternation); the first
// request of a segment after a restart composes over the rebuilt history
// (restartReminder, plan 028 §3.11 table). mu is held.
func (t *turn) remind(step int, base []fantasy.Message) {
	var r pendingReminder
	var ok bool
	if t.segmentFirstRequest && !t.turnFirstRequest {
		r, ok = t.modes.restartReminder(t.carried, t.lastReminder())
	} else {
		r, ok = t.modes.reminderFor(step, t.carried)
	}
	if !ok {
		return
	}
	t.reminders = append(t.reminders, reminder{at: len(base), msg: reminderMessage(r.text), variant: r.variant})
	t.pending, t.hasPending = r, true
	t.carried = r.mode
}

// unwrittenReminders are the reminders no append has written yet. They are
// always a suffix, as the steers are: an append writes every one taken up
// before it, or none. mu is held.
func (t *turn) unwrittenReminders() []reminder { return t.reminders[t.remWritten:] }

// lastReminder is the variant of the last reminder the next request's
// history holds, at a segment's first request (restartReminder, plan 028
// §3.11 table, R4-1): the last of the reminders carried over uncommitted,
// which go out after the rebuilt history, else the rebuilt history's own
// last reminder entry (retainedReminder), else "". mu is held.
func (t *turn) lastReminder() string {
	if n := len(t.reminders); n > 0 {
		return t.reminders[n-1].variant
	}
	return t.retainedReminder
}

// lastReminderVariant is the variant of the last reminder entry among steps,
// a context's (store.Steps), or "" when none holds one: what the history
// rebuilt for a restart says the model was last reminded of. A compaction's
// summary is prose and never counts (R2-3).
func lastReminderVariant(steps []store.Step) string {
	last := ""
	for _, st := range steps {
		for _, e := range st.Entries {
			if e.Type == store.TypeReminder {
				last = e.Variant
			}
		}
	}
	return last
}

// reminderSent is called as the step's request goes out (stepStarted): the
// alternation advances here, because that is a fact about requests, and the
// mode this turn has announced is remembered for the step that persists
// output (modeChangeHeld, modeHeard).
//
// It does nothing for a turn whose context is already done. Fantasy calls
// OnStepStart before it attempts the stream, cancelled or not
// (agent.go:1004-1006), so without this a cancel landing between the compose
// and the request would spend a notice on a request that never went out — and
// §3.3 says a withdrawn or cancelled turn keeps its parity. mu is held.
func (t *turn) reminderSent() {
	if !t.hasPending || t.ctx.Err() != nil {
		return
	}
	r := t.pending
	t.pending, t.hasPending = pendingReminder{}, false
	t.sent = r.mode
	t.modes.sent(r)
}

// modeChangeHeld hands the store the mode_change for the notice a request of
// this turn has carried, from stepFinished and just before that step's own
// append: the store writes held changes ahead of everything else in the batch,
// so the entry still leads the step's output and sits where the change became
// visible in the conversation (plan 023 §3.1). A step that persists nothing
// leaves it held, for the next step or turn that does, exactly as a model
// change is held.
//
// A step whose request announced nothing hands over the mode the model was
// last told of for good, which is nearly always what the transcript already
// says and then costs nothing (recordMode). The case it is for: a change held
// by a turn that persisted nothing, and the user back in the old mode before
// any output. Nothing announces that, so without this the stale entry would be
// written with this step and the transcript would name a mode the
// conversation never entered; the hand-over replaces it, as a model switched
// and switched back is replaced at the next turn's start. mu is held.
func (t *turn) modeChangeHeld() {
	mode := t.sent
	if mode == "" {
		mode = t.modes.toldMode()
	}
	// A store that cannot hold the change cannot hold the step either; the
	// turn stops before another request, as a failed append does.
	if err := t.logMode(mode); err != nil && t.saveErr == nil {
		t.saveErr = err
	}
}

// modeHeard records that the step just appended carried the notice and its
// output is in the transcript: the model has been told about the mode for
// good. A turn that fails before output, returns only reasoning or is
// cancelled never reaches here, and the next turn composes the transition
// again (plan 023 §3.3). mu is held.
func (t *turn) modeHeard() {
	if t.sent == "" {
		return
	}
	t.modes.heard(t.sent)
	t.sent = ""
}

// reminderMessage is one reminder as the model reads it: a user message
// wrapped in <system-reminder> tags, which is how grok-build injects its own
// (conversation.rs:1109-1123, reminders.rs:10-18).
func reminderMessage(text string) fantasy.Message {
	return fantasy.NewUserMessage("<" + reminderTag + ">\n" + escapeReminder(text) + "\n</" + reminderTag + ">")
}

// escapeReminder is grok-build's escape_reminder_close_tag. The texts are
// craze's own, but they carry the plan file's absolute path, which is built
// from the harness home a user configured: a closing tag spelled across two
// directory names would otherwise end the wrapper early.
func escapeReminder(text string) string {
	return strings.ReplaceAll(text, "</"+reminderTag+">", `<\/`+reminderTag+">")
}

// normalizeMode is the mode id names: "" and "agent" are agent mode, "plan"
// and "ask" themselves. Anything else is ErrUnknownMode — the adapter
// resolves a user's word to one of these three first (plan 023 §3.6).
func normalizeMode(id string) (string, error) {
	switch id {
	case "", modeAgent:
		return modeAgent, nil
	case modePlan, modeAsk:
		return id, nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnknownMode, id)
}
