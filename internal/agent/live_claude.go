package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/journal"
)

// The claude session's own steps (plan 052 § A4, § B3, § C2, § C4, § D3): the
// start mode, the setup request's params, the effective-login gate, the
// validation of what the adapter applied, and fast mode kept off.

// claudeGate is the effective-login gate's state on a session (§ A4, R2-1),
// under s.mu: whether the adapter's first _auth/status_update has come
// (seen, and first closed with it) and its verdict; and fatal, a later push
// that failed the gate, which ends the running turn and every prompt after.
type claudeGate struct {
	first chan struct{}
	seen  bool
	err   error
	fatal error
}

// claudeStartMode is the mode a claude session starts or loads in (§ C2):
// Manual, or plan under --plan. A mode the adapter never has — ask — fails
// the start before anything is spawned, in the words /ask is refused with.
func (s *session) claudeStartMode() (string, error) {
	if s.opts.Mode == "" {
		return claudeStartModes[0], nil
	}
	id, ok := ResolveMode(s.opts.Mode, claudeStartModes)
	if !ok {
		return "", claudeErr(errkind.Config, "mode %s is not advertised", sanitizeLine(s.opts.Mode))
	}
	return id, nil
}

// claudeStartRefusal is what a claude start refuses before the spawn: a mode
// it has none of, and --fast (§ D3).
func (s *session) claudeStartRefusal() error {
	if _, err := s.claudeStartMode(); err != nil {
		return err
	}
	if s.opts.Fast != nil && *s.opts.Fast {
		return claudeErr(errkind.Usage, "--fast: %s", FastBillsExtra)
	}
	return nil
}

// unattendedDisallowedTools are the session-bound tools an unattended claude
// session is started without (plan 052 § B3, § E3): each schedules work for
// later, and the host that would run it exits with the turn.
var unattendedDisallowedTools = []string{"ScheduleWakeup", "Monitor", "CronCreate"}

// sessionParams is what session/new or session/load carries for this session
// (§ B3). Cursor and grok carry nothing. Claude carries its start mode and
// fast off; a load also carries the model its index row recorded unless
// --model names one (§ C4), and an unattended session disallows the tools
// that outlive its host.
func (s *session) sessionParams(loading bool) acp.SessionParams {
	p := s.provider()
	if !p.isClaude() {
		return acp.SessionParams{}
	}
	mode, _ := s.claudeStartMode()
	sp := acp.SessionParams{ConfigOptions: []acp.ConfigOptionValue{{ConfigID: "mode", Value: mode}}}
	if loading && s.opts.Model == "" && strings.TrimSpace(s.opts.LoadModel) != "" {
		sp.ConfigOptions = append(sp.ConfigOptions, acp.ConfigOptionValue{ConfigID: "model", Value: strings.TrimSpace(s.opts.LoadModel)})
	}
	sp.ConfigOptions = append(sp.ConfigOptions, acp.ConfigOptionValue{ConfigID: "fast", Value: "off"})
	if s.opts.Unattended {
		sp.Meta = map[string]any{"claudeCode": map[string]any{"options": map[string]any{
			"disallowedTools": append([]string(nil), unattendedDisallowedTools...),
		}}}
	}
	return sp
}

