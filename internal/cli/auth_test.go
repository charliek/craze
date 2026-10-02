package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
// checks that none of them reaches stdout, stderr or an error (A7). A failure
// never prints one either (review r2): it says where a key was found, and
// shows what it prints with every key masked (maskKeys).

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

// authKeys are the keys a leak check looks for: each whole, and the part of
// it after "sk-" long enough to be a leak on its own.
var authKeys = []string{authKey, authKey2, authEnvK}

// keyAt is where the first of authKeys, or a leak-sized part of one, is in
// text; -1 when none is.
func keyAt(text string) int {
	at := -1
	for _, k := range authKeys {
		for _, frag := range []string{k, k[3:14]} {
			if i := strings.Index(text, frag); i >= 0 && (at < 0 || i < at) {
				at = i
			}
		}
	}
	return at
}

// maskKeys is text with every key of this file — each of authKeys, whole or
// in part, and the short one — replaced by <CANARY>: what a failure may print.
func maskKeys(text string) string {
	for _, k := range authKeys {
		text = strings.ReplaceAll(text, k, "<CANARY>")
	}
	for _, k := range authKeys {
		text = strings.ReplaceAll(text, k[3:14], "<CANARY>")
	}
	return strings.ReplaceAll(text, shortKey, "<CANARY>")
}

// noKeyIn fails the case when text — the stream where names it — holds a key,
// saying where and showing text masked.
func noKeyIn(t *testing.T, what, where, text string) {
	t.Helper()
	if at := keyAt(text); at >= 0 {
		t.Fatalf("%s: a key leaked into %s at byte %d; with every key masked it reads:\n%s", maskKeys(what), where, at, maskKeys(text))
	}
}

// runAuth runs craze with argv, stdin as given, over the current CRAZE_HOME,
// and returns its stdout, its stderr and its error, having failed the case
// if a key is in any of them.
func runAuth(t *testing.T, stdin io.Reader, argv ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errw bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(stdin)
	cmd.SetOut(&out)
	cmd.SetErr(&errw)
	cmd.SetArgs(argv)
	err = cmd.Execute()
	what := "craze " + strings.Join(argv, " ")
	noKeyIn(t, what, "stdout", out.String())
	noKeyIn(t, what, "stderr", errw.String())
	noKeyIn(t, what, "the error", errString(err))
	if err != nil {
		line, _ := diagnose(nil, err)
		noKeyIn(t, what, "the line craze prints for the error", line)
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
// It prints the message masked.
func wantExit(t *testing.T, err error, code int, want ...string) {
	t.Helper()
	got, msg := exitCode(t, err)
	if got != code {
		t.Fatalf("exit %d %q; want exit %d", got, maskKeys(msg), code)
	}
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Fatalf("message %q does not say %q", maskKeys(msg), w)
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
			[]string{"craze auth login: name a provider (chatgpt, fireworks, meta, openrouter, zai-coding-plan)", "only on a terminal"}},
		{"an unknown provider", authKey + "\n", []string{"auth", "login", "nosuch"}, 2,
			[]string{"craze auth login: no such provider; craze has chatgpt, fireworks, meta, openrouter, zai-coding-plan"}},
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
			[]string{"craze auth logout: name the provider whose stored key to remove (chatgpt, fireworks, meta, openrouter, zai-coding-plan)"}},
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
				t.Fatalf("the refusal quotes what was typed: %s", maskKeys(err.Error()))
			}
			if stdout != "" {
				t.Fatalf("a refused command printed %q", maskKeys(stdout))
			}
			if _, err := os.Stat(native); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused command made %s", native)
			}
		})
	}
}

