package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/version"
)

// A title at create (plan 043 D8, P8, P9, P11): craze new --title, with and
// without --wait, and craze serve's hidden --title, which both spawners hand
// a new session's host.

// titleCases is plan 043 §5 C4's normalization table — engine's titleCases
// (TestNormalizeTitle) and internal/hub's TestCreateTitleNormalization, input
// for input and output for output — "" a title refused.
var titleCases = []struct{ name, in, want string }{
	{"plain", "Fix the login bug", "Fix the login bug"},
	{"empty", "", ""},
	{"whitespace only", " \t\n  ", ""},
	{"control characters only", "\x00\x07\x1b", ""},
	{"padded", "  padded title  ", "padded title"},
	{"inner runs collapse", "a  b\t\tc\n\nd", "a b c d"},
	{"control characters dropped", "bell\x07 and \x00nul", "bell and nul"},
	{"an escape sequence dropped", "\x1b[31mred\x1b[0m title", "red title"},
	{"an OSC title sequence dropped", "\x1b]0;evil\x07kept", "kept"},
	{"unicode kept", "日本語のタイトル — café ✓", "日本語のタイトル — café ✓"},
	{"120 runes kept", strings.Repeat("é", 120), strings.Repeat("é", 120)},
	{"121 runes cut to 120", strings.Repeat("日", 121), strings.Repeat("日", 120)},
	{"a cut after a space trimmed", strings.Repeat("a b ", 31), strings.TrimSpace(strings.Repeat("a b ", 30))},
	{"a zero-width space only", "\u200b", ""},
	{"a zero-width space inside", "A\u200bB", "AB"},
	{"a BOM and a word joiner", "\ufeffTi\u2060tle", "Title"},
	{"bidi overrides and isolates", "\u202eevil\u202c and \u2066isolate\u2069", "evil and isolate"},
	{"invalid UTF-8 replaced", "bad\xffbyte", "bad\ufffdbyte"},
	{"only stripped runes", "\u200b\u202e\ufeff\u2066\u2069\u2060", ""},
	{"a carriage return dropped", "a\rb", "ab"},
	{"a C1 CSI dropped with its sequence", "\u009b31mred", "red"},
	{"ZWJ and ZWNJ kept", "a\u200db\u200cc", "a\u200db\u200cc"},
}

// TestTitleFlagsNormalizeAsTheHubDoes: craze serve --title and craze new
// --title (plain and --wait, which share newTitle) make of each title of the
// table exactly what the hub's session.create makes of it — the title kept,
// or a usage error (exit 2) where it comes to nothing — and craze serve takes
// the hub's output as it is (the hub hands its host the title already
// normalized). The negative control is "plain", kept as given.
func TestTitleFlagsNormalizeAsTheHubDoes(t *testing.T) {
	for _, tc := range titleCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := engine.NormalizeTitle(tc.in); got != tc.want {
				t.Fatalf("the table is engine's: NormalizeTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
			cmd, f := parseServeFlags(t, "--title="+tc.in)
			got, err := f.createTitle(cmd)
			wantUsage(t, "craze serve --title", tc.in, got, err, tc.want)
			if tc.want != "" {
				cmd, f := parseServeFlags(t, "--title="+got)
				if again, err := f.createTitle(cmd); err != nil || again != got {
					t.Fatalf("craze serve of the hub's own %q: %q, %v", got, again, err)
				}
			}
			nf := &newFlags{title: tc.in}
			ncmd := newNewCmd()
			if err := ncmd.Flags().Set("title", tc.in); err != nil {
				t.Fatal(err)
			}
			got, err = newTitle(ncmd, nf)
			wantUsage(t, "craze new --title", tc.in, got, err, tc.want)
		})
	}
	// Not given at all: no title, from either.
	cmd, f := parseServeFlags(t)
	if got, err := f.createTitle(cmd); got != "" || err != nil {
		t.Fatalf("craze serve with no --title: %q, %v", got, err)
	}
	if got, err := newTitle(newNewCmd(), &newFlags{}); got != "" || err != nil {
		t.Fatalf("craze new with no --title: %q, %v", got, err)
	}
}

