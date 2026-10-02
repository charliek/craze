package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/remote"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/transcript"
	"github.com/charliek/craze/internal/tui"
)

// Errors reconstructed (plan 027 §3.14, A16): a host's refusal comes back as a
// *remote.Error whose errors.Is answers what the engine's own error answers in
// process — for every sentinel classify names — whose Error is the host's
// message verbatim, and whose Unwrap reaches ErrIndexWrite's cause.

// twin is one error as the engine answers it in process: the in-process twin
// a reconstructed error must be indistinguishable from. sentinel names the
// sentinel classify matches it by, as classify's source spells it (engine's
// own unqualified names qualified "engine.").
type twin struct {
	sentinel string
	why      string
	err      error
}

// failIndex is a session index whose every write fails with err.
type failIndex struct{ err error }

func (f failIndex) Upsert(sessions.Row) error { return f.err }

// startedTwinEngine is an engine over a Stub, started, with opts.
func startedTwinEngine(t *testing.T, opts engine.Options) *engine.Engine {
	t.Helper()
	e, err := engine.New(tui.NewStubNoPrimary(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e
}

// indexWriteTwin is a real ErrIndexWrite: a rename whose index write failed
// (the engine's own indexWriteError, which errors.Unwrap reaches the store's
// error through).
func indexWriteTwin(t *testing.T) error {
	t.Helper()
	e := startedTwinEngine(t, engine.Options{Index: engine.IndexOptions{
		Store: failIndex{errors.New("craze: not saving the session: permission denied")}, CWD: "/w", Provider: "cursor"}})
	err := e.SetTitle(engine.Command{Client: e.NewClientID(), ID: "1"}, "a better name")
	if !errors.Is(err, engine.ErrIndexWrite) || errors.Unwrap(err) == nil {
		t.Fatalf("the premise: a failed index write came back as %v", err)
	}
	return err
}

// notRunTwin is a real errNotRun (unexported): a Set the engine answered unrun
// because its context was already past its deadline — the host's own bound's
// way of ending (control's commandCtx is a timeout).
func notRunTwin(t *testing.T) error {
	t.Helper()
	e := startedTwinEngine(t, engine.Options{})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := e.Set(ctx, engine.Command{}, engine.Setting{Kind: engine.SettingModel, Value: "fast"})
	if engine.Reason(err) != string(protocol.ReasonNotRun) {
		t.Fatalf("the premise: a Set past its deadline came back as %v (%s)", err, engine.Reason(err))
	}
	return err
}

// engineTwins is every sentinel classify names, as the engine answers it — the
// context's own errors as a host's command bound ends them: a deadline, never
// a cancellation (control's commandCtx is cancelled only once its call has
// returned).
func engineTwins(t *testing.T) []twin {
	return []twin{
		{"engine.ErrIndexWrite", "a rename whose index write failed", indexWriteTwin(t)},
		{"engine.ErrNotAccepting", "", engine.ErrNotAccepting},
		{"agent.ErrNotInTurn", "", agent.ErrNotInTurn},
		{"engine.ErrCommandInProgress", "", engine.ErrCommandInProgress},
		{"engine.ErrAlreadyPending", "", engine.ErrAlreadyPending},
		{"engine.ErrStaleTurn", "", engine.ErrStaleTurn},
		{"engine.ErrStaleVersion", "", engine.ErrStaleVersion},
		{"engine.ErrUnknownRow", "", engine.ErrUnknownRow},
		{"agent.ErrBadAnswer", "", agent.ErrBadAnswer},
		{"agent.ErrAlreadyResolved", "", agent.ErrAlreadyResolved},
		{"agent.ErrUnknownAsk", "", agent.ErrUnknownAsk},
		{"agent.ErrNoSuchSubagent", "", agent.ErrNoSuchSubagent},
		{"engine.ErrCommandAborted", "", engine.ErrCommandAborted},
		{"engine.ErrSetOutcomeUnknown", "a Set claimed when the host's bound ran out (settings.go's setOutcomeUnknown)",
			fmt.Errorf("%w: %w", engine.ErrSetOutcomeUnknown, context.DeadlineExceeded)},
		{"engine.errNotRun", "a Set its dead context answered unrun", notRunTwin(t)},
		{"engine.ErrStaleModel", "", engine.ErrStaleModel},
		{"agent.ErrOptionGone", "", agent.ErrOptionGone},
		{"agent.ErrBadCatalog", "wrapped as the live session returns it",
			fmt.Errorf("session/set_config_option: %w: member 0 is not an option", agent.ErrBadCatalog)},
		{"context.DeadlineExceeded", "a command that ran and gave up on the host's bound",
			fmt.Errorf("writing session/prompt: %w", context.DeadlineExceeded)},
		{"agent.ErrAskUnavailable", "", agent.ErrAskUnavailable},
		{"agent.ErrSetUnavailable", "", agent.ErrSetUnavailable},
		{"engine.ErrUnavailable", "", engine.ErrUnavailable},
		{"engine.ErrClosing", "", engine.ErrClosing},
		{"engine.ErrAttachRaced", "", fmt.Errorf("%w: 4 snapshots refused", engine.ErrAttachRaced)},
		{"engine.ErrBadRequest", "", engine.ErrBadRequest},
		{"engine.ErrUnknownCommand", "", engine.ErrUnknownCommand},
		{"agent.ErrQueueFull", "", agent.ErrQueueFull},
		{"agent.ErrQueueTextTooLong", "", agent.ErrQueueTextTooLong},
		{"agent.ErrPromptInFlight", "", agent.ErrPromptInFlight},
		{"agent.ErrForeignTurn", "", agent.ErrForeignTurn},
		{"agent.ErrPromptCancelled", "", agent.ErrPromptCancelled},
		{"agent.ErrUnsupported", "", agent.ErrUnsupported},
		{"(default)", "a failure classify names no sentinel for: a provider's refusal", errors.New("the agent refused the mode")},
	}
}

// neverOnTheWire is every sentinel classify names that no host's reply can
// carry, and why: TestEveryReasonReconstructsItsSentinel's guard holds the
// twins and this list to classify's source together.
var neverOnTheWire = map[string]string{
	"engine.ErrUnknownClient": "ClaimClient's answer, never a command's: the server answers a failed claim resumed: false (§3.6)",
	"context.Canceled":        "a host's command context is a timeout cancelled only once its call has returned (control's commandCtx): its end is a deadline",
}

// matchSet is every sentinel a caller of the Backend might match: every one
// classify names, the snapshot codec's, and the backend's own two. A
// reconstructed error answers errors.Is for each exactly as its twin does.
func matchSet() map[string]error {
	return map[string]error{
		"engine.ErrIndexWrite": engine.ErrIndexWrite, "engine.ErrNotAccepting": engine.ErrNotAccepting,
		"agent.ErrNotInTurn": agent.ErrNotInTurn, "engine.ErrCommandInProgress": engine.ErrCommandInProgress,
		"engine.ErrAlreadyPending": engine.ErrAlreadyPending, "engine.ErrStaleTurn": engine.ErrStaleTurn,
		"engine.ErrStaleVersion": engine.ErrStaleVersion, "engine.ErrUnknownRow": engine.ErrUnknownRow,
		"agent.ErrBadAnswer": agent.ErrBadAnswer, "agent.ErrAlreadyResolved": agent.ErrAlreadyResolved,
		"agent.ErrUnknownAsk": agent.ErrUnknownAsk, "agent.ErrNoSuchSubagent": agent.ErrNoSuchSubagent,
		"engine.ErrCommandAborted": engine.ErrCommandAborted, "engine.ErrSetOutcomeUnknown": engine.ErrSetOutcomeUnknown,
		"engine.ErrStaleModel": engine.ErrStaleModel, "agent.ErrOptionGone": agent.ErrOptionGone,
		"agent.ErrBadCatalog": agent.ErrBadCatalog, "context.Canceled": context.Canceled,
		"context.DeadlineExceeded": context.DeadlineExceeded, "agent.ErrAskUnavailable": agent.ErrAskUnavailable,
		"agent.ErrSetUnavailable": agent.ErrSetUnavailable, "engine.ErrUnavailable": engine.ErrUnavailable,
		"engine.ErrClosing":     engine.ErrClosing,
		"engine.ErrAttachRaced": engine.ErrAttachRaced, "engine.ErrBadRequest": engine.ErrBadRequest,
		"engine.ErrUnknownClient": engine.ErrUnknownClient, "engine.ErrUnknownCommand": engine.ErrUnknownCommand,
		"agent.ErrQueueFull": agent.ErrQueueFull, "agent.ErrQueueTextTooLong": agent.ErrQueueTextTooLong,
		"agent.ErrPromptInFlight": agent.ErrPromptInFlight, "agent.ErrForeignTurn": agent.ErrForeignTurn,
		"agent.ErrPromptCancelled": agent.ErrPromptCancelled, "agent.ErrUnsupported": agent.ErrUnsupported,
		"transcript.ErrSnapshotTooLarge": transcript.ErrSnapshotTooLarge,
		"backend.ErrOutcomeUnknown":      backend.ErrOutcomeUnknown, "backend.ErrStaleEpoch": backend.ErrStaleEpoch,
		"backend.ErrStopUnsupported": backend.ErrStopUnsupported,
		"tui.ErrNoAnswer":            tui.ErrNoAnswer,
	}
}

// hostError is err as the host puts it on the wire (control/errors.go's
// engineError): -32000, data.code engine.Code(err), data.reason
// engine.Reason(err), the message err.Error() verbatim, and ErrIndexWrite's
// wrapped failure in data.cause.
func hostError(err error) *protocol.Error {
	pe := &protocol.Error{Code: protocol.RPCRefused, Message: err.Error(), Data: protocol.ErrorData{
		Code: protocol.Code(engine.Code(err)), Reason: protocol.Reason(engine.Reason(err))}}
	if errors.Is(err, engine.ErrIndexWrite) {
		if cause := errors.Unwrap(err); cause != nil {
			pe.Data.Cause = cause.Error()
		}
	}
	return pe
}

// overTheWire is pe as a client reads it: a reply line, encoded and decoded,
// through the one construction every refusal goes through.
func overTheWire(t *testing.T, pe *protocol.Error) *remote.Error {
	t.Helper()
	b, err := protocol.MarshalLine(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: json.RawMessage(`1`), Error: pe})
	if err != nil {
		t.Fatal(err)
	}
	var resp protocol.Response
	if err := json.Unmarshal(b, &resp); err != nil || resp.Error == nil {
		t.Fatalf("the reply line %s: %v", b, err)
	}
	return remote.ErrorFromWire(resp.Error)
}

// reconstructed is twin's error as a client reconstructs it off the host's
// reply.
func reconstructed(t *testing.T, err error) *remote.Error {
	t.Helper()
	return overTheWire(t, hostError(err))
}

// TestEveryReasonReconstructsItsSentinel (§3.14, A16): for every sentinel
// classify names, the host's (code, reason) — engine.Code and engine.Reason,
// the one table — reconstructs an error that errors.Is exactly the sentinels
// the engine's own error is, over every sentinel a caller might match (so it
// is its own, and matches no other the in-process one does not: not_in_turn is
// agent.ErrNotInTurn and NOT engine.ErrNotAccepting, as in process, where
// Interject returns the session's refusal unwrapped); whose Error is the
// host's message verbatim; and whose Unwrap reaches ErrIndexWrite's cause. A
// guard holds the twins to classify's own source: a sentinel classify names
// with no twin (and not listed as never on the wire) fails, and so does an
// engine reason of §3.2's table no twin produces. The client-side reasons are
// never produced by the engine's table, and reconstruct nothing.
func TestEveryReasonReconstructsItsSentinel(t *testing.T) {
	twins := engineTwins(t)
	set := matchSet()
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range set {
			if !yield(k) {
				return
			}
		}
	})
	produced := map[protocol.Reason]bool{}
	for _, tw := range twins {
		reason := protocol.Reason(engine.Reason(tw.err))
		produced[reason] = true
		info, ok := reason.Lookup()
		switch {
		case !ok:
			t.Errorf("%s: engine.Reason %q is not in §3.2's table", tw.sentinel, reason)
			continue
		case info.ClientSide:
			t.Errorf("%s: engine.Reason %q is a client-side reason: no host may send it", tw.sentinel, reason)
		case string(info.Code) != engine.Code(tw.err):
			t.Errorf("%s: reason %q belongs to code %q, the engine says %q", tw.sentinel, reason, info.Code, engine.Code(tw.err))
		}
		got := reconstructed(t, tw.err)
		if got.Code != protocol.Code(engine.Code(tw.err)) || got.Reason != reason {
			t.Errorf("%s: read back as %s/%s", tw.sentinel, got.Code, got.Reason)
		}
		if got.Error() != tw.err.Error() {
			t.Errorf("%s: the message is %q over the wire, %q in process", tw.sentinel, got.Error(), tw.err.Error())
		}
		for _, name := range names {
			s := set[name]
			if in, over := errors.Is(tw.err, s), errors.Is(got, s); in != over {
				t.Errorf("%s (%s/%s): errors.Is(_, %s) is %v over the wire and %v in process: the reason table (sentinels.go) is missing or has a wrong row",
					tw.sentinel, got.Code, got.Reason, name, over, in)
			}
		}
		if errors.Is(tw.err, engine.ErrIndexWrite) {
			if c := errors.Unwrap(got); c == nil || c.Error() != errors.Unwrap(tw.err).Error() {
				t.Errorf("%s: errors.Unwrap over the wire is %v, in process %q", tw.sentinel, c, errors.Unwrap(tw.err))
			}
		}
	}
	// Every engine reason of §3.2's table has a twin: the table's rows and
	// classify's are one.
	for _, info := range protocol.Reasons() {
		if info.Engine && !produced[info.Reason] {
			t.Errorf("the engine reason %q (code %s) has no twin here: add classify's sentinel for it", info.Reason, info.Code)
		}
		if info.ClientSide {
			if s := remote.Sentinels(info.Code, info.Reason); len(s) != 0 {
				t.Errorf("the client-side reason %q reconstructs %v: no host sends it", info.Reason, s)
			}
			if produced[info.Reason] {
				t.Errorf("the client-side reason %q came out of engine.Reason", info.Reason)
			}
		}
	}
	// Every sentinel classify's own source names is a twin's, or never on the
	// wire, with why; is in the match set every comparison above runs over;
	// and — unless it is never on the wire — is one the production table
	// (sentinels.go) reconstructs from some reason. So a sentinel classify
	// gains cannot pass here unmatched by leaving it out of both the match
	// set and the table (astra r61 6).
	have := map[string]bool{}
	for _, tw := range twins {
		have[tw.sentinel] = true
	}
	for _, name := range classifySentinels(t) {
		if !have[name] && neverOnTheWire[name] == "" {
			t.Errorf("classify names %s, which has no twin here and is not listed as never on the wire", name)
		}
		if name == "engine.errNotRun" {
			// Unexported, so neither set can hold it: its twin's reason
			// reconstructs what it wraps (the comparison above).
			continue
		}
		v, ok := set[name]
		switch {
		case !ok:
			t.Errorf("classify names %s, which the match set (matchSet) does not hold: no comparison checks it", name)
		case neverOnTheWire[name] != "":
		case !reconstructedByTable(v):
			t.Errorf("classify names %s, which the production table (sentinels.go) reconstructs from no reason", name)
		}
	}
}

