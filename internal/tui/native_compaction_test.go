package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/transcript"
)

// A native session's compactions in the TUI (plan 028 §3.13; §7 A27, A28):
// the note the fold draws, the working line while one runs, and a resumed
// compacted session.

// nativeSummaryParts is a summarizer's reply the harness accepts — at least
// its shortest real summary, and n bytes when n is more — under the first of
// the seven headings.
func nativeSummaryParts(what string, n int) []fantasy.StreamPart {
	s := "<summary>\n1. Request and intent\n" + what + "\n"
	for len(s) < max(700, n) {
		s += "Filler so the summary is long enough to be a real one. "
	}
	return cat(nativeTextParts(s+"\n</summary>"), nativeFinishParts())
}

// The golden's thinking and summary sizes, in bytes (TestFrameGoldenNative-
// Compaction100x30): about 150k and 110k tokens of estimate.
const (
	compactionGoldenThought = 564_800
	compactionGoldenSummary = 413_188
)

// longThought is n bytes of thinking, in lines: what a turn that thought at
// length leaves in the context, drawn collapsed.
func longThought(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "step %05d: weighing the greeting once more before answering it\n", i)
	}
	return b.String()[:n]
}

// TestFrameGoldenNativeCompaction100x30 (A27) is a /compact through the real
// program on a real native session (agent.NewNative, never tui.Stub): a turn
// that thought at length, then `/compact keep the greeting`, which the engine
// sends as an ordinary prompt; the native prompt path turns it into the
// harness's own compaction, so the model is asked for the summary and
// nothing else. The /compact row is the user's, and under it the fold's note
// says the context was compacted on request, with its estimated size before
// and after.
//
// The sizes are the harness's estimates of the whole context, the system
// prompt and the tools included, and those name the workspace, the shell and
// the temporary directory, which differ from machine to machine by some tens
// of tokens. So the thinking and the summary are sized to put both counts in
// the hundreds of thousands, where the note says them to the thousand, and
// each near the middle of its thousand: the frame is the same wherever the
// suite runs.
func TestFrameGoldenNativeCompaction100x30(t *testing.T) {
	isolateSkillsHome(t)
	ws := frameWorkspace(t)

	// The session and its scripted model are built fresh inside the closure —
	// a session is spent by its one run — so a both-gate-modes wrapper can
	// call build() once per mode (S2's runFrameModes seam). Every model build()
	// makes is appended here, so the call count is asserted against each of
	// them: one per mode.
	var models []*nativeScriptedModel
	build := func() Config {
		model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
		model.steps = [][]fantasy.StreamPart{
			cat(nativeThoughtParts(longThought(compactionGoldenThought)), nativeTextParts("hi there"), nativeFinishParts()),
			nativeSummaryParts("The user greeted the agent.", compactionGoldenSummary),
		}
		models = append(models, model)
		sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()},
			nativeSessionTweak(t.TempDir(), nativeOneModelTable(), model))
		return Config{Session: sess, Theme: "tokyo-night", Workspace: ws, Yolo: true}
	}

	got, _, err := runFrameModes(t, build, 100, 30, "<wait:idle>hello<enter><wait:text:hi there><wait:idle>/compact keep the greeting<enter><wait:text:context compacted><wait:idle>",
		FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "native-compaction-100x30", 100, 30, got,
		[]string{"❯ hello", "hi there", "❯ /compact keep the greeting", "context compacted on request · "},
		// The summary is the harness's, never the transcript's.
		[]string{"Request and intent", "compacting context"})
	for _, model := range models {
		if model.calls != 2 {
			t.Fatalf("the session sent %d requests; want the turn's and the summarizer's", model.calls)
		}
	}

	// The menu offers it: native's one command, with its own description. A
	// separate builder, its own session and model fresh per call too.
	buildMenu := func() Config {
		menu := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()},
			nativeSessionTweak(t.TempDir(), nativeOneModelTable(), &nativeScriptedModel{provider: "test", wire: "wire-echo"}))
		return Config{Session: menu, Theme: "tokyo-night", Workspace: ws, Yolo: true}
	}
	got, _, err = runFrameModes(t, buildMenu, 100, 30, "<wait:idle>/comp", FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	assertFrameGolden(t, "", 100, 30, got,
		[]string{"/compact", "Summarize the conversation so far to free context; /compact <what to keep>"}, nil)
}

