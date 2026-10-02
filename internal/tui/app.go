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
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/host"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/transcript"
)

// ctrlCWindow is how long a Ctrl+C that cancelled a turn stays armed; a second
// press inside it quits.
const ctrlCWindow = time.Second

// stopCancelled is the one stop reason the TUI reads. internal/tui never
// imports internal/acp, so the string is spelled here. It is also the note a
// cancelled turn leaves (transcript.NoteCancelled, the same word), which the
// shared model writes.
const stopCancelled = "cancelled"

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
	Session agent.Session
	// Backend is a session served elsewhere (plan 027 PR 4, §3.14): a socket
	// to the host that owns the engine. When it is set New drives it as the
	// model's backend and builds nothing — no engine, no picker, no OnEngine,
	// no session index, no claim: Session, the pickers' closures, SessionIndex,
	// CrazeSessionID, OnEngine and ClaimSession are the in-process path's and
	// are not read. Init starts it and arms its reader exactly as it does an
	// engine's, and the model behaves as it does over one; what its stream
	// adds — a Ready, a Restore, an End — the model handles as the stream
	// delivers it (restore.go). nil is the in-process path: every test
	// Config, every golden and a plain craze.
	Backend backend.Backend
	// Viewer says this TUI joins a session another craze hosts: `craze
	// attach` (plan 027 §3.15). It is read only with a Backend, and turns off
	// what belongs to the host alone (viewing): the provider and resume
	// pickers and the session swaps they make (which a Backend never reaches
	// anyway), provider persistence (PersistProvider), host status reporting
	// (Host: the host TUI reports its own tab's), and index writes
	// (SessionIndex, which only an engine this TUI built could write). The
	// composer's shell mode runs locally, in the session's workspace as the
	// backend's Info names it (shellDir). Nothing else changes: the keys are
	// the host TUI's — the first Ctrl+C while a turn works acts on the shared
	// session (owner, §3.19) — and every frame of the session is drawn as the
	// host TUI over the same backend draws it.
	Viewer    bool
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
	// OnEngine is handed every engine setSession installs, right after the
	// session owner holds it — the one New builds and the one a picker builds
	// alike — and never nil. It is the raw *engine.Engine the model's backend
	// wraps, since what the hook serves is the engine itself. It runs inside
	// Update when a picker confirms, so it must not block. nil calls nothing,
	// which is what every test Config, every golden and the frame runner get;
	// internal/cli's hook serves the engine over the control socket, claims a
	// new session's craze id and rewrites the host's registry entry (plan 027
	// §3.8–§3.9).
	OnEngine func(*engine.Engine)
	// ClaimSession claims the row the resume picker is about to load, before
	// anything is built (plan 027 §3.9, SQ16): it answers the row's durable
	// craze id — a legacy row is given one first — and a release for the claim
	// it took, or the refusal, whose text the picker shows. It may block on
	// the session index's lock (bounded), so the picker calls it from a
	// tea.Cmd, never from Update, and builds the session only when the answer
	// lands while the picker is still waiting for it; an answer that arrives
	// for an abandoned attempt is released at once. nil keeps the picker's
	// synchronous load exactly as it was — every test Config, the frame
	// runner and the resume goldens.
	ClaimSession func(sessions.Row) (crazeID string, release func(), err error)
	// RefuseLoad is the command line's refusal of a provider, asked by both
	// pre-start pickers before they start anything: the resume picker of the
	// chosen row's provider before it claims or builds that row (plan 028
	// §3.5, P41), and the provider picker of the chosen provider before it
	// builds a session or lets one persist (§3.16, C19a). It is internal/cli's
	// closure over --agent-bin, CRAZE_AGENT_BIN and --ask/--plan, the same
	// check --continue makes of its row and a --provider run of its provider,
	// so `craze --agent-bin X` picking native is refused exactly as `craze
	// --provider native --agent-bin X` is. A refusal is the picker's error
	// row, and nothing is claimed, built or persisted. It is a pure function
	// of the provider and runs inside Update, so it must not block. nil
	// refuses nothing — every test Config and every picker golden.
	RefuseLoad func(agent.Provider) error

	// NewBackend and LoadBackend are the launch flow (plan 030 §3.5): the
	// backend twins of NewSession and LoadSession, for a craze whose sessions
	// run in detached hosts (SD-33). Each spawns — or finds — the host of the
	// session it is asked for and dials it, answering the backend the model
	// adopts, as it adopts a Config.Backend (setBackend): its Start observes
	// the host's own start. Either may take seconds, so the model calls it
	// from a tea.Cmd, never from Update, and draws the session's starting
	// state meanwhile — starting…, or restoring… for a load. NewBackend is a
	// new session of p: Init's own when the provider is known
	// (ProviderLocked), and otherwise the provider picker's choice, explicit
	// when it was Enter on a row rather than the default Esc starts — the
	// same distinction FallbackDefault's persistence draws, since the host is
	// what persists the provider now. LoadBackend loads row: Continue's, which
	// Init loads, and the resume picker's choice, which is not claimed here
	// (ClaimSession is the in-process path's): the host claims it, and a
	// session another host holds is that host's backend.
	//
	// Either set is the launch flow, and Session, NewSession, LoadSession,
	// ClaimSession, OnEngine, SessionIndex and CrazeSessionID — the
	// in-process path's — are not read: the host builds the engine and owns
	// its claim, its index row and its provider's persistence. A failure is
	// the session's start failing (startErr, exactly as Start's), except a
	// *Refusal of a picker's choice, which brings that picker back with the
	// refusal as its error row. A backend answered after the program has
	// quit is never adopted, and is the caller's to close: Start is called
	// on every backend the model adopts, and on no other. A backend that has
	// an AckStarted method (startAcker) is told when the model applies its
	// start and is not quitting — the caller's one sign that its session
	// came up in this TUI (Start's answer can lose to a quit). nil keeps the
	// in-process path — every test Config, every golden, the frame runner
	// and craze under the opt-out.
	NewBackend  func(p agent.Provider, explicit bool) (backend.Backend, error)
	LoadBackend func(p agent.Provider, row sessions.Row) (backend.Backend, error)
	// Continue is --continue's row in the launch flow: internal/cli resolved
	// it — claiming nothing — and Init loads it (LoadBackend), with the
	// provider locked to the row's and Loading set. nil for anything else.
	Continue *sessions.Row
	// Sessions is the session list's source (plan 030 §3.9, Sessions):
	// internal/cli sets it on the launch path alone, where sessions run in
	// detached hosts. nil — everything else — is no session list, and
	// nothing of it on any frame.
	Sessions Sessions

	// NativeDir and Getenv are the only way the TUI's own native-provider
	// surfaces reach the native directory and the environment (plan 031
	// §3.6, §3.9, §3.13; panel astra 13): `/connect` lists the providers,
	// judges which have a key and stores one there, and native's `/model`
	// ends with its connect row by the same judgement. They are the TUI's —
	// the key goes to this TUI's CRAZE_HOME, which the dialog shows by path,
	// even when the session is served by a host started elsewhere (R2).
	// Empty and nil are paths.NativeDir() and os.Getenv, read once by New:
	// every production caller. A test or golden sets both to a fixture
	// directory and environment, never the shipped catalog's variables or
	// the developer's ~/.craze.
	NativeDir string
	Getenv    func(string) string
	// AttachmentsDir is where the composer stores the processed copy of every
	// image pasted into it (plan 033 §3.2–§3.3), and what the envelope's
	// paths name: the TUI's, whichever session it shows — a chip is only
	// made for a session whose host reads this same directory (P27). Empty
	// is paths.AttachmentsDir(), read once by New: every production caller,
	// and the frame runner inside its isolated HOME. A test that pastes an
	// image sets it to a temporary directory.
	AttachmentsDir string
	// LocalPresence is how many clients are attached to the session this
	// TUI hosts, its own seat included (plan 032 §3.14, R2-7): the
	// TUI-hosted control server's count (control.Options.LocalClient),
	// which internal/cli keeps in this latest-value channel, read by a
	// command of the model's own (presenceCmd) since the in-process
	// backend's stream carries only the engine's events. Status row 2 shows
	// `N attached` while it is 2 or more. It is read only on the in-process
	// path, and closed when the server is; nil — every test Config, every
	// golden but the chip's own, the frame runner, the launch flow and a
	// craze with no socket — reads nothing.
	LocalPresence <-chan int
}

