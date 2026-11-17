package engine

import (
	"context"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/errkind"
)

// stoppedSession is the fake session as one that has stopped itself for good
// (agent.StopErrer): a claude session's overage guard.
type stoppedSession struct {
	*fakeSession
	err error
}

func (s *stoppedSession) StoppedErr() error { return s.err }

// quotaErr is a stop error that says it is quota.
type quotaErr struct{}

func (quotaErr) Error() string           { return "Claude would bill this as extra usage" }
func (quotaErr) ErrorInfo() errkind.Info { return errkind.New(errkind.Quota) }

// TestCloseEndsTheTurnWithTheSessionsStopError (plan 052 § D3, review C5b
// item 7): a session that stopped itself and whose turn has not settled yet
// when the host's close comes — the prompt goroutine still on its way back —
// has that turn end failed with the stop's error, not merely closed.
func TestCloseEndsTheTurnWithTheSessionsStopError(t *testing.T) {
	ss := &stoppedSession{fakeSession: newFake(t, agent.EventLogOptions{}), err: quotaErr{}}
	e, err := newEngine(ss, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sc := ss.script(held())
	res, err := e.Submit(Command{}, "go", SubmitQueue, "")
	if err != nil || res.Turn == "" {
		t.Fatalf("submit = %+v, %v", res, err)
	}
	await(t, sc.opened, "the turn's opening")
	if err := e.CloseFor(agent.StopCauseOverage); err != nil {
		t.Fatal(err)
	}
	var ending *agent.TurnInfo
	for _, ev := range drainEngine(e) {
		if ended(res.Turn)(ev) {
			ending = ev.Turn
		}
	}
	if ending == nil || ending.Err == "" || ending.Kind != errkind.Quota || ending.StopReason == stopClosing || ending.Cause != agent.StopCauseOverage {
		t.Fatalf("the ending %+v; want failed with the stop's quota error, cause overage", ending)
	}
}
