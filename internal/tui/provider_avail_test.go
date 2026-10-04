package tui

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
)

// Provider availability in the TUI's pickers (plan 036 §3.3, A5, A6): the
// startup picker's dimmed rows, tags and detail line, its refusals after the
// command line's own, and Config.Availability asked off the Update with a
// stale answer never taken; the session list's /provider dimming and refusing
// on Enter, Tab and a typed line, its membership and states read again at
// each opening, and its answers numbered so an older one never replaces a
// newer.

// The reasons and fixes the fixtures use: the check's own words (plan 036
// §3.1), cursor's naming a config path.
const (
	availCursorReason = "cursor-agent not found on PATH"
	availCursorFix    = "install cursor-agent, or set [agents].cursor in ~/.craze/config.toml"
	availNativeReason = "no model provider has a key"
	// availNativeFix is native's fix as the TUI's sources give it (the
	// check's TUI column, X23): what picking it does, first.
	availNativeFix = `pick it to connect one, or run "craze auth login"`
)

// availFixture is the plan's picker (§3.3's goldens): cursor unavailable,
// grok ready, native needing setup.
func availFixture() []ProviderAvail {
	return []ProviderAvail{
		{ID: "cursor", State: AvailUnavailable, Reason: availCursorReason, Fix: availCursorFix},
		{ID: "grok", State: AvailReady},
		{ID: "native", State: AvailNeedsSetup, Reason: availNativeReason, Fix: availNativeFix},
	}
}

// availRefusalOf is the refusal the picker shows for the fixture's cursor.
const availCursorRefusal = "can't start cursor: " + availCursorReason + " — " + availCursorFix

// availPickerConfig is a startup picker's Config — the default def, the
// built-in rows, sessions built by build — whose Availability answers avail
// and counts its calls in calls.
func availPickerConfig(t *testing.T, def agent.Provider, avail func() []ProviderAvail, calls *atomic.Int32,
	build func(agent.Provider) agent.Session) Config {
	t.Helper()
	isolateSkillsHome(t)
	return Config{
		Theme:      "tokyo-night",
		Workspace:  frameWorkspace(t),
		Model:      "grok",
		Yolo:       true,
		Provider:   def,
		NewSession: build,
		Availability: func() []ProviderAvail {
			if calls != nil {
				calls.Add(1)
			}
			return avail()
		},
	}
}

// availPicker is New(cfg) at cols×rows with Init's read of the picker's
// availability run — within a step — and its answer taken.
func availPicker(t *testing.T, cfg Config, cols, rows int) Model {
	t.Helper()
	m := New(cfg)
	tm, _ := m.Update(tea.WindowSizeMsg{Width: cols, Height: rows})
	m = tm.(Model)
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("the picker opened and Init asked for no availability")
	}
	msg, ok := runWatched(t, cmd).(providerAvailMsg)
	if !ok {
		t.Fatalf("Init's command answered %T, want the availability", msg)
	}
	tm, _ = m.Update(msg)
	return tm.(Model)
}

// builtLog is a NewSession that records each provider it builds a session
// of.
type builtLog struct {
	mu    sync.Mutex
	names []string
}

func (b *builtLog) build(p agent.Provider) agent.Session {
	b.mu.Lock()
	b.names = append(b.names, p.Name())
	b.mu.Unlock()
	s := NewStub()
	s.SetProvider(p)
	return s
}

func (b *builtLog) got() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.names)
}

// TestFrameGoldenProviderPickerAvailability is §3.3's three frames: the
// picker with cursor unavailable and the default — selected, its detail line
// under the list — grok ready and native needing setup (80x24); Enter on
// cursor, refused in place (80x24); and the first frame at 40x12, the state
// word kept.
func TestFrameGoldenProviderPickerAvailability(t *testing.T) {
	log := &builtLog{}
	m := availPicker(t, availPickerConfig(t, agent.CursorProvider(), availFixture, nil, log.build), 80, 24)
	assertGolden(t, "provider-picker-availability-80x24", 80, 24, plainView(m))

	tm, _ := m.Update(enter())
	assertGolden(t, "provider-picker-refused-80x24", 80, 24, plainView(tm.(Model)))

	narrow := availPicker(t, availPickerConfig(t, agent.CursorProvider(), availFixture, nil, log.build), 40, 12)
	assertGolden(t, "provider-picker-narrow-40x12", 40, 12, plainView(narrow))
	if got := log.got(); len(got) != 0 {
		t.Fatalf("the frames built sessions of %v", got)
	}
}

