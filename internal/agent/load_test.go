package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
)

// loadSessionID is the id every load test asks for. The fake agent replays
// whatever id it is handed, so this is also the id the restored snapshot must
// carry: a session that loaded "s-restored" and reports something else is
// listening to a session it did not ask for.
const loadSessionID = "s-restored"

// replayedPrompt is the two chunks cmd/craze-fake-agent/load.go replays,
// joined — what one coalesced EventUser has to read as.
const replayedPrompt = "List the files in the current working directory in one line."

// newLoadSession builds an unstarted session pointed at a load script. Start
// is left to the caller: half of these tests are about what it refuses.
func newLoadSession(t *testing.T, script string, mutate func(*Options)) *session {
	t.Helper()
	grok := strings.HasPrefix(script, "grok-")
	if grok {
		t.Setenv("XAI_API_KEY", "")
		t.Setenv("GROK_CODE_XAI_API_KEY", "")
	}
	opts := Options{
		Binary:        fakeAgentPath(t),
		ExtraArgs:     []string{"-script=" + script},
		Workspace:     t.TempDir(),
		Force:         true,
		Stderr:        io.Discard,
		LoadSessionID: loadSessionID,
	}
	if grok {
		p := GrokProvider()
		opts.Provider = &p
	}
	if mutate != nil {
		mutate(&opts)
	}
	s := newTestSession(t, opts)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// drainBuffered reads everything the session has already emitted, in order.
// It is only called after Start has returned, and by then the whole replay is
// in the channel: the agent sends every notification before the session/load
// result, the client's read loop dispatches each one synchronously, and the
// end bracket is emitted before Start returns.
func drainBuffered(s *session) []Event {
	var out []Event
	for {
		select {
		case ev := <-s.Events():
			out = append(out, ev)
		default:
			return out
		}
	}
}

// eventShape names an event the way the assertions read it: the replay
// brackets by phase, everything else by type, with runs of the same type
// collapsed (one tool call arrives as a pending update and a completed one).
func eventShape(evs []Event) []string {
	var out []string
	for _, ev := range evs {
		kind := string(ev.Type)
		if ev.Type == EventReplay && ev.Replay != nil {
			kind = "replay:" + ev.Replay.Phase
		}
		if n := len(out); n > 0 && out[n-1] == kind {
			continue
		}
		out = append(out, kind)
	}
	return out
}

func userChunkNotification(text string) acp.SessionNotification {
	return acp.SessionNotification{
		SessionID: loadSessionID,
		Update: mustJSON(map[string]any{
			"sessionUpdate": updateUserMessage,
			"content":       map[string]any{"type": "text", "text": text},
		}),
	}
}

func thoughtNotification(text string) acp.SessionNotification {
	return acp.SessionNotification{
		SessionID: loadSessionID,
		Update: mustJSON(map[string]any{
			"sessionUpdate": acp.UpdateAgentThought,
			"content":       map[string]any{"type": "text", "text": text},
		}),
	}
}

func sessionInfoNotification(title string) acp.SessionNotification {
	return acp.SessionNotification{
		SessionID: loadSessionID,
		Update: mustJSON(map[string]any{
			"sessionUpdate": acp.UpdateSessionInfo,
			"title":         title,
		}),
	}
}

// TestLoadReplayIsBracketedAndStamped is the whole shape of a cursor replay:
// the brackets, one user block for the two chunks the agent split the prompt
// into, and Replayed on everything between them and on neither bracket.
func TestLoadReplayIsBracketedAndStamped(t *testing.T) {
	s := newLoadSession(t, "load", nil)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	evs := drainBuffered(s)
	// The meta before the end bracket is the install: the restored snapshot,
	// said in the stream in the section that installed it, flushed before
	// EventReplay{end} so it precedes the boundary that means "the restored
	// snapshot is in place" (r23 finding 2). It is stamped replayed like
	// everything else inside the bracket, which the loop below checks.
	want := []string{"replay:start", "user", "thought", "tool", "text", "meta", "replay:end"}
	if got := eventShape(evs); !equalStrings(got, want) {
		t.Fatalf("replay shape %v, want %v\n%s", got, want, formatEvents(evs))
	}
	var users []Event
	for _, ev := range evs {
		if ev.Type == EventUser {
			users = append(users, ev)
		}
		if ev.Type == EventReplay && ev.Replayed {
			t.Fatalf("a bracket must not be marked replayed: %+v", ev.Replay)
		}
		if ev.Type != EventReplay && !ev.Replayed {
			t.Fatalf("event %s inside the bracket is not marked replayed: %+v", ev.Type, ev)
		}
	}
	if len(users) != 1 {
		t.Fatalf("two replayed chunks must be one user block, got %d:\n%s", len(users), formatEvents(evs))
	}
	if users[0].Text != replayedPrompt {
		t.Fatalf("coalesced prompt %q, want %q", users[0].Text, replayedPrompt)
	}
	if got := s.Snapshot().SessionID; got != loadSessionID {
		t.Fatalf("snapshot session id %q, want %q", got, loadSessionID)
	}
}

// TestLoadRequiresTheCapability: an agent that does not advertise loadSession
// is refused before anything reaches the wire — the error is craze's own, not
// the -32601 the agent would have answered with, and no bracket was opened.
func TestLoadRequiresTheCapability(t *testing.T) {
	s := newLoadSession(t, "echo", nil)
	err := s.Start(t.Context())
	want := "agent: " + CursorProvider().Name() + " does not support session/load"
	if err == nil || err.Error() != want {
		t.Fatalf("start error %v, want %q", err, want)
	}
	if evs := drainBuffered(s); len(evs) != 0 {
		t.Fatalf("nothing may be emitted for a refused load:\n%s", formatEvents(evs))
	}
}

// TestLoadMissingSessionCarriesTheAgentsMessage: an id the agent does not have
// fails Start with what the agent said, and never opens the end bracket.
func TestLoadMissingSessionCarriesTheAgentsMessage(t *testing.T) {
	s := newLoadSession(t, "load-missing", nil)
	err := s.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "Session not found") {
		t.Fatalf("start error %v, want the agent's message", err)
	}
	if strings.Contains(err.Error(), "session/new") {
		t.Fatalf("a failed load must never fall back to session/new: %v", err)
	}
	if got := eventShape(drainBuffered(s)); !equalStrings(got, []string{"replay:start"}) {
		t.Fatalf("events %v, want the opening bracket alone", got)
	}
}

