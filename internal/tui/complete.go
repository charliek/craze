package tui

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The completion component (plan 030 §3.15): one popup, reused by every input
// that completes a token — the session list's input (PR 3: `@` directories,
// `/provider` and `/model`) and the session composer's `@` (PR 4). It is not
// slash.go's menu, whose tokenizer and band belong to the composer's own
// commands and are not touched (§3.15): that menu completes a catalog the model
// already holds, where this one completes from sources that may have to go to
// the disk for their answer.
//
// It is a value its owner holds and drives, and it reads nothing of the model:
//
//   - The owner syncs it after every change to its input (sync): the popup
//     finds the token under the cursor by its grammar — the `@` token's is
//     atGrammar, below — and asks its source for that token's candidates.
//   - A source answers at once, or names a load — work off the Update: listing
//     a directory, walking a workspace — which the popup runs as a tea.Cmd, at
//     most one at a time and once per key for the popup's life, and whose
//     result comes back as a completeLoadedMsg the owner hands to loaded. The
//     result is stamped with the source, the workspace, the popup's opening,
//     the session shown and the load it answers (and the query it was asked
//     for); one the popup no longer awaits is dropped, and the work behind it
//     was cancelled the moment it stopped being awaited — the popup closed or
//     was hidden, its token went, the workspace or the session shown changed,
//     or the query moved on to another load.
//   - Its keys (key): ↑/↓ and ctrl+p/ctrl+n choose, tab completes (on a
//     directory: descends, and the popup stays open), enter accepts, esc hides
//     it until the token changes; tab never submits. What a key chose comes
//     back as the input's next value and cursor for the owner to apply
//     (completeChoice), with the item itself for an owner that acts on the
//     choice rather than writing it.
//   - It draws (view) up to eight rows, and `↓ N more` under them when there
//     are more, as a block the owner places above the input it serves.
//
// The session list's input holds one for its leading `@` token (C14:
// sessions_input.go, its source at_dirs.go); its `/` commands take one in
// C16, and the composer in PR 4.

// completeMaxRows is the most candidate rows the popup draws: grok-build's
// window, which the slash menu already copied (slashMaxRows), and every
// reference tool's but opencode's. The list itself is uncapped: the window
// follows the selection, and `↓ N more` counts what it leaves out.
const completeMaxRows = 8

// The popup's columns: the gutter that marks the selected row, the name, the
// detail and the note, two cells apart. The name and the detail are measured
// over every candidate — not the eight on screen — so the columns do not
// shuffle sideways as the selection scrolls (slashNameWidth's rule); each is
// capped by a share of the row, the mockup's: the name at 24 % and never
// wider than 22 cells, the detail at 36 %, the note the rest.
const (
	completeGutterCols  = 2
	completeGapCols     = 2
	completeNameShare   = 24
	completeNameCap     = 22
	completeDetailShare = 36
)

// The popup's own words. A source may say more for itself (completeAnswer's
// Note): the `@` directory source says which directory is missing.
const (
	completeEmptyNote   = "nothing matches"
	completePendingNote = "searching…"
)

// completeSeq numbers every opening of every popup, and every load, for the
// process. A popup is a value its owner may drop and make afresh — the list's
// state is rebuilt each time the list opens, a switch rebuilds a session's
// (withSession) — and a count of its own would start again at 1, so an old
// load's answer could name the new popup's opening and be taken. One counter
// never issues a number twice.
var completeSeq atomic.Uint64

// ----------------------------------------------------------- the @ token

// atToken is an `@` token (plan 030 §3.15's grammar, owner decision 7): an
// `@` that starts a token — at the start of the input or after whitespace —
// and then either a run of non-whitespace, or `@"…"` for text with spaces in
// it, where `\"` is a quote and `\\` a backslash.
//
// [start, end) are byte offsets into the input, the `@` included, on rune
// boundaries. text is what the token names, unquoted and unescaped — the
// query a source is asked, and what a binding compares. quoted says it is
// written `@"…"`; closed that its closing quote is there (an open quote runs
// to the end of its line: the user is still typing it).
type atToken struct {
	start, end int
	text       string
	quoted     bool
	closed     bool
}

