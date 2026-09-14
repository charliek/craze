package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// queueTwoKeys queues two messages during the fake's long turn.
const queueTwoKeys = "<wait:idle>go the long way<enter><wait:working>" +
	"Reply with PINEAPPLE<enter>Reply with MANGO<enter>"

// runQueueFrame is the golden runner for a turn in progress: the clock and the
// spinner are frozen, and the fake's tool steps are long enough that no build
// is slow enough to leave the state the script waited for.
func runQueueFrame(t *testing.T, script string, cols, rows int, keys string, p agent.Provider) string {
	t.Helper()
	t.Setenv("CRAZE_FAKE_STEP", "30s")
	return runFakeFrameFrozen(t, script, cols, rows, keys, p)
}

// runDrainFrame is the other half: steps short enough that the queue drains
// and the session comes back to idle, which is a state that does not move.
func runDrainFrame(t *testing.T, script string, cols, rows int, keys string, p agent.Provider) string {
	t.Helper()
	t.Setenv("CRAZE_FAKE_STEP", "2s,1ms")
	return runFakeFrameFrozen(t, script, cols, rows, keys, p)
}

func TestFrameGoldenQueueRows(t *testing.T) {
	for _, size := range []struct{ cols, rows int }{{80, 24}, {100, 30}} {
		got := runQueueFrame(t, "long-turn", size.cols, size.rows,
			queueTwoKeys+"<wait:text:#2 Reply with MANGO>", agent.CursorProvider())
		name := fmt.Sprintf("queue-rows-%dx%d", size.cols, size.rows)
		assertGolden(t, name, size.cols, size.rows, got)
		for _, want := range []string{"#1 Reply with PINEAPPLE", "#2 Reply with MANGO", "⧗ 2 queued", "enter queues"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s missing %q:\n%s", name, want, got)
			}
		}
		if strings.Contains(got, "❯ Reply with PINEAPPLE") {
			t.Fatalf("a queued message is not in the transcript yet:\n%s", got)
		}
	}
}

func TestFrameGoldenQueueHover100x30(t *testing.T) {
	// <hover:> is motion with no button, which is what the strip appears on —
	// <motion:> holds the left button and would be a drag.
	got := runQueueFrame(t, "long-turn", 100, 30,
		queueTwoKeys+"<wait:text:#2 Reply with MANGO><hover:10,22>", agent.CursorProvider())
	assertGolden(t, "queue-hover-100x30", 100, 30, got)
	if !strings.Contains(got, "[send now] [edit] [cancel]") {
		t.Fatalf("hover shows no actions:\n%s", got)
	}
}

func TestFrameGoldenQueueSelected80x24(t *testing.T) {
	got := runQueueFrame(t, "long-turn", 80, 24,
		queueTwoKeys+"<wait:text:#2 Reply with MANGO><up>", agent.CursorProvider())
	assertGolden(t, "queue-selected-80x24", 80, 24, got)
	if !strings.Contains(got, "❯ #2 Reply with MANGO") {
		t.Fatalf("↑ selects the newest row:\n%s", got)
	}
	if !strings.Contains(got, "[send now] [edit] [cancel]") {
		t.Fatalf("the selected row shows the actions:\n%s", got)
	}
}

func TestFrameGoldenQueueEdit100x30(t *testing.T) {
	got := runQueueFrame(t, "long-turn", 100, 30,
		queueTwoKeys+"<wait:text:#2 Reply with MANGO><up><enter>", agent.CursorProvider())
	assertGolden(t, "queue-edit-100x30", 100, 30, got)
	if !strings.Contains(got, "editing #2") {
		t.Fatalf("the chip names the row being edited:\n%s", got)
	}
	if !strings.Contains(got, "❯ Reply with MANGO") {
		t.Fatalf("the row's text is in the composer:\n%s", got)
	}
	if !strings.Contains(got, "#2 Reply with MANGO") {
		t.Fatalf("the row stays listed while it is edited:\n%s", got)
	}
}

