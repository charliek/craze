package harness

import "github.com/charliek/craze/internal/harness/tool"

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

// WebState is a session's web tools at Open: the mode, and the service the
// tools reach the web through (tool.Env.Web). A mode that offers the tools
// with no service still offers them; each then answers with a tool_error.
type WebState struct {
	Mode    WebMode
	Service tool.Web
}

// offered reports whether the session offers the web tools. Any mode but on
// and asking withholds them.
func (w WebState) offered() bool { return w.Mode == WebOn || w.Mode == WebAsking }

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

// withWeb is tools less the web tools when w does not offer them, in the
// same order: the list openTools builds everything of a session from, and
// what toolset.check holds a switch's model to. A new slice when one is
// dropped; tools itself is never changed.
func withWeb(tools []tool.Tool, w WebState) []tool.Tool {
	if w.offered() {
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
