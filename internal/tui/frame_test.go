package tui

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

var updateGoldens = flag.Bool("update", false, "rewrite internal/tui/testdata/*.golden")

func TestParseFrameScriptTokens(t *testing.T) {
	toks, err := parseFrameScript("ab<enter><ctrl-o><shift-tab><alt-enter><lt><space>")
	if err != nil {
		t.Fatal(err)
	}
	want := []tea.KeyMsg{
		runeKey('a'),
		runeKey('b'),
		{Type: tea.KeyEnter},
		{Type: tea.KeyCtrlO},
		{Type: tea.KeyShiftTab},
		{Type: tea.KeyEnter, Alt: true},
		runeKey('<'),
		runeKey(' '),
	}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens, want %d", len(toks), len(want))
	}
	for i, tok := range toks {
		if tok.kind != tokKey {
			t.Fatalf("token %d kind %v", i, tok.kind)
		}
		if tok.key.String() != want[i].String() || tok.key.Type != want[i].Type {
			t.Fatalf("token %d = %q (%v), want %q (%v)", i, tok.key.String(), tok.key.Type, want[i].String(), want[i].Type)
		}
	}
}

func TestParseFrameScriptNonKeyTokens(t *testing.T) {
	toks, err := parseFrameScript("<wheel-up><wheel-down><click:10,5><resize:120,40><sleep:5ms>" +
		"<wait:idle><wait:working><wait:card><wait:text:TASKS n/n><wait:gone:Thinking><hover:3,4>")
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 11 {
		t.Fatalf("got %d tokens", len(toks))
	}
	if mouseOf(t, toks[0]).Button != tea.MouseButtonWheelUp || mouseOf(t, toks[1]).Button != tea.MouseButtonWheelDown {
		t.Fatalf("wheel tokens %+v %+v", toks[0].msgs, toks[1].msgs)
	}
	if mm := mouseOf(t, toks[2]); mm.X != 10 || mm.Y != 5 || mm.Button != tea.MouseButtonLeft {
		t.Fatalf("click %+v", mm)
	}
	if toks[3].size.Width != 120 || toks[3].size.Height != 40 {
		t.Fatalf("resize %+v", toks[3].size)
	}
	if toks[4].dur != 5*time.Millisecond {
		t.Fatalf("sleep %v", toks[4].dur)
	}
	wantWaits := []waitSpec{
		{kind: "idle"},
		{kind: "working"},
		{kind: "card"},
		{kind: "text", needle: "TASKS n/n"},
		{kind: "gone", needle: "Thinking"},
	}
	for i, want := range wantWaits {
		got := toks[5+i].wait
		if got != want {
			t.Fatalf("wait %d = %+v, want %+v", i, got, want)
		}
	}
	// A hover is a motion with no button held, which is what all-motion
	// reporting sends and what <motion:> (a drag) is not.
	if mm := mouseOf(t, toks[10]); mm.X != 3 || mm.Y != 4 || mm.Button != tea.MouseButtonNone || mm.Action != tea.MouseActionMotion {
		t.Fatalf("hover token %+v", toks[10].msgs)
	}
}

// mouseOf is the single mouse report a one-cell token carries.
func mouseOf(t *testing.T, tok frameToken) tea.MouseMsg {
	t.Helper()
	if tok.kind != tokMouse || len(tok.msgs) != 1 {
		t.Fatalf("token %+v is not one mouse report", tok)
	}
	mm, ok := tok.msgs[0].(tea.MouseMsg)
	if !ok {
		t.Fatalf("token message is %T, want tea.MouseMsg", tok.msgs[0])
	}
	return mm
}

// TestParseFrameScriptSelectionTokens is §3.5's token set: the three halves of a
// gesture, the shorthand that sends all three, and the deterministic
// double-click.
func TestParseFrameScriptSelectionTokens(t *testing.T) {
	toks, err := parseFrameScript("<press:3,4><motion:5,4><release:7,6><drag:1,2,8,9><dblclick:2,3><wait:copied>")
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 6 {
		t.Fatalf("got %d tokens", len(toks))
	}
	for i, want := range []tea.MouseMsg{
		{X: 3, Y: 4, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress},
		{X: 5, Y: 4, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion},
		// A release reports ButtonNone, the way an X10 terminal does.
		{X: 7, Y: 6, Button: tea.MouseButtonNone, Action: tea.MouseActionRelease},
	} {
		if got := mouseOf(t, toks[i]); got != want {
			t.Fatalf("token %d = %+v, want %+v", i, got, want)
		}
	}
	// One <drag:> is the whole gesture: press, motion, release.
	drag := toks[3]
	if len(drag.msgs) != 3 {
		t.Fatalf("<drag:> sent %d messages, want 3", len(drag.msgs))
	}
	for i, want := range []tea.MouseMsg{
		{X: 1, Y: 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress},
		{X: 8, Y: 9, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion},
		{X: 8, Y: 9, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease},
	} {
		if got := drag.msgs[i].(tea.MouseMsg); got != want {
			t.Fatalf("drag message %d = %+v, want %+v", i, got, want)
		}
	}
	if got, want := toks[4].msgs[0], (dblClickMsg{X: 2, Y: 3}); got != want {
		t.Fatalf("dblclick = %+v, want %+v", got, want)
	}
	if got := toks[5].wait; got != (waitSpec{kind: "copied"}) {
		t.Fatalf("wait = %+v", got)
	}
}

func TestParseFrameScriptPaste(t *testing.T) {
	toks, err := parseFrameScript(`<paste:one\ntwo>`)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 1 || toks[0].kind != tokKey {
		t.Fatalf("got %+v", toks)
	}
	key := toks[0].key
	if !key.Paste || key.Type != tea.KeyRunes || string(key.Runes) != "one\ntwo" {
		t.Fatalf("paste token is %+v, want one bracketed paste of two lines", key)
	}
}

func TestParseFrameScriptRejects(t *testing.T) {
	for _, script := range []string{
		"<nope>",
		"<hover:x>",
		"<paste:>",
		"<ctrl-1>",
		"<ctrl-aa>",
		"<enter",
		"<sleep:soon>",
		"<click:10>",
		"<press:10>",
		"<drag:1,2,3>",
		"<dblclick:x,y>",
		"<resize:0,10>",
		"<wait:done>",
		"<wait:text:>",
	} {
		if _, err := parseFrameScript(script); err == nil {
			t.Fatalf("%q parsed, want an error", script)
		} else {
			var se *ScriptError
			if !errors.As(err, &se) {
				t.Fatalf("%q: got %T, want *ScriptError", script, err)
			}
		}
	}
}

func TestWaitSpecMatch(t *testing.T) {
	idle := frameState{started: true, status: statusIdle, plain: "craze idle"}
	if !(waitSpec{kind: "idle"}).match(idle) {
		t.Fatal("idle should match a started idle frame")
	}
	if (waitSpec{kind: "idle"}).match(frameState{status: statusIdle}) {
		t.Fatal("idle must not match before Start returns")
	}
	if (waitSpec{kind: "idle"}).match(frameState{started: true, status: statusIdle, card: true}) {
		t.Fatal("idle must not match while a card is up")
	}
	if !(waitSpec{kind: "working"}).match(frameState{status: statusWorking}) {
		t.Fatal("working")
	}
	if !(waitSpec{kind: "card"}).match(frameState{card: true}) {
		t.Fatal("card")
	}
	if !(waitSpec{kind: "text", needle: "idle"}).match(idle) {
		t.Fatal("text")
	}
	if !(waitSpec{kind: "gone", needle: "working"}).match(idle) {
		t.Fatal("gone")
	}
}