// atTokenAt reads the `@` token that starts at byte offset start of value,
// and false when none starts there: no `@` there, or one inside a word —
// `user@example.com` never opens a popup (grok-build, codex and opencode all
// refuse a mid-word `@`). Whitespace is unicode.IsSpace, as slashToken's is:
// a newline ends a token, and a non-breaking space before the `@` does not
// silence it.
//
// Inside quotes a backslash escapes a quote or a backslash, and any other
// rune after one is taken as written, backslash and all — the writer
// (atTokenText) escapes every backslash, so this leniency only ever reads
// what a person typed by hand. A quoted token never spans a line: a newline
// before the closing quote ends it there, unclosed.
func atTokenAt(value string, start int) (atToken, bool) {
	if start < 0 || start >= len(value) || value[start] != '@' {
		return atToken{}, false
	}
	if start > 0 {
		if r, _ := utf8.DecodeLastRuneInString(value[:start]); !unicode.IsSpace(r) {
			return atToken{}, false
		}
	}
	i := start + 1
	if i < len(value) && value[i] == '"' {
		var b strings.Builder
		for i++; i < len(value); {
			r, size := utf8.DecodeRuneInString(value[i:])
			switch {
			case r == '"':
				return atToken{start: start, end: i + size, text: b.String(), quoted: true, closed: true}, true
			case r == '\n':
				return atToken{start: start, end: i, text: b.String(), quoted: true}, true
			case r == '\\' && i+1 < len(value) && (value[i+1] == '"' || value[i+1] == '\\'):
				b.WriteByte(value[i+1])
				i += 2
				continue
			}
			b.WriteString(value[i : i+size])
			i += size
		}
		return atToken{start: start, end: len(value), text: b.String(), quoted: true}, true
	}
	end := i
	for end < len(value) {
		r, size := utf8.DecodeRuneInString(value[end:])
		if unicode.IsSpace(r) {
			break
		}
		end += size
	}
	return atToken{start: start, end: end, text: value[i:end]}, true
}

// atTokenUnder is the `@` token the cursor is inside, where inside means
// start < cursor <= end: after the `@` and up to just past the token's last
// rune, so the popup is up while the user types into the token and the space
// that ends it closes it. The cursor on the `@` itself is not inside — typing
// there writes before the token, not into it. (slashToken counts that place
// too; its menu has no binding to lose.)
//
// value is read from its start, token by token, because a quoted token has
// spaces in it and so cannot be found by walking back from the cursor to the
// nearest one.
func atTokenUnder(value string, cursor int) (atToken, bool) {
	cursor = min(max(cursor, 0), len(value))
	for i := 0; i < len(value) && i < cursor; {
		if tok, ok := atTokenAt(value, i); ok {
			if cursor > tok.start && cursor <= tok.end {
				return tok, true
			}
			// A token is at least its `@`, so this always moves on.
			i = tok.end
			continue
		}
		_, size := utf8.DecodeRuneInString(value[i:])
		i += size
	}
	return atToken{}, false
}

// atTokenText writes text as an `@` token — `@text`, or `@"text"` with its
// quotes and backslashes escaped when text has whitespace in it or begins with
// a quote, which unquoted would read as an open quote — and inner is the
// offset in the written token just past text's last rune: where a cursor that
// goes on typing the token's text belongs (before a closing quote). This is
// codex's rule — quote only what needs it — so a plain name stays the plain
// `@name` every provider reads.
//
// false for text a token cannot carry: a control character — a newline would
// end it, and bubbles' sanitiser rewrites tabs and drops the rest, so the
// input would hold another text than the one written, with the cursor offsets
// wrong (slashNameOK's reason) — or invalid UTF-8, which the sanitiser drops
// too.
func atTokenText(text string) (tok string, inner int, ok bool) {
	for _, r := range text {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return "", 0, false
		}
	}
	if !atNeedsQuotes(text) {
		return "@" + text, 1 + len(text), true
	}
	escaped := strings.ReplaceAll(text, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `@"` + escaped + `"`, 2 + len(escaped), true
}

// atNeedsQuotes says text cannot be written as a bare `@text`: whitespace
// would end the token early, and a leading quote would open a quoted one.
func atNeedsQuotes(text string) bool {
	return strings.HasPrefix(text, `"`) || strings.IndexFunc(text, unicode.IsSpace) >= 0
}

// ---------------------------------------------------------- the grammar

// completeToken is the token a popup completes, as its grammar found it:
// [start, end) in the input, and text, the query its source is asked.
type completeToken struct {
	start, end int
	text       string
}

// completeGrammar is how a popup finds its token and writes one back. under
// is the token the cursor is inside; write is text written as a token, with
// the offset inside it where the text ends (atTokenText's inner), false when
// text cannot be written. A popup's owner chooses the grammar: the `@` tokens
// here, and C16's `/` commands on the list's input their own.
type completeGrammar struct {
	under func(value string, cursor int) (completeToken, bool)
	write func(text string) (tok string, inner int, ok bool)
}

// atGrammar is the `@` token's grammar (atTokenUnder, atTokenText).
var atGrammar = completeGrammar{
	under: func(value string, cursor int) (completeToken, bool) {
		tok, ok := atTokenUnder(value, cursor)
		return completeToken{start: tok.start, end: tok.end, text: tok.text}, ok
	},
	write: atTokenText,
}

