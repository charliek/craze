package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/store"
	"github.com/charliek/craze/internal/harness/tool"
)

// A running session learns the keys stored since it opened (plan 031 §3.8,
// P8, r2-1..r2-3): LearnKeys, which the native adapter calls at every turn's
// start with the inline keys of its providers.toml. Every key here is a dummy
// of at least MinKeyLen bytes that shares no text with the redaction marker,
// and each case has its control: the same value before it was learned, which
// nothing redacts, so the redaction after is the learning's doing.

// TestLearnKeysRedactsFromTheNextTurn: a key stored after Open is a value
// like any other until it is learned — a read of the file that holds it shows
// it — and from the next turn on it is redacted from the tool's result, every
// event, that turn's transcript lines and every request, the earlier turn's
// result the history replays included. Session.Redact covers it at once; the
// installed redactor only once a turn has begun, so a turn already running
// keeps its own (R1).
func TestLearnKeysRedactsFromTheNextTurn(t *testing.T) {
	const stored = "sk-stored-while-running-0005"
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	f.put("key.txt", "the stored key is "+stored+"\n")
	readIt := input(t, map[string]any{"filePath": "key.txt"})
	f.models["test/a"].push(
		callStep(callParts("c1", "read", readIt)), answerWith("before"), // turn 1
		callStep(callParts("c2", "read", readIt)), answerWith("after"), // turn 2
	)

	// Control: not yet learned, the value is plain text.
	var first events
	runWith(t, s, "read it", first.sink)
	if got := of[ToolFinished](first.list())[0].Result.Content; !strings.Contains(got, stored) {
		t.Fatalf("control: the value was redacted before it was learned, so learning it proves nothing: %q", got)
	}

	// Surrounding whitespace is the file's, not the key's.
	skipped, err := s.LearnKeys([]modeltable.Secret{modeltable.Secret("  " + stored + "\n")})
	if skipped != nil || err != nil {
		t.Fatalf("LearnKeys = %v, %v; want the key learned", skipped, err)
	}
	if got := s.Redact("key " + stored); got != "key "+redact.Marker {
		t.Fatalf("Redact after LearnKeys = %q; want the stored key covered at once", got)
	}
	if got := s.tools.redactor().String(stored); got != stored {
		t.Fatalf("control: LearnKeys installed the redactor at once (%q); a turn running then would change redactors mid-turn", got)
	}
	if !slices.Contains(s.tools.knownKeys(), stored) || !slices.Contains(s.tools.learnedKeys(), stored) {
		t.Fatal("the trimmed key is not among the session's keys and its learned ones")
	}

	var second events
	runWith(t, s, "and again", second.sink)
	fin := of[ToolFinished](second.list())[0].Result
	if strings.Contains(fin.Text+fin.Content, stored) || !strings.Contains(fin.Content, redact.Marker) {
		t.Fatalf("the next turn's read = %+v; want the stored key replaced by the marker", fin)
	}
	if found := leaks(second.list(), stored); len(found) > 0 {
		t.Fatalf("the stored key reached an event of the next turn at %v", found)
	}
	// Four lines a turn: the prompt, the call, its result, the answer.
	lines := entries(transcript(t, s))
	if len(lines) != 8 || strings.Contains(strings.Join(lines[4:], "\n"), stored) || !strings.Contains(strings.Join(lines[:4], "\n"), stored) {
		t.Fatalf("the transcript: want the key in the first turn's lines only (written before it was learned):\n%s", strings.Join(lines, "\n"))
	}
	reqs := f.models["test/a"].requests()
	if len(reqs) != 4 {
		t.Fatalf("%d requests, want 4", len(reqs))
	}
	if !strings.Contains(requestText(reqs[1], true), stored) {
		t.Fatal("control: the first turn's own requests did not carry the value, so their redaction later proves nothing")
	}
	for i, c := range reqs[2:] {
		if whole := requestText(c, true); strings.Contains(whole, stored) {
			t.Fatalf("the next turn's request %d holds the stored key:\n%s", i+1, whole)
		}
	}
}

// TestLearnKeysSkipsValuesThatCannotBeKeys (r2-3): each value is trimmed and
// held to modeltable.KeyProblem. One shorter than MinKeyLen, and one the
// marker could print back, are skipped — skipped[i] says why, as the
// modeltable sentinel, and never quotes it — and neither joins the redactor
// or what a child inherits. A blank value is no key: neither learned nor
// skipped. The usable one beside them is learned, and nothing refuses.
func TestLearnKeysSkipsValuesThatCannotBeKeys(t *testing.T) {
	const (
		short   = "zq9-w7"
		overlap = "credential"
		usable  = "sk-usable-stored-0006"
	)
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	skipped, err := s.LearnKeys([]modeltable.Secret{short, overlap, "  \t", modeltable.Secret(usable)})
	if err != nil {
		t.Fatalf("LearnKeys = %v; want no refusal", err)
	}
	if len(skipped) != 4 || !errors.Is(skipped[0], modeltable.ErrKeyTooShort) ||
		!errors.Is(skipped[1], modeltable.ErrKeyOverlapsMarker) || skipped[2] != nil || skipped[3] != nil {
		t.Fatalf("skipped = %v; want too short, overlapping, nothing, nothing", skipped)
	}
	for i, v := range []string{short, overlap} {
		if strings.Contains(skipped[i].Error(), v) {
			t.Errorf("the reason for skipping value %d quotes it: %q", i, skipped[i])
		}
		if slices.Contains(s.tools.knownKeys(), v) || slices.Contains(s.tools.learnedKeys(), v) {
			t.Errorf("value %d joined the session's keys", i)
		}
		if got := s.Redact("x " + v); got != "x "+v {
			t.Errorf("value %d is redacted (%q); a value that is no key is not one to redact", i, got)
		}
	}
	if s.Redact(usable) != redact.Marker {
		t.Fatal("the usable key beside them was not learned")
	}
	if learned := s.tools.learnedKeys(); !slices.Equal(learned, []string{usable}) {
		t.Fatalf("learned = %d keys; want only the usable one", len(learned))
	}
	if skipped, err := s.LearnKeys([]modeltable.Secret{"sk-another-usable-0007"}); skipped != nil || err != nil {
		t.Fatalf("LearnKeys of a usable key alone = %v, %v; want nil, nil", skipped, err)
	}
}