// TestResumeAfterCompactionSendsTheSummary (A28): a session that compacted —
// two turns, the first too long for the kept tail, then `/compact keep the
// plan` — closed and loaded again. The load's replay draws the /compact row
// and its note in place, after the turns it summarized and before the
// restored note, through the shared model's own fold; and the next turn's
// request starts from the summary message and goes on with the tail — the
// second turn, verbatim — and the new prompt, with nothing of the first
// turn's answer.
func TestResumeAfterCompactionSendsTheSummary(t *testing.T) {
	isolateSkillsHome(t)
	ws := frameWorkspace(t)
	table := nativeOneModelTable()
	echo := table.Models["test/echo"]
	echo.ContextWindow = 200_000 // a threshold, so a tail budget: 20k tokens
	table.Models["test/echo"] = echo
	home := t.TempDir()
	long := strings.Repeat("the first answer, at length. ", 4_000) // ~29k tokens: past the tail budget
	var calls []fantasy.Call
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo", onCall: func(c fantasy.Call) { calls = append(calls, c) }}
	model.steps = [][]fantasy.StreamPart{
		cat(nativeTextParts(long), nativeFinishParts()),
		cat(nativeTextParts("the second answer"), nativeFinishParts()),
		nativeSummaryParts("The user asked twice; keep the plan.", 0),
		cat(nativeTextParts("the third answer"), nativeFinishParts()),
	}

	first := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()}, nativeSessionTweak(home, table, model))
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, p := range []string{"the first prompt", "the second prompt", "/compact keep the plan"} {
		if _, err := first.Prompt(context.Background(), p); err != nil {
			t.Fatalf("%q: %v", p, err)
		}
	}
	id := first.Snapshot().SessionID
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir(), LoadSessionID: id},
		nativeSessionTweak(home, table, model))
	t.Cleanup(func() { _ = sess.Close() })
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("the load: %v", err)
	}
	m := transcript.New(transcript.Options{})
	for done := false; !done; {
		select {
		case ev := <-sess.Events():
			m.Fold(ev)
		default:
			done = true
		}
	}
	var drawn []string
	for _, e := range m.Main.Entries() {
		text := []rune(e.Text)
		if len(text) > 40 {
			text = append(text[:40], []rune("…")...)
		}
		drawn = append(drawn, e.Kind.String()+": "+string(text))
	}
	want := []string{
		"user: the first prompt",
		"assistant: …", // past the stream cap: its tail, as a live one is drawn
		"user: the second prompt",
		"assistant: the second answer",
		"user: /compact keep the plan",
		"note: context compacted on request · ",
		"note: restored",
	}
	if len(drawn) != len(want) {
		t.Fatalf("the replay drew\n  %s\nwant\n  %s", strings.Join(drawn, "\n  "), strings.Join(want, "\n  "))
	}
	for i := range want {
		if !strings.HasPrefix(drawn[i], want[i]) {
			t.Fatalf("the replay drew\n  %s\nwant\n  %s", strings.Join(drawn, "\n  "), strings.Join(want, "\n  "))
		}
	}

	if _, err := sess.Prompt(context.Background(), "the third prompt"); err != nil {
		t.Fatalf("the turn after the load: %v", err)
	}
	if len(calls) != 4 {
		t.Fatalf("%d requests; want two turns, the summarizer's and the turn after the load", len(calls))
	}
	var sent []string
	for _, msg := range calls[3].Prompt {
		if msg.Role == fantasy.MessageRoleSystem {
			continue
		}
		var b strings.Builder
		for _, p := range msg.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
				b.WriteString(tp.Text)
			}
		}
		text := b.String()
		if strings.Contains(text, "the first answer, at length.") {
			t.Fatalf("the request after the load still carries the first turn's answer")
		}
		sent = append(sent, string(msg.Role)+": "+text)
	}
	if len(sent) != 4 || !strings.HasPrefix(sent[0], "user: <compacted_context>") ||
		!strings.Contains(sent[0], "The user asked twice; keep the plan.") ||
		sent[1] != "user: the second prompt" || sent[2] != "assistant: the second answer" ||
		!strings.HasSuffix(sent[3], "the third prompt") {
		t.Fatalf("the request after the load is\n  %s\nwant the summary message, the tail (the second turn) and the new prompt",
			strings.Join(sent, "\n  "))
	}
}

