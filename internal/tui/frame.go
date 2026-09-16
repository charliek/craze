package tui

import (
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
}

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
type frameState struct {
	view      string
	plain     string
	status    status
	started   bool
	replaying bool
	card      bool
	copied    bool
	sync      int
}

// frameBus carries frames from the bubbletea goroutine to the script runner.
// Publishing never blocks the program loop.
type frameBus struct {
	mu     sync.Mutex
	latest frameState
	have   bool
	queue  []frameState
	n      int
	print  io.Writer
	wake   chan struct{}
}

func newFrameBus(print io.Writer) *frameBus {
	return &frameBus{print: print, wake: make(chan struct{}, 1)}
}

func (b *frameBus) publish(s frameState) {
	b.mu.Lock()
	b.n++
	n := b.n
	b.latest = s
	b.have = true
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

// await returns the first frame matching pred: the newest state if it already
// matches, otherwise the oldest unconsumed frame that does, otherwise it blocks
// for new frames until the deadline or until stop is closed. Closing stop only
// ends the blocking; already-published frames are still checked first.
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

// frameSyncMsg is a no-op message the runner uses to know its previous message
// has been processed and published.
type frameSyncMsg struct{ n int }

// frameModel wraps the real Model and publishes inner.View() after every
// message, so the script runner sees exactly what a terminal would.
type frameModel struct {
	inner Model
	bus   *frameBus
	sync  int
}

func (f frameModel) Init() tea.Cmd {
	cmd := f.inner.Init()
	f.publish()
	return cmd
}

func (f frameModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if s, ok := msg.(frameSyncMsg); ok {
		f.sync = s.n
		f.publish()
		return f, nil
	}
	im, cmd := f.inner.Update(msg)
	f.inner = im.(Model)
	f.publish()
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
		replaying: f.inner.replaying,
		card:      f.inner.cardOpen(),
		copied:    f.inner.copyLingering(),
		sync:      f.sync,
	})
}

type frameRunner struct {
	p        *tea.Program
	bus      *frameBus
	timeout  time.Duration
	finished <-chan struct{}
	seq      int
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
	sess := m.sess
	p := tea.NewProgram(frameModel{inner: m, bus: bus}, tea.WithoutRenderer(), tea.WithInput(nil))
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		_, runErr := p.Run()
		done <- runErr
		close(finished)
	}()

	r := &frameRunner{p: p, bus: bus, timeout: timeout, finished: finished}
	scriptErr := r.run(toks, cols, rows)

	p.Quit()
	select {
	case runErr := <-done:
		if scriptErr == nil {
			scriptErr = runErr
		}
	case <-time.After(timeout):
		p.Kill()
		<-done
	}
	// Close blocks until the child is reaped, and is safe even if Start is
	// still in flight: it will not adopt a child into a closed session.
	if sess != nil {
		_ = sess.Close()
	}

	final := bus.last()
	if te, ok := scriptErr.(*WaitTimeoutError); ok {
		te.LastFrame = final.plain
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
// leaves the model in the error state rather than started.
func startedSpec(s frameState) bool {
	return s.started || s.status == statusError
}

func (r *frameRunner) await(pred func(frameState) bool, what string) error {
	if _, ok := r.bus.await(pred, r.timeout, r.finished); ok {
		return nil
	}
	return &WaitTimeoutError{Wait: what, Timeout: r.timeout}
}

func (r *frameRunner) send(msg tea.Msg, what string) error {
	r.p.Send(msg)
	return r.sync(what)
}

// sync blocks until the program has processed everything sent so far, so the
// next wait sees state caused by this token and not the previous one. A script
// that quits ends the program, and Send after that is a no-op, so a finished
// program counts as synchronised.
func (r *frameRunner) sync(what string) error {
	r.seq++
	n := r.seq
	r.p.Send(frameSyncMsg{n: n})
	pred := func(s frameState) bool { return s.sync >= n }
	if _, ok := r.bus.await(pred, r.timeout, r.finished); ok {
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

// isolateFrameHome points HOME at an empty directory so a developer's own
// config and skills cannot change a frame. The child agent inherits it, because
// the process environment is read when the child is spawned.
func isolateFrameHome() (func(), error) {
	frameHomeMu.Lock()
	dir, err := os.MkdirTemp("", "craze-frame-home")
	if err != nil {
		frameHomeMu.Unlock()
		return nil, err
	}
	prev, had := os.LookupEnv("HOME")
	if err := os.Setenv("HOME", dir); err != nil {
		_ = os.RemoveAll(dir)
		frameHomeMu.Unlock()
		return nil, err
	}
	return func() {
		defer frameHomeMu.Unlock()
		if had {
			_ = os.Setenv("HOME", prev)
		} else {
			_ = os.Unsetenv("HOME")
		}
		_ = os.RemoveAll(dir)
	}, nil
}