// TestLearnKeysFrozenSurfaceRefusesEveryTurn (r2-2): a stored key inside what
// the session sends unredacted with every request — the system prompt (it
// names the working directory), the encoded tools (a schema's own bytes) or
// the plan file's path — cannot be un-sent. LearnKeys learns it all the same,
// returns ErrStoredKeyFrozen, and from then on every Run, Compact and Wake is
// refused with it: no request, no transcript line, no event, and the refusal
// quotes no key. A later LearnKeys of an innocuous key changes nothing, and
// Close ends it: ErrClosed after. The control is a key in none of them:
// learned, and turns go on.
func TestLearnKeysFrozenSurfaceRefusesEveryTurn(t *testing.T) {
	cases := []struct {
		name   string
		key    func(f *fixture, s *Session) string
		frozen bool
	}{
		{"in the prompt", func(f *fixture, _ *Session) string { return f.workspace }, true},
		{"in the tools", func(*fixture, *Session) string { return `"required":["filePath"]` }, true},
		{"in the plan path", func(_ *fixture, s *Session) string { return planPathOf(s) }, true},
		{"nowhere in them", func(*fixture, *Session) string { return "sk-not-in-what-is-sent-0008" }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			f.models["test/a"].push(answerWith("before"))
			runWith(t, s, "hello", nil)
			key := c.key(f, s)
			if err := modeltable.KeyProblem(key); len(key) < modeltable.MinKeyLen || err != nil {
				t.Fatalf("the planted key cannot be a key (%v), so the case proves nothing", err)
			}
			// Where it is, so the case is about what it says.
			in := strings.Contains(s.system, key) || bytes.Contains(s.tools.wire, []byte(key)) || strings.Contains(planPathOf(s), key)
			if in != c.frozen {
				t.Fatalf("control: the key is in a frozen surface: %v, want %v", in, c.frozen)
			}

			_, err := s.LearnKeys([]modeltable.Secret{modeltable.Secret(key)})
			if !c.frozen {
				if err != nil {
					t.Fatalf("LearnKeys = %v; want the key learned and nothing refused", err)
				}
				f.models["test/a"].push(answerWith("after"))
				runWith(t, s, "go on", nil)
				if _, err := s.Wake(context.Background(), nil); !errors.Is(err, ErrNothingPending) {
					t.Fatalf("Wake = %v; want ErrNothingPending", err)
				}
				return
			}
			if !errors.Is(err, ErrStoredKeyFrozen) {
				t.Fatalf("LearnKeys = %v; want ErrStoredKeyFrozen", err)
			}
			if s.Redact(key) != redact.Marker || !slices.Contains(s.tools.knownKeys(), key) {
				t.Fatal("the frozen key was not learned for redaction")
			}

			before, err := os.ReadFile(s.store.Path())
			if err != nil {
				t.Fatal(err)
			}
			requests := len(f.models["test/a"].requests())
			f.models["test/a"].push(answerWith("never sent"))
			var ev events
			refused := func(what string, err error) {
				t.Helper()
				if !errors.Is(err, ErrStoredKeyFrozen) {
					t.Fatalf("%s = %v; want ErrStoredKeyFrozen", what, err)
				}
				if strings.Contains(err.Error(), key) {
					t.Fatalf("%s's refusal quotes the key", what)
				}
			}
			_, err = s.Run(context.Background(), "go on", ev.sink)
			refused("Run", err)
			_, err = s.Compact(context.Background(), "", "/compact", ev.sink)
			refused("Compact", err)
			_, err = s.Wake(context.Background(), ev.sink)
			refused("Wake", err)
			// Learning more changes nothing: the state holds until Close.
			if _, err := s.LearnKeys([]modeltable.Secret{"sk-innocuous-later-0009"}); err != nil {
				t.Fatalf("a later LearnKeys = %v; want nil (it found nothing frozen)", err)
			}
			_, err = s.Run(context.Background(), "and again", ev.sink)
			refused("a second Run", err)

			if n := len(f.models["test/a"].requests()); n != requests {
				t.Fatalf("%d requests after the refusals, want %d: a refused turn sent one", n, requests)
			}
			if len(ev.list()) != 0 {
				t.Fatalf("a refused turn emitted %v", ev.list())
			}
			if after, err := os.ReadFile(s.store.Path()); err != nil || !bytes.Equal(after, before) {
				t.Fatalf("a refused turn wrote to the transcript (%v)", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Run(context.Background(), "after close", nil); !errors.Is(err, ErrClosed) {
				t.Fatalf("Run after Close = %v; want ErrClosed", err)
			}
		})
	}
}

// TestRefusingSessionOwesNoPendingResult (r2-2 with plan 026 §3.11): a
// background child's result that becomes pending after the session refused
// can never be delivered — every turn and wake is refused — so HasPending
// says none, and the adapter's wake worker, which wakes on it, cannot wake,
// be refused, find the result still pending and wake again for ever;
// BackgroundOwed stops counting it too, while a child still running is still
// owed. The registry itself still holds the result pending (the controls:
// its own hasPending and owed say so), and Close reports it undelivered.
func TestRefusingSessionOwesNoPendingResult(t *testing.T) {
	b := openBG(t)
	ws, ids := b.spawn(t, "work")
	if _, err := b.s.LearnKeys([]modeltable.Secret{modeltable.Secret(b.workspace)}); !errors.Is(err, ErrStoredKeyFrozen) {
		t.Fatalf("LearnKeys = %v; want the refusal", err)
	}
	if !b.s.BackgroundOwed() {
		t.Fatal("a child still running is not owed once the session refuses")
	}
	close(ws[0].release)
	if await(t, b.pending, "OnPending for the finished child") {
		t.Fatal("HasPending = true in a refusing session")
	}
	if res := resultOf(t, b.s, ids[0]); res.state != resultPending {
		t.Fatalf("control: the result is in state %v, want pending", res.state)
	}
	if !b.s.subs.hasPending() || !b.s.subs.owed(false) {
		t.Fatal("control: the registry does not hold the result pending, so the gate proves nothing")
	}
	if b.s.HasPending() || b.s.BackgroundOwed() {
		t.Fatalf("HasPending %v, BackgroundOwed %v; want neither in the refusal state", b.s.HasPending(), b.s.BackgroundOwed())
	}
	if _, err := b.s.Wake(context.Background(), nil); !errors.Is(err, ErrStoredKeyFrozen) {
		t.Fatalf("Wake = %v; want the refusal", err)
	}
	if res := resultOf(t, b.s, ids[0]); res.state != resultPending {
		t.Fatalf("the refused wake left the result in state %v, want still pending", res.state)
	}
	if err := b.s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := of[SubagentUndelivered](b.own.list()); len(got) != 1 || got[0].ID != ids[0] {
		t.Fatalf("SubagentUndelivered at Close = %+v; want the one pending result", got)
	}
}

// TestChildOpenedAfterLearningStartsWithTheKeys (r2-1): a sub-agent is opened
// with the stored keys its parent had learned, before its prompt, its tools
// and its transcript are built — so a role that quotes one reaches the
// child's prompt redacted, as does a persona path in its header. The control
// is the same child opened before the parent learned: the raw key in both
// places. It is attached to no runner, so nothing teaches it the key later
// (an attached one is taught at once: TestAKeyLearnedWhileASubagentRunsRedactsItsToolOutput).
func TestChildOpenedAfterLearningStartsWithTheKeys(t *testing.T) {
	const stored = "sk-stored-for-the-child-0010"
	f := newRouted(t)
	parent := f.open(f.options())
	persona := tool.Persona{Name: "reviewer", Role: "Check with " + stored + ".\n", Path: "/p/" + stored + "/reviewer.md", AllTools: true}
	open := func(id string) *Session {
		t.Helper()
		child, err := parent.subs.openChild(parent.view(), &childHandle{id: id}, tool.SubagentCall{ID: "t1.1.1"}, persona, "test/a", "", modeAgent)
		if err != nil {
			t.Fatalf("openChild: %v", err)
		}
		t.Cleanup(func() { _ = child.Close() })
		return child
	}
	earlier := open("child-before")
	if !strings.Contains(earlier.system, stored) || earlier.Redact(stored) != stored {
		t.Fatal("control: a child opened before the parent learned the key already redacts it")
	}

	if _, err := parent.LearnKeys([]modeltable.Secret{stored}); err != nil {
		t.Fatalf("LearnKeys: %v", err)
	}
	later := open("child-after")
	if strings.Contains(later.system, stored) || !strings.Contains(later.system, "Check with "+redact.Marker) {
		t.Fatal("the child opened after learning built its prompt without the stored key")
	}
	if later.Redact(stored) != redact.Marker || !slices.Contains(later.tools.knownKeys(), stored) {
		t.Fatal("the child opened after learning does not know the stored key")
	}
	if h := later.store.Header(); strings.Contains(h.PersonaPath, stored) || !strings.Contains(h.PersonaPath, redact.Marker) {
		t.Fatalf("the child's header records the persona path %q; want it redacted", h.PersonaPath)
	}
	if earlier.Redact(stored) != stored {
		t.Fatal("the child opened before learning, attached to no runner, was taught the key")
	}
}

// TestChildSpawnedAfterLearningRedactsItsToolOutput (r2-1, A7): end to end,
// through the agent tool. A child's own command prints a stored key — enough
// of it to spill — and its own dispatcher is what redacts that, in the
// result it reports and in the spill file, which nothing after it rewrites.
// The child the parent's first turn started, before the key was learned,
// shows it raw in both (the control, and R1); the one its next turn starts,
// after, shows only the marker.
func TestChildSpawnedAfterLearningRedactsItsToolOutput(t *testing.T) {
	const stored = "sk-stored-for-the-child-0011"
	f := newRouted(t)
	s := f.open(f.options())
	f.put("key.txt", strings.Repeat("the key is "+stored+"\n", 3000))
	catIt := input(t, map[string]any{"command": "cat key.txt"})
	a := f.routers["test/a"]
	// Both of the parent's turns are routed under its first prompt: it is
	// every later request's first user message too.
	a.route("go",
		callStep(agentPart(t, "a1", task("look", "child one"))), answerWith("one done"),
		callStep(agentPart(t, "a2", task("look", "child two"))), answerWith("two done"))
	for _, prompt := range []string{"child one", "child two"} {
		a.route(prompt, callStep(callParts("k1", "bash", catIt)), answerWith(prompt+" finished"))
	}
	// The child's own bash result, from its events on their way to the
	// parent's sink.
	bashOf := func(evs []Event, prompt string) tool.Result {
		t.Helper()
		id := startedWith(t, evs, prompt).ID
		for _, e := range of[SubagentEvent](evs) {
			if fin, ok := e.Event.(ToolFinished); ok && e.ID == id {
				return fin.Result
			}
		}
		t.Fatalf("no tool result from the child %q", prompt)
		return tool.Result{}
	}
	spilled := func(res tool.Result) string {
		t.Helper()
		if res.Trunc.Spill == "" {
			t.Fatalf("the child's output did not spill, so the spill file is not checked: %+v", res.Trunc)
		}
		b, err := os.ReadFile(res.Trunc.Spill)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	var first events
	runWith(t, s, "go", first.sink)
	one := bashOf(first.list(), "child one")
	if !strings.Contains(one.Text, stored) || !strings.Contains(spilled(one), stored) {
		t.Fatal("control: the child started before learning redacted the value, so the redaction after proves nothing")
	}

	if _, err := s.LearnKeys([]modeltable.Secret{stored}); err != nil {
		t.Fatalf("LearnKeys: %v", err)
	}
	var second events
	runWith(t, s, "go", second.sink)
	two := bashOf(second.list(), "child two")
	if strings.Contains(two.Text, stored) || !strings.Contains(two.Text, redact.Marker) {
		t.Fatal("the child started after learning reports its output with the key, or without the marker")
	}
	if body := spilled(two); strings.Contains(body, stored) || !strings.Contains(body, redact.Marker) {
		t.Fatal("the child started after learning wrote the key into its spill file")
	}
	if found := leaks(second.list(), stored); len(found) > 0 {
		t.Fatalf("the stored key reached an event of the second turn at %v", found)
	}
}

// TestLearnKeysIsSerialized: LearnKeys from many goroutines at once — and a
// switch's resolve among them, which extends the same set — loses no key:
// each extends the set the last one left, under the toolset's lock, never a
// copy of it from before. Under -race an unguarded update is a failure of its
// own. The next turn installs one redactor over all of them.
func TestLearnKeysIsSerialized(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	env := map[string]string{"TEST_API_KEY": canary}
	var envMu sync.Mutex
	opts := f.options()
	opts.Getenv = func(k string) string {
		envMu.Lock()
		defer envMu.Unlock()
		return env[k]
	}
	s := f.open(opts)
	const exported = "sk-exported-meanwhile-0012"
	envMu.Lock()
	env["OTHER_API_KEY"] = exported
	envMu.Unlock()

	var keys []string
	for i := range 16 {
		keys = append(keys, fmt.Sprintf("sk-stored-concurrently-%02d", i))
	}
	var wg sync.WaitGroup
	for _, k := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.LearnKeys([]modeltable.Secret{modeltable.Secret(k)}); err != nil {
				t.Errorf("LearnKeys: %v", err)
			}
			_ = s.Redact(k)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.SetModel("other/c"); err != nil {
			t.Errorf("SetModel: %v", err)
		}
	}()
	wg.Wait()

	for _, k := range append(slices.Clone(keys), exported) {
		if !slices.Contains(s.tools.knownKeys(), k) || s.Redact(k) != redact.Marker {
			t.Fatalf("a key was lost to a concurrent update")
		}
	}
	if len(s.tools.learnedKeys()) != len(keys) {
		t.Fatalf("%d learned keys, want %d", len(s.tools.learnedKeys()), len(keys))
	}
	f.models["other/c"].push(answerWith("ok"))
	runWith(t, s, "go", nil)
	red := s.tools.redactor()
	for _, k := range append(keys, exported) {
		if red.String(k) != redact.Marker {
			t.Fatal("the turn installed a redactor that misses a key learned concurrently")
		}
	}
}

// TestAKeyLearnedMidTurnEndsTheTurn (plan 034 C4r, r9 #7a's defence in
// depth): a key learned while a turn runs — here mid-request, as the native
// adapter's learning by any path could land — that is inside the frozen
// prompt puts the session in its refusal state, and the turn sends no request
// after the step boundary that sees it: the step's tool call runs and the
// step is persisted, as every finished step is, and the turn ends with
// ErrStoredKeyFrozen instead of asking the model again with the prompt that
// holds the key. The next turn is refused at its admission, as before. The
// control: the same turn with a key that is in no frozen surface learned at
// the same moment goes on to its second request. Negative control: a turn
// that does not look at the refusal state at its boundaries (refusingNow
// always false) sends the second request, its system prompt holding the
// key, and ends well.
func TestAKeyLearnedMidTurnEndsTheTurn(t *testing.T) {
	for _, c := range []struct {
		name   string
		key    func(f *fixture) string
		frozen bool
	}{
		{"in the prompt", func(f *fixture) string { return f.workspace }, true},
		{"nowhere in what is sent", func(*fixture) string { return "sk-not-in-what-is-sent-0341" }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			key := c.key(f)
			if err := modeltable.KeyProblem(key); err != nil {
				t.Fatalf("the planted key cannot be a key (%v), so the case proves nothing", err)
			}
			if strings.Contains(s.system, key) != c.frozen {
				t.Fatalf("control: the key is in the system prompt: %v, want %v", !c.frozen, c.frozen)
			}
			m := f.models["test/a"]
			g := newGate()
			m.push(g.hold(callParts("c1", "todo_write", `{"todos":[{"id":"a","content":"first"}]}`),
				finish(fantasy.FinishReasonToolCalls)), answerWith("second"))
			var ev events
			out := start(context.Background(), s, "go", ev.sink)
			await(t, g.reached, "the first request")
			_, err := s.LearnKeys([]modeltable.Secret{modeltable.Secret(key)})
			if c.frozen != errors.Is(err, ErrStoredKeyFrozen) {
				t.Fatalf("LearnKeys mid-turn = %v; want the refusal state: %v", err, c.frozen)
			}
			close(g.release)
			o := await(t, out, "the turn")
			requests := m.requests()
			if !c.frozen {
				if o.err != nil || len(requests) != 2 {
					t.Fatalf("control: the turn = %v after %d requests; want it to go on to its second", o.err, len(requests))
				}
				return
			}
			if !errors.Is(o.err, ErrStoredKeyFrozen) || strings.Contains(o.err.Error(), key) {
				t.Fatalf("the turn = %v; want ErrStoredKeyFrozen, quoting no key", o.err)
			}
			if len(requests) != 1 {
				t.Fatalf("%d requests; want the first alone: a request after the key was learned carried it", len(requests))
			}
			dones := of[StepDone](ev.list())
			if len(dones) != 1 || !dones[0].Saved {
				t.Fatalf("the step the key was learned in reported %+v; want it persisted", dones)
			}
			if _, err := s.Run(context.Background(), "and again", nil); !errors.Is(err, ErrStoredKeyFrozen) {
				t.Fatalf("the next Run = %v; want ErrStoredKeyFrozen", err)
			}
			if n := len(m.requests()); n != 1 {
				t.Fatalf("%d requests after the refused Run; want 1", n)
			}
		})
	}
}