// TestAnInterjectedCompactIsQueuedAndRunsAsItsOwnTurn (A26's last clause,
// P31, on the engine): /compact interjected into a running native turn is
// refused as "not in a turn" — the running turn's model never reads it — and
// the same text submitted is queued behind that turn, drains once it ends,
// and runs as a turn of its own: the EventCompaction pair and end_turn, with
// the summarizer's request the only one it sends.
func TestAnInterjectedCompactIsQueuedAndRunsAsItsOwnTurn(t *testing.T) {
	isolateSkillsHome(t)
	reached, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var calls []fantasy.Call
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo", onCall: func(c fantasy.Call) {
		calls = append(calls, c)
		if len(calls) == 1 {
			close(reached)
			<-release
		}
	}}
	model.steps = [][]fantasy.StreamPart{
		cat(nativeTextParts("done with the task"), nativeFinishParts()),
		nativeSummaryParts("A long task, done.", 0),
	}
	sess := agent.NewNative(agent.Options{Workspace: frameWorkspace(t), ContentHome: t.TempDir()},
		nativeSessionTweak(t.TempDir(), nativeOneModelTable(), model))
	e, err := engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res, err := e.Submit(engine.Command{}, "a long task", engine.SubmitQueue, ""); err != nil || res.Turn == "" {
		t.Fatalf("the long task = %+v, %v; want a turn", res, err)
	}
	select {
	case <-reached:
	case <-time.After(wakeWatchdog):
		t.Fatal("the long task's request never came")
	}
	if err := e.Interject(context.Background(), engine.Command{}, "/compact keep the task"); !errors.Is(err, agent.ErrNotInTurn) {
		t.Fatalf("the interjected /compact = %v; want it refused as not in a turn", err)
	}
	res, err := e.Submit(engine.Command{}, "/compact keep the task", engine.SubmitQueue, "")
	if err != nil || res.Queued == nil {
		t.Fatalf("the /compact submitted mid-turn = %+v, %v; want it queued", res, err)
	}
	close(release)

	var shapes []string
	deadline := time.After(wakeWatchdog)
	for ended := 0; ended < 2; {
		select {
		case ev := <-e.Events():
			switch {
			case ev.Type == agent.EventCompaction:
				shapes = append(shapes, "compaction "+ev.Compaction.Phase+" "+ev.Compaction.Reason)
			case ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnStarted:
				shapes = append(shapes, "started "+ev.Turn.Origin+" "+ev.Turn.Text)
			case ev.Type == agent.EventTurn && ev.Turn.Phase == agent.TurnEnded:
				shapes = append(shapes, "ended "+ev.Turn.StopReason)
				ended++
			case ev.Type == agent.EventText:
				shapes = append(shapes, "text "+ev.Text)
			}
		case <-deadline:
			t.Fatalf("the two turns did not end: %q", shapes)
		}
	}
	want := []string{
		"started submit a long task", "text done with the task", "ended end_turn",
		"started drain /compact keep the task", "compaction started manual", "compaction ended manual", "ended end_turn",
	}
	if strings.Join(shapes, "|") != strings.Join(want, "|") {
		t.Fatalf("the engine's record:\n got %q\nwant %q", shapes, want)
	}
	if len(calls) != 2 {
		t.Fatalf("%d requests; want the long task's and the summarizer's", len(calls))
	}
	for _, msg := range calls[0].Prompt {
		for _, p := range msg.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok && strings.Contains(tp.Text, "/compact") {
				t.Fatalf("the running turn's model read the refused /compact: %q", tp.Text)
			}
		}
	}
}

