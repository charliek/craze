package tui

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// /connect's ChatGPT sign-in (plan 033 §3.13) as unit tests: the TUI's seams
// (Config.NativeDir, Config.Getenv) on a fixture directory whose table has the
// ChatGPT plan's provider, and chatgptauth behind connect_signin.go's seams —
// a stand-in attempt (fakeSignIn) for the step's own work, or, for the one test
// that signs in end to end, the real package against a loopback fake issuer
// (fakeIssuer). Never OpenAI, a browser, the developer's environment or their
// ~/.craze: TestMain fences the endpoints off (chatgptFence). The frames are
// native_connect_signin_golden_test.go's.

// chatgptFence is where TestMain points the ChatGPT plan's endpoints: port 9
// (discard) on loopback, which nothing serves, so a request that got past
// every seam is refused on this machine (internal/cli's fence).
const chatgptFence = "http://127.0.0.1:9"

// The stand-in attempt's account and addresses. The authorization address is
// the shape chatgptauth.Begin makes — the same parameters, encoded the same
// way — with fixed values for its random ones, so a frame of it can be a
// golden; nothing ever fetches it.
const (
	signInEmail       = "person@example.test"
	signInRedirect    = "http://127.0.0.1:1455/auth/callback"
	signInGoldenState = "golden-state-0123456789abcdefABCD"
	// signInPasted is the browser's redirect for the stand-in: a one-time
	// code and the attempt's state, as the address bar shows it.
	signInPasted = signInRedirect + "?code=golden-code-4f2a9c&scope=openid+profile+email&state=" + signInGoldenState
)

var signInURL = "https://auth.openai.com/api/accounts/authorize?" + url.Values{
	"client_id":             {"dynamic_agent_client"},
	"agent_name_hint":       {"craze"},
	"ext_agent_host_id":     {"urn:uuid:3f2b8c4e-1d6a-4e9b-8f7c-2a5d9e0b1c34"},
	"response_type":         {"code"},
	"redirect_uri":          {signInRedirect},
	"scope":                 {"openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"},
	"resource":              {"https://api.openai.com/v1"},
	"state":                 {signInGoldenState},
	"nonce":                 {"golden-nonce-0123456789abcdefABCD"},
	"code_challenge_method": {"S256"},
	"code_challenge":        {"golden-challenge-0123456789abcdefABCDEFGHIJKL"},
}.Encode()

// fakeSignIn is a stand-in chatgptauth.Attempt: its addresses are fixed, a
// pasted redirect is taken when it is the stand-in's address with its state,
// and Wait answers res (or err) once one arrives — pasted, or the browser's
// (browse) — the context's error once ctx ends, closing it, and
// ErrAttemptOver once it is closed. When it listens it holds a real loopback
// listener, so a test can see Esc close it.
type fakeSignIn struct {
	listening bool
	res       chatgptauth.Result
	err       error
	ln        net.Listener
	lnErr     error // the listener could not be opened: the begin's error

	mu      sync.Mutex
	over    bool
	pastes  int // redirects taken
	refused int // pastes refused as another attempt's

	got        chan struct{}
	closed     chan struct{}
	closeOnce  sync.Once
	waiting    chan struct{} // closed when Wait starts
	waited     chan struct{} // closed when Wait returns
	waitOnce   sync.Once
	waitedOnce sync.Once
}

func newFakeSignIn(listening bool, res chatgptauth.Result) *fakeSignIn {
	f := &fakeSignIn{
		listening: listening, res: res,
		got: make(chan struct{}, 1), closed: make(chan struct{}),
		waiting: make(chan struct{}), waited: make(chan struct{}),
	}
	if listening {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			f.lnErr = err
			return f
		}
		f.ln = ln
		// It accepts and drops: what a test asks of it is whether it is
		// still there.
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
	}
	return f
}

func (f *fakeSignIn) URL() string         { return signInURL }
func (f *fakeSignIn) RedirectURI() string { return signInRedirect }
func (f *fakeSignIn) Listening() bool     { return f.listening }

func (f *fakeSignIn) Paste(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme+"://"+u.Host+u.Path != signInRedirect {
		return chatgptauth.ErrRedirectMismatch
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.over {
		return chatgptauth.ErrAttemptOver
	}
	if u.Query().Get("state") != signInGoldenState {
		f.refused++
		return chatgptauth.ErrRedirectMismatch
	}
	f.over = true
	f.pastes++
	f.got <- struct{}{}
	return nil
}

// browse is the browser's redirect reaching the listener first.
func (f *fakeSignIn) browse() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.over {
		f.over = true
		f.got <- struct{}{}
	}
}

func (f *fakeSignIn) Wait(ctx context.Context) (chatgptauth.Result, error) {
	f.waitOnce.Do(func() { close(f.waiting) })
	defer f.waitedOnce.Do(func() { close(f.waited) })
	select {
	case <-f.got:
		f.Close()
		return f.res, f.err
	case <-ctx.Done():
		f.Close()
		return chatgptauth.Result{}, ctx.Err()
	case <-f.closed:
		return chatgptauth.Result{}, chatgptauth.ErrAttemptOver
	}
}

func (f *fakeSignIn) Close() {
	f.closeOnce.Do(func() {
		f.mu.Lock()
		f.over = true
		f.mu.Unlock()
		close(f.closed)
		if f.ln != nil {
			_ = f.ln.Close()
		}
	})
}

func (f *fakeSignIn) isClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}

func (f *fakeSignIn) counts() (pastes, refused int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pastes, f.refused
}

