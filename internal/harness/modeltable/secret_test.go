package modeltable

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// secretCarriers are the values a caller could plausibly print or encode
// while a key is inside them: the key itself, each struct that holds one,
// and pointers to them.
func secretCarriers(t *testing.T) map[string]any {
	t.Helper()
	tbl := validTable() // openrouter carries the canary inline
	resolved, err := tbl.Resolve("openrouter/minimax-m3", fakeEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if resolved.APIKey.Reveal() != canary {
		t.Fatalf("resolved key = %q, want the canary (the test would prove nothing)", resolved.APIKey.Reveal())
	}
	return map[string]any{
		"Secret":    Secret(canary),
		"*Secret":   ptr(Secret(canary)),
		"Provider":  tbl.Providers["openrouter"],
		"*Provider": ptr(tbl.Providers["openrouter"]),
		"Table":     *tbl,
		"*Table":    tbl,
		"Resolved":  resolved,
		"*Resolved": &resolved,
		"[]Secret":  []Secret{canary},
		"map":       map[string]Secret{"k": canary},
	}
}

func ptr[T any](v T) *T { return &v }

// TestSecretRedactsEverywhere sweeps fmt's verbs, including ones a string
// cannot take (%d), because for those fmt would otherwise print the raw
// value of a Secret inside a struct; and encoding/json and the TOML encoder.
func TestSecretRedactsEverywhere(t *testing.T) {
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10.3v", "%T"}
	for name, v := range secretCarriers(t) {
		for _, verb := range verbs {
			if out := fmt.Sprintf(verb, v); strings.Contains(out, canary) {
				t.Errorf("%s %s = %q leaks the key", name, verb, out)
			}
		}
		// fmt.Print and friends take the %v path through a different entry.
		if out := fmt.Sprint(v); strings.Contains(out, canary) {
			t.Errorf("%s Sprint = %q leaks the key", name, out)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s json: %v", name, err)
		}
		if strings.Contains(string(b), canary) {
			t.Errorf("%s json = %s leaks the key", name, b)
		}
	}

	// The struct renderings carry the marker, so the sweep above is not
	// passing because the field was dropped.
	if out := fmt.Sprintf("%+v", validTable().Providers["openrouter"]); !strings.Contains(out, "APIKey:"+redacted) {
		t.Errorf("%%+v Provider = %q, want APIKey:%s", out, redacted)
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(validTable().Providers["openrouter"]); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), canary) {
		t.Errorf("TOML encoding of a Provider leaks the key:\n%s", buf.String())
	}
}

func TestSecretRendering(t *testing.T) {
	s := Secret(canary)
	if s.Reveal() != canary {
		t.Fatalf("Reveal = %q", s.Reveal())
	}
	cases := map[string]string{
		fmt.Sprintf("%v", s):  redacted,
		fmt.Sprintf("%s", s):  redacted,
		fmt.Sprintf("%q", s):  `"` + redacted + `"`,
		fmt.Sprintf("%#v", s): `"` + redacted + `"`,
		s.String():            redacted,
		s.GoString():          `"` + redacted + `"`,
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("rendered %q, want %q", got, want)
		}
	}
	if b, _ := s.MarshalText(); string(b) != redacted {
		t.Errorf("MarshalText = %q", b)
	}
	if b, _ := json.Marshal(s); string(b) != `"`+redacted+`"` {
		t.Errorf("json = %s", b)
	}

	// An empty key shows as empty: a printed Table tells a provider with an
	// inline key from one without, and still shows no key.
	var empty Secret
	if fmt.Sprintf("%v|%q", empty, empty) != `|""` {
		t.Errorf("empty Secret renders as %q", fmt.Sprintf("%v|%q", empty, empty))
	}
}
