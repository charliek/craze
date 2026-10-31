package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/tui"
	"github.com/charliek/craze/internal/version"
)

// craze new --load (plan 046 B6) reopens a saved session through the same hub
// instead: reopen.go.
//
// craze new (plan 032 §3.12, P8): a session started in the background, through
// the per-machine hub — hub.Ensure finds it or starts one that can create
// sessions — by its session.create, under a requestId of this run's own. It
// returns once the session has started (the hub answers then), printing
// `started <id> in <dir>` — the id's last eight characters, as craze ps shows
// it — and, for a first prompt whose answer was lost, that the prompt may not
// have reached it. The session runs on in its own host; craze attach, the
// session list and craze ps find it.
//
// A connection to the hub that ends before its answer — the hub restarted
// mid-create — is tried once more, through Ensure again, under the same
// requestId, so the hub that answers it joins the session the first one
// started rather than start a second (protocol.md: a create is retried only
// under its own requestId). Any refusal is the hub's words, exit 1; so is a
// session that started and refused its first prompt (the result says so, and
// the session runs on, idle). A spawn_failed from a hub of another craze
// version — one still running from before an upgrade that removed its binary
// — gains how to stop that hub, by its pid from its hello (plan 046 U2,
// newFailure). With --json every one of those failures also
// prints one JSON object on stdout (newFailureJSON); a usage error prints its
// own, from the root (usageJSON).
//
// --title (plan 043 D8, P11) names the session from its start, pinned as a
// /rename pins one: normalized here as the hub normalizes it
// (engine.NormalizeTitle, so a blank one is a usage error before anything is
// dialled), sent as session.create's title, and needed of the hub — Ensure
// asks for createTitle as well as sessionCreate (hub.CreateNeed), so a
// running hub from before titles is LacksError's "cannot set a title at
// create", exit 1, never the hub's unknown_field and never an untitled
// session. With --wait it is the spawned host's --title, as the hub's create
// passes it.

const (
	// newBudget bounds a craze new: the hub's Ensure, and the create, which
	// waits for the session's start (60 s on the hub) and its first prompt's
	// answer (15 s).
	newBudget = 2 * time.Minute
	// newPromptLost is what craze new adds when the first prompt's answer was
	// lost.
	newPromptLost = "the prompt may not have reached it"
)

// newProviderChoices is the providers craze new's no-default refusal names,
// as a choice: "cursor, grok, gx or native".
var newProviderChoices = joinOrPlain(agent.ProviderNames())

// newClient is who craze new says it is to the hub.
var newClient = protocol.ClientInfo{Kind: "cli", Name: "craze new", Version: version.Version}

type newFlags struct {
	dir                   string
	provider, model       string
	effort                string
	fast, noFast, noForce bool
	json                  bool
	// title is --title (plan 043 D8): the session's name from its start.
	title string
	// load is --load (plan 046 B6): a saved session to reopen through the
	// hub instead of a new one (reopen.go's runNewLoad).
	load string

	// --wait and the flags only it takes (plan 041 §3.2, new_wait.go).
	wait       bool
	plan, ask  bool
	timeout    time.Duration
	maxMessage int
	prompt     promptInput
	// exitAfterTurn and sessionFile are --exit-after-turn and --session-file.
	exitAfterTurn bool
	sessionFile   string
}

// newWaitOnly is every flag only craze new --wait takes: on plain craze new
// each is a usage error.
var newWaitOnly = []string{"timeout", "max-message", "prompt-file", "remove-prompt-file", "plan", "ask", "exit-after-turn", "session-file"}

