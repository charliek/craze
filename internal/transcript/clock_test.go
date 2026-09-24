package transcript

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// The clock rule (plan 024 §3.2, T1b): a row drawn from an event is stamped at
// the event's At, and a thought run ends at the At of the event that closed it
// — whatever this client's clock reads when it gets round to consuming that
// event. The client's clock is only the fallback for an event that carries no
// At, and a row the client writes for a message of its own keeps it.
//
// These are internal/tui/clock_test.go's model facts, carried here (T1b's
// TestARunEndsAtTheClosingEventsOwnTime, TestAnUnstampedClosingEventFallsBack-
// ToTheClock, TestAToolIsStampedByItsOwnTimeThenTheEnvelopesThenTheClock and
// TestAChildCommandLineIsStampedByItsEvent), with the TUI's assertion lines and
// its fixtures. The two it also has about rows the client writes itself are
// the client's and stay there. One difference in substance: a question opening
// ends the run here on every client (§3.3), where the TUI's does only when its
// caps show the card — the fixture's question is one the TUI shows, so the
// case reads the same.
//
// Every test here consumes events late on purpose: they are stamped from
// clockBase, and the model's clock reads consumedAfter past it, so a stamp
// taken from the clock instead of the event is a different instant and fails.

var clockBase = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

const (
	// closedAfter is when the closing event says the run ended.
	closedAfter = 2 * time.Second
	// consumedAfter is what the client's clock reads while it consumes it.
	consumedAfter = 30 * time.Second
)

// The TUI's names for two of the note wordings, so the fixtures read as its do.
const (
	foreignTurnNote = NoteForeignTurn
	restoredNote    = NoteRestored
)

// lateModel is a model whose clock reads consumedAfter past clockBase and
// stands still there, so "stamped from the clock" is one exact instant.
func lateModel(t *testing.T) *Model {
	t.Helper()
	late := clockBase.Add(consumedAfter)
	return New(Options{Clock: func() time.Time { return late }})
}

// withChild puts a running sub-agent id in the roster, as the TUI's does, so
// the child is a live one.
func withChild(t *testing.T, m *Model, id string) *Model {
	t.Helper()
	feed(t, m, agent.Event{
		Type:           agent.EventSubagent,
		Subagent:       &agent.SubagentInfo{ID: id, Description: "count lines", Status: agent.SubagentRunning},
		SubagentChange: agent.SubagentChangeSpawned,
	})
	return m
}

// thoughtAt is the open thought run every case starts from, streamed at at.
func thoughtAt(agentID string, at time.Time) agent.Event {
	return agent.Event{Type: agent.EventThought, Agent: agentID, Text: "weighing", At: at}
}

// lastFact is the newest entry of kind whose text is text ("" matches any),
// failing the test when there is none.
func lastFact(t *testing.T, tr *Transcript, kind, text string) fact {
	t.Helper()
	got := factsOf(tr, kind)
	for i := len(got) - 1; i >= 0; i-- {
		if text == "" || got[i].Text == text {
			return got[i]
		}
	}
	t.Fatalf("no %s entry %q in %v", kind, text, facts(tr))
	return fact{}
}

// closedThought is the thought run the case opened, which must be closed.
func closedThought(t *testing.T, tr *Transcript) fact {
	t.Helper()
	thoughts := factsOf(tr, "thought")
	if len(thoughts) != 1 {
		t.Fatalf("want the one thought run, got %v", facts(tr))
	}
	if thoughts[0].Open || streamOpen(tr) {
		t.Fatalf("the closing event left the run open: %v", facts(tr))
	}
	return thoughts[0]
}

// A closing case is one event that ends the open thought run above it, and the
// row it draws if it draws one. build stamps it at at; a zero at is the same
// event unstamped, which is the fallback half of the rule.
type closingCase struct {
	name string
	// prelude runs before the thought is opened, stamped a second before it.
	prelude func(at time.Time) []agent.Event
	build   func(at time.Time) agent.Event
	// rowKind and rowText name the row the event draws; "" draws none the test
	// looks for.
	rowKind, rowText string
}

