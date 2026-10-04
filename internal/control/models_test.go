package control_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control/wiretest"
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
// unknown_session. A nativeDir that is present and empty is refused the same,
// as the published schema's pattern refuses it, while one that is absent is
// none (plan 034 C5r, r12 #3): the host and its schema agree on both, the
// schema judging each request line as the host does. Negative controls: a
// check that lets a relative nativeDir through answers it; one that takes an
// empty nativeDir for none answers that.
func TestModelsRefreshParamsAreChecked(t *testing.T) {
	h := newHost(t, withRefresh("/n"))
	a := h.dial()
	a.sayHello(nil)
	refusedWith(t, a.call(protocol.MethodModelsRefresh, protocol.ModelsRefreshParams{SessionID: sid(h), NativeDir: "n"}),
		protocol.RPCInvalidParams, protocol.CodeBadRequest, protocol.ReasonBadRequest)
	for _, tc := range []struct {
		params map[string]any
		valid  bool
	}{
		{map[string]any{"sessionId": sid(h), "nativeDir": ""}, false},
		{map[string]any{"sessionId": sid(h)}, true},
	} {
		params, err := json.Marshal(tc.params)
		if err != nil {
			t.Fatal(err)
		}
		line, err := json.Marshal(protocol.Request{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(`"99"`),
			Method: protocol.MethodModelsRefresh, Params: params})
		if err != nil {
			t.Fatal(err)
		}
		if err := wiretest.Default().Request(line); (err == nil) != tc.valid {
			t.Fatalf("the schema judges %s valid=%v (%v); want %v", line, err == nil, err, tc.valid)
		}
		resp := a.call(protocol.MethodModelsRefresh, tc.params)
		if !tc.valid {
			refusedWith(t, resp, protocol.RPCInvalidParams, protocol.CodeBadRequest, protocol.ReasonBadRequest)
			continue
		}
		if got := ok[protocol.ModelsRefreshResult](t, resp); got.Status != protocol.ModelsCurrent || got.SameDir != nil {
			t.Fatalf("a refresh with no nativeDir: %+v; want current, no sameDir", got)
		}
	}
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

// lockedWriter is an io.Writer a session's diagnostics can be written to from
// any goroutine, and read back whole.
type lockedWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// nativeTableFiles is a native directory's two model-table files as craze
// writes them (modeltable.Save's shape, written by hand: this package's tests
// do not import the harness): one provider, test, whose key is the
// environment's CRAZE_CONTROL_TEST_KEY, and the shipped catalog off, so the
// list is these models alone. Each model is an alias and its display name.
func nativeTableFiles(t *testing.T, dir string, models [][2]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	providers := "version = 1\n\n[providers.test]\ndriver = \"openai-compat\"\nbase_url = \"http://127.0.0.1:9/v1\"\nenv_keys = [\"CRAZE_CONTROL_TEST_KEY\"]\n"
	var b strings.Builder
	b.WriteString("version = 1\ncatalog = false\ndefault_model = \"test/a\"\n")
	for _, m := range models {
		b.WriteString("\n[models.\"" + m[0] + "\"]\nprovider = \"test\"\nwire_model = \"wire\"\nname = \"" + m[1] + "\"\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "providers.toml"), []byte(providers), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.toml"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAHugeModelNameKeepsTheSessionAttachable (plan 034 C5r, r12 #6): a real
// native session behind the host, whose owner names a model with 9 MiB of
// text in models.toml — over the 8 MiB record limit and the 4 MiB default
// snapshot budget. The refresh that takes it up publishes its catalog section
// as an ordinary event, the name cut to 128 bytes, before its applied reply;
// a client that attaches after it, with the default budget, is answered —
// the list and the snapshot's settings.catalog bounded alike — and the cut is
// one note on the session's diagnostics that carries no part of the name.
// The control: the model is offered (under its cut name). Negative control: a
// list that is not bounded makes the catalog event an omitted record — a
// reset where the event was — and the attach after it snapshot_too_large.
func TestAHugeModelNameKeepsTheSessionAttachable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAZE_HOME", home)
	t.Setenv("CRAZE_CONTROL_TEST_KEY", "sk-control-native-0373")
	dir := filepath.Join(home, "native")
	nativeTableFiles(t, dir, [][2]string{{"test/a", "Model A"}})
	var diag lockedWriter
	h := newHost(t, withSession(func(*tui.Stub) agent.Session {
		return agent.NewNative(agent.Options{Workspace: t.TempDir(), ContentHome: t.TempDir(), Diag: &diag}, nil)
	}))
	a := h.dial()
	a.sayHello(nil)
	if r := a.attach(attachParams(h)); !r.Session.Capabilities.ModelsRefresh {
		t.Fatalf("premise: a native session without modelsRefresh: %+v", r.Session.Capabilities)
	}
	a.note(protocol.NotifySynchronized)

	huge := strings.Repeat("Huge model name ", 9<<20/16) // 9 MiB
	nativeTableFiles(t, dir, [][2]string{{"test/a", "Model A"}, {"test/huge", huge}})
	bounded := func(what string, models []agent.ModelInfo) {
		t.Helper()
		for _, m := range models {
			if m.ID != "test/huge" {
				continue
			}
			if len(m.Name) > 128 || !utf8.ValidString(m.Name) || !strings.HasPrefix(huge, strings.TrimSuffix(m.Name, "…")) {
				t.Fatalf("%s names test/huge in %d bytes; want its head, within 128", what, len(m.Name))
			}
			return
		}
		t.Fatalf("control: %s does not offer test/huge", what)
	}
	id := refreshModels(a, h, dir)
	notes, resp := a.until(id)
	if len(notes) != 1 || notes[0].Method != protocol.NotifyEvent {
		var methods []string
		for _, n := range notes {
			methods = append(methods, n.Method)
		}
		t.Fatalf("the notifications before the reply are %v; want the catalog event alone", methods)
	}
	if _, ev := eventOf(t, notes[0]); ev.State == nil || ev.State.Catalog == nil {
		t.Fatalf("the event before the reply carries no catalog: %+v", ev)
	} else {
		bounded("the catalog event", ev.State.Catalog.Models)
	}
	if got := ok[protocol.ModelsRefreshResult](t, resp); got.Status != protocol.ModelsApplied || got.SameDir == nil || !*got.SameDir {
		t.Fatalf("the refresh: %+v; want applied, sameDir true", got)
	}

	c := h.dial()
	c.sayHello(nil)
	r := c.attach(attachParams(h))
	var infos []agent.ModelInfo
	for _, m := range r.Session.Catalogs.Models {
		infos = append(infos, agent.ModelInfo{ID: m.ID, Name: m.Name, Recent: m.Recent})
	}
	bounded("the attach's info document", infos)
	snap, err := transcript.DecodeSnapshot(r.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Settings.Catalog == nil {
		t.Fatal("the attach's snapshot carries no catalog")
	}
	bounded("the attach's snapshot", snap.Settings.Catalog.Models)
	c.note(protocol.NotifySynchronized)

	said := diag.String()
	if n := strings.Count(said, "held to its bounds"); n != 1 || strings.Contains(said, "Huge model name") {
		t.Fatalf("the diagnostics say %q; want one value-free note of the cut", said)
	}
}
