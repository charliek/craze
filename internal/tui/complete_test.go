package tui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The completion component (complete.go, plan 030 §3.15) driven directly:
// nothing is wired to a screen in C13, so every test here holds a popup and
// plays its owner — syncing it after each edit, handing it keys, running the
// loads it returns and handing their results back.

// ------------------------------------------------------------------ fixtures

// completeStep bounds each wait of a lifecycle test on its own, never one
// deadline for a whole sequence.
const completeStep = 5 * time.Second

// listSource answers synchronously from a list it holds: the candidates whose
// name starts with the query, case-insensitively, in list order.
type listSource struct {
	id    string
	items []completeItem
	asked int
}

func (s *listSource) completeID() string { return s.id }

func (s *listSource) complete(q completeQuery) completeAnswer {
	s.asked++
	var out []completeItem
	for _, it := range s.items {
		if strings.HasPrefix(strings.ToLower(it.Name), strings.ToLower(q.Text)) {
			out = append(out, it)
		}
	}
	return completeAnswer{Items: out}
}

// named is a candidate that writes its own name.
func named(names ...string) []completeItem {
	out := make([]completeItem, 0, len(names))
	for _, n := range names {
		out = append(out, completeItem{Name: n, Value: "/abs/" + n, Insert: n})
	}
	return out
}

// numbered is n candidates, c00 … c<n-1>.
func numbered(n int) []completeItem {
	names := make([]string, 0, n)
	for i := range n {
		names = append(names, fmt.Sprintf("c%02d", i))
	}
	return named(names...)
}

// treeSource is the asynchronous fake: a query with a "/" in it lists its
// parent — a load keyed by the parent, which blocks until the test releases
// it (or cancels) — and filters the children by what follows; a query with
// none filters names synchronously. Every load that starts announces itself
// on started, so a test holds each one's controls.
type treeSource struct {
	id      string
	names   []completeItem
	tree    map[string][]string
	started chan *treeRun
}

type treeRun struct {
	key       string
	release   chan struct{}
	cancelled chan struct{}
}

func newTreeSource() *treeSource {
	return &treeSource{
		id:    "tree",
		names: named("lumen", "roost"),
		tree: map[string][]string{
			"~":          {"projects", "src", "my dir"},
			"~/projects": {"lumen", "roost"},
		},
		started: make(chan *treeRun, 16),
	}
}

func (s *treeSource) completeID() string { return s.id }

func (s *treeSource) complete(q completeQuery) completeAnswer {
	i := strings.LastIndex(q.Text, "/")
	if i < 0 {
		var out []completeItem
		for _, it := range s.names {
			if strings.HasPrefix(it.Name, q.Text) {
				out = append(out, it)
			}
		}
		return completeAnswer{Items: out}
	}
	parent, partial := q.Text[:i], q.Text[i+1:]
	ans := completeAnswer{Title: "folders in " + parent}
	l, ok := q.Loaded(parent)
	if !ok {
		ans.Load = &completeLoad{Key: parent, Run: s.run(parent)}
		return ans
	}
	if l.Err != nil {
		ans.Note, ans.NoteErr = "no directory "+parent, true
		return ans
	}
	for _, it := range l.Items {
		if strings.HasPrefix(it.Name, partial) {
			ans.Items = append(ans.Items, it)
		}
	}
	return ans
}

func (s *treeSource) run(parent string) func(context.Context) completeLoaded {
	return func(ctx context.Context) completeLoaded {
		r := &treeRun{key: parent, release: make(chan struct{}), cancelled: make(chan struct{})}
		s.started <- r
		select {
		case <-r.release:
		case <-ctx.Done():
			close(r.cancelled)
			return completeLoaded{Err: ctx.Err()}
		}
		kids, ok := s.tree[parent]
		if !ok {
			return completeLoaded{Err: fs.ErrNotExist}
		}
		items := make([]completeItem, 0, len(kids))
		for _, k := range kids {
			path := parent + "/" + k
			items = append(items, completeItem{Name: k + "/", Detail: path, Value: path, Insert: path, Openable: true})
		}
		return completeLoaded{Items: items}
	}
}

// awaitRun is the next load the source started, within one step.
func (s *treeSource) awaitRun(t *testing.T) *treeRun {
	t.Helper()
	select {
	case r := <-s.started:
		return r
	case <-time.After(completeStep):
		t.Fatalf("no load started within %v", completeStep)
		return nil
	}
}

// noRun says no further load started (a load already running or already back
// is never started again).
func (s *treeSource) noRun(t *testing.T) {
	t.Helper()
	select {
	case r := <-s.started:
		t.Fatalf("a load of %q started", r.key)
	default:
	}
}

// awaitCancel waits, within one step, for r's work to see its cancel.
func awaitCancel(t *testing.T, r *treeRun) {
	t.Helper()
	select {
	case <-r.cancelled:
	case <-time.After(completeStep):
		t.Fatalf("the load of %q was not cancelled within %v", r.key, completeStep)
	}
}

// runLoad runs a load's command on a goroutine of its own, as bubbletea does;
// its result arrives on the channel.
func runLoad(t *testing.T, cmd tea.Cmd) <-chan completeLoadedMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("no load to run")
	}
	ch := make(chan completeLoadedMsg, 1)
	go func() { ch <- cmd().(completeLoadedMsg) }()
	return ch
}

// awaitLoaded is a load's result, within one step.
func awaitLoaded(t *testing.T, ch <-chan completeLoadedMsg) completeLoadedMsg {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(completeStep):
		t.Fatalf("no load answered within %v", completeStep)
		return completeLoadedMsg{}
	}
}

// synced is p synced to value with the cursor at its end.
func synced(p *completePopup, value string, env completeEnv) tea.Cmd {
	return p.sync(value, len(value), env)
}

