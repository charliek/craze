package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/errkind"
)

// okStatus is a saved claude.ai Max login, `claude auth status --json`'s
// shape (no email or organisation: nothing of them is read).
const okStatus = `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max"}`

// fakeClaude writes a stand-in for Claude Code's `claude`: `auth status` prints
// status (after sleeping sleep seconds, when set) and records its argv and
// directory in <dir>/auth-args; `--version` prints version. It returns the
// script's path.
func fakeClaude(t *testing.T, status, version string, sleep string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "claude")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("case \"$*\" in\n")
	b.WriteString("  *'auth status'*)\n")
	fmt.Fprintf(&b, "    { for a in \"$@\"; do printf '[%%s]' \"$a\"; done; echo; pwd; } > %q\n", filepath.Join(dir, "auth-args"))
	if sleep != "" {
		fmt.Fprintf(&b, "    sleep %s\n", sleep)
	}
	fmt.Fprintf(&b, "    cat <<'EOF'\n%s\nEOF\n    ;;\n", status)
	fmt.Fprintf(&b, "  *--version*) echo %q ;;\n", version+" (Claude Code)")
	b.WriteString("esac\n")
	if err := os.WriteFile(p, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// shortClaudeWaits shortens the preflight's and the login gate's bounds for
// one test.
func shortClaudeWaits(t *testing.T, cmd, total, login time.Duration) {
	t.Helper()
	prevCmd := preflightCmdWait.Swap(int64(cmd))
	prevTotal := preflightTotalWait.Swap(int64(total))
	prevLogin := loginStatusWait.Swap(int64(login))
	t.Cleanup(func() {
		preflightCmdWait.Store(prevCmd)
		preflightTotalWait.Store(prevTotal)
		loginStatusWait.Store(prevLogin)
	})
}

// envLookup is a lookup over env, the last entry of a key winning.
func envLookup(env []string) func(string) string {
	return func(key string) string {
		prefix := key + "="
		for i := len(env) - 1; i >= 0; i-- {
			if strings.HasPrefix(env[i], prefix) {
				return env[i][len(prefix):]
			}
		}
		return ""
	}
}

func TestSpawnEnvCursorAndGrokGainOnlyTheMarker(t *testing.T) {
	base := []string{"A=1", "ANTHROPIC_API_KEY=k", "A=2", "CLAUDECODE=1", "CRAZE_SESSION_PROVIDER=old", "B=3", "NOEQUALS"}
	for _, p := range []Provider{CursorProvider(), GrokProvider()} {
		got := p.spawnEnv(base, "", "/ws")
		want := []string{"ANTHROPIC_API_KEY=k", "A=2", "CLAUDECODE=1", "B=3", "NOEQUALS", SessionProviderEnv + "=" + p.Name()}
		if !slices.Equal(got, want) {
			t.Fatalf("%s: %q, want %q", p.Name(), got, want)
		}
	}
}

func TestSpawnEnvNilIsTheParentsOwn(t *testing.T) {
	t.Setenv("CRAZE_TEST_SPAWNENV", "inherited")
	t.Setenv("ANTHROPIC_MODEL", "pinned")
	got := CursorProvider().spawnEnv(nil, "", "/ws")
	if !slices.Contains(got, "CRAZE_TEST_SPAWNENV=inherited") || !slices.Contains(got, "ANTHROPIC_MODEL=pinned") {
		t.Fatal("a nil base must be the parent environment")
	}
	if envLookup(got)(SessionProviderEnv) != "cursor" {
		t.Fatal("no marker")
	}
	// PWD is the agent's directory, as exec sets it for an inherited
	// environment; an explicit one keeps its own (above: no PWD added).
	t.Setenv("PWD", "/elsewhere")
	if pwd := envLookup(CursorProvider().spawnEnv(nil, "", "/ws"))("PWD"); pwd != "/ws" {
		t.Fatalf("PWD %q", pwd)
	}
	// Claude strips from an inherited environment too.
	got = ClaudeProvider().spawnEnv(nil, "/bin/claude", "/ws")
	if !slices.Contains(got, "CRAZE_TEST_SPAWNENV=inherited") || envLookup(got)("ANTHROPIC_MODEL") != "" {
		t.Fatalf("claude from nil: %v", slices.Contains(got, "CRAZE_TEST_SPAWNENV=inherited"))
	}
}

func TestSpawnEnvClaudeStripsAndSets(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/h", "CLAUDE_CONFIG_DIR=/c", "ANTHROPIC_BETA=x",
		"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR=3", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=4",
		"CLAUDE_AGENT_ACP_PROTOCOL=v2", ClaudeExecutableEnv + "=/elsewhere", SessionProviderEnv + "=grok"}
	// Plan 052 § A3's list, spelled out here rather than read from the code's
	// own, so a name dropped from the code fails.
	planStrip := []string{
		"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_HOST_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
		"CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN",
		"CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_DESKTOP_APP_VERSION", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_EFFORT",
		"CLAUDE_PID", "CLAUDE_AGENT_SDK_VERSION", "ELECTRON_RUN_AS_NODE",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_BEDROCK_BASE_URL",
		"ANTHROPIC_VERTEX_BASE_URL", "ANTHROPIC_FOUNDRY_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR",
		"ANTHROPIC_PROFILE", "ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE",
		"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_CUSTOM_MODEL_OPTION",
		"CLAUDE_MODEL_CONFIG", "CLAUDE_CODE_SUBAGENT_MODEL",
	}
	for _, k := range planStrip {
		base = append(base, k+"=set")
	}
	got := ClaudeProvider().spawnEnv(base, "/opt/claude", "/ws")
	look := envLookup(got)
	for _, k := range planStrip {
		for _, kv := range got {
			if strings.HasPrefix(kv, k+"=") {
				t.Fatalf("%s survived", k)
			}
		}
	}
	for k, v := range map[string]string{"PATH": "/usr/bin", "HOME": "/h", "CLAUDE_CONFIG_DIR": "/c", "ANTHROPIC_BETA": "x",
		"CLAUDE_AGENT_ACP_PROTOCOL": "v1", ClaudeExecutableEnv: "/opt/claude", SessionProviderEnv: "claude"} {
		if look(k) != v {
			t.Fatalf("%s = %q, want %q", k, look(k), v)
		}
	}
	// One of each key: the base's own protocol, executable and marker are
	// replaced, not shadowed.
	seen := map[string]bool{}
	for _, kv := range got {
		k, _, _ := strings.Cut(kv, "=")
		if seen[k] {
			t.Fatalf("%s twice in %q", k, got)
		}
		seen[k] = true
	}
}

