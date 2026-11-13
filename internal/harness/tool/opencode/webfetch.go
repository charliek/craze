package opencode

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"strings"

	"github.com/charliek/craze/internal/harness/tool"
)

type webFetchTool struct{ spec tool.Spec }

func newWebFetch() (tool.Tool, error) {
	desc, err := description("webfetch", nil)
	if err != nil {
		return nil, err
	}
	// opencode's Input (webfetch.ts:24-33) less format and timeout, which come
	// with a local fetch (plan 051 §A, NOTICE).
	return &webFetchTool{spec: tool.Spec{
		ID:          tool.WebFetchTool,
		Description: desc,
		Parameters: map[string]any{
			"url": map[string]any{"type": "string", "description": "The HTTP or HTTPS URL to fetch content from"},
		},
		Required: []string{"url"},
		Kind:     tool.KindFetch,
		ReadOnly: true,
		Parallel: true,
		Truncate: tool.Head,
	}}, nil
}

func (w *webFetchTool) Spec() tool.Spec { return w.spec }

// notPublicText is the refusal of a host that is not public. The provider
// does the fetching, so this is not a guard of craze's own network: it keeps
// an internal address from being sent to the provider, and a call that could
// not succeed from being made (plan 051 §A).
const notPublicText = "webfetch reaches public web pages only, not localhost or a loopback, link-local, private or unspecified address"

func (w *webFetchTool) Prepare(_ tool.Env, c tool.Call) (tool.Prepared, error) {
	a, err := parseArgs(c.Input)
	if err != nil {
		return nil, err
	}
	raw, _, err := a.str("url", true)
	if err != nil {
		return nil, err
	}
	raw = strings.TrimSpace(raw)
	if err := checkFetchURL(raw); err != nil {
		return nil, err
	}
	return &webFetchCall{url: raw}, nil
}

// checkFetchURL refuses a URL webfetch does not fetch: anything but http and
// https, one carrying a user name or password, one with no host, and one
// whose host is not public — the names localhost and *.localhost, and an IP
// literal that is loopback, link-local, private (RFC 1918, unique-local) or
// unspecified, an IPv6 literal judged without its zone, and an IPv4-mapped
// IPv6 literal by its IPv4 address. The host is compared lowercased with a
// trailing dot removed. It is a check of the name or literal as written: no
// name is looked up, so a public name that resolves to a private address
// passes, as does a numeric spelling netip does not parse as an address
// (2130706433, 127.1), which is read as a name.
func checkFetchURL(raw string) error {
	if raw == "" {
		return errors.New("url must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("url is not a valid URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https": // url.Parse lowercases the scheme
		return errors.New("url must be an http or https URL")
	case u.User != nil:
		return errors.New("url must not contain a user name or password")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return errors.New("url has no host")
	}
	if !publicHost(host) {
		return errors.New(notPublicText)
	}
	return nil
}

// publicHost reports whether host, lowercased and without its trailing dot,
// is not one checkFetchURL refuses by name or by address.
func publicHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return true // a name, not an IP literal
	}
	// A zone names the interface, not the address, but netip's predicates
	// compare the whole address: IsUnspecified is false for "::%eth0". The
	// zone is set aside first, so a zoned address is judged as the same
	// address without one — refused or, if public, accepted. netip's
	// predicates judge an IPv4-mapped address by its IPv4 address already;
	// unmapping says so here rather than relying on it.
	ip = ip.WithZone("").Unmap()
	return !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() &&
		!ip.IsPrivate() && !ip.IsUnspecified()
}

type webFetchCall struct{ url string }

func (c *webFetchCall) Request() tool.Request { return tool.Request{Title: c.url} }

func (c *webFetchCall) Run(ctx context.Context, env tool.Env) tool.Result {
	if ctx.Err() != nil {
		return webAborted()
	}
	if env.Web == nil {
		return tool.Result{Text: noWebText, IsError: true, Class: tool.ClassToolError}
	}
	page, err := env.Web.Fetch(ctx, c.url)
	if err != nil {
		return webErrorResult(ctx, err, "Unable to fetch "+c.url)
	}
	// The provider's text as it is (opencode's webfetch returns its
	// converted body as it is, webfetch.ts:185-192).
	// An empty page's answer names the URL, which can be longer than the
	// limit: it is guarded as a page's long line is.
	text := page.Text
	if text == "" {
		text = "No content returned for " + c.url
	}
	return tool.Result{Text: cutFirstLine(text, env)}
}