// TestLoadHangFailsAtTheDeadline drives the script that never answers
// session/load. The deadline is the only thing that can end that call.
func TestLoadHangFailsAtTheDeadline(t *testing.T) {
	t.Cleanup(acp.SetLoadSessionTimeout(200 * time.Millisecond))
	s := newLoadSession(t, "load-hang", nil)
	err := s.Start(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start error %v, want a deadline", err)
	}
}

// TestGrokLoadTurnCompletedIsIgnored: the replayed terminator must not open or
// close a foreign turn, and must emit nothing at all.
func TestGrokLoadTurnCompletedIsIgnored(t *testing.T) {
	s := newLoadSession(t, "grok-load", nil)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	evs := drainBuffered(s)
	for _, ev := range evs {
		if ev.Type == EventForeignTurn {
			t.Fatalf("a replayed turn_completed emitted a foreign turn:\n%s", formatEvents(evs))
		}
	}
	if s.Snapshot().ForeignTurn {
		t.Fatal("a replayed turn_completed left the session in a foreign turn")
	}
}

// TestGrokLoadResultGetsFallbackModes: grok answers a load with models alone,
// so the modes are the provider's fallback — and --plan still resolves against
// them, because the mode tail runs after the restored snapshot is installed.
func TestGrokLoadResultGetsFallbackModes(t *testing.T) {
	s := newLoadSession(t, "grok-load", func(o *Options) { o.Mode = "plan" })
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	var ids []string
	for _, m := range snap.Modes {
		ids = append(ids, m.ID)
	}
	if !equalStrings(ids, []string{"default", "plan", "ask"}) {
		t.Fatalf("modes %v, want the grok fallback", ids)
	}
	if snap.CurrentMode != "plan" {
		t.Fatalf("current mode %q, want plan", snap.CurrentMode)
	}
	if snap.CurrentModel != "grok-4.6" || len(snap.Models) != 1 {
		t.Fatalf("models came from somewhere else: %q %+v", snap.CurrentModel, snap.Models)
	}
	// grok returns no configOptions on a load, so the chips are absent after a
	// resume. Documented as a deviation, pinned here so it stays deliberate.
	if len(snap.Config) != 0 {
		t.Fatalf("config %+v, want none", snap.Config)
	}
}

