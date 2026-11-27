package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/review"
	"github.com/charliek/craze/internal/routing"
)

var reviewFallbackFloor = 45 * time.Second

type reviewFlags struct {
	routingFlags
	dir, model, base, criteria, out, dirty string
	files                                  []string
	timeout, stall                         time.Duration
	maxMessage                             int
	remove, bundleOnly, keep, json         bool
}
type reviewAttemptJSON struct {
	routing.Choice
	EffectiveEffort string       `json:"effectiveEffort,omitempty"`
	EffectiveModel  string       `json:"effectiveModel,omitempty"`
	Status          string       `json:"status"`
	Kind            errkind.Kind `json:"kind,omitempty"`
	Reason          string       `json:"reason,omitempty"`
	Error           *resultError `json:"error,omitempty"`
	DurationMs      int64        `json:"durationMs"`
}
type reviewResultJSON struct {
	Schema          int                 `json:"schema"`
	Status          string              `json:"status"`
	Message         string              `json:"message"`
	MessageCut      bool                `json:"messageCut,omitempty"`
	Route           *routing.Choice     `json:"route,omitempty"`
	Attempts        []reviewAttemptJSON `json:"attempts"`
	OutPath         string              `json:"outPath,omitempty"`
	DirtyFiles      []string            `json:"dirtyFiles"`
	IncompleteCheck bool                `json:"incompleteCheck,omitempty"`
	BundleFile      string              `json:"bundleFile,omitempty"`
	Error           *resultError        `json:"error,omitempty"`
	Stopping        bool                `json:"stopping,omitempty"`
}

func newReviewCmd() *cobra.Command {
	var f reviewFlags
	cmd := &cobra.Command{Use: "review [PROMPT...]", Short: "Review code with an unattended provider and verify workspace integrity", RunE: func(cmd *cobra.Command, words []string) error {
		err := f.run(cmd, words)
		var ee *exitError
		if f.json && err != nil && (!errors.As(err, &ee) || ee.code == 1) {
			_ = json.NewEncoder(cmd.OutOrStdout()).Encode(failureBody{Error: kindError(err.Error(), failureKind(err))})
		}
		return err
	}}
	f.register(cmd)
	flags := cmd.Flags()
	flags.StringVar(&f.request.Provider, "provider", "", "direct provider or filter for a target")
	flags.StringVar(&f.model, "model", "", "model for a direct provider")
	flags.StringVar(&f.criteria, "criteria", "", "correctness, adversarial, plan, or custom instructions")
	flags.StringVar(&f.base, "changes-since", "", "bundle the net changes since a Git commit")
	flags.StringArrayVar(&f.files, "prompt-file", nil, "read a prompt file, in flag order (- for stdin)")
	flags.BoolVar(&f.remove, "remove-prompt-file", false, "delete regular prompt files after reading them")
	flags.StringVarP(&f.dir, "dir", "C", "", "review workspace")
	flags.DurationVar(&f.timeout, "timeout", 0, "command-wide time budget (defaults to target cap)")
	flags.DurationVar(&f.stall, "stall-timeout", 10*time.Minute, "maximum silence after submission (0 disables)")
	flags.StringVar(&f.out, "out", "", "atomically write the full review to this path")
	flags.IntVar(&f.maxMessage, "max-message", 0, "maximum characters on stdout (0 for all)")
	flags.StringVar(&f.dirty, "dirty-check", "fail", "on workspace mutation: fail, warn, or off")
	flags.BoolVar(&f.keep, "keep-bundle", false, "retain the spill file after review")
	flags.BoolVar(&f.bundleOnly, "bundle-only", false, "emit review materials without starting a host")
	flags.BoolVar(&f.json, "json", false, "print a versioned result")
	return cmd
}