// listenerUp says the stand-in's listener still takes connections.
func (f *fakeSignIn) listenerUp() bool {
	if f.ln == nil {
		return false
	}
	c, err := net.DialTimeout("tcp", f.ln.Addr().String(), time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// waitEnded says whether the stand-in's Wait has returned within d.
func (f *fakeSignIn) waitEnded(d time.Duration) bool {
	select {
	case <-f.waited:
		return true
	case <-time.After(d):
		return false
	}
}

// signInStandIns are the step's seams over stand-ins: each begin makes a
// fakeSignIn with make (a frame script's every run begins one of its own) and
// records it; the notice's record and the model list are counted.
type signInStandIns struct {
	mu       sync.Mutex
	make     func() *fakeSignIn
	made     []*fakeSignIn
	dirs     []string
	beginErr error
	fetchErr error
	marks    atomic.Int32
	fetches  atomic.Int32
}

// standInSignIn swaps the step's seams for stand-ins until the test ends. Its
// cleanup puts them back, closes every stand-in — a frame run's quit leaves a
// wait running, as no production quit does (dropConnect) — and fails the test
// if a sign-in's command is still running a moment later.
func standInSignIn(t *testing.T, make func() *fakeSignIn) *signInStandIns {
	t.Helper()
	s := &signInStandIns{make: make}
	prevBegin, prevFetch, prevMark := beginSignIn, fetchPlanModels, markNoticeShown
	beginSignIn = func(ctx context.Context, dir string) (signInAttempt, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.dirs = append(s.dirs, dir)
		if s.beginErr != nil {
			return nil, s.beginErr
		}
		f := s.make()
		if f.lnErr != nil {
			return nil, f.lnErr
		}
		s.made = append(s.made, f)
		return f, nil
	}
	fetchPlanModels = func(ctx context.Context, dir string) (*chatgptauth.Models, error) {
		s.fetches.Add(1)
		if s.fetchErr != nil {
			return nil, s.fetchErr
		}
		return &chatgptauth.Models{Models: []chatgptauth.Model{{Slug: "gpt-5.6-sol"}, {Slug: "gpt-6-astra"}}}, nil
	}
	markNoticeShown = func(context.Context, string) error {
		s.marks.Add(1)
		return nil
	}
	t.Cleanup(func() {
		beginSignIn, fetchPlanModels, markNoticeShown = prevBegin, prevFetch, prevMark
		for _, f := range s.all() {
			f.Close()
		}
		awaitNoSignInRun(t)
	})
	return s
}

func (s *signInStandIns) all() []*fakeSignIn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fakeSignIn(nil), s.made...)
}

// signInRunFrames are the step's commands as a goroutine's stack names them:
// the begin's and the wait's.
var signInRunFrames = []string{"internal/tui.beginSignInCmd.func", "internal/tui.waitSignInCmd.func"}

// signInRunning says whether any goroutine is running one of the step's
// commands, or chatgptauth's listener (whose Serve runs in Begin's closure).
func signInRunning() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, frame := range append(signInRunFrames, "internal/chatgptauth.Begin.func") {
		if bytes.Contains(buf, []byte(frame)) {
			return true
		}
	}
	return false
}

