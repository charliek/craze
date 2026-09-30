package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
)

// The composer's `@` popup (plan 030 §3.16, composer_at.go): its frames with
// a fixed listing (no rg in a golden), the exact text a pick writes, when it
// opens and when it does not, the keys it takes from the composer and the
// ones it leaves, and the searches a closing or a switch leaves behind.

// atComposerPaths is the workspace every case here lists: a repository's
// shape, with a name that needs quoting and one that is not ASCII.
var atComposerPaths = []string{
	"README.md", "go.mod", "Makefile",
	"cmd/craze/main.go",
	"docs/reference/tui.md", "docs/guide/getting started.md",
	"internal/tui/app.go", "internal/tui/composer.go", "internal/tui/composer_at.go", "internal/tui/composer_test.go",
	"internal/tui/complete.go", "internal/tui/complete_test.go", "internal/tui/colors.go", "internal/tui/at_files.go",
	"internal/config/config.go",
	"internal/cli/serve.go", "internal/cli/attach.go",
	"internal/engine/engine.go",
	"notes/café.md",
	".github/workflows/ci.yml",
}

// atFixedListing is a search that lists paths wherever it is asked: no tool, no
// disk, the same answer every time.
func atFixedListing(paths ...string) func(context.Context, string) completeLoaded {
	return func(context.Context, string) completeLoaded {
		return completeLoaded{Data: newAtFileIndex(slices.Clone(paths), "")}
	}
}

// atHeld is a search that waits: it says it started (on started, with the
// root it was asked for), then answers paths once released, or its
// context's error once that ends — and says which on done.
type atHeld struct {
	paths   []string
	started chan string
	release chan struct{}
	done    chan error
}

func newAtHeld(paths ...string) *atHeld {
	return &atHeld{paths: paths, started: make(chan string, 4), release: make(chan struct{}), done: make(chan error, 4)}
}

func (h *atHeld) search(ctx context.Context, root string) completeLoaded {
	h.started <- root
	select {
	case <-h.release:
		h.done <- nil
		return completeLoaded{Data: newAtFileIndex(slices.Clone(h.paths), "")}
	case <-ctx.Done():
		h.done <- ctx.Err()
		return completeLoaded{Err: ctx.Err()}
	}
}

// awaitStarted and awaitDone are the held search's steps, each bounded on
// its own.
func (h *atHeld) awaitStarted(t *testing.T) string {
	t.Helper()
	select {
	case root := <-h.started:
		return root
	case <-time.After(completeStep):
		t.Fatalf("the search did not start within %v", completeStep)
		return ""
	}
}

func (h *atHeld) awaitDone(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.done:
		return err
	case <-time.After(completeStep):
		t.Fatalf("the search did not end within %v", completeStep)
		return nil
	}
}

// atModel is an in-process session (no session list: the opt-out's shape) at
// cols×rows in a workspace named ws, started, its elapsed counters frozen,
// its composer's `@` popup searching with search.
func atModel(t *testing.T, cols, rows int, search func(context.Context, string) completeLoaded) (Model, *Stub) {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	m := startStub(t, stub, frameWorkspace(t), cols, rows)
	m.frozen = true
	m.composerAt.setSource(atFileSource{search: search})
	return m, stub
}

// atLoadIn is the popup's search among cmd — batches walked by name, nothing
// else in them run — or nil when there is none.
func atLoadIn(cmd tea.Cmd) tea.Cmd {
	switch name := cmdFuncName(cmd); {
	case cmd == nil:
	case strings.HasPrefix(name, teaPkg+"compactCmds"), strings.HasPrefix(name, teaPkg+"Batch"):
		if b, ok := cmd().(tea.BatchMsg); ok {
			for _, c := range b {
				if l := atLoadIn(c); l != nil {
					return l
				}
			}
		}
	case strings.Contains(name, "completePopup).startLoad"):
		return cmd
	}
	return nil
}

// typeAt types text into the composer a key at a time, through Update, and
// answers the search a key's Update started — the popup's opening's, the
// Update wrapper's (finish) — if one did.
func typeAt(t *testing.T, m Model, text string) (Model, tea.Cmd) {
	t.Helper()
	var load tea.Cmd
	for _, r := range text {
		var cmd tea.Cmd
		m, cmd = press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		if l := atLoadIn(cmd); l != nil {
			load = l
		}
	}
	return m, load
}

// atSearched runs load — a search the popup started — within one step, and
// delivers its result.
func atSearched(t *testing.T, m Model, load tea.Cmd) Model {
	t.Helper()
	if load == nil {
		t.Fatalf("no search started:\n%s", plainView(m))
	}
	tm, _ := m.Update(awaitLoaded(t, runLoad(t, load)))
	return tm.(Model)
}

// atOpen types text and delivers the search it started: the popup up over
// the listing.
func atOpen(t *testing.T, m Model, text string) Model {
	t.Helper()
	m, load := typeAt(t, m, text)
	return atSearched(t, m, load)
}

// atKey presses k and answers the search it started, if one.
func atKey(t *testing.T, m Model, k tea.KeyMsg) (Model, tea.Cmd) {
	t.Helper()
	m, cmd := press(m, k)
	return m, atLoadIn(cmd)
}

