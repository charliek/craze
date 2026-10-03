package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/chatgptauth"
)

// /connect's sign-in address as something to copy and click (plan 034 §3.2,
// W2: A5–A8): each row of the address a terminal hyperlink (OSC 8) to the
// whole address under the attempt's id, balanced in every frame craze draws
// and plain where it may not be one; a click on it, or Ctrl+Y, copying it
// whole with a line in the box; the hint over SSH or with no listener; and
// the listener's news (Q8). The stand-in attempt is connect_signin_test.go's,
// its id fixed (signInAttemptID), so a raw frame's link is too.

// The stand-in's address link's two escapes, as the step must write them
// (plan 034 Q9): BEL-terminated — x/ansi's form — the open with the attempt's
// id and the whole address, the close with neither.
var (
	signInLinkOpen  = "\x1b]8;id=" + signInLinkID + ";" + signInURL + "\x07"
	signInLinkClose = "\x1b]8;;\x07"
)

// osc8 is one OSC 8 sequence of a line: its parameters and URI, whether BEL
// ends it, and the bytes it spans.
type osc8 struct {
	params, uri string
	bel         bool
	start, end  int
}

// osc8s is every OSC 8 sequence in line, in order, and false when one of them
// is never ended — the escape left open to the end of the line.
func osc8s(line string) ([]osc8, bool) {
	var out []osc8
	for i := 0; ; {
		j := strings.Index(line[i:], "\x1b]8;")
		if j < 0 {
			return out, true
		}
		start := i + j
		body := line[start+len("\x1b]8;"):]
		end := strings.IndexAny(body, "\x07\x1b")
		if end < 0 {
			return out, false
		}
		params, uri, ok := strings.Cut(body[:end], ";")
		if !ok {
			return out, false
		}
		n := end + 1
		if body[end] == '\x1b' {
			if !strings.HasPrefix(body[end:], "\x1b\\") {
				return out, false
			}
			n = end + 2
		}
		seq := osc8{params: params, uri: uri, bel: body[end] == '\x07', start: start, end: start + len("\x1b]8;") + n}
		out = append(out, seq)
		i = seq.end
	}
}

// signInLinkedText checks one line — of a frame, of a box, or one box row
// (row) — for the address's link, and answers the text it links and whether
// it links any: no OSC 8 at all, or exactly one open — the attempt's id, the
// whole address, BEL — and one close after it. In a frame or a box the close
// comes before the row's padding and the box's border; a row (row) ends with
// it.
func signInLinkedText(t *testing.T, where, line string, row bool) (string, bool) {
	t.Helper()
	seqs, ok := osc8s(line)
	switch {
	case !ok:
		t.Fatalf("%s: an OSC 8 escape is never ended: %q", where, line)
	case len(seqs) == 0:
		return "", false
	case len(seqs) != 2:
		t.Fatalf("%s: %d OSC 8 escapes on one line; want one open and one close: %q", where, len(seqs), line)
	}
	open, end := seqs[0], seqs[1]
	if open.params != "id="+signInLinkID || open.uri != signInURL || !open.bel {
		t.Fatalf("%s: the link opens with %q, %d bytes of URI (BEL %v); want the attempt's id and the whole address, BEL-terminated",
			where, open.params, len(open.uri), open.bel)
	}
	if end.params != "" || end.uri != "" || !end.bel {
		t.Fatalf("%s: the link's second escape is not its close: %q", where, line[end.start:end.end])
	}
	text := ansi.Strip(line[open.end:end.start])
	after := ansi.Strip(line[end.end:])
	switch {
	case text == "" || strings.HasSuffix(text, " ") || strings.Contains(text, "│"):
		t.Fatalf("%s: the link holds %q, not one row of the address", where, text)
	case row && end.end != len(line):
		t.Fatalf("%s: the row goes on after its link closes: %q", where, line[end.end:])
	case !row && !strings.HasPrefix(strings.TrimLeft(after, " "), "│"):
		t.Fatalf("%s: the link closes after the box's padding or border: %q", where, after)
	}
	return text, true
}

// assertSignInLinks fails the test unless raw — a frame, or a box's rows
// (rows) — links the address as plan 034 Q9 says on every line
// (signInLinkedText), on exactly want lines, whose linked texts in order are
// the address whole, or its start ending in "…" where the box cuts it.
func assertSignInLinks(t *testing.T, where, raw string, want int, rows bool) {
	t.Helper()
	var texts []string
	for i, line := range strings.Split(raw, "\n") {
		if text, ok := signInLinkedText(t, fmt.Sprintf("%s, line %d", where, i), line, rows); ok {
			texts = append(texts, text)
		}
	}
	if len(texts) != want {
		t.Fatalf("%s: %d rows are links; want %d, the rows of the address", where, len(texts), want)
	}
	if want == 0 {
		return
	}
	joined := strings.Join(texts, "")
	if cut, ok := strings.CutSuffix(joined, "…"); ok {
		if !strings.HasPrefix(signInURL, cut) || len(cut) >= len(signInURL) {
			t.Fatalf("%s: the linked rows are not the address's start, cut", where)
		}
		return
	}
	if joined != signInURL {
		t.Fatalf("%s: the linked rows are not the address in order", where)
	}
}

// assertNoURIPayload fails the test unless plain, a stripped frame, holds
// none of a link's escape: no OSC 8 left as text, no link id, and the address
// nowhere whole — a row of it at most.
func assertNoURIPayload(t *testing.T, where, plain string) {
	t.Helper()
	for _, s := range []string{"]8;", signInLinkPrefix, signInURL, "\x07"} {
		if strings.Contains(plain, s) {
			t.Fatalf("%s: the stripped frame holds a link's payload (%q)", where, s)
		}
	}
}

// addressRows is how many of p's rows are the address's.
func addressRows(p signInPlan) int {
	n := 0
	for _, r := range p.rows {
		if r.kind == signInRowAddress {
			n++
		}
	}
	return n
}

