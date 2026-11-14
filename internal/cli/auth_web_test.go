package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/charliek/craze/internal/harness/modeltable"
)

// craze auth for the web search's key (plan 051 §F.5, R4): stored in
// native/credentials.toml, addressed as websearch/exa (or a bare exa no model
// provider shadows), listed under "Web search". Every case runs over a temp
// CRAZE_HOME with EXA_API_KEY unset (TestMain), uses this file's dummy keys —
// which runAuth checks reach no output, error or printed line — and checks
// what was written.

// webStored is the Exa key native's credentials.toml stores, "" for none.
func webStored(t *testing.T, native string) string {
	t.Helper()
	got, err := modeltable.StoredCredentials(native)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.ID == "exa" {
			return c.Key.Reveal()
		}
	}
	return ""
}

// credentialsPath is native's credentials.toml.
func credentialsPath(native string) string { return filepath.Join(native, modeltable.CredentialsFile) }

// writeNative writes name in native, making native.
func writeNative(t *testing.T, native, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(native, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAuthWebLoginFromStdin: login websearch/exa — and bare exa, which no
// model provider has — reads the key from stdin's first line, trimmed,
// stores it in credentials.toml at 0600 and says where, leaving
// providers.toml alone; the note after says what EXA_API_KEY does to it: used
// first when it holds a key, skipped when it cannot be one.
func TestAuthWebLoginFromStdin(t *testing.T) {
	native := authNative(t)
	stdout, stderr, err := runAuth(t, strings.NewReader("  "+authKey+" \r\nsecond line\n"), "auth", "login", "websearch/exa")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Saved the Exa web search key in " + credentialsPath(native) + ".\n"; stdout != want || stderr != "" {
		t.Fatalf("stdout %q stderr %q; want %q and nothing", maskKeys(stdout), maskKeys(stderr), want)
	}
	if webStored(t, native) != authKey {
		t.Fatal("the stored key is not stdin's first line, trimmed")
	}
	if info, err := os.Stat(credentialsPath(native)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials.toml: %v, %v; want 0600", info, err)
	}
	if _, err := os.Stat(filepath.Join(native, modeltable.ProvidersFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("storing the web search key wrote providers.toml")
	}

	for _, c := range []struct {
		name, env, note string
	}{
		{"exported", authEnvK, "EXA_API_KEY is set in this environment; craze uses it before the stored key.\n"},
		{"short export", shortKey, "EXA_API_KEY is set in this environment to a value that cannot be a key (it is shorter than 8 bytes); craze skips it and uses the stored key.\n"},
		{"marker export", "credential", "EXA_API_KEY is set in this environment to a value that cannot be a key (it overlaps craze's redaction marker); craze skips it and uses the stored key.\n"},
		{"blank export", "  ", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("EXA_API_KEY", c.env)
			stdout, _, err := runAuth(t, strings.NewReader(authKey2+"\n"), "auth", "login", "EXA")
			if err != nil {
				t.Fatal(err)
			}
			if want := "Saved the Exa web search key in " + credentialsPath(native) + ".\n" + c.note; stdout != want {
				t.Fatalf("stdout %q; want %q", maskKeys(stdout), want)
			}
			if webStored(t, native) != authKey2 {
				t.Fatal("bare exa did not store the web search key")
			}
		})
	}
}

// TestAuthWebLoginRefusals: a key that cannot be one, an empty stdin and an
// over-long line are refused exactly as a provider's, naming the credential
// and the rule, never the value, with nothing written — no directory made.
func TestAuthWebLoginRefusals(t *testing.T) {
	for _, c := range []struct {
		name, stdin string
		want        string
	}{
		{"empty stdin", "", "craze auth login: no key given; nothing was saved"},
		{"a 3-byte key", shortKey + "\n", "craze auth login: the key for websearch/exa was not saved: shorter than 8 bytes"},
		{"a key the marker prints back", "credential\n", "craze auth login: the key for websearch/exa was not saved: overlaps craze's redaction marker"},
		{"a line over 8 KiB", strings.Repeat("k", maxKeyLine+1), "the key is longer than 8 KiB; nothing was saved"},
	} {
		t.Run(c.name, func(t *testing.T) {
			native := authNative(t)
			stdout, _, err := runAuth(t, strings.NewReader(c.stdin), "auth", "login", "websearch/exa")
			wantExit(t, err, 1, c.want)
			if strings.Contains(err.Error(), shortKey) || stdout != "" {
				t.Fatalf("the refusal quotes the key or printed %q", maskKeys(stdout))
			}
			if _, err := os.Stat(native); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused login made %s", native)
			}
		})
	}
}