// TestAKeyLearnedMidTurnStopsItsCompaction (plan 034 C4r, r9 #7a's defence
// in depth, at the boundaries around a compaction, whose summarizer's request
// sends the frozen prompt too): a key inside the frozen prompt learned during
// a step that leaves the context at the threshold, or during a request the
// provider then refuses as too large, stops the turn there — no mid-turn or
// overflow compaction runs, no request follows; one learned while the
// mid-turn compaction's summarizer runs stops the turn before the next
// segment's first request. Each ends with the refusal. Negative controls:
// restartDue, or overflowed, that does not look at the refusal state
// compacts first, its summarizer's request carrying the key; a prepareStep
// that does not look sends the next segment's request after the compaction.
func TestAKeyLearnedMidTurnStopsItsCompaction(t *testing.T) {
	for _, c := range []struct {
		name        string
		seeded      bool
		steps       func(g *gate) []step
		requests    int
		summarizers int
	}{
		{"at the threshold", false, func(g *gate) []step {
			return []step{g.hold(bareCall("c1", "nope", `{"n":1}`), finishUsing(fantasy.FinishReasonToolCalls, over)), summaryOf("never")}
		}, 1, 0},
		{"on an overflow", true, func(g *gate) []step { return []step{g.hold(nil, errorPart(overflowErr())), summaryOf("never")} }, 2, 0},
		{"during the compaction", false, func(g *gate) []step {
			return []step{toolStep(1, over), heldSummary(g, "the first step")}
		}, 2, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			windowed(f, "test/a", testWindow, 0)
			s := f.open(f.options())
			a := f.models["test/a"]
			if c.seeded {
				a.push(answerWith("hi"))
				run(t, s, "hello")
			}
			g := newGate()
			a.push(append(c.steps(g), answerWith("never"))...)
			out := start(context.Background(), s, "go", nil)
			await(t, g.reached, "the request")
			if _, err := s.LearnKeys([]modeltable.Secret{modeltable.Secret(f.workspace)}); !errors.Is(err, ErrStoredKeyFrozen) {
				t.Fatalf("premise: LearnKeys = %v; want the refusal state", err)
			}
			close(g.release)
			o := await(t, out, "the turn")
			if !errors.Is(o.err, ErrStoredKeyFrozen) {
				t.Fatalf("the turn = %+v, %v; want ErrStoredKeyFrozen", o.res, o.err)
			}
			if reqs := a.requests(); len(reqs) != c.requests || summarizers(reqs) != c.summarizers {
				t.Fatalf("%d requests, %d of them the summarizer; want %d, %d of them", len(reqs), summarizers(reqs), c.requests, c.summarizers)
			}
		})
	}
}

