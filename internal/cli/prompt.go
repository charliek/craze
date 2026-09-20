package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

const (
	// foreignTurnMax bounds the wait for a turn the agent started on its own
	// (grok's interject fallback). Past it craze stops rather than sending a
	// prompt that would be queued behind a turn nobody asked for.
	foreignTurnMax = 60 * time.Second
	// foreignPollCap and foreignPollMin bound how often a run looks at the
	// engine's state while it waits on such a turn. The cap keeps a minute of
	// waiting down to a few hundred wake-ups; the floor keeps a look from
	// falling between two of the engine's own retries (its tick is 10ms), which
	// would read a claim that keeps being refused as one that went through.
	foreignPollCap   = 200 * time.Millisecond
	foreignPollMin   = 25 * time.Millisecond
	subagentDrainMax = 1500 * time.Millisecond
	subagentQuiet    = 250 * time.Millisecond
	// subagentSweepMax bounds the final sweep so a producer that keeps
	// writing cannot hold the deadline open for ever; it is the session's
	// event buffer, so one pass empties a full channel.
	subagentSweepMax = 256
)

type promptOpts struct {
	workspace string
	model     string
	agentBin  string
	provider  string
	followUps []string
	decisions []string
	// pluginDirs are extra plugin roots, cursor-agent's --plugin-dir spelled
	// the same way. They are craze's own: the child agent is never told.
	pluginDirs []string
	force      bool
	noForce    bool
	ask        bool
	plan       bool
	json       bool
	text       string
	stdout     io.Writer
	stderr     io.Writer
	stdin      io.Reader
	cmd        *cobra.Command
	// foreignMax overrides foreignTurnMax. Zero is the real bound; the tests
	// that drive the give-up path set a short one, because a minute of waiting
	// is not a test anyone runs.
	foreignMax time.Duration
	// eng is the run's engine, set once drive has one. The final sweeps need it
	// for Sync — the barrier that makes "enqueued" mean "delivered" — and it is
	// nil for a test that drives finishRun over a session alone, where every
	// event was published directly and there is nothing asynchronous to wait for.
	eng *engine.Engine
	// beforeGiveUp is a test seam, nil in production: it runs on the run's own
	// goroutine between deciding to give up on a refused claim and asking the
	// engine to. That gap is where the claim can still go through, and a test that
	// cannot land it there cannot show that the give-up is refused when it does.
	beforeGiveUp func()
}

// foreignBudget is how long craze waits on turns the agent started on its own.
func (o *promptOpts) foreignBudget() time.Duration {
	if o.foreignMax > 0 {
		return o.foreignMax
	}
	return foreignTurnMax
}

// foreignPoll is how often a run looks at the engine's state while it waits:
// often enough that a budget a test shortens is still measured in several
// samples, and bounded at both ends by foreignPollCap and foreignPollMin.
func (o *promptOpts) foreignPoll() time.Duration {
	return min(max(o.foreignBudget()/8, foreignPollMin), foreignPollCap)
}

func newPromptCmd() *cobra.Command {
	o := &promptOpts{force: true}
	cmd := &cobra.Command{
		Use:   "prompt [text]",
		Short: "Run a headless ACP turn (and optional follow-ups)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.stdout = cmd.OutOrStdout()
			o.stderr = cmd.ErrOrStderr()
			o.stdin = cmd.InOrStdin()
			o.cmd = cmd
			if len(args) == 1 {
				o.text = args[0]
			}
			return o.run()
		},
	}
	cmd.Flags().StringVar(&o.workspace, "workspace", "", "existing workspace directory (default: current directory)")
	cmd.Flags().StringVar(&o.model, "model", "", "ACP model id (session/set_model after session/new)")
	cmd.Flags().StringVar(&o.agentBin, "agent-bin", "", "path to cursor-agent / fake agent (or CRAZE_AGENT_BIN)")
	cmd.Flags().StringArrayVar(&o.followUps, "follow-up", nil, "additional prompt on the same ACP session (repeatable)")
	cmd.Flags().StringArrayVar(&o.decisions, "permission-decision", nil, "headless permission answer: allow-once or reject-once (repeatable)")
	registerPluginDirFlag(cmd, &o.pluginDirs)
	cmd.Flags().BoolVar(&o.force, "force", true, "spawn the agent with --force (yolo)")
	cmd.Flags().BoolVar(&o.noForce, "no-force", false, "disable yolo and handle permission requests")
	cmd.Flags().BoolVar(&o.ask, "ask", false, "set session mode to ask after session/new")
	cmd.Flags().BoolVar(&o.plan, "plan", false, "set session mode to plan after session/new")
	cmd.Flags().BoolVar(&o.json, "json", false, "write only JSON events to stdout")
	registerProviderFlag(cmd, &o.provider)
	return cmd
}

