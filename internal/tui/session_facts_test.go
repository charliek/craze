package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/host"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// The client gaps plan 030 §3.7 closes, which every TUI meets now that it is
// a detached host's client (SD-33): the permission chip and the elapsed time
// from the host's own word (SF-60, SF-63), the workspace the adopted session
// runs in, and the host status a Backend client — which is no viewer —
// reports. The last turn's ending after a restore is lastturn_test.go's.

// dialHost is a remote.Session to h naming ws as its workspace — as the
// launch flow's and craze attach's dialers name the registry entry's — and
// attaching once the host's start has run, closed when the test ends.
func dialHost(t *testing.T, h *attachHost, ws string) *remote.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	s, err := remote.DialSession(ctx, h.path, remote.SessionOptions{
		Client:    remote.Options{Client: protocol.ClientInfo{Kind: "test", Name: "tui_test"}},
		Workspace: ws,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// upOverTheSocket is a rig over a TUI built from cfg (sized, its gate
// asynchronous) whose start has answered and whose attach's restore is
// applied — the session up, as a launch or an attach has it — with the read
// of the last ending that restore sent held (r.lastTurns). clock, when set,
// is the model's clock from before its start.
func upOverTheSocket(t *testing.T, cfg Config, clock func() time.Time) *gateRig {
	t.Helper()
	isolateSkillsHome(t)
	tm, _ := New(cfg).Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m := asyncGate(t, tm.(Model))
	if clock != nil {
		m.clock = clock
	}
	r := newGateRig(t, m)
	started := runWatched(t, r.m.startCmd())
	if _, ok := started.(startedMsg); !ok {
		t.Fatalf("the start answered %#v", started)
	}
	restore, ok := r.nextStreamMsg().(restoreMsg)
	if !ok {
		t.Fatal("the stream's first item is not the attach's restore")
	}
	r.send(restore)
	r.send(started)
	if !r.m.sessionReady() || len(r.lastTurns) != 1 {
		t.Fatalf("the session is not up (ready %v), or its restore sent %d reads of the last ending, want 1", r.m.sessionReady(), len(r.lastTurns))
	}
	return r
}

// TestTheChipIsTheHostsPermissionMode (plan 030 §3.7, SF-60, AC7): over the
// socket the permission chip says what the host spawned its agent with —
// its --force or --no-force, whatever this craze's own command line said —
// and for a host that does not say (one from before plan 030: the info
// document has no permissionMode) it is the TUI's own config, as it always
// was. Before the attach reply names the host's, the config's too.
func TestTheChipIsTheHostsPermissionMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		host protocol.PermissionMode
		yolo bool
		want string
	}{
		{"a --force host, a --no-force client", protocol.PermissionBypass, false, chipYolo},
		{"a --no-force host, a --force client", protocol.PermissionPrompt, true, chipPrompt},
		{"a --force host, a --force client", protocol.PermissionBypass, true, chipYolo},
		{"a --no-force host, a --no-force client", protocol.PermissionPrompt, false, chipPrompt},
		{"an older host, a --force client", "", true, chipYolo},
		{"an older host, a --no-force client", "", false, chipPrompt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := NewStubNoPrimary()
			h := newAttachHostWith(t, stub, stub, true, control.Options{Workspace: "/work", PermissionMode: tc.host})
			ws := frameWorkspace(t)
			cfg := Config{Backend: dialHost(t, h, ws), Theme: "tokyo-night", Workspace: ws, Yolo: tc.yolo}
			own := chipPrompt
			if tc.yolo {
				own = chipYolo
			}
			isolateSkillsHome(t)
			if got, _ := New(cfg).permissionChip(); got != own {
				t.Fatalf("before the attach reply the chip reads %q, want the config's %q", got, own)
			}
			r := upOverTheSocket(t, cfg, nil)
			if got, _ := r.m.permissionChip(); got != tc.want {
				t.Fatalf("the chip reads %q, want %q", got, tc.want)
			}
			if v := plainView(r.m); !strings.Contains(v, tc.want) {
				t.Fatalf("the frame does not show %q:\n%s", tc.want, v)
			}
		})
	}
}

