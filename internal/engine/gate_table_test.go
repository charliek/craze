package engine_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/tui"
)

// The gate table (plan 027 §3.5, SF-13, A13): what every verb a client can send
// answers in every state the engine can be in. It is DATA — gateTable below —
// and TestTheGateTableIsTheEngines drives every verb in every row through
// engine.Control and asserts every cell, so the table is the engine's by
// construction. The published table in docs/reference/protocol.md is rendered
// from this one (gateTableMarkdown, compared by C10's
// TestPublishedGateTableIsTheTested), so the doc cannot drift from the test,
// and the test cannot drift from the engine.
//
// S2 changes no gate: a cell the engine proves different from §3.5's draft is
// corrected HERE, never in the engine, and recorded as an execution amendment.
//
// A cell is the answer at its row's cut — "allowed" (or, for a prompt, what
// became of it: starts, queues, arms), else the code, with the reason beside it
// where the two differ — plus the conditions that change it (gateVariant), each
// of them driven and asserted too. Where the answer past the engine's own gate
// is the session's, the rows are cut on tui.Stub, whose refusals mirror the
// live session's (Interject's capability check, then its in-turn check), and
// the cell says so in a note.

// gateAsk is the id every rig's ask is opened under.
const gateAsk = "gate-ask"

// gateSession is tui.Stub with the two things a gate table needs that the Stub
// does not do on its own. It REFUSES A CLAIM while its foreign-turn flag is up,
// with agent.ErrForeignTurn, when refuseForeign is set — grok's answer when its
// flag lags craze's, which is what the waiting row is made of (the Stub runs any
// prompt it is handed). And it has a per-child stop (agent.SubagentCanceller)
// that delivers every stop, so subagent.cancel reaches the session wherever the
// engine lets it through.
type gateSession struct {
	*tui.Stub
	// refuseForeign is set at construction and never written again.
	refuseForeign bool
}

func (s *gateSession) Begin(text string) func(context.Context) (agent.Result, error) {
	if s.refuseForeign && s.ForeignTurn() {
		return func(context.Context) (agent.Result, error) { return agent.Result{}, agent.ErrForeignTurn }
	}
	return s.Stub.Begin(text)
}

func (s *gateSession) CancelSubagent(string) error { return nil }

var _ agent.SubagentCanceller = (*gateSession)(nil)

// gateOpts is how one cell is driven on top of its row's cut.
type gateOpts struct {
	// ask: an ask is pending — opened at the cut, or before the close for the
	// closed row — and answer answers it.
	ask bool
	// foreign: the agent is running a turn of its own (the session's flag, and
	// its event).
	foreign bool
	// noInterject: the provider cannot interject (cursor), where every other
	// rig's can (grok).
	noInterject bool
	// armed: a send-now is armed against the running turn, and the cancel it
	// asked for is held before the session, so the arm stands.
	armed bool
	// staleTurn: the cancel names a turn that is not current.
	staleTurn bool
	// unknownAsk: answer names an id this incarnation never issued, instead of
	// the one the cut opened — the closing row's "without an open ask" half
	// (astra r2, plan 027 PR 1).
	unknownAsk bool
}

// gateRig is one engine at one row's cut, over a fresh gateSession.
type gateRig struct {
	t      *testing.T
	stub   *gateSession
	e      *engine.Engine
	opts   gateOpts
	client string
	n      int

	// holds is how many of the next cancels BeforeSessionCancel parks, until
	// release; entered hears each one park.
	mu       sync.Mutex
	holds    int
	entered  chan struct{}
	release  chan struct{}
	released sync.Once
	// returned hears every continuation come back (TurnReturned).
	returned chan string
	// bg is the goroutines the cut started, joined before the engine closes.
	bg sync.WaitGroup

	// holdClose says the "closing" row's cut is holding Close at
	// BeforeSessionClose: closeEntered hears it park there, and closeRelease
	// lets it go, once (closeReleased) — the closing window astra r2 found
	// untested (plan 027 PR 1, C2a).
	holdClose     bool
	closeEntered  chan struct{}
	closeRelease  chan struct{}
	closeReleased sync.Once
}

func (g *gateRig) cmd() engine.Command {
	g.n++
	return engine.Command{Client: g.client, ID: strconv.Itoa(g.n)}
}