// signInStepAt is the sign-in step over the stand-in make makes, begun on a
// model of cols x rows: /connect open, Enter on the ChatGPT plan, the begin's
// answer applied. The TUI's environment is the fixture's with env over it
// (Config.Getenv), and its mouse is off with noMouse (--no-mouse). It answers
// the begin's message too, whose events command a test may run
// (runSignInEvents).
func signInStepAt(t *testing.T, cols, rows int, env map[string]string, noMouse bool, make func() *fakeSignIn) (Model, signInBegunMsg) {
	t.Helper()
	standInSignIn(t, make)
	dir, getenv := signInFixture(t, false)
	m := connectModelOf(t, Config{Session: nativeStub(), NativeDir: dir, NoMouse: noMouse, Getenv: func(k string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return getenv(k)
	}})
	t.Cleanup(func() { _ = m.signIns.closeLog() })
	m, _ = typeCommand(t, m, "/connect")
	if p, ok := m.cdlg.provider(); !ok || !p.SignIn {
		t.Fatalf("step one does not open on the ChatGPT plan (on %q)", p.Name)
	}
	m, cmd := press(m, enter())
	begun, ok := runCmd(cmd).(signInBegunMsg)
	if !ok || begun.att == nil {
		t.Fatal("Enter on the ChatGPT plan did not begin a sign-in")
	}
	t.Cleanup(func() { begun.att.Close(chatgptauth.CloseDone) })
	tm, _ := m.Update(begun)
	tm, _ = tm.(Model).Update(tea.WindowSizeMsg{Width: cols, Height: rows})
	return tm.(Model), begun
}

// listeningStandIn and pasteOnlyStandIn make the stand-in attempt with a
// listener, and without one.
func listeningStandIn() *fakeSignIn { return newFakeSignIn(true, chatgptauth.Result{}) }
func pasteOnlyStandIn() *fakeSignIn { return newFakeSignIn(false, chatgptauth.Result{}) }

// resized is m at cols x rows.
func resized(m Model, cols, rows int) Model {
	tm, _ := m.Update(tea.WindowSizeMsg{Width: cols, Height: rows})
	return tm.(Model)
}

// withoutOSC8 is s with every OSC 8 escape taken out, and nothing else.
func withoutOSC8(s string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		seqs, _ := osc8s(line)
		at := 0
		for _, q := range seqs {
			b.WriteString(line[at:q.start])
			at = q.end
		}
		b.WriteString(line[at:])
	}
	return b.String()
}

// assertSignInFrame fails the test unless m's frame links the address on
// exactly the rows its box's plan draws as the address (signInPlan), is as
// wide on every line as the terminal, and its stripped text holds no link's
// payload; and — unless the box shows the copied line, whose text says
// whether there is a link to click — the links are all that tells it from the
// frame with the address drawn plain, byte for byte. It answers the plan.
func assertSignInFrame(t *testing.T, where string, m Model) signInPlan {
	t.Helper()
	view := m.View()
	if !m.cdlg.signIn.copied {
		unlinked := m
		unlinked.cdlg.signIn.link = ""
		if withoutOSC8(view) != unlinked.View() {
			t.Fatalf("%s: the linked frame is more than the plain frame and its links", where)
		}
	}
	for i, line := range strings.Split(view, "\n") {
		if w := lipgloss.Width(line); w != m.width {
			t.Fatalf("%s: line %d is %d cells wide, not the terminal's %d", where, i, w, m.width)
		}
	}
	assertNoURIPayload(t, where, ansi.Strip(view))
	r := m.lay.Dialog
	if r.Empty() {
		assertSignInLinks(t, where, view, 0, false)
		return signInPlan{}
	}
	p := m.signInPlan(r.W-dialogBorder, r.H-dialogBorder)
	assertSignInLinks(t, where, view, addressRows(p), false)
	return p
}

// TestSignInLinksAtEveryBudget (plan 034 A5): at the widths 60, 80, 100, 140
// and 200 — the box 56 to 104 columns wide — with a listener and without
// one, every frame from a terminal one row tall to one that holds the box
// whole links exactly the rows its box draws as the address, each balanced,
// the whole address under the attempt's id, closed before the padding and
// the border; every line keeps the terminal's width, and the stripped frame
// holds no link. And the box itself at every height budget, from the title
// alone to whole: each row of the address a link that ends with the row, and
// no other row one. The frames are not vacuous: each width drew the box whole
// and cut (the controls).
func TestSignInLinksAtEveryBudget(t *testing.T) {
	for _, make := range []func() *fakeSignIn{listeningStandIn, pasteOnlyStandIn} {
		m, _ := signInStepAt(t, 100, 30, nil, false, make)
		for _, cols := range []int{60, 80, 100, 140, 200} {
			inner := min(cols-dialogGutter, signInDialogWidth) - dialogBorder
			full := len(m.signInBody(inner, 1<<16))
			whole, cut := false, false
			for rows := 1; rows <= 60; rows++ {
				sized := resized(m, cols, rows)
				where := fmt.Sprintf("listening %v, %dx%d", sized.cdlg.signIn.listening, cols, rows)
				p := assertSignInFrame(t, where, sized)
				if r := sized.lay.Dialog; !r.Empty() {
					if r.W-dialogBorder != inner {
						t.Fatalf("%s: the box is %d wide inside; want %d (plan 034 Q11)", where, r.W-dialogBorder, inner)
					}
					whole = whole || r.H-dialogBorder == full
					cut = cut || (addressRows(p) > 0 && r.H-dialogBorder < full)
				}
			}
			if !whole || !cut {
				t.Fatalf("width %d: no frame drew the box whole (%v) or cut (%v)", cols, whole, cut)
			}
			for budget := 1; budget <= full; budget++ {
				where := fmt.Sprintf("listening %v, inner %d, budget %d", m.cdlg.signIn.listening, inner, budget)
				body := m.signInBody(inner, budget)
				p := m.signInPlan(inner, budget)
				if len(body) > budget || len(body) != len(p.rows) {
					t.Fatalf("%s: %d rows drawn from a plan of %d", where, len(body), len(p.rows))
				}
				assertSignInLinks(t, where, strings.Join(body, "\n"), addressRows(p), true)
				for i, row := range body {
					if strings.Contains(row, "\x1b]8;") != (p.rows[i].kind == signInRowAddress) {
						t.Fatalf("%s: row %d (kind %d) is a link: %v", where, i, p.rows[i].kind, strings.Contains(row, "\x1b]8;"))
					}
					if p.rows[i].kind == signInRowAddress && (!strings.HasPrefix(row, signInLinkOpen) || !strings.HasSuffix(row, signInLinkClose)) {
						t.Fatalf("%s: address row %d is not the link's open, the row, and its close: %q", where, i, row)
					}
					if w := lipgloss.Width(row); w > inner {
						t.Fatalf("%s: row %d is %d cells wide in a box %d wide", where, i, w, inner)
					}
				}
			}
		}
	}
}

