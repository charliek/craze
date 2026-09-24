package agent_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/transcript"
)

// TestNativeRosterMatchesFoldedEvents (A11, S1c's A11 rule, plan 026 X8): the
// roster a native session keeps (Snapshot().Subagents) is the roster every
// client folds from its events, at every quiescent point — spawn order and
// evictions included — with internal/transcript's real fold: the engine's own
// model, and what an attached client builds. It is an external test because
// internal/transcript imports internal/agent.
//
// Ten turns each fan out several children at once, so their spawns, their
// progress and their finishes race each other in whatever order the scheduler
// picks, and the thirty-seven of them push the roster past its 32 finished
// rows — by one at turn 9's end, by four more at turn 10's. Some children read
// a file, some take two steps, some fail. Turn 1's two are held and let go in
// the reverse of the order they spawned in, so the first row evicted is the
// oldest finish and not the first spawned: the rule the fold replicates. The
// session's clock is fixed, so every finish carries the same reading and
// EndedAt is the adapter's 1 ns-apart stamp. After each turn — its ending
// published and the fold caught up to it — the fold's roster equals the
// snapshot's field for field, the fold holds a transcript for exactly the
// children the roster holds, and each one's transcript is what that child
// streamed: its task as the user line, its tool rows terminal, its answer.
// Lossy tool progress is the one carve-out (§3.9); no call here makes any.
func TestNativeRosterMatchesFoldedEvents(t *testing.T) {
	r := &foldRouter{queues: map[string][]foldStep{}}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	table := &modeltable.Table{
		DefaultModel: "fold/a",
		Providers: map[string]modeltable.Provider{
			"fold": {Driver: modeltable.DriverOpenAICompat, BaseURL: "http://127.0.0.1:9/v1", EnvKeys: []string{"FOLD_TEST_KEY"}},
		},
		Models: map[string]modeltable.Model{"fold/a": {Provider: "fold", WireModel: "wire-a"}},
	}
	home := t.TempDir()
	s := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()}, func(o *harness.Options) {
		o.Home, o.Table = home, table
		o.Getenv = func(k string) string {
			if k == "FOLD_TEST_KEY" {
				return "sk-fold-test-key-0001"
			}
			return ""
		}
		o.NewModel = func(modeltable.Resolved) (fantasy.LanguageModel, error) { return r, nil }
		o.Now = func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) }
	})
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() {
			_ = s.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(contractWait):
			t.Errorf("Close at cleanup did not return within %v", contractWait)
		}
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The fold, fed every committed event from a subscription — decoded, as
	// any client receives them — on a goroutine of its own.
	sub, err := s.(agent.EventSource).Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	m := transcript.New(transcript.Options{ErrText: func(err error) string { return err.Error() }})
	go func() {
		for rec := range sub.Records() {
			if ev, err := rec.Event(); err == nil {
				m.Fold(ev)
			}
		}
	}()
	// The primary's one reader: what each child streamed, and the seq of each
	// turn's ending.
	var pmu sync.Mutex
	var primary []agent.Event
	go func() {
		for ev := range s.Events() {
			pmu.Lock()
			primary = append(primary, ev)
			pmu.Unlock()
		}
	}()
	streamed := func() []agent.Event {
		pmu.Lock()
		defer pmu.Unlock()
		return slices.Clone(primary)
	}

	answers := map[string]string{}
	var done uint64 // the seq of the last turn's ending
	total := 0      // children so far
	// The children each turn fans out: turn 1's two are ordered below; the
	// counts put the 33rd finish — the first eviction — at turn 9's end and
	// four more at turn 10's.
	for turn, n := range []int{1: 2, 2: 4, 3: 4, 4: 4, 5: 4, 6: 4, 7: 4, 8: 4, 9: 3, 10: 4} {
		if n == 0 {
			continue
		}
		prompt := fmt.Sprintf("turn %d", turn)
		var calls [][]fantasy.StreamPart
		var holds []chan struct{}
		for c := 1; c <= n; c++ {
			task := fmt.Sprintf("task %d.%d", turn, c)
			calls = append(calls, foldAgentCall(t, fmt.Sprintf("a%d", c), "do "+task, task))
			switch {
			case turn == 1: // held until the test lets it finish
				release := make(chan struct{})
				t.Cleanup(func() {
					select {
					case <-release:
					default:
						close(release)
					}
				})
				holds = append(holds, release)
				r.route(task, foldHeld(release, "held "+task))
				answers[task] = "held " + task
			case c == 1: // a read, then the answer
				r.route(task, foldCall("r", "read", `{"filePath":"main.go"}`), foldAnswer("did "+task))
				answers[task] = "did " + task
			case c == 2: // two reads in two steps
				r.route(task, foldCall("r1", "read", `{"filePath":"main.go"}`), foldCall("r2", "read", `{"filePath":"main.go"}`),
					foldAnswer("twice "+task))
				answers[task] = "twice " + task
			case c == 3: // an answer straight away
				r.route(task, foldAnswer("at once "+task))
				answers[task] = "at once " + task
			case c == 4: // a failure after some output
				r.route(task, foldReply(foldText("partial "+task), []fantasy.StreamPart{{Type: fantasy.StreamPartTypeError, Error: errors.New("down")}}))
				answers[task] = "partial " + task
			}
		}
		total += n
		r.route(prompt, foldReply(append(calls, foldFinish(fantasy.FinishReasonToolCalls))...), foldAnswer("done "+prompt))
		out := make(chan error, 1)
		go func() {
			_, err := s.Prompt(context.Background(), prompt)
			out <- err
		}()
		if turn == 1 {
			// Both spawned, in the order the scheduler chose; the one spawned
			// second finishes first. The first eviction (turn 9) takes the
			// oldest FINISH, which is then never the first spawned: a roster
			// that evicted in spawn order parts from the fold there.
			var spawned []string
			foldWaitFor(t, "turn 1's two spawns", func() bool {
				spawned = spawned[:0]
				for _, ev := range streamed() {
					if ev.Type == agent.EventSubagent && ev.SubagentChange == agent.SubagentChangeSpawned {
						spawned = append(spawned, ev.Subagent.ID)
					}
				}
				return len(spawned) == 2
			})
			for _, id := range []string{spawned[1], spawned[0]} {
				row := foldRow(s, id)
				close(holds[slices.Index([]string{"task 1.1", "task 1.2"}, row.Prompt)])
				foldWaitFor(t, "the finish of "+row.Prompt, func() bool {
					return slices.ContainsFunc(streamed(), func(ev agent.Event) bool {
						return ev.Type == agent.EventSubagent && ev.SubagentChange == agent.SubagentChangeFinished && ev.Subagent.ID == id
					})
				})
			}
		}
		if err := <-out; err != nil {
			t.Fatalf("%s: %v", prompt, err)
		}
		// Quiescent: the turn's ending is on the primary — the last event the
		// turn publishes — and the fold has folded every event up to it.
		foldWaitFor(t, prompt+"'s ending", func() bool {
			evs := streamed()
			if i := slices.IndexFunc(evs, func(ev agent.Event) bool { return ev.Type == agent.EventDone && ev.Seq > done }); i >= 0 {
				done = evs[i].Seq
				return true
			}
			return false
		})
		foldWaitFor(t, "the fold to reach "+prompt+"'s ending", func() bool { return m.Seq() >= done })

		snap := s.Snapshot().Subagents
		folded := m.State().Agents
		if len(snap) != len(folded) {
			t.Fatalf("%s: the snapshot's roster holds %d rows and the fold's %d", prompt, len(snap), len(folded))
		}
		for i := range snap {
			if d := rowDiff(snap[i], folded[i]); d != "" {
				t.Fatalf("%s: row %d differs, %s\nsnapshot %+v\nfolded   %+v", prompt, i, d, snap[i], folded[i])
			}
		}
		// Every child has finished once its turn has ended, and 32 finished
		// rows stay.
		if want, got := min(total, 32), len(snap); want != got {
			t.Fatalf("%s: the roster holds %d rows, want %d", prompt, got, want)
		}
		subs := m.Subs()
		slices.Sort(subs)
		rosterIDs := make([]string, 0, len(snap))
		for _, row := range snap {
			rosterIDs = append(rosterIDs, row.ID)
		}
		slices.Sort(rosterIDs)
		if !slices.Equal(subs, rosterIDs) {
			t.Fatalf("%s: the fold holds transcripts for %v; the roster for %v", prompt, subs, rosterIDs)
		}
		evs := streamed()
		for _, row := range snap {
			checkChildTranscript(t, m, row, answers[row.Prompt], evs)
		}
	}
}

