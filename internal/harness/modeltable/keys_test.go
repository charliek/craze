package modeltable

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
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

// The key store (plan 031 §3.7): SetKey, RemoveKey and Providers. The cases
// run over testCatalogV1 (merge_test.go: providers acme and router) through
// the unexported *With forms, so they pin the rules and not the shipped
// contents; TestSetKeyOverTheShippedCatalog runs the exported ones. Every key
// is a dummy of at least MinKeyLen bytes sharing no text with the marker,
// except the deliberately broken ones, which are never printed either.

// keyFileBody is a providers.toml a hand, an old import and craze each wrote
// part of: a comment, a gx entry with an extra variable, a hand entry with an
// explicit empty name and env_keys and a key the rules refuse (with the
// whitespace it was written with), and an entry with no key at all.
const keyFileBody = `version = 1

# a comment craze does not keep
[providers.acme]
source = "gx"
env_keys = ["ACME_API_KEY", "ACME_OLD_KEY"]
api_key = "sk-acme-stored-0001"

[providers.local]
name = ""
driver = "openai-compat"
base_url = "http://127.0.0.1:9/v1"
env_keys = []
api_key = "  zq9-w7 "
source = "manual"

[providers.router]
name = "My Router"
`

// readDoc is providers.toml in dir decoded as the store reads it.
func readDoc(t *testing.T, dir string) providersOverlay {
	t.Helper()
	doc, err := readKeyFile(filepath.Join(dir, ProvidersFile))
	if err != nil {
		t.Fatalf("reading providers.toml back: %v", err)
	}
	return doc
}

// noKeyIn fails when any of texts holds any of keys, or a fragment of one
// long enough to be a real leak.
func noKeyIn(t *testing.T, keys []string, texts ...string) {
	t.Helper()
	for _, k := range keys {
		frags := []string{k}
		if len(k) > 12 {
			frags = append(frags, k[3:12])
		}
		for _, text := range texts {
			for _, f := range frags {
				if strings.Contains(text, f) {
					t.Fatalf("a key leaked into %q", text)
				}
			}
		}
	}
}

