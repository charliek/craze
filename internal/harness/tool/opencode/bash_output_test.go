package opencode

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// The bash_output and bash_stop tools (plan 033 §3.7). As for agent_output,
// the session's jobs answer them; these tests build a dispatcher over the two
// tools alone, with a recording fake behind Env.Jobs.

// recordingJobs records every bash_output and bash_stop call it is handed and
// answers each with res.
type recordingJobs struct {
	res tool.Result

	mu      sync.Mutex
	outputs []tool.JobOutputCall
	stops   []tool.JobStopCall
}

func (r *recordingJobs) Reserve(string) (tool.JobSlot, error) { return nil, tool.JobsFull{Max: 0} }
func (r *recordingJobs) Redact(text string) string            { return text }

func (r *recordingJobs) Output(_ context.Context, c tool.JobOutputCall) tool.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outputs = append(r.outputs, c)
	return r.res
}

func (r *recordingJobs) Stop(_ context.Context, c tool.JobStopCall) tool.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stops = append(r.stops, c)
	return r.res
}

func (r *recordingJobs) seen() ([]tool.JobOutputCall, []tool.JobStopCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.outputs), slices.Clone(r.stops)
}

// newJobToolFixture is a session's worth of the two job tools alone.
func newJobToolFixture(t *testing.T, jobs tool.Jobs) *fixture {
	t.Helper()
	env := tool.Env{
		Workspace: t.TempDir(),
		Home:      t.TempDir(),
		Redactor:  redact.New(keyA),
		Environ:   tool.ChildEnviron(os.Environ(), nil),
		Locks:     &tool.PathLocks{},
	}
	if jobs != nil {
		env.Jobs = jobs
	}
	out, err := newBashOutput()
	if err != nil {
		t.Fatal(err)
	}
	stop, err := newBashStop()
	if err != nil {
		t.Fatal(err)
	}
	d, err := tool.NewDispatcher(tool.Options{Tools: []tool.Tool{out, stop}, Env: env})
	if err != nil {
		t.Fatalf("the job tools do not pass the dispatcher's checks: %v", err)
	}
	return &fixture{env: env, d: d}
}

// TestJobToolSpecs: the two tools' contracts — bash_output reads (kind read,
// read-only), bash_stop executes (not read-only: the mode gate allows it by
// name); both parallel and never cut by the dispatcher; id required, and
// wait_ms bash_output's alone. Their descriptions are rendered, craze's own,
// and say what §3.7 says; the profile offers them right after bash.
func TestJobToolSpecs(t *testing.T) {
	out, err := newBashOutput()
	if err != nil {
		t.Fatal(err)
	}
	stop, err := newBashStop()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tl       tool.Tool
		id       string
		kind     tool.Kind
		readOnly bool
		params   []string
		says     []string
	}{
		{out, tool.BashOutputTool, tool.KindRead, true, []string{"id", "wait_ms"},
			[]string{"run_in_background", "moved to the background when it reached its timeout", "delivered to you on its own",
				"do not poll it or sleep waiting for it", "since your last read", "default 0", "at most 600000", "A result is delivered once"}},
		{stop, tool.BashStopTool, tool.KindExecute, false, []string{"id"},
			[]string{"SIGTERM, then SIGKILL 3 seconds later", "up to 7 seconds", "delivered once", "no longer need"}},
	} {
		s := tc.tl.Spec()
		if s.ID != tc.id || s.Kind != tc.kind || s.ReadOnly != tc.readOnly || !s.Parallel || s.Truncate != tool.None {
			t.Errorf("%s: spec = id %q kind %q readOnly %v parallel %v truncate %d", tc.id, s.ID, s.Kind, s.ReadOnly, s.Parallel, s.Truncate)
		}
		if !slices.Equal(s.Required, []string{"id"}) {
			t.Errorf("%s: required = %q, want id", tc.id, s.Required)
		}
		if params := slices.Sorted(maps.Keys(s.Parameters)); !slices.Equal(params, tc.params) {
			t.Errorf("%s: parameters = %q, want %q", tc.id, params, tc.params)
		}
		d := s.Description
		for _, says := range tc.says {
			if !strings.Contains(d, says) {
				t.Errorf("%s: the description does not say %q", tc.id, says)
			}
		}
		if strings.Contains(d, "${") || !strings.HasSuffix(d, "\n") || strings.HasSuffix(d, "\n\n") {
			t.Errorf("%s: want a rendered description with one final newline:\n%q", tc.id, d)
		}
		for _, absent := range []string{"Task tool", "TodoWrite", "opencode", "BashOutput", "KillShell"} {
			if strings.Contains(d, absent) {
				t.Errorf("%s: the description mentions %q", tc.id, absent)
			}
		}
	}
	p, err := Profile()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(p); !slices.Equal(got[:3], []string{"bash", "bash_output", "bash_stop"}) {
		t.Fatalf("the profile offers %q; want bash_output and bash_stop right after bash", got)
	}
}

