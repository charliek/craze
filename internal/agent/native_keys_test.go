package agent

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
)

// A running native session learns the keys stored in its providers.toml since
// it started (plan 031 §3.8, §7 A7): the adapter looks at the file at every
// turn's start — a prompt, a /compact, a wake — and hands what it finds to the
// harness. Every case writes the file by hand, the way an editor or another
// craze would, in the fixture's own CRAZE_HOME (never the real one), with a
// dummy key of at least MinKeyLen bytes that shares no text with the marker.
// Each has its control: a value that was not stored, or the same value before
// it was, which nothing redacts.

// lockedDiag is a Diag a test reads while the session may still write to it:
// a wake's look runs on the worker's goroutine.
type lockedDiag struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (d *lockedDiag) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.b.Write(p)
}

func (d *lockedDiag) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.b.String()
}

// withStoredKey is dir's providers.toml with key as provider's inline api_key,
// the section found by its header as the fixture's Save writes it.
func withStoredKey(t *testing.T, dir, provider, key string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, modeltable.ProvidersFile))
	if err != nil {
		t.Fatal(err)
	}
	header := []byte("[providers." + provider + "]\n")
	if !bytes.Contains(b, header) {
		t.Fatalf("no %s section in the providers file", header)
	}
	return bytes.Replace(b, header, append(header, []byte(`api_key = "`+key+"\"\n")...), 1)
}

