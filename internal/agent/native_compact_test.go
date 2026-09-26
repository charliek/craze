package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
)

// /compact on a native session (plan 028 §3.12, §3.13, P21, P31; §7 A26): the
// one command native advertises, intercepted in the prompt path and run as a
// turn of its own, and its EventCompaction pair on the stream.

// compactionShapes is every EventCompaction among evs, one line each: phase,
// reason, and, for an ended, whether it failed or carries its counts; then
// the child it belongs to and the replayed mark.
func compactionShapes(evs []Event) []string {
	var out []string
	for _, ev := range evs {
		if ev.Type != EventCompaction {
			continue
		}
		c := ev.Compaction
		s := c.Phase + " " + c.Reason
		if c.Phase == CompactionEnded {
			switch {
			case c.Err != "":
				s += " failed"
			case c.TokensBefore > 0 && c.TokensAfter > 0:
				s += " ok"
			default:
				s += " counts?"
			}
		}
		if ev.Agent != "" {
			s += " (" + ev.Agent + ")"
		}
		if ev.Replayed {
			s += " [r]"
		}
		out = append(out, s)
	}
	return out
}

// nativeSummary is a summarizer's reply the harness accepts: over its
// shortest real summary (compact.go's minSummaryLen), under the seven
// headings' first.
func nativeSummary(what string) step {
	s := "<summary>\n1. Request and intent\n" + what + "\n"
	for len(s) < 700 {
		s += "Filler so the summary is long enough to be a real one. "
	}
	return answer(s + "\n</summary>")
}

// answerUsing is a one-step answer whose request reported input tokens: what
// the next turn's pre-turn check measures the context by (plan 028 §3.7).
func answerUsing(text string, input int64) step {
	return reply(textParts(text), []fantasy.StreamPart{{Type: fantasy.StreamPartTypeFinish,
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: input, OutputTokens: 5, TotalTokens: input + 5}}})
}

// withWindow gives the fixture's test/a a context window, in the table every
// session the fixture opens loads: automatic compaction needs one (§3.6).
func withWindow(t *testing.T, f *nativeFixture, window int) {
	t.Helper()
	table := nativeTestTable("http://127.0.0.1:9/v1")
	m := table.Models["test/a"]
	m.ContextWindow = window
	table.Models["test/a"] = m
	if err := modeltable.Save(f.dir, table); err != nil {
		t.Fatalf("saving the windowed table: %v", err)
	}
}