func newNewCmd() *cobra.Command {
	f := &newFlags{}
	cmd := &cobra.Command{
		Use:   "new [PROMPT...]",
		Short: "Start a session in the background, through the per-machine hub",
		Long: "craze new starts a session in its own host, through the per-machine hub (started on " +
			"demand), in the current directory or -C's, and returns once it has started: its id and " +
			"directory, and — given a PROMPT — whether its first prompt was taken. The session runs on; " +
			"craze attach, the session list (←) and craze ps find it. With --load it brings back a saved " +
			"session instead (craze ps --saved lists them), or answers the one running it already. Without --provider the hub's " +
			"configured default (config.toml's provider) starts it. With --wait it starts an unattended " +
			"session itself, without the hub — questions answered with their first option; a plan ends its " +
			"turn and comes back in the result (cursor, grok and gx are never told it was accepted) — " +
			"sends it the prompt (PROMPT, --prompt-file, or stdin) and waits for its turn's result, as " +
			"craze wait prints one; craze send continues the session.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("load") {
				return runNewLoad(cmd, f, args)
			}
			if f.wait {
				sigs, stop := waitSignals()
				defer stop()
				return runNewWait(cmd, f, args, sigs)
			}
			for _, name := range newWaitOnly {
				if cmd.Flags().Changed(name) {
					return usagef("craze new: --%s needs --wait", name)
				}
			}
			return runNew(cmd, f, args)
		},
	}
	cmd.Flags().StringVarP(&f.dir, "dir", "C", "", "start the session in this directory (default: the current one)")
	registerProviderFlag(cmd, &f.provider)
	cmd.Flags().StringVar(&f.model, "model", "", "model to start on: an ACP model id, or a model alias")
	cmd.Flags().StringVar(&f.effort, "effort", "", "effort to start at, where the model offers one: a level's id or name (default: the model's own)")
	cmd.Flags().BoolVar(&f.fast, "fast", false, "start with the fast setting on, where the model offers one")
	cmd.Flags().BoolVar(&f.noFast, "no-fast", false, "start with the fast setting off, where the model offers one")
	cmd.Flags().BoolVar(&f.noForce, "no-force", false, "disable yolo: the session's agent asks for permission, and a client answers")
	cmd.Flags().StringVar(&f.title, "title", "", "name the session this from its start, pinned as /rename pins a title (one line, at most 120 characters)")
	cmd.Flags().BoolVar(&f.json, "json", false, "print the hub's answer (session.create's result, or with --load session.reopen's) as JSON; with --wait, the result")
	cmd.Flags().StringVar(&f.load, "load", "", "reopen this saved session through the hub instead of starting one: an id craze ps --saved prints, "+
		"or a whole craze or provider session id; it waits for the session's start and sends no prompt")
	// --wait (plan 041 §3.2): another path, past the hub — the session's
	// host spawned here, unattended, sent the prompt and followed to its
	// turn's end.
	cmd.Flags().BoolVar(&f.wait, "wait", false, "start an unattended session here, without the hub, send it the prompt, and wait for its turn's result")
	cmd.Flags().BoolVar(&f.plan, "plan", false, "with --wait: start in plan mode")
	cmd.Flags().BoolVar(&f.ask, "ask", false, "with --wait: start in ask mode")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 0, "with --wait: stop waiting after this long (from the command's start): the result is running, exit 5, with a handle to follow it (default: no limit)")
	cmd.Flags().IntVar(&f.maxMessage, "max-message", 0, "with --wait: print at most this many characters of the message and the plan together, and write the whole result to a file named in the output (default: no limit)")
	registerExitFlags(cmd, &f.exitAfterTurn, &f.sessionFile)
	registerPromptInputFlags(cmd, &f.prompt)
	return cmd
}