// TestAuthSurplusArgumentsAndBadFlags (X29, review r2): an argument too many,
// or a flag the command does not take, is a usage error — exit 2, nothing
// read or written — that says what the command takes and never what it was
// given: cobra's own errors quote it, and a key pasted onto the command line
// is the likeliest one. runAuth checks the key is in no output, error or
// printed line.
func TestAuthSurplusArgumentsAndBadFlags(t *testing.T) {
	const (
		loginTakes  = "craze auth login: takes one argument at most, the provider"
		logoutTakes = "craze auth logout: takes one argument, the provider"
		listTakes   = "craze auth list: takes no arguments"
	)
	flagFor := func(cmd string) string {
		return cmd + ": unknown or malformed flag; see " + cmd + " --help"
	}
	for _, c := range []struct {
		name string
		argv []string
		want string
	}{
		{"list with a key", []string{"auth", "list", authKey}, listTakes},
		{"login with a provider and a key", []string{"auth", "login", "fireworks", authKey}, loginTakes},
		{"login with two keys", []string{"auth", "login", authKey, authKey2}, loginTakes},
		{"logout with a provider and a key", []string{"auth", "logout", "fireworks", authKey}, logoutTakes},
		{"logout with two keys", []string{"auth", "logout", authKey, authKey2}, logoutTakes},
		{"a key as a long flag", []string{"auth", "login", "--" + authKey, "fireworks"}, flagFor("craze auth login")},
		{"a key as short flags", []string{"auth", "list", "-" + authKey}, flagFor("craze auth list")},
		{"a key as a flag's value", []string{"auth", "logout", "--help=" + authKey, "fireworks"}, flagFor("craze auth logout")},
		{"a key as an unknown flag's value", []string{"auth", "login", "fireworks", "--key=" + authKey}, flagFor("craze auth login")},
		{"a key as a flag of the group", []string{"auth", "--" + authKey}, flagFor("craze auth")},
	} {
		t.Run(c.name, func(t *testing.T) {
			native := authNative(t)
			stdout, _, err := runAuth(t, strings.NewReader(authKey2+"\n"), c.argv...)
			wantExit(t, err, 2, c.want)
			if msg := err.Error(); msg != c.want {
				t.Fatalf("the refusal = %q; want exactly %q", maskKeys(msg), c.want)
			}
			if stdout != "" {
				t.Fatalf("a refused command printed %q", maskKeys(stdout))
			}
			if _, err := os.Stat(native); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused command made %s", native)
			}
		})
	}
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
	// The ChatGPT plan's row is its sign-in's (plan 033 §3.11): no key to
	// have, and nobody signed in here.
	wantRows := "" +
		"ChatGPT plan      chatgpt          not signed in\n" +
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
			t.Fatalf("the refusal quotes the answer: %s", maskKeys(err.Error()))
		}
	}
	_, err := a.pick(infos, "  ")
	wantExit(t, err, 1, "no provider chosen; nothing was saved")
}

// ptyAuth runs craze auth with argv on a terminal of its own: stdin and stderr
// (the prompts) are the pty, stdout a buffer. It returns what the terminal
// showed, stdout, and the command's error, and fails when the command left
// the terminal's echo off. drive is the user at the keyboard.
func ptyAuth(t *testing.T, drive func(tail *ptyTail, ptmx *os.File), argv ...string) (screen, stdout string, err error) {
	t.Helper()
	ptmx, tty, perr := pty.Open()
	if perr != nil {
		t.Skipf("no pty: %v", perr)
	}
	defer func() { _ = ptmx.Close() }()
	defer func() { _ = tty.Close() }()
	tail := newPTYTail(ptmx)
	if !echoing(t, tty) {
		t.Fatal("control: a new terminal's echo is off, so finding it on afterwards proves nothing")
	}

	var out bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(tty)
	cmd.SetOut(&out)
	cmd.SetErr(tty)
	cmd.SetArgs(argv)
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	drive(tail, ptmx)
	what := "craze " + strings.Join(argv, " ")
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never finished; the terminal shows (keys masked) %q", maskKeys(what), maskKeys(tail.text()))
	}
	if !echoing(t, tty) {
		t.Fatalf("%s left the terminal's echo off", maskKeys(what))
	}
	// Everything the command wrote — and anything the terminal echoed — is
	// on the master side before this sentinel, written after it finished.
	const sentinel = "--end-of-auth-test--"
	if _, werr := tty.WriteString(sentinel); werr != nil {
		t.Fatal(werr)
	}
	if !tail.wait(sentinel, 5*time.Second) {
		t.Fatalf("the terminal never showed the sentinel: %q", maskKeys(tail.text()))
	}
	screen = tail.text()
	noKeyIn(t, what, "the terminal", screen)
	noKeyIn(t, what, "stdout", out.String())
	noKeyIn(t, what, "the error", errString(err))
	return screen, out.String(), err
}

// echoing is whether the terminal tty echoes what is typed.
func echoing(t *testing.T, tty *os.File) bool {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(tty.Fd()), getTermios)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Lflag&unix.ECHO != 0
}

// seePrompt waits for prompt to show, looking every millisecond, so what the
// test types next comes as soon after the prompt as a person's typeahead or
// paste would.
func seePrompt(t *testing.T, tail *ptyTail, prompt string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(tail.text(), prompt) {
		if time.Now().After(deadline) {
			t.Fatalf("never prompted %q; the terminal shows %q", prompt, maskKeys(tail.text()))
		}
		time.Sleep(time.Millisecond)
	}
}