// TestProviderPickerDrawsTheStates (A5's style test, which the ANSI-stripped
// goldens cannot be): a row that is not ready is drawn in the dim colour — on
// the selection band when the cursor is on it — and a ready row in the
// foreground; its tag is its state word, then `default`. The two detail lines
// (X22) are the selected row's reason and its fix, dim, held blank — never
// dropped — on a ready row; a refusal takes the first, in the error colour,
// with its own fix under it, whichever row the cursor is on; a refusal with
// no fix (the command line's) leaves the second blank. With every row ready
// the box is today's: no detail lines.
func TestProviderPickerDrawsTheStates(t *testing.T) {
	log := &builtLog{}
	cfg := availPickerConfig(t, agent.CursorProvider(), availFixture, nil, log.build)
	const cmdRefusal = "craze: --agent-bin cannot be used with provider native, which runs inside craze"
	cfg.RefuseLoad = func(p agent.Provider) error {
		if p.InProcess() {
			return errors.New(cmdRefusal)
		}
		return nil
	}
	m := availPicker(t, cfg, 80, 24)
	th := m.theme
	dim, errSt := styleFG(th.Dim), styleFG(th.Err)
	inner, budget := m.lay.Dialog.W-dialogBorder, m.lay.Dialog.H-dialogBorder
	body := m.providerDialogBody(inner, budget)
	// title, cursor, grok, native, the reason, the fix, the footer
	if len(body) != 7 {
		t.Fatalf("%d body rows, want 7:\n%s", len(body), plain(strings.Join(body, "\n")))
	}
	cursorRow, grokRow, nativeRow := body[1], body[2], body[3]
	if want := styleFG(th.Dim).Background(th.SelectionBG).Render(plain(cursorRow)); cursorRow != want {
		t.Errorf("the unavailable row under the cursor is not dim on the selection band:\n got %q\nwant %q", cursorRow, want)
	}
	if want := dim.Render(plain(nativeRow)); nativeRow != want {
		t.Errorf("the needs-setup row is not dim:\n got %q\nwant %q", nativeRow, want)
	}
	if want := styleFG(th.FG).Render(plain(grokRow)); grokRow != want {
		t.Errorf("the ready row is not in the foreground:\n got %q\nwant %q", grokRow, want)
	}
	if !strings.HasSuffix(plain(cursorRow), "unavailable · default") || !strings.HasSuffix(plain(nativeRow), "needs setup") ||
		strings.TrimSpace(plain(grokRow)) != "grok" {
		t.Errorf("the tags: %q, %q, %q", plain(cursorRow), plain(grokRow), plain(nativeRow))
	}
	lines := func(m Model, how, first, fix string) {
		t.Helper()
		body := m.providerDialogBody(inner, budget)
		if len(body) != 7 || body[4] != first || body[5] != fix {
			t.Errorf("%s: %d rows, the detail lines\n got %q\n     %q\nwant %q\n     %q", how, len(body),
				plain(body[min(4, len(body)-1)]), plain(body[min(5, len(body)-1)]), plain(first), plain(fix))
		}
	}
	lines(m, "on cursor", dim.Render(clampWidth(availCursorReason, inner)), dim.Render(clampWidth(availCursorFix, inner)))
	onGrok, _ := press(m, tea.KeyMsg{Type: tea.KeyDown})
	lines(onGrok, "on grok, ready", "", "")
	onNative, _ := press(onGrok, tea.KeyMsg{Type: tea.KeyDown})
	lines(onNative, "on native", dim.Render(clampWidth(availNativeReason, inner)), dim.Render(clampWidth(availNativeFix, inner)))

	// A refusal takes the first line, in the error colour, its fix under it —
	// cursor's, refused by Esc while the cursor is on grok.
	refusedHead := "can't start cursor: " + availCursorReason
	next, _ := press(m, enter())
	lines(next, "Enter on cursor", errSt.Render(clampWidth(refusedHead, inner)), dim.Render(clampWidth(availCursorFix, inner)))
	if next.providerErr != availCursorRefusal {
		t.Errorf("the refusal kept is %q, want it whole: %q", next.providerErr, availCursorRefusal)
	}
	next, _ = press(onGrok, tea.KeyMsg{Type: tea.KeyEsc})
	lines(next, "Esc from grok", errSt.Render(clampWidth(refusedHead, inner)), dim.Render(clampWidth(availCursorFix, inner)))
	// The command line's refusal has no fix: the second line is blank.
	next, _ = press(onNative, enter())
	lines(next, "the command line's refusal", errSt.Render(clampWidth(cmdRefusal, inner)), "")

	// Every row ready: the box is exactly what it is with no availability.
	ready := availPicker(t, availPickerConfig(t, agent.CursorProvider(), func() []ProviderAvail {
		return []ProviderAvail{{ID: "cursor", State: AvailReady}, {ID: "grok", State: AvailReady}, {ID: "native", State: AvailReady}}
	}, nil, log.build), 80, 24)
	plainPicker := goldenPicker(t, agent.CursorProvider(), nil, 80, 24)
	if got, want := plainView(ready), plainView(plainPicker); got != want {
		t.Errorf("every row ready drew another box:\n%s\nwant\n%s", got, want)
	}
}

// TestProviderPickerFixLineGoesFirst (X22): short of height, the box gives
// up the fix line before the footer and before any list row, then keeps the
// first detail line as it keeps the error row — over the footer, never over
// the last list row — and the window keeps the cursor's row.
func TestProviderPickerFixLineGoesFirst(t *testing.T) {
	log := &builtLog{}
	m := availPicker(t, availPickerConfig(t, agent.CursorProvider(), availFixture, nil, log.build), 80, 24)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyUp}) // native, the last row
	for budget, want := range map[int]struct {
		shown             int
		line, fix, footer bool
	}{
		2: {shown: 1},
		3: {shown: 1, line: true},
		4: {shown: 1, line: true, footer: true},
		5: {shown: 2, line: true, footer: true},
		6: {shown: 3, line: true, footer: true},
		7: {shown: 3, line: true, fix: true, footer: true},
	} {
		top, shown, footer := m.providerDialogPlan(budget)
		line, fix := m.providerLineShown(budget), m.providerFixShown(budget)
		if shown != want.shown || line != want.line || fix != want.fix || footer != want.footer {
			t.Errorf("budget %d: shown %d line %v fix %v footer %v, want %+v", budget, shown, line, fix, footer, want)
		}
		if m.providerCursor < top || m.providerCursor >= top+shown {
			t.Errorf("budget %d: the window [%d,%d) lost the cursor", budget, top, top+shown)
		}
		body := m.providerDialogBody(dialogMaxWidth-dialogBorder, budget)
		if len(body) > budget {
			t.Errorf("budget %d: %d rows", budget, len(body))
		}
		if fix && strings.TrimSpace(plain(body[len(body)-2])) == "" {
			t.Errorf("budget %d: the fix line is blank on native", budget)
		}
	}
}

