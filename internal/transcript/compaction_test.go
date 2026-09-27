package transcript

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// A compaction on the fold (plan 028 §3.13, seam 7, P34; §7 A25): the
// started opens Compacting on the transcript it belongs to, the ended clears
// it and draws the note in place, whatever ends the turn clears one whose
// ended never came, and a snapshot carries an open one.

func compactionEvent(agentID string, c agent.CompactionInfo, sec int) agent.Event {
	return agent.Event{Type: agent.EventCompaction, Agent: agentID, Compaction: &c, At: at(sec)}
}

func compactionStarted(agentID, reason string, sec int) agent.Event {
	return compactionEvent(agentID, agent.CompactionInfo{Phase: agent.CompactionStarted, Reason: reason}, sec)
}

func compactionEnded(agentID, reason string, before, after int64, sec int) agent.Event {
	return compactionEvent(agentID, agent.CompactionInfo{Phase: agent.CompactionEnded, Reason: reason,
		TokensBefore: before, TokensAfter: after}, sec)
}

// TestFoldDrawsACompactionNote: a started opens Compacting{Since, Reason} on
// the transcript it belongs to and draws nothing; its ended clears it and
// draws one note, worded by the reason — or, for a failure, the error — with
// the counts in k and M; a child's lands in the child's transcript and never
// the main one's; a replayed ended, with no started before it, draws its note
// in place.
func TestFoldDrawsACompactionNote(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ended  agent.CompactionInfo
		want   string
		reason string
	}{
		{"automatic", agent.CompactionInfo{Reason: agent.CompactionAuto, TokensBefore: 890_000, TokensAfter: 21_000},
			"context compacted · 890k → 21k tokens", agent.CompactionAuto},
		{"on request", agent.CompactionInfo{Reason: agent.CompactionManual, TokensBefore: 12_345, TokensAfter: 850},
			"context compacted on request · 12.3k → 850 tokens", agent.CompactionManual},
		{"after an overflow", agent.CompactionInfo{Reason: agent.CompactionOverflow, TokensBefore: 1_210_000, TokensAfter: 9_876},
			"context was too large — compacted · 1.21M → 9.88k tokens", agent.CompactionOverflow},
		{"a failure", agent.CompactionInfo{Reason: agent.CompactionAuto, TokensBefore: 890_000,
			Err: "native: provider \"test\" failed (HTTP 500)\x1b[31m: boom\nagain"},
			`compaction failed: native: provider "test" failed (HTTP 500): boom again`, agent.CompactionAuto},
		{"a reason this build does not know", agent.CompactionInfo{Reason: "scheduled", TokensBefore: 2_000_000, TokensAfter: 999_600},
			"context compacted · 2M → 1M tokens", "scheduled"},
	} {
		for _, who := range []string{"", "sub-1"} {
			t.Run(tc.name+"/"+map[string]string{"": "main", "sub-1": "child"}[who], func(t *testing.T) {
				m := New(Options{})
				foldAll(t, m, true, sequenced([]agent.Event{
					{Type: agent.EventText, Agent: who, Text: "working on it", At: at(1)},
					compactionStarted(who, tc.reason, 2),
				})...)
				tr := m.ensureSub(who)
				got := tr.Compacting()
				if got == nil || !got.Since.Equal(at(2)) || got.Reason != tc.reason {
					t.Fatalf("after the started: Compacting = %+v, want since %v, reason %q", got, at(2), tc.reason)
				}
				if st := m.State().Compacting; !reflect.DeepEqual(st, map[string]Compacting{who: *got}) {
					t.Fatalf("State().Compacting = %+v, want the one open compaction under %q", st, who)
				}
				if n := len(factsOf(tr, "note")); n != 0 {
					t.Fatalf("a started drew %d notes: %v", n, facts(tr))
				}
				ended := tc.ended
				ended.Phase = agent.CompactionEnded
				e := compactionEvent(who, ended, 3)
				e.Seq = 3
				if c := m.Fold(e); !c.State || c.Scope != who || c.AppendedFrom.IsZero() {
					t.Fatalf("the ended's Change = %+v: it appends its note to %q and says the state moved", c, who)
				}
				checkInvariants(t, m)
				if tr.Compacting() != nil || m.State().Compacting != nil {
					t.Fatalf("the ended left Compacting open: %+v", m.State().Compacting)
				}
				notes := factsOf(tr, "note")
				if len(notes) != 1 || notes[0].Text != tc.want || !notes[0].At.Equal(at(3)) {
					t.Fatalf("notes = %v, want one %q at the ended's At", notes, tc.want)
				}
				if who != "" && m.Main.len() != 0 {
					t.Fatalf("a child's compaction reached the main transcript: %v", facts(m.Main))
				}
			})
		}
	}

	t.Run("replayed, in place", func(t *testing.T) {
		m := New(Options{})
		foldAll(t, m, true, sequenced([]agent.Event{
			replayEvent(agent.ReplayStart),
			{Type: agent.EventUser, Text: "the first prompt", Replayed: true, At: at(1)},
			{Type: agent.EventText, Text: "the first answer", Replayed: true, At: at(2)},
			{Type: agent.EventUser, Text: "/compact keep the API", Replayed: true, At: at(3)},
			func() agent.Event {
				e := compactionEnded("", agent.CompactionManual, 40_000, 5_000, 4)
				e.Replayed = true
				return e
			}(),
			{Type: agent.EventUser, Text: "the next prompt", Replayed: true, At: at(5)},
			replayEvent(agent.ReplayEnd),
		})...)
		want := []string{
			`{user "the first prompt"}`, `{assistant "the first answer"}`, `{user "/compact keep the API"}`,
			`{note "context compacted on request · 40k → 5k tokens"}`, `{user "the next prompt"}`, `{note "restored"}`,
		}
		if got := fmt.Sprint(facts(m.Main)); got != "["+strings.Join(want, " ")+"]" {
			t.Fatalf("the replay drew %s\nwant %v", got, want)
		}
		if m.Main.Compacting() != nil {
			t.Fatal("a replayed ended opened a compaction")
		}
	})
}