func (o *promptOpts) run() (retErr error) {
	if o.ask && o.plan {
		return usagef("craze: --ask and --plan are mutually exclusive")
	}
	for _, d := range o.decisions {
		if _, err := decisionKind(d); err != nil {
			return usagef("craze: %v", err)
		}
	}
	if o.noForce {
		o.force = false
	}
	text, err := o.resolveText()
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(o.workspace)
	if err != nil {
		return err
	}

	resolved, err := resolveProvider(o.cmd, o.provider, o.stderr, false)
	if err != nil {
		return err
	}
	if err := refuseInProcess("craze", resolved.Provider, o.agentBin, o.mode()); err != nil {
		return err
	}

	parent := context.Background()
	if o.cmd != nil {
		parent = o.cmd.Context()
	}
	// SIGHUP too: a closed terminal is an exit like any other, and left to
	// its default action it would kill craze with the agent still running.
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	prov := resolved.Provider
	sess := agent.New(agent.Options{
		Binary:     o.agentBin,
		Workspace:  ws,
		Force:      o.force,
		Model:      o.model,
		Mode:       o.mode(),
		Stderr:     o.stderr,
		PluginDirs: o.pluginDirs,
		Provider:   &prov,
		// Headless craze has one stream for its own notes, stderr, as it
		// does for discoverPlugins' (Options.Diag falls back to it).
		JournalDir: journalDir(o.stderr),
	})
	// The engine is this run's one driver: admission, craze's own message queue,
	// the turn a row leaves the queue to become, and the endings the wire never
	// produced (plan 021 §3.4). The chain policy is `craze prompt`'s own, where
	// a chain of follow-ups is one request: a turn that stopped for any reason
	// but end_turn has ended that request and takes the queue with it, and a
	// prompt the session refused because the agent is running a turn of its own
	// is waited out rather than reported.
	eng, err := engine.New(sess, engine.Options{Chain: engine.ChainPolicy{
		StopOnNonEndTurn: true,
		RetryForeignTurn: true,
	}})
	if err != nil {
		_ = sess.Close()
		return err
	}
	// Closing the engine closes the session, and joins the driver with it.
	defer func() { _ = eng.Close() }()

	if err := eng.Start(ctx); err != nil {
		if o.json {
			writeErrorEvent(o.stdout, err)
		}
		return err
	}
	if err := persistProvider(resolved); err != nil {
		fmt.Fprintf(o.stderr, "craze: not saving the provider: %v\n", err)
	}
	return o.drive(ctx, eng, text)
}

