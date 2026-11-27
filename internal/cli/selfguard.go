package cli

import (
	"os"
	"strings"

	"github.com/charliek/craze/internal/agent"
)

// The self-delegation guard (plan 052 § J2): craze new refuses a session on
// the very provider the agent running it is on — Claude Code delegating to
// claude, a cursor session to cursor — which that agent's own sub-agent tool
// does better and which would only nest it inside itself. It is a usage
// error, exit 2 (the usage object under --json), and --allow-same-provider
// passes it. craze send, wait and cancel are not guarded: they continue a
// session already started.
//
// The caller is read from its markers (callerProvider), and the target is
// what each path of craze new would start (runNew, runNewWait, runNewLoad):
// --provider; else plain new's hub default, --wait's config.toml default, or
// the saved row's provider for --load.

const (
	// allowSameProviderFlag is the guard's override.
	allowSameProviderFlag = "allow-same-provider"
	// claudeCodeEnv is set to 1 by Claude Code in its Bash tool's commands.
	claudeCodeEnv = "CLAUDECODE"
	// cursorAgentEnv is set by a standalone cursor-agent in its shell tool's
	// commands (plan 052 L2: with CURSOR_INVOKED_AS, and per-run ids beside).
	cursorAgentEnv = "CURSOR_AGENT"
	// antigravityAgentEnv is set to 1 by agy (Antigravity) in its run_command
	// shell (plan 053 E1).
	antigravityAgentEnv = "ANTIGRAVITY_AGENT"
	// codexThreadEnv is set by Codex CLI in its commandExecution shell
	// (plan 054 E1).
	codexThreadEnv = "CODEX_THREAD_ID"
)

// callerProvider is the provider the agent that ran this craze is on, by
// getenv, or "" when it cannot tell. CRAZE_SESSION_PROVIDER comes first: craze
// sets it fresh for each agent it spawns, so it names the immediate caller
// (a cursor session Claude Code started has CLAUDECODE too, and may target
// claude). Without it, claude when CLAUDECODE=1, cursor when CURSOR_AGENT is
// set, antigravity when ANTIGRAVITY_AGENT is set, codex when CODEX_THREAD_ID
// is set — but only when exactly one of them is: each inherits the others'
// environment when one launches another, so with two or more the immediate
// caller is unknown, and the guard stands aside (the plugin's text rule still
// holds). grok sets no marker craze trusts, so its rule is the text alone.
func callerProvider(getenv func(string) string) string {
	if p := strings.TrimSpace(getenv(agent.SessionProviderEnv)); p != "" {
		return p
	}
	var named []string
	if getenv(claudeCodeEnv) == "1" {
		named = append(named, agent.ClaudeProvider().Name())
	}
	if getenv(cursorAgentEnv) != "" {
		named = append(named, agent.CursorProvider().Name())
	}
	if getenv(antigravityAgentEnv) != "" {
		named = append(named, agent.AntigravityProvider().Name())
	}
	if getenv(codexThreadEnv) != "" {
		named = append(named, agent.CodexProvider().Name())
	}
	if len(named) == 1 {
		return named[0]
	}
	return ""
}

// selfDelegation is the guard's verdict on starting target, a provider id,
// from this process: a usage error when it is the caller's own provider, and
// nil otherwise — when allow is set (--allow-same-provider), when no caller is
// known, and when target is "" or no provider craze knows (that is the
// path's own error, later).
func selfDelegation(target string, allow bool) error {
	if allow || target == "" {
		return nil
	}
	if _, err := agent.ProviderByName(target); err != nil {
		return nil
	}
	if callerProvider(os.Getenv) != target {
		return nil
	}
	return usagef("craze: this agent already runs on %s; use your own sub-agent tool for %s work, not craze (--allow-same-provider overrides)", target, target)
}

// callerKnown says whether the guard has a caller to check at all, so a path
// whose target costs something to resolve (plain new's hub default) only
// resolves it when the answer can matter.
func callerKnown() bool { return callerProvider(os.Getenv) != "" }
