package tui

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
)

// The frames plan 023 §3.6 turns on for native: the mode chip, the question
// card's description line, a plan offer earned with no assistant text at all,
// a denied edit in plan mode and the tasks panel. Each one is a REAL native
// session (agent.NewNative, never tui.Stub) with a scripted model and the
// harness's own tools, so what is pinned is the whole path — the gate, the ask
// registry, the adapter's projection and the renderer — and not a frame drawn
// from a hand-built snapshot.
//
// Every session gets its own temp home (nativeSessionTweak) and its own
// ContentHome (A9), so the plan file lands under the test's directory and the
// menu never depends on what the machine has installed.

// nativePlanText is what the scripted models below put in the plan file. The
// first heading becomes the card's name, so it is what the frame shows.
const nativePlanText = "# Ship the widget\n\n1. read main.go\n2. write the widget\n"

// nativePlanPathRE finds the plan file's absolute path in a request body: the
// backticked path inside a <system-reminder> part, which is the only place the
// harness writes it down (harness/reminders.go) and the only text a test may
// take it from — a prompt of the user's own can name a decoy (r6 finding 4).
// The name carries the session's own id and its start stamp, so nothing
// outside craze can guess it.
var nativePlanPathRE = regexp.MustCompile("(?s)<system-reminder>.*?`([^`]+\\.plan\\.md)`.*?</system-reminder>")

