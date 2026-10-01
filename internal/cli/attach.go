package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
	"github.com/charliek/craze/internal/version"
)

// craze attach (plan 027 §3.15) is the full TUI over a session another craze
// hosts: it resolves one live session in the registry the way craze bridge
// does (§3.10), limited to local sessions, dials its control socket, and runs
// tui.Run with the session's socket as the TUI's backend (tui.Config.Backend)
// and Viewer on — the pickers, provider persistence, host status reporting and
// index writes are the host's alone. Nothing is spawned, nothing binds a
// socket, no session claim is taken and no index row is written.
//
// Its explicit quit — /exit, Ctrl+D, Ctrl+C when idle, the second Ctrl+C —
// ends the session, as it does in every client (plan 030 §3.6, the owner's
// decision 11): the host is asked to stop it (session.stop), and the run exits
// 0 saying nothing more. A host that cannot stop its session — a TUI-hosted
// one, or an older craze — refuses, and the quit detaches instead, exit 0,
// with `craze: that session runs in an older craze; close it there` once the
// screen is restored. SIGTERM and a closed terminal are a view close (§3.9):
// the session goes on on its host. The keys are the host TUI's (owner, §3.19):
// the first Ctrl+C while a turn works acts on the shared session. The
// session's own end — another client's stop, the host's quit — is exit 0 with
// `craze: session ended` on stderr once the screen is restored.
//
// A --continue whose session another craze holds runs the same path
// (attachHeld, SQ16): the second `craze -c` becomes `craze attach --session
// <id>`, resolved through the holder's host id.

// attachFlags is craze attach's command line.
type attachFlags struct {
	session      string
	theme        string
	noMouse      bool
	noBackground bool
	// The flags of a new or a loaded session, which an attach never takes:
	// registered hidden, so each is refused by name (refusedAttachFlags)
	// rather than as a flag cobra has never heard of.
	cont, resume    bool
	provider, model string
}

// refusedAttachFlags are §3.15's refused flags, in the order they are checked.
var refusedAttachFlags = []string{"continue", "resume", "provider", "model"}

func newAttachCmd() *cobra.Command {
	f := &attachFlags{}
	cmd := &cobra.Command{
		Use:   "attach",
		Short: "Join a craze session running in another terminal",
		Long: "craze attach runs the full TUI over a running craze session's control socket " +
			"(plan 027 §3.15): the session in this directory, or --session's. /exit ends the " +
			"session; closing the terminal leaves it running on its host.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAttach(cmd, f)
		},
	}
	cmd.Flags().StringVar(&f.session, "session", "",
		"a craze session id, provider session id, or host id (default: the one session running in this directory)")
	cmd.Flags().StringVar(&f.theme, "theme", "", themeFlagUsage)
	cmd.Flags().BoolVar(&f.noMouse, "no-mouse", false, "disable mouse reporting (wheel scroll and clicks)")
	cmd.Flags().BoolVar(&f.noBackground, "no-background", false, "keep the terminal's own background and text colours")
	cmd.Flags().BoolVarP(&f.cont, "continue", "c", false, "")
	cmd.Flags().BoolVarP(&f.resume, "resume", "r", false, "")
	cmd.Flags().StringVar(&f.provider, "provider", "", "")
	cmd.Flags().StringVar(&f.model, "model", "", "")
	for _, name := range refusedAttachFlags {
		_ = cmd.Flags().MarkHidden(name)
	}
	return cmd
}