// TestACompactionNoteClosesTheDiscardedStream (plan 028 §3.11 item 5, §3.12):
// an overflow's failed request streamed text the transcript then dropped;
// the compaction closes that run where it began, and the note stands between
// it and the replacement's text, which starts an entry of its own below the
// note rather than continuing the discarded attempt's — its thinking too.
func TestACompactionNoteClosesTheDiscardedStream(t *testing.T) {
	for _, kind := range []agent.EventType{agent.EventText, agent.EventThought} {
		t.Run(string(kind), func(t *testing.T) {
			m := New(Options{})
			foldAll(t, m, true, sequenced([]agent.Event{
				{Type: kind, Text: "the discarded attempt", At: at(1)},
				compactionStarted("", agent.CompactionOverflow, 2),
				compactionEnded("", agent.CompactionOverflow, 300_000, 20_000, 5),
				{Type: kind, Text: "the replacement", At: at(6)},
			})...)
			k := "assistant"
			if kind == agent.EventThought {
				k = "thought"
			}
			fs := facts(m.Main)
			if len(fs) != 3 || fs[0].Kind != k || fs[0].Text != "the discarded attempt" ||
				fs[1].Text != "context was too large — compacted · 300k → 20k tokens" ||
				fs[2].Kind != k || fs[2].Text != "the replacement" {
				t.Fatalf("entries = %v, want the discarded run, the note, and the replacement's own run", fs)
			}
			if !fs[0].End.Equal(at(2)) && kind == agent.EventThought {
				t.Fatalf("the discarded thought ended at %v, want the started's At %v", fs[0].End, at(2))
			}
		})
	}
}

