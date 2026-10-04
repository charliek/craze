package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/paths"
)

// /connect (plan 031 §3.9, owner decision Q3): a native session's own way to
// give a model provider its API key without leaving the TUI — the TUI twin of
// `craze auth login`, storing the key the same way (modeltable.SetKey, into
// providers.toml) and, like it, never checking it with the provider (no probe:
// a bad key shows on first use). Also opened by the connect row that ends
// native's `/model` list while some provider has no key (§3.6).
//
//   - Native only. The builtin is listed (slashCatalog) and dispatched
//     (handleEnter, then runBuiltin) on a native session alone; anywhere else a typed /connect
//     is the text it is, sent to the agent as it always was (CR 9). Before
//     any session the same dialog opens in a mode of its own when a picker
//     chooses native while no provider has a key (plan 036 §3.6,
//     connect_pre.go).
//   - Refused while work runs — a turn, the agent's own, or a background
//     sub-agent (connectBusy): a key stored then would be one a running shell
//     or child cannot redact, since a session learns stored keys only when a
//     turn starts (§3.8). The TUI's view of the session can lag the host, and
//     another client can start work while the box is open, so this narrows
//     the window and does not close it (R1); the save re-checks.
//   - Two steps. "Connect a provider" lists every provider craze knows
//     (modeltable.Providers: the shipped catalog's and the user's own), by
//     display name, the connected ones marked ✓ — a rotation is a re-connect.
//     "<Name> API key" is a masked field; its hint names the file the key goes
//     to (R2: the TUI's CRAZE_HOME, which may not be the one an attached
//     host's session reads) and, when one of the provider's variables funds
//     it, that the variable wins. Enter stores the key off the Update; an
//     unusable key is refused in the field, nothing stored.
//   - A provider funded by signing in — the ChatGPT plan's (plan 033 §3.13) —
//     has no key field: its row opens a third step, "Sign in with ChatGPT",
//     which runs the sign-in itself (connect_signin.go).
//   - The running session keeps its model table (P8): the notice after a save
//     says new sessions offer the provider's models, and that this
//     conversation does after /exit and craze -c.
//   - Every read and write goes through Model.nativeDir and Model.nativeEnv
//     (Config.NativeDir, Config.Getenv) and nothing else, and runs off the
//     Update as a gated call (connectCall): the providers' read when the box
//     opens, the save, and the model dialog's question about its connect row.
//     The gate holds what arrives meanwhile — keys typed ahead included — and
//     applies it once the answer is in, so a ↓ Enter typed before the list is
//     read acts on the list, and no frame shows a state the answer is about
//     to change (plan 027 §3.12).
//
// Paste safety (astra 15, CR 23). A key reaches the field typed, as a
// terminal's bracketed paste — a key message, which handleKey hands to the
// open dialog like any key — or as craze's own clipboard paste (Ctrl+V),
// whose read answers later: that answer carries the field it was asked for
// in (keyField: the dialog's number and the field's), and lands only in that
// field while it is open (pasteIntoKey), never in the composer. The field is
// emptied on every way out — a save, Esc, a card or another dialog
// (closeDialog), a session switch (withSession makes the dialog afresh), a
// quit and the session's end (dropConnect) — and when the key in it is
// refused; only a save refused because work started keeps it, for the next
// Enter. Its text is never written to the transcript, an error, a note or the
// composer.

const (
	connectDialogTitle = "Connect a provider"
	// connectPickHint and connectKeyHint are the two steps' footers.
	connectPickHint = "↑↓ · enter · esc"
	connectKeyHint  = "enter saves · esc back"
	// connectKeyTitle follows the provider's display name: step two's title.
	connectKeyTitle = " API key"
	// connectRowText is the row that ends native's /model list while a
	// provider has no key (plan 031 §3.6).
	connectRowText = "Connect a provider…"
	// connectBusyText is /connect refused while work runs (§3.9, R1).
	connectBusyText = "Finish or stop the running work first, then /connect."
	// connectBusySaveText is the same refusal at the save: work that started
	// while the box was open. The key stays in the field for a later Enter.
	connectBusySaveText = "Finish or stop the running work first, then press enter to save."
	// connectConnectedMark marks a connected provider in step one, and
	// connectUnusableMark one whose stored key cannot be used.
	connectConnectedMark = "✓"
	connectUnusableMark  = "stored key unusable"
	// connectKeyMax is the longest key the field stores, as `craze auth
	// login` reads at most 8 KiB: no provider's key comes near it, and a
	// longer paste is a file pasted by mistake.
	connectKeyMax = 8 << 10
	// connectMask draws each character of the key.
	connectMask = '•'
)

