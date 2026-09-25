package engine

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// classifyCase is one row of classify's table: an error, the code Code answers
// for it, the reason Reason answers beside it, and whether gateRefusal forgets
// it.
type classifyCase struct {
	name string
	err  error
	code string
	// reason is Reason(err): the sentinel's own name on the wire (plan 027
	// §3.2), a one-way door — these spellings are the wire's.
	reason string
	// forgot is gateRefusal(err): the answer is NOT stored, and the same id
	// may be resent for a genuine first attempt.
	forgot bool
}

// classifyTable is the table itself, built fresh so that the guard below and
// the walk above read exactly the same rows.
func classifyTable() []classifyCase {
	boom := errors.New("boom")
	return []classifyCase{
		{"ErrIndexWrite", fmt.Errorf("%w: %w", ErrIndexWrite, boom), "index_write", "index_write", false},
		{"ErrNotAccepting", ErrNotAccepting, "not_accepting", "not_accepting", true},
		{"agent.ErrNotInTurn", agent.ErrNotInTurn, "not_accepting", "not_in_turn", true},
		{"ErrCommandInProgress", ErrCommandInProgress, "in_progress", "in_progress", true},
		// A DUPLICATE of a blocking command whose own wait timed out against an
		// owner that is still running (waitReceipt, r31 finding 2). It wraps the
		// context's error too, so this row is also what pins the case ORDER:
		// ErrCommandInProgress is matched ahead of the generic context case
		// below, which would otherwise call this `aborted` — "the command may
		// already have happened, use a new id" — when nothing ran at all and the
		// same id is exactly what to resend.
		{"a duplicate's own timeout against an open reservation",
			fmt.Errorf("%w: %w", ErrCommandInProgress, context.DeadlineExceeded), "in_progress", "in_progress", true},
		{"ErrAlreadyPending", ErrAlreadyPending, "already_submitted", "already_submitted", false},
		{"ErrStaleTurn", ErrStaleTurn, "stale_turn", "stale_turn", false},
		{"ErrStaleVersion", ErrStaleVersion, "stale_version", "stale_version", false},
		{"ErrUnknownRow", ErrUnknownRow, "unknown_row", "unknown_row", false},
		{"agent.ErrBadAnswer", agent.ErrBadAnswer, "bad_request", "bad_answer", false},
		{"agent.ErrAlreadyResolved", agent.ErrAlreadyResolved, "already_resolved", "already_resolved", false},
		{"agent.ErrUnknownAsk", agent.ErrUnknownAsk, "unknown_ask", "unknown_ask", false},
		// A stop of a sub-agent the session holds no running child for (plan 026
		// §3.10): about the resource named, like unknown_ask, and stored.
		{"agent.ErrNoSuchSubagent", agent.ErrNoSuchSubagent, "unknown_subagent", "unknown_subagent", false},
		{"ErrCommandAborted", ErrCommandAborted, "aborted", "command_aborted", false},
		// It wraps the context's own error, so this row also pins it ahead of
		// the context case, whose reason is `context`.
		{"ErrSetOutcomeUnknown", setOutcomeUnknown(context.Canceled), "aborted", "set_outcome_unknown", false},
		{"errNotRun (a Set that never ran)", notRun(context.Canceled), "unavailable", "not_run", true},
		// A config change bound to a model the session has left (plan 025
		// design 3): refused before the claim, and forgotten, because nothing
		// ran and the model can come back.
		{"ErrStaleModel", ErrStaleModel, "stale_model", "stale_model", true},
		// A Set the agent took whose answer no longer lists the option: it ran.
		{"agent.ErrOptionGone", agent.ErrOptionGone, "failed", "option_gone", false},
		// A Set whose answer could not be read (plan 025 design 1, "malformed
		// is an error"): it ran — the agent answered, and may have made the
		// change — and nothing of the answer was installed, so its outcome is
		// one the engine cannot vouch for: STORED, "aborted", never the
		// default's "failed", which reads as a definite failure (astra r5
		// item 2). Wrapped as the live session returns it.
		{"agent.ErrBadCatalog",
			fmt.Errorf("session/set_config_option: %w: member 0 is not an option", agent.ErrBadCatalog),
			"aborted", "bad_catalog", false},
		// A command that RAN and gave up on its own context: the write may
		// already have happened, so the answer is stored and the client re-reads
		// state rather than resending the work under a new id (r30 finding 1).
		{"a bare context.Canceled", context.Canceled, "aborted", "context", false},
		{"a bare context.DeadlineExceeded", context.DeadlineExceeded, "aborted", "context", false},
		{"a wrapped context.DeadlineExceeded",
			fmt.Errorf("writing session/prompt: %w", context.DeadlineExceeded), "aborted", "context", false},
		{"agent.ErrAskUnavailable", agent.ErrAskUnavailable, "unavailable", "ask_unavailable", true},
		{"agent.ErrSetUnavailable", agent.ErrSetUnavailable, "unavailable", "set_unavailable", true},
		{"ErrUnavailable", ErrUnavailable, "unavailable", "log_backed_up", true},
		// Attach outrun by the ring (attach.go): nothing registered, ask again.
		{"ErrAttachRaced", fmt.Errorf("%w: 4 snapshots refused", ErrAttachRaced), "unavailable", "attach_raced", true},
		{"ErrBadRequest", ErrBadRequest, "bad_request", "bad_request", false},
		// ClaimClient's answer, never a command's, and never on the wire (the
		// server answers `resumed: false`): it adds no reason of its own.
		{"ErrUnknownClient", ErrUnknownClient, "bad_request", "bad_request", false},
		{"ErrUnknownCommand", ErrUnknownCommand, "unknown_command", "unknown_command", false},
		{"agent.ErrQueueFull", agent.ErrQueueFull, "queue_full", "queue_full", false},
		{"agent.ErrQueueTextTooLong", agent.ErrQueueTextTooLong, "text_too_long", "text_too_long", false},
		{"agent.ErrPromptInFlight", agent.ErrPromptInFlight, "prompt_in_flight", "prompt_in_flight", false},
		{"agent.ErrForeignTurn", agent.ErrForeignTurn, "foreign_turn", "foreign_turn", false},
		{"agent.ErrPromptCancelled", agent.ErrPromptCancelled, "prompt_cancelled", "prompt_cancelled", false},
		{"agent.ErrUnsupported", agent.ErrUnsupported, "unsupported", "unsupported", false},
		{"an unclassified provider/RPC error (classify's own default)", boom, "failed", "failed", false},
	}
}

