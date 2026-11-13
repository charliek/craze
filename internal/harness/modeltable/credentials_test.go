package modeltable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// The stored web search key (plan 051 §F.5): credentials.toml, read
// leniently, written strictly under its own lock, every diagnostic
// value-free. Every key is a dummy of at least MinKeyLen bytes sharing no
// text with the redaction marker; a failure never prints one (credLeak).

const (
	credKey   = "exa-stored-canary-0001"
	credKey2  = "exa-stored-canary-0002"
	credEnvK  = "exa-exported-canary-0003"
	credOther = "unknown-table-secret-0004"
)

// credLeak fails when text — where says what it is — holds any of the
// file's dummy keys, saying which and where, never the text.
func credLeak(t *testing.T, where, text string, keys ...string) {
	t.Helper()
	for _, k := range append(keys, credKey, credKey2, credEnvK, credOther) {
		if i := strings.Index(text, k); i >= 0 {
			t.Fatalf("%s holds a stored value at byte %d", where, i)
		}
	}
}

// writeCreds writes body as dir's credentials.toml at perm.
func writeCreds(t *testing.T, dir, body string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, CredentialsFile)
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

// decodeCreds is path decoded as the reader decodes it.
func decodeCreds(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if _, err := toml.Decode(string(b), &doc); err != nil {
		t.Fatalf("the written file does not decode: %v", err)
	}
	return doc
}

// TestTheRegistry: one credential, exa, named Exa, keyed by EXA_API_KEY —
// the variable every table lists (webCredentialEnv) — addressed in full as
// websearch/exa; WebCredentials is a copy.
func TestTheRegistry(t *testing.T) {
	got := WebCredentials()
	if len(got) != 1 || got[0] != (WebCredential{ID: "exa", Name: "Exa", Env: ExaKeyEnv}) || got[0].FullName() != "websearch/exa" {
		t.Fatalf("WebCredentials = %+v", got)
	}
	var env []string
	for _, c := range got {
		env = append(env, c.Env)
	}
	if !slices.Equal(env, webCredentialEnv) {
		t.Fatalf("the registry's variables %q are not webCredentialEnv %q", env, webCredentialEnv)
	}
	got[0].ID = "changed"
	if webCredentials[0].ID != "exa" {
		t.Fatal("WebCredentials returned the registry itself")
	}
}

// TestStoredCredentialsReadsLeniently: a missing file is no keys and no
// error; unknown tables and keys, at the top or inside [exa], are ignored; a
// value is returned as written, untrimmed and unjudged (a short one too);
// a blank one is none; and the file's mode is left alone.
func TestStoredCredentialsReadsLeniently(t *testing.T) {
	dir := t.TempDir()
	if got, err := StoredCredentials(dir); err != nil || got != nil {
		t.Fatalf("no file: %+v, %v", got, err)
	}
	if _, err := StoredCredentials(""); err == nil {
		t.Fatal("no directory: want an error")
	}
	path := writeCreds(t, dir, `version = 7
note = "anything"

[exa]
api_key = "  `+credKey+`\n"
region = "eu"

[exa.nested]
x = 1

[tavily]
api_key = "`+credOther+`"

[[later]]
y = true
`, 0o640)
	got, err := StoredCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "exa" || got[0].Key.Reveal() != "  "+credKey+"\n" {
		t.Fatalf("StoredCredentials = %d entries; want exa's, untrimmed", len(got))
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Fatalf("a reading changed the mode to %04o", info.Mode().Perm())
	}
	writeCreds(t, dir, "[exa]\napi_key = \"zq9\"\n", 0o600)
	if got, err := StoredCredentials(dir); err != nil || len(got) != 1 || got[0].Key.Reveal() != "zq9" {
		t.Fatalf("a short value: %+v, %v; want it returned unjudged", len(got), err)
	}
	writeCreds(t, dir, "[exa]\napi_key = \"   \"\n", 0o600)
	if got, err := StoredCredentials(dir); err != nil || got != nil {
		t.Fatalf("a blank value: %d, %v; want none", len(got), err)
	}
}

// credentialsDamage is every way the file can be unreadable or misshapen,
// each with a stored value somewhere in it: whole is a problem of the file
// (nothing read from it), else of one entry.
var credentialsDamage = []struct {
	name, body string
	whole      bool
	want       []string // in the diagnostic
}{
	{"an unterminated string holding the key", "[exa]\napi_key = \"" + credKey + "\n", true, []string{"line 2", "not valid TOML"}},
	{"a bare key", "[exa]\napi_key = " + credKey + "\n", true, []string{"line 2", "not valid TOML"}},
	{"the key twice", "[exa]\napi_key = \"" + credKey + "\"\napi_key = \"" + credKey2 + "\"\n", true, []string{"line 3", "not valid TOML"}},
	{"the key as a table name", "[" + credKey + "\n", true, []string{"line ", "not valid TOML"}},
	{"exa not a table", "exa = \"" + credKey + "\"\n", false, []string{"exa: not a table"}},
	{"exa an array of tables", "[[exa]]\napi_key = \"" + credKey + "\"\n", false, []string{"exa: not a table"}},
	{"api_key not a string", "[exa]\napi_key = [\"" + credKey + "\"]\n", false, []string{"[exa] api_key: not a string"}},
}

