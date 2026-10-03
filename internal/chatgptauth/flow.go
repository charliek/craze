package chatgptauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// exchangeTimeout bounds a sign-in's work after its redirect arrives: the
// code exchange, the key set and the install. A variable only so a test can
// run a step past a short one (plan 034 A14).
var exchangeTimeout = 30 * time.Second

// listen opens the callback listener on 127.0.0.1:port (0 is any free port):
// a seam for the tests, which cannot count on port 1455 being free.
var listen = func(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// beforeRefuse runs as the listener reaches a refusal, before it is counted:
// nil in production; a test holds a refusal there while the attempt ends
// (plan 034 review r3 #5).
var beforeRefuse func(Refusal)

// BeginOptions are a sign-in's choices.
type BeginOptions struct {
	// PasteOnly starts no listener: the redirect can only be pasted
	// (craze auth login --no-browser, or a host the browser cannot reach).
	PasteOnly bool
	// Observe, when set, is told the attempt's events (plan 034 §3.3, Q6;
	// event.go): its begin — or its failure to begin — the listener's
	// refusals, the pasted lines it refused, its redirect, its one terminal
	// outcome, and what the UI reports through Report. It is called on
	// whichever goroutine the event happens on (the caller's, the listener's,
	// Wait's), never with the attempt's state locks (mu, emu) held, and must
	// not block for long: the listener's page waits for it, and so does the
	// attempt's end while a listener's refusal is being reported — the two
	// are reported in order, under the attempt's order lock (omu), so no
	// refusal is reported after the outcome (review r3 #5). For the same
	// reason it must not end the attempt (Close) from a listener's refusal.
	// Each Event is value-free.
	Observe func(Event)
}

// Attempt is one sign-in in progress (plan 033 §3.10, P21): the
// authorization URL to open, and the wait for its redirect — through the
// loopback listener, or pasted (Paste) — then the code exchange, the
// id_token's validation and the install. Begin starts one; Wait finishes it;
// Close (or Wait's context) cancels it and closes the listener.
//
// Its methods may be called from several goroutines: the CLI waits while
// another goroutine reads a pasted redirect from stdin, and the TUI waits in
// a command while its paste field calls Paste.
type Attempt struct {
	dir      string
	ends     endpoints
	register bool   // a first registration (dynamic_agent_client)
	clientID string // the saved issued client id on a re-login
	subject  string // the saved account's subject on a re-login
	state    string
	nonce    string
	verifier string
	redirect *url.URL // the exact redirect_uri, sent in both requests
	authURL  string

	ln  net.Listener // nil when paste-only
	srv *http.Server

	// id is the attempt's random id, began when Begin started, and observe
	// the observer its events go to (BeginOptions.Observe; nil: none).
	id      string
	began   time.Time
	observe func(Event)

	mu       sync.Mutex
	over     bool // a redirect was accepted, or the attempt was closed
	accepted bool // a redirect was accepted: Wait will have it

	got       chan url.Values // the accepted redirect's query; buffered, sent once
	closed    chan struct{}
	closeOnce sync.Once
	waited    atomic.Bool

	// emu guards the events' bookkeeping, apart from mu, so an event is
	// never reported with either held. waiting says Wait is between its
	// start and its outcome; closeReason is the first Close's reason
	// (closeCalled says there was one); ended says the attempt's one terminal
	// outcome has been taken (endLocked); refused counts the listener's
	// refusals by kind.
	emu         sync.Mutex
	waiting     bool
	closeCalled bool
	closeReason CloseReason
	ended       bool
	refused     map[Refusal]int
	// omu orders the listener's refusals with the attempt's end (plan 034
	// review r3 #5): a refusal is counted and, the first of its kind,
	// reported (refuse), and the end takes the counts and reports them and
	// the outcome (end), each whole under it — so once the attempt has
	// ended no refusal is counted or reported, and every one counted is in
	// the end's totals. It is taken before emu, and is the one lock held as
	// the observer is called. endc is closed once the outcome is reported
	// (Ended).
	omu  sync.Mutex
	endc chan struct{}
}

// Result is a finished sign-in. PlanUsage false means the account signed in
// without granting plan usage: nothing was installed, and the person is told
// how to enable it (plan 033 §3.10 step 4). Registered says this sign-in
// registered craze anew. ShowNotice says the one-time plan-usage notice has
// not been shown for this registration yet; the caller shows it and calls
// MarkNoticeShown.
type Result struct {
	Email      string
	PlanUsage  bool
	Registered bool
	ShowNotice bool
}

// Begin starts a sign-in into dir (the native directory). It makes the
// host id if there is none, and chooses the authorization:
//
//   - a first registration — no saved client id — asks for
//     client_id=dynamic_agent_client with agent_name_hint=craze, on
//     127.0.0.1:1455, the registration's port; if 1455 cannot be bound the
//     attempt is paste-only, its redirect still on 1455;
//   - a re-login reuses the saved issued client id with the account's email
//     as login_hint, on 1455, else any free port, else paste-only. It never
//     sends id_token_hint (P21): the URL is printed and copied, and must
//     carry no token. With plan usage off it asks for consent again
//     (prompt=consent).
//
// Every attempt sends ext_agent_host_id, a fresh state, nonce and S256 PKCE
// challenge, the API as its resource, and the full scope set. ctx bounds
// Begin alone (the host id's lock); the attempt lives until Wait returns or
// Close is called.
//
// The attempt's first event is its begin (plan 034 §3.3, Q8): listening or
// paste-only, the redirect's port, and why it is paste-only, or not on 1455
// (ReasonRequested, ReasonPortBusy, ReasonListenFailed) — reported before the
// listener takes its first request. A Begin that fails reports its attempt's
// one terminal outcome instead: failed, with the step and class, or
// cancelled when ctx ended it. A lock held past its bound is a
// StepTimeoutError naming the step (A14).
func Begin(ctx context.Context, dir string, opts BeginOptions) (*Attempt, error) {
	id, began := newAttemptID(), time.Now()
	fail := func(err error) (*Attempt, error) {
		ev := failureEvent(EventFailed, err, StepBegin)
		if ctx.Err() != nil {
			ev = Event{Kind: EventCancelled, Reason: ReasonContext}
		}
		ev.Attempt, ev.Elapsed = id, time.Since(began)
		report(opts.Observe, ev)
		return nil, err
	}
	ends, err := currentEndpoints()
	if err != nil {
		return fail(err)
	}
	if err := ensureAuthDir(dir); err != nil {
		return fail(err)
	}
	host, err := hostID(ctx, dir)
	if err != nil {
		return fail(lockTimeout(StepBegin, signInLockName, err))
	}
	// A registration file craze did not write is no registration: this
	// attempt registers anew, and its install replaces the file.
	c, err := ReadClient(dir)
	if err != nil && !errors.Is(err, errCorrupt) {
		return fail(err)
	}
	a := &Attempt{
		dir:      dir,
		ends:     ends,
		register: c.ClientID == "",
		state:    randomString(24),
		nonce:    randomString(24),
		verifier: randomString(48),
		id:       id,
		began:    began,
		observe:  opts.Observe,
		got:      make(chan url.Values, 1),
		closed:   make(chan struct{}),
		endc:     make(chan struct{}),
	}
	if !a.register {
		a.clientID, a.subject = c.ClientID, c.Subject
	}
	port, why := callbackPort, ReasonRequested
	if !opts.PasteOnly {
		a.ln, port, why = a.openListener()
	}
	a.redirect = &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Path: callbackPath}

	sum := sha256.Sum256([]byte(a.verifier))
	q := url.Values{}
	if a.register {
		q.Set("client_id", dynamicClient)
		q.Set("agent_name_hint", agentName)
	} else {
		q.Set("client_id", a.clientID)
		if loginHint(c.Email) {
			q.Set("login_hint", c.Email)
		}
		if !c.PlanUsage {
			q.Set("prompt", "consent")
		}
	}
	q.Set("ext_agent_host_id", host)
	q.Set("response_type", "code")
	q.Set("redirect_uri", a.redirect.String())
	q.Set("scope", scopes)
	q.Set("resource", resource)
	q.Set("state", a.state)
	q.Set("nonce", a.nonce)
	q.Set("code_challenge_method", "S256")
	q.Set("code_challenge", base64URL(sum[:]))
	a.authURL = ends.authorize() + "?" + q.Encode()

	ev := a.event(EventBegin)
	ev.Mode, ev.Port, ev.Reason, ev.Registration = ModePasteOnly, port, why, registrationOf(a.register)
	if a.ln != nil {
		ev.Mode = ModeListening
	}
	a.emit(ev)
	if a.ln != nil {
		a.srv = &http.Server{Handler: a, ReadHeaderTimeout: 10 * time.Second}
		a.srv.SetKeepAlivesEnabled(false)
		go func() { _ = a.srv.Serve(a.ln) }()
	}
	return a, nil
}

