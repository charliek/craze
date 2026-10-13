package roster

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/sessions"
)

// savedRows is the session list's saved rows (plan 030 §3.9, §3.12): the
// index's sessions that no running host serves. The index is read again only
// when its file has changed since the last good read — a tick costs one stat
// otherwise — and the running are taken out every tick, since hosts come and
// go between index writes.
type savedRows struct {
	stamp    indexStamp
	haveRead bool
	// index is the index as last read, newest first: what SavedSessions
	// chooses the rows from at every tick.
	index []sessions.Row
	// titles is each craze id's newest row's title, whatever its provider:
	// what a running row shows before its host has answered.
	titles map[string]string
	// subAgents is each craze id whose newest row is marked as a sub-agent's
	// session (sessions.Row.Unattended), plan 041 §3.3: what a running row
	// tags itself by. A craze id with no row is absent.
	subAgents map[string]bool
	// rows is the index's saved sessions less the running (SavedSessions,
	// sub-agents' included), at most savedMax; err is why the last read
	// failed, nil after a good one (rows are then the last good read's).
	rows []sessions.Row
	err  error
}

// indexStamp is what says the index file changed: whether it exists, its
// modification time and its size — every write replaces the file whole
// (sessions.Store), so a change moves the time, and the size is a second
// witness for a clock too coarse to.
type indexStamp struct {
	exists bool
	mod    time.Time
	size   int64
}

func stampOf(path string) indexStamp {
	if path == "" {
		return indexStamp{}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return indexStamp{}
	}
	return indexStamp{exists: true, mod: fi.ModTime(), size: fi.Size()}
}

// read brings the saved rows up to date: the index re-read if its file
// changed (reread), and the sessions run names taken out (subtract). It says
// whether the rows, the error, a title or a sub-agent mark changed. A nil
// index is no saved rows.
func (s *savedRows) read(index Index, indexPath func() string, max int, run Running) bool {
	if index == nil {
		return false
	}
	changed := s.reread(index, indexPath)
	return s.subtract(max, run) || changed
}

// reread reads the index again when its file has changed since the last good
// read — a stat otherwise — and says whether the error changed: the index's
// own change shows in the rows (subtract), which are not chosen here; a
// title or mark that moved is a change too.
func (s *savedRows) reread(index Index, indexPath func() string) bool {
	if index == nil {
		return false
	}
	st := stampOf(indexPath())
	if s.haveRead && st == s.stamp {
		return false
	}
	all, err := index.All()
	if err != nil {
		changed := s.err == nil || s.err.Error() != err.Error()
		s.err = err
		return changed
	}
	changed := s.err != nil
	s.err = nil
	s.stamp, s.haveRead = st, true
	titles, subAgents := s.titles, s.subAgents
	s.index = all
	s.titles, s.subAgents = indexMarks(all)
	// A title or a mark can move with the saved rows and the running statuses
	// both unchanged (the session is running, so no saved row shows it), and a
	// running row is drawn from them: every caller is told, the poller's tick
	// as much as Saved.Read.
	return changed || !maps.Equal(titles, s.titles) || !maps.Equal(subAgents, s.subAgents)
}

// subtract is the index's saved sessions less every session run names, as
// the list shows them — sub-agents' included, tagged — at most max: the
// rows (SavedSessions). It says whether they changed.
func (s *savedRows) subtract(max int, run Running) bool {
	rows, _ := SavedSessions(s.index, run, SavedQuery{SubAgents: true, Limit: max})
	if slices.Equal(rows, s.rows) {
		return false
	}
	s.rows = rows
	return true
}

// indexMarks is every craze id's newest title in all — the index, newest
// first — whatever its provider, and the craze ids whose newest row is
// marked as a sub-agent's session: what a running row shows and tags itself
// by before its host has answered.
func indexMarks(all []sessions.Row) (map[string]string, map[string]bool) {
	titles := map[string]string{}
	subAgents := map[string]bool{}
	for _, r := range all {
		if r.CrazeID == "" {
			continue
		}
		if _, ok := titles[r.CrazeID]; !ok {
			titles[r.CrazeID] = r.Title
			if r.Unattended != nil {
				subAgents[r.CrazeID] = true
			}
		}
	}
	return titles, subAgents
}

// SavedQuery is what a listing of the saved sessions keeps (SavedSessions,
// plan 046 B2): Workspace, when set, only the sessions whose directory is
// exactly it; SubAgents the sessions a sub-agent ran too (an index row
// marked Unattended), which are left out otherwise; and at most Limit of
// them, every one when it is 0.
type SavedQuery struct {
	Workspace string
	SubAgents bool
	Limit     int
}

