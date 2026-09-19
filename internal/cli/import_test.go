package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/modeltable"
)

// importCanary is the only key these tests hold: obviously not a secret, and
// every case that could leak one greps stdout and the returned error for it
// (plan 018 §3.3, §7 AC 8a).
const importCanary = "sk-canary-not-a-secret"

// importFixtureConfig is a synthetic gx config.toml: one provider the
// importer can drive (with an inline key, to prove it never reaches stdout),
// two models on it, a Responses-backend provider that is skipped, and a model
// on that skipped provider — not a copy of any real gx file (plan 018 §3.3's
// "a real gx file is never copied into the repo").
const importFixtureConfig = `[models]
default = "acme/one"

[model_providers.acme]
base_url = "https://api.acme.example/v1"
api_backend = "chat_completions"
env_key = "ACME_API_KEY"
api_key = "` + importCanary + `"

[model_providers.respo]
base_url = "https://api.respo.example/v1"
api_backend = "responses"
env_key = "RESPO_API_KEY"

[model."acme/one"]
model = "wire-one"
name = "Acme One"
model_provider = "acme"
context_window = 100000
supports_reasoning_effort = true
reasoning_effort = "high"
reasoning_efforts = ["low", "high"]

[model."acme/two"]
model = "wire-two"
model_provider = "acme"

[model."respo/three"]
model = "wire-three"
model_provider = "respo"
`

// importFixtureConfigNoTwo is importFixtureConfig with "acme/two" removed, so
// a second import against it reports "acme/two" stale rather than unchanged.
var importFixtureConfigNoTwo = strings.Replace(importFixtureConfig, `
[model."acme/two"]
model = "wire-two"
model_provider = "acme"
`, "", 1)

// runImportGx runs `craze import gx` through the real root command — the
// tripwire's PersistentPreRunE included — and returns stdout and whatever
// RunE returned. SilenceErrors on the root command (root.go) means cobra
// itself never writes that error anywhere; craze's own Execute (not called by
// these tests) is what would print it to stderr, so a test that stands in for
// "never in stderr" checks err.Error() instead.
func runImportGx(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"import", "gx"}, args...))
	err := cmd.Execute()
	if stderr.Len() != 0 {
		t.Fatalf("cobra wrote to stderr despite SilenceErrors: %q", stderr.String())
	}
	return stdout.String(), err
}

// writeGxHome writes files under a fresh directory and returns it.
func writeGxHome(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// minimalGxConfig is one provider and one model, named so a precedence test
// can tell which of several gx homes was actually read.
func minimalGxConfig(id, alias string) string {
	return fmt.Sprintf(`[model_providers.%s]
base_url = "https://api.%s.example/v1"
api_backend = "chat_completions"
env_key = "%s_API_KEY"

[model.%q]
model = "wire-1"
model_provider = %q
`, id, id, strings.ToUpper(id), alias, id)
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %v, want %v", path, got, want)
	}
}

