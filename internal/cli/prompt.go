package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
)

const (
	// foreignTurnMax bounds the wait for a turn the agent started on its own
	// (grok's interject fallback). Past it craze stops rather than sending a
	// prompt that would be queued behind a turn nobody asked for.
	foreignTurnMax   = 60 * time.Second
	subagentDrainMax = 1500 * time.Millisecond
	subagentQuiet    = 250 * time.Millisecond
	// subagentSweepMax bounds the final sweep so a producer that keeps
	// writing cannot hold the deadline open for ever; it is the session's
	// event buffer, so one pass empties a full channel.
	subagentSweepMax = 256
)

type promptOpts struct {
	workspace string
	model     string
	agentBin  string
	provider  string
	followUps []string
	decisions []string
	force     bool
	noForce   bool
	ask       bool
	plan      bool
	json      bool
	text      string
	stdout    io.Writer
	stderr    io.Writer
	stdin     io.Reader
	cmd       *cobra.Command
}

func newPromptCmd() *cobra.Command {
	o := &promptOpts{force: true}
	cmd := &cobra.Command{
		Use:   "prompt [text]",
		Short: "Run a headless ACP turn (and optional follow-ups)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.stdout = cmd.OutOrStdout()
			o.stderr = cmd.ErrOrStderr()
			o.stdin = cmd.InOrStdin()
			o.cmd = cmd
			if len(args) == 1 {
				o.text = args[0]
			}
			return o.run()
		},
	}
	cmd.Flags().StringVar(&o.workspace, "workspace", "", "existing workspace directory (default: current directory)")
	cmd.Flags().StringVar(&o.model, "model", "", "ACP model id (session/set_model after session/new)")
	cmd.Flags().StringVar(&o.agentBin, "agent-bin", "", "path to cursor-agent / fake agent (or CRAZE_AGENT_BIN)")
	cmd.Flags().StringArrayVar(&o.followUps, "follow-up", nil, "additional prompt on the same ACP session (repeatable)")
	cmd.Flags().StringArrayVar(&o.decisions, "permission-decision", nil, "headless permission answer: allow-once or reject-once (repeatable)")
	cmd.Flags().BoolVar(&o.force, "force", true, "spawn the agent with --force (yolo)")
	cmd.Flags().BoolVar(&o.noForce, "no-force", false, "disable yolo and handle permission requests")
	cmd.Flags().BoolVar(&o.ask, "ask", false, "set session mode to ask after session/new")
	cmd.Flags().BoolVar(&o.plan, "plan", false, "set session mode to plan after session/new")
	cmd.Flags().BoolVar(&o.json, "json", false, "write only JSON events to stdout")
	registerProviderFlag(cmd, &o.provider)
	return cmd
}

