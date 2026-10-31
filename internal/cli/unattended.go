package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// Unattended sessions (plan 041 §3.2, W2): a session craze new --wait starts,
// or craze send reloads, for no one to attend. Its host is spawned directly —
// spawnHost and serveSpec, the TUI launcher's path, never the hub's
// session.create (P1) — with the environment the hub hands its hosts
// (spawnOptions.hubEnv), out of the command's process tree
// (spawnOptions.reparent, X32), and is told it is unattended (craze serve's
// hidden --unattended):
//
//   - unattendedPolicy: Interactive false and craze prompt's own approval
//     policy (headlessApproval, P2) — questions answered with their first
//     option, permissions allowed under force (the default) and parked
//     otherwise — except that an ACP provider's plan is returned and its turn
//     ended, never accepted (ApprovalReturn, X28; native still accepts, which
//     ends its turn in plan mode) — so no background child or bash job
//     outlives a turn, and only a permission under --no-force can leave an
//     ask open.
//   - unattendedRecord: its start settings on the session's index row
//     (sessions.Row.Unattended, P10–P11), which the index keeps set-only and
//     hands on by craze id, and which craze send passes again when it reloads
//     the session (reloadFlags). It is provenance, not policy: a person's
//     resume of a marked session is an ordinary interactive host.
//
// The commands' own flow — the prompt sent through a gate that knows whether
// it ever was (submitGate), the wait, and the undo of a spawn whose prompt
// never reached its session — is promptJob's.

// unattendedPolicy makes opts an unattended host's session (the file's
// comment): not interactive, which turns native background work off
// (agent.Options.Interactive), and craze prompt's approval policy for its
// asks, which an unset Approval would have derived otherwise.
func unattendedPolicy(opts *agent.Options) {
	opts.Interactive = false
	opts.Approval = headlessApproval(opts.Force)
	if p := opts.Provider; p == nil || !p.InProcess() {
		// An ACP agent told its plan is accepted goes on to carry it out in
		// the same turn (grok, live L7): an unattended host returns the plan
		// and ends the turn there instead (X28, agent.ApprovalReturn). A
		// native session's accepted plan already ends its turn in plan mode,
		// and its harness parks any plan answer but accept.
		opts.Approval.Plan = agent.ApprovalReturn
	}
}

// unattendedRecord is the provenance record an unattended host writes on its
// session's rows (P11): the start's mode, permission mode, model and effort,
// as its command line gave them.
func (f *serveFlags) unattendedRecord() *sessions.Unattended {
	perm := sessions.PermissionBypass
	if !f.force {
		perm = sessions.PermissionPrompt
	}
	return &sessions.Unattended{Mode: f.mode(), PermissionMode: perm, Model: f.model, Effort: strings.TrimSpace(f.effort)}
}

// reloadFlags is the command line craze send reloads row's session with (plan
// 041 §3.4): an unattended session's recorded start settings again — mode,
// --no-force for a permission mode of prompt, model and effort, since an ACP
// agent applies a mode and a model only from the options after a load — and,
// for a row with no record (a person's session), none of them: the ordinary
// interactive host the session list's resume would spawn. Either way no
// --provider (a filter on a load) or --workspace (the row's own), and
// --no-host-status, as every direct spawn passes.
func reloadFlags(row sessions.Row) tuiFlags {
	f := tuiFlags{force: true, noHostStatus: true}
	if u := row.Unattended; u != nil {
		f.model, f.effort = u.Model, u.Effort
		f.ask, f.plan = u.Mode == "ask", u.Mode == "plan"
		if u.PermissionMode == sessions.PermissionPrompt {
			f.force, f.noForce = false, true
		}
	}
	return f
}

// ------------------------------------------------------------ the gate

// promptFate is what became of a command's prompt, as its gate saw it.
type promptFate int

const (
	// promptUnsent: the prompt was never handed to the client, and — the
	// gate closed — never will be.
	promptUnsent promptFate = iota
	// promptRefused: the prompt never ran — the host's own error reply to
	// it, or a command the client knows it never wrote (remote.ErrNotRun).
	promptRefused
	// promptPending: the prompt was sent, and what became of it is unknown —
	// its answer not yet in, lost with its connection (resume_lost), or a
	// success reply that cannot be read: it may have run.
	promptPending
	// promptTaken: the prompt started a turn or became a queued row.
	promptTaken
)