// TestCredentialsDamageIsValueFree (F.5, R4): a file that is not valid TOML
// — its bad line holding a key — yields no stored credentials and one
// diagnostic naming the file and the line; an [exa] that is not a table, or
// an api_key that is not a string, is no stored key and one diagnostic
// naming the table or key. Not one of StoredCredentials' error, WebKeys'
// lines or a table's Warnings (merged and `catalog = false`) holds the
// value, and the table still loads.
func TestCredentialsDamageIsValueFree(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	for _, c := range credentialsDamage {
		t.Run(c.name, func(t *testing.T) {
			for _, models := range []string{"", "version = 1\ncatalog = false\ndefault_model = \"acme/x\"\n\n[models.\"acme/x\"]\nprovider = \"acme\"\nwire_model = \"x\"\n"} {
				providers := ""
				if models != "" {
					providers = "version = 1\n\n[providers.acme]\ndriver = \"openai-compat\"\nbase_url = \"https://proxy.example/v1\"\napi_key = \"stored-proxy-key-01\"\n"
				}
				dir := writeFiles(t, providers, models)
				path := writeCreds(t, dir, c.body, 0o600)
				got, err := StoredCredentials(dir)
				if len(got) != 0 {
					t.Fatalf("%d stored credentials read from a damaged file", len(got))
				}
				if c.whole != (err != nil) {
					t.Fatalf("StoredCredentials error = %v; want one: %v", err != nil, c.whole)
				}
				if err != nil {
					credLeak(t, "StoredCredentials' error", err.Error())
					if !strings.Contains(err.Error(), path) {
						t.Fatalf("the error does not name the file")
					}
				}
				infos, lines := WebKeys(dir, fakeEnv(nil))
				if infos[0].Stored || infos[0].Via != KeyNone || len(lines) != 1 {
					t.Fatalf("WebKeys = %+v with %d lines; want no key and one line", infos[0], len(lines))
				}
				credLeak(t, "WebKeys' line", lines[0])
				for _, w := range append([]string{path}, c.want...) {
					if !strings.Contains(lines[0], w) {
						t.Fatalf("the line lacks %q (it is %d bytes)", w, len(lines[0]))
					}
				}
				tbl, err := LoadWith(dir, cat)
				if err != nil {
					t.Fatalf("a damaged credentials.toml fails the load: %v", err)
				}
				if !slices.Contains(tbl.Warnings, lines[0]) {
					t.Fatalf("the table's warnings lack the line WebKeys says (%d warnings)", len(tbl.Warnings))
				}
				credLeak(t, "the table's warnings", strings.Join(tbl.Warnings, "\n"))
				if tbl.WebSearchKey(fakeEnv(nil)) != "" {
					t.Fatal("a key was read from a damaged file")
				}
				keys, _ := tbl.Keys(fakeEnv(nil))
				if countKey(keys, credKey) != 0 || countKey(keys, credKey2) != 0 {
					t.Fatal("Keys holds a value read from a damaged file")
				}
			}
		})
	}
}

// TestCredentialsSymlinkAndNonRegularYieldNothing (F.5): a credentials.toml
// that is a symlink — even to a good file — or a directory or a FIFO yields
// no stored credentials and one diagnostic naming the file, never a value;
// the reading neither follows the link nor blocks on the pipe.
func TestCredentialsSymlinkAndNonRegularYieldNothing(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	for name, mk := range map[string]func(t *testing.T, path string){
		"a symlink to a good file": func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "elsewhere.toml")
			if err := os.WriteFile(target, []byte("[exa]\napi_key = \""+credKey+"\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
		"a directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"a FIFO": func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, CredentialsFile)
			mk(t, path)
			got, err := StoredCredentials(dir)
			if len(got) != 0 || err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("StoredCredentials = %d, %v; want nothing and an error naming the file", len(got), err)
			}
			credLeak(t, "the error", err.Error())
			_, lines := WebKeys(dir, fakeEnv(nil))
			if len(lines) != 1 {
				t.Fatalf("WebKeys said %d lines; want one", len(lines))
			}
			tbl, err := LoadWith(dir, cat)
			if err != nil || tbl.WebSearchKey(fakeEnv(nil)) != "" || !slices.Contains(tbl.Warnings, lines[0]) {
				t.Fatalf("LoadWith = %v; want a table with no stored key and the line among its warnings", err)
			}
		})
	}
}