func (f reviewFlags) routes() (routing.Result, bool, error) {
	cfg, req, err := f.inputs()
	if err != nil {
		return routing.Result{}, false, err
	}
	paid := req.AllowPaid || cfg.Routing.AllowPaidFallback
	direct := req.Provider != "" && req.Target == "" && req.Role == ""
	if req.Role != "" && (req.Provider != "" || f.model != "") || f.model != "" && !direct || req.Provider != "" && !routing.KnownProvider(req.Provider) || req.Version != "" && req.Target == "" {
		return routing.Result{}, paid, usagef("craze review: invalid combination of target, provider, role, model and version")
	}
	if role, ok := cfg.Roles[req.Role]; ok && role.Seats != nil {
		return routing.Result{}, paid, usagef("craze review: use craze resolve --role %s for a panel", req.Role)
	}
	if direct {
		model := f.model
		if req.Provider == "native" {
			dir := paths.NativeDir()
			cat, e := modelsCatalog()
			if e != nil {
				return routing.Result{}, paid, e
			}
			table, e := modeltable.LoadReadOnly(dir, cat)
			if e != nil {
				return routing.Result{}, paid, kindf(errkind.Config, "craze review: cannot load native model table")
			}
			if model != "" {
				if alias, e := agent.MatchNativeModel(table, model); e == nil {
					model = alias
				}
			} else {
				pick, e := table.StartPick(modeltable.ReadRecent(dir), os.Getenv)
				if e != nil {
					return routing.Result{}, paid, kindf(errkind.Config, "craze review: no eligible native model")
				}
				model = pick.Alias
			}
		}
		// An unnamed direct route uses the provider's default effort. Its
		// billing class is tightened by the same offline and host probes.
		req.Target = "direct"
		cfg.Targets["direct"] = routing.Target{Cap: "20m", Routes: []routing.Route{{Provider: req.Provider, Model: model, Billing: routing.Subscription}}}
	} else if req.Role == "" && req.Target == "" {
		req.Role = "review"
	}
	res, ok, err := cfg.Resolve(req, offlineRouteProbe())
	if err != nil {
		return res, paid, usageKindf(errkind.Config, "craze review: %s", err)
	}
	if !ok {
		if slices.ContainsFunc(res.Skipped, func(s routing.Skip) bool { return s.Reason == routing.SelfDelegation }) {
			return res, paid, usagef("craze review: no eligible non-self route; --allow-same-provider includes the caller")
		}
		return res, paid, kindf(errkind.Config, "craze review: no eligible review route")
	}
	return res, paid, nil
}

