package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/transcript"
)

const (
	defaultFrameTimeout = 10 * time.Second
	frameQueueMax       = 8192
)

// ScriptError is a malformed frame script. The CLI maps it to exit 2.
type ScriptError struct {
	Token  string
	Reason string
}

func (e *ScriptError) Error() string {
	if e.Token == "" {
		return "frame script: " + e.Reason
	}
	return fmt.Sprintf("frame script: %s: %s", e.Token, e.Reason)
}

// WaitTimeoutError is a wait token that never matched. The CLI maps it to exit 3.
type WaitTimeoutError struct {
	Wait      string
	Timeout   time.Duration
	LastFrame string
}

func (e *WaitTimeoutError) Error() string {
	return fmt.Sprintf("frame script: timed out after %s waiting for %s", e.Timeout, e.Wait)
}

// FrameOpts tunes RunFrameScript. The zero value uses a 10s per-wait timeout,
// no forced colour profile and no frame streaming.
type FrameOpts struct {
	Timeout     time.Duration
	ANSI        bool
	PrintFrames bool
	Out         io.Writer
	// Freeze stops the two things a frame of a turn in progress shows that
	// move on their own: the elapsed counters and the spinner's glyph. A
	// golden is otherwise a race against wall time — a slower build (-race,
	// a loaded machine) captures a different frame from the same script.
	//
	// It freezes those two renderings and nothing else. The model's clock is
	// left alone on purpose: it is also what decides a double-click, the
	// Ctrl+C window and every linger, and a frozen one would quietly change
	// what the script under test is exercising.
	Freeze bool
	// Setup builds the Config the runner drives, and is called after HOME has
	// been isolated (and CRAZE_HOME unset) and before New. Everything a frame
	// reads out of the craze directory — the config file, the session index
	// --continue and --resume resolve against — therefore comes from the
	// isolated one, which the Config passed to RunFrameScript cannot: it is
	// built by the caller, outside. Its result replaces that Config entirely
	// (§3.7). nil leaves the passed Config alone.
	Setup func() (Config, error)
	// gateSync runs the model's gated calls inline — the command gate's
	// test-only baseline (gate.go), today's synchronous control flow — where
	// the zero value runs them asynchronously, as every production run does.
	// It is unexported so only internal/tui's own tests can choose it: every
	// frame golden runs in both modes against the same golden file (plan 027
	// §3.12 (d)), and `craze frame` is always asynchronous.
	gateSync bool
	// transport is how the model reaches its session (plan 027 §3.16): in
	// process, the zero value — the engine New builds, as every production
	// run and `craze frame` have it — or over the control socket, where a
	// host built by frameSocketHook serves the session's engine and the model
	// runs over a remote.Session to it (Config.Backend). Unexported like
	// gateSync: the socket host is internal/tui's tests' alone, so the
	// production package links no server for `craze frame`.
	transport frameTransport
	// beforeBarrier, when a test sets it, runs in the runner after each
	// token's sync message is sent and before its barrier is awaited, with the
	// token and its number and the bus's newest frame; an error it returns
	// ends the script there, with the barrier not taken. It is how a test
	// holds the runner back while the program runs on (the rendezvous), or
	// makes the runner give up while the program waits on it.
	beforeBarrier func(tok string, n int, last func() frameState) error
	// afterSend, when a test sets it, runs in the runner right after each
	// message it hands the program, with that message: how a test makes the
	// runner fall behind the program at a point of its choosing.
	afterSend func(msg tea.Msg)
	// beforeQuit, when a test sets it, runs once the script has ended and the
	// model has settled at the capture's boundary, before the quit is sent,
	// with a way to hand the program a message of the test's own and the
	// bus's newest frame: how a test makes something arrive after the settle
	// and before the quit.
	beforeQuit func(send func(tea.Msg), last func() frameState)
}

// frameTransport is FrameOpts.transport: how a frame's model reaches its
// session.
type frameTransport string

const (
	// transportInproc is the model over the engine New builds: the default.
	transportInproc frameTransport = ""
	// transportSocket is the model over a remote.Session to a host that serves
	// the session's engine on a control socket (frameSocketHook).
	transportSocket frameTransport = "socket"
)

// String is the transport's name, as the golden manifest spells it.
func (t frameTransport) String() string {
	if t == transportInproc {
		return "inproc"
	}
	return string(t)
}

// frameHost is a socket run's host (plan 027 §3.16): what frameSocketHook
// built around the caller's Config — the session's engine, served on a control
// socket, and a remote.Session attached to it before the engine starts.
type frameHost struct {
	// cfg is the Config the model runs with: the caller's, its Backend the
	// remote session.
	cfg Config
	// head is the host's engine's stream head (its SyncSeq, streamHead's
	// rule): the capture's boundary, and the check's.
	head func(timeout time.Duration) (uint64, error)
	// start starts the host's engine as a headless host does, once the
	// program is running.
	start func()
	// match holds the model's fold, read while the program is parked, against
	// the host's model: false when the two are not at one seq (the host has
	// moved on), an error when they are and differ.
	match func(shared *transcript.Model) (bool, error)
	// barrier runs once match has passed, within timeout: a round trip to
	// the host whose answer follows every line the host has written to the
	// model's connection so far, so every one of them has been read by the
	// time it returns — what end judges the run's transport by (astra r69 5).
	barrier func(timeout time.Duration) error
	// end runs once the program has ended and the model's backend is closed:
	// it closes the host's engine and its server, and answers what the run's
	// transport did that a golden may not depend on (a reset, a re-attach).
	end func() error
}

