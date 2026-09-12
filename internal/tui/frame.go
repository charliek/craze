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
	kind  frameTokenKind
	text  string
	key   tea.KeyMsg
	mouse tea.MouseMsg
	size  tea.WindowSizeMsg
	dur   time.Duration
	wait  waitSpec
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
		return frameToken{kind: tokMouse, text: raw, mouse: mm}, nil
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

	case strings.HasPrefix(name, "click:"):
		x, y, err := parseFramePair(strings.TrimPrefix(name, "click:"))
		if err != nil {
			return frameToken{}, &ScriptError{Token: raw, Reason: "want <click:X,Y>"}
		}
		mm := tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
		return frameToken{kind: tokMouse, text: raw, mouse: mm}, nil

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

func parseWaitToken(raw, rest string) (frameToken, error) {
	lower := strings.ToLower(rest)
	switch lower {
	case "idle", "working", "card":
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

func (w waitSpec) match(s frameState) bool {
	switch w.kind {
	case "idle":
		return s.started && s.status == statusIdle && !s.card
	case "working":
		return s.status == statusWorking
	case "card":
		return s.card
	case "text":
		return strings.Contains(s.plain, w.needle)
	case "gone":
		return !strings.Contains(s.plain, w.needle)
	}
	return false
}

// frameState is one published frame plus the model state the waits look at.
type frameState struct {
	view    string
	plain   string
	status  status
	started bool
	card    bool
	sync    int
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
		view:    view,
		plain:   ansi.Strip(view),
		status:  f.inner.status,
		started: f.inner.started,
		card:    f.inner.cardOpen(),
		sync:    f.sync,
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
			err = r.send(tok.mouse, tok.text)
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
