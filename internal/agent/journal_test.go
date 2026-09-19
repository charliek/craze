package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/version"
)

// Every session journals when Options.JournalDir is set (plan 020 §3.5,
// commit C5a), and nothing is written anywhere when it is not. These tests
// run the sessions themselves — the live one against the fake agent, the
// native one on a scripted model — and read back the file they left: one
// file per incarnation, where §3.4 says, header first, the session note, every
// event the primary delivered in order, closing last.

// journalOf is s's journal writer, failing the test when it has none.
func journalOf(t *testing.T, l *EventLog) *journal.Writer {
	t.Helper()
	if l.journal == nil {
		t.Fatal("the session was built without a journal")
	}
	return l.journal
}

// closeJournaled closes sess and waits for its journal's writer to finish,
// so the file is whole when the test reads it: the log's Close waits only
// 500 ms for the writer, and a slow -race run must not read a file still
// being written.
func closeJournaled(t *testing.T, sess Session, w *journal.Writer) {
	t.Helper()
	within(t, "Close", func() { _ = sess.Close() })
	if err := writerExited(w); !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("waiting for the journal writer to finish: %v", err)
	}
}

// journalFiles is every regular file under dir, failing the test if any
// directory there is broader than 0700 or any file broader than 0600 ("no
// broader than": a umask may narrow them). A dir that does not exist has
// none.
func journalFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if perm := info.Mode().Perm(); perm&^0o700 != 0 {
				t.Errorf("directory %s is %v, want no broader than 0700", path, perm)
			}
		case info.Mode().IsRegular():
			if perm := info.Mode().Perm(); perm&^0o600 != 0 {
				t.Errorf("file %s is %v, want no broader than 0600", path, perm)
			}
			files = append(files, path)
		default:
			t.Errorf("%s is neither a file nor a directory: %v", path, info.Mode())
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return files
}

// assertOneJournal fails unless dir holds exactly one file, the writer's,
// at <dir>/<workspace slug>/<UTC stamp>_<incarnation>.jsonl, and returns its
// lines.
func assertOneJournal(t *testing.T, dir string, w *journal.Writer, inc string) []map[string]any {
	t.Helper()
	files := journalFiles(t, dir)
	if len(files) != 1 || files[0] != w.Path() {
		t.Fatalf("the journal directory holds %v, want exactly the writer's %s", files, w.Path())
	}
	if got := filepath.Dir(filepath.Dir(w.Path())); got != dir {
		t.Fatalf("the journal is at %s, want one directory below %s", w.Path(), dir)
	}
	name := regexp.MustCompile(`^\d{8}T\d{6}Z_` + regexp.QuoteMeta(inc) + `\.jsonl$`)
	if base := filepath.Base(w.Path()); !name.MatchString(base) {
		t.Fatalf("the journal is named %q, want <yyyymmddThhmmssZ>_%s.jsonl", base, inc)
	}
	return fileLines(t, w)
}

// assertHeader fails unless lines[0] is the header, with the fields every
// header has and the ones want names (JSON values: numbers are float64).
func assertHeader(t *testing.T, lines []map[string]any, want map[string]any) {
	t.Helper()
	h := lines[0]
	if h["type"] != "header" {
		t.Fatalf("the first line is %v, want the header", h)
	}
	fixed := map[string]any{
		"format": float64(journal.FormatVersion), "eventCodec": float64(EventCodecVersion),
		"crazeVersion": version.Version, "os": runtime.GOOS, "arch": runtime.GOARCH,
		"pid": float64(os.Getpid()),
	}
	for _, m := range []map[string]any{fixed, want} {
		for k, v := range m {
			if h[k] != v {
				t.Errorf("header %s = %#v, want %#v (header %v)", k, h[k], v, h)
			}
		}
	}
	if ts, _ := h["ts"].(string); ts == "" {
		t.Errorf("the header has no ts: %v", h)
	}
}

// sessionNotes is the file's session lines.
func sessionNotes(lines []map[string]any) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["type"] == "session" {
			out = append(out, l)
		}
	}
	return out
}

// assertEventLines fails unless the event lines' seqs run 1, 2, 3, … and
// every body decodes (DecodeEvent, through Record.Event) to exactly the event
// the primary delivered, in the same order.
func assertEventLines(t *testing.T, lines []map[string]any, w *journal.Writer, primary []Event) {
	t.Helper()
	var seq float64
	for _, l := range lines {
		if l["type"] != "event" {
			continue
		}
		seq++
		if l["seq"] != seq {
			t.Fatalf("event line seq %v, want %v: the event lines are not contiguous from 1", l["seq"], seq)
		}
	}
	if int(seq) != len(primary) {
		t.Fatalf("%v event lines for %d primary events", seq, len(primary))
	}
	assertRecordsArePrimary(t, "the journal", fileRecords(t, w), primary)
}

