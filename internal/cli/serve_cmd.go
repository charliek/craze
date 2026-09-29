package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// craze serve (plan 030 §3.3) is the headless host: one session, built exactly
// as the TUI builds its own, served over the control socket with no terminal
// and no client of its own. From C4 on the ordinary craze spawns one per
// session — setsid, stdio on /dev/null (C3) — and runs the TUI as its client,
// so a session outlives the terminal that started it (SD-33). Run by hand it is
// the same host in the foreground, its log on stderr.
//
// What it does, in order, and why:
//
//  1. The session flags are settled and refused by the functions the root
//     command uses (sessionflags.go), so a refusal reads as the root's does;
//     --load is parsed; a control socket switched off is a usage error, since a
//     headless host is reachable through nothing else.
//  2. The log opens (--log, else stderr: hostlog.go) and the host logs of hosts
//     gone a week are swept.
//  3. The provider resolves as the root's does. A new session's is held to the
//     spawn flags; a load's row is found — --continue's the newest in the
//     workspace, --load's the one its id names — and claimed before anything
//     is built (claimLoad), a legacy row given its craze id under this host's
//     own claim. A session another craze holds is exit 1 naming its holder,
//     carrying the *rundir.HeldError (C3 turns it into the ready line's held
//     answer).
//  4. The control socket binds — fatally: a failure leaves nothing behind —
//     served with the lifecycle coordinator (hostLifecycle) as its stop seam.
//  5. The session is built as the TUI builds one, sessionOptions and
//     engine.HostOptions, with agent.Options.NoPrimary: nothing reads a
//     primary on a host with no client, and with one the 257th unread event
//     would block the agent. The engine goes to the socket, a new session's
//     id is claimed and the registry entry rewritten for it (runHost.onEngine).
//  6. Start runs on a goroutine of its own, so a remote client's Start only
//     observes it. A start that failed leaves the host up and its failure
//     travels the socket as start_failed (C5 decides when such a host exits);
//     one that succeeded writes the provider as the next plain craze's
//     default, as the TUI's does.
//  7. The host waits for its stop: session.stop, SIGINT or SIGTERM. SIGHUP is
//     caught and dropped — signal.Notify, never signal.Ignore, whose SIG_IGN
//     an agent child would inherit across exec. The stop sequence runs once
//     (serveHost.stop) and craze serve returns: exit 0.

// serveFlags is craze serve's command line: the session flags the root takes
// too (tuiFlags, whose TUI-only members serve never registers and leaves
// zero), and serve's own.
type serveFlags struct {
	tuiFlags
	// load is --load: a craze session id, or <provider>:<providerSessionId>
	// for an index row with none (parseLoadID).
	load string
	// log is --log: the file the host's diagnostics and its agent's stderr go
	// to, "" for stderr.
	log string
}

// serveStartJoin bounds how long a stopping host waits for its Start to
// return, once the engine's close has closed the session under it. Past it the
// process exits anyway, which ends the start with it.
const serveStartJoin = 5 * time.Second

// serveBuilt is told the agent.Options each session craze serve builds is
// built with: a seam for the option-parity tests, a no-op in production.
var serveBuilt = func(agent.Options) {}

func newServeCmd() *cobra.Command {
	f := &serveFlags{tuiFlags: tuiFlags{force: true}}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Host a session with no terminal, for craze to attach to",
		Long: "craze serve hosts one session with no terminal of its own (plan 030 §3.3): built as " +
			"the TUI builds it, and served over its control socket until a client ends it or the " +
			"process is sent SIGTERM or SIGINT. craze attach joins it. It takes the session flags " +
			"the TUI takes, and --load to load a session by its id.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sigs, stop := serveSignals()
			defer stop()
			return runServe(cmd, f, processHostEnv(), sigs)
		},
	}
	registerServeFlags(cmd, f)
	return cmd
}

// registerServeFlags declares craze serve's flags: the session flags
// (registerSessionFlags), then serve's own.
func registerServeFlags(cmd *cobra.Command, f *serveFlags) {
	registerSessionFlags(cmd, &f.tuiFlags)
	cmd.Flags().StringVar(&f.load, "load", "",
		"load this session: its craze session id, or <provider>:<session id> for one craze has given none")
	cmd.Flags().StringVar(&f.log, "log", "",
		"write the host's diagnostics and the agent's stderr to this file, rotated at 4 MiB (default: stderr)")
}

