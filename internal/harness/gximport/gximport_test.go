package gximport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/modeltable"
)

// canary is the only key in any fixture. It is obviously not a secret, and
// every test that could leak a key looks for it.
const canary = "sk-canary-not-a-secret"

// fixture is the synthetic gx home: config.toml plus a providers.toml layered
// over it, covering every filter, the layering, and the field mapping.
const fixture = "testdata/gx"

// fixtureTable is what fixture imports as with no existing table.
func fixtureTable() *modeltable.Table {
	return &modeltable.Table{
		DefaultModel: "fireworks/kimi-k3",
		Providers: map[string]modeltable.Provider{
			"fireworks": {
				Driver:  modeltable.DriverOpenAICompat,
				BaseURL: "https://api.fireworks.example/inference/v1",
				EnvKeys: []string{"FIREWORKS_API_KEY", "FW_API_KEY"},
				Source:  modeltable.SourceGX,
			},
			"zai": {
				Driver:  modeltable.DriverOpenAICompat,
				BaseURL: "https://api.zai.example/api/paas/v4",
				EnvKeys: []string{"ZAI_API_KEY", "ZHIPUAI_API_KEY"},
				APIKey:  canary,
				Source:  modeltable.SourceGX,
			},
			"openrouter": {
				Driver:  modeltable.DriverOpenRouter,
				EnvKeys: []string{"OPENROUTER_API_KEY"},
				Source:  modeltable.SourceGX,
			},
			"meta": {
				Driver:  modeltable.DriverOpenAICompat,
				BaseURL: "https://api.meta.example/v1",
				EnvKeys: []string{"META_API_KEY"},
				Source:  modeltable.SourceGX,
			},
		},
		Models: map[string]modeltable.Model{
			"fireworks/kimi-k3": {
				Provider:        "fireworks",
				WireModel:       "accounts/fireworks/models/kimi-k3",
				Name:            "Kimi K3 (Fireworks)",
				ContextWindow:   262144,
				MaxOutputTokens: 65536,
				Efforts:         []string{"low", "high"},
				DefaultEffort:   "high",
				Source:          modeltable.SourceGX,
			},
			"glm-5.3": {
				Provider:      "zai",
				WireModel:     "glm-5.3",
				Name:          "GLM 5.3",
				ContextWindow: 200000,
				Source:        modeltable.SourceGX,
			},
			"glm-5.3-flash": {
				Provider:        "zai",
				WireModel:       "glm-5.3-flash",
				Name:            "GLM 5.3 Flash",
				ContextWindow:   200000,
				MaxOutputTokens: 16384,
				Source:          modeltable.SourceGX,
			},
			"openrouter/gemini-3.8-flash": {
				Provider:      "openrouter",
				WireModel:     "google/gemini-3.8-flash",
				Name:          "Gemini 3.8 Flash",
				ContextWindow: 1048576,
				Efforts:       []string{"low", "high"},
				DefaultEffort: "high",
				Vision:        true,
				Source:        modeltable.SourceGX,
			},
			"muse-spark-1.3": {
				Provider:        "meta",
				WireModel:       "muse-spark-1.3",
				Name:            "Muse Spark 1.3",
				ContextWindow:   1000000,
				MaxOutputTokens: 131072,
				Efforts:         []string{"low", "high"},
				Source:          modeltable.SourceGX,
			},
			"openrouter/minimax-m3": {
				Provider:      "openrouter",
				WireModel:     "minimax/minimax-m3",
				Name:          "MiniMax M3",
				ContextWindow: 204800,
				Source:        modeltable.SourceGX,
			},
		},
	}
}

var (
	fixtureProviderSkips = []Skip{
		{ID: "chatgpt", Reason: "auth command providers are not supported (ChatGPT-plan auth is deferred)"},
		{ID: "headers", Reason: "extra_headers not supported yet"},
		{ID: "openai", Reason: "Responses API providers are not supported yet"},
	}
	fixtureModelSkips = []Skip{
		{ID: "chatgpt/codex", Reason: `provider "chatgpt" was skipped: auth command providers are not supported (ChatGPT-plan auth is deferred)`},
		{ID: "ghost/model", Reason: `provider "ghost" is not defined`},
		{ID: "grok-4.5", Reason: "no model_provider (a gx built-in model override, not a provider's model)"},
		{ID: "headers/one", Reason: `provider "headers" was skipped: extra_headers not supported yet`},
		{ID: "responses/big", Reason: `provider "openai" was skipped: Responses API providers are not supported yet`},
	}
)