func TestFrameGoldenQueueSendNowConfirm100x30(t *testing.T) {
	// Cursor cannot interject, so ctrl+l on a draft is send now — and every
	// send now that cancels a running turn asks first.
	got := runQueueFrame(t, "long-turn", 100, 30,
		"<wait:idle>go the long way<enter><wait:working>Reply with PINEAPPLE<ctrl-l>",
		agent.CursorProvider())
	assertGolden(t, "queue-sendnow-confirm-100x30", 100, 30, got)
	if !strings.Contains(got, confirmLine) {
		t.Fatalf("the confirm is missing:\n%s", got)
	}
}

func TestFrameGoldenGrokInterject100x30(t *testing.T) {
	// The merge happens at the next tool result, so this one runs the turn
	// out: the frame is the finished turn with the interjection inside it.
	got := runDrainFrame(t, "grok-long-turn", 100, 30,
		"<wait:idle>go the long way<enter><wait:working>Also say BANANA<ctrl-l>"+
			"<wait:text:DONE step1 Also say BANANA step2><wait:idle>",
		agent.GrokProvider())
	assertGolden(t, "grok-interject-100x30", 100, 30, got)
	if !strings.Contains(got, "↳ Also say BANANA") {
		t.Fatalf("the interjection is not in the transcript:\n%s", got)
	}
	if strings.Contains(got, stopCancelled) {
		t.Fatalf("an interjection must not cancel the turn:\n%s", got)
	}
}

func TestFrameGoldenQueueDrain80x24(t *testing.T) {
	// Both queued messages run, in order, one per settled turn, and the band
	// is empty afterwards.
	got := runDrainFrame(t, "long-turn", 80, 24,
		queueTwoKeys+"<wait:text:#2 Reply with MANGO><wait:text:❯ Reply with MANGO><wait:idle>",
		agent.CursorProvider())
	assertGolden(t, "queue-drain-80x24", 80, 24, got)
	first := strings.Index(got, "❯ Reply with PINEAPPLE")
	second := strings.Index(got, "❯ Reply with MANGO")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("the queue drains in order:\n%s", got)
	}
	if strings.Contains(got, "#1 Reply") || strings.Contains(got, "⧗ ") {
		t.Fatalf("the band is empty once it has drained:\n%s", got)
	}
}

func TestFrameGoldenQueueDegrade40x12(t *testing.T) {
	// Four queued messages need four rows, which is one more than a 12-row
	// terminal can spare: degradation takes the band to zero.
	//
	// The `⧗ n queued` part is not on screen here, and that is the pinned
	// drop order rather than a gap: at 40 columns the permission chip, which
	// never drops, has already crowded it out. The 80- and 100-column
	// goldens above are where the count is read.
	keys := "<wait:idle>go the long way<enter><wait:working>" +
		"one<enter>two<enter>three<enter>four<enter><wait:text:⟳ tool>"
	got := runQueueFrame(t, "long-turn", 40, 12, keys, agent.CursorProvider())
	assertGolden(t, "queue-degrade-40x12", 40, 12, got)
	for _, gone := range []string{"#1 one", "#4 four"} {
		if strings.Contains(got, gone) {
			t.Fatalf("the band must be degraded away at 40x12:\n%s", got)
		}
	}
}

// TestQueueCountSurvivesTheBand is the other half of the degradation: the band
// is gone and the count is not, at a width where the count fits.
func TestQueueCountSurvivesTheBand(t *testing.T) {
	keys := "<wait:idle>go the long way<enter><wait:working>" +
		"one<enter>two<enter>three<enter>four<enter><wait:text:⧗ 4 queued>"
	got := runQueueFrame(t, "long-turn", 80, 12, keys, agent.CursorProvider())
	if strings.Contains(got, "#4 four") {
		t.Fatalf("the band must be degraded away at 80x12:\n%s", got)
	}
	if !strings.Contains(got, "⧗ 4 queued") {
		t.Fatalf("the count is what is left of the band:\n%s", got)
	}
}
