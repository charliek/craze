package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/host"
	"github.com/charliek/craze/internal/sessions"
)

// ctrlCWindow is how long a Ctrl+C that cancelled a turn stays armed; a second
// press inside it quits.
const ctrlCWindow = time.Second

// stopCancelled is the one stop reason the TUI reads. internal/tui never
// imports internal/acp, so the string is spelled here.
const stopCancelled = "cancelled"

// foreignTurnNote heads the stream of a turn the agent started on its own.
const foreignTurnNote = "agent continued on its own (interjection fallback)"

// restoredNote closes a session/load replay in the transcript: everything
// above it is history the agent handed back, everything below is this session.
const restoredNote = "restored"

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
	// Providers is the picker's rows, filtered for availability by the caller.
	// tui.New inserts Config.Provider if it is missing, so the resolved default is
	// always offered (§3.4). Empty means the built-in default set —
	// agent.DefaultProviders(), every provider that is not optional — which keeps
	// every test that builds a Config by hand hermetic.
	Providers []agent.Provider
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
	// Resume is --resume's rows, newest first: when it is non-empty New opens
	// the resume picker instead of the provider picker (and instead of
	// starting anything), and Init returns nil until a row is chosen. The
	// caller has already filtered them to this workspace and, when --provider
	// was explicit, to that provider (§3.1).
	Resume []sessions.Row
	// LoadSession constructs a session that loads the chosen row — the same
	// closure as NewSession with agent.Options.LoadSessionID, Title and
	// TitlePinned filled in from the row. It is only ever called from the
	// resume picker; --continue resolves its row in internal/cli and passes
	// the session in Session with Loading set.
	LoadSession func(agent.Provider, sessions.Row) agent.Session
	// Loading says Session was built to resume an existing agent session
	// (agent.Options.LoadSessionID), so Start replays its transcript before
	// it returns. tea.Batch gives no ordering between startCmd and the first
	// waitEvent, so the model cannot learn this from EventReplay{start}: it
	// is constructed replaying and only EventReplay{end} clears the flag
	// (§3.5). A new session leaves this false and behaves exactly as today.
	Loading bool
	// SessionIndex persists ~/.craze/sessions.jsonl, the catalog --continue
	// and --resume read. nil means no persistence, which is what every TUI
	// unit test and every golden uses: the TUI never reaches for the store
	// itself, so nothing here can write a developer's real index (§3.2). It is
	// handed to the engine, which is what writes it (plan 021 §3.8).
	SessionIndex SessionIndex
	// CrazeSessionID is the durable craze session id of the row Session was
	// built to load: --continue resolves its row in internal/cli, so this is
	// how that row's crazeId reaches the engine (session control SD-22). The
	// resume picker has the row in hand and carries the id itself; a new
	// session leaves this empty and the engine mints one.
	CrazeSessionID string
	// TerminalTitle turns on the tab title the Update wrapper maintains
	// (§3.10). It defaults false, which is what every test Config and the
	// frame runner leave it — the frame runner's tea.WithoutRenderer() makes
	// the message a no-op anyway, so nothing depends on it there. runTUI is
	// the one caller that reads ConfigTerminalTitle() into this.
	TerminalTitle bool
	// Background turns on the themed terminal background: while craze runs it
	// sets the terminal's own default colours with OSC 11/10 and resets them
	// with OSC 111/110 on the way out (§3.3). Like TerminalTitle it defaults
	// false, so every test Config and the frame runner emit nothing at all.
	// runTUI is the one caller that fills it, from ConfigBackground(),
	// --no-background and the colour profile.
	Background bool
	// Host receives the derived host status (plan 015 §3.1) — what herdr
	// shows for this pane. nil means nothing is reported, which is what every
	// test Config, every golden and the frame runner get; internal/cli builds
	// one only when a host's environment gate is met, and Run closes it on
	// every exit path.
	Host Host
}

// SessionIndex is the write half of internal/sessions.Store, as the TUI needs
// it. It is an interface rather than the concrete store so a test can inject a
// recorder and so the zero Config persists nothing.
type SessionIndex interface {
	Upsert(sessions.Row) error
}

// Host is the host-status hub as the TUI needs it: internal/host.Hub in
// production, a recorder in a test. Publish must never block — the Update
// wrapper calls it on the bubbletea goroutine — and Close is called once, from
// Run's exit tail, before the session is closed.
type Host interface {
	Publish(host.Status)
	Close(context.Context)
}

type Model struct {
	theme Theme
	vp    viewport.Model
	input textarea.Model

	// eng is the engine the model drives its session through: admission, the
	// message queue and its verbs, send-now, cancel, the asks, the settings,
	// and — since C12 — the session index and the durable session id (plan 021
	// §3.4, §3.6, §3.8). sess is the engine's own session, the raw provider
	// seam, and **no production path calls anything on it at all**: the index
	// was the last thing the model did for itself, and the engine does it now.
	// It is kept because an engine the model could not build leaves the session
	// to be closed all the same (engErr), and because the tests reach for the
	// session they handed in. Both are assigned only in setSession, which
	// records the engine in owner too.
	eng  *engine.Engine
	sess agent.Session
	// client is this model's client id on eng, minted once per engine, and
	// cmdSeq numbers its commands from 1, so every mutating command it sends
	// names itself and the events it caused can be told from another client's
	// (§3.2). Receipts do not exist until later; this is what makes Event.Cause
	// mean something.
	client string
	cmdSeq int
	// chains orders this client's model changes against each other: the
	// dialog's apply chains and `/model <id> [<effort>]` each take a place in
	// its line in the Update that issues them, and run whole, one at a time, in
	// that order (chainLock). It is minted with the client id in setSession,
	// one per engine, and is a pointer so every copy bubbletea makes shares it.
	chains *chainLock
	// engErr is what wrapping the session in an engine came back with. It is
	// unreachable in practice — every session owns an event log and no path
	// wraps one twice — and is carried rather than panicked on, so it fails the
	// way a session that would not start fails: startCmd reports it.
	engErr error
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

	// main is the session transcript's pane. cur() returns the viewed
	// sub-agent's pane when viewing != "", otherwise main. Both are pointers
	// every copy of the model shares (see pane); New allocates them.
	main      *pane
	subs      map[string]*pane
	viewing   string
	tombstone *agent.SubagentInfo

	expanded bool
	width    int
	height   int
	ready    bool
	quitting bool
	started  bool
	// replaying is the second half of the "session is up" gate (§3.5). A
	// loaded session is constructed with it set — Config.Loading knows what
	// Start is about to do, and tea.Batch promises no ordering between
	// startCmd and the first waitEvent — and only EventReplay{end} clears it.
	// Whichever of startedMsg and that event lands second runs sessionUp.
	replaying bool
	// sessionIndex is Config.SessionIndex; nil means nothing is persisted. The
	// model does not write it any more — the engine does, and decides every
	// moment worth recording (plan 021 §3.8) — so this is held only to hand to
	// the engine setSession builds.
	//
	// crazeID is Config.CrazeSessionID: the durable id of the row a --continue
	// loaded, for the engine to keep rather than mint a new one (SD-22). The
	// resume picker has the row itself and passes its id to setSession
	// directly.
	sessionIndex SessionIndex
	crazeID      string

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
	// cardMasking says the cancel mask is up: a cancel answered every request
	// the session was holding, so an opening still in flight must not raise a
	// card for an ask that cancel has already ended. What it drops is decided
	// per opening, against the registry (maskDrops) — a live ask is never
	// swallowed, whatever the mask says — which is what lets the mask cover a
	// cancel with no turn of craze's own too, and so removes the card flash an
	// opening in flight at that Esc used to leave.
	//
	// cardMask is the turn it is **keyed to** for clearing, the engine turn id
	// (plan 021, panel astra 10): it cannot leak onto the next turn, because
	// beginTurn clears it, and it cannot outlive its own, because that turn's
	// ending — which the log orders behind every opening and ending of the turn
	// — clears it too. A cancel with no turn of craze's own keys it to the empty
	// turn id, and then the next beginTurn is what clears it.
	//
	// askEchoes are the causes of answers this model sent whose endings it has
	// not seen yet: their effect was applied in the Update that asked for them,
	// so the events are its own echoes (applyAskEnded).
	//
	// hiddenRetry are answers to asks the config shows no card for that the
	// engine refused for want of room (answerHidden), and hiddenRetryLive says
	// the one beat they are waiting on is in flight (armHiddenRetry).
	cards           []card
	cardMask        string
	cardMasking     bool
	askEchoes       []string
	hiddenRetry     []hiddenAnswer
	hiddenRetryLive bool
	snap            agent.Snapshot
	// queue is the engine's message queue, in send order: refreshSnap and
	// refreshQueue fill it from Control.State().Queue, which is where the
	// queue lives now that it has left the provider seam (plan 021 §3.5).
	queue []agent.QueuedPrompt
	// modeInFlight is a mode change of craze's own that the agent has not
	// answered yet. The chip flips when the user asks for it and reverts only
	// if the agent refuses, but the session's snapshot still says the old mode
	// for the length of that round trip — so refreshSnap keeps this one on the
	// chip instead, or any unrelated update landing inside the window flickers
	// it back to the mode the user just left.
	//
	// modeGen is which request it belongs to. The mode id cannot stand in for
	// that: changes are not serialised, two of them can be answered out of
	// order, and ids repeat — agent → plan → agent → plan hands the third
	// request's protection to the first request's answer. Every writer bumps
	// the generation and every answer carries it back, so an answer that is
	// not the current generation is stale and may neither clear the flag nor
	// speak for the chip. The flag *overrides* the session's snapshot, so a
	// stuck or wrongly-cleared one makes the chip lie for the life of the
	// process, where the bug it was introduced for only made it flicker.
	modeInFlight string
	modeGen      int

	// modeRev, modelRev and configRev are the highest StateDelta Seq this model
	// has applied for each settings section: the mode, the model, and the
	// config options — one revision for all of them, because a config delta
	// carries every option in full.
	//
	// They are what a delayed answer is judged against (mayApply). modeGen,
	// applyGen and modeInFlight stay exactly what they were: optimistic view
	// state about this model's own requests. These are about the shared state
	// the session actually holds, which another client can change too, and the
	// only thing that orders the two is the revision the deltas carry.
	modeRev   uint64
	modelRev  uint64
	configRev uint64

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
	// pickingResume is the resume picker, the other pre-start dialog. It is
	// its own flag rather than a mode of pickingProvider because the two
	// answer a click outside the box differently: the provider picker starts
	// its default, and there is no default session to start (§3.7).
	pickingResume   bool
	resumeCursor    int
	resume          []sessions.Row
	loadSession     func(agent.Provider, sessions.Row) agent.Session
	providerLocked  bool
	persistProvider bool
	fallbackDefault bool
	pickedExplicit  bool
	providerCursor  int
	providerDefault agent.Provider
	// providers is the picker's rows, settled once in New: the caller's
	// availability-filtered list unioned with providerDefault (§3.4). Nothing
	// after the constructor recomputes it, so the rows the user sees are the
	// rows Esc and Enter act on.
	providers  []agent.Provider
	newSession func(agent.Provider) agent.Session

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
	// the edit displaced and Esc puts back. queueEditCtx is the shell context
	// that row was queued with, held out of the composer for the length of the
	// edit and put back in front of whatever is saved (plan 022 §3.6).
	queueEdit    string
	queueEditPos int
	editDraft    string
	queueEditCtx string
	// confirm is the send-now waiting for an answer. The confirm line is
	// client-local UI: nothing is taken from anywhere and the engine has not
	// heard of it. The send-now it turns into, on the other hand, is the
	// engine's armed send (State().SendNow), because the cancel that makes room
	// for it is the engine's.
	confirm *strongSend
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
	// behind. planApprovedSeq is the turn in which a plan ask was ACCEPTED,
	// which is the other way a turn leaves a plan to implement: on the native
	// provider a plan is a file the model wrote and offered through
	// exit_plan_mode, and such a turn can end with no assistant text at all
	// (plan 023 §3.6, correction 2). planOfferSeq is the turn whose ending armed
	// the offer, and planDeadSeq the turn whose offer an action has killed —
	// which is what stops a late EventDone re-arming an offer Esc, /clear or a
	// card retired.
	sawAssistantSeq int
	planApprovedSeq int
	planOfferSeq    int
	planDeadSeq     int
	// turnID is the engine turn turnSeq names: what the model is looking at, and
	// what a cancel is asked against, so a cancel delayed across a queue
	// transition is refused as stale rather than stopping the turn the user did
	// not mean (§3.7).
	turnID string
	// ownTurn is the one turn id the model skips the started event for: the one
	// Submit handed it back synchronously, whose row, working status and
	// turnStart the Update that pressed Enter has already applied. Echo
	// suppression is per effect, so every other started — a drained row, an
	// armed send firing, another client's prompt — draws its row (§3.4).
	ownTurn string
	// nextTurn is a turn Submit started while the model was still displaying
	// another one as working: the engine had finished that turn and the model had
	// not heard yet, because an engine event trails the state it describes. Such
	// a turn is NOT applied in the Update that asked for it — doing so would draw
	// its row ahead of the events of the turn before it, and then draw that
	// turn's row a second time when its own started arrived. It is recorded here
	// instead, and then behaves exactly as a queued row the engine drained: the
	// displayed turn's ending keeps the model working because a successor is
	// coming, and the row is drawn when its own started arrives, in order.
	//
	// One id is enough even for two of them. It means "at least one turn the
	// model has accepted is still to start", the ids arrive in order, and each
	// ending in between finds it set and stays working; the last one clears it.
	nextTurn string
	// armedDraft names the command that armed a send-now from THIS client's
	// composer — the Cause of the Submit that answered Armed with no row — and is
	// "" when nothing of the sort is waiting. Firing that send takes the draft
	// with it; nothing else may.
	//
	// It is the command and not a flag because a flag says draft-versus-row and
	// not WHICH arm, and arms can overlap in what the model has applied: a send
	// armed from a draft can fire, and a second be armed from a newer draft,
	// before the first's started is delivered. A flag cleared by that late event
	// would leave the newer draft to be sent twice. The command's own id cannot be
	// mistaken for another's, this client's or another client's.
	//
	// Exactly one event ever carries an arm's cause — the started that fires it,
	// or the delta that retires it — so the marker is consumed by the started it
	// names and by nothing else. A marker whose arm was retired instead is inert:
	// no started can ever carry that cause again.
	armedDraft string
	// disarmed is the Disarm command whose effect the model has already applied,
	// for the same reason: Esc writes its own note in the Update that pressed
	// it, and the delta the engine publishes for that same command is then this
	// model's own echo. A withdrawn delta from any other command is somebody
	// else's Disarm and is worded for the user.
	disarmed string

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

	// terminalTitle is Config.TerminalTitle: the off switch. false means the
	// Update wrapper never computes or emits a title at all, which is what
	// craze frame relies on (its Config never sets this) and what
	// terminal_title = false gives a real run.
	terminalTitle bool
	// lastTitle is the last string windowTitle() emitted a tea.SetWindowTitle
	// for, so the Update wrapper can write on change alone (§3.10) instead of
	// on every tick. It also doubles as "a title was ever set": Run's exit
	// clear checks it on the final model rather than carrying a second flag.
	lastTitle string

	// term themes the terminal itself, via the OSC 10/11 pair; see terminal.go.
	term *terminalColors
	// shell owns the command the composer is running, if any (plan 022 §3.6).
	// It is a shared pointer for the same reason term and owner are: the model
	// is copied on every Update, and the quit paths — SIGTERM's especially,
	// which reaches finishRun with the model Run *started* with — have to be
	// able to kill a command a much later copy started. Nil only in a zero
	// Model a test built, which every caller guards for.
	shell *shellController
	// shellCtx is what the commands that have finished will tell the agent
	// with the next message this composer sends (shell_context.go). An
	// ordinary copied field and not state on the controller beside it,
	// because it belongs to the message being written rather than to the
	// process that produced it: it is read and cleared in Update, on the one
	// goroutine, exactly as the draft it will lead is.
	shellCtx []agent.ShellResult
	// owner is the session the program holds, shared by every copy the way
	// term is; see sessionOwner. Nil only in a zero Model a test built.
	owner *sessionOwner

	// host is Config.Host: nil means no host status is derived at all. The
	// rest is what host.go reads into host.Input. lastHost is the last status
	// handed to host, so the Update wrapper publishes on change alone, the
	// way lastTitle does. cancelled says the last turn ended cancelled, by
	// either ending; prompted says a prompt has been sent this session, so the
	// idle a session comes up with is "ready" and not a turn that stopped (a
	// flag of its own, not a reading of turnSeq, which starts at 1).
	// sessProvider is the id of the provider the current session was built
	// for — the resolved default, or the picker's or the resume row's choice.
	host         Host
	lastHost     host.Status
	cancelled    bool
	prompted     bool
	sessProvider string
}