// awaitNoSignInRun fails the test unless, within a few seconds, no goroutine
// is running a sign-in's command or listener: none outlives its step.
func awaitNoSignInRun(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for signInRunning() {
		if time.Now().After(deadline) {
			t.Fatal("a sign-in's command or listener is still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// signInFixture is connectFixture's directory with the ChatGPT plan's
// provider added to its table (catalog = false, so it is the provider's
// entry of the user's own, which the catalog-off table keeps): step one lists
// it between Beta and Gamma, the first provider with nothing funding it. With
// planOff, a registration whose account did not grant plan usage is there
// (modeltable.KeyPlanDisabled).
func signInFixture(t *testing.T, planOff bool) (dir string, getenv func(string) string) {
	t.Helper()
	home := t.TempDir()
	dir = filepath.Join(home, ".craze", "native")
	table := nativePickerTable()
	table.Providers["chatgpt"] = modeltable.Provider{Name: "ChatGPT plan", Driver: modeltable.DriverChatGPT}
	if err := modeltable.Save(dir, table); err != nil {
		t.Fatal(err)
	}
	if planOff {
		if err := os.MkdirAll(chatgptauth.AuthDir(dir), 0o700); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(chatgptauth.Client{ClientID: "oaiapp_fixture000000000001", Subject: "user-fixture", Email: signInEmail})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(chatgptauth.ClientFile(dir), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, func(k string) string {
		if k == "HOME" {
			return home
		}
		return nativePickerEnv(k)
	}
}

// signInModel is a started native model over signInFixture's directory with
// /connect open and step one read.
func signInModel(t *testing.T, planOff bool) (Model, string) {
	t.Helper()
	dir, getenv := signInFixture(t, planOff)
	m := connectModel(t, nativeStub(), dir, getenv)
	m, _ = typeCommand(t, m, "/connect")
	if p, ok := m.cdlg.provider(); !ok || !p.SignIn {
		t.Fatalf("step one does not open on the ChatGPT plan (on %q)", p.Name)
	}
	return m, dir
}

// beginStep is Enter on the ChatGPT plan's row, with the begin's command run
// and its answer applied: the step up, its attempt begun — and closed when the
// test ends, however it ends, so a real one's listener never outlives it. It
// answers the wait's command, which the test runs (runWait) or leaves.
func beginStep(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	m, cmd := press(m, enter())
	if m.cdlg.step != connectSignIn {
		t.Fatalf("Enter on the ChatGPT plan did not open the sign-in (step %d)", m.cdlg.step)
	}
	begun, ok := runCmd(cmd).(signInBegunMsg)
	if !ok {
		t.Fatal("Enter on the ChatGPT plan did not begin a sign-in")
	}
	if begun.att != nil {
		t.Cleanup(begun.att.Close)
	}
	tm, wait := m.Update(begun)
	return tm.(Model), wait
}

// runWait runs the wait's command on a goroutine of its own, as bubbletea
// would, and answers its message's channel. Before the wait starts, it
// registers a cleanup that ends m's run and joins the wait (review r14 6): a
// test that skips or fails on its way leaves no wait running into a later
// test's goroutine checks (awaitNoSignInRun).
func runWait(t *testing.T, m Model, wait tea.Cmd) <-chan tea.Msg {
	t.Helper()
	run := m.cdlg.signIn
	out := make(chan tea.Msg, 1)
	joined := make(chan struct{})
	t.Cleanup(func() {
		run.end()
		select {
		case <-joined:
		case <-time.After(10 * time.Second):
			t.Error("the sign-in's wait outlived its test")
		}
	})
	go func() {
		defer close(joined)
		out <- runCmd(wait)
	}()
	return out
}

// awaitMsg is the next message on c, or the test fails.
func awaitMsg(t *testing.T, c <-chan tea.Msg) tea.Msg {
	t.Helper()
	select {
	case msg := <-c:
		return msg
	case <-time.After(10 * time.Second):
		t.Fatal("the sign-in's wait did not answer")
		return nil
	}
}

// signInFieldShown says whether the open dialog draws the address field: a
// box row that opens with the field's prompt (the composer's own prompt is
// outside the box).
func signInFieldShown(view string) bool { return strings.Contains(view, "│❯ ") }

// transcriptText is every entry's text, one per line.
func transcriptText(m Model) string {
	var b strings.Builder
	for _, e := range m.main.entries() {
		b.WriteString(e.text + "\n")
	}
	return b.String()
}

// TestConnectSignInIsTheChatGPTPlansAction (§3.13): Enter on the ChatGPT
// plan's row opens "Sign in with ChatGPT" — the address, the address field —
// into the TUI's own directory (R2), and never a key field. The control is
// Enter on Gamma, a provider funded by a key, which still opens its masked
// key field and begins nothing.
func TestConnectSignInIsTheChatGPTPlansAction(t *testing.T) {
	s := standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(true, chatgptauth.Result{}) })
	m, dir := signInModel(t, false)
	m, wait := beginStep(t, m)
	if wait == nil {
		t.Fatal("the begun attempt was given no wait")
	}
	view := connectView(t, m)
	for _, want := range []string{connectSignInTitle, connectSignInIntro, signInURL[:40], "Waiting for the browser."} {
		if !strings.Contains(view, want) {
			t.Fatalf("the sign-in step does not show %q:\n%s", want, view)
		}
	}
	if !signInFieldShown(view) {
		t.Fatalf("the sign-in step has no address field:\n%s", view)
	}
	if strings.Contains(view, connectKeyTitle) {
		t.Fatalf("the ChatGPT plan got a key field:\n%s", view)
	}
	if m.cdlg.key.EchoMode != textinput.EchoPassword || m.signInField().EchoMode != textinput.EchoPassword {
		t.Fatal("the address field is not masked from the moment it opens")
	}
	if strings.Contains(view, connectSignInReady) || strings.Contains(view, connectSignInPasteHint) {
		t.Fatalf("the empty address field has a status line:\n%s", view)
	}
	if s.dirs[0] != dir {
		t.Fatalf("the sign-in went to %s, not the TUI's directory %s", s.dirs[0], dir)
	}
	m = pressKey(t, m, tea.KeyEsc)

	// The control: a key-funded provider's row opens its key field.
	m = pressKey(t, m, tea.KeyDown)
	if p, _ := m.cdlg.provider(); p.ID != "gamma" {
		t.Fatalf("the control is not on Gamma (%q)", p.ID)
	}
	m, cmd := press(m, enter())
	if m.cdlg.step != connectKey || m.cdlg.key.EchoMode != textinput.EchoPassword {
		t.Fatal("the control: Gamma's row did not open a masked key field")
	}
	if _, began := runCmd(cmd).(signInBegunMsg); began || len(s.all()) != 1 {
		t.Fatal("the control: Gamma's row began a sign-in")
	}
}

// TestConnectSignInEscClosesTheListener (§3.13): Esc on the step ends its
// attempt — the stand-in closed, its listener gone, its wait returned — goes
// back to step one, and leaves no sign-in command running. The control is the
// same step left open: the listener takes connections and the wait is still
// waiting, so the checks can tell the two apart.
func TestConnectSignInEscClosesTheListener(t *testing.T) {
	standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(true, chatgptauth.Result{}) })
	m, _ := signInModel(t, false)
	m, wait := beginStep(t, m)
	done := runWait(t, m, wait)
	f := m.cdlg.signIn.att.(*fakeSignIn)
	<-f.waiting

	// The control first: open, it listens and waits.
	if !f.listenerUp() || f.waitEnded(50*time.Millisecond) || f.isClosed() {
		t.Fatal("the control: an open step's attempt is not listening and waiting")
	}

	m = pressKey(t, m, tea.KeyEsc)
	if m.cdlg.step != connectPick || m.dialog != dialogConnect {
		t.Fatal("Esc did not go back to step one")
	}
	if !f.isClosed() || f.listenerUp() {
		t.Fatal("Esc left the attempt's listener open")
	}
	if !f.waitEnded(5 * time.Second) {
		t.Fatal("Esc left the attempt's wait running")
	}
	// The wait's answer, for a step that has gone, says nothing.
	tm, _ := m.Update(awaitMsg(t, done))
	m = tm.(Model)
	if strings.Contains(transcriptText(m), "/connect") {
		t.Fatalf("a cancelled sign-in wrote to the transcript:\n%s", transcriptText(m))
	}
	awaitNoSignInRun(t)
}

// TestConnectSignInEndsOnEveryWayOut: the attempt is closed however the step
// goes — the box closed (a card, another dialog: closeDialog), dropped on a
// quit or the session's end (dropConnect), a switch to another session
// (withSession) — and an attempt that begins after its step has gone is
// closed where its answer lands. The control: the begin's answer for the step
// still open is adopted, not closed.
func TestConnectSignInEndsOnEveryWayOut(t *testing.T) {
	standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(true, chatgptauth.Result{}) })
	for _, tc := range []struct {
		name string
		out  func(Model) Model
	}{
		{"closeDialog", func(m Model) Model { return m.closeDialog(true) }},
		{"dropConnect", func(m Model) Model { return m.dropConnect() }},
		{"withSession", func(m Model) Model { return m.withSession(sessionSeed{workspace: t.TempDir()}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := signInModel(t, false)
			m, _ = beginStep(t, m)
			f := m.cdlg.signIn.att.(*fakeSignIn)
			if f.isClosed() {
				t.Fatal("the control: an open step's attempt is closed")
			}
			if _ = tc.out(m); !f.isClosed() || f.listenerUp() {
				t.Fatalf("%s left the attempt's listener open", tc.name)
			}
		})
	}
	t.Run("a late begin", func(t *testing.T) {
		m, _ := signInModel(t, false)
		m, cmd := press(m, enter())
		begun := runCmd(cmd).(signInBegunMsg)
		m = pressKey(t, m, tea.KeyEsc) // the step goes before its begin's answer lands
		tm, wait := m.Update(begun)
		m = tm.(Model)
		f := begun.att.(*fakeSignIn)
		if !f.isClosed() || wait != nil || m.cdlg.signIn.att != nil {
			t.Fatal("an attempt that began after its step had gone was not closed")
		}
	})
}

// TestConnectSignInPasteIsRefusedUnquoted: what Enter hands the attempt.
// A line that is not an address — a key pasted out of habit — is refused in
// the field, emptied, never quoted and never handed over; so is another
// attempt's address. The control is the stand-in's own redirect, which is
// handed over once and ends the field.
func TestConnectSignInPasteIsRefusedUnquoted(t *testing.T) {
	standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(false, chatgptauth.Result{}) })
	m, _ := signInModel(t, false)
	m, _ = beginStep(t, m)
	f := m.cdlg.signIn.att.(*fakeSignIn)

	// The field draws the key masked from the frame it lands in (review r14
	// 1), so that frame is screened as the one after Enter is; Enter takes it
	// away and never repeats it.
	m, _ = press(m, pasteKey(connectCanary))
	connectLeak(t, m, nil)
	m, _ = press(m, enter())
	connectLeak(t, m, nil)
	if !strings.Contains(m.cdlg.keyErr, "never by an API key") || m.cdlg.key.Value() != "" {
		t.Fatalf("a key was not refused as one, the field emptied (%q)", m.cdlg.keyErr)
	}
	if p, r := f.counts(); p != 0 || r != 0 {
		t.Fatal("a line that is not an address reached the attempt")
	}

	m, _ = press(m, pasteKey(signInRedirect+"?code=other&state=another-attempts-state"))
	m, _ = press(m, enter())
	if !strings.Contains(m.cdlg.keyErr, "not this sign-in's redirect address") || m.cdlg.key.Value() != "" {
		t.Fatalf("another attempt's address was not refused (%q)", m.cdlg.keyErr)
	}
	if strings.Contains(connectView(t, m), "another-attempts-state") {
		t.Fatal("the refusal quotes the pasted address")
	}

	// The control.
	m, _ = press(m, pasteKey(signInPasted))
	m, _ = press(m, enter())
	if p, _ := f.counts(); p != 1 || !m.cdlg.signIn.handed || m.cdlg.keyErr != "" {
		t.Fatal("the control: the attempt's own address was not handed over")
	}
	if view := connectView(t, m); !strings.Contains(view, connectSignInHanded) || signInFieldShown(view) {
		t.Fatalf("a handed address leaves the field up:\n%s", view)
	}
}

