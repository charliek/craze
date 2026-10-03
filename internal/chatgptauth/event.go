package chatgptauth

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"slices"
	"sort"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
)

// A sign-in's events (plan 034 §3.3, Q5–Q8): what a sign-in attempt and a
// model-list fetch report to their observer (BeginOptions.Observe,
// FetchOptions.Observe), which the sign-in log (internal/signinlog) writes and
// the UIs read the few they show. Only this package sees the paths that were
// silent before — the listener's 400 and 410 pages, the busy-port fallback, a
// refusal's code collapsed to "unrecognised", a step's timeout — so it is the
// one emitter, and the UIs report what only they see (a browser opened)
// through the attempt (Attempt.Report), from this package's constants.
//
// An Event is value-free by construction (Q7). It holds kinds, steps, classes
// and the rest from the fixed sets below, HTTP statuses, an allowlisted OAuth
// error code (knownCodes) or "unrecognised", a port, counts, a duration, the
// attempt's random id and the catalog's client_version pin — never a URL or a
// query, a pasted line, a code, a state, a nonce, a PKCE value, the host id, a
// token, an email or login_hint, a subject, a client id, an
// error_description, or any error's text. A failure's Class comes from
// errors.Is and errors.As over this package's errors and the standard
// library's (classOf), never from a message. The log validates every field
// again as it writes it (internal/signinlog), against the sets this file
// exports, so a value outside them is written as "invalid".

// Event is one thing a sign-in attempt or a model-list fetch reports. Each
// field is set only where its kind says so; the zero value of a field is "not
// said". Attempt is the attempt's id, set on every event of an attempt and on
// its model fetch's (Attempt.Observer); Elapsed is the time since the attempt
// began, or a model fetch's own duration.
type Event struct {
	Kind    EventKind
	Attempt string
	// Step is where a failure happened (failed, models_failed), or the
	// authorize step of a decline.
	Step Step
	// Status is an HTTP status: a refusal's, or the listener's page.
	Status int
	// Code is a refusal's OAuth error code: one of knownCodes, or
	// CodeUnrecognised for a code craze does not know (OAuthError).
	Code  string
	Class Class
	// Mode and Reason are begin's: listening or paste-only, and why the
	// listener is missing or on another port. Reason is also a cancel's
	// (the CloseReason the attempt was closed with).
	Mode   Mode
	Reason Reason
	// Port is the redirect's port (begin).
	Port int
	// Refusal and Part are a listener's or a paste's refusal, and the part of
	// a pasted address that is not this attempt's redirect.
	Refusal Refusal
	Part    Part
	// Check is the id_token check that failed.
	Check Check
	// Via is how an accepted redirect came: the listener or a paste.
	Via Via
	// Usage and Registration are a sign-in's: whether the account granted
	// plan usage, and whether this sign-in registered craze anew.
	Usage        Usage
	Registration Registration
	// Models is a fetched list's model count; ClientVersion the
	// client_version a model fetch was sent with (the catalog's pin).
	Models        int
	ClientVersion string
	Elapsed       time.Duration
	// Count is how many times the listener refused one kind of request in an
	// attempt (listener_refusals).
	Count int
}

// EventKind is what an Event says happened.
type EventKind string

const (
	// EventBegin: an attempt began (Mode, Port, Reason, Registration).
	EventBegin EventKind = "begin"
	// EventBrowserOpened, EventBrowserFailed, EventAddressCopied: what a UI
	// did with the address (Attempt.Report).
	EventBrowserOpened EventKind = "browser_opened"
	EventBrowserFailed EventKind = "browser_failed"
	EventAddressCopied EventKind = "address_copied"
	// EventListenerRefused: the loopback listener refused a request on the
	// callback path (Refusal, Status), the first of its kind in the attempt;
	// a 404 is never reported. EventListenerRefusals is the count of a kind
	// refused more than once, at the attempt's end (Refusal, Status, Count).
	EventListenerRefused  EventKind = "listener_refused"
	EventListenerRefusals EventKind = "listener_refusals"
	// EventPasteRefused: a pasted line was refused (Refusal, Part).
	EventPasteRefused EventKind = "paste_refused"
	// EventRedirectReceived: the attempt accepted its redirect (Via).
	EventRedirectReceived EventKind = "redirect_received"
	// The terminal outcomes: one per attempt (Attempt.Close, Wait).
	// EventSignedIn (Usage, Registration); EventDeclined (Step authorize,
	// Code access_denied); EventFailed (Step, Status, Code, Class, Check);
	// EventCancelled (Reason).
	EventSignedIn  EventKind = "signed_in"
	EventDeclined  EventKind = "declined"
	EventFailed    EventKind = "failed"
	EventCancelled EventKind = "cancelled"
	// A model fetch's (FetchOptions.Observe): fetched (Models,
	// ClientVersion), an empty reply that kept the cache (ClientVersion), or
	// failed (Step, Status, Code, Class, ClientVersion).
	EventModelsFetched EventKind = "models_fetched"
	EventModelsEmpty   EventKind = "models_empty"
	EventModelsFailed  EventKind = "models_failed"
)

