package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// tuiFlags is a session's command line: the root command's, and — its
// TUI-only members never registered there and so zero — craze serve's
// (serveFlags). The session flags both commands take are registered once, by
// registerSessionFlags (sessionflags.go, plan 030 §3.3).
type tuiFlags struct {
	theme     string
	workspace string
	model     string
	// effort is --effort, and fast and noFast --fast and --no-fast (plan 032
	// §3.11): the session's start settings, read through fastSetting.
	effort   string
	fast     bool
	noFast   bool
	agentBin string
	provider string
	// pluginDirs are extra plugin roots, cursor-agent's --plugin-dir spelled
	// the same way. They are craze's own: the child agent is never told.
	pluginDirs []string
	force      bool
	noForce    bool
	noMouse    bool
	// noBackground keeps the terminal's own background and text colours: the
	// only override, since there is no --background to force it back on.
	noBackground bool
	// noHostStatus reports nothing to the terminal multiplexer craze runs in
	// and leaves the agent child's environment whole. Like noBackground it
	// only turns something off: the host's own environment turns it on.
	noHostStatus bool
	ask          bool
	plan         bool
	// cont and resume are --continue/-c and --resume/-r: load the newest
	// session in this workspace, or pick one of the last ten (§3.1). They
	// are mutually exclusive; neither ever falls back to session/new.
	cont   bool
	resume bool
}

// mode is the session mode --ask or --plan names, "" for neither.
func (f *tuiFlags) mode() string {
	switch {
	case f.ask:
		return "ask"
	case f.plan:
		return "plan"
	}
	return ""
}

// refuse is refuseInProcess over this command line's spawn flags. Every
// provider the TUI may start is asked it: the resolved default before anything,
// a --continue row before it is claimed, and whatever either picker is about to
// start (Config.RefuseLoad). So no route to a session lets --agent-bin,
// CRAZE_AGENT_BIN or --ask/--plan through that `--provider` would have refused
// (plan 028 §3.5 and §3.16, C19a).
func (f *tuiFlags) refuse(p agent.Provider) error {
	return refuseInProcess("craze", p, f.agentBin, f.mode())
}

// resumeRowLimit is how many rows --resume offers. The dialog caps at the
// same number, so the picker is the last ten sessions however many the index
// holds.
const resumeRowLimit = 10

// registerTUIFlags declares the root command's flags: the session flags craze
// serve takes too (registerSessionFlags) and the TUI's own. cobra sorts them
// for --help, so the order here is not what a user reads.
func registerTUIFlags(cmd *cobra.Command, f *tuiFlags) {
	// The default is empty so Changed("theme") can tell an explicit --theme
	// from an unset one, which is what the config file loses to.
	cmd.Flags().StringVar(&f.theme, "theme", "", themeFlagUsage)
	registerSessionFlags(cmd, f)
	cmd.Flags().BoolVar(&f.noMouse, "no-mouse", false, "disable mouse reporting (wheel scroll and clicks)")
	cmd.Flags().BoolVar(&f.noBackground, "no-background", false, "keep the terminal's own background and text colours")
	cmd.Flags().BoolVar(&f.noHostStatus, "no-host-status", false, "do not report session status to the terminal multiplexer (herdr, roost)")
	cmd.Flags().BoolVarP(&f.resume, "resume", "r", false, "pick one of the last 10 sessions in this workspace to load")
}