// credentialsWithUnknowns is a file holding, beside [exa]'s key, content
// craze does not know: top-level keys, every TOML value type, nested and
// inline tables, an array of tables, another table's secret.
const credentialsWithUnknowns = `version = 3
owner = "me"

[exa]
api_key = "` + credKey + `"
region = "eu"
retries = 3
weights = [1.5, 2.5]

[exa.limits]
per_minute = 60

[tavily]
api_key = "` + credOther + `"
enabled = true
since = 2026-01-02
at = 07:32:00
when = 2026-01-02T03:04:05Z
local = 2026-01-02T03:04:05
inline = { a = 1, b = "two" }

[[mirrors]]
url = "https://a.example/"

[[mirrors]]
url = "https://b.example/"
`

// TestCredentialWritesKeepWhatTheyDoNotKnow (F.5, R4): SetCredential and
// RemoveCredential change exa's api_key alone. Every other table and key —
// [exa]'s own included, every value type — reads back the same; the file is
// 0600 whatever it was, under the header, with no temp file left; and a
// removal that leaves [exa] empty drops it.
func TestCredentialWritesKeepWhatTheyDoNotKnow(t *testing.T) {
	dir := t.TempDir()
	path := writeCreds(t, dir, credentialsWithUnknowns, 0o644)
	before := decodeCreds(t, path)

	if err := SetCredential(dir, "exa", "  "+credKey2+"\n"); err != nil {
		t.Fatal(err)
	}
	after := decodeCreds(t, path)
	want := decodeCreds(t, path) // a deep copy to edit
	want["exa"].(map[string]any)["api_key"] = before["exa"].(map[string]any)["api_key"]
	if !reflect.DeepEqual(want, before) {
		t.Fatalf("SetCredential changed something besides exa's api_key")
	}
	if got := after["exa"].(map[string]any)["api_key"]; got != credKey2 {
		t.Fatal("SetCredential did not store the key, trimmed")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the written file: %v, %v; want 0600", info, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), credentialsHeader) {
		t.Fatal("the written file does not start with the header")
	}
	// The encoder would move a local value's wall clock by the machine's
	// offset (keepLocalTimes): written back as it was read.
	for _, local := range []string{"at = 07:32:00", "local = 2026-01-02T03:04:05", "since = 2026-01-02", "when = 2026-01-02T03:04:05Z"} {
		if !strings.Contains(string(b), local) {
			t.Fatalf("the written file lacks %q:\n%s", local, strings.ReplaceAll(strings.ReplaceAll(string(b), credKey2, "<KEY>"), credOther, "<KEY>"))
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 { // the file and its lock
		t.Fatalf("the directory holds %d entries; want the file and its lock", len(entries))
	}

	removed, err := RemoveCredential(dir, "exa")
	if err != nil || !removed {
		t.Fatalf("RemoveCredential = %v, %v", removed, err)
	}
	gone := decodeCreds(t, path)
	if _, has := gone["exa"].(map[string]any)["api_key"]; has {
		t.Fatal("the key is still there")
	}
	delete(want["exa"].(map[string]any), "api_key")
	if !reflect.DeepEqual(gone, want) {
		t.Fatal("RemoveCredential changed something besides exa's api_key")
	}
	if removed, err := RemoveCredential(dir, "exa"); err != nil || removed {
		t.Fatalf("a second RemoveCredential = %v, %v; want nothing removed", removed, err)
	}

	// A table left with nothing is dropped; the rest stays.
	writeCreds(t, dir, "[exa]\napi_key = \""+credKey+"\"\n\n[tavily]\nx = 1\n", 0o600)
	if removed, err := RemoveCredential(dir, "exa"); err != nil || !removed {
		t.Fatalf("RemoveCredential = %v, %v", removed, err)
	}
	if got := decodeCreds(t, path); !reflect.DeepEqual(got, map[string]any{"tavily": map[string]any{"x": int64(1)}}) {
		t.Fatalf("after the removal the file decodes to %v", got)
	}
	// A non-string api_key is removed too (it is no key, but it is the key's
	// place).
	writeCreds(t, dir, "[exa]\napi_key = 12345678\nkeep = 1\n", 0o600)
	if removed, err := RemoveCredential(dir, "exa"); err != nil || !removed {
		t.Fatalf("RemoveCredential of a non-string = %v, %v", removed, err)
	}
	if got := decodeCreds(t, path); !reflect.DeepEqual(got, map[string]any{"exa": map[string]any{"keep": int64(1)}}) {
		t.Fatalf("after the removal the file decodes to %v", got)
	}
	// And a store into a missing file makes it, and the directory.
	fresh := filepath.Join(t.TempDir(), "native")
	if err := SetCredential(fresh, "exa", credKey); err != nil {
		t.Fatal(err)
	}
	if got := decodeCreds(t, filepath.Join(fresh, CredentialsFile)); !reflect.DeepEqual(got, map[string]any{"exa": map[string]any{"api_key": credKey}}) {
		t.Fatal("a new file holds something else")
	}
	if info, err := os.Stat(fresh); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the directory made: %v, %v; want 0700", info, err)
	}
}

// TestSetCredentialRefusesInvalidUTF8 (review C5 item 2): a key that is not
// valid UTF-8 — which the encoder would write into a quoted string every
// later decode rejects — is refused before anything is read or written,
// naming the rule and never the value; a file already there is unchanged,
// and still reads.
func TestSetCredentialRefusesInvalidUTF8(t *testing.T) {
	const bad = "exa-key-12345678\xff"
	fresh := filepath.Join(t.TempDir(), "native")
	err := SetCredential(fresh, "exa", bad)
	if !errors.Is(err, ErrKeyNotUTF8) || !strings.Contains(err.Error(), "the key for websearch/exa was not saved: ") {
		t.Fatalf("SetCredential(invalid UTF-8) into a new directory = %v; want ErrKeyNotUTF8", err)
	}
	if strings.Contains(err.Error(), "exa-key-12345678") {
		t.Fatal("the refusal quotes the key")
	}
	if _, err := os.Stat(fresh); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a refused key made the directory")
	}

	dir := t.TempDir()
	path := writeCreds(t, dir, "[exa]\napi_key = \""+credKey+"\"\n", 0o600)
	if err := SetCredential(dir, "exa", bad); !errors.Is(err, ErrKeyNotUTF8) {
		t.Fatalf("SetCredential(invalid UTF-8) over a good file = %v; want ErrKeyNotUTF8", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "[exa]\napi_key = \""+credKey+"\"\n" {
		t.Fatal("a refused key rewrote the file")
	}
	if got, err := StoredCredentials(dir); err != nil || len(got) != 1 || got[0].Key.Reveal() != credKey {
		t.Fatalf("the file after the refusal: %d keys, %v; want the old key", len(got), err)
	}
	if err := SetCredential(dir, "exa", credKey2); err != nil {
		t.Fatalf("a later store: %v", err)
	}
}

// TestLoadTightensAMalformedCredentialsFile (review C5 item 2): a regular
// credentials.toml group or others can read is made 0600 by the load whether
// or not it parses — a syntax error on a line holding a key included — with
// a value-free warning; a symlink's target is left alone.
func TestLoadTightensAMalformedCredentialsFile(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	for _, body := range []string{
		"[exa]\napi_key = \"" + credKey + "\"\n",
		"[exa]\napi_key = \"" + credKey + "\n",
		"[exa]\napi_key = \"" + credKey + "\"\n[[broken\n",
	} {
		dir := t.TempDir()
		path := writeCreds(t, dir, body, 0o644)
		tbl, err := LoadWith(dir, cat)
		if err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("after the load the file is %v, %v; want 0600", info.Mode().Perm(), err)
		}
		warnings := strings.Join(tbl.Warnings, "\n")
		credLeak(t, "the table's warnings", warnings)
		if !strings.Contains(warnings, path+" holds API keys and was mode 0644; tightened it to 0600") {
			t.Fatalf("the warnings do not say the file was tightened (%d warnings)", len(tbl.Warnings))
		}
	}

	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.toml")
	if err := os.WriteFile(target, []byte("[exa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, CredentialsFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWith(dir, cat); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o644 {
		t.Fatal("the load changed a symlink's target's mode")
	}
}

// TestCredentialsFileOverTheCap (review C5 item 2): a credentials.toml over
// maxCredentialsFile bytes is damaged, however valid its TOML: no key is read
// from it — one value-free line says so — and SetCredential and
// RemoveCredential refuse it with nothing written.
func TestCredentialsFileOverTheCap(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	body := "[exa]\napi_key = \"" + credKey + "\"\n" + strings.Repeat("# padding\n", maxCredentialsFile/10+1)
	dir := t.TempDir()
	path := writeCreds(t, dir, body, 0o600)
	got, err := StoredCredentials(dir)
	if len(got) != 0 || err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("StoredCredentials of an over-cap file = %d keys, %v; want none and an error naming the file and the cap", len(got), err)
	}
	credLeak(t, "the error", err.Error())
	infos, lines := WebKeys(dir, fakeEnv(nil))
	if infos[0].Stored || len(lines) != 1 {
		t.Fatalf("WebKeys = %+v with %d lines; want no key and one line", infos[0], len(lines))
	}
	credLeak(t, "WebKeys' line", lines[0])
	tbl, err := LoadWith(dir, cat)
	if err != nil || tbl.WebSearchKey(fakeEnv(nil)) != "" || !slices.Contains(tbl.Warnings, lines[0]) {
		t.Fatalf("LoadWith = %v; want a table with no stored key and the line among its warnings", err)
	}
	for name, op := range map[string]func() error{
		"SetCredential":    func() error { return SetCredential(dir, "exa", credKey2) },
		"RemoveCredential": func() error { _, err := RemoveCredential(dir, "exa"); return err },
	} {
		err := op()
		if err == nil || !strings.Contains(err.Error(), "nothing was written") {
			t.Fatalf("%s over an over-cap file = %v; want a refusal", name, err)
		}
		credLeak(t, name+"'s error", err.Error())
		if b, _ := os.ReadFile(path); string(b) != body {
			t.Fatalf("%s wrote the over-cap file", name)
		}
	}
}

// localZoneChild marks the run of TestCredentialWritesKeepLocalTimes that
// TestCredentialWritesKeepLocalTimes starts under a fixed zone.
const localZoneChild = "CRAZE_TEST_LOCAL_ZONE_CHILD"

// localZone is the zone that run starts in: a nonzero offset, so the
// encoder's move to UTC would change every local value's text.
const localZone = "Asia/Kolkata"

// TestCredentialWritesKeepLocalTimes (F.5, review C5 item 7): a rewrite
// writes every local date, time and date-time back as it was read
// (keepLocalTimes), whatever the machine's zone. The decoder takes the
// process's offset once, when the toml package initializes — before any
// test can set time.Local — so the test runs itself again in a process
// started with TZ set to localZone, a nonzero offset (+05:30) on any
// machine, a UTC one included; without keepLocalTimes the encoder would
// write 2026-01-01T21:34:05, 2026-01-01 and 02:02:00 there.
func TestCredentialWritesKeepLocalTimes(t *testing.T) {
	if os.Getenv(localZoneChild) == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCredentialWritesKeepLocalTimes$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), localZoneChild+"=1", "TZ="+localZone)
		out, err := cmd.CombinedOutput()
		text := strings.NewReplacer(credKey, "<KEY>", credKey2, "<KEY>", credOther, "<KEY>").Replace(string(out))
		if err != nil || !strings.Contains(text, "--- PASS: TestCredentialWritesKeepLocalTimes") {
			t.Fatalf("the run under TZ=%s: %v\n%s", localZone, err, text)
		}
		return
	}
	if _, off := time.Now().Zone(); off == 0 {
		t.Fatalf("TZ=%s gave this process a zero offset; the case would prove nothing", localZone)
	}
	dir := t.TempDir()
	path := writeCreds(t, dir, credentialsWithUnknowns, 0o600)
	if err := SetCredential(dir, "exa", credKey2); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, local := range []string{"at = 07:32:00\n", "local = 2026-01-02T03:04:05\n", "since = 2026-01-02\n", "when = 2026-01-02T03:04:05Z\n"} {
		if !strings.Contains(string(b), local) {
			t.Fatalf("the written file lacks %q:\n%s", local, strings.NewReplacer(credKey2, "<KEY>", credOther, "<KEY>").Replace(string(b)))
		}
	}
}

