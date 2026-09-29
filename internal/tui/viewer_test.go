package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/sessions"
)

// The viewer (plan 027 C28, §3.15): craze attach's TUI over a session another
// craze hosts. Config.Viewer turns off what belongs to the host alone — the
// pickers and their session swaps, provider persistence, host status
// reporting, index writes — runs the composer's shell in the session's
// workspace as Info names it, and changes nothing a frame of the session
// shows. Its quit is a view close: the session goes on on its host.

// recIndex records every row the TUI would write to the session index.
type recIndex struct {
	mu   sync.Mutex
	rows []sessions.Row
}

func (r *recIndex) Upsert(row sessions.Row) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, row)
	return nil
}

func (r *recIndex) written() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rows)
}

// hostOwned is what a Config can hand a TUI that only a host may use, each
// piece recording whether it was.
type hostOwned struct {
	host    *recHost
	index   *recIndex
	built   int
	hooked  int
	claimed int
}

// withHostOwned is cfg with every host-owned piece set: provider persistence,
// a host-status hub, a session index, the engine hook, both pickers' closures
// and --resume's rows.
func withHostOwned(cfg Config, ws string) (Config, *hostOwned) {
	h := &hostOwned{host: &recHost{}, index: &recIndex{}}
	cfg.PersistProvider = true
	cfg.Host = h.host
	cfg.SessionIndex = h.index
	cfg.OnEngine = func(*engine.Engine) { h.hooked++ }
	cfg.NewSession = func(agent.Provider) agent.Session { h.built++; return NewStub() }
	cfg.LoadSession = func(agent.Provider, sessions.Row) agent.Session { h.built++; return NewStub() }
	cfg.ClaimSession = func(sessions.Row) (string, func(), error) { h.claimed++; return "018f-x", func() {}, nil }
	cfg.RefuseLoad = func(agent.Provider) error { return nil }
	cfg.Resume = []sessions.Row{{SessionID: "s-1", Provider: "grok", CWD: ws, Title: "a row", UpdatedAt: time.Now()}}
	return cfg, h
}

