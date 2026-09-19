package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
)

// The models the tests write with. kimi and kimiCode share a provider and
// differ in wire model; minimax is another provider.
var (
	kimi     = Model{Provider: "fireworks", Alias: "fireworks/kimi-k3", WireModel: "accounts/fireworks/models/kimi-k3"}
	kimiCode = Model{Provider: "fireworks", Alias: "fireworks/kimi-k2p7-code", WireModel: "accounts/fireworks/models/kimi-k2p7-code"}
	minimax  = Model{Provider: "openrouter", Alias: "openrouter/minimax-m3", WireModel: "minimax/minimax-m3"}
)

// testWorkspace is the header's cwd. The store never touches the workspace,
// so it need not exist, and a fixed one keeps the header deterministic.
const testWorkspace = "/work/craze"

// clock is a fake Now: each call returns the previous time plus a second.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.t
	c.t = c.t.Add(time.Second)
	return now
}

// seqIDs is an entry-id source counting up from 1 as 8 hex chars.
func seqIDs() func() string {
	var mu sync.Mutex
	n := 0
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf("%08x", n)
	}
}

// scriptedIDs returns ids in order, then fails the test if asked for more.
func scriptedIDs(t *testing.T, ids ...string) func() string {
	return func() string {
		if len(ids) == 0 {
			t.Fatal("the entry-id source ran out")
		}
		id := ids[0]
		ids = ids[1:]
		return id
	}
}

// writeLog records every Write the store makes on its descriptor, and can
// fail one: failOn is the 1-based write that writes the first half of its
// bytes and then returns an error, the way a torn write looks on disk, or,
// with failEmpty, writes nothing before failing (a disk already full).
// onWrite, when set, runs before each write reaches the file.
type writeLog struct {
	mu        sync.Mutex
	writes    [][]byte
	failOn    int
	failEmpty bool
	onWrite   func()
}

func (l *writeLog) opener() func(string, int, os.FileMode) (file, error) {
	return func(name string, flag int, perm os.FileMode) (file, error) {
		f, err := os.OpenFile(name, flag, perm)
		if err != nil {
			return nil, err
		}
		return &loggedFile{f: f, log: l}, nil
	}
}

func (l *writeLog) all() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]byte(nil), l.writes...)
}

type loggedFile struct {
	f   *os.File
	log *writeLog
}

func (lf *loggedFile) Write(p []byte) (int, error) {
	lf.log.mu.Lock()
	lf.log.writes = append(lf.log.writes, append([]byte(nil), p...))
	fail := len(lf.log.writes) == lf.log.failOn
	empty, onWrite := lf.log.failEmpty, lf.log.onWrite
	lf.log.mu.Unlock()
	if onWrite != nil {
		onWrite()
	}
	if fail {
		n := 0
		if !empty {
			n, _ = lf.f.Write(p[:len(p)/2])
		}
		return n, errors.New("injected: no space left on device")
	}
	return lf.f.Write(p)
}

// sessionDirNames is what the session's directory holds, by name: the
// transcript and nothing else once a first write has settled, whatever
// happened to it.
func sessionDirNames(t *testing.T, s *Store) []string {
	t.Helper()
	des, err := os.ReadDir(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

func (lf *loggedFile) Close() error { return lf.f.Close() }

// testOptions is a store rooted in a fresh home with a fixed clock, session
// id and entry ids.
func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Home:         filepath.Join(t.TempDir(), "native"),
		Workspace:    testWorkspace,
		CrazeVersion: "v0.0.0-test",
		SystemPrompt: "You are craze.",
		Now:          newClock().now,
		sessionID:    "00000000-0000-4000-8000-000000000001",
		entryID:      seqIDs(),
	}
}

