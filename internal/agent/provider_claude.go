package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness/tool"
)

// The claude provider (plan 052 § A): claude-agent-acp, the ACP project's
// bridge to the user's own signed-in Claude Code, on their Claude plan. craze
// spawns the adapter, which spawns `claude`; craze holds no credential and
// reads nothing under ~/.claude. What is claude's alone lives here: its
// tables, the environment it is spawned with (spawnEnv), the companion binary
// it needs beside the adapter (CompanionBins), the checks made before the
// spawn (preflight) and the login gate after initialize (claudeLoginRefusal).

const (
	claudeName = "claude"
	// claudeAdapterBin is the adapter's executable, looked up on PATH like
	// cursor-agent and grok; [agents].claude, --agent-bin and
	// $CRAZE_AGENT_BIN point at another.
	claudeAdapterBin = "claude-agent-acp"
	// claudeCompanionBin is Claude Code itself, which the adapter runs.
	claudeCompanionBin = "claude"
	// ClaudeExecutableEnv names the `claude` the adapter runs
	// (claude-cli.js:19-27). craze resolves the companion from it first, then
	// from PATH, and always sets it in the adapter's environment to the path
	// it resolved, so the preflight checks the very binary the adapter runs.
	ClaudeExecutableEnv = "CLAUDE_CODE_EXECUTABLE"
	// SessionProviderEnv is the caller marker (plan 052 § J1): every agent a
	// craze session starts gets it, set to the session's provider id.
	// Native's shell carries it too (tool.WithSessionProvider).
	SessionProviderEnv = tool.SessionProviderEnv
	// claudeLoginHint is what an auth error tells the user to run.
	claudeLoginHint = "claude auth login"
	// claudeAdapterInstall is the adapter's install command (Node 22 or newer);
	// AdapterInstall hands it to the availability check.
	claudeAdapterInstall = "npm i -g @agentclientprotocol/claude-agent-acp"

	// minAdapterVersion is the oldest adapter craze runs: 0.107.0 brought the
	// rate-limit data on errors and the unknown child state. minClaudeVersion
	// is the oldest Claude Code craze has run through it (discovery CL0). The
	// versions tested are recorded in the docs, and not enforced.
	minAdapterVersion = "0.107.0"
	minClaudeVersion  = "2.1.322"
)

// claudeModeKinds is the adapter's modes (plan 052 § C1): Manual (default),
// accept edits, auto and bypass permissions all implement; plan plans.
// Nothing is read-only, so /ask and --ask find no mode to resolve to.
var claudeModeKinds = map[string]ModeKind{
	"default":           ModeImplement,
	"acceptEdits":       ModeImplement,
	"auto":              ModeImplement,
	"bypassPermissions": ModeImplement,
	"plan":              ModePlan,
}

// claudeCycleModes is claude's Shift+Tab cycle (§ C1): Manual, accept edits,
// plan. auto and bypassPermissions are reachable by name only — a key that
// lands on a mode which skips the user's own deny rules is one keypress too
// easy.
var claudeCycleModes = []string{"default", "acceptEdits", "plan"}

// claudeStartModes are the modes a claude session may be started or loaded
// in: Manual, or plan under --plan (§ C2).
var claudeStartModes = []string{"default", "plan"}

// ClaudeProvider is Claude Code through claude-agent-acp (plan 052 § A1),
// listed after grok since its overage guard landed (C5). It is not optional:
// a missing binary — the adapter or `claude` — shows it unavailable. Its
// capabilities: C3's cards (the question card from elicitation/create, the
// plan card from a plan exit), modes, effort, todos, the parameterized picker
// and the interjection (_session/steering); C4's sub-agent rows.
func ClaudeProvider() Provider {
	return Provider{
		name:        claudeName,
		displayName: claudeName,
		modeKinds:   claudeModeKinds,
		// No implement offer after a plan-mode turn: claude's plan is approved
		// on its own card (§ B5), and an empty prompt turns the offer off.
		implementPrompt: "",
		defaultBins:     []string{claudeAdapterBin},
		loginHint:       claudeLoginHint,
		resumable:       true,
		capabilities: Capabilities{
			Effort:              true,
			Modes:               true,
			Todos:               true,
			AskCards:            true,
			PlanCards:           true,
			ParameterizedPicker: true,
			Interject:           true,
			// C4 (plan 052 § E3): a row per child from subagent_update, its
			// own transcript, and its own stop where the child grants cancel.
			// Background sub-agents stay native's presentation.
			SubagentRows:       true,
			SubagentTranscript: true,
			SubagentCancel:     true,
		},
		dialect:             acp.DialectClaude,
		planUpdatesAreTodos: true,
		// Used only when the session declares no sub-agent capability (§ E3).
		subagentToolNames: []string{"Agent", "Task"},
	}
}