// runAttach is craze attach: the refused flags, --session's validity, the
// resolution (resolveAttach) — all before a terminal is needed, so each is
// answered whatever stdout is — then the terminal, then the attach itself.
func runAttach(cmd *cobra.Command, f *attachFlags) error {
	for _, name := range refusedAttachFlags {
		if cmd.Flags().Changed(name) {
			return usagef("craze attach: --%s does not apply: attach joins a running session", name)
		}
	}
	explicit := cmd.Flags().Changed("session")
	if explicit && !rundir.ValidToken(f.session) {
		// Refused whatever its value, "" included, before a registry read
		// builds a path: the bridge's rule (§3.10).
		return usagef("craze attach: --session %q is not a valid session id", f.session)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return exitf(1, "craze attach: %s", sanitizeLine(err.Error()))
	}
	entries, err := rundir.Hosts(rundir.ProcessEnv())
	if err != nil {
		return exitf(1, "craze attach: %s", sanitizeLine(err.Error()))
	}
	entry, err := resolveAttach(entries, f.session, explicit, cwd, indexTitles)
	if err != nil {
		return err
	}
	if !stdoutIsTerminal(cmd) {
		return usagef("craze attach: refusing to start TUI on a non-tty")
	}
	view := attachView{theme: resolveTheme(cmd, f.theme), noMouse: f.noMouse, noBackground: f.noBackground}
	return attachRun(attachTarget{entry: entry, sessionID: entry.CrazeSessionID}, view, cmd.ErrOrStderr())
}

// attachHint is the last line of a listing: how to pick one of the sessions
// it names.
const attachHint = "attach to one with: craze attach --session <id>"

// resolveAttach picks the one live session craze attach joins (§3.15):
//
//   - With --session, the entry it names — by craze session id, provider
//     session id or host id, matched as craze bridge matches (matchSession);
//     none is exit 1, `craze attach: no session <id>`, and several (ids do
//     not collide in practice, but nothing here assumes it) exit 2, listing
//     them.
//   - With none, the live entries whose workspace is the current directory,
//     both sides absolute, cleaned and symlink-resolved (canonicalDir): one is
//     the target; several are exit 2, listing them; none is exit 1, `craze: no
//     running craze session in <cwd>`, then the sessions running elsewhere and
//     the --session hint — or that first line alone, when nothing runs
//     anywhere (owner, §3.19).
//
// A listing is one session per line — its id, its workspace and its title
// when the session index has one (titles, read only for a listing) — so a
// resolver never connects just to describe a session (§3.8).
func resolveAttach(entries []rundir.Entry, session string, explicit bool, cwd string, titles func() map[string]string) (rundir.Entry, error) {
	if explicit {
		matches := matchSession(entries, session)
		switch len(matches) {
		case 0:
			return rundir.Entry{}, exitf(1, "craze attach: no session %s", session)
		case 1:
			return matches[0], nil
		default:
			return rundir.Entry{}, usagef("craze attach: %d sessions match --session %s:\n%s",
				len(matches), session, attachList(matches, titles()))
		}
	}
	here := canonicalDir(cwd)
	var in, elsewhere []rundir.Entry
	for _, e := range entries {
		if here != "" && canonicalDir(e.Workspace) == here {
			in = append(in, e)
		} else {
			elsewhere = append(elsewhere, e)
		}
	}
	where := sanitizeLine(cwd)
	switch len(in) {
	case 1:
		return in[0], nil
	case 0:
		msg := "craze: no running craze session in " + where
		if len(elsewhere) > 0 {
			msg += "\n" + attachList(elsewhere, titles()) + "\n" + attachHint
		}
		return rundir.Entry{}, exitf(1, "%s", msg)
	default:
		return rundir.Entry{}, usagef("craze: %d running craze sessions in %s:\n%s\n%s",
			len(in), where, attachList(in, titles()), attachHint)
	}
}

// canonicalDir is a directory as the resolver compares it: absolute, cleaned,
// and with every symlink resolved, so a workspace the registry recorded as
// /tmp/x and a current directory the kernel reports as /private/tmp/x (macOS)
// are one directory. A path that cannot be resolved is compared cleaned; ""
// is "" (an entry with no workspace is in no directory).
func canonicalDir(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return filepath.Clean(p)
}

// attachList is entries one per line, indented: the id (entryID), the
// workspace and — when the index has one — the title, each made one line
// (sanitizeLine): a workspace or a title is the user's text, and a newline in
// one must not look like another session.
func attachList(entries []rundir.Entry, titles map[string]string) string {
	lines := make([]string, len(entries))
	for i, e := range entries {
		fields := []string{entryID(e)}
		if e.Workspace != "" {
			fields = append(fields, e.Workspace)
		}
		if title := titles[e.CrazeSessionID]; e.CrazeSessionID != "" && title != "" {
			fields = append(fields, title)
		}
		for j, f := range fields {
			fields[j] = sanitizeLine(f)
		}
		lines[i] = "  " + strings.Join(fields, "  ")
	}
	return strings.Join(lines, "\n")
}

