package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/protocol"
)

// editEv is a call's update with its host's stamp.
func editEv(id, status string, edited *agent.ToolEdited) agent.Event {
	return agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: id, Kind: "edit", Status: status, Edited: edited}}
}

func stamp(files ...string) *agent.ToolEdited { return &agent.ToolEdited{Files: files} }

func foldedEdits(evs ...agent.Event) editedFiles {
	a := newTurnAccumulator()
	for _, ev := range evs {
		a.fold(ev)
	}
	return a.edited()
}

// TestFilesAreWhatCompletedCallsLastSaid: a call counts by its stamp, the last it
// sent; it is listed once however often it is folded; a path counts for the call
// that succeeded on it, wherever the failure before it came; and a call that never
// finished, a sub-agent's call and a row with a path but no stamp list nothing.
func TestFilesAreWhatCompletedCallsLastSaid(t *testing.T) {
	for _, tc := range []struct {
		name string
		evs  []agent.Event
		want []string
	}{
		{"none", nil, []string{}},
		{"one", []agent.Event{editEv("a", "completed", stamp("a.go"))}, []string{"a.go"}},
		{"in the order the calls opened", []agent.Event{
			editEv("b", "in_progress", nil), editEv("a", "completed", stamp("a.go")), editEv("b", "completed", stamp("b.go")),
		}, []string{"b.go", "a.go"}},
		{"folded again", []agent.Event{
			editEv("a", "completed", stamp("a.go")), editEv("a", "completed", stamp("a.go")),
		}, []string{"a.go"}},
		{"a path two calls changed", []agent.Event{
			editEv("a", "completed", stamp("a.go", "b.go")), editEv("b", "completed", stamp("b.go", "c.go")),
		}, []string{"a.go", "b.go", "c.go"}},
		{"failed, then a later call succeeds on it", []agent.Event{
			editEv("a", "failed", nil), editEv("b", "completed", stamp("a.go")),
		}, []string{"a.go"}},
		{"started, never finished", []agent.Event{editEv("a", "in_progress", nil)}, []string{}},
		{"a status taken back", []agent.Event{
			editEv("a", "completed", stamp("a.go")), editEv("a", "failed", nil),
		}, []string{}},
		{"a row's paths without a stamp", []agent.Event{toolEv("a", "edit", "completed", "a.go")}, []string{}},
		{"a sub-agent's", []agent.Event{{Type: agent.EventTool, Agent: "child", Tool: editEv("a", "completed", stamp("a.go")).Tool}}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := foldedEdits(tc.evs...).files
			if got == nil || !slices.Equal(got, tc.want) {
				t.Fatalf("%#v, want %q", got, tc.want)
			}
		})
	}
}

// TestTheResultsFileKeys: planFile, outsideFiles and filesTruncated are each absent
// when there is none, and files stays `[]`; on a status that is not terminal none
// of them is written.
func TestTheResultsFileKeys(t *testing.T) {
	render := func(status string, evs ...agent.Event) map[string]any {
		a := newTurnAccumulator()
		for _, ev := range evs {
			a.fold(ev)
		}
		r := newResult(resultSession{id: "0199c0de-1111-7000-8000-397e175bfe79", provider: "grok"}, status)
		r.fill(a)
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := render(statusCompleted)
	if got, ok := m["files"].([]any); !ok || len(got) != 0 {
		t.Fatalf("files: %v", m["files"])
	}
	for _, k := range []string{"planFile", "outsideFiles", "filesTruncated"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s written with none: %v", k, m[k])
		}
	}
	full := []agent.Event{
		editEv("a", "completed", &agent.ToolEdited{Files: []string{"a.go"}, OutsideFiles: []string{"/x/o.go"}}),
		editEv("p", "completed", &agent.ToolEdited{PlanFile: "/h/plan.md"}),
		editEv("t", "completed", &agent.ToolEdited{Files: []string{"b.go"}, Truncated: true}),
	}
	m = render(statusCancelled, full...)
	if b, _ := json.Marshal([]any{m["files"], m["outsideFiles"], m["planFile"], m["filesTruncated"]}); string(b) != `[["a.go","b.go"],["/x/o.go"],"/h/plan.md",true]` {
		t.Fatalf("%s", b)
	}
	m = render(statusRunning, full...)
	for _, k := range []string{"files", "planFile", "outsideFiles", "filesTruncated"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s on a running result: %v", k, m[k])
		}
	}
}

func TestTheFooterSaysWhereFilesWent(t *testing.T) {
	a := newTurnAccumulator()
	a.fold(editEv("a", "completed", &agent.ToolEdited{Files: []string{"a.go"}, OutsideFiles: []string{"/x/o.go"}, PlanFile: "/h/plan.md", Truncated: true}))
	r := newResult(resultSession{id: "0199c0de-1111-7000-8000-397e175bfe79", provider: "grok"}, statusCompleted)
	r.fill(a)
	foot := r.footer()
	for _, want := range []string{
		"\nfiles: a.go\noutside the workspace: /x/o.go\nfiles: a call named more paths than are listed\nplan file: /h/plan.md\n",
	} {
		if !strings.Contains(foot, want) {
			t.Fatalf("%q, want %q", foot, want)
		}
	}
	r = newResult(resultSession{id: "0199c0de-1111-7000-8000-397e175bfe79", provider: "grok"}, statusCompleted)
	r.fill(newTurnAccumulator())
	for _, none := range []string{"files:", "outside", "plan file"} {
		if strings.Contains(r.footer(), none) {
			t.Fatalf("%q with none: %s", none, r.footer())
		}
	}
}

// TestAReconnectRefoldsItsCalls: a waiter that comes in by a snapshot holds the rows
// the snapshot shows, stamps included, and the stream goes on from there: a call
// folded by both is listed once, and one the stream then says failed is gone.
func TestAReconnectRefoldsItsCalls(t *testing.T) {
	r := newWaitRig(t, 0)
	turn, emitted := heldAfter(r,
		editEv("kept", "completed", stamp("kept.go")),
		editEv("taken-back", "completed", stamp("taken-back.go")),
		editEv("running", "in_progress", nil),
		editEv("failing", "in_progress", nil),
	)
	release := make(chan struct{})
	inner := turn.before
	turn.before = func() {
		go func() {
			<-emitted
			<-release
			r.s.emit(editEv("kept", "completed", stamp("kept.go")))
			r.s.emit(editEv("taken-back", "failed", nil))
			r.s.emit(editEv("failing", "failed", nil))
			r.s.emit(editEv("running", "completed", stamp("kept.go", "late.go")))
		}()
		inner()
	}
	r.script(turn)
	r.submit("mine")
	<-emitted
	conn, tp := r.dialTapped()
	w := goWait(conn, waitStart{}, waitOptions{})
	waitFor(t, "the waiter's look", func() bool { return len(tp.lines(protocol.MethodSessionSnapshot)) >= 1 })
	close(release)
	waitFor(t, "the last call on the stream", func() bool { return tp.read(`late.go`) })
	_ = r.eng.CloseFor(agent.StopCauseStop)
	res, f := w.result(t)
	res = wantStopped(t, res, f, agent.StopCauseStop)
	if res.Files == nil || !slices.Equal(*res.Files, []string{"kept.go", "late.go"}) {
		t.Fatalf("files %v", res.Files)
	}
}
