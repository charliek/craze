package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness/websearch"
	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/version"
)

// The web search flags (plan 051 F.1–F.3, S1–S4): --web-search and
// --no-web-search on craze new --wait, craze prompt and craze serve; the
// refusal half of the one decision function in each of them; the
// web_search_not_offered reason; and the record on the session's row that
// every load applies.

const webOffRefusal = "--web-search cannot be honoured: config.toml turns web search off (websearch = false)"

// webConfig writes config.toml into this test's CRAZE_HOME.
func webConfig(t *testing.T, body string) {
	t.Helper()
	writeFileT(t, filepath.Join(os.Getenv("CRAZE_HOME"), "config.toml"), body)
}

// captureSpawns makes every host spawn fail, keeping the argv each was asked
// for.
func captureSpawns(t *testing.T) func() [][]string {
	t.Helper()
	var mu sync.Mutex
	var got [][]string
	prev := hostspawn.Command
	hostspawn.Command = func(argv []string) (*exec.Cmd, error) {
		mu.Lock()
		got = append(got, slices.Clone(argv))
		mu.Unlock()
		return nil, errors.New("no host in this test")
	}
	t.Cleanup(func() { hostspawn.Command = prev })
	return func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// ---- registration and usage ------------------------------------------------

// TestWebSearchFlagsAreOnTheirThreeCommandsOnly: craze new (its usage saying
// --wait), craze prompt and craze serve list both flags; the root command, whose
// sessions ask, lists neither.
func TestWebSearchFlagsAreOnTheirThreeCommandsOnly(t *testing.T) {
	crazeHome(t)
	if root := runHelp(t, "--help"); strings.Contains(root, "web-search") {
		t.Fatalf("craze --help lists a web search flag:\n%s", root)
	}
	for _, argv := range [][]string{{"new", "--help"}, {"prompt", "--help"}, {"serve", "--help"}} {
		help := runHelp(t, argv...)
		for _, flag := range []string{"  --web-search[", "  --no-web-search["} {
			if !strings.Contains(help, flag) {
				t.Errorf("craze %s --help lacks %s:\n%s", argv[0], flag, help)
			}
		}
		if argv[0] == "new" && !strings.Contains(help, "with --wait: offer a native session the web tools") {
			t.Errorf("craze new --help does not say --web-search is --wait's:\n%s", help)
		}
	}
}

// TestWebSearchFlagsUsageErrors (F.1): both together is a usage error on each
// command; either on plain craze new (no --wait), or with craze new --load, is
// too; and craze serve takes neither with --load or --continue — a loaded
// session keeps the choice it was created with. Each is exit 2, nothing
// spawned.
func TestWebSearchFlagsUsageErrors(t *testing.T) {
	serveHome(t)
	spawns := noSpawns(t)
	work := t.TempDir()
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"new", "--wait", "--provider", "native", "-C", work, "--web-search", "--no-web-search", "go"}, "craze new: --web-search and --no-web-search are mutually exclusive"},
		{[]string{"prompt", "--provider", "native", "--web-search", "--no-web-search", "go"}, "craze: --web-search and --no-web-search are mutually exclusive"},
		{[]string{"new", "--provider", "native", "--web-search", "go"}, "craze new: --web-search needs --wait"},
		{[]string{"new", "--provider", "native", "--no-web-search", "go"}, "craze new: --no-web-search needs --wait"},
		{[]string{"new", "--load", "abcd", "--web-search"}, "craze new: --load does not take --web-search: a reopened session keeps its own"},
		{[]string{"new", "--load", "abcd", "--no-web-search"}, "craze new: --load does not take --no-web-search: a reopened session keeps its own"},
	} {
		stdout, stderr, code := executeIn(tc.argv, noStdin())
		if code != 2 || stdout != "" || stderr != tc.want+"\n" {
			t.Errorf("%q: exit %d, %q %q; want 2, %q", tc.argv, code, stdout, stderr, tc.want)
		}
	}
	if n := spawns.Load(); n != 0 {
		t.Fatalf("%d hosts spawned", n)
	}

	seedIndexRow(t, sessions.Row{SessionID: "n-1", Provider: "native", CWD: absDir(work), CrazeID: "0199aaaa-bbbb-7ccc-8ddd-0000000000a1"})
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"--provider", "native", "--web-search", "--no-web-search"}, "craze: --web-search and --no-web-search are mutually exclusive"},
		{[]string{"--load", "0199aaaa-bbbb-7ccc-8ddd-0000000000a1", "--web-search"},
			"craze serve: --web-search is a new session's: a loaded session keeps the web search choice it was created with"},
		{[]string{"--continue", "--workspace", work, "--no-web-search"},
			"craze serve: --no-web-search is a new session's: a loaded session keeps the web search choice it was created with"},
	} {
		r := runServeIn(t, hostEnv{}, tc.argv...)
		if code, msg := exitCode(t, r.result(t, serveStep)); code != 2 || msg != tc.want {
			t.Errorf("craze serve %q: exit %d %q; want 2, %q", tc.argv, code, msg, tc.want)
		}
	}
}

