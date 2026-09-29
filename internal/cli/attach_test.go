package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// craze attach (plan 027 C28, §3.15): the resolution against the registry, the
// refused flags, the exit an attach TUI's run ends in, and the dial.

func assertExit(t *testing.T, err error, wantCode int, wantMsg string) {
	t.Helper()
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("got %v (%T), want an exit error %d %q", err, err, wantCode, wantMsg)
	}
	if ee.code != wantCode || ee.msg != wantMsg {
		t.Fatalf("got exit %d %q, want %d %q", ee.code, ee.msg, wantCode, wantMsg)
	}
}

// noTitles is a resolution that must not need a listing: it fails the test
// if one is asked for.
func noTitles(t *testing.T) func() map[string]string {
	return func() map[string]string {
		t.Helper()
		t.Error("a resolution that needed no listing read the index for titles")
		return nil
	}
}

// TestAttachResolvesTheSessionInThisDirectory is §3.15's table: with no
// --session, the one live session whose workspace is the current directory is
// the target, whatever runs elsewhere; several there are exit 2 listing them
// (id, workspace, title) and the hint; none there is exit 1 naming the
// directory, then what runs elsewhere and the hint — or that first line alone
// when nothing runs anywhere. An entry not yet ready is named by its host id.
func TestAttachResolvesTheSessionInThisDirectory(t *testing.T) {
	here, there := t.TempDir(), t.TempDir()
	mine := rundir.Entry{CrazeSessionID: "craze-here", HostID: "aaaaaaaaaaaa", Provider: "cursor", Workspace: here}
	also := rundir.Entry{HostID: "bbbbbbbbbbbb", Provider: "grok", Workspace: here + "/"}
	away := rundir.Entry{CrazeSessionID: "craze-there", HostID: "cccccccccccc", Provider: "grok", Workspace: there}
	nowhere := rundir.Entry{HostID: "dddddddddddd", Provider: "cursor"}
	titles := func() map[string]string {
		return map[string]string{"craze-here": "fix the parser", "craze-there": "port the docs"}
	}

	got, err := resolveAttach([]rundir.Entry{away, mine, nowhere}, "", false, here, noTitles(t))
	if err != nil || got != mine {
		t.Fatalf("one here: %+v, %v; want %+v", got, err, mine)
	}

	_, err = resolveAttach([]rundir.Entry{mine, away, also}, "", false, here, titles)
	assertExit(t, err, 2, "craze: 2 running craze sessions in "+here+":\n"+
		"  craze-here  "+here+"  fix the parser\n"+
		"  bbbbbbbbbbbb  "+here+"/\n"+
		attachHint)

	_, err = resolveAttach([]rundir.Entry{away, nowhere}, "", false, here, titles)
	assertExit(t, err, 1, "craze: no running craze session in "+here+"\n"+
		"  craze-there  "+there+"  port the docs\n"+
		"  dddddddddddd\n"+
		attachHint)

	_, err = resolveAttach(nil, "", false, here, noTitles(t))
	assertExit(t, err, 1, "craze: no running craze session in "+here)
}

// TestAttachResolvesBySessionID: --session names an entry by its craze
// session id, its provider's session id or its host id — the bridge's
// matching (matchSession) — wherever it runs; an id nothing has is exit 1,
// `craze attach: no session <id>`; two entries one id names are exit 2,
// listed.
func TestAttachResolvesBySessionID(t *testing.T) {
	here := t.TempDir()
	target := rundir.Entry{CrazeSessionID: "craze-1", ProviderSessionID: "prov-1", HostID: "aaaaaaaaaaaa",
		Provider: "cursor", Workspace: "/somewhere/else"}
	other := rundir.Entry{CrazeSessionID: "craze-2", ProviderSessionID: "prov-2", HostID: "bbbbbbbbbbbb",
		Provider: "grok", Workspace: here}
	entries := []rundir.Entry{other, target}
	for _, id := range []string{"craze-1", "prov-1", "aaaaaaaaaaaa"} {
		got, err := resolveAttach(entries, id, true, here, noTitles(t))
		if err != nil || got != target {
			t.Fatalf("--session %s: %+v, %v; want %+v", id, got, err, target)
		}
	}
	_, err := resolveAttach(entries, "nope", true, here, noTitles(t))
	assertExit(t, err, 1, "craze attach: no session nope")

	clash := rundir.Entry{CrazeSessionID: "aaaaaaaaaaaa", HostID: "cccccccccccc", Workspace: "/c"}
	_, err = resolveAttach([]rundir.Entry{target, clash}, "aaaaaaaaaaaa", true, here,
		func() map[string]string { return map[string]string{"craze-1": "one"} })
	assertExit(t, err, 2, "craze attach: 2 sessions match --session aaaaaaaaaaaa:\n"+
		"  craze-1  /somewhere/else  one\n"+
		"  aaaaaaaaaaaa  /c")
}