// isClaude reports whether p is the claude provider.
func (p Provider) isClaude() bool { return p.name == claudeName }

// CycleModes is the subset of advertised modes Shift+Tab moves through, in
// the agent's order; nil means every advertised mode (§ C1).
func (p Provider) CycleModes() []string {
	if p.isClaude() {
		return append([]string(nil), claudeCycleModes...)
	}
	return nil
}

// RecordsModel reports whether p's session index rows carry the session's
// model, which a load sends back (plan 052 § C4). Only claude's: the adapter's
// own choice on a load (ANTHROPIC_MODEL, then the settings, then the
// transcript) would otherwise beat the model the user picked in craze.
func (p Provider) RecordsModel() bool { return p.isClaude() }

// RefusesFast reports whether p refuses its fast setting being turned on (plan
// 052 § D3): claude's fast mode bills as extra usage on a Claude plan.
func (p Provider) RefusesFast() bool { return p.isClaude() }

// FastBillsExtra is why claude refuses fast mode.
const FastBillsExtra = "fast mode bills extra usage on a Claude plan"

// ConfigHidden reports whether a picker leaves opt out for p (plan 052 §
// D3): claude's fast option, which craze keeps off.
//
// It runs on every render of the model dialog, so the name — all RefusesFast
// reads — is checked before the provider is rebuilt.
func (p ProviderInfo) ConfigHidden(opt ConfigOption) bool {
	if p.Name != claudeName {
		return false
	}
	prov := p.provider()
	return prov.RefusesFast() && prov.isFastOption(opt)
}

// CompanionBins is what p needs beside its agent binary (plan 052 § A2):
// claude's `claude`, nil for every other provider.
func (p Provider) CompanionBins() []string {
	if p.isClaude() {
		return []string{claudeCompanionBin}
	}
	return nil
}

// AdapterInstall is the command that installs p's agent when it is a package
// with its own install (claude's npm adapter), "" for every other provider.
func (p Provider) AdapterInstall() string {
	if p.isClaude() {
		return claudeAdapterInstall
	}
	return ""
}

