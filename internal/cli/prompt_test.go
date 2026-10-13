package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/agent"
)

func TestRootRefusesNonTTY(t *testing.T) {
	var stdout bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(ee.msg, "refusing to start TUI on a non-tty") {
		t.Fatalf("msg %q", ee.msg)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout %q", stdout.String())
	}
}

func TestPromptUsage(t *testing.T) {
	t.Run("no text empty stdin", func(t *testing.T) {
		cmd := NewRootCmd()
		cmd.SetIn(&bytes.Buffer{})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"prompt"})
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("ask and plan", func(t *testing.T) {
		cmd := NewRootCmd()
		cmd.SetIn(&bytes.Buffer{})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"prompt", "--ask", "--plan", "hi"})
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("got %v", err)
		}
		if !strings.Contains(ee.msg, "mutually exclusive") {
			t.Fatalf("msg %q", ee.msg)
		}
	})

	t.Run("bad permission decision", func(t *testing.T) {
		cmd := NewRootCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"prompt", "--permission-decision", "maybe", "hi"})
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("workspace missing", func(t *testing.T) {
		cmd := NewRootCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"prompt", "--workspace", t.TempDir() + "/no-such-dir", "hi"})
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("got %v", err)
		}
	})
}

func TestDecisionKind(t *testing.T) {
	kind, err := decisionKind("allow-once")
	if err != nil || kind != "allow_once" {
		t.Fatalf("%q %v", kind, err)
	}
	kind, err = decisionKind("reject_once")
	if err != nil || kind != "reject_once" {
		t.Fatalf("%q %v", kind, err)
	}
}

func asJSONEvent(t *testing.T, v any) jsonEvent {
	t.Helper()
	j, ok := v.(jsonEvent)
	if !ok {
		t.Fatalf("want jsonEvent, got %T", v)
	}
	return j
}