// TestTheWorkingLineSaysCompacting: while the shared model has a compaction
// of the main session's context open, the working line reads
// "compacting context…" in place of the turn's activity, and once its ended
// is folded it reads the activity again (plan 028 §3.13).
func TestTheWorkingLineSaysCompacting(t *testing.T) {
	m := sized(t)
	m = startTurn(t, m, "go")
	if m.status != statusWorking {
		t.Fatalf("status %v, want working", m.status)
	}
	m = feed(t, m, agent.Event{Type: agent.EventCompaction, At: m.now(),
		Compaction: &agent.CompactionInfo{Phase: agent.CompactionStarted, Reason: agent.CompactionAuto}})
	if v := m.spinnerView(); !strings.Contains(v, transcript.CompactingLabel) {
		t.Fatalf("the working line while compacting is %q", v)
	}
	m = feed(t, m, agent.Event{Type: agent.EventCompaction, At: m.now(),
		Compaction: &agent.CompactionInfo{Phase: agent.CompactionEnded, Reason: agent.CompactionAuto, TokensBefore: 890_000, TokensAfter: 21_000}})
	if v := m.spinnerView(); strings.Contains(v, transcript.CompactingLabel) || !strings.Contains(v, "Working") {
		t.Fatalf("the working line after the compaction is %q", v)
	}
	if got := m.shared.Main.Entries(); len(got) == 0 || got[len(got)-1].Text != "context compacted · 890k → 21k tokens" {
		t.Fatalf("the entries are %+v; want the note last", got)
	}
}

// TestTheWorkingLineSaysCompactingWithNoTurnRunning: a foreign-turn or wake
// compaction — the parent turn already ended, its last background child
// already finished — opens main's fold while status is idle and nothing else
// is running. The working line still has to show "compacting context…"
// (plan 028 §3.13); once the ended is folded, the line goes back to hidden.
func TestTheWorkingLineSaysCompactingWithNoTurnRunning(t *testing.T) {
	m := sized(t)
	if m.status != statusIdle {
		t.Fatalf("status %v, want idle", m.status)
	}
	if m.spinnerVisible() {
		t.Fatalf("the spinner is visible before any compaction: %q", m.spinnerView())
	}
	m = feed(t, m, agent.Event{Type: agent.EventCompaction, At: m.now(),
		Compaction: &agent.CompactionInfo{Phase: agent.CompactionStarted, Reason: agent.CompactionOverflow}})
	if m.status != statusIdle {
		t.Fatalf("status %v, want still idle", m.status)
	}
	if !m.spinnerVisible() {
		t.Fatal("the spinner is not visible while an idle-status compaction is open")
	}
	if v := m.spinnerView(); !strings.Contains(v, transcript.CompactingLabel) {
		t.Fatalf("the working line while compacting is %q", v)
	}
	m = feed(t, m, agent.Event{Type: agent.EventCompaction, At: m.now(),
		Compaction: &agent.CompactionInfo{Phase: agent.CompactionEnded, Reason: agent.CompactionOverflow, TokensBefore: 890_000, TokensAfter: 21_000}})
	if m.spinnerVisible() {
		t.Fatalf("the spinner is still visible after the compaction ended: %q", m.spinnerView())
	}
	if v := m.spinnerView(); v != "" {
		t.Fatalf("the working line after the compaction ended is %q, want none", v)
	}
}
