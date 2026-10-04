package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rivo/uniseg"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/rundir"
)

// The provider availability check (plan 036 §3.1, A1). The table test drives
// availability over injected inputs, one case per rule of §3.1's table; the
// native cases run the real Load and StartModel on scratch native
// directories; the builders' cases resolve real files on a scratch PATH and
// in a scratch CRAZE_HOME. No case reads the owner's ~/.craze, PATH or
// session: TestMain pins the login session to the GUI's, and a case that
// needs another sets it.

// availConfig is the config path the injected cases' fixes name.
const availConfig = "/cfg/config.toml"

// availEntry is a providerAvail as the cases compare it: by provider id.
type availEntry struct {
	ID, State, Reason, Fix string
}

func availEntries(as []providerAvail) []availEntry {
	out := make([]availEntry, 0, len(as))
	for _, a := range as {
		out = append(out, availEntry{a.P.Name(), string(a.State), a.Reason, a.Fix})
	}
	return out
}

// The missing-binary texts the cases expect (plan 036 §3.1, X2).
var (
	cursorOnPath = availEntry{"cursor", "unavailable", "cursor-agent not found on PATH",
		"install cursor-agent, or set [agents].cursor in " + availConfig}
	grokOnPath = availEntry{"grok", "unavailable", "grok not found on PATH",
		"install grok, or set [agents].grok in " + availConfig}
	cursorReady  = availEntry{"cursor", "ready", "", ""}
	grokReady    = availEntry{"grok", "ready", "", ""}
	gxReady      = availEntry{"gx", "ready", "", ""}
	nativeReady  = availEntry{"native", "ready", "", ""}
	cursorNotGUI = availEntry{"cursor", "unavailable",
		"this craze runs outside the macOS login session (over ssh), where cursor may not reach the login keychain",
		"run craze from a terminal on the Mac"}
)

// resolvesAs is an injected resolve: each provider's resolution by id, and
// a PATH hit for any provider not named.
func resolvesAs(m map[string]binResolution) func(agent.Provider) binResolution {
	return func(p agent.Provider) binResolution {
		if r, ok := m[p.Name()]; ok {
			return r
		}
		return binResolution{OK: true, Source: binFromPath}
	}
}

// session is an injected login session.
func session(gui, known bool) func() (uint32, bool, bool) {
	return func() (uint32, bool, bool) { return 0, gui, known }
}

// nativeAs is an injected native check.
func nativeAs(state availState, reason, fix string) func() (availState, string, string) {
	return func() (availState, string, string) { return state, reason, fix }
}

