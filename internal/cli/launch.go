package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
	"github.com/charliek/craze/internal/version"
)

// The launch flow (plan 030 §3.5): the ordinary craze resolves what to run,
// spawns — or finds — the detached host that runs it (spawnHost), and runs the
// TUI as that host's client, so a session outlives the terminal that started
// it (SD-33). In order:
//
//  1. The command line is settled and refused as it always was, and the
//     workspace and the provider are resolved by runTUI's own rules;
//     --continue's row is found in the index and --resume's rows are read.
//     Nothing is claimed: a host claims its own session, and the launcher
//     only resolves which row a spawn is for (§3.4).
//  2. The TUI starts at once, in the session's starting state, and spawns
//     its session from a tea.Cmd (tui.Config.NewBackend, LoadBackend;
//     launcher): a new session of the provider known — or of the provider
//     picker's choice — and --continue's row — or the resume picker's choice
//     — with --load. A row a running host serves already is that host's,
//     attached to without a spawn (hostServing). A host that answers ok is
//     dialled, and the TUI adopts it; one refused held is the holder's,
//     found through the rendezvous and attached to (SQ16), whoever holds it.
//  3. The TUI's explicit quit — /exit, Ctrl+D, the second Ctrl+C — stops the
//     session on its host (plan 030 §3.6: tui's stopQuit); a SIGTERM or a
//     closed terminal is a view close, and the session goes on on its host.
//     Then the launcher ends every spawn still in flight, and every host it
//     spawned whose session never came up in the TUI — never taken, quit
//     while starting, or failed to start: a quit before a session is up
//     leaves nobody's session behind. "Came up in the TUI" is the TUI's own
//     word for it (launchedBackend.AckStarted), never the host's answer
//     alone: a quit that wins against the TUI hearing its session is up
//     leaves no host behind either (astra r7-c4 1).
//
// This process binds no socket and takes no claim: the host owns its claim,
// its journal and its index writes, and persists the provider. craze's own
// notes still go through deferredStderr; the agent's stderr is the host's,
// in its log (host-logs/<hostId>.log), which a failed start names.
//
// The opt-out — `detach = false` in config.toml, CRAZE_DETACH=0, or the
// control socket switched off, without which a detached host is reachable
// through nothing — keeps the in-process path of runTUI, unchanged.

// detachEnv turns detached hosts off for one run (plan 030 §3.5).
const detachEnv = "CRAZE_DETACH"

// tuiRun is tui.Run: a seam for the tests that take the Config a run builds —
// in process or launched — where the real one would take the terminal.
var tuiRun = tui.Run

// detachOn is whether this run's session runs in a detached host (plan 030
// §3.5). It is controlSocketOn's rule, a switch that only ever turns it off:
// CRAZE_DETACH set to a false value, `detach = false` in config.toml, or the
// control socket off — for any reason, controlSocketOn's own — since a
// detached host is reachable only through it. It fails closed onto the
// in-process path, today's: a CRAZE_DETACH strconv.ParseBool cannot read, and
// a `detach` that is not a bool (tui.ConfigDetach), are off, each saying so in
// one line on diag. An explicit false is the user's choice and is silent, and
// so is a control socket that is off: the in-process path says why about that
// itself (runTUI's controlSocketOn), once.
//
// CRAZE_DETACH is read trimmed, and empty counts as unset.
func detachOn(diag io.Writer) bool {
	if raw := strings.TrimSpace(os.Getenv(detachEnv)); raw != "" {
		on, err := strconv.ParseBool(raw)
		if err != nil {
			fmt.Fprintf(diag, "craze: detached sessions off: %s=%q is not a bool\n", detachEnv, raw)
			return false
		}
		if !on {
			return false
		}
	}
	if !controlSocketOn(io.Discard) {
		return false
	}
	if on, why := tui.ConfigDetach(); !on {
		if why != "" {
			fmt.Fprintf(diag, "craze: detached sessions off: %s\n", why)
		}
		return false
	}
	return true
}

