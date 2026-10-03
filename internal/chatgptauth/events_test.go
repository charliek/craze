package chatgptauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
)

// The sign-in's events (plan 034 §3.3, A10, A13, A14): every emission point,
// driven against the fake issuer and the real loopback listener, reports
// exactly its events — and each attempt exactly one terminal outcome, however
// it ends. eventScenarios is the table; TestSignInEvents runs it, and the
// sign-in log's value-free scan runs it again through internal/signinlog
// (RunEventScenarios, export_test.go; signinlog_scan_test.go).

// eventSink collects the events an observer is told.
type eventSink struct {
	mu  sync.Mutex
	evs []Event
}

func (s *eventSink) observe(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
}

func (s *eventSink) all() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.evs)
}

// scenarioRun is one scenario's world: a fresh fake issuer and native
// directory, the observer every attempt and fetch reports to — the sink, and
// the caller's own (RunEventScenarios) — and every fixture value the run used,
// for the leak scans.
type scenarioRun struct {
	t       *testing.T
	f       *fakeOpenAI
	dir     string
	sink    *eventSink
	observe func(Event)
	values  []string
}

func newScenarioRun(t *testing.T, extra func(Event)) *scenarioRun {
	t.Helper()
	f := newFake(t)
	useFake(t, f)
	sc := &scenarioRun{t: t, f: f, dir: nativeDir(t), sink: &eventSink{}}
	sc.observe = func(ev Event) {
		sc.sink.observe(ev)
		if extra != nil {
			extra(ev)
		}
	}
	return sc
}

// keep records fixture values the scans look for.
func (sc *scenarioRun) keep(vs ...string) {
	for _, v := range vs {
		if v != "" {
			sc.values = append(sc.values, v)
		}
	}
}

// begin is Begin into the run's directory, reporting to its observer; the
// authorization URL and every secret of the attempt are kept.
func (sc *scenarioRun) begin(opts BeginOptions) *Attempt {
	sc.t.Helper()
	a, err := sc.beginErr(context.Background(), opts)
	if err != nil {
		sc.t.Fatalf("Begin: %v", err)
	}
	return a
}

func (sc *scenarioRun) beginErr(ctx context.Context, opts BeginOptions) (*Attempt, error) {
	opts.Observe = sc.observe
	a, err := Begin(ctx, sc.dir, opts)
	if err != nil {
		return nil, err
	}
	sc.t.Cleanup(func() { a.Close(CloseDone) })
	q := authQuery(sc.t, a)
	sc.keep(a.URL(), a.state, a.nonce, a.verifier, q.Get("code_challenge"), q.Get("ext_agent_host_id"), q.Get("login_hint"))
	return a, nil
}

// redirect is the browser's redirect for a: the fake's approval, kept.
func (sc *scenarioRun) redirect(a *Attempt) url.Values {
	sc.t.Helper()
	q := sc.f.authorize(sc.t, a.URL())
	sc.keep(a.RedirectURI() + "?" + q.Encode())
	return q
}

// paste is a.Paste(line), the line kept.
func (sc *scenarioRun) paste(a *Attempt, line string) error {
	sc.keep(line)
	return a.Paste(line)
}

// pasteOK pastes the attempt's own redirect, which must be accepted.
func (sc *scenarioRun) pasteOK(a *Attempt) {
	sc.t.Helper()
	if err := sc.paste(a, a.RedirectURI()+"?"+sc.redirect(a).Encode()); err != nil {
		sc.t.Fatalf("Paste of the attempt's own redirect: %v", err)
	}
}