// TestSetKeyRoundTripsEveryEntry: storing one provider's key rewrites the
// file whole and keeps every other key every entry wrote — source, an
// explicit empty name and env_keys = [], and another provider's invalid key
// with its whitespace, byte for byte (r2-7) — at 0600 under the new header,
// with the comment gone and models.toml never written. The key itself is
// stored trimmed.
func TestSetKeyRoundTripsEveryEntry(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	dir := writeFiles(t, keyFileBody, "")
	path := filepath.Join(dir, ProvidersFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	before := readDoc(t, dir)

	if err := setKeyWith(dir, "router", "  sk-router-stored-0002\n", cat); err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	want := before
	want.Providers = maps.Clone(before.Providers)
	r := want.Providers["router"]
	r.APIKey = ptr("sk-router-stored-0002")
	want.Providers["router"] = r
	if got := readDoc(t, dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("the file read back differs from what it held plus the one key:\n got %+v\nwant %+v", got, want)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, line := range []string{
		"# craze native harness: API keys and provider overrides.\n# craze rewrites this file for \"craze auth\": comments are not kept. Keep it 0600; never paste it.\n",
		"env_keys = []\n",
		"name = \"\"\n",
		"api_key = \"  zq9-w7 \"\n",
		"source = \"gx\"\n",
	} {
		if !strings.Contains(body, line) {
			t.Errorf("the rewritten file lacks %q", line)
		}
	}
	if strings.Contains(body, "a comment craze does not keep") {
		t.Error("the old comment survived: the header would be wrong about comments")
	}
	if m := mode(t, path); m != providersPerm {
		t.Fatalf("providers.toml is %04o after SetKey, want 0600", m)
	}
	if _, err := os.Stat(filepath.Join(dir, ModelsFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("SetKey wrote models.toml: %v", err)
	}

	// Setting it again changes nothing else: the rewrite is stable.
	if err := setKeyWith(dir, "router", "sk-router-stored-0002", cat); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); string(again) != body {
		t.Fatalf("a second identical SetKey changed the file:\n%s\nthen\n%s", body, again)
	}
}

// TestSetKeyKeepsAnExplicitEmptyEnvKeys (review r1): `env_keys = []` over a
// shipped provider means "no variables", and the rewrite must keep it so —
// dropped to absent it would inherit the shipped ACME_API_KEY again, and an
// exported one would win over the key just stored.
func TestSetKeyKeepsAnExplicitEmptyEnvKeys(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	dir := writeFiles(t, "version = 1\n\n[providers.acme]\nenv_keys = []\n", "")
	if err := setKeyWith(dir, "acme", "sk-acme-stored-0003", cat); err != nil {
		t.Fatal(err)
	}
	if o := readDoc(t, dir).Providers["acme"]; o.EnvKeys == nil || len(*o.EnvKeys) != 0 {
		t.Fatalf("env_keys after SetKey = %v; want an explicit empty list", o.EnvKeys)
	}
	tbl, err := LoadWith(dir, cat)
	if err != nil {
		t.Fatal(err)
	}
	env := fakeEnv(map[string]string{"ACME_API_KEY": "sk-acme-exported-0004"})
	got, err := tbl.Resolve("acme/fast", env)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey.Reveal() != "sk-acme-stored-0003" {
		t.Fatal("the shipped variable came back: the exported key won over the stored one")
	}
}

// TestSetKeyRefusesWhatCannotBeAKey: an empty or blank key and one KeyProblem
// refuses are refused before anything is touched — not even the directory is
// made — with an error that names the rule and never the value.
func TestSetKeyRefusesWhatCannotBeAKey(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	for _, c := range []struct {
		key  string
		want error
	}{
		{"", ErrEmptyKey},
		{" \t\n", ErrEmptyKey},
		{"zq9", ErrKeyTooShort},
		{"  zq9-w7 \n", ErrKeyTooShort},
		{"credential", ErrKeyOverlapsMarker},
	} {
		dir := filepath.Join(t.TempDir(), "native")
		err := setKeyWith(dir, "acme", c.key, cat)
		if !errors.Is(err, c.want) {
			t.Fatalf("SetKey(%d bytes) = %v; want %v", len(c.key), err, c.want)
		}
		if k := strings.TrimSpace(c.key); k != "" && strings.Contains(err.Error(), k) {
			t.Fatalf("the refusal quotes the key: %v", err)
		}
		if !strings.Contains(err.Error(), `"acme"`) || !strings.Contains(err.Error(), "not saved") {
			t.Fatalf("the refusal does not say which key was not saved: %v", err)
		}
		if _, serr := os.Stat(dir); !errors.Is(serr, fs.ErrNotExist) {
			t.Fatalf("a refused key made %s", dir)
		}
	}
}

// TestSetKeyNeedsAKnownProvider: a key goes to a provider the catalog ships
// or one the file already has, never to a new id.
func TestSetKeyNeedsAKnownProvider(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	dir := writeFiles(t, keyFileBody, "")
	before, _ := os.ReadFile(filepath.Join(dir, ProvidersFile))
	err := setKeyWith(dir, "nosuch", "sk-nosuch-0005", cat)
	if !errors.Is(err, ErrUnknownProvider) || !strings.Contains(err.Error(), `"nosuch"`) {
		t.Fatalf("SetKey of an unknown provider = %v; want ErrUnknownProvider naming it", err)
	}
	noKeyIn(t, []string{"sk-nosuch-0005"}, err.Error())
	if after, _ := os.ReadFile(filepath.Join(dir, ProvidersFile)); string(after) != string(before) {
		t.Fatal("a refused SetKey changed the file")
	}
	// A provider of the user's own, and a shipped one with no entry yet.
	for _, id := range []string{"local", "acme", "router"} {
		if err := setKeyWith(dir, id, "sk-"+id+"-stored-0006", cat); err != nil {
			t.Fatalf("SetKey %s: %v", id, err)
		}
	}
	if err := setKeyWith(t.TempDir(), "router", "sk-router-stored-0007", cat); err != nil {
		t.Fatalf("SetKey into a directory with no providers.toml: %v", err)
	}
}

// TestSetKeyRefusesASymlink (panel GLM): a providers.toml that is a symlink
// is refused, not replaced by a regular file: its target is edited by hand.
// The link and the target are left as they were.
func TestSetKeyRefusesASymlink(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.toml")
	if err := os.WriteFile(target, []byte(keyFileBody), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ProvidersFile)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for name, op := range map[string]func() error{
		"SetKey":    func() error { return setKeyWith(dir, "acme", "sk-acme-new-0008", cat) },
		"RemoveKey": func() error { _, err := removeKeyWith(dir, "acme", cat); return err },
	} {
		err := op()
		if !errors.Is(err, ErrSymlinkedKeyFile) || !strings.Contains(err.Error(), "edit its target by hand") {
			t.Fatalf("%s through a symlink = %v; want ErrSymlinkedKeyFile saying to edit the target", name, err)
		}
		if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("%s replaced the symlink", name)
		}
		if got, _ := os.ReadFile(target); string(got) != keyFileBody {
			t.Fatalf("%s wrote through the symlink", name)
		}
	}
}

// TestKeyStoreLockFailureSurfaces (astra 18): a lock that cannot be taken is
// the caller's error — never a write without it, as tui.lockConfig allows —
// and the file is left alone.
func TestKeyStoreLockFailureSurfaces(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	dir := writeFiles(t, keyFileBody, "")
	if err := os.Mkdir(filepath.Join(dir, keyLockName), 0o700); err != nil {
		t.Fatal(err) // a directory where the lock file goes: it cannot be opened
	}
	if err := setKeyWith(dir, "router", "sk-router-new-0009", cat); err == nil || !strings.Contains(err.Error(), "locking") {
		t.Fatalf("SetKey without its lock = %v; want the lock's error", err)
	}
	if _, err := removeKeyWith(dir, "acme", cat); err == nil || !strings.Contains(err.Error(), "locking") {
		t.Fatalf("RemoveKey without its lock = %v; want the lock's error", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ProvidersFile)); string(got) != keyFileBody {
		t.Fatal("the file changed although the lock was never taken")
	}
}

// TestSetKeyConcurrentWriters: writers storing different providers' keys at
// once each read, change and write the whole file, so without the lock one
// would write back a file that lacks another's key. Every key survives.
func TestSetKeyConcurrentWriters(t *testing.T) {
	const n = 12
	var body strings.Builder
	body.WriteString("version = 1\n")
	for i := range n {
		fmt.Fprintf(&body, "\n[providers.p%02d]\ndriver = \"openrouter\"\n", i)
	}
	dir := writeFiles(t, body.String(), "")
	cat := testCatalog(t, testCatalogV1)
	errs := make(chan error, n)
	for i := range n {
		go func() { errs <- setKeyWith(dir, fmt.Sprintf("p%02d", i), fmt.Sprintf("sk-writer-%02d-0010", i), cat) }()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	got, err := StoredKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("%d of %d keys survived concurrent writers", len(got), n)
	}
	for i, k := range got {
		if k.Key.Reveal() != fmt.Sprintf("sk-writer-%02d-0010", i) {
			t.Fatalf("provider %s holds the wrong key", k.Provider)
		}
	}
}

// TestTwoBrokenKeysRepairedOneAtATime (r2-7): with two stored keys the rules
// refuse, the table does not load; storing a good key for one keeps the other
// exactly as it was — Load still fails, now on it alone — and storing the
// second makes the table load.
func TestTwoBrokenKeysRepairedOneAtATime(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	dir := writeFiles(t, "version = 1\n\n[providers.acme]\napi_key = \"zq9-w7\"\n\n[providers.router]\napi_key = \"credential\"\n", "")
	if _, err := LoadWith(dir, cat); err == nil {
		t.Fatal("control: a table with two broken keys loaded")
	}
	infos, err := providersWith(dir, fakeEnv(nil), cat)
	if err != nil {
		t.Fatalf("Providers must list a directory whose keys are broken: %v", err)
	}
	if !errors.Is(infoOf(t, infos, "acme").StoredProblem, ErrKeyTooShort) ||
		!errors.Is(infoOf(t, infos, "router").StoredProblem, ErrKeyOverlapsMarker) {
		t.Fatalf("Providers did not report both broken keys: %+v", infos)
	}

	if err := setKeyWith(dir, "acme", "sk-acme-good-0011", cat); err != nil {
		t.Fatalf("repairing the first: %v", err)
	}
	if o := readDoc(t, dir).Providers["router"]; o.APIKey == nil || *o.APIKey != "credential" {
		t.Fatal("repairing one broken key changed the other")
	}
	_, err = LoadWith(dir, cat)
	wantFileError(t, err, filepath.Join(dir, ProvidersFile), "providers.router", "api_key")

	if err := setKeyWith(dir, "router", "sk-router-good-0012", cat); err != nil {
		t.Fatalf("repairing the second: %v", err)
	}
	if _, err := LoadWith(dir, cat); err != nil {
		t.Fatalf("both repaired, the table still does not load: %v", err)
	}
}

// TestMalformedStoredKeyReplacedAndRemoved (A3): a stored key the rules
// refuse can be removed as well as replaced, and removing a key-only entry
// drops it.
func TestMalformedStoredKeyReplacedAndRemoved(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	dir := writeFiles(t, "version = 1\n\n[providers.acme]\napi_key = \"zq9-w7\"\n", "")
	removed, err := removeKeyWith(dir, "acme", cat)
	if err != nil || !removed {
		t.Fatalf("RemoveKey of a malformed key = %v, %v; want it removed", removed, err)
	}
	if _, ok := readDoc(t, dir).Providers["acme"]; ok {
		t.Fatal("an entry that held only the key was kept empty")
	}
	if _, err := LoadWith(dir, cat); err != nil {
		t.Fatalf("with the broken key gone the table must load: %v", err)
	}
	info := infoOf(t, mustProviders(t, dir, cat), "acme")
	if info.Stored || info.StoredProblem != nil || info.Via != KeyNone {
		t.Fatalf("after removal acme = %+v; want nothing stored and not connected", info)
	}
}

// TestRemoveKeyClearsOnlyTheKey (panel GLM 5): the entry keeps every hand
// field; one left with nothing but a source goes; a provider with no key
// stored, or a directory with no providers.toml, is (false, nil) with nothing
// written and nothing made.
func TestRemoveKeyClearsOnlyTheKey(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	dir := writeFiles(t, keyFileBody, "")
	removed, err := removeKeyWith(dir, "local", cat)
	if err != nil || !removed {
		t.Fatalf("RemoveKey local = %v, %v", removed, err)
	}
	doc := readDoc(t, dir)
	local := doc.Providers["local"]
	if local.APIKey != nil || local.Driver == nil || local.BaseURL == nil || local.EnvKeys == nil || len(*local.EnvKeys) != 0 ||
		local.Name == nil || local.Source == nil {
		t.Fatalf("RemoveKey changed more than the key: %+v", local)
	}
	if doc.Providers["acme"].APIKey == nil {
		t.Fatal("RemoveKey of one provider removed another's key")
	}

	// source and a key: the key goes, and the entry with it.
	removed, err = removeKeyWith(dir, "acme", cat)
	if err != nil || !removed {
		t.Fatalf("RemoveKey acme = %v, %v", removed, err)
	}
	if o, ok := readDoc(t, dir).Providers["acme"]; ok && o.EnvKeys == nil {
		t.Fatalf("acme kept an entry with nothing in it: %+v", o)
	}

	path := filepath.Join(dir, ProvidersFile)
	before, _ := os.ReadFile(path)
	stamp := func() time.Time { info, _ := os.Stat(path); return info.ModTime() }
	was := stamp()
	for _, id := range []string{"router", "local"} { // no key stored (any more)
		if removed, err := removeKeyWith(dir, id, cat); err != nil || removed {
			t.Fatalf("RemoveKey %s with no key = %v, %v; want (false, nil)", id, removed, err)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) || !stamp().Equal(was) {
		t.Fatal("RemoveKey with nothing to remove wrote the file")
	}
	if _, err := removeKeyWith(dir, "nosuch", cat); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("RemoveKey of an unknown provider = %v", err)
	}

	fresh := filepath.Join(t.TempDir(), "native")
	if removed, err := removeKeyWith(fresh, "acme", cat); err != nil || removed {
		t.Fatalf("RemoveKey with no providers.toml = %v, %v; want (false, nil)", removed, err)
	}
	if _, err := os.Stat(fresh); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("RemoveKey made a directory to remove nothing from")
	}
	if _, err := removeKeyWith(fresh, "nosuch", cat); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("RemoveKey of an unknown provider with no file = %v", err)
	}
}

