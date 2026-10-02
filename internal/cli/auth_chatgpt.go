package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// craze auth for the ChatGPT plan (plan 033 §3.13, owner decisions 4 and 10,
// P21): the plan is funded by Sign in with ChatGPT, never by a key, so its
// three commands are a sign-in, a sign-out and a status row rather than a
// stored key. `craze auth login chatgpt` — or the plan picked from login's
// menu — runs the whole sign-in here (chatgptauth.Begin): it prints the
// authorization URL, opens a browser only in a desktop session, and waits for
// the browser's redirect on the loopback listener or pasted on stdin — on a
// terminal at a prompt that does not echo, so a key pasted there out of habit
// is never displayed, and an accepted address is confirmed by its origin and
// path alone, never its query (review r14 1, amending §3.13's "shown, not
// masked"). Once signed in it prints the account's email, the one-time
// plan-usage notice, and the plan's models, fetched there and then. `craze
// auth logout chatgpt` revokes and deletes the tokens; `craze auth list` shows
// the sign-in's state. No token, nor any part of one, is ever printed:
// chatgptauth's errors name a step, a status and an OAuth code, nothing a
// server sent, and a pasted line that is not this sign-in's redirect — an API
// key pasted out of habit, say — is never repeated back.

// signInAttempt is the part of a chatgptauth.Attempt the CLI drives: a seam,
// so the tests can stand in for the browser and OpenAI (auth_chatgpt_test.go)
// and the CLI's own work — the waits, the texts, Ctrl-C — is tested apart
// from the sign-in's, which chatgptauth's tests and tests/cli's fake issuer
// cover.
type signInAttempt interface {
	URL() string
	RedirectURI() string
	Listening() bool
	Paste(raw string) error
	Wait(ctx context.Context) (chatgptauth.Result, error)
	Close()
}

// The sign-in's seams: chatgptauth and the platform in craze; stand-ins in
// this package's tests, which must never reach OpenAI or open a browser.
var (
	beginSignIn = func(ctx context.Context, dir string, opts chatgptauth.BeginOptions) (signInAttempt, error) {
		a, err := chatgptauth.Begin(ctx, dir, opts)
		if err != nil {
			return nil, err
		}
		return a, nil
	}
	fetchPlanModels = func(ctx context.Context, dir string) (*chatgptauth.Models, error) {
		return chatgptauth.FetchModels(ctx, chatgptauth.Source(dir))
	}
	markNoticeShown = chatgptauth.MarkNoticeShown
	signOutChatGPT  = chatgptauth.Logout
	// openBrowser opens url in the desktop's browser (startBrowser), only
	// ever in a desktop session (guiSession).
	openBrowser = startBrowser
	// browserGOOS is the platform guiSession judges by.
	browserGOOS = runtime.GOOS
	// notifySignInSignals and stopSignInSignals are signal.Notify and
	// signal.Stop for the signals that cancel a sign-in: a test sends its own
	// on the channel instead of signalling the test binary.
	notifySignInSignals = func(c chan<- os.Signal) { signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP) }
	stopSignInSignals   = func(c chan<- os.Signal) { signal.Stop(c) }
	// muteSignIn turns the terminal's echo off for the sign-in (muteEcho)
	// and answers what puts it back: a seam, so a test's control can leave
	// the echo on and see what the terminal would show then.
	muteSignIn = func(f *os.File) (echoRestorer, error) {
		q, err := muteEcho(f)
		if err != nil {
			return nil, err
		}
		return q, nil
	}
)

// echoRestorer puts a terminal's echo back (echoOff.restore); a second call
// does nothing.
type echoRestorer interface{ restore() }

// context is the command's context, or a background one when it has none
// (an authRun a test built by hand).
func (a *authRun) context() context.Context {
	if a.ctx == nil {
		return context.Background()
	}
	return a.ctx
}

// guiSession reports whether a browser craze opens would be seen by the
// person at this terminal (plan 033 §3.13): on Linux, a graphical session
// (DISPLAY or WAYLAND_DISPLAY) that is not reached over SSH; on macOS, any
// session not over SSH. Anywhere else, never. SSH is any of the variables
// sshd sets for a session, so a terminal multiplexer started over SSH and
// kept running still counts as remote while it carries them.
func guiSession(goos string, getenv func(string) string) bool {
	for _, v := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY"} {
		if getenv(v) != "" {
			return false
		}
	}
	switch goos {
	case "darwin":
		return true
	case "linux":
		return getenv("DISPLAY") != "" || getenv("WAYLAND_DISPLAY") != ""
	}
	return false
}