// runTUI runs the TUI. env is the environment host status is read from:
// processHostEnv() for the real command, and an empty hostEnv for a test, which
// must never report into the herdr pane or roost tab the test suite itself may
// be running in.
//
// Its session runs in a detached host the TUI is a client of (runLaunch,
// plan 030 §3.5), or — under the opt-out (detachOn) — in this process, the
// path below, which is the whole of runTUI as it was before detached hosts.
func runTUI(cmd *cobra.Command, f *tuiFlags, env hostEnv) error {
	if err := f.settle(); err != nil {
		return err
	}
	if f.cont && f.resume {
		return usagef("craze: --continue and --resume are mutually exclusive")
	}
	// The TUI owns the alt screen for the whole run, so nothing else may write
	// to the terminal: a diagnostic from cursor-agent lands on top of a frame,
	// takes none of the renderer's locks, and would garble it. The agent's
	// stderr and craze's own warnings are held here and printed once the screen
	// is back.
	diag := &deferredStderr{}
	// The session runs in a detached host, which this TUI is the client of
	// (plan 030 §3.5, launch.go) — unless the opt-out says not to: then it
	// runs in this process, exactly as it always has, below.
	if detachOn(diag.craze()) {
		return runLaunch(cmd, f, env, diag)
	}
	// The host id first: the session claims write it into their lock files,
	// with a control socket or without one (plan 027 §3.9). Its teardown is
	// deferred from here, so every return after it releases what was claimed
	// — a --continue refused below included — and, once a socket is bound,
	// closes and unlinks it after tui.Run has closed the engine (serve.go); a
	// run that reaches tui.Run tears down explicitly, before the flush.
	hostID := rundir.NewHostID()
	runEnv := rundir.ProcessEnv()
	ws, err := resolveWorkspace(f.workspace)
	if err != nil {
		return err
	}
	// The index is keyed by the absolute workspace, which is what the model
	// writes (tui.New absolutises it) and so what a read has to ask for. The
	// session itself is still given the path as it was passed.
	indexCWD := ws
	if abs, err := filepath.Abs(ws); err == nil {
		indexCWD = abs
	}
	mode := f.mode()
	rh := &runHost{claims: newSessionClaims(runEnv, hostID, diag.craze())}
	defer rh.close()
	// A loaded session takes its provider from the row it loads, so whatever
	// $CRAZE_PROVIDER or the config file resolved to is only the picker's
	// preselection and the explicit-flag filter — and an unknown id's
	// fallback diagnostic would be about a choice the row overrides (§3.1).
	provDiag := diag.craze()
	if f.cont || f.resume {
		provDiag = io.Discard
	}
	resolved, err := resolveProvider(cmd, f.provider, provDiag, false)
	if err != nil {
		return err
	}
	if f.cont || f.resume {
		// A load starts the row's own provider, not the resolved one, so it
		// is the row's provider the spawn flags are checked against, once the
		// row is known (resolveLoad's refuseLoad, plan 028 §3.5): whatever the
		// environment or the config resolved has no say in it.
		resolved.Fallback = false
	} else if err := f.refuse(resolved.Provider); err != nil {
		// Only a new session is started on the resolved provider. The
		// provider picker may start another, and asks it the same question
		// (Config.RefuseLoad, below).
		return err
	}
	if cmd == nil {
		// Direct callers (tests) have no cobra flag set, so lock the resolved
		// id and skip the picker — the same as an explicit --provider.
		resolved.Locked = true
	}
	// The hosts this run reports to are settled once, before the build
	// closure: sessionOptions strips each active host's hook gate from the
	// agent child's environment, and resolveLoad may build a session before
	// tui.Run ever starts. The hub itself is built later (plan 015 §3.5).
	hosts, childEnv := agentEnv(f, env)
	// The journal directory is settled once too, for the same reason: the
	// picker may build several sessions, and a journal that is off because of
	// a mistake says so once, on craze's own lane.
	journal := journalDir(diag.craze())
	// The launch's own provider (agentBinary, plan 032 §3.11), as the
	// launcher keeps it (launchProvider): the resolved one — another the
	// provider picker chose is another provider — until a load names its
	// row's, the session this command line named (--continue's, or the resume
	// picker's choice). Under ownMu: the pickers build from bubbletea's
	// goroutines.
	var ownMu sync.Mutex
	own := resolved.Provider
	build := func(p agent.Provider, row sessions.Row) agent.Session {
		ownMu.Lock()
		if row.SessionID != "" {
			own = p
		}
		launch := own
		ownMu.Unlock()
		opts := sessionOptions(f, ws, mode, diag.agent(), diag.craze(), childEnv, p, launch, row)
		opts.JournalDir = journal
		return agent.New(opts)
	}
	newSession := func(p agent.Provider) agent.Session { return build(p, sessions.Row{}) }
	cfg := tui.Config{
		Theme:           resolveTheme(cmd, f.theme),
		Workspace:       ws,
		Model:           f.model,
		Yolo:            f.force,
		NoMouse:         f.noMouse,
		Provider:        resolved.Provider,
		Providers:       pickerProviders(resolved.Provider, f.agentBin),
		Availability:    pickerAvailabilityFor(resolved.Provider, f.agentBin),
		ProviderLocked:  resolved.Locked,
		PersistProvider: true,
		FallbackDefault: resolved.Fallback,
		NewSession:      newSession,
		LoadSession:     build,
		// The provider picker's choice is held to the spawn flags the
		// resolved default was, before it is built or persisted; --resume
		// sets the same closure again in resolveLoad.
		RefuseLoad:    f.refuse,
		SessionIndex:  &sessions.Store{KnownProvider: knownProvider},
		TerminalTitle: tui.ConfigTerminalTitle(),
		// Resolved once, here: config, then the flag, then the colour
		// profile. NO_COLOR and TERM=dumb land on the Ascii profile, and a
		// craze that paints no SGR colour must not repaint the terminal
		// either.
		Background: tui.ConfigBackground() && !f.noBackground &&
			lipgloss.ColorProfile() != termenv.Ascii,
	}
	if err := resolveLoad(cmd, f, indexCWD, &cfg, build, rh.claims); err != nil {
		var held *rundir.HeldError
		if !f.cont || !errors.As(err, &held) {
			return err
		}
		// SQ16 (plan 027 §3.9, PR 4): the session is already running
		// elsewhere, and this --continue joins it there instead. Nothing was
		// built, bound or claimed for it, so the teardown has nothing of this
		// run's to release, and runs before the attach takes the terminal.
		rh.close()
		err = attachHeld(cmd, f, runEnv, held, err)
		diag.flush(os.Stderr, false)
		return err
	}
	if cfg.Session == nil && cfg.Resume == nil && resolved.Locked {
		cfg.Session = newSession(resolved.Provider)
	}
	// Only a run that reaches tui.Run serves, so a refused --continue has
	// bound nothing. The claims above were taken either way: the session lock
	// does not depend on the opt-out (plan 027 §3.8).
	if controlSocketOn(diag.craze()) {
		rh.ctl = serveControl(runEnv, hostID, indexCWD, f.force, diag.craze())
	}
	cfg.OnEngine = rh.onEngine
	// The count of clients attached to the session it hosts, its own seat
	// included (plan 032 §3.14): nil with no socket.
	cfg.LocalPresence = rh.ctl.localPresence()
	cfg.ClaimSession = rh.claims.pickerClaim
	// Only after resolveLoad: a --continue with no row has returned above, so
	// the hub's goroutines start only for a run that reaches tui.Run, whose
	// exit tail closes the hub before diag is flushed.
	attachHost(&cfg, hosts, diag.craze())
	res, err := tuiRun(cfg)
	// The teardown runs here, before the flush, and not only in the defer
	// (which stays for the returns above, and is a no-op after this): the
	// socket, the registry entry and the claims are released even when stderr
	// is a pipe nobody reads and the flush blocks, and whatever the teardown
	// says reaches the flush.
	rh.close()
	// err != nil is also folded into Run's own bool, but the craze lane
	// prints either way and a p.Run error is what makes runTUI see err at
	// all, so it stays explicit here too (§3.7.3).
	diag.flush(os.Stderr, err != nil || res.AgentDiag)
	return err
}