// TestAvailabilityRules (plan 036 §3.1, A1): every row of §3.1's table, over
// injected inputs — cursor and grok missing on PATH and through each override,
// found each way; gx missing (left out, or unavailable as the TUI picker's
// default) and found; cursor outside the login session (unavailable, with the
// TUI's or CLI's fix, and the hub's naming its pid), in a session that is not
// known (ready), and missing while outside (the missing binary wins); native's
// state as its check gives it. The rows come back in rows' order.
func TestAvailabilityRules(t *testing.T) {
	miss := func(source, path string) binResolution { return binResolution{Source: source, Path: path} }
	hit := func(source, path string) binResolution { return binResolution{OK: true, Source: source, Path: path} }
	gui, notGUI, unknown := session(true, true), session(false, true), session(false, false)
	native := nativeAs(availReady, "", "")
	for _, tc := range []struct {
		name          string
		resolve       map[string]binResolution
		gui           func() (uint32, bool, bool)
		hubPID        int
		pickerDefault string
		native        func() (availState, string, string)
		want          []availEntry
	}{
		{
			name: "everything found on PATH",
			want: []availEntry{cursorReady, grokReady, gxReady, nativeReady},
		},
		{
			name:    "cursor and grok missing on PATH, gx left out",
			resolve: map[string]binResolution{"cursor": miss(binFromPath, ""), "grok": miss(binFromPath, ""), "gx": miss(binFromPath, "")},
			want:    []availEntry{cursorOnPath, grokOnPath, nativeReady},
		},
		{
			name:    "missing through --agent-bin",
			resolve: map[string]binResolution{"cursor": miss(binFromFlag, "/opt/x/cursor-agent")},
			want: []availEntry{
				{"cursor", "unavailable", "--agent-bin /opt/x/cursor-agent not found", "point --agent-bin at an existing binary"},
				grokReady, gxReady, nativeReady,
			},
		},
		{
			name:    "missing through CRAZE_AGENT_BIN",
			resolve: map[string]binResolution{"grok": miss(binFromEnv, "/opt/x/grok")},
			want: []availEntry{
				cursorReady,
				{"grok", "unavailable", "CRAZE_AGENT_BIN /opt/x/grok not found", "point CRAZE_AGENT_BIN at an existing binary, or unset it"},
				gxReady, nativeReady,
			},
		},
		{
			name:    "missing through [agents]",
			resolve: map[string]binResolution{"cursor": miss(binFromAgents, "/opt/x/cursor-agent")},
			want: []availEntry{
				{"cursor", "unavailable", "[agents].cursor /opt/x/cursor-agent not found",
					"point [agents].cursor at an existing binary in " + availConfig},
				grokReady, gxReady, nativeReady,
			},
		},
		{
			name: "found through each override",
			resolve: map[string]binResolution{"cursor": hit(binFromFlag, "/a/cursor-agent"), "grok": hit(binFromEnv, "/a/grok"),
				"gx": hit(binFromAgents, "/a/gx")},
			want: []availEntry{cursorReady, grokReady, gxReady, nativeReady},
		},
		{
			name:          "a missing gx kept as the TUI picker's default",
			resolve:       map[string]binResolution{"gx": miss(binFromPath, "")},
			pickerDefault: "gx",
			want: []availEntry{cursorReady, grokReady,
				{"gx", "unavailable", "gx not found", "install gx, or set [agents].gx"}, nativeReady},
		},
		{
			name:          "a missing gx default through [agents] names the knob",
			resolve:       map[string]binResolution{"gx": miss(binFromAgents, "/opt/gx")},
			pickerDefault: "gx",
			want: []availEntry{cursorReady, grokReady,
				{"gx", "unavailable", "[agents].gx /opt/gx not found", "point [agents].gx at an existing binary in " + availConfig},
				nativeReady},
		},
		{
			name:          "another default leaves a missing gx out",
			resolve:       map[string]binResolution{"gx": miss(binFromPath, "")},
			pickerDefault: "grok",
			want:          []availEntry{cursorReady, grokReady, nativeReady},
		},
		{
			name: "cursor outside the login session, the TUI's or CLI's answer",
			gui:  notGUI,
			want: []availEntry{cursorNotGUI, grokReady, gxReady, nativeReady},
		},
		{
			name:   "cursor outside the login session, the hub's answer",
			gui:    notGUI,
			hubPID: 4242,
			want: []availEntry{
				{"cursor", "unavailable", cursorNotGUI.Reason, `kill 4242 (no session ends), then run "craze ps" in a terminal on the Mac`},
				grokReady, gxReady, nativeReady,
			},
		},
		{
			name: "a login session that is not known marks nothing",
			gui:  unknown,
			want: []availEntry{cursorReady, grokReady, gxReady, nativeReady},
		},
		{
			name:    "missing while outside the login session: the missing binary wins",
			resolve: map[string]binResolution{"cursor": miss(binFromPath, "")},
			gui:     notGUI,
			hubPID:  4242,
			want:    []availEntry{cursorOnPath, grokReady, gxReady, nativeReady},
		},
		{
			name:   "native needs setup",
			native: nativeAs(availNeedsSetup, nativeNoKeyReason, nativeNoKeyFix),
			want:   []availEntry{cursorReady, grokReady, gxReady, {"native", "needs_setup", nativeNoKeyReason, nativeNoKeyFix}},
		},
		{
			name:   "native unavailable",
			native: nativeAs(availUnavailable, "models.toml could not be read", nativeUnreadFix),
			want: []availEntry{cursorReady, grokReady, gxReady,
				{"native", "unavailable", "models.toml could not be read", nativeUnreadFix}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := availInputs{
				resolve:       resolvesAs(tc.resolve),
				native:        native,
				gui:           gui,
				hubPID:        tc.hubPID,
				configPath:    availConfig,
				pickerDefault: tc.pickerDefault,
			}
			if tc.gui != nil {
				in.gui = tc.gui
			}
			if tc.native != nil {
				in.native = tc.native
			}
			got := availEntries(availability(in, agent.Providers()))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("availability:\n%+v\nwant:\n%+v", got, tc.want)
			}
		})
	}
}