// TestConnectSignInRefusedWhileWorkRuns (§3.13, plan 031 R1): with work
// running the step does not open — the dialog closes with /connect's refusal
// and no attempt begins — and a pasted address's Enter keeps the address for
// a later one. The control is the same Enter with nothing running.
func TestConnectSignInRefusedWhileWorkRuns(t *testing.T) {
	s := standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(false, chatgptauth.Result{}) })
	m, _ := signInModel(t, false)
	m.status = statusWorking
	m, _ = press(m, enter())
	if m.dialog == dialogConnect || m.cdlg.signIn.cancel != nil || len(s.all()) != 0 || !strings.Contains(transcriptText(m), connectBusyText) {
		t.Fatal("the sign-in opened while work ran")
	}

	m, _ = signInModel(t, false)
	m, _ = beginStep(t, m)
	m, _ = press(m, pasteKey(signInPasted))
	if !strings.Contains(connectView(t, m), connectSignInReady) {
		t.Fatal("the control: the pasted address has no status line before the refusal")
	}
	m.status = statusWorking
	m, _ = press(m, enter())
	if m.cdlg.keyErr != connectBusySignInText || m.cdlg.key.Value() != signInPasted || m.cdlg.signIn.handed {
		t.Fatal("a paste's Enter was not refused, the address kept, while work ran")
	}
	// The refusal is the line under the field while it stands: no "press
	// enter" beside "finish the running work first".
	if view := connectView(t, m); !strings.Contains(view, "Finish or stop the running work first") || strings.Contains(view, connectSignInReady) {
		t.Fatalf("the refusal is not the one line under the field:\n%s", view)
	}
	// The control.
	m.status = statusIdle
	m, _ = press(m, enter())
	if !m.cdlg.signIn.handed {
		t.Fatal("the control: the address was not handed over once the work ended")
	}
}

// TestConnectSignInCopiesTheAddress: Ctrl+Y on the step copies the
// authorization address through the TUI's copy, whole, with the status row's
// note. The control: Ctrl+Y before the attempt has begun copies nothing.
func TestConnectSignInCopiesTheAddress(t *testing.T) {
	rec := captureCopies(t)
	standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(false, chatgptauth.Result{}) })
	m, _ := signInModel(t, false)
	m, cmd := press(m, enter()) // the begin not yet answered
	if _, copied := press(m, tea.KeyMsg{Type: tea.KeyCtrlY}); copied != nil || len(rec.copies()) != 0 {
		t.Fatal("the control: Ctrl+Y copied before there was an address")
	}
	tm, _ := m.Update(runCmd(cmd))
	m = tm.(Model)
	_, copied := press(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	done, ok := runCmd(copied).(clipboardDoneMsg)
	if !ok || done.note != connectSignInCopied {
		t.Fatal("Ctrl+Y did not copy with its note")
	}
	if c := rec.copies(); len(c) != 1 || c[0] != signInURL {
		t.Fatalf("Ctrl+Y copied %q, not the address", c)
	}
}

// TestConnectSignInClipboardPasteLandsInItsField: Ctrl+V on the step reads
// the clipboard into the address field and nowhere else, and once the address
// is handed over a late one is dropped. The control is the same paste while
// the field is open, which lands.
func TestConnectSignInClipboardPasteLandsInItsField(t *testing.T) {
	standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(false, chatgptauth.Result{}) })
	m, _ := signInModel(t, false)
	m, _ = beginStep(t, m)
	field := m.cdlg.openField()
	if field == (keyField{}) {
		t.Fatal("the step's field is no field a paste can name")
	}
	tm, _ := m.Update(pasteMsg{text: signInPasted, shownGen: m.shownGen, key: field})
	m = tm.(Model)
	if m.cdlg.key.Value() != signInPasted || m.input.Value() != "" {
		t.Fatal("the control: a clipboard paste did not land in the address field alone")
	}
	m, _ = press(m, enter())
	tm, _ = m.Update(pasteMsg{text: "late", shownGen: m.shownGen, key: field})
	if m = tm.(Model); m.cdlg.key.Value() != "" || m.input.Value() != "" {
		t.Fatal("a paste after the address was handed over landed")
	}
}

