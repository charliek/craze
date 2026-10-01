package harness

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"strings"

	// The image formats an estimate reads the size of (imageTokens): what
	// the attachments path stores and a tool returns (plan 033 §3.2), gif
	// for good measure. Only their headers are ever read here.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"charm.land/fantasy"
	_ "golang.org/x/image/webp"
)

// Token estimates (plan 028 §3.7, §3.9). Where no provider has counted a
// request's tokens — the part of the context written since the last reported
// usage, a prompt not yet sent, a tail a compaction keeps — craze estimates
// them as bytes/4, the references' estimator (pi, grok-build, opencode): no
// tokenizer, the same rule for every model, and slightly high for code and
// JSON, which is the safe side for a context window. An image is not text:
// it is estimated by its size (imageTokens, plan 033 P9), never by the bytes
// of its base64.

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
//
// An image in it — an attached one (a fantasy.FilePart) or one a tool
// returned (a fantasy.ToolResultOutputContentMedia), of an image/* type — is
// weighed by imageTokens instead of its bytes (plan 033 §3.5, P9), with the
// rest of its part still counted as JSON: base64 at bytes/4 would count a
// 1 MB screenshot as some 350k tokens, against the 1–2k a provider bills,
// and a single paste would set off a compaction or ErrContextTooLarge.
func messageTokens(m fantasy.Message) int64 {
	var images int64
	m = mapParts(m, func(p fantasy.MessagePart) (fantasy.MessagePart, bool) {
		if f, ok := fantasy.AsMessagePart[fantasy.FilePart](p); ok && isImage(f.MediaType) {
			images += imageTokens(bytes.NewReader(f.Data))
			f.Data = nil
			return f, true
		}
		r, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](p)
		if !ok {
			return p, false
		}
		o, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](r.Output)
		if !ok || !isImage(o.MediaType) {
			return p, false
		}
		images += imageTokens(base64.NewDecoder(base64.StdEncoding, strings.NewReader(o.Data)))
		o.Data = ""
		r.Output = o
		return r, true
	})
	b, err := json.Marshal(m)
	if err != nil {
		// Every message a history holds marshals: the store's read back
		// from this JSON, and the rest are plain text. An estimate must
		// not fail, so a message that did not would be weighed by its
		// printed form.
		return textTokens(fmt.Sprint(m)) + images
	}
	return tokensOf(len(b)) + images
}

// The image estimate (plan 033 P9): one token per imageTile×imageTile pixels,
// counting part tiles whole, and never more than maxImageTokens, which is
// also what an image whose size cannot be read is taken to cost.
const (
	imageTile      = 28
	maxImageTokens = 1600
)

// imageTokens is the estimate for the image r reads: ⌈w/28⌉·⌈h/28⌉ from the
// width and height its header gives, capped at maxImageTokens, and
// maxImageTokens when no header can be read. image.DecodeConfig reads the
// header alone and decodes no pixels — microseconds, through a base64 decoder
// too — so nothing is cached: no key that tells two images apart (a hash of
// the bytes) costs less than the read it would save.
func imageTokens(r io.Reader) int64 {
	cfg, _, err := image.DecodeConfig(r)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return maxImageTokens
	}
	tiles := int64((cfg.Width+imageTile-1)/imageTile) * int64((cfg.Height+imageTile-1)/imageTile)
	return min(tiles, maxImageTokens)
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
