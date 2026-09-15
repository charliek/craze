package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// ctrlCWindow is how long a Ctrl+C that cancelled a turn stays armed; a second
// press inside it quits.
const ctrlCWindow = time.Second

// stopCancelled is the one stop reason the TUI reads. internal/tui never
// imports internal/acp, so the string is spelled here.
const stopCancelled = "cancelled"

// foreignTurnNote heads the stream of a turn the agent started on its own.
const foreignTurnNote = "agent continued on its own (interjection fallback)"

// wheelLines is how far one wheel notch scrolls the transcript.
const wheelLines = 3

type status int

const (
	statusIdle status = iota
	statusWorking
	statusError
)

func (s status) String() string {
	switch s {
	case statusWorking:
		return "working"
	case statusError:
		return "error"
	default:
		return "idle"
	}
}

type Config struct {
	Session   agent.Session
	Theme     string
	Workspace string
	Model     string
	Yolo      bool
	// NoMouse turns mouse reporting off, which gives the terminal its native
	// drag-select back.
	NoMouse bool
	// Provider is the resolved default the startup picker preselects.
	Provider agent.Provider
	// ProviderLocked skips the picker: an explicit --provider, or the frame
	// runner. The session is constructed immediately.
	ProviderLocked bool
	// PersistProvider writes the provider id after a successful Start.
	PersistProvider bool
	// FallbackDefault is an unknown env/config id that resolved to cursor.
	// Esc on the picker must not persist that automatic fallback; Enter on a
	// row is a real choice and is saved.
	FallbackDefault bool
	// NewSession constructs a session for the chosen provider. The TUI calls
	// it after the picker (or immediately when locked). Tests that pass
	// Session and leave this nil never show the picker.
	NewSession func(agent.Provider) agent.Session
}

type Model struct {
	theme Theme
	vp    viewport.Model
	input textarea.Model

	sess   agent.Session
	cwd    string
	model  string
	yolo   bool
	status status
	err    string
	// startErr is the session that never came up. It is the only error that
	// reaches craze's exit status: Run returns it once the program is over.
	startErr error

	// git is found once, at start; branch is re-read when a turn ends.
	git    gitInfo
	branch string
	// sessStart is when Start returned, which is what the status row's
	// elapsed counts from.
	sessStart time.Time

	// main is the session transcript. cur() returns the viewed sub-agent
	// transcript when viewing != "", otherwise main.
	main      transcript
	subs      map[string]*transcript
	viewing   string
	tombstone *agent.SubagentInfo

	expanded bool
	width    int
	height   int
	ready    bool
	quitting bool
	started  bool

	// mouseEnabled is --no-mouse kept on the model: the flag decides what
	// bubbletea reports, and this decides what craze does with a mouse message
	// that reached it anyway (the frame runner injects them).
	mouseEnabled bool

	// sel is the drag in progress or the highlight it left; pressed is the
	// button that went down, because an X10 terminal reports ButtonNone on the
	// release that ends it.
	sel     selection
	pressed tea.MouseButton
	// The double-click state machine: the last press's cell and time, and how
	// many presses have landed on it inside the window.
	clickPos cellPos
	clickAt  time.Time
	clicks   int

	// copyNote is what the last copy did, shown in status row 2 until
	// copyUntil.
	copyNote  string
	copyUntil time.Time

	// lay is the one layout computation per Update; View and the mouse
	// hit-tester both read it rather than measuring anything themselves.
	// layouts counts the computations relayout made, so a test can hold
	// §3.1's "computed once per Update".
	lay     frameLayout
	layouts int

	// cards is the blocking-request queue (§3.11): permission, question and
	// plan requests in arrival order. Only the head is drawn.
	//
	// cardsCancelled holds from a cancel until the turn ends: the session has
	// answered everything it was holding and will not park another request for
	// this turn, so a card event still in flight must not raise a card.
	cards          []card
	cardsCancelled bool
	snap           agent.Snapshot

	// dialog is the modal layer: at most one is up, drawn over the transcript
	// region and hit-tested before any band. mdlg is the model dialog's own
	// state, helpTop the help box's scroll position.
	dialog  dialogKind
	mdlg    modelDialog
	helpTop int
	// applyGen counts the model dialog's applies. The box closes optimistically,
	// so a second apply can be under way before the first one answers, and only
	// the newest one is allowed to settle what the rows show.
	applyGen int

	// Theme dialog: the list is frozen when it opens because the live preview
	// changes the current theme on every move, and themePrev is the theme Esc
	// (or a click outside, or an arriving card) puts back.
	themeSel   int
	themeNames []string
	themePrev  Theme

	// The slash menu. slashSel is the highlighted row and slashTop the first
	// one drawn, both indices into filteredSlash(); slashKey is the token they
	// belong to, so the selection restarts when the token changes; and
	// slashHideKey is the token Esc hid, so the menu comes back as soon as the
	// token under the cursor is another one. A flag could not tell those apart.
	slashSel     int
	slashTop     int
	slashKey     string
	slashHideKey string
	skills       []slashItem

	pickingProvider bool
	providerLocked  bool
	persistProvider bool
	fallbackDefault bool
	pickedExplicit  bool
	providerCursor  int
	providerDefault agent.Provider
	newSession      func(agent.Provider) agent.Session

	todoPlanned int
	todoDone    bool

	// Agent rows: the selection is held by sub-agent id so it survives a row
	// leaving the band (finish, linger, eviction) above or below it.
	agentSel   int
	agentID    string
	agentStart map[string]time.Time
	agentDone  map[string]time.Time
	// agentFocus is true while ↑/↓ have moved the keyboard from the composer
	// to the rows: the selected row carries the gutter mark and Enter opens
	// it. Any other key hands the keyboard back to the composer.
	agentFocus bool

	// The queue band. The selection is held by id as the agent rows' is, so
	// it survives a row leaving the band above it; queueHov is the pointer's
	// row and the button under it, and exists only while the queue does.
	queueSel   int
	queueID    string
	queueFocus bool
	queueHov   queueHover
	// queueEdit is the row being edited in place, "" when none.
	// queueEditPos is its position, for the chip; editDraft is the composer
	// the edit displaced and Esc puts back.
	queueEdit    string
	queueEditPos int
	editDraft    string
	// confirm is the send-now waiting for an answer; strong is the one that
	// was confirmed and is waiting for the cancelled turn to settle. There is
	// at most one of each, and never two at once.
	confirm *strongSend
	strong  *strongSend
	// mouseAll records which motion mode the terminal is in, so the queue
	// going 0 → 1 rows and back issues exactly one transition each way.
	mouseAll bool

	tasksState    tasksPanelState
	todosSeen     bool
	todosClosedAt time.Time

	// turnSeq identifies the turn. It starts at 1 — the session before the
	// first prompt is a turn too, so every event has an identity — and is
	// bumped on every turn start, and every piece of plan-offer state is
	// recorded against it. A turn start therefore retires the last turn's
	// evidence, offer and kill without clearing a single flag, and nothing a
	// finished turn left behind can arm anything for the turn after it.
	turnSeq int
	// sawAssistantSeq is the turn that said something implementable: a turn
	// that only thought, only ran tools or only sent empty chunks left no plan
	// behind. planOfferSeq is the turn whose ending armed the offer, and
	// planDeadSeq the turn whose offer an action has killed — which is what
	// stops a late EventDone re-arming an offer Esc, /clear or a card retired.
	sawAssistantSeq int
	planOfferSeq    int
	planDeadSeq     int
	// promptEndSeq and streamEndSeq are the turn whose Prompt returned and the
	// turn whose event stream closed. The two race, so a turn is only over —
	// and the status only idle — once both have landed for the same turn. That
	// is also what keeps the next turn from starting while this one's buffered
	// events are still draining, which is what would let a late chunk count as
	// the next turn's evidence.
	promptEndSeq int
	streamEndSeq int

	tickGen     int
	tickLive    bool
	tickFast    bool
	spinFrame   int
	turnStart   time.Time
	lastThought bool

	ctrlCDeadline time.Time
	clock         func() time.Time
	// frozen is the frame runner's --freeze: the elapsed counters read zero
	// and the spinner does not cycle, so a golden of a turn in progress is
	// not a race against wall time. Nothing else about the model changes —
	// the clock still moves, so double-clicks, the Ctrl+C window and the
	// lingers behave exactly as they do in a live session.
	frozen bool
}

