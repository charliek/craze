package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/charliek/craze/internal/harness/modeltable"
)

// craze auth (plan 031 §3.7). Every case runs over a temp CRAZE_HOME, and
// TestMain has unset every variable the shipped catalog names, so the four
// shipped providers start unconnected. Every key is a dummy of at least
// MinKeyLen bytes sharing no text with the redaction marker, and every case
// checks that none of them reaches stdout, stderr or an error (A7).

const (
	authKey  = "sk-auth-canary-0001"
	authKey2 = "sk-auth-canary-0002"
	authEnvK = "sk-auth-exported-0003"
	shortKey = "zq9"
)

// authNative points CRAZE_HOME at a fresh directory (crazeHome) and returns
// its native directory, where craze auth keeps keys.
func authNative(t *testing.T) string {
	t.Helper()
	return filepath.Join(crazeHome(t), "native")
}

// runAuth runs craze with argv, stdin as given, over the current CRAZE_HOME,
// and returns its stdout, its stderr and its error.
func runAuth(t *testing.T, stdin io.Reader, argv ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errw bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(stdin)
	cmd.SetOut(&out)
	cmd.SetErr(&errw)
	cmd.SetArgs(argv)
	err = cmd.Execute()
	for _, k := range []string{authKey, authKey2, authEnvK} {
		for _, text := range []string{out.String(), errw.String(), errString(err)} {
			if strings.Contains(text, k) || strings.Contains(text, k[3:14]) {
				t.Fatalf("craze %q: a key leaked into %q", argv, text)
			}
		}
	}
	return out.String(), errw.String(), err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// wantExit asserts err is an exit with code whose message holds every want.
func wantExit(t *testing.T, err error, code int, want ...string) {
	t.Helper()
	got, msg := exitCode(t, err)
	if got != code {
		t.Fatalf("exit %d %q; want exit %d", got, msg, code)
	}
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Fatalf("message %q does not say %q", msg, w)
		}
	}
}

// storedKey is provider's stored key in native, "" for none. The key is
// compared, never printed.
func storedKey(t *testing.T, native, provider string) string {
	t.Helper()
	keys, err := modeltable.StoredKeys(native)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Provider == provider {
			return k.Key.Reveal()
		}
	}
	return ""
}

// TestAuthLoginFromStdin: with stdin not a terminal, login reads the key from
// its first line, trimmed, stores it and says where; an exported variable of
// the provider's is said to win. Nothing goes to stderr.
func TestAuthLoginFromStdin(t *testing.T) {
	native := authNative(t)
	stdout, stderr, err := runAuth(t, strings.NewReader("  "+authKey+" \r\nsecond line\n"), "auth", "login", "fireworks")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Saved the Fireworks key in " + filepath.Join(native, "providers.toml") + ".\n"; stdout != want || stderr != "" {
		t.Fatalf("stdout %q stderr %q; want %q and nothing", stdout, stderr, want)
	}
	if storedKey(t, native, "fireworks") != authKey {
		t.Fatal("the stored key is not stdin's first line, trimmed")
	}
	if info, err := os.Stat(filepath.Join(native, "providers.toml")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("providers.toml: %v, %v; want 0600", info, err)
	}

	t.Setenv("META_API_KEY", authEnvK)
	stdout, _, err = runAuth(t, strings.NewReader(authKey2+"\n"), "auth", "login", "meta")
	if err != nil {
		t.Fatal(err)
	}
	if want := "META_API_KEY is set in this environment; craze uses it before the stored key.\n"; !strings.HasSuffix(stdout, want) {
		t.Fatalf("stdout %q; want it to end %q", stdout, want)
	}
	if storedKey(t, native, "meta") != authKey2 || storedKey(t, native, "fireworks") != authKey {
		t.Fatal("storing meta's key lost one of the two")
	}
}