// TestAKeyLearnedDuringARequestStopsItsRetry (plan 034 C4r2, r11 #2a): a key
// inside the frozen prompt learned while a step's request is outstanding,
// which then fails, before any output, with an error Fantasy retries, gets
// no retry: Fantasy's retry re-sends the step's prepared request around the
// model's Stream alone, outside PrepareStep, so the refusal gate on the turn's
// model refuses it, nothing is sent, no Retrying is announced, and the turn
// ends with ErrStoredKeyFrozen; the next turn is refused at its admission.
// The control: a key in no frozen surface learned at the same moment, and the
// request is retried, announced, and the turn ends well. Negative controls: a
// gate that does not look at the refusal state re-sends the request, its
// system prompt holding the key; a retry hook that does not look announces a
// retry that never comes.
func TestAKeyLearnedDuringARequestStopsItsRetry(t *testing.T) {
	for _, c := range []struct {
		name   string
		key    func(f *fixture) string
		frozen bool
	}{
		{"in the prompt", func(f *fixture) string { return f.workspace }, true},
		{"nowhere in what is sent", func(*fixture) string { return "sk-not-in-what-is-sent-0351" }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			key := c.key(f)
			if err := modeltable.KeyProblem(key); err != nil {
				t.Fatalf("the planted key cannot be a key (%v), so the case proves nothing", err)
			}
			if strings.Contains(s.system, key) != c.frozen {
				t.Fatalf("control: the key is in the system prompt: %v, want %v", !c.frozen, c.frozen)
			}
			m := f.models["test/a"]
			g := newGate()
			busy := &fantasy.ProviderError{Message: "overloaded", StatusCode: 503, ResponseHeaders: map[string]string{"retry-after-ms": "1"}}
			m.push(g.hold(nil, errorPart(busy)), answerWith("retried"))
			var ev events
			out := start(context.Background(), s, "go", ev.sink)
			await(t, g.reached, "the first request")
			_, err := s.LearnKeys([]modeltable.Secret{modeltable.Secret(key)})
			if c.frozen != errors.Is(err, ErrStoredKeyFrozen) {
				t.Fatalf("LearnKeys mid-request = %v; want the refusal state: %v", err, c.frozen)
			}
			close(g.release)
			o := await(t, out, "the turn")
			requests, retries := m.requests(), of[Retrying](ev.list())
			if !c.frozen {
				if o.err != nil || len(requests) != 2 || len(retries) != 1 {
					t.Fatalf("control: the turn = %v after %d requests and %d retries announced; want it retried once and ended well",
						o.err, len(requests), len(retries))
				}
				return
			}
			if !strings.Contains(textOf(requests[0].Prompt[0]), key) {
				t.Fatal("premise: the first request's system prompt does not hold the key")
			}
			if !errors.Is(o.err, ErrStoredKeyFrozen) || strings.Contains(o.err.Error(), key) {
				t.Fatalf("the turn = %v; want ErrStoredKeyFrozen, quoting no key", o.err)
			}
			if len(requests) != 1 {
				t.Fatalf("%d requests; want the first alone: the retry, sent after the key was learned, carried it", len(requests))
			}
			if len(retries) != 0 {
				t.Fatalf("Retrying announced %+v; want none: no retry follows", retries)
			}
			if !empty(o.res) {
				t.Fatalf("the refused turn's Result = %+v; want nothing", o.res)
			}
			if _, err := s.Run(context.Background(), "and again", nil); !errors.Is(err, ErrStoredKeyFrozen) {
				t.Fatalf("the next Run = %v; want ErrStoredKeyFrozen", err)
			}
			if n := len(m.requests()); n != 1 {
				t.Fatalf("%d requests after the refused Run; want 1", n)
			}
		})
	}
}

