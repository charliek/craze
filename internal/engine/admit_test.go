package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
)

// admittingFake is the fake session as one that admits what it is sent
// (agent.PromptAdmitter): once started it redacts "SECRET", as native redacts
// a key in a shell context block once Start has installed its harness, and
// before that it knows no key and redacts nothing — the redactor it takes at
// admission's entry is what it applies. It remembers what reached its
// Interject, and can hold the next admission (holdNextAdmission).
type admittingFake struct {
	*fakeSession
	up atomic.Bool

	mu         sync.Mutex
	admitted   []string
	interjects []string
	entered    chan struct{}
	release    chan struct{}
}

func (s *admittingFake) Start(ctx context.Context) error {
	err := s.fakeSession.Start(ctx)
	if err == nil {
		s.up.Store(true)
	}
	return err
}

func (s *admittingFake) AdmitPrompt(text string) string {
	redacts := s.up.Load()
	s.mu.Lock()
	s.admitted = append(s.admitted, text)
	entered, release := s.entered, s.release
	s.entered, s.release = nil, nil
	s.mu.Unlock()
	if entered != nil {
		close(entered)
		<-release
	}
	if !redacts {
		return text
	}
	return strings.ReplaceAll(text, "SECRET", "[redacted]")
}

// holdNextAdmission holds the next AdmitPrompt after it has taken its
// redactor: entered closes as it is held, and release lets it return.
func (s *admittingFake) holdNextAdmission() (entered <-chan struct{}, release func()) {
	in, out := make(chan struct{}), make(chan struct{})
	s.mu.Lock()
	s.entered, s.release = in, out
	s.mu.Unlock()
	return in, sync.OnceFunc(func() { close(out) })
}

func (s *admittingFake) Interject(ctx context.Context, text string) error {
	s.mu.Lock()
	s.interjects = append(s.interjects, text)
	s.mu.Unlock()
	return s.fakeSession.Interject(ctx, text)
}

// TestTheEngineAdmitsWhatItIsSent is plan 037 N1's engine half: every text a
// client gives the engine — a prompt that starts a turn, one that is queued
// behind it, a row queued or edited directly, an interjection — is admitted
// by the session before the engine records any of it, so the turn's started
// event, the queue's rows and their events (both of which the journal keeps),
// and what the session is handed all carry the admitted text and never the
// one that was sent. A session that admits nothing is handed the text as it
// was sent (the rest of this package's tests).
func TestTheEngineAdmitsWhatItIsSent(t *testing.T) {
	fake := newFake(t, agent.EventLogOptions{NoPrimary: true})
	s := &admittingFake{fakeSession: fake}
	e, err := newEngine(s, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	sub, err := e.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, s: fake, e: e, sub: sub}
	client := e.NewClientID()
	cmd := func(n int) Command { return Command{Client: client, ID: fmt.Sprint(n)} }

	sc := fake.script(held())
	res, err := e.Submit(cmd(1), "SECRET one", SubmitQueue, "")
	if err != nil || res.Turn == "" || res.Text != "[redacted] one" {
		t.Fatalf("the first submit: %+v, %v", res, err)
	}
	<-sc.opened
	queued, err := e.Submit(cmd(2), "SECRET two", SubmitQueue, "")
	if err != nil || queued.Queued == nil || queued.Queued.Text != "[redacted] two" {
		t.Fatalf("the queued submit: %+v, %v", queued, err)
	}
	three, err := e.Queue(cmd(3), "SECRET three")
	if err != nil || three.Text != "[redacted] three" {
		t.Fatalf("the queued row: %+v, %v", three, err)
	}
	if err := e.EditQueued(cmd(4), three.ID, "SECRET four", nil); err != nil {
		t.Fatal(err)
	}
	_ = e.Interject(context.Background(), cmd(5), "SECRET five")
	r.wantRows("[redacted] two", "[redacted] four")
	sc.release()

	// The whole record, up to the chain's last ending (the two rows drain):
	// no SECRET anywhere in it.
	for _, ev := range r.until(lastEnding) {
		if body := fmt.Sprintf("%+v %+v %+v", ev.Turn, ev.Queue, ev.Text); strings.Contains(body, "SECRET") {
			t.Fatalf("the record carries what was sent: %s", body)
		}
	}
	want := []string{"[redacted] one", "[redacted] two", "[redacted] four"}
	if got := fake.prompts(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("the session was handed %q, want the admitted texts %q", got, want)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.interjects) != 1 || s.interjects[0] != "[redacted] five" {
		t.Fatalf("Interject was handed %q, want the admitted text", s.interjects)
	}
	sent := []string{"SECRET one", "SECRET two", "SECRET three", "SECRET four", "SECRET five"}
	if strings.Join(s.admitted, "|") != strings.Join(sent, "|") {
		t.Fatalf("admitted %q, want each text once, as it was sent: %q", s.admitted, sent)
	}
}

