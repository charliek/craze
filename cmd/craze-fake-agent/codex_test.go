package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncOut struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{}
	once sync.Once
}

func (w *syncOut) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(p)
	s := w.buf.String()
	w.mu.Unlock()
	if strings.Contains(s, `"turn/completed"`) {
		w.once.Do(func() { close(w.done) })
	}
	return n, err
}

func (w *syncOut) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestCodexServerEditsConcurrentCwdRaceFree(t *testing.T) {
	frames := []string{
		`{"id":1,"method":"initialize","params":{}}`,
		`{"id":2,"method":"thread/start","params":{"cwd":"/tmp/ws1"}}`,
		`{"id":3,"method":"turn/start","params":{"threadId":"thread-1","input":[{"type":"text","text":"edit"}]}}`,
		`{"id":4,"method":"thread/start","params":{"cwd":"/tmp/ws2"}}`,
	}

	out := &syncOut{done: make(chan struct{})}
	s := &codexServer{
		script:      "codex-edits",
		stdout:      bufio.NewWriter(out),
		turnCancel:  make(map[string]chan struct{}),
		pendingReqs: make(map[string]chan codexFrame),
		lastModel:   "gpt-6.2-sol",
		lastEffort:  "medium",
		lastMode:    "default",
		lastSandbox: "workspaceWrite",
		lastApprove: "never",
	}
	for _, raw := range frames {
		var msg codexFrame
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatal(err)
		}
		s.handleIncoming(msg)
	}
	select {
	case <-out.done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for turn/completed")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 frames, got %d:\n%s", len(lines), out.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("invalid json frame: %v", err)
	}
}
