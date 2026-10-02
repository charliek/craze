package tui

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// /connect's third step (plan 033 §3.13, owner decision 10): Sign in with
// ChatGPT, in the TUI. A provider funded by signing in rather than by a key —
// the ChatGPT plan's (modeltable.ProviderInfo.SignIn) — has no key field:
// Enter on its row in step one opens "Sign in with ChatGPT" instead, which
// runs the whole sign-in craze auth login chatgpt runs (chatgptauth.Begin),
// into this TUI's native directory.
//
//   - The step shows the authorization address, and Ctrl+Y copies it through
//     the TUI's own copy (copyText: OSC 52, then the native tool), so a person
//     on another machine — over SSH, say — can open it in their own browser.
//     It carries no token: craze never sends id_token_hint (P21).
//   - The wait is a plain tea.Cmd under a context the step owns, never a gated
//     call: a gated call has the gate's 15-second deadline and holds every key
//     typed meanwhile (gate.go, gateDeadline), and a browser's approval takes
//     as long as the person does. So is the begin (the host id under its lock,
//     the listener), so that an attempt the gate gave up on, or dropped across
//     a switch, can never be left listening: an answer that finds its step gone
//     closes the attempt it carries (applySignIn).
//   - The browser comes back to the attempt's loopback listener, or — when the
//     browser is on another machine, where that page cannot load — its address
//     is pasted into the step's field, which is not masked: the address carries
//     a one-time code and the attempt's state, never a token. The listener and
//     the paste race inside the attempt, and whichever is accepted first wins
//     (chatgptauth.Attempt). A line that is not an address — a key pasted out of
//     habit — is refused before the attempt sees it, never quoted, and the field
//     is emptied, as is one the attempt refuses.
//   - Every way out of the step ends the attempt and closes its listener with
//     every connection to it (signInState.end): Esc, which goes back to step
//     one; the dialog closing for any reason (closeDialog); the dialog dropped
//     on a quit or the session's end (dropConnect); and a switch to another
//     session (withSession).
//   - Signed in, the box closes and the transcript says as whom; the first
//     time a registration signs in with plan usage, the one-time notice follows
//     (chatgptauth.NoticeTitle and Notice, craze auth's texts) and is recorded
//     as shown (MarkNoticeShown) only once it is on screen. Then, off the
//     Update, the plan's models are fetched into the TUI's directory
//     (chatgptauth.FetchModels) — which also has this process's token source
//     take the new tokens, so an in-process session redacts them at once —
//     and the transcript names them. An account that did not grant plan usage
//     is told so and how to grant it, craze auth's explanation with /connect.
//   - Mid-session (plan 031's rule, P8): the running session keeps the model
//     table it started with, so the plan's models are offered by new sessions,
//     and by this conversation after /exit and craze -c, which the last note
//     says. The TUI keeps no model list of its own to refresh: the session
//     list's /provider and /model and the /model dialog's connect row read the
//     directory each time they open, so each already sees the sign-in.
//   - It writes to this TUI's CRAZE_HOME (Model.nativeDir, R2): the directory a
//     host under that home reads. A session attached from a shell with another
//     CRAZE_HOME reads its own.
//   - Refused while work runs, as a key is (connectBusy): when the step would
//     open, and at a pasted address's Enter, where the field keeps the address
//     for a later Enter. A browser's redirect to the listener is not asked;
//     the refusal narrows the window, as the key's does (plan 031 R1). A
//     running session learns the new tokens to redact them at its next turn's
//     start (plan 033 §3.12), or at once in this process (above).