// draftOf is the composer's draft and its cursor as a byte offset.
func draftOf(m Model) (string, int) { return m.input.Value(), m.composerCursorOffset() }

// atNames is the names the popup offers, in order.
func atNames(m Model) []string {
	var out []string
	for _, it := range m.composerAt.ans.Items {
		out = append(out, it.Name)
	}
	return out
}

func keyType(k tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: k} }

// TestFrameGoldenComposerAt (§3.17): the composer's `@` popup at 100×30 and
// 80×24 — a token typed after a word, what matches it ranked under the title
// rule (the directory and the files whose names start with it first, the
// shortest first), the rest counted; and a directory descended into with
// tab, the popup open on what is inside it — in process, over a fixed
// listing.
func TestFrameGoldenComposerAt(t *testing.T) {
	for _, size := range []struct{ cols, rows int }{{100, 30}, {80, 24}} {
		suffix := fmt.Sprintf("-%dx%d", size.cols, size.rows)
		m, _ := atModel(t, size.cols, size.rows, atFixedListing(atComposerPaths...))

		files := atOpen(t, m, "explain @co")
		assertFrameGolden(t, "composer-at-files"+suffix, size.cols, size.rows, plainView(files),
			[]string{"files in ws", "❯ internal/config/", "internal/tui/complete.go", "internal/tui/composer.go", "↓ 3 more", "❯ explain @co"}, nil)

		// A model of its own: a textarea's copies share its lines, so a draft
		// typed into one copy is not a clean start for another.
		dir, _ := atModel(t, size.cols, size.rows, atFixedListing(atComposerPaths...))
		dir = atOpen(t, dir, "@int")
		if names := atNames(dir); len(names) == 0 || names[0] != "internal/" {
			t.Fatalf("@int offers %q, want internal/ first", names)
		}
		dir, _ = atKey(t, dir, keyType(tea.KeyTab))
		if v, _ := draftOf(dir); v != "@internal/" || !dir.composerAt.visible() {
			t.Fatalf("tab on internal/ left %q (popup up %v), want @internal/ with the popup open", v, dir.composerAt.visible())
		}
		assertFrameGolden(t, "composer-at-dir"+suffix, size.cols, size.rows, plainView(dir),
			[]string{"files in ws", "❯ internal/cli/", "internal/tui/", "internal/engine/", "❯ @internal/"}, []string{"README.md"})
	}
}

// TestAComposerAtPickWritesExactlyItsPath (§3.16): a pick writes the
// candidate's path relative to the workspace as an `@` token and one space —
// quoted when the path has a space in it, as written when it is not ASCII,
// ending in `/` for a directory — in place of the token under the cursor and
// nothing else; tab on a directory writes `@dir/` with no space and stays
// open, and a pick inside it writes the whole path. Nothing is sent.
func TestAComposerAtPickWritesExactlyItsPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		// typed is typed; then keys, each after its search is delivered.
		typed string
		keys  []tea.KeyMsg
		// draft is the composer's text after, the cursor at its end but
		// where cursor says.
		draft  string
		cursor int
	}{
		{name: "plain", typed: "@main", keys: []tea.KeyMsg{enter()}, draft: "@cmd/craze/main.go ", cursor: -1},
		{name: "tab accepts a file", typed: "@main", keys: []tea.KeyMsg{keyType(tea.KeyTab)}, draft: "@cmd/craze/main.go ", cursor: -1},
		{name: "spaces", typed: "read @getting", keys: []tea.KeyMsg{enter()}, draft: `read @"docs/guide/getting started.md" `, cursor: -1},
		{name: "unicode", typed: "@caf", keys: []tea.KeyMsg{enter()}, draft: "@notes/café.md ", cursor: -1},
		{name: "a directory", typed: "@internal", keys: []tea.KeyMsg{enter()}, draft: "@internal/ ", cursor: -1},
		{
			name: "descend then accept", typed: "@int",
			keys:  []tea.KeyMsg{keyType(tea.KeyTab), runeKey('a'), runeKey('p'), runeKey('p'), enter()},
			draft: "@internal/tui/app.go ", cursor: -1,
		},
		{
			name: "descend then a quoted accept", typed: "@guide",
			keys:  []tea.KeyMsg{keyType(tea.KeyTab), runeKey('g'), runeKey('e'), runeKey('t'), keyType(tea.KeyTab)},
			draft: `@"docs/guide/getting started.md" `, cursor: -1,
		},
		{
			// The token inside the draft: the space after it is taken in, not
			// doubled, and the cursor lands on the word after it.
			name: "mid-draft", typed: "fix  now",
			keys: []tea.KeyMsg{
				keyType(tea.KeyLeft), keyType(tea.KeyLeft), keyType(tea.KeyLeft), keyType(tea.KeyLeft),
				runeKey('@'), runeKey('c'), runeKey('o'), runeKey('m'), runeKey('p'), enter(),
			},
			draft: "fix @internal/tui/complete.go now", cursor: len("fix @internal/tui/complete.go "),
		},
		{
			// Every `@` token opens the popup, and only the one under the
			// cursor is completed.
			name: "the token under the cursor", typed: "@README.md @eng",
			keys:  []tea.KeyMsg{enter()},
			draft: "@README.md @internal/engine/ ", cursor: -1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stub := atModel(t, 100, 30, atFixedListing(atComposerPaths...))
			m, load := typeAt(t, m, tc.typed)
			if load != nil {
				m = atSearched(t, m, load)
			}
			for _, k := range tc.keys {
				var l tea.Cmd
				m, l = atKey(t, m, k)
				if l != nil {
					m = atSearched(t, m, l)
				}
			}
			v, cur := draftOf(m)
			want := tc.cursor
			if want < 0 {
				want = len(tc.draft)
			}
			if v != tc.draft || cur != want {
				t.Fatalf("draft %q, cursor %d; want %q, cursor %d", v, cur, tc.draft, want)
			}
			if m.composerAt.visible() {
				t.Fatalf("the popup is still up after the pick:\n%s", plainView(m))
			}
			if got := stub.Prompts(); len(got) != 0 || m.status == statusWorking {
				t.Fatalf("a pick sent %q (status %v)", got, m.status)
			}
		})
	}
}

