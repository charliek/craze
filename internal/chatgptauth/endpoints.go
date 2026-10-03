package chatgptauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// The fixed origins credentialed HTTP goes to (P37): sign-in, token and
// revocation on auth.openai.com, the API on api.openai.com. Nothing else is
// ever sent a credential, and no redirect is followed (httpClient).
const (
	productionIssuer = "https://auth.openai.com"
	productionAPI    = "https://api.openai.com/v1"

	authorizePath = "/api/accounts/authorize"
	tokenPath     = "/api/accounts/oauth/token"
	discoveryPath = "/.well-known/openid-configuration"
)

// The authorization request's constants (the sign-in docs' table).
const (
	// resource is the API the tokens are for. It is an identifier the
	// server checks, never a URL craze contacts, so it is the production
	// value even when the endpoints are a test's.
	resource = "https://api.openai.com/v1"
	// scopes is the full set: the identity scopes, then plan usage's. Every
	// sign-in asks for all of it — the docs' "complete requested scope set"
	// — so a person who declined plan usage can grant it by signing in again.
	scopes = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	// planScope is the grant that lets craze use the person's ChatGPT plan;
	// a sign-in without it is identity only (plan usage disabled).
	planScope = "chatgpt.tokens.use.direct"
	// dynamicClient is the first registration's client_id: the entry point,
	// never the client id to save or exchange with.
	dynamicClient = "dynamic_agent_client"
	// agentName is the agent_name_hint of a first registration: the app's
	// actual name, the same on every installation.
	agentName = "craze"
	// callbackPath and callbackPort are the loopback redirect's. Only the
	// port may vary, and only once registered (the sign-in docs).
	callbackPath = "/auth/callback"
	callbackPort = 1455
)

// The test overrides (plan 033 §3.10): a loopback http URL for the issuer
// (sign-in, token, revocation, JWKS) and for the API base, honoured for no
// other URL, so neither can be pointed at a host that would receive a
// credential. A variable that is set to anything else is an error, never
// ignored: a test that misnames its fake server must fail, not reach OpenAI.
const (
	IssuerEnv = "CRAZE_TEST_CHATGPT_ISSUER"
	APIEnv    = "CRAZE_TEST_CHATGPT_API"
)

// ErrOverride is a test override that is not a loopback http URL. The value
// is not echoed.
var ErrOverride = errors.New("chatgptauth: " + IssuerEnv + " and " + APIEnv + " may name only a loopback http URL (http://127.0.0.1:<port>)")

// errRedirect is a 3xx from any endpoint: never followed (P37).
var errRedirect = errors.New("chatgptauth: the server answered with a redirect, which craze does not follow")

// getenv is the environment the overrides are read from: a seam for tests,
// os.Getenv in production.
var getenv = os.Getenv

// endpoints is where one operation's requests go: production's fixed
// origins, or a test's loopback ones.
type endpoints struct {
	issuer string // an origin, no trailing slash
	api    string // the API base, no trailing slash
}

func (e endpoints) authorize() string { return e.issuer + authorizePath }
func (e endpoints) token() string     { return e.issuer + tokenPath }
func (e endpoints) discovery() string { return e.issuer + discoveryPath }

// currentEndpoints is production's endpoints, or the test overrides'.
func currentEndpoints() (endpoints, error) {
	e := endpoints{issuer: productionIssuer, api: productionAPI}
	if v := getenv(IssuerEnv); v != "" {
		u, err := loopbackHTTP(v, false)
		if err != nil {
			return endpoints{}, err
		}
		e.issuer = u
	}
	if v := getenv(APIEnv); v != "" {
		u, err := loopbackHTTP(v, true)
		if err != nil {
			return endpoints{}, err
		}
		e.api = u
	}
	return e, nil
}

// APIBase is the ChatGPT plan's API base for the Responses driver
// (responsesapi.Config.BaseURL): https://api.openai.com/v1, or the test
// override, under the same loopback-only rule as every other endpoint here.
func APIBase() (string, error) {
	e, err := currentEndpoints()
	if err != nil {
		return "", err
	}
	return e.api, nil
}