func (g *gateRig) beforeCancel(string) {
	g.mu.Lock()
	hold := g.holds > 0
	if hold {
		g.holds--
	}
	g.mu.Unlock()
	if !hold {
		return
	}
	g.entered <- struct{}{}
	<-g.release
}

// beforeSessionClose is BeforeSessionClose: it parks Close after e.mu is
// released and before Session.Close, for the "closing" row's cut, and is a
// no-op for every other row. closeEntered is buffered (capacity 1) so this
// send never blocks: if the cut's await(closeEntered) already gave up (its
// own watchdog fired before Close reached this point) and t.Cleanup has
// since closed closeRelease, this goroutine still has somewhere to put its
// signal and goes straight on to receive from closeRelease, instead of
// blocking forever on a send nobody is left to receive — which would in turn
// hang the Cleanup that joins it (g.bg.Wait).
func (g *gateRig) beforeSessionClose() {
	if !g.holdClose {
		return
	}
	g.closeEntered <- struct{}{}
	<-g.closeRelease
}

// holdCancels parks the next n cancels before the session hears of them.
func (g *gateRig) holdCancels(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.holds += n
}

func gateCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	t.Cleanup(cancel)
	return ctx
}

// start is Start, which the rows past starting are cut after.
func (g *gateRig) start() {
	g.t.Helper()
	if err := g.e.Start(gateCtx(g.t)); err != nil {
		g.t.Fatal(err)
	}
}

// pending puts in place what the cell's options say is pending: an ask, a
// foreign turn.
func (g *gateRig) pending() {
	g.t.Helper()
	if g.opts.ask {
		g.stub.Emit(agent.Event{Type: agent.EventPermission, Permission: stubPermission(gateAsk)})
	}
	if g.opts.foreign {
		g.stub.SetForeignTurn(agent.ForeignTurnInfo{Running: true})
	}
}

// working opens a turn of craze's own that stays open until it is cancelled.
func (g *gateRig) working() {
	g.t.Helper()
	open := g.stub.HangNext()
	if res, err := g.e.Submit(g.cmd(), "working", engine.SubmitQueue, ""); err != nil || res.Turn == "" {
		g.t.Fatalf("the working turn: %+v, %v", res, err)
	}
	await(g.t, open, "the working turn to open")
}

// arm arms a send-now against the running turn when the options ask for one,
// holding the cancel it asks for so that the turn cannot settle and fire it.
func (g *gateRig) arm() {
	g.t.Helper()
	if !g.opts.armed {
		return
	}
	g.holdCancels(1)
	if res, err := g.e.Submit(g.cmd(), "armed", engine.SubmitSendNow, ""); err != nil || !res.Armed {
		g.t.Fatalf("arming a send-now: %+v, %v", res, err)
	}
}

// await waits on a barrier with the watchdog.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(watchdog):
		t.Fatalf("still waiting for %s after %s", what, watchdog)
	}
}

// gateRow is one state: how an engine is brought to it, what State says there,
// and the answer of every column.
type gateRow struct {
	name string
	// retry is craze prompt's chain policy (ChainPolicy.RetryForeignTurn), the
	// only one under which a refused claim waits.
	retry bool
	cut   func(g *gateRig)
	state func(st engine.State) bool
	cells []gateCell
	// overlay marks the ask-pending row: not a cut of its own, but every other
	// row's cut with an ask pending, each cell "as the row" unless it says
	// otherwise.
	overlay bool
}

// gateCell is one answer: want at the row's cut, a note for the published
// cell, and the conditions that change it.
type gateCell struct {
	want string
	note string
	also []gateVariant
}

// gateVariant is a condition that changes a cell — or, said is set, one the
// cell's note already names — and the answer under it. Every variant is
// driven and asserted; said only keeps it out of the rendering.
type gateVariant struct {
	when string
	opts gateOpts
	want string
	said bool
}

// asTheRow is the overlay row's cell that is whatever the row under it is.
const asTheRow = "as the row"

// markdown is the cell as the published table shows it: the answer, the note
// after a dash, then each condition and its answer.
func (c gateCell) markdown() string {
	s := renderToken(c.want)
	if c.note != "" {
		s += " — " + c.note
	}
	for _, v := range c.also {
		if !v.said {
			s += "; " + renderToken(v.want) + " " + v.when
		}
	}
	return s
}