// errPromptWithheld is a submit the gate refused: it closed first.
var errPromptWithheld = &submitError{msg: "the command ended before the prompt was sent; it was not sent", kind: errkind.Interrupted}

// submitGate stands between the waiter and the prompt (waitStart.submit): it
// records whether the prompt was ever sent and what its answer was, and once
// closed it sends nothing at all. So a command that gave up before its
// prompt's answer — a signal, --timeout, a failure — knows, as it decides
// whether to undo what it did, whether anything can have reached the
// session, and that nothing more ever will (the waiter's submit runs on a
// goroutine of its own, which a closed gate stops at its door).
type submitGate struct {
	submit func(context.Context, uint64) (waitTarget, error)

	mu       sync.Mutex
	closed   bool
	sent     bool
	answered bool
	target   waitTarget
	err      error
}

// run is the waiter's submit (waitStart.submit), through the gate.
func (g *submitGate) run(ctx context.Context, ident uint64) (waitTarget, error) {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return waitTarget{}, errPromptWithheld
	}
	g.sent = true
	g.mu.Unlock()
	t, err := g.submit(ctx, ident)
	g.mu.Lock()
	g.answered, g.target, g.err = true, t, err
	g.mu.Unlock()
	return t, err
}

// close shuts the gate and answers the prompt's fate, and — taken — its
// target, as the answer the gate recorded named it: read in the one section
// with the fate, so a caller never pairs a taken prompt with an answer it
// gave up on. Only a refusal the host itself replied (a *remote.Error), or a
// command the client knows it never wrote (remote.ErrNotRun), is refused:
// anything else — the answer lost (remote.ErrOutcomeUnknown), a success
// reply that cannot be read, a transport's failure after the request may
// have gone — leaves the prompt's fate unknown (pending).
func (g *submitGate) close() (promptFate, waitTarget) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	switch {
	case !g.sent:
		return promptUnsent, waitTarget{}
	case !g.answered:
		return promptPending, waitTarget{}
	case g.err == nil:
		return promptTaken, g.target
	case refusedByHost(g.err):
		return promptRefused, waitTarget{}
	}
	return promptPending, waitTarget{}
}

// refusedByHost says a submit's error is proof the prompt never ran: the
// host's own error reply to it, or a command the client never wrote — and
// never an outcome the client could not learn.
func refusedByHost(err error) bool {
	if errors.Is(err, remote.ErrOutcomeUnknown) {
		return false
	}
	var re *remote.Error
	return errors.As(err, &re) || errors.Is(err, remote.ErrNotRun)
}

// ------------------------------------------------------------ the run

// promptJob is one command that sends a prompt of its own — craze new
// --wait, craze send [--wait] — from its start: how it prints, its deadline,
// its signals, and the host it spawned.
type promptJob struct {
	what string
	cmd  *cobra.Command
	out  resultOutput
	wait bool
	env  rundir.Env
	text string
	// deadline is --timeout's end, counted from the command's start; zero
	// for none.
	deadline time.Time
	// ctx is the command's work before its prompt is sent — the spawn, the
	// reload, the attach — ended by --timeout's end and by the first signal
	// (relaySignals), whose every signal goes on to relayed.
	ctx       context.Context
	cancel    context.CancelFunc
	relayed   <-chan os.Signal
	signalled func() bool
	stopRelay func()
	// hurry is relaySignals': a reparented spawn's abandon arms it, so a
	// further signal cuts that clean-up short (spawnOptions.hurry, C8, 3).
	hurry func() <-chan struct{}
	// spawned is the host of the session this command started — craze new
	// --wait's, brand new, its id told to no one else — which it stops when
	// its prompt never reached it (undo). nil for every other: a reload's host
	// (craze send) serves a session that existed before the command and that
	// another send may have found and prompted meanwhile (C3 review r1, 1), so
	// it stays up like any host — an interactive one idles out, an unattended
	// one stays until craze stop (TF-9); and a held session's host is its
	// holder's.
	spawned *hostRef
	// session is who a result is about, once the command knows.
	session resultSession
}

