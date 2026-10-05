package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/sessions"
)

// A start's own feedback (LM-2(a), plan 037 §3.5): a provider started with no
// picker, whose agent or host this process spawns, is checked off the Update
// (Config.StartCheck), and a reason the check gives is one local note while
// that start is still starting. Every other answer — ready, late, stale, a
// duplicate, one after the start failed — adds nothing, and an attach, a
// picker's choice and a viewer are never checked.

// The reason and fix the fake check answers: internal/cli's not-GUI words.
const (
	checkReason = "this craze runs outside the macOS login session (over ssh), where cursor may not reach the login keychain"
	checkFix    = "run craze from a terminal on the Mac"
	checkNote   = "cursor may not start here: " + checkReason + "; " + checkFix
)

// checkRec is a Config.StartCheck that records the providers it is asked
// about and answers reason and fix.
type checkRec struct {
	mu          sync.Mutex
	asked       []string
	reason, fix string
}

func notGUICheck() *checkRec { return &checkRec{reason: checkReason, fix: checkFix} }

func (c *checkRec) check(p agent.Provider) (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, p.Name())
	return c.reason, c.fix
}

func (c *checkRec) providers() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.asked...)
}

// startNotes is the notes the check has drawn.
func startNotes(m Model) []string {
	var out []string
	for _, n := range texts(m, entryNote) {
		if strings.Contains(n, "may not start here") {
			out = append(out, n)
		}
	}
	return out
}

// checkedModel is the in-process path's model for an explicit --provider p:
// its session built, not started, the check rec.
func checkedModel(t *testing.T, rec *checkRec, p agent.Provider) Model {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	stub.SetProvider(p)
	t.Cleanup(func() { _ = stub.Close() })
	return sizedModel(Config{Session: stub, Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true,
		Provider: p, ProviderLocked: true, StartCheck: rec.check})
}

// checkAnswer is the answer of the start check among cmd's commands, run.
func checkAnswer(t *testing.T, cmd tea.Cmd) startCheckMsg {
	t.Helper()
	msg, ok := runWatched(t, mustCmd(t, cmd, "startCheckCmd")).(startCheckMsg)
	if !ok {
		t.Fatalf("the start check answered %T", msg)
	}
	return msg
}

// noCheck fails the test when cmd holds a start check.
func noCheck(t *testing.T, cmd tea.Cmd, why string) {
	t.Helper()
	if findCmd(cmd, "startCheckCmd") != nil {
		t.Fatalf("%s: a start check was asked", why)
	}
}

// TestAStartsCheckAddsOneNote: in process, Init asks the check of the
// explicit provider beside the start; its unavailable answer, landing before
// the session is up, is one note carrying the reason and the fix, and the
// start goes on. A duplicate of the answer adds nothing, and nor does the
// session coming up.
func TestAStartsCheckAddsOneNote(t *testing.T) {
	rec := notGUICheck()
	m := checkedModel(t, rec, agent.CursorProvider())
	msg := checkAnswer(t, m.Init())
	if got := rec.providers(); !slices.Equal(got, []string{"cursor"}) {
		t.Fatalf("the check was asked of %q", got)
	}
	m = deliver(t, m, msg)
	if got := startNotes(m); !slices.Equal(got, []string{checkNote}) {
		t.Fatalf("the notes after the answer: %q, want %q", got, checkNote)
	}
	if m.started || m.startErr != nil || m.status == statusError {
		t.Fatalf("the answer changed the start: started %v, err %v", m.started, m.startErr)
	}
	m = deliver(t, m, msg)
	m = startedLikeInit(t, m)
	if got := startNotes(m); len(got) != 1 || !m.started {
		t.Fatalf("after a duplicate and the start: notes %q, started %v", got, m.started)
	}
}

// TestAReadyAnswerAddsNoNote (the negative control): an answer with nothing
// to say — ready, or any verdict but the one the check reports — is no note.
func TestAReadyAnswerAddsNoNote(t *testing.T) {
	rec := &checkRec{}
	m := checkedModel(t, rec, agent.CursorProvider())
	m = deliver(t, m, checkAnswer(t, m.Init()))
	if got := startNotes(m); len(got) != 0 || len(rec.providers()) != 1 {
		t.Fatalf("a ready answer drew %q (asked %q)", got, rec.providers())
	}
}