// TestAKeyLearnedDuringASummarizerRequestStopsItsAttempts (plan 034 C4r2, r11
// #2b): every summarizer attempt sends the frozen prompt, so a key inside it
// learned while an attempt's request is outstanding stops the attempts there
// — whatever would have made another: a failure the attempts retry, a
// degenerate summary, the aligned form's overflow, which switches to the text
// form at once. No further request is sent and the refusal surfaces: a manual
// /compact returns ErrStoredKeyFrozen, and a turn whose compaction it was —
// before its first request, between two segments, or for an overflow —
// ends with it, automatic compaction left as it was. A billed attempt is on a
// failure entry, as a cancelled compaction's is; an unbilled one writes none.
// The control: a key in no frozen surface learned at the same moment, and the
// next attempt is sent and summarizes. Negative controls: an attempts loop
// that does not look at the refusal state sends the next attempt, its system
// prompt holding the key; a mid-turn or pre-turn compaction that reads the
// refusal as a summarizer failure switches automatic compaction off; an
// overflow compaction that does reads the turn's end as the overflow.
func TestAKeyLearnedDuringASummarizerRequestStopsItsAttempts(t *testing.T) {
	retryable := func() []fantasy.StreamPart {
		return errorPart(&fantasy.ProviderError{StatusCode: 500, Message: "boom"})
	}
	degenerate := func() []fantasy.StreamPart { return cat(textParts(shortSummary), finish(fantasy.FinishReasonStop)) }
	alignedOverflow := func() []fantasy.StreamPart { return errorPart(overflowErr()) }
	for _, c := range []struct {
		name  string
		kind  string // manual, pre-turn, mid-turn, overflow
		fails func() []fantasy.StreamPart
		// billed says the held attempt reports usage, which a failure entry
		// then holds; before is the requests ahead of the held one.
		billed bool
		before int
	}{
		{"a manual compaction, after a retryable failure", "manual", retryable, false, 1},
		{"a manual compaction, after a degenerate summary", "manual", degenerate, true, 1},
		{"a manual compaction, after the aligned form overflowed", "manual", alignedOverflow, false, 1},
		{"a pre-turn compaction, after a retryable failure", "pre-turn", retryable, false, 1},
		{"a mid-turn compaction, after a retryable failure", "mid-turn", retryable, false, 1},
		{"an overflow compaction, after a retryable failure", "overflow", retryable, false, 2},
	} {
		for _, frozen := range []bool{true, false} {
			name := c.name + ", a key in no frozen surface"
			if frozen {
				name = c.name + ", a key in the prompt"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t, "http://127.0.0.1:1/v1")
				windowed(f, "test/a", testWindow, 0)
				s := f.open(f.options())
				s.sleep = func(context.Context, time.Duration) {}
				a := f.models["test/a"]
				key := "sk-not-in-what-is-sent-0352"
				if frozen {
					key = f.workspace
				}
				if strings.Contains(s.system, key) != frozen {
					t.Fatalf("control: the key is in the system prompt: %v, want %v", !frozen, frozen)
				}
				g := newGate()
				held := g.hold(nil, c.fails())
				type ended struct {
					res Result
					err error
				}
				out := make(chan ended, 1)
				switch c.kind {
				case "manual":
					a.push(answerWith("hi"))
					run(t, s, "hello")
					a.push(held, summaryOf("the next attempt"))
					go func() {
						res, err := s.Compact(context.Background(), "", "/compact", nil)
						out <- ended{res, err}
					}()
				case "pre-turn":
					a.push(answerSpending("hi", over))
					run(t, s, "hello")
					a.push(held, summaryOf("the next attempt"), answerWith("done"))
					go func() {
						res, err := s.Run(context.Background(), "again", nil)
						out <- ended{res, err}
					}()
				case "mid-turn":
					a.push(toolStep(1, over), held, summaryOf("the next attempt"), answerWith("done"))
					go func() {
						res, err := s.Run(context.Background(), "go", nil)
						out <- ended{res, err}
					}()
				case "overflow":
					a.push(answerWith("hi"))
					run(t, s, "hello")
					a.push(overflowed(), held, summaryOf("the next attempt"), answerWith("done"))
					go func() {
						res, err := s.Run(context.Background(), "again", nil)
						out <- ended{res, err}
					}()
				}
				await(t, g.reached, "the summarizer's first request")
				if n := len(a.requests()); n != c.before+1 || summarizers(a.requests()) != 1 {
					t.Fatalf("premise: %d requests, %d of them the summarizer's; want %d, the last the held attempt", n, summarizers(a.requests()), c.before+1)
				}
				if _, err := s.LearnKeys([]modeltable.Secret{modeltable.Secret(key)}); frozen != errors.Is(err, ErrStoredKeyFrozen) {
					t.Fatalf("LearnKeys during the attempt = %v; want the refusal state: %v", err, frozen)
				}
				close(g.release)
				o := await(t, out, "the compaction's end")
				reqs := a.requests()
				if !frozen {
					if o.err != nil || o.res.StopReason != StopEndTurn || summarizers(reqs) != 2 {
						t.Fatalf("control: %+v, %v after %d summarizer requests; want the next attempt sent and the turn ended well",
							o.res, o.err, summarizers(reqs))
					}
					return
				}
				if !errors.Is(o.err, ErrStoredKeyFrozen) || strings.Contains(o.err.Error(), key) {
					t.Fatalf("the end = %+v, %v; want ErrStoredKeyFrozen, quoting no key", o.res, o.err)
				}
				if len(reqs) != c.before+1 || summarizers(reqs) != 1 {
					t.Fatalf("%d requests, %d of them the summarizer's; want none after the held attempt: the next one carried the key",
						len(reqs), summarizers(reqs))
				}
				s.mu.Lock()
				off := s.autoOff.on
				s.mu.Unlock()
				if off {
					t.Fatal("the refusal switched automatic compaction off, as a summarizer failure does")
				}
				entries := compactionEntries(t, s)
				if !c.billed {
					if len(entries) != 0 {
						t.Fatalf("compaction entries = %+v; want none: nothing was billed", entries)
					}
					return
				}
				if len(entries) != 1 || entries[0].Compaction.Succeeded() || entries[0].Usage == nil || *entries[0].Usage == (store.Usage{}) ||
					entries[0].Compaction.Error != cleanErrorText(ErrStoredKeyFrozen) {
					t.Fatalf("compaction entries = %+v; want one failure holding the billed attempt, the refusal its error", entries)
				}
			})
		}
	}
}

