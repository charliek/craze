package engine_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/sessions"
)

// rowRecorder is an engine.Index that keeps every row written.
type rowRecorder struct {
	mu   sync.Mutex
	rows []sessions.Row
}

func (r *rowRecorder) Upsert(row sessions.Row) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, row)
	return nil
}

func (r *rowRecorder) last() (sessions.Row, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.rows) == 0 {
		return sessions.Row{}, false
	}
	return r.rows[len(r.rows)-1], true
}

// TestClaudeRowRecordsTheModelAfterAStartRetry (plan 052 review F2): a claude
// session whose start mode had to be set again still records the model
// session/new answered with (opus) on its index row.
func TestClaudeRowRecordsTheModelAfterAStartRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *'auth status'*) echo '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"apiProvider\":\"firstParty\",\"subscriptionType\":\"max\"}' ;;\n" +
		"  *--version*) echo '2.1.326 (Claude Code)' ;;\nesac\n"
	if err := os.WriteFile(claude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*watchdog)
	defer cancel()
	p := agent.ClaudeProvider()
	sess := agent.New(agent.Options{
		Binary: fakeAgentBin(t), ExtraArgs: []string{"-script=claude-basic"},
		Workspace: t.TempDir(), Force: true, Provider: &p, Stderr: io.Discard,
		Env: append(os.Environ(), agent.ClaudeExecutableEnv+"="+claude, "CRAZE_FAKE_CLAUDE_SKIP_INITIAL=mode"),
	})
	idx := &rowRecorder{}
	e, err := engine.New(sess, engine.Options{Index: engine.IndexOptions{Store: idx, CWD: "/w", Provider: "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(engine.Command{}, "hello", engine.SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * watchdog)
	for time.Now().Before(deadline) {
		if row, ok := idx.last(); ok {
			if row.Model != "opus" {
				t.Fatalf("the row's model %q, want opus: %+v", row.Model, row)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no row was written")
}
