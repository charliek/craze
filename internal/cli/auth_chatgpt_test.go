package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// craze auth for the ChatGPT plan (plan 033 §3.13). These tests drive the
// CLI's own work — the waits, the texts, the browser rule, Ctrl-C — through
// auth_chatgpt.go's seams, with a stand-in for chatgptauth's Attempt
// (fakeSignIn) that keeps its documented contract: a redirect is accepted
// once, pasted or from the listener; anything else pasted is
// ErrRedirectMismatch; a cancelled Wait closes the attempt. The sign-in's
// real wire — PKCE, state, the id_token over JWKS — is chatgptauth's own
// tests' and tests/cli/test_auth.py's, against fake issuers. No test here
// opens a browser or reaches OpenAI (TestMain fences the endpoints anyway).

const (
	signInURL      = "https://auth.openai.com/api/accounts/authorize?client_id=dynamic_agent_client&state=stub-state"
	signInRedirect = "http://127.0.0.1:1455/auth/callback"
	signInGood     = signInRedirect + "?code=stub-code&state=stub-state&client_id=oaiapp_stub"
	signInEmail    = "person@example.test"
)

// fakeSignIn stands in for the sign-in's chatgptauth calls; useFakeSignIn
// installs it. Its knobs are set before the command runs; its records are
// read after, or under mu.
type fakeSignIn struct {
	// Knobs.
	listening bool
	result    chatgptauth.Result
	waitErr   error // Wait's error once the redirect is in
	beginErr  error
	models    *chatgptauth.Models
	fetchErr  error
	// fetchBlocks makes the model fetch wait for its context's end, after
	// saying on waiting that it started.
	fetchBlocks bool
	openErr     error
	markErr     error
	logout      chatgptauth.LogoutResult
	logoutErr   error
	// pasteHold, when set, keeps an accepted Paste from returning until it is
	// closed, and waitHold a Wait whose redirect is in: a case orders the
	// sign-in's loop with them (TestAuthLoginChatGPTConfirmsAPasteEitherWay).
	pasteHold chan struct{}
	waitHold  chan struct{}

	mu       sync.Mutex
	opts     chatgptauth.BeginOptions
	pastes   []string
	over     bool
	closed   bool
	opened   []string
	marked   int
	fetched  int
	logouts  int
	sigs     chan<- os.Signal
	got      chan struct{} // closed once a redirect is accepted
	closedCh chan struct{}
	// waiting is signalled when Wait starts; listened, each time Listening
	// is asked (introduceSignIn asks once, the stdin reader at its end).
	waiting  chan struct{}
	listened chan struct{}
}

func useFakeSignIn(t *testing.T) *fakeSignIn {
	t.Helper()
	f := &fakeSignIn{
		result:   chatgptauth.Result{Email: signInEmail, PlanUsage: true, Registered: true, ShowNotice: true},
		models:   &chatgptauth.Models{Models: []chatgptauth.Model{{Slug: "gpt-6-astra"}, {Slug: "gpt-5.6-sol"}}},
		got:      make(chan struct{}),
		closedCh: make(chan struct{}),
		waiting:  make(chan struct{}, 1),
		listened: make(chan struct{}, 4),
	}
	saved := []func(){}
	swap := func(restore func()) { saved = append(saved, restore) }
	b, fm, mn, so, ob, g, ns, ss := beginSignIn, fetchPlanModels, markNoticeShown, signOutChatGPT, openBrowser, browserGOOS, notifySignInSignals, stopSignInSignals
	swap(func() {
		beginSignIn, fetchPlanModels, markNoticeShown, signOutChatGPT, openBrowser, browserGOOS, notifySignInSignals, stopSignInSignals = b, fm, mn, so, ob, g, ns, ss
	})
	t.Cleanup(func() {
		for _, r := range saved {
			r()
		}
	})
	beginSignIn = func(_ context.Context, dir string, opts chatgptauth.BeginOptions) (signInAttempt, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.beginErr != nil {
			return nil, f.beginErr
		}
		f.opts = opts
		if opts.PasteOnly {
			f.listening = false
		}
		return f, nil
	}
	fetchPlanModels = func(ctx context.Context, _ string) (*chatgptauth.Models, error) {
		f.mu.Lock()
		f.fetched++
		block, models, err := f.fetchBlocks, f.models, f.fetchErr
		f.mu.Unlock()
		if block {
			f.waiting <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return models, err
	}
	markNoticeShown = func(context.Context, string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.marked++
		return f.markErr
	}
	signOutChatGPT = func(context.Context, string) (chatgptauth.LogoutResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.logouts++
		return f.logout, f.logoutErr
	}
	openBrowser = func(u string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.opened = append(f.opened, u)
		return f.openErr
	}
	// Linux with no desktop, unless a case says otherwise.
	browserGOOS = "linux"
	for _, v := range []string{"DISPLAY", "WAYLAND_DISPLAY", "SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		t.Setenv(v, "")
	}
	notifySignInSignals = func(c chan<- os.Signal) {
		f.mu.Lock()
		f.sigs = c
		f.mu.Unlock()
	}
	stopSignInSignals = func(chan<- os.Signal) {}
	return f
}

func (f *fakeSignIn) URL() string         { return signInURL }
func (f *fakeSignIn) RedirectURI() string { return signInRedirect }

func (f *fakeSignIn) Listening() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case f.listened <- struct{}{}:
	default:
	}
	return f.listening
}

