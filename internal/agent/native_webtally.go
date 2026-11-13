package agent

import (
	"slices"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/tool"
)

// The web tally (plan 051 F.4): the native session counts the web service's
// calls by turn, and publishes each turn's count as a cumulative state delta
// (StateDelta.WebSearch) that a fold stamps with the turn running in the log.
// The harness and the service know nothing of turns; the engine names none to
// the session. The session holds, under s.mu:
//
//   - web, the open window's tally, and webOpen. A window is open from the
//     section that opens the turn's record — prompt's, where inPrompt is set,
//     after the engine enqueued the turn's started; openWake's, after the
//     opening bracket is enqueued — to the section that releases it —
//     prompt's deferred release; endWake's, before the ending bracket is
//     enqueued. A claim alone opens nothing.
//   - webIdle, the calls whose reports arrived while no window was open (a
//     background child's, a cancelled turn's call that returned after its
//     release): counted in the next window to open, which takes it as its
//     starting tally, and enqueued then — never between turns, where a fold
//     would put it on the ended turn.
//   - webLast, the released prompt window's final tally, restated in the gap
//     between the release and the engine's settlement by whichever comes
//     first, the settlement (RestateWebTally) or the close's freeze, ahead of
//     the turn's ending: cleared on every claim, every window's opening, the
//     restatement and the session's close.
//     A wake keeps none: its ending bracket is enqueued in the same section as
//     its restatement.
//   - webFrozen, set by the engine's close (FreezeWebTally) or the session's
//     own Close: from then on every report is dropped and no restatement is
//     made — the close's cutoff.
//
// A report belongs to the window open when its callback takes s.mu to update
// the tally. A report while a window is open adds to web and enqueues the
// whole tally, the delta's own copy, in the same section (StateDelta's one-
// author rule); the section that closes a non-empty window enqueues it once
// more, as the turn's last delta (the restatement), so a waiter that joined
// by a cursor mid-turn holds the whole count at the turn's end.
//
// Lock order: the redactor is read first (it takes s.mu briefly), then s.mu
// for the add and the enqueue; the outbox's mutex is a leaf beneath it.
// Nothing is published or flushed under s.mu. The counters are incarnation-
// local, as turn ids are: nothing is persisted, and a load's replay counts
// nothing (it replays tool rows, not service calls).

// The URL pipeline's bounds (F.4.1): a URL is cut to at most webURLMax bytes,
// at a UTF-8 boundary, and at most webURLsMax distinct values are kept.
const (
	webURLMax  = 500
	webURLsMax = 100
)

// cutWebURL is u cut back to the last UTF-8 boundary at or below webURLMax
// bytes, and whether anything was cut.
func cutWebURL(u string) (string, bool) {
	if len(u) <= webURLMax {
		return u, false
	}
	end := webURLMax
	for end > 0 && !utf8.RuneStart(u[end]) {
		end--
	}
	return u[:end], true
}

// addWebCall counts call in t: its kind, its failure, and urls — its URLs
// already redacted and cut, cut saying the cut took something — each kept
// once, first seen first, at most webURLsMax.
func addWebCall(t *WebSearchState, call tool.WebCall, urls []string, cut bool) {
	if call.Fetch {
		t.Fetches++
	} else {
		t.Searches++
	}
	if call.Failed {
		t.Failed++
	}
	if cut {
		t.URLsTruncated = true
	}
	for _, u := range urls {
		if slices.Contains(t.URLs, u) {
			continue
		}
		if len(t.URLs) >= webURLsMax {
			t.URLsTruncated = true
			continue
		}
		t.URLs = append(t.URLs, u)
	}
}