// onAuthStatus is the client's auth handler (read loop). The first push
// settles the start's wait; a later one that fails the gate is fatal: the
// session stops, ending the turn in flight with that error and refusing every
// prompt after it (§ A4: an apiKeyHelper classified late).
func (s *session) onAuthStatus(st acp.AuthStatus) {
	if !s.provider().isClaude() {
		return
	}
	verdict := claudeLoginRefusal(st)
	s.mu.Lock()
	if !s.gate.seen {
		s.gate.seen = true
		s.gate.err = verdict
		close(s.gate.first)
		s.mu.Unlock()
		return
	}
	if verdict == nil || s.gate.fatal != nil {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.log.Note(journal.DiagNote{Kind: "claude_login_refused", Fields: map[string]any{"kind": st.Kind}})
	s.stopFor(verdict)
}

// stopFor stops the session for err, once: it is the session's fatal error
// from here (the turn in flight ends with it, every later prompt is refused
// with it) and the agent is closed, which ends its process group. The login
// gate stops a session so with auth, and fast mode that will not go off with
// config. The close runs off the caller's goroutine, which may be the read
// loop: Close waits for that to end.
func (s *session) stopFor(err error) {
	s.mu.Lock()
	if s.gate.fatal != nil {
		s.mu.Unlock()
		return
	}
	s.gate.fatal = err
	client := s.client
	s.mu.Unlock()
	if client != nil {
		go func() { _ = client.Close() }()
	}
}

// awaitLogin is the gate's wait after initialize (§ A4): the adapter's first
// _auth/status_update within loginStatusWait, and its verdict. None at all
// fails the start closed — retryable, since a slow probe may answer next
// time.
func (s *session) awaitLogin(ctx context.Context) error {
	wait := durationOr(&loginStatusWait, defaultLoginStatusWait)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-s.gate.first:
	case <-timer.C:
		return errNoLoginStatus()
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return fmt.Errorf("agent: session closed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gate.err
}

// fatalErr is the gate's later refusal, nil while the login holds.
func (s *session) fatalErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gate.fatal
}

// configOptionByID is the first option of cfg with id, nil for none.
func configOptionByID(cfg []ConfigOption, id string) *ConfigOption {
	for i := range cfg {
		if cfg[i].ID == id {
			opt := cfg[i]
			return &opt
		}
	}
	return nil
}

// claudeEffectiveMode is the mode the adapter says the session is in: its
// mode option's value, else the modes' current id.
func claudeEffectiveMode(cfg []ConfigOption, current string) string {
	if opt := configOptionByID(cfg, "mode"); opt != nil && opt.Current != "" {
		return opt.Current
	}
	return current
}

// claudeSettle is the validation after a claude session/new or session/load
// (§ B3, § C4, § D3): the adapter skips an initial config option it refuses
// without a word, so what it applied is read back. A mode that differs is set
// once more and fails the start if it still differs; fast mode on is set off
// and fails the start if it stays on; a recorded model the adapter did not
// take leaves a note, and the session runs on the adapter's. snap is the new
// session's snapshot, not yet installed; on a load, whose snapshot is
// installed and published already, it is not read.
func (s *session) claudeSettle(ctx context.Context, client *acp.Client, loading bool, snap *Snapshot) error {
	read := func() Snapshot {
		if loading {
			return s.Snapshot()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		// The catalog and the model session/new answered, unless an update of
		// the agent's own has written them since (startOption's priming): a
		// retry's reply is installed against both, and a model left empty here
		// would be copied back over the new session's own (review F2).
		if !s.agentWrote.config {
			s.snap.Config = cloneConfig(snap.Config)
		}
		if !s.agentWrote.model {
			s.snap.CurrentModel = snap.CurrentModel
		}
		out := *snap
		out.Config = cloneConfig(s.snap.Config)
		return out
	}
	want, _ := s.claudeStartMode()
	cur := read()
	if got := claudeEffectiveMode(cur.Config, cur.CurrentMode); got != want {
		opt := configOptionByID(cur.Config, "mode")
		if opt == nil {
			return claudeErr(errkind.Config, "Claude Code offers no mode option to start this session in %s", want)
		}
		if err := s.startSet(ctx, client, loading, snap, opt, want, "the start mode "+want); err != nil && !refusedByAgent(err) {
			return err
		}
		cur = read()
		if got := claudeEffectiveMode(cur.Config, cur.CurrentMode); got != want {
			return claudeErr(errkind.Config, "Claude Code kept this session in mode %s where craze starts it in %s", sanitizeLine(got), want)
		}
		s.settleMode(loading, snap, want)
	}
	cur = read()
	if fast := FastOption(cur); fast != nil {
		off, on, ok := FastOnOff(fast)
		if ok && fast.Current == on {
			if err := s.startSet(ctx, client, loading, snap, fast, off, "fast off"); err != nil && !refusedByAgent(err) {
				return err
			}
			if FastOn(read()) {
				return claudeErr(errkind.Config, "Claude Code kept fast mode on, and %s", FastBillsExtra)
			}
		}
	}
	if loading && s.opts.Model == "" {
		s.noteRecordedModel(read())
	}
	return nil
}

// settleMode writes a start mode the agent has now taken into the session:
// into the new session's snapshot, which its install publishes, or on a load —
// whose snapshot has already been published — through s.mu, and says so in
// the same section, or a client that folds the stream would keep the mode the
// replay left while Snapshot answers this one, for ever (r23 finding 2).
// Event.Mode stays empty: it is an *agent*-initiated update's field, and this
// is craze's own start mode.
func (s *session) settleMode(loading bool, snap *Snapshot, mode string) {
	if !loading {
		snap.CurrentMode = mode
		return
	}
	s.mu.Lock()
	s.snap.CurrentMode = mode
	s.enqueueDeltaLocked("", Event{}, s.owedModelLocked(&StateDelta{Mode: &mode}))
	s.mu.Unlock()
}

// noteRecordedModel is the load's model check (§ C4): the model the index row
// recorded, sent in session/load, and the one the adapter came back on. A
// model it refused or no longer offers keeps the adapter's, and the session
// says which two.
func (s *session) noteRecordedModel(cur Snapshot) {
	want := strings.TrimSpace(s.opts.LoadModel)
	got := cur.CurrentModel
	if opt := ModelConfigOptionIn(cur.Config); opt != nil && opt.Current != "" {
		got = opt.Current
	}
	if want == "" || got == want {
		return
	}
	s.log.Note(journal.DiagNote{Kind: "recorded_model_refused", Fields: map[string]any{"recorded": want, "model": got}})
	if w := diagWriter(s.opts); w != nil {
		fmt.Fprintf(w, "craze: the session's recorded model %s was not applied; it runs on %s\n", sanitizeLine(want), sanitizeLine(got))
	}
}

// fastFixAttempts bounds the sets of one reset of claude's fast mode: an
// agent that answers each with fast still on is not asked for ever.
const fastFixAttempts = 3

// claudeFastOn reports whether cfg has claude's fast mode on.
func (s *session) claudeFastOn(cfg []ConfigOption) bool {
	return s.provider().RefusesFast() && FastOn(Snapshot{Provider: s.provider().Info(), Config: cfg})
}

// keepFastOffLocked answers a config_option_update that turned claude's fast
// mode on by setting it off again (§ D3). It runs on the read loop, so the
// reset runs on a goroutine of its own (fixFast), one at a time. An update
// that arrives while a reset runs needs nothing more: the reset reads the
// session's catalog again after each set, so it sees it. One that arrives
// before the session/new install, or in a load's replay, is left to the
// start's own check after the install (ensureFastOff). s.mu is held.
func (s *session) keepFastOffLocked(cfg []ConfigOption) {
	if s.client == nil || s.installPending || s.replaying.Load() || s.fastFix != nil || !s.claudeFastOn(cfg) {
		return
	}
	s.fastFix = make(chan struct{})
	go s.fixFast(s.fastFix)
}

// fixFast is one reset of claude's fast mode (review F1): set off, then the
// session's catalog read again — the reply's install, and anything the agent
// pushed since — until it says off, at most fastFixAttempts sets. The last
// read and the end of the reset are one locked section, so an update after
// it starts a reset of its own. A set that fails, or fast still on after the
// last one, stops the session with config: fast mode bills as extra usage,
// and no turn may run with it on. done is closed last, so a prompt waiting
// on it (awaitFastFix) finds the session stopped.
func (s *session) fixFast(done chan struct{}) {
	var fail error
	for attempt := 0; ; attempt++ {
		s.mu.Lock()
		fast := FastOption(Snapshot{Provider: s.provider().Info(), Config: s.snap.Config})
		if !s.claudeFastOn(s.snap.Config) {
			s.fastFix = nil
			s.mu.Unlock()
			close(done)
			return
		}
		s.mu.Unlock()
		off, _, _ := FastOnOff(fast)
		if attempt == fastFixAttempts || off == "" {
			fail = claudeErr(errkind.Config, "Claude Code kept fast mode on, and %s", FastBillsExtra)
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), startSettingWait)
		_, err := s.SetConfig(ctx, "", fast.ID, off, "")
		cancel()
		if err != nil && !errors.Is(err, ErrOptionGone) {
			fail = claudeErr(errkind.Config, "craze could not turn Claude Code's fast mode off (%s), and %s", sanitizeLine(err.Error()), FastBillsExtra)
			break
		}
	}
	s.log.Note(journal.DiagNote{Kind: "fast_off_failed", Fields: map[string]any{"error": fail.Error()}})
	s.stopFor(fail)
	s.mu.Lock()
	s.fastFix = nil
	s.mu.Unlock()
	close(done)
}

// awaitFastFix waits for a reset of claude's fast mode in flight, and answers
// the session's fatal error: the reset's, when it stopped the session.
func (s *session) awaitFastFix(ctx context.Context) error {
	s.mu.Lock()
	done := s.fastFix
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return fmt.Errorf("agent: session closed")
		}
	}
	return s.fatalErr()
}