// now reads the clock through an indirection so tests can inject one.
func (m Model) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

type eventMsg struct{ ev agent.Event }
type startedMsg struct{}
type promptDoneMsg struct {
	res agent.Result
	err error
}
type errMsg struct{ err error }
type actionErrMsg struct{ err error }
type revertModeMsg struct {
	prev string
	err  error
}
type revertModelMsg struct {
	prev string
	err  error
}
type refreshSnapMsg struct{}

// dblClickMsg is the frame runner's <dblclick:X,Y>: the gesture without the
// two presses and the 400 ms between them.
type dblClickMsg struct{ X, Y int }

// planImplementMsg says the mode change the plan offer asked for landed, so
// the implement turn may now be written and sent. planImplementFailedMsg is
// the other half: nothing was sent, so the offer survives.
//
// Both carry the turn they were asked for. A SetMode that comes back after the
// user has already started a turn of their own is answering for a turn that no
// longer exists: writing its note and its user entry then would put a prompt in
// the transcript that the session rejects as already in flight.
type planImplementMsg struct {
	seq  int
	mode string
}
type planImplementFailedMsg struct {
	seq  int
	prev string
	err  error
}

func New(cfg Config) Model {
	cwd := cfg.Workspace
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}

	vp := viewport.New(0, 0)
	vp.KeyMap = viewport.KeyMap{
		PageUp:   key.NewBinding(key.WithKeys("pgup")),
		PageDown: key.NewBinding(key.WithKeys("pgdown")),
	}

	th := Preset(cfg.Theme)
	prov := cfg.Provider
	if prov.Name() == "" {
		prov = agent.CursorProvider()
	}
	m := Model{
		theme:           th,
		queueHov:        noHover(),
		vp:              vp,
		input:           newComposer(th),
		sess:            cfg.Session,
		cwd:             cwd,
		model:           cfg.Model,
		yolo:            cfg.Yolo,
		mouseEnabled:    !cfg.NoMouse,
		providerLocked:  cfg.ProviderLocked,
		persistProvider: cfg.PersistProvider,
		fallbackDefault: cfg.FallbackDefault,
		providerDefault: prov,
		newSession:      cfg.NewSession,
		// Turn 1 is the session before the first prompt, and it is over before
		// it starts: nothing is in flight, so both of its endings have landed.
		turnSeq:      1,
		promptEndSeq: 1,
		streamEndSeq: 1,
	}
	m.git = discoverGit(cwd)
	m.branch = m.git.branch()
	if m.newSession != nil && !m.providerLocked {
		m.pickingProvider = true
		m.dialog = dialogProvider
		m.providerCursor = providerIndex(prov)
	} else {
		if m.sess == nil && m.newSession != nil {
			m.sess = m.newSession(prov)
		}
		if m.sess == nil {
			m.sess = NewStub()
		}
	}
	m.refreshSnap()
	if m.model == "" && m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
	}
	if m.model == "" {
		m.model = "default"
	}
	return m
}

