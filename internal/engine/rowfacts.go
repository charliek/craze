package engine

import (
	"strings"
	"time"
	"unicode"

	"github.com/rivo/uniseg"

	"github.com/charliek/craze/internal/agent"
)

// The row facts (plan 030 §3.8, §3.10): what a sessions.list row of a host
// that advertises the session capability rowFacts says beyond State — what a
// working session is doing, what it last said, what its head ask is about,
// when it entered the state the session list groups it by, and a failed
// start's error — so a list of every session on the machine can say what each
// one wants without attaching to any.
//
// They are reads, computed when a row is asked for from the engine's own
// authorities — its transcript model (the running tool, the streaming text,
// the last reply), its ask registry (the head ask's body and when it opened),
// and the times the engine and its observer keep of each change of state —
// each taken on its own, as State's parts are: not a cut through the stream,
// which a list polled once a second tolerates.

// RowFacts is one row's facts (protocol.SessionRow's, and HeadAsk.Summary).
// Every string is one line (rowLine): the first non-blank one, tabs expanded,
// control characters dropped, at most RowTextCells cells.
type RowFacts struct {
	// Doing is what a working session is doing: the title of the most
	// recently started tool of the running turn still running (its name when
	// it has none), else "Responding" while the agent's text streams, else
	// "Thinking". "" unless a turn is working — craze's own (activity
	// working) or the agent's (a foreign turn).
	Doing string
	// LastReply is the first line of the last completed assistant message,
	// "" when there is none.
	LastReply string
	// Summary is what the head ask is about: a permission's command or tool
	// title, a question's first question (its title when it has none), a
	// plan's name; "" with no ask open, or when the ask says nothing.
	Summary string
	// Since is when the session entered its row state (RowStateOf), on the
	// session's clock.
	Since time.Time
	// StartErr is the first line of a failed start's error, "" unless
	// StartFailed.
	StartErr string
}

// RowTextCells is the widest a row fact's string is, in terminal cells:
// protocol.RowTextCells — this package imports no protocol — which
// internal/control holds it to.
const RowTextCells = 200

// The Doing words when no tool is running: protocol's DoingResponding and
// DoingThinking, which internal/control holds these to.
const (
	DoingResponding = "Responding"
	DoingThinking   = "Thinking"
)

// RowState is the state a session list groups a session by (plan 030 §3.10's
// table), the first of these that holds.
type RowState int

const (
	// RowNeedsYou: an ask is open.
	RowNeedsYou RowState = iota + 1
	// RowFailed: the start failed, or the last turn failed with no turn
	// since — or the engine is in its error state with no last turn and no
	// foreign turn: a failure whose ending the observer has not seen yet (the
	// ending is committed after the settlement that decided it, and the
	// turn's start cleared the last one). A foreign turn after a failure
	// supersedes it, as its ending does, though the engine's own error state
	// stays until craze's next turn.
	RowFailed
	// RowWorking: starting, replaying, working or closing, or the agent is
	// running a turn of its own.
	RowWorking
	// RowIdle: anything else.
	RowIdle
)

// RowStateOf is st's row state (RowState).
func RowStateOf(st State) RowState {
	switch {
	case st.PendingAsks > 0:
		return RowNeedsYou
	case st.StartFailed,
		st.LastTurn != nil && st.LastTurn.Outcome == TurnFailed,
		st.Activity == ActivityError && st.LastTurn == nil && !st.ForeignTurn:
		return RowFailed
	case st.ForeignTurn, st.Activity == ActivityStarting, st.Activity == ActivityReplaying,
		st.Activity == ActivityWorking, st.Activity == ActivityClosing:
		return RowWorking
	}
	return RowIdle
}

// RowFacts computes the row facts for st, a State this engine answered (the
// sessions.list handler's own read): the row state and Doing are st's, so the
// facts describe the row they go on. It takes, one after another and never
// one inside another, e.mu (the activity's time), e.obsMu (the observer's
// times), the registry's lock (the head ask) and the model's (its progress),
// each for a few reads; it waits on nothing and may be called from any
// goroutine but the observer's.
func (e *Engine) RowFacts(st State) RowFacts {
	var f RowFacts
	if st.StartFailed {
		f.StartErr = rowLine(st.Err)
	}
	var head agent.AskRecord
	haveHead := false
	if st.PendingAsks > 0 && st.HeadAsk.ID != "" {
		head, haveHead = e.asks.Record(st.HeadAsk.ID)
		if haveHead {
			f.Summary = rowLine(askSummary(head))
		}
	}
	p := e.model.Progress()
	f.LastReply = rowLine(p.LastReply)
	if st.Activity == ActivityWorking || st.ForeignTurn {
		switch {
		case p.Tool != "":
			f.Doing = rowLine(p.Tool)
		case p.Responding:
			f.Doing = DoingResponding
		default:
			f.Doing = DoingThinking
		}
	}
	f.Since = e.rowSince(st, head, haveHead)
	return f
}

