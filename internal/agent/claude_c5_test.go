package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/errkind"
)

// Usage, plan limits, the overage guard and the prompt errors (plan 052 C5),
// over the fake adapter's claude_usage.go scripts.

// overageRig points the guard at a record of the test's own, with a short
// stop grace, and returns the record's path and a channel told each time an
// overage stop has closed its client.
func overageRig(t *testing.T, grace time.Duration) (record string, stopped <-chan time.Time) {
	t.Helper()
	record = filepath.Join(t.TempDir(), "claude-overage.json")
	t.Cleanup(SetClaudeOverageForTest(record, nil, grace))
	return record, overageStops(t)
}

// overageStops is a channel told each time an overage stop has closed its
// client; it replaces any earlier one until the test ends.
func overageStops(t *testing.T) <-chan time.Time {
	t.Helper()
	ch := make(chan time.Time, 8)
	f := func() {
		select {
		case ch <- time.Now():
		default:
		}
	}
	prev := testOverageStopped.Swap(&f)
	t.Cleanup(func() { testOverageStopped.Store(prev) })
	return ch
}

// idleTripGate is a FIFO for the fake's CRAZE_FAKE_CLAUDE_IDLE_TRIP, and the
// env entry naming it: tripIdle releases the trip, once the test's Start is
// done.
func idleTripGate(t *testing.T) (env string, tripIdle func()) {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "idle-trip")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	return "CRAZE_FAKE_CLAUDE_IDLE_TRIP=" + fifo, func() {
		t.Helper()
		// The fake holds the FIFO open for reading and writing while it
		// waits, so this open does not block once it does; until then it
		// waits for the fake, which opens it right after session/new.
		done := make(chan error, 1)
		go func() {
			f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
			if err == nil {
				_, err = f.Write([]byte{1})
				err = errors.Join(err, f.Close())
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the fake never opened its idle-trip gate")
		}
	}
}

// overageRefusal is the trip record's word for the record's paths as the
// seams give them now (a session reads its own resolved paths).
func overageRefusal() error { return overageRefusalAt(resolveOveragePaths()) }

// readRecord is the trip record at path, failing the test if there is none.
func readRecord(t *testing.T, path string) overageRecord {
	t.Helper()
	rec, err := readOverageRecord(path)
	if err != nil || rec == nil {
		t.Fatalf("the trip record at %s: %+v %v", path, rec, err)
	}
	return *rec
}

// isQuota reports whether err is the guard's quota error.
func isQuota(err error) bool {
	return err != nil && ErrorInfo(err).Kind == errkind.Quota && strings.Contains(err.Error(), "Claude would bill this as extra usage")
}

// waitStopped waits for the overage stop to close the client, and checks the
// agent's process has gone.
func waitStopped(t *testing.T, r *claudeRig, stopped <-chan time.Time, within time.Duration) time.Time {
	t.Helper()
	var at time.Time
	select {
	case at = <-stopped:
	case <-time.After(within):
		t.Fatalf("the session was not stopped within %s", within)
	}
	c := r.s.clientRef()
	if c == nil {
		t.Fatal("no client")
	}
	select {
	case <-c.Exited():
	default:
		t.Fatal("the agent's process is still running after the stop")
	}
	return at
}

// signals are the guard's four trip signals, as the fake's
// CRAZE_FAKE_CLAUDE_OVERAGE names them (overage and included are both the
// rateLimitType signal).
var signals = []string{"using", "inuse", "overage", "included", "rejected"}

