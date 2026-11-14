package agent

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/charliek/craze/internal/harness/redact"
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
// Lock order: s.mu for the add and the enqueue; beneath it, the harness's
// redactor (its own leaf locks, taken as a report and each publication widen
// the tally's redactor) and the outbox's mutex, a leaf. Nothing is flushed
// under s.mu. The counters are incarnation-
// local, as turn ids are: nothing is persisted, and a load's replay counts
// nothing (it replays tool rows, not service calls).
//
// The URLs (F.4.1; review branch F1, review fix1 F1–F3, fix2 #1–#3, fix3
// #1–#2) are held as the web service returned them — its own key already
// scrubbed out — and redacted only as they are published, with the tally's
// own redactor, webRed, which only ever widens: every report and every
// publication first adds to it the keys of the harness's redactor as it
// stands then (redact.Replacer.Union) — the session's, its running
// children's, and those of children retired in the running harness turn,
// which the harness stops covering once that turn returns. A publication is
// therefore the URL as returned redacted with every key the harness's
// redactor has held at any report or publication of this session so far:
// once covered, always covered. A key learned while a publication is being
// made is covered from the next one on (harness.Session.Redactor's
// snapshot). The cut to webURLMax bytes follows the redaction, so it never
// splits a key that redaction knows.

// The URL pipeline's bounds (F.4.1): a published URL is cut to at most
// webURLMax bytes, at a UTF-8 boundary, and at most webURLsMax distinct values
// are published. A tally holds at most webURLsMax URLs — the ones distinct by
// their published form (publishForm) when reported — each in a copy of at
// most webURLHeld bytes, which bounds its memory (webURLsMax × webURLHeld,
// never the replies the URLs were cut from).
const (
	webURLMax  = 500
	webURLsMax = 100
	webURLHeld = 4 << 10
)

// cutWebURL is u cut back to the last UTF-8 boundary at or below webURLMax
// bytes, and whether anything was cut.
func cutWebURL(u string) (string, bool) { return cutAt(u, webURLMax) }

// cutAt is u cut back to the last UTF-8 boundary at or below n bytes, and
// whether anything was cut.
func cutAt(u string, n int) (string, bool) {
	if len(u) <= n {
		return u, false
	}
	end := n
	for end > 0 && !utf8.RuneStart(u[end]) {
		end--
	}
	return u[:end], true
}

// webHeld is one URL as a tally holds it: the text the web service returned,
// cut to webURLHeld bytes — a copy, sharing nothing with what it was cut from
// — and whether the cut took anything, which makes the text the start of a
// longer one.
type webHeld struct {
	text string
	cut  bool
}

// publishForm is h as it is published with red: redacted — as the start of a
// longer text (Head) when it was cut, so a key it ends halfway through is
// not shown halved, else whole (String) — then cut to webURLMax bytes; and
// whether anything of it was left out. The one form both a publication and
// the report's admission (add) judge a URL by.
func publishForm(h webHeld, red *redact.Replacer) (string, bool) {
	text := red.String(h.text)
	if h.cut {
		text = red.Head(h.text)
	}
	v, cut := cutWebURL(text)
	return v, cut || h.cut
}

// webTally is one tally as the session holds it: the calls' counts, and their
// URLs as the web service returned them (webHeld), one per published form at
// the report, first seen first, at most webURLsMax. truncated says a URL
// reported was left out: one past the cap, one whose form was empty, or one
// folded into a held URL with the same form that is not the very same URL
// (add). state publishes it. These URLs never leave the session; only
// state's value is published.
type webTally struct {
	searches, fetches, failed int
	urls                      []webHeld
	truncated                 bool
}

// add counts call: its kind, its failure, and each of its URLs — held unless
// one held already publishes as it would with red, the tally's redactor at
// the report. Such a URL is folded into the held one, silently only when it
// is that very URL — the same held text, cut no further — and otherwise with
// truncated set (review fix4): a URL that differs only behind keys red knows
// now publishes apart again once a later key tells the two apart, and one
// the cut folded was cut, which the held one's publication may not say.
func (t *webTally) add(call tool.WebCall, red *redact.Replacer) {
	if call.Fetch {
		t.fetches++
	} else {
		t.searches++
	}
	if call.Failed {
		t.failed++
	}
	for _, u := range call.URLs {
		if u == "" {
			continue
		}
		text, cut := cutAt(u, webURLHeld)
		h := webHeld{text: text, cut: cut}
		form, _ := publishForm(h, red)
		if form == "" {
			t.truncated = true
			continue
		}
		if i := slices.IndexFunc(t.urls, func(o webHeld) bool { f, _ := publishForm(o, red); return f == form }); i >= 0 {
			if o := t.urls[i]; o.text != h.text || (h.cut && !o.cut) {
				t.truncated = true
			}
			continue
		}
		if len(t.urls) >= webURLsMax {
			t.truncated = true
			continue
		}
		h.text = strings.Clone(text)
		t.urls = append(t.urls, h)
	}
}

// empty says the tally counts nothing.
func (t *webTally) empty() bool {
	return t.searches == 0 && t.fetches == 0 && t.failed == 0 && len(t.urls) == 0 && !t.truncated
}

