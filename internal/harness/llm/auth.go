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
