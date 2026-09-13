package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
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

func (o *promptOpts) run() error {
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
			_ = sess.Cancel(context.Background())
		}
	}()

	if err := sess.Start(ctx); err != nil {
		if o.json {
			writeErrorEvent(o.stdout, err)
		}
		return err
	}
	if err := persistProvider(resolved); err != nil {
		fmt.Fprintf(o.stderr, "craze: not saving the provider: %v\n", err)
	}

	turns := append([]string{text}, o.followUps...)
	queue := append([]string{}, o.decisions...)
	forcedReject := false
	for _, turn := range turns {
		res, rejected, err := o.runTurn(sess, turn, &queue)
		if rejected {
			forcedReject = true
		}
		if err != nil {
			return err
		}
		if res.StopReason != "end_turn" {
			return &exitError{code: 1, msg: ""}
		}
	}
	if forcedReject {
		return &exitError{code: 1, msg: ""}
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
		if o.json {
			if err := encodeEvent(o.stdout, ev); err != nil {
				return agent.Result{}, rejected, err
			}
		} else if ev.Type == agent.EventText {
			if _, err := io.WriteString(o.stdout, ev.Text); err != nil {
				return agent.Result{}, rejected, err
			}
		}
		if ev.Type == agent.EventPermission && ev.Permission != nil {
			r, err := o.answerPermission(sess, ev.Permission, queue)
			if err != nil {
				return agent.Result{}, true, err
			}
			if r {
				rejected = true
			}
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
	for _, o := range opts {
		if o.Kind == kind && o.OptionID != "" {
			return o.OptionID, true
		}
	}
	return "", false
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