func (f *fakeSignIn) Paste(raw string) error {
	f.mu.Lock()
	f.pastes = append(f.pastes, raw)
	switch {
	case f.over:
		f.mu.Unlock()
		return chatgptauth.ErrAttemptOver
	case strings.TrimSpace(raw) != signInGood:
		f.mu.Unlock()
		return chatgptauth.ErrRedirectMismatch
	}
	f.over = true
	close(f.got)
	hold := f.pasteHold
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	return nil
}

// arrive is the browser's redirect reaching the listener.
func (f *fakeSignIn) arrive() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.over {
		f.over = true
		close(f.got)
	}
}

func (f *fakeSignIn) Wait(ctx context.Context) (chatgptauth.Result, error) {
	f.waiting <- struct{}{}
	select {
	case <-f.got:
		if f.waitHold != nil {
			<-f.waitHold
		}
		return f.result, f.waitErr
	case <-ctx.Done():
		f.Close()
		return chatgptauth.Result{}, ctx.Err()
	case <-f.closedCh:
		return chatgptauth.Result{}, chatgptauth.ErrAttemptOver
	}
}

func (f *fakeSignIn) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		f.over = true
		close(f.closedCh)
	}
}

// signal sends sig to the sign-in as the OS would, once it is waiting.
func (f *fakeSignIn) signal(t *testing.T, sig os.Signal) {
	t.Helper()
	select {
	case <-f.waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("the sign-in never started waiting")
	}
	f.mu.Lock()
	c := f.sigs
	f.mu.Unlock()
	if c == nil {
		t.Fatal("the sign-in registered for no signals")
	}
	c <- sig
}

// runSignIn runs craze with argv over stdin in a goroutine, so a case can act
// while it waits, and answers its result once it ends.
func runSignIn(t *testing.T, stdin io.Reader, argv ...string) func() (stdout, stderr string, err error) {
	t.Helper()
	type out struct {
		stdout, stderr string
		err            error
	}
	done := make(chan out, 1)
	go func() {
		o, e, err := runAuthQuiet(stdin, argv...)
		done <- out{o, e, err}
	}()
	return func() (string, string, error) {
		select {
		case o := <-done:
			what := "craze " + strings.Join(argv, " ")
			noKeyIn(t, what, "stdout", o.stdout)
			noKeyIn(t, what, "stderr", o.stderr)
			noKeyIn(t, what, "the error", errString(o.err))
			return o.stdout, o.stderr, o.err
		case <-time.After(10 * time.Second):
			t.Fatal("craze auth never finished")
		}
		return "", "", nil
	}
}

// runAuthQuiet is runAuth for a goroutine that is not the test's: it
// returns, and the caller checks for keys.
func runAuthQuiet(stdin io.Reader, argv ...string) (string, string, error) {
	var out, errw lockedBuffer
	cmd := NewRootCmd()
	cmd.SetIn(stdin)
	cmd.SetOut(&out)
	cmd.SetErr(&errw)
	cmd.SetArgs(argv)
	err := cmd.Execute()
	return out.String(), errw.String(), err
}

// The texts a whole sign-in prints on stdout, in order.
const (
	signedInLine = "Signed in to ChatGPT as " + signInEmail + ".\n"
	noticeLines  = "You're using your ChatGPT plan.\n" +
		"Eligible usage in this app uses your ChatGPT plan. Manage usage in your ChatGPT settings: https://chatgpt.com/settings/usage\n"
	modelsLine = "ChatGPT plan models: chatgpt/gpt-6-astra, chatgpt/gpt-5.6-sol\n"
)

// TestGUISession: a browser is opened only where the person at the terminal
// would see it — a Linux desktop (X or Wayland) or macOS — and never over
// SSH, whichever of sshd's variables says so; never anywhere else.
func TestGUISession(t *testing.T) {
	for _, c := range []struct {
		goos string
		env  map[string]string
		want bool
	}{
		{"linux", map[string]string{"DISPLAY": ":0"}, true},
		{"linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, true},
		{"linux", map[string]string{}, false},
		{"linux", map[string]string{"DISPLAY": ":0", "SSH_CONNECTION": "10.0.0.2 5555 10.0.0.1 22"}, false},
		{"linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0", "SSH_CLIENT": "10.0.0.2 5555 22"}, false},
		{"linux", map[string]string{"DISPLAY": ":0", "SSH_TTY": "/dev/pts/3"}, false},
		{"darwin", map[string]string{}, true},
		{"darwin", map[string]string{"SSH_CONNECTION": "10.0.0.2 5555 10.0.0.1 22"}, false},
		{"freebsd", map[string]string{"DISPLAY": ":0"}, false},
		{"windows", map[string]string{}, false},
	} {
		if got := guiSession(c.goos, func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("guiSession(%s, %v) = %v; want %v", c.goos, c.env, got, c.want)
		}
	}
}

