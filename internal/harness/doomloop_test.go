package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/tool"
)

// The doom-loop guard (plan 019 §3.7, D-42).

// nudge is §3.7's sentence for the nth consecutive identical call of tool.
func nudge(tool string, n int) string {
	return fmt.Sprintf("You have called %s with the same arguments %d times in a row. "+
		"Stop repeating this call; change your approach or explain what is blocking you.", tool, n)
}

// appendTo is a command that adds a line to path: a side effect a refused
// call cannot hide.
func appendTo(path, line string) string { return "echo " + line + " >> " + path }

// appended is what the calls wrote to path, or "" when none ran.
func appended(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// results maps each finished call's id to its class and text, so a test can
// read a step's calls whatever order their goroutines finished in.
func results(evs []Event) map[string]string {
	out := map[string]string{}
	for _, e := range of[ToolFinished](evs) {
		out[e.ID] = string(e.Result.Class) + ": " + e.Result.Text
	}
	return out
}

// TestDoomLoopRefusesAndStops: the same call, a step at a time. The first two
// run; the third and fourth are refused unrun, with the nudge, class
// doom_loop; the fifth is refused with the stop sentence too, and the turn
// ends there with max_turn_requests — before a sixth request, which the model
// had queued. The model reads each refusal, and a Diag records each. The
// negative control is the same steps with the command changed each time:
// every call runs and the turn answers.
func TestDoomLoopRefusesAndStops(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	s := f.open(f.options())
	counter := filepath.Join(f.workspace, "counter")
	var steps []step
	for i := range 6 {
		steps = append(steps, callStep(callParts(fmt.Sprintf("c%d", i), "bash",
			input(t, map[string]any{"command": appendTo(counter, "x")}))))
	}
	f.models["test/a"].push(steps...)

	var ev events
	res, err := s.Run(context.Background(), "loop", ev.sink)
	if err != nil || res.StopReason != StopMaxTurnRequests {
		t.Fatalf("Run = %+v, %v; want max_turn_requests", res, err)
	}
	if n := len(f.models["test/a"].requests()); n != 5 {
		t.Fatalf("the model was asked %d times; want 5, the turn stopping after the fifth identical call", n)
	}
	if got := appended(t, counter); got != "x\nx\n" {
		t.Fatalf("the command ran %q; want two lines, from the first two calls alone", got)
	}

	evs := ev.list()
	var want []string
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("t1.%d.1", i)
		want = append(want, "started "+id, "called "+id)
		class := ""
		if i >= doomNudgeAt {
			want, class = append(want, "diag doom_loop"), "doom_loop"
		}
		want = append(want, "finished "+id+" "+class, fmt.Sprintf("step %d tool-calls tool_use saved=true", i))
	}
	equal(t, "events", seq(evs), want)

	fin := of[ToolFinished](evs)
	for i, text := range map[int]string{2: nudge("bash", 3), 3: nudge("bash", 4), 4: nudge("bash", 5) + " This turn is being stopped."} {
		if fin[i].Result.Text != text || !fin[i].Result.IsError {
			t.Errorf("call %d's result = %q (error %v); want %q", i+1, fin[i].Result.Text, fin[i].Result.IsError, text)
		}
	}
	if fin[0].Result.IsError || fin[1].Result.IsError {
		t.Errorf("the first two calls failed: %q, %q", fin[0].Result.Text, fin[1].Result.Text)
	}
	// Only the fifth asks Fantasy to stop as well; the stop condition is what
	// actually ends the turn (halted), but a dispatched call carries the same
	// answer back.
	for i, want := range []bool{false, false, false, false, true} {
		if fin[i].Result.StopTurn != want {
			t.Errorf("call %d's StopTurn = %v, want %v", i+1, fin[i].Result.StopTurn, want)
		}
	}
	equal(t, "the nudges and the stop", of[Diag](evs), []Diag{
		{Kind: DiagDoomLoop, Fields: map[string]string{"step": "3", "id": "t1.3.1", "tool": "bash", "count": "3", "stopped": "false"}},
		{Kind: DiagDoomLoop, Fields: map[string]string{"step": "4", "id": "t1.4.1", "tool": "bash", "count": "4", "stopped": "false"}},
		{Kind: DiagDoomLoop, Fields: map[string]string{"step": "5", "id": "t1.5.1", "tool": "bash", "count": "5", "stopped": "true"}},
	})

	// The model was sent every refusal, so it had been told to stop before
	// the turn stopped it.
	tr := entries(transcript(t, s))
	if len(tr) != 11 {
		t.Fatalf("transcript:\n%s", strings.Join(tr, "\n"))
	}
	if got := tr[10]; got != "tool test/a high: [error c4: "+nudge("bash", 5)+" This turn is being stopped.]" {
		t.Fatalf("the last result the model was sent = %q", got)
	}

	t.Run("control: a changing command", func(t *testing.T) {
		f := newFixture(t, "http://127.0.0.1:1/v1")
		s := f.open(f.options())
		counter := filepath.Join(f.workspace, "counter")
		var steps []step
		for i := range 5 {
			steps = append(steps, callStep(callParts(fmt.Sprintf("c%d", i), "bash",
				input(t, map[string]any{"command": appendTo(counter, fmt.Sprint(i))}))))
		}
		f.models["test/a"].push(append(steps, answerWith("done"))...)
		var ev events
		res, err := s.Run(context.Background(), "loop", ev.sink)
		if err != nil || res.StopReason != StopEndTurn {
			t.Fatalf("Run = %+v, %v; want end_turn", res, err)
		}
		if got := appended(t, counter); got != "0\n1\n2\n3\n4\n" {
			t.Fatalf("the commands ran %q; want all five", got)
		}
		if d := of[Diag](ev.list()); len(d) != 0 {
			t.Fatalf("the guard fired on five different calls: %v", d)
		}
	})
}

