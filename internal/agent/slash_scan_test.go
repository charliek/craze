package agent

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/paths"
)

// The skill scanner's tests (plan 043 P5) and the live session's catalog
// emission (P3, D3): the scanner alone first, then a live session over the
// fake agent with a walk that blocks on the test's word — the "blocked fake
// filesystem" — and last the real walk over a workspace a test writes into.

// idle reports whether the scanner has no scan in flight.
func (sc *skillScanner) idle() bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return !sc.running
}

// awaitIdle waits for the scanner's flight to end.
func awaitIdle(t *testing.T, sc *skillScanner) {
	t.Helper()
	waitUntil(t, "the scanner to go idle", sc.idle)
}

// heldScanner is a scanner whose every scan reports itself started and then
// waits for the test to release it (or for the scanner's close), and whose
// applies are reported by generation.
type heldScanner struct {
	*skillScanner
	scans   atomic.Int32
	started chan int32
	release chan struct{}
	applied chan uint64
}

func newHeldScanner() *heldScanner {
	h := &heldScanner{started: make(chan int32, 16), release: make(chan struct{}, 16), applied: make(chan uint64, 16)}
	h.skillScanner = newSkillScanner(func(ctx context.Context) []Skill {
		n := h.scans.Add(1)
		h.started <- n
		select {
		case <-h.release:
		case <-ctx.Done():
		}
		return []Skill{{Name: "s"}}
	}, func(gen uint64, _ []Skill) { h.applied <- gen })
	return h
}

func (h *heldScanner) awaitStarted(t *testing.T, want int32) {
	t.Helper()
	if n := await(t, h.started, "a scan to start"); n != want {
		t.Fatalf("scan %d started, want scan %d", n, want)
	}
}

func (h *heldScanner) awaitApplied(t *testing.T, want uint64) {
	t.Helper()
	if gen := await(t, h.applied, "a scan's result to be applied"); gen != want {
		t.Fatalf("generation %d applied, want %d", gen, want)
	}
}

// TestSkillScannerCoalesces is single flight and the dirty bit: requests made
// while a scan runs start nothing beside it, and however many there were, they
// cost exactly one more scan once it ends — under the next generation.
func TestSkillScannerCoalesces(t *testing.T) {
	h := newHeldScanner()
	t.Cleanup(h.close)
	h.arm()
	h.request()
	h.awaitStarted(t, 1)
	for range 5 {
		h.request()
	}
	if n := h.scans.Load(); n != 1 {
		t.Fatalf("%d scans ran beside the first", n-1)
	}
	h.release <- struct{}{}
	h.awaitApplied(t, 1)
	h.awaitStarted(t, 2)
	h.release <- struct{}{}
	h.awaitApplied(t, 2)
	awaitIdle(t, h.skillScanner)
	if n := h.scans.Load(); n != 2 {
		t.Fatalf("five requests during a scan ran %d scans after it, want exactly one", n-1)
	}
	// Idle again, a request is a scan of its own.
	h.request()
	h.awaitStarted(t, 3)
	h.release <- struct{}{}
	h.awaitApplied(t, 3)
}

// TestSkillScannerWaitsToBeArmed: a request before arm runs nothing — the
// session has no catalog yet to compare a result against — and any number of
// them become exactly one scan when it is armed.
func TestSkillScannerWaitsToBeArmed(t *testing.T) {
	h := newHeldScanner()
	t.Cleanup(h.close)
	h.request()
	h.request()
	if n, idle := h.scans.Load(), h.idle(); n != 0 || !idle {
		t.Fatalf("an unarmed scanner started a scan (%d walked, idle %v)", n, idle)
	}
	h.arm()
	h.awaitStarted(t, 1)
	h.release <- struct{}{}
	h.awaitApplied(t, 1)
	awaitIdle(t, h.skillScanner)
	if n := h.scans.Load(); n != 1 {
		t.Fatalf("two requests before arm ran %d scans, want one", n)
	}

	// Armed with nothing owed, arm starts nothing.
	quiet := newHeldScanner()
	t.Cleanup(quiet.close)
	quiet.arm()
	if n := quiet.scans.Load(); n != 0 {
		t.Fatalf("arm with no request owed ran %d scans", n)
	}
}

