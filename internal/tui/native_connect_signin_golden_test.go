package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/chatgptauth"
)

// /connect's ChatGPT sign-in as frames (plan 033 §3.13, §5 C16): a real native
// session — nativePickerSession — with the TUI's seams on signInFixture's
// directory and environment (the ChatGPT plan's provider beside alpha, beta
// and gamma), and chatgptauth behind connect_signin.go's seams: every run of a
// script begins a stand-in attempt of its own (standInSignIn), whose fixed
// addresses make the frames goldens. Nothing reaches OpenAI or a browser. The
// providers' read is a gated call, so a wait for Gamma is a wait for the read;
// the begin and the wait are plain commands, so no frame is held for them.

// signInConfig is the TUI over nativePickerSession with its seams on
// signInFixture's directory (planOff: a registration without plan usage).
func signInConfig(t *testing.T, planOff bool) Config {
	t.Helper()
	ws := frameWorkspace(t)
	dir, getenv := signInFixture(t, planOff)
	return Config{Session: nativePickerSession(t, ws), Theme: "tokyo-night", Workspace: ws, Yolo: true, NativeDir: dir, Getenv: getenv}
}

// runSignInFrame runs keys over signInConfig at cols x rows, in every
// transport and gate mode, each run's frame and error screened for the
// canary first (connectScreen), and answers the frame they agree on.
func runSignInFrame(t *testing.T, planOff bool, cols, rows int, keys string) string {
	t.Helper()
	isolateSkillsHome(t)
	got, _, err := runFrameModesScreened(t, func() Config {
		return signInConfig(t, planOff)
	}, cols, rows, keys, FrameOpts{Timeout: 20 * time.Second}, connectScreen(t))
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	return got
}

// signInOpenKeys opens /connect and the ChatGPT plan's sign-in: the box opens
// on the plan's row, the first with nothing funding it.
const signInOpenKeys = "<wait:idle>/connect<enter><wait:text:Gamma><enter><wait:text:Open this address>"

// signInEnd is the address's last row at a 52-cell box, as the box draws it
// (its border before it): the URL is shown to its end. The border keeps it
// from matching the same characters elsewhere in the address.
var signInEnd = "│" + signInURL[len(signInURL)-len(signInURL)%52:] + " "

// TestFrameGoldenNativeConnectSignIn is the step as it opens: the title, the
// address whole at 100x30 — a listener waits for the browser, and the hint
// says what to paste from another machine — and, at 80x24, an attempt with no
// listener, whose hint names the address the browser lands on, the address
// cut to fit with "…" and Ctrl+Y in the footer to copy it whole. Every run's
// attempt is still open when the frame is taken — its listener up, its wait
// waiting — which is what TestFrameGoldenNativeConnectSignInCancel's Esc
// changes (the control for its checks).
func TestFrameGoldenNativeConnectSignIn(t *testing.T) {
	for _, tc := range []struct {
		cols, rows int
		listening  bool
		want, not  []string
	}{
		{100, 30, true,
			[]string{connectSignInTitle, connectSignInIntro, signInURL[:52], signInEnd, "Waiting for the browser.", connectSignInWaitHint},
			[]string{"…", connectKeyTitle, "•"}},
		{80, 24, false,
			[]string{connectSignInTitle, signInURL[:52], "After you approve, paste the address the browser", signInRedirect, "…", connectSignInWaitHint},
			[]string{signInEnd, "Waiting for the browser.", "•"}},
	} {
		t.Run(fmt.Sprintf("%dx%d", tc.cols, tc.rows), func(t *testing.T) {
			s := standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(tc.listening, chatgptauth.Result{}) })
			got := runSignInFrame(t, false, tc.cols, tc.rows, signInOpenKeys)
			assertFrameGolden(t, fmt.Sprintf("native-connect-signin-%dx%d", tc.cols, tc.rows), tc.cols, tc.rows, got, tc.want, tc.not)
			made := s.all()
			if len(made) == 0 {
				t.Fatal("no run began an attempt")
			}
			for _, f := range made {
				if f.isClosed() || f.waitEnded(0) || (tc.listening && !f.listenerUp()) {
					t.Fatal("an open step's attempt was closed, or its wait ended, before the run ended")
				}
			}
		})
	}
}

// TestFrameGoldenNativeConnectSignInPaste is the browser's redirect pasted
// into the field, not yet sent: drawn as it is, the field scrolled to its
// end — the field shows what reads as the redirect address (§3.13 as review
// r14 1 amends it) and masks anything else, as the key field does
// (TestFrameNativeConnectSignInMasksAKey).
func TestFrameGoldenNativeConnectSignInPaste(t *testing.T) {
	standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(true, chatgptauth.Result{}) })
	got := runSignInFrame(t, false, 100, 30, signInOpenKeys+"<paste:"+signInPasted+">")
	assertFrameGolden(t, "native-connect-signin-paste-100x30", 100, 30, got,
		[]string{"│❯ ", "state=" + signInGoldenState, connectSignInWaitHint},
		[]string{"•", connectSignInHanded})
}