func newStore(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func user(text string, m Model) MessageEntry {
	return MessageEntry{Message: fantasy.NewUserMessage(text), Model: m}
}

// assistantMsg is an assistant message with a reasoning part when reasoning
// is not "" and a text part when text is not "".
func assistantMsg(reasoning, text string) fantasy.Message {
	var parts []fantasy.MessagePart
	if reasoning != "" {
		parts = append(parts, fantasy.ReasoningPart{Text: reasoning})
	}
	if text != "" {
		parts = append(parts, fantasy.TextPart{Text: text})
	}
	return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: parts}
}

func answer(reasoning, text string, m Model) MessageEntry {
	return MessageEntry{Message: assistantMsg(reasoning, text), Model: m, StopReason: "end_turn"}
}

// turn writes one whole turn and fails the test on error.
func turn(t *testing.T, s *Store, prompt, reply string, m Model) {
	t.Helper()
	if err := s.AppendUser(user(prompt, m)); err != nil {
		t.Fatalf("AppendUser: %v", err)
	}
	if err := s.AppendAssistant(answer("", reply, m)); err != nil {
		t.Fatalf("AppendAssistant: %v", err)
	}
}

// fileTypes is the type of each of the file's lines, header included.
func fileTypes(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		env, err := lineType(line)
		if err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		types = append(types, env)
	}
	return types
}

func lineType(line string) (string, error) {
	var env struct {
		Type string `json:"type"`
	}
	err := json.Unmarshal([]byte(line), &env)
	return env.Type, err
}

// messageTexts is each message's role and text parts, for comparing
// contexts at a glance.
func messageTexts(msgs []fantasy.Message) []string {
	var out []string
	for _, m := range msgs {
		var parts []string
		for _, p := range m.Content {
			switch p.GetType() {
			case fantasy.ContentTypeText:
				tp, _ := fantasy.AsMessagePart[fantasy.TextPart](p)
				parts = append(parts, tp.Text)
			case fantasy.ContentTypeReasoning:
				rp, _ := fantasy.AsMessagePart[fantasy.ReasoningPart](p)
				parts = append(parts, "(thinking: "+rp.Text+")")
			}
		}
		out = append(out, string(m.Role)+": "+strings.Join(parts, " "))
	}
	return out
}

func TestNewDoesNoIO(t *testing.T) {
	opts := testOptions(t)
	s := newStore(t, opts)
	// A turn that produced no output: a prompt and a model switch, then the
	// session closes.
	if err := s.AppendModelChange(minimax); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUser(user("hello", minimax)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(opts.Home); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a session with no turn created %s (stat err %v); want no file and no directory", opts.Home, err)
	}
}

func TestPathLayout(t *testing.T) {
	opts := testOptions(t)
	s := newStore(t, opts)
	want := filepath.Join(opts.Home, "sessions", "--work-craze--", "20260918T120000Z_"+opts.sessionID+".jsonl")
	if s.Path() != want {
		t.Fatalf("Path = %s, want %s", s.Path(), want)
	}
	if s.ID() != opts.sessionID {
		t.Fatalf("ID = %s, want %s", s.ID(), opts.sessionID)
	}
}

func TestNewGeneratesAUUIDv4(t *testing.T) {
	opts := testOptions(t)
	opts.sessionID = ""
	a, b := newStore(t, opts), newStore(t, opts)
	for _, id := range []string{a.ID(), b.ID()} {
		// xxxxxxxx-xxxx-4xxx-[89ab]xxx-xxxxxxxxxxxx
		if len(id) != 36 || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
			t.Fatalf("session id %q is not a v4 UUID", id)
		}
	}
	if a.ID() == b.ID() {
		t.Fatalf("two sessions got the same id %s", a.ID())
	}
}

func TestNewRejectsRelativePaths(t *testing.T) {
	for _, tc := range []struct{ home, workspace string }{
		{"native", testWorkspace},
		{"", testWorkspace},
		{"/tmp/native", "src/app"},
		{"/tmp/native", ""},
	} {
		opts := testOptions(t)
		opts.Home, opts.Workspace = tc.home, tc.workspace
		if _, err := New(opts); err == nil {
			t.Errorf("New(home %q, workspace %q) succeeded; want an error", tc.home, tc.workspace)
		}
	}
}