// checkChildTranscript holds one child's folded transcript against its roster
// row and what it streamed on the primary: the task as the first user line,
// every tool row it streamed there and terminal, and its answer as its
// assistant text.
func checkChildTranscript(t *testing.T, m *transcript.Model, row agent.SubagentInfo, answer string, evs []agent.Event) {
	t.Helper()
	sub := m.Sub(row.ID)
	if sub == nil {
		t.Fatalf("the fold holds no transcript for %s", row.ID)
	}
	var user, text []string
	tools := map[string]string{}
	for _, e := range sub.Entries() {
		switch e.Kind {
		case transcript.KindUser:
			user = append(user, e.Text)
		case transcript.KindAssistant:
			text = append(text, e.Text)
		case transcript.KindTool:
			tools[e.Tool.ID] = e.Tool.Status
		}
	}
	if !slices.Equal(user, []string{row.Prompt}) {
		t.Fatalf("child %q: its transcript's user lines are %q, want its task", row.Prompt, user)
	}
	var streamedText strings.Builder
	streamedTools := map[string]string{}
	for _, ev := range evs {
		if ev.Agent != row.ID {
			continue
		}
		switch ev.Type {
		case agent.EventText:
			streamedText.WriteString(ev.Text)
		case agent.EventTool:
			streamedTools[ev.Tool.ID] = ev.Tool.Status
		}
	}
	if got := strings.Join(text, ""); got != answer || streamedText.String() != answer {
		t.Fatalf("child %q: its transcript says %q and it streamed %q; want %q", row.Prompt, got, streamedText.String(), answer)
	}
	if fmt.Sprint(tools) != fmt.Sprint(streamedTools) {
		t.Fatalf("child %q: its transcript's tools %v, streamed %v", row.Prompt, tools, streamedTools)
	}
	for id, st := range tools {
		if st == "pending" || st == "in_progress" {
			t.Fatalf("child %q: its tool %s is still %s", row.Prompt, id, st)
		}
	}
}

