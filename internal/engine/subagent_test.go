package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// cancellerFake is the fake session with a per-child stop
// (agent.SubagentCanceller): it records every id it is asked to stop and
// answers each with err. Everything else is the fakeSession's own.
type cancellerFake struct {
	*fakeSession

	stopMu sync.Mutex
	stops  []string
	err    error
}

func (s *cancellerFake) CancelSubagent(id string) error {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	s.stops = append(s.stops, id)
	return s.err
}

// stopped is every id the session was asked to stop, in order.
func (s *cancellerFake) stopped() []string {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	return slices.Clone(s.stops)
}

// newCancellerEngine is a started engine over a cancellerFake answering every
// stop with answer.
func newCancellerEngine(t *testing.T, answer error) (*Engine, *cancellerFake) {
	t.Helper()
	s := &cancellerFake{fakeSession: newFake(t, agent.EventLogOptions{NoPrimary: true}), err: answer}
	e, err := newEngine(s, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e, s
}

// TestEngineCancelSubagentUnsupported (A12, §3.10): a session without the
// per-child stop — the fake, as every ACP session — is answered
// agent.ErrUnsupported, code unsupported, by the engine itself.
func TestEngineCancelSubagentUnsupported(t *testing.T) {
	r := newRig(t, Options{})
	if _, ok := agent.Session(r.s).(agent.SubagentCanceller); ok {
		t.Fatal("control: the fake session has a per-child stop")
	}
	for _, c := range []Command{{}, {Client: r.e.NewClientID(), ID: "1"}} {
		err := r.e.CancelSubagent(c, "child-1")
		if !errors.Is(err, agent.ErrUnsupported) || Code(err) != "unsupported" {
			t.Fatalf("CancelSubagent(%+v) on a session with no per-child stop = %v (%s); want agent.ErrUnsupported, unsupported",
				c, err, Code(err))
		}
	}
}

// TestEngineCancelSubagentPassesThrough (§3.10): the engine adds nothing but
// the door. The id reaches the session as it was sent, once per command, and
// the session's nil comes back; with no gate of the engine's own, a stop goes
// through with no turn of craze's running, since the session is the one that
// knows which of its children run.
func TestEngineCancelSubagentPassesThrough(t *testing.T) {
	e, s := newCancellerEngine(t, nil)
	if err := e.CancelSubagent(Command{Client: e.NewClientID(), ID: "1"}, "child-1"); err != nil {
		t.Fatalf("CancelSubagent = %v; want the session's nil", err)
	}
	if err := e.CancelSubagent(Command{}, "child-2"); err != nil {
		t.Fatalf("CancelSubagent with no command = %v; want the session's nil", err)
	}
	if got := s.stopped(); !slices.Equal(got, []string{"child-1", "child-2"}) {
		t.Fatalf("the session was asked to stop %q; want each id once, as sent", got)
	}
}

// TestEngineCancelSubagentUnknownIsStored (§3.10): the session's
// agent.ErrNoSuchSubagent is code unknown_subagent, and it is a stored answer:
// a resend of the same command id replays it without asking the session
// again, while a new id is a new stop.
func TestEngineCancelSubagentUnknownIsStored(t *testing.T) {
	e, s := newCancellerEngine(t, agent.ErrNoSuchSubagent)
	c := Command{Client: e.NewClientID(), ID: "1"}
	for i := range 2 {
		if err := e.CancelSubagent(c, "gone"); !errors.Is(err, agent.ErrNoSuchSubagent) || Code(err) != "unknown_subagent" {
			t.Fatalf("attempt %d: CancelSubagent = %v (%s); want agent.ErrNoSuchSubagent, unknown_subagent", i+1, err, Code(err))
		}
	}
	if got := s.stopped(); len(got) != 1 {
		t.Fatalf("the session was asked %d times; want once: the resend replays the stored answer", len(got))
	}
	if err := e.CancelSubagent(Command{Client: c.Client, ID: "2"}, "gone"); !errors.Is(err, agent.ErrNoSuchSubagent) {
		t.Fatalf("a new command id = %v; want the session asked again", err)
	}
	if got := s.stopped(); len(got) != 2 {
		t.Fatalf("the session was asked %d times; want twice: a new id is a new stop", len(got))
	}
}

// TestEngineCancelSubagentAfterClose (X24): a closed engine refuses the stop
// itself, ErrNotAccepting (code not_accepting), as GiveUp and Cancel do, and
// the session is never asked. The refusal is a gate's and never stored: the
// same id sent again with another payload is refused the same way, where a
// stored answer would have made it a mismatched resend (bad_request).
func TestEngineCancelSubagentAfterClose(t *testing.T) {
	e, s := newCancellerEngine(t, nil)
	c := Command{Client: e.NewClientID(), ID: "1"}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"child-1", "child-2"} {
		if err := e.CancelSubagent(c, id); !errors.Is(err, ErrNotAccepting) || Code(err) != "not_accepting" {
			t.Fatalf("CancelSubagent(%q) after Close = %v (%s); want ErrNotAccepting, not_accepting, never stored", id, err, Code(err))
		}
	}
	if got := s.stopped(); len(got) != 0 {
		t.Fatalf("a closed engine asked the session to stop %q", got)
	}
}