// connectBuiltin is /connect's row: in the slash catalog on a native session
// alone, right after /model (slashCatalog). builtinNamed never names it — it
// knows the fixed set, which every provider has — so dispatch has its own
// gate (handleEnter).
var connectBuiltin = slashItem{Name: "connect", Desc: "Connect a model provider: store its API key", Builtin: true}

// nativeProviderName is the id a native session's snapshot names its
// provider by.
var nativeProviderName = agent.NativeProvider().Name()

// connectOffered reports whether this session has /connect and the connect
// row: a native session, by the provider its facts name.
func (m Model) connectOffered() bool { return m.snap.Provider.Name == nativeProviderName }

// connectBusy reports work running that /connect refuses under (§3.9): a
// turn — this client's or another's — the agent's own (a wake), or any
// sub-agent still running, a background one's included. A background bash
// job is not one (plan 033 §3.8, anySubagentRunning): a server left running
// would refuse /connect for its whole life.
//
// The pre-session dialog is never busy (plan 036 §3.6): the rule guards the
// shown session, whose running work could not redact a key stored now, and
// that dialog is no session's — opened from a picker before any, or from the
// list rather than the session behind it — as craze auth login in another
// terminal is no session's.
func (m Model) connectBusy() bool {
	if m.cdlg.pre {
		return false
	}
	return m.status == statusWorking || m.snap.ForeignTurn || m.anySubagentRunning()
}

// configNativeDir and configGetenv are Config.NativeDir and Config.Getenv as
// New resolves them: the craze directory's native/ and the process
// environment when a Config names neither — every production caller. A model
// New never built has neither, and reads nothing: every read and write starts
// from a native directory.
func configNativeDir(dir string) string {
	if dir == "" {
		return paths.NativeDir()
	}
	return dir
}

func configGetenv(getenv func(string) string) func(string) string {
	if getenv == nil {
		return os.Getenv
	}
	return getenv
}

// connectStep is which of the dialog's steps is up: the list, a key field,
// or — for a provider funded by signing in — the sign-in (plan 033 §3.13).
type connectStep int

const (
	connectPick connectStep = iota
	connectKey
	connectSignIn
	// The pre-session dialog's own steps (plan 036 §3.6, connect_pre.go):
	// a key being saved, the plan's models being fetched after a sign-in,
	// and the ChatGPT plan's one-time notice. None has a field.
	connectSaving
	connectFinishing
	connectNotice
)

// connectDialog is /connect's state. The zero value is no dialog, which is
// what closeDialog and withSession leave.
type connectDialog struct {
	// gen is this opening's number (Model.connSeq): the providers' read
	// carries it back (connectLoadedMsg), and applies only to it.
	gen  uint64
	step connectStep
	// pre says this is the pre-session dialog (plan 036 §3.6,
	// connect_pre.go), opened from a picker before any session; returnTo is
	// where it goes back to, and back what it says there, set by the way
	// out that has something to say (preExit). list is the session list as
	// the dialog closed it (decision 11), put back as every way out opens it
	// again; nil when it was opened from the startup picker. signed is its
	// sign-in once finished.
	pre      bool
	returnTo connectReturn
	back     connectBack
	list     *sessBackState
	signed   preSigned
	// loaded says the providers' read has answered: until then step one says
	// it is reading them. loadErr is its failure, value-free (the store's
	// errors name files and rules, never a key).
	loaded    bool
	loadErr   string
	providers []modeltable.ProviderInfo
	// file is where a key goes, as the hint shows it (~ for the home
	// directory).
	file string
	sel  int
	// Step two. field is the key field's own number (Model.connSeq), taken
	// each time the step opens, so a paste asked for in one field never lands
	// in the next (keyField). keyErr is the field's refusal, or "".
	//
	// Step three, the sign-in (connect_signin.go), has a field too — the
	// redirect address's, always drawn masked (signInField) — and uses the
	// same three: its run's number is its field's.
	field  uint64
	key    textinput.Model
	keyErr string
	// signIn is step three's run: the attempt, its addresses and the way to
	// end it. Every way out of the step ends it (signInState.end).
	signIn signInState
}

