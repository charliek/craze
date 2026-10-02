package chatgptauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain fences the package's tests off from OpenAI: every request goes
// through a transport that dials loopback addresses only, so a test whose
// endpoints were not overridden fails rather than reaching auth.openai.com
// or api.openai.com (common.md: fake servers only). The child process of the
// multi-process race runs under the same fence.
func TestMain(m *testing.M) {
	baseTransport = &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				return nil, fmt.Errorf("test fence: refusing to dial %s", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		DisableKeepAlives: true,
	}
	os.Exit(m.Run())
}

// testKey is the fake issuer's signing key, made once per test binary.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

const (
	testKid     = "fake-kid-1"
	testSubject = "user-fake-subject-0001"
	testEmail   = "person@example.test"
	testClient  = "oaiapp_fakeclient000000000001"
)

// pendingCode is an authorization code the fake issued and has not
// exchanged.
type pendingCode struct {
	challenge string
	redirect  string
	clientID  string
	nonce     string
}

// fakeOpenAI is an OIDC issuer, token endpoint, revocation endpoint, key set
// and model-list API in one httptest server, issuing dummy tokens. Its knobs
// are set under mu before the step they shape; its records are read under
// mu after.
type fakeOpenAI struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// Knobs.
	issueClient    string                           // the client id a registration issues
	subject, email string                           // the account
	scope          string                           // the granted scope
	exchangeErr    string                           // an OAuth error the exchange answers (400)
	refreshErr     string                           // an OAuth error a refresh answers
	refreshStatus  int                              // its status (default 400)
	expiresIn      int                              // the tokens' expires_in (default 3600)
	omitScope      bool                             // the exchange's reply has no scope field
	revokeStatus   int                              // revocation's status (default 200)
	idEdit         func(hdr, claims map[string]any) // shapes the next id_tokens
	tamperID       bool                             // breaks the id_token's signature
	accessClient   string                           // the access token's client_id claim ("" = the requester's)
	omitClient     bool                             // the redirect omits client_id even on a registration
	redirectClient string                           // the redirect's client_id on a re-login ("" = omitted)
	holdRefresh    chan struct{}                    // a refresh waits on it, when set
	refreshArrived chan struct{}                    // a refresh signals it on arrival, when set
	holdModels     chan struct{}                    // a model list request waits on it, when set
	modelsArrived  chan struct{}                    // signalled on a model list request, when set
	modelsErr      string                           // an error code the model list answers (400, {"error":{"code":…}})
	redirectPath   map[string]bool                  // these paths answer 302 to /elsewhere
	jwksURI        string                           // the discovery document's jwks_uri ("" = ours)
	models         []map[string]any
	modelsEtag     string
	hooks          map[string]func() // a test's own paths: each calls its func and answers 200

	// Records.
	n          int
	pending    map[string]pendingCode
	access     map[string]string // live access token → its client id
	refresh    map[string]string // refresh token → "live", "rotated" or "revoked"
	refreshCl  map[string]string // refresh token → its client id
	issued     []string          // every token value issued, for the leak scans
	exchanges  int
	refreshes  int
	revokes    []url.Values
	modelsGets int
	elsewhere  int
}

