package modeltable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/redact"
)

// canary is the only key any test file holds. It is obviously not a secret,
// and every test that could leak a key looks for it.
const canary = "sk-canary-not-a-secret"

const validProviders = `version = 1

[providers.fireworks]
driver = "openai-compat"
base_url = "https://api.fireworks.example/inference/v1"
env_keys = ["FIREWORKS_API_KEY"]
source = "gx"

[providers.openrouter]
driver = "openrouter"
env_keys = ["OPENROUTER_API_KEY"]
api_key = "sk-canary-not-a-secret"
`

const validModels = `version = 1
default_model = "fireworks/kimi-k3"

[models."fireworks/kimi-k3"]
provider = "fireworks"
wire_model = "accounts/fireworks/models/kimi-k3"
name = "Kimi K3 (Fireworks)"
context_window = 262144
max_output_tokens = 32768
efforts = ["low", "high", "max"]
default_effort = "high"
vision = true
source = "gx"

[models."openrouter/minimax-m3"]
provider = "openrouter"
wire_model = "minimax/minimax-m3"
`

// validTable is the Table validProviders and validModels load as.
func validTable() *Table {
	return &Table{
		DefaultModel: "fireworks/kimi-k3",
		Providers: map[string]Provider{
			"fireworks": {
				Driver:  DriverOpenAICompat,
				BaseURL: "https://api.fireworks.example/inference/v1",
				EnvKeys: []string{"FIREWORKS_API_KEY"},
				Source:  SourceGX,
			},
			"openrouter": {
				Driver:  DriverOpenRouter,
				EnvKeys: []string{"OPENROUTER_API_KEY"},
				APIKey:  canary,
				Source:  SourceManual,
			},
		},
		Models: map[string]Model{
			"fireworks/kimi-k3": {
				Provider:        "fireworks",
				WireModel:       "accounts/fireworks/models/kimi-k3",
				Name:            "Kimi K3 (Fireworks)",
				ContextWindow:   262144,
				MaxOutputTokens: 32768,
				Efforts:         []string{"low", "high", "max"},
				DefaultEffort:   "high",
				Vision:          true,
				Source:          SourceGX,
			},
			"openrouter/minimax-m3": {
				Provider:  "openrouter",
				WireModel: "minimax/minimax-m3",
				Source:    SourceManual,
			},
		},
	}
}

// writeFiles lays down the two files in a fresh directory at the modes Save
// would use; "" skips a file.
func writeFiles(t *testing.T, providers, models string) string {
	t.Helper()
	dir := t.TempDir()
	if providers != "" {
		if err := os.WriteFile(filepath.Join(dir, ProvidersFile), []byte(providers), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if models != "" {
		if err := os.WriteFile(filepath.Join(dir, ModelsFile), []byte(models), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// wantFileError asserts err is a *FileError locating exactly file, table, key.
func wantFileError(t *testing.T, err error, file, table, key string) *FileError {
	t.Helper()
	var fe *FileError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v (%T), want a *FileError", err, err)
	}
	if fe.File != file || fe.Table != table || fe.Key != key {
		t.Fatalf("FileError at {File:%q Table:%q Key:%q}, want {%q %q %q}; error: %v",
			fe.File, fe.Table, fe.Key, file, table, key, err)
	}
	return fe
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestLoadValidFiles(t *testing.T) {
	dir := writeFiles(t, validProviders, validModels)
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := validTable(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Load =\n%+v\nwant\n%+v", got, want)
	}
	if want := []string{"fireworks/kimi-k3", "openrouter/minimax-m3"}; !slices.Equal(got.Aliases(), want) {
		t.Fatalf("Aliases = %v, want %v", got.Aliases(), want)
	}
}

// TestSaveRoundTripsThroughLoad also pins the modes and the headers: the key
// file private, the model file shareable, the directory private, each file
// saying it is machine-rewritten and how to keep a hand edit.
func TestSaveRoundTripsThroughLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "craze", "native") // Save creates it
	want := validTable()
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip =\n%+v\nwant\n%+v", got, want)
	}

	if m := mode(t, dir); m != 0o700 {
		t.Errorf("dir mode = %04o, want 0700", m)
	}
	if m := mode(t, filepath.Join(dir, ProvidersFile)); m != 0o600 {
		t.Errorf("%s mode = %04o, want 0600", ProvidersFile, m)
	}
	if m := mode(t, filepath.Join(dir, ModelsFile)); m != 0o644 {
		t.Errorf("%s mode = %04o, want 0644", ModelsFile, m)
	}

	pb, err := os.ReadFile(filepath.Join(dir, ProvidersFile))
	if err != nil {
		t.Fatal(err)
	}
	mb, err := os.ReadFile(filepath.Join(dir, ModelsFile))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{ProvidersFile: string(pb), ModelsFile: string(mb)} {
		if !strings.HasPrefix(body, "# craze native harness") {
			t.Errorf("%s does not start with its header comment:\n%s", name, body)
		}
		for _, phrase := range []string{"machine-rewritten by `craze import gx`", "comments and key order", `source = "manual"`} {
			if !strings.Contains(body, phrase) {
				t.Errorf("%s header lacks %q", name, phrase)
			}
		}
	}
	// The key is written for real into its own file, and only there.
	if !strings.Contains(string(pb), `api_key = "`+canary+`"`) {
		t.Errorf("%s does not hold the inline key:\n%s", ProvidersFile, pb)
	}
	if strings.Contains(string(pb), redacted) {
		t.Errorf("%s holds the redaction marker instead of the key", ProvidersFile)
	}
	if strings.Contains(string(mb), canary) {
		t.Errorf("%s holds a key", ModelsFile)
	}
}

// TestSaveWritesStableReadableFiles pins the body models.toml is written
// with: one blank-line-separated section per alias in sorted order, absent
// optional fields left out rather than written as zero. Saving the same table
// twice gives the same bytes, so an import that changed nothing shows no diff.
func TestSaveWritesStableReadableFiles(t *testing.T) {
	const wantBody = `version = 1
default_model = "fireworks/kimi-k3"

[models."fireworks/kimi-k3"]
provider = "fireworks"
wire_model = "accounts/fireworks/models/kimi-k3"
name = "Kimi K3 (Fireworks)"
context_window = 262144
max_output_tokens = 32768
efforts = ["low", "high", "max"]
default_effort = "high"
vision = true
source = "gx"

[models."openrouter/minimax-m3"]
provider = "openrouter"
wire_model = "minimax/minimax-m3"
source = "manual"
`
	read := func(dir string) (string, string) {
		pb, err := os.ReadFile(filepath.Join(dir, ProvidersFile))
		if err != nil {
			t.Fatal(err)
		}
		mb, err := os.ReadFile(filepath.Join(dir, ModelsFile))
		if err != nil {
			t.Fatal(err)
		}
		return string(pb), string(mb)
	}
	a, b := t.TempDir(), t.TempDir()
	for _, dir := range []string{a, b} {
		if err := Save(dir, validTable()); err != nil {
			t.Fatal(err)
		}
	}
	pa, ma := read(a)
	pb, mb := read(b)
	if pa != pb || ma != mb {
		t.Fatal("two saves of one table wrote different bytes")
	}
	if body := strings.TrimPrefix(ma, modelsHeader); body != wantBody {
		t.Fatalf("models.toml body =\n%s\nwant\n%s", body, wantBody)
	}
}

// TestSaveTightensAnExistingDirectory: MkdirAll leaves an existing
// directory's mode alone, so Save has to fix it itself.
func TestSaveTightensAnExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "native")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // defeat the umask
		t.Fatal(err)
	}
	if err := Save(dir, validTable()); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Fatalf("dir mode = %04o, want 0700", m)
	}
}