// TestProvidersIsStructural: every shipped and user provider, sorted by
// display name, each funded exactly as Resolve would fund it — the first
// usable variable, else a usable stored key — with an unusable variable
// skipped, a stored key the rules refuse reported instead of failing, an
// entry the merge drops still listed, the legacy rule's extra variable and an
// endpoint override's dropped variables honoured, and nothing written.
func TestProvidersIsStructural(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	dir := writeFiles(t, `version = 1

[providers.acme]
source = "gx"
env_keys = ["ACME_OLD_KEY"]

[providers.router]
name = "Zed Router"
driver = "openai-compat"
base_url = "http://127.0.0.1:9/v1"
api_key = "sk-router-stored-0013"

[providers.local]
driver = "openai-compat"
base_url = "http://127.0.0.1:9/v1"
env_keys = ["LOCAL_API_KEY"]
api_key = "zq9-w7"

[providers.half]
name = "Half"
api_key = "sk-half-stored-0014"
`, "")
	before, _ := os.ReadFile(filepath.Join(dir, ProvidersFile))
	env := fakeEnv(map[string]string{
		"ACME_API_KEY":   "short", // too short: skipped
		"ACME_OLD_KEY":   "sk-acme-exported-0015",
		"ROUTER_API_KEY": "sk-router-exported-0016", // the override dropped this variable
		"LOCAL_API_KEY":  "",
	})
	infos, err := providersWith(dir, env, cat)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, p := range infos {
		order = append(order, p.Name)
	}
	if want := []string{"Acme", "Half", "local", "Zed Router"}; !slices.Equal(order, want) {
		t.Fatalf("Providers order = %q; want %q (display name, case-insensitive)", order, want)
	}
	acme := infoOf(t, infos, "acme")
	if acme.Via != KeyFromEnv || acme.EnvVar != "ACME_OLD_KEY" || !slices.Equal(acme.EnvKeys, []string{"ACME_API_KEY", "ACME_OLD_KEY"}) {
		t.Fatalf("acme = %+v; want funded by the legacy entry's extra variable, the short one skipped", acme)
	}
	router := infoOf(t, infos, "router")
	if router.Via != KeyStored || router.EnvKeys != nil || !router.Stored {
		t.Fatalf("router = %+v; want its stored key, the shipped variable dropped with the shipped endpoint", router)
	}
	local := infoOf(t, infos, "local")
	if local.Via != KeyNone || !local.Stored || !errors.Is(local.StoredProblem, ErrKeyTooShort) {
		t.Fatalf("local = %+v; want not connected, its stored key reported unusable", local)
	}
	if half := infoOf(t, infos, "half"); half.Via != KeyStored || half.Name != "Half" {
		t.Fatalf("half (an entry the merge drops: no driver) = %+v; want it listed with its stored key", half)
	}
	for _, p := range infos {
		noKeyIn(t, []string{"sk-router-stored-0013", "zq9-w7", "sk-half-stored-0014", "sk-acme-exported-0015"}, fmt.Sprintf("%+v %v", p, p))
	}
	if after, _ := os.ReadFile(filepath.Join(dir, ProvidersFile)); string(after) != string(before) {
		t.Fatal("Providers wrote the file")
	}

	// No providers.toml: the catalog alone, nothing connected.
	infos = mustProviders(t, t.TempDir(), cat)
	if len(infos) != 2 || infos[0].ID != "acme" || infos[1].ID != "router" || infos[0].Via != KeyNone {
		t.Fatalf("Providers of an empty directory = %+v; want acme and router, unconnected", infos)
	}
	if _, err := providersWith("", nil, cat); err == nil {
		t.Fatal("Providers(\"\") read something: it would have read the working directory")
	}
}