// TestAuthLoginChatGPTPasted: with stdin not a terminal, `craze auth login
// chatgpt --no-browser` is a paste-only sign-in: it prints the address and
// what to paste, refuses — without quoting — a line that is not an address
// (a key, the likeliest mistake, never reaches the attempt) and an address
// that is not this sign-in's, takes the right one, and prints the account,
// the one-time notice (recorded as shown) and the plan's models, fetched.
func TestAuthLoginChatGPTPasted(t *testing.T) {
	native := authNative(t)
	f := useFakeSignIn(t)
	wrong := signInRedirect + "?code=stub-code&state=another-attempt"
	stdin := strings.NewReader(authKey + "\n\n" + wrong + "\n" + signInGood + "\n")
	stdout, stderr, err := runSignIn(t, stdin, "auth", "login", "chatgpt", "--no-browser")()
	if err != nil {
		t.Fatalf("login: %v\nstderr: %s", err, maskKeys(stderr))
	}
	if want := signedInLine + noticeLines + modelsLine; stdout != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", stdout, want)
	}
	wantErr := "Sign in with ChatGPT to use your ChatGPT plan in craze. Open this address in a browser and approve craze:\n\n" +
		"  " + signInURL + "\n\n" +
		"After you approve, the browser goes to an address starting with " + signInRedirect + ", which will not load: copy its whole address from the address bar and paste it here.\n" +
		"That is not an address: the ChatGPT plan is funded by signing in, never by an API key. Paste the whole address the browser was sent to; it starts with " + signInRedirect + ".\n" +
		"That is not this sign-in's redirect address. Paste the whole address the browser was sent to; it starts with " + signInRedirect + ".\n"
	if stderr != wantErr {
		t.Fatalf("stderr:\n%s\nwant:\n%s", maskKeys(stderr), wantErr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.opts.PasteOnly {
		t.Fatal("--no-browser did not make the sign-in paste-only")
	}
	if len(f.pastes) != 2 || f.pastes[0] != wrong || f.pastes[1] != signInGood {
		t.Fatalf("the attempt was handed %d lines; want the two addresses alone (no key, no blank line)", len(f.pastes))
	}
	if f.marked != 1 || f.fetched != 1 {
		t.Fatalf("notice recorded %d times, models fetched %d times; want 1 and 1", f.marked, f.fetched)
	}
	if len(f.opened) != 0 {
		t.Fatal("a browser was opened")
	}
	if !f.closed {
		t.Fatal("the attempt was not closed")
	}
	if _, err := os.Stat(filepath.Join(native, modeltable.ProvidersFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the sign-in wrote providers.toml")
	}
}

// TestAuthLoginChatGPTNoticeOnce: a sign-in whose registration has already
// shown the notice (Result.ShowNotice false) prints no notice and records
// nothing.
func TestAuthLoginChatGPTNoticeOnce(t *testing.T) {
	authNative(t)
	f := useFakeSignIn(t)
	f.result.ShowNotice = false
	stdout, _, err := runSignIn(t, strings.NewReader(signInGood+"\n"), "auth", "login", "chatgpt")()
	if err != nil || stdout != signedInLine+modelsLine {
		t.Fatalf("login = %q, %v; want the account and the models alone", stdout, err)
	}
	if f.marked != 0 {
		t.Fatal("the notice was recorded as shown though it was not shown")
	}
}

// TestAuthLoginChatGPTKeyIsRefused (plan 033 §3.13, X134): a key piped into
// `craze auth login chatgpt` is never stored, asked for, handed to the
// sign-in or repeated: the line is refused as not an address, saying the plan
// takes no key, and a paste-only sign-in whose stdin then ends exits 1.
func TestAuthLoginChatGPTKeyIsRefused(t *testing.T) {
	native := authNative(t)
	f := useFakeSignIn(t)
	stdout, stderr, err := runSignIn(t, strings.NewReader(authKey+"\n"), "auth", "login", "chatgpt", "--no-browser")()
	wantExit(t, err, 1, "craze auth login: stdin ended before the redirect address was pasted; nothing was changed")
	if stdout != "" {
		t.Fatalf("stdout %q", maskKeys(stdout))
	}
	if !strings.Contains(stderr, "the ChatGPT plan is funded by signing in, never by an API key") {
		t.Fatalf("stderr does not say the plan takes no key:\n%s", maskKeys(stderr))
	}
	if strings.Contains(stderr, "API key: ") {
		t.Fatal("a key was asked for")
	}
	if len(f.pastes) != 0 {
		t.Fatal("the key reached the sign-in attempt")
	}
	if !f.closed {
		t.Fatal("the attempt was not closed")
	}
	if storedKey(t, native, "chatgpt") != "" {
		t.Fatal("the key was stored")
	}
}

// TestAuthLoginChatGPTListener: with a listener, the end of stdin is not the
// end of the sign-in — the browser's redirect can still come — and it
// finishes when the redirect reaches the listener. The negative control is
// the paste-only case's: the same end of stdin cancels it.
func TestAuthLoginChatGPTListener(t *testing.T) {
	authNative(t)
	f := useFakeSignIn(t)
	f.listening = true
	finish := runSignIn(t, strings.NewReader(""), "auth", "login", "chatgpt")
	// Listening is asked by the intro, then by the reader at stdin's end.
	for range 2 {
		select {
		case <-f.listened:
		case <-time.After(10 * time.Second):
			t.Fatal("the stdin reader never reached stdin's end")
		}
	}
	f.arrive()
	stdout, stderr, err := finish()
	if err != nil || stdout != signedInLine+noticeLines+modelsLine {
		t.Fatalf("login = %q, %v (stderr %q)", stdout, err, stderr)
	}
	want := "craze is waiting for the browser to come back to " + signInRedirect + ".\n" +
		"If the browser is on another machine, that page will not load there: copy its whole address from the address bar and paste it here.\n"
	if !strings.HasSuffix(stderr, want) {
		t.Fatalf("stderr:\n%s\nwant it to end:\n%s", stderr, want)
	}
	if f.opts.PasteOnly {
		t.Fatal("a sign-in without --no-browser was paste-only")
	}
}

// TestAuthLoginChatGPTEndOfStdinPasteOnly: a paste-only sign-in whose stdin
// ends with nothing accepted exits 1, its attempt closed.
func TestAuthLoginChatGPTEndOfStdinPasteOnly(t *testing.T) {
	authNative(t)
	f := useFakeSignIn(t)
	stdout, _, err := runSignIn(t, strings.NewReader("\n"), "auth", "login", "chatgpt", "--no-browser")()
	wantExit(t, err, 1, "stdin ended before the redirect address was pasted; nothing was changed")
	if stdout != "" || !f.closed {
		t.Fatalf("stdout %q, closed %v", stdout, f.closed)
	}
}

// TestAuthLoginChatGPTSignalCancels: Ctrl-C (or SIGTERM, SIGHUP) while the
// sign-in waits cancels it — the attempt, and so its listener, closed — and
// craze exits as the signal asked, 128 plus its number, having printed and
// fetched nothing.
func TestAuthLoginChatGPTSignalCancels(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			authNative(t)
			f := useFakeSignIn(t)
			f.listening = true
			stdinR, stdinW := io.Pipe()
			t.Cleanup(func() { _ = stdinW.Close() })
			finish := runSignIn(t, stdinR, "auth", "login", "chatgpt")
			f.signal(t, sig)
			stdout, _, err := finish()
			wantExit(t, err, 128+int(sig), "craze auth login: the sign-in was cancelled; nothing was changed")
			if stdout != "" || f.fetched != 0 {
				t.Fatalf("stdout %q, models fetched %d times", stdout, f.fetched)
			}
			if !f.closed {
				t.Fatal("the attempt — and its listener — was not closed")
			}
		})
	}
}

