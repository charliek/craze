package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charliek/craze/internal/acp"
)

// The live ACP session's side questions (/btw, plan 040 §3.5): grok's and
// gx's x.ai/btw, which answers in one piece beside a running turn. Nothing is
// journaled or emitted: the question is not a prompt and the answer is not
// part of the transcript.

var _ Asider = (*session)(nil)

const (
	// AsideFoldMax bounds the question x.ai/btw is sent once the earlier
	// pairs are folded in (x.ai/btw takes no history): 64 KiB. The most
	// recent pairs that fit are kept, the oldest dropped first.
	AsideFoldMax = 64 << 10
	// asideFoldAnswerMax bounds one folded answer; a longer one keeps its head.
	asideFoldAnswerMax = 8 << 10
	// asideTruncated marks a folded answer that was cut.
	asideTruncated = "[…truncated]"
)

// Asides is Asider's: the provider's bit (grok and gx on, cursor off).
func (s *session) Asides() bool { return s.provider().Aside() }

// Aside is Asider's. onDelta is never called: x.ai/btw answers in one piece.
// Cancelling ctx only abandons the wait; grok keeps generating. One asked of
// a session not started, or closed, is refused ErrAsideNotReady; one the
// session's own Close cut short — the client torn down under the call — is
// ErrAsideInterrupted.
func (s *session) Aside(ctx context.Context, req AsideRequest, onDelta func(string)) (AsideResult, error) {
	if !s.Asides() {
		return AsideResult{}, ErrAsideUnsupported
	}
	s.mu.Lock()
	client := s.client
	ready := s.started && !s.closed
	model := s.modelNameLocked()
	s.mu.Unlock()
	if client == nil || !ready {
		return AsideResult{}, fmt.Errorf("%w: session not started", ErrAsideNotReady)
	}
	answer, err := client.Aside(ctx, foldAside(req))
	switch {
	case errors.Is(err, acp.ErrUnsupported):
		return AsideResult{}, fmt.Errorf("%w: this agent does not serve x.ai/btw; update grok or gx", ErrAsideUnsupported)
	case errors.Is(err, acp.ErrNoSession):
		return AsideResult{}, fmt.Errorf("%w: no session", ErrAsideNotReady)
	case errors.Is(err, acp.ErrEmptyAnswer):
		return AsideResult{}, ErrAsideEmpty
	case errors.Is(err, acp.ErrClosed) && ctx.Err() == nil && isClosed(s.done):
		// teardown closes done before it closes the client, whose close is
		// what failed the call (acp.ErrClosed): stopped, not refused, and not
		// the agent's failure either. Only that error says the shutdown cut
		// the call short; any other failure that merely lands after Close
		// began is the agent's own and stays as it is (plan 040, C5 review).
		return AsideResult{}, ErrAsideInterrupted
	case err != nil:
		return AsideResult{}, err
	}
	text := strings.TrimSpace(sanitizeText(answer))
	if text == "" {
		return AsideResult{}, ErrAsideEmpty
	}
	return AsideResult{Text: text, Model: strings.TrimSpace(sanitizeText(model))}, nil
}

// modelNameLocked is the display name of the session's current model, or its
// id when the catalog does not name it. s.mu is held.
func (s *session) modelNameLocked() string {
	id := s.snap.CurrentModel
	for _, m := range s.snap.Models {
		if m.ID == id && m.Name != "" {
			return m.Name
		}
	}
	return id
}

// foldAside is the question x.ai/btw is sent: the question alone, or with the
// earlier pairs folded in ahead of it, within AsideFoldMax.
func foldAside(req AsideRequest) string {
	if len(req.Prior) == 0 {
		return req.Question
	}
	const head = "Earlier in this side conversation:\n\n"
	tail := "Now: " + req.Question
	budget := AsideFoldMax - len(head) - len(tail)
	// Walk from the newest pair back, keeping what fits.
	var pairs []string
	for i := len(req.Prior) - 1; i >= 0; i-- {
		p := req.Prior[i]
		block := "Q: " + p.Question + "\nA: " + foldTruncate(p.Answer) + "\n\n"
		if len(block) > budget {
			break
		}
		budget -= len(block)
		pairs = append(pairs, block)
	}
	if len(pairs) == 0 {
		return req.Question
	}
	var b strings.Builder
	b.WriteString(head)
	for i := len(pairs) - 1; i >= 0; i-- {
		b.WriteString(pairs[i])
	}
	b.WriteString(tail)
	return b.String()
}

// foldTruncate keeps the head of a long answer, cut on a rune boundary, and
// marks the cut.
func foldTruncate(s string) string {
	if len(s) <= asideFoldAnswerMax {
		return s
	}
	cut := asideFoldAnswerMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + asideTruncated
}
