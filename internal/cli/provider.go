package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/tui"
)

const envProvider = "CRAZE_PROVIDER"

// providerIDs names every registered provider for the flag usage and the
// unknown-provider error. Computed so a new provider cannot be announced in
// one message and forgotten in the other.
var providerIDs = joinOr(agent.ProviderNames())

// joinOr joins names as a choice rather than a list: "" for none, the bare
// name for one, "a or b" (no comma) for two, and an Oxford "a, b, or c" for
// three or more.
func joinOr(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " or " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
	}
}

type resolvedProvider struct {
	Provider agent.Provider
	Locked   bool
	Fallback bool
}

func registerProviderFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "provider", "", "ACP provider: "+providerIDs)
}

func resolveProvider(cmd *cobra.Command, flag string, stderr io.Writer, hermetic bool) (resolvedProvider, error) {
	locked := hermetic
	if cmd != nil && cmd.Flags().Changed("provider") {
		locked = true
		if id := strings.TrimSpace(flag); id != "" {
			p, err := agent.ProviderByName(id)
			if err != nil {
				return resolvedProvider{}, usagef("craze: unknown provider %q (want %s)", id, providerIDs)
			}
			return resolvedProvider{Provider: p, Locked: true}, nil
		}
	}
	if hermetic {
		return resolvedProvider{Provider: agent.CursorProvider(), Locked: true}, nil
	}
	if env := strings.TrimSpace(os.Getenv(envProvider)); env != "" {
		p, err := agent.ProviderByName(env)
		if err != nil {
			warnUnknownProvider(stderr, env)
			return resolvedProvider{Provider: agent.CursorProvider(), Locked: locked, Fallback: true}, nil
		}
		return resolvedProvider{Provider: p, Locked: locked}, nil
	}
	if cfg := strings.TrimSpace(tui.ConfigProvider()); cfg != "" {
		p, err := agent.ProviderByName(cfg)
		if err != nil {
			warnUnknownProvider(stderr, cfg)
			return resolvedProvider{Provider: agent.CursorProvider(), Locked: locked, Fallback: true}, nil
		}
		return resolvedProvider{Provider: p, Locked: locked}, nil
	}
	return resolvedProvider{Provider: agent.CursorProvider(), Locked: locked}, nil
}

// knownProvider is the session index's view of the registry: a row whose
// provider id this build does not know is kept in the file but never offered
// (§3.2), because craze has no way to start it.
func knownProvider(id string) bool {
	_, err := agent.ProviderByName(id)
	return err == nil
}

// providerFlagExplicit is "--provider was passed, and named something". It is
// what filters the session index and what lets a load still write the
// persisted default — both are about a choice the user made on this command
// line, which is why an id out of the environment or the config file does not
// count (§3.1).
func providerFlagExplicit(cmd *cobra.Command, flag string) bool {
	return cmd != nil && cmd.Flags().Changed("provider") && strings.TrimSpace(flag) != ""
}

func warnUnknownProvider(w io.Writer, id string) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "craze: unknown provider %q, using cursor\n", id)
}

func persistProvider(r resolvedProvider) error {
	if r.Fallback {
		return nil
	}
	return tui.SaveProvider(r.Provider.Name())
}