// grokBackend is an in-process backend over a Stub whose provider is grok,
// not started.
func grokBackend(t *testing.T, ws string) backend.Backend {
	t.Helper()
	stub := NewStub()
	stub.SetProvider(agent.GrokProvider())
	eng, err := engine.New(stub, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return newEngineBackend(eng, ws)
}

// TestAViewerLeavesTheHostItsOwn (§3.15): handed every host-owned piece, a
// viewer opens no picker, builds and claims nothing, hands no engine to the
// hook, persists no provider, reports no host status and writes no index row
// — where the same Config over the same backend without Viewer persists the
// provider and reports its status, so each switch is Viewer's. What Run is
// handed is cleared the same way (Config.viewing), the hub Run closes
// included; and Viewer with no Backend means nothing.
func TestAViewerLeavesTheHostItsOwn(t *testing.T) {
	for _, viewer := range []bool{false, true} {
		isolateSkillsHome(t)
		config := writeConfigFile(t, "")
		ws := t.TempDir()
		cfg, h := withHostOwned(Config{Backend: grokBackend(t, ws), Viewer: viewer, Theme: "tokyo-night",
			Workspace: ws, Yolo: true, Provider: agent.GrokProvider()}, ws)
		m := New(cfg)
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		m = tm.(Model)
		if m.picking() || m.dialog != dialogNone {
			t.Fatalf("viewer=%v: a picker is up (dialog %v)", viewer, m.dialog)
		}
		m = startedLikeInit(t, m)
		m = deliver(t, m, refreshSnapMsg{})
		body, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		persisted := strings.Contains(string(body), `provider = "grok"`)
		reported := len(h.host.statuses) > 0
		if viewer {
			if persisted || reported || h.index.written() != 0 || h.built != 0 || h.hooked != 0 || h.claimed != 0 {
				t.Fatalf("a viewer used what is the host's: persisted %v (%q), reported %v, index %d, built %d, hooked %d, claimed %d",
					persisted, body, reported, h.index.written(), h.built, h.hooked, h.claimed)
			}
			if !m.viewer {
				t.Fatal("the model does not know it is a viewer")
			}
		} else if !persisted || !reported {
			t.Fatalf("fixture: the host TUI over the same backend persisted %v (%q) and reported %v", persisted, body, reported)
		}
	}

	b := grokBackend(t, t.TempDir())
	cfg, _ := withHostOwned(Config{Backend: b, Viewer: true}, "/ws")
	got := cfg.viewing()
	if !got.Viewer || got.Backend != b || got.Host != nil || got.SessionIndex != nil || got.PersistProvider ||
		got.OnEngine != nil || got.NewSession != nil || got.LoadSession != nil || got.ClaimSession != nil ||
		got.RefuseLoad != nil || got.Resume != nil || got.Session != nil {
		t.Fatalf("a viewer's Config keeps something of the host's: %+v", got)
	}
	cfg.Backend = nil
	if got := cfg.viewing(); got.Viewer || got.Host == nil || !got.PersistProvider {
		t.Fatalf("Viewer with no Backend changed the Config: %+v", got)
	}
}

// TestAViewersShellRunsInTheSessionsWorkspace (§3.15): the composer's `!`
// command runs locally, in the session's workspace as the backend's Info
// names it — not the directory the TUI was configured with. Plan 030 §3.7
// ("the workspace follows the session") makes that every backend's rule, not
// a viewer's alone: the launch flow's TUI — a Backend client that is no
// viewer — runs its command where its detached host runs the session too, and
// its status row names that workspace.
func TestAViewersShellRunsInTheSessionsWorkspace(t *testing.T) {
	isolateSkillsHome(t)
	configured, session := t.TempDir(), t.TempDir()
	for dir, marker := range map[string]string{configured: "configured-marker", session: "session-marker"} {
		if err := os.WriteFile(filepath.Join(dir, marker), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, viewer := range []bool{true, false} {
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
		m := New(Config{Backend: b, Viewer: viewer, Theme: "tokyo-night", Workspace: configured, Yolo: true})
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		m = startedLikeInit(t, tm.(Model))
		m.input.SetValue("!ls")
		tm, cmd := m.Update(enter())
		m = tm.(Model)
		done, ok := runCmd(cmd).(shellDoneMsg)
		if !ok {
			t.Fatalf("viewer=%v: Enter ran no command", viewer)
		}
		want, not := "session-marker", "configured-marker"
		if !strings.Contains(done.res.out, want) || strings.Contains(done.res.out, not) {
			t.Fatalf("viewer=%v: the command ran where it listed %q, want %s's", viewer, done.res.out, want)
		}
		if m.cwd != session {
			t.Fatalf("viewer=%v: the model's workspace is %q, want the session's %q", viewer, m.cwd, session)
		}
	}
}

// startedOverTheSocket is a rig over a TUI attached to h — Viewer as given,
// every host-owned piece set — its start answered and its first restore
// applied, frozen so no counter moves between two frames.
func startedOverTheSocket(t *testing.T, h *attachHost, viewer bool, ws string) *gateRig {
	t.Helper()
	return startedOver(t, attachSession(t, h, ""), viewer, ws)
}

// startedOver is startedOverTheSocket over a session the test dialled itself
// (a remote.Session, or a test's backend wrapping one).
func startedOver(t *testing.T, s backend.Backend, viewer bool, ws string) *gateRig {
	t.Helper()
	cfg, _ := withHostOwned(Config{Backend: s, Viewer: viewer, Theme: "tokyo-night",
		Workspace: ws, Yolo: true}, ws)
	m := New(cfg)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	m.frozen = true
	r := newGateRig(t, m)
	started := runWatched(t, r.m.startCmd())
	if _, ok := started.(startedMsg); !ok {
		t.Fatalf("viewer=%v: the start answered %#v", viewer, started)
	}
	restore, ok := r.nextStreamMsg().(restoreMsg)
	if !ok {
		t.Fatalf("viewer=%v: the stream did not open with its restore", viewer)
	}
	r.send(restore)
	r.send(started)
	return r
}

// readTo lands the rig's reads until its fold holds seq.
func (r *gateRig) readTo(seq uint64) {
	r.t.Helper()
	for r.m.foldedSeq() < seq {
		r.send(r.nextStreamMsg())
	}
}

// TestAViewerDrawsWhatTheHostTUIDraws (§3.15): two TUIs attached to one host,
// one a viewer and one not, each handed every host-owned piece, draw the same
// frame at every step — attached, after a turn another client ran on the
// host, with a draft in the composer, with the help open — so Viewer moves no
// drawn fact of an attached session; the pieces it turns off draw nothing. (The
// socket goldens run the host TUI over a remote backend, without Viewer — C29.)
func TestAViewerDrawsWhatTheHostTUIDraws(t *testing.T) {
	isolateSkillsHome(t)
	writeConfigFile(t, "")
	ws := frameWorkspace(t)
	h := newAttachHost(t, true)
	full := startedOverTheSocket(t, h, false, ws)
	view := startedOverTheSocket(t, h, true, ws)
	same := func(step string) {
		t.Helper()
		a, b := full.m.View(), view.m.View()
		if a != b {
			t.Fatalf("%s: the viewer draws another frame\n--- host TUI\n%s\n--- viewer\n%s", step, plain(a), plain(b))
		}
	}
	same("attached")

	other := engine.Command{Client: h.eng.NewClientID(), ID: "1"}
	if _, err := h.eng.Submit(other, "a prompt from the host", engine.SubmitQueue, ""); err != nil {
		t.Fatal(err)
	}
	waitHost(t, "the turn's end", func() bool { return len(h.stub.Prompts()) == 1 && h.eng.State().Turn == "" })
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	seq, err := h.eng.SyncSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	full.readTo(seq)
	view.readTo(seq)
	if v := plain(view.m.View()); !strings.Contains(v, "echo: a prompt from the host") {
		t.Fatalf("fixture: the host's turn is not on screen:\n%s", v)
	}
	same("after the host's turn")

	for _, r := range []*gateRig{full, view} {
		r.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a draft of mine")})
	}
	same("a draft")
	for _, r := range []*gateRig{full, view} {
		r.send(tea.KeyMsg{Type: tea.KeyCtrlU})
		r.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/help")})
		r.send(tea.KeyMsg{Type: tea.KeyEnter})
		if r.m.dialog != dialogHelp {
			t.Fatalf("fixture: /help opened dialog %v", r.m.dialog)
		}
	}
	same("the help")
}

// TestAViewersQuitLeavesTheSessionRunning (§3.9, plan 030 §3.6): a viewer's
// quit — Ctrl+D, the key the host TUI quits with — is the explicit quit, which
// asks the host to stop the session. A host that cannot — this one serves no
// session.stop, as a TUI-hosted session's socket or an older craze's does —
// refuses it, stop_unsupported: the program quits all the same, its socket
// detaches, the run says the stop was refused (Result.StopUnsupported, the
// command line's note), and the session goes on on its host, which another
// client then attaches to and drives.
func TestAViewersQuitLeavesTheSessionRunning(t *testing.T) {
	isolateSkillsHome(t)
	h := newAttachHost(t, true)
	r := startedOverTheSocket(t, h, true, frameWorkspace(t))
	tm, cmd := r.m.gated(tea.KeyMsg{Type: tea.KeyCtrlD}, Model.update)
	r.m = tm.(Model)
	quits := namedCmds(cmd, "stopQuit")
	if len(quits) != 1 {
		t.Fatalf("Ctrl+D asked %d stops, want one", len(quits))
	}
	if _, ok := runWatched(t, quits[0]).(tea.QuitMsg); !ok {
		t.Fatal("the viewer's quit did not quit the program")
	}
	if stopped, unsupported, err := r.m.exit.outcome(); stopped || !unsupported || err != nil {
		t.Fatalf("the quit's stop: stopped %v, unsupported %v, %v; want refused stop_unsupported", stopped, unsupported, err)
	}
	if r.m.ended {
		t.Fatal("a refused stop says the session ended")
	}
	if st := h.eng.State(); st.Activity == engine.ActivityClosing {
		t.Fatalf("the viewer's quit closed the host's session: %+v", st)
	}
	again := attachSession(t, h, "")
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	if err := again.Start(ctx); err != nil {
		t.Fatalf("the session no longer takes a client: %v", err)
	}
	c := engine.Command{Client: again.ClientID(), ID: "1"}
	if _, err := again.Submit(backend.WithEpoch(ctx, again.Epoch()), c, "still here", engine.SubmitQueue, ""); err != nil {
		t.Fatalf("the session no longer takes a prompt: %v", err)
	}
	waitHost(t, "the prompt", func() bool { return len(h.stub.Prompts()) == 1 })
}

// TestAViewersQuitStopsASessionItsHostCanStop (plan 030 §3.6, decision 11):
// over a host that serves session.stop, the viewer's Ctrl+D stops the session
// — in every client, craze attach's included. The stop's receipt comes, the
// quit waits for the session's end, and quits: the run says it stopped the
// session (Result.Stopped), the host's engine has closed, and another client
// attached meanwhile is sent the session's end.
func TestAViewersQuitStopsASessionItsHostCanStop(t *testing.T) {
	isolateSkillsHome(t)
	h := newAttachHostStopping(t, true, true)
	r := startedOverTheSocket(t, h, true, frameWorkspace(t))
	other := attachSession(t, h, "")
	ctx, cancel := context.WithTimeout(context.Background(), pumpWatchdog)
	defer cancel()
	if err := other.Start(ctx); err != nil {
		t.Fatal(err)
	}
	tm, cmd := r.m.gated(tea.KeyMsg{Type: tea.KeyCtrlD}, Model.update)
	r.m = tm.(Model)
	quits := namedCmds(cmd, "stopQuit")
	if len(quits) != 1 {
		t.Fatalf("Ctrl+D asked %d stops, want one", len(quits))
	}
	if _, ok := runWatched(t, quits[0]).(tea.QuitMsg); !ok {
		t.Fatal("the viewer's quit did not quit the program")
	}
	if stopped, unsupported, err := r.m.exit.outcome(); !stopped || unsupported || err != nil {
		t.Fatalf("the quit's stop: stopped %v, unsupported %v, %v; want taken", stopped, unsupported, err)
	}
	select {
	case <-h.eng.Done():
	case <-time.After(pumpWatchdog):
		t.Fatal("the host's session did not end after the viewer's stop")
	}
	for {
		it, err := other.Read(ctx)
		if err != nil {
			t.Fatalf("the other client's stream: %v", err)
		}
		if it.Kind == backend.ItemEnd {
			if it.Err != nil {
				t.Fatalf("the other client's end: %v", it.Err)
			}
			break
		}
	}
	// A second quit while the first would still wait quits at once.
	tm, cmd = r.m.gated(tea.KeyMsg{Type: tea.KeyCtrlD}, Model.update)
	r.m = tm.(Model)
	if len(namedCmds(cmd, "stopQuit")) != 0 {
		t.Fatal("a second quit asked for a second stop")
	}
}
