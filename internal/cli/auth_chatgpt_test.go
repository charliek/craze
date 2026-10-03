package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/signinlog"
)

// craze auth for the ChatGPT plan (plan 033 §3.13). These tests drive the
// CLI's own work — the waits, the texts, the browser rule, Ctrl-C — through
// auth_chatgpt.go's seams, with a stand-in for chatgptauth's Attempt
// (fakeSignIn) that keeps its documented contract: a redirect is accepted
// once, pasted or from the listener; anything else pasted is refused as the
// attempt refuses it (a *chatgptauth.PasteError: too long, not an address, or
// the part that is not this attempt's redirect); a cancelled Wait closes the
// attempt. The sign-in's
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
	// begin, when its Kind is set, is the event the stand-in's begin reports
	// to the sign-in's observer, as chatgptauth.Begin reports its own.
	begin chatgptauth.Event
	// pasteHold, when set, keeps an accepted Paste from returning until it is
	// closed, and waitHold a Wait whose redirect is in: a case orders the
	// sign-in's loop with them (TestAuthLoginChatGPTConfirmsAPasteEitherWay).
	pasteHold chan struct{}
	waitHold  chan struct{}

	mu       sync.Mutex
	opts     chatgptauth.BeginOptions
	pastes   []string
	refusals []chatgptauth.PasteError // every paste refused, as the attempt refused it
	over     bool
	closed   bool
	reasons  []chatgptauth.CloseReason // every Close's reason, in order
	reported []chatgptauth.EventKind   // every Report's kind
	observed bool                      // the model fetch was handed an observer
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
		if f.beginErr != nil {
			f.mu.Unlock()
			return nil, f.beginErr
		}
		f.opts = opts
		if opts.PasteOnly {
			f.listening = false
		}
		begin := f.begin
		f.mu.Unlock()
		if begin.Kind != "" && opts.Observe != nil {
			opts.Observe(begin)
		}
		return f, nil
	}
	fetchPlanModels = func(ctx context.Context, _ string, observe func(chatgptauth.Event)) (*chatgptauth.Models, error) {
		f.mu.Lock()
		f.fetched++
		f.observed = observe != nil
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

// Paste judges raw as chatgptauth.Attempt.Paste does (plan 034 §3.3): longer
// than MaxPaste, not an address, or — once the attempt is not over — another
// address than the stand-in's redirect (PartPath), or its redirect address
// with another query (PartState: the stand-in's state is its only one).
func (f *fakeSignIn) Paste(raw string) error {
	f.mu.Lock()
	f.pastes = append(f.pastes, raw)
	refuse := func(pe chatgptauth.PasteError) error {
		f.refusals = append(f.refusals, pe)
		f.mu.Unlock()
		return &pe
	}
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	switch {
	case len(raw) > chatgptauth.MaxPaste:
		return refuse(chatgptauth.PasteError{Refusal: chatgptauth.RefusalTooLong})
	case err != nil || u.Scheme == "" || u.Host == "":
		return refuse(chatgptauth.PasteError{Refusal: chatgptauth.RefusalNotAddress})
	case f.over:
		f.mu.Unlock()
		return chatgptauth.ErrAttemptOver
	case trimmed != signInGood && u.Scheme+"://"+u.Host+u.Path == signInRedirect:
		return refuse(chatgptauth.PasteError{Refusal: chatgptauth.RefusalMismatch, Part: chatgptauth.PartState})
	case trimmed != signInGood:
		return refuse(chatgptauth.PasteError{Refusal: chatgptauth.RefusalMismatch, Part: chatgptauth.PartPath})
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
		f.shut()
		return chatgptauth.Result{}, ctx.Err()
	case <-f.closedCh:
		return chatgptauth.Result{}, chatgptauth.ErrAttemptOver
	}
}

// Close records why, and closes the stand-in, once.
func (f *fakeSignIn) Close(reason chatgptauth.CloseReason) {
	f.mu.Lock()
	f.reasons = append(f.reasons, reason)
	f.mu.Unlock()
	f.shut()
}

// shut is the close itself: a Wait's own, on its context's end, which no
// caller asked for.
func (f *fakeSignIn) shut() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		f.over = true
		close(f.closedCh)
	}
}