// typeAtPrompt types text and Enter the moment prompt shows (review r2): no
// waiting for the echo to go off first, which would hide a prompt drawn
// before it did. ptyAuth then finds no key on the screen.
func typeAtPrompt(t *testing.T, tail *ptyTail, ptmx *os.File, prompt, text string) {
	t.Helper()
	seePrompt(t, tail, prompt)
	if _, err := ptmx.WriteString(text + "\n"); err != nil {
		t.Fatal(err)
	}
}

// TestAuthLoginPromptDoesNotEcho (§3.7, CR 19, review r2): on a terminal the
// key is read from a prompt drawn with the echo already off — the key typed
// the moment the prompt shows is never displayed — and the terminal's echo
// is back on afterwards.
func TestAuthLoginPromptDoesNotEcho(t *testing.T) {
	native := authNative(t)
	screen, stdout, err := ptyAuth(t, func(tail *ptyTail, ptmx *os.File) {
		typeAtPrompt(t, tail, ptmx, "Fireworks API key: ", authKey)
	}, "auth", "login", "fireworks")
	if err != nil {
		t.Fatalf("login on a terminal: %v (screen %q)", err, maskKeys(screen))
	}
	if !strings.HasPrefix(stdout, "Saved the Fireworks key in ") {
		t.Fatalf("stdout %q", maskKeys(stdout))
	}
	if storedKey(t, native, "fireworks") != authKey {
		t.Fatal("the key typed at the prompt was not stored")
	}
}

// TestAuthLoginMenuOnATerminal: with no provider named, a terminal gets a
// numbered menu by display name with the connected providers marked; the
// number picks the provider whose key is then asked for without echo. The
// answer is read without echo too, and the number written back after the
// prompt, so the menu reads as if it had been echoed.
func TestAuthLoginMenuOnATerminal(t *testing.T) {
	native := authNative(t)
	t.Setenv("FIREWORKS_API_KEY", authEnvK)
	screen, stdout, err := ptyAuth(t, func(tail *ptyTail, ptmx *os.File) {
		typeAtPrompt(t, tail, ptmx, "Provider [1-5]: ", "4")
		typeAtPrompt(t, tail, ptmx, "OpenRouter API key: ", authKey)
	}, "auth", "login")
	if err != nil {
		t.Fatalf("login from the menu: %v (screen %q)", err, maskKeys(screen))
	}
	menu := strings.ReplaceAll(screen, "\r\n", "\n")
	for _, line := range []string{
		"Connect a model provider:\n",
		"  1. ChatGPT plan\n",
		"  2. Fireworks (connected)\n",
		"  3. Meta\n",
		"  4. OpenRouter\n",
		"  5. Z.AI Coding Plan\n",
		"Provider [1-5]: 4\nOpenRouter API key: \n",
	} {
		if !strings.Contains(menu, line) {
			t.Fatalf("the menu lacks %q:\n%s", line, maskKeys(menu))
		}
	}
	if !strings.HasPrefix(stdout, "Saved the OpenRouter key in ") || storedKey(t, native, "openrouter") != authKey {
		t.Fatalf("stdout %q; the menu's choice was not the provider stored", maskKeys(stdout))
	}
}

// TestAuthLoginMenuHidesAPastedKey (X29, review r2): a key pasted at the
// menu's prompt instead of a number — the moment it shows — is never on the
// screen: the answer is read without echo and, not being a number on the
// list, neither written back nor quoted in the refusal. Nothing is saved.
func TestAuthLoginMenuHidesAPastedKey(t *testing.T) {
	native := authNative(t)
	screen, stdout, err := ptyAuth(t, func(tail *ptyTail, ptmx *os.File) {
		typeAtPrompt(t, tail, ptmx, "Provider [1-5]: ", authKey)
	}, "auth", "login")
	wantExit(t, err, 2, "craze auth login: that is not a provider on the list; nothing was saved")
	if got := strings.ReplaceAll(screen, "\r\n", "\n"); !strings.Contains(got, "Provider [1-5]: \n") {
		t.Fatalf("the prompt's line = %q; want it to end with the Enter alone", maskKeys(got))
	}
	if stdout != "" {
		t.Fatalf("stdout %q", maskKeys(stdout))
	}
	if _, err := os.Stat(native); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused login made %s", native)
	}
}

// authEchoRaceEnv makes the test binary's craze child hold the terminal's
// seams in the order TestAuthSignalRacesTheEchoOff forces (childEchoRace),
// read by the child's init in serve_child_test.go.
const authEchoRaceEnv = "CRAZE_CLI_TEST_ECHO_RACE"