// TestAComposerAtRowShowsWhatItsPathHolds (§3.16, X200): the frame draws a
// path's format characters as `<U+XXXX>` — none raw, so no row reads as
// another path — and a pick writes the path as it is: its format characters,
// and a run of spaces its row draws as one.
func TestAComposerAtRowShowsWhatItsPathHolds(t *testing.T) {
	rlo, zwsp, spaces := "notes/\u202Egpj.exe", "zero\u200Bwidth.go", "two  spaces.md"
	listing := atFixedListing(rlo, zwsp, spaces)

	m, _ := atModel(t, 100, 30, listing)
	m = atOpen(t, m, "@")
	if frame := m.View(); strings.IndexFunc(frame, atIsFormat) >= 0 {
		t.Fatalf("the frame draws a format character raw:\n%q", plain(frame))
	}
	frame := plainView(m)
	for _, row := range []string{"notes/<U+202E>gpj.exe", "zero<U+200B>width.go", "two spaces.md"} {
		if !strings.Contains(frame, row) {
			t.Fatalf("no row reads %q:\n%s", row, frame)
		}
	}

	for typed, draft := range map[string]string{
		"@gpj":  "@" + rlo + " ",
		"@zero": "@" + zwsp + " ",
		"@two":  `@"two  spaces.md" `,
	} {
		// A model of its own for each: a textarea's copies share its lines.
		m, _ := atModel(t, 100, 30, listing)
		m = atOpen(t, m, typed)
		m, _ = atKey(t, m, enter())
		if v, cur := draftOf(m); v != draft || cur != len(draft) {
			t.Errorf("picking %s left %q, cursor %d; want %q, cursor %d", typed, v, cur, draft, len(draft))
		}
	}
}

// TestTheComposerAtPopupOpensOnATokenStart (§3.16): the popup is up while the
// cursor is inside an `@` token that starts a word — at the draft's start,
// after a space or a newline — and not for an `@` inside a word, not with the
// cursor on the `@` itself, not in shell mode, and not without a workspace.
func TestTheComposerAtPopupOpensOnATokenStart(t *testing.T) {
	cases := []struct {
		name   string
		typed  string
		keys   []tea.KeyMsg
		up     bool
		prep   func(Model) Model
		reason string
	}{
		{name: "the draft's start", typed: "@", up: true},
		{name: "after a space", typed: "see @x", up: true},
		{name: "after a newline", typed: "first", keys: []tea.KeyMsg{{Type: tea.KeyEnter, Alt: true}, runeKey('@')}, up: true},
		{name: "inside a word", typed: "user@example", up: false},
		{name: "on the @ itself", typed: "@x", keys: []tea.KeyMsg{keyType(tea.KeyLeft), keyType(tea.KeyLeft)}, up: false},
		{name: "one rune into the token", typed: "@x", keys: []tea.KeyMsg{keyType(tea.KeyLeft)}, up: true},
		{name: "shell mode", typed: "!ls @", up: false},
		{name: "no workspace", typed: "@", up: false, prep: func(m Model) Model { m.cwd = ""; return m }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := atModel(t, 100, 30, atFixedListing(atComposerPaths...))
			if tc.prep != nil {
				m = tc.prep(m)
			}
			m, _ = typeAt(t, m, tc.typed)
			for _, k := range tc.keys {
				m, _ = press(m, k)
			}
			if got := m.composerAtShown(); got != tc.up {
				t.Fatalf("popup up %v, want %v:\n%s", got, tc.up, plainView(m))
			}
			if frame := plainView(m); strings.Contains(frame, "files in ") != tc.up {
				t.Fatalf("the frame draws the popup: %v, want %v:\n%s", !tc.up, tc.up, frame)
			}
		})
	}
}