// maxLoginHint is the longest email sent as a re-login's login_hint: RFC
// 5321's longest path, an address's own bound.
const maxLoginHint = 254

// loginHint says email, the saved registration's, may go in the authorization
// address as its login_hint: not empty, at most maxLoginHint bytes, and one
// line of no control character. The registration file is not validated as it
// is read, so an oversized or multi-line email in it would otherwise make an
// address no clipboard takes whole (plan 034 review r4 #5a); a re-login
// without the hint signs in all the same — the person picks the account.
func loginHint(email string) bool {
	if email == "" || len(email) > maxLoginHint {
		return false
	}
	for i := 0; i < len(email); i++ {
		if c := email[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// openListener binds the callback listener: 1455, then — once registered —
// any free port. It answers the listener (nil when none could be bound: the
// attempt is paste-only), the redirect's port, and why the listener is not on
// 1455 ("" when it is): the port busy, or a bind that failed otherwise.
func (a *Attempt) openListener() (net.Listener, int, Reason) {
	ln, err := listen(callbackPort)
	if err == nil {
		return ln, callbackPort, ""
	}
	why := ReasonListenFailed
	if errors.Is(err, syscall.EADDRINUSE) {
		why = ReasonPortBusy
	}
	if a.register {
		return nil, callbackPort, why
	}
	ln, err = listen(0)
	if err != nil {
		return nil, callbackPort, ReasonListenFailed
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, callbackPort, ReasonListenFailed
	}
	return ln, addr.Port, why
}

// URL is the authorization URL to open in a browser. It carries no token.
func (a *Attempt) URL() string { return a.authURL }

// RedirectURI is the address the browser returns to — the one a pasted
// redirect must match.
func (a *Attempt) RedirectURI() string { return a.redirect.String() }

// Listening says a loopback listener is waiting for the redirect; false is a
// paste-only attempt.
func (a *Attempt) Listening() bool { return a.ln != nil }

// ID is the attempt's id: 8 random lowercase hex digits, carried by every
// event of the attempt (plan 034 §3.3).
func (a *Attempt) ID() string { return a.id }

// Ended is closed once the attempt's one terminal outcome has been reported
// to its observer — by a Close, or by Wait as it returns. A UI ending with
// the program waits on it, bounded, before it closes the sign-in log, so an
// outcome Wait is still finishing is logged (plan 034 review r3 #8a).
func (a *Attempt) Ended() <-chan struct{} { return a.endc }

// Close cancels the attempt, if it is still waiting, and closes the
// listener with every connection to it — a browser's spare connection
// included, which would otherwise swallow a later attempt's redirect (pi's
// lesson). It is safe to call more than once, and after Wait.
//
// reason is why (plan 034 §3.3). An attempt that has not ended reports its
// one terminal outcome here, cancelled with reason, before Close returns —
// when no redirect was accepted (whatever Wait is doing, it can only end
// cancelled now), or when no Wait is running. One whose Wait is finishing an
// accepted redirect reports its outcome as Wait returns: signed in or failed,
// or cancelled with this reason if the close's cancel cut the exchange short.
// One that has ended — signed in, declined, failed — reports nothing more: a
// close after a sign-in is cleanup, not a cancel. The first Close's reason is
// the one kept.
func (a *Attempt) Close(reason CloseReason) {
	a.mu.Lock()
	accepted := a.accepted
	a.over = true // no redirect is accepted after this
	a.mu.Unlock()
	a.omu.Lock()
	defer a.omu.Unlock()
	var evs []Event
	a.emu.Lock()
	if !a.closeCalled {
		a.closeCalled, a.closeReason = true, reason
	}
	if !a.ended && (!a.waiting || !accepted) {
		t := a.event(EventCancelled)
		t.Reason = Reason(a.closeReason)
		evs = a.endLocked(t)
	}
	a.emu.Unlock()
	a.shut()
	a.end(evs)
}

// shut ends the attempt's wait and closes the listener, once: Close's work,
// and Wait's own cleanup once it has its redirect or its context ends, which
// is no close a caller asked for and reports nothing.
func (a *Attempt) shut() {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.over = true
		a.mu.Unlock()
		close(a.closed)
		if a.srv != nil {
			_ = a.srv.Close()
		}
	})
}

// Report reports kind, something only the UI sees — the browser opened, or
// not, or the address copied — as the attempt's event (plan 034 §3.3). Any
// other kind is ignored: the attempt's own events are its own to report.
func (a *Attempt) Report(kind EventKind) {
	switch kind {
	case EventBrowserOpened, EventBrowserFailed, EventAddressCopied:
		a.emit(a.event(kind))
	}
}

// Observer is the attempt's observer for its model fetch
// (FetchOptions.Observe): it stamps each event with the attempt's id, so the
// fetch that follows a sign-in is the attempt's in the log. nil when the
// attempt has no observer. It holds the id and the observer alone, not the
// attempt — whose state, nonce and verifier it would otherwise keep alive
// for as long as the fetch runs.
func (a *Attempt) Observer() func(Event) {
	id, observe := a.id, a.observe
	if observe == nil {
		return nil
	}
	return func(ev Event) {
		ev.Attempt = id
		observe(ev)
	}
}

// event is a fresh event of the attempt: kind, its id and the time since it
// began.
func (a *Attempt) event(kind EventKind) Event {
	return Event{Kind: kind, Attempt: a.id, Elapsed: time.Since(a.began)}
}

// emit reports evs to the observer, in order. Never called with mu or emu
// held.
func (a *Attempt) emit(evs ...Event) {
	for _, ev := range evs {
		report(a.observe, ev)
	}
}

// end reports evs, the attempt's end (endLocked) — nothing when it is not
// the end — and then says it has ended (Ended). omu is held, emu is not.
func (a *Attempt) end(evs []Event) {
	if len(evs) == 0 {
		return
	}
	a.emit(evs...)
	close(a.endc)
}

// endLocked marks the attempt ended and answers what its end reports: for each
// kind the listener refused more than once, the count, then t, the terminal
// outcome. omu and emu are held; the caller reports them (end) once emu is
// released, omu still held.
func (a *Attempt) endLocked(t Event) []Event {
	a.ended = true
	var evs []Event
	for _, r := range []Refusal{RefusalOtherAttempt, RefusalNotOurs, RefusalOver} {
		if n := a.refused[r]; n > 1 {
			ev := a.event(EventListenerRefusals)
			ev.Refusal, ev.Status, ev.Count = r, refusalStatus(r), n
			evs = append(evs, ev)
		}
	}
	return append(evs, t)
}

// refusalStatus is the listener's page status for a refusal of kind r.
func refusalStatus(r Refusal) int {
	if r == RefusalOver {
		return http.StatusGone
	}
	return http.StatusBadRequest
}

// refuse counts the listener's refusal of kind r and reports the first of
// its kind (plan 034 Q8: a flood on 127.0.0.1:1455 is counted, not repeated;
// endLocked reports the count). The count and the report are one section
// under omu, the end's (review r3 #5): a refusal that reaches it once the
// attempt has ended is neither counted nor reported — the end's totals and
// its outcome are the last word. It answers the refusal's page status
// (refusalStatus) either way, so the page and the log cannot disagree.
func (a *Attempt) refuse(r Refusal) int {
	if beforeRefuse != nil {
		beforeRefuse(r)
	}
	a.omu.Lock()
	defer a.omu.Unlock()
	a.emu.Lock()
	if a.ended {
		a.emu.Unlock()
		return refusalStatus(r)
	}
	if a.refused == nil {
		a.refused = map[Refusal]int{}
	}
	a.refused[r]++
	first := a.refused[r] == 1
	a.emu.Unlock()
	if first {
		ev := a.event(EventListenerRefused)
		ev.Refusal, ev.Status = r, refusalStatus(r)
		a.emit(ev)
	}
	return refusalStatus(r)
}

// Paste hands the attempt a redirect URL the person copied from the browser
// — the page that could not load, on a machine the listener is not on. The
// line is judged here, for both UIs (plan 034 §3.3, Q6): one longer than
// MaxPaste, or not an address at all, is a *PasteError saying so; an address
// that is not this attempt's redirect address and path, or does not carry its
// state, is a *PasteError naming the part (Part). Every PasteError is
// ErrRedirectMismatch to errors.Is, and the attempt goes on waiting. Once a
// redirect has been accepted, or the attempt closed, it is ErrAttemptOver. An
// accepted one is finished by Wait. Each refusal is reported (paste_refused),
// never the line.
func (a *Attempt) Paste(raw string) error {
	q, err := a.pasted(raw)
	if err != nil {
		ev := a.event(EventPasteRefused)
		var pe *PasteError
		if errors.As(err, &pe) {
			ev.Refusal, ev.Part = pe.Refusal, pe.Part
		} else {
			ev.Refusal = RefusalOver
		}
		a.emit(ev)
		return err
	}
	a.handOver(q, ViaPaste)
	return nil
}

// pasted is Paste's judgement of raw: its query when it is this attempt's
// redirect and the attempt takes it, else why not. The length is raw's, as a
// UI read it, before its blanks are trimmed.
func (a *Attempt) pasted(raw string) (url.Values, error) {
	if len(raw) > MaxPaste {
		return nil, &PasteError{Refusal: RefusalTooLong}
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, &PasteError{Refusal: RefusalNotAddress}
	}
	if part := a.mismatch(u); part != "" {
		return nil, &PasteError{Refusal: RefusalMismatch, Part: part}
	}
	q := u.Query()
	switch err := a.accept(q); {
	case errors.Is(err, ErrRedirectMismatch):
		return nil, &PasteError{Refusal: RefusalMismatch, Part: PartState}
	case err != nil:
		return nil, err
	}
	return q, nil
}

// mismatch is the first part of u that is not this attempt's redirect address
// — its scheme, a user, its host or port, its path — or "" when u is the
// redirect address (whatever its query holds). The same comparison as before
// the parts had names: u's host and port, together, must be the redirect's.
func (a *Attempt) mismatch(u *url.URL) Part {
	switch {
	case u.Scheme != a.redirect.Scheme:
		return PartScheme
	case u.User != nil:
		return PartUserinfo
	case u.Host != a.redirect.Host && u.Hostname() != a.redirect.Hostname():
		return PartHost
	case u.Host != a.redirect.Host:
		return PartPort
	case u.Path != a.redirect.Path:
		return PartPath
	}
	return ""
}

// accept takes q as the attempt's redirect if it carries the attempt's state
// and none was taken before. The state is checked first, whatever else q
// holds — an error included (the sign-in docs).
func (a *Attempt) accept(q url.Values) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.over {
		return ErrAttemptOver
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(a.state)) != 1 {
		return ErrRedirectMismatch
	}
	a.over, a.accepted = true, true
	return nil
}

