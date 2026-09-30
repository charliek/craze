package modeltable

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// StoredKeys and KeyProblem (plan 031 §3.8): what a running session reads of
// its providers.toml to learn the keys stored since it opened. Every key is a
// dummy of at least MinKeyLen bytes that shares no text with the marker.

// TestStoredKeysReadsTheInlineKeysAlone: every provider that writes a
// non-blank api_key, in id order, its value as written — untrimmed and
// unjudged, a short one included, since judging is the learner's and one bad
// value must not hide the others — from the file alone: no models.toml, no
// catalog, and an entry the merge would drop (an unknown driver's) still
// counts. A provider with no api_key, or a blank one, gives none. The file's
// mode is not touched: tightening is Load's.
func TestStoredKeysReadsTheInlineKeysAlone(t *testing.T) {
	dir := t.TempDir()
	body := `version = 1

[providers.zeta]
api_key = "  sk-zeta-stored-0001 "

[providers.alpha]
driver = "openai-compat"
base_url = "http://127.0.0.1:9/v1"
api_key = "sk-alpha-stored-0002"

[providers.short]
api_key = "zq9-w7"

[providers.blank]
api_key = "   "

[providers.none]
env_keys = ["NONE_API_KEY"]
`
	path := filepath.Join(dir, ProvidersFile)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := StoredKeys(dir)
	if err != nil {
		t.Fatalf("StoredKeys: %v", err)
	}
	want := []StoredKey{
		{Provider: "alpha", Key: "sk-alpha-stored-0002"},
		{Provider: "short", Key: "zq9-w7"},
		{Provider: "zeta", Key: "  sk-zeta-stored-0001 "},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StoredKeys = %v; want alpha, short, zeta with their values as written", got)
	}
	if m := mode(t, path); m != 0o644 {
		t.Fatalf("StoredKeys changed the file's mode to %04o", m)
	}
	if _, err := os.Stat(filepath.Join(dir, ModelsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("control: a models.toml exists, so reading without one proves nothing")
	}
}

// TestStoredKeysMissingFileIsNoKeys: no providers.toml is nothing stored, not
// an error; no directory at all is one, as it is for Load.
func TestStoredKeysMissingFileIsNoKeys(t *testing.T) {
	if got, err := StoredKeys(t.TempDir()); got != nil || err != nil {
		t.Fatalf("StoredKeys of an empty directory = %v, %v; want nothing", got, err)
	}
	if _, err := StoredKeys(""); err == nil {
		t.Fatal("StoredKeys(\"\") read something: it would have read the working directory")
	}
}

// TestStoredKeysStructuralErrorsNameNoValue: the file is decoded exactly as
// Load decodes it, so TOML syntax, a key the schema lacks and a wrong version
// each fail the reading — and none of those errors quotes a value, a key on
// the broken line included.
func TestStoredKeysStructuralErrorsNameNoValue(t *testing.T) {
	const key = "sk-on-a-broken-line-0003"
	for name, body := range map[string]string{
		"syntax":      "version = 1\n\n[providers.alpha]\napi_key = \"" + key + "\n",
		"unknown key": "version = 1\n\n[providers.alpha]\napi_key = \"" + key + "\"\napikey = \"" + key + "\"\n",
		"version":     "version = 2\n\n[providers.alpha]\napi_key = \"" + key + "\"\n",
		"no version":  "[providers.alpha]\napi_key = \"" + key + "\"\n",
		"wrong type":  "version = 1\n\n[providers.alpha]\napi_key = [\"" + key + "\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeFiles(t, body, "")
			got, err := StoredKeys(dir)
			if err == nil || got != nil {
				t.Fatalf("StoredKeys = %v, %v; want the file refused and nothing read", got, err)
			}
			if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), key[3:12]) {
				t.Fatalf("the error quotes the key: %v", err)
			}
			if !strings.Contains(err.Error(), ProvidersFile) {
				t.Fatalf("the error does not name the file: %v", err)
			}
		})
	}
}

// TestKeyProblemIsTheLoadRule: KeyProblem trims, calls blank no key, and
// refuses what Load refuses an inline key for and Keys skips an env value
// for, naming the rule and never the value.
func TestKeyProblemIsTheLoadRule(t *testing.T) {
	for _, c := range []struct {
		key  string
		want error
	}{
		{"", nil},
		{" \t\n", nil},
		{"zq9-w7", ErrKeyTooShort},
		{"  zq9-w7  ", ErrKeyTooShort},
		{"credential", ErrKeyOverlapsMarker},
		{"sk-usable-0004", nil},
		{"\tsk-usable-0004\n", nil},
	} {
		err := KeyProblem(c.key)
		if !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
			t.Errorf("KeyProblem(%q) = %v; want %v", c.key, err, c.want)
		}
		if err != nil && strings.TrimSpace(c.key) != "" && strings.Contains(err.Error(), strings.TrimSpace(c.key)) {
			t.Errorf("KeyProblem's error quotes the value: %v", err)
		}
	}
}
