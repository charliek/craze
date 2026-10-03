package remote_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/transcript"
	"github.com/charliek/craze/internal/tui"
)

// withRefreshing serves the Stub as a session that takes up models while it
// runs (tui.RefreshingStub; plan 034 §3.4), reading dir as its native
// directory: what a native session is over the wire.
func withRefreshing(dir string) hostOpt {
	return func(c *hostConfig) {
		c.session = func(s *tui.Stub) agent.Session {
			s.SetNativeDir(dir)
			return tui.RefreshingStub{Stub: s}
		}
	}
}

// modelList is a list of models, each named for itself.
func modelList(ids ...string) []agent.ModelInfo {
	out := make([]agent.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, agent.ModelInfo{ID: id, Name: id})
	}
	return out
}

// sentMethod counts the requests the client wrote for method.
func sentMethod(tp *tap, method string) int {
	n := 0
	for _, l := range tp.linesFrom(0) {
		if l.out && l.method == method && l.resp == nil {
			n++
		}
	}
	return n
}

// hostRefresh is the host's own refresh, as a key saved in its directory and
// a TUI's call would make it: what models it stages, taken up now.
func hostRefresh(t *testing.T, h *host, models []agent.ModelInfo) agent.ModelsRefresh {
	t.Helper()
	h.stub.StageModels(models)
	r, err := h.eng.RefreshModels(tctx(t), "")
	if err != nil {
		t.Fatalf("the host's refresh: %v", err)
	}
	return r
}

// catalogOf is an event item's catalog section, nil for none.
func catalogOf(it backend.Item) *agent.CatalogState {
	if it.Kind != backend.ItemEvent || it.Event.State == nil {
		return nil
	}
	return it.Event.State.Catalog
}

// TestRefreshModelsMakesNoCallWithoutTheCapability (plan 034 A25: a new
// client with an old host makes no call): a session whose info document does
// not say modelsRefresh — before any attach, and an ACP session's (the plain
// Stub), which is what an older host's document looks like too — is answered
// unsupported with nothing written to the host. Negative control: a
// RefreshModels that asks whatever the document says writes the request.
func TestRefreshModelsMakesNoCallWithoutTheCapability(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	s := dialSession(t, h.path, tp, remote.SessionOptions{Provider: "cursor"})
	for _, when := range []string{"before the attach", "after it"} {
		if when == "after it" {
			if err := s.Start(tctx(t)); err != nil {
				t.Fatalf("start: %v", err)
			}
			if s.Info().ModelsRefresh {
				t.Fatalf("an ACP session's info says modelsRefresh")
			}
		}
		r, err := s.RefreshModels(tctx(t), "/home/me/.craze/native")
		if err != nil || r.Status != agent.ModelsUnsupported || r.Revision != 0 || r.SameDir != nil {
			t.Fatalf("%s: %+v, %v; want unsupported and nothing else", when, r, err)
		}
		if n := sentMethod(tp, protocol.MethodModelsRefresh); n != 0 {
			t.Fatalf("%s: the client wrote %d session.models.refresh requests, want none", when, n)
		}
	}
}

// TestARefreshIsAnsweredAsTheHostAnswers (plan 034 §3.4, Q17; A25, A26): over
// the socket a native session's refresh is the host's answer in the backend's
// types — applied at revision 1, its catalog event already on the stream when
// the answer is read (the reply barrier), and sameDir false for another
// native directory, true for the host's own — and the info document of a
// later attach carries the new list at its revision. Negative control: a
// sessionInfo that drops the wire's catalogs.revision leaves the later
// client's Info at revision 0.
func TestARefreshIsAnsweredAsTheHostAnswers(t *testing.T) {
	h := newHost(t, withRefreshing("/home/host/.craze/native"))
	tp := newTap(t)
	s, _ := started(t, h, tp, remote.SessionOptions{})
	if info := s.Info(); !info.ModelsRefresh || info.CatalogRevision != 0 {
		t.Fatalf("a native session's info: modelsRefresh %v, revision %d; want true, 0", info.ModelsRefresh, info.CatalogRevision)
	}
	h.stub.StageModels(modelList("grok", "fast", "native/new"))
	r, err := s.RefreshModels(tctx(t), "/home/tui/.craze/native")
	if err != nil || r.Status != agent.ModelsApplied || r.Revision != 1 || r.SameDir == nil || *r.SameDir {
		t.Fatalf("the refresh: %+v, %v; want applied, revision 1, sameDir false", r, err)
	}
	want := &agent.CatalogState{Models: modelList("grok", "fast", "native/new"), Revision: 1}
	if got := catalogOf(readKind(t, s, backend.ItemEvent)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the stream's next item carries %+v, want the catalog %+v", got, want)
	}
	r, err = s.RefreshModels(tctx(t), "/home/host/.craze/native")
	if err != nil || r.Status != agent.ModelsCurrent || r.Revision != 1 || r.SameDir == nil || !*r.SameDir {
		t.Fatalf("the second refresh: %+v, %v; want current, revision 1, sameDir true", r, err)
	}
	// A client that attaches now reads the list and its revision in the
	// info document.
	late, _ := started(t, h, newTap(t), remote.SessionOptions{})
	if info := late.Info(); info.CatalogRevision != 1 || !reflect.DeepEqual(info.Models, want.Models) {
		t.Fatalf("a later attach's info: revision %d, %+v; want revision 1, %+v", info.CatalogRevision, info.Models, want.Models)
	}
}