// frameSocketHook builds a socket run's host around cfg (FrameOpts.transport).
// It is nil in production — no Config field or flag reaches it — and
// internal/tui's tests install it (frame_socket_test.go), like
// sessionBackendHook.
var frameSocketHook func(cfg Config) (*frameHost, error)

type frameTokenKind int

const (
	tokKey frameTokenKind = iota
	tokMouse
	tokResize
	tokSleep
	tokWait
)

type waitSpec struct {
	kind   string
	needle string
}

type frameToken struct {
	kind frameTokenKind
	text string
	key  tea.KeyMsg
	// msgs is what a tokMouse sends, in order: one message for a click or a
	// wheel notch, three for a <drag:>.
	msgs []tea.Msg
	size tea.WindowSizeMsg
	dur  time.Duration
	wait waitSpec
}

var simpleFrameKeys = map[string]tea.KeyMsg{
	"enter":     {Type: tea.KeyEnter},
	"esc":       {Type: tea.KeyEsc},
	"tab":       {Type: tea.KeyTab},
	"backspace": {Type: tea.KeyBackspace},
	"delete":    {Type: tea.KeyDelete},
	"space":     {Type: tea.KeyRunes, Runes: []rune{' '}},
	"up":        {Type: tea.KeyUp},
	"down":      {Type: tea.KeyDown},
	"left":      {Type: tea.KeyLeft},
	"right":     {Type: tea.KeyRight},
	"pgup":      {Type: tea.KeyPgUp},
	"pgdn":      {Type: tea.KeyPgDown},
	"shift-tab": {Type: tea.KeyShiftTab},
	"alt-enter": {Type: tea.KeyEnter, Alt: true},
}

var simpleFrameMouse = map[string]tea.MouseMsg{
	"wheel-up":   {Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress},
	"wheel-down": {Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress},
}

// leftMouse is one left-button report at a cell, which is what every selection
// token is made of.
func leftMouse(x, y int, action tea.MouseAction) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: action}
}

func runeKey(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// parseFrameScript turns a key script into tokens. Literal text is typed rune
// by rune; anything in angle brackets is a token.
func parseFrameScript(script string) ([]frameToken, error) {
	var out []frameToken
	rs := []rune(script)
	for i := 0; i < len(rs); {
		if rs[i] != '<' {
			out = append(out, frameToken{kind: tokKey, text: string(rs[i]), key: runeKey(rs[i])})
			i++
			continue
		}
		j := -1
		for k := i + 1; k < len(rs); k++ {
			if rs[k] == '>' {
				j = k
				break
			}
		}
		if j < 0 {
			return nil, &ScriptError{Token: string(rs[i:]), Reason: "unterminated token"}
		}
		tok, err := parseFrameToken(string(rs[i+1 : j]))
		if err != nil {
			return nil, err
		}
		out = append(out, tok)
		i = j + 1
	}
	return out, nil
}

func parseFrameToken(body string) (frameToken, error) {
	raw := "<" + body + ">"
	name := strings.ToLower(body)
	if k, ok := simpleFrameKeys[name]; ok {
		return frameToken{kind: tokKey, text: raw, key: k}, nil
	}
	if mm, ok := simpleFrameMouse[name]; ok {
		return frameToken{kind: tokMouse, text: raw, msgs: []tea.Msg{mm}}, nil
	}
	switch {
	case name == "lt":
		return frameToken{kind: tokKey, text: raw, key: runeKey('<')}, nil

	case strings.HasPrefix(name, "ctrl-"):
		r := strings.TrimPrefix(name, "ctrl-")
		if len(r) != 1 || r[0] < 'a' || r[0] > 'z' {
			return frameToken{}, &ScriptError{Token: raw, Reason: "want <ctrl-a>..<ctrl-z>"}
		}
		key := tea.KeyMsg{Type: tea.KeyCtrlA + tea.KeyType(r[0]-'a')}
		return frameToken{kind: tokKey, text: raw, key: key}, nil

	case strings.HasPrefix(name, "sleep:"):
		d, err := time.ParseDuration(strings.TrimPrefix(name, "sleep:"))
		if err != nil || d < 0 {
			return frameToken{}, &ScriptError{Token: raw, Reason: "want a duration like 250ms"}
		}
		return frameToken{kind: tokSleep, text: raw, dur: d}, nil

	case strings.HasPrefix(name, "click:"), strings.HasPrefix(name, "press:"),
		strings.HasPrefix(name, "motion:"), strings.HasPrefix(name, "release:"),
		strings.HasPrefix(name, "dblclick:"):
		return parseMouseToken(raw, name)

	case strings.HasPrefix(name, "hover:"):
		// <motion:> stamps the left button, so it models a drag. A hover is
		// motion with nothing held, which is the only thing the queue's
		// action strip appears on.
		x, y, err := parseFramePair(strings.TrimPrefix(name, "hover:"))
		if err != nil {
			return frameToken{}, &ScriptError{Token: raw, Reason: "want <hover:X,Y>"}
		}
		return frameToken{kind: tokMouse, text: raw, msgs: []tea.Msg{
			tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonNone, Action: tea.MouseActionMotion},
		}}, nil

	case strings.HasPrefix(name, "drag:"):
		// One gesture, three reports: a drag is exactly what the terminal
		// would send, so the script exercises the same path a mouse does.
		x1, y1, x2, y2, err := parseFrameQuad(strings.TrimPrefix(name, "drag:"))
		if err != nil {
			return frameToken{}, &ScriptError{Token: raw, Reason: "want <drag:X1,Y1,X2,Y2>"}
		}
		return frameToken{kind: tokMouse, text: raw, msgs: []tea.Msg{
			leftMouse(x1, y1, tea.MouseActionPress),
			leftMouse(x2, y2, tea.MouseActionMotion),
			leftMouse(x2, y2, tea.MouseActionRelease),
		}}, nil

	case strings.HasPrefix(name, "resize:"):
		c, r, err := parseFramePair(strings.TrimPrefix(name, "resize:"))
		if err != nil || c <= 0 || r <= 0 {
			return frameToken{}, &ScriptError{Token: raw, Reason: "want <resize:COLS,ROWS>"}
		}
		return frameToken{kind: tokResize, text: raw, size: tea.WindowSizeMsg{Width: c, Height: r}}, nil

	case strings.HasPrefix(name, "paste:"):
		// One bracketed paste, the way a terminal delivers it: `\n` in the
		// token body is a line break, so a multi-line paste is one message and
		// not a run of keys.
		text := strings.ReplaceAll(body[len("paste:"):], `\n`, "\n")
		if text == "" {
			return frameToken{}, &ScriptError{Token: raw, Reason: "want <paste:TEXT>"}
		}
		key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text), Paste: true}
		return frameToken{kind: tokKey, text: raw, key: key}, nil

	case strings.HasPrefix(name, "wait:"):
		return parseWaitToken(raw, body[len("wait:"):])
	}
	return frameToken{}, &ScriptError{Token: raw, Reason: "unknown token"}
}

