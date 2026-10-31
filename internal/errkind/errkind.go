// Package errkind is the closed set of kinds a waiting command's failure is
// told by, and what a failure carries beside its words: whether a fresh turn
// may succeed, and the provider's own status and code for it.
//
// A failure is classified where it happens, while its type is still known:
// the harness's ProviderError, the ACP client's RPCError and the CLI's own
// failures each say what they are, and the event codec carries that through
// events, the terminal state, snapshots and replay. A waiter only reads it; it
// never guesses from text.
//
// The package is a leaf, the standard library only, so the harness (which
// imports nothing else of craze's but internal/atomicfile) can use it.
package errkind

import (
	"errors"
	"regexp"
)

// Kind names what a failure was. Results and envelopes print it; docs/reference/cli.md
// has its table (TestKindsTableIsTheSet holds the two together).
type Kind string

const (
	// Usage is a flag or argument error (exit 2).
	Usage Kind = "usage"
	// Config is an unknown provider or model, or bad configuration.
	Config Kind = "config"
	// Workspace is a -C that is missing or not a directory.
	Workspace Kind = "workspace"
	// Prompt is a prompt that is unreadable, empty or too long (exit 2).
	Prompt Kind = "prompt"
	// StartFailed is an agent or host that did not start.
	StartFailed Kind = "start_failed"
	// TimeoutBeforePrompt is --timeout's end before the prompt was sent.
	TimeoutBeforePrompt Kind = "timeout_before_prompt"
	// Provider is any other failure at or on the way to a model provider.
	Provider Kind = "provider"
	// Auth is a provider refusing the credentials, or none there to send.
	Auth Kind = "auth"
	// Quota is an account's quota, credit or plan limit being gone.
	Quota Kind = "quota"
	// ContextTooLarge is a request too long for the model's context window.
	ContextTooLarge Kind = "context_too_large"
	// ModelNotFound is a provider or agent that does not serve the model.
	ModelNotFound Kind = "model_not_found"
	// Agent is an ACP agent's own error, or a turn stopped short of its end.
	Agent Kind = "agent"
	// Stopped is the session having been stopped.
	Stopped Kind = "stopped"
	// ConnectionLost is the waiter losing its socket to a live host.
	ConnectionLost Kind = "connection_lost"
	// HostExited is the host shown gone with no ending observed.
	HostExited Kind = "host_exited"
	// Unobserved is a turn whose ending the waiter could not learn.
	Unobserved Kind = "unobserved"
	// Interrupted is the waiter signalled, before or after the prompt.
	Interrupted Kind = "interrupted"
	// Internal is anything else: craze's own failure.
	Internal Kind = "internal"
	// Unknown is what an error from a host that predates kinds reads as, and
	// a kind from a later craze this build does not know.
	Unknown Kind = "unknown"
)

// kinds is every Kind, in the order the reference's table lists them, each
// with whether a failure of it may be retried unless its source says
// otherwise. retryable means a fresh turn may succeed; it does not promise
// that re-running a turn repeats no side effects already performed.
var kinds = map[Kind]bool{
	Usage:               false,
	Config:              false,
	Workspace:           false,
	Prompt:              false,
	StartFailed:         true,
	TimeoutBeforePrompt: true,
	Provider:            false,
	Auth:                false,
	Quota:               false,
	ContextTooLarge:     false,
	ModelNotFound:       false,
	Agent:               false,
	Stopped:             false,
	ConnectionLost:      true,
	HostExited:          true,
	Unobserved:          true,
	Interrupted:         false,
	Internal:            false,
	Unknown:             false,
}

// Retryable is k's default: what a failure of it says when nothing more is
// known. A provider failure's own class overrides it (harness.ProviderError),
// as an agent's own flag does (acp.RPCError). A kind not in the set is false.
func (k Kind) Retryable() bool { return kinds[k] }

// Info is what a failure says about itself. The zero value is a failure with
// nothing known: a host that predates kinds (Known reads it as Unknown). The
// wire structs embed it, so its tags are the wire's spelling, each field absent
// at its zero value.
type Info struct {
	Kind      Kind `json:"kind,omitempty"`
	Retryable bool `json:"retryable,omitempty"`
	// Status is the provider's HTTP status, 0 for none. An ACP agent's
	// JSON-RPC code is not one and never lands here.
	Status int `json:"status,omitempty"`
	// ProviderCode is the provider's machine-readable name for the failure
	// ("insufficient_quota"), "" for none, kept only as CleanCode keeps it.
	ProviderCode string `json:"provider_code,omitempty"`
}

// New is k with its default retryable.
func New(k Kind) Info { return Info{Kind: k, Retryable: k.Retryable()} }

// Known is i as a reader reports it: a kind that is empty or not in the set —
// an older host's, or a later craze's — is Unknown, and an empty one claims
// nothing retryable. A later craze's flag stands: its host knew the failure.
func (i Info) Known() Info {
	if _, ok := kinds[i.Kind]; ok {
		return i
	}
	if i.Kind == "" {
		i.Retryable = false
	}
	i.Kind = Unknown
	return i
}

// Classified is an error that says what it is. errors.As finds it anywhere in
// a chain, so wrapping one keeps what it said.
type Classified interface {
	error
	ErrorInfo() Info
}

// Of is what err's chain says about itself, and whether anything in it did.
func Of(err error) (Info, bool) {
	var c Classified
	if errors.As(err, &c) {
		return c.ErrorInfo(), true
	}
	return Info{}, false
}

// codeName is what a provider code CleanCode keeps looks like.
var codeName = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// CleanCode is s when it is a short lowercase identifier, else "": a provider
// code is a name, never text, so nothing that could hold a key, a path or a
// piece of a request survives it.
func CleanCode(s string) string {
	if !codeName.MatchString(s) {
		return ""
	}
	return s
}

// CleanStatus is s when it is an HTTP status, else 0.
func CleanStatus(s int) int {
	if s < 100 || s > 599 {
		return 0
	}
	return s
}