// TestAnUnsupportedRefusalIsUnsupported (plan 034 §3.4: the unknown-method
// handling kept as a backstop): a host that refuses the method although the
// document the client holds said modelsRefresh — here an ACP session's host,
// its attach reply rewritten to say so, as a document from before a reconnect
// reached another host would — answers unsupported to the caller, as in
// process, with no error. Negative control: a RefreshModels that hands up the
// host's refusal fails the caller.
func TestAnUnsupportedRefusalIsUnsupported(t *testing.T) {
	h := newHost(t)
	tp := newTap(t)
	rewriteAttachReplies(t, tp, func(si *protocol.SessionInfo) { si.Capabilities.ModelsRefresh = true })
	s, _ := started(t, h, tp, remote.SessionOptions{})
	if !s.Info().ModelsRefresh {
		t.Fatal("the rewritten document does not say modelsRefresh")
	}
	r, err := s.RefreshModels(tctx(t), "")
	if err != nil || r.Status != agent.ModelsUnsupported {
		t.Fatalf("a refused refresh: %+v, %v; want unsupported and no error", r, err)
	}
	if n := sentMethod(tp, protocol.MethodModelsRefresh); n != 1 {
		t.Fatalf("the client wrote %d session.models.refresh requests, want the one the host refused", n)
	}
}

// TestAReconnectNeverRestoresAnOlderCatalog (plan 034 A25: reconnect and
// replay never restore an older revision): a client folds what Read hands up
// into a transcript model, as the TUI does. It has folded revision 1 when it
// loses its connection; the host takes up revisions 2 and 3 while it is away,
// and the cursor re-attach's replay hands both up — here with their bodies
// swapped on the way in, so revision 3's delta comes first and revision 2's
// after it: a replay that hands an older section after a newer one. The fold
// ends at revision 3's list, never showing revision 2's (with the model
// revision 3 took away back in it), and Info — the first attach's document,
// revision 0 — is never newer than the fold, so the mirror's newer-by-
// revision rule shows the fold's. Negative control: a fold that applies every
// catalog section it is handed (no revision guard) ends at revision 2.
func TestAReconnectNeverRestoresAnOlderCatalog(t *testing.T) {
	h := newHost(t, withRefreshing("/n"))
	tp := newTap(t)
	t.Cleanup(tp.releaseDials)
	s, first := started(t, h, tp, remote.SessionOptions{})
	fold := transcript.Restore(first.Snapshot, transcript.Options{})
	foldItem := func(it backend.Item) {
		t.Helper()
		if it.Kind != backend.ItemEvent {
			t.Fatalf("want an event; got %s", describeItem(it))
		}
		fold.Fold(it.Event)
	}
	if r := hostRefresh(t, h, modelList("a", "gone")); r.Status != agent.ModelsApplied || r.Revision != 1 {
		t.Fatalf("revision 1: %+v", r)
	}
	foldItem(readItem(t, s))
	if c := fold.State().Settings.Catalog; c == nil || c.Revision != 1 {
		t.Fatalf("the fold's revision 1: %+v", c)
	}

	rev2, rev3 := modelList("a", "gone", "b"), modelList("a", "b")
	swapped := map[uint64]*agent.CatalogState{2: {Models: rev3, Revision: 3}, 3: {Models: rev2, Revision: 2}}
	tp.setRewriteIn(func(l wireLine) [][]byte {
		if l.method != protocol.NotifyEvent {
			return nil
		}
		var p protocol.EventParams
		if err := json.Unmarshal(l.params, &p); err != nil {
			t.Errorf("an event notification: %v", err)
			return nil
		}
		ev, err := agent.DecodeEvent(string(p.Event))
		if err != nil || ev.State == nil || ev.State.Catalog == nil || swapped[ev.State.Catalog.Revision] == nil {
			return nil
		}
		ev.State.Catalog = swapped[ev.State.Catalog.Revision]
		body, err := agent.EncodeEvent(ev)
		if err != nil {
			t.Errorf("re-encoding: %v", err)
			return nil
		}
		p.Event = json.RawMessage(body)
		params, _ := json.Marshal(p)
		line, _ := json.Marshal(protocol.Notification{JSONRPC: protocol.JSONRPCVersion, Method: protocol.NotifyEvent, Params: params})
		return [][]byte{line}
	})
	held := tp.holdDials()
	tp.kill()
	await(t, held, "the redial")
	if r := hostRefresh(t, h, rev2); r.Revision != 2 {
		t.Fatalf("revision 2: %+v", r)
	}
	if r := hostRefresh(t, h, rev3); r.Revision != 3 {
		t.Fatalf("revision 3: %+v", r)
	}
	tp.releaseDials()

	for i := range 2 {
		foldItem(readItem(t, s))
		c := fold.State().Settings.Catalog
		if c == nil || c.Revision != 3 || !reflect.DeepEqual(c.Models, rev3) {
			t.Fatalf("after the replay's item %d the fold holds %+v; want revision 3's %v", i+1, c, rev3)
		}
		if info := s.Info(); info.CatalogRevision > c.Revision {
			t.Fatalf("Info's revision %d is newer than the fold's %d", info.CatalogRevision, c.Revision)
		}
	}
}
