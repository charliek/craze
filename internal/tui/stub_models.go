package tui

import (
	"context"
	"errors"
	"slices"

	"github.com/charliek/craze/internal/agent"
)

// RefreshingStub is the Stub as a session that takes up models funded while
// it runs (agent.ModelsRefresher; plan 034 §3.4, Q17): what a native session
// is to the engine, the wire and a client — the capability modelsRefresh, the
// method session.models.refresh, the catalog section on the stream — with no
// model table behind it. Every other method is the Stub's own, the optional
// interfaces engine.New looks for included, promoted through the embedded
// pointer (as the fake host's gatedStart and wire_test.go's heldStub are).
//
// A plain Stub is not one: it stays an ACP-like session, so a host over it
// says nothing of modelsRefresh and refuses the method — every golden and
// wire fixture from before the method, byte for byte. A test opts in by
// handing the engine RefreshingStub{Stub: s} and stages what the next refresh
// finds with StageModels.
type RefreshingStub struct{ *Stub }

// RefreshModels is the native session's contract (agent.ModelsRefresher) on
// the Stub's own state (refreshModels).
func (r RefreshingStub) RefreshModels(_ context.Context, nativeDir string) (agent.ModelsRefresh, error) {
	return r.refreshModels(nativeDir)
}

var _ agent.ModelsRefresher = RefreshingStub{}

// stubModels is what RefreshingStub's refresh reads and writes, under the
// Stub's mu: the model list a refresh will find (staged: the files changed),
// whether one found it while a turn ran (pending, taken up as that turn ends,
// as a native session's owed reload is), the native directory the session
// "reads" (SetNativeDir: what sameDir compares with), and a refresh armed to
// fail (FailNextRefresh).
type stubModels struct {
	staged   []agent.ModelInfo
	isStaged bool
	pending  bool
	failNext bool
	dir      string
}

// StageModels is a change to what the session reads its models from — a key
// saved, a plan signed in to — as the next refresh will find it: models, ranks
// and all, replacing Snapshot.Models when it is taken up. It publishes
// nothing: a refresh does (RefreshingStub.RefreshModels), or the end of the
// turn that held one back.
func (s *Stub) StageModels(models []agent.ModelInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models.staged, s.models.isStaged = slices.Clone(models), true
}

// SetNativeDir is the native directory the session reads its models from, as
// a refresh's sameDir compares a client's with (agent.SameNativeDir); "" (the
// default) is none, which no client's matches.
func (s *Stub) SetNativeDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models.dir = dir
}

// FailNextRefresh makes the next refresh answer failed — the files could not
// be read as a model table — leaving the list and anything staged as they are.
func (s *Stub) FailNextRefresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models.failNext = true
}

// errStubClosed is a refresh of a closed Stub: the native session's "session
// closed", the one error the contract has once a session has started.
var errStubClosed = errors.New("stub: session closed")

// refreshModels is the native session's RefreshModels on the Stub's state: an
// error once closed; failed when armed to; current when nothing is staged;
// pending while a turn is claimed or open — the staged list is taken up as
// that turn ends (adoptPendingModelsLocked); otherwise applied — the staged
// list replaces Snapshot.Models at the next revision, and the catalog delta
// saying so is enqueued in the same section, as the native session's step 6
// enqueues it (committed by the log's drainer; the server's reply barrier
// waits for it). Revision is the list's after the call; SameDir is set when
// nativeDir is.
func (s *Stub) refreshModels(nativeDir string) (agent.ModelsRefresh, error) {
	select {
	case <-s.closed:
		return agent.ModelsRefresh{}, errStubClosed
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out agent.ModelsRefresh
	if nativeDir != "" {
		same := agent.SameNativeDir(nativeDir, s.models.dir)
		out.SameDir = &same
	}
	switch {
	case s.models.failNext:
		s.models.failNext = false
		out.Status = agent.ModelsFailed
	case !s.models.isStaged:
		out.Status = agent.ModelsCurrent
	case s.claimed || s.inPrompt:
		s.models.pending = true
		out.Status = agent.ModelsPending
	default:
		s.applyStagedModelsLocked()
		out.Status = agent.ModelsApplied
	}
	out.Revision = s.snap.CatalogRevision
	return out, nil
}

// applyStagedModelsLocked takes up the staged list: Snapshot.Models and its
// revision move together, and the catalog delta carrying both is enqueued in
// the same section. s.mu is held.
func (s *Stub) applyStagedModelsLocked() {
	s.snap.Models = s.models.staged
	s.snap.CatalogRevision++
	s.models.staged, s.models.isStaged, s.models.pending = nil, false, false
	s.enqueueDeltaLocked("", &agent.StateDelta{Catalog: &agent.CatalogState{
		Models: slices.Clone(s.snap.Models), Revision: s.snap.CatalogRevision,
	}})
}

// adoptPendingModelsLocked is a turn's end taking up the refresh it held back
// (pending), as a native session takes its owed reload up at the harness's
// !running edge: the delta follows the turn's EventDone. s.mu is held.
func (s *Stub) adoptPendingModelsLocked() {
	if s.models.pending && s.models.isStaged {
		s.applyStagedModelsLocked()
	}
	s.models.pending = false
}
