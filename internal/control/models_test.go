package control_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/transcript"
	"github.com/charliek/craze/internal/tui"
)

// withRefresh builds the host over the Stub as a session that takes up models
// while it runs (tui.RefreshingStub; plan 034 §3.4), reading dir as its native
// directory.
func withRefresh(dir string) hostOpt {
	return withSession(func(s *tui.Stub) agent.Session {
		s.SetNativeDir(dir)
		return tui.RefreshingStub{Stub: s}
	})
}

// refreshModels sends session.models.refresh and returns its id.
func refreshModels(c *client, h *host, nativeDir string) string {
	c.t.Helper()
	return c.send(protocol.MethodModelsRefresh, protocol.ModelsRefreshParams{SessionID: sid(h), NativeDir: nativeDir})
}

// TestModelsRefreshIsServedWhereTheSessionCanRefresh (plan 034 §3.4, Q17;
// A25): a session that cannot take up models while it runs — the plain Stub,
// as an ACP session is — has no modelsRefresh in its info document, and the
// method is refused unsupported, reason models_refresh_unsupported, whatever
// its params (a relative nativeDir is not judged); one that can says
// modelsRefresh: true and answers, here current with its revision and
// sameDir. Negative control: a server that serves the method whatever its
// session answers the plain Stub's call (the engine's unsupported status)
// instead of refusing it.
func TestModelsRefreshIsServedWhereTheSessionCanRefresh(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	a.sayHello(nil)
	r := a.attach(attachParams(h))
	if r.Session.Capabilities.ModelsRefresh {
		t.Fatalf("a session that cannot refresh says modelsRefresh: %+v", r.Session.Capabilities)
	}
	a.note(protocol.NotifySynchronized)
	for _, dir := range []string{"", "relative/dir"} {
		id := refreshModels(a, h, dir)
		refusedWith(t, a.reply(id), protocol.RPCRefused, protocol.CodeUnsupported, protocol.ReasonModelsRefreshUnsupported)
	}

	h = newHost(t, withRefresh("/home/fake/.craze/native"))
	a = h.dial()
	a.sayHello(nil)
	r = a.attach(attachParams(h))
	if !r.Session.Capabilities.ModelsRefresh {
		t.Fatalf("a session that can refresh does not say so: %+v", r.Session.Capabilities)
	}
	a.note(protocol.NotifySynchronized)
	got := ok[protocol.ModelsRefreshResult](t, a.reply(refreshModels(a, h, "/home/fake/.craze/native/")))
	if got.Status != protocol.ModelsCurrent || got.Revision != 0 || got.SameDir == nil || !*got.SameDir {
		t.Fatalf("a refresh with nothing changed: %+v; want current, revision 0, sameDir true", got)
	}
}

// TestModelsRefreshParamsAreChecked (plan 034 §3.4): a nativeDir must be an
// absolute path of at most NativeDirMax characters — -32602, bad_request
// otherwise, with nothing asked of the session — a field the method does not
// define is unknown_field, and a session this host does not serve
// unknown_session. Negative control: a check that lets a relative nativeDir
// through answers it.
func TestModelsRefreshParamsAreChecked(t *testing.T) {
	h := newHost(t, withRefresh("/n"))
	a := h.dial()
	a.sayHello(nil)
	refusedWith(t, a.call(protocol.MethodModelsRefresh, protocol.ModelsRefreshParams{SessionID: sid(h), NativeDir: "n"}),
		protocol.RPCInvalidParams, protocol.CodeBadRequest, protocol.ReasonBadRequest)
	long := "/" + strings.Repeat("é", protocol.NativeDirMax)
	refusedWith(t, a.call(protocol.MethodModelsRefresh, protocol.ModelsRefreshParams{SessionID: sid(h), NativeDir: long}),
		protocol.RPCInvalidParams, protocol.CodeBadRequest, protocol.ReasonBadRequest)
	refusedWith(t, a.call(protocol.MethodModelsRefresh, map[string]any{"sessionId": sid(h), "commandId": "1"}),
		protocol.RPCInvalidParams, protocol.CodeBadRequest, protocol.ReasonUnknownField)
	refusedWith(t, a.call(protocol.MethodModelsRefresh, protocol.ModelsRefreshParams{SessionID: "not-this-one"}),
		protocol.RPCRefused, protocol.CodeUnknownSession, protocol.ReasonUnknownSession)
	// At the bound, it is a path like any other.
	atBound := "/" + strings.Repeat("é", protocol.NativeDirMax-1)
	got := ok[protocol.ModelsRefreshResult](t, a.call(protocol.MethodModelsRefresh, protocol.ModelsRefreshParams{SessionID: sid(h), NativeDir: atBound}))
	if got.Status != protocol.ModelsCurrent || got.SameDir == nil || *got.SameDir {
		t.Fatalf("a refresh at the bound: %+v; want current, sameDir false", got)
	}
}