// TestSkillScannerCloseMidScanDropsTheResult: close cancels the scan in
// flight, and what it returns is never applied; a request after close runs
// nothing — not even the one a request during the scan left owed.
func TestSkillScannerCloseMidScanDropsTheResult(t *testing.T) {
	h := newHeldScanner()
	h.arm()
	h.request()
	h.awaitStarted(t, 1)
	h.request() // owed, and never run
	h.close()
	awaitIdle(t, h.skillScanner)
	h.request()
	select {
	case gen := <-h.applied:
		t.Fatalf("generation %d was applied after close", gen)
	default:
	}
	if n := h.scans.Load(); n != 1 {
		t.Fatalf("%d scans ran after close", n-1)
	}
}

// hasSlash reports whether ev is a meta that carries a slash section.
func hasSlash(ev Event) bool { return ev.Type == EventMeta && ev.State != nil && ev.State.Slash != nil }

// slashDeltas is every meta in evs that carries a slash section.
func slashDeltas(evs []Event) []*StateDelta {
	var out []*StateDelta
	for _, ev := range evs {
		if hasSlash(ev) {
			out = append(out, ev.State)
		}
	}
	return out
}

func catalogHas(items []SlashItem, name, source string) bool {
	return slices.ContainsFunc(items, func(it SlashItem) bool { return it.Name == name && it.Source == source })
}

// installedSession is a session that has not been started but whose catalog
// has been built, as an install leaves it: what applySkills and the commands
// update need, with no agent behind them.
func installedSession(t *testing.T) *session {
	t.Helper()
	s := newTestSession(t, Options{})
	s.mu.Lock()
	s.snap.Slash = SlashCatalogFor(s.snap, s.skills).Items
	s.mu.Unlock()
	return s
}

// flushedEvents is everything s has published by now, the outbox included.
func flushedEvents(t *testing.T, s *session) []Event {
	t.Helper()
	if err := s.log.Flush(context.Background(), s.done); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return drainBuffered(s)
}

// TestApplySkillsOnlyNewerAndOnlyChanged is the apply's two rules: a result
// whose generation is not newer than the last one applied is dropped — an
// older scan never lands over a newer one — and one that leaves the catalog as
// it was publishes nothing. A change is one delta, carrying the slash section
// alone. After Close nothing is applied.
func TestApplySkillsOnlyNewerAndOnlyChanged(t *testing.T) {
	s := installedSession(t)
	drainBuffered(s)

	s.applySkills(2, []Skill{{Name: "newer", Description: "from scan 2"}})
	evs := flushedEvents(t, s)
	if len(evs) != 1 || len(slashDeltas(evs)) != 1 {
		t.Fatalf("a changed catalog is one slash delta, got:\n%s", formatEvents(evs))
	}
	st := evs[0].State
	if st.Commands != nil || st.Plugins != nil || st.Title != nil || st.Mode != nil {
		t.Fatalf("a scan's delta carries the slash section alone: %+v", st)
	}
	if !catalogHas(st.Slash.Items, "newer", SlashSourceSkill) || !slices.Equal(st.Slash.Items, s.Snapshot().Slash) {
		t.Fatalf("the delta %+v does not say what the snapshot now holds", st.Slash.Items)
	}

	s.applySkills(1, []Skill{{Name: "older", Description: "from scan 1"}})
	if evs := flushedEvents(t, s); len(evs) != 0 {
		t.Fatalf("a stale generation published:\n%s", formatEvents(evs))
	}
	if catalogHas(s.Snapshot().Slash, "older", SlashSourceSkill) {
		t.Fatal("a stale generation's skills reached the catalog")
	}

	s.applySkills(3, []Skill{{Name: "newer", Description: "from scan 2"}})
	if evs := flushedEvents(t, s); len(evs) != 0 {
		t.Fatalf("a scan that found what the last one did published:\n%s", formatEvents(evs))
	}

	_ = s.Close()
	s.applySkills(4, []Skill{{Name: "after-close"}})
	if catalogHas(s.Snapshot().Slash, "after-close", SlashSourceSkill) {
		t.Fatal("a result after Close was applied")
	}
}

