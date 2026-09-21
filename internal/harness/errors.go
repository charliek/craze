package harness

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/llm"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// The errors a Session returns, stable so the adapter can phrase each one
// for the user without reading provider text (plan 018 §3.7). A failed turn's
// error matches at most one of ErrEmptyStep, ErrAuth, ErrModelNotFound,
// ErrContextTooLarge and ErrBadToolCalls; any turn failure from the provider
// is also a *ProviderError, which carries the HTTP status and the provider's
// own message, cleaned. A tool's failure is never a turn's: it goes to the
// model as an error result.
var (
	// ErrNoAPIKey is modeltable's: the model's provider has no key, neither
	// in the environment nor inline. Open and SetModel return it, naming the
	// provider and the variables tried, never a value.
	ErrNoAPIKey = modeltable.ErrNoAPIKey

	// ErrUnknownModel is modeltable's: the alias is not in the model table.
	ErrUnknownModel = modeltable.ErrUnknownModel

	// ErrEmptyStep is llm's: the provider finished the step having sent
	// nothing at all, which is how Meta reports reasoning that used up the
	// whole output ceiling (D-25).
	ErrEmptyStep = llm.ErrEmptyStep

	// ErrAuth is a provider refusing the key: HTTP 401 or 403, or an error
	// the provider flagged as an authentication failure.
	ErrAuth = errors.New("harness: the provider rejected the API key")

	// ErrModelNotFound is HTTP 404: the provider does not serve the model's
	// wire id (a stale models.toml entry, D-24).
	ErrModelNotFound = errors.New("harness: the provider does not know the model")

	// ErrContextTooLarge is a provider saying the request is longer than the
	// model's context window. There is no compaction until H7, so every
	// later turn would fail the same way: the session is done, and a
	// ProviderError of this kind says so, naming the model (plan 019 §3.5).
	ErrContextTooLarge = errors.New("harness: the conversation no longer fits the model's context window; start a new session (compaction arrives with H7)")

	// ErrBadToolCalls is a provider fault: a step's tool calls had an empty
	// or a repeated call id, so their results could not be paired with them
	// — in the next request or in the transcript. Nothing in the step ran,
	// the step was not persisted, and the turn ended (plan 019 §3.5). A
	// ProviderError of this kind carries no status.
	ErrBadToolCalls = errors.New("harness: the provider sent tool calls with missing or repeated ids")

	// ErrProfileMismatch is SetModel's refusal of a model whose tool profile
	// is not the session's: a session's tools and system prompt are fixed
	// when it opens (plan 019 §3.1, Seam 2).
	ErrProfileMismatch = errors.New("harness: this model uses a different tool profile; start a new session")

	// ErrUnknownMode is SetMode's and Open's refusal of a mode that is not
	// "agent" (or ""), "plan" or "ask": the adapter resolves the user's word
	// to one of those three first (plan 023 §3.1). Nothing is changed.
	ErrUnknownMode = errors.New("harness: unknown mode")

	// ErrInTurn is Run's refusal while another Run is live on the session.
	// The adapter serializes turns itself, so it never reaches a user.
	ErrInTurn = errors.New("harness: a turn is already running")

	// ErrNotInTurn is Steer's refusal when there is no running turn to merge
	// the text into: the session is idle, the turn has settled its steers, or
	// the token names a turn that has since ended. Nothing is changed, so the
	// caller still holds the text (plan 019 §3.10).
	ErrNotInTurn = errors.New("harness: no running turn to steer")

	// ErrTooManySteers is Steer's refusal once a turn has taken steerCap
	// interjections. Every one a turn does not answer becomes a queued row, so
	// the bound is the queue's; refusing changes nothing and leaves the text
	// with the caller, as a full queue does (plan 019 §3.10).
	ErrTooManySteers = errors.New("harness: too many interjections in one turn")

	// ErrClosed is every call that would change the session after Close.
	ErrClosed = errors.New("harness: session closed")

	// ErrEmptyPrompt is Run's refusal of a prompt with nothing but
	// whitespace in it: no provider has anything to answer.
	ErrEmptyPrompt = errors.New("harness: empty prompt")
)

// maxMessageBytes bounds ProviderError.Message. A provider whose error the
// SDK cannot parse has its whole response body used as the message — an HTML
// error page from a proxy, say — and a message is shown on one line of a
// terminal, not read as a document.
const maxMessageBytes = 300