// ------------------------------------------------------------- sources

// completeTone colours a candidate's note: dim, as most are; the accent, for
// what is live (`here · 2 running`); the provider's colour, for a provider.
type completeTone int

const (
	completeToneDim completeTone = iota
	completeToneAccent
	completeToneProvider
)

// completeItem is one candidate.
type completeItem struct {
	// Name, Detail and Note are the row's three columns — what the candidate
	// is called (a directory's name, a command), where it is (its path), and
	// what is worth knowing about it (`here · 2 running`, `used 3h ago`) —
	// each one line of display text, sanitised by the source (sanitizeLine).
	Name, Detail, Note string
	Tone               completeTone
	// Value is what the candidate stands for — the absolute directory a pick
	// binds, a provider's id — for the owner that acts on a choice. It is the
	// candidate's identity too: the selection holds on to it while the
	// candidates are refreshed under an unchanged query.
	Value string
	// Insert is the token text an accept writes, as the user would type it
	// between the sigil and the space the accept adds: the grammar quotes it
	// when it must (`@"my dir" `).
	Insert string
	// Openable says the candidate is a directory tab descends into: tab
	// writes Descend (Insert and a "/" when empty), with no space after it,
	// and the popup stays open on what is inside.
	Openable bool
	Descend  string
}

// id is the candidate's identity: its Value, else what it writes, else its
// name.
func (it completeItem) id() string {
	switch {
	case it.Value != "":
		return it.Value
	case it.Insert != "":
		return "\x00" + it.Insert
	}
	return "\x01" + it.Name
}

// descendText is what tab writes on an openable candidate: Descend, or Insert
// ending in one "/".
func (it completeItem) descendText() string {
	if it.Descend != "" {
		return it.Descend
	}
	if strings.HasSuffix(it.Insert, "/") {
		return it.Insert
	}
	return it.Insert + "/"
}

// completeSource is where a popup's candidates come from: the list's `@`
// directories (C14), its `/` commands (C16), the composer's files (PR 4).
type completeSource interface {
	// completeID names the source. A load's result is stamped with it, so a
	// result can never be taken by a popup completing from another source.
	completeID() string
	// complete answers a query. It runs in the Update, on every change to the
	// token, so it is quick: anything slow is a load it names in its answer,
	// and it answers again — at once — when that load has come back.
	complete(q completeQuery) completeAnswer
}

// completeQuery is what a popup asks its source.
type completeQuery struct {
	// Text is the token's text, unquoted: what the user typed after the sigil.
	Text string
	// Workspace is the directory the popup completes in (completeEnv).
	Workspace string
	// loads is what this popup's loads have brought back so far, by key.
	loads map[string]completeLoaded
}

// Loaded is what the load keyed key brought back, if it has come back in
// this popup's life.
func (q completeQuery) Loaded(key string) (completeLoaded, bool) {
	l, ok := q.loads[key]
	return l, ok
}

// completeAnswer is a source's answer.
type completeAnswer struct {
	// Items are the candidates, in the order they are drawn.
	Items []completeItem
	// Title, when set, is drawn in a rule above the candidates (`where
	// should it run?`, `folders in ~/projects`).
	Title string
	// Note is drawn in place of the candidates when there are none — why
	// there are none, as the source puts it (`no directory ~/x`); NoteErr
	// draws it as an error. With no note, the popup says it is still
	// searching while a load is out, and `nothing matches` after.
	Note    string
	NoteErr bool
	// Load is work the answer waits for, keyed: the popup runs it unless a
	// load of that key has already come back in its life (Loaded) or is
	// already running.
	Load *completeLoad
}

// completeLoad is a source's work off the Update. Run gets a context the
// popup cancels once the load is no longer awaited, and honours it: a load
// that has stopped being awaited is thrown away when it answers anyway.
type completeLoad struct {
	// Key is what the load depends on — the directory it lists, the workspace
	// it walks — and nothing else: a result is kept for the popup's life
	// under it, and serves every later query that names the same key (a
	// directory listed for `~/pro` serves `~/proj`).
	Key string
	Run func(ctx context.Context) completeLoaded
}

// completeLoaded is what a load brought back: its candidates, which the
// source then filters and orders for each query, or why there are none.
type completeLoaded struct {
	Items []completeItem
	Err   error
}