// TestFrameNativeConnectSignInMasksAKey (review r14 1, amending §3.13's
// unmasked field): a key pasted into the address field where the redirect
// goes, not yet sent, as every transport and gate mode frames it — every
// run's frame and error screened for the key (connectScreen) — is drawn
// masked, one mask per character, in the step as it was. The control is
// TestFrameGoldenNativeConnectSignInPaste: the browser's redirect in the same
// field, drawn as it is.
func TestFrameNativeConnectSignInMasksAKey(t *testing.T) {
	standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(true, chatgptauth.Result{}) })
	got := runSignInFrame(t, false, 100, 30, signInOpenKeys+"<paste:"+connectCanary+">")
	if !strings.Contains(got, "│❯ "+strings.Repeat(string(connectMask), len(connectCanary))) || !strings.Contains(got, connectSignInWaitHint) {
		t.Fatalf("the pasted key is not drawn masked in the step:\n%s", got)
	}
}

// TestFrameGoldenNativeConnectSignInDone is the pasted redirect sent: the box
// closed, and the transcript says as whom, shows the one-time notice in craze
// auth's words, names the plan's models and says what this conversation gets
// (P8). Every run recorded the notice as shown and fetched the models once.
func TestFrameGoldenNativeConnectSignInDone(t *testing.T) {
	s := standInSignIn(t, func() *fakeSignIn {
		return newFakeSignIn(true, chatgptauth.Result{Email: signInEmail, PlanUsage: true, Registered: true, ShowNotice: true})
	})
	got := runSignInFrame(t, false, 100, 30,
		signInOpenKeys+"<paste:"+signInPasted+"><enter><wait:text:ChatGPT plan models:>")
	assertFrameGolden(t, "native-connect-signin-done-100x30", 100, 30, got,
		[]string{"Signed in to ChatGPT as " + signInEmail + ".", chatgptauth.NoticeTitle, "Eligible usage in this app uses your ChatGPT plan.",
			"ChatGPT plan models: chatgpt/gpt-5.6-sol, chatgpt/gpt-6-astra", "New sessions offer the ChatGPT plan's models"},
		[]string{connectSignInTitle, "plan usage is off"})
	runs := int32(len(s.all()))
	if runs == 0 || s.marks.Load() != runs || s.fetches.Load() != runs {
		t.Fatalf("%d runs recorded the notice %d times and fetched the models %d times", runs, s.marks.Load(), s.fetches.Load())
	}
	for _, f := range s.all() {
		if p, _ := f.counts(); p != 1 {
			t.Fatal("a run's pasted redirect was not handed to its attempt once")
		}
	}
}

// TestFrameGoldenNativeConnectSignInPlanOff is a sign-in whose account did
// not grant plan usage, over a registration that already had it off (step
// one's "plan usage off"): the transcript says so and how to turn it on, in
// craze auth's words with /connect; no notice, no model list.
func TestFrameGoldenNativeConnectSignInPlanOff(t *testing.T) {
	s := standInSignIn(t, func() *fakeSignIn {
		return newFakeSignIn(true, chatgptauth.Result{Email: signInEmail})
	})
	got := runSignInFrame(t, true, 100, 30,
		"<wait:idle>/connect<enter><wait:text:"+connectPlanOffMark+"><enter><wait:text:Open this address>"+
			"<paste:"+signInPasted+"><enter><wait:text:To turn it on>")
	assertFrameGolden(t, "native-connect-signin-off-100x30", 100, 30, got,
		[]string{"Signed in to ChatGPT as " + signInEmail + ", but ChatGPT plan usage is off", "To turn it on, sign in again with /connect"},
		[]string{connectSignInTitle, chatgptauth.NoticeTitle, "ChatGPT plan models:"})
	if s.marks.Load() != 0 || s.fetches.Load() != 0 {
		t.Fatal("a sign-in without plan usage recorded a notice or fetched models")
	}
}

// TestFrameGoldenNativeConnectSignInCancel is Esc on the step: back to step
// one, on the plan's row. Every run's attempt was closed by it — its listener
// refuses connections, its wait has returned — and no sign-in command or
// listener is left running, before the test's own cleanup closes anything
// (TestFrameGoldenNativeConnectSignIn is the control: there they are open).
func TestFrameGoldenNativeConnectSignInCancel(t *testing.T) {
	s := standInSignIn(t, func() *fakeSignIn { return newFakeSignIn(true, chatgptauth.Result{}) })
	got := runSignInFrame(t, false, 100, 30, signInOpenKeys+"<esc><wait:gone:"+connectSignInTitle+">")
	assertFrameGolden(t, "native-connect-signin-cancel-100x30", 100, 30, got,
		[]string{connectDialogTitle, "> ChatGPT plan", "✓", connectPickHint},
		[]string{connectSignInTitle, "Open this address"})
	made := s.all()
	if len(made) == 0 {
		t.Fatal("no run began an attempt")
	}
	for _, f := range made {
		if !f.isClosed() || f.listenerUp() {
			t.Fatal("Esc left a run's listener open")
		}
		if !f.waitEnded(5 * time.Second) {
			t.Fatal("Esc left a run's wait running")
		}
	}
	awaitNoSignInRun(t)
	if strings.Contains(got, "/connect:") {
		t.Fatalf("a cancelled sign-in wrote an error row:\n%s", got)
	}
}
