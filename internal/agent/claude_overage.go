package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/journal"
	"github.com/charliek/craze/internal/paths"
)

// The claude overage guard (plan 052 § D3): containment, not prevention.
// claude-agent-acp says a request was billed as extra usage — or that the
// next one would be — only once it has happened, so the guard cannot stop the
// request that tells it; it ends whatever turn is running with quota, stops
// the session as a whole (session/cancel, a grace, then the adapter's process
// group killed, every child with it), and writes a machine-wide trip record
// that refuses every claude start and prompt until the limit resets. The only
// hard guarantee is extra usage turned off on the Claude account, which the
// docs recommend.
//
// The order on a trip is fixed: the record first, then the turn, then the
// stop. The record is what a new process reads — the adapter sends the rate
// limit only when it changes, so a session started after the trip would
// never be told.

// overageRecord is the trip record's content, `{until, window, at}`.
type overageRecord struct {
	// Until is when the limit resets: resetsAt, else overageResetsAt, else
	// an hour after the trip.
	Until time.Time `json:"until"`
	// Window is the rate limit's type as the adapter named it.
	Window string `json:"window"`
	// At is when the trip was recorded.
	At time.Time `json:"at"`
}

// The guard's seams. Production reads the craze directory's record (and its
// fallback beside the system's temporary directory), the system clock and the
// plan's 2 s; a test points them elsewhere (SetClaudeOverageForTest) — and
// every test in this package does (TestMain), so none can ever touch the
// user's own record.
var (
	overageMu         sync.Mutex
	overagePath       = paths.ClaudeOveragePath
	overageFallback   = defaultOverageFallback
	overageNow        = time.Now
	overageStopGrace  atomic.Int64
	defaultStopGrace  = 2 * time.Second
	overageNoRecord   = time.Hour
	overageLockWait   = 5 * time.Second
	overageUID        = os.Getuid
	errRecordUnusable = errors.New("unreadable")
)

// SetClaudeOverageForTest points the overage guard at a record path of the
// test's (its fallback beside it, path + ".fallback"), a clock of its own (nil
// keeps the system's) and a stop grace (0 keeps the plan's 2 s), and returns
// the restore. It lives outside a _test.go file because other packages' tests
// start claude sessions too; production code never calls it.
func SetClaudeOverageForTest(path string, now func() time.Time, grace time.Duration) (restore func()) {
	overageMu.Lock()
	prevPath, prevFallback, prevNow := overagePath, overageFallback, overageNow
	overagePath = func() string { return path }
	overageFallback = func(string) string { return path + ".fallback" }
	if now != nil {
		overageNow = now
	}
	overageMu.Unlock()
	prevGrace := overageStopGrace.Swap(int64(grace))
	return func() {
		overageMu.Lock()
		overagePath, overageFallback, overageNow = prevPath, prevFallback, prevNow
		overageMu.Unlock()
		overageStopGrace.Store(prevGrace)
	}
}

// overageSeams is the record's path, its fallback's and now.
func overageSeams() (path, fallback string, now time.Time) {
	overageMu.Lock()
	defer overageMu.Unlock()
	path = overagePath()
	return path, overageFallback(path), overageNow()
}

// epochTime is an epoch-seconds field of the adapter's as a time, the zero
// time for none.
func epochTime(sec float64) time.Time {
	if sec <= 0 || math.IsNaN(sec) || math.IsInf(sec, 0) {
		return time.Time{}
	}
	whole, frac := math.Modf(sec)
	return time.Unix(int64(whole), int64(frac*1e9))
}

// newOverageRecord is the record of a trip on rl at now. A reset that is not
// in the future — none was sent, or a stale one — is an hour on, so the
// record always holds for a while.
func newOverageRecord(rl acp.RateLimit, now time.Time) overageRecord {
	until := epochTime(rl.ResetsAt)
	if until.IsZero() {
		until = epochTime(rl.OverageResetsAt)
	}
	if !until.After(now) {
		until = now.Add(overageNoRecord)
	}
	return overageRecord{Until: until.UTC(), Window: rl.RateLimitType, At: now.UTC()}
}

// overageWords is the guard's opening sentence for a limit of window that
// resets at until.
func overageWords(window string, until time.Time) string {
	return fmt.Sprintf("Claude would bill this as extra usage: your plan's %s limit is reached (resets %s)",
		windowWords(window), until.Local().Format("Mon 2 Jan 15:04 MST"))
}

// windowWords names a rate limit's window in the guard's message.
func windowWords(window string) string {
	switch window {
	case "five_hour":
		return "5-hour"
	case "seven_day", "seven_day_overage_included":
		return "7-day"
	case "seven_day_opus":
		return "7-day Opus"
	case "seven_day_sonnet":
		return "7-day Sonnet"
	}
	return "usage"
}