// TestProviderPickerDropsDefaultFirst (§3.3): too narrow for the state word
// and `default` both, the row keeps the state word; too narrow for that, it
// keeps neither (dialogRow's rule). The shipped labels fit both at craze's
// 40-column floor, so this is the box's rule at a narrower inner width.
func TestProviderPickerDropsDefaultFirst(t *testing.T) {
	log := &builtLog{}
	m := availPicker(t, availPickerConfig(t, agent.CursorProvider(), availFixture, nil, log.build), 80, 24)
	c := m.providerChoice(agent.CursorProvider())
	for _, tc := range []struct {
		inner int
		want  string
	}{
		{30, "unavailable · default"},
		{29, "unavailable"},
		{20, "unavailable"},
	} {
		if got := m.providerTag(c, "cursor", tc.inner); got != tc.want {
			t.Errorf("inner %d: tag %q, want %q", tc.inner, got, tc.want)
		}
	}
	for _, tc := range []struct {
		inner int
		want  string
	}{
		{29, "> cursor" + strings.Repeat(" ", 29-8-11) + "unavailable"},
		{19, "> cursor"},
	} {
		body := m.providerDialogBody(tc.inner, 40)
		if got := strings.TrimRight(plain(body[1]), " "); got != tc.want {
			t.Errorf("inner %d: the row %q, want %q", tc.inner, got, tc.want)
		}
	}
	if g := m.providerChoice(agent.GrokProvider()); m.providerTag(g, "grok", 10) != "" {
		t.Errorf("a ready row that is not the default has a tag")
	}
	if n := m.providerChoice(agent.NativeProvider()); m.providerTag(n, "native", 40) != "needs setup" {
		t.Errorf("native's tag %q", m.providerTag(n, "native", 40))
	}
}

// TestProviderPickerRefusesWhatCannotStart (A5): Enter on an unavailable row,
// Esc to an unavailable default and a click outside the box to it are each
// refused in place — the picker up, no session built, the refusal in the
// error row; Enter on native needing setup is not refused but opens the
// pre-session connect dialog (plan 036 §3.6, connect_pre_test.go has its
// every way in and out); a ready row then starts.
func TestProviderPickerRefusesWhatCannotStart(t *testing.T) {
	log := &builtLog{}
	m := availPicker(t, availPickerConfig(t, agent.CursorProvider(), availFixture, nil, log.build), 80, 24)
	refused := func(how string, m Model, cmd tea.Cmd, want string) {
		t.Helper()
		if !m.pickingProvider || m.dialog != dialogProvider || m.eng != nil || cmd != nil || len(log.got()) != 0 {
			t.Fatalf("%s: picking %v, dialog %v, engine %v, command %v, built %v — want the picker up and nothing built",
				how, m.pickingProvider, m.dialog, m.eng != nil, cmd != nil, log.got())
		}
		if m.providerErr != want {
			t.Fatalf("%s: the error row %q, want %q", how, m.providerErr, want)
		}
		if !strings.Contains(plainView(m), "can't start") {
			t.Fatalf("%s: no refusal on screen:\n%s", how, plainView(m))
		}
	}
	next, cmd := press(m, enter())
	refused("Enter on cursor", next, cmd, availCursorRefusal)
	next, cmd = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	refused("Esc to the default", next, cmd, availCursorRefusal)
	// From grok too: Esc is the default's, wherever the cursor is.
	onGrok, _ := press(m, tea.KeyMsg{Type: tea.KeyDown})
	next, cmd = press(onGrok, tea.KeyMsg{Type: tea.KeyEsc})
	refused("Esc from grok", next, cmd, availCursorRefusal)
	tm, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: 0})
	refused("a click outside the box", tm.(Model), cmd, availCursorRefusal)
	onNative, _ := press(m, tea.KeyMsg{Type: tea.KeyUp})
	next, _ = press(onNative, enter())
	if !next.preConnectOpen() || !next.pickingProvider || next.providerErr != "" || len(log.got()) != 0 {
		t.Fatalf("Enter on native: connect dialog %v, picking %v, error row %q, built %v — want the pre-session dialog over the picker",
			next.preConnectOpen(), next.pickingProvider, next.providerErr, log.got())
	}

	// Moving off the refused row clears it, and Enter on grok starts grok.
	next, _ = press(m, enter())
	next, _ = press(next, tea.KeyMsg{Type: tea.KeyDown})
	if next.providerErr != "" {
		t.Fatalf("the cursor moved and the refusal stayed: %q", next.providerErr)
	}
	next, cmd = press(next, enter())
	if next.pickingProvider || cmd == nil || !slices.Equal(log.got(), []string{"grok"}) {
		t.Fatalf("Enter on grok: picking %v, command %v, built %v", next.pickingProvider, cmd != nil, log.got())
	}
}