// serveSignals is craze serve's signal set, registered for the command's
// life: SIGHUP is caught — and dropped by the host (serveHost.wait) — rather
// than ignored, because an ignored signal stays ignored across exec and every
// agent child would inherit it; SIGINT and SIGTERM stop the host.
func serveSignals() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	return ch, func() { signal.Stop(ch) }
}

// runServe is craze serve (the file's doc comment). env is the environment
// the agent child's is derived from (agentEnv): processHostEnv() for the real
// command, and an injected one for a test, as runTUI's is. sigs is the
// process's signals as serveSignals delivers them, or a test's own channel.
// It returns once the host has stopped, nil for a stop however it was asked
// for; an error is a host that never served.
func runServe(cmd *cobra.Command, f *serveFlags, env hostEnv, sigs <-chan os.Signal) (err error) {
	if err := f.settle(); err != nil {
		return err
	}
	load, err := f.loadID(cmd)
	if err != nil {
		return err
	}
	if f.cont && load != nil {
		return usagef("craze serve: --continue and --load are mutually exclusive")
	}
	var why bytes.Buffer
	if !controlSocketOn(&why) {
		return controlSocketRefusal(why.String())
	}

	runEnv := rundir.ProcessEnv()
	out, closeLog, err := openServeLog(runEnv, f.log, errWriter(cmd))
	if err != nil {
		return err
	}
	defer func() {
		// A host that never served says why in its log too: a spawned host's
		// stderr is /dev/null.
		if err != nil && f.log != "" {
			line, _ := diagnose(cmd, err)
			fmt.Fprintln(out, line)
		}
		closeLog()
	}()
	sweepHostLogs(runEnv, out)

	// The host id first, as runTUI's: the session claims write it into their
	// lock files. The teardown is deferred from here, so every return after it
	// releases what was claimed — and, once the socket is bound, closes and
	// unlinks it; the stop sequence runs it itself, and this is then a no-op.
	hostID := rundir.NewHostID()
	rh := &runHost{claims: newSessionClaims(runEnv, hostID, out)}
	defer rh.close()

	loading := f.cont || load != nil
	// A loaded session takes its provider from its row, so whatever the
	// environment or the config file resolved is only the explicit-flag
	// filter, and an unknown id's fallback line would be about a choice the
	// row overrides (runTUI's rule).
	provDiag := out
	if loading {
		provDiag = io.Discard
	}
	resolved, err := resolveProvider(cmd, f.provider, provDiag, false)
	if err != nil {
		return err
	}
	var (
		ws  string
		row sessions.Row
		p   = resolved.Provider
	)
	if loading {
		resolved.Fallback = false
		filter := ""
		if providerFlagExplicit(cmd, f.provider) {
			filter = resolved.Provider.Name()
		}
		if f.cont {
			if ws, err = resolveWorkspace(f.workspace); err != nil {
				return err
			}
			row, err = continueRow(absDir(ws), filter)
		} else if row, err = loadRow(*load, filter); err == nil {
			ws, err = loadWorkspace(cmd, f.workspace, row)
		}
		if err != nil {
			return err
		}
		p, row, err = claimLoad(row, f.refuse, rh.claims)
		if held := heldBy(err); held != nil {
			return &exitError{code: 1, msg: "craze serve: " + rh.claims.pickerRefusal(held), cause: err}
		}
		if err != nil {
			return err
		}
	} else {
		if err := f.refuse(p); err != nil {
			return err
		}
		if ws, err = resolveWorkspace(f.workspace); err != nil {
			return err
		}
	}
	indexCWD := absDir(ws)
	// Settled before anything is built, as runTUI settles them: the agent's
	// environment less the hook gates of the hosts the launching TUI reports
	// to, and the journal.
	_, childEnv := agentEnv(&f.tuiFlags, env)
	journal := journalDir(out)

	lc := newHostLifecycle()
	ctl, err := bindControl(runEnv, hostID, indexCWD, f.force, lc.stopFunc, out)
	if err != nil {
		return exitf(1, "craze serve: the control socket: %v", err)
	}
	rh.ctl = ctl

	// Built as the TUI builds its sessions (runTUI's build closure), less the
	// primary: nothing reads one on a host with no client of its own, and
	// with one the 257th unread event would block the agent — a turn or a
	// replay run with nobody attached must never wait for a reader (SD-33).
	// Clients read through their budgeted subscriptions.
	opts := sessionOptions(&f.tuiFlags, ws, f.mode(), out, out, childEnv, p, row)
	opts.JournalDir = journal
	opts.NoPrimary = true
	serveBuilt(opts)
	sess := agent.New(opts)
	eng, err := engine.New(sess, engine.HostOptions(row.CrazeID, &sessions.Store{KnownProvider: knownProvider}, indexCWD, p.Name()))
	if err != nil {
		_ = sess.Close()
		return exitf(1, "craze serve: %v", err)
	}
	rh.onEngine(eng)
	fmt.Fprintf(out, "craze serve: host %s serving session %s (%s) in %s\n",
		hostID, eng.State().CrazeSessionID, p.Name(), indexCWD)

	h := &serveHost{rh: rh, eng: eng, lc: lc, log: out}
	started := make(chan struct{})
	go func() {
		defer close(started)
		h.start(persistsProvider(cmd, &f.tuiFlags, loading), resolvedProvider{Provider: p, Fallback: resolved.Fallback})
	}()
	h.wait(sigs)
	h.stop()
	select {
	case <-started:
	case <-time.After(serveStartJoin):
		fmt.Fprintln(out, "craze serve: the session's start had not returned when the host stopped")
	}
	return nil
}