func TestResolveCompanion(t *testing.T) {
	dir := t.TempDir()
	onPath := filepath.Join(dir, "claude")
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "claude-other")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CRAZE_AGENT_BIN", other)
	p := ClaudeProvider()
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if got, err := p.ResolveCompanion(get); err != nil || got != onPath {
		t.Fatalf("PATH: %q %v (CRAZE_AGENT_BIN is the adapter's, never the companion's)", got, err)
	}
	env[ClaudeExecutableEnv] = other
	if got, err := p.ResolveCompanion(get); err != nil || got != other {
		t.Fatalf("CLAUDE_CODE_EXECUTABLE wins: %q %v", got, err)
	}
	env[ClaudeExecutableEnv] = "/no/such/claude"
	if _, err := p.ResolveCompanion(get); err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_EXECUTABLE /no/such/claude not found") {
		t.Fatalf("missing override: %v", err)
	}
	delete(env, ClaudeExecutableEnv)
	t.Setenv("PATH", t.TempDir())
	if _, err := p.ResolveCompanion(get); err == nil || !strings.Contains(err.Error(), "claude not found on PATH") || !strings.Contains(err.Error(), "install Claude Code") {
		t.Fatalf("missing: %v", err)
	}
	if got, err := CursorProvider().ResolveCompanion(get); got != "" || err != nil {
		t.Fatal("cursor has no companion")
	}
}

// preflightCase runs claude's preflight against a fake claude and the fake
// agent as the adapter.
func runClaudePreflight(t *testing.T, claude string, extraEnv ...string) (PreflightInfo, error) {
	t.Helper()
	env := append(os.Environ(), extraEnv...)
	return ClaudeProvider().preflight(t.Context(), fakeAgentPath(t), claude, env, t.TempDir())
}

func wantKind(t *testing.T, err error, kind errkind.Kind, says ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", kind)
	}
	if got := StartErrorKind(err); got != kind {
		t.Fatalf("kind %s, want %s: %v", got, kind, err)
	}
	for _, s := range says {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("%q does not say %q", err, s)
		}
	}
}

func TestClaudePreflightPasses(t *testing.T) {
	claude := fakeClaude(t, okStatus, "2.1.326", "")
	cwd := t.TempDir()
	info, err := ClaudeProvider().preflight(t.Context(), fakeAgentPath(t), claude, os.Environ(), cwd)
	if err != nil || info.SubscriptionType != "max" {
		t.Fatalf("%+v %v", info, err)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(claude), "auth-args"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if lines[0] != "[--setting-sources][][auth][status][--json]" {
		t.Fatalf("argv %q", lines[0])
	}
	// A fresh directory of its own, never the session's.
	if len(lines) < 2 || lines[1] == cwd || !strings.Contains(lines[1], "craze-claude-preflight-") {
		t.Fatalf("auth status ran in %q", lines[1:])
	}
}

func TestClaudePreflightFailures(t *testing.T) {
	shortClaudeWaits(t, 400*time.Millisecond, 2*time.Second, 0)
	for _, tc := range []struct {
		name, status, version, sleep string
		env                          []string
		kind                         errkind.Kind
		says                         []string
	}{
		{"signed out", `{"loggedIn":false}`, "2.1.326", "", nil, errkind.Auth, []string{"not signed in", "run `claude auth login`"}},
		{"an API key", `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max","apiKeySource":"ANTHROPIC_API_KEY"}`, "2.1.326", "", nil, errkind.Auth, []string{"Pro or Max plan only", "an API key from ANTHROPIC_API_KEY"}},
		{"an oauth token", `{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"firstParty"}`, "2.1.326", "", nil, errkind.Auth, []string{"Pro or Max plan only", "auth method oauth_token"}},
		{"enterprise", `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"enterprise"}`, "2.1.326", "", nil, errkind.Auth, []string{"Pro or Max plan only", "plan enterprise"}},
		{"bedrock", `{"loggedIn":false,"authMethod":"none","apiProvider":"bedrock"}`, "2.1.326", "", nil, errkind.Auth, nil},
		{"no plan", `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}`, "2.1.326", "", nil, errkind.Auth, []string{"no plan"}},
		{"unparsable", `not json`, "2.1.326", "", nil, errkind.StartFailed, []string{"`claude auth status --json` printed nothing craze could read"}},
		{"old claude", okStatus, "2.1.300", "", nil, errkind.Config, []string{"claude 2.1.300 is older than craze needs (2.1.322)", "claude update"}},
		{"old adapter", okStatus, "2.1.326", "", []string{"CRAZE_FAKE_ADAPTER_VERSION=0.106.9"}, errkind.Config, []string{"claude-agent-acp 0.106.9 is older than craze needs (0.107.0)", "npm i -g @agentclientprotocol/claude-agent-acp"}},
		{"an adapter that is not a version", okStatus, "2.1.326", "", []string{"CRAZE_FAKE_ADAPTER_VERSION=garbage"}, errkind.StartFailed, []string{"claude-agent-acp --version"}},
		{"a timeout", okStatus, "2.1.326", "5", nil, errkind.StartFailed, []string{"did not answer in time"}},
		{"oversized", strings.Repeat("x", preflightOutputCap+10), "2.1.326", "", nil, errkind.StartFailed, []string{"printed nothing craze could read"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runClaudePreflight(t, fakeClaude(t, tc.status, tc.version, tc.sleep), tc.env...)
			wantKind(t, err, tc.kind, tc.says...)
		})
	}
}

func TestClaudePreflightCancelled(t *testing.T) {
	claude := fakeClaude(t, okStatus, "2.1.326", "5")
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := ClaudeProvider().preflight(ctx, fakeAgentPath(t), claude, os.Environ(), t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("the cancel did not end the preflight")
	}
}

// TestClaudePreflightKillsItsProcessGroup: what a command left running in its
// group — a background child of the fake claude — is killed on return.
func TestClaudePreflightKillsItsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	claude := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncase \"$*\" in\n  *'auth status'*) sleep 30 & echo $! > " + strconv.Quote(pidFile) + "\n    echo '" + okStatus + "' ;;\n  *--version*) echo 2.1.326 ;;\nesac\n"
	if err := os.WriteFile(claude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runClaudePreflight(t, claude); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the background child killed", func() bool {
		return syscall.Kill(pid, 0) != nil
	})
}