// TestLoadMergesReplayIntoRestoredSnapshot is the regression a plain
// `s.snap = restored` would cause: the load result is installed *over* a
// snapshot the replay has been filling in, so everything the replay
// accumulated — tool rows, sub-agent rows, and the seeded title — has to
// survive it.
func TestLoadMergesReplayIntoRestoredSnapshot(t *testing.T) {
	s := newLoadSession(t, "grok-load", func(o *Options) {
		o.Title = "yesterday's thread"
		o.TitlePinned = true
	})
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if snap.Title != "yesterday's thread" {
		t.Fatalf("title %q, want the seeded one", snap.Title)
	}
	if len(snap.Tools) != 1 || snap.Tools[0].Title != "List Directory" {
		t.Fatalf("replayed tool rows lost: %+v", snap.Tools)
	}
	if len(snap.Subagents) != 1 || snap.Subagents[0].ID != "sub-1" ||
		snap.Subagents[0].Status != SubagentCompleted {
		t.Fatalf("replayed sub-agent rows lost: %+v", snap.Subagents)
	}
	if snap.SessionID != loadSessionID {
		t.Fatalf("session id %q", snap.SessionID)
	}
	if len(snap.Models) == 0 {
		t.Fatalf("the load result's models never landed: %+v", snap)
	}
}

// TestLoadNeverCallsSessionNew: every load script answers session/new with an
// error, so a Start that succeeded proves craze reached the session the only
// way it is allowed to.
func TestLoadNeverCallsSessionNew(t *testing.T) {
	for _, script := range []string{"load", "grok-load"} {
		t.Run(script, func(t *testing.T) {
			s := newLoadSession(t, script, nil)
			if err := s.Start(t.Context()); err != nil {
				t.Fatalf("start: %v", err)
			}
		})
	}
	for _, script := range []string{"load-missing", "load-hang"} {
		t.Run(script, func(t *testing.T) {
			t.Cleanup(acp.SetLoadSessionTimeout(200 * time.Millisecond))
			s := newLoadSession(t, script, nil)
			err := s.Start(t.Context())
			if err == nil {
				t.Fatal("a refused load must fail Start")
			}
			if strings.Contains(err.Error(), "session/new") {
				t.Fatalf("craze fell back to session/new: %v", err)
			}
		})
	}
}

// TestMainUserChunkOutsideReplayIsDropped: grok and gx echo the user's own
// prompt live. Emitting that would double every user block in the TUI and add
// user lines to `craze prompt --json`, so outside a replay the chunk is
// dropped, exactly as it was before session/load existed.
func TestMainUserChunkOutsideReplayIsDropped(t *testing.T) {
	s := newTestSession(t, Options{})
	s.onUpdate(userChunkNotification("the user's own prompt"))
	if evs := drainBuffered(s); len(evs) != 0 {
		t.Fatalf("a live main-session user chunk must stay dropped:\n%s", formatEvents(evs))
	}
	// Dropped, not parked: an update behind it must not flush it out either,
	// or the block would simply arrive late.
	s.onUpdate(thoughtNotification("thinking"))
	if got := eventShape(drainBuffered(s)); !equalStrings(got, []string{"thought"}) {
		t.Fatalf("events %v, want the thought alone", got)
	}
}