// Step is where in a sign-in or a model fetch something happened.
type Step string

const (
	StepBegin     Step = "begin"     // Begin's own work: the endpoints, the auth directory, the host id
	StepAuthorize Step = "authorize" // the redirect's error, from the authorize endpoint
	StepRedirect  Step = "redirect"  // the redirect's own checks: its code and client id
	StepExchange  Step = "exchange"  // the code exchange and its reply
	StepDiscovery Step = "discovery" // the issuer's OpenID configuration
	StepJWKS      Step = "jwks"      // the issuer's key set
	StepIDToken   Step = "id_token"  // the id_token's validation
	StepInstall   Step = "install"   // the registration and the tokens written
	StepModels    Step = "models"    // the model list's request and file
	StepRefresh   Step = "refresh"   // a token renewal a model fetch needed
)

// Class is what kind of failure an error is (classOf).
type Class string

const (
	ClassTimeout      Class = "timeout"        // a step ran out of time (StepTimeoutError), or a lock wait did
	ClassCancelled    Class = "cancelled"      // the caller's context ended it, or the attempt was closed
	ClassRefused      Class = "refused"        // an endpoint refused (*OAuthError: Status, Code)
	ClassHTTPRedirect Class = "http_redirect"  // an endpoint answered a redirect, never followed
	ClassNetwork      Class = "network"        // a connection failed (*net.OpError)
	ClassDNS          Class = "dns"            // a name did not resolve (*net.DNSError)
	ClassTLS          Class = "tls"            // the TLS handshake or a certificate failed
	ClassBadReply     Class = "bad_reply"      // a reply craze cannot use: not JSON, too large, a field missing
	ClassBadRedirect  Class = "bad_redirect"   // the redirect has no code, or the wrong client id
	ClassIDToken      Class = "id_token"       // the id_token did not validate (Check)
	ClassFilesystem   Class = "filesystem"     // a file could not be read or written
	ClassSignedOut    Class = "signed_out"     // ErrSignedOut
	ClassSignInAgain  Class = "sign_in_again"  // ErrSignInAgain
	ClassPlanUsageOff Class = "plan_usage_off" // ErrPlanUsageDisabled
	ClassUsageLimited Class = "usage_limited"  // ErrUsageLimited
	ClassConfig       Class = "config"         // a test override or a fetch's options are wrong
	ClassOther        Class = "other"          // none of the above
)

// Mode is how a begun attempt waits for its redirect.
type Mode string

const (
	ModeListening Mode = "listening"  // a loopback listener waits; a paste is taken too
	ModePasteOnly Mode = "paste_only" // only a pasted redirect is taken
)

// Reason says why: why an attempt is paste-only or listens on another port
// than 1455 (begin), or why it was closed (cancelled; the CloseReasons).
type Reason string

const (
	ReasonRequested    Reason = "requested"     // BeginOptions.PasteOnly
	ReasonPortBusy     Reason = "port_busy"     // 127.0.0.1:1455 is in use
	ReasonListenFailed Reason = "listen_failed" // a listener could not be opened for another reason
	// ReasonContext: the caller's context ended an attempt that was given no
	// CloseReason (a Begin cancelled while it waited for its lock).
	ReasonContext Reason = "context"
)

// CloseReason is why an attempt is closed (Attempt.Close): a cancel's Reason
// when the attempt had not ended yet, and nothing when it had (a close after a
// sign-in is cleanup, not a cancel).
type CloseReason string

