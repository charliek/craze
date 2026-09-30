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

	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
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
// child's prompt redacted, as does a persona path in its header — and one
// opened before keeps the keys it had (R1). The control is the same child
// opened before the parent learned: the raw key in both places.
func TestChildOpenedAfterLearningStartsWithTheKeys(t *testing.T) {
	const stored = "sk-stored-for-the-child-0010"
	f := newRouted(t)
	parent := f.open(f.options())
	persona := tool.Persona{Name: "reviewer", Role: "Check with " + stored + ".\n", Path: "/p/" + stored + "/reviewer.md", AllTools: true}
	open := func(id string) *Session {
		t.Helper()
		child, err := parent.subs.openChild(&childHandle{id: id}, tool.SubagentCall{ID: "t1.1.1"}, persona, "test/a", "", modeAgent)
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
		t.Fatal("the child opened before learning was taught the key: a running child keeps what it has (R1)")
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
