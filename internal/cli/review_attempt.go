package cli

import (
	"context"
	"errors"

	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/routing"
	"github.com/charliek/craze/internal/rundir"
)

const reviewWhat = "craze review"
const reviewTimedOut = "timed_out"

// The provider-start budget is independent of the command-wide turn budget.
var reviewStartupBudget = 35 * time.Second

type attemptResult struct {
	turn            *turnResult
	startFailure    bool
	cleanupErr      error
	effectiveEffort string
}

func attemptFailure(kind errkind.Kind, msg string, start bool) attemptResult {
	return attemptResult{turn: &turnResult{Status: statusFailed, Error: kindError(msg, kind)}, startFailure: start}
}

func contextResult(ctx context.Context) *turnResult {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &turnResult{Status: reviewTimedOut, Stopping: true}
	}
	return &turnResult{Status: statusCancelled, Stopping: true, Error: kindError("review interrupted", errkind.Interrupted)}
}

// attemptTimer owns one timer and feeds the waiter's existing timeout path.
// All event and submit callbacks update the deadline under its lock. A late
// reset cannot revive an already expired attempt.
type attemptTimer struct {
	mu      sync.Mutex
	cap     time.Time
	stall   time.Duration
	expiry  time.Time
	inc     string
	seq     uint64
	expired bool
	stalled bool
	wake    chan struct{}
	timeout chan time.Time
	stop    chan struct{}
	done    chan struct{}
}

func newAttemptTimer(cap time.Time, stall time.Duration) *attemptTimer {
	t := &attemptTimer{cap: cap, stall: stall, wake: make(chan struct{}, 1), timeout: make(chan time.Time, 1), stop: make(chan struct{}), done: make(chan struct{})}
	go t.run()
	return t
}
func (t *attemptTimer) close() { close(t.stop); <-t.done }
func (t *attemptTimer) reset() {
	now := time.Now()
	if t.expired || (!t.expiry.IsZero() && !now.Before(t.expiry)) {
		return
	}
	t.expiry = now.Add(t.stall)
	select {
	case t.wake <- struct{}{}:
	default:
	}
}
func (t *attemptTimer) arm() {
	if t.stall <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reset()
}
func (t *attemptTimer) progress(inc string, seq uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if inc == "" || t.expiry.IsZero() || (t.inc != "" && inc != t.inc) || seq <= t.seq {
		return
	}
	t.inc, t.seq = inc, seq
	t.reset()
}
func (t *attemptTimer) run() {
	defer close(t.done)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		t.mu.Lock()
		end := t.cap
		stall := !t.expiry.IsZero() && (end.IsZero() || t.expiry.Before(end))
		if stall {
			end = t.expiry
		}
		now := time.Now()
		if !end.IsZero() && !now.Before(end) {
			t.expired, t.stalled = true, stall
			t.mu.Unlock()
			t.timeout <- now
			return
		}
		t.mu.Unlock()
		var tick <-chan time.Time
		if !end.IsZero() {
			timer.Reset(time.Until(end))
			tick = timer.C
		}
		select {
		case <-t.stop:
			return
		case <-t.wake:
		case <-tick:
		}
	}
}
func (t *attemptTimer) expiration() (expired, stalled bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.expired, t.stalled
}

type reviewHost struct {
	child *hostspawn.Child
	id    string
	entry rundir.Entry
}