// TestAuthLoginNamesAProviderByIdOrName: the argument is an id or a display
// name, in any case, with surrounding spaces ignored.
func TestAuthLoginNamesAProviderByIdOrName(t *testing.T) {
	native := authNative(t)
	for arg, id := range map[string]string{
		"FIREWORKS":        "fireworks",
		"z.ai coding plan": "zai-coding-plan",
		" OpenRouter ":     "openrouter",
		"Zai-Coding-Plan":  "zai-coding-plan",
	} {
		if _, _, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", arg); err != nil {
			t.Fatalf("login %q: %v", arg, err)
		}
		if storedKey(t, native, id) != authKey {
			t.Fatalf("login %q did not store %s's key", arg, id)
		}
		if _, _, err := runAuth(t, nil, "auth", "logout", id); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAuthLoginRefusals: each way login refuses, with its exit code and its
// words, never quoting the key — nor an argument that names no provider,
// which may be a key typed in its place — and with nothing written.
func TestAuthLoginRefusals(t *testing.T) {
	for _, c := range []struct {
		name  string
		stdin string
		argv  []string
		code  int
		want  []string
	}{
		{"no provider, no terminal", authKey + "\n", []string{"auth", "login"}, 2,
			[]string{"craze auth login: name a provider (fireworks, meta, openrouter, zai-coding-plan)", "only on a terminal"}},
		{"an unknown provider", authKey + "\n", []string{"auth", "login", "nosuch"}, 2,
			[]string{"craze auth login: no such provider; craze has fireworks, meta, openrouter, zai-coding-plan"}},
		{"a key where the provider goes", authKey + "\n", []string{"auth", "login", authKey2}, 2,
			[]string{"no such provider"}},
		{"empty stdin", "", []string{"auth", "login", "fireworks"}, 1,
			[]string{"craze auth login: no key given; nothing was saved"}},
		{"a blank first line", "  \n" + authKey + "\n", []string{"auth", "login", "fireworks"}, 1,
			[]string{"no key given"}},
		{"a 3-byte key", shortKey + "\n", []string{"auth", "login", "fireworks"}, 1,
			[]string{`the key for provider "fireworks" was not saved: shorter than 8 bytes`}},
		{"a key the marker prints back", "credential\n", []string{"auth", "login", "fireworks"}, 1,
			[]string{"was not saved: overlaps craze's redaction marker"}},
		{"a line over 8 KiB", strings.Repeat("k", maxKeyLine+1), []string{"auth", "login", "fireworks"}, 1,
			[]string{"the key is longer than 8 KiB; nothing was saved"}},
		{"logout of nothing named", "", []string{"auth", "logout"}, 2,
			[]string{"craze auth logout: name the provider whose stored key to remove (fireworks, meta, openrouter, zai-coding-plan)"}},
		{"logout of an unknown provider", "", []string{"auth", "logout", "nosuch"}, 2,
			[]string{"craze auth logout: no such provider"}},
		{"a mistyped subcommand", authKey + "\n", []string{"auth", "nosuch", "fireworks"}, 2,
			[]string{"craze auth: unknown command; want login, logout or list"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			native := authNative(t)
			stdout, _, err := runAuth(t, strings.NewReader(c.stdin), c.argv...)
			wantExit(t, err, c.code, c.want...)
			if strings.Contains(err.Error(), "nosuch") || strings.Contains(err.Error(), shortKey) {
				t.Fatalf("the refusal quotes what was typed: %v", err)
			}
			if stdout != "" {
				t.Fatalf("a refused command printed %q", stdout)
			}
			if _, err := os.Stat(native); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused command made %s", native)
			}
		})
	}
	t.Run("more than one argument", func(t *testing.T) {
		crazeHome(t)
		if _, _, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "fireworks", authKey2); err == nil {
			t.Fatal("login took two arguments")
		}
	})
}

// TestAuthBareIsItsHelp: `craze auth` alone prints the group's help and
// succeeds; only a word after it that names no subcommand is an error.
func TestAuthBareIsItsHelp(t *testing.T) {
	crazeHome(t)
	stdout, _, err := runAuth(t, nil, "auth")
	if err != nil || !strings.Contains(stdout, "Available Commands:") || !strings.Contains(stdout, "login") {
		t.Fatalf("craze auth = %q, %v; want its help", stdout, err)
	}
}

// TestAuthNeedsACrazeDirectory: with no home to find the craze directory in,
// all three commands fail before reading anything.
func TestAuthNeedsACrazeDirectory(t *testing.T) {
	t.Setenv("CRAZE_HOME", "")
	t.Setenv("HOME", "")
	for _, argv := range [][]string{{"auth", "login", "fireworks"}, {"auth", "logout", "fireworks"}, {"auth", "list"}} {
		_, _, err := runAuth(t, strings.NewReader(authKey+"\n"), argv...)
		wantExit(t, err, 1, "there is no craze directory to keep API keys in (set HOME or CRAZE_HOME)")
	}
}

