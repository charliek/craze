package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/chatgptauth"
)

// The pre-session connect dialog (plan 036 §3.6): /connect's dialog opened
// from a picker before any session — native chosen while no model provider
// has a key, which a session could not start on (nothingFundedText). The
// startup picker opens it (confirmProvider: Enter on native, or Esc or a
// click outside the box when native is the default), and so does the session
// list's /provider (Enter or Tab on native, or a typed `/provider native`).
// It is the same dialog — its steps, its masked fields, its paste safety, its
// sign-in — in a mode of its own (connectDialog.pre), with a way back
// (connectDialog.returnTo):
//
//   - No backend call at all, whatever m.eng holds — the list may have been
//     opened from inside a live session. The providers' read and the save run
//     as plain commands, each raced by a deadline of its own (preConnectCall),
//     never as gated calls; nothing asks a session to take up models
//     (thenRefreshModels), and no busy rule refuses it (connectBusy): it is
//     no session's /connect, as craze auth login in another terminal is not.
//   - Nothing is written to the transcript. What the dialog comes to — a
//     key saved, a sign-in finished, a failure — goes to where it goes back
//     to (connectBack): the picker's detail lines, or the list's hint line;
//     in words that name no session (no "/exit and run craze -c").
//   - It stays up until the outcome is in: a save shows its step until the
//     store answers (connectSaving), a sign-in fetches the plan's models
//     before it goes back (connectFinishing), and the ChatGPT plan's one-time
//     notice is a step of its own that only Enter leaves forward
//     (connectNotice) — the notice is recorded as shown on that Enter and on
//     no other way out (markNoticeShown).
//   - Every way out lands in closeDialog, which takes the way back
//     (connectReturned): the picker under the dialog, its states asked again;
//     or the list, which the dialog closed as it opened (decision 11) and
//     which opens again with its input, selection and provider as they were
//     (sessBackState). Esc, on any step, is a way out — the dialog has one
//     purpose, and its "back" is where it came from — and so is a click
//     outside the box, which never starts the picker's default.
//   - A late answer — a read, a save, a step of the sign-in — is taken only
//     by the opening, and the step, it was asked in (the dialog's number and
//     the run's, as every /connect answer is): one that lands after the
//     dialog is gone is dropped.
//   - Ctrl+C and Ctrl+D quit, as they quit the picker or the list it came
//     from (preConnectQuit): the sign-in, if one runs, is ended first, and
//     finishRun waits for its outcome before it closes the sign-in log.

// connectReturn is where the pre-session connect dialog goes back to.
type connectReturn int

const (
	// returnPicker: the startup provider picker, which stays up under it.
	returnPicker connectReturn = iota + 1
	// returnList: the session list, which it closed as it opened.
	returnList
)

// connectBack is what the pre-session dialog came to, for the picker's detail
// lines (Model.providerBack) or the list's hint line: one line of text, which
// never holds a key, and how it reads there — done (sessNoteOK), done with a
// caveat (sessNoteWarn) or failed (sessNoteErr), coloured as the hint line
// colours its notes (sessNoteStyle); the zero value says nothing.
type connectBack struct {
	text string
	kind sessNoteKind
}

// preSigned is the pre-session dialog's sign-in once it has finished (plan
// 036 §3.6): the account's email, whether the one-time notice is owed, and —
// the plan's models fetched — what the dialog will say where it goes back,
// held while the notice step is up; marking says the notice's Enter is being
// recorded.
type preSigned struct {
	email   string
	notice  bool
	text    string
	marking bool
}

// The pre-session dialog's words.
const (
	// preConnectedText is a key saved: the provider it was for can be
	// picked — a native session started now offers its models.
	preConnectedText = "connected %s: it can be picked now"
	// preSaveFailedText is a save the store refused, or that did not answer.
	preSaveFailedText = "could not connect %s: %s"
	preSaveLateText   = "the key store did not answer in time; craze auth list says whether the key was saved"
	// preSavingText is the key's step while the store is written.
	preSavingText = "Saving the key…"
	// The sign-in's: one that could not begin, one that failed; signed in,
	// with or without plan usage; the plan's models being fetched.
	preSignInStartText = "the sign-in could not start: "
	preSignInFailText  = "the sign-in failed: "
	preSignedInHead    = "signed in to ChatGPT"
	preSignedInTail    = ": it can be picked now"
	prePlanOffTail     = ", but ChatGPT plan usage is off: sign in again and allow ChatGPT plan usage when ChatGPT asks"
	preFinishingText   = "Fetching the plan's models…"
	preNoticeUnmarked  = "; the notice could not be recorded as shown, so it may be shown again: "
	// The footers: every step's Esc goes back; the notice's Enter goes on.
	preBackHint   = "esc back"
	preNoticeHint = "enter continues · esc back"
)