// runUnattendedAttempt returns after verified cleanup, without writing
// command output. The caller owns the common deadline and first signal.
func runUnattendedAttempt(ctx context.Context, dir, prompt string, route routing.Choice, allowPaid bool, stall time.Duration) (a attemptResult) {
	if ctx.Err() != nil {
		return attemptResult{turn: contextResult(ctx)}
	}
	flags := tuiFlags{workspace: dir, provider: route.Provider, model: route.Model, effort: route.Effort, noHostStatus: true, readOnlyTools: true}
	switch route.Provider {
	case "codex", "native":
		flags.ask, flags.force, flags.subscriptionOnly = true, true, !allowPaid
	case "claude":
		flags.force = true
	case "antigravity":
		flags.noForce = true
	case "cursor", "grok":
		flags.ask, flags.noForce = true, true
	default:
		return attemptFailure(errkind.Config, "unknown review provider", true)
	}
	env := rundir.ProcessEnv()
	var host reviewHost
	defer func() {
		a.cleanupErr = reviewAfter(host, env)
		if a.cleanupErr != nil {
			a.turn.Status = statusFailed
			a.turn.Error = kindError(a.cleanupErr.Error(), errkind.Internal)
		} else if errors.Is(ctx.Err(), context.Canceled) {
			a.turn = contextResult(ctx)
		}
	}()
	start, stop := context.WithTimeout(ctx, reviewStartupBudget)
	defer stop()
	if route.Provider == "native" {
		if route.Provisional {
			// Refresh under the startup budget even if a local alias already
			// exists. Its wire slug must be on the account's current list.
			agent.RefreshPlanModelList(start, paths.NativeDir())
			facts := loadNativeProbe()(route.Model)
			if ctx.Err() != nil {
				return attemptResult{turn: contextResult(ctx)}
			}
			if facts.Provisional || facts.Reason != "" {
				return attemptFailure(errkind.Config, "could not verify the native model against the current account list", true)
			}
		}
		if _, err := nativeStartCheck(start, reviewWhat, route.Model, route.Effort, false); err != nil {
			return attemptFailure(failureKind(err), err.Error(), true)
		}
		if ctx.Err() != nil {
			return attemptResult{turn: contextResult(ctx)}
		}
	}
	ref, held, err := spawnOnce(start, spawnOptions{flags: flags, env: env, hubEnv: true, unattended: true, onChild: func(c *hostspawn.Child, id string) { host.child, host.id = c, id }})
	if err != nil {
		if ctx.Err() != nil {
			return attemptResult{turn: contextResult(ctx)}
		}
		if errors.Is(start.Err(), context.DeadlineExceeded) {
			return attemptFailure(errkind.StartFailed, "review host did not start within its startup budget", true)
		}
		e := spawnExit(reviewWhat, err)
		return attemptFailure(failureKind(e), e.Error(), true)
	}
	if held != nil {
		return attemptFailure(errkind.StartFailed, "review workspace is held by another session", true)
	}
	host.entry = ref.entry
	conn, err := dialWait(start, ref.entry, ref.entry.CrazeSessionID, reviewWhat, nil)
	if err != nil {
		if ctx.Err() != nil {
			return attemptResult{turn: contextResult(ctx)}
		}
		if errors.Is(start.Err(), context.DeadlineExceeded) || failureKind(err) == errkind.TimeoutBeforePrompt {
			return attemptFailure(errkind.StartFailed, "review host did not become ready within its startup budget", true)
		}
		return attemptFailure(failureKind(err), err.Error(), true)
	}
	defer func() { _ = conn.c.Close() }()
	model, effort := reviewSettings(start, conn, route.Provider)
	stop()
	deadline, _ := ctx.Deadline()
	timer := newAttemptTimer(deadline, stall)
	defer timer.close()
	g := submitGate{submit: submitLastPrompt(conn, prompt, true)}
	defer g.close()
	// Let waitTurn settle items already delivered when the cap expires.
	// Its existing signal path handles a signal during submit or mid-turn.
	sigs := make(chan os.Signal, 1)
	stopSignal := context.AfterFunc(ctx, func() {
		if errors.Is(ctx.Err(), context.Canceled) {
			sigs <- os.Interrupt
		}
	})
	defer stopSignal()
	wctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, wf := waitTurn(wctx, conn, waitStart{submitContext: ctx, submit: func(ctx context.Context, id uint64) (waitTarget, error) {
		timer.arm()
		return g.run(ctx, id)
	}}, waitOptions{sigs: sigs, timeout: timer.timeout, progress: timer.progress, progressPrompt: prompt, fullMessage: true})
	if wf != nil {
		fate, _ := g.close()
		a = attemptFailure(wf.kind, wf.msg, fate == promptUnsent || fate == promptRefused)
		a.turn.Model = wf.model
	} else {
		a.turn = res
	}
	if wf != nil && ctx.Err() != nil {
		a.turn = contextResult(ctx)
	}
	expired, stalled := timer.expiration()
	if a.turn.Status == statusRunning || (wf != nil && expired) {
		if stalled {
			a = attemptFailure(errkind.Stalled, "review produced no progress before the stall timeout", false)
		} else {
			a.turn = &turnResult{Status: reviewTimedOut, Stopping: true}
		}
	}
	if a.turn.Model == "" {
		a.turn.Model = model
	}
	a.effectiveEffort = effort
	finishReviewMessage(a.turn)
	return a
}

// Capture settings after readiness. A fenced host can retire as soon as its
// review ends, before a post-turn state call reaches it.
func reviewSettings(ctx context.Context, conn *waitConn, providerName string) (model, effort string) {
	peek, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var state protocol.StateResult
	if err := conn.c.Call(peek, protocol.MethodSessionState, protocol.StateParams{SessionID: conn.sessionID}, &state); err != nil {
		return "", ""
	}
	if config, e := agent.DecodeConfigState(state.Settings.Config); e == nil && config != nil {
		provider, _ := agent.ProviderByName(providerName)
		if opt := agent.EffortOption(agent.Snapshot{Provider: provider.Info(), Config: config.Options}); opt != nil {
			effort = opt.Current
		}
	}
	return state.Settings.Model, effort
}

func finishReviewMessage(turn *turnResult) {
	if turn.Status == statusCompleted && turn.MessageCut {
		turn.Status = statusFailed
		turn.Error = kindError("the host could not recover the complete review after reconnecting", errkind.Unobserved)
	}
	if turn.Status == statusCompleted && strings.TrimSpace(turn.message()) == "" {
		turn.Status = statusFailed
		turn.Error = kindError("review completed without a message", errkind.Empty)
	}
}

// Collect the host's agent and shell groups while it is alive. The host
// can retire its own record during shutdown, so verify these independently.
func reviewAfter(h reviewHost, env rundir.Env) error {
	if h.child == nil {
		return nil
	}
	ids, obsErr := hostspawn.ObservedGroups(h.child.Groups())
	if h.entry.Socket != "" && !h.child.Exited() {
		// session.stop closes admission and cancels the active turn.
		_ = sendStopBounded(h.entry, reviewWhat)
	}
	if !h.child.WaitExit(exitKillWait) && !h.child.End(exitKillWait) {
		return errors.New("could not confirm review host exit")
	}
	grace, cancel := context.WithTimeout(context.Background(), exitAfterTurnWait)
	defer cancel()
	more, err := hostspawn.ObservedGroups(h.child.Groups())
	hostspawn.ReapHost(grace, env, h.id, nil)
	scopeErr := hostspawn.StopReviewProcesses(grace, h.child, h.id, append(ids, more...))
	if scopeErr != nil {
		return scopeErr
	}
	if obsErr != nil || err != nil {
		return errors.New("could not read review process groups")
	}
	if _, err := os.Stat(h.child.Groups()); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("could not confirm all recorded groups were reaped")
	}
	return nil
}