// maxRedirectLine is the longest line the sign-in reads as a pasted redirect.
// A real one is a few hundred bytes (an address, a code, the scopes, the
// state and the client id); a longer line is not one, and the rest of it is
// read and dropped.
const maxRedirectLine = 16 << 10

// redirectPrompt is the sign-in's prompt for a pasted redirect, on a
// terminal only.
const redirectPrompt = "Redirect address: "

// errNoRedirect is the cause a paste-only sign-in is cancelled with when its
// stdin ends before a redirect was accepted: nothing else can finish it.
var errNoRedirect = errors.New("stdin ended before the redirect address was pasted")

// signalCause is the cause a sign-in is cancelled with when a signal ends
// it: craze exits 128 plus its number, as the key prompt's handler does.
type signalCause struct{ sig os.Signal }

func (c *signalCause) Error() string { return "interrupted by " + c.sig.String() }

// code is the exit code a shell reports for a process the signal ended.
func (c *signalCause) code() int {
	if s, ok := c.sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 128 + int(syscall.SIGINT)
}

// pasteEvent is what the stdin reader tells the sign-in of a line it read: one
// it refused, to say why (never the line); a paste accepted (acceptedPaste
// says what for); or, on a terminal, a blank one, for the prompt to be drawn
// again.
type pasteEvent struct {
	refusal string
	pasted  bool
}

// acceptedPaste is where a pasted redirect the attempt accepted led — its
// origin and path (receivedAt), never its query — for the sign-in to confirm
// on a terminal. The stdin reader holds mu across the attempt's Paste and the
// record (paste), and the sign-in reads it under mu (at): an accepted paste
// can finish the wait — its result back on the sign-in's loop — before the
// reader's event is, and the record is there all the same.
type acceptedPaste struct {
	mu sync.Mutex
	to string
}

// paste hands line to att as a pasted redirect, recording where it led if
// att accepts it.
func (p *acceptedPaste) paste(att signInAttempt, line string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := att.Paste(line)
	if err == nil {
		p.to = receivedAt(line)
	}
	return err
}

// at is where an accepted paste led, or "" when none was accepted.
func (p *acceptedPaste) at() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.to
}

// receivedText is what the sign-in prints on a terminal once a pasted
// address is accepted: where it led, never the one-time code and state its
// query carries — the prompt did not echo it (review r14 1).
func receivedText(at string) string {
	return "Received the redirect to " + at + "; signing in."
}

// signIn is `craze auth login chatgpt` (plan 033 §3.13): a whole sign-in into
// a's directory. It never reads, asks for or stores a key.
func (a *authRun) signIn() error {
	ctx, cancel := context.WithCancelCause(a.context())
	defer cancel(nil)
	sigs := make(chan os.Signal, 1)
	notifySignInSignals(sigs)
	defer stopSignInSignals(sigs)
	// On a terminal the echo goes off before anything is drawn and stays off
	// until the sign-in returns (review r14 1): whatever is typed or pasted
	// at the prompt — a key pasted out of habit included — is never
	// displayed, however soon after the prompt it comes, and the sign-in
	// says what it received instead. The signals are caught from before
	// (above) until after the echo is back — the deferred calls run in
	// reverse — and the one that ends the sign-in puts it back first.
	restoreEcho := func() {}
	if a.tty != nil {
		q, err := muteSignIn(a.tty)
		if err != nil {
			return exitf(1, "%s: turning the terminal's echo off: %v; nothing was changed", a.name, err)
		}
		restoreEcho = q.restore
		defer q.restore()
	}
	go func() {
		select {
		case s := <-sigs:
			restoreEcho()
			cancel(&signalCause{sig: s})
		case <-ctx.Done():
		}
	}()

	att, err := beginSignIn(ctx, a.dir, chatgptauth.BeginOptions{PasteOnly: a.noBrowser})
	if err != nil {
		return a.signInFailed(ctx, err)
	}
	// Ctrl-C, a failure or success: the listener is closed with every
	// connection to it before craze returns (Wait closes it too).
	defer att.Close()
	a.introduceSignIn(att)

	done := make(chan struct{})
	defer close(done)
	events := make(chan pasteEvent)
	var accepted acceptedPaste
	// The stdin reader and the wait each put the echo back if they panic
	// (restoreOnPanic): a panic on a goroutine of their own ends craze
	// without running this function's deferred restore.
	go func() {
		defer restoreOnPanic(restoreEcho)
		a.readRedirects(att, &accepted, cancel, events, done)
	}()
	type waited struct {
		res chatgptauth.Result
		err error
	}
	result := make(chan waited, 1)
	go func() {
		defer restoreOnPanic(restoreEcho)
		res, err := att.Wait(ctx)
		result <- waited{res, err}
	}()
	// open says the prompt's line is open — on a terminal, from the prompt
	// until a line read at it, whose Enter was not echoed, is answered —
	// and confirmed whether an accepted paste's confirmation is on screen.
	open, confirmed := a.tty != nil, false
	confirm := func() {
		if to := accepted.at(); a.tty != nil && to != "" && !confirmed {
			fmt.Fprintln(a.errw, receivedText(to))
			confirmed = true
		}
	}
	for {
		select {
		case ev := <-events:
			if open {
				fmt.Fprintln(a.errw)
				open = false
			}
			if ev.pasted {
				confirm()
				continue
			}
			if ev.refusal != "" {
				fmt.Fprintln(a.errw, ev.refusal)
			}
			if a.tty != nil {
				fmt.Fprint(a.errw, redirectPrompt)
				open = true
			}
		case w := <-result:
			if open {
				// The prompt's line, left open by a listener's redirect, by
				// Ctrl-C, by an address the browser's redirect beat or by a
				// paste whose result came back before its event — none of
				// them echoed — is ended first.
				fmt.Fprintln(a.errw)
			}
			// A paste that finished the wait is confirmed here when its
			// event has not been read yet (acceptedPaste).
			confirm()
			if w.err != nil {
				return a.signInFailed(ctx, w.err)
			}
			return a.signedIn(ctx, w.res)
		}
	}
}