// TestAuthLoginChatGPTFailures: the browser's refusal, and any other
// chatgptauth error (without its package prefix), are exit 1; nothing is
// printed on stdout.
func TestAuthLoginChatGPTFailures(t *testing.T) {
	for _, c := range []struct {
		name     string
		beginErr error
		waitErr  error
		want     string
	}{
		{"declined", nil, chatgptauth.ErrAccessDenied, "craze auth login: the sign-in was declined in the browser; nothing was changed"},
		{"exchange refused", nil, &chatgptauth.OAuthError{Step: "exchange", Status: 400, Code: "invalid_grant"}, "craze auth login: exchange refused (HTTP 400, invalid_grant)"},
		{"no host id", errors.New("chatgptauth: /x/auth/host-id does not hold a urn:uuid host id; remove it to make a new one"), nil, "craze auth login: /x/auth/host-id does not hold a urn:uuid host id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			authNative(t)
			f := useFakeSignIn(t)
			f.beginErr, f.waitErr = c.beginErr, c.waitErr
			stdout, _, err := runSignIn(t, strings.NewReader(signInGood+"\n"), "auth", "login", "chatgpt")()
			wantExit(t, err, 1, c.want)
			if stdout != "" || f.fetched != 0 {
				t.Fatalf("stdout %q, models fetched %d times", stdout, f.fetched)
			}
		})
	}
}

// TestAuthLoginChatGPTPlanUsageOff (plan 033 §3.10 step 4): an account that
// signed in without granting plan usage is told the plan's models cannot be
// used and how to turn usage on; no notice, no model list.
func TestAuthLoginChatGPTPlanUsageOff(t *testing.T) {
	authNative(t)
	f := useFakeSignIn(t)
	f.result = chatgptauth.Result{Email: signInEmail, Registered: true}
	stdout, _, err := runSignIn(t, strings.NewReader(signInGood+"\n"), "auth", "login", "chatgpt")()
	want := "Signed in to ChatGPT as " + signInEmail + ", but ChatGPT plan usage is off: the account did not allow craze to use its ChatGPT plan, so the plan's models cannot be used.\n" +
		"To turn it on, run craze auth login chatgpt again and allow ChatGPT plan usage when ChatGPT asks.\n"
	if err != nil || stdout != want {
		t.Fatalf("login = %q, %v\nwant %q", stdout, err, want)
	}
	if f.fetched != 0 || f.marked != 0 {
		t.Fatalf("models fetched %d times, notice recorded %d times; want neither", f.fetched, f.marked)
	}
}

// TestAuthLoginChatGPTNotes: the notice's record and the model list failing
// are notes; the sign-in stands, exit 0.
func TestAuthLoginChatGPTNotes(t *testing.T) {
	authNative(t)
	f := useFakeSignIn(t)
	f.markErr = errors.New("chatgptauth: taking the sign-in lock: busy")
	f.fetchErr = &chatgptauth.OAuthError{Step: "models", Status: 500}
	stdout, stderr, err := runSignIn(t, strings.NewReader(signInGood+"\n"), "auth", "login", "chatgpt")()
	if err != nil || stdout != signedInLine+noticeLines {
		t.Fatalf("login = %q, %v", stdout, err)
	}
	for _, want := range []string{
		"note: the notice above could not be recorded as shown, so it may be shown again: taking the sign-in lock: busy\n",
		"note: the plan's model list could not be fetched (models refused (HTTP 500)); a native session fetches it when it opens\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr:\n%s\nwant it to hold %q", stderr, want)
		}
	}
}

