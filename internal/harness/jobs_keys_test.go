package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// A background job's stream learns the keys its session learns while it runs
// (plan 033 C10r, review r7 finding 9, superseding §3.7's "a running job's
// stream and spill file keep their call-time Replacer"). The job is a real
// command run by the real bash tool, gated by named pipes so each of its
// writes lands where the test needs it; the key is learned the way the
// adapter teaches a session one, LearnKeys at a turn's start; and the model's
// reads are committed bash_output calls. Each case's control is the same
// schedule with the key never learned — the model then does get the key's
// pieces — so what the case shows is the learning's doing. The negative
// control for the fix itself is the same test with the session's stream
// tracking taken out, which fails it (see the C10r report).

// learnedKey is the key a session learns mid-job; it shares nothing with the
// fixture's keys or the marker.
const learnedKey = "zq-learned-mid-job-7f3a9c"

// jobScript is a background job's command and what it prints at each of its
// gates: gate i (1-based) lets it print prints[i-1], and the last gate lets
// it exit. Each print is one file and so one write — what cat prints of it
// lands in one read of the pipe — so seeing its visible part is seeing it
// all.
type jobScript struct {
	b      *bg
	id     string
	gates  []string
	prints []string
}

// startScript starts, in turn "next", a background job printing prints one at
// a time, each behind a gate, and waits for its receipt.
func startScript(t *testing.T, b *bg, prints ...string) *jobScript {
	t.Helper()
	j := &jobScript{b: b, id: "t2.1.1", prints: prints}
	var cmd []string
	for i, p := range append(slices.Clone(prints), "") {
		gate := makeFIFO(t, b.workspace, "g"+string(rune('1'+i)))
		j.gates = append(j.gates, gate)
		cmd = append(cmd, "read x < "+filepath.Base(gate))
		if p != "" {
			name := "p" + string(rune('1'+i))
			if err := os.WriteFile(filepath.Join(b.workspace, name), []byte(p), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd = append(cmd, "cat "+name)
		}
	}
	in := input(t, map[string]any{"command": strings.Join(cmd, "; "), "run_in_background": true})
	b.routers["test/a"].route("go", callStep(callParts("c1", "bash", in)), answerWith("started"))
	var ev events
	if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
		t.Fatal(err)
	}
	if r := callResult(t, ev.list(), j.id); r.IsError || !strings.Contains(r.Text, tool.JobMarker(j.id)) {
		t.Fatalf("the job's receipt = %+v", r)
	}
	return j
}

// output is everything the job's output holds now, from its first byte.
func (j *jobScript) output(t *testing.T) string {
	t.Helper()
	j.b.s.subs.regMu.Lock()
	res := j.b.s.subs.results[j.id]
	j.b.s.subs.regMu.Unlock()
	if res == nil || res.job == nil {
		t.Fatalf("no job %s", j.id)
	}
	return res.job.body.Output(0).Text
}

// print opens gate i (from 1) and waits until the job's output shows visible:
// the print behind the gate has gone through the stream.
func (j *jobScript) print(t *testing.T, i int, visible string) {
	t.Helper()
	openFIFO(t, j.gates[i-1])
	waitFor(t, func() bool { return strings.Contains(j.output(t), visible) }, "the job's output showing "+visible)
}

// read runs one turn whose step reads the job (bash_output, a snapshot) after
// before has run, and returns what the read answered. Each read is its own
// turn, so its step's append commits it.
func (j *jobScript) read(t *testing.T, prompt string, before func()) string {
	t.Helper()
	a := j.b.routers["test/a"]
	a.route("go", func(ctx context.Context, yield func(fantasy.StreamPart) bool) {
		before()
		callStep(bashOutput(t, "o", j.id, 0))(ctx, yield)
	}, answerWith("read"))
	var ev events
	if _, err := j.b.s.Run(context.Background(), prompt, ev.sink); err != nil {
		t.Fatal(err)
	}
	fins := of[ToolFinished](ev.list())
	if len(fins) != 1 || fins[0].Result.IsError {
		t.Fatalf("the read = %+v", fins)
	}
	return fins[0].Result.Text
}

