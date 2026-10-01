package tui

import (
	"maps"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/backend"
)

// Opening a session in place (plan 030 §3.11): the TUI leaves the session it
// shows for another one, served by another host, without quitting — the
// session list's enter (sessions_list.go). It is its own operation
// (switchBackend), not the pickers' path: those replace a session before any
// of its stream is read, and a switch replaces one whose stream is being read,
// whose gated calls and commands may still be answering, and whose host is
// still serving it.
//
//   - Every message the old backend's own closures produce — each stream item
//     the reader hands up, the start's answer — carries the backend generation
//     it was produced under (Model.bgen), and the command gate drops one of
//     another generation before it does anything else (gated): it clears no
//     reader's flag, quits nothing, starts and fails nothing.
//   - The old backend is closed off the Update (retire): over a socket that is
//     a view close — a detach, the session going on on its host — which ends
//     its stream, so a read of it still in flight ends with nothing, or with
//     an item it had already taken, whose message the gate drops.
//   - Everything the model held about a session is made again by the one
//     constructor New makes it with (withSession): the fold and the panes, the
//     cards, the overlays, the sub-agent view, the gate, the confirm line, the
//     popups, the lifecycle flags, the reader's flag, the timestamps, the
//     workspace's git and skills roots, the session's facts — so the new
//     backend's first restore is a first restore (restore.go) and nothing of
//     the last session is drawn over the next. What is the TUI's stays: the
//     theme, the size, the config, the list and its grouping, the drafts, and
//     the monotonic counters — bgen, shownGen, sessGen and gateSeq — so no
//     generation or gate id is ever issued twice (R2-5).
//   - A command's reply or a gate's timeout issued for the old session is
//     rejected by the gate before any of its bookkeeping (gated).
//   - What this terminal was asked to do for the old session and answers
//     after the switch carries the shown-session generation it was asked
//     under (Model.shownGen; C11r, astra r22-c11 1–2), which only a switch
//     moves — so work asked for before the old session's backend was even
//     adopted is the old session's too (C11r2, astra r24-fix1112): a paste,
//     which would otherwise land in the next session's composer, and a
//     copy's note are dropped by the gate as the old stream's items are; the
//     composer's shell's completion never writes its command and output into
//     the next session's transcript (finishShell).
//   - The dial is a tea.Cmd (Sessions.Open, from the list); an answer that
//     lands after the user has moved on — another session opened, the list
//     left, craze quitting — is closed and never adopted (sessOpened).

// sessionSeed is what a session's own state starts from: what New takes from
// its Config, and a switch from the backend it adopts.
type sessionSeed struct {
	// workspace is the session's working directory: the status row's name,
	// the repository its branch is read from, the skills' root and the
	// composer's shell's directory.
	workspace string
	// model is the model the status row names before the session says
	// (Config.Model): New's alone — a switch names none.
	model string
	// provider is the provider id the session runs (sessProvider), which a
	// host status names.
	provider string
	// loading says the session was built to load a transcript, and is
	// replaying until its replay ends (Config.Loading; a switch to a saved
	// session, plan 030 §3.12).
	loading bool
	// indexTitle is the session index's title for the session, as the list's
	// row it was opened from had it (Model.indexTitle): a switch's alone —
	// New's session is named by the list once it lists it (trackHere).
	indexTitle string
}