// The step's texts.
const (
	// connectSignInTitle is step three's title, the ChatGPT plan's action.
	connectSignInTitle = "Sign in with ChatGPT"
	// connectSignInIntro introduces the address.
	connectSignInIntro = "Open this address in a browser and approve craze:"
	// connectSignInStarting is the step until the attempt has begun.
	connectSignInStarting = "Starting the sign-in…"
	// connectSignInWaiting is the hint under the address while a listener
	// waits for the browser.
	connectSignInWaiting = "Waiting for the browser. If it is on another machine, paste the address it lands on:"
	// connectSignInHanded replaces the hint and the field once a pasted
	// address was accepted: the exchange is under way.
	connectSignInHanded = "Signing in…"
	// The footers: while the attempt begins, while it waits, and once the
	// address is handed over.
	connectSignInStartHint  = "esc back"
	connectSignInWaitHint   = "ctrl+y copies the address · enter · esc back"
	connectSignInHandedHint = "esc cancels"
	// connectSignInCopied is the status row's note after Ctrl+Y.
	connectSignInCopied = "copied the sign-in address"
	// connectBusySignInText is a pasted address's Enter refused because work
	// started while the box was open. The address stays in the field.
	connectBusySignInText = "Finish or stop the running work first, then press enter to sign in."
	// connectPlanOffMark tags, in step one, a provider signed in to by an
	// account that did not grant plan usage (modeltable.KeyPlanDisabled).
	connectPlanOffMark = "plan usage off"
	// connectRedirectMax is the longest line the field takes as a pasted
	// address, as craze auth reads at most 16 KiB: a real one is a few
	// hundred bytes, and a longer line is not one.
	connectRedirectMax = 16 << 10
	// signInFinishTimeout bounds the work after a sign-in: the notice
	// recorded and the model list fetched.
	signInFinishTimeout = time.Minute
)

// The texts the transcript gets once a sign-in is done: craze auth login
// chatgpt's (C15), with /connect as the way to run it again.
const (
	signedInPrefix   = "Signed in to ChatGPT"
	planUsageOffText = ", but ChatGPT plan usage is off: the account did not allow craze to use its ChatGPT plan, so the plan's models cannot be used."
	planUsageOffHow  = "To turn it on, sign in again with /connect and allow ChatGPT plan usage when ChatGPT asks."
	// signedInSessionNote is what a sign-in means for this conversation
	// (plan 031's rule, P8): the connectedNote of a key's save, for the plan.
	signedInSessionNote = "New sessions offer the ChatGPT plan's models; to use them in this conversation, /exit and run craze -c."
)

// signInAttempt is the part of a chatgptauth.Attempt the step drives: a seam,
// as craze auth's is (internal/cli), so the tests can stand in for the browser
// and OpenAI, and the step's own work — its frames, the race, Esc — is tested
// apart from the sign-in's, which chatgptauth's tests cover.
type signInAttempt interface {
	URL() string
	RedirectURI() string
	Listening() bool
	Paste(raw string) error
	Wait(ctx context.Context) (chatgptauth.Result, error)
	Close()
}

// The step's seams: chatgptauth in craze; stand-ins in this package's tests,
// which must never reach OpenAI. Each is read on the Update goroutine and
// handed to its command (sendCopy says why).
var (
	beginSignIn = func(ctx context.Context, dir string) (signInAttempt, error) {
		a, err := chatgptauth.Begin(ctx, dir, chatgptauth.BeginOptions{})
		if err != nil {
			return nil, err
		}
		return a, nil
	}
	fetchPlanModels = func(ctx context.Context, dir string) (*chatgptauth.Models, error) {
		return chatgptauth.FetchModels(ctx, chatgptauth.Source(dir))
	}
	markNoticeShown = chatgptauth.MarkNoticeShown
)

// signInState is step three's own state (connectDialog.signIn): the zero value
// is no sign-in, which every other step has.
type signInState struct {
	// cancel ends the run: the context the begin and the wait run under,
	// whose end closes the attempt's listener (chatgptauth.Attempt.Wait).
	cancel context.CancelFunc
	// att is the attempt, once it has begun; url and redirect are its
	// authorization and redirect addresses, listening whether a loopback
	// listener waits for the browser.
	att       signInAttempt
	url       string
	redirect  string
	listening bool
	// handed says a pasted address was accepted: the attempt is finishing,
	// and the field is gone.
	handed bool
}

// end ends the run, if there is one: the context cancelled, and the attempt —
// if it has begun — closed with its listener and every connection to it. It
// is safe to call more than once, and on a copy: the copies share the run.
func (s signInState) end() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.att != nil {
		s.att.Close()
	}
}