// keyField names one opening of the key field — or the sign-in's address
// field: the dialog's number and the field's. The zero value is no field —
// the composer's paste.
type keyField struct{ dialog, field uint64 }

// openField is the field that is open — step two's key field or step three's
// address field — or the zero keyField when neither is up.
func (d connectDialog) openField() keyField {
	if (d.step != connectKey && d.step != connectSignIn) || d.field == 0 {
		return keyField{}
	}
	return keyField{dialog: d.gen, field: d.field}
}

// provider is the provider step one's selection names, and whether there is
// one.
func (d connectDialog) provider() (modeltable.ProviderInfo, bool) {
	if d.sel < 0 || d.sel >= len(d.providers) {
		return modeltable.ProviderInfo{}, false
	}
	return d.providers[d.sel], true
}

// leaveKeyStep is Esc on the key field, or on the sign-in: back to step one,
// the field and its refusal gone with it, and the sign-in's attempt ended
// with its listener (plan 033 §3.13).
func (d connectDialog) leaveKeyStep() connectDialog {
	d.signIn.end(chatgptauth.CloseEsc)
	d.step, d.field, d.key, d.keyErr, d.signIn = connectPick, 0, textinput.Model{}, "", signInState{}
	return d
}

// dropConnect is /connect gone whatever is on screen: the dialog closed, if
// it is up, and its state — the key field with it — emptied (plan 031 §3.9,
// astra r4 1). It is the way out of the exits that are not the dialog's own,
// where the model, and a key typed or pasted into its field, would otherwise
// live on: a quit (requestQuit — Ctrl+D, the idle or second Ctrl+C, /exit, a
// served session's stopQuit — whose model waits out the session's stop and
// is the program's last), and the end of the session the dialog belongs to
// (endMsg — its own end or its connection lost — whether craze goes back to
// the session list, where nothing reaches the field behind it again, or
// quits).
func (m Model) dropConnect() Model {
	// A sign-in running in the box ends for the quit or the session's end
	// (CloseShutdown), before the box's own close would call it a dialog's.
	m.cdlg.signIn.end(chatgptauth.CloseShutdown)
	if m.dialog == dialogConnect {
		return m.closeDialog(false)
	}
	m.cdlg = connectDialog{}
	return m
}

// openConnect is /connect and the model dialog's connect row: any open
// dialog closes, and either the refusal is written (connectBusy) or step one
// opens and its providers are read off the Update.
func (m Model) openConnect() (Model, tea.Cmd) {
	m = m.closeDialog(true)
	if m.connectBusy() {
		m.addError(connectBusyText)
		return m, nil
	}
	m.connSeq++
	m.cdlg = connectDialog{gen: m.connSeq}
	m.dialog = dialogConnect
	return m.connectCall(readConnectProviders(m.connSeq, m.nativeDir, m.nativeEnv),
		connectLoadedMsg{gen: m.connSeq, err: "no answer in time"})
}

// connectCall runs read — a store read or write — as a gated call (plan 027
// §3.12): off the Update, in a tea.Cmd, with everything that arrives
// meanwhile held until its answer is applied (applyConnect) in the
// continuation. late is what is applied instead when no answer comes within
// the gate's deadline. A model with no backend — one a test built bare — has
// no gate to run it under, and runs it as a plain command.
//
// The pre-session dialog makes no backend call, whatever the model holds
// (plan 036 §3.6): its read and its save are plain commands too, each raced
// by late after a deadline of its own (preConnectCall).
func (m Model) connectCall(read tea.Cmd, late connectAnswer) (Model, tea.Cmd) {
	if m.cdlg.pre {
		return m, preConnectCall(read, late)
	}
	if m.eng == nil {
		return m, read
	}
	return m.run(gateDeadline,
		func(context.Context, backend.Backend) (any, error) { return read(), nil },
		func(m Model, r gateReply) (Model, tea.Cmd) {
			msg, ok := r.result.(connectAnswer)
			if !ok || r.err != nil {
				msg = late
			}
			return m.applyConnect(msg)
		})
}