func toolAt(id, status string, at time.Time) *agent.ToolEvent {
	return &agent.ToolEvent{ID: id, Kind: "read", Status: status, Title: "Read main.go", At: at}
}

// The TUI's card fixtures (internal/tui/cards_test.go, replay_test.go).

func stubQuestion() *agent.QuestionEvent {
	return &agent.QuestionEvent{
		ID:    "ask-1",
		Title: "Question",
		Questions: []agent.Question{
			{
				ID:      "q1",
				Prompt:  "Pick one",
				Options: []agent.Option{{ID: "opt-a", Label: "A"}, {ID: "opt-b", Label: "B"}},
			},
			{
				ID:            "q2",
				Prompt:        "Pick any",
				AllowMultiple: true,
				Options: []agent.Option{
					{ID: "opt-x", Label: "X"},
					{ID: "opt-y", Label: "Y"},
					{ID: "opt-z", Label: "Z"},
				},
			},
		},
	}
}

func stubPlanEvent() *agent.PlanEvent {
	return &agent.PlanEvent{
		ID:       "plan-1",
		Name:     "Fake Plan",
		Overview: "Two steps, then stop.",
		Plan:     "## Steps\n\n- read main.go\n- edit main.go\n",
		Todos: []agent.Todo{
			{ID: "1", Content: "Read main.go", Status: "pending"},
			{ID: "2", Content: "Edit main.go", Status: "pending"},
		},
	}
}

func stubPermissionEvent(always bool) *agent.PermissionEvent {
	opts := []agent.PermissionOption{
		{OptionID: "opt-once", Name: "Allow once", Kind: "allow_once"},
		{OptionID: "opt-reject", Name: "Reject once", Kind: "reject_once"},
	}
	if always {
		opts = append([]agent.PermissionOption{
			{OptionID: "opt-always", Name: "Allow always", Kind: "allow_always"},
		}, opts...)
	}
	return &agent.PermissionEvent{ID: "perm-1", Tool: "Shell", Options: opts}
}

func replayEvent(phase string) agent.Event {
	return agent.Event{Type: agent.EventReplay, Replay: &agent.ReplayInfo{Phase: phase}}
}