func TestSaveRefusesAnInvalidTable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "native")
	tbl := validTable()
	tbl.DefaultModel = "nope"
	err := Save(dir, tbl)
	wantFileError(t, err, ModelsFile, "", "default_model")
	if _, statErr := os.Stat(dir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("Save wrote something for an invalid table: stat err = %v", statErr)
	}
}

func TestLoadMissingFiles(t *testing.T) {
	t.Run("neither", func(t *testing.T) {
		_, err := Load(t.TempDir())
		if !errors.Is(err, ErrNotConfigured) || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want ErrNotConfigured and fs.ErrNotExist", err)
		}
	})
	t.Run("only providers", func(t *testing.T) {
		_, err := Load(writeFiles(t, validProviders, ""))
		if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrNotConfigured) {
			t.Fatalf("err = %v, want fs.ErrNotExist without ErrNotConfigured", err)
		}
		if !strings.Contains(err.Error(), ModelsFile+" is missing") {
			t.Fatalf("err = %v, want it to name %s", err, ModelsFile)
		}
	})
	t.Run("only models", func(t *testing.T) {
		_, err := Load(writeFiles(t, "", validModels))
		if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrNotConfigured) {
			t.Fatalf("err = %v, want fs.ErrNotExist without ErrNotConfigured", err)
		}
		if !strings.Contains(err.Error(), ProvidersFile+" is missing") {
			t.Fatalf("err = %v, want it to name %s", err, ProvidersFile)
		}
	})
	t.Run("no directory", func(t *testing.T) {
		// "" must never mean the working directory.
		if _, err := Load(""); err == nil {
			t.Fatal("Load(\"\") succeeded")
		}
		if err := Save("", validTable()); err == nil {
			t.Fatal("Save(\"\") succeeded")
		}
	})
}