// rowDiff names the first field two roster rows disagree on, "" for none. The
// times compare as instants: the fold's are decoded UTC.
func rowDiff(a, b agent.SubagentInfo) string {
	switch {
	case a.ID != b.ID:
		return "ID"
	case a.AttemptID != b.AttemptID || a.ParentID != b.ParentID || a.ToolCallID != b.ToolCallID:
		return "the ids"
	case a.Description != b.Description || a.SubagentType != b.SubagentType || a.Model != b.Model:
		return "the labels"
	case a.Status != b.Status || a.Error != b.Error:
		return "the status"
	case a.Prompt != b.Prompt || a.Output != b.Output || a.Activity != b.Activity:
		return "the texts"
	case !a.StartedAt.Equal(b.StartedAt) || !a.EndedAt.Equal(b.EndedAt) || a.DurationMs != b.DurationMs:
		return "the times"
	case a.ToolCalls != b.ToolCalls || a.Turns != b.Turns || a.TokensUsed != b.TokensUsed:
		return "the counts"
	case !slices.Equal(a.ToolsUsed, b.ToolsUsed):
		return "ToolsUsed"
	case a.Transcript != b.Transcript || a.Background != b.Background:
		return "the flags"
	}
	return ""
}

// foldWaitFor polls cond until it holds: every wait is for a positive fact.
func foldWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(contractWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// foldStep is one scripted response.
type foldStep func(ctx context.Context, yield func(fantasy.StreamPart) bool)

// foldRouter answers each request with the next step queued for its turn's
// prompt, so children streaming at once never take each other's steps (the
// harness's router, kept small here). The prompt is the LAST user message:
// the parent runs nine turns, each request carrying the ones before, and an
// agent-mode turn with no steer adds no user message after its prompt; a
// child runs one turn.
type foldRouter struct {
	mu     sync.Mutex
	queues map[string][]foldStep
}

func (m *foldRouter) route(prompt string, steps ...foldStep) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queues[prompt] = append(m.queues[prompt], steps...)
}

