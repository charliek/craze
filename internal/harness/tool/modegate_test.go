package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordingGate is an inner gate that remembers what reached it and answers
// with whatever the test set: "allowed calls still reach inner" is a claim
// about this, not about the absence of a denial.
type recordingGate struct {
	mu   sync.Mutex
	seen []Request
	dec  Decision
	err  error
}

func (g *recordingGate) Check(_ context.Context, req Request) (Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen = append(g.seen, req)
	if g.dec == nil && g.err == nil {
		return Allow{}, nil
	}
	return g.dec, g.err
}

func (g *recordingGate) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.seen)
}

// planFixture is a harness home with a session's plan file in it, plus the
// shapes another path can take: a symlink to the plan, a link that dangles
// onto it, a file that is not the plan, and one that does not exist.
type planFixture struct {
	plan, link, dangling, other, missing string
}

func newPlanFixture(t *testing.T) planFixture {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "--w--")
	work := filepath.Join(home, "work")
	for _, d := range []string{dir, work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f := planFixture{
		plan:     filepath.Join(dir, "20260921T120000Z_s.plan.md"),
		link:     filepath.Join(work, "plan-link.md"),
		dangling: filepath.Join(work, "dangling.md"),
		other:    filepath.Join(work, "a.txt"),
		missing:  filepath.Join(work, "new.txt"),
	}
	for _, p := range []string{f.plan, f.other} {
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(f.plan, f.link); err != nil {
		t.Fatal(err)
	}
	// A link onto a name that is not there: the write would create the
	// target, so that is what the gate must judge.
	if err := os.Symlink(f.missing, f.dangling); err != nil {
		t.Fatal(err)
	}
	return f
}

// target is a path as a tool's Prepare resolves it for Request.Targets.
func target(t *testing.T, path string) []string {
	t.Helper()
	real, err := RealPath(path)
	if err != nil {
		t.Fatalf("resolving %s: %v", path, err)
	}
	return []string{real}
}

// TestModeGate is A1 at the gate itself: the three modes against the kinds and
// the target shapes that decide a call. Plan mode admits an edit whose targets
// are all the plan file, spelled outright or reached through a symlink, and
// refuses another file, a file that is not there, a dangling link, an
// unresolved relative spelling, no target at all, and the plan file among
// others; reads, searches and commands go inward in plan mode, nothing that is
// not read-only does in ask mode, and exit_plan_mode is refused by name
// wherever there is no plan to present. Everything not refused reaches the
// inner gate, which is what a later evaluator will be.
//
// The shapes it does not judge are judged next door: a case-equivalent
// spelling and a hard link in TestModeGateCaseEquivalentPlanPaths, and what a
// tool's own Prepare resolves — a relative spelling, a directory symlink, a
// path through "..", one that will not resolve, and an edit tool that names no
// target — through the dispatcher in TestModeGateThroughTheDispatcher and,
// with the production write tool, in opencode's TestPlanModeThroughTheGate.
func TestModeGate(t *testing.T) {
	f := newPlanFixture(t)
	rejected := planEditRejected(mustReal(t, f.plan))

	cases := []struct {
		name string
		mode string
		req  Request
		deny string // "" = allowed, and the inner gate saw it
	}{
		{name: "agent lets an edit through", mode: ModeAgent,
			req: Request{Tool: "write", Kind: KindEdit, Targets: target(t, f.other)}},
		{name: "agent lets a command through", mode: ModeAgent,
			req: Request{Tool: "bash", Kind: KindExecute}},
		{name: "agent refuses exit_plan_mode by name", mode: ModeAgent,
			req: Request{Tool: ExitPlanModeTool, Kind: KindRead, ReadOnly: true}, deny: planDisabledText},

		{name: "plan allows the plan file", mode: ModePlan,
			req: Request{Tool: "edit", Kind: KindEdit, Targets: target(t, f.plan)}},
		{name: "plan allows a symlink to the plan file", mode: ModePlan,
			req: Request{Tool: "write", Kind: KindEdit, Targets: target(t, f.link)}},
		{name: "plan refuses another file", mode: ModePlan,
			req: Request{Tool: "write", Kind: KindEdit, Targets: target(t, f.other)}, deny: rejected},
		{name: "plan refuses a file that does not exist", mode: ModePlan,
			req: Request{Tool: "write", Kind: KindEdit, Targets: target(t, f.missing)}, deny: rejected},
		{name: "plan refuses a dangling symlink", mode: ModePlan,
			req: Request{Tool: "write", Kind: KindEdit, Targets: target(t, f.dangling)}, deny: rejected},
		{name: "plan refuses an unresolved relative spelling", mode: ModePlan,
			req: Request{Tool: "write", Kind: KindEdit, Targets: []string{"a.txt"}}, deny: rejected},
		{name: "plan refuses an edit with no target at all", mode: ModePlan,
			req: Request{Tool: "write", Kind: KindEdit}, deny: rejected},
		{name: "plan refuses the plan file among others", mode: ModePlan,
			req: Request{Tool: "write", Kind: KindEdit, Targets: append(target(t, f.plan), mustReal(t, f.other))}, deny: rejected},
		{name: "plan allows a read of the plan file", mode: ModePlan,
			req: Request{Tool: "read", Kind: KindRead, ReadOnly: true, Paths: []string{f.plan}}},
		{name: "plan allows a read of anything else", mode: ModePlan,
			req: Request{Tool: "read", Kind: KindRead, ReadOnly: true, Paths: []string{f.other}}},
		{name: "plan allows a search", mode: ModePlan,
			req: Request{Tool: "grep", Kind: KindSearch, ReadOnly: true}},
		{name: "plan allows a command", mode: ModePlan, // owner decision 3
			req: Request{Tool: "bash", Kind: KindExecute, Command: "ls"}},
		{name: "plan allows exit_plan_mode", mode: ModePlan,
			req: Request{Tool: ExitPlanModeTool, Kind: KindRead, ReadOnly: true}},

		{name: "ask refuses an edit of the plan file too", mode: ModeAsk,
			req: Request{Tool: "write", Kind: KindEdit, Targets: target(t, f.plan)}, deny: askRejectedText},
		{name: "ask refuses a command", mode: ModeAsk,
			req: Request{Tool: "bash", Kind: KindExecute, Command: "ls"}, deny: askRejectedText},
		{name: "ask refuses exit_plan_mode by name, not for being a write", mode: ModeAsk,
			req: Request{Tool: ExitPlanModeTool, Kind: KindRead, ReadOnly: true}, deny: planDisabledText},
		{name: "ask allows a read", mode: ModeAsk,
			req: Request{Tool: "read", Kind: KindRead, ReadOnly: true, Paths: []string{f.other}}},
		{name: "ask allows a question", mode: ModeAsk,
			req: Request{Tool: "ask_user_question", Kind: KindRead, ReadOnly: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &recordingGate{}
			g := NewModeGate(tc.mode, inner)
			g.SetPlanPath(f.plan)
			dec, err := g.Check(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if tc.deny == "" {
				if _, ok := dec.(Allow); !ok {
					t.Fatalf("decision = %#v, want Allow", dec)
				}
				if inner.calls() != 1 {
					t.Fatal("the call did not reach the inner gate")
				}
				return
			}
			d, ok := dec.(Deny)
			if !ok || d.Reason != tc.deny {
				t.Fatalf("decision = %#v, want Deny{%q}", dec, tc.deny)
			}
			if inner.calls() != 0 {
				t.Fatal("a refused call reached the inner gate")
			}
		})
	}
}

// mustReal is RealPath or a failed test.
func mustReal(t *testing.T, path string) string {
	t.Helper()
	real, err := RealPath(path)
	if err != nil {
		t.Fatalf("resolving %s: %v", path, err)
	}
	return real
}

// TestModeGateCaseEquivalentPlanPaths: a target that is the plan file under a
// spelling that differs only in case is the plan file, where the file system
// says so — on macOS's APFS a resolved path keeps the spelling it was given,
// so string equality alone would refuse an edit of the plan file itself. A
// hard link to it at another name is not the plan file anywhere: os.SameFile
// is true of it, which is why the comparison asks EqualFold as well.
func TestModeGateCaseEquivalentPlanPaths(t *testing.T) {
	f := newPlanFixture(t)
	g := NewModeGate(ModePlan, nil) // nil inner is AllowAll
	g.SetPlanPath(f.plan)
	edit := func(path string) Decision {
		t.Helper()
		dec, err := g.Check(context.Background(), Request{Tool: "write", Kind: KindEdit, Targets: target(t, path)})
		if err != nil {
			t.Fatalf("Check(%s): %v", path, err)
		}
		return dec
	}

	// One inode, two names: refused, whatever the file system.
	hard := filepath.Join(filepath.Dir(f.other), "hard-link.md")
	if err := os.Link(f.plan, hard); err != nil {
		t.Skipf("this file system has no hard links, so the negative half cannot run: %v", err)
	}
	if d, ok := edit(hard).(Deny); !ok || !strings.Contains(d.Reason, "the only editable file") {
		t.Fatalf("a hard link to the plan file = %#v, want the plan-mode refusal", edit(hard))
	}

	// The same file, spelled in another case.
	dir := filepath.Dir(f.plan)
	upper := filepath.Join(dir, strings.ToUpper(filepath.Base(f.plan)))
	if !caseInsensitive(t, dir) {
		t.Skip("this file system is case-sensitive: the upper-cased spelling is another file, which the gate already refuses")
	}
	if _, ok := edit(upper).(Allow); !ok {
		t.Fatalf("the plan file spelled %q = %#v, want Allow", filepath.Base(upper), edit(upper))
	}
}

// caseInsensitive reports whether dir's file system has one file under many
// spellings: it writes a name and stats the upper-cased one.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	_, err := os.Stat(filepath.Join(dir, "CASE-PROBE"))
	return err == nil
}

// TestModeGateThroughTheDispatcher is the same gate where a call really meets
// it: Prepare resolves the target the way the file tools do (RealPath over the
// workspace-resolved path, opencode/file.go:89-99), the dispatcher hands the
// gate that copy, and Run either refuses or runs the tool. It covers the
// spellings a model actually sends — relative, through a directory symlink,
// through ".." — a path that will not resolve at all, and an edit-kind tool
// that names Paths and no Targets, which nothing can judge and plan mode
// therefore refuses.
func TestModeGateThroughTheDispatcher(t *testing.T) {
	env := testEnv(t)
	plan := filepath.Join(env.Home, "20260921T120000Z_s.plan.md")
	if err := os.WriteFile(plan, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.Workspace, "a.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(env.Workspace, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory symlink onto the harness home, and a pair of links that
	// point at each other: RealPath resolves the first and cannot resolve the
	// second, which then falls back to the path as spelled (plan 023 §3.1,
	// amendment X4).
	if err := os.Symlink(env.Home, filepath.Join(env.Workspace, "home-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(env.Workspace, "loop-b"), filepath.Join(env.Workspace, "loop-a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(env.Workspace, "loop-a"), filepath.Join(env.Workspace, "loop-b")); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(env.Workspace, plan)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, path string
		pathsOnly  bool
		allowed    bool
	}{
		{name: "the plan file, absolute", path: plan, allowed: true},
		{name: "the plan file, spelled relative to the workspace", path: rel, allowed: true},
		{name: "the plan file through a directory symlink", path: filepath.Join("home-link", filepath.Base(plan)), allowed: true},
		{name: "the plan file through ..", path: filepath.Join("sub", "..", rel), allowed: true},
		{name: "a file in the workspace", path: "a.txt"},
		{name: "a path that will not resolve", path: "loop-a"},
		{name: "an edit tool that names no target", path: plan, pathsOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := newFake("write", nil)
			writer.spec.Kind, writer.spec.ReadOnly = KindEdit, false
			writer.req = func(in fakeInput, env Env) Request {
				abs := env.Resolve(in.Path)
				if tc.pathsOnly {
					// A tool that says where it will write only in the field
					// the event text carries, which the gate may not judge by.
					return Request{Title: in.Text, Paths: []string{abs}}
				}
				return Request{Title: in.Text, Paths: []string{abs}, Targets: []string{targetOf(abs)}}
			}
			g := NewModeGate(ModePlan, nil)
			g.SetPlanPath(plan)
			d := newDispatcher(t, env, g, writer)

			if _, _, ok := d.Prepare(Call{ID: "t1.1.1", Tool: "write", Input: input(t, fakeInput{Text: "hi", Path: tc.path})}); !ok {
				t.Fatal("Prepare refused the call")
			}
			res := d.Run(context.Background(), "t1.1.1", nil)
			switch {
			case tc.allowed && (res.IsError || writer.runs.Load() != 1):
				t.Fatalf("Run = %+v after %d runs, want the edit to run", res, writer.runs.Load())
			case !tc.allowed && (!res.IsError || res.Class != ClassDenied || !strings.Contains(res.Text, "the only editable file")):
				t.Fatalf("Run = %+v, want the plan-mode refusal", res)
			case !tc.allowed && writer.runs.Load() != 0:
				t.Fatal("a refused call ran anyway")
			}
		})
	}
}

// targetOf is what an edit-kind tool puts in Request.Targets: the file the
// call will really land on, or the path as it was spelled when nothing can
// resolve it (opencode/file.go:89-99).
func targetOf(abs string) string {
	if real, err := RealPath(abs); err == nil {
		return real
	}
	return abs
}

// The inner gate keeps the last word on everything the mode allows: its
// refusal and its error both come back unchanged, so wrapping it adds rules
// and takes none away.
func TestModeGateKeepsTheInnerDecision(t *testing.T) {
	f := newPlanFixture(t)
	inner := &recordingGate{dec: Deny{Reason: "no writes today"}}
	g := NewModeGate(ModePlan, inner)
	g.SetPlanPath(f.plan)

	dec, err := g.Check(context.Background(), Request{Tool: "write", Kind: KindEdit, Targets: target(t, f.plan)})
	if d, ok := dec.(Deny); err != nil || !ok || d.Reason != "no writes today" {
		t.Fatalf("Check = %#v, %v; want the inner gate's refusal", dec, err)
	}

	inner.dec, inner.err = nil, errors.New("the evaluator is down")
	if _, err := g.Check(context.Background(), Request{Tool: "read", Kind: KindRead, ReadOnly: true}); err == nil {
		t.Fatal("the inner gate's error did not come back")
	}
}

// A gate whose plan path was never set refuses every edit in plan mode: a
// mode that cannot say which file is the plan cannot let one through.
func TestModeGateWithNoPlanPathRefusesEveryEdit(t *testing.T) {
	f := newPlanFixture(t)
	g := NewModeGate(ModePlan, nil) // nil inner is AllowAll
	dec, _ := g.Check(context.Background(), Request{Tool: "write", Kind: KindEdit, Targets: target(t, f.plan)})
	if d, ok := dec.(Deny); !ok || !strings.Contains(d.Reason, "the only editable file") {
		t.Fatalf("Check = %#v, want the plan-mode refusal", dec)
	}
	if got := g.PlanPath(); got != "" {
		t.Fatalf("PlanPath = %q, want none", got)
	}
}

// TestModeGateSwitchesBetweenPrepareAndRun pins the boundary a mode switch is
// effective at (plan 023 §3.1): the mode is read when Run reaches the gate,
// so a call prepared in agent mode and run after SetMode("plan") is judged
// under plan. The control is the same call with no switch.
func TestModeGateSwitchesBetweenPrepareAndRun(t *testing.T) {
	for _, switched := range []bool{true, false} {
		name := "agent throughout"
		if switched {
			name = "plan mode entered after Prepare"
		}
		t.Run(name, func(t *testing.T) {
			env := testEnv(t)
			writer := newFake("write", nil)
			writer.spec.Kind, writer.spec.ReadOnly = KindEdit, false
			writer.req = func(in fakeInput, env Env) Request {
				return Request{Title: in.Text, Targets: []string{mustReal(t, env.Resolve(in.Path))}}
			}
			g := NewModeGate(ModeAgent, nil)
			g.SetPlanPath(filepath.Join(env.Home, "plan.md"))
			d := newDispatcher(t, env, g, writer)

			_, _, ok := d.Prepare(Call{ID: "t1.1.1", Tool: "write", Input: input(t, fakeInput{Text: "hi", Path: "a.txt"})})
			if !ok {
				t.Fatal("Prepare refused the call")
			}
			if switched {
				g.SetMode(ModePlan)
			}
			res := d.Run(context.Background(), "t1.1.1", nil)
			switch {
			case switched && (!res.IsError || res.Class != ClassDenied || !strings.Contains(res.Text, "the only editable file")):
				t.Fatalf("Run after the switch = %+v, want the plan-mode refusal", res)
			case switched && writer.runs.Load() != 0:
				t.Fatal("the tool ran under the mode it was prepared in")
			case !switched && (res.IsError || writer.runs.Load() != 1):
				t.Fatalf("control: Run = %+v after %d runs", res, writer.runs.Load())
			}
		})
	}
}
