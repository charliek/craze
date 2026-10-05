package tui

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/sessions"
)

// The session list (plan 030 §3.10): every session of this user on this
// machine, one row each, grouped by what it needs — the screen `←` on an
// empty composer, `←` over a card (SF-99) and /sessions open, only when
// Config.Sessions is set.
//
// It is a top-level mode of the model, not a dialog and not a region of the
// session's frame: while it is open (sessList.open) it is routed first —
// handleKey hands it every key ahead of Ctrl+C and Ctrl+D, a card, a dialog,
// the confirm line and the sub-agent view; update drops the mouse — and View
// draws its own region set in place of the session's frame (sessionsView:
// the header, the list, the footer rule and the hint line). The session
// behind it stays attached: its stream is read and folded as ever, a card it
// raises is drawn when the list is left and takes no key meanwhile, and its
// end does not quit craze while the list is up — its row is marked ended,
// and `esc`/`←` stay on the list and say so. The loss of its connection is
// not its end: its row is left to the roster, and opens it again.
//
// The rows are the roster's (internal/roster, through Config.Sessions): a
// poller opened with the list and closed with it, whose latest Snapshot
// arrives as a sessSnapMsg. Every row is held by identity — craze id and
// incarnation (sessKey) — never by index: the selection and an armed close
// survive the rows reordering around them.

const (
	// sessMinCols and sessMinRows are the smallest terminal the list draws
	// in (plan 030 §3.10); below either it is the too-small message.
	sessMinCols = 40
	sessMinRows = 10
	// sessCloseWindow is how long a close armed by ctrl+x waits for the
	// second press that sends it (§3.10: "within 2 s").
	sessCloseWindow = 2 * time.Second
	// sessHeaderRows is the header region: the title row and a spacer.
	// sessFooterRows is the footer region: the rule and the hint line.
	sessHeaderRows = 2
	sessFooterRows = 2
)

// The list's words.
const (
	sessEmptyNote   = "No other sessions."
	sessEndedNote   = "that session ended"
	sessLostNote    = "lost the connection to that session"
	sessUntitled    = "new session"
	sessUnreachNote = "that session is not answering"
	sessOlderNote   = "that session runs in an older craze; close it there"
)

// sessListState is the list: whether it is open and everything it shows.
// It is a value on the model like every other piece of its state; the
// roster it holds is the one thing shared, and the list alone closes it.
type sessListState struct {
	open bool
	// gen stamps each opening: every message of the list — a snapshot, a
	// ctrl+x's answer, a timer — carries the opening it was issued under,
	// and one from an earlier opening is dropped. It only moves forward,
	// across openings.
	gen uint64
	// roster is the poller this opening started (Sessions.Roster), closed
	// when the list closes or craze quits from it.
	roster SessionRoster
	// snap is the latest snapshot, have that one has arrived: until it has,
	// the list draws no rows and claims no count.
	snap roster.Snapshot
	have bool
	// byDir is ctrl+s's grouping by directory. It is the TUI's, not the
	// opening's: it survives closing and reopening the list (§3.11 keeps
	// the list's grouping per TUI).
	byDir bool
	// savedOpen says the saved group is expanded (enter on its line).
	savedOpen bool
	// here is the session the list was opened from — the one the model
	// holds — by craze id and incarnation, never the id alone (sol r19-c10
	// 2): a later incarnation of the same craze id is another session's row.
	// It is taken as the list opens and again with each snapshot while that
	// session runs (so one its host names after the list opened is found),
	// and kept as it was once it has ended. hereEnded says it ended while the
	// list was up, and hereEndedAt when — the ended row's age. hereRow is its
	// row as the roster last listed it (or, never listed, as the model knows
	// it): once the session has ended and its host has left the registry,
	// that copy is its row, drawn ended until the list closes (sol r19-c10 1).
	here        sessKey
	hereEnded   bool
	hereEndedAt time.Time
	hereRow     *roster.Row
	// hereLost says the backend the list was opened from is gone but not
	// its session: its stream ended for the transport — re-attaches spent, a
	// hole, a refused re-attach — not for the session's own end on its host
	// (endMsg's nil), so the session may well run on (C12r2, r27-pr2 1). The
	// list has nothing behind it to go back to, and here is cleared: the
	// session's row is the roster's like any other — live while its host
	// answers, unreachable while it does not, gone with its registry entry —
	// and enter opens it afresh (Sessions.Open), ctrl+x stops or closes it.
	// Only the session's own end is drawn `· ended` (hereEnded).
	hereLost bool
	// none says nothing at all is behind the list: it was opened over an
	// unstarted session (plan 030 §3.13), which the opening discarded — `←`
	// before a prompt leaves nothing — so the model holds no session, and
	// `esc`/`←` stay on the list and say so. The cursor starts on the row that
	// session was opened from.
	none bool
	// home is $HOME when the list opened: directory headers abbreviate it.
	home string
	// sel is the selected line, by identity; selIdx its place among the
	// selectable lines when it was last found — where a row that disappears
	// leaves the selection (its neighbour takes that place).
	sel    sessKey
	selIdx int
	// armed is the row a first ctrl+x armed a close on, armedAt when, and
	// armSeq the arming — the one its expiry timer may disarm.
	armed   sessKey
	armedAt time.Time
	armSeq  uint64
	// note is the hint line's one-off message — what a ctrl+x came to, the
	// session behind having ended — until the next key.
	note     string
	noteKind sessNoteKind
	// dialSeq stamps each open this opening of the list asks for (enter on a
	// running row, plan 030 §3.11, or on a saved one, §3.12), and dialing is
	// the one it waits for (0 for none), dialTitle that row's title for the
	// hint line and dialSaved whether it is a saved session's resume. An
	// answer for any other — an earlier open the user replaced with a later
	// one, an open from an opening of the list since left (gen) — is closed,
	// never adopted (sessOpened).
	dialSeq   uint64
	dialing   uint64
	dialTitle string
	dialSaved bool
	// dispatchSeq stamps each background dispatch this opening of the list
	// starts (dispatch.go); the input waits for the latest (in.dispatching).
	dispatchSeq uint64
	// in is the input under the rows (sessions_input.go, plan 030 §3.13):
	// only when Config.Sessions can start sessions (SessionStarter); in.on
	// false is PR 2's list, which has none.
	in sessInput
	// avail is the latest ProviderAvailabilitySource answer this opening of
	// the list took (plan 036 §3.3): what /provider offers and refuses, and
	// a typed `/provider <id>` is checked against. availN is its number
	// (provAvailSeq), so an answer from an older call — the list's own read
	// landing after /provider's — never replaces a newer one; availHave says
	// there is one: before it, /provider offers the startup picker's rows,
	// every one ready.
	avail     []ProviderAvail
	availN    uint64
	availHave bool
}

// sessAvailAnswer is one ProviderAvailabilitySource answer, numbered as its
// call started (provAvailSeq).
type sessAvailAnswer struct {
	n     uint64
	avail []ProviderAvail
}

// sessAvailMsg is the list's own read of the providers' availability as it
// opens, for its opening gen.
type sessAvailMsg struct {
	gen    uint64
	answer sessAvailAnswer
}

// askSessAvail is one ProviderAvailabilitySource call, numbered as it starts:
// off the Update, since it reads the disk.
func askSessAvail(src ProviderAvailabilitySource) sessAvailAnswer {
	n := provAvailSeq.Add(1)
	return sessAvailAnswer{n: n, avail: src.ProviderAvailability()}
}

// readSessAvail reads the providers' availability off the Update, for the
// list's opening gen: what a typed `/provider <id>` is checked against before
// /provider's values have ever been opened.
func readSessAvail(src ProviderAvailabilitySource, gen uint64) tea.Cmd {
	return func() tea.Msg { return sessAvailMsg{gen: gen, answer: askSessAvail(src)} }
}

// takeSessAvail keeps ans as the list's latest availability, unless a newer
// call's answer is already kept.
func (l *sessListState) takeSessAvail(ans sessAvailAnswer) {
	if l.availHave && ans.n <= l.availN {
		return
	}
	l.avail, l.availN, l.availHave = ans.avail, ans.n, true
}

// sessKey is a line's identity: a running session by its craze id and
// incarnation, a host that has published no session yet by its host id, a
// saved session by its index identity, and the saved group's own line.
type sessKey struct{ id, inc string }

// sessSavedLine is the saved group's line: "▸ saved · N not running".
var sessSavedLine = sessKey{id: "\x00saved"}