// drive is the headless run over an engine that is already up: the follow-ups
// become its queue, the first prompt starts the chain, and every event the
// session and the engine publish is printed until the chain is over. It is the
// whole of what `craze prompt` does with a session, bar building one, so a test
// can drive the edges a real agent cannot be asked for.
func (o *promptOpts) drive(ctx context.Context, eng *engine.Engine, text string) (retErr error) {
	o.eng = eng
	var inRun atomic.Bool
	inRun.Store(true)
	defer inRun.Store(false)
	// stopped is closed once the signal's own work is done, which is what a run
	// waiting for a drain waits for: the rows are gone from the queue, their
	// removals are delivered, and the cancel has been written. Waiting for the
	// signal itself instead would race the stop — the final sweep could run
	// before the removals were even enqueued, and the JSON would lose them.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		if !inRun.Load() {
			return
		}
		// A signal stops everything pending, not just the running turn. Stop
		// refuses every later admission, clears the queue — one removal event
		// per row, which is how the JSON says where the follow-ups went — waits
		// for those removals to be delivered, and only then cancels what is
		// running, so nothing starts behind the cancel and nothing the cancel
		// ends is printed ahead of them.
		//
		// Its context is not the signal's, which is already done: this is the
		// work the signal asked for, and a cancel made on a dead context would
		// be no cancel at all.
		_ = eng.Stop(context.Background(), engine.Command{})
	}()

	// decisions is the headless permission queue, consumed by whichever reader
	// of the event stream sees the request — the chain's own loop, or the
	// sub-agent drain on the way out.
	decisions := append([]string{}, o.decisions...)
	defer func() { retErr = o.finishRun(eng.Session(), &decisions, retErr) }()

	// --follow-up is the headless queue: every follow-up is queued before the
	// first turn, and the engine's drain sends them one per settled turn, in
	// order, exactly as it does for the TUI.
	for _, f := range o.followUps {
		if _, err := eng.Queue(engine.Command{}, f); err != nil {
			return fmt.Errorf("craze: %w", err)
		}
	}
	return o.readChain(ctx, eng, stopped, text, &decisions)
}

// finishRun is the run's last reader. Whatever the session was still writing
// as the chain ended goes to stdout here — the queue's own last events, a
// signal's removed lines, a child's late output — and a permission among them
// is answered like any other. A write that fails means the JSON on stdout is
// incomplete, which is an error even when everything else succeeded, and a
// permission refused on the way out is still a refusal. retErr is what run is
// about to return: the first failure wins.
func (o *promptOpts) finishRun(sess agent.Session, decisions *[]string, retErr error) error {
	fail := func(err error) {
		if retErr == nil {
			retErr = err
		}
	}
	// Each sweep is preceded by the barrier that makes "enqueued" mean
	// "delivered": what the engine authored on the way out — a signal's queue
	// removals above all — is published asynchronously, and a non-blocking read
	// has nothing to wait for (syncEvents).
	for _, step := range []func(agent.Session, *[]string) (bool, error){
		o.syncEvents, o.flushEvents, o.syncEvents, o.drainSubagents,
	} {
		r, err := step(sess, decisions)
		if r {
			fail(&exitError{code: 1, msg: ""})
		}
		if err != nil {
			fail(err)
		}
	}
	return retErr
}