// TestTheComposerAtPopupIsTheComposersInEveryMode (§3.16): the popup is a
// composer feature — the in-process opt-out's (no session list) and a
// detached craze's (a session list) alike — and its root is the session's
// workspace.
func TestTheComposerAtPopupIsTheComposersInEveryMode(t *testing.T) {
	optOut, _ := atModel(t, 100, 30, atFixedListing(atComposerPaths...))
	if optOut.sessions != nil {
		t.Fatal("fixture: the opt-out's model has a session list")
	}
	detached, _, _ := newSessModel(t, 100, 30)
	held := newAtHeld(atComposerPaths...)
	detached.composerAt.setSource(atFileSource{search: held.search})
	for _, c := range []struct {
		name  string
		m     Model
		held  *atHeld
		wants string
	}{{"the opt-out", optOut, nil, "internal/tui/complete.go"}, {"detached", detached, held, "internal/tui/complete.go"}} {
		t.Run(c.name, func(t *testing.T) {
			m, load := typeAt(t, c.m, "@comp")
			if !m.composerAtShown() || load == nil {
				t.Fatalf("no popup, or no search, under %s:\n%s", c.name, plainView(m))
			}
			ch := runLoad(t, load)
			if c.held != nil {
				if root := c.held.awaitStarted(t); root != m.sessHereDir() {
					t.Fatalf("searched %q, want the session's workspace %q", root, m.sessHereDir())
				}
				close(c.held.release)
			}
			tm, _ := m.Update(awaitLoaded(t, ch))
			m = tm.(Model)
			if names := atNames(m); len(names) == 0 || names[0] != c.wants {
				t.Fatalf("offers %q, want %s first", names, c.wants)
			}
		})
	}
}

// TestTheComposerAtPopupClosesUnderACardOrADialog (§3.16): a dialog or a card
// over the composer closes the popup — its search cancelled — and a
// dialog's closing brings it back over the same token, a new opening.
func TestTheComposerAtPopupClosesUnderACardOrADialog(t *testing.T) {
	held := newAtHeld(atComposerPaths...)
	m, stub := atModel(t, 100, 30, held.search)
	m, load := typeAt(t, m, "@comp")
	ch := runLoad(t, load)
	held.awaitStarted(t)
	opening := m.composerAt.gen

	m, _ = press(m, keyType(tea.KeyCtrlG))
	if m.dialog != dialogTheme || m.composerAt.visible() || m.composerAtShown() {
		t.Fatalf("the theme picker left the popup up (dialog %v):\n%s", m.dialog, plainView(m))
	}
	if err := held.awaitDone(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("the search under the dialog ended with %v, want it cancelled", err)
	}
	late := awaitLoaded(t, ch)
	m, again := atKey(t, m, keyType(tea.KeyEsc))
	if m.dialog != dialogNone || !m.composerAt.visible() || m.composerAt.gen == opening || again == nil {
		t.Fatalf("closing the dialog did not open the popup again, afresh and searching (up %v, opening %d → %d)",
			m.composerAt.visible(), opening, m.composerAt.gen)
	}
	tm, _ := m.Update(late)
	m = tm.(Model)
	if !m.composerAt.pending() || len(m.composerAt.ans.Items) != 0 {
		t.Fatalf("the cancelled search's answer was taken by the new opening: %q", atNames(m))
	}
	// The new opening's own search; then a card over the composer.
	ch = runLoad(t, again)
	held.awaitStarted(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(false)})
	if m.composerAt.visible() || m.composerAtShown() {
		t.Fatalf("a card left the popup up:\n%s", plainView(m))
	}
	if err := held.awaitDone(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("the search under the card ended with %v, want it cancelled", err)
	}
	awaitLoaded(t, ch)
}

// TestComposerAtEnterAcceptsOnlyWithACandidate (§3.16, X127): enter with a
// candidate picks it and sends nothing; with none — nothing matches, or the
// search has not answered — enter is the composer's and the draft goes as
// typed.
func TestComposerAtEnterAcceptsOnlyWithACandidate(t *testing.T) {
	t.Run("nothing matches", func(t *testing.T) {
		m, _ := atModel(t, 100, 30, atFixedListing(atComposerPaths...))
		m = atOpen(t, m, "look at @zzz")
		if !m.composerAt.visible() || len(m.composerAt.ans.Items) != 0 || !strings.Contains(plainView(m), completeEmptyNote) {
			t.Fatalf("fixture: @zzz should be up with nothing to offer:\n%s", plainView(m))
		}
		m, _ = press(m, enter())
		if got := texts(m, entryUser); m.status != statusWorking || !slices.Equal(got, []string{"look at @zzz"}) {
			t.Fatalf("enter with no candidate: status %v, sent %q", m.status, got)
		}
	})
	t.Run("still searching", func(t *testing.T) {
		held := newAtHeld(atComposerPaths...)
		m, _ := atModel(t, 100, 30, held.search)
		m, load := typeAt(t, m, "@comp")
		ch := runLoad(t, load)
		held.awaitStarted(t)
		if !m.composerAt.pending() || !strings.Contains(plainView(m), completePendingNote) {
			t.Fatalf("fixture: the popup should be searching:\n%s", plainView(m))
		}
		m, _ = press(m, enter())
		if got := texts(m, entryUser); m.status != statusWorking || !slices.Equal(got, []string{"@comp"}) {
			t.Fatalf("enter while searching: status %v, sent %q", m.status, got)
		}
		if err := held.awaitDone(t); !errors.Is(err, context.Canceled) {
			t.Fatalf("the search the send left ended with %v, want it cancelled", err)
		}
		awaitLoaded(t, ch)
	})
	t.Run("a candidate", func(t *testing.T) {
		m, stub := atModel(t, 100, 30, atFixedListing(atComposerPaths...))
		m = atOpen(t, m, "@comp")
		m, _ = press(m, enter())
		if v, _ := draftOf(m); v != "@internal/tui/complete.go " || m.status == statusWorking || len(stub.Prompts()) != 0 {
			t.Fatalf("enter on a candidate: draft %q, status %v, sent %q", v, m.status, stub.Prompts())
		}
		// The next enter, the popup closed past the pick's space, sends.
		m, _ = press(m, enter())
		if got := texts(m, entryUser); !slices.Equal(got, []string{"@internal/tui/complete.go"}) {
			t.Fatalf("the second enter sent %q", got)
		}
	})
}

