package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/tui"
)

// craze new --wait (plan 041 §3.2, W2): another path than plain craze new's.
// No hub and no session.create (P1): the session's host is spawned here,
// directly, as the TUI's launcher spawns one (spawnHost, serveSpec) — with
// the session flags given, --no-host-status, the hidden --unattended
// (unattended.go), the environment the hub hands its hosts (hub.ChildEnv: no
// CRAZE_PROVIDER, CRAZE_AGENT_BIN, TMUX, HERDR_* …) and no --agent-bin, so
// the agent binary is `[agents]`'s or PATH's, as a hub-created host's is. It
// is detached in a session of its own (setsid), so it outlives the command
// and the process group that ran it, and started through an intermediate
// that leaves it out of the command's process tree (spawnOptions.reparent,
// X32), so a signal for that whole tree — Claude Code's TaskStop — reaches
// the command alone, which cancels its own turn, and the session goes on;
// -C defaults to the current directory.
//
// Then the command is the waiter's (§3.1): it attaches, sends the prompt
// (PROMPT…, --prompt-file, or stdin: prompt_input.go) once the attachment's
// incarnation is proven, and follows the prompt's turn to its end, printing
// the result as craze wait does — exit 0 completed, 3 failed or cancelled, 4
// needs_input, 5 running at --timeout, 1 craze's own failure. Before its
// prompt was sent, a signal stops the session the command spawned and reports
// it `cancelled` (exit 3), and --timeout's end stops it too (exit 1); a
// provider that cannot start is exit 1 with the host's words. The session
// runs on once its turn ends, for craze send to continue.

// newWhat names craze new in its lines.
const newWhat = "craze new"

// runNewWait is craze new --wait (the file's comment): the flags, the
// provider, the directory and the prompt all checked before anything is
// spawned.
func runNewWait(cmd *cobra.Command, f *newFlags, args []string, sigs <-chan os.Signal) error {
	started := time.Now()
	switch {
	case f.fast && f.noFast:
		return usagef("craze new: --fast and --no-fast are mutually exclusive")
	case f.plan && f.ask:
		return usagef("craze new: --plan and --ask are mutually exclusive")
	case f.timeout < 0:
		return usagef("craze new: --timeout must not be negative")
	case f.maxMessage < 0:
		return usagef("craze new: --max-message must not be negative")
	}
	if err := f.prompt.check(cmd, newWhat, args); err != nil {
		return err
	}
	// --title as plain craze new takes it (newTitle: the hub's own
	// normalization), handed to the host this spawns as its --title, which
	// is what the hub's create hands its own (plan 043 D8).
	title, err := newTitle(cmd, f)
	if err != nil {
		return err
	}
	// An explicit --provider by its id alone, as plain craze new checks it
	// (plan 037 LC-4).
	provider := strings.TrimSpace(f.provider)
	if provider != "" {
		if _, err := agent.ProviderByName(provider); err != nil {
			return unknownProviderError(provider)
		}
	}
	stdout := cmd.OutOrStdout()
	// craze new's rule: a stdout whose reader has gone fails its write rather
	// than kill the command before its stderr line.
	ignoreSIGPIPE()
	r := newPromptJob(cmd, newWhat, "", true, started, f.timeout, f.json, f.maxMessage, sigs)
	defer r.close()
	// Under --timeout and the signals, as the rest of the command (C3 review
	// r1, 6), and before anything is spawned.
	text, err := f.prompt.read(r.ctx, cmd, newWhat, args, r.signalled)
	if err != nil {
		return failJSON(stdout, r.out, err)
	}
	r.text = text
	fail := func(kind errkind.Kind, format string, a ...any) error {
		return failJSON(stdout, r.out, kindf(kind, newWhat+": "+format, a...))
	}
	dir := f.dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fail(errkind.Workspace, "the current directory: %v", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fail(errkind.Workspace, "%s: %v", dir, err)
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fail(errkind.Workspace, "%s is not a directory", abs)
	}
	if provider == "" {
		// The default a hub-created session would have had: config.toml's
		// provider (the hub's own hubCreates rule), never CRAZE_PROVIDER,
		// which the host's environment does not carry.
		provider = tui.ConfigProvider()
		if provider == "" {
			return fail(errkind.Config, "no default provider yet; pass --provider %s", newProviderChoices)
		}
		if _, err := agent.ProviderByName(provider); err != nil {
			return fail(errkind.Config, "config.toml's provider %q is not one this craze knows; pass --provider %s", provider, newProviderChoices)
		}
	}
	flags := tuiFlags{workspace: abs, provider: provider, model: strings.TrimSpace(f.model), effort: strings.TrimSpace(f.effort),
		fast: f.fast, noFast: f.noFast, force: !f.noForce, noForce: f.noForce, ask: f.ask, plan: f.plan, noHostStatus: true}
	// No model: --model is only what was asked for, which the agent may yet
	// refuse, and a result says the model once the session has said it.
	r.session = resultSession{provider: provider}
	ref, err := r.spawn(spawnOptions{env: r.env, flags: flags, unattended: true, hubEnv: true, reparent: true, title: title}, true)
	if err != nil {
		return r.failBefore(spawnExit(newWhat, err))
	}
	r.session.id = ref.entry.CrazeSessionID
	conn, err := dialWait(r.ctx, ref.entry, ref.entry.CrazeSessionID, newWhat, nil)
	if err != nil {
		return r.failBefore(err)
	}
	// The detach as the command exits ends at a signal the waiter did not
	// take (runWait's rule).
	defer conn.close(r.relayed)
	return r.waitFor(conn)
}

// spawnExit is a failed spawn (spawnHost) as command what's exit 1: the
// spawn's own line — the host's words, and where its log is — under the
// command's name rather than craze's.
func spawnExit(what string, err error) error {
	var se *spawnError
	if !errors.As(err, &se) {
		return &exitError{code: 1, msg: what + ": " + sanitizeLine(err.Error()), cause: err, kind: errkind.StartFailed}
	}
	// A refusal is the host's own line, "craze serve: …" (choiceRefusal): of
	// what the host was asked to run, which it will refuse again.
	kind := errkind.StartFailed
	if se.kind == spawnRefused {
		kind = errkind.Config
	}
	msg := strings.TrimPrefix(strings.TrimPrefix(sanitizeLine(se.msg), "craze: "), "craze serve: ")
	return &exitError{code: 1, msg: what + ": " + msg, cause: err, kind: kind}
}