// storeInPlace stores key by rewriting providers.toml through its own inode,
// as an editor that writes in place does: only the size and the modification
// time say it changed.
func storeInPlace(t *testing.T, dir, provider, key string) {
	t.Helper()
	b := withStoredKey(t, dir, provider, key)
	if err := os.WriteFile(filepath.Join(dir, modeltable.ProvidersFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// storeByRename stores key by writing a new file and renaming it over
// providers.toml, as craze's own atomic writes do: a new inode.
func storeByRename(t *testing.T, dir, provider, key string) {
	t.Helper()
	b := withStoredKey(t, dir, provider, key)
	tmp := filepath.Join(dir, "providers.toml.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, modeltable.ProvidersFile)); err != nil {
		t.Fatal(err)
	}
}

// sentAt is the text the request at index i ends with, which must be a user
// message: for the first request of a turn, its prompt as it went out.
func sentAt(t *testing.T, m *scriptedModel, i int) string {
	t.Helper()
	reqs := m.requests()
	if i >= len(reqs) {
		t.Fatalf("the model was sent %d requests; want one at %d", len(reqs), i)
	}
	return lastUserTexts(t, reqs[i])
}

// readOf is what the read row id shows: its output's content, which is what
// an expanded native row draws.
func readOf(t *testing.T, evs []Event, id string) string {
	t.Helper()
	row := rowByID(t, rowsOf(evs), id)
	if row.Output == nil {
		t.Fatalf("the read row %s has no output: %+v", id, row)
	}
	return row.Output.Content
}

// TestNativeLearnsAKeyStoredWhileRunning (A7): a key written into the
// session's providers.toml while it runs is a value like any other until the
// next turn starts; from then on it is redacted from the tool's row, every
// event and the turn's transcript lines. (What a person types is sent as
// typed, by design — Session.Redact's doc; what craze assembles, a command's
// body or a /compact focus, goes through hs.Redact after the look:
// TestNativeCompactLearnsFirst.) A second key, stored by renaming a
// new file over the first, is learned the same way at the turn after. The
// controls: the first turn, which read both values raw, and the Diag, which
// says nothing (nothing went wrong) and never a key.
func TestNativeLearnsAKeyStoredWhileRunning(t *testing.T) {
	const (
		stored  = "sk-stored-by-hand-0101"
		renamed = "sk-stored-by-rename-0102"
	)
	f := newNativeFixture(t)
	ws := nativeWorkspaceWith(t, map[string]string{"key.txt": "one " + stored + "\ntwo " + renamed + "\n"})
	a := f.models["test/a"]
	a.push(readStep("c1", "key.txt"), answer("saw them"),
		readStep("c2", "key.txt"), answer("saw them again"),
		readStep("c3", "key.txt"), answer("and again"))
	var diag lockedDiag
	s := f.started(Options{Workspace: ws, Diag: &diag})

	if _, err := s.Prompt(context.Background(), "read key.txt"); err != nil {
		t.Fatal(err)
	}
	first := drained(s)
	if len(nativeLeaks(first, stored)) == 0 || len(nativeLeaks(first, renamed)) == 0 {
		t.Fatal("control: the first turn did not show the values raw, so their redaction later proves nothing")
	}
	turnOne := len(transcriptOf(t, f))

	storeInPlace(t, f.dir, "nokey", stored)
	if _, err := s.Prompt(context.Background(), "read it again"); err != nil {
		t.Fatal(err)
	}
	second := drained(s)
	if content := readOf(t, second, "t2.1.1"); strings.Contains(content, stored) || !strings.Contains(content, "one "+redact.Marker) ||
		!strings.Contains(content, "two "+renamed) {
		t.Fatalf("the second turn's read row = %q; want the stored key redacted and the other value, not yet stored, raw", content)
	}
	if found := nativeLeaks(second, stored); len(found) > 0 {
		t.Fatalf("the stored key reached an event of the next turn at %v", found)
	}
	lines := transcriptOf(t, f)
	if strings.Contains(strings.Join(lines[turnOne:], "\n"), stored) {
		t.Fatal("the stored key is in the next turn's transcript lines")
	}

	storeByRename(t, f.dir, "other", renamed)
	if _, err := s.Prompt(context.Background(), "once more"); err != nil {
		t.Fatal(err)
	}
	third := drained(s)
	if content := readOf(t, third, "t3.1.1"); strings.Contains(content, renamed) ||
		strings.Contains(content, stored) || !strings.Contains(content, "two "+redact.Marker) {
		t.Fatalf("the third turn's read row = %q; want both stored keys redacted", content)
	}
	if found := nativeLeaks(third, renamed); len(found) > 0 {
		t.Fatalf("the renamed-in key reached an event at %v", found)
	}
	if d := diag.String(); strings.Contains(d, stored) || strings.Contains(d, renamed) || strings.Contains(d, "providers.toml") {
		t.Fatalf("Diag = %q; want nothing said about a file that read cleanly, and never a key", d)
	}
}

// TestNativeStoredKeyInTheFrozenPromptRefuses (r2-2, A7): a stored key that
// is inside the session's frozen system prompt — which names the working
// directory — refuses every later turn with one fixed sentence, a prompt and
// a /compact alike, sending the model nothing; the Diag says why once, naming
// the file and no key.
func TestNativeStoredKeyInTheFrozenPromptRefuses(t *testing.T) {
	f := newNativeFixture(t)
	ws := t.TempDir()
	var diag lockedDiag
	s := f.started(Options{Workspace: ws, Diag: &diag})
	a := f.models["test/a"]
	a.push(answer("hi"))
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("control: the first turn = %v", err)
	}
	drained(s)
	if err := modeltable.KeyProblem(ws); err != nil {
		t.Fatalf("the workspace path cannot be a key (%v), so the case proves nothing", err)
	}

	storeInPlace(t, f.dir, "nokey", ws)
	const want = "native: a newly stored API key appears in this session's frozen prompt; start a new session"
	before := a.callCount()
	for _, text := range []string{"go on", "and again", "/compact"} {
		_, err := s.Prompt(context.Background(), text)
		if err == nil || err.Error() != want || !errors.Is(err, harness.ErrStoredKeyFrozen) {
			t.Fatalf("Prompt(%q) = %v; want the refusal %q", text, err, want)
		}
		if e := endings(t, drained(s), ""); e == nil || e.Error() != want {
			t.Fatalf("Prompt(%q)'s EventError = %v; want the refusal", text, e)
		}
	}
	if n := a.callCount(); n != before {
		t.Fatalf("the refused turns sent %d requests", n-before)
	}
	d := diag.String()
	if strings.Count(d, "appears in its frozen prompt") != 1 || !strings.Contains(d, filepath.Join(f.dir, modeltable.ProvidersFile)) {
		t.Fatalf("Diag = %q; want the refusal said once, naming the file", d)
	}
	if strings.Contains(d, ws) {
		t.Fatal("Diag quotes the key")
	}
}

// TestNativeSkipsAStoredValueThatCannotBeAKey (r2-3, A7): a stored value
// under MinKeyLen is no key — it could never be sent, and a new session will
// refuse the file — so the session skips it and says so once, on the Diag,
// naming the provider and the rule and never the value, and the turn runs.
func TestNativeSkipsAStoredValueThatCannotBeAKey(t *testing.T) {
	const short = "zq9-w7"
	f := newNativeFixture(t)
	var diag lockedDiag
	s := f.started(Options{Diag: &diag})
	f.models["test/a"].push(answer("one"), answer("two"))
	storeInPlace(t, f.dir, "nokey", short)
	for _, text := range []string{"hello", "again"} {
		if res, err := s.Prompt(context.Background(), text); err != nil || res.StopReason != harness.StopEndTurn {
			t.Fatalf("Prompt(%q) = %+v, %v; want the turn to run", text, res, err)
		}
	}
	d := diag.String()
	if strings.Count(d, `provider "nokey": its api_key is shorter than 8 bytes`) != 1 {
		t.Fatalf("Diag = %q; want the skipped value said once, naming its provider and the rule", d)
	}
	if strings.Contains(d, short) {
		t.Fatal("Diag quotes the skipped value")
	}
	if got := s.hs.Redact("x " + short); got != "x "+short {
		t.Fatalf("the skipped value is redacted (%q): it was learned", got)
	}
}

// TestNativeUnreadableProvidersFileLearnsNothing: a providers.toml broken
// while the session runs — here with a key on its broken line — is one
// value-free line on the Diag, said once however many turns see it, and
// nothing is learned from it (the key on it stays raw); fixed, it is read at
// the next turn and its key learned.
func TestNativeUnreadableProvidersFileLearnsNothing(t *testing.T) {
	const stored = "sk-on-a-broken-line-0104"
	f := newNativeFixture(t)
	ws := nativeWorkspaceWith(t, map[string]string{"key.txt": "the key " + stored + "\n"})
	a := f.models["test/a"]
	a.push(readStep("c1", "key.txt"), answer("one"), readStep("c2", "key.txt"), answer("two"),
		readStep("c3", "key.txt"), answer("three"))
	var diag lockedDiag
	s := f.started(Options{Workspace: ws, Diag: &diag})
	good := withStoredKey(t, f.dir, "nokey", stored)
	broken := bytes.Replace(good, []byte(stored+`"`), []byte(stored), 1) // an unterminated string
	path := filepath.Join(f.dir, modeltable.ProvidersFile)
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"t1.1.1", "t2.1.1"} {
		if _, err := s.Prompt(context.Background(), "read key.txt"); err != nil {
			t.Fatal(err)
		}
		if content := readOf(t, drained(s), id); !strings.Contains(content, stored) {
			t.Fatalf("turn %d's read = %q; want the key raw: nothing is learned from a broken file", i+1, content)
		}
	}
	d := diag.String()
	if strings.Count(d, "cannot be read") != 1 || !strings.Contains(d, path) || !strings.Contains(d, "line ") {
		t.Fatalf("Diag = %q; want the broken file said once, naming it and the line", d)
	}
	if strings.Contains(d, stored) {
		t.Fatal("Diag quotes the key on the broken line")
	}

	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(context.Background(), "read key.txt"); err != nil {
		t.Fatal(err)
	}
	if content := readOf(t, drained(s), "t3.1.1"); strings.Contains(content, stored) {
		t.Fatalf("the read after the fix = %q; want the key learned", content)
	}
	if strings.Count(diag.String(), "cannot be read") != 1 {
		t.Fatal("the fixed file was reported again")
	}
}

// TestNativeLearnsFromTheHomeItOpened (panel astra 12, A7): the look reads
// the providers.toml of the Home the harness was opened with — here a
// directory of the seam's own, not paths.NativeDir() — so a key stored there
// is learned and one stored under paths.NativeDir() is not; and nothing of
// the session lands under paths.NativeDir(): the only change there is the
// test's own write.
func TestNativeLearnsFromTheHomeItOpened(t *testing.T) {
	const (
		mine      = "sk-stored-in-its-own-home-0105"
		elsewhere = "sk-stored-in-the-other-home-0106"
	)
	f := newNativeFixture(t)
	own := filepath.Join(t.TempDir(), "native")
	if err := modeltable.Save(own, nativeTestTable("http://127.0.0.1:9/v1")); err != nil {
		t.Fatal(err)
	}
	f.edit = func(o *harness.Options) { o.Home = own }
	ws := nativeWorkspaceWith(t, map[string]string{"key.txt": "mine " + mine + "\nelsewhere " + elsewhere + "\n"})
	f.models["test/a"].push(readStep("c1", "key.txt"), answer("one"))
	tree := func() []string {
		var out []string
		_ = filepath.WalkDir(f.dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil {
				out = append(out, p)
			}
			return nil
		})
		return out
	}
	before := tree()
	s := f.started(Options{Workspace: ws})
	storeInPlace(t, own, "nokey", mine)
	storeInPlace(t, f.dir, "nokey", elsewhere)
	if _, err := s.Prompt(context.Background(), "read key.txt"); err != nil {
		t.Fatal(err)
	}
	content := readOf(t, drained(s), "t1.1.1")
	if strings.Contains(content, mine) || !strings.Contains(content, "elsewhere "+elsewhere) {
		t.Fatalf("the read = %q; want the own home's key learned and the other home's not", content)
	}
	if after := tree(); !slices.Equal(after, before) {
		t.Fatalf("the session wrote under paths.NativeDir(): %v, was %v", after, before)
	}
	if got, _ := filepath.Glob(filepath.Join(own, "sessions", "*", "*.jsonl")); len(got) != 1 {
		t.Fatalf("%d transcripts in the session's own home; want its one", len(got))
	}
}