// TestComposerAtEscHidesUntilTheTokenChanges (§3.16, X127): esc hides the
// popup and leaves the draft; the same token keeps it hidden, the cursor
// moving within it included, and typing into it brings it back.
func TestComposerAtEscHidesUntilTheTokenChanges(t *testing.T) {
	m, _ := atModel(t, 100, 30, atFixedListing(atComposerPaths...))
	m = atOpen(t, m, "@comp")
	m, _ = press(m, keyType(tea.KeyEsc))
	if v, _ := draftOf(m); v != "@comp" || m.composerAt.visible() || strings.Contains(plainView(m), "files in ") {
		t.Fatalf("esc: draft %q, popup up %v:\n%s", v, m.composerAt.visible(), plainView(m))
	}
	if m.status == statusWorking {
		t.Fatal("esc on the popup did something else too")
	}
	// The cursor moving within the token, leaving it (onto its `@`) and
	// coming back, and a tick: still hidden.
	for _, k := range []tea.KeyType{tea.KeyLeft, tea.KeyLeft, tea.KeyLeft, tea.KeyLeft, tea.KeyLeft} {
		m, _ = press(m, keyType(k))
		if m.composerAt.visible() {
			t.Fatalf("the hidden popup came back over the same token, the cursor at %d", m.composerCursorOffset())
		}
	}
	for range 5 {
		m, _ = press(m, keyType(tea.KeyRight))
	}
	tm, _ := m.Update(tickMsg{})
	m = tm.(Model)
	if _, cur := draftOf(m); m.composerAt.visible() || cur != len("@comp") {
		t.Fatalf("the hidden popup came back over the same token (cursor %d)", cur)
	}
	m, load := typeAt(t, m, "o")
	m = atSearched(t, m, load)
	if !m.composerAt.visible() || atNames(m)[0] != "internal/tui/composer.go" {
		t.Fatalf("typing into the token did not bring the popup back: %q", atNames(m))
	}
}

// TestComposerAtEscUnderARunningTurnHidesThenCancels (§3.16): the popup
// takes Esc where the slash menu does — the first Esc hides it and does
// nothing else, the second cancels the running turn.
func TestComposerAtEscUnderARunningTurnHidesThenCancels(t *testing.T) {
	m, _ := queueWorking(t)
	m.composerAt.setSource(atFileSource{search: atFixedListing(atComposerPaths...)})
	m = atOpen(t, m, "see @comp")
	if !m.composerAtActive() {
		t.Fatal("fixture: the popup should be up mid-message")
	}
	m, _ = press(m, keyType(tea.KeyEsc))
	if m.composerAt.visible() || m.cardMask != "" || m.status != statusWorking {
		t.Fatalf("the first esc: popup up %v, cancel mask %q, status %v", m.composerAt.visible(), m.cardMask, m.status)
	}
	m, _ = press(m, keyType(tea.KeyEsc))
	if m.cardMask == "" {
		t.Fatal("the second esc did not cancel the turn")
	}
	if v, _ := draftOf(m); v != "see @comp" {
		t.Fatalf("the draft moved: %q", v)
	}
}

// atSlashDraft is an unfinished quoted `@` token with a `/` after its space:
// the popup completes it, and slash.go's tokenizer, walking back from the
// cursor to the nearest space, reads its `/` as a bare slash token too.
const atSlashDraft = `@"a /`

// atSlashModel is atModel with an agent advertising commit and review — set
// before the start, which folds them into the catalog (slashModel's way).
func atSlashModel(t *testing.T, cols, rows int) Model {
	t.Helper()
	isolateSkillsHome(t)
	stub := NewStub()
	setStubCommands(stub, "commit", "review")
	m := startStub(t, stub, frameWorkspace(t), cols, rows)
	m.frozen = true
	m.composerAt.setSource(atFileSource{search: atFixedListing(atComposerPaths...)})
	return m
}

// atSlashOverlap types atSlashDraft into m, whose agent advertises a command,
// and delivers the popup's search: the popup up with nothing to offer (no
// path lies under `a /`), and slash.go's own gate (slashActive) reading a
// menu under it — the overlap sol r37-c18 found, so a test built on it bites
// wherever the composer's gate (composerAtHolds) is missing.
func atSlashOverlap(t *testing.T, m Model) Model {
	t.Helper()
	m = atOpen(t, m, atSlashDraft)
	if !m.composerAtActive() || len(m.composerAt.ans.Items) != 0 {
		t.Fatalf("fixture: %s should have the popup up with nothing to offer (%q):\n%s", atSlashDraft, atNames(m), plainView(m))
	}
	if !m.slashActive() {
		t.Fatalf("fixture: slash.go should read a menu inside the `@` token:\n%s", plainView(m))
	}
	return m
}