// TestReadKeyLine: stdin's first line, at most 8 KiB, trimmed; nothing after
// it is used, and a longer line is refused rather than cut.
func TestReadKeyLine(t *testing.T) {
	full := strings.Repeat("k", maxKeyLine)
	for _, c := range []struct {
		in, want string
		err      error
	}{
		{"", "", nil},
		{"\n", "", nil},
		{authKey, authKey, nil},
		{" \t" + authKey + "\r\n" + authKey2 + "\n", authKey, nil},
		{full, full, nil},
		{full + "\n" + "tail", full, nil},
		{full + "\r\n", full, nil},
		{full + "k", "", errKeyLineTooLong},
		{full + "k\n", "", errKeyLineTooLong},
	} {
		got, err := readKeyLine(strings.NewReader(c.in))
		if got != c.want || !errors.Is(err, c.err) {
			t.Fatalf("readKeyLine(%d bytes) = %d bytes, %v; want %d bytes, %v", len(c.in), len(got), err, len(c.want), c.err)
		}
	}
}

// TestAuthLogout: logout clears a stored key and says so, says so when there
// was none, and names the variable that still funds the provider.
func TestAuthLogout(t *testing.T) {
	native := authNative(t)
	if _, _, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "fireworks"); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runAuth(t, nil, "auth", "logout", "Fireworks")
	if err != nil || stdout != "Removed the stored Fireworks key.\n" || stderr != "" {
		t.Fatalf("logout = %q, %q, %v", stdout, stderr, err)
	}
	if storedKey(t, native, "fireworks") != "" {
		t.Fatal("the key is still stored")
	}
	stdout, _, err = runAuth(t, nil, "auth", "logout", "fireworks")
	if err != nil || stdout != "No stored Fireworks key.\n" {
		t.Fatalf("a second logout = %q, %v", stdout, err)
	}
	t.Setenv("FIREWORKS_API_KEY", authEnvK)
	stdout, _, err = runAuth(t, nil, "auth", "logout", "fireworks")
	if err != nil || stdout != "No stored Fireworks key.\nFireworks is still connected through FIREWORKS_API_KEY.\n" {
		t.Fatalf("logout with the variable set = %q, %v", stdout, err)
	}
}