// TestAuthWebLogout: logout websearch/exa clears the stored key and says so,
// says so when there was none — writing nothing then, not even the directory
// — and names the variable that still keys the search.
func TestAuthWebLogout(t *testing.T) {
	native := authNative(t)
	stdout, _, err := runAuth(t, nil, "auth", "logout", "websearch/exa")
	if err != nil || stdout != "No stored Exa web search key.\n" {
		t.Fatalf("logout with nothing stored = %q, %v", stdout, err)
	}
	if _, err := os.Stat(native); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a logout of nothing made the directory")
	}
	if _, _, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "websearch/exa"); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runAuth(t, nil, "auth", "logout", "Websearch/Exa")
	if err != nil || stdout != "Removed the stored Exa web search key.\n" || stderr != "" {
		t.Fatalf("logout = %q, %q, %v", stdout, stderr, err)
	}
	if webStored(t, native) != "" {
		t.Fatal("the key is still stored")
	}
	stdout, _, err = runAuth(t, nil, "auth", "logout", "exa")
	if err != nil || stdout != "No stored Exa web search key.\n" {
		t.Fatalf("a second logout = %q, %v", stdout, err)
	}
	t.Setenv("EXA_API_KEY", authEnvK)
	stdout, _, err = runAuth(t, nil, "auth", "logout", "exa")
	if err != nil || stdout != "No stored Exa web search key.\nExa web search is still connected through EXA_API_KEY.\n" {
		t.Fatalf("logout with the variable set = %q, %v", maskKeys(stdout), err)
	}
}

// webSection is the "Web search" section of craze auth list's stdout, from
// its heading.
func webSection(t *testing.T, stdout string) string {
	t.Helper()
	_, section, ok := strings.Cut(stdout, "Web search\n")
	if !ok {
		t.Fatalf("no Web search section in %q", maskKeys(stdout))
	}
	return section
}

// TestAuthWebList: the Web search section after the providers, in each
// state — no key (the keyless endpoint), a stored key, the variable — with a
// variable that cannot be a key shown as unusable, beside a stored key and
// without one, and said once on stderr by its rule; and a stored key that
// cannot be one said by its rule, never the value.
func TestAuthWebList(t *testing.T) {
	native := authNative(t)
	row := func(state string) string { return "Exa  websearch/exa  " + state + "\n" }
	for _, c := range []struct {
		name, env, stored string
		row               string
		note              string
	}{
		{"nothing", "", "", row("no key (keyless endpoint)"), ""},
		{"stored", "", authKey, row("stored key"), ""},
		{"env", authEnvK, "", row("env EXA_API_KEY"), ""},
		{"env beats stored", authEnvK, authKey, row("env EXA_API_KEY"), ""},
		{"blank env, stored", "   ", authKey, row("stored key"), ""},
		{"short env, stored", shortKey, authKey, row("stored key; EXA_API_KEY is set but unusable"),
			"note: EXA_API_KEY is set to a value shorter than 8 bytes, which cannot be an API key; it is ignored\n"},
		{"marker env, nothing stored", "credential", "", row("no key (keyless endpoint); EXA_API_KEY is set but unusable"),
			"note: EXA_API_KEY is set to a value that overlaps craze's redaction marker, which would print the key back in its own place; it is ignored\n"},
		{"short stored", "", shortKey + "-w7", row("no key (keyless endpoint)"),
			"note: " + credentialsPath(native) + ": [exa] api_key: shorter than 8 bytes: too short to be a real key, or to redact from tool output; it is ignored (replace it with \"craze auth login websearch/exa\")\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_ = os.Remove(credentialsPath(native))
			if c.stored != "" {
				writeNative(t, native, modeltable.CredentialsFile, "[exa]\napi_key = \""+c.stored+"\"\n")
			}
			t.Setenv("EXA_API_KEY", c.env)
			stdout, stderr, err := runAuth(t, nil, "auth", "list")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stdout, "Z.AI Coding Plan  zai-coding-plan  not connected\n\nWeb search\n") {
				t.Fatalf("the section does not follow the providers after a blank line: %q", maskKeys(stdout))
			}
			if got := webSection(t, stdout); got != c.row {
				t.Fatalf("the Web search section = %q; want %q", maskKeys(got), c.row)
			}
			if c.note != "" && strings.Count(stderr, c.note) != 1 || c.note == "" && strings.Contains(stderr, "EXA") {
				t.Fatalf("stderr = %q; want the note %q once", maskKeys(stderr), c.note)
			}
			if strings.Contains(stderr, shortKey) {
				t.Fatal("a note quotes the unusable value")
			}
		})
	}
}

