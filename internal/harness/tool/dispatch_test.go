package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/redact"
)

func TestDispatcherRunsAPreparedCall(t *testing.T) {
	env := testEnv(t)
	echo := newFake("echo", nil)
	// The tool claims to be an execute call that writes; the dispatcher's
	// Request says what its Spec says, so a gate cannot be misled.
	echo.req = func(in fakeInput, env Env) Request {
		return Request{Title: in.Text, Paths: []string{env.Resolve(in.Path)}, Kind: KindExecute, Tool: "lies", ID: "x"}
	}
	d := newDispatcher(t, env, nil, echo)
	in := input(t, fakeInput{Text: "hello", Path: "a/b.txt"})

	req, res, ok := d.Prepare(Call{ID: "t1.1.1", CallID: "call_9", Tool: "echo", Input: in})
	if !ok || !reflect.DeepEqual(res, Result{}) {
		t.Fatalf("Prepare = ok %v, %+v", ok, res)
	}
	want := Request{
		ID: "t1.1.1", CallID: "call_9", Tool: "echo", Kind: KindRead, ReadOnly: true,
		Title: "hello", Paths: []string{filepath.Join(env.Workspace, "a/b.txt")}, Input: in,
	}
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("Request =\n%+v\nwant\n%+v", req, want)
	}
	if n := d.Pending(); n != 1 {
		t.Fatalf("Pending = %d after Prepare, want 1", n)
	}
	if echo.runs.Load() != 0 {
		t.Fatal("Prepare ran the tool")
	}

	got := d.Run(context.Background(), "t1.1.1", nil)
	if wantRes := (Result{Text: "hello", Trunc: Truncation{5, 5, 1, 1, ""}}); !reflect.DeepEqual(got, wantRes) {
		t.Fatalf("Run = %+v, want %+v", got, wantRes)
	}
	if n := d.Pending(); n != 0 {
		t.Fatalf("Pending = %d after Run, want 0: the entry leaked", n)
	}

	// The negative control: the entry is gone, so the call cannot run twice.
	again := d.Run(context.Background(), "t1.1.1", nil)
	if !again.IsError || again.Class != ClassToolError || !strings.Contains(again.Text, "no prepared tool call") {
		t.Fatalf("second Run = %+v, want a tool_error", again)
	}
	if n := echo.runs.Load(); n != 1 {
		t.Fatalf("the tool ran %d times, want 1", n)
	}
}

func TestDispatcherPrepareFailureIsInvalidInput(t *testing.T) {
	echo := newFake("echo", nil)
	d := newDispatcher(t, testEnv(t), nil, echo)
	cases := map[string]string{
		"a number where a string goes": `{"text": 5}`,
		"invalid JSON":                 `{"text": "unterminated`,
		"a value Prepare refuses":      `{"text": ""}`,
	}
	n := 0
	for name, raw := range cases {
		n++
		id := fmt.Sprintf("t1.1.%d", n)
		t.Run(name, func(t *testing.T) {
			req, res, ok := d.Prepare(Call{ID: id, Tool: "echo", Input: json.RawMessage(raw)})
			if ok || !res.IsError || res.Class != ClassInvalidInput {
				t.Fatalf("Prepare = ok %v, %+v; want an invalid_input result", ok, res)
			}
			if !strings.HasPrefix(res.Text, "The echo tool was called with invalid arguments: ") ||
				!strings.HasSuffix(res.Text, ".\nPlease rewrite the input so it satisfies the expected schema.") ||
				strings.Contains(res.Text, "..") {
				t.Fatalf("text = %q, want opencode's InvalidArgumentsError", res.Text)
			}
			// The card still gets what the model sent.
			if req.Tool != "echo" || req.Kind != KindRead || string(req.Input) != raw {
				t.Fatalf("Request = %+v", req)
			}
			// Fantasy may still call Run (it checks only JSON and required
			// names); the same result comes back and nothing runs.
			if got := d.Run(context.Background(), id, nil); !reflect.DeepEqual(got, res) {
				t.Fatalf("Run = %+v, want Prepare's result %+v", got, res)
			}
		})
	}
	if echo.runs.Load() != 0 {
		t.Fatal("a call that failed Prepare ran")
	}
	// The negative control: valid input prepares.
	if _, _, ok := d.Prepare(Call{ID: "t1.2.1", Tool: "echo", Input: json.RawMessage(`{"text":"ok"}`)}); !ok {
		t.Fatal("a valid call failed to prepare")
	}
}