// completeLoadedMsg is a load's result on its way to the popup that ran it,
// stamped with everything that says whether that popup still awaits it
// (loaded): the source, the workspace, the popup's opening (gen), the session
// shown when it was asked for (shownGen), the load's key and number (seq),
// and the query it was asked for, which a later query naming the same key is
// served by too.
//
// It is the terminal's own work for the session shown (plan 030 X121), so it
// carries the shown-session generation as a paste does: the command gate
// drops it once a switch has left that session, before it is held or applied,
// and a switch takes it out of the held queue (leftBehind, dropStaleHeld).
type completeLoadedMsg struct {
	source    string
	workspace string
	gen       uint64
	shownGen  uint64
	query     string
	key       string
	seq       uint64
	res       completeLoaded
}

func (m completeLoadedMsg) shownUnder() uint64 { return m.shownGen }

// ---------------------------------------------------------------- the popup

// completeEnv is where the popup completes, as its owner says at every sync:
// the workspace its source resolves against (the session's; for the list's
// input, the session it was opened from) and the session shown
// (Model.shownGen). A popup whose environment changes is closed and opened
// again: its loads were for the old one.
type completeEnv struct {
	Workspace string
	Shown     uint64
}

// completeWait is the load the popup awaits: its key and number, and the
// cancel of the context its work runs under.
type completeWait struct {
	key    string
	seq    uint64
	cancel context.CancelFunc
}

// completeLoadSet is every popup load still awaited, by its number, with the
// cancel of the context its work runs under: shared by every copy of the model
// (Model.completeLoads), as the roster set is, so a quit the popup never saw —
// a signal, a program error — cancels what it left running (finishRun; sol
// r28-c14 2). A popup adds a load as it starts it and takes it out as it stops
// awaiting it (cancelWait). Nil — a popup a test built on its own — holds
// nothing, and every method allows it.
type completeLoadSet struct {
	mu   sync.Mutex
	open map[uint64]context.CancelFunc
}

func (s *completeLoadSet) add(seq uint64, cancel context.CancelFunc) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil {
		s.open = map[uint64]context.CancelFunc{}
	}
	s.open[seq] = cancel
}

func (s *completeLoadSet) done(seq uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.open, seq)
}

// cancelAll cancels every load still awaited: finishRun's, on every exit path.
func (s *completeLoadSet) cancelAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	open := s.open
	s.open = nil
	s.mu.Unlock()
	for _, cancel := range open {
		cancel()
	}
}

// completePopup is one popup: the token it is open on, what its source last
// answered, the selection and the window, and its loads.
type completePopup struct {
	src     completeSource
	grammar completeGrammar
	// loadSet is the model's set of awaited loads (Model.completeLoads) this
	// popup's are recorded in, kept across its openings; nil records nothing.
	loadSet *completeLoadSet

	// open says the popup is up, gen names this opening (completeSeq), env
	// is where it opened.
	open bool
	gen  uint64
	env  completeEnv
	// value and cursor are the input as the last sync saw it; tok is the
	// token under the cursor there, and tokKey that token's identity — where it
	// starts and what it holds — which esc records in hideKey: the popup
	// stays hidden while the token under the cursor is that one, and comes
	// back the moment it is another (slashTokenKey's rule). hideEnv is the
	// environment the hide was made in: it is that environment's alone, so a
	// token of the same text at the same place in another workspace or
	// another session shown is another token (sol r26-c13 1).
	value   string
	cursor  int
	tok     completeToken
	tokKey  string
	hideKey string
	hideEnv completeEnv

	// ans is the source's last answer, its candidates cut to what can be
	// written. sel is the highlighted candidate, selID its identity, top the
	// first of the completeMaxRows the window shows.
	ans   completeAnswer
	sel   int
	top   int
	selID string
	// loads is what this opening's loads brought back, by key; wait is the
	// one load awaited, while waiting.
	loads   map[string]completeLoaded
	wait    completeWait
	waiting bool
}

// newCompletePopup is a closed popup completing from src by g.
func newCompletePopup(src completeSource, g completeGrammar) completePopup {
	return completePopup{src: src, grammar: g}
}

// trackIn records the popup's loads in set (Model.completeLoads) from now on.
func (p *completePopup) trackIn(set *completeLoadSet) { p.loadSet = set }

// setSource hands the popup its source as it stands now, for an owner whose
// candidates move under the popup — the session list's `@` directories
// follow the rows it lists (at_dirs.go) — before it syncs the popup or hands
// it a load's result. It is the same source, answering to the same
// completeID: a load's result is stamped with that id, and one from another
// source is never taken.
func (p *completePopup) setSource(src completeSource) { p.src = src }

// visible says the popup is up: on screen, and the keyboard's while it is.
// An open popup always draws something — its candidates, or a note saying
// why there are none.
func (p completePopup) visible() bool { return p.open }