// connectAnswer is every message /connect's commands answer with, and the
// connect row's (applyConnect): update has one case for all of them.
type connectAnswer interface{ connectAnswer() }

// connectLoadedMsg is the providers' read for the dialog numbered gen.
type connectLoadedMsg struct {
	gen   uint64
	infos []modeltable.ProviderInfo
	file  string
	err   string
}

// connectSavedMsg is a save's outcome: name is the provider's display name,
// err the refusal's text ("" when the key was stored), and notes the other
// providers' stored keys that cannot be used (brokenKeyNote). It carries no
// key. shownGen is the shown-session generation it was asked under: the
// notice is that conversation's, so one landing after a switch is dropped by
// the command gate (shownStamped). pre says it is the pre-session dialog's,
// numbered gen, whose save step alone takes it (preSaved).
type connectSavedMsg struct {
	shownGen uint64
	pre      bool
	gen      uint64
	name     string
	err      string
	notes    []string
}

// connectRowMsg says whether the model dialog numbered gen ends with the
// connect row: some provider has no usable key.
type connectRowMsg struct {
	gen  uint64
	show bool
}

func (connectLoadedMsg) connectAnswer() {}
func (connectSavedMsg) connectAnswer()  {}
func (connectRowMsg) connectAnswer()    {}

func (m connectSavedMsg) shownUnder() uint64 { return m.shownGen }

// readConnectProviders reads, off the Update, every provider a key can be
// given to in dir and how each is funded through getenv (modeltable.Providers),
// with the path the key would go to.
func readConnectProviders(gen uint64, dir string, getenv func(string) string) tea.Cmd {
	return func() tea.Msg {
		msg := connectLoadedMsg{gen: gen}
		if dir == "" {
			msg.err = "there is no craze directory to keep API keys in (set HOME or CRAZE_HOME)"
			return msg
		}
		msg.file = sessTilde(filepath.Clean(getenv("HOME")), filepath.Join(dir, modeltable.ProvidersFile))
		infos, err := modeltable.Providers(dir, getenv)
		if err != nil {
			msg.err = storeErrText(err, "")
			return msg
		}
		msg.infos = infos
		return msg
	}
}

// askConnectRow is the model dialog's question, on a native session: does its
// list end with the connect row? Asked off the Update (connectCall), through
// the TUI's own seams, and answered for the opening that asked
// (connectRowMsg). Nothing is asked on an ACP session, whose dialog never has
// the row, or with no native directory.
//
// refresh is the dialog opening with a session that takes up models while it
// runs (canRefreshModels; plan 034 §3.4): the session is asked to, in the same
// gated call and before the row's question, so a key saved or a plan signed in
// to since the session opened — by craze auth in another terminal, say — is in
// the list the box draws once the catalog delta lands (recompute re-renders
// the box). What the refresh came to is not said: the list is the answer, and
// a refresh that failed leaves the list as it was.
func (m Model) askConnectRow(refresh bool) (Model, tea.Cmd) {
	ask := m.dialog == dialogModel && m.connectOffered() && m.nativeDir != ""
	if !ask && !refresh {
		return m, nil
	}
	gen, dir, getenv := m.mdlg.gen, m.nativeDir, m.nativeEnv
	row := func() tea.Msg {
		infos, err := modeltable.Providers(dir, getenv)
		// A providers.toml that cannot be read says nothing about which
		// providers are connected: no row, and a typed /connect names the
		// problem.
		return connectRowMsg{gen: gen, show: err == nil && firstUnconnected(infos) >= 0}
	}
	if !refresh {
		return m.connectCall(row, connectRowMsg{gen: gen})
	}
	return m.run(gateDeadline,
		func(ctx context.Context, b backend.Backend) (any, error) {
			_, _ = refreshModelsCall(m.nativeDir)(ctx, b)
			if !ask {
				return nil, nil
			}
			return row(), nil
		},
		func(m Model, r gateReply) (Model, tea.Cmd) {
			if !ask {
				return m, nil
			}
			msg, ok := r.result.(connectAnswer)
			if !ok || r.err != nil {
				msg = connectRowMsg{gen: gen}
			}
			return m.applyConnect(msg)
		})
}

