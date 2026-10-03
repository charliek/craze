package chatgptauth

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// deliver stands in for the browser's return to the listener: GET the
// callback path with q, on the listener's real address (the seam binds a
// free port where the redirect says 1455).
func deliver(t *testing.T, a *Attempt, path string, q url.Values) *http.Response {
	t.Helper()
	if a.ln == nil {
		t.Fatal("the attempt has no listener")
	}
	resp, err := http.Get("http://" + a.ln.Addr().String() + path + "?" + q.Encode())
	if err != nil {
		t.Fatalf("delivering the redirect: %v", err)
	}
	return resp
}

func authQuery(t *testing.T, a *Attempt) url.Values {
	t.Helper()
	u, err := url.Parse(a.URL())
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// TestFirstRegistration (plan 033 §3.10 step 1, P21, A17): with no saved
// client id the authorization asks for dynamic_agent_client with
// agent_name_hint=craze on 127.0.0.1:1455, with the host id, PKCE S256, a
// state, a nonce, the API resource and the full scope set — and no
// login_hint, id_token_hint or prompt. The listener's redirect completes it:
// the issued client id, the account and plan usage land in 0600 files under
// a 0700 directory, and the host id is the one the next attempt reuses (the
// control: a second Begin sends the same one).
func TestFirstRegistration(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	asked := useListener(t, false)
	dir := nativeDir(t)

	a, err := Begin(context.Background(), dir, BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(CloseDone)
	q := authQuery(t, a)
	host, err := os.ReadFile(HostIDFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(a.verifier))
	want := map[string]string{
		"client_id": dynamicClient, "agent_name_hint": "craze", "ext_agent_host_id": strings.TrimSpace(string(host)),
		"response_type": "code", "redirect_uri": "http://127.0.0.1:1455/auth/callback", "scope": scopes,
		"resource": "https://api.openai.com/v1", "code_challenge_method": "S256", "code_challenge": b64(sum[:]),
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("authorize %s = %q, want %q", k, q.Get(k), v)
		}
	}
	for _, k := range []string{"login_hint", "id_token_hint", "prompt"} {
		if q.Has(k) {
			t.Errorf("a first registration sent %s", k)
		}
	}
	if q.Get("state") == "" || q.Get("nonce") == "" || q.Get("state") == q.Get("nonce") {
		t.Errorf("state and nonce must be fresh, distinct values")
	}
	if !validHostID(q.Get("ext_agent_host_id")) {
		t.Errorf("the host id is not a urn:uuid")
	}
	if !a.Listening() || len(*asked) != 1 || (*asked)[0] != callbackPort {
		t.Fatalf("a first registration listened on %v, want port 1455 alone", *asked)
	}

	resp := deliver(t, a, callbackPath, f.authorize(t, a.URL()))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "craze received the sign-in") {
		t.Fatalf("callback page = %d %q", resp.StatusCode, body)
	}
	res, err := a.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Registered || !res.PlanUsage || !res.ShowNotice || res.Email != testEmail {
		t.Fatalf("result = %+v", res)
	}
	c, err := ReadClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c != (Client{ClientID: testClient, Subject: testSubject, Email: testEmail, PlanUsage: true}) {
		t.Fatalf("registration = %+v", c)
	}
	r := readRec(t, dir)
	if r.Version != 1 || r.ClientID != testClient || r.Subject != testSubject || r.Email != testEmail ||
		r.Issuer != f.URL() || r.Generation != 1 || r.Incarnation == "" || !hasScope(r.Scopes, planScope) ||
		!f.liveAccess(r.AccessToken) || f.refreshState(r.RefreshToken) != "live" || r.IDToken == "" {
		t.Fatalf("the token record is not the sign-in's")
	}
	if left := time.Until(r.AccessExpiresAt); left < 59*time.Minute || left > time.Hour {
		t.Fatalf("access expiry %s from now, want about an hour", left)
	}
	if r.EarliestRefreshAt.IsZero() {
		t.Fatal("earliest_refresh_at was not kept")
	}
	assertModes(t, dir)

	b, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	b.Close(CloseDone)
	if authQuery(t, b).Get("ext_agent_host_id") != q.Get("ext_agent_host_id") {
		t.Fatal("a second attempt sent another host id")
	}
	clientFile, _ := os.ReadFile(ClientFile(dir))
	f.assertNoLeak(t, a.URL(), b.URL(), string(body), string(clientFile), string(host), fmt.Sprintf("%+v", res))
}