// TestNativeLookRecordsTheStampFromBeforeTheReading: a write that lands
// between the look's reading and its recording of the stamp — held open with
// the seam — must leave the recorded stamp older than the file, so the next
// look reads again and learns the key the write stored. A look that recorded
// the file's stamp after reading it would record the write's and never read
// it: the key would stay raw for the rest of the session.
func TestNativeLookRecordsTheStampFromBeforeTheReading(t *testing.T) {
	const late = "sk-stored-mid-look-0107"
	f := newNativeFixture(t)
	ws := nativeWorkspaceWith(t, map[string]string{"key.txt": "late " + late + "\n"})
	f.models["test/a"].push(readStep("c1", "key.txt"), answer("one"), readStep("c2", "key.txt"), answer("two"))
	s := f.started(Options{Workspace: ws})
	var once sync.Once
	s.keysSeam = func() { once.Do(func() { storeInPlace(t, f.dir, "nokey", late) }) }

	if _, err := s.Prompt(context.Background(), "read key.txt"); err != nil {
		t.Fatal(err)
	}
	if content := readOf(t, drained(s), "t1.1.1"); !strings.Contains(content, late) {
		t.Fatalf("control: the first turn's read = %q; the key was written after its look read the file", content)
	}
	if _, err := s.Prompt(context.Background(), "read key.txt"); err != nil {
		t.Fatal(err)
	}
	if content := readOf(t, drained(s), "t2.1.1"); strings.Contains(content, late) {
		t.Fatalf("the next turn's read = %q; want the key written mid-look learned", content)
	}
}