// restoreOnPanic is deferred by each goroutine signIn starts that runs the
// attempt's code — the stdin reader and the wait (review r15 a). A panic
// that unwinds such a goroutine ends craze there and then, without running
// signIn's deferred restore, which would leave the terminal it turned the echo
// off on silent for the shell after it. So the panic is recovered just long
// enough to put the terminal back — restore does nothing a second time, and
// nothing on a sign-in with no terminal — and is raised again with the same
// value: craze still crashes, with the panic's own stack, which is still on
// the goroutine while this deferred call runs.
func restoreOnPanic(restore func()) {
	if r := recover(); r != nil {
		restore()
		panic(r)
	}
}

// introduceSignIn prints the authorization URL and how the redirect comes
// back, and opens the browser in a desktop session unless --no-browser
// said not to. All of it is on stderr, with the prompts; the URL carries no
// token (P21: no id_token_hint), so it may be copied anywhere.
func (a *authRun) introduceSignIn(att signInAttempt) {
	redirect := sanitizeLine(att.RedirectURI())
	fmt.Fprintf(a.errw, "Sign in with ChatGPT to use your ChatGPT plan in craze. Open this address in a browser and approve craze:\n\n  %s\n\n", sanitizeLine(att.URL()))
	if !a.noBrowser && guiSession(browserGOOS, a.getenv) {
		if err := openBrowser(att.URL()); err != nil {
			fmt.Fprintf(a.errw, "craze could not open a browser (%s); open the address yourself.\n", sanitizeLine(err.Error()))
		} else {
			fmt.Fprintln(a.errw, "craze opened it in your browser.")
		}
	}
	if att.Listening() {
		fmt.Fprintf(a.errw, "craze is waiting for the browser to come back to %s.\n", redirect)
		fmt.Fprintln(a.errw, "If the browser is on another machine, that page will not load there: copy its whole address from the address bar and paste it here.")
	} else {
		fmt.Fprintf(a.errw, "After you approve, the browser goes to an address starting with %s, which will not load: copy its whole address from the address bar and paste it here.\n", redirect)
	}
	if a.tty != nil {
		fmt.Fprint(a.errw, redirectPrompt)
	}
}

// readRedirects reads stdin a line at a time and hands each non-blank one to
// the attempt as a pasted redirect, until one is accepted or the attempt is
// over. A line it refuses is told to the sign-in as a reason, never quoted:
// whatever was pasted by mistake — a key included — is not repeated. One
// accepted is recorded with where it led (accepted) and told, and on a terminal
// a blank line is told too, so the prompt — whose Enter did not echo — is
// drawn again. At the end of stdin a paste-only attempt is cancelled
// (errNoRedirect), since nothing else can finish it; one with a listener goes
// on waiting for the browser. It never writes: the sign-in prints what it
// sends, until done.
func (a *authRun) readRedirects(att signInAttempt, accepted *acceptedPaste, cancel context.CancelCauseFunc, events chan<- pasteEvent, done <-chan struct{}) {
	send := func(ev pasteEvent) bool {
		select {
		case events <- ev:
			return true
		case <-done:
			return false
		}
	}
	br := bufio.NewReader(a.in)
	for {
		line, long, err := readRedirectLine(br)
		line = strings.TrimSpace(line)
		switch {
		case line != "" || long:
			// A line too long, or not an address at all, never reaches the
			// attempt: it is not a redirect, and may be a key.
			refusal := redirectRefusal(line, long, att.RedirectURI())
			if refusal == "" {
				switch perr := accepted.paste(att, line); {
				case perr == nil:
					send(pasteEvent{pasted: true})
					return
				case errors.Is(perr, chatgptauth.ErrAttemptOver):
					return
				}
				refusal = "That is not this sign-in's redirect address. " + pasteWhat(att.RedirectURI())
			}
			if !send(pasteEvent{refusal: refusal}) {
				return
			}
		case err == nil && a.tty != nil:
			if !send(pasteEvent{}) {
				return
			}
		}
		if err != nil {
			if !att.Listening() {
				cancel(errNoRedirect)
			}
			return
		}
	}
}