// TestSetCredentialRefusals (F.5, R4): a key that cannot be one is refused
// before anything is read or written — no directory is made — naming the
// rule and never the value; a file that cannot be parsed (its bad line
// holding a key), a symlink, a non-regular file, and an [exa] that is not a
// table are refused under the lock with nothing written, by SetCredential
// and RemoveCredential alike, every error value-free; an id the registry
// lacks is refused.
func TestSetCredentialRefusals(t *testing.T) {
	for _, c := range []struct {
		key  string
		rule error
		says string
	}{
		{"", ErrEmptyKey, "it is empty"},
		{"  \n", ErrEmptyKey, "it is empty"},
		{"zq9-w7", ErrKeyTooShort, "shorter than 8 bytes"},
		{"credential", ErrKeyOverlapsMarker, "overlaps craze's redaction marker"},
	} {
		dir := filepath.Join(t.TempDir(), "native")
		err := SetCredential(dir, "exa", c.key)
		if !errors.Is(err, c.rule) || !strings.Contains(err.Error(), "the key for websearch/exa was not saved: "+c.says) {
			t.Fatalf("SetCredential(%d bytes) = %v; want %v", len(c.key), err, c.rule)
		}
		if strings.TrimSpace(c.key) != "" && strings.Contains(err.Error(), strings.TrimSpace(c.key)) {
			t.Fatal("the refusal quotes the key")
		}
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("a refused key made the directory")
		}
	}

	if err := SetCredential(t.TempDir(), "nosuch", credKey); !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("an unknown id: %v", err)
	}
	if _, err := RemoveCredential(t.TempDir(), "nosuch"); !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("an unknown id: %v", err)
	}

	ops := map[string]func(dir string) error{
		"SetCredential":    func(dir string) error { return SetCredential(dir, "exa", credKey2) },
		"RemoveCredential": func(dir string) error { _, err := RemoveCredential(dir, "exa"); return err },
	}
	for _, d := range credentialsDamage {
		if d.name == "api_key not a string" {
			continue // a table: login replaces the value, logout removes it (TestCredentialWritesKeepWhatTheyDoNotKnow)
		}
		for name, op := range ops {
			dir := t.TempDir()
			path := writeCreds(t, dir, d.body, 0o644)
			err := op(dir)
			if err == nil {
				t.Fatalf("%s over %s: want a refusal", name, d.name)
			}
			credLeak(t, name+"'s error", err.Error())
			if d.whole == errors.Is(err, ErrCredentialNotATable) || !strings.Contains(err.Error(), path) {
				t.Fatalf("%s over %s = %v", name, d.name, err)
			}
			if b, _ := os.ReadFile(path); string(b) != d.body {
				t.Fatalf("%s over %s wrote the file", name, d.name)
			}
			if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
				t.Fatalf("%s over %s changed the file's mode", name, d.name)
			}
		}
	}

	for name, op := range ops {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "elsewhere.toml")
		body := "[exa]\napi_key = \"" + credKey + "\"\n"
		if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, CredentialsFile)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := op(dir); !errors.Is(err, ErrSymlinkedCredentialsFile) || !strings.Contains(err.Error(), "nothing was written") {
			t.Fatalf("%s through a symlink = %v", name, err)
		}
		if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("%s replaced the symlink", name)
		}
		if b, _ := os.ReadFile(target); string(b) != body {
			t.Fatalf("%s wrote through the symlink", name)
		}

		dir = t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, CredentialsFile), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := op(dir); !errors.Is(err, ErrCredentialsNotRegular) {
			t.Fatalf("%s over a directory = %v", name, err)
		}
	}
}

