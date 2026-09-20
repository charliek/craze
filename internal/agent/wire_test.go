package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// These tests pin issue #18: a session/cancel must never reach the agent ahead
// of the session/prompt it means to stop. Prompt raises inPrompt before its
// request is written, so Cancel waits for the turn's wire outcome and writes
// after it. Most of them play the agent in-process, over two unbuffered pipes,
// so that what craze wrote — and, as exactly, what it did not — is read off
// the wire rather than inferred from a fake's stop reason.

// inProcess is a session on an acp client whose agent is the test itself.
// Every frame craze writes is read here, one at a time and only when the test
// asks, so a write nobody has asked for yet stays blocked in the unbuffered
// pipe: that is how a test holds a write in flight. serverR is the agent's end
// of craze's write direction; closing it fails craze's next write and leaves
// the other direction, and the connection, open.
type inProcess struct {
	s       *session
	client  *acp.Client
	enc     *acp.Encoder
	serverR *io.PipeReader
	want    chan struct{}
	got     chan wireFrame
	asked   bool
}

type wireFrame struct {
	msg *acp.Message
	err error
}

// newInProcess builds the least a session needs for Prompt and Cancel: a
// client over the test's pipes, set as s.client with s.started, and a session
// id taught through session/new, because Client.Cancel writes nothing without
// one. wrap, when set, sits between the client and its pipe.
func newInProcess(t *testing.T, dialect acp.DialectID, wrap func(io.Writer) io.Writer) *inProcess {
	t.Helper()
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	var out io.Writer = clientW
	if wrap != nil {
		out = wrap(clientW)
	}
	client := acp.DialWithDialect(clientR, out, dialect)
	p := &inProcess{
		client:  client,
		enc:     acp.NewEncoder(serverW),
		serverR: serverR,
		want:    make(chan struct{}, 1),
		got:     make(chan wireFrame, 1),
	}
	dec := acp.NewDecoder(serverR)
	go func() {
		for range p.want {
			msg, err := dec.ReadMessage()
			p.got <- wireFrame{msg, err}
			if err != nil {
				return
			}
		}
	}()
	var opts Options
	if dialect == acp.DialectGrok {
		grok := GrokProvider()
		opts.Provider = &grok
	}
	p.s = newTestSession(t, opts)
	p.s.mu.Lock()
	p.s.client = client
	p.s.started = true
	p.s.mu.Unlock()
	t.Cleanup(func() {
		// The pipes go first: a Close that finds a turn in flight writes a
		// cancel, and on a pipe nobody reads any more that write would never
		// return.
		_ = serverR.Close()
		_ = serverW.Close()
		_ = clientR.Close()
		_ = clientW.Close()
		_ = p.s.Close()
		close(p.want)
	})

	cwd := t.TempDir()
	created := make(chan error, 1)
	go func() {
		_, err := client.NewSession(context.Background(), cwd)
		created <- err
	}()
	req := p.expect(t, acp.MethodSessionNew)
	p.reply(t, req.ID, map[string]string{"sessionId": "s1"})
	select {
	case err := <-created:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session/new never returned")
	}
	return p
}

// next is the next frame craze wrote, or nil when none arrives within d. A
// read that timed out stays outstanding, so the frame it eventually gets is
// the one the following call returns: nothing is ever skipped.
func (p *inProcess) next(t *testing.T, d time.Duration) *acp.Message {
	t.Helper()
	if !p.asked {
		p.want <- struct{}{}
		p.asked = true
	}
	select {
	case f := <-p.got:
		p.asked = false
		if f.err != nil {
			t.Fatalf("reading what craze wrote: %v", f.err)
		}
		return f.msg
	case <-time.After(d):
		return nil
	}
}

func (p *inProcess) expect(t *testing.T, method string) *acp.Message {
	t.Helper()
	msg := p.next(t, 5*time.Second)
	if msg == nil {
		t.Fatalf("craze never wrote %s", method)
	}
	if msg.Method != method {
		t.Fatalf("craze wrote %s, want %s", msg.Method, method)
	}
	return msg
}

// expectCancel reads the one session/cancel a Cancel writes, for this
// session.
func (p *inProcess) expectCancel(t *testing.T) {
	t.Helper()
	msg := p.expect(t, acp.MethodSessionCancel)
	var params acp.CancelParams
	if err := json.Unmarshal(msg.Params, &params); err != nil || params.SessionID != "s1" {
		t.Fatalf("session/cancel params %s (%v)", msg.Params, err)
	}
}