func (k sessKey) zero() bool { return k == sessKey{} }

// sessNoteKind colours the hint line's note.
type sessNoteKind int

const (
	sessNoteWarn sessNoteKind = iota
	sessNoteErr
	// sessNoteOK: something asked for happened (a session started).
	sessNoteOK
)

// sessNoteStyle is a note's colour: the hint line's, and the startup
// picker's detail lines' for what the pre-session connect dialog came back
// with (plan 036 §3.6, connectBack).
func (m Model) sessNoteStyle(k sessNoteKind) lipgloss.Style {
	switch k {
	case sessNoteErr:
		return styleFG(m.theme.Err)
	case sessNoteOK:
		return styleFG(m.theme.OK)
	}
	return styleFG(m.theme.Warn)
}

// sessState is a row's state (plan 030 §3.10's table), in the order the
// groups are drawn.
type sessState int

const (
	sessNeedsYou sessState = iota
	sessWorking
	sessFailed
	sessIdle
	// sessUnreachable: the registry lists the host and its socket does not
	// answer — never shown as saved (§3.9).
	sessUnreachable
	sessSaved
)

// sessRow is one row as the list draws it.
type sessRow struct {
	key   sessKey
	state sessState
	// connecting is a host the roster has no answer from yet (X68): drawn
	// with the working rows, Starting… or Connecting….
	connecting bool
	// ended is the session behind the list, ended while the list was up.
	ended bool
	saved bool
	// here is the session the list was opened from.
	here bool
	// title, want (what it wants — §3.10's fourth column) and note (an
	// older host's craze version) are one line each, sanitised.
	title, want, note string
	// indexTitle is the session index's title for the row's session (a
	// running row's roster.Row.IndexTitle, a saved row's own), sanitised:
	// what the band names the session by once it is opened, while the
	// session names none of its own (sessTitle).
	indexTitle string
	provider   string
	workspace  string
	// since is when the row entered its state (a saved row: its last use).
	since time.Time
	ref   roster.Ref
}

// closable says ctrl+x arms a close on the row: an idle or failed session
// whose host answers.
func (r sessRow) closable() bool {
	return !r.saved && !r.connecting && !r.ended && (r.state == sessIdle || r.state == sessFailed)
}

// cancellable says ctrl+x stops the row's turn: a working or asking session
// whose host answers and has published its session.
func (r sessRow) cancellable() bool {
	return !r.saved && !r.connecting && !r.ended && (r.state == sessWorking || r.state == sessNeedsYou)
}

// ---------------------------------------------------------------- messages

// sessSnapMsg is the roster's latest snapshot for the list's opening gen;
// closed says its slot has closed (the roster stopped) and nothing more
// will come.
type sessSnapMsg struct {
	gen    uint64
	snap   roster.Snapshot
	closed bool
}

// sessActKind is what a ctrl+x sent.
type sessActKind int

const (
	sessActCancel sessActKind = iota + 1
	sessActClose
)

// sessActionMsg is what a ctrl+x's call came to (Sessions.Cancel or Stop).
type sessActionMsg struct {
	gen   uint64
	kind  sessActKind
	title string
	err   error
}

// sessDisarmMsg is an armed close's window running out: it disarms the
// arming seq, if that is still the one armed.
type sessDisarmMsg struct {
	gen, seq uint64
}

// sessRedrawMsg asks for a frame and nothing else: the hint line of a
// Ctrl+C window that has closed.
type sessRedrawMsg struct{ gen uint64 }

// sessOpenedMsg is an open's answer (Sessions.Open): the backend of the
// session the row names, or why there is none. gen and seq are the opening
// of the list and the open it was asked for under (sessListState.dialSeq);
// title is the row's, for the note a failure leaves, and indexTitle the
// index's title the row had, which the band names the session by while it
// names none of its own (sessTitle); saved says the row was a saved
// session's, whose backend loads it (plan 030 §3.12).
type sessOpenedMsg struct {
	gen, seq   uint64
	title      string
	indexTitle string
	saved      bool
	b          backend.Backend
	err        error
}

// ------------------------------------------------------------ open & close

// openSessions opens the list (plan 030 §3.10): the roster starts polling,
// and the cursor starts on the session the list was opened from. Nothing
// without Config.Sessions — the caller has checked, and this checks again.
func (m Model) openSessions() (tea.Model, tea.Cmd) {
	if m.sessions == nil || m.sessList.open {
		return m, nil
	}
	r := m.sessions.Roster()
	if r == nil {
		return m, nil
	}
	// An unstarted session the list is opened over is discarded (plan 030
	// §3.13): nothing is behind the list, and the cursor starts where that
	// session was opened from.
	none, from := false, sessKey{}
	if u := m.unstarted; u != nil {
		none, from = true, u.from
		m = m.discardUnstarted()
	}
	m.sessRosters.add(r)
	// From here on every session's frame carries the band — which session it
	// is and the way back to the list (band.go, plan 030 §3.11).
	m.bandOn = true
	here := m.hereKey()
	home, _ := os.UserHomeDir()
	m.sessList = sessListState{
		open:   true,
		gen:    m.sessList.gen + 1,
		roster: r,
		byDir:  m.sessList.byDir,
		here:   here,
		home:   home,
		sel:    here,
		none:   none,
	}
	if none {
		m.sessList.sel = from
	}
	m.ctrlCDeadline = time.Time{}
	read := readSessSnap(r, m.sessList.gen)
	st, ok := m.sessions.(SessionStarter)
	if !ok {
		return m, read
	}
	// The input under the rows, and the recent directories its `@` offers
	// (plan 030 §3.13, §3.15), read once for this opening — and the
	// providers' availability its /provider offers and checks (plan 036
	// §3.3), read again at each opening of /provider's values.
	m.sessList.in = newSessInput(m.completeLoads)
	sync := m.syncSessInput()
	var avail tea.Cmd
	if src, ok := m.sessions.(ProviderAvailabilitySource); ok {
		avail = readSessAvail(src, m.sessList.gen)
	}
	return m, tea.Batch(read, readRecents(st, m.sessList.gen), avail, sync)
}

// hereKey is the session the model holds, as its row is keyed: zero with
// none, or before its host has named it.
func (m Model) hereKey() sessKey {
	if m.eng == nil {
		return sessKey{}
	}
	info := m.info()
	if info.CrazeSessionID == "" {
		return sessKey{}
	}
	return sessKey{id: info.CrazeSessionID, inc: info.Incarnation}
}

// leaveSessions is esc and ← (and enter on the session's own row): back to
// the session behind the list, whose roster is closed on the way. A session
// that ended while the list was up has nothing to go back to, and nor has
// one whose connection was lost (hereLost — its row, if its host still
// lists it, opens it afresh): the list stays, and says so. An open still
// dialling is abandoned: its answer is closed when it lands (sessOpened).
func (m Model) leaveSessions() (tea.Model, tea.Cmd) {
	switch {
	case m.sessList.hereEnded:
		m.sessNote(sessEndedNote, sessNoteWarn)
		return m, nil
	case m.sessList.hereLost:
		m.sessNote(sessLostNote, sessNoteWarn)
		return m, nil
	case m.sessList.none:
		m.sessNote(sessNothingNote, sessNoteWarn)
		return m, nil
	}
	return m.closeSessList()
}

// closeSessList takes the list down, as a leave does and as the pre-session
// connect dialog does opening over it (connectOverSessions): its state goes
// but for its opening's number and its grouping, a pending Ctrl+C with it,
// and its roster is closed by the command it answers.
func (m Model) closeSessList() (Model, tea.Cmd) {
	r := m.sessList.roster
	// The input's popup is dropped with the list: a load it is waiting for
	// is cancelled, and its answer would find nothing to take it.
	m.sessList.in.closePopups()
	m.sessList = sessListState{gen: m.sessList.gen, byDir: m.sessList.byDir}
	m.ctrlCDeadline = time.Time{}
	return m, m.sessRosters.closeCmd(r)
}

// sessBackState is the list as the pre-session connect dialog found it (plan
// 036 §3.6, decision 11): its input — the line, its cursor and the leading
// token's binding — its selection and the saved group's state, what it knew
// of the session behind it (none, here and its ending), and its last
// listing, so the list that opens again looks as it did until its new roster
// answers. Its providers' states are not kept: the opening reads them afresh,
// as any opening does, so native's is recomputed from the store, never
// assumed.
type sessBackState struct {
	value          string
	cursor         int
	bound, boundTo string
	sel            sessKey
	selIdx         int
	savedOpen      bool
	none           bool
	here           sessKey
	hereEnded      bool
	hereEndedAt    time.Time
	hereRow        *roster.Row
	hereLost       bool
	snap           roster.Snapshot
	have           bool
}