// TestProviderPickerRefuseLoadComesFirst (decision 4): the command line's
// refusal is asked first and is the one shown; availability is asked only of
// what it lets through.
func TestProviderPickerRefuseLoadComesFirst(t *testing.T) {
	log := &builtLog{}
	cfg := availPickerConfig(t, agent.CursorProvider(), availFixture, nil, log.build)
	const refusal = "craze: --agent-bin cannot be used with provider native, which runs inside craze"
	cfg.RefuseLoad = func(p agent.Provider) error {
		if p.InProcess() {
			return errors.New(refusal)
		}
		return nil
	}
	m := availPicker(t, cfg, 80, 24)
	onNative, _ := press(m, tea.KeyMsg{Type: tea.KeyUp})
	next, _ := press(onNative, enter())
	if next.providerErr != refusal {
		t.Fatalf("native, refused by both: %q, want the command line's", next.providerErr)
	}
	next, _ = press(m, enter())
	if next.providerErr != availCursorRefusal {
		t.Fatalf("cursor, which the command line lets through: %q", next.providerErr)
	}
}

// TestProviderPickerEveryRowUnavailable (A5): with no row that can start,
// every confirmation refuses — Enter on each row, Esc, a click outside — and
// Ctrl+C still quits.
func TestProviderPickerEveryRowUnavailable(t *testing.T) {
	log := &builtLog{}
	none := func() []ProviderAvail {
		return []ProviderAvail{
			{ID: "cursor", State: AvailUnavailable, Reason: availCursorReason, Fix: availCursorFix},
			{ID: "grok", State: AvailUnavailable, Reason: "grok not found on PATH", Fix: "install grok"},
			{ID: "native", State: AvailUnavailable, Reason: "no home directory for native's keys", Fix: `run "craze auth list" for the error`},
		}
	}
	m := availPicker(t, availPickerConfig(t, agent.CursorProvider(), none, nil, log.build), 80, 24)
	for i, p := range m.providers {
		at := m
		for range i {
			at, _ = press(at, tea.KeyMsg{Type: tea.KeyDown})
		}
		next, cmd := press(at, enter())
		if !next.pickingProvider || cmd != nil || !strings.HasPrefix(next.providerErr, "can't start "+p.DisplayName()+": ") {
			t.Fatalf("Enter on %s: picking %v, command %v, error %q", p.Name(), next.pickingProvider, cmd != nil, next.providerErr)
		}
	}
	for how, k := range map[string]tea.Msg{
		"Esc":                     tea.KeyMsg{Type: tea.KeyEsc},
		"a click outside the box": tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: 0},
	} {
		tm, cmd := m.Update(k)
		if next := tm.(Model); !next.pickingProvider || cmd != nil || next.providerErr != availCursorRefusal {
			t.Fatalf("%s: picking %v, command %v, error %q", how, next.pickingProvider, cmd != nil, next.providerErr)
		}
	}
	if got := log.got(); len(got) != 0 {
		t.Fatalf("built sessions of %v", got)
	}
	next, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if !next.quitting || cmd == nil {
		t.Fatalf("Ctrl+C: quitting %v, command %v", next.quitting, cmd != nil)
	}
	if _, ok := runWatched(t, cmd).(tea.QuitMsg); !ok {
		t.Fatal("Ctrl+C's command did not quit the program")
	}
}

// TestProviderPickerAsksOffTheUpdate (A5, X12): Config.Availability is called
// from a command, never from New, Init or Update — so one that blocks leaves
// every key answered — and until its answer lands every row is ready and a
// choice is taken as it always was.
func TestProviderPickerAsksOffTheUpdate(t *testing.T) {
	log := &builtLog{}
	release := make(chan struct{})
	var calls atomic.Int32
	blocked := func() []ProviderAvail {
		<-release
		return availFixture()
	}
	defer close(release)
	m := New(availPickerConfig(t, agent.CursorProvider(), blocked, &calls, log.build))
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(Model)
	cmd := m.Init()
	if cmd == nil || calls.Load() != 0 {
		t.Fatalf("Init: command %v, calls %d — want the call handed back, not made", cmd != nil, calls.Load())
	}
	answered := make(chan tea.Msg, 1)
	go func() { answered <- cmd() }()
	deadline := time.Now().Add(pumpWatchdog)
	for calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the availability command never called the callback")
		}
		time.Sleep(time.Millisecond)
	}
	// The callback is blocked: the keys are still the picker's, every row
	// ready — no detail line — and Enter on cursor would start it.
	done := make(chan Model, 1)
	go func() {
		next, _ := press(m, tea.KeyMsg{Type: tea.KeyDown})
		next, _ = press(next, tea.KeyMsg{Type: tea.KeyUp})
		_ = plainView(next)
		done <- next
	}()
	select {
	case next := <-done:
		if next.providerCursor != 0 || next.providerAnyNotReady() {
			t.Fatalf("while the callback blocks: cursor %d, a row not ready %v", next.providerCursor, next.providerAnyNotReady())
		}
	case <-time.After(pumpWatchdog):
		t.Fatal("Update blocked behind the availability callback")
	}
	started, cmd := press(m, enter())
	if started.pickingProvider || cmd == nil || !slices.Equal(log.got(), []string{"cursor"}) {
		t.Fatalf("Enter before the answer: picking %v, built %v — want it taken as today (X12)", started.pickingProvider, log.got())
	}
	select {
	case msg := <-answered:
		t.Fatalf("the callback answered before it was released: %T", msg)
	default:
	}
}