// sessionOptions is the one description of a session the TUI starts — and
// craze serve, which starts the very same session headless (plan 030 §3.3): a
// new one when row is the zero value, and a load of that row when it is not. The two
// differ in three fields and agree in every other, so they are spelled once —
// --ask/--plan/--model apply to a loaded session exactly as they do to a fresh
// one, because Start orders them after the session is set up either way (§3.1).
//
// env is the agent child's environment, hostSet.childEnv's answer: nil inherits
// craze's own, and anything else replaces it wholesale.
//
// stderr and diag are the two lanes deferredStderr splits (§3.7.1): stderr is
// the agent child's own stderr, diag is where craze's own notes about the
// session — discoverPlugins' warn closure — go instead. craze serve, which has
// no alt screen to defer for, hands both its log.
//
// JournalDir is left to the caller — runTUI's build closure, craze serve —
// which sets the directory it resolved once for the whole run (journalDir);
// so is NoPrimary, which only a headless host sets.
//
// launch is the launch's own provider, which the agent binary is resolved
// against (agentBinary, plan 032 §3.11, P7): --agent-bin and CRAZE_AGENT_BIN
// are p's only when p is it, and [agents].<p> or p's PATH candidates
// otherwise. --effort and --fast/--no-fast go to every session the command
// line starts, as --model does.
func sessionOptions(f *tuiFlags, ws, mode string, stderr, diag io.Writer, env []string, p, launch agent.Provider, row sessions.Row) agent.Options {
	opts := agent.Options{
		Workspace:   ws,
		Force:       f.force,
		Model:       f.model,
		Effort:      strings.TrimSpace(f.effort),
		Fast:        f.fastSetting(),
		Mode:        mode,
		Stderr:      stderr,
		Diag:        diag,
		Env:         env,
		PluginDirs:  f.pluginDirs,
		Interactive: true,
		Provider:    &p,
		Compat:      compatClaude(diag),
		// The index's title and pin are seeded before Start so the composer
		// rule shows the stored title through the replay and a /rename
		// survives any number of --continues (§3.4).
		LoadSessionID: row.SessionID,
		Title:         row.Title,
		TitlePinned:   row.Pinned,
	}
	agentBinary(p, launch, f.agentBin, diag).apply(&opts)
	return opts
}