func closingCases() []closingCase {
	return []closingCase{
		{
			name: "done",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventDone, StopReason: "end_turn", At: at}
			},
		},
		{
			name: "done cancelled",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventDone, StopReason: stopCancelled, At: at}
			},
			rowKind: "note", rowText: stopCancelled,
		},
		{
			name: "foreign turn running",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "ft-1", Running: true}, At: at}
			},
			rowKind: "note", rowText: foreignTurnNote,
		},
		{
			name: "foreign turn ended",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventForeignTurn, ForeignTurn: &agent.ForeignTurnInfo{ID: "ft-1"}, At: at}
			},
		},
		{
			name: "replay end",
			build: func(at time.Time) agent.Event {
				ev := replayEvent(agent.ReplayEnd)
				ev.At = at
				return ev
			},
			rowKind: "note", rowText: restoredNote,
		},
		{
			name: "permission opening",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(false), At: at}
			},
		},
		{
			name: "question opening",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventQuestion, Question: stubQuestion(), At: at}
			},
		},
		{
			name: "plan opening",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventPlan, Plan: stubPlanEvent(), At: at}
			},
			rowKind: "plan",
		},
		{
			name: "new tool row",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventTool, Tool: toolAt("t1", "pending", at), At: at}
			},
			rowKind: "tool",
		},
		{
			name: "tool update in place",
			prelude: func(at time.Time) []agent.Event {
				return []agent.Event{{Type: agent.EventTool, Tool: toolAt("t1", "pending", at), At: at}}
			},
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventTool, Tool: toolAt("t1", "completed", at), At: at}
			},
		},
		{
			name: "todos note",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "1", Content: "Read main.go", Status: "pending"}}, At: at}
			},
			rowKind: "note", rowText: "tasks: 1 planned",
		},
		{
			name: "command line",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventCommand, Command: &agent.ExpandedCommand{
					PluginCommand: agent.PluginCommand{Qualified: "probe-plugin:probe-echo", Kind: agent.PluginKindCommand},
				}, At: at}
			},
			rowKind: "note", rowText: "⤷ probe-plugin:probe-echo (command)",
		},
		{
			name: "error event",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventError, Err: errors.New("the agent fell over"), At: at}
			},
			rowKind: "error", rowText: "the agent fell over",
		},
		{
			name: "synthetic cancelled ending",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{
					ID: "turn-9", Phase: agent.TurnEnded, StopReason: stopCancelled, Synthetic: true,
				}, At: at}
			},
			rowKind: "note", rowText: stopCancelled,
		},
		{
			name: "synthetic refused ending",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{
					ID: "turn-9", Phase: agent.TurnEnded, Synthetic: true, Err: "prompt refused",
				}, At: at}
			},
			rowKind: "error", rowText: "prompt refused",
		},
		{
			name: "engine cancel detail",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Detail: "cancel failed: gone"}, At: at}
			},
			rowKind: "error", rowText: "cancel failed: gone",
		},
		{
			name: "index write failure",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{IndexErr: "index: disk full"}, At: at}
			},
			rowKind: "error", rowText: "index: disk full",
		},
		{
			name: "interjection",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventUser, Interjection: true, Text: "and this", At: at}
			},
			rowKind: "user", rowText: "and this",
		},
		{
			name: "replayed user row",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventUser, Replayed: true, Text: "an old prompt", At: at}
			},
			rowKind: "user", rowText: "an old prompt",
		},
		{
			name: "another client's started",
			build: func(at time.Time) agent.Event {
				return agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{
					ID: "turn-7", Phase: agent.TurnStarted, Text: "from the phone", Origin: agent.TurnOriginSubmit,
				}, At: at}
			},
			rowKind: "user", rowText: "from the phone",
		},
	}
}

// TestARunEndsAtTheClosingEventsOwnTime is the rule for every way a main-session
// thought run is closed — a turn's done, both foreign-turn brackets, a replay's
// end, each non-Auto ask opening, a tool row new or updated in place, and every
// event-driven row — with the event consumed long after it was stamped: the
// run's End, and the row's own At, are the event's At and not the clock.
func TestARunEndsAtTheClosingEventsOwnTime(t *testing.T) {
	for _, tc := range closingCases() {
		t.Run(tc.name, func(t *testing.T) {
			m := lateModel(t)
			tr := m.Main
			if tc.prelude != nil {
				feed(t, m, tc.prelude(clockBase.Add(-time.Second))...)
			}
			closedAt := clockBase.Add(closedAfter)
			feed(t, m, thoughtAt("", clockBase), tc.build(closedAt))

			if f := closedThought(t, tr); !f.End.Equal(closedAt) {
				t.Fatalf("the run ends at %v, want the closing event's %v (the clock reads %v)", f.End, closedAt, m.now())
			}
			if tc.rowKind != "" {
				if f := lastFact(t, tr, tc.rowKind, tc.rowText); !f.At.Equal(closedAt) {
					t.Fatalf("the %s row is stamped %v, want the event's %v", tc.rowKind, f.At, closedAt)
				}
			}
		})
	}
}

// TestAnUnstampedClosingEventFallsBackToTheClock is the same cases unstamped: an
// event with a zero At — only a unit test's fixture carries one — closes the run
// and stamps its row at the client's clock, which is today's behaviour kept.
func TestAnUnstampedClosingEventFallsBackToTheClock(t *testing.T) {
	for _, tc := range closingCases() {
		t.Run(tc.name, func(t *testing.T) {
			m := lateModel(t)
			tr := m.Main
			if tc.prelude != nil {
				feed(t, m, tc.prelude(time.Time{})...)
			}
			feed(t, m, thoughtAt("", clockBase), tc.build(time.Time{}))

			clock := m.now()
			if f := closedThought(t, tr); !f.End.Equal(clock) {
				t.Fatalf("an unstamped closing event ends the run at %v, want the clock's %v", f.End, clock)
			}
			if tc.rowKind != "" {
				if f := lastFact(t, tr, tc.rowKind, tc.rowText); !f.At.Equal(clock) {
					t.Fatalf("an unstamped %s row is stamped %v, want the clock's %v", tc.rowKind, f.At, clock)
				}
			}
		})
	}
}