func frameWorkspace(t *testing.T) string {
	t.Helper()
	// A fixed basename keeps the status line (workspace basename) golden-stable.
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

func runStubFrame(t *testing.T, cols, rows int, script string) string {
	t.Helper()
	isolateSkillsHome(t)
	plain, _, err := RunFrameScript(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: frameWorkspace(t),
		Model:     "grok",
		Yolo:      true,
	}, cols, rows, script, FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	return plain
}

// runStubFrameRaw is runStubFrame with the raw frame kept, so a test can assert
// on the escape sequences as well as the text. TestMain already forces a
// true-colour profile, so the raw frame carries real colours.
func runStubFrameRaw(t *testing.T, cols, rows int, script string) (string, string) {
	t.Helper()
	isolateSkillsHome(t)
	plain, raw, err := RunFrameScript(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: frameWorkspace(t),
		Model:     "grok",
		Yolo:      true,
	}, cols, rows, script, FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	return plain, raw
}

// runThemeFrame is runStubFrame with the theme named explicitly and the raw
// frame kept, so a test can assert on the palette and not only on the text.
func runThemeFrame(t *testing.T, cols, rows int, theme, script string) (string, string) {
	t.Helper()
	isolateSkillsHome(t)
	plainOut, raw, err := RunFrameScript(Config{
		Session:   NewStub(),
		Theme:     theme,
		Workspace: frameWorkspace(t),
		Model:     "grok",
		Yolo:      true,
	}, cols, rows, script, FrameOpts{Timeout: 10 * time.Second, ANSI: true})
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	return plainOut, raw
}

func assertGolden(t *testing.T, name string, cols, rows int, got string) {
	t.Helper()
	for _, ln := range strings.Split(got, "\n") {
		if w := lipgloss.Width(ln); w > cols {
			t.Fatalf("line is %d wide, max is %d: %q", w, cols, ln)
		}
	}
	// The frame owes the terminal every row it has and not one more; this is
	// the height contract the layout engine exists to keep.
	if h := lipgloss.Height(got); h != rows {
		t.Fatalf("frame is %d rows, want %d:\n%s", h, rows, got)
	}
	path := filepath.Join("testdata", name+".golden")
	if *updateGoldens {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (regenerate with: go test ./internal/tui -run TestFrameGolden -update)", err)
	}
	if string(want) != got {
		t.Fatalf("golden %s mismatch\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
	}
}

// assertFrameGolden is assertGolden plus the substrings a case names, so a
// golden diff says the frame changed and the assertions say what about it
// mattered. An empty name skips the golden, for a frame whose text is
// machine-specific and can only be asserted on in fragments.
func assertFrameGolden(t *testing.T, name string, cols, rows int, got string, want, absent []string) {
	t.Helper()
	if name != "" {
		assertGolden(t, name, cols, rows, got)
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("frame is missing %q:\n%s", w, got)
		}
	}
	for _, no := range absent {
		if strings.Contains(got, no) {
			t.Fatalf("frame should not contain %q:\n%s", no, got)
		}
	}
}

// echoPrompt is 224 characters, so the stub's "echo: " reply is 230, and opens
// with a 150-character unbroken token that only a hard wrap can break.
func echoPrompt() string {
	return strings.Repeat("x", 150) + " alphas bravo charlie delta echo foxtrot golf hotel india juliet kilo lima"
}

// composerLines is a draft of n numbered logical lines, typed the way a user
// makes one: alt+enter between them.
func composerLines(n int) string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("line %d of the draft", i))
	}
	return strings.Join(out, "<alt-enter>")
}

// composerRow is the nth row under the composer's titled top rule, so row 0 is
// the first input row and row ComposerRows is the closing rule.
func composerRow(t *testing.T, frame string, n int) string {
	t.Helper()
	rows := strings.Split(frame, "\n")
	for i, ln := range rows {
		if strings.HasSuffix(ln, " craze ─") && i+1+n < len(rows) {
			return rows[i+1+n]
		}
	}
	t.Fatalf("no titled composer rule in:\n%s", frame)
	return ""
}

// TestFrameGoldenComposer is §3.1's shape at every size the window has to
// cope with: the first row of the draft stays on screen with its prompt for
// as long as the draft fits the band, and the band follows the cursor once it
// does not.
func TestFrameGoldenComposer(t *testing.T) {
	const inner = 100 - composerPromptW
	for _, tc := range []struct {
		name       string
		cols, rows int
		keys       string
		wantFirst  string
		bandRows   int
	}{
		{"composer-one-line-100x30", 100, 30, "one line of draft", "❯ one line of draft", 1},
		{"composer-two-lines-100x30", 100, 30, "line one<alt-enter>line two", "❯ line one", 2},
		// 250 unbroken characters hard-wrap onto three rows and still start at
		// the prompt.
		{"composer-long-line-100x30", 100, 30, strings.Repeat("z", 250), "❯ " + strings.Repeat("z", inner), 3},
		// An exactly-full line is two rows, not one: bubbles spills the tail.
		{"composer-full-line-100x30", 100, 30, strings.Repeat("y", inner), "❯ " + strings.Repeat("y", inner), 2},
		{"composer-paste-100x30", 100, 30, `<paste:one\ntwo\nthree\nfour\nfive>`, "❯ one", 5},
		{"composer-six-lines-100x30", 100, 30, composerLines(6), "❯ line 1 of the draft", 6},
		// Nine lines window to six and the band follows the cursor down.
		{"composer-nine-lines-100x30", 100, 30, composerLines(9), "  line 4 of the draft", 6},
		{"composer-nine-lines-top-100x30", 100, 30,
			composerLines(9) + "<up><up><up><up><up><up><up><up>", "❯ line 1 of the draft", 6},
		// Under 30 rows the band caps at three, so five lines window there too.
		{"composer-five-lines-80x24", 80, 24, composerLines(5), "  line 3 of the draft", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runStubFrame(t, tc.cols, tc.rows, "<wait:idle>"+tc.keys)
			assertGolden(t, tc.name, tc.cols, tc.rows, got)
			if first := composerRow(t, got, 0); !strings.HasPrefix(first, tc.wantFirst) {
				t.Fatalf("first composer row is %q, want it to start %q", first, tc.wantFirst)
			}
			// The band is exactly the rows the window promised, between its
			// two rules.
			if row := composerRow(t, got, tc.bandRows); !strings.HasPrefix(row, "───") {
				t.Fatalf("row %d of the band is %q, want the closing rule", tc.bandRows, row)
			}
		})
	}
}

// slashCatalogLanded types a query only the catalog's last entry matches,
// waits for it, then backs out of the query again. The commands update lands
// after session/new has replied — cursor takes seconds over it — so a golden
// that only typed "/" would race the catalog and show the builtins alone.
// Waiting on an entry is waiting on the update that carried it, and the four
// backspaces leave exactly the token the golden is about.
const slashCatalogLanded = "/zulu<wait:text:zulu-tool><backspace><backspace><backspace><backspace>"