func TestVersionOlder(t *testing.T) {
	for _, tc := range []struct {
		v, min    string
		older, ok bool
	}{
		{"0.107.3", "0.107.0", false, true},
		{"0.107.0", "0.107.0", false, true},
		{"0.106.12", "0.107.0", true, true},
		{"1.0", "0.107.0", false, true},
		// A prerelease comes before its release (review F5), and a dotted
		// suffix is never a version component.
		{"2.1.322-beta", "2.1.322", true, true},
		{"0.107.0-rc1", "0.107.0", true, true},
		{"0.107.0-rc.1", "0.107.0", true, true},
		{"0.107.1-rc.1", "0.107.0", false, true},
		{"0.107.0+build.7", "0.107.0", false, true},
		{"0.106.9-rc.200", "0.107.0", true, true},
		{"2.1", "2.1.322", true, true},
		{"x.y", "2.1.322", false, false},
		{"7", "2.1.322", false, false},
	} {
		older, ok := versionOlder(tc.v, tc.min)
		if older != tc.older || ok != tc.ok {
			t.Errorf("%s vs %s = %v %v", tc.v, tc.min, older, ok)
		}
	}
}

func TestClaudeLoginRefusal(t *testing.T) {
	for _, tc := range []struct {
		st   acp.AuthStatus
		pass bool
		says string
	}{
		{acp.AuthStatus{Kind: "account", Label: "Claude Max", Plan: "max"}, true, ""},
		// probe2's second push: the plan carried as a label.
		{acp.AuthStatus{Kind: "account", Label: "Claude Ultra", Plan: "Claude Ultra"}, true, ""},
		{acp.AuthStatus{Kind: "account", Label: "Claude Enterprise", Plan: "enterprise"}, false, "Pro or Max plan only"},
		{acp.AuthStatus{Kind: "account", Label: "Claude subscription", Detail: "OAuth token"}, false, "OAuth token"},
		{acp.AuthStatus{Kind: "account", Label: "Claude Max", Plan: "max", Detail: "OAuth token"}, false, "OAuth token"},
		{acp.AuthStatus{Kind: "api_key", Label: "Anthropic API key", Detail: "apiKeyHelper"}, false, "an API key pays for this session"},
		{acp.AuthStatus{Kind: "none"}, false, "not signed in"},
		{acp.AuthStatus{Kind: "external", Label: "Amazon Bedrock"}, false, "bill this session to external"},
		{acp.AuthStatus{Kind: "gateway", Label: "Gateway"}, false, "gateway"},
	} {
		err := claudeLoginRefusal(tc.st)
		if tc.pass {
			if err != nil {
				t.Errorf("%+v refused: %v", tc.st, err)
			}
			continue
		}
		if err == nil || ErrorInfo(err).Kind != errkind.Auth || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%+v: %v", tc.st, err)
		}
	}
}

// claudeSession builds a claude session on the fake agent, with a fake claude
// as its companion and env as the extra environment.
type claudeRig struct {
	s     *session
	dir   string
	calls string
	setup string
	diag  *lockedDiag
}

func newClaudeRig(t *testing.T, script string, opts Options, env ...string) *claudeRig {
	t.Helper()
	shortClaudeWaits(t, 2*time.Second, 4*time.Second, 2*time.Second)
	dir := t.TempDir()
	r := &claudeRig{dir: dir, calls: filepath.Join(dir, "calls"), setup: filepath.Join(dir, "setup"), diag: &lockedDiag{}}
	claude := fakeClaude(t, okStatus, "2.1.326", "")
	p := ClaudeProvider()
	opts.Provider = &p
	opts.Binary = fakeAgentPath(t)
	opts.ExtraArgs = []string{"-script=" + script}
	if opts.Workspace == "" {
		opts.Workspace = t.TempDir()
	}
	opts.Stderr = &lockedDiag{}
	opts.Diag = r.diag
	t.Setenv("HOME", t.TempDir())
	opts.Env = append(os.Environ(), append([]string{
		ClaudeExecutableEnv + "=" + claude,
		"CRAZE_FAKE_DUMP_CALLS=" + r.calls,
		"CRAZE_FAKE_DUMP_SETUP=" + r.setup,
		"CRAZE_FAKE_DUMP_ENV=" + filepath.Join(dir, "env"),
	}, env...)...)
	r.s = newSession(opts)
	t.Cleanup(func() { _ = r.s.Close() })
	return r
}

func (r *claudeRig) read(t *testing.T, name string) string {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(r.dir, name))
	return string(raw)
}

func TestClaudeSessionStartsInManualWithFastOff(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{})
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	setup := r.read(t, "setup")
	if !strings.Contains(setup, `"configOptions":[{"configId":"mode","value":"default"},{"configId":"fast","value":"off"}]`) || strings.Contains(setup, `"_meta"`) {
		t.Fatalf("session/new params %s", setup)
	}
	snap := r.s.Snapshot()
	if snap.CurrentMode != "default" || FastOn(snap) {
		t.Fatalf("mode %q fast %v", snap.CurrentMode, FastOn(snap))
	}
	calls := r.read(t, "calls")
	if strings.Contains(calls, "session/set_mode") || strings.Contains(calls, "session/set_config_option") {
		t.Fatalf("the start mode is the setup request's, not a set: %s", calls)
	}
	env := r.read(t, "env")
	for _, want := range []string{"CRAZE_SESSION_PROVIDER=claude", "CLAUDE_AGENT_ACP_PROTOCOL=v1", "CLAUDE_CODE_EXECUTABLE="} {
		if !strings.Contains(env, want) {
			t.Fatalf("env lacks %s", want)
		}
	}
	res, err := r.s.Prompt(t.Context(), "hello")
	if err != nil || res.StopReason != "end_turn" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestClaudeSessionPlanAndUnattended(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{Mode: "plan", Unattended: true})
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	setup := r.read(t, "setup")
	if !strings.Contains(setup, `{"configId":"mode","value":"plan"}`) ||
		!strings.Contains(setup, `"_meta":{"claudeCode":{"options":{"disallowedTools":["ScheduleWakeup","Monitor","CronCreate"]}}}`) {
		t.Fatalf("params %s", setup)
	}
	if got := r.s.Snapshot().CurrentMode; got != "plan" {
		t.Fatalf("mode %q", got)
	}
}