// expectNothing is the exact negative: a read that must time out. The pipe is
// unbuffered and the test is its only reader, so anything craze wrote would
// be here; a write decided on is one marshal away, far inside the bound.
func (p *inProcess) expectNothing(t *testing.T, why string) {
	t.Helper()
	if msg := p.next(t, 200*time.Millisecond); msg != nil {
		t.Fatalf("%s: craze wrote %s", why, msg.Method)
	}
}

func (p *inProcess) reply(t *testing.T, id json.RawMessage, result any) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.enc.WriteMessage(&acp.Message{ID: id, Result: raw}); err != nil {
		t.Fatal(err)
	}
}

func (p *inProcess) replyErr(t *testing.T, id json.RawMessage, msg string) {
	t.Helper()
	if err := p.enc.WriteMessage(&acp.Message{ID: id, Error: &acp.RPCError{Code: -32000, Message: msg}}); err != nil {
		t.Fatal(err)
	}
}

func (p *inProcess) notify(t *testing.T, method, params string) {
	t.Helper()
	if err := p.enc.WriteMessage(&acp.Message{Method: method, Params: json.RawMessage(params)}); err != nil {
		t.Fatal(err)
	}
}

// startForeignTurn has grok say it is running a turn of its own, which is
// what makes the client refuse a prompt with ErrForeignTurn. The broadcast
// needs the session id session/new taught the client.
func (p *inProcess) startForeignTurn(t *testing.T) {
	t.Helper()
	p.notify(t, acp.MethodGrokQueueChangedWrapped,
		`{"sessionId":"s1","entries":[],"runningPromptId":"interject-fallback-1","runningText":"note","runningKind":"prompt"}`)
	waitFor(t, "the foreign turn", p.client.ForeignTurnRunning)
}

// failPrompts wraps w so that every session/prompt frame written through it
// fails and every other frame passes on, so a prompt's write fails while the
// pipe — and any cancel written to it — still works. It is a wrap for
// newInProcess.
func failPrompts(w io.Writer) io.Writer { return failPromptWriter{w} }

type failPromptWriter struct{ w io.Writer }

var errPromptWrite = errors.New("test: the prompt's write failed")

func (f failPromptWriter) Write(b []byte) (int, error) {
	if bytes.Contains(b, []byte(`"method":"session/prompt"`)) {
		return 0, errPromptWrite
	}
	return f.w.Write(b)
}

// wireSeam holds prompts at testBeforeWire, the point where a turn is open and
// its request not yet written. Each prompt that reaches it hands over its
// turn's wire and parks until the test lets it go, its session closes, or the
// test ends: parking against s.done and releasing everything in t.Cleanup is
// what keeps a failing assertion from hanging the package on a prompt nobody
// would ever release.
type wireSeam struct {
	at   chan *turnWire
	next chan struct{}
	all  chan struct{}
	once sync.Once
}

func holdBeforeWire(t *testing.T) *wireSeam {
	t.Helper()
	w := &wireSeam{at: make(chan *turnWire, 8), next: make(chan struct{}), all: make(chan struct{})}
	testBeforeWire = func(s *session) {
		s.mu.Lock()
		wire := s.wire
		s.mu.Unlock()
		select {
		case w.at <- wire:
		case <-w.all:
		case <-s.done:
		}
		select {
		case <-w.next:
		case <-w.all:
		case <-s.done:
		}
	}
	t.Cleanup(func() {
		w.releaseAll()
		testBeforeWire = nil
	})
	return w
}

// parked is the wire of the next prompt to reach the seam.
func (w *wireSeam) parked(t *testing.T) *turnWire {
	t.Helper()
	select {
	case wire := <-w.at:
		if wire == nil {
			t.Fatal("a prompt reached the wire with no wire outcome to report")
		}
		return wire
	case <-time.After(5 * time.Second):
		t.Fatal("no prompt reached the wire")
		return nil
	}
}

// letOne lets exactly one parked prompt go on to the client.
func (w *wireSeam) letOne(t *testing.T) {
	t.Helper()
	select {
	case w.next <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("no prompt was parked to let go")
	}
}

func (w *wireSeam) releaseAll() { w.once.Do(func() { close(w.all) }) }

// waitCancelling is the barrier every wait-and-write test needs before it lets
// the prompt move: Cancel marks the turn cancelling in the same locked section
// that reads the turn and its wire, so once the mark is visible Cancel has
// committed to that wire, and it can only write after the wire says what
// became of the prompt.
func waitCancelling(t *testing.T, s *session) {
	t.Helper()
	waitFor(t, "Cancel to mark the turn cancelling", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cancelling
	})
}

func cancelOn(ctx context.Context, s *session) <-chan error {
	out := make(chan error, 1)
	go func() { _, err := s.Cancel(ctx); out <- err }()
	return out
}