// TestLoadForImport covers the one case it differs from Load in: a lone file,
// which an interrupted first Save (providers.toml first) leaves behind. Load
// refuses it; LoadForImport reads it for the import to merge into, still
// strictly, and skips only the checks that need the missing file.
func TestLoadForImport(t *testing.T) {
	t.Run("neither file: nothing to merge into", func(t *testing.T) {
		got, err := LoadForImport(t.TempDir())
		if err != nil || got != nil {
			t.Fatalf("LoadForImport = %v, %v; want nil, nil", got, err)
		}
	})
	t.Run("providers.toml alone, after an interrupted first save", func(t *testing.T) {
		dir := t.TempDir()
		if err := Save(dir, validTable()); err != nil {
			t.Fatal(err)
		}
		mpath := filepath.Join(dir, ModelsFile)
		if err := os.Remove(mpath); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil || errors.Is(err, ErrNotConfigured) {
			t.Fatalf("Load = %v, want the lone-file error, unchanged", err)
		}
		got, err := LoadForImport(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Providers, validTable().Providers) || len(got.Models) != 0 || got.DefaultModel != "" {
			t.Fatalf("LoadForImport = %+v, want the providers and nothing else", got)
		}
		if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], mpath+" is missing") {
			t.Fatalf("Warnings = %q, want one naming %s", got.Warnings, mpath)
		}
	})
	t.Run("models.toml alone: its providers are not checked", func(t *testing.T) {
		got, err := LoadForImport(writeFiles(t, "", validModels))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Models, validTable().Models) || len(got.Providers) != 0 || got.DefaultModel != "fireworks/kimi-k3" {
			t.Fatalf("LoadForImport = %+v, want the models and default", got)
		}
	})
	t.Run("the lone file is still strict and valid on its own", func(t *testing.T) {
		cases := []struct {
			name, providers, models, file, table, key string
		}{
			{"unknown key", validProviders + "bse_url = \"x\"\n", "", ProvidersFile, "providers.openrouter", "bse_url"},
			{"provider rule", strings.Replace(validProviders, `driver = "openrouter"`, `driver = "bogus"`, 1), "", ProvidersFile, "providers.openrouter", "driver"},
			{"default_model still names a model", "", strings.Replace(validModels, `default_model = "fireworks/kimi-k3"`, `default_model = "nope"`, 1), ModelsFile, "", "default_model"},
			{"model rule", "", strings.Replace(validModels, `default_effort = "high"`, `default_effort = "medium"`, 1), ModelsFile, `models."fireworks/kimi-k3"`, "default_effort"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				dir := writeFiles(t, tc.providers, tc.models)
				_, err := LoadForImport(dir)
				wantFileError(t, err, filepath.Join(dir, tc.file), tc.table, tc.key)
			})
		}
	})
	t.Run("both files: exactly Load, cross-file checks included", func(t *testing.T) {
		dir := writeFiles(t, validProviders, strings.Replace(validModels, `provider = "openrouter"`, `provider = "nope"`, 1))
		_, err := LoadForImport(dir)
		wantFileError(t, err, filepath.Join(dir, ModelsFile), `models."openrouter/minimax-m3"`, "provider")
	})
}

// TestLoadDoesNotChmodThroughASymlink: the target of a symlinked
// providers.toml can be anywhere; Load reads through it but never changes its
// mode, and says so.
func TestLoadDoesNotChmodThroughASymlink(t *testing.T) {
	dir := writeFiles(t, "", validModels)
	target := filepath.Join(t.TempDir(), "elsewhere.toml")
	if err := os.WriteFile(target, []byte(validProviders), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil { // defeat the umask
		t.Fatal(err)
	}
	link := filepath.Join(dir, ProvidersFile)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m := mode(t, target); m != 0o644 {
		t.Fatalf("the symlink's target is now %04o: Load chmodded through the link", m)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], link+" is a symlink; permissions not changed") {
		t.Fatalf("Warnings = %q, want one naming the symlink", got.Warnings)
	}
	if strings.Contains(got.Warnings[0], canary) {
		t.Fatal("the warning carries file contents")
	}
	if !reflect.DeepEqual(got.Providers, validTable().Providers) {
		t.Fatalf("providers read through the link = %+v", got.Providers)
	}
}

// TestSaveReplacesASymlinkedProvidersFile pins what Save's doc comment says:
// the rename replaces the link with a regular 0600 file, and the file it
// pointed to is neither written through nor chmodded.
func TestSaveReplacesASymlinkedProvidersFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.toml")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ProvidersFile)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, validTable()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("providers.toml is %v, want a regular 0600 file", info.Mode())
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "original" || mode(t, target) != 0o644 {
		t.Fatalf("the old target was changed: %q, %04o", b, mode(t, target))
	}
}

func TestLoadStrictDecodeNamesFileTableAndKey(t *testing.T) {
	cases := []struct {
		name              string
		providers, models string
		file, table, key  string
	}{
		{
			name:      "provider field typo",
			providers: strings.Replace(validProviders, "driver = \"openai-compat\"", "driver = \"openai-compat\"\nbse_url = \"x\"", 1),
			models:    validModels,
			file:      ProvidersFile, table: "providers.fireworks", key: "bse_url",
		},
		{
			name:      "model field typo in a quoted alias",
			providers: validProviders,
			models:    strings.Replace(validModels, "context_window = 262144", "contxt_window = 262144", 1),
			file:      ModelsFile, table: `models."fireworks/kimi-k3"`, key: "contxt_window",
		},
		{
			name:      "top-level typo",
			providers: validProviders,
			models:    strings.Replace(validModels, "default_model", "default_model = \"fireworks/kimi-k3\"\ndefaultmodel", 1),
			file:      ModelsFile, table: "", key: "defaultmodel",
		},
		{
			name:      "a sub-table the schema lacks",
			providers: validProviders + "\n[providers.openrouter.extra_headers]\nX-Title = \"craze\"\n",
			models:    validModels,
			file:      ProvidersFile, table: "providers.openrouter", key: "extra_headers",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, tc.providers, tc.models)
			_, err := Load(dir)
			fe := wantFileError(t, err, filepath.Join(dir, tc.file), tc.table, tc.key)
			if fe.Reason != "unknown key" {
				t.Fatalf("reason = %q, want unknown key", fe.Reason)
			}
		})
	}
}

func TestLoadVersion(t *testing.T) {
	noVersion := func(s string) string { return strings.Replace(s, "version = 1\n", "", 1) }
	v2 := func(s string) string { return strings.Replace(s, "version = 1", "version = 2", 1) }
	cases := []struct {
		name, providers, models, file, reason string
	}{
		{"providers missing", noVersion(validProviders), validModels, ProvidersFile, "missing"},
		{"models missing", validProviders, noVersion(validModels), ModelsFile, "missing"},
		{"providers newer", v2(validProviders), validModels, ProvidersFile, "unsupported version 2"},
		// A newer file's new keys must not be reported as typos first.
		{"models newer with a new key", validProviders, strings.Replace(v2(validModels), "version = 2", "version = 2\nnew_in_v2 = true", 1), ModelsFile, "unsupported version 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, tc.providers, tc.models)
			_, err := Load(dir)
			fe := wantFileError(t, err, filepath.Join(dir, tc.file), "", "version")
			if !strings.Contains(fe.Reason, tc.reason) {
				t.Fatalf("reason = %q, want it to contain %q", fe.Reason, tc.reason)
			}
		})
	}
}