// withSession is the one constructor of a session's state (plan 030 §3.11),
// New's and switchBackend's alike: m's per-TUI half, carried as it is, with
// the per-session half as a session starts from seed — everything else of it
// the zero value, as New has always left it. What is the TUI's and what is the
// session's is decided here, field by field, once: a field added to Model is
// the session's (and so made afresh by every switch) unless it is carried
// below (TestTheSessionConstructorDecidesEveryField holds the two lists).
//
// It builds no backend and adopts none: the caller does (setSession,
// setBackend), which also moves the session generation and the backend
// generation.
func (m Model) withSession(seed sessionSeed) Model {
	s := Model{
		// ---- The TUI's: kept across every session it shows.
		theme:  m.theme,
		vp:     m.vp,
		input:  m.input,
		yolo:   m.yolo,
		width:  m.width,
		height: m.height,
		ready:  m.ready,
		// Ctrl+O's detail toggle is a preference of the user's, like the
		// theme, not a fact of the session.
		expanded:     m.expanded,
		quitting:     m.quitting,
		mouseEnabled: m.mouseEnabled,
		lay:          m.lay,
		layouts:      m.layouts,
		// The terminal's motion mode and the timers in flight are the
		// terminal's and the program's: a switch changes neither.
		mouseAll:        m.mouseAll,
		tickGen:         m.tickGen,
		tickLive:        m.tickLive,
		tickFast:        m.tickFast,
		spinFrame:       m.spinFrame,
		hiddenRetryLive: m.hiddenRetryLive,
		clock:           m.clock,
		// A test's oracle (paintEveryEvent) paints every session it shows
		// the same way.
		paintEveryEvent: m.paintEveryEvent,
		frozen:          m.frozen,
		frameStart:      m.frameStart,
		terminalTitle:   m.terminalTitle,
		lastTitle:       m.lastTitle,
		term:            m.term,
		shell:           m.shell,
		owner:           m.owner,
		exit:            m.exit,
		host:            m.host,
		lastHost:        m.lastHost,
		viewer:          m.viewer,
		// The config: the in-process path's hooks and rows, the pickers'
		// closures, the launch flow's spawns and the session list.
		sessionIndex:    m.sessionIndex,
		crazeID:         m.crazeID,
		resume:          m.resume,
		loadSession:     m.loadSession,
		claimSession:    m.claimSession,
		refuseLoad:      m.refuseLoad,
		onEngine:        m.onEngine,
		providerLocked:  m.providerLocked,
		persistProvider: m.persistProvider,
		fallbackDefault: m.fallbackDefault,
		pickedExplicit:  m.pickedExplicit,
		providerDefault: m.providerDefault,
		providers:       m.providers,
		newSession:      m.newSession,
		spawnNew:        m.spawnNew,
		spawnLoad:       m.spawnLoad,
		cont:            m.cont,
		sessions:        m.sessions,
		sessList:        m.sessList,
		sessRosters:     m.sessRosters,
		sessPick:        m.sessPick,
		bandOn:          m.bandOn,
		drafts:          m.drafts,
		retired:         m.retired,
		completeLoads:   m.completeLoads,
		// Where /connect and the connect row read and write (plan 031
		// §3.9): the TUI's own native directory and environment, whichever
		// session it shows.
		nativeDir: m.nativeDir,
		nativeEnv: m.nativeEnv,
		// The composer's `@` popup follows the draft, which is the TUI's
		// (input): a switch to another session closes it first
		// (switchBackend, openUnstarted), and the first adoption of the
		// session shown keeps it open over the same draft (X121).
		composerAt: m.composerAt,
		// The counters that only ever move forward, across every session:
		// no generation, gate id, stamp, restore or turn count is reused.
		sessGen:       m.sessGen,
		bgen:          m.bgen,
		shownGen:      m.shownGen,
		gateSeq:       m.gateSeq,
		resumeAttempt: m.resumeAttempt,
		spawnSeq:      m.spawnSeq,
		unstartedSeq:  m.unstartedSeq,
		connSeq:       m.connSeq,
		restores:      m.restores,
		turnStarts:    m.turnStarts,
		// The gate's queue holds the TUI's messages as well as the session's
		// — keys, the frame harness's tokens, the list's own — in arrival
		// order; a switch takes the old stream's items out of it
		// (dropStaleHeld) and keeps the rest. The harness's own tokens and
		// the gate's mode are the harness's.
		held:        m.held,
		heldBytes:   m.heldBytes,
		heldDrained: m.heldDrained,
		syncAck:     m.syncAck,
		syncPending: m.syncPending,
		gateSync:    m.gateSync,
		harnessQuit: m.harnessQuit,

		// ---- The session's: as it starts.
		cwd:          seed.workspace,
		model:        seed.model,
		sessProvider: seed.provider,
		indexTitle:   seed.indexTitle,
		// A load is replaying before its first event: see Model.replaying.
		replaying: seed.loading,
		queueHov:  noHover(),
		// Allocated here, not on first use, so every copy of this model holds
		// the same panes from the start (see pane).
		main: &pane{},
		subs: make(map[string]*pane),
		// Turn 1 is the session before the first prompt: every event has an
		// identity from the start, and no engine turn carries it.
		turnSeq: 1,
	}
	s.git = discoverGit(seed.workspace)
	s.branch = s.git.branch()
	return s
}