// liveJournalOptions are the options the live tests share: the fake agent
// asked for by name, so the header's requested binary and the session note's
// resolved one differ, found on a PATH that leads with the fake agent's own
// directory.
func liveJournalOptions(t *testing.T, script, dir string) Options {
	t.Helper()
	bin, err := filepath.Abs(fakeAgentPath(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	return Options{
		Binary:      filepath.Base(bin),
		ExtraArgs:   []string{"-script=" + script},
		Workspace:   t.TempDir(),
		Force:       true,
		Interactive: true,
		Stderr:      io.Discard,
		JournalDir:  dir,
	}
}

// TestJournalLiveSessionWritesOneFile is A18's Go half for the ACP session: a
// real Start, a turn with a tool call, and Close leave one file with the
// header (the requested binary, the absolute workspace), one session note
// (the provider's id and the resolved binary), every event the primary got,
// decoding to it, and closing last.
func TestJournalLiveSessionWritesOneFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	opts := liveJournalOptions(t, "tool", dir)
	s := newTestSession(t, opts)
	w := journalOf(t, s.log)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	closeJournaled(t, s, w)
	primary := drained(s)

	lines := assertOneJournal(t, dir, w, s.Incarnation())
	assertHeader(t, lines, map[string]any{
		"incarnation": s.Incarnation(), "provider": "cursor", "agentBinary": opts.Binary,
		"cwd": opts.Workspace, "force": true, "interactive": true, "mode": "",
	})
	notes := sessionNotes(lines)
	resolved, err := filepath.Abs(fakeAgentPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0]["providerSessionId"] != s.Snapshot().SessionID ||
		notes[0]["agentBinary"] != resolved || notes[0]["loadedFrom"] != nil {
		t.Fatalf("session notes %v, want one for %q resolved to %s, not loaded", notes, s.Snapshot().SessionID, resolved)
	}
	if len(ofType(primary, EventTool)) == 0 {
		t.Fatalf("the turn had no tool event to journal: %v", typesOf(primary))
	}
	assertEventLines(t, lines, w, primary)
	assertClosingIsLast(t, lines)
}

// linkAgent puts a link to the fake agent at path, as a directory on PATH
// would hold it.
func linkAgent(t *testing.T, agent, path string) {
	t.Helper()
	abs, err := filepath.Abs(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(abs, path); err != nil {
		t.Fatal(err)
	}
}

// writeDecoy puts an executable at path that is not an agent at all: it
// records that it ran and exits. A session that spawned it would hang on
// initialize, so the marker is what a test asserts on rather than the failure.
func writeDecoy(t *testing.T, path, marker string) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\n: > %q\nexit 1\n", marker)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestJournalSessionNoteNamesTheBinaryThatRan: the agent binary is resolved
// once, before the spawn, and that one path is both what runs and what the
// session note records. So a PATH that leads somewhere else by the time the
// note is written — a candidate swapped on disk, a CRAZE_AGENT_BIN changed
// under the process — cannot make the note name a binary the session never
// ran, or name none at all. Both spellings of "which binary" are here, because
// the single resolution has to keep CRAZE_AGENT_BIN's precedence exactly.
func TestJournalSessionNoteNamesTheBinaryThatRan(t *testing.T) {
	for _, c := range []struct {
		name     string
		explicit bool
	}{
		{"asked for by name", true},
		{"asked for through CRAZE_AGENT_BIN", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			const name = "craze-test-agent"
			onPath, decoy := t.TempDir(), t.TempDir()
			ran := filepath.Join(onPath, name)
			linkAgent(t, fakeAgentPath(t), ran)
			marker := filepath.Join(t.TempDir(), "the-decoy-ran")
			writeDecoy(t, filepath.Join(decoy, name), marker)
			t.Setenv("PATH", onPath)
			t.Setenv("CRAZE_AGENT_BIN", "")

			dir := filepath.Join(t.TempDir(), "journal")
			opts := Options{
				ExtraArgs:  []string{"-script=echo"},
				Workspace:  t.TempDir(),
				Force:      true,
				Stderr:     io.Discard,
				Diag:       io.Discard,
				JournalDir: dir,
			}
			if c.explicit {
				opts.Binary = name
			} else {
				t.Setenv("CRAZE_AGENT_BIN", name)
			}
			// The swap, at the one instant that used to matter: from here the
			// name leads to the decoy, and nothing after the resolution may
			// look it up again — not the spawn, and not the note.
			testAfterBinaryResolved = func() { t.Setenv("PATH", decoy) }
			t.Cleanup(func() { testAfterBinaryResolved = nil })

			s := newTestSession(t, opts)
			w := journalOf(t, s.log)
			if err := s.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Prompt(t.Context(), "hello"); err != nil {
				t.Fatal(err)
			}
			closeJournaled(t, s, w)

			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("the session ran %s, the decoy the PATH led to after the resolution", filepath.Join(decoy, name))
			}
			notes := sessionNotes(fileLines(t, w))
			if len(notes) != 1 || notes[0]["agentBinary"] != ran {
				t.Fatalf("session notes %v, want one naming %s, the binary that ran", notes, ran)
			}
		})
	}
}

