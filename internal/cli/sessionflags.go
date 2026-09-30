package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// The session flags (plan 030 §3.3): the command line of a session, which two
// commands take — the root command, whose TUI hosts its session in process
// today, and craze serve, the headless host — registered once for both
// (registerSessionFlags) and settled, refused and persisted by the same
// functions on both, so a flag cannot mean one thing to the TUI and another to
// the host it is handed to:
//
//	--workspace --provider --model --agent-bin --force/--no-force --ask
//	--plan --plugin-dir --continue
//
// The TUI's own flags (--theme, --no-mouse, --no-background,
// --no-host-status, --resume) stay on the root: a headless host draws nothing
// and reports to no terminal multiplexer, and --resume is a picker. craze
// serve's own are --load and --log (serve_cmd.go).
//
// Their values live in tuiFlags, whose name is older than the second command:
// a serve command line is a tuiFlags whose TUI-only members were never
// registered and so stay zero (serveFlags).

// registerSessionFlags declares the session flags on cmd, into f: the one
// registration both commands make (registerTUIFlags, registerServeFlags), so
// each flag's name, default and usage are spelled once.
func registerSessionFlags(cmd *cobra.Command, f *tuiFlags) {
	cmd.Flags().StringVar(&f.workspace, "workspace", "", "existing workspace directory (default: current directory)")
	// Worded without the provider's name: the root's help names native once,
	// in --provider (TestRootHelpNamesNativeOnceAndNeverTheHarness).
	cmd.Flags().StringVar(&f.model, "model", "", "model to start on: an ACP model id, or a model alias")
	cmd.Flags().StringVar(&f.agentBin, "agent-bin", "", "path to cursor-agent / fake agent (or CRAZE_AGENT_BIN)")
	registerPluginDirFlag(cmd, &f.pluginDirs)
	cmd.Flags().BoolVar(&f.force, "force", true, "spawn the agent with --force (yolo)")
	cmd.Flags().BoolVar(&f.noForce, "no-force", false, "disable yolo and handle permission requests")
	cmd.Flags().BoolVar(&f.ask, "ask", false, "set session mode to ask after session/new")
	cmd.Flags().BoolVar(&f.plan, "plan", false, "set session mode to plan after session/new")
	cmd.Flags().BoolVarP(&f.cont, "continue", "c", false, "load the newest session in this workspace instead of starting a new one")
	registerProviderFlag(cmd, &f.provider)
}

// settle is the session flags' own rules, before anything reads them: --ask
// and --plan are mutually exclusive — a usage error, worded as the root has
// always worded it — and --no-force turns --force off.
func (f *tuiFlags) settle() error {
	if f.ask && f.plan {
		return usagef("craze: --ask and --plan are mutually exclusive")
	}
	if f.noForce {
		f.force = false
	}
	return nil
}

// agentEnv is the agent child's environment for this command line, and the
// hosts it was settled against (plan 015 §3.4): every host whose gate is met
// (resolveHosts) and the environment less each such host's hook gate
// (hostSet.childEnv) — nil, inheriting craze's own, when none is. It is the one
// function both commands that start a session take it from (plan 030 §3.3): the
// TUI reports to those hosts itself, and craze serve reports to none, but a
// served session's agent is the launching TUI's session all the same, and its
// installed hooks must not fight that TUI for the pane or tab it reports to.
func agentEnv(f *tuiFlags, env hostEnv) (hostSet, []string) {
	hosts := resolveHosts(f, env)
	return hosts, hosts.childEnv(env.list())
}

// persistsProvider is whether a run may write the provider it starts as the
// default for the next plain craze (§3.1): a new session may, and a load only
// when --provider named its provider on this command line — continuing a grok
// thread is not a decision about tomorrow's default. The TUI hands the answer to
// its model (Config.PersistProvider), which writes on startedMsg; craze serve
// writes after its own Start (persistProvider), which skips a fallback and a
// hidden provider exactly as the model does.
func persistsProvider(cmd *cobra.Command, f *tuiFlags, load bool) bool {
	return !load || providerFlagExplicit(cmd, f.provider)
}

// continueRow is --continue's row, on both commands: the newest session in
// cwd — the absolute workspace, as the index keys it — of the provider filter
// names, "" for any (only an explicit --provider filters, §3.1). None is exit
// 1, and so is an index craze cannot read, which is never rewritten by the
// attempt (§3.8).
func continueRow(cwd, filter string) (sessions.Row, error) {
	index := &sessions.Store{KnownProvider: knownProvider}
	row, ok, err := index.Latest(cwd, filter)
	if err != nil {
		return sessions.Row{}, exitf(1, "%s: %v", noSessionMsg(cwd, filter), err)
	}
	if !ok {
		return sessions.Row{}, exitf(1, "%s", noSessionMsg(cwd, filter))
	}
	return row, nil
}

// claimLoad is a found row taken for this run, the half of a load both
// commands share (plan 027 §3.9, plan 028 §3.5, plan 030 §3.3): --continue on
// either, and craze serve's --load. The row's provider wins — the index says
// which agent wrote the session and only that one is trusted to load it,
// whatever was resolved — and is held to the spawn flags (refuse); the row is
// claimed (sessionClaims.claimRow), given its durable craze id under the
// index's own lock first when it has none.
//
// The order is the refusal's: a legacy row (no craze id) is held to the spawn
// flags BEFORE the claim, whose EnsureCrazeID would mint its id and write the
// index — a load the command line can never start leaves no trace (seam 6). A
// row with an id is claimed first (SQ16, PR 4): one another craze holds is
// reported held whatever the flags say — an attach takes none of them — and
// only one this run now holds is held to them, its claim given back when they
// refuse it.
//
// It answers the provider and the row carrying the craze id just claimed —
// empty only when no id could be written, and then the engine mints one that
// the row gains on its next write. A claim refused is exit 1 with refusal's
// words, carrying claimRow's error: a *rundir.HeldError is a session another
// craze holds, which each caller turns into its own answer (the TUI attaches
// to it, attachHeld; craze serve exits with the holder). Nothing is built
// here, so no agent is spawned for a load that is refused.
func claimLoad(row sessions.Row, refuse func(agent.Provider) error, claims *sessionClaims) (agent.Provider, sessions.Row, error) {
	// Reading the registry cannot fail here — the store filters out a provider
	// this build does not know — but a row is user-editable JSON, so the
	// refusal is spelled rather than assumed.
	p, err := agent.ProviderByName(row.Provider)
	if err != nil {
		return agent.Provider{}, sessions.Row{}, exitf(1, "craze: %v", err)
	}
	legacy := row.CrazeID == ""
	if legacy {
		if err := refuse(p); err != nil {
			return agent.Provider{}, sessions.Row{}, err
		}
	}
	// Claimed before anything is built, which has no error return and would
	// otherwise have to hand back a session that must never start.
	crazeID, release, err := claims.claimRow(row)
	if err != nil {
		return agent.Provider{}, sessions.Row{}, &exitError{code: 1, msg: "craze: " + refusal(err), cause: err}
	}
	if !legacy {
		if err := refuse(p); err != nil {
			release()
			return agent.Provider{}, sessions.Row{}, err
		}
	}
	// The row's durable craze id travels with the session built from it: this
	// is the same thread of work, loaded into another agent session (session
	// control SD-22).
	row.CrazeID = crazeID
	return p, row, nil
}

// heldBy is the session another craze holds that err refuses a load for, or
// nil: claimLoad's refusal carries the claim's *rundir.HeldError.
func heldBy(err error) *rundir.HeldError {
	var held *rundir.HeldError
	if errors.As(err, &held) {
		return held
	}
	return nil
}
