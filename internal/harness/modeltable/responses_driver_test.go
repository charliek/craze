package modeltable

import (
	"errors"
	"path/filepath"
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

// TestOpenAIResponsesOverShippedMeta: the override plan 047 §8 runs and D15
// describes — Meta's shipped provider on the driver, its base_url and
// env_keys written beside it — loads with no warning. A non-loopback http
// base_url written with the driver fails the load (a pair that cannot go
// together), and the driver written alone is dropped with a warning, the
// shipped driver kept: its base_url is the shipped endpoint's, which a
// driver change drops (overlayProvider), so it has none.
func TestOpenAIResponsesOverShippedMeta(t *testing.T) {
	tbl, err := Load(writeFiles(t, "version = 1\n\n[providers.meta]\ndriver = \"openai-responses\"\nbase_url = \"https://api.meta.ai/v1\"\nenv_keys = [\"META_API_KEY\"]\n", ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p := tbl.Providers["meta"]; p.Driver != DriverOpenAIResponses || p.BaseURL != "https://api.meta.ai/v1" || len(p.EnvKeys) != 1 || p.EnvKeys[0] != "META_API_KEY" {
		t.Fatalf("meta = %+v", p)
	}
	if len(tbl.Warnings) != 0 {
		t.Fatalf("warnings = %q", tbl.Warnings)
	}

	dir := writeFiles(t, "version = 1\n\n[providers.meta]\ndriver = \"openai-responses\"\nbase_url = \"http://203.0.113.9/v1\"\n", "")
	_, err = Load(dir)
	wantFileError(t, err, filepath.Join(dir, ProvidersFile), "providers.meta", "base_url")

	tbl, err = Load(writeFiles(t, "version = 1\n\n[providers.meta]\ndriver = \"openai-responses\"\n", ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p := tbl.Providers["meta"]; p.Driver != DriverOpenAICompat {
		t.Fatalf("meta = %+v, want the shipped driver kept", p)
	}
	if len(tbl.Warnings) != 1 || !strings.Contains(tbl.Warnings[0], "base_url") {
		t.Fatalf("warnings = %q, want one naming base_url", tbl.Warnings)
	}
}