// TestJournalNativeSessionWritesOneFile is A18's Go half for the native
// adapter on a scripted model: the header names the native provider and no
// binary, the session note carries the harness's session id alone.
func TestJournalNativeSessionWritesOneFile(t *testing.T) {
	f := newNativeFixture(t)
	f.models["test/a"].push(reply(thoughtParts("thinking"), textParts("hello ", "there"), finishParts(fantasy.FinishReasonStop)))
	dir := filepath.Join(t.TempDir(), "journal")
	ws := t.TempDir()
	s := f.session(Options{Workspace: ws, JournalDir: dir})
	w := journalOf(t, s.log)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(t.Context(), "hi"); err != nil {
		t.Fatal(err)
	}
	closeJournaled(t, s, w)
	primary := drained(s)

	lines := assertOneJournal(t, dir, w, s.Incarnation())
	assertHeader(t, lines, map[string]any{
		"incarnation": s.Incarnation(), "provider": "native", "agentBinary": "",
		"cwd": ws, "force": false, "interactive": false, "mode": "",
	})
	notes := sessionNotes(lines)
	if len(notes) != 1 || notes[0]["providerSessionId"] != s.Snapshot().SessionID || s.Snapshot().SessionID == "" {
		t.Fatalf("session notes %v, want one for the harness's %q", notes, s.Snapshot().SessionID)
	}
	if _, ok := notes[0]["agentBinary"]; ok {
		t.Fatalf("the native session note names a binary: %v", notes[0])
	}
	if joined(primary, EventText) != "hello there" {
		t.Fatalf("the turn's text is %q", joined(primary, EventText))
	}
	assertEventLines(t, lines, w, primary)
	assertClosingIsLast(t, lines)
}

// TestJournalUnstartedSessionLeavesNothing is A26: a session built with a
// journal and closed without starting — a picker's discarded one — leaves no
// file and no directory, for either kind.
func TestJournalUnstartedSessionLeavesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(dir string) (Session, *EventLog)
	}{
		{"acp", func(dir string) (Session, *EventLog) {
			s := newSession(Options{Workspace: t.TempDir(), JournalDir: dir})
			return s, s.log
		}},
		{"native", func(dir string) (Session, *EventLog) {
			s := newNative(Options{Workspace: t.TempDir(), JournalDir: dir}, nil)
			return s, s.log
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "journal")
			sess, log := tc.build(dir)
			closeJournaled(t, sess, journalOf(t, log))
			if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a session never started left %s behind (%v): %v", dir, err, journalFiles(t, dir))
			}
		})
	}
}

// TestJournalRefusedIsOneDiagLine: a journal that cannot be built (here, a
// relative directory, which the journal refuses) never refuses the session.
// It runs without one, and Diag — else Stderr — says so in one line.
func TestJournalRefusedIsOneDiagLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(Options) (Session, *EventLog)
	}{
		{"acp", func(o Options) (Session, *EventLog) { s := newSession(o); return s, s.log }},
		{"native", func(o Options) (Session, *EventLog) { s := newNative(o, nil); return s, s.log }},
	} {
		for _, lane := range []string{"diag", "stderr"} {
			t.Run(tc.name+" on "+lane, func(t *testing.T) {
				var buf bytes.Buffer
				o := Options{Workspace: t.TempDir(), JournalDir: "relative/journal"}
				if lane == "diag" {
					o.Diag, o.Stderr = &buf, io.Discard
				} else {
					o.Stderr = &buf
				}
				sess, log := tc.build(o)
				closeAtCleanup(t, sess)
				if log.journal != nil {
					t.Fatal("a relative JournalDir built a journal")
				}
				if got := buf.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "not journaled") {
					t.Fatalf("%s got %q, want one line saying the session is not journaled", lane, got)
				}
			})
		}
	}
}