// TestAvailabilityCapsReasonAndFix (plan 036 §3.1, decision 7): a reason and
// a fix pass through engine.RowLine — one line, at most RowTextCells cells,
// an ellipsis ending one that was cut — so a very long [agents] path or
// config path cannot make a row of any length, and a newline in one cannot
// make two.
func TestAvailabilityCapsReasonAndFix(t *testing.T) {
	long := "/" + strings.Repeat("a/", 150) + "cursor-agent"
	in := availInputs{
		resolve:    resolvesAs(map[string]binResolution{"cursor": {Source: binFromAgents, Path: long}}),
		native:     nativeAs(availUnavailable, "first line\nsecond line", nativeUnreadFix),
		gui:        session(true, true),
		configPath: "/" + strings.Repeat("c/", 150) + "config.toml",
	}
	got := availability(in, agent.Providers())
	cursor, native := got[0], got[len(got)-1]
	for _, s := range []string{cursor.Reason, cursor.Fix} {
		if w := uniseg.StringWidth(s); w > engine.RowTextCells || !strings.HasSuffix(s, "…") {
			t.Fatalf("%q: %d cells, want at most %d and cut", s, w, engine.RowTextCells)
		}
	}
	if !strings.HasPrefix(cursor.Reason, "[agents].cursor /a/a/") {
		t.Fatalf("the cut reason %q does not start with the knob and the path", cursor.Reason)
	}
	if native.Reason != "first line" {
		t.Fatalf("native's reason %q, want its first line alone", native.Reason)
	}
}

// writeNativeFile writes name in the native directory dir, making it.
func writeNativeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// availSecretKey is an unknown key in providers.toml that looks like a pasted
// secret: the strict decode names it in its error (modeltable's unknownKey),
// and the availability reason must not.
const availSecretKey = "sk-or-v1-availcanary0123456789"

// The ChatGPT sign-in the plan-usage cases lay down, as internal/chatgptauth
// leaves it (modeltable's discovered_test.go): a registration, a token file
// with no token in it, and the account's model list, which holds one of the
// catalog's start models.
const (
	availPlanSubject = "user-subject-avail"
	availPlanClient  = "app_client-avail"
)

func signedInNative(t *testing.T, dir string, planUsage bool) {
	t.Helper()
	writeNativeFile(t, dir, filepath.Join(modeltable.ChatGPTAuthDir, modeltable.ChatGPTClientFile),
		fmt.Sprintf(`{"client_id":%q,"subject":%q,"email":"someone@example.com","plan_usage":%v,"notice_shown":true}`,
			availPlanClient, availPlanSubject, planUsage))
	writeNativeFile(t, dir, filepath.Join(modeltable.ChatGPTAuthDir, modeltable.ChatGPTTokenFile), `{"placeholder":"no token here"}`)
	writeNativeFile(t, dir, modeltable.ChatGPTModelsFile, fmt.Sprintf(`{"version":1,"subject":%q,"client_id":%q,"models_etag":"e1","fetched_at":"2026-10-01T00:00:00Z","models":[
{"slug":"gpt-6.1-sol","display_name":"GPT-6.1 Sol","context_window":272000,"efforts":["low","medium","high"],"default_effort":"medium","input_modalities":["text"],"priority":0,"parallel_tool_calls":true}
]}`, availPlanSubject, availPlanClient))
}