// cancelBounded is cancelOn under its own 15s context, so a Cancel that
// regressed into waiting forever fails its test with a message instead of
// hanging the package.
func cancelBounded(t *testing.T, s *session) <-chan error {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	return cancelOn(ctx, s)
}

// cancelOutcomeResult is a Cancel call's outcome and error together, for the
// tests that pin CancelOutcome rather than only the error.
type cancelOutcomeResult struct {
	outcome CancelOutcome
	err     error
}

// cancelOutcomeOn is cancelOn, keeping the CancelOutcome alongside the error.
func cancelOutcomeOn(ctx context.Context, s *session) <-chan cancelOutcomeResult {
	out := make(chan cancelOutcomeResult, 1)
	go func() {
		outcome, err := s.Cancel(ctx)
		out <- cancelOutcomeResult{outcome, err}
	}()
	return out
}

// cancelOutcomeBounded is cancelBounded, keeping the CancelOutcome alongside
// the error.
func cancelOutcomeBounded(t *testing.T, s *session) <-chan cancelOutcomeResult {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	return cancelOutcomeOn(ctx, s)
}

func cancelOutcomeStillWaiting(t *testing.T, cancelled <-chan cancelOutcomeResult, why string) {
	t.Helper()
	select {
	case r := <-cancelled:
		t.Fatalf("Cancel returned outcome %+v err %v %s", r.outcome, r.err, why)
	default:
	}
}

func cancelOutcomeReturn(t *testing.T, out <-chan cancelOutcomeResult, d time.Duration) cancelOutcomeResult {
	t.Helper()
	select {
	case r := <-out:
		return r
	case <-time.After(d):
		t.Fatal("Cancel never returned")
		return cancelOutcomeResult{}
	}
}

// openTurnByHand sets exactly the turn state Cancel reads — inPrompt, the
// claim, the wire, promptDone — with no Prompt behind it, and returns what
// ends that turn as a release would. Ending it twice is harmless, and the
// test's cleanup ends it too, so a failed assertion never leaves a Cancel
// waiting on a turn nobody will end.
func openTurnByHand(t *testing.T, s *session, in, claimed bool, wire *turnWire) (end func()) {
	t.Helper()
	promptDone := make(chan struct{})
	s.mu.Lock()
	s.inPrompt = in
	s.claimed = claimed
	s.wire = wire
	s.promptDone = promptDone
	s.mu.Unlock()
	var once sync.Once
	end = func() {
		once.Do(func() {
			s.mu.Lock()
			s.inPrompt = false
			s.claimed = false
			s.wire = nil
			close(promptDone)
			s.mu.Unlock()
		})
	}
	t.Cleanup(end)
	return end
}

func cancelReturn(t *testing.T, out <-chan error, d time.Duration) error {
	t.Helper()
	select {
	case err := <-out:
		return err
	case <-time.After(d):
		t.Fatal("Cancel never returned")
		return nil
	}
}

// cancelStillWaiting fails if Cancel has already returned. It is called only
// where nothing can have ended what Cancel is waiting on — its wire, or the
// turn behind it — so it is a statement about what Cancel did, not a guess
// about scheduling.
func cancelStillWaiting(t *testing.T, cancelled <-chan error, why string) {
	t.Helper()
	select {
	case err := <-cancelled:
		t.Fatalf("Cancel returned %v %s", err, why)
	default:
	}
}

// settled reports a wire's outcome, and whether it is settled at all.
func settled(s *session, w *turnWire) (wireOutcome, bool) {
	select {
	case <-w.done:
		return s.outcomeOf(w), true
	default:
		return wirePending, false
	}
}

// String names an outcome in a failure message.
func (o wireOutcome) String() string {
	switch o {
	case wirePending:
		return "pending"
	case wireSent:
		return "sent"
	case wireRefused:
		return "refused"
	case wireFailed:
		return "failed"
	case wireWithdrawn:
		return "withdrawn"
	}
	return "unknown"
}

// callReceipts are the `calls: ` lines the callorder script has sent so far,
// in order. Each is one prompt's receipt of every session/prompt and
// session/cancel the fake had read when it answered.
func callReceipts(log *eventLog) []string {
	var out []string
	for _, line := range strings.Split(texts(log.snapshot()), "\n") {
		if strings.HasPrefix(line, "calls: ") {
			out = append(out, line)
		}
	}
	return out
}

