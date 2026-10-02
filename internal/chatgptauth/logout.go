package chatgptauth

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// revokeTimeout bounds sign-out's revocation, discovery included (plan 033
// §3.10: 10 s, best effort).
const revokeTimeout = 10 * time.Second

// LogoutResult is what a sign-out did. SignedIn says there were tokens to
// remove. Revoked says the server confirmed the refresh token's revocation;
// false with SignedIn means "remote revocation not confirmed" — the tokens
// are gone from this machine all the same, and the person can disconnect
// craze in ChatGPT's settings. Either way an access token already issued
// keeps working until it expires, up to an hour (the spike: revocation ends
// the refresh token, not the access token), which the caller says.
type LogoutResult struct {
	SignedIn bool
	Revoked  bool
}

// Logout signs dir out (plan 033 §3.10): under the lock, the refresh token is
// revoked at the revocation endpoint the issuer's OpenID configuration names
// (on the issuer's host only), within revokeTimeout and at most once, and
// chatgpt.json is deleted whatever the revocation's outcome. The
// registration and the host id are kept, so the next sign-in reuses them.
// Every other process's token source finds the file gone at its next request
// (ErrSignedOut) and never recreates it.
func Logout(ctx context.Context, dir string) (LogoutResult, error) {
	if _, err := os.Lstat(AuthDir(dir)); errors.Is(err, fs.ErrNotExist) {
		return LogoutResult{}, nil
	}
	unlock, err := lock(ctx, dir)
	if err != nil {
		return LogoutResult{}, fmt.Errorf("chatgptauth: taking the sign-in lock: %w", err)
	}
	defer unlock()
	rec, _, err := readRecord(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return LogoutResult{}, nil
	case err != nil && !errors.Is(err, errCorrupt):
		return LogoutResult{}, err
	}
	res := LogoutResult{SignedIn: true}
	if rec != nil {
		res.Revoked = revoke(ctx, rec) == nil
	}
	if err := removeRecord(dir); err != nil {
		return res, err
	}
	return res, nil
}

// revoke ends rec's renewable session: a form POST of the refresh token
// (token_type_hint=refresh_token) and the issued client id to the
// revocation endpoint. An empty 200 is success, for an already-invalid token
// too (the docs).
func revoke(ctx context.Context, rec *record) error {
	ends, err := currentEndpoints()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	doc, err := ends.discover(ctx)
	if err != nil {
		return err
	}
	if doc.RevocationEndpoint == "" {
		return errors.New("chatgptauth: discovery: the OpenID configuration names no revocation endpoint")
	}
	form := url.Values{}
	form.Set("token", rec.RefreshToken)
	form.Set("token_type_hint", "refresh_token")
	form.Set("client_id", rec.ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.RevocationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, _, body, err := do(ctx, "revoke", req, maxReply)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return oauthError("revoke", status, body)
	}
	return nil
}
