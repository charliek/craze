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
	"time"
)

// exchangeTimeout bounds a sign-in's work after its redirect arrives: the
// code exchange, the key set and the install.
const exchangeTimeout = 30 * time.Second

// listen opens the callback listener on 127.0.0.1:port (0 is any free port):
// a seam for the tests, which cannot count on port 1455 being free.
var listen = func(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// BeginOptions are a sign-in's choices.
type BeginOptions struct {
	// PasteOnly starts no listener: the redirect can only be pasted
	// (craze auth login --no-browser, or a host the browser cannot reach).
	PasteOnly bool
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

	mu   sync.Mutex
	over bool // a redirect was accepted, or the attempt was closed

	got       chan url.Values // the accepted redirect's query; buffered, sent once
	closed    chan struct{}
	closeOnce sync.Once
	waited    atomic.Bool
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
func Begin(ctx context.Context, dir string, opts BeginOptions) (*Attempt, error) {
	ends, err := currentEndpoints()
	if err != nil {
		return nil, err
	}
	if err := ensureAuthDir(dir); err != nil {
		return nil, err
	}
	host, err := hostID(ctx, dir)
	if err != nil {
		return nil, err
	}
	// A registration file craze did not write is no registration: this
	// attempt registers anew, and its install replaces the file.
	c, err := ReadClient(dir)
	if err != nil && !errors.Is(err, errCorrupt) {
		return nil, err
	}
	a := &Attempt{
		dir:      dir,
		ends:     ends,
		register: c.ClientID == "",
		state:    randomString(24),
		nonce:    randomString(24),
		verifier: randomString(48),
		got:      make(chan url.Values, 1),
		closed:   make(chan struct{}),
	}
	if !a.register {
		a.clientID, a.subject = c.ClientID, c.Subject
	}
	port := callbackPort
	if !opts.PasteOnly {
		a.ln, port = a.openListener()
	}
	a.redirect = &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Path: callbackPath}

	sum := sha256.Sum256([]byte(a.verifier))
	q := url.Values{}
	if a.register {
		q.Set("client_id", dynamicClient)
		q.Set("agent_name_hint", agentName)
	} else {
		q.Set("client_id", a.clientID)
		if c.Email != "" {
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

	if a.ln != nil {
		a.srv = &http.Server{Handler: a, ReadHeaderTimeout: 10 * time.Second}
		a.srv.SetKeepAlivesEnabled(false)
		go func() { _ = a.srv.Serve(a.ln) }()
	}
	return a, nil
}

// openListener binds the callback listener: 1455, then — once registered —
// any free port. It answers the listener (nil when none could be bound: the
// attempt is paste-only) and the redirect's port.
func (a *Attempt) openListener() (net.Listener, int) {
	if ln, err := listen(callbackPort); err == nil {
		return ln, callbackPort
	}
	if a.register {
		return nil, callbackPort
	}
	ln, err := listen(0)
	if err != nil {
		return nil, callbackPort
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, callbackPort
	}
	return ln, addr.Port
}

// URL is the authorization URL to open in a browser. It carries no token.
func (a *Attempt) URL() string { return a.authURL }

// RedirectURI is the address the browser returns to — the one a pasted
// redirect must match.
func (a *Attempt) RedirectURI() string { return a.redirect.String() }

// Listening says a loopback listener is waiting for the redirect; false is a
// paste-only attempt.
func (a *Attempt) Listening() bool { return a.ln != nil }

// Close cancels the attempt, if it is still waiting, and closes the
// listener with every connection to it — a browser's spare connection
// included, which would otherwise swallow a later attempt's redirect (pi's
// lesson). It is safe to call more than once, and after Wait.
func (a *Attempt) Close() {
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

// Paste hands the attempt a redirect URL the person copied from the browser
// — the page that could not load, on a machine the listener is not on. It
// must be this attempt's redirect address and path, then carry its state;
// anything else is ErrRedirectMismatch and the attempt goes on waiting.
// Once a redirect has been accepted, or the attempt closed, it is
// ErrAttemptOver. An accepted one is finished by Wait.
func (a *Attempt) Paste(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != a.redirect.Scheme || u.Host != a.redirect.Host ||
		u.Path != a.redirect.Path || u.User != nil {
		return ErrRedirectMismatch
	}
	q := u.Query()
	if err := a.accept(q); err != nil {
		return err
	}
	a.got <- q
	return nil
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
	a.over = true
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
		page(w, http.StatusGone, pageOver)
		return
	case err != nil:
		page(w, http.StatusBadRequest, pageNotOurs)
		return
	}
	if q.Get("error") != "" {
		page(w, http.StatusOK, pageDeclined)
	} else {
		page(w, http.StatusOK, pageReceived)
	}
	_ = http.NewResponseController(w).Flush()
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
func (a *Attempt) Wait(ctx context.Context) (Result, error) {
	if !a.waited.CompareAndSwap(false, true) {
		return Result{}, errors.New("chatgptauth: Wait was already called on this sign-in attempt")
	}
	var q url.Values
	select {
	case q = <-a.got:
	case <-ctx.Done():
		a.Close()
		return Result{}, ctx.Err()
	case <-a.closed:
		return Result{}, ErrAttemptOver
	}
	a.Close()
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	return a.finish(ctx, q)
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
		return Result{}, errors.New("chatgptauth: the redirect carries no authorization code")
	}
	clientID := a.clientID
	got := q.Get("client_id")
	switch {
	case a.register && (got == "" || got == dynamicClient):
		return Result{}, errors.New("chatgptauth: the registration is incomplete: the redirect carries no issued client id")
	case a.register:
		clientID = got
	case got != "" && got != a.clientID:
		return Result{}, errors.New("chatgptauth: the redirect names another client id than craze's registration; nothing was changed")
	}
	if !validClientID(clientID) {
		return Result{}, errors.New("chatgptauth: the issued client id is not one craze can use")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("code", code)
	form.Set("code_verifier", a.verifier)
	form.Set("redirect_uri", a.redirect.String())
	form.Set("resource", resource)
	reply, sent, err := a.ends.postToken(ctx, "exchange", form)
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
		return Result{}, errors.New("chatgptauth: exchange: the token type is not Bearer")
	}
	if reply.AccessToken == "" {
		return Result{}, errors.New("chatgptauth: exchange: the reply has no access token")
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
		return Result{}, errors.New("chatgptauth: exchange: the access token is for another client id")
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
		return Result{}, errors.New("chatgptauth: exchange: the reply has no refresh token or no expiry")
	}

	unlock, err := lock(ctx, a.dir)
	if err != nil {
		return Result{}, fmt.Errorf("chatgptauth: taking the sign-in lock: %w", err)
	}
	defer unlock()
	prev, err := ReadClient(a.dir)
	if err != nil && !errors.Is(err, errCorrupt) {
		return Result{}, err
	}
	shown := prev.NoticeShown && prev.ClientID == clientID && prev.Subject == claims.Subject
	c := Client{ClientID: clientID, Subject: claims.Subject, Email: claims.Email, PlanUsage: plan, NoticeShown: shown}
	if err := writeClient(a.dir, c); err != nil {
		return Result{}, err
	}
	res := Result{Email: claims.Email, PlanUsage: plan, Registered: a.register, ShowNotice: plan && !shown}
	if !plan {
		return res, removeRecord(a.dir)
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
		return Result{}, err
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