// popupNames is the names of the popup's candidates.
func popupNames(p completePopup) []string {
	var out []string
	for _, it := range p.ans.Items {
		out = append(out, it.Name)
	}
	return out
}

// popupText is the popup drawn at width in at most rows lines, as text.
func popupText(p completePopup, width, rows int) []string {
	v := p.view(Preset("dark"), width, rows)
	if v == "" {
		return nil
	}
	return strings.Split(plain(v), "\n")
}

// trimmed is lines with their trailing padding cut.
func trimmed(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimRight(l, " ")
	}
	return out
}

// popKey hands p one key.
func popKey(p *completePopup, k tea.KeyMsg) (completeChoice, bool) { return p.key(k) }

func keyOf(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

// ---------------------------------------------------------- the @ grammar

func TestAnAtTokenIsReadByTheGrammar(t *testing.T) {
	cases := []struct {
		name   string
		value  string
		start  int
		want   atToken
		wantOK bool
	}{
		{"a bare word", "@foo", 0, atToken{start: 0, end: 4, text: "foo"}, true},
		{"a bare sigil", "@", 0, atToken{start: 0, end: 1, text: ""}, true},
		{"after a space", "see @foo bar", 4, atToken{start: 4, end: 8, text: "foo"}, true},
		{"inside a word", "a@foo", 1, atToken{}, false},
		{"not an @", "foo", 0, atToken{}, false},
		{"after a non-breaking space", "x\u00a0@foo", 3, atToken{start: 3, end: 7, text: "foo"}, true},
		{"after a newline", "one\n@two", 4, atToken{start: 4, end: 8, text: "two"}, true},
		{"a tab ends it", "@foo\tbar", 0, atToken{start: 0, end: 4, text: "foo"}, true},
		{"an @ inside it", "@foo@bar", 0, atToken{start: 0, end: 8, text: "foo@bar"}, true},
		{"Unicode", "@café/ü x", 0, atToken{start: 0, end: len("@café/ü"), text: "café/ü"}, true},
		{"quoted", `@"my dir" rest`, 0, atToken{start: 0, end: 9, text: "my dir", quoted: true, closed: true}, true},
		{"an escaped quote", `@"say \"hi\""`, 0, atToken{start: 0, end: 13, text: `say "hi"`, quoted: true, closed: true}, true},
		{"an escaped backslash", `@"a\\b"`, 0, atToken{start: 0, end: 7, text: `a\b`, quoted: true, closed: true}, true},
		{"another backslash kept", `@"a\nb"`, 0, atToken{start: 0, end: 7, text: `a\nb`, quoted: true, closed: true}, true},
		{"still open", `@"my di`, 0, atToken{start: 0, end: 7, text: "my di", quoted: true}, true},
		{"open at a trailing backslash", `@"a\`, 0, atToken{start: 0, end: 4, text: `a\`, quoted: true}, true},
		{"open up to a newline", "@\"my di\nnext", 0, atToken{start: 0, end: 7, text: "my di", quoted: true}, true},
		{"an empty quote", `@""`, 0, atToken{start: 0, end: 3, text: "", quoted: true, closed: true}, true},
		{"quoted Unicode", `@"日本 語"`, 0, atToken{start: 0, end: len(`@"日本 語"`), text: "日本 語", quoted: true, closed: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := atTokenAt(c.value, c.start)
			if ok != c.wantOK || got != c.want {
				t.Fatalf("atTokenAt(%q, %d) = %+v, %v; want %+v, %v", c.value, c.start, got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestTheTokenUnderTheCursorIsTheOneItIsInside(t *testing.T) {
	cases := []struct {
		name   string
		value  string
		cursor int
		start  int // -1: none
	}{
		{"on the @", "@foo", 0, -1},
		{"just after the @", "@foo", 1, 0},
		{"at the token's end", "@foo", 4, 0},
		{"past the space that ends it", "@foo bar", 5, -1},
		{"a later token", "a @b c @dee", 11, 7},
		{"between tokens", "a @b c @dee", 6, -1},
		{"inside a quoted token's space", `x @"a b" @c`, 5, 2},
		{"just past a closing quote", `x @"a b" @c`, 8, 2},
		{"a token after a quoted one", `x @"a b" @c`, 11, 9},
		{"an @ inside quotes is the quoted token's", `@"a @b"`, 6, 0},
		{"an @ right after a closing quote is no token", `@"a b"@c`, 8, -1},
		{"an open quote runs to the end", `fix @"my dir and more`, 20, 4},
		{"a mid-word @", "mail a@b", 8, -1},
		{"on a later line", "first\n@foo", 10, 6},
		{"a cursor past the end is clamped", "@foo", 99, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := atTokenUnder(c.value, c.cursor)
			switch {
			case c.start < 0 && ok:
				t.Fatalf("atTokenUnder(%q, %d) = %+v; want none", c.value, c.cursor, got)
			case c.start >= 0 && (!ok || got.start != c.start):
				t.Fatalf("atTokenUnder(%q, %d) = %+v, %v; want the token at %d", c.value, c.cursor, got, ok, c.start)
			}
		})
	}
}

// A written token reads back as the text it was written for, whole, and its
// inner offset is where that text ends — inside the closing quote of a quoted
// one.
func TestAWrittenTokenReadsBackAsItsText(t *testing.T) {
	cases := []struct {
		text   string
		quoted bool
	}{
		{"lumen", false},
		{"~/projects/lumen", false},
		{"", false},
		{`a"b`, false},
		{`a\b`, false},
		{`trailing\`, false},
		{"café/ü", false},
		{"日本語", false},
		{"🎉party", false},
		{"/tmp/my dir", true},
		{`"lead`, true},
		{`""`, true},
		{`a\"b c`, true},
		{`a b\`, true},
		{`\\ \\`, true},
		{`say "hi" there`, true},
		{"café au lait", true},
		{"日本語 ディレクトリ", true},
		{"emoji 🎉 dir", true},
		{"nbsp\u00a0dir", true},
		{"ends with a space ", true},
		{" starts with one", true},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			written, inner, ok := atTokenText(c.text)
			if !ok {
				t.Fatalf("atTokenText(%q) refused", c.text)
			}
			if got := strings.HasPrefix(written, `@"`); got != c.quoted {
				t.Fatalf("atTokenText(%q) = %q; quoted %v, want %v", c.text, written, got, c.quoted)
			}
			// In the middle of a line, as a composer would hold it.
			value := "see " + written + " and more"
			tok, ok := atTokenAt(value, 4)
			if !ok || tok.text != c.text || tok.end != 4+len(written) || tok.quoted != c.quoted || tok.closed != c.quoted {
				t.Fatalf("%q read back as %+v, %v; want text %q ending at %d", written, tok, ok, c.text, 4+len(written))
			}
			under, ok := atTokenUnder(value, 4+inner)
			if !ok || under.start != 4 || under.text != c.text {
				t.Fatalf("the cursor at the written text's end (%d in %q) is not inside the token: %+v, %v", 4+inner, value, under, ok)
			}
			if want := len(written) - map[bool]int{true: 1, false: 0}[c.quoted]; inner != want {
				t.Fatalf("atTokenText(%q) inner = %d; want %d", c.text, inner, want)
			}
		})
	}
}