// get sends GET path?q to a's loopback listener, as a browser or another
// local page would, and answers the status; the URL is kept.
func (sc *scenarioRun) get(a *Attempt, path string, q url.Values) int {
	sc.t.Helper()
	u := "http://" + a.ln.Addr().String() + path
	if q != nil {
		u += "?" + q.Encode()
	}
	sc.keep(u)
	resp, err := http.Get(u)
	if err != nil {
		sc.t.Fatalf("GET the listener: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// signIn is a whole paste-only sign-in, which must succeed.
func (sc *scenarioRun) signIn() *Attempt {
	sc.t.Helper()
	a := sc.begin(BeginOptions{PasteOnly: true})
	sc.pasteOK(a)
	if _, err := a.Wait(context.Background()); err != nil {
		sc.t.Fatalf("Wait: %v", err)
	}
	a.Close(CloseDone)
	return a
}

// fetch is FetchModels into the run's directory at version, reporting to the
// attempt's observer.
func (sc *scenarioRun) fetch(a *Attempt, version string) error {
	_, err := FetchModels(context.Background(), Source(sc.dir), FetchOptions{ClientVersion: version, Observe: a.Observer()})
	return err
}

// hold makes the fake's request behind *gate — holdExchange, holdModels —
// wait until the test ends.
func (sc *scenarioRun) hold(gate *chan struct{}) {
	hold := make(chan struct{})
	sc.t.Cleanup(func() { close(hold) })
	sc.f.mu.Lock()
	*gate = hold
	sc.f.mu.Unlock()
}

// registered seeds the run's directory with a registration, so the next
// attempt is a re-login.
func (sc *scenarioRun) registered() {
	sc.t.Helper()
	if err := ensureAuthDir(sc.dir); err != nil {
		sc.t.Fatal(err)
	}
	if err := writeClient(sc.dir, Client{ClientID: testClient, Subject: testSubject, Email: testEmail, PlanUsage: true, NoticeShown: true}); err != nil {
		sc.t.Fatal(err)
	}
}

// waitStarted waits until a's Wait is waiting.
func waitStarted(t *testing.T, a *Attempt) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.emu.Lock()
		w := a.waiting
		a.emu.Unlock()
		if w {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the attempt's Wait never started")
		}
		time.Sleep(time.Millisecond)
	}
}

// waitAsync runs a.Wait(ctx) on a goroutine of its own, once it is waiting,
// and answers its error's channel.
func waitAsync(t *testing.T, a *Attempt, ctx context.Context) <-chan error {
	t.Helper()
	out := make(chan error, 1)
	go func() {
		_, err := a.Wait(ctx)
		out <- err
	}()
	waitStarted(t, a)
	return out
}

// setVar sets *p to v for one test.
func setVar[T any](t *testing.T, p *T, v T) {
	prev := *p
	*p = v
	t.Cleanup(func() { *p = prev })
}

// anyPort is a want's Port for a free port the listener took in 1455's
// place: any port but 1455.
const anyPort = -1

// The expected events, by what they are.
func evBegin(mode Mode, port int, why Reason, reg Registration) Event {
	return Event{Kind: EventBegin, Mode: mode, Port: port, Reason: why, Registration: reg}
}

func evFailed(step Step, status int, code string, class Class, check Check) Event {
	return Event{Kind: EventFailed, Step: step, Status: status, Code: code, Class: class, Check: check}
}

var (
	evPasteBegin   = evBegin(ModePasteOnly, callbackPort, ReasonRequested, RegistrationNew)
	evPasted       = Event{Kind: EventRedirectReceived, Via: ViaPaste}
	evListened     = Event{Kind: EventRedirectReceived, Via: ViaListener}
	evSignedIn     = Event{Kind: EventSignedIn, Usage: UsagePlan, Registration: RegistrationNew}
	evReSignedIn   = Event{Kind: EventSignedIn, Usage: UsagePlan, Registration: RegistrationReused}
	evModelsListed = Event{Kind: EventModelsFetched, Models: 8, ClientVersion: testPin}
)

func evCancelled(why CloseReason) Event { return Event{Kind: EventCancelled, Reason: Reason(why)} }

func evPasteRefused(r Refusal, p Part) Event {
	return Event{Kind: EventPasteRefused, Refusal: r, Part: p}
}

// eventScenario is one emission path: run drives it, want is every event it
// reports in order (Attempt and Elapsed checked, then left out), and wantErr,
// when set, is the error text the run's last step must answer (A14).
type eventScenario struct {
	name string
	run  func(t *testing.T, sc *scenarioRun) error
	want []Event
	// wantErr is the text of the error run answers; "" is none.
	wantErr string
}

var eventScenarios = []eventScenario{
	{
		name: "a pasted sign-in registers, then the model list is the attempt's",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.signIn()
			a.Close(CloseEsc) // after the outcome: cleanup, nothing more
			if err := sc.fetch(a, testPin); err != nil {
				return err
			}
			// A list already current is no event (RefreshModels).
			_, err := RefreshModels(context.Background(), Source(sc.dir), ModelsMaxAge, FetchOptions{ClientVersion: testPin, Observe: a.Observer()})
			return err
		},
		want: []Event{evPasteBegin, evPasted, evSignedIn, evModelsListed},
	},
	{
		name: "the listener: a 404 is unreported, another attempt's redirect and a stray request are reported once each and counted",
		run: func(t *testing.T, sc *scenarioRun) error {
			sc.registered()
			useListener(t, false)
			a := sc.begin(BeginOptions{})
			for _, want := range []struct {
				path string
				q    url.Values
				code int
			}{
				{"/favicon.ico", nil, http.StatusNotFound},
				{callbackPath, url.Values{"code": {"fake-code-other-attempt"}, "state": {"another-attempts-state"}}, http.StatusBadRequest},
				{callbackPath, url.Values{"code": {"fake-code-other-attempt"}, "state": {"another-attempts-state"}}, http.StatusBadRequest},
				{"/", url.Values{"code": {"x"}}, http.StatusNotFound},
				{callbackPath, url.Values{"code": {"fake-code-other-attempt"}, "state": {"another-attempts-state"}}, http.StatusBadRequest},
				{callbackPath, url.Values{"hello": {"there"}}, http.StatusBadRequest},
				{callbackPath, url.Values{"code": {"fake-code-no-state"}}, http.StatusBadRequest},
			} {
				if got := sc.get(a, want.path, want.q); got != want.code {
					t.Fatalf("GET %s = %d; want %d", want.path, got, want.code)
				}
			}
			if got := sc.get(a, callbackPath, sc.redirect(a)); got != http.StatusOK {
				t.Fatalf("the redirect = %d", got)
			}
			_, err := a.Wait(context.Background())
			return err
		},
		want: []Event{
			evBegin(ModeListening, callbackPort, "", RegistrationReused),
			{Kind: EventListenerRefused, Refusal: RefusalOtherAttempt, Status: http.StatusBadRequest},
			{Kind: EventListenerRefused, Refusal: RefusalNotOurs, Status: http.StatusBadRequest},
			evListened,
			{Kind: EventListenerRefusals, Refusal: RefusalOtherAttempt, Status: http.StatusBadRequest, Count: 3},
			{Kind: EventListenerRefusals, Refusal: RefusalNotOurs, Status: http.StatusBadRequest, Count: 2},
			evReSignedIn,
		},
	},
	{
		name: "the listener after a paste: the attempt is over, 410 reported once and counted",
		run: func(t *testing.T, sc *scenarioRun) error {
			sc.registered()
			useListener(t, false)
			a := sc.begin(BeginOptions{})
			q := sc.redirect(a)
			if err := sc.paste(a, a.RedirectURI()+"?"+q.Encode()); err != nil {
				return err
			}
			for range 2 {
				if got := sc.get(a, callbackPath, q); got != http.StatusGone {
					t.Fatalf("the redirect after the paste = %d; want 410", got)
				}
			}
			_, err := a.Wait(context.Background())
			return err
		},
		want: []Event{
			evBegin(ModeListening, callbackPort, "", RegistrationReused),
			evPasted,
			{Kind: EventListenerRefused, Refusal: RefusalOver, Status: http.StatusGone},
			{Kind: EventListenerRefusals, Refusal: RefusalOver, Status: http.StatusGone, Count: 2},
			evReSignedIn,
		},
	},
	{
		name: "every pasted refusal is named, never quoted; Esc cancels",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.begin(BeginOptions{PasteOnly: true})
			r := a.RedirectURI()
			for _, line := range []string{
				strings.Repeat("x", MaxPaste+1),
				"sk-fake-key-pasted-out-of-habit-0123456789",
				"https://127.0.0.1:1455/auth/callback?code=c1&state=s1",
				"http://someone@127.0.0.1:1455/auth/callback?code=c2&state=s2",
				"http://localhost:1455/auth/callback?code=c3&state=s3",
				"http://127.0.0.1:1456/auth/callback?code=c4&state=s4",
				"http://127.0.0.1:1455/auth/elsewhere?code=c5&state=s5",
				r + "?code=fake-code-earlier&state=an-earlier-attempts-state",
			} {
				if err := sc.paste(a, line); !errors.Is(err, ErrRedirectMismatch) {
					t.Fatalf("a refused paste = %v; want ErrRedirectMismatch", err)
				}
			}
			a.Close(CloseEsc)
			if err := sc.paste(a, r+"?code=late&state="+a.state); !errors.Is(err, ErrAttemptOver) {
				t.Fatalf("a paste after the close = %v; want ErrAttemptOver", err)
			}
			return nil
		},
		want: []Event{
			evPasteBegin,
			evPasteRefused(RefusalTooLong, ""),
			evPasteRefused(RefusalNotAddress, ""),
			evPasteRefused(RefusalMismatch, PartScheme),
			evPasteRefused(RefusalMismatch, PartUserinfo),
			evPasteRefused(RefusalMismatch, PartHost),
			evPasteRefused(RefusalMismatch, PartPort),
			evPasteRefused(RefusalMismatch, PartPath),
			evPasteRefused(RefusalMismatch, PartState),
			evCancelled(CloseEsc),
			evPasteRefused(RefusalOver, ""),
		},
	},
	{
		name: "Esc while the wait runs: cancelled once, by the close",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.begin(BeginOptions{PasteOnly: true})
			done := waitAsync(t, a, context.Background())
			a.Close(CloseEsc)
			if err := await(t, done, "the wait"); !errors.Is(err, ErrAttemptOver) {
				t.Fatalf("Wait = %v; want ErrAttemptOver", err)
			}
			a.Close(CloseDialog)
			return nil
		},
		want: []Event{evPasteBegin, evCancelled(CloseEsc)},
	},
	{
		name: "the dialog closed: its context cancelled first, then the close",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.begin(BeginOptions{PasteOnly: true})
			ctx, cancel := context.WithCancel(context.Background())
			done := waitAsync(t, a, ctx)
			cancel()
			if err := await(t, done, "the wait"); !errors.Is(err, context.Canceled) {
				t.Fatalf("Wait = %v; want context.Canceled", err)
			}
			if got := len(sc.sink.all()); got != 1 {
				t.Fatalf("a cancelled wait with no close reported %d events; want the begin alone, its close to come", got)
			}
			a.Close(CloseDialog)
			return nil
		},
		want: []Event{evPasteBegin, evCancelled(CloseDialog)},
	},
	{
		name: "a signal (the CLI): the context's cause, then the close",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.begin(BeginOptions{PasteOnly: true})
			ctx, cancel := context.WithCancelCause(context.Background())
			done := waitAsync(t, a, ctx)
			cancel(errors.New("interrupted"))
			_ = await(t, done, "the wait")
			a.Close(CloseSignal)
			return nil
		},
		want: []Event{evPasteBegin, evCancelled(CloseSignal)},
	},
	{
		name: "a shutdown while the wait waits for a redirect: the close reports it before it returns",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.begin(BeginOptions{PasteOnly: true})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := waitAsync(t, a, ctx)
			a.Close(CloseShutdown)
			// Reported by now, whatever the wait does next: finishRun closes
			// the log right after it closes every attempt.
			if evs := sc.sink.all(); len(evs) != 2 || evs[1].Kind != EventCancelled {
				t.Fatalf("after the close the attempt had reported %d events; want its cancel among them", len(evs))
			}
			cancel()
			_ = await(t, done, "the wait")
			return nil
		},
		want: []Event{evPasteBegin, evCancelled(CloseShutdown)},
	},
	{
		name: "Esc while an accepted redirect is exchanged: the wait reports the cancel, with Esc's reason",
		run: func(t *testing.T, sc *scenarioRun) error {
			sc.hold(&sc.f.holdExchange)
			a := sc.begin(BeginOptions{PasteOnly: true})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sc.pasteOK(a)
			done := waitAsync(t, a, ctx)
			a.Close(CloseEsc)
			if evs := sc.sink.all(); len(evs) != 2 {
				t.Fatalf("the close of a sign-in being finished reported its outcome itself (%d events)", len(evs))
			}
			cancel()
			if err := await(t, done, "the wait"); !errors.Is(err, context.Canceled) {
				t.Fatalf("Wait = %v; want context.Canceled", err)
			}
			return nil
		},
		want: []Event{evPasteBegin, evPasted, evCancelled(CloseEsc)},
	},
	{
		name: "a shutdown before the wait",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.begin(BeginOptions{PasteOnly: true})
			a.Close(CloseShutdown)
			if _, err := a.Wait(context.Background()); !errors.Is(err, ErrAttemptOver) {
				t.Fatalf("Wait after the close = %v; want ErrAttemptOver", err)
			}
			return nil
		},
		want: []Event{evPasteBegin, evCancelled(CloseShutdown)},
	},
	{
		name: "stdin ended (the CLI's paste-only)",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.begin(BeginOptions{PasteOnly: true})
			done := waitAsync(t, a, context.Background())
			a.Close(CloseNoInput)
			_ = await(t, done, "the wait")
			return nil
		},
		want: []Event{evPasteBegin, evCancelled(CloseNoInput)},
	},
	{
		name: "declined in the browser",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.begin(BeginOptions{PasteOnly: true})
			if err := sc.paste(a, a.RedirectURI()+"?"+url.Values{"error": {"access_denied"}, "state": {a.state}, "error_description": {testErrorDescription}}.Encode()); err != nil {
				return err
			}
			_, err := a.Wait(context.Background())
			a.Close(CloseDone)
			return err
		},
		want:    []Event{evPasteBegin, evPasted, {Kind: EventDeclined, Step: StepAuthorize, Code: "access_denied"}},
		wantErr: ErrAccessDenied.Error(),
	},
	{
		name: "another authorize error, allowlisted",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.begin(BeginOptions{PasteOnly: true})
			if err := sc.paste(a, a.RedirectURI()+"?"+url.Values{"error": {"invalid_scope"}, "state": {a.state}}.Encode()); err != nil {
				return err
			}
			_, err := a.Wait(context.Background())
			return err
		},
		want:    []Event{evPasteBegin, evPasted, evFailed(StepAuthorize, 0, "invalid_scope", ClassRefused, "")},
		wantErr: "chatgptauth: authorize refused (invalid_scope)",
	},
	{
		name: "an authorize error craze does not know: unrecognised, never repeated",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.begin(BeginOptions{PasteOnly: true})
			echoed := "fake-refresh-echoed-as-a-code-7a7a7a"
			sc.keep(echoed)
			if err := sc.paste(a, a.RedirectURI()+"?"+url.Values{"error": {echoed}, "state": {a.state}}.Encode()); err != nil {
				return err
			}
			_, err := a.Wait(context.Background())
			return err
		},
		want:    []Event{evPasteBegin, evPasted, evFailed(StepAuthorize, 0, CodeUnrecognised, ClassRefused, "")},
		wantErr: "chatgptauth: authorize refused (an unrecognised error code)",
	},
	{
		name: "the exchange refused",
		run: func(t *testing.T, sc *scenarioRun) error {
			sc.f.exchangeErr = "invalid_grant"
			a := sc.begin(BeginOptions{PasteOnly: true})
			sc.pasteOK(a)
			_, err := a.Wait(context.Background())
			return err
		},
		want:    []Event{evPasteBegin, evPasted, evFailed(StepExchange, 400, "invalid_grant", ClassRefused, "")},
		wantErr: "chatgptauth: exchange refused (HTTP 400, invalid_grant)",
	},
	{
		name: "an id_token for another nonce",
		run: func(t *testing.T, sc *scenarioRun) error {
			sc.f.idEdit = func(_, claims map[string]any) { claims["nonce"] = "another-nonce-0000" }
			a := sc.begin(BeginOptions{PasteOnly: true})
			sc.pasteOK(a)
			_, err := a.Wait(context.Background())
			sc.keep(sc.f.issued...)
			return err
		},
		want:    []Event{evPasteBegin, evPasted, evFailed(StepIDToken, 0, "", ClassIDToken, CheckNonce)},
		wantErr: "chatgptauth: the sign-in's id_token is not valid: its nonce is not this sign-in's",
	},
	{
		name: "a tampered id_token",
		run: func(t *testing.T, sc *scenarioRun) error {
			sc.f.tamperID = true
			a := sc.begin(BeginOptions{PasteOnly: true})
			sc.pasteOK(a)
			_, err := a.Wait(context.Background())
			return err
		},
		want:    []Event{evPasteBegin, evPasted, evFailed(StepIDToken, 0, "", ClassIDToken, CheckSignature)},
		wantErr: "chatgptauth: the sign-in's id_token is not valid: its signature does not verify",
	},
	{
		name: "a redirect with no code",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.begin(BeginOptions{PasteOnly: true})
			if err := sc.paste(a, a.RedirectURI()+"?"+url.Values{"state": {a.state}}.Encode()); err != nil {
				return err
			}
			_, err := a.Wait(context.Background())
			return err
		},
		want:    []Event{evPasteBegin, evPasted, evFailed(StepRedirect, 0, "", ClassBadRedirect, "")},
		wantErr: "chatgptauth: the redirect carries no authorization code",
	},
	{
		name: "the install fails",
		run: func(t *testing.T, sc *scenarioRun) error {
			setVar(t, &writeSync, func(path string, _ []byte, _ os.FileMode, _ func() error) error {
				return &fs.PathError{Op: "write", Path: path, Err: syscall.ENOSPC}
			})
			a := sc.begin(BeginOptions{PasteOnly: true})
			sc.pasteOK(a)
			_, err := a.Wait(context.Background())
			return err
		},
		want: []Event{evPasteBegin, evPasted, evFailed(StepInstall, 0, "", ClassFilesystem, "")},
	},
	{
		name: "signed in without plan usage",
		run: func(t *testing.T, sc *scenarioRun) error {
			sc.f.scope = "openid profile email offline_access"
			sc.signIn()
			return nil
		},
		want: []Event{evPasteBegin, evPasted, {Kind: EventSignedIn, Usage: UsageOff, Registration: RegistrationNew}},
	},
	{
		name: "the exchange runs past its deadline: named by its step (A14)",
		run: func(t *testing.T, sc *scenarioRun) error {
			setVar(t, &exchangeTimeout, 300*time.Millisecond)
			sc.hold(&sc.f.holdExchange)
			a := sc.begin(BeginOptions{PasteOnly: true})
			sc.pasteOK(a)
			_, err := a.Wait(context.Background())
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("a timed-out exchange = %v; want it to be context.DeadlineExceeded still", err)
			}
			return err
		},
		want:    []Event{evPasteBegin, evPasted, evFailed(StepExchange, 0, "", ClassTimeout, "")},
		wantErr: "chatgptauth: exchange: timed out after 300ms",
	},
	{
		name: "Begin's lock held past its bound: named by its step (A14)",
		run: func(t *testing.T, sc *scenarioRun) error {
			setVar(t, &lockWait, 50*time.Millisecond)
			if err := ensureAuthDir(sc.dir); err != nil {
				t.Fatal(err)
			}
			unlock, err := atomicfile.Lock(lockFile(sc.dir))
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			_, err = sc.beginErr(context.Background(), BeginOptions{PasteOnly: true})
			if !errors.Is(err, atomicfile.ErrLockBusy) {
				t.Fatalf("Begin = %v; want the lock busy", err)
			}
			return err
		},
		want:    []Event{evFailed(StepBegin, 0, "", ClassTimeout, "")},
		wantErr: "chatgptauth: begin: timed out after 50ms waiting for the sign-in lock, which another craze process holds",
	},
	{
		name: "Begin cancelled as it waits for its lock",
		run: func(t *testing.T, sc *scenarioRun) error {
			if err := ensureAuthDir(sc.dir); err != nil {
				t.Fatal(err)
			}
			unlock, err := atomicfile.Lock(lockFile(sc.dir))
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_, err = sc.beginErr(ctx, BeginOptions{PasteOnly: true})
			if err == nil {
				t.Fatal("Begin succeeded under a held lock")
			}
			return nil
		},
		want: []Event{{Kind: EventCancelled, Reason: ReasonContext}},
	},
	{
		name: "1455 busy: a registration is paste-only, and says why",
		run: func(t *testing.T, sc *scenarioRun) error {
			useListener(t, true)
			a := sc.begin(BeginOptions{})
			if a.Listening() {
				t.Fatal("a registration listened with 1455 busy")
			}
			a.Close(CloseEsc)
			return nil
		},
		want: []Event{evBegin(ModePasteOnly, callbackPort, ReasonPortBusy, RegistrationNew), evCancelled(CloseEsc)},
	},
	{
		name: "1455 busy: a re-login listens on another port, and says why",
		run: func(t *testing.T, sc *scenarioRun) error {
			sc.registered()
			useListener(t, true)
			a := sc.begin(BeginOptions{})
			if !a.Listening() {
				t.Fatal("a re-login did not listen on another port")
			}
			a.Close(CloseEsc)
			return nil
		},
		want: []Event{evBegin(ModeListening, anyPort, ReasonPortBusy, RegistrationReused), evCancelled(CloseEsc)},
	},
	{
		name: "what the UI reports, and nothing else",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.begin(BeginOptions{PasteOnly: true})
			for _, k := range []EventKind{EventBrowserOpened, EventBrowserFailed, EventAddressCopied, EventSignedIn, EventFailed, "made-up"} {
				a.Report(k)
			}
			a.Close(CloseEsc)
			return nil
		},
		want: []Event{evPasteBegin, {Kind: EventBrowserOpened}, {Kind: EventBrowserFailed}, {Kind: EventAddressCopied}, evCancelled(CloseEsc)},
	},
	{
		name: "the model list: an empty reply keeps the cache, and says the pin",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.signIn()
			if err := sc.fetch(a, testPin); err != nil {
				return err
			}
			return sc.fetch(a, "0.0.1")
		},
		want:    []Event{evPasteBegin, evPasted, evSignedIn, evModelsListed, {Kind: EventModelsEmpty, ClientVersion: "0.0.1"}},
		wantErr: "chatgptauth: the plan's model list came back empty (client_version 0.0.1)",
	},
	{
		name: "the model list refused",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.signIn()
			sc.f.mu.Lock()
			sc.f.modelsErr = "subscription_sharing_user_not_eligible"
			sc.f.mu.Unlock()
			return sc.fetch(a, testPin)
		},
		want: []Event{evPasteBegin, evPasted, evSignedIn, {
			Kind: EventModelsFailed, Step: StepModels, Status: 400, Code: "subscription_sharing_user_not_eligible", Class: ClassRefused, ClientVersion: testPin,
		}},
		wantErr: "chatgptauth: models refused (HTTP 400, subscription_sharing_user_not_eligible)",
	},
	{
		name: "the model list runs past its deadline: named by its step (A14)",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.signIn()
			setVar(t, &modelsTimeout, 300*time.Millisecond)
			sc.hold(&sc.f.holdModels)
			err := sc.fetch(a, testPin)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("a timed-out fetch = %v; want it to be context.DeadlineExceeded still", err)
			}
			return err
		},
		want:    []Event{evPasteBegin, evPasted, evSignedIn, {Kind: EventModelsFailed, Step: StepModels, Class: ClassTimeout, ClientVersion: testPin}},
		wantErr: "chatgptauth: models: timed out after 300ms",
	},
	{
		name: "the model list's lock held past its bound: named by its step (A14)",
		run: func(t *testing.T, sc *scenarioRun) error {
			a := sc.signIn()
			setVar(t, &lockWait, 50*time.Millisecond)
			unlock, err := atomicfile.Lock(ModelsFile(sc.dir) + ".lock")
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			return sc.fetch(a, testPin)
		},
		want:    []Event{evPasteBegin, evPasted, evSignedIn, {Kind: EventModelsFailed, Step: StepModels, Class: ClassTimeout, ClientVersion: testPin}},
		wantErr: "chatgptauth: models: timed out after 50ms waiting for the model list's lock, which another craze process holds",
	},
}