// TestCancelWaitsForThePromptToBeWritten is issue #18 made deterministic,
// against the real fake agent. The prompt is parked at the seam — its turn
// open, inPrompt true, nothing written — and Cancel runs while it sits there,
// which is the window Esc-right-after-Enter lands in. The barrier is
// Cancel's own mark: from then on its write can only come after the wire
// outcome, and the outcome cannot be published while the prompt is parked. So
// the order the fake reads is prompt, then cancel, every time.
//
// It detects the old code just as deterministically: that Cancel wrote the
// moment it had marked the turn, while the prompt was still parked, so the
// first receipt began with the cancel. Prompt A's own receipt is not asserted:
// the fake builds it on its handler goroutine while the read loop goes on
// noting arrivals, so it may read `prompt` or `prompt,cancel`. Prompt B's is
// written after both, and is exact.
func TestCancelWaitsForThePromptToBeWritten(t *testing.T) {
	s := startScript(t, "callorder", true)
	log := collect(t, s)
	seam := holdBeforeWire(t)

	outA := promptOn(s, "first")
	seam.parked(t)

	cancelled := cancelBounded(t, s)
	waitCancelling(t, s)
	seam.letOne(t)

	if err := cancelReturn(t, cancelled, 15*time.Second); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := promptReturn(t, outA, "the first prompt never came back"); err != nil {
		t.Fatalf("first prompt: %v", err)
	}
	outB := promptOn(s, "second")
	seam.letOne(t)
	if err := promptReturn(t, outB, "the second prompt never came back"); err != nil {
		t.Fatalf("second prompt: %v", err)
	}

	var receipts []string
	waitFor(t, "both receipts", func() bool {
		receipts = callReceipts(log)
		return len(receipts) == 2
	})
	for _, r := range receipts {
		if strings.HasPrefix(r, "calls: cancel") {
			t.Fatalf("the cancel reached the agent ahead of its prompt: receipts %q", receipts)
		}
	}
	if receipts[1] != "calls: prompt,cancel,prompt" {
		t.Fatalf("second receipt %q, want %q", receipts[1], "calls: prompt,cancel,prompt")
	}
}