// TestKeyStoreHonoursCatalogFalse (P10): a directory whose models.toml turns
// the catalog off has only its own providers for key management too — a
// shipped id is unknown there, since an incomplete entry would break its
// table — while a models.toml that does not parse leaves the catalog on, so
// keys can still be managed while it is fixed.
func TestKeyStoreHonoursCatalogFalse(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	own := "version = 1\n\n[providers.local]\ndriver = \"openai-compat\"\nbase_url = \"http://127.0.0.1:9/v1\"\n"
	dir := writeFiles(t, own, "version = 1\ncatalog = false\ndefault_model = \"m\"\n\n[models.m]\nprovider = \"local\"\nwire_model = \"w\"\n")
	infos := mustProviders(t, dir, cat)
	if len(infos) != 1 || infos[0].ID != "local" {
		t.Fatalf("Providers under catalog = false = %+v; want the file's own alone", infos)
	}
	if err := setKeyWith(dir, "acme", "sk-acme-new-0017", cat); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("SetKey of a shipped id under catalog = false = %v; want ErrUnknownProvider", err)
	}
	if err := setKeyWith(dir, "local", "sk-local-new-0018", cat); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWith(dir, cat); err != nil {
		t.Fatalf("the table stopped loading after SetKey: %v", err)
	}

	broken := writeFiles(t, own, "version = 1\ncatalog = false\n[models.m\n")
	if got := mustProviders(t, broken, cat); len(got) != 3 {
		t.Fatalf("Providers with a models.toml that does not parse = %+v; want the catalog's and the file's", got)
	}
}