// runNew is craze new (the file's comment).
func runNew(cmd *cobra.Command, f *newFlags, args []string) error {
	if f.fast && f.noFast {
		return usagef("craze new: --fast and --no-fast are mutually exclusive")
	}
	title, err := newTitle(cmd, f)
	if err != nil {
		return err
	}
	// An explicit, non-empty --provider is checked here, before anything is
	// dialled (plan 037 LC-4): by its id alone — no CRAZE_PROVIDER, no
	// config, no default — and a wrong one is the usage error every other
	// command gives, exit 2, kind config under --json. Omitted or empty, the
	// hub chooses: its configured default.
	provider := strings.TrimSpace(f.provider)
	if provider != "" {
		if _, err := agent.ProviderByName(provider); err != nil {
			return unknownProviderError(provider)
		}
	}
	out := cmd.OutOrStdout()
	// A stdout whose reader has gone fails its write with EPIPE rather than
	// killing craze new with SIGPIPE before its stderr line (plan 035 r18): a
	// failure printed as JSON first must still say why on stderr and exit 1,
	// as cli.md's "--json failures" promises, and so must a result that
	// cannot be written.
	ignoreSIGPIPE()
	fail := newFail(out, f.json)
	dir := f.dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fail(errkind.Workspace, err, "the current directory: %v", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fail(errkind.Workspace, err, "%s: %v", dir, err)
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fail(errkind.Workspace, nil, "%s is not a directory", abs)
	}
	// The hub's refusal of a create that names no provider when it has no
	// default is wire words (params.provider …), and its badParams carries
	// no reason of its own to tell it by. So in plain mode the same question
	// is asked here first, of the config file the hub reads its default
	// from (hubCreates' tui.ConfigProvider, which trims it), and answered in
	// craze's words — never by matching the hub's message. Not
	// hubDefaultProvider: an unknown configured id is the hub's own
	// (different) refusal, not "no default". --json passes the hub's refusal
	// through whole, as cli.md documents.
	if provider == "" && !f.json && tui.ConfigProvider() == "" {
		return fail(errkind.Config, nil, "no default provider yet; pass --provider %s", newProviderChoices)
	}
	id, err := newRequestID()
	if err != nil {
		return fail(errkind.Internal, err, "%v", err)
	}
	p := protocol.CreateParams{Cwd: abs, Prompt: strings.Join(args, " "), Provider: provider,
		Model: strings.TrimSpace(f.model), Effort: strings.TrimSpace(f.effort), RequestID: id, Title: title}
	if strings.TrimSpace(p.Prompt) == "" {
		p.Prompt = ""
	}
	switch {
	case f.fast:
		on := true
		p.Fast = &on
	case f.noFast:
		off := false
		p.Fast = &off
	}
	if f.noForce {
		p.PermissionMode = protocol.PermissionPrompt
	}
	env := rundir.ProcessEnv()
	ctx, cancel := context.WithTimeout(context.Background(), newBudget)
	defer cancel()
	raw, hello, err := createThroughHub(ctx, env, p)
	if err != nil {
		return fail(hubFailureKind(err), err, "%s", newFailure(err, hello))
	}
	var res protocol.CreateResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fail(errkind.Internal, nil, "the hub's answer: %v", err)
	}
	if f.json {
		if _, err := fmt.Fprintf(out, "%s\n", raw); err != nil {
			return err
		}
	} else {
		ws := res.Session.Host.Workspace
		if ws == "" {
			ws = abs
		}
		// A write that fails (stdout's reader gone: EPIPE, ignoreSIGPIPE) is the
		// run's failure, as the --json result's is: exit 1 with the write's
		// error, never a silent exit 0 (plan 035 r19).
		if _, err := fmt.Fprintf(out, "started %s in %s\n", psShort(res.Session.SessionID), psLine(psTilde(env.Home, ws))); err != nil {
			return err
		}
		if res.Prompt == protocol.CreatePromptUnknown {
			if _, err := fmt.Fprintln(out, newPromptLost); err != nil {
				return err
			}
		}
	}
	if res.Prompt == protocol.CreatePromptRefused {
		return exitf(1, "craze new: the session started and refused its first prompt: %s", sanitizeLine(res.PromptError))
	}
	return nil
}

// createThroughHub is p's create through the hub (the file's comment): Ensure,
// needing sessionCreate — and createTitle for a create with a title
// (hub.CreateNeed, plan 043 P11) — within hubBudget (X34), then the create
// within ctx; a connection that ended before the answer is tried once more,
// the same requestId, through Ensure again — and only that. It answers too the
// hello of the hub that took the create's own connection (plan 046 U2), for
// newFailure — not Ensure's, whose hub may have ended and another bound the
// socket since: the zero value when the create got no hello.
func createThroughHub(ctx context.Context, env rundir.Env, p protocol.CreateParams) (json.RawMessage, protocol.HubHelloResult, error) {
	for retried := false; ; retried = true {
		ectx, cancel := context.WithTimeout(ctx, hubBudget)
		sock, err := hub.Ensure(ectx, env, hub.CreateNeed(p))
		cancel()
		if err != nil {
			return nil, protocol.HubHelloResult{}, err
		}
		raw, hello, err := hub.CreateHello(ctx, sock, newClient, p)
		if err == nil || retried || !errors.Is(err, hub.ErrUnanswered) || ctx.Err() != nil {
			return raw, hello, err
		}
	}
}

