package control_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/transcript"
	"github.com/charliek/craze/internal/tui"
)

// withSlash builds the host over the Stub as a session that publishes the
// slash catalog (tui.Stub.PublishSlash; plan 043 C2): its install, and so
// every attach's snapshot, carries the catalog.
func withSlash() hostOpt {
	return withSession(func(s *tui.Stub) agent.Session {
		s.InstallOnStart = true
		s.PublishSlash = true
		return s
	})
}

// TestTheInfoDocumentSaysSlashCatalogWhereTheSessionPublishesIt (plan 043 P2):
// the session capability slashCatalog is the session's own — the server reads
// it from the engine (engine.PublishesSlashCatalog), as it reads aside — so a
// session that publishes no catalog, the plain Stub, has no slashCatalog in
// its info document, its attach reply's or its sessions.list row's, and its
// snapshot no settings.slash; one that does says slashCatalog: true in both,
// and its attach snapshot carries the catalog it installed. Negative control:
// a server that never sets the capability fails the second half.
func TestTheInfoDocumentSaysSlashCatalogWhereTheSessionPublishesIt(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	a.sayHello(nil)
	r := a.attach(attachParams(h))
	if r.Session.Capabilities.SlashCatalog {
		t.Fatalf("a session that publishes no catalog says slashCatalog: %+v", r.Session.Capabilities)
	}
	if bytes.Contains(r.Snapshot, []byte(`"slash"`)) {
		t.Fatalf("a session that publishes no catalog has one in its snapshot: %s", r.Snapshot)
	}
	a.note(protocol.NotifySynchronized)
	if row := ok[protocol.SessionsListResult](t, a.call(protocol.MethodSessionsList, protocol.SessionsListParams{})).Sessions[0]; row.Capabilities.SlashCatalog {
		t.Fatalf("its sessions.list row says slashCatalog: %+v", row.Capabilities)
	}

	h = newHost(t, withSlash())
	a = h.dial()
	a.sayHello(nil)
	r = a.attach(attachParams(h))
	if !r.Session.Capabilities.SlashCatalog {
		t.Fatalf("a session that publishes the catalog does not say so: %+v", r.Session.Capabilities)
	}
	a.note(protocol.NotifySynchronized)
	if row := ok[protocol.SessionsListResult](t, a.call(protocol.MethodSessionsList, protocol.SessionsListParams{})).Sessions[0]; !row.Capabilities.SlashCatalog {
		t.Fatalf("its sessions.list row does not say slashCatalog: %+v", row.Capabilities)
	}
	snap, err := transcript.DecodeSnapshot(r.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	want := agent.SlashCatalogFor(h.stub.Snapshot(), nil).Items
	if len(want) == 0 || !reflect.DeepEqual(snap.Settings.Slash, want) {
		t.Fatalf("the attach snapshot's catalog is %+v, want the installed %+v", snap.Settings.Slash, want)
	}
}