// cutAtWidth is one row of ASCII text and escapes cut as a naive cut would
// cut it: everything up to the width, and nothing after — escapes included.
// It is TestSignInLinkSurvivesEveryCut's control: the checks must see the
// link it leaves open.
func cutAtWidth(row string, w int) string {
	var b strings.Builder
	cells := 0
	for i := 0; i < len(row); {
		if row[i] == '\x1b' {
			_, _, n, _ := ansi.DecodeSequence(row[i:], 0, nil)
			b.WriteString(row[i : i+n])
			i += n
			continue
		}
		if cells == w {
			break
		}
		b.WriteByte(row[i])
		cells++
		i++
	}
	return b.String()
}

// TestSignInLinkSurvivesEveryCut (plan 034 A5; §3.2 "an OSC 8 open must never
// survive without its close"): the box's rows spliced into a styled frame
// line — itself highlighted, as a selection highlights it — at every width
// that line may have, from one that ends before the box to one past it, the
// box cut off at the right edge by every amount (spliceRow); and a linked
// row cut by every cut the drawing makes — the clamp with "…" (clampWidth),
// the padding (padRow), a cut from the left (ansi.TruncateLeft) — at every
// width. Every result keeps each link whole: its open and its close, or
// neither; a spliced line keeps the line's width. The control is cutAtWidth,
// a cut that drops what is past it: the same checks catch the open it leaves.
func TestSignInLinkSurvivesEveryCut(t *testing.T) {
	m, _ := signInStepAt(t, 100, 30, nil, false, listeningStandIn)
	r := m.lay.Dialog
	box := strings.Split(m.dialogView(r), "\n")
	linked := 0
	for _, line := range box {
		if strings.Contains(line, signInLinkOpen) {
			linked++
		}
	}
	if linked == 0 {
		t.Fatal("the box has no linked row to cut")
	}
	balanced := func(where, s string, want int) {
		t.Helper()
		seqs, ok := osc8s(s)
		if !ok || len(seqs) != want {
			t.Fatalf("%s: %d OSC 8 escapes (all ended: %v); want %d", where, len(seqs), ok, want)
		}
		for i, q := range seqs {
			if open := q.uri != ""; open != (i%2 == 0) {
				t.Fatalf("%s: escape %d is not the link's open then close", where, i)
			}
		}
	}
	bg := selectionSeq(m.theme.SelectionBG)
	for full := 0; full <= r.X+r.W+3; full++ {
		base := bg + styleFG(m.theme.Err).Render(strings.Repeat("x", full)) + ansi.ResetStyle
		for i, line := range box {
			where := fmt.Sprintf("a line %d wide, box row %d", full, i)
			got := spliceRow(base, line, r.X, r.W)
			if w := ansi.StringWidth(got); w != full {
				t.Fatalf("%s: the spliced line is %d wide", where, w)
			}
			want := 0
			if strings.Contains(line, signInLinkOpen) && full > r.X {
				want = 2
			}
			balanced(where, got, want)
		}
	}
	row := linkRow(styleFG(m.theme.FG).Render(signInURL[:40]), signInURL, signInLinkID)
	// The control: a cut that drops what is past it leaves the link open.
	if seqs, ok := osc8s(cutAtWidth(row, 10)); !ok || len(seqs) != 1 {
		t.Fatalf("the control: a dropping cut left %d escapes (all ended: %v); the checks would not see it", len(seqs), ok)
	}
	for w := 0; w <= 42; w++ {
		balanced(fmt.Sprintf("clampWidth %d", w), clampWidth(row, w), map[bool]int{true: 0, false: 2}[w == 0])
		balanced(fmt.Sprintf("padRow %d", w), padRow(row, w), map[bool]int{true: 0, false: 2}[w == 0])
		balanced(fmt.Sprintf("TruncateLeft %d", w), ansi.TruncateLeft(row, w, ""), 2)
		balanced(fmt.Sprintf("Truncate %d", w), ansi.Truncate(row, w, ""), 2)
	}
}

// TestSignInLinksOverASelection (plan 034 A5): the box over a transcript
// whose every row is highlighted by a selection — its own escapes in each
// frame line the box is spliced into — still links exactly its address rows,
// each balanced, at the terminal's width. The control is the highlight
// itself, in the frame beside the box.
func TestSignInLinksOverASelection(t *testing.T) {
	m, _ := signInStepAt(t, 100, 30, nil, false, listeningStandIn)
	m = feed(t, m, agent.Event{Type: agent.EventText, Text: manyRows(60)})
	m = feed(t, m, agent.Event{Type: agent.EventDone, StopReason: "end_turn"})
	if m.dialog != dialogConnect || m.cdlg.step != connectSignIn {
		t.Fatal("the reply closed the sign-in step")
	}
	m.sel = selection{on: true, anchor: cellPos{line: 0, col: 0}, head: cellPos{line: 1 << 20, col: m.width - 1}}
	bg := selectionSeq(m.theme.SelectionBG)
	if bg == "" || !strings.Contains(m.View(), bg) {
		t.Fatal("the control: the frame has no selection highlight under the box")
	}
	assertSignInFrame(t, "a selection under the box", m)
}

// TestSignInLinksAcrossAResize (plan 034 A5): the step opened wide, then the
// terminal narrowed, shortened, widened and made tall again — each frame
// links exactly its box's address rows, whole and balanced, and keeps the
// terminal's width; the field keeps its width with the box (fitDialogFields).
func TestSignInLinksAcrossAResize(t *testing.T) {
	m, _ := signInStepAt(t, 140, 40, nil, false, listeningStandIn)
	for _, size := range [][2]int{{140, 40}, {60, 16}, {60, 30}, {200, 12}, {200, 50}, {80, 24}} {
		m = resized(m, size[0], size[1])
		where := fmt.Sprintf("resized to %dx%d", size[0], size[1])
		if p := assertSignInFrame(t, where, m); addressRows(p) == 0 {
			t.Fatalf("%s: the box draws no row of the address", where)
		}
		if m.cdlg.key.Width != dialogFieldWidth(m.lay.Dialog.W-dialogBorder) {
			t.Fatalf("%s: the field is %d wide in a box %d wide", where, m.cdlg.key.Width, m.lay.Dialog.W)
		}
	}
}