// TestClaudeOverageTripsWhereverItLands (AC14): each trip signal, mid-prompt,
// before any output, at the turn's terminal usage, during a foreign turn,
// while idle and as a trailing usage into the next turn, writes the record,
// ends the running turn with quota, stops the session within the grace and
// leaves every later prompt refused.
func TestClaudeOverageTripsWhereverItLands(t *testing.T) {
	placements := []string{"MID", "BEFORE", "TERMINAL", "BG", "IDLE", "TRAILING"}
	for _, sig := range signals {
		for _, where := range placements {
			t.Run(sig+"/"+where, func(t *testing.T) {
				shortForeign(t, 5*time.Second)
				const grace = 150 * time.Millisecond
				record, stopped := overageRig(t, grace)
				env := []string{"CRAZE_FAKE_CLAUDE_OVERAGE=" + sig, "CRAZE_FAKE_CLAUDE_GAP=150ms"}
				tripIdle := func() {}
				if where == "IDLE" {
					var gate string
					gate, tripIdle = idleTripGate(t)
					env = append(env, gate)
				}
				r, log := startClaude(t, "claude-overage", Options{Interactive: true}, env...)
				before := time.Now()
				switch where {
				case "MID", "BEFORE", "TERMINAL":
					_, err := r.s.Prompt(t.Context(), where)
					if !isQuota(err) {
						t.Fatalf("the prompt ended with %v; want the guard's quota", err)
					}
					// The record was written before the turn ended.
					readRecord(t, record)
				case "BG":
					if _, err := r.s.Prompt(t.Context(), "BG"); err != nil {
						t.Fatal(err)
					}
					b := log.waitBrackets(t, 2)
					if b[1].Running || b[1].Err == nil || b[1].Err.Kind != errkind.Quota || !strings.Contains(b[1].Err.Message, "extra usage") {
						t.Fatalf("the foreign turn ended %+v; want failed with quota", b[1])
					}
					readRecord(t, record)
				case "IDLE":
					tripIdle()
				case "TRAILING":
					if _, err := r.s.Prompt(t.Context(), "TRAILING"); err != nil {
						t.Fatal(err)
					}
					// The next turn is running when the trailing usage lands:
					// it ends too, deliberately.
					_, err := r.s.Prompt(t.Context(), "HOLD")
					if !isQuota(err) {
						t.Fatalf("the next turn ended with %v; want the guard's quota", err)
					}
				}
				at := waitStopped(t, r, stopped, 5*time.Second)
				rec := readRecord(t, record)
				if rec.Window == "" || !rec.Until.After(before.Add(50*time.Minute)) || rec.At.Before(before.Add(-time.Second)) {
					t.Fatalf("record %+v; want the window and an hour on", rec)
				}
				if at.Sub(before) > 5*time.Second {
					t.Fatalf("stopped %s after the start", at.Sub(before))
				}
				// Every later prompt is refused, on this session and by the record.
				if _, err := r.s.Prompt(t.Context(), "again"); !isQuota(err) {
					t.Fatalf("a prompt after the stop: %v; want quota", err)
				}
				if err := overageRefusal(); !isQuota(err) || !strings.Contains(err.Error(), "resets ") {
					t.Fatalf("the record's refusal %v; want quota naming the reset", err)
				}
			})
		}
	}
}