// TestTheSocketHarnessServesThePlan030Facts: the socket goldens' host serves
// the info document a plan 030 host does — the permission mode of the
// builder's Config.Yolo, and its start — so every golden's chip and elapsed
// read over the socket from the host's word, byte-identical with the
// in-process runs' own.
func TestTheSocketHarnessServesThePlan030Facts(t *testing.T) {
	for _, yolo := range []bool{true, false} {
		before := time.Now()
		fh, err := buildSocketHost(Config{Session: NewStubNoPrimary(), Workspace: frameWorkspace(t), Yolo: yolo})
		if err != nil {
			t.Fatal(err)
		}
		info := fh.cfg.Backend.Info()
		want := backend.PermissionPrompt
		if yolo {
			want = backend.PermissionBypass
		}
		if info.PermissionMode != want || info.StartedAt.Before(before.Add(-time.Second)) || info.StartedAt.After(time.Now()) {
			t.Errorf("yolo %v: the harness host says %q, started %s; want %q, started just now", yolo, info.PermissionMode, info.StartedAt, want)
		}
		if err := fh.end(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestElapsedCountsFromTheHostsStart (plan 030 §3.7, SF-63, AC7): a client
// that attaches to a session its host has served for an hour and seven
// minutes shows 1h07m, not the 0m of its own attach; a host that does not
// say when it started (one from before plan 030) is counted from the attach,
// as it always was.
func TestElapsedCountsFromTheHostsStart(t *testing.T) {
	t0 := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		started time.Time
		// at is the client's clock when its session comes up, and later its
		// clock once the row is read.
		at, later time.Time
		want      string
	}{
		{"the host says when it started", t0, t0.Add(67 * time.Minute), t0.Add(67 * time.Minute), "1h07m"},
		{"an older host: from the attach", time.Time{}, t0.Add(67 * time.Minute), t0.Add(70 * time.Minute), "3m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := NewStubNoPrimary()
			h := newAttachHostWith(t, stub, stub, true, control.Options{Workspace: "/work", StartedAt: tc.started})
			ws := frameWorkspace(t)
			now := tc.at
			r := upOverTheSocket(t, Config{Backend: dialHost(t, h, ws), Theme: "tokyo-night", Workspace: ws, Yolo: true}, func() time.Time { return now })
			now = tc.later
			if got := r.m.sessionElapsed(); got != tc.want {
				t.Fatalf("elapsed %q, want %q", got, tc.want)
			}
			if row := statusText(r.m.statusRow1()); !strings.HasSuffix(strings.TrimRight(row, " "), "│ "+tc.want) {
				t.Fatalf("status row 1 %q does not end with the elapsed %q", row, tc.want)
			}
		})
	}
}

// TestTheWorkspaceFollowsTheAdoptedSession (plan 030 §3.7, AC7): a session
// served elsewhere runs where its host runs it, which need not be the
// directory the TUI was started in. On adopting it, the TUI's workspace is
// the session's — the status row's name, the branch read from the
// repository there, the skills scanned under it, and where the composer's
// shell runs — for a Backend client that is no viewer, a Config.Backend's
// and a launch's alike. In process the workspace is the TUI's own, as ever.
func TestTheWorkspaceFollowsTheAdoptedSession(t *testing.T) {
	configured := frameWorkspace(t)
	session := filepath.Join(t.TempDir(), "proj")
	writeRepo(t, session, "ref: refs/heads/feature-x\n")
	writeSkillMD(t, filepath.Join(session, ".cursor", "skills", "hosted", "SKILL.md"),
		"---\nname: hosted\ndescription: a skill of the session's workspace\n---\n")
	check := func(t *testing.T, m Model) {
		t.Helper()
		if m.cwd != session || m.branch != "feature-x" || m.shellDir() != session {
			t.Fatalf("workspace %q, branch %q, shell in %q; want the session's %q on feature-x", m.cwd, m.branch, m.shellDir(), session)
		}
	}

	t.Run("over the socket", func(t *testing.T) {
		stub := NewStubNoPrimary()
		h := newAttachHostWith(t, stub, stub, true, control.Options{Workspace: session})
		cfg := Config{Backend: dialHost(t, h, session), Theme: "tokyo-night", Workspace: configured, Yolo: true}
		isolateSkillsHome(t)
		check(t, New(cfg))
		r := upOverTheSocket(t, cfg, nil)
		check(t, r.m)
		if _, ok := catalogByName(r.m, "hosted"); !ok {
			t.Fatal("the skills were not scanned under the session's workspace")
		}
		if row := statusText(r.m.statusRow1()); !strings.HasPrefix(row, "proj │ feature-x │ ") {
			t.Fatalf("status row 1 %q does not name the session's workspace and branch", row)
		}
	})

	t.Run("a launch's spawned backend", func(t *testing.T) {
		isolateSkillsHome(t)
		stub := NewStub()
		eng, err := engine.New(stub, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = eng.Close() })
		inner := newEngineBackend(eng, configured)
		info := inner.Info()
		info.Workspace = session
		b := &laterInfo{Backend: inner, eng: eng, info: info}
		m := sizedModel(Config{
			Theme: "tokyo-night", Workspace: configured, Yolo: true, ProviderLocked: true,
			NewBackend: func(agent.Provider, bool) (backend.Backend, error) { return b, nil },
		})
		if m.cwd != configured {
			t.Fatalf("before the spawn the workspace is %q, want the configured %q", m.cwd, configured)
		}
		tm, _ := m.Update(spawnedBy(t, m.Init()))
		check(t, tm.(Model))
	})

	t.Run("in process", func(t *testing.T) {
		isolateSkillsHome(t)
		stub := NewStub()
		t.Cleanup(func() { _ = stub.Close() })
		m := New(Config{Session: stub, Theme: "tokyo-night", Workspace: configured, Yolo: true})
		if m.cwd != configured || m.shellDir() != configured || m.branch != "" {
			t.Fatalf("in process the workspace is %q (shell %q, branch %q), want the configured %q", m.cwd, m.shellDir(), m.branch, configured)
		}
	})
}

