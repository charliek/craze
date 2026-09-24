package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

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
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
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