// TestConnectSignInBrowserWinsTheRace: the listener's redirect arriving while
// an address sits in the field finishes the sign-in, and a later Enter on the
// field is as good as handed — nothing is refused or quoted. The control is
// the transcript before the answer lands: nothing signed in yet.
func TestConnectSignInBrowserWinsTheRace(t *testing.T) {
	standInSignIn(t, func() *fakeSignIn {
		return newFakeSignIn(true, chatgptauth.Result{Email: signInEmail, PlanUsage: true})
	})
	m, _ := signInModel(t, false)
	m, wait := beginStep(t, m)
	done := runWait(t, m, wait)
	f := m.cdlg.signIn.att.(*fakeSignIn)
	m, _ = press(m, pasteKey(signInPasted))
	f.browse()
	msg := awaitMsg(t, done)
	if strings.Contains(transcriptText(m), signedInPrefix) {
		t.Fatal("the control: signed in before the answer landed")
	}
	m, _ = press(m, enter())
	if m.cdlg.keyErr != "" || !m.cdlg.signIn.handed {
		t.Fatalf("an Enter after the browser's redirect was refused (%q)", m.cdlg.keyErr)
	}
	tm, _ := m.Update(msg)
	if m = tm.(Model); m.dialog == dialogConnect || !strings.Contains(transcriptText(m), signedInPrefix+" as "+signInEmail+".") {
		t.Fatalf("the browser's redirect did not finish the sign-in:\n%s", transcriptText(m))
	}
	if p, _ := f.counts(); p != 0 {
		t.Fatal("the field's address reached an attempt the browser had finished")
	}
}

// TestConnectSignInFailures: a sign-in that fails closes the box with an
// error row in chatgptauth's words, the browser's refusal in craze auth's;
// one that could not begin says so. The control is a sign-in that works,
// which writes no error row.
func TestConnectSignInFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		beginErr error
		waitErr  error
		want     string
	}{
		{"declined", nil, chatgptauth.ErrAccessDenied, "/connect: the sign-in was declined in the browser; nothing was changed"},
		{"exchange", nil, &chatgptauth.OAuthError{Step: "exchange", Status: 400, Code: "invalid_grant"}, "/connect: exchange refused (HTTP 400, invalid_grant)"},
		{"begin", errors.New("chatgptauth: the host id is malformed"), nil, "/connect: the sign-in could not start: the host id is malformed"},
		{"control", nil, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := standInSignIn(t, func() *fakeSignIn {
				f := newFakeSignIn(false, chatgptauth.Result{Email: signInEmail, PlanUsage: true})
				f.err = tc.waitErr
				return f
			})
			s.beginErr = tc.beginErr
			m, _ := signInModel(t, false)
			m, cmd := press(m, enter())
			tm, wait := m.Update(runCmd(cmd))
			m = tm.(Model)
			if tc.beginErr == nil {
				done := runWait(t, m, wait)
				m, _ = press(m, pasteKey(signInPasted))
				m, _ = press(m, enter())
				tm, _ = m.Update(awaitMsg(t, done))
				m = tm.(Model)
			}
			if m.dialog == dialogConnect {
				t.Fatal("the box is still open")
			}
			var errs []string
			for _, e := range m.main.entries() {
				if e.kind == entryError {
					errs = append(errs, e.text)
				}
			}
			switch {
			case tc.want == "" && len(errs) != 0:
				t.Fatalf("the control wrote an error row: %q", errs)
			case tc.want != "" && (len(errs) != 1 || errs[0] != tc.want):
				t.Fatalf("error rows %q, want %q", errs, tc.want)
			}
		})
	}
}

// TestConnectSignInStepOneMarks (X129): step one tags the ChatGPT plan by its
// sign-in — "plan usage off" for an account that did not grant it, nothing
// when not signed in — and never as a provider with an unusable stored key.
// The control is the same row with nothing signed in, which has no tag.
func TestConnectSignInStepOneMarks(t *testing.T) {
	for _, tc := range []struct {
		planOff bool
		want    string
	}{{true, connectPlanOffMark}, {false, ""}} {
		m, _ := signInModel(t, tc.planOff)
		p, _ := m.cdlg.provider()
		if got := connectMark(p); got != tc.want {
			t.Fatalf("planOff %v: the ChatGPT plan's mark is %q, want %q", tc.planOff, got, tc.want)
		}
	}
	if connectMark(modeltable.ProviderInfo{SignIn: true, StoredProblem: modeltable.ErrKeyTooShort}) != "" {
		t.Fatal("a sign-in provider is marked by a key stored for it by hand")
	}
}

// TestRedirectReady (review r15 b): what the line under the address field
// calls this attempt's redirect address — the attempt's own address, as the
// browser is sent to it after an approval, with a code and the attempt's
// state, its surrounding blanks aside — and what it does not: a key alone, a
// key glued onto the address's start or onto a whole address, or after a
// blank; the address cut short, with no code, with another attempt's state,
// on another attempt's port, scheme or host, or with a user; and anything
// before the attempt has begun. The true rows are the controls.
func TestRedirectReady(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		ready bool
	}{
		{"the redirect", signInPasted, true},
		{"the redirect between blanks", "  " + signInPasted + " ", true},
		{"its parameters in another order", signInRedirect + "?state=" + signInGoldenState + "&code=c", true},
		{"a key alone", connectCanary, false},
		{"a key glued onto the address's start", signInRedirect + connectCanary, false},
		{"a key glued onto the address's start, a query after", signInRedirect + connectCanary + "?code=c&state=" + signInGoldenState, false},
		{"a key glued onto the whole address", signInPasted + connectCanary, false},
		{"a key after a blank", signInPasted + " " + connectCanary, false},
		{"the address alone", signInRedirect, false},
		{"the address being typed", "http://127.0", false},
		{"no code", signInRedirect + "?state=" + signInGoldenState, false},
		{"an empty code", signInRedirect + "?code=&state=" + signInGoldenState, false},
		{"another attempt's state", signInRedirect + "?code=c&state=another-attempts-state", false},
		{"no state", signInRedirect + "?code=c", false},
		{"another port", "http://127.0.0.1:50123/auth/callback?code=c&state=" + signInGoldenState, false},
		{"another scheme", "https://127.0.0.1:1455/auth/callback?code=c&state=" + signInGoldenState, false},
		{"another host", "http://localhost:1455/auth/callback?code=c&state=" + signInGoldenState, false},
		{"another path", "http://127.0.0.1:1455/auth/other?code=c&state=" + signInGoldenState, false},
		{"a user", "http://u@127.0.0.1:1455/auth/callback?code=c&state=" + signInGoldenState, false},
		{"nothing", "", false},
	} {
		if got := redirectReady(tc.value, signInRedirect, signInGoldenState); got != tc.ready {
			t.Errorf("%s: redirectReady = %v, want %v", tc.name, got, tc.ready)
		}
	}
	if redirectReady(signInPasted, "", signInGoldenState) || redirectReady(signInPasted, signInRedirect, "") {
		t.Error("the redirect is ready before the attempt has begun")
	}
	if authState(signInURL) != signInGoldenState {
		t.Error("the state is not read off the authorization address")
	}
}