// preConnectDeadline bounds the pre-session dialog's read and save as the
// gate's deadline bounds a gated call's (gateDeadline): a variable only so a
// test can run the race to its end.
var preConnectDeadline = gateDeadline

// preConnectOpen says the pre-session connect dialog is up.
func (m Model) preConnectOpen() bool { return m.dialog == dialogConnect && m.cdlg.pre }

// openPreConnect opens the pre-session dialog, going back to to: step one, its
// providers read off the Update as a plain command (connectCall). The picker
// stays up under it (pickingProvider), and nothing is built; a list was
// closed by the caller (connectOverSessions).
//
// Over the picker, the request for its states still out — the last way
// back's (reaskProviderAvail), say, its answer not in yet — is given up: the
// store may change under this dialog (a sign-in without plan usage removes
// an earlier one's tokens), so an answer read before it opened must not land
// while it is up, nor after it, and draw a native ready that can no longer
// start (plan 036 r4). The picker keeps the states it opened this dialog on
// (native needing setup) until this dialog's own way back asks again.
func (m Model) openPreConnect(to connectReturn) (Model, tea.Cmd) {
	if to == returnPicker {
		m.availSeq, m.availNative = provAvailSeq.Add(1), 0
	}
	m.connSeq++
	m.cdlg = connectDialog{gen: m.connSeq, pre: true, returnTo: to}
	m.dialog = dialogConnect
	return m.connectCall(readConnectProviders(m.connSeq, m.nativeDir, m.nativeEnv),
		connectLoadedMsg{gen: m.connSeq, err: "no answer in time"})
}

// preConnectCall is the pre-session dialog's read or save (connectCall): the
// plain command, raced by late after preConnectDeadline (preConnectLate) —
// whichever lands first is taken by the step that asked, and the other is
// dropped (its step has moved on, or the dialog has gone).
func preConnectCall(read tea.Cmd, late connectAnswer) tea.Cmd {
	return tea.Batch(read, preConnectLate(late))
}

// preConnectLate is the deadline a pre-session read or save is raced by: late,
// once preConnectDeadline has passed — a timer, as tea.Tick's is.
func preConnectLate(late connectAnswer) tea.Cmd {
	d := preConnectDeadline
	return func() tea.Msg {
		timer := time.NewTimer(d)
		defer timer.Stop()
		<-timer.C
		return late
	}
}

// preStampedSave is a save's command whose answer is the pre-session dialog
// numbered gen's (preSaved).
func preStampedSave(save tea.Cmd, gen uint64) tea.Cmd {
	return func() tea.Msg {
		msg, _ := save().(connectSavedMsg)
		msg.pre, msg.gen = true, gen
		return msg
	}
}

// preExit is the pre-session dialog's way out with back to say where it goes
// back: the dialog closes (closeDialog), and the way back is taken there.
func (m Model) preExit(back connectBack) Model {
	m.cdlg.back = back
	return m.closeDialog(false)
}

// connectReturned is the pre-session dialog's way back, from closeDialog,
// which has just taken the dialog down: to the picker, with back on its
// detail lines and its states asked again within its opening
// (reaskProviderAvail) — native's recomputed from the store, never assumed —
// the answer moving the cursor onto native when native is ready now (X21);
// or to the list, opened again as list recorded it (reopenSessions), with
// back on its hint line and its states read afresh as any opening reads them.
// The commands this leaves are the Update wrapper's to hand on (backCmd).
func (m Model) connectReturned(to connectReturn, back connectBack, list *sessBackState) Model {
	switch to {
	case returnPicker:
		if !m.pickingProvider {
			return m
		}
		m.dialog = dialogProvider
		m.providerErr, m.providerBack, m.availNative = "", back, 0
		if ask := m.reaskProviderAvail(); ask != nil {
			m.availNative = m.availSeq
			m.backCmd = tea.Batch(m.backCmd, ask)
		}
	case returnList:
		m = m.reopenSessions(back, list)
	}
	return m
}