func Run(cfg Config) error {
	m := New(cfg)
	// One writer for the whole session: bubbletea's frames and the OSC 52 copy
	// are written from different goroutines, and a copy landing inside a frame
	// would tear it, so both go through the same lock.
	out := newSyncWriter(os.Stdout)
	prevOut := setClipboardOut(out)
	defer setClipboardOut(prevOut)
	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithOutput(out)}
	if !cfg.NoMouse {
		// Cell motion, not all motion: the quieter mode reports a drag once per
		// cell rather than per pixel, which is all the selection needs.
		opts = append(opts, tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(m, opts...)
	final, err := p.Run()
	var sess agent.Session
	var startErr error
	if fm, ok := final.(Model); ok {
		sess = fm.sess
		startErr = fm.startErr
	}
	if sess == nil {
		sess = m.sess
	}
	if sess != nil {
		_ = sess.Close()
	}
	if err != nil {
		return err
	}
	// A quit is clean unless the session never started. Only startCmd's
	// failure counts: an error mid-session leaves a usable craze, and quitting
	// out of one is a normal exit.
	return startErr
}

func (m Model) Init() tea.Cmd {
	if m.pickingProvider {
		return nil
	}
	return m.startCmd()
}

func (m Model) startCmd() tea.Cmd {
	sess := m.sess
	if sess == nil {
		return func() tea.Msg {
			return errMsg{fmt.Errorf("craze: no session")}
		}
	}
	return func() tea.Msg {
		if err := sess.Start(context.Background()); err != nil {
			return errMsg{err}
		}
		return startedMsg{}
	}
}

// Update runs the handler and then lays the frame out exactly once, from the
// state the handler left behind, and keeps the single tick chain alive.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	tm, cmd := m.update(msg)
	next, ok := tm.(Model)
	if !ok {
		return tm, cmd
	}
	// Mutation only marks the transcript dirty. Paint the drawn one here so a
	// background transcript (U3b) never moves m.vp.
	if next.cur().dirty {
		next.refreshViewport()
	}
	// The handler has already decided where the transcript sits: sticking now
	// only follows it down when the chrome above it changed shape.
	next.relayout(next.vp.Height == 0 || next.vp.AtBottom())
	next.storeViewport(next.cur())
	// The queue going 0 → 1 rows and back is the only thing that changes the
	// terminal's motion mode, and it is decided from the state the handler
	// left behind, once per Update.
	if mouse := next.queueMouseCmd(); mouse != nil {
		cmd = tea.Batch(cmd, mouse)
	}
	// The tick chain is batched last, so a test can run the handler's own
	// command without waiting out a timer.
	if tick := next.armTick(); tick != nil {
		cmd = tea.Batch(cmd, tick)
	}
	return next, cmd
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Stickiness is decided from where the user was before the resize; the
		// layout itself is left to the one relayout the Update wrapper runs.
		stick := !m.ready || m.vp.Height == 0 || m.vp.AtBottom()
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		m.setViewportContent(stick)
		return m, nil

	case tickMsg:
		m.handleTick(msg)
		return m, nil

	case startedMsg:
		m.started = true
		m.status = statusIdle
		m.sessStart = m.now()
		m.branch = m.git.branch()
		m.refreshSnap()
		if m.persistProvider && (!m.fallbackDefault || m.pickedExplicit) {
			name := m.snap.Provider.Name
			if name == "" {
				name = agent.CursorProvider().Name()
			}
			if err := SaveProvider(name); err != nil {
				m.addError(err.Error())
			}
		}
		m.rescanSkills()
		return m, waitEvent(m.sess)

	case errMsg:
		// errMsg is only ever startCmd's: the session never came up. The TUI
		// stays on screen so the error is readable, but craze must not exit 0
		// afterwards, so the failure rides out on the final model.
		m.status = statusError
		m.err = msg.err.Error()
		m.startErr = msg.err
		m.addError(m.err)
		return m, nil

	case actionErrMsg:
		m.addError(msg.err.Error())
		return m, nil

	case revertModeMsg:
		m.snap.CurrentMode = msg.prev
		m.addError(msg.err.Error())
		return m, nil

	case revertModelMsg:
		m.snap.CurrentModel = msg.prev
		m.model = msg.prev
		m.addError(msg.err.Error())
		return m, nil

	case modelApplyMsg:
		// The steps that landed are the truth; the one that did not is named.
		for _, st := range msg.done {
			m.addNote(st.note)
		}
		if msg.err != nil {
			m.addError(msg.step + ": " + msg.err.Error())
		}
		if msg.gen == m.applyGen {
			// Only the newest apply settles the rows: the snapshot is re-read
			// so they show what the agent has, and the steps this apply landed
			// are written over it. An older apply is not allowed to speak —
			// its failure would undo a value the user has changed since.
			m.refreshSnap()
			for _, st := range msg.done {
				m = m.settleStep(st)
			}
		}
		return m, nil

	case refreshSnapMsg:
		m.refreshSnap()
		return m, nil

	case cancelFailedMsg:
		// The turn it belonged to may already be over; only the armed send
		// that was waiting on this cancel is affected.
		if msg.seq == m.turnSeq {
			m.dropStrongSend("cancel failed")
		}
		m.addError(msg.err.Error())
		return m, nil

	case clipboardDoneMsg:
		m.copyNote = msg.note
		m.copyUntil = m.now().Add(copyNoteLinger)
		return m, nil

	case pasteMsg:
		// The read is asynchronous, so the composer may no longer be where the
		// keyboard is by the time the text arrives.
		if msg.text == "" || m.composerCovered() {
			return m, nil
		}
		// One bracketed paste, the way a terminal delivers it: the textarea
		// inserts the whole thing as text instead of reading it as keys.
		return m, m.updateComposer(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(msg.text), Paste: true})

	case dblClickMsg:
		// The frame runner's deterministic double-click: two real presses would
		// make a golden depend on the clock. The state machine itself is
		// unit-tested against the injected one.
		if !m.mouseEnabled || m.cardOpen() || !m.selectable(msg.X, msg.Y) {
			return m, nil
		}
		pos, ok := m.transcriptCell(msg.X, msg.Y)
		if !ok {
			return m, nil
		}
		// The token stands in for the whole gesture, release included, so it
		// copies the way the real release does.
		return m.selectWord(pos).copySelection()

	case planImplementMsg:
		// The session is in the implement mode now, so the note is honest
		// whatever else has happened meanwhile.
		m.addNote(modeNote(m.snap.Modes, msg.mode))
		if msg.seq != m.turnSeq {
			// A turn of the user's own started while SetMode was in flight, so
			// the offer this answers is gone. Writing the implement entry now
			// would put a prompt in the transcript that the session refuses as
			// already in flight.
			return m, nil
		}
		// The user entry is honest too, and the prompt goes out behind it.
		return m.sendText(m.snap.Provider.ImplementPrompt())

	case planImplementFailedMsg:
		// Nothing was sent: the mode reverts the way any failed SetMode does,
		// and the plan is still the last thing on screen, so it is still on
		// offer — unless something retired it while SetMode was in flight, in
		// which case there is no plan above to implement any more.
		tm, cmd := m.update(revertModeMsg{prev: msg.prev, err: msg.err})
		next := tm.(Model)
		if msg.seq == next.turnSeq && next.planDeadSeq != next.turnSeq {
			next.planOfferSeq = next.turnSeq
		}
		return next, cmd

	case eventMsg:
		m.applyEvent(msg.ev)
		// One transition settles the turn and starts whatever it left to do.
		// A foreign turn ending is the other moment the drain can run: the
		// turn it was blocked on is over and craze's own already settled.
		next, cmd := m.finishTurn()
		if ev := msg.ev; ev.Type == agent.EventForeignTurn && ev.ForeignTurn != nil && !ev.ForeignTurn.Running {
			next, cmd = next.drainSettledTurn()
		}
		return next, tea.Batch(cmd, waitEvent(next.sess))

	case promptDoneMsg:
		// The stream is closed by EventDone, which shares the event channel with
		// the chunks; this message races them and would split a run in two.
		m.promptEndSeq = m.turnSeq
		if msg.err != nil {
			// A prompt the session never accepted emits no events at all, so
			// its stream is over too: waiting for an ending that cannot come
			// would leave the turn unfinishable. Any other failure was emitted
			// as EventError before Prompt returned, so that ending is on its
			// way.
			if errors.Is(msg.err, agent.ErrPromptInFlight) || errors.Is(msg.err, agent.ErrForeignTurn) {
				m.streamEndSeq = m.turnSeq
			}
			m.status = statusError
			m.err = msg.err.Error()
			m.addError(m.err)
			m.dropStrongSend("")
			m.confirm = nil
			return m, nil
		}
		return m.finishTurn()

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return m, nil
}

// handleMouse is the mouse contract: the wheel scrolls the transcript, a left
// press either starts a selection over the transcript or hit-tests the regions
// frameLayout drew, and motion and release carry the drag.
//
// --no-mouse is kept on the model as well as withheld from bubbletea, so a
// mouse message that reached craze another way (the frame runner injects them)
// is ignored too.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if !m.mouseEnabled || m.cardOpen() {
		// A card owns the mouse as well as the keyboard, wheel included. It
		// arrived while the button was down, so the drag goes with it.
		return m, nil
	}
	switch msg.Action {
	case tea.MouseActionPress:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			if m.slashActive() && m.lay.Region(regionOverlay).Contains(msg.Y) {
				return m.slashWheel(-1), nil
			}
			// The selection is in transcript rows, not screen rows, so it
			// scrolls with the text it holds and survives the wheel.
			m.vp.ScrollUp(wheelLines)
		case tea.MouseButtonWheelDown:
			if m.slashActive() && m.lay.Region(regionOverlay).Contains(msg.Y) {
				return m.slashWheel(1), nil
			}
			m.vp.ScrollDown(wheelLines)
		case tea.MouseButtonLeft:
			return m.handlePress(msg.X, msg.Y)
		}
	case tea.MouseActionMotion:
		// Motion with no button down is a hover, which is what the queue's
		// action strip appears on. Only a drag extends a selection: a
		// double-click's word is already the selection it meant, and the
		// pointer wobbling over it must not eat into it.
		if msg.Button == tea.MouseButtonNone {
			m.queueHov = m.queueHoverAt(msg.X, msg.Y)
			return m, nil
		}
		if m.pressed == tea.MouseButtonLeft && m.sel.drag {
			return m.handleDrag(msg.X, msg.Y), nil
		}
	case tea.MouseActionRelease:
		// X10 terminals report ButtonNone on release, so the button that went
		// down is the only record of which one came up.
		if m.pressed == tea.MouseButtonLeft {
			return m.handleRelease(msg.X, msg.Y)
		}
	}
	return m, nil
}

