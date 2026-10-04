package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
)

// craze providers (plan 036 §3.2): the agent providers this craze could
// start a session of, from this process's own view — the availability check's
// TUI and CLI column (processAvailInputs), with no --agent-bin, over the
// listed providers (a missing gx is left out) — each with its state and, when
// it is not ready, why and what to do. It exits 0 whenever it answered,
// whatever the states say.

type providersFlags struct {
	json bool
}

func newProvidersCmd() *cobra.Command {
	f := &providersFlags{}
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "List the agent providers and whether each can start here",
		Long: "craze providers lists the agent providers craze can start a session of — cursor, grok, " +
			"gx when it is installed, and native — with each one's state on this machine: ready, " +
			"needs setup, or unavailable, and for the last two why and what to do about it. These are " +
			"agent providers; craze auth list shows the native provider's model providers.",
		// An argument is a usage error, exit 2, as craze auth list's is.
		Args: authArgs(0, "takes no arguments"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProviders(cmd, *f)
		},
	}
	// A flag it does not take is a usage error, exit 2, as an argument is.
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return usagef("%s: %v", cmd.CommandPath(), err)
	})
	cmd.Flags().BoolVar(&f.json, "json", false, "print the providers as JSON")
	return cmd
}

// providersJSON is craze providers --json: the providers, each as the wire's
// providerOption (sessions.createOptions, plan 036 §3.4) spells one.
type providersJSON struct {
	Providers []providerOptionJSON `json:"providers"`
}

// providerOptionJSON is one provider: its id, its label (DisplayName), its
// state in the wire's words, and its reason and fix, both absent when ready.
type providerOptionJSON struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Fix    string `json:"fix,omitempty"`
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
	avail := availability(processAvailInputs(launch.Provider, ""), agent.Providers())
	out := cmd.OutOrStdout()
	if f.json {
		doc := providersJSON{Providers: make([]providerOptionJSON, 0, len(avail))}
		for _, a := range avail {
			doc.Providers = append(doc.Providers, providerOptionJSON{
				ID: a.P.Name(), Label: a.P.DisplayName(), State: string(a.State), Reason: a.Reason, Fix: a.Fix,
			})
		}
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		err = enc.Encode(doc)
	} else {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, providersHeader)
		// A ready provider's reason and fix are empty cells, "-" (psNone).
		for _, a := range avail {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.P.Name(), a.State.human(), cmp.Or(a.Reason, psNone), cmp.Or(a.Fix, psNone))
		}
		err = tw.Flush()
	}
	if err != nil {
		return exitf(1, "craze providers: %v", err)
	}
	return nil
}