// TestAuthWebShadowing (F.5): a model provider named exa — by id, or by
// display name — takes the bare name, before and after a search key was
// stored: bare exa logs that provider in and out (its key in providers.toml),
// while websearch/exa still stores, lists, resolves and removes the search
// key, which keeps working. Two providers sharing the display name make bare
// exa ambiguous — a provider error naming the full name among the choices —
// never the search key.
func TestAuthWebShadowing(t *testing.T) {
	for _, c := range []struct {
		name, entry, id string
		shown           string // the provider's display name
	}{
		{"by id", "[providers.exa]\ndriver = \"openai-compat\"\nbase_url = \"https://exa-models.example/v1\"\nenv_keys = [\"EXA_MODELS_KEY\"]\n", "exa", "exa"},
		{"by display name", "[providers.mine]\nname = \"Exa\"\ndriver = \"openai-compat\"\nbase_url = \"https://exa-models.example/v1\"\nenv_keys = [\"EXA_MODELS_KEY\"]\n", "mine", "Exa"},
	} {
		for _, storedFirst := range []bool{false, true} {
			name := c.name
			if storedFirst {
				name += ", a search key stored before"
			}
			t.Run(name, func(t *testing.T) {
				native := authNative(t)
				if storedFirst {
					if _, _, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "exa"); err != nil {
						t.Fatal(err)
					}
				}
				writeNative(t, native, modeltable.ProvidersFile, "version = 1\n\n"+c.entry)

				stdout, _, err := runAuth(t, strings.NewReader(authKey2+"\n"), "auth", "login", "exa")
				if err != nil || !strings.HasPrefix(stdout, "Saved the "+c.shown+" key in "+filepath.Join(native, modeltable.ProvidersFile)) {
					t.Fatalf("bare exa = %q, %v; want the provider's key saved", maskKeys(stdout), err)
				}
				if storedKey(t, native, c.id) != authKey2 {
					t.Fatal("bare exa did not store the provider's key")
				}
				if want := map[bool]string{true: authKey, false: ""}[storedFirst]; webStored(t, native) != want {
					t.Fatal("bare exa touched the search key")
				}

				if _, _, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "websearch/exa"); err != nil {
					t.Fatal(err)
				}
				if webStored(t, native) != authKey || storedKey(t, native, c.id) != authKey2 {
					t.Fatal("websearch/exa did not store the search key alone")
				}
				// Resolution is unaffected: a session's table sends it.
				table, err := modeltable.Load(native)
				if err != nil {
					t.Fatal(err)
				}
				if table.WebSearchKey(os.Getenv).Reveal() != authKey {
					t.Fatal("the stored search key does not resolve under the shadowing provider")
				}
				stdout, _, err = runAuth(t, nil, "auth", "list")
				if err != nil || webSection(t, stdout) != "Exa  websearch/exa  stored key\n" {
					t.Fatalf("list = %q, %v", maskKeys(stdout), err)
				}

				stdout, _, err = runAuth(t, nil, "auth", "logout", "exa")
				if err != nil || stdout != "Removed the stored "+c.shown+" key.\n" {
					t.Fatalf("bare logout = %q, %v; want the provider's key removed", maskKeys(stdout), err)
				}
				if webStored(t, native) != authKey {
					t.Fatal("a bare logout removed the search key")
				}
				stdout, _, err = runAuth(t, nil, "auth", "logout", "websearch/exa")
				if err != nil || stdout != "Removed the stored Exa web search key.\n" || webStored(t, native) != "" {
					t.Fatalf("logout websearch/exa = %q, %v", stdout, err)
				}
			})
		}
	}

	t.Run("an ambiguous display name", func(t *testing.T) {
		native := authNative(t)
		writeNative(t, native, modeltable.ProvidersFile, "version = 1\n\n"+
			"[providers.one]\nname = \"Exa\"\ndriver = \"openai-compat\"\nbase_url = \"https://one.example/v1\"\nenv_keys = [\"ONE_KEY\"]\n\n"+
			"[providers.two]\nname = \"exa\"\ndriver = \"openai-compat\"\nbase_url = \"https://two.example/v1\"\nenv_keys = [\"TWO_KEY\"]\n")
		for _, argv := range [][]string{{"auth", "login", "exa"}, {"auth", "logout", "Exa"}} {
			_, _, err := runAuth(t, strings.NewReader(authKey+"\n"), argv...)
			wantExit(t, err, 2, "more than one provider has that name; name it by id (", "websearch/exa)")
		}
		if _, err := os.Stat(credentialsPath(native)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("an ambiguous name stored the search key")
		}
		if _, _, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "websearch/exa"); err != nil || webStored(t, native) != authKey {
			t.Fatalf("the full name under an ambiguous display name: %v", err)
		}
	})
}