// signInFieldRow is the address field's row in view, from its prompt to the
// box's border, or "" when the box has no field.
func signInFieldRow(view string) string {
	for _, line := range strings.Split(view, "\n") {
		if _, rest, ok := strings.Cut(line, "│❯ "); ok {
			row, _, _ := strings.Cut(rest, "│")
			return row
		}
	}
	return ""
}

// assertSignInFieldMasked fails the test unless view — a frame already
// screened for the canary key, a box of the widest a dialog gets — draws its
// address field as value's masks alone: one per character, as far as the
// field is wide, and nothing else but the cursor's blank.
func assertSignInFieldMasked(t *testing.T, view, value string) {
	t.Helper()
	row := signInFieldRow(view)
	want := min(len([]rune(value)), dialogFieldWidth(dialogMaxWidth-dialogBorder))
	if strings.Trim(row, string(connectMask)+" ") != "" || strings.Count(row, string(connectMask)) != want {
		t.Fatalf("the field is not drawn as %d masks (row %q, the field holding %d bytes)", want, row, len(value))
	}
}

// TestConnectSignInFieldIsAlwaysMasked (review r15 b, amending §3.13's
// unmasked field and superseding r14 1's address-only mask): whatever the
// address field holds, the frame right after it lands — before Enter — draws
// it masked, one mask per character: a key alone, pasted or typed a
// character at a time; a key glued onto the redirect address's start, which
// r14 1's rule drew whole; a key glued onto the whole redirect address; and
// the redirect address itself. The key is nowhere in any of those frames
// (connectLeak), nor the address's one-time code. The line under the field
// tells them apart without a word of them: the redirect is "That's the
// redirect address", the rest "Paste the whole address" — each the other's
// control — and an emptied field has no line.
func TestConnectSignInFieldIsAlwaysMasked(t *testing.T) {
	const code = "golden-code-4f2a9c" // signInPasted's one-time code
	for _, tc := range []struct {
		name  string
		value string
		typed bool
		ready bool
	}{
		{"a key", connectCanary, false, false},
		{"a key typed", connectCanary, true, false},
		{"a key glued onto the address's start", signInRedirect + connectCanary, false, false},
		{"a key glued onto the address", signInPasted + connectCanary, false, false},
		{"the redirect", signInPasted, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(false, chatgptauth.Result{}) })
			m, _ := signInModel(t, false)
			m, _ = beginStep(t, m)
			if tc.typed {
				for _, r := range tc.value {
					m, _ = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
					connectLeak(t, m, nil)
					assertSignInFieldMasked(t, connectView(t, m), m.cdlg.key.Value())
				}
			} else {
				m, _ = press(m, pasteKey(tc.value))
			}
			connectLeak(t, m, nil)
			view := connectView(t, m)
			assertSignInFieldMasked(t, view, tc.value)
			if strings.Contains(view, code) {
				t.Fatal("the frame shows the pasted address's one-time code")
			}
			want, not := connectSignInPasteHint, connectSignInReady
			if tc.ready {
				want, not = not, want
			}
			if !strings.Contains(view, want) || strings.Contains(view, not) {
				t.Fatalf("the line under the field is not %q:\n%s", want, view)
			}
			if _, ready := m.signInStatus(); ready != tc.ready {
				t.Fatalf("signInStatus says ready %v, want %v", ready, tc.ready)
			}

			// Emptied, the field has no line under it.
			m.cdlg.key.Reset()
			if view := connectView(t, m); strings.Contains(view, connectSignInReady) || strings.Contains(view, connectSignInPasteHint) {
				t.Fatalf("the emptied field still has a status line:\n%s", view)
			}
		})
	}
}

