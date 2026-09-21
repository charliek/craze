package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

type tuiFlags struct {
	theme     string
	workspace string
	model     string
	agentBin  string
	provider  string
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

// resumeRowLimit is how many rows --resume offers. The dialog caps at the
// same number, so the picker is the last ten sessions however many the index
// holds.
const resumeRowLimit = 10

func registerTUIFlags(cmd *cobra.Command, f *tuiFlags) {
	// The default is empty so Changed("theme") can tell an explicit --theme
	// from an unset one, which is what the config file loses to.
	cmd.Flags().StringVar(&f.theme, "theme", "", themeFlagUsage)
	cmd.Flags().StringVar(&f.workspace, "workspace", "", "existing workspace directory (default: current directory)")
	cmd.Flags().StringVar(&f.model, "model", "", "ACP model id")
	cmd.Flags().StringVar(&f.agentBin, "agent-bin", "", "path to cursor-agent / fake agent (or CRAZE_AGENT_BIN)")
	registerPluginDirFlag(cmd, &f.pluginDirs)
	cmd.Flags().BoolVar(&f.force, "force", true, "spawn the agent with --force (yolo)")
	cmd.Flags().BoolVar(&f.noForce, "no-force", false, "disable yolo and handle permission requests")
	cmd.Flags().BoolVar(&f.noMouse, "no-mouse", false, "disable mouse reporting (wheel scroll and clicks)")
	cmd.Flags().BoolVar(&f.noBackground, "no-background", false, "keep the terminal's own background and text colours")
	cmd.Flags().BoolVar(&f.noHostStatus, "no-host-status", false, "do not report session status to the terminal multiplexer (herdr, roost)")
	cmd.Flags().BoolVar(&f.ask, "ask", false, "set session mode to ask after session/new")
	cmd.Flags().BoolVar(&f.plan, "plan", false, "set session mode to plan after session/new")
	cmd.Flags().BoolVarP(&f.cont, "continue", "c", false, "load the newest session in this workspace instead of starting a new one")
	cmd.Flags().BoolVarP(&f.resume, "resume", "r", false, "pick one of the last 10 sessions in this workspace to load")
	registerProviderFlag(cmd, &f.provider)
}

// runTUI runs the TUI. env is the environment host status is read from:
// processHostEnv() for the real command, and an empty hostEnv for a test, which
// must never report into the herdr pane or roost tab the test suite itself may
// be running in.
func runTUI(cmd *cobra.Command, f *tuiFlags, env hostEnv) error {
	if f.ask && f.plan {
		return usagef("craze: --ask and --plan are mutually exclusive")
	}
	if f.cont && f.resume {
		return usagef("craze: --continue and --resume are mutually exclusive")
	}
	if f.noForce {
		f.force = false
	}
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
	mode := ""
	switch {
	case f.ask:
		mode = "ask"
	case f.plan:
		mode = "plan"
	}
	// The TUI owns the alt screen for the whole run, so nothing else may write
	// to the terminal: a diagnostic from cursor-agent lands on top of a frame,
	// takes none of the renderer's locks, and would garble it. The agent's
	// stderr and craze's own warnings are held here and printed once the screen
	// is back.
	diag := &deferredStderr{}
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
		resolved.Fallback = false
	} else if err := refuseInProcess("craze", resolved.Provider, f.agentBin, mode); err != nil {
		// Only a new session is started on the resolved provider. A load
		// starts the row's own provider, which is never an in-process one
		// (knownProvider), so --agent-bin and --ask/--plan still mean what
		// they say there, whatever the environment or the config resolved.
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
	hosts := resolveHosts(f, env)
	childEnv := hosts.childEnv(env.list())
	// The journal directory is settled once too, for the same reason: the
	// picker may build several sessions, and a journal that is off because of
	// a mistake says so once, on craze's own lane.
	journal := journalDir(diag.craze())
	build := func(p agent.Provider, row sessions.Row) agent.Session {
		opts := sessionOptions(f, ws, mode, diag.agent(), diag.craze(), childEnv, p, row)
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
		Providers:       pickerProviders(f.agentBin),
		ProviderLocked:  resolved.Locked,
		PersistProvider: true,
		FallbackDefault: resolved.Fallback,
		NewSession:      newSession,
		LoadSession:     build,
		SessionIndex:    &sessions.Store{KnownProvider: knownProvider},
		TerminalTitle:   tui.ConfigTerminalTitle(),
		// Resolved once, here: config, then the flag, then the colour
		// profile. NO_COLOR and TERM=dumb land on the Ascii profile, and a
		// craze that paints no SGR colour must not repaint the terminal
		// either.
		Background: tui.ConfigBackground() && !f.noBackground &&
			lipgloss.ColorProfile() != termenv.Ascii,
	}
	if err := resolveLoad(cmd, f, indexCWD, &cfg, build); err != nil {
		return err
	}
	if cfg.Session == nil && cfg.Resume == nil && resolved.Locked {
		cfg.Session = newSession(resolved.Provider)
	}
	// Only after resolveLoad: a --continue with no row has returned above, so
	// the hub's goroutines start only for a run that reaches tui.Run, whose
	// exit tail closes the hub before diag is flushed.
	attachHost(&cfg, hosts, diag.craze())
	failed, err := tui.Run(cfg)
	// err != nil is also folded into Run's own bool, but the craze lane
	// prints either way and a p.Run error is what makes runTUI see err at
	// all, so it stays explicit here too (§3.7.3).
	diag.flush(os.Stderr, err != nil || failed)
	return err
}

// sessionOptions is the one description of a session the TUI starts: a new one
// when row is the zero value, and a load of that row when it is not. The two
// differ in three fields and agree in every other, so they are spelled once —
// --ask/--plan/--model apply to a loaded session exactly as they do to a fresh
// one, because Start orders them after the session is set up either way (§3.1).
//
// env is the agent child's environment, hostSet.childEnv's answer: nil inherits
// craze's own, and anything else replaces it wholesale.
//
// stderr and diag are the two lanes deferredStderr splits (§3.7.1): stderr is
// the agent child's own stderr, diag is where craze's own notes about the
// session — discoverPlugins' warn closure — go instead.
//
// JournalDir is left to runTUI's build closure, which sets the directory it
// resolved once for the whole run (journalDir).
func sessionOptions(f *tuiFlags, ws, mode string, stderr, diag io.Writer, env []string, p agent.Provider, row sessions.Row) agent.Options {
	return agent.Options{
		Binary:      f.agentBin,
		Workspace:   ws,
		Force:       f.force,
		Model:       f.model,
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
}

// resolveLoad answers --continue and --resume out of the index, and is a no-op
// without them. Neither flag ever reaches the TUI empty-handed: no row is exit
// 1 before a frame is drawn, and so is an index craze cannot read (§3.8) —
// which is never rewritten by the attempt, exactly as a malformed config file
// is not.
func resolveLoad(cmd *cobra.Command, f *tuiFlags, cwd string, cfg *tui.Config, build func(agent.Provider, sessions.Row) agent.Session) error {
	if !f.cont && !f.resume {
		return nil
	}
	// Only an explicit --provider filters the index. A provider that came
	// from $CRAZE_PROVIDER or config.toml is a default for a *new* session,
	// and filtering yesterday's sessions by it would hide the thread the user
	// asked to continue (§3.1). The same explicitness decides whether this
	// run may still write the persisted default: continuing a grok thread is
	// not a decision about tomorrow's default.
	explicit := providerFlagExplicit(cmd, f.provider)
	filter := ""
	if explicit {
		filter = cfg.Provider.Name()
	}
	cfg.PersistProvider = explicit

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
		return nil
	}
	row, ok, err := index.Latest(cwd, filter)
	if err != nil {
		return exitf(1, "%s: %v", noSessionMsg(cwd, filter), err)
	}
	if !ok {
		return exitf(1, "%s", noSessionMsg(cwd, filter))
	}
	// The row's provider wins: the index says which agent wrote this session
	// and only that one is trusted to load it, whatever resolveProvider
	// resolved. Reading the registry cannot fail here — the store filters out
	// a provider this build does not know — but a row is user-editable JSON,
	// so the refusal is spelled rather than assumed.
	p, err := agent.ProviderByName(row.Provider)
	if err != nil {
		return exitf(1, "craze: %v", err)
	}
	cfg.Provider = p
	cfg.ProviderLocked = true
	cfg.FallbackDefault = false
	cfg.Loading = true
	// The row's durable craze id travels with the session built from it: this
	// is the same thread of work, loaded into another agent session (session
	// control SD-22). A row written before crazeId existed has none, and the
	// engine mints one that the row gains on its next write.
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
// explicitBin is --agent-bin. Resolution is the same question spawn asks, so an
// override makes every optional provider resolve — with one set, a gx session
// genuinely would spawn that binary — and an override that resolves to nothing
// hides gx even with gx on PATH, because the override is exclusive.
//
// This filters for availability and nothing else. Inserting the resolved
// default belongs to tui.New, which guarantees it for every caller rather than
// for the ones that remember (§3.4).
func pickerProviders(explicitBin string) []agent.Provider {
	all := agent.Providers()
	out := make([]agent.Provider, 0, len(all))
	for _, p := range all {
		if p.Optional() && !p.BinaryResolves(explicitBin) {
			continue
		}
		out = append(out, p)
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

func stdoutIsTTY() bool {
	st, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