// state is the tally as it is published (F.4.1 steps 2–5): each held URL in
// its published form with red, the tally's redactor at the publication; each
// value listed once, first seen first, at most webURLsMax; URLsTruncated when
// a URL was cut or a distinct one was left out, now or as it was reported. It
// shares nothing with t.
func (t *webTally) state(red *redact.Replacer) *WebSearchState {
	w := &WebSearchState{Searches: t.searches, Fetches: t.fetches, Failed: t.failed, URLsTruncated: t.truncated}
	for _, h := range t.urls {
		v, cut := publishForm(h, red)
		if cut {
			w.URLsTruncated = true
		}
		if v == "" || slices.Contains(w.URLs, v) {
			continue
		}
		if len(w.URLs) >= webURLsMax {
			w.URLsTruncated = true
			continue
		}
		w.URLs = append(w.URLs, strings.Clone(v))
	}
	return w
}

// webRedactorLocked widens the tally's redactor (webRed) by the harness's as
// it stands now — every key the session knows, its running children's and
// those retired in its running turn included (harness.Session.Replacer) — and
// returns it: never narrower than any it returned before. s.mu is held; the
// harness takes only its own leaf locks beneath it.
func (s *nativeSession) webRedactorLocked() *redact.Replacer {
	if s.hs != nil {
		s.webRed = s.webRed.Union(s.hs.Replacer())
	}
	return s.webRed
}

// webReport is harness.Options.OnWebCall: one call the web service handled,
// from any goroutine — a turn's tool, a child's runner, a background worker —
// with no lock of the harness's held, while the child whose call it was, if
// any, is still registered with the harness. Under s.mu the tally's
// redactor is widened now — so it covers that child's keys from here on —
// and the call is counted, its URLs held as returned, in the open window
// (and the tally enqueued) or in webIdle, or dropped once the tally is
// frozen (the engine's close, or the session's own).
func (s *nativeSession) webReport(call tool.WebCall) {
	if seam := s.webReportSeam; seam != nil {
		seam()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.webFrozen {
		return
	}
	red := s.webRedactorLocked()
	if !s.webOpen {
		s.webIdle.add(call, red)
		return
	}
	s.web.add(call, red)
	s.enqueueWebLocked(&s.web)
}

// enqueueWebLocked enqueues t as a web section: its published value, made
// with the tally's redactor widened now, the delta's own. s.mu is held.
func (s *nativeSession) enqueueWebLocked(t *webTally) {
	s.enqueueDeltaLocked("", Event{}, &StateDelta{WebSearch: t.state(s.webRedactorLocked())})
}

// openWebLocked opens a turn's window: webLast cleared, webIdle taken as the
// starting tally and enqueued at once when it counts anything. s.mu is held.
func (s *nativeSession) openWebLocked() {
	s.webLast = webTally{}
	s.web, s.webIdle = s.webIdle, webTally{}
	s.webOpen = true
	if !s.webFrozen && !s.web.empty() {
		s.enqueueWebLocked(&s.web)
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
		if !s.web.empty() {
			s.enqueueWebLocked(&s.web)
		}
		if keepLast {
			s.webLast = s.web
		}
	}
	s.web = webTally{}
}

// FreezeWebTally is WebTallyFreezer: the engine's close cutoff, called under
// e.mu before the synthetic closing ending is enqueued. The tally a waiter on
// the current turn must hold goes out first — the open window's when it
// counts anything, else, while the engine still has a turn current, the
// released prompt's (webLast): a restatement replaces what was published
// before, with the redactor as it stands now — and then nothing is counted
// again. Calls still idle are discarded. Only s.mu and the leaves beneath it
// (the harness's redactor, the outbox) are taken; nothing blocks or flushes.
// Idempotent.
func (s *nativeSession) FreezeWebTally(turnCurrent bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.webFrozen {
		return
	}
	switch {
	case s.webOpen && !s.web.empty():
		s.enqueueWebLocked(&s.web)
	case turnCurrent && !s.webLast.empty():
		s.enqueueWebLocked(&s.webLast)
	}
	s.freezeWebLocked()
}

// freezeWebLocked is the cutoff itself, the engine's close's (FreezeWebTally)
// and the session's own Close's: nothing counted or restated from here, every
// tally — the calls still idle included — discarded. s.mu is held.
func (s *nativeSession) freezeWebLocked() {
	s.webFrozen = true
	s.web, s.webIdle, s.webLast = webTally{}, webTally{}, webTally{}
}

// RestateWebTally is WebTallyFreezer's settlement half: called by the engine
// under e.mu before it enqueues the ended of the turn it settles, it enqueues
// the released prompt's tally (webLast) once more when it counts anything —
// the gap's twin of the close's freeze, for a turn that ends on its own: a
// waiter whose cursor fell after the release's restatement meets the count
// ahead of the ending. webLast is then spent: that turn's ending follows.
// Freezes nothing; the value published before is replaced, redacted with the
// session's keys as they stand now.
func (s *nativeSession) RestateWebTally() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.webFrozen && !s.webLast.empty() {
		s.enqueueWebLocked(&s.webLast)
	}
	s.webLast = webTally{}
}

// WebSearch is WebSearcher: whether this incarnation's tool list held
// websearch and webfetch at Open, and whether Open has decided yet.
func (s *nativeSession) WebSearch() (offered, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.webOffered, s.webKnown
}
