package tui

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

var updateGoldens = flag.Bool("update", false, "rewrite internal/tui/testdata/*.golden")

func TestParseFrameScriptTokens(t *testing.T) {
	toks, err := parseFrameScript("ab<enter><ctrl-o><shift-tab><alt-enter><lt><space>")
	if err != nil {
		t.Fatal(err)
	}
	want := []tea.KeyMsg{
		runeKey('a'),
		runeKey('b'),
		{Type: tea.KeyEnter},
		{Type: tea.KeyCtrlO},
		{Type: tea.KeyShiftTab},
		{Type: tea.KeyEnter, Alt: true},
		runeKey('<'),
		runeKey(' '),
	}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens, want %d", len(toks), len(want))
	}
	for i, tok := range toks {
		if tok.kind != tokKey {
			t.Fatalf("token %d kind %v", i, tok.kind)
		}
		if tok.key.String() != want[i].String() || tok.key.Type != want[i].Type {
			t.Fatalf("token %d = %q (%v), want %q (%v)", i, tok.key.String(), tok.key.Type, want[i].String(), want[i].Type)
		}
	}
}

func TestParseFrameScriptNonKeyTokens(t *testing.T) {
	toks, err := parseFrameScript("<wheel-up><wheel-down><click:10,5><resize:120,40><sleep:5ms>" +
		"<wait:idle><wait:working><wait:card><wait:text:TASKS n/n><wait:gone:Thinking>")
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 10 {
		t.Fatalf("got %d tokens", len(toks))
	}
	if toks[0].mouse.Button != tea.MouseButtonWheelUp || toks[1].mouse.Button != tea.MouseButtonWheelDown {
		t.Fatalf("wheel tokens %+v %+v", toks[0].mouse, toks[1].mouse)
	}
	if toks[2].mouse.X != 10 || toks[2].mouse.Y != 5 || toks[2].mouse.Button != tea.MouseButtonLeft {
		t.Fatalf("click %+v", toks[2].mouse)
	}
	if toks[3].size.Width != 120 || toks[3].size.Height != 40 {
		t.Fatalf("resize %+v", toks[3].size)
	}
	if toks[4].dur != 5*time.Millisecond {
		t.Fatalf("sleep %v", toks[4].dur)
	}
	wantWaits := []waitSpec{
		{kind: "idle"},
		{kind: "working"},
		{kind: "card"},
		{kind: "text", needle: "TASKS n/n"},
		{kind: "gone", needle: "Thinking"},
	}
	for i, want := range wantWaits {
		got := toks[5+i].wait
		if got != want {
			t.Fatalf("wait %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestParseFrameScriptRejects(t *testing.T) {
	for _, script := range []string{
		"<nope>",
		"<ctrl-1>",
		"<ctrl-aa>",
		"<enter",
		"<sleep:soon>",
		"<click:10>",
		"<resize:0,10>",
		"<wait:done>",
		"<wait:text:>",
	} {
		if _, err := parseFrameScript(script); err == nil {
			t.Fatalf("%q parsed, want an error", script)
		} else {
			var se *ScriptError
			if !errors.As(err, &se) {
				t.Fatalf("%q: got %T, want *ScriptError", script, err)
			}
		}
	}
}

func TestWaitSpecMatch(t *testing.T) {
	idle := frameState{started: true, status: statusIdle, plain: "craze idle"}
	if !(waitSpec{kind: "idle"}).match(idle) {
		t.Fatal("idle should match a started idle frame")
	}
	if (waitSpec{kind: "idle"}).match(frameState{status: statusIdle}) {
		t.Fatal("idle must not match before Start returns")
	}
	if (waitSpec{kind: "idle"}).match(frameState{started: true, status: statusIdle, card: true}) {
		t.Fatal("idle must not match while a card is up")
	}
	if !(waitSpec{kind: "working"}).match(frameState{status: statusWorking}) {
		t.Fatal("working")
	}
	if !(waitSpec{kind: "card"}).match(frameState{card: true}) {
		t.Fatal("card")
	}
	if !(waitSpec{kind: "text", needle: "idle"}).match(idle) {
		t.Fatal("text")
	}
	if !(waitSpec{kind: "gone", needle: "working"}).match(idle) {
		t.Fatal("gone")
	}
}

func frameWorkspace(t *testing.T) string {
	t.Helper()
	// A fixed basename keeps the status line (workspace basename) golden-stable.
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

func runStubFrame(t *testing.T, cols, rows int, script string) string {
	t.Helper()
	isolateSkillsHome(t)
	plain, _, err := RunFrameScript(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: frameWorkspace(t),
		Model:     "grok",
		Yolo:      true,
	}, cols, rows, script, FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	return plain
}

// runThemeFrame is runStubFrame with the theme named explicitly and the raw
// frame kept, so a test can assert on the palette and not only on the text.
func runThemeFrame(t *testing.T, cols, rows int, theme, script string) (string, string) {
	t.Helper()
	isolateSkillsHome(t)
	plainOut, raw, err := RunFrameScript(Config{
		Session:   NewStub(),
		Theme:     theme,
		Workspace: frameWorkspace(t),
		Model:     "grok",
		Yolo:      true,
	}, cols, rows, script, FrameOpts{Timeout: 10 * time.Second, ANSI: true})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	return plainOut, raw
}

func assertGolden(t *testing.T, name string, cols, rows int, got string) {
	t.Helper()
	for _, ln := range strings.Split(got, "\n") {
		if w := lipgloss.Width(ln); w > cols {
			t.Fatalf("line is %d wide, max is %d: %q", w, cols, ln)
		}
	}
	// The frame owes the terminal every row it has and not one more; this is
	// the height contract the layout engine exists to keep.
	if h := lipgloss.Height(got); h != rows {
		t.Fatalf("frame is %d rows, want %d:\n%s", h, rows, got)
	}
	path := filepath.Join("testdata", name+".golden")
	if *updateGoldens {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./internal/tui -run TestFrameGolden -update)", err)
	}
	if string(want) != got {
		t.Fatalf("golden %s mismatch\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
	}
}

// echoPrompt is 224 characters, so the stub's "echo: " reply is 230, and opens
// with a 150-character unbroken token that only a hard wrap can break.
func echoPrompt() string {
	return strings.Repeat("x", 150) + " alphas bravo charlie delta echo foxtrot golf hotel india juliet kilo lima"
}

func TestFrameGoldenEcho80x24(t *testing.T) {
	prompt := echoPrompt()
	if len(prompt) != 224 {
		t.Fatalf("prompt is %d chars, want 224", len(prompt))
	}
	got := runStubFrame(t, 80, 24, "<wait:idle>"+prompt+"<enter><wait:text:echo:><wait:idle>")
	assertGolden(t, "echo-80x24", 80, 24, got)

	broken := 0
	for _, ln := range strings.Split(got, "\n") {
		if strings.Contains(ln, strings.Repeat("x", 40)) {
			broken++
		}
	}
	if broken < 2 {
		t.Fatalf("the 150-char token was not broken across rows:\n%s", got)
	}
	if !strings.Contains(got, "lima") {
		t.Fatalf("reply tail missing, so the reply is not across 3 rows:\n%s", got)
	}
}

func TestFrameGoldenQuickNotQuit(t *testing.T) {
	got := runStubFrame(t, 80, 24, "<wait:idle>quick question")
	assertGolden(t, "quick-not-quit", 80, 24, got)
	if !strings.Contains(got, "quick question") {
		t.Fatalf("composer lost the typed text:\n%s", got)
	}
	// The session line only fills in once Start returned, so it proves craze
	// stayed up as well as the old "idle" word did.
	if !strings.Contains(got, "cursor │ Grok") {
		t.Fatalf("craze did not stay up:\n%s", got)
	}
}

func TestFrameWaitTimeoutReapsChild(t *testing.T) {
	isolateSkillsHome(t)
	bin := buildFakeAgent(t)
	ws := frameWorkspace(t)
	sess := agent.New(agent.Options{
		Binary:    bin,
		ExtraArgs: []string{"-script=hang"},
		Workspace: ws,
		Force:     true,
		Stderr:    io.Discard,
	})

	start := time.Now()
	_, _, err := RunFrameScript(Config{
		Session:   sess,
		Theme:     "tokyo-night",
		Workspace: ws,
		Yolo:      true,
	}, 80, 24, "<wait:idle>hang<enter><wait:idle>", FrameOpts{Timeout: 2 * time.Second})

	var te *WaitTimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("got %v, want *WaitTimeoutError", err)
	}
	if te.Wait != "<wait:idle>" {
		t.Fatalf("wait %q", te.Wait)
	}
	// The status rows carry no status word; the spinner line is what a working
	// frame shows.
	if !strings.Contains(te.LastFrame, "esc to interrupt") {
		t.Fatalf("diagnostics should carry the last frame, got:\n%s", te.LastFrame)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	if runtime.GOOS != "linux" {
		return
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processRunning(bin) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fake-agent child still running after the frame timeout")
}

func TestFrameWaitsForStartBeforeTyping(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	stub.DelayStart(150 * time.Millisecond)
	plain, _, err := RunFrameScript(Config{
		Session:   stub,
		Theme:     "tokyo-night",
		Workspace: frameWorkspace(t),
		Model:     "grok",
		Yolo:      true,
	}, 80, 24, "hi<enter><wait:text:echo: hi>", FrameOpts{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("a script with no leading wait must still reach the agent: %v", err)
	}
	if !strings.Contains(plain, "echo: hi") {
		t.Fatalf("prompt was dropped before Start returned:\n%s", plain)
	}
}

func TestFrameQuitScriptDoesNotTimeOut(t *testing.T) {
	// The trailing tokens land after bubbletea has exited, where Send is a
	// no-op: the sync barrier must not wait them out.
	for _, script := range []string{
		"<wait:idle><ctrl-d>",
		"<wait:idle>/quit<enter><sleep:100ms><esc>",
		"<ctrl-d><sleep:100ms><esc><esc>",
	} {
		t.Run(script, func(t *testing.T) {
			isolateSkillsHome(t)
			start := time.Now()
			_, _, err := RunFrameScript(Config{
				Session:   NewStub(),
				Theme:     "tokyo-night",
				Workspace: frameWorkspace(t),
				Yolo:      true,
			}, 80, 24, script, FrameOpts{Timeout: 3 * time.Second})
			if err != nil {
				t.Fatalf("quit script returned %v", err)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("quit script waited %s, so it raced the sync barrier", elapsed)
			}
		})
	}
}

func TestFrameIsolatesHomeFromTheChild(t *testing.T) {
	isolateSkillsHome(t)
	outer := os.Getenv("HOME")
	dir := t.TempDir()
	record := filepath.Join(dir, "child-home")
	bin := filepath.Join(dir, "home-probe")
	probe := "#!/bin/sh\nprintf '%s' \"$HOME\" > " + record + "\nexit 1\n"
	if err := os.WriteFile(bin, []byte(probe), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := frameWorkspace(t)
	sess := agent.New(agent.Options{Binary: bin, Workspace: ws, Stderr: io.Discard})

	// An empty script still starts, fails, and cleans up; the child records the
	// HOME it was spawned with.
	if _, _, err := RunFrameScript(Config{
		Session:   sess,
		Theme:     "tokyo-night",
		Workspace: ws,
	}, 80, 24, "", FrameOpts{Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("empty script: %v", err)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("child did not run: %v", err)
	}
	if string(got) == "" {
		t.Fatal("child saw an empty HOME")
	}
	if string(got) == outer {
		t.Fatalf("child inherited the caller's HOME %q", outer)
	}
	if os.Getenv("HOME") != outer {
		t.Fatalf("HOME was not restored: %q", os.Getenv("HOME"))
	}
}

func TestRunFrameScriptRejectsBadScript(t *testing.T) {
	isolateSkillsHome(t)
	_, _, err := RunFrameScript(Config{
		Session:   NewStub(),
		Workspace: frameWorkspace(t),
	}, 80, 24, "<nope>", FrameOpts{Timeout: time.Second})
	var se *ScriptError
	if !errors.As(err, &se) {
		t.Fatalf("got %T (%v), want *ScriptError", err, err)
	}
}

// runFakeFrame drives a real Model against the scripted ACP server, which is
// the only way a golden exercises the whole protocol-to-transcript path.
func runFakeFrame(t *testing.T, script string, cols, rows int, keys string) string {
	t.Helper()
	return runFakeFrameForce(t, script, cols, rows, keys, true)
}

// runFakeFrameForce makes yolo explicit: --no-force is the only way the
// permission line is ever drawn.
func runFakeFrameForce(t *testing.T, script string, cols, rows int, keys string, force bool) string {
	t.Helper()
	bin := buildFakeAgent(t)
	isolateSkillsHome(t)
	ws := frameWorkspace(t)
	sess := agent.New(agent.Options{
		Binary:      bin,
		ExtraArgs:   []string{"-script=" + script},
		Workspace:   ws,
		Force:       force,
		Interactive: true,
		Stderr:      io.Discard,
	})
	plain, _, err := RunFrameScript(Config{
		Session:   sess,
		Theme:     "tokyo-night",
		Workspace: ws,
		Yolo:      force,
	}, cols, rows, keys, FrameOpts{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("run %s frame: %v", script, err)
	}
	return plain
}

func TestFrameGoldenMarkdown80x24(t *testing.T) {
	got := runFakeFrame(t, "markdown", 80, 24, "<wait:idle>go<enter><wait:text:Inline><wait:idle>")
	assertGolden(t, "markdown-80x24", 80, 24, got)
	if strings.ContainsAny(got, "`*") {
		t.Fatalf("markdown markers reached the screen:\n%s", got)
	}
}

func TestFrameGoldenMarkdown120x40(t *testing.T) {
	got := runFakeFrame(t, "markdown", 120, 40, "<wait:idle>go<enter><wait:text:Inline><wait:idle>")
	assertGolden(t, "markdown-120x40", 120, 40, got)
	for _, want := range []string{"+ Thought for ", "Heading", "• first item", "  │ go", "  │ func main() {"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "the shape of this reply") {
		t.Fatalf("a collapsed thought must not show its text:\n%s", got)
	}
}

func TestFrameGoldenThoughtExpanded120x40(t *testing.T) {
	got := runFakeFrame(t, "markdown", 120, 40,
		"<wait:idle>go<enter><wait:text:Inline><wait:idle><ctrl-o><wait:text:shape of this reply>")
	assertGolden(t, "thought-expanded-120x40", 120, 40, got)
	if !strings.Contains(got, "+ Thought for ") {
		t.Fatalf("the summary row stays above the expansion:\n%s", got)
	}
}

func TestFrameGoldenDiff80x24(t *testing.T) {
	got := runFakeFrame(t, "diff", 80, 24, "<wait:idle>go<enter><wait:text:done diff><wait:idle>")
	assertGolden(t, "diff-80x24", 80, 24, got)
	for _, want := range []string{"✓ read  main.go", "✓ edit  main.go  +1 −1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "✓ edit") != 1 {
		t.Fatalf("one row per tool call:\n%s", got)
	}
}

func TestFrameGoldenDiffExpanded120x40(t *testing.T) {
	got := runFakeFrame(t, "diff", 120, 40,
		"<wait:idle>go<enter><wait:text:done diff><wait:idle><ctrl-o><wait:text:package main>")
	assertGolden(t, "diff-expanded-120x40", 120, 40, got)
}

func TestFrameGoldenBash80x24(t *testing.T) {
	got := runFakeFrame(t, "bash", 80, 24, "<wait:idle>go<enter><wait:text:done bash><wait:idle>")
	assertGolden(t, "bash-80x24", 80, 24, got)
	if !strings.Contains(got, "✓ bash  go vet ./...  exit 127") {
		t.Fatalf("missing the exit code row:\n%s", got)
	}
	if !strings.Contains(got, "Command 'go' not found") {
		t.Fatalf("missing the stderr preview:\n%s", got)
	}
}

func TestFrameGoldenTask80x24(t *testing.T) {
	got := runFakeFrame(t, "task", 80, 24, "<wait:idle>go<enter><wait:text:done task><wait:idle>")
	assertGolden(t, "task-80x24", 80, 24, got)
	if !strings.Contains(got, "✓ agent  Count main.go lines  8.0s · grok-4.6-high-fast") {
		t.Fatalf("missing the completed agent row:\n%s", got)
	}
}

func TestFrameGoldenTaskLate80x24(t *testing.T) {
	got := runFakeFrame(t, "task-late", 80, 24, "<wait:idle>go<enter><wait:text:done task><wait:idle>")
	assertGolden(t, "task-late-80x24", 80, 24, got)
	if !strings.Contains(got, "✓ agent  Count main.go lines  8.0s · grok-4.6-high-fast") {
		t.Fatalf("a receipt that arrived first must still join:\n%s", got)
	}
}

func TestFrameGoldenTasks80x24(t *testing.T) {
	got := runFakeFrame(t, "tasks", 80, 24, "<wait:idle>go<enter><wait:text:done tasks><wait:idle>")
	assertGolden(t, "tasks-80x24", 80, 24, got)
	if !strings.Contains(got, "✓ agent  Subagent research") {
		t.Fatalf("the regex fallback should still make an agent row:\n%s", got)
	}
	if !strings.Contains(got, "✓ bash  echo hi") {
		t.Fatalf("missing the shell row:\n%s", got)
	}
}

func TestFrameGoldenTodosHidesTheTodoTool(t *testing.T) {
	got := runFakeFrame(t, "todos", 80, 24, "<wait:idle>go<enter><wait:text:done todos><wait:idle>")
	assertGolden(t, "todos-notes-80x24", 80, 24, got)
	if strings.Contains(got, "Update TODOs") {
		t.Fatalf("the todo writer reached the transcript:\n%s", got)
	}
	if !strings.Contains(got, "tasks: 3 planned") {
		t.Fatalf("missing the todo note:\n%s", got)
	}
	// 24 rows is the short form: header only, no task rows.
	if !strings.Contains(got, "TASKS 1/3") {
		t.Fatalf("missing the pinned panel header:\n%s", got)
	}
	if strings.Contains(got, "▸ Edit main.go") {
		t.Fatalf("a 24-row terminal degrades to a header-only panel:\n%s", got)
	}
}

func TestFrameGoldenTodos100x30(t *testing.T) {
	got := runFakeFrame(t, "todos", 100, 30, "<wait:idle>go<enter><wait:text:done todos><wait:idle>")
	assertGolden(t, "todos-100x30", 100, 30, got)
	for _, want := range []string{"TASKS 1/3", "┃ ▸ Edit main.go", "┃ ○ Run go vet"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Read main.go") {
		t.Fatalf("a completed item folds into the count:\n%s", got)
	}
	if strings.Contains(got, "esc to interrupt") {
		t.Fatalf("an idle frame has no spinner line:\n%s", got)
	}
}

func TestFrameGoldenTodosExpanded100x30(t *testing.T) {
	got := runFakeFrame(t, "todos", 100, 30,
		"<wait:idle>go<enter><wait:text:done todos><wait:idle><ctrl-t><wait:text:Read main.go>")
	assertGolden(t, "todos-expanded-100x30", 100, 30, got)
	if !strings.Contains(got, "┃ ✓ Read main.go") {
		t.Fatalf("expanded lists the completed item:\n%s", got)
	}
}

func TestFrameGoldenTooSmall30x8(t *testing.T) {
	got := runStubFrame(t, 30, 8, "<wait:idle>")
	assertGolden(t, "too-small-30x8", 30, 8, got)
	if !strings.Contains(got, "terminal too small") {
		t.Fatalf("missing the minimum-size message:\n%s", got)
	}
}

func TestFrameGoldenTaskRows100x30(t *testing.T) {
	got := runFakeFrame(t, "task", 100, 30, "<wait:idle>go<enter><wait:text:done task><wait:idle>")
	assertGolden(t, "task-rows-100x30", 100, 30, got)
	// The row lingers under the status rows after the sub-agent finished.
	if !strings.Contains(got, "✓ task  Count main.go lines  8.0s · grok-4.6-high-fast") {
		t.Fatalf("missing the lingering sub-agent row:\n%s", got)
	}
	rows := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if !strings.Contains(rows[len(rows)-1], "✓ task") {
		t.Fatalf("the agent rows belong under the status rows:\n%s", got)
	}
}

func TestFrameGoldenStatus60x24(t *testing.T) {
	got := runStubFrame(t, 60, 24, "<wait:idle>")
	assertGolden(t, "status-60x24", 60, 24, got)
	for _, want := range []string{"ws │ cursor │ Grok (medium) │ agent │ 0m", "▸▸ bypass permissions on"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
}

// TestFrameGoldenAsk100x30 is the question card as cursor's own request drew
// it: one question at a time, with its position in the request.
func TestFrameGoldenAsk100x30(t *testing.T) {
	got := runFakeFrame(t, "ask", 100, 30, "<wait:idle>go<enter><wait:card>")
	assertGolden(t, "ask-100x30", 100, 30, got)
	for _, want := range []string{"question 1/2  Pick one", "> 1 A", "  2 B", "esc skip", "Waiting for your answer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Pick any") {
		t.Fatalf("only the question on screen is drawn:\n%s", got)
	}
}

// TestFrameAskAnswersBothQuestions walks the whole request: a number picks the
// single-select and advances, space and a number toggle the multi-select, and
// Enter on the last question sends the answer.
//
// §5's key string is `1<enter>` then `1<space>3<enter>`; Enter is not needed to
// leave a single-select (a number already picks and advances, §3.11), and an
// Enter there would confirm the second question with nothing picked, so the
// two questions are answered as `1` and `<space>3<enter>`.
func TestFrameAskAnswersBothQuestions(t *testing.T) {
	got := runFakeFrame(t, "ask", 100, 30,
		"<wait:idle>go<enter><wait:card>1<wait:text:question 2/2><space>3<enter><wait:text:asked:><wait:idle>")
	if !strings.Contains(got, "asked:answered:q1=opt-a;q2=opt-x,opt-z") {
		t.Fatalf("the agent did not see the answer:\n%s", got)
	}
	for _, want := range []string{"? Pick one → A", "? Pick any → X, Z"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing note %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "question 1/2") {
		t.Fatalf("the card should be gone once it is answered:\n%s", got)
	}
}

func TestFrameAskEscSkips(t *testing.T) {
	got := runFakeFrame(t, "ask", 100, 30, "<wait:idle>go<enter><wait:card><esc><wait:text:asked:><wait:idle>")
	if !strings.Contains(got, "asked:skipped:") {
		t.Fatalf("esc should skip the whole request:\n%s", got)
	}
	if !strings.Contains(got, "? Question → skipped") {
		t.Fatalf("missing the skipped note:\n%s", got)
	}
}

// TestFrameGoldenPlan100x30 shows both halves of §3.11's plan card: the plan
// itself as a transcript note block, and the two lines that answer it.
func TestFrameGoldenPlan100x30(t *testing.T) {
	got := runFakeFrame(t, "plan", 100, 30, "<wait:idle>go<enter><wait:card>")
	assertGolden(t, "plan-100x30", 100, 30, got)
	for _, want := range []string{
		"PLAN Fake Plan", "Two steps, then stop.", "Steps", "• read main.go",
		"○ Read main.go", "plan Fake Plan", "[a]ccept  [r]eject  esc cancel",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
}

func TestFramePlanDecisions(t *testing.T) {
	for _, tc := range []struct{ name, keys, want string }{
		{"accept", "a", "planned:accepted"},
		{"reject", "r", "planned:rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runFakeFrame(t, "plan", 100, 30,
				"<wait:idle>go<enter><wait:card>"+tc.keys+"<wait:text:planned:><wait:idle>")
			if !strings.Contains(got, tc.want) {
				t.Fatalf("missing %q:\n%s", tc.want, got)
			}
			if !strings.Contains(got, "plan Fake Plan → "+tc.name+"ed") {
				t.Fatalf("missing the decision note:\n%s", got)
			}
		})
	}
}

func TestFramePlanEscCancels(t *testing.T) {
	got := runFakeFrame(t, "plan", 100, 30, "<wait:idle>go<enter><wait:card><esc><wait:idle>")
	if strings.Contains(got, "planned:") {
		t.Fatalf("a cancelled plan produces no decision:\n%s", got)
	}
	if strings.Contains(got, "[a]ccept") {
		t.Fatalf("the card should be gone:\n%s", got)
	}
}

// TestFrameGoldenPermissionNoForce100x30 is the permission line in its pinned
// position, with the [A]lways the fake request offers.
func TestFrameGoldenPermissionNoForce100x30(t *testing.T) {
	got := runFakeFrameForce(t, "permission", 100, 30, "<wait:idle>go<enter><wait:card>", false)
	assertGolden(t, "permission-noforce-100x30", 100, 30, got)
	for _, want := range []string{
		"permission Shell  [a]llow once  [A]lways  [n] reject",
		"▸ prompting for permissions",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
}

func TestFramePermissionAlwaysPicksTheRequestsOwnID(t *testing.T) {
	got := runFakeFrameForce(t, "permission", 100, 30,
		"<wait:idle>go<enter><wait:card>A<wait:text:decision:><wait:idle>", false)
	if !strings.Contains(got, "decision:opt-always") {
		t.Fatalf("A should pick the allow_always option by its own id:\n%s", got)
	}
}

// TestFrameGoldenThemePicker100x30 runs the picker through the real program:
// the names-only list, and the palette changing under the cursor before Enter
// has been pressed.
func TestFrameGoldenThemePicker100x30(t *testing.T) {
	got, raw := runThemeFrame(t, 100, 30, "craze-dark", "<wait:idle><ctrl-g>")
	assertGolden(t, "theme-picker-100x30", 100, 30, got)
	for _, want := range ThemeNames() {
		if !strings.Contains(got, want) {
			t.Fatalf("the picker should list %q:\n%s", want, got)
		}
	}
	if !strings.Contains(raw, ansiFG("#e8a33d")) {
		t.Fatal("the craze-dark accent is missing from the raw frame")
	}

	// Six moves down the frozen list reach gruvbox; the screen is re-themed on
	// the way, with nothing written to disk yet.
	_, moved := runThemeFrame(t, 100, 30, "craze-dark", "<wait:idle><ctrl-g><down><down><down><down><down><down>")
	if !strings.Contains(moved, ansiFG("#fabd2f")) {
		t.Fatal("moving the cursor did not repaint the frame in gruvbox")
	}
	if strings.Contains(moved, ansiFG("#e8a33d")) {
		t.Fatal("the craze-dark accent survived the live preview")
	}

	// Esc puts the palette back.
	_, reverted := runThemeFrame(t, 100, 30, "craze-dark", "<wait:idle><ctrl-g><down><esc>")
	if !strings.Contains(reverted, ansiFG("#e8a33d")) {
		t.Fatal("esc did not restore the craze-dark accent")
	}
}