// viewing is c as it runs: for a viewer (Config.Viewer with a Backend)
// everything the host alone owns is cleared — the pickers' closures and rows,
// the claim and engine hooks, the launch flow's spawns, provider persistence,
// the session index and the host-status hub — so that no path of New or Run
// can reach it. Viewer without a Backend means nothing and is cleared too;
// any other Config is returned as it is. It is idempotent: Run applies it,
// and New again.
func (c Config) viewing() Config {
	if !c.Viewer || c.Backend == nil {
		c.Viewer = false
		return c
	}
	c.Session, c.NewSession, c.LoadSession, c.Resume = nil, nil, nil, nil
	c.ClaimSession, c.RefuseLoad, c.OnEngine = nil, nil, nil
	c.LocalPresence = nil
	c.NewBackend, c.LoadBackend, c.Continue = nil, nil, nil
	c.PersistProvider = false
	c.SessionIndex = nil
	c.Host = nil
	return c
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
	vp    viewport
	input textarea.Model

	// eng is the session's backend (plan 027 §3.12): the engine in process —
	// an engineBackend wrapping the *engine.Engine setSession built — or, from
	// PR 4, a socket to the host that owns the engine. The model drives its
	// session through it alone: admission, the message queue and its verbs,
	// send-now, cancel, the asks, the settings, and — since C12 — the session
	// index and the durable session id (plan 021 §3.4, §3.6, §3.8). No
	// production path reaches past it for the engine or the session
	// underneath, and it is assigned only in setSession, which records it in
	// owner too. It keeps the name it had when it was the engine itself.
	//
	// It is nil or a live backend, never a nil *engineBackend: every
	// `m.eng == nil` check reads "no session", which a typed nil would defeat.
	eng backend.Backend
	// sessGen is the session generation (plan 027 §3.12, astra r2 13): it
	// moves whenever the session the model drives is replaced — setSession,
	// on every engine swap, and in PR 4 a restore from another incarnation —
	// and every session-dependent asynchronous result carries the generation
	// it was issued under (issued), so one that lands after a replacement is
	// dropped rather than writing the old session's facts into the new one.
	//
	// It is the TUI's, never the session's: a switch to another session
	// (switchBackend, plan 030 §3.11) keeps counting from where the last one
	// left it, so no generation is ever issued twice (R2-5).
	sessGen uint64
	// bgen is the backend generation (plan 030 §3.11; round-1 panel finding
	// 3, R2-5): it moves on every backend the model adopts (adopt), and every
	// message a backend's own closures produce — each stream item the reader
	// hands up (waitEvent: an event, a restore, a ready, the end) and the
	// start's answer (startCmd) — carries the one it was read or started
	// under. The command gate drops a message of any other generation before
	// it does anything else (gated, staleBackend): a stream a switch left
	// behind can neither clear the reader's flag, which is the new backend's
	// read, nor quit the program with its End, nor start or fail the session
	// that replaced it. Like sessGen and gateSeq it only ever moves forward,
	// across every session this TUI shows, so a generation is never reused.
	// 0 on a message is one a test built by hand, and means whichever
	// backend the model holds, as the zero session stamp does (issued).
	bgen uint64
	// attached is how many clients the backend's stream last said are
	// attached to the session, this one included (backend.ItemPresence; plan
	// 032 §3.14, SF-64): the current backend generation's, 0 while not known
	// — before the first presence, and from a restore, the stream's end or the
	// adoption of another backend until the next one. Status row 2 shows it
	// at 2 or more (presenceChip).
	attached int
	// localPresence is Config.LocalPresence, the in-process host TUI's count
	// from its own server, read by presenceCmd; nil anywhere else. hostAttached
	// is the last count it gave: the server's, for its whole life, whichever
	// engine a picker has put behind it.
	localPresence <-chan int
	hostAttached  int
	// shownGen is the shown-session generation (plan 030 §3.11; C11r2, astra
	// r24-fix1112): it moves when the TUI starts showing another session —
	// a switch (switchBackend) — and on nothing else, and the results of this
	// terminal's own work carry the one they were asked under: a paste
	// (pasteMsg), a copy's note (clipboardDoneMsg), the composer's shell's
	// completion (shellDoneMsg). It is not bgen, which the first adoption
	// moves: the session a launch spawns, or a picker's choice spawns, is
	// shown before its backend exists — its composer is up, and a paste can
	// be asked for, while the spawn runs — so that adoption is the same
	// session's and what was asked for before it is that session's; a switch
	// is another session's, whether or not a backend had been adopted before
	// it. New starts it at 1, so the zero stamp is only ever a message a test
	// built by hand, and means whichever session is shown. Like bgen it only
	// ever moves forward, across every session this TUI shows.
	shownGen uint64
	// cmdSeq numbers this model's commands from 1. Each command also names the
	// backend's client id, read per command (nextCmd) and never cached, so
	// every mutating command it sends names itself and the events it caused
	// can be told from another client's (§3.2).
	cmdSeq int
	// chains orders this client's model changes against each other: the
	// dialog's apply chains and `/model <id> [<effort>]` each take a place in
	// its line in the Update that issues them, and run whole, one at a time, in
	// that order (chainLock). It is minted with the backend in setSession,
	// one per engine, and is a pointer so every copy bubbletea makes shares it.
	chains *chainLock
	// engErr is what wrapping the session in an engine came back with. It is
	// unreachable in practice — every session owns an event log and no path
	// wraps one twice — and is carried rather than panicked on, so it fails the
	// way a session that would not start fails: startCmd reports it.
	engErr error
	cwd    string
	model  string
	// yolo is Config.Yolo: this craze's own --force, which the permission
	// chip shows when the session's host does not say what it spawned its
	// agent with (bypassing).
	yolo   bool
	status status
	err    string
	// startErr is the session that never came up. It is the only error that
	// reaches craze's exit status: Run returns it once the program is over.
	startErr error
	// startInc is the incarnation a socket backend's start answered for
	// (plan 030 C7r2): the one the model held when it applied the stream's
	// Ready. A socket's stream hands up one Ready at most — the one its first
	// attach was owed — after the restore of the incarnation it is about,
	// and that Ready is the start's outcome: a remote.Session's Start answers
	// with it. "" until one is applied, and always in process, where no
	// Ready comes. startErr is that incarnation's failure: a restore of
	// another takes it away, and a start that answers once the model has
	// left it is not a failure of the session the model now holds
	// (startLeft).
	startInc string

	// git is found at start and again whenever the session's workspace moves
	// the model's (followWorkspace); branch is re-read when a turn ends.
	git    gitInfo
	branch string
	// sessStart is when the session came up (sessionUp; a frame run's own
	// start in a frame run, frameStart), which is what the status row's
	// elapsed counts from when the session's host does not say when it
	// started (hostStart).
	sessStart time.Time
	// hostPerm and hostStart are the session's facts the status rows read
	// (plan 030 §3.7): the permission mode its host spawned the agent with
	// (SF-60) and when its host started serving it (SF-63), as the backend's
	// Info said them at the last recompute — so they move where the rest of
	// the mirror does, in the Update that applies a restore or a ready, and
	// never under a frame. PermissionUnsaid and zero when the backend does
	// not say (in process; an older host): the chip is then the config's
	// (yolo), and the elapsed counts from sessStart, as they always have.
	hostPerm  backend.PermissionMode
	hostStart time.Time

	// shared is the session's transcript as every client folding its events
	// agrees on it (plan 024 §3.8): this client's own instance, folded from
	// every event the primary delivers (foldEvent), made fresh with each
	// session (setSession). foldIn is what each fold takes from this client
	// (foldInputs). Both are pointers every copy of the model shares, as the
	// panes are.
	shared *transcript.Model
	foldIn *foldInputs
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
	// replayFolded counts the events folded while replaying, and paintNow
	// asks finish to paint the drawn pane this Update whatever the replay's
	// cadence (paintDue): every replayPaintEvery-th of those events sets it,
	// and so does a restore. finish clears it. The count is one replay's: a
	// replay's start begins it again, and so does a restore that adopts
	// another incarnation (applyRestore); a forced paint, and a restore of
	// the same incarnation, leave it where it is.
	replayFolded int
	paintNow     bool
	// paintEveryEvent turns the replay's cadence off: every Update paints a
	// dirty drawn pane, as before plan 032 C6. Only tests set it, as the
	// oracle the cadence's frames are held to
	// (TestTheReplayCadenceDrawsWhatPaintingEveryEventDraws).
	paintEveryEvent bool
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
	// hiddenRetry are answers to asks the config shows no card for that the
	// engine refused for want of room (answerHidden), and hiddenRetryLive says
	// the one beat they are waiting on is in flight (armHiddenRetry).
	cards           []card
	cardMask        string
	cardMasking     bool
	hiddenRetry     []hiddenAnswer
	hiddenRetryLive bool
	// snap is the session's state as the TUI draws it, and queue the message
	// queue in send order: both derived (recompute, mirror.go) from this
	// client's fold, the backend's static facts and its own overlays, after
	// every fold and every overlay write (plan 027 §3.13). sendNowArmed is the
	// armed send-now as the same mirror says (sendNowPending). Nothing else
	// writes them.
	snap         agent.Snapshot
	queue        []agent.QueuedPrompt
	sendNowArmed bool
	// ov is this client's overlays on the fold: what its own commands
	// confirmed, or asked for and have not heard back about, that the fold
	// has not caught up with (overlays, mirror.go).
	ov overlays
	// modeInFlight is a mode change of craze's own that the agent has not
	// answered yet: set when the user asks for it, cleared by the answer —
	// success or refusal — for its own request. What the chip shows meanwhile
	// is the mode overlay's (requestMode), which holds past a success until
	// the change's own delta is folded (plan 027 §3.13).
	//
	// modeGen is which request it belongs to. The mode id cannot stand in for
	// that: changes are not serialised, two of them can be answered out of
	// order, and ids repeat — agent → plan → agent → plan hands the third
	// request's protection to the first request's answer. Every writer bumps
	// the generation and every answer carries it back, so an answer that is
	// not the current generation is stale and may neither clear the flag nor
	// speak for the chip.
	modeInFlight string
	modeGen      int

	// modeRev, modelRev and configRev are the highest StateDelta Seq this model
	// has applied for each settings section: the mode, the model, and the
	// config options — one revision for all of them, because a config delta
	// carries every option in full (observe).
	//
	// They are what a confirmed settings overlay is judged against (settled,
	// mayApply). modeGen, applyGen and modeInFlight are view state about this
	// model's own requests. These are about the shared state the session
	// actually holds, which another client can change too, and the only thing
	// that orders the two is the revision the deltas carry.
	modeRev   uint64
	modelRev  uint64
	configRev uint64

	// dialog is the modal layer: at most one is up, drawn over the transcript
	// region and hit-tested before any band. mdlg is the model dialog's own
	// state, helpTop the help box's scroll position.
	dialog  dialogKind
	mdlg    modelDialog
	helpTop int
	// cdlg is the /connect dialog's own state (connect_dialog.go, plan 031
	// §3.9), the session's like every dialog's, so a switch clears it and its
	// key field with it. connSeq numbers every opening of that dialog, of its
	// key field and of the model dialog, whose answers carry the number back
	// (connectAnswer, keyField); it only ever moves forward, across every
	// session, so no number is issued twice. nativeDir and nativeEnv are
	// Config.NativeDir and Config.Getenv, defaults applied: the one way the
	// dialog and the model dialog's connect row reach files and the
	// environment.
	cdlg      connectDialog
	connSeq   uint64
	nativeDir string
	nativeEnv func(string) string
	// images is the composer's sidecar (plan 033 §3.3, composer_image.go):
	// the image behind each chip of the draft. The TUI's, as input is — it
	// is the draft's, and the draft is carried through an unstarted
	// session's adoption and put back by a switch with its text (drafts,
	// takeDraft). attachSeq numbers its entries for the TUI's life, so a
	// processing result finds its entry by an id no other entry ever had;
	// attachDir is Config.AttachmentsDir, its default applied: where the
	// processed copies go.
	images    draftImages
	attachSeq uint64
	attachDir string
	// attachRuns is the chips' processing lane (attachLane): how many are
	// running (processCmd), one at a time, and what is still wanted. Shared by
	// every copy of the model, as shell is, since a processing outlives the
	// Update that started it — and outlives its chip, when the chip is deleted
	// first. Its count is what the frame runner waits out (imagesInFlight).
	// Nil only in a model New never built.
	attachRuns *attachLane
	// attachReads is P27's answer for the backend adopted (adopt,
	// readsAttachments): its session's host reads attachDir. The session's,
	// as its backend is.
	attachReads bool
	// applyGen counts this client's model changes: the model dialog's applies
	// and `/model`'s. The box closes optimistically, so a second change can be
	// under way before the first one answers; each answer carries its own, and
	// settles only the overlays its own request wrote (mirror.go).
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
	pickingResume bool
	resumeCursor  int
	resume        []sessions.Row
	loadSession   func(agent.Provider, sessions.Row) agent.Session
	// claimSession is Config.ClaimSession. resumeAttempt stamps each claim
	// the picker starts, resumeWaiting is the one it is waiting for (0 for
	// none), and resumeErr is the last refusal, drawn as the dialog's error
	// row until the cursor moves.
	claimSession  func(sessions.Row) (string, func(), error)
	resumeAttempt int
	resumeWaiting int
	resumeErr     string
	// refuseLoad is Config.RefuseLoad, which both pickers ask.
	refuseLoad func(agent.Provider) error
	// onEngine is Config.OnEngine.
	onEngine        func(*engine.Engine)
	providerLocked  bool
	persistProvider bool
	fallbackDefault bool
	pickedExplicit  bool
	providerCursor  int
	// providerErr is the provider picker's last refusal (refuseLoad), drawn as
	// its error row until the cursor moves: resumeErr's twin.
	providerErr     string
	providerDefault agent.Provider
	// providers is the picker's rows, settled once in New: the caller's
	// availability-filtered list unioned with providerDefault (§3.4). Nothing
	// after the constructor recomputes it, so the rows the user sees are the
	// rows Esc and Enter act on.
	providers  []agent.Provider
	newSession func(agent.Provider) agent.Session

	// The launch flow (plan 030 §3.5, launch.go): spawnNew and spawnLoad are
	// Config.NewBackend and Config.LoadBackend, and cont is Config.Continue.
	// spawnSeq stamps each spawn the model starts, spawnWaiting is the one it
	// is waiting for (0 for none), and spawnFrom is the picker whose choice it
	// is — dialogNone for Init's own — which a refusal brings back.
	spawnNew     func(agent.Provider, bool) (backend.Backend, error)
	spawnLoad    func(agent.Provider, sessions.Row) (backend.Backend, error)
	cont         *sessions.Row
	spawnSeq     int
	spawnWaiting int
	spawnFrom    dialogKind

	// sessions is Config.Sessions, the session list's source (plan 030
	// §3.9): nil is no list — no ← binding, no /sessions builtin, no help
	// line, nothing of it on any frame. sessList is the list itself
	// (sessions_list.go), a top-level mode while it is open. sessRosters is
	// every roster the list opened and has not closed, shared by every copy
	// as owner is, so every exit path closes one left open (finishRun).
	sessions    Sessions
	sessList    sessListState
	sessRosters *sessRosterSet
	// sessPick is what the list's /provider, /model, /effort and /fast chose
	// for the sessions it starts (plan 030 §3.14, plan 032 C17,
	// sessions_models.go): the TUI's, so it lasts across every opening of the
	// list and every session shown, until changed or craze quits.
	sessPick sessPick
	// bandOn says the session list has been opened in this TUI: from then on
	// every session's frame carries the band — which session it is, and the
	// way back to the list (band.go, plan 030 §3.11). Until then the band has
	// no rows, so no frame of a TUI that never opened the list moves.
	bandOn bool
	// indexTitle is the session index's title for the session the model
	// shows, as the session list last listed it (roster.Row.IndexTitle: the
	// row a switch opened it from, then its own row while the list is up —
	// trackHere): what the band names the session by while it names none of
	// its own, as its list row does (sessTitle; plan 030 C11r). The session's
	// own, made afresh with it (withSession); "" until the list has listed
	// it, and after that it is as fresh as the list's last listing.
	indexTitle string
	// drafts is the composer's text of each session this TUI has left, by
	// its craze id (draftKey): stashed as a switch leaves a session and put
	// back when a switch returns to it, so a draft never follows the user to
	// another session (plan 030 §3.11). Copied on write, as cards is. Each
	// carries its sidecar (plan 033 §3.3): a draft's chips come back with
	// their images.
	drafts map[string]stashedDraft
	// retired is every backend a switch let go of, and every one a dial
	// answered after the user had moved on, until its close — a view close,
	// made off the Update — has run (switch.go). Shared by every copy, as
	// sessRosters is, so every exit path closes one whose close never ran
	// (finishRun). A background dispatch's connection is here while it runs
	// (dispatch.go), for the same reason.
	retired *backendSet
	// completeLoads is every completion popup's load still awaited (the
	// list's `@` listing of a directory, complete.go), shared by every copy as
	// retired is: a popup cancels its own as it closes, and finishRun cancels
	// whatever a quit the popup never saw left running (sol r28-c14 2).
	completeLoads *completeLoadSet
	// signIns is every /connect ChatGPT sign-in run not yet ended — its
	// context's cancel and, from the moment Begin returns it, its attempt
	// (connect_signin.go) — shared by every copy as completeLoads is: the
	// step ends its own on every way out it sees, and finishRun ends whatever
	// an exit no Update sees left listening (plan 033 §3.13; review r14 2).
	signIns *signInRuns
	// composerAt is the composer's `@` popup (plan 030 §3.16,
	// composer_at.go): the files of the shown session's workspace, completed
	// into an `@` token of the draft. The TUI's, as input is — it follows the
	// draft, synced with it after every applied message (finish) — and closed
	// where another session is shown (switchBackend, openUnstarted), its
	// search cancelled.
	composerAt completePopup
	// unstarted is the session shown when it is a new one opened in place by
	// the list's input with a leading `@dir` alone, not yet spawned (plan 030
	// §3.13, dispatch.go): the model has no backend while it is set, and its
	// first prompt spawns one. first is that prompt while the session its
	// spawn adopted comes up: sent once it is up, or — the start failing —
	// the session is unstarted again. unstartedSeq numbers the unstarted
	// sessions' temporary draft ids and their spawns, for the TUI's life.
	unstarted    *unstartedSession
	first        *firstPrompt
	unstartedSeq uint64

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
	// queueEditVer is the row's version when the edit began: a save of the
	// edit is a check-and-edit against it (plan 027 §3.13, C22), so a row
	// another client changed meanwhile refuses the save rather than being
	// overwritten; the refusal refreshes it to the row the band then shows,
	// for a second Enter that saves over that change knowingly.
	queueEdit    string
	queueEditPos int
	editDraft    string
	queueEditCtx string
	queueEditVer int
	// editImages is editDraft's sidecar (plan 033 §3.3): the draft's images,
	// displaced with it while the composer holds the row's own (its envelope
	// parsed back into chips, startQueueEdit), and put back with it.
	editImages draftImages
	// confirm is the send-now waiting for an answer. The confirm line is
	// client-local UI: nothing is taken from anywhere and the engine has not
	// heard of it. The send-now it turns into, on the other hand, is the
	// engine's armed send (the mirror's sendNowArmed), because the cancel that
	// makes room for it is the engine's.
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
	// offerGen is the plan offer's generation: every restore but the first
	// moves it (restore.go), and an implementation dispatched from the offer
	// carries the one it was dispatched under (implementPlan). One that lands
	// under another answers for a transcript a restore has replaced: it sends
	// nothing, and puts no offer back.
	offerGen int
	// turnID is the engine turn turnSeq names: what the model is looking at, and
	// what a cancel is asked against, so a cancel delayed across a queue
	// transition is refused as stale rather than stopping the turn the user did
	// not mean (§3.7).
	turnID string
	// ownTurn is the one turn id the model skips the started event for: the one
	// Submit handed back, whose row, working status and turnStart its
	// continuation has already applied — before any event the Submit caused,
	// which the command gate holds until then (§3.12). Echo
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
	// for the same reason: Esc writes its own note in the Disarm's
	// continuation — before any event the command caused, which the command
	// gate holds until then (§3.12) — and the delta the engine publishes for
	// that same command is then this model's own echo. A withdrawn delta from
	// any other command is somebody else's Disarm and is worded for the user.
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
	// frameStart is the frame runner's start (RunFrameScript), zero outside
	// a frame run: the session's start its elapsed counts from when its host
	// does not say (sessionUp), in place of the moment it came up — so an
	// in-process run counts from the instant a socket run's host serves as
	// its StartedAt, and a frame's elapsed is the same by transport however
	// long its run takes (plan 030 §3.7, SF-63; sol r10-c6 2). Like frozen,
	// the clock itself is left alone.
	frameStart time.Time

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
	// exit is what the explicit quit came to over a backend served elsewhere
	// (stopQuit): shared by every copy, as owner is, because the quit's
	// tea.Cmd writes it after the Update that asked has returned, and Run
	// reads it from the model it started with. Nil only in a zero Model.
	exit *exitState
	// remote says the backend is a session served elsewhere (setBackend: a
	// Config.Backend, or the launch flow's), whose explicit quit stops it on
	// its host (plan 030 §3.6); false for an engine this TUI built, whose
	// quit closes it as ever.
	remote bool

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

	// foreignEnded counts the foreign-turn endings this model has applied,
	// which is what names the agent's turn Esc stops (foreignEpisode).
	// foreignNoted is the episode whose cancelled note is drawn, 0 for none,
	// so Esc pressed again on the same turn draws no second one
	// (applyForeignCancelled, plan 026 X48).
	foreignEnded uint64
	foreignNoted uint64

	// The command gate (gate.go, plan 027 §3.12). gate is the one gated call
	// whose reply the model is waiting for, nil when none is open; gateSeq
	// numbers them. held is every message that arrived while a gate was open,
	// or while earlier held ones were still draining, in arrival order, and
	// heldBytes the payload they retain (payloadBytes); heldDrained counts the
	// drained slots at the front of held's array (drain compacts it once they
	// are more than half of it). reading says a Read of
	// the backend's stream is in flight: exactly one ever is (readOn). syncAck
	// is the last frame-sync token acknowledged, which the frame harness
	// publishes as its barrier, and syncPending one that arrived while a gate
	// was open, acknowledged by the release that leaves none open. gateSync is
	// the test-only baseline that runs a gated call inline (gateSyncDefault).
	//
	// None of this is drawn: it is the gate's own bookkeeping, and the
	// invisibility watch leaves it out of the state it holds still.
	gate        *gate
	gateSeq     uint64
	held        []heldMsg
	heldBytes   int
	heldDrained int
	reading     bool
	syncAck     int
	syncPending int
	gateSync    bool
	// harnessQuit says the frame runner's quit message (frameQuitMsg) has been
	// applied: the frame of that Update is the run's capture (frame.go).
	harnessQuit bool
	// ended says the backend's stream ended (an End item: the session closed
	// on its host, or the transport gave up), which quits the program, and
	// endErr is why — nil for the session's own end (restore.go's endMsg).
	// The final model carries both for the command line's last word; in
	// process no End ever comes.
	ended  bool
	endErr error
	// viewer is Config.Viewer (with a Backend): this TUI joins a session
	// another craze hosts. New has cleared what the host alone owns
	// (Config.viewing), and nothing the model decides reads it any more:
	// where the composer's shell runs was a viewer's own rule, and is every
	// backend's since plan 030 §3.7 (shellDir, followWorkspace).
	viewer bool
	// infoPin is the facts the model reads in place of the backend's Info
	// while a restore is being applied: the restore item's own (applyRestore),
	// so every decision the restore makes reads the session it restores and
	// never a later one a socket backend has already received. nil at every
	// other moment (info).
	infoPin *backend.SessionInfo
	// upDone says the session-is-up tail has run (sessionUp): it runs once,
	// whichever of its keys lands last and however often a replay closes the
	// gate again.
	upDone bool
	// restores counts the restores this model has applied, and turnStarts
	// the turns it has seen begin — its own and every other client's
	// (beginTurn), and the agent's own (a foreign turn's running bracket).
	// They are the tag the read after a restore carries (lastturn.go, plan
	// 030 §3.7): its answer is applied only while both still stand where
	// they stood when it was sent. Both only ever move forward, across every
	// session this model holds, so no answer can find its tag again.
	restores   uint64
	turnStarts uint64
}

// info is the session's static facts as the model reads them (plan 027
// §3.13): the backend's Info, or the restore item's own while that restore is
// being applied (infoPin).
func (m Model) info() backend.SessionInfo {
	if m.infoPin != nil {
		return *m.infoPin
	}
	return m.eng.Info()
}

// foldedSeq is the seq of the last event the model folded into its shared
// transcript, 0 before it has one.
func (m Model) foldedSeq() uint64 {
	if m.shared == nil {
		return 0
	}
	return m.shared.Seq()
}

// now reads the clock through an indirection so tests can inject one.
func (m Model) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

// eventMsg is one event of the backend's stream, and gen the stream
// generation it came from (backend.Item.Gen): 0 in process, where the stream
// is never replaced; over the socket the attachment it was read on, which a
// restore replaces — so a restore held behind it can tell an event its
// snapshot already holds (restore.go, dropSuperseded). bgen is the backend
// generation it was read under (Model.bgen): the stream generation is the
// backend's own count, and says nothing across a switch to another backend.
type eventMsg struct {
	ev   agent.Event
	gen  uint64
	bgen uint64
}

