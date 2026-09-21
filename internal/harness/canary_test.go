package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// The secrets canaries (plan 019 §3.8, §7.8): craze's own provider keys must
// not reach the model, an event, the transcript or a spill file verbatim and
// by accident. Three keys are planted — one in the environment (canary,
// TEST_API_KEY's), one only in the environment of another provider
// (canaryOther), one inline in providers.toml — and the model, scripted,
// makes every call the plan names: `env`, `cat providers.toml` (bash may
// read it; redaction must catch it), `cat /proc/self/environ`, a read and an
// edit of a .env holding a key, a grep for a key, a key split across two
// pipe reads, a raw argument holding a key, and output long enough to spill.

const canaryInline = "sk-canary-inline-0001"

// canaryFixture is a fixture whose providers.toml also holds canaryInline,
// inline, for the provider no model's session uses, and whose real
// environment holds the keys where a careless shell would find them.
func canaryFixture(t *testing.T) *fixture {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	path := filepath.Join(f.home, modeltable.ProvidersFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte(`env_keys = ["NOKEY_API_KEY"]`), []byte(`env_keys = ["NOKEY_API_KEY"]`+"\napi_key = \""+canaryInline+"\""), 1)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f.table = f.load()
	// The child's environment is the process's, less the providers' env_keys
	// and OPENAI_*: these two are stripped, and CRAZE_CANARY_COPY — a copy
	// under a name no provider declares — is what `env` then finds.
	t.Setenv("TEST_API_KEY", canary)
	t.Setenv("OPENAI_API_KEY", canaryOther)
	t.Setenv("CRAZE_CANARY_COPY", canary)
	return f
}

func TestSecretsCanaries(t *testing.T) {
	f := canaryFixture(t)
	s := f.open(f.options())
	f.put(".env", "API_KEY="+canary+"\nOTHER=1\n")
	providers := filepath.Join(f.home, modeltable.ProvidersFile)
	bash := func(cmd string) string { return input(t, map[string]any{"command": cmd}) }

	type call struct{ name, tool, in string }
	calls := []call{
		{"env", "bash", bash("env")},
		{"cat providers.toml", "bash", bash("cat " + providers)},
		{"read .env", "read", input(t, map[string]any{"filePath": ".env"})},
		{"grep for the key", "grep", input(t, map[string]any{"pattern": canary, "path": f.workspace})},
		{"edit .env", "edit", input(t, map[string]any{"filePath": ".env", "oldString": "OTHER=1", "newString": "OTHER=2"})},
		{"a key split across two reads", "bash", bash("printf 'sk-canary-'; sleep 0.3; printf 'not-a-secret\\n'")},
		{"a raw argument holding the key", "bash", bash("echo " + canaryOther)},
		{"output that spills", "bash", bash(`for i in $(seq 3000); do echo "$i $CRAZE_CANARY_COPY"; done`)},
	}
	if runtime.GOOS == "linux" {
		calls = append(calls, call{"/proc/self/environ", "bash", bash("cat /proc/self/environ")})
	}
	var steps []step
	for i, c := range calls {
		steps = append(steps, callStep(callParts(fmt.Sprintf("c%d", i), c.tool, c.in)))
	}
	f.models["test/a"].push(append(steps, answerWith("done"))...)

	var ev events
	if res, err := s.Run(context.Background(), "look around", ev.sink); err != nil || res.StopReason != StopEndTurn {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	evs := ev.list()
	fin := of[ToolFinished](evs)
	if len(fin) != len(calls) {
		t.Fatalf("%d calls finished, want %d: %v", len(fin), len(calls), seq(evs))
	}
	haveRG := true
	if _, err := exec.LookPath("rg"); err != nil {
		haveRG = false
	}

	// The controls: each call really met a key, and redaction replaced it.
	for i, c := range calls {
		r := fin[i].Result
		shown := r.Text + r.Content
		for _, e := range r.Edits {
			shown += e.Old + e.New
		}
		if c.name == "grep for the key" && !haveRG {
			continue // the pinned error; the argument is still checked below
		}
		if c.name == "a raw argument holding the key" {
			shown = of[ToolCalled](evs)[i].Request.Input + r.Text
		}
		if !strings.Contains(shown, redact.Marker) {
			t.Errorf("%s: nothing was redacted, so the canary proves nothing: %+v", c.name, r)
		}
	}
	envOut := fin[0].Result.Text
	for _, gone := range []string{"TEST_API_KEY=", "OPENAI_API_KEY="} {
		if strings.Contains(envOut, gone) {
			t.Errorf("the child environment kept %s", gone)
		}
	}
	if !strings.Contains(envOut, "CRAZE_CANARY_COPY="+redact.Marker) {
		t.Errorf("env's output does not show the copy, redacted:\n%s", envOut)
	}
	spill := fin[7].Result.Trunc.Spill
	if spill == "" {
		t.Fatal("the long output did not spill, so the spill file is not checked")
	}

	for _, key := range []string{canary, canaryOther, canaryInline} {
		if found := leaks(evs, key); len(found) > 0 {
			t.Errorf("%s reached an event at %v", key, found)
		}
		b, err := os.ReadFile(s.store.Path())
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(key)) {
			t.Errorf("%s is in the transcript", key)
		}
		files, _ := filepath.Glob(filepath.Join(f.home, tool.SpillDir, "*"))
		if len(files) == 0 {
			t.Fatal("no spill file to check")
		}
		for _, p := range files {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(key)) {
				t.Errorf("%s is in the spill file %s", key, p)
			}
		}
		// Every request, whole: the system prompt, the tools, and every
		// message's text, thinking and results. The one thing left out is
		// what the script itself made the model say — the arguments of its
		// tool calls, which Fantasy replays inside the turn as the model
		// sent them (redactCalls).
		for i, c := range f.models["test/a"].requests() {
			if strings.Contains(requestText(c, false), key) {
				t.Errorf("request %d sent the model %s:\n%s", i+1, key, requestText(c, false))
			}
		}
	}
}

