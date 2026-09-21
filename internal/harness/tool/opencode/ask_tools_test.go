package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// ask_user_question and exit_plan_mode against a stub asker (plan 023 §3.4,
// §7 A3, A4). What ends a turn, and what a step's later calls get, is the
// harness's and is tested there (internal/harness/asker_test.go).

// stubAsker answers from what the test set, and records what it was shown.
// With block set it waits for its context instead, as an asker whose person
// never answers does.
type stubAsker struct {
	mu        sync.Mutex
	questions [][]tool.Question
	plans     []tool.PlanOffer

	answers tool.Answers
	outcome tool.PlanOutcome
	err     error
	block   bool
}

func (a *stubAsker) AskQuestion(ctx context.Context, qs []tool.Question) (tool.Answers, error) {
	a.mu.Lock()
	a.questions = append(a.questions, qs)
	a.mu.Unlock()
	if a.block {
		<-ctx.Done()
		return tool.Answers{}, nil
	}
	return a.answers, a.err
}

func (a *stubAsker) PresentPlan(ctx context.Context, p tool.PlanOffer) (tool.PlanOutcome, error) {
	a.mu.Lock()
	a.plans = append(a.plans, p)
	a.mu.Unlock()
	if a.block {
		<-ctx.Done()
		return tool.PlanUnanswered, nil
	}
	return a.outcome, a.err
}