func TestATokenCannotCarryControlsOrBrokenUTF8(t *testing.T) {
	for _, text := range []string{"a\nb", "a\tb", "a\x1bb", "a\x7fb", "a\u0085b", string([]byte{'a', 0xff})} {
		if got, _, ok := atTokenText(text); ok {
			t.Errorf("atTokenText(%q) = %q; want it refused", text, got)
		}
	}
}

// ------------------------------------------------------------- the popup

func TestThePopupIsUpWhileTheCursorIsInAToken(t *testing.T) {
	src := &listSource{id: "list", items: named("lumen", "luna", "roost")}
	p := newCompletePopup(src, atGrammar)
	env := completeEnv{Workspace: "/w", Shown: 1}

	if cmd := synced(&p, "@lu", env); cmd != nil || !p.visible() {
		t.Fatalf("@lu: visible %v, cmd %v; want up with no load", p.visible(), cmd != nil)
	}
	if got := popupNames(p); !slices.Equal(got, []string{"lumen", "luna"}) {
		t.Fatalf("@lu offers %v", got)
	}
	for _, c := range []struct {
		value  string
		cursor int
		up     bool
	}{
		{"@lu", 0, false},
		{"@lu ", 4, false},
		{"hi @r", 5, true},
		{"hi@r", 4, false},
		{"", 0, false},
	} {
		p.sync(c.value, c.cursor, env)
		if p.visible() != c.up {
			t.Errorf("%q with the cursor at %d: visible %v, want %v", c.value, c.cursor, p.visible(), c.up)
		}
	}
	// A closed popup takes no key.
	p.sync("plain text", 10, env)
	if _, handled := popKey(&p, keyOf(tea.KeyDown)); handled {
		t.Fatal("a closed popup took ↓")
	}
	if p.height(10) != 0 || p.view(Preset("dark"), 40, 10) != "" {
		t.Fatal("a closed popup draws something")
	}
}

func TestTheKeysChooseCompleteAndAccept(t *testing.T) {
	src := &listSource{id: "list", items: named("lumen", "luna", "lux")}
	env := completeEnv{Workspace: "/w", Shown: 1}
	open := func() completePopup {
		p := newCompletePopup(src, atGrammar)
		synced(&p, "@lu", env)
		return p
	}

	p := open()
	steps := []struct {
		key  tea.KeyMsg
		want int
	}{
		{keyOf(tea.KeyDown), 1}, {keyOf(tea.KeyCtrlN), 2}, {keyOf(tea.KeyDown), 0}, // wraps
		{keyOf(tea.KeyUp), 2}, {keyOf(tea.KeyCtrlP), 1}, {keyOf(tea.KeyUp), 0},
	}
	for i, s := range steps {
		if c, handled := popKey(&p, s.key); !handled || c.verb != 0 || p.sel != s.want {
			t.Fatalf("step %d (%v): handled %v, verb %v, sel %d; want sel %d", i, s.key, handled, c.verb, p.sel, s.want)
		}
	}

	for _, k := range []tea.KeyMsg{keyOf(tea.KeyTab), keyOf(tea.KeyEnter)} {
		p := open()
		popKey(&p, keyOf(tea.KeyDown))
		c, handled := popKey(&p, k)
		if !handled || c.verb != completeAccepted || c.item.Name != "luna" || c.value != "@luna " || c.cursor != len("@luna ") {
			t.Fatalf("%v: %+v, handled %v; want luna accepted as \"@luna \"", k, c, handled)
		}
	}

	// Alt+enter is the composer's newline, and a rune is the input's.
	p = open()
	for _, k := range []tea.KeyMsg{{Type: tea.KeyEnter, Alt: true}, {Type: tea.KeyRunes, Runes: []rune("x")}, keyOf(tea.KeyLeft), keyOf(tea.KeyShiftTab)} {
		if _, handled := popKey(&p, k); handled {
			t.Fatalf("the popup took %v", k)
		}
	}

	// With nothing to choose: tab is still the popup's and does nothing (it
	// never submits), the arrows are the popup's, enter is the owner's.
	p = newCompletePopup(src, atGrammar)
	synced(&p, "@zzz", env)
	if !p.visible() || len(p.ans.Items) != 0 {
		t.Fatalf("@zzz: visible %v, items %v", p.visible(), popupNames(p))
	}
	for _, k := range []tea.KeyType{tea.KeyTab, tea.KeyDown, tea.KeyUp, tea.KeyCtrlN, tea.KeyCtrlP} {
		if c, handled := popKey(&p, keyOf(k)); !handled || c.verb != 0 {
			t.Fatalf("%v with nothing to choose: handled %v, verb %v", k, handled, c.verb)
		}
	}
	if _, handled := popKey(&p, keyOf(tea.KeyEnter)); handled {
		t.Fatal("enter with nothing to accept was the popup's")
	}
}