// Report records kind.
func (f *fakeSignIn) Report(kind chatgptauth.EventKind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reported = append(f.reported, kind)
}

// Observer is the begin's observer, as the real attempt's is: what the
// sign-in hands the model fetch.
func (f *fakeSignIn) Observer() func(chatgptauth.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opts.Observe
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
// (a key, the likeliest mistake: the attempt refuses it, plan 034 §3.3), the
// redirect of an earlier attempt and another address, takes the right one,
// and prints the account, the one-time notice (recorded as shown) and the
// plan's models, fetched with the attempt's observer. A blank line reaches no
// attempt. The attempt is closed as done.
func TestAuthLoginChatGPTPasted(t *testing.T) {
	native := authNative(t)
	f := useFakeSignIn(t)
	wrong := signInRedirect + "?code=stub-code&state=another-attempt"
	other := "http://127.0.0.1:1455/elsewhere?code=stub-code&state=stub-state"
	stdin := strings.NewReader(authKey + "\n\n" + wrong + "\n" + other + "\n" + signInGood + "\n")
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
		"That is the redirect of an earlier sign-in attempt. Use the address shown above.\n" +
		"That is not this sign-in's redirect address. Paste the whole address the browser was sent to; it starts with " + signInRedirect + ".\n"
	if stderr != wantErr {
		t.Fatalf("stderr:\n%s\nwant:\n%s", maskKeys(stderr), wantErr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.opts.PasteOnly {
		t.Fatal("--no-browser did not make the sign-in paste-only")
	}
	if len(f.pastes) != 4 || f.pastes[0] != authKey || f.pastes[1] != wrong || f.pastes[2] != other || f.pastes[3] != signInGood {
		t.Fatalf("the attempt was handed %d lines; want the key and the three addresses (no blank line)", len(f.pastes))
	}
	if want := []chatgptauth.PasteError{
		{Refusal: chatgptauth.RefusalNotAddress},
		{Refusal: chatgptauth.RefusalMismatch, Part: chatgptauth.PartState},
		{Refusal: chatgptauth.RefusalMismatch, Part: chatgptauth.PartPath},
	}; !slices.Equal(f.refusals, want) {
		t.Fatalf("the attempt refused %v; want %v", f.refusals, want)
	}
	if f.marked != 1 || f.fetched != 1 || !f.observed {
		t.Fatalf("notice recorded %d times, models fetched %d times (with the observer: %v); want 1, 1 and true", f.marked, f.fetched, f.observed)
	}
	if len(f.opened) != 0 || len(f.reported) != 0 {
		t.Fatalf("a browser was opened (reported %v)", f.reported)
	}
	if !f.closed || !slices.Equal(f.reasons, []chatgptauth.CloseReason{chatgptauth.CloseDone}) {
		t.Fatalf("the attempt was closed with %v; want done", f.reasons)
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
// `craze auth login chatgpt` is never stored, asked for or repeated: the
// attempt refuses it as not an address (plan 034 §3.3: the pre-check is the
// attempt's), and the sign-in says the plan takes no key; a paste-only
// sign-in whose stdin then ends exits 1, its attempt closed for the end of its
// input.
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
	if !slices.Equal(f.refusals, []chatgptauth.PasteError{{Refusal: chatgptauth.RefusalNotAddress}}) {
		t.Fatalf("the attempt refused %v; want the key, as not an address", f.refusals)
	}
	if !f.closed || !slices.Equal(f.reasons, []chatgptauth.CloseReason{chatgptauth.CloseNoInput}) {
		t.Fatalf("the attempt was closed with %v; want no_input", f.reasons)
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
			if !f.closed || !slices.Equal(f.reasons, []chatgptauth.CloseReason{chatgptauth.CloseSignal}) {
				t.Fatalf("the attempt — and its listener — was closed with %v; want signal", f.reasons)
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
		// A step that ran out of time is named (plan 034 A14).
		{"exchange timed out", nil, &chatgptauth.StepTimeoutError{Step: chatgptauth.StepExchange, After: 30 * time.Second}, "craze auth login: exchange: timed out after 30s"},
		{"a lock held", &chatgptauth.StepTimeoutError{Step: chatgptauth.StepBegin, After: time.Minute, Lock: "the sign-in lock"}, nil, "craze auth login: begin: timed out after 1m0s waiting for the sign-in lock, which another craze process holds"},
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
			var reported []chatgptauth.EventKind
			if c.opened {
				want = []string{signInURL}
				reported = []chatgptauth.EventKind{chatgptauth.EventBrowserOpened}
				if c.openErr != nil {
					reported = []chatgptauth.EventKind{chatgptauth.EventBrowserFailed}
				}
			}
			if !slices.Equal(f.opened, want) {
				t.Fatalf("opened %q; want %q", f.opened, want)
			}
			if !slices.Equal(f.reported, reported) {
				t.Fatalf("reported %v; want %v", f.reported, reported)
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
// Each order's screen is the other's control. However the test ends, the hold
// is released and the command joined (review r15 d): a prompt that never shows
// fails the test with a goroutine still held, which must not run on into the
// next test's.
func TestAuthLoginChatGPTConfirmsAPasteEitherWay(t *testing.T) {
	confirmed := redirectPrompt + "\n" + receivedText(signInRedirect) + "\n"
	for _, first := range []string{"the event", "the result"} {
		t.Run(first, func(t *testing.T) {
			authNative(t)
			f := useFakeSignIn(t)
			hold := make(chan struct{})
			release := sync.OnceFunc(func() { close(hold) })
			if first == "the event" {
				f.waitHold = hold
			} else {
				f.pasteHold = hold
			}
			screen, stdout, err := ptyAuth(t, func(tail *ptyTail, ptmx *os.File) {
				// Registered once the command runs, after ptyAuthScreen's
				// cleanup that joins it, so it runs before that join: the
				// held goroutine is let go, then the command is waited for.
				t.Cleanup(release)
				typeAtPrompt(t, tail, ptmx, redirectPrompt, signInGood)
				if first == "the event" {
					seePrompt(t, tail, receivedText(signInRedirect))
				} else {
					seePrompt(t, tail, redirectPrompt+"\r\n")
				}
				release()
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
// The redirect pasted after it signs in; the attempt refused the key as not
// an address (plan 034 §3.3), keeping nothing of it.
// ptyAuth checks the whole screen, stdout and the error. The control is the
// same keyboard with the echo left on (muteSignIn a no-op): the key is on that
// screen, so the check finds a key the terminal echoes.
func TestAuthLoginChatGPTHidesAPastedKey(t *testing.T) {
	refused := pasteRefusalText(&chatgptauth.PasteError{Refusal: chatgptauth.RefusalNotAddress}, signInRedirect)
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
		if stdout != signedInLine+noticeLines+modelsLine || len(f.pastes) != 2 || f.pastes[1] != signInGood ||
			!slices.Equal(f.refusals, []chatgptauth.PasteError{{Refusal: chatgptauth.RefusalNotAddress}}) {
			t.Fatalf("stdout %q, %d pastes, refused %v; want the key refused as not an address, then the redirect", stdout, len(f.pastes), f.refusals)
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
			if !tail.wait("That is the redirect of an earlier sign-in attempt. Use the address shown above.", 10*time.Second) {
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

// authSignInPanicEnv makes the sign-in of the test binary's craze child panic
// on one of its own goroutines (childSignInPanic): "reader", the stdin
// reader's, as it hands the line typed at the prompt to the attempt; "wait",
// the wait's, once that line has reached the attempt. Read by the child's init
// in serve_child_test.go.
const authSignInPanicEnv = "CRAZE_CLI_TEST_SIGNIN_PANIC"

// signInPanicText is the panicking stand-in's panic value, which the child's
// stderr — the terminal — shows as craze crashes.
const signInPanicText = "craze test: the sign-in's stand-in panics"

// panicSignIn is the craze child's stand-in attempt under
// authSignInPanicEnv: paste-only, with this file's fixed addresses. Where is
// "reader": its Paste panics, on the stdin reader's goroutine. Where is
// "wait": its Paste takes the line, and its Wait, on the wait's goroutine,
// panics once it has.
type panicSignIn struct {
	where  string
	pasted chan struct{}
	once   sync.Once
}

func (p *panicSignIn) URL() string                       { return signInURL }
func (p *panicSignIn) RedirectURI() string               { return signInRedirect }
func (p *panicSignIn) Listening() bool                   { return false }
func (p *panicSignIn) Close(chatgptauth.CloseReason)     {}
func (p *panicSignIn) Report(chatgptauth.EventKind)      {}
func (p *panicSignIn) Observer() func(chatgptauth.Event) { return nil }

func (p *panicSignIn) Paste(string) error {
	if p.where == "reader" {
		panic(signInPanicText)
	}
	p.once.Do(func() { close(p.pasted) })
	return nil
}

func (p *panicSignIn) Wait(ctx context.Context) (chatgptauth.Result, error) {
	select {
	case <-p.pasted:
		panic(signInPanicText)
	case <-ctx.Done():
		return chatgptauth.Result{}, ctx.Err()
	}
}

// childSignInPanic puts panicSignIn, panicking where says, behind the craze
// child's sign-in.
func childSignInPanic(where string) {
	beginSignIn = func(context.Context, string, chatgptauth.BeginOptions) (signInAttempt, error) {
		return &panicSignIn{where: where, pasted: make(chan struct{})}, nil
	}
}

// termiosOf is the terminal settings of tty, the pty's slave side.
func termiosOf(t *testing.T, tty *os.File) unix.Termios {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(tty.Fd()), getTermios)
	if err != nil {
		t.Fatal(err)
	}
	return *tio
}

// TestAuthLoginChatGPTPanicRestoresTheEcho (review r15 a): a panic on one of
// the sign-in's own goroutines — the stdin reader's, or the wait's — ends
// craze as a panic does, exit 2 with the panic on stderr, but with the
// terminal put back first: its echo on, and every setting as a new terminal
// has them, as craze found this one. craze runs in a process of its own, so
// the panic is a real one that ends it, past every deferred call but the
// panicking goroutine's; its attempt is a stand-in that panics
// (authSignInPanicEnv). The control is the echo at the prompt, which is off.
func TestAuthLoginChatGPTPanicRestoresTheEcho(t *testing.T) {
	// A new terminal's settings, which the child's terminal has when it
	// starts: what a restored one is compared with.
	newPTMX, newTTY, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	found := termiosOf(t, newTTY)
	_ = newPTMX.Close()
	_ = newTTY.Close()
	for _, where := range []string{"reader", "wait"} {
		t.Run(where, func(t *testing.T) {
			authNative(t)
			cmd, tail, ptmx, tty := authChildIO(t, []string{authSignInPanicEnv + "=" + where}, "auth", "login", "chatgpt", "--no-browser")
			seePrompt(t, tail, redirectPrompt)
			if echoing(t, tty) {
				t.Fatal("control: the echo is on at the sign-in's prompt, so finding it on afterwards proves nothing")
			}
			if _, err := ptmx.WriteString(signInGood + "\n"); err != nil {
				t.Fatal(err)
			}
			if code := authChildExit(t, cmd, tail); code != 2 {
				t.Fatalf("exit %d; want a panic's 2 (the terminal shows %q)", code, tail.text())
			}
			if !tail.wait("panic: "+signInPanicText, 5*time.Second) {
				t.Fatalf("the terminal does not show the %s's panic:\n%s", where, tail.text())
			}
			if !echoing(t, tty) {
				t.Fatalf("a panic in the sign-in's %s left the terminal's echo off", where)
			}
			if got := termiosOf(t, tty); got != found {
				t.Fatalf("a panic in the sign-in's %s left the terminal's settings changed:\n got %+v\nwant %+v", where, got, found)
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
// CRLF) dropped; a line over maxRedirectLine bytes — however much over — is
// reported long and kept cut one byte past the bound, all of it read, so the
// next line is read whole after it; the last line needs no newline. The cut
// line is one the attempt refuses as too long (chatgptauth.MaxPaste is the
// bound), phrased unquoted; the control is a line of exactly the bound, which
// the attempt judges as an address.
func TestReadRedirectLine(t *testing.T) {
	long := strings.Repeat("x", maxRedirectLine+1)
	longer := strings.Repeat("z", 3*maxRedirectLine) + "\r"
	exact := strings.Repeat("y", maxRedirectLine)
	br := bufio.NewReader(strings.NewReader(signInGood + "\r\n" + long + "\n" + longer + "\n" + exact + "\n" + "last"))
	for i, want := range []struct {
		line string
		long bool
		err  error
	}{
		{signInGood, false, nil},
		{long, true, nil},
		{longer[:maxRedirectLine+1], true, nil},
		{exact, false, nil},
		{"last", false, io.EOF},
	} {
		line, isLong, err := readRedirectLine(br)
		if line != want.line || isLong != want.long || !errors.Is(err, want.err) && err != want.err {
			t.Fatalf("line %d = (%d bytes, long %v, %v); want (%d bytes, long %v, %v)", i, len(line), isLong, err, len(want.line), want.long, want.err)
		}
	}
	if maxRedirectLine != chatgptauth.MaxPaste {
		t.Fatalf("the reader's bound %d is not the attempt's %d", maxRedirectLine, chatgptauth.MaxPaste)
	}
	got := pasteRefusalText(&chatgptauth.PasteError{Refusal: chatgptauth.RefusalTooLong}, signInRedirect)
	if got != "That is too long to be the redirect address. Paste the whole address the browser was sent to; it starts with "+signInRedirect+"." {
		t.Fatalf("the long line's refusal = %q", got)
	}
}

// signInRecords is native's sign-in log, each record decoded (plan 034
// §3.3): nil when there is none.
func signInRecords(t *testing.T, native string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(native, paths.LogsName, signinlog.FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a sign-in log record is not JSON: %v", err)
		}
		out = append(out, rec)
	}
	return out
}

// recordKinds is the event of each record.
func recordKinds(recs []map[string]any) []string {
	var out []string
	for _, r := range recs {
		s, _ := r["event"].(string)
		out = append(out, s)
	}
	return out
}

// TestAuthLoginChatGPTSaysWhyItIsNotListening (plan 034 Q8, §3.3): an attempt
// that is paste-only because it could not listen says so on stderr, before
// what to paste — the port busy, or no listener to be had; one paste-only by
// request (--no-browser) says nothing of a port, the control. The attempt's
// begin is in the sign-in log, surface cli.
func TestAuthLoginChatGPTSaysWhyItIsNotListening(t *testing.T) {
	for _, c := range []struct {
		name string
		why  chatgptauth.Reason
		argv []string
		says string
	}{
		{"port busy", chatgptauth.ReasonPortBusy, nil, "craze is not listening for the browser: another program is using 127.0.0.1:1455.\n"},
		{"no listener", chatgptauth.ReasonListenFailed, nil, "craze is not listening for the browser: it could not listen on 127.0.0.1:1455.\n"},
		{"by request", chatgptauth.ReasonRequested, []string{"--no-browser"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			native := authNative(t)
			f := useFakeSignIn(t)
			f.begin = chatgptauth.Event{
				Kind: chatgptauth.EventBegin, Attempt: "0a1b2c3d", Mode: chatgptauth.ModePasteOnly, Port: 1455,
				Reason: c.why, Registration: chatgptauth.RegistrationNew,
			}
			argv := append([]string{"auth", "login", "chatgpt"}, c.argv...)
			_, stderr, err := runSignIn(t, strings.NewReader(signInGood+"\n"), argv...)()
			if err != nil {
				t.Fatal(err)
			}
			paste := "After you approve, the browser goes to an address starting with "
			switch {
			case c.says != "" && !strings.Contains(stderr, c.says+paste):
				t.Fatalf("stderr:\n%s\nwant %q before what to paste", stderr, c.says)
			case c.says == "" && strings.Contains(stderr, "not listening"):
				t.Fatalf("the control: a paste-only sign-in by request spoke of a port:\n%s", stderr)
			}
			recs := signInRecords(t, native)
			if len(recs) != 1 || recs[0]["event"] != "begin" || recs[0]["surface"] != "cli" || recs[0]["attempt"] != "0a1b2c3d" || recs[0]["reason"] != string(c.why) {
				t.Fatalf("the sign-in log holds %v; want the begin, surface cli", recs)
			}
		})
	}
}

// TestAuthLoginChatGPTRefusedLogIsOneNote (plan 034 A12): a sign-in log that
// is refused — a symlink where its directory goes — is one note on stderr,
// naming why, and the sign-in goes on and finishes as ever, nothing written
// through the symlink. The control is the same sign-in with the log's
// directory free: no note.
func TestAuthLoginChatGPTRefusedLogIsOneNote(t *testing.T) {
	for _, refused := range []bool{true, false} {
		t.Run(map[bool]string{true: "refused", false: "control"}[refused], func(t *testing.T) {
			native := authNative(t)
			useFakeSignIn(t)
			elsewhere := t.TempDir()
			if refused {
				if err := os.MkdirAll(native, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(elsewhere, filepath.Join(native, paths.LogsName)); err != nil {
					t.Fatal(err)
				}
			}
			stdout, stderr, err := runSignIn(t, strings.NewReader(signInGood+"\n"), "auth", "login", "chatgpt")()
			if err != nil || stdout != signedInLine+noticeLines+modelsLine {
				t.Fatalf("login = %q, %v; a refused log must not stop the sign-in", stdout, err)
			}
			notes := strings.Count(stderr, "note: the sign-in log is off: ")
			switch {
			case refused && (notes != 1 || !strings.Contains(stderr, "is a symbolic link")):
				t.Fatalf("stderr:\n%s\nwant one note saying the log's directory is a symlink", stderr)
			case !refused && notes != 0:
				t.Fatalf("the control noted the log:\n%s", stderr)
			}
			if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
				t.Fatal("the log was written through the symlink")
			}
		})
	}
}

// TestAuthLoginChatGPTSaysTheListenerSawAnotherAttempt (plan 034 A13, Q8): the
// real attempt (chatgptauth, no stand-in), a re-login listening on 127.0.0.1:
// a redirect carrying a code and another attempt's state, sent to the
// listener three times — an earlier attempt's browser — is said once on
// stderr, and logged once, its count at the attempt's end; a 404 is neither.
// Ctrl-C then ends it, logged as the signal's. Nothing leaves the machine:
// the endpoints are TestMain's fence, and no request is due before a redirect
// is accepted.
func TestAuthLoginChatGPTSaysTheListenerSawAnotherAttempt(t *testing.T) {
	native := authNative(t)
	if err := os.MkdirAll(filepath.Join(native, "auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	client := `{"client_id":"oaiapp_stub0000000000000001","subject":"user-stub","email":"` + signInEmail + `","plan_usage":true,"notice_shown":true}`
	if err := os.WriteFile(chatgptauth.ClientFile(native), []byte(client), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"DISPLAY", "WAYLAND_DISPLAY"} {
		t.Setenv(v, "")
	}
	t.Setenv("SSH_CONNECTION", "10.0.0.2 5555 10.0.0.1 22")
	var mu sync.Mutex
	var sigs chan<- os.Signal
	prevNotify, prevStop := notifySignInSignals, stopSignInSignals
	notifySignInSignals = func(c chan<- os.Signal) {
		mu.Lock()
		defer mu.Unlock()
		sigs = c
	}
	stopSignInSignals = func(chan<- os.Signal) {}
	t.Cleanup(func() { notifySignInSignals, stopSignInSignals = prevNotify, prevStop })

	stdinR, stdinW := io.Pipe()
	t.Cleanup(func() { _ = stdinW.Close() })
	var out, errw lockedBuffer
	cmd := NewRootCmd()
	cmd.SetIn(stdinR)
	cmd.SetOut(&out)
	cmd.SetErr(&errw)
	cmd.SetArgs([]string{"auth", "login", "chatgpt"})
	done := make(chan error, 1)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		done <- cmd.Execute()
	}()
	// However the test ends, the command is ended and joined.
	t.Cleanup(func() {
		mu.Lock()
		c := sigs
		mu.Unlock()
		if c != nil {
			select {
			case c <- syscall.SIGINT:
			default:
			}
		}
		select {
		case <-ended:
		case <-time.After(10 * time.Second):
			t.Error("the sign-in outlived its test")
		}
	})

	waiting := regexp.MustCompile(`craze is waiting for the browser to come back to (http://127\.0\.0\.1:\d+/auth/callback)\.`)
	var callback string
	deadline := time.Now().Add(10 * time.Second)
	for callback == "" {
		if m := waiting.FindStringSubmatch(errw.String()); m != nil {
			callback = m[1]
		} else if time.Now().After(deadline) {
			t.Fatalf("the sign-in never listened:\n%s", errw.String())
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
	get := func(u string) int {
		t.Helper()
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	base := strings.TrimSuffix(callback, "/auth/callback")
	if got := get(base + "/favicon.ico"); got != http.StatusNotFound {
		t.Fatalf("a request off the callback path = %d; want 404", got)
	}
	for range 3 {
		if got := get(callback + "?code=fake-code-earlier&state=an-earlier-attempts-state"); got != http.StatusBadRequest {
			t.Fatalf("another attempt's redirect = %d; want 400", got)
		}
	}
	deadline = time.Now().Add(10 * time.Second)
	for !strings.Contains(errw.String(), otherAttemptText) {
		if time.Now().After(deadline) {
			t.Fatalf("the sign-in never said the browser came back from another attempt:\n%s", errw.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	c := sigs
	mu.Unlock()
	c <- syscall.SIGINT
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sign-in never ended on SIGINT")
	}
	wantExit(t, err, 130, "the sign-in was cancelled")
	if n := strings.Count(errw.String(), otherAttemptText); n != 1 {
		t.Fatalf("the sign-in said the other attempt's return %d times; want once:\n%s", n, errw.String())
	}
	if strings.Contains(errw.String(), "an-earlier-attempts-state") || strings.Contains(errw.String(), "fake-code-earlier") {
		t.Fatal("the other attempt's redirect was quoted")
	}
	recs := signInRecords(t, native)
	if got, want := recordKinds(recs), []string{"begin", "listener_refused", "listener_refusals", "cancelled"}; !slices.Equal(got, want) {
		t.Fatalf("the sign-in log holds %v; want %v", got, want)
	}
	if recs[1]["refusal"] != "other_attempt" || recs[1]["status"] != 400.0 || recs[1]["count"] != nil {
		t.Fatalf("the refusal's record = %v", recs[1])
	}
	if recs[2]["refusal"] != "other_attempt" || recs[2]["count"] != 3.0 {
		t.Fatalf("the refusals' count = %v; want 3", recs[2])
	}
	if recs[3]["reason"] != "signal" {
		t.Fatalf("the outcome = %v; want cancelled by the signal", recs[3])
	}
	for _, r := range recs {
		if r["surface"] != "cli" || r["attempt"] != recs[0]["attempt"] {
			t.Fatalf("a record is not the attempt's, surface cli: %v", r)
		}
		if r["status"] == 404.0 {
			t.Fatal("a 404 was logged")
		}
	}
	if b, _ := os.ReadFile(filepath.Join(native, paths.LogsName, signinlog.FileName)); strings.Contains(string(b), "earlier") || strings.Contains(string(b), signInEmail) {
		t.Fatal("the sign-in log holds a value of the request or the account")
	}
}

// TestAuthLoginChatGPTNeverWaitsOnTheLog (plan 034 review r3 #4): a sign-in
// log whose writer is stuck in the file system — held before it touches a
// file, for good (signinlog.Options.Stall) — holds up neither the sign-in's
// begin nor its wait nor its end: the sign-in finishes as ever, within the
// log's one-second close, and says once, at the end, that the log closed
// with records unwritten — what was queued is lost with the writer — as the
// TUI says it (review r5 #6). The control is the sign-in itself, which prints
// what it always does.
func TestAuthLoginChatGPTNeverWaitsOnTheLog(t *testing.T) {
	authNative(t)
	f := useFakeSignIn(t)
	f.begin = chatgptauth.Event{Kind: chatgptauth.EventBegin, Attempt: "0a1b2c3d", Mode: chatgptauth.ModeListening, Port: 1455, Registration: chatgptauth.RegistrationNew}
	stall := make(chan struct{}) // never closed: the writer touches nothing, ever
	prev := openSignInLog
	openSignInLog = func(dir string) (*signinlog.Log, error) {
		return signinlog.OpenWith(dir, signinlog.Options{Stall: stall})
	}
	t.Cleanup(func() { openSignInLog = prev })
	start := time.Now()
	stdout, stderr, err := runSignIn(t, strings.NewReader(signInGood+"\n"), "auth", "login", "chatgpt")()
	if err != nil || stdout != signedInLine+noticeLines+modelsLine {
		t.Fatalf("login = %q, %v; a stuck log must not stop the sign-in", stdout, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the sign-in took %s behind a stuck log", d)
	}
	if n := strings.Count(stderr, "sign-in log"); n != 1 || !strings.Contains(stderr, "note: the sign-in log is off: signinlog: the log closed before every record was written\n") {
		t.Fatalf("stderr:\n%s\nwant one note saying the log closed with records unwritten", stderr)
	}
}

// TestAuthLoginChatGPTNotesALogBrokenByItsLastRecord (plan 034 review r3
// #8c): a log that breaks at the sign-in's very last record — its directory
// made writable by others just before the model fetch reports — is still one
// note on stderr, at the end, once the log's close has let its writer reach
// it. The control is the record before the break, which is written.
func TestAuthLoginChatGPTNotesALogBrokenByItsLastRecord(t *testing.T) {
	native := authNative(t)
	f := useFakeSignIn(t)
	f.begin = chatgptauth.Event{Kind: chatgptauth.EventBegin, Attempt: "0a1b2c3d", Mode: chatgptauth.ModeListening, Port: 1455, Registration: chatgptauth.RegistrationNew}
	logs := filepath.Join(native, paths.LogsName)
	fetch := fetchPlanModels
	fetchPlanModels = func(ctx context.Context, dir string, observe func(chatgptauth.Event)) (*chatgptauth.Models, error) {
		// The begin is written before the log is broken: the control.
		for deadline := time.Now().Add(10 * time.Second); len(signInRecords(t, native)) != 1; time.Sleep(2 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Error("the begin was never written")
				break
			}
		}
		if err := os.Chmod(logs, 0o777); err != nil {
			t.Error(err)
		}
		observe(chatgptauth.Event{Kind: chatgptauth.EventModelsFetched, Attempt: "0a1b2c3d", Models: 2, ClientVersion: "0.160.0"})
		return fetch(ctx, dir, observe)
	}
	t.Cleanup(func() { fetchPlanModels = fetch })
	stdout, stderr, err := runSignIn(t, strings.NewReader(signInGood+"\n"), "auth", "login", "chatgpt")()
	if err != nil || stdout != signedInLine+noticeLines+modelsLine {
		t.Fatalf("login = %q, %v; a broken log must not stop the sign-in", stdout, err)
	}
	if n := strings.Count(stderr, "note: the sign-in log is off: "); n != 1 || !strings.Contains(stderr, "is writable by other users") {
		t.Fatalf("stderr:\n%s\nwant one note saying the log's directory is writable by others", stderr)
	}
	if got := recordKinds(signInRecords(t, native)); !slices.Equal(got, []string{"begin"}) {
		t.Fatalf("the log holds %v; want the begin before the break, alone", got)
	}
}
