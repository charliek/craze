package engine_test

import (
	"sync/atomic"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/tui"
)

// webDecidingStub is the Stub as a session that decides web search at Open
// (agent.WebSearcher, plan 051 F.4.3), its answer set by the test.
type webDecidingStub struct {
	*tui.Stub
	offered, known *atomic.Bool
}

func (s webDecidingStub) WebSearch() (offered, known bool) { return s.offered.Load(), s.known.Load() }

// TestWebSearchIsTheSessionsAnswer (plan 051 F.4.3): Engine.WebSearch answers
// from a session that is an agent.WebSearcher — whatever it says, read each
// time, so not-yet-known and then known as Open decides — and (false, false)
// for one that is not: the plain Stub, as an ACP session. Negative control: an
// accessor that answers known for any session fails the ACP-like case.
func TestWebSearchIsTheSessionsAnswer(t *testing.T) {
	plain, err := engine.New(tui.NewStubNoPrimary(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	if offered, known := plain.WebSearch(); offered || known {
		t.Fatalf("an ACP-like session: WebSearch() = %v, %v; want false, false", offered, known)
	}

	var offered, known atomic.Bool
	var sess agent.Session = webDecidingStub{Stub: tui.NewStubNoPrimary(), offered: &offered, known: &known}
	e, err := engine.New(sess, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	for _, want := range [][2]bool{{false, false}, {true, true}, {false, true}} {
		offered.Store(want[0])
		known.Store(want[1])
		if o, k := e.WebSearch(); o != want[0] || k != want[1] {
			t.Fatalf("WebSearch() = %v, %v; want %v, %v", o, k, want[0], want[1])
		}
	}
}
