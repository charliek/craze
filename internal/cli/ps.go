package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/hub"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/roster"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/transcript"
	"github.com/charliek/craze/internal/version"
)

// craze ps (plan 032 §3.12, P8): every running craze session of this user on
// this machine, from the hub's roster — hub.Ensure finds the hub or starts
// one, and its sessions.list answers — one row a session. With --no-hub, or
// when no hub can be had (Ensure or the list failing), each session's host
// is read directly instead, as the hub would read it (hub.Direct: the same
// poll, the same rows), and stderr says so. It fails, exit 1, only when the
// path it took last fails too: the registry cannot be read.
//
// A row's state is the session list's rule (roster.Row.State), so craze ps
// and the list never disagree about a session.

const (
	// hubBudget bounds a hub client command's Ensure and the call it makes
	// (X34: Ensure is never run without a deadline).
	hubBudget = 10 * time.Second
	// psDirectWait bounds craze ps's read of the hosts themselves: one round
	// of the poll, each host asked once.
	psDirectWait = 3 * time.Second
	// psTitleCells is the title column's width when stdout is no terminal
	// that says its own; psTitleMin the width a terminal's narrower DIR
	// column gives the title back first, down to psDirMin cells.
	psTitleCells = 100
	psTitleMin   = 10
	psDirMin     = 10
	// psModelCells is the widest the MODEL column is: a model id is the
	// row's exact one (plan 035 §3.2 C11) and unbounded, so a longer one is
	// cut with an ellipsis, as a title is, rather than widening every row.
	// It holds every model of craze's shipped catalog whole (the longest
	// alias is 29); an agent's own ids are its own.
	psModelCells = 32
	// psShortID is how many characters of a craze session id craze ps shows.
	psShortID = 8
	// psNone is a cell craze ps has nothing for.
	psNone = "-"
)

// psClient is who craze ps says it is, to the hub and to a host.
var psClient = protocol.ClientInfo{Kind: "cli", Name: "craze ps", Version: version.Version}

// psNoSessions is what craze ps prints when nothing runs.
const psNoSessions = "no sessions running"

type psFlags struct {
	json  bool
	noHub bool
	saved bool
	dir   string
	brief bool
}

func newPsCmd() *cobra.Command {
	f := &psFlags{}
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List this machine's running craze sessions",
		Long: "craze ps lists every running craze session of this user on this machine, from the " +
			"per-machine hub (started on demand): its id, state, provider, model, directory, how " +
			"long it has been in that state, and its title. --no-hub reads each session's host " +
			"directly instead. --saved lists the saved sessions instead — this craze directory's " +
			"sessions that no host runs — through a running hub, or from the session index with none.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.dir != "" && !f.saved {
				return usagef("craze ps: -C/--dir needs --saved")
			}
			switch {
			case f.brief && !f.json:
				return usagef("craze ps: --brief needs --json: the table has no catalogs to leave out")
			case f.brief && f.saved:
				return usagef("craze ps: --brief does not go with --saved, whose rows have no catalogs")
			}
			if f.saved {
				return runPsSaved(cmd, *f)
			}
			return runPs(cmd, *f)
		},
	}
	cmd.Flags().BoolVar(&f.json, "json", false, "print the hub's roster (sessions.list's result) as JSON")
	cmd.Flags().BoolVar(&f.brief, "brief", false, "with --json: leave each session's model and mode catalogs out of its row")
	cmd.Flags().BoolVar(&f.noHub, "no-hub", false, "read each session's host directly, without the hub")
	cmd.Flags().BoolVar(&f.saved, "saved", false, "list the saved sessions — not running, ready to reopen — newest first "+
		"(with --json, sessions.saved's result; with --no-hub, read from the session index)")
	cmd.Flags().StringVarP(&f.dir, "dir", "C", "", "with --saved, only the sessions that ran in this directory")
	return cmd
}

