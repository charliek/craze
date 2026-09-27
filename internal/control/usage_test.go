package control_test

import (
	"bytes"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/tui"
)

// usageSession is the host's Stub with a usage section in its snapshot: what
// a native session's is once it has reported one (plan 028 §3.14).
type usageSession struct {
	*tui.Stub
	usage *agent.UsageState
}

func (s *usageSession) Snapshot() agent.Snapshot {
	snap := s.Stub.Snapshot()
	u := *s.usage
	snap.Usage = &u
	return snap
}

// TestSessionStateCarriesUsageOnlyWhenTheSessionHasOne (plan 028 §3.14, seam
// 4; A32): session.state's settings carry the usage section through the event
// codec's leaf wrapper when the session has one, and no "usage" key at all
// when it has none — every ACP session, the Stub here — never null or {}.
// Every reply is held to session.state.json by the test client (wiretest).
func TestSessionStateCarriesUsageOnlyWhenTheSessionHasOne(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	a.sayHello(nil)
	r := a.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)})
	if bytes.Contains(r.Result, []byte(`"usage"`)) {
		t.Fatalf("a session with no usage answers with the key: %s", r.Result)
	}
	if st := ok[protocol.StateResult](t, r); st.Settings.Usage != nil {
		t.Fatalf("the settings' usage: %s", st.Settings.Usage)
	}

	want := agent.UsageState{ContextTokens: 34_000, ContextWindow: 100_000,
		Turn:    agent.Spend{Input: 10, Output: 5, CostPicoUSD: 20_000_000},
		Session: agent.Spend{Input: 30, Output: 15, CacheRead: 700, CostPicoUSD: 60_000_000, Unpriced: true}}
	h = newHost(t, withSession(func(s *tui.Stub) agent.Session { return &usageSession{Stub: s, usage: &want} }))
	a = h.dial()
	a.sayHello(nil)
	st := ok[protocol.StateResult](t, a.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)}))
	got, err := agent.DecodeUsageState(st.Settings.Usage)
	if err != nil || got == nil || *got != want {
		t.Fatalf("the settings' usage: %s → %+v, %v; want %+v", st.Settings.Usage, got, err, want)
	}
}