// renderToken puts a code, and a reason beside it, in backticks; a word that
// is not a code is left as it is.
func renderToken(tok string) string {
	switch tok {
	case "allowed", "starts", "queues", "arms", asTheRow:
		return tok
	}
	if code, reason, ok := strings.Cut(tok, " ("); ok {
		return "`" + code + "` (`" + strings.TrimSuffix(reason, ")") + "`)"
	}
	return "`" + tok + "`"
}

// gateToken is what a verb came to, in a cell's words: its outcome when it
// succeeded, else its code — with its reason beside it when the two differ.
func gateToken(outcome string, err error) string {
	if err == nil {
		return outcome
	}
	code, reason := engine.Code(err), engine.Reason(err)
	if reason != code {
		return code + " (" + reason + ")"
	}
	return code
}

// submitOutcome is what became of a prompt.
func submitOutcome(res engine.SubmitResult, err error) (string, error) {
	switch {
	case err != nil:
		return "", err
	case res.Turn != "":
		return "starts", nil
	case res.Queued != nil:
		return "queues", nil
	case res.Armed:
		return "arms", nil
	}
	return "", errors.New("a submit that did nothing")
}

// gateColumn is one verb, as a client sends it.
type gateColumn struct {
	name string
	// needsAsk: the verb acts on an ask, so every cut it runs on has one.
	needsAsk bool
	run      func(g *gateRig) (string, error)
}

var gateColumns = []gateColumn{
	{name: "prompt queue", run: func(g *gateRig) (string, error) {
		return submitOutcome(g.e.Submit(g.cmd(), "gate", engine.SubmitQueue, ""))
	}},
	{name: "prompt send_now", run: func(g *gateRig) (string, error) {
		return submitOutcome(g.e.Submit(g.cmd(), "gate", engine.SubmitSendNow, ""))
	}},
	{name: "interject", run: func(g *gateRig) (string, error) {
		return "allowed", g.e.Interject(gateCtx(g.t), g.cmd(), "gate")
	}},
	{name: "cancel", run: func(g *gateRig) (string, error) {
		turn := ""
		if g.opts.staleTurn {
			turn = "turn-99"
		}
		_, err := g.e.Cancel(gateCtx(g.t), g.cmd(), turn)
		return "allowed", err
	}},
	{name: "disarm", run: func(g *gateRig) (string, error) {
		return "allowed", g.e.Disarm(g.cmd())
	}},
	{name: "queue verbs", run: queueVerbs},
	{name: "set", run: func(g *gateRig) (string, error) {
		_, err := g.e.Set(gateCtx(g.t), g.cmd(), engine.Setting{Kind: engine.SettingMode, Value: "plan"})
		return "allowed", err
	}},
	{name: "setTitle", run: func(g *gateRig) (string, error) {
		return "allowed", g.e.SetTitle(g.cmd(), "gate")
	}},
	{name: "answer", needsAsk: true, run: func(g *gateRig) (string, error) {
		id := gateAsk
		if g.opts.unknownAsk {
			// The cut still opened gateAsk (needsAsk forces opts.ask), so this
			// names one this incarnation never issued instead.
			id = gateAsk + "-unknown"
		}
		return "allowed", g.e.Answer(g.cmd(), id, agent.AskAnswer{OptionID: "allow-once"})
	}},
	{name: "subagent.cancel", run: func(g *gateRig) (string, error) {
		return "allowed", g.e.CancelSubagent(g.cmd(), "child-1")
	}},
}

// queueVerbs is the four queue verbs on one cut, in the order a row lives —
// queued, edited, taken back, and a clear — and their one answer: they are one
// column because the engine gates all four alike, and a cut where they
// disagreed would fail here rather than be averaged into a cell.
func queueVerbs(g *gateRig) (string, error) {
	row, qerr := g.e.Queue(g.cmd(), "gate")
	id := row.ID
	if id == "" {
		id = "q-none"
	}
	eerr := g.e.EditQueued(g.cmd(), id, "gate, edited", nil)
	_, uerr := g.e.Unqueue(g.cmd(), id)
	_, cerr := g.e.ClearQueue(g.cmd())
	got := map[string]string{
		"Queue":      gateToken("allowed", qerr),
		"EditQueued": gateToken("allowed", eerr),
		"Unqueue":    gateToken("allowed", uerr),
		"ClearQueue": gateToken("allowed", cerr),
	}
	first := got["Queue"]
	for verb, tok := range got {
		if tok != first {
			g.t.Errorf("the queue verbs disagree: %s answered %q and Queue %q (%v)", verb, tok, first, got)
		}
	}
	return "allowed", qerr
}