// TestCredentialsLockFailureSurfaces: a lock that cannot be taken is the
// caller's error — never a write without it — and the file is left alone.
func TestCredentialsLockFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	body := "[exa]\napi_key = \"" + credKey + "\"\n"
	path := writeCreds(t, dir, body, 0o600)
	if err := os.Mkdir(filepath.Join(dir, credentialsLockName), 0o700); err != nil {
		t.Fatal(err) // a directory where the lock file goes: it cannot be opened
	}
	if err := SetCredential(dir, "exa", credKey2); err == nil || !strings.Contains(err.Error(), "locking") {
		t.Fatalf("SetCredential without its lock = %v", err)
	}
	if _, err := RemoveCredential(dir, "exa"); err == nil || !strings.Contains(err.Error(), "locking") {
		t.Fatalf("RemoveCredential without its lock = %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != body {
		t.Fatal("the file changed although the lock was never taken")
	}
	// Its own lock: the provider store's is another file.
	if credentialsLockName == keyLockName {
		t.Fatal("the two stores share a lock name")
	}
}

// TestCredentialsConcurrentWriters: writers each read, change and write the
// whole file, so without the lock one would write back a file that lacks
// another's change. Each writer adds a table of its own while others store
// the key; every table, and a key, survive.
func TestCredentialsConcurrentWriters(t *testing.T) {
	const n = 12
	dir := t.TempDir()
	errs := make(chan error, 2*n)
	for i := range n {
		go func() {
			errs <- editCredentials(dir, true, func(_ string, doc map[string]any) (bool, error) {
				doc[fmt.Sprintf("other%02d", i)] = map[string]any{"n": int64(i)}
				return true, nil
			})
		}()
		go func() { errs <- SetCredential(dir, "exa", fmt.Sprintf("exa-writer-%02d-0005", i)) }()
	}
	for range 2 * n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	doc := decodeCreds(t, filepath.Join(dir, CredentialsFile))
	if len(doc) != n+1 {
		t.Fatalf("%d of %d tables survived concurrent writers", len(doc)-1, n)
	}
	got, err := StoredCredentials(dir)
	if err != nil || len(got) != 1 || !strings.HasPrefix(got[0].Key.Reveal(), "exa-writer-") {
		t.Fatalf("the stored key after concurrent writers: %d, %v", len(got), err)
	}
}

