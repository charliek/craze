package tui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/signinlog"
)

// /connect's third step (plan 033 §3.13, owner decision 10): Sign in with
// ChatGPT, in the TUI. A provider funded by signing in rather than by a key —
// the ChatGPT plan's (modeltable.ProviderInfo.SignIn) — has no key field:
// Enter on its row in step one opens "Sign in with ChatGPT" instead, which
// runs the whole sign-in craze auth login chatgpt runs (chatgptauth.Begin),
// into this TUI's native directory.
//
//   - The step shows the authorization address, and Ctrl+Y — or a click on
//     any of its rows — copies it through the TUI's own copy (copyText: OSC
//     52, then the native tool), so a person on another machine — over SSH,
//     say — can open it in their own browser; a line in the box says so, and
//     what to do when nothing reached the clipboard (plan 034 §3.2, Q10). Each
//     row of it is also a terminal hyperlink to the whole address (OSC 8, Q9;
//     linkRow), so a Cmd/Ctrl+click opens it whole however the box wraps or
//     cuts it. It carries no token: craze never sends id_token_hint (P21).
//   - One layout plan (signInPlan) says which row is which, for the drawing
//     and for a click alike, so a click can only copy from a row that shows
//     the address.
//   - The wait is a plain tea.Cmd under a context the step owns, never a gated
//     call: a gated call has the gate's 15-second deadline and holds every key
//     typed meanwhile (gate.go, gateDeadline), and a browser's approval takes
//     as long as the person does. So is the begin (the host id under its lock,
//     the listener), so that an attempt the gate gave up on, or dropped across
//     a switch, can never be left listening: an answer that finds its step gone
//     closes the attempt it carries (applySignIn).
//   - The browser comes back to the attempt's loopback listener, or — when the
//     browser is on another machine, where that page cannot load — its address
//     is pasted into the step's field. The field is always drawn masked, as the
//     key field is, whatever it holds (signInField; review r15 b, amending
//     §3.13's unmasked field and superseding r14 1's address-only mask, which a
//     key glued onto the address got past): a key pasted out of habit is never
//     in a frame, not even the one before Enter refuses it. A line under the
//     field says, in one of two fixed texts, whether what it holds is this
//     attempt's redirect address (signInStatus) — never a word of it. The
//     listener and the paste race inside the attempt, and whichever is
//     accepted first wins (chatgptauth.Attempt). Enter hands the line to the
//     attempt, which judges it (plan 034 §3.3): one that is not an address,
//     one too long, another attempt's redirect or another address is refused
//     in the field — phrased from the attempt's refusal (pasteRefusalText),
//     never quoting it — and the field is emptied. Over SSH, or when craze
//     could not listen, the hint under the address leads with the paste path
//     (plan 034 Q12): the browser's page will not load, so its address is what
//     to paste — and, over SSH with a listener, the ssh -L line that forwards
//     the listener's port instead (signInState.hint).
//   - Every way out of the step ends the attempt and closes its listener with
//     every connection to it (signInState.end): Esc, which goes back to step
//     one; the dialog closing for any reason (closeDialog); the dialog dropped
//     on a quit or the session's end (dropConnect); and a switch to another
//     session (withSession). The exits no Update sees — SIGTERM, SIGHUP, a
//     program failure — end it in finishRun, through the run's record in the
//     model's shared set (signInRuns), which also reaches an attempt whose
//     begin's answer was never read (review r14 2).
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
//   - Every attempt is logged, value-free, in that directory's sign-in log
//     (plan 034 §3.3, Q5; internal/signinlog), surface tui: the attempt reports
//     its events to the run's observer (observeSignIn), which records each one
//     — its begin, the listener's refusals, a refused paste, its redirect, its
//     one outcome with why (the CloseReason every way out passes), the address
//     copied, and the model fetch after it. The log is opened the first time a
//     sign-in begins and closed by finishRun (signInRuns). A log that cannot be
//     kept is one transcript note, and the sign-in goes on. Two of the events
//     are also lines in the box (plan 034 Q8; signInShown): why craze is not
//     listening for the browser (the begin's reason), and the listener's
//     refusal of the browser's return from a different attempt — each a fixed
//     text, never a value.
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
	// waits for the browser, craze running on this machine.
	connectSignInWaiting = "Waiting for the browser. If it is on another machine, paste the address it lands on:"
	// The hint over SSH, or with no listener (plan 034 Q12; signInState.hint):
	// the paste path first. connectSignInSSH opens it over SSH; with a
	// listener, connectSignInSSHListening follows, naming the listener's port
	// twice (the ssh -L forward); with none, connectSignInSSHPasteOnly,
	// naming the address the browser lands on. On this machine with no
	// listener the hint is connectSignInPasteOnly alone. A forward to a port
	// craze is not listening on would reach whatever holds it, so a
	// paste-only hint offers none.
	connectSignInSSH          = "craze runs on another machine (SSH). Open the address on your computer and approve. "
	connectSignInSSHListening = "When the browser can't connect to 127.0.0.1, copy that page's address and paste it here — or forward the port first: ssh -L %s:127.0.0.1:%s."
	connectSignInSSHPasteOnly = "The browser's page will not load: copy its whole address (it starts with %s) and paste it here:"
	connectSignInPasteOnly    = "The browser's page will not load after you approve: copy its whole address (it starts with %s) and paste it here:"
	// connectSignInHanded replaces the hint and the field once a pasted
	// address was accepted: the exchange is under way.
	connectSignInHanded = "Signing in…"
	// The line under the field while it holds something (signInStatus): it
	// is this attempt's redirect address, or it is not (yet). Neither repeats
	// anything of the field's, which is masked.
	connectSignInReady     = "That's the redirect address: press enter to sign in."
	connectSignInPasteHint = "Paste the whole address the browser was sent to."
	// The footers: while the attempt begins, while it waits — with the mouse,
	// a narrow box's shorter form, and with --no-mouse, where a click is the
	// terminal's own — and once the address is handed over.
	connectSignInStartHint      = "esc back"
	connectSignInWaitHint       = "click or ctrl+y copies · enter · esc back"
	connectSignInWaitHintNarrow = "click/ctrl+y copies · enter · esc"
	connectSignInWaitHintKeys   = "ctrl+y copies the address · enter · esc back"
	connectSignInHandedHint     = "esc cancels"
	// connectSignInCopied is the status row's note after a copy (Ctrl+Y or a
	// click).
	connectSignInCopied = "copied the sign-in address"
	// The box's line after a copy (plan 034 Q10; signInState.copied), until
	// the next key or for signInCopiedLinger: what was done, and — the address
	// being a link (Q9) — what to do when OSC 52 reached no clipboard. Plain
	// rows have no link to click, so they say the first part alone.
	connectSignInCopiedLine     = "Copied the address. Paste it into a browser."
	connectSignInCopiedLinkLine = connectSignInCopiedLine + " Nothing on the clipboard? Cmd/Ctrl+click it (some terminals need Shift while craze holds the mouse)."
	// connectSignInOtherAttempt is the listener's refusal of a redirect with a
	// code and another attempt's state (plan 034 Q8): said once an attempt,
	// in the words of the paste's own refusal of one (connectSignInEarlierText).
	connectSignInOtherAttempt = "The browser came back from a different sign-in attempt. Use the address shown now."
	// connectBusySignInText is a pasted address's Enter refused because work
	// started while the box was open. The address stays in the field.
	connectBusySignInText = "Finish or stop the running work first, then press enter to sign in."
	// connectSignInEarlierText is a pasted address refused because it is the
	// sign-in's redirect address with another attempt's state (plan 034
	// §3.3, chatgptauth.PartState): the browser's return from an attempt
	// before this one.
	connectSignInEarlierText = "That is the redirect of an earlier sign-in attempt. Use the address shown now."
	// connectPlanOffMark tags, in step one, a provider signed in to by an
	// account that did not grant plan usage (modeltable.KeyPlanDisabled).
	connectPlanOffMark = "plan usage off"
	// connectRedirectMax is the longest line the field takes as a pasted
	// address: the attempt's own bound (chatgptauth.MaxPaste), which craze
	// auth reads by too. A real one is a few hundred bytes, and a longer line
	// is not one.
	connectRedirectMax = chatgptauth.MaxPaste
	// signInFinishTimeout bounds the work after a sign-in: the notice
	// recorded and the model list fetched.
	signInFinishTimeout = time.Minute
	// signInDialogWidth is the widest the step's box gets (plan 034 Q11;
	// dialogMaxWidth): wider than every other dialog, so the address takes
	// half the rows at 100 columns and more. It cannot make it one row.
	signInDialogWidth = 104
	// signInLinkPrefix opens the OSC 8 id every row of one attempt's address
	// shares (linkRow), so a terminal highlights the rows as one link; the
	// attempt's id (chatgptauth.Attempt.ID) follows.
	signInLinkPrefix = "craze-signin-"
	// signInShownMax is how many shown events a run holds unread
	// (signInShown): the listener reports each kind once an attempt, so it
	// never fills; one past it is dropped rather than block the listener.
	signInShownMax = 4
)