// TestProviderPickerTakesOnlyTheLatestAnswer (A5, X12, X25): an answer to an
// opening the picker has since replaced — a spawn refused brings it back, and
// asks again — changes nothing, before the latest lands or after; one that
// lands while a choice has closed the picker (its spawn pending) is dropped
// outright; and an opening starts with no answer, every row ready until its
// own lands, even when the opening before it took one. The cases deliver the
// first opening's answer before the confirmation (taken there, legitimately),
// between the confirmation and the spawn's refusal (the picker closed), and
// after the reopening (replaced).
func TestProviderPickerTakesOnlyTheLatestAnswer(t *testing.T) {
	second := []ProviderAvail{{ID: "cursor", State: AvailReady}, {ID: "grok", State: AvailReady}, {ID: "native", State: AvailReady}}
	// picker is a launch picker whose every spawn its host refuses, the
	// cursor on grok (ready either way), and the first opening's answer —
	// cursor unavailable — run but not delivered.
	picker := func(t *testing.T) (Model, providerAvailMsg, *atomic.Pointer[[]ProviderAvail]) {
		t.Helper()
		var answer atomic.Pointer[[]ProviderAvail]
		first := availFixture()
		answer.Store(&first)
		cfg := availPickerConfig(t, agent.CursorProvider(), func() []ProviderAvail { return *answer.Load() }, nil, (&builtLog{}).build)
		cfg.NewSession = nil
		cfg.NewBackend = func(agent.Provider, bool) (backend.Backend, error) {
			return nil, &Refusal{Err: errors.New("craze: grok refused by its host")}
		}
		m := New(cfg)
		tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		m = tm.(Model)
		stale := runWatched(t, m.Init()).(providerAvailMsg)
		m, _ = press(m, tea.KeyMsg{Type: tea.KeyDown})
		return m, stale, &answer
	}
	// confirm is Enter on grok: the spawn's command, the picker closed.
	confirm := func(t *testing.T, m Model) (Model, tea.Cmd) {
		t.Helper()
		m, cmd := press(m, enter())
		if m.pickingProvider {
			t.Fatal("fixture: Enter on grok did not spawn")
		}
		return m, cmd
	}
	// refuse runs the spawn, whose refusal brings the picker back asking
	// afresh, and answers that request's command.
	refuse := func(t *testing.T, m Model, cmd tea.Cmd) (Model, tea.Cmd) {
		t.Helper()
		spawned := runWatched(t, mustCmd(t, cmd, "spawnCmd")).(spawnedMsg)
		tm, cmd := m.Update(spawned)
		m = tm.(Model)
		if !m.pickingProvider || cmd == nil {
			t.Fatalf("the refused spawn: picking %v, command %v — want the picker back, asking again", m.pickingProvider, cmd != nil)
		}
		return m, cmd
	}
	allReady := func(t *testing.T, how string, m Model) {
		t.Helper()
		if m.provAvail != nil || m.providerAnyNotReady() || strings.Contains(plainView(m), "unavailable") {
			t.Fatalf("%s: the reopened picker draws an earlier answer (%v):\n%s", how, m.provAvail, plainView(m))
		}
	}

	t.Run("before the confirmation", func(t *testing.T) {
		m, stale, _ := picker(t)
		tm, _ := m.Update(stale)
		m = tm.(Model)
		if m.provAvail["cursor"].State != AvailUnavailable {
			t.Fatal("the control: the open picker did not take its own answer")
		}
		m, cmd := confirm(t, m)
		m, _ = refuse(t, m, cmd)
		allReady(t, "an answer taken before the confirmation", m)
	})
	t.Run("between the confirmation and the refusal", func(t *testing.T) {
		m, stale, _ := picker(t)
		m, cmd := confirm(t, m)
		tm, _ := m.Update(stale)
		m = tm.(Model)
		if m.provAvail != nil {
			t.Fatalf("an answer landing while the picker was closed was taken: %v", m.provAvail)
		}
		m, _ = refuse(t, m, cmd)
		allReady(t, "an answer landing while the spawn was pending", m)
	})
	t.Run("after the reopening", func(t *testing.T) {
		m, stale, answer := picker(t)
		m, cmd := confirm(t, m)
		m, cmd = refuse(t, m, cmd)
		answer.Store(&second)
		latest := runWatched(t, mustCmd(t, cmd, "providerAvailCmd")).(providerAvailMsg)
		if latest.seq == stale.seq {
			t.Fatal("fixture: the second opening's request is the first's")
		}
		// The first opening's answer, late: nothing changes.
		tm, _ := m.Update(stale)
		m = tm.(Model)
		allReady(t, "a replaced opening's answer", m)
		tm, _ = m.Update(latest)
		m = tm.(Model)
		tm, _ = m.Update(stale)
		m = tm.(Model)
		if m.providerAnyNotReady() || m.provAvail["cursor"].State != AvailReady {
			t.Fatalf("after the latest, the late answer of the first opening was taken: %v", m.provAvail)
		}
	})
}

// ------------------------------------------------------------ /provider

// availSessions is a startSessions that says which providers its sessions
// could run (ProviderAvailabilitySource): its answer as set, each call
// counted, and — while gate is set — each call held until gate closes.
type availSessions struct {
	*startSessions
	amu    sync.Mutex
	answer []ProviderAvail
	calls  int
	gate   chan struct{}
}

var _ ProviderAvailabilitySource = (*availSessions)(nil)

func (f *availSessions) ProviderAvailability() []ProviderAvail {
	f.amu.Lock()
	f.calls++
	ans, gate := slices.Clone(f.answer), f.gate
	f.amu.Unlock()
	if gate != nil {
		<-gate
	}
	return ans
}

func (f *availSessions) set(ans []ProviderAvail) {
	f.amu.Lock()
	f.answer = ans
	f.amu.Unlock()
}

func (f *availSessions) callCount() int {
	f.amu.Lock()
	defer f.amu.Unlock()
	return f.calls
}