func TestClaudeSessionRefusesAskAndFastBeforeSpawning(t *testing.T) {
	on := true
	for _, tc := range []struct {
		opts Options
		kind errkind.Kind
		says string
	}{
		{Options{Mode: "ask"}, errkind.Config, "mode ask is not advertised"},
		{Options{Fast: &on}, errkind.Usage, FastBillsExtra},
	} {
		r := newClaudeRig(t, "claude-basic", tc.opts)
		err := r.s.Start(t.Context())
		if err == nil || ErrorInfo(err).Kind != tc.kind || !strings.Contains(err.Error(), tc.says) {
			t.Fatalf("%v", err)
		}
		if r.read(t, "env") != "" {
			t.Fatal("the agent was spawned")
		}
	}
}

func TestClaudeSessionValidatesTheStartMode(t *testing.T) {
	// The initial mode skipped silently, and a set takes it: retried once.
	r := newClaudeRig(t, "claude-basic", Options{}, "CRAZE_FAKE_CLAUDE_SKIP_INITIAL=mode")
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.read(t, "calls"), "session/set_config_option mode=default") || r.s.Snapshot().CurrentMode != "default" {
		t.Fatalf("calls %s mode %q", r.read(t, "calls"), r.s.Snapshot().CurrentMode)
	}
	// Neither takes it: the start fails with config.
	r = newClaudeRig(t, "claude-basic", Options{}, "CRAZE_FAKE_CLAUDE_STICKY=mode")
	err := r.s.Start(t.Context())
	wantKind(t, err, errkind.Config, "kept this session in mode auto where craze starts it in default")
}

func TestClaudeSessionKeepsFastOff(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{}, "CRAZE_FAKE_CLAUDE_FAST=on", "CRAZE_FAKE_CLAUDE_SKIP_INITIAL=fast")
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.read(t, "calls"), "session/set_config_option fast=off") || FastOn(r.s.Snapshot()) {
		t.Fatalf("fast left on: %s", r.read(t, "calls"))
	}
	// A config_option_update turning it on mid-session is answered by
	// setting it off again.
	before := strings.Count(r.read(t, "calls"), "fast=off")
	if _, err := r.s.Prompt(t.Context(), "FAST-ON"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fast set off again", func() bool { return strings.Count(r.read(t, "calls"), "fast=off") > before })
	waitFor(t, "fast off in the snapshot", func() bool { return !FastOn(r.s.Snapshot()) })
	// No client turns it on.
	if _, err := r.s.SetConfig(t.Context(), "", "fast", "on", ""); err == nil || !strings.Contains(err.Error(), FastBillsExtra) {
		t.Fatalf("set fast on: %v", err)
	}
	// Stuck on: the start fails.
	r = newClaudeRig(t, "claude-basic", Options{}, "CRAZE_FAKE_CLAUDE_FAST=on", "CRAZE_FAKE_CLAUDE_STICKY=fast")
	wantKind(t, r.s.Start(t.Context()), errkind.Config, "kept fast mode on")
}

func TestClaudeLoginGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		kind errkind.Kind
		says string
	}{
		{"no status at all", []string{"CRAZE_FAKE_CLAUDE_AUTH=silent"}, errkind.StartFailed, "could not confirm which Claude login pays"},
		{"a delayed api key", []string{"CRAZE_FAKE_CLAUDE_AUTH=api_key", "CRAZE_FAKE_CLAUDE_AUTH_DELAY=300ms"}, errkind.Auth, "an API key pays for this session"},
		{"an enterprise account", []string{"CRAZE_FAKE_CLAUDE_AUTH=enterprise"}, errkind.Auth, "Pro or Max plan only"},
		{"an oauth token account", []string{"CRAZE_FAKE_CLAUDE_AUTH=oauth"}, errkind.Auth, "OAuth token"},
		{"signed out", []string{"CRAZE_FAKE_CLAUDE_AUTH=none"}, errkind.Auth, "not signed in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newClaudeRig(t, "claude-basic", Options{}, tc.env...)
			shortClaudeWaits(t, 2*time.Second, 4*time.Second, time.Second)
			err := r.s.Start(t.Context())
			wantKind(t, err, tc.kind, tc.says)
			if strings.Contains(r.read(t, "calls"), "session/new") {
				t.Fatal("a session was created on a refused login")
			}
		})
	}
}

// TestClaudeLoginGateLaterStatus: a push after the first that fails the gate
// ends the turn in flight with auth, and every prompt after it is refused.
func TestClaudeLoginGateLaterStatus(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{})
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err := r.s.Prompt(t.Context(), "AUTH-LATER")
	wantKind(t, err, errkind.Auth, "an API key pays for this session")
	_, err = r.s.Prompt(t.Context(), "hello")
	wantKind(t, err, errkind.Auth, "an API key pays for this session")
}

func TestClaudeForcePicksAllowOnceOnly(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{Force: true})
	log := collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.Prompt(t.Context(), "WRITE"); err != nil {
		t.Fatal(err)
	}
	// The option id the request carried, echoed byte for byte.
	log.waitTexts(t, "permission: allow-once")
	var stamped *ToolEdited
	for _, tool := range r.s.Snapshot().Tools {
		if tool.ID == "toolu_write_1" {
			stamped = tool.Edited
		}
	}
	if stamped == nil || !slices.Equal(stamped.Files, []string{"out.txt"}) {
		t.Fatalf("the Write's stamp: %+v", stamped)
	}
}

func TestClaudeForceWithNoAllowOnceShowsTheCard(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{Force: true, Interactive: true})
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := r.s.Prompt(context.Background(), "WRITE-NOONCE"); done <- err }()
	waitFor(t, "the permission card", func() bool { return len(r.s.asks.Asks()) == 1 })
	ask := r.s.asks.Asks()[0]
	if ask.Kind != AskPermission || ask.Status != AskOpen {
		t.Fatalf("ask %+v", ask)
	}
	if _, err := r.s.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestClaudeStopReasonsAreAgentErrors(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{})
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, stop := range []string{acp.StopRefusal, acp.StopMaxTokens, acp.StopMaxTurnRequests} {
		_, err := r.s.Prompt(t.Context(), "STOP "+stop)
		if err == nil || ErrorInfo(err).Kind != errkind.Agent || !strings.Contains(err.Error(), stop) {
			t.Fatalf("%s: %v", stop, err)
		}
	}
	if refusedStopReason(CursorProvider(), acp.StopRefusal) != nil || refusedStopReason(GrokProvider(), acp.StopMaxTokens) != nil {
		t.Fatal("cursor and grok keep their stop reasons")
	}
}