// wireReasons is plan 027 §3.2's reason table, transcribed: for each code
// several sentinels share, every reason a host may send beside it — the
// engine's own and internal/protocol's alike, since a reason is only ever
// checked against its code's row. A code not listed here has exactly one
// reason, the code itself ("every other code | the code itself").
var wireReasons = map[string][]string{
	"not_accepting": {"not_accepting", "not_in_turn", "start_failed"},
	"aborted":       {"command_aborted", "set_outcome_unknown", "bad_catalog", "context"},
	"unavailable":   {"log_backed_up", "ask_unavailable", "set_unavailable", "not_run", "attach_raced", "not_ready", "busy"},
	"failed":        {"option_gone", "failed", "response_too_large", "snapshot_too_large"},
	"bad_request": {"bad_request", "bad_answer", "hello_required", "unknown_field", "line_too_long",
		"protocol_version", "bad_token", "already_attached"},
	"unsupported": {"unsupported", "unknown_method", "stop_unsupported", "roster_unsupported", "hub_only"},
}

// reasonBelongsToCode reports whether §3.2's table lets a host send reason
// beside code.
func reasonBelongsToCode(code, reason string) bool {
	allowed, ok := wireReasons[code]
	if !ok {
		return reason == code
	}
	return slices.Contains(allowed, reason)
}