const (
	CloseEsc      CloseReason = "esc"      // the person backed out (the TUI's Esc)
	CloseDialog   CloseReason = "dialog"   // the dialog closed another way, or the session was switched
	CloseShutdown CloseReason = "shutdown" // the program is quitting, or the session ended
	CloseSignal   CloseReason = "signal"   // a signal ended the program (the CLI's Ctrl-C)
	CloseNoInput  CloseReason = "no_input" // stdin ended with no redirect pasted on a paste-only attempt (the CLI)
	CloseDone     CloseReason = "done"     // the caller is done with an attempt whose wait has returned
)

// Refusal is what a listener or a paste refused.
type Refusal string

const (
	// The listener's (Status 400, 400, 410).
	RefusalOtherAttempt Refusal = "other_attempt" // a redirect carrying a code and another attempt's state (Q8)
	RefusalNotOurs      Refusal = "not_ours"      // any other request on the callback path without this attempt's state
	RefusalOver         Refusal = "over"          // the attempt already has its redirect, or is closed (a paste's too)
	// A paste's.
	RefusalTooLong    Refusal = "too_long"    // longer than MaxPaste
	RefusalNotAddress Refusal = "not_address" // not an address at all (a key pasted out of habit, say)
	RefusalMismatch   Refusal = "mismatch"    // an address, but not this attempt's redirect (Part)
)

// Part is the part of a pasted address that is not this attempt's redirect.
type Part string

const (
	PartScheme   Part = "scheme"
	PartUserinfo Part = "userinfo"
	PartHost     Part = "host"
	PartPort     Part = "port"
	PartPath     Part = "path"
	PartState    Part = "state" // the redirect address is right, but its state is not this attempt's
)

// Check is the id_token check that failed (verifyIDToken), named, never a
// claim's value.
type Check string

const (
	CheckFormat    Check = "format"    // not a signed JWT
	CheckHeader    Check = "header"    // its header does not decode
	CheckAlg       Check = "alg"       // not RS256
	CheckKey       Check = "key"       // no key in the key set signed it
	CheckSignature Check = "signature" // its signature does not decode or verify
	CheckClaims    Check = "claims"    // its claims do not decode
	CheckIssuer    Check = "iss"
	CheckAudience  Check = "aud"
	CheckExpiry    Check = "exp"
	CheckNotBefore Check = "nbf"
	CheckNonce     Check = "nonce"
	CheckSubject   Check = "subject" // no subject, or another account's on a re-login
)

// Via is how an accepted redirect reached the attempt.
type Via string

const (
	ViaListener Via = "listener"
	ViaPaste    Via = "paste"
)

// Usage is whether a signed-in account granted plan usage.
type Usage string

const (
	UsagePlan Usage = "plan"
	UsageOff  Usage = "off"
)

// Registration is whether a sign-in registers craze anew (a first
// registration) or reuses the saved registration (a re-login).
type Registration string

const (
	RegistrationNew    Registration = "new"
	RegistrationReused Registration = "reused"
)

// CodeUnrecognised is Event.Code for a refusal whose code craze does not know
// (OAuthError.Unrecognised): the fact that there was one, never the code.
const CodeUnrecognised = "unrecognised"