// parseMouseToken handles the one-cell mouse tokens. <click:> and <press:> are
// the same message; they are spelled twice because a click is a hit-test and a
// press is the start of a selection, and a script reads better saying which.
func parseMouseToken(raw, name string) (frameToken, error) {
	kind, rest, _ := strings.Cut(name, ":")
	x, y, err := parseFramePair(rest)
	if err != nil {
		return frameToken{}, &ScriptError{Token: raw, Reason: "want <" + kind + ":X,Y>"}
	}
	var msg tea.Msg
	switch kind {
	case "click", "press":
		msg = leftMouse(x, y, tea.MouseActionPress)
	case "motion":
		msg = leftMouse(x, y, tea.MouseActionMotion)
	case "release":
		// Deliberately ButtonNone: that is what an X10 terminal reports, and
		// the release has to finalise the drag anyway.
		msg = tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonNone, Action: tea.MouseActionRelease}
	case "dblclick":
		msg = dblClickMsg{X: x, Y: y}
	}
	return frameToken{kind: tokMouse, text: raw, msgs: []tea.Msg{msg}}, nil
}

func parseWaitToken(raw, rest string) (frameToken, error) {
	lower := strings.ToLower(rest)
	switch lower {
	case "idle", "working", "card", "copied":
		return frameToken{kind: tokWait, text: raw, wait: waitSpec{kind: lower}}, nil
	}
	for _, kind := range []string{"text", "gone"} {
		if !strings.HasPrefix(lower, kind+":") {
			continue
		}
		needle := rest[len(kind)+1:]
		if needle == "" {
			return frameToken{}, &ScriptError{Token: raw, Reason: "empty needle"}
		}
		return frameToken{kind: tokWait, text: raw, wait: waitSpec{kind: kind, needle: needle}}, nil
	}
	return frameToken{}, &ScriptError{Token: raw, Reason: "unknown wait"}
}

func parseFramePair(s string) (int, int, error) {
	a, b, ok := strings.Cut(s, ",")
	if !ok {
		return 0, 0, fmt.Errorf("want two numbers")
	}
	x, err := strconv.Atoi(strings.TrimSpace(a))
	if err != nil {
		return 0, 0, err
	}
	y, err := strconv.Atoi(strings.TrimSpace(b))
	if err != nil {
		return 0, 0, err
	}
	return x, y, nil
}

func parseFrameQuad(s string) (int, int, int, int, error) {
	a, rest, ok := strings.Cut(s, ",")
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("want four numbers")
	}
	b, tail, ok := strings.Cut(rest, ",")
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("want four numbers")
	}
	x1, err := strconv.Atoi(strings.TrimSpace(a))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	y1, err := strconv.Atoi(strings.TrimSpace(b))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	x2, y2, err := parseFramePair(tail)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return x1, y1, x2, y2, nil
}

func (w waitSpec) match(s frameState) bool {
	if s.gated {
		// A frame published while a gated call waits for its reply shows the
		// issuing Update's own work and nothing after it: no wait may match it
		// (plan 027 §3.12, "The frame harness").
		return false
	}
	switch w.kind {
	case "idle":
		// A loaded session is not idle while its replay is still arriving:
		// the status is the zero value until the gate opens (§3.5), so
		// without the replay test <wait:idle> would match mid-restore.
		return s.started && !s.replaying && s.status == statusIdle && !s.card
	case "working":
		return s.status == statusWorking
	case "card":
		return s.card
	case "copied":
		return s.copied
	case "text":
		return strings.Contains(s.plain, w.needle)
	case "gone":
		return !strings.Contains(s.plain, w.needle)
	}
	return false
}