// TestAuthWebPastABrokenProvidersFile (R4): with a providers.toml craze cannot
// read, websearch/exa is still stored, listed and removed — it is dispatched
// before the file is read — while bare exa fails as any provider name does,
// its message naming websearch/exa; list prints the Web search section and
// then fails on the providers. A models.toml craze cannot read stops none of
// it.
func TestAuthWebPastABrokenProvidersFile(t *testing.T) {
	native := authNative(t)
	ppath := writeNative(t, native, modeltable.ProvidersFile, "version = 1\n[providers.x\n")
	if _, _, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "websearch/exa"); err != nil {
		t.Fatalf("login websearch/exa past a broken providers.toml: %v", err)
	}
	if webStored(t, native) != authKey {
		t.Fatal("the key was not stored")
	}
	for _, argv := range [][]string{{"auth", "login", "exa"}, {"auth", "logout", "EXA"}} {
		_, _, err := runAuth(t, strings.NewReader(authKey2+"\n"), argv...)
		wantExit(t, err, 1, ppath, "not valid TOML", "for the Exa web search key, whatever that file holds, name it websearch/exa")
	}
	t.Setenv("EXA_API_KEY", shortKey)
	stdout, stderr, err := runAuth(t, nil, "auth", "list")
	wantExit(t, err, 1, ppath, "not valid TOML")
	if stdout != "Web search\nExa  websearch/exa  stored key; EXA_API_KEY is set but unusable\n" {
		t.Fatalf("list past a broken providers.toml printed %q; want the Web search section alone", maskKeys(stdout))
	}
	if !strings.Contains(stderr, "note: EXA_API_KEY is set to a value shorter than 8 bytes") {
		t.Fatalf("list's notes past a broken providers.toml = %q; want the variable's", stderr)
	}
	t.Setenv("EXA_API_KEY", "")
	// A provider name still fails as before, without the hint.
	_, _, err = runAuth(t, strings.NewReader(authKey2+"\n"), "auth", "login", "fireworks")
	wantExit(t, err, 1, ppath)
	if strings.Contains(err.Error(), "websearch/exa") {
		t.Fatalf("a provider's failure names the search key: %q", maskKeys(err.Error()))
	}
	if stdout, _, err := runAuth(t, nil, "auth", "logout", "websearch/exa"); err != nil || stdout != "Removed the stored Exa web search key.\n" {
		t.Fatalf("logout websearch/exa past a broken providers.toml = %q, %v", stdout, err)
	}

	// A broken models.toml: everything works.
	if err := os.Remove(ppath); err != nil {
		t.Fatal(err)
	}
	writeNative(t, native, modeltable.ModelsFile, "version = 1\n[models.x\n")
	for _, argv := range [][]string{{"auth", "login", "exa"}, {"auth", "login", "websearch/exa"}} {
		if _, _, err := runAuth(t, strings.NewReader(authKey+"\n"), argv...); err != nil || webStored(t, native) != authKey {
			t.Fatalf("%v past a broken models.toml: %v", argv, err)
		}
	}
	stdout, _, err = runAuth(t, nil, "auth", "list")
	if err != nil || webSection(t, stdout) != "Exa  websearch/exa  stored key\n" {
		t.Fatalf("list past a broken models.toml = %q, %v", maskKeys(stdout), err)
	}
	if stdout, _, err := runAuth(t, nil, "auth", "logout", "exa"); err != nil || stdout != "Removed the stored Exa web search key.\n" {
		t.Fatalf("logout exa past a broken models.toml = %q, %v", stdout, err)
	}
}

