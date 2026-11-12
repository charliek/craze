package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/version"
)

// Plan 050 C2: craze models, the waiting commands' model notices, and craze
// new --wait's pre-check — over a small synthetic catalog (fixtures must not
// move with every catalog refresh, which deprecates and retires shipped
// models), which also writes the orchestrator fixtures nocturn builds
// against (tests/cli/fixtures/orchestrator/v1, -update rewrites them).

var updateFixtures = flag.Bool("update", false, "rewrite tests/cli/fixtures/orchestrator/v1 from the synthetic catalog")

// fixtureVersion is the crazeVersion the fixtures carry: every version a
// command prints is replaced by it, so a release's version bump moves none.
const fixtureVersion = "0.0.0-fixture"

// fixtureKeyEnv is the synthetic acme provider's key variable, and
// fixtureKey a value for it.
const (
	fixtureKeyEnv = "ACME_FIXTURE_API_KEY"
	fixtureKey    = "sk-acme-fixture-0001"
)

// fixtureCatalog is the synthetic catalog: acme, a key provider, with an
// active model with efforts and a context default (acme/sonic), a
// deprecated one (acme/sonic-1, replaced by acme/sonic) and one with no
// effort control (acme/plain); the ChatGPT plan, whose models are the
// account's own alone (none without a list); and two retired aliases, one
// naming its replacement.
func fixtureCatalog() *modeltable.Catalog {
	return &modeltable.Catalog{
		ProviderOrder: []string{"chatgpt", "acme"},
		Providers: map[string]modeltable.Provider{
			"acme":    {Name: "Acme", Driver: modeltable.DriverOpenAICompat, BaseURL: "https://api.acme.example/v1", EnvKeys: []string{fixtureKeyEnv}},
			"chatgpt": {Name: "ChatGPT plan", Driver: modeltable.DriverChatGPT},
		},
		Starts: map[string][]string{"acme": {"acme/sonic"}},
		Models: map[string]modeltable.Model{
			"acme/sonic": {Provider: "acme", WireModel: "acme-sonic-2", Name: "Acme Sonic", ContextWindow: 1048576, DefaultContext: 272000,
				MaxOutputTokens: 128000, Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium", Vision: true},
			"acme/sonic-1": {Provider: "acme", WireModel: "acme-sonic-1", Name: "Acme Sonic 1", ContextWindow: 200000,
				Efforts: []string{"low", "high"}, DefaultEffort: "high", Deprecated: true, ReplacedBy: "acme/sonic"},
			"acme/plain": {Provider: "acme", WireModel: "acme-plain", Name: "Acme Plain", ContextWindow: 128000},
		},
		Retired: []modeltable.Retired{
			{Alias: "acme/sonic-0", Provider: "acme", WireModels: []string{"acme-sonic-0"}, ReplacedBy: "acme/sonic"},
			{Alias: "acme/ancient", Provider: "acme"},
		},
	}
}

// withFixtureCatalog makes the fixture catalog the CLI's, over a fresh craze
// directory, with the acme key set when funded.
func withFixtureCatalog(t *testing.T, funded bool) {
	t.Helper()
	prev := modelsCatalog
	modelsCatalog = func() (*modeltable.Catalog, error) { return fixtureCatalog(), nil }
	t.Cleanup(func() { modelsCatalog = prev })
	crazeHome(t)
	t.Setenv(fixtureKeyEnv, "")
	if funded {
		t.Setenv(fixtureKeyEnv, fixtureKey)
	}
}

// noSpawns makes every host spawn fail, and counts them.
func noSpawns(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	prev := hostspawn.Command
	hostspawn.Command = func([]string) (*exec.Cmd, error) {
		n.Add(1)
		return nil, errors.New("no host in this test")
	}
	t.Cleanup(func() { hostspawn.Command = prev })
	return &n
}

// fixtureOut is a command's stdout with this craze's version as the fixture's.
func fixtureOut(stdout string) string {
	return strings.ReplaceAll(stdout, `"crazeVersion":"`+version.Version+`"`, `"crazeVersion":"`+fixtureVersion+`"`)
}

// oneObject decodes stdout, which must be one JSON object on one line.
func oneObject(t *testing.T, stdout string) map[string]any {
	t.Helper()
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout is not one line: %q", stdout)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Fatalf("stdout is not one object: %v: %q", err, stdout)
	}
	return v
}