// frameState is one published frame plus the model state the waits look at.
//
// sync is the last sync token the model acknowledged (Model.syncAck), which is
// what each token's barrier waits for. gated says a gated call was waiting for
// its reply when the frame was published: such a frame matches no wait.
type frameState struct {
	view    string
	plain   string
	status  status
	started bool
	// picking is a pre-start dialog: the model is waiting for the user to
	// choose, so Start has not been called and never will be until a key
	// says so. <start> takes it as "as started as this frame gets".
	picking   bool
	replaying bool
	card      bool
	copied    bool
	sync      int
	gated     bool
	// settled says no gated call was waiting for its reply and nothing was
	// held: every message that had reached the model was reduced. folded is
	// the seq of the last event the model folded (its shared transcript's).
	// The runner sends its quit only once the newest frame is settled and has
	// folded the session's stream as far as it had gone when the script ended
	// (RunFrameScript); the capture itself need not be settled (final).
	settled bool
	folded  uint64
	// shared is the model's fold (m.shared) as the frame was published: a
	// socket run's check reads it while the program is parked
	// (captureThenMatch).
	shared *transcript.Model
	// final says the frame runner's quit message had been applied: this is
	// the frame the run captures (RunFrameScript) — the model after every
	// message that reached the program before the quit, in order, and none
	// after it. A message that arrived after the quit may still be held then,
	// and is not part of the capture (astra r47 3).
	final bool
}

// frameBus carries frames from the bubbletea goroutine to the script runner.
// Publishing never blocks the program loop; the rendezvous (meet) does, and
// only the frame runner's program meets.
type frameBus struct {
	mu     sync.Mutex
	latest frameState
	have   bool
	queue  []frameState
	n      int
	print  io.Writer
	wake   chan struct{}
	// taken is the newest sync token whose barrier the runner has taken, and
	// took wakes a program waiting on it. over is closed when the runner is
	// done, however it ended, and releases any rendezvous for good.
	taken    int
	took     chan struct{}
	over     chan struct{}
	overOnce sync.Once
	// captured is the first final frame published, if any (have it).
	captured     frameState
	haveCaptured bool
}

func newFrameBus(print io.Writer) *frameBus {
	return &frameBus{print: print, wake: make(chan struct{}, 1), took: make(chan struct{}, 1), over: make(chan struct{})}
}

// take records that the runner has taken token n's barrier.
func (b *frameBus) take(n int) {
	b.mu.Lock()
	b.taken = max(b.taken, n)
	b.mu.Unlock()
	select {
	case b.took <- struct{}{}:
	default:
	}
}

// finish says the runner is done: no barrier will be taken again, and no
// program may wait for one.
func (b *frameBus) finish() { b.overOnce.Do(func() { close(b.over) }) }

// meet is the rendezvous (plan 027 C17a, astra C17 1): the program has just
// published the frame that acknowledges sync token n, and waits here — the
// program loop blocked — until the runner has taken that token's barrier, or
// is done. So the barrier's newest frame is always the acknowledgement, the
// barrier clears exactly the frames up to it, and every frame published after
// it is queued for the next wait (§2.7). Without it, a program that drained a
// short turn's held ending before the runner resumed left an idle newest frame
// for the barrier to match, and the barrier cleared the release's working
// frame with the rest.
func (b *frameBus) meet(n int) {
	for {
		b.mu.Lock()
		taken := b.taken >= n
		b.mu.Unlock()
		if taken {
			return
		}
		select {
		case <-b.took:
		case <-b.over:
			return
		}
	}
}

func (b *frameBus) publish(s frameState) {
	b.mu.Lock()
	b.n++
	n := b.n
	b.latest = s
	b.have = true
	if s.final && !b.haveCaptured {
		b.captured, b.haveCaptured = s, true
	}
	if len(b.queue) >= frameQueueMax {
		b.queue = append(b.queue[:0], b.queue[1:]...)
	}
	b.queue = append(b.queue, s)
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
	// Printing stays outside the lock: a slow writer must not stall await, or a
	// wait could outlive its own timeout.
	if b.print != nil {
		fmt.Fprintf(b.print, "--- frame %d status=%s started=%v card=%v ---\n%s\n", n, s.status, s.started, s.card, s.plain)
	}
}

func (b *frameBus) last() frameState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.latest
}

// capture is the run's frame: the one the quit message's Update published —
// the model after every message that reached the program before the quit, in
// order, and none after it (frameQuitMsg) — or, for a run that ended some
// other way — a script that quits, a panic, a kill — the newest.
func (b *frameBus) capture() frameState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.haveCaptured {
		return b.captured
	}
	return b.latest
}

// awaitLatest is await on the newest frame alone: it returns once the newest
// published frame satisfies pred, never an older queued one (C17c, astra
// C17b 3), or false at the deadline or once stop is closed.
func (b *frameBus) awaitLatest(pred func(frameState) bool, timeout time.Duration, stop <-chan struct{}) (frameState, bool) {
	deadline := time.Now().Add(timeout)
	for {
		b.mu.Lock()
		have, last := b.have, b.latest
		b.mu.Unlock()
		if have && pred(last) {
			return last, true
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return last, false
		}
		timer := time.NewTimer(wait)
		select {
		case <-b.wake:
			timer.Stop()
		case <-stop:
			timer.Stop()
			b.mu.Lock()
			have, last = b.have, b.latest
			b.mu.Unlock()
			return last, have && pred(last)
		case <-timer.C:
			return last, false
		}
	}
}