// ---- craze new --wait's refusal (S1, S2) ------------------------------------

// TestNewWaitRefusesAWebSearchItCannotHonour: every refusal of --web-search —
// a provider that is not native (explicit, or config.toml's default),
// config.toml off, invalid, unparsable or unreadable, an invalid test override
// — is exit 1, kind config, detail {"reason":"web_search_not_offered"} with no
// model, before any host is spawned and before the prompt is read: the
// --prompt-file is there afterwards although --remove-prompt-file was given.
// It wins over a model refusal, and says no model notice.
func TestNewWaitRefusesAWebSearchItCannotHonour(t *testing.T) {
	const native = "web search is a native provider feature"
	for _, tc := range []struct {
		name     string
		config   string
		dir      bool // config.toml is a directory: it cannot be read
		override string
		args     []string
		why      string
	}{
		{name: "cursor", args: []string{"--provider", "cursor"}, why: native},
		{name: "the default provider, cursor", config: "provider = \"cursor\"\n", why: native},
		{name: "config off", config: "websearch = false\n", args: []string{"--provider", "native"}, why: "config.toml turns web search off (websearch = false)"},
		{name: "config off, the default provider", config: "provider = \"native\"\nwebsearch = false\n", why: "config.toml turns web search off (websearch = false)"},
		{name: "config invalid", config: "websearch = true\n", args: []string{"--provider", "native"}, why: "config.toml websearch is not one of its two forms"},
		{name: "config unparsable", config: "websearch = [\n", args: []string{"--provider", "native"}, why: "config.toml could not be parsed"},
		{name: "config unreadable", dir: true, args: []string{"--provider", "native"}, why: "config.toml could not be read"},
		{name: "override invalid", override: "http://example.com/mcp", args: []string{"--provider", "native"}, why: websearch.TestEndpointEnv + " is not a loopback http URL"},
		{name: "over a model refusal", config: "websearch = false\n", args: []string{"--provider", "native", "--model", "acme/sonic-1", "--effort", "minimal"},
			why: "config.toml turns web search off (websearch = false)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFixtureCatalog(t, true)
			spawns := noSpawns(t)
			switch {
			case tc.dir:
				if err := os.Mkdir(filepath.Join(os.Getenv("CRAZE_HOME"), "config.toml"), 0o700); err != nil {
					t.Fatal(err)
				}
			case tc.config != "":
				webConfig(t, tc.config)
			}
			if tc.override != "" {
				t.Setenv(websearch.TestEndpointEnv, tc.override)
			}
			task := filepath.Join(t.TempDir(), "task.md")
			writeFileT(t, task, "the task\n")
			argv := append([]string{"new", "--wait", "--json", "-C", t.TempDir(), "--web-search", "--prompt-file", task, "--remove-prompt-file"}, tc.args...)
			stdout, stderr, code := executeIn(argv, noStdin())
			var v struct {
				Error struct {
					Message   string          `json:"message"`
					Kind      string          `json:"kind"`
					Retryable bool            `json:"retryable"`
					Detail    json.RawMessage `json:"detail"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stdout), &v); err != nil || code != 1 {
				t.Fatalf("exit %d, %q %q", code, stdout, stderr)
			}
			if v.Error.Kind != string(errkind.Config) || v.Error.Retryable || string(v.Error.Detail) != `{"reason":"web_search_not_offered"}` ||
				!strings.HasPrefix(v.Error.Message, "--web-search cannot be honoured: "+tc.why) {
				t.Errorf("error = %+v (detail %s)", v.Error, v.Error.Detail)
			}
			if stderr != "craze new: "+v.Error.Message+"\n" || strings.Contains(stdout, "warnings") {
				t.Errorf("stderr = %q, stdout %s: want the message alone, no notice", stderr, stdout)
			}
			if b, err := os.ReadFile(task); err != nil || string(b) != "the task\n" {
				t.Errorf("the prompt file after the refusal: %q, %v", b, err)
			}
			if n := spawns.Load(); n != 0 {
				t.Errorf("%d hosts spawned", n)
			}
		})
	}
}

// TestNewWaitPassesItsWebSearchToTheHost: a --web-search nothing refuses, and
// --no-web-search on any provider over any config — never refused, a no-op
// off native — reach the host they spawn as its own flag (the tri-state
// through tuiFlags, serveSpec and hostspawn.Args); with neither, the host is
// given neither. A --web-search with no default provider is left to that
// error.
func TestNewWaitPassesItsWebSearchToTheHost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		args   []string
		want   []string
	}{
		{"--web-search, nothing in the home", "", []string{"--provider", "native", "--web-search"}, []string{"--web-search"}},
		{"--web-search over a config that is on", "[websearch]\nprovider = \"exa\"\n", []string{"--provider", "native", "--web-search"}, []string{"--web-search"}},
		{"--no-web-search over a config that is on", "[websearch]\nprovider = \"exa\"\n", []string{"--provider", "native", "--no-web-search"}, []string{"--no-web-search"}},
		{"--no-web-search over a config that is off", "websearch = false\n", []string{"--provider", "native", "--no-web-search"}, []string{"--no-web-search"}},
		{"--no-web-search on cursor", "websearch = false\n", []string{"--provider", "cursor", "--no-web-search"}, []string{"--no-web-search"}},
		{"neither", "", []string{"--provider", "native"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFixtureCatalog(t, true)
			spawned := captureSpawns(t)
			if tc.config != "" {
				webConfig(t, tc.config)
			}
			stdout, _, code := executeIn(append(append([]string{"new", "--wait", "--json", "-C", t.TempDir()}, tc.args...), "go"), noStdin())
			if code != 1 || !strings.Contains(stdout, `"kind":"start_failed"`) {
				t.Fatalf("exit %d, %s: want the spawn's own failure", code, stdout)
			}
			argvs := spawned()
			if len(argvs) != 1 {
				t.Fatalf("%d spawns", len(argvs))
			}
			var got []string
			for _, a := range argvs[0] {
				if a == "--web-search" || a == "--no-web-search" {
					got = append(got, a)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("the host was given %q, want %q: %q", got, tc.want, argvs[0])
			}
		})
	}

	withFixtureCatalog(t, true)
	spawns := noSpawns(t)
	stdout, stderr, code := executeIn([]string{"new", "--wait", "-C", t.TempDir(), "--web-search", "go"}, noStdin())
	if code != 1 || !strings.Contains(stderr, "no default provider yet") || spawns.Load() != 0 {
		t.Fatalf("no default provider: exit %d, %q %q", code, stdout, stderr)
	}
}

// ---- craze prompt (S4) -----------------------------------------------------

// TestPromptRefusesAWebSearchItCannotHonour: exit 1 before any session is
// built or provider called — the message on stderr and, under --json, the one
// startup error event, with no kind or detail; nothing on stdout without
// --json. A non-native provider is refused too; --no-web-search never is.
func TestPromptRefusesAWebSearchItCannotHonour(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		args         []string
		why          string
	}{
		{"config off", "websearch = false\n", []string{"--provider", "native"}, "config.toml turns web search off (websearch = false)"},
		{"cursor", "", []string{"--provider", "cursor", "--agent-bin", "/nonexistent/agent"}, "web search is a native provider feature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crazeHome(t)
			if tc.config != "" {
				webConfig(t, tc.config)
			}
			want := "craze prompt: --web-search cannot be honoured: " + tc.why
			stdout, stderr, code := executeIn(append(append([]string{"prompt", "--json", "--web-search"}, tc.args...), "hi"), noStdin())
			var ev map[string]any
			if code != 1 || strings.Count(stdout, "\n") != 1 || json.Unmarshal([]byte(stdout), &ev) != nil {
				t.Fatalf("exit %d, %q %q", code, stdout, stderr)
			}
			if len(ev) != 2 || ev["type"] != "error" || ev["message"] != want || stderr != want+"\n" {
				t.Fatalf("the event %v, stderr %q; want %q", ev, stderr, want)
			}
			stdout, stderr, code = executeIn(append(append([]string{"prompt", "--web-search"}, tc.args...), "hi"), noStdin())
			if code != 1 || stdout != "" || stderr != want+"\n" {
				t.Fatalf("plain: exit %d, %q %q", code, stdout, stderr)
			}
		})
	}

	crazeHome(t)
	webConfig(t, "websearch = false\n")
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	stdout, stderr, code := executeIn([]string{"prompt", "--provider", "cursor", "--agent-bin", fakeAgentPath(t), "--no-web-search", "hi"}, noStdin())
	if code != 0 || strings.Contains(stderr, "web") {
		t.Fatalf("--no-web-search on cursor: exit %d, %q %q", code, stdout, stderr)
	}
}

// TestPromptHandsItsWebSearchToTheSession: craze prompt's options carry the
// flag as given now, unattended (Interactive false), for the native Open.
func TestPromptHandsItsWebSearchToTheSession(t *testing.T) {
	crazeHome(t)
	for _, tc := range []struct {
		on, off bool
		want    agent.WebFlag
	}{{true, false, agent.WebFlagOn}, {false, true, agent.WebFlagOff}, {false, false, agent.WebFlagAbsent}} {
		o := &promptOpts{webSearch: tc.on, noWebSearch: tc.off, stderr: io.Discard}
		got := o.agentOptions(t.TempDir(), headlessApproval(true), nil)
		if got.WebFlag != tc.want || got.WebFlagOrigin != agent.WebFlagGiven || got.Interactive {
			t.Errorf("on %t off %t: flag %v origin %v interactive %t", tc.on, tc.off, got.WebFlag, got.WebFlagOrigin, got.Interactive)
		}
	}
}

// ---- the session's options (F.3) -------------------------------------------

// TestSessionOptionsTakeTheWebSearchChoice: a new session's flag is given now;
// a load's is its row's, recorded; a row with none, and a new session given
// none, have none.
func TestSessionOptionsTakeTheWebSearchChoice(t *testing.T) {
	crazeHome(t)
	p := agent.NativeProvider()
	for _, tc := range []struct {
		name   string
		f      tuiFlags
		row    sessions.Row
		flag   agent.WebFlag
		origin agent.WebFlagOrigin
	}{
		{"new, --web-search", tuiFlags{webSearch: true}, sessions.Row{}, agent.WebFlagOn, agent.WebFlagGiven},
		{"new, --no-web-search", tuiFlags{noWebSearch: true}, sessions.Row{}, agent.WebFlagOff, agent.WebFlagGiven},
		{"new, neither", tuiFlags{}, sessions.Row{}, agent.WebFlagAbsent, agent.WebFlagRecorded},
		{"load, recorded on", tuiFlags{}, sessions.Row{SessionID: "s", WebSearch: "on"}, agent.WebFlagOn, agent.WebFlagRecorded},
		{"load, recorded off", tuiFlags{}, sessions.Row{SessionID: "s", WebSearch: "off"}, agent.WebFlagOff, agent.WebFlagRecorded},
		{"load, none recorded", tuiFlags{}, sessions.Row{SessionID: "s"}, agent.WebFlagAbsent, agent.WebFlagRecorded},
	} {
		o := sessionOptions(&tc.f, t.TempDir(), "", io.Discard, io.Discard, nil, p, p, tc.row)
		if o.WebFlag != tc.flag || tc.flag != agent.WebFlagAbsent && o.WebFlagOrigin != tc.origin {
			t.Errorf("%s: flag %v origin %v, want %v %v", tc.name, o.WebFlag, o.WebFlagOrigin, tc.flag, tc.origin)
		}
	}
	// A reload's and a reopen's argv carry no web flag: the host reads the row.
	row := sessions.Row{SessionID: "s", Provider: "native", CWD: "/w", CrazeID: "0199aaaa-bbbb-7ccc-8ddd-0000000000a9", WebSearch: "on",
		Unattended: &sessions.Unattended{PermissionMode: sessions.PermissionBypass}}
	flags := reloadFlags(row)
	if argv := hostspawn.Args(serveSpec(&flags, loadArg(row), "0123456789ab", "/l")); slices.ContainsFunc(argv, func(a string) bool { return a == "--web-search" || a == "--no-web-search" }) {
		t.Fatalf("a reload's argv carries a web flag: %q", argv)
	}
}

// ---- craze serve (S3, F.3) ---------------------------------------------------

// TestServeRefusesAWebSearchItCannotHonour: before the session's id is minted
// (the pre-mint hook, refuse) a non-native provider or a config that is off
// refuses --web-search, exit 1, as a refusal of the session asked for — the
// ready line says refused — nothing built, no host left.
func TestServeRefusesAWebSearchItCannotHonour(t *testing.T) {
	env, ws := serveHome(t)
	built := recordBuilt(t)
	for _, tc := range []struct {
		name, config string
		argv         []string
		want         string
	}{
		{"cursor", "", []string{"--provider", "cursor", "--agent-bin", fakeAgentPath(t)}, "craze: --web-search cannot be honoured: web search is a native provider feature"},
		{"config off", "websearch = false\n", []string{"--provider", "native"}, "craze: " + webOffRefusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.config != "" {
				webConfig(t, tc.config)
			}
			r, rd := runServeReady(t, false, append([]string{"--workspace", ws, "--web-search"}, tc.argv...)...)
			line := readyFrom(t, rd)
			code, msg := exitCode(t, r.result(t, serveStep))
			if line.OK || !line.Refused || line.Error != msg || code != 1 || msg != tc.want {
				t.Fatalf("the line %+v; exit %d %q", line, code, msg)
			}
		})
	}
	select {
	case o := <-built:
		t.Fatalf("a refused run built a session: %+v", o)
	default:
	}
	if entries, _ := rundir.Hosts(env); len(entries) != 0 {
		t.Fatalf("a refusal left hosts behind: %+v", entries)
	}
}

// TestAnAttendedServeRecordsItsWebSearch is F.3's case of a host with a flag
// and no --unattended: its row records the choice and is not marked
// unattended; reloaded (craze serve --load, as the session list, the resume
// picker and craze send to a person's session spawn it), the session takes the
// recorded choice, is interactive, and the row keeps both as they were.
func TestAnAttendedServeRecordsItsWebSearch(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	built := recordBuilt(t)
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--provider", "cursor", "--no-web-search")
	if o := builtOptions(t, built); o.WebFlag != agent.WebFlagOff || o.WebFlagOrigin != agent.WebFlagGiven || !o.Interactive {
		t.Fatalf("the new session's options: flag %v origin %v interactive %t", o.WebFlag, o.WebFlagOrigin, o.Interactive)
	}
	e := r.waitServing(t, env, true)
	promptUnattached(t, e, "a task")
	waitLastTurn(t, e)
	id := e.CrazeSessionID
	if row := waitIndexRowByID(t, id); row.WebSearch != "off" || row.Unattended != nil {
		t.Fatalf("the row: webSearch %q, unattended %+v", row.WebSearch, row.Unattended)
	}
	r.sigs <- syscall.SIGTERM
	if err := r.result(t, serveStep); err != nil {
		t.Fatal(err)
	}
	assertHostGone(t, env, e)

	t.Setenv("CRAZE_FAKE_SCRIPT", "load")
	seeded := indexRowByID(t, id).UpdatedAt
	r = runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--load", id)
	if o := builtOptions(t, built); o.WebFlag != agent.WebFlagOff || o.WebFlagOrigin != agent.WebFlagRecorded || !o.Interactive || o.Approval != nil {
		t.Fatalf("the reload's options: flag %v origin %v interactive %t approval %+v", o.WebFlag, o.WebFlagOrigin, o.Interactive, o.Approval)
	}
	e = r.waitServing(t, env, true)
	promptUnattached(t, e, "again")
	waitLastTurn(t, e)
	waitFor(t, "the reload's write of the row", func() bool { return indexRowByID(t, id).UpdatedAt.After(seeded) })
	if row := indexRowByID(t, id); row.WebSearch != "off" || row.Unattended != nil {
		t.Fatalf("the reloaded row: webSearch %q, unattended %+v", row.WebSearch, row.Unattended)
	}
}

// TestEveryLoadTakesTheRecordedWebSearch: a native session's recorded choice,
// on and off, reaches the session on every load path's host — craze serve
// --load attended (the session list, the resume picker, a hub reopen or craze
// send of a person's session), --load --unattended (craze send's reload and a
// hub reopen of a sub-agent's session) and --continue — as recorded, with the
// final interactivity. A recorded on that config.toml has since turned off is
// refused for the unattended load, as a refusal of the session (the ready line
// says so) with nothing built and the claim given back; a person's load is
// built all the same, and opens without web tools (the native Open's own
// decision, internal/agent).
func TestEveryLoadTakesTheRecordedWebSearch(t *testing.T) {
	env, _ := serveHome(t)
	built := recordBuilt(t)
	for i, rec := range []string{"on", "off"} {
		ws := t.TempDir()
		id := "0199aaaa-bbbb-7ccc-8ddd-0000000000b" + strconv.Itoa(i)
		seedIndexRow(t, sessions.Row{SessionID: "n-" + rec, Provider: "native", CWD: absDir(ws), CrazeID: id, WebSearch: rec})
		for _, route := range []struct {
			name     string
			argv     []string
			attended bool
		}{
			{"--load", []string{"--load", id}, true},
			{"--load --unattended", []string{"--load", id, "--unattended"}, false},
			{"--continue", []string{"--continue", "--workspace", ws}, true},
		} {
			t.Run(rec+", "+route.name, func(t *testing.T) {
				r := runServeIn(t, hostEnv{}, route.argv...)
				o := builtOptions(t, built)
				if o.WebFlag != agent.ParseWebFlag(rec) || o.WebFlagOrigin != agent.WebFlagRecorded || o.Interactive != route.attended {
					t.Fatalf("built flag %v origin %v interactive %t", o.WebFlag, o.WebFlagOrigin, o.Interactive)
				}
				r.sigs <- syscall.SIGTERM
				if err := r.result(t, serveStep); err != nil {
					t.Fatal(err)
				}
			})
		}
	}

	webConfig(t, "websearch = false\n")
	ws := t.TempDir()
	const id = "0199aaaa-bbbb-7ccc-8ddd-0000000000c1"
	seedIndexRow(t, sessions.Row{SessionID: "n-c1", Provider: "native", CWD: absDir(ws), CrazeID: id, WebSearch: "on"})
	r, rd := runServeReady(t, false, "--load", id, "--unattended")
	line := readyFrom(t, rd)
	code, msg := exitCode(t, r.result(t, serveStep))
	want := "craze serve: this session was created with --web-search, which cannot be honoured: config.toml turns web search off (websearch = false)"
	if line.OK || !line.Refused || line.Error != msg || code != 1 || msg != want {
		t.Fatalf("an unattended reload of an on config.toml has turned off: the line %+v; exit %d %q", line, code, msg)
	}
	select {
	case o := <-built:
		t.Fatalf("the refused reload built a session: %+v", o)
	default:
	}
	if p := filepath.Join(env.Home, ".cache", "craze", "locks", id+".lock"); fileExists(p) && lockHeldBy(t, p, os.Getpid()) {
		t.Fatal("the refused reload still holds the session")
	}
	r2 := runServeIn(t, hostEnv{}, "--load", id)
	if o := builtOptions(t, built); o.WebFlag != agent.WebFlagOn || o.WebFlagOrigin != agent.WebFlagRecorded || !o.Interactive {
		t.Fatalf("a person's resume: flag %v origin %v interactive %t", o.WebFlag, o.WebFlagOrigin, o.Interactive)
	}
	r2.sigs <- syscall.SIGTERM
	if err := r2.result(t, serveStep); err != nil {
		t.Fatal(err)
	}
}

// evictIndex empties the session index under its own lock, as the 500-row
// cap's eviction would drop a row (sessions.Store.Upsert's evictOverflow,
// which protects no live or claimed session): whatever a host writes after it
// is a row created afresh.
func evictIndex(t *testing.T) {
	t.Helper()
	unlock, err := atomicfile.Lock(indexPath(t) + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := os.WriteFile(indexPath(t), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAReloadedHostKeepsTheRecordedWebSearchThroughEviction: a reload's host
// (craze serve --load, no web flag on its command line) writes the choice it
// loaded on every row it writes, so a row evicted while the session runs — or
// before its first write — comes back carrying it. Without that the record
// was lost for good, and the next load followed the home's settings (C6
// review F2).
func TestAReloadedHostKeepsTheRecordedWebSearchThroughEviction(t *testing.T) {
	for i, rec := range []string{"off", "on"} {
		t.Run(rec, func(t *testing.T) {
			env, ws := serveHome(t)
			t.Setenv("CRAZE_FAKE_SCRIPT", "load")
			id := "0199aaaa-bbbb-7ccc-8ddd-0000000000d" + strconv.Itoa(i)
			seedIndexRow(t, sessions.Row{SessionID: "ev-" + rec, Provider: "cursor", CWD: absDir(ws), CrazeID: id, WebSearch: rec})
			r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--load", id)
			e := r.waitServing(t, env, true)
			evictIndex(t)
			promptUnattached(t, e, "after the eviction")
			waitLastTurn(t, e)
			if row := waitIndexRowByID(t, id); row.WebSearch != rec {
				t.Fatalf("the row recreated after the eviction: webSearch %q, want %q", row.WebSearch, rec)
			}
			r.sigs <- syscall.SIGTERM
			if err := r.result(t, serveStep); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestTheInProcessContinueCarriesTheRecordedWebSearch: the in-process TUI's
// --continue (CRAZE_DETACH off) hands the TUI the row's recorded choice
// (tui.Config.WebSearch) beside the session that applies it, so the engine's
// index writer carries it on every row it writes (C6 review F2; the TUI's half
// is internal/tui's TestTheRecordedWebSearchTravelsEveryLoadPath).
func TestTheInProcessContinueCarriesTheRecordedWebSearch(t *testing.T) {
	for _, rec := range []string{"off", "on"} {
		t.Run(rec, func(t *testing.T) {
			indexHome(t)
			ws := t.TempDir()
			seedRow(t, sessions.Row{SessionID: "c-" + rec, Provider: "cursor", CWD: absDir(ws), WebSearch: rec}, time.Minute)
			cfg, built, err := runResolveLoad(t, ws, "--continue")
			if err != nil {
				t.Fatal(err)
			}
			if len(built) != 1 || built[0].WebFlag != agent.ParseWebFlag(rec) || built[0].WebFlagOrigin != agent.WebFlagRecorded {
				t.Fatalf("built %+v", built)
			}
			if cfg.WebSearch != rec || cfg.CrazeSessionID == "" {
				t.Fatalf("the TUI's config: webSearch %q, craze id %q", cfg.WebSearch, cfg.CrazeSessionID)
			}
		})
	}
}

// TestNewLoadOfARecordedWebSearchAgainstAnOlderHub (C6 review F1): a running
// hub whose hello does not say reopenWebSearch — one from before the record,
// whose hosts (its own craze's serve --load) would open the session without
// it — is LacksError's words for craze new --load of a session whose row
// records a web search choice, with the hub's pid, exit 1, and no
// session.reopen sent; never another hub spawned beside it. The negative
// control, a session with no record on the same kind of hub, is
// internal/hub's TestEnsureReportsAHubThatCannotReopenAWebSearch.
func TestNewLoadOfARecordedWebSearchAgainstAnOlderHub(t *testing.T) {
	for i, rec := range []string{"off", "on"} {
		t.Run(rec, func(t *testing.T) {
			_, env := providersHubMachine(t)
			row := savedRow("0199c0de-aaaa-7000-8000-0000aaaa222"+strconv.Itoa(i), "cursor", "s-w", env.Home, "", time.Minute)
			row.WebSearch = rec
			writeSavedIndex(t, row)
			creates := hubCreates()
			creates.WebSearch = false
			reopen := creates.Reopen
			creates.Reopen = func(id string, logf func(string, ...any)) (hub.Reopening, *protocol.Error) {
				t.Errorf("a hub that does not apply the record was asked to reopen %s", id)
				return reopen(id, logf)
			}
			inProcessHub(t, env, creates)
			prev := hub.Command
			hub.Command = func([]string) (*exec.Cmd, error) {
				t.Error("craze new --load spawned a hub beside the running one")
				return nil, errors.New("no hub is spawned here")
			}
			t.Cleanup(func() { hub.Command = prev })
			stdout, stderr, code := loadIn(noStdin(), "aaaa222"+strconv.Itoa(i))
			want := fmt.Sprintf("craze new: this hub (craze %s) cannot reopen a session created with --web-search or --no-web-search; "+
				"it exits once no session runs and no client is connected, or stop it now with kill %d (no session ends)\n", version.Version, os.Getpid())
			if code != 1 || stdout != "" || stderr != want {
				t.Fatalf("craze new --load of a recorded %s on an older hub: exit %d, stdout %q, stderr %q; want %q", rec, code, stdout, stderr, want)
			}
		})
	}
}

// TestNewLoadOfARecordedWebSearchThroughThisHub is the positive control of
// TestNewLoadOfARecordedWebSearchAgainstAnOlderHub: this craze's hub says
// reopenWebSearch, so a session that records a choice is reopened through it,
// in a host the hub starts, and the row keeps the choice.
func TestNewLoadOfARecordedWebSearchThroughThisHub(t *testing.T) {
	env, ws := reopenMachine(t, "load")
	const id = "0199c0de-feed-7000-8000-00000000fee1"
	seedIndexRow(t, sessions.Row{SessionID: "fake-session-w", Provider: "cursor", CWD: ws, CrazeID: id, WebSearch: "off"})
	stdout, stderr, code := loadIn(noStdin(), "0000fee1")
	if code != 0 || stderr != "" || stdout != "reopened 0000fee1 in "+psTilde(env.Home, ws)+"\n" {
		t.Fatalf("craze new --load: exit %d, %q %q (hub log: %s)", code, stdout, stderr, hubLog(env)())
	}
	if hosts := serving(t, env, id); len(hosts) != 1 {
		t.Fatalf("the hosts serving the reopened session: %+v", hosts)
	}
	if row := indexRowByID(t, id); row.WebSearch != "off" {
		t.Fatalf("the reopened session's row: webSearch %q", row.WebSearch)
	}
}

// ---- the reason set (S1) ---------------------------------------------------

// TestCLIReferenceNamesEveryReason: cli.md names every detail.reason the CLI
// declares (the reason* constants of modelcheck.go), read from the source so a
// new one cannot be added without its documentation; the drift tests check
// JSON keys only, not their values.
func TestCLIReferenceNamesEveryReason(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "modelcheck.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "reason") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s is not a string literal", name.Name)
				}
				v, _ := strconv.Unquote(lit.Value)
				reasons = append(reasons, v)
			}
		}
	}
	if len(reasons) < 5 || !slices.Contains(reasons, reasonWebSearchNotOffered) {
		t.Fatalf("read %q from modelcheck.go: the test reads nothing it should", reasons)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reasons {
		if !strings.Contains(string(doc), "`"+r+"`") {
			t.Errorf("docs/reference/cli.md does not name the reason `%s`", r)
		}
	}
}
