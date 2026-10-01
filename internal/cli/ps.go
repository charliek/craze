package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

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
}

func newPsCmd() *cobra.Command {
	f := &psFlags{}
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List this machine's running craze sessions",
		Long: "craze ps lists every running craze session of this user on this machine, from the " +
			"per-machine hub (started on demand): its id, state, provider, model, directory, how " +
			"long it has been in that state, and its title. --no-hub reads each session's host " +
			"directly instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPs(cmd, *f)
		},
	}
	cmd.Flags().BoolVar(&f.json, "json", false, "print the hub's roster (sessions.list's result) as JSON")
	cmd.Flags().BoolVar(&f.noHub, "no-hub", false, "read each session's host directly, without the hub")
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
		_, err := fmt.Fprintf(out, "%s\n", raw)
		return err
	}
	_, err := io.WriteString(out, psTable(psRows(res, env.Home, indexTitles(), time.Now()), psWidth(out)))
	return err
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
// index's (titles, by craze id), as the list's. No row carries the model the
// session runs now, so MODEL is "-" (SF-114). Every cell is one line, its
// escape sequences and controls gone.
func psRows(res protocol.HubSessionsListResult, home string, titles map[string]string, now time.Time) []psRow {
	rows := make([]psRow, 0, len(res.Sessions))
	for _, rr := range res.Sessions {
		r, err := roster.FromRosterRow(rr)
		if err != nil {
			// A row this build cannot read: the hub's facts alone.
			rr.Row = nil
			r, _ = roster.FromRosterRow(rr)
		}
		provider, dir, title := r.Host.Provider, r.Host.Workspace, ""
		if s := r.Session; s != nil {
			if s.Provider != "" {
				provider = s.Provider
			}
			if s.Workspace != "" {
				dir = s.Workspace
			}
			title = s.Title
		}
		if psLine(title) == "" {
			title = titles[rr.SessionID]
		}
		row := psRow{state: r.State(), since: r.Since(), id: rr.SessionID}
		row.cells = [7]string{
			psShort(rr.SessionID),
			row.state.String(),
			psLine(provider),
			psNone,
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

// psTable is rows as craze ps prints them: psNoSessions for none; otherwise
// a header and a row a session, each column as wide as its widest cell and
// two spaces apart, the title last, cut with an ellipsis — to psTitleCells
// when width is 0 (no terminal), and on a terminal to what is left of its
// width, no line ever wider than it (r24 1): when what is left is under
// psTitleMin, the DIR column gives cells back first, down to psDirMin, its
// paths cut from the left so their ends stay; a terminal too narrow for the
// other columns has every line cut to its width.
func psTable(rows []psRow, width int) string {
	if len(rows) == 0 {
		return psNoSessions + "\n"
	}
	var widths [6]int
	for i := range widths {
		widths[i] = ansi.StringWidth(psHeader[i])
		for _, r := range rows {
			widths[i] = max(widths[i], ansi.StringWidth(r.cells[i]))
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
			give := max(min(short, widths[psDir]-psDirMin), 0)
			widths[psDir] -= give
			titleMax += give
		}
	}
	var b strings.Builder
	line := func(cells [7]string) {
		var l strings.Builder
		for i, w := range widths {
			cell := cells[i]
			if i == psDir {
				cell = psCutLeft(cell, w)
			}
			l.WriteString(cell)
			l.WriteString(strings.Repeat(" ", w-ansi.StringWidth(cell)+2))
		}
		title := cells[6]
		if ansi.StringWidth(title) > titleMax {
			title = ""
			if titleMax > 0 {
				title = ansi.Truncate(cells[6], titleMax, "…")
			}
		}
		l.WriteString(title)
		out := l.String()
		if width > 0 && ansi.StringWidth(out) > width {
			out = ansi.Truncate(out, width, "…")
		}
		b.WriteString(strings.TrimRight(out, " "))
		b.WriteByte('\n')
	}
	line(psHeader)
	for _, r := range rows {
		line(r.cells)
	}
	return b.String()
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