// indexTitles is every session index row's title by its craze id (§3.15:
// "from the session index row with that craze id, … else omitted"). An index
// that cannot be read titles nothing: it is a description, never a reason to
// fail.
func indexTitles() map[string]string {
	rows, err := (&sessions.Store{}).ByCrazeID()
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(rows))
	for id, row := range rows {
		if row.Title != "" {
			out[id] = row.Title
		}
	}
	return out
}

// attachTarget is the session an attach joins: the live registry entry that
// serves it, and the craze session id the attach names.
type attachTarget struct {
	entry rundir.Entry
	// sessionID is the entry's own craze session id, or for a --continue of
	// a held session the id its holder's lock was claimed for (attachHeld).
	// "" is the host's one session: an entry not yet rewritten for its engine.
	sessionID string
}

// attachView is what the attach TUI takes from the command line: the theme
// (resolveTheme's precedence), --no-mouse and --no-background.
type attachView struct {
	theme        string
	noMouse      bool
	noBackground bool
}

// attachRun runs an attach and says how it ended (attachTo). A seam: the
// SQ16 tests of runTUI record what a --continue would have attached to, where
// the real one would take the terminal.
var attachRun = attachTo

// attachTo dials the target (attachConfig), runs the TUI over it and answers
// the exit (attachExit), whose line, if any, goes to stderr once the screen is
// restored.
func attachTo(target attachTarget, view attachView, stderr io.Writer) error {
	cfg, err := attachConfig(target, view)
	if err != nil {
		return err
	}
	res, err := tui.Run(cfg)
	return attachExit(res, err, stderr)
}

// attachConfig dials the target's control socket — bounded (dialTimeout, the
// bridge's), the peer's uid checked before a byte is written — and is the TUI
// that runs over it: its backend the session's socket (remote.Session), which
// attaches once the host's start has run (when: ready), its Info before then
// the registry entry's provider and workspace (GLM 11); Viewer on; the entry's
// workspace; the command line's theme, mouse and background.
//
// The permission chip reads the host's own word (the info document's
// permissionMode, plan 030 §3.7, SF-60): what the host's --force or
// --no-force spawned its agent with. Yolo is only what it shows for a host
// that does not say — one from before plan 030 — and is craze's own default
// (--force) there, as it always was: attach has no --force of its own. A
// failed dial is exit 1, one `craze attach: …` line.
func attachConfig(target attachTarget, view attachView) (tui.Config, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	s, err := remote.DialSession(ctx, target.entry.Socket, remote.SessionOptions{
		Client: remote.Options{
			Client:    protocol.ClientInfo{Kind: "tui", Name: "craze attach", Version: version.Version},
			PeerCheck: rundir.DialCheck(os.Geteuid()),
		},
		SessionID: target.sessionID,
		When:      protocol.WhenReady,
		Provider:  target.entry.Provider,
		Workspace: target.entry.Workspace,
		// P27 (plan 033): attach resolves a session in any CRAZE_HOME the
		// registry lists, and only a host of this craze's own namespace
		// reads the attachments directory the TUI stores pasted images in.
		ReadsAttachments: rundir.SocketInNamespace(target.entry.Socket, paths.CrazeDir()),
	})
	if err != nil {
		return tui.Config{}, exitf(1, "craze attach: session %s is unreachable: %s",
			sanitizeLine(entryID(target.entry)), sanitizeLine(err.Error()))
	}
	return tui.Config{
		Backend:   s,
		Viewer:    true,
		Theme:     view.theme,
		Workspace: target.entry.Workspace,
		// The chip's fallback for a host that does not say (above).
		Yolo:          true,
		NoMouse:       view.noMouse,
		TerminalTitle: tui.ConfigTerminalTitle(),
		Background: tui.ConfigBackground() && !view.noBackground &&
			lipgloss.ColorProfile() != termenv.Ascii,
	}, nil
}