// TestTurnEndClearsAnOpenCompaction (P34): a started whose ended never
// comes cannot leave "compacting…" up — every way the main session's turn
// ends clears it, with no note (there is nothing to say about how it went);
// a child's is cleared by the child's own end, and never by the parent's
// turn ending, since a background child outlives it.
func TestTurnEndClearsAnOpenCompaction(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  agent.Event
	}{
		{"the engine's turn ending", agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, StopReason: "end_turn"}}},
		{"another turn's ending", agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-9", Phase: agent.TurnEnded, StopReason: "end_turn"}}},
		{"the wire's done", agent.Event{Type: agent.EventDone, StopReason: "end_turn"}},
		{"the session's error", agent.Event{Type: agent.EventError, Err: &agent.RemoteError{Message: "boom"}}},
		{"a foreign turn's end", agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "wake-1", Reason: agent.ReasonSubagentWake}}},
		{"a replay's end", replayEvent(agent.ReplayEnd)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			end := tc.end
			end.At = at(9)
			foldAll(t, m, true, sequenced([]agent.Event{
				{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "go"}, At: at(1)},
				compactionStarted("", agent.CompactionAuto, 2),
				compactionStarted("sub-1", agent.CompactionAuto, 3),
				end,
			})...)
			if m.Main.Compacting() != nil {
				t.Fatalf("%s left the main compaction open", tc.name)
			}
			if got := m.State().Compacting; !reflect.DeepEqual(got, map[string]Compacting{"sub-1": {Since: at(3), Reason: agent.CompactionAuto}}) {
				t.Fatalf("State().Compacting = %+v: the child's is the child's to end", got)
			}
			for _, f := range factsOf(m.Main, "note") {
				if strings.Contains(f.Text, "compact") {
					t.Fatalf("clearing drew a compaction note: %v", facts(m.Main))
				}
			}
		})
	}

	for _, change := range []string{agent.SubagentChangeFinished, agent.SubagentChangeSpawned} {
		t.Run("a child's "+change, func(t *testing.T) {
			m := New(Options{})
			status := agent.SubagentCompleted
			if change == agent.SubagentChangeSpawned {
				status = agent.SubagentRunning
			}
			foldAll(t, m, true, sequenced([]agent.Event{
				{Type: agent.EventSubagent, SubagentChange: agent.SubagentChangeSpawned, Subagent: &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning}, At: at(1)},
				compactionStarted("sub-1", agent.CompactionAuto, 2),
				{Type: agent.EventSubagent, SubagentChange: change, Subagent: &agent.SubagentInfo{ID: "sub-1", Status: status}, At: at(3)},
			})...)
			if c := m.Sub("sub-1").Compacting(); c != nil || m.State().Compacting != nil {
				t.Fatalf("the child's %s left its compaction open: %+v", change, c)
			}
		})
	}
}