func TestDispatcherUnknownTool(t *testing.T) {
	d := newDispatcher(t, testEnv(t), nil, newFake("echo", nil), newFake("other", nil))
	req, res, ok := d.Prepare(Call{ID: "t1.1.1", CallID: "c", Tool: "nope", Input: json.RawMessage(`{}`)})
	if ok || res.Class != ClassInvalidInput || res.Text != "tool not found: nope. Available tools: echo, other" {
		t.Fatalf("Prepare = ok %v, %+v; want Fantasy's tool-not-found text", ok, res)
	}
	if req.Tool != "nope" || req.Kind != "" {
		t.Fatalf("Request = %+v", req)
	}
	if got := d.Run(context.Background(), "t1.1.1", nil); !reflect.DeepEqual(got, res) {
		t.Fatalf("Run = %+v, want %+v", got, res)
	}
	// The negative control: a registered name prepares.
	if _, _, ok := d.Prepare(Call{ID: "t1.1.2", Tool: "other", Input: json.RawMessage(`{"text":"x"}`)}); !ok {
		t.Fatal("a known tool failed to prepare")
	}
}

func TestDispatcherGate(t *testing.T) {
	cases := []struct {
		name     string
		gate     GateFunc
		class    ErrorClass
		text     string // exact, or a substring when partial
		partial  bool
		runsTool bool
	}{
		{"deny with a reason", func(context.Context, Request) (Decision, error) { return Deny{Reason: "blocked by the test"}, nil },
			ClassDenied, "blocked by the test", false, false},
		{"deny without a reason", func(context.Context, Request) (Decision, error) { return Deny{}, nil },
			ClassDenied, deniedText, false, false},
		{"ask, with no one to ask", func(context.Context, Request) (Decision, error) { return Ask{Prompt: "may I?"}, nil },
			ClassDenied, NoApprovalChannel, false, false},
		{"a gate error", func(context.Context, Request) (Decision, error) { return nil, errors.New("evaluator unreachable") },
			ClassDenied, "permission check failed: evaluator unreachable", true, false},
		{"a gate panic", func(context.Context, Request) (Decision, error) { panic("gate exploded") },
			ClassDenied, "permission check panicked: gate exploded", true, false},
		{"no decision", func(context.Context, Request) (Decision, error) { return nil, nil },
			ClassDenied, "gave no decision", true, false},
		// The negative control.
		{"allow", func(context.Context, Request) (Decision, error) { return Allow{}, nil }, "", "run", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			echo := newFake("echo", nil)
			var seen Request
			gate := GateFunc(func(ctx context.Context, r Request) (Decision, error) {
				seen = r
				return tc.gate(ctx, r)
			})
			d := newDispatcher(t, testEnv(t), gate, echo)
			req, _, _ := d.Prepare(Call{ID: "t1.1.1", Tool: "echo", Input: input(t, fakeInput{Text: "run"})})
			res := d.Run(context.Background(), "t1.1.1", nil)
			if !reflect.DeepEqual(seen, req) {
				t.Fatalf("the gate saw %+v, want the prepared Request %+v", seen, req)
			}
			if (echo.runs.Load() == 1) != tc.runsTool {
				t.Fatalf("tool ran %d times, want ran=%v", echo.runs.Load(), tc.runsTool)
			}
			if res.Class != tc.class || res.IsError == tc.runsTool {
				t.Fatalf("Run = %+v, want class %q", res, tc.class)
			}
			if tc.partial && !strings.Contains(res.Text, tc.text) || !tc.partial && res.Text != tc.text {
				t.Fatalf("text = %q, want %q", res.Text, tc.text)
			}
			if d.Pending() != 0 {
				t.Fatal("a gated call's entry leaked")
			}
		})
	}
}

func TestAllowAllAllows(t *testing.T) {
	dec, err := AllowAll.Check(context.Background(), Request{})
	if err != nil || dec != (Allow{}) {
		t.Fatalf("AllowAll = %v, %v", dec, err)
	}
}

