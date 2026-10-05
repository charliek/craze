package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/version"
)

// craze providers (plan 036 §3.2): the agent providers this craze could
// start a session of, from this process's own view — the availability check's
// TUI and CLI column (processAvailInputs), with no --agent-bin, over the
// listed providers (a missing gx is left out) — each with its state and, when
// it is not ready, why and what to do. It exits 0 whenever it answered,
// whatever the states say.
//
// --hub asks the running hub instead (sessions.createOptions): its own view —
// the binaries its hosts would find, its login session — with the default
// provider and the recent directories it offers a create. It finds a hub and
// never starts one (hub.Find): over ssh a started hub would be the very
// ssh-born hub it is asking about. No hub running, or one that cannot answer
// (an older craze, left to exit when idle), is exit 1.

type providersFlags struct {
	json bool
	hub  bool
}

func newProvidersCmd() *cobra.Command {
	f := &providersFlags{}
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "List the agent providers and whether each can start here",
		Long: "craze providers lists the agent providers craze can start a session of — cursor, grok, " +
			"gx when it is installed, and native — with each one's state on this machine: ready, " +
			"needs setup, or unavailable, and for the last two why and what to do about it. These are " +
			"agent providers; craze auth list shows the native provider's model providers. --hub asks " +
			"the running hub instead, and never starts one.",
		// An argument is a usage error, exit 2, as craze auth list's is.
		Args: authArgs(0, "takes no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.hub {
				return runProvidersHub(cmd, *f)
			}
			return runProviders(cmd, *f)
		},
	}
	// A flag it does not take is a usage error, exit 2, as an argument is:
	// the root's (usageFlagError), which never quotes the flag.
	cmd.Flags().BoolVar(&f.json, "json", false, "print the providers as JSON")
	cmd.Flags().BoolVar(&f.hub, "hub", false, "ask the running hub what it can create (never starts one)")
	return cmd
}

// providersJSON is craze providers --json: the providers, each as the wire's
// providerOption (sessions.createOptions, plan 036 §3.4) spells one.
type providersJSON struct {
	Providers []protocol.ProviderOption `json:"providers"`
}

// providerOptions is avail as the wire's providerOptions: each provider's id,
// its label (DisplayName), its state in the wire's words, and its reason and
// fix, both "" — absent — when ready. Never nil.
func providerOptions(avail []providerAvail) []protocol.ProviderOption {
	out := make([]protocol.ProviderOption, 0, len(avail))
	for _, a := range avail {
		out = append(out, protocol.ProviderOption{
			ID: a.P.Name(), Label: a.P.DisplayName(), State: protocol.ProviderState(a.State), Reason: a.Reason, Fix: a.Fix,
		})
	}
	return out
}

// providersHeader is craze providers' header row.
const providersHeader = "PROVIDER\tSTATE\tREASON\tFIX"

// runProviders is craze providers (the file's comment). The launch provider
// the column resolves CRAZE_AGENT_BIN for is the one a plain craze would
// start: $CRAZE_PROVIDER, else the config's, else cursor.
func runProviders(cmd *cobra.Command, f providersFlags) error {
	launch, err := resolveProvider(nil, "", errWriter(cmd), false)
	if err != nil {
		return exitf(1, "craze providers: %v", err)
	}
	options := providerOptions(availability(processAvailInputs(launch.Provider, ""), agent.Providers()))
	out := cmd.OutOrStdout()
	if f.json {
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		err = enc.Encode(providersJSON{Providers: options})
	} else {
		err = writeProvidersTable(out, options, func(s string) string { return s })
	}
	if err != nil {
		return exitf(1, "craze providers: %v", err)
	}
	return nil
}

// writeProvidersTable writes options as craze providers' table: the header,
// then a row a provider — its id, its state in words (needs_setup as "needs
// setup"), its reason and fix, a ready one's "-" (psNone) — each cell through
// clean.
func writeProvidersTable(out io.Writer, options []protocol.ProviderOption, clean func(string) string) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, providersHeader)
	for _, o := range options {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", cmp.Or(clean(o.ID), psNone), availState(clean(string(o.State))).human(),
			cmp.Or(clean(o.Reason), psNone), cmp.Or(clean(o.Fix), psNone))
	}
	return tw.Flush()
}

// providersClient is who craze providers --hub says it is to the hub.
var providersClient = protocol.ClientInfo{Kind: "cli", Name: "craze providers", Version: version.Version}

// noHubRunning is craze providers --hub's word for no hub to ask.
const noHubRunning = "no hub is running"

// runProvidersHub is craze providers --hub (the file's comment): the hub
// found (hub.Find, needing createOptions) and asked (hub.CreateOptions)
// within hubBudget. --json prints the result as the hub wrote it; the table
// is its providers, then "default:" — the default provider, "-" for none —
// and a "recent:" line a directory, newest first ("recent: -" for none),
// every cell one line, its controls and escape sequences gone (psLine).
func runProvidersHub(cmd *cobra.Command, f providersFlags) error {
	ctx, cancel := context.WithTimeout(context.Background(), hubBudget)
	defer cancel()
	raw, err := providersFromHub(ctx, rundir.ProcessEnv())
	if errors.Is(err, hub.ErrNoHub) {
		return exitf(1, "craze providers: %s", noHubRunning)
	}
	if err != nil {
		return exitf(1, "craze providers: %s", sanitizeLine(err.Error()))
	}
	out := cmd.OutOrStdout()
	if f.json {
		_, err = fmt.Fprintf(out, "%s\n", raw)
	} else {
		err = writeHubOptions(out, raw)
	}
	if err != nil {
		return exitf(1, "craze providers: %v", err)
	}
	return nil
}

// providersFromHub is the running hub's sessions.createOptions result, as the
// hub wrote it: hub.ErrNoHub when no hub answers, a *hub.LacksError when the
// one that does cannot say.
func providersFromHub(ctx context.Context, env rundir.Env) (json.RawMessage, error) {
	sock, err := hub.Find(ctx, env, protocol.ConnectionCapabilities{CreateOptions: true})
	if err != nil {
		return nil, err
	}
	return hub.CreateOptions(ctx, sock, providersClient)
}

// writeHubOptions writes the hub's result raw as craze providers --hub's
// table (runProvidersHub).
func writeHubOptions(out io.Writer, raw json.RawMessage) error {
	var res protocol.CreateOptionsResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	if err := writeProvidersTable(out, res.Providers, psLine); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "default: %s\n", cmp.Or(psLine(res.DefaultProvider), psNone)); err != nil {
		return err
	}
	if len(res.RecentDirs) == 0 {
		_, err := fmt.Fprintf(out, "recent: %s\n", psNone)
		return err
	}
	for _, d := range res.RecentDirs {
		if _, err := fmt.Fprintf(out, "recent: %s\n", psLine(d.Dir)); err != nil {
			return err
		}
	}
	return nil
}