// assertModes: the sign-in directory is 0700 and every file in it 0600.
func assertModes(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Stat(AuthDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("auth dir mode %v, want 0700", info.Mode().Perm())
	}
	entries, err := os.ReadDir(AuthDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		seen[e.Name()] = true
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", e.Name(), fi.Mode().Perm())
		}
	}
	for _, n := range []string{hostIDName, clientFileName, tokenFileName, lockFileName} {
		if !seen[n] {
			t.Errorf("no %s in the auth dir", n)
		}
	}
}

// TestAuthDirIsTightened: a sign-in directory made 0755 by someone else is
// made 0700; the control is a fresh one, made 0700.
func TestAuthDirIsTightened(t *testing.T) {
	dir := nativeDir(t)
	if err := os.MkdirAll(AuthDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(AuthDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureAuthDir(dir); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(AuthDir(dir)); info.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v, want 0700", info.Mode().Perm())
	}
	fresh := nativeDir(t)
	if err := ensureAuthDir(fresh); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(AuthDir(fresh)); info.Mode().Perm() != 0o700 {
		t.Fatalf("fresh mode %v, want 0700", info.Mode().Perm())
	}
	link := nativeDir(t)
	if err := os.MkdirAll(link, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), AuthDir(link)); err != nil {
		t.Fatal(err)
	}
	if err := ensureAuthDir(link); err == nil {
		t.Fatal("a symlinked auth dir was accepted")
	}
}

// TestRelogin (P21, A17): once registered, the authorization reuses the
// issued client id with the account's email as login_hint — never
// id_token_hint, though an id_token is saved, and no token value anywhere in
// the URL — on 1455 when it is free, else any free port. A first
// registration whose 1455 is taken is paste-only on 1455. Plan usage off
// asks for consent again (prompt=consent); on, it does not (the control).
func TestRelogin(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)

	busy := useListener(t, true)
	first, err := Begin(context.Background(), dir, BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Listening() || first.RedirectURI() != "http://127.0.0.1:1455/auth/callback" || len(*busy) != 1 {
		t.Fatalf("a first registration with 1455 taken: listening %v, redirect %s, tried %v; want paste-only on 1455 having tried it alone",
			first.Listening(), first.RedirectURI(), *busy)
	}
	q := f.authorize(t, first.URL())
	if err := first.Paste(first.RedirectURI() + "?" + q.Encode()); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved := readRec(t, dir)

	free := useListener(t, false)
	a, err := Begin(context.Background(), dir, BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a.Close(CloseDone)
	aq := authQuery(t, a)
	if aq.Get("client_id") != testClient || aq.Get("login_hint") != testEmail || aq.Get("redirect_uri") != "http://127.0.0.1:1455/auth/callback" {
		t.Fatalf("re-login: client_id %q, login_hint %q, redirect %q", aq.Get("client_id"), aq.Get("login_hint"), aq.Get("redirect_uri"))
	}
	for _, k := range []string{"id_token_hint", "agent_name_hint", "prompt"} {
		if aq.Has(k) {
			t.Errorf("a re-login sent %s", k)
		}
	}
	for _, v := range saved.values() {
		if strings.Contains(a.URL(), v) {
			t.Fatal("the re-login URL carries a saved token value")
		}
	}
	if len(*free) != 1 || (*free)[0] != callbackPort {
		t.Fatalf("a re-login with 1455 free tried %v", *free)
	}

	taken := useListener(t, true)
	b, err := Begin(context.Background(), dir, BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(CloseDone)
	if !b.Listening() || len(*taken) != 2 || (*taken)[1] != 0 {
		t.Fatalf("a re-login with 1455 taken tried %v, listening %v; want 1455 then any port", *taken, b.Listening())
	}
	port := b.ln.Addr().(*net.TCPAddr).Port
	if want := "http://127.0.0.1:" + strconv.Itoa(port) + "/auth/callback"; b.RedirectURI() != want || authQuery(t, b).Get("redirect_uri") != want {
		t.Fatalf("redirect = %s, want the bound port's %s", b.RedirectURI(), want)
	}
	resp := deliver(t, b, callbackPath, f.authorize(t, b.URL()))
	resp.Body.Close()
	res, err := b.Wait(context.Background())
	if err != nil {
		t.Fatalf("re-login on a free port: %v", err)
	}
	if res.Registered || !res.PlanUsage || res.ShowNotice != true {
		t.Fatalf("re-login result = %+v", res)
	}

	c, _ := ReadClient(dir)
	c.PlanUsage = false
	if err := writeClient(dir, c); err != nil {
		t.Fatal(err)
	}
	off, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	off.Close(CloseDone)
	if authQuery(t, off).Get("prompt") != "consent" {
		t.Fatal("plan usage off: the re-login did not ask for consent again")
	}
	f.assertNoLeak(t, first.URL(), a.URL(), b.URL(), off.URL())
}

// TestCallbackClientIDRules (plan 033 §3.10 step 2): a first registration
// needs the issued client id from the redirect — none, or
// dynamic_agent_client, fails with nothing saved; a re-login accepts a
// redirect without one (the saved id is exchanged) and refuses one that
// names another, leaving every file as it was. The registration with an id
// (TestFirstRegistration) is the control.
func TestCallbackClientIDRules(t *testing.T) {
	t.Run("a first registration without an issued id", func(t *testing.T) {
		for _, issued := range []string{"", dynamicClient} {
			f := newFake(t)
			useFake(t, f)
			dir := nativeDir(t)
			f.omitClient = issued == ""
			f.issueClient = dynamicClient
			if issued == "" {
				f.issueClient = testClient
			}
			a, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			q := f.authorize(t, a.URL())
			if err := a.Paste(a.RedirectURI() + "?" + q.Encode()); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "registration is incomplete") {
				t.Fatalf("issued %q: Wait = %v, want the incomplete registration", issued, err)
			}
			if ex, _, _, _ := f.counts(); ex != 0 {
				t.Fatalf("issued %q: a code was exchanged", issued)
			}
			if c, _ := ReadClient(dir); c.ClientID != "" || !tokenFileGone(t, dir) {
				t.Fatalf("issued %q: something was saved", issued)
			}
		}
	})
	t.Run("a re-login without one keeps the saved id", func(t *testing.T) {
		f := newFake(t)
		useFake(t, f)
		dir := nativeDir(t)
		signIn(t, f, dir)
		f.redirectClient = ""
		res := signIn(t, f, dir)
		if res.Registered || readRec(t, dir).ClientID != testClient {
			t.Fatalf("result %+v", res)
		}
	})
	t.Run("a re-login naming another is refused", func(t *testing.T) {
		f := newFake(t)
		useFake(t, f)
		dir := nativeDir(t)
		signIn(t, f, dir)
		before := snapshot(t, dir)
		f.redirectClient = "oaiapp_someoneelse000000000"
		a, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		q := f.authorize(t, a.URL())
		if q.Get("client_id") != f.redirectClient {
			t.Fatal("the fake did not name another client id")
		}
		if err := a.Paste(a.RedirectURI() + "?" + q.Encode()); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "another client id") {
			t.Fatalf("Wait = %v, want the other client id refused", err)
		}
		if ex, _, _, _ := f.counts(); ex != 1 {
			t.Fatalf("exchanges = %d, want only the first sign-in's", ex)
		}
		assertUnchanged(t, dir, before)
	})
}