// assertNoSlashBand says the band draws no slash menu and the menu has no
// key: none of the rows slash.go reads is in the frame, and the composer
// grants the menu nothing.
func assertNoSlashBand(t *testing.T, m Model, when string) {
	t.Helper()
	frame := plainView(m)
	items := m.filteredSlash()
	if len(items) == 0 {
		t.Fatalf("%s: fixture: slash.go reads no menu to keep out", when)
	}
	for _, it := range items {
		if strings.Contains(frame, it.Desc) {
			t.Fatalf("%s: the slash menu's %s is drawn inside the `@` token:\n%s", when, it.Name, frame)
		}
	}
	if m.slashBandActive() {
		t.Fatalf("%s: the slash menu has the keys inside the `@` token", when)
	}
}

// TestComposerAtSlashOverlapLeavesPagingToTheTranscript (§3.16, X187; sol
// r37-c18): inside a quoted `@"a /`, PgUp/PgDn and the wheel over the popup
// scroll the transcript — the slash menu slash.go reads at the `/` is not
// drawn and takes none of them.
func TestComposerAtSlashOverlapLeavesPagingToTheTranscript(t *testing.T) {
	m := atSlashModel(t, 100, 30)
	var b strings.Builder
	for i := range 60 {
		fmt.Fprintf(&b, "line %02d\n\n", i)
	}
	tm, _ := m.Update(eventMsg{ev: agent.Event{Type: agent.EventText, Text: b.String()}})
	m = atSlashOverlap(t, tm.(Model))
	assertNoSlashBand(t, m, "the popup up")
	bottom := m.vp.YOffset
	if !m.vp.AtBottom() || bottom < 2*wheelLines {
		t.Fatalf("fixture: the transcript should be at its bottom with scrollback (offset %d)", bottom)
	}

	m, _ = press(m, keyType(tea.KeyPgUp))
	if m.vp.YOffset >= bottom || m.slashSel != 0 {
		t.Fatalf("PgUp: transcript at %d (was %d), slash selection %d", m.vp.YOffset, bottom, m.slashSel)
	}
	m, _ = press(m, keyType(tea.KeyPgDown))
	if m.vp.YOffset != bottom || m.slashSel != 0 {
		t.Fatalf("PgDn: transcript at %d, want %d; slash selection %d", m.vp.YOffset, bottom, m.slashSel)
	}
	y := m.lay.Region(regionOverlay).Top
	if !m.composerAtShown() || !m.lay.Region(regionOverlay).Contains(y) {
		t.Fatalf("fixture: the popup should still be drawn in the band:\n%s", plainView(m))
	}
	m = mouse(t, m, tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp, X: 1, Y: y})
	if m.vp.YOffset != bottom-wheelLines || m.slashSel != 0 {
		t.Fatalf("the wheel over the popup: transcript at %d, want %d; slash selection %d", m.vp.YOffset, bottom-wheelLines, m.slashSel)
	}
	if v, _ := draftOf(m); v != atSlashDraft {
		t.Fatalf("the draft moved: %q", v)
	}
}

// TestComposerAtSlashOverlapEnterSendsTheDraft (§3.16, X127; sol r37-c18):
// inside a quoted `@"a /` with no file to offer, enter is the composer's and
// the draft goes as typed — the slash menu slash.go reads at the `/` does not
// write a command into it.
func TestComposerAtSlashOverlapEnterSendsTheDraft(t *testing.T) {
	m := atSlashOverlap(t, atSlashModel(t, 100, 30))
	m, _ = press(m, enter())
	if got := texts(m, entryUser); m.status != statusWorking || !slices.Equal(got, []string{atSlashDraft}) {
		v, _ := draftOf(m)
		t.Fatalf("enter with no candidate: status %v, sent %q, draft %q", m.status, got, v)
	}
}

// TestComposerAtSlashOverlapEscHidesThenCancels (§3.16, X187; sol r37-c18):
// inside a quoted `@"a /` under a running turn, the first esc hides the popup
// and brings no slash menu up in its place, and the second esc cancels the
// turn.
func TestComposerAtSlashOverlapEscHidesThenCancels(t *testing.T) {
	// queueWorking's agent advertises one command (the Stub's research),
	// which a bare `/` offers.
	m, _ := queueWorking(t)
	m.composerAt.setSource(atFileSource{search: atFixedListing(atComposerPaths...)})
	m = atSlashOverlap(t, m)
	m, _ = press(m, keyType(tea.KeyEsc))
	if m.composerAt.visible() || m.cardMask != "" || m.status != statusWorking {
		t.Fatalf("the first esc: popup up %v, cancel mask %q, status %v", m.composerAt.visible(), m.cardMask, m.status)
	}
	assertNoSlashBand(t, m, "the popup hidden")
	if h := m.lay.Region(regionOverlay).Height(); h != 0 {
		t.Fatalf("the band kept %d rows with the popup hidden:\n%s", h, plainView(m))
	}
	m, _ = press(m, keyType(tea.KeyEsc))
	if m.cardMask == "" {
		t.Fatalf("the second esc did not cancel the turn:\n%s", plainView(m))
	}
	if v, _ := draftOf(m); v != atSlashDraft {
		t.Fatalf("the draft moved: %q", v)
	}
}

