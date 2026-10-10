package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charliek/craze/internal/harness"
)

// The native session's side questions (/btw, plan 040 §3.4): the harness's
// Aside, with the adapter's part around it — the readiness refusal, the
// question admitted as a prompt is (its shell-context block redacted), the
// ChatGPT plan's usage latch lifted as a person's turn lifts it, everything
// shown sanitized, and the session's spend published when no turn runs. A
// side question is never emitted, journaled or recorded: its only shared
// effect is the usage section's aggregate, which s.spent publishes while the
// session is idle (and the next step's report carries otherwise).

var _ Asider = (*nativeSession)(nil)

// Asides is Asider's: a native session always answers side questions. One
// asked before the session is ready — not started, loading, or closed — is
// refused ErrAsideNotReady, which the engine answers not_accepting, so a
// client's info document never hides /btw from a session still starting.
// One the session had taken when Close ran is ErrAsideInterrupted instead.
func (s *nativeSession) Asides() bool { return true }

// Aside is Asider's (plan 040 §3.4): the harness's side question, its
// answer's pieces sanitized on their way to onDelta, and the result's text,
// model label and error sanitized too — an answer that is nothing once
// sanitized is ErrAsideEmpty. An escape sequence split across two pieces may
// show in the stream, and the final text is what corrects it. ctx also ends
// when the session closes, and a call Close cut short — taken here, past the
// readiness check — is ErrAsideInterrupted, never the refusal
// ErrAsideNotReady. The whole call, the publication of the session's spend
// after it included, is joined by Close, so nothing is published after Close
// returns.
func (s *nativeSession) Aside(ctx context.Context, req AsideRequest, onDelta func(string)) (AsideResult, error) {
	if onDelta == nil {
		onDelta = func(string) {}
	}
	s.mu.Lock()
	hs, signIn := s.hs, s.signIn
	switch {
	case s.closed:
		s.mu.Unlock()
		return AsideResult{}, fmt.Errorf("%w: session closed", ErrAsideNotReady)
	case hs == nil || s.loading:
		// A load's harness is installed before its replay, and the session is
		// not started until the bracket has closed, as prompt has it.
		s.mu.Unlock()
		return AsideResult{}, fmt.Errorf("%w: session not started", ErrAsideNotReady)
	}
	s.asides.Add(1)
	s.mu.Unlock()
	defer s.asides.Done()

	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	// The person's words are never redacted; only a shell-context block's
	// output is, as a prompt's (admit). The earlier pairs are the caller's,
	// as it showed them.
	question, _ := s.admit(req.Question)
	prior := make([]harness.AsidePair, len(req.Prior))
	for i, p := range req.Prior {
		prior[i] = harness.AsidePair{Question: p.Question, Answer: p.Answer}
	}
	// A side question is a person's request, as a turn they start is: it
	// lifts the ChatGPT plan's usage latch (P33).
	if signIn != nil {
		signIn.ClearUsageLimit()
	}
	a, err := hs.Aside(ctx, prior, question, func(delta string) {
		if d := sanitizeText(delta); d != "" {
			onDelta(d)
		}
	})
	// What it spent, failed or not, is in the session's total from now; when
	// no turn runs nothing else would say so until the next one. While one
	// runs, its next report counts it, and the harness owes the report when
	// the turn has none left to make — publishOwedSpend, at its end.
	if e, ok := hs.IdleSpent(); ok {
		if seam := s.asideSeam; seam != nil {
			seam("spent")
		}
		s.spent(e)
	}
	if err != nil {
		return AsideResult{}, s.phraseAside(parent, err)
	}
	text := sanitizeText(a.Text)
	if strings.TrimSpace(text) == "" {
		return AsideResult{}, ErrAsideEmpty
	}
	return AsideResult{
		Text:  text,
		Model: sanitizeLine(a.Model),
		Usage: &AsideUsage{Input: a.Usage.Input, Output: a.Usage.Output, CacheRead: a.Usage.CacheRead},
	}, nil
}

// publishOwedSpend publishes the session's spend when the harness owes it
// (harness.Session.OwedSpent, plan 040 §3.4): a side question finished while
// a turn or a wake ran on hs, after the turn's last report, and its usage is
// in no report published since. prompt and endWake call it once their ending
// is out — the turn's own report is what its end shows (the S2 seam, spent),
// and this one follows it — so the session never sits idle without that
// usage. A turn begun meanwhile leaves the debt to that turn.
func (s *nativeSession) publishOwedSpend(hs *harness.Session) {
	if e, ok := hs.OwedSpent(); ok {
		s.spent(e)
	}
}

// phraseAside is a failed side question in the adapter's words, as an agent
// sentinel where there is one: the caller's context's own error when it
// ended; ErrAsideInterrupted once the session has closed — the call was
// taken (Aside's readiness check), and the session's own shutdown cut it
// short, so it was stopped and not refused; ErrAsideEmpty for an answer with
// no text, a tool call's or an empty step's; and a turn's phrasing of
// anything else (phraseTurn), whose text is sanitized.
func (s *nativeSession) phraseAside(parent context.Context, err error) error {
	switch {
	case parent.Err() != nil:
		return parent.Err()
	case errors.Is(err, harness.ErrClosed), isClosed(s.done):
		return ErrAsideInterrupted
	case errors.Is(err, harness.ErrAsideToolCall):
		return fmt.Errorf("%w: the model tried to use a tool, which a side question cannot do", ErrAsideEmpty)
	case errors.Is(err, harness.ErrAsideEmpty):
		return ErrAsideEmpty
	case errors.Is(err, harness.ErrEmptyStep):
		return fmt.Errorf("%w: %s", ErrAsideEmpty, strings.TrimPrefix(s.phraseTurn(err).Error(), "native: "))
	}
	return s.phraseTurn(err)
}

// isClosed reports whether ch is closed, without waiting.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