// newPromptJob is command what's run (cmd): its prompt text, --wait, its
// start and --timeout (0: none), --json and --max-message, and its signals —
// relayed from here on (relaySignals), the first ending the work before the
// prompt is sent. close ends it.
func newPromptJob(cmd *cobra.Command, what, text string, wait bool, started time.Time, timeout time.Duration,
	json bool, maxMessage int, sigs <-chan os.Signal) *promptJob {
	env := rundir.ProcessEnv()
	r := &promptJob{what: what, cmd: cmd, wait: wait, env: env, text: text,
		out: resultOutput{what: what, json: json, maxMessage: maxMessage, env: env}}
	ctx, cancel := context.WithCancel(context.Background())
	if timeout > 0 {
		r.deadline = started.Add(timeout)
		var cancelDeadline context.CancelFunc
		ctx, cancelDeadline = context.WithDeadline(ctx, r.deadline)
		inner := cancel
		cancel = func() { cancelDeadline(); inner() }
	}
	r.ctx, r.cancel = ctx, cancel
	r.relayed, r.signalled, r.stopRelay, r.hurry = relaySignals(sigs, cancel)
	return r
}

// close ends the run's own goroutines and context.
func (r *promptJob) close() {
	r.stopRelay()
	r.cancel()
}

// timedOut says --timeout's end has come.
func (r *promptJob) timedOut() bool {
	return !r.deadline.IsZero() && !time.Now().Before(r.deadline)
}

// spawn starts a host for opts on the run's work context, a further signal
// hurrying a reparented start's clean-up (r.hurry, C8, 3). own says the
// session is the command's own — new, its id known to no one else (craze new
// --wait) — and so the run's to undo (spawned); a held answer is the
// holder's host, never the run's.
func (r *promptJob) spawn(opts spawnOptions, own bool) (hostRef, error) {
	opts.hurry = r.hurry
	ref, err := spawnHost(r.ctx, opts)
	if err == nil && !ref.held {
		if own {
			r.spawned = &ref
		}
		promptSpawned(ref)
	}
	return ref, err
}

// promptSpawned sees each host a prompt's command spawned, once it answered
// ok, before the command attaches to it: a no-op, and the barrier a test
// sends a signal after when it means one before the prompt was sent.
var promptSpawned = func(hostRef) {}

// failBefore is a failure before the prompt was sent (err: the spawn's, the
// reload's, the attach's): nothing of the command's reached any session, so
// the session it started is stopped (undo: craze new --wait's alone), and —
// interrupted by a signal — an owned form (--wait) reports its prompt
// `cancelled`, exit 3 (§3.1 "Signals": new --wait before its prompt was
// submitted); otherwise exit 1, `timed out before the prompt was sent` at
// --timeout's end, or err's own words.
func (r *promptJob) failBefore(err error) error {
	// The first signal is the one that ended the work (relaySignals): it is
	// taken here, so the undo meets only a second one.
	if r.signalled() {
		select {
		case <-r.relayed:
		default:
		}
	}
	undone := r.undo(false)
	switch {
	case r.signalled():
		return r.notSent("interrupted before the prompt was sent", undone)
	case r.timedOut():
		return r.fail(&waitFailure{msg: "timed out before the prompt was sent" + undone.note(), kind: errkind.TimeoutBeforePrompt})
	}
	return failJSON(r.cmd.OutOrStdout(), r.out, withNote(err, undone.note()))
}

// waitAfter is the end of a wait whose prompt went through the gate g (the
// waiter's own result or failure, res or wf, over conn): a failure the gate
// says left the prompt unsent or refused is one before the prompt reached the
// session, and is answered as failBefore answers one — the undo hurried when
// the waiter already took a second signal (conn.abandon: the command exits at
// once, C3 review r1, 4); one whose prompt may have reached it names the
// command that follows the session; and one whose prompt was taken after
// --timeout gave up on its answer is no timeout before it, but a turn whose
// ending was not observed, which the answer's target names.
func (r *promptJob) waitAfter(g *submitGate, conn *waitConn, res *turnResult, wf *waitFailure) error {
	stdout := r.cmd.OutOrStdout()
	if wf == nil {
		return writeResult(stdout, r.cmd.ErrOrStderr(), r.out, res)
	}
	switch fate, target := g.close(); fate {
	case promptUnsent, promptRefused:
		undone := r.undo(conn.abandon)
		switch {
		case r.signalled():
			return r.notSent("interrupted before the prompt was sent", undone)
		case fate == promptUnsent && r.timedOut():
			wf = &waitFailure{msg: "timed out before the prompt was sent", kind: errkind.TimeoutBeforePrompt}
		}
		wf.msg += undone.note()
	case promptPending:
		if wf.handle == "" && !strings.Contains(wf.msg, "craze wait ") {
			wf.msg += "; craze wait " + psShort(r.session.id) + " follows the session"
		}
		if wf.kind == errkind.TimeoutBeforePrompt {
			// The prompt may have gone: it was not before it.
			wf.kind = errkind.Unobserved
		}
	case promptTaken:
		if wf.kind == errkind.TimeoutBeforePrompt {
			wf.kind = errkind.Unobserved
			wf.msg = "timed out before the turn could be followed; the prompt was taken"
			h := wf.awaited
			h.target = target
			if s := h.String(); h.incarnation != "" && s != "" {
				wf.handle, wf.follow = s, true
			} else {
				wf.msg += "; craze wait " + psShort(r.session.id) + " follows the session"
			}
		}
	}
	return r.fail(wf)
}