// TestFrameGoldenSlashMenu is §3.4's band through the real program and the
// real wire: the name column, the clamped descriptions, the gutter mark, the
// k/n ▲ ▼ marks at three granted heights, the accept, and the one-physical-row
// rule for a description the agent advertised with a newline in it.
//
// The fake's `commands` script advertises 24 entries, so the cursor fixture's
// ten builtins make 34 at message start and 24 with the token mid-message —
// the counts the marks have to agree with.
func TestFrameGoldenSlashMenu(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cols, rows int
		keys       string
		want       []string
		absent     []string
	}{
		// Eight of 34 from the top: the count, no ▲ at the top of the list,
		// and a ▼ because the other 26 are below.
		{"slash-open-100x30", 100, 30, slashCatalogLanded,
			[]string{"❯ /help", "1/34", "▼", "/model            Switch model"}, []string{"▲"}},
		// Ten rows down the window has moved three: the count follows the
		// selection, not the window, and both arrows are up.
		{"slash-scroll-100x30", 100, 30, slashCatalogLanded + strings.Repeat("<down>", 10),
			[]string{"❯ /alpha-review", "11/34 ▲", "▼", "/tasks"}, []string{"/help"}},
		// §3.5's click, on the same scrolled window slash-scroll-100x30 draws:
		// row 17 on screen is /tasks, items[3] because slashTop is 3 there, not
		// items[0] — a click that used the bare band row instead of
		// slashTop+row would have accepted /help.
		{"slash-click-100x30", 100, 30, slashCatalogLanded + strings.Repeat("<down>", 10) + "<click:5,17>",
			[]string{"❯ /tasks "}, []string{"❯ /help ", "❯ /alpha-review", "▲", "▼", "11/34"}},
		// Mid-message the builtins are not offered, so the same catalog is 24.
		{"slash-mid-80x24", 80, 24, "see " + slashCatalogLanded,
			[]string{"❯ /alpha-review", "1/24", "▼"}, []string{"/help", "/exit", "▲"}},
		// Twelve rows is the smallest frame craze draws; the band gets four of
		// them and the selection is still on screen five rows down.
		{"slash-crop-80x12", 80, 12, slashCatalogLanded + strings.Repeat("<down>", 5),
			[]string{"❯ /rename", "6/34 ▲", "▼"}, []string{"/help"}},
		// One match, so Tab needs no scrolling: the token becomes "/name " and
		// the trailing space closes the band by the rule, not by a flag.
		{"slash-accept-100x30", 100, 30, "/gau<wait:text:gauntlet-like><tab>",
			[]string{"❯ /gauntlet-like "}, []string{"1/1", "▼", "Discover, plan"}},
		// The advertised description carries a \n; sanitizeLine folded it at
		// catalog time, so the row is one line with both halves on it.
		{"slash-multiline-desc-100x30", 100, 30, "/multiline<wait:text:multiline-note>",
			[]string{"❯ /multiline-note  Draws on one line. The newline in this description must not split the row."},
			[]string{"1/1", "▲", "▼"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runFakeFrame(t, "commands", tc.cols, tc.rows, "<wait:idle>"+tc.keys)
			assertGolden(t, tc.name, tc.cols, tc.rows, got)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("frame is missing %q:\n%s", want, got)
				}
			}
			for _, no := range tc.absent {
				if strings.Contains(got, no) {
					t.Fatalf("frame should not contain %q:\n%s", no, got)
				}
			}
		})
	}
}

// TestFrameGoldenTitleRule is the other half of §3.1: the session title cursor
// sends lands at the right end of the top rule.
func TestFrameGoldenTitleRule(t *testing.T) {
	// The title update rides ahead of the reply, so the reply is what the
	// frame waits on and the title only has to have landed by the end.
	got := runFakeFrame(t, "title", 100, 30,
		"<wait:idle>go<enter><wait:text:echo: go><wait:idle><wait:text:Fake Title>")
	assertGolden(t, "title-100x30", 100, 30, got)
	if !strings.Contains(got, " Fake Title ─") {
		t.Fatalf("the title is missing from the rule:\n%s", got)
	}
	if strings.Contains(got, " craze ─") {
		t.Fatalf("the placeholder title survived the session title:\n%s", got)
	}
}

// TestFrameGoldenPlanMode is §3.3 end to end against the wire: the offer after
// a plan-mode turn, Enter on it, and a refinement instead.
//
// The waits are on the placeholder rather than on <wait:idle>, because the
// offer needs both EventDone and promptDoneMsg and those two race; waiting for
// what the offer draws is waiting for both.
func TestFrameGoldenPlanMode(t *testing.T) {
	const offered = "<wait:text:planned:><wait:text:enter implements this plan>"
	for _, tc := range []struct {
		name, keys string
		want       []string
	}{
		{
			"planmode-offer-100x30",
			"<wait:idle>plan it<enter>" + offered,
			[]string{"◆ plan", planOfferPlaceholder},
		},
		{
			// The fake answers "implementing" only if session/set_mode reached
			// it before session/prompt, so this golden is the proof that the
			// two are chained and not batched.
			"planmode-enter-100x30",
			"<wait:idle>plan it<enter>" + offered + "<enter><wait:text:implementing:><wait:idle>",
			[]string{"◆ agent", "mode → agent", "❯ Implement the plan above.", "implementing: Implement the plan above."},
		},
		{
			"planmode-refine-100x30",
			"<wait:idle>plan it<enter>" + offered +
				"add more detail<enter><wait:text:planned: add more detail><wait:text:enter implements this plan>",
			[]string{"◆ plan", "❯ add more detail", "planned: add more detail"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runFakeFrame(t, "planmode", 100, 30, tc.keys)
			assertGolden(t, tc.name, 100, 30, got)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("frame is missing %q:\n%s", want, got)
				}
			}
			if strings.Contains(got, "WRONG ORDER") {
				t.Fatalf("the fake saw session/prompt before session/set_mode:\n%s", got)
			}
		})
	}
}

func TestFrameGoldenEcho80x24(t *testing.T) {
	prompt := echoPrompt()
	if len(prompt) != 224 {
		t.Fatalf("prompt is %d chars, want 224", len(prompt))
	}
	got := runStubFrame(t, 80, 24, "<wait:idle>"+prompt+"<enter><wait:text:echo:><wait:idle>")
	assertGolden(t, "echo-80x24", 80, 24, got)

	broken := 0
	for _, ln := range strings.Split(got, "\n") {
		if strings.Contains(ln, strings.Repeat("x", 40)) {
			broken++
		}
	}
	if broken < 2 {
		t.Fatalf("the 150-char token was not broken across rows:\n%s", got)
	}
	if !strings.Contains(got, "lima") {
		t.Fatalf("reply tail missing, so the reply is not across 3 rows:\n%s", got)
	}
}

func TestFrameGoldenQuickNotQuit(t *testing.T) {
	got := runStubFrame(t, 80, 24, "<wait:idle>quick question")
	assertGolden(t, "quick-not-quit", 80, 24, got)
	if !strings.Contains(got, "quick question") {
		t.Fatalf("composer lost the typed text:\n%s", got)
	}
	// The session line only fills in once Start returned, so it proves craze
	// stayed up as well as the old "idle" word did.
	if !strings.Contains(got, "cursor │ Grok") {
		t.Fatalf("craze did not stay up:\n%s", got)
	}
}

func TestFrameWaitTimeoutReapsChild(t *testing.T) {
	isolateSkillsHome(t)
	bin := buildFakeAgent(t)
	ws := frameWorkspace(t)
	sess := agent.New(agent.Options{
		Binary:    bin,
		ExtraArgs: []string{"-script=hang"},
		Workspace: ws,
		Force:     true,
		Stderr:    io.Discard,
	})

	start := time.Now()
	_, _, err := RunFrameScript(Config{
		Session:   sess,
		Theme:     "tokyo-night",
		Workspace: ws,
		Yolo:      true,
	}, 80, 24, "<wait:idle>hang<enter><wait:idle>", FrameOpts{Timeout: 2 * time.Second})

	var te *WaitTimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("got %v, want *WaitTimeoutError", err)
	}
	if te.Wait != "<wait:idle>" {
		t.Fatalf("wait %q", te.Wait)
	}
	// The status rows carry no status word; the spinner line is what a working
	// frame shows.
	if !strings.Contains(te.LastFrame, "esc to interrupt") {
		t.Fatalf("diagnostics should carry the last frame, got:\n%s", te.LastFrame)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processRunning(t, bin) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fake-agent child still running after the frame timeout")
}