// TestDispatcherCancelled: a call whose turn is already cancelled neither
// asks the gate nor runs; a gate that fails because the turn was cancelled
// under it is an abort, not a denial.
func TestDispatcherCancelled(t *testing.T) {
	echo := newFake("echo", nil)
	var asked atomic.Int32
	gate := GateFunc(func(ctx context.Context, _ Request) (Decision, error) {
		asked.Add(1)
		return Allow{}, nil
	})
	d := newDispatcher(t, testEnv(t), gate, echo)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.Prepare(Call{ID: "t1.1.1", Tool: "echo", Input: input(t, fakeInput{Text: "x"})})
	res := d.Run(ctx, "t1.1.1", nil)
	if !res.IsError || res.Class != ClassAborted || res.Text != AbortedText || asked.Load() != 0 || echo.runs.Load() != 0 {
		t.Fatalf("Run = %+v (gate asked %d, tool ran %d), want aborted before anything", res, asked.Load(), echo.runs.Load())
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	slow := GateFunc(func(ctx context.Context, _ Request) (Decision, error) {
		cancel2()
		<-ctx.Done()
		return nil, ctx.Err()
	})
	d = newDispatcher(t, testEnv(t), slow, echo)
	d.Prepare(Call{ID: "t1.1.1", Tool: "echo", Input: input(t, fakeInput{Text: "x"})})
	if res := d.Run(ctx2, "t1.1.1", nil); res.Class != ClassAborted || res.Text != AbortedText {
		t.Fatalf("Run = %+v, want aborted", res)
	}
}

func TestDispatcherRecoversPanics(t *testing.T) {
	cases := map[string]*fake{
		"in Run":     newFake("echo", func(context.Context, Env, fakeInput) Result { panic("run exploded") }),
		"in Prepare": {spec: fakeSpec("echo", Head), prepPanic: true},
		"in Request": {spec: fakeSpec("echo", Head), reqPanic: true},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			d := newDispatcher(t, testEnv(t), nil, f)
			_, pres, ok := d.Prepare(Call{ID: "t1.1.1", Tool: "echo", Input: input(t, fakeInput{Text: "x"})})
			res := d.Run(context.Background(), "t1.1.1", nil)
			if !res.IsError || res.Class != ClassToolError || !strings.Contains(res.Text, "exploded") {
				t.Fatalf("Run = %+v, want a tool_error naming the panic", res)
			}
			if name != "in Run" && (ok || !reflect.DeepEqual(pres, res)) {
				t.Fatalf("Prepare = ok %v, %+v; want the same tool_error", ok, pres)
			}
		})
	}
	// The negative control: the same tool without a panic succeeds.
	d := newDispatcher(t, testEnv(t), nil, newFake("echo", nil))
	d.Prepare(Call{ID: "t1.1.1", Tool: "echo", Input: input(t, fakeInput{Text: "x"})})
	if res := d.Run(context.Background(), "t1.1.1", nil); res.IsError {
		t.Fatalf("Run = %+v", res)
	}
}

// TestDispatcherOwnsClassAndStopTurn: an unclassified failure is tool_error,
// a tool's own class stands, and StopTurn is the harness's to set.
func TestDispatcherOwnsClassAndStopTurn(t *testing.T) {
	results := map[string]Result{
		"unclassified": {Text: "broke", IsError: true, StopTurn: true},
		"classified":   {Text: "no such file", IsError: true, Class: ClassNotFound},
		"success":      {Text: "fine", StopTurn: true},
	}
	want := map[string]ErrorClass{"unclassified": ClassToolError, "classified": ClassNotFound, "success": ""}
	for text, r := range results {
		d := newDispatcher(t, testEnv(t), nil, newFake("echo", func(context.Context, Env, fakeInput) Result { return r }))
		d.Prepare(Call{ID: "t1.1.1", Tool: "echo", Input: input(t, fakeInput{Text: text})})
		got := d.Run(context.Background(), "t1.1.1", nil)
		if got.Class != want[text] || got.StopTurn {
			t.Fatalf("%s: Run = %+v, want class %q and StopTurn false", text, got, want[text])
		}
	}
}