func newFake(t *testing.T) *fakeOpenAI {
	t.Helper()
	f := &fakeOpenAI{
		t:           t,
		issueClient: testClient,
		subject:     testSubject,
		email:       testEmail,
		scope:       scopes,
		pending:     map[string]pendingCode{},
		access:      map[string]string{},
		refresh:     map[string]string{},
		refreshCl:   map[string]string{},
		modelsEtag:  "fake-models-etag-1",
		models:      defaultModels(),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func defaultModels() []map[string]any {
	levels := func(es ...string) []any {
		var out []any
		for _, e := range es {
			out = append(out, map[string]any{"effort": e, "description": "d"})
		}
		return out
	}
	return []map[string]any{
		{"slug": "gpt-6-astra", "display_name": "GPT-6-Astra", "visibility": "list", "priority": 2, "context_window": 272000,
			"max_context_window": 872000, "input_modalities": []any{"text", "image"}, "supported_reasoning_levels": levels("low", "medium", "high", "xhigh", "max", "ultra"),
			"default_reasoning_level": "medium", "supports_parallel_tool_calls": true, "base_instructions": strings.Repeat("codex prompt ", 50)},
		{"slug": "gpt-reserve", "display_name": "GPT-Reserve", "visibility": "hide", "priority": 4, "context_window": 272000},
		{"slug": "gpt-5.6-luna", "display_name": "GPT-5.6-Luna", "visibility": "list", "priority": 9, "context_window": 272000,
			"input_modalities": []any{"text"}, "supported_reasoning_levels": levels("low", "medium"), "default_reasoning_level": "medium",
			"supports_parallel_tool_calls": false},
		{"slug": "gpt-5.6-sol", "display_name": "GPT-5.6-Sol", "visibility": "list", "priority": 5, "context_window": 272000,
			"input_modalities": []any{"text", "image"}, "supported_reasoning_levels": levels("low", "medium", "high"), "default_reasoning_level": "low"},
		{"slug": "bad slug!", "display_name": "Bad", "visibility": "list", "priority": 1},
		{"slug": "gpt-esc", "display_name": "Evil\x1b[31m", "visibility": "list", "priority": 1},
		{"slug": "codex-auto-review", "display_name": "Codex Auto Review", "visibility": "hide", "priority": 43},
	}
}

// URL is the fake's origin: the issuer override.
func (f *fakeOpenAI) URL() string { return f.srv.URL }

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// mintLocked issues a live access and refresh token for clientID. f.mu is
// held. The access token is JWT-shaped (unsigned: the docs call it opaque)
// with a client_id claim, as the real one has.
func (f *fakeOpenAI) mintLocked(clientID string) (access, refresh string) {
	f.n++
	ac := clientID
	if f.accessClient != "" {
		ac = f.accessClient
	}
	hdr, _ := json.Marshal(map[string]any{"alg": "none"})
	claims, _ := json.Marshal(map[string]any{"client_id": ac, "aud": resource, "n": f.n, "r": randHex(8)})
	access = b64(hdr) + "." + b64(claims) + ".fakesig" + randHex(4)
	refresh = fmt.Sprintf("fake-refresh-%04d-%s", f.n, randHex(12))
	f.access[access] = clientID
	f.refresh[refresh] = "live"
	f.refreshCl[refresh] = clientID
	f.issued = append(f.issued, access, refresh)
	return access, refresh
}

// mint is mintLocked for a test seeding a signed-in directory.
func (f *fakeOpenAI) mint(clientID string) (access, refresh string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mintLocked(clientID)
}

// idTokenLocked signs an id_token for clientID with nonce. f.mu is held.
func (f *fakeOpenAI) idTokenLocked(clientID, nonce string) string {
	hdr := map[string]any{"alg": "RS256", "kid": testKid, "typ": "JWT"}
	t := time.Now()
	claims := map[string]any{
		"iss": f.srv.URL, "aud": clientID, "sub": f.subject, "email": f.email, "nonce": nonce,
		"iat": t.Unix(), "exp": t.Add(time.Hour).Unix(), "auth_time": t.Unix(), "jti": randHex(8),
	}
	if f.idEdit != nil {
		f.idEdit(hdr, claims)
	}
	tok := signJWT(hdr, claims)
	if f.tamperID {
		tok = tok[:len(tok)-4] + "AAAA"
	}
	f.issued = append(f.issued, tok)
	return tok
}

// signJWT signs a JWT with testKey (RS256), whatever hdr says its alg is.
func signJWT(hdr, claims map[string]any) string {
	h, _ := json.Marshal(hdr)
	c, _ := json.Marshal(claims)
	signing := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, testKey(), crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signing + "." + b64(sig)
}

// authorize stands in for the browser: it reads the attempt's authorization
// URL as the server would, issues a code, and answers the redirect's query.
func (f *fakeOpenAI) authorize(t *testing.T, authURL string) url.Values {
	t.Helper()
	q, err := f.authorizeErr(authURL)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// authorizeErr is authorize for a goroutine that is not the test's.
func (f *fakeOpenAI) authorizeErr(authURL string) (url.Values, error) {
	u, err := url.Parse(authURL)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(authURL, f.srv.URL+authorizePath+"?") {
		return nil, errors.New("the authorization URL is not the fake's authorize endpoint")
	}
	q := u.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	clientID := q.Get("client_id")
	registering := clientID == dynamicClient
	if registering {
		clientID = f.issueClient
	}
	code := "fake-code-" + randHex(8)
	f.pending[code] = pendingCode{challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), clientID: clientID, nonce: q.Get("nonce")}
	out := url.Values{"code": {code}, "state": {q.Get("state")}, "scope": {f.scope}}
	switch {
	case registering && !f.omitClient:
		out.Set("client_id", clientID)
	case !registering && f.redirectClient != "":
		out.Set("client_id", f.redirectClient)
	}
	return out, nil
}

func (f *fakeOpenAI) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeOpenAI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	hook := f.hooks[r.URL.Path]
	f.mu.Unlock()
	if hook != nil {
		hook()
		return
	}
	f.mu.Lock()
	redirect := f.redirectPath[r.URL.Path]
	if r.URL.Path == "/elsewhere" {
		f.elsewhere++
	}
	f.mu.Unlock()
	if redirect {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
		return
	}
	switch r.URL.Path {
	case discoveryPath:
		f.mu.Lock()
		jwks := f.jwksURI
		f.mu.Unlock()
		if jwks == "" {
			jwks = f.srv.URL + "/jwks"
		}
		f.writeJSON(w, 200, map[string]any{
			"issuer": f.srv.URL, "jwks_uri": jwks,
			"revocation_endpoint":    f.srv.URL + "/api/accounts/oauth/revoke",
			"authorization_endpoint": f.srv.URL + authorizePath, "token_endpoint": f.srv.URL + tokenPath,
		})
	case "/jwks":
		pub := testKey().PublicKey
		f.writeJSON(w, 200, map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": testKid, "use": "sig", "alg": "RS256",
			"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	case tokenPath:
		f.token(w, r)
	case "/api/accounts/oauth/revoke":
		_ = r.ParseForm()
		f.mu.Lock()
		f.revokes = append(f.revokes, r.PostForm)
		status := f.revokeStatus
		if status == 0 {
			status = 200
			if _, ok := f.refresh[r.PostForm.Get("token")]; ok {
				f.refresh[r.PostForm.Get("token")] = "revoked"
			}
		}
		f.mu.Unlock()
		w.WriteHeader(status)
	case "/v1/models":
		f.mu.Lock()
		f.modelsGets++
		arrived, hold := f.modelsArrived, f.holdModels
		f.mu.Unlock()
		if arrived != nil {
			arrived <- struct{}{}
		}
		if hold != nil {
			<-hold
		}
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		_, live := f.access[tok]
		models, etag, refuse := f.models, f.modelsEtag, f.modelsErr
		f.mu.Unlock()
		if refuse != "" {
			f.writeJSON(w, 400, map[string]any{"error": map[string]any{"code": refuse, "message": "refused"}})
			return
		}
		if !live {
			f.writeJSON(w, 401, map[string]any{"error": map[string]any{"code": "invalid_api_key", "message": "no"}})
			return
		}
		w.Header().Set("X-Models-Etag", etag)
		f.writeJSON(w, 200, map[string]any{"models": models})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeOpenAI) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.writeJSON(w, 400, map[string]any{"error": "invalid_request"})
		return
	}
	form := r.PostForm
	if form.Get("resource") != resource {
		f.writeJSON(w, 400, map[string]any{"error": "invalid_target"})
		return
	}
	switch form.Get("grant_type") {
	case "authorization_code":
		f.mu.Lock()
		defer f.mu.Unlock()
		f.exchanges++
		p, ok := f.pending[form.Get("code")]
		delete(f.pending, form.Get("code"))
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		switch {
		case f.exchangeErr != "":
			f.writeJSON(w, 400, map[string]any{"error": f.exchangeErr, "error_description": "refused"})
			return
		case !ok || p.clientID != form.Get("client_id") || p.redirect != form.Get("redirect_uri") || b64(sum[:]) != p.challenge:
			f.writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
			return
		}
		access, refresh := f.mintLocked(p.clientID)
		reply := map[string]any{
			"access_token": access, "refresh_token": refresh, "id_token": f.idTokenLocked(p.clientID, p.nonce),
			"token_type": "Bearer", "expires_in": f.lifeLocked(), "scope": f.scope, "earliest_refresh_at": time.Now().Add(3240 * time.Second).Unix(),
		}
		if f.omitScope {
			delete(reply, "scope")
		}
		f.writeJSON(w, 200, reply)
	case "refresh_token":
		f.mu.Lock()
		f.refreshes++
		arrived, hold := f.refreshArrived, f.holdRefresh
		f.mu.Unlock()
		if arrived != nil {
			arrived <- struct{}{}
		}
		if hold != nil {
			<-hold
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		rt := form.Get("refresh_token")
		state, known := f.refresh[rt]
		switch {
		case f.refreshErr != "":
			status := f.refreshStatus
			if status == 0 {
				status = 400
			}
			f.writeJSON(w, status, map[string]any{"error": f.refreshErr})
			return
		case f.refreshStatus != 0:
			w.WriteHeader(f.refreshStatus)
			return
		case !known || state == "revoked":
			f.writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
			return
		case state == "rotated":
			f.writeJSON(w, 400, map[string]any{"error": "refresh_token_reused"})
			return
		case f.refreshCl[rt] != form.Get("client_id"):
			f.writeJSON(w, 401, map[string]any{"error": "invalid_client"})
			return
		}
		f.refresh[rt] = "rotated"
		access, refresh := f.mintLocked(form.Get("client_id"))
		f.writeJSON(w, 200, map[string]any{
			"access_token": access, "refresh_token": refresh, "id_token": f.idTokenLocked(form.Get("client_id"), ""),
			"token_type": "Bearer", "expires_in": f.lifeLocked(),
			"scope": f.scope, "earliest_refresh_at": time.Now().Add(3240 * time.Second).Unix(),
		})
	default:
		f.writeJSON(w, 400, map[string]any{"error": "unsupported_grant_type"})
	}
}

// lifeLocked is the tokens' expires_in. f.mu is held.
func (f *fakeOpenAI) lifeLocked() int {
	if f.expiresIn != 0 {
		return f.expiresIn
	}
	return 3600
}

// counts is the fake's request counts.
func (f *fakeOpenAI) counts() (exchanges, refreshes, revokes, models int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges, f.refreshes, len(f.revokes), f.modelsGets
}

// liveAccess says tok is an access token the fake issued.
func (f *fakeOpenAI) liveAccess(tok string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.access[tok]
	return ok
}

// refreshState is a refresh token's state at the fake.
func (f *fakeOpenAI) refreshState(rt string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refresh[rt]
}

// assertNoLeak fails if any token value the fake issued appears in any of
// texts. The message never prints the value (common.md), only which text
// held one.
func (f *fakeOpenAI) assertNoLeak(t *testing.T, texts ...string) {
	t.Helper()
	f.mu.Lock()
	issued := append([]string(nil), f.issued...)
	f.mu.Unlock()
	if len(issued) == 0 {
		t.Fatal("the leak scan has no issued token to look for; it would pass vacuously")
	}
	for i, text := range texts {
		for _, v := range issued {
			if strings.Contains(text, v) {
				t.Fatalf("text %d of the scan holds an issued token value (%d bytes)", i, len(v))
			}
		}
	}
}

// useFake points the package's endpoints at f for one test, through the
// getenv seam.
func useFake(t *testing.T, f *fakeOpenAI) {
	t.Helper()
	env := map[string]string{IssuerEnv: f.URL(), APIEnv: f.URL() + "/v1"}
	setEnv(t, env)
}

func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	getenv = func(k string) string { return env[k] }
	t.Cleanup(func() { getenv = os.Getenv })
}

// useListener replaces the listen seam: port 1455 binds a free port in its
// place unless busy1455 says another program holds it; port 0 binds a free
// port. It answers the ports the attempt asked for, in order.
func useListener(t *testing.T, busy1455 bool) *[]int {
	t.Helper()
	var asked []int
	var mu sync.Mutex
	listen = func(port int) (net.Listener, error) {
		mu.Lock()
		asked = append(asked, port)
		mu.Unlock()
		if port == callbackPort && busy1455 {
			return nil, errors.New("address already in use")
		}
		return net.Listen("tcp", "127.0.0.1:0")
	}
	t.Cleanup(func() {
		listen = func(port int) (net.Listener, error) {
			return net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
		}
	})
	return &asked
}

// nativeDir is a fresh native directory under the test's temp dir.
func nativeDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "native")
}