// TestConnectSignInEndsAtFinishRun (review r14 2): the exits no Update sees —
// SIGTERM, SIGHUP, a program failure — all land in finishRun, which ends the
// sign-in through the model's shared set, reached from the model Run started
// with: the listener closed and the wait returned, during the wait; an
// attempt Begin returned whose answer the program never read closed too, its
// wait returning at once; and one Begin returns after the shutdown closed as
// it does. The controls are each attempt just before finishRun — listening,
// its wait still waiting — which is where r14 found them left.
func TestConnectSignInEndsAtFinishRun(t *testing.T) {
	t.Run("during the wait", func(t *testing.T) {
		standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(true, chatgptauth.Result{}) })
		initial, _ := signInModel(t, false)
		m, wait := beginStep(t, initial)
		done := runWait(t, m, wait)
		f := m.cdlg.signIn.att.(*fakeSignIn)
		<-f.waiting
		if !f.listenerUp() || f.waitEnded(50*time.Millisecond) || f.isClosed() {
			t.Fatal("the control: the attempt is not listening and waiting before the shutdown")
		}
		_, _ = finishRun(io.Discard, nil, initial, nil)
		if !f.isClosed() || f.listenerUp() {
			t.Fatal("finishRun left the attempt's listener open")
		}
		if _, ok := awaitMsg(t, done).(signInDoneMsg); !ok || !f.waitEnded(5*time.Second) {
			t.Fatal("finishRun left the attempt's wait running")
		}
		awaitNoSignInRun(t)
	})
	t.Run("its begin never read", func(t *testing.T) {
		standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(true, chatgptauth.Result{}) })
		initial, _ := signInModel(t, false)
		_, cmd := press(initial, enter())
		begun := runCmd(cmd).(signInBegunMsg) // never handed to Update
		f := begun.att.(*fakeSignIn)
		if f.isClosed() || !f.listenerUp() {
			t.Fatal("the control: an attempt whose answer was not read is not listening before the shutdown")
		}
		_, _ = finishRun(io.Discard, nil, initial, nil)
		if !f.isClosed() || f.listenerUp() {
			t.Fatal("finishRun left an unread begin's listener open")
		}
		waited := make(chan tea.Msg, 1)
		go func() { waited <- runCmd(begun.wait) }()
		if _, ok := awaitMsg(t, waited).(signInDoneMsg); !ok || !f.waitEnded(5*time.Second) {
			t.Fatal("an unread begin's wait did not return at once")
		}
		awaitNoSignInRun(t)
	})
	t.Run("a begin after the shutdown", func(t *testing.T) {
		s := standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(true, chatgptauth.Result{}) })
		release := make(chan struct{})
		standIn := beginSignIn
		beginSignIn = func(ctx context.Context, dir string) (signInAttempt, error) {
			<-release
			return standIn(ctx, dir)
		}
		t.Cleanup(func() { beginSignIn = standIn })
		initial, _ := signInModel(t, false)
		_, cmd := press(initial, enter())
		answer := make(chan tea.Msg, 1)
		go func() { answer <- runCmd(cmd) }()
		_, _ = finishRun(io.Discard, nil, initial, nil)
		close(release)
		begun, ok := awaitMsg(t, answer).(signInBegunMsg)
		if !ok || begun.att != nil || begun.err == nil {
			t.Fatal("a begin after the shutdown was answered as begun")
		}
		made := s.all()
		if len(made) != 1 || !made[0].isClosed() || made[0].listenerUp() {
			t.Fatal("a begin after the shutdown left its listener open")
		}
		awaitNoSignInRun(t)
	})
}

// ---------------------------------------------------------------- end to end

// fakeIssuer is a loopback OpenID issuer, token endpoint, key set and model
// list in one httptest server, issuing dummy tokens: just enough of the
// sign-in's server side for chatgptauth's own flow to finish against it. Every
// token value it issues is kept, so a test can look for each in what the TUI
// shows.
type fakeIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu      sync.Mutex
	pending map[string]fakeCode
	access  map[string]bool
	issued  []string
	models  int
}

// fakeCode is an authorization code the fake issued and has not exchanged.
type fakeCode struct{ clientID, redirect, nonce, challenge string }

const (
	issuerClient  = "oaiapp_tuifake0000000000001"
	issuerSubject = "user-tui-fake-0001"
	issuerKid     = "tui-fake-kid"
)

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: k, pending: map[string]fakeCode{}, access: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	t.Setenv(chatgptauth.IssuerEnv, f.srv.URL)
	t.Setenv(chatgptauth.APIEnv, f.srv.URL+"/v1")
	return f
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// authorize stands in for the browser and the person's approval: it reads the
// attempt's authorization address as the server would and answers the
// redirect address the browser would be sent to — the one a person pastes.
func (f *fakeIssuer) authorize(t *testing.T, authURL string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil || !strings.HasPrefix(authURL, f.srv.URL+"/api/accounts/authorize?") {
		t.Fatal("the authorization address is not the fake issuer's")
	}
	q := u.Query()
	if q.Get("client_id") != "dynamic_agent_client" || q.Get("id_token_hint") != "" {
		t.Fatal("the first sign-in is not a registration, or sends a token hint")
	}
	code := "tui-code-" + randomHex(8)
	f.mu.Lock()
	f.pending[code] = fakeCode{clientID: issuerClient, redirect: q.Get("redirect_uri"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	f.mu.Unlock()
	back := url.Values{"code": {code}, "state": {q.Get("state")}, "client_id": {issuerClient}, "scope": {q.Get("scope")}}
	return q.Get("redirect_uri") + "?" + back.Encode()
}

func (f *fakeIssuer) values() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.issued...)
}

func (f *fakeIssuer) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeIssuer) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		f.json(w, 200, map[string]any{"issuer": f.srv.URL, "jwks_uri": f.srv.URL + "/jwks", "revocation_endpoint": f.srv.URL + "/api/accounts/oauth/revoke"})
	case "/jwks":
		f.json(w, 200, map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": issuerKid, "use": "sig", "alg": "RS256",
			"n": b64url(f.key.N.Bytes()), "e": b64url(big.NewInt(int64(f.key.E)).Bytes()),
		}}})
	case "/api/accounts/oauth/token":
		f.token(w, r)
	case "/v1/models":
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		live := f.access[tok]
		f.models++
		f.mu.Unlock()
		if !live {
			f.json(w, 401, map[string]any{"error": map[string]any{"code": "invalid_api_key"}})
			return
		}
		f.json(w, 200, map[string]any{"models": []any{
			map[string]any{"slug": "gpt-5.6-sol", "display_name": "GPT-5.6-Sol", "visibility": "list", "priority": 1, "context_window": 272000,
				"input_modalities": []any{"text"}, "supported_reasoning_levels": []any{map[string]any{"effort": "low"}}, "default_reasoning_level": "low"},
		}})
	default:
		http.NotFound(w, r)
	}
}