// snapshot is the bytes of dir's two JSON files.
func snapshot(t *testing.T, dir string) [2][]byte {
	t.Helper()
	var out [2][]byte
	for i, p := range []string{ClientFile(dir), TokenFile(dir)} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = b
	}
	return out
}

func assertUnchanged(t *testing.T, dir string, before [2][]byte) {
	t.Helper()
	after := snapshot(t, dir)
	if !bytes.Equal(before[0], after[0]) || !bytes.Equal(before[1], after[1]) {
		t.Fatal("a refused sign-in changed the saved registration or tokens")
	}
}

// TestReloginRefusesAnotherAccount (plan 033 §3.10 step 3): a re-login
// whose id_token names another subject than the registration's is refused
// and changes nothing; the same subject (the control) installs.
func TestReloginRefusesAnotherAccount(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	signIn(t, f, dir)
	before := snapshot(t, dir)

	f.subject = "user-another-account-0002"
	a, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Paste(a.RedirectURI() + "?" + f.authorize(t, a.URL()).Encode()); err != nil {
		t.Fatal(err)
	}
	_, err = a.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "another account") {
		t.Fatalf("Wait = %v, want another account refused", err)
	}
	assertUnchanged(t, dir, before)
	f.assertNoLeak(t, err.Error())

	f.subject = testSubject
	signIn(t, f, dir)
	if readRec(t, dir).Generation != 1 || bytes.Equal(snapshot(t, dir)[1], before[1]) {
		t.Fatal("the same account's re-login did not install new tokens")
	}
}