// signInCopiedLinger is how long the box's copied line stays without a key
// (plan 034 Q10): a variable only so a test can run its tick.
var signInCopiedLinger = 10 * time.Second

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
	// ID is the attempt's random id (chatgptauth.Attempt.ID): the address's
	// link id (signInLink).
	ID() string
	URL() string
	RedirectURI() string
	Listening() bool
	Paste(raw string) error
	Wait(ctx context.Context) (chatgptauth.Result, error)
	Close(reason chatgptauth.CloseReason)
	Report(kind chatgptauth.EventKind)
	Observer() func(chatgptauth.Event)
}

// The step's seams: chatgptauth in craze; stand-ins in this package's tests,
// which must never reach OpenAI. Each is read on the Update goroutine and
// handed to its command (sendCopy says why).
var (
	// beginSignIn begins an attempt into dir that reports its events to
	// observe (chatgptauth.BeginOptions.Observe).
	beginSignIn = func(ctx context.Context, dir string, observe func(chatgptauth.Event)) (signInAttempt, error) {
		a, err := chatgptauth.Begin(ctx, dir, chatgptauth.BeginOptions{Observe: observe})
		if err != nil {
			return nil, err
		}
		return a, nil
	}
	// fetchPlanModels fetches the plan's models into dir after a sign-in,
	// reporting to observe, the attempt's observer (Attempt.Observer).
	fetchPlanModels = func(ctx context.Context, dir string, observe func(chatgptauth.Event)) (*chatgptauth.Models, error) {
		return chatgptauth.FetchModels(ctx, chatgptauth.Source(dir), chatgptauth.FetchOptions{
			ClientVersion: modeltable.ChatGPTModelsClientVersion(), Observe: observe,
		})
	}
	markNoticeShown = chatgptauth.MarkNoticeShown
	// openSignInLog opens the native directory's sign-in log.
	openSignInLog = signinlog.Open
)

// signInState is step three's own state (connectDialog.signIn): the zero value
// is no sign-in, which every other step has.
type signInState struct {
	// runs is the model's set of runs (Model.signIns) this one is recorded
	// in, under its number run — the step's field number (Model.connSeq).
	runs *signInRuns
	run  uint64
	// cancel ends the run: the context the begin and the wait run under,
	// whose end closes the attempt's listener (chatgptauth.Attempt.Wait). Its
	// cause carries the end's reason (signInEnded).
	cancel context.CancelCauseFunc
	// att is the attempt, once it has begun; url and redirect are its
	// authorization and redirect addresses, listening whether a loopback
	// listener waits for the browser. state is the state url carries
	// (authState), which the browser's redirect brings back: what the line
	// under the field knows this attempt's redirect by (signInStatus).
	att       signInAttempt
	url       string
	redirect  string
	state     string
	listening bool
	// handed says a pasted address was accepted: the attempt is finishing,
	// and the field is gone.
	handed bool
	// link is the OSC 8 id every address row is a link under (signInLink,
	// plan 034 Q9), or "" when the rows are drawn plain; ssh says craze runs
	// over SSH (Model.overSSH), which the hint leads with (Q12). Both are
	// judged once, as the attempt is adopted.
	link string
	ssh  bool
	// why is why craze is not listening for the browser, from the attempt's
	// begin (listenerMissing, Q8), or ""; other says the listener refused the
	// browser's return from a different attempt (Q8). Each is a line in the
	// box.
	why   string
	other bool
	// copied says the box shows the copied line (Q10): from a copy until the
	// next key, or until the tick of copy number copies (signInCopiedMsg).
	copied bool
	copies uint64
}

