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
	// model's context window, and the turn could not recover from it (plan
	// 028 §3.12): a turn compacts once on an overflow and tries again, so a
	// turn fails with it on a second overflow, when that compaction failed,
	// or when there was nothing stored to compact — a new session's first
	// request, whose prompt alone is too large — or no request left to retry
	// with (the step allowance). A ProviderError of this kind says so, naming
	// the model (plan 019 §3.5), and says "even after compacting" only when
	// the turn did compact for it (ProviderError.Compacted). The sentinel's
	// own text claims neither.
	ErrContextTooLarge = errors.New("harness: the request is too large for the model's context window")

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

	// ErrChildMode is SetMode's refusal on a sub-agent's session: a child runs
	// in the mode its parent was in when it opened, and only the parent's own
	// later switches reach it, as tightening through its gate (plan 026 §3.5).
	// Nothing is changed.
	ErrChildMode = errors.New("harness: a sub-agent's mode is its parent's and cannot be switched")

	// ErrInTurn is Run's refusal while another Run is live on the session.
	// The adapter serializes turns itself, so it never reaches a user. It is
	// also Replay's refusal once a turn has begun, or while one (or another
	// Replay) runs: a replay shows the transcript as the session was opened
	// on it (plan 028 §3.4).
	ErrInTurn = errors.New("harness: a turn is already running")

	// ErrTurnRunning is SetTable's refusal while a turn — a Run, a Compact,
	// a Wake — or a Replay holds the session (plan 034 §3.4): a turn reads
	// one model table from its begin to its end, every sub-agent it starts
	// included, so a table that arrives meanwhile is the caller's to hand
	// over again once the turn has ended. Nothing is changed. It is not the
	// store's busy error: nothing is wrong with the session, only early.
	ErrTurnRunning = errors.New("harness: a turn is running; the model table can be swapped once it ends")

	// errTableChanged is SetModel's refusal when the model table was swapped
	// (SetTable) while it built the new model's client twice running (plan
	// 034 §3.4): the switch is judged against the table the session holds,
	// never one it has stopped holding, and two swaps inside one switch are
	// not worth a third build. Nothing is changed. The native adapter never
	// meets it: its switches and its swaps take turns (native.go's modelsMu).
	errTableChanged = errors.New("harness: the model table changed during the switch; try again")

	// ErrReplayed is Replay's refusal of a second replay: a session's stored
	// conversation is walked once, before its first turn (plan 028 §3.4).
	ErrReplayed = errors.New("harness: the session has already been replayed")

	// ErrNoTranscript is the store's: a resumed session (Options.Resume) has
	// no file at all in its workspace's session directory — and only that; a
	// file that is there and is not the session's transcript is another
	// error (plan 028 §3.2, R2-15). Open returns it wrapped, having opened
	// nothing, for the caller to decide what an absent file means (§3.5).
	ErrNoTranscript = store.ErrNoTranscript

	// ErrResumeModel is Open's refusal to resume a session no model in the
	// table can continue (plan 028 §3.3): none was asked for, and neither the
	// transcript's own model nor the table's default resolves — has its key
	// and the tool profile the session's header records. It names the
	// profile and why each candidate failed.
	ErrResumeModel = errors.New("harness: no model with the session's tool profile is available")

	// ErrNothingPending is Wake's answer when no background sub-agent's result
	// was waiting to be delivered as it began (plan 026 §3.11): nothing was
	// written, no event was emitted, and the session is as it was. A result
	// set aside by a failed wake (suspended) does not count; the next turn the
	// user starts delivers it.
	ErrNothingPending = errors.New("harness: no sub-agent result is waiting to be delivered")

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

	// ErrStoredKeyFrozen is the refusal state a session enters when a key
	// it learned as stored after it opened (LearnKeys, plan 031 §3.8, r2-2)
	// turns out to be inside what it sends unredacted with every request —
	// the system prompt, the encoded tools or the plan file's path. None of
	// those can be rewritten, and every later request would carry the key
	// again, so from then on every Run, Compact and Wake is refused with it,
	// having sent and written nothing, until Close (after which they are
	// ErrClosed). LearnKeys returns it too, from the call that found the
	// key. The key is learned all the same, for everything the session
	// still redacts. The text names no key and no surface.
	ErrStoredKeyFrozen = errors.New("harness: a newly stored API key appears in this session's frozen prompt; start a new session")

	// ErrEmptyPrompt is Run's refusal of a prompt with nothing but
	// whitespace in it: no provider has anything to answer.
	ErrEmptyPrompt = errors.New("harness: empty prompt")
)