func (m *foldRouter) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	key := ""
	for _, msg := range call.Prompt {
		if msg.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, p := range msg.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
				key = tp.Text
				break
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.queues[key]
	if len(q) == 0 {
		return nil, fmt.Errorf("foldRouter: no step queued for %q", key)
	}
	next := q[0]
	m.queues[key] = q[1:]
	return func(yield func(fantasy.StreamPart) bool) { next(ctx, yield) }, nil
}

func (m *foldRouter) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("foldRouter: Generate is not used")
}

func (m *foldRouter) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("foldRouter: GenerateObject is not used")
}

func (m *foldRouter) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("foldRouter: StreamObject is not used")
}

func (m *foldRouter) Provider() string { return "fold" }
func (m *foldRouter) Model() string    { return "wire-a" }

func foldReply(parts ...[]fantasy.StreamPart) foldStep {
	return func(_ context.Context, yield func(fantasy.StreamPart) bool) {
		for _, group := range parts {
			for _, p := range group {
				if !yield(p) {
					return
				}
			}
		}
	}
}

func foldText(text string) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeTextStart, ID: "0"},
		{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: text},
		{Type: fantasy.StreamPartTypeTextEnd, ID: "0"},
	}
}

func foldFinish(reason fantasy.FinishReason) []fantasy.StreamPart {
	return []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish, FinishReason: reason,
		Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}}}
}

func foldAnswer(text string) foldStep {
	return foldReply(foldText(text), foldFinish(fantasy.FinishReasonStop))
}

// foldHeld answers text and ends only once release is closed: how a test
// decides the order children finish in.
func foldHeld(release <-chan struct{}, text string) foldStep {
	return func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		select {
		case <-release:
		case <-ctx.Done():
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
			return
		}
		foldAnswer(text)(ctx, yield)
	}
}

// foldRow is the roster row of child id.
func foldRow(s agent.Session, id string) agent.SubagentInfo {
	for _, row := range s.Snapshot().Subagents {
		if row.ID == id {
			return row
		}
	}
	return agent.SubagentInfo{}
}

func foldCallParts(id, name, input string) []fantasy.StreamPart {
	return []fantasy.StreamPart{
		{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: name},
		{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: input},
		{Type: fantasy.StreamPartTypeToolInputEnd, ID: id},
		{Type: fantasy.StreamPartTypeToolCall, ID: id, ToolCallName: name, ToolCallInput: input},
	}
}

func foldCall(id, name, input string) foldStep {
	return foldReply(foldCallParts(id, name, input), foldFinish(fantasy.FinishReasonToolCalls))
}

func foldAgentCall(t *testing.T, id, description, prompt string) []fantasy.StreamPart {
	t.Helper()
	return foldCallParts(id, "agent", fmt.Sprintf(`{"description":%q,"prompt":%q}`, description, prompt))
}