// normalized is evs with each attempt id checked — 8 lowercase hex, one per
// run — and every Elapsed checked not negative, then both left out; a port a
// listener took in 1455's place is anyPort.
func normalized(t *testing.T, evs []Event) []Event {
	t.Helper()
	ids := map[string]bool{}
	out := make([]Event, len(evs))
	for i, ev := range evs {
		if len(ev.Attempt) != 8 || strings.Trim(ev.Attempt, "0123456789abcdef") != "" {
			t.Fatalf("event %d (%s) has attempt id %q; want 8 lowercase hex digits", i, ev.Kind, ev.Attempt)
		}
		ids[ev.Attempt] = true
		if ev.Elapsed < 0 {
			t.Fatalf("event %d (%s) has a negative elapsed time", i, ev.Kind)
		}
		if ev.Kind == EventBegin && ev.Port != callbackPort && ev.Port > 0 {
			ev.Port = anyPort
		}
		ev.Attempt, ev.Elapsed = "", 0
		out[i] = ev
	}
	if len(ids) > 1 {
		t.Fatalf("one run's events carry %d attempt ids; want one", len(ids))
	}
	return out
}

// terminals counts the terminal outcomes among evs.
func terminals(evs []Event) int {
	n := 0
	for _, ev := range evs {
		switch ev.Kind {
		case EventSignedIn, EventDeclined, EventFailed, EventCancelled:
			n++
		}
	}
	return n
}