// TestSignInWithoutPlanScope (plan 033 §3.10 step 4, A17b): a grant without
// chatgpt.tokens.use.direct saves plan_usage false and no tokens — and
// removes the tokens an earlier sign-in left — and the next attempt asks for
// consent. The full grant (the first sign-in) is the control.
func TestSignInWithoutPlanScope(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	if res := signIn(t, f, dir); !res.PlanUsage || tokenFileGone(t, dir) {
		t.Fatal("the full grant did not install tokens")
	}

	f.scope = "openid profile email offline_access resource.invoke"
	res := signIn(t, f, dir)
	if res.PlanUsage || res.ShowNotice || res.Email != testEmail {
		t.Fatalf("result = %+v, want plan usage off", res)
	}
	if !tokenFileGone(t, dir) {
		t.Fatal("a grant without plan usage left tokens behind")
	}
	c, _ := ReadClient(dir)
	if c.PlanUsage || c.ClientID != testClient {
		t.Fatalf("registration = %+v, want plan_usage false and the client kept", c)
	}
	st, err := ReadStatus(dir)
	if err != nil || st.SignedIn || st.PlanUsage || !st.Registered {
		t.Fatalf("status = %+v, %v", st, err)
	}
	a, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	a.Close(CloseDone)
	if authQuery(t, a).Get("prompt") != "consent" {
		t.Fatal("the next attempt did not ask for consent")
	}

	// A reply with no scope at all granted what was asked (RFC 6749 §5.1).
	f.omitScope = true
	if res := signIn(t, f, dir); !res.PlanUsage || tokenFileGone(t, dir) || !hasScope(readRec(t, dir).Scopes, planScope) {
		t.Fatalf("a reply without a scope: %+v, want the requested grant", res)
	}
}