// availListModel is newSessModel's model over an availSessions answering
// answer, its list not yet open.
func availListModel(t *testing.T, answer []ProviderAvail) (Model, *availSessions) {
	t.Helper()
	m, fs, _ := newSessModel(t, 100, 30)
	as := &availSessions{startSessions: fs, answer: answer}
	m.sessions = as
	return m, as
}

// openAvailList opens the list (←) over as, hands it a snapshot and its
// recent directories, and answers the command its opening returned, whose
// availability read the caller runs (mustCmd "readSessAvail") or not.
func openAvailList(t *testing.T, m Model, as *availSessions) (Model, tea.Cmd) {
	t.Helper()
	m, cmd := press(m, tea.KeyMsg{Type: tea.KeyLeft})
	if !m.sessList.open || !m.sessList.in.on {
		t.Fatalf("← did not open a list with an input:\n%s", plainView(m))
	}
	m = listSnap(t, m, richSnapshot(m.sessList.home, m.hereKey()))
	return listRecents(t, m, as.startSessions), cmd
}

// takeOpenRead runs the list's own availability read, which its opening
// returned in cmd, and hands its answer to m.
func takeOpenRead(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	msg, ok := runWatched(t, mustCmd(t, cmd, "readSessAvail")).(sessAvailMsg)
	if !ok {
		t.Fatalf("the list's read answered %T", msg)
	}
	tm, _ := m.Update(msg)
	return tm.(Model)
}

// providersLoaded types text — a `/provider …` line — and runs the
// availability read the opening of /provider's values starts, handing its
// answer back.
func providersLoaded(t *testing.T, m Model, text string) Model {
	t.Helper()
	m, cmd := typeList(t, m, text)
	msg, ok := runWatched(t, mustCmd(t, cmd, "startLoad")).(completeLoadedMsg)
	if !ok || msg.key != sessProvidersKey {
		t.Fatalf("/provider's load answered %+v", msg)
	}
	tm, _ := m.Update(msg)
	return tm.(Model)
}

// TestSessProviderDimsAndRefuses (A6): /provider's values carry each
// provider's state — the reason in place of its detail, the state word for
// its note — and Enter or Tab on one that cannot start takes nothing: the
// input and what new sessions run stay as they were, and the popup, still up,
// says why until the next key. Native needing setup carries no refusal: Enter
// on it opens the pre-session connect dialog instead (plan 036 §3.6,
// connect_pre_test.go). A ready one is taken; a typed `/provider <id>` with
// the popup put away is refused on the hint line, the line left as typed.
func TestSessProviderDimsAndRefuses(t *testing.T) {
	m, as := availListModel(t, availFixture())
	m, cmd := openAvailList(t, m, as)
	m = takeOpenRead(t, m, cmd)
	m = providersLoaded(t, m, "/provider ")
	if got := cmdItems(m); !slices.Equal(got, []string{"cursor", "grok", "native"}) {
		t.Fatalf("/provider lists %v", got)
	}
	cur, nat := cmdItem(t, m, "cursor"), cmdItem(t, m, "native")
	if cur.Detail != availCursorReason || cur.Note != "unavailable" || cur.Tone != completeToneDim || cur.Refusal != availCursorRefusal {
		t.Fatalf("cursor's item: %+v", cur)
	}
	if nat.Note != "needs setup" || nat.Tone != completeToneDim || nat.Refusal != "" {
		t.Fatalf("native's item: %+v", nat)
	}
	if g := cmdItem(t, m, "grok"); g.Refusal != "" || g.Note == "unavailable" {
		t.Fatalf("grok's item: %+v", g)
	}
	pickBefore := m.sessPick
	refused := func(how string, m Model) {
		t.Helper()
		in := m.sessList.in
		if v, _ := inputOf(m); v != "/provider " || !in.cmd.visible() || !in.cmd.ans.NoteErr ||
			in.cmd.ans.Note != availCursorRefusal || !reflect.DeepEqual(m.sessPick, pickBefore) {
			t.Fatalf("%s: input %q, popup %v, note %q (err %v), pick %+v — want all as they were, the refusal noted",
				how, v, in.cmd.visible(), in.cmd.ans.Note, in.cmd.ans.NoteErr, m.sessPick)
		}
		if len(in.cmd.ans.Items) != 3 {
			t.Fatalf("%s: the refusal took the candidates: %v", how, cmdItems(m))
		}
		view := plainView(m)
		if !strings.Contains(view, "  "+availCursorRefusal[:40]) {
			t.Fatalf("%s: the refusal is not on screen:\n%s", how, view)
		}
		if !strings.Contains(m.View(), ansiFG(string(m.theme.Err))+"  can't start") {
			t.Fatalf("%s: the refusal is not in the error colour", how)
		}
	}
	next, _ := press(m, enter())
	refused("Enter on cursor", next)
	// The next key ends it.
	moved, _ := press(next, tea.KeyMsg{Type: tea.KeyDown})
	if moved.sessList.in.cmd.ans.Note != "" || strings.Contains(plainView(moved), "can't start") {
		t.Fatalf("the refusal outlived the next key: %q", moved.sessList.in.cmd.ans.Note)
	}
	next, _ = press(m, tea.KeyMsg{Type: tea.KeyTab})
	refused("Tab on cursor", next)
	onNative, _ := press(m, tea.KeyMsg{Type: tea.KeyUp})
	next, _ = press(onNative, enter())
	if !next.preConnectOpen() || next.sessList.open || !reflect.DeepEqual(next.sessPick, pickBefore) {
		t.Fatalf("Enter on native: connect dialog %v, list open %v, pick %+v — want the pre-session dialog, nothing taken",
			next.preConnectOpen(), next.sessList.open, next.sessPick)
	}

	// A ready one is taken.
	onGrok, _ := press(m, tea.KeyMsg{Type: tea.KeyDown})
	next, _ = press(onGrok, enter())
	if !next.sessPick.provSet || next.sessPick.prov.Name() != "grok" {
		t.Fatalf("Enter on grok: pick %+v", next.sessPick)
	}

	// Typed in full, the popup put away: refused on the hint line, the line
	// left as typed and the pick as it was.
	typed := clearInput(t, next)
	typed, _ = typeList(t, typed, "/provider cursor")
	typed, _ = press(typed, tea.KeyMsg{Type: tea.KeyEsc})
	if typed.sessList.in.cmd.visible() {
		t.Fatal("fixture: esc left the popup up")
	}
	typed, _ = press(typed, enter())
	if v, _ := inputOf(typed); v != "/provider cursor" || typed.sessList.note != availCursorRefusal ||
		typed.sessList.noteKind != sessNoteErr || typed.sessPick.prov.Name() != "grok" {
		t.Fatalf("typed /provider cursor: input %q, note %q (%v), pick %s", v, typed.sessList.note, typed.sessList.noteKind,
			typed.sessPick.prov.Name())
	}
	if !strings.Contains(plainView(typed), availCursorRefusal[:40]) {
		t.Fatalf("the typed refusal is not on the hint line:\n%s", plainView(typed))
	}
}