// TestAuthList: one row per provider by display name — name, id, and the
// variable, the stored key or nothing — then the notes on stderr: a stored
// key that cannot be used, a hand entry that repeats the catalog (§3.3), an
// unusable variable, and a table that does not load. No key anywhere.
func TestAuthList(t *testing.T) {
	native := authNative(t)
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	providers := "version = 1\n\n[providers.meta]\napi_key = \"" + authKey + "\"\n\n[providers.openrouter]\napi_key = \"" + authKey2 + "\"\n"
	if err := os.WriteFile(filepath.Join(native, "providers.toml"), []byte(providers), 0o600); err != nil {
		t.Fatal(err)
	}
	models := filepath.Join(native, "models.toml")
	if err := os.WriteFile(models, []byte("version = 1\ndefault_model = \"fireworks/deepseek-v4p1-flash\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIREWORKS_API_KEY", authEnvK)
	t.Setenv("OPENROUTER_API_KEY", authEnvK)
	t.Setenv("ZHIPU_API_KEY", shortKey)

	stdout, stderr, err := runAuth(t, nil, "auth", "list")
	if err != nil {
		t.Fatal(err)
	}
	wantRows := "" +
		"Fireworks         fireworks        env FIREWORKS_API_KEY\n" +
		"Meta              meta             stored key\n" +
		"OpenRouter        openrouter       env OPENROUTER_API_KEY\n" +
		"Z.AI Coding Plan  zai-coding-plan  not connected\n"
	if stdout != wantRows {
		t.Fatalf("list rows:\n%s\nwant:\n%s", stdout, wantRows)
	}
	wantNotes := "" +
		"note: " + models + ": default_model: repeats craze's shipped default — delete it to follow craze's updates\n" +
		"note: ZHIPU_API_KEY is set to a value shorter than 8 bytes, which cannot be an API key; it is ignored\n"
	if stderr != wantNotes {
		t.Fatalf("list notes:\n%s\nwant:\n%s", stderr, wantNotes)
	}

	// A stored key that cannot be one, and a models.toml that does not
	// parse: the rows still come, and each problem is a note.
	providers += "\n[providers.zai-coding-plan]\napi_key = \"" + shortKey + "\"\n"
	if err := os.WriteFile(filepath.Join(native, "providers.toml"), []byte(providers), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(models, []byte("version = 1\n[models.x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = runAuth(t, nil, "auth", "list")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != wantRows {
		t.Fatalf("list rows with broken files:\n%s\nwant:\n%s", stdout, wantRows)
	}
	for _, want := range []string{
		`note: the stored Z.AI Coding Plan key cannot be used: it is shorter than 8 bytes; replace it with "craze auth login zai-coding-plan" or remove it with "craze auth logout zai-coding-plan"` + "\n",
		"note: native sessions cannot start until this is fixed: ",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("list notes:\n%s\nwant them to hold %q", stderr, want)
		}
	}
	if strings.Contains(stderr, shortKey) {
		t.Fatal("a note quotes the short key")
	}
}

// TestAuthLoginKeepsAndReportsOtherBrokenKeys (r2-7): storing one provider's
// key keeps another's unusable stored key exactly as it was, and says so on
// stderr — by the rule, never the value — so it can be repaired next.
func TestAuthLoginKeepsAndReportsOtherBrokenKeys(t *testing.T) {
	native := authNative(t)
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "providers.toml"), []byte("version = 1\n\n[providers.meta]\napi_key = \"credential\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runAuth(t, strings.NewReader(authKey+"\n"), "auth", "login", "fireworks")
	if err != nil || !strings.HasPrefix(stdout, "Saved the Fireworks key in ") {
		t.Fatalf("login = %q, %v", stdout, err)
	}
	want := `note: the stored Meta key cannot be used: it overlaps craze's redaction marker; replace it with "craze auth login meta" or remove it with "craze auth logout meta"` + "\n"
	if stderr != want {
		t.Fatalf("stderr %q; want %q", stderr, want)
	}
	if storedKey(t, native, "meta") != "credential" {
		t.Fatal("login changed another provider's stored key")
	}
	if strings.Contains(stdout+stderr, "credential\"") {
		t.Fatal("the broken key was quoted")
	}
	// And the broken one is replaced like any other.
	if _, stderr, err := runAuth(t, strings.NewReader(authKey2+"\n"), "auth", "login", "meta"); err != nil || stderr != "" {
		t.Fatalf("replacing the broken key = %q, %v", stderr, err)
	}
}

// TestAuthPick: a menu answer is a number on the list, or an id or a name;
// anything else is refused without being quoted back.
func TestAuthPick(t *testing.T) {
	a := &authRun{name: "craze auth login"}
	infos := []modeltable.ProviderInfo{{ID: "fireworks", Name: "Fireworks"}, {ID: "zai-coding-plan", Name: "Z.AI Coding Plan"}}
	for answer, want := range map[string]string{"1": "fireworks", " 2 ": "zai-coding-plan", "z.ai coding plan": "zai-coding-plan", "FIREWORKS": "fireworks"} {
		if p, err := a.pick(infos, answer); err != nil || p.ID != want {
			t.Fatalf("pick %q = %v, %v; want %s", answer, p.ID, err, want)
		}
	}
	for _, answer := range []string{"0", "3", authKey, "-1"} {
		_, err := a.pick(infos, answer)
		wantExit(t, err, 2, "that is not a provider on the list; nothing was saved")
		if strings.Contains(err.Error(), answer) {
			t.Fatalf("the refusal quotes the answer: %v", err)
		}
	}
	_, err := a.pick(infos, "  ")
	wantExit(t, err, 1, "no provider chosen; nothing was saved")
}

// ptyAuth runs craze auth with argv on a terminal of its own: stdin and stderr
// (the prompts) are the pty, stdout a buffer. It returns what the terminal
// showed, stdout, and the command's error, and fails when the command left
// the terminal's echo off. drive is the user at the keyboard.
func ptyAuth(t *testing.T, drive func(tail *ptyTail, ptmx, tty *os.File), argv ...string) (screen, stdout string, err error) {
	t.Helper()
	ptmx, tty, perr := pty.Open()
	if perr != nil {
		t.Skipf("no pty: %v", perr)
	}
	defer func() { _ = ptmx.Close() }()
	defer func() { _ = tty.Close() }()
	tail := newPTYTail(ptmx)

	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(tty)
	cmd.SetOut(&out)
	cmd.SetErr(tty)
	cmd.SetArgs(argv)
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	drive(tail, ptmx, tty)
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("craze %q never finished; the terminal shows %q", argv, tail.text())
	}
	if tio, terr := unix.IoctlGetTermios(int(tty.Fd()), getTermios); terr != nil || tio.Lflag&unix.ECHO == 0 {
		t.Fatalf("craze %q left the terminal's echo off (%v)", argv, terr)
	}
	// Everything the command wrote — and anything the terminal echoed — is
	// on the master side before this sentinel, written after it finished.
	const sentinel = "--end-of-auth-test--"
	if _, werr := tty.WriteString(sentinel); werr != nil {
		t.Fatal(werr)
	}
	if !tail.wait(sentinel, 5*time.Second) {
		t.Fatalf("the terminal never showed the sentinel: %q", tail.text())
	}
	return tail.text(), out.String(), err
}

// typeKeyUnechoed waits for prompt, then for the terminal's echo to be off —
// the prompt is written just before the no-echo read begins — and types the
// key and Enter.
func typeKeyUnechoed(t *testing.T, tail *ptyTail, ptmx, tty *os.File, prompt, key string) {
	t.Helper()
	if !tail.wait(prompt, 10*time.Second) {
		t.Fatalf("never prompted %q; the terminal shows %q", prompt, tail.text())
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		tio, err := unix.IoctlGetTermios(int(tty.Fd()), getTermios)
		if err != nil {
			t.Fatal(err)
		}
		if tio.Lflag&unix.ECHO == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the terminal's echo never went off: the key would be shown as it is typed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := ptmx.WriteString(key + "\n"); err != nil {
		t.Fatal(err)
	}
}

// TestAuthLoginPromptDoesNotEcho (§3.7, CR 19): on a terminal the key is read
// from a prompt that does not echo it — the terminal never shows it — and
// the terminal's echo is back on afterwards.
func TestAuthLoginPromptDoesNotEcho(t *testing.T) {
	native := authNative(t)
	screen, stdout, err := ptyAuth(t, func(tail *ptyTail, ptmx, tty *os.File) {
		typeKeyUnechoed(t, tail, ptmx, tty, "Fireworks API key: ", authKey)
	}, "auth", "login", "fireworks")
	if err != nil {
		t.Fatalf("login on a terminal: %v (screen %q)", err, screen)
	}
	if strings.Contains(screen, authKey) || strings.Contains(screen, authKey[3:14]) {
		t.Fatalf("the terminal showed the key: %q", screen)
	}
	if !strings.HasPrefix(stdout, "Saved the Fireworks key in ") {
		t.Fatalf("stdout %q", stdout)
	}
	if storedKey(t, native, "fireworks") != authKey {
		t.Fatal("the key typed at the prompt was not stored")
	}
}

// TestAuthLoginMenuOnATerminal: with no provider named, a terminal gets a
// numbered menu by display name with the connected providers marked; the
// number picks the provider whose key is then asked for without echo.
func TestAuthLoginMenuOnATerminal(t *testing.T) {
	native := authNative(t)
	t.Setenv("FIREWORKS_API_KEY", authEnvK)
	screen, stdout, err := ptyAuth(t, func(tail *ptyTail, ptmx, tty *os.File) {
		if !tail.wait("Provider [1-4]: ", 10*time.Second) {
			t.Fatalf("no menu; the terminal shows %q", tail.text())
		}
		if _, err := ptmx.WriteString("3\n"); err != nil {
			t.Fatal(err)
		}
		typeKeyUnechoed(t, tail, ptmx, tty, "OpenRouter API key: ", authKey)
	}, "auth", "login")
	if err != nil {
		t.Fatalf("login from the menu: %v (screen %q)", err, screen)
	}
	menu := strings.ReplaceAll(screen, "\r\n", "\n")
	for _, line := range []string{
		"Connect a model provider:\n",
		"  1. Fireworks (connected)\n",
		"  2. Meta\n",
		"  3. OpenRouter\n",
		"  4. Z.AI Coding Plan\n",
	} {
		if !strings.Contains(menu, line) {
			t.Fatalf("the menu lacks %q:\n%s", line, menu)
		}
	}
	if strings.Contains(screen, authKey) || strings.Contains(screen, authEnvK) {
		t.Fatalf("the terminal showed a key: %q", screen)
	}
	if !strings.HasPrefix(stdout, "Saved the OpenRouter key in ") || storedKey(t, native, "openrouter") != authKey {
		t.Fatalf("stdout %q; the menu's choice was not the provider stored", stdout)
	}
}