// TestDoomLoopCountsCallsFantasyRefuses: a call Fantasy will not dispatch —
// arguments that do not fit the schema, or a tool that does not exist — never
// reaches a tool, so nothing but the guard can stop it repeating; it is
// counted, and the fifth ends the turn with max_turn_requests, five requests
// in rather than two hundred. Its card keeps Fantasy's own reason, which is
// what the model was sent: the veto is never consulted for a call that is
// never run. The negative control is five such calls with different
// arguments, where the turn runs on to its answer.
func TestDoomLoopCountsCallsFantasyRefuses(t *testing.T) {
	cases := []struct {
		name, tool string
		args       func(i int) string
	}{
		{"invalid arguments", "read", func(i int) string { return fmt.Sprintf(`{"offset":"x%d"}`, i) }},
		{"a tool that does not exist", "nope", func(i int) string { return fmt.Sprintf(`{"n":%d}`, i) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			var steps []step
			for i := range 6 {
				steps = append(steps, callStep(callParts(fmt.Sprintf("c%d", i), tc.tool, tc.args(0))))
			}
			f.models["test/a"].push(steps...)
			var ev events
			res, err := s.Run(context.Background(), "loop", ev.sink)
			if err != nil || res.StopReason != StopMaxTurnRequests {
				t.Fatalf("Run = %+v, %v; want max_turn_requests", res, err)
			}
			if n := len(f.models["test/a"].requests()); n != 5 {
				t.Fatalf("the model was asked %d times; want 5", n)
			}
			d := of[Diag](ev.list())
			if len(d) != 3 || d[2].Fields["count"] != "5" || d[2].Fields["stopped"] != "true" {
				t.Fatalf("the guard reported %v; want the third, fourth and fifth calls, the last stopping the turn", d)
			}
			fin := of[ToolFinished](ev.list())
			if last := fin[len(fin)-1].Result; last.Class != "invalid_input" || strings.Contains(last.Text, "times in a row") {
				t.Fatalf("the refused call's result = %+v; want Fantasy's own reason", last)
			}
		})
		t.Run(tc.name+", control: different arguments", func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			var steps []step
			for i := range 5 {
				steps = append(steps, callStep(callParts(fmt.Sprintf("c%d", i), tc.tool, tc.args(i))))
			}
			f.models["test/a"].push(append(steps, answerWith("done"))...)
			var ev events
			res, err := s.Run(context.Background(), "loop", ev.sink)
			if err != nil || res.StopReason != StopEndTurn {
				t.Fatalf("Run = %+v, %v; want end_turn", res, err)
			}
			if d := of[Diag](ev.list()); len(d) != 0 {
				t.Fatalf("the guard fired on five different calls: %v", d)
			}
		})
	}
}