// connectOverSessions is native needing setup chosen in the list's
// /provider — Enter or Tab on it, or a typed `/provider native` (plan 036
// §3.6, decision 11): the list closes, as a leave closes it (closeSessList)
// — its popups, its roster — with what it showed recorded on the dialog
// (connectDialog.list) but none of a leave's refusals, since nothing is gone
// back to; and the connect dialog opens in the modal layer, which already
// routes keys, the mouse, pastes and masking to it, over whatever is behind
// the list. Every way out of the dialog opens the list again as it was
// (reopenSessions); what new sessions run is the TUI's (sessPick) and is not
// touched.
func (m Model) connectOverSessions() (Model, tea.Cmd) {
	l := m.sessList
	v, cur := l.in.ti.Value(), sessByteCursor(l.in.ti.Value(), l.in.ti.Position())
	list := &sessBackState{
		value: v, cursor: cur, bound: l.in.bound, boundTo: l.in.boundTok,
		sel: l.sel, selIdx: l.selIdx, savedOpen: l.savedOpen,
		none: l.none, here: l.here, hereEnded: l.hereEnded, hereEndedAt: l.hereEndedAt, hereRow: l.hereRow, hereLost: l.hereLost,
		snap: l.snap, have: l.have,
	}
	m, closeRoster := m.closeSessList()
	next, cmd := m.openPreConnect(returnList)
	next.cdlg.list = list
	return next, tea.Batch(closeRoster, cmd)
}

// reopenSessions is the pre-session connect dialog's way back to the list
// (connectReturned): the list opens again (openSessions) — a roster of its
// own, its providers' states and its recent directories read afresh — and
// then gets back what it showed when the dialog closed it (list): its input,
// put back as it was, which opens /provider's values again over a
// `/provider ` line, with native's state read anew; its selection; the
// session behind it, as it knew it. back is said on its hint line. The
// commands the opening returned are the Update wrapper's to hand on
// (backCmd). With no roster to open the list with, the model stays where the
// dialog left it.
func (m Model) reopenSessions(back connectBack, list *sessBackState) Model {
	tm, open := m.openSessions()
	m = tm.(Model)
	if !m.sessList.open {
		return m
	}
	l := &m.sessList
	if b := list; b != nil {
		l.sel, l.selIdx, l.savedOpen = b.sel, b.selIdx, b.savedOpen
		l.none, l.here, l.hereEnded, l.hereEndedAt, l.hereRow, l.hereLost = b.none, b.here, b.hereEnded, b.hereEndedAt, b.hereRow, b.hereLost
		l.snap, l.have = b.snap, b.have
		if l.in.on {
			l.in.set(b.value, b.cursor)
			l.in.bound, l.in.boundTok = b.bound, b.boundTo
		}
	}
	m.syncSessList()
	sync := m.syncSessInput()
	if back.text != "" {
		m.sessNote(back.text, back.kind)
	}
	m.backCmd = tea.Batch(m.backCmd, open, sync)
	return m
}

// sessListUp says the session list is over the session the model holds: open,
// or closed for the pre-session connect dialog standing in for it (plan 036
// §3.6, decision 11), whose every way out opens it again. What the list being
// up decides about the session behind it — an unstarted session's first
// prompt dropped when the session it spawned fails to come up or ends there
// (X154), a composer paste landing in the draft the list covers — the dialog
// standing in for it decides the same way. Routing (keys, the mouse, the
// frame) is not this: the dialog has the modal layer's.
func (m Model) sessListUp() bool {
	return m.sessList.open || (m.preConnectOpen() && m.cdlg.returnTo == returnList)
}

// endedToList is the stream of the session the model shows ending while the
// list is not up (plan 030 §3.10, "a viewed session that ends"): the TUI goes
// back to the list rather than to the shell, the cursor on the session's row,
// and the stream's end is taken as one behind the list is (sessionEnded). err
// is why, as endMsg carries it: nil for the session's own end, its row marked
// ended and the hint line saying `that session ended`; the transport's
// failure otherwise — the connection lost, the session perhaps still running
// (C12r2) — its row left to the roster and the hint line saying the
// connection was lost, and why (sessionEnded's note). false when no list can
// be opened (a Sessions with no roster): the caller quits, as without a list.
func (m Model) endedToList(err error) (Model, tea.Cmd, bool) {
	tm, cmd := m.openSessions()
	next := tm.(Model)
	if !next.sessList.open {
		return m, nil, false
	}
	next.sessionEnded(err)
	if err == nil {
		next.sessNote(sessEndedNote, sessNoteWarn)
	}
	return next, cmd, true
}

// sessQuit is Ctrl+D, or the second Ctrl+C, on the list — and `/exit` in its
// input (§3.13): craze quits and every session keeps running (plan 030
// §3.10). It is not requestQuit, the explicit quit that stops the session a
// client shows (decision 11): the session behind the list is closed as a view
// close is (finishRun's close, a detach), and the roster is closed with the
// program. The input's popup is closed first, so a directory listing it waits
// for stops now rather than running on past the program (sol r28-c14 2).
func (m Model) sessQuit() (tea.Model, tea.Cmd) {
	m.sessList.in.closePopups()
	if m.quitting {
		return m, tea.Quit
	}
	m.quitting = true
	r, h, sh, set := m.sessList.roster, m.host, m.shell, m.sessRosters
	return m, func() tea.Msg {
		sh.shutdown()
		closeHost(h)
		set.close(r)
		return tea.Quit()
	}
}

// readSessSnap is one read of r's latest-snapshot slot, for the list's
// opening gen. It returns when a snapshot arrives or the slot closes (the
// roster's Close), so a read left over from a closed list ends with it.
func readSessSnap(r SessionRoster, gen uint64) tea.Cmd {
	ch := r.Updates()
	return func() tea.Msg {
		s, ok := <-ch
		return sessSnapMsg{gen: gen, snap: s, closed: !ok}
	}
}

// sessRosterSet is every roster the list has opened and not yet closed,
// shared by every copy of the model as owner and shell are (sol r19-c10 3).
// The list closes its own roster as it closes — a leave's command, a quit
// from the list — but a quit it never saw (SIGTERM, SIGHUP, a program error,
// a recovered panic) reaches finishRun with a final model that may be nil, and
// a leave's close is a command the program may stop before running: finishRun
// closes whatever is still here (closeAll), so no poller outlives the program.
// Nil only in a zero Model a test built, which every method allows.
type sessRosterSet struct {
	mu   sync.Mutex
	open []SessionRoster
}

func (s *sessRosterSet) add(r SessionRoster) {
	if s == nil || r == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open = append(s.open, r)
}

// close closes r (Close joins its poller) and forgets it. The lock is not
// held across Close, which can wait out an attempt's budget.
func (s *sessRosterSet) close(r SessionRoster) {
	if r == nil {
		return
	}
	r.Close()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open = slices.DeleteFunc(s.open, func(o SessionRoster) bool { return o == r })
}

// closeCmd is close off the Update: Close joins the poller.
func (s *sessRosterSet) closeCmd(r SessionRoster) tea.Cmd {
	if r == nil {
		return nil
	}
	return func() tea.Msg {
		s.close(r)
		return nil
	}
}

// closeAll closes every roster still open: finishRun's, on every exit path.
// Close is idempotent, so one a leave's command is closing at the same time
// is closed once.
func (s *sessRosterSet) closeAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	open := s.open
	s.open = nil
	s.mu.Unlock()
	for _, r := range open {
		r.Close()
	}
}

// ----------------------------------------------------------------- updates

