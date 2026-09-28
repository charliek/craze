package tui

import (
	"reflect"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/transcript"
)

// The TUI on a restore (plan 027 §3.14; astra 16, CodeRabbit 6; PR 4, C27).
//
// A socket's stream (remote.Session) hands up three things the in-process
// primary never does: a Restore — the transcript as the host holds it, which
// replaces everything the model held, on the first attach and after every
// re-attach that could not go on from where it was — a Ready, when the host's
// start completes, and an End. The reader delivers each as its message
// (waitEvent), and the command gate places each as it places an event: held in
// arrival order while a gate is open or anything is held, drained one per
// Update, the reader reconciled once it is placed (gate.go).
//
// A restore is a full model initialisation, not only rows (applyRestore): the
// shared model is restored from the snapshot, every pane rebuilt from it, the
// cards raised for every open ask, the turn, the replay flag and the revision
// guards set from it, and everything this client laid over the stream it
// replaces — the overlays, the echo markers, the cancel mask, the plan offer —
// cleared. A restore held behind events of the stream it replaces drops them
// where they are held: its snapshot holds them already, or they are a session
// that is gone (dropSuperseded). An open gate is never released by a restore:
// the restore waits its turn behind the gate's reply, whose continuation runs
// as usual — a reply the resume loss made unknown reads as ErrNoAnswer
// (noAnswerFor) — and then the restore clears whatever that continuation
// installed, the optimistic rows among them, so nothing it drew can show twice.

// restoreMsg is the stream's Restore (backend.ItemRestore): the snapshot the
// model is initialised from, the session's facts as the attach reply carried
// them (the backend's Info already holds them), and the stream generation it
// opens.
type restoreMsg struct {
	info backend.SessionInfo
	snap *transcript.Snapshot
	gen  uint64
}

// readyMsg is the stream's Ready (backend.ItemReady): the host's start has
// completed — err is its failure, which Start answers too, as errMsg — and
// info the facts it published, which the backend's Info already holds.
type readyMsg struct {
	info backend.SessionInfo
	err  error
}

// endMsg is the stream's End (backend.ItemEnd): nothing more will come. err is
// why — nil for the session's own end on its host, the transport's failure
// otherwise.
type endMsg struct{ err error }

// reloadedNote is the one local row a restore draws, after every restore but
// the first: what the screen showed was replaced by the session's own account.
const reloadedNote = "transcript reloaded"