// signInBegunMsg is a run's begin, for the step numbered run of the dialog
// numbered gen: the attempt and the wait to run over it, or why it could not
// begin. It is never dropped unread (it is no shownStamped): applySignIn
// closes an attempt whose step is gone.
type signInBegunMsg struct {
	gen, run uint64
	att      signInAttempt
	wait     tea.Cmd
	err      error
}

// signInDoneMsg is a run's wait: the sign-in's result, or why it ended. It
// carries the account's email, never a token.
type signInDoneMsg struct {
	gen, run uint64
	res      chatgptauth.Result
	err      error
}

// signInFinishedMsg is the work after a sign-in, off the Update: the plan's
// models as aliases (or why they could not be fetched) and whether the notice
// could be recorded as shown. shownGen is the shown-session generation it was
// asked under, as a key's save's notice is (connectSavedMsg).
type signInFinishedMsg struct {
	shownGen  uint64
	aliases   []string
	modelsErr string
	noticeErr string
}

func (signInBegunMsg) connectAnswer()    {}
func (signInDoneMsg) connectAnswer()     {}
func (signInFinishedMsg) connectAnswer() {}

func (m signInFinishedMsg) shownUnder() uint64 { return m.shownGen }

// pickConnectProvider is Enter, or a click, on step one's selection: the
// sign-in for a provider funded by signing in, the key field for any other.
// Nothing happens until the providers have been read.
func (m Model) pickConnectProvider() (Model, tea.Cmd) {
	if p, ok := m.cdlg.provider(); ok && m.cdlg.loaded && p.SignIn {
		return m.openSignInStep()
	}
	return m.openKeyStep(), nil
}

// openSignInStep is step three: refused, with the dialog closed, while work
// runs (connectBusy), and otherwise opened at once with the attempt beginning
// off the Update (beginSignInCmd) under a context of its own and a number of
// its own (Model.connSeq), which is also its field's: a clipboard paste asked
// for in it lands in it alone (keyField).
func (m Model) openSignInStep() (Model, tea.Cmd) {
	if m.connectBusy() {
		m = m.closeDialog(false)
		m.addError(connectBusyText)
		return m, nil
	}
	ti := m.dialogInput()
	// Not masked: what is pasted here is an address (plan 033 §3.13).
	ti.CharLimit = connectRedirectMax + 1
	// bubbles' own Ctrl+V reads the clipboard with no seam in front of it;
	// craze reads it itself, tagged with this field (pasteFromClipboard).
	ti.KeyMap.Paste.SetEnabled(false)
	m.connSeq++
	ctx, cancel := context.WithCancel(context.Background())
	m.cdlg.step, m.cdlg.field, m.cdlg.key, m.cdlg.keyErr = connectSignIn, m.connSeq, ti, ""
	m.cdlg.signIn = signInState{cancel: cancel}
	return m, beginSignInCmd(ctx, m.cdlg.gen, m.connSeq, m.nativeDir)
}

// errNoSignInDir is a sign-in with no native directory to sign in to.
var errNoSignInDir = errors.New("there is no craze directory to sign in to (set HOME or CRAZE_HOME)")

// beginSignInCmd begins a sign-in into dir, off the Update, under ctx, which
// the step ends: an attempt that begins after that is closed here, and one
// that begins before it is closed where its answer lands (applySignIn). The
// wait over it is made here, with ctx, for the step to run once it adopts the
// attempt.
func beginSignInCmd(ctx context.Context, gen, run uint64, dir string) tea.Cmd {
	begin := beginSignIn
	return func() tea.Msg {
		msg := signInBegunMsg{gen: gen, run: run}
		if dir == "" {
			msg.err = errNoSignInDir
			return msg
		}
		att, err := begin(ctx, dir)
		if err != nil {
			msg.err = err
			return msg
		}
		if err := ctx.Err(); err != nil {
			att.Close()
			msg.err = err
			return msg
		}
		msg.att, msg.wait = att, waitSignInCmd(ctx, gen, run, att)
		return msg
	}
}