// childEchoRace sets the terminal's seams in the craze child so a signal
// lands right as login starts to turn the echo off: quiet, its handler in
// place, signals the process and waits until the handler has put the
// terminal back; the handler, before it exits, gives quiet up to
// echoRaceWait to turn the echo off after it. A quiet that did not wait for
// the handler would turn it off in that time — and the process would exit
// with it off.
func childEchoRace() {
	restored := make(chan struct{})
	muted := make(chan struct{})
	authQuieting = func() {
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		<-restored
	}
	authRestored = func() {
		close(restored)
		select {
		case <-muted:
		case <-time.After(echoRaceWait):
		}
	}
	authQuieted = func() { close(muted) }
}

// echoRaceWait is how long the race's handler waits for a quiet that should
// never come.
const echoRaceWait = 300 * time.Millisecond

// authChild starts `craze <argv...>` as the test binary's craze child
// (serve_child_test.go) on a terminal of its own — stdin and stderr the pty,
// stdout discarded — with env added to the test's environment. It returns
// the child, the screen, and the pty's slave side, where the test reads the
// terminal's settings once the child is gone.
func authChild(t *testing.T, env []string, argv ...string) (cmd *exec.Cmd, tail *ptyTail, tty *os.File) {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	t.Cleanup(func() { _ = ptmx.Close(); _ = tty.Close() })
	tail = newPTYTail(ptmx)
	if !echoing(t, tty) {
		t.Fatal("control: a new terminal's echo is off, so finding it on afterwards proves nothing")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(exe, "-test.run=^$")
	cmd.Env = append(childEnv(b), env...)
	cmd.Stdin, cmd.Stderr = tty, tty
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, tail, tty
}

// authChildExit is the child's exit code, within 10 seconds.
func authChildExit(t *testing.T, cmd *exec.Cmd, tail *ptyTail) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if err == nil {
			return 0
		}
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				t.Fatalf("the child was ended by %v, not by its handler; the terminal shows %q", ws.Signal(), maskKeys(tail.text()))
			}
			return ee.ExitCode()
		}
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatalf("the child never exited; the terminal shows %q", maskKeys(tail.text()))
	}
	return -1
}

// TestAuthSignalAtThePromptRestoresTheEcho (X29): a signal that ends craze
// auth login while it waits at the key's prompt, the echo off, puts the echo
// back on before craze exits as the signal asked — 128 plus its number —
// having saved nothing. craze runs in a process of its own, so the signal and
// the exit are real.
func TestAuthSignalAtThePromptRestoresTheEcho(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			native := authNative(t)
			cmd, tail, tty := authChild(t, nil, "auth", "login", "fireworks")
			seePrompt(t, tail, "Fireworks API key: ")
			if echoing(t, tty) {
				t.Fatal("control: the echo is on at the prompt, so finding it on afterwards proves nothing")
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if code := authChildExit(t, cmd, tail); code != 128+int(sig) {
				t.Fatalf("exit %d; want %d", code, 128+int(sig))
			}
			if !echoing(t, tty) {
				t.Fatal("craze exited on the signal with the terminal's echo off")
			}
			if _, err := os.Stat(native); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("an interrupted login made %s", native)
			}
		})
	}
}

// TestAuthSignalRacesTheEchoOff (review r2): the signal arrives right as
// login starts to turn the echo off — after the handler is in place, before
// the echo goes — and the handler puts the terminal back first. Nothing may
// turn the echo off after that: craze exits 130 with the echo on. The order
// is forced with the terminal's seams (childEchoRace); without the lock that
// quiet and the handler share, quiet turns the echo off while the handler
// waits, and craze exits with it off.
func TestAuthSignalRacesTheEchoOff(t *testing.T) {
	native := authNative(t)
	cmd, tail, tty := authChild(t, []string{authEchoRaceEnv + "=1"}, "auth", "login", "fireworks")
	if code := authChildExit(t, cmd, tail); code != 130 {
		t.Fatalf("exit %d; want 130 (the terminal shows %q)", code, maskKeys(tail.text()))
	}
	if !echoing(t, tty) {
		t.Fatal("the echo was turned off after the signal's handler put the terminal back: craze exited with it off")
	}
	if strings.Contains(tail.text(), "API key:") {
		t.Fatalf("the prompt was drawn after the handler ran: %q", maskKeys(tail.text()))
	}
	if _, err := os.Stat(native); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an interrupted login made %s", native)
	}
}