// TestALateOrStaleAnswerAddsNoNote: an answer that lands once the session is
// up, one for another session generation (the session replaced since), one
// for another provider, and one after the start failed — its failure row
// drawn — add nothing.
func TestALateOrStaleAnswerAddsNoNote(t *testing.T) {
	t.Run("late", func(t *testing.T) {
		m := checkedModel(t, notGUICheck(), agent.CursorProvider())
		msg := checkAnswer(t, m.Init())
		m = startedLikeInit(t, m)
		if m = deliver(t, m, msg); len(startNotes(m)) != 0 {
			t.Fatalf("a late answer drew %q", startNotes(m))
		}
	})
	t.Run("another session", func(t *testing.T) {
		m := checkedModel(t, notGUICheck(), agent.CursorProvider())
		msg := checkAnswer(t, m.Init())
		m.setSession(NewStub(), "")
		if m = deliver(t, m, msg); len(startNotes(m)) != 0 {
			t.Fatalf("an answer for a session replaced drew %q", startNotes(m))
		}
	})
	t.Run("another provider", func(t *testing.T) {
		m := checkedModel(t, notGUICheck(), agent.CursorProvider())
		msg := checkAnswer(t, m.Init())
		msg.provider, msg.label = "grok", "grok"
		if m = deliver(t, m, msg); len(startNotes(m)) != 0 {
			t.Fatalf("an answer for another provider drew %q", startNotes(m))
		}
	})
	t.Run("after the start failed", func(t *testing.T) {
		m := checkedModel(t, notGUICheck(), agent.CursorProvider())
		msg := checkAnswer(t, m.Init())
		m = deliver(t, m, errMsg{err: errors.New("acp: agent exited: exit status 1")})
		m = deliver(t, m, msg)
		entries := m.main.entries()
		if len(startNotes(m)) != 0 || len(entries) == 0 || entries[len(entries)-1].kind != entryError {
			t.Fatalf("after the failure row: notes %q", startNotes(m))
		}
	})
}

// TestResumeChecksTheProviderThatStarts: the resume picker's row starts the
// row's provider, whatever the resolved one was (here grok), so the check is
// asked of the row's — cursor — in process and in the launch flow alike.
func TestResumeChecksTheProviderThatStarts(t *testing.T) {
	row := sessions.Row{SessionID: "s-1", Provider: "cursor", CWD: "/ws", Title: "a cursor row", CrazeID: "0199-c", UpdatedAt: time.Now()}
	t.Run("in process", func(t *testing.T) {
		isolateSkillsHome(t)
		rec := notGUICheck()
		m := sizedModel(Config{Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true, Provider: agent.GrokProvider(),
			Resume: []sessions.Row{row}, StartCheck: rec.check,
			LoadSession: func(p agent.Provider, _ sessions.Row) agent.Session {
				s := NewStub()
				s.SetProvider(p)
				return s
			}})
		if m.Init() != nil && findCmd(m.Init(), "startCheckCmd") != nil {
			t.Fatal("the resume picker asked a check before a row was chosen")
		}
		tm, cmd := m.Update(enter())
		m = tm.(Model)
		msg := checkAnswer(t, cmd)
		if got := rec.providers(); !slices.Equal(got, []string{"cursor"}) {
			t.Fatalf("the check was asked of %q, want the row's provider", got)
		}
		if m = deliver(t, m, msg); !slices.Equal(startNotes(m), []string{checkNote}) {
			t.Fatalf("the notes: %q", startNotes(m))
		}
	})
	t.Run("launch", func(t *testing.T) {
		ws := t.TempDir()
		b := &spawnedHere{Backend: grokBackend(t, ws), spawned: true}
		rec := &spawnRec{answer: func() (backend.Backend, error) { return b, nil }}
		used := 0
		cfg := launchConfig(t, ws, rec, &used)
		check := notGUICheck()
		cfg.Provider, cfg.Resume, cfg.StartCheck = agent.GrokProvider(), []sessions.Row{row}, check.check
		m := sizedModel(cfg)
		tm, cmd := m.Update(enter())
		m = tm.(Model)
		tm, cmd = m.Update(spawnedBy(t, cmd))
		m = tm.(Model)
		msg := checkAnswer(t, cmd)
		if got := check.providers(); !slices.Equal(got, []string{"cursor"}) {
			t.Fatalf("the check was asked of %q, want the row's provider", got)
		}
		if m = deliver(t, m, msg); !slices.Equal(startNotes(m), []string{checkNote}) {
			t.Fatalf("the notes: %q", startNotes(m))
		}
	})
}