// waitSignInCmd waits for att's redirect — the listener's or a pasted one —
// and the sign-in it finishes (chatgptauth.Attempt.Wait), under ctx: the
// step's end cancels it, which closes the listener.
func waitSignInCmd(ctx context.Context, gen, run uint64, att signInAttempt) tea.Cmd {
	return func() tea.Msg {
		res, err := att.Wait(ctx)
		return signInDoneMsg{gen: gen, run: run, res: res, err: err}
	}
}

// signInOpen says the step numbered run of the dialog numbered gen is the one
// on screen.
func (m Model) signInOpen(gen, run uint64) bool {
	return m.dialog == dialogConnect && m.cdlg.gen == gen && m.cdlg.step == connectSignIn && m.cdlg.field == run
}

// applySignIn applies the step's answers (applyConnect).
func (m Model) applySignIn(msg connectAnswer) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case signInBegunMsg:
		if !m.signInOpen(msg.gen, msg.run) {
			// The step has gone — Esc, the box closed, a switch — and its
			// end found no attempt to close yet: this one is closed now.
			if msg.att != nil {
				msg.att.Close()
			}
			return m, nil
		}
		if msg.err != nil {
			m = m.closeDialog(false)
			m.addError("/connect: the sign-in could not start: " + signInErrorText(msg.err))
			return m, nil
		}
		s := &m.cdlg.signIn
		s.att, s.url, s.redirect, s.listening = msg.att, msg.att.URL(), msg.att.RedirectURI(), msg.att.Listening()
		return m, msg.wait
	case signInDoneMsg:
		open := m.signInOpen(msg.gen, msg.run)
		if msg.err != nil {
			// A run that has ended — the person left, or the box closed —
			// ends its wait with the end's own error: nothing to say.
			if !open {
				return m, nil
			}
			m = m.closeDialog(false)
			m.addError("/connect: " + signInFailureText(msg.err))
			return m, nil
		}
		// A sign-in that finished is the TUI's, whatever is on screen now:
		// the tokens are installed, so it is said even when the person
		// left the step as the browser came back.
		if open {
			m = m.closeDialog(false)
		}
		return m.signedIn(msg.res)
	case signInFinishedMsg:
		if msg.noticeErr != "" {
			m.addNote("the notice above could not be recorded as shown, so it may be shown again: " + msg.noticeErr)
		}
		switch {
		case msg.modelsErr != "":
			m.addNote("the plan's model list could not be fetched (" + msg.modelsErr + "); a native session fetches it when it opens")
		case len(msg.aliases) == 0:
			m.addNote("ChatGPT plan models: none listed for this account.")
		default:
			m.addNote("ChatGPT plan models: " + sanitizeLine(strings.Join(msg.aliases, ", ")))
		}
		m.addNote(signedInSessionNote)
	}
	return m, nil
}

// signedIn writes a finished sign-in to the transcript (plan 033 §3.13): the
// account, then — with plan usage, the first time this registration signs in
// with it — the notice, then, off the Update, the notice recorded and the
// plan's models fetched (finishSignInCmd). Without plan usage, the
// explanation and how to grant it.
func (m Model) signedIn(res chatgptauth.Result) (Model, tea.Cmd) {
	who := signedInPrefix
	if e := sanitizeLine(res.Email); e != "" {
		who += " as " + e
	}
	if !res.PlanUsage {
		m.addNote(who + planUsageOffText)
		m.addNote(planUsageOffHow)
		return m, nil
	}
	m.addNote(who + ".")
	if res.ShowNotice {
		m.addNote(chatgptauth.NoticeTitle)
		m.addNote(chatgptauth.Notice)
	}
	return m, finishSignInCmd(m.shownGen, m.nativeDir, res.ShowNotice)
}