// TestClaudeOpenToolsClosedOnAFailedTurn: a tool still running when the prompt
// fails, or the agent exits, is closed with the turn (plan 052 § B9).
func TestClaudeOpenToolsClosedOnAFailedTurn(t *testing.T) {
	for _, directive := range []string{"TOOL-FAIL", "TOOL-EXIT"} {
		r := newClaudeRig(t, "claude-basic", Options{})
		if err := r.s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := r.s.Prompt(t.Context(), directive); err == nil {
			t.Fatalf("%s: no error", directive)
		}
		for _, tool := range r.s.Snapshot().Tools {
			if tool.ID == "toolu_open" && toolStatusInFlight(tool.Status) {
				t.Fatalf("%s: the tool is still %s", directive, tool.Status)
			}
		}
	}
}

// TestClaudeCancelNeverPrecedesThePrompt: a cancel asked the moment a prompt
// is claimed reaches the agent behind it, or not at all (plan 052 § B9).
func TestClaudeCancelNeverPrecedesThePrompt(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{})
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	run := r.s.Begin("HANG")
	done := make(chan error, 1)
	go func() { _, err := run(context.Background()); done <- err }()
	if _, err := r.s.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-done
	calls := r.read(t, "calls")
	p, c := strings.Index(calls, "session/prompt"), strings.Index(calls, "session/cancel")
	if c >= 0 && (p < 0 || c < p) {
		t.Fatalf("cancel before the prompt: %s", calls)
	}
}

func TestClaudeLoadSendsModeAndRecordedModel(t *testing.T) {
	r := newClaudeRig(t, "claude-load", Options{LoadSessionID: "s-old", LoadModel: "haiku"})
	drain := collect(t, r.s)
	_ = drain
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	setup := r.read(t, "setup")
	if !strings.Contains(setup, `"configOptions":[{"configId":"mode","value":"default"},{"configId":"model","value":"haiku"},{"configId":"fast","value":"off"}]`) {
		t.Fatalf("load params %s", setup)
	}
	snap := r.s.Snapshot()
	if snap.CurrentMode != "default" {
		t.Fatalf("a load resumes in Manual, not the transcript's bypass: %q", snap.CurrentMode)
	}
	if opt := ModelConfigOptionIn(snap.Config); opt == nil || opt.Current != "haiku" {
		t.Fatalf("model %+v", opt)
	}
}

func TestClaudeLoadNotesARefusedRecordedModel(t *testing.T) {
	r := newClaudeRig(t, "claude-load", Options{LoadSessionID: "s-old", LoadModel: "haiku"}, "CRAZE_FAKE_CLAUDE_REFUSE_MODEL=haiku")
	collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := r.diag.String(); !strings.Contains(got, "the session's recorded model haiku was not applied; it runs on opus") {
		t.Fatalf("diag %q", got)
	}
	// --model names the model instead: none is sent in the load.
	r = newClaudeRig(t, "claude-load", Options{LoadSessionID: "s-old", LoadModel: "haiku", Model: "sonnet"})
	collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.read(t, "setup"), `"configId":"model"`) {
		t.Fatalf("--model must not send the recorded model: %s", r.read(t, "setup"))
	}
}

func TestClaudeModeCycle(t *testing.T) {
	snap := Snapshot{Provider: ClaudeProvider().Info()}
	for _, id := range claudeModes() {
		snap.Modes = append(snap.Modes, ModeInfo{ID: id})
	}
	for cur, next := range map[string]string{
		"default": "acceptEdits", "acceptEdits": "plan", "plan": "default",
		"auto": "default", "bypassPermissions": "default",
	} {
		snap.CurrentMode = cur
		if got := NextModeID(snap); got != next {
			t.Errorf("from %s: %s, want %s", cur, got, next)
		}
	}
	// Cursor still cycles every advertised mode.
	snap.Provider = CursorProvider().Info()
	snap.Modes = []ModeInfo{{ID: "agent"}, {ID: "plan"}, {ID: "ask"}}
	snap.CurrentMode = "plan"
	if NextModeID(snap) != "ask" {
		t.Fatal("cursor's cycle changed")
	}
	if ClaudeProvider().ModeKind("default") != ModeImplement || ClaudeProvider().ModeKind("plan") != ModePlan ||
		ClaudeProvider().ModeKind("ask") != ModeUnknown || ClaudeProvider().ImplementPrompt() != "" {
		t.Fatal("claude's mode table")
	}
}

func claudeModes() []string {
	return []string{"default", "acceptEdits", "plan", "auto", "bypassPermissions"}
}

func TestClaudeProviderIsHiddenAndResolvable(t *testing.T) {
	p, err := ProviderByName("claude")
	if err != nil || !p.Hidden() || !p.Resumable() || p.Dialect() != acp.DialectClaude || !slices.Equal(p.Bins(), []string{"claude-agent-acp"}) {
		t.Fatalf("%+v %v", p, err)
	}
	if slices.Contains(ProviderNames(), "claude") {
		t.Fatal("claude is listed before C5")
	}
	// C3's capabilities, and nothing of a later milestone's: no sub-agent
	// rows (C4), no fast toggle (craze keeps fast off, § D3).
	want := Capabilities{Effort: true, Modes: true, Todos: true, AskCards: true, PlanCards: true, ParameterizedPicker: true, Interject: true}
	if p.Capabilities() != want {
		t.Fatalf("claude declares %+v, want C3's %+v", p.Capabilities(), want)
	}
	if !p.isSubagentTool("Agent") || !p.isSubagentTool("Task") || p.isSubagentTool("task") {
		t.Fatal("claude's sub-agent tool names")
	}
}

func TestClaudeForceOption(t *testing.T) {
	opts := []acp.PermissionOption{
		{OptionID: "allow-with-updates", Kind: acp.KindAllowAlways},
		{OptionID: "allow-once", Kind: acp.KindAllowOnce},
		{OptionID: "reject", Kind: acp.KindRejectOnce},
	}
	if id, ok := ClaudeProvider().ForceOption(opts); !ok || id != "allow-once" {
		t.Fatalf("claude %q", id)
	}
	if id, ok := CursorProvider().ForceOption(opts); !ok || id != "allow-with-updates" {
		t.Fatalf("cursor %q", id)
	}
	if _, ok := ClaudeProvider().ForceOption(opts[:1]); ok {
		t.Fatal("claude has nothing to pick with no allow_once")
	}
}