// waitFate sees an owned wait's end, before it reads its gate: a no-op, and the
// barrier a test lets a prompt's answer in at, once the waiter has given up on it.
var waitFate = func(*submitGate) {}

// fail prints an exit-1 failure (writeFailure) about the run's session.
func (r *promptJob) fail(wf *waitFailure) error {
	return writeFailure(r.cmd.OutOrStdout(), r.out, psShort(r.session.id), wf)
}

// notSent is an owned prompt cancelled before it was sent (§3.1 "Signals"):
// `cancelled`, exit 3, a terminal result with nothing in it, its error saying
// why — and, when it did, what became of the session the command started. A
// result about a session the command never learnt (its spawn cut short) has
// no session.
func (r *promptJob) notSent(why string, undone undoing) error {
	if !r.wait {
		return r.fail(&waitFailure{msg: why + undone.note(), kind: errkind.Interrupted})
	}
	res := newResult(r.session, statusCancelled)
	res.fill(newTurnAccumulator())
	res.Error = kindError(why+undone.note(), errkind.Interrupted)
	return writeResult(r.cmd.OutOrStdout(), r.cmd.ErrOrStderr(), r.out, res)
}

// undoing is what undo did with the session the command started.
type undoing int

const (
	// undoNone: there was none to stop (spawned is nil).
	undoNone undoing = iota
	// undoStopped: it was stopped and its host waited for
	// (hostRef.abandon), or a signal ended that wait with the stop on its
	// way.
	undoStopped
	// undoStopSent: session.stop was sent, and the host's going not waited
	// for — the command already took a second signal, which exits at once.
	undoStopSent
)

// note is what a failure adds for it.
func (u undoing) note() string {
	switch u {
	case undoStopped:
		return "; the session it started was stopped"
	case undoStopSent:
		return "; the session it started was told to stop"
	}
	return ""
}

// undo stops the session the command started (spawned: craze new --wait's
// alone), its prompt never having reached it — the host's stop and end,
// hostRef.abandon — a further signal ending the wait for it at once, the stop
// on its way. hurried is a command that already took its second signal (the
// waiter's abandon): session.stop alone is sent — bounded by spawnStopWait,
// and by any signal more — and the host's going is not waited for (C3 review
// r1, 4).
func (r *promptJob) undo(hurried bool) undoing {
	ref := r.spawned
	if ref == nil {
		return undoNone
	}
	r.spawned = nil
	done := make(chan struct{})
	go func() {
		defer close(done)
		if hurried {
			_ = stopHost(ref.entry)
			return
		}
		ref.abandon()
	}()
	select {
	case <-done:
	case <-r.relayed:
	}
	if hurried {
		return undoStopSent
	}
	return undoStopped
}

// withNote is err with note after its message: an exit error keeps its code.
func withNote(err error, note string) error {
	if note == "" {
		return err
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return &exitError{code: ee.code, msg: ee.msg + note, cause: ee.cause, kind: ee.kind}
	}
	return errors.New(err.Error() + note)
}

// waitFor is an owned wait over conn (§3.1): the prompt sent through a gate
// (submitGate) once the attachment's incarnation is proven, followed to its
// turn's end, the result or the failure printed (waitAfter).
func (r *promptJob) waitFor(conn *waitConn) error {
	g := &submitGate{submit: submitPrompt(conn, r.text)}
	timeout, stopTimer := timeoutAt(r.deadline)
	defer stopTimer()
	res, wf := waitTurn(context.Background(), conn, waitStart{submit: g.run}, waitOptions{timeout: timeout, sigs: r.relayed})
	waitFate(g)
	return r.waitAfter(g, conn, res, wf)
}