// TestCommandsUpdateIsOneDeltaAndOnlyOnChange is P3 and D3 on the catalog
// update: one delta carrying the commands, the plugin rows and the slash
// catalog built from them; nothing at all for an update that changed none of
// them; and, while a scan is pending, the catalog built with the skills last
// known.
func TestCommandsUpdateIsOneDeltaAndOnlyOnChange(t *testing.T) {
	s := installedSession(t)
	s.mu.Lock()
	s.skills = []Skill{{Name: "known", Description: "the last scan's"}}
	s.mu.Unlock()
	drainBuffered(s)

	s.onUpdate(availableCommandsNotification("research"))
	evs := drainBuffered(s)
	if len(evs) != 1 {
		t.Fatalf("a catalog update is one delta, got:\n%s", formatEvents(evs))
	}
	st := evs[0].State
	if st == nil || st.Commands == nil || st.Plugins == nil || st.Slash == nil {
		t.Fatalf("the delta carries commands, plugins and slash together: %+v", st)
	}
	if !catalogHas(st.Slash.Items, "research", SlashSourceAgent) || !catalogHas(st.Slash.Items, "known", SlashSourceSkill) {
		t.Fatalf("the slash section %+v lacks the new command or the known skill", st.Slash.Items)
	}

	s.onUpdate(availableCommandsNotification("research"))
	if evs := drainBuffered(s); len(evs) != 0 {
		t.Fatalf("a repeated catalog published:\n%s", formatEvents(evs))
	}

	s.onUpdate(availableCommandsNotification("research", "deploy"))
	evs = drainBuffered(s)
	if len(evs) != 1 || evs[0].State.Commands == nil || evs[0].State.Slash == nil ||
		!catalogHas(evs[0].State.Slash.Items, "deploy", SlashSourceAgent) {
		t.Fatalf("a changed catalog is one delta with every section, got:\n%s", formatEvents(evs))
	}
}

// describedCommandsNotification is an available_commands_update carrying one
// command, named and described as given.
func describedCommandsNotification(name, desc string) acp.SessionNotification {
	return acp.SessionNotification{
		SessionID: loadSessionID,
		Update: mustJSON(map[string]any{"sessionUpdate": acp.UpdateAvailableCommands,
			"availableCommands": []map[string]any{{"name": name, "description": desc}}}),
	}
}