// newFailure is a failed hub request in craze new's words: the hub's own
// message for its refusal — a start that failed carries its cause in it — and
// an older hub said as Ensure says it; anything else as it is, on one line.
// A refusal hub.UpgradeRemedy finds may be an upgrade's — a spawn_failed from
// a hub, as hello says it, of another craze version — gains that remedy after
// a "; " (plan 046 U2): the hub's pid, both versions and how to stop it.
func newFailure(err error, hello protocol.HubHelloResult) string {
	msg := err.Error()
	var perr *protocol.Error
	if errors.As(err, &perr) {
		msg = perr.Message
	}
	if remedy := hub.UpgradeRemedy(err, hello); remedy != "" {
		msg += "; " + remedy
	}
	return sanitizeLine(msg)
}

// newFail is craze new's fail, writing to out: every exit-1 path before the
// session started (SF-124, plan 035 P5) — the stderr line as ever and, under
// --json (asJSON), one JSON object on stdout first (newFailureJSON).
func newFail(out io.Writer, asJSON bool) func(kind errkind.Kind, err error, format string, args ...any) error {
	return func(kind errkind.Kind, err error, format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		if asJSON {
			fmt.Fprintf(out, "%s\n", newFailureJSON(err, kind, msg))
		}
		return kindf(kind, "craze new: %s", msg)
	}
}

// hubFailureKind is what a failed hub request was: a refusal by what it says,
// and otherwise a hub that could not be started or reached, whose session did
// not start.
func hubFailureKind(err error) errkind.Kind {
	var perr *protocol.Error
	if !errors.As(err, &perr) {
		if errors.Is(err, hub.ErrUnanswered) {
			return errkind.ConnectionLost
		}
		return errkind.StartFailed
	}
	switch perr.Data.Reason {
	case protocol.ReasonStartFailed:
		return startKind(perr.Data.Kind)
	case protocol.ReasonSpawnFailed, protocol.ReasonClosing, protocol.ReasonNotReady:
		return errkind.StartFailed
	case protocol.ReasonHostUnreachable:
		return errkind.ConnectionLost
	case protocol.ReasonUnknownSession, protocol.ReasonNotResumable, protocol.ReasonBadRequest:
		return errkind.Config
	}
	return errkind.Internal
}

// newFailureJSON is a failed craze new's one JSON object (SF-124, plan 035
// P5): a protocol refusal as {"error": <protocol.Error>}, so a script reads
// .error.data.code, .reason and .cause where it reads them on the wire; any
// other failure as {"error":{"message": msg}}; and either way kind beside it,
// the kind the waiting commands' error says. Neither key is one of
// session.create's result, so success and failure stay disjoint.
func newFailureJSON(err error, kind errkind.Kind, msg string) []byte {
	var body any = struct {
		Message string `json:"message"`
	}{msg}
	var perr *protocol.Error
	if errors.As(err, &perr) {
		body = perr
	}
	raw, merr := json.Marshal(struct {
		Error any          `json:"error"`
		Kind  errkind.Kind `json:"kind"`
	}{body, kind})
	if merr != nil {
		raw = []byte(`{"error":{"message":"craze new failed"},"kind":"internal"}`)
	}
	return raw
}

// newTitle is --title (plan 043 D8, P9): "" when it was not given, and
// otherwise engine.NormalizeTitle's — the hub's own normalization, and craze
// serve --title's, so the title the session ends up with is the one this
// sends — or, for one that normalizes to nothing, a usage error: a title is
// omitted, never sent blank.
func newTitle(cmd *cobra.Command, f *newFlags) (string, error) {
	if f.title == "" && !cmd.Flags().Changed("title") {
		return "", nil
	}
	title := engine.NormalizeTitle(f.title)
	if title == "" {
		return "", usagef("craze new: --title is blank")
	}
	return title, nil
}

// newRequestID is a fresh requestId for one craze new: "new-" and 16 random
// hex digits.
func newRequestID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "new-" + hex.EncodeToString(b[:]), nil
}
