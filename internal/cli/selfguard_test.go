package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/errkind"
)

// The self-delegation guard (plan 052 § J2, J6): the caller's markers and
// their precedence, the verdict and its override, and each path of craze new
// checked against the provider it would actually start.

// sameProvider is the guard's refusal for provider p, word for word.
func sameProvider(p string) string {
	return "craze: this agent already runs on " + p + "; use your own sub-agent tool for " + p +
		" work, not craze (--allow-same-provider overrides)"
}

// markers sets the three caller markers for one case: "" unsets one.
func markers(t *testing.T, session, claudeCode, cursorAgent string) {
	t.Helper()
	for name, v := range map[string]string{agent.SessionProviderEnv: session, claudeCodeEnv: claudeCode, cursorAgentEnv: cursorAgent} {
		t.Setenv(name, v)
		if v == "" {
			_ = os.Unsetenv(name)
		}
	}
}

// TestCallerProviderMarkers: CRAZE_SESSION_PROVIDER first, whatever else is
// set; then Claude Code's CLAUDECODE=1 or cursor-agent's CURSOR_AGENT alone;
// both together (one launched the other, each inherits the other's) name no
// caller, and neither does a CLAUDECODE that is not 1.
func TestCallerProviderMarkers(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		session, claudeCode, cursorA string
		want                         string
	}{
		{"nothing", "", "", "", ""},
		{"Claude Code", "", "1", "", "claude"},
		{"CLAUDECODE not 1", "", "0", "", ""},
		{"cursor-agent", "", "", "1", "cursor"},
		{"cursor-agent, any value", "", "", "cli", "cursor"},
		{"both env markers: ambiguous", "", "1", "1", ""},
		{"the session marker alone", "grok", "", "", "grok"},
		{"the session marker beats CLAUDECODE", "cursor", "1", "", "cursor"},
		{"the session marker beats CURSOR_AGENT", "native", "", "1", "native"},
		{"the session marker settles the ambiguity", "claude", "1", "1", "claude"},
		{"a blank session marker is none", "  ", "1", "", "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			markers(t, tc.session, tc.claudeCode, tc.cursorA)
			if got := callerProvider(os.Getenv); got != tc.want {
				t.Fatalf("callerProvider = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSelfDelegationVerdict: the caller's own provider is a usage error, exit
// 2, kind usage, in exactly the plan's words; --allow-same-provider, another
// provider, no caller, no target and an unknown target all pass.
func TestSelfDelegationVerdict(t *testing.T) {
	markers(t, "", "1", "")
	err := selfDelegation("claude", false)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 || ee.msg != sameProvider("claude") || ee.ErrorInfo().Kind != errkind.Usage {
		t.Fatalf("claude from Claude Code: %#v", err)
	}
	for _, target := range []string{"cursor", "grok", "native", "", "nosuch"} {
		if err := selfDelegation(target, false); err != nil {
			t.Errorf("%q from Claude Code: %v", target, err)
		}
	}
	if err := selfDelegation("claude", true); err != nil {
		t.Errorf("--allow-same-provider: %v", err)
	}
	markers(t, "cursor", "1", "")
	if err := selfDelegation("claude", false); err != nil {
		t.Errorf("claude from a cursor session Claude Code started: %v", err)
	}
	if err := selfDelegation("cursor", false); err == nil || err.Error() != sameProvider("cursor") {
		t.Errorf("cursor from a cursor session: %v", err)
	}
	markers(t, "", "", "")
	if err := selfDelegation("claude", false); err != nil {
		t.Errorf("no caller: %v", err)
	}
}

// TestNewGuardOnTheHubsDefault: plain craze new with no --provider checks the
// hub's default — and only when there is a caller to check it against — binds
// a default that passes into the create, and leaves none, or an unknown one,
// to the hub's own refusal. --allow-same-provider asks nothing.
func TestNewGuardOnTheHubsDefault(t *testing.T) {
	asked := 0
	def := func(id string) func() string {
		return func() string { asked++; return id }
	}
	markers(t, "", "", "")
	if got, err := newGuard(&newFlags{}, "", def("claude")); err != nil || got != "" || asked != 0 {
		t.Fatalf("no caller: %q %v, asked %d", got, err, asked)
	}
	markers(t, "", "1", "")
	if _, err := newGuard(&newFlags{}, "", def("claude")); err == nil || err.Error() != sameProvider("claude") {
		t.Fatalf("a claude default from Claude Code: %v", err)
	}
	if got, err := newGuard(&newFlags{}, "", def("cursor")); err != nil || got != "cursor" {
		t.Fatalf("a cursor default: %q %v, want it bound", got, err)
	}
	for _, id := range []string{"", "nosuch"} {
		if got, err := newGuard(&newFlags{}, "", def(id)); err != nil || got != "" {
			t.Fatalf("default %q: %q %v, want the hub's own answer", id, got, err)
		}
	}
	asked = 0
	if got, err := newGuard(&newFlags{allowSame: true}, "", def("claude")); err != nil || got != "" || asked != 0 {
		t.Fatalf("--allow-same-provider: %q %v, asked %d", got, err, asked)
	}
	if got, err := newGuard(&newFlags{}, "claude", def("cursor")); err == nil || got != "claude" || asked != 0 {
		t.Fatalf("--provider claude: %q %v, asked %d", got, err, asked)
	}
}

// guardRun is one craze new under the guard: its stdout, stderr and exit.
func guardRun(t *testing.T, argv ...string) (string, string, int) {
	t.Helper()
	return executeIn(append([]string{"new"}, argv...), noStdin())
}

// wantRefused checks a run was the guard's refusal of p: exit 2, its line on
// stderr, and under --json the usage object on stdout.
func wantRefused(t *testing.T, what, p, stdout, stderr string, code int, asJSON bool) {
	t.Helper()
	wantOut := ""
	if asJSON {
		msg, _ := json.Marshal(sameProvider(p))
		wantOut = `{"error":{"message":` + string(msg) + `,"kind":"usage","retryable":false}}` + "\n"
	}
	if code != 2 || stderr != sameProvider(p)+"\n" || stdout != wantOut {
		t.Fatalf("%s: exit %d, stdout %q, stderr %q; want the refusal of %s", what, code, stdout, stderr, p)
	}
}

// TestNewGuardEachPath (R2-6): each path of craze new refuses the caller's own
// provider as the target it would really start — --provider; plain new's
// configured default (no hub runs here); --wait's config.toml default, which
// a conflicting CRAZE_PROVIDER does not move; and --load's saved row — plain
// and under --json, before anything is dialled or spawned.
func TestNewGuardEachPath(t *testing.T) {
	_, ws := serveHome(t)
	isolateRunEnv(t)
	// Every agent binary points nowhere, so a path the guard failed to stop
	// fails to start rather than run a real agent.
	agents := "\n[agents]\ncursor = \"/nonexistent/agent\"\nclaude = \"/nonexistent/agent\"\n"
	writeFileT(t, filepath.Join(os.Getenv("CRAZE_HOME"), "config.toml"), "provider = \"claude\"\n"+agents)
	t.Setenv("CRAZE_PROVIDER", "cursor")
	markers(t, "", "1", "")
	for _, asJSON := range []bool{false, true} {
		j := func(argv ...string) []string {
			if asJSON {
				return append([]string{"--json"}, argv...)
			}
			return argv
		}
		stdout, stderr, code := guardRun(t, j("--provider", "claude", "-C", ws, "hi")...)
		wantRefused(t, "--provider claude", "claude", stdout, stderr, code, asJSON)
		stdout, stderr, code = guardRun(t, j("-C", ws, "hi")...)
		wantRefused(t, "the configured default, CRAZE_PROVIDER cursor", "claude", stdout, stderr, code, asJSON)
		stdout, stderr, code = guardRun(t, j("--wait", "-C", ws, "hi")...)
		wantRefused(t, "--wait on config.toml's default, CRAZE_PROVIDER cursor", "claude", stdout, stderr, code, asJSON)
		stdout, stderr, code = guardRun(t, j("--wait", "--provider", "claude", "-C", ws, "hi")...)
		wantRefused(t, "--wait --provider claude", "claude", stdout, stderr, code, asJSON)
	}

	// A refused --wait has not read its --prompt-file, and leaves it there.
	task := filepath.Join(ws, "task.md")
	writeFileT(t, task, "hi")
	stdout, stderr, code := guardRun(t, "--wait", "--provider", "claude", "-C", ws, "--prompt-file", task, "--remove-prompt-file")
	wantRefused(t, "--wait --prompt-file --remove-prompt-file", "claude", stdout, stderr, code, false)
	if _, err := os.Stat(task); err != nil {
		t.Fatalf("the refused run removed its prompt file: %v", err)
	}

	// From a cursor session, the cursor default refused and claude let by.
	writeFileT(t, filepath.Join(os.Getenv("CRAZE_HOME"), "config.toml"), "provider = \"cursor\"\n"+agents)
	t.Setenv("CRAZE_PROVIDER", "claude")
	markers(t, "cursor", "1", "")
	stdout, stderr, code = guardRun(t, "--wait", "-C", ws, "hi")
	wantRefused(t, "--wait on a cursor default from cursor", "cursor", stdout, stderr, code, false)
	stdout, stderr, code = guardRun(t, "-C", ws, "hi")
	wantRefused(t, "plain new on a cursor default from cursor", "cursor", stdout, stderr, code, false)

	// --load: the saved row's provider.
	writeSavedIndex(t, savedRow("0199c0de-aaaa-7000-8000-0000aaaa1111", "claude", "s-a", ws, "", time.Minute))
	markers(t, "", "1", "")
	stdout, stderr, code = loadIn(noStdin(), "aaaa1111")
	wantRefused(t, "--load of a claude row", "claude", stdout, stderr, code, false)
	stdout, stderr, code = loadIn(noStdin(), "aaaa1111", "--json")
	wantRefused(t, "--load --json of a claude row", "claude", stdout, stderr, code, true)
}

// TestNewGuardPasses: what the guard lets by goes on to the path's next step —
// here a failure past the guard, never the refusal: the hub a test binary
// cannot start (plain new, --load), or a -C that is no directory (--wait,
// which checks it after the guard). --allow-same-provider passes every path,
// and a cursor marker beside CLAUDECODE=1 may target claude.
func TestNewGuardPasses(t *testing.T) {
	_, ws := serveHome(t)
	isolateRunEnv(t)
	writeSavedIndex(t, savedRow("0199c0de-aaaa-7000-8000-0000aaaa1111", "claude", "s-a", ws, "", time.Minute))
	missing := filepath.Join(ws, "no-such-dir")
	noHub := regexp.MustCompile(`^craze new: hub: no hub in a test binary[^\n]*\n$`)
	for _, tc := range []struct {
		name        string
		session     string
		argv        []string
		wantWorkdir bool
	}{
		{"--allow-same-provider, plain", "", []string{"--allow-same-provider", "--provider", "claude", "-C", ws, "hi"}, false},
		{"--allow-same-provider, --wait", "", []string{"--wait", "--allow-same-provider", "--provider", "claude", "-C", missing, "hi"}, true},
		{"--allow-same-provider, --load", "", []string{"--load", "aaaa1111", "--allow-same-provider"}, false},
		{"a cursor session Claude Code started, plain", "cursor", []string{"--provider", "claude", "-C", ws, "hi"}, false},
		{"a cursor session Claude Code started, --wait", "cursor", []string{"--wait", "--provider", "claude", "-C", missing, "hi"}, true},
		{"a cursor session Claude Code started, --load", "cursor", []string{"--load", "aaaa1111"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			markers(t, tc.session, "1", "")
			stdout, stderr, code := guardRun(t, tc.argv...)
			if tc.wantWorkdir {
				if code != 1 || stderr != "craze new: "+missing+" is not a directory\n" {
					t.Fatalf("exit %d, stdout %q, stderr %q; want the directory's failure", code, stdout, stderr)
				}
				return
			}
			if code != 1 || !noHub.MatchString(stderr) {
				t.Fatalf("exit %d, stdout %q, stderr %q; want the hub's failure", code, stdout, stderr)
			}
		})
	}
	// craze send, wait and cancel are not guarded: they continue a session
	// already started, and take no override.
	root := NewRootCmd()
	for _, name := range []string{"send", "wait", "cancel"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil || cmd.Name() != name || cmd.Flags().Lookup(allowSameProviderFlag) != nil {
			t.Errorf("craze %s: %v, or it takes --%s", name, err, allowSameProviderFlag)
		}
	}
}