// TestProvidersStructuralErrorsNameNoValue: providers.toml is read strictly,
// so its syntax, an unknown key or a wrong version fail Providers and the
// writers, with an error that names the file and never a value.
func TestProvidersStructuralErrorsNameNoValue(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	const key = "sk-on-a-broken-line-0019"
	for name, body := range map[string]string{
		"syntax":      "version = 1\n\n[providers.acme]\napi_key = \"" + key + "\n",
		"unknown key": "version = 1\n\n[providers.acme]\napikey = \"" + key + "\"\n",
		"version":     "version = 2\n\n[providers.acme]\napi_key = \"" + key + "\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeFiles(t, body, "")
			_, perr := providersWith(dir, nil, cat)
			serr := setKeyWith(dir, "acme", "sk-acme-new-0020", cat)
			_, rerr := removeKeyWith(dir, "acme", cat)
			for _, err := range []error{perr, serr, rerr} {
				if err == nil || !strings.Contains(err.Error(), ProvidersFile) {
					t.Fatalf("got %v; want the file refused, by name", err)
				}
				noKeyIn(t, []string{key, "sk-acme-new-0020"}, err.Error())
			}
			if got, _ := os.ReadFile(filepath.Join(dir, ProvidersFile)); string(got) != body {
				t.Fatal("a file that does not decode was rewritten")
			}
		})
	}
}

