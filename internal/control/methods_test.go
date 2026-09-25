package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/transcript"
	"github.com/charliek/craze/internal/version"
)

// TestHelloAnswersWithTheHost (§3.3): the host's hello result — the protocol
// chosen, the endpoint, a fresh client id and 128-bit token, the connection
// capabilities, both codecs, both limits and the retry horizon.
func TestHelloAnswersWithTheHost(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	r := a.sayHello(nil)
	want := protocol.Endpoint{Kind: protocol.EndpointHost, HostID: h.srv.HostID(), CrazeVersion: version.Version, PID: os.Getpid()}
	switch {
	case r.Protocol != protocol.ProtocolVersion, r.Endpoint != want, len(h.srv.HostID()) != 12:
		t.Fatalf("the host: %+v, want %+v", r, want)
	case r.ClientID != "c-1" || len(r.Token) != 32 || r.Resumed:
		t.Fatalf("the client: %+v", r)
	case r.Capabilities != protocol.HostCapabilities(), r.Limits != protocol.HostLimits():
		t.Fatalf("capabilities %+v, limits %+v", r.Capabilities, r.Limits)
	case r.Codecs != protocol.Codecs{Event: agent.EventCodecVersion, Snapshot: transcript.SnapshotVersion}:
		t.Fatalf("codecs %+v", r.Codecs)
	case r.RetryHorizon != protocol.RetryHorizon{Commands: 1024, AgeMs: 600_000}:
		t.Fatalf("retry horizon %+v", r.RetryHorizon)
	}
	b := h.dial()
	if rb := b.sayHello(nil); rb.ClientID == r.ClientID || rb.Token == r.Token {
		t.Fatalf("two connections share a client: %+v, %+v", r, rb)
	}
}

// TestSessionsListIsOneRowAtItsCursor (§3.3, SD-28): a host's roster is its
// one session — the info document with the live roster facts — with epoch the
// hostId and a cursor at or past every event that describes what the row
// shows.
func TestSessionsListIsOneRowAtItsCursor(t *testing.T) {
	h := newHost(t)
	h.stub.SetProvider(agent.GrokProvider())
	a := h.dial()
	a.sayHello(nil)
	ok[protocol.QueueAddResult](t, queueAdd(a, a.cmd(), "later"))
	synced := ok[protocol.SyncResult](t, a.call(protocol.MethodSessionSync, protocol.SyncParams{SessionID: sid(h)}))
	h.stub.Emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell",
		Options: []agent.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: "allow_once"}}}})

	l := ok[protocol.SessionsListResult](t, a.call(protocol.MethodSessionsList, protocol.SessionsListParams{}))
	st := h.eng.State()
	if l.Epoch != h.srv.HostID() || l.Cursor <= synced.Seq || len(l.Sessions) != 1 {
		t.Fatalf("the roster: epoch %q cursor %d (after %d), %d rows", l.Epoch, l.Cursor, synced.Seq, len(l.Sessions))
	}
	row := l.Sessions[0]
	switch {
	case row.SessionID != st.CrazeSessionID, row.Incarnation != st.Incarnation, row.ProviderSessionID != "stub-session-1",
		row.HostID != h.srv.HostID(), row.Workspace != "/work":
		t.Fatalf("the identities: %+v", row.SessionInfo)
	case row.Provider != protocol.Provider{Name: "grok", Label: "grok"}:
		t.Fatalf("the provider: %+v", row.Provider)
	case len(row.Catalogs.Models) != 2 || len(row.Catalogs.Modes) != 3:
		t.Fatalf("the catalogs of a ready session: %+v", row.Catalogs)
	case !row.Capabilities.Interject || !row.Capabilities.Cancel || !row.Capabilities.Approvals ||
		!row.Capabilities.HistoryCursor || row.Capabilities.Stop:
		t.Fatalf("the capabilities: %+v", row.Capabilities)
	case row.Activity != protocol.ActivityIdle || row.ForeignTurn || row.PendingAsks != 1:
		t.Fatalf("the live facts: %+v", row)
	case row.HeadAsk == nil || *row.HeadAsk != protocol.HeadAsk{ID: "perm-1", Kind: "permission", Label: "permission Shell"}:
		t.Fatalf("the head ask: %+v", row.HeadAsk)
	}
}