// await returns the first frame matching pred: the newest state if it already
// matches, otherwise the oldest unconsumed frame that does, otherwise it blocks
// for new frames until the deadline or until stop is closed. Closing stop only
// ends the blocking; already-published frames are still checked first.
//
// A documented residual, of these semantics and not of the command gate's: a
// state that comes and goes on its own after a barrier and before the next
// wait runs — a card that opens and closes by itself before the runner reaches
// <wait:card> — is still matched by the oldest-queued fallback, although the
// newest frame no longer shows it (astra C17 1's converse schedule).
func (b *frameBus) await(pred func(frameState) bool, timeout time.Duration, stop <-chan struct{}) (frameState, bool) {
	deadline := time.Now().Add(timeout)
	stopped := false
	for {
		b.mu.Lock()
		if b.have && pred(b.latest) {
			st := b.latest
			b.queue = b.queue[:0]
			b.mu.Unlock()
			return st, true
		}
		for i, s := range b.queue {
			if pred(s) {
				b.queue = append(b.queue[:0], b.queue[i+1:]...)
				b.mu.Unlock()
				return s, true
			}
		}
		b.queue = b.queue[:0]
		last := b.latest
		b.mu.Unlock()

		if stopped {
			return last, false
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return last, false
		}
		timer := time.NewTimer(wait)
		select {
		case <-b.wake:
			timer.Stop()
		case <-stop:
			timer.Stop()
			stopped = true
		case <-timer.C:
			return last, false
		}
	}
}

// frameTokenMsg is a key, mouse or resize token and its sync token, handed to
// the program as ONE message (C17b, astra r43 1): frameModel applies the two
// back to back, so the sync token is always the very next message the model
// reduces after its key — acknowledged in that Update when the key opened no
// gate, pending under the gate it opened and acknowledged at that gate's
// release. Sent as two, a runner descheduled between them let the program
// reduce the key's whole short turn first, and the acknowledging frame was
// already idle.
type frameTokenMsg struct {
	msg tea.Msg
	n   int
}

// frameQuitMsg is the frame runner's quit, through the model's FIFO (C17c,
// astra C17b 3): held behind an open gate and the messages held before it,
// like a key, and applied in arrival order, where it marks the model
// (harnessQuit) and answers tea.Quit. So the quit lands after every message
// that reached the program before it — as bubbletea's own quit did before a
// held message could be waiting — and the frame its Update publishes is the
// run's capture: the model after every message that reached the program
// before the quit, in order, and none after it. A message that arrives after
// the quit — behind a gate that opened late, say — may remain held, and is not
// part of the capture; "nothing held" is not what the capture promises (astra
// r47 3).
//
// linger marks the capture and quits nothing: a socket run's program runs on
// after its capture, so the check that follows it (captureThenMatch) can let
// the fold follow the host to a seq both hold without moving what was
// captured; the run's shutdown then quits it with a plain one.
type frameQuitMsg struct{ linger bool }

// frameSyncMsg is a no-op message the runner uses to know its previous message
// has been processed and published. It goes through the model's own FIFO: the
// model acknowledges it (Model.syncAck) when it is reduced — at once when
// nothing is waiting, in the release of a gated call it arrived during, or
// when drained behind the messages held before it (gate.go's syncFrame) — and
// it never runs the Update wrapper.
type frameSyncMsg struct{ n int }

// frameModel wraps the real Model and publishes inner.View() after every
// message, so the script runner sees exactly what a terminal would. meet is
// the frame runner's program: an Update that acknowledges a sync token waits
// there for the runner to take its barrier (frameBus.meet). A test driving a
// frameModel by hand leaves it false.
type frameModel struct {
	inner Model
	bus   *frameBus
	meet  bool
}

func (f frameModel) Init() tea.Cmd {
	cmd := f.inner.Init()
	f.publish()
	return cmd
}

func (f frameModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if t, ok := msg.(frameTokenMsg); ok {
		// The key, published, then its token, published and met: two Updates
		// of the model in one program message, their commands together.
		next, keyCmd := f.Update(t.msg)
		next, syncCmd := next.(frameModel).Update(frameSyncMsg{n: t.n})
		return next, tea.Batch(keyCmd, syncCmd)
	}
	acked := f.inner.syncAck
	im, cmd := f.inner.Update(msg)
	f.inner = im.(Model)
	f.publish()
	// A token acknowledged — at once, by a release's pending ack, or drained —
	// and its frame published: nothing more is reduced until the runner has
	// taken its barrier.
	if f.meet && f.inner.syncAck > acked {
		f.bus.meet(f.inner.syncAck)
	}
	return f, cmd
}

func (f frameModel) View() string { return f.inner.View() }

func (f frameModel) publish() {
	view := f.inner.View()
	f.bus.publish(frameState{
		view:      view,
		plain:     ansi.Strip(view),
		status:    f.inner.status,
		started:   f.inner.started,
		picking:   f.inner.picking(),
		replaying: f.inner.replaying,
		card:      f.inner.cardOpen(),
		copied:    f.inner.copyLingering(),
		sync:      f.inner.syncAck,
		gated:     f.inner.gate != nil,
		settled:   f.inner.gate == nil && len(f.inner.held) == 0,
		folded:    f.inner.foldedSeq(),
		shared:    f.inner.shared,
		final:     f.inner.harnessQuit,
	})
}

type frameRunner struct {
	p             *tea.Program
	bus           *frameBus
	timeout       time.Duration
	finished      <-chan struct{}
	seq           int
	beforeBarrier func(tok string, n int, last func() frameState) error
	afterSend     func(msg tea.Msg)
}

func (r *frameRunner) done() bool {
	select {
	case <-r.finished:
		return true
	default:
		return false
	}
}