func TestDispatcherDiscardAndIDs(t *testing.T) {
	echo := newFake("echo", nil)
	d := newDispatcher(t, testEnv(t), nil, echo)
	ok := func(id string) bool {
		_, _, ok := d.Prepare(Call{ID: id, Tool: "echo", Input: input(t, fakeInput{Text: "x"})})
		return ok
	}
	if !ok("t1.1.1") || !ok("t1.1.2") {
		t.Fatal("Prepare failed")
	}
	d.Discard("t1.1.1")
	d.Discard("never-prepared") // a no-op
	if n := d.Pending(); n != 1 {
		t.Fatalf("Pending = %d after one Discard, want 1", n)
	}
	if res := d.Run(context.Background(), "t1.1.1", nil); res.Class != ClassToolError {
		t.Fatalf("Run of a discarded call = %+v, want a tool_error", res)
	}

	// A second Prepare of a pending id fails both calls, so neither runs,
	// let alone twice.
	if ok("t1.1.2") {
		t.Fatal("a duplicate id prepared")
	}
	if res := d.Run(context.Background(), "t1.1.2", nil); res.Class != ClassToolError || !strings.Contains(res.Text, "used twice") {
		t.Fatalf("Run of a duplicated id = %+v", res)
	}

	// Ids that are unsafe as file names never prepare.
	for _, id := range []string{"", ".", "..", "../t1", "t1/2", "-t1", strings.Repeat("t", 65)} {
		if ok(id) {
			t.Fatalf("id %q prepared", id)
		}
		d.Discard(id)
	}
	if echo.runs.Load() != 0 || d.Pending() != 0 {
		t.Fatalf("runs %d, pending %d; want 0 and 0", echo.runs.Load(), d.Pending())
	}
}

// TestDispatcherRedactsEveryOutwardField: a key in any field a tool returns,
// or in the model's raw input, is redacted in what the dispatcher returns
// and in what the gate and the progress consumer see; the tool itself acts
// on the real input. The same run with no keys is the negative control:
// every field then carries the key, so the assertions see what they check.
func TestDispatcherRedactsEveryOutwardField(t *testing.T) {
	for _, redacting := range []bool{true, false} {
		t.Run(fmt.Sprintf("redacting=%v", redacting), func(t *testing.T) {
			var keys []string
			if redacting {
				keys = []string{keyA}
			}
			env := testEnv(t, keys...)
			var snapshot string
			delivered := make(chan struct{})
			consumer := func(s string) { snapshot = s; close(delivered) }
			f := newFake("echo", func(_ context.Context, env Env, in fakeInput) Result {
				env.Progress("progress " + in.Text)
				// Hold the call open until the snapshot is through: Run's end
				// closes the gate, and a snapshot not yet delivered is dropped.
				select {
				case <-delivered:
				case <-time.After(10 * time.Second):
				}
				return Result{
					Text:    "text " + in.Text,
					IsError: false,
					Output:  &ExecOutput{ExitCode: 1, Output: "out " + in.Text},
					Content: "content " + in.Text,
					Edits:   []FileEdit{{Path: "/p/" + in.Text, Old: "old " + in.Text, New: "new " + in.Text}},
				}
			})
			f.req = func(in fakeInput, env Env) Request {
				return Request{Title: in.Text, Paths: []string{env.Resolve(in.Text)}, Command: "echo " + in.Text, Workdir: "/w/" + in.Text}
			}
			var gateSaw Request
			gate := GateFunc(func(_ context.Context, r Request) (Decision, error) { gateSaw = r; return Allow{}, nil })
			d := newDispatcher(t, env, gate, f)

			// The model streamed its arguments in pieces and the key was cut
			// between two of them; the call carries them joined.
			half := len(keyA) / 2
			raw := json.RawMessage(`{"text":"` + keyA[:half] + keyA[half:] + `"}`)
			req, _, ok := d.Prepare(Call{ID: "t1.1.1", CallID: "call-" + keyA, Tool: "echo", Input: raw})
			if !ok {
				t.Fatal("Prepare failed")
			}
			res := d.Run(context.Background(), "t1.1.1", consumer)
			if snapshot == "" {
				t.Fatal("no progress snapshot was delivered")
			}

			if got, _ := f.lastText.Load().(string); got != keyA {
				t.Fatalf("the tool saw %q, want the real input", got)
			}
			outward := fmt.Sprintf("%+v %+v %+v %+v %+v %s", req, gateSaw, res, *res.Output, res.Edits, snapshot)
			outward += string(req.Input)
			if redacting {
				if strings.Contains(outward, keyA) || strings.Contains(outward, keyA[:half+2]) {
					t.Fatalf("a key survived: %s", outward)
				}
				if n := strings.Count(outward, redact.Marker); n < 14 {
					t.Fatalf("only %d markers in %s", n, outward)
				}
				if !json.Valid(req.Input) {
					t.Fatalf("redacted input %s is not valid JSON", req.Input)
				}
			} else if n := strings.Count(outward, keyA); n < 14 {
				t.Fatalf("the control run shows the key %d times; the fields checked are not the fields returned: %s", n, outward)
			}
		})
	}
}