// finishSignInCmd is the work after a sign-in, off the Update: the notice
// recorded as shown when it was (noticeShown), then the plan's models fetched
// into dir, as aliases. A failure of either is a note: the sign-in stands, and
// a native session fetches the list when it opens.
func finishSignInCmd(shown uint64, dir string, noticeShown bool) tea.Cmd {
	mark, fetch := markNoticeShown, fetchPlanModels
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), signInFinishTimeout)
		defer cancel()
		msg := signInFinishedMsg{shownGen: shown}
		if noticeShown {
			if err := mark(ctx, dir); err != nil {
				msg.noticeErr = signInErrorText(err)
			}
		}
		models, err := fetch(ctx, dir)
		if err != nil {
			msg.modelsErr = signInErrorText(err)
			return msg
		}
		for _, md := range models.Models {
			msg.aliases = append(msg.aliases, modeltable.ChatGPTAliasPrefix+md.Slug)
		}
		return msg
	}
}

// signInErrorText is err, from chatgptauth, as one line without the
// package's prefix. chatgptauth's errors name a step, a status and an OAuth
// code, never a token or anything a server sent.
func signInErrorText(err error) string {
	return sanitizeLine(strings.TrimPrefix(err.Error(), "chatgptauth: "))
}

// signInFailureText is why a run's wait failed, as craze auth words it.
func signInFailureText(err error) string {
	if errors.Is(err, chatgptauth.ErrAccessDenied) {
		return "the sign-in was declined in the browser; nothing was changed"
	}
	return signInErrorText(err)
}

// handleSignInKey is step three's keyboard: Esc goes back to step one,
// ending the attempt; Ctrl+Y copies the address; Enter hands the field's
// address to the attempt; Ctrl+V pastes the clipboard into this field and no
// other; every other key — a terminal's bracketed paste among them — is the
// field's, until an address has been handed over.
func (m Model) handleSignInKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := m.cdlg.signIn
	switch msg.Type {
	case tea.KeyEsc:
		m.cdlg = m.cdlg.leaveKeyStep()
		return m, nil
	case tea.KeyCtrlY:
		if s.url == "" {
			return m, nil
		}
		return m, copyText(m.shownGen, s.url, connectSignInCopied)
	case tea.KeyEnter:
		return m.pasteSignIn(), nil
	case tea.KeyCtrlV:
		if s.handed {
			return m, nil
		}
		return m, pasteFromClipboardForKey(m.shownGen, m.cdlg.openField())
	}
	if s.handed {
		return m, nil
	}
	before := m.cdlg.key.Value()
	var cmd tea.Cmd
	m.cdlg.key, cmd = m.cdlg.key.Update(msg)
	if m.cdlg.key.Value() != before {
		m.cdlg.keyErr = ""
	}
	return m, cmd
}

// pasteSignIn is Enter on the field: its address handed to the attempt
// (Attempt.Paste), whose wait then finishes the sign-in. Nothing happens
// before the attempt has begun, after an address was handed over, or with the
// field empty. Work that started while the box was open refuses it, the
// address kept for a later Enter. A line that is not an address, one too long
// to be one, or one the attempt refuses is refused in the field, which is
// emptied; the line is never quoted. An address the attempt finds over — the
// browser's came first — is as good as handed: the wait is finishing.
func (m Model) pasteSignIn() Model {
	s := m.cdlg.signIn
	line := strings.TrimSpace(m.cdlg.key.Value())
	if s.att == nil || s.handed || line == "" {
		return m
	}
	if m.connectBusy() {
		m.cdlg.keyErr = connectBusySignInText
		return m
	}
	why := redirectRefusal(line, s.redirect)
	if why == "" {
		switch err := s.att.Paste(line); {
		case err == nil, errors.Is(err, chatgptauth.ErrAttemptOver):
			m.cdlg.signIn.handed = true
			m.cdlg.key.Reset()
			m.cdlg.keyErr = ""
			return m
		default:
			why = "That is not this sign-in's redirect address. " + pasteWhat(s.redirect)
		}
	}
	m.cdlg.key.Reset()
	m.cdlg.keyErr = why
	return m
}