// TestDoomLoopResetsOnADifferentCall: "consecutive" is consecutive in call
// order, so one different call in the middle clears the count and all five
// run. The negative control is the same run without that one call, where the
// third is refused and the fifth stops the turn.
func TestDoomLoopResetsOnADifferentCall(t *testing.T) {
	for _, reset := range []bool{true, false} {
		t.Run(fmt.Sprintf("reset=%v", reset), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			counter := filepath.Join(f.workspace, "counter")
			cmds := []string{"a", "a", "a", "a", "a"}
			if reset {
				cmds[2] = "b"
			}
			var steps []step
			for i, line := range cmds {
				steps = append(steps, callStep(callParts(fmt.Sprintf("c%d", i), "bash",
					input(t, map[string]any{"command": appendTo(counter, line)}))))
			}
			f.models["test/a"].push(append(steps, answerWith("done"))...)

			var ev events
			res, err := s.Run(context.Background(), "loop", ev.sink)
			if err != nil {
				t.Fatalf("Run = %+v, %v", res, err)
			}
			if !reset {
				if got := appended(t, counter); res.StopReason != StopMaxTurnRequests || got != "a\na\n" {
					t.Fatalf("control: stop %q after %q; want max_turn_requests and the first two calls alone", res.StopReason, got)
				}
				return
			}
			if res.StopReason != StopEndTurn {
				t.Fatalf("Run = %+v; want end_turn", res)
			}
			if got := appended(t, counter); got != "a\na\nb\na\na\n" {
				t.Fatalf("the commands ran %q; want all five, the different one clearing the count", got)
			}
			if d := of[Diag](ev.list()); len(d) != 0 {
				t.Fatalf("the guard fired: %v", d)
			}
		})
	}
}

// TestDoomLoopCountsParallelCallsInCallOrder: three identical calls in one
// step, of a tool Fantasy runs in parallel. Every OnToolCall runs before any
// dispatch, so the guard counts them in the order the model made them: the
// first two read the file, the third is refused. Three is not five, so the
// turn goes on and answers. The negative control is the same step with three
// different calls, all of which read.
func TestDoomLoopCountsParallelCallsInCallOrder(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(fmt.Sprintf("same=%v", same), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			names := []string{"a.txt", "a.txt", "a.txt"}
			if !same {
				names = []string{"a.txt", "b.txt", "c.txt"}
			}
			for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
				f.put(n, "content of "+n+"\n")
			}
			var parts [][]fantasy.StreamPart
			for i, n := range names {
				parts = append(parts, callParts(fmt.Sprintf("c%d", i+1), "read", input(t, map[string]any{"filePath": n})))
			}
			f.models["test/a"].push(callStep(parts...), answerWith("done"))

			var ev events
			res, err := s.Run(context.Background(), "read it", ev.sink)
			if err != nil || res.StopReason != StopEndTurn {
				t.Fatalf("Run = %+v, %v; want end_turn", res, err)
			}
			got := results(ev.list())
			if len(got) != 3 || !strings.Contains(got["t1.1.1"], "content of a.txt") || !strings.Contains(got["t1.1.2"], "content of "+names[1]) {
				t.Fatalf("the first two calls = %v", got)
			}
			third := got["t1.1.3"]
			if !same {
				if !strings.Contains(third, "content of c.txt") {
					t.Fatalf("control: the third call = %q; want the file it asked for", third)
				}
				if d := of[Diag](ev.list()); len(d) != 0 {
					t.Fatalf("control: the guard fired on three different calls: %v", d)
				}
				return
			}
			if third != "doom_loop: "+nudge("read", 3) {
				t.Fatalf("the third call = %q; want the nudge", third)
			}
			equal(t, "the nudge", of[Diag](ev.list()), []Diag{{Kind: DiagDoomLoop, Fields: map[string]string{
				"step": "1", "id": "t1.1.3", "tool": "read", "count": "3", "stopped": "false"}}})
		})
	}
}

