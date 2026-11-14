package websearch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/tool"
)

// Consent (plan 051 §C.4): what an undecided session asks, what each answer
// does, what is remembered, and that a session that cannot ask never does.
// The provider is always a fake MCP server on loopback.

// fakeAsker answers the consent question with answer, after release closes
// when it is set, recording each question it was put.
type fakeAsker struct {
	answer  func(ctx context.Context) (tool.Answers, error)
	release chan struct{}

	mu    sync.Mutex
	asked [][]tool.Question
	// opened is told of each ask as it arrives.
	opened chan struct{}
}

func (a *fakeAsker) AskQuestion(ctx context.Context, qs []tool.Question) (tool.Answers, error) {
	a.mu.Lock()
	a.asked = append(a.asked, qs)
	a.mu.Unlock()
	if a.opened != nil {
		a.opened <- struct{}{}
	}
	if a.release != nil {
		select {
		case <-a.release:
		case <-ctx.Done():
			return tool.Answers{}, nil
		}
	}
	return a.answer(ctx)
}

func (a *fakeAsker) PresentPlan(context.Context, tool.PlanOffer) (tool.PlanOutcome, error) {
	return tool.PlanUnanswered, errors.New("not here")
}

func (a *fakeAsker) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.asked)
}

// picks answers with picked.
func picks(picked ...[]int) func(context.Context) (tool.Answers, error) {
	return func(context.Context) (tool.Answers, error) {
		return tool.Answers{Answered: true, Picked: picked}, nil
	}
}

// firstOption is an asker answering as craze's headless policy does: the
// first option, at once, whatever was asked.
func firstOption() *fakeAsker { return &fakeAsker{answer: picks([]int{0})} }