// TestCancelDecidesOnTheWireOutcome is Cancel's decision on its own, over every
// state its one locked section can read: whether a turn of craze's own is open,
// whether a prompt has claimed the slot, and what that prompt's wire says —
// already, or once Cancel has started waiting. The state is set by hand because
// the decision reads nothing else; the schedules that reach each state through
// a real Prompt or Begin are the tests below and in claim_test.go. "Nothing
// written" is exact: the test is the agent, and a read that must time out is
// the proof.
func TestCancelDecidesOnTheWireOutcome(t *testing.T) {
	cases := []struct {
		name string
		in   bool
		// claimed is Begin's claim, which holds from before the turn opens
		// until the prompt returns: claimed and not in is a prompt whose
		// continuation has not opened its turn yet.
		claimed bool
		// noWire leaves s.wire nil. start is the wire's outcome when Cancel
		// reads it; publish, when start is pending, is what the prompt then
		// says while Cancel waits.
		noWire  bool
		start   wireOutcome
		publish wireOutcome
		// opens is what happens alongside the publish, before Cancel reads
		// the turn again: the claimed prompt's own turn opening, or a later
		// prompt's.
		opens  turnOpening
		writes bool
		// waits: Cancel stays until the turn ends, as it always has.
		waits bool
	}{
		{name: "no turn", noWire: true, writes: true},
		{name: "no turn, a wire left behind", start: wirePending, writes: true},
		{name: "a turn with no wire", in: true, noWire: true, writes: true},
		{name: "sent", in: true, start: wireSent, writes: true, waits: true},
		{name: "refused", in: true, start: wireRefused, writes: true, waits: true},
		{name: "failed", in: true, start: wireFailed, waits: true},
		{name: "withdrawn", in: true, start: wireWithdrawn, waits: true},
		{name: "pending, then sent", in: true, start: wirePending, publish: wireSent, writes: true, waits: true},
		{name: "pending, then refused", in: true, start: wirePending, publish: wireRefused, writes: true, waits: true},
		{name: "pending, then failed", in: true, start: wirePending, publish: wireFailed, waits: true},
		{name: "pending, then withdrawn", in: true, start: wirePending, publish: wireWithdrawn, waits: true},

		// A claimed prompt whose turn is open: every turn, once claims exist.
		{name: "claimed and open, sent", in: true, claimed: true, start: wireSent, writes: true, waits: true},
		{name: "claimed and open, pending, then sent", in: true, claimed: true, start: wirePending, publish: wireSent, writes: true, waits: true},
		{name: "claimed and open, pending, then failed", in: true, claimed: true, start: wirePending, publish: wireFailed, waits: true},

		// Claimed and not open: Esc between Enter and the prompt's goroutine.
		// The prompt withdraws, or fails before its turn opens, and nothing
		// is written for it; with no turn open there is no turn to wait for.
		{name: "claimed, pending, then withdrawn", claimed: true, start: wirePending, publish: wireWithdrawn},
		{name: "claimed, pending, then failed", claimed: true, start: wirePending, publish: wireFailed},
		{name: "claimed, withdrawn", claimed: true, start: wireWithdrawn},
		{name: "claimed, failed", claimed: true, start: wireFailed},
		// The decision is the outcome's, whatever the claim: a claimed prompt
		// that did reach the wire is cancelled like any other.
		{name: "claimed, pending, then sent", claimed: true, start: wirePending, publish: wireSent, writes: true},
		{name: "claimed, pending, then refused", claimed: true, start: wirePending, publish: wireRefused, writes: true},
		{name: "claimed with no wire", claimed: true, noWire: true, writes: true},
		// The turn is read again after the wire settles: one that opened in the
		// meantime is waited for if it is the claimed prompt's own, and not if
		// it is a later prompt's.
		{name: "claimed, then its turn opens and is sent", claimed: true, start: wirePending, publish: wireSent, opens: openClaimed, writes: true, waits: true},
		{name: "claimed, withdrawn while a later turn is open", claimed: true, start: wirePending, publish: wireWithdrawn, opens: openLater},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newInProcess(t, acp.DialectCursor, nil)
			s := p.s
			var wire *turnWire
			if !tc.noWire {
				wire = &turnWire{done: make(chan struct{})}
				if tc.start != wirePending {
					s.publishWire(wire, tc.start)
				}
			}
			endTurn := openTurnByHand(t, s, tc.in, tc.claimed, wire)

			cancelled := cancelOutcomeBounded(t, s)
			if tc.publish != wirePending {
				waitCancelling(t, s)
				p.expectNothing(t, "a cancel went out while its prompt's write was still pending")
				cancelOutcomeStillWaiting(t, cancelled, "while the wire was still pending")
				s.mu.Lock()
				switch tc.opens {
				case openClaimed:
					s.inPrompt = true
				case openLater:
					s.inPrompt = true
					s.wire = &turnWire{done: make(chan struct{})}
				}
				s.mu.Unlock()
				s.publishWire(wire, tc.publish)
			}
			if tc.writes {
				p.expectCancel(t)
			}
			p.expectNothing(t, "nothing more may be written")
			if tc.waits {
				cancelOutcomeStillWaiting(t, cancelled, "before the turn ended")
				endTurn()
			}
			r := cancelOutcomeReturn(t, cancelled, 5*time.Second)
			if r.err != nil {
				t.Fatalf("cancel: %v", r.err)
			}
			// CancelOutcome (plan 021 §3.7): Wrote is exactly whether this case
			// writes to the agent at all — every writing path here succeeds, so
			// there is no case where Cancel wrote nothing yet still fired a
			// notification craze does not know about. Withdrew is set only when
			// the wire this Cancel was tracking settled wireWithdrawn: wireFailed
			// is a prompt that ended some other way, not a withdrawal this call
			// caused. Every case here either wrote or waited for its own turn's
			// promptDone to close (or found nothing left to wait for at all), so
			// Settled is always true — TestCancelWaitIsBoundedByItsContext and
			// TestCancelWaitEndsWhenTheSessionCloses hold the false cases.
			final := tc.start
			if tc.publish != wirePending {
				final = tc.publish
			}
			want := CancelOutcome{Wrote: tc.writes, Withdrew: final == wireWithdrawn, Settled: true}
			if r.outcome != want {
				t.Fatalf("CancelOutcome = %+v, want %+v", r.outcome, want)
			}
		})
	}
}

// turnOpening is what TestCancelDecidesOnTheWireOutcome has the session do
// alongside a publish; the zero value is nothing.
type turnOpening int

const (
	// openClaimed: the claimed prompt's own turn opens — inPrompt, same wire.
	openClaimed turnOpening = iota + 1
	// openLater: a later prompt's turn is open, on a wire of its own.
	openLater
)