func TestFrameWaitsForStartBeforeTyping(t *testing.T) {
	isolateSkillsHome(t)
	stub := NewStub()
	stub.DelayStart(150 * time.Millisecond)
	plain, _, err := RunFrameScript(Config{
		Session:   stub,
		Theme:     "tokyo-night",
		Workspace: frameWorkspace(t),
		Model:     "grok",
		Yolo:      true,
	}, 80, 24, "hi<enter><wait:text:echo: hi>", FrameOpts{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("a script with no leading wait must still reach the agent: %v", err)
	}
	if !strings.Contains(plain, "echo: hi") {
		t.Fatalf("prompt was dropped before Start returned:\n%s", plain)
	}
}

func TestFrameQuitScriptDoesNotTimeOut(t *testing.T) {
	// The trailing tokens land after bubbletea has exited, where Send is a
	// no-op: the sync barrier must not wait them out.
	for _, script := range []string{
		"<wait:idle><ctrl-d>",
		"<wait:idle>/exit<enter><sleep:100ms><esc>",
		"<ctrl-d><sleep:100ms><esc><esc>",
	} {
		t.Run(script, func(t *testing.T) {
			isolateSkillsHome(t)
			start := time.Now()
			_, _, err := RunFrameScript(Config{
				Session:   NewStub(),
				Theme:     "tokyo-night",
				Workspace: frameWorkspace(t),
				Yolo:      true,
			}, 80, 24, script, FrameOpts{Timeout: 3 * time.Second})
			if err != nil {
				t.Fatalf("quit script returned %v", err)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("quit script waited %s, so it raced the sync barrier", elapsed)
			}
		})
	}
}

func TestFrameIsolatesHomeFromTheChild(t *testing.T) {
	isolateSkillsHome(t)
	outer := os.Getenv("HOME")
	dir := t.TempDir()
	record := filepath.Join(dir, "child-home")
	bin := filepath.Join(dir, "home-probe")
	probe := "#!/bin/sh\nprintf '%s' \"$HOME\" > " + record + "\nexit 1\n"
	if err := os.WriteFile(bin, []byte(probe), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := frameWorkspace(t)
	sess := agent.New(agent.Options{Binary: bin, Workspace: ws, Stderr: io.Discard})

	// An empty script still starts, fails, and cleans up; the child records the
	// HOME it was spawned with.
	if _, _, err := RunFrameScript(Config{
		Session:   sess,
		Theme:     "tokyo-night",
		Workspace: ws,
	}, 80, 24, "", FrameOpts{Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("empty script: %v", err)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("child did not run: %v", err)
	}
	if string(got) == "" {
		t.Fatal("child saw an empty HOME")
	}
	if string(got) == outer {
		t.Fatalf("child inherited the caller's HOME %q", outer)
	}
	if os.Getenv("HOME") != outer {
		t.Fatalf("HOME was not restored: %q", os.Getenv("HOME"))
	}
}

func TestRunFrameScriptRejectsBadScript(t *testing.T) {
	isolateSkillsHome(t)
	_, _, err := RunFrameScript(Config{
		Session:   NewStub(),
		Workspace: frameWorkspace(t),
	}, 80, 24, "<nope>", FrameOpts{Timeout: time.Second})
	var se *ScriptError
	if !errors.As(err, &se) {
		t.Fatalf("got %T (%v), want *ScriptError", err, err)
	}
}

// runFakeFrame drives a real Model against the scripted ACP server, which is
// the only way a golden exercises the whole protocol-to-transcript path.
func runFakeFrame(t *testing.T, script string, cols, rows int, keys string) string {
	t.Helper()
	return runFakeFrameForce(t, script, cols, rows, keys, true)
}

// runFakeFrameForce makes yolo explicit: --no-force is the only way the
// permission line is ever drawn.
func runFakeFrameForce(t *testing.T, script string, cols, rows int, keys string, force bool) string {
	t.Helper()
	return runFakeFrameProvider(t, script, cols, rows, keys, agent.CursorProvider(), force)
}

func runFakeFrameProvider(t *testing.T, script string, cols, rows int, keys string, p agent.Provider, force bool) string {
	t.Helper()
	return runFakeFrameOpts(t, script, cols, rows, keys, fakeFrameOpts{provider: p, force: force})
}

// runFakeFramePlugins is runFakeFrame with --plugin-dir's Options half threaded
// through. The frame runner builds its session in process, so the CLI flag
// never runs here: a golden that wants a plugin fixture has to hand the
// directories to agent.Options itself.
func runFakeFramePlugins(t *testing.T, script string, cols, rows int, keys string, dirs ...string) string {
	t.Helper()
	return runFakeFrameOpts(t, script, cols, rows, keys, fakeFrameOpts{force: true, pluginDirs: dirs})
}

// fakeFrameOpts is what a golden varies about its session beyond the script and
// the frame size. It is a struct rather than four more parameters because every
// wrapper above would otherwise have to grow each one — and so that a case
// needing a combination no wrapper spells can state it here instead of earning
// a wrapper of its own.
type fakeFrameOpts struct {
	// provider is the ACP dialect; the zero value takes cursor's, which is
	// what every golden but grok's wants.
	provider   agent.Provider
	force      bool
	freeze     bool
	pluginDirs []string
	// timeout is the per-wait timeout; zero takes the 15s the goldens have
	// always used.
	timeout time.Duration
}

func runFakeFrameOpts(t *testing.T, script string, cols, rows int, keys string, o fakeFrameOpts) string {
	t.Helper()
	bin := buildFakeAgent(t)
	isolateSkillsHome(t)
	ws := frameWorkspace(t)
	if o.provider.Name() == "" {
		o.provider = agent.CursorProvider()
	}
	prov := o.provider
	sess := agent.New(agent.Options{
		Binary:      bin,
		ExtraArgs:   []string{"-script=" + script},
		Workspace:   ws,
		Force:       o.force,
		PluginDirs:  o.pluginDirs,
		Interactive: true,
		Stderr:      io.Discard,
		Provider:    &prov,
	})
	timeout := o.timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	plain, _, err := RunFrameScript(Config{
		Session:        sess,
		Theme:          "tokyo-night",
		Workspace:      ws,
		Yolo:           o.force,
		Provider:       prov,
		ProviderLocked: true,
	}, cols, rows, keys, FrameOpts{Timeout: timeout, Freeze: o.freeze})
	if err != nil {
		t.Fatalf("run %s frame: %v", script, err)
	}
	return plain
}

func TestFrameGoldenMarkdown80x24(t *testing.T) {
	got := runFakeFrame(t, "markdown", 80, 24, "<wait:idle>go<enter><wait:text:Inline><wait:idle>")
	assertGolden(t, "markdown-80x24", 80, 24, got)
	if strings.ContainsAny(got, "`*") {
		t.Fatalf("markdown markers reached the screen:\n%s", got)
	}
}

func TestFrameGoldenMarkdown120x40(t *testing.T) {
	got := runFakeFrame(t, "markdown", 120, 40, "<wait:idle>go<enter><wait:text:Inline><wait:idle>")
	assertGolden(t, "markdown-120x40", 120, 40, got)
	for _, want := range []string{"+ Thought", "Heading", "• first item", "  │ go", "  │ func main() {"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "the shape of this reply") {
		t.Fatalf("a collapsed thought must not show its text:\n%s", got)
	}
}

func TestFrameGoldenThoughtExpanded120x40(t *testing.T) {
	got := runFakeFrame(t, "markdown", 120, 40,
		"<wait:idle>go<enter><wait:text:Inline><wait:idle><ctrl-o><wait:text:shape of this reply>")
	assertGolden(t, "thought-expanded-120x40", 120, 40, got)
	if !strings.Contains(got, "+ Thought") {
		t.Fatalf("the summary row stays above the expansion:\n%s", got)
	}
}

func TestFrameGoldenDiff80x24(t *testing.T) {
	got := runFakeFrame(t, "diff", 80, 24, "<wait:idle>go<enter><wait:text:done diff><wait:idle>")
	assertGolden(t, "diff-80x24", 80, 24, got)
	for _, want := range []string{"✓ read  main.go", "✓ edit  main.go  +1 −1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "✓ edit") != 1 {
		t.Fatalf("one row per tool call:\n%s", got)
	}
}

func TestFrameGoldenDiffExpanded120x40(t *testing.T) {
	got := runFakeFrame(t, "diff", 120, 40,
		"<wait:idle>go<enter><wait:text:done diff><wait:idle><ctrl-o><wait:text:package main>")
	assertGolden(t, "diff-expanded-120x40", 120, 40, got)
}

func TestFrameGoldenBash80x24(t *testing.T) {
	got := runFakeFrame(t, "bash", 80, 24, "<wait:idle>go<enter><wait:text:done bash><wait:idle>")
	assertGolden(t, "bash-80x24", 80, 24, got)
	if !strings.Contains(got, "✓ bash  go vet ./...  exit 127") {
		t.Fatalf("missing the exit code row:\n%s", got)
	}
	if !strings.Contains(got, "Command 'go' not found") {
		t.Fatalf("missing the stderr preview:\n%s", got)
	}
}

func TestFrameGoldenTask80x24(t *testing.T) {
	got := runFakeFrame(t, "task", 80, 24, "<wait:idle>go<enter><wait:text:done task><wait:idle>")
	assertGolden(t, "task-80x24", 80, 24, got)
	if !strings.Contains(got, "✓ agent  Count main.go lines  8.0s · grok-4.6-high-fast") {
		t.Fatalf("missing the completed agent row:\n%s", got)
	}
}

func TestFrameGoldenTaskLate80x24(t *testing.T) {
	got := runFakeFrame(t, "task-late", 80, 24, "<wait:idle>go<enter><wait:text:done task><wait:idle>")
	assertGolden(t, "task-late-80x24", 80, 24, got)
	if !strings.Contains(got, "✓ agent  Count main.go lines  8.0s · grok-4.6-high-fast") {
		t.Fatalf("a receipt that arrived first must still join:\n%s", got)
	}
}

// TestFrameTaskRowIsPaintedWhileRunning pins the state §3.4 gives the task row
// before its receipt lands. Nothing asserted it until now, and the fake used to
// send in_progress and completed back to back, so the running row existed for
// less than one render frame and no terminal ever drew it — the tmux smoke's
// "● agent then ✓ agent" expectation was unfalsifiable. The <wait:text:> is the
// assertion: it fails the run if that row is never published.
func TestFrameTaskRowIsPaintedWhileRunning(t *testing.T) {
	for _, script := range []string{"task", "task-late"} {
		t.Run(script, func(t *testing.T) {
			got := runFakeFrame(t, script, 80, 24,
				"<wait:idle>go<enter><wait:text:● agent  Count main.go lines  running>"+
					"<wait:text:done task><wait:idle>")
			if !strings.Contains(got, "✓ agent  Count main.go lines  8.0s") {
				t.Fatalf("the running row must end up completed:\n%s", got)
			}
		})
	}
}

func TestFrameGoldenTasks80x24(t *testing.T) {
	got := runFakeFrame(t, "tasks", 80, 24, "<wait:idle>go<enter><wait:text:done tasks><wait:idle>")
	assertGolden(t, "tasks-80x24", 80, 24, got)
	if !strings.Contains(got, "✓ agent  Subagent research") {
		t.Fatalf("the regex fallback should still make an agent row:\n%s", got)
	}
	if !strings.Contains(got, "✓ bash  echo hi") {
		t.Fatalf("missing the shell row:\n%s", got)
	}
}

func TestFrameGoldenTodosHidesTheTodoTool(t *testing.T) {
	got := runFakeFrame(t, "todos", 80, 24, "<wait:idle>go<enter><wait:text:done todos><wait:idle>")
	assertGolden(t, "todos-notes-80x24", 80, 24, got)
	if strings.Contains(got, "Update TODOs") {
		t.Fatalf("the todo writer reached the transcript:\n%s", got)
	}
	if !strings.Contains(got, "tasks: 3 planned") {
		t.Fatalf("missing the todo note:\n%s", got)
	}
	// 24 rows is the short form: header only, no task rows.
	if !strings.Contains(got, "TASKS 1/3") {
		t.Fatalf("missing the pinned panel header:\n%s", got)
	}
	if strings.Contains(got, "▸ Edit main.go") {
		t.Fatalf("a 24-row terminal degrades to a header-only panel:\n%s", got)
	}
}

func TestFrameGoldenTodos100x30(t *testing.T) {
	got := runFakeFrame(t, "todos", 100, 30, "<wait:idle>go<enter><wait:text:done todos><wait:idle>")
	assertGolden(t, "todos-100x30", 100, 30, got)
	for _, want := range []string{"TASKS 1/3", "┃ ▸ Edit main.go", "┃ ○ Run go vet"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Read main.go") {
		t.Fatalf("a completed item folds into the count:\n%s", got)
	}
	if strings.Contains(got, "esc to interrupt") {
		t.Fatalf("an idle frame has no spinner line:\n%s", got)
	}
}

func TestFrameGoldenTodosExpanded100x30(t *testing.T) {
	got := runFakeFrame(t, "todos", 100, 30,
		"<wait:idle>go<enter><wait:text:done todos><wait:idle><ctrl-t><wait:text:Read main.go>")
	assertGolden(t, "todos-expanded-100x30", 100, 30, got)
	if !strings.Contains(got, "┃ ✓ Read main.go") {
		t.Fatalf("expanded lists the completed item:\n%s", got)
	}
}

func TestFrameGoldenTooSmall30x8(t *testing.T) {
	got := runStubFrame(t, 30, 8, "<wait:idle>")
	assertGolden(t, "too-small-30x8", 30, 8, got)
	if !strings.Contains(got, "terminal too small") {
		t.Fatalf("missing the minimum-size message:\n%s", got)
	}
}

func TestFrameGoldenTaskRows100x30(t *testing.T) {
	got := runFakeFrame(t, "task", 100, 30, "<wait:idle>go<enter><wait:text:done task><wait:idle>")
	assertGolden(t, "task-rows-100x30", 100, 30, got)
	// The row lingers under the status rows after the sub-agent finished.
	if !strings.Contains(got, "✓ task  Count main.go lines  8.0s · grok-4.6-high-fast") {
		t.Fatalf("missing the lingering sub-agent row:\n%s", got)
	}
	rows := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if !strings.Contains(rows[len(rows)-1], "✓ task") {
		t.Fatalf("the agent rows belong under the status rows:\n%s", got)
	}
}

func TestFrameGoldenStatus60x24(t *testing.T) {
	got := runStubFrame(t, 60, 24, "<wait:idle>")
	assertGolden(t, "status-60x24", 60, 24, got)
	for _, want := range []string{
		"ws │ cursor │ Grok (medium) │ 0m",
		"◆ agent · shift+tab · ▸▸ bypass permissions on",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
}

// TestFrameGoldenAsk100x30 is the question card as cursor's own request drew
// it: one question at a time, with its position in the request.
func TestFrameGoldenAsk100x30(t *testing.T) {
	got := runFakeFrame(t, "ask", 100, 30, "<wait:idle>go<enter><wait:card>")
	assertGolden(t, "ask-100x30", 100, 30, got)
	for _, want := range []string{"question 1/2  Pick one", "> 1 A", "  2 B", "esc skip", "Waiting for your answer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Pick any") {
		t.Fatalf("only the question on screen is drawn:\n%s", got)
	}
}

// TestFrameAskAnswersBothQuestions walks the whole request: a number picks the
// single-select and advances, space and a number toggle the multi-select, and
// Enter on the last question sends the answer.
//
// §5's key string is `1<enter>` then `1<space>3<enter>`; Enter is not needed to
// leave a single-select (a number already picks and advances, §3.11), and an
// Enter there would confirm the second question with nothing picked, so the
// two questions are answered as `1` and `<space>3<enter>`.
func TestFrameAskAnswersBothQuestions(t *testing.T) {
	got := runFakeFrame(t, "ask", 100, 30,
		"<wait:idle>go<enter><wait:card>1<wait:text:question 2/2><space>3<enter><wait:text:asked:><wait:idle>")
	if !strings.Contains(got, "asked:answered:q1=opt-a;q2=opt-x,opt-z") {
		t.Fatalf("the agent did not see the answer:\n%s", got)
	}
	for _, want := range []string{"? Pick one → A", "? Pick any → X, Z"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing note %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "question 1/2") {
		t.Fatalf("the card should be gone once it is answered:\n%s", got)
	}
}

func TestFrameAskEscSkips(t *testing.T) {
	got := runFakeFrame(t, "ask", 100, 30, "<wait:idle>go<enter><wait:card><esc><wait:text:asked:><wait:idle>")
	if !strings.Contains(got, "asked:skipped:") {
		t.Fatalf("esc should skip the whole request:\n%s", got)
	}
	if !strings.Contains(got, "? Question → skipped") {
		t.Fatalf("missing the skipped note:\n%s", got)
	}
}

// TestFrameGoldenPlan100x30 shows both halves of §3.11's plan card: the plan
// itself as a transcript note block, and the two lines that answer it.
func TestFrameGoldenPlan100x30(t *testing.T) {
	got := runFakeFrame(t, "plan", 100, 30, "<wait:idle>go<enter><wait:card>")
	assertGolden(t, "plan-100x30", 100, 30, got)
	for _, want := range []string{
		"PLAN Fake Plan", "Two steps, then stop.", "Steps", "• read main.go",
		"○ Read main.go", "plan Fake Plan", "[a]ccept  [r]eject  esc cancel",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
}

func TestFramePlanDecisions(t *testing.T) {
	for _, tc := range []struct{ name, keys, want string }{
		{"accept", "a", "planned:accepted"},
		{"reject", "r", "planned:rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runFakeFrame(t, "plan", 100, 30,
				"<wait:idle>go<enter><wait:card>"+tc.keys+"<wait:text:planned:><wait:idle>")
			if !strings.Contains(got, tc.want) {
				t.Fatalf("missing %q:\n%s", tc.want, got)
			}
			if !strings.Contains(got, "plan Fake Plan → "+tc.name+"ed") {
				t.Fatalf("missing the decision note:\n%s", got)
			}
		})
	}
}

func TestFramePlanEscCancels(t *testing.T) {
	got := runFakeFrame(t, "plan", 100, 30, "<wait:idle>go<enter><wait:card><esc><wait:idle>")
	if strings.Contains(got, "planned:") {
		t.Fatalf("a cancelled plan produces no decision:\n%s", got)
	}
	if strings.Contains(got, "[a]ccept") {
		t.Fatalf("the card should be gone:\n%s", got)
	}
}

// TestFrameGoldenPermissionNoForce100x30 is the permission line in its pinned
// position, with the [A]lways the fake request offers.
func TestFrameGoldenPermissionNoForce100x30(t *testing.T) {
	got := runFakeFrameForce(t, "permission", 100, 30, "<wait:idle>go<enter><wait:card>", false)
	assertGolden(t, "permission-noforce-100x30", 100, 30, got)
	for _, want := range []string{
		"permission Shell  [a]llow once  [A]lways  [n] reject",
		"▸ prompting for permissions",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
}

func TestFramePermissionAlwaysPicksTheRequestsOwnID(t *testing.T) {
	got := runFakeFrameForce(t, "permission", 100, 30,
		"<wait:idle>go<enter><wait:card>A<wait:text:decision:><wait:idle>", false)
	if !strings.Contains(got, "decision:opt-always") {
		t.Fatalf("A should pick the allow_always option by its own id:\n%s", got)
	}
}

// TestFrameGoldenModelDialog is §3.4 end to end: the centred box over the
// transcript, the filter, and the effort and fast rows under the list.
func TestFrameGoldenModelDialog(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cols, rows int
		keys       string
		want       []string
	}{
		{"model-dialog-100x30", 100, 30, "<wait:idle>/model<enter>gro",
			[]string{"\u276f gro", "> Grok", "current", "effort", "fast", "esc"}},
		// Tab twice reaches the fast row, one right turns it on, and Enter
		// applies just that step: the note and status row 1 are the proof the
		// advertised value went out and came back.
		{"model-dialog-fast-100x30", 100, 30,
			"<wait:idle>/model<enter><tab><tab><right><enter><wait:text:fast \u2192 on>",
			[]string{"fast \u2192 on", "Grok (medium \u00b7 fast)"}},
		// The three focus states, pinned as three frames: Tab moves the "> "
		// gutter off the list and onto a toggle row, and the footer says which
		// keys are live. All of it survives the ANSI strip these goldens are.
		{"model-dialog-effort-100x30", 100, 30, "<wait:idle>/model<enter>gro<tab>",
			[]string{"· Grok", "> effort  low  [medium]  high", "  fast", "←→ change"}},
		{"model-dialog-fast-focus-100x30", 100, 30, "<wait:idle>/model<enter>gro<tab><tab>",
			[]string{"· Grok", "  effort", "> fast  [off]  on", "←→ change"}},
		{"model-dialog-80x24", 80, 24, "<wait:idle>/model<enter>", []string{"model", "[medium]", "[off]"}},
		// 40x12 is the smallest frame craze draws: the box shrinks, and every
		// band still owns its own rows.
		{"model-dialog-40x12", 40, 12, "<wait:idle>/model<enter>", []string{"model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runStubFrame(t, tc.cols, tc.rows, tc.keys)
			assertGolden(t, tc.name, tc.cols, tc.rows, got)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("frame is missing %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestFrameModelDialogFocusDrawsThreeFrames is the bug the goldens above could
// not see before this change: Tab cycled the focus and the stripped frame came
// out identical, so nothing on screen said which row the arrows would move.
func TestFrameModelDialogFocusDrawsThreeFrames(t *testing.T) {
	seen := map[string]string{}
	for _, tc := range []struct{ name, keys string }{
		{"list", "<wait:idle>/model<enter>gro"},
		{"effort", "<wait:idle>/model<enter>gro<tab>"},
		{"fast", "<wait:idle>/model<enter>gro<tab><tab>"},
	} {
		got := runStubFrame(t, 100, 30, tc.keys)
		if prev, dup := seen[got]; dup {
			t.Fatalf("focus %q draws the same frame as %q:\n%s", tc.name, prev, got)
		}
		seen[got] = tc.name
		if n := strings.Count(got, dialogCursorMark); n != 1 {
			t.Fatalf("focus %q draws %d cursor gutters, want exactly one:\n%s", tc.name, n, got)
		}
	}
}

// TestFrameGoldenHelpDialog is Task 2's help box in the shared dialog frame:
// centred, grouped under headings, one key per row in two aligned columns, and
// scrolled rather than overflowing. 40x12 is the floor, where the box is a
// title and nothing else.
func TestFrameGoldenHelpDialog(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cols, rows int
		keys       string
		want       []string
	}{
		{"help-100x30", 100, 30, "<wait:idle>/help<enter>", []string{
			"help", "sending and editing", "  enter             send the draft",
			"  shift+tab         cycle the mode: agent, plan, ask", "▼",
		}},
		// Paged to the bottom: the command sections, and the agent's own
		// commands under their own heading so they cannot read as builtins.
		{"help-bottom-100x30", 100, 30, "<wait:idle>/help<enter><pgdn><pgdn><pgdn><pgdn>", []string{
			"commands", "  /model            Switch model", "  /exit             Quit craze",
			"this session's commands", "  /research         Agent-advertised command", "▲",
		}},
		{"help-80x24", 80, 24, "<wait:idle>/help<enter>", []string{"help", "sending and editing"}},
		{"help-40x12", 40, 12, "<wait:idle>/help<enter>", []string{"help"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runStubFrame(t, tc.cols, tc.rows, tc.keys)
			assertGolden(t, tc.name, tc.cols, tc.rows, got)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("frame is missing %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestFrameClickOpensModelDialog is the third opener through the real program:
// a press on the model name in status row 1 draws the same frame /model does.
func TestFrameClickOpensModelDialog(t *testing.T) {
	x, y := frameModelSpan(t, 100, 30)
	clicked := runStubFrame(t, 100, 30, fmt.Sprintf("<wait:idle><click:%d,%d>", x, y))
	typed := runStubFrame(t, 100, 30, "<wait:idle>/model<enter>")
	if clicked != typed {
		t.Fatalf("the click frame differs from the /model frame\n--- click ---\n%s\n--- /model ---\n%s", clicked, typed)
	}
	if !strings.Contains(clicked, modelDialogHint) {
		t.Fatalf("clicking the model span drew no dialog:\n%s", clicked)
	}
}

// frameModelSpan is the middle of row 1's model name in a frame of this size,
// taken from the same fitting pass the frame runs.
func frameModelSpan(t *testing.T, cols, rows int) (x, y int) {
	t.Helper()
	isolateSkillsHome(t)
	m := New(Config{
		Session:   NewStub(),
		Theme:     "tokyo-night",
		Workspace: frameWorkspace(t),
		Model:     "grok",
		Yolo:      true,
	})
	tm, _ := m.Update(tea.WindowSizeMsg{Width: cols, Height: rows})
	m = tm.(Model)
	tm, _ = m.Update(startedMsg{})
	m = tm.(Model)
	_, spans := m.statusRow1()
	for _, s := range spans {
		if s.id == spanModel {
			return (s.x0 + s.x1) / 2, m.lay.Region(regionStatus).Top
		}
	}
	t.Fatal("no model span in status row 1")
	return 0, 0
}

// TestFrameGoldenThemeDialog100x30 runs the picker through the real program in
// its V3 home, the shared dialog frame: the names-only list, and the palette
// changing under the cursor before Enter has been pressed.
func TestFrameGoldenThemeDialog100x30(t *testing.T) {
	got, raw := runThemeFrame(t, 100, 30, "craze-dark", "<wait:idle><ctrl-g>")
	assertGolden(t, "theme-dialog-100x30", 100, 30, got)
	for _, want := range ThemeNames() {
		if !strings.Contains(got, want) {
			t.Fatalf("the picker should list %q:\n%s", want, got)
		}
	}
	if !strings.Contains(raw, ansiFG("#e8a33d")) {
		t.Fatal("the craze-dark accent is missing from the raw frame")
	}

	// Six moves down the frozen list reach gruvbox; the screen is re-themed on
	// the way, with nothing written to disk yet.
	_, moved := runThemeFrame(t, 100, 30, "craze-dark", "<wait:idle><ctrl-g><down><down><down><down><down><down>")
	if !strings.Contains(moved, ansiFG("#fabd2f")) {
		t.Fatal("moving the cursor did not repaint the frame in gruvbox")
	}
	if strings.Contains(moved, ansiFG("#e8a33d")) {
		t.Fatal("the craze-dark accent survived the live preview")
	}

	// Esc puts the palette back.
	_, reverted := runThemeFrame(t, 100, 30, "craze-dark", "<wait:idle><ctrl-g><down><esc>")
	if !strings.Contains(reverted, ansiFG("#e8a33d")) {
		t.Fatal("esc did not restore the craze-dark accent")
	}
}

// TestFrameGoldenSelection is §3.5 through the real program: a drag highlights
// the rows it covers, copies them, and says so in status row 2. The goldens are
// ANSI-stripped, so the highlight itself is asserted on the raw frame.
func TestFrameGoldenSelection(t *testing.T) {
	// "hi" gives two adjacent transcript rows at the top of the band: the user
	// line and the reply.
	const said = "<wait:idle>hi<enter><wait:text:echo: hi><wait:idle>"
	for _, tc := range []struct {
		name string
		keys string
		want []string
		// cells are (row, col) pairs that must carry the selection background.
		cells [][2]int
		// clear are cells that must not.
		clear [][2]int
	}{
		{
			// Forwards, from the start of the user line to the middle of the
			// reply.
			name:  "select-two-lines-100x30",
			keys:  said + "<drag:0,0,5,1><wait:copied>",
			want:  []string{"copied 2 lines", "❯ hi", "echo: hi"},
			cells: [][2]int{{0, 0}, {0, 99}, {1, 0}, {1, 5}},
			clear: [][2]int{{1, 6}, {2, 0}},
		},
		{
			// The same selection drawn backwards: press on the later cell and
			// release on the earlier one.
			name:  "select-reverse-100x30",
			keys:  said + "<drag:5,1,0,0><wait:copied>",
			want:  []string{"copied 2 lines"},
			cells: [][2]int{{0, 0}, {0, 99}, {1, 0}, {1, 5}},
			clear: [][2]int{{1, 6}, {2, 0}},
		},
		{
			// The deterministic double-click: the word under the pointer, with
			// no dependence on the clock.
			name:  "dblclick-word-100x30",
			keys:  said + "<dblclick:2,1><wait:copied>",
			want:  []string{`copied "echo:"`},
			cells: [][2]int{{1, 0}, {1, 4}},
			clear: [][2]int{{1, 5}, {0, 0}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain, raw := runStubFrameRaw(t, 100, 30, tc.keys)
			assertGolden(t, tc.name, 100, 30, plain)
			for _, want := range tc.want {
				if !strings.Contains(plain, want) {
					t.Fatalf("frame is missing %q:\n%s", want, plain)
				}
			}
			assertSelected(t, raw, tc.cells, tc.clear)
		})
	}
}

// TestFrameGoldenSelectStyledRow drags over a diff row, which already has a
// background of its own: the selection's has to win for the cells it covers and
// the row has to keep its width.
func TestFrameGoldenSelectStyledRow(t *testing.T) {
	bin := buildFakeAgent(t)
	isolateSkillsHome(t)
	ws := frameWorkspace(t)
	sess := agent.New(agent.Options{
		Binary:      bin,
		ExtraArgs:   []string{"-script=diff"},
		Workspace:   ws,
		Force:       true,
		Interactive: true,
		Stderr:      io.Discard,
	})
	// Rows 10 and 11 of the expanded diff are the − and + lines.
	plain, raw, err := RunFrameScript(Config{
		Session:   sess,
		Theme:     "tokyo-night",
		Workspace: ws,
		Yolo:      true,
	}, 100, 30, "<wait:idle>go<enter><wait:text:done diff><wait:idle><ctrl-o>"+
		"<wait:text:package main><drag:5,10,45,11><wait:copied>", FrameOpts{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("run diff frame: %v", err)
	}
	assertGolden(t, "select-styled-row-100x30", 100, 30, plain)
	if !strings.Contains(plain, "copied 2 lines") {
		t.Fatalf("the drag over the diff did not copy:\n%s", plain)
	}
	assertSelected(t, raw, [][2]int{{10, 5}, {10, 99}, {11, 0}, {11, 45}}, [][2]int{{11, 46}, {10, 4}})
}

// assertSelected checks the selection background cell by cell on the raw frame,
// which is the only place the highlight exists: the goldens are stripped.
func assertSelected(t *testing.T, raw string, on, off [][2]int) {
	t.Helper()
	bg := selectionSeq(Preset("tokyo-night").SelectionBG)
	if bg == "" {
		t.Skip("no colour profile")
	}
	rows := strings.Split(raw, "\n")
	for _, want := range [][2]interface{}{{on, true}, {off, false}} {
		for _, cell := range want[0].([][2]int) {
			row, col := cell[0], cell[1]
			if row >= len(rows) {
				t.Fatalf("row %d is off the frame", row)
			}
			cells := selectedCells(rows[row], bg)
			if col >= len(cells) {
				t.Fatalf("cell %d,%d is off the row (%d cells)", row, col, len(cells))
			}
			if got := cells[col]; got != want[1].(bool) {
				t.Fatalf("cell %d,%d selected=%v, want %v\nrow: %q", row, col, got, want[1], rows[row])
			}
		}
	}
}

func TestFrameGoldenGrokEcho(t *testing.T) {
	keys := "<wait:idle>go<enter><wait:text:echo: go><wait:idle>"
	for _, size := range []struct{ cols, rows int }{{80, 24}, {100, 30}} {
		got := runFakeFrameProvider(t, "grok-echo", size.cols, size.rows, keys, agent.GrokProvider(), true)
		name := fmt.Sprintf("grok-echo-%dx%d", size.cols, size.rows)
		assertGolden(t, name, size.cols, size.rows, got)
		if !strings.Contains(got, " │ grok │ ") {
			t.Fatalf("%s missing grok provider:\n%s", name, got)
		}
		if strings.Contains(got, " │ cursor │ ") {
			t.Fatalf("%s still shows cursor:\n%s", name, got)
		}
		if strings.Contains(got, "fast") {
			t.Fatalf("%s must not show fast:\n%s", name, got)
		}
		if !strings.Contains(got, "◆ default") {
			t.Fatalf("%s missing mode chip:\n%s", name, got)
		}
	}
}

func TestFrameGoldenGrokAsk(t *testing.T) {
	keys := "<wait:idle>go<enter><wait:card>"
	for _, size := range []struct{ cols, rows int }{{80, 24}, {100, 30}} {
		got := runFakeFrameProvider(t, "grok-ask", size.cols, size.rows, keys, agent.GrokProvider(), true)
		name := fmt.Sprintf("grok-ask-%dx%d", size.cols, size.rows)
		assertGolden(t, name, size.cols, size.rows, got)
		for _, want := range []string{"question 1/2  Pick one", "> 1 A", "  2 B", "esc skip"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s missing %q:\n%s", name, want, got)
			}
		}
	}
}

func TestFrameGoldenGrokPlan(t *testing.T) {
	keys := "<wait:idle>go<enter><wait:card>"
	for _, size := range []struct{ cols, rows int }{{80, 24}, {100, 30}} {
		got := runFakeFrameProvider(t, "grok-plan", size.cols, size.rows, keys, agent.GrokProvider(), true)
		name := fmt.Sprintf("grok-plan-%dx%d", size.cols, size.rows)
		assertGolden(t, name, size.cols, size.rows, got)
		for _, want := range []string{"[a]ccept", "[r]eject", "esc cancel"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s missing %q:\n%s", name, want, got)
			}
		}
	}
}

func TestFrameGoldenGrokSubagentRows(t *testing.T) {
	keys := "<wait:idle>go<enter><wait:text:4.7k tok>"
	for _, size := range []struct{ cols, rows int }{{80, 24}, {100, 30}} {
		got := runFakeFrameProvider(t, "grok-subagent", size.cols, size.rows, keys, agent.GrokProvider(), true)
		name := fmt.Sprintf("grok-subagent-rows-%dx%d", size.cols, size.rows)
		assertGolden(t, name, size.cols, size.rows, got)
		if !strings.Contains(got, "○ explore  List directory files  0s · 4.7k tok") {
			t.Fatalf("%s missing the running row:\n%s", name, got)
		}
	}
}

func TestFrameGoldenGrokSubagentView(t *testing.T) {
	keys := "<wait:idle>go<enter><wait:text:4.7k tok><down><enter><wait:text:✓ tool  list_dir>"
	for _, size := range []struct{ cols, rows int }{{80, 24}, {100, 30}} {
		got := runFakeFrameProvider(t, "grok-subagent", size.cols, size.rows, keys, agent.GrokProvider(), true)
		name := fmt.Sprintf("grok-subagent-view-%dx%d", size.cols, size.rows)
		assertGolden(t, name, size.cols, size.rows, got)
		if !strings.Contains(got, "esc to return") {
			t.Fatalf("%s missing the banner:\n%s", name, got)
		}
		if !strings.Contains(got, "(grok-4.6)") {
			t.Fatalf("%s missing the chip:\n%s", name, got)
		}
		if !strings.Contains(got, "✓ tool  list_dir") {
			t.Fatalf("%s missing the child tool row:\n%s", name, got)
		}
	}
}

func TestFrameGoldenGrokSubagentViewDone100x30(t *testing.T) {
	got := runFakeFrameProvider(t, "grok-subagent", 100, 30,
		"<wait:idle>go<enter><wait:text:4.7k tok><down><enter><wait:idle>",
		agent.GrokProvider(), true)
	assertGolden(t, "grok-subagent-view-done-100x30", 100, 30, got)
	if !strings.Contains(got, "esc to return") {
		t.Fatalf("missing the banner:\n%s", got)
	}
	if !strings.Contains(got, "completed") {
		t.Fatalf("finished while viewed should Warn completed:\n%s", got)
	}
}

func TestFrameGoldenGrokSubagentTwoView100x30(t *testing.T) {
	got := runFakeFrameProvider(t, "grok-subagent-two", 100, 30,
		"<wait:idle>go<enter><wait:text:4.7k tok><down><enter><tab><wait:text:✓ tool  read_file>",
		agent.GrokProvider(), true)
	assertGolden(t, "grok-subagent-two-view-100x30", 100, 30, got)
	if !strings.Contains(got, "Report README first line") {
		t.Fatalf("tab should switch to sub-2:\n%s", got)
	}
	if !strings.Contains(got, "esc to return · tab next agent") {
		t.Fatalf("missing the tab hint after esc to return:\n%s", got)
	}
}

func TestFrameGoldenGrokSubagentFail80x24(t *testing.T) {
	got := runFakeFrameProvider(t, "grok-subagent-fail", 80, 24,
		"<wait:idle>go<enter><wait:text:✗ explore><wait:idle>",
		agent.GrokProvider(), true)
	assertGolden(t, "grok-subagent-fail-80x24", 80, 24, got)
	if !strings.Contains(got, "✗ explore") {
		t.Fatalf("missing the failed row:\n%s", got)
	}
}

func TestFrameGoldenGrokSubagentCancel100x30(t *testing.T) {
	got := runFakeFrameProvider(t, "grok-subagent-cancel", 100, 30,
		"<wait:idle>go<enter><wait:text:○ general-purpose><esc><wait:text:– general-purpose><down><enter><wait:text:Execute sleep>",
		agent.GrokProvider(), true)
	assertGolden(t, "grok-subagent-cancel-100x30", 100, 30, got)
	if !strings.Contains(got, "cancelled") {
		t.Fatalf("missing the cancelled banner:\n%s", got)
	}
	if !strings.Contains(got, "– tool  Execute sleep 45 && echo finished") {
		t.Fatalf("missing the cancelled child tool row:\n%s", got)
	}
}

func TestFrameGoldenGrokSubagentLate80x24(t *testing.T) {
	got := runFakeFrameProvider(t, "grok-subagent-late", 80, 24,
		"<wait:idle>go<enter><wait:idle>",
		agent.GrokProvider(), true)
	assertGolden(t, "grok-subagent-late-80x24", 80, 24, got)
	if !strings.Contains(got, "○") {
		t.Fatalf("parent idle, the child should still be running:\n%s", got)
	}
	if !strings.Contains(got, "← 1 agent") {
		t.Fatalf("missing the agent count:\n%s", got)
	}
}

func TestFrameGoldenTaskView100x30(t *testing.T) {
	got := runFakeFrame(t, "task", 100, 30,
		"<wait:idle>go<enter><wait:text:done task><wait:idle><down><enter>")
	assertGolden(t, "task-view-100x30", 100, 30, got)
	if !strings.Contains(got, "receipt only") {
		t.Fatalf("cursor view is receipt-only:\n%s", got)
	}
	if !strings.Contains(got, "esc to return") {
		t.Fatalf("missing the banner:\n%s", got)
	}
}

func TestFrameGrokModelDialogHasNoFastRow(t *testing.T) {
	got := runFakeFrameProvider(t, "grok-echo", 100, 30, "<wait:idle>/model<enter>", agent.GrokProvider(), true)
	if strings.Contains(got, "fast") {
		t.Fatalf("grok model dialog must not draw fast:\n%s", got)
	}
	if !strings.Contains(got, "effort") {
		t.Fatalf("grok model dialog should still draw effort:\n%s", got)
	}
}

// runFakeFrameFrozen is runFakeFrameProvider with the clock and the spinner
// stopped, so a golden of a turn in progress does not depend on how fast the
// build is.
func runFakeFrameFrozen(t *testing.T, script string, cols, rows int, keys string, p agent.Provider) string {
	t.Helper()
	return runFakeFrameOpts(t, script, cols, rows, keys, fakeFrameOpts{
		provider: p,
		force:    true,
		freeze:   true,
		timeout:  20 * time.Second,
	})
}
