package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/routing"
)

type resolveResultJSON = routing.Result
type routingFlags struct {
	request routing.Request
	caller  string
}

func (f *routingFlags) register(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.StringVar(&f.request.Target, "target", "", "target name, optionally pinned as name@version")
	flags.StringVar(&f.request.Version, "version", "", "numeric version for --target")
	flags.StringVar(&f.request.Role, "role", "", "configured routing role")
	flags.StringVar(&f.request.Effort, "effort", "", "preferred reasoning effort")
	flags.StringVar(&f.caller, "caller", "", "caller provider for maker exclusion")
	flags.BoolVar(&f.request.AllowPaid, "allow-paid-fallback", false, "include eligible API-key and metered routes")
	flags.BoolVar(&f.request.AllowSame, allowSameProviderFlag, false, "include routes on the calling provider")
}
func (f routingFlags) checkFlags(cmd *cobra.Command) error {
	for _, name := range []string{"target", "version", "role", "caller", "effort", "provider", "model"} {
		flag := cmd.Flags().Lookup(name)
		if flag != nil && flag.Changed && flag.Value.String() == "" {
			return usagef("%s: --%s requires a value", cmd.CommandPath(), name)
		}
	}
	return nil
}
func (f routingFlags) inputs() (routing.Config, routing.Request, error) {
	cfg, err := routing.Load(paths.ConfigPath())
	if err != nil {
		return cfg, f.request, usageKindf(errkind.Config, "craze: %s", err)
	}
	f.request.Caller, err = routing.DetectCaller(os.Getenv, callerProvider(os.Getenv), f.caller)
	if err != nil {
		return cfg, f.request, usagef("craze: %s", err)
	}
	return cfg, f.request, nil
}
func newResolveCmd() *cobra.Command {
	var f routingFlags
	var asJSON bool
	cmd := &cobra.Command{Use: "resolve", Short: "Show eligible routes for configured model targets and roles", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := f.checkFlags(cmd); err != nil {
			return err
		}
		cfg, req, err := f.inputs()
		if err != nil {
			return err
		}
		res, ok, err := cfg.Resolve(req, offlineRouteProbe())
		if err != nil {
			return usageKindf(errkind.Config, "craze resolve: %s", err)
		}
		for _, w := range res.Warnings {
			fmt.Fprintln(cmd.ErrOrStderr(), "craze resolve:", w)
		}
		selfOnly := !ok && req.Target != "" && len(res.Skipped) > 0 && slices.ContainsFunc(res.Skipped, func(s routing.Skip) bool { return s.Reason == routing.SelfDelegation })
		if asJSON {
			err = json.NewEncoder(cmd.OutOrStdout()).Encode(res)
		} else {
			err = printResolution(cmd.OutOrStdout(), res)
		}
		if err != nil {
			return err
		}
		if selfOnly {
			return &exitError{code: 2, kind: errkind.Usage, reported: asJSON, msg: "craze resolve: no eligible non-self route; --allow-same-provider includes the caller"}
		}
		if !ok {
			return kindf(errkind.Config, "craze resolve: no eligible route for one or more selections")
		}
		return nil
	}}
	f.register(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print a versioned routing document")
	return cmd
}

// offlineRouteProbe defers each local lookup until a matching route needs it.
// Never probe a provider through an RPC, discovery call, or a host spawn.
func offlineRouteProbe() routing.Probe {
	in := hubAvailInputs(0)
	native := sync.OnceValue(loadNativeProbe)
	overage := sync.OnceValue(agent.ClaudeOverageHeld)
	return func(provider, model string) routing.Facts {
		if provider == "native" {
			return native()(model)
		}
		p, err := agent.ProviderByName(provider)
		if err != nil || providerAvailability(in, p).State != availReady {
			return routing.Facts{Reason: routing.BinaryMissing}
		}
		switch provider {
		case "claude":
			if overage() {
				return routing.Facts{Reason: routing.OverageHold}
			}
		case "grok":
			if os.Getenv("XAI_API_KEY") != "" || os.Getenv("GROK_CODE_XAI_API_KEY") != "" {
				return routing.Facts{Billing: routing.APIKey}
			}
		case "cursor":
			if os.Getenv("CURSOR_API_KEY") != "" {
				return routing.Facts{Billing: routing.APIKey}
			}
		case "antigravity":
			return routing.Facts{Efforts: antigravityRouteEfforts(model)}
		case "codex":
			f := routing.Facts{Efforts: []string{"minimal", "low", "medium", "high", "xhigh"}}
			if os.Getenv("CODEX_API_KEY") != "" {
				f.Billing = routing.APIKey
			} else {
				f.Provisional = true
				f.Note = "verified at start via account/read"
			}
			return f
		}
		return routing.Facts{}
	}
}