// consented is a service at f's endpoint that opened undecided, with asker,
// canAsk and the state file at path.
func consented(t *testing.T, f *fakeMCP, asker tool.Asker, canAsk bool, path string, warn func(string)) *Service {
	t.Helper()
	s, err := New(Config{Endpoint: f.srv.URL + "/mcp", Undecided: true, CanAsk: canAsk, Asker: asker, StatePath: path, Warn: warn})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

const oneResult = "Title: A\nURL: https://a.example/\nHighlights:\nx"

func statePath(t *testing.T) string { return filepath.Join(t.TempDir(), "native", StateFile) }

// stateIs says the state file holds want, or does not exist when want is "".
func stateIs(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	switch {
	case want == "" && !os.IsNotExist(err):
		t.Fatalf("the state file was written: %q, %v", b, err)
	case want != "" && (err != nil || string(b) != want):
		t.Fatalf("the state file is %q, %v; want %q", b, err, want)
	}
}

// TestConsentQuestion: the card's one question, its options in order, and the
// detail saying where queries go.
func TestConsentQuestion(t *testing.T) {
	a := &fakeAsker{answer: picks([]int{0})}
	f := newFake(t, replyText(oneResult))
	if _, err := search(consented(t, f, a, true, statePath(t), nil), "q"); err != nil {
		t.Fatal(err)
	}
	if a.count() != 1 {
		t.Fatalf("asked %d times", a.count())
	}
	q := a.asked[0]
	want := []tool.Question{{
		Header:   "Web search",
		Question: "Allow craze to search the web and fetch pages for up-to-date information?",
		Options: []tool.QuestionOption{
			{Label: "Allow via Exa", Description: "Queries and URLs are sent to Exa (exa.ai)."},
			{Label: "Disable web search"},
		},
	}}
	if !reflect.DeepEqual(q, want) {
		t.Fatalf("asked %+v, want %+v", q, want)
	}
}

// TestConsentAllow: exactly the first option allows — the call goes to the
// provider, "exa" is remembered — and later calls, search or fetch, never
// ask again.
func TestConsentAllow(t *testing.T) {
	a := &fakeAsker{answer: picks([]int{0})}
	f := newFake(t, replyText(oneResult))
	path := statePath(t)
	s := consented(t, f, a, true, path, nil)
	if r, err := search(s, "q"); err != nil || len(r) != 1 {
		t.Fatalf("Search = %+v, %v", r, err)
	}
	stateIs(t, path, `{"version":1,"provider":"exa"}`+"\n")
	if _, err := s.Fetch(context.Background(), "https://a.example/"); err != nil {
		t.Fatal(err)
	}
	if _, err := search(s, "again"); err != nil {
		t.Fatal(err)
	}
	if a.count() != 1 || len(f.calls()) != 3 {
		t.Fatalf("asked %d times, %d provider calls; want 1 and 3", a.count(), len(f.calls()))
	}
}

// TestConsentDisable: exactly the second option disables: this call and
// every later one answer that web search is disabled, the provider is never
// called, false is remembered, and nobody is asked again.
func TestConsentDisable(t *testing.T) {
	a := &fakeAsker{answer: picks([]int{1})}
	f := newFake(t, replyText(oneResult))
	path := statePath(t)
	s := consented(t, f, a, true, path, nil)
	_, err := search(s, "q")
	wantErr(t, err, tool.ClassToolError, "Web search is disabled")
	stateIs(t, path, `{"version":1,"provider":false}`+"\n")
	_, err = s.Fetch(context.Background(), "https://a.example/")
	wantErr(t, err, tool.ClassToolError, "Web search is disabled")
	if a.count() != 1 || len(f.calls()) != 0 {
		t.Fatalf("asked %d times, %d provider calls; want 1 and none", a.count(), len(f.calls()))
	}
}

// TestConsentDecline: anything but exactly one of the two options — nobody
// answering, a skip (unanswered), an empty or out-of-range pick, two picks,
// an answer to two questions, an asker's own error — declines for this
// session: the call answers that web search was not approved, nothing is
// remembered, the provider is not called, and the next call does not ask
// again.
func TestConsentDecline(t *testing.T) {
	for name, answer := range map[string]func(context.Context) (tool.Answers, error){
		"unanswered":    func(context.Context) (tool.Answers, error) { return tool.Answers{}, nil },
		"no picks":      picks(),
		"empty pick":    picks([]int{}),
		"out of range":  picks([]int{2}),
		"negative":      picks([]int{-1}),
		"both":          picks([]int{0, 1}),
		"two questions": picks([]int{0}, []int{0}),
		"answered nil":  func(context.Context) (tool.Answers, error) { return tool.Answers{Answered: true}, nil },
		"asker error":   func(context.Context) (tool.Answers, error) { return tool.Answers{}, errors.New("broken") },
	} {
		t.Run(name, func(t *testing.T) {
			a := &fakeAsker{answer: answer}
			f := newFake(t, replyText(oneResult))
			path := statePath(t)
			s := consented(t, f, a, true, path, nil)
			_, err := search(s, "q")
			wantErr(t, err, tool.ClassToolError, "Web search was not approved in this session")
			_, err = s.Fetch(context.Background(), "https://a.example/")
			wantErr(t, err, tool.ClassToolError, "Web search was not approved in this session")
			stateIs(t, path, "")
			if a.count() != 1 || len(f.calls()) != 0 {
				t.Fatalf("asked %d times, %d provider calls; want 1 and none", a.count(), len(f.calls()))
			}
		})
	}
}

// TestConsentCannotAsk: a session that cannot ask — its question policy
// answers on nobody's behalf — never asks, however willing its asker, never
// writes the state file and never calls the provider: an automatic first
// option is not consent. Without an asker it is the same.
func TestConsentCannotAsk(t *testing.T) {
	for name, tc := range map[string]struct {
		asker  tool.Asker
		canAsk bool
	}{
		"first option": {firstOption(), false},
		"no asker":     {nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, replyText(oneResult))
			path := statePath(t)
			s := consented(t, f, tc.asker, tc.canAsk, path, nil)
			for range 2 {
				_, err := search(s, "q")
				wantErr(t, err, tool.ClassToolError, "Web search was not approved in this session")
				_, err = s.Fetch(context.Background(), "https://a.example/")
				wantErr(t, err, tool.ClassToolError, "Web search was not approved in this session")
			}
			stateIs(t, path, "")
			if fa, ok := tc.asker.(*fakeAsker); ok && fa.count() != 0 {
				t.Fatalf("asked %d times", fa.count())
			}
			if len(f.calls()) != 0 {
				t.Fatalf("%d provider calls", len(f.calls()))
			}
		})
	}
}

