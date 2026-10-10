package tui

import (
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
)

// The side view's frames (plan 040 §3.6, §7.7), in process and over the
// socket: an AsidingStub answers side questions as a native, grok or gx
// session does, so the whole path — the slash catalog, the view, the
// session.aside call and its aside notifications over the socket — is the
// production one.

// runAsideFrame runs script against an AsidingStub, its first turn hung
// until it is cancelled when hang is set, with the clocks frozen.
func runAsideFrame(t *testing.T, cols, rows int, script string, hang bool) string {
	t.Helper()
	isolateSkillsHome(t)
	plain, _, err := runFrameModes(t, func() Config {
		stub := frameStub()
		if hang {
			stub.HangNext()
		}
		return Config{
			Session:   AsidingStub{Stub: stub},
			Theme:     "tokyo-night",
			Workspace: frameWorkspace(t),
			Model:     "grok",
			Yolo:      true,
		}
	}, cols, rows, script, FrameOpts{Timeout: 10 * time.Second, Freeze: true})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	return plain
}

// TestFrameGoldenBtwMidTurn100x30: /btw over a running turn opens the side
// view over the transcript region and streams the answer into it — the
// header, the question, the answer and its footer — while the chrome is the
// main session's: its working line (with nothing to interrupt from here), and
// in status row 2 where the keyboard is instead of the mode.
func TestFrameGoldenBtwMidTurn100x30(t *testing.T) {
	got := runAsideFrame(t, 100, 30,
		"<wait:idle>wait<enter><wait:working>/btw what is going on?<enter><wait:text:The session goes on.>", true)
	assertFrameGolden(t, "btw-midturn-100x30", 100, 30, got,
		[]string{btwHeaderNote, "❯ what is going on?", "You asked: what is going on? (side question 1). The session goes on.",
			"Grok · 1k in (800 cached) · 4 out", "Working · 0s", btwStatusChip, btwHint},
		[]string{"❯ wait", "esc to interrupt", "◆ agent"})
}

// TestFrameGoldenBtwFollowUp100x30: a follow-up in the view carries the first
// pair — its answer counts it — under the first exchange, on an idle session
// whose own transcript stays where it is underneath.
func TestFrameGoldenBtwFollowUp100x30(t *testing.T) {
	got := runAsideFrame(t, 100, 30,
		"<wait:idle>hello<enter><wait:text:echo: hello><wait:idle>/btw first question<enter><wait:text:(side question 1)>"+
			"and a follow-up<enter><wait:text:(side question 2)>", false)
	assertFrameGolden(t, "btw-followup-100x30", 100, 30, got,
		[]string{"❯ first question", "❯ and a follow-up", "You asked: and a follow-up (side question 2).",
			"Grok · 2k in (1.6k cached) · 4 out"},
		[]string{"echo: hello"})
}

// TestFrameGoldenBtwMenu100x30: where the session answers side questions,
// the slash menu offers /btw.
func TestFrameGoldenBtwMenu100x30(t *testing.T) {
	got := runAsideFrame(t, 100, 30, "<wait:idle>/b", false)
	assertFrameGolden(t, "btw-menu-100x30", 100, 30, got,
		[]string{"/btw", "Ask a side question (not saved): /btw <question>"}, nil)
}

// TestFrameGoldenNativeBtw100x30 is the side view over a real native session
// (agent.NewNative over the scripted model): a turn, then /btw, whose answer
// is the model's second step — its own request, after the turn has ended, so
// the scripted model is never called twice at once — with the native footer:
// the model's display name and the step's token counts.
func TestFrameGoldenNativeBtw100x30(t *testing.T) {
	got, _, err := runFrameModes(t, func() Config {
		model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
		model.steps = [][]fantasy.StreamPart{
			cat(nativeTextParts("hi there"), nativeFinishParts()),
			cat(nativeTextParts("side ", "answer"), nativeFinishParts()),
		}
		ws := frameWorkspace(t)
		return Config{
			Session: agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir(), NoPrimary: frameNoPrimary},
				nativeSessionTweak(t.TempDir(), nativeOneModelTable(), model)),
			Theme:     "tokyo-night",
			Workspace: ws,
			Yolo:      true,
		}
	}, 100, 30, "<wait:idle>hello<enter><wait:text:hi there><wait:idle>/btw what did you say?<enter><wait:text:side answer>",
		FrameOpts{Timeout: 20 * time.Second, Freeze: true})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "native-btw-100x30", 100, 30, got,
		[]string{btwHeaderNote, "❯ what did you say?", "side answer", "Echo · 10 in (0 cached) · 5 out"},
		// The main transcript is not drawn under the side view.
		[]string{"hi there", "❯ hello"})
}