// sync follows the input: the token under the cursor opens the popup, or
// keeps it open and asks the source again; no token, or the token esc hid,
// closes it. The command it returns is the load the answer needs, if one has
// to start — the owner batches it with its own.
func (p *completePopup) sync(value string, cursor int, env completeEnv) tea.Cmd {
	tok, ok := p.grammar.under(value, cursor)
	if !ok {
		if p.hideKey != "" && !p.hiddenIn(value) {
			// The token esc hid is gone from the input — deleted, not only
			// left by the cursor: the dismissal was that token's, and one
			// typed or pasted in its place is another (sol r28-c14 1).
			p.hideKey, p.hideEnv = "", completeEnv{}
		}
		p.close()
		return nil
	}
	key := strconv.Itoa(tok.start) + ":" + value[tok.start:tok.end]
	if p.hideKey != "" && env != p.hideEnv {
		// The hide was made in another workspace or for another session
		// shown: whatever token this is, it is not the one esc dismissed.
		p.hideKey, p.hideEnv = "", completeEnv{}
	}
	if key == p.hideKey {
		p.close()
		return nil
	}
	// Another token: the hide was for the one before it.
	p.hideKey, p.hideEnv = "", completeEnv{}
	if p.open && (tok.start != p.tok.start || env != p.env) {
		// Another token, another workspace or another session shown: the
		// loads and whatever is still running were for what is gone.
		p.close()
	}
	fresh := !p.open || tok.text != p.tok.text
	if !p.open {
		p.open, p.gen, p.env = true, completeSeq.Add(1), env
	}
	p.value, p.cursor, p.tok, p.tokKey = value, cursor, tok, key
	return p.ask(fresh)
}

// hiddenIn says the token esc hid is still in value, where it was: the same
// text at the same place, and still a whole token there.
func (p completePopup) hiddenIn(value string) bool {
	i := strings.IndexByte(p.hideKey, ':')
	if i < 0 {
		return false
	}
	start, err := strconv.Atoi(p.hideKey[:i])
	text := p.hideKey[i+1:]
	end := start + len(text)
	if err != nil || start < 0 || end > len(value) || value[start:end] != text {
		return false
	}
	tok, ok := p.grammar.under(value, end)
	return ok && tok.start == start && tok.end == end
}

// ask puts the token's query to the source and installs the answer — fresh
// says the query changed, so the selection starts again from the top — and
// starts the load the answer names, unless it is the one already running or
// one already back. Any other load running is no longer awaited, and is
// cancelled.
func (p *completePopup) ask(fresh bool) tea.Cmd {
	ans := p.src.complete(completeQuery{Text: p.tok.text, Workspace: p.env.Workspace, loads: p.loads})
	p.install(ans, fresh)
	l := ans.Load
	if l == nil {
		p.cancelWait()
		return nil
	}
	if _, back := p.loads[l.Key]; back {
		// The source named a load it already has the result of: nothing to
		// run, and nothing else is awaited.
		p.cancelWait()
		return nil
	}
	if p.waiting && p.wait.key == l.Key {
		return nil
	}
	return p.startLoad(*l)
}

// startLoad runs l off the Update, awaited from now on under a number of its
// own; a load it replaces is cancelled.
func (p *completePopup) startLoad(l completeLoad) tea.Cmd {
	p.cancelWait()
	ctx, cancel := context.WithCancel(context.Background())
	seq := completeSeq.Add(1)
	p.wait, p.waiting = completeWait{key: l.Key, seq: seq, cancel: cancel}, true
	p.loadSet.add(seq, cancel)
	stamp := completeLoadedMsg{
		source: p.src.completeID(), workspace: p.env.Workspace, gen: p.gen, shownGen: p.env.Shown,
		query: p.tok.text, key: l.Key, seq: seq,
	}
	run := l.Run
	return func() tea.Msg {
		msg := stamp
		msg.res = run(ctx)
		return msg
	}
}

// cancelWait stops awaiting the load that is running, and cancels its work.
func (p *completePopup) cancelWait() {
	if p.waiting && p.wait.cancel != nil {
		p.wait.cancel()
	}
	if p.waiting {
		p.loadSet.done(p.wait.seq)
	}
	p.wait, p.waiting = completeWait{}, false
}

// loaded takes a load's result: kept for the popup's life under its key, and
// the source asked again with it, if it is the load the popup awaits — this
// opening's, from this source, in this workspace, for this session shown,
// with the number it was started under. Anything else is dropped (false): an
// answer from before a close or a hide, for a query that has moved on to
// another load, from another popup. The command is a further load the new
// answer needs.
func (p *completePopup) loaded(msg completeLoadedMsg) (tea.Cmd, bool) {
	if !p.open || !p.waiting || msg.gen != p.gen || msg.seq != p.wait.seq || msg.key != p.wait.key ||
		msg.source != p.src.completeID() || msg.workspace != p.env.Workspace || msg.shownGen != p.env.Shown {
		return nil, false
	}
	p.cancelWait()
	// A copy, not the map a copy of the model made before this one holds.
	loads := maps.Clone(p.loads)
	if loads == nil {
		loads = map[string]completeLoaded{}
	}
	loads[msg.key] = msg.res
	p.loads = loads
	return p.ask(false), true
}