// now reads the clock through an indirection so tests can inject one.
func (m Model) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

type eventMsg struct{ ev agent.Event }

// startedMsg says the session is up: the start command's Start has returned.
// errMsg is that command's other answer, and the only error that reaches craze's
// exit status.
//
// Both name the engine they are for. A start command outlives the model copy that
// made it — a picker's choice closes one engine and builds another in the same
// Update, with the old command still in flight — and neither message may then
// speak for the engine that replaced the one it was about: a stale startedMsg
// would open the new engine's gate before its own Start had returned, and a stale
// errMsg would fail a session that is starting perfectly well. A nil eng means
// "whichever engine the model holds", which is what a test injecting either
// message by hand intends.
type startedMsg struct{ eng *engine.Engine }
type errMsg struct {
	err error
	eng *engine.Engine
}
type actionErrMsg struct{ err error }

// revertModeMsg is a mode change coming back refused, or never coming back
// inside modeCallTimeout. gen is the request it answers for; prev is what the
// chip showed before that request asked, and at the mode section's revision
// when it asked (mayApply).
type revertModeMsg struct {
	gen  int
	prev string
	err  error
	at   uint64
}

// modeAppliedMsg is a mode change coming back accepted: the session's snapshot
// carries this mode now, so the chip can go back to reading it. gen is the
// request it answers for. It writes no value of its own — it only takes the
// chip's mask down — so it needs no revision.
type modeAppliedMsg struct {
	gen int
	id  string
}

// revertModelMsg is a model change coming back refused. at is the model
// section's revision when it asked: a refusal that arrives after somebody
// else's change has been applied may show its error but may not put prev back
// (mayApply).
type revertModelMsg struct {
	prev string
	err  error
	at   uint64
}

// modelUnreadMsg is a model change coming back with an answer the session
// could not read (agent.ErrBadCatalog). It is not revertModelMsg: the agent
// answered and may have switched, so nothing says the model was refused and
// there is no prev to put back. Nothing of the answer was installed, so the
// screen reads the session's snapshot back, and the row says the outcome is
// unknown (unreadModelText).
type modelUnreadMsg struct{}
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
//
// Both carry the mode generation as well, for the same reason revertModeMsg
// and modeAppliedMsg do: the turn says whether the prompt is still wanted, the
// generation says whether this is still the mode request the chip is showing.
type planImplementMsg struct {
	seq  int
	gen  int
	mode string
}
type planImplementFailedMsg struct {
	seq  int
	gen  int
	prev string
	err  error
	at   uint64
}

// mayApply reports whether a settings answer that has come back late may still
// write its value: it may unless a delta for its section with a higher revision
// has been applied since the request was issued.
//
// applied is the highest revision this model has applied for that section
// (Model.modeRev and its two siblings), at is where the section stood when the
// request was issued, and rev is the revision the engine confirmed the change
// at — 0 for a refusal, which confirms nothing. So a refusal is judged against
// where it started and a confirmation against where it landed, and in both
// cases the question is the same: has anything newer been applied?
//
// This is the guard a client that reads State() and a clock cannot have. The
// engine's events trail the state they describe, a setting is not monotonic —
// it can go plan → agent → plan — and the model's own optimistic value is not
// the session's, so "read the snapshot and compare" answers the wrong question.
// The revision of the last delta applied is the one thing that only ever moves
// forward (plan 021 §3.8, panel astra 15).
func mayApply(applied, at, rev uint64) bool {
	if rev > at {
		at = rev
	}
	return applied <= at
}

// sessionOwner is the one record of which engine — and so which session — the
// program holds. Model.eng is a plain field, so every copy bubbletea makes
// carries its own, and the session a picker builds lives only in the copies made
// after it. On a quit that is harmless: p.Run hands back the last model. On a
// recovered Update or View panic it hands back nil, and the model Run started
// with still holds whatever New gave it — often nothing — so the agent the
// picker spawned would never be closed and its process group never signalled.
// Model carries the owner as a pointer, allocated in New, so every copy shares
// it, and the exit tails close what it holds rather than what some copy
// remembers.
//
// It holds the ENGINE and not the session (plan 021 §3.1): closing the engine
// closes the session, and closing only the session would leave the engine's
// driver goroutine running and its last events unpublished.
type sessionOwner struct {
	mu  sync.Mutex
	eng *engine.Engine
}

func (o *sessionOwner) set(e *engine.Engine) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.eng = e
}

// current is the engine last set. The lock is released before it returns, so
// a caller never holds it across Close, which blocks until the agent is reaped.
func (o *sessionOwner) current() *engine.Engine {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.eng
}

// setSession is the only way the model's session is assigned: it wraps s in the
// engine that drives it and writes m.eng, m.sess and the owner together, so the
// three can never name different sessions and a new assignment site cannot
// forget either the engine or the owner.
//
// The engine is built here rather than in tui.Config because Config.Session
// stays an agent.Session: internal/cli hands the TUI a provider session, the
// pickers build one when a row or a provider is chosen, and every one of those
// paths lands here. Exactly one engine ever wraps one session — a second is
// refused by design — so a caller that swaps sessions closes the old engine
// first, which is the same call that closes the old session.
//
// crazeID is the durable craze session id this session already has: the crazeId
// of the row it was built to load, so that one thread of work keeps one
// identity across every agent session it is loaded into (session control
// SD-22). Every construction path supplies it — internal/cli through
// Config.CrazeSessionID for --continue, internal/cli's frame runner the same
// way, and the resume picker from the row it chose — and "" is a new session,
// which the engine mints an id for. The provider picker builds a NEW session
// and so passes "" deliberately.
func (m *Model) setSession(s agent.Session, crazeID string) {
	// A command belongs to the session it was run from — its workspace is that
	// session's — so a session change ends it. It does not *wait* for it: this
	// runs inside Update, on the one goroutine bubbletea draws from, and a
	// teardown bounded in seconds (shellController.shutdown) would be that many
	// seconds of frozen frame for a user who only picked a session. The kill
	// starts here and finishes on the run's own goroutine, which is what ends
	// every command anyway: it sends the group its last SIGKILL before it
	// returns, whatever stopped it, and the row settles when the shellDoneMsg
	// lands, exactly as it does for Esc. Nothing can be running on the paths
	// that reach here today — the pickers run before the session is ready, and
	// shell mode is refused until it is — so this is the guarantee the next
	// assignment site inherits rather than one being used now.
	m.shell.cancel()
	// What the last session's commands printed is not context for the next
	// one's first message: a different agent, and usually a different
	// workspace, being told about a `git status` nobody ran there (§3.6). Two
	// halves, because a command outlives this Update: what has already finished
	// is dropped, and the run still dying is disowned, so the result that lands
	// after this cannot put itself back (shellController.disown).
	m.shell.disown()
	m.dropShellContext()
	m.eng, m.sess, m.engErr = nil, nil, nil
	m.client, m.cmdSeq, m.chains = "", 0, nil
	m.owner.set(nil)
	if s == nil {
		return
	}
	// The zero ChainPolicy is the TUI's: Esc stops a turn and the queue behind
	// it carries on, and a prompt the session refuses is shown as the refusal it
	// is rather than waited out (engine.ChainPolicy).
	eng, err := engine.New(s, engine.Options{
		CrazeSessionID: crazeID,
		Index: engine.IndexOptions{
			Store: m.sessionIndex,
			CWD:   m.cwd,
			// The provider a row is recorded under before the session has
			// answered with one of its own: the resolved default it was
			// started as.
			Provider:  m.providerDefault.Name(),
			Hidden:    hiddenProvider,
			TitleLine: indexTitleLine,
		},
	})
	if err != nil {
		m.engErr = err
		m.sess = s
		return
	}
	m.eng, m.sess = eng, eng.Session()
	m.client = eng.NewClientID()
	// A new client, so a new order: a chain still running on the engine this
	// replaced orders nothing on this one.
	m.chains = &chainLock{}
	m.owner.set(eng)
}