// TestCancelReportsAFailedCancelWrite: when the cancel's own write fails,
// Cancel says so — that error is what reaches the TUI as a failed cancel —
// whether a prompt of craze's own was written first or nothing was running.
func TestCancelReportsAFailedCancelWrite(t *testing.T) {
	t.Run("no turn", func(t *testing.T) {
		p := newInProcess(t, acp.DialectCursor, nil)
		_ = p.serverR.Close()
		outcome, err := p.s.Cancel(t.Context())
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("cancel over a closed write direction: %v", err)
		}
		// Nothing of craze's own was running either way, so Settled is true
		// whether or not the write itself succeeded.
		if want := (CancelOutcome{Settled: true}); outcome != want {
			t.Fatalf("CancelOutcome = %+v, want %+v", outcome, want)
		}
	})
	t.Run("a written prompt", func(t *testing.T) {
		p := newInProcess(t, acp.DialectCursor, nil)
		out := promptOn(p.s, "hi")
		req := p.expect(t, acp.MethodSessionPrompt)
		_ = p.serverR.Close()
		cancelCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		outcome, err := p.s.Cancel(cancelCtx)
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("cancel over a closed write direction: %v", err)
		}
		// The write is known to have failed once the wire said sent, so
		// nothing here is known: the zero CancelOutcome.
		if outcome != (CancelOutcome{}) {
			t.Fatalf("CancelOutcome = %+v, want the zero value", outcome)
		}
		// The agent's own direction still works, so the turn can end.
		p.reply(t, req.ID, map[string]string{"stopReason": acp.StopEndTurn})
		if err := promptReturn(t, out, "the prompt never came back"); err != nil {
			t.Fatalf("prompt: %v", err)
		}
	})
}

// TestCancelWaitIsBoundedByItsContext: the wait for the wire is bounded by the
// caller's context and nothing else, and a Cancel that gave up writes nothing
// afterwards — not when the prompt finally goes out, not ever.
func TestCancelWaitIsBoundedByItsContext(t *testing.T) {
	p := newInProcess(t, acp.DialectCursor, nil)
	seam := holdBeforeWire(t)
	out := promptOn(p.s, "hi")
	seam.parked(t)

	cancelCtx, cancel := context.WithCancel(t.Context())
	cancelled := cancelOutcomeOn(cancelCtx, p.s)
	waitCancelling(t, p.s)
	cancel()
	r := cancelOutcomeReturn(t, cancelled, 5*time.Second)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancel whose context ended: %v", r.err)
	}
	// The wire's outcome was still unknown when the context ended: nothing is
	// known, and CancelOutcome says so.
	if r.outcome != (CancelOutcome{}) {
		t.Fatalf("CancelOutcome = %+v, want the zero value", r.outcome)
	}
	p.expectNothing(t, "a cancel that gave up")

	seam.letOne(t)
	req := p.expect(t, acp.MethodSessionPrompt)
	p.expectNothing(t, "a cancel that gave up, once its prompt was out")
	p.reply(t, req.ID, map[string]string{"stopReason": acp.StopEndTurn})
	if err := promptReturn(t, out, "the prompt never came back"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
}

// TestCancelWaitEndsWhenTheSessionCloses: a session closed under a waiting
// Cancel ends the wait. The prompt stays parked through the close — this one
// seam does not watch s.done, so the wire is still pending and the close is
// the only thing that can end the wait; t.Cleanup still lets it go.
func TestCancelWaitEndsWhenTheSessionCloses(t *testing.T) {
	p := newInProcess(t, acp.DialectCursor, nil)
	reached := make(chan struct{}, 1)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	testBeforeWire = func(*session) {
		reached <- struct{}{}
		<-gate
	}
	t.Cleanup(func() {
		release()
		testBeforeWire = nil
	})
	out := promptOn(p.s, "hi")
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt never reached the wire")
	}

	cancelled := cancelOutcomeBounded(t, p.s)
	waitCancelling(t, p.s)
	if err := p.s.Close(); err != nil {
		t.Fatal(err)
	}
	r := cancelOutcomeReturn(t, cancelled, 5*time.Second)
	if r.err != nil {
		t.Fatalf("cancel on a closed session: %v", r.err)
	}
	// s.done closing is "outcome as known": the wire was still pending, so
	// nothing was known, and the nil error must not be read as Settled.
	if r.outcome != (CancelOutcome{}) {
		t.Fatalf("CancelOutcome = %+v, want the zero value", r.outcome)
	}
	release()
	if err := promptReturn(t, out, "the prompt never came back"); err == nil {
		t.Fatal("a prompt on a closed session succeeded")
	}
}