// spawnedHere is a launch flow backend that says whether this process spawned
// its host (hostSpawner).
type spawnedHere struct {
	backend.Backend
	spawned bool
}

func (b *spawnedHere) SpawnedHost() bool { return b.spawned }

// TestALaunchChecksOnlyAHostItSpawned: in the launch flow, Init's own spawn —
// an explicit --provider, --continue's row — is checked once its backend is
// adopted, when this process spawned its host: one note. A host attached to
// — one serving the session already, or holding it — and a backend that does
// not say are never checked, and an answer for one adds nothing.
func TestALaunchChecksOnlyAHostItSpawned(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wrap    func(backend.Backend) backend.Backend
		checked bool
	}{
		{"a host it spawned", func(b backend.Backend) backend.Backend { return &spawnedHere{Backend: b, spawned: true} }, true},
		{"a host attached to", func(b backend.Backend) backend.Backend { return &spawnedHere{Backend: b} }, false},
		{"a backend that does not say", func(b backend.Backend) backend.Backend { return b }, false},
	} {
		for _, cont := range []bool{false, true} {
			name := tc.name + ", --provider"
			if cont {
				name = tc.name + ", --continue"
			}
			t.Run(name, func(t *testing.T) {
				ws := t.TempDir()
				b := tc.wrap(grokBackend(t, ws))
				rec := &spawnRec{answer: func() (backend.Backend, error) { return b, nil }}
				used := 0
				cfg := launchConfig(t, ws, rec, &used)
				check := notGUICheck()
				cfg.ProviderLocked, cfg.StartCheck = true, check.check
				if cont {
					row := sessions.Row{SessionID: "s-2", Provider: "cursor", CWD: ws, CrazeID: "0199-d"}
					cfg.Loading, cfg.Continue = true, &row
				}
				m := sizedModel(cfg)
				noCheck(t, m.Init(), "Init, before the spawn answered")
				tm, cmd := m.Update(spawnedBy(t, m.Init()))
				m = tm.(Model)
				if !tc.checked {
					noCheck(t, cmd, "the adopt")
					if len(check.providers()) != 0 {
						t.Fatalf("the check was asked of %q", check.providers())
					}
					// An answer that reached the model anyway is nobody's.
					m = deliver(t, m, startCheckMsg{issued: m.issue(), provider: "cursor", label: "cursor", reason: checkReason, fix: checkFix})
					if len(startNotes(m)) != 0 {
						t.Fatalf("an attach drew %q", startNotes(m))
					}
					return
				}
				msg := checkAnswer(t, cmd)
				if got := check.providers(); !slices.Equal(got, []string{"cursor"}) {
					t.Fatalf("the check was asked of %q", got)
				}
				if m = deliver(t, m, msg); !slices.Equal(startNotes(m), []string{checkNote}) {
					t.Fatalf("the notes: %q", startNotes(m))
				}
			})
		}
	}
}