// end ends the run, if there is one, for reason (plan 034 §3.3): the attempt
// closed with its listener and every connection to it — the one the step
// adopted, or one Begin has returned whose answer the step has not seen yet
// (signInRuns) — and the context cancelled. The attempt is closed first, so
// an attempt still waiting ends cancelled with reason; one that has ended
// already takes the close as cleanup. It is safe to call more than once, and
// on a copy: the copies share the run, and the first reason is the one kept.
func (s signInState) end(reason chatgptauth.CloseReason) {
	if s.att != nil {
		s.att.Close(reason)
	}
	s.runs.end(s.run, reason)
	if s.cancel != nil {
		s.cancel(&signInEnded{reason: reason})
	}
}

// signInEnded is the cause a run's context is cancelled with: why it ended,
// for an attempt Begin returns after its run did (beginSignInCmd).
type signInEnded struct{ reason chatgptauth.CloseReason }

func (e *signInEnded) Error() string { return "the sign-in ended: " + string(e.reason) }

// endReason is why ctx's run ended, by its cause; CloseDialog when it says
// none.
func endReason(ctx context.Context) chatgptauth.CloseReason {
	var e *signInEnded
	if errors.As(context.Cause(ctx), &e) {
		return e.reason
	}
	return chatgptauth.CloseDialog
}

// signInRuns is every /connect sign-in run not yet ended, by its number: the
// cancel of the context its begin and wait run under, and its attempt from the
// moment Begin returns it. It is shared by every copy of the model
// (Model.signIns), as the completion popups' loads are, for the exits no
// Update sees (plan 033 §3.13; review r14 2): SIGTERM, SIGHUP and a program
// failure reach finishRun without passing through requestQuit's dropConnect,
// and finishRun ends every run here (closeAll), so no listener — nor a wait
// that a browser's late redirect could still finish — outlives the program.
//
// The begin's command records its attempt here as Begin returns it (adopt),
// not where its answer lands: an answer the program never reads — it quit in
// between — would otherwise leave that attempt listening. Whichever comes
// second, the end or the record, closes it. Nil — a model a test built on its
// own — holds nothing, and every method allows it.
//
// It also holds the runs' sign-in log (plan 034 §3.3): opened the first time
// a run begins (logFor, off the Update), from the native directory the run
// signs in to — the TUI's Config.NativeDir, so a test with a temp NativeDir
// never touches ~/.craze — and closed by finishRun (closeLog), after closeAll
// has had every run still open report its end.
type signInRuns struct {
	mu   sync.Mutex
	open map[uint64]*signInRun

	logMu     sync.Mutex
	logOpened bool
	logClosed bool
	log       *signinlog.Log
	logErr    error // the open's refusal
	logTold   bool  // the one transcript note was written
}

// signInRun is one run's: its context's cancel and, once begun, its attempt.
type signInRun struct {
	cancel context.CancelCauseFunc
	att    signInAttempt
}

// stop closes the run's attempt, if it has one, for reason, and cancels its
// context.
func (r *signInRun) stop(reason chatgptauth.CloseReason) {
	if r.att != nil {
		r.att.Close(reason)
	}
	r.cancel(&signInEnded{reason: reason})
}

// add records run, its begin and wait to run under cancel's context.
func (s *signInRuns) add(run uint64, cancel context.CancelCauseFunc) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil {
		s.open = map[uint64]*signInRun{}
	}
	s.open[run] = &signInRun{cancel: cancel}
}

// adopt records att as run's attempt, Begin having returned it, and says
// whether the run is still open. False is a run already ended — by its step
// or by finishRun — whose attempt nothing else will close: the caller closes
// it.
func (s *signInRuns) adopt(run uint64, att signInAttempt) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.open[run]
	if ok {
		r.att = att
	}
	return ok
}

// end ends run, if it is still open, for reason: its attempt closed, its
// context cancelled, and the run forgotten.
func (s *signInRuns) end(run uint64, reason chatgptauth.CloseReason) {
	if s == nil {
		return
	}
	s.mu.Lock()
	r := s.open[run]
	delete(s.open, run)
	s.mu.Unlock()
	if r != nil {
		r.stop(reason)
	}
}

// closeAll ends every run still open, for reason: finishRun's, on every exit
// path (CloseShutdown).
func (s *signInRuns) closeAll(reason chatgptauth.CloseReason) {
	if s == nil {
		return
	}
	s.mu.Lock()
	open := s.open
	s.open = nil
	s.mu.Unlock()
	for _, r := range open {
		r.stop(reason)
	}
}

// logFor is the runs' sign-in log, opened in dir the first time it is asked
// for: nil when it could not be opened (logErr says why, for mention), or
// once closeLog has run — a run that begins as the program ends logs nothing.
// It does disk work, off the Update (beginSignInCmd), and outside logMu, which
// mention takes on the Update. One begin runs at a time (one box), so no
// second open races the first.
func (s *signInRuns) logFor(dir string) *signinlog.Log {
	if s == nil || dir == "" {
		return nil
	}
	s.logMu.Lock()
	switch {
	case s.logClosed:
		s.logMu.Unlock()
		return nil
	case s.logOpened:
		l := s.log
		s.logMu.Unlock()
		return l
	}
	s.logOpened = true
	s.logMu.Unlock()
	l, err := openSignInLog(dir)
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logClosed {
		_ = l.Close()
		return nil
	}
	s.log, s.logErr = l, err
	return l
}