// finish opens the job's last gate and waits for its result to be pending:
// its spill file is complete then. It returns the spill file's contents.
func (j *jobScript) finish(t *testing.T) string {
	t.Helper()
	openFIFO(t, j.gates[len(j.gates)-1])
	if !await(t, j.b.pending, "the job's result") {
		t.Fatal("nothing pending")
	}
	raw, err := os.ReadFile(filepath.Join(j.b.home, tool.SpillDir, "tool_"+j.id))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// learn teaches the session key as the adapter does at a turn's start.
func learn(t *testing.T, s *Session, key string) {
	t.Helper()
	if skipped, err := s.LearnKeys([]modeltable.Secret{modeltable.Secret(key)}); skipped != nil || err != nil {
		t.Fatalf("LearnKeys = %v, %v", skipped, err)
	}
}

// payload is a bash_output answer's output: what follows its head.
func payload(t *testing.T, answer string) string {
	t.Helper()
	if _, rest, ok := strings.Cut(answer, " Output since your last read:\n"); ok {
		return rest
	}
	if strings.HasSuffix(answer, jobNoNewOutput) {
		return ""
	}
	t.Fatalf("not a running job's read: %q", answer)
	return ""
}

// modelSaw is every request the model was sent from the first, as one text:
// the tool results among them.
func modelSaw(b *bg) string {
	var all strings.Builder
	for _, c := range b.routers["test/a"].requests("go") {
		all.WriteString(requestText(c, true))
	}
	return all.String()
}

// jobOutputs are the JobOutput snapshots the session's sink saw, in order.
func jobOutputs(b *bg) []string {
	var out []string
	for _, e := range of[JobOutput](b.own.list()) {
		out = append(out, e.Output)
	}
	return out
}

// TestJobStreamLearnsSessionKeys: a key the session learns while a job runs
// reaches the model in no piece — in neither of two committed bash_output
// reads that split it, nor in their concatenation, nor in the spill file the
// reads name, nor in a JobOutput snapshot — however the job prints it after
// the session learned it: split across two writes and two reads, whole, or
// completing a prefix the stream was holding back as the start of a key it
// knew already when the key was learned. One the job printed before the
// session learned it is redacted in every JobOutput snapshot from then on
// (the snapshot is the output's tail, where it is whole).
func TestJobStreamLearnsSessionKeys(t *testing.T) {
	half := len(learnedKey) / 2
	pieces := []string{learnedKey[:half], learnedKey[half:]}

	t.Run("split across two writes and two reads", func(t *testing.T) {
		for _, learned := range []bool{true, false} {
			b := openJobs(t)
			j := startScript(t, b, "token: "+learnedKey[:half], learnedKey[half:]+"\ndone\n")
			if learned {
				learn(t, b.s, learnedKey)
			}
			first := j.read(t, "first", func() { j.print(t, 1, "token: ") })
			second := j.read(t, "second", func() { j.print(t, 2, "done\n") })
			spill := j.finish(t)
			got := payload(t, first) + payload(t, second)
			if !learned {
				// The control: the schedule does split the key across the reads.
				if !strings.Contains(first, pieces[0]) || !strings.Contains(second, pieces[1]) || !strings.Contains(spill, learnedKey) {
					t.Fatalf("control: with the key never learned the reads are %q, %q and the spill %q: the schedule does not split it", first, second, spill)
				}
				continue
			}
			for _, p := range pieces {
				if strings.Contains(first+second, p) || strings.Contains(modelSaw(b), p) {
					t.Fatalf("a piece of the learned key, %q, reached the model: reads %q, %q", p, first, second)
				}
			}
			if strings.Contains(got, learnedKey) || strings.Contains(spill, learnedKey) {
				t.Fatalf("the reads together (%q) or the spill file (%q) hold the learned key", got, spill)
			}
			if want := "token: " + redact.Marker + "\ndone\n"; got != want || spill != want {
				t.Fatalf("the reads together are %q and the spill file %q; want %q", got, spill, want)
			}
			for _, snap := range jobOutputs(b) {
				if strings.Contains(snap, pieces[0]) || strings.Contains(snap, pieces[1]) {
					t.Fatalf("a JobOutput snapshot holds a piece of the key: %q", snap)
				}
			}
		}
	})

	t.Run("whole, after it was learned", func(t *testing.T) {
		b := openJobs(t)
		j := startScript(t, b, "key="+learnedKey+"\n")
		learn(t, b.s, learnedKey)
		answer := j.read(t, "read", func() { j.print(t, 1, "key=") })
		spill := j.finish(t)
		if strings.Contains(answer, learnedKey) || strings.Contains(spill, learnedKey) || strings.Contains(strings.Join(jobOutputs(b), "\n"), learnedKey) {
			t.Fatalf("the learned key reached the read %q, the spill file %q or a snapshot", answer, spill)
		}
		if want := "key=" + redact.Marker + "\n"; payload(t, answer) != want || spill != want {
			t.Fatalf("read %q, spill %q; want %q", payload(t, answer), spill, want)
		}
	})

	t.Run("learned while its start was held as a known key's", func(t *testing.T) {
		// The fixture's key is canary, "sk-canary-not-a-secret": the stream
		// holds "sk-canary-not-" back as its possible start. The key learned
		// then begins with those same bytes.
		const held = "sk-canary-not-"
		const key = held + "learned-0099"
		if !strings.HasPrefix(canary, held) {
			t.Fatal("the fixture's key changed: the held prefix is no longer its start")
		}
		b := openJobs(t)
		j := startScript(t, b, "token: "+held, key[len(held):]+"\ndone\n")
		first := j.read(t, "first", func() { j.print(t, 1, "token: ") })
		if strings.Contains(first, held) {
			t.Fatalf("control: the prefix was not held back (%q), so its being kept proves nothing", first)
		}
		learn(t, b.s, key)
		second := j.read(t, "second", func() { j.print(t, 2, "done\n") })
		spill := j.finish(t)
		if strings.Contains(first+second, key[len(held):]) || strings.Contains(spill, key) {
			t.Fatalf("the key learned while its start was held reached the model: reads %q, %q; spill %q", first, second, spill)
		}
		if want := "token: " + redact.Marker + "\ndone\n"; payload(t, first)+payload(t, second) != want || spill != want {
			t.Fatalf("reads %q, spill %q; want %q", payload(t, first)+payload(t, second), spill, want)
		}
	})

	t.Run("printed before it was learned: the snapshots", func(t *testing.T) {
		b := openJobs(t)
		j := startScript(t, b, "key="+learnedKey+"\n", "more\n")
		j.print(t, 1, "key=")
		waitFor(t, func() bool { return strings.Contains(strings.Join(jobOutputs(b), "\n"), learnedKey) },
			"control: a snapshot of the key before it was learned")
		learn(t, b.s, learnedKey)
		j.print(t, 2, "more\n")
		waitFor(t, func() bool { return strings.Contains(strings.Join(jobOutputs(b), "\n"), "more") }, "a snapshot after the key was learned")
		snaps := jobOutputs(b)
		from := len(snaps) - 1
		for from > 0 && strings.Contains(snaps[from-1], "more") {
			from--
		}
		for _, snap := range snaps[from:] {
			if strings.Contains(snap, learnedKey) || !strings.Contains(snap, redact.Marker) {
				t.Fatalf("a snapshot taken after the key was learned = %q; want the key redacted", snap)
			}
		}
		j.finish(t)
	})
}