func (f reviewFlags) run(cmd *cobra.Command, words []string) error {
	started := time.Now()
	parent, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ignoreSIGPIPE()
	if err := f.checkFlags(cmd); err != nil {
		return err
	}
	if strings.HasPrefix(f.base, "-") {
		return usagef("craze review: invalid changes-since ref")
	}
	if cmd.Flags().Changed("changes-since") && f.base == "" || f.bundleOnly && f.base == "" || !slices.Contains([]string{"fail", "warn", "off"}, f.dirty) || f.stall < 0 || cmd.Flags().Changed("timeout") && f.timeout <= 0 || f.maxMessage < 0 {
		return usagef("craze review: check the bundle ref, dirty-check mode, and time or output limits")
	}
	if err := f.checkInputs(words); err != nil {
		return err
	}
	res := reviewResultJSON{Schema: 1, Attempts: []reviewAttemptJSON{}, DirtyFiles: []string{}}
	capDuration := f.timeout
	if capDuration == 0 {
		// Bound routing itself while reading the configured target's cap.
		capDuration = 20 * time.Minute
	}
	ctx, stopInitial := context.WithDeadline(parent, started.Add(capDuration))
	defer stopInitial()
	endContext := func() error {
		turn := contextResult(ctx)
		res.Status, res.Stopping, res.Error = turn.Status, turn.Stopping, turn.Error
		return f.render(cmd, res)
	}
	var routes routing.Result
	var paid bool
	if !f.bundleOnly {
		// Offline filesystem reads can still block. Only the caller starts
		// hosts or writes output, so a cancelled lookup cannot run a route.
		type resolved struct {
			result routing.Result
			paid   bool
			err    error
		}
		ready := make(chan resolved, 1)
		go func() { r, p, e := f.routes(); ready <- resolved{r, p, e} }()
		select {
		case <-ctx.Done():
			return endContext()
		case r := <-ready:
			if ctx.Err() != nil {
				return endContext()
			}
			routes, paid = r.result, r.paid
			for _, s := range routes.Skipped {
				fmt.Fprintf(cmd.ErrOrStderr(), "craze review: skipped %s %s %s\n", s.Target, s.Provider, s.Reason)
			}
			for _, w := range routes.Warnings {
				fmt.Fprintln(cmd.ErrOrStderr(), "craze review:", sanitizeLine(w))
			}
			if r.err != nil {
				if f.base != "" {
					dir, e := resolveWorkspace(f.dir)
					if e == nil {
						e = review.ValidateRef(ctx, dir, f.base)
					}
					if ctx.Err() != nil {
						return endContext()
					}
					if failureKind(e) == errkind.Usage {
						return usageKindf(errkind.Usage, "craze review: %s", e)
					}
				}
				return r.err
			}
		}
		capDuration = time.Duration(routes.CapSeconds * float64(time.Second))
	}
	if f.timeout == 0 {
		automatic, cancel := context.WithDeadline(parent, started.Add(capDuration))
		defer cancel()
		ctx = automatic
	}
	dir, err := resolveWorkspace(f.dir)
	if err != nil {
		return kindf(errkind.Workspace, "craze review: %s", err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	input, err := f.readInputs(ctx, cmd, words)
	if ctx.Err() != nil {
		return endContext()
	}
	if err != nil {
		return err
	}
	prompt := review.Instructions(f.criteria, input)
	var bundle review.Bundle
	if f.base != "" {
		bundle, err = review.Build(ctx, dir, f.base, prompt, promptTextMax)
		if ctx.Err() != nil {
			_ = bundle.Remove()
			return endContext()
		}
		if err != nil {
			k := failureKind(err)
			if k == errkind.Prompt || k == errkind.Usage {
				return usageKindf(k, "craze review: %s", err)
			}
			return kindf(k, "craze review: building diff: %s", err)
		}
		prompt = bundle.Prompt
		if !f.keep && !f.bundleOnly {
			defer func() {
				if e := bundle.Remove(); e != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "craze review: removing retained bundle %s: %s\n", bundle.BundleFile, sanitizeLine(e.Error()))
				}
			}()
		} else {
			res.BundleFile = bundle.BundleFile
		}
	}
	if len(prompt) > promptTextMax {
		return usageKindf(errkind.Prompt, "craze review: assembled prompt exceeds 512 KiB")
	}
	if f.bundleOnly {
		if f.json {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(bundle)
		}
		_, err = io.WriteString(cmd.OutOrStdout(), prompt)
		return err
	}
	var before review.Snapshot
	if f.dirty != "off" {
		before, err = review.Inspect(ctx, dir)
		if ctx.Err() != nil {
			return endContext()
		}
		if err != nil {
			return kindf(errkind.Internal, "craze review: checking workspace: %s", err)
		}
		if before.Root == "" {
			fmt.Fprintln(cmd.ErrOrStderr(), "craze review: outside a Git worktree, skipping integrity check")
		}
	}
	for i, route := range routes.Routes {
		if ctx.Err() != nil {
			return endContext()
		}
		start := time.Now()
		fmt.Fprintf(cmd.ErrOrStderr(), "craze review: attempt %d %s/%s\n", i+1, route.Provider, sanitizeLine(route.Model))
		if route.Provider == "antigravity" {
			fmt.Fprintln(cmd.ErrOrStderr(), "craze review: Antigravity can edit files and run commands without prompting under --no-force. Git checks cover the review worktree only")
		}
		a := runUnattendedAttempt(ctx, dir, prompt, route, paid, f.stall)
		t := a.turn
		if a.billing != "" {
			route.Billing = a.billing
			route.Provisional, route.Note = a.billing == "unknown", ""
		} else if !paid && route.Provisional && t.Status == statusCompleted {
			route.Provisional, route.Note = false, ""
		}
		res.Route, res.Status, res.Message, res.Stopping, res.Error = &route, t.Status, t.message(), t.Stopping, t.Error
		res.MessageCut = t.MessageCut
		if a.effectiveEffort != "" && route.Effort != "" && a.effectiveEffort != route.Effort {
			fmt.Fprintf(cmd.ErrOrStderr(), "craze review: %s/%s adjusted effort %s to %s\n", route.Provider, sanitizeLine(route.Model), route.Effort, sanitizeLine(a.effectiveEffort))
		}
		rec := reviewAttemptJSON{Choice: route, EffectiveModel: t.Model, EffectiveEffort: a.effectiveEffort, Status: t.Status, Error: t.Error, DurationMs: time.Since(start).Milliseconds()}
		if t.Error != nil {
			rec.Kind = t.Error.Kind
			if t.Error.Detail != nil {
				rec.Reason = t.Error.Detail.Reason
			}
			for _, reason := range []string{"paid_blocked", "not_signed_in"} {
				if strings.Contains(t.Error.Message, reason+":") {
					rec.Reason = reason
				}
			}
		}
		res.Attempts = append(res.Attempts, rec)
		if a.cleanupErr != nil {
			return kindf(errkind.Internal, "craze review: %s", a.cleanupErr)
		}
		if before.Root != "" {
			// Always inspect after verified shutdown, even after a signal or cap.
			check, done := context.WithTimeout(context.Background(), 30*time.Second)
			after, e := review.Inspect(check, dir)
			done()
			if e != nil {
				res.IncompleteCheck = true
				fmt.Fprintf(cmd.ErrOrStderr(), "craze review: could not verify workspace: %s\n", sanitizeLine(e.Error()))
				if before.IndexChanged() {
					res.DirtyFiles = []string{"index"}
				} else {
					res.Status, res.Error = statusFailed, kindError("could not verify workspace integrity", errkind.Unobserved)
					break
				}
			} else {
				res.IncompleteCheck = before.Incomplete || after.Incomplete
				res.DirtyFiles = review.Changes(before, after)
			}
			if res.IncompleteCheck {
				fmt.Fprintln(cmd.ErrOrStderr(), "craze review: workspace check used bounded file metadata")
			}
			if len(res.DirtyFiles) > 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "craze review: workspace changed:", strings.Join(res.DirtyFiles, ", "))
				if f.dirty == "fail" {
					res.Status = statusFailed
					res.Error = kindError("workspace changed during review", errkind.DirtyTree)
				}
			}
		}
		if t.MessageCut || len(res.DirtyFiles) != 0 {
			break
		}
		// Keep the delivered attempt and its text. A new command cap or
		// signal during mandatory cleanup or inspection has its own exit code.
		if a.settledBeforeEnd && ctx.Err() != nil {
			return endContext()
		}
		if i+1 == len(routes.Routes) || !mayRetry(a) {
			break
		}
		if ctx.Err() != nil {
			return endContext()
		}
		deadline, _ := ctx.Deadline()
		if time.Until(deadline) < reviewFallbackFloor {
			break
		}
	}
	return f.render(cmd, res)
}