// loopbackHTTP is raw when it is an http URL on a loopback IP literal — not
// a name, which a resolver could send anywhere — with no user, query or
// fragment, and (unless withPath) no path; else ErrOverride.
func loopbackHTTP(raw string, withPath bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", ErrOverride
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return "", ErrOverride
	}
	if !withPath && u.Path != "" && u.Path != "/" {
		return "", ErrOverride
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// sameOrigin says a and b are absolute URLs of one scheme and host (port
// included), neither carrying a user.
func sameOrigin(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	return ua.Scheme != "" && ua.Host != "" && ua.User == nil && ub.User == nil &&
		ua.Scheme == ub.Scheme && strings.EqualFold(ua.Host, ub.Host)
}

// baseTransport carries every request: a seam for tests, which refuse any
// connection that is not to a loopback address so that no test can reach
// OpenAI; http.DefaultTransport in production.
var baseTransport http.RoundTripper = http.DefaultTransport

// httpClient is a client that never follows a redirect (P37): a 3xx is
// returned as it is, and every caller refuses it (errRedirect), so a
// credential in a request goes to its endpoint and nowhere else.
func httpClient() *http.Client {
	return &http.Client{
		Transport:     baseTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// userAgent names craze to OpenAI's endpoints.
const userAgent = "craze"

// maxReply bounds what is read of any reply but the model list's.
const maxReply = 1 << 20

// do sends req with the client and answers its body (within limit) and
// status. A redirect is errRedirect; a transport error is returned with the
// step named and the context's error preferred, so a cancel reads as one.
// Every error is labelled with step (stepError, plan 034 §3.3), which leaves
// its text as it was: the owner of the step's deadline names a timeout by it
// (deadlineAt), and the sign-in log records it.
func do(ctx context.Context, step Step, req *http.Request, limit int64) (int, http.Header, []byte, error) {
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient().Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, nil, nil, &stepError{step: step, err: ctxErr}
		}
		return 0, nil, nil, &stepError{step: step, err: fmt.Errorf("chatgptauth: %s: %w", step, transportError(err))}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		return resp.StatusCode, nil, nil, &stepError{step: step, status: resp.StatusCode, err: fmt.Errorf("chatgptauth: %s: %w", step, errRedirect)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, nil, nil, &stepError{step: step, err: ctxErr}
		}
		return 0, nil, nil, &stepError{step: step, err: fmt.Errorf("chatgptauth: %s: reading the reply: %w", step, transportError(err))}
	}
	if int64(len(body)) > limit {
		return 0, nil, nil, badReply(step, fmt.Sprintf("the reply is larger than %d bytes", limit))
	}
	return resp.StatusCode, resp.Header, body, nil
}

// transportError is err without the request URL net/http wraps it in: the
// URLs here carry no secret, but the error's own text is all a caller needs.
func transportError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// discoveryDoc is the part of the issuer's OpenID configuration craze reads.
type discoveryDoc struct {
	Issuer             string `json:"issuer"`
	JWKSURI            string `json:"jwks_uri"`
	RevocationEndpoint string `json:"revocation_endpoint"`
}

// discover fetches the issuer's OpenID configuration. Its JWKS and
// revocation endpoints are accepted only on the issuer's own origin —
// auth.openai.com, or a test's loopback issuer (P37) — and its issuer, when
// it names one, must be ours.
func (e endpoints) discover(ctx context.Context) (discoveryDoc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.discovery(), nil)
	if err != nil {
		return discoveryDoc{}, err
	}
	req.Header.Set("Accept", "application/json")
	status, _, body, err := do(ctx, StepDiscovery, req, maxReply)
	if err != nil {
		return discoveryDoc{}, err
	}
	if status != http.StatusOK {
		return discoveryDoc{}, &OAuthError{Step: "discovery", Status: status}
	}
	var doc discoveryDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return discoveryDoc{}, badReply(StepDiscovery, "the OpenID configuration is not JSON")
	}
	if doc.Issuer != "" && strings.TrimSuffix(doc.Issuer, "/") != e.issuer {
		return discoveryDoc{}, badReply(StepDiscovery, "the OpenID configuration names another issuer")
	}
	for _, u := range []string{doc.JWKSURI, doc.RevocationEndpoint} {
		if u != "" && !sameOrigin(u, e.issuer) {
			return discoveryDoc{}, badReply(StepDiscovery, "an endpoint in the OpenID configuration is not on the issuer's host")
		}
	}
	return doc, nil
}