// TestFirstTurnIsOneWrite proves the atomic first write by counting the
// Write calls on the descriptor: the header, the user entry and the
// assistant entry arrive in one, and each later turn is one more.
func TestFirstTurnIsOneWrite(t *testing.T) {
	opts := testOptions(t)
	var log writeLog
	opts.openFile = log.opener()
	s := newStore(t, opts)

	turn(t, s, "what is 2+2?", "4", kimi)
	writes := log.all()
	if len(writes) != 1 {
		t.Fatalf("the first turn took %d writes, want 1", len(writes))
	}
	if n := bytes.Count(writes[0], []byte("\n")); n != 3 {
		t.Fatalf("the first write has %d lines, want 3 (header, user, assistant):\n%s", n, writes[0])
	}
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, writes[0]) {
		t.Fatalf("the file is not exactly the first write:\n%s", data)
	}

	turn(t, s, "and 3+3?", "6", kimi)
	if err := s.AppendEffortChange("high"); err != nil {
		t.Fatal(err)
	}
	turn(t, s, "and 4+4?", "8", kimi)
	writes = log.all()
	if len(writes) != 3 {
		t.Fatalf("three turns took %d writes, want 3", len(writes))
	}
	for i, want := range []int{2, 3} {
		if n := bytes.Count(writes[i+1], []byte("\n")); n != want {
			t.Fatalf("write %d has %d lines, want %d:\n%s", i+2, n, want, writes[i+1])
		}
	}
	got := fileTypes(t, s.Path())
	want := []string{"session", "message", "message", "message", "message", "effort_change", "message", "message"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("line types = %v, want %v", got, want)
	}
}

// TestChangeWaitsForOutput pins the choice the package comment documents: a
// model switch followed by a turn cancelled before any output leaves no lone
// model_change in the file; the change goes out ahead of the next turn that
// produces output, and a second switch replaces an unwritten one.
func TestChangeWaitsForOutput(t *testing.T) {
	opts := testOptions(t)
	var log writeLog
	opts.openFile = log.opener()
	s := newStore(t, opts)
	turn(t, s, "first", "one", kimi)

	if err := s.AppendModelChange(kimiCode); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUser(user("cancelled before any output", kimiCode)); err != nil {
		t.Fatal(err)
	}
	// The turn is cancelled: the runner writes nothing.
	if n := len(log.all()); n != 1 {
		t.Fatalf("a change and a turn with no output made %d writes in all, want 1 (the first turn)", n)
	}

	// The next turn switches again, then answers.
	if err := s.AppendModelChange(minimax); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEffortChange(""); err != nil {
		t.Fatal(err)
	}
	turn(t, s, "second", "two", minimax)
	writes := log.all()
	if len(writes) != 2 {
		t.Fatalf("got %d writes, want 2", len(writes))
	}
	got := fileTypes(t, s.Path())
	want := []string{"session", "message", "message", "model_change", "effort_change", "message", "message"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("line types = %v, want %v", got, want)
	}

	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	change := tr.Entries[2]
	if change.Model != minimax {
		t.Fatalf("the written model_change is %+v, want the later switch %+v", change.Model, minimax)
	}
	if effort := tr.Entries[3]; effort.Type != TypeEffortChange || effort.Effort != "" {
		t.Fatalf("effort change = %+v, want an effort_change to none", effort)
	}
	if msgs := messageTexts(tr.Context(minimax)); !reflect.DeepEqual(msgs, []string{
		"user: first", "assistant: one", "user: second", "assistant: two",
	}) {
		t.Fatalf("the cancelled prompt reached the transcript: %q", msgs)
	}
}