// firstUnconnected is the index of the first provider with no usable key —
// or, for the ChatGPT plan, no funded sign-in (ProviderInfo.Connected) — or
// -1.
func firstUnconnected(infos []modeltable.ProviderInfo) int {
	for i, p := range infos {
		if !p.Connected() {
			return i
		}
	}
	return -1
}

// saveConnectKey stores key as p's, off the Update. The key lives in this
// closure alone: the answer carries the provider's name and what came of it,
// and a refusal's text is checked for the key before it leaves (storeErrText).
func saveConnectKey(shown uint64, dir string, getenv func(string) string, p modeltable.ProviderInfo, key string) tea.Cmd {
	return func() tea.Msg {
		msg := connectSavedMsg{shownGen: shown, name: sanitizeLine(p.Name)}
		if err := modeltable.SetKey(dir, p.ID, key); err != nil {
			msg.err = storeErrText(err, key)
			return msg
		}
		// Every other provider's stored key is kept as it was, usable or
		// not (§3.7, r2-7); one that cannot be used is said, as craze auth
		// login says it after a login.
		if infos, err := modeltable.Providers(dir, getenv); err == nil {
			for _, o := range infos {
				if o.ID != p.ID && o.StoredProblem != nil {
					msg.notes = append(msg.notes, brokenKeyNote(o))
				}
			}
		}
		return msg
	}
}

// storeErrText is a store error as one line, without the package's prefix.
// The store never quotes a key (its errors name files and rules); should one
// ever, the line is replaced rather than shown.
func storeErrText(err error, key string) string {
	text := sanitizeLine(strings.TrimPrefix(err.Error(), "modeltable: "))
	if k := strings.TrimSpace(key); k != "" && strings.Contains(text, k) {
		return "the key was not saved"
	}
	return text
}

// brokenKeyNote says p's stored key cannot be used, by the rule and never
// the value, and how to replace or remove it — craze auth's note, with
// /connect as the replacement.
func brokenKeyNote(p modeltable.ProviderInfo) string {
	return fmt.Sprintf(`the stored %s key cannot be used: %s; replace it with /connect, or remove it with "craze auth logout %s"`,
		sanitizeLine(p.Name), storedProblemText(p.StoredProblem), sanitizeLine(p.ID))
}

// storedProblemText is why a stored key cannot be used, as a clause.
func storedProblemText(err error) string {
	if errors.Is(err, modeltable.ErrKeyTooShort) {
		return fmt.Sprintf("it is shorter than %d bytes", modeltable.MinKeyLen)
	}
	return "it overlaps craze's redaction marker"
}

// connectKeyRule is why the field's key cannot be stored, or "": the rules
// SetKey itself holds a key to (empty, too short, overlapping the redaction
// marker), plus craze auth's length bound, judged before anything is written
// so the refusal is the field's own line and nothing touches the disk.
func connectKeyRule(k string) string {
	switch {
	case k == "":
		return "Not saved: the key is empty."
	case len(k) > connectKeyMax:
		return fmt.Sprintf("Not saved: the key is longer than %d KiB.", connectKeyMax>>10)
	}
	switch err := modeltable.KeyProblem(k); {
	case errors.Is(err, modeltable.ErrKeyTooShort):
		return fmt.Sprintf("Not saved: a key is at least %d bytes.", modeltable.MinKeyLen)
	case err != nil:
		return "Not saved: the key overlaps craze's redaction marker."
	}
	return ""
}