// TestAuthLoginChatGPTBrowser (plan 033 §3.13): the browser is opened, with
// the authorization URL, only in a desktop session and without --no-browser;
// a browser that cannot be opened is a line saying so, and the sign-in goes
// on.
func TestAuthLoginChatGPTBrowser(t *testing.T) {
	for _, c := range []struct {
		name    string
		goos    string
		env     map[string]string
		argv    []string
		openErr error
		opened  bool
		says    string
	}{
		{"linux desktop", "linux", map[string]string{"DISPLAY": ":0"}, nil, nil, true, "craze opened it in your browser.\n"},
		{"macOS", "darwin", nil, nil, nil, true, "craze opened it in your browser.\n"},
		{"no-browser", "linux", map[string]string{"DISPLAY": ":0"}, []string{"--no-browser"}, nil, false, ""},
		{"over ssh", "darwin", map[string]string{"SSH_CONNECTION": "10.0.0.2 5555 10.0.0.1 22"}, nil, nil, false, ""},
		{"no desktop", "linux", nil, nil, nil, false, ""},
		{"opener fails", "linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, nil, errors.New("exec: \"xdg-open\": executable file not found in $PATH"), true,
			"craze could not open a browser (exec: \"xdg-open\": executable file not found in $PATH); open the address yourself.\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			authNative(t)
			f := useFakeSignIn(t)
			browserGOOS, f.openErr = c.goos, c.openErr
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			argv := append([]string{"auth", "login", "chatgpt"}, c.argv...)
			_, stderr, err := runSignIn(t, strings.NewReader(signInGood+"\n"), argv...)()
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			if c.opened {
				want = []string{signInURL}
			}
			if !slices.Equal(f.opened, want) {
				t.Fatalf("opened %q; want %q", f.opened, want)
			}
			said := strings.Contains(stderr, "craze opened it in your browser.") || strings.Contains(stderr, "craze could not open a browser")
			if c.says == "" && said || c.says != "" && !strings.Contains(stderr, c.says) {
				t.Fatalf("stderr:\n%s\nwant it to say %q about the browser", stderr, c.says)
			}
		})
	}
}

// TestAuthLoginChatGPTOnATerminal (plan 033 §3.13 as review r14 1 amends
// it): on a terminal the redirect is read at a prompt drawn with the echo
// already off, as a key's is, so the address pasted the moment it shows is not
// displayed; the sign-in confirms it by its origin and path alone — never its
// query, the one-time code — and finishes with it. A blank Enter before it,
// which the terminal did not echo either, draws the prompt again on a line of
// its own. ptyAuth finds the echo back on after. The control for the query's
// absence is the authorization URL above the prompt, whose query the screen
// does show.
func TestAuthLoginChatGPTOnATerminal(t *testing.T) {
	authNative(t)
	useFakeSignIn(t)
	screen, stdout, err := ptyAuth(t, func(tail *ptyTail, ptmx *os.File) {
		typeAtPrompt(t, tail, ptmx, redirectPrompt, "")
		typeAtPrompt(t, tail, ptmx, redirectPrompt+"\r\n"+redirectPrompt, signInGood)
	}, "auth", "login", "chatgpt")
	if err != nil {
		t.Fatalf("login on a terminal: %v (screen %q)", err, maskKeys(screen))
	}
	got := strings.ReplaceAll(screen, "\r\n", "\n")
	if !strings.Contains(got, redirectPrompt+"\n"+redirectPrompt+"\n"+receivedText(signInRedirect)+"\n") {
		t.Fatalf("the blank line's prompt, then the accepted address confirmed by its origin and path, are not on the screen:\n%s", got)
	}
	if !strings.Contains(got, signInURL) {
		t.Fatalf("control: the authorization URL's query is not on the screen:\n%s", got)
	}
	if strings.Contains(got, "stub-code") {
		t.Fatalf("the pasted address's query is on the screen:\n%s", got)
	}
	if stdout != signedInLine+noticeLines+modelsLine {
		t.Fatalf("stdout %q", stdout)
	}
}

// TestAuthLoginChatGPTConfirmsAPasteEitherWay: an accepted paste is
// confirmed on the terminal whichever the sign-in's loop reads first — the
// reader's event, or the result of the wait the paste finished. The orders are
// forced: a held Wait keeps the result back until the confirmation is on the
// screen; a held Paste keeps the event back until the loop has taken the
// result — the prompt's line ended — which is the order a confirmation carried
// by the event alone loses (the -race run's failure: it confirmed nothing).
// Each order's screen is the other's control.
func TestAuthLoginChatGPTConfirmsAPasteEitherWay(t *testing.T) {
	confirmed := redirectPrompt + "\n" + receivedText(signInRedirect) + "\n"
	for _, first := range []string{"the event", "the result"} {
		t.Run(first, func(t *testing.T) {
			authNative(t)
			f := useFakeSignIn(t)
			hold := make(chan struct{})
			if first == "the event" {
				f.waitHold = hold
			} else {
				f.pasteHold = hold
			}
			screen, stdout, err := ptyAuth(t, func(tail *ptyTail, ptmx *os.File) {
				typeAtPrompt(t, tail, ptmx, redirectPrompt, signInGood)
				if first == "the event" {
					seePrompt(t, tail, receivedText(signInRedirect))
				} else {
					seePrompt(t, tail, redirectPrompt+"\r\n")
				}
				close(hold)
			}, "auth", "login", "chatgpt")
			if err != nil {
				t.Fatalf("login on a terminal: %v (screen %q)", err, maskKeys(screen))
			}
			got := strings.ReplaceAll(screen, "\r\n", "\n")
			if !strings.Contains(got, confirmed) || strings.Count(got, receivedText(signInRedirect)) != 1 {
				t.Fatalf("the accepted paste is not confirmed once:\n%s", got)
			}
			if stdout != signedInLine+noticeLines+modelsLine {
				t.Fatalf("stdout %q", stdout)
			}
		})
	}
}