// TestSignInEvents (plan 034 A10, A13, A14): each scenario reports exactly
// its events, in order, with one attempt id, and exactly one terminal outcome
// — Esc, a dialog's close, a shutdown, a signal, the end of stdin, a decline,
// every kind of failure, a success with closes after it. A 404 on the
// listener is never reported, and a refusal kind repeated is reported once,
// with its count at the end (A13). A step that runs out of time says which
// (A14), in its error and its event.
func TestSignInEvents(t *testing.T) {
	for _, sc := range eventScenarios {
		t.Run(sc.name, func(t *testing.T) {
			run := newScenarioRun(t, nil)
			err := sc.run(t, run)
			switch {
			case sc.wantErr != "" && (err == nil || err.Error() != sc.wantErr):
				t.Fatalf("the run's error = %v; want %q", err, sc.wantErr)
			case sc.wantErr == "" && err != nil && sc.want[len(sc.want)-1].Kind != EventFailed:
				t.Fatalf("the run failed: %v", err)
			}
			got := normalized(t, run.sink.all())
			if !slices.Equal(got, sc.want) {
				t.Fatalf("events:\n%s\nwant:\n%s", eventsText(got), eventsText(sc.want))
			}
			if n := terminals(got); n != 1 {
				t.Fatalf("the attempt reported %d terminal outcomes; want exactly one", n)
			}
		})
	}
}