func TestEffortChangeKeepsEmptyEffort(t *testing.T) {
	opts := testOptions(t)
	s := newStore(t, opts)
	if err := s.AppendEffortChange(""); err != nil {
		t.Fatal(err)
	}
	turn(t, s, "q", "a", kimi)
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"type":"effort_change","id":"00000001","parentId":null,"timestamp":"2026-09-18T12:00:01.000Z","effort":""}`)) {
		t.Fatalf("an effort change to none must carry effort \"\":\n%s", data)
	}
}

func TestAssistantWithoutTextIsNotPersisted(t *testing.T) {
	opts := testOptions(t)
	s := newStore(t, opts)
	if err := s.AppendUser(user("think hard", kimi)); err != nil {
		t.Fatal(err)
	}
	for _, m := range []fantasy.Message{
		assistantMsg("only reasoning so far", ""),
		assistantMsg("", " \n\t"),
		{Role: fantasy.MessageRoleAssistant},
	} {
		err := s.AppendAssistant(MessageEntry{Message: m, Model: kimi, Interrupted: true})
		if !errors.Is(err, ErrNoOutput) {
			t.Fatalf("AppendAssistant(%s) = %v, want ErrNoOutput", messageTexts([]fantasy.Message{m}), err)
		}
	}
	if _, err := os.Stat(opts.Home); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an answer with no text created %s", opts.Home)
	}
	// The held user entry was not disturbed: a real answer still writes it.
	if err := s.AppendAssistant(answer("thought", "answer", kimi)); err != nil {
		t.Fatal(err)
	}
	if got := fileTypes(t, s.Path()); !reflect.DeepEqual(got, []string{"session", "message", "message"}) {
		t.Fatalf("line types = %v", got)
	}
}

func TestFirstAssistantNeedsAUser(t *testing.T) {
	s := newStore(t, testOptions(t))
	if err := s.AppendAssistant(answer("", "hi", kimi)); !errors.Is(err, ErrNoUser) {
		t.Fatalf("AppendAssistant with no user entry = %v, want ErrNoUser", err)
	}
}

// TestLaterStepAppendsAlone covers a turn's second step (H2's tool loop):
// with no user entry held, the assistant entry is written on its own,
// parented on the step before.
func TestLaterStepAppendsAlone(t *testing.T) {
	s := newStore(t, testOptions(t))
	turn(t, s, "q", "step one", kimi)
	if err := s.AppendAssistant(answer("", "step two", kimi)); err != nil {
		t.Fatal(err)
	}
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	last := tr.Entries[len(tr.Entries)-1]
	if last.ParentID != tr.Entries[len(tr.Entries)-2].ID {
		t.Fatalf("the second step's parent is %q, want the first step %q", last.ParentID, tr.Entries[len(tr.Entries)-2].ID)
	}
}

func TestRoleAndModelAreChecked(t *testing.T) {
	s := newStore(t, testOptions(t))
	if err := s.AppendUser(MessageEntry{Message: assistantMsg("", "x"), Model: kimi}); err == nil {
		t.Error("AppendUser took an assistant message")
	}
	if err := s.AppendAssistant(user("x", kimi)); err == nil {
		t.Error("AppendAssistant took a user message")
	}
	if err := s.AppendUser(user("x", Model{Provider: "fireworks"})); err == nil {
		t.Error("AppendUser took an entry with no wire model")
	}
	if err := s.AppendModelChange(Model{Alias: "a", WireModel: "w"}); err == nil {
		t.Error("AppendModelChange took a model with no provider")
	}
}

func TestAppendAfterCloseIsAnError(t *testing.T) {
	s := newStore(t, testOptions(t))
	turn(t, s, "q", "a", kimi)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("a second Close = %v, want nil", err)
	}
	for name, err := range map[string]error{
		"AppendUser":         s.AppendUser(user("q", kimi)),
		"AppendAssistant":    s.AppendAssistant(answer("", "a", kimi)),
		"AppendModelChange":  s.AppendModelChange(minimax),
		"AppendEffortChange": s.AppendEffortChange("low"),
	} {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("%s after Close = %v, want ErrClosed", name, err)
		}
	}
	if got := len(s.Context(kimi)); got != 2 {
		t.Fatalf("Context after Close has %d messages, want 2", got)
	}
	if got := fileTypes(t, s.Path()); len(got) != 3 {
		t.Fatalf("appends after Close reached the file: %v", got)
	}
}

// TestFailedAppendIsSticky: once a write to the file has failed after
// writing some of its bytes, the file may end in a torn line, and appending
// after it would turn the tear into mid-file corruption. Every later append
// fails; the file still loads, with the torn turn dropped.
func TestFailedAppendIsSticky(t *testing.T) {
	opts := testOptions(t)
	log := writeLog{failOn: 2}
	opts.openFile = log.opener()
	s := newStore(t, opts)
	turn(t, s, "q1", "a1", kimi)

	if err := s.AppendUser(user("q2", kimi)); err != nil {
		t.Fatal(err)
	}
	err := s.AppendAssistant(answer("", "a2", kimi))
	if !errors.Is(err, ErrFailed) || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("a failed append = %v, want ErrFailed naming the cause", err)
	}
	if err := s.AppendUser(user("q3", kimi)); !errors.Is(err, ErrFailed) {
		t.Fatalf("AppendUser after a failed write = %v, want ErrFailed", err)
	}
	if n := len(log.all()); n != 2 {
		t.Fatalf("the store wrote %d times, want 2 (nothing after the failure)", n)
	}
	if got := messageTexts(s.Context(kimi)); !reflect.DeepEqual(got, []string{"user: q1", "assistant: a1"}) {
		t.Fatalf("Context after a failed write = %q; the failed turn must not be in it", got)
	}
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatalf("Load of a file with a torn tail: %v", err)
	}
	if n := len(tr.Entries); n != 2 {
		t.Fatalf("Load kept %d entries, want 2: the torn turn must be dropped whole", n)
	}
	if got := messageTexts(tr.Context(kimi)); !reflect.DeepEqual(got, []string{"user: q1", "assistant: a1"}) {
		t.Fatalf("Load's context = %q", got)
	}
}

// TestAppendThatWritesNothingIsNotSticky: a later append that fails before
// any byte reaches the file (a disk already full) leaves the file ending on
// a whole turn. It fails that turn only; the next one is written, parented
// on the last turn that was.
func TestAppendThatWritesNothingIsNotSticky(t *testing.T) {
	opts := testOptions(t)
	log := writeLog{failOn: 2, failEmpty: true}
	opts.openFile = log.opener()
	s := newStore(t, opts)
	turn(t, s, "q1", "a1", kimi)

	if err := s.AppendUser(user("q2", kimi)); err != nil {
		t.Fatal(err)
	}
	err := s.AppendAssistant(answer("", "a2", kimi))
	if err == nil || errors.Is(err, ErrFailed) || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("an append that wrote nothing = %v, want a plain error naming the cause", err)
	}
	turn(t, s, "q3", "a3", kimi)
	want := []string{"user: q1", "assistant: a1", "user: q3", "assistant: a3"}
	if got := messageTexts(s.Context(kimi)); !reflect.DeepEqual(got, want) {
		t.Fatalf("Context = %q, want %q", got, want)
	}
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := messageTexts(tr.Context(kimi)); !reflect.DeepEqual(got, want) {
		t.Fatalf("Load's context = %q, want %q", got, want)
	}
}

// TestFirstTurnAppearsWhole: the first turn is written under a temporary
// name and linked into place, so while it is being written the session path
// does not exist, and afterwards it holds the whole turn and the temporary
// name is gone.
func TestFirstTurnAppearsWhole(t *testing.T) {
	opts := testOptions(t)
	var log writeLog
	opts.openFile = log.opener()
	s := newStore(t, opts)
	var during []error
	log.onWrite = func() {
		_, err := os.Stat(s.Path())
		during = append(during, err)
	}
	turn(t, s, "q1", "a1", kimi)
	if len(during) != 1 || !errors.Is(during[0], fs.ErrNotExist) {
		t.Fatalf("while the first turn was written, stat(session path) = %v; want it absent", during)
	}
	if got := fileTypes(t, s.Path()); !reflect.DeepEqual(got, []string{"session", "message", "message"}) {
		t.Fatalf("line types = %v", got)
	}
	if names := sessionDirNames(t, s); !reflect.DeepEqual(names, []string{filepath.Base(s.Path())}) {
		t.Fatalf("the session directory holds %q; want only the transcript", names)
	}
	// Later turns append through the same descriptor, to the linked name.
	turn(t, s, "q2", "a2", kimi)
	if got := fileTypes(t, s.Path()); len(got) != 5 {
		t.Fatalf("line types after a second turn = %v", got)
	}
}

// TestFailedFirstWriteLeavesNoFile: if the first write fails, nothing
// appears at the session path and no temporary file is left behind, and the
// next turn starts the file again, header first.
func TestFailedFirstWriteLeavesNoFile(t *testing.T) {
	opts := testOptions(t)
	log := writeLog{failOn: 1}
	opts.openFile = log.opener()
	s := newStore(t, opts)
	if err := s.AppendUser(user("q1", kimi)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAssistant(answer("", "a1", kimi)); err == nil || errors.Is(err, ErrFailed) {
		t.Fatalf("a failed first write = %v, want a plain error", err)
	}
	if _, err := os.Stat(s.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a failed first write left %s behind (stat err %v)", s.Path(), err)
	}
	if names := sessionDirNames(t, s); len(names) != 0 {
		t.Fatalf("a failed first write left %q in the session directory", names)
	}
	turn(t, s, "q2", "a2", kimi)
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := messageTexts(tr.Context(kimi)); !reflect.DeepEqual(got, []string{"user: q2", "assistant: a2"}) {
		t.Fatalf("context = %q", got)
	}
}

// TestFirstWriteNeverClobbers: a file already at the session path (another
// session's, however unlikely with a fresh UUID in the name) is never
// replaced. Every first write reports the failure, and the other file and
// the directory are left as they were.
func TestFirstWriteNeverClobbers(t *testing.T) {
	s := newStore(t, testOptions(t))
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	const theirs = "someone else's transcript\n"
	if err := os.WriteFile(s.Path(), []byte(theirs), 0o600); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := s.AppendUser(user("q", kimi)); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendAssistant(answer("", "a", kimi)); !errors.Is(err, fs.ErrExist) {
			t.Fatalf("attempt %d: a first write onto an existing file = %v, want fs.ErrExist", attempt, err)
		}
	}
	if got, err := os.ReadFile(s.Path()); err != nil || string(got) != theirs {
		t.Fatalf("the existing file now reads %q (err %v); it must be untouched", got, err)
	}
	if names := sessionDirNames(t, s); !reflect.DeepEqual(names, []string{filepath.Base(s.Path())}) {
		t.Fatalf("the session directory holds %q; want only the existing file", names)
	}
}

func TestEntryIDsAreUniqueWithinTheFile(t *testing.T) {
	opts := testOptions(t)
	// The source repeats itself within one write (the second draw), and
	// then repeats an id already in the file (the fourth draw).
	opts.entryID = scriptedIDs(t, "aaaaaaaa", "aaaaaaaa", "bbbbbbbb", "aaaaaaaa", "cccccccc", "dddddddd")
	s := newStore(t, opts)
	turn(t, s, "q1", "a1", kimi)
	turn(t, s, "q2", "a2", kimi)
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range tr.Entries {
		ids = append(ids, e.ID)
	}
	if want := []string{"aaaaaaaa", "bbbbbbbb", "cccccccc", "dddddddd"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("entry ids = %v, want %v", ids, want)
	}
}

func TestEntryIDSourceThatNeverVariesFails(t *testing.T) {
	opts := testOptions(t)
	opts.entryID = func() string { return "aaaaaaaa" }
	s := newStore(t, opts)
	if err := s.AppendUser(user("q", kimi)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAssistant(answer("", "a", kimi)); err == nil {
		t.Fatal("a turn with no unused id to give its second entry was written")
	}
	if _, err := os.Stat(opts.Home); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused turn created %s", opts.Home)
	}
}

func TestRandomEntryIDs(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := randomEntryID()
		if len(id) != 8 || strings.Trim(id, "0123456789abcdef") != "" {
			t.Fatalf("entry id %q is not 8 hex chars", id)
		}
		seen[id] = true
	}
	if len(seen) < 99 {
		t.Fatalf("100 random ids had only %d distinct values", len(seen))
	}
}

// TestFilesArePrivate: transcripts hold the user's prompts, so the file is
// 0600 and the directories the store creates are 0700 (a umask can only
// narrow either).
func TestFilesArePrivate(t *testing.T) {
	opts := testOptions(t)
	s := newStore(t, opts)
	turn(t, s, "q", "a", kimi)
	for _, p := range []string{opts.Home, filepath.Join(opts.Home, "sessions"), filepath.Dir(s.Path()), s.Path()} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s is %#o, want no group or other access", p, perm)
		}
	}
}

// TestUnreadableEntryIsRefused: every entry is decoded back before it is
// written, so provider metadata Fantasy cannot decode fails the append with
// nothing written, instead of leaving a line that makes the file unreadable.
func TestUnreadableEntryIsRefused(t *testing.T) {
	opts := testOptions(t)
	s := newStore(t, opts)
	if err := s.AppendUser(user("q", kimi)); err != nil {
		t.Fatal(err)
	}
	m := assistantMsg("", "a")
	m.Content = append(m.Content, fantasy.ReasoningPart{
		Text:            "signed",
		ProviderOptions: fantasy.ProviderOptions{"test": &unregistered{}},
	})
	err := s.AppendAssistant(MessageEntry{Message: m, Model: kimi})
	if err == nil || !strings.Contains(err.Error(), "would not read back") {
		t.Fatalf("AppendAssistant with unregistered metadata = %v, want a read-back error", err)
	}
	if _, err := os.Stat(opts.Home); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused turn created %s", opts.Home)
	}
}

// unregistered is provider data whose type no Fantasy provider registered.
type unregistered struct{}

func (unregistered) Options() {}

func (u unregistered) MarshalJSON() ([]byte, error) {
	return fantasy.MarshalProviderType("craze-test.unregistered", struct{}{})
}

func (*unregistered) UnmarshalJSON([]byte) error { return nil }

// TestCloseDuringAppends runs one writer, the way the turn runner drives the
// store, against a Close from another goroutine, and relies on -race to
// catch unsynchronised access. Whatever the interleaving, every append
// either lands whole or fails with ErrClosed, and the file loads with
// exactly the turns that were acknowledged.
func TestCloseDuringAppends(t *testing.T) {
	s := newStore(t, testOptions(t))
	threeTurns := make(chan struct{})
	written := make(chan int)
	go func() {
		n := 0
		for i := 0; ; i++ {
			if i == 3 {
				close(threeTurns)
			}
			if err := s.AppendUser(user(fmt.Sprint("q", i), kimi)); err != nil {
				if !errors.Is(err, ErrClosed) {
					t.Errorf("AppendUser: %v", err)
				}
				break
			}
			if err := s.AppendAssistant(answer("", fmt.Sprint("a", i), kimi)); err != nil {
				if !errors.Is(err, ErrClosed) {
					t.Errorf("AppendAssistant: %v", err)
				}
				break
			}
			_ = s.Context(kimi)
			n++
		}
		written <- n
	}()
	select {
	case <-threeTurns:
	case <-time.After(10 * time.Second):
		t.Fatal("the writer never finished three turns")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var n int
	select {
	case n = <-written:
	case <-time.After(10 * time.Second):
		t.Fatal("the writer never saw ErrClosed")
	}
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(tr.Context(kimi)); got != 2*n {
		t.Fatalf("the file holds %d messages, want %d (the %d acknowledged turns)", got, 2*n, n)
	}
}