// readChain submits the first prompt and prints what the session and the engine
// publish until the chain is over.
//
// It drives nothing. The engine decides what runs: the first prompt, then one
// turn per queued row, with the chain policy's verdict on the queue behind a
// turn that did not end end_turn. What is left here is reading, printing,
// answering a permission request, and the two waits the engine hands back to its
// client.
//
// The chain is over on the ending with no successor and nothing queued behind
// it. `ended{Next: "", Pending: 0}` is always the last event of a settlement,
// and an empty queue never waits for a turn the agent started on its own (plan
// 021 §3.4, A8). An ending that has rows pending and no successor is the drain
// held by such a turn: the run keeps reading until that drain's own started
// arrives, until a signal ends the wait, or until the budget runs out — and then
// it says so and exits 1 rather than wait for ever.
//
// The exit statuses are the chain's own, unchanged: any stop reason but end_turn
// is 1, a turn that failed returns its error, a permission any reader refused is
// 1, a signal is 1, and end_turn all the way is 0.
func (o *promptOpts) readChain(ctx context.Context, eng *engine.Engine, stopped <-chan struct{}, text string, decisions *[]string) error {
	if ctx.Err() != nil {
		// A signal ends the run here and nowhere later: a turn started after it
		// runs on a context of its own, so it would run to completion with no
		// second Ctrl+C able to stop it, and the exit status would hide it.
		return &exitError{code: 1, msg: ""}
	}
	res, err := eng.Submit(engine.Command{}, text, engine.SubmitQueue, "")
	if err != nil {
		if ctx.Err() != nil {
			return &exitError{code: 1, msg: ""}
		}
		return fmt.Errorf("craze: %w", err)
	}

	sess := eng.Session()
	// rejected is a permission any reader refused, which counts towards the exit
	// status exactly as one refused inside a turn does.
	rejected, inTurn := false, res.Turn != ""
	var streamErr error
	// waiting says the chain is between turns with rows still queued, and drainUntil
	// is how long this run waits for the drain that is being held. A prompt the
	// engine queued rather than started — nothing does that today, since a direct
	// submit is admitted whatever the agent is doing — is the same wait.
	waiting, drainUntil := !inTurn, time.Time{}
	if waiting {
		drainUntil = time.Now().Add(o.foreignBudget())
	}
	// claimTurn is the turn whose refused claim this run is timing, and claimUntil
	// when it stops waiting for it. The budget is per turn, it starts at the
	// first look that finds that turn waiting, and — this is the whole of the
	// rule — it is NEVER cleared by a look that does not: State.Waiting is false
	// for the instant a re-claim is in flight, and clearing on that would spend
	// the budget for ever, while a genuinely running foreign turn (where the
	// engine rightly declines to claim again) would never be given up on at all.
	// Keying it to the turn is what makes "never cleared" safe: a turn's claim is
	// refused only before it runs, so one turn id has one refusal streak.
	claimTurn, gaveUp := "", false
	var claimUntil time.Time

	// signalDone says the stop a signal asked for has finished: the rows are out
	// of the queue, their removals are delivered, and the cancel has been written.
	signalDone := false

	poll := time.NewTicker(o.foreignPoll())
	defer poll.Stop()
	events := eng.Events()
	for {
		// A signal ends a wait for a drain, and this is when that wait is over: the
		// stop has done its work, so no successor is coming — it cleared the very
		// row this was waiting for — and the turn the AGENT was running has ended,
		// which is the same condition the baseline's own wait read to its end. That
		// last part matters on the stream and not just in the clock: the session
		// publishes the foreign turn's closing bracket when it ends, and a run that
		// exited on the signal alone would leave that line unprinted.
		//
		// Bounded by the same budget as the wait it is part of: an agent that
		// answers neither the cancel nor anything else does not hold the exit open.
		if waiting && signalDone && (!eng.State().ForeignTurn || !time.Now().Before(drainUntil)) {
			return &exitError{code: 1, msg: ""}
		}
		// While a turn of craze's own is running there is nothing to wait for here:
		// its cancelled ending is coming, and it is the ending that ends the chain.
		var signalled <-chan struct{}
		if waiting && !signalDone {
			signalled = stopped
		}
		select {
		case ev, ok := <-events:
			if !ok {
				// Only the log's own close ends the primary, and nothing closes
				// it while this reads: the session went away mid-chain.
				return &exitError{code: 1, msg: ""}
			}
			r, err := o.consume(sess, ev, decisions)
			rejected = rejected || r
			if err != nil {
				return err
			}
			if ev.Type == agent.EventError && inTurn && streamErr == nil && !eng.State().Waiting {
				// "The prompt succeeded but the stream carried an error" is still
				// a failed run, and this is that error — with the scope the
				// baseline's own reader had: the first error event of the turn
				// that is running, and never one from a turn the AGENT is running
				// while craze's claim waits to be taken again (the baseline threw
				// those away with the refused attempt they arrived during).
				//
				// The scope is as close as a client can get, not exact: an engine
				// event trails the state it describes and a session's own event
				// leads it, so an error published in the instant either side of a
				// refusal can be attributed either way. What is exact is the
				// failure of the turn itself, which comes from the engine
				// (eng.TurnErr) and takes precedence over this.
				streamErr = ev.Err
			}
			if ev.Type != agent.EventTurn || ev.Turn == nil {
				continue
			}
			switch ev.Turn.Phase {
			case agent.TurnStarted:
				// A turn is running: the drain that was held has been released,
				// or the successor of the one that just ended is away.
				inTurn, streamErr, waiting = true, nil, false
			case agent.TurnEnded:
				inTurn = false
				over, err := o.turnEnded(ctx, eng, ev.Turn, streamErr, gaveUp, rejected)
				if over {
					return err
				}
				streamErr = nil
				if ev.Turn.Next == "" {
					// Rows behind it and no successor: the drain is held.
					waiting, drainUntil = true, time.Now().Add(o.foreignBudget())
				}
			}
		case <-signalled:
			signalDone = true
		case <-poll.C:
			if waiting {
				if ctx.Err() != nil {
					// A signal is already ending this wait, and what it is waiting
					// for is that stop's completion. The queue is not blocked, it
					// is gone, so there is nothing to report — but the budget still
					// bounds it, or an agent that answers neither the cancel nor
					// anything else would hold the exit open for ever.
					if !time.Now().Before(drainUntil) {
						return &exitError{code: 1, msg: ""}
					}
					continue
				}
				if time.Now().Before(drainUntil) {
					continue
				}
				// The rows behind the turn that ended are not going to run: the
				// agent has held the session for the whole budget.
				fmt.Fprintf(o.stderr, "craze: the queue is still blocked after %s\n", o.foreignBudget())
				return &exitError{code: 1, msg: ""}
			}
			if gaveUp {
				continue
			}
			st := eng.State()
			switch {
			case !st.Waiting:
				// Either the claim went through or a fresh one is in flight, and a
				// single look cannot tell the two apart. Nothing to decide, and
				// nothing to unwind: the budget of whichever turn was waiting
				// stands.
			case st.Turn != claimTurn:
				claimTurn, claimUntil = st.Turn, time.Now().Add(o.foreignBudget())
			case time.Now().Before(claimUntil):
			default:
				// The agent has kept the session for its own turns for the whole
				// budget. GiveUp ends the wait — the turn settles as the refusal
				// it was, with the queue left as it is — and its ending comes back
				// through this loop. It is conditional on the turn still waiting,
				// decided under the engine's own lock, so the claim going through
				// between the look above and this call means the prompt is running
				// after all: the give-up is refused, nothing is cancelled, and
				// this run carries on reading, exactly as it did once the
				// baseline's own Prompt call was accepted.
				if o.beforeGiveUp != nil {
					o.beforeGiveUp()
				}
				if err := eng.GiveUp(engine.Command{}, claimTurn); err == nil {
					gaveUp = true
				}
			}
		}
	}
}

