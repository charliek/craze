package harness

import (
	"fmt"
	"path/filepath"

	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/websearch"
)

// WebMode is whether a session offers the web tools, websearch and webfetch
// (plan 051 §C.3). The agent layer decides it once, at Open; the harness only
// applies it.
type WebMode int

const (
	// WebWithheld offers neither tool. It is the zero value, so a session
	// whose caller sets no web state is offered exactly what it was before
	// the tools existed: the same specs, agent tool text and tools_sha256.
	WebWithheld WebMode = iota
	// WebOn offers both, and gives them to the session's children too.
	WebOn
	// WebAsking offers both in a session that has not decided yet and can ask
	// (the service asks on the first call). Its children are not offered
	// them: a child has nobody to ask.
	WebAsking
)

// WebCall is one call the web service handled (tool.WebCall, plan 051
// F.4.1), as Options.OnWebCall is told of it: an alias, so harness.go stays
// clear of the tool framework (TestSeamOne).
type WebCall = tool.WebCall

// WebState is a session's web tools at Open: the mode, and the service the
// tools reach the web through (tool.Env.Web). A mode that offers the tools
// with no service, where Options.WebEndpoint is set, has one built at Open
// (withService); with neither, the tools are still offered, and each answers
// with a tool_error.
type WebState struct {
	Mode    WebMode
	Service tool.Web
	// Provider is the search provider a built service calls
	// (websearch.Providers); "" is the default.
	Provider string
	// CanAsk is the session's question policy at Open, as the adapter read
	// it: a person is asked (plan 051 §C.3). A built service asks the
	// consent question only when it is set, whatever the mode — so an
	// automatic answer is never taken for consent — and remembers nothing
	// when it is not.
	CanAsk bool
}

// Offered reports whether the session offers the web tools. Any mode but on
// and asking withholds them.
func (w WebState) Offered() bool { return w.Mode == WebOn || w.Mode == WebAsking }

// forChild is the state a child of a session in w opens with (plan 051 §C.3,
// "Children"): the parent's own, sharing its service, when it opened on, and
// withheld otherwise — an asking parent's child has nobody to ask, and one
// withheld has nothing to give.
func (w WebState) forChild() WebState {
	if w.Mode == WebOn {
		return w
	}
	return WebState{}
}

// withService is w with the web service a top-level session's tools reach
// the web through, built at Open (plan 051 §B, §C.4) when w offers the tools,
// has no service of its own and o.WebEndpoint is set (the adapter always
// sets it; a harness test that leaves it nil builds nothing and so reaches
// no network). The service calls w's provider at o.WebEndpoint's answer
// ("" is the provider's own endpoint) with the table's key for it, read once
// here through getenv — the variable, else the key credentials.toml stored
// when the table was loaded (modeltable.WebSearchKey, plan 051 §F.5): the
// reading the session's redactor is built from, so the key sent is one it
// holds, fixed until the session opens again — and asks o.Asker the consent question on its
// first call when w is asking and can ask, remembering the answer in
// websearch.StateFile under o.Home and reporting a failed write to o.Warn.
// A child's state comes from its parent with the parent's service
// (forChild), so a child never builds one, and never reads or writes the
// remembered choice.
func (w WebState) withService(o *Options, getenv func(string) string) (WebState, error) {
	if !w.Offered() || w.Service != nil || o.WebEndpoint == nil || o.Child != nil {
		return w, nil
	}
	endpoint, err := o.WebEndpoint()
	if err != nil {
		return w, fmt.Errorf("harness: web search: %w", err)
	}
	svc, err := newWebService(websearch.Config{
		Provider:  w.Provider,
		Key:       o.Table.WebSearchKey(getenv).Reveal(),
		Endpoint:  endpoint,
		Undecided: w.Mode == WebAsking,
		CanAsk:    w.CanAsk,
		Asker:     o.Asker,
		StatePath: filepath.Join(o.Home, websearch.StateFile),
		Warn:      o.Warn,
		Report:    o.OnWebCall,
	})
	if err != nil {
		return w, fmt.Errorf("harness: web search: %w", err)
	}
	w.Service = svc
	return w, nil
}

// newWebService builds the service withService hands the session: a seam for
// the tests that read the configuration a session built it from (the key it
// resolved) without reaching the provider's own endpoint; websearch.New in
// production.
var newWebService = websearch.New

// withWeb is tools less the web tools when w does not offer them, in the
// same order: the list openTools builds everything of a session from, and
// what toolset.check holds a switch's model to. A new slice when one is
// dropped; tools itself is never changed.
func withWeb(tools []tool.Tool, w WebState) []tool.Tool {
	if w.Offered() {
		return tools
	}
	out := make([]tool.Tool, 0, len(tools))
	for _, t := range tools {
		if !tool.IsWebTool(t.Spec().ID) {
			out = append(out, t)
		}
	}
	return out
}