func TestAnAcceptReplacesOnlyItsToken(t *testing.T) {
	src := &listSource{id: "list", items: []completeItem{
		{Name: "lumen", Value: "/p/lumen", Insert: "lumen"},
		{Name: "my dir", Value: "/p/my dir", Insert: "/p/my dir"},
		{Name: "quote", Value: "/p/q", Insert: `say "hi"`},
	}}
	env := completeEnv{Workspace: "/w"}
	cases := []struct {
		name       string
		value      string
		cursor     int
		want       string
		wantCursor int
	}{
		{"in the middle, taking the space after it", "fix @lu the bug", 7, "fix @lumen the bug", len("fix @lumen ")},
		{"one space taken, the next kept", "@lu  x", 3, "@lumen  x", len("@lumen ")},
		{"a newline after it kept", "@lu\nnext", 3, "@lumen \nnext", len("@lumen ")},
		{"the cursor mid-token", "@lu", 2, "@lumen ", len("@lumen ")},
		{"a path with a space quoted", "go @m", 5, `go @"/p/my dir" `, len(`go @"/p/my dir" `)},
		{"quotes escaped", "@q", 2, `@"say \"hi\"" `, len(`@"say \"hi\"" `)},
		{"an open quote replaced whole", `@"my`, 4, `@"/p/my dir" `, len(`@"/p/my dir" `)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newCompletePopup(src, atGrammar)
			p.sync(c.value, c.cursor, env)
			ch, handled := popKey(&p, keyOf(tea.KeyEnter))
			if !handled || ch.verb != completeAccepted || ch.value != c.want || ch.cursor != c.wantCursor {
				t.Fatalf("got %q at %d (handled %v, verb %v); want %q at %d", ch.value, ch.cursor, handled, ch.verb, c.want, c.wantCursor)
			}
			// The token written reads back as the candidate's text.
			if tok, ok := atTokenUnder(ch.value, ch.cursor-1); !ok || tok.text != ch.item.Insert {
				t.Fatalf("the accepted token reads back as %+v, %v; want %q", tok, ok, ch.item.Insert)
			}
			// The space an accept writes ends the token, so the popup closes.
			p.sync(ch.value, ch.cursor, env)
			if p.visible() {
				t.Fatalf("the popup is still up after accepting into %q", ch.value)
			}
		})
	}
}

func TestEscHidesThePopupUntilTheTokenChanges(t *testing.T) {
	src := &listSource{id: "list", items: named("lumen", "luna")}
	env := completeEnv{Workspace: "/w"}
	p := newCompletePopup(src, atGrammar)
	synced(&p, "x @lu", env)
	if c, handled := popKey(&p, keyOf(tea.KeyEsc)); !handled || c.verb != 0 || p.visible() {
		t.Fatalf("esc: handled %v, verb %v, visible %v", handled, c.verb, p.visible())
	}
	// The same token — synced again, the cursor moved inside it, or left and
	// come back to — stays hidden.
	for _, cur := range []int{5, 4, 0, 5} {
		p.sync("x @lu", cur, env)
		if p.visible() {
			t.Fatalf("the hidden token came back with the cursor at %d", cur)
		}
	}
	// Typed into: another token, and the popup is back.
	synced(&p, "x @lum", env)
	if !p.visible() || !slices.Equal(popupNames(p), []string{"lumen"}) {
		t.Fatalf("after typing: visible %v, %v", p.visible(), popupNames(p))
	}
	// Hidden again, then another token elsewhere opens it.
	popKey(&p, keyOf(tea.KeyEsc))
	synced(&p, "x @lum @l", env)
	if !p.visible() {
		t.Fatal("another token did not open the popup")
	}
}

func TestTabDescendsIntoADirectoryAndStaysOpen(t *testing.T) {
	src := &listSource{id: "list", items: []completeItem{
		{Name: "projects/", Value: "/h/projects", Insert: "~/projects", Openable: true},
		{Name: "my dir/", Value: "/h/my dir", Insert: "~/my dir", Openable: true},
		{Name: "src/", Value: "/h/src", Insert: "~/src/", Descend: "~/src/", Openable: true},
	}}
	env := completeEnv{Workspace: "/w"}

	p := newCompletePopup(src, atGrammar)
	p.sync("fix @ now", 5, env)
	c, handled := popKey(&p, keyOf(tea.KeyTab))
	if !handled || c.verb != completeDescended || c.value != "fix @~/projects/ now" || c.cursor != len("fix @~/projects/") {
		t.Fatalf("tab on projects/: %q at %d (verb %v)", c.value, c.cursor, c.verb)
	}
	p.sync(c.value, c.cursor, env)
	if !p.visible() || p.tok.text != "~/projects/" {
		t.Fatalf("after descending: visible %v, query %q", p.visible(), p.tok.text)
	}

	// Enter on the same row accepts it: its text and a space.
	p = newCompletePopup(src, atGrammar)
	p.sync("@", 1, env)
	if c, _ := popKey(&p, keyOf(tea.KeyEnter)); c.verb != completeAccepted || c.value != "@~/projects " {
		t.Fatalf("enter on projects/: %q (verb %v)", c.value, c.verb)
	}

	// A directory with a space descends inside its quotes: typing goes on in
	// the token.
	p = newCompletePopup(src, atGrammar)
	p.sync("@", 1, env)
	popKey(&p, keyOf(tea.KeyDown))
	c, _ = popKey(&p, keyOf(tea.KeyTab))
	if c.verb != completeDescended || c.value != `@"~/my dir/"` || c.cursor != len(`@"~/my dir/`) {
		t.Fatalf("tab on my dir/: %q at %d (verb %v)", c.value, c.cursor, c.verb)
	}
	typed := c.value[:c.cursor] + "x" + c.value[c.cursor:]
	p.sync(typed, c.cursor+1, env)
	if !p.visible() || p.tok.text != "~/my dir/x" {
		t.Fatalf("typing inside the quotes: visible %v, query %q", p.visible(), p.tok.text)
	}

	// Descend is written as the source gave it.
	p = newCompletePopup(src, atGrammar)
	p.sync("@", 1, env)
	popKey(&p, keyOf(tea.KeyUp))
	if c, _ := popKey(&p, keyOf(tea.KeyTab)); c.value != "@~/src/" {
		t.Fatalf("tab on src/: %q", c.value)
	}
}