// nextCmd is the model's next command id. Every mutating engine call carries
// one, so the events it causes name their cause and the model can tell its own
// effects' echoes from another client's change (§3.2).
func (m *Model) nextCmd() engine.Command {
	if m.client == "" {
		return engine.Command{}
	}
	m.cmdSeq++
	return engine.Command{Client: m.client, ID: fmt.Sprintf("%d", m.cmdSeq)}
}

// nextCmds is n command ids at once, for an Update that hands a tea.Cmd more
// than one call to make: the closure runs on another goroutine and may not
// touch the model, so every id it can spend is minted here. One it turns out
// not to need is simply a number nobody used.
func (m *Model) nextCmds(n int) []engine.Command {
	cmds := make([]engine.Command, n)
	for i := range cmds {
		cmds[i] = m.nextCmd()
	}
	return cmds
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
		cwd:             cwd,
		model:           cfg.Model,
		yolo:            cfg.Yolo,
		mouseEnabled:    !cfg.NoMouse,
		providerLocked:  cfg.ProviderLocked,
		persistProvider: cfg.PersistProvider,
		fallbackDefault: cfg.FallbackDefault,
		providerDefault: prov,
		providers:       pickerRows(cfg.Providers, prov),
		newSession:      cfg.NewSession,
		loadSession:     cfg.LoadSession,
		resume:          resumeRows(cfg.Resume),
		sessionIndex:    cfg.SessionIndex,
		crazeID:         cfg.CrazeSessionID,
		terminalTitle:   cfg.TerminalTitle,
		host:            cfg.Host,
		sessProvider:    prov.Name(),
		// Discard until Run says otherwise: a model built by a test, by
		// `craze frame` or by any direct caller writes no OSC at all.
		term:  newTerminalColors(io.Discard),
		owner: &sessionOwner{},
		shell: newShellController(),
		// Allocated here, not on first use, so every copy of this model holds
		// the same panes from the start (see pane).
		main: &pane{},
		subs: make(map[string]*pane),
		// A load is replaying before its first event: see Model.replaying.
		replaying: cfg.Loading,
		// Turn 1 is the session before the first prompt: every event has an
		// identity from the start, and no engine turn carries it.
		turnSeq: 1,
	}
	m.git = discoverGit(cwd)
	m.branch = m.git.branch()
	sess := cfg.Session
	switch {
	case len(m.resume) > 0:
		// --resume outranks the provider picker: every row carries its own
		// provider and choosing one locks it, so asking which provider to
		// start before asking which session to load would be asking a
		// question the answer overrides (§3.1).
		m.pickingResume = true
		m.dialog = dialogResume
	case m.newSession != nil && !m.providerLocked:
		m.pickingProvider = true
		m.dialog = dialogProvider
		m.providerCursor = m.providerIndex(prov)
	default:
		if sess == nil && m.newSession != nil {
			sess = m.newSession(prov)
		}
		if sess == nil {
			sess = NewStub()
		}
	}
	// Once the switch has decided: a picker starts with whatever Config.Session
	// was, usually nothing, and its own setSession replaces it. Config's craze
	// id belongs to Config.Session — the row --continue resolved — so a picker
	// that builds another session carries its own row's id instead.
	m.setSession(sess, m.crazeID)
	m.refreshSnap()
	if m.model == "" && m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
	}
	if m.model == "" {
		m.model = "default"
	}
	return m
}

// Run returns whether the agent's own diagnostics should print after exit —
// broader than just an agent exit, see finishRun — and the start failure, if
// any (§3.7.3).
func Run(cfg Config) (bool, error) {
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
	if cfg.Background {
		// Set once, before the first frame: the pair goes through the same
		// lock bubbletea renders through, so it can never tear a frame.
		m.term = newTerminalColors(out)
		m.term.apply(m.theme)
	}
	p := tea.NewProgram(m, opts...)
	// A terminal hangup — what a closed tab or window sends — is an exit like
	// SIGTERM. Left to its default action it kills craze before the tail
	// below runs, and the agent, in a process group of its own, is never
	// signalled. bubbletea turns SIGTERM into a QuitMsg; p.Quit sends that
	// same message, so SIGHUP lands on precisely the SIGTERM path. No agent
	// exists before this registration: the session's Start runs only from inside the
	// event loop.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	stop := make(chan struct{})
	handlerDone := make(chan struct{})
	// Written by the handler, read only after the join below.
	hungUp := false
	go func() {
		// Stopped in this goroutine's own defer, as bubbletea's handler does,
		// and before handlerDone closes, so the join below means SIGHUP is
		// unregistered. From the first SIGHUP onward the default action is
		// back for the whole of bubbletea's shutdown and the tail, so a
		// second hangup can still end a shutdown that is stuck.
		defer func() {
			signal.Stop(hup)
			close(handlerDone)
		}()
		select {
		case <-hup:
			hungUp = true
			// A no-op once the program has stopped, and safe before p.Run
			// starts: Send waits for the event loop or for p.Run's cancel.
			p.Quit()
		case <-stop:
		}
	}()
	final, err := p.Run()
	// The exit that was not a hangup unregisters too, before the tail, just as
	// bubbletea's own handler has stopped by the time p.Run returns.
	close(stop)
	<-handlerDone
	// A hangup that arrived after p.Run returned, and before the handler
	// stopped listening, is still waiting in the channel.
	select {
	case <-hup:
		hungUp = true
	default:
	}
	err = runErrAfterHangup(err, hungUp)
	showAgentDiag, startErr := finishRun(out, final, m, cfg.Host)
	// p.Run's own error is folded in here too: a recovered panic or another
	// run failure is reason enough to show the agent's stderr, whatever
	// finishRun made of the session close (§3.7.3).
	showAgentDiag = showAgentDiag || err != nil
	if err != nil {
		return showAgentDiag, err
	}
	// A quit is clean unless the session never started. Only startCmd's
	// failure counts: an error mid-session leaves a usable craze, and quitting
	// out of one is a normal exit.
	return showAgentDiag, startErr
}

// runErrAfterHangup is p.Run's error once a terminal hangup has ended the
// program. By then the terminal is gone, and bubbletea may report that itself:
// a read racing the hangup of a pty can fail with EIO rather than read EOF,
// and that comes back as an input error. None of it is craze failing — a
// hangup is an exit like SIGTERM, which exits 0 — so it is dropped. A
// recovered panic is kept: that run failed whatever the terminal did.
func runErrAfterHangup(err error, hungUp bool) error {
	if hungUp && err != nil && !errors.Is(err, tea.ErrProgramPanic) {
		return nil
	}
	return err
}

// finishRun is Run's exit tail, in the order plan 015 §3.2 pins: the tab title
// is cleared, the terminal's colours are reset, the host hub releases, and the
// session closes. p.Run returns on /exit, on SIGINT/SIGTERM, on SIGHUP (Run's
// own handler) and on a recovered panic, and every one of them lands here.
//
// final is whatever p.Run handed back, which on a recovered Update or View
// panic is nil; m is the model Run started with. The hub arrives as its own
// argument — Config.Host, never a field read through final — so the panic path
// still releases the pane: herdr leaves a pane that was never released showing
// the last state it was told. The session comes from m's owner, never from
// final, for the same reason.
//
// It returns the start failure final carries, if any, and — issue #23 —
// whether the agent's own stderr should print: the broader "the run failed"
// predicate, not just an agent exit, so it is named for what it decides
// rather than for the issue. That predicate is startErr != nil, or the
// session never having started at all (a nil final — a recovered panic —
// leaves started false, same as one that never got startedMsg), or the
// agent's own exit having been reaped before this Close on it sampled that.
// internal/cli folds in a fourth term, p.Run's own error, which this frame
// never sees.
func finishRun(out io.Writer, final tea.Model, m Model, h Host) (bool, error) {
	var startErr error
	started := false
	if fm, ok := final.(Model); ok {
		startErr = fm.startErr
		started = fm.started
		// Every exit path lands here: clearWindowTitle no-ops when titles
		// were off or a title was never set, and otherwise writes OSC 2
		// through the same writer bubbletea rendered into (§3.10).
		clearWindowTitle(out, fm)
	}
	// Unconditional, and immediately after the title clear: the controller is
	// a pointer m and fm share, reset is a no-op unless a set was written, and
	// this also covers a final that is not a Model. The terminal gets its own
	// colours back on every exit path — before the engine's Close, which may block.
	m.term.reset()
	// The same reasoning, and the same shared pointer: this is the only place
	// SIGTERM, SIGHUP and a recovered panic reach, and none of them ran
	// requestQuit. A quit craze asked for has already done this, and a second
	// shutdown with nothing running is a no-op.
	m.shell.shutdown()
	// Before the engine's Close for the same reason: the release is bounded, and the
	// session's close is not. On a quit craze asked for, requestQuit has
	// already done both in this order and the hub's Close is idempotent; on
	// SIGTERM, SIGHUP or a recovered panic this is the first and only release.
	closeHost(h)
	// The owner and not m.eng: m is the model Run started with, and a session
	// a picker built after it lives only in later copies — which a recovered
	// panic does not hand back. Every copy shares the owner, so it names the
	// engine the program ended with on every exit path. A zero Model has none.
	// Closing the engine closes its session — and stops its driver first, so
	// nothing is left running behind the program — and answers with what the
	// session's own Close said.
	var agentExited bool
	if m.owner != nil {
		if eng := m.owner.current(); eng != nil {
			agentExited = errors.Is(eng.Close(), agent.ErrAgentExited)
		}
	}
	failed := startErr != nil || agentExited || !started
	return failed, startErr
}