// askFixture is a fixture over the whole profile with asker wired in and a
// plan file path under its home. closing, when not nil, is Env.Closing.
func askFixture(t *testing.T, asker tool.Asker, closing <-chan struct{}) (*fixture, string) {
	t.Helper()
	env := tool.Env{
		Workspace: t.TempDir(),
		Home:      t.TempDir(),
		Redactor:  redact.New(keyA),
		Environ:   tool.ChildEnviron(os.Environ(), nil),
		Locks:     &tool.PathLocks{},
		Closing:   closing,
		Asker:     asker,
	}
	p, err := Profile()
	if err != nil {
		t.Fatal(err)
	}
	d, err := tool.NewDispatcher(tool.Options{Tools: p.Tools, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(env.Home, "sessions", "s.plan.md")
	must(t, os.MkdirAll(filepath.Dir(plan), 0o700))
	d.SetPlanPath(plan)
	return &fixture{env: env, d: d}, plan
}

func oneQuestion() map[string]any {
	return map[string]any{"questions": []any{map[string]any{
		"question": "Which database?",
		"header":   "DB",
		"options": []any{
			map[string]any{"label": "Postgres (Recommended)", "description": "Relational"},
			map[string]any{"label": "SQLite"},
		},
	}}}
}

func TestAskUserQuestionAnswers(t *testing.T) {
	a := &stubAsker{answers: tool.Answers{Answered: true, Picked: [][]int{{1, 0, 7}}}}
	f, _ := askFixture(t, a, nil)
	req, res := f.call(t, "ask_user_question", oneQuestion())
	if req.Kind != tool.KindAsk || !req.ReadOnly || req.Title != "Which database?" {
		t.Fatalf("request = %+v", req)
	}
	// opencode's answer text; an index the question does not offer is dropped.
	want := `User has answered your questions: "Which database?"="SQLite, Postgres (Recommended)". You can now continue with the user's answers in mind.`
	if res.IsError || res.Text != want {
		t.Fatalf("result = %+v\nwant %q", res, want)
	}
	if len(a.questions) != 1 || a.questions[0][0].Header != "DB" || a.questions[0][0].Options[0].Description != "Relational" {
		t.Fatalf("the asker saw %+v", a.questions)
	}
}

// A question nobody answered is a result, not a failure: the model carries on.
// The same ending under a done context or a closing session is the call cut
// short (plan 023 §3.5).
func TestAskUserQuestionUnanswered(t *testing.T) {
	t.Run("declined", func(t *testing.T) {
		f, _ := askFixture(t, &stubAsker{}, nil)
		if _, res := f.call(t, "ask_user_question", oneQuestion()); res.IsError || res.Text != UnansweredText {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("no asker", func(t *testing.T) {
		f, _ := askFixture(t, nil, nil)
		if _, res := f.call(t, "ask_user_question", oneQuestion()); res.IsError || res.Text != UnansweredText {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("answered one of two", func(t *testing.T) {
		in := oneQuestion()
		in["questions"] = append(in["questions"].([]any), map[string]any{
			"question": "And the cache?", "options": []any{map[string]any{"label": "Redis"}}})
		f, _ := askFixture(t, &stubAsker{answers: tool.Answers{Answered: true, Picked: [][]int{{0}}}}, nil)
		_, res := f.call(t, "ask_user_question", in)
		if !strings.Contains(res.Text, `"And the cache?"="Unanswered"`) {
			t.Fatalf("result = %q", res.Text)
		}
	})
	t.Run("cancelled while blocked", func(t *testing.T) {
		f, _ := askFixture(t, &stubAsker{block: true}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(10*time.Millisecond, cancel)
		_, res := f.callCtx(t, ctx, "ask_user_question", oneQuestion())
		if !res.IsError || res.Class != tool.ClassAborted {
			t.Fatalf("result = %+v, want aborted", res)
		}
	})
	t.Run("closing", func(t *testing.T) {
		closing := make(chan struct{})
		close(closing)
		// The registry resolves the ask as closing before the context is
		// cancelled: the asker says nobody answered, and the context is live.
		f, _ := askFixture(t, &stubAsker{}, closing)
		_, res := f.call(t, "ask_user_question", oneQuestion())
		if !res.IsError || res.Class != tool.ClassAborted {
			t.Fatalf("result = %+v, want aborted", res)
		}
	})
	t.Run("the asker failed", func(t *testing.T) {
		f, _ := askFixture(t, &stubAsker{err: errors.New("the registry is gone")}, nil)
		_, res := f.call(t, "ask_user_question", oneQuestion())
		if !res.IsError || res.Class != tool.ClassToolError || !strings.Contains(res.Text, "the registry is gone") {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestAskUserQuestionValidates(t *testing.T) {
	opt := map[string]any{"label": "a"}
	many := func(n int, v any) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = v
		}
		return out
	}
	distinct := make([]any, maxQuestions+1)
	for i := range distinct {
		distinct[i] = map[string]any{"question": fmt.Sprintf("q%d?", i), "options": []any{opt}}
	}
	cases := map[string]any{
		"no questions":       map[string]any{},
		"not an array":       map[string]any{"questions": "which?"},
		"empty":              map[string]any{"questions": []any{}},
		"too many":           map[string]any{"questions": distinct},
		"not an object":      map[string]any{"questions": []any{"which?"}},
		"no question text":   map[string]any{"questions": []any{map[string]any{"options": []any{opt}}}},
		"blank question":     map[string]any{"questions": []any{map[string]any{"question": " ", "options": []any{opt}}}},
		"no options":         map[string]any{"questions": []any{map[string]any{"question": "q?"}}},
		"empty options":      map[string]any{"questions": []any{map[string]any{"question": "q?", "options": []any{}}}},
		"too many options":   map[string]any{"questions": []any{map[string]any{"question": "q?", "options": many(maxOptions+1, opt)}}},
		"option not object":  map[string]any{"questions": []any{map[string]any{"question": "q?", "options": []any{"a"}}}},
		"blank label":        map[string]any{"questions": []any{map[string]any{"question": "q?", "options": []any{map[string]any{"label": ""}}}}},
		"multi_select typed": map[string]any{"questions": []any{map[string]any{"question": "q?", "options": []any{opt}, "multi_select": "yes"}}},
		"duplicate label": map[string]any{"questions": []any{map[string]any{"question": "q?", "options": []any{
			map[string]any{"label": "Use this", "description": "SQLite"}, map[string]any{"label": "Use this", "description": "Postgres"}}}}},
		"duplicate question": map[string]any{"questions": many(2, map[string]any{"question": "q?", "options": []any{opt}})},
	}
	a := &stubAsker{}
	f, _ := askFixture(t, a, nil)
	for name, in := range cases {
		if _, res := f.call(t, "ask_user_question", in); !res.IsError || res.Class != tool.ClassInvalidInput {
			t.Errorf("%s: result = %+v, want invalid_input", name, res)
		}
	}
	// Bounded before it is decoded, like todo_write.
	big := map[string]any{"questions": []any{map[string]any{"question": strings.Repeat("q", maxAskInputBytes), "options": []any{opt}}}}
	if _, res := f.call(t, "ask_user_question", big); res.Class != tool.ClassInvalidInput {
		t.Errorf("oversized: result = %+v", res)
	}
	if len(a.questions) != 0 {
		t.Fatalf("a refused call reached the asker: %+v", a.questions)
	}
	// header:null and description:null are omissions, not wrong types.
	ok := map[string]any{"questions": []any{map[string]any{"question": "q?", "header": nil,
		"options": []any{map[string]any{"label": "a", "description": nil}}}}}
	if _, res := f.call(t, "ask_user_question", ok); res.IsError {
		t.Fatalf("nulls: result = %+v", res)
	}
}

// Everything shown to the person is redacted first: the asker's side of the
// seam has no redactor, and the answer still reaches the model in the model's
// own words, because it comes back by index.
func TestAskToolsRedactWhatIsShown(t *testing.T) {
	a := &stubAsker{answers: tool.Answers{Answered: true, Picked: [][]int{{0}}}, outcome: tool.PlanRejected}
	f, plan := askFixture(t, a, nil)
	in := map[string]any{"questions": []any{map[string]any{
		"question": "Use " + keyA + "?", "header": keyA,
		"options": []any{map[string]any{"label": "yes " + keyA, "description": keyA}},
	}}}
	_, res := f.call(t, "ask_user_question", in)
	q := a.questions[0][0]
	for _, shown := range []string{q.Question, q.Header, q.Options[0].Label, q.Options[0].Description} {
		if strings.Contains(shown, keyA) || !strings.Contains(shown, redact.Marker) {
			t.Fatalf("the asker was shown %q", shown)
		}
	}
	if strings.Contains(res.Text, keyA) {
		t.Fatalf("the result holds the key: %q", res.Text)
	}

	must(t, os.WriteFile(plan, []byte("# Plan\nrotate "+keyA+"\n"), 0o600))
	f.call(t, "exit_plan_mode", map[string]any{})
	if got := a.plans[0]; strings.Contains(got.Text, keyA) || !strings.Contains(got.Text, redact.Marker) || got.Path != plan {
		t.Fatalf("the asker was shown %+v", got)
	}
}

func TestExitPlanModeOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		outcome tool.PlanOutcome
		want    string
	}{
		"approved":   {tool.PlanApproved, PlanApprovedText},
		"rejected":   {tool.PlanRejected, PlanRejectedText},
		"unanswered": {tool.PlanUnanswered, PlanUnansweredText},
	} {
		t.Run(name, func(t *testing.T) {
			a := &stubAsker{outcome: tc.outcome}
			f, plan := askFixture(t, a, nil)
			must(t, os.WriteFile(plan, []byte(bom+"# The plan\n\n1. do it\n"), 0o600))
			// Claude Code's ExitPlanMode takes the plan as an argument; one
			// sent here is ignored, and the file is what is presented.
			req, res := f.call(t, "exit_plan_mode", map[string]any{"plan": "not this"})
			if req.Kind != tool.KindAsk || !req.ReadOnly {
				t.Fatalf("request = %+v", req)
			}
			if res.IsError || res.Text != tc.want {
				t.Fatalf("result = %+v, want %q", res, tc.want)
			}
			if len(a.plans) != 1 || a.plans[0].Text != "# The plan\n\n1. do it\n" || a.plans[0].Path != plan {
				t.Fatalf("the asker was shown %+v", a.plans)
			}
		})
	}
	t.Run("no asker", func(t *testing.T) {
		f, plan := askFixture(t, nil, nil)
		must(t, os.WriteFile(plan, []byte("# plan"), 0o600))
		if _, res := f.call(t, "exit_plan_mode", map[string]any{}); res.IsError || res.Text != PlanUnansweredText {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("cancelled while blocked", func(t *testing.T) {
		f, plan := askFixture(t, &stubAsker{block: true}, nil)
		must(t, os.WriteFile(plan, []byte("# plan"), 0o600))
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(10*time.Millisecond, cancel)
		if _, res := f.callCtx(t, ctx, "exit_plan_mode", map[string]any{}); !res.IsError || res.Class != tool.ClassAborted {
			t.Fatalf("result = %+v, want aborted", res)
		}
	})
	t.Run("no plan file in this session", func(t *testing.T) {
		f, _ := askFixture(t, &stubAsker{}, nil)
		f.d.SetPlanPath("")
		if _, res := f.call(t, "exit_plan_mode", map[string]any{}); !res.IsError || res.Class != tool.ClassToolError {
			t.Fatalf("result = %+v", res)
		}
	})
}

// An empty or missing plan asks nobody; and nothing but a regular file of a
// plan's size is read — a FIFO there would otherwise hang the tool goroutine
// the turn joins (plan 023 §3.4, A4).
func TestExitPlanModeReadsOnlyAPlan(t *testing.T) {
	type setup func(t *testing.T, plan string)
	empty := func(text string) setup {
		return func(t *testing.T, plan string) { must(t, os.WriteFile(plan, []byte(text), 0o600)) }
	}
	for name, tc := range map[string]struct {
		setup setup
		class tool.ErrorClass // "" for the empty-plan text
	}{
		"missing":    {func(*testing.T, string) {}, ""},
		"empty":      {empty(""), ""},
		"whitespace": {empty(" \n\t\n"), ""},
		"a BOM only": {empty(bom), ""},
		"a fifo": {func(t *testing.T, plan string) {
			must(t, syscall.Mkfifo(plan, 0o600))
		}, tool.ClassToolError},
		"a directory": {func(t *testing.T, plan string) { must(t, os.Mkdir(plan, 0o700)) }, tool.ClassToolError},
		"a symlink to a device": {func(t *testing.T, plan string) {
			must(t, os.Symlink("/dev/zero", plan))
		}, tool.ClassToolError},
		"oversized": {func(t *testing.T, plan string) {
			must(t, os.WriteFile(plan, []byte(strings.Repeat("x", maxPlanBytes+1)), 0o600))
		}, tool.ClassOutputLimit},
	} {
		t.Run(name, func(t *testing.T) {
			a := &stubAsker{outcome: tool.PlanApproved}
			f, plan := askFixture(t, a, nil)
			tc.setup(t, plan)
			done := make(chan tool.Result, 1)
			go func() {
				_, res := f.call(t, "exit_plan_mode", map[string]any{})
				done <- res
			}()
			var res tool.Result
			select {
			case res = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("exit_plan_mode did not return")
			}
			if len(a.plans) != 0 {
				t.Fatalf("the asker was shown %+v", a.plans)
			}
			if tc.class == "" {
				if res.IsError || res.Text != fmt.Sprintf(EmptyPlanText, plan) {
					t.Fatalf("result = %+v, want the empty-plan text", res)
				}
				return
			}
			if !res.IsError || res.Class != tc.class {
				t.Fatalf("result = %+v, want class %s", res, tc.class)
			}
		})
	}
	t.Run("at the bound", func(t *testing.T) {
		a := &stubAsker{outcome: tool.PlanRejected}
		f, plan := askFixture(t, a, nil)
		must(t, os.WriteFile(plan, []byte(strings.Repeat("x", maxPlanBytes)), 0o600))
		if _, res := f.call(t, "exit_plan_mode", map[string]any{}); res.IsError || len(a.plans) != 1 {
			t.Fatalf("result = %+v after %d asks", res, len(a.plans))
		}
	})
}

// The plan file's path is craze's own; what is at it may not be. Made an alias
// of the harness's key file it is refused, as every file tool refuses that
// file, and never presented — with a key the session's redactor has never
// seen, which is the case redaction cannot cover.
func TestExitPlanModeRefusesTheCredentialsFile(t *testing.T) {
	const rotated = "sk-rotated-on-disk-since-the-table-loaded"
	for name, alias := range map[string]func(cred, plan string) error{
		"a symlink":   os.Symlink,
		"a hard link": os.Link,
	} {
		t.Run(name, func(t *testing.T) {
			a := &stubAsker{outcome: tool.PlanApproved}
			f, plan := askFixture(t, a, nil)
			cred := filepath.Join(f.env.Home, CredentialsFile)
			must(t, os.WriteFile(cred, []byte("api_key = \""+rotated+"\"\n"), 0o600))
			must(t, alias(cred, plan))
			_, res := f.call(t, "exit_plan_mode", map[string]any{})
			if !res.IsError || res.Text != credentialsText {
				t.Fatalf("result = %+v, want the credentials refusal", res)
			}
			if len(a.plans) != 0 || strings.Contains(res.Text, rotated) {
				t.Fatalf("the key file was presented: %+v", a.plans)
			}
		})
	}
}

// The plan is read under craze's path lock, which a write still landing
// holds: a reader started behind it waits, as the write tool's own call does
// (TestWriteTakesThePathLock), and a cancel is what gets it back. The asker
// here answers at once, so a reader that took no lock would have returned the
// person's answer instead of waiting.
func TestExitPlanModeWaitsForThePathLock(t *testing.T) {
	a := &stubAsker{outcome: tool.PlanRejected}
	f, plan := askFixture(t, a, nil)
	must(t, os.WriteFile(plan, []byte("# plan"), 0o600))
	real, err := realPath(plan)
	must(t, err)
	unlock, err := f.env.Locks.Lock(context.Background(), real)
	must(t, err)
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan tool.Result, 1)
	go func() {
		_, res := f.callCtx(t, ctx, "exit_plan_mode", map[string]any{})
		done <- res
	}()
	select {
	case res := <-done:
		t.Fatalf("exit_plan_mode did not wait for the lock: %+v", res)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case res := <-done:
		if !res.IsError || res.Class != tool.ClassAborted {
			t.Fatalf("result = %+v, want aborted", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled exit_plan_mode kept waiting")
	}
	if len(a.plans) != 0 {
		t.Fatalf("the plan was presented from behind a held lock: %+v", a.plans)
	}
}

// And the lock is given back before the person is asked: nobody waits on a
// card to write a file.
func TestExitPlanModeAsksWithoutTheLock(t *testing.T) {
	a := &stubAsker{block: true}
	f, plan := askFixture(t, a, nil)
	must(t, os.WriteFile(plan, []byte("# plan"), 0o600))
	real, err := realPath(plan)
	must(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.callCtx(t, ctx, "exit_plan_mode", map[string]any{})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		a.mu.Lock()
		asked := len(a.plans)
		a.mu.Unlock()
		if asked == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the plan was never presented")
		}
		time.Sleep(time.Millisecond)
	}
	lockCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	unlock, err := f.env.Locks.Lock(lockCtx, real)
	if err != nil {
		t.Fatalf("the path lock is held while the person is asked: %v", err)
	}
	unlock()
	cancel()
	<-done
}