// TestAuthWebDamagedCredentialsFile (F.5, R4): a credentials.toml that is not
// valid TOML — its bad line holding a key — a symlink, a directory, or one
// whose [exa] is not a table: login and logout refuse it with nothing
// written; list shows no key and one note naming the file. The key is in no
// output or error (runAuth), and the file is as it was.
func TestAuthWebDamagedCredentialsFile(t *testing.T) {
	for _, c := range []struct {
		name string
		make func(t *testing.T, native string) (check func(t *testing.T))
		says string
	}{
		{"not valid TOML", func(t *testing.T, native string) func(t *testing.T) {
			body := "[exa]\napi_key = \"" + authKey + "\n"
			p := writeNative(t, native, modeltable.CredentialsFile, body)
			return func(t *testing.T) { sameFile(t, p, body) }
		}, "line 2: not valid TOML"},
		{"exa not a table", func(t *testing.T, native string) func(t *testing.T) {
			body := "exa = \"" + authKey + "\"\n"
			p := writeNative(t, native, modeltable.CredentialsFile, body)
			return func(t *testing.T) { sameFile(t, p, body) }
		}, "exa: not a table"},
		{"a symlink", func(t *testing.T, native string) func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "elsewhere.toml")
			body := "[exa]\napi_key = \"" + authKey + "\"\n"
			if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(native, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, credentialsPath(native)); err != nil {
				t.Fatal(err)
			}
			return func(t *testing.T) {
				if info, err := os.Lstat(credentialsPath(native)); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("the symlink was replaced")
				}
				sameFile(t, target, body)
			}
		}, "is a symlink"},
		{"a directory", func(t *testing.T, native string) func(t *testing.T) {
			if err := os.MkdirAll(credentialsPath(native), 0o700); err != nil {
				t.Fatal(err)
			}
			return func(t *testing.T) {
				if info, err := os.Lstat(credentialsPath(native)); err != nil || !info.IsDir() {
					t.Fatal("the directory was replaced")
				}
			}
		}, "is not a regular file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			native := authNative(t)
			check := c.make(t, native)
			for _, argv := range [][]string{{"auth", "login", "websearch/exa"}, {"auth", "logout", "websearch/exa"}} {
				stdout, _, err := runAuth(t, strings.NewReader(authKey2+"\n"), argv...)
				wantExit(t, err, 1, credentialsPath(native), c.says)
				if stdout != "" {
					t.Fatalf("%v printed %q", argv, maskKeys(stdout))
				}
				check(t)
			}
			stdout, stderr, err := runAuth(t, nil, "auth", "list")
			if err != nil || webSection(t, stdout) != "Exa  websearch/exa  no key (keyless endpoint)\n" {
				t.Fatalf("list = %q, %v", maskKeys(stdout), err)
			}
			if strings.Count(stderr, credentialsPath(native)) != 1 || !strings.Contains(stderr, c.says) {
				t.Fatalf("list's notes = %q; want the file named once", maskKeys(stderr))
			}
			check(t)
		})
	}
}

// sameFile fails when path no longer holds body; it never prints either.
func sameFile(t *testing.T, path, body string) {
	t.Helper()
	if b, err := os.ReadFile(path); err != nil || string(b) != body {
		t.Fatalf("%s changed (%v)", path, err)
	}
}