// resolveLoad answers --continue and --resume out of the index, and is a no-op
// without them. Neither flag ever reaches the TUI empty-handed: no row is exit
// 1 before a frame is drawn, and so is an index craze cannot read (§3.8) —
// which is never rewritten by the attempt, exactly as a malformed config file
// is not.
//
// The row's provider is held to the spawn flags a new session's is
// (refuseInProcess, plan 028 §3.5): --agent-bin or CRAZE_AGENT_BIN with a row
// of an in-process provider — native — is exit 2 with the same usage error,
// before the index is written, a craze id minted or anything claimed; and
// --ask/--plan go through, since native has modes. --continue asks it of its
// row here; --resume hands the same closure to the picker (Config.RefuseLoad),
// which asks it of the row chosen before it claims that row.
//
// --continue claims its row before anything is built (plan 027 §3.9, SQ16):
// the row is given its durable craze id under the index's lock, bounded, and
// that id is claimed (sessionClaims.claimRow) — before the spawn flags are
// asked of a row that has its id already, so a held one is attached to
// whatever they say, and after them for a legacy row, whose claim would mint
// and write its id (plan 028 §3.5). A session another craze holds
// is exit 1, `craze: that session is already running (pid N)`, carrying
// the *rundir.HeldError — which runTUI attaches through instead, when the
// holder serves its session (SQ16, PR 4: attachHeld); an index held busy past
// the bound is exit 1 too, and so is a row with no craze id that left the
// index since it was read (`craze: the session index changed — try again`) —
// in every case build is never called, so no agent is spawned. --resume claims
// nothing here: its picker claims the row it is given, through
// Config.ClaimSession.
func resolveLoad(cmd *cobra.Command, f *tuiFlags, cwd string, cfg *tui.Config, build func(agent.Provider, sessions.Row) agent.Session, claims *sessionClaims) error {
	if !f.cont && !f.resume {
		return nil
	}
	// Only an explicit --provider filters the index. A provider that came
	// from $CRAZE_PROVIDER or config.toml is a default for a *new* session,
	// and filtering yesterday's sessions by it would hide the thread the user
	// asked to continue (§3.1). The same explicitness decides whether this
	// run may still write the persisted default: continuing a grok thread is
	// not a decision about tomorrow's default.
	filter := ""
	if providerFlagExplicit(cmd, f.provider) {
		filter = cfg.Provider.Name()
	}
	cfg.PersistProvider = persistsProvider(cmd, f, true)
	refuseLoad := f.refuse

	index := &sessions.Store{KnownProvider: knownProvider}
	if f.resume {
		rows, err := index.Recent(cwd, filter, resumeRowLimit)
		if err != nil {
			return exitf(1, "%s: %v", noSessionMsg(cwd, filter), err)
		}
		if len(rows) == 0 {
			return exitf(1, "%s", noSessionMsg(cwd, filter))
		}
		cfg.Resume = rows
		cfg.RefuseLoad = refuseLoad
		return nil
	}
	row, err := continueRow(cwd, filter)
	if err != nil {
		return err
	}
	// The row's provider, held to the spawn flags, and the row claimed, in the
	// order claimLoad keeps (plan 028 §3.5, SQ16). Its refusal carries
	// claimRow's error: a session another craze holds is attached to instead
	// (runTUI, SQ16).
	p, row, err := claimLoad(row, refuseLoad, claims)
	if err != nil {
		return err
	}
	cfg.Provider = p
	cfg.ProviderLocked = true
	cfg.FallbackDefault = false
	cfg.Loading = true
	// The row's durable craze id travels with the session built from it: this
	// is the same thread of work, loaded into another agent session (session
	// control SD-22). It is the id just claimed — a row written before crazeId
	// existed was given one first — so the claim and the engine agree. Only
	// when no id could be written is it empty, and then the engine mints one
	// that the row gains on its next write.
	cfg.CrazeSessionID = row.CrazeID
	cfg.Session = build(p, row)
	return nil
}

