package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// Starting a session from the list (plan 030 §3.13, C15), production's pieces
// end to end, as the TUI's background dispatch drives them (tui's
// runDispatch): Spawn in another workspace, Open — a connection of the
// dispatch's own — its stream drained, Start, the prompt with that
// connection's own first command id, LeaveRunning, Close. Every host is this
// test binary run as craze serve with the fake agent, and every wait is
// bounded on its own (serveStep).

// TestTheSessionListDispatchesANewSession: a new session from the list runs
// in the workspace it was given, with the permission mode of the session the
// list came from, the launch's agent binary and none of the command line's
// own session's --plan, model or provider; a prompt sent on the dispatch's
// own connection under its first command id starts a turn; left running and
// the connection closed, the host outlives the quit, and the session —
// prompted — has its index row.
func TestTheSessionListDispatchesANewSession(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	elsewhere := absDir(t.TempDir())
	f := launchFlags(t, ws)
	// The command line's own session's mode and model: not a new session's
	// from the list.
	f.plan, f.model = true, "launch-model"
	var id string
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		ref, err := cfg.Sessions.Spawn(tui.SpawnSpec{Workspace: elsewhere, Provider: agent.CursorProvider(), PermissionMode: backend.PermissionPrompt})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		b, err := cfg.Sessions.Open(ref)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for {
				if _, err := b.Read(ctx); err != nil {
					return
				}
			}
		}()
		if err := b.Start(stepCtx(t)); err != nil {
			t.Fatalf("Start: %v", err)
		}
		info := b.Info()
		id = info.CrazeSessionID
		if info.Workspace != elsewhere || info.PermissionMode != backend.PermissionPrompt {
			t.Fatalf("the new session runs in %q with permissions %q, want %q and prompt", info.Workspace, info.PermissionMode, elsewhere)
		}
		res, err := b.Submit(stepCtx(t), engine.Command{Client: b.ClientID(), ID: "1"}, "hello from the list", engine.SubmitQueue, "")
		if err != nil || res.Turn == "" {
			t.Fatalf("the prompt on the dispatch's own connection: %+v, %v", res, err)
		}
		if err := cfg.Sessions.LeaveRunning(ref); err != nil {
			t.Fatalf("LeaveRunning: %v", err)
		}
		_ = b.Close()
		cancel()
		select {
		case <-drained:
		case <-time.After(serveStep):
			t.Fatal("the drain did not end with the connection")
		}
		return tui.Result{}, nil
	})
	if err := runTUI(nil, f, hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	argv := argvOf(t, cmds, 0)
	for _, want := range []string{"--workspace=" + elsewhere, "--provider=cursor", "--no-force", "--agent-bin="} {
		if !hasArg(argv, want) {
			t.Fatalf("the host's command line %q lacks %s", argv, want)
		}
	}
	for _, flag := range []string{"--plan", "--ask", "--model=", "--continue", "--load=", "--force"} {
		if hasArg(argv, flag) {
			t.Fatalf("the host's command line %q passes %s", argv, flag)
		}
	}
	e := onlyHost(t, env)
	if e.CrazeSessionID != id || e.Workspace != elsewhere {
		t.Fatalf("after the quit %+v, want the dispatched session %s in %q still served", e, id, elsewhere)
	}
	deadline := time.Now().Add(serveStep)
	for {
		rows, err := (&sessions.Store{}).ByCrazeID()
		if err != nil {
			t.Fatal(err)
		}
		if row, ok := rows[id]; ok {
			if row.CWD != elsewhere {
				t.Fatalf("the index row's workspace %q, want %q", row.CWD, elsewhere)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the prompted session has no index row within %s", serveStep)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopEntry(t, e, cmds.pids()[0])
}

// TestANewNativeSessionFromTheListTakesNoAgentBinary: --agent-bin is the
// launch's binary for an ACP provider; a new session of a provider craze runs
// in process is spawned without it — which that provider's host would refuse
// — whatever the command line gave.
func TestANewNativeSessionFromTheListTakesNoAgentBinary(t *testing.T) {
	env, ws, cmds := launchHome(t, nil)
	native, err := agent.ProviderByName("native")
	if err != nil {
		t.Fatal(err)
	}
	var spawnErr error
	fakeRun(t, func(cfg tui.Config) (tui.Result, error) {
		_, spawnErr = cfg.Sessions.Spawn(tui.SpawnSpec{Workspace: ws, Provider: native})
		return tui.Result{}, nil
	})
	if err := runTUI(nil, launchFlags(t, ws), hostEnv{}); err != nil {
		t.Fatalf("runTUI: %v", err)
	}
	if spawnErr != nil && strings.Contains(spawnErr.Error(), "agent-bin") {
		t.Fatalf("the native spawn was refused its agent binary: %v", spawnErr)
	}
	if spawnErr != nil {
		t.Fatalf("the native spawn: %v", spawnErr)
	}
	argv := argvOf(t, cmds, 0)
	if !hasArg(argv, "--provider=native") || hasArg(argv, "--agent-bin=") {
		t.Fatalf("the native host's command line %q", argv)
	}
	// Neither opened nor left running: the quit stopped it.
	assertNoHosts(t, env)
}

// openWatch is the launch's session list as the TUI is handed it, recording
// its Open's answer (answer, once; opened closed as it is recorded) — the
// test's view of a backend the TUI never hears of.
type openWatch struct {
	tui.SessionStarter
	mu     *sync.Mutex
	answer *openAnswer
	opened chan struct{}
}

type openAnswer struct {
	b   backend.Backend
	err error
}

func (w openWatch) Open(ref roster.Ref) (backend.Backend, error) {
	b, err := w.SessionStarter.Open(ref)
	w.mu.Lock()
	if w.answer.b == nil && w.answer.err == nil {
		*w.answer = openAnswer{b: b, err: err}
		close(w.opened)
	}
	w.mu.Unlock()
	return b, err
}

// typeInto writes keys to r's terminal.
func typeInto(t *testing.T, r *ttyRun, keys string) {
	t.Helper()
	if _, err := r.ptmx.Write([]byte(keys)); err != nil {
		t.Fatal(err)
	}
}

// waitServing waits a step for n hosts in env's registry, each serving a
// session.
func waitServing(t *testing.T, env rundir.Env, n int) {
	t.Helper()
	deadline := time.Now().Add(serveStep)
	for {
		entries, err := rundir.Hosts(env)
		serving := 0
		for _, e := range entries {
			if e.CrazeSessionID != "" {
				serving++
			}
		}
		if err == nil && serving == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not %d serving hosts after %v: %+v, %v", n, serveStep, entries, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAnUnstartedSessionsSpawnAtTheQuitLeavesNoHost (C15r, astra r30-c15 3):
// the real TUI in a pty and the launch's own cleanup (finish). The list's
// input opens an unstarted session in the launch's directory (`@`, its here
// row, enter), and its first prompt's spawn — Spawn, then Open — is running
// when craze quits. The program is over before the spawn answers, so its
// answer never reaches the TUI's Update, and nothing of the TUI's closes what
// it holds: the launch's finish does. Forced at each place:
//
//   - the spawn held (its host never answers ready), and ctrl+d: finish
//     cancels it, and its host is terminated; Open is never called;
//   - Open held after its dial and attach, and ctrl+d: released by finish's
//     own cancel — finish has begun, and waits for it — Open answers the
//     backend;
//   - Open held likewise, and SIGTERM: released once the program is over and
//     its exit tail (finishRun) has swept what the TUI held, before finish
//     begins — Open answers the backend then.
//
// Each time runTUI returns with the spawned host gone — reaped, out of the
// registry — and any backend Open answered closed; the launch's own session,
// which the unstarted session's opening let go of (a view close), goes on.
func TestAnUnstartedSessionsSpawnAtTheQuitLeavesNoHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		// held is what the spawn is held at: "spawn", or Open, released
		// "after finish began" or "before finish".
		held string
		sig  syscall.Signal // 0: ctrl+d
	}{
		{name: "the spawn held, ctrl+d", held: "spawn"},
		{name: "Open held, answering after finish began, ctrl+d", held: "after finish began"},
		{name: "Open held, answering before finish, SIGTERM", held: "before finish", sig: syscall.SIGTERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ws, cmds := launchHome(t, func(n int) []string {
				if n == 2 && tc.held == "spawn" {
					return []string{cliChildReady + "=skip"}
				}
				return nil
			})
			t.Setenv("CRAZE_FAKE_SCRIPT", "echo")

			// The unstarted session's Open, once armed: dialled and attached
			// on a context of the test's own — all the launch's cancel reaches
			// is the hold — then held.
			var armed atomic.Bool
			dialled, release := make(chan struct{}), make(chan struct{})
			cause := make(chan error, 1)
			prevDial := spawnDial
			spawnDial = func(ctx context.Context, socket string, opts remote.SessionOptions) (*remote.Session, error) {
				if !armed.Load() {
					return prevDial(ctx, socket, opts)
				}
				dctx, cancel := context.WithTimeout(context.Background(), serveStep)
				defer cancel()
				s, err := prevDial(dctx, socket, opts)
				if err == nil {
					if err = s.Attach(dctx); err != nil {
						_ = s.Close()
					}
				}
				if err != nil {
					return nil, err
				}
				close(dialled)
				if tc.held == "after finish began" {
					<-ctx.Done()
					cause <- ctx.Err()
					return s, nil
				}
				select {
				case <-release:
				case <-time.After(serveStep):
				}
				return s, nil
			}
			t.Cleanup(func() { spawnDial = prevDial })

			var mu sync.Mutex
			var answer openAnswer
			opened := make(chan struct{})
			prevRun := tuiRun
			tuiRun = func(cfg tui.Config) (tui.Result, error) {
				cfg.Sessions = openWatch{SessionStarter: cfg.Sessions.(tui.SessionStarter), mu: &mu, answer: &answer, opened: opened}
				res, err := prevRun(cfg)
				if tc.held == "before finish" {
					// The program is over and finishRun has run; finish has
					// not begun (it follows this return).
					close(release)
					select {
					case <-opened:
					case <-time.After(serveStep):
						t.Errorf("Open did not answer within %v of its release", serveStep)
					}
				}
				return res, err
			}
			t.Cleanup(func() { tuiRun = prevRun })

			r := runInPTY(t, launchFlags(t, ws))
			r.see(t, " craze ─")
			first := onlyHost(t, env)
			r.see(t, statusElapsed)
			armed.Store(true)
			// ← on the empty composer: the list, its input under the rows.
			typeInto(t, r, "\x1b[D")
			r.see(t, " sessions")
			typeInto(t, r, "@")
			r.see(t, "where should it run?")
			// enter picks the first row — here, the launch's own directory —
			// and enter on that token alone opens the unstarted session.
			typeInto(t, r, "\r\r")
			r.see(t, "type the first prompt to start this session")
			typeInto(t, r, "hi\r")
			if tc.held == "spawn" {
				waitServing(t, env, 2)
			} else {
				select {
				case <-dialled:
				case <-time.After(serveStep):
					t.Fatal("the unstarted session's Open never dialled")
				}
			}

			if tc.sig != 0 {
				if err := syscall.Kill(os.Getpid(), tc.sig); err != nil {
					t.Fatal(err)
				}
			} else {
				typeInto(t, r, "\x04")
			}
			select {
			case err := <-r.done:
				if err != nil {
					t.Fatalf("runTUI: %v", err)
				}
			case <-time.After(serveStep):
				t.Fatal("craze did not quit")
			}

			// No wait: finish stopped the spawned host before runTUI
			// returned.
			pids := cmds.pids()
			if len(pids) != 2 {
				t.Fatalf("%d hosts spawned, want the launch's and the unstarted session's", len(pids))
			}
			if processAlive(pids[1]) {
				t.Fatalf("runTUI returned with the unstarted session's host %d still there (%s)", pids[1], procState(pids[1]))
			}
			if entries, err := rundir.Hosts(env); err != nil || len(entries) != 1 || entries[0].HostID != first.HostID {
				t.Fatalf("the registry after the quit: %+v, %v; want the launch's own host %s alone", entries, err, first.HostID)
			}
			select {
			case <-opened:
			default:
				if tc.held != "spawn" {
					t.Fatal("runTUI returned before the held Open answered")
				}
			}
			mu.Lock()
			got := answer
			mu.Unlock()
			switch {
			case tc.held == "spawn":
				if got.b != nil || got.err != nil {
					t.Fatalf("Open was called for a spawn the quit cancelled: %+v", got)
				}
			case got.err != nil || got.b == nil:
				t.Fatalf("the held Open answered %v, want the backend", got.err)
			default:
				ctx, cancel := context.WithTimeout(context.Background(), serveStep)
				defer cancel()
				if _, err := got.b.Read(ctx); !errors.Is(err, backend.ErrClosed) {
					t.Fatalf("the backend Open answered after the program ended: a read answered %v, want it closed", err)
				}
			}
			if tc.held == "after finish began" {
				if err := <-cause; !errors.Is(err, context.Canceled) {
					t.Fatalf("the hold ended with %v, want finish's cancel", err)
				}
			}
			stopEntry(t, first, pids[0])
		})
	}
}