func TestTheSelectionHoldsItsCandidateAcrossARefresh(t *testing.T) {
	src := &listSource{id: "list", items: named("lumen", "luna", "lux")}
	env := completeEnv{Workspace: "/w"}
	p := newCompletePopup(src, atGrammar)
	synced(&p, "@lu", env)
	popKey(&p, keyOf(tea.KeyDown))
	popKey(&p, keyOf(tea.KeyDown))
	if it, _ := p.selected(); it.Name != "lux" {
		t.Fatalf("selected %q", it.Name)
	}
	// The candidates change under the same query (a running count moved, a
	// session started): the selection stays on lux.
	src.items = append(named("lucid"), src.items...)
	synced(&p, "@lu", env)
	if it, _ := p.selected(); it.Name != "lux" || p.sel != 3 {
		t.Fatalf("after a refresh: selected %q at %d; want lux at 3", it.Name, p.sel)
	}
	// lux goes: the selection keeps its place.
	src.items = named("lucid", "lumen", "luna")
	synced(&p, "@lu", env)
	if it, _ := p.selected(); it.Name != "luna" {
		t.Fatalf("after lux went: selected %q; want luna, the last", it.Name)
	}
	// A new query starts from the top.
	synced(&p, "@lum", env)
	if p.sel != 0 {
		t.Fatalf("a new query kept the selection at %d", p.sel)
	}
}

func TestACandidateThatCannotBeWrittenIsNotOffered(t *testing.T) {
	src := &listSource{id: "list", items: []completeItem{
		{Name: "ok", Insert: "ok"},
		{Name: "newline", Insert: "new\nline"},
		{Name: "tab", Insert: "a\tb"},
		{Name: "bad descend", Insert: "fine", Openable: true, Descend: "bad\x1b/"},
		{Name: "open ok", Insert: "dir", Openable: true},
	}}
	p := newCompletePopup(src, atGrammar)
	synced(&p, "@", completeEnv{})
	if got := popupNames(p); !slices.Equal(got, []string{"ok", "open ok"}) {
		t.Fatalf("offered %v", got)
	}
}

// ------------------------------------------------------------------- view

