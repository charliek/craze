package roster

import (
	"os"
	"slices"
	"time"

	"github.com/charliek/craze/internal/agent"
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
// changed, and the sessions hosts serves taken out. It says whether the rows
// or the error changed. A nil index is no saved rows.
func (s *savedRows) read(index Index, o options, hosts map[string]*hostState) bool {
	if index == nil {
		return false
	}
	changed := false
	if st := stampOf(o.indexPath()); !s.haveRead || st != s.stamp {
		all, err := index.All()
		switch {
		case err != nil:
			changed = s.err == nil || s.err.Error() != err.Error()
			s.err = err
		default:
			changed = s.err != nil
			s.err = nil
			s.stamp, s.haveRead = st, true
			s.candidates, s.titles = candidates(all)
		}
	}
	if rows := notRunning(s.candidates, hosts, o.savedMax); !slices.Equal(rows, s.rows) {
		s.rows = rows
		changed = true
	}
	return changed
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

// notRunning is cands less every session a listed host serves — named by
// its registry entry or its last answer, whether or not it answers now: a
// host that does not answer is unreachable, never saved — at most max.
func notRunning(cands []sessions.Row, hosts map[string]*hostState, max int) []sessions.Row {
	running := map[string]bool{}
	legacy := map[legacyKey]bool{}
	for _, h := range hosts {
		if h.gone {
			continue
		}
		running[h.entry.CrazeSessionID] = true
		legacy[legacyKey{h.entry.Provider, h.entry.ProviderSessionID}] = true
		if s := h.session; s != nil {
			running[s.ID] = true
			legacy[legacyKey{s.Provider, s.ProviderSessionID}] = true
		}
	}
	var out []sessions.Row
	for _, r := range cands {
		if len(out) == max {
			break
		}
		if r.CrazeID != "" && running[r.CrazeID] || r.CrazeID == "" && legacy[legacyKey{r.Provider, r.SessionID}] {
			continue
		}
		out = append(out, r)
	}
	return out
}