func TestImportFixture(t *testing.T) {
	got, report, err := Import(fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := fixtureTable(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Import =\n%+v\nwant\n%+v", got, want)
	}
	want := Report{
		Providers: Changes{Added: []string{"fireworks", "meta", "openrouter", "zai"}, Skipped: fixtureProviderSkips},
		Models: Changes{
			Added: []string{
				"fireworks/kimi-k3", "glm-5.3", "glm-5.3-flash", "muse-spark-1.3",
				"openrouter/gemini-3.8-flash", "openrouter/minimax-m3",
			},
			Skipped: fixtureModelSkips,
		},
		// gx's default names a skipped model; providers.toml's [models]
		// default is not a layered table, so it is not considered.
		DefaultModel: "fireworks/kimi-k3",
		DefaultRule:  DefaultFirst,
	}
	if !reflect.DeepEqual(report, want) {
		t.Fatalf("report =\n%+v\nwant\n%+v", report, want)
	}
}

// TestImportLayering pins gx's providers-layer rules on the fixture: tables
// merge key by key (fireworks keeps config.toml's base_url and takes
// providers.toml's env_key), a scalar is overridden, an array is replaced
// whole rather than merged, and only the three layered tables are taken from
// providers.toml.
func TestImportLayering(t *testing.T) {
	got, report, err := Import(fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	fw := got.Providers["fireworks"]
	if fw.BaseURL != "https://api.fireworks.example/inference/v1" {
		t.Errorf("fireworks base_url = %q: the layer replaced the table instead of merging it", fw.BaseURL)
	}
	if want := []string{"FIREWORKS_API_KEY", "FW_API_KEY"}; !slices.Equal(fw.EnvKeys, want) {
		t.Errorf("fireworks env_keys = %v, want the layer's %v", fw.EnvKeys, want)
	}
	kimi := got.Models["fireworks/kimi-k3"]
	if kimi.MaxOutputTokens != 65536 {
		t.Errorf("kimi max_output_tokens = %d, want the layer's 65536", kimi.MaxOutputTokens)
	}
	if want := []string{"low", "high"}; !slices.Equal(kimi.Efforts, want) {
		t.Errorf("kimi efforts = %v, want the layer's array whole, %v", kimi.Efforts, want)
	}
	if kimi.Name != "Kimi K3 (Fireworks)" || kimi.ContextWindow != 262144 {
		t.Errorf("kimi lost config.toml's fields the layer does not set: %+v", kimi)
	}
	if _, ok := got.Models["muse-spark-1.3"]; !ok {
		t.Error("a model only the layer has was not imported")
	}
	if report.DefaultModel == "glm-5.3" {
		t.Error("providers.toml's [models] default was layered; gx takes only model, model_providers, auth_provider")
	}
}

func TestImportFieldMapping(t *testing.T) {
	got, _, err := Import(fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		alias         string
		wantEfforts   []string
		wantDefault   string
		wantMaxOutput int
	}{
		{"fireworks/kimi-k3", []string{"low", "high"}, "high", 65536},
		// supports_reasoning_effort = false, with a list: no efforts.
		{"glm-5.3", nil, "", 0},
		// supports_reasoning_effort absent, with a list: no efforts.
		{"glm-5.3-flash", nil, "", 16384},
		// Table-form list, a repeated value dropped, the flagged default.
		{"openrouter/gemini-3.8-flash", []string{"low", "high"}, "high", 0},
		// reasoning_effort not offered by the list: no default.
		{"muse-spark-1.3", []string{"low", "high"}, "", 131072},
	}
	for _, tc := range cases {
		m := got.Models[tc.alias]
		if !slices.Equal(m.Efforts, tc.wantEfforts) || m.DefaultEffort != tc.wantDefault {
			t.Errorf("%s efforts = %v default %q, want %v default %q", tc.alias, m.Efforts, m.DefaultEffort, tc.wantEfforts, tc.wantDefault)
		}
		if m.MaxOutputTokens != tc.wantMaxOutput {
			t.Errorf("%s max_output_tokens = %d, want %d", tc.alias, m.MaxOutputTokens, tc.wantMaxOutput)
		}
	}
	if w := got.Models["glm-5.3-flash"].WireModel; w != "glm-5.3-flash" {
		t.Errorf("a model with no `model` has wire id %q, want its alias", w)
	}
	if c := got.Models["muse-spark-1.3"].ContextWindow; c != 1000000 {
		t.Errorf("muse context_window = %d, want its provider's 1000000", c)
	}
	if !got.Models["openrouter/gemini-3.8-flash"].Vision || got.Models["glm-5.3"].Vision {
		t.Error("vision does not follow supports_vision")
	}
	if want := []string{"ZAI_API_KEY", "ZHIPUAI_API_KEY"}; !slices.Equal(got.Providers["zai"].EnvKeys, want) {
		t.Errorf("zai env_keys = %v, want %v (list order kept, blank dropped)", got.Providers["zai"].EnvKeys, want)
	}
	if got.Providers["zai"].APIKey.Reveal() != canary {
		t.Error("zai's inline key was not carried over")
	}
}

// TestImportDriverRule: the openrouter driver has no base-URL option, so an
// openrouter.ai URL becomes that driver only when dropping it loses nothing;
// otherwise the provider is skipped, naming what differs and never echoing
// userinfo. Every other host stays openai-compat with its URL.
func TestImportDriverRule(t *testing.T) {
	const fixed = "the openrouter driver's endpoint is fixed"
	cases := []struct {
		baseURL, wantDriver, wantBase string
		skip                          string // a reason fragment when p must be skipped
	}{
		{baseURL: "https://openrouter.ai/api/v1", wantDriver: modeltable.DriverOpenRouter},
		{baseURL: "https://openrouter.ai/api/v1/", wantDriver: modeltable.DriverOpenRouter},
		{baseURL: "https://OpenRouter.AI/api/v1", wantDriver: modeltable.DriverOpenRouter},
		{baseURL: "https://openrouter.ai:443/api/v1", wantDriver: modeltable.DriverOpenRouter},
		{baseURL: "http://openrouter.ai/api/v1", skip: "must be https"},
		{baseURL: "https://user:hunter2@openrouter.ai/api/v1", skip: "with userinfo"},
		{baseURL: "https://openrouter.ai:8443/api/v1", skip: "non-default port"},
		{baseURL: "https://openrouter.ai/api/v2", skip: "the path /api/v1"},
		{baseURL: "https://openrouter.ai/", skip: "the path /api/v1"},
		{baseURL: "https://openrouter.ai/api/v1?route=fallback", skip: "query or fragment"},
		{baseURL: "https://api.openrouter.ai.example/v1", wantDriver: modeltable.DriverOpenAICompat, wantBase: "https://api.openrouter.ai.example/v1"},
		{baseURL: "https://proxy.example/openrouter.ai/v1", wantDriver: modeltable.DriverOpenAICompat, wantBase: "https://proxy.example/openrouter.ai/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.baseURL, func(t *testing.T) {
			dir := gxHome(t, okProvider+fmt.Sprintf(`
[model_providers.p]
base_url = %q
env_key = "P_KEY"

[model."p/m"]
model_provider = "p"
`, tc.baseURL), "")
			got, report, err := Import(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			p, imported := got.Providers["p"]
			if tc.skip == "" {
				if !imported || p.Driver != tc.wantDriver || p.BaseURL != tc.wantBase {
					t.Fatalf("p = %+v (imported %v), want driver %q base_url %q", p, imported, tc.wantDriver, tc.wantBase)
				}
				return
			}
			if imported {
				t.Fatalf("p was imported as %+v, want it skipped", p)
			}
			want := Skip{ID: "p", Reason: tc.skip}
			i := slices.IndexFunc(report.Providers.Skipped, func(s Skip) bool { return s.ID == "p" })
			if i < 0 || !strings.Contains(report.Providers.Skipped[i].Reason, tc.skip) || !strings.Contains(report.Providers.Skipped[i].Reason, fixed) {
				t.Fatalf("skipped = %+v, want %+v", report.Providers.Skipped, want)
			}
			if out := fmt.Sprintf("%+v", report); strings.Contains(out, "hunter2") || strings.Contains(out, "user:") {
				t.Fatalf("the report carries the URL's userinfo: %s", out)
			}
		})
	}
}

// TestImportDropsAVarAPIKeyWithANote: gx expands a $VAR api_key at load; the
// import cannot, so it drops only the inline key, keeps the provider and its
// env_keys, and says so without echoing the value.
func TestImportDropsAVarAPIKeyWithANote(t *testing.T) {
	dir := gxHome(t, okProvider+`
[model_providers.p]
base_url = "https://p.example/v1"
env_key = "P_KEY"
api_key = "${`+canary+`}"

[model."p/m"]
model_provider = "p"
`, "")
	got, report, err := Import(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := modeltable.Provider{
		Driver:  modeltable.DriverOpenAICompat,
		BaseURL: "https://p.example/v1",
		EnvKeys: []string{"P_KEY"},
		Source:  modeltable.SourceGX,
	}
	if !reflect.DeepEqual(got.Providers["p"], want) {
		t.Fatalf("p = %+v, want %+v (no inline key)", got.Providers["p"], want)
	}
	if _, ok := got.Models["p/m"]; !ok {
		t.Fatal("the provider's model was not imported")
	}
	if wantNotes := []Note{{ID: "p", Text: noteVarAPIKey}}; !reflect.DeepEqual(report.Providers.Notes, wantNotes) {
		t.Fatalf("notes = %+v, want %+v", report.Providers.Notes, wantNotes)
	}
	if strings.Contains(fmt.Sprintf("%+v", report), canary) {
		t.Fatal("the report echoes the api_key value")
	}
}

// TestImportDropsAShortAPIKeyWithANote: the model table refuses an inline
// key under 8 bytes, or one the redaction marker could print back, at load,
// so an import that carried one over would write a table nothing can load.
// It drops only the key, like a $VAR one, and the saved table loads; an
// 8-byte key is the negative control, imported as is.
func TestImportDropsAShortAPIKeyWithANote(t *testing.T) {
	const short, eight, marker = "zq-1234", "zq-12345", "redacted-cred"
	for _, tc := range []struct {
		key     string
		wantKey string
		notes   []Note
	}{
		{short, "", []Note{{ID: "p", Text: noteShortAPIKey}}},
		{marker, "", []Note{{ID: "p", Text: noteMarkerAPIKey}}},
		{eight, eight, nil},
	} {
		dir := gxHome(t, okProvider+`
[model_providers.p]
base_url = "https://p.example/v1"
env_key = "P_KEY"
api_key = "`+tc.key+`"

[model."p/m"]
model_provider = "p"
`, "")
		got, report, err := Import(dir, nil)
		if err != nil {
			t.Fatalf("key %q: %v", tc.key, err)
		}
		if k := got.Providers["p"].APIKey.Reveal(); k != tc.wantKey {
			t.Fatalf("key %q: imported api_key %q, want %q", tc.key, k, tc.wantKey)
		}
		if !reflect.DeepEqual(report.Providers.Notes, tc.notes) {
			t.Fatalf("key %q: notes = %+v, want %+v", tc.key, report.Providers.Notes, tc.notes)
		}
		if tc.wantKey == "" && strings.Contains(fmt.Sprintf("%+v", report), tc.key) {
			t.Fatal("the report echoes the api_key value")
		}
		out := t.TempDir()
		if err := modeltable.Save(out, got); err != nil {
			t.Fatalf("key %q: saving the import: %v", tc.key, err)
		}
		if _, err := modeltable.Load(out); err != nil {
			t.Fatalf("key %q: the saved import does not load: %v", tc.key, err)
		}
	}
}

// TestImportCompletesAHalfWrittenPair is the recovery Save's write order and
// LoadForImport exist for: a first import interrupted between its two writes
// leaves providers.toml alone; Load refuses that, and re-running the import
// through LoadForImport keeps the lone file's hand-made entries and writes a
// complete, loadable pair.
func TestImportCompletesAHalfWrittenPair(t *testing.T) {
	first, _, err := Import(fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The owner hand-added a provider before the interruption.
	first.Providers["local"] = modeltable.Provider{Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:8080/v1", Source: modeltable.SourceManual}
	dir := filepath.Join(t.TempDir(), "native")
	if err := modeltable.Save(dir, first); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, modeltable.ModelsFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := modeltable.Load(dir); err == nil {
		t.Fatal("Load accepted a lone providers.toml")
	}

	existing, err := modeltable.LoadForImport(dir)
	if err != nil {
		t.Fatal(err)
	}
	merged, report, err := Import(fixture, existing)
	if err != nil {
		t.Fatal(err)
	}
	if err := modeltable.Save(dir, merged); err != nil {
		t.Fatal(err)
	}
	back, err := modeltable.Load(dir)
	if err != nil {
		t.Fatalf("the pair is still incomplete: %v", err)
	}
	if !reflect.DeepEqual(back.Providers["local"], first.Providers["local"]) {
		t.Fatalf("local = %+v, want the hand-made entry kept", back.Providers["local"])
	}
	if !reflect.DeepEqual(back.Models, fixtureTable().Models) || back.DefaultModel != "fireworks/kimi-k3" {
		t.Fatalf("models = %v default %q, want the fixture's", back.Aliases(), back.DefaultModel)
	}
	if report.DefaultRule != DefaultFirst || !slices.Equal(report.Providers.Kept, []string{"local"}) {
		t.Fatalf("report = %+v, want local kept and the default chosen afresh", report)
	}
}

// okProvider is a provider and model that always import, so a filter case
// never ends in ErrNothingToImport.
const okProvider = `
[model_providers.ok]
base_url = "https://ok.example/v1"
env_key = "OK_KEY"

[model."ok/m"]
model_provider = "ok"
`

func TestImportFilters(t *testing.T) {
	cases := []struct {
		name    string
		toml    string
		model   bool   // the skipped id is a model's (else a provider's)
		id      string // the id that must be skipped; "" means none is
		imports string // or: the id that must be imported
		reason  string
	}{
		{name: "Responses backend", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\napi_backend = \"responses\"\n", id: "p", reason: "Responses API providers are not supported yet"},
		{name: "Messages backend", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\napi_backend = \"messages\"\n", id: "p", reason: "Messages API providers are not supported yet"},
		{name: "an unknown backend is not echoed", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\napi_backend = \"grpc-v9\"\n", id: "p", reason: "api_backend is not chat_completions"},
		{name: "absent backend is Chat Completions", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\n", imports: "p"},
		{name: "auth command", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\n[model_providers.p.auth]\ncommand = \"helper\"\n", id: "p", reason: "auth command providers are not supported"},
		{name: "auth_provider helper", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\nauth_provider = \"sso\"\n", id: "p", reason: "auth_provider helpers are not supported"},
		{name: "extra_headers", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\n[model_providers.p.extra_headers]\nX-A = \"b\"\n", id: "p", reason: "extra_headers not supported yet"},
		{name: "empty extra_headers adds nothing", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\n[model_providers.p.extra_headers]\n", imports: "p"},
		{name: "query_params", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\nquery_params = { v = \"1\" }\n", id: "p", reason: "query_params not supported yet"},
		{name: "env_http_headers", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\nenv_http_headers = { X-Key = \"P_HDR\" }\n", id: "p", reason: "env_http_headers not supported yet"},
		{name: "api_base_url", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\napi_base_url = \"https://q.example\"\n", id: "p", reason: "api_base_url not supported yet"},
		{name: "no base_url", toml: "[model_providers.p]\nenv_key = \"P\"\n", id: "p", reason: "no base_url"},
		{name: "relative base_url", toml: "[model_providers.p]\nbase_url = \"p.example/v1\"\n", id: "p", reason: "base_url is not an absolute http or https URL"},
		{name: "$VAR base_url", toml: "[model_providers.p]\nbase_url = \"https://p.example/${REGION}\"\n", id: "p", reason: "base_url holds a $VAR reference"},
		{name: "$VAR env_key", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\nenv_key = \"$P_KEY_NAME\"\n", id: "p", reason: "env_key holds a $VAR reference"},
		{name: "$VAR in an env_key list", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\nenv_key = [\"P_KEY\", \"${OTHER}\"]\n", id: "p", reason: "env_key holds a $VAR reference"},
		{name: "$VAR api_key costs only the key", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\nenv_key = \"P_KEY\"\napi_key = \"${P_KEY}\"\n", imports: "p"},
		{name: "env_key of the wrong type", toml: "[model_providers.p]\nbase_url = \"https://p.example\"\nenv_key = 7\n", id: "p", reason: "env_key has the wrong type"},
		{name: "not a table", toml: "[model_providers]\np = \"x\"\n", id: "p", reason: "not a table"},
		{name: "an empty provider id", toml: "[model_providers.\"\"]\nbase_url = \"https://p.example\"\n", id: "", reason: "an empty provider id"},
		{name: "an empty alias", model: true, toml: "[model.\" \"]\nmodel_provider = \"ok\"\n", id: " ", reason: "an empty alias"},

		{name: "model-level base_url", model: true, toml: "[model.\"ok/x\"]\nmodel_provider = \"ok\"\nbase_url = \"https://elsewhere.example\"\n", id: "ok/x", reason: "a model-level base_url is not supported"},
		{name: "model-level api_key", model: true, toml: "[model.\"ok/x\"]\nmodel_provider = \"ok\"\napi_key = \"k\"\n", id: "ok/x", reason: "a model-level api_key is not supported"},
		{name: "model-level extra_headers", model: true, toml: "[model.\"ok/x\"]\nmodel_provider = \"ok\"\n[model.\"ok/x\".extra_headers]\nX-A = \"b\"\n", id: "ok/x", reason: "a model-level extra_headers is not supported"},
		{name: "$VAR wire id", model: true, toml: "[model.\"ok/x\"]\nmodel_provider = \"ok\"\nmodel = \"${MODEL_ID}\"\n", id: "ok/x", reason: "model (the wire id) holds a $VAR reference"},
		{name: "model on a Responses backend", model: true, toml: "[model.\"ok/x\"]\nmodel_provider = \"ok\"\napi_backend = \"responses\"\n", id: "ok/x", reason: "Responses API providers are not supported yet"},
		{name: "model backend matching its provider", model: true, toml: "[model.\"ok/x\"]\nmodel_provider = \"ok\"\napi_backend = \"chat_completions\"\n", imports: "ok/x"},
		{name: "context_window of the wrong type", model: true, toml: "[model.\"ok/x\"]\nmodel_provider = \"ok\"\ncontext_window = \"big\"\n", id: "ok/x", reason: "context_window has the wrong type"},
		{name: "negative max_completion_tokens", model: true, toml: "[model.\"ok/x\"]\nmodel_provider = \"ok\"\nmax_completion_tokens = -1\n", id: "ok/x", reason: "max_completion_tokens is negative"},
		{name: "reasoning_efforts of the wrong type", model: true, toml: "[model.\"ok/x\"]\nmodel_provider = \"ok\"\nsupports_reasoning_effort = true\nreasoning_efforts = [1, 2]\n", id: "ok/x", reason: "reasoning_efforts has the wrong type"},
		{name: "reasoning_efforts table with no value", model: true, toml: "[model.\"ok/x\"]\nmodel_provider = \"ok\"\nreasoning_efforts = [{ label = \"Hi\" }]\n", id: "ok/x", reason: "reasoning_efforts has the wrong type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, report, err := Import(gxHome(t, okProvider+tc.toml, ""), nil)
			if err != nil {
				t.Fatal(err)
			}
			changes, has := report.Providers, func(id string) bool { _, ok := got.Providers[id]; return ok }
			if tc.model {
				changes, has = report.Models, func(id string) bool { _, ok := got.Models[id]; return ok }
			}
			if tc.imports != "" {
				if !has(tc.imports) {
					t.Fatalf("%q was not imported; skipped: %+v", tc.imports, changes.Skipped)
				}
				return
			}
			if has(tc.id) {
				t.Fatalf("%q was imported, want it skipped", tc.id)
			}
			i := slices.IndexFunc(changes.Skipped, func(s Skip) bool { return s.ID == tc.id })
			if i < 0 {
				t.Fatalf("%q is not reported skipped: %+v", tc.id, changes.Skipped)
			}
			if r := changes.Skipped[i].Reason; !strings.Contains(r, tc.reason) {
				t.Fatalf("reason = %q, want it to contain %q", r, tc.reason)
			}
			if strings.Contains(changes.Skipped[i].Reason, "grpc-v9") {
				t.Fatal("the reason echoes a value")
			}
		})
	}
}

// existingTable is an earlier import plus the owner's own entries, set up so
// the fixture exercises every merge outcome.
func existingTable() *modeltable.Table {
	fx := fixtureTable()
	fwOld := fx.Providers["fireworks"]
	fwOld.BaseURL = "https://old.fireworks.example/v1"
	kimiOld := fx.Models["fireworks/kimi-k3"]
	kimiOld.MaxOutputTokens = 1024
	return &modeltable.Table{
		DefaultModel: "local/model",
		Providers: map[string]modeltable.Provider{
			"fireworks":  fwOld,               // gx, changed: updated
			"zai":        fx.Providers["zai"], // gx, identical: unchanged
			"openrouter": {Driver: modeltable.DriverOpenRouter, EnvKeys: []string{"MY_OR_KEY"}, Source: modeltable.SourceManual},
			"local":      {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:8080/v1", Source: "custom"},
			"retired":    {Driver: modeltable.DriverOpenAICompat, BaseURL: "https://retired.example/v1", Source: modeltable.SourceGX},
			"headers":    {Driver: modeltable.DriverOpenAICompat, BaseURL: "https://headers.example/v1", Source: modeltable.SourceGX},
		},
		Models: map[string]modeltable.Model{
			"fireworks/kimi-k3":     kimiOld,              // gx, changed: updated
			"glm-5.3":               fx.Models["glm-5.3"], // gx, identical: unchanged
			"openrouter/minimax-m3": {Provider: "openrouter", WireModel: "minimax/minimax-m3", Name: "Mine", Source: modeltable.SourceManual},
			"local/model":           {Provider: "local", WireModel: "qwen", Source: "custom"},
			"retired/model":         {Provider: "retired", WireModel: "old-1", Source: modeltable.SourceGX},
		},
	}
}

func TestImportMergeRule(t *testing.T) {
	existing := existingTable()
	if err := existing.Validate(); err != nil {
		t.Fatalf("existingTable is invalid: %v", err)
	}
	got, report, err := Import(fixture, existing)
	if err != nil {
		t.Fatal(err)
	}

	wantReport := Report{
		Providers: Changes{
			Added:     []string{"meta"},
			Updated:   []string{"fireworks"},
			Unchanged: []string{"zai"},
			Kept:      []string{"local", "openrouter"},
			// headers is also Skipped: gx still has it but it no longer
			// passes the filters.
			Stale:   []string{"headers", "retired"},
			Skipped: fixtureProviderSkips,
		},
		Models: Changes{
			Added:     []string{"glm-5.3-flash", "muse-spark-1.3", "openrouter/gemini-3.8-flash"},
			Updated:   []string{"fireworks/kimi-k3"},
			Unchanged: []string{"glm-5.3"},
			Kept:      []string{"local/model", "openrouter/minimax-m3"},
			Stale:     []string{"retired/model"},
			Skipped:   fixtureModelSkips,
		},
		DefaultModel: "local/model",
		DefaultRule:  DefaultKept,
	}
	if !reflect.DeepEqual(report, wantReport) {
		t.Fatalf("report =\n%+v\nwant\n%+v", report, wantReport)
	}

	fx, before := fixtureTable(), existingTable()
	// gx entries are replaced wholesale.
	if !reflect.DeepEqual(got.Providers["fireworks"], fx.Providers["fireworks"]) {
		t.Errorf("fireworks = %+v, want gx's", got.Providers["fireworks"])
	}
	if !reflect.DeepEqual(got.Models["fireworks/kimi-k3"], fx.Models["fireworks/kimi-k3"]) {
		t.Errorf("kimi = %+v, want gx's", got.Models["fireworks/kimi-k3"])
	}
	// Manual and other-source entries are untouched, even where gx has one
	// by the same name; stale entries are kept. Nothing is deleted.
	for id, p := range before.Providers {
		if id == "fireworks" {
			continue
		}
		if !reflect.DeepEqual(got.Providers[id], p) {
			t.Errorf("provider %s = %+v, want it kept as %+v", id, got.Providers[id], p)
		}
	}
	for alias, m := range before.Models {
		if alias == "fireworks/kimi-k3" {
			continue
		}
		if !reflect.DeepEqual(got.Models[alias], m) {
			t.Errorf("model %s = %+v, want it kept as %+v", alias, got.Models[alias], m)
		}
	}
	// The caller's table is not written through.
	if !reflect.DeepEqual(existing, existingTable()) {
		t.Error("Import modified the existing table")
	}
}

// TestImportIsIdempotent: importing over the previous import's result
// changes nothing and says so.
func TestImportIsIdempotent(t *testing.T) {
	first, _, err := Import(fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, report, err := Import(fixture, first)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("re-import changed the table:\n%+v\nwant\n%+v", second, first)
	}
	for kind, c := range map[string]Changes{"providers": report.Providers, "models": report.Models} {
		if len(c.Added)+len(c.Updated)+len(c.Kept)+len(c.Stale) != 0 {
			t.Errorf("%s: a no-op re-import reported changes: %+v", kind, c)
		}
	}
	if len(report.Models.Unchanged) != len(first.Models) || report.DefaultRule != DefaultKept {
		t.Errorf("report = %+v, want every model unchanged and the default kept", report)
	}
}

// TestImportKeepsAManualToolProfile: an import never writes tool_profile,
// so the owner's choice survives a re-import, Save and Load on a manual
// model; on a gx-sourced model the import replaces the entry wholesale and
// the hand edit goes — the negative control, and the rule models.toml's
// header states.
func TestImportKeepsAManualToolProfile(t *testing.T) {
	first, _, err := Import(fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	for alias, m := range first.Models {
		if m.ToolProfile != "" {
			t.Fatalf("the import wrote tool_profile %q on %s", m.ToolProfile, alias)
		}
	}
	manual := first.Models["glm-5.3"]
	manual.ToolProfile, manual.Source = "opencode", modeltable.SourceManual
	first.Models["glm-5.3"] = manual
	gx := first.Models["fireworks/kimi-k3"]
	gx.ToolProfile = "opencode"
	first.Models["fireworks/kimi-k3"] = gx

	dir := filepath.Join(t.TempDir(), "native")
	if err := modeltable.Save(dir, first); err != nil {
		t.Fatal(err)
	}
	existing, err := modeltable.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	merged, _, err := Import(fixture, existing)
	if err != nil {
		t.Fatal(err)
	}
	if err := modeltable.Save(dir, merged); err != nil {
		t.Fatal(err)
	}
	back, err := modeltable.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Models["glm-5.3"]; got.ToolProfile != "opencode" || got.Source != modeltable.SourceManual {
		t.Fatalf("the manual model = %+v, want its tool_profile kept", got)
	}
	if got := back.Models["fireworks/kimi-k3"].ToolProfile; got != "" {
		t.Fatalf("the gx model kept a hand-set tool_profile %q through a re-import", got)
	}
}

// TestImportPreservesSubagents: gx has no concept of sub-agent defaults or
// tiers, so an existing [subagents] section survives a merge untouched, the
// same way a manual provider or model does (plan 026 §3.6, decision 3).
// TestImportClearsAStaleSubagentsEffort (PR #51's CodeRabbit review): the
// [subagents] default model is a gx one, and the existing table offers an
// effort on it that the re-imported model no longer does. The import still
// succeeds: the stale effort is cleared and a note says so, and the model and
// the tiers are kept. Without the clearing, Validate refused the whole merge.
func TestImportClearsAStaleSubagentsEffort(t *testing.T) {
	existing := existingTable()
	kimi := existing.Models["fireworks/kimi-k3"]
	kimi.Efforts = append(slices.Clone(kimi.Efforts), "ultra")
	existing.Models["fireworks/kimi-k3"] = kimi
	existing.Subagents = modeltable.Subagents{Model: "fireworks/kimi-k3", Effort: "ultra",
		Tiers: map[string]string{"opus": "local/model"}}
	if err := existing.Validate(); err != nil {
		t.Fatalf("control: the existing table is invalid: %v", err)
	}
	got, report, err := Import(fixture, existing)
	if err != nil {
		t.Fatalf("Import = %v; want the stale effort cleared, not the import refused", err)
	}
	if slices.Contains(got.Models["fireworks/kimi-k3"].Efforts, "ultra") {
		t.Fatal("control: the re-imported model still offers ultra, so nothing went stale")
	}
	want := modeltable.Subagents{Model: "fireworks/kimi-k3", Tiers: map[string]string{"opus": "local/model"}}
	if !reflect.DeepEqual(got.Subagents, want) {
		t.Fatalf("Subagents = %+v, want %+v", got.Subagents, want)
	}
	if !slices.ContainsFunc(report.Models.Notes, func(n Note) bool {
		return n.ID == "fireworks/kimi-k3" && strings.Contains(n.Text, `effort "ultra"`)
	}) {
		t.Fatalf("no note says the effort was cleared: %+v", report.Models.Notes)
	}
}

func TestImportPreservesSubagents(t *testing.T) {
	existing := existingTable()
	existing.Subagents = modeltable.Subagents{
		Model:  "fireworks/kimi-k3",
		Effort: "high",
		Tiers: map[string]string{
			"opus": "local/model",
			"crew": "openrouter/minimax-m3",
		},
	}
	if err := existing.Validate(); err != nil {
		t.Fatalf("existingTable with Subagents is invalid: %v", err)
	}

	got, _, err := Import(fixture, existing)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Subagents, existing.Subagents) {
		t.Fatalf("Subagents = %+v, want it kept as %+v", got.Subagents, existing.Subagents)
	}
	// The caller's table is not written through, and its own map is not
	// aliased by the merged one.
	before := existingTable()
	before.Subagents = modeltable.Subagents{
		Model: "fireworks/kimi-k3", Effort: "high",
		Tiers: map[string]string{"opus": "local/model", "crew": "openrouter/minimax-m3"},
	}
	if !reflect.DeepEqual(existing, before) {
		t.Error("Import modified the existing table's Subagents")
	}
	got.Subagents.Tiers["opus"] = "changed"
	if existing.Subagents.Tiers["opus"] != "local/model" {
		t.Fatal("the merged table's Tiers map aliases the caller's")
	}
	got.Subagents.Tiers["opus"] = "local/model" // restore before Save below

	// It survives a Save/Load round trip too, the way TestImportKeepsAManualToolProfile
	// pins a manual tool_profile.
	dir := filepath.Join(t.TempDir(), "native")
	if err := modeltable.Save(dir, got); err != nil {
		t.Fatal(err)
	}
	back, err := modeltable.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := modeltable.Subagents{
		Model: "fireworks/kimi-k3", Effort: "high",
		Tiers: map[string]string{"opus": "local/model", "crew": "openrouter/minimax-m3"},
	}
	if !reflect.DeepEqual(back.Subagents, want) {
		t.Fatalf("after Save/Load, Subagents = %+v, want %+v", back.Subagents, want)
	}

	// No existing table at all: Import still produces the zero value, not a
	// panic on a nil Tiers map.
	fresh, _, err := Import(fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Subagents, modeltable.Subagents{}) {
		t.Fatalf("Subagents with no existing table = %+v, want the zero value", fresh.Subagents)
	}
}

func TestImportDefaultModelRule(t *testing.T) {
	const two = okProvider + `
[model."ok/a"]
model_provider = "ok"
`
	t.Run("gx default when imported", func(t *testing.T) {
		_, report, err := Import(gxHome(t, "[models]\ndefault = \"ok/m\"\n"+two, ""), nil)
		if err != nil {
			t.Fatal(err)
		}
		if report.DefaultModel != "ok/m" || report.DefaultRule != DefaultFromGX {
			t.Fatalf("default %q by %q, want ok/m by gx", report.DefaultModel, report.DefaultRule)
		}
	})
	t.Run("first alias when gx has none", func(t *testing.T) {
		_, report, err := Import(gxHome(t, two, ""), nil)
		if err != nil {
			t.Fatal(err)
		}
		if report.DefaultModel != "ok/a" || report.DefaultRule != DefaultFirst {
			t.Fatalf("default %q by %q, want ok/a by first", report.DefaultModel, report.DefaultRule)
		}
	})
	t.Run("an existing valid default beats gx's", func(t *testing.T) {
		first, _, err := Import(gxHome(t, two, ""), nil) // default ok/a
		if err != nil {
			t.Fatal(err)
		}
		_, report, err := Import(gxHome(t, "[models]\ndefault = \"ok/m\"\n"+two, ""), first)
		if err != nil {
			t.Fatal(err)
		}
		if report.DefaultModel != "ok/a" || report.DefaultRule != DefaultKept {
			t.Fatalf("default %q by %q, want ok/a kept", report.DefaultModel, report.DefaultRule)
		}
	})
}

func TestImportMissingAndEmpty(t *testing.T) {
	t.Run("neither file", func(t *testing.T) {
		if _, _, err := Import(t.TempDir(), nil); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want fs.ErrNotExist", err)
		}
	})
	t.Run("only providers.toml", func(t *testing.T) {
		got, _, err := Import(gxHome(t, "", okProvider), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got.Models["ok/m"]; !ok {
			t.Fatalf("models = %v, want ok/m", got.Aliases())
		}
	})
	t.Run("no directory", func(t *testing.T) {
		if _, _, err := Import("", nil); err == nil {
			t.Fatal(`Import("") succeeded`)
		}
	})
	t.Run("nothing passes the filters", func(t *testing.T) {
		dir := gxHome(t, "[model_providers.p]\nbase_url = \"https://p.example\"\napi_backend = \"responses\"\n[model.\"p/m\"]\nmodel_provider = \"p\"\n", "")
		got, report, err := Import(dir, nil)
		if !errors.Is(err, ErrNothingToImport) || got != nil {
			t.Fatalf("Import = %v, %v; want nil, ErrNothingToImport", got, err)
		}
		if len(report.Providers.Skipped) != 1 || len(report.Models.Skipped) != 1 {
			t.Fatalf("report = %+v, want the skip reasons kept for the caller to print", report)
		}
	})
}

// TestImportSyntaxErrorCarriesOnlyTheLine: the gx files are the other place a
// malformed key line can be quoted back.
func TestImportSyntaxErrorCarriesOnlyTheLine(t *testing.T) {
	bad := "[model_providers.p]\nbase_url = \"https://p.example\"\napi_key = \"" + canary + "\n"
	for _, file := range []string{ConfigFile, ProvidersFile} {
		t.Run(file, func(t *testing.T) {
			cfg, layer := okProvider, bad
			if file == ConfigFile {
				cfg, layer = bad, okProvider
			}
			dir := gxHome(t, cfg, layer)
			_, _, err := Import(dir, nil)
			if err == nil {
				t.Fatal("Import accepted a malformed file")
			}
			msg := err.Error()
			if !strings.Contains(msg, filepath.Join(dir, file)) || !strings.Contains(msg, "line 3") {
				t.Fatalf("err = %q, want the file and line 3", msg)
			}
			if rest := strings.ReplaceAll(msg, dir, ""); strings.Contains(rest, canary) || strings.Contains(rest, "sk") {
				t.Fatalf("err = %q echoes the source line", msg)
			}
		})
	}
}

// TestImportSavesThroughModeltable is acceptance criterion 2's file half: the
// imported table saves and loads back unchanged, providers.toml private and
// carrying the inline key, models.toml carrying no key.
func TestImportSavesThroughModeltable(t *testing.T) {
	tbl, _, err := Import(fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "native")
	if err := modeltable.Save(dir, tbl); err != nil {
		t.Fatal(err)
	}
	back, err := modeltable.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, tbl) {
		t.Fatalf("round trip =\n%+v\nwant\n%+v", back, tbl)
	}
	info, err := os.Stat(filepath.Join(dir, modeltable.ProvidersFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("providers.toml mode = %04o", info.Mode().Perm())
	}
	pb, _ := os.ReadFile(filepath.Join(dir, modeltable.ProvidersFile))
	mb, _ := os.ReadFile(filepath.Join(dir, modeltable.ModelsFile))
	if !strings.Contains(string(pb), canary) {
		t.Fatal("providers.toml lost the inline key")
	}
	if strings.Contains(string(mb), canary) {
		t.Fatal("models.toml holds a key")
	}
}

// TestCanaryNeverEscapes sweeps everything the two packages hand back that a
// caller could print — reports, tables, resolved models, warnings and every
// kind of error — with the canary flowing through each, and asserts it is in
// none of them.
func TestCanaryNeverEscapes(t *testing.T) {
	var outputs []string
	add := func(what string, vs ...any) {
		for _, v := range vs {
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
				outputs = append(outputs, what+" "+verb+": "+fmt.Sprintf(verb, v))
			}
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("%s: json: %v", what, err)
			}
			outputs = append(outputs, what+" json: "+string(b))
		}
	}
	addErr := func(what string, err error) {
		if err == nil {
			t.Fatalf("%s: want an error", what)
		}
		outputs = append(outputs, what+": "+err.Error(), what+" %+v: "+fmt.Sprintf("%+v", err))
	}

	// The import: its table (zai holds the canary inline) and its report,
	// including the note for a provider whose key is a $VAR reference and the
	// skip for an openrouter.ai URL carrying the canary as userinfo.
	dir := gxHome(t, okProvider+"[model_providers.v]\nbase_url = \"https://v.example\"\napi_key = \"${"+canary+"}\"\n"+
		"[model_providers.or]\nbase_url = \"https://"+canary+"@openrouter.ai/api/v1\"\n", "")
	varTable, varReport, err := Import(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(varReport.Providers.Notes) != 1 || len(varReport.Providers.Skipped) != 1 {
		t.Fatalf("report = %+v, want the note and the skip: the sweep would prove nothing", varReport)
	}
	add("$VAR report", varReport)
	add("$VAR table", varTable)
	tbl, report, err := Import(fixture, existingTable())
	if err != nil {
		t.Fatal(err)
	}
	add("report", report)
	add("table", tbl, *tbl, tbl.Providers, tbl.Models)

	// Resolving: zai's models carry the canary as their key; the rest fail
	// with ErrNoAPIKey while an unrelated variable holds the canary.
	getenv := func(name string) string {
		if name == "UNRELATED" {
			return canary
		}
		return ""
	}
	resolvedCanary := false
	for _, alias := range tbl.Aliases() {
		r, err := tbl.Resolve(alias, getenv)
		if err != nil {
			addErr("resolve "+alias, err)
			continue
		}
		resolvedCanary = resolvedCanary || r.APIKey.Reveal() == canary
		add("resolved "+alias, r, &r)
	}
	if !resolvedCanary {
		t.Fatal("no resolved model carried the canary: the sweep would prove nothing")
	}

	// Saving: providers.toml must hold the key; models.toml must not.
	native := filepath.Join(t.TempDir(), "native")
	if err := modeltable.Save(native, tbl); err != nil {
		t.Fatal(err)
	}
	pb, _ := os.ReadFile(filepath.Join(native, modeltable.ProvidersFile))
	if !strings.Contains(string(pb), canary) {
		t.Fatal("providers.toml does not hold the canary: the sweep would prove nothing")
	}
	mb, _ := os.ReadFile(filepath.Join(native, modeltable.ModelsFile))
	outputs = append(outputs, "models.toml: "+string(mb))

	// Loading: a loose providers.toml holding the canary warns by path only.
	if err := os.Chmod(filepath.Join(native, modeltable.ProvidersFile), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := modeltable.Load(native)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Warnings) == 0 {
		t.Fatal("no tightening warning: the sweep would prove nothing")
	}
	add("load warnings", loaded.Warnings)
	add("loaded table", loaded)

	// Errors from files that hold the canary: malformed lines in both
	// packages' inputs, a strict-decode error and a validation error.
	keyLine := "api_key = \"" + canary + "\"\n"
	for name, body := range map[string]string{
		"load: unterminated key":    "version = 1\n[providers.p]\ndriver = \"openai-compat\"\napi_key = \"" + canary + "\n",
		"load: unquoted key":        "version = 1\n[providers.p]\ndriver = \"openai-compat\"\napi_key = " + canary + "\n",
		"load: unknown key":         "version = 1\n[providers.p]\ndriver = \"openai-compat\"\nbase_url = \"https://p.example\"\n" + keyLine + "bse_url = \"x\"\n",
		"load: invalid driver":      "version = 1\n[providers.p]\ndriver = \"bogus\"\n" + keyLine,
		"load: base_url with a key": "version = 1\n[providers.p]\ndriver = \"openai-compat\"\nbase_url = \"p.example/?key=" + canary + "\"\n",
	} {
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, modeltable.ProvidersFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, modeltable.ModelsFile), []byte("version = 1\ndefault_model = \"m\"\n[models.m]\nprovider = \"p\"\nwire_model = \"w\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := modeltable.Load(d)
		addErr(name, err)
	}
	for _, file := range []string{ConfigFile, ProvidersFile} {
		bad := "[model_providers.p]\nbase_url = \"https://p.example\"\napi_key = " + canary + "\n"
		cfg, layer := okProvider, bad
		if file == ConfigFile {
			cfg, layer = bad, okProvider
		}
		_, _, err := Import(gxHome(t, cfg, layer), nil)
		addErr("import: malformed "+file, err)
	}

	for _, out := range outputs {
		if strings.Contains(out, canary) {
			t.Errorf("the canary escaped in %s", out)
		}
	}
}

// gxHome writes a gx home with the given config.toml and providers.toml
// bodies; "" leaves a file out.
func gxHome(t *testing.T, config, providers string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{ConfigFile: config, ProvidersFile: providers} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