// TestReplayUserChunksCoalesceIntoOneEvent: the chunks accumulate and go out
// as one EventUser when the next non-user update arrives.
func TestReplayUserChunksCoalesceIntoOneEvent(t *testing.T) {
	s := newTestSession(t, Options{})
	s.replaying.Store(true)
	s.onUpdate(userChunkNotification("List the files in the"))
	s.onUpdate(userChunkNotification(" current working directory."))
	if evs := drainBuffered(s); len(evs) != 0 {
		t.Fatalf("a chunk must not be emitted on its own:\n%s", formatEvents(evs))
	}
	s.onUpdate(thoughtNotification("thinking"))
	evs := drainBuffered(s)
	if got := eventShape(evs); !equalStrings(got, []string{"user", "thought"}) {
		t.Fatalf("events %v, want the coalesced prompt then the thought\n%s", got, formatEvents(evs))
	}
	if evs[0].Text != "List the files in the current working directory." {
		t.Fatalf("coalesced text %q", evs[0].Text)
	}
	if !evs[0].Replayed {
		t.Fatal("the coalesced prompt is a replayed event")
	}
}

// TestLoadSeedsTitleAndPinBeforeReplayEnd: replay end means "the restored
// session is ready", so the title the index carried — and the session id, and
// the models — are all readable from the snapshot the moment the end bracket
// is seen. The pin then holds against a title the agent sends later.
func TestLoadSeedsTitleAndPinBeforeReplayEnd(t *testing.T) {
	s := newLoadSession(t, "load", func(o *Options) {
		o.Title = "yesterday's thread"
		o.TitlePinned = true
	})
	type atEnd struct {
		title  string
		id     string
		models int
	}
	seen := make(chan atEnd, 1)
	go func() {
		for ev := range s.Events() {
			if ev.Type == EventReplay && ev.Replay != nil && ev.Replay.Phase == ReplayEnd {
				snap := s.Snapshot()
				seen <- atEnd{snap.Title, snap.SessionID, len(snap.Models)}
				return
			}
		}
	}()
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	var got atEnd
	select {
	case got = <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("no end bracket")
	}
	if got.title != "yesterday's thread" {
		t.Fatalf("title at replay end %q, want the seeded one", got.title)
	}
	if got.id != loadSessionID || got.models == 0 {
		t.Fatalf("the snapshot was not ready at replay end: %+v", got)
	}
	// The pin: a session_info_update after the load changes nothing and says
	// nothing.
	s.onUpdate(sessionInfoNotification("a title the agent invented"))
	if title := s.Snapshot().Title; title != "yesterday's thread" {
		t.Fatalf("a pinned title was overwritten: %q", title)
	}
	if evs := drainBuffered(s); len(evs) != 0 {
		t.Fatalf("an ignored session_info_update must emit nothing:\n%s", formatEvents(evs))
	}
}

// TestLoadWithoutPinLetsTheAgentRetitle is the other half: an unpinned seeded
// title is a starting point, not a decision.
func TestLoadWithoutPinLetsTheAgentRetitle(t *testing.T) {
	s := newLoadSession(t, "load", func(o *Options) { o.Title = "first prompt fallback" })
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	drainBuffered(s)
	s.onUpdate(sessionInfoNotification("the agent's own title"))
	if title := s.Snapshot().Title; title != "the agent's own title" {
		t.Fatalf("title %q, want the agent's", title)
	}
	if got := eventShape(drainBuffered(s)); !equalStrings(got, []string{"meta"}) {
		t.Fatalf("events %v, want one meta", got)
	}
}