// turnEnded is what one turn's ending means for the run: whether the chain is
// over, and with what. streamErr is the error event that turn published, if any,
// and gaveUp says this run stopped waiting for a claim the agent kept refusing.
//
// The error a failed turn returns is the turn's OWN, from the engine: the value
// its continuation returned (eng.TurnErr), which is what the baseline returned
// from Prompt and what a caller matching on a sentinel needs. TurnInfo.Err is
// text — an event may not carry an error value — so it is only the fallback for a
// turn the engine no longer remembers.
func (o *promptOpts) turnEnded(ctx context.Context, eng *engine.Engine, t *agent.TurnInfo, streamErr error, gaveUp, rejected bool) (bool, error) {
	if gaveUp {
		// The refusal the give-up turned into an ending. A script sees the status
		// the retry has always given it and not the raw wire refusal.
		fmt.Fprintf(o.stderr, "craze: the agent kept the session for its own turns for %s\n", o.foreignBudget())
		return true, &exitError{code: 1, msg: ""}
	}
	// Prompt's own error first, then the stream's: the baseline's precedence. A
	// withdrawn prompt's ErrPromptCancelled comes back here too, which is what a
	// signal between the claim and the turn opening has always printed.
	if err := eng.TurnErr(t.ID); err != nil {
		return true, err
	}
	if t.Err != "" {
		return true, errors.New(t.Err)
	}
	if streamErr != nil {
		return true, streamErr
	}
	switch {
	case t.StopReason != stopEndTurn:
		// The stop reason ends the chain exactly as it did before the queue
		// existed: anything but end_turn is exit 1 and no more turns. The chain
		// policy has already cleared what was queued behind it, each row with a
		// removal of its own, so the JSON still says where the follow-ups went.
		return true, &exitError{code: 1, msg: ""}
	case t.Next != "" || t.Pending > 0:
		// A successor is away, or rows are waiting for a drain that is held.
		return false, nil
	}
	// Nothing ran behind it and nothing is queued: this is the chain's last
	// event. A signal that landed while it drained still ends the run, and so
	// does a permission a reader refused, even when every turn ended end_turn.
	if ctx.Err() != nil || rejected {
		return true, &exitError{code: 1, msg: ""}
	}
	return true, nil
}