// TestLoadWrongTypeNamesTheKey: a value of the wrong type is a decode error
// that is not a syntax error; it names the key path and never the value.
func TestLoadWrongTypeNamesTheKey(t *testing.T) {
	dir := writeFiles(t, validProviders, strings.Replace(validModels, "context_window = 262144", `context_window = "big"`, 1))
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load accepted a string context_window")
	}
	for _, want := range []string{ModelsFile, "context_window"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
}

// TestLoadSyntaxErrorCarriesOnlyTheLine: BurntSushi's ParseError quotes a
// fragment of the offending line in its message and prints the whole line
// in ErrorWithPosition; for a malformed api_key line that line is the key.
func TestLoadSyntaxErrorCarriesOnlyTheLine(t *testing.T) {
	head := "version = 1\n\n[providers.x]\ndriver = \"openai-compat\"\n"
	cases := []struct {
		name, line string
		wantLine   int
	}{
		{"unterminated string", `api_key = "` + canary, 5},
		{"unquoted value", `api_key = ` + canary, 5},
		{"missing equals", `api_key "` + canary + `"`, 5},
		{"a key pasted alone", canary, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, head+tc.line+"\n", validModels)
			_, err := Load(dir)
			fe := wantFileError(t, err, filepath.Join(dir, ProvidersFile), "", "")
			if fe.Line != tc.wantLine {
				t.Errorf("line = %d, want %d", fe.Line, tc.wantLine)
			}
			msg := err.Error()
			// The path is ours; anything else that looks like the line is not.
			if rest := strings.ReplaceAll(msg, dir, ""); strings.Contains(rest, canary) || strings.Contains(rest, "sk") {
				t.Fatalf("syntax error echoes the source line: %q", msg)
			}
			if !strings.Contains(msg, "line 5") {
				t.Fatalf("err = %q, want it to name line 5", msg)
			}
		})
	}
}

func TestLoadSourceDefaultsToManualAndKeepsOthers(t *testing.T) {
	providers := strings.Replace(validProviders, "source = \"gx\"", "source = \"custom\"", 1)
	models := strings.Replace(validModels, "source = \"gx\"\n", "", 1)
	got, err := Load(writeFiles(t, providers, models))
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Providers["fireworks"].Source; s != "custom" {
		t.Errorf("an unknown source = %q, want it kept verbatim", s)
	}
	if s := got.Providers["openrouter"].Source; s != SourceManual {
		t.Errorf("an absent provider source = %q, want manual", s)
	}
	if s := got.Models["fireworks/kimi-k3"].Source; s != SourceManual {
		t.Errorf("an absent model source = %q, want manual", s)
	}
}

func TestLoadTightensALooseProvidersFile(t *testing.T) {
	dir := writeFiles(t, validProviders, validModels)
	path := filepath.Join(dir, ProvidersFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m := mode(t, path); m != 0o600 {
		t.Fatalf("mode after Load = %04o, want 0600", m)
	}
	if len(got.Warnings) != 1 {
		t.Fatalf("Warnings = %q, want exactly one", got.Warnings)
	}
	w := got.Warnings[0]
	if !strings.Contains(w, path) || !strings.Contains(w, "0644") {
		t.Fatalf("warning %q does not name the path and the old mode", w)
	}
	if strings.Contains(w, canary) || strings.Contains(w, "fireworks") {
		t.Fatalf("warning %q carries file contents", w)
	}

	// Already private: nothing to say.
	again, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Warnings) != 0 {
		t.Fatalf("a private file still warned: %q", again.Warnings)
	}
}