// TestNativeCompactCommand (A26): native advertises /compact in both install
// deltas — a new session's and a load's — and a plugin's own compact is then
// qualified; the prompt path intercepts exactly `/compact [focus]` and runs
// it as a turn of its own — the summarizer's request and nothing else, the
// EventCompaction pair, end_turn, the command recorded on the entry; a
// context with no message yet is "nothing to compact yet"; a /compact that
// succeeds turns a suppressed automatic compaction back on; and an
// interjected /compact is refused, so it never reaches the running turn's
// model, and runs as its own turn once the caller sends it after that turn.
func TestNativeCompactCommand(t *testing.T) {
	t.Run("recognized exactly", func(t *testing.T) {
		for text, want := range map[string]*[2]string{
			"/compact":                          {"", "/compact"},
			"  /compact  ":                      {"", "/compact"},
			"/compact keep the API design":      {"keep the API design", "/compact keep the API design"},
			"/compact\nkeep\nboth lines":        {"keep\nboth lines", "/compact\nkeep\nboth lines"},
			"/compact\tthe tests":               {"the tests", "/compact\tthe tests"},
			"/compactor":                        nil,
			"/Compact":                          nil,
			"please /compact":                   nil,
			"compact":                           nil,
			"/project:compact":                  nil,
			ShellContextBlock(nil) + "/compact": {"", "/compact"},
			ShellContextBlock([]ShellResult{{Command: "ls", Output: "a\n"}}) + "/compact the list": {"the list", "/compact the list"},
		} {
			focus, command, ok := nativeCompact(text)
			switch {
			case want == nil && ok:
				t.Errorf("nativeCompact(%q) took it as /compact (%q, %q)", text, focus, command)
			case want != nil && (!ok || focus != want[0] || command != want[1]):
				t.Errorf("nativeCompact(%q) = %q, %q, %v; want %q, %q", text, focus, command, ok, want[0], want[1])
			}
		}
	})

	t.Run("advertised in both install deltas", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, ".claude", "commands"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, ".claude", "commands", "compact.md"),
			[]byte("---\ndescription: the project's own compact\n---\nbody\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f := newNativeFixture(t)
		s := f.session(Options{Workspace: ws})
		if err := s.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		install := takeStartDelta(t, s.log)
		if st := install.State; st.Commands == nil || !reflect.DeepEqual(st.Commands.Commands, nativeCommands()) {
			t.Fatalf("a new session's install delta carries commands %+v, want %+v", st.Commands, nativeCommands())
		}
		// The project's own compact is not the command: it is offered by its
		// qualified name, and its expansion is still reachable that way.
		rows := s.Snapshot().Plugins
		if len(rows) != 1 || rows[0].Display != "project:compact" {
			t.Fatalf("the plugin rows are %+v; want the project's compact qualified, since compact is taken", rows)
		}
		f.models["test/a"].push(answer("hi"))
		if _, err := s.Prompt(context.Background(), "hello"); err != nil {
			t.Fatal(err)
		}
		id := s.Snapshot().SessionID
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		loaded := f.session(Options{Workspace: ws, LoadSessionID: id})
		evs, err := startLoad(t, loaded)
		if err != nil {
			t.Fatalf("the load: %v", err)
		}
		var delta *StateDelta
		for _, ev := range evs {
			if ev.Type == EventMeta && ev.Replayed && ev.State != nil && ev.State.Commands != nil {
				delta = ev.State
			}
		}
		if delta == nil || !reflect.DeepEqual(delta.Commands.Commands, nativeCommands()) {
			t.Fatalf("the load's install delta carries commands %+v, want %+v:\n%s", delta, nativeCommands(), strings.Join(loadLines(evs), "\n"))
		}
		if got := loaded.Snapshot().Commands; !reflect.DeepEqual(got, nativeCommands()) {
			t.Fatalf("the loaded snapshot's commands are %+v", got)
		}
	})

	t.Run("intercepted, no model turn", func(t *testing.T) {
		f := newNativeFixture(t)
		s := f.started(Options{})
		a := f.models["test/a"]
		a.push(answer("hi there"))
		if _, err := s.Prompt(context.Background(), "hello"); err != nil {
			t.Fatal(err)
		}
		title := s.Snapshot().Title
		deltaSettled(t, s)

		a.push(nativeSummary("The user said hello."))
		before := a.callCount()
		res, err := s.Prompt(context.Background(), "/compact keep the greeting")
		if err != nil || res.StopReason != harness.StopEndTurn {
			t.Fatalf("/compact = %+v, %v; want end_turn", res, err)
		}
		if n := a.callCount() - before; n != 1 {
			t.Fatalf("/compact sent %d requests; want the summarizer's alone", n)
		}
		sum := a.requests()[before]
		if last := lastUserTexts(t, sum); !strings.Contains(last, "1. Request and intent") || !strings.Contains(last, "keep the greeting") {
			t.Fatalf("the one request is not the summarizer's with the focus: %q", last)
		}
		evs := deltaSettled(t, s)
		if got, want := compactionShapes(evs), []string{"started manual", "ended manual ok"}; !slices.Equal(got, want) {
			t.Fatalf("compactions = %q, want %q", got, want)
		}
		for _, ev := range evs {
			switch ev.Type {
			case EventText, EventThought, EventTool, EventCommand, EventError:
				t.Fatalf("/compact published a %s: no model turn follows it: %+v", ev.Type, ev)
			}
		}
		if err := endings(t, evs, "end_turn"); err != nil {
			t.Fatal(err)
		}
		if s.Snapshot().Title != title {
			t.Fatalf("/compact renamed the session to %q", s.Snapshot().Title)
		}

		id := s.Snapshot().SessionID
		tr, err := store.Load(storedPath(t, f, id))
		if err != nil {
			t.Fatal(err)
		}
		var cmds []string
		for _, e := range tr.Entries {
			if e.Type == store.TypeCompaction {
				cmds = append(cmds, e.Compaction.Reason+" "+e.Compaction.Command)
			}
		}
		if want := []string{"manual /compact keep the greeting"}; !slices.Equal(cmds, want) {
			t.Fatalf("the compaction entries record %q, want %q", cmds, want)
		}

		// A prompt that only looks like it goes to the model.
		a.push(answer("a word, not a command"))
		if _, err := s.Prompt(context.Background(), "/compactor"); err != nil {
			t.Fatal(err)
		}
		if got := lastUserTexts(t, a.requests()[a.callCount()-1]); got != "/compactor" {
			t.Fatalf("/compactor reached the model as %q", got)
		}
		if got := compactionShapes(deltaSettled(t, s)); len(got) != 0 {
			t.Fatalf("/compactor compacted: %q", got)
		}
	})

	t.Run("nothing to compact yet", func(t *testing.T) {
		f := newNativeFixture(t)
		s := f.started(Options{})
		_, err := s.Prompt(context.Background(), "/compact")
		if err == nil || !strings.Contains(err.Error(), "nothing to compact yet") || !errors.Is(err, store.ErrNothingToCompact) {
			t.Fatalf("/compact on a new session = %v; want nothing to compact yet", err)
		}
		evs := deltaSettled(t, s)
		if e := endings(t, evs, ""); e == nil || e.Error() != "native: nothing to compact yet" {
			t.Fatalf("the turn's error is %v", e)
		}
		if got := compactionShapes(evs); len(got) != 0 {
			t.Fatalf("nothing to compact published %q", got)
		}
		if n := f.models["test/a"].callCount(); n != 0 {
			t.Fatalf("nothing to compact sent %d requests", n)
		}
		if got := f.transcripts(); len(got) != 0 {
			t.Fatalf("nothing to compact wrote %v", got)
		}
	})

	t.Run("clears suppression", func(t *testing.T) {
		f := newNativeFixture(t)
		withWindow(t, f, 200_000) // threshold 170k; a summary and the tools fit well under it
		const big = 190_000
		s := f.started(Options{})
		a := f.models["test/a"]
		turn := func(text string, steps ...step) []Event {
			t.Helper()
			a.push(steps...)
			if _, err := s.Prompt(context.Background(), text); err != nil {
				t.Fatalf("%s: %v", text, err)
			}
			return deltaSettled(t, s)
		}
		var got []string
		got = append(got, compactionShapes(turn("turn one", answerUsing("one", big)))...)
		// Over the threshold: the pre-turn compaction fails, is recorded, and
		// turns automatic compaction off; the turn goes on.
		got = append(got, compactionShapes(turn("turn two",
			reply(errorParts(&fantasy.ProviderError{StatusCode: 404, Message: "no such model"})),
			answerUsing("two", big)))...)
		// Still over, and suppressed: no compaction.
		got = append(got, compactionShapes(turn("turn three", answerUsing("three", big)))...)
		got = append(got, compactionShapes(turn("/compact", nativeSummary("Turns one to three.")))...)
		got = append(got, compactionShapes(turn("turn four", answerUsing("four", big)))...)
		// Over again, and on again: /compact cleared the suppression.
		got = append(got, compactionShapes(turn("turn five", nativeSummary("All four."), answer("five")))...)
		want := []string{
			"started auto", "ended auto failed",
			"started manual", "ended manual ok",
			"started auto", "ended auto ok",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("compactions:\n got %q\nwant %q", got, want)
		}
	})

	t.Run("an interjected /compact is refused and runs as its own turn", func(t *testing.T) {
		f := newNativeFixture(t)
		s := f.started(Options{})
		a := f.models["test/a"]
		h := newHeld(t)
		a.push(h.step(textParts("working"), finishParts(fantasy.FinishReasonStop)))
		out := startPrompt(s, "a long task")
		await(t, h.reached, "the held step")
		for _, text := range []string{"/compact", "/compact keep the task"} {
			if err := s.Interject(context.Background(), text); !errors.Is(err, ErrNotInTurn) {
				t.Fatalf("Interject(%q) = %v; want it refused as not in a turn, so the caller queues it", text, err)
			}
		}
		close(h.release)
		if got := await(t, out, "the held turn"); got.err != nil || len(got.res.Unanswered) != 0 {
			t.Fatalf("the held turn = %+v, %v; want it to end with nothing unanswered", got.res, got.err)
		}
		for _, c := range a.requests() {
			for _, m := range c.Prompt {
				if strings.Contains(messageTexts(m), "/compact") {
					t.Fatalf("a refused /compact reached the model: %q", messageTexts(m))
				}
			}
		}
		if got := compactionShapes(deltaSettled(t, s)); len(got) != 0 {
			t.Fatalf("the held turn compacted: %q", got)
		}
		// The caller's queue sends it after the turn: a turn of its own.
		a.push(nativeSummary("A long task, working."))
		if res, err := s.Prompt(context.Background(), "/compact keep the task"); err != nil || res.StopReason != harness.StopEndTurn {
			t.Fatalf("the queued /compact = %+v, %v", res, err)
		}
		if got, want := compactionShapes(deltaSettled(t, s)), []string{"started manual", "ended manual ok"}; !slices.Equal(got, want) {
			t.Fatalf("the queued /compact published %q, want %q", got, want)
		}
	})
}