func (o *promptOpts) run() (retErr error) {
	if o.ask && o.plan {
		return usagef("craze: --ask and --plan are mutually exclusive")
	}
	for _, d := range o.decisions {
		if _, err := decisionKind(d); err != nil {
			return usagef("craze: %v", err)
		}
	}
	if o.noForce {
		o.force = false
	}
	text, err := o.resolveText()
	if err != nil {
		return err
	}
	ws, err := resolveWorkspace(o.workspace)
	if err != nil {
		return err
	}

	resolved, err := resolveProvider(o.cmd, o.provider, o.stderr, false)
	if err != nil {
		return err
	}

	parent := context.Background()
	if o.cmd != nil {
		parent = o.cmd.Context()
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	prov := resolved.Provider
	sess := agent.New(agent.Options{
		Binary:    o.agentBin,
		Workspace: ws,
		Force:     o.force,
		Model:     o.model,
		Mode:      o.mode(),
		Stderr:    o.stderr,
		Provider:  &prov,
	})
	defer func() { _ = sess.Close() }()

	var inRun atomic.Bool
	inRun.Store(true)
	defer inRun.Store(false)
	go func() {
		<-ctx.Done()
		if inRun.Load() {
			// A signal stops everything pending, not just the running turn:
			// the queue goes first so nothing starts behind the cancel.
			sess.ClearQueue()
			_ = sess.Cancel(context.Background())
		}
	}()

	// decisions is the headless permission queue, consumed by whichever
	// reader of the event stream sees the request — a turn's own loop, the
	// drain between turns, or a turn the agent started on its own.
	decisions := append([]string{}, o.decisions...)

	if err := sess.Start(ctx); err != nil {
		if o.json {
			writeErrorEvent(o.stdout, err)
		}
		return err
	}
	defer func() {
		// A drain write that fails means the JSON on stdout is incomplete;
		// that is an error even when everything else succeeded. The queue's
		// own last events — a signal's removed lines — are flushed first.
		if err := o.flushEvents(sess, &decisions); err != nil && retErr == nil {
			retErr = err
		}
		if err := o.drainSubagents(sess); err != nil && retErr == nil {
			retErr = err
		}
	}()
	if err := persistProvider(resolved); err != nil {
		fmt.Fprintf(o.stderr, "craze: not saving the provider: %v\n", err)
	}

	// --follow-up is the headless queue: every follow-up is queued before the
	// first turn, and the drain below sends them one per settled turn, in
	// order, exactly as the TUI does.
	for _, f := range o.followUps {
		if _, err := sess.Queue(f); err != nil {
			return fmt.Errorf("craze: %w", err)
		}
	}
	forcedReject := false
	turn := text
	for {
		// A turn the agent started on its own can begin between the check
		// below and the prompt going out, and the session refuses a prompt
		// while one runs. The refusal reaches nothing: no turn was attempted
		// and no queued message was lost, so it is waited out and retried
		// once rather than reported.
		res, rejected, err := o.runTurn(sess, turn, &decisions)
		if errors.Is(err, agent.ErrForeignTurn) {
			if werr := o.waitForeignTurn(sess, &decisions); werr != nil {
				return werr
			}
			res, rejected, err = o.runTurn(sess, turn, &decisions)
		}
		if rejected {
			forcedReject = true
		}
		if err != nil {
			return err
		}
		if err := o.flushEvents(sess, &decisions); err != nil {
			return err
		}
		// The stop reason still ends the chain exactly as it did before the
		// queue existed: anything but end_turn is exit 1 and no more turns.
		if res.StopReason != "end_turn" {
			return &exitError{code: 1, msg: ""}
		}
		next, ok, err := o.nextQueued(ctx, sess, &decisions)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		turn = next.Text
	}
	// A signal that landed while the queue was draining still ends the run,
	// even if every turn that did run ended end_turn.
	if ctx.Err() != nil {
		return &exitError{code: 1, msg: ""}
	}
	if forcedReject {
		return &exitError{code: 1, msg: ""}
	}
	return nil
}

// nextQueued is the headless drain: wait out any turn the agent is running on
// its own, then take the head. A blocked take is not an empty queue — the two
// are told apart by the snapshot, so follow-ups are never abandoned with an
// exit status that says everything ran.
func (o *promptOpts) nextQueued(ctx context.Context, sess agent.Session, queue *[]string) (agent.QueuedPrompt, bool, error) {
	deadline := time.Now().Add(foreignTurnMax)
	for {
		if err := o.waitForeignTurn(sess, queue); err != nil {
			return agent.QueuedPrompt{}, false, err
		}
		// A signal clears the queue and ends the run; taking a row now would
		// start a turn nothing is left to cancel.
		if ctx.Err() != nil {
			return agent.QueuedPrompt{}, false, nil
		}
		next, ok := sess.PopQueue()
		if ok {
			if err := o.flushEvents(sess, queue); err != nil {
				return agent.QueuedPrompt{}, false, err
			}
			return next, true, nil
		}
		if len(sess.Snapshot().Queue) == 0 {
			return agent.QueuedPrompt{}, false, nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(o.stderr, "craze: the queue is still blocked after %s\n", foreignTurnMax)
			return agent.QueuedPrompt{}, false, &exitError{code: 1, msg: ""}
		}
		// Something still holds the drain. Let its events through and look
		// again rather than spinning on the snapshot.
		if err := o.flushEvents(sess, queue); err != nil {
			return agent.QueuedPrompt{}, false, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// flushEvents writes whatever the session has already emitted without waiting
// for more. Queue changes happen between turns, where runTurn's own loop is
// not reading, so this is what puts them on stdout in the order they happened.
func (o *promptOpts) flushEvents(sess agent.Session, queue *[]string) error {
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				return nil
			}
			if _, err := o.consume(sess, ev, queue); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// consume is what every reader of the event stream does with one event: write
// it, and answer it if it is a permission request. A request that arrives
// outside a turn craze prompted — during a foreign turn, or between turns —
// is still the agent waiting on an answer, so every reader goes through here.
func (o *promptOpts) consume(sess agent.Session, ev agent.Event, queue *[]string) (bool, error) {
	if err := o.writeEvent(ev); err != nil {
		return false, err
	}
	if ev.Type != agent.EventPermission || ev.Permission == nil {
		return false, nil
	}
	return o.answerPermission(sess, ev.Permission, queue)
}

// waitForeignTurn waits out a turn the agent started on its own before the
// next queued message goes. Sending into one would put craze's prompt in the
// agent's queue, where its completion is no longer craze's to recognise.
func (o *promptOpts) waitForeignTurn(sess agent.Session, queue *[]string) error {
	if !sess.Snapshot().ForeignTurn {
		return nil
	}
	deadline := time.NewTimer(foreignTurnMax)
	defer deadline.Stop()
	for sess.Snapshot().ForeignTurn {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				return nil
			}
			// A foreign turn runs tools of its own, so it can ask for
			// permission; nothing else is reading the stream here.
			if _, err := o.consume(sess, ev, queue); err != nil {
				return err
			}
		case <-deadline.C:
			fmt.Fprintf(o.stderr, "craze: the agent is still running a turn of its own after %s\n", foreignTurnMax)
			return &exitError{code: 1, msg: ""}
		}
	}
	return nil
}

func (o *promptOpts) mode() string {
	if o.ask {
		return "ask"
	}
	if o.plan {
		return "plan"
	}
	return ""
}

func (o *promptOpts) resolveText() (string, error) {
	if strings.TrimSpace(o.text) != "" {
		return o.text, nil
	}
	if stdinIsTTY(o.stdin) {
		return "", usagef("craze: prompt text required (or pipe stdin)")
	}
	b, err := io.ReadAll(o.stdin)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return "", usagef("craze: prompt text required (or pipe stdin)")
	}
	return text, nil
}

func (o *promptOpts) runTurn(sess agent.Session, text string, queue *[]string) (agent.Result, bool, error) {
	type outcome struct {
		res agent.Result
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := sess.Prompt(context.Background(), text)
		ch <- outcome{res, err}
	}()

	rejected := false
	for {
		ev, ok := <-sess.Events()
		if !ok {
			break
		}
		r, err := o.consume(sess, ev, queue)
		if err != nil {
			return agent.Result{}, rejected || ev.Type == agent.EventPermission, err
		}
		if r {
			rejected = true
		}
		if ev.Type == agent.EventDone || ev.Type == agent.EventError {
			out := <-ch
			if ev.Type == agent.EventError && out.err == nil {
				out.err = ev.Err
			}
			if out.err != nil {
				return agent.Result{}, rejected, out.err
			}
			return out.res, rejected, nil
		}
	}
	out := <-ch
	return out.res, rejected, out.err
}

func (o *promptOpts) writeEvent(ev agent.Event) error {
	if o.json {
		return encodeEvent(o.stdout, ev)
	}
	if ev.Type == agent.EventText && ev.Agent == "" {
		_, err := io.WriteString(o.stdout, ev.Text)
		return err
	}
	return nil
}

func (o *promptOpts) drainSubagents(sess agent.Session) error {
	return drainSubagentEvents(sess.Events(), sess.Snapshot, o.writeEvent)
}

func drainSubagentEvents(events <-chan agent.Event, snapFn func() agent.Snapshot, write func(agent.Event) error) error {
	if !hasSpawnedSubagent(snapFn()) {
		return nil
	}
	maxTimer := time.NewTimer(subagentDrainMax)
	defer maxTimer.Stop()
	var quiet *time.Timer
	stopQuiet := func() {
		if quiet == nil {
			return
		}
		if !quiet.Stop() {
			select {
			case <-quiet.C:
			default:
			}
		}
		quiet = nil
	}
	defer stopQuiet()

	for {
		running := hasRunningSubagent(snapFn())
		var quietC <-chan time.Time
		if running {
			stopQuiet()
		} else {
			if quiet == nil {
				quiet = time.NewTimer(subagentQuiet)
			}
			quietC = quiet.C
		}
		select {
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if quiet != nil {
				if !quiet.Stop() {
					select {
					case <-quiet.C:
					default:
					}
				}
				quiet.Reset(subagentQuiet)
			}
			if err := write(ev); err != nil {
				return err
			}
		case <-quietC:
			return sweepEvents(events, write)
		case <-maxTimer.C:
			return sweepEvents(events, write)
		}
	}
}

// sweepEvents writes what is already buffered, without blocking. A deadline
// and a ready event can both be ready when select runs, and select picks
// between them at random: without this, a timer could return while the
// session had already produced lines that stdout never got.
func sweepEvents(events <-chan agent.Event, write func(agent.Event) error) error {
	for i := 0; i < subagentSweepMax; i++ {
		select {
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if err := write(ev); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

func hasSpawnedSubagent(snap agent.Snapshot) bool {
	return len(snap.Subagents) > 0
}

func hasRunningSubagent(snap agent.Snapshot) bool {
	for _, a := range snap.Subagents {
		if a.Status == agent.SubagentRunning || a.Status == "" {
			return true
		}
	}
	return false
}

func (o *promptOpts) answerPermission(sess agent.Session, perm *agent.PermissionEvent, queue *[]string) (bool, error) {
	id, rejected, rest, perr := pickPermission(perm.Options, *queue)
	*queue = rest
	if perr != nil {
		if err := sess.AnswerPermission(perm.ID, ""); err != nil {
			return true, err
		}
		return true, nil
	}
	if err := sess.AnswerPermission(perm.ID, id); err != nil {
		return rejected, err
	}
	return rejected, nil
}

func pickPermission(opts []agent.PermissionOption, queue []string) (optionID string, rejected bool, rest []string, err error) {
	for i, raw := range queue {
		kind, decErr := decisionKind(raw)
		if decErr != nil {
			continue
		}
		if id, ok := optionIDForKind(opts, kind); ok {
			rest = append(append([]string{}, queue[:i]...), queue[i+1:]...)
			return id, kind == "reject_once" || kind == "reject_always", rest, nil
		}
	}
	if id, ok := optionIDForKind(opts, "reject_once"); ok {
		return id, true, queue, nil
	}
	return "", true, nil, fmt.Errorf("craze: no permission decision remaining")
}

func optionIDForKind(opts []agent.PermissionOption, kind string) (string, bool) {
	return agent.OptionIDForKind(opts, kind)
}

func decisionKind(raw string) (string, error) {
	switch strings.ReplaceAll(strings.TrimSpace(strings.ToLower(raw)), "-", "_") {
	case "allow_once":
		return "allow_once", nil
	case "reject_once":
		return "reject_once", nil
	default:
		return "", fmt.Errorf("invalid --permission-decision %q (want allow-once or reject-once)", raw)
	}
}

func resolveWorkspace(path string) (string, error) {
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = wd
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", usagef("craze: --workspace %s: %v", path, err)
	}
	if !st.IsDir() {
		return "", usagef("craze: --workspace must be an existing directory")
	}
	return path, nil
}

func stdinIsTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
