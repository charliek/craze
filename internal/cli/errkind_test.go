package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
)

// internalFailureSites is every failure in this package that leaves its kind
// unsaid, and so reports `internal`: `file: words`. Under --json, the ones on
// the waiting commands' paths print that kind, so this list is what shrinks when
// a failure is given a kind of its own. The rest are commands with no JSON failure
// to print.
var internalFailureSites = []string{
	`attach.go: exitf "%s"`,
	`attach.go: exitf "craze attach: %s"`,
	`attach.go: exitf "craze attach: %s"`,
	`attach.go: exitf "craze attach: no session %s"`,
	`attach.go: exitf "craze attach: session %s is unreachable: %s"`,
	`attach.go: exitf "craze: %s%s"`,
	`attach.go: exitf "craze: lost the session: %s"`,
	`auth.go: exitf "%s: %s"`,
	`auth.go: exitf "%s: %v; nothing was saved"`,
	`auth.go: exitf "%s: %v; nothing was saved"`,
	`auth.go: exitf "%s: no key given; nothing was saved"`,
	`auth.go: exitf "%s: no provider chosen; nothing was saved"`,
	`auth.go: exitf "%s: reading the choice: %v"`,
	`auth.go: exitf "%s: reading the key: %v; nothing was saved"`,
	`auth.go: exitf "%s: there is no craze directory to keep API keys in (set HOME or CRAZE_HOME)"`,
	`auth.go: exitf "%s: there is no provider to connect"`,
	`auth.go: exitf "%s: turning the terminal's echo off: %v; nothing was saved"`,
	`auth_chatgpt.go: exitf "%s: %s"`,
	`auth_chatgpt.go: exitf "%s: signing out of ChatGPT: %s"`,
	`auth_chatgpt.go: exitf "%s: signing out of ChatGPT: %s; the tokens may still be on this machine"`,
	`auth_chatgpt.go: exitf "%s: stdin ended before the redirect address was pasted; nothing was changed"`,
	`auth_chatgpt.go: exitf "%s: the sign-in was declined in the browser; nothing was changed"`,
	`auth_chatgpt.go: exitf "%s: turning the terminal's echo off: %v; nothing was changed"`,
	`bridge.go: exitError bridgePrefix + fmt.Sprintf(format, args...)`,
	`cancel_stop.go: exitf "%s: %s"`,
	`cancel_stop.go: exitf "%s: %s"`,
	`cancel_stop.go: exitf "%s: session %s runs in a craze that cannot stop it from here (its TUI, or an older craze); close it there"`,
	`hostlog.go: exitf "craze serve: --log %s: %v"`,
	`hub_cmd.go: exitf "craze hub: %v"`,
	`launch.go: exitf "%s"`,
	`launch.go: exitf "%s: %v"`,
	`launch.go: exitf "craze: %v"`,
	`new.go: exitf "craze new: the session started and refused its first prompt: %s"`,
	`prompt.go: exitError ""`,
	`prompt.go: exitError ""`,
	`prompt.go: exitError ""`,
	`prompt.go: exitError ""`,
	`prompt.go: exitError ""`,
	`prompt.go: exitError ""`,
	`prompt.go: exitError ""`,
	`prompt.go: exitError ""`,
	`prompt.go: exitError ""`,
	`prompt.go: exitError ""`,
	`providers_cmd.go: exitf "craze providers: %s"`,
	`providers_cmd.go: exitf "craze providers: %s"`,
	`providers_cmd.go: exitf "craze providers: %v"`,
	`providers_cmd.go: exitf "craze providers: %v"`,
	`providers_cmd.go: exitf "craze providers: %v"`,
	`ps.go: exitf "craze ps: %s"`,
	`ps.go: exitf "craze ps: %s"`,
	`ps.go: exitf "craze ps: %s: %s"`,
	`ps.go: exitf "craze ps: %v"`,
	`ps.go: exitf "craze ps: %v"`,
	`resolve.go: exitf "%s: %s"`,
	`send.go: exitf "%s: %s"`,
	`send.go: exitf "%s: the outcome: %v"`,
	`send.go: exitf "%s: the session index: %s"`,
	`serve.go: exitError "craze serve: " + e.Error()`,
	`serve_cmd.go: exitError "craze serve: " + rh.claims.pickerRefusal(held)`,
	`serve_cmd.go: exitf "%s"`,
	`serve_cmd.go: exitf "%s: %v"`,
	`serve_cmd.go: exitf "craze serve: %v"`,
	`serve_cmd.go: exitf "craze serve: that session ran in %s, which is no longer a directory"`,
	`serve_cmd.go: exitf "craze serve: the control socket: %v"`,
	`sessionflags.go: exitError "craze: " + refusal(err)`,
	`sessionflags.go: exitf "%s"`,
	`sessionflags.go: exitf "%s: %v"`,
	`sessionflags.go: exitf "craze: %v"`,
	`tui.go: exitf "%s"`,
	`tui.go: exitf "%s: %v"`,
	`turnresult.go: exitf "%s: %s"`,
	`turnresult.go: exitf "%s: the result: %v"`,
	`turnwait.go: exitf "%s: session %s runs in a craze whose stream this one cannot read (event codec %d, snapshot %d)"`,
	`turnwait.go: waitFailure "event " + strconv.FormatUint(it.Seq, 10) + " cannot be read: " + sanitizeLine(err.Error())`,
	`turnwait.go: waitFailure "the host sent no snapshot to start from"`,
	`turnwait.go: waitFailure "the prompt started no turn and was not queued"`,
	`turnwait.go: waitFailure "the session's snapshot cannot be read: " + sanitizeLine(err.Error())`,
}