// SavedSessions is the saved sessions of all — the session index, newest
// first (sessions.Store.All) — that no session run names serves, as every
// list of them chooses them (plan 046 B2): the session list's saved rows
// (savedRows, Saved) and the hub's sessions.saved alike, each with its own
// query. One row per craze id: its newest, whatever it is — the row a load
// of the craze id loads (craze serve --load takes the newest row carrying
// it, sessions.Store.ByCrazeID, and session.reopen resolves its id to the
// same row) — offered only when that row can be resumed (Unresumable: a
// provider this build can load, the wire's bounds), so a craze id whose
// newest row cannot be is not offered at all, never by an older row a load
// would not take; and a row with no craze id kept as the row it is (its
// provider session id names it; a craze id is given it when it is resumed),
// offered on the same terms. Less every session run names (by craze id, or by
// its provider and provider session id). Then, in this order: q.Workspace's
// filter, the sub-agent filter, the order — UpdatedAt, newest first, ties by
// SavedID — and q.Limit, more saying the limit left some out: every filter
// runs before the cut. It reads nothing and stats nothing: a workspace gone
// is still listed, so a hung filesystem cannot hold a listing.
func SavedSessions(all []sessions.Row, run Running, q SavedQuery) (rows []sessions.Row, more bool) {
	seen := map[string]bool{}
	for _, r := range all {
		if r.CrazeID != "" {
			if seen[r.CrazeID] {
				continue
			}
			seen[r.CrazeID] = true
		}
		if Unresumable(r) != "" {
			continue
		}
		if run.serves(r) {
			continue
		}
		if q.Workspace != "" && r.CWD != q.Workspace {
			continue
		}
		if r.Unattended != nil && !q.SubAgents {
			continue
		}
		rows = append(rows, r)
	}
	slices.SortStableFunc(rows, func(a, b sessions.Row) int {
		if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(SavedID(a), SavedID(b))
	})
	if q.Limit > 0 && len(rows) > q.Limit {
		return rows[:q.Limit], true
	}
	return rows, false
}

// Unresumable says why a saved session's deciding row — a craze id's newest
// row, or a row with no craze id — can be neither listed saved
// (SavedSessions) nor reopened (session.reopen), "" when it can be: its
// provider one this build knows and can load (the resume picker's rule), and
// the row within the wire's bounds (savedRowFits; the index is a file a
// person can edit). In words a refusal carries.
func Unresumable(r sessions.Row) string {
	if p, err := agent.ProviderByName(r.Provider); err != nil || !p.Resumable() {
		return fmt.Sprintf("that session's provider %q is not one this craze can resume", r.Provider)
	}
	if !savedRowFits(r) {
		return "that session's index row is malformed"
	}
	return ""
}

// SavedID is what reopens a saved session (plan 046 B2): its craze id, or
// <provider>:<provider session id> for a row with none — the form `craze
// serve --load` takes.
func SavedID(r sessions.Row) string {
	if r.CrazeID != "" {
		return r.CrazeID
	}
	return r.Provider + ":" + r.SessionID
}

// savedRowFits says an index row fits sessions.saved's bounds (plan 046 B2):
// its craze id, when it has one, a token a load takes (rundir.ValidToken, as
// craze serve --load's claim checks it: [A-Za-z0-9._-], 1 to 128 bytes — one
// that is not could be read as another session's id, a legacy key's colon
// among them); its id and its provider session id within protocol.SavedIDMax
// and SavedProviderSessionIDMax bytes, its workspace absolute and within
// SavedWorkspaceMax bytes, and both its times ones the wire can write in UTC
// (savedTimeFits). The index is a file a person can edit; a row that does not
// fit is malformed, and no list offers it.
func savedRowFits(r sessions.Row) bool {
	return (r.CrazeID == "" || rundir.ValidToken(r.CrazeID)) &&
		len(SavedID(r)) <= protocol.SavedIDMax && len(r.SessionID) <= protocol.SavedProviderSessionIDMax &&
		len(r.CWD) <= protocol.SavedWorkspaceMax && filepath.IsAbs(r.CWD) &&
		savedTimeFits(r.CreatedAt) && savedTimeFits(r.UpdatedAt)
}

// savedTimeFits says t, in UTC, is a time JSON can carry: RFC 3339 has four
// digits of year, and an index time can parse with an offset that carries it
// past them in UTC (9999-12-31T23:59:59-01:00 is the year 10000), which
// time.Time's MarshalJSON refuses.
func savedTimeFits(t time.Time) bool {
	_, err := t.UTC().MarshalJSON()
	return err == nil
}

// legacyKey is an index row's own key, (provider, provider session id): how
// a row with no craze id is matched against a running session.
type legacyKey struct{ provider, sessionID string }

// Running is every session the running hosts serve, as SavedSessions takes
// them out of the saved: by craze id, and by (provider, provider session id)
// — an index row's own key, matched whether or not the row has a craze id.
type Running struct {
	ids    map[string]bool
	legacy map[legacyKey]bool
}

func newRunning() Running {
	return Running{ids: map[string]bool{}, legacy: map[legacyKey]bool{}}
}

// add names one session as running: its craze id, and its provider and
// provider session id.
func (r Running) add(crazeID, provider, providerSessionID string) {
	r.ids[crazeID] = true
	r.legacy[legacyKey{provider, providerSessionID}] = true
}

