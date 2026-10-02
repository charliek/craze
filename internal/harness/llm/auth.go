package llm

import "context"

// Auth is a credential that is not a static key: a bearer token that is
// minted, renewed and revoked elsewhere, which a driver asks for at each
// request (plan 033 §3.9, §3.10, P31). The ChatGPT plan's driver
// (newResponsesModel) is the one that takes it, from a Sign in with ChatGPT
// token source (internal/chatgptauth, outside the harness), injected through
// harness.Options; the harness itself knows only this interface, so it
// imports nothing of the sign-in.
//
// A token source satisfies it structurally. Each method may be called from
// any goroutine, at once: every session's steps, its sub-agents and its
// summarizer share one source per process.
type Auth interface {
	// Token is the bearer for the next request, and gen, an opaque value
	// naming that token, which the caller hands back to Invalidate when the
	// server refuses it. The source renews the token as it sees fit before
	// handing it out (P20); an error means there is no usable token — the
	// user signed out, or must sign in again — and is returned as it is, so
	// its sentinel survives (the scrubber keeps it, plan 033 §3.12).
	Token(ctx context.Context) (token string, gen uint64, err error)

	// Invalidate tells the source that the token gen named was refused (an
	// HTTP 401): a newer token than gen is adopted, or the token is renewed
	// now, ignoring any advice to wait (plan 033 §3.10). A gen already
	// replaced is a no-op. The caller then asks Token again and retries the
	// request once; an error means the token could not be renewed, and the
	// caller gives up with it.
	Invalidate(ctx context.Context, gen uint64) error

	// Values is every secret value the source holds or has held recently —
	// access, refresh and id tokens, current and retired — for the scrubber,
	// which keeps each out of every error the model returns (plan 033 §3.12,
	// P19, P35). It never blocks on the network.
	Values() []string
}

// The optional sides of an Auth (plan 033 §3.12). The ChatGPT plan's token
// source (chatgptauth.TokenSource) has both; an Auth without them works, with
// neither its sentinels kept nor a usage limit latched.

// authSentinels is an Auth whose failures callers tell apart by sentinel —
// the sign-in's "signed out", "sign in again", "plan usage disabled" and
// "usage limit reached" — each a fixed-text error its Token wraps. The
// scrubber keeps the one an error carries reachable through the error it
// rebuilds (scrubber.sentinel), as it keeps context.Canceled: a sentinel has
// no text but its own, so it cannot carry a token, and errors.Is then answers
// through every layer above, which is how the adapter phrases them.
type authSentinels interface {
	Sentinels() []error
}

// usageLatch is an Auth that stops every new request once the ChatGPT plan's
// usage limit is reached (P33): the driver calls LatchUsageLimit as it
// returns the usage-limit FinalError, so no wake, sub-agent, summarizer or
// retry sends another request on the plan until the adapter clears the latch
// at the next turn a person starts. Until then Token fails at once.
type usageLatch interface {
	LatchUsageLimit()
}