// The conditions the table names, in its words.
var (
	ifAsk        = gateVariant{when: "if an ask is pending", opts: gateOpts{ask: true}, want: "allowed"}
	ifForeign    = gateVariant{when: "if a foreign turn runs", opts: gateOpts{foreign: true}, want: "allowed"}
	ifStale      = gateVariant{when: "naming a turn that is not current", opts: gateOpts{staleTurn: true}, want: "stale_turn"}
	ifNoInterj   = gateVariant{when: "if the provider cannot interject", opts: gateOpts{noInterject: true}, want: "unsupported"}
	ifArmedSend  = gateVariant{when: "if one is armed", opts: gateOpts{armed: true}, want: "already_submitted"}
	ifArmedDisar = gateVariant{when: "if a send-now is armed", opts: gateOpts{armed: true}, want: "allowed"}
	ifUnknownAsk = gateVariant{when: "naming an ask never opened", opts: gateOpts{unknownAsk: true}, want: "unknown_ask"}
)

// refused is a row every verb of which the engine's own gate refuses, but for
// the cancel's conditions, the answer and the sub-agent stop.
func refused() []gateCell {
	na := gateCell{want: "not_accepting"}
	return []gateCell{
		na, na, na,
		{want: "not_accepting", also: []gateVariant{ifAsk, ifForeign, ifStale}},
		na, na, na, na,
		{want: "allowed"},
		{want: "allowed", note: "the session's answer"},
	}
}

// closingCancelCell is the cancel cell the closing and closed rows share:
// e.closed is checked before holdCancelLocked's own conditions (an ask
// pending, a foreign turn, no turn at all), so nothing admits it despite any
// of them (astra r2, plan 027 PR 1).
func closingCancelCell() gateCell {
	return gateCell{want: "not_accepting", note: "always, even with a foreign turn running or a turn named",
		also: []gateVariant{
			{when: "if a foreign turn runs", opts: gateOpts{foreign: true}, want: "not_accepting", said: true},
			{when: "naming a turn", opts: gateOpts{staleTurn: true}, want: "not_accepting", said: true},
		}}
}