// TestComposerAtSlashOverlapStaysOutWhileTheQueueHasTheKeys (§3.16; sol
// r37-c18): the kind of a token is the draft's, not the focus's — with the
// popup hidden in a quoted `@"a /`, ↑ takes the keyboard to the queue (no
// menu to take it), and the slash menu does not come up under the queue
// though the popup, the composer no longer having the keyboard, is closed.
func TestComposerAtSlashOverlapStaysOutWhileTheQueueHasTheKeys(t *testing.T) {
	m, _ := queueWorking(t)
	m.composerAt.setSource(atFileSource{search: atFixedListing(atComposerPaths...)})
	m = typeEnter(t, m, "queued first")
	if len(queueTexts(m)) != 1 {
		t.Fatalf("fixture: nothing queued: %q", queueTexts(m))
	}
	m = atSlashOverlap(t, m)
	m, _ = press(m, keyType(tea.KeyEsc))
	m, _ = press(m, keyType(tea.KeyUp))
	if !m.queueFocus || m.composerAtOn() {
		t.Fatalf("↑ with the popup hidden did not reach the queue (focus %v):\n%s", m.queueFocus, plainView(m))
	}
	assertNoSlashBand(t, m, "the queue focused")
	if v, _ := draftOf(m); v != atSlashDraft {
		t.Fatalf("the draft moved: %q", v)
	}
}

// TestComposerAtSlashMenuOutsideAnAtToken (§3.16; sol r37-c18): the slash
// menu is refused only inside an `@` token — a draft `/co`, and `x @a /co`
// with the cursor after `/co`, past the `@a` token, still draw it and tab
// still accepts from it, the composer's popup and all.
func TestComposerAtSlashMenuOutsideAnAtToken(t *testing.T) {
	for _, tc := range []struct{ typed, want string }{
		{"/co", "/commit "},
		{"x @a /co", "x @a /commit "},
	} {
		t.Run(tc.typed, func(t *testing.T) {
			m, _ := typeAt(t, atSlashModel(t, 100, 30), tc.typed)
			if m.composerAtShown() {
				t.Fatalf("fixture: the cursor should be outside any `@` token:\n%s", plainView(m))
			}
			if !m.slashBandActive() || m.lay.Region(regionOverlay).Height() == 0 || !strings.Contains(plainView(m), "advertised commit") {
				t.Fatalf("no slash menu for %q:\n%s", tc.typed, plainView(m))
			}
			m, _ = press(m, keyType(tea.KeyTab))
			if v, cur := draftOf(m); v != tc.want || cur != len(tc.want) {
				t.Fatalf("tab: draft %q, cursor %d; want %q at its end", v, cur, tc.want)
			}
		})
	}
}

// TestComposerAtKeysShadowTheComposersOnlyWhileUp (§3.16): while the popup is
// up ↑/↓ choose in it rather than moving the keyboard to the queue, ctrl+n
// and ctrl+p rather than moving between the draft's lines; alt+enter is still
// the composer's newline, which ends the token and closes the popup. With no
// popup, ↑ reaches the queue as ever.
func TestComposerAtKeysShadowTheComposersOnlyWhileUp(t *testing.T) {
	m, _ := queueWorking(t)
	m.frozen = true
	m.composerAt.setSource(atFileSource{search: atFixedListing(atComposerPaths...)})
	m = typeEnter(t, m, "queued first")
	if len(queueTexts(m)) != 1 {
		t.Fatalf("fixture: nothing queued: %q", queueTexts(m))
	}
	m = atOpen(t, m, "@comp")
	first := atNames(m)[0]
	m, _ = press(m, keyType(tea.KeyUp))
	if m.queueFocus || m.composerAt.sel == 0 || !m.composerAt.visible() {
		t.Fatalf("↑ with the popup up: queue focus %v, selection %d", m.queueFocus, m.composerAt.sel)
	}
	m, _ = press(m, keyType(tea.KeyCtrlN))
	if m.composerAt.sel != 0 {
		t.Fatalf("ctrl+n did not move the popup's selection back to %s (at %d)", first, m.composerAt.sel)
	}
	m, _ = press(m, keyType(tea.KeyCtrlP))
	m, _ = press(m, keyType(tea.KeyDown))
	if m.composerAt.sel != 0 {
		t.Fatalf("ctrl+p then ↓ is back where it began: at %d", m.composerAt.sel)
	}
	m, _ = press(m, tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if v, _ := draftOf(m); v != "@comp\n" || m.composerAt.visible() {
		t.Fatalf("alt+enter: draft %q, popup up %v", v, m.composerAt.visible())
	}
	m, _ = press(m, keyType(tea.KeyBackspace))
	m, _ = press(m, keyType(tea.KeyEsc))
	m.input.SetValue("")
	m, _ = press(m, keyType(tea.KeyUp))
	if !m.queueFocus {
		t.Fatal("↑ with no popup up did not reach the queue")
	}
}

// TestComposerAtASwitchClosesThePopupAndCancelsItsSearch (§3.16, X125): the
// popup is the session shown's — a switch to another session closes it,
// cancels the search it had running, and the search's late answer is left
// behind by the gate (staleShown); the next session's draft brings nothing of
// it back.
func TestComposerAtASwitchClosesThePopupAndCancelsItsSearch(t *testing.T) {
	a, b := newLane(t, "sess-a", "alpha"), newLane(t, "sess-b", "bravo")
	r, _ := laneModel(t, a, map[string][]*laneBackend{"sess-b": {b}})
	held := newAtHeld(atComposerPaths...)
	r.m.composerAt.setSource(atFileSource{search: held.search})
	lateAt := make(chan tea.Msg, 1)
	for _, ch := range "@comp" {
		r.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}})
		if l := atLoadIn(r.last); l != nil {
			go func() { lateAt <- l() }()
		}
	}
	if root := held.awaitStarted(t); root != a.info.Workspace || !r.m.composerAtShown() {
		t.Fatalf("fixture: searching %q (popup up %v), want alpha's workspace", root, r.m.composerAtShown())
	}
	next, cmd := r.m.switchBackend(b, false, "")
	r.m = next
	r.sort(cmd)
	if r.m.composerAt.visible() {
		t.Fatal("the switch left the popup up")
	}
	if err := held.awaitDone(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("the search the switch left ended with %v, want it cancelled", err)
	}
	var late tea.Msg
	select {
	case late = <-lateAt:
	case <-time.After(completeStep):
		t.Fatalf("the cancelled search did not answer within %v", completeStep)
	}
	r.dropped("the search the switch left", late)
	if r.m.composerAt.visible() || r.m.composerAtShown() {
		t.Fatal("the late answer opened the popup over bravo")
	}
}