// runLaunch is runTUI for a detached session (the file's doc comment): f is
// settled and --continue/--resume checked, and diag is the run's deferred
// stderr.
func runLaunch(cmd *cobra.Command, f *tuiFlags, env hostEnv, diag *deferredStderr) error {
	ws, err := resolveWorkspace(f.workspace)
	if err != nil {
		return err
	}
	// runTUI's provider rules: a load's provider is its row's, so the
	// resolved one is only the filter and the picker's preselection, and a
	// new session's is held to the spawn flags here, before anything starts;
	// the provider picker asks the same of its choice (Config.RefuseLoad).
	provDiag := diag.craze()
	if f.cont || f.resume {
		provDiag = io.Discard
	}
	resolved, err := resolveProvider(cmd, f.provider, provDiag, false)
	if err != nil {
		return err
	}
	if f.cont || f.resume {
		resolved.Fallback = false
	} else if err := f.refuse(resolved.Provider); err != nil {
		return err
	}
	if cmd == nil {
		// Direct callers (tests) have no cobra flag set: the resolved id is
		// locked, as runTUI locks it.
		resolved.Locked = true
	}
	l := newLauncher(cmd, f, resolved, diag.craze())
	defer l.finish()
	cfg := tui.Config{
		Theme:     resolveTheme(cmd, f.theme),
		Workspace: ws,
		Model:     f.model,
		// The permission chip's fallback alone: the host a launch spawns
		// says what it spawned its agent with, and the chip reads that
		// (plan 030 §3.7, SF-60) — a held session's host included, whose
		// --force need not be this command line's.
		Yolo:      f.force,
		NoMouse:   f.noMouse,
		Provider:  resolved.Provider,
		Providers: pickerProviders(f.agentBin),
		// No picker when the provider is known; the picker's choice is
		// spawned otherwise. The host persists the provider it starts
		// (serve's persistProvider), so the TUI does not: PersistProvider
		// stays false, and newBackend passes the provider on exactly when
		// the in-process TUI would have written it.
		ProviderLocked:  resolved.Locked,
		FallbackDefault: resolved.Fallback,
		NewBackend:      l.newBackend,
		LoadBackend:     l.loadBackend,
		RefuseLoad:      f.refuse,
		TerminalTitle:   tui.ConfigTerminalTitle(),
		Background: tui.ConfigBackground() && !f.noBackground &&
			lipgloss.ColorProfile() != termenv.Ascii,
	}
	if err := l.resolveLoad(cmd, f, absDir(ws), &cfg); err != nil {
		return err
	}
	// The session list (plan 030 §3.9): a launch's alone — sessions run
	// detached here, so the TUI can leave one for another without ending it.
	cfg.Sessions = sessionList{l}
	// The launching TUI is its session's client, not a Viewer: it keeps the
	// host-status hub and reports for the session it shows (plan 030 §3.7) —
	// the host reports nothing. Only after resolveLoad, as runTUI's: nothing
	// between here and tuiRun returns.
	attachHost(&cfg, resolveHosts(f, env), diag.craze())
	res, err := tuiRun(cfg)
	// Before the flush, as runTUI's teardown: a spawn still in flight, or a
	// host whose session never came up, is ended even when stderr is a pipe
	// nobody reads.
	l.finish()
	// The session's end on its host, the transport given up, a start that
	// failed, a view close: craze attach's own reading of how the TUI ended.
	err = attachExit(res, err, diag.craze())
	diag.flush(os.Stderr, err != nil || res.AgentDiag)
	return err
}

// launcher is a launch's spawns (plan 030 §3.5): the closures the TUI calls
// for its session (newBackend, loadBackend) and every backend they answered,
// so that a host whose session never came up is ended when it quits
// (finish).
type launcher struct {
	cmd      *cobra.Command
	env      rundir.Env
	flags    tuiFlags
	resolved resolvedProvider
	diag     io.Writer

	// ctx ends every spawn in flight when the TUI has quit (finish).
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	inflight sync.WaitGroup
	launched []*launchedBackend
	done     sync.Once
}

func newLauncher(cmd *cobra.Command, f *tuiFlags, resolved resolvedProvider, diag io.Writer) *launcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &launcher{cmd: cmd, env: rundir.ProcessEnv(), flags: *f, resolved: resolved, diag: diag, ctx: ctx, cancel: cancel}
}