// TestCancelAroundARefusedOrFailedPrompt is the three schedules a prompt that
// never becomes a turn of craze's own can meet a Cancel in, counting the
// notifications that actually reach the agent.
func TestCancelAroundARefusedOrFailedPrompt(t *testing.T) {
	// A cancel already waiting when the client refuses the prompt because the
	// agent is running a turn of its own: that turn is still running, so the
	// cancel goes out — once.
	t.Run("pending before a foreign-turn refusal", func(t *testing.T) {
		p := newInProcess(t, acp.DialectGrok, nil)
		p.startForeignTurn(t)
		seam := holdBeforeWire(t)
		out := promptOn(p.s, "mine")
		seam.parked(t)

		cancelled := cancelBounded(t, p.s)
		waitCancelling(t, p.s)
		p.expectNothing(t, "a cancel ahead of the refusal")
		seam.letOne(t)

		if err := promptReturn(t, out, "the refused prompt never came back"); !errors.Is(err, ErrForeignTurn) {
			t.Fatalf("prompt during a foreign turn: %v", err)
		}
		p.expectCancel(t)
		if err := cancelReturn(t, cancelled, 5*time.Second); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		p.expectNothing(t, "a second cancel")
	})

	// A cancel landing after the refusal was published but before the turn's
	// release. Nothing a refusal does blocks between the two, so no reply of
	// the test's can hold that window open, and a second seam point is the
	// only way to schedule a real Cancel into it. What such a Cancel reads is
	// fixed, though: it reads inPrompt and the wire in one locked section and
	// decides on nothing else. So the refused prompt runs for real, its wire —
	// settled by Prompt's own publisher — is kept, and the window is rebuilt
	// around that wire: inPrompt still true, the turn not yet released.
	t.Run("after a published refusal, the turn still open", func(t *testing.T) {
		p := newInProcess(t, acp.DialectGrok, nil)
		p.startForeignTurn(t)
		seam := holdBeforeWire(t)
		out := promptOn(p.s, "mine")
		wire := seam.parked(t)
		seam.letOne(t)
		if err := promptReturn(t, out, "the refused prompt never came back"); !errors.Is(err, ErrForeignTurn) {
			t.Fatalf("prompt during a foreign turn: %v", err)
		}
		if got, ok := settled(p.s, wire); !ok || got != wireRefused {
			t.Fatalf("the refused prompt's wire: settled=%v outcome=%v, want refused", ok, got)
		}

		endTurn := openTurnByHand(t, p.s, true, false, wire)
		cancelled := cancelBounded(t, p.s)
		p.expectCancel(t)
		p.expectNothing(t, "a second cancel")
		endTurn()
		if err := cancelReturn(t, cancelled, 5*time.Second); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	})

	// A cancel already waiting when the prompt's own write fails: nothing
	// reached the agent, so there is nothing to cancel and nothing is written.
	// The pipe itself still works — only the prompt's frame fails — so the
	// silence is the decision, not a dead transport.
	t.Run("pending before a failed write", func(t *testing.T) {
		p := newInProcess(t, acp.DialectCursor, failPrompts)
		seam := holdBeforeWire(t)
		out := promptOn(p.s, "mine")
		seam.parked(t)

		cancelled := cancelBounded(t, p.s)
		waitCancelling(t, p.s)
		seam.letOne(t)

		if err := promptReturn(t, out, "the failed prompt never came back"); !errors.Is(err, errPromptWrite) {
			t.Fatalf("prompt whose write failed: %v", err)
		}
		if err := cancelReturn(t, cancelled, 5*time.Second); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		p.expectNothing(t, "a cancel for a prompt that never reached the agent")
	})
}