// close takes the popup down: whatever it was waiting for is cancelled, and
// what it held is dropped with it. The next opening is a new one. The hide
// esc recorded is not the opening's, and stays — with the environment it was
// made in, outside which it hides nothing (sync).
func (p *completePopup) close() {
	p.cancelWait()
	hide, hideEnv := p.hideKey, p.hideEnv
	*p = completePopup{src: p.src, grammar: p.grammar, loadSet: p.loadSet, hideKey: hide, hideEnv: hideEnv}
}

// install makes ans the popup's answer. Candidates whose text the grammar
// cannot write are left out, so every row on screen can be completed. The
// answer's title and note are folded onto one line each (sanitizeLine), as
// every candidate's display text already is by its source: each is drawn in
// one row that height counts, and a newline in either — a directory's name in
// `folders in …` or `no directory …` — would draw more rows than it says, and
// an escape sequence in one would reach the terminal (sol r26-c13 2). The
// selection starts from the top for a fresh query, and otherwise holds on to
// the candidate it was on (by id) while that one is still offered, as the
// session list holds its rows.
func (p *completePopup) install(ans completeAnswer, fresh bool) {
	ans.Title, ans.Note = sanitizeLine(ans.Title), sanitizeLine(ans.Note)
	items := make([]completeItem, 0, len(ans.Items))
	for _, it := range ans.Items {
		if !p.writable(it) {
			continue
		}
		items = append(items, it)
	}
	ans.Items = items
	p.ans = ans
	sel := 0
	if !fresh {
		sel = min(p.sel, max(0, len(items)-1))
		for i, it := range items {
			if it.id() == p.selID {
				sel = i
				break
			}
		}
	}
	p.setSel(sel)
}

func (p completePopup) writable(it completeItem) bool {
	if _, _, ok := p.grammar.write(it.Insert); !ok {
		return false
	}
	if it.Openable {
		if _, _, ok := p.grammar.write(it.descendText()); !ok {
			return false
		}
	}
	return true
}

// setSel selects candidate i and moves the window only as far as it has to
// for i to be among the completeMaxRows it shows.
func (p *completePopup) setSel(i int) {
	n := len(p.ans.Items)
	if n == 0 {
		p.sel, p.top, p.selID = 0, 0, ""
		return
	}
	p.sel = min(max(i, 0), n-1)
	p.selID = p.ans.Items[p.sel].id()
	p.top = min(max(p.top, 0), max(0, n-completeMaxRows))
	if p.sel < p.top {
		p.top = p.sel
	}
	if p.sel >= p.top+completeMaxRows {
		p.top = p.sel - completeMaxRows + 1
	}
}

// selected is the highlighted candidate, if there is one.
func (p completePopup) selected() (completeItem, bool) {
	if !p.open || p.sel < 0 || p.sel >= len(p.ans.Items) {
		return completeItem{}, false
	}
	return p.ans.Items[p.sel], true
}

// pending says the answer is waiting on its load.
func (p completePopup) pending() bool { return p.open && p.waiting }

// -------------------------------------------------------------------- keys

// completeVerb is what a key did with the highlighted candidate.
type completeVerb int

const (
	// completeAccepted: enter, or tab on a candidate that is not a directory
	// to descend into — the token is replaced by the candidate and a space,
	// which ends the token and so closes the popup.
	completeAccepted completeVerb = iota + 1
	// completeDescended: tab on a directory — the token is replaced by the
	// directory's path and a "/", with no space, and the popup stays open on
	// what is inside it.
	completeDescended
)

// completeChoice is a candidate a key chose, and the input it leaves: the
// owner puts value in its input with the cursor at cursor (a byte offset),
// then syncs the popup — or, choosing to act on item rather than write it,
// does that instead.
type completeChoice struct {
	verb   completeVerb
	item   completeItem
	value  string
	cursor int
}