// TestAuthLoginChatGPTRefusesWithTheEchoOn: a terminal whose echo the
// sign-in cannot turn off is no place to paste — a key pasted there would
// show — so the sign-in ends, exit 1, before it begins: no address printed,
// no prompt, nothing changed. The control is the same terminal with the echo
// turned off, which begins and prints the address.
func TestAuthLoginChatGPTRefusesWithTheEchoOn(t *testing.T) {
	authNative(t)
	f := useFakeSignIn(t)
	prev := muteSignIn
	t.Cleanup(func() { muteSignIn = prev })
	muteSignIn = func(*os.File) (echoRestorer, error) { return nil, errors.New("no termios here") }
	screen, stdout, err := ptyAuth(t, func(*ptyTail, *os.File) {}, "auth", "login", "chatgpt", "--no-browser")
	wantExit(t, err, 1, "craze auth login: turning the terminal's echo off: no termios here; nothing was changed")
	if strings.Contains(screen, signInURL) || strings.Contains(screen, redirectPrompt) || stdout != "" || f.opts.PasteOnly {
		t.Fatalf("a sign-in began with the echo on (screen %q)", screen)
	}

	muteSignIn = prev
	screen, _, err = ptyAuth(t, func(tail *ptyTail, ptmx *os.File) {
		typeAtPrompt(t, tail, ptmx, redirectPrompt, signInGood)
	}, "auth", "login", "chatgpt", "--no-browser")
	if err != nil || !strings.Contains(screen, signInURL) || !f.opts.PasteOnly {
		t.Fatalf("the control: with the echo off the sign-in did not begin: %v (screen %q)", err, screen)
	}
}

// noEcho is a muteSignIn that leaves the echo on: a control's.
type noEcho struct{}

func (noEcho) restore() {}