// handlePreConnectKey is a key while the pre-session dialog is up (handleKey,
// ahead of everything but the list, which it closed): Ctrl+C and Ctrl+D quit
// (preConnectQuit); Esc, on any step, is the way out — the dialog has one
// purpose, and its "back" is where it came from — and every other key is the
// dialog's (handleConnectDialogKey).
func (m Model) handlePreConnectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyCtrlD:
		return m.preConnectQuit()
	case tea.KeyEsc:
		return m.closeDialog(true), nil
	}
	return m.handleConnectDialogKey(msg)
}

// preConnectQuit is Ctrl+C or Ctrl+D on the pre-session dialog: craze quits
// as it would from where the dialog came — the picker's quit (requestQuit,
// with no session to stop), or the list's (sessQuit: every session keeps
// running, the one behind the list detached, never stopped and never its turn
// cancelled, as the session's own Ctrl+C would). One press, as the picker's
// Ctrl+C and every dialog's are. The dialog goes first with no way back
// (dropConnect): a sign-in running in it is ended for the shutdown, and
// finishRun waits for its outcome before it flushes and closes the sign-in
// log (plan 034 X79).
func (m Model) preConnectQuit() (tea.Model, tea.Cmd) {
	to := m.cdlg.returnTo
	m.cdlg.returnTo = 0
	m = m.dropConnect()
	if to == returnList {
		return m.sessQuit()
	}
	return m.requestQuit()
}

// preSaved is a save's answer for the pre-session dialog: taken only by the
// opening that asked, while its save step is up — the first of the store's
// answer and the deadline's — and the way out with it: `connected <name>`,
// with any other provider's stored key that cannot be used after it, or why
// not.
func (m Model) preSaved(msg connectSavedMsg) (Model, tea.Cmd) {
	if !m.preConnectOpen() || m.cdlg.gen != msg.gen || m.cdlg.step != connectSaving {
		return m, nil
	}
	if msg.err != "" {
		return m.preExit(connectBack{text: fmt.Sprintf(preSaveFailedText, msg.name, msg.err), kind: sessNoteErr}), nil
	}
	text := fmt.Sprintf(preConnectedText, msg.name)
	for _, n := range msg.notes {
		text += "; " + n
	}
	return m.preExit(connectBack{text: text, kind: sessNoteOK}), nil
}

// preSignInDone is a pre-session sign-in's wait (signInDoneMsg): taken only by
// the step that began it, while it is up — one whose step has gone, Esc or
// the dialog closed, is dropped, its run ended already. A failure is the way
// out with why; a sign-in without plan usage is too, with what to do, native
// still needing setup; one with plan usage fetches the plan's models first,
// the dialog up meanwhile (connectFinishing), and the notice is not recorded
// there — only its own step's Enter records it (preNoticeEnter).
func (m Model) preSignInDone(msg signInDoneMsg) (Model, tea.Cmd) {
	if !m.signInOpen(msg.gen, msg.run) {
		return m, nil
	}
	m.cdlg.signIn.end(chatgptauth.CloseDone)
	if msg.err != nil {
		return m.preExit(connectBack{text: preSignInFailureText(msg.err), kind: sessNoteErr}), nil
	}
	email := sanitizeLine(msg.res.Email)
	if !msg.res.PlanUsage {
		return m.preExit(connectBack{text: signedInAs(preSignedInHead, email) + prePlanOffTail, kind: sessNoteWarn}), nil
	}
	run := m.cdlg.field
	m.cdlg.step, m.cdlg.key, m.cdlg.keyErr, m.cdlg.signIn = connectFinishing, textinput.Model{}, "", signInState{}
	m.cdlg.signed = preSigned{email: email, notice: msg.res.ShowNotice}
	return m, finishSignInCmd(m.signIns, m.shownGen, m.nativeDir, false, msg.observe, signInStamp{gen: m.cdlg.gen, run: run, pre: true})
}