// readRedirectLine is br's next line, without its line ending, keeping at
// most maxRedirectLine bytes and saying whether there were more (long). err
// is io.EOF, or the read's error, once nothing more can be read.
func readRedirectLine(br *bufio.Reader) (line string, long bool, err error) {
	var b strings.Builder
	for {
		chunk, rerr := br.ReadSlice('\n')
		if b.Len()+len(chunk) > maxRedirectLine+2 {
			long = true
		} else {
			b.Write(chunk)
		}
		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue
		}
		line = strings.TrimSuffix(strings.TrimSuffix(b.String(), "\n"), "\r")
		if len(line) > maxRedirectLine {
			long = true
		}
		if long {
			line = ""
		}
		return line, long, rerr
	}
}

// redirectRefusal is why line, pasted at the sign-in, cannot be its redirect
// before the attempt is asked — too long, or not an address at all — and ""
// when it is an address the attempt must judge. It never quotes the line: a
// line that is not an address is most likely a key typed where the plan's
// sign-in goes, and is told the plan takes none (plan 033 §3.13: no API-key
// login for the ChatGPT plan).
func redirectRefusal(line string, long bool, redirect string) string {
	if long {
		return "That is too long to be the redirect address. " + pasteWhat(redirect)
	}
	if u, err := url.Parse(line); err != nil || u.Scheme == "" || u.Host == "" {
		return "That is not an address: the ChatGPT plan is funded by signing in, never by an API key. " + pasteWhat(redirect)
	}
	return ""
}

// receivedAt is the address line, an accepted paste, was for: its scheme,
// host and path — the attempt's redirect address — without the query, whose
// one-time code and state the sign-in never prints.
func receivedAt(line string) string {
	u, err := url.Parse(line)
	if err != nil {
		return ""
	}
	return sanitizeLine(u.Scheme + "://" + u.Host + u.Path)
}

// pasteWhat says what to paste instead: the address the browser was sent
// to, which starts with the attempt's redirect address.
func pasteWhat(redirect string) string {
	return fmt.Sprintf("Paste the whole address the browser was sent to; it starts with %s.", sanitizeLine(redirect))
}

// signInFailed is the sign-in's exit for err, by what ended it: a signal
// (128 plus its number), stdin's end before any redirect, the person's
// refusal in the browser, or anything else chatgptauth said (exit 1).
func (a *authRun) signInFailed(ctx context.Context, err error) error {
	cause := context.Cause(ctx)
	var sig *signalCause
	switch {
	case errors.As(cause, &sig):
		return exitf(sig.code(), "%s: the sign-in was cancelled; nothing was changed", a.name)
	case errors.Is(cause, errNoRedirect):
		return exitf(1, "%s: stdin ended before the redirect address was pasted; nothing was changed", a.name)
	case errors.Is(err, chatgptauth.ErrAccessDenied):
		return exitf(1, "%s: the sign-in was declined in the browser; nothing was changed", a.name)
	}
	return exitf(1, "%s: %s", a.name, signInErrorText(err))
}

// signInErrorText is err, from chatgptauth, as one line without the
// package's prefix.
func signInErrorText(err error) string {
	return sanitizeLine(strings.TrimPrefix(err.Error(), "chatgptauth: "))
}