// TestComposerAtAResultAfterThePopupClosedIsDropped (§3.16, X122–X123): a
// search whose popup has closed — the token ended with a space — is
// cancelled, and its answer, when it comes, is taken by nothing: not the
// closed popup, and not the popup the same token opens next, which waits for
// its own search.
func TestComposerAtAResultAfterThePopupClosedIsDropped(t *testing.T) {
	held := newAtHeld(atComposerPaths...)
	m, _ := atModel(t, 100, 30, held.search)
	m, load := typeAt(t, m, "@comp")
	ch := runLoad(t, load)
	held.awaitStarted(t)

	m, _ = typeAt(t, m, " ")
	if m.composerAt.visible() {
		t.Fatal("a space after the token left the popup up")
	}
	if err := held.awaitDone(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("the closed popup's search ended with %v, want it cancelled", err)
	}
	late := awaitLoaded(t, ch)
	tm, _ := m.Update(late)
	m = tm.(Model)
	if m.composerAt.visible() {
		t.Fatal("the closed popup's answer opened it")
	}

	m, again := atKey(t, m, keyType(tea.KeyBackspace))
	if !m.composerAt.pending() || again == nil {
		t.Fatalf("backspace into the token did not open it afresh, searching (up %v)", m.composerAt.visible())
	}
	tm, _ = m.Update(late)
	m = tm.(Model)
	if !m.composerAt.pending() || len(m.composerAt.ans.Items) != 0 {
		t.Fatalf("the old search's answer was taken by the new opening: %q", atNames(m))
	}
	ch = runLoad(t, again)
	held.awaitStarted(t)
	close(held.release)
	tm, _ = m.Update(awaitLoaded(t, ch))
	m = tm.(Model)
	if names := atNames(m); len(names) == 0 || names[0] != "internal/tui/complete.go" {
		t.Fatalf("the new opening's own search offers %q", names)
	}
}

// TestTheComposerAtTitleSaysWhoseFilesAndWhyNotAll (§3.16, X179): every
// answer of the file source is titled with the workspace by its status-row
// name — while the search runs, when it failed, when it listed nothing — and
// a list that is not the workspace's whole says why after it.
func TestTheComposerAtTitleSaysWhoseFilesAndWhyNotAll(t *testing.T) {
	const root = "/home/u/projects/craze"
	src := atFileSource{}
	ask := func(l *completeLoaded) completeAnswer {
		q := completeQuery{Text: "", Workspace: root}
		if l != nil {
			q.loads = map[string]completeLoaded{root: *l}
		}
		return src.complete(q)
	}
	for _, c := range []struct {
		name  string
		l     *completeLoaded
		title string
		note  string
	}{
		{"searching", nil, "files in craze", ""},
		{"whole", &completeLoaded{Data: newAtFileIndex([]string{"a.go"}, "")}, "files in craze", ""},
		{"cut at the cap", &completeLoaded{Data: newAtFileIndex([]string{"a.go"}, atFilesCapTitle)}, "files in craze · only the first 50,000 files", ""},
		{"stopped by the clock", &completeLoaded{Data: newAtFileIndex([]string{"a.go"}, atFilesTimeoutTitle)}, "files in craze · listing stopped after 3s", ""},
		{"empty", &completeLoaded{Data: newAtFileIndex(nil, "")}, "files in craze", "no files here"},
		{"failed", &completeLoaded{Err: errAtFilesTimeout}, "files in craze", "listing files took over 3s"},
	} {
		if a := ask(c.l); a.Title != c.title || a.Note != c.note {
			t.Errorf("%s: title %q, note %q; want %q, %q", c.name, a.Title, a.Note, c.title, c.note)
		}
	}
}
