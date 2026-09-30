package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The `@` directory source (plan 030 §3.15, owner decisions 7–8): where a new
// session from the session list's input runs, picked the way Claude Code
// picks one — the places sessions are and were, and any directory typed as a
// path, browsed a level at a time with tab. Nothing is hard-coded: no
// projects folder, no setting (decision 7).
//
// It is completePopup's source (complete.go) for the list's leading `@`
// token (sessions_input.go), in two modes by what the token holds:
//
//   - A name — anything not starting with `~`, `/` or `.` — filters the
//     candidates the list knows: the directory of the session the list came
//     from (here), then every directory a running session is in, the most
//     recently active first, then the directories of the session index's
//     newest rows that are still there (sessions.Store.RecentDirs, read as
//     the list opens). Each once, noted `here · N running`, `N running` or
//     `used 3h ago`. Typing filters them: a basename prefix first, then a
//     substring of the path as it is drawn, case folded both ways.
//   - A path — `~`, `~/…`, `/…`, `.`, `./…`, `..` — browses: the text up to
//     its last `/` names a directory (the whole text when it has no `/`),
//     read off the Update, and its subdirectories whose names start with
//     what follows are the candidates — dot-directories only once what
//     follows starts with `.`, sorted, at most sessBrowseMax. tab descends
//     into one (the popup stays open on what is inside), enter picks it. A
//     directory that is not there says so: `no directory ~/x`.
//
// The candidates are the owner's, handed to the popup before each sync
// (setSource): the list's rows move under it with every snapshot, and the
// recent directories arrive once the list has read them.

const (
	// sessDirSourceID names the source: a load's result is stamped with it,
	// and the list takes only its own (applySessMsg).
	sessDirSourceID = "sessions-dirs"
	// sessRecentDirs is how many of the index's recent directories the list
	// reads (§3.15: RecentDirs(10)).
	sessRecentDirs = 10
	// sessBrowseMax is the most subdirectories a browsed directory offers
	// (§3.15): past it the user types more of the name.
	sessBrowseMax = 200
	// sessBrowseReadMax bounds how many entries a browse reads from one
	// directory, and sessBrowseBatch how many at a time, the load's context
	// checked between batches: a directory of a million files is read no
	// further than a user could ever want listed, and a browse the user has
	// moved on from stops within one batch.
	sessBrowseReadMax = 20000
	sessBrowseBatch   = 256
)

// The source's words.
const (
	sessAtTitle     = "where should it run?"
	sessBrowseTitle = "folders in "
)

// sessDirCand is one directory the name mode offers: absolute, its note, and
// the note's tone.
type sessDirCand struct {
	dir  string
	note string
	tone completeTone
}

// sessDirSource is the source as the list stands at one sync: the name mode's
// candidates in order, $HOME for `~` (as the list read it when it opened),
// and the directory relative paths start from — the session the list came
// from's workspace, which is the popup's completeEnv.Workspace too.
type sessDirSource struct {
	home  string
	here  string
	cands []sessDirCand
}

func (s sessDirSource) completeID() string { return sessDirSourceID }

func (s sessDirSource) complete(q completeQuery) completeAnswer {
	if sessPathLike(q.Text) {
		return s.browse(q)
	}
	return s.names(q.Text)
}

// sessPathLike says a token's text is a path to browse, not a name to
// filter by (§3.15: a token starting with `~`, `/` or `.`).
func sessPathLike(text string) bool {
	return strings.HasPrefix(text, "~") || strings.HasPrefix(text, "/") || strings.HasPrefix(text, ".")
}

// names is the name mode: every candidate whose basename starts with q, then
// every other whose path as drawn holds it, both case folded — in the
// candidates' own order within each.
func (s sessDirSource) names(q string) completeAnswer {
	fold := strings.ToLower(q)
	items := s.items()
	var starts, within []completeItem
	for _, it := range items {
		switch {
		case strings.HasPrefix(strings.ToLower(it.Name), fold):
			starts = append(starts, it)
		case strings.Contains(strings.ToLower(it.Detail), fold):
			within = append(within, it)
		}
	}
	return completeAnswer{Title: sessAtTitle, Items: append(starts, within...)}
}