// TestASnapshotCarriesAnOpenCompaction (seam 7): a snapshot cut while a
// compaction runs — the main session's and a child's — carries each on its
// transcript, so a client restored from it, in process or through the codec,
// is the model: its working line reads the same, and the ended that follows
// clears both alike and draws the same note. It holds at every cut of the
// script and when the window omits every entry it can; a snapshot with none
// open carries no "compacting" member at all.
func TestASnapshotCarriesAnOpenCompaction(t *testing.T) {
	evs := sequenced([]agent.Event{
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnStarted, Text: "go"}, At: at(1)},
		{Type: agent.EventSubagent, SubagentChange: agent.SubagentChangeSpawned, Subagent: &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning}, At: at(2)},
		{Type: agent.EventText, Text: "reading", At: at(3)},
		{Type: agent.EventText, Agent: "sub-1", Text: "digging", At: at(4)},
		compactionStarted("", agent.CompactionAuto, 5),
		compactionStarted("sub-1", agent.CompactionOverflow, 6),
		compactionEnded("sub-1", agent.CompactionOverflow, 300_000, 20_000, 7),
		compactionEnded("", agent.CompactionAuto, 890_000, 21_000, 8),
		{Type: agent.EventText, Text: "onwards", At: at(9)},
		{Type: agent.EventDone, StopReason: "end_turn", At: at(10)},
		{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, StopReason: "end_turn"}, At: at(11)},
	})
	for cutAt := 0; cutAt <= len(evs); cutAt++ {
		m := New(Options{})
		foldAll(t, m, true, evs[:cutAt]...)
		s, b := snapshotOf(t, m, 0)
		open := m.State().Compacting
		if has := strings.Contains(string(b), `"compacting"`); has != (open != nil) {
			t.Fatalf("cut %d: the encoding carries compacting=%v with %+v open:\n%s", cutAt, has, open, b)
		}
		r1, r2 := restoredBoth(t, s, b, Options{})
		assertSameModel(t, fmt.Sprintf("restored in process at %d", cutAt), m, r1)
		assertSameModel(t, fmt.Sprintf("restored through the codec at %d", cutAt), m, r2)
		for _, r := range []*Model{r1, r2} {
			if !reflect.DeepEqual(r.Main.Compacting(), m.Main.Compacting()) {
				t.Fatalf("cut %d: the restored main transcript's compaction is %+v, want %+v", cutAt, r.Main.Compacting(), m.Main.Compacting())
			}
		}
		continueBoth(t, fmt.Sprintf("cut %d", cutAt), m, []*Model{r1, r2}, evs[cutAt:]...)
	}

	// Both open, pinned: the member is the transcript's own, after its
	// continuation members, the time in UTC with nanoseconds.
	m := New(Options{})
	foldAll(t, m, true, evs[:6]...)
	_, b := snapshotOf(t, m, 0)
	for _, want := range []string{
		`"main":{"compacting":{"since":"2026-09-21T09:00:05Z","reason":"auto"},"entries":[`,
		`{"id":"sub-1","compacting":{"since":"2026-09-21T09:00:06Z","reason":"overflow"},"entries":[`,
	} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("the snapshot does not carry %s:\n%s", want, b)
		}
	}
	// A window that omits everything it may still carries both.
	full, _ := snapshotOf(t, m, 1<<40)
	budget := encodedLen(t, windowedTo(full, 1, 0))
	s, b := snapshotOf(t, m, budget)
	if !s.Main.Windowed || s.Main.Compacting == nil || len(s.Subs) != 1 || s.Subs[0].Compacting == nil {
		t.Fatalf("the windowed snapshot lost a compaction: main %+v, subs %+v\n%s", s.Main.Compacting, s.Subs, b)
	}
	r1, r2 := restoredBoth(t, s, b, Options{})
	for _, r := range []*Model{r1, r2} {
		if !reflect.DeepEqual(r.State().Compacting, m.State().Compacting) {
			t.Fatalf("the windowed restore's compactions = %+v, want %+v", r.State().Compacting, m.State().Compacting)
		}
	}
}

// TestTokenCountIsThreeFigures pins the note's counts: the number under a
// thousand, then k and M to three significant figures, trailing zeros
// dropped, rounding up across a unit.
func TestTokenCountIsThreeFigures(t *testing.T) {
	for n, want := range map[int64]string{
		-5: "0", 0: "0", 850: "850", 999: "999", 1000: "1k", 1234: "1.23k", 5000: "5k", 9_996: "10k",
		12_345: "12.3k", 21_000: "21k", 99_960: "100k", 890_000: "890k", 999_499: "999k", 999_600: "1M",
		1_000_000: "1M", 1_210_000: "1.21M", 1_500_000: "1.5M", 12_345_678: "12.3M",
	} {
		if got := tokenCount(n); got != want {
			t.Errorf("tokenCount(%d) = %q, want %q", n, got, want)
		}
	}
}