// The ChatGPT plan's error codes the adapter words its failures by (plan 033
// §3.12), as ProviderError.Code carries them: package llm's.
const (
	// CodeUsageLimit is the plan's usage limit reached; a ProviderError with
	// it is Final, and the sign-in is latched (P33).
	CodeUsageLimit = llm.UsageLimitCode
	// CodeNotEligible is an account whose plan cannot be used here.
	CodeNotEligible = llm.NotEligibleCode
	// CodeUnsupportedCapability is a request using something the route does
	// not support, named by ProviderError.Param.
	CodeUnsupportedCapability = llm.UnsupportedCapabilityCode
)

// EmptyStepError is ErrEmptyStep on one model (classify): the provider
// finished a step having sent nothing at all (D-25). It names the model's
// alias and the driver it ran on, since what to do about it is the driver's
// — raising max_output_tokens helps a key-funded model, and means nothing on
// the ChatGPT plan, whose requests carry no output ceiling (plan 033 §3.12).
// errors.Is(err, ErrEmptyStep) answers through it.
type EmptyStepError struct {
	Model  string // the alias the turn ran on
	Driver string // that model's provider's driver
}

func (e *EmptyStepError) Error() string {
	return fmt.Sprintf("harness: model %q: %v", e.Model, ErrEmptyStep)
}
func (e *EmptyStepError) Unwrap() error { return ErrEmptyStep }

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
	Driver     string // the provider's driver: what the adapter's wording of an auth failure turns on
	StatusCode int    // the HTTP status; 0 for an error with none, such as a stream error event
	Message    string

	// Code and Type are the provider's own machine-readable names for the
	// failure, from its response's structured error (llm.ErrorNames): the
	// OpenAI-family envelope's error.code and error.type — for a quota
	// that is gone, "insufficient_quota" — or "" when it sent none. They are
	// what the provider says the failure is, where Message is display text
	// cut to maxMessageBytes, so a decision that turns on the kind of
	// failure reads them first (compact.go's quotaExhausted, review r2 major
	// 2). Package llm read them from the response as it arrived, before the
	// scrub, and kept each only as a short lowercase identifier the scrub
	// leaves as it is, so neither can hold the key (review r3 major 2).
	Code, Type string

	// Param is the provider's name for the part of the request a failure is
	// about — the ChatGPT plan's error.param, "tools[0]" or
	// "input[3].content[0].image_url" — kept only as a field path
	// (llm.FinalError); "" when it sent none.
	Param string

	// Final is set when the provider said not to repeat the request: the
	// ChatGPT plan's every 429, every 400, and the codes its docs say not to
	// retry (llm.FinalError, plan 033 §3.9, P33). Nothing retries it — not
	// Fantasy, and not the summarizer's attempts (summarizerFailureKind) —
	// though an overflow among them is still answered by compacting.
	Final bool

	// Compacted is set on an ErrContextTooLarge the turn compacted for (plan
	// 028 §3.12): its overflow compaction ran, and either failed or left a
	// context whose replacement request overflowed too. Unset, nothing was
	// compacted for it — a new session's first request, an overflow at the
	// step allowance, or a summarizer's own request — and the text says the
	// request alone is too large, not "even after compacting" (C9c item 4).
	Compacted bool

	kind error
	// signIn is the sign-in's own sentinel the failure carried — "signed
	// out", "sign in again", "usage limit reached" (llm.AuthSentinel, plan
	// 033 §3.12) — or nil: fixed text, kept so the adapter tells the sign-in's
	// failures apart by errors.Is.
	signIn error
}