// items is every candidate as a row: its basename, its path as drawn (`~`
// for $HOME), its note. A pick writes its basename (`@lumen `) — unless two
// candidates share it, or it would read back as a path to browse (a
// directory named `.dotfiles`), when it writes the path (`@~/a/lumen `); the
// grammar quotes either when it holds a space. Tab accepts a name as enter
// does: descending is the path mode's (the mockup's "tab/enter use it").
func (s sessDirSource) items() []completeItem {
	count := make(map[string]int, len(s.cands))
	for _, c := range s.cands {
		count[filepath.Base(c.dir)]++
	}
	out := make([]completeItem, 0, len(s.cands))
	for _, c := range s.cands {
		base := filepath.Base(c.dir)
		shown := sessTilde(s.home, c.dir)
		insert := base
		if count[base] > 1 || sessPathLike(base) {
			insert = shown
		}
		out = append(out, completeItem{
			Name: sanitizeLine(base), Detail: sanitizeLine(shown), Note: c.note, Tone: c.tone,
			Value: c.dir, Insert: insert,
		})
	}
	return out
}

// sessSplitPath splits a path token's text at its last "/": the directory it
// names as typed (the whole text when there is no "/"; "/" for a token
// directly under the root), what follows — the partial name the listing is
// filtered by — and the text a child's name is written after.
func sessSplitPath(text string) (parent, partial, prefix string) {
	i := strings.LastIndex(text, "/")
	switch {
	case i < 0:
		return text, "", text + "/"
	case i == 0:
		return "/", text[1:], "/"
	}
	return text[:i], text[i+1:], text[:i+1]
}

// sessExpand is a path token's text as an absolute directory: `~` and `~/…`
// under home, an absolute path cleaned, anything else under here (`.`, `..`,
// `./x`, `.config`). `~user` is not expanded — false, as is a `~` with no
// home or a relative path with no here.
func sessExpand(text, home, here string) (string, bool) {
	switch {
	case text == "~" || strings.HasPrefix(text, "~/"):
		if home == "" {
			return "", false
		}
		return filepath.Join(home, strings.TrimPrefix(text, "~")), true
	case strings.HasPrefix(text, "~"):
		return "", false
	case filepath.IsAbs(text):
		return filepath.Clean(text), true
	case here == "":
		return "", false
	}
	return filepath.Join(here, text), true
}

// browse is the path mode: the directory the token names, listed off the
// Update once (a load keyed by the directory, kept for the popup's life so
// every partial name typed after serves from it), and its subdirectories
// filtered by the partial name.
func (s sessDirSource) browse(q completeQuery) completeAnswer {
	parent, partial, prefix := sessSplitPath(q.Text)
	dir, ok := sessExpand(parent, s.home, s.here)
	if !ok {
		return completeAnswer{Note: "no directory " + parent, NoteErr: true}
	}
	ans := completeAnswer{Title: sessBrowseTitle + sessTilde(s.home, dir)}
	l, back := q.Loaded(dir)
	if !back {
		ans.Load = &completeLoad{Key: dir, Run: func(ctx context.Context) completeLoaded { return sessListDirs(ctx, dir) }}
		return ans
	}
	if l.Err != nil {
		if errors.Is(l.Err, fs.ErrNotExist) || errors.Is(l.Err, errNotADirectory) {
			return completeAnswer{Note: "no directory " + parent, NoteErr: true}
		}
		return completeAnswer{Note: "could not read " + parent + ": " + sessErrText(l.Err), NoteErr: true}
	}
	hidden := strings.HasPrefix(partial, ".")
	fold := strings.ToLower(partial)
	for _, child := range l.Items {
		name := child.Name
		if strings.HasPrefix(name, ".") && !hidden {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(name), fold) {
			continue
		}
		if len(ans.Items) == sessBrowseMax {
			break
		}
		path := filepath.Join(dir, name)
		ans.Items = append(ans.Items, completeItem{
			Name: sanitizeLine(name) + "/", Detail: sanitizeLine(sessTilde(s.home, path)),
			Value: path, Insert: prefix + name, Openable: true,
		})
	}
	return ans
}

// errNotADirectory is a browse of a path that is there and is not a
// directory: drawn as a missing directory is.
var errNotADirectory = errors.New("not a directory")

// sessErrText is a listing's failure in a few words: the cause without the
// path it was about (the note names the path as typed).
func sessErrText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return sanitizeLine(err.Error())
}

// sessListBatchHook, when a test sets it, is called after each batch
// sessListDirs reads, with the entries read so far: a test cancels a listing
// between two batches there, and counts what a listing read. nil in
// production.
var sessListBatchHook func(read int)