// TestAKeyLearnedDuringTheLastSummarizerAttemptRefusesTheCompaction (plan 034
// C4r3, r13 #3): the attempts loop checks the refusal on its way into an
// attempt, so a key inside the frozen prompt learned during an attempt that
// no other follows — the last one, failing as the two before it did, or one
// that fails fatally — would leave the loop as a summarizer failure. It
// leaves it as the refusal instead, checked once the attempts have failed: a
// manual /compact returns ErrStoredKeyFrozen, and a turn whose compaction it
// was — before its first request, between two segments, or for an overflow —
// ends with it, automatic compaction left as it was; no failure entry unless
// an attempt was billed, and then one holding every billed attempt, the
// refusal its error. The control: a key in no frozen surface learned at the
// same moment, and the compaction fails as a summarizer failure does — its
// failure entry written, automatic compaction switched off before and
// mid-turn, the overflow the turn's end. Negative control: no check after the
// loop, and the refused compaction ends as that failure.
func TestAKeyLearnedDuringTheLastSummarizerAttemptRefusesTheCompaction(t *testing.T) {
	retryable := func() []fantasy.StreamPart {
		return errorPart(&fantasy.ProviderError{StatusCode: 500, Message: "boom"})
	}
	degenerate := func() []fantasy.StreamPart { return cat(textParts(shortSummary), finish(fantasy.FinishReasonStop)) }
	fatal := func() []fantasy.StreamPart {
		return errorPart(&fantasy.ProviderError{StatusCode: 404, Message: "no such model"})
	}
	for _, c := range []struct {
		name  string
		kind  string // manual, pre-turn, mid-turn, overflow
		fails func() []fantasy.StreamPart
		// attempts are the summarizer's requests, the last one held; billed
		// says each reports usage; before is the requests ahead of the
		// first.
		attempts int
		billed   bool
		before   int
	}{
		{"a manual compaction, its last attempt failing retryably", "manual", retryable, summarizerAttempts, false, 1},
		{"a manual compaction, its last attempt degenerate", "manual", degenerate, summarizerAttempts, true, 1},
		{"a manual compaction, its first attempt failing fatally", "manual", fatal, 1, false, 1},
		{"a pre-turn compaction, its last attempt failing retryably", "pre-turn", retryable, summarizerAttempts, false, 1},
		{"a pre-turn compaction, its first attempt failing fatally", "pre-turn", fatal, 1, false, 1},
		{"a mid-turn compaction, its last attempt failing retryably", "mid-turn", retryable, summarizerAttempts, false, 1},
		{"a mid-turn compaction, its first attempt failing fatally", "mid-turn", fatal, 1, false, 1},
		{"an overflow compaction, its last attempt failing retryably", "overflow", retryable, summarizerAttempts, false, 2},
		{"an overflow compaction, its first attempt failing fatally", "overflow", fatal, 1, false, 2},
	} {
		for _, frozen := range []bool{true, false} {
			name := c.name + ", a key in no frozen surface"
			if frozen {
				name = c.name + ", a key in the prompt"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t, "http://127.0.0.1:1/v1")
				windowed(f, "test/a", testWindow, 0)
				s := f.open(f.options())
				s.sleep = func(context.Context, time.Duration) {}
				a := f.models["test/a"]
				key := "sk-not-in-what-is-sent-0362"
				if frozen {
					key = f.workspace
				}
				if strings.Contains(s.system, key) != frozen {
					t.Fatalf("control: the key is in the system prompt: %v, want %v", !frozen, frozen)
				}
				g := newGate()
				// The attempts before the last fail unheld; the last is held,
				// and fails as they did once released.
				attempts := make([]step, 0, c.attempts)
				for range c.attempts - 1 {
					attempts = append(attempts, reply(c.fails()))
				}
				attempts = append(attempts, g.hold(nil, c.fails()))
				type ended struct {
					res Result
					err error
				}
				out := make(chan ended, 1)
				switch c.kind {
				case "manual":
					a.push(answerWith("hi"))
					run(t, s, "hello")
					a.push(attempts...)
					go func() {
						res, err := s.Compact(context.Background(), "", "/compact", nil)
						out <- ended{res, err}
					}()
				case "pre-turn":
					a.push(answerSpending("hi", over))
					run(t, s, "hello")
					a.push(attempts...)
					a.push(answerWith("done"))
					go func() {
						res, err := s.Run(context.Background(), "again", nil)
						out <- ended{res, err}
					}()
				case "mid-turn":
					a.push(toolStep(1, over))
					a.push(attempts...)
					a.push(answerWith("done"))
					go func() {
						res, err := s.Run(context.Background(), "go", nil)
						out <- ended{res, err}
					}()
				case "overflow":
					a.push(answerWith("hi"))
					run(t, s, "hello")
					a.push(overflowed())
					a.push(attempts...)
					a.push(answerWith("done"))
					go func() {
						res, err := s.Run(context.Background(), "again", nil)
						out <- ended{res, err}
					}()
				}
				await(t, g.reached, "the summarizer's last attempt")
				if n := len(a.requests()); n != c.before+c.attempts || summarizers(a.requests()) != c.attempts {
					t.Fatalf("premise: %d requests, %d of them the summarizer's; want %d, the last the held attempt",
						n, summarizers(a.requests()), c.before+c.attempts)
				}
				if _, err := s.LearnKeys([]modeltable.Secret{modeltable.Secret(key)}); frozen != errors.Is(err, ErrStoredKeyFrozen) {
					t.Fatalf("LearnKeys during the attempt = %v; want the refusal state: %v", err, frozen)
				}
				close(g.release)
				o := await(t, out, "the compaction's end")
				reqs := a.requests()
				s.mu.Lock()
				off := s.autoOff.on
				s.mu.Unlock()
				entries := compactionEntries(t, s)
				if !frozen {
					// A summarizer failure: its entry, billed or not, and what
					// each caller makes of one.
					if errors.Is(o.err, ErrStoredKeyFrozen) || len(entries) != 1 || entries[0].Compaction.Succeeded() {
						t.Fatalf("control: the end = %+v, %v, entries %+v; want the summarizer's failure, its entry written", o.res, o.err, entries)
					}
					switch c.kind {
					case "manual":
						if o.err == nil {
							t.Fatal("control: the manual compaction returned no error")
						}
					case "pre-turn", "mid-turn":
						if !off || o.err != nil {
							t.Fatalf("control: the turn = %v, automatic compaction off: %v; want the turn ended well, compaction switched off", o.err, off)
						}
					case "overflow":
						if !errors.Is(o.err, ErrContextTooLarge) {
							t.Fatalf("control: the turn = %v; want the overflow", o.err)
						}
					}
					return
				}
				if !errors.Is(o.err, ErrStoredKeyFrozen) || strings.Contains(o.err.Error(), key) {
					t.Fatalf("the end = %+v, %v; want ErrStoredKeyFrozen, quoting no key", o.res, o.err)
				}
				if len(reqs) != c.before+c.attempts || summarizers(reqs) != c.attempts {
					t.Fatalf("%d requests, %d of them the summarizer's; want none after the held attempt", len(reqs), summarizers(reqs))
				}
				if off {
					t.Fatal("the refusal switched automatic compaction off, as a summarizer failure does")
				}
				if !c.billed {
					if len(entries) != 0 {
						t.Fatalf("compaction entries = %+v; want none: nothing was billed", entries)
					}
					return
				}
				var want store.Usage
				for range c.attempts {
					want = addUsage(want, *store.UsageOf(stepUsage))
				}
				if len(entries) != 1 || entries[0].Compaction.Succeeded() || entries[0].Usage == nil || *entries[0].Usage != want ||
					entries[0].Compaction.Error != cleanErrorText(ErrStoredKeyFrozen) {
					t.Fatalf("compaction entries = %+v; want one failure holding every billed attempt, the refusal its error", entries)
				}
			})
		}
	}
}