// TestNativeCompactedIsTheCompactionEvent (plan 028 §3.13, §3.17, X34): the
// harness's Compacted — the parent's, and a child's wrapped in its
// SubagentEvent — is one EventCompaction, the child's under the child's id,
// carrying the phase, the reason and the counts, and the failure's text
// redacted and folded onto one line. Its Usage, the harness's own account of
// what the summarizer was billed, is not an agent field and never reaches the
// wire: the encoded body has no usage and no count of the tokens billed.
func TestNativeCompactedIsTheCompactionEvent(t *testing.T) {
	f := newNativeFixture(t)
	s := f.started(Options{})
	billed := harness.Usage{Input: 424242, Output: 373737, CacheRead: 919191}
	s.sink(harness.Compacted{Phase: harness.CompactionStarted, Reason: "auto"})
	s.sink(harness.Compacted{Phase: harness.CompactionEnded, Reason: "auto", TokensBefore: 890_000, TokensAfter: 21_000, Usage: billed})
	s.subagentEvent(harness.SubagentEvent{ID: "child-1", Event: harness.Compacted{Phase: harness.CompactionEnded, Reason: "overflow",
		TokensBefore: 300_000, Err: "provider failed with " + nativeCanary + "\n\x1b[31mtwice", Usage: billed}})
	evs := deltaSettled(t, s)
	want := []Event{
		{Type: EventCompaction, Compaction: &CompactionInfo{Phase: CompactionStarted, Reason: CompactionAuto}},
		{Type: EventCompaction, Compaction: &CompactionInfo{Phase: CompactionEnded, Reason: CompactionAuto, TokensBefore: 890_000, TokensAfter: 21_000}},
		{Type: EventCompaction, Agent: "child-1", Compaction: &CompactionInfo{Phase: CompactionEnded, Reason: CompactionOverflow,
			TokensBefore: 300_000, Err: "provider failed with " + redact.Marker + " twice"}},
	}
	if len(evs) != len(want) {
		t.Fatalf("published %d events, want %d: %+v", len(evs), len(want), evs)
	}
	for i := range want {
		got := evs[i]
		if got.Type != want[i].Type || got.Agent != want[i].Agent || !reflect.DeepEqual(got.Compaction, want[i].Compaction) {
			t.Fatalf("event %d = %s %q %+v, want %s %q %+v", i, got.Type, got.Agent, got.Compaction, want[i].Type, want[i].Agent, want[i].Compaction)
		}
		body, err := EncodeEvent(got)
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{"usage", "424242", "373737", "919191", nativeCanary} {
			if strings.Contains(body, leak) {
				t.Fatalf("the wire carries %q: %s", leak, body)
			}
		}
	}
}