// TestAnUnstampedClosingEventWithNoClockLeavesTheRunsEnd is the engine's
// configuration of the case above: with no clock a zero At stays zero, so the
// run closes with the End its last chunk gave it (today's endRun: a zero stamp
// leaves End untouched) and the row it draws is unstamped.
func TestAnUnstampedClosingEventWithNoClockLeavesTheRunsEnd(t *testing.T) {
	for _, tc := range closingCases() {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			tr := m.Main
			if tc.prelude != nil {
				feed(t, m, tc.prelude(time.Time{})...)
			}
			feed(t, m, thoughtAt("", clockBase), tc.build(time.Time{}))
			if f := closedThought(t, tr); !f.End.Equal(clockBase) {
				t.Fatalf("the run's end moved to %v with no stamp and no clock, want its own %v", f.End, clockBase)
			}
			if tc.rowKind != "" {
				if f := lastFact(t, tr, tc.rowKind, tc.rowText); !f.At.IsZero() {
					t.Fatalf("an unstamped %s row got a stamp, %v, with no clock", tc.rowKind, f.At)
				}
			}
		})
	}
}

// TestAToolIsStampedByItsOwnTimeThenTheEnvelopesThenTheClock pins execution
// amendment X1: a tool event carries two stamps, and the one that closes the run
// above it and dates a new row is Tool.At when set, else the envelope's At, else
// the client's clock — in the main transcript and a child's alike, for a new row
// and for an update that lands in place (which moves no row's stamp).
func TestAToolIsStampedByItsOwnTimeThenTheEnvelopesThenTheClock(t *testing.T) {
	toolTime := clockBase.Add(closedAfter)
	envelope := clockBase.Add(2 * closedAfter)
	stamps := []struct {
		name          string
		tool, env     time.Time
		wantFromClock bool
		want          time.Time
	}{
		{name: "its own", tool: toolTime, env: envelope, want: toolTime},
		{name: "the envelope's", env: envelope, want: envelope},
		{name: "the clock", wantFromClock: true},
	}
	for _, agentID := range []string{"", "task-1"} {
		for _, inPlace := range []bool{false, true} {
			for _, st := range stamps {
				where := "main"
				if agentID != "" {
					where = "child"
				}
				shape := "new row"
				if inPlace {
					shape = "in place"
				}
				t.Run(fmt.Sprintf("%s %s %s", where, shape, st.name), func(t *testing.T) {
					m := lateModel(t)
					if agentID != "" {
						m = withChild(t, m, agentID)
					}
					earlier := clockBase.Add(-time.Second)
					if inPlace {
						feed(t, m, agent.Event{Type: agent.EventTool, Agent: agentID, Tool: toolAt("t1", "pending", earlier), At: earlier})
					}
					feed(t, m, thoughtAt(agentID, clockBase))
					tr := m.ensureSub(agentID)
					feed(t, m, agent.Event{Type: agent.EventTool, Agent: agentID, Tool: toolAt("t1", "completed", st.tool), At: st.env})

					want := st.want
					if st.wantFromClock {
						want = m.now()
					}
					if f := closedThought(t, tr); !f.End.Equal(want) {
						t.Fatalf("the tool closes the run at %v, want %v", f.End, want)
					}
					tools := factsOf(tr, "tool")
					if len(tools) != 1 {
						t.Fatalf("want one tool row, got %v", facts(tr))
					}
					wantRow := want
					if inPlace {
						// An update lands in the row the first report drew, which
						// keeps that report's stamp.
						wantRow = earlier
					}
					if !tools[0].At.Equal(wantRow) {
						t.Fatalf("the tool row is stamped %v, want %v", tools[0].At, wantRow)
					}
				})
			}
		}
	}
}