// slashWheel is the wheel over the band: one row of selection per notch,
// clamped rather than wrapped. §3.5 deliberately does not reuse wheelLines —
// the menu is a selection, not a viewport, so "scrolling" it three at a time
// would jump past rows the user never saw highlighted. relayout's syncSlash
// carries slashTop along afterwards, the same as it does for a key move.
func (m Model) slashWheel(delta int) Model {
	items := m.filteredSlash()
	if len(items) == 0 {
		return m
	}
	m.slashSel = min(max(m.slashSel+delta, 0), len(items)-1)
	return m
}

// handlePress is the left button going down: over the transcript it anchors a
// selection (or picks a word, on the second press inside the double-click
// window), and anywhere else it is the click the regions already understood.
func (m Model) handlePress(x, y int) (tea.Model, tea.Cmd) {
	now := m.now()
	if !m.selectable(x, y) {
		// The press belongs to another band, so whatever was highlighted is
		// the previous gesture and goes.
		m.sel = selection{}
		m.clicks = 0
		return m.handleClick(x, y)
	}
	m.pressed = tea.MouseButtonLeft
	pos, ok := m.transcriptCell(x, y)
	if !ok {
		return m, nil
	}
	// A second press on the same cell inside the window is a double-click; a
	// third one inside it starts over, so a rattle of clicks does not keep
	// re-selecting the word.
	double := m.clicks == 1 && pos == m.clickPos && now.Sub(m.clickAt) < doubleClickWindow
	m.clickPos, m.clickAt = pos, now
	if double {
		// The word is selected now and copied by the release that follows,
		// exactly as a drag over it would be.
		m.clicks = 2
		return m.selectWord(pos), nil
	}
	m.clicks = 1
	m.sel = selection{on: true, drag: true, anchor: pos, head: pos}
	return m, nil
}

// handleDrag extends the selection to the cell under the pointer. A motion
// event on the top or bottom row of the band scrolls one line first, so a drag
// can reach past the screen.
//
// Cell motion only reports a change of cell: a pointer held still on the edge
// does not keep scrolling. That is the mode's contract, not a bug to fix.
func (m Model) handleDrag(x, y int) tea.Model {
	tr := m.lay.Region(regionTranscript)
	if tr.Empty() {
		return m
	}
	switch {
	case y <= tr.Top:
		m.vp.ScrollUp(1)
	case y >= tr.Bottom-1:
		m.vp.ScrollDown(1)
	}
	if pos, ok := m.transcriptCell(x, y); ok {
		m.sel.head = pos
	}
	return m
}

// handleRelease finalises the drag: the head is the cell under the pointer, and
// a selection of more than the one cell the press made is copied.
func (m Model) handleRelease(x, y int) (tea.Model, tea.Cmd) {
	m.pressed = tea.MouseButtonNone
	if !m.sel.on {
		return m, nil
	}
	if pos, ok := m.transcriptCell(x, y); ok && m.sel.drag {
		m.sel.head = pos
	}
	m.sel.drag = false
	return m.copySelection()
}

// selectWord is the double-click: the run of non-space cells under the pointer.
// The selection is not a drag, so nothing afterwards moves its head — a word is
// the gesture's whole answer.
func (m Model) selectWord(pos cellPos) Model {
	m.sel = selection{}
	plain := m.cur().transcriptPlain
	if pos.line >= len(plain) {
		return m
	}
	lo, hi, ok := wordAt(plain[pos.line], pos.col)
	if !ok {
		return m
	}
	// exact, because a one-letter word is a whole word: the range is inclusive,
	// so its two endpoints are the same cell and that is still a selection.
	m.sel = selection{
		on:     true,
		exact:  true,
		anchor: cellPos{line: pos.line, col: lo},
		head:   cellPos{line: pos.line, col: hi},
	}
	return m
}

// copySelection puts the highlighted cells on the clipboard. An empty
// selection — a plain click, or a double-click on whitespace — copies nothing
// and says nothing.
func (m Model) copySelection() (tea.Model, tea.Cmd) {
	text := m.selectionText()
	if text == "" {
		return m, nil
	}
	return m, copyRows(text)
}

// copySelectionOrLastReply is Ctrl+Y. Without a selection it copies the last
// reply's own text rather than the rows it was wrapped into, so what lands on
// the clipboard is the agent's paragraph and not the screen's line breaks.
func (m Model) copySelectionOrLastReply() (tea.Model, tea.Cmd) {
	if !m.sel.empty() {
		return m.copySelection()
	}
	entries := m.cur().entries
	for i := len(entries) - 1; i >= 0; i-- {
		if e := &entries[i]; e.kind == entryAssistant && e.text != "" {
			return m, copyText(e.text, "copied last reply")
		}
	}
	return m, nil
}

// copyLingering is the note's 2-second window, which is also why the tick chain
// has to stay fast until it closes.
func (m Model) copyLingering() bool {
	return !m.copyUntil.IsZero() && m.now().Before(m.copyUntil)
}

// handleClick reads the same frameLayout View() drew from, so what is on the
// screen and what is clickable cannot drift apart.
//
// The modal layer is hit-tested first and swallows the click either way: a
// press inside it is a dialog row, and a press anywhere else closes it the way
// Esc does — applying nothing, reverting a theme preview.
func (m Model) handleClick(x, y int) (tea.Model, tea.Cmd) {
	lay := m.lay
	if lay.TooSmall {
		return m, nil
	}
	// A click is "anything else" to the confirm line: it declines, and that
	// is all it does — carrying on would let the same click act on a row or
	// arm a new question over the one just answered.
	if m.confirm != nil {
		m.declineStrongSend()
		return m, nil
	}
	if r := lay.Dialog; !r.Empty() {
		if r.Contains(x, y) {
			return m.dialogClick(y - r.Y)
		}
		if m.pickingProvider {
			return m.confirmProvider(m.providerDefault, false)
		}
		return m.closeDialog(true), nil
	}
	switch {
	case lay.Region(regionOverlay).Contains(y):
		// slashTop is the top syncSlash settled for the frame just drawn, so
		// a click after scrolling lands on the row that was actually on
		// screen, not row 0 of the whole catalog. acceptSlash no-ops past
		// the last item — the catalog can shrink between the draw and the
		// click.
		return m.acceptSlash(m.slashTop + lay.Region(regionOverlay).Row(y)), nil
	case lay.Region(regionTasks).Contains(y):
		if lay.Region(regionTasks).Row(y) == 0 {
			return m.cycleTasks()
		}
	case lay.Region(regionComposer).Contains(y):
		if m.viewing != "" && lay.Region(regionComposer).Row(y) == 1 {
			m.leaveView()
		}
	case lay.Region(regionQueue).Contains(y):
		// The last row can be "… +n more", which is not a queued message.
		return m.queueClick(x, lay.Region(regionQueue).Row(y))
	case lay.Region(regionAgents).Contains(y):
		// The last row can be "… +n more", which is not a sub-agent.
		m.selectAgent(lay.Region(regionAgents).Row(y))
	case lay.Region(regionStatus).Contains(y):
		return m.clickStatus(x, lay.Region(regionStatus).Row(y), lay)
	}
	return m, nil
}