// redirectRefusal is why line cannot be the sign-in's redirect before the
// attempt is asked — too long, or not an address at all — and "" when it is
// an address the attempt must judge. It never quotes the line: one that is
// not an address is most likely a key put where the plan's sign-in goes, and
// is told the plan takes none (craze auth's rule, plan 033 §3.13).
func redirectRefusal(line, redirect string) string {
	if len(line) > connectRedirectMax {
		return "That is too long to be the redirect address. " + pasteWhat(redirect)
	}
	if u, err := url.Parse(line); err != nil || u.Scheme == "" || u.Host == "" {
		return "That is not an address: the ChatGPT plan is funded by signing in, never by an API key. " + pasteWhat(redirect)
	}
	return ""
}

// pasteWhat says what to paste instead: the address the browser was sent to,
// which starts with the attempt's redirect address.
func pasteWhat(redirect string) string {
	return "Paste the whole address the browser was sent to; it starts with " + sanitizeLine(redirect) + "."
}

// hint is the line under the address: how the redirect comes back, or that
// it has.
func (s signInState) hint() string {
	switch {
	case s.handed:
		return connectSignInHanded
	case s.listening:
		return connectSignInWaiting
	}
	return "After you approve, paste the address the browser lands on (it starts with " + sanitizeLine(s.redirect) + "):"
}

// signInBody is step three at an inner size: the title, the introduction, the
// address, the hint, the field, its refusal and the footer, top to bottom. A
// short box keeps, in order: the title, the field and its refusal; the
// address's first row; the footer, which says Ctrl+Y copies the whole
// address; the hint, which says how the paste path ends; the introduction;
// and the rest of the address, its last row shown ending in "…" when it is
// cut.
func (m Model) signInBody(inner, budget int) []string {
	s := m.cdlg.signIn
	dim, plain := styleFG(m.theme.Dim), styleFG(m.theme.FG)
	title := m.dialogTitle(connectSignInTitle, inner)
	if s.att == nil {
		rows := []string{title}
		if budget >= 2 {
			rows = append(rows, dim.Render(clampWidth(connectSignInStarting, inner)))
		}
		if budget >= 3 {
			rows = append(rows, m.dialogFooter(connectSignInStartHint, inner))
		}
		return rows
	}
	room := budget - 1 // the title
	take := func(rows []string) []string {
		rows = rows[:min(len(rows), max(room, 0))]
		room -= len(rows)
		return rows
	}
	var field []string
	if !s.handed {
		field = take([]string{dialogInputView(m.cdlg.key, inner)})
	}
	var errs []string
	if m.cdlg.keyErr != "" {
		errs = take(dialogWrap(m.cdlg.keyErr, inner))
	}
	// The address is cut at the width, never at a word: a URL's hyphens and
	// slashes are no place to break it (dialogWrap would), and its rows are
	// what a person copies by hand when Ctrl+Y cannot reach their clipboard.
	all := strings.Split(ansi.Hardwrap(sanitizeLine(s.url), inner, false), "\n")
	link := take(all[:min(len(all), 1)])
	footer := len(take([]string{""})) == 1
	hint := take(dialogWrap(s.hint(), inner))
	intro := take(dialogWrap(connectSignInIntro, inner))
	link = append(slices.Clone(link), take(all[len(link):])...)
	if n := len(link); n > 0 && n < len(all) {
		last := []rune(link[n-1])
		link[n-1] = string(last[:max(len(last)-1, 0)]) + "…"
	}

	rows := []string{title}
	for _, r := range intro {
		rows = append(rows, dim.Render(clampWidth(r, inner)))
	}
	for _, r := range link {
		rows = append(rows, plain.Render(clampWidth(r, inner)))
	}
	for _, r := range hint {
		rows = append(rows, dim.Render(clampWidth(r, inner)))
	}
	rows = append(rows, field...)
	for _, e := range errs {
		rows = append(rows, styleFG(m.theme.Err).Render(clampWidth(e, inner)))
	}
	if footer {
		hintText := connectSignInWaitHint
		if s.handed {
			hintText = connectSignInHandedHint
		}
		rows = append(rows, m.dialogFooter(hintText, inner))
	}
	return rows
}