// TestSignInPlainRows (plan 034 A6): the address drawn with no link at all —
// on a terminal that would draw the escape as text or has no hyperlinks
// (TERM dumb, TERM linux, read through Config.Getenv), for an address a link
// may not point to (a fake issuer's, another host's, one with an escape in
// it: chatgptauth.LinkableAuthorizeURL), and for an attempt id that is not
// one — and the copied line then says nothing of a click. The controls are
// the fixture's environment and another terminal's TERM, which link it.
func TestSignInPlainRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		set   func(*fakeSignIn)
		plain bool
	}{
		{"control: the fixture's environment", nil, nil, false},
		{"control: another TERM", map[string]string{"TERM": "xterm-ghostty"}, nil, false},
		{"TERM=dumb", map[string]string{"TERM": "dumb"}, nil, true},
		{"TERM=linux", map[string]string{"TERM": "linux"}, nil, true},
		{"a fake issuer's address", nil, func(f *fakeSignIn) {
			f.url = "http://127.0.0.1:9" + strings.TrimPrefix(signInURL, "https://auth.openai.com")
		}, true},
		{"another host's address", nil, func(f *fakeSignIn) {
			f.url = strings.Replace(signInURL, "auth.openai.com", "auth.openai.com.evil.test", 1)
		}, true},
		{"an address with an escape in it", nil, func(f *fakeSignIn) { f.url = signInURL + "\x1b]8;;\x07" }, true},
		{"an id that is not an attempt's", nil, func(f *fakeSignIn) { f.id = "0A1B2C3D" }, true},
		{"an id with a separator in it", nil, func(f *fakeSignIn) { f.id = "0a1b:c3d" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := signInStepAt(t, 100, 30, tc.env, false, func() *fakeSignIn {
				f := listeningStandIn()
				if tc.set != nil {
					tc.set(f)
				}
				return f
			})
			m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlY})
			view := m.View()
			if !tc.plain {
				assertSignInFrame(t, tc.name, m)
				if !m.cdlg.signIn.copied || !strings.Contains(plainView(m), "Cmd/Ctrl+click it") {
					t.Fatalf("the control: a linked address's copied line does not say to click it:\n%s", plainView(m))
				}
				return
			}
			if strings.Contains(view, "\x1b]8;") {
				t.Fatalf("the address is a link: %q", view)
			}
			if !m.cdlg.signIn.copied || m.cdlg.signIn.copiedLine() != connectSignInCopiedLine || strings.Contains(plainView(m), "click it") {
				t.Fatalf("a plain address's copied line says to click it:\n%s", plainView(m))
			}
		})
	}
}