// TestTheInfoDocumentsCatalogsWaitForReady (§3.3, §3.13): the info document's
// catalogs are empty until the session's start has run.
func TestTheInfoDocumentsCatalogsWaitForReady(t *testing.T) {
	h := newHost(t, withoutStart())
	a := h.dial()
	a.sayHello(nil)
	row := ok[protocol.SessionsListResult](t, a.call(protocol.MethodSessionsList, protocol.SessionsListParams{})).Sessions[0]
	if len(row.Catalogs.Models) != 0 || len(row.Catalogs.Modes) != 0 || row.Activity != protocol.ActivityStarting {
		t.Fatalf("before the start: %+v", row)
	}
	if err := h.eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	row = ok[protocol.SessionsListResult](t, a.call(protocol.MethodSessionsList, protocol.SessionsListParams{})).Sessions[0]
	if len(row.Catalogs.Models) == 0 || len(row.Catalogs.Modes) == 0 || row.Activity != protocol.ActivityIdle {
		t.Fatalf("after the start: %+v", row)
	}
}

// TestSessionStateCarriesTheSettings (§3.3): session.state is State's
// projection plus the host's live settings, the config through the event
// codec's leaf wrapper, and the queue as the codec's rows.
func TestSessionStateCarriesTheSettings(t *testing.T) {
	h := newHost(t)
	a := h.dial()
	a.sayHello(nil)
	ok[protocol.QueueAddResult](t, queueAdd(a, a.cmd(), "queued"))
	st := ok[protocol.StateResult](t, a.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)}))
	if st.Activity != protocol.ActivityIdle || st.Turn != "" || st.SendNow != nil || st.HeadAsk != nil {
		t.Fatalf("the state: %+v", st)
	}
	if len(st.Queue) != 1 {
		t.Fatalf("the queue: %s", st.Queue)
	}
	if q, err := agent.DecodeQueuedPrompt(st.Queue[0]); err != nil || q.Text != "queued" {
		t.Fatalf("the row: %+v, %v", q, err)
	}
	if st.Settings.Model != "grok" || st.Settings.Mode != "agent" {
		t.Fatalf("the settings: %+v", st.Settings)
	}
	cfg, err := agent.DecodeConfigState(st.Settings.Config)
	if err != nil || cfg == nil || len(cfg.Options) != 2 || cfg.Options[0].ID != "effort" {
		t.Fatalf("the config: %s (%v)", st.Settings.Config, err)
	}
}