// TestAuthWebLoginOnATerminal (F.5): on a terminal, websearch/exa is asked
// for with the echo off — the key typed the moment the prompt shows is never
// on the screen — and the menu offers the web search after the providers,
// numbered on, its number picking it.
func TestAuthWebLoginOnATerminal(t *testing.T) {
	native := authNative(t)
	screen, stdout, err := ptyAuth(t, func(tail *ptyTail, ptmx *os.File) {
		typeAtPrompt(t, tail, ptmx, "Exa API key: ", authKey)
	}, "auth", "login", "websearch/exa")
	if err != nil || !strings.HasPrefix(stdout, "Saved the Exa web search key in ") || webStored(t, native) != authKey {
		t.Fatalf("login websearch/exa on a terminal: %q, %v (screen %q)", maskKeys(stdout), err, maskKeys(screen))
	}

	t.Setenv("EXA_API_KEY", authEnvK)
	screen, stdout, err = ptyAuth(t, func(tail *ptyTail, ptmx *os.File) {
		typeAtPrompt(t, tail, ptmx, "Provider [1-8]: ", "8")
		typeAtPrompt(t, tail, ptmx, "Exa API key: ", authKey2)
	}, "auth", "login")
	if err != nil {
		t.Fatalf("login from the menu: %v (screen %q)", err, maskKeys(screen))
	}
	menu := strings.ReplaceAll(screen, "\r\n", "\n")
	if !strings.Contains(menu, "  7. Z.AI Coding Plan\nWeb search:\n  8. Exa (websearch/exa) (connected)\nProvider [1-8]: 8\nExa API key: \n") {
		t.Fatalf("the menu:\n%s", maskKeys(menu))
	}
	if want := "Saved the Exa web search key in " + credentialsPath(native) + ".\nEXA_API_KEY is set in this environment; craze uses it before the stored key.\n"; stdout != want || webStored(t, native) != authKey2 {
		t.Fatalf("stdout %q; the menu's choice was not the search key", maskKeys(stdout))
	}
}

// TestAuthWebMenuFullNameBeatsAProvidersDisplayName (review C5 item 4): a
// model provider whose display name is websearch/exa does not capture the
// full name typed at the menu — the answer names the web search, as login's
// argument does — so the key is stored in credentials.toml and the
// provider's entry in providers.toml is untouched.
func TestAuthWebMenuFullNameBeatsAProvidersDisplayName(t *testing.T) {
	native := authNative(t)
	writeNative(t, native, modeltable.ProvidersFile, "version = 1\n\n"+
		"[providers.mine]\nname = \"websearch/exa\"\ndriver = \"openai-compat\"\nbase_url = \"https://mine.example/v1\"\nenv_keys = [\"MINE_KEY\"]\n")
	screen, stdout, err := ptyAuth(t, func(tail *ptyTail, ptmx *os.File) {
		typeAtPrompt(t, tail, ptmx, "Provider [1-9]: ", "websearch/exa")
		typeAtPrompt(t, tail, ptmx, " API key: ", authKey)
	}, "auth", "login")
	if err != nil {
		t.Fatalf("login from the menu: %v (screen %q)", err, maskKeys(screen))
	}
	if storedKey(t, native, "mine") != "" {
		t.Fatal("the full name typed at the menu stored the key as the provider named websearch/exa")
	}
	if !strings.HasPrefix(stdout, "Saved the Exa web search key in "+credentialsPath(native)) || webStored(t, native) != authKey {
		t.Fatalf("stdout %q; want the search key stored", maskKeys(stdout))
	}
}

// TestAuthWebFIFOIsNotRead: a credentials.toml that is a FIFO is refused as
// not a regular file, and nothing blocks on it.
func TestAuthWebFIFOIsNotRead(t *testing.T) {
	native := authNative(t)
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(credentialsPath(native), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	_, _, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "websearch/exa")
	wantExit(t, err, 1, "is not a regular file")
	stdout, _, err := runAuth(t, nil, "auth", "list")
	if err != nil || webSection(t, stdout) != "Exa  websearch/exa  no key (keyless endpoint)\n" {
		t.Fatalf("list = %q, %v", stdout, err)
	}
}