// TestJobToolsPrepare: the session's jobs are handed each call's id, trimmed,
// under the call's own harness id, and bash_output's wait — 0 when wait_ms
// is absent or null (P13: a snapshot), 0 for a negative, 600 s at most; a
// missing, blank or non-string id and a wait_ms that is not an integer are
// invalid_input naming the field, and never reach the jobs. Each row's title
// names what the call is of.
func TestJobToolsPrepare(t *testing.T) {
	jobs := &recordingJobs{res: tool.Result{Text: "the answer"}}
	f := newJobToolFixture(t, jobs)
	for _, tc := range []struct {
		input string
		wait  time.Duration
	}{
		{`{"id":" t4.2.1 "}`, 0},
		{`{"id":"t4.2.1","wait_ms":null}`, 0},
		{`{"id":"t4.2.1","wait_ms":-5}`, 0},
		{`{"id":"t4.2.1","wait_ms":1500}`, 1500 * time.Millisecond},
		{`{"id":"t4.2.1","wait_ms":600000}`, 600 * time.Second},
		{`{"id":"t4.2.1","wait_ms":9000000}`, 600 * time.Second},
	} {
		outs, _ := jobs.seen()
		req, res := f.call(t, "bash_output", tc.input)
		if ok(t, res) != "the answer" || req.Title != "background job output t4.2.1" {
			t.Fatalf("%s: result %q, title %q", tc.input, res.Text, req.Title)
		}
		got, _ := jobs.seen()
		if c := got[len(outs)]; c.ID != "t4.2.1" || c.Wait != tc.wait || !strings.HasPrefix(c.CallID, "t1.1.") {
			t.Fatalf("%s: the jobs were handed %+v; want t4.2.1, wait %v, under the call's harness id", tc.input, c, tc.wait)
		}
	}
	req, res := f.call(t, "bash_stop", `{"id":" t4.2.1 "}`)
	_, stops := jobs.seen()
	if ok(t, res) != "the answer" || req.Title != "stop background job t4.2.1" || len(stops) != 1 || stops[0].ID != "t4.2.1" ||
		!strings.HasPrefix(stops[0].CallID, "t1.1.") {
		t.Fatalf("bash_stop: result %q, title %q, handed %+v", res.Text, req.Title, stops)
	}
	outs, stops := jobs.seen()
	for _, tc := range []struct{ tool, input, field string }{
		{"bash_output", `{}`, "id is required"},
		{"bash_output", `{"id":"  "}`, "id must not be empty"},
		{"bash_output", `{"id":5}`, "id must be a string"},
		{"bash_output", `{"id":"k","wait_ms":"10"}`, "wait_ms must be an integer"},
		{"bash_stop", `{}`, "id is required"},
		{"bash_stop", `{"id":" "}`, "id must not be empty"},
	} {
		_, res := f.call(t, tc.tool, tc.input)
		if !res.IsError || res.Class != tool.ClassInvalidInput || !strings.Contains(res.Text, tc.field) {
			t.Errorf("%s %s: result %+v, want invalid_input naming %q", tc.tool, tc.input, res, tc.field)
		}
	}
	if o, s := jobs.seen(); len(o) != len(outs) || len(s) != len(stops) {
		t.Fatal("the jobs were handed refused calls")
	}
}

// TestJobToolsWithoutJobs: a session with no jobs answers both with a
// tool_error saying so; the session with jobs above is the control.
func TestJobToolsWithoutJobs(t *testing.T) {
	f := newJobToolFixture(t, nil)
	for _, name := range []string{"bash_output", "bash_stop"} {
		_, res := f.call(t, name, `{"id":"t1.1.1"}`)
		failed(t, res, tool.ClassToolError, "This session runs no background jobs.")
	}
}