// TestInternalFailureSitesAreListed walks every non-test file for the failures
// that say no kind (above): a new one is a decision to report `internal`, to be
// made here, and giving one a kind takes it off the list. Negative control: a
// waitFailure without its kind fails this.
func TestInternalFailureSitesAreListed(t *testing.T) {
	got := internalSites(t)
	slices.Sort(got)
	want := slices.Clone(internalFailureSites)
	slices.Sort(want)
	if slices.Equal(got, want) {
		return
	}
	for _, s := range got {
		if !slices.Contains(want, s) {
			t.Errorf("a failure reporting `internal` that is not listed: %s", s)
		}
	}
	for _, s := range want {
		if !slices.Contains(got, s) {
			t.Errorf("listed, and no such failure any more: %s", s)
		}
	}
	t.Errorf("the failures reporting `internal`, as many of each as there are:\n%s", strings.Join(got, "\n"))
}

// internalSites is each failure no kind is said for.
func internalSites(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	text := func(e ast.Expr) string {
		var b bytes.Buffer
		if err := printer.Fprint(&b, fset, e); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	var out []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				id, ok := n.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				switch {
				case id.Name == "exitf" && len(n.Args) > 1 && text(n.Args[0]) == "1":
					out = append(out, name+": exitf "+text(n.Args[1]))
				case id.Name == "kindf" && len(n.Args) > 1 && text(n.Args[0]) == "errkind.Internal":
					out = append(out, name+": kindf "+text(n.Args[1]))
				}
			case *ast.CompositeLit:
				id, ok := n.Type.(*ast.Ident)
				if !ok {
					return true
				}
				switch id.Name {
				case "waitFailure":
					if k := keyed(n, "kind"); k == nil || text(k) == "errkind.Internal" {
						out = append(out, name+": waitFailure "+text(keyedOr(n, "msg")))
					}
				case "exitError":
					if c := keyed(n, "code"); c != nil && text(c) == "1" && keyed(n, "kind") == nil {
						out = append(out, name+": exitError "+text(keyedOr(n, "msg")))
					}
				}
			}
			return true
		})
	}
	return out
}

func keyed(c *ast.CompositeLit, key string) ast.Expr {
	for _, e := range c.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
				return kv.Value
			}
		}
	}
	return nil
}

func keyedOr(c *ast.CompositeLit, key string) ast.Expr {
	if v := keyed(c, key); v != nil {
		return v
	}
	return &ast.Ident{Name: "?"}
}

// classifiedErr is a failure that says what it is, as a provider's does.
type classifiedErr struct {
	msg  string
	info errkind.Info
}

func (e *classifiedErr) Error() string           { return e.msg }
func (e *classifiedErr) ErrorInfo() errkind.Info { return e.info }