// applyConnect is every connectAnswer applied: in a gated call's
// continuation (connectCall), or as update's case for one a plain command
// answered.
func (m Model) applyConnect(msg connectAnswer) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case connectLoadedMsg:
		if m.dialog != dialogConnect || m.cdlg.gen != msg.gen || m.cdlg.loaded {
			return m, nil
		}
		m.cdlg.loaded, m.cdlg.loadErr, m.cdlg.file = true, msg.err, msg.file
		m.cdlg.providers = msg.infos
		// The box opens on the first provider without a key — the likeliest
		// reason to be here — or on the first one when every one has one.
		m.cdlg.sel = max(firstUnconnected(msg.infos), 0)
	case connectSavedMsg:
		if msg.pre {
			// The pre-session dialog's (plan 036 §3.6): nothing of it is the
			// transcript's, and no session is asked to take the models up.
			return m.preSaved(msg)
		}
		if msg.err != "" {
			m.addError("/connect: " + msg.err)
			return m, nil
		}
		// The session is asked to take the key's models up (plan 034 §3.4),
		// and the notice says what came of it; the other providers' notes
		// follow it, as they always have.
		return m.thenRefreshModels(func(m Model, r *agent.ModelsRefresh) Model {
			m.addNote(connectedNote(msg.name, r))
			for _, n := range msg.notes {
				m.addNote(n)
			}
			return m
		})
	case connectRowMsg:
		if m.dialog == dialogModel && m.mdlg.gen == msg.gen {
			m.mdlg.connect = msg.show
		}
	case signInBegunMsg, signInDoneMsg, signInFinishedMsg, signInShownMsg, signInCopiedMsg, signInLogOffMsg:
		return m.applySignIn(msg)
	case preNoticeMarkedMsg:
		return m.preNoticeMarked(msg)
	}
	return m, nil
}

// pasteIntoKey is a clipboard paste asked for in a key field — or the
// sign-in's address field (pasteFromClipboard): into that field when it is
// still the one open, and dropped otherwise — the dialog gone, back on step
// one, another field opened since, or the sign-in's address already handed
// over.
func (m Model) pasteIntoKey(msg pasteMsg) Model {
	if msg.text == "" || m.dialog != dialogConnect || m.cdlg.openField() != msg.key || m.cdlg.signIn.handed {
		return m
	}
	m.cdlg.key, _ = m.cdlg.key.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(msg.text), Paste: true})
	m.cdlg.keyErr = ""
	return m
}

// handleConnectDialogKey is the dialog's keyboard. On step one ↑/↓ (and
// Tab/Shift+Tab) move, Enter opens the selected provider's key field — or,
// for the ChatGPT plan, its sign-in (pickConnectProvider) — and Esc closes the
// box; everything else — typing, a paste — is swallowed, since step one has no
// field and a key pasted there must land nowhere.
//
// The pre-session dialog's own steps (plan 036 §3.6: saving, finishing, the
// notice) have keys of their own (handlePreStepKey); its Esc, on any step,
// never reaches here (handlePreConnectKey).
func (m Model) handleConnectDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.cdlg.step {
	case connectKey:
		return m.handleConnectKeyStep(msg)
	case connectSignIn:
		return m.handleSignInKey(msg)
	case connectSaving, connectFinishing, connectNotice:
		return m.handlePreStepKey(msg)
	}
	n := len(m.cdlg.providers)
	switch msg.Type {
	case tea.KeyEsc:
		return m.closeDialog(true), nil
	case tea.KeyEnter:
		return m.pickConnectProvider()
	case tea.KeyUp, tea.KeyShiftTab:
		if n > 0 {
			m.cdlg.sel = (m.cdlg.sel - 1 + n) % n
		}
	case tea.KeyDown, tea.KeyTab:
		if n > 0 {
			m.cdlg.sel = (m.cdlg.sel + 1) % n
		}
	}
	return m, nil
}

// openKeyStep is step two for the selected provider, with a fresh, masked,
// focused field under a number of its own. Nothing happens until the
// providers have been read.
func (m Model) openKeyStep() Model {
	if _, ok := m.cdlg.provider(); !ok || !m.cdlg.loaded {
		return m
	}
	ti := m.dialogInput()
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = connectMask
	// bubbles' own Ctrl+V reads the clipboard with no seam in front of it;
	// craze reads it itself, tagged with this field (pasteFromClipboard).
	ti.KeyMap.Paste.SetEnabled(false)
	m.connSeq++
	m.cdlg.step, m.cdlg.field, m.cdlg.key, m.cdlg.keyErr = connectKey, m.connSeq, ti, ""
	return m
}

// handleConnectKeyStep is the key field's keyboard: Enter saves, Esc goes
// back to step one, Ctrl+V pastes the clipboard into this field and no
// other, and every other key — a terminal's bracketed paste among them — is
// the field's.
func (m Model) handleConnectKeyStep(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.cdlg = m.cdlg.leaveKeyStep()
		return m, nil
	case tea.KeyEnter:
		return m.saveConnect()
	case tea.KeyCtrlV:
		return m, pasteFromClipboardForKey(m.shownGen, m.cdlg.openField())
	}
	before := m.cdlg.key.Value()
	var cmd tea.Cmd
	m.cdlg.key, cmd = m.cdlg.key.Update(msg)
	if m.cdlg.key.Value() != before {
		m.cdlg.keyErr = ""
	}
	return m, cmd
}

