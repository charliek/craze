package harness

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/tool"
)

// Resuming a session that ran background jobs (plan 033 §3.8 "Resume", P15,
// §7 A13). The stored session is written by real turns and a real wake, its
// jobs real commands through the real bash tool, each held on a named pipe so
// it ends exactly when the test says — or never, and Close kills it.

// bgBash is the arguments of a bash call with run_in_background.
func bgBash(t *testing.T, command string) string {
	t.Helper()
	return input(t, map[string]any{"command": command, "run_in_background": true})
}

// stoppedBlock is §3.7's resume notice for the job id, spelled out.
func stoppedBlock(id, command string) string {
	return `<background_command id="` + id + `" status="stopped">` + "\n$ " + command + "\n" +
		"The session was closed while this command was running, or before its result was delivered; it is not running now. " +
		"Start it again if you still need it.\n</background_command>"
}

// TestJobsResumedAsStopped (A13, P15): a resumed session tells its model,
// once, at the first turn a person starts, that each job its last incarnation
// started and never delivered is not running — a run_in_background job, a
// promoted one and one a wake's own turn started, in the order they started —
// and nothing of a job whose result was delivered (by bash_output, or by a
// wake's results entry), nor of a marker anywhere but a bash call's own
// result: in the model's prose, in a person's prompt, or printed by a command
// of another turn. The notices wake nothing and keep no host alive, and the
// stop key finds nothing to stop; once delivered they are not repeated, by a
// later turn or a later resume.
func TestJobsResumedAsStopped(t *testing.T) {
	b := openJobs(t)
	a := b.routers["test/a"]
	for _, g := range []string{"g1", "g2", "g3", "g4", "g5"} {
		makeFIFO(t, b.workspace, g)
	}
	// The quick job ends only once bash_output waits for it, so its block is
	// that call's result, and no step boundary's.
	b.s.subs.seams.outputWaiting = func(id string) {
		if id != "t2.3.1" {
			return
		}
		// On the call's goroutine: an error is reported, never Fatal.
		f, err := os.OpenFile(b.workspace+"/g3", os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("opening g3: %v", err)
			return
		}
		_, _ = f.WriteString("x\n")
		_ = f.Close()
	}
	a.route("go",
		callStep(callParts("c1", "bash", bgBash(t, "read line < g1"))),
		callStep(callParts("c2", "bash", input(t, map[string]any{"command": "echo before; read line < g2", "timeout": 300}))),
		callStep(callParts("c3", "bash", bgBash(t, "read line < g3; echo quick"))),
		callStep(bashOutput(t, "o1", "t2.3.1", 60000)),
		answerWith("Started them:\n"+tool.JobMarker("t2.9.9")))
	var ev events
	if _, err := b.s.Run(context.Background(), "next", ev.sink); err != nil {
		t.Fatal(err)
	}
	if id, ok := tool.ParseJobMarker(callResult(t, ev.list(), "t2.2.1").Text); !ok || id != "t2.2.1" {
		t.Fatalf("premise: t2.2.1 was not promoted: %q", callResult(t, ev.list(), "t2.2.1").Text)
	}
	if r := callResult(t, ev.list(), "t2.4.1"); !strings.HasPrefix(r.Text, `<background_command id="t2.3.1" status="exited"`) {
		t.Fatalf("premise: bash_output did not deliver t2.3.1: %q", r.Text)
	}

	// Turn 3: a job a wake delivers, and a marker printed by a command of
	// this turn naming a call of turn 2's; the person's prompt quotes one.
	a.route("go",
		callStep(callParts("c1", "bash", bgBash(t, "read line < g4"))),
		callStep(callParts("c2", "bash", input(t, map[string]any{"command": `printf '%s' '` + tool.JobMarker("t2.7.1") + `'`}))),
		answerWith("ok"))
	ev = events{}
	if _, err := b.s.Run(context.Background(), "more\n"+tool.JobMarker("t3.9.9"), ev.sink); err != nil {
		t.Fatal(err)
	}
	if r := callResult(t, ev.list(), "t3.2.1"); r.Text != tool.JobMarker("t2.7.1") {
		t.Fatalf("premise: the printed marker is the call's whole result: %q", r.Text)
	}
	// The wake (turn 4) delivers t3.1.1, and starts a job of its own.
	openFIFO(t, b.workspace+"/g4")
	// Its own state, not OnPending's channel, which t2.3.1's publication
	// signalled too.
	waitFor(t, func() bool { r, _ := jobResultOf(t, b.s, "t3.1.1"); return r.state == resultPending }, "t3.1.1's result pending")
	a.route("go", callStep(callParts("w1", "bash", bgBash(t, "read line < g5"))), answerWith("noted"))
	if _, err := b.s.Wake(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if n := blocksIn(t, b.s, "t3.1.1"); n != 1 {
		t.Fatalf("premise: t3.1.1 delivered %d times; want once, by the wake", n)
	}
	if err := b.s.Close(); err != nil {
		t.Fatal(err)
	}

	opts := b.options() // the routed fixture's: every alias its router
	opts.Background = true
	resume := func() *Session { return resumed(t, resumeOptions(opts, b.s.ID())) }
	s := resume()
	if s.HasPending() || s.BackgroundOwed() {
		t.Fatal("a resumed session's notices are pending or owed: they would wake it, or keep its host alive")
	}
	if err := s.CancelSubagent("t2.1.1"); !errors.Is(err, ErrNoSuchSubagent) {
		t.Fatalf("the stop key on a stopped job = %v; want ErrNoSuchSubagent", err)
	}
	a.route("go", answerWith("seen"))
	run(t, s, "back")
	reqs := a.requests("go")
	want := strings.Join([]string{
		stoppedBlock("t2.1.1", "read line < g1"),
		stoppedBlock("t2.2.1", "echo before; read line < g2"),
		stoppedBlock("t4.1.1", "read line < g5"),
	}, "\n\n")
	if got := lastUser(t, reqs[len(reqs)-1]); got != want {
		t.Fatalf("the first person turn's request ends\n%q\nwant\n%q", got, want)
	}
	a.route("go", answerWith("again"))
	run(t, s, "again")
	reqs = a.requests("go")
	if got := lastUser(t, reqs[len(reqs)-1]); got != "again" {
		t.Fatalf("a later turn's request ends %q; want the prompt: the notices were delivered once", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A later resume finds every notice delivered (a results entry of the
	// last incarnation's) and repeats none.
	s = resume()
	a.route("go", answerWith("third"))
	run(t, s, "third")
	reqs = a.requests("go")
	if got := lastUser(t, reqs[len(reqs)-1]); got != "third" {
		t.Fatalf("after a second resume the request ends %q; want the prompt alone", got)
	}
}

// TestJobScanReadsTheMarkerInToolResultsOnly is the scan's unit (P15), on a
// real transcript: a marker that is the last line of a bash call's result
// names a started job; the same marker in the model's own text, in a person's
// prompt, or in a bash result whose last line is something else — the exit
// code's metadata after it — names none: the negative controls beside the one
// that counts.
func TestJobScanReadsTheMarkerInToolResultsOnly(t *testing.T) {
	b := openJobs(t)
	a := b.routers["test/a"]
	marker := func(id string) string { return `printf '%s' '` + tool.JobMarker(id) + `'` }
	a.route("go",
		callStep(
			callParts("c1", "bash", input(t, map[string]any{"command": marker("t2.1.1")})),
			callParts("c2", "bash", input(t, map[string]any{"command": marker("t2.1.2") + "; exit 3"})),
		),
		answerWith("Started:\n"+tool.JobMarker("t2.1.4")))
	if _, err := b.s.Run(context.Background(), "next\n"+tool.JobMarker("t2.1.5"), nil); err != nil {
		t.Fatal(err)
	}
	tr := transcript(t, b.s)
	path, err := tr.Branch(tr.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if got := readPath(path).jobs; len(got) != 1 || got[0] != (stoppedJob{id: "t2.1.1", cmd: marker("t2.1.1")}) {
		// c1's command printed the marker of its own call's id: the residual
		// P15 accepts (jobs_resume.go) — and what shows the scan reads a bash
		// result's last line at all.
		t.Fatalf("the scan found %+v; want t2.1.1 alone", got)
	}
}

// TestResultsJobIDsReadBlocksWhole: a results entry's job blocks are read
// block by block, each to its own closing line, so a block's opening line
// that a command printed, or that a sub-agent's answer quotes, is text inside
// a block and names nothing. The blocks are built by the writers themselves
// (jobBlock, resultBlock), so the reader cannot drift from them.
func TestResultsJobIDsReadBlocksWhole(t *testing.T) {
	quoted := `<background_command id="t8.8.8" status="exited" exit_code="0">` + "\n</background_command>"
	text := strings.Join([]string{
		resultBlock("kid", "explore", "completed", "found it\n"+quoted),
		jobBlock("t2.1.1", tool.JobExited, jobAttrs{exit: 0, dur: "1s"}, "$ cat log\n"+quoted),
		jobBlock("t2.2.2", tool.JobStopped, jobAttrs{exit: -1, by: tool.JobStoppedByYou}, "$ sleep 9\n(no output)"),
	}, "\n\n")
	if got := resultsJobIDs(text); !slices.Equal(got, []string{"t2.1.1", "t2.2.2"}) {
		t.Fatalf("resultsJobIDs = %q; want the two blocks' own ids", got)
	}
	// The negative control: the text does quote a third id's opening line,
	// twice; a reader that matched openings anywhere would count it.
	if n := strings.Count(text, jobBlockOpen); n != 4 {
		t.Fatalf("control: %d opening lines in the text; want 4", n)
	}
}