// applyRestore initialises the model from a restore (§3.14, astra 16): from the
// restored model's State and the restore item's own facts (r.info) — read in
// place of the backend's Info for every decision the restore makes, since a
// socket backend replaces its Info as it receives the next restore, which may
// already be on its way (infoPin; C27a). The next recompute after the restore
// reads the backend again.
//
// The first restore — the model held no incarnation — draws nothing of its own:
// attached before its session started, as the socket goldens attach, it
// restores an empty session and leaves every frame as it was. Every later one
// says so, in one local row (reloadedNote). A restore of another incarnation —
// the host restarted, the engine was replaced — is another session: the
// session generation moves, so every result still in flight for the old one is
// dropped where it lands (issued), and nothing keyed by the old incarnation's
// ids survives (its hidden answers waiting to be retried, the cards' progress,
// the sub-agent rows' first sightings). One of the same incarnation — a
// resumed client re-attached, a slow consumer reset — keeps the generation:
// its results are about this session still.
func (m *Model) applyRestore(r restoreMsg) {
	if r.snap == nil {
		return
	}
	info := r.info
	m.infoPin = &info
	defer func() { m.infoPin = nil }()
	held := ""
	if m.shared != nil {
		held = m.shared.Incarnation()
	}
	first := held == ""
	moved := !first && r.snap.Incarnation != held
	if moved {
		m.sessGen++
		// Nothing reads the flag but its own answer, which the generation
		// now drops.
		m.modeInFlight = ""
		// Ask ids are the incarnation's: a retry would answer another ask.
		m.hiddenRetry = nil
		m.agentStart, m.agentDone = nil, nil
	}
	if moved && m.queueEdit != "" {
		// Queue ids are the incarnation's (they start again at q-1): the row
		// this edit began from is gone with it, whatever the new queue holds
		// under its id, so the edit ends as one whose row left the queue does
		// (syncQueue). On the same incarnation the edit stands while its row
		// does — syncQueue ends it otherwise — and the version check keeps it
		// honest.
		m.cancelQueueEdit()
		m.note(editGoneNote)
	}
	if !first {
		// The foreign turn's episodes are counted from what this client
		// folded, and a restore may hold endings and openings it never folded:
		// the count starts again past every episode a reply still on its way
		// can name (foreignEpisode), so a late cancel's answer neither marks
		// nor silences an episode it was not about (applyForeignCancelled).
		m.foreignEnded = m.foreignEpisode() + 1
	}
	// A send-now waiting for its confirm, and the Ctrl+C window, belong to the
	// turn the model was looking at, which the restore replaces: they are
	// retired as that turn's own ending would have retired them
	// (applyTurnEnded).
	if m.confirm != nil {
		m.confirm = nil
		m.note("send now dropped")
	}
	m.ctrlCDeadline = time.Time{}
	priorTodos, priorCards := m.snap.Todos, m.cards

	// The fold, and every pane from it: all of its rows are the session's.
	in := &foldInputs{}
	m.foldIn = in
	m.shared = transcript.Restore(r.snap, sharedOptions(in.now, in.errorText))
	m.rebuildPanes()
	if restoreHook != nil {
		restoreHook(m, r.snap)
	}
	st := m.shared.State()
	// The pane's todo-note dedupe is the fold's again, since its rows are.
	main := m.shared.History().Main
	m.todoPlanned, m.todoDone = main.TodoPlanned, main.TodoDone

	// Everything this client laid over the stream the restore replaces.
	m.clearOverlays()
	m.modeRev, m.modelRev, m.configRev = st.Seq, st.Seq, st.Seq
	m.ownTurn, m.nextTurn, m.armedDraft, m.disarmed = "", "", "", ""
	m.askEchoes = nil
	m.cardMask, m.cardMasking = "", false
	m.planOfferSeq = 0
	m.lastThought = false
	m.sel = selection{}
	m.pruneAgentStamps(st.Agents)

	// The mirror, from the restored fold and the backend's facts.
	m.recompute()
	if !reflect.DeepEqual(priorTodos, m.snap.Todos) {
		// A list the model had not been shown is news, as its event would be.
		m.noteTodoLifecycle(m.snap.Todos)
	}
	m.restoreTurn(st.Turn, moved)
	m.restoreCards(st.Asks, priorCards, moved)
	m.restoreViewing()
	wasReplaying := m.replaying
	if st.Replaying || st.Seq > 0 {
		// A snapshot that folded anything says whether a replay is running.
		// One that folded nothing — cut before its host started — says
		// nothing about a load to come, and leaves a guard Config.Loading set
		// where it stands (the replay-start arm sets it too).
		m.replaying = st.Replaying
	}
	if wasReplaying && !m.replaying {
		// The replay's end is inside the snapshot, and will not come as an
		// event: this is where the session is up, if Start has returned.
		m.sessionUp()
	}
	if !first {
		m.addNote(reloadedNote)
	}
}

// rebuildPanes shows the restored model in every pane (pane.rebuild): the main
// one, and one per child transcript it holds. A pane for a child it does not
// hold goes — a receipt child's the view rebuilds from the roster if it is the
// one on screen (restoreViewing), and a later event makes one as it always has
// (ensureSub).
func (m *Model) rebuildPanes() {
	m.main.rebuild(m.shared.Main)
	for id := range m.subs {
		if m.shared.Sub(id) == nil {
			delete(m.subs, id)
		}
	}
	for _, id := range m.shared.Subs() {
		m.ensureSub(id).rebuild(m.shared.Sub(id))
	}
}

// pruneAgentStamps forgets the first sightings of the rows the restored roster
// does not hold: a row that comes back is sighted again (recompute).
func (m *Model) pruneAgentStamps(rows []agent.SubagentInfo) {
	for _, stamps := range []map[string]time.Time{m.agentStart, m.agentDone} {
		for id := range stamps {
			if _, ok := liveInfo(rows, id); !ok {
				delete(stamps, id)
			}
		}
	}
}