// TestClaudeOverageStopsAChildThatIgnoresTheCancel (AC14, § D3): the root
// settles at once on the cancel while a child keeps working; the process
// group is killed within the grace, and nothing the child sends after the
// kill reaches the session.
//
// "At once" is an order, not a duration (a starved run took 500 ms to get
// there): the kill is held until the prompt has returned, so a root that
// settled only because of the kill never returns, and fails on its context.
func TestClaudeOverageStopsAChildThatIgnoresTheCancel(t *testing.T) {
	const grace = 300 * time.Millisecond
	_, stopped := overageRig(t, grace)
	r, log := startClaude(t, "claude-overage", Options{Interactive: true})
	returned := make(chan struct{})
	r.s.clientRef().SetBeforeSeverForTest(func() { <-returned })
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := r.s.Prompt(ctx, "CHILD")
	released := time.Now()
	close(returned)
	if !isQuota(err) {
		t.Fatalf("the prompt ended with %v; want quota, with the kill held", err)
	}
	at := waitStopped(t, r, stopped, 5*time.Second)
	// The kill is outright: the agent ignores SIGTERM, and a stop that sent
	// one first would wait out the reaper's own grace too. It comes after the
	// grace, and right after the later of the grace's end and the release.
	from := start.Add(grace)
	if released.After(from) {
		from = released
	}
	if took := at.Sub(start); took < grace || at.Sub(from) > time.Second {
		t.Fatalf("stopped %s after the prompt (%s after the grace and the release); want the grace (%s) and the kill", took, at.Sub(from), grace)
	}
	ticks := func() int {
		n := 0
		for _, ev := range log.snapshot() {
			if ev.Type == EventText && ev.Agent == "child-1" && strings.HasPrefix(ev.Text, "tick ") {
				n++
			}
		}
		return n
	}
	// The child was still ticking after the root settled: it ignored the
	// cancel, so only the kill could stop it. Everything published by the
	// stop is in the collector before it is counted.
	log.caughtUp(t)
	n := ticks()
	if n < 5 {
		t.Fatalf("%d ticks before the kill; want the child to have outlived the root's cancel", n)
	}
	// Nothing after the kill: a window to look in, then the collector caught
	// up again, so a late tick is counted, not missed.
	time.Sleep(400 * time.Millisecond)
	log.caughtUp(t)
	if after := ticks(); after != n {
		t.Fatalf("%d ticks after the kill (from %d); want none", after-n, n)
	}
}