// wantUsage is one flag's answer for a title of the table: the normalized
// title, or a usage error (exit 2) where want is "".
func wantUsage(t *testing.T, what, in, got string, err error, want string) {
	t.Helper()
	if want == "" {
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 || !strings.Contains(err.Error(), "--title is blank") {
			t.Fatalf("%s %q: %q, %v; want a usage error", what, in, got, err)
		}
		return
	}
	if err != nil || got != want {
		t.Fatalf("%s %q: %q, %v; want %q", what, in, got, err, want)
	}
}

// TestServeTitleIsANewSessionsAndHidden: --title beside --continue or --load
// is a usage error before anything is created — a loaded session keeps its
// row's title — and the flag is craze serve's alone, hidden as every
// spawner's flag of serve's is (cli.md documents craze new --title instead).
func TestServeTitleIsANewSessionsAndHidden(t *testing.T) {
	for _, argv := range [][]string{
		{"--title=x", "--continue"},
		{"--title=x", "--load=018f-some-id"},
	} {
		cmd, f := parseServeFlags(t, argv...)
		if _, err := f.createTitle(cmd); err == nil || !strings.Contains(err.Error(), "names a new session") {
			t.Fatalf("%v: %v, want a usage error", argv, err)
		}
	}
	serve, root := newServeCmd(), NewRootCmd()
	if fl := serve.Flags().Lookup("title"); fl == nil || !fl.Hidden || root.Flags().Lookup("title") != nil {
		t.Fatalf("--title: hidden on craze serve alone, got %+v", fl)
	}
	if fl := newNewCmd().Flags().Lookup("title"); fl == nil || fl.Hidden {
		t.Fatalf("craze new --title: visible, got %+v", fl)
	}
}