// Init starts the session and arms the event reader in the same batch. The
// reader cannot wait for startedMsg: a session/load replays its whole
// transcript from the client's read loop *during* Start, and a replay longer
// than the session's 256-slot event channel would block that read loop, so the
// session/load result would never be read and Start would never return (§2.2).
//
// Applying events before started is safe because nothing in applyEvent depends
// on m.started: the engine authors no turn event before it is started, cancelTurn
// is a no-op with no cards and no running turn, refreshSnap is idempotent, and
// the only card-producing events — permission, question and plan — are requests
// an agent makes of a live turn and never appear in a replay (§2.1).
func (m Model) Init() tea.Cmd {
	if m.picking() {
		return nil
	}
	return tea.Batch(m.startCmd(), waitEvent(m.eng))
}

// startCmd starts the session through the engine, whose gate opens on it: until
// it has returned the engine admits no command at all.
func (m Model) startCmd() tea.Cmd {
	// Neither of these names an engine: there is none to name, and the failure is
	// this model's however its copies move on.
	if err := m.engErr; err != nil {
		return func() tea.Msg { return errMsg{err: err} }
	}
	eng := m.eng
	if eng == nil {
		return func() tea.Msg {
			return errMsg{err: fmt.Errorf("craze: no session")}
		}
	}
	return func() tea.Msg {
		if err := eng.Start(context.Background()); err != nil {
			return errMsg{err: err, eng: eng}
		}
		return startedMsg{eng: eng}
	}
}

