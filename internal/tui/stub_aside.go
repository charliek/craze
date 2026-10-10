package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/charliek/craze/internal/agent"
)

// AsidingStub is the Stub as a session that answers side questions
// (agent.Asider; plan 040 §3.8): what a native, grok or gx session is to the
// engine, the wire and a client — the capability aside, the methods
// session.aside and session.aside.cancel, the aside notifications — with a
// scripted answer and no model behind it. Every other method is the Stub's
// own, the optional interfaces engine.New looks for included, promoted
// through the embedded pointer (as RefreshingStub's are).
//
// A plain Stub is not one: it stays a cursor-like session, so a host over it
// says nothing of aside and refuses both methods — every golden and wire
// fixture from before them, byte for byte. A test opts in by handing the
// engine AsidingStub{Stub: s} (or RefreshingAsidingStub, for a session that
// does both, as a native one does).
//
// The answer is deterministic, StubAsidePieces of the request, streamed one
// piece per onDelta; HoldNextAside parks the next answer after its first
// piece. Asked records every request that reached the session.
type AsidingStub struct{ *Stub }

// Asides is always true: the Stub answers side questions whenever the engine
// lets one through (a closed Stub answers agent.ErrAsideNotReady).
func (a AsidingStub) Asides() bool { return true }

// Aside is the side question's contract (agent.Asider) on the Stub's own
// state (aside).
func (a AsidingStub) Aside(ctx context.Context, req agent.AsideRequest, onDelta func(string)) (agent.AsideResult, error) {
	return a.aside(ctx, req, onDelta)
}

var _ agent.Asider = AsidingStub{}

// RefreshingAsidingStub is the Stub as a session that both takes up models
// while it runs and answers side questions — RefreshingStub's RefreshModels
// and AsidingStub's Asides and Aside, on one value: what a native session is.
// The fake host builds it when both of its opt-ins are on.
type RefreshingAsidingStub struct{ *Stub }

// RefreshModels is RefreshingStub's.
func (r RefreshingAsidingStub) RefreshModels(_ context.Context, nativeDir string) (agent.ModelsRefresh, error) {
	return r.refreshModels(nativeDir)
}

// Asides is AsidingStub's: always true.
func (r RefreshingAsidingStub) Asides() bool { return true }

// Aside is AsidingStub's.
func (r RefreshingAsidingStub) Aside(ctx context.Context, req agent.AsideRequest, onDelta func(string)) (agent.AsideResult, error) {
	return r.aside(ctx, req, onDelta)
}

var (
	_ agent.ModelsRefresher = RefreshingAsidingStub{}
	_ agent.Asider          = RefreshingAsidingStub{}
)

// StubAsidePieces is the Stub's scripted answer to req, piece by piece: what
// AsidingStub streams, one onDelta each, and whose concatenation its result's
// Text is. It names the question and counts it among the side conversation's
// (len(req.Prior)+1), so a follow-up's answer shows its prior arrived.
func StubAsidePieces(req agent.AsideRequest) []string {
	return []string{
		"You asked: ",
		req.Question,
		fmt.Sprintf(" (side question %d).", len(req.Prior)+1),
		" The session goes on.",
	}
}

// stubAsides is what the Stub's side questions read and write, under its mu:
// every request that reached it (Asked), and an armed HoldNextAside.
type stubAsides struct {
	asked []agent.AsideRequest
	hold  *stubHold
}

// HoldNextAside parks the next side question once it has sent its first
// piece, until release is called, its context ends (answering the context's
// error) or the Stub closes (answering context.Canceled, as a session's end
// cancels its side questions): a fixture or a test can then cancel it
// mid-answer, or see a client's view of a partial one. held closes once the
// aside is parked. release may be called more than once.
func (s *Stub) HoldNextAside() (held <-chan struct{}, release func()) {
	return s.armHold(&s.asides.hold)
}

// Asked is every side question that reached the Stub, in order: what a host
// test counts to see a client never asked twice.
func (s *Stub) Asked() []agent.AsideRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]agent.AsideRequest, len(s.asides.asked))
	for i, r := range s.asides.asked {
		out[i] = agent.AsideRequest{Question: r.Question, Prior: slices.Clone(r.Prior)}
	}
	return out
}

// aside answers req with StubAsidePieces, one onDelta per piece, honouring
// ctx before each; a closed Stub answers agent.ErrAsideNotReady. The result's
// model is the current model's display name, and its usage counts scale with
// the side conversation's length, so both are deterministic.
func (s *Stub) aside(ctx context.Context, req agent.AsideRequest, onDelta func(string)) (agent.AsideResult, error) {
	select {
	case <-s.closed:
		return agent.AsideResult{}, agent.ErrAsideNotReady
	default:
	}
	if onDelta == nil {
		onDelta = func(string) {}
	}
	s.mu.Lock()
	s.asides.asked = append(s.asides.asked, agent.AsideRequest{Question: req.Question, Prior: slices.Clone(req.Prior)})
	hold := s.asides.hold
	s.asides.hold = nil
	model := s.snap.CurrentModel
	for _, m := range s.snap.Models {
		if m.ID == s.snap.CurrentModel && m.Name != "" {
			model = m.Name
		}
	}
	s.mu.Unlock()
	pieces := StubAsidePieces(req)
	for i, p := range pieces {
		if err := ctx.Err(); err != nil {
			return agent.AsideResult{}, err
		}
		onDelta(p)
		if i > 0 || hold == nil {
			continue
		}
		close(hold.held)
		select {
		case <-hold.release:
		case <-ctx.Done():
			return agent.AsideResult{}, ctx.Err()
		case <-s.closed:
			return agent.AsideResult{}, fmt.Errorf("stub: the session closed: %w", context.Canceled)
		}
	}
	n := int64(len(req.Prior) + 1)
	return agent.AsideResult{
		Text:  strings.Join(pieces, ""),
		Model: model,
		Usage: &agent.AsideUsage{Input: 1000 * n, Output: int64(len(pieces)), CacheRead: 800 * n},
	}, nil
}