// signIn runs a whole sign-in into dir against f through a pasted redirect,
// and answers its result.
func signIn(t *testing.T, f *fakeOpenAI, dir string) Result {
	t.Helper()
	res, err := signInErr(f, dir)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// signInErr is signIn for a goroutine that is not the test's.
func signInErr(f *fakeOpenAI, dir string) (Result, error) {
	a, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
	if err != nil {
		return Result{}, fmt.Errorf("Begin: %w", err)
	}
	defer a.Close()
	q, err := f.authorizeErr(a.URL())
	if err != nil {
		return Result{}, err
	}
	if err := a.Paste(a.RedirectURI() + "?" + q.Encode()); err != nil {
		return Result{}, fmt.Errorf("Paste: %w", err)
	}
	res, err := a.Wait(context.Background())
	if err != nil {
		return Result{}, fmt.Errorf("Wait: %w", err)
	}
	return res, nil
}

// seedOpts shape a signed-in directory seedSignedIn writes directly.
type seedOpts struct {
	expiresIn      time.Duration // the access token's life left (default 1 h)
	earliestIn     time.Duration // earliest_refresh_at from now (0: none)
	earliestPassed bool          // earliest_refresh_at in the past
}

// seedSignedIn writes a signed-in directory — registration, host id and a
// token record of live fake tokens — as a sign-in would, without one.
func seedSignedIn(t *testing.T, f *fakeOpenAI, dir string, o seedOpts) *record {
	t.Helper()
	if o.expiresIn == 0 {
		o.expiresIn = time.Hour
	}
	if err := ensureAuthDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := writeClient(dir, Client{ClientID: testClient, Subject: testSubject, Email: testEmail, PlanUsage: true, NoticeShown: true}); err != nil {
		t.Fatal(err)
	}
	access, refresh := f.mint(testClient)
	r := &record{
		Version: recordVersion, ClientID: testClient, Issuer: f.URL(), Subject: testSubject, Email: testEmail,
		Scopes: strings.Fields(scopes), AccessToken: access, AccessExpiresAt: time.Now().Add(o.expiresIn).UTC(),
		RefreshToken: refresh, IDToken: "fake-idtoken-" + randHex(8), Incarnation: "inc-" + randHex(6), Generation: 1,
	}
	f.mu.Lock()
	f.issued = append(f.issued, r.IDToken)
	f.mu.Unlock()
	switch {
	case o.earliestIn != 0:
		r.EarliestRefreshAt = time.Now().Add(o.earliestIn).UTC()
	case o.earliestPassed:
		r.EarliestRefreshAt = time.Now().Add(-time.Minute).UTC()
	}
	if err := writeRecord(dir, r, false); err != nil {
		t.Fatal(err)
	}
	return r
}

// readRec is dir's token record, failing the test when there is none.
func readRec(t *testing.T, dir string) *record {
	t.Helper()
	r, _, err := readRecord(dir)
	if err != nil {
		t.Fatalf("reading the token record: %v", err)
	}
	return r
}

// tokenFileGone says dir's token file does not exist.
func tokenFileGone(t *testing.T, dir string) bool {
	t.Helper()
	_, err := os.Lstat(TokenFile(dir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err != nil
}

// digest is a token's fingerprint for comparisons a failure message may
// print: never the value itself.
func digest(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:6])
}