// TestSessProviderTypedBeforeAnyAnswer (A6, X12): with no availability answer
// yet, a typed `/provider <id>` is taken as it always was.
func TestSessProviderTypedBeforeAnyAnswer(t *testing.T) {
	m, as := availListModel(t, availFixture())
	m, _ = openAvailList(t, m, as) // its read never delivered
	m, _ = typeList(t, m, "/provider cursor")
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	m, _ = press(m, enter())
	if !m.sessPick.provSet || m.sessPick.prov.Name() != "cursor" || m.sessList.noteKind == sessNoteErr {
		t.Fatalf("typed before any answer: pick %+v, note %q", m.sessPick, m.sessList.note)
	}
}

// TestSessProviderRecomputesAtEachOpening (A6): each opening of /provider's
// values reads the providers' availability again, so what it offers follows
// the answer — gx appearing, then going — with each provider's state; a
// typed line follows it too; and the list read as it opens again takes the
// answer as it stands then.
func TestSessProviderRecomputesAtEachOpening(t *testing.T) {
	allReady := []ProviderAvail{{ID: "cursor", State: AvailReady}, {ID: "grok", State: AvailReady}, {ID: "native", State: AvailReady}}
	m, as := availListModel(t, allReady)
	m, cmd := openAvailList(t, m, as)
	m = takeOpenRead(t, m, cmd)
	m = providersLoaded(t, m, "/provider ")
	if got := cmdItems(m); !slices.Equal(got, []string{"cursor", "grok", "native"}) {
		t.Fatalf("first opening: %v", got)
	}

	withGx := []ProviderAvail{
		{ID: "cursor", State: AvailUnavailable, Reason: availCursorReason, Fix: availCursorFix},
		{ID: "grok", State: AvailReady},
		{ID: "gx", State: AvailReady},
		{ID: "native", State: AvailNeedsSetup, Reason: availNativeReason, Fix: availNativeFix},
	}
	as.set(withGx)
	m = clearInput(t, m)
	m = providersLoaded(t, m, "/provider ")
	if got := cmdItems(m); !slices.Equal(got, []string{"cursor", "grok", "gx", "native"}) {
		t.Fatalf("gx installed: %v", got)
	}
	if cmdItem(t, m, "cursor").Refusal == "" || cmdItem(t, m, "gx").Refusal != "" || cmdItem(t, m, "native").Note != "needs setup" {
		t.Fatalf("the states did not follow the answer: %+v", m.sessList.in.cmd.ans.Items)
	}
	m = clearInput(t, m)
	m, _ = typeList(t, m, "/provider gx")
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	m, _ = press(m, enter())
	if m.sessPick.prov.Name() != "gx" {
		t.Fatalf("typed /provider gx with gx installed: pick %+v, note %q", m.sessPick, m.sessList.note)
	}

	as.set(allReady)
	m = clearInput(t, m)
	m = providersLoaded(t, m, "/provider ")
	if got := cmdItems(m); !slices.Equal(got, []string{"cursor", "grok", "native"}) {
		t.Fatalf("gx gone: %v", got)
	}
	m = clearInput(t, m)
	m, _ = typeList(t, m, "/provider gx")
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	m, _ = press(m, enter())
	if m.sessList.note != "no provider gx: one of cursor, grok, native" {
		t.Fatalf("typed /provider gx with gx gone: note %q", m.sessList.note)
	}

	// The list opened again reads the answer as it stands then.
	as.set(withGx)
	m = clearInput(t, m)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.sessList.open {
		t.Fatal("fixture: esc did not leave the list")
	}
	m, cmd = openAvailList(t, m, as)
	m = takeOpenRead(t, m, cmd)
	if got := m.sessProviderChoices(); len(got) != 4 || got[2].p.Name() != "gx" || got[0].refusal() == "" {
		t.Fatalf("the list opened again offers %+v", got)
	}
}