// serves says r names row's session: by its craze id when it has one, or by
// its provider and provider session id whether or not it has one — two
// CRAZE_HOMEs' indexes can give one provider session different craze ids,
// and the registry is the whole HOME's.
func (r Running) serves(row sessions.Row) bool {
	if row.CrazeID != "" && r.ids[row.CrazeID] {
		return true
	}
	return r.legacy[legacyKey{row.Provider, row.SessionID}]
}

// RunningOfEntries is every session the registry's entries name as running
// (plan 046 B2): a reader's that lists the saved without a roster
// (sessions.saved, craze ps --saved). A host marked stopping (rundir.Serving)
// is not counted, so its session is listed saved at once — the roster has
// dropped it already — rather than in neither list.
func RunningOfEntries(entries []rundir.Entry) Running {
	r := newRunning()
	for _, e := range entries {
		if rundir.Serving(e) {
			r.add(e.CrazeSessionID, e.Provider, e.ProviderSessionID)
		}
	}
	return r
}

// runningOfHosts is every session a listed host serves — named by its
// registry entry or its last answer, whether or not it answers now: a host
// that does not answer is unreachable, never saved.
func runningOfHosts(hosts map[string]*hostState) Running {
	r := newRunning()
	for _, h := range hosts {
		if h.gone {
			continue
		}
		r.add(h.entry.CrazeSessionID, h.entry.Provider, h.entry.ProviderSessionID)
		if s := h.session; s != nil {
			r.add(s.ID, s.Provider, s.ProviderSessionID)
		}
	}
	return r
}

// runningOfRows is every session rows serve — named by its host or its last
// answer, whatever its status — as runningOfHosts names a poller's.
func runningOfRows(rows []Row) Running {
	r := newRunning()
	for _, row := range rows {
		r.add(row.Host.CrazeSessionID, row.Host.Provider, row.Host.ProviderSessionID)
		if s := row.Session; s != nil {
			r.add(s.ID, s.Provider, s.ProviderSessionID)
		}
	}
	return r
}

// SavesRunning says s lists saved a session one of its running rows serves —
// a saved row of a running craze id, or of a running provider session id:
// the rule the saved half is taken by (SavedSessions), checked
// against s's own rows. A poller's Snapshot can: its saved half is taken at
// its tick, and a host's answer between two ticks can name a session — a
// provider session id its registry entry does not carry — that the last tick
// could not take out. The next tick does.
func (s Snapshot) SavesRunning() bool {
	run := runningOfRows(s.Running)
	for _, r := range s.Saved {
		if run.serves(r) {
			return true
		}
	}
	return false
}

// Saved is the session list's saved half kept on its own (plan 032 §3.13):
// the hub's list roster, whose running rows come from the hub's roster
// subscription rather than a poll, keeps this CRAZE_HOME's saved rows with
// it — the poller's rules exactly (savedRows): the index read again only when
// its file's stamp has moved, its rows SavedSessions' — those of a provider
// craze can resume, one per craze id, less every session running — at most
// savedMax. It is not safe for concurrent use: one goroutine owns it.
type Saved struct {
	index Index
	path  func() string
	max   int
	s     savedRows
}

// NewSaved is the saved half over index — nil: no saved rows — whose file's
// stamp says it changed: indexPath's, paths.SessionsPath when nil.
func NewSaved(index Index, indexPath func() string) *Saved {
	if indexPath == nil {
		indexPath = paths.SessionsPath
	}
	return &Saved{index: index, path: indexPath, max: savedMax}
}

// Read brings the rows up to date as a poller's tick does: the index read
// again if its file changed since the last good read, and every session
// running serves taken out. It says whether the rows, the error or a title
// (Title) changed — a running row's index title is part of what a reader is
// told of, so a read that moved only titles is a change too.
func (s *Saved) Read(running []Row) bool {
	return s.s.read(s.index, s.path, s.max, runningOfRows(running))
}

// Subtract takes every session running serves out of the index as last read,
// reading nothing: the running rows moved, the index did not. It says whether
// the rows changed.
func (s *Saved) Subtract(running []Row) bool {
	if s.index == nil {
		return false
	}
	return s.s.subtract(s.max, runningOfRows(running))
}

// Rows is the saved rows now, newest first: a copy.
func (s *Saved) Rows() []sessions.Row { return slices.Clone(s.s.rows) }

// Err is why the index could not be read at the last read, nil when it was:
// Rows are then the last good read's.
func (s *Saved) Err() error { return s.s.err }

// Title is the index's title for craze session id — its newest row's — ""
// when it has none: what a running row shows before its host has answered.
func (s *Saved) Title(crazeID string) string { return s.s.titles[crazeID] }

// SubAgent is whether the index's newest row for craze session id is marked as
// a sub-agent's session (plan 041 §3.3), false when it has none: what a
// running row tags itself by, as Title is what it names itself by.
func (s *Saved) SubAgent(crazeID string) bool { return s.s.subAgents[crazeID] }