// The callback page's texts. None repeats anything from the request.
const (
	pageReceived = "craze received the sign-in. You can close this tab and return to craze."
	pageDeclined = "The sign-in was not completed. You can close this tab and return to craze."
	pageNotOurs  = "This is not the sign-in craze is waiting for. Start the sign-in again from craze."
	pageOver     = "craze is no longer waiting for this sign-in. You can close this tab."
	pageNotFound = "Not found."
)

// ServeHTTP is the loopback listener's one page. Every answer is sent with a
// strict CSP, no-store and no-referrer (plan 033 §3.10), and closes its
// connection; only GET of the callback path with the attempt's state is
// taken, and the page is written before the redirect is handed to Wait, so
// the listener's closing cannot cut it off.
//
// A refusal on the callback path is counted, and the first of its kind in the
// attempt reported, before its page is written (plan 034 Q8): a redirect
// carrying a code and another state (another attempt's), any other request
// without this state, and anything once the attempt is over. A request off
// the callback path — a browser's favicon, a port scan — is a 404 and never
// reported. An accepted redirect is reported once its page is on the wire,
// before Wait has it, so the redirect always precedes the outcome.
func (a *Attempt) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Connection", "close")
	if r.Method != http.MethodGet || r.URL.Path != callbackPath {
		page(w, http.StatusNotFound, pageNotFound)
		return
	}
	q := r.URL.Query()
	switch err := a.accept(q); {
	case errors.Is(err, ErrAttemptOver):
		page(w, a.refuse(RefusalOver), pageOver)
		return
	case err != nil:
		kind := RefusalNotOurs
		if q.Get("code") != "" && q.Get("state") != "" {
			kind = RefusalOtherAttempt
		}
		page(w, a.refuse(kind), pageNotOurs)
		return
	}
	if q.Get("error") != "" {
		page(w, http.StatusOK, pageDeclined)
	} else {
		page(w, http.StatusOK, pageReceived)
	}
	_ = http.NewResponseController(w).Flush()
	a.handOver(q, ViaListener)
}