// TestModelsShippedReadsNothing: --shipped is the catalog alone — no row
// state, the same document whatever the craze directory or environment
// holds (a models.toml that would add a model, a key that would fund one, no
// craze directory at all) — and the real shipped catalog's document has the
// shape the contract names: rows sorted by alias with every key, `efforts` an
// array, the retired rows and the providers sorted.
func TestModelsShippedReadsNothing(t *testing.T) {
	home := crazeHome(t)
	stdout, stderr, code := executeErr([]string{"models", "--shipped", "--json"})
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, %q", code, stderr)
	}
	native := filepath.Join(home, "native")
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, modeltable.ModelsFile), []byte("version = 1\n\n[models.\"mine/m\"]\nprovider = \"fireworks\"\nwire_model = \"m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIREWORKS_API_KEY", "sk-fireworks-not-a-key-0001")
	again, _, _ := executeErr([]string{"models", "--shipped", "--json"})
	t.Setenv("HOME", "")
	t.Setenv("CRAZE_HOME", "")
	homeless, _, code := executeErr([]string{"models", "--shipped", "--json"})
	if again != stdout || homeless != stdout || code != 0 {
		t.Fatalf("--shipped moved with the machine:\n%s\n%s\n%s", stdout, again, homeless)
	}
	if local, _, code := executeErr([]string{"models", "--json"}); code != 1 || !strings.Contains(local, "there is no craze directory") {
		t.Fatalf("the local form with no craze directory: exit %d, %q", code, local)
	}

	var doc struct {
		Schema       int                          `json:"schema"`
		CrazeVersion string                       `json:"crazeVersion"`
		Models       []map[string]json.RawMessage `json:"models"`
		Retired      []map[string]string          `json:"retired"`
		Providers    []map[string]string          `json:"providers"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Schema != 1 || doc.CrazeVersion != version.Version || len(doc.Models) == 0 || len(doc.Retired) == 0 {
		t.Fatalf("document = %+v", doc)
	}
	keys := []string{"alias", "provider", "maker", "name", "status", "source", "efforts", "defaultEffort", "fast", "vision", "contextWindow", "defaultContext"}
	var aliases []string
	for _, row := range doc.Models {
		for _, k := range keys {
			if _, ok := row[k]; !ok {
				t.Errorf("row %s has no %s", row["alias"], k)
			}
		}
		if _, ok := row["state"]; ok {
			t.Errorf("row %s has state in the shipped form", row["alias"])
		}
		if string(row["efforts"])[0] != '[' || string(row["source"]) != `"catalog"` || string(row["fast"]) != "false" {
			t.Errorf("row %s: efforts %s, source %s, fast %s", row["alias"], row["efforts"], row["source"], row["fast"])
		}
		var alias string
		_ = json.Unmarshal(row["alias"], &alias)
		aliases = append(aliases, alias)
	}
	if !slices.IsSorted(aliases) || !slices.Contains(aliases, "openai/gpt-6.1-sol") {
		t.Fatalf("aliases = %q", aliases)
	}
	if !slices.IsSortedFunc(doc.Retired, func(a, b map[string]string) int { return strings.Compare(a["alias"], b["alias"]) }) ||
		!slices.IsSortedFunc(doc.Providers, func(a, b map[string]string) int { return strings.Compare(a["id"], b["id"]) }) {
		t.Fatal("retired or providers are not sorted")
	}
	for _, p := range doc.Providers {
		want := map[string][2]string{"chatgpt": {"sign-in", "only"}, "anthropic": {"key", "also"}}[p["id"]]
		if want == ([2]string{}) {
			want = [2]string{"key", "none"}
		}
		if p["auth"] != want[0] || p["accountModels"] != want[1] {
			t.Errorf("provider %s: auth %q, accountModels %q; want %q", p["id"], p["auth"], p["accountModels"], want)
		}
	}
}

// TestModelsLocalAndOneModel: the local form's rows carry their state —
// funded once the key is set, offered unless deprecated — a source of user
// for the user's own model and catalog for a shipped one; an alias is read as
// --model reads it (its case, the provider-qualified form), and a miss is the
// pre-check's error object: model_retired naming the replacement, or
// model_unknown — unverifiable under the ChatGPT plan with no list, never
// under a key provider. The table form says the same to a person.
func TestModelsLocalAndOneModel(t *testing.T) {
	withFixtureCatalog(t, true)
	native := filepath.Join(os.Getenv("CRAZE_HOME"), "native")
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, modeltable.ModelsFile), []byte("version = 1\n\n[models.\"mine\"]\nprovider = \"acme\"\nwire_model = \"m\"\n\n[models.\"acme/plain\"]\nname = \"Plain, renamed\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := executeErr([]string{"models", "--json"})
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, %q", code, stderr)
	}
	var doc modelsDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range doc.Models {
		got[r.Alias] = r.Source + " " + r.Status
		if r.State == nil {
			t.Fatalf("%s has no state", r.Alias)
		}
		if !r.State.Funded || r.State.Offered != (r.Status == modelActive) {
			t.Errorf("%s: state %+v", r.Alias, *r.State)
		}
	}
	want := map[string]string{"acme/plain": "catalog active", "acme/sonic": "catalog active", "acme/sonic-1": "catalog deprecated", "mine": "user active"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	for _, raw := range []string{"ACME/Sonic", " acme/sonic "} {
		out, _, code := executeErr([]string{"models", raw, "--json"})
		if row := oneObject(t, out); code != 0 || row["alias"] != "acme/sonic" {
			t.Fatalf("models %q: exit %d, %v", raw, code, row)
		}
	}
	for _, tc := range []struct {
		raw, reason, model, replacedBy string
		unverifiable                   bool
	}{
		{"acme/sonic-0", reasonModelRetired, "acme/sonic-0", "acme/sonic", false},
		{"acme/ancient", reasonModelRetired, "acme/ancient", "", false},
		{"acme/nope", reasonModelUnknown, "acme/nope", "", false},
		{"chatgpt/gpt-9", reasonModelUnknown, "chatgpt/gpt-9", "", true},
		{"bare $(name)", reasonModelUnknown, "barename", "", false},
		{"!!!", reasonModelUnknown, "", "", false},
	} {
		out, errOut, code := executeErr([]string{"models", tc.raw, "--json"})
		v := oneObject(t, out)
		e, _ := v["error"].(map[string]any)
		d, _ := e["detail"].(map[string]any)
		model, hasModel := d["model"]
		if code != 1 || e["kind"] != "config" || e["retryable"] != false || d["reason"] != tc.reason || !hasModel || model != tc.model ||
			(d["replaced_by"] != nil) != (tc.replacedBy != "") || (tc.replacedBy != "" && d["replaced_by"] != tc.replacedBy) ||
			(v["unverifiable"] == true) != tc.unverifiable {
			t.Errorf("models %q: exit %d, %s", tc.raw, code, out)
		}
		if tc.replacedBy != "" && !strings.HasPrefix(e["message"].(string), "native: "+tc.model+" was retired; use "+tc.replacedBy) {
			t.Errorf("models %q: the replacement does not lead: %q", tc.raw, e["message"])
		}
		if !strings.HasPrefix(errOut, "craze models: ") || strings.Count(errOut, "\n") != 1 {
			t.Errorf("models %q: stderr %q", tc.raw, errOut)
		}
	}

	table, _, code := executeErr([]string{"models"})
	for _, w := range []string{"ALIAS", "FUNDED", "acme/sonic-1", "deprecated (use acme/sonic)", "272000 of 1048576", "retired: acme/sonic-0 (use acme/sonic)"} {
		if code != 0 || !strings.Contains(table, w) {
			t.Errorf("the table lacks %q (exit %d):\n%s", w, code, table)
		}
	}
	if one, _, _ := executeErr([]string{"models", "acme/plain"}); strings.Count(one, "\n") != 2 || !strings.Contains(one, "acme/plain") {
		t.Errorf("one model's table:\n%s", one)
	}
	if _, _, code := executeErr([]string{"models", "a", "b"}); code != 2 {
		t.Errorf("two aliases: exit %d, want 2", code)
	}
}

// TestModelsUnfundedAndBrokenTable: with no key a row is neither funded nor
// offered; a models.toml that does not load is exit 1, kind config, one
// object under --json.
func TestModelsUnfundedAndBrokenTable(t *testing.T) {
	withFixtureCatalog(t, false)
	out, _, code := executeErr([]string{"models", "acme/sonic", "--json"})
	if st, _ := oneObject(t, out)["state"].(map[string]any); code != 0 || st["funded"] != false || st["offered"] != false {
		t.Fatalf("unfunded: exit %d, %s", code, out)
	}
	native := filepath.Join(os.Getenv("CRAZE_HOME"), "native")
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	// A key pasted where a key name goes: the load's error quotes it, and
	// nothing of it may reach stdout or stderr — the file is named, and
	// craze auth list shows why.
	const canary = "sk-canary-pasted-0123456789"
	if err := os.WriteFile(filepath.Join(native, modeltable.ModelsFile), []byte("version = 1\n"+canary+" = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"models", "--json"}, {"models"}, {"models", "acme/sonic", "--json"}} {
		out, errOut, code := executeErr(argv)
		if code != 1 || strings.Contains(out+errOut, canary) {
			t.Fatalf("%q: exit %d, the file's text leaked: %s %q", argv, code, out, errOut)
		}
		want := `craze models: models.toml could not be read; run "craze auth list" for the error` + "\n"
		if errOut != want {
			t.Errorf("%q: stderr %q, want %q", argv, errOut, want)
		}
		if argv[len(argv)-1] == "--json" {
			e, _ := oneObject(t, out)["error"].(map[string]any)
			if e["kind"] != "config" || e["detail"] != nil || e["message"] != strings.TrimSuffix(strings.TrimPrefix(want, "craze models: "), "\n") {
				t.Errorf("%q: error %v", argv, e)
			}
		}
	}
}

// TestModelsShippedIgnoresAHostileEnvironment: --shipped prints the same
// bytes whatever the environment holds — the removed config-file variable
// (which every other command refuses), a garbage CRAZE_HOME, no HOME — and
// main's reparenting mark is never honoured for it (ShippedModelsRequest).
func TestModelsShippedIgnoresAHostileEnvironment(t *testing.T) {
	crazeHome(t)
	clean, _, code := executeErr([]string{"models", "--shipped", "--json"})
	if code != 0 {
		t.Fatalf("clean: exit %d", code)
	}
	t.Setenv("CRAZE_"+"CONFIG", "ignored")
	t.Setenv("CRAZE_HOME", "\x01not a directory")
	t.Setenv("HOME", "")
	t.Setenv("CRAZE_REPARENT_FD", "not-a-pipe")
	for _, argv := range [][]string{{"models", "--shipped", "--json"}, {"models", "--json", "--shipped=true"}} {
		got, errOut, code := executeErr(argv)
		if code != 0 || errOut != "" || got != clean {
			t.Fatalf("%q under a hostile environment: exit %d, %q, same output %v", argv, code, errOut, got == clean)
		}
	}
	// The control: the local form still refuses the removed variable.
	if _, _, code := executeErr([]string{"models", "--json"}); code != 2 {
		t.Fatalf("the local form with the removed variable: exit %d, want 2", code)
	}
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"models", "--shipped"}, true},
		{[]string{"models", "--json", "--shipped", "acme/x"}, true},
		{[]string{"models", "--shipped=true"}, true},
		{[]string{"models", "--shipped=false"}, false},
		{[]string{"models", "--", "--shipped"}, false},
		{[]string{"models"}, false},
		{[]string{"serve", "--shipped"}, false},
		{nil, false},
	} {
		if got := ShippedModelsRequest(tc.args); got != tc.want {
			t.Errorf("ShippedModelsRequest(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestPreCheckRefusalsAndStartFailuresCarryTheNotice: a deprecated model the
// pre-check resolved is noticed on its refusal (an unlisted --effort, --fast)
// and on a start that fails after it passed — the stderr line with or without
// --json, and `warnings` on the --json object.
func TestPreCheckRefusalsAndStartFailuresCarryTheNotice(t *testing.T) {
	withFixtureCatalog(t, true)
	noSpawns(t)
	const line = "craze new: acme/sonic-1 is deprecated; use acme/sonic\n"
	const warning = `"warnings":[{"kind":"model_deprecated","model":"acme/sonic-1","replacedBy":"acme/sonic","message":"acme/sonic-1 is deprecated; use acme/sonic"}]`
	for _, args := range [][]string{
		{"--model", "acme/sonic-1", "--effort", "minimal"},
		{"--model", "acme/sonic-1", "--fast"},
		{"--model", "acme/sonic-1"},
	} {
		stdout, stderr, code := newWait(t, args...)
		if code != 1 || !strings.Contains(stdout, warning) || !strings.HasPrefix(stderr, line) || strings.Count(stderr, "\n") != 2 {
			t.Errorf("--json %q: exit %d, %s %q", args, code, stdout, stderr)
		}
		argv := append([]string{"new", "--wait", "--provider", "native", "-C", t.TempDir()}, args...)
		stdout, stderr, code = executeErr(append(argv, "go"))
		if code != 1 || stdout != "" || !strings.HasPrefix(stderr, line) || strings.Count(stderr, "\n") != 2 {
			t.Errorf("plain %q: exit %d, %q %q", args, code, stdout, stderr)
		}
	}
	if stdout, stderr, _ := newWait(t, "--model", "acme/sonic", "--effort", "minimal"); strings.Contains(stdout, "warnings") || strings.Contains(stderr, "deprecated") {
		t.Errorf("an active model's refusal carries a notice: %s %q", stdout, stderr)
	}
}

// newWait runs craze new --wait --json --provider native with args before
// the prompt, in a temporary directory.
func newWait(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	argv := append([]string{"new", "--wait", "--json", "--provider", "native", "-C", t.TempDir()}, args...)
	return executeErr(append(argv, "go"))
}

// TestNewWaitPreCheck: each reason, refused before any host is spawned (the
// spawn is never reached: no host, so no provider request), exit 1, kind
// config, its detail; and the controls — a level in another case, --no-fast,
// no --effort, no --model with an --effort — pass the check and reach the
// spawn. A table that will not load checks nothing: the host reports.
func TestNewWaitPreCheck(t *testing.T) {
	withFixtureCatalog(t, true)
	spawns := noSpawns(t)
	for _, tc := range []struct {
		name   string
		args   []string
		detail string
		msg    string
	}{
		{"unknown", []string{"--model", "acme/nope"}, `{"reason":"model_unknown","model":"acme/nope"}`, `native: unknown model "acme/nope"; craze models lists the models`},
		{"retired", []string{"--model", "acme/sonic-0"}, `{"reason":"model_retired","model":"acme/sonic-0","replaced_by":"acme/sonic"}`, "native: acme/sonic-0 was retired; use acme/sonic"},
		{"retired, no replacement", []string{"--model", "ACME/ancient"}, `{"reason":"model_retired","model":"acme/ancient"}`, "native: acme/ancient was retired (the model table has"},
		{"an effort not listed", []string{"--model", "acme/sonic", "--effort", "minimal"}, `{"reason":"effort_not_offered","model":"acme/sonic"}`, "--effort minimal: acme/sonic offers low, medium, high"},
		{"no effort control", []string{"--model", "acme/plain", "--effort", "high"}, `{"reason":"effort_not_offered","model":"acme/plain"}`, "--effort high: acme/plain has no effort control"},
		{"fast", []string{"--model", "acme/sonic", "--fast"}, `{"reason":"fast_not_offered","model":"acme/sonic"}`, "--fast: acme/sonic has no fast variant"},
		{"fast, no model", []string{"--fast"}, `{"reason":"fast_not_offered","model":""}`, "--fast: no native model has a fast variant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := newWait(t, tc.args...)
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
			if v.Error.Kind != string(errkind.Config) || v.Error.Retryable || string(v.Error.Detail) != tc.detail ||
				!strings.HasPrefix(v.Error.Message, tc.msg) {
				t.Errorf("error = %+v (detail %s)", v.Error, v.Error.Detail)
			}
			if stderr != "craze new: "+v.Error.Message+"\n" {
				t.Errorf("stderr = %q", stderr)
			}
		})
	}
	if n := spawns.Load(); n != 0 {
		t.Fatalf("%d hosts spawned for refusals", n)
	}
	for i, args := range [][]string{
		{"--model", "acme/sonic", "--effort", "HIGH"},
		{"--model", "acme/sonic", "--no-fast"},
		{"--model", "acme/sonic-1"},
		{"--effort", "minimal"},
		{"--model", "acme/plain"},
	} {
		stdout, _, code := newWait(t, args...)
		if code != 1 || !strings.Contains(stdout, `"kind":"start_failed"`) || spawns.Load() != int32(i+1) {
			t.Errorf("%q: exit %d, %d spawns, %s", args, code, spawns.Load(), stdout)
		}
	}

	native := filepath.Join(os.Getenv("CRAZE_HOME"), "native")
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, modeltable.ModelsFile), []byte("version = [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := spawns.Load()
	if stdout, _, code := newWait(t, "--model", "acme/nope", "--fast"); code != 1 || spawns.Load() != before+1 || strings.Contains(stdout, `"reason"`) {
		t.Fatalf("a table that does not load: exit %d, %d spawns, %s", code, spawns.Load()-before, stdout)
	}
}

// TestNewWaitPreCheckIsNativeAlone: an ACP provider's --model, --effort and
// --fast are not checked here — the agent's own word stands — and craze send
// and plain craze new have no pre-check (nativeStartCheck is called by craze
// new --wait alone).
func TestNewWaitPreCheckIsNativeAlone(t *testing.T) {
	withFixtureCatalog(t, true)
	spawns := noSpawns(t)
	stdout, _, code := executeErr([]string{"new", "--wait", "--json", "--provider", "cursor", "--model", "acme/sonic-0", "--effort", "minimal", "--fast", "-C", t.TempDir(), "go"})
	if code != 1 || spawns.Load() != 1 || strings.Contains(stdout, `"reason"`) {
		t.Fatalf("cursor: exit %d, %d spawns, %s", code, spawns.Load(), stdout)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for range strings.Count(string(b), "nativeStartCheck(") - strings.Count(string(b), "func nativeStartCheck(") {
			callers = append(callers, name)
		}
	}
	if !slices.Equal(callers, []string{"new_wait.go"}) {
		t.Fatalf("nativeStartCheck is called from %q, want craze new --wait's new_wait.go alone", callers)
	}
}

// withNoticeTable makes the notices read the fixture catalog's table.
func withNoticeTable(t *testing.T, load func() (*modeltable.Table, error)) {
	t.Helper()
	prev := noticeTable
	noticeTable = load
	t.Cleanup(func() { noticeTable = prev })
}

// fixtureTable is the fixture catalog over an empty directory.
func fixtureTable() (*modeltable.Table, error) {
	return modeltable.CatalogTable(fixtureCatalog())
}

// fixtureResult is a completed result about a session of provider on model.
func fixtureResult(provider, model string) *turnResult {
	res := newResult(resultSession{id: "0199c0de-1111-7000-8000-397e175bfe79", provider: provider, model: model}, statusCompleted)
	res.Turn = "turn-1"
	res.fill(newTurnAccumulator())
	msg := "done"
	res.Message = &msg
	return res
}

// TestModelNotices: a native result or exit-1 failure on a deprecated model
// says so on stderr, once, under the command's name, and under --json in
// `warnings`; not an ACP provider's on the same name, not an active model, not
// a command with no notes (craze send's receipt), and nothing when the table
// will not load — the exit code the same throughout. craze new --wait's
// pre-checked model stands in while the result names none.
func TestModelNotices(t *testing.T) {
	withNoticeTable(t, fixtureTable)
	const line = "craze wait: acme/sonic-1 is deprecated; use acme/sonic\n"
	const warnings = `"warnings":[{"kind":"model_deprecated","model":"acme/sonic-1","replacedBy":"acme/sonic","message":"acme/sonic-1 is deprecated; use acme/sonic"}]`
	run := func(o resultOutput, res *turnResult) (string, string, error) {
		var out, errs bytes.Buffer
		o.notes = &errs
		err := writeResult(&out, &errs, o, res)
		return out.String(), errs.String(), err
	}
	o := resultOutput{what: waitWhat, json: true}
	out, errs, err := run(o, fixtureResult("native", "acme/sonic-1"))
	if err != nil || errs != line || !strings.Contains(out, warnings) || strings.Count(out, "\n") != 1 {
		t.Fatalf("deprecated: %v %q %s", err, errs, out)
	}
	for _, tc := range []struct{ provider, model string }{{"cursor", "acme/sonic-1"}, {"native", "acme/sonic"}, {"native", ""}} {
		out, errs, err := run(o, fixtureResult(tc.provider, tc.model))
		if err != nil || errs != "" || strings.Contains(out, "warnings") {
			t.Errorf("%s %s: %v %q %s", tc.provider, tc.model, err, errs, out)
		}
	}
	asked := o
	asked.asked = "acme/sonic-1"
	if _, errs, _ := run(asked, fixtureResult("native", "")); errs != line {
		t.Errorf("the pre-checked model, no model told: %q", errs)
	}
	var quiet bytes.Buffer
	if err := writeResult(&quiet, &quiet, o, fixtureResult("native", "acme/sonic-1")); err != nil || strings.Contains(quiet.String(), "warnings") {
		t.Errorf("a command with no notes: %v %s", err, quiet.String())
	}
	plain, errs, _ := run(resultOutput{what: waitWhat}, fixtureResult("native", "acme/sonic-1"))
	if errs != line || strings.Contains(plain, "deprecated") {
		t.Errorf("plain: %q %q", errs, plain)
	}

	failed := fixtureResult("native", "acme/sonic-1")
	failed.Status = statusFailed
	failed.Error = kindError("boom", errkind.Provider)
	_, errs, err = run(o, failed)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 || errs != line {
		t.Errorf("a failed result: %v %q", err, errs)
	}

	var out2, errs2 bytes.Buffer
	o.notes = &errs2
	err = writeFailure(&out2, o, "bfe79", &waitFailure{msg: "lost", kind: errkind.ConnectionLost, provider: "native", model: "acme/sonic-1"})
	if failureKind(err) != errkind.ConnectionLost || errs2.String() != line || !strings.Contains(out2.String(), warnings) {
		t.Errorf("an exit-1 failure: %v %q %s", err, errs2.String(), out2.String())
	}

	withNoticeTable(t, func() (*modeltable.Table, error) { return nil, errors.New("will not load") })
	out, errs, err = run(o, fixtureResult("native", "acme/sonic-1"))
	if err != nil || errs != "" || strings.Contains(out, "warnings") {
		t.Errorf("a table that will not load: %v %q %s", err, errs, out)
	}
}

// TestNoticesAreCapped: at most maxWarnings.
func TestNoticesAreCapped(t *testing.T) {
	var many []resultWarning
	for range maxWarnings + 3 {
		many = append(many, resultWarning{Kind: warningModelDeprecated})
	}
	if got := capWarnings(many); len(got) != maxWarnings {
		t.Fatalf("%d warnings kept, want %d", len(got), maxWarnings)
	}
}

// orchestratorFixtures is where nocturn's fixtures live.
const orchestratorFixtures = "../../tests/cli/fixtures/orchestrator/v1"

// TestOrchestratorFixtures regenerates every fixture of the orchestrator
// contract (plan 050 C2) from the synthetic catalog, through the commands
// themselves, and compares it with the file: they cannot rot. -update
// rewrites them.
func TestOrchestratorFixtures(t *testing.T) {
	withFixtureCatalog(t, true)
	withNoticeTable(t, loadNativeTable)
	spawns := noSpawns(t)
	got := map[string]string{}
	run := func(name string, wantCode int, argv ...string) {
		stdout, _, code := executeErr(argv)
		if code != wantCode {
			t.Fatalf("%s: exit %d, want %d: %s", name, code, wantCode, stdout)
		}
		got[name] = fixtureOut(stdout)
	}
	run("models.json", 0, "models", "--shipped", "--json")
	run("model-active.json", 0, "models", "acme/sonic", "--json")
	run("model-deprecated.json", 0, "models", "acme/sonic-1", "--json")
	run("model-retired.json", 1, "models", "acme/sonic-0", "--json")
	run("model-unknown.json", 1, "models", "acme/nope", "--json")
	run("model-unverifiable.json", 1, "models", "chatgpt/gpt-9", "--json")
	dir := t.TempDir()
	run("new-wait-retired.json", 1, "new", "--wait", "--json", "--provider", "native", "--model", "acme/sonic-0", "-C", dir, "go")
	run("new-wait-effort.json", 1, "new", "--wait", "--json", "--provider", "native", "--model", "acme/sonic", "--effort", "minimal", "-C", dir, "go")
	run("new-wait-fast.json", 1, "new", "--wait", "--json", "--provider", "native", "--model", "acme/sonic", "--fast", "-C", dir, "go")
	run("new-wait-effort-deprecated.json", 1, "new", "--wait", "--json", "--provider", "native", "--model", "acme/sonic-1", "--effort", "minimal", "-C", dir, "go")
	if spawns.Load() != 0 {
		t.Fatalf("%d hosts spawned", spawns.Load())
	}
	var out, errs bytes.Buffer
	if err := writeResult(&out, &errs, resultOutput{what: newWhat, json: true, notes: &errs}, fixtureResult("native", "acme/sonic-1")); err != nil {
		t.Fatal(err)
	}
	if errs.String() != "craze new: acme/sonic-1 is deprecated; use acme/sonic\n" {
		t.Fatalf("stderr = %q", errs.String())
	}
	got["wait-result-warnings.json"] = out.String()

	for name, body := range got {
		path := filepath.Join(orchestratorFixtures, name)
		if *updateFixtures {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v (run go test ./internal/cli -run TestOrchestratorFixtures -update)", name, err)
			continue
		}
		if string(want) != body {
			t.Errorf("%s moved:\n got %s\nwant %s", name, body, want)
		}
	}
	entries, err := os.ReadDir(orchestratorFixtures)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := got[e.Name()]; !ok && e.Name() != "README.md" {
			t.Errorf("%s is no fixture this test writes", e.Name())
		}
	}
}

// TestModelsMakerAndSourceNotes (plan 050 C5): every row's maker is its
// model's — a provider's for each model of it, a host's model its own, ""
// for none — in the shipped and the local forms; with Fireworks and
// OpenRouter keys the local rows OpenRouter's DeepSeek and GLM routers hide
// behind are funded and not offered, and the table form says where each
// maker's models come from.
func TestModelsMakerAndSourceNotes(t *testing.T) {
	crazeHome(t)
	for _, name := range modeltable.CatalogEnvNames() {
		t.Setenv(name, "")
	}
	t.Setenv("FIREWORKS_API_KEY", "fw-models-dummy-0001")
	t.Setenv("OPENROUTER_API_KEY", "or-models-dummy-0001")
	for _, args := range [][]string{{"models", "--shipped", "--json"}, {"models", "--json"}} {
		stdout, _, code := executeErr(args)
		var doc modelsDoc
		if err := json.Unmarshal([]byte(stdout), &doc); code != 0 || err != nil {
			t.Fatalf("%q: exit %d, %v", args, code, err)
		}
		rows := map[string]modelRow{}
		for _, r := range doc.Models {
			rows[r.Alias] = r
		}
		for alias, want := range map[string]string{
			"openrouter/gpt-sol-latest": "openai", "openai/gpt-6.1-sol": "openai", "glm-5.3": "zai",
			"muse-wonder-1.0": "meta", "openrouter/muse-wonder": "meta", "fireworks/glm-5p3": "zai",
			"openrouter/deepseek-flash-latest": "deepseek", "anthropic/claude-opus-5-5": "anthropic",
			"fireworks/kimi-k3p1-pro": "", "openrouter/gemini-flash-latest": "", "fireworks/ember-1": "",
		} {
			if got := rows[alias].Maker; got != want {
				t.Errorf("%q: %s maker = %q, want %q", args, alias, got, want)
			}
		}
		if args[1] == "--json" {
			for _, alias := range []string{"openrouter/glm-latest", "openrouter/deepseek-flash-latest"} {
				if st := rows[alias].State; st == nil || !st.Funded || st.Offered {
					t.Errorf("%s state = %+v, want funded and not offered", alias, st)
				}
			}
			if st := rows["fireworks/glm-5p3"].State; st == nil || !st.Offered {
				t.Errorf("fireworks/glm-5p3 state = %+v, want offered", st)
			}
		}
	}
	table, _, code := executeErr([]string{"models"})
	for _, w := range []string{
		"note: DeepSeek models come from Fireworks; 1 model of OpenRouter is not listed and still runs by name (source_order in models.toml changes the order)\n",
		"note: GLM models come from Fireworks; 2 models of OpenRouter are not listed and still run by name (source_order in models.toml changes the order)\n",
	} {
		if code != 0 || !strings.Contains(table, w) {
			t.Errorf("the table lacks %q (exit %d):\n%s", w, code, table)
		}
	}
	if strings.Contains(table, "note: Meta") || strings.Contains(table, "note: OpenAI") {
		t.Errorf("the table notes a maker it hides nothing of:\n%s", table)
	}
}

// TestModelsTableNotesTheTablesWarnings (review P3): the table form prints the
// warnings the table's load gave — an unknown name in source_order among
// them — as notes, as craze auth list does, with the craze directory's path
// left out; --json stays one object, with none of them.
func TestModelsTableNotesTheTablesWarnings(t *testing.T) {
	crazeHome(t)
	for _, name := range modeltable.CatalogEnvNames() {
		t.Setenv(name, "")
	}
	native := filepath.Join(os.Getenv("CRAZE_HOME"), "native")
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, modeltable.ModelsFile), []byte("version = 1\nsource_order = [\"nope\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	table, _, code := executeErr([]string{"models"})
	const want = "note: models.toml: source_order: \"nope\" is not a provider; the name is passed over\n"
	if code != 0 || !strings.Contains(table, want) || strings.Contains(table, native) {
		t.Fatalf("exit %d, the table:\n%s\nwant %q and no path", code, table, want)
	}
	out, _, code := executeErr([]string{"models", "--json"})
	if code != 0 || strings.Contains(out, "nope") {
		t.Fatalf("--json: exit %d, %q", code, out)
	}
	oneObject(t, out)
}