// clickStatus hit-tests a status row against the spans the fitting pass that
// drew it reported, so a click cannot land on a segment that was dropped or
// truncated away: row 1's model name opens the model dialog, row 2's chip
// cycles the mode.
func (m Model) clickStatus(x, row int, lay frameLayout) (tea.Model, tea.Cmd) {
	// Both are a key under the pointer, gating included: a card blocks them
	// (handleMouse already returned), an overlay that swallows the key
	// swallows the click, and working blocks neither.
	if m.dialogOpen() {
		return m, nil
	}
	switch row {
	case 0:
		_, spans := m.statusRow1()
		if spanAt(spans, x) == spanModel {
			return m.openModelDialog(), nil
		}
	case 1:
		if m.viewing != "" {
			return m, nil
		}
		_, spans := m.statusRow2(lay)
		if spanAt(spans, x) == spanMode {
			return m.cycleMode()
		}
	}
	return m, nil
}

// dialogClick turns a press inside the box into the row it landed on: the
// border and the rows above the list are not options.
func (m Model) dialogClick(row int) (tea.Model, tea.Cmd) {
	body := m.dialogBody(m.lay.Dialog.W-dialogBorder, m.lay.Dialog.H-dialogBorder)
	i := row - 1 // the top border
	if i < 0 || i >= len(body) {
		return m, nil
	}
	switch m.dialog {
	case dialogModel:
		return m.modelDialogClick(i)
	case dialogTheme:
		return m.themeDialogClick(i)
	case dialogProvider:
		return m.providerDialogClick(i)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The highlight is a mouse gesture: any key but the one that copies it
	// means the user has moved on.
	if msg.Type != tea.KeyCtrlY {
		m.sel = selection{}
	}
	switch msg.Type {
	case tea.KeyCtrlD:
		return m.requestQuit()
	case tea.KeyCtrlC:
		return m.handleCtrlC()
	}
	m.ctrlCDeadline = time.Time{}

	// A card owns the keyboard: everything below this, Ctrl+T / Ctrl+G /
	// Ctrl+O and the agent-row arrows included, is out of reach until it is
	// answered.
	if m.cardOpen() {
		return m.handleCardKey(msg)
	}

	switch m.dialog {
	case dialogTheme:
		return m.handleThemeDialogKey(msg)
	case dialogModel:
		return m.handleModelDialogKey(msg)
	case dialogHelp:
		return m.handleHelpDialogKey(msg)
	case dialogProvider:
		return m.handleProviderDialogKey(msg)
	}

	// The confirm line is a question with two answers: Enter confirms, Esc
	// and anything else decline and give the draft back. It outranks the
	// sub-agent view: a view opened over it would leave it armed and
	// unanswerable.
	if m.confirm != nil {
		switch msg.Type {
		case tea.KeyEnter:
			return m.confirmStrongSend()
		default:
			m.declineStrongSend()
			return m, nil
		}
	}

	if m.viewing != "" {
		return m.handleViewKey(msg)
	}

	if msg.Type == tea.KeyCtrlL {
		return m.handleStrongSend()
	}

	if msg.Type == tea.KeyCtrlY {
		// A keyboard feature, so it works under --no-mouse as well.
		return m.copySelectionOrLastReply()
	}
	if msg.Type == tea.KeyCtrlV {
		return m, pasteFromClipboard()
	}
	if msg.Type == tea.KeyCtrlO {
		return m.toggleExpanded()
	}
	if msg.Type == tea.KeyCtrlT {
		return m.cycleTasks()
	}
	if msg.Type == tea.KeyCtrlG {
		return m.openThemePicker(), nil
	}
	if msg.Type == tea.KeyShiftTab {
		return m.cycleMode()
	}
	if m.queueFocus {
		handled, next := m.handleQueueKey(msg)
		m = next
		if handled {
			return m, nil
		}
	}
	if m.agentFocus {
		handled, next := m.handleRowsKey(msg)
		m = next
		if handled {
			return m, nil
		}
	}
	if msg.Type == tea.KeyEsc {
		if m.queueEdit != "" {
			m.cancelQueueEdit()
			return m, nil
		}
		if m.slashActive() {
			// Esc hides this token's menu and nothing else; the draft is
			// untouched (pinned). Recording the token rather than setting a
			// flag is what reopens the menu as soon as the token changes or
			// the cursor moves into another one. The consequence under a
			// running turn is accepted: Esc on any /word hides the menu and
			// the second Esc cancels the turn.
			if start, _, name, ok := slashToken(m.input.Value(), m.composerCursorOffset()); ok {
				m.slashHideKey = slashTokenKey(start, name)
			}
			m.slashSel, m.slashTop = 0, 0
			return m, nil
		}
		if m.strong != nil {
			// The cancel is still in flight; dropping the send-now here is
			// what takes the text back before it turns into a turn.
			m.dropStrongSend("send now dropped")
			return m, nil
		}
		if m.status == statusWorking {
			// The queue survives Esc on purpose: cancelling this turn is not
			// cancelling what was meant to follow it, and the drain sends the
			// head once the cancelled turn settles.
			return m.cancelTurn()
		}
		if m.planArmed() {
			// Esc declines the plan offer and nothing else: the composer is
			// still where the user is, so it keeps the focus.
			m.retirePlanOffer()
			return m, nil
		}
		m.input.Blur()
		return m, nil
	}
	// While the band is up it is what the keyboard is on, so it takes these
	// keys before the transcript pages or the arrows leave the composer. The
	// key type is tested first because slashRows() costs a catalog build.
	// Esc is deliberately not here: its precedence is per-key (a queue edit
	// outranks it), not per-band, so it stays in the ladder above.
	switch msg.Type {
	case tea.KeyTab, tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown:
		if rows := m.slashRows(); rows > 0 {
			return m.handleSlashKey(msg.Type, rows), nil
		}
	}
	if msg.Type == tea.KeyPgUp || msg.Type == tea.KeyPgDown {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	if isNewlineKey(msg) {
		return m, m.updateComposer(msg)
	}
	if msg.Type == tea.KeyEnter {
		return m.handleEnter()
	}
	// Only the arrow keys move the keyboard out of the composer, empty
	// composer or not — a user typing a follow-up still browses what is
	// above; ctrl+p / ctrl+n stay with the textarea.
	//
	// ↑ reaches the queue band first (Claude Code's "press up to edit queued
	// messages") and the sub-agent rows otherwise; ↓ reaches the sub-agent
	// rows first, because they are the band below the composer, and the queue
	// only when there are none.
	if msg.Type == tea.KeyUp || msg.Type == tea.KeyDown {
		queued := len(m.visibleQueue())
		agents := len(m.visibleAgents())
		switch {
		case msg.Type == tea.KeyUp && queued > 0:
			m.focusQueue(queued - 1)
			return m, nil
		case agents > 0:
			m.focusRows()
			return m, nil
		case queued > 0:
			m.focusQueue(0)
			return m, nil
		}
	}
	return m, m.updateComposer(msg)
}

// focusRows moves the keyboard from the composer to the sub-agent rows: the
// composer loses its cursor and the selected row gains the gutter mark. The
// selection itself does not move, so ↓ from the composer lands where the
// user last was (the first row to begin with).
func (m *Model) focusRows() {
	m.agentFocus = true
	m.input.Blur()
}

// focusComposer hands the keyboard back to the composer.
func (m *Model) focusComposer() {
	m.agentFocus = false
	m.queueFocus = false
	_ = m.input.Focus()
}

// handleRowsKey is the keyboard while the rows have it. ↑ past the first row,
// Esc and any key that is not a row key return to the composer; that key is
// then handled as usual (handled == false), so typing never needs a second
// press.
func (m Model) handleRowsKey(msg tea.KeyMsg) (bool, Model) {
	items := m.visibleAgents()
	if len(items) == 0 {
		m.focusComposer()
		return false, m
	}
	switch msg.Type {
	case tea.KeyUp:
		switch {
		case m.agentSel > 0:
			m.moveAgent(-1)
		case len(m.visibleQueue()) > 0:
			// The band above the composer is the next thing up.
			m.focusQueue(len(m.visibleQueue()) - 1)
		default:
			m.focusComposer()
		}
		return true, m
	case tea.KeyDown:
		if m.agentSel < len(items)-1 {
			m.moveAgent(1)
		}
		return true, m
	case tea.KeyEnter:
		if m.agentID != "" {
			m.enterView(m.agentID)
		} else {
			m.selectAgent(m.agentSelection(len(items)))
		}
		return true, m
	case tea.KeyEsc:
		m.focusComposer()
		return true, m
	}
	m.focusComposer()
	return false, m
}

func (m *Model) updateComposer(msg tea.KeyMsg) tea.Cmd {
	// A blurred textarea drops every key, so whatever took the cursor away
	// (Esc on an idle turn, the rows) gives it back before the key lands.
	if !m.input.Focused() {
		_ = m.input.Focus()
	}
	prev := m.input.Value()
	// bubbles repositions its own viewport inside Update (textarea.go:1087),
	// against the height in force *before* the key, and never rewinds slack
	// afterwards: at height 1 a second line scrolled the first one — and the
	// `❯` that only ever marks display row 0 — off the top, and SetHeight does
	// not reposition. Lending the textarea more rows than any draft can have
	// keeps the cursor inside the view, so the offset stays 0 through newline,
	// wrap boundary and bracketed paste. relayout puts the real height back
	// before anything is drawn.
	m.input.SetHeight(composerHeadroom)
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() != prev {
		// A bare "/" coming under the cursor rescans the disk skills, so a
		// skill saved since the session started is in the catalog the menu is
		// about to draw. It is gated on the value changing — a keystroke, not
		// cursor motion — so the bounded walk runs once per such key.
		if _, _, name, ok := slashToken(m.input.Value(), m.composerCursorOffset()); ok && name == "" {
			m.rescanSkills()
		}
	}
	return cmd
}

// handleCtrlC implements the pinned state machine: working cancels and arms a
// one-second window, a second press inside the window quits, and idle or an
// error state quits outright.
func (m Model) handleCtrlC() (tea.Model, tea.Cmd) {
	if m.status != statusWorking {
		return m.requestQuit()
	}
	now := m.now()
	if !m.ctrlCDeadline.IsZero() && now.Before(m.ctrlCDeadline) {
		return m.requestQuit()
	}
	// One key stops everything pending, not just the turn: the queue, the
	// confirm, the send-now that was waiting for the cancel, and the edit.
	m.clearPending()
	tm, cmd := m.cancelTurn()
	next := tm.(Model)
	next.ctrlCDeadline = now.Add(ctrlCWindow)
	return next, cmd
}

// clearPending empties everything the queue band is holding. The strong send's
// text does not come back here: Ctrl+C means stop, and a draft reappearing
// under the cursor would be one more thing to undo.
func (m *Model) clearPending() {
	m.confirm = nil
	m.strong = nil
	if m.queueEdit != "" {
		m.cancelQueueEdit()
	}
	if m.sess != nil {
		m.sess.ClearQueue()
	}
	m.queueFocus = false
	m.queueHov = noHover()
	m.refreshSnap()
}

func (m Model) handleEnter() (tea.Model, tea.Cmd) {
	if m.confirm != nil {
		return m.confirmStrongSend()
	}
	// The menu owns Enter while it is up, and it owns it before the queue edit
	// saves: a row accepted inside an edit completes the text being edited, and
	// the next Enter saves it. A token that already spells the highlighted row
	// falls through, so a fully typed /help still runs on the first press and a
	// fully typed /gauntlet still sends.
	if m.slashActive() && !m.slashExactlyTyped() {
		return m.acceptSlash(m.slashSel), nil
	}
	if m.queueEdit != "" {
		return m.saveQueueEdit()
	}
	name, args, ok := parseSlashLine(m.input.Value())
	if ok && name == "exit" {
		return m.runBuiltin(name, args)
	}
	// The offer outranks the sub-agent rows: an empty composer under a live
	// offer means "build it", and the rows stay reachable for an empty
	// composer without one. The test is Value()=="" and not composerEmpty,
	// so Enter agrees with the placeholder the user is looking at.
	if m.planOffering() && m.input.Value() == "" {
		return m.implementPlan()
	}
	if m.cardOpen() || !m.started {
		return m, nil
	}
	// A builtin never queues: it is craze's own, it does not need the agent,
	// and holding it until the turn ends would be surprising. The ones that
	// do need the agent keep today's refusal.
	if ok && name != "" && builtinNamed(name) {
		// A turn the agent runs on its own is a running turn for this
		// purpose too: /model or /plan into it would land on a session
		// that is busy.
		if (m.status == statusWorking || m.snap.ForeignTurn) && !runsWhileWorking(name) {
			return m, nil
		}
		return m.runBuiltin(name, args)
	}
	// A running turn queues; so does a turn the agent is running on its own
	// (grok's interject fallback): a prompt sent into it would be refused,
	// and the queue drains the moment it ends.
	if m.status == statusWorking || m.snap.ForeignTurn {
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		return m.queueDraft(text)
	}
	return m.send()
}

// runsWhileWorking names the builtins that do not need the agent and are
// therefore answered mid-turn rather than refused. The rest keep today's
// silent refusal.
func runsWhileWorking(name string) bool {
	switch name {
	case "exit", "help", "theme", "tasks", "clear":
		return true
	}
	return false
}

func (m Model) cycleMode() (tea.Model, tea.Cmd) {
	if m.cardOpen() || !m.showModes() {
		return m, nil
	}
	id := agent.NextModeID(m.snap)
	return m.applyMode(id)
}

func (m Model) send() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	m.input.SetValue("")
	m.resetSlash()
	return m.sendText(text)
}

// sendText starts a turn with text that is already decided. It is what the
// composer's own send and the plan offer have in common: the plan offer never
// touches the draft, so the two differ only in where the text came from.
func (m Model) sendText(text string) (tea.Model, tea.Cmd) {
	m.addUser(text)
	m.status = statusWorking
	m.cardsCancelled = false
	m.turnStart = m.now()
	m.err = ""
	// The new turn's identity is the one thing that retires the last one: its
	// evidence, its offer and the kill that retired it all belong to a number
	// this turn no longer has.
	m.turnSeq++
	sess := m.sess
	return m, func() tea.Msg {
		res, err := sess.Prompt(context.Background(), text)
		return promptDoneMsg{res, err}
	}
}

// turnSettled reports whether the turn that was started has finished both ways:
// the prompt has returned and the stream has closed. They race, so the status
// waits for both — and so does the next turn, which is what stops one turn's
// buffered chunks arriving inside the next one.
func (m Model) turnSettled() bool {
	return m.promptEndSeq == m.turnSeq && m.streamEndSeq == m.turnSeq
}

// finishTurn is the one place a turn's ending is acted on. It fires on the
// unsettled → settled transition only — both endings in for the same turn,
// and the status still working — and then, in order: a confirmed send-now
// starts, or the drain waits out a turn the agent is running of its own, or
// one queued message goes. An error state stands: the turn that failed said
// so, the session has already cleared the queue, and only the next send
// clears the status.
func (m Model) finishTurn() (Model, tea.Cmd) {
	if m.status != statusWorking || !m.turnSettled() {
		return m, nil
	}
	m.status = statusIdle
	if m.confirm != nil {
		// The question was "cancel the running turn and send?" and the turn
		// answered it first. Nothing was taken from anywhere, so the draft
		// and the row are both still where they were.
		m.confirm = nil
		m.note("the turn ended first")
	}
	return m.drainSettledTurn()
}

// drainSettledTurn starts what a settled turn left behind. It is separate from
// finishTurn because a foreign turn ending re-opens the same decision long
// after the status went idle.
func (m Model) drainSettledTurn() (Model, tea.Cmd) {
	if m.status != statusIdle || !m.turnSettled() || m.sess == nil {
		return m, nil
	}
	if m.snap.ForeignTurn {
		// The agent is talking on its own; nothing can be sent into that,
		// the armed send-now included. Everything re-runs when it stops.
		return m, nil
	}
	if p := m.strong; p != nil {
		m.strong = nil
		if p.seq != 0 && p.seq != m.turnSeq {
			// It was armed against a turn that is no longer the one that
			// just ended, so it is not this turn's business.
			m.note("send now dropped")
		} else {
			tm, cmd := m.fireStrongSend(*p)
			next := tm.(Model)
			if next.status == statusWorking {
				return next, cmd
			}
			// The row it named was already gone; fall through to the drain.
			m = next
		}
	}
	next, ok := m.sess.PopQueue()
	if !ok {
		return m, nil
	}
	m.refreshSnap()
	tm, cmd := m.sendText(next.Text)
	return tm.(Model), cmd
}

// planArmed is the offer as state: it belongs to the turn that earned it, so a
// turn start retires it and a late event from a retired turn cannot revive it.
func (m Model) planArmed() bool { return m.turnSeq != 0 && m.planOfferSeq == m.turnSeq }

// retirePlanOffer kills the offer for the turn that is running and records the
// kill against it, so an EventDone still on its way — or a SetMode that comes
// back after the action — cannot arm it again.
func (m *Model) retirePlanOffer() {
	m.planOfferSeq = 0
	m.planDeadSeq = m.turnSeq
}

// planOffering is the offer once it can be acted on. EventDone arms it and the
// turn's two endings settle the status, in either order, so everything the user
// can see or press waits for both — which is what makes the orderings
// indistinguishable. A card outranks it: cards own Enter, so the offer hides
// until the card is answered rather than competing for the key.
func (m Model) planOffering() bool {
	return m.planArmed() && m.status == statusIdle && !m.cardOpen()
}

// implementModeID is the advertised mode that means "do the work". Without one
// there is nowhere for the offer to go, so it is never made.
func (m Model) implementModeID() string {
	for _, md := range m.snap.Modes {
		if m.snap.Provider.Kind(md.ID) == agent.ModeImplement {
			return md.ID
		}
	}
	return ""
}

// planEarnsOffer is what a finished turn has to have been for the composer to
// offer the plan it left behind.
func (m Model) planEarnsOffer(stopReason string) bool {
	if !m.showModes() || stopReason == stopCancelled || m.status == statusError {
		return false
	}
	// An action retired this turn's offer, so the ending that would have armed
	// it is answering for a plan nobody is looking at any more.
	if m.planDeadSeq == m.turnSeq {
		return false
	}
	// A turn that only thought, or only ran tools, said nothing to implement —
	// and so did one whose chunks were all empty.
	if m.sawAssistantSeq != m.turnSeq {
		return false
	}
	if m.snap.Provider.Kind(m.snap.CurrentMode) != agent.ModePlan {
		return false
	}
	return m.implementModeID() != ""
}

// implementPlan is Enter on the offer. The mode change and the prompt are
// chained rather than batched: cursor must not be asked to build the plan
// while the session is still in plan mode, so the prompt is only written once
// SetMode has come back, and a SetMode that fails sends nothing at all.
func (m Model) implementPlan() (tea.Model, tea.Cmd) {
	if m.viewing != "" {
		return m, nil
	}
	id := m.implementModeID()
	if id == "" {
		return m, nil
	}
	prev := m.snap.CurrentMode
	// Optimistic, the way applyMode is: the chip flips now and reverts if the
	// agent refuses.
	m.snap.CurrentMode = id
	// The offer is spent, not retired: a SetMode that fails leaves the plan on
	// screen, and that path is allowed to put the offer back.
	m.planOfferSeq = 0
	seq := m.turnSeq
	sess := m.sess
	return m, func() tea.Msg {
		if err := sess.SetMode(context.Background(), id); err != nil {
			return planImplementFailedMsg{seq: seq, prev: prev, err: err}
		}
		return planImplementMsg{seq: seq, mode: id}
	}
}

// cancelTurn drops the whole card queue and cancels. Cancel answers every
// request the session is still holding — permission, question and plan alike —
// with that kind's cancelled outcome, exactly once each, so the UI must not
// answer them itself and race it.
func (m Model) cancelTurn() (tea.Model, tea.Cmd) {
	cards := len(m.cards)
	working := m.status == statusWorking
	if cards == 0 && !working {
		return m, nil
	}
	m.cards = nil
	m.cardsCancelled = true
	// A cancelled turn offers nothing, whichever of its two endings the cancel
	// beat: the kill is recorded against the turn, so the EventDone still on
	// its way cannot arm what Esc just declined.
	m.retirePlanOffer()
	sess := m.sess
	seq := m.turnSeq
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := sess.Cancel(ctx); err != nil {
			return cancelFailedMsg{seq: seq, err: err}
		}
		return nil
	}
}