// RunFrameScript drives a real Model headlessly and returns the final frame,
// ANSI-stripped and raw.
func RunFrameScript(cfg Config, cols, rows int, script string, opts FrameOpts) (string, string, error) {
	toks, err := parseFrameScript(script)
	if err != nil {
		return "", "", err
	}
	if cols <= 0 || rows <= 0 {
		return "", "", &ScriptError{Reason: "cols and rows must be positive"}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultFrameTimeout
	}

	restoreHome, err := isolateFrameHome()
	if err != nil {
		return "", "", err
	}
	defer restoreHome()

	// Inside the isolated HOME, and before New: a --continue seeded by the
	// caller writes its rows into the isolated index and resolves them there,
	// so nothing a frame does can be read out of — or written into — the
	// developer's own ~/.craze (§3.7).
	if opts.Setup != nil {
		cfg, err = opts.Setup()
		if err != nil {
			return "", "", err
		}
	}

	// A socket run's host is built here, inside the isolated HOME too, since
	// the agent its engine starts inherits it: the session the Config names,
	// served on a control socket, with the model's backend attached to it
	// before its engine starts (frameSocketHook, plan 027 §3.16). Its end
	// closes the engine and the server after the program's shutdown — the
	// model's view close — however the run ends: deferred before that
	// shutdown's own defer below, so it runs after it on a panic, and called
	// in line after it otherwise, where what it answers is the run's error.
	var host *frameHost
	hostEnded := false
	endHost := func() error {
		if host == nil || hostEnded {
			return nil
		}
		hostEnded = true
		return host.end()
	}
	if opts.transport == transportSocket {
		if frameSocketHook == nil {
			return "", "", errors.New("frame: the socket transport has no host: only internal/tui's tests build one")
		}
		host, err = frameSocketHook(cfg)
		if err != nil {
			return "", "", fmt.Errorf("frame: the socket host: %w", err)
		}
		cfg = host.cfg
		defer func() { _ = endHost() }()
	}

	// The runner prints its final frame to stdout, so an OSC 52 sequence in
	// that stream would corrupt it — and nothing under `make test` may reach for
	// the developer's own clipboard. Both writes are recorded instead. The
	// swap is safe because isolateFrameHome already serialises frame runs.
	_, restoreClipboard := recordCopies()
	defer restoreClipboard()

	if opts.ANSI {
		prev := lipgloss.ColorProfile()
		lipgloss.SetColorProfile(termenv.TrueColor)
		defer lipgloss.SetColorProfile(prev)
	}

	var print io.Writer
	if opts.PrintFrames {
		print = opts.Out
		if print == nil {
			print = os.Stderr
		}
	}
	bus := newFrameBus(print)

	m := New(cfg)
	m.frozen = opts.Freeze
	// Always set from the options, never left to New: a test binary's default
	// is the baseline (gateSyncDefault), and a frame is asynchronous unless its
	// caller asked for the baseline.
	m.gateSync = opts.gateSync
	p := tea.NewProgram(frameModel{inner: m, bus: bus, meet: true}, tea.WithoutRenderer(), tea.WithInput(nil))
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		_, runErr := p.Run()
		done <- runErr
		close(finished)
	}()
	if host != nil {
		// The model's start waits for the session's readiness (its backend's
		// Start); the host starts its engine as a headless host does, and the
		// start's events reach the model live, as the primary's reach an
		// in-process one.
		host.start()
	}

	// shutdown ends the run (C17c, astra C17b 2): the runner is done, however
	// the script ended — its last token, a wait that timed out, a hook's error
	// or panic — so a program waiting at a rendezvous is let go; the quit goes
	// in through the model's FIFO (frameQuitMsg), handed over on a goroutine of
	// its own so a program blocked inside an Update cannot block the runner;
	// and if the program has not ended within the timeout, the engine is
	// closed — which is what a call blocked inside an Update is waiting on, and
	// what a gate held open by a call that never answers is waiting on — and
	// the program is killed, and then waited for. It is never abandoned (astra
	// r47): a program still running — blocked in a PrintFrames writer, say —
	// would outlive the run that restores HOME, the clipboard's recording and
	// the colour profile around it, and overlap the next run. A program that
	// never ends even then is the test binary's timeout's to catch.
	//
	// The engine is the owner's, not m's: a pre-start picker has none at New,
	// the one the picker built is the one that owns a child process, and on a
	// recovered panic p.Run hands back no model to read it from. Closing the
	// engine closes its session, and stops its driver: Close blocks until the
	// child is reaped, and is safe even if Start is still in flight — it will
	// not adopt a child into a closed session. It is idempotent. Over the
	// socket the owner holds the remote session, whose Close is a view close:
	// every call waiting on it is answered, and the host's engine is the
	// host's to close (endHost).
	closeEngine := func() {
		if eng := m.owner.current(); eng != nil {
			_ = eng.Close()
		}
	}
	shutdown := func() error {
		bus.finish()
		go p.Send(frameQuitMsg{})
		var runErr error
		select {
		case runErr = <-done:
		case <-time.After(timeout):
			closeEngine()
			p.Kill()
			runErr = <-done
		}
		closeEngine()
		return runErr
	}
	shut := false
	defer func() {
		// A panic out of the runner (a test's hook) unwinds past the tail
		// below: the program and the engine still end (C17b, astra r43).
		if !shut {
			_ = shutdown()
		}
	}()

	r := &frameRunner{p: p, bus: bus, timeout: timeout, finished: finished, beforeBarrier: opts.beforeBarrier, afterSend: opts.afterSend}
	scriptErr := r.run(toks, cols, rows)
	if scriptErr == nil {
		// The quit waits for a settled model (C17b): every message that
		// reached the model by the end of the script is reduced first — a
		// gate's release and the drain of what it held, which the drain's own
		// commands would otherwise race the quit to — and so is every event the
		// session had published by the time the script ended, which the
		// reader, parked while held messages drain (§3.12), may still have
		// left on the stream. Today's synchronous Update left nothing arrived
		// unreduced and its reader never parked; the gateSync baseline is
		// always settled. The capture is then the quit's frame (frameQuitMsg):
		// the model after every message that reached the program before the
		// quit, in order, and none after it — which is not always a settled
		// one, since a message arriving after the quit may still be held.
		//
		// Over the socket the boundary is the host's engine's head: the model's
		// backend is a remote session, with no engine behind it in this
		// process.
		head := func() (uint64, error) { return streamHead(m.owner, timeout) }
		if host != nil {
			head = func() (uint64, error) { return host.head(timeout) }
		}
		scriptErr = r.settleAt(head)
		if scriptErr == nil && opts.beforeQuit != nil {
			opts.beforeQuit(p.Send, bus.last)
		}
		if scriptErr == nil && host != nil {
			scriptErr = r.captureThenMatch(host)
		}
	}
	shut = true
	if runErr := shutdown(); scriptErr == nil {
		scriptErr = runErr
	}

	final := bus.capture()
	if te, ok := scriptErr.(*WaitTimeoutError); ok {
		te.LastFrame = final.plain
	}
	if err := endHost(); err != nil {
		// What the socket host saw — a reset, a re-attach — fails the run
		// whatever else it came to: it is the socket run's alone, so a run
		// that failed the same way in process still disagrees with it.
		if scriptErr == nil {
			scriptErr = err
		} else {
			scriptErr = fmt.Errorf("%w; %w", scriptErr, err)
		}
	}
	return final.plain, final.view, scriptErr
}