// sessListDirs is dir's subdirectories by name, sorted (case folded, then
// byte order): a symbolic link to a directory is one. It reads at most
// sessBrowseReadMax entries, sessBrowseBatch at a time, and gives up — with
// ctx's error, which nobody reads: the popup cancels only a load it no longer
// awaits — as soon as ctx is done.
func sessListDirs(ctx context.Context, dir string) completeLoaded {
	f, err := os.Open(dir)
	if err != nil {
		return completeLoaded{Err: err}
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err != nil {
		return completeLoaded{Err: err}
	} else if !st.IsDir() {
		return completeLoaded{Err: errNotADirectory}
	}
	var names []string
	for read := 0; read < sessBrowseReadMax; {
		if err := ctx.Err(); err != nil {
			return completeLoaded{Err: err}
		}
		ents, err := f.ReadDir(min(sessBrowseBatch, sessBrowseReadMax-read))
		read += len(ents)
		if sessListBatchHook != nil {
			sessListBatchHook(read)
		}
		for _, e := range ents {
			switch {
			case e.IsDir():
			case e.Type()&fs.ModeSymlink != 0:
				if st, err := os.Stat(filepath.Join(dir, e.Name())); err != nil || !st.IsDir() {
					continue
				}
			default:
				continue
			}
			names = append(names, e.Name())
		}
		if errors.Is(err, io.EOF) || (err == nil && len(ents) == 0) {
			break
		}
		if err != nil {
			return completeLoaded{Err: err}
		}
	}
	slices.SortFunc(names, func(a, b string) int {
		if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	items := make([]completeItem, len(names))
	for i, n := range names {
		items[i] = completeItem{Name: n}
	}
	return completeLoaded{Items: items}
}

// ------------------------------------------------------------ candidates

// sessDirCands is the name mode's candidates as the list stands (§3.15):
// here — the session the list came from's workspace — then each directory a
// running session is in, the most recently active first (the newest time a
// row there entered its state; ties by path), then the index's recent
// directories as the list read them. Each directory once, at its first
// place. here counts the sessions running in it (`here · 2 running`); a
// running directory its count (`1 running`); a recent one when it was last
// used (`used 3h ago`). An ended session runs nowhere.
func (m Model) sessDirCands() []sessDirCand {
	count := map[string]int{}
	latest := map[string]time.Time{}
	var running []string
	for _, r := range m.sessRunningRows() {
		if r.ended || r.workspace == "" {
			continue
		}
		ws := filepath.Clean(r.workspace)
		if count[ws] == 0 {
			running = append(running, ws)
		}
		count[ws]++
		if r.since.After(latest[ws]) {
			latest[ws] = r.since
		}
	}
	slices.SortStableFunc(running, func(a, b string) int {
		if !latest[a].Equal(latest[b]) {
			if latest[a].After(latest[b]) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	var out []sessDirCand
	seen := map[string]bool{}
	add := func(dir, note string, tone completeTone) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, sessDirCand{dir: dir, note: note, tone: tone})
	}
	if here := m.sessCameFrom(); here != "" {
		note := "here"
		if n := count[here]; n > 0 {
			note = fmt.Sprintf("here · %d running", n)
		}
		add(here, note, completeToneAccent)
	}
	for _, ws := range running {
		add(ws, fmt.Sprintf("%d running", count[ws]), completeToneAccent)
	}
	for _, r := range m.sessList.in.recents {
		add(filepath.Clean(r.Dir), "used "+m.sessAge(r.UsedAt)+" ago", completeToneDim)
	}
	return out
}

// sessHereDir is the workspace of the session the list came from, cleaned:
// the directory relative paths start from, and — while that session is
// behind the list (sessCameFrom) — here, the `@` picker's first row, and the
// target when nothing else names one.
func (m Model) sessHereDir() string {
	if m.cwd == "" {
		return ""
	}
	return filepath.Clean(m.cwd)
}

// sessCameFrom is here — the workspace of the session behind the list — or
// "" when nothing is behind it (X142's note, C15): its connection was lost
// (hereLost), whatever its host still does, or the list was opened over an
// unstarted session and discarded it (none). Then the `@` picker offers no
// here row, and a target no token names is the selected row's alone. Relative
// paths still start from the TUI's last workspace (sessHereDir): `@.` there
// is where the status row last said this terminal was.
func (m Model) sessCameFrom() string {
	if m.sessList.hereLost || m.sessList.none {
		return ""
	}
	return m.sessHereDir()
}

// sessAtSource is the source as the list stands now.
func (m Model) sessAtSource() sessDirSource {
	return sessDirSource{home: m.sessList.home, here: m.sessHereDir(), cands: m.sessDirCands()}
}
