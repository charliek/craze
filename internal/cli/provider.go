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
	cmd.Flags().StringVar(dst, "provider", "", providerUsage(agent.Providers()))
}

// providerUsage is --provider's help: every listed provider, with the ACP
// agents craze spawns told apart from the one that runs inside it — "cursor,
// grok, gx (ACP agents) or native (runs inside craze)". Computed from the
// registry, as providerIDs is, so a new provider cannot be named in one and
// missing from the other. "Runs inside craze" is the refusal's own wording
// (refuseInProcess) and not "harness", an implementation word the root --help
// must not carry (TestImportGxHelpNeverMentionsTheHarness).
func providerUsage(ps []agent.Provider) string {
	var acp, own []string
	for _, p := range ps {
		if p.InProcess() {
			own = append(own, p.Name())
		} else {
			acp = append(acp, p.Name())
		}
	}
	var groups []string
	if len(acp) > 0 {
		groups = append(groups, strings.Join(acp, ", ")+" (ACP agents)")
	}
	if len(own) > 0 {
		groups = append(groups, strings.Join(own, ", ")+" (runs inside craze)")
	}
	return "provider: " + joinOr(groups)
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

// envAgentBin is the environment's --agent-bin, read by acp's binary lookup.
const envAgentBin = "CRAZE_AGENT_BIN"

// refuseInProcess is the usage error for asking an in-process provider for
// something it has not got (plan 018 §3.4): an agent binary, from --agent-bin
// or CRAZE_AGENT_BIN, has nothing to be spawned as, and a mode (--ask or
// --plan, as mode) needs modes to map onto — silently ignoring a requested plan
// mode would be worse than refusing it. cmd is the command's name as its other
// usage errors spell it. It is nil for every provider craze spawns.
//
// The mode half is keyed on the capability rather than on "in-process", which
// is what lets native take --plan and --ask from H5 on while an in-process
// provider without modes is still refused (plan 023 §3.6). One function covers
// all three callers — prompt, a fresh TUI session and frame, which has no mode
// flag of its own and passes "" — so `craze --provider native --plan` switches
// plan mode on in the TUI path too. A load asks it of the loaded row's own
// provider, whatever was resolved: --continue of its row, and the resume
// picker of the row chosen, both before anything is claimed (plan 028 §3.5).
// The provider picker asks it of the provider chosen, before that is started
// or persisted (C19a): the resolved default is not the only one it can start.
//
// The environment variable counts exactly as acp reads it — set and
// non-empty — so a run the refusal lets through could never have spawned that
// binary either.
func refuseInProcess(cmd string, p agent.Provider, agentBin, mode string) error {
	if !p.InProcess() {
		return nil
	}
	switch {
	case agentBin != "":
		return usagef("%s: --agent-bin cannot be used with provider %s, which runs inside craze", cmd, p.Name())
	case os.Getenv(envAgentBin) != "":
		return usagef("%s: %s cannot be used with provider %s, which runs inside craze; unset it", cmd, envAgentBin, p.Name())
	case mode != "" && !p.Capabilities().Modes:
		return usagef("%s: --%s cannot be used with provider %s, which has no modes", cmd, mode, p.Name())
	}
	return nil
}

// knownProvider is the session index's view of the registry: a row whose
// provider id this build does not know is kept in the file but never offered
// (§3.2), because craze has no way to start it. A provider that is not
// resumable counts as unknown here (plan 028 §3.5): craze cannot load its
// sessions, so a row naming one — which craze never writes, but the index is
// user-editable JSON — is kept and never offered by --continue or --resume
// either. Hidden is not the question: before D-65 listed it, native was
// hidden and its rows still loaded.
func knownProvider(id string) bool {
	p, err := agent.ProviderByName(id)
	return err == nil && p.Resumable()
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

// persistProvider writes the provider craze prompt just started as the default
// for the next plain craze. A fallback is not a choice, so it is not written;
// nor is a hidden provider, so trying one once never changes what a plain
// craze starts (plan 018 §3.4). The TUI's own write on startedMsg skips it the
// same way.
func persistProvider(r resolvedProvider) error {
	if r.Fallback || r.Provider.Hidden() {
		return nil
	}
	return tui.SaveProvider(r.Provider.Name())
}