func TestClaudePlanFileIsNoEdit(t *testing.T) {
	cfg := t.TempDir()
	get := func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return cfg
		}
		return ""
	}
	dir := claudeConfigDir(get)
	if !isClaudePlanFile(filepath.Join(cfg, "plans", "fuzzy-plan.md"), dir) {
		t.Fatal("the plan file")
	}
	if isClaudePlanFile(filepath.Join(cfg, "settings.json"), dir) || isClaudePlanFile(filepath.Join(cfg, "plans"), dir) {
		t.Fatal("not a plan file")
	}
	sc := editScope{workspace: t.TempDir(), isPlan: func(p string) bool { return isClaudePlanFile(p, dir) }}
	got := sc.edited([]string{filepath.Join(cfg, "plans", "p.md")})
	if got == nil || got.PlanFile == "" || len(got.OutsideFiles) != 0 {
		t.Fatalf("%+v", got)
	}
}

// TestClaudeAgentEnvironmentIsStripped (plan 052 AC4): the adapter, as
// spawned, sees none of the stripped variables and the three set ones.
func TestClaudeAgentEnvironmentIsStripped(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{}, "ANTHROPIC_API_KEY=sk-test", "CLAUDECODE=1",
		"ANTHROPIC_MODEL=pinned", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=5", SessionProviderEnv+"=grok")
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(r.read(t, "env"), "\n")
	for _, gone := range []string{"ANTHROPIC_API_KEY", "CLAUDECODE", "ANTHROPIC_MODEL", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR"} {
		if slices.Contains(lines, gone) {
			t.Fatalf("%s reached the adapter", gone)
		}
	}
	if !slices.Contains(lines, SessionProviderEnv+"=claude") || !slices.Contains(lines, "CLAUDE_AGENT_ACP_PROTOCOL=v1") {
		t.Fatal("the set variables are missing")
	}
	// The preflight is claude's alone: any other provider has nothing to run.
	if _, err := CursorProvider().preflight(t.Context(), "/no/such/agent", "", nil, ""); err != nil {
		t.Fatalf("cursor preflight: %v", err)
	}
}

// TestClaudeRefusedModelIsModelNotFound (plan 052 AC12): a --model the
// adapter refuses fails the start with model_not_found.
func TestClaudeRefusedModelIsModelNotFound(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{Model: "no-such-model"})
	err := r.s.Start(t.Context())
	if got := StartErrorKind(err); got != errkind.ModelNotFound {
		t.Fatalf("kind %s: %v", got, err)
	}
}

// TestResolveCompanionIsAbsolute (review F4): a relative CLAUDE_CODE_EXECUTABLE
// resolves to an absolute path, so the preflight — run in a temporary
// directory of its own — and the spawn find the same file.
func TestResolveCompanionIsAbsolute(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	claude := fakeClaude(t, okStatus, "2.1.326", "")
	raw, err := os.ReadFile(claude)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tools", "claude"), raw, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	got, err := ClaudeProvider().ResolveCompanion(func(k string) string {
		if k == ClaudeExecutableEnv {
			return "./tools/claude"
		}
		return ""
	})
	if err != nil || got != filepath.Join(dir, "tools", "claude") {
		t.Fatalf("resolved %q, %v", got, err)
	}
	if _, err := runClaudePreflight(t, got); err != nil {
		t.Fatalf("the preflight of the resolved companion: %v", err)
	}
}

// TestClaudeStartRetryKeepsTheModel (review F2): a start mode or fast retry
// on a new session with no --model keeps the model session/new answered with,
// which the index row records.
func TestClaudeStartRetryKeepsTheModel(t *testing.T) {
	for _, env := range [][]string{
		{"CRAZE_FAKE_CLAUDE_SKIP_INITIAL=mode"},
		{"CRAZE_FAKE_CLAUDE_FAST=on", "CRAZE_FAKE_CLAUDE_SKIP_INITIAL=fast"},
	} {
		r := newClaudeRig(t, "claude-basic", Options{}, env...)
		if err := r.s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.read(t, "calls"), "session/set_config_option") {
			t.Fatalf("%v: no retry was made", env)
		}
		if got := r.s.Snapshot().CurrentModel; got != "opus" {
			t.Fatalf("%v: the model after the retry is %q, want opus", env, got)
		}
	}
}

// claudeFastOnUpdate is a config_option_update whose catalog — the fake's
// own, mode Manual and model opus — has fast mode on.
func claudeFastOnUpdate() acp.SessionNotification {
	sel := func(id, cat, cur string, vals ...string) map[string]any {
		opts := []map[string]string{}
		for _, v := range vals {
			opts = append(opts, map[string]string{"value": v, "name": v})
		}
		return map[string]any{"id": id, "name": id, "category": cat, "type": "select", "currentValue": cur, "options": opts}
	}
	fast := sel("fast", "model_config", "on")
	fast["options"] = []map[string]string{{"value": "on", "name": "On"}, {"value": "off", "name": "Off"}}
	raw, _ := json.Marshal(map[string]any{"sessionUpdate": "config_option_update", "configOptions": []any{
		sel("mode", "mode", "default", "default", "acceptEdits", "plan", "auto", "bypassPermissions"),
		sel("model", "model", "opus", "default", "opus", "sonnet", "haiku"),
		sel("effort", "thought_level", "high", "low", "high"),
		fast,
		sel("ultracode", "model_config", "off", "on", "off"),
	}})
	return acp.SessionNotification{SessionID: "fake-session-1", Update: raw}
}

// TestClaudeFastOnBeforeTheInstallIsRechecked (review F1a): fast turned on
// after the start's own check and before the session/new install is still
// turned off before the start returns.
func TestClaudeFastOnBeforeTheInstallIsRechecked(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{})
	r.s.beforeInstall = func() { r.s.onUpdate(claudeFastOnUpdate()) }
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if FastOn(r.s.Snapshot()) || !strings.Contains(r.read(t, "calls"), "session/set_config_option fast=off") {
		t.Fatalf("fast left on after the start: %s", r.read(t, "calls"))
	}
}

// TestClaudeFastOnDuringTheResetIsRechecked (review F1b): fast turned on again
// while a reset is in flight is reset once more, not dropped.
func TestClaudeFastOnDuringTheResetIsRechecked(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{})
	var armed, fired atomic.Bool
	r.s.beforeSetSection = func() {
		if armed.Load() && fired.CompareAndSwap(false, true) {
			r.s.onUpdate(claudeFastOnUpdate())
		}
	}
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	if _, err := r.s.Prompt(t.Context(), "FAST-ON"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second reset", func() bool { return strings.Count(r.read(t, "calls"), "fast=off") >= 2 })
	waitFor(t, "fast off", func() bool { return !FastOn(r.s.Snapshot()) })
	if _, err := r.s.Prompt(t.Context(), "hello"); err != nil {
		t.Fatalf("a prompt after fast went off: %v", err)
	}
}