// cancelFailedMsg says the cancel never reached the agent. A send-now armed
// behind it would wait for a turn that is not ending, so the text goes back
// to the composer instead.
type cancelFailedMsg struct {
	seq int
	err error
}

// requestQuit closes the session, which answers every card still queued with
// its cancelled outcome on the way out. The queue is left alone: the model is
// on its way out with it, and clearing it would only restart the tick chain.
func (m Model) requestQuit() (tea.Model, tea.Cmd) {
	m.quitting = true
	sess := m.sess
	return m, func() tea.Msg {
		if sess != nil {
			_ = sess.Close()
		}
		return tea.Quit()
	}
}

func (m *Model) applyEvent(ev agent.Event) {
	if ev.Type == agent.EventSubagent {
		m.applySubagentEvent(ev)
		return
	}
	if ev.Agent != "" {
		m.applyChildEvent(ev)
		return
	}
	// The spinner names what the turn is doing; only a thought chunk leaves it
	// on "Thinking…".
	m.lastThought = ev.Type == agent.EventThought
	switch ev.Type {
	case agent.EventUser:
		if ev.Interjection {
			// The turn is still running: the text joined it rather than
			// starting one, so it is the only user block craze does not
			// write from its own send.
			m.addInterjection(ev.Text)
		}
		return
	case agent.EventCommand:
		// It arrives before the request reaches the wire, so the line lands
		// under the user block craze has already written and above anything
		// the agent goes on to say.
		m.addCommandLine(ev.Command)
		return
	case agent.EventQueue:
		// The band draws from the snapshot; nothing else has to happen.
		m.refreshSnap()
		if ev.QueueChange == agent.QueueRemoved && m.status == statusError {
			m.note("queue cleared")
		}
		return
	case agent.EventForeignTurn:
		m.refreshSnap()
		if ev.ForeignTurn != nil && ev.ForeignTurn.Running {
			// What follows is the agent talking without a prompt of craze's.
			// The note is what stops the reply reading as an answer to the
			// last thing the user said.
			m.breakStream()
			m.addNote(foreignTurnNote)
		} else {
			m.breakStream()
		}
		return
	case agent.EventText:
		if ev.Text != "" {
			// Only a chunk that says something is evidence: appendStream drops
			// an empty one, and a turn whose whole reply was empty left no plan
			// on the screen to implement.
			m.sawAssistantSeq = m.turnSeq
		}
		m.appendStream(entryAssistant, ev.Text, ev.At)
	case agent.EventThought:
		m.appendStream(entryThought, ev.Text, ev.At)
	case agent.EventTool:
		m.refreshSnap()
		if ev.Tool != nil {
			m.upsertTool(ev.Tool)
		}
	case agent.EventTodos:
		m.refreshSnap()
		todos := m.todosOf(ev)
		m.noteTodoLifecycle(todos)
		m.noteTodos(todos)
	case agent.EventPermission:
		if ev.Permission != nil {
			m.pushCard(card{kind: cardPermission, perm: ev.Permission})
		}
	case agent.EventQuestion:
		// An auto-answered request (headless) is already decided; only an
		// interactive one is a card.
		if ev.Question != nil && !ev.Question.Auto {
			if m.showAsk() {
				m.pushCard(card{kind: cardQuestion, ask: ev.Question})
			} else if m.sess != nil {
				_ = m.sess.AnswerQuestion(ev.Question.ID, nil, true)
			}
		}
	case agent.EventPlan:
		if ev.Plan != nil && !ev.Plan.Auto {
			// The plan itself is transcript material; the card is only the
			// three answers it needs.
			m.addPlan(ev.Plan)
			if m.showPlan() {
				m.pushCard(card{kind: cardPlan, plan: ev.Plan})
			} else if m.sess != nil {
				_ = m.sess.AnswerPlan(ev.Plan.ID, false)
			}
		}
	case agent.EventDone:
		m.breakStream()
		m.cardsCancelled = false
		m.streamEndSeq = m.turnSeq
		// The turn is over, so this is the one moment the branch can have
		// changed under craze. No polling, no resize hook.
		m.branch = m.git.branch()
		if ev.StopReason == stopCancelled {
			// Esc leaves nothing else behind: the spinner going away is the
			// only other sign the cancel landed, and it is indistinguishable
			// from the turn having finished on its own.
			m.addNote(stopCancelled)
		}
		// EventDone, not promptDoneMsg, is what orders the transcript, so it is
		// also what decides whether the turn left a plan behind.
		if m.planEarnsOffer(ev.StopReason) {
			m.planOfferSeq = m.turnSeq
		}
	case agent.EventError:
		// The error is the prompt's other ending: Prompt emits it and returns,
		// so no EventDone follows it.
		m.streamEndSeq = m.turnSeq
		m.status = statusError
		// Nothing drains from an error state, so an armed send would sit
		// there and fire behind whatever the user sends next.
		m.dropStrongSend("")
		m.confirm = nil
		if ev.Err != nil {
			m.err = ev.Err.Error()
			m.addError(m.err)
		}
	case agent.EventMeta:
		if ev.Mode != "" {
			// Any mode update at all retires the offer, even one that names the
			// mode craze already thought it was in: the agent may have gone
			// plan → ask → plan, and comparing snapshots cannot see the trip.
			m.retirePlanOffer()
		}
		m.refreshSnap()
	}
}