// indexRowFor is the session index's row of craze id, once it carries want as
// its title, polled within serveStep.
func indexRowFor(t *testing.T, crazeID, want string) sessions.Row {
	t.Helper()
	var row sessions.Row
	deadline := time.Now().Add(serveStep)
	for {
		rows, err := (&sessions.Store{KnownProvider: knownProvider}).ByCrazeID()
		if err == nil {
			if row = rows[crazeID]; row.Title == want {
				return row
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session %s's index row is %+v (%v) after %v, want its title %q", crazeID, row, err, serveStep, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServeWithATitleNamesItsSessionFromTheFirstRow (P8, P10): a craze serve
// given --title serves a session whose sessions.list row — read as soon as
// the host is listed, whether or not its start has finished (the seed is the
// session's construction's: TestServeWithATitleWhoseStartFails has a start
// that never does) — has the title, normalized; once started, the session
// index holds it as the user's, pinned; and a first prompt that would name an
// untitled session (its fallback) leaves it be.
func TestServeWithATitleNamesItsSessionFromTheFirstRow(t *testing.T) {
	env, ws := serveHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	// A zero-width space and a bidi override the session's scrub drops:
	// gone before the title is seeded, so the row, the index and the stored
	// title are one string (review C7).
	r := runServeIn(t, hostEnv{}, "--agent-bin", fakeAgentPath(t), "--workspace", ws, "--title", "  Fix the\u200b\tbuild\u202e ")
	e := r.waitServing(t, env, false)
	if row := listRow(t, e); row.Title != "Fix the build" {
		t.Fatalf("the first row's title is %q", row.Title)
	}
	e = r.waitServing(t, env, true)
	row := indexRowFor(t, e.CrazeSessionID, "Fix the build")
	if !row.Pinned {
		t.Fatalf("the index row is not pinned: %+v", row)
	}
	promptUnattached(t, e, "a first prompt that names nothing")
	waitLastTurn(t, e)
	if got := listRow(t, e).Title; got != "Fix the build" {
		t.Fatalf("the first prompt renamed the session %q", got)
	}
	r.sigs <- syscall.SIGTERM
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve after SIGTERM: %v", err)
	}
	if got := indexRowFor(t, e.CrazeSessionID, "Fix the build"); !got.Pinned {
		t.Fatalf("the index row after the stop: %+v", got)
	}
}

// TestServeWithATitleWhoseStartFails: a titled host whose agent cannot start
// still serves the title on its row — seeded before its engine was
// published, not by the start — and, with no session id ever learned, writes
// no index row; its stop returns (the worker's last attempt finds no id).
func TestServeWithATitleWhoseStartFails(t *testing.T) {
	env, ws := serveHome(t)
	r := runServeIn(t, hostEnv{}, "--agent-bin", "/nowhere/agent", "--workspace", ws, "--title", "Never started")
	e := r.waitServing(t, env, false)
	if row := listRow(t, e); row.Title != "Never started" {
		t.Fatalf("the failed session's row title is %q", row.Title)
	}
	r.sigs <- syscall.SIGTERM
	if err := r.result(t, serveStep); err != nil {
		t.Fatalf("craze serve after SIGTERM: %v", err)
	}
	rows, err := (&sessions.Store{KnownProvider: knownProvider}).ByCrazeID()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if row, ok := rows[e.CrazeSessionID]; ok {
		t.Fatalf("a session that never started is indexed: %+v", row)
	}
}

// TestNewTitleThroughTheHub is plan 043 T5/T6's in-process half: craze new
// --title creates its session through a real hub — a craze hub child, its
// hosts craze serve children over the fake agent — whose hello says
// createTitle; the host is handed the normalized title, and the session's
// index row carries it as the user's, pinned, over the fallback its first
// prompt would have given it. The negative control is an untitled craze new
// beside it, whose row is named by its prompt.
func TestNewTitleThroughTheHub(t *testing.T) {
	_, ws := serveHome(t)
	writeCrazeConfig(t, fmt.Sprintf("host_idle_exit = \"30s\"\n\n[agents]\ncursor = %q\n", fakeAgentPath(t)))
	env := rundir.ProcessEnv()
	hubAsChild(t, cliChildHubHosts+"=1")
	t.Cleanup(func() { stopEveryHost(t, env) })
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	// Two sessions, two rows: the fake agent's session id is its directory's
	// name, and each create gets a directory of its own.
	t.Setenv("CRAZE_FAKE_SESSION_ID", "fake-{dir}")
	stdout, stderr, code := executeErr([]string{"new", "--provider", "cursor", "-C", ws, "--title", "  Fix the\u200b\tbuild\ufeff ", "do", "it"})
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "started ") {
		t.Fatalf("craze new --title: exit %d, %q %q (hub log: %s)", code, stdout, stderr, hubLog(env)())
	}
	created := createdEntry(t, env, "")
	if row := indexRowFor(t, created.CrazeSessionID, "Fix the build"); !row.Pinned {
		t.Fatalf("the titled session's row: %+v", row)
	}
	stdout, stderr, code = executeErr([]string{"new", "--provider", "cursor", "-C", t.TempDir(), "named", "by", "its", "prompt"})
	if code != 0 || stderr != "" {
		t.Fatalf("craze new: exit %d, %q %q", code, stdout, stderr)
	}
	plain := createdEntry(t, env, created.HostID)
	if row := indexRowFor(t, plain.CrazeSessionID, "named by its prompt"); row.Pinned {
		t.Fatalf("the untitled session's row is pinned: %+v", row)
	}
}

// createdEntry is the registry entry of a host a session.create started (an
// entry carrying a requestId) other than not's, waited for within serveStep.
func createdEntry(t *testing.T, env rundir.Env, not string) rundir.Entry {
	t.Helper()
	var found rundir.Entry
	waitFor(t, "a created host's entry", func() bool {
		entries, err := rundir.Hosts(env)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if e.RequestID != "" && e.HostID != not && e.CrazeSessionID != "" {
				found = e
				return true
			}
		}
		return false
	})
	return found
}

// TestNewWaitTitlesItsSession is T6's --wait half: craze new --wait --title
// hands the host it spawns itself — no hub — the normalized title as its
// --title, the same flag the hub's create passes, and the session's index row
// holds it, pinned, over its prompt's fallback.
func TestNewWaitTitlesItsSession(t *testing.T) {
	env, ws, kids := unattendedHome(t)
	t.Setenv("CRAZE_FAKE_SCRIPT", "echo")
	stdout, stderr, code := executeErr([]string{"new", "--wait", "--provider", "cursor", "-C", ws, "--title", " Fix\u2060  the build\u2066", "do", "the", "task"})
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "echo: do the task\n---\ncompleted · ") {
		t.Fatalf("craze new --wait --title: exit %d, %q %q", code, stdout, stderr)
	}
	checkArgv(t, childArgv(t, kids.cmd(t, 0)), []string{"--title=Fix the build", "--unattended"}, nil)
	entries, err := rundir.Hosts(env)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the registry: %+v, %v", entries, err)
	}
	if row := indexRowFor(t, entries[0].CrazeSessionID, "Fix the build"); !row.Pinned {
		t.Fatalf("the --wait session's row: %+v", row)
	}
}