// applySessMsg is update's hand for the list's own messages, open or not:
// each is judged against the opening it was issued under.
func (m Model) applySessMsg(msg tea.Msg) (Model, tea.Cmd, bool) {
	l := &m.sessList
	switch msg := msg.(type) {
	case sessSnapMsg:
		if !l.open || msg.gen != l.gen || msg.closed {
			return m, nil, true
		}
		l.snap, l.have = msg.snap, true
		m.trackHere()
		m.syncSessList()
		// The rows are the `@` picker's candidates too.
		sync := m.syncSessInput()
		return m, tea.Batch(readSessSnap(l.roster, l.gen), sync), true
	case sessRecentsMsg:
		if !l.open || msg.gen != l.gen || !l.in.on {
			return m, nil, true
		}
		// A failed read offers no recent directories: the list says the
		// index could not be read on its own (IndexErr).
		l.in.recents = msg.dirs
		sync := m.syncSessInput()
		return m, sync, true
	case sessAvailMsg:
		// The list's own read of the providers' availability (plan 036
		// §3.3): this opening's alone, and only while no newer call's answer
		// is kept. An open /provider popup is answered again from it.
		if !l.open || msg.gen != l.gen || !l.in.on {
			return m, nil, true
		}
		l.takeSessAvail(msg.answer)
		sync := m.syncSessInput()
		return m, sync, true
	case completeLoadedMsg:
		// The `@` popup's listing of a directory, or the `/` popup's models
		// (C16): the popup takes it only if it still awaits it
		// (completePopup.loaded) — a later listing asked for, the popup
		// closed, the list left or opened again, a switch. Another source's
		// result is not the list's.
		if msg.source != sessDirSourceID && msg.source != sessCmdSourceID {
			return m, nil, false
		}
		if !l.open || !l.in.on {
			return m, nil, true
		}
		if msg.source == sessCmdSourceID {
			// /provider's read of the providers' availability (plan 036
			// §3.3) is the list's latest too — but only the read this
			// opening of the popup awaits: one it has stopped awaiting
			// changes nothing.
			if ans, ok := msg.res.Data.(sessAvailAnswer); ok && l.in.cmd.awaits(msg) {
				l.takeSessAvail(ans)
			}
			l.in.cmd.setSource(m.sessCmdSourceNow())
			cmd, _ := l.in.cmd.loaded(msg)
			return m, cmd, true
		}
		l.in.at.setSource(m.sessAtSource())
		cmd, _ := l.in.at.loaded(msg)
		return m, cmd, true
	case sessNativeDefaultMsg:
		// /provider native's default model, read off the Update (C16): the
		// pick is the TUI's, so it is taken whether the list is up or not.
		next := m.sessNativeDefault(msg)
		sync := next.syncSessInput()
		return next, sync, true
	case sessActionMsg:
		if !l.open || msg.gen != l.gen {
			return m, nil, true
		}
		m.sessActionDone(msg)
		return m, nil, true
	case sessDisarmMsg:
		if l.open && msg.gen == l.gen && msg.seq == l.armSeq {
			l.armed = sessKey{}
		}
		return m, nil, true
	case sessRedrawMsg:
		return m, nil, true
	case sessDispatchedMsg:
		next, cmd := m.sessDispatched(msg)
		return next, cmd, true
	case sessOpenedMsg:
		next, cmd := m.sessOpened(msg)
		return next, cmd, true
	}
	return m, nil, false
}

// sessOpened is an open's answer (plan 030 §3.11). Only the open the list is
// waiting for, in the opening it was asked in, while craze is not quitting,
// is acted on: an answer after the user has moved on — a later open asked
// for, the list left (and perhaps opened again), a quit — is closed off the
// Update and never adopted (retire). A failure is the hint line's, in the
// list's own words (`could not open …`, `could not resume …` for a saved
// session — its workspace gone, its provider one this craze cannot run, its
// host refusing the load): the launcher's "craze: " is dropped, as a picker's
// error row drops it (repick). A backend is switched to (switchBackend) — a
// saved session's as a load, restoring until its replay is over (§3.12).
func (m Model) sessOpened(msg sessOpenedMsg) (Model, tea.Cmd) {
	l := &m.sessList
	if !l.open || msg.gen != l.gen || msg.seq != l.dialing || m.quitting {
		return m, m.retire(msg.b)
	}
	l.dialing, l.dialTitle, l.dialSaved = 0, "", false
	if msg.err != nil || msg.b == nil {
		err := msg.err
		if err == nil {
			err = errors.New("no session")
		}
		verb := "could not open "
		if msg.saved {
			verb = "could not resume "
		}
		why := strings.TrimPrefix(sanitizeLine(failureText(err)), "craze: ")
		m.sessNote(verb+msg.title+": "+why, sessNoteErr)
		return m, m.retire(msg.b)
	}
	return m.switchBackend(msg.b, msg.saved, msg.indexTitle)
}

// sessActionDone is a ctrl+x's answer, on the hint line.
func (m *Model) sessActionDone(msg sessActionMsg) {
	title := msg.title
	switch {
	case msg.err == nil && msg.kind == sessActCancel:
		m.sessNote("stopped: "+title, sessNoteWarn)
	case msg.err == nil:
		m.sessNote("closed: "+title, sessNoteWarn)
	case errors.Is(msg.err, backend.ErrStopUnsupported):
		m.sessNote(sessOlderNote, sessNoteWarn)
	case msg.kind == sessActCancel:
		m.sessNote("could not stop "+title+": "+sanitizeLine(failureText(msg.err)), sessNoteErr)
	default:
		m.sessNote("could not close "+title+": "+sanitizeLine(failureText(msg.err)), sessNoteErr)
	}
}

func (m *Model) sessNote(text string, kind sessNoteKind) {
	m.sessList.note, m.sessList.noteKind = text, kind
}

// sessionEnded is the stream of the session behind the list ending while
// the list is up (plan 030 §3.10): craze does not quit — the list is where
// the user is. err is why, as endMsg carries it. The session's own end (nil)
// marks its row ended, and the row stays, drawn from its last listing, once
// its host has left the registry (sol r19-c10 1); a session that ended before
// the roster ever listed it gets its row from what the model knows of it. A
// transport's failure ended only this client's connection (C12r2, r27-pr2
// 1): the session is no longer behind the list (hereLost), and its row is
// whatever the roster lists — never marked ended, since the host may serve it
// still, and opened again as any other row is. Nothing on the row says so,
// so the hint line does: the connection lost, and why.
func (m *Model) sessionEnded(err error) {
	l := &m.sessList
	if err != nil {
		l.hereLost, l.here, l.hereRow = true, sessKey{}, nil
		m.syncSessList()
		m.sessNote(sessLostNote+": "+sanitizeLine(failureText(err)), sessNoteWarn)
		return
	}
	m.trackHere()
	l.hereEnded, l.hereEndedAt = true, m.now()
	if l.hereRow == nil && !l.here.zero() {
		row := m.hereAsRow()
		l.hereRow = &row
	}
	m.syncSessList()
}

// trackHere follows the session behind the list: its identity from the
// model while it has not ended (hereKey — the session a host named after
// the list opened is found), and its row whenever the roster lists that
// identity, kept for when its host has gone — and, while it runs, the index's
// title that row carries (Model.indexTitle). Once its connection is lost
// nothing is behind the list (hereLost): the model's identity, the lost
// backend's, is not taken again.
func (m *Model) trackHere() {
	l := &m.sessList
	if !l.hereEnded && !l.hereLost {
		if k := m.hereKey(); !k.zero() {
			l.here = k
		}
	}
	if l.here.zero() {
		return
	}
	for _, r := range l.snap.Running {
		if sessRowKey(r) == l.here {
			l.hereRow = &r
			if !l.hereEnded {
				// The index's title as the row reads it, which the band names
				// the session by while it names none of its own (sessTitle).
				m.indexTitle = sanitizeLine(r.IndexTitle)
			}
			return
		}
	}
}

// hereAsRow is the session behind the list as a roster row, from what the
// model knows of it: its identity, provider, directory and title.
func (m Model) hereAsRow() roster.Row {
	here := m.sessList.here
	var info backend.SessionInfo
	if m.eng != nil {
		info = m.info()
	}
	return roster.Row{
		Host: roster.Host{CrazeSessionID: here.id, Incarnation: here.inc,
			Provider: info.Provider, Workspace: info.Workspace, Ready: true},
		Status: roster.Reachable,
		Session: &roster.Session{ID: here.id, Incarnation: here.inc,
			Provider: info.Provider, Workspace: info.Workspace, Title: m.snap.Title, RowFacts: true},
		IndexTitle: m.indexTitle,
	}
}

// -------------------------------------------------------------------- keys

