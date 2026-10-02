package chatgptauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// tokenReply is the token endpoint's answer to a code exchange or a refresh
// (the token reference: access_token, refresh_token, id_token, token_type,
// expires_in, scope, earliest_refresh_at). Scope is nil when the reply has
// none, which on a refresh keeps the grant (the docs: omit scope to retain
// it).
type tokenReply struct {
	AccessToken       string          `json:"access_token"`
	RefreshToken      string          `json:"refresh_token"`
	IDToken           string          `json:"id_token"`
	TokenType         string          `json:"token_type"`
	ExpiresIn         json.Number     `json:"expires_in"`
	Scope             *string         `json:"scope"`
	EarliestRefreshAt json.RawMessage `json:"earliest_refresh_at"`
}

// maxExpiresIn bounds a token's stated lifetime: an access token lives an
// hour (the token reference), and a reply claiming more than a day is not
// one this package understands.
const maxExpiresIn = 24 * time.Hour

// expiresIn is the reply's access-token lifetime, if it states a sane one.
func (r *tokenReply) expiresIn() (time.Duration, bool) {
	f, err := r.ExpiresIn.Float64()
	if err != nil || f <= 0 {
		return 0, false
	}
	d := time.Duration(f * float64(time.Second))
	return d, d <= maxExpiresIn
}

// earliestRefresh is the reply's earliest_refresh_at — Unix seconds (the
// spike's shape) or an RFC 3339 time — and the zero time when it has none or
// one this package cannot read: the hint is advisory (the spike refreshed
// early without harm), so a missing one costs nothing.
func (r *tokenReply) earliestRefresh() time.Time {
	if len(r.EarliestRefreshAt) == 0 {
		return time.Time{}
	}
	var n json.Number
	if json.Unmarshal(r.EarliestRefreshAt, &n) == nil {
		if t, ok := unixTime(&n); ok {
			return t.UTC()
		}
	}
	var s string
	if json.Unmarshal(r.EarliestRefreshAt, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// hasScope says scopes holds s.
func hasScope(scopes []string, s string) bool {
	for _, x := range scopes {
		if x == s {
			return true
		}
	}
	return false
}

// postToken POSTs form to the token endpoint and answers its reply and when
// the request was sent (the access token's lifetime counts from then, so its
// expiry is never later than the server's). A refusal is an *OAuthError
// carrying the reply's error code; the body itself is never kept.
func (e endpoints) postToken(ctx context.Context, step string, form url.Values) (*tokenReply, time.Time, error) {
	sent := now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.token(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, sent, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	status, _, body, err := do(ctx, step, req, maxReply)
	if err != nil {
		return nil, sent, err
	}
	if status != http.StatusOK {
		return nil, sent, oauthError(step, status, body)
	}
	var r tokenReply
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, sent, errors.New("chatgptauth: " + step + ": the reply is not a token response")
	}
	return &r, sent, nil
}

// oauthError reads an endpoint's refusal: its OAuth error code — the
// standard {"error":"<code>"}, or an {"error":{"code":…}} object — and
// nothing else of the body.
func oauthError(step string, status int, body []byte) *OAuthError {
	var v struct {
		Error json.RawMessage `json:"error"`
		Code  string          `json:"code"`
	}
	code := ""
	if json.Unmarshal(body, &v) == nil {
		var s string
		var o struct {
			Code string `json:"code"`
		}
		switch {
		case json.Unmarshal(v.Error, &s) == nil:
			code = s
		case json.Unmarshal(v.Error, &o) == nil:
			code = o.Code
		}
		if code == "" {
			code = v.Code
		}
	}
	return &OAuthError{Step: step, Status: status, Code: codeOf(code)}
}

// signInAgainCodes are the refresh refusals that mean the refresh token can
// never be used again (the errors-and-recovery docs' "unusable refresh
// token"): the tokens are deleted and only a new sign-in continues.
var signInAgainCodes = map[string]bool{
	"invalid_grant":             true,
	"invalid_refresh_token":     true,
	"token_expired":             true,
	"refresh_token_expired":     true,
	"refresh_token_invalidated": true,
	"refresh_token_reused":      true,
}

// codeInvalidClient is the refusal of the registration itself (at the
// exchange or a refresh): the saved client id is dropped, and the next
// sign-in registers craze anew.
const codeInvalidClient = "invalid_client"

// refusal is err's *OAuthError when the token endpoint refused with a client
// error (4xx) — the only refusals that end a sign-in; a 5xx keeps the
// credentials whatever its code says (the docs: never erase credentials for a
// temporary failure).
func refusal(err error) *OAuthError {
	var oe *OAuthError
	if errors.As(err, &oe) && oe.Status >= 400 && oe.Status <= 499 {
		return oe
	}
	return nil
}