// webReport is harness.Options.OnWebCall: one call the web service handled,
// from any goroutine — a turn's tool, a child's runner, a background worker —
// with no lock of the harness's held. The URLs go through the session's
// redactor as it stands now and the cut; then, under s.mu, the call is counted
// in the open window (and the tally enqueued) or in webIdle, or dropped once
// the tally is frozen (the engine's close, or the session's own).
func (s *nativeSession) webReport(call tool.WebCall) {
	var urls []string
	cut := false
	if len(call.URLs) > 0 {
		red := s.redactor()
		for _, u := range call.URLs {
			v, c := cutWebURL(red(u))
			cut = cut || c
			if v != "" {
				urls = append(urls, v)
			}
		}
	}
	if seam := s.webReportSeam; seam != nil {
		seam()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.webFrozen {
		return
	}
	if !s.webOpen {
		addWebCall(&s.webIdle, call, urls, cut)
		return
	}
	addWebCall(&s.web, call, urls, cut)
	s.enqueueWebLocked(s.web)
}

// enqueueWebLocked enqueues t as a web section, the delta's own copy. s.mu is
// held.
func (s *nativeSession) enqueueWebLocked(t WebSearchState) {
	s.enqueueDeltaLocked("", Event{}, &StateDelta{WebSearch: t.Clone()})
}

// openWebLocked opens a turn's window: webLast cleared, webIdle taken as the
// starting tally and enqueued at once when it counts anything. s.mu is held.
func (s *nativeSession) openWebLocked() {
	s.webLast = WebSearchState{}
	s.web, s.webIdle = s.webIdle, WebSearchState{}
	s.webOpen = true
	if !s.webFrozen && !s.web.Empty() {
		s.enqueueWebLocked(s.web)
	}
}

// closeWebLocked closes the open window, if one is: its tally restated as its
// last delta when it counts anything and the tally is not frozen, and — for a
// prompt's window (keepLast) — kept as webLast, for a close in the gap before
// the engine settles the turn. s.mu is held.
func (s *nativeSession) closeWebLocked(keepLast bool) {
	if !s.webOpen {
		return
	}
	s.webOpen = false
	if !s.webFrozen {
		if !s.web.Empty() {
			s.enqueueWebLocked(s.web)
		}
		if keepLast {
			s.webLast = s.web
		}
	}
	s.web = WebSearchState{}
}

// FreezeWebTally is WebTallyFreezer: the engine's close cutoff, called under
// e.mu before the synthetic closing ending is enqueued. The tally a waiter on
// the current turn must hold goes out first — the open window's when it
// counts anything, else, while the engine still has a turn current, the
// released prompt's (webLast): a restatement of a value already published is
// idempotent, since a section replaces — and then nothing is counted again.
// Calls still idle are discarded. Only s.mu and the outbox's leaf are taken;
// nothing blocks or flushes. Idempotent.
func (s *nativeSession) FreezeWebTally(turnCurrent bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.webFrozen {
		return
	}
	switch {
	case s.webOpen && !s.web.Empty():
		s.enqueueWebLocked(s.web)
	case turnCurrent && !s.webLast.Empty():
		s.enqueueWebLocked(s.webLast)
	}
	s.freezeWebLocked()
}

// freezeWebLocked is the cutoff itself, the engine's close's (FreezeWebTally)
// and the session's own Close's: nothing counted or restated from here, every
// tally — the calls still idle included — discarded. s.mu is held.
func (s *nativeSession) freezeWebLocked() {
	s.webFrozen = true
	s.web, s.webIdle, s.webLast = WebSearchState{}, WebSearchState{}, WebSearchState{}
}

// RestateWebTally is WebTallyFreezer's settlement half: called by the engine
// under e.mu before it enqueues the ended of the turn it settles, it enqueues
// the released prompt's tally (webLast) once more when it counts anything —
// the gap's twin of the close's freeze, for a turn that ends on its own: a
// waiter whose cursor fell after the release's restatement meets the count
// ahead of the ending. webLast is then spent: that turn's ending follows.
// Freezes nothing; a value already published is replaced by itself.
func (s *nativeSession) RestateWebTally() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.webFrozen && !s.webLast.Empty() {
		s.enqueueWebLocked(s.webLast)
	}
	s.webLast = WebSearchState{}
}

// WebSearch is WebSearcher: whether this incarnation's tool list held
// websearch and webfetch at Open, and whether Open has decided yet.
func (s *nativeSession) WebSearch() (offered, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.webOffered, s.webKnown
}
