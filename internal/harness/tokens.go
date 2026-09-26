package harness

import (
	"encoding/json"
	"fmt"

	"charm.land/fantasy"
)

// Token estimates (plan 028 §3.7, §3.9). Where no provider has counted a
// request's tokens — the part of the context written since the last reported
// usage, a prompt not yet sent, a tail a compaction keeps — craze estimates
// them as bytes/4, the references' estimator (pi, grok-build, opencode): no
// tokenizer, the same rule for every model, and slightly high for code and
// JSON, which is the safe side for a context window. Images are H8's, and
// are not budgeted here.

// bytesPerToken is the estimator's one constant.
const bytesPerToken = 4

// tokensOf is the estimate for n bytes: n/4, rounded up, so that anything
// that is not empty counts.
func tokensOf(n int) int64 {
	return int64((n + bytesPerToken - 1) / bytesPerToken)
}

// textTokens is the estimate for s, a prompt or the system prompt.
func textTokens(s string) int64 { return tokensOf(len(s)) }

// messageTokens is the estimate for one message of a history: the bytes of
// its JSON as Fantasy writes it, which is how the transcript stores it — its
// text, its reasoning, a call's name and arguments, a result's output, and
// each part's provider metadata, which a request sends back too. It is the
// size the cut weighs a step by (store.Cut).
func messageTokens(m fantasy.Message) int64 {
	b, err := json.Marshal(m)
	if err != nil {
		// Every message a history holds marshals: the store's read back
		// from this JSON, and the rest are plain text. An estimate must
		// not fail, so a message that did not would be weighed by its
		// printed form.
		return textTokens(fmt.Sprint(m))
	}
	return tokensOf(len(b))
}

// tailBudget is the most a compaction's verbatim tail may hold, in tokens
// (plan 028 §3.9, owner decision 1, PD23): tailTokens (models.toml's
// [compaction] tail_tokens), and never more than a quarter of the threshold,
// so a compaction always frees most of what triggered it. threshold is 0 for
// a model whose window is unknown, and the budget is then tailTokens.
func tailBudget(tailTokens, threshold int64) int64 {
	if threshold <= 0 {
		return tailTokens
	}
	return min(tailTokens, threshold/4)
}
