package agent

import (
	"context"
	"slices"
)

// The live session's half of the slash catalog (plan 043 D2, P3, P5): the
// disk skills it scans for its provider, and the catalog it rebuilds from them
// and its snapshot. The scanner itself is slash_scan.go's.

var (
	_ SkillRescanner = (*session)(nil)
	_ SlashCataloger = (*session)(nil)
)

// PublishesSlashCatalog is SlashCataloger's: every live session does (plan 043
// P2) — its installs restate the catalog, and a commands update or a scan
// that changes it publishes it — whichever ACP agent it drives.
func (s *session) PublishesSlashCatalog() bool { return true }

// RescanSkills is SkillRescanner's: a scan asked for and never waited on. The
// engine calls it for every attach (engine.Attach), so a client opening an
// idle session learns of a skill created since the last turn ended — in the
// snapshot it is handed, when the walk is done before that is cut, or in a
// delta behind it (branch review B6: the order is not promised).
func (s *session) RescanSkills() { s.scanner.request() }

// walkSkills is one scan's work: the provider's disk skills under cwd and
// home, redacted against the keys craze knows (redactSkills). It runs with no
// lock held — on Start's goroutine for the first scan, on the scanner's for
// every later one.
func (s *session) walkSkills(ctx context.Context, cwd, home string) []Skill {
	walk := s.skillWalk
	if walk == nil {
		walk = discoverSkills
	}
	skills := walk(ctx, s.provider(), cwd, home)
	if ctx.Err() != nil {
		// Cut short — by Close, or by Start's own context on the install
		// walk: a partial walk is never anybody's catalog.
		return nil
	}
	if len(skills) == 0 {
		// Nothing to redact, so no model table to read for its keys.
		return skills
	}
	return redactSkills(skills, knownProviderKeys())
}

// installWalk is Start's own scan, the install trigger's (plan 043 P5): a
// walkSkills cancelled when either Start's ctx or the scanner's context (Close)
// ends, whichever is first. A walk that Start's ctx cut short fails the start
// with that context's error, as session/new would; one Close cut short returns
// no skills, and Start meets the close at its next step.
func (s *session) installWalk(ctx context.Context, cwd, home string) ([]Skill, error) {
	walkCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(s.scanner.ctx, cancel)
	defer stop()
	skills := s.walkSkills(walkCtx, cwd, home)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return skills, nil
}

// scanSkills is the scanner's scan: walkSkills over the directories Start
// read, which never change after it.
func (s *session) scanSkills(ctx context.Context) []Skill {
	s.mu.Lock()
	cwd, home := s.skillDir, s.skillHome
	s.mu.Unlock()
	return s.walkSkills(ctx, cwd, home)
}

// applySkills is the scanner's apply: a scan's result, taken only if its
// generation is newer than the last one applied — so an older result can never
// land over a newer one — and only while the session is open. The catalog is
// rebuilt in the same section, and when it changed, ONE delta says so (P3);
// when it did not — the common case, a scan that found what the last one did —
// nothing is published at all (D3).
//
// The scanner is armed only once an install has built the catalog, so there
// is always one to compare against. The delta is craze's own and goes through
// owedModelLocked as every such delta does. It is enqueued under s.mu and
// never flushed: the scanner's goroutine publishes nothing of its own that it
// would have to be ordered against, and the log's drainer delivers it.
func (s *session) applySkills(gen uint64, skills []Skill) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || gen <= s.skillsGen {
		return
	}
	s.skillsGen, s.skills = gen, skills
	if slash := s.slashChangedLocked(); slash != nil {
		s.enqueueDeltaLocked("", Event{}, s.owedModelLocked(&StateDelta{Slash: slash}))
	}
}

// slashChangedLocked rebuilds Snapshot.Slash from the snapshot and the skills
// last known, and returns the section a delta carries — the delta's own copy —
// when the catalog changed, nil when it is the one already stood. s.mu is
// held.
func (s *session) slashChangedLocked() *SlashCatalog {
	next := SlashCatalogFor(s.snap, s.skills).Items
	if s.snap.Slash != nil && slices.Equal(next, s.snap.Slash) {
		return nil
	}
	s.snap.Slash = next
	return &SlashCatalog{Items: cloneSlash(next)}
}