// staleFor reports that a start command's message is about an engine the model no
// longer holds — one a picker closed and replaced — so nothing it says applies.
func (m Model) staleFor(eng *engine.Engine) bool { return eng != nil && eng != m.eng }

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
	// A hidden answer the outbox refused for room is retried from here for the
	// same reason: it is the one place every transition passes through, and the
	// retry must not depend on another event ever arriving (armHiddenRetry).
	if retry := next.armHiddenRetry(); retry != nil {
		cmd = tea.Batch(cmd, retry)
	}
	// The tick chain is batched last, so a test can run the handler's own
	// command without waiting out a timer.
	if tick := next.armTick(); tick != nil {
		cmd = tea.Batch(cmd, tick)
	}
	// The one place every transition passes through on its way to the frame:
	// no handler sets the title itself, so no transition can miss it, and
	// nothing is written on a tick because this only fires on change (§3.10).
	if next.terminalTitle {
		if title := next.windowTitle(); title != next.lastTitle {
			next.lastTitle = title
			cmd = tea.Batch(cmd, tea.SetWindowTitle(title))
		}
	}
	// The host status rides the same choke point for the same reason, and is
	// handed to the hub on change alone, so a tick never reaches its lock
	// (plan 015 §3.1).
	if next.host != nil {
		next.publishHost()
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

	case hiddenRetryMsg:
		m.handleHiddenRetry()
		return m, nil

	case startedMsg:
		// Half of the gate: Start has returned. For a new session that is the
		// whole of it, and sessionUp runs from here exactly as it always has;
		// for a loaded one the replay may still be draining, in which case
		// EventReplay{end} runs the tail instead. The event reader is not
		// re-armed here — Init armed it, and eventMsg re-arms it after that.
		//
		// The engine is told too. startCmd's own Start has already opened its
		// gate in the ordinary run; this is the same fact arriving by the route
		// the model actually learns it on, and saying it twice changes nothing.
		if m.staleFor(msg.eng) {
			return m, nil
		}
		if m.eng != nil {
			m.eng.Started(nil)
		}
		m.started = true
		m.branch = m.git.branch()
		m.refreshSnap()
		if m.persistProvider && (!m.fallbackDefault || m.pickedExplicit) {
			name := m.snap.Provider.Name
			if name == "" {
				name = agent.CursorProvider().Name()
			}
			// A hidden provider is never written as the default (plan 018
			// §3.4): trying it once must not change what a plain craze
			// starts. A session that has not reported its provider yet was
			// started as the resolved default (as in writeIndex), so a hidden
			// default is skipped too rather than saving the cursor fallback.
			hidden := hiddenProvider(name) || (m.snap.Provider.Name == "" && m.providerDefault.Hidden())
			if !hidden {
				if err := SaveProvider(name); err != nil {
					m.addError(err.Error())
				}
			}
		}
		m.sessionUp()
		return m, nil

	case errMsg:
		// errMsg is only ever startCmd's: the session never came up. The TUI
		// stays on screen so the error is readable, but craze must not exit 0
		// afterwards, so the failure rides out on the final model. The engine
		// hears the same thing, and admits nothing from here.
		if m.staleFor(msg.eng) {
			return m, nil
		}
		if m.eng != nil {
			m.eng.Started(msg.err)
		}
		m.status = statusError
		m.err = msg.err.Error()
		m.startErr = msg.err
		m.addError(m.err)
		return m, nil

	case actionErrMsg:
		m.addError(msg.err.Error())
		return m, nil

	case revertModeMsg:
		if msg.gen != m.modeGen {
			// Stale: a newer request of the user's own is what the chip is
			// showing, so this refusal may neither roll the chip back to a
			// mode two changes ago nor drop the newer request's protection.
			// The error is still theirs to see — the agent refused something
			// they asked for, and swallowing that would be a bug of its own.
			m.addError(msg.err.Error())
			return m, nil
		}
		// The revert is the last word on this request, so nothing may put the
		// refused mode back on the chip afterwards. prev is what the chip
		// showed before it asked; the session is the real authority, and with
		// the flag down nothing masks it any more, so it is read straight back
		// — an agent-initiated mode that landed while this was on the wire is
		// on the chip rather than lost behind prev. refreshSnap is a no-op
		// before a session exists, which is what leaves prev in place then.
		m.modeInFlight = ""
		if mayApply(m.modeRev, msg.at, 0) {
			m.snap.CurrentMode = msg.prev
		}
		m.refreshSnap()
		m.addError(msg.err.Error())
		return m, nil

	case modeAppliedMsg:
		return m.modeSettled(msg.gen), nil

	case revertModelMsg:
		// The guard /model never had: another client — or the agent — may have
		// changed the model while this request was in flight, and a refusal
		// about a value nobody is on any more must not put its prev back. The
		// error row is still the user's to see either way.
		if mayApply(m.modelRev, msg.at, 0) {
			m.snap.CurrentModel = msg.prev
			m.model = msg.prev
		}
		m.addError(msg.err.Error())
		return m, nil

	case modelUnreadMsg:
		// Read back rather than put back: the snapshot is the model the
		// session is on as far as anyone can say — the one before the call,
		// or whatever the agent or another client has moved it to since.
		m.refreshSnap()
		m.addError(m.unreadModelText())
		return m, nil

	case modelApplyMsg:
		// The steps that landed are the truth; the one that did not is named.
		for _, st := range msg.done {
			m.addNote(st.note)
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
		// After the rows are settled, because a model step whose answer could
		// not be read names the model the screen then shows.
		switch {
		case msg.unread:
			m.addError(m.unreadModelText())
		case msg.err != nil:
			m.addError(msg.step + ": " + msg.err.Error())
		}
		return m, nil

	case refreshSnapMsg:
		m.refreshSnap()
		return m, nil

	case effortNotAppliedMsg:
		// The model step landed, so the rows are read back as the landed effort
		// step's refreshSnapMsg reads them, and then the note says why the
		// effort did not follow it.
		m.refreshSnap()
		m.addNote(msg.note)
		return m, nil

	case shellDoneMsg:
		// The command is over; the row it opened says how it went.
		m.finishShell(msg)
		return m, nil

	case cancelFailedMsg:
		// The error is this model's to show. A send-now that was waiting on
		// this cancel is the engine's, and it disarms it in the section that
		// releases the cancel's hold, with the cancel_failed reason the delta
		// carries — so the note arrives on that event and not from here.
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
		// whatever else has happened meanwhile — and so is its snapshot.
		m = m.modeSettled(msg.gen)
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
		tm, cmd := m.update(revertModeMsg{gen: msg.gen, prev: msg.prev, err: msg.err, at: msg.at})
		next := tm.(Model)
		if msg.seq == next.turnSeq && next.planDeadSeq != next.turnSeq {
			next.planOfferSeq = next.turnSeq
		}
		return next, cmd

	case eventMsg:
		// Every ending is one event now, the engine's EventTurn{ended}, and
		// everything a settled turn left to do — the drain, an armed send-now,
		// the queue the chain policy clears — is the engine's own decision,
		// arriving as the events it authored. There is nothing left for the
		// model to settle here but the reader it re-arms.
		m.applyEvent(msg.ev)
		return m, waitEvent(m.eng)

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
	rows := m.cur().rows
	for i := len(rows) - 1; i >= 0; i-- {
		if e := rows[i]; e.kind == entryAssistant && e.text != "" {
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
		if m.pickingResume {
			// Swallowed, and nothing else: a click outside a pre-start
			// picker that closed it would leave the model with no session
			// and no start command — a craze that draws a frame and can
			// never do anything (§3.7). The provider picker has a default to
			// start; there is no default session.
			return m, nil
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
	case dialogResume:
		return m.resumeDialogClick(i)
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
		// A running command outranks the whole Ctrl+C state machine, and is the
		// only way to stop one while a card has the keyboard (a card takes Esc
		// before the ladder below ever sees it). One press, one kill: it does
		// not quit, it does not cancel the agent's turn, and it does not arm
		// the double-press window — the user stopped the thing they started,
		// and nothing else about the session changed (plan 022 §3.6).
		if m.shellRunning() {
			m.killShell()
			return m, nil
		}
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
	case dialogResume:
		return m.handleResumeDialogKey(msg)
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
		// The ladder's first rung: Esc stops the command the composer is
		// running. Everything that takes Esc *earlier* — a card, a dialog, the
		// confirm line, the sub-agent view, the two focus bands — keeps its
		// meaning, because a layer that owns the keyboard owns this key too;
		// under one of those, Ctrl+C is how a command is killed.
		if m.shellRunning() {
			m.killShell()
			return m, nil
		}
		// With nothing running, Esc in shell mode clears the draft — leaving
		// the mode is leaving the draft, there being nothing else to it. It
		// returns here rather than falling through, so a `!` in the composer
		// can never be the key that cancels the agent's turn below.
		if m.shellMode() {
			m.input.SetValue("")
			m.resetSlash()
			return m, nil
		}
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
		if m.sendNowPending() {
			// The cancel is still in flight; taking the send-now back here is
			// what takes the text back before it turns into a turn.
			m.withdrawSendNow("send now dropped")
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
// under the cursor would be one more thing to undo. It says nothing about the
// send-now it took back either — Ctrl+C is already the whole answer — which is
// why the withdrawal is applied here with no note and its own delta skipped.
func (m *Model) clearPending() {
	m.confirm = nil
	m.withdrawSendNow("")
	if m.queueEdit != "" {
		m.cancelQueueEdit()
	}
	if m.eng != nil {
		_, _ = m.eng.ClearQueue(m.nextCmd())
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
	if ok && (name == "exit" || name == "rename") {
		// The two builtins that run before the session is up. Quitting has
		// always had to; /rename joins it because the gate below refuses
		// silently, and a rename typed at a session that is still restoring
		// owes the user the reason rather than nothing at all (§3.6). Neither
		// touches the wire, and neither is reachable while a card is up —
		// handleKey hands the keyboard to the card before Enter gets here.
		return m.runBuiltin(name, args)
	}
	// The offer outranks the sub-agent rows: an empty composer under a live
	// offer means "build it", and the rows stay reachable for an empty
	// composer without one. The test is Value()=="" and not composerEmpty,
	// so Enter agrees with the placeholder the user is looking at.
	if m.planOffering() && m.input.Value() == "" {
		return m.implementPlan()
	}
	if m.cardOpen() || !m.sessionReady() {
		return m, nil
	}
	// Shell mode sits behind that gate rather than in front of it: two of its
	// three refusals — a card on screen, a session still restoring, the draft
	// kept in both — are exactly what the gate already is. It sits in front of
	// the builtins because `!` is not a slash and parseSlashLine found nothing
	// above, and in front of send() because the draft is not going to the
	// agent. A command is allowed to run while the agent is working: it is the
	// user's own shell and it needs nothing of the session but its workspace.
	if m.shellMode() {
		return m.runShellDraft()
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
	// One admission, whatever the model happens to be showing: "send this, and
	// queue it if it cannot go now". The engine decides which, in the section
	// that would claim the turn, and it is the only thing that knows — the
	// model's view of a running turn, or of a turn the agent started on its own,
	// lags the engine by however long an event takes to be delivered. Deciding
	// here instead is how a follow-up got stranded: the model thought a turn was
	// running and called Queue, which starts nothing and wakes nothing, on an
	// engine that had already gone idle.
	return m.send()
}

// runsWhileWorking names the builtins that do not need the agent and are
// therefore answered mid-turn rather than refused. The rest keep today's
// silent refusal.
func runsWhileWorking(name string) bool {
	switch name {
	case "exit", "help", "theme", "tasks", "clear", "rename":
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

// send is Enter on the composer: the draft goes, as a turn if the engine can
// start one and as a queued row if it cannot. The composer is cleared by the
// submit, and only if the text was accepted — a refusal leaves it for the user to
// shorten or send later, which is what a full queue has always done.
func (m Model) send() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	if !m.sessionReady() {
		// The composer refuses Enter before the gate opens: a prompt into a
		// session that is still restoring would race the replay it is reading.
		return m, nil
	}
	next, _, _ := m.submitOwn(text, engine.SubmitQueue)
	return next, nil
}

// sendText starts a turn with text of craze's own: today the plan offer's
// implement prompt, which never touched the draft.
//
// It carries no shell context. The block belongs to the message the user wrote
// — it is their command's output, in front of their question about it — and the
// offer's prompt is craze's sentence, sent by pressing Enter on an empty
// composer. Attaching it here would spend the context on a message that never
// asked for it (§3.6).
func (m Model) sendText(text string) (tea.Model, tea.Cmd) {
	if !m.sessionReady() {
		// The plan offer refuses for the composer's reason, above.
		return m, nil
	}
	next, _, _ := m.submit(text, engine.SubmitQueue, "")
	return next, nil
}

// submitOwn is submit for the text this composer is holding: the draft's send,
// whether it starts a turn or becomes a queued row, and the confirm's send-now
// of that same draft. It is the one submission that carries the pending shell
// context, and it is what clears it — only once the text was accepted, so a
// refusal leaves the block with the draft it belongs to, for the send that
// follows.
func (m Model) submitOwn(text string, mode engine.SubmitMode) (Model, engine.SubmitResult, error) {
	next, res, err := m.submit(m.withShellContext(text), mode, "")
	if shellContextTaken(res, err) {
		next.dropShellContext()
	}
	return next, res, err
}

// submit hands one prompt to the engine and applies what it answered. It is the
// one place the model does: a plain send, the plan offer's implement prompt, a
// row's send now, and a confirmed send-now all come through here, so the echo
// rule has one synchronous half to match.
//
// Exactly one thing happened, and the model applies exactly that one:
//
//   - a turn started while the model had nothing running, so the row is drawn,
//     the status goes working and the turn is stamped in this very Update — which
//     is what a frame capture right after Enter sees — and that turn id is the one
//     started event the model will skip;
//   - a turn started while the model was still displaying another one as working:
//     nothing is drawn, and the turn is recorded as the pending successor
//     (nextTurn), which is what stops the model drawing it ahead of the events of
//     the turn before it. The text is accepted either way, so the draft goes;
//   - a send-now was armed, so nothing is drawn and nothing leaves the queue or
//     the composer, but the cancel mask goes up here: the cancel that makes room
//     for the send is the engine's own now, so cancelTurn is not the one setting
//     it (§3.4);
//   - the text was queued, or the row it named is still queued, so the band is
//     all that changed;
//   - or it was refused, which is one line for the user. Every one of these
//     refusals was unreachable before the engine — Begin could not refuse a
//     prompt the model had already gated — so what matters is that a prompt which
//     went nowhere says so.
//
// It waits on nothing: Submit is one locked section in the engine and calls
// nothing that blocks.
func (m Model) submit(text string, mode engine.SubmitMode, fromRow string) (Model, engine.SubmitResult, error) {
	if m.eng == nil {
		return m, engine.SubmitResult{}, nil
	}
	if fromRow != "" {
		// The engine sends the row's own text, so the row the model draws is
		// read from the band it is looking at rather than from whatever the
		// caller remembered. In S1b there is one client, so the two are the same
		// text; a second client editing a row between the read and the submit is
		// S2's problem, with a receipt to answer it.
		if row, ok := m.queuedRow(fromRow); ok {
			text = row.Text
		}
	}
	c := m.nextCmd()
	res, err := m.eng.Submit(c, text, mode, fromRow)
	switch {
	case err != nil:
		m.note(submitErrNote(err))
	case res.Turn != "":
		if m.status == statusWorking {
			// The engine had finished the turn on screen and the model has not
			// applied its ending yet. Record the successor and draw nothing: its
			// row is the started event's, in order behind everything the turn
			// before it still owes.
			m.nextTurn = res.Turn
			if mode == engine.SubmitSendNow {
				// A send-now confirmed in this window started at once instead of
				// arming, so there is no cancel — but the cards of the turn it
				// was meant to replace go, as they did when this window took the
				// cancelTurn branch.
				m.maskCards()
			}
		} else {
			m.beginTurn(res.Turn, text, m.now())
			m.ownTurn = res.Turn
		}
		if fromRow == "" {
			m.clearMatchingDraft(text)
		}
	case res.Armed:
		m.maskCards()
		if fromRow == "" {
			// The arm holds this composer's draft, and this command is what will
			// name it when it fires. A row-sourced arm holds nothing of the
			// composer's, so it leaves the marker alone rather than clearing it: an
			// older draft arm of this client's may still be on its way to firing.
			m.armedDraft = c.Cause()
		}
	case res.Queued != nil && fromRow == "":
		m.clearMatchingDraft(text)
	}
	m.refreshQueue()
	return m, res, err
}

// beginTurn is what one turn starting does to the model's view of the session,
// whether the model learned of it from Submit's own answer or from the started
// event the engine published — a drained row, an armed send firing, another
// client's prompt. One place, so the two can never drift.
//
// The first prompt's index seed used to be here; it is the engine's now, which
// is what makes it happen for a turn no client started (plan 021 §3.8).
//
// at stamps the user row: the client's clock for its own send, the started
// event's At for a turn the model learned of from the log.
func (m *Model) beginTurn(id, text string, at time.Time) {
	m.addUserAt(text, at)
	m.status = statusWorking
	// A new turn: whatever a cancel masked belonged to the turn before it.
	m.cardMask, m.cardMasking = "", false
	m.turnStart = m.now()
	m.err = ""
	m.cancelled = false
	m.prompted = true
	// The new turn's identity is the one thing that retires the last one: its
	// evidence, its offer and the kill that retired it all belong to a number
	// this turn no longer has. turnSeq stays the model's own count — the plan
	// offer is client-local UI — and turnID is the engine turn it names.
	m.turnSeq++
	m.turnID = id
}

// maskCards drops the cards a cancel has answered and retires the plan offer:
// what cancelTurn does synchronously, and what an armed send-now owes as well,
// because the cancel it asked for is made by the engine and answers every
// request the session was holding just the same.
//
// The mask goes up for a cancel with no turn of craze's own too, keyed to the
// empty turn id and cleared by the next beginTurn. That is safe now that the
// mask cannot swallow a live ask (maskDrops), and it is what stops an opening
// already in flight at that Esc from flashing a card up for the one Update
// before its own cancelled ending removes it — which the host's publications
// and a <wait:card> could both see.
func (m *Model) maskCards() {
	m.cards = nil
	m.cardMask, m.cardMasking = "", true
	if m.status == statusWorking {
		m.cardMask = m.turnID
	}
	m.retirePlanOffer()
}

// clearMatchingDraft takes the composer's text away if it is still the text that
// went. The text was trimmed on its way out and the draft may not have been, and a
// draft the user has changed since is theirs to keep — which is the rule a
// send-now has always followed, whether it fired at once or after a cancel.
//
// The comparison is against what was typed: the shell context craze put in
// front of it was never in the composer, so a draft matched against the whole
// sent string would never match and would sit there after its own send (§3.6).
func (m *Model) clearMatchingDraft(text string) {
	_, text = agent.SplitShellContext(text)
	if strings.TrimSpace(m.input.Value()) != text {
		return
	}
	m.input.SetValue("")
	m.resetSlash()
}

// queuedRow is the queued row id names, from the band the model is looking at.
func (m Model) queuedRow(id string) (agent.QueuedPrompt, bool) {
	for _, p := range m.queueItems() {
		if p.ID == id {
			return p, true
		}
	}
	return agent.QueuedPrompt{}, false
}

// submitErrNote is a refused Submit as one line.
func submitErrNote(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, engine.ErrNotAccepting):
		return "not ready to send yet"
	case errors.Is(err, engine.ErrAlreadyPending):
		return "send now already pending"
	default:
		return queueErrNote(err)
	}
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
	// and so did one whose chunks were all empty. Unless it had a plan
	// approved: native's plan is the file exit_plan_mode presented, and a turn
	// of write(plan) + exit_plan_mode says nothing in words (plan 023 §3.6).
	if m.sawAssistantSeq != m.turnSeq && m.planApprovedSeq != m.turnSeq {
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
	if m.viewing != "" || m.eng == nil {
		return m, nil
	}
	id := m.implementModeID()
	if id == "" {
		return m, nil
	}
	prev := m.snap.CurrentMode
	// Optimistic, the way applyMode is: the chip flips now and reverts if the
	// agent refuses, and the snapshot may not put the old mode back meanwhile.
	m.snap.CurrentMode = id
	m.modeGen++
	m.modeInFlight = id
	gen := m.modeGen
	// The offer is spent, not retired: a SetMode that fails leaves the plan on
	// screen, and that path is allowed to put the offer back.
	m.planOfferSeq = 0
	seq := m.turnSeq
	eng, cmd, at := m.eng, m.nextCmd(), m.modeRev
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), modeCallTimeout)
		defer cancel()
		if _, err := eng.Set(ctx, cmd, engine.Setting{Kind: engine.SettingMode, Value: id}); err != nil {
			return planImplementFailedMsg{seq: seq, gen: gen, prev: prev, err: err, at: at}
		}
		return planImplementMsg{seq: seq, gen: gen, mode: id}
	}
}

// modeCallTimeout bounds session/set_mode. Without one an agent that is alive
// but not answering leaves Conn.Call blocked for ever, no answer is ever
// produced, and the flag that overrides the chip is never taken down: the chip
// then lies for the life of the process. A cancel's 2 s is too tight for this
// one — a cancel interrupts an agent that is by definition mid-turn and
// listening, while set_mode can queue behind whatever the agent is doing — so
// this is long enough that a busy agent answering between tool calls still
// makes it, and short enough that a wedged one corrects itself while the user
// is still looking at the same screen. Timing out is an ordinary refusal: it
// returns the revert, which puts the chip back and clears the flag.
//
// The bound is real only because the engine honours it after the settings
// worker has claimed the request too: such a Set returns
// engine.ErrSetOutcomeUnknown on this context rather than waiting for a
// provider that may never answer (r25 finding 2). Here that error is an error
// like any other — the row, and the revert the revision guard allows — and if
// the change did land after all, its own delta corrects the mirror.
//
// A var rather than a const only so a test can shorten it: nothing in the
// program writes it.
var modeCallTimeout = 15 * time.Second

// cancelTurn drops the whole card queue and cancels. Cancel answers every
// request the session is still holding — permission, question and plan alike —
// with that kind's cancelled outcome, exactly once each, so the UI must not
// answer them itself and race it.
//
// The cancel names the turn the model is displaying, and the engine refuses it
// as stale if that turn is no longer the current one — which is what the seq
// check on cancelFailedMsg used to mean, decided by the one component that knows
// what is running rather than by a counter the model kept. That is also what
// makes the model's own view being behind harmless here: if the engine has moved
// on — a settlement drained a row whose started the model has not applied yet —
// the id names a turn that is over and the cancel is refused, rather than landing
// on the turn the user did not mean.
//
// With no turn of craze's own — cards on screen and nothing working — it names
// none: the agent may be holding a request that arrived between turns, and the
// cancel is what answers it.
func (m Model) cancelTurn() (tea.Model, tea.Cmd) {
	cards := len(m.cards)
	working := m.status == statusWorking
	if cards == 0 && !working {
		return m, nil
	}
	m.maskCards()
	eng := m.eng
	if eng == nil {
		return m, nil
	}
	turn := ""
	if working {
		turn = m.turnID
	}
	c := m.nextCmd()
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		res, err := eng.Cancel(ctx, c, turn)
		if err == nil {
			return nil
		}
		switch {
		case errors.Is(err, engine.ErrStaleTurn):
			// The turn this was for is already over; there is nothing to
			// report and nothing failed.
			return nil
		case res.Reported:
			// The failure has been published as a state delta — this cancel
			// disarmed a send-now, so an event was going out for it anyway — and
			// that delta carries both halves of what this message used to write,
			// in order: the note, then the row. Reporting it again from here would
			// draw the row twice, and in whichever order the two arrived.
			return nil
		}
		return cancelFailedMsg{err: err}
	}
}

// cancelFailedMsg says the cancel never reached the agent, and is the model's own
// report of it: it is returned only where the engine published nothing for the
// failure (engine.CancelResult.Reported).
type cancelFailedMsg struct{ err error }

// requestQuit closes the engine, which closes the session — answering every card
// still queued with its cancelled outcome on the way out — and stops the driver
// with it. The queue is left alone: the model is on its way out with it, and
// clearing it would only restart the tick chain.
//
// The host is released first, exactly as finishRun orders it (plan 015 §3.2):
// the close may block, and a pane that is never released stays showing craze's
// last state. It also means the cancelled cards the close produces are never
// reported, since a closed hub ignores Publish.
//
// finishRun closes the owner's engine again once p.Run returns, and that is this
// same engine: setSession writes both. The second Close serialises behind this
// one — Engine.Close runs once and answers every later caller with the same
// error — so it returns when the first does and cannot deadlock.
func (m Model) requestQuit() (tea.Model, tea.Cmd) {
	m.quitting = true
	eng := m.eng
	h := m.host
	sh := m.shell
	return m, func() tea.Msg {
		// The user's command goes first and craze waits for it: this is the
		// quit Ctrl+D, /exit and the second Ctrl+C all reach, and nothing the
		// composer started may outlive the program that started it. The wait is
		// bounded twice over (shellController.shutdown), so a command that will
		// not die delays the quit by seconds rather than blocking it.
		sh.shutdown()
		closeHost(h)
		if eng != nil {
			_ = eng.Close()
		}
		return tea.Quit()
	}
}

func (m *Model) applyEvent(ev agent.Event) {
	// An event means the log is moving, which is what a hidden answer the outbox
	// had no room for is waiting on (retryHidden) — the fast path, ahead of the
	// beat that guarantees the retry (armHiddenRetry). It runs before the event
	// is applied, so a hidden ask answered here is not counted twice by an
	// answerHidden this same event causes.
	m.retryHidden()
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
			m.addInterjectionAt(ev.Text, ev.At)
			return
		}
		if m.replaying || ev.Replayed {
			// A prompt out of the restored transcript, which craze never sent
			// and therefore never wrote. The session coalesces a multi-chunk
			// one into a single event, so this is one user block per prompt.
			m.addUserAt(ev.Text, ev.At)
			return
		}
		// A live main-session echo. grok and gx send one for every prompt the
		// user types, and craze has already written that block from its own
		// send, so taking this one would double it (§2.2).
		return
	case agent.EventReplay:
		if ev.Replay == nil {
			return
		}
		if ev.Replay.Phase != agent.ReplayEnd {
			// The start phase is informational: the model was built replaying
			// because Config.Loading knew a load was coming, and it had to be,
			// since tea.Batch could deliver startedMsg before this event ever
			// arrived (§3.5).
			return
		}
		// The restored snapshot is installed, so this is the moment the
		// session is up as far as the replay is concerned. breakStream first,
		// or the last replayed thought stays open and the note lands inside
		// it.
		m.breakStream(ev.At)
		m.addNoteAt(restoredNote, ev.At)
		m.replaying = false
		m.refreshSnap()
		m.sessionUp()
		return
	case agent.EventCommand:
		// It arrives before the request reaches the wire, so the line lands
		// under the user block craze has already written and above anything
		// the agent goes on to say.
		m.addCommandLine(ev.Command, ev.At)
		return
	case agent.EventQueue:
		// The band draws from the engine's queue, which refreshSnap re-reads;
		// nothing else has to happen.
		m.refreshSnap()
		if ev.QueueChange == agent.QueueRemoved && m.status == statusError {
			m.note("queue cleared")
		}
		return
	case agent.EventTurn:
		if ev.Turn == nil {
			return
		}
		switch ev.Turn.Phase {
		case agent.TurnStarted:
			m.applyTurnStarted(ev)
		case agent.TurnEnded:
			m.applyTurnEnded(ev.Turn, ev.At)
		}
		return
	case agent.EventForeignTurn:
		m.refreshSnap()
		if ev.ForeignTurn != nil && ev.ForeignTurn.Running {
			// What follows is the agent talking without a prompt of craze's.
			// The note is what stops the reply reading as an answer to the
			// last thing the user said.
			m.breakStream(ev.At)
			m.addNoteAt(foreignTurnNote, ev.At)
		} else {
			m.breakStream(ev.At)
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
			m.upsertTool(ev.Tool, ev.At)
		}
	case agent.EventTodos:
		m.refreshSnap()
		todos := m.todosOf(ev)
		m.noteTodoLifecycle(todos)
		m.noteTodos(todos, ev.At)
	case agent.EventPermission:
		if ev.Permission != nil {
			m.pushCard(card{kind: cardPermission, perm: ev.Permission}, ev.At)
		}
	case agent.EventQuestion:
		// An auto-answered request (headless) is already decided; only an
		// interactive one is a card.
		if ev.Question != nil && !ev.Question.Auto {
			if m.showAsk() {
				m.pushCard(card{kind: cardQuestion, ask: ev.Question}, ev.At)
			} else {
				// The config hides questions: it is skipped where it stands,
				// with no card and — as it always has — no row. The ending it
				// causes names this command and is skipped with the rest of
				// this model's own echoes.
				m.answerHidden(ev.Question.ID, agent.AskAnswer{Skip: true})
			}
		}
	case agent.EventAsk:
		if ev.Ask != nil {
			m.applyAskEnded(ev.Cause, ev.Ask)
		}
	case agent.EventPlan:
		if ev.Plan != nil && !ev.Plan.Auto {
			// The plan itself is transcript material; the card is only the
			// three answers it needs.
			m.addPlan(ev.Plan, ev.At)
			if m.showPlan() {
				m.pushCard(card{kind: cardPlan, plan: ev.Plan}, ev.At)
			} else {
				m.answerHidden(ev.Plan.ID, agent.AskAnswer{Reject: true})
			}
		}
	case agent.EventDone:
		// The wire's own ending. It orders the transcript — which is why it, and
		// not the turn's ending, is what decides whether the turn left a plan
		// behind — but it no longer settles the status: the engine's
		// EventTurn{ended} is the one ordered ending, and it knows whether
		// anything succeeds this turn. Going idle here would show the idle
		// between a cancelled turn and the row queued behind it that the old
		// code never showed (§3.4, A7).
		//
		// It carries no turn id, and needs none, because it cannot be late the
		// way an engine event can. It is the session's own and is published from
		// inside the continuation, before that continuation returns; the engine
		// settles a turn only once it HAS returned, and a turn only becomes
		// current either in that settlement's batch or through a Submit the
		// engine admits after it. So every route by which the model could be on
		// a later turn passes through an event the log ordered behind this one:
		// a done or an error for turn N is always applied while N is still the
		// turn on screen. Same for EventError below. (What this does still do is
		// what it did at the baseline: a turn the agent ran on its own ends here
		// too, and its reply can arm a plan offer. Unchanged, and not this
		// commit's to fix.)
		m.breakStream(ev.At)
		// The cancel mask is NOT cleared here. It belongs to a turn, and it is
		// the engine's ending for that turn that says every opening and ending
		// of it has been delivered — this event is the session's own and can
		// overtake an opening still in the outbox.
		//
		// The turn is over, so this is the one moment the branch can have
		// changed under craze. No polling, no resize hook.
		m.branch = m.git.branch()
		if ev.StopReason == stopCancelled {
			// Esc leaves nothing else behind: the spinner going away is the
			// only other sign the cancel landed, and it is indistinguishable
			// from the turn having finished on its own.
			m.cancelled = true
			m.addNoteAt(stopCancelled, ev.At)
		}
		if m.planEarnsOffer(ev.StopReason) {
			m.planOfferSeq = m.turnSeq
		}
		// A turn ended, so this session is the newest thing in the workspace —
		// which the engine's own observer records in the index (plan 021 §3.8).
	case agent.EventError:
		// The session's own failure, and the one place its row is drawn: the
		// turn's ending follows and only settles the status, which this has
		// already said — as it always did, without waiting for the other ending.
		// Like EventDone it cannot arrive under a later turn; see there.
		m.status = statusError
		m.confirm = nil
		if ev.Err != nil {
			m.err = ev.Err.Error()
			m.addErrorAt(m.err, ev.At)
		}
	case agent.EventMeta:
		if ev.State != nil {
			// A state delta: the engine's, or the session's own. The send-now
			// section is the whole of what S1b reads from one, and what it writes
			// for it is what the model wrote for the same event before the engine
			// existed — the toasts, and the one error row a failed cancel has
			// always left. A settings delta draws nothing at all (§3.8).
			m.applyStateDelta(ev)
		}
		if ev.Mode != "" {
			// Any mode update at all retires the offer, even one that names the
			// mode craze already thought it was in: the agent may have gone
			// plan → ask → plan, and comparing snapshots cannot see the trip.
			// Only an agent-initiated update fills Mode; a craze-initiated
			// change carries its payload in State alone (§3.8).
			m.retirePlanOffer()
		}
		m.refreshSnap()
		// session_info_update fills ev.Text: the agent named the session, and
		// the engine's observer writes that to the index as an agent title
		// (plan 021 §3.8). The composer's own title is read back by refreshSnap
		// above, as it always was.
	}
}

// hiddenAnswer is one answer to a hidden ask that has still to be taken
// (hiddenRetry).
type hiddenAnswer struct {
	id string
	a  agent.AskAnswer
}

// answerHidden answers an ask the config never shows a card for — a question or
// a plan hidden by the provider's own settings — where it arrives, with no card
// and no row, exactly as the model has always answered those. A failure is
// dropped for the same reason the row is: the user asked not to be shown this
// request at all.
//
// With ONE exception: agent.ErrAskUnavailable is the log's outbox refusing for
// want of room, before it mutated anything, so the ask is still open and the
// provider is still waiting — and no card will ever raise it, because this is
// the path that has none. The answer is kept and tried
// again, by the next event the model applies and by a beat of its own until one
// of them takes it (retryHidden, armHiddenRetry). It is bounded by the number of
// hidden asks open at once.
func (m *Model) answerHidden(id string, a agent.AskAnswer) {
	if m.eng == nil {
		return
	}
	cmd := m.nextCmd()
	switch err := m.eng.Answer(cmd, id, a); {
	case err == nil:
		m.noteAskEcho(cmd.Cause())
	case errors.Is(err, agent.ErrAskUnavailable):
		m.hiddenRetry = append(append([]hiddenAnswer(nil), m.hiddenRetry...), hiddenAnswer{id: id, a: a})
	}
}

// retryHidden re-sends the hidden answers the outbox had no room for. Each one
// is either taken, refused for good — the ask was resolved some other way in the
// meantime, agent.ErrAlreadyResolved among them — or kept for the next try by
// answerHidden itself. The list is taken first, so one that is kept is appended
// to an empty list rather than walked twice.
func (m *Model) retryHidden() {
	if len(m.hiddenRetry) == 0 {
		return
	}
	pending := m.hiddenRetry
	m.hiddenRetry = nil
	for _, h := range pending {
		m.answerHidden(h.id, h.a)
	}
}

// hiddenRetryEvery is how long a hidden answer refused for room waits before it
// is tried again. It is the spinner's own beat, which is short enough that the
// provider's wait is not felt and long enough to leave the drainer room to move.
const hiddenRetryEvery = 250 * time.Millisecond

// hiddenRetryMsg is one beat of the hidden-answer retry timer. It carries no
// generation, unlike the tick chain's tickMsg: armHiddenRetry never abandons a
// beat for a faster one, so there is no second chain a stale beat could belong
// to — and a beat that somehow arrived twice would only run retryHidden against
// an empty list. Control.Answer validates and claims atomically on every try, so
// a retry is a retry and never a second answer.
type hiddenRetryMsg struct{}

// armHiddenRetry keeps exactly one retry beat in flight while an answer to a
// hidden ask is still waiting for room, and none when nothing is.
//
// It belongs to Update, not to applyEvent, because the schedule the retry has to
// survive is the one where no further event ever arrives: the final batch's
// LAST event is what gives the outbox its room back, so
// the retry that event carries runs before commitBatch and is refused, the
// drainer goes idle, and nothing is left to try again. The hidden ask would stay
// open for ever with the provider waiting on it.
func (m *Model) armHiddenRetry() tea.Cmd {
	if len(m.hiddenRetry) == 0 || m.hiddenRetryLive {
		return nil
	}
	m.hiddenRetryLive = true
	return tea.Tick(hiddenRetryEvery, func(time.Time) tea.Msg { return hiddenRetryMsg{} })
}

// handleHiddenRetry spends one beat on the answers still waiting for room.
// Update arms the next one only if something was refused for room again, so a
// list that empties — taken, or ended by somebody else — stops the timer.
func (m *Model) handleHiddenRetry() {
	m.hiddenRetryLive = false
	m.retryHidden()
}

// noteAskEcho records that the ending caused by cause is this model's own: its
// effect was applied in the Update that asked for it, so the event is an echo.
// The slice is copied rather than written through, because every Model copy
// shares it.
func (m *Model) noteAskEcho(cause string) {
	if cause == "" {
		return
	}
	m.askEchoes = append(append([]string(nil), m.askEchoes...), cause)
}

// takeAskEcho reports whether cause is one of this model's own answers, and
// takes it off the list if it is. One entry per answer, removed by the one
// ending that answer causes: a cause that matched can never match twice.
func (m *Model) takeAskEcho(cause string) bool {
	if cause == "" {
		return false
	}
	for i, c := range m.askEchoes {
		if c != cause {
			continue
		}
		next := append([]string(nil), m.askEchoes[:i]...)
		m.askEchoes = append(next, m.askEchoes[i+1:]...)
		return true
	}
	return false
}

// applyAskEnded is one ask's ending. Every way an ask can end carries one now,
// so this is where a card the model did not answer itself goes away — another
// client's answer, a cancel, the turn it belonged to ending underneath it, the
// session closing — and where the row that answer earned is written.
//
// What it writes is exactly what the local path writes for the same outcome, so
// a question answered from a phone reads in this transcript as one answered
// here: the question notes for an answer, the skipped note for a skip, the
// verb for a plan. And exactly as little: nothing for a permission (a permission
// answer has never written a row), nothing for a cancel, a turn's end, a close
// or an automatic resolution, and nothing for an ask this model never raised a
// card for — the hidden paths, a card the cancel mask dropped, and an ending
// that carries its own Body, which by definition never had an opening.
func (m *Model) applyAskEnded(cause string, u *agent.AskUpdate) {
	m.notePlanApproved(u)
	if m.takeAskEcho(cause) {
		// This model's own answer, applied in the Update that sent it.
		return
	}
	c, ok := m.removeCard(u.ID)
	if !ok || u.Outcome != agent.AskAnswered {
		return
	}
	// The notes are client-local — written only by a client that had the card,
	// like the ones this client writes when it answers (commitQuestion,
	// skipQuestion, answerPlan) — so they keep this client's clock rather than
	// taking the ending's At (plan 024 §3.3).
	switch {
	case c.kind == cardQuestion && u.Skip:
		m.addNote(skipNote(c.ask))
	case c.kind == cardQuestion:
		m.addAnswerNotes(c.ask, u.Answers)
	case c.kind == cardPlan:
		m.addNote(planNote(c.plan, u.Accepted))
	}
}

// notePlanApproved records the turn an accepted plan belongs to, so a turn that
// left a plan behind and said nothing in words still earns the implement offer
// (planEarnsOffer, plan 023 correction 2). It is the first thing applyAskEnded
// does, ahead of the echo check, because the two routes to an accepted plan —
// this model answering the card itself, whose ending comes back as its own echo
// and is skipped, and an accept from anywhere else — must both land here.
//
// Only an answered acceptance counts. An automatic one (Auto) is craze's own
// headless policy, where nothing draws a card: the plan is never written to the
// transcript either (applyEvent's EventPlan arm skips an Auto plan), so an offer
// to implement "the plan above" would point at nothing on screen.
func (m *Model) notePlanApproved(u *agent.AskUpdate) {
	if u.Kind == agent.AskPlan && u.Outcome == agent.AskAnswered && u.Accepted {
		m.planApprovedSeq = m.turnSeq
	}
}

// removeCard takes the card for one ask out of the queue, wherever it is in it,
// and answers with it. The queue is copied rather than written through, because
// every Model copy shares the slice.
func (m *Model) removeCard(id string) (card, bool) {
	if id == "" {
		return card{}, false
	}
	for i, c := range m.cards {
		if cardAskID(c) != id {
			continue
		}
		next := append([]card(nil), m.cards[:i]...)
		m.cards = append(next, m.cards[i+1:]...)
		return c, true
	}
	return card{}, false
}

// cardAskID is the ask id a card answers.
func cardAskID(c card) string {
	switch {
	case c.perm != nil:
		return c.perm.ID
	case c.ask != nil:
		return c.ask.ID
	case c.plan != nil:
		return c.plan.ID
	default:
		return ""
	}
}

// applyTurnStarted is a turn the engine started, as an event. The echo rule is
// per effect: the one started the model skips is the turn Submit handed it back
// synchronously, because that Update has already drawn the row, gone working and
// stamped the turn. Every other one — a drained row, an armed send-now firing,
// another client's prompt — is a turn the model has applied nothing for yet, so
// it draws its row like any other (§3.4).
//
// The id is matched rather than a flag consumed, and ownTurn can hold at most one
// id, so no started can be skipped for the wrong turn and none can leave ownTurn
// standing. Two arguments, both about the engine:
//
//   - The started of the turn Submit reserved is enqueued in the same locked
//     section that reserved it, and the log delivers in order, so it arrives
//     before the started of any turn reserved after it. The first started the
//     model sees once ownTurn is set is therefore ownTurn's.
//   - Only one can be outstanding: a synchronous apply happens only when the
//     model is not already displaying a working turn, and it leaves it working,
//     so the next Submit either queues, arms, or is recorded as the pending
//     successor — none of which touches ownTurn.
//
// The one case that leaves ownTurn set for good is an engine closed between the
// reserve and the publish, where the enqueue is dropped with everything else at
// the cut. The program is quitting; nothing reads it again.
func (m *Model) applyTurnStarted(ev agent.Event) {
	if id := ev.Turn.ID; id != "" {
		if id == m.ownTurn {
			m.ownTurn = ""
			return
		}
		if id == m.nextTurn {
			// The pending successor, arriving in its place. Nothing was applied
			// for it, so it is drawn like any other started; what the marker did
			// was keep the model working until this moment.
			m.nextTurn = ""
		}
	}
	m.beginTurn(ev.Turn.ID, ev.Turn.Text, ev.At)
	if ev.Turn.Origin == agent.TurnOriginSendNow && ev.Cause != "" && ev.Cause == m.armedDraft {
		// The send-now this client armed from its composer, firing: it takes that
		// draft with it if the composer still holds it, and the marker goes with
		// it. Matched on the arming command and not on the origin, because a
		// send_now started is only this composer's business when it is the arm this
		// composer's own Submit made — a row-sourced send never held the draft, and
		// another client's send-now, or an older arm of this client's whose event is
		// only now arriving, must not take a draft accepted since.
		m.armedDraft = ""
		m.clearMatchingDraft(ev.Turn.Text)
	}
}

// applyTurnEnded is the turn's one ordered ending, and the whole of what settles
// the status. The endings the wire never produced arrive here as synthetic ones,
// and each writes exactly what the message that used to carry it wrote: a
// cancelled prompt its note, a refused prompt its one error row, and a turn that
// failed nothing at all, because the session's own EventError has already drawn
// that row.
//
// The status goes idle only with an empty Next AND no pending successor of the
// model's own. With a successor — a queued row the settlement drained, an armed
// send-now firing, or a turn Submit started while this one was still on screen
// (nextTurn) — the model stays working, so nothing with work in flight behind it
// ever shows an idle, and the host hub never publishes one (A7).
//
// # What settles, and what is only recorded
//
// An engine event trails the state it describes, so an ending can arrive after the
// model has started another turn. It is reachable: a turn that failed puts the
// model in its error state from the session's own EventError, which is published
// before the continuation returns and therefore before the engine can settle —
// and a direct Submit is admitted from an error state, so Enter in that window
// starts the next turn synchronously, with the ending of the one before it still
// in the log's outbox.
//
// So everything that settles the model — the status, m.err, cancelled, and the
// confirm with its note — applies only to the turn the model is displaying
// (m.turnID). Settling on a stale ending would put an error, or an idle, under a
// turn that is running: the host hub would publish Failed or Idle for work in
// flight, a <wait:idle> could match a state the old code never showed, and the
// next Enter would be routed by a status that is two turns out of date.
//
// The ROWS are the other half, and they are written whichever turn is current,
// because they are facts about the turn that ended and the transcript is a record:
// a late cancelled note, or a late refusal's one error row, landing under the next
// user block is the honest account of a late fact. That is also exactly what the
// baseline did — promptDoneMsg carried no turn at all and drew both rows
// unconditionally (app.go at 6581e0a) — so nothing a user sees moves here. Both
// rows are the ending event's, and are stamped at its At.
func (m *Model) applyTurnEnded(t *agent.TurnInfo, at time.Time) {
	if t.ID != "" && t.ID == m.cardMask {
		// The masked turn is over, and its ending is ordered behind every
		// opening and every ask ending that turn produced — the session ends
		// the turn for the registry and waits for the outbox before it
		// publishes its own ending, and this event is enqueued after that. So
		// there is nothing left to mask, and a request that arrives now belongs
		// to no turn this cancel touched.
		m.cardMask, m.cardMasking = "", false
	}
	// current is "this is the ending of the turn on screen". An id the model has
	// never seen is not it, and neither is "" — before any turn there is nothing
	// to settle.
	current := t.ID != "" && t.ID == m.turnID
	failed := t.Err != ""
	// cancelled is the synthetic ending the model has a word for. The engine
	// authors one other kind that is synthetic and did not fail — the ending it
	// gives the turn that was running when the session closed (engine's Close,
	// stop reason "closing") — and that one is not a cancel: it reaches Update
	// only while craze is already quitting, and a "cancelled" note under it
	// would be the wrong word for the last thing on the screen.
	cancelled := t.Synthetic && !failed && t.StopReason == stopCancelled
	switch {
	case cancelled:
		// Cancelled before the prompt's turn opened: while it was still waiting
		// for the agent's first command catalog, or right after Enter. Nothing
		// ran and nothing failed, so this is not an error state: it is the ending
		// a cancelled turn has, and the transcript owes the row it already drew
		// the same note — Esc leaves nothing else behind.
		m.addNoteAt(stopCancelled, at)
	case t.Synthetic && failed:
		// A prompt the session refused emits no event of any kind, so this is
		// the only place its row can be drawn.
		m.addErrorAt(t.Err, at)
	}
	if !current {
		return
	}
	if cancelled {
		// Part of the settlement and not of the row: it is what tells the idle
		// that follows Esc from the idle that follows an answer, so it belongs to
		// the turn the model is on.
		m.cancelled = true
	}
	if failed {
		m.status = statusError
		m.err = t.Err
		m.confirm = nil
		return
	}
	if m.confirm != nil {
		// The question was "cancel the running turn and send?" and the turn
		// answered it first. Nothing was taken from anywhere, so the draft
		// and the row are both still where they were.
		m.confirm = nil
		m.note("the turn ended first")
	}
	if t.Next == "" && m.nextTurn == "" {
		m.status = statusIdle
	}
}

// applyStateDelta is a state delta, written exactly as the model has always
// written the same news: the toast for each way an armed send-now can be lost,
// and — for a reason that is a failure — the error row beside it, in the order
// cancelFailedMsg produced the two. The send-now section is the whole of what S1b
// reads; no settings delta draws anything at all.
//
// The two halves are read independently, because a reason can stand without a
// section (agent.StateDelta): the note belongs to the send-now section, since it
// says what was lost, and the row belongs to the failure, since that happened
// whether or not a send was still there to lose. That is the shape of the one
// delta that carries a reason and no section — a cancel the engine made for an arm
// the client had already taken back, which still failed and still owes its row.
//
// An arm is not reported here: the model applied it from Submit's own answer, and
// the delta that says a send is armed carries no reason because nothing was lost.
//
// None of it is keyed to a turn, and none of it needs to be. A delta is about the
// armed send, of which there is one at a time, and what it writes is a note and a
// row: nothing here settles any state, so a delta that arrives late says something
// true about a send that is gone rather than contradicting the one that replaced
// it. Whether a send is armed *now* is read from the engine (sendNowPending),
// never from these events — and which arm a draft belongs to is armedDraft's, which
// this deliberately leaves alone.
func (m *Model) applyStateDelta(ev agent.Event) {
	st := ev.State
	// The settings sections draw nothing, and the mirror is still read from the
	// session (refreshSnap, below in applyEvent): what is taken from them here
	// is only their revision — the highest Seq applied per section — which is
	// what tells a delayed answer whether the value it is about is still the
	// current one (mayApply). It is recorded for every delta, this model's own
	// included: an echo is still a change that has been applied, and the
	// revision has to be able to overtake a request issued before it.
	if st.Mode != nil && ev.Seq > m.modeRev {
		m.modeRev = ev.Seq
	}
	if st.Model != nil && ev.Seq > m.modelRev {
		m.modelRev = ev.Seq
	}
	if st.Config != nil && ev.Seq > m.configRev {
		m.configRev = ev.Seq
	}
	// The note: what was lost, which only a cleared send-now section can say. It
	// does not touch armedDraft: this delta names the command that caused the
	// DISARM, not the one that armed what it retired, so clearing the marker from
	// here would be clearing whatever is armed NOW on the strength of an event
	// about something older. The marker needs no clearing — only the started that
	// names it can consume it, and a retired arm never produces one.
	if sn := st.SendNow; sn != nil && !sn.Armed {
		switch st.Reason {
		case agent.SendNowWithdrawn:
			if ev.Cause != "" && ev.Cause == m.disarmed {
				// This model's own Disarm, whose note it wrote in the Update that
				// asked for it. Anything else is somebody else taking it back.
				m.disarmed = ""
			} else {
				m.note("send now dropped")
			}
		case agent.SendNowOtherTurn:
			// It was armed against a turn that is no longer the one that just
			// ended, so it was not that turn's business.
			m.note("send now dropped")
		case agent.SendNowRowGone:
			// The row it named went some other way — the drain sent it, or it was
			// cancelled — so there was nothing left to send now.
			m.note("that message has already gone")
		case agent.SendNowCancelFailed:
			// The cancel never reached the agent, so the turn it would have
			// replaced is still running. The text is where it was.
			m.note("cancel failed")
		case agent.SendNowTurnFailed, agent.SendNowStopped, agent.SendNowClosing:
			// Silent, as they always were: the failure, the stop or the quit is
			// already the whole of what the user is being told.
		}
	}
	// The row: the failure behind the reason, which the engine fills only for a
	// cancel it made itself. A cancel this model asked for answers it directly,
	// and drawing the row from both would draw one failure twice.
	if st.Detail != "" {
		m.addErrorAt(st.Detail, ev.At)
	}
	// A session-index write that failed: the same row writeIndex drew itself
	// before the index moved into the engine, and §2.4's rule that it stays a
	// client-local one. It is drawn for this model's OWN commands too — a seed
	// is reported after Submit has already answered, so there is no return
	// value it could have come back on — and for the writes no command caused
	// at all (the agent's title, a turn's end), which is exactly the set the
	// model used to write rows for.
	if st.IndexErr != "" {
		m.addErrorAt(st.IndexErr, ev.At)
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

// modeSettled is SetMode coming back accepted, for gen. The snapshot is always
// re-read: the session is the authority on the mode, and refreshSnap puts a
// newer request of craze's own back over it, so reading it can never contradict
// one. What only the current generation may do is take the mask down — an older
// answer arriving late says nothing about the request the chip is showing.
//
// The re-read is the point, not bookkeeping: the RPC succeeding and this
// message being handled are two different moments, and an agent-initiated mode
// can land in between. refreshSnap masked it at the time, and clearing the flag
// without reading it back would leave the chip on the mode craze asked for
// while the session is in another one, with nothing to correct it afterwards.
func (m Model) modeSettled(gen int) Model {
	if gen == m.modeGen {
		m.modeInFlight = ""
	}
	m.refreshSnap()
	return m
}

// sessionReady is the two-key gate of §3.5: Start has returned *and*, for a
// loaded session, its replay has ended and the restored snapshot is installed.
// tea.Batch orders neither of them, so both are latched and whichever lands
// second opens the gate. Sends and /rename wait for it; a new session never
// sets replaying, so it means exactly what m.started alone used to.
func (m Model) sessionReady() bool { return m.started && !m.replaying }

// sessionUp is the tail startedMsg used to run alone: the status goes idle,
// the elapsed counter starts and the skills are rescanned. It is called from
// both keys and does nothing until both have landed, so it runs exactly once
// however they are ordered.
//
// A loaded session's index row used to be touched here. It is the engine's
// now, keyed to the one event that says a load is over — EventReplay{end},
// which only a load produces (plan 021 §3.8).
func (m *Model) sessionUp() {
	if !m.sessionReady() {
		return
	}
	m.status = statusIdle
	m.sessStart = m.now()
	m.rescanSkills()
}

// indexWriteText is the failure behind an engine.ErrIndexWrite as a transcript
// row draws it: the store's own message and not the sentinel's, which is the
// line writeIndex drew before the index moved into the engine.
func indexWriteText(err error) string {
	if cause := errors.Unwrap(err); cause != nil {
		return cause.Error()
	}
	return err.Error()
}

// titleRuneCap is how long a session title may be in the index. Runes, not
// bytes: the cap exists so a picker row is a row, and a prompt is as likely to
// open in Japanese as in ASCII.
const titleRuneCap = 120

// indexTitleLine folds a title onto the one line a session-index row holds and
// caps it. It is what the engine writes every row's title through
// (engine.IndexOptions.TitleLine), and what /rename normalises with before it
// even asks: how a title is made safe to draw is a rendering rule, so it stays
// here with the rest of them rather than being spelled a second time above the
// provider seam.
func indexTitleLine(title string) string {
	return capRunes(sanitizeLine(title), titleRuneCap)
}

func capRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i, count := 0, 0
	for i = range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// refreshSnap re-reads the session's state through the engine, which embeds the
// session's own snapshot and merges its own fields into it. It draws no
// conclusions from what changed: a mode change is reported by the event that
// carries it, because applyMode has already written the user's own change into
// the snapshot and an agent-side change that went round in a circle leaves
// nothing to compare.
//
// The queue is the engine's alone now (plan 021 §3.5): m.queue, not a field on
// m.snap, is what everything that draws the band reads.
func (m *Model) refreshSnap() {
	if m.eng == nil {
		return
	}
	st := m.eng.State()
	m.snap = st.Snapshot
	m.queue = st.Queue
	if m.modeInFlight != "" {
		// A SetMode of craze's own is still on the wire. The snapshot answers
		// with the mode the session is still in, which is the one the user
		// just left: taking it would flicker the chip back for as long as the
		// round trip lasts.
		m.snap.CurrentMode = m.modeInFlight
	}
	if m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
	}
	if m.dialog == dialogModel {
		// The model dialog's tabs are this snapshot's catalog, which a delta
		// can change under the open box — another client's model change
		// brings another model's options. A focused tab whose option has gone
		// hands the focus back to the list for good, rather than taking it
		// back if the option returns (plan 025 design 4).
		m.mdlg = m.mdlg.repaired(m.modelDialogTabs())
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

// refreshQueue re-reads the message queue alone. It is what a submit owes: the
// band changed — a row taken, or a row queued — and nothing else the model mirrors
// did.
//
// The rest of the snapshot is deliberately left alone, and refreshSnap is not what
// runs here. The turn this same Update just started is already running on the
// engine's goroutine, and a session that names itself from the prompt (native's
// own title) writes that while the Update is still in progress: re-reading the
// whole snapshot here would show a change this Update has no business showing, and
// which frame it first appeared in would be a race.
func (m *Model) refreshQueue() {
	if m.eng == nil {
		return
	}
	m.queue = m.eng.State().Queue
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

// waitEvent reads the engine's primary — the session's own, unchanged: the
// engine publishes into the same log, so one stream carries the agent's events
// and the engine's alike. A primary client keeps reading until it closes the
// engine, because the log's outbox may still be publishing after a turn's
// ending.
func waitEvent(eng *engine.Engine) tea.Cmd {
	if eng == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-eng.Events()
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
