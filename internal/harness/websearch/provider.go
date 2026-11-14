package websearch

import (
	"context"
	"maps"
	"slices"

	"github.com/charliek/craze/internal/harness/tool"
)

// Provider is one search provider: what the service calls for a search and a
// fetch. Its errors are the caller's failures (*failure), which the service
// puts in words; a provider never returns text of its own to the model.
type Provider interface {
	Search(ctx context.Context, query string) ([]tool.WebResult, error)
	Fetch(ctx context.Context, url string) (tool.WebPage, error)
}

// DefaultProvider is the provider a service uses when none is named: the only
// one there is.
const DefaultProvider = "exa"

// entry is a provider in the table: where it is reached, the variable its
// key is read from (named in the hint a rate limit gives when no key was
// sent), and how it is built over a caller of that endpoint.
type entry struct {
	endpoint string
	keyEnv   string
	build    func(*caller) Provider
}

// providers are the providers by id (plan 051 §B). One for now; a second
// brings failover with it.
var providers = map[string]entry{
	"exa": {endpoint: ExaEndpoint, keyEnv: ExaKeyEnv, build: newExa},
}

// Providers is every provider's id, sorted.
func Providers() []string { return slices.Sorted(maps.Keys(providers)) }