// saveConnect is Enter on the key field. Work that started while the box was
// open refuses it, the key kept for a later Enter; a key the store would not
// take is refused in the field, which is emptied, and nothing is written.
// Otherwise the box closes — the field with it — and the key is stored off
// the Update (saveConnectKey), whose answer writes the notice.
//
// The pre-session dialog stays up instead (plan 036 §3.6), on its save step
// until the store answers or the deadline does (preSaved), since where it
// goes back to has to hear what came of it; the field is emptied all the
// same, the key living in the save's closure alone.
func (m Model) saveConnect() (tea.Model, tea.Cmd) {
	p, ok := m.cdlg.provider()
	if !ok {
		return m, nil
	}
	if m.connectBusy() {
		m.cdlg.keyErr = connectBusySaveText
		return m, nil
	}
	k := strings.TrimSpace(m.cdlg.key.Value())
	if why := connectKeyRule(k); why != "" {
		m.cdlg.key.Reset()
		m.cdlg.keyErr = why
		return m, nil
	}
	dir, getenv, shown := m.nativeDir, m.nativeEnv, m.shownGen
	if m.cdlg.pre {
		gen, name := m.cdlg.gen, sanitizeLine(p.Name)
		m.cdlg.step, m.cdlg.field, m.cdlg.key, m.cdlg.keyErr = connectSaving, 0, textinput.Model{}, ""
		return m.connectCall(preStampedSave(saveConnectKey(shown, dir, getenv, p, k), gen),
			connectSavedMsg{shownGen: shown, pre: true, gen: gen, name: name, err: preSaveLateText})
	}
	m = m.closeDialog(false)
	// The late answer is only ever applied in the gate's continuation, which
	// the gate fences to this session, and applyConnect reads a refusal's err
	// alone.
	return m.connectCall(saveConnectKey(shown, dir, getenv, p, k), connectSavedMsg{
		err: "the key store did not answer in time; craze auth list says whether the key was saved"})
}

// connectPickNotice is what step one shows in place of the list, or nil when
// the list is shown: the read still out, its failure, or nothing to connect.
func (m Model) connectPickNotice(inner int) (lines []string, failed bool) {
	switch {
	case !m.cdlg.loaded:
		return []string{"Reading the providers…"}, false
	case m.cdlg.loadErr != "":
		return dialogWrap("The providers could not be read: "+sanitizeLine(m.cdlg.loadErr), inner), true
	case len(m.cdlg.providers) == 0:
		return []string{"There is no provider to connect."}, false
	}
	return nil, false
}

// connectPickPlan is step one at an inner size: the notice in place of the
// list, if any (connectPickNotice), the window onto the list or the notice's
// rows, and whether the footer survived — the title never goes, the footer
// only once even one row no longer fits. The renderer and the hit-tester both
// take it.
type connectPickPlan struct {
	notice     []string
	failed     bool
	top, shown int
	footer     bool
}

func (m Model) connectPickPlan(inner, budget int) connectPickPlan {
	var p connectPickPlan
	rows := budget - 1 // the title
	p.footer = rows >= 2
	if p.footer {
		rows--
	}
	n := len(m.cdlg.providers)
	if p.notice, p.failed = m.connectPickNotice(inner); p.notice != nil {
		n = len(p.notice)
	}
	p.top, p.shown = dialogListWindow(n, m.cdlg.sel, max(rows, 0))
	return p
}

// connectDialogBody draws the step that is up.
func (m Model) connectDialogBody(inner, budget int) []string {
	switch m.cdlg.step {
	case connectKey:
		return m.connectKeyBody(inner, budget)
	case connectSignIn:
		return m.signInBody(inner, budget)
	case connectSaving, connectFinishing, connectNotice:
		return m.preStepBody(inner, budget)
	}
	plan := m.connectPickPlan(inner, budget)
	rows := []string{m.dialogTitle(connectDialogTitle, inner)}
	if plan.notice != nil {
		st := styleFG(m.theme.Dim)
		if plan.failed {
			st = styleFG(m.theme.Err)
		}
		for _, line := range plan.notice[plan.top : plan.top+plan.shown] {
			rows = append(rows, st.Render(clampWidth(line, inner)))
		}
	} else {
		for i := plan.top; i < plan.top+plan.shown; i++ {
			p := m.cdlg.providers[i]
			rows = append(rows, m.dialogRow(p.Name, connectMark(p), i == m.cdlg.sel, true, inner))
		}
	}
	if plan.footer {
		rows = append(rows, m.dialogFooter(connectPickHint, inner))
	}
	return rows
}