func (r *frameRunner) run(toks []frameToken, cols, rows int) error {
	if err := r.send(tea.WindowSizeMsg{Width: cols, Height: rows}, "<resize>"); err != nil {
		return err
	}
	// Nothing typed before Start returns would reach the agent, so hold the
	// script until the model has left the starting state.
	if err := r.await(startedSpec, "<start>"); err != nil {
		return err
	}
	for _, tok := range toks {
		var err error
		switch tok.kind {
		case tokKey:
			err = r.send(tok.key, tok.text)
		case tokMouse:
			for _, mm := range tok.msgs {
				if err = r.send(mm, tok.text); err != nil {
					break
				}
			}
		case tokResize:
			err = r.send(tok.size, tok.text)
		case tokSleep:
			time.Sleep(tok.dur)
			err = r.sync(tok.text)
		case tokWait:
			err = r.await(tok.wait.match, tok.text)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// startedSpec matches once Start has returned, either way: a failed Start
// leaves the model in the error state rather than started. A pre-start picker
// counts too — nothing is starting until the script chooses a row, and the
// script is what the runner is holding. Like every wait, it never matches a
// gated frame.
func startedSpec(s frameState) bool {
	if s.gated {
		return false
	}
	return s.started || s.status == statusError || s.picking
}

func (r *frameRunner) await(pred func(frameState) bool, what string) error {
	if _, ok := r.bus.await(pred, r.timeout, r.finished); ok {
		return nil
	}
	return &WaitTimeoutError{Wait: what, Timeout: r.timeout}
}

// settleAt establishes the capture's boundary — the session's stream head
// when the script ended (head: streamHead, or a socket run's host engine's) —
// and waits for the model to settle at it (settle).
func (r *frameRunner) settleAt(head func() (uint64, error)) error {
	at, err := head()
	if err != nil {
		return fmt.Errorf("frame: the capture's boundary could not be established: %w", err)
	}
	return r.settle(at)
}

// settle waits, within the frame timeout, for the newest frame — never an
// older queued one — to be of a settled model (no gated call waiting, nothing
// held) that has folded the session's stream up to head: every event
// published before the script ended. A program that has ended is as settled
// as it gets.
func (r *frameRunner) settle(head uint64) error {
	pred := func(s frameState) bool { return s.settled && s.folded >= head }
	if _, ok := r.bus.awaitLatest(pred, r.timeout, r.finished); ok || r.done() {
		return nil
	}
	return &WaitTimeoutError{Wait: "<settle>", Timeout: r.timeout}
}

// streamHead is the seq the session's stream has reached: every event the
// engine had enqueued is delivered to the model's stream first (SyncSeq). It
// is the in-process engine's — the backend's own, or the one a test's wrapper
// around it names (engineBehind); with no engine it is 0. An engine that is
// closing or closed publishes nothing more: 0, with nothing to wait for. Any
// other failure — the timeout above all — is an error: the boundary could not
// be established, and a capture without it would be incomplete. (A socket
// run's boundary is its host's engine's head, by the same rule: frameHost.)
func streamHead(owner *sessionOwner, timeout time.Duration) (uint64, error) {
	eng := engineBehind(owner.current())
	if eng == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	head, err := eng.SyncSeq(ctx)
	switch {
	case err == nil:
		return head, nil
	case errors.Is(err, agent.ErrLogClosing), errors.Is(err, agent.ErrClosed), errors.Is(err, agent.ErrFlushGaveUp):
		return 0, nil
	}
	return 0, err
}

// captureThenMatch is a socket run's end (plan 027 §3.16): the capture, then
// the check that the model's fold is the host's model.
//
// The capture is taken exactly as an in-process run takes it — the quit's
// frame, the model after every message that reached the program before the
// quit, in order — but the quit lingers (frameQuitMsg): the program runs on
// after it, so the check can wait for the fold to reach a seq the host's model
// is at too without anything it waits for reaching the capture. Then, within
// the frame timeout: the host's head is read, the model is let settle at it,
// and a sync token parks the program at its rendezvous — nothing folds while
// it is parked — where host.match holds the fold, as its acknowledging frame
// names it, against the host's model at that one seq. A host that has moved
// on meanwhile is followed, and the check is made again. Then the host's
// barrier: every line the host has written to the connection so far is read
// (frameHost.barrier). The run's shutdown then quits the program.
func (r *frameRunner) captureThenMatch(host *frameHost) error {
	go r.p.Send(frameQuitMsg{linger: true})
	// Every frame from the quit's on is final (harnessQuit stays set); the
	// first is the capture (frameBus.capture).
	captured := func(s frameState) bool { return s.final }
	if _, ok := r.bus.awaitLatest(captured, r.timeout, r.finished); !ok {
		if r.done() {
			// The script ended the program itself: its frame is the capture
			// (frameBus.capture), and no fold is left to check.
			return errors.New("frame: the program ended before its capture: a socket run's fold cannot be checked")
		}
		return &WaitTimeoutError{Wait: "<capture>", Timeout: r.timeout}
	}
	deadline := time.Now().Add(r.timeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return &WaitTimeoutError{Wait: "<match> (the host's model and the fold at one seq)", Timeout: r.timeout}
		}
		head, err := host.head(left)
		if err != nil {
			return fmt.Errorf("frame: the host's head for the check: %w", err)
		}
		if err := r.settle(head); err != nil {
			return err
		}
		if r.done() {
			return errors.New("frame: the program ended before the host's model could be checked")
		}
		r.seq++
		n := r.seq
		r.p.Send(frameSyncMsg{n: n})
		st, ok := r.bus.awaitLatest(func(s frameState) bool { return s.sync >= n }, time.Until(deadline), r.finished)
		var same bool
		if ok {
			// Parked: the frame that acknowledged n is the newest, and the
			// program waits at its rendezvous until the barrier is taken.
			same, err = host.match(st.shared)
		}
		r.bus.take(n)
		switch {
		case !ok && r.done():
			return errors.New("frame: the program ended before the host's model could be checked")
		case !ok:
			return &WaitTimeoutError{Wait: "<match> (frame sync)", Timeout: r.timeout}
		case err != nil:
			return err
		case same:
			return host.barrier(r.timeout)
		}
	}
}

// send hands the program msg and its sync token as one message (frameTokenMsg)
// and takes the token's barrier.
func (r *frameRunner) send(msg tea.Msg, what string) error {
	r.seq++
	n := r.seq
	tok := frameTokenMsg{msg: msg, n: n}
	r.p.Send(tok)
	if r.afterSend != nil {
		r.afterSend(tok)
	}
	return r.barrier(n, what)
}

// sync sends a bare sync token — what <sleep:> ends with — and takes its
// barrier.
func (r *frameRunner) sync(what string) error {
	r.seq++
	n := r.seq
	r.p.Send(frameSyncMsg{n: n})
	if r.afterSend != nil {
		r.afterSend(frameSyncMsg{n: n})
	}
	return r.barrier(n, what)
}

// barrier blocks until the program has processed everything sent so far, so
// the next wait sees state caused by this token and not the previous one. A
// script that quits ends the program, and Send after that is a no-op, so a
// finished program counts as synchronised.
func (r *frameRunner) barrier(n int, what string) error {
	if r.beforeBarrier != nil {
		if err := r.beforeBarrier(what, n, r.bus.last); err != nil {
			return err
		}
	}
	pred := func(s frameState) bool { return s.sync >= n }
	_, ok := r.bus.await(pred, r.timeout, r.finished)
	// Taken, matched or not: the program may go on (frameBus.meet).
	r.bus.take(n)
	if ok {
		return nil
	}
	if r.done() {
		return nil
	}
	return &WaitTimeoutError{Wait: what + " (frame sync)", Timeout: r.timeout}
}

// frameHomeMu serialises the process-wide HOME swap: two overlapping runs would
// otherwise restore each other's already-deleted directory.
var frameHomeMu sync.Mutex

// isolateFrameHome points HOME at an empty directory and unsets CRAZE_HOME, so
// a developer's own config, session index and skills cannot change a frame —
// and a frame's seeded index cannot land in their craze directory, wherever
// CRAZE_HOME had moved it. The child agent inherits both, because the process
// environment is read when the child is spawned. The returned func puts each
// variable back as it was, set or unset.
func isolateFrameHome() (func(), error) {
	frameHomeMu.Lock()
	dir, err := os.MkdirTemp("", "craze-frame-home")
	if err != nil {
		frameHomeMu.Unlock()
		return nil, err
	}
	restoreHome, restoreCrazeHome := envRestorer("HOME"), envRestorer("CRAZE_HOME")
	restore := func() {
		restoreCrazeHome()
		restoreHome()
		_ = os.RemoveAll(dir)
	}
	if err := os.Setenv("HOME", dir); err != nil {
		restore()
		frameHomeMu.Unlock()
		return nil, err
	}
	if err := os.Unsetenv("CRAZE_HOME"); err != nil {
		restore()
		frameHomeMu.Unlock()
		return nil, err
	}
	return func() {
		defer frameHomeMu.Unlock()
		restore()
	}, nil
}

// envRestorer records one variable's current state and returns the func that
// puts it back: the same value, or unset if it was unset.
func envRestorer(name string) func() {
	prev, had := os.LookupEnv(name)
	return func() {
		if had {
			_ = os.Setenv(name, prev)
		} else {
			_ = os.Unsetenv(name)
		}
	}
}