// preSignInFinished is the work after a pre-session sign-in (signInFinishedMsg:
// the plan's models fetched, or why not), for the step that is waiting on it:
// the notice step when the notice is owed, else the way out.
func (m Model) preSignInFinished(msg signInFinishedMsg) (Model, tea.Cmd) {
	if !m.preConnectOpen() || m.cdlg.gen != msg.gen || m.cdlg.step != connectFinishing || m.cdlg.field != msg.run {
		return m, nil
	}
	text := signedInAs(preSignedInHead, m.cdlg.signed.email) + preSignedInTail +
		"; " + planModelsText(msg, "no ChatGPT plan models are listed for this account")
	if m.cdlg.signed.notice {
		m.cdlg.step, m.cdlg.signed.text = connectNotice, text
		return m, nil
	}
	return m.preExit(connectBack{text: text, kind: sessNoteOK}), nil
}

// preNoticeMarkedMsg is the notice's Enter recorded (markNoticeShown), for the
// pre-session dialog numbered gen.
type preNoticeMarkedMsg struct {
	gen uint64
	err error
}

func (preNoticeMarkedMsg) connectAnswer() {}

// preNoticeEnter is Enter on the notice step: the notice is recorded as shown
// — now, on this Enter, and on no other way out — off the Update, and the way
// out follows once it is (preNoticeMarked). A second Enter meanwhile does
// nothing.
func (m Model) preNoticeEnter() (Model, tea.Cmd) {
	if m.cdlg.signed.marking {
		return m, nil
	}
	m.cdlg.signed.marking = true
	gen, dir, mark := m.cdlg.gen, m.nativeDir, markNoticeShown
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), signInFinishTimeout)
		defer cancel()
		return preNoticeMarkedMsg{gen: gen, err: mark(ctx, dir)}
	}
}

// preNoticeMarked is the notice's record, for the step that asked: the way
// out, saying the record failed when it did.
func (m Model) preNoticeMarked(msg preNoticeMarkedMsg) (Model, tea.Cmd) {
	if !m.preConnectOpen() || m.cdlg.gen != msg.gen || m.cdlg.step != connectNotice || !m.cdlg.signed.marking {
		return m, nil
	}
	back := connectBack{text: m.cdlg.signed.text, kind: sessNoteOK}
	if msg.err != nil {
		back.text += preNoticeUnmarked + signInErrorText(msg.err)
		back.kind = sessNoteWarn
	}
	return m.preExit(back), nil
}

// preSignInFailureText is why a pre-session sign-in failed: declined in the
// browser in craze auth's words, anything else as the attempt said it.
func preSignInFailureText(err error) string {
	if text := signInFailureText(err); text != signInErrorText(err) {
		return text
	}
	return preSignInFailText + signInErrorText(err)
}

// handlePreStepKey is the keyboard of the pre-session dialog's own steps —
// saving, finishing, the notice — whose Esc handlePreConnectKey took
// already: Enter on the notice goes on; every other key, a paste among them,
// lands nowhere, since none of them has a field.
func (m Model) handlePreStepKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.cdlg.step == connectNotice && msg.Type == tea.KeyEnter {
		return m.preNoticeEnter()
	}
	return m, nil
}

// preStepBody draws the pre-session dialog's own steps: the title; what is
// happening — the key being saved, the plan's models being fetched — or the
// notice; and the footer. A short box gives the text up from its last row,
// then the footer; the title stays.
func (m Model) preStepBody(inner, budget int) []string {
	var title, footer string
	var lines []string
	st := styleFG(m.theme.Dim)
	switch m.cdlg.step {
	case connectSaving:
		p, _ := m.cdlg.provider()
		title, footer, lines = sanitizeLine(p.Name)+connectKeyTitle, preBackHint, []string{preSavingText}
	case connectFinishing:
		who := signedInAs(signedInPrefix, m.cdlg.signed.email)
		title, footer, lines = connectSignInTitle, preBackHint, dialogWrap(who+". "+preFinishingText, inner)
	case connectNotice:
		title, footer, lines = chatgptauth.NoticeTitle, preNoticeHint, dialogWrap(chatgptauth.Notice, inner)
		st = styleFG(m.theme.FG)
	}
	rows := []string{m.dialogTitle(title, inner)}
	room := budget - 1
	hasFooter := room >= 2
	if hasFooter {
		room--
	}
	for _, l := range lines[:min(len(lines), max(room, 0))] {
		rows = append(rows, st.Render(clampWidth(l, inner)))
	}
	if hasFooter {
		rows = append(rows, m.dialogFooter(footer, inner))
	}
	return rows
}