// reconstructedByTable says the production table answers target for some
// reason a host sends.
func reconstructedByTable(target error) bool {
	for _, info := range protocol.Reasons() {
		if slices.Contains(remote.Sentinels(info.Code, info.Reason), target) {
			return true
		}
	}
	return false
}

// classifySentinels is every sentinel classify (engine/control.go) matches by
// errors.Is, as its source spells it — engine's own names qualified
// "engine.".
func classifySentinels(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "engine", "control.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "classify" {
			continue
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isCall(call, "errors", "Is") {
				return true
			}
			name := exprString(fset, call.Args[1])
			if !strings.Contains(name, ".") {
				name = "engine." + name
			}
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
			return true
		})
	}
	if len(out) < 30 {
		t.Fatalf("the premise: classify matches %d sentinels, which is not the table: %v", len(out), out)
	}
	return out
}

func isCall(call *ast.CallExpr, pkg, fn string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != fn {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	_ = printer.Fprint(&b, fset, e)
	return b.String()
}

// TestTheProtocolsReasonsReconstructTheirSentinel (§3.2): a reason of the
// protocol's own reconstructs the one sentinel an engine error stands behind —
// start_failed is a gate refusal (engine.ErrNotAccepting, refusalLocked's for a
// failed start), snapshot_too_large transcript.ErrSnapshotTooLarge, and
// stop_unsupported the backend's own backend.ErrStopUnsupported (plan 030
// §3.6a) — and every other none, the hub's reasons (plan 032 §3.15) among
// them; a reason sent under another code than the table's, or one this
// build does not know, reconstructs nothing.
func TestTheProtocolsReasonsReconstructTheirSentinel(t *testing.T) {
	want := map[protocol.Reason][]string{
		protocol.ReasonStartFailed:      {"engine.ErrNotAccepting"},
		protocol.ReasonSnapshotTooLarge: {"transcript.ErrSnapshotTooLarge"},
		protocol.ReasonStopUnsupported:  {"backend.ErrStopUnsupported"},
	}
	set := matchSet()
	held := map[protocol.Reason]bool{}
	for _, info := range protocol.Reasons() {
		if info.Engine || info.ClientSide {
			continue
		}
		held[info.Reason] = true
		got := overTheWire(t, &protocol.Error{Code: protocol.RPCRefused, Message: "m",
			Data: protocol.ErrorData{Code: info.Code, Reason: info.Reason}})
		var matched []string
		for name, s := range set {
			if errors.Is(got, s) {
				matched = append(matched, name)
			}
		}
		sort.Strings(matched)
		if !slices.Equal(matched, want[info.Reason]) {
			t.Errorf("%s/%s reconstructs %v, want %v", info.Code, info.Reason, matched, want[info.Reason])
		}
	}
	// The hub's reasons are the protocol's own, so the loop above held each
	// of them to reconstructing nothing.
	for _, r := range []protocol.Reason{protocol.ReasonHostOnly, protocol.ReasonConnectNotFirst, protocol.ReasonAmbiguousSession,
		protocol.ReasonAlreadySubscribed, protocol.ReasonRequestConflict, protocol.ReasonSpawnFailed, protocol.ReasonHostUnreachable} {
		if !held[r] {
			t.Errorf("the hub's reason %s is not one of the protocol's own the table lists", r)
		}
	}
	for _, e := range []*protocol.Error{
		{Code: protocol.RPCRefused, Message: "m", Data: protocol.ErrorData{Code: protocol.CodeFailed, Reason: protocol.ReasonQueueFull}},
		{Code: protocol.RPCRefused, Message: "m", Data: protocol.ErrorData{Code: "a_later_code", Reason: "a_later_reason"}},
		{Code: protocol.RPCRefused, Message: "m", Data: protocol.ErrorData{Code: protocol.CodeQueueFull}},
	} {
		if s := remote.Sentinels(e.Data.Code, e.Data.Reason); len(s) != 0 {
			t.Errorf("%s/%s reconstructs %v, want nothing", e.Data.Code, e.Data.Reason, s)
		}
	}
}

// TestOnlyTheClientsOwnOutcomesAreOutcomeUnknown (the C26 addendum): every
// *OutcomeUnknownError the client makes — resume_lost, disconnected, and the
// stale epoch — is backend.ErrOutcomeUnknown (remote's is the same value), and
// no host's refusal ever is: whatever a host sent, the command's outcome is
// its answer. The stale one is backend.ErrStaleEpoch too. (The schedules that
// produce each over a real host are the Session tests': a resume loss, a Close
// with a command waiting, a stale epoch.)
func TestOnlyTheClientsOwnOutcomesAreOutcomeUnknown(t *testing.T) {
	if remote.ErrOutcomeUnknown != backend.ErrOutcomeUnknown {
		t.Fatal("remote.ErrOutcomeUnknown is not backend.ErrOutcomeUnknown")
	}
	for _, e := range []*remote.OutcomeUnknownError{
		{Method: protocol.MethodSessionPrompt, CommandID: "1", Reason: protocol.ReasonResumeLost},
		{Method: protocol.MethodSessionPrompt, CommandID: "1", Reason: protocol.ReasonDisconnected},
		{Method: protocol.MethodSessionPrompt, CommandID: "1", Reason: protocol.ReasonResumeLost, Err: backend.ErrStaleEpoch},
	} {
		var err error = e
		if !errors.Is(err, backend.ErrOutcomeUnknown) || !errors.Is(err, remote.ErrOutcomeUnknown) || !e.Reason.ClientSide() {
			t.Errorf("%v: not backend.ErrOutcomeUnknown", err)
		}
		if stale := e.Err != nil; errors.Is(err, backend.ErrStaleEpoch) != stale {
			t.Errorf("%v: errors.Is(_, backend.ErrStaleEpoch) is %v", err, !stale)
		}
	}
	for _, info := range protocol.Reasons() {
		if info.ClientSide {
			continue
		}
		got := overTheWire(t, &protocol.Error{Code: protocol.RPCRefused, Message: "m",
			Data: protocol.ErrorData{Code: info.Code, Reason: info.Reason}})
		if errors.Is(got, backend.ErrOutcomeUnknown) || errors.Is(got, backend.ErrStaleEpoch) {
			t.Errorf("the host's %s/%s is an outcome unknown", info.Code, info.Reason)
		}
	}
}

// TestEveryWireCapabilityComesBack (§3.3, §3.13): the session capability set
// on the wire comes back as agent.Capabilities one to one — every
// agent.Capabilities field (found by reflection, so a field a later phase adds
// fails here until it is mapped back) from exactly one wire field, and every
// wire field but the four the protocol states for every host (cancel,
// approvals, historyCursor, stop) and the host's rowFacts (plan 030 §3.8: what
// its sessions.list row carries, no concern of a session's client) and
// presence (plan 032 §3.14: the host's count, which reaches a client on its
// stream, not in its capabilities) to exactly one agent field.
func TestEveryWireCapabilityComesBack(t *testing.T) {
	protocolOwn := map[string]bool{"Cancel": true, "Approvals": true, "HistoryCursor": true, "Stop": true, "RowFacts": true, "Presence": true}
	wt := reflect.TypeFor[protocol.SessionCapabilities]()
	at := reflect.TypeFor[agent.Capabilities]()
	from := map[string]string{} // agent field → wire field
	for i := range wt.NumField() {
		var c protocol.SessionCapabilities
		reflect.ValueOf(&c).Elem().Field(i).SetBool(true)
		got := reflect.ValueOf(remote.CapabilitiesFromWire(c))
		var on []string
		for j := range at.NumField() {
			if got.Field(j).Bool() {
				on = append(on, at.Field(j).Name)
			}
		}
		name := wt.Field(i).Name
		switch {
		case protocolOwn[name] && len(on) != 0:
			t.Errorf("the protocol's own %s sets %v", name, on)
		case !protocolOwn[name] && len(on) != 1:
			t.Errorf("the wire's %s sets %v, want exactly one agent field", name, on)
		case !protocolOwn[name]:
			if prev, dup := from[on[0]]; dup {
				t.Errorf("agent.Capabilities.%s comes from both %s and %s", on[0], prev, name)
			}
			from[on[0]] = name
		}
	}
	for j := range at.NumField() {
		if from[at.Field(j).Name] == "" {
			t.Errorf("agent.Capabilities.%s comes from no wire field: map it back in capabilities (session.go)", at.Field(j).Name)
		}
	}
}

// tuiSite is one place internal/tui's production code reads an error (plan
// 027 §3.14, A16): its file, its function, how it reads it — errors.Is,
// errors.As, errors.Unwrap or .Error() — and what, as the source spells it;
// n is how many times that function reads it so. proof is how the read works
// on a reconstructed error, or why no Backend error reaches it.
type tuiSite struct {
	file, fn, how, what string
	n                   int
	proof               string
}

// The proofs a site can have.
const (
	// proofSentinel: errors.Is of a sentinel the host's twin is: the
	// reconstructed error matches it (checked here, per site).
	proofSentinel = "sentinel"
	// proofText: .Error() of an error a Backend call answers: the host's
	// message verbatim (TestEveryReasonReconstructsItsSentinel, every twin),
	// or a *StartError's text.
	proofText = "text"
	// proofCause: errors.Unwrap of ErrIndexWrite, and .Error() of what it
	// reaches: the cause's text (checked here).
	proofCause = "cause"
	// proofDeadline: errors.Is(err, ctx.Err()) once the gate's ctx is past its
	// deadline — or errors.Is(err, context.DeadlineExceeded), a dispatch's
	// prompt's (plan 030 C15r2): context.DeadlineExceeded, which a remote
	// command answers itself when that ctx ends, and which the host's bound's
	// reasons reconstruct (checked here).
	proofDeadline = "deadline"
	// proofGate: tui.ErrNoAnswer, the gate's own, which no reconstructed
	// error ever is (checked here, over every twin).
	proofGate = "gate"
	// proofViewClose: errors.Is(Close(), agent.ErrAgentExited): a Session's
	// Close is a view close and never answers it (TestAViewCloseLeavesTheSession).
	proofViewClose = "view close"
	// proofCodec: an event's own error, which the lossless event codec
	// carries in the event body, verbatim over the socket (no *Error).
	proofCodec = "event codec"
	// proofLocal: an error of the TUI's own — a file, a process, a dialog, a
	// config — that no Backend call answers.
	proofLocal = "local"
	// proofOutcome: errors.Is of backend.ErrOutcomeUnknown, which every
	// outcome-unknown answer the client makes itself is, and no host's
	// refusal ever is (checked here, over every twin; the client's own are
	// TestOnlyTheClientsOwnOutcomesAreOutcomeUnknown's too).
	proofOutcome = "outcome"
	// proofStop: errors.Is of backend.ErrStopUnsupported, which a host's
	// stop_unsupported refusal of session.stop reconstructs (plan 030 §3.6a;
	// checked here) — the explicit quit's reading of a host that cannot stop
	// its session.
	proofStop = "stop"
)

// tuiSites is every error read in internal/tui's production files
// (TestEveryTUIErrorSiteWorksOverTheWire scans for them), with its proof.
var tuiSites = []tuiSite{
	{"app.go", "runErrAfterHangup", "Is", "tea.ErrProgramPanic", 1, proofLocal},
	{"app.go", "finishRun", "Is", "agent.ErrAgentExited", 1, proofViewClose},
	// SaveProvider's, in the tail startedMsg's arm shares (plan 030 C7r2).
	{"app.go", "comeUp", "Error", "err", 1, proofLocal},
	// errMsg (Start's failure).
	{"app.go", "update", "Error", "msg.err", 1, proofText},
	// That failure's row drawn again after a restore (plan 030 C7r): the
	// error errMsg recorded (startErr) — a *StartError's text over the
	// socket. spawnFailed's own never meets a restore: it adopted no
	// backend, so no stream follows it.
	{"restore.go", "applyRestore", "Error", "m.startErr", 1, proofText},
	{"app.go", "settlePending", "Is", "ErrNoAnswer", 1, proofGate},
	{"app.go", "submitErrNote", "Is", "ErrNoAnswer", 1, proofGate},
	{"app.go", "submitErrNote", "Is", "engine.ErrNotAccepting", 1, proofSentinel},
	{"app.go", "submitErrNote", "Is", "engine.ErrAlreadyPending", 1, proofSentinel},
	{"app.go", "cancelTurn", "Is", "engine.ErrStaleTurn", 1, proofSentinel},
	{"app.go", "cancelForeignTurn", "Is", "engine.ErrNotAccepting", 1, proofSentinel},
	{"app.go", "cancelForeignTurn", "Is", "engine.ErrStaleTurn", 1, proofSentinel},
	{"app.go", "reduceEvent", "Error", "ev.Err", 1, proofCodec},
	{"app.go", "answerHidden", "Is", "agent.ErrAskUnavailable", 1, proofSentinel},
	{"app.go", "indexWriteText", "Unwrap", "err", 1, proofCause},
	{"app.go", "indexWriteText", "Error", "cause", 1, proofCause},
	{"app.go", "indexWriteText", "Error", "err", 1, proofText},
	{"cards.go", "answered", "Is", "ErrNoAnswer", 1, proofGate},
	{"cards.go", "answered", "Is", "agent.ErrAlreadyResolved", 1, proofSentinel},
	{"cards.go", "answered", "Is", "agent.ErrBadAnswer", 1, proofSentinel},
	{"cards.go", "answered", "Is", "agent.ErrAskUnavailable", 1, proofSentinel},
	{"cards.go", "answered", "Error", "err", 1, proofText},
	{"config.go", "readConfigAt", "Is", "fs.ErrNotExist", 1, proofLocal},
	{"config.go", "ConfigJournal", "Is", "ErrConfigMalformed", 1, proofLocal},
	{"config.go", "ConfigControlSocket", "Is", "ErrConfigMalformed", 1, proofLocal},
	{"config.go", "ConfigDetach", "Is", "ErrConfigMalformed", 1, proofLocal},
	{"config.go", "ConfigHostIdleExit", "Is", "ErrConfigMalformed", 1, proofLocal},
	// /connect's (plan 031 §3.9): the TUI's own key store, read and written
	// in this process through Config.NativeDir — never a Backend call's
	// answer — and the rule a key is judged by before it is stored.
	{"connect_dialog.go", "storeErrText", "Error", "err", 1, proofLocal},
	{"connect_dialog.go", "storedProblemText", "Is", "modeltable.ErrKeyTooShort", 1, proofLocal},
	{"connect_dialog.go", "connectKeyRule", "Is", "modeltable.ErrKeyTooShort", 1, proofLocal},
	// The composer's images (plan 033 §3.3): a pasted file and the
	// clipboard's image, read, processed and stored in this process — never
	// a Backend call's answer.
	{"composer_image.go", "attachErrReason", "Is", "fs.ErrNotExist", 1, proofLocal},
	{"composer_image.go", "attachErrReason", "Error", "err", 1, proofLocal},
	{"clipboard_image.go", "readImageFrom", "Is", "attach.ErrSourceTooLarge", 1, proofLocal},
	// The clipboard's text read (plan 033 C3r): no tool could list the
	// clipboard's types, in this process.
	{"clipboard_text.go", "readTextVia", "Is", "errNoTextBackend", 1, proofLocal},
	// The frame harness's capture boundary: in process only (engineBehind).
	{"frame.go", "streamHead", "Is", "agent.ErrLogClosing", 1, proofLocal},
	{"frame.go", "streamHead", "Is", "agent.ErrClosed", 1, proofLocal},
	{"frame.go", "streamHead", "Is", "agent.ErrFlushGaveUp", 1, proofLocal},
	{"gate.go", "unanswered", "Is", "ctx.Err()", 1, proofDeadline},
	// The launch flow's spawns (plan 030 §3.5): the answer of a Config
	// closure (NewBackend, LoadBackend) — the launcher's own, about a host it
	// spawned or found — which no Backend call answers.
	{"launch.go", "Error", "Error", "r.Err", 1, proofLocal},
	{"launch.go", "spawned", "As", "&refused", 1, proofLocal},
	{"launch.go", "repick", "Error", "r", 1, proofLocal},
	{"launch.go", "spawnFailed", "Error", "err", 1, proofLocal},
	// A gated reply's error, and a command's or a chain's failure as the
	// reducer words it (failureText): ErrNoAnswer for the client's own
	// outcome-unknown answers, the host's error unchanged otherwise.
	{"gate.go", "noAnswerFor", "Is", "ErrNoAnswer", 1, proofGate},
	{"gate.go", "noAnswerFor", "Is", "backend.ErrOutcomeUnknown", 1, proofOutcome},
	{"gate.go", "Error", "Error", "ErrNoAnswer", 1, proofLocal},
	// actionErrMsg (a Set or Settings refusal), revertModeMsg ×2 and
	// revertModelMsg (a Set's), modelApplyMsg (a step's), cancelFailedMsg (a
	// Cancel's): the host's text, or ErrNoAnswer's for an outcome unknown.
	{"gate.go", "failureText", "Error", "noAnswerFor(err)", 1, proofText},
	// A command the close fence refused (plan 030 §3.6), worded as the
	// TUI's own refusals are (closingNote, C6).
	{"gate.go", "failureText", "Is", "engine.ErrClosing", 1, proofSentinel},
	{"model_dialog.go", "runModelApply", "Is", "agent.ErrBadCatalog", 1, proofSentinel},
	{"model_dialog.go", "runModelApply", "Is", "engine.ErrStaleModel", 1, proofSentinel},
	{"model_dialog.go", "runModelApply", "Is", "agent.ErrOptionGone", 1, proofSentinel},
	{"provider_dialog.go", "confirmProvider", "Error", "err", 1, proofLocal},
	{"queue.go", "queueErrNote", "Is", "agent.ErrQueueFull", 1, proofSentinel},
	{"queue.go", "queueErrNote", "Is", "agent.ErrQueueTextTooLong", 1, proofSentinel},
	{"queue.go", "queueErrNote", "Is", "engine.ErrClosing", 1, proofSentinel},
	{"queue.go", "queueErrNote", "Error", "err", 1, proofText},
	{"queue.go", "interjectErrNote", "Is", "ErrNoAnswer", 1, proofGate},
	{"queue.go", "interjectErrNote", "Is", "agent.ErrNotInTurn", 1, proofSentinel},
	{"queue.go", "interjectErrNote", "Is", "agent.ErrUnsupported", 1, proofSentinel},
	{"queue.go", "interjectErrNote", "Is", "agent.ErrQueueFull", 1, proofSentinel},
	{"queue.go", "withdrawn", "Is", "ErrNoAnswer", 1, proofGate},
	{"queue.go", "saveQueueEdit", "Is", "ErrNoAnswer", 2, proofGate},
	{"queue.go", "saveQueueEdit", "Is", "engine.ErrStaleVersion", 1, proofSentinel},
	{"queue.go", "dropQueuedRow", "Is", "ErrNoAnswer", 1, proofGate},
	{"resume_dialog.go", "chooseResume", "Error", "err", 1, proofLocal},
	{"resume_dialog.go", "resumeClaimed", "Error", "msg.err", 1, proofLocal},
	{"shared.go", "errorText", "Error", "err", 1, proofCodec},
	{"shell.go", "shellSuffix", "Error", "s.start", 1, proofLocal},
	{"shell_run.go", "shellExitCode", "Is", "exec.ErrWaitDelay", 1, proofLocal},
	{"shell_run.go", "shellExitCode", "As", "&ee", 1, proofLocal},
	{"slash.go", "runBuiltin", "Is", "ErrNoAnswer", 1, proofGate},
	{"slash.go", "runBuiltin", "Is", "engine.ErrIndexWrite", 1, proofSentinel},
	{"slash.go", "runBuiltin", "Error", "err", 2, proofText}, // SetTitle's refusal; resolveModelArgs' (local, same spelling)
	{"slash.go", "applyModelEffort", "Is", "agent.ErrBadCatalog", 1, proofSentinel},
	{"slash.go", "runModelEffort", "Is", "engine.ErrStaleModel", 1, proofSentinel},
	{"slash.go", "runModelEffort", "Is", "agent.ErrOptionGone", 1, proofSentinel},
	// The explicit quit's stop (plan 030 §3.6): a host that cannot stop its
	// session refuses, stop_unsupported.
	{"stopquit.go", "answered", "Is", "backend.ErrStopUnsupported", 1, proofStop},
	// The session list's close of another session (plan 030 §3.10): its
	// Sessions.Stop is session.stop over a connection of its own, whose
	// refusal is the same reconstruction; and the roster's registry and
	// index reads, the list's own (internal/roster), which no Backend call
	// answers.
	{"sessions_list.go", "sessActionDone", "Is", "backend.ErrStopUnsupported", 1, proofStop},
	{"sessions_list.go", "sessLines", "Error", "err", 2, proofLocal},
	// The list's input (plan 030 §3.13, §3.15): the `@` picker's listing of a
	// directory, read off the Update from the disk, and the leading token's
	// resolution — a name among the list's candidates, a path stat'ed —
	// worded by the list itself; no Backend call answers any of them.
	{"at_dirs.go", "browse", "Is", "fs.ErrNotExist", 1, proofLocal},
	{"at_dirs.go", "browse", "Is", "errNotADirectory", 1, proofLocal},
	{"at_dirs.go", "sessErrText", "As", "&pe", 1, proofLocal},
	{"at_dirs.go", "sessErrText", "Error", "err", 1, proofLocal},
	{"at_dirs.go", "sessListDirs", "Is", "io.EOF", 1, proofLocal},
	{"sessions_input.go", "sessTargetRule", "Error", "t.err", 1, proofLocal},
	{"sessions_input.go", "sessInputEnter", "Error", "err", 1, proofLocal},
	// The composer's `@` file search (plan 030 §3.16, C17): a listing of the
	// workspace read off the Update from rg, git or a walk — its records,
	// the tool's exit, and its own timeout, worded for the popup's note; no
	// Backend call answers any of them.
	{"at_files.go", "atFilesErrNote", "Is", "errAtFilesTimeout", 1, proofLocal},
	{"at_files.go", "atExitCode", "As", "&ee", 1, proofLocal},
	{"at_files.go", "readPaths", "Is", "bufio.ErrBufferFull", 1, proofLocal},
	{"at_files.go", "readPaths", "Is", "io.EOF", 1, proofLocal},
	// A background dispatch (plan 030 §3.13, C15): its start given up at its
	// own bound, the dispatch's context.
	{"dispatch.go", "runDispatch", "Is", "context.DeadlineExceeded", 1, proofLocal},
	// Its prompt's answer read as the session's own — accepted or refused —
	// unless it is no answer (promptUnknown, C15r2): the call's time ran out —
	// the deadline, which a remote command answers itself when its ctx ends
	// and a host's own bound's reasons reconstruct; context.Canceled, only
	// ever the dispatch's own ctx's, which no host's reply carries
	// (neverOnTheWire) — or the client could not learn the outcome (its
	// connection went after sending, or closed under it), which it says
	// itself and no host's refusal ever is.
	{"dispatch.go", "promptUnknown", "Is", "context.DeadlineExceeded", 1, proofDeadline},
	{"dispatch.go", "promptUnknown", "Is", "context.Canceled", 1, proofLocal},
	{"dispatch.go", "promptUnknown", "Is", "backend.ErrOutcomeUnknown", 1, proofOutcome},
	// /provider and /model (plan 030 §3.14, C16): native's default model read
	// from its model table off the Update (plan 031 §3.5: StartModel, and
	// nothing funded is ErrNothingFunded) — its key judged against the TUI's
	// environment — and the hint line's word for one that could not be
	// read; the disk's answers, never a Backend's.
	{"sessions_models.go", "nativeDefaultModel", "Is", "modeltable.ErrNothingFunded", 1, proofLocal},
	{"sessions_models.go", "sessNativeDefault", "Error", "msg.err", 1, proofLocal},
	{"theme.go", "noteAndSaveTheme", "Error", "err", 1, proofLocal},
}

// scanTUI is every error read in internal/tui's production files, keyed as
// tuiSites is, with its count.
func scanTUI(t *testing.T) map[[4]string]int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "tui", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[[4]string]int{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Base(path)
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch {
				case isCall(call, "errors", "Is"):
					out[[4]string{file, fd.Name.Name, "Is", exprString(fset, call.Args[1])}]++
				case isCall(call, "errors", "As"):
					out[[4]string{file, fd.Name.Name, "As", exprString(fset, call.Args[1])}]++
				case isCall(call, "errors", "Unwrap"):
					out[[4]string{file, fd.Name.Name, "Unwrap", exprString(fset, call.Args[0])}]++
				default:
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Error" && len(call.Args) == 0 {
						out[[4]string{file, fd.Name.Name, "Error", exprString(fset, sel.X)}]++
					}
				}
				return true
			})
		}
	}
	return out
}