// handleSessionsKey is every key while the list is open (plan 030 §3.10).
// The note on the hint line lasts until the next key; a close armed by
// ctrl+x is disarmed by any other key; the Ctrl+C window by any key but
// Ctrl+C. With an input under the list (§3.13) the input has every key first
// — its popup's, what is typed — and leaves the list's own to it
// (sessInputKey); without one, every other key does nothing.
func (m Model) handleSessionsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	l := &m.sessList
	if msg.Type != tea.KeyCtrlC {
		m.ctrlCDeadline = time.Time{}
	}
	if msg.Type != tea.KeyCtrlX {
		l.armed = sessKey{}
	}
	l.note = ""
	if l.in.on {
		if next, cmd, ok := m.sessInputKey(msg); ok {
			return next, cmd
		}
	}
	switch msg.Type {
	case tea.KeyCtrlD:
		return m.sessQuit()
	case tea.KeyCtrlC:
		now := m.now()
		if !m.ctrlCDeadline.IsZero() && now.Before(m.ctrlCDeadline) {
			return m.sessQuit()
		}
		m.ctrlCDeadline = now.Add(ctrlCWindow)
		gen := l.gen
		return m, tea.Tick(ctrlCWindow, func(time.Time) tea.Msg { return sessRedrawMsg{gen: gen} })
	case tea.KeyUp:
		m.sessMove(-1)
	case tea.KeyDown:
		m.sessMove(1)
	case tea.KeyCtrlS:
		l.byDir = !l.byDir
		m.syncSessList()
	case tea.KeyCtrlX:
		return m.sessCtrlX()
	case tea.KeyEsc:
		return m.leaveSessions()
	case tea.KeyLeft:
		// alt+← is not a list key (plan 030: no cycling keys).
		if !msg.Alt {
			return m.leaveSessions()
		}
	case tea.KeyEnter:
		return m.sessEnter()
	case tea.KeyRight:
		if !msg.Alt {
			return m.sessEnter()
		}
	}
	return m, nil
}

// sessEnter is enter and → on the selected line: the saved group's line
// expands or collapses it; the session the list was opened from is the
// screen behind the list, so opening it is going back; any other running
// session is opened in place (plan 030 §3.11: sessOpen), the one whose
// connection was lost among them (no longer behind the list: hereLost), and
// so is a saved one — resumed by a host loading it in its own workspace, or
// the host that holds it already (§3.12: Sessions.Open of its saved ref). A
// host that does not answer is said not to, without a dial.
func (m Model) sessEnter() (tea.Model, tea.Cmd) {
	if m.sessList.sel == sessSavedLine {
		m.sessList.savedOpen = !m.sessList.savedOpen
		m.syncSessList()
		return m, nil
	}
	r, ok := m.sessSelected()
	switch {
	case !ok:
		return m, nil
	case r.here:
		return m.leaveSessions()
	case r.state == sessUnreachable:
		m.sessNote(sessUnreachNote, sessNoteWarn)
		return m, nil
	}
	return m.sessOpen(r)
}

// sessOpen dials r's session off the Update (Sessions.Open may take seconds:
// a dial and an attach, and for a saved session a host spawned to load it)
// and waits for that open alone: a later one replaces it, and leaving the
// list abandons it (sessOpened). The list stays up meanwhile, the session
// behind it attached, and the hint line says what is being opened — or
// resumed.
func (m Model) sessOpen(r sessRow) (tea.Model, tea.Cmd) {
	l := &m.sessList
	l.dialSeq++
	l.dialing, l.dialTitle, l.dialSaved = l.dialSeq, r.title, r.saved
	s, ref, gen, seq, title, index, saved := m.sessions, r.ref, l.gen, l.dialSeq, r.title, r.indexTitle, r.saved
	return m, func() tea.Msg {
		b, err := s.Open(ref)
		return sessOpenedMsg{gen: gen, seq: seq, title: title, indexTitle: index, saved: saved, b: b, err: err}
	}
}

// sessCtrlX is ctrl+x on the selected row (plan 030 §3.10): on a working or
// asking session its turn is stopped and its queue cleared
// (Sessions.Cancel); on an idle or failed one the first press arms a close
// and a second within sessCloseWindow sends it (Sessions.Stop). A saved row,
// a host not yet answering and the ended session take nothing; an
// unreachable one is said to be.
func (m Model) sessCtrlX() (tea.Model, tea.Cmd) {
	l := &m.sessList
	r, ok := m.sessSelected()
	switch {
	case !ok:
		l.armed = sessKey{}
		return m, nil
	case r.state == sessUnreachable:
		l.armed = sessKey{}
		m.sessNote(sessUnreachNote, sessNoteWarn)
		return m, nil
	case r.cancellable():
		l.armed = sessKey{}
		return m, m.sessAction(sessActCancel, r)
	case !r.closable():
		l.armed = sessKey{}
		return m, nil
	}
	now := m.now()
	if l.armed == r.key && now.Sub(l.armedAt) < sessCloseWindow {
		l.armed = sessKey{}
		return m, m.sessAction(sessActClose, r)
	}
	l.armed, l.armedAt = r.key, now
	l.armSeq++
	gen, seq := l.gen, l.armSeq
	return m, tea.Tick(sessCloseWindow, func(time.Time) tea.Msg { return sessDisarmMsg{gen: gen, seq: seq} })
}

// sessAction makes a ctrl+x's call off the Update: Sessions' methods may
// take seconds (a dial and a round trip or two).
func (m Model) sessAction(kind sessActKind, r sessRow) tea.Cmd {
	s, ref, gen, title := m.sessions, r.ref, m.sessList.gen, r.title
	return func() tea.Msg {
		var err error
		if kind == sessActCancel {
			err = s.Cancel(ref)
		} else {
			err = s.Stop(ref)
		}
		return sessActionMsg{gen: gen, kind: kind, title: title, err: err}
	}
}

// sessMove moves the selection delta selectable lines, stopping at either
// end.
func (m *Model) sessMove(delta int) {
	keys := sessKeys(m.sessLines())
	if len(keys) == 0 {
		return
	}
	i := slices.Index(keys, m.sessList.sel)
	if i < 0 {
		i = min(max(m.sessList.selIdx, 0), len(keys)-1)
	} else {
		i = min(max(i+delta, 0), len(keys)-1)
	}
	m.sessList.sel, m.sessList.selIdx = keys[i], i
}

// syncSessList re-finds the selection and the armed close among the lines
// as they stand now (plan 030 §3.10): both are held by identity, so a
// reorder moves neither. A selected row that is gone leaves the selection
// to its neighbour — the line that took its place, or the last — except a
// running session whose incarnation was replaced, which stays selected under
// its new one. An armed close is dropped when its row is gone, replaced, or
// no longer closable.
func (m *Model) syncSessList() {
	l := &m.sessList
	lines := m.sessLines()
	if !l.armed.zero() {
		r, ok := sessFind(lines, l.armed)
		if !ok || !r.closable() {
			l.armed = sessKey{}
		}
	}
	keys := sessKeys(lines)
	if i := slices.Index(keys, l.sel); i >= 0 {
		l.selIdx = i
		return
	}
	if len(keys) == 0 {
		return
	}
	if l.sel != sessSavedLine && l.sel.id != "" {
		for i, k := range keys {
			if k.id == l.sel.id && k != sessSavedLine {
				if r, ok := sessFind(lines, k); ok && !r.saved {
					l.sel, l.selIdx = k, i
					return
				}
			}
		}
	}
	i := min(max(l.selIdx, 0), len(keys)-1)
	l.sel, l.selIdx = keys[i], i
}

// sessSelected is the selected session row, if the selection is on one.
func (m Model) sessSelected() (sessRow, bool) {
	return sessFind(m.sessLines(), m.sessList.sel)
}

// ------------------------------------------------------------------- rows

// sessRunningRows is every running session the snapshot lists, as rows,
// and the session behind the list once it has ended: marked ended while its
// host is still listed, and from its kept row (hereRow) once the host has
// left the registry (sol r19-c10 1). An ended row's age counts from its end.
func (m Model) sessRunningRows() []sessRow {
	l := m.sessList
	rows := make([]sessRow, 0, len(l.snap.Running)+1)
	ended := func(row sessRow) sessRow {
		if row.here && l.hereEnded {
			row.state, row.ended, row.want, row.connecting = sessIdle, true, "ended", false
			row.since = l.hereEndedAt
		}
		return row
	}
	listed := false
	for _, r := range l.snap.Running {
		row := ended(sessRunningRow(r, l.here))
		listed = listed || row.here
		rows = append(rows, row)
	}
	if l.hereEnded && !listed && l.hereRow != nil {
		rows = append(rows, ended(sessRunningRow(*l.hereRow, l.here)))
	}
	return rows
}