// handOver reports the accepted redirect q, which came via, and hands it to
// Wait: reported first, so the redirect always precedes the outcome.
func (a *Attempt) handOver(q url.Values, via Via) {
	ev := a.event(EventRedirectReceived)
	ev.Via = via
	a.emit(ev)
	a.got <- q
}

// page writes one of the callback page's texts, with its length, so the
// whole answer is on the wire once it is flushed.
func page(w http.ResponseWriter, status int, text string) {
	body := text + "\n"
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// Wait waits for the attempt's redirect — the listener's or a pasted one —
// and finishes the sign-in: the code exchange, the id_token's validation
// (RS256 over the issuer's key set, iss, aud, exp, nonce, and on a re-login
// the saved subject), the plan scope, and the install under the lock. ctx
// cancels the wait and bounds the rest (within exchangeTimeout); a cancel
// closes the listener, as Close does. Wait may be called once.
//
// The redirect's client id (plan 033 §3.10 step 2): a first registration
// takes it from the redirect, which must carry one; a re-login keeps the
// saved one, accepting a redirect without any and refusing one that names
// another. A sign-in without plan usage installs no tokens (Result.PlanUsage
// false) and removes any there were. A refused registration
// (invalid_client) drops the saved client id, so the next attempt registers
// anew.
//
// A step that runs past exchangeTimeout is a StepTimeoutError naming it
// (plan 034 A14). Wait reports the attempt's terminal outcome as it returns
// (settle), unless a Close already has: signed in, declined or failed; or
// cancelled, with the reason of the Close whose cancel cut the exchange
// short — and when ctx ended it with no Close yet, the caller's Close, which
// follows on every way out, reports it instead.
func (a *Attempt) Wait(ctx context.Context) (Result, error) {
	if !a.waited.CompareAndSwap(false, true) {
		return Result{}, errors.New("chatgptauth: Wait was already called on this sign-in attempt")
	}
	a.emu.Lock()
	a.waiting = true
	a.emu.Unlock()
	res, err := a.wait(ctx)
	a.settle(ctx, res, err)
	return res, err
}

// wait is Wait's work.
func (a *Attempt) wait(ctx context.Context) (Result, error) {
	var q url.Values
	select {
	case q = <-a.got:
	case <-ctx.Done():
		a.shut()
		return Result{}, ctx.Err()
	case <-a.closed:
		return Result{}, ErrAttemptOver
	}
	a.shut()
	fctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	res, err := a.finish(fctx, q)
	return res, deadlineAt(ctx, err, StepExchange, func(Step) time.Duration { return exchangeTimeout })
}

// settle reports the attempt's terminal outcome as Wait returns it with res
// and err, unless one was reported already: signed in, declined in the
// browser, failed (its step, status, code, class and check), or cancelled —
// ctx done, or the attempt closed — with the Close's reason. A cancel with no
// Close yet is left to the Close.
func (a *Attempt) settle(ctx context.Context, res Result, err error) {
	a.omu.Lock()
	defer a.omu.Unlock()
	var evs []Event
	a.emu.Lock()
	a.waiting = false
	if !a.ended {
		var t Event
		switch {
		case err == nil:
			t = a.event(EventSignedIn)
			t.Usage, t.Registration = usageOf(res.PlanUsage), registrationOf(res.Registered)
		case ctx.Err() != nil || errors.Is(err, ErrAttemptOver):
			if a.closeCalled {
				t = a.event(EventCancelled)
				t.Reason = Reason(a.closeReason)
			}
		case errors.Is(err, ErrAccessDenied):
			t = a.event(EventDeclined)
			t.Step, t.Code = StepAuthorize, "access_denied"
		default:
			t = failureEvent(EventFailed, err, "")
			t.Attempt, t.Elapsed = a.id, time.Since(a.began)
		}
		if t.Kind != "" {
			evs = a.endLocked(t)
		}
	}
	a.emu.Unlock()
	a.end(evs)
}

// finish is Wait's work once the redirect is in hand.
func (a *Attempt) finish(ctx context.Context, q url.Values) (Result, error) {
	if e := q.Get("error"); e != "" {
		if e == "access_denied" {
			return Result{}, ErrAccessDenied
		}
		return Result{}, refused("authorize", 0, e)
	}
	code := q.Get("code")
	if code == "" {
		return Result{}, badRedirect("chatgptauth: the redirect carries no authorization code")
	}
	clientID := a.clientID
	got := q.Get("client_id")
	switch {
	case a.register && (got == "" || got == dynamicClient):
		return Result{}, badRedirect("chatgptauth: the registration is incomplete: the redirect carries no issued client id")
	case a.register:
		clientID = got
	case got != "" && got != a.clientID:
		return Result{}, badRedirect("chatgptauth: the redirect names another client id than craze's registration; nothing was changed")
	}
	if !validClientID(clientID) {
		return Result{}, badRedirect("chatgptauth: the issued client id is not one craze can use")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("code", code)
	form.Set("code_verifier", a.verifier)
	form.Set("redirect_uri", a.redirect.String())
	form.Set("resource", resource)
	reply, sent, err := a.ends.postToken(ctx, StepExchange, form)
	if err != nil {
		if oe := refusal(err); oe != nil && oe.Code == codeInvalidClient && !a.register {
			if derr := dropClientID(ctx, a.dir, clientID); derr != nil {
				return Result{}, fmt.Errorf("%w (and dropping the registration failed: %w)", err, derr)
			}
			return Result{}, fmt.Errorf("%w: craze's registration was refused, and is forgotten; sign in again to register anew", err)
		}
		return Result{}, err
	}
	if !strings.EqualFold(reply.TokenType, "Bearer") {
		return Result{}, badReply(StepExchange, "the token type is not Bearer")
	}
	if reply.AccessToken == "" {
		return Result{}, badReply(StepExchange, "the reply has no access token")
	}
	keys, err := a.ends.fetchJWKS(ctx)
	if err != nil {
		return Result{}, err
	}
	claims, err := verifyIDToken(reply.IDToken, keys, idWant{
		issuer: a.ends.issuer, clientID: clientID, nonce: a.nonce, subject: a.subject,
	}, now())
	if err != nil {
		return Result{}, err
	}
	if c := accessClientID(reply.AccessToken); c != "" && c != clientID {
		return Result{}, badReply(StepExchange, "the access token is for another client id")
	}
	// A reply without a scope granted what was asked (RFC 6749 §5.1: the
	// field may be left out when it is identical to the request's).
	granted := strings.Fields(scopes)
	if reply.Scope != nil {
		granted = strings.Fields(*reply.Scope)
	}
	plan := hasScope(granted, planScope)
	life, lifeOK := reply.expiresIn()
	if plan && (reply.RefreshToken == "" || !lifeOK) {
		return Result{}, badReply(StepExchange, "the reply has no refresh token or no expiry")
	}

	unlock, err := lock(ctx, a.dir)
	if err != nil {
		return Result{}, lockTimeout(StepInstall, signInLockName, fmt.Errorf("chatgptauth: taking the sign-in lock: %w", err))
	}
	defer unlock()
	prev, err := ReadClient(a.dir)
	if err != nil && !errors.Is(err, errCorrupt) {
		return Result{}, atStep(StepInstall, err)
	}
	shown := prev.NoticeShown && prev.ClientID == clientID && prev.Subject == claims.Subject
	c := Client{ClientID: clientID, Subject: claims.Subject, Email: claims.Email, PlanUsage: plan, NoticeShown: shown}
	if err := writeClient(a.dir, c); err != nil {
		return Result{}, atStep(StepInstall, err)
	}
	res := Result{Email: claims.Email, PlanUsage: plan, Registered: a.register, ShowNotice: plan && !shown}
	if !plan {
		return res, atStep(StepInstall, removeRecord(a.dir))
	}
	rec := &record{
		Version:           recordVersion,
		ClientID:          clientID,
		Issuer:            a.ends.issuer,
		Subject:           claims.Subject,
		Email:             claims.Email,
		Scopes:            granted,
		AccessToken:       reply.AccessToken,
		AccessExpiresAt:   sent.Add(life).UTC(),
		RefreshToken:      reply.RefreshToken,
		EarliestRefreshAt: reply.earliestRefresh(),
		IDToken:           reply.IDToken,
		Incarnation:       randomString(18),
		Generation:        1,
		LastRefresh:       sent.UTC(),
	}
	if err := writeRecord(a.dir, rec, false); err != nil {
		return Result{}, atStep(StepInstall, err)
	}
	return res, nil
}

// validClientID says id can be an issued client id: one to 128 of letters,
// digits and "_.:-" (the spike's was oaiapp_ and 24 more), so it is safe in
// a form, a URL and a file.
func validClientID(id string) bool { return identifier(id, 128) }

// dropClientID forgets dir's registration when the server refused it
// (invalid_client), if it is still the one refused: the next sign-in
// registers craze anew. The rest of the registration is kept.
func dropClientID(ctx context.Context, dir, refused string) error {
	unlock, err := lock(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()
	return dropClientIDLocked(dir, refused)
}

func dropClientIDLocked(dir, refused string) error {
	c, err := ReadClient(dir)
	if err != nil || c.ClientID != refused {
		return err
	}
	c.ClientID = ""
	return writeClient(dir, c)
}