// TestSessProviderAsksOffTheUpdate (A6): the availability is read from
// commands, never in Update — so a source that blocks leaves every key
// answered — and /provider offers the picker's rows, every one ready, until
// it answers.
func TestSessProviderAsksOffTheUpdate(t *testing.T) {
	m, as := availListModel(t, availFixture())
	gate := make(chan struct{})
	as.gate = gate
	defer close(gate)
	m, open := openAvailList(t, m, as)
	m, load := typeList(t, m, "/provider ")
	if as.callCount() != 0 {
		t.Fatalf("Update called the source %d times", as.callCount())
	}
	if got := cmdItems(m); !slices.Equal(got, []string{"cursor", "grok", "native"}) || cmdItem(t, m, "cursor").Refusal != "" {
		t.Fatalf("before any answer /provider offers %v (cursor %+v)", got, cmdItem(t, m, "cursor"))
	}
	for _, c := range []tea.Cmd{mustCmd(t, open, "readSessAvail"), mustCmd(t, load, "startLoad")} {
		go func() { _ = c() }()
	}
	deadline := time.Now().Add(pumpWatchdog)
	for as.callCount() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the reads called the source %d times, want 2", as.callCount())
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan Model, 1)
	go func() {
		next, _ := press(m, tea.KeyMsg{Type: tea.KeyDown})
		_ = plainView(next)
		done <- next
	}()
	select {
	case next := <-done:
		if it, ok := next.sessList.in.cmd.selected(); !ok || it.Name != "grok" {
			t.Fatalf("while the source blocks ↓ selected %+v", it)
		}
	case <-time.After(pumpWatchdog):
		t.Fatal("Update blocked behind the availability source")
	}
}

// TestSessProviderTakesNoStaleAnswer (A6): an answer never replaces a newer
// one — the list's own read landing after /provider's — nor reaches an
// opening it was not asked for: the list's, or the popup's.
func TestSessProviderTakesNoStaleAnswer(t *testing.T) {
	older := []ProviderAvail{{ID: "cursor", State: AvailUnavailable, Reason: availCursorReason}, {ID: "grok", State: AvailReady}}
	newer := []ProviderAvail{{ID: "cursor", State: AvailReady}, {ID: "grok", State: AvailReady}, {ID: "gx", State: AvailReady}}
	ids := func(m Model) []string {
		var out []string
		for _, c := range m.sessProviderChoices() {
			out = append(out, c.p.Name()+":"+string(c.a.State))
		}
		return out
	}

	// The list's read starts first and lands last.
	m, as := availListModel(t, older)
	m, open := openAvailList(t, m, as)
	late := runWatched(t, mustCmd(t, open, "readSessAvail")).(sessAvailMsg)
	as.set(newer)
	m = providersLoaded(t, m, "/provider ")
	want := []string{"cursor:ready", "grok:ready", "gx:ready"}
	if got := ids(m); !slices.Equal(got, want) {
		t.Fatalf("after /provider's read: %v", got)
	}
	tm, _ := m.Update(late)
	m = tm.(Model)
	if got := ids(m); !slices.Equal(got, want) {
		t.Fatalf("the older read replaced the newer: %v", got)
	}

	// /provider's read for an opening of the popup since closed and opened
	// again is not taken.
	m2, as2 := availListModel(t, newer)
	m2, open2 := openAvailList(t, m2, as2)
	m2 = takeOpenRead(t, m2, open2)
	as2.set(older)
	m2, first := typeList(t, m2, "/provider ")
	stalePopup := runWatched(t, mustCmd(t, first, "startLoad")).(completeLoadedMsg)
	m2 = clearInput(t, m2)
	m2, _ = typeList(t, m2, "/provider ")
	tm, _ = m2.Update(stalePopup)
	m2 = tm.(Model)
	if got := ids(m2); !slices.Equal(got, want) {
		t.Fatalf("a closed popup's read was taken: %v", got)
	}

	// The list's read for an opening since left is not taken either — landing
	// before the new opening's own, so no newer answer stands in its way.
	m3, as3 := availListModel(t, older)
	m3, open3 := openAvailList(t, m3, as3)
	staleList := runWatched(t, mustCmd(t, open3, "readSessAvail")).(sessAvailMsg)
	m3, _ = press(m3, tea.KeyMsg{Type: tea.KeyEsc})
	as3.set(newer)
	m3, open3 = openAvailList(t, m3, as3)
	tm, _ = m3.Update(staleList)
	m3 = tm.(Model)
	if m3.sessList.availHave {
		t.Fatalf("a left opening's read was taken: %v", ids(m3))
	}
	m3 = takeOpenRead(t, m3, open3)
	if got := ids(m3); !slices.Equal(got, want) {
		t.Fatalf("the opening's own read: %v", got)
	}
}

// TestCompletePopupKeepsARefusalsLine: a refusal (refuse) is a line under
// the candidates while one candidate row is left beside it, and the next key
// ends it.
func TestCompletePopupKeepsARefusalsLine(t *testing.T) {
	p := newCompletePopup(&listSource{id: "refusal", items: named("a", "b")}, atGrammar)
	p.sync("@", 1, completeEnv{})
	p.refuse("can't start a: gone")
	for rows, want := range map[int]completeShape{
		1: {shown: 1},
		2: {shown: 1, note: true},
		3: {shown: 2, note: true},
	} {
		if got := p.shape(rows); got != want {
			t.Errorf("%d rows: %+v, want %+v", rows, got, want)
		}
	}
	lines := strings.Split(p.view(Preset("tokyo-night"), 40, 3), "\n")
	if len(lines) != 3 || strings.TrimSpace(plain(lines[2])) != "can't start a: gone" {
		t.Fatalf("the view: %q", lines)
	}
	p.key(tea.KeyMsg{Type: tea.KeyDown})
	if p.refused() || p.shape(3).note {
		t.Fatal("the next key left the refusal")
	}
}