func TestValidateFailuresNameFileTableAndKey(t *testing.T) {
	const fw, kimi = "providers.fireworks", `models."fireworks/kimi-k3"`
	cases := []struct {
		name             string
		mutate           func(*Table)
		file, table, key string
	}{
		{"default_model missing", func(t *Table) { t.DefaultModel = "" }, ModelsFile, "", "default_model"},
		{"default_model unknown", func(t *Table) { t.DefaultModel = "nope" }, ModelsFile, "", "default_model"},
		{"driver missing", func(t *Table) { setProvider(t, "fireworks", func(p *Provider) { p.Driver = "" }) }, ProvidersFile, fw, "driver"},
		{"driver unknown", func(t *Table) { setProvider(t, "fireworks", func(p *Provider) { p.Driver = "anthropic" }) }, ProvidersFile, fw, "driver"},
		{"base_url missing for openai-compat", func(t *Table) { setProvider(t, "fireworks", func(p *Provider) { p.BaseURL = "" }) }, ProvidersFile, fw, "base_url"},
		{"base_url not a URL", func(t *Table) {
			setProvider(t, "fireworks", func(p *Provider) { p.BaseURL = "api.fireworks.example/v1" })
		}, ProvidersFile, fw, "base_url"},
		{"base_url for openrouter", func(t *Table) {
			setProvider(t, "openrouter", func(p *Provider) { p.BaseURL = "https://openrouter.ai/api/v1" })
		}, ProvidersFile, "providers.openrouter", "base_url"},
		{"env_keys empty entry", func(t *Table) { setProvider(t, "fireworks", func(p *Provider) { p.EnvKeys = []string{"A", " "} }) }, ProvidersFile, fw, "env_keys"},
		{"api_key under 8 bytes", func(t *Table) { setProvider(t, "fireworks", func(p *Provider) { p.APIKey = " zq-1234 " }) }, ProvidersFile, fw, "api_key"},
		{"api_key inside the redaction marker", func(t *Table) { setProvider(t, "fireworks", func(p *Provider) { p.APIKey = "credential" }) }, ProvidersFile, fw, "api_key"},
		{"empty provider id", func(t *Table) { t.Providers[""] = t.Providers["fireworks"] }, ProvidersFile, `providers.""`, ""},
		{"empty alias", func(t *Table) { t.Models[""] = t.Models["fireworks/kimi-k3"] }, ModelsFile, `models.""`, ""},
		{"model provider missing", func(t *Table) { setModel(t, "fireworks/kimi-k3", func(m *Model) { m.Provider = "" }) }, ModelsFile, kimi, "provider"},
		{"model provider unknown", func(t *Table) { setModel(t, "fireworks/kimi-k3", func(m *Model) { m.Provider = "nope" }) }, ModelsFile, kimi, "provider"},
		{"wire_model missing", func(t *Table) { setModel(t, "fireworks/kimi-k3", func(m *Model) { m.WireModel = "" }) }, ModelsFile, kimi, "wire_model"},
		{"context_window negative", func(t *Table) { setModel(t, "fireworks/kimi-k3", func(m *Model) { m.ContextWindow = -1 }) }, ModelsFile, kimi, "context_window"},
		{"max_output_tokens negative", func(t *Table) { setModel(t, "fireworks/kimi-k3", func(m *Model) { m.MaxOutputTokens = -1 }) }, ModelsFile, kimi, "max_output_tokens"},
		{"efforts empty entry", func(t *Table) { setModel(t, "fireworks/kimi-k3", func(m *Model) { m.Efforts = []string{"low", ""} }) }, ModelsFile, kimi, "efforts"},
		{"efforts duplicate", func(t *Table) {
			setModel(t, "fireworks/kimi-k3", func(m *Model) { m.Efforts = []string{"high", "high"} })
		}, ModelsFile, kimi, "efforts"},
		{"default_effort not in efforts", func(t *Table) { setModel(t, "fireworks/kimi-k3", func(m *Model) { m.DefaultEffort = "medium" }) }, ModelsFile, kimi, "default_effort"},
		{"default_effort without efforts", func(t *Table) {
			setModel(t, "fireworks/kimi-k3", func(m *Model) { m.Efforts, m.DefaultEffort = nil, "high" })
		}, ModelsFile, kimi, "default_effort"},
		{"tool_profile unknown", func(t *Table) { setModel(t, "fireworks/kimi-k3", func(m *Model) { m.ToolProfile = "gpt" }) }, ModelsFile, kimi, "tool_profile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tbl := validTable()
			tc.mutate(tbl)
			wantFileError(t, tbl.Validate(), tc.file, tc.table, tc.key)
		})
	}

	// The same rules run inside Load, naming the full path.
	dir := writeFiles(t, strings.Replace(validProviders, `driver = "openrouter"`, `driver = "openrouter"`+"\nbase_url = \"https://openrouter.ai/api/v1\"", 1), validModels)
	_, err := Load(dir)
	wantFileError(t, err, filepath.Join(dir, ProvidersFile), "providers.openrouter", "base_url")

	// A valid table stays valid: the cases above fail for their own reason.
	if err := validTable().Validate(); err != nil {
		t.Fatalf("validTable: %v", err)
	}
}

// TestLoadRefusesAShortKeyOnAnUnusedProvider: every provider's key is
// redacted from tool output, so the floor applies to providers no model
// names too — a short key there could reach a model through a file. The
// error names the provider and the key's place, never the key. An 8-byte key
// is the negative control; a blank one is no key at all and loads.
func TestLoadRefusesAShortKeyOnAnUnusedProvider(t *testing.T) {
	const short = "zq-1234" // 7 bytes, text no message would hold by accident
	spare := func(key string) string {
		return validProviders + "\n[providers.spare]\ndriver = \"openrouter\"\napi_key = \"" + key + "\"\n"
	}
	for _, key := range []string{short, "  " + short + "\t"} {
		dir := writeFiles(t, spare(key), validModels)
		_, err := Load(dir)
		fe := wantFileError(t, err, filepath.Join(dir, ProvidersFile), "providers.spare", "api_key")
		if msg := err.Error(); strings.Contains(msg, short) || !strings.Contains(msg, "spare") || !strings.Contains(msg, "8 bytes") {
			t.Fatalf("Load error %q must name the provider and the floor, and not the key", msg)
		}
		if strings.Contains(fmt.Sprintf("%+v", fe), short) {
			t.Fatal("the FileError's fields carry the key")
		}
	}
	for _, key := range []string{"zq-12345", "   "} {
		if _, err := Load(writeFiles(t, spare(key), validModels)); err != nil {
			t.Fatalf("api_key %q: %v", key, err)
		}
	}
}