// TestCorruptRegistrationRegistersAnew: a chatgpt-client.json craze did not
// write is no registration — the attempt registers anew and replaces it;
// the control is a valid one, which re-logs in.
func TestCorruptRegistrationRegistersAnew(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	signIn(t, f, dir)
	a, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	a.Close(CloseDone)
	if authQuery(t, a).Get("client_id") != testClient {
		t.Fatal("a valid registration did not re-log in")
	}
	if err := os.WriteFile(ClientFile(dir), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := signIn(t, f, dir); !res.Registered {
		t.Fatal("a corrupt registration was not replaced by a new one")
	}
	if c, err := ReadClient(dir); err != nil || c.ClientID != testClient {
		t.Fatalf("registration = %+v, %v", c, err)
	}
}

// TestInvalidClientAtExchange (plan 033 §3.10 step 5, A17b): a re-login
// whose exchange the server refuses with invalid_client drops the saved
// client id, so the next attempt registers anew; another refusal of the
// exchange (the control) keeps it.
func TestInvalidClientAtExchange(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	signIn(t, f, dir)

	attempt := func() error {
		a, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Paste(a.RedirectURI() + "?" + f.authorize(t, a.URL()).Encode()); err != nil {
			t.Fatal(err)
		}
		_, err = a.Wait(context.Background())
		return err
	}

	f.exchangeErr = "invalid_grant"
	if err := attempt(); refusal(err) == nil || refusal(err).Code != "invalid_grant" {
		t.Fatalf("Wait = %v, want the invalid_grant refusal", err)
	}
	if c, _ := ReadClient(dir); c.ClientID != testClient {
		t.Fatal("invalid_grant dropped the registration")
	}

	f.exchangeErr = "invalid_client"
	refused := attempt()
	if refusal(refused) == nil || refusal(refused).Code != codeInvalidClient {
		t.Fatalf("Wait = %v, want the invalid_client refusal", refused)
	}
	c, _ := ReadClient(dir)
	if c.ClientID != "" || c.Subject != testSubject {
		t.Fatalf("registration = %+v, want the client id dropped and the rest kept", c)
	}
	next, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	next.Close(CloseDone)
	if q := authQuery(t, next); q.Get("client_id") != dynamicClient || q.Get("agent_name_hint") != "craze" {
		t.Fatal("after invalid_client the next attempt does not register anew")
	}
	f.assertNoLeak(t, refused.Error())
}

// TestIDTokenValidation (plan 033 §3.10 step 3, A17): every way an id_token
// can fail its checks is refused, the good token (the control) passes, and
// so does one within the 5 s skew. An end-to-end tampered signature
// installs nothing.
func TestIDTokenValidation(t *testing.T) {
	pub := testKey().PublicKey
	keys := &jwks{keys: map[string]*rsa.PublicKey{testKid: &pub}}
	t0 := time.Unix(1_790_000_000, 0)
	want := idWant{issuer: "https://auth.example.test", clientID: testClient, nonce: "nonce-1"}
	base := func() (map[string]any, map[string]any) {
		return map[string]any{"alg": "RS256", "kid": testKid},
			map[string]any{"iss": want.issuer, "aud": testClient, "sub": testSubject, "email": testEmail,
				"nonce": "nonce-1", "exp": t0.Add(time.Hour).Unix(), "iat": t0.Unix()}
	}
	cases := []struct {
		name    string
		edit    func(h, c map[string]any)
		raw     func(string) string
		want    idWant
		at      time.Time
		problem string // "" passes
	}{
		{name: "the good token"},
		{name: "expired within the skew", at: t0.Add(time.Hour + 4*time.Second)},
		{name: "expired past the skew", at: t0.Add(time.Hour + 6*time.Second), problem: "expired"},
		{name: "not valid yet", edit: func(h, c map[string]any) { c["nbf"] = t0.Add(10 * time.Minute).Unix() }, problem: "not valid yet"},
		{name: "a forged signature", raw: func(s string) string { return s[:len(s)-6] + "AAAAAA" }, problem: "signature"},
		{name: "HS256", edit: func(h, c map[string]any) { h["alg"] = "HS256" }, problem: "RS256"},
		{name: "alg none", edit: func(h, c map[string]any) { h["alg"] = "none" }, problem: "RS256"},
		{name: "an unknown key", edit: func(h, c map[string]any) { h["kid"] = "other" }, problem: "no key"},
		{name: "another issuer", edit: func(h, c map[string]any) { c["iss"] = "https://evil.example.test" }, problem: "issuer"},
		{name: "another audience", edit: func(h, c map[string]any) { c["aud"] = "oaiapp_other" }, problem: "client id"},
		{name: "several audiences, no azp", edit: func(h, c map[string]any) { c["aud"] = []any{testClient, "x"} }, problem: "client id"},
		{name: "several audiences, azp ours", edit: func(h, c map[string]any) { c["aud"] = []any{testClient, "x"}; c["azp"] = testClient }},
		{name: "another nonce", edit: func(h, c map[string]any) { c["nonce"] = "nonce-2" }, problem: "nonce"},
		{name: "no expiry", edit: func(h, c map[string]any) { delete(c, "exp") }, problem: "expiry"},
		{name: "no subject", edit: func(h, c map[string]any) { delete(c, "sub") }, problem: "no account"},
		{name: "another subject on a re-login", want: idWant{issuer: want.issuer, clientID: testClient, nonce: "nonce-1", subject: "someone-else"}, problem: "another account"},
		{name: "the same subject on a re-login", want: idWant{issuer: want.issuer, clientID: testClient, nonce: "nonce-1", subject: testSubject}},
		{name: "not a JWT", raw: func(string) string { return "fake-not-a-jwt" }, problem: "not a signed JWT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, c := base()
			if tc.edit != nil {
				tc.edit(h, c)
			}
			raw := signJWT(h, c)
			if tc.raw != nil {
				raw = tc.raw(raw)
			}
			w := want
			if tc.want != (idWant{}) {
				w = tc.want
			}
			at := t0.Add(time.Minute)
			if !tc.at.IsZero() {
				at = tc.at
			}
			claims, err := verifyIDToken(raw, keys, w, at)
			switch {
			case tc.problem == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.problem == "" && (claims.Subject != testSubject || claims.Email != testEmail):
				t.Fatalf("claims = %+v", claims)
			case tc.problem != "" && (err == nil || !strings.Contains(err.Error(), tc.problem)):
				t.Fatalf("err = %v, want one naming %q", err, tc.problem)
			case tc.problem != "" && strings.Contains(err.Error(), raw):
				t.Fatal("the error carries the token")
			}
		})
	}

	t.Run("end to end, a tampered signature installs nothing", func(t *testing.T) {
		f := newFake(t)
		useFake(t, f)
		dir := nativeDir(t)
		f.tamperID = true
		a, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Paste(a.RedirectURI() + "?" + f.authorize(t, a.URL()).Encode()); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("Wait = %v", err)
		}
		if c, _ := ReadClient(dir); c.ClientID != "" || !tokenFileGone(t, dir) {
			t.Fatal("a sign-in with a forged id_token saved something")
		}
	})
}