// overageTripErr is the error a trip ends the running turn and the session
// with (§ D3's message).
func overageTripErr(rec overageRecord) error {
	return claudeErr(errkind.Quota, "%s. craze stopped this session; the request in flight may already have been billed", overageWords(rec.Window, rec.Until))
}

func stopGrace() time.Duration { return durationOr(&overageStopGrace, defaultStopGrace) }

// tripOverage is a trip this session saw, on the wire (a usage_update's rate
// limit, the read loop) or in a prompt's error: the record written first,
// then the session contained, and only then anything that can block — a
// diagnostic, the journal, an event (review C5 item 1). A trip while the
// session is already stopping still writes its record, so a later reset it
// carries extends the hold (review C5 item 4). It answers the error the
// running turn ends with: the first trip's.
func (s *session) tripOverage(rl acp.RateLimit) error {
	_, _, now := overageSeams()
	rec := newOverageRecord(rl, now)
	s.mu.Lock()
	if hasReset(rl, now) {
		// A reset of its own: the record says it.
	} else if !s.tripUntil.IsZero() {
		// A reset-less trip handled again (review C5b item 11): the hold the
		// first one computed, not a fresh hour.
		rec.Until = s.tripUntil
	} else {
		s.tripUntil = rec.Until
	}
	s.mu.Unlock()
	// The write starts first, on a goroutine of its own, and the stop waits
	// for it at most overageWriteBound (review C5b item 1): a filesystem that
	// stalls never holds the containment.
	type written struct {
		where           string
		primaryErr, err error
	}
	done := make(chan written, 1)
	go func() {
		where, primaryErr, err := writeOverageRecord(rec)
		done <- written{where, primaryErr, err}
	}()
	var w *written
	t := time.NewTimer(overageWriteBound)
	select {
	case got := <-done:
		w = &got
	case <-t.C:
	}
	t.Stop()
	err := overageTripErr(rec)
	if w != nil && w.err != nil {
		err = claudeErr(errkind.Quota, "%s. craze stopped this session; the request in flight may already have been billed. craze could not record the hold (%s), so other Claude sessions on this machine are not refused", overageWords(rec.Window, rec.Until), sanitizeLine(w.err.Error()))
	}
	stopped := s.containOverage(err)
	report := func(w written, late bool) {
		fields := map[string]any{"window": rec.Window, "until": rec.Until.Format(time.RFC3339), "path": w.where,
			"isUsingOverage": rl.IsUsingOverage, "overageInUse": rl.OverageInUse, "status": rl.Status, "overageStatus": rl.OverageStatus}
		if late {
			fields["late"] = true
		}
		if w.primaryErr != nil {
			fields["primaryError"] = w.primaryErr.Error()
		}
		if w.err != nil {
			fields["recordError"] = w.err.Error()
		}
		s.log.Note(journal.DiagNote{Kind: "claude_overage_trip", Fields: fields})
		switch {
		case w.err != nil && late:
			s.fallbackLine(fmt.Sprintf("craze: the Claude overage record could not be written (%s)", sanitizeLine(w.err.Error())))
		case w.primaryErr != nil && w.err == nil:
			s.fallbackLine(fmt.Sprintf("craze: the Claude overage record went to %s (%s)", w.where, sanitizeLine(w.primaryErr.Error())))
		}
	}
	if w != nil {
		report(*w, false)
	} else {
		// Still writing: said once it has, whenever that is.
		go func() { report(<-done, true) }()
	}
	return stopped
}

// hasReset reports whether rl names a reset still to come.
func hasReset(rl acp.RateLimit, now time.Time) bool {
	return epochTime(rl.ResetsAt).After(now) || (epochTime(rl.ResetsAt).IsZero() && epochTime(rl.OverageResetsAt).After(now))
}

// fallbackLine is diagLine for the record's whereabouts, coalesced (review
// C5b item 10): one such line in flight at a time, the rest dropped.
func (s *session) fallbackLine(line string) {
	w := diagWriter(s.opts)
	if w == nil || !s.recordLineBusy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.recordLineBusy.Store(false)
		fmt.Fprintln(w, line)
	}()
}

// diagLine writes one line to the diagnostic lane off the caller's goroutine:
// a sink that blocks never holds the read loop or the overage stop (review C5
// item 1).
func (s *session) diagLine(line string) {
	w := diagWriter(s.opts)
	if w == nil {
		return
	}
	go func() { fmt.Fprintln(w, line) }()
}