// TestKeysCoversEveryProviderAndRefusesShortOnes: the redactor's key list is
// every provider's inline key and every set env_keys variable, used or not;
// an env value under 8 bytes, which Load cannot see, fails here naming the
// provider and the variable only.
func TestKeysCoversEveryProviderAndRefusesShortOnes(t *testing.T) {
	tbl := validTable() // fireworks: env FIREWORKS_API_KEY; openrouter: env OPENROUTER_API_KEY + inline canary
	setProvider(tbl, "fireworks", func(p *Provider) { p.EnvKeys = []string{"FIREWORKS_API_KEY", "FW_KEY"} })
	env := map[string]string{
		"FIREWORKS_API_KEY": " fw-key-0000001\n", // trimmed
		"FW_KEY":            "fw-key-0000002",    // not the one Resolve picks, still a key
		"UNRELATED":         "zq-1",
	}
	keys, err := tbl.Keys(fakeEnv(env))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range keys {
		got = append(got, k.Reveal())
	}
	if want := []string{"fw-key-0000001", "fw-key-0000002", canary}; !slices.Equal(got, want) {
		t.Fatalf("Keys = %q, want %q", got, want)
	}

	// An unused provider's short env value fails the whole list.
	env["OPENROUTER_API_KEY"] = "zq-1234"
	_, err = tbl.Keys(fakeEnv(env))
	if !errors.Is(err, ErrKeyTooShort) {
		t.Fatalf("err = %v, want ErrKeyTooShort", err)
	}
	if msg := err.Error(); strings.Contains(msg, "zq-1234") || !strings.Contains(msg, `"openrouter"`) || !strings.Contains(msg, "OPENROUTER_API_KEY") {
		t.Fatalf("err = %q must name the provider and the variable, and not the value", msg)
	}
	// The negative control: eight bytes pass.
	env["OPENROUTER_API_KEY"] = "zq-12345"
	if _, err := tbl.Keys(fakeEnv(env)); err != nil {
		t.Fatalf("an 8-byte env key: %v", err)
	}
}

// TestKeysTheMarkerPrintsBackAreRefused: a key inside the redaction marker
// ("credential"), holding it, or overlapping either end of it would come
// back out of the redactor verbatim, so Load refuses one inline and Keys one
// from the environment, naming the place and not the value. The negative
// controls: the redactor really does print such a key back, and an ordinary
// key of the same length passes both.
func TestKeysTheMarkerPrintsBackAreRefused(t *testing.T) {
	if out := redact.New("credential").String("pw=credential"); !strings.Contains(out, "credential") {
		t.Fatalf("redacting %q gave %q: the marker no longer contains it, so this test proves nothing", "credential", out)
	}
	spare := func(key string) string {
		return validProviders + "\n[providers.spare]\ndriver = \"openrouter\"\napi_key = \"" + key + "\"\n"
	}
	for _, key := range []string{"credential", "redacted", redact.Marker, "sk-live-" + redact.Marker[:6], "ential]-live-key"} {
		dir := writeFiles(t, spare(key), validModels)
		_, err := Load(dir)
		wantFileError(t, err, filepath.Join(dir, ProvidersFile), "providers.spare", "api_key")
		if msg := err.Error(); strings.Contains(msg, key) || !strings.Contains(msg, "redaction marker") {
			t.Fatalf("Load error %q must name the marker rule and not the key", msg)
		}

		tbl := validTable()
		_, err = tbl.Keys(fakeEnv(map[string]string{"OPENROUTER_API_KEY": key}))
		if !errors.Is(err, ErrKeyOverlapsMarker) {
			t.Fatalf("Keys with %q in the environment = %v, want ErrKeyOverlapsMarker", key, err)
		}
		if msg := err.Error(); strings.Contains(msg, key) || !strings.Contains(msg, "OPENROUTER_API_KEY") || !strings.Contains(msg, `"openrouter"`) {
			t.Fatalf("Keys error %q must name the provider and the variable, and not the value", msg)
		}
	}
	// Holding a piece of the marker is not overlapping it: redacting this key
	// leaves a marker that does not contain it.
	if _, err := Load(writeFiles(t, spare("credentialx"), validModels)); err != nil {
		t.Fatalf("an ordinary key: %v", err)
	}
	if _, err := validTable().Keys(fakeEnv(map[string]string{"OPENROUTER_API_KEY": "sk-live-0000"})); err != nil {
		t.Fatalf("an ordinary env key: %v", err)
	}
}