// TestSignInClickCopiesTheAddress (plan 034 A7, Q10): a click on each row of
// the address — in a tall box, where it is whole, and a short one, where it
// is cut with "…" — copies the address whole, the attempt's own bytes,
// through the TUI's copy with the status row's note, tells the attempt
// (address_copied), and shows the box's copied line, even in the short box.
// A click on any other row of the step copies nothing and shows nothing (the
// controls), and with --no-mouse a click does nothing at all.
func TestSignInClickCopiesTheAddress(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {100, 13}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			rec := captureCopies(t)
			m, _ := signInStepAt(t, size[0], size[1], nil, false, listeningStandIn)
			f := m.cdlg.signIn.att.(*fakeSignIn)
			r := m.lay.Dialog
			p := m.signInPlan(r.W-dialogBorder, r.H-dialogBorder)
			isCut := strings.Contains(plainView(m), "…")
			if tall := size[1] == 30; tall == isCut || addressRows(p) == 0 {
				t.Fatalf("the box is not %s (cut %v, %d address rows)", map[bool]string{true: "whole", false: "cut"}[tall], isCut, addressRows(p))
			}
			copies := 0
			for i, row := range p.rows {
				// Each click on the step as it was drawn: the model is a
				// value, so m is that step every time.
				click := tea.MouseMsg{X: r.X + 1 + i%7, Y: r.Y + 1 + i, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
				tm, cmd := m.Update(click)
				got := tm.(Model)
				if row.kind != signInRowAddress {
					if cmd != nil || got.cdlg.signIn.copied || len(rec.copies()) != copies {
						t.Fatalf("a click on row %d (kind %d) copied", i, row.kind)
					}
					continue
				}
				copies++
				done, ok := runCmd(cmd).(clipboardDoneMsg)
				if !ok || done.note != connectSignInCopied {
					t.Fatalf("a click on address row %d did not copy with the status row's note", i)
				}
				if c := rec.copies(); len(c) != copies || c[len(c)-1] != f.URL() {
					t.Fatalf("a click on address row %d copied %d bytes, not the whole address", i, len(c[len(c)-1]))
				}
				if !got.cdlg.signIn.copied || !strings.Contains(plainView(got), connectSignInCopiedLine) {
					t.Fatalf("a click on address row %d shows no copied line in the box:\n%s", i, plainView(got))
				}
				assertSignInFrame(t, fmt.Sprintf("after a click on address row %d", i), got)
			}
			if copies != addressRows(p) || len(f.reports()) != copies {
				t.Fatalf("%d clicks on %d address rows told the attempt %d times", copies, addressRows(p), len(f.reports()))
			}
			for _, k := range f.reports() {
				if k != chatgptauth.EventAddressCopied {
					t.Fatalf("a click reported %s; want address_copied", k)
				}
			}
		})
	}
	t.Run("--no-mouse", func(t *testing.T) {
		rec := captureCopies(t)
		m, _ := signInStepAt(t, 100, 30, nil, true, listeningStandIn)
		r := m.lay.Dialog
		p := m.signInPlan(r.W-dialogBorder, r.H-dialogBorder)
		i := slices.IndexFunc(p.rows, func(r signInRow) bool { return r.kind == signInRowAddress })
		tm, cmd := m.Update(tea.MouseMsg{X: r.X + 2, Y: r.Y + 1 + i, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
		if cmd != nil || tm.(Model).cdlg.signIn.copied || len(rec.copies()) != 0 {
			t.Fatal("with --no-mouse a click copied")
		}
		view := plainView(m)
		if !strings.Contains(view, connectSignInWaitHintKeys) || strings.Contains(view, "click") {
			t.Fatalf("with --no-mouse the footer offers a click:\n%s", view)
		}
		m, cmd = press(m, tea.KeyMsg{Type: tea.KeyCtrlY})
		if _, ok := runCmd(cmd).(clipboardDoneMsg); !ok || len(rec.copies()) != 1 || rec.copies()[0] != signInURL {
			t.Fatal("with --no-mouse Ctrl+Y did not copy the whole address")
		}
		if !strings.Contains(plainView(m), connectSignInCopiedLine) {
			t.Fatalf("with --no-mouse Ctrl+Y shows no copied line:\n%s", plainView(m))
		}
	})
}

// TestSignInCopiedLineLasts (plan 034 Q10): Ctrl+Y shows the copied line in
// the box, as a click does; it goes at the next key — any key but Ctrl+Y —
// or when its own copy's tick comes (signInCopiedLinger), and not at an
// earlier copy's tick, another run's or another dialog's (the controls),
// which would end a later copy's line early.
func TestSignInCopiedLineLasts(t *testing.T) {
	prev := signInCopiedLinger
	signInCopiedLinger = time.Millisecond
	t.Cleanup(func() { signInCopiedLinger = prev })
	captureCopies(t)
	m, _ := signInStepAt(t, 100, 30, nil, false, listeningStandIn)
	if m.cdlg.signIn.copied || strings.Contains(plainView(m), connectSignInCopiedLine) {
		t.Fatal("the control: the copied line shows before a copy")
	}
	ticks := func(cmd tea.Cmd) []signInCopiedMsg {
		t.Helper()
		var out []signInCopiedMsg
		for _, msg := range runAll(t, cmd) {
			if tick, ok := msg.(signInCopiedMsg); ok {
				out = append(out, tick)
			}
		}
		if len(out) != 1 {
			t.Fatalf("a copy made %d ticks; want one", len(out))
		}
		return out
	}
	m, first := press(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	firstTick := ticks(first)[0]
	if !m.cdlg.signIn.copied || !strings.Contains(plainView(m), connectSignInCopiedLine) {
		t.Fatalf("Ctrl+Y shows no copied line:\n%s", plainView(m))
	}
	m, second := press(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	secondTick := ticks(second)[0]
	for _, stale := range []signInCopiedMsg{
		firstTick,
		{gen: secondTick.gen, run: secondTick.run + 1, n: secondTick.n},
		{gen: secondTick.gen + 1, run: secondTick.run, n: secondTick.n},
	} {
		tm, _ := m.Update(stale)
		if !tm.(Model).cdlg.signIn.copied {
			t.Fatalf("a stale tick %+v ended the copied line", stale)
		}
	}
	tm, _ := m.Update(secondTick)
	if tm.(Model).cdlg.signIn.copied || strings.Contains(plainView(tm.(Model)), connectSignInCopiedLine) {
		t.Fatal("the copy's own tick did not end the copied line")
	}
	// The next key ends it as well.
	m = pressKey(t, m, tea.KeyRight)
	if m.cdlg.signIn.copied || strings.Contains(plainView(m), connectSignInCopiedLine) {
		t.Fatal("the next key did not end the copied line")
	}
}

// TestSignInHints (plan 034 A8, Q12): the hint under the address. On this
// machine with a listener it is as it was. Over SSH — any one of
// SSH_CONNECTION, SSH_CLIENT and SSH_TTY, read through Config.Getenv — with a
// listener it leads with the paste path and offers the forward of the
// listener's own port (a re-login's free port here, not 1455). With no
// listener it says the browser's page will not load and what to paste, with
// the SSH lead over SSH, and offers no forward: nothing of craze's listens on
// the port. The drawn rows are the hint, word-wrapped.
func TestSignInHints(t *testing.T) {
	const port = "50123"
	redirect := "http://127.0.0.1:" + port + "/auth/callback"
	const lead = "craze runs on another machine (SSH). Open the address on your computer and approve. "
	forward := "When the browser can't connect to 127.0.0.1, copy that page's address and paste it here — or forward the port first: ssh -L " +
		port + ":127.0.0.1:" + port + "."
	paste := "The browser's page will not load after you approve: copy its whole address (it starts with " + redirect + ") and paste it here:"
	sshPaste := "The browser's page will not load: copy its whole address (it starts with " + redirect + ") and paste it here:"
	for _, tc := range []struct {
		name      string
		env       map[string]string
		listening bool
		want      string
	}{
		{"on this machine, listening", nil, true, connectSignInWaiting},
		{"SSH_CONNECTION, listening", map[string]string{"SSH_CONNECTION": "10.0.0.1 50000 10.0.0.2 22"}, true, lead + forward},
		{"SSH_CLIENT, listening", map[string]string{"SSH_CLIENT": "10.0.0.1 50000 22"}, true, lead + forward},
		{"SSH_TTY, listening", map[string]string{"SSH_TTY": "/dev/pts/3"}, true, lead + forward},
		{"SSH, paste-only", map[string]string{"SSH_CONNECTION": "10.0.0.1 50000 10.0.0.2 22"}, false, lead + sshPaste},
		{"on this machine, paste-only", nil, false, paste},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := signInStepAt(t, 140, 40, tc.env, false, func() *fakeSignIn {
				f := newFakeSignIn(tc.listening, chatgptauth.Result{})
				f.redirect = redirect
				return f
			})
			if got := m.cdlg.signIn.hint(); got != tc.want {
				t.Fatalf("the hint is\n%q\nwant\n%q", got, tc.want)
			}
			r := m.lay.Dialog
			var rows []string
			for _, row := range m.signInPlan(r.W-dialogBorder, r.H-dialogBorder).rows {
				if row.kind == signInRowHint {
					rows = append(rows, row.text)
				}
			}
			if strings.Join(rows, " ") != tc.want {
				t.Fatalf("the box draws the hint as %q", rows)
			}
			if strings.Contains(tc.want, "ssh -L") != (tc.listening && tc.env != nil) {
				t.Fatal("the forward is offered where nothing listens, or not over SSH with a listener")
			}
		})
	}
}

// runSignInEvents runs the delivery of the run's shown events (cmd, a begin's
// signInBegunMsg.events) on a goroutine of its own, as bubbletea would, and
// answers its message's channel. The test's end ends m's run and joins it.
func runSignInEvents(t *testing.T, m Model, cmd tea.Cmd) <-chan tea.Msg {
	t.Helper()
	run := m.cdlg.signIn
	out := make(chan tea.Msg, 1)
	joined := make(chan struct{})
	t.Cleanup(func() {
		run.end(chatgptauth.CloseDialog)
		select {
		case <-joined:
		case <-time.After(10 * time.Second):
			t.Error("the delivery of the sign-in's events outlived its test")
		}
	})
	go func() {
		defer close(joined)
		out <- cmd()
	}()
	return out
}

// TestSignInShowsTheBrowsersReturnFromAnotherAttempt (plan 034 Q8, A13): the
// listener's refusal of a redirect with a code and another attempt's state,
// reported on the listener's goroutine, reaches the step through the run's
// delivery and is a line in the box — "Use the address shown now", as the
// paste's refusal of one says — and the delivery goes on for the next. An
// event the step does not show is never delivered (the control: nothing
// arrives); the same refusal delivered to a step it is not for — another run
// after Esc and back, another opening of the box — is dropped, with the
// delivery after it; and Esc ends the delivery with the run.
func TestSignInShowsTheBrowsersReturnFromAnotherAttempt(t *testing.T) {
	m, begun := signInStepAt(t, 100, 30, nil, false, listeningStandIn)
	f := m.cdlg.signIn.att.(*fakeSignIn)
	events := runSignInEvents(t, m, begun.events)
	for _, ev := range []chatgptauth.Event{
		{Kind: chatgptauth.EventListenerRefused, Refusal: chatgptauth.RefusalNotOurs, Status: 400},
		{Kind: chatgptauth.EventListenerRefused, Refusal: chatgptauth.RefusalOver, Status: 410},
		{Kind: chatgptauth.EventPasteRefused, Refusal: chatgptauth.RefusalMismatch, Part: chatgptauth.PartState},
		{Kind: chatgptauth.EventAddressCopied},
	} {
		f.Observer()(ev)
	}
	select {
	case msg := <-events:
		t.Fatalf("the control: an event the step does not show was delivered: %T", msg)
	case <-time.After(100 * time.Millisecond):
	}
	if strings.Contains(plainView(m), connectSignInOtherAttempt) {
		t.Fatal("the control: the line shows before the listener's refusal")
	}
	f.Observer()(chatgptauth.Event{Kind: chatgptauth.EventListenerRefused, Refusal: chatgptauth.RefusalOtherAttempt, Status: 400})
	msg, ok := awaitMsg(t, events).(signInShownMsg)
	if !ok || msg.gen != m.cdlg.gen || msg.run != m.cdlg.signIn.run {
		t.Fatal("the listener's refusal was not delivered stamped with the step's dialog and run")
	}
	tm, next := m.Update(msg)
	shown := tm.(Model)
	if !strings.Contains(plainView(shown), connectSignInOtherAttempt) || next == nil {
		t.Fatalf("the refusal is not a line in the box, or the delivery stopped:\n%s", plainView(shown))
	}
	assertSignInFrame(t, "with the other attempt's line", shown)

	// Stale: another run of the step (Esc, and the plan picked again), and
	// another opening of the box.
	back := pressKey(t, m, tea.KeyEsc)
	back, _ = beginStep(t, back)
	reopened := back.closeDialog(true)
	reopened, _ = typeCommand(t, reopened, "/connect")
	reopened, _ = beginStep(t, reopened)
	for name, other := range map[string]Model{"another run": back, "another opening": reopened} {
		tm, cmd := other.Update(msg)
		if tm.(Model).cdlg.signIn.other || cmd != nil || strings.Contains(plainView(tm.(Model)), connectSignInOtherAttempt) {
			t.Fatalf("%s took another step's refusal", name)
		}
	}

	// The delivery after it ends with the run: Esc ended it above, so the
	// next delivery answers nothing.
	nextMsgs := make(chan tea.Msg, 1)
	go func() { nextMsgs <- next() }()
	if got := awaitMsg(t, nextMsgs); got != nil {
		t.Fatalf("the delivery went on after its run ended: %T", got)
	}
}

// TestSignInSaysWhyCrazeIsNotListening (plan 034 Q8): an attempt that could
// not listen — 127.0.0.1:1455 busy, or no listener to be had — says why, from
// its begin, in craze auth's words, between the address and the hint, from
// the frame the step adopts it in. One that listens — on 1455, or on another
// port because 1455 was busy (a re-login's fallback) — and one paste-only by
// request say nothing (the controls).
func TestSignInSaysWhyCrazeIsNotListening(t *testing.T) {
	begin := func(mode chatgptauth.Mode, why chatgptauth.Reason, port int) chatgptauth.Event {
		return chatgptauth.Event{Kind: chatgptauth.EventBegin, Attempt: signInAttemptID, Mode: mode, Reason: why, Port: port}
	}
	for _, tc := range []struct {
		name  string
		begin chatgptauth.Event
		want  string
	}{
		{"1455 busy", begin(chatgptauth.ModePasteOnly, chatgptauth.ReasonPortBusy, 1455),
			"craze is not listening for the browser: another program is using 127.0.0.1:1455."},
		{"no listener to be had", begin(chatgptauth.ModePasteOnly, chatgptauth.ReasonListenFailed, 1455),
			"craze is not listening for the browser: it could not listen on 127.0.0.1:1455."},
		{"control: listening", begin(chatgptauth.ModeListening, "", 1455), ""},
		{"control: listening elsewhere, 1455 busy", begin(chatgptauth.ModeListening, chatgptauth.ReasonPortBusy, 50123), ""},
		{"control: paste-only by request", begin(chatgptauth.ModePasteOnly, chatgptauth.ReasonRequested, 1455), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := signInStepAt(t, 100, 30, nil, false, func() *fakeSignIn {
				f := newFakeSignIn(tc.begin.Mode == chatgptauth.ModeListening, chatgptauth.Result{})
				f.emit = []chatgptauth.Event{tc.begin}
				return f
			})
			if got := m.cdlg.signIn.why; got != tc.want {
				t.Fatalf("why = %q, want %q", got, tc.want)
			}
			r := m.lay.Dialog
			p := m.signInPlan(r.W-dialogBorder, r.H-dialogBorder)
			at := slices.IndexFunc(p.rows, func(r signInRow) bool { return r.kind == signInRowWhy })
			if tc.want == "" {
				if at >= 0 || strings.Contains(plainView(m), "craze is not listening") {
					t.Fatal("the control: a line says why craze is not listening")
				}
				return
			}
			if at < 1 || p.rows[at-1].kind != signInRowAddress || p.rows[at+1].kind != signInRowHint || !strings.Contains(plainView(m), tc.want) {
				t.Fatalf("the reason is not a line between the address and the hint:\n%s", plainView(m))
			}
		})
	}
}