// switchBackend opens b in place of the session the model shows (plan 030
// §3.11): the list's enter, once its dial has answered (sessOpened). The
// composer's draft is stashed under the session it was written for, the list
// closes — its roster with it — and the model is made again around b by the
// shared constructor (withSession): a new shown-session generation (shownGen),
// adopted as a Config.Backend is (setBackend: a new session generation, a new
// backend generation), started and read as Init starts and reads one, its own
// draft put back. The backend it replaces is closed off the Update (retire): a
// view close, the session going on on its host.
//
// loading says b loads a saved session (§3.12): the model starts replaying,
// as the resume picker's choice and Config.Loading start it — `restoring…`
// until the replay's end, which a restore or the stream's replay event says
// (a held session already up says so in its first restore). indexTitle is
// the index's title the list's row had for it, which the band names it by
// while it names none of its own (sessTitle).
func (m Model) switchBackend(b backend.Backend, loading bool, indexTitle string) (Model, tea.Cmd) {
	old := m.eng
	m.stashDraft()
	roster := m.sessList.roster
	// The list's input goes with the list: its popup's load, if one runs, is
	// cancelled (its answer is left behind by shownGen as well). So does the
	// composer's `@` popup, which was the session left's (plan 030 §3.16).
	m.sessList.in.closePopups()
	m.composerAt.close()
	m.sessList = sessListState{gen: m.sessList.gen, byDir: m.sessList.byDir}
	info := b.Info()
	ws := info.Workspace
	if ws == "" {
		// A backend that names no workspace changes none (followWorkspace).
		ws = m.cwd
	}
	next := m.withSession(sessionSeed{workspace: ws, provider: info.Provider, loading: loading, indexTitle: indexTitle})
	// Another session is shown from here (shownGen): what this terminal was
	// asked to do for the last one — a paste, a copy's note, a command's
	// completion — is the last one's, even when it was asked for before that
	// session's backend was adopted, or when none ever was (a launch's spawn
	// the switch abandons). Before dropStaleHeld, which judges by it.
	next.shownGen++
	next.setBackend(b)
	next.dropStaleHeld()
	next.takeDraft()
	_ = next.input.Focus()
	next.recompute()
	if next.model == "" && next.snap.CurrentModel != "" {
		next.model = next.snap.CurrentModel
	}
	if next.model == "" {
		next.model = "default"
	}
	// The pane is new and empty; the viewport still holds the last one's
	// rows until it is painted.
	next.setViewportContent(true)
	// Init's batch, for the backend Init never saw: the read it arms is the
	// one the command gate's reader rule counts (readOn) — or none yet, while
	// held messages drain, whose last one arms it.
	return next, tea.Batch(next.retire(old), next.sessRosters.closeCmd(roster), next.startCmd(), next.readOn())
}

// dropStaleHeld takes out of the held queue every item of a stream the model
// has left, every start's answer, and every paste and copy's note asked for
// in the session it no longer shows: a switch adopted a backend none of them
// is about, and each would be dropped where it drained (leftBehind). Keys,
// ticks, the frame harness's tokens, the list's messages and every other
// message stay, in order; a result issued for the old session is dropped
// where it drains (outdated), and so is a shell's completion (finishShell).
func (m *Model) dropStaleHeld() {
	var kept []heldMsg
	bytes := 0
	dropped := false
	for _, h := range m.held {
		if m.leftBehind(h.msg) {
			dropped = true
			continue
		}
		kept = append(kept, h)
		bytes += h.bytes
	}
	if dropped {
		// A fresh array: the drained slots of the old one are not carried.
		m.held, m.heldBytes, m.heldDrained = kept, bytes, 0
	}
}

