package engine_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

// fakeClaudeCLI writes a `claude` that passes the preflight.
func fakeClaudeCLI(t *testing.T) string {
	t.Helper()
	claude := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *'auth status'*) echo '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"apiProvider\":\"firstParty\",\"subscriptionType\":\"max\"}' ;;\n" +
		"  *--version*) echo '2.1.326 (Claude Code)' ;;\nesac\n"
	if err := os.WriteFile(claude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return claude
}

// TestClaudeForeignQueuedPromptSentAfter (plan 052 AC17): a message typed
// while claude runs a turn of its own is queued, and sent only once that turn
// has ended — its started comes after the foreign turn's ending in the stream,
// and the agent saw no prompt meanwhile.
func TestClaudeForeignQueuedPromptSentAfter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(agent.SetClaudeForeignTimingForTest(agent.ForeignTiming{Grace: 2 * time.Second, Silence: 5 * time.Second}))
	calls := filepath.Join(t.TempDir(), "calls")
	p := agent.ClaudeProvider()
	sess := agent.New(agent.Options{
		Binary: fakeAgentBin(t), ExtraArgs: []string{"-script=claude-wake"},
		Workspace: t.TempDir(), Force: true, Provider: &p, Stderr: io.Discard,
		Env: append(os.Environ(), agent.ClaudeExecutableEnv+"="+fakeClaudeCLI(t),
			"CRAZE_FAKE_DUMP_CALLS="+calls, "CRAZE_FAKE_CLAUDE_WAKE_DELAY=200ms"),
	})
	e, err := engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	sub, err := e.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 4*watchdog)
	defer cancel()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(engine.Command{}, "BG run it", engine.SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	until := func(what string, ok func(engine.State) bool) {
		t.Helper()
		deadline := time.Now().Add(watchdog)
		for time.Now().Before(deadline) {
			if ok(e.State()) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s: %+v", what, e.State())
	}
	until("the agent's own turn", func(st engine.State) bool { return st.ForeignTurn })
	res, err := e.Submit(engine.Command{}, "next", engine.SubmitQueue, "")
	if err != nil || res.Queued == nil {
		t.Fatalf("a message during the foreign turn: %+v, %v; want it queued", res, err)
	}
	until("the queued message's turn", func(st engine.State) bool {
		return st.LastTurn != nil && st.LastTurn.TurnID == "turn-2" && len(st.Queue) == 0
	})
	var foreignEnd, nextStart = -1, -1
	for i := 0; ; i++ {
		var rec agent.Record
		select {
		case rec = <-sub.Records():
		case <-time.After(watchdog):
			t.Fatal("the stream ran dry before the second turn's start")
		}
		ev, err := rec.Event()
		if err != nil {
			continue
		}
		if ev.Type == agent.EventForeignTurn && ev.ForeignTurn != nil && !ev.ForeignTurn.Running {
			foreignEnd = i
		}
		if ev.Type == agent.EventTurn && ev.Turn != nil && ev.Turn.Phase == agent.TurnStarted && ev.Turn.ID == "turn-2" {
			nextStart = i
			break
		}
	}
	if foreignEnd < 0 || nextStart < foreignEnd {
		t.Fatalf("the foreign turn ended at %d and the queued turn started at %d; want the queue to wait for the ending", foreignEnd, nextStart)
	}
	raw, _ := os.ReadFile(calls)
	if n := countLines(string(raw), "session/prompt"); n != 2 {
		t.Fatalf("%d prompts on the wire; want 2", n)
	}
}

func countLines(s, line string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if l == line {
			n++
		}
	}
	return n
}
