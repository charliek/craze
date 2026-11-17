package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

// The guard's seams. Production reads the craze directory's record, the
// system clock and the plan's 2 s; a test points them elsewhere
// (SetClaudeOverageForTest) — and every test in this package does
// (TestMain), so none can ever touch the user's own record.
var (
	overageMu         sync.Mutex
	overagePath       = paths.ClaudeOveragePath
	overageNow        = time.Now
	overageStopGrace  atomic.Int64
	defaultStopGrace  = 2 * time.Second
	overageNoRecord   = time.Hour
	errRecordUnusable = errors.New("unreadable")
)

// SetClaudeOverageForTest points the overage guard at a record path of the
// test's, a clock of its own (nil keeps the system's) and a stop grace (0
// keeps the plan's 2 s), and returns the restore. It lives outside a _test.go
// file because other packages' tests start claude sessions too; production
// code never calls it.
func SetClaudeOverageForTest(path string, now func() time.Time, grace time.Duration) (restore func()) {
	overageMu.Lock()
	prevPath, prevNow := overagePath, overageNow
	overagePath = func() string { return path }
	if now != nil {
		overageNow = now
	}
	overageMu.Unlock()
	prevGrace := overageStopGrace.Swap(int64(grace))
	return func() {
		overageMu.Lock()
		overagePath, overageNow = prevPath, prevNow
		overageMu.Unlock()
		overageStopGrace.Store(prevGrace)
	}
}

func overageSeams() (path string, now time.Time) {
	overageMu.Lock()
	defer overageMu.Unlock()
	return overagePath(), overageNow()
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
// everything a trip does. A record already there that holds longer keeps its
// later until. It answers the path written ("" when craze has no directory to
// keep one in) and why it could not be written.
func writeOverageRecord(rec overageRecord) (string, error) {
	overageMu.Lock()
	defer overageMu.Unlock()
	path := overagePath()
	if path == "" {
		return "", errors.New("craze has no directory to keep the trip record in")
	}
	if prev, err := readOverageRecord(path); err == nil && prev != nil && prev.Until.After(rec.Until) {
		rec.Until = prev.Until
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return path, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, err
	}
	return path, atomicfile.Write(path, append(b, '\n'), 0o600)
}

// readOverageRecord is the record at path: nil for none, an error for one
// that is there and cannot be read or does not parse.
func readOverageRecord(path string) (*overageRecord, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
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
// (§ D3): the quota refusal while it holds, nil once it has expired or when
// there is none. A record that is there and cannot be read refuses too, and
// says which file: the guard fails closed.
func overageRefusal() error {
	path, now := overageSeams()
	if path == "" {
		return nil
	}
	rec, err := readOverageRecord(path)
	if err != nil {
		return claudeErr(errkind.Quota, "craze cannot read its Claude overage record %s (%v); it refuses Claude sessions until the file is fixed or removed", path, err)
	}
	if rec == nil || !rec.Until.After(now) {
		return nil
	}
	return claudeErr(errkind.Quota, "%s. craze refuses Claude sessions on this machine until then (%s)", overageWords(rec.Window, rec.Until), path)
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
// then the session contained. It answers the error the running turn ends
// with. A session already stopping answers the error it stopped with.
func (s *session) tripOverage(rl acp.RateLimit) error {
	s.mu.Lock()
	prev := s.overage
	s.mu.Unlock()
	if prev != nil {
		return prev
	}
	_, now := overageSeams()
	rec := newOverageRecord(rl, now)
	path, werr := writeOverageRecord(rec)
	fields := map[string]any{"window": rec.Window, "until": rec.Until.Format(time.RFC3339), "path": path,
		"isUsingOverage": rl.IsUsingOverage, "overageInUse": rl.OverageInUse, "status": rl.Status, "overageStatus": rl.OverageStatus}
	if werr != nil {
		fields["recordError"] = werr.Error()
	}
	s.log.Note(journal.DiagNote{Kind: "claude_overage_trip", Fields: fields})
	if werr != nil {
		if w := diagWriter(s.opts); w != nil {
			fmt.Fprintf(w, "craze: the Claude overage record could not be written (%s)\n", sanitizeLine(werr.Error()))
		}
	}
	err := overageTripErr(rec)
	return s.containOverage(err)
}

// containOverage stops the session for err, once (§ D3, R2-2): nothing more
// is written to the agent but a cancel (the fence goes up in the same locked
// section that records the error, and before it, so a prompt past its gate is
// refused at the write); the turn running ends with err — a prompt's when it
// returns, a foreign turn's at once (the caller's, or the client's on a wire
// trip); and the agent is stopped: session/cancel, the grace, then its
// process group killed, nothing it sends after that read. It answers the error
// the session stopped with.
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
	s.mu.Unlock()
	if w := diagWriter(s.opts); w != nil {
		fmt.Fprintf(w, "craze: %s\n", sanitizeLine(err.Error()))
	}
	if client == nil {
		return err
	}
	// A foreign turn still open when the client closes ends with it too.
	client.FailForeign(err)
	grace := stopGrace()
	go func() {
		client.StopWithin(grace)
		if testOverageStopped != nil {
			testOverageStopped()
		}
	}()
	return err
}

// testOverageStopped runs, when a test sets it, once the overage stop has
// closed the client.
var testOverageStopped func()

// onClaudeRateLimit is the client's rate-limit handler (read loop, before the
// foreign-turn machine reads the update): the plan fields of the usage
// section, and the guard's trip. Its answer is the error a trip ends the
// running foreign turn with.
func (s *session) onClaudeRateLimit(rl acp.RateLimit) error {
	s.noteRateLimit(rl)
	if !rl.Trips() {
		return nil
	}
	return s.tripOverage(rl)
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
	s.noteRateLimit(rl)
	if !rl.Trips() {
		return nil
	}
	trip := s.tripOverage(rl)
	if c := s.clientRef(); c != nil {
		c.EndForeignWith(trip)
	}
	return trip
}