// gateTable is the table: the engine's real states, then the two overlays.
func gateTable() []gateRow {
	allowed := gateCell{want: "allowed"}
	session := gateCell{want: "allowed", note: "the session's answer"}
	return []gateRow{
		{
			name:  "starting",
			cut:   func(g *gateRig) { g.pending() },
			state: func(st engine.State) bool { return st.Activity == engine.ActivityStarting },
			cells: refused(),
		},
		{
			// Driven over a STARTED, idle engine (astra r2, plan 027 PR 1): the
			// original fixture emitted this before Start, so State's replaying
			// override (activity Starting || Idle, plus the flag) was masked by
			// refusalLocked's own starting branch, which is checked first — every
			// refusal came from THAT case, and the replay check below it
			// (isReplaying) was never the one proven. Starting first means the
			// engine is Idle when the flag goes up, so a verb's not_accepting can
			// only be the replay gate.
			name: "replaying",
			cut: func(g *gateRig) {
				g.start()
				g.stub.Emit(agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: agent.ReplayStart}})
				g.pending()
			},
			state: func(st engine.State) bool { return st.Activity == engine.ActivityReplaying },
			cells: refused(),
		},
		{
			name:  "idle",
			cut:   func(g *gateRig) { g.start(); g.pending() },
			state: func(st engine.State) bool { return st.Activity == engine.ActivityIdle && st.Turn == "" },
			cells: []gateCell{
				{want: "starts"},
				{want: "starts"},
				{want: "not_accepting (not_in_turn)", also: []gateVariant{ifNoInterj}},
				{want: "not_accepting", also: []gateVariant{ifAsk, ifForeign, ifStale}},
				{want: "not_accepting", note: "nothing is armed"},
				allowed, allowed, allowed, allowed, session,
			},
		},
		{
			name:  "working",
			cut:   func(g *gateRig) { g.start(); g.working(); g.pending(); g.arm() },
			state: func(st engine.State) bool { return st.Activity == engine.ActivityWorking && st.Turn != "" },
			cells: []gateCell{
				{want: "queues"},
				{want: "arms", also: []gateVariant{ifArmedSend}},
				{want: "allowed", also: []gateVariant{ifNoInterj}},
				{want: "allowed", also: []gateVariant{ifStale}},
				{want: "not_accepting", also: []gateVariant{ifArmedDisar}},
				allowed, allowed, allowed, allowed, session,
			},
		},
		{
			// A cancel in flight: its hold is taken and the session has not yet
			// been told. No turn may start by any path until it returns.
			name: "cancelling",
			cut: func(g *gateRig) {
				g.start()
				g.working()
				g.holdCancels(1)
				c := g.cmd()
				g.bg.Add(1)
				go func() {
					defer g.bg.Done()
					_, _ = g.e.Cancel(context.Background(), c, "")
				}()
				await(g.t, g.entered, "the cancel to be held")
				g.pending()
				g.arm()
			},
			state: func(st engine.State) bool { return st.Activity == engine.ActivityWorking && st.Turn != "" },
			cells: []gateCell{
				{want: "queues"},
				{want: "arms", also: []gateVariant{ifArmedSend}},
				{want: "allowed", note: "the session's answer: it has not been told of the cancel yet",
					also: []gateVariant{ifNoInterj}},
				{want: "allowed", also: []gateVariant{ifStale}},
				{want: "not_accepting", also: []gateVariant{ifArmedDisar}},
				allowed, allowed, allowed, allowed, session,
			},
		},
		{
			// A turn whose claim the session refused because the agent is
			// running one of its own, kept current and claimed again once that
			// ends (State.Waiting) — craze prompt's chain policy only; the TUI
			// shows the refusal instead.
			name:  "waiting",
			retry: true,
			cut: func(g *gateRig) {
				g.start()
				g.stub.SetForeignTurn(agent.ForeignTurnInfo{Running: true})
				res, err := g.e.Submit(g.cmd(), "waiting", engine.SubmitQueue, "")
				if err != nil || res.Turn == "" {
					g.t.Fatalf("the waiting turn: %+v, %v", res, err)
				}
				select {
				case <-g.returned:
				case <-time.After(watchdog):
					g.t.Fatal("the refused claim never came back")
				}
				g.pending()
			},
			state: func(st engine.State) bool { return st.Activity == engine.ActivityWorking && st.Waiting },
			cells: []gateCell{
				{want: "queues"},
				{want: "foreign_turn"},
				{want: "not_accepting (not_in_turn)", note: "the session's answer: no turn of craze's is open"},
				{want: "allowed", also: []gateVariant{ifStale}},
				{want: "not_accepting", note: "nothing can be armed"},
				allowed, allowed, allowed, allowed, session,
			},
		},
		{
			name: "error (start failed)",
			cut: func(g *gateRig) {
				g.pending()
				g.e.Started(errors.New("the agent did not start"))
			},
			state: func(st engine.State) bool { return st.Activity == engine.ActivityError && st.StartFailed },
			cells: refused(),
		},
		{
			// Stop: nothing more is admitted, the queue is cleared, and what
			// ran is cancelled.
			name: "stopped",
			cut: func(g *gateRig) {
				g.start()
				if err := g.e.Stop(gateCtx(g.t), g.cmd()); err != nil {
					g.t.Fatalf("stop: %v", err)
				}
				g.pending()
			},
			state: func(st engine.State) bool { return st.Activity == engine.ActivityIdle && st.Turn == "" },
			cells: refused(),
		},
		{
			// The window between the section that refuses admission (closed =
			// true, ActivityClosing set, e.mu released) and the session's OWN
			// close, which is what ends every open ask "closing" (tui.Stub.Close,
			// as the live session's does): astra r2 found the combined row's
			// answer cell wrong here, because the original fixture called Close
			// synchronously and asserted only after it returned, never driving
			// this window at all (plan 027 PR 1, C2a). BeforeSessionClose (the
			// "closing" hold) parks Close right there; every other engine gate
			// is closed already (closed is checked before anything else), so
			// only answer, which the engine gates not at all, tells the two
			// rows apart.
			name: "closing",
			cut: func(g *gateRig) {
				g.start()
				g.pending()
				g.holdClose = true
				g.bg.Add(1)
				go func() {
					defer g.bg.Done()
					_ = g.e.Close()
				}()
				await(g.t, g.closeEntered, "Close to reach the session's own close")
			},
			state: func(st engine.State) bool { return st.Activity == engine.ActivityClosing },
			cells: []gateCell{
				{want: "not_accepting"},
				{want: "not_accepting"},
				{want: "not_accepting"},
				closingCancelCell(),
				{want: "not_accepting"},
				{want: "not_accepting"},
				{want: "not_accepting"},
				{want: "not_accepting"},
				{want: "allowed", note: "the registry is still open: the session's own close, not yet run, is what ends every ask",
					also: []gateVariant{ifUnknownAsk}},
				{want: "not_accepting"},
			},
		},
		{
			// After Session.Close has returned: every ask it was holding ended
			// `closing`, so answer goes back to already_resolved.
			name: "closed",
			cut: func(g *gateRig) {
				g.start()
				g.pending()
				if err := g.e.Close(); err != nil {
					g.t.Fatalf("close: %v", err)
				}
			},
			state: func(st engine.State) bool { return st.Activity == engine.ActivityClosing },
			cells: []gateCell{
				{want: "not_accepting"},
				{want: "not_accepting"},
				{want: "not_accepting"},
				closingCancelCell(),
				{want: "not_accepting"},
				{want: "not_accepting"},
				{want: "not_accepting"},
				{want: "not_accepting"},
				{want: "already_resolved", note: "the close ended every ask"},
				{want: "not_accepting"},
			},
		},
		{
			// The agent running a turn of its own, over idle: what the grok
			// fallback produces.
			name: "+ foreign turn",
			cut: func(g *gateRig) {
				g.start()
				g.stub.SetForeignTurn(agent.ForeignTurnInfo{Running: true})
				g.pending()
			},
			state: func(st engine.State) bool {
				return st.Activity == engine.ActivityIdle && st.Turn == "" && st.ForeignTurn
			},
			cells: []gateCell{
				{want: "queues"},
				{want: "foreign_turn"},
				{want: "not_accepting (not_in_turn)", note: "the session's answer"},
				{want: "allowed", note: "written at once", also: []gateVariant{ifStale}},
				{want: "not_accepting", note: "nothing can be armed"},
				allowed, allowed, allowed, allowed, session,
			},
		},
		{
			name:    "+ ask pending",
			overlay: true,
			cells: []gateCell{
				{want: asTheRow}, {want: asTheRow}, {want: asTheRow},
				{want: "allowed", note: "in every row but closing or closed, where e.closed refuses it outright"},
				{want: asTheRow}, {want: asTheRow}, {want: asTheRow}, {want: asTheRow}, {want: asTheRow}, {want: asTheRow},
			},
		},
	}
}