// restoreTurn is the restored turn, as the model's view of the session: a turn
// running is working, under the turn it names — whose id a cancel is asked
// against — and none is not. A turn the model was not looking at is a new
// turn's identity (beginTurn's rule: it retires the last turn's plan evidence
// and offer); the one it was looking at goes on as it was. Another incarnation
// is another session — its turn ids start again, so even the same id is a new
// turn — whose error and cancellation the old one's are not.
func (m *Model) restoreTurn(t transcript.Turn, moved bool) {
	if moved {
		// Another incarnation's turns are numbered afresh: its turn-1 is not
		// the one this model held, whatever the id says.
		if m.status == statusError {
			m.status, m.err = statusIdle, ""
		}
		m.cancelled = false
	}
	if t.ID != m.turnID || moved {
		m.turnSeq++
		m.turnID = t.ID
		if t.ID != "" {
			m.turnStart = t.At
			if m.turnStart.IsZero() {
				m.turnStart = m.now()
			}
			m.err = ""
			m.cancelled = false
		}
	}
	if t.ID != "" || t.Origin != "" {
		m.prompted = true
	}
	switch {
	case t.ID != "":
		m.status = statusWorking
	case m.status == statusWorking:
		m.status = statusIdle
	}
}

// restoreCards raises a card for every open ask the restored model holds, in
// the order they opened, as each ask's opening would have raised it (pushCard,
// with no cancel mask up): a permission always, a question or a plan unless it
// is automatic or the session's capabilities hide its kind. A hidden kind never
// had a card, so a restore raises none, and it answers none either: this
// client answers a hidden ask where it sees the opening, as every client
// folding the stream does, and an ask whose opening it did not see was answered
// by the clients that did. A card the model already had for the same ask of the
// same incarnation keeps its progress (the question on screen, the picks); a
// new one makes way as an arriving card does.
func (m *Model) restoreCards(asks []transcript.Ask, prior []card, moved bool) {
	var cards []card
	arrived := false
	for _, a := range asks {
		c, ok := m.cardFor(a)
		if !ok {
			continue
		}
		if old, had := findCard(prior, a.ID); had && !moved {
			old.truncated = a.Truncated
			c = old
		} else {
			arrived = true
		}
		cards = append(cards, c)
	}
	m.cards = cards
	if arrived {
		m.makeWayForCard()
	}
}

// cardFor is the card an open ask raises, and whether it raises one.
func (m *Model) cardFor(a transcript.Ask) (card, bool) {
	b := a.Body
	switch {
	case b.Permission != nil:
		return card{kind: cardPermission, perm: b.Permission, truncated: a.Truncated}, true
	case b.Question != nil && !b.Question.Auto && m.showAsk():
		return card{kind: cardQuestion, ask: b.Question, truncated: a.Truncated}, true
	case b.Plan != nil && !b.Plan.Auto && m.showPlan():
		return card{kind: cardPlan, plan: b.Plan, truncated: a.Truncated}, true
	}
	return card{}, false
}

// findCard is the card for ask id among cards.
func findCard(cards []card, id string) (card, bool) {
	for _, c := range cards {
		if cardAskID(c) == id {
			return c, true
		}
	}
	return card{}, false
}

// restoreViewing keeps the sub-agent view on a child the restored model still
// knows — rebuilt from the roster when its provider streams no child
// transcript — and leaves it for the main transcript when it knows the child no
// more.
func (m *Model) restoreViewing() {
	id := m.viewing
	if id == "" {
		return
	}
	_, rostered := liveInfo(m.snap.Subagents, id)
	switch {
	case m.shared.Sub(id) == nil && !rostered:
		m.viewing, m.tombstone = "", nil
		m.setViewportContent(true)
	case !m.showSubagentTranscript():
		m.rebuildReceiptTranscript(id)
	}
}

// dropSuperseded takes out of the held queue every message of a stream older
// than gen — the events of the attachment a restore of generation gen
// replaces, and a restore of an older generation still waiting — as that
// restore is held behind them (hold): its snapshot holds those events already,
// or they are a session that is gone, so they are never applied. Keys, ticks,
// replies and every other message stay, in order.
func (m *Model) dropSuperseded(gen uint64) {
	var kept []heldMsg
	dropped := false
	for _, h := range m.held {
		switch msg := h.msg.(type) {
		case eventMsg:
			if msg.gen < gen {
				dropped = true
				m.heldBytes -= h.bytes
				continue
			}
		case restoreMsg:
			if msg.gen < gen {
				dropped = true
				m.heldBytes -= h.bytes
				continue
			}
		}
		kept = append(kept, h)
	}
	if dropped {
		// A fresh array: the drained slots of the old one are not carried.
		m.held, m.heldDrained = kept, 0
	}
}
