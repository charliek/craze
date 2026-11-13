package modeltable

import (
	"slices"
	"testing"
)

// TestExaKeyIsACredential (plan 051 §B): the web search's key variable,
// EXA_API_KEY, is a credential of every table — merged over a catalog, loaded
// alone with `catalog = false`, built in memory — listed by
// CredentialEnvNames, so commands are started without it, and read by Keys,
// so the redactor holds its value; it funds nothing. A value that cannot be
// a key is skipped, as any variable's is; a provider that declares the same
// variable has its value once; and once WithholdFrozen marks the value
// frozen, Keys no longer returns it while CredentialEnvNames still names it.
func TestExaKeyIsACredential(t *testing.T) {
	const exaKey = "3f1c2a9e-8b7d-4c6e-9f0a-1b2c3d4e5f60"
	cat := testCatalog(t, testCatalogV1)
	merged, _ := loadOver(t, cat, "", "")
	alone, _ := loadOver(t, cat, `version = 1

[providers.acme]
driver = "openai-compat"
base_url = "https://proxy.example/v1"
api_key = "stored-proxy-key-01"
`, `version = 1
catalog = false
default_model = "acme/x"

[models."acme/x"]
provider = "acme"
wire_model = "x"
`)
	declares := validTable()
	setProvider(declares, "fireworks", func(p *Provider) { p.EnvKeys = append(p.EnvKeys, ExaKeyEnv) })
	for name, tbl := range map[string]*Table{
		"merged":                     merged,
		"catalog = false":            alone,
		"in memory":                  validTable(),
		"a provider declares it too": declares,
	} {
		t.Run(name, func(t *testing.T) {
			if !slices.Contains(tbl.CredentialEnvNames(), ExaKeyEnv) {
				t.Fatalf("CredentialEnvNames = %q, want %s among them", tbl.CredentialEnvNames(), ExaKeyEnv)
			}
			env := fakeEnv(map[string]string{ExaKeyEnv: "  " + exaKey + "\n"})
			keys, err := tbl.Keys(env)
			if err != nil {
				t.Fatalf("Keys: %v", err)
			}
			if n := countKey(keys, exaKey); n != 1 {
				t.Fatalf("Keys holds the Exa key %d times, want once, trimmed", n)
			}
			for _, unusable := range []string{"short", "redacted-credential"} {
				keys, err := tbl.Keys(fakeEnv(map[string]string{ExaKeyEnv: unusable}))
				if err != nil || countKey(keys, unusable) != 0 {
					t.Fatalf("Keys with an unusable %s = %d of it, %v", ExaKeyEnv, countKey(keys, unusable), err)
				}
			}
			if name != "a provider declares it too" {
				if with, without := len(tbl.Choices(nil, env, "")), len(tbl.Choices(nil, fakeEnv(nil), "")); with != without {
					t.Fatalf("%s set offers %d models, unset %d: it funds something", ExaKeyEnv, with, without)
				}
			}

			tbl.WithholdFrozen(env, func(v string) bool { return v == exaKey }, "")
			keys, err = tbl.Keys(env)
			if err != nil || countKey(keys, exaKey) != 0 {
				t.Fatalf("after WithholdFrozen, Keys holds the frozen Exa key %d times (%v)", countKey(keys, exaKey), err)
			}
			names := tbl.CredentialEnvNames()
			if !slices.Contains(names, ExaKeyEnv) || len(slices.Compact(slices.Clone(names))) != len(names) {
				t.Fatalf("after WithholdFrozen, CredentialEnvNames = %q, want %s once", names, ExaKeyEnv)
			}
		})
	}
}

func countKey(keys []Secret, v string) int {
	n := 0
	for _, k := range keys {
		if k.Reveal() == v {
			n++
		}
	}
	return n
}