// ---------------------------------------------------------------- frames

// runSignInFrameRaw runs keys over signInConfig at cols x rows, the TUI's
// environment with env over it, in every transport and gate mode, each run's
// frame and error screened for the canary (connectScreen), and answers the
// frame they agree on, plain and raw.
func runSignInFrameRaw(t *testing.T, cols, rows int, env map[string]string, make func() *fakeSignIn, keys string) (string, string) {
	t.Helper()
	isolateSkillsHome(t)
	standInSignIn(t, make)
	plain, raw, err := runFrameModesScreened(t, func() Config {
		cfg := signInConfig(t, false)
		getenv := cfg.Getenv
		cfg.Getenv = func(k string) string {
			if v, ok := env[k]; ok {
				return v
			}
			return getenv(k)
		}
		return cfg
	}, cols, rows, keys, FrameOpts{Timeout: 20 * time.Second}, connectScreen(t))
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	return plain, raw
}

// boxText is what the box in plain, a stripped frame, holds on each of its
// rows: the text between its borders, its padding trimmed.
func boxText(plain string) []string {
	var out []string
	for _, line := range strings.Split(plain, "\n") {
		if _, rest, ok := strings.Cut(line, "│"); ok {
			row, _, _ := strings.Cut(rest, "│")
			out = append(out, strings.TrimRight(row, " "))
		}
	}
	return out
}