// errLaunchOver is a spawn asked for after the TUI has quit.
var errLaunchOver = errors.New("craze is exiting")

// resolveLoad is resolveLoad (tui.go) for the launch: --continue's row and
// --resume's rows, out of the index, with its refusals word for word — no row
// is exit 1 before a frame is drawn, and so is an index craze cannot read —
// and nothing claimed. --continue's row is Config.Continue, its provider
// locked and the model loading; --resume's rows are the picker's, whose
// choice the TUI spawns.
//
// The spawn flags are held to --continue's row's provider here, before
// anything is spawned (plan 028 §3.5) — exit 2 — except for a session a host
// serves already: that one is attached to whatever they say, since an attach
// takes none of them (SQ16), as the in-process path's claim-first order has
// it. The host holds its own row to them again under its claim.
//
// The resume picker is handed no refusal to ask (Config.RefuseLoad nil,
// astra r7-c4 3): its choice goes to the host's own claim-first order, as
// --continue's row does when a host serves it — a session another host holds
// is attached to whatever the flags say, and one nobody holds is refused by
// the host that claims it, a *tui.Refusal the picker shows as its error row
// (launchFailure). Asked in the picker, before the claim, the flags would
// refuse a held row --continue attaches to (X25's parity). The in-process
// path keeps asking it there (resolveLoad, tui.go): its picker claims.
func (l *launcher) resolveLoad(cmd *cobra.Command, f *tuiFlags, cwd string, cfg *tui.Config) error {
	if !f.cont && !f.resume {
		return nil
	}
	filter := ""
	if providerFlagExplicit(cmd, f.provider) {
		filter = cfg.Provider.Name()
	}
	if f.resume {
		rows, err := (&sessions.Store{KnownProvider: knownProvider}).Recent(cwd, filter, resumeRowLimit)
		if err != nil {
			return exitf(1, "%s: %v", noSessionMsg(cwd, filter), err)
		}
		if len(rows) == 0 {
			return exitf(1, "%s", noSessionMsg(cwd, filter))
		}
		cfg.Resume = rows
		cfg.RefuseLoad = nil
		return nil
	}
	row, err := continueRow(cwd, filter)
	if err != nil {
		return err
	}
	p, err := agent.ProviderByName(row.Provider)
	if err != nil {
		return exitf(1, "craze: %v", err)
	}
	if _, served := hostServing(l.env, row.CrazeID); !served {
		if err := f.refuse(p); err != nil {
			return err
		}
	}
	cfg.Provider = p
	cfg.ProviderLocked = true
	cfg.FallbackDefault = false
	cfg.Loading = true
	cfg.Continue = &row
	return nil
}

// hostServing is the live host in the registry that serves the session
// crazeID — its identity published, which a host writes once it is bound with
// that session's engine, as the held rendezvous finds a holder — and whether
// there is one. A row with no craze id (a legacy row) is served by nobody
// anyone can name, and a registry that cannot be read lists nobody.
func hostServing(env rundir.Env, crazeID string) (rundir.Entry, bool) {
	if crazeID == "" {
		return rundir.Entry{}, false
	}
	entries, err := rundir.Hosts(env)
	if err != nil {
		return rundir.Entry{}, false
	}
	for _, e := range entries {
		if e.CrazeSessionID == crazeID && e.Socket != "" {
			return e, true
		}
	}
	return rundir.Entry{}, false
}

// newBackend is tui.Config.NewBackend: a host spawned for a new session of p,
// dialled. The provider reaches the host as --provider — so the host starts
// it, and persists it after a start that succeeded as the TUI's startedMsg
// would have — except when it is the resolved default that was only a
// fallback (an unknown id in the environment or the config) and was not
// chosen on the picker explicitly: the host then resolves the same fallback
// itself, and knows it for one, and writes nothing, as the in-process TUI
// writes nothing for it.
func (l *launcher) newBackend(p agent.Provider, explicit bool) (backend.Backend, error) {
	f := l.flags
	f.cont = false
	f.provider = p.Name()
	if !explicit && l.resolved.Fallback && p.Name() == l.resolved.Provider.Name() {
		f.provider = ""
	}
	return l.spawn(spawnOptions{env: l.env, flags: f}, "")
}