// TestConsentDecidedNeverAsks: a session that opened on — config.toml, the
// remembered choice, the caller's flag — never asks, whatever it could, and
// writes nothing.
func TestConsentDecidedNeverAsks(t *testing.T) {
	a := firstOption()
	f := newFake(t, replyText(oneResult))
	path := statePath(t)
	s, err := New(Config{Endpoint: f.srv.URL + "/mcp", CanAsk: true, Asker: a, StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := search(s, "q"); err != nil {
		t.Fatal(err)
	}
	stateIs(t, path, "")
	if a.count() != 0 || len(f.calls()) != 1 {
		t.Fatalf("asked %d times, %d provider calls", a.count(), len(f.calls()))
	}
}

// TestConsentCancel: the asking call's context ending before an answer
// aborts it and every call waiting on the same question, remembers nothing
// and decides nothing: the next call asks again (a cancel is not a decline).
func TestConsentCancel(t *testing.T) {
	a := &fakeAsker{answer: picks([]int{0}), release: make(chan struct{}), opened: make(chan struct{}, 4)}
	f := newFake(t, replyText(oneResult))
	path := statePath(t)
	s := consented(t, f, a, true, path, nil)

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := s.Search(ctx, "q"); first <- err }()
	<-a.opened
	waiter := make(chan error, 1)
	go func() { _, err := s.Fetch(context.Background(), "https://a.example/"); waiter <- err }()
	// The waiter is parked on the question in flight, not asking one of its
	// own: no second ask arrives.
	select {
	case <-a.opened:
		t.Fatal("a second question was asked while one was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	wantErr(t, recv(t, first), tool.ClassAborted, tool.AbortedText)
	wantErr(t, recv(t, waiter), tool.ClassAborted, tool.AbortedText)
	stateIs(t, path, "")
	if len(f.calls()) != 0 {
		t.Fatalf("%d provider calls", len(f.calls()))
	}

	// Asked again, and this time allowed.
	close(a.release)
	if _, err := search(s, "q"); err != nil {
		t.Fatal(err)
	}
	<-a.opened
	if a.count() != 2 {
		t.Fatalf("asked %d times, want 2", a.count())
	}
	stateIs(t, path, `{"version":1,"provider":"exa"}`+"\n")
}

// TestConsentLateAnswer: an answer that arrives only after the asking call's
// context has ended — the card resolved as the call was cancelled — is no
// answer: the asking call and every waiter are aborted, nothing is remembered,
// the provider is not called, and the next call asks again.
func TestConsentLateAnswer(t *testing.T) {
	for name, pick := range map[string][]int{"allow": {0}, "disable": {1}} {
		t.Run(name, func(t *testing.T) {
			// The answer comes after the asking ctx ends; a call that cannot be
			// cancelled (the next one) is answered at once.
			a := &fakeAsker{answer: func(ctx context.Context) (tool.Answers, error) {
				if done := ctx.Done(); done != nil {
					<-done
				}
				return tool.Answers{Answered: true, Picked: [][]int{pick}}, nil
			}, opened: make(chan struct{}, 4)}
			f := newFake(t, replyText(oneResult))
			path := statePath(t)
			s := consented(t, f, a, true, path, nil)

			ctx, cancel := context.WithCancel(context.Background())
			first := make(chan error, 1)
			go func() { _, err := s.Search(ctx, "q"); first <- err }()
			<-a.opened
			waiter := make(chan error, 1)
			go func() { _, err := s.Fetch(context.Background(), "https://a.example/"); waiter <- err }()
			select {
			case <-a.opened:
				t.Fatal("a second question was asked while one was in flight")
			case <-time.After(50 * time.Millisecond):
			}
			cancel()
			wantErr(t, recv(t, first), tool.ClassAborted, tool.AbortedText)
			wantErr(t, recv(t, waiter), tool.ClassAborted, tool.AbortedText)
			stateIs(t, path, "")
			if len(f.calls()) != 0 {
				t.Fatalf("%d provider calls", len(f.calls()))
			}

			// Nothing was decided: the next call asks again.
			_, _ = search(s, "again")
			if a.count() != 2 {
				t.Fatalf("asked %d times, want 2: the late answer was kept", a.count())
			}
		})
	}
}

// TestConsentWaiterCancel: a waiting call whose own context ends goes alone;
// the question stays open and its answer reaches the rest.
func TestConsentWaiterCancel(t *testing.T) {
	a := &fakeAsker{answer: picks([]int{0}), release: make(chan struct{}), opened: make(chan struct{}, 4)}
	f := newFake(t, replyText(oneResult))
	s := consented(t, f, a, true, statePath(t), nil)
	first := make(chan error, 1)
	go func() { _, err := search(s, "q"); first <- err }()
	<-a.opened
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() { _, err := s.Search(ctx, "w"); waiter <- err }()
	cancel()
	wantErr(t, recv(t, waiter), tool.ClassAborted, tool.AbortedText)
	close(a.release)
	if err := recv(t, first); err != nil {
		t.Fatal(err)
	}
	if a.count() != 1 {
		t.Fatalf("asked %d times", a.count())
	}
}

// TestConsentConcurrentCalls: searches and fetches arriving together while
// undecided ask once and share the outcome — all go to the provider when it
// is allowed, all answer the same text when it is disabled or declined.
func TestConsentConcurrentCalls(t *testing.T) {
	for name, tc := range map[string]struct {
		answer func(context.Context) (tool.Answers, error)
		text   string
	}{
		"allow":   {picks([]int{0}), ""},
		"disable": {picks([]int{1}), "Web search is disabled"},
		"decline": {picks(), "Web search was not approved in this session"},
	} {
		t.Run(name, func(t *testing.T) {
			a := &fakeAsker{answer: tc.answer, release: make(chan struct{}), opened: make(chan struct{}, 16)}
			f := newFake(t, replyText(oneResult))
			s := consented(t, f, a, true, statePath(t), nil)
			const n = 6
			errs := make(chan error, n)
			for i := range n {
				go func() {
					var err error
					if i%2 == 0 {
						_, err = search(s, "q")
					} else {
						_, err = s.Fetch(context.Background(), "https://a.example/")
					}
					errs <- err
				}()
			}
			<-a.opened
			// Let every call reach the gate before the answer.
			time.Sleep(50 * time.Millisecond)
			close(a.release)
			for range n {
				err := recv(t, errs)
				if tc.text == "" {
					if err != nil {
						t.Fatal(err)
					}
					continue
				}
				wantErr(t, err, tool.ClassToolError, tc.text)
			}
			wantCalls := 0
			if tc.text == "" {
				wantCalls = n
			}
			if a.count() != 1 || len(f.calls()) != wantCalls {
				t.Fatalf("asked %d times, %d provider calls; want 1 and %d", a.count(), len(f.calls()), wantCalls)
			}
		})
	}
}

// TestConsentWriteFails: a state file that cannot be written keeps the
// person's answer for this session — an allow goes on to the provider, a
// disable stays disabled — and says so once on the warn lane, in words that
// name the file but not its path.
func TestConsentWriteFails(t *testing.T) {
	if WriteFailedText != "web search: the answer could not be saved to websearch.json; it holds for this session, and the next one asks again" {
		t.Fatalf("WriteFailedText = %q", WriteFailedText)
	}
	for name, tc := range map[string]struct {
		pick []int
		text string
	}{
		"allow":   {[]int{0}, ""},
		"disable": {[]int{1}, "Web search is disabled"},
	} {
		t.Run(name, func(t *testing.T) {
			blocker := filepath.Join(t.TempDir(), "native")
			writeFile(t, blocker, "a file where the directory should be")
			path := filepath.Join(blocker, StateFile)
			var mu sync.Mutex
			var warned []string
			warn := func(s string) { mu.Lock(); warned = append(warned, s); mu.Unlock() }
			a := &fakeAsker{answer: picks(tc.pick)}
			f := newFake(t, replyText(oneResult))
			s := consented(t, f, a, true, path, warn)
			for range 2 {
				_, err := search(s, "q")
				if tc.text == "" && err != nil {
					t.Fatal(err)
				}
				if tc.text != "" {
					wantErr(t, err, tool.ClassToolError, tc.text)
				}
			}
			if a.count() != 1 {
				t.Fatalf("asked %d times", a.count())
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(warned, []string{WriteFailedText}) {
				t.Fatalf("warned %q", warned)
			}
		})
	}
}

// TestConsentWaitIsNotTheDeadline: the provider's deadline covers the
// provider call only, never the time the person takes to answer.
func TestConsentWaitIsNotTheDeadline(t *testing.T) {
	a := &fakeAsker{answer: func(context.Context) (tool.Answers, error) {
		time.Sleep(300 * time.Millisecond)
		return tool.Answers{Answered: true, Picked: [][]int{{0}}}, nil
	}}
	f := newFake(t, replyText(oneResult))
	s, err := New(Config{Endpoint: f.srv.URL + "/mcp", Undecided: true, CanAsk: true, Asker: a, StatePath: statePath(t), timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := search(s, "q"); err != nil || len(r) != 1 {
		t.Fatalf("Search = %+v, %v; the wait for the answer counted against the call's deadline", r, err)
	}
}

func recv(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("a call did not return")
		return nil
	}
}