// TestPastedRedirect (plan 033 §3.10 step 2, A17): a pasted redirect must be
// this attempt's address and path, then carry its state; each mismatch is
// ErrRedirectMismatch and the attempt goes on waiting, so the right paste
// (the control) still completes it — after which any paste is
// ErrAttemptOver.
func TestPastedRedirect(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	a, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(CloseDone)
	q := f.authorize(t, a.URL())
	wrongState := url.Values{}
	for k, v := range q {
		wrongState[k] = v
	}
	wrongState.Set("state", "another-attempts-state")
	for _, bad := range []string{
		"http://localhost:1455/auth/callback?" + q.Encode(),
		"http://127.0.0.1:1456/auth/callback?" + q.Encode(),
		"http://127.0.0.1:1455/callback?" + q.Encode(),
		"https://127.0.0.1:1455/auth/callback?" + q.Encode(),
		"http://u@127.0.0.1:1455/auth/callback?" + q.Encode(),
		"http://127.0.0.1:1455/auth/callback?" + wrongState.Encode(),
		"not a url at all %%",
		"",
	} {
		if err := a.Paste(bad); !errors.Is(err, ErrRedirectMismatch) {
			t.Fatalf("paste %d = %v, want ErrRedirectMismatch", len(bad), err)
		}
	}
	if err := a.Paste("  " + a.RedirectURI() + "?" + q.Encode() + "\n"); err != nil {
		t.Fatalf("the right paste = %v", err)
	}
	if err := a.Paste(a.RedirectURI() + "?" + q.Encode()); !errors.Is(err, ErrAttemptOver) {
		t.Fatalf("a second paste = %v, want ErrAttemptOver", err)
	}
	if _, err := a.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestListenerChecksPathAndState: the listener answers 404 off the callback
// path and 400 to another attempt's state, and goes on waiting; the right
// redirect (the control) is taken. Every answer carries the strict headers
// (CSP, no-store, no-referrer) and none repeats the code or the state.
func TestListenerChecksPathAndState(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	useListener(t, false)
	dir := nativeDir(t)
	a, err := Begin(context.Background(), dir, BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(CloseDone)
	q := f.authorize(t, a.URL())
	bad := url.Values{"code": {q.Get("code")}, "state": {"not-ours"}}
	check := func(resp *http.Response, status int) {
		t.Helper()
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != status {
			t.Fatalf("status %d, want %d", resp.StatusCode, status)
		}
		h := resp.Header
		if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") || h.Get("Cache-Control") != "no-store" ||
			h.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("headers = %v", h)
		}
		if strings.Contains(string(body), q.Get("code")) || strings.Contains(string(body), q.Get("state")) {
			t.Fatal("the page repeats the redirect's code or state")
		}
	}
	check(deliver(t, a, "/elsewhere", q), 404)
	check(deliver(t, a, callbackPath, bad), 400)
	check(deliver(t, a, callbackPath, q), 200)
	check(deliver(t, a, callbackPath, q), http.StatusGone)
	if _, err := a.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestAccessDenied (plan 033 §3.10 step 2): a redirect with
// error=access_denied and the attempt's state stops the attempt with
// ErrAccessDenied and no exchange; the same error under another state is
// not this attempt's (ErrRedirectMismatch), which is the control.
func TestAccessDenied(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	a, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	state := authQuery(t, a).Get("state")
	denied := url.Values{"error": {"access_denied"}, "state": {"forged"}}
	if err := a.Paste(a.RedirectURI() + "?" + denied.Encode()); !errors.Is(err, ErrRedirectMismatch) {
		t.Fatalf("a denial under another state = %v", err)
	}
	denied.Set("state", state)
	if err := a.Paste(a.RedirectURI() + "?" + denied.Encode()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Wait(context.Background()); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("Wait = %v, want ErrAccessDenied", err)
	}
	if ex, _, _, _ := f.counts(); ex != 0 {
		t.Fatal("a declined sign-in exchanged a code")
	}
	if c, _ := ReadClient(dir); c.ClientID != "" {
		t.Fatal("a declined sign-in saved a registration")
	}
}

// TestListenerClosesAfterTheRedirect (pi's lesson, plan 033 §3.10 step 2):
// once the redirect is taken, the listener is closed with every connection
// to it — a browser's idle spare connection included. The control: before
// the redirect that connection is open (a read times out rather than
// ending).
func TestListenerClosesAfterTheRedirect(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	useListener(t, false)
	dir := nativeDir(t)
	a, err := Begin(context.Background(), dir, BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	addr := a.ln.Addr().String()
	spare, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer spare.Close()
	_ = spare.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	var ne net.Error
	if _, err := spare.Read(make([]byte, 1)); !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("before the redirect the spare connection read %v, want a timeout (open)", err)
	}
	resp := deliver(t, a, callbackPath, f.authorize(t, a.URL()))
	resp.Body.Close()
	if _, err := a.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = spare.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := spare.Read(make([]byte, 1)); err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("after the redirect the spare connection read %v, want it closed", err)
	}
	if c, err := net.Dial("tcp", addr); err == nil {
		c.Close()
		t.Fatal("the listener still accepts connections after the redirect")
	}
}

// TestListenerClosesOnCancel (plan 033 §3.10): a cancelled Wait returns the
// context's error and closes the listener; Close does the same for a Wait
// in progress (ErrAttemptOver). The control: the listener accepts before.
func TestListenerClosesOnCancel(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	useListener(t, false)
	for _, how := range []string{"cancel", "close"} {
		t.Run(how, func(t *testing.T) {
			dir := nativeDir(t)
			a, err := Begin(context.Background(), dir, BeginOptions{})
			if err != nil {
				t.Fatal(err)
			}
			addr := a.ln.Addr().String()
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("the listener refused before the cancel: %v", err)
			}
			c.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := a.Wait(ctx)
				done <- err
			}()
			want := context.Canceled
			if how == "cancel" {
				cancel()
			} else {
				a.Close(CloseDone)
				want = ErrAttemptOver
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("Wait = %v, want %v", err, want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Wait did not return")
			}
			if c, err := net.Dial("tcp", addr); err == nil {
				c.Close()
				t.Fatal("the listener still accepts after the cancel")
			}
			if err := a.Paste(a.RedirectURI() + "?" + f.authorize(t, a.URL()).Encode()); !errors.Is(err, ErrAttemptOver) {
				t.Fatalf("a paste after the cancel = %v", err)
			}
		})
	}
}