// TestWebKeys (F.5): each state `craze auth list` shows, judged as
// WebSearchKey resolves — the variable when usable, else a usable stored
// key, else none — with an unusable variable and an unusable stored key
// each reported by the rule, and skipped.
func TestWebKeys(t *testing.T) {
	for _, c := range []struct {
		name      string
		env       string
		stored    string
		via       KeySource
		envRule   error
		storedBad error
	}{
		{"nothing", "", "", KeyNone, nil, nil},
		{"stored", "", credKey, KeyStored, nil, nil},
		{"env", credEnvK, "", KeyFromEnv, nil, nil},
		{"env beats stored", credEnvK, credKey, KeyFromEnv, nil, nil},
		{"short env, stored", "zq9", credKey, KeyStored, ErrKeyTooShort, nil},
		{"blank env, stored", "   ", credKey, KeyStored, nil, nil},
		{"marker env, stored", "redacted-credential", credKey, KeyStored, ErrKeyOverlapsMarker, nil},
		{"short env, nothing stored", "zq9", "", KeyNone, ErrKeyTooShort, nil},
		{"marker env, nothing stored", "credential", "", KeyNone, ErrKeyOverlapsMarker, nil},
		{"short stored", "", "zq9-w7", KeyNone, nil, ErrKeyTooShort},
		{"env over a short stored", credEnvK, "zq9-w7", KeyFromEnv, nil, ErrKeyTooShort},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.stored != "" {
				writeCreds(t, dir, "[exa]\napi_key = \""+c.stored+"\"\n", 0o600)
			}
			infos, lines := WebKeys(dir, fakeEnv(map[string]string{ExaKeyEnv: c.env}))
			if len(infos) != 1 {
				t.Fatalf("%d infos", len(infos))
			}
			w := infos[0]
			if w.ID != "exa" || w.Via != c.via || !errors.Is(w.EnvProblem, c.envRule) || (c.envRule == nil) != (w.EnvProblem == nil) ||
				w.Stored != (c.stored != "") || !errors.Is(w.StoredProblem, c.storedBad) || (c.storedBad == nil) != (w.StoredProblem == nil) {
				t.Fatalf("WebKeys = %+v", w)
			}
			if w.Connected() != (c.via != KeyNone) {
				t.Fatal("Connected disagrees with Via")
			}
			wantLines := 0
			if c.storedBad != nil {
				wantLines++
			}
			if c.envRule != nil {
				wantLines++
				if !slices.Contains(lines, envWarning(ExaKeyEnv, c.envRule)) {
					t.Fatalf("the lines lack EnvWarnings' line for the variable")
				}
			}
			if len(lines) != wantLines {
				t.Fatalf("%d lines; want %d", len(lines), wantLines)
			}
			credLeak(t, "the lines", strings.Join(lines, "\n"), "zq9-w7")

			// The table loaded from the same directory resolves the same key.
			tbl, err := LoadWith(dir, testCatalog(t, testCatalogV1))
			if err != nil {
				t.Fatal(err)
			}
			want := map[KeySource]string{KeyNone: "", KeyStored: c.stored, KeyFromEnv: c.env}[c.via]
			if got := tbl.WebSearchKey(fakeEnv(map[string]string{ExaKeyEnv: c.env})).Reveal(); got != want {
				t.Fatalf("WebSearchKey disagrees with WebKeys' %s", c.via)
			}
		})
	}
}