// TestNothingIsAdmittedBeforeTheEngineHasStarted is astra r11's P1 on N1: a
// command that reached admission while the session was still starting — a
// socket client can, since a host publishes its engine before it starts it —
// took the redactor of a session that knew no key yet, and if it was then
// recorded after the start opened the gate, the key went with it into the
// turn's started event or the queued row, the journal that keeps both, and
// the answer. So the engine refuses it, as it refuses every command before
// the start, before the session's redactor is taken.
//
// Deterministic: the admission is held, when it is reached at all, the
// engine is started under it, and only then is it let go. Each case then
// queues a marker after the start, which a started session admits; the
// record up to the marker must hold no SECRET, and the command's own answer
// must be a refusal or hold none either.
func TestNothingIsAdmittedBeforeTheEngineHasStarted(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(e *Engine, c Command) (string, error)
	}{
		{"submit", func(e *Engine, c Command) (string, error) {
			res, err := e.Submit(c, "SECRET early", SubmitQueue, "")
			if res.Queued != nil {
				return res.Queued.Text, err
			}
			return res.Text, err
		}},
		{"queue", func(e *Engine, c Command) (string, error) {
			row, err := e.Queue(c, "SECRET early")
			return row.Text, err
		}},
		{"interject", func(e *Engine, c Command) (string, error) {
			return "", e.Interject(context.Background(), c, "SECRET early")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake(t, agent.EventLogOptions{NoPrimary: true})
			s := &admittingFake{fakeSession: fake}
			e, err := newEngine(s, Options{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Close() })
			sub, err := e.Subscribe(agent.SubscribeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(sub.Close)
			r := &rig{t: t, s: fake, e: e, sub: sub}
			client := e.NewClientID()

			entered, release := s.holdNextAdmission()
			t.Cleanup(release)
			type answer struct {
				text string
				err  error
			}
			done := make(chan answer, 1)
			go func() {
				text, err := tc.run(e, Command{Client: client, ID: "1"})
				done <- answer{text, err}
			}()
			var got answer
			select {
			case <-entered:
				// Admitted before the gate opened, with no key known: the
				// start completes under it, and then it goes on.
				if err := e.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				release()
				got = <-done
			case got = <-done:
				release()
				if err := e.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
			case <-time.After(watchdog):
				t.Fatal("the early command neither reached admission nor answered")
			}
			if got.err == nil && strings.Contains(got.text, "SECRET") {
				t.Fatalf("the early command was answered with what was sent: %q", got.text)
			}
			if got.err != nil && !errors.Is(got.err, ErrNotAccepting) && !errors.Is(got.err, agent.ErrUnsupported) {
				t.Fatalf("the early command: %v, want the unstarted engine's refusal", got.err)
			}

			marker, err := e.Queue(Command{Client: client, ID: "2"}, "SECRET marker")
			if err != nil || marker.Text != "[redacted] marker" {
				t.Fatalf("the marker after the start: %+v, %v", marker, err)
			}
			for _, ev := range r.until(func(ev agent.Event) bool {
				return ev.Type == agent.EventQueue && ev.Queue != nil && ev.Queue.ID == marker.ID
			}) {
				if body := fmt.Sprintf("%+v %+v %+v", ev.Turn, ev.Queue, ev.Text); strings.Contains(body, "SECRET") {
					t.Fatalf("the record carries what was sent before the start: %s", body)
				}
			}
			for _, p := range fake.prompts() {
				if strings.Contains(p, "SECRET") {
					t.Fatalf("the session was handed %q", p)
				}
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, text := range s.interjects {
				if strings.Contains(text, "SECRET") {
					t.Fatalf("Interject was handed %q", text)
				}
			}
		})
	}
}