// eventsText is evs, one per line, for a failure message: every field is a
// fixed-set value or a number, so it may be printed.
func eventsText(evs []Event) string {
	var b strings.Builder
	for _, ev := range evs {
		fmt.Fprintf(&b, "  %+v\n", ev)
	}
	return b.String()
}

// TestClassOfIsTypedNotTextual (plan 034 Q7): an error's class comes from its
// type and the sentinels it wraps, never its words. The control: an error
// whose text names a class it is not of is "other".
func TestClassOfIsTypedNotTextual(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want Class
	}{
		{&StepTimeoutError{Step: StepExchange, After: time.Second}, ClassTimeout},
		{context.Canceled, ClassCancelled},
		{&OAuthError{Step: "exchange", Status: 400}, ClassRefused},
		{&idTokenError{check: CheckAudience, what: "x"}, ClassIDToken},
		{&stepError{step: StepJWKS, err: errRedirect}, ClassHTTPRedirect},
		{ErrSignedOut, ClassSignedOut},
		{errCorrupt, ClassSignInAgain},
		{badReply(StepExchange, "x"), ClassBadReply},
		{&fs.PathError{Op: "open", Path: "p", Err: syscall.EACCES}, ClassFilesystem},
		{errors.New("timeout refused tls dns network"), ClassOther},
	} {
		if got := classOf(tc.err); got != tc.want {
			t.Errorf("classOf(%T) = %s; want %s", tc.err, got, tc.want)
		}
	}
}