// noSessionMsg is the refusal both flags share, verbatim: the workspace it
// looked in, and the provider when an explicit --provider narrowed the search.
func noSessionMsg(cwd, provider string) string {
	msg := "craze: no session to continue in " + cwd
	if provider != "" {
		msg += " for provider " + provider
	}
	return msg
}

// pickerProviders is the startup picker's row list: every provider craze knows,
// less the optional ones whose binary does not resolve. gx is a personal fork,
// so a machine without it is not shown a row that could only fail at spawn;
// cursor and grok are never filtered, which is what makes an empty picker
// impossible (§3.3).
//
// Resolution is the same question the session's own lookup asks, by the same
// per-provider rule (agentBinary, plan 032 §3.11, P7): launch is the resolved
// provider — the picker's preselection — and explicitBin --agent-bin, which
// with CRAZE_AGENT_BIN is that provider's alone. So an override makes gx
// resolve only when gx is the resolved provider — a gx session started from
// the picker then genuinely spawns it — and one that resolves to nothing hides
// gx even with gx on PATH, because the override is exclusive; for any other
// launch gx shows when `[agents].gx` or its PATH candidate resolves.
//
// This filters for availability and nothing else. Inserting the resolved
// default belongs to tui.New, which guarantees it for every caller rather than
// for the ones that remember (§3.4).
//
// The rows are the availability check's listed providers (plan 036 X13): the
// check is what leaves a missing gx out, so the picker's rows and its states
// (pickerAvailability) cannot disagree about which providers there are.
func pickerProviders(launch agent.Provider, explicitBin string) []agent.Provider {
	avail := availability(processAvailInputs(launch, explicitBin), agent.Providers())
	out := make([]agent.Provider, 0, len(avail))
	for _, a := range avail {
		out = append(out, a.P)
	}
	return out
}

// pickerAvailabilityFor is the startup picker's tui.Config.Availability
// (plan 036 §3.3): the picker opens only before any session, so the launch's
// own provider is the resolved one, def, which is the picker's default too.
func pickerAvailabilityFor(def agent.Provider, explicitBin string) func() []tui.ProviderAvail {
	return func() []tui.ProviderAvail { return pickerAvailability(def, explicitBin, def) }
}

// pickerAvailability is the TUI's pickers' availability (plan 036 §3.3,
// tui.Config.Availability and the session list's /provider): the check's TUI
// column — binaries resolved as a session of each provider started by this
// process would resolve them, launch being the launch's own provider (whose
// alone --agent-bin, explicitBin, and CRAZE_AGENT_BIN are) — over the
// picker's rows: every listed provider, and def, the picker's configured
// default, even when it is a gx whose binary is missing, which is then
// unavailable rather than left out (§3.1's gx row) — the row tui.New always
// adds for it (pickerRows). Native needing setup has the TUI's fix
// (nativeNoKeyFixTUI, X23): in the TUI picking it is the way to set it up.
// It reads the disk: the TUI calls it off its Update.
func pickerAvailability(launch agent.Provider, explicitBin string, def agent.Provider) []tui.ProviderAvail {
	in := processAvailInputs(launch, explicitBin)
	in.pickerDefault = def.Name()
	rows := agent.Providers()
	if !slices.ContainsFunc(rows, func(p agent.Provider) bool { return p.Name() == def.Name() }) {
		// A hidden default (plan 018 §3.4) is a row of the picker's too.
		rows = append(rows, def)
	}
	avail := availability(in, rows)
	out := make([]tui.ProviderAvail, 0, len(avail))
	for _, a := range avail {
		fix := a.Fix
		if a.P.Name() == agent.NativeProvider().Name() && a.State == availNeedsSetup {
			fix = nativeNoKeyFixTUI
		}
		out = append(out, tui.ProviderAvail{ID: a.P.Name(), State: tui.AvailState(a.State), Reason: a.Reason, Fix: fix})
	}
	return out
}