// TestEveryTurnSettlesItsWire: by the time Prompt returns, its turn's wire has
// exactly one terminal outcome, and it is the true one — not the release's
// backstop. A refusal that fell through to the backstop would read failed, and
// a Cancel waiting on it would then leave the foreign turn running.
func TestEveryTurnSettlesItsWire(t *testing.T) {
	cases := []struct {
		name    string
		dialect acp.DialectID
		wrap    func(io.Writer) io.Writer
		before  func(t *testing.T, p *inProcess)
		drive   func(t *testing.T, p *inProcess)
		after   func(t *testing.T, p *inProcess)
		wantErr bool
		want    wireOutcome
	}{
		{
			name:    "written and answered",
			dialect: acp.DialectCursor,
			drive: func(t *testing.T, p *inProcess) {
				req := p.expect(t, acp.MethodSessionPrompt)
				p.reply(t, req.ID, map[string]string{"stopReason": acp.StopEndTurn})
			},
			want: wireSent,
		},
		{
			// The bytes went out, so the agent has the prompt: a failed
			// reply does not take that back, and a cancel still has a turn to
			// stop.
			name:    "written, then the reply failed",
			dialect: acp.DialectCursor,
			drive: func(t *testing.T, p *inProcess) {
				req := p.expect(t, acp.MethodSessionPrompt)
				p.replyErr(t, req.ID, "the turn failed")
			},
			wantErr: true,
			want:    wireSent,
		},
		{
			// prompt_complete settles a grok turn while its writer is still
			// held in the pipe: the clean return is what says sent.
			name:    "grok, settled before its writer reported",
			dialect: acp.DialectGrok,
			drive: func(t *testing.T, p *inProcess) {
				waitFor(t, "the client's turn to open", p.client.PromptInFlight)
				p.notify(t, acp.MethodGrokPromptComplete, `{"sessionId":"s1","stopReason":"end_turn"}`)
			},
			// Then the abandoned writer finishes, and its sent reports into
			// a wire that is already settled.
			after: func(t *testing.T, p *inProcess) { p.expect(t, acp.MethodSessionPrompt) },
			want:  wireSent,
		},
		{
			name:    "refused: a foreign turn",
			dialect: acp.DialectGrok,
			before:  func(t *testing.T, p *inProcess) { p.startForeignTurn(t) },
			wantErr: true,
			want:    wireRefused,
		},
		{
			name:    "the write failed",
			dialect: acp.DialectCursor,
			wrap:    failPrompts,
			wantErr: true,
			want:    wireFailed,
		},
		{
			name:    "the connection was already closed",
			dialect: acp.DialectCursor,
			before: func(t *testing.T, p *inProcess) {
				if err := p.client.Conn().Close(); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
			want:    wireFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newInProcess(t, tc.dialect, tc.wrap)
			if tc.before != nil {
				tc.before(t, p)
			}
			seam := holdBeforeWire(t)
			out := promptOn(p.s, "hi")
			wire := seam.parked(t)
			if _, ok := settled(p.s, wire); ok {
				t.Fatal("the wire was settled before the prompt reached the client")
			}
			seam.letOne(t)
			if tc.drive != nil {
				tc.drive(t, p)
			}
			err := promptReturn(t, out, "the prompt never came back")
			if (err != nil) != tc.wantErr {
				t.Fatalf("prompt: %v, want error %v", err, tc.wantErr)
			}
			got, ok := settled(p.s, wire)
			if !ok || got != tc.want {
				t.Fatalf("wire when Prompt returned: settled=%v outcome=%v, want %v", ok, got, tc.want)
			}
			p.s.mu.Lock()
			left := p.s.wire
			p.s.mu.Unlock()
			if left != nil {
				t.Fatal("the turn's release left its wire behind")
			}
			if tc.after != nil {
				tc.after(t, p)
			}
		})
	}
}

// TestALateSentHookLeavesTheNextTurnAlone is the hazard grok's abandoned writer
// brings: its sent can run after its turn is over, by which time the session
// may be holding the next turn's wire. The hook writes into its own turn's
// wire, which is settled already, so it changes nothing — in particular it
// cannot mark the next turn's prompt written while that prompt is still parked,
// which would let a cancel overtake it again.
//
// Turn A's context ends while its request is held unread in the pipe, so A is
// over — failed, nothing known to have reached the agent — with its writer
// still blocked. Turn B then parks at the seam, and only then is A's request
// read: A's writer completes and its sent runs, while B's wire is pending.
func TestALateSentHookLeavesTheNextTurnAlone(t *testing.T) {
	p := newInProcess(t, acp.DialectGrok, nil)
	seam := holdBeforeWire(t)

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	outA := make(chan error, 1)
	go func() {
		_, err := p.s.Prompt(ctxA, "first")
		outA <- err
	}()
	wireA := seam.parked(t)
	seam.letOne(t)
	waitFor(t, "turn A to reach the client", p.client.PromptInFlight)
	cancelA()
	if err := promptReturn(t, outA, "turn A never came back"); !errors.Is(err, context.Canceled) {
		t.Fatalf("turn A: %v", err)
	}
	if got, ok := settled(p.s, wireA); !ok || got != wireFailed {
		t.Fatalf("turn A's wire: settled=%v outcome=%v, want failed", ok, got)
	}

	outB := promptOn(p.s, "second")
	wireB := seam.parked(t)
	p.expect(t, acp.MethodSessionPrompt) // A's request: its writer completes and its sent runs
	select {
	case <-wireB.done:
		t.Fatalf("turn A's late sent settled turn B's wire as %v while B was parked", p.s.outcomeOf(wireB))
	case <-time.After(200 * time.Millisecond):
	}
	if got, _ := settled(p.s, wireA); got != wireFailed {
		t.Fatalf("turn A's late sent changed its settled wire to %v", got)
	}

	seam.letOne(t)
	reqB := p.expect(t, acp.MethodSessionPrompt)
	select {
	case <-wireB.done:
	case <-time.After(5 * time.Second):
		t.Fatal("turn B's written prompt never settled its wire")
	}
	if got := p.s.outcomeOf(wireB); got != wireSent {
		t.Fatalf("turn B's wire %v, want sent", got)
	}
	p.reply(t, reqB.ID, map[string]string{"stopReason": acp.StopEndTurn})
	if err := promptReturn(t, outB, "turn B never came back"); err != nil {
		t.Fatalf("turn B: %v", err)
	}
}