// TestClassifyIsTheOneTable is r28 finding 1: walks EVERY exported sentinel
// Code (control.go) can return — engine's own and the ones it takes from
// internal/agent — plus errNotRun (settings.go, unexported, r28's own new
// one) and a plain unclassified error standing for classify's "failed"
// default, and asserts the invariant classify exists to hold over the WHOLE
// closed set:
//
//	Code(err) ∈ {unavailable, not_accepting, in_progress, stale_model} ⇔ the receipt was
//	NOT stored — gateRefusal(err) is true, the same id may be resent, and
//	classify(err).stored is false.
//
// Code and gateRefusal are both DERIVED from classify (control.go,
// receipts.go), so this one table is what pins them from drifting apart
// again the way r28 finding 1 found them apart over agent.ErrNotInTurn and a
// Set's dead ctx: Code already said not_accepting for ErrNotInTurn while
// gateRefusal did not know it, and a command the docs promise is never stored
// was stored anyway.
//
// Reason is the third column (plan 027 §3.2), derived from the same switch:
// every row's reason is pinned, none is empty for a non-nil error, and each
// belongs to its code's row of §3.2's table (wireReasons). The spellings are
// the wire's, so a changed one fails here before it can ship.
func TestClassifyIsTheOneTable(t *testing.T) {
	cases := classifyTable()
	t.Run("no sentinel escapes the table", func(t *testing.T) { guardSentinels(t, cases) })
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Code(tc.err); got != tc.code {
				t.Fatalf("Code(%v) = %q, want %q", tc.err, got, tc.code)
			}
			if got := Reason(tc.err); got != tc.reason {
				t.Fatalf("Reason(%v) = %q, want %q", tc.err, got, tc.reason)
			}
			if tc.reason == "" {
				t.Fatalf("%s: a non-nil error has no reason", tc.name)
			}
			if !reasonBelongsToCode(tc.code, tc.reason) {
				t.Fatalf("%s: reason %q is not one §3.2's table lets a host send with code %q", tc.name, tc.reason, tc.code)
			}
			if got := gateRefusal(tc.err); got != tc.forgot {
				t.Fatalf("gateRefusal(%v) = %v, want %v", tc.err, got, tc.forgot)
			}
			// The invariant itself, in both directions, over THIS row: forgotten
			// iff the code is one of the four codes never stored. A row that
			// fails this has a bad table, not a bad implementation — classify's
			// own switch is what both Code and gateRefusal read, so the two
			// cannot disagree with each other; they can still disagree with the
			// invariant, which is what this line catches.
			wantForgotten := tc.code == "unavailable" || tc.code == "not_accepting" || tc.code == "in_progress" ||
				tc.code == "stale_model"
			if tc.forgot != wantForgotten {
				t.Fatalf("%s: table says forgotten=%v for code %q, want %v", tc.name, tc.forgot, tc.code, wantForgotten)
			}
		})
	}
	if got := Code(nil); got != "" {
		t.Fatalf("Code(nil) = %q, want \"\"", got)
	}
	if got := Reason(nil); got != "" {
		t.Fatalf("Reason(nil) = %q, want \"\"", got)
	}
	if gateRefusal(nil) {
		t.Fatal("gateRefusal(nil) must be false: nil never reaches finish as a refusal")
	}
}

// TestEveryListedEngineReasonIsProduced holds the reverse direction against
// §3.2: every reason of the engine's own that the table lists is one some
// row of classify's table produces. A reason §3.2 promises and the engine
// never sends would leave a client's reconstruction (plan 027 §3.14) with a
// sentinel nothing maps to. The protocol-level reasons are internal/protocol's
// and are named here only to be set aside.
func TestEveryListedEngineReasonIsProduced(t *testing.T) {
	protocolReasons := map[string]bool{
		"start_failed": true, "not_ready": true, "busy": true, "response_too_large": true,
		"snapshot_too_large": true, "hello_required": true, "unknown_field": true, "line_too_long": true,
		"protocol_version": true, "bad_token": true, "already_attached": true, "unknown_method": true,
		"stop_unsupported": true, "roster_unsupported": true, "hub_only": true,
	}
	produced := map[string]bool{}
	for _, tc := range classifyTable() {
		produced[tc.reason] = true
	}
	for code, reasons := range wireReasons {
		for _, reason := range reasons {
			if protocolReasons[reason] {
				continue
			}
			if !produced[reason] {
				t.Errorf("§3.2 lists %q beside %q, and no row of classify's table produces it", reason, code)
			}
		}
	}
}

// engineSentinels is every exported Err* variable internal/engine declares,
// by the name the guard's scan of the sources spells. errNotRun is beside them
// although it is unexported: it is the one unexported error classify names,
// and the table has a row for it.
func engineSentinels() map[string]error {
	return map[string]error{
		"ErrNotAccepting":      ErrNotAccepting,
		"ErrAlreadyPending":    ErrAlreadyPending,
		"ErrStaleTurn":         ErrStaleTurn,
		"ErrStaleVersion":      ErrStaleVersion,
		"ErrStaleModel":        ErrStaleModel,
		"ErrUnknownRow":        ErrUnknownRow,
		"ErrUnavailable":       ErrUnavailable,
		"ErrBadRequest":        ErrBadRequest,
		"ErrUnknownCommand":    ErrUnknownCommand,
		"ErrCommandInProgress": ErrCommandInProgress,
		"ErrSetOutcomeUnknown": ErrSetOutcomeUnknown,
		"ErrIndexWrite":        ErrIndexWrite,
		"ErrCommandAborted":    ErrCommandAborted,
		"ErrAttachRaced":       ErrAttachRaced,
		"ErrUnknownClient":     ErrUnknownClient,
	}
}