// TestAShadowedCommandsChangeStillRestatesTheCatalog is astra C1 H6: a
// catalog update that changes the commands but not the slash catalog — the
// agent's own /help, which craze's builtin shadows, re-described — is still
// one delta carrying commands, plugins AND the slash section, restated as it
// stands, so a client reads the commands and the catalog built from them as
// one change (P3). The repeat of that same update says nothing (D3).
func TestAShadowedCommandsChangeStillRestatesTheCatalog(t *testing.T) {
	s := installedSession(t)
	s.onUpdate(describedCommandsNotification("help", "old"))
	drainBuffered(s)
	before := s.Snapshot().Slash

	s.onUpdate(describedCommandsNotification("help", "new"))
	evs := drainBuffered(s)
	if len(evs) != 1 {
		t.Fatalf("a commands change is one delta, got:\n%s", formatEvents(evs))
	}
	st := evs[0].State
	if st == nil || st.Commands == nil || len(st.Commands.Commands) != 1 || st.Commands.Commands[0].Description != "new" {
		t.Fatalf("the delta does not carry the re-described command: %+v", st)
	}
	if st.Plugins == nil || st.Slash == nil {
		t.Fatalf("a commands delta must restate plugins and slash (P3): %+v", st)
	}
	after := s.Snapshot().Slash
	if !slices.Equal(before, after) {
		t.Fatal("fixture: the builtin /help should shadow the agent's, leaving the catalog as it was")
	}
	if !slices.Equal(st.Slash.Items, after) {
		t.Fatalf("the restated catalog %+v is not the snapshot's %+v", st.Slash.Items, after)
	}
	st.Slash.Items[0].Description = "changed"
	if s.Snapshot().Slash[0].Description == "changed" {
		t.Fatal("the restated catalog shares its rows with the session's")
	}

	s.onUpdate(describedCommandsNotification("help", "new"))
	if evs := drainBuffered(s); len(evs) != 0 {
		t.Fatalf("a repeated catalog published:\n%s", formatEvents(evs))
	}
}

// blockingWalkSession is a session over the fake agent whose skill walk is
// the test's: Start's own scan returns at once with what first says, and every
// later one reports that it started and then waits for the test's release — a
// filesystem that blocks — before returning what next says. The agent
// advertises no catalog (the fake's nocommands script), so no catalog update
// asks for a scan behind the test's back: every walk after Start's is one the
// test caused.
type blockingWalkSession struct {
	*session
	walks   atomic.Int32
	started chan struct{}
	release chan struct{}
	next    atomic.Value // []Skill
}

func newBlockingWalkSession(t *testing.T, first []Skill) *blockingWalkSession {
	t.Helper()
	b := &blockingWalkSession{started: make(chan struct{}, 16), release: make(chan struct{}, 16)}
	b.session = newTestSession(t, Options{
		Binary:    fakeAgentPath(t),
		ExtraArgs: []string{"-script=nocommands"},
		Workspace: t.TempDir(),
		Stderr:    io.Discard,
	})
	b.next.Store(first)
	b.skillWalk = func(ctx context.Context, _ Provider, _, _ string) []Skill {
		if b.walks.Add(1) == 1 {
			return first
		}
		b.started <- struct{}{}
		select {
		case <-b.release:
		case <-ctx.Done():
		}
		return b.next.Load().([]Skill)
	}
	return b
}

func (b *blockingWalkSession) awaitWalk(t *testing.T) {
	t.Helper()
	await(t, b.started, "a scan to start")
}

// awaitEvent waits for the collector to have an event that match accepts.
func awaitEvent(t *testing.T, log *eventLog, what string, match func(Event) bool) {
	t.Helper()
	waitUntil(t, what, func() bool { return slices.ContainsFunc(log.snapshot(), match) })
}

// awaitSlashDelta waits on the collector for a slash delta whose catalog has
// the named skill.
func awaitSlashDelta(t *testing.T, log *eventLog, skill string) {
	t.Helper()
	awaitEvent(t, log, "a slash delta listing the skill "+skill, func(ev Event) bool {
		return hasSlash(ev) && catalogHas(ev.State.Slash.Items, skill, SlashSourceSkill)
	})
}