// newGateRig is a fresh engine over a fresh gateSession, brought to row's cut
// with opts, and checked to be in that state.
func newGateRig(t *testing.T, row gateRow, opts gateOpts) *gateRig {
	t.Helper()
	stub := tui.NewStubNoPrimary()
	if opts.noInterject {
		stub.SetProvider(agent.CursorProvider())
	} else {
		stub.SetProvider(agent.GrokProvider())
	}
	g := &gateRig{
		t:            t,
		stub:         &gateSession{Stub: stub, refuseForeign: row.retry},
		opts:         opts,
		entered:      make(chan struct{}, 8),
		release:      make(chan struct{}),
		returned:     make(chan string, 16),
		closeEntered: make(chan struct{}, 1),
		closeRelease: make(chan struct{}),
	}
	e, err := engine.NewForGateTable(g.stub, engine.Options{Chain: engine.ChainPolicy{RetryForeignTurn: row.retry}},
		engine.GateTableHooks{
			BeforeSessionCancel: g.beforeCancel,
			TurnReturned: func(id string) {
				select {
				case g.returned <- id:
				default:
				}
			},
			BeforeSessionClose: g.beforeSessionClose,
		})
	if err != nil {
		t.Fatal(err)
	}
	g.e = e
	// Last registered runs first: the close hold goes, then the held cancels,
	// then what the cut started is joined, then the engine closes.
	t.Cleanup(func() { _ = e.Close() })
	t.Cleanup(g.bg.Wait)
	t.Cleanup(func() { g.released.Do(func() { close(g.release) }) })
	t.Cleanup(func() { g.closeReleased.Do(func() { close(g.closeRelease) }) })
	g.client = e.NewClientID()
	row.cut(g)
	st := e.State()
	if !row.state(st) {
		t.Fatalf("the %s cut is not that state: %+v", row.name, st)
	}
	if opts.ask && row.name != "closed" && st.PendingAsks != 1 {
		t.Fatalf("the %s cut has %d asks pending, want the one it opened", row.name, st.PendingAsks)
	}
	return g
}