// TestImportGxWritesFilesAndReport is the acceptance-criteria case (plan 018
// §7 AC 2): the report names what was added and what was skipped and why,
// the files land at the right modes, the key is in providers.toml alone, and
// none of it — including the inline key the fixture carries — ever reaches
// stdout.
func TestImportGxWritesFilesAndReport(t *testing.T) {
	grokHome := writeGxHome(t, map[string]string{"config.toml": importFixtureConfig})
	dest := crazeHome(t)

	stdout, err := runImportGx(t, "--grok-home", grokHome)
	if err != nil {
		t.Fatalf("import: %v\n%s", err, stdout)
	}
	nativeDir := filepath.Join(dest, "native")
	for _, want := range []string{
		"craze import gx: " + grokHome + " -> " + nativeDir,
		"providers:\n  added (1): acme",
		"skipped (1):\n    respo: Responses API providers are not supported yet",
		"models:\n  added (2): acme/one, acme/two",
		`respo/three: provider "respo" was skipped: Responses API providers are not supported yet`,
		"default model: acme/one (gx)",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("report missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, importCanary) {
		t.Fatalf("the report leaked the key:\n%s", stdout)
	}
	if strings.Contains(err2str(err), importCanary) {
		t.Fatalf("the error leaked the key: %v", err)
	}

	assertPerm(t, nativeDir, 0o700)
	assertPerm(t, filepath.Join(nativeDir, "providers.toml"), 0o600)
	assertPerm(t, filepath.Join(nativeDir, "models.toml"), 0o644)

	table, err := modeltable.Load(nativeDir)
	if err != nil {
		t.Fatalf("loading the written table: %v", err)
	}
	if table.DefaultModel != "acme/one" {
		t.Fatalf("DefaultModel = %q, want acme/one", table.DefaultModel)
	}
	if got := table.Providers["acme"].APIKey.Reveal(); got != importCanary {
		t.Fatalf("the inline key was not written to providers.toml: %q", got)
	}

	modelsBody, err := os.ReadFile(filepath.Join(nativeDir, "models.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(modelsBody), importCanary) {
		t.Fatalf("models.toml carries the key:\n%s", modelsBody)
	}
}

func err2str(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestImportGxIsIdempotent: a second import of the same gx config reports
// every previously-added entry unchanged, nothing added or updated, and the
// default model kept rather than re-derived (plan 018 §3.3's merge rule).
func TestImportGxIsIdempotent(t *testing.T) {
	grokHome := writeGxHome(t, map[string]string{"config.toml": importFixtureConfig})
	crazeHome(t)
	if _, err := runImportGx(t, "--grok-home", grokHome); err != nil {
		t.Fatalf("first import: %v", err)
	}
	stdout, err := runImportGx(t, "--grok-home", grokHome)
	if err != nil {
		t.Fatalf("second import: %v\n%s", err, stdout)
	}
	for _, want := range []string{
		"providers:\n  unchanged (1): acme",
		"models:\n  unchanged (2): acme/one, acme/two",
		"default model: acme/one (kept)",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("report missing %q:\n%s", want, stdout)
		}
	}
	for _, unwanted := range []string{"added (", "updated ("} {
		if strings.Contains(stdout, unwanted) {
			t.Fatalf("a repeat import reported a change (%q):\n%s", unwanted, stdout)
		}
	}
}

// TestImportGxDetectsStale: a model gx no longer has after a re-import is
// kept on disk (an import never deletes) and reported stale.
func TestImportGxDetectsStale(t *testing.T) {
	grokHome := writeGxHome(t, map[string]string{"config.toml": importFixtureConfig})
	dest := crazeHome(t)
	if _, err := runImportGx(t, "--grok-home", grokHome); err != nil {
		t.Fatalf("first import: %v", err)
	}

	trimmedHome := writeGxHome(t, map[string]string{"config.toml": importFixtureConfigNoTwo})
	stdout, err := runImportGx(t, "--grok-home", trimmedHome)
	if err != nil {
		t.Fatalf("second import: %v\n%s", err, stdout)
	}
	for _, want := range []string{
		"models:\n  unchanged (1): acme/one",
		"stale (1): acme/two",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("report missing %q:\n%s", want, stdout)
		}
	}
	table, err := modeltable.Load(filepath.Join(dest, "native"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := table.Models["acme/two"]; !ok {
		t.Fatal("a stale model was deleted; an import must never delete")
	}
}

// TestImportGxDryRunWritesNothing: --dry-run prints the same shape of report
// and creates nothing at all — not the directory, not the lock file, not the
// two TOML files (plan 018 §3.3, §7 AC 2).
func TestImportGxDryRunWritesNothing(t *testing.T) {
	grokHome := writeGxHome(t, map[string]string{"config.toml": importFixtureConfig})
	dest := crazeHome(t)

	stdout, err := runImportGx(t, "--grok-home", grokHome, "--dry-run")
	if err != nil {
		t.Fatalf("dry-run import: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "dry run: nothing written") {
		t.Fatalf("report does not say nothing was written:\n%s", stdout)
	}
	if !strings.Contains(stdout, "providers:\n  added (1): acme") {
		t.Fatalf("dry-run report missing the would-be change:\n%s", stdout)
	}
	if strings.Contains(stdout, importCanary) {
		t.Fatalf("the dry-run report leaked the key:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dest, "native")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dry-run created the native directory: %v", err)
	}
	if entries, err := os.ReadDir(dest); err != nil || len(entries) != 0 {
		t.Fatalf("dry-run touched the craze directory: %v %v", entries, err)
	}
}

// TestImportGxGrokHomePrecedence: --grok-home beats $GROK_HOME beats
// ~/.grok, each proven by which fixture's model shows up in the report
// (plan 018 §3.3).
func TestImportGxGrokHomePrecedence(t *testing.T) {
	flagHome := writeGxHome(t, map[string]string{"config.toml": minimalGxConfig("flag", "flag/x")})
	envHome := writeGxHome(t, map[string]string{"config.toml": minimalGxConfig("env", "env/x")})
	homeDir := t.TempDir()
	homeGrok := filepath.Join(homeDir, ".grok")
	if err := os.MkdirAll(homeGrok, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeGrok, "config.toml"), []byte(minimalGxConfig("home", "home/x")), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("GROK_HOME", envHome)

	crazeHome(t)
	stdout, err := runImportGx(t, "--grok-home", flagHome)
	if err != nil {
		t.Fatalf("--grok-home: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "craze import gx: "+flagHome) || !strings.Contains(stdout, "flag/x") {
		t.Fatalf("--grok-home did not win over $GROK_HOME and ~/.grok:\n%s", stdout)
	}

	crazeHome(t)
	stdout, err = runImportGx(t)
	if err != nil {
		t.Fatalf("$GROK_HOME: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "craze import gx: "+envHome) || !strings.Contains(stdout, "env/x") {
		t.Fatalf("$GROK_HOME did not win over ~/.grok:\n%s", stdout)
	}

	t.Setenv("GROK_HOME", "")
	crazeHome(t)
	stdout, err = runImportGx(t)
	if err != nil {
		t.Fatalf("~/.grok: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "craze import gx: "+homeGrok) || !strings.Contains(stdout, "home/x") {
		t.Fatalf("~/.grok fallback did not run:\n%s", stdout)
	}
}

// TestImportGxMissingGrokHome: a --grok-home that holds neither of gx's files
// is a clear, plain failure (exit 1) naming the directory, and nothing is
// printed before it.
func TestImportGxMissingGrokHome(t *testing.T) {
	crazeHome(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	stdout, err := runImportGx(t, "--grok-home", missing)
	if err == nil {
		t.Fatal("import against a missing grok home succeeded")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error %v does not wrap fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %q does not name the missing directory", err)
	}
	var ee *exitError
	if errors.As(err, &ee) {
		t.Fatalf("unexpected exit error %+v, want a plain error (exit 1 by default)", ee)
	}
	if stdout != "" {
		t.Fatalf("a failed import printed a report:\n%s", stdout)
	}
}

// TestImportGxNoCrazeDirectory: with no HOME and no CRAZE_HOME there is
// nowhere to import into, and the CLI says so rather than falling back to a
// relative path.
func TestImportGxNoCrazeDirectory(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("CRAZE_HOME", "")
	grokHome := writeGxHome(t, map[string]string{"config.toml": importFixtureConfig})
	stdout, err := runImportGx(t, "--grok-home", grokHome)
	if err == nil {
		t.Fatal("import with no craze directory succeeded")
	}
	if !strings.Contains(err.Error(), "no craze directory") {
		t.Fatalf("error %q does not say there is no craze directory", err)
	}
	if stdout != "" {
		t.Fatalf("a failed import printed a report:\n%s", stdout)
	}
}

// TestImportGxMalformedGxFileNeverLeaksCanary: a gx file with a syntax error
// fails with a message naming only the file and the line — never the canary
// sitting right there in the broken line (plan 018 §3.3, §7 AC 8a).
func TestImportGxMalformedGxFileNeverLeaksCanary(t *testing.T) {
	grokHome := writeGxHome(t, map[string]string{"config.toml": "api_key = \"" + importCanary})
	crazeHome(t)
	stdout, err := runImportGx(t, "--grok-home", grokHome)
	if err == nil {
		t.Fatal("a malformed gx file was accepted")
	}
	if strings.Contains(err.Error(), importCanary) || strings.Contains(stdout, importCanary) {
		t.Fatalf("the canary leaked: err=%q stdout=%q", err, stdout)
	}
	if !strings.Contains(err.Error(), "not valid TOML") {
		t.Fatalf("error %q does not say the file is malformed", err)
	}
}

// TestImportGxNothingToImport: every provider skipped leaves nothing to
// import; the command fails (exit 1) with a clear message, and the report
// above it still says why (plan 018 §3.3's report, and the orchestrator's
// choice of exit code where the plan left it open — see the C10 report).
func TestImportGxNothingToImport(t *testing.T) {
	cfg := `[model_providers.respo]
base_url = "https://api.respo.example/v1"
api_backend = "responses"
env_key = "RESPO_API_KEY"

[model."respo/one"]
model = "wire-one"
model_provider = "respo"
`
	grokHome := writeGxHome(t, map[string]string{"config.toml": cfg})
	crazeHome(t)
	stdout, err := runImportGx(t, "--grok-home", grokHome)
	if err == nil {
		t.Fatal("an import with nothing importable succeeded")
	}
	var ee *exitError
	if errors.As(err, &ee) {
		t.Fatalf("unexpected exit error %+v, want a plain error (exit 1 by default)", ee)
	}
	if !strings.Contains(err.Error(), "no model could be imported") {
		t.Fatalf("error %q", err)
	}
	if !strings.Contains(stdout, "skipped (1):\n    respo: Responses API providers are not supported yet") {
		t.Fatalf("report missing the skip reason:\n%s", stdout)
	}
}

// TestImportGxHelpNeverMentionsNative pins §3.4's "hidden": the command that
// fills the native harness's config must not be the place a reader first
// learns the provider exists.
func TestImportGxHelpNeverMentionsNative(t *testing.T) {
	for _, argv := range [][]string{{"--help"}, {"import", "--help"}, {"import", "gx", "--help"}} {
		var stdout bytes.Buffer
		cmd := NewRootCmd()
		cmd.SetOut(&stdout)
		cmd.SetErr(&stdout)
		cmd.SetArgs(argv)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%q: %v", argv, err)
		}
		got := strings.ToLower(stdout.String())
		for _, bad := range []string{"native", "harness"} {
			if strings.Contains(got, bad) {
				t.Fatalf("craze %v --help mentions %q:\n%s", argv, bad, stdout.String())
			}
		}
	}
}