func TestThePopupDrawsItsColumns(t *testing.T) {
	src := &listSource{id: "list", items: []completeItem{
		{Name: "lumen", Detail: "~/projects/lumen", Note: "here · 2 running", Tone: completeToneAccent, Value: "a", Insert: "lumen"},
		{Name: "roost", Detail: "~/projects/roost", Note: "1 running", Tone: completeToneAccent, Value: "b", Insert: "roost"},
		{Name: "craze", Detail: "~/src/craze", Note: "used 3h ago", Value: "c", Insert: "craze"},
	}}
	p := newCompletePopup(titled{src, "where should it run?"}, atGrammar)
	synced(&p, "@", completeEnv{})
	got := popupText(p, 60, 20)
	want := []string{
		strings.Repeat("─", 37) + " where should it run? ─",
		"❯ lumen  ~/projects/lumen  here · 2 running",
		"  roost  ~/projects/roost  1 running",
		"  craze  ~/src/craze       used 3h ago",
	}
	if !slices.Equal(trimmed(got), want) {
		t.Fatalf("drawn:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i, l := range got {
		if w := lipgloss.Width(l); w != 60 {
			t.Fatalf("line %d is %d cells wide, want 60: %q", i, w, l)
		}
	}
	if p.height(20) != 4 {
		t.Fatalf("height %d, want 4", p.height(20))
	}
	// The note's tone is its colour.
	th := Preset("dark")
	v := p.view(th, 60, 20)
	if !strings.Contains(v, styleFG(th.Accent).Render("here · 2 running")) || !strings.Contains(v, styleFG(th.Dim).Render("used 3h ago")) {
		t.Fatal("the notes are not drawn in their tones")
	}
	// Squeezed, the title goes first.
	if got := trimmed(popupText(p, 60, 3)); !slices.Equal(got, want[1:]) {
		t.Fatalf("in 3 rows:\n%s", strings.Join(got, "\n"))
	}
}

// titled is a source whose answers carry a title.
type titled struct {
	*listSource
	title string
}

func (s titled) complete(q completeQuery) completeAnswer {
	ans := s.listSource.complete(q)
	ans.Title = s.title
	return ans
}

func TestTheColumnsAreMeasuredOverEveryCandidate(t *testing.T) {
	items := []completeItem{
		{Name: "a", Detail: "d"},
		{Name: "日本語", Detail: "/x"},
		{Name: strings.Repeat("n", 40), Detail: strings.Repeat("p", 40), Note: "n"},
	}
	if n, d := completeColumns(100, items[:2]); n != 6 || d != 2 {
		t.Fatalf("short entries: name %d, detail %d; want 6 (日本語 is six cells) and 2", n, d)
	}
	// Capped by their shares of the row: the name at 24 % and 22 cells, the
	// detail at 36 %.
	if n, d := completeColumns(100, items); n != 22 || d != 36 {
		t.Fatalf("at 100: name %d, detail %d; want 22, 36", n, d)
	}
	if n, d := completeColumns(50, items); n != 12 || d != 18 {
		t.Fatalf("at 50: name %d, detail %d; want 12, 18", n, d)
	}
	// Names alone take the row.
	if n, d := completeColumns(50, []completeItem{{Name: strings.Repeat("n", 80)}}); n != 48 || d != 0 {
		t.Fatalf("names alone: name %d, detail %d; want 48, 0", n, d)
	}
}

func TestTheWindowShowsEightRowsAndCountsTheRest(t *testing.T) {
	src := &listSource{id: "list", items: numbered(20)}
	p := newCompletePopup(src, atGrammar)
	synced(&p, "@c", completeEnv{})

	check := func(label string, rows int, want []string) {
		t.Helper()
		if got := trimmed(popupText(p, 40, rows)); !slices.Equal(got, want) {
			t.Fatalf("%s:\n%s\nwant:\n%s", label, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if h := p.height(rows); h != len(want) {
			t.Fatalf("%s: height %d, want %d", label, h, len(want))
		}
	}
	rowsOf := func(sel, from, to int) []string {
		var out []string
		for i := from; i < to; i++ {
			mark := "  "
			if i == sel {
				mark = "❯ "
			}
			out = append(out, fmt.Sprintf("%sc%02d", mark, i))
		}
		return out
	}

	check("at the top", 20, append(rowsOf(0, 0, 8), "  ↓ 12 more"))
	for range 7 {
		popKey(&p, keyOf(tea.KeyDown))
	}
	check("the eighth row", 20, append(rowsOf(7, 0, 8), "  ↓ 12 more"))
	popKey(&p, keyOf(tea.KeyDown))
	check("scrolled one", 20, append(rowsOf(8, 1, 9), "  ↓ 11 more"))
	// Up from the top wraps to the last, where the count says what is above.
	for range 9 {
		popKey(&p, keyOf(tea.KeyUp))
	}
	check("wrapped to the last", 20, append(rowsOf(19, 12, 20), "  ↑ 12 more"))
	// Back up inside the window: it does not move.
	popKey(&p, keyOf(tea.KeyUp))
	check("up inside the window", 20, append(rowsOf(18, 12, 20), "  ↑ 12 more"))

	// Given fewer rows: the window narrows around the selection, the count
	// line kept; one row is the selection alone.
	check("in four rows", 4, append(rowsOf(18, 16, 19), "  ↓ 1 more"))
	check("in one row", 1, rowsOf(18, 18, 19))

	// Eight fit: no count line.
	src.items = numbered(8)
	synced(&p, "@c0", completeEnv{})
	check("eight", 20, rowsOf(0, 0, 8))
}

func TestAPopupWithNothingToOfferSaysWhy(t *testing.T) {
	src := &listSource{id: "list", items: named("lumen")}
	p := newCompletePopup(src, atGrammar)
	synced(&p, "@zzz", completeEnv{})
	if got := trimmed(popupText(p, 40, 10)); !slices.Equal(got, []string{"  nothing matches"}) {
		t.Fatalf("drawn %q", got)
	}
	if p.height(10) != 1 {
		t.Fatalf("height %d", p.height(10))
	}
}

// --------------------------------------------------------------- loads

func TestALoadRunsOnceAndServesEveryQueryThatNamesIt(t *testing.T) {
	src := newTreeSource()
	env := completeEnv{Workspace: "/w", Shown: 1}
	p := newCompletePopup(src, atGrammar)

	ch := runLoad(t, synced(&p, "@~/p", env))
	run := src.awaitRun(t)
	if run.key != "~" || !p.pending() {
		t.Fatalf("load %q, pending %v", run.key, p.pending())
	}
	if got := trimmed(popupText(p, 40, 10)); !slices.Equal(got, []string{strings.Repeat("─", 25) + " folders in ~ ─", "  searching…"}) {
		t.Fatalf("while listing:\n%s", strings.Join(got, "\n"))
	}
	// Typed on while it lists: the same load serves the new query, and is not
	// started again.
	if cmd := synced(&p, "@~/pr", env); cmd != nil {
		t.Fatal("a second load of ~ started")
	}
	close(run.release)
	msg := awaitLoaded(t, ch)
	if msg.query != "~/p" || msg.key != "~" || msg.source != "tree" || msg.workspace != "/w" || msg.shownGen != 1 || msg.gen != p.gen {
		t.Fatalf("the result's stamp: %+v", msg)
	}
	if cmd, ok := p.loaded(msg); !ok || cmd != nil {
		t.Fatalf("the awaited result: taken %v, further load %v", ok, cmd != nil)
	}
	if got := popupNames(p); !slices.Equal(got, []string{"projects/"}) || p.pending() {
		t.Fatalf("after the listing, @~/pr offers %v (pending %v)", got, p.pending())
	}
	// Kept for the popup's life: backing up needs no load.
	if cmd := synced(&p, "@~/", env); cmd != nil {
		t.Fatal("~ was listed again")
	}
	if got := popupNames(p); !slices.Equal(got, []string{"projects/", "src/", "my dir/"}) {
		t.Fatalf("@~/ offers %v", got)
	}
	// Tab on projects/ descends, naming another load.
	c, _ := popKey(&p, keyOf(tea.KeyTab))
	if c.value != "@~/projects/" {
		t.Fatalf("descended into %q", c.value)
	}
	ch = runLoad(t, p.sync(c.value, c.cursor, env))
	run = src.awaitRun(t)
	if run.key != "~/projects" {
		t.Fatalf("descending listed %q", run.key)
	}
	close(run.release)
	if _, ok := p.loaded(awaitLoaded(t, ch)); !ok {
		t.Fatal("the listing of ~/projects was dropped")
	}
	if got := popupNames(p); !slices.Equal(got, []string{"lumen/", "roost/"}) {
		t.Fatalf("@~/projects/ offers %v", got)
	}
	src.noRun(t)
}

func TestALoadThatFailsIsKeptAndSaysWhy(t *testing.T) {
	src := newTreeSource()
	env := completeEnv{Workspace: "/w"}
	p := newCompletePopup(src, atGrammar)
	ch := runLoad(t, synced(&p, "@~/nope/x", env))
	run := src.awaitRun(t)
	close(run.release)
	if _, ok := p.loaded(awaitLoaded(t, ch)); !ok {
		t.Fatal("the failed listing was dropped")
	}
	if got := trimmed(popupText(p, 40, 10)); !slices.Equal(got, []string{strings.Repeat("─", 20) + " folders in ~/nope ─", "  no directory ~/nope"}) {
		t.Fatalf("drawn:\n%s", strings.Join(got, "\n"))
	}
	th := Preset("dark")
	if !strings.Contains(p.view(th, 40, 10), styleFG(th.Err).Render("  no directory ~/nope")) {
		t.Fatal("the source's error note is not drawn as an error")
	}
	if cmd := synced(&p, "@~/nope/xy", env); cmd != nil {
		t.Fatal("a failed listing was run again in the same popup")
	}
}

// Every way a load stops being awaited: its work is cancelled then, and its
// result — whenever it arrives — is dropped and changes nothing on screen.
func TestALoadNoLongerAwaitedIsCancelledAndItsResultDropped(t *testing.T) {
	env := completeEnv{Workspace: "/w", Shown: 1}
	cases := []struct {
		name string
		// leave moves the popup on from the load of ~ it is waiting for.
		leave func(t *testing.T, p *completePopup, src *treeSource)
		// open says the popup is still up afterwards.
		open bool
	}{
		{"the popup closed", func(t *testing.T, p *completePopup, _ *treeSource) { p.close() }, false},
		{"esc hid it", func(t *testing.T, p *completePopup, _ *treeSource) {
			if _, handled := popKey(p, keyOf(tea.KeyEsc)); !handled {
				t.Fatal("esc was not the popup's")
			}
		}, false},
		{"the token went", func(t *testing.T, p *completePopup, _ *treeSource) { synced(p, "@~/p done", env) }, false},
		{"the query moved to a name", func(t *testing.T, p *completePopup, _ *treeSource) { synced(p, "@lu", env) }, true},
		{"the query moved to another load", func(t *testing.T, p *completePopup, src *treeSource) {
			if cmd := synced(p, "@~/projects/", env); cmd == nil {
				t.Fatal("no load of ~/projects")
			}
		}, true},
		{"the workspace changed", func(t *testing.T, p *completePopup, src *treeSource) {
			if cmd := synced(p, "@~/p", completeEnv{Workspace: "/other", Shown: 1}); cmd == nil {
				t.Fatal("the new workspace's load did not start")
			}
		}, true},
		{"the session shown changed", func(t *testing.T, p *completePopup, src *treeSource) {
			if cmd := synced(p, "@~/p", completeEnv{Workspace: "/w", Shown: 2}); cmd == nil {
				t.Fatal("the new session's load did not start")
			}
		}, true},
		{"the token moved", func(t *testing.T, p *completePopup, src *treeSource) {
			if cmd := synced(p, "x @~/p", env); cmd == nil {
				t.Fatal("the moved token's load did not start")
			}
		}, true},
	}
	for _, c := range cases {
		// Both orders: the work sees its cancel, and the work finished before
		// the cancel reached it (its result arrives anyway).
		for _, finished := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, finished %v", c.name, finished), func(t *testing.T) {
				src := newTreeSource()
				p := newCompletePopup(src, atGrammar)
				ch := runLoad(t, synced(&p, "@~/p", env))
				run := src.awaitRun(t)
				var msg completeLoadedMsg
				if finished {
					close(run.release)
					msg = awaitLoaded(t, ch)
				}
				c.leave(t, &p, src)
				if !finished {
					awaitCancel(t, run)
					msg = awaitLoaded(t, ch)
				}
				if p.visible() != c.open {
					t.Fatalf("visible %v, want %v", p.visible(), c.open)
				}
				before := p
				if cmd, ok := p.loaded(msg); ok || cmd != nil {
					t.Fatalf("a load no longer awaited was taken (%+v)", msg)
				}
				if !slices.Equal(popupNames(p), popupNames(before)) || p.pending() != before.pending() ||
					!slices.Equal(popupText(p, 40, 10), popupText(before, 40, 10)) {
					t.Fatal("a dropped result changed the popup")
				}
			})
		}
	}
}

