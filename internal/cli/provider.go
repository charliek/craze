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

const (
	envProvider = "CRAZE_PROVIDER"
	providerIDs = "cursor or grok"
)

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
