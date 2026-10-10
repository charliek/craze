package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
)

// asidesMax is how many side questions may be in flight on one engine, from
// every client together (plan 040 §3.2): a third is refused ErrAsideBusy.
const asidesMax = 2

// The side question's refusals (plan 040 §3.3). ErrBadAside and
// ErrAsideTooLong ride sentinels classify already places — bad_request and
// text_too_long — so a client over the socket reconstructs what an
// in-process caller matches; ErrAsideBusy has a reason of its own.
var (
	// ErrBadAside refuses a malformed side question: a blank question, or
	// more prior pairs than protocol.AsidePriorMax. It is ErrBadRequest.
	ErrBadAside = fmt.Errorf("%w: side question", ErrBadRequest)
	// ErrAsideTooLong refuses a side question whose question, or a prior
	// question or answer, is over its bound (protocol.AsideQuestionMax,
	// protocol.AsideAnswerMax), in bytes. It is agent.ErrQueueTextTooLong,
	// code text_too_long.
	ErrAsideTooLong = fmt.Errorf("%w: side question", agent.ErrQueueTextTooLong)
	// ErrAsideBusy refuses a side question while asidesMax are already in
	// flight on the session: code unavailable, reason aside_busy. Nothing was
	// asked of the model; ask again once one has answered.
	ErrAsideBusy = errors.New("engine: two side questions are already in flight on this session")
)

// Asides reports whether the session answers side questions (/btw, plan
// 040): it is an agent.Asider whose Asides says so — the session capability
// aside on the wire, and the TUI's in-process Info alike. It waits on
// nothing.
func (e *Engine) Asides() bool { return e.asider() != nil }

// asider is the session as the agent.Asider that serves side questions now,
// nil when it is none or its Asides says no.
func (e *Engine) asider() agent.Asider {
	if a, ok := e.sess.(agent.Asider); ok && a.Asides() {
		return a
	}
	return nil
}

// Aside answers one side question from the session's context (plan 040 §3.3),
// calling onDelta with each piece of a streamed answer; onDelta must not
// block, and nil is none. It is the session's own verb (agent.Asider), and
// the engine adds only the door:
//
//   - ErrBadAside (bad_request) or ErrAsideTooLong (text_too_long): the
//     request is outside the protocol's bounds. Nothing was asked;
//   - ErrNotAccepting (not_accepting), or ErrClosing (unavailable, closing):
//     the engine admits nothing now (refusalLocked) — closed, stopped, its
//     session not up, replaying, or a close fence up — or the session itself
//     answered agent.ErrAsideNotReady, ctx still live. Nothing was asked;
//   - agent.ErrAsideUnsupported (unsupported, aside_unsupported): the session
//     has no side questions, or its agent turned out not to serve them;
//   - ErrAsideBusy (unavailable, aside_busy): asidesMax are in flight;
//   - the context's error (aborted, context): ctx ended — before the call, or
//     during it, whatever the session's own error then said (even
//     agent.ErrAsideNotReady: the context's end wins);
//   - agent.ErrAsideInterrupted (aborted, context): the session's own
//     shutdown cut short a call it had taken — a host closing the engine
//     while another client's aside streams — whatever ctx says. It is
//     context.Canceled, and passes through as it is: stopped, not refused;
//   - any other error, agent.ErrAsideEmpty among them (failed): the model or
//     the agent failed, or answered nothing.
//
// It is not a command: no Command, no receipt, nothing journaled, published
// or fenced. It blocks for the whole answer, so it belongs on a goroutine of
// its own, never the primary's reader; e.mu is held only to read the gate and
// count the call in, and again to count it out — in a defer, so a panicking
// session leaves the count as it found it.
func (e *Engine) Aside(ctx context.Context, req agent.AsideRequest, onDelta func(string)) (agent.AsideResult, error) {
	if err := checkAside(req); err != nil {
		return agent.AsideResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return agent.AsideResult{}, err
	}
	a := e.asider()
	e.mu.Lock()
	refused := e.refusalLocked()
	switch {
	case refused != nil:
	case a == nil:
		refused = agent.ErrAsideUnsupported
	case e.asides >= asidesMax:
		refused = ErrAsideBusy
	default:
		e.asides++
	}
	e.mu.Unlock()
	if refused != nil {
		return agent.AsideResult{}, refused
	}
	defer e.asideDone()
	if onDelta == nil {
		onDelta = func(string) {}
	}
	r, err := a.Aside(ctx, req, onDelta)
	switch {
	case err == nil:
		return r, nil
	case ctx.Err() != nil:
		// Looked at before any other translation: the session's own error
		// for a call its context ended — a closed pipe, a provider's "request
		// aborted", agent.ErrAsideNotReady from a shutdown the cancel raced —
		// is the cancel, not a failure or a refusal.
		if !errors.Is(err, ctx.Err()) {
			err = fmt.Errorf("%w: %w", ctx.Err(), err)
		}
	case errors.Is(err, agent.ErrAsideNotReady):
		err = fmt.Errorf("%w: %w", ErrNotAccepting, err)
	}
	return agent.AsideResult{}, err
}

// asideDone counts one side question out.
func (e *Engine) asideDone() {
	e.mu.Lock()
	e.asides--
	e.mu.Unlock()
}

// checkAside holds a side question to the protocol's bounds (plan 040 §3.2),
// in bytes: a blank question or too many prior pairs is ErrBadAside, a
// question or a prior question or answer over its bound ErrAsideTooLong.
func checkAside(req agent.AsideRequest) error {
	if strings.TrimSpace(req.Question) == "" {
		return fmt.Errorf("%w: the question is blank", ErrBadAside)
	}
	if n := len(req.Prior); n > protocol.AsidePriorMax {
		return fmt.Errorf("%w: %d earlier questions, over %d", ErrBadAside, n, protocol.AsidePriorMax)
	}
	if n := len(req.Question); n > protocol.AsideQuestionMax {
		return fmt.Errorf("%w: the question is %d bytes, over %d", ErrAsideTooLong, n, protocol.AsideQuestionMax)
	}
	for i, p := range req.Prior {
		if n := len(p.Question); n > protocol.AsideQuestionMax {
			return fmt.Errorf("%w: earlier question %d is %d bytes, over %d", ErrAsideTooLong, i+1, n, protocol.AsideQuestionMax)
		}
		if n := len(p.Answer); n > protocol.AsideAnswerMax {
			return fmt.Errorf("%w: earlier answer %d is %d bytes, over %d", ErrAsideTooLong, i+1, n, protocol.AsideAnswerMax)
		}
	}
	return nil
}