// ensureFastOff is the start's last check of claude's fast mode (review F1):
// on in the catalog the start leaves — an update the install kept, or a
// load's replay — it is reset now, and the start waits for it. A reset that
// cannot turn it off fails the start with config.
func (s *session) ensureFastOff(ctx context.Context) error {
	s.mu.Lock()
	if s.fastFix == nil && s.client != nil && s.claudeFastOn(s.snap.Config) {
		s.fastFix = make(chan struct{})
		go s.fixFast(s.fastFix)
	}
	s.mu.Unlock()
	return s.awaitFastFix(ctx)
}

// refusedStopReason is a claude turn's stop reason that is not an ending
// (plan 052 § B8): the model refused, ran out of output tokens, or the agent
// capped its model requests. Such a turn ends as an agent error naming it.
func refusedStopReason(p Provider, stop string) error {
	if !p.isClaude() {
		return nil
	}
	var why string
	switch stop {
	case acp.StopRefusal:
		why = "the model refused to continue"
	case acp.StopMaxTokens:
		why = "the model reached its output token limit"
	case acp.StopMaxTurnRequests:
		why = "the turn reached its limit of model requests"
	default:
		return nil
	}
	return &stopReasonError{reason: stop, why: why}
}

// stopReasonError is a turn that stopped short of its end (§ B8).
type stopReasonError struct{ reason, why string }

func (e *stopReasonError) Error() string {
	return fmt.Sprintf("agent: the turn stopped (%s): %s", e.reason, e.why)
}
func (e *stopReasonError) ErrorInfo() errkind.Info { return errkind.New(errkind.Agent) }