// TestAnAppliedRefreshFollowsItsCatalogDelta (plan 034 §3.4, Q17; A25, A26):
// a refresh that takes up a staged list publishes the catalog section — the
// whole list and revision 1 — to every attached client, and its reply,
// applied at revision 1, follows the event on the calling connection (the
// reply barrier). sameDir says false for another native directory. A
// second refresh with nothing changed is current at revision 1 (idempotent).
// Then every read a client catches up from carries the new list: a new
// attach's info document (catalogs.models and catalogs.revision) and its
// snapshot's settings.catalog, session.state's settings.catalog, and
// sessions.list's row. Negative control: a handler that answers without the
// reply barrier hands the reply over before the event.
func TestAnAppliedRefreshFollowsItsCatalogDelta(t *testing.T) {
	h := newHost(t, withRefresh("/home/fake/.craze/native"))
	before := ok[protocol.StateResult](t, func() *protocol.Response {
		c := h.dial()
		c.sayHello(nil)
		return c.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)})
	}())
	if before.Settings.Catalog != nil {
		t.Fatalf("session.state carries a catalog before any reload: %s", before.Settings.Catalog)
	}
	a, b := h.dial(), h.dial()
	for _, c := range []*client{a, b} {
		c.sayHello(nil)
		c.attach(attachParams(h))
		c.note(protocol.NotifySynchronized)
	}
	staged := []agent.ModelInfo{{ID: "grok", Name: "Grok"}, {ID: "fast", Name: "Fast"}, {ID: "native/new", Name: "New", Recent: 1}}
	h.stub.StageModels(staged)
	want := &agent.CatalogState{Models: staged, Revision: 1}

	id := refreshModels(a, h, "/elsewhere/.craze/native")
	notes, resp := a.until(id)
	if len(notes) != 1 {
		t.Fatalf("%d notifications before the reply, want the catalog event alone", len(notes))
	}
	if _, ev := eventOf(t, notes[0]); ev.State == nil || !reflect.DeepEqual(ev.State.Catalog, want) {
		t.Fatalf("the event before the reply: %+v; want the catalog %+v", ev.State, want)
	}
	got := ok[protocol.ModelsRefreshResult](t, resp)
	if got.Status != protocol.ModelsApplied || got.Revision != 1 || got.SameDir == nil || *got.SameDir {
		t.Fatalf("the applied refresh: %+v; want applied, revision 1, sameDir false", got)
	}
	if _, ev := eventOf(t, b.anyNote()); ev.State == nil || !reflect.DeepEqual(ev.State.Catalog, want) {
		t.Fatalf("the other client's event: %+v; want the catalog %+v", ev.State, want)
	}
	if got := ok[protocol.ModelsRefreshResult](t, a.reply(refreshModels(a, h, ""))); got.Status != protocol.ModelsCurrent ||
		got.Revision != 1 || got.SameDir != nil {
		t.Fatalf("a second refresh: %+v; want current, revision 1, no sameDir", got)
	}

	c := h.dial()
	c.sayHello(nil)
	r := c.attach(attachParams(h))
	wantModels := []protocol.CatalogModel{{ID: "grok", Name: "Grok"}, {ID: "fast", Name: "Fast"}, {ID: "native/new", Name: "New", Recent: 1}}
	if !reflect.DeepEqual(r.Session.Catalogs.Models, wantModels) || r.Session.Catalogs.Revision != 1 {
		t.Fatalf("the attach after it: %+v; want %+v at revision 1", r.Session.Catalogs, wantModels)
	}
	snap, err := transcript.DecodeSnapshot(r.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snap.Settings.Catalog, want) {
		t.Fatalf("the attach's snapshot: catalog %+v, want %+v", snap.Settings.Catalog, want)
	}
	c.note(protocol.NotifySynchronized)
	st := ok[protocol.StateResult](t, c.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)}))
	if cat, err := agent.DecodeCatalogState(st.Settings.Catalog); err != nil || !reflect.DeepEqual(cat, want) {
		t.Fatalf("session.state's catalog: %s → %+v, %v; want %+v", st.Settings.Catalog, cat, err, want)
	}
	l := ok[protocol.SessionsListResult](t, c.call(protocol.MethodSessionsList, protocol.SessionsListParams{}))
	if row := l.Sessions[0]; !reflect.DeepEqual(row.Catalogs.Models, wantModels) || row.Catalogs.Revision != 1 {
		t.Fatalf("the row's catalogs: %+v", row.Catalogs)
	}
}

// TestAnUnchangedListWritesNoRevision (plan 034 §3.4, A25's compatibility):
// a session that has never reloaded — every ACP session, a native one before
// its first change — writes no catalogs.revision, no catalog in session.state
// and no modelsRefresh key, so an older client reads exactly the document it
// always did.
func TestAnUnchangedListWritesNoRevision(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	a.sayHello(nil)
	resp := a.call(protocol.MethodSessionAttach, attachParams(h))
	for _, key := range []string{`"revision"`, `"modelsRefresh"`, `"catalog"`} {
		if bytes.Contains(resp.Result, []byte(key)) {
			t.Fatalf("the attach reply of a session with its first list writes %s: %s", key, resp.Result)
		}
	}
	a.note(protocol.NotifySynchronized)
	st := a.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)})
	if bytes.Contains(st.Result, []byte(`"catalog"`)) {
		t.Fatalf("session.state writes a catalog: %s", st.Result)
	}
}

// TestTheRefreshStatusesAreTheAgents (plan 034 §3.4): the wire's refresh
// statuses are agent.ModelsStatus's five, value for value and in order, since
// the handler converts one to the other by its string (session.models.refresh
// .json's enum is held to protocol.ModelsStatuses by internal/protocol's
// test). Negative control: a wire status spelled otherwise fails it.
func TestTheRefreshStatusesAreTheAgents(t *testing.T) {
	agents := []agent.ModelsStatus{agent.ModelsApplied, agent.ModelsCurrent, agent.ModelsPending, agent.ModelsUnsupported, agent.ModelsFailed}
	wire := protocol.ModelsStatuses()
	if len(wire) != len(agents) {
		t.Fatalf("the wire has %d statuses, agent %d", len(wire), len(agents))
	}
	for i, a := range agents {
		if string(wire[i]) != string(a) {
			t.Fatalf("status %d: the wire's %q, agent's %q", i, wire[i], a)
		}
	}
}