// TestSkillScanNeverDelaysTheTurnsEnd is P5's promise, with a scan the
// filesystem holds: one is in flight (an attach asked for it) when a turn
// runs, and the turn still ends — EventDone published, the prompt returned —
// while the walk is blocked; the session's lock is free the whole time
// (Snapshot answers); the turn's end coalesces into the flight rather than
// starting a second walk beside it; and once the walk is released its result
// is ONE delta, the coalesced rescan that finds the same thing publishing
// nothing.
func TestSkillScanNeverDelaysTheTurnsEnd(t *testing.T) {
	b := newBlockingWalkSession(t, nil)
	log := collect(t, b.session)
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}

	b.next.Store([]Skill{{Name: "fresh", Description: "written while the session was idle"}})
	b.RescanSkills() // as engine.Attach does
	b.awaitWalk(t)

	done := make(chan error, 1)
	go func() {
		_, err := b.Prompt(context.Background(), "hello")
		done <- err
	}()
	if err := await(t, done, "the turn to end while a skill scan was blocked (its end waited on the walk)"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	awaitEvent(t, log, "EventDone while a skill scan was blocked", func(ev Event) bool { return ev.Type == EventDone })
	snapDone := make(chan Snapshot, 1)
	go func() { snapDone <- b.Snapshot() }()
	if snap := await(t, snapDone, "Snapshot while a scan is blocked (the scan holds the session's lock)"); catalogHas(snap.Slash, "fresh", SlashSourceSkill) {
		t.Fatal("the blocked scan's skill is in the catalog before the walk returned")
	}
	if n := b.walks.Load(); n != 2 {
		t.Fatalf("%d walks have run, want Start's and the blocked one: the turn's end started a walk beside it", n)
	}

	b.release <- struct{}{}
	awaitSlashDelta(t, log, "fresh")
	// The rescan the turn's end left owed runs next and finds the same skill.
	b.awaitWalk(t)
	b.release <- struct{}{}
	awaitIdle(t, b.scanner)
	// A marker behind whatever the rescan enqueued: once the collector has it,
	// it has everything before it, so the count below is the whole of it.
	if err := b.SetTitle("", "marker"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	awaitEvent(t, log, "the marker's title delta", func(ev Event) bool {
		return ev.Type == EventMeta && ev.State != nil && ev.State.Title != nil && *ev.State.Title == "marker"
	})
	var after []*StateDelta
	for _, st := range slashDeltas(log.snapshot()) {
		if catalogHas(st.Slash.Items, "fresh", SlashSourceSkill) {
			after = append(after, st)
		}
	}
	if len(after) != 1 {
		t.Fatalf("the skill was announced by %d deltas, want one (the rescan found nothing new)", len(after))
	}
	// Nothing but the turn's end can have asked for the third: the attach's
	// request was the second walk itself, and the agent sends no catalog.
	if n := b.walks.Load(); n != 3 {
		t.Fatalf("%d walks ran, want Start's, the attach's and the turn end's, coalesced", n)
	}
}

// TestCloseDropsTheScanInFlight: a session closed while its scan is blocked
// cancels the walk (its context ends) and applies nothing it returns.
func TestCloseDropsTheScanInFlight(t *testing.T) {
	b := newBlockingWalkSession(t, nil)
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	b.next.Store([]Skill{{Name: "late", Description: "after close"}})
	b.RescanSkills()
	b.awaitWalk(t)
	_ = b.Close()
	awaitIdle(t, b.scanner) // the walk saw its context end, with no release
	if catalogHas(b.Snapshot().Slash, "late", SlashSourceSkill) {
		t.Fatal("a scan cut short by Close was applied")
	}
	b.RescanSkills()
	if n := b.walks.Load(); n != 2 {
		t.Fatalf("a request after Close walked: %d walks", n)
	}
}

// TestCancellingStartEndsTheInstallWalk: the install trigger's walk runs on
// Start's goroutine, before session/new, and a walk the filesystem holds must
// not hold a start whose caller has given up (astra C1). Cancelling Start's
// context ends the walk, and Start fails at once with the context's error —
// as it would had the cancel landed in session/new — with nothing installed.
func TestCancellingStartEndsTheInstallWalk(t *testing.T) {
	s := newTestSession(t, Options{
		Binary:    fakeAgentPath(t),
		ExtraArgs: []string{"-script=nocommands"},
		Workspace: t.TempDir(),
		Stderr:    io.Discard,
	})
	walking := make(chan struct{}, 1)
	s.skillWalk = func(ctx context.Context, _ Provider, _, _ string) []Skill {
		walking <- struct{}{}
		<-ctx.Done() // a filesystem that answers nothing until the walk is given up
		return []Skill{{Name: "partial"}}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()
	await(t, walking, "the install walk to start")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start returned %v, want the cancelled context's error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return when its context was cancelled during the install walk")
	}
	if snap := s.Snapshot(); snap.Slash != nil {
		t.Fatalf("a cancelled start installed a catalog: %+v", snap.Slash)
	}
}

func writeSlashSkill(t *testing.T, dir, name, desc string) {
	t.Helper()
	path := filepath.Join(dir, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTheInstallListsTheSkillsAndATurnPicksUpNewOnes is D2 over the real walk
// (cursor's roots, which the fake agent's default provider has): a skill on
// disk before Start is in the install delta's catalog — the install trigger's
// scan runs before the install, so no delta has to follow it — and one written
// while the session runs is picked up when a turn ends, in a delta of its own.
// The description is redacted against the keys craze knows (P18): here one
// the shipped catalog funds a provider from.
func TestTheInstallListsTheSkillsAndATurnPicksUpNewOnes(t *testing.T) {
	names := modeltable.CatalogEnvNames()
	const key = "sk-slash-test-0123456789abcdefghijklmnop"
	t.Setenv(names[0], key)
	// The model table the keys are read from is this test's own empty craze
	// directory's: the shipped catalog alone, whatever the developer has.
	t.Setenv("CRAZE_HOME", t.TempDir())
	ws := t.TempDir()
	skills := filepath.Join(ws, ".cursor", "skills")
	writeSlashSkill(t, skills, "before-start", "on disk at Start")
	// No catalog from the agent (nocommands), so the turn's end is the only
	// thing after Start that asks for a scan.
	s := newTestSession(t, Options{
		Binary:    fakeAgentPath(t),
		ExtraArgs: []string{"-script=nocommands"},
		Workspace: ws,
		Stderr:    io.Discard,
	})
	log := collect(t, s)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !catalogHas(s.Snapshot().Slash, "before-start", SlashSourceSkill) {
		t.Fatalf("the catalog after Start lacks the skill on disk: %+v", s.Snapshot().Slash)
	}
	_ = s.log.Flush(context.Background(), s.done)
	awaitEvent(t, log, "a slash section", hasSlash)
	// The install delta is the one carrying every section (installDeltaLocked).
	var install *StateDelta
	for _, st := range slashDeltas(log.snapshot()) {
		if st.Title != nil && st.Config != nil {
			install = st
		}
	}
	if install == nil || !catalogHas(install.Slash.Items, "before-start", SlashSourceSkill) {
		t.Fatalf("the install delta does not restate the catalog with the skill: %+v", install)
	}

	writeSlashSkill(t, skills, "made-by-a-turn", "holds "+key)
	if _, err := s.Prompt(t.Context(), "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	awaitSlashDelta(t, log, "made-by-a-turn")
	desc := slashRow(t, SlashCatalog{Items: s.Snapshot().Slash}, "made-by-a-turn").Description
	if strings.Contains(desc, key) || desc != "holds "+redact.Marker {
		t.Fatalf("the skill's description %q was not redacted", desc)
	}
}

// TestALoadInstallsTheCatalogAndArmsTheScanner is the load path's half: the
// load's install delta — inside the replay bracket, stamped replayed —
// restates the catalog built over the restored modes (grok's fallback, so
// /agent is "default") with the skills Start found, and once Start has
// returned the scanner is armed: an attach's request is a walk, and a skill
// written since is one delta.
func TestALoadInstallsTheCatalogAndArmsTheScanner(t *testing.T) {
	ws := t.TempDir()
	skills := filepath.Join(ws, ".grok", "skills")
	writeSlashSkill(t, skills, "restored-with", "on disk at the load")
	s := newLoadSession(t, "grok-load", func(o *Options) { o.Workspace = ws })
	log := collect(t, s)
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The collector reads on a goroutine of its own: wait for the end bracket,
	// which the install precedes.
	awaitEvent(t, log, "the load's end bracket", func(ev Event) bool {
		return ev.Type == EventReplay && ev.Replay != nil && ev.Replay.Phase == ReplayEnd
	})
	var install *StateDelta
	for _, ev := range log.snapshot() {
		if ev.Type == EventMeta && ev.Replayed && ev.State != nil && ev.State.Config != nil {
			install = ev.State
		}
	}
	if install == nil || install.Slash == nil {
		t.Fatalf("the load's install delta carries no catalog: %+v", install)
	}
	if !catalogHas(install.Slash.Items, "restored-with", SlashSourceSkill) {
		t.Fatalf("the load's catalog lacks the skill on disk: %+v", install.Slash.Items)
	}
	if agentRun := slashRow(t, *install.Slash, "agent").Run; agentRun.Setting.Value != "default" {
		t.Fatalf("/agent on a restored grok session runs %+v, want the fallback's default", agentRun)
	}

	writeSlashSkill(t, skills, "after-the-load", "written once it was up")
	s.RescanSkills()
	awaitSlashDelta(t, log, "after-the-load")
}

// keyedCatalogProvider is a provider of the shipped catalog a key can be
// stored for: the first, by id, that is not the ChatGPT plan's.
func keyedCatalogProvider(t *testing.T) string {
	t.Helper()
	cat, err := modeltable.ShippedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range slices.Sorted(maps.Keys(cat.Providers)) {
		if cat.Providers[id].Driver != modeltable.DriverChatGPT {
			return id
		}
	}
	t.Fatal("the shipped catalog has no provider a key can be stored for")
	return ""
}

// TestSkillRedactionSurvivesABrokenModelTable is astra C1 H9: a models.toml
// that is not TOML makes the model table fail to load, and the skill scan's
// redaction must not fail open with it. Every source a native session's
// redactor reads is covered on its own — a key exported under one of the
// shipped catalog's credential variables, a key stored in providers.toml (as
// `craze auth` and /connect store one), and the ChatGPT plan's token values —
// so a skill description holding any of them reaches the catalog redacted.
func TestSkillRedactionSurvivesABrokenModelTable(t *testing.T) {
	t.Setenv("CRAZE_HOME", t.TempDir())
	dir := paths.NativeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const (
		envKey    = "sk-env-0123456789abcdefghijklmnopqrstu"
		storedKey = "sk-stored-0123456789abcdefghijklmnopq"
		access    = "plan-access-0123456789abcdefghijklmno"
		refresh   = "plan-refresh-0123456789abcdefghijklmn"
	)
	t.Setenv(modeltable.CatalogEnvNames()[0], envKey)
	if err := modeltable.SetKey(dir, keyedCatalogProvider(t), storedKey); err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	writePlanTokens(t, dir, access, refresh, "inc-h9", 1)
	if err := os.WriteFile(filepath.Join(dir, modeltable.ModelsFile), []byte("this is [not toml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := modeltable.Load(dir); err == nil {
		t.Fatal("fixture: the broken models.toml loaded")
	}

	s := newTestSession(t, Options{})
	s.skillWalk = func(context.Context, Provider, string, string) []Skill {
		return []Skill{
			{Name: "env", Description: "uses " + envKey},
			{Name: "stored", Description: "uses " + storedKey},
			{Name: "access", Description: "uses " + access},
			{Name: "refresh", Description: "uses " + refresh},
		}
	}
	got := s.walkSkills(t.Context(), t.TempDir(), t.TempDir())
	if len(got) != 4 {
		t.Fatalf("the walk's skills: %+v", got)
	}
	for _, sk := range got {
		if sk.Description != "uses "+redact.Marker {
			t.Errorf("skill %s: description %q, want the key redacted", sk.Name, sk.Description)
		}
	}
}