// runPs is craze ps (the file's comment).
func runPs(cmd *cobra.Command, f psFlags) error {
	env := rundir.ProcessEnv()
	stderr := errWriter(cmd)
	var (
		raw json.RawMessage
		res protocol.HubSessionsListResult
	)
	if f.noHub {
		fmt.Fprintln(stderr, "craze ps: --no-hub: reading each session's host directly")
	} else {
		var err error
		raw, res, err = psFromHub(env)
		if err != nil {
			raw = nil
			fmt.Fprintf(stderr, "craze ps: the hub is not available (%s); reading each session's host directly\n",
				sanitizeLine(err.Error()))
		}
	}
	if raw == nil {
		ctx, cancel := context.WithTimeout(context.Background(), psDirectWait)
		defer cancel()
		var err error
		if res, err = hub.Direct(ctx, env, psClient); err != nil {
			return exitf(1, "craze ps: %s", sanitizeLine(err.Error()))
		}
	}
	out := cmd.OutOrStdout()
	if f.json {
		if raw == nil {
			b, err := json.Marshal(res)
			if err != nil {
				return exitf(1, "craze ps: %v", err)
			}
			raw = b
		}
		if f.brief {
			b, err := psBrief(raw)
			if err != nil {
				return kindf(errkind.Internal, "craze ps: --brief: %v", err)
			}
			raw = b
		}
		_, err := fmt.Fprintf(out, "%s\n", raw)
		return err
	}
	_, err := io.WriteString(out, psTable(psRows(res, env.Home, indexTitles(), time.Now()), psWidth(out)))
	return err
}

// psBrief is a roster's JSON less each row's catalogs and nothing else: read as
// raw members, so what a later craze or host adds passes through it as through
// the hub. A roster row's row may be absent (not read yet).
func psBrief(raw json.RawMessage) (json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	var rows []map[string]json.RawMessage
	if s, ok := top["sessions"]; ok {
		if err := json.Unmarshal(s, &rows); err != nil {
			return nil, err
		}
	}
	for _, r := range rows {
		rr, ok := r["row"]
		if !ok {
			continue
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal(rr, &row); err != nil {
			return nil, err
		}
		delete(row, "catalogs")
		b, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		r["row"] = b
	}
	if rows != nil {
		b, err := json.Marshal(rows)
		if err != nil {
			return nil, err
		}
		top["sessions"] = b
	}
	return json.Marshal(top)
}

// psFromHub is the hub's roster: Ensure, then its sessions.list, within
// hubBudget — the result as the hub wrote it, and decoded.
func psFromHub(env rundir.Env) (json.RawMessage, protocol.HubSessionsListResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hubBudget)
	defer cancel()
	sock, err := hub.Ensure(ctx, env, protocol.ConnectionCapabilities{})
	if err != nil {
		return nil, protocol.HubSessionsListResult{}, err
	}
	raw, err := hub.List(ctx, sock, psClient)
	if err != nil {
		return nil, protocol.HubSessionsListResult{}, err
	}
	var res protocol.HubSessionsListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, protocol.HubSessionsListResult{}, err
	}
	return raw, res, nil
}

// psWidth is out's width in cells when it is a terminal that says one, else
// 0: no terminal.
func psWidth(out io.Writer) int {
	if f, ok := out.(*os.File); ok {
		if w, ok := terminalWidth(f); ok {
			return w
		}
	}
	return 0
}

// psRow is one session as craze ps prints it: its cells, and what it is
// ordered by.
type psRow struct {
	state roster.State
	since time.Time
	id    string
	cells [7]string
}

// psHeader is craze ps's header row.
var psHeader = [7]string{"SESSION", "STATE", "PROVIDER", "MODEL", "DIR", "SINCE", "TITLE"}