// stopEndTurn is the one stop reason that lets the chain carry on.
const stopEndTurn = "end_turn"

// syncEvents waits for everything the engine has enqueued to be delivered,
// printing whatever arrives meanwhile.
//
// It is the barrier the run's final sweeps need. A sweep is a non-blocking read
// of what is already buffered, which was the whole truth while every event came
// from the session itself — "emitted means buffered" — and is not the truth for
// the events the engine authors: those go through the log's outbox, so a sweep
// with nothing to wait for would print a signal's removed lines one run in ten.
//
// Sync runs on a helper goroutine while this one keeps reading, because a Sync
// called from the primary's own reader with the primary full would wait for a
// slot only that reader can free (plan 021 §3.2). Its error is not the run's: it
// can only be a log that is already closing, which has nothing left to order.
// With no engine — a test driving the sweep over a session alone — there is
// nothing asynchronous to wait for and nothing to do.
func (o *promptOpts) syncEvents(sess agent.Session, queue *[]string) (bool, error) {
	if o.eng == nil {
		return false, nil
	}
	synced := make(chan struct{})
	go func() {
		defer close(synced)
		_ = o.eng.Sync(context.Background())
	}()
	rejected := false
	for {
		select {
		case <-synced:
			return rejected, nil
		case ev, ok := <-sess.Events():
			if !ok {
				return rejected, nil
			}
			r, err := o.consume(sess, ev, queue)
			rejected = rejected || r
			if err != nil {
				// Left to finish on its own: waiting for it here, with nothing
				// reading, is the one way Sync can never return.
				return rejected, err
			}
		}
	}
}

// flushEvents writes whatever the session has already emitted without waiting
// for more. A closed channel is an empty one: the session is gone and there is
// nothing left to read.
//
// It reports whether anything it answered was a rejection: a permission refused
// here counts towards the exit status exactly as one refused inside a turn does,
// or the run could exit 0 having said no.
func (o *promptOpts) flushEvents(sess agent.Session, queue *[]string) (bool, error) {
	rejected := false
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				return rejected, nil
			}
			r, err := o.consume(sess, ev, queue)
			rejected = rejected || r
			if err != nil {
				return rejected, err
			}
		default:
			return rejected, nil
		}
	}
}

// consume is what every reader of the event stream does with one event: write
// it, and answer it if it is a permission request. A request that arrives
// outside a turn craze prompted — during a foreign turn, or between turns —
// is still the agent waiting on an answer, so every reader goes through here.
func (o *promptOpts) consume(sess agent.Session, ev agent.Event, queue *[]string) (bool, error) {
	if err := o.writeEvent(ev); err != nil {
		return false, err
	}
	if ev.Type != agent.EventPermission || ev.Permission == nil {
		return false, nil
	}
	return o.answerPermission(sess, ev.Permission, queue)
}

func (o *promptOpts) mode() string {
	if o.ask {
		return "ask"
	}
	if o.plan {
		return "plan"
	}
	return ""
}

func (o *promptOpts) resolveText() (string, error) {
	if strings.TrimSpace(o.text) != "" {
		return o.text, nil
	}
	if stdinIsTTY(o.stdin) {
		return "", usagef("craze: prompt text required (or pipe stdin)")
	}
	b, err := io.ReadAll(o.stdin)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return "", usagef("craze: prompt text required (or pipe stdin)")
	}
	return text, nil
}

func (o *promptOpts) writeEvent(ev agent.Event) error {
	if o.json {
		return encodeEvent(o.stdout, ev)
	}
	if ev.Type == agent.EventText && ev.Agent == "" {
		_, err := io.WriteString(o.stdout, ev.Text)
		return err
	}
	return nil
}

// drainSubagents reads what the children were still writing as the run ended.
// It goes through consume like every other reader: a permission request that
// arrives here is the agent waiting on an answer, and printing it without
// answering would leave the agent hanging and the refusal out of the exit
// status.
func (o *promptOpts) drainSubagents(sess agent.Session, queue *[]string) (bool, error) {
	rejected := false
	err := drainSubagentEvents(sess.Events(), sess.Snapshot, func(ev agent.Event) error {
		r, err := o.consume(sess, ev, queue)
		rejected = rejected || r
		return err
	})
	return rejected, err
}