// signedIn is what a finished sign-in prints (plan 033 §3.13), on stdout:
// the account, then — the first time this registration signs in with plan
// usage — the notice, then the plan's models, fetched now so the next session
// offers them. An account that did not grant plan usage is told so, and how
// to grant it: signing in again, which asks ChatGPT for consent again
// (prompt=consent, chatgptauth.Begin). A notice that cannot be recorded or a
// model list that cannot be fetched is a note: the sign-in stands, and a
// native session fetches the list when it opens.
func (a *authRun) signedIn(ctx context.Context, res chatgptauth.Result) error {
	who := "Signed in to ChatGPT"
	if res.Email != "" {
		who += " as " + sanitizeLine(res.Email)
	}
	if !res.PlanUsage {
		fmt.Fprintf(a.out, "%s, but ChatGPT plan usage is off: the account did not allow craze to use its ChatGPT plan, so the plan's models cannot be used.\n", who)
		fmt.Fprintln(a.out, "To turn it on, run craze auth login chatgpt again and allow ChatGPT plan usage when ChatGPT asks.")
		return nil
	}
	fmt.Fprintln(a.out, who+".")
	if res.ShowNotice {
		fmt.Fprintln(a.out, chatgptauth.NoticeTitle)
		fmt.Fprintln(a.out, chatgptauth.Notice)
		if err := markNoticeShown(ctx, a.dir); err != nil {
			a.note("the notice above could not be recorded as shown, so it may be shown again: " + signInErrorText(err))
		}
	}
	models, err := fetchPlanModels(ctx, a.dir)
	if err != nil {
		a.note("the plan's model list could not be fetched (" + signInErrorText(err) + "); a native session fetches it when it opens")
		var sig *signalCause
		if errors.As(context.Cause(ctx), &sig) {
			return exitf(sig.code(), "%s: interrupted; you are signed in, but the plan's model list was not fetched", a.name)
		}
		return nil
	}
	aliases := make([]string, len(models.Models))
	for i, m := range models.Models {
		aliases[i] = modeltable.ChatGPTAliasPrefix + m.Slug
	}
	if len(aliases) == 0 {
		fmt.Fprintln(a.out, "ChatGPT plan models: none listed for this account.")
	} else {
		fmt.Fprintf(a.out, "ChatGPT plan models: %s\n", sanitizeLine(strings.Join(aliases, ", ")))
	}
	return nil
}

// signOut is `craze auth logout chatgpt` (plan 033 §3.13): the refresh token
// revoked, best effort, and the tokens deleted whatever the revocation's
// outcome (chatgptauth.Logout); the registration is kept, so the next sign-in
// reuses it. An access token already issued keeps working until it expires
// (the spike: revocation ends the refresh token, not the access token), which
// the command says. A key someone wrote for the plan in providers.toml by
// hand — which funds nothing (modeltable drops it with a warning) — is removed
// too, as logout removes any provider's.
func (a *authRun) signOut(p modeltable.ProviderInfo) error {
	res, err := signOutChatGPT(a.context(), a.dir)
	switch {
	case err != nil && res.SignedIn:
		return exitf(1, "%s: signing out of ChatGPT: %s; the tokens may still be on this machine", a.name, signInErrorText(err))
	case err != nil:
		return exitf(1, "%s: signing out of ChatGPT: %s", a.name, signInErrorText(err))
	case !res.SignedIn:
		fmt.Fprintln(a.out, "Not signed in to ChatGPT.")
	case res.Revoked:
		fmt.Fprintln(a.out, "Signed out of ChatGPT: the sign-in is revoked, and its tokens are deleted from this machine.")
	default:
		fmt.Fprintln(a.out, "Signed out of ChatGPT: its tokens are deleted from this machine, but remote revocation not confirmed. You can disconnect craze in ChatGPT's settings, under Apps.")
	}
	if res.SignedIn {
		fmt.Fprintln(a.out, "An access token already issued may keep working for up to an hour, until it expires.")
	}
	if p.Stored {
		removed, err := modeltable.RemoveKey(a.dir, p.ID)
		if err != nil {
			return a.fail(err)
		}
		if removed {
			fmt.Fprintf(a.out, "Removed the key stored for %s in %s; the plan never uses one.\n", sanitizeLine(p.Name), modeltable.ProvidersFile)
		}
	}
	return nil
}

// signInStatus is the ChatGPT plan's `craze auth list` column (plan 033
// §3.13) for via, its sign-in's funding (modeltable.Providers): the account
// it is signed in as — read from the registration, never a token — or why
// it does not fund the plan.
func (a *authRun) signInStatus(via modeltable.KeySource) string {
	switch via {
	case modeltable.KeySignedIn:
		who := "signed in"
		if st, err := chatgptauth.ReadStatus(a.dir); err == nil && st.Email != "" {
			who += " as " + sanitizeLine(st.Email)
		}
		return who + " · ChatGPT plan · renews automatically"
	case modeltable.KeyPlanDisabled:
		return "plan usage disabled — run craze auth login chatgpt"
	}
	return "not signed in"
}