// TestNativeCompactLearnsFirst: /compact is a turn: its look comes before its
// focus is redacted, so a stored key quoted in `/compact <focus>` reaches the
// summarizer as the marker.
func TestNativeCompactLearnsFirst(t *testing.T) {
	const stored = "sk-quoted-in-a-focus-0108"
	f := newNativeFixture(t)
	s := f.started(Options{})
	a := f.models["test/a"]
	a.push(answer("hi there"))
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	storeInPlace(t, f.dir, "nokey", stored)
	a.push(nativeSummary("The user said hello."))
	before := a.callCount()
	if _, err := s.Prompt(context.Background(), "/compact keep "+stored); err != nil {
		t.Fatalf("/compact: %v", err)
	}
	if sum := sentAt(t, a, before); strings.Contains(sum, stored) || !strings.Contains(sum, "keep "+redact.Marker) {
		t.Fatalf("the summarizer's request ends %q; want the focus with the stored key redacted", sum[max(0, len(sum)-200):])
	}
}

// TestNativeWakeLearnsFirst (A7): a wake is a turn: its look comes before it
// takes the pending results, which it redacts as it reserves them — so a key
// stored while a background child ran, and quoted in that child's answer,
// reaches the wake's request as the marker. The control is a value in the
// same answer that was never stored: raw.
func TestNativeWakeLearnsFirst(t *testing.T) {
	const (
		stored  = "sk-stored-while-a-child-ran-0109"
		control = "sk-never-stored-0110"
	)
	rig := newWakeRig(t, Options{}, nil)
	child := newHeld(t)
	rig.a.route("go", callsStep(bgCall(t, "a1", "job", "child work")), answer("started"), answer("delivered"))
	rig.a.route("child work", child.step(openTextParts("found "), closeTextParts(stored+" and "+control)))
	if _, err := rig.s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	rig.w.waitType(EventDone)
	rig.awaitDecided(false, "the turn's release, the child still running")
	id := spawnedWith(t, rig.w.events(), "child work")

	storeInPlace(t, rig.f.dir, "nokey", stored)
	rig.finish(child, id)
	rig.awaitDecided(true, "the result's publication")
	rig.bracket(false, 1, "wake-1")
	reqs := rig.resultRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests delivered a result; want the wake's one", len(reqs))
	}
	delivered := lastUserTexts(t, reqs[0])
	if !strings.Contains(delivered, control) {
		t.Fatalf("control: the delivered result does not carry the unstored value, so the redaction proves nothing: %q", delivered)
	}
	if strings.Contains(delivered, stored) || !strings.Contains(delivered, redact.Marker+" and "+control) {
		t.Fatalf("the wake delivered %q; want the stored key redacted", delivered)
	}
}