// TestAChildCommandLineIsStampedByItsEvent is the child's half of the command
// line: it closes the child's run and dates its note at the event's At.
func TestAChildCommandLineIsStampedByItsEvent(t *testing.T) {
	m := withChild(t, lateModel(t), "task-1")
	closedAt := clockBase.Add(closedAfter)
	feed(t, m, thoughtAt("task-1", clockBase))
	tr := m.ensureSub("task-1")
	feed(t, m, agent.Event{Type: agent.EventCommand, Agent: "task-1", Command: &agent.ExpandedCommand{
		PluginCommand: agent.PluginCommand{Qualified: "probe-plugin:probe-echo", Kind: agent.PluginKindCommand},
	}, At: closedAt})

	if f := closedThought(t, tr); !f.End.Equal(closedAt) {
		t.Fatalf("the child's run ends at %v, want the command's %v", f.End, closedAt)
	}
	if f := lastFact(t, tr, "note", "⤷ probe-plugin:probe-echo (command)"); !f.At.Equal(closedAt) {
		t.Fatalf("the child's command line is stamped %v, want %v", f.At, closedAt)
	}
	if got := factsOf(m.Main, "note"); len(got) != 0 {
		t.Fatalf("the child's line reached the main transcript: %v", got)
	}
}

// TestAChildFinishedIsStampedByItsEvent is r1's fix, carried from the TUI's
// TestAChildFinishedIsStampedByItsEvent: a sub-agent's finished closes its
// transcript's run at the event's own At, unstamped falls back to
// Options.Clock like every other event-driven close, and with no clock
// configured a zero At leaves the run's end as it was (the engine's
// TestAnUnstampedClosingEventWithNoClockLeavesTheRunsEnd, carried).
func TestAChildFinishedIsStampedByItsEvent(t *testing.T) {
	fin := func() *agent.SubagentInfo {
		return &agent.SubagentInfo{ID: "task-1", Description: "count lines", Status: agent.SubagentCompleted}
	}
	t.Run("stamped", func(t *testing.T) {
		m := withChild(t, lateModel(t), "task-1")
		closedAt := clockBase.Add(closedAfter)
		feed(t, m, thoughtAt("task-1", clockBase))
		tr := m.ensureSub("task-1")
		feed(t, m, agent.Event{
			Type: agent.EventSubagent, Subagent: fin(), SubagentChange: agent.SubagentChangeFinished, At: closedAt,
		})

		if f := closedThought(t, tr); !f.End.Equal(closedAt) {
			t.Fatalf("the child's run ends at %v, want the finished event's %v", f.End, closedAt)
		}
	})
	t.Run("unstamped falls back to the clock", func(t *testing.T) {
		m := withChild(t, lateModel(t), "task-1")
		feed(t, m, thoughtAt("task-1", clockBase))
		tr := m.ensureSub("task-1")
		feed(t, m, agent.Event{
			Type: agent.EventSubagent, Subagent: fin(), SubagentChange: agent.SubagentChangeFinished,
		})

		clock := m.now()
		if f := closedThought(t, tr); !f.End.Equal(clock) {
			t.Fatalf("an unstamped finished ends the child's run at %v, want the clock's %v", f.End, clock)
		}
	})
	t.Run("unstamped with no clock leaves the run's end", func(t *testing.T) {
		m := New(Options{})
		m = withChild(t, m, "task-1")
		feed(t, m, thoughtAt("task-1", clockBase))
		tr := m.ensureSub("task-1")
		feed(t, m, agent.Event{
			Type: agent.EventSubagent, Subagent: fin(), SubagentChange: agent.SubagentChangeFinished,
		})

		if f := closedThought(t, tr); !f.End.Equal(clockBase) {
			t.Fatalf("the child's run end moved to %v with no stamp and no clock, want its own %v", f.End, clockBase)
		}
	})
}