// toggleExpanded is the global Ctrl+O detail toggle; every entry redraws
// because the render key changed.
func (m Model) toggleExpanded() (tea.Model, tea.Cmd) {
	stick := m.vp.Height == 0 || m.vp.AtBottom()
	m.expanded = !m.expanded
	m.setViewportContent(stick)
	return m, nil
}

// todosOf prefers the list the event carried and falls back to the snapshot.
func (m Model) todosOf(ev agent.Event) []agent.Todo {
	if len(ev.Todos) > 0 {
		return ev.Todos
	}
	return m.snap.Todos
}

// refreshSnap re-reads the session's snapshot. It draws no conclusions from
// what changed: a mode change is reported by the event that carries it, because
// applyMode has already written the user's own change into the snapshot and an
// agent-side change that went round in a circle leaves nothing to compare.
func (m *Model) refreshSnap() {
	if m.sess == nil {
		return
	}
	m.snap = m.sess.Snapshot()
	if m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
	}
	// Stamp the rows on first sight in a snapshot, not only on the lifecycle
	// event: a tool re-emit can carry a finished status one Update ahead of
	// the `finished` event, and a finished row without its stamp would drop
	// out of the band for that frame and move the selection under the user.
	for i := range m.snap.Subagents {
		s := &m.snap.Subagents[i]
		if subagentTerminal(*s) {
			m.noteAgentDone(s.ID)
		} else {
			m.noteAgentStart(s.ID)
		}
	}
}