// TestDispatcherTruncates runs a long result through each Direction. Head
// and Tail cut the text and spill the whole of it, redacted, to a private
// file named from the call's id; None leaves text and Trunc as the tool set
// them; an error result is never cut.
func TestDispatcherTruncates(t *testing.T) {
	// Short lines, so the line limit is the one hit; a key every 100 lines.
	var b strings.Builder
	for i := range 3000 {
		if i%100 == 0 {
			fmt.Fprintf(&b, "line %d %s\n", i, keyA)
		} else {
			fmt.Fprintf(&b, "line %d\n", i)
		}
	}
	long := b.String()
	redacted := strings.ReplaceAll(long, keyA, redact.Marker)

	for _, dir := range []Direction{Head, Tail, None} {
		t.Run(fmt.Sprint(dir), func(t *testing.T) {
			env := testEnv(t, keyA)
			f := newFake("long", func(context.Context, Env, fakeInput) Result {
				return Result{Text: long, Trunc: Truncation{KeptBytes: 1, TotalBytes: 2}}
			})
			f.spec.Truncate = dir
			d := newDispatcher(t, env, nil, f)
			d.Prepare(Call{ID: "t3.2.1", Tool: "long", Input: input(t, fakeInput{Text: "x"})})
			res := d.Run(context.Background(), "t3.2.1", nil)
			spill := filepath.Join(env.Home, "tool-output", "tool_t3.2.1")

			if dir == None {
				if res.Text != redacted || res.Trunc != (Truncation{KeptBytes: 1, TotalBytes: 2}) {
					t.Fatalf("None changed the result: %d bytes, %+v", len(res.Text), res.Trunc)
				}
				if _, err := os.Lstat(spill); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("None spilled: %v", err)
				}
				return
			}
			if res.Trunc.Spill != spill || !res.Trunc.Truncated() || res.Trunc.TotalBytes != len(redacted) || res.Trunc.KeptLines != MaxLines {
				t.Fatalf("Trunc = %+v, want %d of %d lines kept and the spill at %s", res.Trunc, MaxLines, 3001, spill)
			}
			if !strings.Contains(res.Text, "Full output saved to: "+spill) || strings.Contains(res.Text, keyA) {
				t.Fatalf("text does not point at the spill, or leaks the key")
			}
			first, last := "line 0 ", "line 2999\n"
			if dir == Tail {
				first, last = last, first
			}
			if !strings.Contains(res.Text, first) || strings.Contains(res.Text, last) {
				t.Fatalf("direction %v kept the wrong end", dir)
			}
			data, err := os.ReadFile(spill)
			if err != nil || string(data) != redacted {
				t.Fatalf("spill holds %d bytes (%v), want the %d redacted bytes", len(data), err, len(redacted))
			}
			if m := mode(t, spill); m != 0o600 {
				t.Fatalf("spill mode %04o, want 0600", m)
			}
			if m := mode(t, filepath.Dir(spill)); m != 0o700 {
				t.Fatalf("spill directory mode %04o, want 0700", m)
			}
		})
	}

	// An error result reaches the model whole: the hint says the call
	// succeeded, which would be false.
	env := testEnv(t)
	f := newFake("long", func(context.Context, Env, fakeInput) Result { return Result{Text: long, IsError: true} })
	d := newDispatcher(t, env, nil, f)
	d.Prepare(Call{ID: "t3.2.1", Tool: "long", Input: input(t, fakeInput{Text: "x"})})
	if res := d.Run(context.Background(), "t3.2.1", nil); res.Text != long || res.Trunc != (Truncation{}) {
		t.Fatalf("an error result was cut: %d bytes, %+v", len(res.Text), res.Trunc)
	}
}