// sessRowKey is a running row's identity: the craze id and incarnation its
// host answered, else its registry entry's; a host that has named no session
// yet by its host id.
func sessRowKey(r roster.Row) sessKey {
	id, inc := r.Host.CrazeSessionID, r.Host.Incarnation
	if s := r.Session; s != nil {
		if s.ID != "" {
			id = s.ID
		}
		if s.Incarnation != "" {
			inc = s.Incarnation
		}
	}
	if id == "" {
		return sessKey{id: "\x00host:" + r.Host.ID}
	}
	return sessKey{id: id, inc: inc}
}

// sessTitle is a running session's title as its list row and its band both
// name it (plan 030 §3.10–§3.11): its own — the agent's, or a rename — else
// the session index's (the first prompt's line the index titles it by until
// either names it), else sessUntitled. One rule, so the band over a session
// opened from its row reads what the row read (C11r, found by C12).
func sessTitle(own, index string) string {
	if t := sanitizeLine(own); t != "" {
		return t
	}
	if t := sanitizeLine(index); t != "" {
		return t
	}
	return sessUntitled
}

// sessRunningRow is one running session's row (plan 030 §3.10's table).
// The host's own answer (Session) wins over what its registry entry and
// the index say; a host with none yet is Connecting (X68), drawn with the
// working rows: Starting… while it has not published a ready session,
// Connecting… until its first answer. It is the session behind the list
// (here) only under that session's own identity — craze id and incarnation.
func sessRunningRow(r roster.Row, here sessKey) sessRow {
	h, s := r.Host, r.Session
	row := sessRow{
		ref:        r.Ref(),
		provider:   h.Provider,
		workspace:  h.Workspace,
		indexTitle: sanitizeLine(r.IndexTitle),
		key:        sessRowKey(r),
	}
	own := ""
	if s != nil {
		if s.Provider != "" {
			row.provider = s.Provider
		}
		if s.Workspace != "" {
			row.workspace = s.Workspace
		}
		own = s.Title
		if !s.RowFacts && r.Version != "" {
			// An older host (no rowFacts, §3.8): listed with what S2's row
			// has, and its craze version in the row's note.
			row.note = "craze " + sanitizeLine(r.Version)
		}
	}
	row.title = sessTitle(own, row.indexTitle)
	row.here = !here.zero() && row.key == here
	// The state is the roster's rule (roster.Row.State), which craze ps
	// prints too: one rule for both.
	row.since = r.Since()
	switch r.State() {
	case roster.StateUnreachable:
		row.state, row.want = sessUnreachable, "not answering"
	case roster.StateStarting:
		row.state, row.connecting = sessWorking, true
		row.want = "Connecting…"
		if !h.Ready || h.CrazeSessionID == "" {
			row.want = "Starting…"
		}
	default:
		row.state, row.want = sessStateOf(s)
	}
	return row
}

// sessStateOf is an answering session's state and what it wants (plan 030
// §3.10's table): its state is the roster's rule (roster.Session.State, the
// engine's own precedence, X64). An older host's row has no row facts: its
// head ask's label, no error text, no Doing, no last reply — each read as
// the table's fallback.
func sessStateOf(s *roster.Session) (sessState, string) {
	switch s.State() {
	case roster.StateNeedsYou:
		return sessNeedsYou, sessAskText(s.HeadAsk)
	case roster.StateFailed:
		msg := ""
		switch {
		case s.StartFailed:
			msg = s.StartErr
		case s.LastTurn != nil:
			msg = s.LastTurn.Err
		}
		if msg = sanitizeLine(msg); msg == "" {
			return sessFailed, "error"
		}
		return sessFailed, "error: " + msg
	case roster.StateWorking:
		if d := sanitizeLine(s.Doing); d != "" {
			return sessWorking, d
		}
		switch s.Activity {
		case engine.ActivityStarting:
			return sessWorking, "Starting…"
		case engine.ActivityReplaying:
			return sessWorking, "Loading…"
		case engine.ActivityClosing:
			return sessWorking, "Closing…"
		}
		return sessWorking, "Working"
	}
	if r := sanitizeLine(s.LastReply); r != "" {
		return sessIdle, r
	}
	return sessIdle, "waiting for a prompt"
}

// sessAskText is a head ask's "what it wants": `<kind>: <summary>`, or, from
// a host without the summary (an older one), its label — which names the
// kind first, and is drawn the same way (permission Shell → permission:
// Shell).
func sessAskText(a *roster.HeadAsk) string {
	if a == nil {
		return "waiting for you"
	}
	kind := sanitizeLine(a.Kind)
	if sum := sanitizeLine(a.Summary); sum != "" && kind != "" {
		return kind + ": " + sum
	}
	label := sanitizeLine(a.Label)
	if rest, ok := strings.CutPrefix(label, kind+" "); ok && kind != "" && rest != "" {
		return kind + ": " + rest
	}
	if label != "" {
		return label
	}
	if kind != "" {
		return kind
	}
	return "waiting for you"
}

// sessSavedRow is a saved session's row: its title only (§3.10's table).
func sessSavedRow(r sessions.Row) sessRow {
	id := "\x00saved:" + r.CrazeID
	if r.CrazeID == "" {
		id = "\x00saved:" + r.Provider + ":" + r.SessionID
	}
	title := sanitizeLine(r.Title)
	if title == "" {
		title = r.SessionID
	}
	return sessRow{
		key: sessKey{id: id}, state: sessSaved, saved: true,
		title: title, indexTitle: sanitizeLine(r.Title),
		provider: r.Provider, workspace: r.CWD, since: r.UpdatedAt,
		ref: roster.SavedRef(r),
	}
}

// sessOrder is a group's order (plan 030 §3.10): by state, then by since,
// newest first, then by craze id — so rows do not reshuffle as what a
// session is doing changes, only as it changes state.
func sessOrder(a, b sessRow) int {
	if a.state != b.state {
		return cmp.Compare(a.state, b.state)
	}
	if !a.since.Equal(b.since) {
		if a.since.After(b.since) {
			return -1
		}
		return 1
	}
	return strings.Compare(a.key.id, b.key.id)
}

// sessLineKind is what one line of the list body is.
type sessLineKind int

const (
	sessLineBlank sessLineKind = iota
	sessLineGroup
	sessLineRow
	sessLineSaved
	sessLineNote
)

// sessLine is one line of the list body before it is drawn.
type sessLine struct {
	kind sessLineKind
	// A group's header: its label, colour and count.
	label string
	color lipgloss.CompleteColor
	count int
	// row is a session row's; for sessLineNote its text is want, and
	// noteErr colours it as an error.
	row     sessRow
	noteErr bool
}

// sessLines is the list body, in order: the running sessions in their
// groups — by state (needs you, working, failed, idle, unreachable) or, with
// ctrl+s, by directory — then the saved group's line and, expanded, its
// rows; then any note. Pure: every key handler and View build the same
// lines from the same state.
func (m Model) sessLines() []sessLine {
	l := m.sessList
	if !l.have {
		return nil
	}
	running := m.sessRunningRows()
	slices.SortStableFunc(running, sessOrder)
	var out []sessLine
	group := func(label string, color lipgloss.CompleteColor, rows []sessRow) {
		if len(rows) == 0 {
			return
		}
		if len(out) > 0 {
			out = append(out, sessLine{kind: sessLineBlank})
		}
		out = append(out, sessLine{kind: sessLineGroup, label: label, color: color, count: len(rows)})
		for _, r := range rows {
			out = append(out, sessLine{kind: sessLineRow, row: r})
		}
	}
	if l.byDir {
		var dirs []string
		byDir := map[string][]sessRow{}
		for _, r := range running {
			if _, ok := byDir[r.workspace]; !ok {
				dirs = append(dirs, r.workspace)
			}
			byDir[r.workspace] = append(byDir[r.workspace], r)
		}
		for _, d := range dirs {
			label := sessTilde(l.home, d)
			if label == "" {
				label = "(no directory)"
			}
			group(label, m.theme.FG, byDir[d])
		}
	} else {
		th := m.theme
		for _, g := range []struct {
			state sessState
			label string
			color lipgloss.CompleteColor
		}{
			{sessNeedsYou, "needs you", th.Warn},
			{sessWorking, "working", th.Accent},
			{sessFailed, "failed", th.Err},
			{sessIdle, "idle", th.Dim},
			{sessUnreachable, "unreachable", th.Dim},
		} {
			var rows []sessRow
			for _, r := range running {
				if r.state == g.state {
					rows = append(rows, r)
				}
			}
			group(g.label, g.color, rows)
		}
	}
	if n := len(l.snap.Saved); n > 0 {
		out = append(out, sessLine{kind: sessLineBlank},
			sessLine{kind: sessLineSaved, count: n})
		if l.savedOpen {
			for _, r := range l.snap.Saved {
				out = append(out, sessLine{kind: sessLineRow, row: sessSavedRow(r)})
			}
		}
	}
	others := len(l.snap.Saved)
	for _, r := range running {
		if !r.here {
			others++
		}
	}
	if others == 0 {
		if len(out) > 0 {
			out = append(out, sessLine{kind: sessLineBlank})
		}
		note := sessEmptyNote
		if l.in.on {
			// The list with an input points at it (§3.10: PR 3 names it).
			note = sessEmptyNoteInput
		}
		out = append(out, sessLine{kind: sessLineNote, row: sessRow{want: note}})
	}
	if err := l.snap.RegistryErr; err != nil {
		out = append(out, sessLine{kind: sessLineNote, noteErr: true,
			row: sessRow{want: "the running sessions could not be read: " + sanitizeLine(err.Error())}})
	}
	if err := l.snap.IndexErr; err != nil {
		out = append(out, sessLine{kind: sessLineNote, noteErr: true,
			row: sessRow{want: "the saved sessions could not be read: " + sanitizeLine(err.Error())}})
	}
	return out
}