// A result is its own popup's alone: not another popup's with the same
// source, not a popup made afresh on the same token, not one completing from
// another source.
func TestALoadsResultIsItsOwnPopupsAlone(t *testing.T) {
	env := completeEnv{Workspace: "/w", Shown: 1}

	t.Run("another popup", func(t *testing.T) {
		src := newTreeSource()
		a, b := newCompletePopup(src, atGrammar), newCompletePopup(src, atGrammar)
		chA := runLoad(t, synced(&a, "@~/p", env))
		runA := src.awaitRun(t)
		chB := runLoad(t, synced(&b, "@~/p", env))
		runB := src.awaitRun(t)
		close(runA.release)
		close(runB.release)
		msgA, msgB := awaitLoaded(t, chA), awaitLoaded(t, chB)
		if _, ok := b.loaded(msgA); ok {
			t.Fatal("b took a's result")
		}
		if _, ok := b.loaded(msgB); !ok {
			t.Fatal("b dropped its own")
		}
		if _, ok := a.loaded(msgA); !ok {
			t.Fatal("a dropped its own")
		}
	})

	t.Run("a popup made afresh", func(t *testing.T) {
		src := newTreeSource()
		p := newCompletePopup(src, atGrammar)
		ch := runLoad(t, synced(&p, "@~/p", env))
		run := src.awaitRun(t)
		// The owner's state is rebuilt — the list reopened — without a close:
		// the old work goes on, and the new popup opens on the same token.
		p = newCompletePopup(src, atGrammar)
		chNew := runLoad(t, synced(&p, "@~/p", env))
		runNew := src.awaitRun(t)
		close(run.release)
		if _, ok := p.loaded(awaitLoaded(t, ch)); ok {
			t.Fatal("the new popup took the old one's result")
		}
		close(runNew.release)
		if _, ok := p.loaded(awaitLoaded(t, chNew)); !ok {
			t.Fatal("the new popup dropped its own result")
		}
	})

	t.Run("another source", func(t *testing.T) {
		src := newTreeSource()
		p := newCompletePopup(src, atGrammar)
		ch := runLoad(t, synced(&p, "@~/p", env))
		close(src.awaitRun(t).release)
		msg := awaitLoaded(t, ch)
		for _, bad := range []func(m *completeLoadedMsg){
			func(m *completeLoadedMsg) { m.source = "other" },
			func(m *completeLoadedMsg) { m.workspace = "/other" },
			func(m *completeLoadedMsg) { m.shownGen = 2 },
			func(m *completeLoadedMsg) { m.gen++ },
			func(m *completeLoadedMsg) { m.seq++ },
			func(m *completeLoadedMsg) { m.key = "~/projects" },
		} {
			m := msg
			bad(&m)
			if _, ok := p.loaded(m); ok {
				t.Fatalf("a result stamped %+v was taken", m)
			}
		}
		if _, ok := p.loaded(msg); !ok {
			t.Fatal("the popup dropped its own result")
		}
		// Taken once: the same result again is no longer awaited.
		if _, ok := p.loaded(msg); ok {
			t.Fatal("a result was taken twice")
		}
	})
}