// agentSentinels is every exported Err* variable internal/agent declares that
// a Control method can answer with, and therefore that classify has to place.
func agentSentinels() map[string]error {
	return map[string]error{
		"ErrBadAnswer":        agent.ErrBadAnswer,
		"ErrAlreadyResolved":  agent.ErrAlreadyResolved,
		"ErrUnknownAsk":       agent.ErrUnknownAsk,
		"ErrNoSuchSubagent":   agent.ErrNoSuchSubagent,
		"ErrAskUnavailable":   agent.ErrAskUnavailable,
		"ErrQueueFull":        agent.ErrQueueFull,
		"ErrQueueTextTooLong": agent.ErrQueueTextTooLong,
		"ErrNotInTurn":        agent.ErrNotInTurn,
		"ErrPromptInFlight":   agent.ErrPromptInFlight,
		"ErrUnsupported":      agent.ErrUnsupported,
		"ErrForeignTurn":      agent.ErrForeignTurn,
		"ErrPromptCancelled":  agent.ErrPromptCancelled,
		"ErrSetUnavailable":   agent.ErrSetUnavailable,
		"ErrOptionGone":       agent.ErrOptionGone,
		"ErrBadCatalog":       agent.ErrBadCatalog,
		// The session's refusal of a Set bound to a model it has left, made just
		// before the write. engine.ErrStaleModel is this very value, so the
		// table's ErrStaleModel row is its row too.
		"ErrStaleModel": agent.ErrStaleModel,
	}
}

// agentSentinelsNotClassified is the rest of internal/agent's exported Err*
// variables: the ones no Control method ever answers with, so classify names
// none of them and each would land on its "failed" default if one ever
// escaped. They are listed rather than ignored so that a NEW sentinel over
// there cannot join them silently — the scan below forces a decision about
// every one, and the decision for these is written down here.
//
//   - ErrAskIDInUse is the registry refusing a DUPLICATE ask id, which only
//     the provider side of internal/agent can hit: no command opens an ask.
//   - ErrClosed, ErrLogClosing, ErrSlowConsumer, ErrFlushGaveUp and
//     ErrObserverSet are the event log's own, about a subscription, a flush or
//     an observer — never a command's answer (a command refused for room is
//     told so with one of the three Unavailable sentinels above). Subscribe
//     and Attach return ErrClosed on a closed log, and a subscription ends
//     with it or ErrSlowConsumer, but neither takes a Command: no receipt is
//     ever stored for them.
//   - ErrNoRest and ErrRestUnavailable are Subscription.Rest's answers about
//     a subscription's closing tail (plan 027 §3.7) — read by the socket
//     server's forwarder after its log closed, never a command's answer.
//   - ErrAgentExited is the transport's. A command that runs into it ran, and
//     its failure is a real one: "failed" is exactly right for it, and it
//     needs no row.
var agentSentinelsNotClassified = []string{
	"ErrAgentExited",
	"ErrAskIDInUse",
	"ErrClosed",
	"ErrFlushGaveUp",
	"ErrLogClosing",
	"ErrNoRest",
	"ErrObserverSet",
	"ErrRestUnavailable",
	"ErrSlowConsumer",
}

// guardSentinels is what makes TestClassifyIsTheOneTable catch a sentinel
// somebody ADDS tomorrow, which the hand-written table alone could not: it
// parses the two packages' sources and holds their exported Err* variables
// against the maintained lists above, in BOTH directions, and then holds those
// lists against the table's rows.
//
// So a new sentinel fails here — first because the scan finds a name the lists
// do not have, and then, once it is listed, because no row of the table
// matches it — rather than slipping silently into classify's "failed" default
// with nobody having decided that is what it should be.
func guardSentinels(t *testing.T, cases []classifyCase) {
	t.Helper()
	engine := engineSentinels()
	classified := agentSentinels()

	wantEngine := make([]string, 0, len(engine))
	for name := range engine {
		wantEngine = append(wantEngine, name)
	}
	sameNames(t, "internal/engine", exportedErrVars(t, "."), wantEngine)

	wantAgent := append([]string(nil), agentSentinelsNotClassified...)
	for name := range classified {
		wantAgent = append(wantAgent, name)
	}
	sameNames(t, "internal/agent", exportedErrVars(t, filepath.Join("..", "agent")), wantAgent)

	inTable := func(target error) bool {
		for _, tc := range cases {
			if errors.Is(tc.err, target) {
				return true
			}
		}
		return false
	}
	for name, err := range engine {
		if !inTable(err) {
			t.Errorf("engine.%s is a sentinel classify has to place and the table has no row for it", name)
		}
	}
	for name, err := range classified {
		if !inTable(err) {
			t.Errorf("agent.%s is a sentinel classify has to place and the table has no row for it", name)
		}
	}
	// errNotRun is unexported, so the scan cannot see it; the table's own row
	// is all there is, and this is the line that says it must stay there.
	if !inTable(errNotRun) {
		t.Error("errNotRun is a sentinel classify names and the table has no row for it")
	}
}