// shownAddressRows is how many rows of the box in plain show a piece of the
// address: the rows a frame must link.
func shownAddressRows(plain string) int {
	n := 0
	for _, row := range boxText(plain) {
		piece := strings.TrimSuffix(row, "…")
		if piece != "" && strings.Contains(signInURL, piece) && !strings.HasPrefix(row, "❯") {
			n++
		}
	}
	return n
}

// TestFrameNativeConnectSignInLinks (plan 034 A5), through the real program in
// every transport and gate mode, each run screened: the raw frame at the
// widths 60, 80, 100, 140 and 200 — tall enough for the box whole, and short
// enough to cut it — and after a resize, links every row of the address it
// shows and no other, each the whole address under the attempt's fixed id,
// balanced and closed before the padding and the border; every line is the
// terminal's width; the stripped frame holds no link.
func TestFrameNativeConnectSignInLinks(t *testing.T) {
	// A short frame is the step opened at 100x30, where step one's list
	// shows the plan's row, and the terminal shortened.
	for _, tc := range []struct{ cols, rows, toCols, toRows int }{
		{60, 30, 0, 0}, {80, 24, 0, 0}, {100, 30, 0, 0}, {140, 40, 0, 0}, {200, 50, 0, 0},
		{100, 30, 60, 12}, {100, 30, 100, 13}, {100, 30, 200, 12}, {140, 40, 60, 18},
	} {
		keys, cols := signInOpenKeys, tc.cols
		if tc.toCols != 0 {
			keys += fmt.Sprintf("<resize:%d,%d>", tc.toCols, tc.toRows)
			cols = tc.toCols
		}
		t.Run(fmt.Sprintf("%dx%d to %dx%d", tc.cols, tc.rows, tc.toCols, tc.toRows), func(t *testing.T) {
			plain, raw := runSignInFrameRaw(t, tc.cols, tc.rows, nil, listeningStandIn, keys)
			want := shownAddressRows(plain)
			if want == 0 {
				t.Fatalf("the frame shows no row of the address:\n%s", plain)
			}
			assertSignInLinks(t, "the raw frame", raw, want, false)
			assertNoURIPayload(t, "the stripped frame", plain)
			if ansi.Strip(raw) != plain {
				t.Fatal("the stripped raw frame is not the plain frame")
			}
			for i, line := range strings.Split(raw, "\n") {
				if w := lipgloss.Width(line); w != cols {
					t.Fatalf("line %d is %d cells wide, not %d", i, w, cols)
				}
			}
		})
	}
}

// TestFrameNativeConnectSignInPlainOnDumbTerminal (plan 034 A6): with TERM
// dumb in the TUI's environment the raw frame has no hyperlink at all, and
// says what the linked frame says — the control, the same step with the
// fixture's environment, links it.
func TestFrameNativeConnectSignInPlainOnDumbTerminal(t *testing.T) {
	var linkedPlain string
	t.Run("control", func(t *testing.T) {
		var raw string
		linkedPlain, raw = runSignInFrameRaw(t, 100, 30, nil, listeningStandIn, signInOpenKeys)
		if !strings.Contains(raw, signInLinkOpen) {
			t.Fatal("the control: the fixture's environment draws no link")
		}
	})
	t.Run("TERM=dumb", func(t *testing.T) {
		plain, raw := runSignInFrameRaw(t, 100, 30, map[string]string{"TERM": "dumb"}, listeningStandIn, signInOpenKeys)
		if strings.Contains(raw, "\x1b]8;") {
			t.Fatal("TERM=dumb draws a hyperlink")
		}
		if linkedPlain == "" || plain != linkedPlain {
			t.Fatalf("TERM=dumb's frame says something else:\n%s", plain)
		}
	})
}