// connectMark is a step-one row's tag: ✓ for a provider with a usable key,
// from a variable or stored, or a funded sign-in (ProviderInfo.Connected);
// for a provider funded by signing in, "plan usage off" when its account did
// not grant plan usage (plan 033 X129) — a key stored for it by hand funds
// nothing, so it is never "stored key unusable"; for any other, the word for
// one whose stored key cannot be used and that nothing else funds; nothing
// for the rest.
func connectMark(p modeltable.ProviderInfo) string {
	switch {
	case p.Connected():
		return connectConnectedMark
	case p.SignIn && p.Via == modeltable.KeyPlanDisabled:
		return connectPlanOffMark
	case p.SignIn:
		return ""
	case p.StoredProblem != nil:
		return connectUnusableMark
	}
	return ""
}

// connectKeyHints are the key field's hint lines, wrapped: where the key goes,
// then — when they apply — that a variable funds the provider ahead of the
// stored key, and that the key stored now cannot be used.
func (m Model) connectKeyHints(p modeltable.ProviderInfo, inner int) []string {
	var out []string
	if m.cdlg.file != "" {
		out = append(out, dialogWrap("Stored in "+sanitizeLine(m.cdlg.file)+".", inner)...)
	}
	if p.EnvVar != "" {
		out = append(out, dialogWrap(sanitizeLine(p.EnvVar)+" is set in this environment; craze uses it before the stored key.", inner)...)
	}
	if p.StoredProblem != nil {
		out = append(out, dialogWrap("The stored key cannot be used: "+storedProblemText(p.StoredProblem)+". This one replaces it.", inner)...)
	}
	return out
}

// connectKeyBody is step two: the title, the masked field, the refusal under
// it, the hints and the footer. A short box drops the hints first, from the
// last, then the footer; the title, the field and the refusal stay.
func (m Model) connectKeyBody(inner, budget int) []string {
	p, _ := m.cdlg.provider()
	rows := []string{m.dialogTitle(sanitizeLine(p.Name)+connectKeyTitle, inner), dialogInputView(m.cdlg.key, inner)}
	room := budget - len(rows)
	if m.cdlg.keyErr != "" {
		errs := dialogWrap(m.cdlg.keyErr, inner)
		errs = errs[:min(len(errs), max(room, 0))]
		for _, e := range errs {
			rows = append(rows, styleFG(m.theme.Err).Render(clampWidth(e, inner)))
		}
		room -= len(errs)
	}
	footer := room >= 1
	if footer {
		room--
	}
	hints := m.connectKeyHints(p, inner)
	for _, h := range hints[:min(len(hints), max(room, 0))] {
		rows = append(rows, styleFG(m.theme.Dim).Render(clampWidth(h, inner)))
	}
	if footer {
		rows = append(rows, m.dialogFooter(connectKeyHint, inner))
	}
	return rows
}

// connectDialogClick is a press on a body row: on step one a provider's row
// opens its key field, or its sign-in, as Enter on it does; on the sign-in, a
// row of the address copies it (signInClick, plan 034 Q10); nothing else in
// the box acts.
func (m Model) connectDialogClick(i int) (tea.Model, tea.Cmd) {
	switch m.cdlg.step {
	case connectSignIn:
		return m.signInClick(i)
	case connectKey, connectSaving, connectFinishing, connectNotice:
		return m, nil
	}
	plan := m.connectPickPlan(m.lay.Dialog.W-dialogBorder, m.lay.Dialog.H-dialogBorder)
	row := i - 1 // the title row
	if plan.notice != nil || row < 0 || row >= plan.shown {
		return m, nil
	}
	m.cdlg.sel = plan.top + row
	return m.pickConnectProvider()
}
