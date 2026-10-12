package agent

import (
	"context"
	"io"
	"slices"
	"testing"
)

// A new session created with a title (plan 043 D8, P8): Options.Title and
// TitlePinned with no LoadSessionID — craze serve --title's — name and pin the
// session from its construction, before its Start and so before any engine
// over it is published; the session/new install keeps it and says it; and
// neither an agent's session_info_update nor native's first-prompt title
// replaces it.

const createdTitle = "Fix the login bug"

// installTitle is the Title section of the install delta among evs — the
// one delta with no cause that restates the mode, the config and the commands
// together (an agent's commands update, which can land on either side of it,
// carries neither the mode nor the config) — and whether it carried one at
// all.
func installTitle(t *testing.T, evs []Event) (string, bool) {
	t.Helper()
	for _, ev := range evs {
		if ev.Type == EventMeta && ev.State != nil && ev.Cause == "" &&
			ev.State.Commands != nil && ev.State.Mode != nil && ev.State.Config != nil {
			if ev.State.Title == nil {
				return "", false
			}
			return *ev.State.Title, true
		}
	}
	t.Fatalf("no install delta among the events:\n%s", formatEvents(evs))
	return "", false
}

// TestALiveSessionCreatedWithATitleIsNamedFromItsConstruction: the snapshot
// a host reads before Start — the first state and roster row — is titled;
// session/new's reply names nothing, and the install keeps the pinned title
// rather than install the reply's none, its delta carrying it; the fold a
// client builds ends on it, and agrees with the snapshot. The negative
// control: the same session built with no title starts untitled, its install
// saying so.
func TestALiveSessionCreatedWithATitleIsNamedFromItsConstruction(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
	}{{"titled", createdTitle}, {"untitled", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSession(t, Options{Binary: fakeAgentPath(t), ExtraArgs: []string{"-script=modelconfig"},
				Workspace: t.TempDir(), Stderr: io.Discard, Title: tc.title, TitlePinned: tc.title != ""})
			if got := s.Snapshot().Title; got != tc.title {
				t.Fatalf("before Start the session is named %q, want %q", got, tc.title)
			}
			if err := s.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			awaitCatalog(t, s)
			_ = s.log.Flush(context.Background(), s.done)
			evs := drainBuffered(s)
			snap := s.Snapshot()
			if snap.Title != tc.title {
				t.Fatalf("after the install the session is named %q, want %q", snap.Title, tc.title)
			}
			if got, ok := installTitle(t, evs); !ok || got != tc.title {
				t.Fatalf("the install delta's title is %q (present %v), want %q\n%s", got, ok, tc.title, formatEvents(evs))
			}
			wantFoldMatchesSnapshot(t, evs, snap)
		})
	}
}

// TestAnEarlyAgentTitleLosesToTheCreatedOne (P8): an agent's
// session_info_update that lands before the install — before session/new's
// reply (the preinstall script, flushed inside NewSession) and after it (the
// read loop's, at the install barrier) — is the window where the install
// keeps what the agent wrote (r27 finding 1). Against a created title it
// writes nothing: the pin refuses it, no delta names it, and the install and
// the fold end on the created title. The negative control is
// TestAnUpdateAppliedBeforeTheInstallSurvivesIt: with no title the same
// updates name the session.
func TestAnEarlyAgentTitleLosesToTheCreatedOne(t *testing.T) {
	check := func(t *testing.T, s *session, agentTitle string) {
		t.Helper()
		awaitCatalog(t, s)
		_ = s.log.Flush(context.Background(), s.done)
		evs := drainBuffered(s)
		if got := s.Snapshot().Title; got != createdTitle {
			t.Fatalf("the agent's early title took the session: %q", got)
		}
		for _, ev := range evs {
			if ev.Text == agentTitle || (ev.State != nil && ev.State.Title != nil && *ev.State.Title == agentTitle) {
				t.Fatalf("a delta names the agent's title:\n%s", formatEvents(evs))
			}
		}
		if got, ok := installTitle(t, evs); !ok || got != createdTitle {
			t.Fatalf("the install delta's title is %q (present %v)", got, ok)
		}
		wantFoldMatchesSnapshot(t, evs, s.Snapshot())
	}
	t.Run("before session/new's reply", func(t *testing.T) {
		s := newTestSession(t, Options{Binary: fakeAgentPath(t), ExtraArgs: []string{"-script=preinstall"},
			Workspace: t.TempDir(), Stderr: io.Discard, Title: createdTitle, TitlePinned: true})
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		check(t, s, "named before the reply")
	})
	t.Run("after session/new's reply", func(t *testing.T) {
		s := newTestSession(t, Options{Binary: fakeAgentPath(t), ExtraArgs: []string{"-script=modelconfig"},
			Workspace: t.TempDir(), Stderr: io.Discard, Title: createdTitle, TitlePinned: true})
		s.beforeInstall = func() { s.onUpdate(sessionInfoNotification("named before the install")) }
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		check(t, s, "named before the install")
	})
}

