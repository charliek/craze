package control_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/tui"
)

// The row facts on the wire (plan 030 §3.8): a server built with
// Options.RowFacts says so in its info document (capabilities.rowFacts) and
// puts the engine's facts on its sessions.list row; one built without — an
// older host's shape, and the fake host's by default — answers S2's row.

func withRowFacts() hostOpt { return func(c *hostConfig) { c.opts.RowFacts = true } }

// list is the host's one roster row.
func list(t *testing.T, c *client) protocol.SessionRow {
	t.Helper()
	l := ok[protocol.SessionsListResult](t, c.call(protocol.MethodSessionsList, protocol.SessionsListParams{}))
	if len(l.Sessions) != 1 {
		t.Fatalf("%d rows, want the host's one", len(l.Sessions))
	}
	return l.Sessions[0]
}

// TestARowWithoutRowFactsIsS2s: no capability, no fact — even for a session
// with a turn run and an ask open — so an older host's row is what it was.
func TestARowWithoutRowFactsIsS2s(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	a.sayHello(nil)
	h.stub.Emit(agent.Event{Type: agent.EventText, Text: "hello"})
	h.stub.Emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell",
		Options: []agent.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}}}})
	row := list(t, a)
	switch {
	case row.Capabilities.RowFacts:
		t.Fatal("a host without row facts says it has them")
	case row.HeadAsk == nil || row.HeadAsk.Summary != "":
		t.Fatalf("the head ask %+v, want no summary", row.HeadAsk)
	case row.Doing != "" || row.LastReply != "" || !row.Since.IsZero() || row.StartFailed || row.StartErr != "" || row.Prompted:
		t.Fatalf("an S2 row carries row facts: %+v", row)
	}
}

// TestARowCarriesTheRowFacts: the capability in the info document — the
// attach reply's too — and the row facts through a turn: Thinking, a running
// tool's title, Responding, the ask's summary; then idle, prompted, with the
// reply's first line, and Since moving with each change of row state.
func TestARowCarriesTheRowFacts(t *testing.T) {
	h := newHost(t, withRowFacts())
	a := h.dial()
	a.sayHello(nil)
	row := list(t, a)
	switch {
	case !row.Capabilities.RowFacts:
		t.Fatal("the row's capabilities do not say rowFacts")
	case row.Prompted || row.Doing != "" || row.LastReply != "" || row.StartFailed:
		t.Fatalf("a fresh session: %+v", row)
	case row.Since.IsZero() || row.Since.Location() != time.UTC:
		t.Fatalf("since %v, want the start's, in UTC", row.Since)
	}
	idleSince := row.Since
	if att := ok[protocol.AttachResult](t, a.call(protocol.MethodSessionAttach, protocol.AttachParams{SessionID: sid(h)})); !att.Session.Capabilities.RowFacts {
		t.Fatal("the attach reply's info document does not say rowFacts")
	}

	b := h.dial()
	b.sayHello(nil)
	hung := h.stub.HangNext()
	ok[protocol.PromptResult](t, b.call(protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: sid(h),
		CommandID: b.cmd(), Text: "fix it", Mode: protocol.PromptQueue}))
	<-hung
	row = list(t, b)
	if row.Activity != protocol.ActivityWorking || row.Doing != protocol.DoingThinking || !row.Prompted || !row.Since.After(idleSince.Add(-time.Nanosecond)) {
		t.Fatalf("a turn with nothing yet: %+v", row)
	}
	h.stub.Emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t-1", Name: "shell", Title: "Run `go test ./...`", Status: "in_progress"}})
	if row = list(t, b); row.Doing != "Run `go test ./...`" {
		t.Fatalf("a tool running: doing %q", row.Doing)
	}
	h.stub.Emit(agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: "t-1", Name: "shell", Status: "completed"}})
	h.stub.Emit(agent.Event{Type: agent.EventText, Text: "All green.\nThe clock was the flake."})
	if row = list(t, b); row.Doing != protocol.DoingResponding || row.LastReply != "" {
		t.Fatalf("text streaming: doing %q, last reply %q", row.Doing, row.LastReply)
	}
	h.stub.Emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Run `rm -rf build`",
		Options: []agent.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}}}})
	if row = list(t, b); row.PendingAsks != 1 || row.HeadAsk == nil || row.HeadAsk.Summary != "Run `rm -rf build`" {
		t.Fatalf("an ask open: %+v", row.HeadAsk)
	}
	st := ok[protocol.StateResult](t, b.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)}))
	if st.HeadAsk == nil || st.HeadAsk.Summary != "" {
		t.Fatalf("session.state's head ask %+v, want no summary: it is a row fact", st.HeadAsk)
	}
}

// failStart is the Stub with a start that fails.
type failStart struct {
	*tui.Stub
	err error
}

func (s *failStart) Start(context.Context) error { return s.err }

// TestARowOfAFailedStartSaysWhy: startFailed and the error's first line.
func TestARowOfAFailedStartSaysWhy(t *testing.T) {
	boom := errors.New("cursor-agent: not logged in\nrun `cursor-agent login` first")
	h := newHost(t, withRowFacts(), withoutStart(),
		withSession(func(s *tui.Stub) agent.Session { return &failStart{Stub: s, err: boom} }))
	if err := h.eng.Start(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("start: %v", err)
	}
	a := h.dial()
	a.sayHello(nil)
	row := list(t, a)
	if !row.StartFailed || row.StartErr != "cursor-agent: not logged in" || row.Activity != protocol.ActivityError || row.Since.IsZero() {
		t.Fatalf("a failed start's row: %+v", row)
	}
}

// TestTheEnginesRowWordsAreTheProtocols: the engine, which imports no
// protocol, spells the Doing words and bounds a row fact's string exactly as
// the protocol documents them.
func TestTheEnginesRowWordsAreTheProtocols(t *testing.T) {
	if engine.DoingResponding != protocol.DoingResponding || engine.DoingThinking != protocol.DoingThinking {
		t.Fatalf("the engine's words %q/%q, the protocol's %q/%q",
			engine.DoingResponding, engine.DoingThinking, protocol.DoingResponding, protocol.DoingThinking)
	}
	if engine.RowTextCells != protocol.RowTextCells {
		t.Fatalf("the engine bounds a row fact at %d cells, the protocol at %d", engine.RowTextCells, protocol.RowTextCells)
	}
}
