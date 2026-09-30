package tui

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/roster"
)

// The opened session's goldens (plan 030 §3.11, §3.17): a frame script opens
// the session list, enters another session's row, and is shown that session
// in place — its replayed transcript, and the band over it. The session list
// is a fake (frameSessions) whose Open answers an in-process engine backend,
// so the frame after the switch is the same whichever transport the session
// the script started on ran over: the goldens run under both.

// frameSessions is a frame run's session list: its roster lists one other
// session, flaky's, and Open answers a new in-process session for it — a
// Stub loading a short exchange, titled, in its own workspace.
type frameSessions struct {
	ws string
	mu sync.Mutex
	// opened is every backend Open answered, for the test's own look.
	opened []backend.Backend
}

var _ Sessions = (*frameSessions)(nil)

// frameFlakyTitle is the opened session's title.
const frameFlakyTitle = "triage the flaky pty test"

func (f *frameSessions) Roster() SessionRoster {
	r := &fakeRoster{ch: make(chan roster.Snapshot, 1)}
	r.ch <- roster.Snapshot{Running: []roster.Row{
		answeredInc("flaky", "inc-flaky", frameFlakyTitle, "cursor", f.ws, 5*time.Minute, func(s *roster.Session) {
			s.LastReply = "os.NewFile(0) races the pty close."
		}),
	}}
	return r
}

func (f *frameSessions) Open(ref roster.Ref) (backend.Backend, error) {
	if ref.Host.ID != "host-flaky" {
		return nil, errors.New("frameSessions: no such session")
	}
	stub := NewStub()
	stub.AgentTitle(frameFlakyTitle)
	stub.Replay = []agent.Event{
		{Type: agent.EventUser, Text: "Why does the pty test flake under load?"},
		{Type: agent.EventText, Text: "os.NewFile(0) races the pty close in the test helper."},
	}
	eng, err := engine.New(stub, engine.HostOptions("", nil, f.ws, "cursor"))
	if err != nil {
		return nil, err
	}
	b := newEngineBackend(eng, f.ws)
	f.mu.Lock()
	f.opened = append(f.opened, b)
	f.mu.Unlock()
	return b, nil
}

// opens is how many backends Open has answered.
func (f *frameSessions) opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.opened)
}

func (f *frameSessions) Spawn(SpawnSpec) (roster.Ref, error) {
	return roster.Ref{}, errors.New("frameSessions: Spawn is PR 3's")
}
func (f *frameSessions) LeaveRunning(roster.Ref) error { return nil }
func (f *frameSessions) Stop(roster.Ref) error         { return errors.New("frameSessions: no Stop") }
func (f *frameSessions) Cancel(roster.Ref) error       { return errors.New("frameSessions: no Cancel") }

// TestFrameGoldenSessionOpened (§3.11, §3.17): ← opens the list, enter opens
// flaky's session in place, and the frame is that session: its replayed
// exchange, its workspace on the status row, and the band — its title,
// provider and directory, and the way back — at 100×30 and 80×24, under both
// transports, the spinner frozen.
func TestFrameGoldenSessionOpened(t *testing.T) {
	for _, size := range []struct {
		cols, rows int
		name       string
	}{{100, 30, "sessions-opened-100x30"}, {80, 24, "sessions-opened-80x24"}} {
		isolateSkillsHome(t)
		ws := laneWorkspace(t, "flaky")
		var lists []*frameSessions
		got, _, err := runFrameModes(t, func() Config {
			s := &frameSessions{ws: ws}
			lists = append(lists, s)
			return Config{Session: frameStub(), Theme: "tokyo-night", Workspace: frameWorkspace(t), Model: "grok", Yolo: true, Sessions: s}
		}, size.cols, size.rows,
			"<wait:idle><left><wait:text:triage the flaky><enter><wait:text:restored><wait:idle>",
			FrameOpts{Timeout: 10 * time.Second, Freeze: true})
		if err != nil {
			t.Fatalf("run frame script: %v", err)
		}
		for _, s := range lists {
			if n := s.opens(); n != 1 {
				t.Fatalf("a run opened %d sessions, want flaky's once", n)
			}
		}
		assertFrameGolden(t, size.name, size.cols, size.rows, got,
			[]string{"─ " + frameFlakyTitle + " · cursor · flaky ", bandBack + " ─",
				"Why does the pty test flake under load?", "races the pty close", "restored", "flaky │"},
			[]string{"sessions  ", "opening", "starting", "restoring", "ws │"})
	}
}