// controlSocketRefusal is craze serve's answer to a control socket switched
// off (plan 030 §3.3's last bullet): a usage error, exit 2, since a headless
// host would be reachable through nothing at all. why is controlSocketOn's
// line for a switch it could not read, "" for an explicit false.
func controlSocketRefusal(why string) error {
	why = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(why), "craze: control socket off: "))
	if why == "" {
		why = "control_socket = false in config.toml, or " + controlSocketEnv + " is false"
	}
	return usagef("craze serve: the control socket is off (%s), and a headless host is reachable only through it", why)
}

// absDir is dir made absolute, or dir as it is when it cannot be: the index
// keys rows by the absolute workspace (runTUI's indexCWD).
func absDir(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// loadID is craze serve's --load, parsed (plan 030 §3.3, §3.12): a craze
// session id, or for an index row that has none — a legacy row — its provider
// and the provider's session id, the index's own key.
type loadID struct {
	crazeID   string
	provider  string
	sessionID string
}

func (id loadID) String() string {
	if id.crazeID != "" {
		return id.crazeID
	}
	return id.provider + ":" + id.sessionID
}

// loadID parses --load: nil when it was not given. A craze id is a token
// (rundir.ValidToken: it names a lock file); <provider>:<sessionId> names a
// provider this build knows and a session id that is not blank. Anything else
// is a usage error, before anything is read.
func (f *serveFlags) loadID(cmd *cobra.Command) (*loadID, error) {
	if f.load == "" && (cmd == nil || !cmd.Flags().Changed("load")) {
		return nil, nil
	}
	if prov, sid, ok := strings.Cut(f.load, ":"); ok {
		if _, err := agent.ProviderByName(prov); err != nil {
			return nil, usagef("craze serve: --load %q: unknown provider %q (want %s)", f.load, prov, providerIDs)
		}
		if strings.TrimSpace(sid) == "" {
			return nil, usagef("craze serve: --load %q names no session id", f.load)
		}
		return &loadID{provider: prov, sessionID: sid}, nil
	}
	if !rundir.ValidToken(f.load) {
		return nil, usagef("craze serve: --load %q is neither a craze session id nor <provider>:<session id>", f.load)
	}
	return &loadID{crazeID: f.load}, nil
}

// loadRow is the index row --load names: for a craze id, the newest of the
// rows that carry it (one thread of work loaded into another agent session
// keeps its id, SD-22); for <provider>:<sessionId>, the row of that key. A row
// of a provider this build cannot load, one another provider's than an explicit
// --provider's (filter, the root's --continue rule), and none at all are exit
// 1; so is an index craze cannot read.
func loadRow(id loadID, filter string) (sessions.Row, error) {
	index := &sessions.Store{KnownProvider: knownProvider}
	var (
		row sessions.Row
		ok  bool
		err error
	)
	if id.crazeID != "" {
		var rows map[string]sessions.Row
		if rows, err = index.ByCrazeID(); err == nil {
			row, ok = rows[id.crazeID]
			ok = ok && knownProvider(row.Provider)
		}
	} else {
		row, ok, err = index.Find(id.provider, id.sessionID)
	}
	msg := "craze serve: no session " + id.String()
	if filter != "" {
		msg += " for provider " + filter
	}
	switch {
	case err != nil:
		return sessions.Row{}, exitf(1, "%s: %v", msg, err)
	case !ok, filter != "" && row.Provider != filter:
		return sessions.Row{}, exitf(1, "%s", msg)
	}
	return row, nil
}

// loadWorkspace is the workspace a --load runs in: its row's own, where the
// session ran and where its agent can load it again (a grok session is bound
// to its workspace), and the directory its index row is keyed by. The spawner
// passes --workspace too; one that names another directory is a usage error
// rather than a session loaded somewhere it never ran. A row whose workspace
// is no longer a directory is exit 1.
func loadWorkspace(cmd *cobra.Command, flag string, row sessions.Row) (string, error) {
	if flag != "" || (cmd != nil && cmd.Flags().Changed("workspace")) {
		ws, err := resolveWorkspace(flag)
		if err != nil {
			return "", err
		}
		if !sameDir(absDir(ws), row.CWD) {
			return "", usagef("craze serve: --load names a session that ran in %s, not in --workspace %s", row.CWD, absDir(ws))
		}
	}
	if st, err := os.Stat(row.CWD); err != nil || !st.IsDir() {
		return "", exitf(1, "craze serve: that session ran in %s, which is no longer a directory", row.CWD)
	}
	return row.CWD, nil
}

// serveHost is a running craze serve: its socket and claims (runHost), its
// engine, its lifecycle coordinator and its log.
type serveHost struct {
	rh  *runHost
	eng *engine.Engine
	lc  *hostLifecycle
	log io.Writer
}

// start starts the session, on a goroutine of its own. A failure is said once
// on the log; the engine keeps it (StartFailed), and every client that
// attaches is told so. A start that succeeded writes the provider as the next
// plain craze's default when persist says the run may (persistsProvider), with
// persistProvider's own rules — a fallback and a hidden provider are never
// written — exactly as the TUI's startedMsg does.
//
// It is the TUI's startCmd: context.Background, since what ends a start in
// flight is the engine's close, which closes the session under it — and a
// start that returns after the close finds a closed engine, whose Started is
// a no-op.
func (h *serveHost) start(persist bool, p resolvedProvider) {
	if err := h.eng.Start(context.Background()); err != nil {
		select {
		case <-h.lc.stopping:
			// The stop closed it; that is not a start that failed.
		default:
			fmt.Fprintf(h.log, "craze serve: the session did not start: %v\n", err)
		}
		return
	}
	if persist {
		if err := persistProvider(p); err != nil {
			fmt.Fprintf(h.log, "craze: not saving the provider: %v\n", err)
		}
	}
}

// wait parks craze serve's goroutine until the host is asked to stop: a
// session.stop the server took (hostLifecycle.stopFunc), or SIGINT or SIGTERM
// on sigs, which it asks for itself. A SIGHUP is a line on the log and nothing
// else: the terminal that started a foreground host has gone, and the session
// runs on.
func (h *serveHost) wait(sigs <-chan os.Signal) {
	for {
		select {
		case <-h.lc.stopping:
			return
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				fmt.Fprintln(h.log, "craze serve: hangup ignored; the session runs on")
				continue
			}
			h.lc.request(signalName(sig))
		}
	}
}

// signalName is how the log names a stop signal.
func signalName(sig os.Signal) string {
	switch sig {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	}
	return sig.String()
}

// stop is the stop sequence (plan 030 §3.6a), run once, on craze serve's own
// goroutine, after the first stop request:
//
//  1. attaches are refused closing from here: the stop's own attach fence,
//     never lowered (a session.stop raised the server's own already; this is
//     one more that is never lowered, and the one a signal's stop has);
//  2. the engine's own close — today's quit: it authors a running turn's
//     ending and every parked ask's, closes the agent, and ends every
//     attachment's stream with reset{session_closed} once what each admitted
//     is written;
//  3. S2's close order (controlHost.close): the flush wait, Server.Close, the
//     registry entry, the socket, the host lock;
//  4. last, the session claims (runHost.close).
//
// Every request after the first has joined this one; runServe returns once it
// is done, and the process exits 0.
func (h *serveHost) stop() {
	fmt.Fprintf(h.log, "craze serve: stopping: %s\n", h.lc.cause())
	_, _ = h.rh.ctl.server.FenceAttaches()
	teardownStep("fenced")
	_ = h.eng.Close()
	teardownStep("engine closed")
	h.rh.close()
}