// loadBackend is tui.Config.LoadBackend: a host spawned to load row (--load,
// loadArg: its craze id, or <provider>:<sessionId> for a legacy row), dialled
// — or, when the row's session is held, the holder's. The row names its
// provider, and runs in its own workspace (X13), so neither --workspace nor
// the provider the picker was for is passed; an explicit --provider is, the
// same filter --continue found the row by.
//
// A row whose session a running host serves already is attached to there
// without a spawn (spawn's crazeID): the reattach after a closed terminal is
// the ordinary way back to a session, and costs no process and no log.
func (l *launcher) loadBackend(_ agent.Provider, row sessions.Row) (backend.Backend, error) {
	f := l.flags
	f.cont = false
	f.workspace = ""
	return l.spawn(spawnOptions{env: l.env, flags: f, load: loadArg(row)}, row.CrazeID)
}

// spawn answers the backend the TUI adopts for opts — recorded, so a backend
// the TUI never took up is ended at finish — and a spawn after finish refuses
// at once.
//
// A load of a session with a craze id first looks for a host that serves it
// already (hostServing) and attaches to it there, spawning nothing: no claim
// is taken — the host holds it — and the flags of a new session are ignored,
// with the note a held answer leaves (noteHeld). The host is taken only once
// its attach has succeeded (reattach): its hello alone says nothing of
// whether it will take a client — a host that is stopping still answers
// hello and refuses every attach closing. Found and not attached to — it is
// stopping, or gone since — the spawn is the fallback, and so is a session
// nobody serves and a legacy row, which has no id to look for. The spawn's
// own held answer covers a host that took the session between the look and
// the spawn: the rendezvous finds it, and it is attached to the same way.
//
// Otherwise a host is spawned for opts and dialled (spawnHost, dialHost).
func (l *launcher) spawn(opts spawnOptions, crazeID string) (backend.Backend, error) {
	done, err := l.begin()
	if err != nil {
		return nil, err
	}
	defer done()

	if e, ok := hostServing(l.env, crazeID); ok {
		b, err := l.reattach(hostRef{entry: e, held: true})
		if err == nil {
			return b, nil
		}
		if l.ctx.Err() != nil {
			// The TUI has quit meanwhile (finish): nothing is spawned for it.
			return nil, err
		}
	}
	ref, err := spawnHost(l.ctx, opts)
	if err != nil {
		return nil, launchFailure(err)
	}
	return l.dial(ref)
}

// dial dials the host ref names as the TUI's client (connect) and takes the
// session (take): a host this launch started is ended when it cannot be
// dialled, and a holder's is left alone.
func (l *launcher) dial(ref hostRef) (backend.Backend, error) {
	ctx, cancel := context.WithTimeout(l.ctx, dialTimeout)
	defer cancel()
	s, err := l.connect(ctx, ref)
	if err != nil {
		return nil, err
	}
	return l.take(s, ref), nil
}