// TestAKeyLearnedWhileASubagentRunsStopsTheSubagent (plan 034 C4r2, r11 #2c):
// a key learned while a sub-agent runs reaches it — every attached child
// learns the parent's keys, as AddSecrets visits them — and is judged by the
// child's own frozen surfaces: one inside the prompt it inherited puts the
// child in its refusal state, so its turn sends no request after its next
// boundary and fails. A background child does so while its parent idles,
// which is when an idle reload learns keys; a foreground child mid-call, the
// parent's turn ending with the refusal after it. The control: a key in no
// frozen surface, and the child goes on to its second request. Negative
// control: a LearnKeys that teaches the parent alone leaves the child sending
// its second request, the key in its system prompt.
func TestAKeyLearnedWhileASubagentRunsStopsTheSubagent(t *testing.T) {
	for _, background := range []bool{true, false} {
		for _, frozen := range []bool{true, false} {
			name := "foreground"
			if background {
				name = "background"
			}
			if frozen {
				name += ", a key in the prompt it inherited"
			} else {
				name += ", a key in no frozen surface"
			}
			t.Run(name, func(t *testing.T) {
				b := openBG(t)
				a := b.routers["test/a"]
				key := "sk-not-in-what-is-sent-0353"
				if frozen {
					key = b.workspace
				}
				w := newWorker()
				// The child's first step makes a call, and holds; its second
				// answers.
				a.route("child work", w.step(bareCall("c1", "nope", `{"n":1}`), finish(fantasy.FinishReasonToolCalls)), answerWith("done"))
				var ev events
				var out <-chan outcome
				if background {
					a.route("go", callStep(bgPart(t, "a1", "job", "child work")), answerWith("started"))
					if res, err := b.s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
						t.Fatalf("the spawning turn = %+v, %v; want end_turn", res, err)
					}
				} else {
					a.route("go", callStep(agentPart(t, "a1", task("job", "child work"))), answerWith("never"))
					out = start(context.Background(), b.s, "go", ev.sink)
				}
				await(t, w.reached, "the child's first step")
				id := startedWith(t, ev.list(), "child work").ID
				if got := textOf(a.requests("child work")[0].Prompt[0]); strings.Contains(got, key) != frozen {
					t.Fatalf("premise: the child's system prompt holds the key: %v, want %v", !frozen, frozen)
				}
				if _, err := b.s.LearnKeys([]modeltable.Secret{modeltable.Secret(key)}); frozen != errors.Is(err, ErrStoredKeyFrozen) {
					t.Fatalf("LearnKeys = %v; want the parent's refusal state: %v", err, frozen)
				}
				close(w.release)
				var status string
				if background {
					await(t, b.pending, "the child's result")
					status = resultOf(t, b.s, id).status
				} else {
					o := await(t, out, "the parent's turn")
					if frozen != errors.Is(o.err, ErrStoredKeyFrozen) {
						t.Fatalf("the parent's turn = %v; want the refusal: %v", o.err, frozen)
					}
					status = of[SubagentFinished](ev.list())[0].Status
				}
				n := len(a.requests("child work"))
				if !frozen {
					if n != 2 || status != SubagentCompleted {
						t.Fatalf("control: the child sent %d requests and ended %q; want its second request sent and completed", n, status)
					}
					return
				}
				if n != 1 {
					t.Fatalf("the child sent %d requests; want its first alone: the next, after the key was learned, carried it", n)
				}
				if status != SubagentFailed {
					t.Fatalf("the child ended %q; want failed, with the refusal", status)
				}
			})
		}
	}
}