// nativePlanPathIn is the plan file the reminder in call names, or "".
func nativePlanPathIn(call fantasy.Call) string {
	for _, msg := range call.Prompt {
		for _, p := range msg.Content {
			t, ok := fantasy.AsMessagePart[fantasy.TextPart](p)
			if !ok {
				continue
			}
			if m := nativePlanPathRE.FindStringSubmatch(t.Text); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// writesThePlan is a scripted model's onCall hook: a request that names a plan
// file gets nativePlanText written to it, the same bytes however many name it.
//
// The model writes it directly rather than through the write tool on purpose.
// A write row would carry the file's path relative to the workspace, and the
// plan file lives under the harness home — a fresh temp directory whose name is
// this run's — so the frame would hold a path no golden could pin. What the
// tool path does with a plan is held in Go instead
// (agent.TestNativePlanModeTurnEndToEnd) and over the wire
// (tests/cli/test_native.py).
func writesThePlan(t *testing.T) func(fantasy.Call) {
	t.Helper()
	return func(call fantasy.Call) {
		path := nativePlanPathIn(call)
		if path == "" {
			return
		}
		if err := os.WriteFile(path, []byte(nativePlanText), 0o600); err != nil {
			t.Errorf("writing the plan file: %v", err)
		}
	}
}

// nativeFrameSession is a native session started in mode, in a frame workspace,
// with model answering its requests. "" is agent mode, the mode a session with
// no Options.Mode opens in.
func nativeFrameSession(t *testing.T, ws, mode string, model *nativeScriptedModel) agent.Session {
	t.Helper()
	return agent.NewNative(
		agent.Options{Workspace: ws, ContentHome: t.TempDir(), Mode: mode, Interactive: true},
		nativeSessionTweak(t.TempDir(), nativeOneModelTable(), model))
}

// TestFrameGoldenNativePlanMode is the chip: `craze --provider native --plan`
// reaches the harness now, so the mode is drawn exactly as it is for an ACP
// agent — the id, coloured by what the provider says it means, with shift+tab
// beside it (A7).
func TestFrameGoldenNativePlanMode(t *testing.T) {
	ws := frameWorkspace(t)
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.steps = [][]fantasy.StreamPart{cat(nativeTextParts("still planning"), nativeFinishParts())}

	got, _, err := RunFrameScript(Config{
		Session:   nativeFrameSession(t, ws, "plan", model),
		Theme:     "tokyo-night",
		Workspace: ws,
		Yolo:      true,
	}, 100, 30, "<wait:idle>what should we do<enter><wait:text:still planning><wait:idle>",
		FrameOpts{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "native-mode-100x30", 100, 30, got,
		[]string{"◆ plan", "shift+tab", "still planning"},
		// A reminder is never rendered, and it is the one thing in the request
		// that names the plan file (§3.3's provenance).
		[]string{"plan.md", "system-reminder"})
}

// TestFrameGoldenNativePlanOffer is correction 2, drawn: the turn says nothing
// in words — it presents a plan and the plan is accepted — and the offer is
// still armed, because a native plan is a file rather than a reply.
func TestFrameGoldenNativePlanOffer(t *testing.T) {
	ws := frameWorkspace(t)
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.onCall = writesThePlan(t)
	model.steps = [][]fantasy.StreamPart{
		nativeToolStep("c1", "exit_plan_mode", "{}"),
		// Never reached: approving the plan ends the turn (D-51).
		cat(nativeTextParts("never requested"), nativeFinishParts()),
	}

	got, _, err := RunFrameScript(Config{
		Session:   nativeFrameSession(t, ws, "plan", model),
		Theme:     "tokyo-night",
		Workspace: ws,
		Yolo:      true,
	}, 100, 30, "<wait:idle>plan it<enter><wait:card>a<wait:text:"+planOfferPlaceholder+">",
		FrameOpts{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "native-plan-offer-100x30", 100, 30, got,
		[]string{"◆ plan", planOfferPlaceholder, "PLAN Ship the widget", "plan Ship the widget → accepted"},
		// The model said nothing at all this turn, and the step that would
		// have was never asked for.
		[]string{"never requested"})
}

// TestFrameGoldenNativeQuestion is the question card with a description line
// (§3.4): Claude Code's AskUserQuestion gives each option one, and it is drawn
// dimmed under its label. An option with none draws the row it always has,
// which is what keeps every ACP question's frame where it was.
func TestFrameGoldenNativeQuestion(t *testing.T) {
	ws := frameWorkspace(t)
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.steps = [][]fantasy.StreamPart{
		nativeToolStep("c1", "ask_user_question", `{"questions":[{"question":"Which storage?",`+
			`"header":"storage","options":[`+
			`{"label":"sqlite","description":"one file, no server, good enough for a laptop"},`+
			`{"label":"postgres","description":"a server to run, and every query it can answer"},`+
			`{"label":"neither"}]}]}`),
		cat(nativeTextParts("noted"), nativeFinishParts()),
	}

	got, _, err := RunFrameScript(Config{
		Session:   nativeFrameSession(t, ws, "", model),
		Theme:     "tokyo-night",
		Workspace: ws,
		Yolo:      true,
	}, 100, 30, "<wait:idle>ask me<enter><wait:card>", FrameOpts{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "native-question-100x30", 100, 30, got,
		[]string{
			"question 1/1  Which storage?",
			"> 1 sqlite",
			"    one file, no server, good enough for a laptop",
			"  2 postgres",
			"    a server to run, and every query it can answer",
			"  3 neither",
		},
		// The option with no description gains no row of its own.
		[]string{"noted"})
}

// TestFrameGoldenNativePlanDenied is A1 and V2 as the user sees them: an edit
// outside the plan file is denied by the gate in plan mode, and the row shows
// what a denied edit has always shown — the heading, failed, with no reason
// beside it (§4's known limitation). The model reads the refusal and says so.
func TestFrameGoldenNativePlanDenied(t *testing.T) {
	ws := frameWorkspace(t)
	writeFrameFile(t, ws, "notes.txt", "alpha\n")
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.steps = [][]fantasy.StreamPart{
		nativeToolStep("c1", "edit", `{"filePath":"notes.txt","oldString":"alpha","newString":"beta"}`),
		cat(nativeTextParts("I cannot edit files in plan mode"), nativeFinishParts()),
	}

	got, _, err := RunFrameScript(Config{
		Session:   nativeFrameSession(t, ws, "plan", model),
		Theme:     "tokyo-night",
		Workspace: ws,
		Yolo:      true,
	}, 100, 30, "<wait:idle>edit it<enter><wait:text:I cannot edit files in plan mode><wait:idle>",
		FrameOpts{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "native-plan-denied-100x30", 100, 30, got,
		[]string{"✗ edit  notes.txt", "◆ plan", "I cannot edit files in plan mode"},
		// The refusal reaches the model, never the row (transcript.go has no
		// class on a ToolEvent).
		[]string{"Rejected:", "+ beta", "- alpha"})
	if body := readFrameFile(t, ws, "notes.txt"); body != "alpha\n" {
		t.Fatalf("the workspace file is %q; a denied edit writes nothing", body)
	}
}

// TestFrameGoldenNativeTodos is the tasks panel on native: todo_write is the
// harness's own tool, the adapter projects the whole list into the snapshot and
// one event, and the panel the ACP providers have always had draws it (A5).
func TestFrameGoldenNativeTodos(t *testing.T) {
	ws := frameWorkspace(t)
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.steps = [][]fantasy.StreamPart{
		nativeToolStep("c1", "todo_write", `{"todos":[`+
			`{"id":"1","content":"Read main.go","status":"completed"},`+
			`{"id":"2","content":"Edit main.go","status":"in_progress"},`+
			`{"id":"3","content":"Run go vet","status":"pending"}]}`),
		cat(nativeTextParts("done todos"), nativeFinishParts()),
	}

	got, _, err := RunFrameScript(Config{
		Session:   nativeFrameSession(t, ws, "", model),
		Theme:     "tokyo-night",
		Workspace: ws,
		Yolo:      true,
	}, 100, 30, "<wait:idle>go<enter><wait:text:done todos><wait:idle>", FrameOpts{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "native-todos-100x30", 100, 30, got,
		[]string{"TASKS 1/3", "┃ ▸ Edit main.go", "┃ ○ Run go vet", "✓ todo  2 todos", "tasks: 3 planned"},
		// A completed item folds into the count until Ctrl+T expands the panel.
		[]string{"Read main.go"})
}

// readFrameFile reads one file out of a frame workspace: writeFrameFile's
// counterpart, joining the path the way it does.
func readFrameFile(t *testing.T, dir, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