// Match the installed adapter's known base IDs and its legacy effort-suffixed
// aliases. Unknown models have no offline effort information.
func antigravityRouteEfforts(model string) []string {
	if model == "gpt-oss-120b-medium" || strings.HasPrefix(model, "gemini-pro-orchestrator-") {
		return []string{}
	}
	for _, level := range []string{"-low", "-medium", "-high", "-xhigh"} {
		if base, ok := strings.CutSuffix(model, level); ok {
			model = base
			break
		}
	}
	switch model {
	case "gemini-4-neon", "gemini-4-hydrogen":
		return []string{"low", "medium", "high", "xhigh"}
	case "gemini-4-argon", "gemini-3.8-flash", "claude-opus-5-5", "claude-sonnet-5-5":
		return []string{"low", "medium", "high"}
	}
	return nil
}

func loadNativeProbe() func(string) routing.Facts {
	dir := paths.NativeDir()
	cat, err := modelsCatalog()
	if err != nil {
		return unavailableNativeRoute
	}
	t, err := modeltable.LoadReadOnly(dir, cat)
	if err != nil {
		return unavailableNativeRoute
	}
	offers := t.Offers(os.Getenv)
	envOnly := t.EnvironmentOnlySources(os.Getenv)
	planAvailable := sync.OnceValue(t.ChatGPTPlanAvailable)
	planModels := sync.OnceValue(func() *chatgptauth.Models {
		return chatgptauth.CurrentModels(dir, chatgptauth.ModelsMaxAge, modeltable.ChatGPTModelsClientVersion())
	})
	return func(alias string) routing.Facts {
		m, exists := t.Models[alias]
		pname := m.Provider
		if !exists {
			pname, _, _ = strings.Cut(alias, "/")
		}
		driver := t.Providers[pname].Driver
		f := routing.Facts{}
		if driver == modeltable.DriverChatGPT {
			f.Billing = routing.Subscription
			if !planAvailable() {
				f.Reason = routing.NotSignedIn
				return f
			}
			if current := planModels(); current == nil {
				f.Provisional = true
				f.Note = string(routing.ChatGPTListUncached)
			} else if !exists || !slices.ContainsFunc(current.Models, func(entry chatgptauth.Model) bool { return entry.Slug == m.WireModel }) {
				f.Reason = routing.ModelUnavailable
				return f
			}
		} else if driver == modeltable.DriverOpenRouter {
			f.Billing = routing.Metered
		} else if driver != "" {
			f.Billing = routing.APIKey
		}
		switch {
		case !exists && f.Provisional:
			f.Efforts = modeltable.ChatGPTEfforts()
		case !exists:
			f.Reason = routing.ModelUnavailable
		case offers.Held[alias] != nil || envOnly[pname] != "":
			f.Reason = routing.HeldEnvKey
		case !offers.Runs[alias]:
			f.Reason = routing.Unfunded
		default:
			f.Efforts = append([]string{}, m.Efforts...)
		}
		return f
	}
}
func unavailableNativeRoute(string) routing.Facts { return routing.Facts{Reason: routing.Unfunded} }

func printResolution(w io.Writer, r routing.Result) error {
	var b strings.Builder
	fmt.Fprintln(&b, "caller:", cmp.Or(strings.Join(r.Caller.Providers, ", "), "unknown"))
	rows := r.Targets
	if r.Kind == "panel" {
		rows = r.Seats
	}
	if r.Kind == "role" || r.Kind == "target" {
		rows = []routing.Selection{{Routes: r.Routes, Skipped: r.Skipped}}
	}
	for _, row := range rows {
		if row.Target != "" {
			fmt.Fprintln(&b, row.Target, cmp.Or(row.Mode, row.Status))
		}
		for i, c := range row.Routes {
			label := "selected"
			if i > 0 {
				label = "fallback"
			}
			fmt.Fprintf(&b, "  %s  %s  %s/%s  %s", label, c.Target, c.Provider, c.Model, c.Billing)
			if c.Provisional {
				fmt.Fprintf(&b, "  provisional (%s)", c.Note)
			}
			fmt.Fprintln(&b)
		}
		for _, s := range row.Skipped {
			fmt.Fprintf(&b, "  skipped  %s  %s %s  %s\n", s.Target, s.Provider, s.Model, s.Reason)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