// key is the popup's keyboard, for its owner to call first while the popup
// is visible. handled says the key was the popup's: the owner does nothing
// more with it — and when choice.verb is set, applies choice. A key that is
// not the popup's (handled false) is the owner's as if no popup were up:
// every key the popup does not name, and enter with no candidate to accept.
//
//   - ↑/ctrl+p and ↓/ctrl+n move the selection, wrapping as the slash menu's
//     do; with nothing to choose they do nothing, and are still the popup's —
//     it is what the keyboard is on.
//   - tab completes the highlighted candidate — descends into a directory,
//     accepts anything else — and with no candidate does nothing: tab never
//     submits (codex, grok-build).
//   - enter accepts the highlighted candidate. Alt+enter is not enter: it is
//     the composer's newline.
//   - esc hides the popup until the token under the cursor changes; the input
//     is left as it is.
func (p *completePopup) key(msg tea.KeyMsg) (choice completeChoice, handled bool) {
	if !p.open {
		return completeChoice{}, false
	}
	switch msg.Type {
	case tea.KeyUp, tea.KeyCtrlP:
		p.move(-1)
		return completeChoice{}, true
	case tea.KeyDown, tea.KeyCtrlN:
		p.move(1)
		return completeChoice{}, true
	case tea.KeyTab:
		it, ok := p.selected()
		if !ok {
			return completeChoice{}, true
		}
		if it.Openable {
			return p.choose(completeDescended, it), true
		}
		return p.choose(completeAccepted, it), true
	case tea.KeyEnter:
		if msg.Alt {
			return completeChoice{}, false
		}
		it, ok := p.selected()
		if !ok {
			return completeChoice{}, false
		}
		return p.choose(completeAccepted, it), true
	case tea.KeyEsc:
		p.hideKey, p.hideEnv = p.tokKey, p.env
		p.close()
		return completeChoice{}, true
	}
	return completeChoice{}, false
}

// move moves the selection delta candidates, wrapping at either end.
func (p *completePopup) move(delta int) {
	n := len(p.ans.Items)
	if n == 0 {
		return
	}
	p.setSel(((p.sel+delta)%n + n) % n)
}

// choose is the input a choice leaves. An accept replaces the token with the
// candidate's text and one space after it — a space already following the
// token is taken in rather than doubled (acceptSlash's rule; only a space,
// because a newline after it is the draft's own) — with the cursor past the
// space. A descend replaces it with the directory's text alone, with the
// cursor where that text ends, inside the closing quote of a quoted token so
// that typing goes on inside it.
func (p completePopup) choose(verb completeVerb, it completeItem) completeChoice {
	text := it.Insert
	if verb == completeDescended {
		text = it.descendText()
	}
	tok, inner, ok := p.grammar.write(text)
	if !ok {
		// install left out every candidate that cannot be written.
		return completeChoice{}
	}
	v := p.value
	start, end := p.tok.start, p.tok.end
	if verb == completeDescended {
		return completeChoice{verb: verb, item: it, value: v[:start] + tok + v[end:], cursor: start + inner}
	}
	if end < len(v) && v[end] == ' ' {
		end++
	}
	ins := tok + " "
	return completeChoice{verb: verb, item: it, value: v[:start] + ins + v[end:], cursor: start + len(ins)}
}

// -------------------------------------------------------------------- view

// completeShape is how the popup fills the rows it is given: the title rule,
// the candidate rows shown, and the line under them that counts the rest.
type completeShape struct {
	title bool
	shown int
	more  bool
}

// shape fits the popup into at most rows lines. Its natural shape is the title
// rule (when the source gave a title), up to completeMaxRows candidates — or
// one line saying why there are none — and, when the candidates do not all
// fit, the line that counts the rest. Short of room the title goes first,
// then candidate rows, the count line kept while there are two lines to share.
func (p completePopup) shape(rows int) completeShape {
	if !p.open || rows <= 0 {
		return completeShape{}
	}
	s := completeShape{title: p.ans.Title != ""}
	n := len(p.ans.Items)
	if n == 0 {
		// One line: the note.
		s.shown = 1
		if s.title && rows < 2 {
			s.title = false
		}
		return s
	}
	s.shown = min(n, completeMaxRows)
	s.more = n > s.shown
	if s.lines() <= rows {
		return s
	}
	s.title = false
	if s.lines() <= rows {
		return s
	}
	if rows == 1 {
		return completeShape{shown: 1}
	}
	return completeShape{shown: rows - 1, more: true}
}

func (s completeShape) lines() int {
	n := s.shown
	if s.title {
		n++
	}
	if s.more {
		n++
	}
	return n
}

// height is how many lines the popup takes given at most rows: 0 while it is
// closed. The owner lays out that many above its input and hands the same
// number to view.
func (p completePopup) height(rows int) int { return p.shape(rows).lines() }