// TestAuthLoginChatGPTHidesAPastedKey (review r14 1): a key pasted at the
// sign-in's prompt the moment it shows — out of habit, where a key prompt
// would be — is on no part of the terminal: not echoed as it arrives, nor in
// the refusal, which says the plan takes no key and draws the prompt again.
// The redirect pasted after it signs in, the key never handed to the attempt.
// ptyAuth checks the whole screen, stdout and the error. The control is the
// same keyboard with the echo left on (muteSignIn a no-op): the key is on that
// screen, so the check finds a key the terminal echoes.
func TestAuthLoginChatGPTHidesAPastedKey(t *testing.T) {
	refused := "That is not an address: the ChatGPT plan is funded by signing in, never by an API key. " + pasteWhat(signInRedirect)
	drive := func(tail *ptyTail, ptmx *os.File) {
		typeAtPrompt(t, tail, ptmx, redirectPrompt, authKey)
		seePrompt(t, tail, refused+"\r\n"+redirectPrompt)
		if _, err := ptmx.WriteString(signInGood + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("the echo off", func(t *testing.T) {
		authNative(t)
		f := useFakeSignIn(t)
		screen, stdout, err := ptyAuth(t, drive, "auth", "login", "chatgpt")
		if err != nil {
			t.Fatalf("login on a terminal: %v (screen %q)", err, maskKeys(screen))
		}
		got := strings.ReplaceAll(screen, "\r\n", "\n")
		want := redirectPrompt + "\n" + refused + "\n" + redirectPrompt + "\n" + receivedText(signInRedirect) + "\n"
		if !strings.Contains(got, want) {
			t.Fatalf("the screen lacks %q:\n%s", want, maskKeys(got))
		}
		if stdout != signedInLine+noticeLines+modelsLine || len(f.pastes) != 1 || f.pastes[0] != signInGood {
			t.Fatalf("stdout %q, %d pastes", stdout, len(f.pastes))
		}
	})
	t.Run("control: the echo on", func(t *testing.T) {
		authNative(t)
		useFakeSignIn(t)
		prev := muteSignIn
		muteSignIn = func(*os.File) (echoRestorer, error) { return noEcho{}, nil }
		t.Cleanup(func() { muteSignIn = prev })
		screen, _, err := ptyAuthScreen(t, drive, "auth", "login", "chatgpt")
		if err != nil {
			t.Fatalf("login on a terminal: %v (screen %q)", err, maskKeys(screen))
		}
		if keyAt(screen) < 0 {
			t.Fatal("control: with the echo on the pasted key is not on the screen, so finding none there proves nothing")
		}
	})
}

// TestAuthLoginMenuChoosesTheChatGPTPlan (X134): the ChatGPT plan picked from
// login's menu is signed in to, never asked for a key: no key prompt is drawn,
// and the redirect's prompt has the echo off again, the sign-in's own (review
// r14 1) — the pasted address is confirmed by its origin and path alone.
func TestAuthLoginMenuChoosesTheChatGPTPlan(t *testing.T) {
	authNative(t)
	f := useFakeSignIn(t)
	screen, stdout, err := ptyAuth(t, func(tail *ptyTail, ptmx *os.File) {
		typeAtPrompt(t, tail, ptmx, "Provider [1-5]: ", "1")
		typeAtPrompt(t, tail, ptmx, redirectPrompt, signInGood)
	}, "auth", "login")
	if err != nil {
		t.Fatalf("login from the menu: %v (screen %q)", err, maskKeys(screen))
	}
	got := strings.ReplaceAll(screen, "\r\n", "\n")
	if strings.Contains(got, "API key") {
		t.Fatalf("a key was asked for:\n%s", got)
	}
	for _, want := range []string{"Provider [1-5]: 1\n", redirectPrompt + "\n" + receivedText(signInRedirect) + "\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the screen lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "stub-code") {
		t.Fatalf("the pasted address's query is on the screen:\n%s", got)
	}
	if stdout != signedInLine+noticeLines+modelsLine || len(f.pastes) != 1 {
		t.Fatalf("stdout %q, %d pastes", stdout, len(f.pastes))
	}
}

// TestAuthLogoutChatGPT (plan 033 §3.13): logout signs out — saying whether
// the revocation was confirmed, and that an issued access token may work up
// to an hour longer — or says there was no sign-in; a key written for the
// plan by hand, which funds nothing, goes too.
func TestAuthLogoutChatGPT(t *testing.T) {
	const hour = "An access token already issued may keep working for up to an hour, until it expires.\n"
	for _, c := range []struct {
		name   string
		res    chatgptauth.LogoutResult
		stored bool
		want   string
	}{
		{"revoked", chatgptauth.LogoutResult{SignedIn: true, Revoked: true}, false,
			"Signed out of ChatGPT: the sign-in is revoked, and its tokens are deleted from this machine.\n" + hour},
		{"not confirmed", chatgptauth.LogoutResult{SignedIn: true}, false,
			"Signed out of ChatGPT: its tokens are deleted from this machine, but remote revocation not confirmed. You can disconnect craze in ChatGPT's settings, under Apps.\n" + hour},
		{"not signed in", chatgptauth.LogoutResult{}, false, "Not signed in to ChatGPT.\n"},
		{"a key by hand", chatgptauth.LogoutResult{}, true, "Not signed in to ChatGPT.\nRemoved the key stored for ChatGPT plan in providers.toml; the plan never uses one.\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			native := authNative(t)
			f := useFakeSignIn(t)
			f.logout = c.res
			if c.stored {
				writeProviders(t, native, "version = 1\n\n[providers.chatgpt]\napi_key = \""+authKey+"\"\n")
			}
			stdout, stderr, err := runAuth(t, nil, "auth", "logout", "chatgpt")
			if err != nil || stdout != c.want || stderr != "" {
				t.Fatalf("logout = %q, %q, %v\nwant %q", stdout, stderr, err, c.want)
			}
			if f.logouts != 1 {
				t.Fatalf("signed out %d times", f.logouts)
			}
			if storedKey(t, native, "chatgpt") != "" {
				t.Fatal("the key stored for the plan is still there")
			}
		})
	}
	t.Run("failure", func(t *testing.T) {
		authNative(t)
		f := useFakeSignIn(t)
		f.logout, f.logoutErr = chatgptauth.LogoutResult{SignedIn: true}, errors.New("chatgptauth: removing the token file: read-only file system")
		_, _, err := runAuth(t, nil, "auth", "logout", "chatgpt")
		wantExit(t, err, 1, "craze auth logout: signing out of ChatGPT: removing the token file: read-only file system; the tokens may still be on this machine")
	})
}

// writeProviders writes native's providers.toml.
func writeProviders(t *testing.T, native, body string) {
	t.Helper()
	if err := os.MkdirAll(native, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, modeltable.ProvidersFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAuthListChatGPTStates (plan 033 §3.13): the ChatGPT plan's row is its
// sign-in — the account and how it renews, plan usage off and what to run,
// or not signed in — read from the registration and the token file's
// presence. A value in the token file never shows.
func TestAuthListChatGPTStates(t *testing.T) {
	const token = "fake-access-token-never-listed-0001"
	for _, c := range []struct {
		name      string
		planUsage bool
		tokens    bool
		want      string
	}{
		{"signed in", true, true, "signed in as " + signInEmail + " · ChatGPT plan · renews automatically"},
		{"plan usage disabled", false, false, "plan usage disabled — run craze auth login chatgpt"},
		{"signed out", true, false, "not signed in"},
	} {
		t.Run(c.name, func(t *testing.T) {
			native := authNative(t)
			auth := filepath.Join(native, "auth")
			if err := os.MkdirAll(auth, 0o700); err != nil {
				t.Fatal(err)
			}
			client := `{"client_id":"oaiapp_stub","subject":"user-stub","email":"` + signInEmail + `","plan_usage":` + map[bool]string{true: "true", false: "false"}[c.planUsage] + `,"notice_shown":true}`
			if err := os.WriteFile(filepath.Join(auth, "chatgpt-client.json"), []byte(client), 0o600); err != nil {
				t.Fatal(err)
			}
			if c.tokens {
				if err := os.WriteFile(filepath.Join(auth, "chatgpt.json"), []byte(`{"access_token":"`+token+`"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			stdout, _, err := runAuth(t, nil, "auth", "list")
			if err != nil {
				t.Fatal(err)
			}
			first := strings.SplitN(stdout, "\n", 2)[0]
			if want := "ChatGPT plan      chatgpt          " + c.want; first != want {
				t.Fatalf("the plan's row = %q; want %q", first, want)
			}
			if strings.Contains(stdout, token) {
				t.Fatal("a token value was listed")
			}
		})
	}
}

// TestAuthLoginChatGPTRealCancel: the real sign-in (chatgptauth, no stand-in)
// in a craze process of its own on a terminal, paste-only: the URL is a first
// registration's — no login_hint, never an id_token_hint — a wrong address
// pasted is refused by the attempt itself and never shown (the prompt does not
// echo: review r14 1), and SIGINT, SIGTERM or SIGHUP cancels it, saying so,
// with exit 128 plus the signal's number, no token file, and the terminal's
// echo back on. The control for the last is the echo at the prompt, which is
// off. Nothing leaves the machine: the endpoints are TestMain's fence, and no
// request is due before a redirect is accepted.
func TestAuthLoginChatGPTRealCancel(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			native := authNative(t)
			cmd, tail, ptmx, tty := authChildIO(t, nil, "auth", "login", "chatgpt", "--no-browser")
			seePrompt(t, tail, redirectPrompt)
			if echoing(t, tty) {
				t.Fatal("control: the echo is on at the sign-in's prompt, so finding it on afterwards proves nothing")
			}
			screen := tail.text()
			at := strings.Index(screen, chatgptFence+"/api/accounts/authorize?")
			if at < 0 {
				t.Fatalf("no authorization URL on the fence's issuer:\n%s", screen)
			}
			raw := strings.Fields(screen[at:])[0]
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			if q.Get("client_id") != "dynamic_agent_client" || q.Get("agent_name_hint") != "craze" || q.Get("redirect_uri") != signInRedirect ||
				q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" || q.Has("login_hint") || q.Has("id_token_hint") {
				t.Fatalf("the authorization URL is not a first registration's: %v", q)
			}
			if _, err := ptmx.WriteString(signInRedirect + "?code=x&state=not-this-attempts\n"); err != nil {
				t.Fatal(err)
			}
			if !tail.wait("That is not this sign-in's redirect address.", 10*time.Second) {
				t.Fatalf("the wrong address was not refused:\n%s", tail.text())
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if code := authChildExit(t, cmd, tail); code != 128+int(sig) {
				t.Fatalf("exit %d; want %d (the terminal shows %q)", code, 128+int(sig), tail.text())
			}
			if !tail.wait("craze auth login: the sign-in was cancelled; nothing was changed", 5*time.Second) {
				t.Fatalf("no cancel line:\n%s", tail.text())
			}
			if !echoing(t, tty) {
				t.Fatal("the signal ended the sign-in with the terminal's echo off")
			}
			if strings.Contains(tail.text(), "not-this-attempts") {
				t.Fatalf("the pasted address was echoed:\n%s", tail.text())
			}
			if _, err := os.Stat(filepath.Join(native, "auth", "chatgpt.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a cancelled sign-in left a token file")
			}
		})
	}
}

// TestAuthLoginChatGPTSignalWhileFetchingModels: a signal after the sign-in
// succeeded, while the plan's models are fetched, ends craze as the signal
// asked, saying the sign-in stands and the list was not fetched.
func TestAuthLoginChatGPTSignalWhileFetchingModels(t *testing.T) {
	authNative(t)
	f := useFakeSignIn(t)
	f.fetchBlocks = true
	finish := runSignIn(t, strings.NewReader(signInGood+"\n"), "auth", "login", "chatgpt")
	<-f.waiting // Wait began; the fetch is next
	f.signal(t, syscall.SIGINT)
	stdout, stderr, err := finish()
	wantExit(t, err, 130, "craze auth login: interrupted; you are signed in, but the plan's model list was not fetched")
	if stdout != signedInLine+noticeLines {
		t.Fatalf("stdout %q", stdout)
	}
	if !strings.Contains(stderr, "note: the plan's model list could not be fetched (context canceled); a native session fetches it when it opens\n") {
		t.Fatalf("stderr:\n%s", stderr)
	}
}

// TestReadRedirectLine: a pasted line is read whole, its line ending (LF or
// CRLF) dropped; a line over maxRedirectLine bytes is reported long and kept
// as nothing, all of it read, so the next line is read whole after it; the
// last line needs no newline. A long line is refused as too long, unquoted.
func TestReadRedirectLine(t *testing.T) {
	long := strings.Repeat("x", maxRedirectLine+1)
	exact := strings.Repeat("y", maxRedirectLine)
	br := bufio.NewReader(strings.NewReader(signInGood + "\r\n" + long + "\n" + exact + "\n" + "last"))
	for i, want := range []struct {
		line string
		long bool
		err  error
	}{
		{signInGood, false, nil},
		{"", true, nil},
		{exact, false, nil},
		{"last", false, io.EOF},
	} {
		line, isLong, err := readRedirectLine(br)
		if line != want.line || isLong != want.long || !errors.Is(err, want.err) && err != want.err {
			t.Fatalf("line %d = (%d bytes, long %v, %v); want (%d bytes, long %v, %v)", i, len(line), isLong, err, len(want.line), want.long, want.err)
		}
	}
	got := redirectRefusal("", true, signInRedirect)
	if got != "That is too long to be the redirect address. Paste the whole address the browser was sent to; it starts with "+signInRedirect+"." {
		t.Fatalf("the long line's refusal = %q", got)
	}
	if redirectRefusal(signInGood, false, signInRedirect) != "" {
		t.Fatal("an address is refused before the attempt judges it")
	}
}