func (e *ProviderError) Error() string {
	head := "harness: provider error"
	switch {
	case e.kind == ErrContextTooLarge && e.Compacted:
		head = fmt.Sprintf("harness: the conversation no longer fits model %q's context window even after compacting; start a new session", e.Model)
	case e.kind == ErrContextTooLarge:
		head = fmt.Sprintf("harness: the request alone is too large for model %q's context window", e.Model)
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

// Unwrap is the error's kind, so errors.Is(err, ErrAuth) works, and the
// sign-in's sentinel when the failure carried one.
func (e *ProviderError) Unwrap() []error {
	var out []error
	for _, err := range []error{e.kind, e.signIn} {
		if err != nil {
			out = append(out, err)
		}
	}
	return out
}

// classify maps a failed turn's error, from Fantasy's agent loop, onto the
// errors above. Every error that left the model has been rebuilt by package
// llm from scrubbed values, so nothing here can carry the key; classify
// keeps only what the adapter needs, so a raw response body never travels
// further either.
//
//   - ErrEmptyStep passes through, naming the model and its driver
//     (EmptyStepError).
//   - A *fantasy.RetryError (the step failed, was retried, and failed again)
//     is judged by its last error, the one the user would have seen.
//   - The ChatGPT plan's *llm.FinalError is Final (plan 033 §3.12), with its
//     status, message, code, type and param; it is ErrContextTooLarge when
//     it says so, which the turn answers by compacting.
//   - The wrapper's *llm.MidStreamError (a failure after output began) and a
//     *fantasy.ProviderError are classified the same way: by status, the
//     auth flag, and the context-too-large flag; and both give up the
//     provider's code and type for the failure (llm.ErrorNames), which
//     package llm read from the response before scrubbing it.
//   - Anything else — a connection that failed, the sign-in's own failure —
//     is a ProviderError with no status, carrying the error's text, and the
//     sign-in's sentinel when the scrubber kept one (llm.AuthSentinel).
//
// The error is never Compacted: only the turn knows whether it compacted
// for an overflow (turn.classify).
func classify(err error, r modeltable.Resolved) error {
	m := idOf(r)
	if errors.Is(err, ErrEmptyStep) {
		return &EmptyStepError{Model: m.Alias, Driver: r.Driver}
	}
	var re *fantasy.RetryError
	if errors.As(err, &re) && len(re.Errors) > 0 {
		err = re.Errors[len(re.Errors)-1]
	}
	pe := &ProviderError{Provider: m.Provider, Model: m.Alias, Driver: r.Driver}
	var fe *llm.FinalError
	var mse *llm.MidStreamError
	var fpe *fantasy.ProviderError
	switch {
	case errors.As(err, &fe):
		pe.StatusCode, pe.Message, pe.Final = fe.StatusCode, fe.Message, true
		if pe.Message == "" {
			pe.Message = fe.Error()
		}
		pe.kind = kindOf(fe.StatusCode, false, fe.IsContextTooLarge())
		pe.Code, pe.Type, pe.Param = fe.Code, fe.Type, fe.Param
	case errors.As(err, &mse):
		pe.StatusCode, pe.Message = mse.StatusCode, mse.Message
		pe.kind = kindOf(mse.StatusCode, mse.AuthError, mse.IsContextTooLarge())
		pe.Code, pe.Type = mse.Code, mse.Type
	case errors.As(err, &fpe):
		pe.StatusCode, pe.Message = fpe.StatusCode, fpe.Message
		if pe.Message == "" {
			pe.Message = fpe.Title
		}
		pe.kind = kindOf(fpe.StatusCode, fpe.AuthError, fpe.IsContextTooLarge())
		pe.Code, pe.Type = llm.ErrorNames(fpe)
	default:
		pe.Message = err.Error()
		pe.signIn = llm.AuthSentinel(err)
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