// TestAttachComparesDirectoriesResolved: the registry records a host's
// workspace as it was given and the kernel reports the current directory as
// it resolves it (macOS: /tmp is /private/tmp), so both sides are compared
// absolute, cleaned and symlink-resolved — a link to the workspace is the
// workspace, from either side.
func TestAttachComparesDirectoriesResolved(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	byReal := rundir.Entry{CrazeSessionID: "by-real", HostID: "aaaaaaaaaaaa", Workspace: real}
	byLink := rundir.Entry{CrazeSessionID: "by-link", HostID: "bbbbbbbbbbbb", Workspace: link + "/./"}
	if got, err := resolveAttach([]rundir.Entry{byReal}, "", false, link, noTitles(t)); err != nil || got != byReal {
		t.Fatalf("cwd through a link: %+v, %v", got, err)
	}
	if got, err := resolveAttach([]rundir.Entry{byLink}, "", false, real, noTitles(t)); err != nil || got != byLink {
		t.Fatalf("a workspace recorded through a link: %+v, %v", got, err)
	}
	t.Chdir(link)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := resolveAttach([]rundir.Entry{byReal}, "", false, cwd, noTitles(t)); err != nil || got != byReal {
		t.Fatalf("the process's own cwd (%s): %+v, %v", cwd, got, err)
	}
}

// TestTheNoSessionWordingTitlesFromTheIndex: a listing's titles are the
// session index's rows, by craze id — any workspace, any provider, the newest
// row of an id — and a session with no row, or a row with no title, is
// listed without one. Nothing connects to a host to describe it (§3.8).
func TestTheNoSessionWordingTitlesFromTheIndex(t *testing.T) {
	indexHome(t)
	here := t.TempDir()
	seedRow(t, sessions.Row{SessionID: "s-1", Provider: "grok", CWD: "/elsewhere", CrazeID: "craze-titled",
		Title: "an older title", TitleKind: sessions.TitleKindUser}, time.Hour)
	seedRow(t, sessions.Row{SessionID: "s-2", Provider: "cursor", CWD: "/elsewhere", CrazeID: "craze-titled",
		Title: "fix the parser", TitleKind: sessions.TitleKindUser}, time.Minute)
	seedRow(t, sessions.Row{SessionID: "s-3", Provider: "cursor", CWD: "/elsewhere", CrazeID: "craze-untitled",
		TitleKind: sessions.TitleKindNone}, time.Minute)
	entries := []rundir.Entry{
		{CrazeSessionID: "craze-titled", HostID: "aaaaaaaaaaaa", Workspace: "/elsewhere"},
		{CrazeSessionID: "craze-untitled", HostID: "bbbbbbbbbbbb", Workspace: "/elsewhere"},
		{CrazeSessionID: "craze-unindexed", HostID: "cccccccccccc", Workspace: "/other"},
	}
	_, err := resolveAttach(entries, "", false, here, indexTitles)
	assertExit(t, err, 1, "craze: no running craze session in "+here+"\n"+
		"  craze-titled  /elsewhere  fix the parser\n"+
		"  craze-untitled  /elsewhere\n"+
		"  craze-unindexed  /other\n"+
		"attach to one with: craze attach --session <id>")
}

// TestAListingIsOneLinePerSession: a workspace or a title is the user's text;
// a newline in one must not read as another session.
func TestAListingIsOneLinePerSession(t *testing.T) {
	got := attachList([]rundir.Entry{{CrazeSessionID: "c-1", Workspace: "/a\nb"}},
		map[string]string{"c-1": "two\nlines"})
	if got != "  c-1  /a b  two lines" {
		t.Fatalf("listed %q", got)
	}
}

// attachArgv runs `craze attach` with argv through the real command tree,
// under an isolated HOME whose registry is empty.
func attachArgv(t *testing.T, argv ...string) error {
	t.Helper()
	indexHome(t)
	cmd := NewRootCmd()
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"attach"}, argv...))
	return cmd.Execute()
}