// mention is the transcript's one note about the sign-in log, once: why it
// was refused when it opened, or why it stopped since; "" otherwise, and ever
// after the note was written.
func (s *signInRuns) mention() string {
	if s == nil {
		return ""
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logTold {
		return ""
	}
	err := s.logErr
	if err == nil {
		err = s.log.Failure()
	}
	if err == nil {
		return ""
	}
	s.logTold = true
	return "the sign-in log is off: " + sanitizeLine(err.Error())
}

// closeLog closes the runs' sign-in log, flushing it for at most a second:
// finishRun's, after closeAll, so every run's end is written first.
func (s *signInRuns) closeLog() {
	if s == nil {
		return
	}
	s.logMu.Lock()
	l := s.log
	s.logClosed = true
	s.logMu.Unlock()
	_ = l.Close()
}

// observeSignIn is a run's observer (chatgptauth.BeginOptions.Observe), called
// on whichever goroutine the attempt reports on: the fan-out every event goes
// through. Its first leg records the event in the sign-in log, surface tui.
// Its second, show, hands the events the step draws a line for to the step
// (signInShown.show; plan 034 Q8). nil shows nothing.
func observeSignIn(log *signinlog.Log, show func(chatgptauth.Event)) func(chatgptauth.Event) {
	return func(ev chatgptauth.Event) {
		log.Record(ev, signinlog.SurfaceTUI)
		if show != nil {
			show(ev)
		}
	}
}

// signInShown is one run's events the step draws a line for (plan 034 Q8),
// from the attempt's observer — on whichever goroutine the attempt reports on
// — to the step:
//
//   - the attempt's begin, which says why there is no listener (Mode
//     paste-only, Reason port_busy or listen_failed). Begin reports it on the
//     begin's own goroutine before it returns, so the begin's answer carries
//     it (signInBegunMsg.begin): no frame draws the paste-only step without
//     its reason, and nothing races the answer that adopts the attempt;
//   - the listener's refusal of a redirect with a code and another attempt's
//     state (RefusalOtherAttempt), which can come at any time: it is queued
//     here, never blocking the listener's page, and delivered to the model by
//     a command stamped with the dialog's and the run's numbers
//     (signInEventsCmd), as the wait's answer is.
//
// Every other event is the log's alone. done is closed when the run's wait
// has returned, which ends the delivering command with the attempt.
type signInShown struct {
	mu     sync.Mutex
	begin  chatgptauth.Event
	events chan chatgptauth.Event
	done   chan struct{}
	once   sync.Once
}

func newSignInShown() *signInShown {
	return &signInShown{events: make(chan chatgptauth.Event, signInShownMax), done: make(chan struct{})}
}

// show is the run's observer's second leg (observeSignIn).
func (s *signInShown) show(ev chatgptauth.Event) {
	switch {
	case ev.Kind == chatgptauth.EventBegin:
		s.mu.Lock()
		s.begin = ev
		s.mu.Unlock()
	case ev.Kind == chatgptauth.EventListenerRefused && ev.Refusal == chatgptauth.RefusalOtherAttempt:
		select {
		case s.events <- ev:
		default:
		}
	}
}

// begun is the attempt's begin, as Begin reported it: the zero Event before.
func (s *signInShown) begun() chatgptauth.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.begin
}

// waited says the run's wait has returned: nothing more is delivered.
func (s *signInShown) waited() { s.once.Do(func() { close(s.done) }) }

// signInEventsCmd delivers the run's next queued event (signInShown) as a
// signInShownMsg stamped with the dialog numbered gen and the step numbered
// run — whose application asks for the one after it (next) while the step is
// open — or nothing, once the run has ended (ctx) or its wait has returned.
func signInEventsCmd(ctx context.Context, gen, run uint64, shown *signInShown) tea.Cmd {
	return func() tea.Msg {
		select {
		case ev := <-shown.events:
			return signInShownMsg{gen: gen, run: run, ev: ev, next: signInEventsCmd(ctx, gen, run, shown)}
		case <-ctx.Done():
		case <-shown.done:
		}
		return nil
	}
}

// signInBegunMsg is a run's begin, for the step numbered run of the dialog
// numbered gen: the attempt, its begin's event (signInShown), the wait to run
// over it and the delivery of its shown events, or why it could not begin. It
// is never dropped unread (it is no shownStamped): applySignIn closes an
// attempt whose step is gone.
type signInBegunMsg struct {
	gen, run uint64
	att      signInAttempt
	begin    chatgptauth.Event
	wait     tea.Cmd
	events   tea.Cmd
	err      error
}

// signInShownMsg is one of a run's shown events (signInShown), for the step
// numbered run of the dialog numbered gen: dropped once that step is gone.
// next delivers the one after it.
type signInShownMsg struct {
	gen, run uint64
	ev       chatgptauth.Event
	next     tea.Cmd
}

// signInCopiedMsg is the copied line's time up (signInCopiedLinger) for copy
// number n of the step numbered run of the dialog numbered gen: it ends the
// line only while that copy is the step's last.
type signInCopiedMsg struct{ gen, run, n uint64 }