// TestAKeyLearnedWhileASubagentRunsRedactsItsToolOutput (plan 034 C4r3, r13
// #4): a background child runs on while its parent idles, and an idle reload
// learns a key then — one in none of the child's frozen surfaces, so the
// child is not refused and goes on. The child's next tool reads a workspace
// file holding the key, a .env beside the code: the child installs the key at
// once, as AddSecrets does — it runs one turn, so a key left for its next one
// would never be installed — and the read's result, and so the child's next
// request, carries the marker and never the key. The control: the same child
// with nothing learned, whose next request carries the key, so the read's
// result does reach the request. Negative control: a LearnKeys that teaches
// the child the key without installing it (toolset.learn, X64's "the next
// turn") sends the key in the child's next request.
func TestAKeyLearnedWhileASubagentRunsRedactsItsToolOutput(t *testing.T) {
	for _, learned := range []bool{true, false} {
		name := "nothing learned"
		if learned {
			name = "a key learned while the child runs"
		}
		t.Run(name, func(t *testing.T) {
			const key = "sk-in-a-file-the-child-reads-0361"
			b := openBG(t)
			a := b.routers["test/a"]
			b.put(".env", "API_KEY="+key+"\n")
			w := newWorker()
			// The child's first step holds before it streams anything; once
			// released, it calls read on the file — after the key was learned
			// — and its second step answers.
			readIt := input(t, map[string]any{"filePath": ".env"})
			a.route("child work", w.step(nil, cat(callParts("c1", "read", readIt), finish(fantasy.FinishReasonToolCalls))), answerWith("done"))
			a.route("go", callStep(bgPart(t, "a1", "job", "child work")), answerWith("started"))
			var ev events
			if res, err := b.s.Run(context.Background(), "go", ev.sink); err != nil || res.StopReason != StopEndTurn {
				t.Fatalf("the spawning turn = %+v, %v; want end_turn", res, err)
			}
			await(t, w.reached, "the child's first step")
			id := startedWith(t, ev.list(), "child work").ID
			if got := requestText(a.requests("child work")[0], true); strings.Contains(got, key) {
				t.Fatal("premise: the child's first request already holds the key, so it is in a frozen surface")
			}
			if learned {
				if _, err := b.s.LearnKeys([]modeltable.Secret{key}); err != nil {
					t.Fatalf("LearnKeys = %v; want the key learned, in no frozen surface", err)
				}
			}
			close(w.release)
			await(t, b.pending, "the child's result")
			reqs := a.requests("child work")
			if len(reqs) != 2 {
				t.Fatalf("the child sent %d requests; want its second, after the read, sent", len(reqs))
			}
			next := requestText(reqs[1], true)
			if status := resultOf(t, b.s, id).status; status != SubagentCompleted {
				t.Fatalf("the child ended %q; want completed: the key is in none of its frozen surfaces", status)
			}
			if !learned {
				if !strings.Contains(next, key) {
					t.Fatalf("control: the child's next request does not hold the file's key, so its redaction proves nothing:\n%s", next)
				}
				return
			}
			if strings.Contains(next, key) || !strings.Contains(next, "API_KEY="+redact.Marker) {
				t.Fatalf("the child's next request carries the key it learned before its read, or not the marker:\n%s", next)
			}
		})
	}
}