// The valid sets, in a fixed order. The exported functions answer copies, so
// no caller can change what the log validates against.
var (
	eventKinds = []EventKind{
		EventBegin, EventBrowserOpened, EventBrowserFailed, EventAddressCopied,
		EventListenerRefused, EventListenerRefusals, EventPasteRefused, EventRedirectReceived,
		EventSignedIn, EventDeclined, EventFailed, EventCancelled,
		EventModelsFetched, EventModelsEmpty, EventModelsFailed,
	}
	steps = []Step{
		StepBegin, StepAuthorize, StepRedirect, StepExchange, StepDiscovery, StepJWKS, StepIDToken,
		StepInstall, StepModels, StepRefresh,
	}
	classes = []Class{
		ClassTimeout, ClassCancelled, ClassRefused, ClassHTTPRedirect, ClassNetwork, ClassDNS, ClassTLS,
		ClassBadReply, ClassBadRedirect, ClassIDToken, ClassFilesystem, ClassSignedOut, ClassSignInAgain,
		ClassPlanUsageOff, ClassUsageLimited, ClassConfig, ClassOther,
	}
	modes        = []Mode{ModeListening, ModePasteOnly}
	closeReasons = []CloseReason{CloseEsc, CloseDialog, CloseShutdown, CloseSignal, CloseNoInput, CloseDone}
	refusals     = []Refusal{
		RefusalOtherAttempt, RefusalNotOurs, RefusalOver, RefusalTooLong, RefusalNotAddress, RefusalMismatch,
	}
	parts  = []Part{PartScheme, PartUserinfo, PartHost, PartPort, PartPath, PartState}
	checks = []Check{
		CheckFormat, CheckHeader, CheckAlg, CheckKey, CheckSignature, CheckClaims, CheckIssuer, CheckAudience,
		CheckExpiry, CheckNotBefore, CheckNonce, CheckSubject,
	}
	vias          = []Via{ViaListener, ViaPaste}
	usages        = []Usage{UsagePlan, UsageOff}
	registrations = []Registration{RegistrationNew, RegistrationReused}
)

// EventKinds is every EventKind: the set the sign-in log validates Kind
// against. The functions after it are the other fields' sets, each the
// field's every valid value.
func EventKinds() []EventKind       { return slices.Clone(eventKinds) }
func Steps() []Step                 { return slices.Clone(steps) }
func Classes() []Class              { return slices.Clone(classes) }
func Modes() []Mode                 { return slices.Clone(modes) }
func Refusals() []Refusal           { return slices.Clone(refusals) }
func Parts() []Part                 { return slices.Clone(parts) }
func Checks() []Check               { return slices.Clone(checks) }
func Vias() []Via                   { return slices.Clone(vias) }
func Usages() []Usage               { return slices.Clone(usages) }
func Registrations() []Registration { return slices.Clone(registrations) }

// Reasons is every Reason: begin's and the context's, then every CloseReason,
// since a cancel's Reason is the reason its Close was given.
func Reasons() []Reason {
	out := []Reason{ReasonRequested, ReasonPortBusy, ReasonListenFailed, ReasonContext}
	for _, c := range closeReasons {
		out = append(out, Reason(c))
	}
	return out
}

// usageOf is a sign-in's Usage: plan when plan usage was granted.
func usageOf(plan bool) Usage {
	if plan {
		return UsagePlan
	}
	return UsageOff
}

// registrationOf is a sign-in's Registration: new when it registered craze.
func registrationOf(registered bool) Registration {
	if registered {
		return RegistrationNew
	}
	return RegistrationReused
}