func TestEventJSON(t *testing.T) {
	v, ok := eventJSON(agent.Event{Type: agent.EventThought, Text: "thinking"})
	j := asJSONEvent(t, v)
	if !ok || j.Type != "thought" || j.Text != "thinking" {
		t.Fatalf("thought json %+v %v", j, ok)
	}
	v, ok = eventJSON(agent.Event{Type: agent.EventText, Text: "hi"})
	j = asJSONEvent(t, v)
	if !ok || j.Type != "text" || j.Text != "hi" {
		t.Fatalf("%+v %v", j, ok)
	}
	v, ok = eventJSON(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{
		ID: "call-1", Name: "Shell", Status: "pending", Kind: "execute", Title: "Shell", RawInput: "echo hi",
	}})
	j = asJSONEvent(t, v)
	if !ok || j.Kind != "execute" || j.Title != "Shell" || j.Name != "Shell" {
		t.Fatalf("tool json %+v %v", j, ok)
	}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["rawInput"]; ok {
		t.Fatalf("must not dump rawInput: %s", b)
	}
	v, _ = eventJSON(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "c1", Name: "n", Status: "s"}})
	j = asJSONEvent(t, v)
	b, err = json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	m = map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["kind"]; ok {
		t.Fatalf("empty kind should omit: %s", b)
	}
	if _, ok := m["title"]; ok {
		t.Fatalf("empty title should omit: %s", b)
	}

	v, ok = eventJSON(agent.Event{Type: agent.EventUser, Agent: "sub-1", Text: "prompt"})
	j = asJSONEvent(t, v)
	if !ok || j.Type != "user" || j.Agent != "sub-1" || j.Text != "prompt" {
		t.Fatalf("user json %+v %v", j, ok)
	}
	v, ok = eventJSON(agent.Event{
		Type:           agent.EventSubagent,
		SubagentChange: agent.SubagentChangeSpawned,
		Subagent:       &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning, Transcript: true},
	})
	if !ok {
		t.Fatal("eventJSON must encode EventSubagent")
	}
	b, err = json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"type":"subagent"`) || !strings.Contains(string(b), `"event":"spawned"`) {
		t.Fatalf("subagent json %s", b)
	}
}

// TestEventJSONDropsEventTurn: EventTurn falls to eventJSON's default like any
// other kind it does not know how to render, so a new engine-authored kind
// never gains a --json line without someone deciding it should (plan 021
// §3.9's "no line kind").
func TestEventJSONDropsEventTurn(t *testing.T) {
	_, ok := eventJSON(agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted}})
	if ok {
		t.Fatal("eventJSON must not render EventTurn")
	}
}

// TestEventJSONDropsAStateDelta: a state delta rides on an EventMeta with no
// Text, which eventJSON already drops — only a title has anything a headless
// caller could print — so a craze-initiated change gains no --json line either
// (plan 021 §3.9's "no line kind").
func TestEventJSONDropsAStateDelta(t *testing.T) {
	_, ok := eventJSON(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{
		SendNow: &agent.SendNowState{}, Reason: agent.SendNowWithdrawn,
	}})
	if ok {
		t.Fatal("eventJSON must not render a state delta")
	}
}

// TestEventJSONDropsAUsageDelta (plan 028 §3.13, §3.14): a native session's
// usage section rides on an EventMeta with no Text too, after every step, so
// it prints no --json line — it still takes a seq, which is why a native
// session's --json seq values have gaps — and an ACP session's output is
// exactly what it was (A36).
func TestEventJSONDropsAUsageDelta(t *testing.T) {
	_, ok := eventJSON(agent.Event{Type: agent.EventMeta, Seq: 7, State: &agent.StateDelta{
		Usage: &agent.UsageState{ContextTokens: 1_000, Session: agent.Spend{Input: 10, Output: 5, Unpriced: true}},
	}})
	if ok {
		t.Fatal("eventJSON must not render a usage delta")
	}
}

func TestDrainSkippedWhenNothingSpawned(t *testing.T) {
	evs := make(chan agent.Event)
	start := time.Now()
	if err := drainSubagentEvents(evs, func() agent.Snapshot { return agent.Snapshot{} }, func(agent.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("drain added latency %s", time.Since(start))
	}
}

func TestDrainWritesLateFinished(t *testing.T) {
	evs := make(chan agent.Event, 4)
	var mu sync.Mutex
	info := agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning}
	snap := agent.Snapshot{Subagents: []agent.SubagentInfo{info}}
	go func() {
		time.Sleep(400 * time.Millisecond)
		fin := info
		fin.Status = agent.SubagentCancelled
		mu.Lock()
		snap.Subagents[0] = fin
		mu.Unlock()
		evs <- agent.Event{Type: agent.EventSubagent, SubagentChange: agent.SubagentChangeFinished, Subagent: &fin}
	}()
	var got []agent.Event
	start := time.Now()
	if err := drainSubagentEvents(evs, func() agent.Snapshot {
		mu.Lock()
		defer mu.Unlock()
		out := snap
		out.Subagents = append([]agent.SubagentInfo(nil), snap.Subagents...)
		return out
	}, func(ev agent.Event) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 350*time.Millisecond {
		t.Fatalf("drain returned too fast to be state-aware: %s", time.Since(start))
	}
	if len(got) != 1 || got[0].SubagentChange != agent.SubagentChangeFinished {
		t.Fatalf("got %+v", got)
	}
}

// TestDrainPropagatesWriteErrors pins the contract run()'s deferred drain
// relies on: a write that fails during the drain is an error, so a headless
// run can never exit 0 with incomplete JSON.
func TestDrainPropagatesWriteErrors(t *testing.T) {
	evs := make(chan agent.Event, 1)
	snap := agent.Snapshot{Subagents: []agent.SubagentInfo{{ID: "sub-1", Status: agent.SubagentCompleted}}}
	evs <- agent.Event{Type: agent.EventSubagent, SubagentChange: agent.SubagentChangeFinished, Subagent: &snap.Subagents[0]}
	close(evs)
	want := errors.New("pipe broke")
	err := drainSubagentEvents(evs, func() agent.Snapshot { return snap }, func(agent.Event) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("drain error %v, want %v", err, want)
	}
}

// TestSweepEventsTakesBufferedWithoutBlocking pins the helper the drain's
// deadline branches use: everything already buffered is written, and an empty
// channel returns at once rather than waiting for the next event.
func TestSweepEventsTakesBufferedWithoutBlocking(t *testing.T) {
	evs := make(chan agent.Event, 4)
	for i := 0; i < 3; i++ {
		evs <- agent.Event{Type: agent.EventText, Text: "x"}
	}
	var got int
	start := time.Now()
	if err := sweepEvents(evs, func(agent.Event) error {
		got++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("wrote %d of 3 buffered events", got)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("sweep blocked for %s", time.Since(start))
	}
	want := errors.New("pipe broke")
	evs <- agent.Event{Type: agent.EventText, Text: "x"}
	if err := sweepEvents(evs, func(agent.Event) error { return want }); !errors.Is(err, want) {
		t.Fatalf("sweep error %v, want %v", err, want)
	}
}

// TestDrainKeepsBufferedEventsWhenTheTimerFires pins the race the sweep
// closes: when a deadline and a buffered event are both ready, select picks
// between them at random, so returning on the timer alone would drop JSON the
// session had already produced. The first write outlasts the quiet window, so
// by the next iteration both are ready.
func TestDrainKeepsBufferedEventsWhenTheTimerFires(t *testing.T) {
	const n = 8
	evs := make(chan agent.Event, n)
	for i := 0; i < n; i++ {
		evs <- agent.Event{Type: agent.EventText, Text: "x"}
	}
	snap := agent.Snapshot{Subagents: []agent.SubagentInfo{{ID: "sub-1", Status: agent.SubagentCompleted}}}
	var got int
	err := drainSubagentEvents(evs, func() agent.Snapshot { return snap }, func(agent.Event) error {
		if got == 0 {
			time.Sleep(subagentQuiet + 50*time.Millisecond)
		}
		got++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Fatalf("the drain wrote %d of %d buffered events", got, n)
	}
}

func TestPickPermissionKeepsUnusedDecisions(t *testing.T) {
	opts := []agent.PermissionOption{
		{OptionID: "opt-reject", Kind: "reject_once"},
	}
	id, rejected, rest, err := pickPermission(opts, []string{"allow-once"})
	if err != nil || !rejected || id != "opt-reject" {
		t.Fatalf("id=%q rejected=%v err=%v", id, rejected, err)
	}
	if len(rest) != 1 || rest[0] != "allow-once" {
		t.Fatalf("queue should keep unused allow-once, got %v", rest)
	}
}

// promptOptionsFor parses a craze prompt command line and answers the
// agent.Options its run builds (plan 041 §3.5): the flags as the command
// registers them, the options as run hands them to agent.New.
func promptOptionsFor(t *testing.T, argv ...string) agent.Options {
	t.Helper()
	// agentOptions reads the config and journal settings: the case's own,
	// never a developer's.
	isolateRunEnv(t)
	cmd := newPromptCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.RunE = func(*cobra.Command, []string) error { return nil }
	cmd.SetArgs(argv)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	effort, _ := cmd.Flags().GetString("effort")
	fast, _ := cmd.Flags().GetBool("fast")
	noFast, _ := cmd.Flags().GetBool("no-fast")
	po := &promptOpts{effort: effort, fast: fast, noFast: noFast, force: true, stderr: io.Discard}
	return po.agentOptions(t.TempDir(), nil, nil)
}

// TestPromptStartSettingsBuildTheOptions is plan 041 §3.5 (criterion 19) on
// the built options: --effort trimmed, --fast and --no-fast as true and false,
// and neither as the empty effort and a nil Fast — the options of a run that
// never heard of the flags.
func TestPromptStartSettingsBuildTheOptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		argv   []string
		effort string
		fast   *bool
	}{
		{"neither", nil, "", nil},
		{"effort", []string{"--effort", " high "}, "high", nil},
		{"fast", []string{"--fast"}, "", boolPtr(true)},
		{"no-fast", []string{"--no-fast"}, "", boolPtr(false)},
		{"all", []string{"--effort", "low", "--no-fast"}, "low", boolPtr(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := promptOptionsFor(t, tc.argv...)
			if got.Effort != tc.effort {
				t.Fatalf("Effort %q, want %q", got.Effort, tc.effort)
			}
			if (got.Fast == nil) != (tc.fast == nil) || (got.Fast != nil && *got.Fast != *tc.fast) {
				t.Fatalf("Fast %v, want %v", got.Fast, tc.fast)
			}
		})
	}
}

// TestPromptFastAndNoFastAreAUsageError: the session flags' wording and exit
// code (2), refused before anything is resolved or started.
func TestPromptFastAndNoFastAreAUsageError(t *testing.T) {
	isolateRunEnv(t)
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"prompt", "--fast", "--no-fast", "hi"})
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("got %v", err)
	}
	if ee.msg != "craze: --fast and --no-fast are mutually exclusive" {
		t.Fatalf("msg %q", ee.msg)
	}
}

// TestPromptStartSettingsReachTheAgent is criterion 19's wire half: against
// the fake agent's per-model catalog (grok-4.6 offers effort and fast), the
// flags arrive as session/set_config_option calls, after the model and ahead
// of the first prompt.
func TestPromptStartSettingsReachTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want []string
	}{
		{"effort", []string{"--effort", "low"}, []string{"model=grok-4.6", "effort=low"}},
		{"fast", []string{"--no-fast"}, []string{"model=grok-4.6", "fast=false"}},
		{"neither", nil, []string{"model=grok-4.6"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "calls")
			t.Setenv("CRAZE_FAKE_DUMP_CALLS", path)
			runPromptStdout(t, "permodel", append([]string{"--json", "--model", "grok-4.6"}, tc.argv...), "go")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			calls := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
			var sets []string
			prompts := 0
			for i, c := range calls {
				if v, ok := strings.CutPrefix(c, "session/set_config_option "); ok {
					sets = append(sets, v)
				}
				if c == "session/prompt" {
					prompts++
					if len(sets) != len(tc.want) {
						t.Fatalf("a prompt at call %d, ahead of the start settings %v: %q", i, tc.want, calls)
					}
				}
			}
			// The run's one prompt, after every start setting: a run that
			// never prompted would satisfy the ordering vacuously.
			if prompts != 1 {
				t.Fatalf("%d session/prompt calls, want 1: %q", prompts, calls)
			}
			if !slices.Equal(sets, tc.want) {
				t.Fatalf("sets %q, want %q (calls %q)", sets, tc.want, calls)
			}
		})
	}
}

// TestPlainPromptPutsAParagraphBreakBetweenTextRuns (plan 047 D7): plain
// output separates a text run that starts after a top-level non-todo tool, a
// non-empty thought or a turn's done from the output so far by a blank line,
// adding only the newlines missing; a child's events set no boundary and its
// text is not written; --json is the events as they are. Negative control:
// the cases fail if runBoundary is never consulted (no breaks) or an empty
// chunk consumes the boundary.
func TestPlainPromptPutsAParagraphBreakBetweenTextRuns(t *testing.T) {
	text := func(s string) agent.Event { return agent.Event{Type: agent.EventText, Text: s} }
	child := func(ev agent.Event) agent.Event { ev.Agent = "sub-1"; return ev }
	tool := agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "c1", ToolName: "Shell"}}
	todo := agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "c2", ToolName: "updateTodos"}}
	thought := agent.Event{Type: agent.EventThought, Text: "hmm"}
	done := agent.Event{Type: agent.EventDone}
	for _, tc := range []struct {
		name   string
		events []agent.Event
		want   string
	}{
		{"nothing before the first text", []agent.Event{tool, text("a")}, "a"},
		{"a tool between texts", []agent.Event{text("a"), tool, text("b")}, "a\n\nb"},
		{"a thought between texts", []agent.Event{text("a"), thought, text("b")}, "a\n\nb"},
		{"a turn boundary", []agent.Event{text("a"), done, text("b")}, "a\n\nb"},
		{"no boundary, no break", []agent.Event{text("a"), text("b")}, "ab"},
		{"one trailing newline", []agent.Event{text("a\n"), tool, text("b")}, "a\n\nb"},
		{"two trailing newlines", []agent.Event{text("a\n\n"), tool, text("b")}, "a\n\nb"},
		{"a chunk starting with a newline", []agent.Event{text("a"), tool, text("\nb")}, "a\n\nb"},
		{"a chunk starting with two", []agent.Event{text("a"), tool, text("\n\nb")}, "a\n\nb"},
		{"one trailing and a leading newline", []agent.Event{text("a\n"), tool, text("\nb")}, "a\n\nb"},
		{"an empty chunk keeps the boundary", []agent.Event{text("a"), tool, text(""), text("b")}, "a\n\nb"},
		{"empty chunks at the start", []agent.Event{tool, text(""), text("a")}, "a"},
		{"an empty thought is no boundary", []agent.Event{text("a"), {Type: agent.EventThought}, text("b")}, "ab"},
		{"a todo tool is no boundary", []agent.Event{text("a"), todo, text("b")}, "ab"},
		{"a child's events set none", []agent.Event{text("a"), child(tool), child(thought), child(done), text("b")}, "ab"},
		{"a child's text is not written", []agent.Event{text("a"), tool, child(text("zz")), text("b")}, "a\n\nb"},
		{"several boundaries, one break", []agent.Event{text("a"), tool, thought, done, text("b")}, "a\n\nb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			o := &promptOpts{stdout: &out}
			for _, ev := range tc.events {
				if err := o.writeEvent(ev); err != nil {
					t.Fatal(err)
				}
			}
			if out.String() != tc.want {
				t.Fatalf("output %q; want %q", out.String(), tc.want)
			}
		})
	}
}