// reattach is spawn's direct reattach to ref, the host a registry entry says
// serves the session already (plan 030 C4r, X43): dialled, and attached —
// the attach the TUI's Start would make, made here, so the host is taken only
// once it has taken this client (C5r2, astra r9-fix45 2). A hello answered is
// not enough: a host that is stopping (its attach fence up) still answers
// hello, refuses the attach closing, and taken on its hello would be the
// TUI's failed start where a spawn would have run the session. So any
// failure — the dial's, or the attach's refusal — closes the candidate,
// unrecorded and with no note, and answers the error: spawn falls back to a
// spawn of its own. The dial and the attach share dialTimeout, and end with
// the launch. An attached candidate is taken as dial takes one: the TUI's
// Start finds it attached and waits for its readiness alone.
func (l *launcher) reattach(ref hostRef) (backend.Backend, error) {
	ctx, cancel := context.WithTimeout(l.ctx, dialTimeout)
	defer cancel()
	s, err := l.connect(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := s.Attach(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return l.take(s, ref), nil
}

// connect dials the host ref names as the TUI's client (dialHost), the
// session not yet attached: a host this launch started is ended when it
// cannot be, and a holder's is left alone.
func (l *launcher) connect(ctx context.Context, ref hostRef) (*remote.Session, error) {
	s, err := dialHost(ctx, ref, l.sessionOptions(ref))
	if err != nil {
		return nil, dialFailure(ref, err)
	}
	return s, nil
}

// sessionOptions is how the TUI's client of the host ref names is dialled:
// by its session, attached now.
func (l *launcher) sessionOptions(ref hostRef) remote.SessionOptions {
	return remote.SessionOptions{
		Client: remote.Options{
			Client:    protocol.ClientInfo{Kind: "tui", Name: "craze", Version: version.Version},
			PeerCheck: rundir.DialCheck(os.Geteuid()),
		},
		SessionID: ref.entry.CrazeSessionID,
		// Attached now, while the host's start runs (plan 030 C5, undoing
		// X23): a load's replay streams in as the agent sends it, as the
		// in-process TUI shows it, instead of arriving whole once the start is
		// over. A start that fails no longer takes the session's log down
		// with it on a host (agent.Options.KeepLogOnFailedStart), so a stream
		// attached before it is sent the failure — the ready notification's
		// failed form — and never an End ahead of it: Start answers it as the
		// in-process Start does, and the TUI stays up with it. An attach made
		// once the start has failed is refused start_failed, the same answer.
		When:      protocol.WhenNow,
		Provider:  ref.entry.Provider,
		Workspace: ref.entry.Workspace,
	}
}

// take is s, dialled to the host ref names, as the backend the TUI adopts:
// recorded, so finish ends it if its session never comes up in the TUI, and
// — a holder's — naming the flags its attach ignored (noteHeld).
func (l *launcher) take(s *remote.Session, ref hostRef) backend.Backend {
	b := &launchedBackend{Session: s, ref: ref}
	l.mu.Lock()
	l.launched = append(l.launched, b)
	l.mu.Unlock()
	if ref.held {
		l.noteHeld(ref)
	}
	return b
}

// noteHeld is the line an attach to a held session leaves for after the
// TUI, as attachHeld's is printed before one (SQ16): only when the command
// line gave flags a new session takes, which the attach did not — naming
// them. A plain reattach says nothing: after a closed terminal it is the
// ordinary way back to a session.
func (l *launcher) noteHeld(ref hostRef) {
	ignored := ignoredForAttach(l.cmd, &l.flags)
	if len(ignored) == 0 {
		return
	}
	held := &rundir.HeldError{CrazeID: ref.entry.CrazeSessionID, Holder: rundir.Holder{PID: ref.entry.PID, HostID: ref.entry.HostID}}
	fmt.Fprintf(l.diag, "craze: %s; attached to it (ignored: %s)\n", refusal(held), strings.Join(ignored, ", "))
}

// finish ends the launch, once: no spawn starts after it, every spawn in
// flight is cancelled and waited for — a host not yet ready is terminated,
// one ready and not yet dialled is stopped (spawnHost, dialHost) — and every
// backend whose session never came up in the TUI is closed and its host, when
// this launch spawned it, stopped (hostRef.abandon): one the TUI never took,
// one it quit while it was starting, and one whose start failed. A session
// nobody was shown, or that never ran, is not left running; a held session's
// host is its holder's, and is left alone. A session that came up — the TUI
// acknowledged its start (AckStarted) — was closed by the TUI's own exit, a
// view close, and its host goes on.
//
// finish runs once tui.Run has returned, and an acknowledgement is made only
// inside the program's Update: nothing can acknowledge after the reading here,
// so a host is kept exactly when the TUI took its session as up, and a quit
// that won against that — a SIGTERM or a hang-up whose quit the program acted
// on first — stops it (astra r7-c4 1).
func (l *launcher) finish() {
	l.done.Do(func() {
		l.mu.Lock()
		l.closed = true
		l.mu.Unlock()
		l.cancel()
		l.inflight.Wait()
		l.mu.Lock()
		launched := l.launched
		l.mu.Unlock()
		for _, b := range launched {
			if b.acked.Load() {
				continue
			}
			_ = b.Close()
			b.ref.abandon()
		}
	})
}

// launchStarted is called when a launched backend's Start has answered nil —
// the host's start is over — before that answer goes back to the TUI, whose
// startedMsg it becomes: a seam for the test that holds the message there
// until the TUI has quit; a no-op in production.
var launchStarted = func() {}

// launchedBackend is a launch's session: the socket to its host, which a
// failed start names the log of.
type launchedBackend struct {
	*remote.Session
	ref hostRef
	// acked says the session came up in the TUI: the TUI took its start's
	// answer while it was not quitting (AckStarted). Start answering nil is
	// not it — the TUI hears that answer later, if it is still there to.
	acked atomic.Bool
}

// AckStarted is the TUI's acknowledgement that the session came up there
// (tui's startAcker): called from the program's Update, when the start's
// answer (startedMsg) is applied and the program is not quitting. Only a
// session acknowledged is kept at finish.
func (b *launchedBackend) AckStarted() { b.acked.Store(true) }

// Start is the Session's, and a failure of a host this launch spawned names
// that host's log, where the agent's own stderr is — the in-process run
// printed it after the screen; a detached one cannot.
func (b *launchedBackend) Start(ctx context.Context) error {
	err := b.Session.Start(ctx)
	if err == nil {
		launchStarted()
		return nil
	}
	if b.ref.log != "" && !errors.Is(err, backend.ErrClosed) {
		return &hostStartError{err: err, log: b.ref.log}
	}
	return err
}

// hostStartError is a detached session's start failure, and where its host's
// log is.
type hostStartError struct {
	err error
	log string
}

func (e *hostStartError) Error() string {
	return e.err.Error() + " (the session host's log: " + e.log + ")"
}

func (e *hostStartError) Unwrap() error { return e.err }

// launchFailure is how the TUI shows a spawn that found no host (plan 030
// §3.5, X24). The host's refusal of what it was asked to run (spawnRefused:
// no such session, a flag its provider cannot take, a claim to try again)
// and a held session whose holder cannot be attached to are answers about
// what was chosen: a *tui.Refusal, which a picker shows as its error row, in
// craze's own words ("craze serve: " is the host's name for itself, not the
// user's). Anything else is the detached host failing to come up — it
// exited, timed out, said nothing readable, could not be started, or stopped
// before it was ready for a reason of its own, its socket not bound among
// them (spawnNotReady) — a start failure naming its log and the opt-out,
// which runs the session inside craze as before, from a picker too: choosing
// again would not change it (astra r7-c4 2).
func launchFailure(err error) error {
	var se *spawnError
	if errors.As(err, &se) && (se.kind == spawnRefused || se.kind == spawnHeld) {
		msg := se.msg
		if rest, ok := strings.CutPrefix(msg, "craze serve: "); ok {
			msg = "craze: " + rest
		}
		return &tui.Refusal{Err: &launchError{msg: msg, err: err}}
	}
	return optOut(err)
}

// dialFailure is a host a spawn found that could not be dialled: one it
// started, which dialHost has ended, is a start failure naming its log and
// the opt-out; a holder's is a refusal of the choice, as a holder serving no
// socket is.
func dialFailure(ref hostRef, err error) error {
	if ref.held {
		held := &rundir.HeldError{CrazeID: ref.entry.CrazeSessionID, Holder: rundir.Holder{PID: ref.entry.PID, HostID: ref.entry.HostID}}
		return &tui.Refusal{Err: &launchError{msg: "craze: " + refusal(held) + " — it cannot be reached: " + sanitizeLine(err.Error()), err: err}}
	}
	return optOut(&launchError{msg: "craze: the session host cannot be reached: " + sanitizeLine(err.Error()) + "; its log: " + ref.log, err: err})
}

// optOut is err, the detached host failing to come up, naming the switch that
// runs the session inside craze instead.
func optOut(err error) error {
	return &launchError{msg: err.Error() + "; " + detachEnv + "=0 runs sessions inside craze instead", err: err}
}

// launchError is a launch failure's line, and what it is about.
type launchError struct {
	msg string
	err error
}

func (e *launchError) Error() string { return e.msg }
func (e *launchError) Unwrap() error { return e.err }
