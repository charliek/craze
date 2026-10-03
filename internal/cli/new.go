package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/version"
)

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
// the session runs on, idle). With --json every one of those failures also
// prints one JSON object on stdout (newFailureJSON); usage errors stay plain.

const (
	// newBudget bounds a craze new: the hub's Ensure, and the create, which
	// waits for the session's start (60 s on the hub) and its first prompt's
	// answer (15 s).
	newBudget = 2 * time.Minute
	// newPromptLost is what craze new adds when the first prompt's answer was
	// lost.
	newPromptLost = "the prompt may not have reached it"
)

// newClient is who craze new says it is to the hub.
var newClient = protocol.ClientInfo{Kind: "cli", Name: "craze new", Version: version.Version}

type newFlags struct {
	dir                   string
	provider, model       string
	effort                string
	fast, noFast, noForce bool
	json                  bool
}

func newNewCmd() *cobra.Command {
	f := &newFlags{}
	cmd := &cobra.Command{
		Use:   "new [PROMPT...]",
		Short: "Start a session in the background, through the per-machine hub",
		Long: "craze new starts a session in its own host, through the per-machine hub (started on " +
			"demand), in the current directory or -C's, and returns once it has started: its id and " +
			"directory, and — given a PROMPT — whether its first prompt was taken. The session runs on; " +
			"craze attach, the session list (←) and craze ps find it. Without --provider the hub's " +
			"configured default (config.toml's provider) starts it.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
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
	cmd.Flags().BoolVar(&f.json, "json", false, "print the hub's answer (session.create's result) as JSON")
	return cmd
}

// runNew is craze new (the file's comment).
func runNew(cmd *cobra.Command, f *newFlags, args []string) error {
	if f.fast && f.noFast {
		return usagef("craze new: --fast and --no-fast are mutually exclusive")
	}
	out := cmd.OutOrStdout()
	// A stdout whose reader has gone fails its write with EPIPE rather than
	// killing craze new with SIGPIPE before its stderr line (plan 035 r18): a
	// failure printed as JSON first must still say why on stderr and exit 1,
	// as cli.md's "--json failures" promises, and so must a result that
	// cannot be written.
	ignoreSIGPIPE()
	// fail is every exit-1 path before the session started (SF-124, plan 035
	// P5): the stderr line as ever and, under --json, one JSON object on
	// stdout first (newFailureJSON).
	fail := func(err error, format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		if f.json {
			fmt.Fprintf(out, "%s\n", newFailureJSON(err, msg))
		}
		return exitf(1, "craze new: %s", msg)
	}
	dir := f.dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fail(err, "the current directory: %v", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fail(err, "%s: %v", dir, err)
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fail(nil, "%s is not a directory", abs)
	}
	id, err := newRequestID()
	if err != nil {
		return fail(err, "%v", err)
	}
	p := protocol.CreateParams{Cwd: abs, Prompt: strings.Join(args, " "), Provider: strings.TrimSpace(f.provider),
		Model: strings.TrimSpace(f.model), Effort: strings.TrimSpace(f.effort), RequestID: id}
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
	raw, err := createThroughHub(ctx, env, p)
	if err != nil {
		return fail(err, "%s", newFailure(err))
	}
	var res protocol.CreateResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fail(nil, "the hub's answer: %v", err)
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
		fmt.Fprintf(out, "started %s in %s\n", psShort(res.Session.SessionID), psLine(psTilde(env.Home, ws)))
		if res.Prompt == protocol.CreatePromptUnknown {
			fmt.Fprintln(out, newPromptLost)
		}
	}
	if res.Prompt == protocol.CreatePromptRefused {
		return exitf(1, "craze new: the session started and refused its first prompt: %s", sanitizeLine(res.PromptError))
	}
	return nil
}

// createThroughHub is p's create through the hub (the file's comment): Ensure,
// needing sessionCreate, within hubBudget (X34), then the create within ctx;
// a connection that ended before the answer is tried once more, the same
// requestId, through Ensure again — and only that.
func createThroughHub(ctx context.Context, env rundir.Env, p protocol.CreateParams) (json.RawMessage, error) {
	for retried := false; ; retried = true {
		ectx, cancel := context.WithTimeout(ctx, hubBudget)
		sock, err := hub.Ensure(ectx, env, protocol.ConnectionCapabilities{SessionCreate: true})
		cancel()
		if err != nil {
			return nil, err
		}
		raw, err := hub.Create(ctx, sock, newClient, p)
		if err == nil || retried || !errors.Is(err, hub.ErrUnanswered) || ctx.Err() != nil {
			return raw, err
		}
	}
}

// newFailure is a failed create in craze new's words: the hub's own message
// for its refusal — a start that failed carries its cause in it — and an
// older hub said as Ensure says it; anything else as it is, on one line.
func newFailure(err error) string {
	var perr *protocol.Error
	if errors.As(err, &perr) {
		return sanitizeLine(perr.Message)
	}
	return sanitizeLine(err.Error())
}

// newFailureJSON is a failed craze new's one JSON object (SF-124, plan 035
// P5): a protocol refusal as {"error": <protocol.Error>}, so a script reads
// .error.data.code, .reason and .cause where it reads them on the wire; any
// other failure as {"error":{"message": msg}}. Its key, "error", is not one
// of session.create's result, so success and failure stay disjoint.
func newFailureJSON(err error, msg string) []byte {
	var body any = struct {
		Message string `json:"message"`
	}{msg}
	var perr *protocol.Error
	if errors.As(err, &perr) {
		body = perr
	}
	raw, merr := json.Marshal(map[string]any{"error": body})
	if merr != nil {
		raw = []byte(`{"error":{"message":"craze new failed"}}`)
	}
	return raw
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
