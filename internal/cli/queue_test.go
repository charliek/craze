package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestPromptFollowUpsAreTheQueue: every follow-up is queued before the first
// turn and sent one per settled turn, in order, with a line for each change.
func TestPromptFollowUpsAreTheQueue(t *testing.T) {
	evs := runPromptJSONArgs(t, "echo",
		[]string{"--follow-up", "Reply PINEAPPLE", "--follow-up", "Reply MANGO"}, "one")

	var dones int
	for _, ev := range evs {
		if ev.m["type"] == "done" {
			dones++
		}
	}
	if dones != 3 {
		t.Fatalf("want three done, got %d", dones)
	}

	var queued, sent []map[string]any
	for _, ev := range evs {
		if ev.m["type"] != "queue" {
			continue
		}
		switch ev.m["event"] {
		case "queued":
			queued = append(queued, ev.m)
		case "sent":
			sent = append(sent, ev.m)
		default:
			t.Fatalf("unexpected queue event %s", ev.raw)
		}
	}
	if len(queued) != 2 || len(sent) != 2 {
		t.Fatalf("queued %d sent %d", len(queued), len(sent))
	}
	if queued[0]["text"] != "Reply PINEAPPLE" || queued[0]["position"] != float64(0) {
		t.Fatalf("first queued %v", queued[0])
	}
	if queued[1]["text"] != "Reply MANGO" || queued[1]["position"] != float64(1) {
		t.Fatalf("second queued %v", queued[1])
	}
	if sent[0]["id"] != queued[0]["id"] || sent[1]["id"] != queued[1]["id"] {
		t.Fatalf("sent out of order: %v %v", sent[0], sent[1])
	}
	// Each row is the head when it goes.
	for i, s := range sent {
		if s["position"] != float64(0) {
			t.Fatalf("sent %d position %v", i, s["position"])
		}
	}
	// The turns ran in the queued order.
	var replies []string
	for _, ev := range evs {
		if ev.m["type"] == "text" {
			replies = append(replies, fmtString(ev.m["text"]))
		}
	}
	joined := strings.Join(replies, "")
	if strings.Index(joined, "PINEAPPLE") > strings.Index(joined, "MANGO") {
		t.Fatalf("replies out of order: %q", joined)
	}
}

// TestPromptPlainModeWritesNoQueueLines: the queue is JSON-only chrome.
func TestPromptPlainModeWritesNoQueueLines(t *testing.T) {
	isolateProviderConfig(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"prompt", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir(),
		"--follow-up", "second", "one",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt: %v\nstderr: %s", err, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "queue") || strings.Contains(out, "\"event\"") {
		t.Fatalf("plain stdout carries queue chrome: %q", out)
	}
	if !strings.Contains(out, "echo: one") || !strings.Contains(out, "second") {
		t.Fatalf("both turns must still run: %q", out)
	}
}

// TestPromptCancelledFirstTurnStopsTheChain: the stop reason still ends the
// run with exit 1 and no queued turn behind it.
func TestPromptCancelledFirstTurnStopsTheChain(t *testing.T) {
	isolateProviderConfig(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "hang")
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"prompt", "--json", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir(),
		"--follow-up", "never runs", "go",
	})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	// The hang script waits for session/cancel; the signal path is what the
	// context cancel stands in for here.
	time.Sleep(300 * time.Millisecond)
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("prompt hung")
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("err %v", err)
	}
	evs := parseJSONLines(t, stdout.String())
	var removed, sent, dones int
	for _, ev := range evs {
		switch {
		case ev.m["type"] == "queue" && ev.m["event"] == "removed":
			removed++
		case ev.m["type"] == "queue" && ev.m["event"] == "sent":
			sent++
		case ev.m["type"] == "done":
			dones++
		}
	}
	if removed != 1 {
		t.Fatalf("the signal must clear the queue: %d removed\n%s", removed, stdout.String())
	}
	if sent != 0 {
		t.Fatalf("a cleared row must never be sent: %d", sent)
	}
	if dones != 1 {
		t.Fatalf("%d done: the chain must stop", dones)
	}
}

