package modeltable

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The openai-responses driver (plan 047 D13, P25): the Responses API with a
// static key, at a base_url craze may send that key to.

// responsesTable is a whole table (catalog = false) with one provider, acme,
// on driver at base, and one model on it.
func responsesTable(driver, base string) (providers, models string) {
	providers = "version = 1\n\n[providers.acme]\ndriver = \"" + driver + "\"\n"
	if base != "" {
		providers += "base_url = \"" + base + "\"\n"
	}
	providers += "env_keys = [\"ACME_API_KEY\"]\n"
	models = "version = 1\ncatalog = false\ndefault_model = \"acme/m\"\n\n[models.\"acme/m\"]\nprovider = \"acme\"\nwire_model = \"m-1\"\n"
	return providers, models
}

// TestOpenAIResponsesEndpointRule: the driver's base_url is https on any
// host, or http only to a loopback IP literal, with no user info, query or
// fragment; anything else fails the load naming providers.toml, the
// provider's table and base_url, never echoing the value. The control: the
// same http URLs load for openai-compat, whose rule this is not.
func TestOpenAIResponsesEndpointRule(t *testing.T) {
	for _, tc := range []struct {
		base string
		ok   bool
	}{
		{"https://api.meta.ai/v1", true},
		{"https://localhost:8443/v1", true},
		{"https://10.0.0.7/v1", true},
		{"http://127.0.0.1:8080/v1", true},
		{"http://127.9.8.7/v1", true},
		{"http://[::1]:8080/v1", true},
		{"", false},
		{"http://localhost:8080/v1", false},
		{"http://10.0.0.7/v1", false},
		{"http://api.meta.ai/v1", false},
		{"http://[::2]/v1", false},
		{"https://user:zq-secret-pw@api.example/v1", false},
		{"https://api.example/v1?key=zq-secret-pw", false},
		{"https://api.example/v1?", false},
		{"https://api.example/v1#zq-secret-pw", false},
	} {
		t.Run(tc.base, func(t *testing.T) {
			p, m := responsesTable(DriverOpenAIResponses, tc.base)
			dir := writeFiles(t, p, m)
			tbl, err := Load(dir)
			if tc.ok {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if p := tbl.Providers["acme"]; p.Driver != DriverOpenAIResponses || p.BaseURL != tc.base {
					t.Fatalf("provider = %+v", p)
				}
				return
			}
			fe := wantFileError(t, err, filepath.Join(dir, ProvidersFile), "providers.acme", "base_url")
			// The reason alone: the file's path is the test's temporary
			// directory, which is named for the subtest.
			if strings.Contains(fe.Reason, "zq-secret-pw") {
				t.Fatalf("the refusal echoes the URL: %v", fe.Reason)
			}
		})
	}
	for _, base := range []string{"http://localhost:8080/v1", "http://10.0.0.7/v1"} {
		p, m := responsesTable(DriverOpenAICompat, base)
		if _, err := Load(writeFiles(t, p, m)); err != nil {
			t.Errorf("openai-compat at %s: %v; the stricter rule is openai-responses' alone", base, err)
		}
	}
}