// TestAFailedTurnsResultSaysWhatTheFailureSaid: classification made where the
// failure happened reaches the result over a real socket, through the engine's
// ending and the event codec, with nothing guessed by the waiter. And the same
// failure read afterwards from lastTurn says the same.
func TestAFailedTurnsResultSaysWhatTheFailureSaid(t *testing.T) {
	boom := &classifiedErr{msg: "native: provider \"x\" failed (HTTP 429)",
		info: errkind.Info{Kind: errkind.Provider, Retryable: true, Status: 429, ProviderCode: "rate_limit_error"}}
	r := newWaitRig(t, 0, stubTurn{err: boom})
	stdout, stderr, code := executeErr([]string{"wait", waitRigShort, "--json"})
	if code != 0 {
		t.Fatalf("before any turn: exit %d, %q %q", code, stdout, stderr)
	}
	r.submit("go")
	waitFor(t, "the turn's end", func() bool { return r.eng.State().LastTurn != nil })

	stdout, stderr, code = executeErr([]string{"wait", waitRigShort, "--json"})
	m := decodeResult(t, stdout)
	e, _ := m["error"].(map[string]any)
	if code != 3 || m["status"] != "failed" || m["stopReason"] != "error" ||
		e["message"] != boom.msg || e["kind"] != "provider" || e["retryable"] != true {
		t.Fatalf("read from lastTurn: exit %d, %v, %q", code, m, stderr)
	}
	if d, _ := e["detail"].(map[string]any); d["status"] != float64(429) || d["provider_code"] != "rate_limit_error" || len(d) != 2 {
		t.Fatalf("its detail: %v", e["detail"])
	}
	stdout, _, _ = executeErr([]string{"wait", waitRigShort})
	if !strings.Contains(stdout, "\nerror: "+boom.msg+"\n") {
		t.Fatalf("plain output says the error as a line:\n%s", stdout)
	}

	r.script(stubTurn{err: boom})
	conn := r.dial(nil)
	w := goWait(conn, waitStart{submit: submitPrompt(conn, "again")}, waitOptions{})
	res, f := w.result(t)
	res = wantResult(t, res, f, statusFailed)
	want := resultError{Message: boom.msg, Kind: errkind.Provider, Retryable: true, Detail: &errorDetail{Status: 429, ProviderCode: "rate_limit_error"}}
	if res.Error == nil || res.Error.Message != want.Message || res.Error.Kind != want.Kind || !res.Error.Retryable ||
		res.Error.Detail == nil || *res.Error.Detail != *want.Detail || res.StopReason != stopError {
		t.Fatalf("followed live: %+v", res)
	}
}

// waiterAt is a waiter at the target turn, for an ending handed to it directly:
// what an older host's bytes decode to, or a later one's.
func waiterAt(acc *turnAccumulator) *turnWaiter {
	w := newTurnWaiter(&waitConn{sessionID: "0199c0de-1111-7000-8000-00000000a001"}, waitStart{}, waitOptions{})
	w.target, w.folding, w.startSeen = waitTarget{kind: targetTurn, id: "turn-1"}, true, true
	w.inc = "inc"
	if acc != nil {
		w.acc = acc
	}
	return w
}