func mayRetry(a attemptResult) bool {
	if a.cleanupErr != nil || a.turn.Status != statusFailed || a.turn.Error == nil {
		return false
	}
	switch a.turn.Error.Kind {
	case errkind.Config, errkind.StartFailed:
		return a.startFailure
	case errkind.Auth, errkind.Quota, errkind.ModelNotFound:
		return true
	case errkind.Provider:
		return !a.startFailure && a.turn.Error.Retryable
	}
	return false
}

func (f reviewFlags) render(cmd *cobra.Command, r reviewResultJSON) error {
	if r.MessageCut {
		fmt.Fprintln(cmd.ErrOrStderr(), "craze review: only part of the review was recovered")
	}
	if f.out != "" && !r.MessageCut && strings.TrimSpace(r.Message) != "" {
		if err := os.MkdirAll(filepath.Dir(f.out), 0700); err != nil {
			return kindf(errkind.Internal, "craze review: creating output directory: %s", err)
		}
		if err := atomicfile.Write(f.out, []byte(r.Message), 0600); err != nil {
			return kindf(errkind.Internal, "craze review: writing output: %s", err)
		}
		r.OutPath = f.out
	}
	r.Message, _ = cutMessage(r.Message, f.maxMessage)
	var err error
	if f.json {
		err = json.NewEncoder(cmd.OutOrStdout()).Encode(r)
	} else if r.Message != "" {
		text := r.Message
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		_, err = io.WriteString(cmd.OutOrStdout(), text)
	}
	if err != nil {
		return kindf(errkind.Internal, "craze review: writing stdout: %s", err)
	}
	code := 3
	switch r.Status {
	case statusCompleted:
		return nil
	case reviewTimedOut:
		code = 5
	case statusNeedsInput:
		code = 4
	}
	msg := r.Status
	if r.Error != nil {
		msg = string(r.Error.Kind) + ": " + r.Error.Message
	}
	return exitf(code, "craze review: %s", msg)
}