// TestAPickersChoiceAndAnAttachAreNotChecked: the provider picker's choice —
// in process and in the launch flow — was checked by the picker itself, a
// session served elsewhere (Config.Backend) was not spawned here, and a
// viewer's Config has no check at all: none asks it.
func TestAPickersChoiceAndAnAttachAreNotChecked(t *testing.T) {
	t.Run("the provider picker, in process", func(t *testing.T) {
		isolateSkillsHome(t)
		check := notGUICheck()
		m := sizedModel(Config{Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true, Provider: agent.CursorProvider(),
			NewSession: func(agent.Provider) agent.Session { return NewStub() }, StartCheck: check.check})
		if !m.pickingProvider {
			t.Fatal("fixture: no provider picker")
		}
		_, cmd := m.Update(enter())
		noCheck(t, cmd, "the picker's choice")
		if len(check.providers()) != 0 {
			t.Fatalf("asked %q", check.providers())
		}
	})
	t.Run("the provider picker, launch", func(t *testing.T) {
		ws := t.TempDir()
		b := &spawnedHere{Backend: grokBackend(t, ws), spawned: true}
		rec := &spawnRec{answer: func() (backend.Backend, error) { return b, nil }}
		used := 0
		cfg := launchConfig(t, ws, rec, &used)
		check := notGUICheck()
		cfg.StartCheck = check.check
		m := sizedModel(cfg)
		tm, cmd := m.Update(enter())
		m = tm.(Model)
		_, cmd = m.Update(spawnedBy(t, cmd))
		noCheck(t, cmd, "the picker's spawned choice")
	})
	t.Run("a session served elsewhere", func(t *testing.T) {
		isolateSkillsHome(t)
		ws := t.TempDir()
		check := notGUICheck()
		m := sizedModel(Config{Backend: grokBackend(t, ws), Theme: "tokyo-night", Workspace: ws, Yolo: true,
			Provider: agent.CursorProvider(), ProviderLocked: true, StartCheck: check.check})
		noCheck(t, m.Init(), "an attach")
		if m.startAsk != (startIdentity{}) {
			t.Fatalf("an attach recorded a start to check: %+v", m.startAsk)
		}
	})
	t.Run("a viewer", func(t *testing.T) {
		cfg := Config{Backend: grokBackend(t, t.TempDir()), Viewer: true, StartCheck: notGUICheck().check}
		if cfg.viewing().StartCheck != nil {
			t.Fatal("a viewer's Config keeps the start check")
		}
	})
}

// ------------------------------------------------------------ over a socket

// checkedOverTheSocket is the launch flow's TUI over a real host whose agent
// has not started — as cursor sits on a locked keychain — attached "now" (the
// launch's attach), adopted from Init's spawn of a host this process spawned:
// the gate rig, the stream's first read outstanding; the start check's answer;
// and the attach's first restore, neither applied. sess is the host's session
// (stub its Stub), whose start the test runs when it wants one.
func checkedOverTheSocket(t *testing.T, sess agent.Session, stub *Stub) (*gateRig, startCheckMsg, restoreMsg, *attachHost) {
	t.Helper()
	h := newAttachHostWith(t, sess, stub, false, control.Options{Workspace: "/work"})
	s := attachSession(t, h, protocol.WhenNow)
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	// Attached before the host's start has run, so the attach reply — the
	// first restore — is queued ahead of whatever the start sends.
	if err := s.Attach(ctx); err != nil {
		t.Fatalf("attach: %v", err)
	}
	ws := t.TempDir()
	rec := &spawnRec{answer: func() (backend.Backend, error) { return &spawnedHere{Backend: s, spawned: true}, nil }}
	used := 0
	cfg := launchConfig(t, ws, rec, &used)
	check := notGUICheck()
	cfg.ProviderLocked, cfg.StartCheck = true, check.check
	m := sizedModel(cfg)
	tm, cmd := m.Update(spawnedBy(t, m.Init()))
	warn := checkAnswer(t, cmd)
	r := newGateRig(t, tm.(Model))
	restore, ok := r.nextStreamMsg().(restoreMsg)
	if !ok {
		t.Fatal("fixture: the stream's first item is not the attach's restore")
	}
	return r, warn, restore, h
}

// TestTheWarningOutlivesTheFirstRestore: over a socket the check answers
// beside the stream's reader, so its note can land before the attach's first
// restore, whose rebuild takes every local row: that restore draws it again,
// and the two orders end with the same one note. Only the first restore does:
// a later one — the transcript reloaded — replaces it, as it replaces every
// local row.
func TestTheWarningOutlivesTheFirstRestore(t *testing.T) {
	want := []string{"local note:" + checkNote}
	t.Run("the answer, then the restore", func(t *testing.T) {
		stub := NewStubNoPrimary()
		r, warn, restore, _ := checkedOverTheSocket(t, stub, stub)
		r.send(warn)
		if got := mainRows(r.m); !slices.Equal(got, want) {
			t.Fatalf("the answer drew %q, want %q", got, want)
		}
		r.send(restore)
		if got := mainRows(r.m); !slices.Equal(got, want) {
			t.Fatalf("the first restore left %q, want %q", got, want)
		}
		again := restore
		again.gen++
		r.landed()
		r.send(again)
		if got := mainRows(r.m); !slices.Equal(got, []string{"local note:" + reloadedNote}) {
			t.Fatalf("a later restore left %q, want the reload's note alone", got)
		}
	})
	t.Run("the restore, then the answer", func(t *testing.T) {
		stub := NewStubNoPrimary()
		r, warn, restore, _ := checkedOverTheSocket(t, stub, stub)
		r.send(restore)
		if got := mainRows(r.m); len(got) != 0 {
			t.Fatalf("fixture: the first restore drew %q", got)
		}
		r.send(warn)
		if got := mainRows(r.m); !slices.Equal(got, want) {
			t.Fatalf("the answer drew %q, want %q", got, want)
		}
	})
}