// TestObserverIsCalledOutsideTheLocks (plan 034 §3.3): an observer that calls
// back into the attempt — what a UI's observer may do — does not deadlock:
// every event is reported with neither of the attempt's state locks (mu,
// emu) held; only the order lock (omu) may be, as a listener's refusal and
// the outcome are reported (review r3 #5). The control is the run finishing
// at all; a held lock would hang it.
func TestObserverIsCalledOutsideTheLocks(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	useListener(t, false)
	dir := nativeDir(t)
	var a *Attempt
	var seen []EventKind
	var mu sync.Mutex
	observe := func(ev Event) {
		mu.Lock()
		seen = append(seen, ev.Kind)
		mu.Unlock()
		if a == nil {
			return
		}
		// Each lock is taken and released: a held one would block here.
		a.mu.Lock()
		_ = a.over
		a.mu.Unlock()
		a.emu.Lock()
		_ = a.ended
		a.emu.Unlock()
	}
	var err error
	a, err = Begin(context.Background(), dir, BeginOptions{Observe: observe})
	if err != nil {
		t.Fatal(err)
	}
	// The listener is closed however the test ends — with shut, which takes
	// no event lock, so a deadlock this test finds cannot hang its cleanup.
	t.Cleanup(a.shut)
	resp, err := http.Get("http://" + a.ln.Addr().String() + callbackPath + "?code=x&state=other")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := a.Paste("not an address"); err == nil {
		t.Fatal("a line that is no address was taken")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Close(CloseEsc)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("an observer that takes the attempt's locks deadlocked the close")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(seen, EventListenerRefused) || !slices.Contains(seen, EventPasteRefused) || !slices.Contains(seen, EventCancelled) {
		t.Fatalf("the observer saw %v", seen)
	}
}

// TestNoRefusalAfterTheEnd (plan 034 review r3 #5): a listener's refusal that
// reaches the attempt as it ends — its handler paused before it counted, the
// attempt closed meanwhile, the handler let go — is neither counted nor
// reported: nothing follows the outcome, and the count the end reported
// (none for one refusal, its total for more) is the attempt's count. The
// first of its kind, and a second after one already reported, are both
// held; the control is the refusals before the pause, counted and reported.
func TestNoRefusalAfterTheEnd(t *testing.T) {
	for _, before := range []int{0, 1} {
		t.Run(fmt.Sprintf("%d before", before), func(t *testing.T) {
			f := newFake(t)
			useFake(t, f)
			useListener(t, false)
			sink := &eventSink{}
			a, err := Begin(context.Background(), nativeDir(t), BeginOptions{Observe: sink.observe})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { a.Close(CloseDone) })
			// other is the browser's return from another attempt, to the
			// attempt's own handler.
			other := func() {
				req := httptest.NewRequest(http.MethodGet, callbackPath+"?code=fake-code-other&state=another-attempts-state", nil)
				a.ServeHTTP(httptest.NewRecorder(), req)
			}
			for range before {
				other()
			}
			reached, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			setVar(t, &beforeRefuse, func(Refusal) {
				once.Do(func() {
					close(reached)
					<-resume
				})
			})
			handled := make(chan struct{})
			go func() {
				defer close(handled)
				other()
			}()
			<-reached
			a.Close(CloseEsc)
			close(resume)
			<-handled

			evs := sink.all()
			if last := evs[len(evs)-1]; last.Kind != EventCancelled || last.Reason != Reason(CloseEsc) {
				t.Fatalf("the last event is %+v; want the outcome, cancelled by esc:\n%s", last, eventsText(evs))
			}
			refused, totals := 0, 0
			for _, ev := range evs {
				switch ev.Kind {
				case EventListenerRefused:
					refused++
				case EventListenerRefusals:
					totals++
				}
			}
			a.emu.Lock()
			n := a.refused[RefusalOtherAttempt]
			a.emu.Unlock()
			if n != before || refused != before || totals != 0 {
				t.Fatalf("counted %d, reported %d and %d totals; want the %d before the end alone:\n%s", n, refused, totals, before, eventsText(evs))
			}
			select {
			case <-a.Ended():
			default:
				t.Fatal("the attempt reported its outcome but Ended is open")
			}
		})
	}
}