// startedMsg says the session is up: the start command's Start has returned.
// errMsg is that command's other answer, and the only error that reaches craze's
// exit status.
//
// Both name the backend they are for. A start command outlives the model copy
// that made it — a picker's choice closes one engine and builds another in the
// same Update, with the old command still in flight — and neither message may
// then speak for the backend that replaced the one it was about: a stale
// startedMsg would open the new engine's gate before its own Start had
// returned, and a stale errMsg would fail a session that is starting perfectly
// well. A nil eng means "whichever backend the model holds", which is what a
// test injecting either message by hand intends. bgen is the backend
// generation the start was dispatched under (startCmd), which the command gate
// judges first (staleBackend): a start answering after a switch is dropped
// there, whatever the backend's identity would say.
type startedMsg struct {
	issued
	eng  backend.Backend
	bgen uint64
}
type errMsg struct {
	issued
	err  error
	eng  backend.Backend
	bgen uint64
}

// backendStamped is a message that carries the backend generation it was
// produced under (Model.bgen), and is nothing to the model once that backend
// is left: a backend's own — a stream item the reader handed up, the start's
// answer.
type backendStamped interface{ backendGen() uint64 }

func (m eventMsg) backendGen() uint64   { return m.bgen }
func (m restoreMsg) backendGen() uint64 { return m.bgen }
func (m readyMsg) backendGen() uint64   { return m.bgen }
func (m endMsg) backendGen() uint64     { return m.bgen }
func (m startedMsg) backendGen() uint64 { return m.bgen }
func (m errMsg) backendGen() uint64     { return m.bgen }

// shownStamped is a result of this terminal's own work that is only ever
// about the session shown when it was asked for, and carries the
// shown-session generation it was asked under (Model.shownGen): a paste
// (pasteMsg), whose text is that session's composer's, a copy's note
// (clipboardDoneMsg), which that session's status row shows (plan 030 C11r,
// astra r22-c11 2) — asked for before that session's backend was adopted
// too, which a switch leaves behind as it leaves any other (C11r2, astra
// r24-fix1112) — and a chip's processing (attachDoneMsg, plan 033 §3.3),
// whose draft a switch stashed and starts again when it comes back. The composer's shell's completion carries the generation
// too, and is judged where it is applied instead (finishShell): its row may
// still be on screen.
type shownStamped interface{ shownUnder() uint64 }

func (m pasteMsg) shownUnder() uint64         { return m.shownGen }
func (m clipboardDoneMsg) shownUnder() uint64 { return m.shownGen }

// leftBehind reports that msg is something a switch left behind (plan 030
// §3.11): a message of a backend the model has left (staleBackend), or the
// result of this terminal's work for a session it no longer shows
// (staleShown). The command gate drops one before it does anything else
// (gated), the held queue's drain drops one found held (drain), and a switch
// takes each out of the held queue as it is made (dropStaleHeld).
func (m Model) leftBehind(msg tea.Msg) bool { return m.staleBackend(msg) || m.staleShown(msg) }

// staleBackend reports that msg is a message of a backend the model has left
// (plan 030 §3.11): produced under another backend generation (leftBackend).
func (m Model) staleBackend(msg tea.Msg) bool {
	s, ok := msg.(backendStamped)
	return ok && m.leftBackend(s.backendGen())
}

// leftBackend reports that g is a backend generation the model has left: the
// model has adopted another backend since. A backend's own messages are only
// ever produced once it is adopted, so their generation is never the zero
// one; the zero generation is a message a test built by hand — or a start
// failure with no backend to name (startCmd) — and is never left.
func (m Model) leftBackend(g uint64) bool { return g != 0 && g != m.bgen }

// staleShown reports that msg is the result of this terminal's work for a
// session the model no longer shows (C11r2): asked for under another
// shown-session generation (leftShown).
func (m Model) staleShown(msg tea.Msg) bool {
	s, ok := msg.(shownStamped)
	return ok && m.leftShown(s.shownUnder())
}

// leftShown reports that g is a shown-session generation the model has left:
// it has switched to another session since. The zero generation is a
// message a test built by hand, and is never left.
func (m Model) leftShown(g uint64) bool { return g != 0 && g != m.shownGen }

// actionErrMsg is a failure a command reports as an error row: a chain's
// settings read or Set that failed. landed is `/model`'s model step when it
// landed before the effort step failed (modelLanded), applied first.
type actionErrMsg struct {
	issued
	err    error
	landed modelLanded
}

// issued stamps a session-dependent asynchronous result — a gated call's
// reply, a settings answer, a cancel's report, a hidden answer the outbox
// refused, a start's answer — with the session generation it was issued
// under (plan 027 §3.12, astra r2 13): the model's sessGen, read in the Update
// that dispatched the command, never on the command's goroutine. A result
// whose generation is no longer the model's is about a session the model has
// left, and is dropped where it lands (outdated): an engine swap and, in
// PR 4, a restore from another incarnation move the generation, and a result
// issued before must not write that session's facts into this one. This is
// staleFor generalised from the start messages to every result.
//
// The zero stamp is a message a test built by hand, and means whichever
// session the model holds — as a start message's nil backend does.
type issued struct{ sessGen uint64 }

func (i issued) issuedUnder() uint64 { return i.sessGen }

// issue is the stamp a command dispatched now carries.
func (m Model) issue() issued { return issued{sessGen: m.sessGen} }

// dispatchCtx is the context a call dispatched now against b carries — and
// every later call of the same operation, a chain's next step among them: b's
// epoch, read here, in the Update, never on the command's goroutine
// (backend.WithEpoch). A call that runs once b has moved to another session is
// refused before anything is sent (§3.12, "Chains are fenced in the backend
// too"). It never ends; a site that needs a deadline derives one from it.
func dispatchCtx(b backend.Backend) context.Context {
	return backend.WithEpoch(context.Background(), b.Epoch())
}

// outdated reports that msg is a result issued under a session generation
// other than the model's: dropped, unapplied.
func (m *Model) outdated(msg tea.Msg) bool {
	s, ok := msg.(interface{ issuedUnder() uint64 })
	if !ok {
		return false
	}
	gen := s.issuedUnder()
	return gen != 0 && gen != m.sessGen
}

// ownStart reports whether msg is the start answer (startedMsg, errMsg) of
// the backend the model holds now. A start answers for its backend, not for a
// session generation: a restore from another incarnation moves the generation
// (restore.go) and keeps the backend, whose Start returns once, so dropping its
// answer would leave the model never started. An answer from a backend the
// model has replaced (setSession) is outdated as ever, and staleFor besides.
func (m Model) ownStart(msg tea.Msg) bool {
	var b backend.Backend
	switch msg := msg.(type) {
	case startedMsg:
		b = msg.eng
	case errMsg:
		b = msg.eng
	default:
		return false
	}
	return b != nil && b == m.eng
}

// revertModeMsg is a mode change coming back refused, or never coming back
// inside modeCallTimeout. gen is the request it answers for, whose mode
// overlay it takes down; prev is what the chip showed before that request
// asked, and at the mode section's revision when it asked — the request's
// record. Nothing puts prev back: with the overlay gone the chip reads the
// fold, which is the session's mode as far as this client has heard (§3.13).
type revertModeMsg struct {
	issued
	gen  int
	prev string
	err  error
	at   uint64
}

// modeAppliedMsg is a mode change coming back accepted (plan 027 §3.13: every
// settings success reaches the reducer with its result): gen is the request it
// answers for, id the mode the session confirmed (engine.SetResult.Value,
// which is not always the one asked for) and rev the revision it committed it
// at. Its overlay holds that value until the fold's mode revision reaches rev
// — or its own delta is folded, the same event — and, with no revision to wait
// for (rev 0: a change that published nothing), until the fold shows it
// (confirmMode).
type modeAppliedMsg struct {
	issued
	gen int
	id  string
	rev uint64
}

// revertModelMsg is a model change coming back refused. gen is the request it
// answers for, whose model overlay it takes down; prev is the model the screen
// showed when it asked, and at the model section's revision then — the
// request's record. Nothing puts prev back (SF-38): two overlapping refusals
// both land on the fold's model, never on the first one's optimistic value.
type revertModelMsg struct {
	issued
	gen  int
	prev string
	err  error
	at   uint64
}

// modelUnreadMsg is a model change coming back with an answer the session
// could not read (agent.ErrBadCatalog). It is not revertModelMsg: the agent
// answered and may have switched, so nothing says the model was refused. Its
// overlay goes all the same — nothing of the answer was installed — so the
// screen reads the fold, and the row says the outcome is unknown
// (unreadModelText).
type modelUnreadMsg struct {
	issued
	gen int
}

// modelLanded is `/model`'s model step coming back accepted (plan 027 §3.13:
// every settings success reaches the reducer with its result): gen is the
// request, cause the step's command and at the model section's revision when
// it was sent, and res what the session confirmed and the revision it
// committed it at, which the model overlay holds until the fold reaches
// (confirmModel). set says there is one.
type modelLanded struct {
	set   bool
	gen   int
	cause string
	at    uint64
	res   engine.SetResult
}

// land applies a landed model step to the model overlay, when there is one.
func (m *Model) land(l modelLanded) {
	if l.set {
		m.confirmModel(l.gen, l.cause, l.res.Value, l.at, l.res.Rev)
	}
}

// modelSetMsg is `/model <id>` — no effort — coming back accepted: the model
// step's landing, and nothing after it. With an effort the chain answers the
// effort step's own message instead — effortSetMsg, effortNotAppliedMsg or
// actionErrMsg — carrying the model step's landing beside what the effort came
// to.
type modelSetMsg struct {
	issued
	landed modelLanded
}

// effortSetMsg is `/model <id> <effort>`'s effort step coming back accepted:
// option id, sent as cause from the model step's request (landed.gen) when the
// config section stood at at, confirmed at res (confirmOption).
type effortSetMsg struct {
	issued
	cause, id string
	at        uint64
	res       engine.SetResult
	// landed is the model step's landing (modelLanded), applied first.
	landed modelLanded
}

// refreshSnapMsg redraws the mirror: tests send it bare, as a nudge.
type refreshSnapMsg struct{ issued }

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
// And both carry the offer's generation (offerGen): a restore since the
// dispatch retired the offer they answer for.
type planImplementMsg struct {
	issued
	seq   int
	gen   int
	offer int
	mode  string
	// rev is the mode change's revision (modeAppliedMsg's).
	rev uint64
}
type planImplementFailedMsg struct {
	issued
	seq   int
	gen   int
	offer int
	prev  string
	err   error
	at    uint64
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
// driver goroutine running and its last events unpublished. It holds it as the
// model does, as the session's backend (plan 027 §3.12), whose Close is the
// engine's in process.
type sessionOwner struct {
	mu  sync.Mutex
	eng backend.Backend
}

func (o *sessionOwner) set(b backend.Backend) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.eng = b
}

// current is the backend last set. The lock is released before it returns, so
// a caller never holds it across Close, which blocks until the agent is reaped.
func (o *sessionOwner) current() backend.Backend {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.eng
}

// setSession is the only way the model's session is assigned: it wraps s in the
// engine that drives it and writes m.eng and the owner together, so the two
// can never name different sessions and a new assignment site cannot forget
// either one.
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
	m.dropSession()
	if s == nil {
		return
	}
	eng, err := engine.New(s, engine.HostOptions(crazeID, m.sessionIndex, m.cwd, m.providerDefault.Name()))
	if err != nil {
		// m.eng stays the untyped nil it was set to above: a failure leaves no
		// backend, never a nil *engineBackend that would read as one.
		m.engErr = err
		return
	}
	// The backend mints this model's client on the engine, once: the
	// in-process client is never released (plan 027 §3.6).
	m.adopt(newEngineBackend(eng, m.cwd))
	m.remote = false
	// After the owner holds it, so whatever the hook starts — a socket
	// serving this engine — can never name an engine the exit tail would not
	// close. The hook is handed the engine itself, not the backend: what it
	// serves is the engine.
	if m.onEngine != nil {
		m.onEngine(eng)
	}
}