// TestDoomLoopStopsWhateverFinishCarriedTheFifthCall: Fantasy announces a
// step's calls on any finish but the abnormal four, and dispatches them only
// on "tool-calls", so a fifth identical call arriving under "stop" or "other"
// never reaches runTool and its veto is never returned there. The runner must
// answer it with the guard's text all the same — D-43's "not executed, its
// arguments may be truncated" would tell the model the opposite of what
// happened — leave that step's not_executed Diag unclaimed, and end the turn
// with max_turn_requests rather than the step's own end_turn. The negative
// control is a different fifth call under the same finish: it gets D-43's
// result and Diag, and the turn ends end_turn.
func TestDoomLoopStopsWhateverFinishCarriedTheFifthCall(t *testing.T) {
	for _, fr := range []fantasy.FinishReason{fantasy.FinishReasonStop, fantasy.FinishReasonOther} {
		for _, same := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/same=%v", fr, same), func(t *testing.T) {
				f := newFixture(t, "http://127.0.0.1:1/v1")
				s := f.open(f.options())
				counter := filepath.Join(f.workspace, "counter")
				args := func(line string) string { return input(t, map[string]any{"command": appendTo(counter, line)}) }
				var steps []step
				for i := range 4 {
					steps = append(steps, callStep(callParts(fmt.Sprintf("c%d", i), "bash", args("x"))))
				}
				fifth := args("x")
				if !same {
					fifth = args("y")
				}
				f.models["test/a"].push(append(steps, reply(callParts("c4", "bash", fifth), finish(fr)), answerWith("done"))...)

				var ev events
				res, err := s.Run(context.Background(), "loop", ev.sink)
				if err != nil {
					t.Fatalf("Run = %+v, %v", res, err)
				}
				fin, diags := of[ToolFinished](ev.list()), of[Diag](ev.list())
				last, tr := fin[len(fin)-1].Result, entries(transcript(t, s))
				recorded := tr[len(tr)-1]

				if !same {
					if res.StopReason != StopEndTurn {
						t.Fatalf("control: stop reason %q, want end_turn", res.StopReason)
					}
					if last.Class != tool.ClassNotExecuted || !strings.Contains(recorded, "was not executed") {
						t.Fatalf("control: the fifth call = %+v, recorded as %q", last, recorded)
					}
					if d := diags[len(diags)-1]; d.Kind != DiagNotExecuted || d.Fields["calls"] != "1" {
						t.Fatalf("control: the last Diag = %+v, want one not_executed call", d)
					}
					return
				}
				if res.StopReason != StopMaxTurnRequests {
					t.Fatalf("stop reason %q, want max_turn_requests", res.StopReason)
				}
				want := nudge("bash", 5) + " This turn is being stopped."
				if last.Class != tool.ClassDoomLoop || last.Text != want {
					t.Fatalf("the fifth call = %+v; want the guard's text", last)
				}
				if recorded != "tool test/a high: [error c4: "+want+"]" {
					t.Fatalf("the model was sent %q", recorded)
				}
				for _, d := range diags {
					if d.Kind != DiagDoomLoop {
						t.Fatalf("a %s Diag claimed a call the guard refused: %+v", d.Kind, d)
					}
				}
				if got := appended(t, counter); got != "x\nx\n" {
					t.Fatalf("the command ran %q; want the first two calls alone", got)
				}
				if n := len(f.models["test/a"].requests()); n != 5 {
					t.Fatalf("the model was asked %d times; want 5", n)
				}
			})
		}
	}
}

