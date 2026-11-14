package websearch

import (
	"context"
	"slices"
	"sync"

	"github.com/charliek/craze/internal/harness/tool"
)

// Consent (plan 051 §C.4): a session that opened undecided asks the person
// on its first web call, once, and remembers the answer.
const (
	// consentHeader is the consent card's title.
	consentHeader = "Web search"
	// ConsentQuestion is what the person is asked.
	ConsentQuestion = "Allow craze to search the web and fetch pages for up-to-date information?"
	// consentDetail says where the queries go: the allowing option's
	// description, drawn under it on the card.
	consentDetail = "Queries and URLs are sent to Exa (exa.ai)."
	// consentAllow and consentDisable are the options, in this order: the
	// first allows (and is remembered), the second disables (and is
	// remembered).
	consentAllow   = "Allow via Exa"
	consentDisable = "Disable web search"

	// DisabledText answers a call in a session whose person disabled web
	// search from the card: this call, and every later one.
	DisabledText = "Web search is disabled"
	// DeclinedText answers a call in a session that was not allowed web
	// search: the card was skipped, left unanswered or answered with
	// something but one option, or the session cannot ask at all.
	DeclinedText = "Web search was not approved in this session"
	// WriteFailedText is the diagnostic when the answer cannot be remembered.
	// It names the file, never its path or the error's text.
	WriteFailedText = "web search: the answer could not be saved to " + StateFile + "; it holds for this session, and the next one asks again"
)

// consentQuestion is the one question the card asks.
var consentQuestion = []tool.Question{{
	Header:   consentHeader,
	Question: ConsentQuestion,
	Options: []tool.QuestionOption{
		{Label: consentAllow, Description: consentDetail},
		{Label: consentDisable},
	},
}}

// verdict is where a session's consent stands.
type verdict int

const (
	undecided verdict = iota
	allowed
	disabled // from the card: remembered
	declined // for this session only: nothing remembered
)

// consent is a service's gate: every call passes it before the provider is
// called, so the provider's deadline never covers the time the person takes.
type consent struct {
	asker    tool.Asker
	canAsk   bool
	path     string
	provider string
	warn     func(string)

	mu      sync.Mutex
	verdict verdict
	// asking is the question in flight, which every call that arrives
	// meanwhile waits on rather than asking again.
	asking *pendingAsk
}

// pendingAsk is one question in flight: done closes when it is settled, and
// aborted then says the asking call ended before an answer came.
type pendingAsk struct {
	done    chan struct{}
	aborted bool
}

// newConsent is the gate cfg describes: allowed when the session opened
// decided, and undecided otherwise.
func newConsent(cfg Config, provider string) *consent {
	c := &consent{asker: cfg.Asker, canAsk: cfg.CanAsk, path: cfg.StatePath, provider: provider, warn: cfg.Warn, verdict: allowed}
	if cfg.Undecided {
		c.verdict = undecided
	}
	return c
}

// pass says whether a call may go on: nil when it may, and otherwise the
// error the call answers with.
//
//   - Allowed passes; disabled and declined answer DisabledText and
//     DeclinedText.
//   - Undecided with no way to ask (canAsk false, or no asker) answers
//     DeclinedText, asking nobody and writing nothing: an answer a policy
//     gives on nobody's behalf is not consent. The verdict stays undecided.
//   - Undecided with a question in flight waits for it and shares its
//     outcome; a waiter whose own ctx ends first is aborted alone.
//   - Undecided otherwise asks. Exactly the first option allows and the
//     second disables, each remembered; anything else declines for the
//     session, remembering nothing. If ctx has ended by the time the asker
//     returns — even with an answer — this call and every waiter are
//     aborted, nothing is decided, and the next call asks again.
func (c *consent) pass(ctx context.Context) *tool.WebError {
	c.mu.Lock()
	for {
		switch c.verdict {
		case allowed:
			c.mu.Unlock()
			return nil
		case disabled:
			c.mu.Unlock()
			return &tool.WebError{Class: tool.ClassToolError, Text: DisabledText}
		case declined:
			c.mu.Unlock()
			return &tool.WebError{Class: tool.ClassToolError, Text: DeclinedText}
		}
		if !c.canAsk || c.asker == nil {
			c.mu.Unlock()
			return &tool.WebError{Class: tool.ClassToolError, Text: DeclinedText}
		}
		if p := c.asking; p != nil {
			c.mu.Unlock()
			select {
			case <-p.done:
			case <-ctx.Done():
				return aborted()
			}
			if p.aborted {
				return aborted()
			}
			c.mu.Lock()
			continue
		}
		p := &pendingAsk{done: make(chan struct{})}
		c.asking = p
		c.mu.Unlock()

		v, ok := c.ask(ctx)

		c.mu.Lock()
		c.asking = nil
		p.aborted = !ok
		if ok {
			c.verdict = v
		}
		close(p.done)
		c.mu.Unlock()
		if !ok {
			return aborted()
		}
		c.remember(v)
		c.mu.Lock()
	}
}

// ask puts the question and reads the answer: allowed for exactly [[0]],
// disabled for exactly [[1]], declined for anything else — unless ctx has
// ended by the time the asker returns, which is no answer at all (ok false)
// whatever the asker brought back: an answer that lands as the asking call
// is cancelled is neither acted on nor remembered.
func (c *consent) ask(ctx context.Context) (v verdict, ok bool) {
	ans, err := c.asker.AskQuestion(ctx, consentQuestion)
	if ctx.Err() != nil {
		return undecided, false
	}
	if err == nil && ans.Answered && len(ans.Picked) == 1 {
		switch {
		case slices.Equal(ans.Picked[0], []int{0}):
			return allowed, true
		case slices.Equal(ans.Picked[0], []int{1}):
			return disabled, true
		}
	}
	return declined, true
}

// remember writes an allow or a disable to the state file. A failed write
// keeps the answer for this session and says so on the warn lane; the next
// session, finding nothing, asks again.
func (c *consent) remember(v verdict) {
	var ch Choice
	switch v {
	case allowed:
		ch = Choice{Provider: c.provider}
	case disabled:
		ch = Choice{Disabled: true}
	default:
		return
	}
	if c.path == "" {
		return
	}
	if err := WriteChoice(c.path, ch); err != nil && c.warn != nil {
		c.warn(WriteFailedText)
	}
}

func aborted() *tool.WebError {
	return &tool.WebError{Class: tool.ClassAborted, Text: tool.AbortedText}
}