// attachExit is how an attach TUI's run ends (§3.9):
//
//   - the program's own failure (Run's error when it is not the start's: a
//     recovered panic, the terminal's input failing) is that error, exit 1 —
//     never `session ended` over it, whatever the stream did meanwhile;
//   - the session's own end (an End with no error: the host quit) is exit 0,
//     `craze: session ended` on stderr;
//   - an End carrying an error — the transport gave up, its redials spent —
//     is exit 1, `craze: lost the session: <err>`;
//   - a start failure is the host TUI's: the error, exit 1. A start the
//     stream's end cut short is that end, not a failure of the host's start
//     (remote.StartError);
//   - the explicit quit (plan 030 §3.6, decision 11) stops the session on its
//     host: taken, it says nothing about the session's end that follows —
//     this client asked for it (Result.Stopped); refused by a host that cannot
//     stop it, the quit detached, and one line says where the session still
//     runs (stopUnsupportedNote); a stop answered neither way is one line too,
//     and exit 0: the quit happened, the session may not have ended. Either
//     note is the whole of the exit (X36; plan 030 C5r): a stream's end that
//     arrived meanwhile — the transport given up, or the session's own end —
//     adds no second line and no exit 1, because the quit is what the user
//     asked for, and its outcome is what they are told;
//   - anything else — a view close: SIGTERM, a closed terminal — is exit 0
//     and says nothing: the session goes on on its host.
//
// The notes are written once the screen is restored: stderr is the caller's
// after tui.Run has returned.
func attachExit(res tui.Result, err error, stderr io.Writer) error {
	var start *remote.StartError
	switch {
	case err != nil && !errors.Is(err, res.StartErr):
		return err
	case res.StopUnsupported:
		fmt.Fprintln(stderr, stopUnsupportedNote)
		return startOrNil(err, res)
	case res.StopErr != nil:
		fmt.Fprintf(stderr, "craze: the session may still be running: %s\n", sanitizeLine(res.StopErr.Error()))
		return startOrNil(err, res)
	}
	switch {
	case res.Ended && res.Stopped && res.EndErr == nil:
		return startOrNil(err, res)
	case res.Ended && !errors.As(err, &start):
		if res.EndErr != nil {
			return exitf(1, "craze: lost the session: %s", sanitizeLine(res.EndErr.Error()))
		}
		fmt.Fprintln(stderr, "craze: session ended")
		return nil
	case err != nil:
		return err
	}
	return nil
}

// stopUnsupportedNote is the explicit quit's one line when the session's host
// cannot stop it (plan 030 §3.6): the TUI detached, and the session runs on in
// the craze that hosts it.
const stopUnsupportedNote = "craze: that session runs in an older craze; close it there"

// startOrNil is a stopped session's exit: its start failure, if it never
// came up, and nothing otherwise.
func startOrNil(err error, res tui.Result) error {
	if err != nil && errors.Is(err, res.StartErr) {
		return err
	}
	return nil
}

// holderSocket is what a held session's holder serves to attach through
// (holderEntry).
type holderSocket int

const (
	// holderUnnamed: the session's lock names no host yet (pid ?) — nothing
	// says where, or whether, its holder serves.
	holderUnnamed holderSocket = iota
	// holderNoSocket: no live registry entry has the holder's host id — its
	// socket opted out, or its entry is not written yet — or its entry names
	// no session yet, or the registry cannot be read.
	holderNoSocket
	// holderElsewhere: the holder's live entry serves another session. A
	// holder keeps every claim for its life, and the engine it serves can be
	// replaced, so the host id alone does not say the socket serves the
	// session that was claimed.
	holderElsewhere
	// holderServes: the holder's live entry serves the claimed session.
	holderServes
)