// sessKeys is the selectable lines' keys, in order: every session row and
// the saved group's line.
func sessKeys(lines []sessLine) []sessKey {
	var keys []sessKey
	for _, ln := range lines {
		switch ln.kind {
		case sessLineRow:
			keys = append(keys, ln.row.key)
		case sessLineSaved:
			keys = append(keys, sessSavedLine)
		}
	}
	return keys
}

// sessFind is the session row lines hold under key.
func sessFind(lines []sessLine, key sessKey) (sessRow, bool) {
	for _, ln := range lines {
		if ln.kind == sessLineRow && ln.row.key == key {
			return ln.row, true
		}
	}
	return sessRow{}, false
}

// sessTilde is dir with home abbreviated to ~.
func sessTilde(home, dir string) string {
	if home == "" || home == "/" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(dir, home+"/"); ok {
		return "~/" + rest
	}
	return dir
}

// sessAge is how long a row has been in its state, in one unit — the resume
// picker's rule, with seconds under a minute. Frozen (the frame runner's
// --freeze) it reads zero, as every elapsed counter does; "" for a row with
// no time.
func (m Model) sessAge(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	d := m.now().Sub(since)
	if d < 0 || m.frozen {
		d = 0
	}
	return sessAgeText(d)
}

// sessAgeText is an age in its one largest unit: `42s`, `5m`, `3h`, `6d`.
func sessAgeText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// sessListSpinning says the list shows a working row, whose spinner the
// fast tick turns.
func (m Model) sessListSpinning() bool {
	if !m.sessList.open {
		return false
	}
	for _, r := range m.sessRunningRows() {
		if r.state == sessWorking {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------- view

// The columns (plan 030 §3.10): mark, glyph, title, what it wants,
// provider, directory basename, age — fixed shares of what the fixed
// columns leave, the mockup's: the title 36 %, the directory 18 % (none when
// grouped by directory), what it wants the rest; each clipped with an
// ellipsis.
const (
	sessMarkCols     = 2
	sessGlyphCols    = 2
	sessGapCols      = 2
	sessProviderCols = 8
	sessAgeCols      = 5
	sessFixedCols    = sessMarkCols + sessGlyphCols + 2*sessGapCols + sessProviderCols + sessAgeCols
	// sessNoteMinWant is the fewest cells what a row wants keeps beside an
	// older host's note; narrower, the note goes.
	sessNoteMinWant = 8
)

// sessColumns is the three shared columns' widths at width.
func sessColumns(width int, showDir bool) (title, want, dir int) {
	rest := max(0, width-sessFixedCols)
	if showDir {
		dir = rest * 18 / 100
	}
	title = rest * 36 / 100
	return title, rest - title - dir, dir
}

// sessionsView is the list's whole screen: its own region set — the header
// and a spacer, the list, the footer rule and the hint line — exactly
// width × height, or the too-small message below 40×10. With an input
// (§3.13) the footer is the input's: the rule naming where a new session
// would run, the input, the rule and the hint line, and the input's popup —
// the `@` one or the `/` one — while it is up, sits between the list and that
// rule, the list keeping at least sessBodyMinRows.
func (m Model) sessionsView() string {
	w, h := m.width, m.height
	if w < sessMinCols || h < sessMinRows {
		return tooSmallViewFor(w, h, sessMinCols, sessMinRows)
	}
	lines := m.sessLines()
	in := m.sessList.in
	footer := sessFooterRows
	if in.on {
		footer = sessInputRows
	}
	avail := h - sessHeaderRows - footer
	pop := 0
	popup, up := in.popup()
	if in.on && up {
		pop = popup.height(max(0, avail-sessBodyMinRows))
	}
	rows := make([]string, 0, h)
	rows = append(rows, padRow(m.sessHeaderRow(lines), w), padRow("", w))
	for _, ln := range m.sessBody(lines, avail-pop) {
		rows = append(rows, padRow(ln, w))
	}
	if pop > 0 {
		rows = append(rows, popup.view(m.theme, w, pop))
	}
	if in.on {
		rows = append(rows, padRow(m.sessTargetRule(w), w), padRow(m.sessInputRow(w), w))
	}
	rows = append(rows, padRow(styleFG(m.theme.Rule).Render(strings.Repeat("─", w)), w))
	rows = append(rows, padRow(m.sessHintRow(lines), w))
	return strings.Join(rows, "\n")
}

// sessHeaderRow is ` sessions  N running` and, right-aligned, the counts —
// `! 2 need you`, `✳ 2 working`, `✗ 1 failed`, `○ 1 idle` — dropped from the
// right as the width shrinks. Before the first snapshot it claims no count.
func (m Model) sessHeaderRow(lines []sessLine) string {
	th := m.theme
	left := []seg{{" sessions", styleFG(th.Bright).Bold(true)}}
	if !m.sessList.have {
		return renderSegs(m.width, left...)
	}
	var counts [4]int
	for _, ln := range lines {
		if ln.kind != sessLineRow || ln.row.saved {
			continue
		}
		switch ln.row.state {
		case sessNeedsYou, sessWorking, sessFailed, sessIdle:
			counts[ln.row.state]++
		}
	}
	left = append(left, seg{fmt.Sprintf("  %d running", len(m.sessList.snap.Running)), styleFG(th.Dim)})
	var chips []seg
	for i, c := range []struct {
		text  string
		color lipgloss.CompleteColor
	}{
		{"! %d need you", th.Warn},
		{m.spinnerGlyph() + " %d working", th.Accent},
		{"✗ %d failed", th.Err},
		{"○ %d idle", th.Dim},
	} {
		if counts[i] > 0 {
			chips = append(chips, seg{fmt.Sprintf(c.text, counts[i]), styleFG(c.color)})
		}
	}
	leftW := segsWidth(left)
	for len(chips) > 0 && leftW+2+sessChipsWidth(chips)+1 > m.width {
		chips = chips[:len(chips)-1]
	}
	if len(chips) == 0 {
		return renderSegs(m.width, left...)
	}
	segs := append(left, seg{strings.Repeat(" ", m.width-leftW-sessChipsWidth(chips)-1), lipgloss.NewStyle()})
	for i, c := range chips {
		if i > 0 {
			segs = append(segs, seg{"   ", lipgloss.NewStyle()})
		}
		segs = append(segs, c)
	}
	return renderSegs(m.width, segs...)
}

// sessChipsWidth is the chips' width, three spaces between each.
func sessChipsWidth(chips []seg) int {
	return segsWidth(chips) + 3*max(0, len(chips)-1)
}

func segsWidth(segs []seg) int {
	w := 0
	for _, s := range segs {
		w += lipgloss.Width(s.text)
	}
	return w
}

// sessBody is the list's n rows: the lines drawn and windowed onto the
// selection — the selected line kept off the window's first and last row
// while there is more above or below, where `↑ more above` and `↓ more
// below` say so.
func (m Model) sessBody(lines []sessLine, n int) []string {
	if n <= 0 {
		return nil
	}
	sel := 0
	for i, ln := range lines {
		if (ln.kind == sessLineRow && ln.row.key == m.sessList.sel) ||
			(ln.kind == sessLineSaved && m.sessList.sel == sessSavedLine) {
			sel = i
			break
		}
	}
	off := 0
	if len(lines) > n {
		off = max(0, min(sel+2-n, len(lines)-n))
		if sel-1 < off {
			off = max(0, sel-1)
		}
	}
	end := min(len(lines), off+n)
	out := make([]string, 0, n)
	for i := off; i < end; i++ {
		out = append(out, m.sessLineView(lines[i], i == sel && m.sessSelectable(lines[i])))
	}
	dim := styleFG(m.theme.Dim)
	if n > 1 && off > 0 {
		out[0] = renderSegs(m.width, seg{"    ↑ more above", dim})
	}
	if n > 1 && len(lines) > end {
		out[len(out)-1] = renderSegs(m.width, seg{"    ↓ more below", dim})
	}
	for len(out) < n {
		out = append(out, "")
	}
	return out
}

func (m Model) sessSelectable(ln sessLine) bool {
	return ln.kind == sessLineRow || ln.kind == sessLineSaved
}

// sessLineView draws one body line.
func (m Model) sessLineView(ln sessLine, selected bool) string {
	th := m.theme
	rule := styleFG(th.Rule)
	switch ln.kind {
	case sessLineGroup:
		pre := []seg{{ln.label, styleFG(ln.color).Bold(true)}, {fmt.Sprintf(" %d ", ln.count), styleFG(th.Dim)}}
		return renderSegs(m.width, append(pre, seg{strings.Repeat("─", max(1, m.width-segsWidth(pre))), rule})...)
	case sessLineSaved:
		arrow := "▸ "
		if m.sessList.savedOpen {
			arrow = "▾ "
		}
		pre := []seg{m.sessMark(selected), {arrow, styleFG(th.Dim)}, {"saved", styleFG(th.Dim).Bold(true)},
			{fmt.Sprintf(" · %d not running ", ln.count), styleFG(th.Dim)}}
		return renderSegs(m.width, append(pre, seg{strings.Repeat("─", max(1, m.width-segsWidth(pre))), rule})...)
	case sessLineRow:
		return m.sessRowView(ln.row, selected)
	case sessLineNote:
		st := styleFG(th.Dim)
		if ln.noteErr {
			st = styleFG(th.Err)
		}
		return renderSegs(m.width, seg{"  " + ln.row.want, st})
	}
	return ""
}

func (m Model) sessMark(selected bool) seg {
	if selected {
		return seg{agentGutterMark, styleFG(m.theme.Accent)}
	}
	return seg{agentGutterBlank, lipgloss.NewStyle()}
}

// sessGlyph is a row's state glyph and its style (plan 030 §3.10's table):
// needs you `!` bold amber, working the spinner, failed `✗` red, idle `○`
// dim, unreachable `?` dim, saved and ended `·` dim. Nothing blinks.
func (m Model) sessGlyph(r sessRow) (string, lipgloss.Style) {
	th := m.theme
	switch {
	case r.saved || r.ended:
		return "·", styleFG(th.Dim)
	case r.state == sessNeedsYou:
		return "!", styleFG(th.Warn).Bold(true)
	case r.state == sessWorking:
		return m.spinnerGlyph(), styleFG(th.Accent)
	case r.state == sessFailed:
		return "✗", styleFG(th.Err).Bold(true)
	case r.state == sessUnreachable:
		return "?", styleFG(th.Dim)
	}
	return "○", styleFG(th.Dim)
}

// sessRowView draws one session row in its columns. The directory column is
// omitted when grouped by directory — the header says it — except on a
// saved row, which sits under no directory header.
func (m Model) sessRowView(r sessRow, selected bool) string {
	th := m.theme
	showDir := !m.sessList.byDir || r.saved
	tW, wW, dW := sessColumns(m.width, showDir)
	glyph, gst := m.sessGlyph(r)
	titleSt := styleFG(th.Bright)
	if r.saved {
		titleSt = styleFG(th.Dim)
	}
	if selected {
		titleSt = titleSt.Bold(true)
	}
	wantSt := styleFG(th.Dim)
	switch {
	case r.ended:
	case r.state == sessNeedsYou:
		wantSt = styleFG(th.Warn)
	case r.state == sessFailed:
		wantSt = styleFG(th.Err)
	}
	none := lipgloss.NewStyle()
	segs := []seg{m.sessMark(selected), {glyph + " ", gst}, {padCells(r.title, tW), titleSt}, {"  ", none}}
	// An older host's note keeps its cells: what it wants is clipped
	// first, down to a few cells, before the note is dropped.
	note := ""
	if r.note != "" {
		note = " · " + r.note
	}
	if nw := lipgloss.Width(note); note != "" && wW >= nw+sessNoteMinWant {
		want := clampWidth(r.want, wW-nw)
		segs = append(segs, seg{want, wantSt}, seg{padCells(note, wW-lipgloss.Width(want)), styleFG(th.Dim)})
	} else {
		segs = append(segs, seg{padCells(r.want, wW), wantSt})
	}
	segs = append(segs, seg{"  ", none}, seg{padCells(r.provider, sessProviderCols), styleFG(th.Provider)})
	if showDir {
		dir := workspaceName(r.workspace)
		if r.workspace == "" {
			dir = ""
		}
		if r.here {
			dir += " · here"
		}
		segs = append(segs, seg{padCells(dir, dW), styleFG(th.Dim)})
	}
	segs = append(segs, seg{rightCells(m.sessAge(r.since), sessAgeCols), styleFG(th.Dim)})
	return renderSegs(m.width, segs...)
}

// padCells is s in exactly w cells: clipped with an ellipsis, or padded.
func padCells(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = clampWidth(s, w)
	return s + strings.Repeat(" ", max(0, w-lipgloss.Width(s)))
}

// rightCells is s right-aligned in exactly w cells.
func rightCells(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = clampWidth(s, w)
	return strings.Repeat(" ", max(0, w-lipgloss.Width(s))) + s
}

// sessHintRow is the hint line: a Ctrl+C window's, an armed close's, the
// note, the `@` popup's keys while it is up, the input's while something is
// typed (sessInputHint), or the keys the selected line takes.
func (m Model) sessHintRow(lines []sessLine) string {
	th := m.theme
	key := func(k string) seg { return seg{k, styleFG(th.Accent)} }
	txt := func(t string) seg { return seg{t, styleFG(th.Dim)} }
	lead := seg{" ", lipgloss.NewStyle()}
	l := m.sessList
	switch {
	case !m.ctrlCDeadline.IsZero() && m.now().Before(m.ctrlCDeadline):
		return renderSegs(m.width, lead, key("ctrl+c"), txt(" again quits craze; every session keeps running"))
	case !l.armed.zero():
		return renderSegs(m.width, lead, seg{"ctrl+x", styleFG(th.Err).Bold(true)},
			seg{" again closes it; the transcript stays resumable", styleFG(th.Err)})
	case l.note != "":
		return renderSegs(m.width, lead, seg{l.note, m.sessNoteStyle(l.noteKind)})
	case l.dialing != 0 && l.dialSaved:
		return renderSegs(m.width, lead, txt("resuming "+l.dialTitle+"…"))
	case l.dialing != 0:
		return renderSegs(m.width, lead, txt("opening "+l.dialTitle+"…"))
	}
	if hint := m.sessInputHint(key, txt); hint != nil {
		return renderSegs(m.width, append([]seg{lead}, hint...)...)
	}
	grouping := " by directory · "
	if l.byDir {
		grouping = " by state · "
	}
	tail := []seg{key("ctrl+s"), txt(grouping), key("←"), txt(" back")}
	segs := []seg{lead}
	if l.sel == sessSavedLine && slices.Contains(sessKeys(lines), sessSavedLine) {
		verb := " show saved · "
		if l.savedOpen {
			verb = " hide saved · "
		}
		segs = append(segs, key("enter"), txt(verb))
		return renderSegs(m.width, append(segs, tail...)...)
	}
	r, ok := sessFind(lines, l.sel)
	switch {
	case !ok:
	case r.saved:
		segs = append(segs, key("enter"), txt(" resume · "))
	case r.here && !r.ended:
		segs = append(segs, key("enter"), txt(" back to it · "))
	case !r.ended:
		segs = append(segs, key("enter"), txt(" open · "))
	}
	switch {
	case !ok:
	case r.cancellable():
		segs = append(segs, key("ctrl+x"), txt(" stop · "))
	case r.closable():
		segs = append(segs, key("ctrl+x"), txt(" close · "))
	}
	return renderSegs(m.width, append(segs, tail...)...)
}
