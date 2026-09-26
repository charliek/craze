package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
)

// TestFrameGoldenNativeResume100x30 is a restored native session (plan 028
// §3.4, A11's golden): a real native session (agent.NewNative, never tui.Stub)
// runs one turn — a prompt behind a shell-context block, a read, an edit, a
// command that passed and one that exited non-zero, a todo list, thinking and
// an answer — and closes; a second one loads it by id, with the title its
// index row would carry, and the frame is drawn once the replay's bracket has
// closed. The user row is the prompt alone; the rows are the calls' own and,
// collapsed, look like live ones (native-tools-80x24 is the live frame,
// expanded): each command row previews its output's first line, and no exit
// code is shown, since the transcript keeps none; the edit row has no counts,
// since it keeps no diff either. The task panel is the restored list, the
// composer rule carries the seeded title, and the `restored` note marks the
// seam. Expanded, a replayed row ends with the "(replayed)" label after its
// output (checked on a second load, in fragments: the read's stored text holds
// the workspace's absolute path). Nothing in internal/tui changed for any of
// it: the load goes through Config.Loading exactly as an ACP load does
// (TestFrameGoldenReplay).
func TestFrameGoldenNativeResume100x30(t *testing.T) {
	isolateSkillsHome(t)
	ws := frameWorkspace(t)
	writeFrameFile(t, ws, "main.go", "package main\n\n// TODO: ship it\n")
	writeFrameFile(t, ws, "notes.txt", "alpha\n")
	table := nativeOneModelTable()
	harnessHome := t.TempDir()
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.steps = [][]fantasy.StreamPart{
		nativeToolStep("c1", "read", `{"filePath":"main.go"}`),
		nativeToolStep("c2", "edit", `{"filePath":"notes.txt","oldString":"alpha","newString":"beta"}`),
		nativeToolStep("c3", "bash", `{"command":"echo hi"}`),
		nativeToolStep("c4", "bash", `{"command":"echo boom >&2; exit 3"}`),
		nativeToolStep("c5", "todo_write", `{"todos":[{"id":"1","content":"Edit notes.txt","status":"completed"},{"id":"2","content":"Run the tests","status":"in_progress"}]}`),
		cat(nativeThoughtParts("wrapping up"), nativeTextParts("done tools"), nativeFinishParts()),
	}

	// The stored session: one turn, then closed, as a quit leaves it.
	first := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()}, nativeSessionTweak(harnessHome, table, model))
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	prompt := agent.ShellContextBlock([]agent.ShellResult{{Command: "git status", Output: "clean\n"}}) + "fix the notes"
	if _, err := first.Prompt(context.Background(), prompt); err != nil {
		t.Fatalf("the stored turn: %v", err)
	}
	id := first.Snapshot().SessionID
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Each frame loads the stored session afresh: the runner closes its engine,
	// and with it the session and the transcript's lock, when the script ends.
	load := func(cols, rows int, script string) string {
		t.Helper()
		sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir(), LoadSessionID: id, Title: "fix the notes"},
			nativeSessionTweak(harnessHome, table, model))
		got, _, err := RunFrameScript(Config{
			Session:   sess,
			Theme:     "tokyo-night",
			Workspace: ws,
			Yolo:      true,
			Loading:   true,
		}, cols, rows, script, FrameOpts{Timeout: 20 * time.Second})
		if err != nil {
			t.Fatalf("run frame script: %v", err)
		}
		return got
	}

	got := load(100, 30, "<wait:text:restored><wait:idle>")
	assertFrameGolden(t, "native-resume-100x30", 100, 30, got,
		[]string{
			"❯ fix the notes",
			"✓ read  main.go",
			"✓ edit  notes.txt",
			"✓ bash  echo hi",
			"✓ bash  echo boom >&2; exit 3",
			"+ Thought",
			"done tools",
			"restored",
			"Run the tests",   // the restored task list
			"fix the notes ─", // the seeded title on the composer rule
		},
		// No exit code the transcript does not keep, and the label only once a
		// row is expanded.
		[]string{"shell_context", "git status", "restoring", "exit 3  exit 3", "(replayed)"})
	// A command row previews its output's first line, as a live one does.
	for head, want := range map[string]string{"✓ bash  echo hi": "hi", "✓ bash  echo boom >&2; exit 3": "boom"} {
		if got := frameLineAfter(got, head); got != want {
			t.Fatalf("the row %q previews %q, want %q:\n%s", head, got, want, got)
		}
	}

	// Expanded, each row with a body ends with the label after its output:
	// the read's content, and each command's stream.
	open := load(100, 50, "<wait:text:restored><wait:idle><ctrl-o><wait:text:(replayed)>")
	assertFrameGolden(t, "", 100, 50, open, []string{"package main", "// TODO: ship it", "exit code: 3"}, nil)
	if got := frameLineAfter(open, "✓ bash  echo hi"); got != "hi" {
		t.Fatalf("the expanded row previews %q, want its output first:\n%s", got, open)
	}
	lines := strings.Split(open, "\n")
	var before []string
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "(replayed)" {
			before = append(before, strings.TrimSpace(lines[i-1]))
		}
	}
	if want := []string{"</content>", "hi", "</shell_metadata>"}; strings.Join(before, "|") != strings.Join(want, "|") {
		t.Fatalf("the label follows %q, want %q: the read's content and the two commands' streams, each last:\n%s", before, want, open)
	}
}