// TestAttachRefusesTheFlagsOfASession (§3.15): --continue, --resume,
// --provider and --model choose or start a session, and an attach joins one
// that runs — each is refused by name, a usage error (exit 2), never cobra's
// unknown-flag error; and none of them is offered in the help.
func TestAttachRefusesTheFlagsOfASession(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		flag string
	}{
		{[]string{"--continue"}, "continue"},
		{[]string{"-c"}, "continue"},
		{[]string{"--resume"}, "resume"},
		{[]string{"-r"}, "resume"},
		{[]string{"--provider", "grok"}, "provider"},
		{[]string{"--model", "gpt-5"}, "model"},
		{[]string{"--session", "craze-1", "--model", "gpt-5"}, "model"},
	} {
		err := attachArgv(t, tc.argv...)
		assertExit(t, err, 2, "craze attach: --"+tc.flag+" does not apply: attach joins a running session")
	}
	cmd := newAttachCmd()
	for _, name := range refusedAttachFlags {
		if f := cmd.Flags().Lookup(name); f == nil || !f.Hidden {
			t.Fatalf("--%s is not a hidden flag of attach: %+v", name, f)
		}
	}
	for _, name := range []string{"session", "theme", "no-mouse", "no-background"} {
		if f := cmd.Flags().Lookup(name); f == nil || f.Hidden {
			t.Fatalf("--%s is not an offered flag of attach: %+v", name, f)
		}
	}
}

// TestAttachValidatesAnExplicitSession: an explicitly passed --session is the
// bridge's token whatever its value, "" included — never "no --session" — and
// is refused before the registry is read.
func TestAttachValidatesAnExplicitSession(t *testing.T) {
	for _, id := range []string{"", "../x", "a b"} {
		err := attachArgv(t, "--session", id)
		assertExit(t, err, 2, fmt.Sprintf("craze attach: --session %q is not a valid session id", id))
	}
	err := attachArgv(t, "--session", "craze-1")
	assertExit(t, err, 1, "craze attach: no session craze-1")
}

// TestAttachWithNothingRunningSaysSo: the whole command, from the command
// line to the resolution: nothing runs anywhere, so one line, exit 1 — before
// any terminal is asked for.
func TestAttachWithNothingRunningSaysSo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	err = attachArgv(t)
	assertExit(t, err, 1, "craze: no running craze session in "+cwd)
}

// TestAnAttachEndsAsItsSessionDid is §3.9's exit table for the attach TUI: a
// view close says nothing and exits 0; the session's own end (the host quit)
// exits 0 with `craze: session ended` on stderr; an end carrying an error (the
// transport gave up) is exit 1, `craze: lost the session: <err>`; a start
// failure is the host TUI's — its own error — even when the host then quits;
// a start the stream's end cut short is that end; and the program's own
// failure (tui.Run's error that is not the start's) is that error whatever
// the stream did, never `session ended` over it. The explicit quit's stop
// (plan 030 §3.6): taken, it says nothing; refused by an older host, the note;
// answered neither way, one line.
func TestAnAttachEndsAsItsSessionDid(t *testing.T) {
	startFailed := &remote.StartError{Text: "agent auth failed"}
	cutShort := fmt.Errorf("remote: the stream ended before the session was ready: %w", errors.New("the session ended"))
	programFailed := errors.New("error reading input: read /dev/stdin: input/output error")
	for _, tc := range []struct {
		name    string
		res     tui.Result
		err     error
		code    int // 0: no error returned
		msg     string
		stderr  string
		sameErr bool
	}{
		{name: "a view close", res: tui.Result{}},
		{name: "the host quit", res: tui.Result{Ended: true}, stderr: "craze: session ended\n"},
		{name: "the transport gave up", res: tui.Result{Ended: true, EndErr: errors.New("redials spent\nafter 3")},
			code: 1, msg: "craze: lost the session: redials spent after 3"},
		{name: "a start failure", res: tui.Result{StartErr: startFailed}, err: startFailed, sameErr: true},
		{name: "a start failure, then the host quit", res: tui.Result{Ended: true, StartErr: startFailed}, err: startFailed, sameErr: true},
		{name: "a start cut short by the end", res: tui.Result{Ended: true, StartErr: cutShort}, err: cutShort, stderr: "craze: session ended\n"},
		// The program's own failure beats the stream's end (sol r66): the End
		// was applied, and then the terminal's input failed.
		{name: "the program failed after the End", res: tui.Result{Ended: true}, err: programFailed, sameErr: true},
		{name: "the program failed after an End with an error", res: tui.Result{Ended: true, EndErr: errors.New("redials spent")},
			err: programFailed, sameErr: true},
		{name: "the program failed over a start failure", res: tui.Result{StartErr: startFailed}, err: programFailed, sameErr: true},
		// The explicit quit (plan 030 §3.6): a stop taken says nothing of
		// the end it asked for; one refused by a host that cannot stop is a
		// detach, with its note; one answered neither way is a line, exit 0.
		{name: "a stop taken, the session ended", res: tui.Result{Ended: true, Stopped: true}},
		{name: "a stop taken, the quit before the end", res: tui.Result{Stopped: true}},
		{name: "a stop taken on a failed start", res: tui.Result{Ended: true, Stopped: true, StartErr: startFailed},
			err: startFailed, sameErr: true},
		{name: "a stop an older host refused", res: tui.Result{StopUnsupported: true},
			stderr: "craze: that session runs in an older craze; close it there\n"},
		{name: "a stop answered neither way", res: tui.Result{StopErr: errors.New("outcome unknown\ndisconnected")},
			stderr: "craze: the session may still be running: outcome unknown disconnected\n"},
		{name: "a stop taken, the transport gave up", res: tui.Result{Ended: true, Stopped: true, EndErr: errors.New("redials spent")},
			code: 1, msg: "craze: lost the session: redials spent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			err := attachExit(tc.res, tc.err, &stderr)
			switch {
			case tc.sameErr:
				if err != tc.err {
					t.Fatalf("returned %v, want the start's own error %v", err, tc.err)
				}
			case tc.code != 0:
				assertExit(t, err, tc.code, tc.msg)
			case err != nil:
				t.Fatalf("returned %v, want exit 0", err)
			}
			if stderr.String() != tc.stderr {
				t.Fatalf("stderr %q, want %q", stderr.String(), tc.stderr)
			}
		})
	}
}