// psRows is res's roster rows as craze ps prints them, in its order: by
// state (roster.State's order: needs you first), then newest in its state
// first, then by craze session id. The state and since are the list's rule
// (roster.Row's); the provider and directory are the host's answer's, else
// its registry entry's; the title the session's own, else the session
// index's (titles, by craze id), as the list's; the model the session's
// current model id as its row says it (SF-114), cut to psModelCells, "-"
// where the row does not say (an older host, or one not yet read). Every
// cell is one line, its escape sequences and controls gone.
func psRows(res protocol.HubSessionsListResult, home string, titles map[string]string, now time.Time) []psRow {
	rows := make([]psRow, 0, len(res.Sessions))
	for _, rr := range res.Sessions {
		r, err := roster.FromRosterRow(rr)
		if err != nil {
			// A row this build cannot read: the hub's facts alone.
			rr.Row = nil
			r, _ = roster.FromRosterRow(rr)
		}
		provider, model, dir, title := r.Host.Provider, "", r.Host.Workspace, ""
		if s := r.Session; s != nil {
			if s.Provider != "" {
				provider = s.Provider
			}
			if s.Workspace != "" {
				dir = s.Workspace
			}
			model, title = s.Model, s.Title
		}
		if psLine(title) == "" {
			title = titles[rr.SessionID]
		}
		row := psRow{state: r.State(), since: r.Since(), id: rr.SessionID}
		row.cells = [7]string{
			psShort(rr.SessionID),
			row.state.String(),
			psLine(provider),
			psClip(psLine(model), psModelCells),
			psLine(psTilde(home, dir)),
			psAge(now, row.since),
			psLine(title),
		}
		for i, c := range row.cells {
			if c == "" {
				row.cells[i] = psNone
			}
		}
		rows = append(rows, row)
	}
	slices.SortStableFunc(rows, func(a, b psRow) int {
		if c := cmp.Compare(a.state, b.state); c != 0 {
			return c
		}
		if c := b.since.Compare(a.since); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	return rows
}

// psDir is the DIR column's index.
const psDir = 4

// psTable is rows as craze ps prints them (psTableOf), psNoSessions for none.
func psTable(rows []psRow, width int) string {
	cells := make([][]string, len(rows))
	for i := range rows {
		cells[i] = rows[i].cells[:]
	}
	return psTableOf(psHeader[:], cells, psDir, psNoSessions, width)
}

// psTableOf is a table as craze ps prints one: none for no rows (cut to a
// terminal's width, as every line is); otherwise header and a line a row,
// each column as wide as its widest cell and two spaces apart, the title
// last, cut with an ellipsis — to psTitleCells
// when width is 0 (no terminal), and on a terminal to what is left of its
// width, no line ever wider than it (r24 1): when what is left is under
// psTitleMin, the DIR column (index dir) gives cells back first, down to
// psDirMin, its paths cut from the left so their ends stay; a terminal too
// narrow for the other columns has every line cut to its width. Every row
// has header's columns.
func psTableOf(header []string, rows [][]string, dir int, none string, width int) string {
	if len(rows) == 0 {
		return psClip(none, width) + "\n"
	}
	last := len(header) - 1
	widths := make([]int, last)
	for i := range widths {
		widths[i] = ansi.StringWidth(header[i])
		for _, r := range rows {
			widths[i] = max(widths[i], ansi.StringWidth(r[i]))
		}
	}
	titleMax := psTitleCells
	if width > 0 {
		used := 0
		for _, w := range widths {
			used += w + 2
		}
		titleMax = width - used
		if short := psTitleMin - titleMax; short > 0 {
			give := max(min(short, widths[dir]-psDirMin), 0)
			widths[dir] -= give
			titleMax += give
		}
	}
	var b strings.Builder
	line := func(cells []string) {
		var l strings.Builder
		for i, w := range widths {
			cell := cells[i]
			if i == dir {
				cell = psCutLeft(cell, w)
			}
			l.WriteString(cell)
			l.WriteString(strings.Repeat(" ", w-ansi.StringWidth(cell)+2))
		}
		title := cells[last]
		if ansi.StringWidth(title) > titleMax {
			title = ""
			if titleMax > 0 {
				title = ansi.Truncate(cells[last], titleMax, "…")
			}
		}
		l.WriteString(title)
		b.WriteString(strings.TrimRight(psClip(l.String(), width), " "))
		b.WriteByte('\n')
	}
	line(header)
	for _, r := range rows {
		line(r)
	}
	return b.String()
}

// psClip is line cut with an ellipsis to width cells, when width is > 0: a
// terminal's — no line craze ps prints is wider than its terminal,
// psNoSessions included (r25 5) — or the MODEL column's psModelCells.
func psClip(line string, width int) string {
	if width > 0 && ansi.StringWidth(line) > width {
		return ansi.Truncate(line, width, "…")
	}
	return line
}

// psCutLeft is s cut from the left to at most w cells, an ellipsis in place
// of what went: a path's end is what tells it apart.
func psCutLeft(s string, w int) string {
	n := ansi.StringWidth(s)
	if n <= w {
		return s
	}
	if w < 1 {
		return ""
	}
	out := ansi.TruncateLeft(s, n-w+1, "…")
	for drop := n - w + 2; ansi.StringWidth(out) > w; drop++ {
		// A wide character straddling the cut.
		out = ansi.TruncateLeft(s, drop, "…")
	}
	return out
}

// psShort is a craze session id as craze ps shows it: its last psShortID
// characters. A craze id is a UUIDv7 whose first eight hex digits are its
// clock's — the same for every session started within about a minute — and
// whose last ones are random.
func psShort(id string) string {
	if len(id) <= psShortID {
		return id
	}
	return id[len(id)-psShortID:]
}

// psLine is s on one line: escape sequences and controls gone, runs of
// whitespace one space (the list's rule).
func psLine(s string) string { return transcript.SanitizeLine(s) }

// psTilde is dir with home abbreviated to ~: home cleaned first, so a HOME
// written with a trailing slash still is (r24 2).
func psTilde(home, dir string) string {
	if home != "" {
		home = filepath.Clean(home)
	}
	if home == "" || home == "/" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(dir, home+"/"); ok {
		return "~/" + rest
	}
	return dir
}

// psAge is how long ago since was, in its one largest unit — `42s`, `5m`,
// `3h`, `6d`, the list's — or "" with no time. A time ahead of now (another
// clock's) is 0s.
func psAge(now, since time.Time) string {
	if since.IsZero() {
		return ""
	}
	d := max(now.Sub(since), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// craze ps --saved (plan 046 B6): the saved sessions — this CRAZE_HOME's
// session index's sessions that no running host serves — newest first, at
// most psSavedLimit, sub-agents' left out: a running hub's sessions.saved
// (hub.Find, needing the capability reopen — which says a hub serves
// sessions.saved and session.reopen both — and never starting a hub, since
// the index is this process's to read too), else — no hub running, --no-hub,
// a hub without reopen (an older craze's, Find's LacksError), or any other
// failure of the hub's path, as plain craze ps falls back on one — the same
// function the hub's answer is (savedSessions) over the index directly, and
// stderr says so. It fails, exit 1, only when the path it took last fails
// too: the registry cannot be read.

const (
	// psSavedLimit is how many saved sessions craze ps --saved asks for: the
	// method's most.
	psSavedLimit = protocol.SavedLimitMax
	// psNoSaved is what craze ps --saved prints when nothing is saved.
	psNoSaved = "no saved sessions"
)

// psSavedHeader is craze ps --saved's header row.
var psSavedHeader = []string{"SESSION", "PROVIDER", "DIR", "AGE", "TITLE"}

// psSavedDir is craze ps --saved's DIR column's index.
const psSavedDir = 2

// runPsSaved is craze ps --saved (the comment above).
func runPsSaved(cmd *cobra.Command, f psFlags) error {
	env := rundir.ProcessEnv()
	stderr := errWriter(cmd)
	q := protocol.SavedParams{Limit: psSavedLimit}
	if f.dir != "" {
		abs, err := filepath.Abs(f.dir)
		if err != nil {
			return exitf(1, "craze ps: %s: %s", sanitizeLine(f.dir), sanitizeLine(err.Error()))
		}
		q.Workspace = abs
	}
	var (
		raw json.RawMessage
		res protocol.SavedResult
	)
	if f.noHub {
		fmt.Fprintln(stderr, "craze ps: --no-hub: reading the session index directly")
	} else {
		var err error
		raw, res, err = psSavedFromHub(env, q)
		if err != nil {
			raw = nil
			fmt.Fprintf(stderr, "craze ps: %s; reading the session index directly\n", psSavedWhy(err))
		}
	}
	if raw == nil {
		var perr *protocol.Error
		res, perr = savedSessions(env, q, func(format string, args ...any) {
			fmt.Fprintf(stderr, "craze ps: %s\n", sanitizeLine(fmt.Sprintf(format, args...)))
		})
		if perr != nil {
			return exitf(1, "craze ps: %s", sanitizeLine(perr.Message))
		}
	}
	out := cmd.OutOrStdout()
	if f.json {
		if raw == nil {
			// As the hub writes it (protocol.MarshalLine), byte for byte.
			b, err := protocol.MarshalLine(res)
			if err != nil {
				return exitf(1, "craze ps: %v", err)
			}
			raw = bytes.TrimSuffix(b, []byte{'\n'})
		}
		_, err := fmt.Fprintf(out, "%s\n", raw)
		return err
	}
	if res.More {
		fmt.Fprintf(stderr, "craze ps: more saved sessions than these %d: only the newest are listed\n", len(res.Sessions))
	}
	_, err := io.WriteString(out, psTableOf(psSavedHeader, psSavedRows(res, env.Home, time.Now()), psSavedDir, psNoSaved, psWidth(out)))
	return err
}

// psSavedFromHub is a running hub's saved sessions: Find (never a start),
// then its sessions.saved, within hubBudget — the result as the hub wrote
// it, and decoded.
func psSavedFromHub(env rundir.Env, q protocol.SavedParams) (json.RawMessage, protocol.SavedResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), hubBudget)
	defer cancel()
	sock, err := hub.Find(ctx, env, hub.ReopenNeed())
	if err != nil {
		return nil, protocol.SavedResult{}, err
	}
	raw, err := hub.Saved(ctx, sock, psClient, q)
	if err != nil {
		return nil, protocol.SavedResult{}, err
	}
	var res protocol.SavedResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, protocol.SavedResult{}, err
	}
	return raw, res, nil
}

// psSavedWhy is why craze ps --saved reads the index itself, in its words:
// no hub running (hub.ErrNoHub, Find's answer for none); a hub that cannot
// answer sessions.saved — one whose hello lacks reopen (Find's LacksError: a
// craze from before it), or one that refuses the method unsupported all the
// same; or anything else of the hub's path, as plain craze ps says it.
func psSavedWhy(err error) string {
	if errors.Is(err, hub.ErrNoHub) {
		return "no hub is running"
	}
	if _, ok := errors.AsType[*hub.LacksError](err); ok {
		return "the hub cannot list saved sessions (an older craze's)"
	}
	if pe, ok := errors.AsType[*protocol.Error](err); ok && pe.Data.Code == protocol.CodeUnsupported {
		return "the hub cannot list saved sessions (an older craze's)"
	}
	return fmt.Sprintf("the hub is not available (%s)", sanitizeLine(err.Error()))
}

// psSavedRows is res's saved sessions as craze ps --saved prints them, in
// res's order (newest first): SESSION the craze id's last psShortID
// characters — a row with none, its provider session id's, which can be any
// string, so counted in runes (psShortRunes) — PROVIDER, DIR
// with home as ~, AGE since its last activity, and TITLE, "-" for none.
// Every cell is one line, its escape sequences and controls gone.
func psSavedRows(res protocol.SavedResult, home string, now time.Time) [][]string {
	rows := make([][]string, 0, len(res.Sessions))
	for _, s := range res.Sessions {
		id := s.CrazeSessionID
		if id == "" {
			id = s.ProviderSessionID
		}
		cells := []string{
			psLine(psShortRunes(id)),
			psLine(s.Provider),
			psLine(psTilde(home, s.Workspace)),
			psAge(now, s.UpdatedAt),
			psLine(s.Title),
		}
		for i, c := range cells {
			if c == "" {
				cells[i] = psNone
			}
		}
		rows = append(rows, cells)
	}
	return rows
}

// psShortRunes is psShort counted in runes: id's last psShortID characters,
// never a cut inside one — a provider session id is the provider's, and need
// not be ASCII as a craze id is.
func psShortRunes(id string) string {
	n := 0
	for i := len(id); i > 0; {
		_, size := utf8.DecodeLastRuneInString(id[:i])
		i -= size
		if n++; n == psShortID {
			return id[i:]
		}
	}
	return id
}