// TestClaudeOverageWakeWhileTheRecordHolds (§ D3): a session started before
// another's trip gets a wake of its own: the turn ends with the refusal, and
// the session stops.
func TestClaudeOverageWakeWhileTheRecordHolds(t *testing.T) {
	shortForeign(t, 5*time.Second)
	record, stopped := overageRig(t, 150*time.Millisecond)
	r, log := startClaude(t, "claude-overage", Options{Interactive: true}, "CRAZE_FAKE_CLAUDE_IDLE_WAKE=300ms")
	// Another session trips.
	if _, _, err := writeOverageRecord(resolveOveragePaths(), overageRecord{Until: time.Now().Add(time.Hour), Window: "seven_day", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	b := log.waitBrackets(t, 2)
	if !b[0].Running || b[1].Running || b[1].Err == nil || b[1].Err.Kind != errkind.Quota {
		t.Fatalf("brackets %+v; want the wake ended with quota", b)
	}
	waitStopped(t, r, stopped, 5*time.Second)
	if _, err := os.Stat(record); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeOverageRefusesWhileTheRecordHolds (AC14): a new claude session on
// the machine is refused at its preflight, and a running one's prompt before
// anything is written, until the record's until; an expired record refuses
// nothing, and one that cannot be read refuses (the guard fails closed).
func TestClaudeOverageRefusesWhileTheRecordHolds(t *testing.T) {
	record, _ := overageRig(t, 150*time.Millisecond)
	// A running session first, then the record.
	r, _ := startClaude(t, "claude-basic", Options{Interactive: true})
	if _, _, err := writeOverageRecord(resolveOveragePaths(), overageRecord{Until: time.Now().Add(time.Hour), Window: "five_hour", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.Prompt(t.Context(), "hello"); !isQuota(err) || !strings.Contains(err.Error(), "5-hour") {
		t.Fatalf("a prompt while the record holds: %v; want quota", err)
	}
	if calls := r.read(t, "calls"); strings.Contains(calls, "session/prompt") {
		t.Fatalf("a refused prompt reached the agent: %s", calls)
	}
	// A new session.
	n := newClaudeRig(t, "claude-basic", Options{})
	err := n.s.Start(t.Context())
	if !isQuota(err) || StartErrorKind(err) != errkind.Quota || !strings.Contains(err.Error(), record) {
		t.Fatalf("a start while the record holds: %v; want quota naming the record", err)
	}
	// Expired: nothing refused.
	if err := os.WriteFile(record, []byte(`{"until":"2020-01-01T00:00:00Z","window":"seven_day","at":"2020-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newClaudeRig(t, "claude-basic", Options{})
	if err := e.s.Start(t.Context()); err != nil {
		t.Fatalf("a start after the record expired: %v", err)
	}
	// Unreadable: refused, naming the file.
	if err := os.WriteFile(record, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := overageRefusal(); err == nil || ErrorInfo(err).Kind != errkind.Quota || !strings.Contains(err.Error(), record) {
		t.Fatalf("an unreadable record: %v; want a quota refusal naming it", err)
	}
}

// TestClaudeOverageRecordKeepsTheLaterUntil: a trip never shortens a record
// that holds longer, and a reset that is not in the future is an hour on.
func TestClaudeOverageRecordKeepsTheLaterUntil(t *testing.T) {
	record, _ := overageRig(t, 0)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	later := now.Add(48 * time.Hour)
	if _, _, err := writeOverageRecord(resolveOveragePaths(), overageRecord{Until: later, Window: "seven_day", At: now}); err != nil {
		t.Fatal(err)
	}
	rec := newOverageRecord(acp.RateLimit{RateLimitType: "five_hour", ResetsAt: float64(now.Add(time.Hour).Unix())}, now)
	if _, _, err := writeOverageRecord(resolveOveragePaths(), rec); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(t, record); !got.Until.Equal(later) || got.Window != "seven_day" {
		t.Fatalf("record %+v; want the later until kept", got)
	}
	for _, rl := range []acp.RateLimit{{}, {ResetsAt: float64(now.Add(-time.Minute).Unix())}} {
		if got := newOverageRecord(rl, now); !got.Until.Equal(now.Add(time.Hour)) {
			t.Fatalf("%+v: until %s; want an hour on", rl, got.Until)
		}
	}
	if got := newOverageRecord(acp.RateLimit{OverageResetsAt: float64(now.Add(3 * time.Hour).Unix())}, now); !got.Until.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("until %s; want overageResetsAt", got.Until)
	}
}

// TestClaudePromptErrorTrips (§ D3, § D4): a prompt's data.rateLimit that
// trips stops the session as a usage's would.
func TestClaudePromptErrorTrips(t *testing.T) {
	record, stopped := overageRig(t, 150*time.Millisecond)
	r, _ := startClaude(t, "claude-errors", Options{Interactive: true}, "CRAZE_FAKE_CLAUDE_OVERAGE=rejected")
	if _, err := r.s.Prompt(t.Context(), "ERR rate_limit TRIP"); !isQuota(err) {
		t.Fatalf("%v; want the guard's quota", err)
	}
	readRecord(t, record)
	waitStopped(t, r, stopped, 5*time.Second)
}

// TestClaudeErrorInfoTable (AC16): § D4's table, one row each.
func TestClaudeErrorInfoTable(t *testing.T) {
	rejected := `,"rateLimit":{"status":"rejected","overageStatus":"rejected"}`
	for _, tc := range []struct {
		code      int
		data, msg string
		kind      errkind.Kind
		retryable bool
		tabled    bool
	}{
		{-32603, `{"errorKind":"billing_error"}`, "", errkind.Quota, false, true},
		{-32603, `{"errorKind":"rate_limit"` + rejected + `}`, "", errkind.Quota, false, true},
		{-32603, `{"errorKind":"rate_limit","rateLimit":{"status":"allowed_warning"}}`, "", errkind.Provider, true, true},
		{-32603, `{"errorKind":"rate_limit"}`, "", errkind.Provider, true, true},
		{-32603, `{"errorKind":"authentication_failed"}`, "", errkind.Auth, false, true},
		{-32603, `{"errorKind":"oauth_org_not_allowed"}`, "", errkind.Auth, false, true},
		{-32603, `{"errorKind":"account_on_hold"}`, "", errkind.Auth, false, true},
		{-32603, `{"errorKind":"verification_required"}`, "", errkind.Auth, false, true},
		{-32603, `{"errorKind":"model_not_found"}`, "", errkind.ModelNotFound, false, true},
		{-32603, `{"errorKind":"invalid_request"}`, "Internal error: Prompt is too long: 201000 tokens > 200000 maximum", errkind.ContextTooLarge, false, true},
		{-32603, `{"errorKind":"invalid_request"}`, "Internal error: bad request", errkind.Provider, false, true},
		{-32603, `{"errorKind":"max_output_tokens"}`, "", errkind.Agent, false, true},
		{-32603, `{"errorKind":"overloaded"}`, "", errkind.Provider, true, true},
		{-32603, `{"errorKind":"server_error"}`, "", errkind.Provider, true, true},
		{-32603, `{"errorKind":"worker_shutdown"}`, "", errkind.Provider, false, true},
		{-32603, `{"errorKind":"cloud_credential_error"}`, "", errkind.Provider, false, true},
		{-32000, ``, "Authentication required", errkind.Auth, false, true},
		{-32603, ``, "Internal error", "", false, false},
		{-32602, `{"errorKind":"billing_error"}`, "", "", false, false},
	} {
		info, ok := ClaudeErrorInfo(tc.code, json.RawMessage(tc.data), tc.msg)
		if ok != tc.tabled || info.Kind != tc.kind || info.Retryable != tc.retryable {
			t.Errorf("%d %s %q: %+v %v; want %s retryable=%v tabled=%v", tc.code, tc.data, tc.msg, info, ok, tc.kind, tc.retryable, tc.tabled)
		}
	}
}

// TestClaudePromptErrorsAreClassified (AC16): the table at work on a turn's
// failure, end to end, and never on another provider's.
func TestClaudePromptErrorsAreClassified(t *testing.T) {
	_, _ = overageRig(t, 150*time.Millisecond)
	r, _ := startClaude(t, "claude-errors", Options{Interactive: true})
	for _, tc := range []struct {
		prompt    string
		kind      errkind.Kind
		retryable bool
	}{
		{"ERR billing_error", errkind.Quota, false},
		{"ERR rate_limit rejected", errkind.Quota, false},
		{"ERR rate_limit", errkind.Provider, true},
		{"ERR overloaded", errkind.Provider, true},
		{"ERR model_not_found", errkind.ModelNotFound, false},
		{"ERR invalid_request TOOLONG", errkind.ContextTooLarge, false},
		{"ERR authentication_failed", errkind.Auth, false},
		{"AUTH-REQUIRED", errkind.Auth, false},
	} {
		_, err := r.s.Prompt(t.Context(), tc.prompt)
		info := ErrorInfo(err)
		var rpc *acp.RPCError
		if err == nil || info.Kind != tc.kind || info.Retryable != tc.retryable || !errors.As(err, &rpc) {
			t.Fatalf("%s: %v %+v; want %s", tc.prompt, err, info, tc.kind)
		}
	}
	if r.s.fatalErr() != nil {
		t.Fatal("a rejected rate limit with extra usage off stopped the session; it is no trip")
	}
}

// TestClaudeUsageSection (AC13): the context meter, the turn's tokens
// unpriced, the plan, the window and its use, and the model in use.
func TestClaudeUsageSection(t *testing.T) {
	_, _ = overageRig(t, 150*time.Millisecond)
	r, log := startClaude(t, "claude-usage", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "count"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.Prompt(t.Context(), "count PLAN-SONNET"); err != nil {
		t.Fatal(err)
	}
	u := r.s.Snapshot().Usage
	if u == nil {
		t.Fatal("no usage section")
	}
	turn := Spend{Input: 4, Output: 164, CacheRead: 28511, CacheCreation: 30907, Unpriced: true}
	if u.ContextTokens != 30938 || u.ContextWindow != 1000000 || u.Turn != turn ||
		u.Session.Input != 8 || u.Session.CacheRead != 2*28511 || !u.Session.Unpriced || u.Session.CostPicoUSD != 0 {
		t.Fatalf("usage %+v", *u)
	}
	if u.Plan != "max" || u.Window != "seven_day" || u.WindowUtilization != 0.45 || u.ResetsAt.Before(time.Now().Add(71*time.Hour)) {
		t.Fatalf("plan fields %+v", *u)
	}
	if u.Model != "claude-sonnet-5-5" {
		t.Fatalf("model in use %q; want the last report's", u.Model)
	}
	if firstIndex(log.snapshot(), isUsageDelta) < 0 {
		t.Fatal("no usage delta published")
	}
}

// TestClaudeNoticeAndCompactionNotes (§ B2): a notice is an EventNotice, and
// a compaction its started and ended, with no sizes.
func TestClaudeNoticeAndCompactionNotes(t *testing.T) {
	_, _ = overageRig(t, 150*time.Millisecond)
	r, log := startClaude(t, "claude-usage", Options{Interactive: true})
	if _, err := r.s.Prompt(t.Context(), "NOTICE COMPACT"); err != nil {
		t.Fatal(err)
	}
	// The events are read off the session's stream by the collector: waited
	// for, not assumed there the moment the prompt returns.
	waitFor(t, "the notice and the compaction's ending", func() bool {
		evs := log.snapshot()
		return firstIndex(evs, func(ev Event) bool { return ev.Type == EventNotice }) >= 0 &&
			firstIndex(evs, func(ev Event) bool {
				return ev.Type == EventCompaction && ev.Compaction != nil && ev.Compaction.Phase == CompactionEnded
			}) >= 0
	})
	evs := log.snapshot()
	n := firstIndex(evs, func(ev Event) bool { return ev.Type == EventNotice })
	if n < 0 || *evs[n].Notice != (NoticeInfo{Severity: "warning", Title: "Stop hook says", Description: "remember the tests"}) {
		t.Fatalf("notice at %d", n)
	}
	var phases []string
	for _, ev := range evs {
		if ev.Type == EventCompaction && ev.Compaction != nil {
			phases = append(phases, ev.Compaction.Phase+"/"+ev.Compaction.Err)
			if ev.Compaction.TokensBefore != 0 || ev.Compaction.TokensAfter != 0 {
				t.Fatalf("sizes %+v", ev.Compaction)
			}
		}
	}
	if strings.Join(phases, ",") != "started/,ended/" {
		t.Fatalf("compaction phases %v", phases)
	}
}

// TestRecordIsNeverTheUsersInTests: every test of this package reads and
// writes a record of its own (TestMain).
func TestRecordIsNeverTheUsersInTests(t *testing.T) {
	path, fallback := overageSeams()
	for _, p := range []string{path, fallback} {
		if dir := filepath.Dir(p); !strings.Contains(dir, os.TempDir()) {
			t.Fatalf("the package's record is %s; want a temporary one", p)
		}
	}
}

// TestClaudeOverageRecordExpiresByTheClock: the record refuses until its until
// on the guard's clock, and nothing after.
func TestClaudeOverageRecordExpiresByTheClock(t *testing.T) {
	until := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	now := until.Add(-time.Minute)
	record := filepath.Join(t.TempDir(), "claude-overage.json")
	restore := SetClaudeOverageForTest(record, func() time.Time { return now }, 0)
	t.Cleanup(restore)
	if err := overageRefusal(); err != nil {
		t.Fatalf("no record: %v", err)
	}
	if _, _, err := writeOverageRecord(resolveOveragePaths(), overageRecord{Until: until, Window: "seven_day", At: now}); err != nil {
		t.Fatal(err)
	}
	if err := overageRefusal(); !isQuota(err) || !strings.Contains(err.Error(), "7-day") {
		t.Fatalf("a minute before until: %v", err)
	}
	now = until.Add(time.Second)
	if err := overageRefusal(); err != nil {
		t.Fatalf("after until: %v", err)
	}
}