// TestTheWarningIsItsStartsOnly: the note a start's check drew is drawn again
// by the first restore of that start's session alone — not by the first
// restore of a session that replaced it (another session generation).
func TestTheWarningIsItsStartsOnly(t *testing.T) {
	m := checkedModel(t, notGUICheck(), agent.CursorProvider())
	m = deliver(t, m, checkAnswer(t, m.Init()))
	if len(startNotes(m)) != 1 {
		t.Fatalf("fixture: the answer drew %q", startNotes(m))
	}
	same := restoredWith(t, m, "inc-1", 1)
	if got := startNotes(same); len(got) != 1 {
		t.Fatalf("the first restore of the start's own session left %q", got)
	}
	m.setSession(NewStub(), "")
	if got := startNotes(restoredWith(t, m, "inc-2", 1)); len(got) != 0 {
		t.Fatalf("another session's first restore drew %q", got)
	}
}

// TestAReadyRetiresTheCheck: the host's Ready — a start that came up, or one
// that failed — lands before the start's own answer over a socket, and an
// answer after it is too late: no note, whichever of startedMsg and errMsg
// follows.
func TestAReadyRetiresTheCheck(t *testing.T) {
	// readyAfter applies the stream's items after the restore up to the
	// host's Ready, which it answers unapplied.
	readyAfter := func(t *testing.T, r *gateRig) readyMsg {
		t.Helper()
		for range 64 {
			msg := r.nextStreamMsg()
			if ready, ok := msg.(readyMsg); ok {
				return ready
			}
			r.send(msg)
		}
		t.Fatal("the host's Ready never came")
		return readyMsg{}
	}
	t.Run("came up: Ready, the answer, startedMsg", func(t *testing.T) {
		stub := NewStubNoPrimary()
		r, warn, restore, h := checkedOverTheSocket(t, stub, stub)
		r.send(restore)
		if err := h.eng.Start(context.Background()); err != nil {
			t.Fatalf("the host's start: %v", err)
		}
		ready := readyAfter(t, r)
		if ready.err != nil {
			t.Fatalf("fixture: the Ready reports %v", ready.err)
		}
		r.send(ready)
		r.send(warn)
		started, ok := runWatched(t, r.m.startCmd()).(startedMsg)
		if !ok {
			t.Fatal("fixture: the start did not answer startedMsg")
		}
		r.send(started)
		if got := startNotes(r.m); len(got) != 0 || !r.m.started {
			t.Fatalf("after the Ready: notes %q, started %v", got, r.m.started)
		}
	})
	t.Run("failed: Ready, the answer, errMsg", func(t *testing.T) {
		stub := NewStubNoPrimary()
		r, warn, restore, h := checkedOverTheSocket(t, &failingStart{Stub: stub, err: errAuthFailed}, stub)
		r.send(restore)
		if err := h.eng.Start(context.Background()); !errors.Is(err, errAuthFailed) {
			t.Fatalf("fixture: the host's start answered %v", err)
		}
		ready := readyAfter(t, r)
		if ready.err == nil {
			t.Fatal("fixture: the Ready reports no failure")
		}
		r.send(ready)
		r.send(warn)
		failed, ok := runWatched(t, r.m.startCmd()).(errMsg)
		if !ok {
			t.Fatal("fixture: the start did not answer errMsg")
		}
		r.send(failed)
		if got, want := mainRows(r.m), []string{"local error:" + errAuthFailed.Error()}; !slices.Equal(got, want) {
			t.Fatalf("after the failed Ready: %q, want the failure's row alone", got)
		}
	})
}
