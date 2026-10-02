package roster

import (
	"maps"
	"os"
	"slices"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/paths"
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
	// candidates is the index as the list offers it, before the running
	// are taken out: of a provider craze can resume, one row per craze id
	// (its newest) and every row with none, newest first.
	candidates []sessions.Row
	// titles is each craze id's newest row's title, whatever its provider:
	// what a running row shows before its host has answered.
	titles map[string]string
	// rows is candidates less the running, at most savedMax; err is why the
	// last read failed, nil after a good one (rows are then the last good
	// read's).
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
// whether the rows or the error changed. A nil index is no saved rows.
func (s *savedRows) read(index Index, indexPath func() string, max int, run runningSet) bool {
	if index == nil {
		return false
	}
	changed := s.reread(index, indexPath)
	return s.subtract(max, run) || changed
}

// reread reads the index again when its file has changed since the last good
// read — a stat otherwise — and says whether the error changed: the
// candidates' own change shows in the rows (subtract), which the running are
// not taken out of here.
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
	s.candidates, s.titles = candidates(all)
	return changed
}

// subtract is the candidates less every session run names, at most max: the
// rows. It says whether they changed.
func (s *savedRows) subtract(max int, run runningSet) bool {
	rows := notRunning(s.candidates, run, max)
	if slices.Equal(rows, s.rows) {
		return false
	}
	s.rows = rows
	return true
}

// candidates is all — the index, newest first — as the list offers it: rows
// of a provider craze can resume (the resume picker's rule: a provider this
// build knows and can load), deduplicated by craze id to its newest row, a
// row with no craze id kept as the row it is (its provider session id names
// it; a craze id is given it when it is resumed). titles is every craze id's
// newest title.
func candidates(all []sessions.Row) ([]sessions.Row, map[string]string) {
	var out []sessions.Row
	titles := map[string]string{}
	seen := map[string]bool{}
	for _, r := range all {
		if r.CrazeID != "" {
			if _, ok := titles[r.CrazeID]; !ok {
				titles[r.CrazeID] = r.Title
			}
		}
		if p, err := agent.ProviderByName(r.Provider); err != nil || !p.Resumable() {
			continue
		}
		if r.CrazeID != "" {
			if seen[r.CrazeID] {
				continue
			}
			seen[r.CrazeID] = true
		}
		out = append(out, r)
	}
	return out, titles
}

// legacyKey is an index row's own key, (provider, provider session id): how
// a row with no craze id is matched against a running session.
type legacyKey struct{ provider, sessionID string }

// runningSet is every session the running hosts serve, as notRunning takes
// them out of the saved rows: by craze id, and by (provider, provider session
// id) for an index row with no craze id.
type runningSet struct {
	ids    map[string]bool
	legacy map[legacyKey]bool
}

func newRunningSet() runningSet {
	return runningSet{ids: map[string]bool{}, legacy: map[legacyKey]bool{}}
}

// add names one session as running: its craze id, and its provider and
// provider session id.
func (r runningSet) add(crazeID, provider, providerSessionID string) {
	r.ids[crazeID] = true
	r.legacy[legacyKey{provider, providerSessionID}] = true
}

// runningOfHosts is every session a listed host serves — named by its
// registry entry or its last answer, whether or not it answers now: a host
// that does not answer is unreachable, never saved.
func runningOfHosts(hosts map[string]*hostState) runningSet {
	r := newRunningSet()
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
func runningOfRows(rows []Row) runningSet {
	r := newRunningSet()
	for _, row := range rows {
		r.add(row.Host.CrazeSessionID, row.Host.Provider, row.Host.ProviderSessionID)
		if s := row.Session; s != nil {
			r.add(s.ID, s.Provider, s.ProviderSessionID)
		}
	}
	return r
}

// notRunning is cands less every session run names, at most max.
func notRunning(cands []sessions.Row, run runningSet, max int) []sessions.Row {
	var out []sessions.Row
	for _, r := range cands {
		if len(out) == max {
			break
		}
		if r.CrazeID != "" && run.ids[r.CrazeID] || r.CrazeID == "" && run.legacy[legacyKey{r.Provider, r.SessionID}] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// SavesRunning says s lists saved a session one of its running rows serves —
// a saved row of a running craze id, or a legacy row of a running provider
// session id: the rule the saved half is taken by (notRunning), checked
// against s's own rows. A poller's Snapshot can: its saved half is taken at
// its tick, and a host's answer between two ticks can name a session — a
// provider session id its registry entry does not carry — that the last tick
// could not take out. The next tick does.
func (s Snapshot) SavesRunning() bool {
	run := runningOfRows(s.Running)
	for _, r := range s.Saved {
		if r.CrazeID != "" && run.ids[r.CrazeID] || r.CrazeID == "" && run.legacy[legacyKey{r.Provider, r.SessionID}] {
			return true
		}
	}
	return false
}

// Saved is the session list's saved half kept on its own (plan 032 §3.13):
// the hub's list roster, whose running rows come from the hub's roster
// subscription rather than a poll, keeps this CRAZE_HOME's saved rows with
// it — the poller's rules exactly (savedRows): the index read again only when
// its file's stamp has moved, its rows those of a provider craze can resume,
// one per craze id, less every session running, at most savedMax. It is not
// safe for concurrent use: one goroutine owns it.
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
	titles := s.s.titles
	changed := s.s.read(s.index, s.path, s.max, runningOfRows(running))
	return changed || !maps.Equal(titles, s.s.titles)
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