// TestPromptForeignTurnIsWaitedOut is the grok fallback through the CLI. The
// fake strands an interjection of its own as the first turn ends — craze never
// interjects headlessly, but grok will mint a fallback for an interjection
// craze did not send — and the queued follow-up must run after that turn, not
// into it.
func TestPromptForeignTurnIsWaitedOut(t *testing.T) {
	evs := runPromptJSONArgs(t, "grok-long-turn-fallback",
		[]string{"--provider", "grok", "--follow-up", "Reply PINEAPPLE"},
		"do the steps STRAND-INTERJECTION")
	var started, ended, dones int
	var order []string
	for _, ev := range evs {
		switch {
		case ev.m["type"] == "foreign_turn" && ev.m["event"] == "started":
			started++
			order = append(order, "foreign-start")
		case ev.m["type"] == "foreign_turn" && ev.m["event"] == "ended":
			ended++
			order = append(order, "foreign-end")
		case ev.m["type"] == "done":
			dones++
			order = append(order, "done")
		case ev.m["type"] == "queue" && ev.m["event"] == "sent":
			order = append(order, "sent")
		}
	}
	if dones != 2 {
		t.Fatalf("want one done per craze prompt, got %d\n%v", dones, order)
	}
	if started != 1 || ended != 1 {
		t.Fatalf("foreign turn: %d started, %d ended\n%v", started, ended, order)
	}
	joined := strings.Join(order, ",")
	if !strings.Contains(joined, "foreign-end,sent") {
		t.Fatalf("the queued message must be sent after the foreign turn: %v", order)
	}
	if strings.Contains(joined, "foreign-start,sent") {
		t.Fatalf("a message left the queue while the agent held the session: %v", order)
	}
	// The interjection grok could not merge is still shown as a user block.
	pick(t, evs, "user", func(m map[string]any) bool { return m["interjection"] == true })
}

// TestQueueFullRefusesTheFollowUp: the bound is the session's, and the CLI
// reports it rather than silently dropping a message.
func TestQueueFullRefusesTheFollowUp(t *testing.T) {
	isolateProviderConfig(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	args := []string{"prompt", "--json", "--agent-bin", fakeAgentPath(t), "--workspace", t.TempDir()}
	for i := 0; i < 33; i++ {
		args = append(args, "--follow-up", "x")
	}
	args = append(args, "one")
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "queue is full") {
		t.Fatalf("err %v", err)
	}
}

// TestPromptAnswersPermissionOutsideATurn: a permission request that arrives
// while nothing is reading a turn's own stream — between turns, or during a
// turn the agent started on its own — is still the agent waiting for an
// answer, so every reader of the stream answers it.
func TestPromptAnswersPermissionDuringAForeignTurn(t *testing.T) {
	isolateProviderConfig(t)
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("GROK_CODE_XAI_API_KEY", "")
	t.Setenv("CRAZE_FAKE_SCRIPT", "grok-long-turn-fallback")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"prompt", "--json", "--provider", "grok", "--agent-bin", fakeAgentPath(t),
		"--workspace", t.TempDir(), "--follow-up", "Reply PINEAPPLE",
		"do the steps STRAND-INTERJECTION",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prompt: %v\nstderr: %s", err, stderr.String())
	}
	evs := parseJSONLines(t, stdout.String())
	// The queued follow-up ran after the foreign turn, which is only true if
	// nextQueued told "blocked" apart from "empty".
	var sentAfterEnd bool
	var ended bool
	for _, ev := range evs {
		if ev.m["type"] == "foreign_turn" && ev.m["event"] == "ended" {
			ended = true
		}
		if ended && ev.m["type"] == "queue" && ev.m["event"] == "sent" {
			sentAfterEnd = true
		}
	}
	if !sentAfterEnd {
		t.Fatalf("the follow-up must run after the foreign turn:\n%s", stdout.String())
	}
}

// TestPermissionRejectedBetweenTurnsStillFailsTheRun: the queue gave the
// event stream three readers, and a permission refused by any of them is
// still a refusal. A run that said no and then exited 0 would tell a script
// the opposite of what happened.
func TestPermissionRejectedBetweenTurnsStillFailsTheRun(t *testing.T) {
	isolateProviderConfig(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "permission")
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"prompt", "--json", "--no-force", "--agent-bin", fakeAgentPath(t),
		"--workspace", t.TempDir(),
		"--permission-decision", "reject-once",
		"--follow-up", "second", "go",
	})
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("a rejected permission must fail the run: %v", err)
	}
}