// TestSetKeyOverTheShippedCatalog: the exported store over the catalog this
// binary ships. A key stored for fireworks in an empty directory funds its
// models through Load, and Providers says so; RemoveKey takes it back.
func TestSetKeyOverTheShippedCatalog(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "native")
	const key = "sk-fireworks-stored-0021"
	if err := SetKey(dir, "fireworks", key); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, filepath.Join(dir, ProvidersFile)); m != providersPerm {
		t.Fatalf("providers.toml is %04o", m)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != dirPerm {
		t.Fatalf("the directory SetKey made: %v, %v; want 0700", info, err)
	}
	tbl, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tbl.Resolve(tbl.DefaultModel, fakeEnv(nil))
	if err != nil || got.APIKey.Reveal() != key {
		t.Fatalf("the default model is not funded by the stored key: %v", err)
	}
	infos, err := Providers(dir, fakeEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if p := infoOf(t, infos, "fireworks"); p.Via != KeyStored || p.Name != "Fireworks" {
		t.Fatalf("fireworks = %+v; want Fireworks, stored key", p)
	}
	if removed, err := RemoveKey(dir, "fireworks"); err != nil || !removed {
		t.Fatalf("RemoveKey = %v, %v", removed, err)
	}
	if keys, err := StoredKeys(dir); err != nil || len(keys) != 0 {
		t.Fatalf("after RemoveKey the file still stores %v (%v)", keys, err)
	}
	if err := SetKey(dir, "nosuch", key); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("SetKey nosuch = %v", err)
	}
}

func mustProviders(t *testing.T, dir string, cat *Catalog) []ProviderInfo {
	t.Helper()
	infos, err := providersWith(dir, fakeEnv(nil), cat)
	if err != nil {
		t.Fatal(err)
	}
	return infos
}

func infoOf(t *testing.T, infos []ProviderInfo, id string) ProviderInfo {
	t.Helper()
	for _, p := range infos {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no provider %q in %+v", id, infos)
	return ProviderInfo{}
}