// wantGate drives col on a fresh rig at row's cut with opts, and checks the
// answer.
func wantGate(t *testing.T, row gateRow, col gateColumn, opts gateOpts, want string) {
	t.Helper()
	opts.ask = opts.ask || col.needsAsk
	g := newGateRig(t, row, opts)
	if got := gateToken(col.run(g)); got != want {
		t.Errorf("%s, %s (%+v): %q, want %q", row.name, col.name, opts, got, want)
	}
}

// TestTheGateTableIsTheEngines is A13: every verb in every row, through
// Control, each cell and each of its conditions asserted — and the ask-pending
// overlay driven over every row it can stand on, where every cell must be the
// row's own but the cancel's.
func TestTheGateTableIsTheEngines(t *testing.T) {
	table := gateTable()
	var overlay gateRow
	for _, row := range table {
		if len(row.cells) != len(gateColumns) {
			t.Fatalf("the %s row has %d cells for %d columns", row.name, len(row.cells), len(gateColumns))
		}
		if row.overlay {
			overlay = row
			continue
		}
		for i, col := range gateColumns {
			cell := row.cells[i]
			t.Run(row.name+"/"+col.name, func(t *testing.T) {
				wantGate(t, row, col, gateOpts{}, cell.want)
				for _, v := range cell.also {
					t.Run(v.when, func(t *testing.T) { wantGate(t, row, col, v.opts, v.want) })
				}
			})
		}
	}
	for _, row := range table {
		if row.overlay || row.name == "closing" || row.name == "closed" {
			continue
		}
		for i, col := range gateColumns {
			want := overlay.cells[i].want
			if want == asTheRow {
				want = row.cells[i].want
			}
			t.Run(overlay.name+"/"+row.name+"/"+col.name, func(t *testing.T) {
				wantGate(t, row, col, gateOpts{ask: true}, want)
			})
		}
	}
}

// gateTableMarkdown renders the tested table as the published doc shows it:
// one row per state, the columns in order, codes in backticks. It is what
// docs/reference/protocol.md's gate table is compared with
// (TestPublishedGateTableIsTheTested, C10), so it is deterministic — the
// table's own order, nothing from a map.
func gateTableMarkdown() string {
	var b strings.Builder
	b.WriteString("| state |")
	for _, col := range gateColumns {
		b.WriteString(" " + col.name + " |")
	}
	b.WriteString("\n|---|")
	for range gateColumns {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, row := range gateTable() {
		b.WriteString("| " + row.name + " |")
		for _, cell := range row.cells {
			b.WriteString(" " + cell.markdown() + " |")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// TestTheGateTableRenders holds the renderer to the table's shape — a header,
// a rule, and one line per row, each with a cell per column — and to being the
// same twice.
func TestTheGateTableRenders(t *testing.T) {
	md := gateTableMarkdown()
	if md != gateTableMarkdown() {
		t.Fatal("the gate table renders differently twice")
	}
	lines := strings.Split(strings.TrimSuffix(md, "\n"), "\n")
	if want := len(gateTable()) + 2; len(lines) != want {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), want, md)
	}
	for i, line := range lines {
		if got := strings.Count(line, "|"); got != len(gateColumns)+2 {
			t.Fatalf("line %d has %d separators, want %d: %s", i, got, len(gateColumns)+2, line)
		}
	}
	t.Logf("\n%s", md)
}
