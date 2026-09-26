package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/tool/opencode"
)

// Resume and replay (plan 028 §3.3, §3.4; §7 A1's harness part, A5, A6's
// harness half). Every stored session here is written by real turns on the
// scripted models, closed, and reopened by id from the same home.

// resumeOptions are opts reopening session id.
func resumeOptions(opts Options, id string) Options {
	opts.Resume = id
	return opts
}

// storedTurn opens a session with opts, runs prep on it, runs one turn on the
// model it is then on, closes it, and returns its id.
func storedTurn(t *testing.T, f *fixture, opts Options, prep func(*Session)) string {
	t.Helper()
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if prep != nil {
		prep(s)
	}
	cur, _ := s.Current()
	f.models[cur].push(answerWith("ok"))
	run(t, s, "hi")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s.ID()
}

// resumed opens opts, failing the test on an error, and closes the session
// when the test ends.
func resumed(t *testing.T, opts Options) *Session {
	t.Helper()
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open (resume %s): %v", opts.Resume, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// userTurns are the turns the transcript's user entries record, in order: 0
// for one that records none (a steer, or results).
func userTurns(tr *store.Transcript) []int {
	var out []int
	for _, e := range tr.Entries {
		if e.Type == store.TypeMessage && e.Message.Role == fantasy.MessageRoleUser {
			out = append(out, e.Turn)
		}
	}
	return out
}

// addModel adds alias to f's table, with a scripted model to build it as.
func addModel(f *fixture, alias, provider, wire string) {
	f.table.Models[alias] = modeltable.Model{Provider: provider, WireModel: wire}
	f.models[alias] = &scripted{provider: provider, wire: wire}
}

// TestResumeContinuesTheSameTranscript (A1, the harness's part): two turns
// with output around one that has none, Close, and Open by id — the same id
// and the same file — then a turn that appends to it, numbered after the
// largest turn the file records rather than after the count of its prompts,
// so its call ids never repeat the last incarnation's. The resume entry leads
// the new turn's step, and the request carries the whole history.
func TestResumeContinuesTheSameTranscript(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s, err := Open(f.options())
	if err != nil {
		t.Fatal(err)
	}
	a := f.models["test/a"]
	a.push(
		answerWith("first"),
		reply(reasoningParts("only thinking"), finish(fantasy.FinishReasonStop)),
		callStep(globPart("g1")), answerWith("third"),
	)
	run(t, s, "one")
	run(t, s, "two") // persists nothing: turn 2 is spent, and recorded nowhere
	var ev3 events
	if _, err := s.Run(context.Background(), "three", ev3.sink); err != nil {
		t.Fatal(err)
	}
	if st := of[ToolStarted](ev3.list()); len(st) != 1 || st[0].ID != "t3.1.1" {
		t.Fatalf("turn three's calls: %+v; want t3.1.1", st)
	}
	id, path := s.ID(), s.store.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := resumed(t, resumeOptions(f.options(), id))
	if s2.ID() != id || s2.store.Path() != path {
		t.Fatalf("resumed as %s at %s; want %s at %s", s2.ID(), s2.store.Path(), id, path)
	}
	if cur, effort := s2.Current(); cur != "test/a" || effort != "high" {
		t.Fatalf("resumed on %s at %q; want the transcript's test/a at high", cur, effort)
	}
	a.push(callStep(globPart("g2")), answerWith("fourth"))
	var ev4 events
	if _, err := s2.Run(context.Background(), "four", ev4.sink); err != nil {
		t.Fatal(err)
	}
	if st := of[ToolStarted](ev4.list()); len(st) != 1 || st[0].ID != "t4.1.1" {
		t.Fatalf("the resumed turn's calls: %+v; want t4.1.1, after the largest recorded turn", st)
	}

	tr := transcript(t, s2)
	equal(t, "transcript", entries(tr), []string{
		"user test/a high: one",
		"assistant test/a high end_turn: first",
		"user test/a high: three",
		"assistant test/a high tool_use: [call g1 glob {\"pattern\":\"*.none\"}]",
		"tool test/a high: [result g1: No files found]",
		"assistant test/a high end_turn: third",
		"resume",
		"user test/a high: four",
		"assistant test/a high tool_use: [call g2 glob {\"pattern\":\"*.none\"}]",
		"tool test/a high: [result g2: No files found]",
		"assistant test/a high end_turn: fourth",
	})
	equal(t, "the user entries' turns", userTurns(tr), []int{1, 3, 4})
	if c := tr.Entries[6].Contract; c.SystemPromptSHA256 != s2.PromptSHA256() || c.ToolProfile != opencode.Name || c.CrazeVersion != "v0.0.0-test" {
		t.Fatalf("the resume entry records %+v; want this incarnation's prompt digest %s, the profile and the version", c, s2.PromptSHA256())
	}
	names, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.jsonl"))
	if err != nil || len(names) != 1 || names[0] != path {
		t.Fatalf("the session directory holds %v (%v); want the one transcript", names, err)
	}
	calls := a.requests()
	equal(t, "the resumed turn's first request", promptOf(calls[len(calls)-2])[1:], []string{
		"user: one",
		"assistant: first",
		"user: three",
		"assistant: [call g1 glob {\"pattern\":\"*.none\"}]",
		"tool: [result g1: No files found]",
		"assistant: third",
		"user: four",
	})
	if items := s2.tools.todos.snapshot(); len(items) != 0 {
		t.Fatalf("a transcript that recorded no list restored %+v", items)
	}
}

// TestResumeRefusals: a session with no file is ErrNoTranscript, and opens
// nothing; one another session holds is the store's ErrBusy, wrapped with
// the session's id; and a sub-agent is never resumed.
func TestResumeRefusals(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	_, err := Open(resumeOptions(f.options(), "0b8f3c1e-0000-4000-8000-000000000000"))
	if !errors.Is(err, ErrNoTranscript) || !strings.HasPrefix(err.Error(), "harness: resume 0b8f3c1e-") {
		t.Fatalf("resuming a session with no file = %v; want ErrNoTranscript, naming it", err)
	}
	if _, err := os.Stat(filepath.Join(f.home, "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused resume created the sessions directory (stat: %v)", err)
	}

	id := storedTurn(t, f, f.options(), nil)
	s := resumed(t, resumeOptions(f.options(), id))
	if _, err := Open(resumeOptions(f.options(), id)); !errors.Is(err, store.ErrBusy) || !strings.Contains(err.Error(), "resume "+id) {
		t.Fatalf("a second resume while the first holds the session = %v; want ErrBusy", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	resumed(t, resumeOptions(f.options(), id)) // Close released the lock

	opts := resumeOptions(f.options(), id)
	opts.Child = &ChildOptions{ID: "child-1"}
	if _, err := Open(opts); err == nil || !strings.Contains(err.Error(), "never resumed") {
		t.Fatalf("resuming as a sub-agent = %v; want a refusal", err)
	}
}

// TestResumeModelPrecedence (A5, the harness's part): an explicit model wins
// and must resolve; unspecified, the transcript's model is found by identity
// — its own alias while it still names it, else the first alias in sorted
// order that does, never a re-pointed alias — then the table's default, and
// a model with another tool profile is skipped; none is ErrResumeModel. The
// effort and the mode follow the same shape. A refused resume releases the
// session's lock.
func TestResumeModelPrecedence(t *testing.T) {
	const url = "http://127.0.0.1:1/v1"
	onB := func(s *Session) {
		if err := s.SetModel("test/b"); err != nil {
			t.Fatal(err)
		}
	}
	current := func(t *testing.T, s *Session, model, effort string) {
		t.Helper()
		if m, e := s.Current(); m != model || e != effort {
			t.Fatalf("resumed on %s at %q; want %s at %q", m, e, model, effort)
		}
	}
	warned := func(opts *Options) *[]string {
		var mu sync.Mutex
		var out []string
		opts.Warn = func(msg string) {
			mu.Lock()
			defer mu.Unlock()
			out = append(out, msg)
		}
		return &out
	}

	t.Run("explicit wins", func(t *testing.T) {
		f := newFixture(t, url)
		id := storedTurn(t, f, f.options(), nil)
		opts := resumeOptions(f.options(), id)
		opts.Model = "test/b"
		s := resumed(t, opts)
		current(t, s, "test/b", "high") // test/b accepts the transcript's high
		f.models["test/b"].push(answerWith("ok"))
		run(t, s, "again")
		if got := entries(transcript(t, s))[2:4]; !slices.Equal(got, []string{"resume", "model_change test/b"}) {
			t.Fatalf("after the explicit switch the transcript has %v; want the resume entry, then the switch, as a live one", got)
		}
	})
	t.Run("the transcript's own alias", func(t *testing.T) {
		f := newFixture(t, url)
		id := storedTurn(t, f, f.options(), onB)
		addModel(f, "test/a-b", "test", "wire-b") // sorts first, and is the same model
		opts := resumeOptions(f.options(), id)
		warns := warned(&opts)
		s := resumed(t, opts)
		current(t, s, "test/b", "high") // the effort SetModel carried over, which test/b accepts
		if len(*warns) != 0 {
			t.Fatalf("resuming on the transcript's own model warned %q", *warns)
		}
	})
	t.Run("a re-pointed alias is not followed", func(t *testing.T) {
		f := newFixture(t, url)
		id := storedTurn(t, f, f.options(), onB)
		m := f.table.Models["test/b"]
		m.WireModel = "wire-a" // the alias now names another model
		f.table.Models["test/b"] = m
		addModel(f, "test/z-b", "test", "wire-b")
		addModel(f, "test/m-b", "test", "wire-b")
		opts := resumeOptions(f.options(), id)
		warns := warned(&opts)
		s := resumed(t, opts)
		current(t, s, "test/m-b", "") // the first alias in sorted order with the identity
		if len(*warns) != 0 {
			t.Fatalf("resuming on the same model under another alias warned %q", *warns)
		}
		f.models["test/m-b"].push(answerWith("ok"))
		run(t, s, "again")
		// After the stored turn's model_change, prompt and answer.
		if got := entries(transcript(t, s))[3:6]; !slices.Equal(got, []string{"resume", "model_change test/m-b", "effort_change "}) {
			t.Fatalf("the transcript has %v; want the alias and the effort recorded as switches", got)
		}
	})
	t.Run("the default", func(t *testing.T) {
		f := newFixture(t, url)
		id := storedTurn(t, f, f.options(), onB)
		delete(f.table.Models, "test/b")
		opts := resumeOptions(f.options(), id)
		warns := warned(&opts)
		s := resumed(t, opts)
		current(t, s, "test/a", "high")
		if len(*warns) != 1 || !strings.Contains((*warns)[0], "test/b") || !strings.Contains((*warns)[0], "continuing on test/a") {
			t.Fatalf("falling back to the default warned %q; want one warning naming both models", *warns)
		}
	})
	t.Run("an explicit model that does not resolve refuses", func(t *testing.T) {
		f := newFixture(t, url)
		id := storedTurn(t, f, f.options(), nil)
		for alias, want := range map[string]error{"nokey/d": ErrNoAPIKey, "gone/x": ErrUnknownModel} {
			opts := resumeOptions(f.options(), id)
			opts.Model = alias
			if _, err := Open(opts); !errors.Is(err, want) {
				t.Fatalf("resuming on %s = %v; want %v", alias, err, want)
			}
		}
		resumed(t, resumeOptions(f.options(), id)) // the refusals released the lock
	})
	t.Run("another tool profile is skipped", func(t *testing.T) {
		f := newFixture(t, url)
		profiles := func() (*tool.Registry, error) {
			p, err := opencode.Profile()
			if err != nil {
				return nil, err
			}
			var reg tool.Registry
			if err := reg.Register(p); err != nil {
				return nil, err
			}
			second := tool.Profile{Name: "second", Tools: []tool.Tool{&probeTool{}}, System: func(tool.SystemEnv) string { return "second" }}
			return &reg, reg.Register(second)
		}
		opts := f.options()
		opts.tools.profiles = profiles
		id := storedTurn(t, f, opts, onB)
		m := f.table.Models["test/b"]
		m.ToolProfile = "second"
		f.table.Models["test/b"] = m

		explicit := resumeOptions(opts, id)
		explicit.Model = "test/b"
		if _, err := Open(explicit); !errors.Is(err, ErrProfileMismatch) {
			t.Fatalf("resuming explicitly on a model with another profile = %v; want ErrProfileMismatch", err)
		}
		ropts := resumeOptions(opts, id)
		warns := warned(&ropts)
		s := resumed(t, ropts)
		current(t, s, "test/a", "high")
		if len(*warns) != 1 {
			t.Fatalf("skipping the transcript's model warned %q; want once", *warns)
		}
	})
	t.Run("none is ErrResumeModel", func(t *testing.T) {
		f := newFixture(t, url)
		id := storedTurn(t, f, f.options(), onB)
		delete(f.table.Models, "test/b")
		f.table.DefaultModel = "nokey/d"
		_, err := Open(resumeOptions(f.options(), id))
		if !errors.Is(err, ErrResumeModel) || !strings.Contains(err.Error(), `"opencode"`) || !strings.Contains(err.Error(), "nokey") {
			t.Fatalf("resuming with no usable model = %v; want ErrResumeModel naming the profile and why", err)
		}
		f.table.DefaultModel = "test/a"
		resumed(t, resumeOptions(f.options(), id)) // the refusal released the lock
	})
	t.Run("effort", func(t *testing.T) {
		f := newFixture(t, url)
		id := storedTurn(t, f, f.options(), func(s *Session) {
			if err := s.SetEffort("low"); err != nil {
				t.Fatal(err)
			}
		})
		cases := []struct{ model, effort, want, wantModel string }{
			{"", "", "low", "test/a"},            // the transcript's
			{"", "high", "high", "test/a"},       // explicit
			{"test/b", "", "medium", "test/b"},   // test/b has no low: its default
			{"other/c", "", "", "other/c"},       // no effort control at all
			{"test/b", "high", "high", "test/b"}, // both explicit
		}
		for _, tc := range cases {
			opts := resumeOptions(f.options(), id)
			opts.Model, opts.Effort = tc.model, tc.effort
			s := resumed(t, opts)
			current(t, s, tc.wantModel, tc.want)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		}
		opts := resumeOptions(f.options(), id)
		opts.Effort = "max"
		if _, err := Open(opts); err == nil || !strings.Contains(err.Error(), `effort "max"`) {
			t.Fatalf("resuming at an effort the model lacks = %v; want a refusal", err)
		}
		resumed(t, resumeOptions(f.options(), id))
	})
	t.Run("mode", func(t *testing.T) {
		f := newFixture(t, url)
		plain := storedTurn(t, f, f.options(), nil)
		planned := storedTurn(t, f, modeOptions(f, modePlan), nil)
		cases := []struct{ id, mode, want string }{
			{plain, "", modeAgent},
			{planned, "", modePlan},
			{planned, modeAgent, modeAgent},
			{planned, modeAsk, modeAsk},
			{plain, modePlan, modePlan},
		}
		for _, tc := range cases {
			opts := resumeOptions(f.options(), tc.id)
			opts.Mode = tc.mode
			s := resumed(t, opts)
			if s.Mode() != tc.want {
				t.Fatalf("resuming %s with mode %q is in %q; want %q", tc.id, tc.mode, s.Mode(), tc.want)
			}
			if tc.want == modePlan && !exists(planPathOf(s)) {
				t.Fatal("a session resumed in plan mode has no plan file")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		}
		opts := resumeOptions(f.options(), planned)
		opts.Mode = "architect"
		if _, err := Open(opts); !errors.Is(err, ErrUnknownMode) {
			t.Fatalf("resuming in an unknown mode = %v; want ErrUnknownMode", err)
		}
	})
}

// TestResumeSeedsTheToldMode (A5, P29): the model was told of the mode the
// transcript last recorded. Resumed in plan mode, it reads the standing plan
// reminder — not the re-entry notice for a plan mode it "previously exited",
// though a plan is written — and nothing new is recorded; resumed from plan
// into agent mode, it reads the exit notice, and the switch is recorded with
// the step that carried it.
func TestResumeSeedsTheToldMode(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	a := f.models["test/a"]
	s, err := Open(modeOptions(f, modePlan))
	if err != nil {
		t.Fatal(err)
	}
	a.push(answerWith("planning"))
	run(t, s, "plan it")
	if err := os.WriteFile(planPathOf(s), []byte("# the plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, plan := s.ID(), planPathOf(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := resumed(t, resumeOptions(f.options(), id))
	if s2.Mode() != modePlan || planPathOf(s2) != plan {
		t.Fatalf("resumed in %q with plan file %s; want plan mode and %s", s2.Mode(), planPathOf(s2), plan)
	}
	a.push(answerWith("still planning"))
	run(t, s2, "go on")
	got, _ := reminderIn(t, a, len(a.requests())-1)
	if strings.Contains(got, "previously exited") || !strings.Contains(got, fmt.Sprintf(planFileWritten, plan)) {
		t.Fatalf("the resumed plan-mode turn read %q; want the standing reminder for a written plan", got)
	}
	tail := entries(transcript(t, s2))[3:] // after the first turn's mode_change, prompt and answer
	equal(t, "the resumed turn's entries", tail, []string{"resume", "user test/a high: go on", "assistant test/a high end_turn: still planning"})
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	opts := resumeOptions(f.options(), id)
	opts.Mode = modeAgent
	s3 := resumed(t, opts)
	a.push(answerWith("implementing"))
	run(t, s3, "build it")
	if got, _ := reminderIn(t, a, len(a.requests())-1); got != "user: "+reminderMessageText(fmt.Sprintf(planReminderExit, plan)) {
		t.Fatalf("resumed from plan into agent mode, the turn read %q; want the exit notice", got)
	}
	tail = entries(transcript(t, s3))[6:]
	equal(t, "the switch", tail, []string{"resume", "mode_change agent", "user test/a high: build it", "assistant test/a high end_turn: implementing"})
}

// reminderMessageText is a reminder's text as promptOf shows its message.
func reminderMessageText(text string) string { return messageText(reminderMessage(text)) }

// todoPart is one todo_write call.
func todoPart(id, args string) []fantasy.StreamPart { return callParts(id, "todo_write", args) }

// TestResumeRestoresTheLastTodos (A5, P7): a step whose calls changed the
// todo list records the list after the step on its tool entry — after both
// of two parallel calls — a step that did not records nothing, and an
// emptied list is recorded as []. A resume restores the last list on the
// path, and a replay ends with it.
func TestResumeRestoresTheLastTodos(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	a := f.models["test/a"]
	s, err := Open(f.options())
	if err != nil {
		t.Fatal(err)
	}
	a.push(
		callStep(todoPart("w1", `{"todos":[{"id":"a","content":"alpha"}]}`), todoPart("w2", `{"todos":[{"id":"b","content":"beta"}]}`)),
		answerWith("listed"),
		callStep(todoPart("w3", `{"todos":[{"id":"a","status":"completed"}]}`)),
		callStep(globPart("g1")),
		answerWith("worked"),
	)
	run(t, s, "plan")
	afterFirst := s.tools.todos.snapshot()
	run(t, s, "work")
	last := s.tools.todos.snapshot()
	if i := slices.IndexFunc(last, func(it tool.Todo) bool { return it.ID == "a" }); len(afterFirst) != 2 || len(last) != 2 || i < 0 || last[i].Status != tool.TodoCompleted {
		t.Fatalf("the lists after the turns: %+v, then %+v", afterFirst, last)
	}
	tr := transcript(t, s)
	var recorded []*[]store.Todo
	for _, e := range tr.Entries {
		if e.Type == store.TypeMessage && e.Message.Role == fantasy.MessageRoleTool {
			recorded = append(recorded, e.Todos)
		}
	}
	if len(recorded) != 3 || recorded[0] == nil || recorded[1] == nil || recorded[2] != nil {
		t.Fatalf("the tool entries record %v; want the first two steps' lists and nothing on the glob's", recorded)
	}
	equal(t, "the parallel step's list", toolTodos(*recorded[0]), afterFirst)
	equal(t, "the second step's list", toolTodos(*recorded[1]), last)
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := resumed(t, resumeOptions(f.options(), id))
	equal(t, "the restored list", s2.tools.todos.snapshot(), last)
	var ev events
	if err := s2.Replay(ev.sink); err != nil {
		t.Fatal(err)
	}
	evs := ev.list()
	if _, final := evs[len(evs)-1].(Todos); !final {
		t.Fatalf("the replay ends with a %T; want the Todos", evs[len(evs)-1])
	}
	if todos := of[Todos](evs); len(todos) != 1 || !slices.Equal(todos[0].Items, last) {
		t.Fatalf("the replay's Todos: %+v; want one, last, with the restored list", todos)
	}

	// Emptied: [] on the entry, and an empty list back.
	a.push(callStep(todoPart("w4", `{"merge":false,"todos":[]}`)), answerWith("cleared"))
	run(t, s2, "clear")
	raw, err := os.ReadFile(s2.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"todos":[]`) {
		t.Fatal("the emptied list is not recorded as []")
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := resumed(t, resumeOptions(f.options(), id))
	if items := s3.tools.todos.snapshot(); len(items) != 0 {
		t.Fatalf("an emptied list restored as %+v", items)
	}
	var ev3 events
	if err := s3.Replay(ev3.sink); err != nil {
		t.Fatal(err)
	}
	if n := len(of[Todos](ev3.list())); n != 0 {
		t.Fatalf("replaying an emptied list sent %d Todos; want none", n)
	}
}

// replayLine is one replayed event on one line.
func replayLine(ev Event) string {
	switch e := ev.(type) {
	case Prompted:
		if e.Steer {
			return "steer: " + e.Text
		}
		return "prompt: " + e.Text
	case TextDelta:
		return "text: " + e.Text
	case ThoughtDelta:
		return "thinking: " + e.Text
	case ToolStarted:
		return fmt.Sprintf("started %s step %d %s %s", e.ID, e.Step, e.Tool, e.Kind)
	case ToolCalled:
		return fmt.Sprintf("called %s %s", e.ID, e.Request.Tool)
	case ToolFinished:
		return fmt.Sprintf("finished %s error=%v replayed=%v", e.ID, e.Result.IsError, e.Replayed)
	case Todos:
		return fmt.Sprintf("todos %d", len(e.Items))
	}
	return fmt.Sprintf("%T", ev)
}

// callsOf are the ids of the transcript's assistant entries that made calls,
// in order.
func callsOf(tr *store.Transcript) []string {
	var ids []string
	for _, e := range tr.Entries {
		if e.Type == store.TypeMessage && e.Message.Role == fantasy.MessageRoleAssistant && len(openCalls(e.Message)) > 0 {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// TestReplayWalksTheTranscript (A6, the harness's half; §3.4's table): a
// resumed session replays its path in order — each prompt, the thinking and
// text, each call started and called as the dispatcher describes it and
// finished with its stored result as the card's content, under the ids
// <entry id>.<k> — a steer as a steer, a results entry as nothing, a
// background agent call as its launch receipt and no sub-agent of its own,
// the prompt redacted, and the restored todo list last, once. No lock of the
// harness's is held while the sink runs: it calls the session's methods,
// every one of which takes one, and a Run from it is refused. A second
// replay is ErrReplayed.
func TestReplayWalksTheTranscript(t *testing.T) {
	b := openBG(t)
	a := b.routers["test/a"]
	b.put("notes.txt", "a note\n")

	// Turn 1: a background agent call, answered at once with its receipt.
	w := newWorker()
	a.route("child one", w.step(openText("did child one"), finishText()))
	a.route("go", reply(reasoningParts("delegate"), bgPart(t, "a1", "job child one", "child one"), finish(fantasy.FinishReasonToolCalls)),
		answerWith("started"))
	var live events
	if _, err := b.s.Run(context.Background(), "go", live.sink); err != nil {
		t.Fatal(err)
	}
	child := startedWith(t, live.list(), "child one").ID
	b.finish(t, w)

	// Turn 2: the result is taken up after the prompt; three calls, held while
	// a steer is accepted, which the next step takes up.
	g := newGate()
	a.route("go",
		g.hold(cat(callParts("r1", "read", `{"filePath":"notes.txt"}`), callParts("b1", "bash", `{"command":"echo hi"}`),
			todoPart("w1", `{"todos":[{"id":"a","content":"alpha"}]}`)), finish(fantasy.FinishReasonToolCalls)),
		answerWith("all done"))
	out := start(context.Background(), b.s, "next with "+canary, live.sink)
	await(t, g.reached, "the second turn's first step")
	if err := sendSteer(b.s, "also check the tests"); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	if got := await(t, out, "the second turn"); got.err != nil || got.res.StopReason != StopEndTurn {
		t.Fatalf("the second turn: %+v, %v", got.res, got.err)
	}
	id := b.s.ID()
	if err := b.s.Close(); err != nil {
		t.Fatal(err)
	}

	s := resumed(t, resumeOptions(b.options(), id))
	tr := transcript(t, s)
	made := callsOf(tr)
	if len(made) != 2 {
		t.Fatalf("the transcript has %d entries with calls; want 2:\n%s", len(made), strings.Join(entries(tr), "\n"))
	}
	var mu sync.Mutex
	var got []Event
	sink := func(ev Event) {
		// Each takes a lock of the session's: the session's own, the modes',
		// the toolset's and the runner's.
		s.Current()
		s.Mode()
		s.Redact("x")
		s.HasPending()
		if _, err := s.Run(context.Background(), "during", nil); !errors.Is(err, ErrInTurn) {
			t.Errorf("a Run during the replay = %v; want ErrInTurn", err)
		}
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}
	done := make(chan error, 1)
	go func() { done <- s.Replay(sink) }()
	if err := await(t, done, "the replay"); err != nil {
		t.Fatal(err)
	}

	a1, a2 := made[0], made[1]
	var lines []string
	for _, ev := range got {
		lines = append(lines, replayLine(ev))
	}
	equal(t, "the replay", lines, []string{
		"prompt: go",
		"thinking: delegate",
		"started " + a1 + ".0 step 1 agent task",
		"called " + a1 + ".0 agent",
		"finished " + a1 + ".0 error=false replayed=true",
		"text: started",
		"prompt: next with " + redact.Marker,
		"started " + a2 + ".0 step 1 read read",
		"called " + a2 + ".0 read",
		"started " + a2 + ".1 step 1 bash execute",
		"called " + a2 + ".1 bash",
		"started " + a2 + ".2 step 1 todo_write todo",
		"called " + a2 + ".2 todo_write",
		"finished " + a2 + ".0 error=false replayed=true",
		"finished " + a2 + ".1 error=false replayed=true",
		"finished " + a2 + ".2 error=false replayed=true",
		"steer: also check the tests",
		"text: all done",
		"todos 1",
	})

	// Each call is described as it was live, and finished with the text the
	// model read, as the card's content.
	liveIDs := map[string]string{a1 + ".0": "t1.1.1", a2 + ".0": "t2.1.1", a2 + ".1": "t2.1.2", a2 + ".2": "t2.1.3"}
	for _, c := range of[ToolCalled](got) {
		want := callOf(t, live.list(), liveIDs[c.ID])
		equal(t, "the replayed request of "+c.ID, c.Request, want.Request)
		if !c.At.Equal(testNow()) {
			t.Errorf("%s was called at %v; want its entry's time", c.ID, c.At)
		}
	}
	for _, fin := range of[ToolFinished](got) {
		want := callResult(t, live.list(), liveIDs[fin.ID])
		if fin.Result.Text != want.Text || fin.Result.Content != want.Text || fin.Result.Output != nil || fin.Result.Edits != nil {
			t.Errorf("%s replayed as %+v; want the text the model read, %q, as its content and nothing else", fin.ID, fin.Result, want.Text)
		}
	}
	if receipt := of[ToolFinished](got)[0].Result.Content; receipt != ackText(child) {
		t.Errorf("the background call replayed %q; want its receipt", receipt)
	}
	if bash := of[ToolFinished](got)[2].Result.Content; !strings.Contains(bash, "hi") {
		t.Errorf("the command's card shows %q; want its output", bash)
	}
	for _, ev := range got {
		switch ev.(type) {
		case SubagentStarted, SubagentEvent, SubagentFinished, StepDone:
			t.Errorf("the replay sent a %T", ev)
		}
	}

	if err := s.Replay(nil); !errors.Is(err, ErrReplayed) {
		t.Fatalf("a second replay = %v; want ErrReplayed", err)
	}
}

// callOf is the ToolCalled of the call id.
func callOf(t *testing.T, evs []Event, id string) ToolCalled {
	t.Helper()
	for _, c := range of[ToolCalled](evs) {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no ToolCalled for %s", id)
	return ToolCalled{}
}

// TestReplayRefusals: a replay comes before the first turn — after one it is
// ErrInTurn, also on a new session, whose replay sends nothing — and after
// Close it is ErrClosed.
func TestReplayRefusals(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	var ev events
	if err := s.Replay(ev.sink); err != nil || len(ev.list()) != 0 {
		t.Fatalf("a new session's replay = %v, sent %v; want nothing", err, ev.list())
	}
	s2 := f.open(f.options())
	f.models["test/a"].push(answerWith("ok"))
	run(t, s2, "hi")
	if err := s2.Replay(nil); !errors.Is(err, ErrInTurn) {
		t.Fatalf("a replay after a turn = %v; want ErrInTurn", err)
	}
	s3 := f.open(f.options())
	if err := s3.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s3.Replay(nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("a replay after Close = %v; want ErrClosed", err)
	}
}

// TestReplayRedactsWithTheLiveRedactor: a key the session learned only after
// an entry was written — a command's arguments and its output, written by a
// session whose environment did not have it — is redacted when the entry is
// replayed, in the call's description and in its result.
func TestReplayRedactsWithTheLiveRedactor(t *testing.T) {
	const later = "sk-learned-later-not-a-secret"
	f := newFixture(t, "http://127.0.0.1:1/v1")
	before := f.options()
	before.Getenv = func(k string) string {
		if k == "OTHER_API_KEY" {
			return ""
		}
		return testEnv[k]
	}
	s, err := Open(before)
	if err != nil {
		t.Fatal(err)
	}
	f.models["test/a"].push(callStep(callParts("b1", "bash", input(t, map[string]any{"command": "echo " + later}))), answerWith("echoed"))
	run(t, s, "say it")
	if raw, err := os.ReadFile(s.store.Path()); err != nil || strings.Count(string(raw), later) < 2 {
		t.Fatalf("the transcript should hold the not-yet-known key in the call and its output (%v)", err)
	}
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	after := resumeOptions(f.options(), id)
	after.Getenv = func(k string) string {
		if k == "OTHER_API_KEY" {
			return later
		}
		return testEnv[k]
	}
	s2 := resumed(t, after)
	var ev events
	if err := s2.Replay(ev.sink); err != nil {
		t.Fatal(err)
	}
	if found := leaks(ev.list(), later); len(found) != 0 {
		t.Fatalf("the replay shows the key at %v", found)
	}
	if fin := of[ToolFinished](ev.list()); len(fin) != 1 || !strings.Contains(fin[0].Result.Content, redact.Marker) {
		t.Fatalf("the command's replayed output: %+v; want the key redacted", fin)
	}
}

// turnField is a user entry's recorded turn, as the file spells it.
var turnField = regexp.MustCompile(`,"turn":\d+`)

// TestReplayInfersTurnsInAnOlderTranscript: a transcript written before
// turns were recorded — no user entry carries one — is read by inference: a
// user entry after a tool entry is a steer, and the others open turns, which
// a resume numbers on from. The turns the resumed session records from then
// on are read as recorded, the older ones still inferred.
func TestReplayInfersTurnsInAnOlderTranscript(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	a := f.models["test/a"]
	s, err := Open(f.options())
	if err != nil {
		t.Fatal(err)
	}
	g := newGate()
	a.push(g.hold(globPart("g1"), finish(fantasy.FinishReasonToolCalls)), answerWith("one done"), answerWith("two done"))
	out := start(context.Background(), s, "one", nil)
	await(t, g.reached, "the first step")
	if err := sendSteer(s, "and this"); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	if got := await(t, out, "the first turn"); got.err != nil {
		t.Fatal(got.err)
	}
	run(t, s, "two")
	id, path := s.ID(), s.store.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(turnField.FindAll(raw, -1)); n != 2 {
		t.Fatalf("the transcript records %d turns; want 2 before they are stripped", n)
	}
	if err := os.WriteFile(path, turnField.ReplaceAll(raw, nil), 0o600); err != nil {
		t.Fatal(err)
	}

	s2 := resumed(t, resumeOptions(f.options(), id))
	var ev events
	if err := s2.Replay(ev.sink); err != nil {
		t.Fatal(err)
	}
	equal(t, "the prompts", of[Prompted](ev.list()), []Prompted{{Text: "one"}, {Text: "and this", Steer: true}, {Text: "two"}})
	a.push(callStep(globPart("g2")), answerWith("three done"))
	var ev3 events
	if _, err := s2.Run(context.Background(), "three", ev3.sink); err != nil {
		t.Fatal(err)
	}
	if st := of[ToolStarted](ev3.list()); len(st) != 1 || st[0].ID != "t3.1.1" {
		t.Fatalf("the resumed turn's calls: %+v; want t3.1.1, after the two inferred turns", st)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	s3 := resumed(t, resumeOptions(f.options(), id))
	var ev4 events
	if err := s3.Replay(ev4.sink); err != nil {
		t.Fatal(err)
	}
	equal(t, "the prompts, again", of[Prompted](ev4.list()),
		[]Prompted{{Text: "one"}, {Text: "and this", Steer: true}, {Text: "two"}, {Text: "three"}})
	a.push(callStep(globPart("g3")), answerWith("four done"))
	var ev5 events
	if _, err := s3.Run(context.Background(), "four", ev5.sink); err != nil {
		t.Fatal(err)
	}
	if st := of[ToolStarted](ev5.list()); len(st) != 1 || st[0].ID != "t4.1.1" {
		t.Fatalf("the next resumed turn's calls: %+v; want t4.1.1", st)
	}
}

// TestReplayedEventsRoundTrip: Prompted and a replayed ToolFinished are plain
// data, as every event is (TestEventsRoundTrip).
func TestReplayedEventsRoundTrip(t *testing.T) {
	for _, ev := range []Event{
		Prompted{Text: "hi", Steer: true},
		ToolFinished{ID: "0a1b2c3d.0", At: testNow(), Replayed: true, Result: tool.Result{Text: "out", Content: "out"}},
	} {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("%T does not encode: %v", ev, err)
		}
		back := reflect.New(reflect.TypeOf(ev))
		if err := json.Unmarshal(b, back.Interface()); err != nil {
			t.Fatalf("%T does not decode: %v", ev, err)
		}
		if got := back.Elem().Interface(); !reflect.DeepEqual(got, ev) {
			t.Errorf("%T round-tripped to\n%#v\nwant\n%#v", ev, got, ev)
		}
	}
}
