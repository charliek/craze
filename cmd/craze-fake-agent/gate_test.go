package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// openGateWriter opens a FIFO's write end once its reader has it open. The
// open is non-blocking, retrying ENXIO (no reader yet; the same errno on Linux
// and macOS) until a deadline that fails the test, so a reader that never
// opens leaves no goroutine blocked in an open that nothing can interrupt.
func openGateWriter(t *testing.T, fifo string) *os.File {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			return w
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatal(err)
		}
		if !time.Now().Before(deadline) {
			t.Fatal("the gate's reader never opened the FIFO")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAwaitGateHoldsUntilItsByte is CRAZE_FAKE_GATE's barrier: awaitGate
// returns at once with no path, and with one it returns only once it has read
// a byte. Opening the FIFO for writing completes only when awaitGate has
// opened it for reading, and with the writer open and nothing written its read
// cannot have returned — so the "still held" check is a fact, not a timing.
func TestAwaitGateHoldsUntilItsByte(t *testing.T) {
	awaitGate("") // unset: returns

	fifo := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		awaitGate(fifo)
		close(done)
	}()
	w := openGateWriter(t, fifo)
	defer w.Close()
	select {
	case <-done:
		t.Fatal("the gate opened before its byte was written")
	default:
	}
	if _, err := w.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the gate did not open on its byte")
	}
}

// TestASetGateHoldsAWriterWhileItWaits is CRAZE_FAKE_SET_GATE's wait (plan 035
// C3): while a set waits, its FIFO has a write end of the wait's own, so a
// writer that closes without writing — the test's writer for the set before
// — leaves it empty, not at an end of file: that close is no release, and the
// next writer's byte cannot be dropped with the pipe. A reader that does not
// wait tells the two apart (EAGAIN, or a read of 0), and the next byte
// releases the set. Opening the FIFO for writing completes only once the wait
// has it open, so the check is a fact, not a timing.
func TestASetGateHoldsAWriterWhileItWaits(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "set-gate")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		awaitSetGate(fifo)
		close(done)
	}()
	before := openGateWriter(t, fifo)
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(fifo, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	n, err := syscall.Read(fd, b[:])
	_ = syscall.Close(fd)
	if !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("a reader of the waiting set's FIFO: %d, %v; want EAGAIN (empty, with a writer), not an end of file (0)", n, err)
	}
	w := openGateWriter(t, fifo)
	defer w.Close()
	if _, err := w.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the set was not released by its byte")
	}
}

// sigintHoldSecondPrompt runs the sigint-hold script over a pipe, answers turn
// 1 (echo: end_turn with no wait), and returns with turn 2's prompt on the wire.
func sigintHoldSecondPrompt(t *testing.T) (*pmWire, json.RawMessage) {
	t.Helper()
	w := dialPermodel(t, "sigint-hold")
	w.ok(acp.MethodInitialize, map[string]any{"protocolVersion": acp.ProtocolVersion})
	w.ok(acp.MethodSessionNew, map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
	resp, _ := w.call(acp.MethodSessionPrompt, map[string]any{
		"sessionId": fakeSessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "one"}},
	})
	if got := stopReason(t, resp); got != acp.StopEndTurn {
		t.Fatalf("turn 1 stopReason %q, want %q", got, acp.StopEndTurn)
	}
	id := w.send(acp.MethodSessionPrompt, map[string]any{
		"sessionId": fakeSessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "two"}},
	})
	return w, id
}

func stopReason(t *testing.T, m *acp.Message) string {
	t.Helper()
	if m.Error != nil {
		t.Fatalf("refused: %+v", m.Error)
	}
	var r struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(m.Result, &r); err != nil {
		t.Fatal(err)
	}
	return r.StopReason
}

// answerTo reads frames until the answer to id. With quiet > 0 it instead
// requires that no answer arrives within quiet, and returns nil.
func (w *pmWire) answerTo(id json.RawMessage, quiet time.Duration) *acp.Message {
	w.t.Helper()
	var timeout <-chan time.Time
	if quiet > 0 {
		timeout = time.After(quiet)
	}
	for {
		select {
		case msg, ok := <-w.frames:
			if !ok {
				w.t.Fatal("the fake closed the wire")
			}
			if msg.IsResponse() && string(msg.ID) == string(id) {
				if quiet > 0 {
					w.t.Fatalf("the held prompt was answered early: %s", msg.Result)
				}
				return msg
			}
		case <-timeout:
			return nil
		case <-time.After(pmWait):
			w.t.Fatalf("no answer within %v", pmWait)
		}
	}
}