func drainSubagentEvents(events <-chan agent.Event, snapFn func() agent.Snapshot, write func(agent.Event) error) error {
	if !hasSpawnedSubagent(snapFn()) {
		return nil
	}
	maxTimer := time.NewTimer(subagentDrainMax)
	defer maxTimer.Stop()
	var quiet *time.Timer
	stopQuiet := func() {
		if quiet == nil {
			return
		}
		if !quiet.Stop() {
			select {
			case <-quiet.C:
			default:
			}
		}
		quiet = nil
	}
	defer stopQuiet()

	for {
		running := hasRunningSubagent(snapFn())
		var quietC <-chan time.Time
		if running {
			stopQuiet()
		} else {
			if quiet == nil {
				quiet = time.NewTimer(subagentQuiet)
			}
			quietC = quiet.C
		}
		select {
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if quiet != nil {
				if !quiet.Stop() {
					select {
					case <-quiet.C:
					default:
					}
				}
				quiet.Reset(subagentQuiet)
			}
			if err := write(ev); err != nil {
				return err
			}
		case <-quietC:
			return sweepEvents(events, write)
		case <-maxTimer.C:
			return sweepEvents(events, write)
		}
	}
}

// sweepEvents writes what is already buffered, without blocking. A deadline
// and a ready event can both be ready when select runs, and select picks
// between them at random: without this, a timer could return while the
// session had already produced lines that stdout never got.
func sweepEvents(events <-chan agent.Event, write func(agent.Event) error) error {
	for i := 0; i < subagentSweepMax; i++ {
		select {
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if err := write(ev); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

func hasSpawnedSubagent(snap agent.Snapshot) bool {
	return len(snap.Subagents) > 0
}

func hasRunningSubagent(snap agent.Snapshot) bool {
	for _, a := range snap.Subagents {
		if a.Status == agent.SubagentRunning || a.Status == "" {
			return true
		}
	}
	return false
}

func (o *promptOpts) answerPermission(sess agent.Session, perm *agent.PermissionEvent, queue *[]string) (bool, error) {
	id, rejected, rest, perr := pickPermission(perm.Options, *queue)
	*queue = rest
	if perr != nil {
		if err := sess.AnswerPermission(perm.ID, ""); err != nil {
			return true, err
		}
		return true, nil
	}
	if err := sess.AnswerPermission(perm.ID, id); err != nil {
		return rejected, err
	}
	return rejected, nil
}

func pickPermission(opts []agent.PermissionOption, queue []string) (optionID string, rejected bool, rest []string, err error) {
	for i, raw := range queue {
		kind, decErr := decisionKind(raw)
		if decErr != nil {
			continue
		}
		if id, ok := optionIDForKind(opts, kind); ok {
			rest = append(append([]string{}, queue[:i]...), queue[i+1:]...)
			return id, kind == "reject_once" || kind == "reject_always", rest, nil
		}
	}
	if id, ok := optionIDForKind(opts, "reject_once"); ok {
		return id, true, queue, nil
	}
	return "", true, nil, fmt.Errorf("craze: no permission decision remaining")
}

func optionIDForKind(opts []agent.PermissionOption, kind string) (string, bool) {
	return agent.OptionIDForKind(opts, kind)
}

func decisionKind(raw string) (string, error) {
	switch strings.ReplaceAll(strings.TrimSpace(strings.ToLower(raw)), "-", "_") {
	case "allow_once":
		return "allow_once", nil
	case "reject_once":
		return "reject_once", nil
	default:
		return "", fmt.Errorf("invalid --permission-decision %q (want allow-once or reject-once)", raw)
	}
}

func resolveWorkspace(path string) (string, error) {
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = wd
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", usagef("craze: --workspace %s: %v", path, err)
	}
	if !st.IsDir() {
		return "", usagef("craze: --workspace must be an existing directory")
	}
	return path, nil
}

func stdinIsTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
