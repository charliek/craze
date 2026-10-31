package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/transcript"
)

// retried feeds m a step's failed attempt — a thought, text, a call begun — and
// its retry, each stream event numbered from from, and the next attempt's text.
func retried(t *testing.T, m Model, agentID string, from uint64) Model {
	t.Helper()
	at := m.now()
	return feed(t, m,
		agent.Event{Seq: from, Agent: agentID, Type: agent.EventThought, Text: "abandoned thought", At: at},
		agent.Event{Seq: from + 1, Agent: agentID, Type: agent.EventText, Text: "**abandoned** text", At: at},
		agent.Event{Seq: from + 2, Agent: agentID, Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t1.1.1", Name: "bash", ToolName: "bash", Status: "failed", At: at}, At: at},
		agent.Event{Seq: from + 3, Agent: agentID, Type: agent.EventStepRetry, At: at,
			StepRetry: &agent.StepRetryInfo{Turn: 1, Step: 1, Attempt: 1, Total: 1, Reason: "INTERNAL_ERROR", From: from, Discarded: true}},
		agent.Event{Seq: from + 4, Agent: agentID, Type: agent.EventText, Text: "whole answer", At: at},
	)
}

const strikeSGR = ";9m"

// TestARetriedAttemptIsStruckThrough: every row of the attempt stays where it is,
// struck through and not rendered as an answer, with the note below it saying
// why; the row above and the answer that follows draw as ever.
func TestARetriedAttemptIsStruckThrough(t *testing.T) {
	m := sized(t)
	at := m.now()
	m = feed(t, m, agent.Event{Seq: 100, Type: agent.EventText, Text: "a finished step", At: at},
		agent.Event{Seq: 101, Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t0", Name: "read", Status: "completed", At: at}, At: at})
	m = retried(t, m, "", 102)

	var rows []*entry
	for _, r := range m.main.rows {
		if !r.local {
			rows = append(rows, r)
		}
	}
	want := []struct {
		kind      entryKind
		retracted bool
	}{
		{entryAssistant, false}, {entryTool, false},
		{entryThought, true}, {entryAssistant, true}, {entryTool, true},
		{entryNote, false}, {entryAssistant, false},
	}
	if len(rows) != len(want) {
		t.Fatalf("%d rows: %+v", len(rows), rows)
	}
	key := m.renderKey()
	for i, w := range want {
		r := rows[i]
		if r.kind != w.kind || r.retracted != w.retracted {
			t.Fatalf("row %d: kind %v retracted %v; want %+v", i, r.kind, r.retracted, w)
		}
		drawn := strings.Join(m.renderEntry(m.main, r, key), "\n")
		if struck := strings.Contains(drawn, strikeSGR); struck != w.retracted {
			t.Fatalf("row %d (%q): struck %v; want %v", i, drawn, struck, w.retracted)
		}
	}
	if plain := plainView(m); !strings.Contains(plain, transcript.NoteStepRetried) || !strings.Contains(plain, "**abandoned** text") || !strings.Contains(plain, "whole answer") {
		t.Fatalf("the view:\n%s", plain)
	}
}

// TestAChildsRetryIsStruckInItsPane.
func TestAChildsRetryIsStruckInItsPane(t *testing.T) {
	m := sized(t)
	at := m.now()
	m = feed(t, m, agent.Event{Seq: 100, Type: agent.EventSubagent, SubagentChange: agent.SubagentChangeSpawned,
		Subagent: &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning}, At: at})
	m = retried(t, m, "sub-1", 101)
	p := m.subs["sub-1"]
	if p == nil {
		t.Fatal("no pane for the child")
	}
	struck := 0
	for _, r := range p.rows {
		if r.retracted {
			struck++
		}
	}
	if struck != 3 {
		t.Fatalf("%d struck rows in the child's pane; want 3", struck)
	}
	for _, r := range m.main.rows {
		if r.retracted {
			t.Fatalf("a row of the main pane was struck: %+v", r)
		}
	}
}

// TestAClientThatAttachesAfterARetryDrawsIt: a pane built from a restored model
// strikes what the live one did.
func TestAClientThatAttachesAfterARetryDrawsIt(t *testing.T) {
	m := sized(t)
	m = retried(t, m, "", 100)
	p := &pane{}
	p.rebuild(m.shared.Main)
	struck := 0
	for _, r := range p.rows {
		if r.retracted {
			struck++
		}
	}
	if struck != 3 {
		t.Fatalf("%d struck rows; want 3", struck)
	}
}

// A retried attempt that had streamed nothing draws nothing at all.
func TestARetryOfNothingDrawsNothing(t *testing.T) {
	m := sized(t)
	at := m.now()
	before := len(m.main.rows)
	m = feed(t, m, agent.Event{Seq: 100, Type: agent.EventStepRetry, At: at,
		StepRetry: &agent.StepRetryInfo{Turn: 1, Step: 1, Attempt: 1, Total: 1, Reason: "HTTP 503", From: 100}})
	if len(m.main.rows) != before {
		t.Fatalf("%d rows, was %d", len(m.main.rows), before)
	}
}

// TestCtrlYSkipsARetractedReply: with no selection, Ctrl+Y copies the last reply,
// and a struck-out attempt is none: during the backoff, with no replacement yet,
// it is the reply above; once the replacement streams, that.
func TestCtrlYSkipsARetractedReply(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	rec := captureCopies(t)
	m := selModel(t, &now, "the reply above")
	m.mouseEnabled = false
	at := m.now()
	m = feed(t, m,
		agent.Event{Seq: 200, Type: agent.EventThought, Text: "abandoned thought", At: at},
		agent.Event{Seq: 201, Type: agent.EventText, Text: "abandoned text", At: at},
		agent.Event{Seq: 202, Type: agent.EventStepRetry, At: at,
			StepRetry: &agent.StepRetryInfo{Turn: 1, Step: 1, Attempt: 1, Total: 1, Reason: "INTERNAL_ERROR", From: 200, Discarded: true}},
	)
	ctrlY := func() {
		t.Helper()
		tm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
		m = tm.(Model)
		if msg := runCmd(cmd); msg != nil {
			tm, _ = m.Update(msg)
			m = tm.(Model)
		}
	}
	ctrlY()
	if copies := rec.copies(); len(copies) != 1 || copies[0] != "the reply above" {
		t.Fatalf("during the backoff Ctrl+Y copied %q; want the reply above", copies)
	}
	m = feed(t, m, agent.Event{Seq: 203, Type: agent.EventText, Text: "whole answer", At: at})
	ctrlY()
	if copies := rec.copies(); len(copies) != 2 || copies[1] != "whole answer" {
		t.Fatalf("after the replacement Ctrl+Y copied %q", copies)
	}
}
