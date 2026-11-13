package control_test

import (
	"context"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/tui"
)

// webStub is the host's Stub as a session that decides web search at Open
// (agent.WebSearcher, the native session's verb; plan 051 F.4.3).
type webStub struct {
	*tui.Stub
	offered, known bool
}

func (s webStub) WebSearch() (offered, known bool) { return s.offered, s.known }

func withWeb(offered, known bool) hostOpt {
	return withSession(func(s *tui.Stub) agent.Session { return webStub{Stub: s, offered: offered, known: known} })
}

// listedWeb is the webSearch of the session's sessions.list row.
func listedWeb(t *testing.T, a *client) *protocol.WebSearchInfo {
	t.Helper()
	return ok[protocol.SessionsListResult](t, a.call(protocol.MethodSessionsList, protocol.SessionsListParams{})).Sessions[0].WebSearch
}

// TestTheInfoDocumentSaysWhetherWebSearchWasOffered (plan 051 F.4.3): a ready
// session that decides web search (agent.WebSearcher) says so in its info
// document — the attach reply's and its sessions.list row's — as
// webSearch.offered, true and false alike (always written); one that has not
// decided yet (known false) and one that is no WebSearcher — the plain Stub,
// as an ACP session is and the fake host's session by default — carry no
// webSearch at all. Negative control: a server that sets the member whatever
// known says fails the undecided case.
func TestTheInfoDocumentSaysWhetherWebSearchWasOffered(t *testing.T) {
	for _, c := range []struct {
		name string
		opts []hostOpt
		want *protocol.WebSearchInfo
	}{
		{"an ACP-like session", nil, nil},
		{"offered", []hostOpt{withWeb(true, true)}, &protocol.WebSearchInfo{Offered: true}},
		{"withheld", []hostOpt{withWeb(false, true)}, &protocol.WebSearchInfo{Offered: false}},
		{"not decided", []hostOpt{withWeb(true, false)}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHost(t, c.opts...)
			if offered, known := h.eng.WebSearch(); c.want == nil && known || c.want != nil && (!known || offered != c.want.Offered) {
				t.Fatalf("engine.WebSearch() = %v, %v; want %+v", offered, known, c.want)
			}
			a := h.dial()
			a.sayHello(nil)
			r := a.attach(attachParams(h))
			if !r.Ready {
				t.Fatal("fixture: the session is not ready")
			}
			if got := r.Session.WebSearch; (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
				t.Fatalf("the attach reply's webSearch is %+v, want %+v", got, c.want)
			}
			a.note(protocol.NotifySynchronized)
			if got := listedWeb(t, a); (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
				t.Fatalf("the sessions.list row's webSearch is %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestWebSearchIsAbsentUntilTheSessionIsReady: beside the catalogs and like
// them, webSearch is left out of a document read before the session is up —
// the when: "now" attach reply before the start, ready: false — and is in the
// final document the ready notification carries.
func TestWebSearchIsAbsentUntilTheSessionIsReady(t *testing.T) {
	h := newHost(t, withoutStart(), withWeb(true, true))
	a := h.dial()
	a.sayHello(nil)
	p := attachParams(h)
	p.When = protocol.WhenNow
	r := a.attach(p)
	if r.Ready {
		t.Fatal("fixture: the session was ready before its start")
	}
	if r.Session.WebSearch != nil {
		t.Fatalf("the reply before the start carries webSearch: %+v", r.Session.WebSearch)
	}
	a.note(protocol.NotifySynchronized)
	if err := h.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for {
		n := a.anyNote()
		if n.Method != protocol.NotifyReady {
			continue
		}
		ready := paramsOf[protocol.ReadyParams](t, n)
		if w := ready.Session.WebSearch; w == nil || !w.Offered {
			t.Fatalf("the ready notification's webSearch is %+v, want offered", w)
		}
		return
	}
}
