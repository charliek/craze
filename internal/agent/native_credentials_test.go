package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
)

// A running native session learns the web search keys stored in its
// credentials.toml since it started (plan 051 §F.5): the turn-start look reads
// that file too, whatever the model table's state, and hands every key to the
// harness — for redaction and the frozen-prompt check only (R3: the web
// service keeps the key it opened with). Every case stores the key the way
// `craze auth login websearch/exa` does, modeltable.SetCredential, in the
// fixture's own CRAZE_HOME; each has its control: the same value before it
// was stored, which nothing redacts.

// storeWebKey stores key as the session's Exa key, as craze auth does.
func storeWebKey(t *testing.T, dir, key string) {
	t.Helper()
	if err := modeltable.SetCredential(dir, "exa", key); err != nil {
		t.Fatalf("SetCredential into the session's Home: %v", err)
	}
}

// TestNativeLearnsAWebKeyStoredWhileRunning (F.5): a web search key stored
// while the session runs is raw until the next turn starts, and from then on
// redacted from the tool's row, every event and the turn's transcript lines;
// a rotation to a second key is learned the same way at the turn after —
// with a providers.toml broken in between, since the look does not need the
// model table. The Diag says nothing about the file that read cleanly, and
// never a key.
func TestNativeLearnsAWebKeyStoredWhileRunning(t *testing.T) {
	const (
		stored  = "exa-stored-while-running-0201"
		rotated = "exa-rotated-while-running-0202"
	)
	f := newNativeFixture(t)
	ws := nativeWorkspaceWith(t, map[string]string{"key.txt": "one " + stored + "\ntwo " + rotated + "\n"})
	a := f.models["test/a"]
	a.push(readStep("c1", "key.txt"), answer("saw them"),
		readStep("c2", "key.txt"), answer("saw them again"),
		readStep("c3", "key.txt"), answer("and again"))
	var diag lockedDiag
	s := f.started(Options{Workspace: ws, Diag: &diag})
	noReloads(s)

	if _, err := s.Prompt(context.Background(), "read key.txt"); err != nil {
		t.Fatal(err)
	}
	first := drained(s)
	if len(nativeLeaks(first, stored)) == 0 || len(nativeLeaks(first, rotated)) == 0 {
		t.Fatal("control: the first turn did not show the values raw, so their redaction later proves nothing")
	}
	turnOne := len(transcriptOf(t, f))

	storeWebKey(t, f.dir, stored)
	if _, err := s.Prompt(context.Background(), "read it again"); err != nil {
		t.Fatal(err)
	}
	second := drained(s)
	if content := readOf(t, second, "t2.1.1"); strings.Contains(content, stored) || !strings.Contains(content, "one "+redact.Marker) ||
		!strings.Contains(content, "two "+rotated) {
		t.Fatalf("the second turn's read row = %q; want the stored key redacted and the other value, not yet stored, raw", content)
	}
	if found := nativeLeaks(second, stored); len(found) > 0 {
		t.Fatalf("the stored key reached an event of the next turn at %v", found)
	}
	if lines := transcriptOf(t, f); strings.Contains(strings.Join(lines[turnOne:], "\n"), stored) {
		t.Fatal("the stored key is in the next turn's transcript lines")
	}

	// The model table broken, the key rotated: still learned.
	if err := os.WriteFile(filepath.Join(f.dir, modeltable.ProvidersFile), []byte("version = 1\n[providers.x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeWebKey(t, f.dir, rotated)
	if _, err := s.Prompt(context.Background(), "once more"); err != nil {
		t.Fatal(err)
	}
	third := drained(s)
	if content := readOf(t, third, "t3.1.1"); strings.Contains(content, rotated) || strings.Contains(content, stored) ||
		!strings.Contains(content, "two "+redact.Marker) {
		t.Fatalf("the third turn's read row = %q; want both keys redacted, the rotated one learned with the table broken", content)
	}
	if d := diag.String(); strings.Contains(d, stored) || strings.Contains(d, rotated) || strings.Contains(d, modeltable.CredentialsFile) {
		t.Fatalf("Diag = %q; want nothing said about credentials.toml, which read cleanly, and never a key", d)
	}
}

// TestNativeWebKeyInTheFrozenPromptRefuses (F.5): a web search key stored
// while the session runs that is inside its frozen system prompt — which
// names the working directory — refuses every later turn with the stored-key
// refusal, sending the model nothing; the Diag says why once, naming
// credentials.toml and no key.
func TestNativeWebKeyInTheFrozenPromptRefuses(t *testing.T) {
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

	storeWebKey(t, f.dir, ws)
	before := a.callCount()
	for _, text := range []string{"go on", "/compact"} {
		_, err := s.Prompt(context.Background(), text)
		if err == nil || !errors.Is(err, harness.ErrStoredKeyFrozen) {
			t.Fatalf("Prompt(%q) = %v; want the stored-key refusal", text, err)
		}
		drained(s)
	}
	if n := a.callCount(); n != before {
		t.Fatalf("the refused turns sent %d requests", n-before)
	}
	d := diag.String()
	if strings.Count(d, "a web search key stored in") != 1 || !strings.Contains(d, filepath.Join(f.dir, modeltable.CredentialsFile)) {
		t.Fatalf("Diag = %q; want the refusal said once, naming the file", d)
	}
	if strings.Contains(d, ws) {
		t.Fatal("Diag quotes the key")
	}
}

// TestNativeUnreadableCredentialsFileLearnsNothing (F.5): a credentials.toml
// broken while the session runs — its broken line holding a key — is one
// value-free line on the Diag, said once however many turns see it, naming
// the file and the line, and nothing is learned from it; fixed, it is read
// at the next turn and its key learned. A short stored value is skipped and
// said, by the rule.
func TestNativeUnreadableCredentialsFileLearnsNothing(t *testing.T) {
	const stored = "exa-on-a-broken-line-0203"
	f := newNativeFixture(t)
	ws := nativeWorkspaceWith(t, map[string]string{"key.txt": "the key " + stored + "\n"})
	a := f.models["test/a"]
	a.push(readStep("c1", "key.txt"), answer("one"), readStep("c2", "key.txt"), answer("two"),
		readStep("c3", "key.txt"), answer("three"), answer("four"))
	var diag lockedDiag
	s := f.started(Options{Workspace: ws, Diag: &diag})
	path := filepath.Join(f.dir, modeltable.CredentialsFile)
	if err := os.WriteFile(path, []byte("[exa]\napi_key = \""+stored+"\n"), 0o600); err != nil {
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
	if strings.Contains(d, stored) {
		t.Fatal("Diag quotes the key on the broken line")
	}
	if strings.Count(d, "cannot be read") != 1 || !strings.Contains(d, path) || !strings.Contains(d, "line ") {
		t.Fatalf("Diag = %q; want the broken file said once, naming it and the line", d)
	}

	// craze auth refuses to write over the broken file; it is fixed by hand.
	if err := modeltable.SetCredential(f.dir, "exa", stored); err == nil {
		t.Fatal("SetCredential wrote over a file it cannot read")
	}
	if err := os.WriteFile(path, []byte("[exa]\napi_key = \""+stored+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(context.Background(), "read key.txt"); err != nil {
		t.Fatal(err)
	}
	if content := readOf(t, drained(s), "t3.1.1"); strings.Contains(content, stored) {
		t.Fatalf("the read after the fix = %q; want the key learned", content)
	}

	const short = "zq9-w7"
	if err := os.WriteFile(path, []byte("[exa]\napi_key = \""+short+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if d := diag.String(); strings.Count(d, "[exa] api_key is shorter than 8 bytes") != 1 || strings.Contains(d, short) {
		t.Fatalf("Diag = %q; want the short value said once by the rule, never quoted", d)
	}
}

// TestNativeChildSpawnedAfterLearningAWebKeyRedacts (F.5): a web search key
// stored while the session runs is learned at the next turn's start, and a
// sub-agent that turn starts opens with it: the child's command prints the
// key, enough to spill, and the child's row and its spill file hold only the
// marker. The control is a value in the same output that was never stored.
func TestNativeChildSpawnedAfterLearningAWebKeyRedacts(t *testing.T) {
	const (
		stored  = "exa-stored-before-the-child-0204"
		control = "exa-never-stored-0205"
	)
	f, r := routedNative(t)
	ws := nativeWorkspaceWith(t, map[string]string{"key.txt": strings.Repeat("the key "+stored+" not "+control+"\n", 3000)})
	a := r["test/a"]
	a.route("go", callsStep(agentCall(t, "a1", "look", "child work")), answer("done"))
	a.route("child work", nativeCallStep("k1", "bash", nativeArgs(t, map[string]any{"command": "cat key.txt"})), answer("child done"))
	s := f.started(Options{Workspace: ws})
	storeWebKey(t, f.dir, stored)
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

// TestNativeChildAfterARotationAndReloadKeepsTheStartupKey (F.5, review C5
// item 1): the key credentials.toml stored when the session opened (A) is one
// the session knows from Open; rotated to B before the first turn, and the
// table reloaded at that turn's start (models.toml changed), the new table
// holds B alone and the look learns B alone. A sub-agent that turn opens
// still starts with A: its command printing A shows the marker in its row,
// its spill file and its next request. The control is a value never stored.
func TestNativeChildAfterARotationAndReloadKeepsTheStartupKey(t *testing.T) {
	const (
		startup = "exa-stored-at-open-0210"
		rotated = "exa-rotated-before-turn-one-0211"
		control = "exa-never-stored-0212"
	)
	f, r := routedNative(t)
	ws := nativeWorkspaceWith(t, map[string]string{"key.txt": strings.Repeat("the key "+startup+" not "+control+"\n", 3000)})
	a := r["test/a"]
	a.route("go", callsStep(agentCall(t, "a1", "look", "child work")), answer("done"))
	a.route("child work", nativeCallStep("k1", "bash", nativeArgs(t, map[string]any{"command": "cat key.txt"})), answer("child done"))
	storeWebKey(t, f.dir, startup)
	s := f.started(Options{Workspace: ws})
	if s.hs.Redact(startup) == startup {
		t.Fatal("control: the session's Open does not know the stored key")
	}
	storeWebKey(t, f.dir, rotated)
	mpath := filepath.Join(f.dir, modeltable.ModelsFile)
	models, err := os.ReadFile(mpath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mpath, append(models, "\n# changed, so the table reloads\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	evs := drained(s)
	id := spawnedWith(t, evs, "child work")
	rows := 0
	for _, ev := range evs {
		if ev.Type == EventTool && ev.Agent == id {
			rows++
		}
	}
	if rows == 0 {
		t.Fatal("no tool row of the child's")
	}
	if found := nativeLeaks(evs, startup); len(found) > 0 {
		t.Fatalf("the startup key reached an event at %v", found)
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
		if bytes.Contains(b, []byte(startup)) || !bytes.Contains(b, []byte(redact.Marker+" not "+control)) {
			t.Fatalf("the spill file %s: want the startup key redacted and the unstored value raw", filepath.Base(p))
		}
	}
	childReqs := 0
	for _, c := range a.requests() {
		if firstUserPrompt(c) != "child work" {
			continue
		}
		childReqs++
		b, err := json.Marshal(c.Prompt)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(startup)) {
			t.Fatalf("the child's request %d carries the startup key", childReqs)
		}
	}
	if childReqs < 2 {
		t.Fatalf("%d child requests; want its next request after the command", childReqs)
	}
}

// TestNativeCredentialsDamagedAfterTheLoadIsSaid (review C5 item 6): the
// table loads a credentials.toml that reads cleanly, and the file is broken
// right after (before what the load said is recorded). The load said nothing
// of it, so the first turn's look says it — once, naming the file and the
// line, never the value — rather than taking it as already said.
func TestNativeCredentialsDamagedAfterTheLoadIsSaid(t *testing.T) {
	const stored = "exa-broken-after-the-load-0213"
	f := newNativeFixture(t)
	storeWebKey(t, f.dir, stored)
	path := filepath.Join(f.dir, modeltable.CredentialsFile)
	var diag lockedDiag
	s := f.session(Options{Diag: &diag})
	s.loadedSeam = func() {
		if err := os.WriteFile(path, []byte("[exa]\napi_key = \""+stored+"\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	takeStartDelta(t, s.log)
	if d := diag.String(); strings.Contains(d, modeltable.CredentialsFile) {
		t.Fatalf("control: the load said something of the file it read cleanly: %q", d)
	}
	a := f.models["test/a"]
	a.push(answer("one"), answer("two"))
	for _, text := range []string{"hello", "again"} {
		if _, err := s.Prompt(context.Background(), text); err != nil {
			t.Fatal(err)
		}
		drained(s)
	}
	d := diag.String()
	if strings.Contains(d, stored) {
		t.Fatal("Diag quotes the key on the broken line")
	}
	if strings.Count(d, path+" cannot be read") != 1 || !strings.Contains(d, "line 2") {
		t.Fatalf("Diag = %q; want the file broken after the load said once, naming it and the line", d)
	}
}

// TestNativeCredentialsShapeProblemIsSaid (review C5 item 6): an [exa]
// api_key replaced by an array while the session runs is no key, and the
// look says so — once, value-free, naming the file and the key — however
// many turns and rewrites see it; one the load already said at Open is not
// said again by the first turn's look.
func TestNativeCredentialsShapeProblemIsSaid(t *testing.T) {
	const stored = "exa-replaced-by-an-array-0214"
	const want = "[exa] api_key: not a string; it is ignored"
	f := newNativeFixture(t)
	storeWebKey(t, f.dir, stored)
	path := filepath.Join(f.dir, modeltable.CredentialsFile)
	var diag lockedDiag
	s := f.started(Options{Diag: &diag})
	a := f.models["test/a"]
	a.push(answer("one"), answer("two"), answer("three"), answer("four"))
	if _, err := s.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	drained(s)
	for i, body := range []string{"[exa]\napi_key = [\"" + stored + "\"]\n", "[exa]\napi_key = [\"" + stored + "\"]\nregion = \"eu\"\n"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Prompt(context.Background(), "turn"); err != nil {
			t.Fatal(err)
		}
		drained(s)
		d := diag.String()
		if strings.Contains(d, stored) {
			t.Fatal("Diag quotes the value")
		}
		if strings.Count(d, want) != 1 || !strings.Contains(d, path) {
			t.Fatalf("after rewrite %d Diag = %q; want the array said once, naming the file and the key", i+1, d)
		}
	}

	// Said at Open by the load: not again by the first turn's look.
	f2 := newNativeFixture(t)
	path2 := filepath.Join(f2.dir, modeltable.CredentialsFile)
	if err := os.WriteFile(path2, []byte("[exa]\napi_key = 12345678\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var diag2 lockedDiag
	s2 := f2.started(Options{Diag: &diag2})
	f2.models["test/a"].push(answer("one"))
	if _, err := s2.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	drained(s2)
	if d := diag2.String(); strings.Count(d, want) != 1 {
		t.Fatalf("Diag = %q; want the shape the load said at Open said once", d)
	}
}

// TestNativeWakeLearnsAWebKeyFirst (F.5): a web search key stored while a
// background child runs — a live child — and quoted in that child's answer
// reaches the wake's request as the marker: the wake's look learns it before
// the result is reserved and redacted. The control is a value never stored.
func TestNativeWakeLearnsAWebKeyFirst(t *testing.T) {
	const (
		stored  = "exa-stored-while-a-child-ran-0206"
		control = "exa-never-stored-0207"
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

	storeWebKey(t, rig.f.dir, stored)
	rig.finish(child, id)
	rig.awaitDecided(true, "the result's publication")
	rig.bracket(false, 1, "wake-1")
	reqs := rig.resultRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests delivered a result; want the wake's one", len(reqs))
	}
	delivered := lastUserTexts(t, reqs[0])
	if !strings.Contains(delivered, control) {
		t.Fatalf("control: the delivered result does not carry the unstored value: %q", delivered)
	}
	if strings.Contains(delivered, stored) || !strings.Contains(delivered, redact.Marker+" and "+control) {
		t.Fatalf("the wake delivered %q; want the stored key redacted", delivered)
	}
}

// TestNativeStartsPastABrokenCredentialsFile (F.5): a credentials.toml that
// is not valid TOML — its broken line holding a key — does not stop a
// session: Start says so once on the Diag, naming the file and the line, and
// the turn runs. The value is in no line the session said, no event, and no
// file it wrote: every file under the craze directory but credentials.toml
// is scanned.
func TestNativeStartsPastABrokenCredentialsFile(t *testing.T) {
	const secret = "exa-on-a-broken-line-at-start-0209"
	f := newNativeFixture(t)
	path := filepath.Join(f.dir, modeltable.CredentialsFile)
	if err := os.WriteFile(path, []byte("[exa]\napi_key = \""+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var diag lockedDiag
	s := f.started(Options{Diag: &diag})
	f.models["test/a"].push(answer("fine"))
	if res, err := s.Prompt(context.Background(), "hello"); err != nil || res.StopReason != harness.StopEndTurn {
		t.Fatalf("Prompt = %+v, %v; want the turn to run", res, err)
	}
	evs := drained(s)
	d := diag.String()
	if strings.Contains(d, secret) {
		t.Fatal("Diag quotes the key on the broken line")
	}
	if strings.Count(d, path) != 1 || !strings.Contains(d, "line 2: not valid TOML") {
		t.Fatalf("Diag = %q; want the broken file named once, with its line", d)
	}
	if found := nativeLeaks(evs, secret); len(found) > 0 {
		t.Fatalf("the key reached an event at %v", found)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	home := filepath.Dir(f.dir)
	if err := filepath.WalkDir(home, func(p string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() || p == path {
			return err
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(secret)) {
			t.Errorf("the key is in %s", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestNativeResumedSessionKnowsTheStoredWebKey (F.5): a session resumed after
// a web search key was stored has it in its redactor from Open — the table
// it loads reads credentials.toml — so its first turn's tool output shows
// the marker. The control is the first session's read, before the store.
func TestNativeResumedSessionKnowsTheStoredWebKey(t *testing.T) {
	const stored = "exa-stored-before-a-resume-0208"
	f := newNativeFixture(t)
	ws := nativeWorkspaceWith(t, map[string]string{"key.txt": "the key " + stored + "\n"})
	a := f.models["test/a"]
	a.push(readStep("c1", "key.txt"), answer("one"))
	s := f.started(Options{Workspace: ws})
	if _, err := s.Prompt(context.Background(), "read key.txt"); err != nil {
		t.Fatal(err)
	}
	if content := readOf(t, drained(s), "t1.1.1"); !strings.Contains(content, stored) {
		t.Fatalf("control: the first session's read = %q; want the value raw", content)
	}
	id := s.Snapshot().SessionID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	storeWebKey(t, f.dir, stored)
	resumed := f.session(Options{Workspace: ws, LoadSessionID: id})
	if _, err := startLoad(t, resumed); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := resumed.hs.Redact(stored); got == stored {
		t.Fatal("the resumed session's Open does not know the stored key")
	}
	a.push(readStep("c2", "key.txt"), answer("two"))
	if _, err := resumed.Prompt(context.Background(), "read key.txt"); err != nil {
		t.Fatal(err)
	}
	evs := drained(resumed)
	if found := nativeLeaks(evs, stored); len(found) > 0 {
		t.Fatalf("the stored key reached an event of the resumed session at %v", found)
	}
}