// TestJournalOffIsolation is A19's first sentence: a real Start, a prompt and
// Close with no JournalDir — every caller's default — under an isolated HOME,
// CRAZE_HOME and workspace create no file under any of them. The native half
// may write its own transcripts under CRAZE_HOME/native (the harness's store,
// not the journal), so it is held to "no journal directory, no other file".
func TestJournalOffIsolation(t *testing.T) {
	bin := fakeAgentPath(t) // built, if at all, before HOME moves
	t.Run("acp", func(t *testing.T) {
		home, crazeHome, ws := t.TempDir(), t.TempDir(), t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("CRAZE_HOME", crazeHome)
		s := newSession(Options{Binary: bin, ExtraArgs: []string{"-script=echo"}, Workspace: ws, Force: true, Stderr: io.Discard})
		closeAtCleanup(t, s)
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Prompt(t.Context(), "hello"); err != nil {
			t.Fatal(err)
		}
		within(t, "Close", func() { _ = s.Close() })
		if st := s.log.Health().Journal.State; st != journal.StateOff {
			t.Fatalf("journal state %q with no JournalDir, want off", st)
		}
		for _, dir := range []string{home, crazeHome, ws} {
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("%s holds %v (%v) after a session with no journal", dir, entries, err)
			}
		}
	})
	t.Run("native", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		f := newNativeFixture(t) // CRAZE_HOME, with the model table under native/
		crazeHome := filepath.Dir(f.dir)
		f.models["test/a"].push(answer("ok"))
		ws := t.TempDir()
		s := f.started(Options{Workspace: ws})
		if _, err := s.Prompt(t.Context(), "hi"); err != nil {
			t.Fatal(err)
		}
		within(t, "Close", func() { _ = s.Close() })
		if st := s.log.Health().Journal.State; st != journal.StateOff {
			t.Fatalf("journal state %q with no JournalDir, want off", st)
		}
		for _, dir := range []string{home, ws} {
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("%s holds %v (%v) after a session with no journal", dir, entries, err)
			}
		}
		entries, err := os.ReadDir(crazeHome)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != "native" {
				t.Fatalf("CRAZE_HOME holds %q besides the harness's own directory", e.Name())
			}
		}
	})
}

// TestJournalResumeWritesANewFile is A22: a session loaded from an earlier
// one's id is a new incarnation, journaled to a new file, whose session note
// says what it was loaded from, whose replayed events carry replayed: true
// between the replay brackets (the brackets themselves not), and which
// leaves the earlier file byte for byte as it was.
func TestJournalResumeWritesANewFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	opts := liveJournalOptions(t, "echo", dir)
	first := newTestSession(t, opts)
	firstW := journalOf(t, first.log)
	if err := first.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Prompt(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	id := first.Snapshot().SessionID
	closeJournaled(t, first, firstW)
	before, err := os.ReadFile(firstW.Path())
	if err != nil {
		t.Fatal(err)
	}

	second := newLoadSession(t, "load", func(o *Options) {
		o.Binary, o.Workspace, o.JournalDir, o.LoadSessionID = opts.Binary, opts.Workspace, dir, id
	})
	secondW := journalOf(t, second.log)
	if err := second.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	closeJournaled(t, second, secondW)

	if second.Incarnation() == first.Incarnation() || secondW.Path() == firstW.Path() {
		t.Fatalf("the resumed session shares the first's incarnation or file: %s", secondW.Path())
	}
	if files := journalFiles(t, dir); len(files) != 2 {
		t.Fatalf("the journal directory holds %v, want the two sessions' files", files)
	}
	after, err := os.ReadFile(firstW.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the first incarnation's file changed:\nbefore %s\nafter  %s", before, after)
	}

	lines := fileLines(t, secondW)
	assertHeader(t, lines, map[string]any{"incarnation": second.Incarnation(), "agentBinary": opts.Binary})
	notes := sessionNotes(lines)
	if len(notes) != 1 || notes[0]["providerSessionId"] != id || notes[0]["loadedFrom"] != id {
		t.Fatalf("session notes %v, want one for %q loaded from %q", notes, id, id)
	}
	assertClosingIsLast(t, lines)

	var evs []Event
	inReplay := false
	for _, rec := range fileRecords(t, secondW) {
		ev, err := rec.Event()
		if err != nil {
			t.Fatal(err)
		}
		bracket := ev.Type == EventReplay && ev.Replay != nil
		switch {
		case bracket && ev.Replay.Phase == ReplayStart:
			inReplay = true
		case bracket && ev.Replay.Phase == ReplayEnd:
			inReplay = false
		}
		if ev.Replayed != (inReplay && !bracket) {
			t.Fatalf("seq %d (%s) replayed = %v, want %v", ev.Seq, ev.Type, ev.Replayed, inReplay && !bracket)
		}
		evs = append(evs, ev)
	}
	// TestLoadReplayIsBracketedAndStamped's shape, read back from the file.
	want := []string{"replay:start", "user", "thought", "tool", "text", "replay:end"}
	if got := eventShape(evs); !equalStrings(got, want) {
		t.Fatalf("the resumed journal's events are %v, want %v", got, want)
	}
	assertRecordsArePrimary(t, "the resumed journal", fileRecords(t, secondW), drained(second))
}