// TestEveryTUIErrorSiteWorksOverTheWire (§3.14, A16): every place
// internal/tui's production code reads an error — found by scanning its
// source, so a site added later fails here until it is proven — works on the
// error a Session answers: a sentinel match holds for the reconstruction of
// that sentinel's host error, as it does in process; ErrIndexWrite's cause is
// reached by errors.Unwrap with its text; the gate's deadline match holds for
// what a remote command answers when the gate's ctx ends; no reconstructed
// error is ever the gate's own ErrNoAnswer; and the rest are the TUI's own.
func TestEveryTUIErrorSiteWorksOverTheWire(t *testing.T) {
	found := scanTUI(t)
	listed := map[[4]string]bool{}
	for _, s := range tuiSites {
		key := [4]string{s.file, s.fn, s.how, s.what}
		listed[key] = true
		if n := found[key]; n != s.n {
			t.Errorf("%s %s: errors.%s/%s(%s) is read %d times, the table says %d", s.file, s.fn, s.how, s.how, s.what, n, s.n)
		}
	}
	for key, n := range found {
		if !listed[key] {
			t.Errorf("%s %s reads an error the table does not prove (%s %s, %d times): add its site and its proof", key[0], key[1], key[2], key[3], n)
		}
	}

	twins := engineTwins(t)
	bySentinel := map[string]twin{}
	for _, tw := range twins {
		bySentinel[tw.sentinel] = tw
	}
	set := matchSet()
	for _, s := range tuiSites {
		switch s.proof {
		case proofSentinel:
			tw, ok := bySentinel[s.what]
			if !ok {
				t.Errorf("%s %s matches %s, which no host error is", s.file, s.fn, s.what)
				continue
			}
			got := reconstructed(t, tw.err)
			if !errors.Is(got, set[s.what]) || got.Error() != tw.err.Error() {
				t.Errorf("%s %s: errors.Is(_, %s) is false over the wire (%s/%s %q)", s.file, s.fn, s.what, got.Code, got.Reason, got)
			}
		case proofCause:
			tw := bySentinel["engine.ErrIndexWrite"]
			got := reconstructed(t, tw.err)
			if c := errors.Unwrap(got); c == nil || c.Error() != errors.Unwrap(tw.err).Error() {
				t.Errorf("%s %s: the cause over the wire is %v, in process %q", s.file, s.fn, c, errors.Unwrap(tw.err))
			}
		case proofDeadline:
			// The gate's ctx past its deadline: a remote command waiting on it
			// answers ctx.Err() itself (Client.Command), and a host's answer of
			// its own bound — the context, not_run and set_outcome_unknown
			// reasons — matches the deadline as the engine's own does when the
			// gate's ctx was the one the engine ran on.
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			for _, name := range []string{"context.DeadlineExceeded", "engine.errNotRun", "engine.ErrSetOutcomeUnknown"} {
				if got := reconstructed(t, bySentinel[name].err); !errors.Is(got, ctx.Err()) {
					t.Errorf("%s %s: the host's %s/%s is not ctx.Err() (%v)", s.file, s.fn, got.Code, got.Reason, ctx.Err())
				}
			}
			cancel()
		case proofGate:
			for _, tw := range twins {
				if errors.Is(reconstructed(t, tw.err), tui.ErrNoAnswer) {
					t.Errorf("%s %s: the host's %s reconstructs the gate's ErrNoAnswer", s.file, s.fn, tw.sentinel)
				}
			}
		case proofStop:
			got := overTheWire(t, &protocol.Error{Code: protocol.RPCRefused, Message: "this host cannot stop its session",
				Data: protocol.ErrorData{Code: protocol.CodeUnsupported, Reason: protocol.ReasonStopUnsupported}})
			if !errors.Is(got, backend.ErrStopUnsupported) {
				t.Errorf("%s %s: a host's stop_unsupported is not backend.ErrStopUnsupported over the wire (%v)", s.file, s.fn, got)
			}
		case proofOutcome:
			for _, tw := range twins {
				if errors.Is(reconstructed(t, tw.err), backend.ErrOutcomeUnknown) {
					t.Errorf("%s %s: the host's %s reconstructs an outcome unknown", s.file, s.fn, tw.sentinel)
				}
			}
			for _, reason := range []protocol.Reason{protocol.ReasonResumeLost, protocol.ReasonDisconnected} {
				if err := error(&remote.OutcomeUnknownError{Method: protocol.MethodSessionPrompt, CommandID: "1", Reason: reason}); !errors.Is(err, backend.ErrOutcomeUnknown) {
					t.Errorf("%s %s: the client's own %s is not an outcome unknown", s.file, s.fn, reason)
				}
			}
		}
	}
	// And the text sites: every twin's message is the host's verbatim (the
	// reconstruction test holds it for each), and a failed start's text is
	// the host's start error's.
	if got := (&remote.StartError{Text: "the agent would not start"}).Error(); got != "the agent would not start" {
		t.Errorf("a start's failure reads %q", got)
	}
}