// TestDispatcherRedactsTheSpillPath: truncation adds its notice and the
// spill path after the tool's text is redacted, and a tool that truncates
// itself reports its own spill path; with a key in the harness's home, none
// of them may carry it out. The file itself is still written at its real
// path. With no keys, the negative control, the path shows as it is.
func TestDispatcherRedactsTheSpillPath(t *testing.T) {
	for _, redacting := range []bool{true, false} {
		t.Run(fmt.Sprintf("redacting=%v", redacting), func(t *testing.T) {
			env := testEnv(t)
			env.Home = filepath.Join(t.TempDir(), keyA, "native")
			if redacting {
				env.Redactor = redact.New(keyA)
			}
			long := numbered(3000)
			head := newFake("head", func(context.Context, Env, fakeInput) Result { return Result{Text: long} })
			self := newFake("self", func(_ context.Context, env Env, _ fakeInput) Result {
				return Result{Text: "kept", Trunc: Truncation{KeptBytes: 4, TotalBytes: 9, Spill: filepath.Join(env.Home, SpillDir, "tool_own")}}
			})
			self.spec.Truncate = None
			d := newDispatcher(t, env, nil, head, self)
			d.Prepare(Call{ID: "t1.1.1", Tool: "head", Input: input(t, fakeInput{Text: "x"})})
			d.Prepare(Call{ID: "t1.1.2", Tool: "self", Input: input(t, fakeInput{Text: "x"})})
			cut := d.Run(context.Background(), "t1.1.1", nil)
			own := d.Run(context.Background(), "t1.1.2", nil)

			onDisk := filepath.Join(env.Home, SpillDir, "tool_t1.1.1")
			if data, err := os.ReadFile(onDisk); err != nil || string(data) != long {
				t.Fatalf("the spill file at its real path: %d bytes, %v", len(data), err)
			}
			if !strings.Contains(cut.Text, "Full output saved to: "+cut.Trunc.Spill+"\n") {
				t.Fatalf("the notice does not name Trunc.Spill %q", cut.Trunc.Spill)
			}
			outward := cut.Text + "\n" + cut.Trunc.Spill + "\n" + own.Trunc.Spill
			if redacting {
				if strings.Contains(outward, keyA) {
					t.Fatalf("the home's key leaked: %s", outward[len(outward)-300:])
				}
				if !strings.Contains(cut.Trunc.Spill, redact.Marker) || !strings.Contains(own.Trunc.Spill, redact.Marker) {
					t.Fatalf("spill paths %q, %q were not redacted", cut.Trunc.Spill, own.Trunc.Spill)
				}
			} else if n := strings.Count(outward, keyA); n != 3 || cut.Trunc.Spill != onDisk {
				// The notice, and each of the two spill paths.
				t.Fatalf("the control shows the key %d times (want 3) and Spill %q (want %q)", n, cut.Trunc.Spill, onDisk)
			}
		})
	}
}

// TestDispatcherRedactsTheNotice: the truncation notice is fixed text added
// after the result's redaction, so a key that happens to be one of its words
// ("succeeded") must still be redacted in it. The negative control, with no
// keys, shows the word.
func TestDispatcherRedactsTheNotice(t *testing.T) {
	const word = "succeeded"
	for _, redacting := range []bool{true, false} {
		t.Run(fmt.Sprintf("redacting=%v", redacting), func(t *testing.T) {
			env := testEnv(t)
			if redacting {
				env.Redactor = redact.New(word)
			}
			long := numbered(3000)
			head := newFake("head", func(context.Context, Env, fakeInput) Result { return Result{Text: long} })
			d := newDispatcher(t, env, nil, head)
			d.Prepare(Call{ID: "t1.1.1", Tool: "head", Input: input(t, fakeInput{Text: "x"})})
			cut := d.Run(context.Background(), "t1.1.1", nil)
			if !cut.Trunc.Truncated() {
				t.Fatal("the result was not truncated")
			}
			if got := strings.Contains(cut.Text, word); got == redacting {
				t.Fatalf("redacting=%v but the notice's %q present=%v:\n%s", redacting, word, got, cut.Text[len(cut.Text)-300:])
			}
		})
	}
}