// View places the regions the layout decided, each forced to exactly its own
// row count, so the frame is always exactly as tall as the terminal.
func (m Model) View() string {
	if !m.ready || m.width <= 0 || m.height <= 0 {
		// A degenerate size still owes the terminal exactly its own rows.
		return blankFrame(m.width, m.height)
	}
	lay := m.lay
	if lay.Width != m.width || lay.Height != m.height {
		// Only reachable when something resized the model without an Update;
		// m is a copy here, so this does not count against "once per Update".
		lay = m.computeLayout()
	}
	if lay.TooSmall {
		return tooSmallView(m.width, m.height)
	}
	rows := make([]string, 0, m.height)
	for i := range frameRegions {
		r := lay.regions[i]
		if r.Empty() {
			continue
		}
		rows = append(rows, fitRows(frameRegions[i].view(m, lay), r.Height(), m.width)...)
	}
	base := strings.Join(rows, "\n")
	// The dialog is a layer, not a band: it is composited over the frame the
	// region list just drew, so the bands keep their rows and the box covers
	// only the transcript.
	if r := lay.Dialog; !r.Empty() {
		base = overlay(base, m.dialogView(r), r)
	}
	return base
}

// workspaceName is the basename of the workspace, falling back to the path
// itself at a filesystem root.
func workspaceName(cwd string) string {
	base := filepath.Base(cwd)
	switch base {
	case "", ".", string(filepath.Separator):
		return cwd
	}
	return base
}

func waitEvent(sess agent.Session) tea.Cmd {
	if sess == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-sess.Events()
		if !ok {
			return nil
		}
		return eventMsg{ev}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
