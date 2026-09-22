package transcript

import (
	"slices"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// Fold applies one event to the model and says what it changed (plan 024
// §3.3). It is the TUI's fold (internal/tui/app.go applyEvent and its
// helpers, subview.go applySubagentEvent/applyChildEvent, cards.go pushCard)
// with its rules unchanged but for the two §3.3 names: every non-Auto ask
// opening ends the run, and an ask's ending draws no entry.
//
// Routing is the TUI's: EventSubagent goes to the roster whatever Agent says;
// any other event with an Agent goes to that child's transcript, created on
// first use; everything else to the main transcript and the model. The kind
// table below is the dispatch, and TestFoldClassifiesEveryEventKind holds it
// against agent's EventType constants in both directions.
//
// Fold takes the model's mutex and nothing else, blocks on nothing, runs no
// callback but Options.Clock — and that only for an event with a zero At —
// never calls Error() on an error, and is total over every event shape: an
// unknown kind, and every kind with any payload nil, is a no-op or a defined
// effect.
func (m *Model) Fold(ev agent.Event) Change {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.begin(ev.Seq)
	row, ok := kinds[ev.Type]
	switch {
	case !ok:
		// A kind this package does not know: nothing to do.
	case row.anyAgent || ev.Agent == "":
		row.main(m, ev)
	default:
		// The TUI's applyChildEvent ensures the child's transcript before it
		// looks at the kind, so a kind a child ignores still creates it.
		t := m.ensureSub(ev.Agent)
		if row.child != nil {
			row.child(t, ev)
		}
	}
	return m.finish()
}

// class is what a kind does to the model (§3.3): stream rows append history
// and are not re-applicable; state rows are keyed upserts or full replacements
// and converge when re-applied; marker rows move flags and close runs, and
// converge too. A kind can be more than one.
type class uint8

const (
	classStream class = 1 << iota
	classState
	classMarker
)

// kindRow is one row of §3.3's table.
type kindRow struct {
	class class
	// main folds the kind for the main session.
	main func(m *Model, ev agent.Event)
	// child folds it into a child's transcript; nil with childIgnored set is
	// the table saying a child's event of this kind is deliberately dropped.
	child        func(t *Transcript, ev agent.Event)
	childIgnored bool
	// anyAgent routes the kind to main whatever Agent says: the roster.
	anyAgent bool
}

// kinds is §3.3's table, row by row, and the fold's dispatch.
var kinds = map[agent.EventType]kindRow{
	agent.EventText:        {class: classStream, main: foldText, child: childText},
	agent.EventThought:     {class: classStream, main: foldThought, child: childThought},
	agent.EventTool:        {class: classState | classStream, main: foldTool, child: childTool},
	agent.EventTodos:       {class: classState | classStream, main: foldTodos, childIgnored: true},
	agent.EventPermission:  {class: classState, main: foldPermission, childIgnored: true},
	agent.EventQuestion:    {class: classState, main: foldQuestion, childIgnored: true},
	agent.EventPlan:        {class: classState | classStream, main: foldPlan, childIgnored: true},
	agent.EventAsk:         {class: classState, main: foldAsk, childIgnored: true},
	agent.EventDone:        {class: classMarker | classStream, main: foldDone, childIgnored: true},
	agent.EventError:       {class: classStream, main: foldError, childIgnored: true},
	agent.EventMeta:        {class: classState | classStream, main: foldMeta, childIgnored: true},
	agent.EventUser:        {class: classStream, main: foldUser, child: childUser},
	agent.EventSubagent:    {class: classState, main: foldSubagent, anyAgent: true},
	agent.EventQueue:       {class: classState, main: foldQueue, childIgnored: true},
	agent.EventCommand:     {class: classStream, main: foldCommand, child: childCommand},
	agent.EventForeignTurn: {class: classMarker | classStream, main: foldForeignTurn, childIgnored: true},
	agent.EventReplay:      {class: classMarker | classStream, main: foldReplay, childIgnored: true},
	agent.EventTurn:        {class: classState | classStream, main: foldTurn, childIgnored: true},
}

// ------------------------------------------------------------ stream kinds

func foldText(m *Model, ev agent.Event)       { childText(m.Main, ev) }
func childText(t *Transcript, ev agent.Event) { t.appendStream(KindAssistant, ev.Text, ev.At) }

func foldThought(m *Model, ev agent.Event)       { childThought(m.Main, ev) }
func childThought(t *Transcript, ev agent.Event) { t.appendStream(KindThought, ev.Text, ev.At) }

func foldCommand(m *Model, ev agent.Event) { childCommand(m.Main, ev) }

// childCommand is the command line, stamped at the event's At, in the
// transcript the event names.
func childCommand(t *Transcript, ev agent.Event) {
	if ev.Command == nil {
		return
	}
	t.addCommandLine(ev.Command, t.model.stamp(ev.At))
}

// foldUser is the main session's user event. An interjection is the one user
// row drawn from what came back rather than from a send; a replayed prompt is
// one craze never sent; a live echo draws nothing, because the row is the
// started's (§3.2). The shell context in front of the text is wire content,
// never display content.
func foldUser(m *Model, ev agent.Event) {
	if ev.Interjection {
		_, text := agent.SplitShellContext(ev.Text)
		m.Main.addInterjection(text, m.stamp(ev.At))
		return
	}
	if m.replaying || ev.Replayed {
		_, text := agent.SplitShellContext(ev.Text)
		m.Main.addUser(text, m.stamp(ev.At))
	}
}

// childUser is a child's user event, which streams (today's rule): a prompt
// the parent handed the child arrives in chunks like any reply.
func childUser(t *Transcript, ev agent.Event) { t.appendStream(KindUser, ev.Text, ev.At) }

func foldError(m *Model, ev agent.Event) {
	if ev.Err == nil {
		return
	}
	m.Main.addErrValue(ev.Err, m.stamp(ev.At))
}

// ------------------------------------------------------------- tool, todos

func foldTool(m *Model, ev agent.Event) { childTool(m.Main, ev) }

func childTool(t *Transcript, ev agent.Event) {
	if ev.Tool == nil || ev.Tool.IsTodoTool() {
		return
	}
	t.upsertTool(ev.Tool, ev.At)
	t.model.fc.state = true
}

// foldTodos keeps the list (empty clears it) and writes the tasks notes under
// the main transcript's dedupe. Every EventTodos carries the full list, so the
// TUI's fallback to its snapshot for an empty one reads the same list.
func foldTodos(m *Model, ev agent.Event) {
	m.todos = nilIfEmpty(ev.Todos)
	m.Main.noteTodos(ev.Todos, ev.At)
	m.fc.state = true
}

// -------------------------------------------------------------------- asks

// foldPermission opens a permission ask. It is never Auto, so it always ends
// the run above it, at the opening's At.
func foldPermission(m *Model, ev agent.Event) {
	p := ev.Permission
	if p == nil {
		return
	}
	at := m.stamp(ev.At)
	m.Main.closeStream(at)
	m.openAsk(p.ID, agent.AskPermission, agent.AskBody{Permission: p}, at)
}

// foldQuestion opens a question ask; a non-Auto one ends the run (§3.3: on
// every client, whatever its caps or its cancel mask decide about a card).
func foldQuestion(m *Model, ev agent.Event) {
	q := ev.Question
	if q == nil {
		return
	}
	at := m.stamp(ev.At)
	if !q.Auto {
		m.Main.closeStream(at)
	}
	m.openAsk(q.ID, agent.AskQuestion, agent.AskBody{Question: q}, at)
}

// foldPlan opens a plan ask; a non-Auto plan ends the run and is transcript
// material: the plan entry.
func foldPlan(m *Model, ev agent.Event) {
	p := ev.Plan
	if p == nil {
		return
	}
	at := m.stamp(ev.At)
	if !p.Auto {
		m.Main.closeStream(at)
		m.Main.addPlan(p, at)
	}
	m.openAsk(p.ID, agent.AskPlan, agent.AskBody{Plan: p}, at)
}

// openAsk records an open ask, keyed by id: a second opening of the same id
// replaces the first where it stands. An opening with no id cannot be ended
// and is not kept.
func (m *Model) openAsk(id string, kind agent.AskKind, body agent.AskBody, at time.Time) {
	if id == "" {
		return
	}
	a := Ask{ID: id, Kind: kind, Body: body, At: at}
	m.fc.state = true
	for i := range m.asks {
		if m.asks[i].ID == id {
			m.asks[i] = a
			return
		}
	}
	m.asks = append(m.asks, a)
}

// foldAsk is an ask's ending: the ask leaves the open set and joins the
// last-ended list. No entry — the answer, skip and plan notes are written only
// by the client that had the card (§3.3).
func foldAsk(m *Model, ev agent.Event) {
	u := ev.Ask
	if u == nil {
		return
	}
	m.fc.state = true
	if i := slices.IndexFunc(m.asks, func(a Ask) bool { return a.ID == u.ID }); i >= 0 {
		m.asks = slices.Delete(m.asks, i, i+1)
	}
	if u.ID == "" {
		return
	}
	end := AskEnding{ID: u.ID, Kind: u.Kind, Outcome: u.Outcome, By: u.By, At: m.stamp(ev.At)}
	if i := slices.IndexFunc(m.ended, func(a AskEnding) bool { return a.ID == u.ID }); i >= 0 {
		m.ended[i] = end
		return
	}
	if len(m.ended) >= maxEnded {
		m.ended = slices.Delete(m.ended, 0, len(m.ended)-maxEnded+1)
	}
	m.ended = append(m.ended, end)
}

// ----------------------------------------------------------- markers, turn

// foldDone is the wire's own ending: it closes the run, and a cancelled one
// leaves the note. The engine's turn ending does not close a run
// (TestOnlyTheWiresEndingClosesAStreamRun).
func foldDone(m *Model, ev agent.Event) {
	at := m.stamp(ev.At)
	m.Main.closeStream(at)
	if ev.StopReason == stopCancelled {
		m.Main.addNote(NoteCancelled, at)
	}
	m.fc.state = true
}

// foldForeignTurn is either end of a turn the agent ran on its own. Either
// closes the run — a nil payload too, as the TUI's does — and the start heads
// what follows with a note.
func foldForeignTurn(m *Model, ev agent.Event) {
	at := m.stamp(ev.At)
	m.turn.Foreign = ev.ForeignTurn
	m.fc.state = true
	m.Main.closeStream(at)
	if ev.ForeignTurn != nil && ev.ForeignTurn.Running {
		m.Main.addNote(NoteForeignTurn, at)
	}
}

// foldReplay is a session/load replay bracket. Its end closes the last
// replayed run first, so the note does not land inside it. A phase that is
// neither is ignored.
func foldReplay(m *Model, ev agent.Event) {
	r := ev.Replay
	if r == nil {
		return
	}
	switch r.Phase {
	case agent.ReplayStart:
		m.replaying = true
		m.fc.state = true
	case agent.ReplayEnd:
		m.replaying = false
		m.fc.state = true
		at := m.stamp(ev.At)
		m.Main.closeStream(at)
		m.Main.addNote(NoteRestored, at)
	}
}

// foldTurn is one end of an engine-driven turn. A started is the turn's user
// row, whoever sent it (§3.2); an ended settles the turn it names, and the
// two synthetic endings the wire never reports leave their rows — a cancel's
// note, a refusal's error — whichever turn is current.
func foldTurn(m *Model, ev agent.Event) {
	tu := ev.Turn
	if tu == nil {
		return
	}
	switch tu.Phase {
	case agent.TurnStarted:
		at := m.stamp(ev.At)
		m.turn.ID, m.turn.Text, m.turn.Origin, m.turn.At = tu.ID, tu.Text, tu.Origin, at
		m.fc.state = true
		_, text := agent.SplitShellContext(tu.Text)
		m.Main.addUser(text, at)
	case agent.TurnEnded:
		if m.turn.ID == tu.ID {
			m.turn.ID = ""
		}
		m.fc.state = true
		failed := tu.Err != ""
		switch {
		case tu.Synthetic && !failed && tu.StopReason == stopCancelled:
			m.Main.addNote(NoteCancelled, m.stamp(ev.At))
		case tu.Synthetic && failed:
			m.Main.addError(tu.Err, m.stamp(ev.At))
		}
	}
}

// ------------------------------------------------------------------- queue

// foldQueue keeps the queue keyed by id: queued inserts at QueuePos unless the
// id is already there, when it replaces the row where it stands; edited
// replaces; removed and sent delete.
func foldQueue(m *Model, ev agent.Event) {
	q := ev.Queue
	if q == nil {
		return
	}
	i := slices.IndexFunc(m.queue, func(p agent.QueuedPrompt) bool { return p.ID == q.ID })
	switch ev.QueueChange {
	case agent.QueueQueued:
		if i >= 0 {
			m.queue[i] = *q
		} else {
			pos := min(max(ev.QueuePos, 0), len(m.queue))
			m.queue = slices.Insert(m.queue, pos, *q)
		}
	case agent.QueueEdited:
		if i >= 0 {
			m.queue[i] = *q
		}
	case agent.QueueRemoved, agent.QueueSent:
		if i >= 0 {
			m.queue = slices.Delete(m.queue, i, i+1)
		}
	default:
		return
	}
	m.fc.state = true
}

// ------------------------------------------------------------------ roster

// foldSubagent is one roster change: the row is upserted from the event, a
// finished child's run is closed at the event's At, and finished rows past
// Bounds.Agents are evicted oldest-finish first with their transcripts.
func foldSubagent(m *Model, ev agent.Event) {
	info := ev.Subagent
	if info == nil || info.ID == "" {
		return
	}
	id := info.ID
	// The TUI's applySubagentEvent ensures the child's transcript first.
	t := m.ensureSub(id)
	row, ok := m.agents[id]
	if !ok {
		m.agentOrder = append(m.agentOrder, id)
	}
	row.info = *info
	switch ev.SubagentChange {
	case agent.SubagentChangeSpawned:
		// A new attempt no longer holds a finish slot.
		row.finish = 0
	case agent.SubagentChangeFinished:
		// A finish is claimed once per run: a restated finish is not the
		// newest one (subagents.go stampFinishLocked).
		if row.finish == 0 {
			m.finishSeq++
			row.finish = m.finishSeq
		}
	}
	m.agents[id] = row
	m.fc.state = true
	if ev.SubagentChange == agent.SubagentChangeFinished {
		// The event's own At, and Options.Clock's fallback for an unstamped
		// one, like every other event-driven close (r1: the TUI's
		// applySubagentEvent carried the same fix).
		t.closeStream(m.stamp(ev.At))
		m.evictFinished(id)
	}
}

// evictFinished is the live session's roster rule, replicated exactly
// (internal/agent/subagents.go evictFinishedLocked, ~:525): the finished rows
// — every row whose status is neither running nor empty — past Bounds.Agents
// are dropped oldest FINISH first (EndedAt, then finish order), never keep,
// the row that just finished. A dropped row's child transcript goes with it.
func (m *Model) evictFinished(keep string) {
	var finished []string
	for _, id := range m.agentOrder {
		if s := m.agents[id].info.Status; s != agent.SubagentRunning && s != "" {
			finished = append(finished, id)
		}
	}
	if len(finished) <= m.bounds.Agents {
		return
	}
	slices.SortStableFunc(finished, func(x, y string) int {
		a, b := m.agents[x], m.agents[y]
		if !a.info.EndedAt.Equal(b.info.EndedAt) {
			if a.info.EndedAt.Before(b.info.EndedAt) {
				return -1
			}
			return 1
		}
		switch {
		case a.finish < b.finish:
			return -1
		case a.finish > b.finish:
			return 1
		}
		return 0
	})
	for len(finished) > m.bounds.Agents {
		drop := slices.IndexFunc(finished, func(id string) bool { return id != keep })
		if drop < 0 {
			return
		}
		id := finished[drop]
		finished = slices.Delete(finished, drop, drop+1)
		delete(m.agents, id)
		if i := slices.Index(m.agentOrder, id); i >= 0 {
			m.agentOrder = slices.Delete(m.agentOrder, i, i+1)
		}
		m.dropSub(id)
	}
}

// -------------------------------------------------------------------- meta

// deltaField is one field of agent.StateDelta: a section — a full replacement
// into Settings, nil meaning untouched — or a report, which is news and not
// mirrored state. TestFoldClassifiesEveryStateDeltaSection holds this table
// against the struct's fields in both directions.
type deltaField struct {
	name   string
	report bool
	apply  func(m *Model, st *agent.StateDelta, at time.Time)
}

// deltaFields is the section table, in the order a delta is applied: the
// sections, then the reports in the TUI's order (Detail's row, then
// IndexErr's).
var deltaFields = []deltaField{
	{name: "Title", apply: func(m *Model, st *agent.StateDelta, _ time.Time) {
		if st.Title != nil {
			m.settings.Title = *st.Title
		}
	}},
	{name: "Mode", apply: func(m *Model, st *agent.StateDelta, _ time.Time) {
		if st.Mode != nil {
			m.settings.Mode = *st.Mode
		}
	}},
	{name: "Model", apply: func(m *Model, st *agent.StateDelta, _ time.Time) {
		if st.Model != nil {
			m.settings.Model = *st.Model
		}
	}},
	{name: "Config", apply: func(m *Model, st *agent.StateDelta, _ time.Time) {
		if st.Config != nil {
			m.settings.Config = nilIfEmpty(st.Config.Options)
		}
	}},
	{name: "Commands", apply: func(m *Model, st *agent.StateDelta, _ time.Time) {
		if st.Commands != nil {
			m.settings.Commands = nilIfEmpty(st.Commands.Commands)
		}
	}},
	{name: "Plugins", apply: func(m *Model, st *agent.StateDelta, _ time.Time) {
		if st.Plugins != nil {
			m.settings.Plugins = nilIfEmpty(st.Plugins.Plugins)
		}
	}},
	{name: "SendNow", apply: func(m *Model, st *agent.StateDelta, _ time.Time) {
		if st.SendNow != nil {
			m.settings.SendNow = *st.SendNow
		}
	}},
	// Reason names what happened to a send-now; alone it draws nothing, and
	// its words are the client's toasts, never rows.
	{name: "Reason", report: true, apply: func(*Model, *agent.StateDelta, time.Time) {}},
	// Detail is the failure behind the engine's own failed cancel: a row.
	{name: "Detail", report: true, apply: func(m *Model, st *agent.StateDelta, at time.Time) {
		if st.Detail != "" {
			m.Main.addError(st.Detail, m.stamp(at))
		}
	}},
	// IndexErr is a session-index write that failed: a row.
	{name: "IndexErr", report: true, apply: func(m *Model, st *agent.StateDelta, at time.Time) {
		if st.IndexErr != "" {
			m.Main.addError(st.IndexErr, m.stamp(at))
		}
	}},
}

// foldMeta applies a state delta, section by section. Event.Mode and
// Event.Text — what an agent-initiated update fills beside its section — add
// nothing the sections do not carry; a Mode section inside a replay bracket is
// a replacement like any other (the fold retires nothing on a section).
func foldMeta(m *Model, ev agent.Event) {
	st := ev.State
	if st == nil {
		return
	}
	m.fc.state = true
	for i := range deltaFields {
		deltaFields[i].apply(m, st, ev.At)
	}
}

func nilIfEmpty[S ~[]E, E any](s S) S {
	if len(s) == 0 {
		return nil
	}
	return s
}