// configWorkspace is the session's working directory as New resolves
// Config.Workspace: the process's own when it names none, made absolute.
func configWorkspace(ws string) string {
	if ws == "" {
		ws, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(ws); err == nil {
		ws = abs
	}
	return ws
}

// configProvider is the resolved default New starts from: Config.Provider, or
// cursor when it names none.
func configProvider(p agent.Provider) agent.Provider {
	if p.Name() == "" {
		return agent.CursorProvider()
	}
	return p
}

// setBackend is setSession for a session served elsewhere (Config.Backend):
// the same reset of everything the model held about a session, and b as its
// backend — with nothing built, since the engine is the host's.
func (m *Model) setBackend(b backend.Backend) {
	m.dropSession()
	if b != nil {
		m.adopt(b)
		m.remote = true
	}
}

// adopt makes b the model's backend, recorded in the owner too, with a
// command order of its own (chains), and its session's workspace the model's
// (followWorkspace). It is a new backend generation (bgen): whatever the
// backend it replaces still hands up is another backend's from here on.
func (m *Model) adopt(b backend.Backend) {
	m.bgen++
	m.eng = b
	// P27 is read from b as it was handed over, before a test's hook wraps it
	// (plan 033 §3.3, readsAttachments).
	m.attachReads = readsAttachments(b)
	if sessionBackendHook != nil {
		m.eng = sessionBackendHook(m.eng)
	}
	// The shared model again, now that its session's capabilities can be read
	// from the backend (newShared): dropSession made one before there was a
	// backend, and nothing has been folded into it since.
	m.newShared()
	// A new client, so a new order: a chain still running on the backend this
	// replaced orders nothing on this one.
	m.chains = &chainLock{}
	m.owner.set(m.eng)
	m.followWorkspace(m.eng.Info().Workspace)
}

// followWorkspace makes ws — the session's own workspace, as its backend's
// Info names it — the model's (plan 030 §3.7, "the workspace follows the
// session"): m.cwd, which the status row names, the skills are scanned under
// (rescanSkills) and the composer's shell runs in (shellDir), and the
// repository the branch is read from, found again from there. It is called on
// every adopt: a session served elsewhere runs where its host runs it — a
// --continue row's directory, a session another craze started — which need
// not be the directory this craze was started in, and a socket backend names
// it before its first attach reply (the registry entry's workspace, which its
// dialler passes: remote.SessionOptions.Workspace). In process the engine
// backend's Info names m.cwd itself, so nothing moves; "" — a backend that
// names none — changes nothing either.
func (m *Model) followWorkspace(ws string) {
	if ws == "" || ws == m.cwd {
		return
	}
	m.cwd = ws
	m.git = discoverGit(ws)
	m.branch = m.git.branch()
}

// dropSession is the half of setSession and setBackend that lets go of the
// session the model held: its commands, its generation, its fold, its
// overlays and its client, leaving no backend.
func (m *Model) dropSession() {
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
	// A new session, so a new generation: every result still in flight for
	// the one this replaces is dropped where it lands (issued).
	m.sessGen++
	m.eng, m.engErr = nil, nil
	// A new session is a new transcript: the shared model is folded from its
	// events alone.
	m.newShared()
	// And nothing this client laid over the old session's fold is about the
	// new one (plan 027 §3.13: a restore clears every overlay).
	m.clearOverlays()
	m.modeRev, m.modelRev, m.configRev = 0, 0, 0
	m.cmdSeq, m.chains = 0, nil
	// A new session is a new start: its session-is-up tail is still to run,
	// and its backend's Ready is still to come.
	m.upDone = false
	m.startInc = ""
	// Its host's facts are its own, read again from its backend (recompute);
	// its count of attached clients comes on its own stream (presence.go).
	m.hostPerm, m.hostStart = backend.PermissionUnsaid, time.Time{}
	m.attached = 0
	m.owner.set(nil)
}

// sessionBackendHook, when a test sets it, wraps every backend the model
// adopts — the one setSession builds, and a Config.Backend: the jitter run (plan 027 §8, V8) delays each of the backend's
// answers and reads through it. It is nil in production — no Config field or
// flag reaches it — like gateHook.
var sessionBackendHook func(backend.Backend) backend.Backend

// nextCmd is the model's next command id. Every mutating engine call carries
// one, so the events it causes name their cause and the model can tell its own
// effects' echoes from another client's change (§3.2).
//
// The client is the backend's, read here for every command and never kept
// (plan 027 §3.12): a socket client that reconnects without resuming has a new
// id, and a command must name the client it is sent as. No backend, or one
// with no client, is the zero Command, as it always was.
func (m *Model) nextCmd() engine.Command {
	if m.eng == nil {
		return engine.Command{}
	}
	client := m.eng.ClientID()
	if client == "" {
		return engine.Command{}
	}
	m.cmdSeq++
	return engine.Command{Client: client, ID: fmt.Sprintf("%d", m.cmdSeq)}
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
	cfg = cfg.viewing().launching()
	cwd := configWorkspace(cfg.Workspace)

	th := Preset(cfg.Theme)
	prov := configProvider(cfg.Provider)
	loads := &completeLoadSet{}
	// The TUI's half, from the Config; the session's half is the one
	// constructor every session's state is made by, a switch's included
	// (withSession, plan 030 §3.11).
	m := Model{
		theme:           th,
		input:           newComposer(th),
		yolo:            cfg.Yolo,
		mouseEnabled:    !cfg.NoMouse,
		providerLocked:  cfg.ProviderLocked,
		persistProvider: cfg.PersistProvider,
		fallbackDefault: cfg.FallbackDefault,
		providerDefault: prov,
		providers:       pickerRows(cfg.Providers, prov),
		newSession:      cfg.NewSession,
		loadSession:     cfg.LoadSession,
		spawnNew:        cfg.NewBackend,
		spawnLoad:       cfg.LoadBackend,
		cont:            cfg.Continue,
		sessions:        cfg.Sessions,
		claimSession:    cfg.ClaimSession,
		refuseLoad:      cfg.RefuseLoad,
		onEngine:        cfg.OnEngine,
		localPresence:   cfg.LocalPresence,
		resume:          resumeRows(cfg.Resume),
		sessionIndex:    cfg.SessionIndex,
		crazeID:         cfg.CrazeSessionID,
		terminalTitle:   cfg.TerminalTitle,
		host:            cfg.Host,
		viewer:          cfg.Viewer,
		// Discard until Run says otherwise: a model built by a test, by
		// `craze frame` or by any direct caller writes no OSC at all.
		term:          newTerminalColors(io.Discard),
		owner:         &sessionOwner{},
		exit:          &exitState{},
		shell:         newShellController(),
		sessRosters:   &sessRosterSet{},
		retired:       &backendSet{},
		completeLoads: loads,
		signIns:       &signInRuns{},
		// The composer's `@` popup searches the disk: rg, git or a walk of
		// the workspace (at_files.go). A test hands it a listing of its own
		// (setSource).
		composerAt: newComposerAt(atFileSource{search: newAtFileSearcher().search}, loads),
		gateSync:   gateSyncDefault,
		nativeDir:  configNativeDir(cfg.NativeDir),
		nativeEnv:  configGetenv(cfg.Getenv),
		attachDir:  configAttachmentsDir(cfg.AttachmentsDir),
		attachRuns: newAttachLane(),
		// The first session shown is shown from here, before any backend
		// of it is adopted (shownGen): 1, so no message the program makes
		// carries the zero stamp a test's hand-built one does.
		shownGen: 1,
	}.withSession(sessionSeed{workspace: cwd, model: cfg.Model, provider: prov.Name(), loading: cfg.Loading})
	sess := cfg.Session
	switch {
	case cfg.Backend != nil:
		// A session served elsewhere: there is nothing to pick, build or
		// claim (Config.Backend).
	case len(m.resume) > 0:
		// --resume outranks the provider picker: every row carries its own
		// provider and choosing one locks it, so asking which provider to
		// start before asking which session to load would be asking a
		// question the answer overrides (§3.1).
		m.pickingResume = true
		m.dialog = dialogResume
	case (m.newSession != nil || m.spawnNew != nil) && !m.providerLocked && m.cont == nil:
		m.pickingProvider = true
		m.dialog = dialogProvider
		m.providerCursor = m.providerIndex(prov)
	case m.launch():
		// The launch flow with its session known: nothing is built here, and
		// Init spawns it (plan 030 §3.5). New records the attempt Init's call
		// is, since Init cannot — m.reading's rule, below.
		m.spawnSeq++
		m.spawnWaiting, m.spawnFrom = m.spawnSeq, dialogNone
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
	if cfg.Backend != nil {
		m.setBackend(cfg.Backend)
		// Its count comes on its stream (attached); a host's own count is
		// the in-process path's alone.
		m.localPresence = nil
	} else {
		m.setSession(sess, m.crazeID)
	}
	// Init arms the stream's first read exactly when it has a session to read
	// and no picker to wait for, and it cannot record that itself (a value
	// receiver whose model is thrown away), so the model starts out agreeing
	// with it here: the gate's reader rule counts that read as in flight.
	m.reading = m.eng != nil && !m.picking()
	m.recompute()
	if m.model == "" && m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
	}
	if m.model == "" {
		m.model = "default"
	}
	return m
}

// Result is how a run ended, for the command line's last word (Run).
type Result struct {
	// AgentDiag says the agent's own diagnostics should print after exit:
	// broader than just an agent exit, see finishRun (§3.7.3).
	AgentDiag bool
	// Ended says the backend's stream ended and that End is what quit the
	// program: the session closed on its host, or the transport gave up
	// (Config.Backend; in process no End ever comes). EndErr is why — nil for
	// the session's own end. A view close ends nothing: it is this program
	// quitting, and Ended stays false.
	Ended  bool
	EndErr error
	// StartErr is the session's start failure, when it never came up. Run
	// returns it as its error unless p.Run failed itself: a caller telling
	// the program's own failure from the start's compares the two.
	StartErr error
	// Stopped says the explicit quit — /exit, Ctrl+D, the second Ctrl+C —
	// asked the session's host to end it and the host took it (plan 030
	// §3.6): the session's end that may have followed is this program's own
	// doing, and nothing to report. StopUnsupported says the host cannot stop
	// its session — an older craze, a TUI-hosted one — so the quit detached
	// and the session runs on there. StopErr is a stop that was neither: sent,
	// and answered by neither its receipt nor the session's end. All three
	// are for a backend served elsewhere; an engine this program built is
	// closed by its quit, as ever.
	Stopped         bool
	StopUnsupported bool
	StopErr         error
}

// Run returns how the run ended (Result) and the start failure, if any
// (§3.7.3), or p.Run's own error.
func Run(cfg Config) (Result, error) {
	cfg = cfg.viewing()
	sweepAttachmentsAtStart()
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
	res := Result{AgentDiag: showAgentDiag || err != nil, StartErr: startErr}
	res.Stopped, res.StopUnsupported, res.StopErr = m.exit.outcome()
	if fm, ok := final.(Model); ok {
		// The End that quit the program, and why, as the final model holds
		// them (endMsg).
		res.Ended, res.EndErr = fm.ended, fm.endErr
	}
	if err != nil {
		return res, err
	}
	// A quit is clean unless the session never started. Only startCmd's
	// failure counts: an error mid-session leaves a usable craze, and quitting
	// out of one is a normal exit.
	return res, startErr
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
// session closes — a session list's roster still open, and any backend a
// switch retired whose close has not run, closed just before it (plan 030).
// p.Run returns on /exit, on SIGINT/SIGTERM, on SIGHUP (Run's
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
	// A session list's roster left open: the list closes its own as it
	// closes and on its own quit, but a signal, a program error or a
	// recovered panic quits with the list up, and a leave's close is a command
	// the program may have stopped before running (sol r19-c10 3). The set is
	// shared by every copy, so m reaches what the final model had open even
	// when final is nil. Bounded: Close cancels every attempt in flight.
	m.sessRosters.closeAll()
	// A completion popup's load still running — the list's listing of a
	// directory, a quit the popup never saw having ended the program (sol
	// r28-c14 2) — is cancelled with it.
	m.completeLoads.cancelAll()
	// A /connect sign-in still running — its loopback listener, and a wait a
	// browser's late redirect could still finish into the native directory —
	// is ended the same way, before the engine's Close, which may block: none
	// of SIGTERM, SIGHUP or a recovered panic passed through requestQuit's
	// dropConnect (review r14 2). The set reaches an attempt Begin returned
	// whose answer the program stopped before reading, too.
	m.signIns.closeAll()
	// And a backend a switch let go of, or a dial answered after the user
	// had moved on, whose close — a command — the program stopped before it
	// ran (plan 030 §3.11): a view close each, bounded, side by side.
	m.retired.closeAll()
	// The owner and not m.eng: m is the model Run started with, and a session
	// a picker built after it lives only in later copies — which a recovered
	// panic does not hand back. Every copy shares the owner, so it names the
	// engine the program ended with on every exit path. A zero Model has none.
	// Closing the engine closes its session — and stops its driver first, so
	// nothing is left running behind the program — and answers with what the
	// session's own Close said.
	//
	// After an explicit quit that stopped a session served elsewhere, the close
	// shares that quit's one deadline (exitState.closeBackend, plan 030 C5r).
	var agentExited bool
	if m.owner != nil {
		if eng := m.owner.current(); eng != nil {
			agentExited = errors.Is(m.exit.closeBackend(eng), agent.ErrAgentExited)
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
// is a no-op with no cards and no running turn, recompute is idempotent, and
// the only card-producing events — permission, question and plan — are requests
// an agent makes of a live turn and never appear in a replay (§2.1).
//
// The read it arms is the one the command gate's reader rule counts: New set
// m.reading to match, since this value receiver cannot.
func (m Model) Init() tea.Cmd {
	// The host TUI's count of attached clients is read for the program's
	// whole life, a picker's wait included: it is the server's, not a
	// session's (presenceCmd; nil off the in-process path).
	presence := presenceCmd(m.localPresence)
	if m.picking() {
		return presence
	}
	if m.spawnWaiting != 0 {
		// The launch flow's session is not here yet: Init spawns it, and its
		// start and its reader are armed once it is adopted (launch.go).
		return tea.Batch(m.initSpawn(), presence)
	}
	return tea.Batch(m.startCmd(), waitEvent(m.eng, m.bgen), presence)
}

// startCmd starts the session through the engine, whose gate opens on it: until
// it has returned the engine admits no command at all. Its answer carries the
// backend generation it was dispatched under (bgen, staleBackend).
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
	iss, bgen := m.issue(), m.bgen
	return func() tea.Msg {
		if err := eng.Start(context.Background()); err != nil {
			return errMsg{issued: iss, err: err, eng: eng, bgen: bgen}
		}
		return startedMsg{issued: iss, eng: eng, bgen: bgen}
	}
}

// staleFor reports that a start command's message is about a backend the model
// no longer holds — one a picker closed and replaced — so nothing it says
// applies. It is interface identity: the same backend, not an equal one. The
// start messages carry the session generation too (issued, outdated), which
// every other result now carries: a restore from another incarnation (PR 4)
// moves the generation without replacing the backend. staleFor stays for a
// start message a test builds by hand around a backend.
func (m Model) staleFor(b backend.Backend) bool { return b != nil && b != m.eng }

// Update is the command gate (gate.go) over the handler: a message that
// arrives while a gated call is waiting for its reply, or while the messages
// held behind one are still draining, is held in arrival order; everything
// else runs the handler and then the wrapper (finish).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	return m.gated(msg, Model.update)
}

// finish is the Update wrapper, run after the handler of every message that is
// applied — never for a held one, which runs it when it is drained: it lays
// the frame out exactly once, from the state the handler left behind, and
// keeps the single tick chain alive.
func (m Model) finish(cmd tea.Cmd) (Model, tea.Cmd) {
	next := m
	// Mutation only marks the transcript dirty. Paint the drawn one here so a
	// background transcript (U3b) never moves m.vp — and, while a replay
	// runs, only on its cadence (paintDue).
	if next.cur().dirty && next.paintDue() {
		next.refreshViewport()
	}
	next.paintNow = false
	// The composer's `@` popup follows the draft the handler left (plan 030
	// §3.16): synced here, where every change to the draft passes, and before
	// the layout that gives it its rows. Its search, when an opening starts
	// one, runs off the Update.
	if at := next.syncComposerAt(); at != nil {
		cmd = tea.Batch(cmd, at)
	}
	// The draft's sidecar follows it the same way (plan 033 §3.3): a chip no
	// longer in the text — broken by hand, gone with an external editor's
	// rewrite, cleared with the draft — takes its image with it.
	next.reconcileImages()
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
	if m.outdated(msg) && !m.ownStart(msg) {
		// A result issued for a session the model has since left (issued):
		// nothing it says applies to this one.
		return m, nil
	}
	// The session list's own messages, and the list ahead of everything
	// else while it is open (plan 030 §3.10, sessions_list.go): its keys
	// are handleKey's first rung; the mouse is dropped, and a paste lands in
	// the list's input (§3.13) or, with none, is dropped; the end of the
	// session behind it — or of its connection — leaves craze running, its row
	// marked ended or left to the roster (sessionEnded). Everything else — the
	// session's stream, its command replies, the ticks — is applied as ever,
	// behind the list.
	if next, cmd, ok := m.applySessMsg(msg); ok {
		return next, cmd
	}
	if p, ok := msg.(pasteMsg); ok && p.textRefused && !p.hasImage() {
		// Clipboard "text" that was not text (plan 033 C3r): nothing lands,
		// wherever it was asked for, and the status row says why. With an
		// image, pasteClipboardImage says it beside what it did.
		m.note(clipboardNotText)
	}
	if m.sessList.open {
		switch msg := msg.(type) {
		case tea.MouseMsg, dblClickMsg:
			return m, nil
		case pasteMsg:
			// A paste lands where it was asked for (X140, C15): one asked for
			// in the list's input there, where there is one, and only in the
			// opening of the list it was asked in — one from an opening since
			// closed is dropped, the list opened again over the same session
			// included (C15r); one asked for in the composer before the list
			// opened in the composer's draft, which is where the user finds it
			// on going back.
			if msg.hasImage() && msg.listGen == 0 && msg.key == (keyField{}) {
				// An image off the clipboard, asked for in the composer (plan
				// 033 §3.3): a chip in the composer's draft, where the
				// composer's text paste lands too.
				return m.pasteClipboardImage(msg)
			}
			if msg.text == "" || msg.key != (keyField{}) {
				// One asked for in /connect's key field (plan 031 §3.9) lands
				// in that field while it is open, or nowhere: never in the
				// composer's draft the list covers.
				return m, nil
			}
			if msg.listGen != 0 {
				if m.sessList.in.on && msg.listGen == m.sessList.gen {
					return m.sessInputPaste(msg.text)
				}
				return m, nil
			}
			return m, m.updateComposer(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(msg.text), Paste: true})
		case endMsg:
			// An unstarted session's first prompt (§3.13) whose session ended
			// with the list up over it — its composer emptied and ← pressed
			// before it came up — is dropped, as a start failing there drops
			// it (the errMsg arm, X154): the end is the session's, as any
			// session's is behind the list, and a start answering after it
			// sends nothing to a session that has ended (plan 030 C15r4,
			// CodeRabbit on #71).
			m.first = nil
			m.ended, m.endErr = true, msg.err
			m.attached = 0
			m.sessionEnded(msg.err)
			return m, nil
		}
	}
	switch msg := msg.(type) {
	case restoreMsg:
		// The backend's stream replaced what the model held (restore.go): the
		// whole model is initialised from the snapshot and its facts, and the
		// session's last ending, which no snapshot carries, is read once
		// beside it (lastturn.go).
		if !m.applyRestore(msg) {
			return m, nil
		}
		// A new attachment: its own count of attached clients follows it
		// (presence.go), and until then there is none.
		m.attached = 0
		return m, m.readLastTurn()

	case presenceMsg:
		// How many clients are attached, this one included (presence.go):
		// the stream's latest, 0 when it no longer knows.
		m.attached = msg.n
		return m, nil

	case localPresenceMsg:
		// The host TUI's own server's count, and the next read of it.
		m.hostAttached = msg.n
		return m, presenceCmd(m.localPresence)

	case lastTurnMsg:
		m.applyLastTurn(msg)
		return m, nil

	case readyMsg:
		// The host's start completed: the backend's Info already holds the
		// facts it published (its catalogs above all), so the mirror is read
		// again. A start that failed is answered by Start too, as errMsg —
		// which the incarnation this Ready arrived in tells about (startInc).
		if m.startInc == "" {
			m.startInc = m.incarnation()
		}
		m.recompute()
		return m, nil

	case endMsg:
		// The stream ended: the session closed on its host, or the transport
		// gave up. Nothing more will come. With a session list the TUI goes
		// back to it, where every other session still is (plan 030 §3.10: a
		// viewed session that ends — another client's /exit, the list's
		// ctrl+x, an idle exit — or whose connection is lost, when its row
		// opens it again: endedToList); without one — the opt-out, craze
		// attach — or on the way out after this client's own quit asked for
		// the end, the program quits, and the final model says why (ended,
		// endErr) for the command line's last word. /connect goes with the
		// session it was opened in, its key field emptied, whichever way
		// this goes (plan 031 §3.9, dropConnect): the list covers the box,
		// and nothing would reach it or its field again.
		m = m.dropConnect()
		// Nobody is attached through a stream that has ended.
		m.attached = 0
		if f := m.first; f != nil && !m.quitting {
			// The session an unstarted session's first prompt spawned ended
			// before it came up: the same as its start failing (§3.13).
			err := msg.err
			if err == nil {
				err = errors.New("the session ended before it started")
			}
			return m.backToUnstarted(*f, err)
		}
		m.ended, m.endErr = true, msg.err
		if m.sessions != nil && !m.quitting {
			if next, cmd, ok := m.endedToList(msg.err); ok {
				return next, cmd
			}
		}
		m.quitting = true
		return m, tea.Quit

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
		cmd := m.handleHiddenRetry()
		return m, cmd

	case hiddenRefusedMsg:
		// The outbox had no room for a hidden answer: it goes back on the
		// retry list, and the wrapper arms its beat (armHiddenRetry).
		m.hiddenRetry = append(append([]hiddenAnswer(nil), m.hiddenRetry...), msg.h)
		return m, nil

	case resumeClaimMsg:
		return m.resumeClaimed(msg)

	case spawnedMsg:
		return m.spawned(msg)

	case unstartedSpawnedMsg:
		return m.unstartedSpawned(msg)

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
		m.comeUp()
		if m.first != nil {
			// An unstarted session's first prompt, now that the session its
			// spawn adopted is up (plan 030 §3.13): through the ordinary
			// submit path, with this TUI's own command numbering.
			return m.sendFirst()
		}
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
		if m.startLeft() {
			// Over a socket, the start answered for an incarnation the model
			// has since left — a restore of another has been applied after
			// the Ready that failed (plan 030 C7r2): the failure is that
			// session's, not this one's, and it draws nothing and fails
			// nothing here. The start has answered all it will, so the
			// session the model now holds comes up as a late startedMsg
			// would bring it up.
			m.comeUp()
			if m.first != nil {
				return m.sendFirst()
			}
			return m, nil
		}
		if f := m.first; f != nil {
			// The session an unstarted session's first prompt spawned did not
			// come up: it is that unstarted session again, the error drawn
			// and the prompt still in the composer (plan 030 §3.13). Not the
			// session's start failure: nothing of it was up, and craze goes
			// on. With the list up over it (its composer emptied and ← pressed
			// meanwhile) the failure is the session's, as any start's is, and
			// its prompt is dropped.
			m.first = nil
			if !m.sessList.open {
				return m.backToUnstarted(*f, msg.err)
			}
		}
		m.status = statusError
		m.err = msg.err.Error()
		m.startErr = msg.err
		m.addError(m.err)
		return m, nil

	case actionErrMsg:
		m.land(msg.landed)
		m.addError(failureText(msg.err))
		return m, nil

	case revertModeMsg:
		if msg.gen != m.modeGen {
			// Stale: a newer request of the user's own is what the chip is
			// showing, so this refusal may neither take the newer request's
			// overlay down nor clear its flag. The error is still theirs to
			// see — the agent refused something they asked for, and
			// swallowing that would be a bug of its own.
			m.addError(failureText(msg.err))
			return m, nil
		}
		// The revert is the last word on this request: its overlay goes, and
		// nothing is put back in its place — the chip reads the fold, so an
		// agent-initiated mode that landed while this was on the wire is on
		// the chip rather than lost behind a captured value.
		m.modeInFlight = ""
		m.refuseMode(msg.gen)
		m.addError(failureText(msg.err))
		return m, nil

	case modeAppliedMsg:
		return m.modeSettled(msg.gen, msg.id, msg.rev), nil

	case revertModelMsg:
		// Its own overlay goes, and nothing is put back (SF-38): the screen
		// reads the fold's model — the one before the call, or whatever the
		// agent or another client has moved it to since. The error row is the
		// user's to see either way.
		m.refuseModel(msg.gen)
		m.addError(failureText(msg.err))
		return m, nil

	case modelUnreadMsg:
		// Read back rather than put back: nothing of the answer was
		// installed, so the overlay goes and the fold is the model the
		// session is on as far as anyone can say.
		m.refuseModel(msg.gen)
		m.addError(m.unreadModelText())
		return m, nil

	case modelSetMsg:
		// The model step landed: its overlay holds what the session confirmed
		// until the fold's model revision reaches it.
		m.land(msg.landed)
		return m, nil

	case effortSetMsg:
		// The model step landed, and then the effort step: each overlay holds
		// what the session confirmed until the fold reaches it.
		m.land(msg.landed)
		m.confirmOption(msg.landed.gen, msg.cause, msg.id, msg.res.Value, msg.at, msg.res.Rev)
		return m, nil

	case modelApplyMsg:
		// The steps that landed are the truth; the one that did not is named.
		for _, st := range msg.done {
			m.addNote(st.note)
		}
		// This apply's overlays: a step that landed holds its confirmed value
		// until the fold reaches it, and every other one of its requests —
		// refused, skipped, or never sent once an earlier step failed — goes,
		// with nothing put back. An older apply's answer settles its own
		// overlays the same way, and cannot touch a newer one's (settleApply).
		m.settleApply(msg)
		// After the rows are settled, because a model step whose answer could
		// not be read names the model the screen then shows.
		switch {
		case msg.unread:
			m.addError(m.unreadModelText())
		case msg.err != nil:
			m.addError(msg.step + ": " + failureText(msg.err))
		}
		return m, nil

	case refreshSnapMsg:
		m.recompute()
		return m, nil

	case effortNotAppliedMsg:
		// The model step landed and the effort did not follow it: nothing of
		// the effort was written, so the note says why.
		m.land(msg.landed)
		m.recompute()
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
		m.addError(failureText(msg.err))
		return m, nil

	case foreignCancelledMsg:
		// The engine accepted a cancel Esc made on the agent's own turn: what
		// it cancelled decides the note and the cancelled state (X48).
		m.applyForeignCancelled(msg)
		return m, nil

	case clipboardDoneMsg:
		m.copyNote = msg.note
		m.copyUntil = m.now().Add(copyNoteLinger)
		return m, nil

	case connectAnswer:
		// /connect's reads and its save, and the model dialog's connect row
		// (connect_dialog.go, plan 031 §3.9).
		return m.applyConnect(msg)

	case pasteMsg:
		// The read is asynchronous, so the composer may no longer be where the
		// keyboard is by the time the text arrives. One asked for in a session
		// the model has since left — before that session's backend was
		// adopted included (C11r2) — never gets here: the gate dropped it
		// (staleShown, pasteMsg). One asked for in the session list's input,
		// which has closed since, had only that input to land in.
		if msg.key != (keyField{}) {
			// Asked for in /connect's key field (plan 031 §3.9): it goes into
			// that field while it is still the one open, and nowhere else —
			// never the composer, whatever is on screen now.
			return m.pasteIntoKey(msg), nil
		}
		if msg.hasImage() && msg.listGen == 0 {
			// An image off the clipboard (plan 033 §3.3): a chip, or — where
			// no chip may be made — the clipboard's text after all.
			return m.pasteClipboardImage(msg)
		}
		if msg.text == "" || msg.listGen != 0 || m.composerCovered() {
			return m, nil
		}
		// One bracketed paste, the way a terminal delivers it: the textarea
		// inserts the whole thing as text instead of reading it as keys.
		return m, m.updateComposer(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(msg.text), Paste: true})

	case attachDoneMsg:
		// A chip's processing (plan 033 §3.3). One for a session the model
		// has since left never gets here (staleShown, attachDoneMsg).
		return m.attachAnswer(msg)

	case completeLoadedMsg:
		// The composer `@` popup's search (plan 030 §3.16): the list's own
		// results were applySessMsg's. One asked for in a session the model
		// has since left never gets here (staleShown, X125).
		if msg.source != atFilesSourceID {
			return m, nil
		}
		return m.composerAtLoaded(msg)

	case dblClickMsg:
		// The frame runner's deterministic double-click: two real presses would
		// make a golden depend on the clock. The state machine itself is
		// unit-tested against the injected one.
		if !m.mouseEnabled || m.cardOpen() {
			return m, nil
		}
		if m.lay.Region(regionTranscript).Contains(msg.Y) {
			// The word is read from the rows as they stand (catchUp), as a
			// press reads them.
			m.catchUp()
		}
		if !m.selectable(msg.X, msg.Y) {
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
		m = m.modeSettled(msg.gen, msg.mode, msg.rev)
		if msg.offer != m.offerGen {
			// A restore retired the offer this answers (offerGen): the plan
			// it would implement is not the transcript on screen. The mode
			// request's own bookkeeping settles; nothing is written or sent.
			return m, nil
		}
		// The session is in the implement mode now, so the note is honest
		// whatever else has happened meanwhile — and so is its snapshot.
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
		// which case there is no plan above to implement any more: an action,
		// or a restore (offerGen).
		tm, cmd := m.update(revertModeMsg{gen: msg.gen, prev: msg.prev, err: msg.err, at: msg.at})
		next := tm.(Model)
		if msg.seq == next.turnSeq && next.planDeadSeq != next.turnSeq && msg.offer == next.offerGen {
			next.planOfferSeq = next.turnSeq
		}
		return next, cmd

	// An eventMsg never reaches the handler: the gate applies an event itself,
	// through applyEvent, and owns the reader (gate.go's apply and readOn), so
	// no path can arm a second read of the stream. Every ending is one event
	// now, the engine's EventTurn{ended}, and everything a settled turn left to
	// do — the drain, an armed send-now, the queue the chain policy clears — is
	// the engine's own decision, arriving as the events it authored.

	case frameQuitMsg:
		// The frame runner's quit, applied in its turn behind whatever had
		// arrived before it (frame.go). A lingering one marks the capture and
		// leaves the program running: the socket run's check after it
		// (frameRunner.captureThenMatch).
		m.harnessQuit = true
		if msg.linger {
			return m, nil
		}
		return m, tea.Quit

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
			// slashBandActive, not slashActive: over the composer's `@`
			// popup the wheel is the transcript's (plan 030 X187), even
			// for a quoted `@"a /` whose `/` the menu would read.
			if m.slashBandActive() && m.lay.Region(regionOverlay).Contains(msg.Y) {
				return m.slashWheel(-1), nil
			}
			// The selection is in transcript rows, not screen rows, so it
			// scrolls with the text it holds and survives the wheel. The
			// scroll is from the rows as they stand (catchUp).
			m.catchUp()
			m.vp.ScrollUp(wheelLines)
		case tea.MouseButtonWheelDown:
			if m.slashBandActive() && m.lay.Region(regionOverlay).Contains(msg.Y) {
				return m.slashWheel(1), nil
			}
			m.catchUp()
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
	if m.lay.Region(regionTranscript).Contains(y) {
		// A selection starts from the rows as they stand, so a press into the
		// transcript paints them first when a replay's cadence left them
		// unpainted (catchUp): before it asks whether there is a row under
		// it, and before it anchors a selection, which a paint would clear.
		m.catchUp()
	}
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
	// Rows a replay's cadence left unpainted are painted first (catchUp), as
	// a paint on every event would have painted them before this motion came:
	// that paint clears the selection, and a drag with no selection under way
	// does nothing.
	m.catchUp()
	if !m.sel.drag {
		return m
	}
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
	// As a drag does (handleDrag): a selection over rows the cadence left
	// unpainted goes with the paint, so nothing is hit-tested or copied from
	// them.
	m.catchUp()
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
	tr := m.cur()
	if pos.line >= tr.drawnLen() {
		return m
	}
	lo, hi, ok := wordAt(tr.plainRow(pos.line), pos.col)
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
	return m, copyRows(m.shownGen, text)
}

// copySelectionOrLastReply is Ctrl+Y. Without a selection it copies the last
// reply's own text rather than the rows it was wrapped into, so what lands on
// the clipboard is the agent's paragraph and not the screen's line breaks.
func (m Model) copySelectionOrLastReply() (tea.Model, tea.Cmd) {
	// A selection over rows a replay's cadence left unpainted goes with the
	// paint (catchUp), as it went with a paint on every event: what is copied
	// then is the last reply.
	m.catchUp()
	if !m.sel.empty() {
		return m.copySelection()
	}
	rows := m.cur().rows
	for i := len(rows) - 1; i >= 0; i-- {
		if e := rows[i]; e.kind == entryAssistant && e.text != "" {
			return m, copyText(m.shownGen, e.text, "copied last reply")
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
		if m.composerAtShown() {
			// The composer's `@` popup is the keyboard's alone (plan 030
			// §3.16): a click on it does nothing, and never reaches a slash
			// token inside a quoted `@"…"` the band is not drawing.
			return m, nil
		}
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
			return m.showModelDialog()
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
	case dialogConnect:
		return m.connectDialogClick(i)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The session list is the first rung while it is open (plan 030 §3.10):
	// ahead of Ctrl+D and Ctrl+C, a card, a dialog, the confirm line and the
	// sub-agent view, none of which may take a key from it.
	if m.sessList.open {
		return m.handleSessionsKey(msg)
	}
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
	// answered. The one exception is an unmodified ← where there is a session
	// list (SF-99, plan 032 §3.2 C3): it leaves the session for the list with
	// the card unanswered. The ask stays open on the host, the list shows the
	// session under "needs you", and the card — its progress, the draft under
	// it and the sub-agent view it is over — is all still here on the way
	// back (leaveSessions touches none of them). No card binds ←, and Alt+←
	// stays the card's (swallowed): only the bare key passes through.
	if m.cardOpen() {
		if msg.Type == tea.KeyLeft && !msg.Alt && m.sessions != nil {
			return m.openSessions()
		}
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
	case dialogConnect:
		return m.handleConnectDialogKey(msg)
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
		return m, pasteFromClipboard(m.shownGen, 0)
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
		handled, next, cmd := m.handleQueueKey(msg)
		m = next
		if handled {
			return m, cmd
		}
	}
	if m.agentFocus {
		handled, next, cmd := m.handleRowsKey(msg)
		m = next
		if handled {
			return m, cmd
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
		// The composer's `@` popup takes Esc where the slash menu does, and
		// for the same thing (plan 030 §3.16, X127): it hides for the token
		// under the cursor, until that token changes, and the draft is
		// untouched — a second Esc then cancels a running turn, no slash
		// menu taking it on the way for a `/` inside the hidden token
		// (slashBandActive).
		if next, ok := m.composerAtKey(msg); ok {
			return next, nil
		}
		if m.slashBandActive() {
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
			// what takes the text back before it turns into a turn. The ladder
			// ends here, so the Disarm's continuation is all there is after it.
			return m.withdrawSendNow("send now dropped", linkDone)
		}
		if m.status == statusWorking {
			// The queue survives Esc on purpose: cancelling this turn is not
			// cancelling what was meant to follow it, and the drain sends the
			// head once the cancelled turn settles.
			return m.cancelTurn()
		}
		if m.foreignTurnStoppable() {
			// Nothing of craze's own is working, but the agent is running a
			// turn of its own — native's wake — and Esc is how it is stopped
			// (plan 026 X44).
			return m.cancelForeignTurn()
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
	//
	// The composer's `@` popup is the same kind of band (plan 030 §3.16,
	// X127): ↑/↓, ctrl+p/ctrl+n and tab are its while it is up — ahead of the
	// arrows that leave the composer and of the textarea's own line keys —
	// and PgUp/PgDn stay the transcript's, the slash menu having no rows
	// inside an `@` token (slashBandRows). Enter is handleEnter's.
	switch msg.Type {
	case tea.KeyTab, tea.KeyUp, tea.KeyDown, tea.KeyCtrlP, tea.KeyCtrlN:
		if next, ok := m.composerAtKey(msg); ok {
			return next, nil
		}
	}
	switch msg.Type {
	case tea.KeyTab, tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown:
		if rows := m.slashBandRows(); rows > 0 {
			return m.handleSlashKey(msg.Type, rows), nil
		}
	}
	if msg.Type == tea.KeyPgUp || msg.Type == tea.KeyPgDown {
		// A page of the rows as they stand (catchUp), so where it leaves the
		// transcript, and whether it still follows the bottom, is decided on
		// them.
		m.catchUp()
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
	// ← on an empty composer opens the session list (plan 030 §3.10) — only
	// where there is one, so without Config.Sessions the key reaches the
	// textarea exactly as it always has. alt+← stays the composer's word
	// motion. Everything that owns the keyboard — a card, a dialog, the
	// confirm line, the sub-agent view, the focused bands — has had the key
	// above; a queue edit holds its text in the composer, so it is not empty.
	if msg.Type == tea.KeyLeft && !msg.Alt && m.sessions != nil && m.input.Value() == "" && m.queueEdit == "" {
		return m.openSessions()
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
func (m Model) handleRowsKey(msg tea.KeyMsg) (bool, Model, tea.Cmd) {
	items := m.visibleAgents()
	if len(items) == 0 {
		m.focusComposer()
		return false, m, nil
	}
	if stopped, next, cmd := m.stopRowKey(msg, items); stopped { // subcancel.go
		return true, next, cmd
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
		return true, m, nil
	case tea.KeyDown:
		if m.agentSel < len(items)-1 {
			m.moveAgent(1)
		}
		return true, m, nil
	case tea.KeyEnter:
		if m.agentID != "" {
			m.enterView(m.agentID)
		} else {
			m.selectAgent(m.agentSelection(len(items)))
		}
		return true, m, nil
	case tea.KeyEsc:
		m.focusComposer()
		return true, m, nil
	}
	m.focusComposer()
	return false, m, nil
}

func (m *Model) updateComposer(msg tea.KeyMsg) tea.Cmd {
	// A blurred textarea drops every key, so whatever took the cursor away
	// (Esc on an idle turn, the rows) gives it back before the key lands.
	if !m.input.Focused() {
		_ = m.input.Focus()
	}
	// bubbles' word-left (textarea v0.21.0's wordLeft, on its WordBackward
	// keys: alt+←, alt+b, and macOS Terminal's ESC b, which arrives as alt+b)
	// never returns when nothing but whitespace is before the cursor — an
	// empty composer among them: it steps left looking for a word's end, and
	// at the start of the text a step left no longer moves, so it steps for
	// ever and craze hangs. Wherever it does return with only whitespace
	// before the cursor (the cursor at the very start, a word under it), it
	// has not moved the cursor, so the key is dropped there: the same result,
	// without the hang. The forward motions and the word deletions stop at
	// the end of the text on their own (TestWordMotionOverBlankTextReturns).
	if key.Matches(msg, m.input.KeyMap.WordBackward) && strings.TrimSpace(m.input.Value()[:m.composerCursorOffset()]) == "" {
		return nil
	}
	// Images (plan 033 §3.3, composer_image.go). Every composer paste, drop,
	// Alt+V, Backspace and Delete passes here, and nowhere else does: never
	// the session list's input, /connect's key field, or a dialog's.
	var imgCmd tea.Cmd
	switch {
	case msg.Alt && msg.Type == tea.KeyRunes && len(msg.Runes) == 1 && msg.Runes[0] == 'v':
		// Alt+V is Ctrl+V's alias: the clipboard's image, then its text.
		return pasteFromClipboard(m.shownGen, 0)
	case msg.Paste && len(msg.Runes) == 0:
		// An empty bracketed paste: what a terminal sends for an image it
		// could not paste as text. Its clipboard is the GUI's — except over
		// SSH, where the clipboard craze could read is the remote machine's,
		// not the one the user pasted from — and a `!` command line takes no
		// image.
		if m.overSSH() || m.shellMode() {
			return nil
		}
		return probeClipboardImage(m.shownGen)
	case msg.Paste:
		// A paste of image paths becomes their chips, inserted as the paste
		// would have been; any other paste goes in as it came.
		if chips, cmd := m.pasteImages(string(msg.Runes)); cmd != nil {
			msg.Runes, imgCmd = []rune(chips), cmd
		}
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
	// Backspace at a chip's end, or Delete at its start, takes the whole chip
	// and its image (plan 033 §3.3); any other key is the textarea's.
	if !m.deleteChip(msg) {
		m.input, cmd = m.input.Update(msg)
	}
	cmd = tea.Batch(cmd, imgCmd)
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
	//
	// The cancel is the chain's last link (§3.12, astra 3): it is issued only
	// once the Disarm and the ClearQueue have answered, so the turn it ends
	// cannot settle into a row the user was clearing. The window is armed from
	// the moment of the key, read above, not from when the call answered.
	return m.clearPending(func(m Model) (Model, tea.Cmd) {
		tm, cmd := m.cancelTurn()
		next := tm.(Model)
		next.ctrlCDeadline = now.Add(ctrlCWindow)
		return next, cmd
	})
}

// clearPending empties everything the queue band is holding. The strong send's
// text does not come back here: Ctrl+C means stop, and a draft reappearing
// under the cursor would be one more thing to undo. It says nothing about the
// send-now it took back either — Ctrl+C is already the whole answer — which is
// why the withdrawal is applied with no note and its own delta skipped.
//
// Its two engine calls are ONE gated call (§3.12; clearPendingCall): the
// Disarm and then the ClearQueue, back to back on the call's goroutine, as
// they were back to back in today's one Update. Two gates would put a reply's
// round trip between them, and a send-now's own cancel that settles the turn
// inside it would drain the queue's head — a row the user was clearing —
// before the ClearQueue could take it. The confirm comes down, and both
// command ids are minted, in the Update that asks; the continuation is
// everything after, in today's order: the Disarm's outcome, the edit's end,
// the band, then the caller's own post-call work (then) — Ctrl+C's cancel,
// /clear's transcript. A call that did not answer (ErrNoAnswer) knows neither
// outcome: it says both, in that order, and goes on — the user asked for
// everything to stop.
func (m Model) clearPending(then linkThen) (Model, tea.Cmd) {
	m.confirm = nil
	if m.eng == nil {
		// No backend, so neither call: what followed them still runs.
		m.settlePending(engine.Command{}, engine.Command{}, clearAnswer{disarm: errNoBackend, clear: errNoBackend})
		return then(m)
	}
	dc, cc := m.nextCmd(), m.nextCmd()
	return m.run(gateDeadline, clearPendingCall(dc, cc),
		func(m Model, r gateReply) (Model, tea.Cmd) {
			ans, ok := r.result.(clearAnswer)
			if !ok || r.err != nil {
				ans = clearAnswer{disarm: ErrNoAnswer, clear: ErrNoAnswer}
			}
			m.settlePending(dc, cc, ans)
			return then(m)
		})
}

// errNoBackend stands for a call a model with no backend never made: refused,
// with nothing to say, as today's code skipped it.
var errNoBackend = errors.New("tui: no backend")

// clearAnswer is clearPending's one call answered: what each of its two verbs
// came to, and the rows the ClearQueue removed.
type clearAnswer struct {
	disarm, clear error
	removed       []agent.QueuedPrompt
}

// clearPendingCall is clearPending's gated call: the Disarm, then the
// ClearQueue whatever the Disarm came to — nothing armed is the usual answer,
// and the queue is cleared all the same. Each verb that gave up because the
// call's deadline passed did not answer (unanswered).
func clearPendingCall(dc, cc engine.Command) gateCall {
	return func(ctx context.Context, b backend.Backend) (any, error) {
		var ans clearAnswer
		ans.disarm = unanswered(ctx, b.Disarm(ctx, dc))
		removed, err := b.ClearQueue(ctx, cc)
		ans.clear = unanswered(ctx, err)
		ans.removed = removed
		return ans, nil
	}
}

// settlePending is clearPending's continuation up to the caller's own work, in
// today's order: the Disarm's outcome, the edit's end, then the band — from
// the ClearQueue's result: each row it removed is hidden until that row's own
// removal is folded, whether or not this client had folded the row at all (a
// row another client added a moment ago is among them, and must not flash
// back; §3.12's table). A ClearQueue that did not answer hides nothing: the
// band is the fold's.
func (m *Model) settlePending(dc, cc engine.Command, ans clearAnswer) {
	m.withdrawn(dc, "", ans.disarm)
	if m.queueEdit != "" {
		m.cancelQueueEdit()
	}
	if errors.Is(ans.clear, ErrNoAnswer) {
		m.note(noAnswerClearNote)
	}
	m.queueFocus = false
	m.queueHov = noHover()
	if ans.clear == nil && cc.Cause() != "" {
		for _, row := range ans.removed {
			m.ov = m.ov.withResult(rowAbsent(cc.Cause(), row.ID))
		}
	}
	m.recompute()
}

func (m Model) handleEnter() (tea.Model, tea.Cmd) {
	if m.confirm != nil {
		return m.confirmStrongSend()
	}
	// The composer's `@` popup owns Enter while it has a candidate to accept
	// (plan 030 §3.16, X127), ahead of the queue edit's save as the slash
	// menu is: the pick completes the text being edited. With none — still
	// searching, or nothing matches — Enter is the composer's, and the draft
	// goes as it is typed: not to a slash menu over a `/` inside the token
	// (slashBandActive), which would write a command into it.
	if next, ok := m.composerAtKey(tea.KeyMsg{Type: tea.KeyEnter}); ok {
		return next, nil
	}
	// The menu owns Enter while it is up, and it owns it before the queue edit
	// saves: a row accepted inside an edit completes the text being edited, and
	// the next Enter saves it. A token that already spells the highlighted row
	// falls through, so a fully typed /help still runs on the first press and a
	// fully typed /gauntlet still sends.
	if m.slashBandActive() && !m.slashExactlyTyped() {
		return m.acceptSlash(m.slashSel), nil
	}
	if m.queueEdit != "" {
		return m.saveQueueEdit(linkDone)
	}
	name, args, ok := parseSlashLine(m.input.Value())
	if ok && (name == "exit" || name == "rename" || (name == "sessions" && m.sessions != nil)) {
		// The builtins that run before the session is up. Quitting has
		// always had to; /rename joins it because the gate below refuses
		// silently, and a rename typed at a session that is still restoring
		// owes the user the reason rather than nothing at all (§3.6); and
		// /sessions, where there is a session list, opens it whatever the
		// session behind it is doing, as ← does (plan 030 §3.10). None
		// touches the wire, and none is reachable while a card is up —
		// handleKey hands the keyboard to the card before Enter gets here.
		return m.runBuiltin(name, args)
	}
	if m.unstarted != nil {
		// A new session not spawned yet (plan 030 §3.13): its first prompt
		// spawns it. A builtin or a shell command needs a session to run in.
		return m.enterUnstarted(ok && name != "" && builtinNamed(name))
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
	// /connect is a builtin of native sessions alone (plan 031 §3.9, CR 9):
	// anywhere else nothing claims it, and it goes to the agent below as the
	// text it is, as it always did. It is refused while work runs — with a
	// line, not the silence below — and never queued.
	if ok && name == connectBuiltin.Name && m.connectOffered() {
		return m.runBuiltin(name, args)
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
	// A chip still being processed holds the send (plan 033 §3.3): the draft
	// stays, and the status row says which image it waits for.
	if m.imagePending() {
		return m, nil
	}
	return m.submitOwn(text, m.images.list, engine.SubmitQueue, submitted)
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
	return m.submit(text, engine.SubmitQueue, "", submitted)
}

// submitOwn is submit for the text this composer is holding: the draft's send,
// whether it starts a turn or becomes a queued row, and the confirm's send-now
// of that same draft. It is the one submission that carries the pending shell
// context, and it is what clears it — only once the text was accepted, so a
// refusal leaves the block with the draft it belongs to, for the send that
// follows.
//
// The context is attached now, in the Update that sends, and dropped in the
// continuation, once the answer says the text was taken. A Submit that did not
// answer (ErrNoAnswer) keeps it: the prompt may never have been sent, and the
// block stays with the draft it belongs to, as for any refusal — the next send
// carries it.
//
// images is the sidecar the text was written with (plan 033 §3.3): the
// envelope for the chips text holds goes in front of everything, the shell
// context included (§3.1, withImages). The sidecar itself is the draft's, and
// is cleared with it — only once the send was accepted (clearMatchingDraft) —
// so a refused send keeps its chips and their images for the next attempt.
func (m Model) submitOwn(text string, images []attachment, mode engine.SubmitMode, then submitThen) (Model, tea.Cmd) {
	return m.submit(withImages(images, m.withShellContext(text)), mode, "", func(m Model, res engine.SubmitResult, err error) (Model, tea.Cmd) {
		if shellContextTaken(res, err) {
			m.dropShellContext()
		}
		return then(m, res, err)
	})
}

// submitThen is what a caller of submit did once Submit had returned: its own
// post-call work, which runs in submit's continuation after submit's own
// (§3.12 "The operation chain").
type submitThen func(m Model, res engine.SubmitResult, err error) (Model, tea.Cmd)

// submitted is the caller with nothing left to do once submit has applied the
// answer: send, sendText and confirmStrongSend all returned there.
func submitted(m Model, _ engine.SubmitResult, _ error) (Model, tea.Cmd) { return m, nil }

// noAnswerSubmitNote is a Submit that did not answer in time (ErrNoAnswer,
// §3.12): the command may have run, so the note says the prompt may have been
// sent, and the draft is kept — nothing was drawn and nothing is cleared.
const noAnswerSubmitNote = "no answer from the session — the prompt may have been sent"

// submit hands one prompt to the engine and applies what it answered. It is the
// one place the model does: a plain send, the plan offer's implement prompt, a
// row's send now, and a confirmed send-now all come through here, so the echo
// rule has one half to match, applied before any event the Submit caused.
//
// The call goes through the command gate (§3.12): what comes before it — the
// row's text read from the band, the command id — runs in the Update that
// sends; the answer is applied in the continuation, where the model is exactly
// as that Update left it and nothing that arrived meanwhile has been applied
// yet, and then the caller's own post-call work runs (then). In the gateSync
// baseline both halves run in the one Update, today's control flow.
//
// Exactly one thing happened, and the model applies exactly that one:
//
//   - a turn started while the model had nothing running, so the row is drawn,
//     the status goes working and the turn is stamped in the Update the answer
//     lands in — which is what a frame capture right after Enter sees, the
//     frames between being the gate's — and that turn id is the one started
//     event the model will skip;
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
//     went nowhere says so. A Submit that did not answer at all (ErrNoAnswer) is
//     the same one line, saying the outcome is unknown: nothing drawn, no echo
//     marker set, the draft kept.
//
// In process Submit waits on nothing but the inline index seed's lock; over the
// socket it is a round trip, which is why it is gated.
func (m Model) submit(text string, mode engine.SubmitMode, fromRow string, then submitThen) (Model, tea.Cmd) {
	if m.eng == nil {
		return then(m, engine.SubmitResult{}, nil)
	}
	if fromRow != "" {
		// The engine sends the row's own text, so what this client asks with is
		// read from the band it is looking at rather than from whatever the
		// caller remembered. It is not what the sent row is drawn from: another
		// client can edit the row between this read and the engine taking it,
		// and a turn started from it draws the text the engine answers it
		// started with (SubmitResult.Text, submitted).
		if row, ok := m.queuedRow(fromRow); ok {
			text = row.Text
		}
	}
	c := m.nextCmd()
	return m.run(gateDeadline,
		func(ctx context.Context, b backend.Backend) (any, error) {
			return b.Submit(ctx, c, text, mode, fromRow)
		},
		func(m Model, r gateReply) (Model, tea.Cmd) {
			res, _ := r.result.(engine.SubmitResult)
			m = m.submitted(text, mode, fromRow, c, res, r.err)
			return then(m, res, r.err)
		})
}

// submitted is submit's continuation: Submit's answer applied to the model, as
// the Update that sent it left it, before any event the Submit caused.
func (m Model) submitted(text string, mode engine.SubmitMode, fromRow string, c engine.Command, res engine.SubmitResult, err error) Model {
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
			// The optimistic row: this client's own send, drawn at Enter and
			// at its own clock, before any event about the turn exists. The
			// started that follows is its echo (applyTurnStarted). It is the
			// text the turn started with, as the engine answers it — for a
			// typed send the text sent, for a row the row's text as the engine
			// took it, which another client may have edited since this one
			// read the band (plan 027 §3.13, "Two-client correctness").
			m.addUser(res.Text)
			m.beginTurn(res.Turn)
			m.ownTurn = res.Turn
		}
		if fromRow == "" {
			// The composer's draft is matched against what this client sent.
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
	// What the frame shows of it comes from the result (§3.12's table), as an
	// overlay until the Submit's own event for its item is folded: the row it
	// queued present, the row it started a turn from gone, the send-now it
	// armed armed. A row Submit found still queued — it cannot start yet — is
	// a success that says nothing (engine.go's submit), and installs nothing;
	// so is a turn started from the composer's text, whose row Enter drew. A
	// refusal and an unanswered Submit install nothing: the band is the fold's.
	if err == nil {
		switch {
		case res.Armed:
			m.noteResult(resultEntry{kind: resultArmed, cause: c.Cause()})
		case res.Turn != "" && fromRow != "":
			m.noteResult(rowAbsent(c.Cause(), fromRow))
		case res.Queued != nil && fromRow == "":
			m.noteResult(resultEntry{kind: resultRowPresent, cause: c.Cause(), row: *res.Queued})
		}
	}
	return m
}

// beginTurn is what one turn starting does to the model's view of the session,
// whether the model learned of it from Submit's own answer or from the started
// event the engine published — a drained row, an armed send firing, another
// client's prompt. One place, so the two can never drift.
//
// The first prompt's index seed used to be here; it is the engine's now, which
// is what makes it happen for a turn no client started (plan 021 §3.8).
//
// The turn's user row is not drawn here, because the two draw it down different
// paths: Submit's answer draws the optimistic row, a local one at the client's
// clock (addUser), and a started is the shared model's user entry, at the
// event's At, which the fold has already given its row.
func (m *Model) beginTurn(id string) {
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
	// A turn this model has seen begin: the ending a read after a restore
	// brings back is no longer the last one (lastturn.go).
	m.turnStarts++
}

// maskCards drops the cards a cancel has answered and retires the plan offer:
// what cancelTurn does synchronously, and what an armed send-now owes as well,
// because the cancel it asked for is made by the engine and answers every
// request the session was holding just the same.
//
// The mask goes up for a cancel with no turn of craze's own too — cards with
// nothing working, or the agent's own turn (cancelForeignTurn) — keyed to the
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
// Nor was the attachment envelope in front of that (plan 033 §3.1), which the
// same split takes off.
//
// The draft's sidecar goes with it, its chip numbering restarting at 1 (owner
// decision 6): the images went with the text. A draft kept — changed since it
// was sent — keeps its chips, and whatever images they still stand for.
func (m *Model) clearMatchingDraft(text string) {
	_, text = agent.SplitShellContext(text)
	if strings.TrimSpace(m.input.Value()) != text {
		return
	}
	m.clearDraft()
}

// clearDraft empties the composer of a draft that has gone — sent, queued,
// interjected, or wiped by /clear: its text, its sidecar, the chip numbering
// restarting at 1 (plan 033 §3.3, owner decision 6), and the slash menu it
// opened (resetSlash).
func (m *Model) clearDraft() {
	m.input.SetValue("")
	m.images = draftImages{}
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
	case errors.Is(err, ErrNoAnswer):
		return noAnswerSubmitNote
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
	m.modeGen++
	m.modeInFlight = id
	gen := m.modeGen
	// The offer is spent, not retired: a SetMode that fails leaves the plan on
	// screen, and that path is allowed to put the offer back.
	m.planOfferSeq = 0
	seq, offer := m.turnSeq, m.offerGen
	eng, cmd, at, iss, base := m.eng, m.nextCmd(), m.modeRev, m.issue(), dispatchCtx(m.eng)
	// Optimistic, the way applyMode is: the chip flips now, by the request's
	// overlay, and reverts only if the agent refuses.
	m.requestMode(gen, cmd.Cause(), id)
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, modeCallTimeout)
		defer cancel()
		res, err := eng.Set(ctx, cmd, engine.Setting{Kind: engine.SettingMode, Value: id})
		if err != nil {
			return planImplementFailedMsg{issued: iss, seq: seq, gen: gen, offer: offer, prev: prev, err: err, at: at}
		}
		return planImplementMsg{issued: iss, seq: seq, gen: gen, offer: offer, mode: res.Value, rev: res.Rev}
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
	c, iss, base := m.nextCmd(), m.issue(), dispatchCtx(eng)
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, 2*time.Second)
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
		return cancelFailedMsg{issued: iss, err: err}
	}
}

// cancelFailedMsg says the cancel never reached the agent, and is the model's own
// report of it: it is returned only where the engine published nothing for the
// failure (engine.CancelResult.Reported).
type cancelFailedMsg struct {
	issued
	err error
}

// foreignTurnStoppable says Esc has a turn of the agent's own to stop (plan 026
// X44): the model's mirror says one is running — m.snap.ForeignTurn, the
// fold's last foreign-turn bracket (plan 027 X8, §3.13) — and no card
// holds the key. It is any provider's: the native session's wake, which
// delivers a background sub-agent's result and which nothing else could stop
// since it never sets working, and grok's interjection fallback alike. A flag
// the model has not heard of yet — the bracket still in the channel — is not
// read here: Esc then has nothing to stop, as before.
func (m Model) foreignTurnStoppable() bool {
	return m.snap.ForeignTurn && len(m.cards) == 0
}

// cancelForeignTurn is Esc on a turn the agent runs on its own, with nothing of
// craze's own working: the engine's cancel with no turn named, which it accepts
// for exactly that case (cancel.go's holdCancelLocked, with ForeignTurn()). An
// ErrNotAccepting answer means the turn ended between the key and the engine's
// look, and there was nothing to do; a stale-turn answer cannot come back for a
// cancel that names no turn. Success is reported as foreignCancelledMsg, which
// draws the note: a wake publishes no EventDone, so nothing else will.
//
// The cards are masked first, as cancelTurn masks them (plan 026 X48): the
// cancel answers every request the session holds, so an ask the agent's turn
// opened whose opening has not reached the model yet is one this cancel
// resolves, and without the mask it would flash a card up — taking the
// keyboard, and a host's blocked status with it — until its ending arrived.
//
// The message carries what the handler needs to settle it by what was
// cancelled rather than by what the display shows when it lands (X48): the
// engine's CancelResult.Turn, the foreign episode this Esc was pressed on, and
// the turn the model was on.
//
// Residual (SF-48): a cancel that names no turn lands on whatever is current,
// so a craze turn the owed drain claimed between the key and the engine's
// section is cancelled instead of the agent's. The engine says so — Turn names
// that craze turn — and its own ending draws the note.
func (m Model) cancelForeignTurn() (tea.Model, tea.Cmd) {
	m.maskCards()
	eng := m.eng
	if eng == nil {
		return m, nil
	}
	c, iss, base := m.nextCmd(), m.issue(), dispatchCtx(eng)
	episode, seq := m.foreignEpisode(), m.turnSeq
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, 2*time.Second)
		defer cancel()
		res, err := eng.Cancel(ctx, c, "")
		switch {
		case err == nil:
			return foreignCancelledMsg{issued: iss, turn: res.Turn, episode: episode, seq: seq}
		case errors.Is(err, engine.ErrNotAccepting), errors.Is(err, engine.ErrStaleTurn):
			return nil
		case res.Reported:
			return nil
		}
		return cancelFailedMsg{issued: iss, err: err}
	}
}

// foreignCancelledMsg says the engine accepted the cancel of a turn the agent
// ran on its own (cancelForeignTurn).
//
// turn is the engine's CancelResult.Turn: "" when the cancel was held with no
// turn of craze's own current, so what it stopped was the agent's turn; a
// craze turn's id when it landed on one instead (SF-48). episode is the
// model's foreignEpisode when Esc was pressed, and seq its turnSeq.
type foreignCancelledMsg struct {
	issued
	turn    string
	episode uint64
	seq     int
}

// foreignEpisode names the agent's turn the model's flag says is running: one
// more than the foreign-turn endings it has applied. It is counted from the
// endings rather than the openings, so the flag the mirror shows — the fold's,
// which moves with the brackets alone — names the episode whose opening it
// folded, and never the one before it, whose note may be drawn. It starts at
// 1, so 0 can mean "no note drawn".
//
// The limit is the model's own view: a turn that ended and a next one that
// started while the model's flag stayed up, both brackets still in the
// channel, are one episode to it — as they are on screen.
func (m Model) foreignEpisode() uint64 { return m.foreignEnded + 1 }

// applyForeignCancelled settles a foreign cancel by what the engine says it
// cancelled (plan 026 X48), never by the display's status when the answer
// lands — which can be a craze turn started after the cancel, or the idle a
// cancelled craze turn's ending left.
//
//   - A craze turn (turn != ""): that turn's own ending draws its note and
//     settles its state, as any cancelled craze turn's does. Nothing here.
//   - The agent's turn (turn == ""): nothing else will draw the note — a wake
//     publishes no EventDone — so this draws it, once per foreign episode:
//     Esc pressed again on the same turn, while the first cancel is in flight
//     or after it returned with the turn still running, is another accepted
//     cancel and no second note. The note is a row, and rows are written
//     whichever turn is current (applyTurnEnded), so a follow-up that started
//     meanwhile does not suppress it.
//
// And the cancelled state a host reads (m.cancelled) is set as a craze turn's
// cancel sets it — only for the turn and the foreign episode the model is on:
// a craze turn that began after this Esc owns it now, and its own ending says
// how it went; and a wake that started after the one this Esc stopped is
// another episode, which a late answer must not mark cancelled (CodeRabbit on
// #54: foreign turns do not advance turnSeq). The episode matches while it
// runs (foreignEpisode) or once it has ended (foreignEnded names it then).
func (m *Model) applyForeignCancelled(msg foreignCancelledMsg) {
	if msg.turn != "" {
		return
	}
	if msg.episode != m.foreignNoted {
		m.main.appendLocal(entry{kind: entryNote, text: stopCancelled}, m.stamp(m.now()))
		m.foreignNoted = msg.episode
	}
	sameEpisode := m.snap.ForeignTurn && msg.episode == m.foreignEpisode() ||
		!m.snap.ForeignTurn && msg.episode == m.foreignEnded
	if msg.seq == m.turnSeq && sameEpisode {
		m.cancelled = true
	}
}

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
	// /connect's key field does not outlive the quit, local or served: the
	// model it is in waits out the stop and is the program's last (plan 031
	// §3.9, dropConnect).
	m = m.dropConnect()
	if m.remote && m.eng != nil {
		return m.stopQuit()
	}
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

func (m *Model) applyEvent(ev agent.Event) tea.Cmd {
	// An event means the log is moving, which is what a hidden answer the outbox
	// had no room for is waiting on (retryHidden) — the fast path, ahead of the
	// beat that guarantees the retry (armHiddenRetry). It runs before the event
	// is applied, so a hidden ask answered here is not counted twice by an
	// answerHidden this same event causes. Its answers are commands of their
	// own (fire-and-forget, never gated, §3.12), handed back with whatever
	// this event's arm hands back: several in one Update are several commands.
	// The retry list is taken here, once, for every arm (reduceEvent): no
	// return of the event's can drop a retried answer.
	retry := m.retryHidden()
	cmd := m.reduceEvent(ev)
	if m.replaying {
		m.replayFolded++
		if m.replayFolded%replayPaintEvery == 0 {
			m.paintNow = true
		}
	}
	return tea.Batch(retry, cmd)
}

// replayPaintEvery is a replay's paint cadence: the drawn pane is painted on
// every replayPaintEvery-th event folded while replaying (paintDue).
const replayPaintEvery = 256

// paintDue says whether finish paints the drawn pane, when it is dirty, this
// Update (plan 032 §3.3 C6). Outside a replay it always does. While one runs,
// it does on every replayPaintEvery-th event folded and on a restore
// (paintNow), once the replay has failed — the start failed, or the stream
// ended — once craze is quitting, and whenever the viewport is not following
// the bottom of the pane; and not otherwise: a long resume draws in steps of
// 256 events rather than on every one, and which frames it draws depends on
// the events and the user's gestures alone, never on the clock. The replay's
// end is not replaying any more.
//
// A quit is the program's last frame on its way: Bubble Tea takes the quit
// without another Update and draws View once more, so whatever the cadence
// held back would be missing from it (r12 P2). The Update that sets quitting
// paints — Ctrl+D, /exit, a second Ctrl+C, the list's quit — and so does
// every event applied while the quit waits for its stop, its shell or its
// host, so a second quit's tea.Quit, which comes at once (stopQuit), finds
// the pane painted too. A forced refresh that paints for itself — a resize, a theme, Ctrl+O, a
// view switch — needs nothing from here, and neither does a user gesture that
// reads or moves the drawn rows — a press, a drag, a release, a double-click,
// Ctrl+Y, the wheel, the page keys and a sub-agent view's arrows: each paints
// first (catchUp), so it acts on the rows a paint on every event would have
// left, never on rows the cadence held back.
//
// The cadence holds only while the viewport follows the bottom, because only
// there does skipping a paint change nothing a later frame shows: a paint on
// every event would have kept it at the bottom of every intermediate set of
// rows, and the next paint puts it at the bottom of the latest. A viewport the
// user scrolled away from the bottom sits where it was left only as long as
// the rows under it never shrink to it: a paint on every event moves it to the
// bottom, and keeps it there, the moment one does (a tool row that loses its
// diff, a re-wrapped paragraph, the cap's trim), which a paint on the cadence
// alone would not see. So while the user is scrolled away the replay paints
// every event, as craze did before C6; the replay's cost is the cadence's
// again once the viewport is back at the bottom. Only the drawn pane's
// viewport counts: a background pane is painted when it is switched to.
//
// What it buys: the per-event cost of a replay no longer depends on how long
// the message being replayed is, whatever its shape; a single growing block,
// which no markdown checkpoint can split (mdCheckpoint), is rendered every
// 256 events rather than on every one. What it keeps: every frame drawn on a
// boundary or a forced refresh, and the one the replay's end draws, is the
// frame painting every event would have drawn there
// (TestTheReplayCadenceDrawsWhatPaintingEveryEventDraws).
func (m Model) paintDue() bool {
	following := m.vp.Height == 0 || m.vp.AtBottom()
	return !m.replaying || m.paintEveryEvent || m.paintNow || m.startErr != nil || m.ended || m.quitting || !following
}

// reduceEvent is applyEvent's event itself: the fold, then its arm, answering
// the arm's own command — a masked opening's gated read, a hidden ask's
// answer — or nil.
func (m *Model) reduceEvent(ev agent.Event) tea.Cmd {
	// The shared model folds every event, before anything below can return
	// early (plan 024 §3.8): every row an event draws is the fold's, and the
	// panes show what it changed. What the fold takes from this client's own
	// state is decided first, before that state moves:
	//
	//   - the echo rule: the started of the turn Submit handed back draws no
	//     row, because the optimistic row Enter drew is its display;
	//   - the todo notes: this pane's dedupe decides which note it shows (X31
	//     revised), over the list the event itself carries — the fold's rule is
	//     replacement, so that list is the whole of the new one, and an empty
	//     one clears it (plan 027 §3.13: no fallback to the mirror, which
	//     before this fold is the list this event replaces) — so a note the
	//     fold writes that the pane has already drawn gets no row, and one the
	//     pane owes that the fold does not write is the pane's own, written
	//     below;
	//   - an error's text, read once for the row and for m.err alike.
	var (
		hide     transcript.Kind
		todoNote string
		errText  string
	)
	switch {
	case m.ownStarted(ev):
		hide = transcript.KindUser
	case ev.Type == agent.EventTodos && ev.Agent == "":
		m.noteTodoLifecycle(ev.Todos)
		if todoNote = m.todoNoteOwed(ev.Todos); todoNote == "" {
			hide = transcript.KindNote
		}
	case errorEvent(ev):
		errText = ev.Err.Error()
	}
	ch := m.foldEvent(ev, hide, errText)
	// The mirror follows the fold at once, before any arm reads it (plan 027
	// §3.13): the event's own bookkeeping — revisions, the send-now's arm, the
	// overlays it retires — and then the mirror rebuilt from the fold.
	m.observe(ev)
	m.recompute()
	if ev.Type == agent.EventSubagent {
		m.applySubagentEvent(ev)
		return nil
	}
	if ev.Agent != "" {
		m.applyChildEvent(ev)
		return nil
	}
	// The spinner names what the turn is doing; only a thought chunk leaves it
	// on "Thinking…".
	m.lastThought = ev.Type == agent.EventThought
	switch ev.Type {
	case agent.EventUser:
		// An interjection's row and a replayed prompt's are the fold's. A live
		// main-session echo — grok and gx send one for every prompt the user
		// types — draws nothing anywhere: the row is the turn's started's
		// (§2.2).
		return nil
	case agent.EventReplay:
		if ev.Replay == nil {
			return nil
		}
		if ev.Replay.Phase != agent.ReplayEnd {
			// The model was built replaying when Config.Loading knew a load
			// was coming, and it had to be, since tea.Batch could deliver
			// startedMsg before this event ever arrived (§3.5) — so in process
			// this changes nothing. A client the load was not announced to —
			// one attached over a socket before its host started (plan 027
			// C27a) — learns of the replay here, and the session is not up
			// until its end: sends wait, and the tail runs once (sessionUp).
			if ev.Replay.Phase == agent.ReplayStart {
				// A replay's cadence counts its own events (paintDue): this
				// one is its first, whatever an earlier replay — of this
				// incarnation or of one before it — left the count at.
				m.replaying = true
				m.replayFolded = 0
			}
			return nil
		}
		// The restored snapshot is installed, so this is the moment the
		// session is up as far as the replay is concerned. The fold has closed
		// the last replayed run and written the restored note under it.
		m.replaying = false
		m.sessionUp()
		return nil
	case agent.EventCommand:
		// It arrives before the request reaches the wire, so the fold's line
		// lands under the user block and above anything the agent goes on to
		// say.
		return nil
	case agent.EventQueue:
		// The band draws from the mirror's queue, which the fold has already
		// moved; nothing else has to happen.
		if ev.QueueChange == agent.QueueRemoved && m.status == statusError {
			m.note("queue cleared")
		}
		return nil
	case agent.EventTurn:
		if ev.Turn == nil {
			return nil
		}
		switch ev.Turn.Phase {
		case agent.TurnStarted:
			m.applyTurnStarted(ev)
		case agent.TurnEnded:
			m.applyTurnEnded(ev.Turn)
		}
		return nil
	case agent.EventForeignTurn:
		// Either bracket ends the run above it, and the start heads what
		// follows with the note that stops the reply reading as an answer to
		// the last thing the user said: both the fold's.
		if ev.ForeignTurn == nil || !ev.ForeignTurn.Running {
			// The agent's turn is over, so the next one is another episode
			// (foreignEpisode). A nil payload closes the run, as the fold's.
			m.foreignEnded++
		} else {
			// A new turn of the agent's starts with nothing cancelled: its
			// ending reads as it went, not as an earlier Esc left the flag
			// (a late answer for a previous episode cannot set it either,
			// applyForeignCancelled).
			m.cancelled = false
			// And it is a turn begun, as beginTurn counts craze's own
			// (lastturn.go).
			m.turnStarts++
		}
		return nil
	case agent.EventText:
		if ev.Text != "" {
			// Only a chunk that says something is evidence: the fold drops an
			// empty one, and a turn whose whole reply was empty left no plan
			// on the screen to implement.
			m.sawAssistantSeq = m.turnSeq
		}
	case agent.EventTodos:
		// A todos fold appends nothing but its note, so no append is no note:
		// a note the pane owes that the fold did not write — which a /clear
		// makes possible — is the pane's own, stamped at the event's At.
		if todoNote != "" && ch.AppendedFrom.IsZero() {
			m.main.appendLocal(entry{kind: entryNote, text: todoNote}, m.stamp(ev.At))
		}
	case agent.EventPermission:
		if ev.Permission != nil {
			return m.pushCard(card{kind: cardPermission, perm: ev.Permission})
		}
	case agent.EventQuestion:
		// An auto-answered request (headless) is already decided; only an
		// interactive one is a card.
		if ev.Question != nil && !ev.Question.Auto {
			if m.showAsk() {
				return m.pushCard(card{kind: cardQuestion, ask: ev.Question})
			}
			// The config hides questions: it is skipped where it stands, with
			// no card and — as it always has — no row. Its ending finds no
			// card to remove, and the fold, given the same capabilities,
			// draws no note for it (transcript.Options.Hidden).
			return m.answerHidden(ev.Question.ID, agent.AskAnswer{Skip: true})
		}
	case agent.EventAsk:
		if ev.Ask != nil {
			m.applyAskEnded(ev.Ask)
		}
	case agent.EventPlan:
		if ev.Plan != nil && !ev.Plan.Auto {
			// The plan itself is transcript material, the fold's plan entry;
			// the card is only the three answers it needs.
			if m.showPlan() {
				return m.pushCard(card{kind: cardPlan, plan: ev.Plan})
			}
			return m.answerHidden(ev.Plan.ID, agent.AskAnswer{Reject: true})
		}
	case agent.EventDone:
		// The wire's own ending. It orders the transcript — the fold closes
		// the run above it, and a cancel leaves its note — which is why it,
		// and not the turn's ending, is what decides whether the turn left a
		// plan behind; but it no longer settles the status: the engine's
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
		//
		// The cancel mask is NOT cleared here. It belongs to a turn, and it is
		// the engine's ending for that turn that says every opening and ending
		// of it has been delivered — this event is the session's own and can
		// overtake an opening still in the outbox.
		//
		// The turn is over, so this is the one moment the branch can have
		// changed under craze. No polling, no resize hook.
		m.branch = m.git.branch()
		if ev.StopReason == stopCancelled {
			// Esc leaves nothing else behind: the spinner going away, and the
			// note, are the only signs the cancel landed, and the first is
			// indistinguishable from the turn having finished on its own.
			m.cancelled = true
		}
		if m.planEarnsOffer(ev.StopReason) {
			m.planOfferSeq = m.turnSeq
		}
		// A turn ended, so this session is the newest thing in the workspace —
		// which the engine's own observer records in the index (plan 021 §3.8).
	case agent.EventError:
		// The session's own failure, whose row the fold draws — the one place
		// it is drawn: the turn's ending follows and only settles the status,
		// which this has already said, as it always did, without waiting for
		// the other ending. Like EventDone it cannot arrive under a later turn;
		// see there.
		m.status = statusError
		m.confirm = nil
		if ev.Err != nil {
			// The text the fold drew the row with, read once.
			m.err = errText
		}
	case agent.EventMeta:
		if ev.State != nil {
			// A state delta: the engine's, or the session's own. The send-now
			// section is the whole of what S1b reads from one, and what it writes
			// for it is the toasts the model wrote for the same event before the
			// engine existed; the one error row a failed cancel has always left
			// is the fold's. A settings delta draws nothing at all (§3.8).
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
		// session_info_update fills ev.Text: the agent named the session, and
		// the engine's observer writes that to the index as an agent title
		// (plan 021 §3.8). The title the mirror shows is the fold's.
	}
	return nil
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
//
// The answer is fire-and-forget (plan 027 §3.12): a command of its own, never
// gated, because nothing it answers is drawn — no card exists for it — and
// nothing after it needs its result in the Update that sent it. The command's
// one message is the refusal for room (hiddenRefusedMsg), which puts the
// answer back on the retry list, stamped with the session generation and
// fenced by the backend epoch like any other result. Its ending finds no card
// to remove (applyAskEnded), and draws no note: the shared model is built
// with the asks the session's capabilities hide, as every folder of the
// session is, so the skip or reject sent here is never drawn as the user's
// (transcript.Options.Hidden, plan 032 C4).
func (m *Model) answerHidden(id string, a agent.AskAnswer) tea.Cmd {
	if m.eng == nil {
		return nil
	}
	b, c, iss, ctx := m.eng, m.nextCmd(), m.issue(), dispatchCtx(m.eng)
	return func() tea.Msg {
		if err := b.Answer(ctx, c, id, a); errors.Is(err, agent.ErrAskUnavailable) {
			return hiddenRefusedMsg{issued: iss, h: hiddenAnswer{id: id, a: a}}
		}
		return nil
	}
}

// hiddenRefusedMsg is a hidden answer the outbox had no room for
// (answerHidden): it goes back on the retry list.
type hiddenRefusedMsg struct {
	issued
	h hiddenAnswer
}

// retryHidden re-sends the hidden answers the outbox had no room for, one
// command each. Each one is either taken, refused for good — the ask was
// resolved some other way in the meantime, agent.ErrAlreadyResolved among them
// — or refused for room again, and put back by its own message. The list is
// taken first, so an answer is never in flight twice.
func (m *Model) retryHidden() tea.Cmd {
	if len(m.hiddenRetry) == 0 {
		return nil
	}
	pending := m.hiddenRetry
	m.hiddenRetry = nil
	cmds := make([]tea.Cmd, 0, len(pending))
	for _, h := range pending {
		cmds = append(cmds, m.answerHidden(h.id, h.a))
	}
	return tea.Batch(cmds...)
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
// Update arms the next one only if something was refused for room again — its
// refusal back on the list (hiddenRefusedMsg) — so a list that empties, taken
// or ended by somebody else, stops the timer.
func (m *Model) handleHiddenRetry() tea.Cmd {
	m.hiddenRetryLive = false
	return m.retryHidden()
}

// applyAskEnded is one ask's ending. Every way an ask can end carries one now,
// so this is where a card goes away — another client's answer, a cancel, the
// turn it belonged to ending underneath it, the session closing — whichever
// card the queue still holds for it: none for this client's own answer, whose
// card it popped before it sent the answer.
//
// It writes no row. The outcome note an answer earns — the question notes, the
// skipped note, the plan's verb — is the shared transcript's, which the fold
// has already drawn from this event, for this client's own answer and
// another's alike, and is there again after a restore or for a client that
// attaches later (plan 032 §3.2 C4, SF-61). The fold's eligibility is the rule
// this function used to apply: nothing for a permission, a cancel, a turn's
// end, a close, an automatic resolution, a hidden ask, or an ending whose
// opening it never saw.
func (m *Model) applyAskEnded(u *agent.AskUpdate) {
	m.notePlanApproved(u)
	m.removeCard(u.ID)
}

// notePlanApproved records the turn an accepted plan belongs to, so a turn that
// left a plan behind and said nothing in words still earns the implement offer
// (planEarnsOffer, plan 023 correction 2). It is the first thing applyAskEnded
// does, ahead of anything about a card, because the two routes to an accepted
// plan — this model answering the card itself, whose ending finds no card,
// and an accept from anywhere else — must both land here.
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
// The row half of that rule is the pane's to decide: the shared model folds
// every started into the user entry it is, and applyEvent folds the skipped one
// with hide set, so the pane gives that entry no row — the optimistic row Enter
// drew is the display (plan 024 §3.8, X26). The rest of the skip is the
// model's, and is here.
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
	id := ev.Turn.ID
	if m.ownStarted(ev) {
		m.ownTurn = ""
		return
	}
	if id != "" && id == m.nextTurn {
		// The pending successor, arriving in its place. Nothing was applied
		// for it, so it is drawn like any other started; what the marker did
		// was keep the model working until this moment.
		m.nextTurn = ""
	}
	m.beginTurn(id)
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
// rows are the ending event's, and the shared model's fold draws them.
func (m *Model) applyTurnEnded(t *agent.TurnInfo) {
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
	//
	// Cancelled before the prompt's turn opened — while it was still waiting for
	// the agent's first command catalog, or right after Enter — nothing ran and
	// nothing failed, so this is not an error state: it is the ending a
	// cancelled turn has, and the fold writes the note the transcript owes it
	// (Esc leaves nothing else behind). A prompt the session refused emits no
	// event of any kind, so its synthetic ending is the only place its row can
	// be drawn, and the fold draws it there.
	cancelled := t.Synthetic && !failed && t.StopReason == stopCancelled
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
// written the same news: the toast for each way an armed send-now can be lost.
// The error row beside it — for a reason that is a failure, in the order
// cancelFailedMsg produced the two — is the shared model's fold's, drawn before
// this runs. The send-now section is the whole of what S1b reads; no settings
// delta draws anything at all.
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
// it. Whether a send is armed *now* is the mirror's (sendNowPending) — and which
// arm a draft belongs to is armedDraft's, which this deliberately leaves alone.
// The settings sections draw nothing here: the mirror has already taken them
// from the fold, and their revisions (observe).
func (m *Model) applyStateDelta(ev agent.Event) {
	st := ev.State
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
				// This model's own Disarm, whose note it wrote when the Disarm
				// answered. Anything else is somebody else taking it back.
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
	// The rows are the fold's. Detail is the failure behind the reason, which
	// the engine fills only for a cancel it made itself — a cancel this model
	// asked for answers it directly, and drawing the row from both would draw
	// one failure twice. IndexErr is a session-index write that failed: the
	// same row writeIndex drew itself before the index moved into the engine,
	// drawn for this model's OWN commands too — a seed is reported after Submit
	// has already answered, so there is no return value it could have come back
	// on — and for the writes no command caused at all (the agent's title, a
	// turn's end), which is exactly the set the model used to write rows for.
}

// toggleExpanded is the global Ctrl+O detail toggle; every entry redraws
// because the render key changed.
func (m Model) toggleExpanded() (tea.Model, tea.Cmd) {
	stick := m.vp.Height == 0 || m.vp.AtBottom()
	m.expanded = !m.expanded
	m.setViewportContent(stick)
	return m, nil
}

// modeSettled is SetMode coming back accepted, for request gen, confirming
// value at revision rev. Only the current generation takes the in-flight flag
// down — an older
// answer arriving late says nothing about the request the chip is showing —
// and the chip is not taken back to the fold on success: the request's
// overlay holds the confirmed mode until the change's own delta is folded
// (confirmMode, plan 027 §3.13), so the chip never snaps back to the mode the
// user just left while that delta is on its way. An agent-initiated mode that
// lands after it is the fold's, and shows once the overlay is retired.
func (m Model) modeSettled(gen int, value string, rev uint64) Model {
	if gen == m.modeGen {
		m.modeInFlight = ""
	}
	m.confirmMode(gen, value, rev)
	return m
}

// comeUp is the session coming up in this TUI once its backend's start has
// answered: startedMsg's arm, after the engine is told — and, over a socket, a
// start whose failure was an incarnation's the model has since left, which
// leaves the session it holds as a start that answered leaves it (startLeft,
// plan 030 C7r2). The launch flow's backend hears that its session came up
// here, and not while the program is on its way out (startAcker); the
// provider is persisted where this craze persists it; and the session-is-up
// tail runs, once its other key has landed too (sessionUp).
func (m *Model) comeUp() {
	if a, ok := m.eng.(startAcker); ok && !m.quitting {
		a.AckStarted()
	}
	m.started = true
	m.branch = m.git.branch()
	m.recompute()
	if m.persistProvider && (!m.fallbackDefault || m.pickedExplicit) {
		name := m.snap.Provider.Name
		if name == "" {
			name = agent.CursorProvider().Name()
		}
		// A hidden provider is never written as the default (plan 018
		// §3.4): trying it once must not change what a plain craze starts.
		// A session that has not reported its provider yet was started as
		// the resolved default (as in writeIndex), so a hidden default is
		// skipped too rather than saving the cursor fallback.
		hidden := hiddenProvider(name) || (m.snap.Provider.Name == "" && m.providerDefault.Hidden())
		if !hidden {
			if err := SaveProvider(name); err != nil {
				m.addError(err.Error())
			}
		}
	}
	m.sessionUp()
}

// startLeft reports that the backend's start answered for an incarnation the
// model has left (startInc, plan 030 C7r2): the stream's Ready was applied in
// one, and a restore of another has been applied since. Over a socket the
// start's answer and the stream are read side by side, so the answer can land
// after that restore; in process no Ready comes, and it is never so.
func (m *Model) startLeft() bool {
	return m.startInc != "" && m.startInc != m.incarnation()
}

// incarnation is the incarnation of the session the model holds, as its fold
// knows it: the last restore's — "" before a socket backend's first, and in
// process, where none comes.
func (m *Model) incarnation() string {
	if m.shared == nil {
		return ""
	}
	return m.shared.Incarnation()
}

// sessionReady is the two-key gate of §3.5: Start has returned *and*, for a
// loaded session, its replay has ended and the restored snapshot is installed.
// tea.Batch orders neither of them, so both are latched and whichever lands
// second opens the gate. Sends and /rename wait for it; a new session never
// sets replaying, so it means exactly what m.started alone used to.
func (m Model) sessionReady() bool { return m.started && !m.replaying }

// sessionUp is the tail startedMsg used to run alone: the status goes idle —
// unless the session already said otherwise — the elapsed counter starts and
// the skills are rescanned. It is called from both keys and does nothing until
// both have landed, and it runs once (upDone): however they are ordered, and
// when a replay a socket's stream is still draining closes the gate again after
// the session came up (the replay-start arm).
//
// A loaded session's index row used to be touched here. It is the engine's
// now, keyed to the one event that says a load is over — EventReplay{end},
// which only a load produces (plan 021 §3.8).
func (m *Model) sessionUp() {
	if !m.sessionReady() || m.upDone {
		return
	}
	m.upDone = true
	// What the session said before it came up stands: its coming up ends
	// neither a turn running nor a failure. A restore that says a turn is
	// running set the working status and the turn it names (restore.go). A
	// failure set the error state — the last ending the read after a restore
	// applied (lastturn.go), or a turn's failure the stream delivered: Init
	// runs the start and the stream's reader side by side, so over a socket
	// either can land before Start's answer does (plan 030 §3.7; sol r10-c6
	// 1). In process no turn can be running or have failed before the
	// session is up — the engine admits nothing before its Start has
	// returned, and a start that failed never comes up (errMsg) — so this is
	// idle there, as it always was.
	if m.status != statusError && (m.status != statusWorking || m.turnID == "") {
		m.status = statusIdle
	}
	// A frame run counts from its runner's own start (frameStart), the
	// instant its socket host serves as its StartedAt, so the elapsed a
	// frame shows is the same by transport.
	m.sessStart = m.now()
	if !m.frameStart.IsZero() {
		m.sessStart = m.frameStart
	}
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

// titleRuneCap is how long a session title may be in the index, which /rename
// caps a title at before it asks: engine.IndexTitleRunes, the cap every host
// writes a row's title through (engine.IndexTitleLine), and a test holds the two
// rules together.
const titleRuneCap = engine.IndexTitleRunes

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

// View places the regions the layout decided, each forced to exactly its own
// row count, so the frame is always exactly as tall as the terminal.
func (m Model) View() string {
	if !m.ready || m.width <= 0 || m.height <= 0 {
		// A degenerate size still owes the terminal exactly its own rows.
		return blankFrame(m.width, m.height)
	}
	if m.sessList.open {
		// The session list's own region set in place of the session's
		// frame (sessions_list.go): the session is behind it, not under it.
		return m.sessionsView()
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

// waitEvent reads the backend's stream, one item: in process the engine's
// primary — the session's own, unchanged: the engine publishes into the same
// log, so one stream carries the agent's events and the engine's alike. A
// primary client keeps reading until it closes the engine, because the log's
// outbox may still be publishing after a turn's ending.
//
// Every item is delivered, each as its message: an event as an eventMsg with
// its stream generation; and the four a socket's stream adds (PR 4; nothing
// in process delivers them) — a Restore as a restoreMsg, a Ready as a readyMsg,
// an End as an endMsg (restore.go) and a Presence as a presenceMsg (plan 032
// §3.14, presence.go). After each the command gate decides
// whether the next read starts (readOn): exactly one is ever in flight, and
// each is held and drained in arrival order like any other message. A stream
// that has ended (backend.ErrClosed, after its End) or failed is nil, as a
// closed channel always was: nothing more is coming, and the reader is not
// re-armed.
//
// The read's context never ends. Read returns every item it takes, a context
// cancelled meanwhile or not (backend.Backend.Read), and a read that is never
// cancelled is never abandoned either: nothing this reader takes is dropped.
//
// Every message is stamped with bgen, the backend generation the read was
// armed under (Model.bgen; plan 030 §3.11): a read a switch left in flight on
// the backend it replaced ends when that backend's close ends its stream —
// nothing, as any ended stream — or with an item it had already taken, whose
// message the command gate then drops as another backend's (staleBackend).
func waitEvent(b backend.Backend, bgen uint64) tea.Cmd {
	if b == nil {
		return nil
	}
	return func() tea.Msg {
		for {
			it, err := b.Read(context.Background())
			if err != nil {
				return nil
			}
			switch it.Kind {
			case backend.ItemEvent:
				return eventMsg{ev: it.Event, gen: it.Gen, bgen: bgen}
			case backend.ItemRestore:
				return restoreMsg{info: it.Info, snap: it.Snapshot, gen: it.Gen, bgen: bgen}
			case backend.ItemReady:
				return readyMsg{info: it.Info, err: it.Err, bgen: bgen}
			case backend.ItemEnd:
				return endMsg{err: it.Err, bgen: bgen}
			case backend.ItemPresence:
				return presenceMsg{n: it.Attached, gen: it.Gen, bgen: bgen}
			}
			// A kind this build does not know carries nothing to apply: it
			// is read past, as the stream's own unknown items are.
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