// sameNames fails with the difference in both directions, which is the whole
// point of the guard: a name the sources have and the list does not is a
// sentinel nobody has classified, and one the list has and the sources do not
// is a list left behind by a rename.
func sameNames(t *testing.T, what string, found, listed []string) {
	t.Helper()
	have, want := map[string]bool{}, map[string]bool{}
	for _, n := range found {
		have[n] = true
	}
	for _, n := range listed {
		want[n] = true
	}
	var missing, extra []string
	for n := range have {
		if !want[n] {
			missing = append(missing, n)
		}
	}
	for n := range want {
		if !have[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("%s declares %v, which classify_test.go's own list does not name: classify it and list it here",
			what, missing)
	}
	if len(extra) > 0 {
		t.Errorf("%s no longer declares %v, which classify_test.go's own list still names", what, extra)
	}
}

// exportedErrVars is every exported Err* variable declared at package level in
// dir's non-test sources, by name: the go/ast scan the guard holds its lists
// against. Build tags are deliberately not applied — every file in the
// directory is read — so a sentinel behind one cannot hide from it.
func exportedErrVars(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, id := range vs.Names {
					if id.IsExported() && strings.HasPrefix(id.Name, "Err") {
						out = append(out, id.Name)
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestClassifyThroughRealCommands drives three of the table above through an
// actual Control call, where the fake session makes that feasible: the
// dead-ctx Set (errNotRun), a Set the provider refuses, and a Cancel that
// fails — the two new "failed" schedules r28 finding 1 names by name
// ("a provider / RPC refusal of a Set... or a Cancel that failed"). Every
// other row already has its own dedicated, real-command coverage elsewhere
// (cancel_test.go, sendnow_test.go, settings_test.go, queue_test.go,
// receipts_test.go, lifecycle_test.go, driver_test.go, index_test.go), which
// is what the table above exists to pin together rather than duplicate.
func TestClassifyThroughRealCommands(t *testing.T) {
	t.Run("errNotRun: a Set made with an already-dead context", func(t *testing.T) {
		r := newRig(t, Options{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := r.e.Set(ctx, Command{}, modeSetting("plan"))
		if Code(err) != "unavailable" || !errors.Is(err, context.Canceled) {
			t.Fatalf("a Set made with a dead context: %v (%s), want context.Canceled coded unavailable", err, Code(err))
		}
		if n := r.s.setCalls(); n != 0 {
			t.Fatalf("%d settings reached the provider for a dead-ctx Set", n)
		}
	})
	t.Run("failed: a Set the provider refused", func(t *testing.T) {
		r := newRig(t, Options{})
		boom := errors.New("the agent refused")
		r.s.failSets(boom)
		_, err := r.e.Set(context.Background(), Command{}, modeSetting("plan"))
		if Code(err) != "failed" || !errors.Is(err, boom) {
			t.Fatalf("a Set the provider refused: %v (%s), want it wrapped errors.Is boom and coded failed", err, Code(err))
		}
	})
	t.Run("failed: a Cancel that failed", func(t *testing.T) {
		r := newRig(t, Options{})
		turn := r.s.script(held())
		res := r.submit("one")
		await(t, turn.opened, "the turn to open")
		boom := errors.New("the pipe is gone")
		r.s.mu.Lock()
		r.s.cancelErr = boom
		r.s.mu.Unlock()
		_, err := r.e.Cancel(context.Background(), Command{}, res.Turn)
		if Code(err) != "failed" || !errors.Is(err, boom) {
			t.Fatalf("a cancel that failed: %v (%s), want it wrapped errors.Is boom and coded failed", err, Code(err))
		}
		turn.release()
		r.until(lastEnding)
	})
}

// TestAContextErrorFromACommandThatRanIsAborted is r30 finding 1, through the
// three real commands whose provider call takes a context and whose write can
// already have happened when that context ends: Cancel, Stop and Interject.
//
// "failed" told a client the command ran and definitely did not do what it was
// asked, whose documented answer is to send the work again under a NEW id — and
// for an Interject whose deadline passed after internal/acp had written its
// request, that is how the same text is interjected twice. Cancel was worse
// still: it answered CancelUnknown, which says in so many words that a cancel
// may already have been written, beside an error coded as though it had not.
// "aborted" is what both of them mean: STORED, so this id never runs again, and
// re-read state before deciding anything.
//
// Bare and WRAPPED, because a provider error that merely carries a context
// error underneath it is the shape a real transport returns.
func TestAContextErrorFromACommandThatRanIsAborted(t *testing.T) {
	for _, ctxErr := range []struct {
		name string
		err  error
	}{
		{"bare", context.DeadlineExceeded},
		{"wrapped", fmt.Errorf("writing session/cancel: %w", context.Canceled)},
	} {
		t.Run(ctxErr.name+": Cancel", func(t *testing.T) {
			r := newRig(t, Options{})
			turn := r.s.script(held())
			res := r.submit("one")
			await(t, turn.opened, "the turn to open")
			r.s.mu.Lock()
			r.s.cancelErr = ctxErr.err
			r.s.mu.Unlock()

			out, err := r.e.Cancel(context.Background(), Command{}, res.Turn)
			if Code(err) != "aborted" {
				t.Fatalf("a cancel that gave up on its context: %v (%s), want aborted", err, Code(err))
			}
			if !errors.Is(err, ctxErr.err) {
				t.Fatalf("the context error is not matchable through it: %v", err)
			}
			if out.Outcome != CancelUnknown {
				t.Fatalf("the outcome is %q, want %q: the code and the outcome say the same thing",
					out.Outcome, CancelUnknown)
			}
			turn.release()
			r.until(lastEnding)
		})
		t.Run(ctxErr.name+": Stop", func(t *testing.T) {
			r := newRig(t, Options{})
			turn := r.s.script(held())
			r.submit("one")
			await(t, turn.opened, "the turn to open")
			r.s.mu.Lock()
			r.s.cancelErr = ctxErr.err
			r.s.mu.Unlock()

			err := r.e.Stop(context.Background(), Command{})
			if Code(err) != "aborted" || !errors.Is(err, ctxErr.err) {
				t.Fatalf("a stop whose cancel gave up: %v (%s), want the context error coded aborted", err, Code(err))
			}
			turn.release()
			r.until(lastEnding)
		})
		t.Run(ctxErr.name+": Interject", func(t *testing.T) {
			r := newRig(t, Options{})
			r.s.failInterjects(ctxErr.err)
			err := r.e.Interject(context.Background(), Command{}, "and one more thing")
			if Code(err) != "aborted" || !errors.Is(err, ctxErr.err) {
				t.Fatalf("an interject that gave up: %v (%s), want the context error coded aborted", err, Code(err))
			}
		})
	}
}

// TestAContextErrorIsStoredSoTheSameIdNeverRunsAgain is the other half of the
// code: aborted is STORED. A resend of the same id replays that answer rather
// than making a second cancel, which is what makes "re-read state, and use a
// NEW id if you still want it" a rule with teeth.
func TestAContextErrorIsStoredSoTheSameIdNeverRunsAgain(t *testing.T) {
	r := newRig(t, Options{})
	turn := r.s.script(held())
	res := r.submit("one")
	await(t, turn.opened, "the turn to open")
	r.s.mu.Lock()
	r.s.cancelErr = context.DeadlineExceeded
	r.s.mu.Unlock()

	c := Command{Client: r.e.NewClientID(), ID: "1"}
	if _, err := r.e.Cancel(context.Background(), c, res.Turn); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the first cancel: %v", err)
	}
	wrote := r.s.cancelsWritten()
	if _, err := r.e.Cancel(context.Background(), c, res.Turn); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the resend answered %v, want the stored answer replayed", err)
	}
	if n := r.s.cancelsWritten(); n != wrote {
		t.Fatalf("the resend wrote %d cancels, want the first call's %d: an aborted answer is stored", n, wrote)
	}
	turn.release()
	r.until(lastEnding)
}