// TestClaudeFastThatWillNotGoOffStopsTheSession (review F1c): a reset that
// fails, or whose reply still has fast on, stops the session with config;
// every prompt after is refused.
func TestClaudeFastThatWillNotGoOffStopsTheSession(t *testing.T) {
	for _, knob := range []string{"CRAZE_FAKE_CLAUDE_STICKY=fast", "CRAZE_FAKE_SET_REFUSE=fast"} {
		r := newClaudeRig(t, "claude-basic", Options{}, knob)
		if err := r.s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		_, _ = r.s.Prompt(t.Context(), "FAST-ON")
		waitFor(t, "the session stopped", func() bool { return r.s.fatalErr() != nil })
		_, err := r.s.Prompt(t.Context(), "hello")
		wantKind(t, err, errkind.Config, FastBillsExtra)
	}
}

// TestClaudeLoadSetsFastOffWhateverTheReplySays (review C2b 1): after a load
// craze cannot know the adapter's fast mode — a push during the replay is
// overruled by the load's reply — so it sets it off once, whatever the reply
// said.
func TestClaudeLoadSetsFastOffWhateverTheReplySays(t *testing.T) {
	r := newClaudeRig(t, "claude-load", Options{LoadSessionID: "s-old"}, "CRAZE_FAKE_CLAUDE_LOAD_FAST_HIDDEN=1")
	collect(t, r.s)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	calls := r.read(t, "calls")
	load := strings.Index(calls, "session/load")
	if load < 0 || !strings.Contains(calls[load:], "session/set_config_option fast=off") {
		t.Fatalf("no fast reset after the load: %s", calls)
	}
}

// holdSets makes CRAZE_FAKE_SET_GATE a FIFO nothing writes until release
// is called: every session/set_config_option is answered only then.
func holdSets(t *testing.T) (env string, release func()) {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "set-gate")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	return "CRAZE_FAKE_SET_GATE=" + fifo, func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = f.Write([]byte{1})
		_ = f.Close()
	}
}