// frameLineAfter is the line after the first line of frame that starts with
// head, trimmed: the first body line a row draws under its head.
func frameLineAfter(frame, head string) string {
	lines := strings.Split(frame, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(ln, head) && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1])
		}
	}
	return ""
}

// TestNativeLoadStartedFollowsTheReplay is A6's last clause on the engine rig
// (plan 028 §3.4, S2's conditions): the engine's Started — Ready closing, the
// gate opening — comes only once the load's whole bracket is in the log, the
// end bracket last, so a client that waits for readiness never sees a replay
// still running; until then the engine admits no command.
func TestNativeLoadStartedFollowsTheReplay(t *testing.T) {
	isolateSkillsHome(t)
	ws := frameWorkspace(t)
	table := nativeOneModelTable()
	harnessHome := t.TempDir()
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.steps = [][]fantasy.StreamPart{cat(nativeTextParts("hi there"), nativeFinishParts())}
	first := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()}, nativeSessionTweak(harnessHome, table, model))
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	id := first.Snapshot().SessionID
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir(), LoadSessionID: id},
		nativeSessionTweak(harnessHome, table, model))
	e, err := engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if _, err := e.Submit(engine.Command{}, "too early", engine.SubmitQueue, ""); err == nil {
		t.Fatal("the engine admitted a command before the load started")
	}
	// No reader: the replay is far shorter than the primary, so Start runs to
	// its end, and everything it published is buffered when Ready closes.
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-e.Ready():
	default:
		t.Fatal("Ready is not closed after Start")
	}
	if st := e.State(); st.Activity != engine.ActivityIdle || st.SessionID != id {
		t.Fatalf("after the load: activity %v, session %q; want idle on %s", st.Activity, st.SessionID, id)
	}
	var shapes []string
	for done := false; !done; {
		select {
		case ev := <-e.Events():
			shape := string(ev.Type)
			if ev.Replay != nil {
				shape += ":" + string(ev.Replay.Phase)
			}
			if ev.Replayed {
				shape += " (replayed)"
			}
			shapes = append(shapes, shape)
		default:
			done = true
		}
	}
	want := []string{"replay:start", "user (replayed)", "text (replayed)", "meta (replayed)", "replay:end"}
	if strings.Join(shapes, ", ") != strings.Join(want, ", ") {
		t.Fatalf("the load published %q before Started; want %q, the end bracket last", shapes, want)
	}
}
