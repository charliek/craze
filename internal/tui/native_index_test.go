package tui

import (
	"context"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/sessions"
)

// Native sessions are indexed (plan 028 §3.5): whether a provider's sessions
// go into sessions.jsonl is Resumable's question, which the TUI hands the
// engine as IndexOptions.Unindexed (unindexedProvider), and no longer
// Hidden's. Native is where the two part: hidden until it is listed (C19), and
// resumable from H7's PR 1.

// TestNativeSessionIsIndexed is A9 on the engine rig: a real native session
// under the real engine, whose index is fed the TUI's own predicate, writes its
// row at the first prompt — the native provider, the harness's session id,
// which is the id its transcript is filed under, the workspace, the engine's
// craze id and the prompt as its title — and the turn's end touches it.
func TestNativeSessionIsIndexed(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	harnessHome := t.TempDir()
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.steps = [][]fantasy.StreamPart{cat(nativeTextParts("hi there"), nativeFinishParts())}
	sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()}, nativeSessionTweak(harnessHome, nativeOneModelTable(), model))
	idx := &fakeIndex{}
	e, err := engine.New(sess, engine.Options{Index: engine.IndexOptions{
		Store:     idx,
		CWD:       ws,
		Provider:  agent.NativeProvider().Name(),
		Unindexed: unindexedProvider,
		TitleLine: indexTitleLine,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.Submit(engine.Command{}, "fix the notes", engine.SubmitQueue, ""); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	st := e.State()
	want := sessions.Row{
		SessionID: st.SessionID, Provider: "native", CWD: ws, CrazeID: st.CrazeSessionID,
		Title: "fix the notes", TitleKind: sessions.TitleKindFallback,
	}
	if got := idx.seedRow(t); got != want || st.SessionID == "" || st.CrazeSessionID == "" {
		t.Fatalf("the first prompt's row is %+v, want %+v", got, want)
	}
	// The turn's end touches the row, by which time the turn's output has
	// filed the transcript under the row's session id.
	waitRows(t, idx, 2)
	if got := idx.last(); got.SessionID != st.SessionID || got.Provider != "native" || got.TitleKind != sessions.TitleKindNone {
		t.Fatalf("the turn's end wrote %+v, want a touch of the native row", got)
	}
	stored, err := filepath.Glob(filepath.Join(harnessHome, "sessions", "*", "*_"+st.SessionID+".jsonl"))
	if err != nil || len(stored) != 1 {
		t.Fatalf("the row's session id names %v (%v); want its one transcript", stored, err)
	}
}

// TestResumableSplitsFromHidden is A9's other half: Hidden and Resumable are two
// questions. The planted hidden provider is neither listed nor resumable, so
// its sessions stay out of the index and its rows out of the picker; native is
// hidden and resumable, so its sessions are indexed and its rows offered.
// Hidden keeps its own meaning for both — never persisted as the default —
// and every listed provider is resumable.
func TestResumableSplitsFromHidden(t *testing.T) {
	planted := plantHidden(t)
	native := agent.NativeProvider()
	for _, tc := range []struct {
		p                 agent.Provider
		hidden, resumable bool
	}{
		{agent.CursorProvider(), false, true},
		{agent.GrokProvider(), false, true},
		{agent.GxProvider(), false, true},
		{native, true, true},
		{planted, true, false},
	} {
		name := tc.p.Name()
		if tc.p.Hidden() != tc.hidden || hiddenProvider(name) != tc.hidden {
			t.Fatalf("%s: Hidden %v, hiddenProvider %v; want %v", name, tc.p.Hidden(), hiddenProvider(name), tc.hidden)
		}
		if tc.p.Resumable() != tc.resumable || unindexedProvider(name) == tc.resumable {
			t.Fatalf("%s: Resumable %v, unindexedProvider %v; want resumable %v", name, tc.p.Resumable(), unindexedProvider(name), tc.resumable)
		}
	}

	// The picker offers the native row and drops the planted one.
	rows := resumeRows([]sessions.Row{
		{SessionID: "n-1", Provider: native.Name()},
		{SessionID: "h-1", Provider: planted.Name()},
	})
	if len(rows) != 1 || rows[0].SessionID != "n-1" {
		t.Fatalf("the picker offers %+v, want the native row alone", rows)
	}

	// And the index, through the TUI's own engine: the first prompt of a
	// native session writes its row; the planted provider's writes nothing,
	// and says nothing about it.
	for _, tc := range []struct {
		p       agent.Provider
		indexed bool
	}{{native, true}, {planted, false}} {
		t.Run(tc.p.Name(), func(t *testing.T) {
			m, stub, idx := shellCtxModel(t, tc.p)
			t.Cleanup(func() { _ = stub.Close() })
			m = typeEnter(t, m, "the first prompt")
			if !tc.indexed {
				if n := idx.tries(); n != 0 {
					t.Fatalf("the planted provider's session was indexed: %+v", idx.all())
				}
				if got := texts(m, entryError); len(got) != 0 {
					t.Fatalf("skipping the index is not an error: %q", got)
				}
				return
			}
			if got := idx.seedRow(t); got.Provider != tc.p.Name() || got.SessionID != m.snap.SessionID || got.Title != "the first prompt" {
				t.Fatalf("the native row is %+v", got)
			}
		})
	}
}

// TestNativeLoadSeedsTitleAndPin is A8's index half (plan 028 §3.4, §3.5; the
// adapter's half is internal/agent's test of the same name): a native session
// loaded from its row, now that native rows exist, is indexed like any
// resumable provider's, so /rename persists to that very row — its session id,
// the native provider, the row's craze id, the user's title, pinned.
func TestNativeLoadSeedsTitleAndPin(t *testing.T) {
	isolateSkillsHome(t)
	ws := t.TempDir()
	table := nativeOneModelTable()
	harnessHome := t.TempDir()
	model := &nativeScriptedModel{provider: "test", wire: "wire-echo"}
	model.steps = [][]fantasy.StreamPart{cat(nativeTextParts("hi there"), nativeFinishParts())}
	first := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir()}, nativeSessionTweak(harnessHome, table, model))
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := first.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("the stored turn: %v", err)
	}
	id := first.Snapshot().SessionID
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sess := agent.NewNative(agent.Options{Workspace: ws, ContentHome: t.TempDir(), LoadSessionID: id, Title: "hello"},
		nativeSessionTweak(harnessHome, table, model))
	// Started before the model is built, as TestNativeSessionDoesNotPersistOrIndex
	// starts its session: the replay is short, so it is all buffered for the
	// drain below.
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("the load: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	idx := &fakeIndex{}
	m := New(Config{
		Session:        sess,
		Theme:          "tokyo-night",
		Workspace:      ws,
		Yolo:           true,
		ProviderLocked: true,
		Loading:        true,
		SessionIndex:   idx,
		CrazeSessionID: "018f-the-row",
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	drainSessionEvents(t, &m, sess)
	if m.replaying || m.snap.Title != "hello" {
		t.Fatalf("after the load: replaying %v, title %q; want the restored session, titled by its row", m.replaying, m.snap.Title)
	}

	m = runSlash(t, m, "/rename a user title")
	var renamed []sessions.Row
	for _, row := range idx.all() {
		if row.TitleKind == sessions.TitleKindUser {
			renamed = append(renamed, row)
		}
	}
	want := sessions.Row{SessionID: id, Provider: "native", CWD: ws, CrazeID: "018f-the-row", Title: "a user title", TitleKind: sessions.TitleKindUser}
	if len(renamed) != 1 || renamed[0] != want {
		t.Fatalf("/rename wrote %+v, want the one pinned row %+v", renamed, want)
	}
	if got := m.snap.Title; got != "a user title" {
		t.Fatalf("the composer's title is %q after /rename", got)
	}
}