// TestOpenAIResponsesIsKeyFunded: the driver is funded as openai-compat is —
// the first usable env_keys variable, else the inline api_key — by any
// provider id (no provider owns it), and with neither the model is not
// funded. Its name is in the unknown-driver message.
func TestOpenAIResponsesIsKeyFunded(t *testing.T) {
	p, m := responsesTable(DriverOpenAIResponses, "https://api.acme.example/v1")
	tbl, err := Load(writeFiles(t, p, m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r, err := tbl.Resolve("acme/m", func(name string) string {
		if name == "ACME_API_KEY" {
			return canary
		}
		return ""
	})
	if err != nil || r.Driver != DriverOpenAIResponses || r.BaseURL != "https://api.acme.example/v1" || r.APIKey.Reveal() != canary || r.Auth != "" {
		t.Fatalf("Resolve = %+v, %v; want the variable's key on the driver at its base", r, err)
	}
	if _, err := tbl.Resolve("acme/m", func(string) string { return "" }); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("with no key: err = %v, want ErrNoAPIKey", err)
	}

	inline := strings.Replace(p, "env_keys = [\"ACME_API_KEY\"]\n", "api_key = \""+canary+"\"\n", 1)
	tbl, err = Load(writeFiles(t, inline, m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r, err := tbl.Resolve("acme/m", func(string) string { return "" }); err != nil || r.APIKey.Reveal() != canary {
		t.Fatalf("an inline key: Resolve = %+v, %v", r, err)
	}

	if owner := driverOwner(DriverOpenAIResponses); owner != "" {
		t.Fatalf("driver %q is owned by %q; no provider owns it", DriverOpenAIResponses, owner)
	}
	if r := driverProblem("bedrock"); !strings.Contains(r, `"openai-responses"`) {
		t.Fatalf("the unknown-driver reason %q does not name openai-responses", r)
	}
	if r := driverProblem(DriverOpenAIResponses); r != "" {
		t.Fatalf("driverProblem(openai-responses) = %q", r)
	}
}

// TestMetaDriverOverrides: Meta's shipped provider is on the openai-responses
// driver (plan 047 D14), and these are the providers.toml entries that move it
// or would. D15's way back — driver = "openai-compat", the shipped base_url
// and env_keys written together — loads with no warning and puts Meta on Chat
// Completions at its own endpoint and variable; without env_keys or an
// api_key it loads unfunded, with a warning saying what to add. An entry the
// former gx importer wrote (source = "gx") carrying the old driver yields to
// the catalog (legacySource): its key funds Meta, on the shipped driver. The
// override plan 047's live checks ran on C6 now repeats the shipped entry: a
// redundant note, not a warning. A non-loopback http base_url written with the
// driver fails the load (a pair that cannot go together); written alone, under
// the shipped driver, it is dropped with a warning and the shipped endpoint
// kept. And D15's driver written alone is dropped with a warning: a driver
// change drops the shipped base_url (overlayProvider), which openai-compat
// needs, so the shipped driver stays.
func TestMetaDriverOverrides(t *testing.T) {
	const back = "version = 1\n\n[providers.meta]\ndriver = \"openai-compat\"\nbase_url = \"https://api.meta.ai/v1\"\nenv_keys = [\"META_API_KEY\"]\n"
	shipped := Provider{Name: "Meta", Driver: DriverOpenAIResponses, BaseURL: "https://api.meta.ai/v1", EnvKeys: []string{"META_API_KEY"}}
	metaIs := func(t *testing.T, tbl *Table, want Provider, origin Origin) {
		t.Helper()
		if p := tbl.Providers["meta"]; !reflect.DeepEqual(p, want) || tbl.ProviderOrigin("meta") != origin {
			t.Fatalf("meta = %+v (%s), want %+v (%s)", p, tbl.ProviderOrigin("meta"), want, origin)
		}
	}

	tbl, err := Load(writeFiles(t, back, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rolled := shipped
	rolled.Driver, rolled.Source = DriverOpenAICompat, SourceManual
	metaIs(t, tbl, rolled, OriginOverridden)
	if len(tbl.Warnings) != 0 || len(tbl.RedundantOverrides()) != 0 {
		t.Fatalf("D15's way back: warnings = %q, redundant = %q", tbl.Warnings, tbl.RedundantOverrides())
	}

	// The upgrade the switch breaks (C7 review): a driver = "openai-compat"
	// of the user's own with the base_url and no env_keys inherited
	// META_API_KEY while the shipped driver was the same; now it inherits
	// nothing, so it loads on Chat Completions, unfunded, with one warning
	// naming the provider and the env_keys to add. An inline api_key funds
	// it, and an explicit env_keys is the user's choice: neither warns. Nor
	// does the way back marked source = "manual", which the docs offer to an
	// old import's entry in place of its source = "gx".
	noVars := strings.Replace(back, "env_keys = [\"META_API_KEY\"]\n", "", 1)
	tbl, err = Load(writeFiles(t, noVars, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	unfunded := rolled
	unfunded.EnvKeys = nil
	metaIs(t, tbl, unfunded, OriginOverridden)
	if len(tbl.Warnings) != 1 || !strings.Contains(tbl.Warnings[0], "[providers.meta] env_keys") ||
		!strings.Contains(tbl.Warnings[0], `add env_keys = ["META_API_KEY"]`) || !strings.Contains(tbl.Warnings[0], `"craze auth login meta"`) {
		t.Fatalf("an unfunded driver = \"openai-compat\": warnings = %q, want one naming providers.meta and env_keys = [\"META_API_KEY\"]", tbl.Warnings)
	}
	if _, err := tbl.Resolve("muse-spark-1.3-contributor", func(n string) string {
		if n == "META_API_KEY" {
			return canary
		}
		return ""
	}); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("an exported META_API_KEY funds the entry: err = %v, want ErrNoAPIKey", err)
	}
	manual := strings.Replace(back, "[providers.meta]\n", "[providers.meta]\nsource = \"manual\"\n", 1)
	for _, funded := range []string{noVars + "api_key = \"" + canary + "\"\n", noVars + "env_keys = []\n", manual} {
		tbl, err = Load(writeFiles(t, funded, ""))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(tbl.Warnings) != 0 || tbl.Providers["meta"].Driver != DriverOpenAICompat {
			t.Fatalf("meta = %+v, warnings = %q; want Chat Completions with no warning", tbl.Providers["meta"], tbl.Warnings)
		}
	}

	legacy := strings.Replace(back, "[providers.meta]\n", "[providers.meta]\nsource = \"gx\"\napi_key = \""+canary+"\"\n", 1)
	tbl, err = Load(writeFiles(t, legacy, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	lent := shipped
	lent.APIKey = Secret(canary)
	metaIs(t, tbl, lent, OriginShipped)
	if len(tbl.Warnings) != 0 || len(tbl.RedundantOverrides()) != 0 {
		t.Fatalf("an old import: warnings = %q, redundant = %q", tbl.Warnings, tbl.RedundantOverrides())
	}
	if r, err := tbl.Resolve("muse-spark-1.3-contributor", func(string) string { return "" }); err != nil || r.Driver != DriverOpenAIResponses || r.APIKey.Reveal() != canary {
		t.Fatalf("an old import's key on the shipped driver: Resolve = %+v, %v", r, err)
	}

	tbl, err = Load(writeFiles(t, strings.Replace(back, DriverOpenAICompat, DriverOpenAIResponses, 1), ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	same := shipped
	same.Source = SourceManual
	metaIs(t, tbl, same, OriginOverridden)
	if len(tbl.Warnings) != 0 || len(tbl.RedundantOverrides()) != 1 || !strings.Contains(tbl.RedundantOverrides()[0], "providers.meta") {
		t.Fatalf("the C6 override: warnings = %q, redundant = %q, want one note on providers.meta", tbl.Warnings, tbl.RedundantOverrides())
	}

	dir := writeFiles(t, "version = 1\n\n[providers.meta]\ndriver = \"openai-responses\"\nbase_url = \"http://203.0.113.9/v1\"\n", "")
	_, err = Load(dir)
	wantFileError(t, err, filepath.Join(dir, ProvidersFile), "providers.meta", "base_url")

	tbl, err = Load(writeFiles(t, "version = 1\n\n[providers.meta]\nbase_url = \"http://203.0.113.9/v1\"\n", ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p := tbl.Providers["meta"]; p.Driver != DriverOpenAIResponses || p.BaseURL != shipped.BaseURL || !slices.Equal(p.EnvKeys, shipped.EnvKeys) {
		t.Fatalf("meta = %+v, want the shipped endpoint kept", p)
	}
	if len(tbl.Warnings) != 1 || !strings.Contains(tbl.Warnings[0], "base_url") {
		t.Fatalf("warnings = %q, want one naming base_url", tbl.Warnings)
	}

	tbl, err = Load(writeFiles(t, "version = 1\n\n[providers.meta]\ndriver = \"openai-compat\"\n", ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p := tbl.Providers["meta"]; p.Driver != DriverOpenAIResponses || p.BaseURL != shipped.BaseURL {
		t.Fatalf("meta = %+v, want the shipped driver kept", p)
	}
	if len(tbl.Warnings) != 1 || !strings.Contains(tbl.Warnings[0], "base_url") {
		t.Fatalf("warnings = %q, want one naming base_url", tbl.Warnings)
	}
}