// view is the popup in at most rows lines of exactly width cells: the title
// rule, the window of candidates — the gutter's ❯ on the selected one — and
// `↓ N more` under them while there are more below, `↑ N more` once the
// window has reached the bottom (so the count line does not come and go as
// the selection scrolls, and the popup's height never changes under it).
func (p completePopup) view(th Theme, width, rows int) string {
	s := p.shape(rows)
	if s.lines() == 0 || width <= 0 {
		return ""
	}
	out := make([]string, 0, s.lines())
	if s.title {
		out = append(out, completeTitleRule(th, p.ans.Title, width))
	}
	items := p.ans.Items
	if len(items) == 0 {
		note, st := p.ans.Note, styleFG(th.Dim)
		switch {
		case note != "" && p.ans.NoteErr:
			st = styleFG(th.Err)
		case note != "":
		case p.pending():
			note = completePendingNote
		default:
			note = completeEmptyNote
		}
		out = append(out, padRow(renderSegs(width, seg{"  " + note, st}), width))
		return strings.Join(out, "\n")
	}
	nameW, detailW := completeColumns(width, items)
	// The window is the one the keys settled for completeMaxRows, narrowed
	// to the rows given — around the selection, which is always on screen.
	top := min(max(p.top, 0), len(items)-s.shown)
	if p.sel < top {
		top = p.sel
	}
	if p.sel >= top+s.shown {
		top = p.sel - s.shown + 1
	}
	for i := top; i < top+s.shown; i++ {
		out = append(out, padRow(completeRow(th, items[i], i == p.sel, width, nameW, detailW), width))
	}
	if s.more {
		line := fmt.Sprintf("  ↑ %d more", top)
		if below := len(items) - top - s.shown; below > 0 {
			line = fmt.Sprintf("  ↓ %d more", below)
		}
		out = append(out, padRow(renderSegs(width, seg{line, styleFG(th.Dim)}), width))
	}
	return strings.Join(out, "\n")
}

// completeRow is one candidate: the gutter, the name in its column, the
// detail in its, and the note in whatever the row has left. The selected row
// is marked in the gutter and its name drawn bold, as the session list marks
// its rows.
func completeRow(th Theme, it completeItem, selected bool, width, nameW, detailW int) string {
	none := lipgloss.NewStyle()
	gutter := seg{agentGutterBlank, none}
	nameSt := styleFG(th.Bright)
	if selected {
		gutter = seg{agentGutterMark, styleFG(th.Accent)}
		nameSt = nameSt.Bold(true)
	}
	segs := []seg{gutter, {padCells(it.Name, nameW), nameSt}}
	used := completeGutterCols + nameW
	if detailW > 0 {
		segs = append(segs, seg{strings.Repeat(" ", completeGapCols), none},
			seg{padCells(it.Detail, detailW), styleFG(th.Dim)})
		used += completeGapCols + detailW
	}
	if avail := width - used - completeGapCols; it.Note != "" && avail >= 1 {
		noteSt := styleFG(th.Dim)
		switch it.Tone {
		case completeToneAccent:
			noteSt = styleFG(th.Accent)
		case completeToneProvider:
			noteSt = styleFG(th.Provider)
		}
		segs = append(segs, seg{strings.Repeat(" ", completeGapCols), none}, seg{clampWidth(it.Note, avail), noteSt})
	}
	return renderSegs(width, segs...)
}

// completeColumns is the name and detail columns for items at width: each as
// wide as its widest entry across every candidate, capped by its share of the
// row. With no detail and no note anywhere the name takes the row.
func completeColumns(width int, items []completeItem) (nameW, detailW int) {
	names, details, notes := 0, 0, false
	for _, it := range items {
		names = max(names, cellWidth(it.Name))
		details = max(details, cellWidth(it.Detail))
		notes = notes || it.Note != ""
	}
	avail := max(1, width-completeGutterCols)
	if details == 0 && !notes {
		return max(1, min(names, avail)), 0
	}
	nameW = max(1, min(names, completeNameCap, width*completeNameShare/100))
	if details > 0 {
		detailW = min(details, width*completeDetailShare/100)
	}
	return nameW, detailW
}

// cellWidth is s's width in cells, without lipgloss's measurement for the
// ASCII text most names and paths are: completeColumns measures every
// candidate of an answer, and the composer's can run to tens of thousands.
func cellWidth(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return lipgloss.Width(s)
		}
	}
	return len(s)
}

// completeTitleRule is the rule a title is drawn in: `──── title ─`, the
// mockup's, right-aligned so the candidates' names below it keep their own
// left edge.
func completeTitleRule(th Theme, title string, width int) string {
	rule := styleFG(th.Rule)
	t := " " + clampWidth(title, max(1, width-3)) + " "
	fill := max(1, width-lipgloss.Width(t)-1)
	return padRow(renderSegs(width, seg{strings.Repeat("─", fill), rule}, seg{t, styleFG(th.Bright)}, seg{"─", rule}), width)
}