// ProviderError is a turn that failed at or on the way to the provider: an
// HTTP error, an error event in the stream, a connection that failed, or a
// response the harness could not use. Its Unwrap is ErrAuth,
// ErrModelNotFound, ErrContextTooLarge or ErrBadToolCalls when the failure is
// one of those, else nil.
//
// Message is the provider's own text, already scrubbed of the key by
// package llm, then cut down to one line of at most maxMessageBytes with
// control characters removed. It is still text from outside craze: the
// adapter sanitizes it before a terminal shows it (plan 018 §3.8).
type ProviderError struct {
	Provider   string // the model table's provider id
	Model      string // the alias the turn ran on
	StatusCode int    // the HTTP status; 0 for an error with none, such as a stream error event
	Message    string

	kind error
}

func (e *ProviderError) Error() string {
	head := "harness: provider error"
	switch {
	case e.kind == ErrContextTooLarge:
		head = fmt.Sprintf("harness: the conversation no longer fits model %q's context window; start a new session (compaction arrives with H7)", e.Model)
	case e.kind != nil:
		head = e.kind.Error()
	}
	status := ""
	if e.StatusCode != 0 {
		status = fmt.Sprintf(", HTTP %d", e.StatusCode)
	}
	msg := ""
	if e.Message != "" {
		msg = ": " + e.Message
	}
	return fmt.Sprintf("%s (provider %q, model %q%s)%s", head, e.Provider, e.Model, status, msg)
}

// Unwrap is the error's kind, so errors.Is(err, ErrAuth) works.
func (e *ProviderError) Unwrap() error { return e.kind }

// classify maps a failed turn's error, from Fantasy's agent loop, onto the
// errors above. Every error that left the model has been rebuilt by package
// llm from scrubbed values, so nothing here can carry the key; classify
// keeps only what the adapter needs, so a raw response body never travels
// further either.
//
//   - ErrEmptyStep passes through, naming the model.
//   - A *fantasy.RetryError (the step failed, was retried, and failed again)
//     is judged by its last error, the one the user would have seen.
//   - The wrapper's *llm.MidStreamError (a failure after output began) and a
//     *fantasy.ProviderError are classified the same way: by status, the
//     auth flag, and the context-too-large flag.
//   - Anything else — a connection that failed, say — is a ProviderError
//     with no status, carrying the error's text.
func classify(err error, m store.Model) error {
	if errors.Is(err, ErrEmptyStep) {
		return fmt.Errorf("harness: model %q: %w", m.Alias, ErrEmptyStep)
	}
	var re *fantasy.RetryError
	if errors.As(err, &re) && len(re.Errors) > 0 {
		err = re.Errors[len(re.Errors)-1]
	}
	pe := &ProviderError{Provider: m.Provider, Model: m.Alias}
	var mse *llm.MidStreamError
	var fpe *fantasy.ProviderError
	switch {
	case errors.As(err, &mse):
		pe.StatusCode, pe.Message = mse.StatusCode, mse.Message
		pe.kind = kindOf(mse.StatusCode, mse.AuthError, mse.IsContextTooLarge())
	case errors.As(err, &fpe):
		pe.StatusCode, pe.Message = fpe.StatusCode, fpe.Message
		if pe.Message == "" {
			pe.Message = fpe.Title
		}
		pe.kind = kindOf(fpe.StatusCode, fpe.AuthError, fpe.IsContextTooLarge())
	default:
		pe.Message = err.Error()
	}
	pe.Message = oneLine(pe.Message, maxMessageBytes)
	return pe
}

// badToolCalls is the error a turn ends with when a step's tool calls had an
// empty or repeated provider id: a provider fault, classified like any
// other, with no status.
func badToolCalls(m store.Model) error {
	return &ProviderError{
		Provider: m.Provider,
		Model:    m.Alias,
		Message:  "a tool call in the response had an empty or repeated id, so none of the response's tool calls was run",
		kind:     ErrBadToolCalls,
	}
}

// kindOf is the sentinel a provider failure matches, or nil. Authentication
// is checked first: a 401 is never also "context too large".
func kindOf(status int, auth, contextTooLarge bool) error {
	switch {
	case auth || status == 401 || status == 403:
		return ErrAuth
	case status == 404:
		return ErrModelNotFound
	case contextTooLarge:
		return ErrContextTooLarge
	}
	return nil
}

// oneLine turns s into one line of at most max bytes: invalid UTF-8 dropped,
// every run of whitespace and control characters (newlines and terminal
// escapes' ESC included) collapsed to one space, and the end cut at a rune
// boundary and marked with an ellipsis when it had to be cut.
func oneLine(s string, max int) string {
	fields := strings.FieldsFunc(strings.ToValidUTF8(s, ""), func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
	s = strings.Join(fields, " ")
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	cut := max - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}