// ResolveCompanion is p's companion binary as a session resolves it, getenv
// being the environment the agent will see: $CLAUDE_CODE_EXECUTABLE when it
// is set, which wins, else the companion's PATH lookup. --agent-bin,
// $CRAZE_AGENT_BIN and [agents].claude are the adapter's alone. The error
// names what was looked for; a provider with no companion resolves to "".
// The path is absolute: the preflight runs `claude` in a temporary directory
// of its own, where a relative spelling names nothing (review F4).
func (p Provider) ResolveCompanion(getenv func(string) string) (string, error) {
	path, err := p.resolveCompanion(getenv)
	if err != nil || path == "" {
		return path, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return abs, nil
}

func (p Provider) resolveCompanion(getenv func(string) string) (string, error) {
	bins := p.CompanionBins()
	if len(bins) == 0 {
		return "", nil
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if v := strings.TrimSpace(getenv(ClaudeExecutableEnv)); v != "" {
		path, err := acp.LookupBinary(v, nil)
		if err != nil {
			return "", fmt.Errorf("%s %s not found; install Claude Code (claude), or point %s at it", ClaudeExecutableEnv, v, ClaudeExecutableEnv)
		}
		return path, nil
	}
	path, err := acp.LookupBinary("", bins)
	if err != nil {
		return "", fmt.Errorf("%s not found on PATH; install Claude Code (claude), or set %s", bins[0], ClaudeExecutableEnv)
	}
	return path, nil
}

// claudeStrippedEnv is every variable spawnEnv removes from a claude
// session's environment (plan 052 § A3, CLD-09), by its exact name. It is the
// process environment alone: a Claude settings.json env block, managed policy
// or an apiKeyHelper can still route billing, which the login gate and the
// overage guard catch.
var claudeStrippedEnv = []string{
	// The identity of a Claude Code session that launched craze: a claude
	// spawned with these acts as part of that session. Ported from t3code's
	// CLAUDE_HOST_SESSION_VARIABLES and its Electron variable
	// (apps/server/src/provider/Drivers/ClaudeHome.ts:48-81), plus the
	// entrypoint, which craze leaves Claude Code to set.
	"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_HOST_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN",
	"CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_DESKTOP_APP_VERSION", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_EFFORT",
	"CLAUDE_PID", "CLAUDE_AGENT_SDK_VERSION", "ELECTRON_RUN_AS_NODE",
	// Credentials, clouds and gateways: anything that would bill a turn to
	// something other than the user's Claude plan (pandacode claude_auth.rs:22-30
	// informed the list; the adapter's providers.js:175-191 names the rest).
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"ANTHROPIC_BEDROCK_BASE_URL", "ANTHROPIC_VERTEX_BASE_URL", "ANTHROPIC_FOUNDRY_BASE_URL",
	"ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_PROFILE", "ANTHROPIC_FEDERATION_RULE_ID",
	"ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE",
	// Model pins: craze owns the model, and ANTHROPIC_MODEL would beat the
	// session's own on a load.
	"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_SMALL_FAST_MODEL",
	"ANTHROPIC_CUSTOM_MODEL_OPTION", "CLAUDE_MODEL_CONFIG", "CLAUDE_CODE_SUBAGENT_MODEL",
}

// claudeStripped reports whether spawnEnv removes the variable named k from a
// claude session: one of claudeStrippedEnv, or any
// CLAUDE_CODE_*_FILE_DESCRIPTOR (a credential handed over by descriptor).
func claudeStripped(k string) bool {
	return (strings.HasPrefix(k, "CLAUDE_CODE_") && strings.HasSuffix(k, "_FILE_DESCRIPTOR")) ||
		slices.Contains(claudeStrippedEnv, k)
}

// spawnEnv is the environment p's agent is spawned with, from base, the one
// the session was given (plan 052 § A3): nil is the parent's own, expanded
// first, so what is removed and added applies to an inherited environment
// too. Every ACP provider's agent gets the caller marker; claude's loses the
// strip list and gets the adapter's protocol, the companion it runs and the
// marker. Duplicate keys collapse to the last, in the last one's place, as
// exec does with a child's environment — so the variables set here win over
// any the base carried.
//
// dir is the directory the agent is spawned in. For an inherited environment
// PWD is set to it, as exec does for a child given none of its own (an
// explicit Env is left as it is, by the same rule): without it the agent
// would see the parent's PWD, and resolve its working directory through
// symlinks where it used to keep the spelling it was given.
func (p Provider) spawnEnv(base []string, companion, dir string) []string {
	if base == nil {
		base = os.Environ()
		if dir != "" {
			if abs, err := filepath.Abs(dir); err == nil {
				base = append(base, "PWD="+abs)
			}
		}
	}
	out := make([]string, 0, len(base)+3)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if p.isClaude() && claudeStripped(k) {
			continue
		}
		out = append(out, kv)
	}
	if p.isClaude() {
		out = append(out, "CLAUDE_AGENT_ACP_PROTOCOL=v1", ClaudeExecutableEnv+"="+companion)
	}
	out = append(out, SessionProviderEnv+"="+p.name)
	return dedupeEnv(out)
}

// dedupeEnv keeps the last entry of each key, in that entry's position. An
// entry with no "=" is kept as it is.
func dedupeEnv(env []string) []string {
	last := make(map[string]int, len(env))
	for i, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok {
			last[k] = i
		}
	}
	out := make([]string, 0, len(last))
	for i, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if ok && last[k] != i {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// claudeStartError is a claude start refused by craze's own checks, with the
// kind it is (plan 052 § A4's failure table).
type claudeStartError struct {
	kind errkind.Kind
	msg  string
}

func (e *claudeStartError) Error() string           { return "agent: " + e.msg }
func (e *claudeStartError) ErrorInfo() errkind.Info { return errkind.New(e.kind) }

func claudeErr(kind errkind.Kind, format string, args ...any) error {
	return &claudeStartError{kind: kind, msg: fmt.Sprintf(format, args...)}
}

// PreflightInfo is what the preflight learned that a session shows: the plan
// the saved login is on (the status row's, plan 052 § D2).
type PreflightInfo struct {
	SubscriptionType string
}

// The preflight's bounds (plan 052 § A4): each command has its own, inside
// one for all three, and reads at most preflightOutputCap of stdout. Atomic
// for the tests that shorten them under -race.
var (
	preflightCmdWait   atomic.Int64
	preflightTotalWait atomic.Int64
)

const (
	defaultPreflightCmdWait   = 5 * time.Second
	defaultPreflightTotalWait = 10 * time.Second
	preflightOutputCap        = 32 << 10
)

func durationOr(v *atomic.Int64, def time.Duration) time.Duration {
	if d := v.Load(); d > 0 {
		return time.Duration(d)
	}
	return def
}

// preflightRun is one preflight command's outcome.
type preflightRun struct {
	out      []byte
	overflow bool
	err      error
	timedOut bool
}

// preflight runs claude's three checks before the spawn (plan 052 § A4), at
// the exact paths the spawn uses — adapter the adapter, companion the claude
// it will run — under env, the environment the adapter is spawned with, and
// concurrently: `claude --setting-sources "" auth status --json` in a fresh
// temporary directory, so no project's settings or helpers run (pandacode's
// claude_auth.rs:122-133 runs it the same way); `claude-agent-acp --version`
// in the session's directory cwd, where a per-project Node pin could break
// its shim; and `claude --version`. The saved login must be a claude.ai
// subscription that is not enterprise, and both versions at least their
// minimum. Any other provider has nothing to check.
func (p Provider) preflight(ctx context.Context, adapter, companion string, env []string, cwd string) (PreflightInfo, error) {
	if !p.isClaude() {
		return PreflightInfo{}, nil
	}
	// The overage guard's trip record first (plan 052 § D3): while it holds,
	// no claude session starts on this machine.
	if err := overageRefusal(); err != nil {
		return PreflightInfo{}, err
	}
	total := durationOr(&preflightTotalWait, defaultPreflightTotalWait)
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	tmp, err := os.MkdirTemp("", "craze-claude-preflight-")
	if err != nil {
		return PreflightInfo{}, claudeErr(errkind.StartFailed, "the claude preflight could not make a directory: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	var (
		wg                   sync.WaitGroup
		auth, adapterV, cliV preflightRun
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		auth = runPreflight(ctx, companion, tmp, env, "--setting-sources", "", "auth", "status", "--json")
	}()
	go func() { defer wg.Done(); adapterV = runPreflight(ctx, adapter, cwd, env, "--version") }()
	go func() { defer wg.Done(); cliV = runPreflight(ctx, companion, tmp, env, "--version") }()
	wg.Wait()
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		// The caller gave up: its own reason, not a preflight failure.
		return PreflightInfo{}, err
	}
	info, err := judgeAuthStatus(auth)
	if err != nil {
		return PreflightInfo{}, err
	}
	if err := judgeVersion(adapterV, claudeAdapterBin+" --version", claudeAdapterBin, minAdapterVersion, "run `"+claudeAdapterInstall+"`"); err != nil {
		return PreflightInfo{}, err
	}
	if err := judgeVersion(cliV, claudeCompanionBin+" --version", claudeCompanionBin, minClaudeVersion, "run `claude update`"); err != nil {
		return PreflightInfo{}, err
	}
	return info, nil
}

// runPreflight runs one preflight command in its own process group, which is
// killed whatever happens, bounded by the per-command wait inside ctx, with
// stdout read up to preflightOutputCap and stderr dropped.
func runPreflight(ctx context.Context, path, dir string, env []string, args ...string) preflightRun {
	ctx, cancel := context.WithTimeout(ctx, durationOr(&preflightCmdWait, defaultPreflightCmdWait))
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	// A grandchild holding stdout open must not hold the wait past the bound.
	cmd.WaitDelay = 500 * time.Millisecond
	out := &cappedWriter{limit: preflightOutputCap}
	cmd.Stdout = out
	err := cmd.Run()
	// Whatever the command left in its group goes with it.
	_ = killGroup(cmd)
	return preflightRun{out: out.buf, overflow: out.overflow, err: err, timedOut: ctx.Err() != nil}
}

// killGroup SIGKILLs the process group cmd leads, if it was started.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// claudeAuthStatus is the part of `claude auth status --json` the preflight
// judges. Nothing else of it — the email, the organisation — is read.
type claudeAuthStatus struct {
	LoggedIn         *bool  `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	APIProvider      string `json:"apiProvider"`
	SubscriptionType string `json:"subscriptionType"`
	APIKeySource     string `json:"apiKeySource"`
}

// judgeAuthStatus is the saved login's verdict (§ A4): signed in, through
// claude.ai, first party, on a subscription that is not enterprise, with no
// API key beside it. An oauth_token login cannot come from the stripped
// environment, so one reported came from settings, and is refused like any
// other kind.
func judgeAuthStatus(r preflightRun) (PreflightInfo, error) {
	const cmd = `claude auth status --json`
	if r.timedOut {
		return PreflightInfo{}, claudeErr(errkind.StartFailed, "`%s` did not answer in time", cmd)
	}
	var st claudeAuthStatus
	if r.overflow || json.Unmarshal(bytes.TrimSpace(r.out), &st) != nil || st.LoggedIn == nil {
		return PreflightInfo{}, claudeErr(errkind.StartFailed, "`%s` printed nothing craze could read", cmd)
	}
	if !*st.LoggedIn {
		return PreflightInfo{}, claudeErr(errkind.Auth, "Claude Code is not signed in (run `%s`)", claudeLoginHint)
	}
	plan := strings.TrimSpace(st.SubscriptionType)
	switch {
	case st.AuthMethod != "claude.ai", st.APIProvider != "firstParty", st.APIKeySource != "",
		plan == "", isEnterprisePlan(plan):
		return PreflightInfo{}, claudeErr(errkind.Auth, "craze runs claude on a Claude Pro or Max plan only, and this login is %s; sign in to one with `%s`",
			describeLogin(st), claudeLoginHint)
	}
	return PreflightInfo{SubscriptionType: plan}, nil
}

// describeLogin names what the saved login is, from the fields that decided
// against it.
func describeLogin(st claudeAuthStatus) string {
	var parts []string
	if st.AuthMethod != "" {
		parts = append(parts, "auth method "+st.AuthMethod)
	}
	if st.APIProvider != "" && st.APIProvider != "firstParty" {
		parts = append(parts, "provider "+st.APIProvider)
	}
	if st.APIKeySource != "" {
		parts = append(parts, "an API key from "+st.APIKeySource)
	}
	if plan := strings.TrimSpace(st.SubscriptionType); plan != "" {
		parts = append(parts, "plan "+plan)
	} else {
		parts = append(parts, "no plan")
	}
	return sanitizeLine(strings.Join(parts, ", "))
}

// isEnterprisePlan reports whether a plan name is an enterprise seat, which
// can be usage-billed. Any other name passes, so a new plan does not lock its
// users out.
func isEnterprisePlan(plan string) bool {
	return strings.Contains(strings.ToLower(plan), "enterprise")
}

// judgeVersion is one --version check: the first token of what bin printed,
// at least min. An answer that never came, or that is not a version, fails
// the start retryably; an old version is a config failure with the update
// command.
func judgeVersion(r preflightRun, cmd, bin, min, fix string) error {
	if r.timedOut {
		return claudeErr(errkind.StartFailed, "`%s` did not answer in time", cmd)
	}
	fields := strings.Fields(string(r.out))
	if r.err != nil || r.overflow || len(fields) == 0 {
		return claudeErr(errkind.StartFailed, "`%s` printed nothing craze could read", cmd)
	}
	got := strings.TrimPrefix(fields[0], "v")
	older, ok := versionOlder(got, min)
	if !ok {
		return claudeErr(errkind.StartFailed, "`%s` printed nothing craze could read", cmd)
	}
	if older {
		return claudeErr(errkind.Config, "%s %s is older than craze needs (%s): %s", bin, sanitizeLine(got), min, fix)
	}
	return nil
}

// versionOlder reports whether dotted version v is older than min, comparing
// the releases numerically part by part, and a prerelease of a release
// before the release itself; a "+build" suffix is dropped. ok is false when v
// is not a version.
func versionOlder(v, min string) (older, ok bool) {
	a, preA, okA := versionParts(v)
	b, preB, okB := versionParts(min)
	if !okA || !okB {
		return false, false
	}
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x < y, true
		}
	}
	// The same release: a prerelease of it comes before it, as semver orders
	// them (0.107.0-rc1 is older than 0.107.0; review F5).
	switch {
	case preA == preB, preA == "":
		return false, true
	case preB == "":
		return true, true
	}
	return preA < preB, true
}

// versionParts is v's numeric release, dotted, and its prerelease: what
// follows the first "-", up to any "+build", which says nothing of the order
// and is dropped. A dot inside the prerelease is the prerelease's own, never
// a release component.
func versionParts(v string) (release []int, pre string, ok bool) {
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v, pre = v[:i], v[i+1:]
	}
	segs := strings.Split(v, ".")
	if len(segs) < 2 {
		return nil, "", false
	}
	release = make([]int, 0, len(segs))
	for _, s := range segs {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return nil, "", false
		}
		release = append(release, n)
	}
	return release, pre, true
}

// loginStatusWait bounds the wait after initialize for the adapter's first
// _auth/status_update (plan 052 § A4, R2-1). Atomic for the tests that
// shorten it.
var loginStatusWait atomic.Int64

const defaultLoginStatusWait = 10 * time.Second

// claudeLoginRefusal is the effective-login gate's verdict on one
// _auth/status_update (§ A4): nil only for an account login on a named plan
// that is not enterprise and is not an OAuth token. Anything else — an API
// key from an apiKeyHelper, an external cloud, a gateway, no login — is an
// auth error naming what the adapter reported.
func claudeLoginRefusal(st acp.AuthStatus) error {
	plan := strings.TrimSpace(st.Plan)
	tokenLogin := strings.Contains(strings.ToLower(st.Detail), "oauth token")
	if st.Kind == acp.AuthKindAccount && plan != "" && !isEnterprisePlan(plan) && !tokenLogin {
		return nil
	}
	what := sanitizeLine(st.Label)
	if what == "" {
		what = "no login"
	}
	if d := sanitizeLine(st.Detail); d != "" {
		what += " (" + d + ")"
	}
	switch st.Kind {
	case acp.AuthKindNone:
		return claudeErr(errkind.Auth, "Claude Code is not signed in (run `%s`)", claudeLoginHint)
	case acp.AuthKindAPIKey:
		return claudeErr(errkind.Auth, "an API key pays for this session, not your Claude plan: %s; craze runs claude on a Claude Pro or Max plan only", what)
	case acp.AuthKindAccount:
		return claudeErr(errkind.Auth, "this Claude login is %s; craze runs claude on a Claude Pro or Max plan only (run `%s`)", what, claudeLoginHint)
	}
	return claudeErr(errkind.Auth, "Claude Code would bill this session to %s (%s); craze runs claude on a Claude Pro or Max plan only",
		sanitizeLine(st.Kind), what)
}

// errNoLoginStatus is the gate failing closed: the adapter never said which
// login pays (plan 052 R2-1).
func errNoLoginStatus() error {
	return claudeErr(errkind.StartFailed, "craze could not confirm which Claude login pays for this session")
}