// TestEndedFollowsTheOutcome (plan 034 review r3 #8a): Ended is closed only
// once the attempt's one outcome has been reported — by Close when no Wait
// is finishing a redirect, and by Wait as it returns when one is, which a UI
// ending with the program waits for before it closes the sign-in log. The
// control is Ended open before either.
func TestEndedFollowsTheOutcome(t *testing.T) {
	ended := func(a *Attempt) bool {
		select {
		case <-a.Ended():
			return true
		default:
			return false
		}
	}
	t.Run("closed", func(t *testing.T) {
		f := newFake(t)
		useFake(t, f)
		a, err := Begin(context.Background(), nativeDir(t), BeginOptions{PasteOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if ended(a) {
			t.Fatal("the control: Ended is closed before the attempt ended")
		}
		a.Close(CloseEsc)
		if !ended(a) {
			t.Fatal("Close reported the outcome but Ended is open")
		}
	})
	t.Run("a Wait finishing its redirect", func(t *testing.T) {
		f := newFake(t)
		useFake(t, f)
		hold := make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(hold) }) }
		t.Cleanup(release) // a failed test leaves no exchange held
		f.mu.Lock()
		f.holdExchange = hold
		f.mu.Unlock()
		sink := &eventSink{}
		a, err := Begin(context.Background(), nativeDir(t), BeginOptions{PasteOnly: true, Observe: sink.observe})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Paste(a.RedirectURI() + "?" + f.authorize(t, a.URL()).Encode()); err != nil {
			t.Fatal(err)
		}
		waited := waitAsync(t, a, context.Background())
		a.Close(CloseShutdown) // the Wait is finishing the redirect: the outcome is Wait's
		if ended(a) {
			t.Fatal("Ended closed before the Wait finishing the redirect reported its outcome")
		}
		release()
		if err := await(t, waited, "the wait"); err != nil {
			t.Fatalf("the Wait = %v", err)
		}
		select {
		case <-a.Ended():
		case <-time.After(10 * time.Second):
			t.Fatal("Ended never closed after the Wait reported its outcome")
		}
		if evs := sink.all(); evs[len(evs)-1].Kind != EventSignedIn {
			t.Fatalf("the outcome is not the last event:\n%s", eventsText(evs))
		}
	})
}