// TestConcurrentRunsNeverWaitOnProgress: five calls run at once, as
// Fantasy's semaphore allows, each flooding Progress while the consumer is
// stuck. Every Run must return anyway; under -race this also proves the
// dispatcher's shared state is guarded. A tool that keeps its Progress past
// Run reaches no one.
func TestConcurrentRunsNeverWaitOnProgress(t *testing.T) {
	release := make(chan struct{})
	var delivered, inside atomic.Int32
	var entered sync.Map // call id -> true once a snapshot of it is in the consumer
	consumer := func(s string) {
		delivered.Add(1)
		inside.Add(1)
		entered.Store(strings.Fields(s)[0], true)
		<-release
		inside.Add(-1)
	}
	var kept sync.Map // call id -> the Progress its Run was given
	f := newFake("flood", func(ctx context.Context, env Env, in fakeInput) Result {
		kept.Store(in.Text, env.Progress)
		// Flood until one snapshot is stuck in the consumer (so the count
		// below is exact rather than a race with Run's end), then 2000 more.
		deadline := time.Now().Add(10 * time.Second)
		for i := 0; ; i++ {
			env.Progress(fmt.Sprintf("%s %d", in.Text, i))
			if _, ok := entered.Load(in.Text); ok && i >= 2000 {
				break
			}
			if time.Now().After(deadline) {
				return Result{Text: "no snapshot was ever delivered", IsError: true}
			}
			if _, ok := entered.Load(in.Text); !ok {
				time.Sleep(time.Microsecond)
			}
		}
		return Result{Text: in.Text}
	})
	d := newDispatcher(t, testEnv(t), nil, f)
	d.interval = 0 // every snapshot the consumer is ready for is delivered

	ids := []string{"t1.1.1", "t1.1.2", "t1.1.3", "t1.1.4", "t1.1.5"}
	for _, id := range ids {
		if _, _, ok := d.Prepare(Call{ID: id, Tool: "flood", Input: input(t, fakeInput{Text: id})}); !ok {
			t.Fatal("Prepare failed")
		}
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			if res := d.Run(context.Background(), id, consumer); res.Text != id {
				t.Errorf("Run(%s) = %+v", id, res)
			}
		})
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("Run blocked on a stuck progress consumer")
	}
	// Each call's first snapshot went out and is stuck; the rest dropped.
	if n := delivered.Load(); n != int32(len(ids)) {
		t.Fatalf("%d snapshots reached the consumer, want exactly one per call (%d)", n, len(ids))
	}
	close(release)
	waitFor(t, func() bool { return inside.Load() == 0 })
	// After Run, a kept Progress is dead.
	kept.Range(func(_, p any) bool {
		p.(Progress)("late")
		return true
	})
	time.Sleep(10 * time.Millisecond)
	if n := delivered.Load(); n != int32(len(ids)) {
		t.Fatalf("a snapshot sent after Run reached the consumer (%d delivered)", n)
	}
	if d.Pending() != 0 {
		t.Fatal("entries leaked")
	}
}

// fakeClock is a settable clock for the throttle.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// idle waits until g has no delivery in flight.
func idle(t *testing.T, g *progressGate) {
	t.Helper()
	waitFor(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return !g.busy
	})
}

func TestProgressThrottle(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	got := make(chan string, 10)
	block := make(chan struct{})
	g := &progressGate{
		deliver: func(s string) {
			got <- s
			if s == "slow" {
				<-block
			}
		},
		redactor: testEnv(t, keyA).Redactor,
		now:      clock.Now,
		interval: progressInterval,
	}
	recv := func() string {
		select {
		case s := <-got:
			return s
		case <-time.After(10 * time.Second):
			t.Fatal("no snapshot delivered")
			return ""
		}
	}

	g.send("first " + keyA)
	if s := recv(); s != "first "+redact.Marker {
		t.Fatalf("delivered %q, want the first snapshot, redacted", s)
	}
	idle(t, g)
	clock.Advance(99 * time.Millisecond)
	g.send("too soon") // dropped: within 100 ms of the last
	clock.Advance(time.Millisecond)
	g.send("slow") // the negative control: 100 ms on, delivered
	if s := recv(); s != "slow" {
		t.Fatalf("delivered %q, want %q", s, "slow")
	}
	clock.Advance(time.Second)
	g.send("while busy") // dropped: the consumer has not taken "slow" yet
	close(block)
	idle(t, g)
	clock.Advance(time.Second)
	g.send("after")
	if s := recv(); s != "after" {
		t.Fatalf("delivered %q, want %q", s, "after")
	}
	idle(t, g)
	g.close()
	clock.Advance(time.Second)
	g.send("closed")
	time.Sleep(10 * time.Millisecond)
	select {
	case s := <-got:
		t.Fatalf("delivered %q; want nothing more", s)
	default:
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