// TestWorkspaceWithAKeyIsRefused: the system prompt names the working
// directory and the header records it, both frozen, and a path cannot be
// redacted and still be a path — so a workspace whose path holds a provider
// key refuses to open, saying so without naming the key. The control is the
// same directory one character different: it opens, and the prompt and the
// header carry the real path.
func TestWorkspaceWithAKeyIsRefused(t *testing.T) {
	for _, planted := range []bool{true, false} {
		t.Run(fmt.Sprintf("planted=%v", planted), func(t *testing.T) {
			dir := canary
			if !planted {
				// One byte different, so the key is not in it at all.
				dir = canary[:len(canary)-1] + "X"
			}
			f := newFixture(t, "http://127.0.0.1:1/v1")
			f.workspace = filepath.Join(f.workspace, dir)
			if err := os.MkdirAll(f.workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			s, err := Open(f.options())
			if planted {
				if err == nil {
					s.Close()
					t.Fatal("a workspace whose path holds a key opened")
				}
				if !errors.Is(err, errWorkspaceKey) || strings.Contains(err.Error(), canary) {
					t.Fatalf("Open = %v; want the refusal, naming no key", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("control: Open = %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			f.put("a.txt", "alpha\n")
			f.models["test/a"].push(answerWith("done"))
			run(t, s, "hi")
			if prompt := promptOf(f.models["test/a"].requests()[0])[0]; !strings.Contains(prompt, "- Working directory: "+f.workspace+"\n") {
				t.Errorf("control: the prompt does not name the real workspace")
			}
			if cwd := transcript(t, s).Header.Cwd; cwd != f.workspace {
				t.Errorf("control: the header's cwd = %q, want the real workspace %q", cwd, f.workspace)
			}
		})
	}
}

// TestPlanPathWithAKeyIsRefused is TestWorkspaceWithAKeyIsRefused's twin for
// the plan file (plan 023 §3.3). Every plan-mode reminder hands the model the
// plan file's absolute path so it can write the plan there, unredacted —
// redacting it would hand the model a path that opens nothing — and the path
// comes from the harness home a user configured. So a home whose plan path
// would hold a provider key refuses to open, saying so without naming the key.
//
// The control is the same home one character different: it opens, and the
// reminder names the real path.
func TestPlanPathWithAKeyIsRefused(t *testing.T) {
	for _, planted := range []bool{true, false} {
		t.Run(fmt.Sprintf("planted=%v", planted), func(t *testing.T) {
			dir := canary
			if !planted {
				// One byte different, so the key is not in it at all.
				dir = canary[:len(canary)-1] + "X"
			}
			f := newFixture(t, "http://127.0.0.1:1/v1")
			home := filepath.Join(t.TempDir(), dir, "native")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			opts := modeOptions(f, "plan")
			opts.Home = home // the table is already loaded; only the files the session writes move
			s, err := Open(opts)
			if planted {
				if err == nil {
					s.Close()
					t.Fatal("a home whose plan path holds a key opened")
				}
				if !errors.Is(err, errPlanPathKey) || strings.Contains(err.Error(), canary) {
					t.Fatalf("Open = %v; want the refusal, naming no key", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("control: Open = %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			f.models["test/a"].push(answerWith("planned"))
			run(t, s, "plan it")
			text, _ := reminderIn(t, f.models["test/a"], 0)
			if !strings.Contains(text, planPathOf(s)) || !strings.Contains(text, home) {
				t.Errorf("control: the reminder does not name the real plan path: %q", text)
			}
		})
	}
}

// keyEnv is the fixture's environment with one variable replaced, for a case
// that plants a key of its own: testEnv is shared by every test in the
// package and must not be written to.
func keyEnv(name, value string) func(string) string {
	return func(want string) string {
		if want == name {
			return value
		}
		return testEnv[want]
	}
}

// TestProfileKeyIsRefused (plan 022, round-3 review): a configured key that
// lies inside the tool profile's own words, or across the join between them
// and the extras rendered after them, refuses the session.
//
// Neither can be redacted. The profile's text is craze's, not a caller's, so
// there is nothing about the session that could be changed to take the key
// out of it; and redacting either rewrites bytes on the profile's side of the
// join, which moves the byte-identical prefix every request of every session
// shares (D-30). Before this, the first case returned the profile unchanged —
// the one path on which a configured key went out verbatim — and the second
// quietly moved the prefix.
func TestProfileKeyIsRefused(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	base := systemPrompt(opencodeProfile(t), filepath.Clean(f.workspace), runtime.GOOS)
	inside, _, _ := strings.Cut(base, "\n")
	if len(inside) < modeltable.MinKeyLen {
		t.Fatalf("the profile's first line is %d bytes, too short to stand in for a key", len(inside))
	}
	// The join: the profile's last bytes and the extras' first, which is the
	// heading withPromptExtras writes one newline after them.
	across := base[len(base)-8:] + "\n" + instructionsHeading[:16]
	docs := PromptExtras{Instructions: []PromptDoc{{Path: "/w/CLAUDE.md", Text: "hello\n"}}}
	for _, tc := range []struct {
		name string
		key  string
		x    PromptExtras
	}{
		{name: "wholly inside the profile, with no extras", key: inside},
		{name: "wholly inside the profile, with extras", key: inside, x: docs},
		{name: "across the profile and the extras", key: across, x: docs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := f.options()
			opts.Getenv = keyEnv("TEST_API_KEY", tc.key)
			opts.Prompt = tc.x
			s, err := Open(opts)
			if err == nil {
				s.Close()
				t.Fatal("a prompt holding the key opened")
			}
			if !errors.Is(err, errProfileKey) || strings.Contains(err.Error(), tc.key) {
				t.Fatalf("Open = %v; want the refusal, naming no key", err)
			}
		})
	}
	// The control: the same session, with a key that is in none of it.
	opts := f.options()
	opts.Prompt = docs
	if got := f.open(opts).system; !strings.HasPrefix(got, base) {
		t.Fatalf("control: the prompt does not begin with the profile's text:\n%s", got)
	}
}

// TestHeaderDigestKeyIsRefused (plan 022, round-3 review): the transcript
// header's SHA-256 of the frozen prompt is disclosed as it is — the header
// holds it, and the adapter's prompt_sources note repeats it, which C6
// requires to be the same string — so a configured key that happens to be
// that digest refuses the session. Redacting one copy would break the
// equality and leave the other, and a digest with a marker in it is not a
// digest.
//
// Contrived by construction: the digest is read off a session that opened
// with an ordinary key, and planted as the key of a second session over the
// same workspace, which is the only way to make the collision happen.
func TestHeaderDigestKeyIsRefused(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	digest := f.open(f.options()).PromptSHA256()
	if len(digest) != 64 {
		t.Fatalf("the header's prompt digest is %q, want 64 hex characters", digest)
	}
	opts := f.options()
	opts.Getenv = keyEnv("TEST_API_KEY", digest)
	s, err := Open(opts)
	if err == nil {
		s.Close()
		t.Fatal("a session whose prompt digest is a configured key opened")
	}
	if !errors.Is(err, errDigestKey) || strings.Contains(err.Error(), digest) {
		t.Fatalf("Open = %v; want the refusal, naming no key", err)
	}
}

// TestToolDescriptionsAreRedacted: bash's description names the machine's
// temporary directory, which comes from the environment and goes to the
// model with every request — nothing else redacts it. The tools the header
// hashes are the same ones, so the two cannot disagree. The control is the
// same session with no key in TMPDIR: the directory arrives whole.
func TestToolDescriptionsAreRedacted(t *testing.T) {
	for _, planted := range []bool{true, false} {
		t.Run(fmt.Sprintf("planted=%v", planted), func(t *testing.T) {
			dir := "plain"
			if planted {
				dir = canary
			}
			f := newFixture(t, "http://127.0.0.1:1/v1")
			tmp := filepath.Join(t.TempDir(), dir)
			if err := os.MkdirAll(tmp, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", tmp)
			s := f.open(f.options())
			f.models["test/a"].push(answerWith("done"))
			run(t, s, "hi")

			tools := fmt.Sprint(f.models["test/a"].requests()[0].Tools)
			if !planted {
				if !strings.Contains(tools, tmp) {
					t.Fatal("control: the tools do not name the temporary directory at all")
				}
				return
			}
			if strings.Contains(tools, canary) {
				t.Errorf("the tools sent hold the key")
			}
			if !strings.Contains(tools, strings.Replace(tmp, canary, redact.Marker, 1)) {
				t.Errorf("the key was not replaced by the marker in place:\n%s", tools)
			}
			// The header's hash is of the tools as they were sent.
			sum := sha256.Sum256(s.tools.wire)
			if got := transcript(t, s).Header.ToolsSHA256; got != hex.EncodeToString(sum[:]) || bytes.Contains(s.tools.wire, []byte(canary)) {
				t.Errorf("the hashed tools are not the redacted ones sent")
			}
		})
	}
}

// TestSwitchLearnsAKeyForTheNextTurn (round-2 and round-3 review): the
// redactor is built at Open from the keys the environment has then, but a
// switch makes current a model whose provider's key may have been exported
// since — from that moment it is craze's credential. The switch resolves it;
// the next turn takes it up, before its first request and before it has a
// step to persist, so one turn is redacted one way from end to end.
//
// The switch here is made in the middle of a turn, which the session allows.
// Two controls, one each way: the turn that was already running keeps the
// redactor it began with (its result, and the step persisted after the
// switch, still show the value craze did not know at the time), and a turn
// before any switch shows it too — so the redaction the third turn does
// cannot be something that was there all along.
func TestSwitchLearnsAKeyForTheNextTurn(t *testing.T) {
	// A key of its own, sharing no text with the one Open knows.
	const later = "sk-exported-later-0002"
	f := newFixture(t, "http://127.0.0.1:1/v1")
	env := map[string]string{"TEST_API_KEY": canary} // other/c's provider has no key yet
	opts := f.options()
	opts.Getenv = func(name string) string { return env[name] }
	s := f.open(opts)
	f.put(".env", "OTHER_API_KEY="+later+"\n")
	readEnv := input(t, map[string]any{"filePath": ".env"})
	g := newGate()
	f.models["test/a"].push(
		callStep(callParts("c1", "read", readEnv)), answerWith("before"), // turn 1
		callStep(callParts("c2", "read", readEnv)), // turn 2's first step
		// Its second step waits, so the switch lands mid-turn, after the
		// read's result and before the step that persists last.
		g.hold(nil, cat(textParts("during"), finish(fantasy.FinishReasonStop))),
	)
	f.models["other/c"].push(callStep(callParts("c3", "read", readEnv)), answerWith("after"))

	// Control 1: a key craze has not resolved is a value like any other.
	var first events
	runWith(t, s, "read it", first.sink)
	if got := of[ToolFinished](first.list())[0].Result.Content; !strings.Contains(got, later) {
		t.Fatalf("control: the unknown key was already redacted, so the switch proves nothing: %q", got)
	}

	// The switch, mid-turn.
	var during events
	out := start(context.Background(), s, "read it again", during.sink)
	await(t, g.reached, "the second turn's last step")
	env["OTHER_API_KEY"] = later
	if err := s.SetModel("other/c"); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	if got := await(t, out, "the second turn"); got.err != nil {
		t.Fatal(got.err)
	}
	// Control 2: that turn ran under the redactor it began with, its steps
	// all of them — the one persisted after the switch included.
	if got := of[ToolFinished](during.list())[0].Result.Content; !strings.Contains(got, later) {
		t.Errorf("control: the running turn's result changed under it: %q", got)
	}
	if lines := entries(transcript(t, s)); !strings.Contains(strings.Join(lines[4:], "\n"), later) {
		t.Errorf("control: the running turn's transcript changed under it:\n%s", strings.Join(lines[4:], "\n"))
	}

	// The next turn, on the model the switch made current: everything it
	// produces redacts the key.
	var after events
	runWith(t, s, "and again", after.sink)
	fin := of[ToolFinished](after.list())[0].Result
	if strings.Contains(fin.Text, later) || strings.Contains(fin.Content, later) {
		t.Errorf("the key the switch resolved reached the result: %+v", fin)
	}
	if !strings.Contains(fin.Content, redact.Marker) {
		t.Errorf("the key was not replaced by the marker: %q", fin.Content)
	}
	if found := leaks(after.list(), later); len(found) > 0 {
		t.Errorf("the key reached an event at %v", found)
	}
	// Nor anywhere in any request the new model saw — this turn's result,
	// and the earlier turns' entries the history replays, which were written
	// before the key was known and are redacted on the way out (the file
	// itself still holds them, above). Nor this turn's transcript lines.
	for i, c := range f.models["other/c"].requests() {
		whole := requestText(c, true)
		if strings.Contains(whole, later) {
			t.Errorf("the new model's request %d holds the key:\n%s", i+1, whole)
		}
		if i == 0 && !strings.Contains(whole, redact.Marker) {
			t.Error("the replayed history carried no redaction, so the earlier turns' entries were not in it")
		}
	}
	// The negative control for that pass: the same replay before the key was
	// known — this turn's own first request — did carry it.
	if !strings.Contains(requestText(f.models["test/a"].requests()[2], true), later) {
		t.Error("control: the replay before the switch did not carry the key, so redacting it proves nothing")
	}
	lines := entries(transcript(t, s))
	if strings.Contains(strings.Join(lines[8:], "\n"), later) {
		t.Errorf("the key is in the transcript's third turn:\n%s", strings.Join(lines[8:], "\n"))
	}

	// A key that cannot be redacted, one inside the prompt, and one inside
	// the tools — a schema's own bytes, which no description holds — each
	// refuses the switch and leaves the model where it was.
	for what, key := range map[string]string{
		"unredactable":  "k3y-x7",
		"in the prompt": f.workspace, // the prompt names the working directory
		"in the tools":  `"required":["filePath"]`,
		// Every plan-mode reminder hands the model the plan file's path, which
		// is fixed at Open and cannot be redacted and still be a path (plan 023
		// §3.3): a switch that would make it hold a key is refused like one
		// that would put a key in the prompt.
		"in the plan path": planPathOf(s),
		// A sentence of bash's own description, taken as it is written rather
		// than rebuilt from the machine: the directory it names is chosen at
		// runtime (os.TempDir()'s spelling differs per platform, and a
		// planted path falls back to a random name), so a hand-built copy of
		// it does not always appear in what is sent.
		"in a description":  "for temporary work outside the workspace",
		"nowhere in either": "sk-not-in-what-is-sent-0003",
	} {
		env["NOKEY_API_KEY"] = key
		err := s.SetModel("test/a")
		switch {
		case what == "unredactable":
			if !errors.Is(err, modeltable.ErrKeyTooShort) {
				t.Errorf("a switch with an %s key = %v, want the floor's refusal", what, err)
			}
		case what == "nowhere in either": // the control: this one is allowed
			if err != nil {
				t.Errorf("control: a switch with a key %s = %v, want it allowed", what, err)
			}
		case !errors.Is(err, errFrozenKey):
			t.Errorf("a switch with a key %s = %v, want errFrozenKey", what, err)
		}
	}
}

// runWith is run with a sink.
func runWith(t *testing.T, s *Session, text string, sink func(Event)) {
	t.Helper()
	if _, err := s.Run(context.Background(), text, sink); err != nil {
		t.Fatalf("Run(%q): %v", text, err)
	}
}

// TestSessionRedactIsTheCallersGuard (plan 022 §3.3): Run persists and sends
// the text it is given exactly as given — the right rule for a prompt a
// person typed, and the wrong one for text an adapter assembled out of files
// on disk. Session.Redact is what such a caller runs it through first.
//
// The control is the same turn without it: the key really does reach the
// transcript, so the redaction below is doing something.
func TestSessionRedactIsTheCallersGuard(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())

	if got := s.Redact("export KEY=" + canary + " # done"); strings.Contains(got, canary) {
		t.Fatalf("Redact left the key in %q", got)
	}
	if got := s.Redact("nothing secret here"); got != "nothing secret here" {
		t.Fatalf("Redact changed text holding no key: %q", got)
	}

	f.models["test/a"].push(answerWith("one"), answerWith("two"))
	runWith(t, s, "export KEY="+canary, nil)
	if lines := entries(transcript(t, s)); !strings.Contains(strings.Join(lines, "\n"), canary) {
		t.Fatalf("control: Run redacted the caller's prompt after all:\n%s", strings.Join(lines, "\n"))
	}
	runWith(t, s, s.Redact("export KEY="+canary), nil)
	lines := entries(transcript(t, s))
	if strings.Count(strings.Join(lines, "\n"), canary) != 1 {
		t.Fatalf("the redacted prompt put the key in the transcript too:\n%s", strings.Join(lines, "\n"))
	}
}

// TestRedactCoversAKeyTheSwitchJustLearned (round-2 review of plan 022): a
// switch resolves a provider key the environment gained since Open and leaves
// it prepared for the next turn to adopt (toolset.resolve, adopt). Between
// those two moments the installed replacer is the narrower one, so a caller
// that expanded a command file, ran it through Session.Redact and handed the
// result to Run would put that very key on the wire and in the transcript —
// the turn adopts the wider replacer at begin and then sends the string it was
// given unchanged. Redact reads pending for exactly this reason.
//
// Two controls: the same call before the switch, where the value is not
// craze's credential and Redact leaves it alone, and the installed redactor
// after it, which must still be the narrow one — otherwise the assertion
// between them would pass without Redact having looked at pending at all.
func TestRedactCoversAKeyTheSwitchJustLearned(t *testing.T) {
	// A key of its own, sharing no text with the one Open knows.
	const later = "sk-exported-later-0004"
	f := newFixture(t, "http://127.0.0.1:1/v1")
	env := map[string]string{"TEST_API_KEY": canary} // other/c's provider has no key yet
	opts := f.options()
	opts.Getenv = func(name string) string { return env[name] }
	s := f.open(opts)

	// What an adapter's expansion of a command file looks like.
	body := "export OTHER_API_KEY=" + later
	if got := s.Redact(body); got != body {
		t.Fatalf("control: Redact rewrote a value craze had not resolved yet: %q", got)
	}

	env["OTHER_API_KEY"] = later
	if err := s.SetModel("other/c"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if got := s.tools.redactor().String(body); got != body {
		t.Fatalf("control: the switch installed the new redactor, so reading pending proves nothing: %q", got)
	}
	if got := s.Redact(body); strings.Contains(got, later) {
		t.Fatalf("Redact left the key the switch had already resolved in %q", got)
	}

	// End to end: the turn that adopts it sends and persists what Redact
	// returned, so nothing of the key is left anywhere.
	f.models["other/c"].push(answerWith("ok"))
	runWith(t, s, s.Redact(body), nil)
	if lines := entries(transcript(t, s)); strings.Contains(strings.Join(lines, "\n"), later) {
		t.Fatalf("the key reached the transcript:\n%s", strings.Join(lines, "\n"))
	}
	if found := leaks(f.models["other/c"].requests(), later); len(found) > 0 {
		t.Fatalf("the key reached the wire at %v", found)
	}
}

// TestStoreErrorsAreRedacted (round-2 review): a store error names the file
// it could not write, under the home craze was configured with, and that
// error is what Run and Close return to the adapter, which puts it on the
// screen. Its text is redacted — while it still unwraps to what it was, so
// callers can still match store.ErrClosed.
//
// The two halves are apart because no live session's store error can hold a
// key any more: a home whose plan path would hold one refuses to open
// (errPlanPathKey, plan 023 §3.3 — the plan file's path is handed to the
// model), and a key learned later that is inside it refuses the switch
// (errFrozenKey), and the plan path holds everything the transcript's path
// does. So the first half is a live failure reaching Run over a clean home,
// and the second is the redaction itself, over a store built directly on a
// home craze would now refuse. The redaction is defence, and it is what Run
// and Close still put every store error through.
func TestStoreErrorsAreRedacted(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	// A file where the transcript's directory has to go: every save fails,
	// with an error naming the path it could not make.
	if err := os.WriteFile(filepath.Join(f.home, "sessions"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := f.open(f.options())
	f.models["test/a"].push(answerWith("lost"))

	_, err := s.Run(context.Background(), "hi", nil)
	var pe *fs.PathError
	if err == nil || !errors.As(err, &pe) {
		t.Fatalf("Run = %v, want the store's failure, still unwrapping to it", err)
	}
	if !strings.Contains(err.Error(), "sessions") {
		t.Errorf("the error returned to the caller reads %q", err)
	}

	// A home whose path holds a key, which Open refuses and a store built
	// straight over it does not: its errors name the key.
	home := filepath.Join(f.home, canary)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "sessions"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if bad, err := Open(func() Options { o := f.options(); o.Home = home; return o }()); err == nil {
		bad.Close()
		t.Fatal("control: a home whose plan path holds a key opened, so the rest proves less than it says")
	} else if !errors.Is(err, errPlanPathKey) {
		t.Fatalf("control: Open over the keyed home = %v, want errPlanPathKey", err)
	}
	st, serr := store.New(store.Options{Home: home, Workspace: f.workspace})
	if serr != nil {
		t.Fatal(serr)
	}
	m := store.Model{Provider: "test", Alias: "test/a", WireModel: "wire-a"}
	if err := st.AppendUser(store.MessageEntry{Message: fantasy.NewUserMessage("hi"), Model: m}); err != nil {
		t.Fatal(err)
	}
	raw := st.AppendAssistant(store.MessageEntry{Message: streamed("", "lost"), Model: m})
	if raw == nil || !strings.Contains(raw.Error(), canary) {
		t.Fatalf("control: the store's own error does not name the home: %v", raw)
	}
	// Close's error goes through the same redaction (harness.Close).
	if got := s.tools.redactErr(raw); strings.Contains(got.Error(), canary) || !errors.Is(got, raw) {
		t.Errorf("redactErr(%v) = %v", raw, got)
	}
}

// TestKeyFloor (§7.8): a key the redactor cannot handle fails Open, for any
// provider, used or not — here the one no session of this table can even
// use, NOKEY_API_KEY's. The control: a key of a usable length opens.
func TestKeyFloor(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want error // nil: opens
	}{
		{"under eight bytes", "k3y-x7", modeltable.ErrKeyTooShort},
		{"overlapping the marker", "credential", modeltable.ErrKeyOverlapsMarker},
		// The name must not be the key: a test's temporary directory carries
		// it, and a workspace path holding a key refuses to open.
		{"usable", "sk-long-enough-0001", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			opts := f.options()
			opts.Getenv = func(name string) string {
				if name == "NOKEY_API_KEY" {
					return tc.key
				}
				return testEnv[name]
			}
			s, err := Open(opts)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Open = %v", err)
				}
				s.Close()
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Open = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), tc.key) {
				t.Fatalf("the error names the key: %v", err)
			}
		})
	}
}