// TestRedirectsRefused (P37): a token endpoint answering 302 fails the
// exchange and its target is never asked; the model list's 302 fails the
// fetch the same way. The controls: the same steps without the redirect
// succeed.
func TestRedirectsRefused(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	f.redirectPath = map[string]bool{tokenPath: true}
	a, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Paste(a.RedirectURI() + "?" + f.authorize(t, a.URL()).Encode()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Wait(context.Background()); !errors.Is(err, errRedirect) {
		t.Fatalf("Wait = %v, want the redirect refused", err)
	}
	f.redirectPath = map[string]bool{"/v1/models": true}
	signIn(t, f, dir)
	src := newSource(dir)
	if _, err := FetchModels(context.Background(), src, FetchOptions{ClientVersion: testPin}); !errors.Is(err, errRedirect) {
		t.Fatalf("FetchModels = %v, want the redirect refused", err)
	}
	f.mu.Lock()
	hits := f.elsewhere
	f.redirectPath = nil
	f.mu.Unlock()
	if hits != 0 {
		t.Fatalf("a redirect target was asked %d times", hits)
	}
	if _, err := FetchModels(context.Background(), src, FetchOptions{ClientVersion: testPin}); err != nil {
		t.Fatalf("FetchModels without the redirect = %v", err)
	}
}

// TestNonAPIHostRefused (P37): the bearer is sent only to the API's origin —
// a request built for another host is refused before anything is sent — and
// a discovery document naming a key set or revocation endpoint off the
// issuer's host is refused, that host never asked. The controls: the API's
// own URL is sent, and the issuer's own key set validates.
func TestNonAPIHostRefused(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	other := newFake(t)
	ends, err := currentEndpoints()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ends.apiGET(context.Background(), other.URL()+"/v1/models", "fake-bearer-value"); !errors.Is(err, errOffAPI) {
		t.Fatalf("a bearer request to another host = %v, want errOffAPI", err)
	}
	if _, _, _, err := ends.apiGET(context.Background(), "https://api.openai.com.evil.test/v1/models", "fake-bearer-value"); !errors.Is(err, errOffAPI) {
		t.Fatalf("a bearer request to a look-alike host = %v, want errOffAPI", err)
	}
	if status, _, _, err := ends.apiGET(context.Background(), f.URL()+"/v1/models?client_version="+testPin, "fake-bearer-value"); err != nil || status != 401 {
		t.Fatalf("the API's own URL: %d, %v; want the fake's 401", status, err)
	}
	if _, _, _, _, m := other.countsAll(); m != 0 {
		t.Fatal("the other host was asked")
	}

	dir := nativeDir(t)
	f.jwksURI = other.URL() + "/jwks"
	a, err := Begin(context.Background(), dir, BeginOptions{PasteOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Paste(a.RedirectURI() + "?" + f.authorize(t, a.URL()).Encode()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "not on the issuer's host") {
		t.Fatalf("Wait = %v, want the foreign key set refused", err)
	}
	if n, _, _, _, _ := other.countsAll(); n != 0 {
		t.Fatal("the foreign key set's host was asked")
	}
	f.jwksURI = ""
	signIn(t, f, dir)
}

// countsAll is every request the fake answered, by path family.
func (f *fakeOpenAI) countsAll() (any, exchanges, refreshes, revokes, models int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges + f.refreshes + len(f.revokes) + f.modelsGets + f.elsewhere, f.exchanges, f.refreshes, len(f.revokes), f.modelsGets
}

// TestOverridesAreLoopbackOnly (plan 033 §3.10): CRAZE_TEST_CHATGPT_ISSUER
// and _API are honoured only for a loopback http URL on an IP literal;
// anything else is ErrOverride — never ignored — so a sign-in fails before
// any request. Unset, the endpoints are production's.
func TestOverridesAreLoopbackOnly(t *testing.T) {
	setEnv(t, map[string]string{})
	e, err := currentEndpoints()
	if err != nil || e.issuer != "https://auth.openai.com" || e.api != "https://api.openai.com/v1" {
		t.Fatalf("unset: %+v, %v", e, err)
	}
	if b, err := APIBase(); err != nil || b != "https://api.openai.com/v1" {
		t.Fatalf("APIBase unset = %q, %v", b, err)
	}
	good := map[string][2]string{
		"http://127.0.0.1:8080":  {"http://127.0.0.1:8080", "http://127.0.0.1:8080/v1"},
		"http://127.0.0.1:8080/": {"http://127.0.0.1:8080", "http://127.0.0.1:8080/v1"},
		"http://[::1]:9":         {"http://[::1]:9", "http://[::1]:9/v1"},
		"http://127.1.2.3:443":   {"http://127.1.2.3:443", "http://127.1.2.3:443/v1"},
	}
	for issuer, want := range good {
		setEnv(t, map[string]string{IssuerEnv: issuer, APIEnv: want[1]})
		e, err := currentEndpoints()
		if err != nil || e.issuer != want[0] || e.api != want[1] {
			t.Fatalf("%s: %+v, %v", issuer, e, err)
		}
	}
	for _, bad := range []string{
		"https://127.0.0.1:8080", "http://localhost:8080", "http://example.com", "http://127.0.0.1.evil.test",
		"http://10.0.0.1:80", "http://user@127.0.0.1:80", "http://127.0.0.1:80/path", "http://127.0.0.1:80?x=1",
		"http://127.0.0.1:80#f", "ftp://127.0.0.1", "127.0.0.1:80", "http://0.0.0.0:80",
	} {
		setEnv(t, map[string]string{IssuerEnv: bad})
		if _, err := currentEndpoints(); !errors.Is(err, ErrOverride) {
			t.Errorf("issuer %q: %v, want ErrOverride", bad, err)
		}
		if _, err := Begin(context.Background(), nativeDir(t), BeginOptions{PasteOnly: true}); !errors.Is(err, ErrOverride) {
			t.Errorf("Begin with issuer %q: %v, want ErrOverride", bad, err)
		}
	}
	for _, bad := range []string{"https://api.openai.com/v1", "http://localhost/v1", "http://example.com/v1"} {
		setEnv(t, map[string]string{APIEnv: bad})
		if _, err := APIBase(); !errors.Is(err, ErrOverride) {
			t.Errorf("api %q: %v, want ErrOverride", bad, err)
		}
	}
	setEnv(t, map[string]string{APIEnv: "http://127.0.0.1:1/some/path"})
	if b, err := APIBase(); err != nil || b != "http://127.0.0.1:1/some/path" {
		t.Fatalf("an API override with a path = %q, %v", b, err)
	}
}

// TestHostIDIsKept: a host-id file holding something else is an error, never
// replaced; the control is the made one, read back unchanged.
func TestHostIDIsKept(t *testing.T) {
	dir := nativeDir(t)
	id, err := hostID(context.Background(), dir)
	if err != nil || !validHostID(id) {
		t.Fatalf("hostID = %q, %v", id, err)
	}
	if again, err := hostID(context.Background(), dir); err != nil || again != id {
		t.Fatal("the host id changed")
	}
	if err := os.WriteFile(HostIDFile(dir), []byte("someone@example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hostID(context.Background(), dir); err == nil {
		t.Fatal("a host-id file of another shape was accepted")
	}
	if b, _ := os.ReadFile(HostIDFile(dir)); string(b) != "someone@example.test\n" {
		t.Fatal("a bad host-id file was replaced")
	}
}