// TestSigintHoldTurnTwoEndsOnCancel: turn 2 is still unanswered after a quiet
// stretch, and session/cancel answers it cancelled with no gate involved, so a
// signal landing anywhere in craze's between-turns race still finds it in
// flight.
func TestSigintHoldTurnTwoEndsOnCancel(t *testing.T) {
	t.Setenv("CRAZE_FAKE_GATE", "")
	w, id := sigintHoldSecondPrompt(t)
	w.answerTo(id, 200*time.Millisecond)
	raw, _ := json.Marshal(map[string]any{"sessionId": fakeSessionID})
	if err := w.enc.WriteMessage(&acp.Message{Method: acp.MethodSessionCancel, Params: raw}); err != nil {
		t.Fatal(err)
	}
	if got := stopReason(t, w.answerTo(id, 0)); got != acp.StopCancelled {
		t.Fatalf("turn 2 stopReason %q, want %q", got, acp.StopCancelled)
	}
}

// TestSigintHoldTurnTwoEndsOnTheGate: with CRAZE_FAKE_GATE set, the gate's byte
// releases the held turn as an ordinary echo.
func TestSigintHoldTurnTwoEndsOnTheGate(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_GATE", fifo)
	w, id := sigintHoldSecondPrompt(t)
	f := openGateWriter(t, fifo)
	defer f.Close()
	w.answerTo(id, 200*time.Millisecond)
	if _, err := f.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if got := stopReason(t, w.answerTo(id, 0)); got != acp.StopEndTurn {
		t.Fatalf("turn 2 stopReason %q, want %q", got, acp.StopEndTurn)
	}
}

// TestSigintHoldCancelledTurnDoesNotTakeTheNextGateByte: turn 2 is held and
// cancelled, turn 3 is held, and one gate byte releases turn 3 as echo. One
// shared reader owns the FIFO, so the cancelled turn has no reader left to take
// the byte meant for the turn after it. The gate writer stays open throughout:
// opening it completes once the first held turn's reader has the FIFO open,
// and every wait is bounded by pmWait.
func TestSigintHoldCancelledTurnDoesNotTakeTheNextGateByte(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_GATE", fifo)
	w, id2 := sigintHoldSecondPrompt(t)
	// The writer opens only once the shared reader has the FIFO open, under
	// openGateWriter's own bound: a reader that never starts fails here, not at
	// the package timeout, and leaves no goroutine blocked in the open.
	f := openGateWriter(t, fifo)
	defer f.Close()
	raw, _ := json.Marshal(map[string]any{"sessionId": fakeSessionID})
	if err := w.enc.WriteMessage(&acp.Message{Method: acp.MethodSessionCancel, Params: raw}); err != nil {
		t.Fatal(err)
	}
	if got := stopReason(t, w.answerTo(id2, 0)); got != acp.StopCancelled {
		t.Fatalf("turn 2 stopReason %q, want %q", got, acp.StopCancelled)
	}
	id3 := w.send(acp.MethodSessionPrompt, map[string]any{
		"sessionId": fakeSessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "three"}},
	})
	w.answerTo(id3, 200*time.Millisecond)
	if _, err := f.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if got := stopReason(t, w.answerTo(id3, 0)); got != acp.StopEndTurn {
		t.Fatalf("turn 3 stopReason %q, want %q", got, acp.StopEndTurn)
	}
}

// TestTheSharedGateLosesNoByte is the shared reader's protocol (plan 033
// C3r): two bytes in the pipe at once — two releases back to back, here one
// write of two — release two waits. A reader that opened the FIFO for each
// byte and closed it after reading one dropped the second with the pipe:
// back-to-back releases of a long turn's two steps lost the second whenever
// the second writer opened before the first read's close, and the turn was
// held for ever.
func TestTheSharedGateLosesNoByte(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "gate")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRAZE_FAKE_GATE", fifo)
	s := &server{}
	gate := s.gate()
	w := openGateWriter(t, fifo)
	defer w.Close() // a failed write's t.Fatal still closes it
	if _, err := w.Write([]byte{1, 1}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		select {
		case <-gate:
		case <-time.After(10 * time.Second):
			t.Fatalf("release %d of 2 never came", i+1)
		}
	}
}
