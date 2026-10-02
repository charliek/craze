// Package chatgptauth is Sign in with ChatGPT for craze's ChatGPT plan
// provider (plan 033 §3.10, owner decision 4): the browser sign-in that
// registers craze as the person's own client, the token store it leaves
// behind, the token source every native session's requests draw their bearer
// from, the plan's model list, and sign-out. It follows OpenAI's Sign in with
// ChatGPT docs for open-source apps, as the plan's live spike verified them
// (research/siwc-spike/FINDINGS.md).
//
// It lives outside the native harness (P31): the harness knows only the
// llm.Auth interface, which *TokenSource satisfies structurally, injected
// through harness.Options. It imports the standard library and
// internal/atomicfile alone (deps_test.go holds the line), so the CLI, the
// TUI and the agent adapter can all build on it.
//
// # The files
//
// Everything is under the native directory, <CRAZE_HOME>/native, which every
// function here takes as dir:
//
//	auth/                      0700
//	auth/host-id               urn:uuid:<v4>: this host, made once, kept for good
//	auth/chatgpt-client.json   the registration: client id, subject, email, plan usage, notice shown (not secret)
//	auth/chatgpt.json          the tokens (secret; written with atomicfile.WriteSync)
//	auth/chatgpt.json.lock     the lock every change of the two JSON files is made under
//	chatgpt-models.json        the plan's model list, bound to the account (not secret)
//
// Every file is 0600. Sign-out deletes chatgpt.json and keeps the rest, so
// the next sign-in reuses the registration (the docs' "client reuse").
//
// # Processes
//
// Every native session is its own craze serve process, and the TUI and craze
// auth are others, so the token file is shared state. A refresh rotates the
// refresh token — the server keeps only the newest, and a second use of an
// old one is refresh_token_reused, the end of the sign-in — so every change
// is made under the lock, after re-reading the file: a process that finds a
// peer's newer token adopts it rather than refreshing again (P20).
//
// # Secrets
//
// No token value, nor any part of one, is ever put in an error, a log line or
// a URL: errors name the endpoint, the HTTP status and the OAuth error code,
// never a body, and the sign-in URL carries no token (craze never sends
// id_token_hint, P21).
package chatgptauth

import (
	"errors"
	"fmt"
	"path/filepath"
)

// The names of the files, one name everywhere (plan 033 §3.10).
const (
	authDirName    = "auth"
	hostIDName     = "host-id"
	clientFileName = "chatgpt-client.json"
	tokenFileName  = "chatgpt.json"
	lockFileName   = "chatgpt.json.lock"
	modelsFileName = "chatgpt-models.json"
)

// AuthDir is dir's sign-in directory, <dir>/auth: every file of the sign-in
// but the model list. File tools refuse it whole (plan 033 §3.12).
func AuthDir(dir string) string { return filepath.Join(dir, authDirName) }

// TokenFile is the secret token record, <dir>/auth/chatgpt.json: the file a
// session's turn-start look learns redaction values from (StoredValues), and
// whose presence — a non-empty regular file — says the person is signed in.
func TokenFile(dir string) string { return filepath.Join(dir, authDirName, tokenFileName) }

// ClientFile is the registration, <dir>/auth/chatgpt-client.json, which is
// not secret: the issued client id, the account's subject and email, whether
// plan usage was granted, and whether the one-time notice was shown.
func ClientFile(dir string) string { return filepath.Join(dir, authDirName, clientFileName) }

// HostIDFile is this host's ext_agent_host_id, <dir>/auth/host-id.
func HostIDFile(dir string) string { return filepath.Join(dir, authDirName, hostIDName) }

// lockFile is the lock every change of chatgpt.json and chatgpt-client.json
// is made under, and host-id's creation.
func lockFile(dir string) string { return filepath.Join(dir, authDirName, lockFileName) }

// ModelsFile is the plan's model list as the account last fetched it,
// <dir>/chatgpt-models.json (plan 033 §3.10, P34).
func ModelsFile(dir string) string { return filepath.Join(dir, modelsFileName) }

// The answers callers tell apart. Each is a sentinel an error wraps, so
// errors.Is finds it through every layer above (the scrubber keeps them,
// plan 033 §3.12).
var (
	// ErrSignedOut: there is no token file — the person never signed in, or
	// signed out (here or in another process). The token source never
	// recreates the file.
	ErrSignedOut = errors.New("chatgptauth: not signed in to ChatGPT")

	// ErrSignInAgain: the sign-in can no longer be renewed — the server
	// refused the refresh token, or the registration — so the tokens were
	// deleted (the registration and host id kept, unless the registration
	// was the refusal) and only a new sign-in can continue.
	ErrSignInAgain = errors.New("chatgptauth: the ChatGPT sign-in is no longer valid; sign in again")

	// ErrPlanUsageDisabled: the account is signed in, but did not grant
	// craze use of its ChatGPT plan (the chatgpt.tokens.use.direct scope),
	// so there are no tokens to use; signing in again with consent enables
	// it.
	ErrPlanUsageDisabled = errors.New("chatgptauth: ChatGPT plan usage is off for craze")

	// ErrUsageLimited: the plan's usage limit was reached, and the process
	// sends no new request on it until the next turn the person starts
	// (P33; TokenSource.LatchUsageLimit).
	ErrUsageLimited = errors.New("chatgptauth: the ChatGPT plan's usage limit was reached; no new requests until the next turn you start")

	// ErrAccessDenied: the person declined the sign-in in the browser
	// (error=access_denied).
	ErrAccessDenied = errors.New("chatgptauth: the sign-in was declined in the browser")

	// ErrRedirectMismatch: a pasted redirect is not this sign-in's — another
	// address or path, or another attempt's state. The attempt goes on
	// waiting for the right one.
	ErrRedirectMismatch = errors.New("chatgptauth: that is not this sign-in's redirect address")

	// ErrAttemptOver: the attempt already has its redirect, or was closed.
	ErrAttemptOver = errors.New("chatgptauth: this sign-in attempt is over")
)

// OAuthError is a refusal from one of the sign-in's endpoints: the step
// (authorize, exchange, refresh, revoke, models), the HTTP status (0 for a
// refusal the redirect carried), and the OAuth error code when the reply had
// an identifier-like one. A reply's description and body are never kept: the
// code is the machine-readable part (the docs' "handle errors by their
// code"), and free text from the server is no place to risk a token.
type OAuthError struct {
	Step   string
	Status int
	Code   string
}

func (e *OAuthError) Error() string {
	switch {
	case e.Code != "" && e.Status != 0:
		return fmt.Sprintf("chatgptauth: %s refused (HTTP %d, %s)", e.Step, e.Status, e.Code)
	case e.Code != "":
		return fmt.Sprintf("chatgptauth: %s refused (%s)", e.Step, e.Code)
	default:
		return fmt.Sprintf("chatgptauth: %s refused (HTTP %d)", e.Step, e.Status)
	}
}

// codeOf is s when it can be an OAuth error code — one to 64 of letters,
// digits and "_.:-" — and "" otherwise, so nothing else a server sends is
// ever repeated.
func codeOf(s string) string {
	if !identifier(s, 64) {
		return ""
	}
	return s
}

// identifier says s is one to max of letters, digits and "_.:-".
func identifier(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '.' || c == ':' || c == '-':
		default:
			return false
		}
	}
	return true
}