// ------------------------------------------------------------------- drafts

// draftKey is the stash key of the session the model shows: its craze id,
// "" while its host has not named it. An unstarted session (§3.13,
// dispatch.go) keys its draft by a temporary id of its own until its host
// answers, and the stash moves to the real id then (moveDraft,
// adoptUnstarted): this is where that key is decided.
func (m Model) draftKey() string {
	if u := m.unstarted; u != nil {
		return u.draft
	}
	if m.eng == nil {
		return ""
	}
	return m.info().CrazeSessionID
}

// moveDraft moves a draft stashed under from to to: an unstarted session's,
// from its temporary id to the craze id its host answered with. Nothing
// stashed under from moves nothing.
func (m *Model) moveDraft(from, to string) {
	text, ok := m.drafts[from]
	if !ok || from == to {
		return
	}
	d := maps.Clone(m.drafts)
	delete(d, from)
	d[to] = text
	m.drafts = d
}

// stashDraft puts the composer's text away under the session it was written
// for (draftKey), as a switch leaves it; an empty composer leaves nothing
// stashed. A session with no name yet keeps no draft: nothing could find it
// again.
func (m *Model) stashDraft() {
	key := m.draftKey()
	if key == "" {
		return
	}
	d := maps.Clone(m.drafts)
	if d == nil {
		d = map[string]string{}
	}
	if text := m.input.Value(); text != "" {
		d[key] = text
	} else {
		delete(d, key)
	}
	m.drafts = d
}

// takeDraft puts the draft stashed for the session the model now shows back
// in the composer — and takes it out of the stash, since it is the
// composer's again — or empties the composer when none is: a draft never
// follows the user to another session.
func (m *Model) takeDraft() {
	key := m.draftKey()
	text, ok := m.drafts[key]
	if key != "" && ok {
		d := maps.Clone(m.drafts)
		delete(d, key)
		m.drafts = d
	}
	m.input.SetValue(text)
}

// --------------------------------------------------------- retired backends

// retire closes b off the Update (a close may wait on its host: a socket's
// detach is bounded in seconds), recorded meanwhile in the set every copy of
// the model shares, so an exit whose program stops before the close runs
// still closes it (finishRun). nil is nothing to close.
func (m Model) retire(b backend.Backend) tea.Cmd {
	if b == nil {
		return nil
	}
	set := m.retired
	set.add(b)
	return func() tea.Msg {
		set.close(b)
		return nil
	}
}

// backendSet is every backend the model has let go of and not yet closed
// (Model.retired), shared by every copy as sessRosterSet is. Nil only in a
// zero Model a test built, which every method allows.
type backendSet struct {
	mu   sync.Mutex
	open []backend.Backend
}

func (s *backendSet) add(b backend.Backend) {
	if s == nil || b == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open = append(s.open, b)
}

// close closes b and forgets it. The lock is not held across Close, which
// may wait out a detach.
func (s *backendSet) close(b backend.Backend) {
	if b == nil {
		return
	}
	_ = b.Close()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, o := range s.open {
		if o == b {
			s.open = append(s.open[:i:i], s.open[i+1:]...)
			return
		}
	}
}

// closeAll closes every backend still here, side by side — each close is
// bounded on its own (a socket's detach) — and returns once all have:
// finishRun's, and the frame runner's, on every exit path. Close is
// idempotent on every backend, so one a retire is closing at the same time
// is closed once.
func (s *backendSet) closeAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	open := s.open
	s.open = nil
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, b := range open {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.Close()
		}()
	}
	wg.Wait()
}