// TestABackendClientReportsItsSessionsStatus (plan 030 §3.7): the launching
// TUI is its detached host's client, and no viewer — so it keeps Config.Host
// and reports to roost or herdr for the session it shows, which the host
// itself never does: the session's ready idle, and a failed last turn read
// after the restore as the failure it is. A viewer (craze attach) reports
// nothing: its Host is cleared, as ever.
func TestABackendClientReportsItsSessionsStatus(t *testing.T) {
	for _, viewer := range []bool{false, true} {
		name := "a Backend client"
		if viewer {
			name = "a viewer"
		}
		t.Run(name, func(t *testing.T) {
			h, _ := hostWithAFailedTurn(t, control.Options{Workspace: "/work"})
			rec := &recHost{}
			ws := frameWorkspace(t)
			r := upOverTheSocket(t, Config{Backend: dialHost(t, h, ws), Viewer: viewer, Host: rec, Theme: "tokyo-night", Workspace: ws, Yolo: true}, nil)
			r.send(runWatched(t, r.lastTurns[0]))
			if viewer {
				if r.m.host != nil || len(rec.statuses) != 0 {
					t.Fatalf("a viewer reported %s", fmtStatuses(rec.statuses))
				}
				return
			}
			if len(rec.statuses) == 0 {
				t.Fatal("the Backend client reported nothing")
			}
			last := rec.statuses[len(rec.statuses)-1]
			if last.Kind != host.Failed || last.Message != errTurnFailed.Error() || last.SessionID == "" || last.SessionID != r.m.snap.SessionID {
				t.Fatalf("the last status %+v, want the session's failure %q (all %s)", last, errTurnFailed, fmtStatuses(rec.statuses))
			}
		})
	}
}