// TestSessionSnapshotOfMainAndOfAChild (§3.4): session.snapshot is one bounded
// snapshot through the snapshot codec, embedded raw: of the main transcript,
// or with a child's window filled first; an unknown child is
// unknown_subagent, a budget too small is snapshot_too_large.
func TestSessionSnapshotOfMainAndOfAChild(t *testing.T) {
	h := newHost(t)
	big := strings.Repeat("m", 8<<10)
	for i := range 12 {
		typ := agent.EventText
		if i%2 == 1 {
			typ = agent.EventThought
		}
		h.stub.Emit(agent.Event{Type: typ, Text: big})
	}
	h.stub.Emit(agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-1", Status: agent.SubagentRunning}, SubagentChange: agent.SubagentChangeSpawned})
	h.stub.Emit(agent.Event{Type: agent.EventUser, Agent: "sub-1", Text: "look"})
	h.stub.Emit(agent.Event{Type: agent.EventThought, Agent: "sub-1", Text: big})
	h.stub.Emit(agent.Event{Type: agent.EventText, Text: "main: newest"})
	a := h.dial()
	a.sayHello(nil)

	decode := func(r *protocol.Response) *transcript.Snapshot {
		t.Helper()
		s, err := transcript.DecodeSnapshot(ok[protocol.SnapshotResult](t, r).Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	whole := decode(a.call(protocol.MethodSessionSnapshot, snapshotParams(h, 0)))
	if whole.Incarnation != h.eng.State().Incarnation || whole.Main.Windowed || len(whole.Subs) != 1 || len(whole.Subs[0].Entries) != 2 {
		t.Fatalf("the default budget holds everything: %+v", whole)
	}
	const small = 40 << 10
	main := decode(a.call(protocol.MethodSessionSnapshot, snapshotParams(h, small)))
	child := decode(a.call(protocol.MethodSessionSnapshot, protocol.SnapshotParams{SessionID: sid(h), AgentID: "sub-1",
		Budget: &protocol.SnapshotBudget{SnapshotBytes: small}}))
	if !main.Subs[0].Windowed || child.Subs[0].Windowed || len(child.Subs[0].Entries) != 2 {
		t.Fatalf("the child's window: main-first %+v, child-first %+v", main.Subs[0], child.Subs[0])
	}
	if n := len(child.Main.Entries); n == 0 || child.Main.Entries[n-1].Text != "main: newest" {
		t.Fatal("a child's snapshot keeps the main transcript's newest entry")
	}
	refusedWith(t, a.call(protocol.MethodSessionSnapshot, protocol.SnapshotParams{SessionID: sid(h), AgentID: "sub-9"}),
		protocol.RPCRefused, protocol.CodeUnknownSubagent, protocol.ReasonUnknownSubagent)
	refusedWith(t, a.call(protocol.MethodSessionSnapshot, snapshotParams(h, 64)),
		protocol.RPCRefused, protocol.CodeFailed, protocol.ReasonSnapshotTooLarge)
}

// TestAsksListGetAndAnswer (§3.2, §3.3): asks.list is summaries; asks.get is
// the record with every body string capped at ItemCap and truncated set; an
// answer resolves it — once — and the record says how.
func TestAsksListGetAndAnswer(t *testing.T) {
	h := newHost(t)
	long := strings.Repeat("n", transcript.ItemCap+100)
	h.stub.Emit(agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionEvent{ID: "perm-1", Tool: "Shell",
		Options: []agent.PermissionOption{{OptionID: "allow", Name: long, Kind: "allow_once"}, {OptionID: "deny", Name: "Deny", Kind: "reject_once"}}}})
	a := h.dial()
	a.sayHello(nil)

	list := ok[protocol.AsksListResult](t, a.call(protocol.MethodAsksList, protocol.AsksListParams{SessionID: sid(h)}))
	if len(list.Asks) != 1 || list.Asks[0].ID != "perm-1" || list.Asks[0].Kind != "permission" || list.Asks[0].Label != "permission Shell" {
		t.Fatalf("the list: %+v", list)
	}
	rec := ok[protocol.AsksGetResult](t, a.call(protocol.MethodAsksGet, protocol.AsksGetParams{SessionID: sid(h), AskID: "perm-1"})).Ask
	if rec.Status != protocol.AskOpen || !rec.Truncated || rec.Answer != nil || !rec.ResolvedAt.IsZero() {
		t.Fatalf("the open record: %+v", rec)
	}
	var body struct {
		Permission json.RawMessage `json:"permission"`
	}
	if err := json.Unmarshal(rec.Body, &body); err != nil {
		t.Fatal(err)
	}
	p, err := agent.DecodePermissionEvent(body.Permission)
	if err != nil || len(p.Options) != 2 || len(p.Options[0].Name) != transcript.ItemCap || p.Options[1].Name != "Deny" {
		t.Fatalf("the capped body: %v", err)
	}

	answer := protocol.AsksAnswerParams{SessionID: sid(h), CommandID: a.cmd(), AskID: "perm-1", Answer: protocol.Answer{OptionID: "deny"}}
	ok[protocol.Empty](t, a.call(protocol.MethodAsksAnswer, answer))
	rec = ok[protocol.AsksGetResult](t, a.call(protocol.MethodAsksGet, protocol.AsksGetParams{SessionID: sid(h), AskID: "perm-1"})).Ask
	if rec.Status != protocol.AskResolved || rec.Outcome != "answered" || rec.Answer == nil || rec.Answer.OptionID != "deny" || rec.ResolvedAt.IsZero() {
		t.Fatalf("the resolved record: %+v", rec)
	}
	answer.CommandID = a.cmd()
	refusedWith(t, a.call(protocol.MethodAsksAnswer, answer), protocol.RPCRefused, protocol.CodeAlreadyResolved, protocol.ReasonAlreadyResolved)
	refusedWith(t, a.call(protocol.MethodAsksGet, protocol.AsksGetParams{SessionID: sid(h), AskID: "perm-9"}),
		protocol.RPCRefused, protocol.CodeUnknownAsk, protocol.ReasonUnknownAsk)
}

// TestTheCommandsMapToTheEngine (§3.3's table): each mutating method is its
// engine verb under the connection's client id, answered with the verb's
// result or its error — code and reason from the engine's one table.
func TestTheCommandsMapToTheEngine(t *testing.T) {
	h := newHost(t)
	h.stub.SetProvider(agent.GrokProvider())
	a := h.dial()
	a.sayHello(nil)
	prompt := func(mode protocol.PromptMode, text, fromRow string) *protocol.Response {
		return a.call(protocol.MethodSessionPrompt, protocol.PromptParams{SessionID: sid(h), CommandID: a.cmd(), Text: text, FromRow: fromRow, Mode: mode})
	}
	cancel := func(turn string) *protocol.Response {
		return a.call(protocol.MethodSessionCancel, protocol.CancelParams{SessionID: sid(h), CommandID: a.cmd(), TurnID: turn})
	}

	// Idle: nothing to cancel, disarm or interject into.
	if e := refusedWith(t, cancel(""), protocol.RPCRefused, protocol.CodeNotAccepting, protocol.ReasonNotAccepting); len(e.Data.Result) != 0 {
		t.Fatalf("a refused cancel carries a result: %s", e.Data.Result)
	}
	refusedWith(t, a.call(protocol.MethodSessionDisarm, protocol.DisarmParams{SessionID: sid(h), CommandID: a.cmd()}),
		protocol.RPCRefused, protocol.CodeNotAccepting, protocol.ReasonNotAccepting)
	refusedWith(t, prompt(protocol.PromptInterject, "and also", ""), protocol.RPCRefused, protocol.CodeNotAccepting, protocol.ReasonNotInTurn)

	// A turn that stays open: queue behind it and interject.
	hung := h.stub.HangNext()
	started := ok[protocol.PromptResult](t, prompt(protocol.PromptQueue, "one", ""))
	if started.Turn == "" || started.Text == nil || *started.Text != "one" {
		t.Fatalf("a started prompt: %+v", started)
	}
	await(t, hung, "the turn to open")
	sub, err := h.eng.Subscribe(agent.SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	queued := ok[protocol.PromptResult](t, prompt(protocol.PromptQueue, "two", ""))
	row, err := agent.DecodeQueuedPrompt(queued.Queued)
	if err != nil || row.Text != "two" {
		t.Fatalf("a queued prompt: %+v, %v", queued, err)
	}
	// A command runs under the connection's client id: its event's cause is
	// the connection's client and the command's own id.
	if cause, want := queueCause(t, sub, row.ID), a.hello.ClientID+"/"+a.lastCmd(); cause != want {
		t.Fatalf("the queued row's cause is %q, want %q", cause, want)
	}
	if r := prompt(protocol.PromptInterject, "and also", ""); r.Error != nil || string(r.Result) != "{}" {
		t.Fatalf("an interjection's result: %s %v", r.Result, r.Error)
	}
	if got := h.stub.Interjections(); len(got) != 2 || got[1] != "and also" {
		t.Fatalf("the Stub was interjected %q", got)
	}

	// The queue's verbs on the row.
	version := 0
	ok[protocol.Empty](t, a.call(protocol.MethodQueueEdit, protocol.QueueEditParams{SessionID: sid(h), CommandID: a.cmd(),
		RowID: row.ID, Text: "two, edited", ExpectedVersion: &version}))
	refusedWith(t, a.call(protocol.MethodQueueEdit, protocol.QueueEditParams{SessionID: sid(h), CommandID: a.cmd(),
		RowID: row.ID, Text: "stale", ExpectedVersion: &version}), protocol.RPCRefused, protocol.CodeStaleVersion, protocol.ReasonStaleVersion)
	removed, err := agent.DecodeQueuedPrompt(ok[protocol.QueueRemoveResult](t, a.call(protocol.MethodQueueRemove,
		protocol.QueueRemoveParams{SessionID: sid(h), CommandID: a.cmd(), RowID: row.ID})).Row)
	if err != nil || removed.Text != "two, edited" {
		t.Fatalf("the removed row: %+v, %v", removed, err)
	}
	ok[protocol.QueueAddResult](t, queueAdd(a, a.cmd(), "three"))
	cleared := ok[protocol.QueueClearResult](t, a.call(protocol.MethodQueueClear, protocol.QueueClearParams{SessionID: sid(h), CommandID: a.cmd()}))
	if len(cleared.Removed) != 1 {
		t.Fatalf("cleared %s", cleared.Removed)
	}
	if empty := ok[protocol.QueueClearResult](t, a.call(protocol.MethodQueueClear, protocol.QueueClearParams{SessionID: sid(h), CommandID: a.cmd()})); empty.Removed == nil || len(empty.Removed) != 0 {
		t.Fatalf("an empty clear: %+v", empty)
	}

	// A named stale turn, then the cancel of the running one.
	refusedWith(t, cancel("turn-9"), protocol.RPCRefused, protocol.CodeStaleTurn, protocol.ReasonStaleTurn)
	c := ok[protocol.CancelResult](t, cancel(started.Turn))
	if c.Turn != started.Turn || c.Outcome == "" {
		t.Fatalf("the cancel: %+v", c)
	}

	// A send-now over a working turn arms (and the arm's own cancel makes
	// room for it at once).
	waitFor(t, "the session idle again", func() bool { return h.eng.State().Activity == engine.ActivityIdle })
	hung = h.stub.HangNext()
	ok[protocol.PromptResult](t, prompt(protocol.PromptQueue, "four", ""))
	await(t, hung, "the second turn to open")
	if r := ok[protocol.PromptResult](t, prompt(protocol.PromptSendNow, "now", "")); !r.Armed {
		t.Fatalf("a send-now over a working turn: %+v", r)
	}

	// Settings, the title, a sub-agent stop the Stub cannot make.
	set := ok[protocol.SetResult](t, a.call(protocol.MethodSessionSet, protocol.SetParams{SessionID: sid(h), CommandID: a.cmd(),
		Setting: protocol.Setting{Kind: protocol.SettingMode, Value: "plan"}}))
	if set.Value != "plan" || set.Rev == 0 {
		t.Fatalf("the set: %+v", set)
	}
	refusedWith(t, a.call(protocol.MethodSessionSet, protocol.SetParams{SessionID: sid(h), CommandID: a.cmd(),
		Setting: protocol.Setting{Kind: protocol.SettingModel, ID: "x", Value: "fast"}}), protocol.RPCRefused, protocol.CodeBadRequest, protocol.ReasonBadRequest)
	ok[protocol.Empty](t, a.call(protocol.MethodSessionSetTitle, protocol.SetTitleParams{SessionID: sid(h), CommandID: a.cmd(), Title: "renamed"}))
	if st := h.eng.State(); st.Title != "renamed" {
		t.Fatalf("the title is %q", st.Title)
	}
	refusedWith(t, a.call(protocol.MethodSubagentCancel, protocol.SubagentCancelParams{SessionID: sid(h), CommandID: a.cmd(), AgentID: "sub-1"}),
		protocol.RPCRefused, protocol.CodeUnsupported, protocol.ReasonUnsupported)
}

// queueCause reads sub until the queued event for row, and is its cause.
func queueCause(t *testing.T, sub *agent.Subscription, row string) string {
	t.Helper()
	for {
		select {
		case rec, open := <-sub.Records():
			if !open {
				t.Fatalf("the subscription ended: %v", sub.Err())
			}
			ev, err := rec.Event()
			if err != nil {
				t.Fatal(err)
			}
			if ev.Type == agent.EventQueue && ev.Queue != nil && ev.Queue.ID == row && ev.QueueChange == agent.QueueQueued {
				return ev.Cause
			}
		case <-time.After(watchdog):
			t.Fatalf("no queued event for %s", row)
		}
	}
}

// failingStore is a session index whose every write fails.
type failingStore struct{}

func (failingStore) Upsert(sessions.Row) error { return errors.New("disk full") }

// TestAnIndexWriteFailureCarriesItsCause (§3.2): a rename whose index write
// failed is index_write, and data.cause is the write's own failure — the text
// a client draws beside "renamed".
func TestAnIndexWriteFailureCarriesItsCause(t *testing.T) {
	h := newHost(t, withIndex(engine.IndexOptions{Store: failingStore{}, CWD: "/w", Provider: "cursor"}))
	a := h.dial()
	a.sayHello(nil)
	e := refusedWith(t, a.call(protocol.MethodSessionSetTitle, protocol.SetTitleParams{SessionID: sid(h), CommandID: a.cmd(), Title: "renamed"}),
		protocol.RPCRefused, protocol.CodeIndexWrite, protocol.ReasonIndexWrite)
	if e.Data.Cause != "disk full" || !strings.Contains(e.Message, "disk full") {
		t.Fatalf("the cause: %+v", e)
	}
}

// TestAHostWithNoSessionIsNotReady: before SetEngine, hello is unavailable,
// reason not_ready — never stored, retry.
func TestAHostWithNoSessionIsNotReady(t *testing.T) {
	h := newHost(t, withoutEngine())
	a := h.dial()
	refusedWith(t, a.call(protocol.MethodHello, helloParams(nil)), protocol.RPCRefused, protocol.CodeUnavailable, protocol.ReasonNotReady)
	h.srv.SetEngine(h.eng)
	a.sayHello(nil)
}

// TestAResponseOverTheLineLimitIsFailed (§3.2): a response longer than the
// outbound line limit is replaced by failed, reason response_too_large —
// never truncated — and the connection goes on. The limit is 16 MiB; a test
// seam lowers it.
func TestAResponseOverTheLineLimitIsFailed(t *testing.T) {
	h := newHost(t, withMaxLine(2048))
	h.stub.Emit(agent.Event{Type: agent.EventText, Text: strings.Repeat("y", 4096)})
	a := h.dial()
	a.sayHello(nil)
	refusedWith(t, a.call(protocol.MethodSessionSnapshot, snapshotParams(h, 0)), protocol.RPCRefused, protocol.CodeFailed, protocol.ReasonResponseTooLarge)
	ok[protocol.StateResult](t, a.call(protocol.MethodSessionState, protocol.StateParams{SessionID: sid(h)}))
}