// containOverage stops the session for err, once (§ D3, R2-2): nothing more
// is written to the agent but a cancel (the fence goes up in the same locked
// section that records the error, and before it, so a prompt past its gate is
// refused at the write); the turn running ends with err — a prompt's when it
// returns, a foreign turn's at once (the caller's, or the client's on a wire
// trip); and the agent is stopped: session/cancel, the grace, then its
// process group and the descendants that left it killed, nothing it sends
// after that read. Then the session stops for real (review C5 item 8): an idle
// session says why with an error event, and the host it runs in is asked to
// stop (Options.Stopped), so a later `craze -c` loads it in a fresh adapter.
// Nothing here waits on a reader before the stop has begun. It answers the
// error the session stopped with.
func (s *session) containOverage(err error) error {
	s.mu.Lock()
	if s.overage != nil {
		prev := s.overage
		s.mu.Unlock()
		return prev
	}
	s.overage = err
	client := s.client
	if client != nil {
		client.Fence()
	}
	if s.gate.fatal == nil {
		s.gate.fatal = err
	}
	idle := !s.inPrompt && !s.claimed && !s.foreign
	s.mu.Unlock()
	if client != nil {
		// A foreign turn still open when the client closes ends with it too.
		client.FailForeign(err)
		grace := stopGrace()
		go func() {
			client.StopWithin(grace)
			s.afterOverageStop(err, idle)
		}()
	}
	s.diagLine("craze: " + sanitizeLine(err.Error()))
	return err
}

// afterOverageStop is the session's end once the agent is gone (review C5
// item 8): an idle session's error event — a running turn ends with err
// already — and the host's stop.
func (s *session) afterOverageStop(err error, idle bool) {
	if testOverageStopped != nil {
		testOverageStopped()
	}
	if idle {
		s.emit(Event{Type: EventError, Err: err})
	}
	if stop := s.opts.Stopped; stop != nil {
		stop(StopCauseOverage, err.Error())
	}
}

// testOverageStopped runs, when a test sets it, once the overage stop has
// closed the client.
var testOverageStopped func()

// onClaudeRateLimit is the client's rate-limit handler (read loop, before the
// foreign-turn machine reads the update): the guard's trip first, then the
// plan fields of the usage section, whose publication can wait on a reader
// (review C5 item 1). Its answer is the error a trip ends the running foreign
// turn with.
func (s *session) onClaudeRateLimit(rl acp.RateLimit) error {
	var err error
	if rl.Trips() {
		err = s.tripOverage(rl)
	}
	s.noteRateLimit(rl)
	return err
}

// guardForeignStart is the trip record's word on a foreign turn just opened
// (§ D3): a turn the agent started on its own while the record holds is a
// model call on an account that bills extra usage, so it ends with the
// refusal and the session is stopped, as for a trip of its own.
func (s *session) guardForeignStart(f acp.ForeignTurn) {
	if !f.Running || !s.provider().isClaude() || s.replaying.Load() {
		return
	}
	refusal := overageRefusal()
	if refusal == nil {
		return
	}
	s.log.Note(journal.DiagNote{Kind: "claude_overage_held", Fields: map[string]any{"turn": f.ID}})
	err := s.containOverage(refusal)
	if c := s.clientRef(); c != nil {
		c.EndForeignWith(err)
	}
}

// promptGate is the ACP client's last check before a prompt's or a steer's
// bytes go out (SetPromptGate): claude's fast reset and fatal stop
// (awaitFastFix), then the trip record (§ D3) — a record another session
// wrote refuses this prompt with quota too.
func (s *session) promptGate(ctx context.Context) error {
	if err := s.awaitFastFix(ctx); err != nil {
		return err
	}
	if !s.provider().isClaude() {
		return nil
	}
	return overageRefusal()
}

// claudePromptTrip is the guard's read of a claude prompt's error (§ D3, §
// D4): `data.rateLimit` that trips stops the session as a usage_update's
// would, and the turn ends with the trip's error.
func (s *session) claudePromptTrip(err error) error {
	var rpc *acp.RPCError
	if !s.provider().isClaude() || !errors.As(err, &rpc) || rpc == nil {
		return nil
	}
	rl, ok := acp.ErrorRateLimit(rpc.Data)
	if !ok {
		return nil
	}
	if !rl.Trips() {
		s.noteRateLimit(rl)
		return nil
	}
	// The trip before anything that can wait on a reader (review C5 item 1).
	trip := s.tripOverage(rl)
	if c := s.clientRef(); c != nil {
		c.EndForeignWith(trip)
	}
	s.noteRateLimit(rl)
	return trip
}

// StoppedErr is agent.StopErrer: the overage guard's error once it has
// stopped the session, nil until then.
func (s *session) StoppedErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overage
}