// TestAttachDialsTheEntry: the attach TUI's Config is the entry's session over
// its socket — Viewer on, the entry's workspace, the command line's view
// flags — and before the attach completes its Info is the registry entry's
// provider (GLM 11). Once started, the Info is the host's. A dial that fails
// is exit 1, one `craze attach:` line.
func TestAttachDialsTheEntry(t *testing.T) {
	indexHome(t)
	env := rundir.ProcessEnv()
	rh, hostID := servingHost(t, env, io.Discard)
	eng := grokStubEngine(t)
	rh.onEngine(eng)
	if err := eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	crazeID := eng.State().CrazeSessionID
	entry := waitEntry(t, entryPath(env, hostID), func(e rundir.Entry) bool { return e.Ready })
	entry.Provider = "gx" // what the fallback reads, told apart from the host's grok
	cfg, err := attachConfig(attachTarget{entry: entry, sessionID: entry.CrazeSessionID},
		attachView{theme: "dracula", noMouse: true, noBackground: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cfg.Backend.Close() })
	if !cfg.Viewer || cfg.Workspace != entry.Workspace || cfg.Theme != "dracula" || !cfg.NoMouse || cfg.Background {
		t.Fatalf("the attach TUI's config: %+v", cfg)
	}
	if cfg.Session != nil || cfg.NewSession != nil || cfg.OnEngine != nil || cfg.SessionIndex != nil || cfg.Host != nil ||
		cfg.PersistProvider || cfg.ClaimSession != nil {
		t.Fatalf("the attach TUI was handed something of a host's: %+v", cfg)
	}
	if info := cfg.Backend.Info(); info.Provider != "gx" || info.Workspace != entry.Workspace || info.CrazeSessionID != crazeID {
		t.Fatalf("Info before the attach: %+v", info)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cfg.Backend.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if info := cfg.Backend.Info(); info.Provider != "grok" || info.CrazeSessionID != crazeID {
		t.Fatalf("Info once attached: %+v", info)
	}

	gone := entry
	gone.Socket = filepath.Join(filepath.Dir(entry.Socket), "gone.sock")
	_, err = attachConfig(attachTarget{entry: gone, sessionID: crazeID}, attachView{})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 ||
		!strings.HasPrefix(ee.msg, "craze attach: session "+crazeID+" is unreachable: ") || strings.Contains(ee.msg, "\n") {
		t.Fatalf("a dial to nothing: %v", err)
	}
}

// TestTheTerminalCheckIsATerminalsOwn (sol r66): a TUI needs a terminal, not
// any character device — /dev/null is one, and neither the host TUI nor craze
// attach may start on it; a pty's end is a terminal. craze attach asks once
// its target is resolved, so the refusal is the usage error, not a TUI no one
// sees.
func TestTheTerminalCheckIsATerminalsOwn(t *testing.T) {
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if st, err := null.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("fixture: %s is not a character device (%v)", os.DevNull, err)
	}
	if isTerminal(null) {
		t.Fatalf("%s reads as a terminal", os.DevNull)
	}
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	if !isTerminal(tty) {
		t.Fatal("a pty's end does not read as a terminal")
	}

	indexHome(t)
	cmd := NewRootCmd()
	cmd.SetOut(null)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)
	assertExit(t, cmd.Execute(), 2, "craze: refusing to start TUI on a non-tty")

	env := rundir.ProcessEnv()
	rh, hostID := servingHost(t, env, io.Discard)
	rh.onEngine(grokStubEngine(t))
	waitEntry(t, entryPath(env, hostID), func(e rundir.Entry) bool { return e.CrazeSessionID != "" })
	cmd = NewRootCmd()
	cmd.SetOut(null)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"attach", "--session", hostID})
	assertExit(t, cmd.Execute(), 2, "craze attach: refusing to start TUI on a non-tty")
}