// TestARenameBeforeTheInstallSurvivesIt: a SetTitle made on the session
// itself before session/new's install pins the title, and the install keeps
// it (where it used to install the reply's none over it) and says it — the
// install's pinned-title rule, which the created title rides. No rename
// reaches the session this early through the engine, which refuses one while
// the session starts (engine's refusalLocked): this is the seam's own
// contract, called directly.
func TestARenameBeforeTheInstallSurvivesIt(t *testing.T) {
	s := newTestSession(t, Options{Binary: fakeAgentPath(t), ExtraArgs: []string{"-script=modelconfig"},
		Workspace: t.TempDir(), Stderr: io.Discard})
	s.beforeInstall = func() {
		if err := s.SetTitle("c-1/1", "renamed early"); err != nil {
			t.Errorf("SetTitle: %v", err)
		}
	}
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	awaitCatalog(t, s)
	_ = s.log.Flush(context.Background(), s.done)
	evs := drainBuffered(s)
	if got := s.Snapshot().Title; got != "renamed early" {
		t.Fatalf("the install dropped the early rename: %q", got)
	}
	if got, ok := installTitle(t, evs); !ok || got != "renamed early" {
		t.Fatalf("the install delta's title is %q (present %v)", got, ok)
	}
	wantFoldMatchesSnapshot(t, evs, s.Snapshot())
}

// TestANativeSessionCreatedWithATitleIsNamedFromItsConstruction: native's
// half (P8). The snapshot before Start is titled; the new-session install —
// which carries no Title section for an untitled session, the first prompt
// being what names one — carries the created title; and the first prompt,
// which names an untitled session, leaves it as it is and publishes no title.
// The negative control is the untitled session beside it: no Title section in
// its install, and its first prompt names it.
func TestANativeSessionCreatedWithATitleIsNamedFromItsConstruction(t *testing.T) {
	for _, tc := range []struct {
		name, title string
		wantInstall bool
		wantTitles  []string
		wantAfter   string
	}{
		{"titled", createdTitle, true, nil, createdTitle},
		{"untitled", "", false, []string{"rename me please"}, "rename me please"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeFixture(t)
			s := f.session(Options{Title: tc.title, TitlePinned: tc.title != ""})
			if got := s.Snapshot().Title; got != tc.title {
				t.Fatalf("before Start the session is named %q, want %q", got, tc.title)
			}
			if err := s.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			evs := deltaSettled(t, s)
			got, present := installTitle(t, evs)
			if present != tc.wantInstall || got != tc.title {
				t.Fatalf("the install delta's title is %q (present %v), want %q (present %v)", got, present, tc.title, tc.wantInstall)
			}
			wantFoldMatchesSnapshot(t, evs, s.Snapshot())
			f.models["test/a"].push(answer("ok"))
			if _, err := s.Prompt(context.Background(), "rename me please\nand more"); err != nil {
				t.Fatal(err)
			}
			if titles := titleDeltas(deltaSettled(t, s)); !slices.Equal(titles, tc.wantTitles) {
				t.Fatalf("the first prompt published titles %q, want %q", titles, tc.wantTitles)
			}
			if got := s.Snapshot().Title; got != tc.wantAfter {
				t.Fatalf("after the first prompt the session is named %q, want %q", got, tc.wantAfter)
			}
		})
	}
}