// TestNativeChildSpawnedAfterLearningRedacts (r2-1, A7): a key stored by hand
// is learned at the turn's start, and a sub-agent that turn starts opens with
// it: the child's own command prints the key — enough of it to spill — and
// the child's tool row and its spill file hold only the marker. The control
// is a value in the same output that was never stored: raw in the spill file.
func TestNativeChildSpawnedAfterLearningRedacts(t *testing.T) {
	const (
		stored  = "sk-stored-before-the-child-0111"
		control = "sk-never-stored-0112"
	)
	f, r := routedNative(t)
	ws := nativeWorkspaceWith(t, map[string]string{"key.txt": strings.Repeat("the key "+stored+" not "+control+"\n", 3000)})
	a := r["test/a"]
	a.route("go", callsStep(agentCall(t, "a1", "look", "child work")), answer("done"))
	a.route("child work", nativeCallStep("k1", "bash", nativeArgs(t, map[string]any{"command": "cat key.txt"})), answer("child done"))
	s := f.started(Options{Workspace: ws})
	storeInPlace(t, f.dir, "nokey", stored)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs := drained(s)
	id := spawnedWith(t, evs, "child work")
	var rows []Event
	for _, ev := range evs {
		if ev.Type == EventTool && ev.Agent == id {
			rows = append(rows, ev)
		}
	}
	if len(rows) == 0 {
		t.Fatal("no tool row of the child's")
	}
	if found := nativeLeaks(rows, stored); len(found) > 0 {
		t.Fatalf("the child's tool row holds the stored key at %v", found)
	}
	if found := nativeLeaks(evs, stored); len(found) > 0 {
		t.Fatalf("the stored key reached an event at %v", found)
	}
	spills, err := filepath.Glob(filepath.Join(f.dir, "tool-output", "*"))
	if err != nil || len(spills) == 0 {
		t.Fatalf("no spill file (%v), so the child's is not checked", err)
	}
	for _, p := range spills {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(stored)) || !bytes.Contains(b, []byte(redact.Marker+" not "+control)) {
			t.Fatalf("the spill file %s: want the stored key redacted and the unstored value raw", filepath.Base(p))
		}
	}
}

