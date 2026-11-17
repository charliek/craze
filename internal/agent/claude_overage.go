package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/atomicfile"
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
	errRecordUnusable = errors.New("unreadable")
)

// defaultOverageFallback is where the trip record goes when the craze
// directory will not take it (review C5 item 3): a per-user directory in the
// system's temporary directory, under a name drawn from the record's own path,
// so each craze directory has one fallback and every check of it reads both.
func defaultOverageFallback(primary string) string {
	sum := sha256.Sum256([]byte(primary))
	return filepath.Join(os.TempDir(), fmt.Sprintf("craze-%d", os.Getuid()), "claude-overage-"+hex.EncodeToString(sum[:6])+".json")
}

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

// writeOverageRecord writes rec as the machine's trip record, first of
// everything a trip does. The craze directory's record is tried twice; when it
// will not take it, the fallback is written instead (review C5 item 3), which
// every check reads too, so the hold is machine-wide all the same. It answers
// where the record went, and the craze directory's failure when the fallback
// had to take it; err is set only when neither took it — the session that
// tripped still stops, but the hold is not recorded.
func writeOverageRecord(rec overageRecord) (where string, primaryErr, err error) {
	path, fallback, _ := overageSeams()
	if path != "" {
		if primaryErr = writeOverageRecordAt(path, rec); primaryErr != nil {
			primaryErr = writeOverageRecordAt(path, rec)
		}
		if primaryErr == nil {
			return path, nil, nil
		}
	} else {
		primaryErr = errors.New("craze has no directory to keep the trip record in")
	}
	if ferr := writeOverageRecordAt(fallback, rec); ferr != nil {
		return "", primaryErr, fmt.Errorf("%v; and the fallback %s: %v", primaryErr, fallback, ferr)
	}
	return fallback, primaryErr, nil
}

// testOverageAfterRead runs, when a test sets it, in writeOverageRecordAt
// between the read of the record there and the write that replaces it.
var testOverageAfterRead func(rec overageRecord)

// writeOverageRecordAt writes rec at path under an exclusive lock on its
// sibling path + ".lock" (review C5 item 2): the read, the comparison and the
// replacement are one transaction across every craze process on the machine,
// so a record that holds longer is never replaced by a shorter one — the later
// until, and its window, are kept.
func writeOverageRecordAt(path string, rec overageRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := atomicfile.LockWithin(path+".lock", overageLockWait)
	if err != nil {
		return err
	}
	defer unlock()
	if prev, err := readOverageRecord(path); err == nil && prev != nil && prev.Until.After(rec.Until) {
		rec.Until, rec.Window = prev.Until, prev.Window
	}
	if testOverageAfterRead != nil {
		testOverageAfterRead(rec)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, append(b, '\n'), 0o600)
}

// readOverageRecord is the record at path: nil for none, an error for one
// that is there and cannot be read or does not parse.
func readOverageRecord(path string) (*overageRecord, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		// Nothing there, or nothing could be: a path through a file.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec overageRecord
	if err := json.Unmarshal(b, &rec); err != nil || rec.Until.IsZero() {
		return nil, errRecordUnusable
	}
	return &rec, nil
}

// overageRefusal is the trip record's word before a claude start or prompt
// (§ D3): the quota refusal while it — the craze directory's, or its fallback
// (review C5 item 3), whichever holds longer — holds, nil once both have
// expired or when there are none. A record that is there and cannot be read
// refuses too, and says which file: the guard fails closed.
func overageRefusal() error {
	path, fallback, now := overageSeams()
	var held *overageRecord
	heldAt := ""
	for _, p := range []string{path, fallback} {
		if p == "" {
			continue
		}
		rec, err := readOverageRecord(p)
		if err != nil {
			return claudeErr(errkind.Quota, "craze cannot read its Claude overage record %s (%v); it refuses Claude sessions until the file is fixed or removed", p, err)
		}
		if rec != nil && rec.Until.After(now) && (held == nil || rec.Until.After(held.Until)) {
			held, heldAt = rec, p
		}
	}
	if held == nil {
		return nil
	}
	return claudeErr(errkind.Quota, "%s. craze refuses Claude sessions on this machine until then (%s)", overageWords(held.Window, held.Until), heldAt)
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
	where, primaryErr, werr := writeOverageRecord(rec)
	err := overageTripErr(rec)
	if werr != nil {
		err = claudeErr(errkind.Quota, "%s. craze stopped this session; the request in flight may already have been billed. craze could not record the hold (%s), so other Claude sessions on this machine are not refused", overageWords(rec.Window, rec.Until), sanitizeLine(werr.Error()))
	}
	stopped := s.containOverage(err)
	fields := map[string]any{"window": rec.Window, "until": rec.Until.Format(time.RFC3339), "path": where,
		"isUsingOverage": rl.IsUsingOverage, "overageInUse": rl.OverageInUse, "status": rl.Status, "overageStatus": rl.OverageStatus}
	if primaryErr != nil {
		fields["primaryError"] = primaryErr.Error()
	}
	if werr != nil {
		fields["recordError"] = werr.Error()
	}
	s.log.Note(journal.DiagNote{Kind: "claude_overage_trip", Fields: fields})
	if primaryErr != nil && werr == nil {
		s.diagLine(fmt.Sprintf("craze: the Claude overage record went to %s (%s)", where, sanitizeLine(primaryErr.Error())))
	}
	return stopped
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
	if !f.Running || !s.provider().isClaude() {
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