// TestFrameNativeConnectSignInOverSSH (plan 034 A8): with SSH_CONNECTION in
// the TUI's environment the step's hint leads with the paste path and offers
// the forward of its listener's port.
func TestFrameNativeConnectSignInOverSSH(t *testing.T) {
	plain, _ := runSignInFrameRaw(t, 100, 30, map[string]string{"SSH_CONNECTION": "10.0.0.1 50000 10.0.0.2 22"}, listeningStandIn, signInOpenKeys)
	text := strings.Join(boxText(plain), " ")
	for _, want := range []string{"craze runs on another machine (SSH).", "can't connect to 127.0.0.1", "ssh -L 1455:127.0.0.1:1455."} {
		if !strings.Contains(text, want) {
			t.Fatalf("the hint over SSH does not say %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "Waiting for the browser.") {
		t.Fatalf("over SSH the hint is the local one:\n%s", plain)
	}
}

// TestFrameNativeConnectSignInClickCopies (plan 034 A7): a click on a row of
// the address, through the real program in every transport and gate mode,
// shows the copied line in the box and the status row's note. The click's
// cell is read off the frame before it.
func TestFrameNativeConnectSignInClickCopies(t *testing.T) {
	x, y := -1, -1
	t.Run("the frame before", func(t *testing.T) {
		before, _ := runSignInFrameRaw(t, 100, 30, nil, listeningStandIn, signInOpenKeys)
		for i, line := range strings.Split(before, "\n") {
			if at := strings.Index(line, "│"+signInURL[:30]); at >= 0 {
				x, y = ansi.StringWidth(line[:at])+5, i
				break
			}
		}
		if y < 0 {
			t.Fatalf("no row of the address in the frame:\n%s", before)
		}
	})
	t.Run("the click", func(t *testing.T) {
		if y < 0 {
			t.Fatal("no cell to click")
		}
		plain, raw := runSignInFrameRaw(t, 100, 30, nil, listeningStandIn,
			signInOpenKeys+fmt.Sprintf("<click:%d,%d><wait:text:Copied the address.><wait:copied>", x, y))
		text := strings.Join(boxText(plain), " ")
		if !strings.Contains(text, connectSignInCopiedLinkLine) || !strings.Contains(plain, connectSignInCopied) {
			t.Fatalf("the click shows no copied line, or no note:\n%s", plain)
		}
		assertSignInLinks(t, "the raw frame after the click", raw, shownAddressRows(plain), false)
	})
}

// TestFrameNativeConnectSignInListenerNews (plan 034 Q8): an attempt whose
// begin says 1455 is busy and whose listener refused a different attempt's
// redirect, as the real program draws it in every transport and gate mode:
// the reason between the address and the hint, the other attempt's line
// under the field.
func TestFrameNativeConnectSignInListenerNews(t *testing.T) {
	plain, _ := runSignInFrameRaw(t, 100, 30, nil, func() *fakeSignIn {
		f := pasteOnlyStandIn()
		f.emit = []chatgptauth.Event{
			{Kind: chatgptauth.EventBegin, Attempt: signInAttemptID, Mode: chatgptauth.ModePasteOnly, Reason: chatgptauth.ReasonPortBusy, Port: 1455},
			{Kind: chatgptauth.EventListenerRefused, Attempt: signInAttemptID, Refusal: chatgptauth.RefusalOtherAttempt, Status: 400},
		}
		return f
	}, signInOpenKeys+"<wait:text:different sign-in attempt>")
	text := strings.Join(boxText(plain), " ")
	for _, want := range []string{"craze is not listening for the browser: another program is using 127.0.0.1:1455.", connectSignInOtherAttempt} {
		if !strings.Contains(text, want) {
			t.Fatalf("the box does not say %q:\n%s", want, plain)
		}
	}
}

// TestSignInRefusesToCopyAnAddressTooLong (plan 034 review r4 #5a): an address
// longer than a copy takes whole (clipboardMax) — one a registration file's
// oversized email could make before chatgptauth bounded the login hint — is
// not copied at all by Ctrl+Y or a click: nothing reaches a clipboard, the
// attempt is told of no copy, and the box says the address was too long to
// copy instead of "Copied". The control is the address as it is, copied
// whole and said so.
func TestSignInRefusesToCopyAnAddressTooLong(t *testing.T) {
	prev := signInCopiedLinger
	signInCopiedLinger = time.Millisecond
	t.Cleanup(func() { signInCopiedLinger = prev })
	for _, long := range []bool{true, false} {
		t.Run(map[bool]string{true: "too long", false: "control"}[long], func(t *testing.T) {
			rec := captureCopies(t)
			addr := signInURL
			if long {
				addr += "&login_hint=" + strings.Repeat("a", clipboardMax)
			}
			m, _ := signInStepAt(t, 100, 30, nil, false, func() *fakeSignIn {
				f := listeningStandIn()
				f.url = addr
				return f
			})
			f := m.cdlg.signIn.att.(*fakeSignIn)
			r := m.lay.Dialog
			p := m.signInPlan(r.W-dialogBorder, r.H-dialogBorder)
			i := slices.IndexFunc(p.rows, func(r signInRow) bool { return r.kind == signInRowAddress })
			for _, how := range []tea.Msg{
				tea.KeyMsg{Type: tea.KeyCtrlY},
				tea.MouseMsg{X: r.X + 2, Y: r.Y + 1 + i, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress},
			} {
				tm, cmd := m.Update(how)
				got := tm.(Model)
				copied := slices.ContainsFunc(runAll(t, cmd), func(msg tea.Msg) bool {
					_, ok := msg.(clipboardDoneMsg)
					return ok
				})
				view := plainView(got)
				if long {
					if copied || len(rec.copies()) != 0 || len(f.reports()) != 0 {
						t.Fatalf("%T copied an address too long to copy whole (%d copies, %v reported)", how, len(rec.copies()), f.reports())
					}
					if !strings.Contains(view, connectSignInUncopiedLine) || strings.Contains(view, connectSignInCopiedLine) {
						t.Fatalf("%T: the box does not say the address was too long to copy:\n%s", how, view)
					}
					continue
				}
				if c := rec.copies(); !copied || len(c) == 0 || c[len(c)-1] != addr || !strings.Contains(view, connectSignInCopiedLine) {
					t.Fatalf("the control: %T did not copy the address whole and say so", how)
				}
			}
		})
	}
}

// TestSignInCopiedLineEndsAtAnInterceptedKey (plan 034 review r4 #5b): the
// copied line goes at the next key even when a layer above the step takes
// it: Ctrl+C while work runs stops the work and never reaches the step, whose
// box stays open — and its copied line is gone all the same. The control is
// the copied line before the key.
func TestSignInCopiedLineEndsAtAnInterceptedKey(t *testing.T) {
	captureCopies(t)
	m, _ := signInStepAt(t, 100, 30, nil, false, listeningStandIn)
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	if !m.cdlg.signIn.copied || !strings.Contains(plainView(m), connectSignInCopiedLine) {
		t.Fatal("the control: Ctrl+Y shows no copied line")
	}
	m.status = statusWorking
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = tm.(Model)
	if m.dialog != dialogConnect || m.cdlg.step != connectSignIn {
		t.Fatal("Ctrl+C with work running closed the sign-in step")
	}
	if m.cdlg.signIn.copied || strings.Contains(plainView(m), connectSignInCopiedLine) {
		t.Fatalf("Ctrl+C, taken before the step, left the copied line:\n%s", plainView(m))
	}
}
