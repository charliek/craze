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
//     — with --load. A host that answers ok is dialled, and the TUI adopts
//     it; one refused held is the holder's, found through the rendezvous and
//     attached to (SQ16), whoever holds it.
//  3. The TUI's quit is a view close (the session goes on on its host; C5
//     gives /exit its stop). Then the launcher ends every spawn still in
//     flight, and every host it spawned whose session never came up in the
//     TUI — never taken, quit while starting, or failed to start: a quit
//     before a session is up leaves nobody's session behind.
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
	if row.CrazeID == "" || !servedSession(l.env, row.CrazeID) {
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

// servedSession reports whether a live host in the registry serves the
// session crazeID.
func servedSession(env rundir.Env, crazeID string) bool {
	entries, err := rundir.Hosts(env)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.CrazeSessionID == crazeID {
			return true
		}
	}
	return false
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
	return l.spawn(spawnOptions{env: l.env, flags: f}, false)
}

// loadBackend is tui.Config.LoadBackend: a host spawned to load row (--load,
// loadArg: its craze id, or <provider>:<sessionId> for a legacy row), dialled
// — or, when the row's session is held, the holder's. The row names its
// provider, and runs in its own workspace (X13), so neither --workspace nor
// the provider the picker was for is passed; an explicit --provider is, the
// same filter --continue found the row by.
func (l *launcher) loadBackend(_ agent.Provider, row sessions.Row) (backend.Backend, error) {
	f := l.flags
	f.cont = false
	f.workspace = ""
	return l.spawn(spawnOptions{env: l.env, flags: f, load: loadArg(row)}, true)
}

// spawn spawns a host for opts and dials it (spawnHost, dialHost), and
// answers the backend the TUI adopts — recorded, so a backend the TUI never
// took is ended at finish. A spawn after finish refuses at once. load says
// the spawn is a load, whose session may be another host's (the held note).
func (l *launcher) spawn(opts spawnOptions, load bool) (backend.Backend, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, errLaunchOver
	}
	l.inflight.Add(1)
	l.mu.Unlock()
	defer l.inflight.Done()

	ref, err := spawnHost(l.ctx, opts)
	if err != nil {
		return nil, launchFailure(err)
	}
	ctx, cancel := context.WithTimeout(l.ctx, dialTimeout)
	defer cancel()
	s, err := dialHost(ctx, ref, remote.SessionOptions{
		Client: remote.Options{
			Client:    protocol.ClientInfo{Kind: "tui", Name: "craze", Version: version.Version},
			PeerCheck: rundir.DialCheck(os.Geteuid()),
		},
		SessionID: ref.entry.CrazeSessionID,
		// Attached once the host's start is over, as craze attach attaches:
		// a start that fails can take the session's log down with it (the
		// agent session's own teardown), which would end a stream attached
		// before it — an End ahead of any Ready, which quits the TUI as a
		// session that ended rather than one that failed to start. Attached
		// when ready, a failed start is the attach's refusal, start_failed,
		// carrying the start's own error: Start answers it as the in-process
		// Start does, and the TUI stays up with it. Until then the frame is
		// the starting state; a load's transcript arrives whole.
		When:      protocol.WhenReady,
		Provider:  ref.entry.Provider,
		Workspace: ref.entry.Workspace,
	})
	if err != nil {
		return nil, dialFailure(ref, err)
	}
	b := &launchedBackend{Session: s, ref: ref}
	l.mu.Lock()
	l.launched = append(l.launched, b)
	l.mu.Unlock()
	if ref.held && load {
		l.noteHeld(ref)
	}
	return b, nil
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
// host is its holder's, and is left alone. A session that came up (its Start
// answered nil) was closed by the TUI's own exit — a view close — and its
// host goes on.
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
			if b.up.Load() {
				continue
			}
			_ = b.Close()
			b.ref.abandon()
		}
	})
}

// launchedBackend is a launch's session: the socket to its host, which a
// failed start names the log of.
type launchedBackend struct {
	*remote.Session
	ref hostRef
	// up says the session came up in the TUI: its Start — which the TUI
	// calls on the backend it adopts, and on no other — answered nil.
	up atomic.Bool
}

// Start is the Session's, recording a session that came up, and a failure
// of a host this launch spawned names that host's log, where the agent's own
// stderr is — the in-process run printed it after the screen; a detached one
// cannot.
func (b *launchedBackend) Start(ctx context.Context) error {
	err := b.Session.Start(ctx)
	if err == nil {
		b.up.Store(true)
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
// §3.5). The host's own refusal — not ready: a row it cannot load, a claim it
// cannot take — and a held session whose holder cannot be attached to are
// answers about what was chosen: a *tui.Refusal, which a picker shows as its
// error row, in craze's own words ("craze serve: " is the host's name for
// itself, not the user's). Anything else is the detached host failing to come
// up — it exited, timed out, said nothing readable, could not be started — a
// start failure naming its log and the opt-out, which runs the session inside
// craze as before.
func launchFailure(err error) error {
	var se *spawnError
	if errors.As(err, &se) && (se.kind == spawnNotReady || se.kind == spawnHeld) {
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