// TestALoadIsNotSeededAsANewSession: Options.Title on a load is the replay's
// seed, as it always was — not the constructor's — so a loaded session's
// first snapshot is untitled until its Start seeds it (loadSession,
// openReplay). The new-session seed is for a new session alone.
func TestALoadIsNotSeededAsANewSession(t *testing.T) {
	live := newTestSession(t, Options{Binary: fakeAgentPath(t), LoadSessionID: "s-1", Workspace: t.TempDir(),
		Stderr: io.Discard, Title: "a row's title", TitlePinned: true})
	native := newNativeFixture(t).session(Options{LoadSessionID: "s-1", Title: "a row's title", TitlePinned: true})
	for name, snap := range map[string]Snapshot{"live": live.Snapshot(), "native": native.Snapshot()} {
		if snap.Title != "" {
			t.Fatalf("a %s load is named %q before its Start", name, snap.Title)
		}
	}
	live.mu.Lock()
	pinned := live.titlePinned
	live.mu.Unlock()
	if pinned {
		t.Fatal("a live load is pinned before its Start")
	}
}

// TestAnnounceSeedSaysTheCreatedTitleOnce is the session's half of branch
// review B9 (SeedAnnouncer): a new session created with a title enqueues it
// as ONE title-only delta when announced — craze's own shape: no Event.Text,
// no cause, no other section — and a second announce says nothing; live and
// native alike. The negative controls: a session created with no title, and a
// load whose row has one (its replay seeds it), announce nothing and enqueue
// no event at all.
func TestAnnounceSeedSaysTheCreatedTitleOnce(t *testing.T) {
	type announcing interface {
		Session
		LogOwner
		SeedAnnouncer
	}
	build := func(t *testing.T, provider string, opts Options) announcing {
		t.Helper()
		if provider == "native" {
			return newNativeFixture(t).session(opts)
		}
		opts.Workspace, opts.Stderr = t.TempDir(), io.Discard
		return newTestSession(t, opts)
	}
	for _, provider := range []string{"live", "native"} {
		t.Run(provider, func(t *testing.T) {
			s := build(t, provider, Options{Title: createdTitle, TitlePinned: true})
			if !s.AnnounceSeed() {
				t.Fatal("a titled session announced nothing")
			}
			if s.AnnounceSeed() {
				t.Fatal("a titled session announced twice")
			}
			evs := deltaSettled(t, s)
			if len(evs) != 1 {
				t.Fatalf("the announce enqueued %d events:\n%s", len(evs), formatEvents(evs))
			}
			ev, want := evs[0], createdTitle
			if ev.Type != EventMeta || ev.Text != "" || ev.Cause != "" || ev.State == nil || ev.State.Title == nil ||
				*ev.State.Title != want || (*ev.State != StateDelta{Title: ev.State.Title}) {
				t.Fatalf("the announce is %+v (state %+v), want a title-only delta of %q", ev, ev.State, want)
			}
			for name, opts := range map[string]Options{
				"untitled": {},
				"a load":   {LoadSessionID: "s-1", Title: "a row's title", TitlePinned: true},
			} {
				quiet := build(t, provider, opts)
				if quiet.AnnounceSeed() {
					t.Fatalf("%s announced a seed", name)
				}
				if evs := deltaSettled(t, quiet); len(evs) != 0 {
					t.Fatalf("%s enqueued %d events:\n%s", name, len(evs), formatEvents(evs))
				}
			}
		})
	}
}