// rowSince is when the session entered st's row state: the latest of the
// changes that can have moved it there, since whichever was last is the one
// that did — a later change out of the state would have moved it on.
//
//   - needs you: the head ask's opening. The registry lists the open asks in
//     opening order, so the head is the one open longest (an earlier one
//     answered since does not count: the row shows the oldest ask still
//     waiting).
//   - failed: a failed start's time (the activity's), else the failed turn's
//     ending, else — the error state read before the observer saw its
//     ending — the activity's; or an open ask's ending after it.
//   - working: since the engine was built while it starts or replays (a load
//     replays as part of coming up); the activity's for a turn of craze's own
//     — a queued turn that follows another in one settlement keeps the first
//     one's (settleLocked) — the foreign turn's start for the agent's own, the
//     activity's while closing; or an open ask's ending after it, which moved
//     the row back from needs you.
//   - idle: the latest of the activity's change (a turn settling, the start),
//     the last turn's ending (a foreign turn's leaves the activity alone), a
//     replay's end, and an open ask's ending.
//
// An ask that was never open — craze's policy answered it, or it ended as it
// opened — never made the row needs you, so its ending is no change of state
// and is not one of these (askWasOpen).
func (e *Engine) rowSince(st State, head agent.AskRecord, haveHead bool) time.Time {
	e.mu.Lock()
	activityAt := e.activityAt
	e.mu.Unlock()
	e.obsMu.Lock()
	askEnded, foreignAt, replayEnded := e.askEndedAt, e.foreignAt, e.replayEndedAt
	e.obsMu.Unlock()
	var since time.Time
	switch RowStateOf(st) {
	case RowNeedsYou:
		if haveHead {
			return head.OpenedAt
		}
		since = activityAt
	case RowFailed:
		since = activityAt
		if lt := st.LastTurn; !st.StartFailed && lt != nil && lt.Outcome == TurnFailed {
			since = lt.EndedAt
		}
		since = latest(since, askEnded)
	case RowWorking:
		switch st.Activity {
		case ActivityStarting, ActivityReplaying:
			since = e.bornAt
		case ActivityWorking, ActivityClosing:
			since = activityAt
		default:
			since = latest(foreignAt, activityAt)
		}
		since = latest(since, askEnded)
	default:
		since = latest(activityAt, askEnded, replayEnded)
		if lt := st.LastTurn; lt != nil {
			since = latest(since, lt.EndedAt)
		}
	}
	return since
}

// latest is the latest of ts; zero times lose to any other.
func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// observeRowTimes keeps the stream's times of the row-state changes the
// engine's activity does not see (rowSince): an open ask's ending, a foreign
// turn's start and a replay's end, each the event's own At. A replayed event
// is history and changes nothing now. It runs inside the observer (e.obsMu, a
// leaf, once).
func (e *Engine) observeRowTimes(ev agent.Event) {
	if ev.Replayed || ev.At.IsZero() {
		return
	}
	var at *time.Time
	switch {
	case ev.Type == agent.EventAsk && ev.Ask != nil && askWasOpen(ev.Ask):
		at = &e.askEndedAt
	case ev.Type == agent.EventForeignTurn && ev.ForeignTurn != nil && ev.ForeignTurn.Running:
		at = &e.foreignAt
	case ev.Type == agent.EventReplay && ev.Replay != nil && ev.Replay.Phase == agent.ReplayEnd:
		at = &e.replayEndedAt
	default:
		return
	}
	e.obsMu.Lock()
	*at = ev.At
	e.obsMu.Unlock()
}

// askWasOpen says the ask an ending ended had been open — parked in the
// registry with its opening published, so counted in PendingAsks, the row
// needs you while it was — and so that its ending moved the row out of needs
// you (sol r17-c9 4). One that ended as it opened carries its own body (a
// refused Open, a request the provider answered before any handler ran, a
// permission craze's policy answered); one craze's policy answered with an
// opening of its own is marked Auto there and ends automatic (a question or a
// plan the policy decided). Neither was ever open: a working row's Since does
// not move for a permission answered on its own mid-turn.
func askWasOpen(u *agent.AskUpdate) bool {
	return u.Body == nil && u.Outcome != agent.AskAutomatic
}

// askSummary is what an open ask is about (RowFacts.Summary), before rowLine:
// a permission's tool title — which carries the command a shell permission
// asks to run — a question's first question, else its title, and a plan's
// name.
func askSummary(r agent.AskRecord) string {
	b := r.Body
	switch {
	case b.Permission != nil:
		return b.Permission.Tool
	case b.Question != nil:
		for _, q := range b.Question.Questions {
			if strings.TrimSpace(q.Prompt) != "" {
				return q.Prompt
			}
		}
		return b.Question.Title
	case b.Plan != nil:
		return b.Plan.Name
	}
	return ""
}

// rowTab is what a tab becomes in a row fact: the TUI's own tab width (its
// plainLine), so a line keeps its indentation's shape.
const rowTab = "    "

// rowLine is s as a row fact's string (plan 030 §3.10): its first non-blank
// line, trimmed, tabs expanded, control characters dropped — the agent's text
// is sanitised where it enters craze already, and this keeps a row to one
// line whatever reached it — and cut to RowTextCells cells, an ellipsis
// ending a line that was cut.
func rowLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString(rowTab)
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return capCells(b.String(), RowTextCells)
}

// capCells is s cut to at most n terminal cells, grapheme by grapheme, with
// "…" (one cell) ending a string that was cut; s itself when it fits.
func capCells(s string, n int) string {
	if uniseg.StringWidth(s) <= n {
		return s
	}
	var b strings.Builder
	w := 0
	state := -1
	rest := s
	for rest != "" {
		var cluster string
		var cw int
		cluster, rest, cw, state = uniseg.FirstGraphemeClusterInString(rest, state)
		if w+cw > n-1 {
			break
		}
		b.WriteString(cluster)
		w += cw
	}
	b.WriteString("…")
	return b.String()
}