// TestLoadReadsTheStoredWebKey (F.5, R3): every kind of loaded table —
// merged over a catalog and `catalog = false` — reads credentials.toml:
// Keys hands the redactor its usable key, which funds no model; WebSearchKey
// takes EXA_API_KEY first (env beats stored) and the stored key when the
// variable is unset or unusable; a stored key that cannot be one is a
// warning and is skipped; the file is made 0600, said in a warning; a key
// stored or rotated after the load is not the table's (one snapshot); and
// once WithholdFrozen marks the stored key frozen it is neither returned nor
// sent. A home with no file loads as before: no warning, no stored key.
func TestLoadReadsTheStoredWebKey(t *testing.T) {
	cat := testCatalog(t, testCatalogV1)
	const aloneModels = "version = 1\ncatalog = false\ndefault_model = \"acme/x\"\n\n[models.\"acme/x\"]\nprovider = \"acme\"\nwire_model = \"x\"\n"
	const aloneProviders = "version = 1\n\n[providers.acme]\ndriver = \"openai-compat\"\nbase_url = \"https://proxy.example/v1\"\napi_key = \"stored-proxy-key-01\"\n"
	for name, files := range map[string][2]string{"merged": {"", ""}, "catalog = false": {aloneProviders, aloneModels}} {
		t.Run(name, func(t *testing.T) {
			// An older-shape home: no credentials.toml.
			plain, dir := loadOver(t, cat, files[0], files[1])
			if plain.webKeys != nil || plain.WebSearchKey(fakeEnv(nil)) != "" {
				t.Fatal("a home with no credentials.toml has a stored web key")
			}
			for _, w := range plain.Warnings {
				if strings.Contains(w, CredentialsFile) {
					t.Fatalf("a home with no credentials.toml warns about it: %q", w)
				}
			}

			path := writeCreds(t, dir, "[exa]\napi_key = \"  "+credKey+" \"\n\n[tavily]\napi_key = \""+credOther+"\"\n", 0o644)
			tbl, err := LoadWith(dir, cat)
			if err != nil {
				t.Fatal(err)
			}
			if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
				t.Fatalf("the file is %04o after the load; want it tightened to 0600", info.Mode().Perm())
			}
			if !slices.ContainsFunc(tbl.Warnings, func(w string) bool { return strings.Contains(w, path) && strings.Contains(w, "tightened it to 0600") }) {
				t.Fatalf("no warning says the file was tightened: %q", tbl.Warnings)
			}
			keys, err := tbl.Keys(fakeEnv(nil))
			if err != nil || countKey(keys, credKey) != 1 || countKey(keys, credOther) != 0 {
				t.Fatalf("Keys holds the stored key %d times (want once, trimmed) and an unknown table's %d (want 0): %v",
					countKey(keys, credKey), countKey(keys, credOther), err)
			}
			if with, without := len(tbl.Choices(nil, fakeEnv(nil), "")), len(plain.Choices(nil, fakeEnv(nil), "")); with != without {
				t.Fatalf("the stored key funds something: %d models offered, %d without", with, without)
			}
			if got := tbl.WebSearchKey(fakeEnv(nil)).Reveal(); got != credKey {
				t.Fatal("WebSearchKey with nothing exported is not the stored key, trimmed")
			}
			env := fakeEnv(map[string]string{ExaKeyEnv: credEnvK})
			if got := tbl.WebSearchKey(env).Reveal(); got != credEnvK {
				t.Fatal("env does not beat stored")
			}
			if keys, _ := tbl.Keys(env); countKey(keys, credKey) != 1 || countKey(keys, credEnvK) != 1 {
				t.Fatal("Keys does not hold both the exported and the stored key")
			}
			for _, bad := range []string{"zq9", "redacted-credential", "  "} {
				if got := tbl.WebSearchKey(fakeEnv(map[string]string{ExaKeyEnv: bad})).Reveal(); got != credKey {
					t.Fatalf("an unusable EXA_API_KEY (%d bytes) is not skipped for the stored key", len(bad))
				}
			}

			// One snapshot (R3): a key stored after the load is not the table's.
			writeCreds(t, dir, "[exa]\napi_key = \""+credKey2+"\"\n", 0o600)
			if got := tbl.WebSearchKey(fakeEnv(nil)).Reveal(); got != credKey {
				t.Fatal("a rotation after the load changed the table's key")
			}
			if keys, _ := tbl.Keys(fakeEnv(nil)); countKey(keys, credKey2) != 0 {
				t.Fatal("a rotation after the load reached the table's Keys")
			}

			// Frozen stored and env keys.
			frozen := tbl.WithholdFrozen(env, func(v string) bool { return v == credKey || v == credEnvK }, "")
			_ = frozen
			if keys, _ := tbl.Keys(env); countKey(keys, credKey) != 0 || countKey(keys, credEnvK) != 0 {
				t.Fatal("after WithholdFrozen, Keys still holds a frozen key")
			}
			if got := tbl.WebSearchKey(env); got != "" {
				t.Fatal("after WithholdFrozen, WebSearchKey sends a frozen key")
			}

			// A stored key that cannot be one is skipped, and said.
			writeCreds(t, dir, "[exa]\napi_key = \"zq9-w7\"\n", 0o600)
			short, err := LoadWith(dir, cat)
			if err != nil {
				t.Fatal(err)
			}
			if short.WebSearchKey(fakeEnv(nil)) != "" {
				t.Fatal("an unusable stored key is sent")
			}
			if keys, _ := short.Keys(fakeEnv(nil)); countKey(keys, "zq9-w7") != 0 {
				t.Fatal("an unusable stored key is handed to the redactor")
			}
			if !slices.ContainsFunc(short.Warnings, func(w string) bool {
				return strings.Contains(w, path) && strings.Contains(w, "[exa] api_key: shorter than 8 bytes") && strings.Contains(w, "craze auth login websearch/exa")
			}) {
				t.Fatalf("no warning names the unusable stored key: %q", short.Warnings)
			}
			credLeak(t, "the warnings", strings.Join(short.Warnings, "\n"), "zq9-w7")
		})
	}
}

// TestEnvWarningsNameTheWebKeyVariable: an EXA_API_KEY that cannot be a key
// is said once, by name and rule, never the value — also when a provider
// declares the same variable.
func TestEnvWarningsNameTheWebKeyVariable(t *testing.T) {
	declares := validTable()
	setProvider(declares, "fireworks", func(p *Provider) { p.EnvKeys = append(p.EnvKeys, ExaKeyEnv) })
	for _, tbl := range []*Table{validTable(), declares} {
		got := tbl.EnvWarnings(fakeEnv(map[string]string{ExaKeyEnv: "zq9"}))
		if len(got) != 1 || !strings.Contains(got[0], ExaKeyEnv+" is set to a value shorter than 8 bytes") || strings.Contains(got[0], "zq9") {
			t.Fatalf("EnvWarnings = %q", got)
		}
		if got := tbl.EnvWarnings(fakeEnv(map[string]string{ExaKeyEnv: credEnvK})); len(got) != 0 {
			t.Fatalf("a usable key warned: %q", got)
		}
	}
}
