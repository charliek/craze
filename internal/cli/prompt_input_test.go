package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// The prompt's source read under the command's deadline and signals, the
// prompt file removed only once read (C3 review r1, 6 and 7), and the gate's
// rule for a refusal (5).

// goCrazeIn is goCraze with stdin the command reads from.
func goCrazeIn(stdin io.Reader, argv ...string) *crazeRun {
	r := &crazeRun{done: make(chan struct{})}
	go func() {
		r.stdout, r.stderr, r.code = executeIn(argv, stdin)
		close(r.done)
	}()
	return r
}

// TestThePromptIsReadUnderTheCommandsDeadlineAndSignals: a prompt read from
// a source that never ends — stdin a pipe its writer holds open, a FIFO
// nobody writes — is given up at --timeout's end, and at a signal, exit 1
// naming the source (one JSON error object with --json), with nothing sent
// and nothing spawned.
func TestThePromptIsReadUnderTheCommandsDeadlineAndSignals(t *testing.T) {
	r := newWaitRig(t, 0, answerTurn(textEv("never")))
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })
	stdout, stderr, code := goCrazeIn(pr, "send", waitRigShort, "--timeout", "200ms", "--json").wait(t)
	if code != 1 || stderr != "craze send: timed out reading the prompt from stdin\n" ||
		stdout != `{"error":{"message":"timed out reading the prompt from stdin","kind":"timeout_before_prompt","retryable":true}}`+"\n" {
		t.Fatalf("a pipe held open: exit %d, %q %q", code, stdout, stderr)
	}

	kids := spawnAsChild(t, nil)
	fifo := filepath.Join(t.TempDir(), "prompt")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The read's goroutine, still opening the FIFO, is let go.
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
	sigs := withWaitSignals(t)
	reading := make(chan struct{}, 1)
	prev := promptReading
	promptReading = func() { reading <- struct{}{} }
	t.Cleanup(func() { promptReading = prev })
	run := goCraze("new", "--wait", "--provider", "cursor", "-C", t.TempDir(), "--prompt-file", fifo)
	select {
	case <-reading:
	case <-time.After(stubWatchdog):
		t.Fatal("craze new --wait never began to read its prompt")
	}
	sigs <- syscall.SIGINT
	stdout, stderr, code = run.wait(t)
	if code != 1 || stdout != "" || stderr != "craze new: interrupted while reading the prompt from "+fifo+"\n" {
		t.Fatalf("a FIFO nobody writes: exit %d, %q %q", code, stdout, stderr)
	}
	if kids.count() != 0 || len(r.s.sent()) != 0 {
		t.Fatalf("spawned %d hosts, sent %q", kids.count(), r.s.sent())
	}
}

// TestRemovePromptFileOnlyOnceRead: --remove-prompt-file deletes a file it
// read — a prompt then refused as empty included (P14: "after it was read")
// — and never one it could not read: unreadable (mode 000), or a directory.
func TestRemovePromptFileOnlyOnceRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file of mode 000")
	}
	newWaitRig(t, 0, answerTurn(textEv("never")))
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked.md")
	if err := os.WriteFile(locked, []byte("a task"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, want string
		kept       bool
	}{
		{locked, "the prompt cannot be read", true},
		{sub, "the prompt cannot be read", true},
		{empty, "the prompt is empty", false},
	} {
		stdout, stderr, code := executeErr([]string{"send", waitRigShort, "--prompt-file", tc.path, "--remove-prompt-file"})
		if code != 2 || stdout != "" || !containsLine(stderr, tc.want) {
			t.Fatalf("%s: exit %d, %q %q", tc.path, code, stdout, stderr)
		}
		if _, err := os.Lstat(tc.path); (err == nil) != tc.kept {
			t.Fatalf("%s: kept %v, want %v", tc.path, err == nil, tc.kept)
		}
	}
}

// containsLine says s is one line holding want.
func containsLine(s, want string) bool {
	return strings.Count(s, "\n") == 1 && strings.HasSuffix(s, "\n") && strings.Contains(s, want)
}

// TestTheGateRefusesOnlyTheHostsOwnRefusal (C3 review r1, 5): a prompt is
// refused — never ran, so craze new --wait may stop the session it started —
// only on the host's own error reply to it, or a command the client knows it
// never wrote. An answer lost (outcome unknown), a success reply that cannot
// be read, and any other failure leave its fate unknown: it may have run.
func TestTheGateRefusesOnlyTheHostsOwnRefusal(t *testing.T) {
	hostRefusal := &remote.Error{RPC: -32000, Code: protocol.CodeNotAccepting, Reason: protocol.ReasonQueueFull, Message: "the queue is full"}
	for _, tc := range []struct {
		name string
		err  error
		want promptFate
	}{
		{"taken", nil, promptTaken},
		{"the host's refusal", hostRefusal, promptRefused},
		{"the host's refusal, wrapped", &submitError{msg: "refused", err: hostRefusal}, promptRefused},
		{"never written", remote.ErrNotRun, promptRefused},
		{"outcome unknown", &remote.OutcomeUnknownError{Method: protocol.MethodSessionPrompt, Reason: protocol.ReasonResumeLost}, promptPending},
		{"a reply that cannot be read", &json.SyntaxError{Offset: 3}, promptPending},
		{"anything else", errors.New("connection reset"), promptPending},
	} {
		g := &submitGate{submit: func(context.Context, uint64) (waitTarget, error) {
			return waitTarget{kind: targetTurn, id: "turn-1"}, tc.err
		}}
		_, _ = g.run(context.Background(), 1)
		if fate, target := g.close(); fate != tc.want || (fate == promptTaken) != (target.id == "turn-1") {
			t.Errorf("%s: %v, %+v; want %v", tc.name, fate, target, tc.want)
		}
	}
	unsent := &submitGate{submit: func(context.Context, uint64) (waitTarget, error) { return waitTarget{}, nil }}
	if fate, _ := unsent.close(); fate != promptUnsent {
		t.Errorf("never sent: %v", fate)
	}
	if _, err := unsent.run(context.Background(), 1); !errors.Is(err, errPromptWithheld) {
		t.Errorf("a closed gate sent the prompt: %v", err)
	}
}