// TestSetTitlePinsAndPublishesADelta: /rename is craze's own — it renames the
// session, pins it against the agent's own title, and says so in a Title delta
// with **no Event.Text**, which is what keeps a rename out of
// `craze prompt --json`'s title line and out of the index as an agent title
// (plan 021 §3.8). It still waits on nothing: the delta is enqueued with the
// pin, under the same lock, and never published from the caller's goroutine,
// because /rename is answered in the UI's own Update.
func TestSetTitlePinsAndPublishesADelta(t *testing.T) {
	s := newTestSession(t, Options{})
	if err := s.SetTitle("c-1/4", "fix the flaky pty test"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	if got := s.Snapshot().Title; got != "fix the flaky pty test" {
		t.Fatalf("title %q", got)
	}
	_ = s.log.Flush(context.Background(), nil)
	evs := drainBuffered(s)
	if len(evs) != 1 || evs[0].Type != EventMeta || evs[0].Text != "" || evs[0].Cause != "c-1/4" ||
		evs[0].State == nil || evs[0].State.Title == nil || *evs[0].State.Title != "fix the flaky pty test" {
		t.Fatalf("SetTitle published:\n%s", formatEvents(evs))
	}
	s.onUpdate(sessionInfoNotification("the agent's own title"))
	if got := s.Snapshot().Title; got != "fix the flaky pty test" {
		t.Fatalf("the pin did not hold: %q", got)
	}
	// The pin refused the agent's title, so nothing changed and nothing is
	// said: a delta is a change, not a notification that one was attempted.
	_ = s.log.Flush(context.Background(), nil)
	if evs := drainBuffered(s); len(evs) != 0 {
		t.Fatalf("a refused agent title published:\n%s", formatEvents(evs))
	}
}

// TestLoadLongReplayRacesConsumers is the -race test: a replay far longer than
// the event buffer streaming into a consumer that is draining it, while other
// goroutines read the snapshot across the phase transitions. It is also the
// proof that a draining consumer cannot deadlock the load.
func TestLoadLongReplayRacesConsumers(t *testing.T) {
	s := newLoadSession(t, "load-long", nil)
	var (
		mu      sync.Mutex
		texts   int
		stamped int
		phases  []string
	)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range s.Events() {
			mu.Lock()
			if ev.Type == EventText {
				texts++
				if ev.Replayed {
					stamped++
				}
			}
			if ev.Type == EventReplay && ev.Replay != nil {
				phases = append(phases, ev.Replay.Phase)
				if ev.Replay.Phase == ReplayEnd {
					mu.Unlock()
					return
				}
			}
			mu.Unlock()
		}
	}()
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 3; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.Snapshot()
			}
		}()
	}
	err := s.Start(t.Context())
	close(stop)
	waitDone(t, &readers)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the replay never reached its end bracket")
	}
	mu.Lock()
	defer mu.Unlock()
	if texts < 600 || stamped != texts {
		t.Fatalf("%d replayed texts, %d stamped", texts, stamped)
	}
	if !equalStrings(phases, []string{ReplayStart, ReplayEnd}) {
		t.Fatalf("phases %v", phases)
	}
}

// TestReplayPhaseFlipsUnderUpdates races the phase transitions themselves
// against updates arriving: the flag is read on the client's read loop and
// written on Start's goroutine, and the coalescing buffer is touched from
// both. Run under -race, this is what says the atomic and the mutex are
// enough.
func TestReplayPhaseFlipsUnderUpdates(t *testing.T) {
	s := newTestSession(t, Options{})
	// The drainer outlives the workers: an emit blocks on a full channel, so
	// a reader that stopped first would wedge the updates rather than race
	// them.
	stop := make(chan struct{})
	var drain sync.WaitGroup
	drain.Add(1)
	go func() {
		defer drain.Done()
		for {
			select {
			case <-stop:
				return
			case <-s.Events():
			}
		}
	}()
	var work sync.WaitGroup
	work.Add(2)
	go func() {
		defer work.Done()
		for i := 0; i < 500; i++ {
			s.onUpdate(userChunkNotification("chunk "))
			s.onUpdate(thoughtNotification("thinking"))
		}
	}()
	go func() {
		defer work.Done()
		for i := 0; i < 500; i++ {
			s.replaying.Store(true)
			s.replaying.Store(false)
			s.flushReplayUser()
			_ = s.Snapshot()
		}
	}()
	waitDone(t, &work)
	close(stop)
	waitDone(t, &drain)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func formatEvents(evs []Event) string {
	var b strings.Builder
	for _, ev := range evs {
		b.WriteString(string(ev.Type))
		if ev.Replay != nil {
			b.WriteString(":" + ev.Replay.Phase)
		}
		if ev.Replayed {
			b.WriteString(" (replayed)")
		}
		if ev.Text != "" {
			b.WriteString(" " + jsonQuote(ev.Text))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return s
	}
	return string(b)
}

// waitDone waits for wg, but gives up rather than hanging. An unbounded Wait
// on a WaitGroup a bug never satisfies blocks until the test binary's own
// timeout fires, which kills every other test in the package and reports a
// goroutine dump instead of a failure. The deadline is far longer than any of
// these waits legitimately needs, so it only ever fires on a real bug -- and
// then it names the test and the line.
func waitDone(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the goroutines to finish")
	}
}