// OAuthCodes is every OAuth error code an Event may carry: knownCodes, the
// codes a refusal is allowed to repeat (plan 033 C14r), and
// CodeUnrecognised. Sorted.
func OAuthCodes() []string {
	out := []string{CodeUnrecognised}
	for c := range knownCodes {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// newAttemptID is a fresh attempt id: 8 lowercase hex digits, random, made in
// Begin and carried by every event of the attempt. It names one attempt in
// the log and nothing else.
func newAttemptID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// MaxPaste is the longest line Attempt.Paste takes as a pasted redirect: a
// real one is a few hundred bytes (an address, a code, the scopes, the state
// and the client id), so a longer line is not one. Both UIs bound what they
// read by it (plan 034 §3.3: the pre-checks moved here).
const MaxPaste = 16 << 10

// PasteError is Attempt.Paste's refusal of a pasted line (plan 034 §3.3):
// too long, not an address, or an address that is not this attempt's
// redirect — Part saying which part of it is not. It never holds the line.
// errors.Is(err, ErrRedirectMismatch) is true of every PasteError, as it was
// of every refusal before the UIs' own pre-checks moved here.
type PasteError struct {
	Refusal Refusal
	Part    Part
}

func (e *PasteError) Error() string {
	switch e.Refusal {
	case RefusalTooLong:
		return "chatgptauth: that is too long to be the redirect address"
	case RefusalNotAddress:
		return "chatgptauth: that is not an address"
	}
	return ErrRedirectMismatch.Error()
}

func (e *PasteError) Unwrap() error { return ErrRedirectMismatch }

// PasteRefusalText is how both UIs say why Paste refused a line (plan 034
// §3.3), phrased from its *PasteError and never quoting the line: too long;
// not an address at all — most likely a key typed where the plan's sign-in
// goes, so it says the plan takes none (plan 033 §3.13: no API-key login for
// the ChatGPT plan); the redirect of an earlier attempt — the right address
// with another state — said in earlier, the surface's own sentence; or
// another address. redirect is the attempt's redirect address as the surface
// prints it (sanitized), named as what to paste instead.
func PasteRefusalText(err error, redirect, earlier string) string {
	what := "Paste the whole address the browser was sent to; it starts with " + redirect + "."
	var pe *PasteError
	if errors.As(err, &pe) {
		switch {
		case pe.Refusal == RefusalTooLong:
			return "That is too long to be the redirect address. " + what
		case pe.Refusal == RefusalNotAddress:
			return "That is not an address: the ChatGPT plan is funded by signing in, never by an API key. " + what
		case pe.Part == PartState:
			return earlier
		}
	}
	return "That is not this sign-in's redirect address. " + what
}

// StepTimeoutError is a step that ran out of time (plan 034 A14): the bare
// "context deadline exceeded" of a request, the exchange's or the model
// list's deadline, or a lock another process held past the wait's bound,
// named by the step in progress — "chatgptauth: exchange: timed out after
// 30s". errors.Is finds context.DeadlineExceeded in a deadline's, and
// atomicfile.ErrLockBusy in a lock's (Lock names the lock then).
type StepTimeoutError struct {
	Step  Step
	After time.Duration
	Lock  string
}

func (e *StepTimeoutError) Error() string {
	s := fmt.Sprintf("chatgptauth: %s: timed out after %s", e.Step, e.After)
	if e.Lock != "" {
		s += " waiting for " + e.Lock + ", which another craze process holds"
	}
	return s
}

func (e *StepTimeoutError) Unwrap() error {
	if e.Lock != "" {
		return atomicfile.ErrLockBusy
	}
	return context.DeadlineExceeded
}

// The locks a StepTimeoutError can name.
const (
	signInLockName = "the sign-in lock"
	modelsLockName = "the model list's lock"
)

// deadlineAt is err as a StepTimeoutError when it is a deadline that ran out
// within the caller's context (caller still live): the step err names, or
// fallback, after the bound that ran out. Anything else is err as it is: a
// caller's own cancel or deadline is the caller's, not a step's.
func deadlineAt(caller context.Context, err error, fallback Step, after func(Step) time.Duration) error {
	if err == nil || caller.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var ste *StepTimeoutError
	if errors.As(err, &ste) && ste.Lock != "" {
		return err
	}
	step := stepOf(err)
	if step == "" {
		step = fallback
	}
	return &StepTimeoutError{Step: step, After: after(step)}
}

// lockTimeout is a lock wait's error at step: the lock held past lockWait is
// a StepTimeoutError naming it; anything else is err, at step.
func lockTimeout(step Step, lock string, err error) error {
	if errors.Is(err, atomicfile.ErrLockBusy) {
		return &StepTimeoutError{Step: step, After: lockWait, Lock: lock}
	}
	return atStep(step, err)
}

// stepError is err with the step it happened in, and — for a reply or a
// redirect craze refused, or an endpoint's redirect — its class and HTTP
// status. It is transparent: its text is err's, and errors.Is and errors.As
// see through it.
type stepError struct {
	step   Step
	status int
	class  Class
	err    error
}

func (e *stepError) Error() string { return e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

// atStep is err labelled with step, unless it already names one (the
// narrowest label wins: a discovery failure inside the key set's fetch stays
// a discovery failure).
func atStep(step Step, err error) error {
	if err == nil || stepOf(err) != "" {
		return err
	}
	return &stepError{step: step, err: err}
}

// badReply is a reply at step craze cannot use: what is wrong with it, as
// "chatgptauth: <step>: <what>".
func badReply(step Step, what string) error {
	return &stepError{step: step, class: ClassBadReply, err: fmt.Errorf("chatgptauth: %s: %s", step, what)}
}

// badRedirect is a redirect craze cannot use, with msg its text.
func badRedirect(msg string) error {
	return &stepError{step: StepRedirect, class: ClassBadRedirect, err: errors.New(msg)}
}

// idTokenError is an id_token that did not validate (verifyIDToken): which
// check failed, as a Check and in words, never a claim's value. It is
// errIDToken to errors.Is, with the same text as before it had a type.
type idTokenError struct {
	check Check
	what  string
}

func (e *idTokenError) Error() string        { return errIDToken.Error() + ": " + e.what }
func (e *idTokenError) Is(target error) bool { return target == errIDToken }

// stepNamer is an error that names the step it happened in: a
// StepTimeoutError, a stepError, an *OAuthError or an id_token's error.
type stepNamer interface{ errStep() Step }

func (e *StepTimeoutError) errStep() Step { return e.Step }
func (e *stepError) errStep() Step        { return e.step }
func (e *OAuthError) errStep() Step       { return Step(e.Step) }
func (e *idTokenError) errStep() Step     { return StepIDToken }

// stepOf is the step err names — the outermost stepNamer errors.As finds in
// it — or "".
func stepOf(err error) Step {
	var n stepNamer
	if errors.As(err, &n) {
		return n.errStep()
	}
	return ""
}

// classOf sorts err by errors.Is and errors.As over this package's errors and
// the standard library's, never by its text (plan 034 Q7).
func classOf(err error) Class {
	var (
		ste      *StepTimeoutError
		oe       *OAuthError
		ie       *idTokenError
		se       *stepError
		dnsErr   *net.DNSError
		opErr    *net.OpError
		certErr  *tls.CertificateVerificationError
		authErr  x509.UnknownAuthorityError
		hostErr  x509.HostnameError
		invErr   x509.CertificateInvalidError
		recErr   tls.RecordHeaderError
		alertErr tls.AlertError
		pathErr  *fs.PathError
		linkErr  *os.LinkError
		sysErr   *os.SyscallError
	)
	switch {
	case err == nil:
		return ""
	case errors.As(err, &ste), errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	case errors.Is(err, context.Canceled), errors.Is(err, ErrAttemptOver):
		return ClassCancelled
	case errors.As(err, &oe):
		return ClassRefused
	case errors.As(err, &ie):
		return ClassIDToken
	case errors.Is(err, errRedirect):
		return ClassHTTPRedirect
	case errors.Is(err, ErrOverride), errors.Is(err, ErrClientVersionRequired), errors.Is(err, errOffAPI):
		return ClassConfig
	case errors.Is(err, ErrSignedOut):
		return ClassSignedOut
	case errors.Is(err, ErrPlanUsageDisabled):
		return ClassPlanUsageOff
	case errors.Is(err, ErrUsageLimited):
		return ClassUsageLimited
	case errors.Is(err, ErrSignInAgain):
		return ClassSignInAgain
	case errors.As(err, &se) && se.class != "":
		return se.class
	case errors.As(err, &certErr), errors.As(err, &authErr), errors.As(err, &hostErr), errors.As(err, &invErr),
		errors.As(err, &recErr), errors.As(err, &alertErr):
		return ClassTLS
	case errors.As(err, &dnsErr):
		return ClassDNS
	case errors.As(err, &opErr), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF),
		errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET):
		return ClassNetwork
	case errors.As(err, &pathErr), errors.As(err, &linkErr), errors.As(err, &sysErr):
		return ClassFilesystem
	}
	return ClassOther
}

// failureEvent is kind's event for err: its step (stepOf, or fallback when
// err names none), class (classOf), HTTP status and allowlisted code (an
// *OAuthError's, or a stepError's status), and the id_token check that
// failed. Nothing of err's text.
func failureEvent(kind EventKind, err error, fallback Step) Event {
	ev := Event{Kind: kind, Step: stepOf(err), Class: classOf(err)}
	if ev.Step == "" {
		ev.Step = fallback
	}
	var oe *OAuthError
	var se *stepError
	var ie *idTokenError
	switch {
	case errors.As(err, &oe):
		ev.Status, ev.Code = oe.Status, oe.Code
		if ev.Code == "" && oe.Unrecognised {
			ev.Code = CodeUnrecognised
		}
	case errors.As(err, &se):
		ev.Status = se.status
	}
	if errors.As(err, &ie) {
		ev.Check = ie.check
	}
	return ev
}

// report hands ev to observe, when there is one.
func report(observe func(Event), ev Event) {
	if observe != nil {
		observe(ev)
	}
}