// deferredStderr holds what was said until the terminal is craze's to write
// on again, in two lanes behind one mutex (§3.7.1, issue #23): crazeBuf is
// craze's own diagnostics — a host-status line, the unknown-provider line,
// a plugin-dir note — and agentBuf is the agent child's stderr, which
// internal/acp copies from a goroutine of its own. craze's own lane is
// UNBOUNDED BY DESIGN: it holds a handful of one-line notes, each written at
// most once or twice per run, and it must never be crowded out by an agent
// that loops on its own stderr for an hour — which is what deferredStderrMax
// exists to cap, and caps on the agent's lane alone.
type deferredStderr struct {
	mu       sync.Mutex
	crazeBuf bytes.Buffer
	agentBuf bytes.Buffer
	dropped  int
}

// deferredStderrMax is how much of the agent's own stderr craze will hold.
// An agent looping on its own stderr for an hour must not grow that lane
// without bound; past the cap the tail is dropped and counted, because the
// first thing it said is the thing worth reading. It does not apply to
// craze's own lane (see deferredStderr's doc comment).
const deferredStderrMax = 256 << 10

// craze is the writer view of craze's own lane: host warnings, the
// unknown-provider line and discoverPlugins' notes go here, never through the
// agent's own Stderr.
func (d *deferredStderr) craze() io.Writer { return crazeLane{d} }

// agent is the writer view of the agent child's own stderr lane, capped at
// deferredStderrMax.
func (d *deferredStderr) agent() io.Writer { return agentLane{d} }

type crazeLane struct{ d *deferredStderr }

func (l crazeLane) Write(p []byte) (int, error) {
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	return l.d.crazeBuf.Write(p)
}

type agentLane struct{ d *deferredStderr }

func (l agentLane) Write(p []byte) (int, error) {
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	d := l.d
	if room := deferredStderrMax - d.agentBuf.Len(); room < len(p) {
		d.dropped += len(p) - max(room, 0)
		if room <= 0 {
			return len(p), nil
		}
		p = p[:room]
	}
	return d.agentBuf.Write(p)
}

// flush prints the diagnostics to the real stderr, once, after Run has
// returned — which is after bubbletea has left the alt screen. craze's own
// lane always prints; the agent's lane and its dropped count print only when
// failed says the run did not end cleanly. Either way both lanes are gone
// afterwards: a clean run's flush(false) DISCARDS the agent lane and its
// count rather than holding them back, so a later flush(true) — there is
// none, flush runs exactly once per process, but nothing here depends on
// that — could never resurrect them.
func (d *deferredStderr) flush(w io.Writer, failed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.crazeBuf.Len() > 0 {
		_, _ = w.Write(d.crazeBuf.Bytes())
		d.crazeBuf.Reset()
	}
	if failed {
		if d.agentBuf.Len() > 0 {
			_, _ = w.Write(d.agentBuf.Bytes())
		}
		if d.dropped > 0 {
			_, _ = fmt.Fprintf(w, "craze: dropped %d further bytes of agent diagnostics\n", d.dropped)
		}
	}
	d.agentBuf.Reset()
	d.dropped = 0
}

// themeFlagUsage names the presets once, for both commands that take --theme.
var themeFlagUsage = "TUI theme: " + strings.Join(tui.ThemeNames(), ", ") +
	" (default: ~/.craze/config.toml, else " + tui.DefaultTheme + ")"

// registerPluginDirFlag declares --plugin-dir once, for all three commands that
// start a session. It is a stringArray, not a stringSlice: a plugin path may
// hold a comma, and repeating the flag is how cursor-agent spells it.
func registerPluginDirFlag(cmd *cobra.Command, dst *[]string) {
	cmd.Flags().StringArrayVar(dst, "plugin-dir", nil,
		"extra plugin directory whose commands and skills craze expands (repeatable)")
}

// resolveTheme is the pinned precedence: an explicitly passed --theme wins,
// then the config file, then the default preset. "Explicitly passed" is
// Changed, not a non-empty value, so --theme "" is still a choice.
func resolveTheme(cmd *cobra.Command, flag string) string {
	if cmd != nil && cmd.Flags().Changed("theme") {
		return flag
	}
	if name := tui.ConfigTheme(); name != "" {
		return name
	}
	return tui.DefaultTheme
}

// stdoutIsTerminal is whether cmd's stdout is a terminal a TUI can take
// (isTerminal): a character device that is not one — /dev/null — is refused
// with every other non-terminal. A stdout that is not a file (a test's buffer)
// is judged by the process's own. The root command and craze attach both ask
// it.
func stdoutIsTerminal(cmd *cobra.Command) bool {
	if f, ok := cmd.OutOrStdout().(*os.File); ok {
		return isTerminal(f)
	}
	return isTerminal(os.Stdout)
}