// token is the exchange: the code, its client, its redirect and its PKCE
// verifier checked, then dummy tokens and an id_token signed for the nonce.
func (f *fakeIssuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		f.json(w, 400, map[string]any{"error": "unsupported_grant_type"})
		return
	}
	form := r.PostForm
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pending[form.Get("code")]
	delete(f.pending, form.Get("code"))
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	if !ok || p.clientID != form.Get("client_id") || p.redirect != form.Get("redirect_uri") || b64url(sum[:]) != p.challenge {
		f.json(w, 400, map[string]any{"error": "invalid_grant"})
		return
	}
	hdr, _ := json.Marshal(map[string]any{"alg": "none"})
	claims, _ := json.Marshal(map[string]any{"client_id": issuerClient, "r": randomHex(8)})
	access := b64url(hdr) + "." + b64url(claims) + ".tuifakesig" + randomHex(4)
	refresh := "tui-fake-refresh-" + randomHex(12)
	now := time.Now()
	idHdr, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": issuerKid, "typ": "JWT"})
	idClaims, _ := json.Marshal(map[string]any{
		"iss": f.srv.URL, "aud": issuerClient, "sub": issuerSubject, "email": signInEmail, "nonce": p.nonce,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	signing := b64url(idHdr) + "." + b64url(idClaims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		f.json(w, 500, map[string]any{"error": "server_error"})
		return
	}
	id := signing + "." + b64url(sig)
	f.access[access] = true
	f.issued = append(f.issued, access, refresh, id)
	f.json(w, 200, map[string]any{
		"access_token": access, "refresh_token": refresh, "id_token": id, "token_type": "Bearer",
		"expires_in": 3600, "scope": "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct",
	})
}

// TestConnectSignInPastedRedirectAgainstFakeIssuer (§3.13, §5 C16): the whole
// sign-in through /connect, with chatgptauth itself behind the step against a
// fake issuer: the address shown is the issuer's, a registration with no
// token hint; the browser's redirect, pasted into the field, finishes the
// sign-in; the TUI says as whom, shows the notice and records it, fetches the
// plan's models into its own directory and says what this conversation gets.
// No token value the issuer issued reaches the frame or the transcript, and
// the listener (when 1455 was free to bind) is closed. The control is the
// directory before the paste: not signed in.
func TestConnectSignInPastedRedirectAgainstFakeIssuer(t *testing.T) {
	iss := newFakeIssuer(t)
	m, dir := signInModel(t, false)
	m, wait := beginStep(t, m)
	done := runWait(t, m, wait)
	s := m.cdlg.signIn
	if st, err := chatgptauth.ReadStatus(dir); err != nil || st.SignedIn {
		t.Fatalf("the control: signed in before the paste (%v)", err)
	}

	redirect := iss.authorize(t, s.url)
	m, _ = press(m, pasteKey(redirect))
	// chatgptauth's own redirect, masked like anything else, is known for
	// this attempt's by the state its authorization address carries (review
	// r15 b): the line under the field says so, and the one-time code is not
	// in the frame.
	ru, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}
	view := connectView(t, m)
	if !signInFieldShown(view) || !strings.Contains(view, connectSignInReady) || strings.Contains(view, ru.Query().Get("code")) {
		t.Fatalf("the pasted redirect is not masked and known as this attempt's:\n%s", view)
	}
	m, _ = press(m, enter())
	if !m.cdlg.signIn.handed || m.cdlg.keyErr != "" {
		t.Fatalf("the pasted redirect was not handed to the attempt (%q)", m.cdlg.keyErr)
	}
	tm, finish := m.Update(awaitMsg(t, done))
	m = tm.(Model)
	if m.dialog == dialogConnect {
		t.Fatalf("the box is still open:\n%s", transcriptText(m))
	}
	tm, _ = m.Update(runCmd(finish))
	m = tm.(Model)

	secrets := iss.values()
	if len(secrets) != 3 {
		t.Fatalf("the issuer issued %d token values, want 3", len(secrets))
	}
	connectLeak(t, m, nil, secrets...)
	text := transcriptText(m)
	for _, want := range []string{
		signedInPrefix + " as " + signInEmail + ".", chatgptauth.NoticeTitle, chatgptauth.Notice,
		"ChatGPT plan models: chatgpt/gpt-5.6-sol", signedInSessionNote,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the transcript does not say %q:\n%s", want, text)
		}
	}
	st, err := chatgptauth.ReadStatus(dir)
	if err != nil || !st.SignedIn || !st.PlanUsage || st.Email != signInEmail {
		t.Fatalf("the TUI's directory is not signed in with plan usage (%+v, %v)", st, err)
	}
	if c, err := chatgptauth.ReadClient(dir); err != nil || !c.NoticeShown {
		t.Fatal("the notice was not recorded as shown")
	}
	if ms, err := chatgptauth.ReadModels(dir); err != nil || len(ms.Models) != 1 || ms.Models[0].Slug != "gpt-5.6-sol" {
		t.Fatal("the plan's models were not fetched into the TUI's directory")
	}
	if info, err := os.Stat(chatgptauth.TokenFile(dir)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("the token file is not 0600")
	}
	if s.listening {
		u, _ := url.Parse(s.redirect)
		if c, err := net.DialTimeout("tcp", u.Host, time.Second); err == nil {
			_ = c.Close()
			t.Fatal("the attempt's listener is still open after the sign-in")
		}
	}
	awaitNoSignInRun(t)
}

// TestConnectSignInEscClosesTheRealListener: the same Esc as
// TestConnectSignInEscClosesTheListener, over chatgptauth's own attempt — its
// loopback listener refuses connections after, and no sign-in command or
// listener goroutine is left. The directory is already registered with
// ChatGPT (signInFixture's plan-off registration), so the attempt is a
// re-login, which listens on 1455 or, when that is taken, on a free port
// (chatgptauth.Begin): there is always a listener to close, and the test
// never skips (review r14 6). Its run is ended and its wait joined on every
// way out of the test (runWait, beginStep). The control is the listener
// before Esc, which takes a connection.
func TestConnectSignInEscClosesTheRealListener(t *testing.T) {
	newFakeIssuer(t)
	m, _ := signInModel(t, true)
	m, wait := beginStep(t, m)
	done := runWait(t, m, wait)
	s := m.cdlg.signIn
	if !s.listening {
		t.Fatal("a re-login's attempt has no listener: neither 1455 nor a free port could be bound")
	}
	u, _ := url.Parse(s.redirect)
	c, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		t.Fatalf("the control: the attempt's listener takes no connection: %v", err)
	}
	_ = c.Close()
	m = pressKey(t, m, tea.KeyEsc)
	if _, ok := awaitMsg(t, done).(signInDoneMsg); !ok {
		t.Fatal("the wait did not end with Esc")
	}
	if c, err := net.DialTimeout("tcp", u.Host, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("Esc left the attempt's listener open")
	}
	if m.cdlg.step != connectPick {
		t.Fatal("Esc did not go back to step one")
	}
	awaitNoSignInRun(t)
}