// signInDoneMsg is a run's wait: the sign-in's result, or why it ended. It
// carries the account's email, never a token, and the attempt's observer, for
// the model fetch that follows a sign-in (Attempt.Observer).
type signInDoneMsg struct {
	gen, run uint64
	res      chatgptauth.Result
	err      error
	observe  func(chatgptauth.Event)
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
func (signInShownMsg) connectAnswer()    {}
func (signInCopiedMsg) connectAnswer()   {}

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
// for in it lands in it alone (keyField). The run is recorded in the model's
// shared set (Model.signIns), so every exit ends it, those no Update sees
// included (finishRun).
func (m Model) openSignInStep() (Model, tea.Cmd) {
	if m.connectBusy() {
		m = m.closeDialog(false)
		m.addError(connectBusyText)
		return m, nil
	}
	ti := m.dialogInput()
	// Masked, whatever it holds (signInField; plan 033 §3.13 as review r15 b
	// amends it).
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = connectMask
	ti.CharLimit = connectRedirectMax + 1
	// bubbles' own Ctrl+V reads the clipboard with no seam in front of it;
	// craze reads it itself, tagged with this field (pasteFromClipboard).
	ti.KeyMap.Paste.SetEnabled(false)
	m.connSeq++
	ctx, cancel := context.WithCancelCause(context.Background())
	m.signIns.add(m.connSeq, cancel)
	m.cdlg.step, m.cdlg.field, m.cdlg.key, m.cdlg.keyErr = connectSignIn, m.connSeq, ti, ""
	m.cdlg.signIn = signInState{runs: m.signIns, run: m.connSeq, cancel: cancel}
	return m, beginSignInCmd(ctx, m.signIns, m.cdlg.gen, m.connSeq, m.nativeDir)
}

// errNoSignInDir is a sign-in with no native directory to sign in to.
var errNoSignInDir = errors.New("there is no craze directory to sign in to (set HOME or CRAZE_HOME)")

// beginSignInCmd begins a sign-in into dir, off the Update, under ctx, which
// the step ends. The attempt is recorded with its run in runs the moment
// Begin returns it (signInRuns.adopt), so whatever ends the run — the step, or
// finishRun on an exit no Update sees — closes it even when this command's
// answer is never read; an attempt that begins after the run has ended is
// closed here, for the run's own reason (endReason). The wait over it is made
// here, with ctx, for the step to run once it adopts the attempt
// (applySignIn). The attempt reports to the run's observer (observeSignIn),
// over the runs' sign-in log, which the first begin opens, and the run's
// shown events (signInShown): its begin, read as Begin returns, and the
// delivery of the rest, made here with ctx too.
func beginSignInCmd(ctx context.Context, runs *signInRuns, gen, run uint64, dir string) tea.Cmd {
	begin := beginSignIn
	return func() tea.Msg {
		msg := signInBegunMsg{gen: gen, run: run}
		if dir == "" {
			msg.err = errNoSignInDir
			return msg
		}
		shown := newSignInShown()
		att, err := begin(ctx, dir, observeSignIn(runs.logFor(dir), shown.show))
		if err != nil {
			msg.err = err
			return msg
		}
		if !runs.adopt(run, att) || ctx.Err() != nil {
			att.Close(endReason(ctx))
			msg.err = context.Canceled
			return msg
		}
		msg.att, msg.begin = att, shown.begun()
		msg.wait, msg.events = waitSignInCmd(ctx, gen, run, att, shown), signInEventsCmd(ctx, gen, run, shown)
		return msg
	}
}

// waitSignInCmd waits for att's redirect — the listener's or a pasted one —
// and the sign-in it finishes (chatgptauth.Attempt.Wait), under ctx: the
// step's end cancels it, which closes the listener. Its return ends the
// delivery of the run's shown events (signInShown.waited).
func waitSignInCmd(ctx context.Context, gen, run uint64, att signInAttempt, shown *signInShown) tea.Cmd {
	return func() tea.Msg {
		res, err := att.Wait(ctx)
		shown.waited()
		return signInDoneMsg{gen: gen, run: run, res: res, err: err, observe: att.Observer()}
	}
}

// signInOpen says the step numbered run of the dialog numbered gen is the one
// on screen.
func (m Model) signInOpen(gen, run uint64) bool {
	return m.dialog == dialogConnect && m.cdlg.gen == gen && m.cdlg.step == connectSignIn && m.cdlg.field == run
}

// applySignIn applies the step's answers (applyConnect). The sign-in log's
// one note, when it could not be opened or has stopped, is written with the
// first answer after (signInRuns.mention).
func (m Model) applySignIn(msg connectAnswer) (Model, tea.Cmd) {
	if note := m.signIns.mention(); note != "" {
		m.addNote(note)
	}
	switch msg := msg.(type) {
	case signInBegunMsg:
		if !m.signInOpen(msg.gen, msg.run) {
			// The step has gone — Esc, the box closed, a switch — and its
			// end found no attempt to close yet: this one is closed now. (An
			// end that found it closed it with its own reason already, which
			// the attempt keeps: this close is then a no-op.)
			if msg.att != nil {
				msg.att.Close(chatgptauth.CloseDialog)
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
		s.state = authState(s.url)
		s.link, s.ssh, s.why = signInLink(s.url, msg.att.ID(), m.getenv), m.overSSH(), listenerMissing(msg.begin)
		return m, tea.Batch(msg.wait, msg.events)
	case signInShownMsg:
		// Another dialog's, or another run's — Esc and back opened a new
		// one — is dropped, and so is the delivery after it.
		if !m.signInOpen(msg.gen, msg.run) {
			return m, nil
		}
		if msg.ev.Kind == chatgptauth.EventListenerRefused && msg.ev.Refusal == chatgptauth.RefusalOtherAttempt {
			m.cdlg.signIn.other = true
		}
		return m, msg.next
	case signInCopiedMsg:
		if s := &m.cdlg.signIn; m.signInOpen(msg.gen, msg.run) && s.copies == msg.n {
			s.copied = false
		}
	case signInDoneMsg:
		open := m.signInOpen(msg.gen, msg.run)
		if msg.err != nil {
			// A run that has ended — the person left, or the box closed —
			// ends its wait with the end's own error: nothing to say.
			if !open {
				return m, nil
			}
			// The wait has returned: the attempt reported its outcome, and
			// the box's close is cleanup (CloseDone), not a cancel.
			m.cdlg.signIn.end(chatgptauth.CloseDone)
			m = m.closeDialog(false)
			m.addError("/connect: " + signInFailureText(msg.err))
			return m, nil
		}
		// A sign-in that finished is the TUI's, whatever is on screen now:
		// the tokens are installed, so it is said even when the person
		// left the step as the browser came back.
		if open {
			m.cdlg.signIn.end(chatgptauth.CloseDone)
			m = m.closeDialog(false)
		}
		return m.signedIn(msg.res, msg.observe)
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
// plan's models fetched (finishSignInCmd), reporting to observe, the
// attempt's observer. Without plan usage, the explanation and how to grant
// it.
func (m Model) signedIn(res chatgptauth.Result, observe func(chatgptauth.Event)) (Model, tea.Cmd) {
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
	return m, finishSignInCmd(m.shownGen, m.nativeDir, res.ShowNotice, observe)
}

// finishSignInCmd is the work after a sign-in, off the Update: the notice
// recorded as shown when it was (noticeShown), then the plan's models fetched
// into dir, as aliases, the fetch reporting to observe. A failure of either is
// a note: the sign-in stands, and a native session fetches the list when it
// opens.
func finishSignInCmd(shown uint64, dir string, noticeShown bool, observe func(chatgptauth.Event)) tea.Cmd {
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
		models, err := fetch(ctx, dir, observe)
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
// ending the attempt; Ctrl+Y copies the address (copySignInAddress); Enter
// hands the field's address to the attempt; Ctrl+V pastes the clipboard into
// this field and no other; every other key — a terminal's bracketed paste
// among them — is the field's, until an address has been handed over. Every
// key but Ctrl+Y ends the copied line (plan 034 Q10).
func (m Model) handleSignInKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type != tea.KeyCtrlY {
		m.cdlg.signIn.copied = false
	}
	s := m.cdlg.signIn
	switch msg.Type {
	case tea.KeyEsc:
		m.cdlg = m.cdlg.leaveKeyStep()
		return m, nil
	case tea.KeyCtrlY:
		return m.copySignInAddress()
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

// copySignInAddress is Ctrl+Y on the step, or a click on a row of the address
// (plan 034 Q10; signInClick): the authorization address — the attempt's own
// bytes, whole, however the box wraps or cuts it — through the TUI's copy
// (copyText: OSC 52, then the native tool), with the status row's note; and
// the box's copied line, until the next key or for signInCopiedLinger, whose
// tick is stamped with this copy's number, so an earlier copy's tick never
// ends a later copy's line. The attempt is told (address_copied), for the
// sign-in log; whether a clipboard took the address no one can tell. Nothing
// happens before the attempt has begun.
func (m Model) copySignInAddress() (Model, tea.Cmd) {
	s := &m.cdlg.signIn
	if s.url == "" {
		return m, nil
	}
	s.att.Report(chatgptauth.EventAddressCopied)
	s.copies++
	s.copied = true
	gen, run, n := m.cdlg.gen, s.run, s.copies
	return m, tea.Batch(
		copyText(m.shownGen, s.url, connectSignInCopied),
		tea.Tick(signInCopiedLinger, func(time.Time) tea.Msg { return signInCopiedMsg{gen: gen, run: run, n: n} }),
	)
}

// pasteSignIn is Enter on the field: its line handed to the attempt
// (Attempt.Paste), whose wait then finishes the sign-in. Nothing happens
// before the attempt has begun, after an address was handed over, or with the
// field empty. Work that started while the box was open refuses it, the
// address kept for a later Enter. The attempt judges the line (plan 034
// §3.3): one that is not an address, one too long to be one, or one that is
// not this attempt's redirect is refused in the field, which is emptied,
// phrased from the attempt's refusal (pasteRefusalText); the line is never
// quoted. An address the attempt finds over — the browser's came first — is
// as good as handed: the wait is finishing.
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
	m.cdlg.key.Reset()
	switch err := s.att.Paste(line); {
	case err == nil, errors.Is(err, chatgptauth.ErrAttemptOver):
		m.cdlg.signIn.handed = true
		m.cdlg.keyErr = ""
	default:
		m.cdlg.keyErr = pasteRefusalText(err, s.redirect)
	}
	return m
}

// pasteRefusalText is why the attempt refused a pasted line, phrased from its
// refusal as craze auth phrases it (chatgptauth.PasteRefusalText), never
// quoting it — an earlier attempt's redirect as connectSignInEarlierText.
func pasteRefusalText(err error, redirect string) string {
	return chatgptauth.PasteRefusalText(err, sanitizeLine(redirect), connectSignInEarlierText)
}

// signInField is the field as the step draws it: every character masked, as
// the key field's are, whatever it holds — the redirect address included
// (plan 033 §3.13 as review r15 b amends it). r14 1's rule drew the text
// while it started with the redirect address, and a key glued onto that
// start, or onto a whole address, was drawn with it; no rule about what the
// text starts with can tell a key from the rest of an address. The step opens
// the field masked (openSignInStep), and the mask is put on again here as it
// is drawn, so no way text reaches it — a key, a terminal's paste, Ctrl+V's
// clipboard — draws any of it in any frame, the one before Enter refuses it
// included. What the person cannot see, signInStatus says.
func (m Model) signInField() textinput.Model {
	f := m.cdlg.key
	f.EchoMode, f.EchoCharacter = textinput.EchoPassword, connectMask
	return f
}

// signInStatus is the line under the field, and whether it says the field
// holds the redirect address (review r15 b): nothing while the field is empty
// or once an address was handed over; connectSignInReady while the field
// holds this attempt's redirect address as the browser is sent to it
// (redirectReady), so Enter will hand it over; and connectSignInPasteHint for
// anything else — a key, an address cut short, one still being typed. Both are
// fixed texts: the line never repeats any of the field's text.
func (m Model) signInStatus() (text string, ready bool) {
	s := m.cdlg.signIn
	v := m.cdlg.key.Value()
	switch {
	case s.handed || v == "":
		return "", false
	case redirectReady(v, s.redirect, s.state):
		return connectSignInReady, true
	}
	return connectSignInPasteHint, false
}

// redirectReady says whether value, the field's text, is this attempt's
// redirect address as the browser is sent to it after an approval: its
// surrounding blanks aside and with none inside, an address with the
// attempt's redirect address's scheme, host, port and path, no user, a code,
// and the attempt's state — state, which the step reads off the
// authorization address it shows (authState). That is what the attempt's
// Paste takes (chatgptauth.Attempt.Paste), and the code an approval's
// redirect carries. A key glued onto the address changes its path or its
// state, so it is not one; a declined approval's redirect, which has no code,
// is not either, though Enter still hands it over, to be told as declined.
// Nor is anything the attempt would refuse as too long on Enter: an address
// padded past connectRedirectMax never reads as ready (review r16 b).
func redirectReady(value, redirect, state string) bool {
	v := strings.TrimSpace(value)
	if redirect == "" || state == "" || strings.ContainsFunc(v, unicode.IsSpace) || len(v) > connectRedirectMax {
		return false
	}
	want, err := url.Parse(redirect)
	if err != nil {
		return false
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != want.Scheme || u.Host != want.Host || u.Path != want.Path || u.User != nil {
		return false
	}
	q := u.Query()
	return q.Get("code") != "" && q.Get("state") == state
}

// authState is the state the authorization address authURL carries, which
// the browser's redirect brings back to the attempt (chatgptauth.Begin puts
// it there), or "" when it carries none.
func authState(authURL string) string {
	u, err := url.Parse(authURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("state")
}

// hint is the line under the address: how the redirect comes back, or that
// it has (plan 034 Q12). On this machine with a listener it is
// connectSignInWaiting, as it always was. Over SSH (ssh) it leads with the
// paste path — the browser is on the person's own computer, where the
// listener's page cannot load — and, with a listener, offers the forward of
// its port: the redirect's, which a re-login's fallback may have moved off
// 1455. With no listener (paste-only: 1455 busy, or no listener to be had)
// the browser's page will not load anywhere, which it says, naming the
// address it lands on; it offers no forward, since nothing of craze's
// listens on that port — whatever holds it would get the redirect.
func (s signInState) hint() string {
	redirect := sanitizeLine(s.redirect)
	switch port := redirectPort(s.redirect); {
	case s.handed:
		return connectSignInHanded
	case s.listening && !s.ssh:
		return connectSignInWaiting
	case s.listening && port != "":
		return connectSignInSSH + fmt.Sprintf(connectSignInSSHListening, port, port)
	case s.ssh:
		return connectSignInSSH + fmt.Sprintf(connectSignInSSHPasteOnly, redirect)
	}
	return fmt.Sprintf(connectSignInPasteOnly, redirect)
}

// redirectPort is the port of the attempt's redirect address — the listener's
// — or "" when it names none.
func redirectPort(redirect string) string {
	u, err := url.Parse(redirect)
	if err != nil {
		return ""
	}
	return sanitizeLine(u.Port())
}

// listenerMissing is why the attempt has no listener, from its begin (plan
// 034 Q8; signInShown): 127.0.0.1:<port> in use, or not to be had — in craze
// auth's words (internal/cli) — or "" for an attempt that listens, one that
// is paste-only because it was asked to be (the TUI asks for none), or a
// begin that was not reported.
func listenerMissing(begun chatgptauth.Event) string {
	if begun.Kind != chatgptauth.EventBegin || begun.Mode != chatgptauth.ModePasteOnly {
		return ""
	}
	addr := "127.0.0.1"
	if begun.Port > 0 {
		addr = fmt.Sprintf("127.0.0.1:%d", begun.Port)
	}
	switch begun.Reason {
	case chatgptauth.ReasonPortBusy:
		return "craze is not listening for the browser: another program is using " + addr + "."
	case chatgptauth.ReasonListenFailed:
		return "craze is not listening for the browser: it could not listen on " + addr + "."
	}
	return ""
}

// signInLink is the OSC 8 id the address's rows are links under (plan 034
// Q9), or "" for plain rows: when the address is not one a link may point to
// (chatgptauth.LinkableAuthorizeURL: its bytes as the attempt made them, all
// printable ASCII, on production's authorize endpoint), when the attempt's id
// is not one — 8 lowercase hex digits, chatgptauth's form; the id is a
// parameter of the escape, so nothing else may go in it — or on a terminal
// that would draw the escape as text, or has no hyperlinks: TERM dumb, or the
// Linux console's linux, read through getenv (Config.Getenv).
func signInLink(url, id string, getenv func(string) string) string {
	switch getenv("TERM") {
	case "dumb", "linux":
		return ""
	}
	if !chatgptauth.LinkableAuthorizeURL(url) || !attemptIDForm(id) {
		return ""
	}
	return signInLinkPrefix + id
}

// attemptIDForm says id is an attempt id as chatgptauth makes one: 8
// lowercase hex digits.
func attemptIDForm(id string) bool {
	if len(id) != 8 {
		return false
	}
	for i := range len(id) {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// linkRow is row — one row of the address, already cut to the box ("…"),
// clamped and styled — as an OSC 8 hyperlink to url under id (plan 034 Q9):
// the open before it and the close after it, BEL-terminated (x/ansi's form),
// so the link ends inside the row, before the box pads and borders it. Every
// escape is zero cells wide, so the row is as wide as it was. Nothing
// downstream cuts the close off: dialogView's clamp and spliceRow's cuts keep
// every escape sequence past a cut (ansi.Truncate, ansi.TruncateLeft), which
// TestSignInLinkSurvivesEveryCut holds them to.
func linkRow(row, url, id string) string {
	return ansi.SetHyperlink(url, "id="+id) + row + ansi.ResetHyperlink()
}

// copiedLine is the box's line after a copy (plan 034 Q10): with a link to
// Cmd/Ctrl+click when nothing reached the clipboard, or without one for plain
// rows.
func (s signInState) copiedLine() string {
	if s.link != "" {
		return connectSignInCopiedLinkLine
	}
	return connectSignInCopiedLine
}

// signInFooter is the step's key hint at an inner width: the begin's; the
// wait's — a click and Ctrl+Y copying, in a shorter form where it does not
// fit, or Ctrl+Y alone with --no-mouse, where a click is the terminal's own;
// and the one once the address is handed over.
func (m Model) signInFooter(inner int) string {
	s := m.cdlg.signIn
	switch {
	case s.att == nil:
		return connectSignInStartHint
	case s.handed:
		return connectSignInHandedHint
	case !m.mouseEnabled:
		return connectSignInWaitHintKeys
	case lipgloss.Width(connectSignInWaitHint) > inner:
		return connectSignInWaitHintNarrow
	}
	return connectSignInWaitHint
}

// signInRowKind is what one row of the step is (signInPlan).
type signInRowKind int

const (
	signInRowTitle signInRowKind = iota
	signInRowStarting
	signInRowIntro
	signInRowAddress
	signInRowWhy
	signInRowHint
	signInRowField
	signInRowRefusal
	signInRowStatus
	signInRowOther
	signInRowCopied
	signInRowFooter
)

// signInRow is one row of the step: its kind, and its text, wrapped to the
// box — the field's, which draws itself, has none.
type signInRow struct {
	kind signInRowKind
	text string
}

// signInPlan is step three at an inner size (plan 034 §3.2): its rows in
// display order — the title, the introduction, the address, why craze is not
// listening (Q8), the hint, the field, the line under it (its refusal, or
// else its status, signInStatus), the browser's return from a different
// attempt (Q8), the copied line (Q10) and the footer — and whether the status
// says the field holds this attempt's redirect. The renderer (signInBody) and
// the hit-tester (signInClick) both take it, so a click copies only from a
// row that is drawn as the address.
//
// A short box keeps, in order: the title; the field and the line under it;
// the address's first row; the copied line, the answer to the click or the
// Ctrl+Y just made, which goes again at the next key; the footer, which says
// a click or Ctrl+Y copies the whole address; the browser's return from a
// different attempt, news the field cannot show; the hint, which says how the
// paste path ends; why craze is not listening, which the hint's paste path
// already works around; the introduction; and the rest of the address, its
// last row shown ending in "…" when it is cut. Once an
// address is handed over, the two lines about the paste path — the other
// attempt's return and why craze is not listening — are done with and go.
// Before the attempt has begun it is the title, the starting line and the
// footer.
type signInPlan struct {
	rows  []signInRow
	ready bool
}

func (m Model) signInPlan(inner, budget int) signInPlan {
	s := m.cdlg.signIn
	p := signInPlan{rows: []signInRow{{kind: signInRowTitle, text: connectSignInTitle}}}
	if s.att == nil {
		if budget >= 2 {
			p.rows = append(p.rows, signInRow{kind: signInRowStarting, text: connectSignInStarting})
		}
		if budget >= 3 {
			p.rows = append(p.rows, signInRow{kind: signInRowFooter, text: m.signInFooter(inner)})
		}
		return p
	}
	room := budget - 1 // the title
	take := func(rows []string) []string {
		rows = rows[:min(len(rows), max(room, 0))]
		room -= len(rows)
		return rows
	}
	var field []string
	if !s.handed {
		field = take([]string{""})
	}
	// A refusal is the field's news while it stands, and the status says
	// what the field holds otherwise: one of them under the field.
	var refusal, status []string
	if m.cdlg.keyErr != "" {
		refusal = take(dialogWrap(m.cdlg.keyErr, inner))
	} else if text, ready := m.signInStatus(); text != "" {
		status, p.ready = take(dialogWrap(text, inner)), ready
	}
	// The address is cut at the width, never at a word: a URL's hyphens and
	// slashes are no place to break it (dialogWrap would), and its rows are
	// what a person copies by hand when nothing else reaches their
	// clipboard.
	all := strings.Split(ansi.Hardwrap(sanitizeLine(s.url), inner, false), "\n")
	address := take(all[:min(len(all), 1)])
	var copied, other, why []string
	if s.copied {
		copied = take(dialogWrap(s.copiedLine(), inner))
	}
	footer := take([]string{m.signInFooter(inner)})
	if s.other && !s.handed {
		other = take(dialogWrap(connectSignInOtherAttempt, inner))
	}
	hint := take(dialogWrap(s.hint(), inner))
	if s.why != "" && !s.handed {
		why = take(dialogWrap(s.why, inner))
	}
	intro := take(dialogWrap(connectSignInIntro, inner))
	address = append(slices.Clone(address), take(all[len(address):])...)
	if n := len(address); n > 0 && n < len(all) {
		last := []rune(address[n-1])
		address[n-1] = string(last[:max(len(last)-1, 0)]) + "…"
	}
	for _, part := range []struct {
		kind  signInRowKind
		texts []string
	}{
		{signInRowIntro, intro}, {signInRowAddress, address}, {signInRowWhy, why}, {signInRowHint, hint},
		{signInRowField, field}, {signInRowRefusal, refusal}, {signInRowStatus, status},
		{signInRowOther, other}, {signInRowCopied, copied}, {signInRowFooter, footer},
	} {
		for _, text := range part.texts {
			p.rows = append(p.rows, signInRow{kind: part.kind, text: text})
		}
	}
	return p
}

// signInBody is step three drawn from its plan (signInPlan), each row styled
// by its kind and clamped to the box. Each row of the address is a link to
// the whole address — the attempt's own bytes — when the attempt's is one
// (signInState.link, linkRow): made last, after the row is cut, clamped and
// styled, so the link closes inside the row, before the box pads and borders
// it.
func (m Model) signInBody(inner, budget int) []string {
	s := m.cdlg.signIn
	p := m.signInPlan(inner, budget)
	rows := make([]string, 0, len(p.rows))
	for _, r := range p.rows {
		var row string
		switch r.kind {
		case signInRowTitle:
			row = m.dialogTitle(r.text, inner)
		case signInRowField:
			row = dialogInputView(m.signInField(), inner)
		case signInRowFooter:
			row = m.dialogFooter(r.text, inner)
		case signInRowAddress:
			row = styleFG(m.theme.FG).Render(clampWidth(r.text, inner))
			if s.link != "" {
				row = linkRow(row, s.url, s.link)
			}
		default:
			row = m.signInRowStyle(r.kind, p.ready).Render(clampWidth(r.text, inner))
		}
		rows = append(rows, row)
	}
	return rows
}

// signInRowStyle is a text row's style by its kind: a refusal's error colour;
// the status's ok colour when it says the field holds the redirect; the
// warning colour for the two lines about what went wrong — why craze is not
// listening, the other attempt's return; the ok colour for the copied line;
// dim for the rest.
func (m Model) signInRowStyle(kind signInRowKind, ready bool) lipgloss.Style {
	switch {
	case kind == signInRowRefusal:
		return styleFG(m.theme.Err)
	case kind == signInRowStatus && ready, kind == signInRowCopied:
		return styleFG(m.theme.OK)
	case kind == signInRowWhy, kind == signInRowOther:
		return styleFG(m.theme.Warn)
	}
	return styleFG(m.theme.Dim)
}

// signInClick is a press on body row i of the step (connectDialogClick),
// mapped through the plan the box was drawn from (signInPlan) at the box's
// own size: a row of the address copies the whole address (copySignInAddress,
// plan 034 Q10), as Ctrl+Y does; every other row does nothing.
func (m Model) signInClick(i int) (tea.Model, tea.Cmd) {
	p := m.signInPlan(m.lay.Dialog.W-dialogBorder, m.lay.Dialog.H-dialogBorder)
	if i < 0 || i >= len(p.rows) || p.rows[i].kind != signInRowAddress {
		return m, nil
	}
	return m.copySignInAddress()
}