// TestAnEndingFromAnOlderHostIsUnknown: the bytes an older host writes — its
// failure with no kind — decode, and report kind unknown, claiming nothing is
// retryable. Unmarked by the class, which is an older vocabulary (auth there
// would be a guess). A later craze's kind this build does not know reads as
// unknown too, its flag standing; none of them is an error to read.
func TestAnEndingFromAnOlderHostIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		retryable  bool
	}{
		{"an older host", `{"type":"turn","turn":{"id":"turn-1","phase":"ended","errClass":"auth","err":"rejected"}}`, false},
		{"a later host", `{"type":"turn","turn":{"id":"turn-1","phase":"ended","errClass":"quantum","err":"rejected","kind":"quantum","retryable":true}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := agent.DecodeEvent(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			again, err := agent.EncodeEvent(ev)
			if err != nil || again != tc.body {
				t.Fatalf("encoded again: %s (%v), want it unchanged", again, err)
			}
			w := waiterAt(nil)
			w.ended(context.Background(), ev.Turn)
			res := wantResult(t, w.result, w.fail, statusFailed)
			if e := res.Error; e == nil || e.Message != "rejected" || e.Kind != errkind.Unknown || e.Retryable != tc.retryable || e.Detail != nil {
				t.Fatalf("its error: %+v", e)
			}
			if res.StopReason != stopError {
				t.Fatalf("its stop reason: %q", res.StopReason)
			}
		})
	}
}

// TestTheStopReasonRule: on every terminal status the engine's reason when it
// sent one, else the status's own; and a status that is not terminal has none.
func TestTheStopReasonRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		tu   agent.TurnInfo
		// status, stop reason and kind of the result ("" none).
		status, stop string
		kind         errkind.Kind
	}{
		{"end_turn", agent.TurnInfo{StopReason: "end_turn"}, statusCompleted, "end_turn", ""},
		{"max_tokens", agent.TurnInfo{StopReason: "max_tokens"}, statusFailed, "max_tokens", errkind.Agent},
		{"refusal", agent.TurnInfo{StopReason: "refusal"}, statusFailed, "refusal", errkind.Agent},
		{"max_turn_requests", agent.TurnInfo{StopReason: "max_turn_requests"}, statusFailed, "max_turn_requests", errkind.Agent},
		{"cancelled", agent.TurnInfo{StopReason: "cancelled"}, statusCancelled, "cancelled", ""},
		{"a failure with no reason", agent.TurnInfo{Err: "boom", Info: errkind.New(errkind.Internal)}, statusFailed, "error", errkind.Internal},
		{"a failure with its reason", agent.TurnInfo{Err: "boom", Info: errkind.New(errkind.Internal), StopReason: "refusal"}, statusFailed, "refusal", errkind.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := waiterAt(nil)
			tc.tu.ID, tc.tu.Phase = "turn-1", agent.TurnEnded
			w.ended(context.Background(), &tc.tu)
			res := wantResult(t, w.result, w.fail, tc.status)
			if res.StopReason != tc.stop {
				t.Fatalf("stopReason %q, want %q", res.StopReason, tc.stop)
			}
			switch {
			case tc.kind == "" && res.Error != nil:
				t.Fatalf("an error on a result that has none: %+v", res.Error)
			case tc.kind != "" && (res.Error == nil || res.Error.Kind != tc.kind || res.Error.Retryable):
				t.Fatalf("its error: %+v, want kind %s", res.Error, tc.kind)
			}
		})
	}
	for status, want := range map[string]string{statusCompleted: "end_turn", statusFailed: "error", statusCancelled: "cancelled", statusDropped: "cancelled"} {
		res := newResult(resultSession{}, status)
		res.fill(newTurnAccumulator())
		if res.StopReason != want {
			t.Errorf("%s with no reason told: %q, want %q", status, res.StopReason, want)
		}
	}
	for _, status := range []string{statusIdle, statusNeedsInput, statusRunning} {
		res := newResult(resultSession{}, status)
		res.fill(newTurnAccumulator())
		if res.StopReason != "" {
			t.Errorf("%s has a stop reason: %q", status, res.StopReason)
		}
	}
}

// TestTheLastTurnsEndingSaysTheSame: a result settled afterwards, from
// session.state's lastTurn, by what that says — and an older host, which says no
// reason at all, still has one on the result. Its exit does not change.
func TestTheLastTurnsEndingSaysTheSame(t *testing.T) {
	for _, tc := range []struct {
		name         string
		lt           protocol.LastTurn
		status, stop string
		kind         errkind.Kind
		retryable    bool
	}{
		{"done", protocol.LastTurn{Outcome: protocol.TurnDone, StopReason: "end_turn"}, statusCompleted, "end_turn", "", false},
		{"done, an older host", protocol.LastTurn{Outcome: protocol.TurnDone}, statusCompleted, "end_turn", "", false},
		{"cancelled", protocol.LastTurn{Outcome: protocol.TurnCancelled, StopReason: "cancelled"}, statusCancelled, "cancelled", "", false},
		{"cancelled, an older host", protocol.LastTurn{Outcome: protocol.TurnCancelled}, statusCancelled, "cancelled", "", false},
		{"closed under it", protocol.LastTurn{Outcome: protocol.TurnCancelled, StopReason: "closing"}, statusCancelled, "stopped", errkind.Stopped, false},
		{"failed", protocol.LastTurn{Outcome: protocol.TurnFailed, Err: "boom", Kind: "quota", Status: 429, ProviderCode: "insufficient_quota"}, statusFailed, "error", errkind.Quota, false},
		{"failed, an older host", protocol.LastTurn{Outcome: protocol.TurnFailed, Err: "boom"}, statusFailed, "error", errkind.Unknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := waiterAt(nil)
			w.settleOutcome(context.Background(), &tc.lt)
			res := wantResult(t, w.result, w.fail, tc.status)
			if res.StopReason != tc.stop {
				t.Fatalf("stopReason %q, want %q", res.StopReason, tc.stop)
			}
			switch {
			case tc.kind == "" && res.Error != nil:
				t.Fatalf("an error on a result that has none: %+v", res.Error)
			case tc.kind != "" && (res.Error == nil || res.Error.Kind != tc.kind || res.Error.Retryable != tc.retryable):
				t.Fatalf("its error: %+v", res.Error)
			}
		})
	}
}

// TestPlanEndedIsTheActualEnding: true only for a turn that really ended at its
// plan: returned (the ACP agents', stop reason cancelled), or approved with nothing
// after it (native's, end_turn). An agent that goes on to carry the plan out did not.
func TestPlanEndedIsTheActualEnding(t *testing.T) {
	plan := func(accepted bool) agent.Event {
		return agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{Name: "p", Plan: "1. x", Auto: true, Accepted: accepted}}
	}
	tool := func(id, status string) agent.Event {
		return agent.Event{Type: agent.EventTool, Tool: &agent.ToolEvent{ID: id, Status: status}}
	}
	answered := agent.Event{Type: agent.EventAsk, Ask: &agent.AskUpdate{Kind: agent.AskPlan, Outcome: agent.AskAnswered, Accepted: true}}
	for _, tc := range []struct {
		name string
		evs  []agent.Event
		stop string
		want bool
	}{
		{"no plan at all", []agent.Event{textEv("done")}, "end_turn", false},
		{"native: approved automatically", []agent.Event{tool("exit", "in_progress"), plan(true), tool("exit", "completed")}, "end_turn", true},
		{"native: approved by a person", []agent.Event{answered}, "end_turn", true},
		{"native: a later call the approval refused", []agent.Event{plan(true), tool("vetoed", "failed")}, "end_turn", true},
		{"returned", []agent.Event{plan(false)}, "cancelled", true},
		{"accepted, and carried out", []agent.Event{plan(true), textEv("doing it")}, "end_turn", false},
		{"accepted, and a tool run", []agent.Event{plan(true), tool("edit", "in_progress")}, "end_turn", false},
		{"accepted, and stopped by the step limit", []agent.Event{plan(true)}, "max_turn_requests", false},
		{"accepted, then another plan open", []agent.Event{plan(true), {Type: agent.EventPlan, Plan: &agent.PlanEvent{Name: "q", Plan: "2. y"}}}, "end_turn", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTurnAccumulator()
			for _, ev := range tc.evs {
				a.fold(ev)
			}
			w := waiterAt(a)
			w.ended(context.Background(), &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, StopReason: tc.stop})
			if w.result == nil || w.result.PlanEnded != tc.want {
				t.Fatalf("planEnded on %+v, want %v", w.result, tc.want)
			}
			b, err := json.Marshal(w.result)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(b), `"planEnded":true`); got != tc.want {
				t.Fatalf("on the wire: %s", b)
			}
		})
	}
}

// TestPlanEndedReadAfterwards: a turn settled from lastTurn is `completed` whatever
// stopped it short (that outcome is done), so its own reason still decides: an
// approved plan the step limit cut off did not end there.
func TestPlanEndedReadAfterwards(t *testing.T) {
	for stop, want := range map[string]bool{"end_turn": true, "max_turn_requests": false} {
		a := newTurnAccumulator()
		a.fold(agent.Event{Type: agent.EventPlan, Plan: &agent.PlanEvent{Name: "p", Plan: "1. x", Auto: true, Accepted: true}})
		w := waiterAt(a)
		w.settleOutcome(context.Background(), &protocol.LastTurn{Outcome: protocol.TurnDone, StopReason: stop})
		res := wantResult(t, w.result, w.fail, statusCompleted)
		if res.PlanEnded != want || res.StopReason != stop {
			t.Errorf("%s: %+v, want planEnded %v", stop, res, want)
		}
	}
}

// TestTheWaitersOwnFailuresHaveTheirKinds: the failures that are the waiter's
// alone, each with the kind the reference's table gives it, and the retryable that
// goes with it.
func TestTheWaitersOwnFailuresHaveTheirKinds(t *testing.T) {
	check := func(t *testing.T, f *waitFailure, kind errkind.Kind, retryable bool) {
		t.Helper()
		if f == nil || f.kind != kind || kind.Retryable() != retryable {
			t.Fatalf("the failure: %+v, want kind %s", f, kind)
		}
	}
	t.Run("the socket lost, the host still listed", func(t *testing.T) {
		r := newWaitRig(t, 0)
		w := newTurnWaiter(r.dial(nil), waitStart{}, waitOptions{})
		w.lost(errors.New("broken pipe"))
		check(t, w.fail, errkind.ConnectionLost, true)
	})
	t.Run("the socket lost, the host gone", func(t *testing.T) {
		serveHome(t)
		w := waiterAt(nil)
		w.lost(errors.New("broken pipe"))
		check(t, w.fail, errkind.HostExited, true)
	})
	t.Run("the session's end from its host", func(t *testing.T) {
		serveHome(t)
		w := waiterAt(nil)
		w.item(context.Background(), pumped{it: remote.Item{Kind: remote.KindEnd}})
		check(t, w.fail, errkind.Stopped, false)
	})
	t.Run("the engine's close under the turn", func(t *testing.T) {
		serveHome(t)
		w := waiterAt(nil)
		w.ended(context.Background(), &agent.TurnInfo{ID: "turn-1", Phase: agent.TurnEnded, Synthetic: true, StopReason: stopClosing})
		check(t, w.fail, errkind.Stopped, false)
	})
	t.Run("the ending not observed", func(t *testing.T) {
		w := waiterAt(nil)
		w.notObserved()
		check(t, w.fail, errkind.Unobserved, true)
	})
	t.Run("the session's start failed", func(t *testing.T) {
		for kind, want := range map[string]errkind.Kind{"": errkind.StartFailed, "model_not_found": errkind.ModelNotFound, "internal": errkind.Internal, "quantum": errkind.Unknown} {
			w := waiterAt(nil)
			w.item(context.Background(), pumped{it: remote.Item{Kind: remote.KindReady, Ready: &protocol.ReadyParams{StartFailed: true, Err: "boom", Kind: kind}}})
			if w.fail == nil || w.fail.kind != want {
				t.Fatalf("started with kind %q: %+v, want %s", kind, w.fail, want)
			}
		}
	})
	t.Run("a signal to a plain wait", func(t *testing.T) {
		w := waiterAt(nil)
		w.signalled(nil)
		check(t, w.fail, errkind.Interrupted, false)
	})
	t.Run("--timeout before a target", func(t *testing.T) {
		w := newTurnWaiter(&waitConn{}, waitStart{}, waitOptions{})
		w.timedOut(nil)
		check(t, w.fail, errkind.TimeoutBeforePrompt, true)
	})
	t.Run("a submit that failed", func(t *testing.T) {
		for _, tc := range []struct {
			err  error
			want errkind.Kind
		}{
			{errPromptWithheld, errkind.Interrupted},
			{&submitError{msg: "x", kind: errkind.Unobserved}, errkind.Unobserved},
			{errors.New("x"), errkind.Internal},
		} {
			w := waiterAt(nil)
			w.submitAnswered(context.Background(), submitAnswer{err: tc.err})
			if w.fail == nil || w.fail.kind != tc.want {
				t.Fatalf("%v: %+v, want %s", tc.err, w.fail, tc.want)
			}
		}
	})
	t.Run("the host's refusal of a prompt", func(t *testing.T) {
		for reason, want := range map[protocol.Reason]errkind.Kind{
			protocol.ReasonNotAccepting:   errkind.Stopped,
			protocol.ReasonStartFailed:    errkind.StartFailed,
			protocol.ReasonTextTooLong:    errkind.Prompt,
			protocol.ReasonUnknownSession: errkind.Config,
			protocol.ReasonQueueFull:      errkind.Agent,
			protocol.ReasonForeignTurn:    errkind.Agent,
			protocol.ReasonFailed:         errkind.Internal,
		} {
			if got := refusalKind(&remote.Error{Reason: reason}); got != want {
				t.Errorf("%s: %s, want %s", reason, got, want)
			}
		}
	})
}

// TestAResultNeverCarriesMoreThanItsDetail: the object's shape, whatever a host
// sent: a message on one line, and a detail of names and numbers, no text.
func TestAResultNeverCarriesMoreThanItsDetail(t *testing.T) {
	e := newResultError("a\nb\x1b[31m", errkind.Info{Kind: errkind.Provider, Status: -32602, ProviderCode: "sk-abc DEF {\"prompt\":\"secret\"}"})
	if e.Message != "a b [31m" || e.Detail != nil {
		t.Fatalf("%+v", e)
	}
	b, err := json.Marshal(newResultError("m", errkind.Info{Kind: errkind.Quota, Status: 429, ProviderCode: "insufficient_quota"}))
	if err != nil || string(b) != `{"message":"m","kind":"quota","retryable":false,"detail":{"status":429,"provider_code":"insufficient_quota"}}` {
		t.Fatalf("%s %v", b, err)
	}
}

// usageRun is a command line through what Execute does (report), its one object
// on stdout, if any, decoded.
func usageRun(t *testing.T, argv ...string) (stdout, stderr string, code int, obj map[string]any) {
	t.Helper()
	stdout, stderr, code = executeErr(argv)
	if stdout != "" {
		obj = decodeResult(t, stdout)
	}
	return stdout, stderr, code, obj
}

// TestUsageErrorsUnderJSON: a command line that asked for JSON and could not be
// run prints exactly one {"error":{…}} object on stdout, the same words as the
// stderr line, which stays. An unknown flag before and after --json, an invalid
// value, an argument error, a failure before the command runs, and a command's own
// usage errors, each with its kind.
func TestUsageErrorsUnderJSON(t *testing.T) {
	serveHome(t)
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		argv []string
		kind errkind.Kind
	}{
		{"an unknown flag after --json", []string{"wait", "--json", "--nope"}, errkind.Usage},
		{"an unknown flag before --json", []string{"wait", "--nope", "--json"}, errkind.Usage},
		{"a command that has no --json", []string{"version", "--json"}, errkind.Usage},
		{"--json on the root", []string{"--json"}, errkind.Usage},
		{"an unknown command", []string{"--json", "nosuchcommand"}, errkind.Usage},
		{"an invalid value", []string{"wait", "x", "--json", "--timeout", "soon"}, errkind.Usage},
		{"a negative value", []string{"wait", "x", "--json", "--timeout", "-1s"}, errkind.Usage},
		{"a missing value", []string{"wait", "x", "--json", "--timeout"}, errkind.Usage},
		{"an argument error", []string{"wait", "a", "b", "--json"}, errkind.Usage},
		{"no argument", []string{"wait", "--json"}, errkind.Usage},
		{"a flag --json itself cannot read", []string{"ps", "--json=maybe"}, errkind.Usage},
		{"new: two flags that exclude each other", []string{"new", "--wait", "--json", "--plan", "--ask", "hi"}, errkind.Usage},
		{"new: a blank title", []string{"new", "--json", "--title", " "}, errkind.Usage},
		{"a bool shorthand's bad value before --json", []string{"new", "-h=C", "--json"}, errkind.Usage},
		{"an unknown shorthand before a value's", []string{"wait", "-xC", "--json"}, errkind.Usage},
		{"new --wait: an unknown provider", []string{"new", "--wait", "--json", "--provider", "nosuch", "hi"}, errkind.Config},
		{"new: an unknown provider", []string{"new", "--json", "--provider", "nosuch", "hi"}, errkind.Config},
		{"new --wait: an unreadable prompt", []string{"new", "--wait", "--json", "--prompt-file", "/no/such/file"}, errkind.Prompt},
		{"new --wait: an empty prompt", []string{"new", "--wait", "--json", "--prompt-file", empty}, errkind.Prompt},
		{"new --wait: an over-size prompt", []string{"new", "--wait", "--json", strings.Repeat("x", promptTextMax+1)}, errkind.Prompt},
		{"send: an over-size prompt", []string{"send", "abcd", "--json", strings.Repeat("x", promptTextMax+1)}, errkind.Prompt},
		{"send: both prompts", []string{"send", "abcd", "--json", "--prompt-file", empty, "words"}, errkind.Usage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code, obj := usageRun(t, tc.argv...)
			if code != 2 || obj == nil {
				t.Fatalf("exit %d, object %v, stderr %q", code, obj, stderr)
			}
			if len(obj) != 1 || !errorIs(obj, strings.TrimSuffix(stderr, "\n"), string(tc.kind), false) {
				t.Fatalf("the object is %v, stderr %q, want kind %s", obj, stderr, tc.kind)
			}
		})
	}
	t.Run("a failure before the command runs", func(t *testing.T) {
		t.Setenv("CRAZE_"+"CONFIG", "/x")
		_, stderr, code, obj := usageRun(t, "wait", "x", "--json")
		if code != 2 || len(obj) != 1 || !errorIs(obj, strings.TrimSuffix(stderr, "\n"), "usage", false) {
			t.Fatalf("exit %d, %v, %q", code, obj, stderr)
		}
	})
	t.Run("an empty prompt on stdin", func(t *testing.T) {
		run := goCrazeIn(strings.NewReader("  \n"), "new", "--wait", "--json", "--provider", "cursor")
		stdout, stderr, code := run.wait(t)
		if code != 2 || !errorIs(decodeResult(t, stdout), strings.TrimSuffix(stderr, "\n"), "prompt", false) {
			t.Fatalf("exit %d, %q %q", code, stdout, stderr)
		}
	})
}

// TestUsageErrorsWithoutJSONStayPlain: --json in the line as anything but the flag
// is not the flag: a value it is the argument of, or a word after `--`. Each is
// the stderr line alone. And a later --json=false takes it back, as pflag would.
func TestUsageErrorsWithoutJSONStayPlain(t *testing.T) {
	serveHome(t)
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"no --json at all", []string{"wait", "--nope"}, "craze wait: unknown or malformed flag; see craze wait --help\n"},
		{"the value of a string flag", []string{"new", "--title", "--json", "--nope"}, "craze new: unknown or malformed flag; see craze new --help\n"},
		{"the value of a shorthand", []string{"new", "-C", "--json", "--nope"}, "craze new: unknown or malformed flag; see craze new --help\n"},
		{"the value of a duration", []string{"wait", "--timeout", "--json"}, "craze wait: unknown or malformed flag; see craze wait --help\n"},
		{"inline in a flag's value", []string{"new", "--title=--json", "--nope"}, "craze new: unknown or malformed flag; see craze new --help\n"},
		{"after --", []string{"new", "--wait", "--nope", "--", "--json"}, "craze new: unknown or malformed flag; see craze new --help\n"},
		{"after -- with the error past it", []string{"send", "--", "--json"}, ""},
		{"taken back", []string{"wait", "--json", "--json=false", "--nope"}, "craze wait: unknown or malformed flag; see craze wait --help\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code, _ := usageRun(t, tc.argv...)
			if code != 2 || stdout != "" || (tc.want != "" && stderr != tc.want) {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
		})
	}
	t.Run("--json before a value that is itself --json", func(t *testing.T) {
		_, stderr, code, obj := usageRun(t, "new", "--json", "--title", "--json", "--nope")
		if code != 2 || !errorIs(obj, strings.TrimSuffix(stderr, "\n"), "usage", false) {
			t.Fatalf("exit %d, %v, %q", code, obj, stderr)
		}
	})
}

// TestCommandsWithAStdoutOfTheirOwnGetNoObject: craze prompt's stdout is an event
// stream; a completion request's is the shell's; bridge's is a protocol.
func TestCommandsWithAStdoutOfTheirOwnGetNoObject(t *testing.T) {
	serveHome(t)
	for _, argv := range [][]string{
		{"prompt", "--json", "--nope"},
		{"prompt", "--json", "a", "b"},
		{"bridge", "--json"},
		{"__complete", "wait", "--json", "--nope", ""},
	} {
		stdout, stderr, code := executeErr(argv)
		if stdout != "" || stderr == "" || code == 0 {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", argv, code, stdout, stderr)
		}
	}
}

// TestJSONRequestedReadsAsPflagDoes: every short command line of these words, read
// by pflag itself and by jsonRequested, says the same. The flags here take any value,
// so that a word's place is all that decides; a line pflag fails on (a flag it does
// not know, a value it did not get) is TestJSONRequested's. Negative control: reading
// `-h=C` as two shorthands disagrees on `-h=C --json`.
func TestJSONRequestedReadsAsPflagDoes(t *testing.T) {
	words := []string{"--json", "--json=false", "--json=x", "--json=", "--title", "--title=x", "--wait", "--wait=x",
		"-C", "-Cx", "-C=x", "-C=", "-w", "-w=x", "-w=C", "-wC", "-wCx", "-wC=x", "-wh", "-h=C", "-hC", "-h=", "-ww=C", "--", "-", "word"}
	flagsOf := func() (*pflag.FlagSet, *anyValue) {
		fs := pflag.NewFlagSet("x", pflag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.Usage = func() {}
		json := &anyValue{}
		for _, f := range []struct {
			name, short string
			boolean     bool
			v           pflag.Value
		}{{"title", "", false, &anyValue{}}, {"dir", "C", false, &anyValue{}}, {"wait", "w", true, &anyValue{}}, {"help", "h", true, &anyValue{}}, {"json", "", true, json}} {
			flag := fs.VarPF(f.v, f.name, f.short, "")
			if f.boolean {
				flag.NoOptDefVal = "true"
			}
		}
		return fs, json
	}
	cmd := &cobra.Command{Use: "x"}
	cmd.Flags().AddFlagSet(func() *pflag.FlagSet { fs, _ := flagsOf(); return fs }())
	compared := 0
	check := func(argv []string) {
		fs, json := flagsOf()
		if fs.Parse(argv) != nil {
			return
		}
		compared++
		if want := json.asked(); jsonRequested(cmd, argv) != want {
			t.Errorf("jsonRequested(%q) = %v, pflag read %v", argv, !want, want)
		}
	}
	for _, a := range words {
		check([]string{a})
		for _, b := range words {
			check([]string{a, b})
			for _, c := range words {
				check([]string{a, b, c})
			}
		}
	}
	if compared < 1000 {
		t.Fatalf("only %d command lines parsed", compared)
	}
}

// anyValue is a flag's value that takes any word, and remembers the last.
type anyValue struct {
	set  bool
	last string
}

func (v *anyValue) String() string { return v.last }
func (v *anyValue) Type() string   { return "any" }
func (v *anyValue) Set(s string) error {
	v.set, v.last = true, s
	return nil
}

// asked is jsonRequested's reading of what --json was given: never, no; a value
// that is no bool still asks, that being the error to report.
func (v *anyValue) asked() bool {
	if !v.set {
		return false
	}
	b, err := strconv.ParseBool(v.last)
	return err != nil || b
}

// TestJSONRequested: the reading itself, against a command's real flags.
func TestJSONRequested(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	cmd.Flags().String("title", "", "")
	cmd.Flags().StringP("dir", "C", "", "")
	cmd.Flags().BoolP("wait", "w", false, "")
	cmd.Flags().Bool("json", false, "")
	for argv, want := range map[string]bool{
		"--json":              true,
		"--json=true":         true,
		"--json=false":        false,
		"--json=garbage":      true,
		"--json --json=false": false,
		"--json=false --json": true,
		"-- --json":           false,
		"--json --":           true,
		"--title --json":      false,
		"--title=x --json":    true,
		"--title x --json":    true,
		"-C --json":           false,
		"-Cdir --json":        true,
		"-wC --json":          false,
		"-w --json":           true,
		"-w=C --json":         true,
		"-w=x --json":         true,
		"-w=wC --json":        true,
		"-C=x --json":         true,
		"-C= --json":          true,
		"-w= --json":          true,
		"-wC=x --json":        true,
		"-w=true -C --json":   false,
		"-xC --json":          true,
		"-wxC --json":         true,
		"-x=C --json":         true,
		"-x --json":           true,
		"---x --json":         true,
		"--=x --json":         true,
		"--wait --json":       true,
		"--unknown --json":    true,
		"--unknown=--json":    false,
		"word --json":         true,
		"-":                   false,
		"--title":             false,
		"--json-ish":          false,
	} {
		if got := jsonRequested(cmd, strings.Fields(argv)); got != want {
			t.Errorf("jsonRequested(%q) = %v, want %v", argv, got, want)
		}
	}
}