// TestSubagentsSectionRoundTrip: a table with a [subagents] section
// round-trips through Save/Load byte for byte, and one with none saves
// exactly as it did before the section existed (TestSaveWritesStableReadableFiles
// pins that byte-for-byte, over validTable(), which sets no Subagents).
func TestSubagentsSectionRoundTrip(t *testing.T) {
	tbl := validTable()
	tbl.Subagents = Subagents{
		Model:  "fireworks/kimi-k3",
		Effort: "high",
		Tiers: map[string]string{
			"opus":   "fireworks/kimi-k3",
			"sonnet": "openrouter/minimax-m3",
			"custom": "openrouter/minimax-m3",
		},
	}
	if err := tbl.Validate(); err != nil {
		t.Fatalf("a valid Subagents section: %v", err)
	}

	dir := t.TempDir()
	if err := Save(dir, tbl); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, tbl) {
		t.Fatalf("round trip =\n%+v\nwant\n%+v", got, tbl)
	}

	mb, err := os.ReadFile(filepath.Join(dir, ModelsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[subagents]\n",
		`model = "fireworks/kimi-k3"`,
		`effort = "high"`,
		"[subagents.tiers]\n",
		`custom = "openrouter/minimax-m3"`,
		`opus = "fireworks/kimi-k3"`,
		`sonnet = "openrouter/minimax-m3"`,
	} {
		if !strings.Contains(string(mb), want) {
			t.Fatalf("%s does not contain %q:\n%s", ModelsFile, want, mb)
		}
	}

	// Saving twice gives the same bytes (TestSaveWritesStableReadableFiles's
	// rule extended to a table that has a [subagents] section).
	dir2 := t.TempDir()
	if err := Save(dir2, tbl); err != nil {
		t.Fatal(err)
	}
	mb2, err := os.ReadFile(filepath.Join(dir2, ModelsFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(mb) != string(mb2) {
		t.Fatal("two saves of one table with a [subagents] section wrote different bytes")
	}
}

// TestSubagentsSectionOmittedWhenEmpty: an in-memory Subagents value that
// reads as "no section" (every field zero, even a present-but-empty Tiers
// map) never writes [subagents] at all, matching what Load returns for a
// models.toml with none.
func TestSubagentsSectionOmittedWhenEmpty(t *testing.T) {
	tbl := validTable()
	tbl.Subagents = Subagents{Tiers: map[string]string{}}
	dir := t.TempDir()
	if err := Save(dir, tbl); err != nil {
		t.Fatal(err)
	}
	mb, err := os.ReadFile(filepath.Join(dir, ModelsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mb), "subagents") {
		t.Fatalf("%s holds a [subagents] section for an empty one:\n%s", ModelsFile, mb)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Subagents, Subagents{}) {
		t.Fatalf("Subagents = %+v, want the zero value", got.Subagents)
	}
}

// TestSubagentsUnknownKeyIsStrict: an unknown key under [subagents] fails
// the same way any other unknown key does (decodeStrict, plan 026 §3.6).
func TestSubagentsUnknownKeyIsStrict(t *testing.T) {
	models := validModels + "\n[subagents]\nbogus = \"x\"\n"
	dir := writeFiles(t, validProviders, models)
	_, err := Load(dir)
	fe := wantFileError(t, err, filepath.Join(dir, ModelsFile), "subagents", "bogus")
	if fe.Reason != "unknown key" {
		t.Fatalf("reason = %q, want unknown key", fe.Reason)
	}
}

// TestValidateSubagentsFailures pins decision 1's rules: model and every
// tier value must be aliases in the table ("" allowed for model and
// effort); a tier key must match [a-z0-9-]+; an effort set together with a
// model must be one that model offers.
func TestValidateSubagentsFailures(t *testing.T) {
	cases := []struct {
		name             string
		subagents        Subagents
		file, table, key string
		// wantReason, when set, is a substring the FileError's Reason must
		// contain, so a case is pinned to its own rule and not just any
		// rejection at the same key (used by the "inherit" cases below,
		// which would otherwise also satisfy tierKeyPattern's generic
		// reason for the upper-case spelling).
		wantReason string
	}{
		{
			name: "model unknown", subagents: Subagents{Model: "nope"},
			file: ModelsFile, table: "subagents", key: "model",
		},
		{
			name:      "effort not offered when model is set",
			subagents: Subagents{Model: "openrouter/minimax-m3", Effort: "high"}, // that model has no Efforts
			file:      ModelsFile, table: "subagents", key: "effort",
		},
		{
			name:      "tier key bad format",
			subagents: Subagents{Tiers: map[string]string{"Opus": "fireworks/kimi-k3"}},
			file:      ModelsFile, table: "subagents.tiers", key: "Opus",
		},
		{
			// review r5, finding 1: "inherit" always means the parent's model
			// (resolveModelValue checks it before ever consulting
			// [subagents.tiers]), so a mapping under that key could never be
			// used.
			name:      "tier key is inherit",
			subagents: Subagents{Tiers: map[string]string{"inherit": "fireworks/kimi-k3"}},
			file:      ModelsFile, table: "subagents.tiers", key: "inherit",
			wantReason: "parent's model",
		},
		{
			// Case-insensitively: an upper-case spelling is caught by the same
			// rule, not just tierKeyPattern's generic "must match [a-z0-9-]+".
			name:      "tier key is inherit in another case",
			subagents: Subagents{Tiers: map[string]string{"INHERIT": "fireworks/kimi-k3"}},
			file:      ModelsFile, table: "subagents.tiers", key: "INHERIT",
			wantReason: "parent's model",
		},
		{
			name:      "tier key with an underscore",
			subagents: Subagents{Tiers: map[string]string{"my_tier": "fireworks/kimi-k3"}},
			file:      ModelsFile, table: "subagents.tiers", key: "my_tier",
		},
		{
			name:      "tier value empty",
			subagents: Subagents{Tiers: map[string]string{"opus": ""}},
			file:      ModelsFile, table: "subagents.tiers", key: "opus",
		},
		{
			name:      "tier value unknown",
			subagents: Subagents{Tiers: map[string]string{"opus": "nope"}},
			file:      ModelsFile, table: "subagents.tiers", key: "opus",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tbl := validTable()
			tbl.Subagents = tc.subagents
			fe := wantFileError(t, tbl.Validate(), tc.file, tc.table, tc.key)
			if tc.wantReason != "" && !strings.Contains(fe.Reason, tc.wantReason) {
				t.Fatalf("reason = %q, want it to contain %q", fe.Reason, tc.wantReason)
			}
		})
	}

	// A model-less effort is not checked at Validate: resolution checks it
	// against whichever model the child actually runs.
	tbl := validTable()
	tbl.Subagents = Subagents{Effort: "an-effort-no-model-offers"}
	if err := tbl.Validate(); err != nil {
		t.Fatalf("a model-less subagents.effort failed Validate: %v", err)
	}

	// A valid section still passes, so the cases above fail for their own
	// reason and not some other one.
	valid := validTable()
	valid.Subagents = Subagents{
		Model: "fireworks/kimi-k3", Effort: "high",
		Tiers: map[string]string{"opus": "fireworks/kimi-k3"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid Subagents section: %v", err)
	}
}

func setProvider(t *Table, id string, f func(*Provider)) {
	p := t.Providers[id]
	f(&p)
	t.Providers[id] = p
}

func setModel(t *Table, alias string, f func(*Model)) {
	m := t.Models[alias]
	f(&m)
	t.Models[alias] = m
}

// fakeEnv is a getenv over a fixed map, so no test reads the real
// environment.
func fakeEnv(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestResolveKeyOrder(t *testing.T) {
	cases := []struct {
		name    string
		envKeys []string
		inline  Secret
		env     map[string]string
		want    string
	}{
		{"first env var wins", []string{"A", "B"}, "", map[string]string{"A": "key-a", "B": "key-b"}, "key-a"},
		{"an empty env var is skipped", []string{"A", "B"}, "", map[string]string{"A": "", "B": "key-b"}, "key-b"},
		{"a blank env var is skipped", []string{"A", "B"}, "", map[string]string{"A": " \t", "B": "key-b"}, "key-b"},
		{"env beats inline", []string{"A"}, "key-inline", map[string]string{"A": "key-a"}, "key-a"},
		{"inline when no env var is set", []string{"A", "B"}, "key-inline", nil, "key-inline"},
		{"inline with no env_keys at all", nil, "key-inline", nil, "key-inline"},
		{"surrounding whitespace is trimmed", []string{"A"}, "", map[string]string{"A": " key-a\n"}, "key-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tbl := validTable()
			setProvider(tbl, "fireworks", func(p *Provider) { p.EnvKeys, p.APIKey = tc.envKeys, tc.inline })
			got, err := tbl.Resolve("fireworks/kimi-k3", fakeEnv(tc.env))
			if err != nil {
				t.Fatal(err)
			}
			if got.APIKey.Reveal() != tc.want {
				t.Fatalf("key = %q, want %q", got.APIKey.Reveal(), tc.want)
			}
		})
	}
}

func TestResolveNoAPIKeyNamesOnlyVariableNames(t *testing.T) {
	tbl := validTable()
	setProvider(tbl, "fireworks", func(p *Provider) { p.EnvKeys = []string{"FIREWORKS_API_KEY", "FW_KEY"} })
	// Set but blank; and a canary in an unrelated variable that must not
	// surface either.
	env := fakeEnv(map[string]string{"FIREWORKS_API_KEY": "  ", "OTHER": canary})
	_, err := tbl.Resolve("fireworks/kimi-k3", env)
	if !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("err = %v, want ErrNoAPIKey", err)
	}
	msg := err.Error()
	for _, want := range []string{`"fireworks"`, "FIREWORKS_API_KEY", "FW_KEY"} {
		if !strings.Contains(msg, want) {
			t.Errorf("err = %q, want it to contain %q", msg, want)
		}
	}
	if strings.Contains(msg, canary) {
		t.Fatalf("err = %q carries a value", msg)
	}

	// No env_keys and no inline key says so rather than listing nothing.
	setProvider(tbl, "fireworks", func(p *Provider) { p.EnvKeys = nil })
	_, err = tbl.Resolve("fireworks/kimi-k3", env)
	if !errors.Is(err, ErrNoAPIKey) || !strings.Contains(err.Error(), "names no env_keys") {
		t.Fatalf("err = %v, want ErrNoAPIKey saying there are no env_keys", err)
	}

	// Only the models on the unfunded provider fail.
	if _, err := tbl.Resolve("openrouter/minimax-m3", env); err != nil {
		t.Fatalf("a funded provider failed: %v", err)
	}
}

func TestResolveFields(t *testing.T) {
	tbl := validTable()
	env := fakeEnv(map[string]string{"FIREWORKS_API_KEY": "key-fw"})
	got, err := tbl.Resolve("fireworks/kimi-k3", env)
	if err != nil {
		t.Fatal(err)
	}
	want := Resolved{
		Alias:           "fireworks/kimi-k3",
		ProviderID:      "fireworks",
		Driver:          DriverOpenAICompat,
		BaseURL:         "https://api.fireworks.example/inference/v1",
		APIKey:          "key-fw",
		WireModel:       "accounts/fireworks/models/kimi-k3",
		Name:            "Kimi K3 (Fireworks)",
		ContextWindow:   262144,
		MaxOutputTokens: 32768,
		Efforts:         []string{"low", "high", "max"},
		DefaultEffort:   "high",
		Vision:          true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve =\n%+v\nwant\n%+v", got, want)
	}
	// The caller's copy of Efforts is its own.
	got.Efforts[0] = "changed"
	if tbl.Models["fireworks/kimi-k3"].Efforts[0] != "low" {
		t.Fatal("Resolve returned the table's own Efforts slice")
	}

	// A model with no name shows its alias; with no effort control, none.
	or, err := tbl.Resolve("openrouter/minimax-m3", env)
	if err != nil {
		t.Fatal(err)
	}
	if or.Name != "openrouter/minimax-m3" || or.BaseURL != "" || or.Efforts != nil || or.MaxOutputTokens != 0 {
		t.Fatalf("Resolve(openrouter/minimax-m3) = %+v", or)
	}

	if _, err := tbl.Resolve("nope", env); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("err = %v, want ErrUnknownModel", err)
	}
}