// TestNativeAvailability (plan 036 §3.1, decision 9, A1): native's state
// through the real modeltable.Load and StartModel, on scratch native
// directories — a funded key (stored, or in the environment) and a ChatGPT
// sign-in with plan usage are ready; no key, and a sign-in with plan usage
// off, need setup; a providers.toml or a models.toml that does not load — an
// unknown key, a value of the wrong type, a syntax error — is unavailable,
// naming the file and nothing of the error; no directory at all is
// unavailable.
func TestNativeAvailability(t *testing.T) {
	noEnv := func(string) string { return "" }
	// writes is a setup that writes body to name in the native directory.
	writes := func(name, body string) func(*testing.T, string) {
		return func(t *testing.T, dir string) { writeNativeFile(t, dir, name, body) }
	}
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, dir string) // nil: an empty directory
		noDir  bool                           // no directory at all: dir ""
		getenv func(string) string
		want   availEntry
	}{
		{
			name:  "a funded stored key",
			setup: writes(modeltable.ProvidersFile, "version = 1\n\n[providers.openrouter]\napi_key = \"sk-avail-canary-0001\"\n"),
			want:  nativeReady,
		},
		{
			name: "a funded key in the environment",
			getenv: func(k string) string {
				if k == "FIREWORKS_API_KEY" {
					return "fw-avail-canary-0002"
				}
				return ""
			},
			want: nativeReady,
		},
		{
			name: "no key",
			want: availEntry{"native", "needs_setup", nativeNoKeyReason, nativeNoKeyFix},
		},
		{
			name:  "a ChatGPT sign-in with plan usage",
			setup: func(t *testing.T, dir string) { signedInNative(t, dir, true) },
			want:  nativeReady,
		},
		{
			name:  "a ChatGPT sign-in with plan usage off",
			setup: func(t *testing.T, dir string) { signedInNative(t, dir, false) },
			want:  availEntry{"native", "needs_setup", nativeNoKeyReason, nativeNoKeyFix},
		},
		{
			name:  "a broken providers.toml",
			setup: writes(modeltable.ProvidersFile, "version = 1\n\n[providers.openrouter]\n"+availSecretKey+" = \"x\"\n"),
			want:  availEntry{"native", "unavailable", "providers.toml could not be read", nativeUnreadFix},
		},
		{
			name:  "a value of the wrong type in providers.toml",
			setup: writes(modeltable.ProvidersFile, "version = 1\n\n[providers.openrouter]\napi_key = 123456789012\n"),
			want:  availEntry{"native", "unavailable", "providers.toml could not be read", nativeUnreadFix},
		},
		{
			name:  "a broken models.toml",
			setup: writes(modeltable.ModelsFile, "version = 1\n[models.x\n"),
			want:  availEntry{"native", "unavailable", "models.toml could not be read", nativeUnreadFix},
		},
		{
			name:  "no directory",
			noDir: true,
			want:  availEntry{"native", "unavailable", nativeNoDirReason, nativeUnreadFix},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := tc.getenv
			if getenv == nil {
				getenv = noEnv
			}
			dir := ""
			if !tc.noDir {
				dir = t.TempDir()
				if tc.setup != nil {
					tc.setup(t, dir)
				}
			}
			state, reason, fix := nativeAvailability(dir, getenv)
			if got := (availEntry{"native", string(state), reason, fix}); got != tc.want {
				t.Fatalf("native: %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestNativeAvailabilityNamesTheFileOnly (plan 036 §3.1, A1): a model table
// whose providers.toml has an unknown key that looks like a secret fails to
// load with an error naming the key — the case is not vacuous — and the
// reason names the file and never the key, nor any other part of the error.
func TestNativeAvailabilityNamesTheFileOnly(t *testing.T) {
	dir := t.TempDir()
	writeNativeFile(t, dir, modeltable.ProvidersFile, "version = 1\n\n[providers.openrouter]\n"+availSecretKey+" = \"x\"\n")
	_, err := modeltable.Load(dir)
	var fe *modeltable.FileError
	if err == nil || !errors.As(err, &fe) || !strings.Contains(err.Error(), availSecretKey) {
		t.Fatalf("Load: %v, want a FileError naming the key", err)
	}
	state, reason, fix := nativeAvailability(dir, func(string) string { return "" })
	if state != availUnavailable || reason != "providers.toml could not be read" {
		t.Fatalf("native: %s %q, want unavailable naming providers.toml", state, reason)
	}
	for _, s := range []string{reason, fix} {
		if strings.Contains(s, availSecretKey) || strings.Contains(s, "unknown key") || strings.Contains(s, dir) {
			t.Fatalf("%q carries the error's text", s)
		}
	}
}

// TestLoadErrFile (plan 036 §3.1): the file a Load error names, by its
// *modeltable.FileError when it is one — whose File may be a base name alone
// (Validate's, for a table with no directory) — else by which of the two
// paths its text carries, else neither.
func TestLoadErrFile(t *testing.T) {
	dir := "/n/native"
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&modeltable.FileError{File: modeltable.ModelsFile, Key: "default_model", Reason: "x"}, "models.toml"},
		{fmt.Errorf("modeltable: %s: toml: incompatible types", filepath.Join(dir, modeltable.ProvidersFile)), "providers.toml"},
		{fmt.Errorf("modeltable: %s holds neither %s nor %s", dir, modeltable.ProvidersFile, modeltable.ModelsFile), "native's model table"},
	} {
		if got := loadErrFile(tc.err, dir); got != tc.want {
			t.Fatalf("loadErrFile(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// availPath points PATH at a fresh directory holding an executable stub of
// each of names, and returns the directory.
func availPath(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		writeExecutable(t, dir, n)
	}
	t.Setenv("PATH", dir)
	return dir
}

// TestProcessResolution (plan 036 §3.1): the TUI's and CLI's column resolves
// a binary exactly as agentBinary has a session of the launch's resolve it,
// and records where it came from — --agent-bin and CRAZE_AGENT_BIN for the
// launch's own provider only (env when the agent's own lookup is left to read
// the variable), then [agents], then PATH — with the override it tried.
func TestProcessResolution(t *testing.T) {
	cursor, grok := agent.CursorProvider(), agent.GrokProvider()
	for _, tc := range []struct {
		name     string
		p        agent.Provider
		launch   agent.Provider
		onPath   bool   // cursor-agent / grok on PATH
		flag     string // "ok", "missing" or ""
		env      string
		agents   string
		wantOK   bool
		wantFrom string
		wantPath string // "flag", "env", "agents" for that override's path
	}{
		{name: "on PATH", p: cursor, launch: cursor, onPath: true, wantOK: true, wantFrom: binFromPath},
		{name: "missing on PATH", p: cursor, launch: cursor, wantFrom: binFromPath},
		{name: "--agent-bin found", p: cursor, launch: cursor, flag: "ok", wantOK: true, wantFrom: binFromFlag, wantPath: "flag"},
		{name: "--agent-bin missing, PATH not consulted", p: cursor, launch: cursor, onPath: true, flag: "missing", wantFrom: binFromFlag, wantPath: "flag"},
		{name: "CRAZE_AGENT_BIN found", p: grok, launch: grok, env: "ok", wantOK: true, wantFrom: binFromEnv, wantPath: "env"},
		{name: "CRAZE_AGENT_BIN missing", p: grok, launch: grok, onPath: true, env: "missing", wantFrom: binFromEnv, wantPath: "env"},
		{name: "CRAZE_AGENT_BIN is the launch's, not grok's", p: grok, launch: cursor, env: "ok", wantFrom: binFromPath},
		{name: "--agent-bin is the launch's, not grok's", p: grok, launch: cursor, flag: "ok", onPath: true, wantOK: true, wantFrom: binFromPath},
		{name: "[agents] found", p: grok, launch: cursor, agents: "ok", wantOK: true, wantFrom: binFromAgents, wantPath: "agents"},
		{name: "[agents] missing, PATH not consulted", p: grok, launch: cursor, onPath: true, agents: "missing", wantFrom: binFromAgents, wantPath: "agents"},
		{name: "the launch's --agent-bin wins over [agents]", p: cursor, launch: cursor, flag: "missing", agents: "ok", wantFrom: binFromFlag, wantPath: "flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var names []string
			if tc.onPath {
				names = tc.p.Bins()[:1]
			}
			availPath(t, names...)
			bin := func(how string) string {
				switch how {
				case "ok":
					return writeExecutable(t, t.TempDir(), "some-agent")
				case "missing":
					return filepath.Join(t.TempDir(), "does-not-exist")
				}
				return ""
			}
			paths := map[string]string{"flag": bin(tc.flag), "env": bin(tc.env), "agents": bin(tc.agents)}
			t.Setenv(envAgentBin, paths["env"])
			crazeHome(t)
			if paths["agents"] != "" {
				writeCrazeConfig(t, fmt.Sprintf("[agents]\n%s = %q\n", tc.p.Name(), paths["agents"]))
			}
			got := processResolution(tc.p, tc.launch, paths["flag"])
			want := binResolution{OK: tc.wantOK, Source: tc.wantFrom, Path: paths[tc.wantPath]}
			if got != want {
				t.Fatalf("resolution %+v, want %+v", got, want)
			}
		})
	}
}

// TestHubColumnIgnoresCrazeAgentBin (plan 036 §3.1, A1): the hub's column
// resolves a binary as the hub's hosts do — [agents], then PATH — and never
// reads CRAZE_AGENT_BIN, which the hub strips from them: with the variable
// naming an existing binary and grok otherwise missing, the hub says grok is
// missing, while the TUI's and CLI's column, for a grok launch, says it is
// ready, from the variable.
func TestHubColumnIgnoresCrazeAgentBin(t *testing.T) {
	availPath(t)
	crazeHome(t)
	t.Setenv(envAgentBin, writeExecutable(t, t.TempDir(), "some-agent"))

	if r := hubResolution(agent.GrokProvider()); r != (binResolution{Source: binFromPath}) {
		t.Fatalf("the hub's grok: %+v, want missing on PATH", r)
	}
	if r := processResolution(agent.GrokProvider(), agent.GrokProvider(), ""); !r.OK || r.Source != binFromEnv {
		t.Fatalf("the TUI's grok: %+v, want found through CRAZE_AGENT_BIN", r)
	}
	hub := availEntries(availability(hubAvailInputs(4242), agent.Providers()))
	if hub[1].ID != "grok" || hub[1].State != "unavailable" || hub[1].Reason != "grok not found on PATH" {
		t.Fatalf("the hub's answer for grok: %+v", hub[1])
	}
	own := availEntries(availability(processAvailInputs(agent.GrokProvider(), ""), agent.Providers()))
	if own[1] != grokReady {
		t.Fatalf("the TUI's answer for grok: %+v", own[1])
	}
}

// TestHubResolution (plan 036 §3.1): the hub's column takes [agents] when it
// is set — found, or missing with PATH not consulted — and otherwise PATH.
func TestHubResolution(t *testing.T) {
	grok := agent.GrokProvider()
	availPath(t, "grok")
	crazeHome(t)
	if r := hubResolution(grok); r != (binResolution{OK: true, Source: binFromPath}) {
		t.Fatalf("grok on PATH: %+v", r)
	}
	found := writeExecutable(t, t.TempDir(), "some-agent")
	writeCrazeConfig(t, fmt.Sprintf("[agents]\ngrok = %q\n", found))
	if r := hubResolution(grok); r != (binResolution{OK: true, Source: binFromAgents, Path: found}) {
		t.Fatalf("[agents].grok found: %+v", r)
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	writeCrazeConfig(t, fmt.Sprintf("[agents]\ngrok = %q\n", missing))
	if r := hubResolution(grok); r != (binResolution{Source: binFromAgents, Path: missing}) {
		t.Fatalf("[agents].grok missing, grok on PATH: %+v", r)
	}
}

// TestAvailabilityInputsFromTheProcess (plan 036 §3.1): the two columns'
// builders over a real scratch PATH, CRAZE_HOME and forced login session —
// the not-GUI rule from rundir.GUISession, with the TUI's and CLI's fix in
// one and the hub's naming its pid in the other; a long [agents] path cut
// to the cap; native from the process's craze directory, and unavailable
// with no home directory at all.
func TestAvailabilityInputsFromTheProcess(t *testing.T) {
	availPath(t, "cursor-agent", "grok")
	crazeHome(t)
	t.Setenv(envAgentBin, "")
	t.Setenv(rundir.GUISessionEnv, "0")
	cursor := agent.CursorProvider()

	needsSetup := availEntry{"native", "needs_setup", nativeNoKeyReason, nativeNoKeyFix}
	own := availEntries(availability(processAvailInputs(cursor, ""), agent.Providers()))
	if want := []availEntry{cursorNotGUI, grokReady, needsSetup}; !reflect.DeepEqual(own, want) {
		t.Fatalf("the TUI's answer outside the login session:\n%+v\nwant:\n%+v", own, want)
	}
	hub := availEntries(availability(hubAvailInputs(4242), agent.Providers()))
	hubCursor := cursorNotGUI
	hubCursor.Fix = `kill 4242 (no session ends), then run "craze ps" in a terminal on the Mac`
	if want := []availEntry{hubCursor, grokReady, needsSetup}; !reflect.DeepEqual(hub, want) {
		t.Fatalf("the hub's answer outside the login session:\n%+v\nwant:\n%+v", hub, want)
	}

	t.Setenv(rundir.GUISessionEnv, "1")
	long := "/" + strings.Repeat("a/", 150) + "cursor-agent"
	writeCrazeConfig(t, fmt.Sprintf("[agents]\ncursor = %q\n", long))
	for name, in := range map[string]availInputs{"TUI": processAvailInputs(cursor, ""), "hub": hubAvailInputs(4242)} {
		got := availability(in, agent.Providers())[0]
		if got.State != availUnavailable || !strings.HasPrefix(got.Reason, "[agents].cursor /a/a/") ||
			uniseg.StringWidth(got.Reason) > engine.RowTextCells || !strings.HasSuffix(got.Reason, "…") {
			t.Fatalf("the %s's cursor with a long [agents] path: %+v", name, got)
		}
	}

	t.Setenv("HOME", "")
	t.Setenv("CRAZE_HOME", "")
	state, reason, _ := processNative()
	if state != availUnavailable || reason != nativeNoDirReason {
		t.Fatalf("native with no home directory: %s %q", state, reason)
	}
}

// TestAvailabilityIsNeverCached (plan 036 §3.1): inputs built once answer
// from the machine as it is at each call — a binary installed and a key
// stored after the first call show in the second — so a picker, a list or a
// hub that keeps its inputs still lists what is there now.
func TestAvailabilityIsNeverCached(t *testing.T) {
	path := availPath(t, "cursor-agent")
	native := authNative(t)
	t.Setenv(envAgentBin, "")
	in := processAvailInputs(agent.CursorProvider(), "")
	before := availEntries(availability(in, agent.Providers()))
	if before[1].State != "unavailable" || before[2].State != "needs_setup" {
		t.Fatalf("before: %+v", before)
	}
	writeExecutable(t, path, "grok")
	writeNativeFile(t, native, modeltable.ProvidersFile,
		"version = 1\n\n[providers.openrouter]\napi_key = \"sk-avail-canary-0003\"\n")
	after := availEntries(availability(in, agent.Providers()))
	if want := []availEntry{cursorReady, grokReady, nativeReady}; !reflect.DeepEqual(after, want) {
		t.Fatalf("after a grok and a key:\n%+v\nwant:\n%+v", after, want)
	}
}