// TestStampOfSeesEachKindOfChange: the stamp is size, modification time and
// inode, and each alone tells a change: a rewrite in place to another size, a
// rewrite in place to the same size at another time, and a rename of a new
// file over it with the same size and the same time — a new inode. The file
// untouched keeps its stamp.
func TestStampOfSeesEachKindOfChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, modeltable.ProvidersFile)
	write := func(p, body string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	stamp := func() fileStamp {
		t.Helper()
		st, err := stampOf(path)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	write(path, "aaaa", at)
	first := stamp()
	if first.ino == 0 || stamp() != first {
		t.Fatalf("the stamp of an untouched file changed, or has no inode: %+v", first)
	}

	write(path, "aaaaa", at)
	if st := stamp(); st == first || st.size == first.size {
		t.Fatalf("a rewrite to another size kept the stamp: %+v", st)
	}
	write(path, "aaaa", at)
	if st := stamp(); st != first {
		t.Fatalf("control: the same inode, size and time is not the same stamp: %+v, was %+v", st, first)
	}
	write(path, "bbbb", at.Add(time.Second))
	if st := stamp(); st == first || st.ino != first.ino || st.size != first.size {
		t.Fatalf("a same-size rewrite at another time: %+v, was %+v; want only the time changed", st, first)
	}
	write(path, "aaaa", at)
	other := filepath.Join(dir, "next")
	write(other, "cccc", at)
	if err := os.Rename(other, path); err != nil {
		t.Fatal(err)
	}
	if st := stamp(); st == first || st.ino == first.ino || st.size != first.size || st.mtime != first.mtime {
		t.Fatalf("a rename over it with the same size and time: %+v, was %+v; want only the inode changed", st, first)
	}
	if _, err := stampOf(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stampOf of a missing file = %v; want fs.ErrNotExist", err)
	}
}

// TestNativeRefusedWakeDoesNotLoop (r2-2): a key stored inside the frozen
// prompt while a background child runs is found by the wake that would
// deliver the child's result. That wake is refused — and the one after it
// never claims: the result is still pending in the registry, but a session in
// the refusal state has none to wake for (HasPending), so the worker's
// recheck at the refused wake's ending stands down instead of claiming,
// refusing and kicking itself for ever. The next prompt is refused with the
// same sentence.
func TestNativeRefusedWakeDoesNotLoop(t *testing.T) {
	rig := newWakeRig(t, Options{}, nil)
	child := newHeld(t)
	id := rig.spawnOne(child)

	before := rig.a.requests()
	storeInPlace(t, rig.f.dir, "nokey", rig.s.opts.Workspace)
	rig.finish(child, id)
	rig.awaitDecided(true, "the result's publication")
	rig.bracket(false, 1, "wake-1")
	rig.awaitDecided(false, "the refused wake's ending: nothing to wake for")
	rig.noRecheck("after the refused wake")
	if n := len(rig.a.requests()) - len(before); n != 0 {
		t.Fatalf("the refused wake sent %d requests", n)
	}
	if rig.s.OwesWork() {
		t.Fatal("OwesWork: a refusing session owes a result nothing can deliver, so its host would never go idle")
	}
	_, err := rig.s.Prompt(context.Background(), "and now")
	if err == nil || !errors.Is(err, harness.ErrStoredKeyFrozen) {
		t.Fatalf("the next prompt = %v; want the refusal", err)
	}
}

// TestNativeFirstTurnLearnsAKeyTheMergeDropped: the first turn's look always
// reads the file. An inline key whose entry the merge dropped — a provider of
// the user's own with no driver, ignored with a warning — is in no table, so
// Open never knew it (the control); the first turn learns it, and redacts it
// from its tool output. A look that trusted the file to be the one the table
// was loaded from would leave it raw for the whole session.
func TestNativeFirstTurnLearnsAKeyTheMergeDropped(t *testing.T) {
	const dropped = "sk-in-a-dropped-entry-0113"
	f := newNativeFixture(t)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Over the catalog this time (no `catalog = false`): the merge is what
	// drops the entry. The fixture's environment funds only test/a.
	write(modeltable.ProvidersFile, `version = 1

[providers.test]
driver = "openai-compat"
base_url = "http://127.0.0.1:9/v1"
env_keys = ["NATIVE_TEST_KEY"]

[providers.mine]
api_key = "`+dropped+`"
`)
	write(modeltable.ModelsFile, `version = 1
default_model = "test/a"

[models."test/a"]
provider = "test"
wire_model = "wire-a"
`)
	ws := nativeWorkspaceWith(t, map[string]string{"key.txt": "dropped " + dropped + "\n"})
	f.models["test/a"].push(readStep("c1", "key.txt"), answer("one"))
	var diag lockedDiag
	s := f.started(Options{Workspace: ws, Diag: &diag})
	if !strings.Contains(diag.String(), "the entry is ignored") {
		t.Fatalf("control: the merge did not drop the entry, so the case proves nothing: %q", diag.String())
	}
	if got := s.hs.Redact(dropped); got != dropped {
		t.Fatal("control: Open already knew the dropped entry's key")
	}
	if _, err := s.Prompt(context.Background(), "read key.txt"); err != nil {
		t.Fatal(err)
	}
	if content := readOf(t, drained(s), "t1.1.1"); strings.Contains(content, dropped) || !strings.Contains(content, "dropped "+redact.Marker) {
		t.Fatalf("the first turn's read = %q; want the dropped entry's key learned and redacted", content)
	}
}