// TestNewTitleAgainstAnOlderHub is P11: a running hub whose create takes no
// title — an older craze's, played by the real hub given hubCreates() less
// its Title — is LacksError's words for craze new --title, exit 1, before any
// create is sent (the hub's log names none); never the hub's unknown_field,
// and never a session created untitled. The negative control: craze new with
// no title goes on to the create on that same hub, which then fails for its
// own reason (a test binary's hub starts no host), not for a title.
func TestNewTitleAgainstAnOlderHub(t *testing.T) {
	_, env := providersHubMachine(t)
	creates := hubCreates()
	creates.Title = false
	inProcessHub(t, env, creates)
	// Ensure finds the running hub first; one it would spawn is a failure of
	// this test (a test binary spawns none without hub.Command).
	prev := hub.Command
	hub.Command = func([]string) (*exec.Cmd, error) {
		t.Error("craze new spawned a hub beside the running one")
		return nil, errors.New("no hub is spawned here")
	}
	t.Cleanup(func() { hub.Command = prev })
	stdout, stderr, code := executeErr([]string{"new", "-C", t.TempDir(), "--title", "A title"})
	// The hub runs in this process: its pid is the test's.
	want := fmt.Sprintf("craze new: this hub (craze %s) cannot set a title at create; it exits once no session runs "+
		"and no client is connected, or stop it now with kill %d (no session ends)\n", version.Version, os.Getpid())
	if code != 1 || stdout != "" || stderr != want {
		t.Fatalf("craze new --title of an older hub: exit %d, stdout %q, stderr %q; want %q", code, stdout, stderr, want)
	}
	stdout, stderr, code = executeErr([]string{"new", "-C", t.TempDir(), "--json", "--title", "A title"})
	if code != 1 || !strings.Contains(stdout, `"message":"this hub (craze `+version.Version+`) cannot set a title at create`) {
		t.Fatalf("craze new --json --title of an older hub: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	_, stderr, code = executeErr([]string{"new", "-C", t.TempDir()})
	if code != 1 || strings.Contains(stderr, "title") || !strings.Contains(stderr, "cannot be started") {
		t.Fatalf("an untitled craze new on the older hub: exit %d, stderr %q", code, stderr)
	}
}

// gatedAgent is an ACP agent that never answers: it waits for one byte on the
// FIFO gate — so craze's start sits in initialize for as long as the test
// wants — and then exits 1, failing that start before session/new, and so
// before its install.
const gatedAgent = "#!/bin/sh\nhead -c 1 \"$CRAZE_TEST_START_GATE\" >/dev/null\nexit 1\n"

// TestACreatedTitleIsInAnEarlyAttachSnapshot is branch review B9, on a real
// craze serve: while a titled session's start is held before its install — an
// ACP agent that has not answered initialize, a native session whose model
// table is a FIFO nothing has written — an attach with when: "now" is handed a
// snapshot that already carries the title, because the session's first event
// said it (agent.SeedAnnouncer, engine.New). The start then fails, before any
// install, and the title stays the session's: its row still says it. The
// negative control is the untitled session over the same held start: its
// snapshot has no title and no event at all (Seq 0), so a session created
// without a title publishes nothing it did not before.
func TestACreatedTitleIsInAnEarlyAttachSnapshot(t *testing.T) {
	for _, provider := range []string{"cursor", "native"} {
		for _, title := range []string{"Fix the build", ""} {
			name := provider + "/untitled"
			if title != "" {
				name = provider + "/titled"
			}
			t.Run(name, func(t *testing.T) {
				env, ws := serveHome(t)
				gate := filepath.Join(t.TempDir(), "gate")
				argv := []string{"--workspace", ws, "--provider", provider}
				if provider == "native" {
					// native's start reads the model table first: a FIFO
					// there holds it until the test writes to it.
					dir := filepath.Join(os.Getenv("CRAZE_HOME"), "native")
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					gate = filepath.Join(dir, "models.toml")
				} else {
					agentPath := filepath.Join(t.TempDir(), "gated-agent")
					if err := os.WriteFile(agentPath, []byte(gatedAgent), 0o700); err != nil {
						t.Fatal(err)
					}
					t.Setenv("CRAZE_TEST_START_GATE", gate)
					argv = append(argv, "--agent-bin", agentPath)
				}
				if err := syscall.Mkfifo(gate, 0o600); err != nil {
					t.Fatal(err)
				}
				if title != "" {
					argv = append(argv, "--title", title)
				}
				// The gate is released once, by the test or by its cleanup
				// (review c6 3): a failure before the test releases it would
				// leave native's start parked in its read of the FIFO, which
				// no SIGTERM ends. Registered before the host starts, and again
				// once it has — cleanups run last first, so that one releases
				// the start before the host's own cleanup stops it, and the
				// stop does not wait out its start.
				var released sync.Once
				release := func(within time.Duration) error {
					err := errors.New("released already")
					released.Do(func() { err = releaseGate(gate, within) })
					return err
				}
				t.Cleanup(func() { _ = release(gateCleanupWait) })
				r := runServeIn(t, hostEnv{}, argv...)
				t.Cleanup(func() { _ = release(gateCleanupWait) })
				e := r.waitServing(t, env, false)
				s, err := remote.DialSession(stepCtx(t), e.Socket, remote.SessionOptions{
					Client:    remote.Options{PeerCheck: rundir.DialCheck(os.Geteuid())},
					SessionID: e.CrazeSessionID, Provider: e.Provider, Workspace: e.Workspace,
					When: protocol.WhenNow,
				})
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				t.Cleanup(func() { _ = s.Close() })
				if err := s.Attach(stepCtx(t)); err != nil {
					t.Fatalf("attach when now: %v", err)
				}
				it, err := s.Read(stepCtx(t))
				if err != nil || it.Kind != backend.ItemRestore || it.Snapshot == nil {
					t.Fatalf("the first item: %+v, %v; want a restore", it, err)
				}
				snap := it.Snapshot
				wantSeq := uint64(0)
				if title != "" {
					wantSeq = 1
				}
				if snap.Settings.Title != title || snap.Seq != wantSeq {
					t.Fatalf("the early snapshot's title is %q at seq %d, want %q at seq %d", snap.Settings.Title, snap.Seq, title, wantSeq)
				}
				if row := listRow(t, e); row.Title != title {
					t.Fatalf("the row's title is %q, want %q", row.Title, title)
				}

				// The start fails, before its install: the gate opens on
				// something no start can use.
				if err := release(serveStep); err != nil {
					t.Fatalf("release the start: %v", err)
				}
				for {
					it, err := s.Read(stepCtx(t))
					if err != nil {
						t.Fatalf("waiting for the failed start: %v", err)
					}
					if it.Kind == backend.ItemReady {
						if it.Err == nil {
							t.Fatalf("the held start started: %+v", it)
						}
						break
					}
				}
				if row := listRow(t, e); row.Title != title {
					t.Fatalf("after the failed start the row's title is %q, want %q", row.Title, title)
				}
				r.sigs <- syscall.SIGTERM
				if err := r.result(t, serveStep); err != nil {
					t.Fatalf("craze serve after SIGTERM: %v", err)
				}
			})
		}
	}
}

// gateCleanupWait bounds a cleanup's release of a start gate (releaseGate):
// by then the start may never have reached the gate, or its reader may be
// gone with the agent the host's stop ended.
const gateCleanupWait = 2 * time.Second

// releaseGate writes to the FIFO gate a held start reads — something no start
// can use, so it fails before its install — and closes it, once a reader has
// the FIFO open, waiting at most within for one. The open is non-blocking, so
// it can never hang: with no reader it fails ENXIO, on Linux and macOS alike,
// and is tried again every few milliseconds until within has passed.
func releaseGate(gate string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		w, err := os.OpenFile(gate, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_, werr := w.WriteString("x = [\n")
			return errors.Join(werr, w.Close())
		}
		if !errors.Is(err, syscall.ENXIO) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no start opened the gate %s within %v", gate, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