// refusalSuffix ends a held session's refusal with why it cannot be attached
// to instead (§3.9): nothing for an unnamed holder, whose refusal stays PR
// 2's as it was.
func (h holderSocket) refusalSuffix() string {
	switch h {
	case holderNoSocket:
		return " — it serves no control socket"
	case holderElsewhere:
		return " — it serves another session"
	}
	return ""
}

// holderEntry is the live registry entry that serves a held session: the one
// whose host id the session's lock names (§3.9: "resolved through the holder's
// hostId") AND whose crazeSessionId is the session claimed, and holderServes;
// otherwise what there is instead (holderSocket). The entry is returned only
// with holderServes.
func holderEntry(env rundir.Env, held *rundir.HeldError) (rundir.Entry, holderSocket) {
	if held.Holder.HostID == "" {
		return rundir.Entry{}, holderUnnamed
	}
	entries, err := rundir.Hosts(env)
	if err != nil {
		return rundir.Entry{}, holderNoSocket
	}
	for _, e := range entries {
		switch {
		case e.HostID != held.Holder.HostID:
		case e.CrazeSessionID == held.CrazeID:
			return e, holderServes
		case e.CrazeSessionID == "":
			// Serving, and not yet rewritten for its engine: not yet a
			// socket for this session.
			return rundir.Entry{}, holderNoSocket
		default:
			return rundir.Entry{}, holderElsewhere
		}
	}
	return rundir.Entry{}, holderNoSocket
}

// attachHeld is SQ16 for --continue in PR 4 (§3.9): the session resolveLoad
// found held by another craze is attached to instead of refused — `craze -c`
// becomes `craze attach --session <id>`, resolved through the holder's host
// id — after one stderr line, printed before the TUI takes the screen:
// `craze: that session is already running (pid N); attaching`. The
// flags a new session would take (--model, --ask, --plan, --agent-bin,
// --provider) do not apply to an attach: they are ignored, and the line names
// the ones given. Nothing is built, spawned, bound or claimed for it.
//
// A holder whose live registry entry does not serve that session keeps PR
// 2's refusal, exit 1 naming the pid, plus why (holderSocket.refusalSuffix):
// `— it serves no control socket`, or `— it serves another session`; one
// whose lock names no holder yet (pid ?) keeps PR 2's refusal as it was
// (refused).
func attachHeld(cmd *cobra.Command, f *tuiFlags, env rundir.Env, held *rundir.HeldError, refused error) error {
	entry, serves := holderEntry(env, held)
	switch serves {
	case holderUnnamed:
		return refused
	case holderServes:
	default:
		return exitf(1, "craze: %s%s", refusal(held), serves.refusalSuffix())
	}
	note := "craze: " + refusal(held) + "; attaching"
	if ignored := ignoredForAttach(cmd, f); len(ignored) > 0 {
		note += " (ignored: " + strings.Join(ignored, ", ") + ")"
	}
	stderr := errWriter(cmd)
	fmt.Fprintln(stderr, note)
	view := attachView{theme: resolveTheme(cmd, f.theme), noMouse: f.noMouse, noBackground: f.noBackground}
	return attachRun(attachTarget{entry: entry, sessionID: held.CrazeID}, view, stderr)
}

// ignoredForAttach is the flags of a new session this command line gave,
// which an attach does not take: explicitly passed, when there is a command
// line to ask, else set (a direct caller's).
func ignoredForAttach(cmd *cobra.Command, f *tuiFlags) []string {
	var out []string
	for _, fl := range []struct {
		name string
		set  bool
	}{
		{"model", f.model != ""},
		{"ask", f.ask},
		{"plan", f.plan},
		{"agent-bin", f.agentBin != ""},
		{"provider", f.provider != ""},
	} {
		given := fl.set
		if cmd != nil {
			given = cmd.Flags().Changed(fl.name)
		}
		if given {
			out = append(out, "--"+fl.name)
		}
	}
	return out
}

// errWriter is where a command's own lines go: its stderr, or the process's
// for a direct caller with no command.
func errWriter(cmd *cobra.Command) io.Writer {
	if cmd == nil {
		return os.Stderr
	}
	return cmd.ErrOrStderr()
}