// A load's result is the terminal's work for the session shown (plan 030
// X121): once a switch has shown another session the command gate drops it
// before it is held or applied, and a switch takes it out of the held queue.
func TestASwitchLeavesALoadsResultBehind(t *testing.T) {
	var m Model
	m.shownGen = 1
	msg := completeLoadedMsg{source: "tree", shownGen: 1}
	if m.leftBehind(msg) {
		t.Fatal("a result for the session shown is left behind")
	}
	if m.leftBehind(completeLoadedMsg{}) {
		t.Fatal("a hand-built result (no stamp) is left behind")
	}
	m.shownGen = 2
	if !m.leftBehind(msg) {
		t.Fatal("a result for a session no longer shown is not left behind")
	}
	handled := false
	m.gated(msg, func(m Model, _ tea.Msg) (tea.Model, tea.Cmd) {
		handled = true
		return m, nil
	})
	if handled {
		t.Fatal("the gate applied a result a switch left behind")
	}
	key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}
	m.held = []heldMsg{{msg: msg, bytes: 1}, {msg: key, bytes: 1}}
	m.heldBytes = 2
	m.dropStaleHeld()
	if _, ok := m.held[0].msg.(tea.KeyMsg); len(m.held) != 1 || !ok || m.heldBytes != 1 {
		t.Fatalf("held after a switch: %+v", m.held)
	}
}

// Every opening and every load has a number no other ever had, so no result
// can name a popup it was not asked for.
func TestEveryOpeningAndLoadIsNumberedOnce(t *testing.T) {
	src := newTreeSource()
	seen := map[uint64]bool{}
	note := func(n uint64) {
		t.Helper()
		if seen[n] {
			t.Fatalf("%d issued twice", n)
		}
		seen[n] = true
	}
	for range 3 {
		p := newCompletePopup(src, atGrammar)
		ch := runLoad(t, synced(&p, "@~/p", completeEnv{}))
		note(p.gen)
		note(p.wait.seq)
		close(src.awaitRun(t).release)
		msg := awaitLoaded(t, ch)
		if msg.gen != p.gen || msg.seq != p.wait.seq {
			t.Fatalf("the result is stamped %d/%d, the popup awaits %d/%d", msg.gen, msg.seq, p.gen, p.wait.seq)
		}
		p.close()
	}
}

// A load's context is cancelled once its result has been taken, so nothing
// of it outlives its answer.
func TestATakenLoadReleasesItsContext(t *testing.T) {
	var got context.Context
	src := &funcSource{id: "f", answer: func(q completeQuery) completeAnswer {
		if l, ok := q.Loaded("k"); ok {
			return completeAnswer{Items: l.Items}
		}
		return completeAnswer{Load: &completeLoad{Key: "k", Run: func(ctx context.Context) completeLoaded {
			got = ctx
			return completeLoaded{Items: named("x")}
		}}}
	}}
	p := newCompletePopup(src, atGrammar)
	cmd := synced(&p, "@", completeEnv{})
	msg := cmd().(completeLoadedMsg)
	if got.Err() != nil {
		t.Fatal("the load's context was cancelled while it ran")
	}
	if _, ok := p.loaded(msg); !ok {
		t.Fatal("dropped")
	}
	if !errors.Is(got.Err(), context.Canceled) {
		t.Fatalf("after its result was taken the load's context is %v", got.Err())
	}
	if got := popupNames(p); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("offered %v", got)
	}
}

type funcSource struct {
	id     string
	answer func(completeQuery) completeAnswer
}

func (s *funcSource) completeID() string                      { return s.id }
func (s *funcSource) complete(q completeQuery) completeAnswer { return s.answer(q) }