// TestCallSignature: the signature ignores how the arguments were written —
// key order at any depth, and whitespace — and nothing else. A value, an
// array's order, the tool's name, and arguments that are not one JSON value
// at all, each tell two calls apart; and a number keeps the literal it was
// written as, so the guard would rather miss a loop than refuse a call the
// model did not make twice.
func TestCallSignature(t *testing.T) {
	cases := []struct {
		what       string
		a, b       [2]string // tool name, raw arguments
		wantEquals bool
	}{
		{"key order", [2]string{"read", `{"a":1,"b":2}`}, [2]string{"read", `{"b":2,"a":1}`}, true},
		{"nested key order", [2]string{"read", `{"o":{"a":1,"b":2},"z":3}`}, [2]string{"read", `{"z":3,"o":{"b":2,"a":1}}`}, true},
		{"whitespace", [2]string{"read", `{"a": 1,  "b":[1, 2]}`}, [2]string{"read", `{"a":1,"b":[1,2]}`}, true},
		{"the same invalid arguments", [2]string{"read", `{"a":`}, [2]string{"read", `{"a":`}, true},
		// An integer's spelling: the tools read 5, 5.0, 5e0 and 5.00 as the
		// integer 5 (args.integer), so the guard must too, or a model
		// alternating spellings loops for ever.
		{"5 and 5.0", [2]string{"read", `{"a":5}`}, [2]string{"read", `{"a":5.0}`}, true},
		{"5 and 5e0", [2]string{"read", `{"a":5}`}, [2]string{"read", `{"a":5e0}`}, true},
		{"5.0 and 5.00", [2]string{"read", `{"a":5.0}`}, [2]string{"read", `{"a":5.00}`}, true},
		{"a fraction's spelling", [2]string{"read", `{"a":0.5}`}, [2]string{"read", `{"a":5e-1}`}, true},
		{"a value", [2]string{"read", `{"a":1}`}, [2]string{"read", `{"a":2}`}, false},
		{"two numbers that are not the same", [2]string{"read", `{"a":5.0}`}, [2]string{"read", `{"a":5.5}`}, false},
		// Past float64's exact integer range two literals that are different
		// numbers round to one float, so they keep the spelling they came
		// with: the guard misses a loop rather than refusing a call the model
		// did not make twice.
		{"integers past float64's exact range", [2]string{"read", `{"a":9007199254740993}`}, [2]string{"read", `{"a":9007199254740992}`}, false},
		{"an array's order", [2]string{"read", `{"a":[1,2]}`}, [2]string{"read", `{"a":[2,1]}`}, false},
		{"the tool", [2]string{"read", `{"a":1}`}, [2]string{"grep", `{"a":1}`}, false},
		{"different invalid arguments", [2]string{"read", `{"a":`}, [2]string{"read", `{"b":`}, false},
		{"trailing junk", [2]string{"read", `{"a":1}`}, [2]string{"read", `{"a":1} {"a":1}`}, false},
	}
	for _, tc := range cases {
		a, b := callSignature(tc.a[0], tc.a[1]), callSignature(tc.b[0], tc.b[1])
		if (a == b) != tc.wantEquals {
			t.Errorf("%s: %s %s and %s %s signed %q and %q; want equal=%v",
				tc.what, tc.a[0], tc.a[1], tc.b[0], tc.b[1], a, b, tc.wantEquals)
		}
	}
	// What the spellings are canonicalized to: an integer to its integer
	// literal, and a number past float64's exact range to nothing at all —
	// never to a nearby integer it is not.
	for input, want := range map[string]string{
		`{"a":5.00,"b":-0.0,"c":5e0}`: `{"a":5,"b":0,"c":5}`,
		`{"a":9007199254740993}`:      `{"a":9007199254740993}`,
		`{"a":1e999}`:                 `{"a":1e999}`, // not a float64 at all
	} {
		if got := canonicalJSON(input); got != want {
			t.Errorf("canonicalJSON(%s) = %s, want %s", input, got, want)
		}
	}
	// The name and the arguments cannot run together: a name that ends where
	// another's arguments begin still signs differently.
	if callSignature(`a"`, `{}`) == callSignature("a", `"{}`) {
		t.Error("the name and the arguments run together")
	}
}

// TestDoomLoopCanonicalizesTheRealCall: the canonical signature reaches the
// live path — the same call written with its keys in three orders, and its
// limit once as 2 and once as 2.0, is one call, and the third is refused. The
// negative control is the same three calls with the last one's limit changed
// to a different number, which runs.
func TestDoomLoopCanonicalizesTheRealCall(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(fmt.Sprintf("same=%v", same), func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			s := f.open(f.options())
			f.put("a.txt", "alpha\nbeta\n")
			third := `{"limit":2.0,"offset":0,"filePath":"a.txt"}`
			if !same {
				third = `{"limit":1,"offset":0,"filePath":"a.txt"}`
			}
			f.models["test/a"].push(
				callStep(callParts("c1", "read", `{"filePath":"a.txt","offset":0,"limit":2}`)),
				callStep(callParts("c2", "read", `{"offset":0,"limit":2,"filePath":"a.txt"}`)),
				callStep(callParts("c3", "read", third)),
				answerWith("done"),
			)
			var ev events
			if res, err := s.Run(context.Background(), "read it", ev.sink); err != nil || res.StopReason != StopEndTurn {
				t.Fatalf("Run = %+v, %v; want end_turn", res, err)
			}
			got := results(ev.list())["t1.3.1"]
			if !same {
				if !strings.Contains(got, "alpha") {
					t.Fatalf("control: the third call = %q; want the file", got)
				}
				return
			}
			if got != "doom_loop: "+nudge("read", 3) {
				t.Fatalf("the third call = %q; want the nudge", got)
			}
		})
	}
}