// TestClaudePromptWaitsForAResetThatStartsBeforeItsWrite (review C2b 2): a
// fast reset that begins after the prompt's own check, just before its write,
// still holds the prompt back until fast is off.
func TestClaudePromptWaitsForAResetThatStartsBeforeItsWrite(t *testing.T) {
	gate, release := holdSets(t)
	r := newClaudeRig(t, "claude-basic", Options{}, gate)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	var fired atomic.Bool
	prev := testBeforeWire
	testBeforeWire = func(s *session) {
		if s == r.s && fired.CompareAndSwap(false, true) {
			s.onUpdate(claudeFastOnUpdate())
		}
	}
	t.Cleanup(func() { testBeforeWire = prev })
	done := make(chan error, 1)
	go func() { _, err := r.s.Prompt(context.Background(), "hello"); done <- err }()
	waitFor(t, "the reset's set", func() bool { return strings.Contains(r.read(t, "calls"), "fast=off") })
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(r.read(t, "calls"), "session/prompt") {
		t.Fatalf("the prompt was written while fast was being reset: %s", r.read(t, "calls"))
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestClaudePromptNotWrittenAfterTheSessionStopped (review C2b 2): a prompt
// past its checks when the session is stopped is never written.
func TestClaudePromptNotWrittenAfterTheSessionStopped(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{})
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	stop := claudeErr(errkind.Config, "stopped for the test")
	prev := testBeforeWire
	testBeforeWire = func(s *session) {
		if s == r.s {
			s.stopFor(stop)
		}
	}
	t.Cleanup(func() { testBeforeWire = prev })
	_, err := r.s.Prompt(t.Context(), "hello")
	wantKind(t, err, errkind.Config, "stopped for the test")
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(r.read(t, "calls"), "session/prompt") {
		t.Fatalf("the prompt reached the agent: %s", r.read(t, "calls"))
	}
}

// TestClaudeFastResetHasAHardDeadline (review C2b 3): a reset whose set never
// comes back stops the session at its own deadline, whatever the set does.
func TestClaudeFastResetHasAHardDeadline(t *testing.T) {
	gate, _ := holdSets(t)
	r := newClaudeRig(t, "claude-basic", Options{}, gate)
	prev := fastFixDeadline.Swap(int64(300 * time.Millisecond))
	t.Cleanup(func() { fastFixDeadline.Store(prev) })
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.Prompt(t.Context(), "FAST-ON"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for r.s.fatalErr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the reset outlived its deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, err := r.s.Prompt(t.Context(), "hello")
	wantKind(t, err, errkind.Config, FastBillsExtra)
}

// TestClaudeNoPromptWrittenOnceTheStopIsRecorded (review C2c 1): a prompt past
// its gate when the session is stopped is never written once the stop is on
// record — the agent's stdin is closed before the fatal error is visible.
func TestClaudeNoPromptWrittenOnceTheStopIsRecorded(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{})
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	passed, release := make(chan struct{}), make(chan struct{})
	r.s.clientRef().SetPromptGate(func(ctx context.Context) error {
		err := r.s.awaitFastFix(ctx)
		if err == nil {
			close(passed)
			<-release
		}
		return err
	})
	written := make(chan struct{})
	prev := testStopForRecorded
	testStopForRecorded = func() {
		// The prompt, past its gate, goes for the wire now, in the window
		// between the stop's record and anything after it.
		close(release)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(r.read(t, "calls"), "session/prompt") {
				close(written)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Cleanup(func() { testStopForRecorded = prev })
	done := make(chan error, 1)
	go func() { _, err := r.s.Prompt(context.Background(), "hello"); done <- err }()
	<-passed
	r.s.stopFor(claudeErr(errkind.Config, "stopped for the test"))
	<-done
	select {
	case <-written:
		t.Fatalf("the prompt reached the agent after the stop was recorded: %s", r.read(t, "calls"))
	default:
	}
}

// TestClaudeSteeringGoesThroughTheGate (review C2c 2): a mid-turn steer waits
// for a fast reset in flight like a prompt, and a stopped session refuses it
// with its own error.
func TestClaudeSteeringGoesThroughTheGate(t *testing.T) {
	gate, release := holdSets(t)
	r := newClaudeRig(t, "claude-basic", Options{}, gate)
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	turn := make(chan error, 1)
	go func() { _, err := r.s.Prompt(context.Background(), "HANG"); turn <- err }()
	waitFor(t, "the turn", func() bool { return strings.Contains(r.read(t, "calls"), "session/prompt") })
	r.s.onUpdate(claudeFastOnUpdate())
	waitFor(t, "the reset's set", func() bool { return strings.Contains(r.read(t, "calls"), "fast=off") })
	steered := make(chan error, 1)
	go func() { steered <- r.s.Interject(context.Background(), "and also this") }()
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(r.read(t, "calls"), acp.MethodClaudeSteering) {
		t.Fatalf("the steer was written while fast was being reset: %s", r.read(t, "calls"))
	}
	release()
	<-steered
	// Stopped: a steer is refused with the session's own error.
	r.s.stopFor(claudeErr(errkind.Config, "stopped for the test"))
	err := r.s.Interject(context.Background(), "more")
	wantKind(t, err, errkind.Config, "stopped for the test")
	<-turn
}

// TestClaudeAnObsoleteFastDeadlineDoesNothing (review C2c 3): a reset's
// deadline that fires once that reset has ended — another in flight, or the
// session closed — stops nothing.
func TestClaudeAnObsoleteFastDeadlineDoesNothing(t *testing.T) {
	r := newClaudeRig(t, "claude-basic", Options{})
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	a, b := make(chan struct{}), make(chan struct{})
	r.s.mu.Lock()
	r.s.fastFix = b // reset A confirmed off and ended; B is in flight
	r.s.mu.Unlock()
	r.s.fastDeadline(a, time.Second)
	if err := r.s.fatalErr(); err != nil {
		t.Fatalf("A's deadline stopped the session while B ran: %v", err)
	}
	r.s.mu.Lock()
	r.s.fastFix = nil
	r.s.mu.Unlock()
	_ = r.s.Close()
	r.s.fastDeadline(b, time.Second)
	if err := r.s.fatalErr(); err != nil {
		t.Fatalf("a deadline after Close stopped the session: %v", err)
	}
}

// TestClaudeLoadFastResetTimeoutIsConfig (review C2c 4): the fast reset a load
// owes, unanswered, fails the start with config.
func TestClaudeLoadFastResetTimeoutIsConfig(t *testing.T) {
	gate, _ := holdSets(t)
	prev := startSettingWait
	startSettingWait = 300 * time.Millisecond
	t.Cleanup(func() { startSettingWait = prev })
	r := newClaudeRig(t, "claude-load", Options{LoadSessionID: "s-old"}, gate)
	collect(t, r.s)
	wantKind(t, r.s.Start(t.Context()), errkind.Config, FastBillsExtra)
}

// TestClaudeAFastDeadlineParkedAfterItsCheck (review C2d 1): a deadline that
// found its reset current and is then held — while that reset completes and
// another starts, or the session closes — stops nothing it did not check: the
// check and the stop are one step.
func TestClaudeAFastDeadlineParkedAfterItsCheck(t *testing.T) {
	for _, gap := range []string{"A completes, B starts", "Close"} {
		t.Run(gap, func(t *testing.T) {
			r := newClaudeRig(t, "claude-basic", Options{})
			if err := r.s.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			a, b := make(chan struct{}), make(chan struct{})
			r.s.mu.Lock()
			r.s.fastFix = a
			r.s.mu.Unlock()
			// The competitor changes the state the check read — B replaces A,
			// or the session closes — and records whether the stop had already
			// been recorded when it got there. It needs s.mu, so it can only
			// run where the deadline's check has let go of it.
			competitor := func() bool {
				if gap == "Close" {
					r.s.mu.Lock()
					stopped := r.s.gate.fatal != nil
					r.s.mu.Unlock()
					_ = r.s.Close()
					return stopped
				}
				r.s.mu.Lock()
				defer r.s.mu.Unlock()
				r.s.fastFix = b
				return r.s.gate.fatal != nil
			}
			var ran, lockHeld bool
			done := make(chan bool, 1)
			prev := testFastDeadlineValidated
			testFastDeadlineValidated = func() {
				ran = true
				// Parked just after the check passed. If the check let go of
				// s.mu, the competitor runs to its end here, between the check
				// and the stop — the very gap the atomic check closes; if it
				// still holds it, the competitor can only follow the stop.
				if r.s.mu.TryLock() {
					r.s.mu.Unlock()
					done <- competitor()
					return
				}
				lockHeld = true
				go func() { done <- competitor() }()
			}
			t.Cleanup(func() { testFastDeadlineValidated = prev })
			r.s.fastDeadline(a, time.Second)
			stoppedFirst := <-done // the competitor, joined
			if !ran {
				t.Fatal("the deadline's check never passed: the parked callback did not run")
			}
			if !lockHeld || !stoppedFirst {
				t.Fatalf("after %s the state changed between A's check and its stop (lock held %v, stop recorded first %v)", gap, lockHeld, stoppedFirst)
			}
		})
	}
}

// TestClaudeLoadFastSetTimeoutStaysConfigUnderALateCancel (review C2d 2): the
// load's fast-off set that ran out its own time is config, even when the
// caller's context is cancelled a moment later.
func TestClaudeLoadFastSetTimeoutStaysConfigUnderALateCancel(t *testing.T) {
	gate, _ := holdSets(t)
	prevWait := startSettingWait
	startSettingWait = 300 * time.Millisecond
	t.Cleanup(func() { startSettingWait = prevWait })
	r := newClaudeRig(t, "claude-load", Options{LoadSessionID: "s-old"}, gate)
	collect(t, r.s)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	prev := testAfterLoadFastSet
	testAfterLoadFastSet = func() { cancel() }
	t.Cleanup(func() { testAfterLoadFastSet = prev })
	wantKind(t, r.s.Start(ctx), errkind.Config, FastBillsExtra)
}

func TestCallerEnded(t *testing.T) {
	refusal := &acp.RPCError{Code: -32602, Message: "nope"}
	for _, tc := range []struct {
		err, cause error
		want       bool
	}{
		{context.Canceled, context.Canceled, true},                 // the caller cancelled first
		{context.DeadlineExceeded, context.DeadlineExceeded, true}, // the caller's own deadline
		{context.DeadlineExceeded, errStartSettingWait, false},     // the set's own timeout
		{refusal, nil, false},                                      // the agent refused
		{refusal, context.Canceled, false},                         // refused, then the caller left
	} {
		if got := callerEnded(tc.err, tc.cause); got != tc.want {
			t.Errorf("callerEnded(%v, %v) = %v", tc.err, tc.cause, got)
		}
	}
}
