package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/journal"
)

// A running native session takes up models funded while it runs (plan 034
// §3.4, Q13–Q16), superseding plan 031's P8 for the picker: a key saved, the
// ChatGPT plan signed in to or out of, or the plan's list fetched again is
// offered at once, without /exit and craze -c. What does not change mid-session
// is P8's other half — the model it runs on keeps the client it built, and the
// keys it learns are only ever added — and everything the session froze at
// open: its prompt, its tools and the agent tool's model menu among them, its
// compaction and sub-agent settings.
//
// # The transaction
//
// A reload (reloadModels) is one transaction, and every reload and every
// switch takes turns on one lock, modelsMu, which is never taken under s.mu or
// any lock of the harness's, and under which nothing slow but the files'
// reading happens — no network, no turn:
//
//  1. The five inputs are stat'ed, through their path helpers, before anything
//     is read: providers.toml, models.toml, the plan's registration
//     (auth/chatgpt-client.json), its model list (chatgpt-models.json) and its
//     sign-in (auth/chatgpt.json, stat only — Resolve reads its state). If none
//     changed since the last reload that was published — and the list was not
//     just fetched — the answer is ModelsCurrent.
//  2. Outside every lock of the adapter's and the harness's: the table is
//     loaded; the running model's entry is carried into it unchanged (Q16,
//     modeltable.Table.Carry); every provider whose key the session cannot take
//     — one inside its frozen prompt, tools or plan path — is withheld, with
//     one note (A22, WithholdFrozen); the session is taught every key the
//     table holds (LearnKeys), so a model it offers is one whose key it
//     redacts; and the picker's list (Choices, with the model memory read now,
//     one sealed reading of the environment and the running model), the
//     efforts and the matcher are built from it.
//  3. A generation is taken: the list's revision, monotonic.
//  4. While the session is not started, loading or closed, nothing is
//     published: a load's replay bracket holds the reload until its end
//     bracket is out (A19), when it is taken up.
//  5. The harness takes the table (SetTable). While a turn runs it refuses
//     (ErrTurnRunning): the reload is owed, and the turn's end — every end: a
//     success, a cancel, a withdrawal, a failure, a wake's, a /compact's —
//     takes it up, before the next turn's claim (owed below).
//  6. In one s.mu section, the adapter's table, list, efforts and revision
//     change together, and the StateDelta.Catalog saying so — with the effort
//     option, should the running model's have changed — is enqueued in that
//     section. A generation older than the one published is dropped.
//
// Only a reload that published records the stamps it took in step 1, so one
// that failed or is owed is made again. A failed Load records nothing and is
// one value-free journal note (models_reload), said once until it changes.
//
// SetModel and SetConfig take modelsMu across their reading of the list and
// the harness's switch, so a switch is judged against the very list the
// session offers: the picker never offers what SetModel refuses (A24).
//
// # Triggers (Q14)
//
//   - a turn's start — a prompt's, a /compact's, a wake's — beside the key
//     learning (native_keys.go), before the harness's claim;
//   - RefreshModels (ModelsRefresher), which the wire's session.models.refresh
//     calls, and which also starts the age- and version-aware fetch of the
//     plan's list (refreshModelsLocked);
//   - that fetch's completion, which reloads through the same transaction and
//     never swaps anything itself;
//   - a turn's or a load's end, for a reload that was owed.

// reloadReason is why reloadModels runs.
type reloadReason int

const (
	// reloadTurn is a turn's start (Q14 a).
	reloadTurn reloadReason = iota
	// reloadAsked is RefreshModels (Q14 b).
	reloadAsked
	// reloadFetched follows a fetch of the plan's list that wrote one (Q14
	// c): it reloads even when the stamps match, since a list rewritten within
	// one tick of the file system's clock could leave them so.
	reloadFetched
	// reloadOwed takes up a reload a turn or a load held back (step 5).
	reloadOwed
)

// diagModelsReload is the journal diag a reload whose table would not load is
// noted as (step 1's failure): its one field, "error", is modeltable's error,
// which names a file and a line, table or key and never a value, redacted and
// made one line besides.
const diagModelsReload = "models_reload"

// modelsWatch is what the reloads keep between them, guarded by modelsMu —
// but for getenv and reloadable, set by open(), for a table it loaded, before
// the harness is installed and only read after.
type modelsWatch struct {
	// getenv is the environment the harness was opened with, before open()
	// sealed it (Options.Getenv after the seam; nil is os.Getenv): each
	// reload seals its own reading of it (sealedGetenv).
	getenv func(string) string
	// reloadable is false for a session whose table a test's seam handed in
	// rather than open() loading it from its Home: a reload would replace the
	// test's table with whatever the directory holds.
	reloadable bool
	// stamps are the five inputs as stat'ed before the table published last
	// was read — the one open() loaded, at first — and read says there is
	// one.
	stamps modelStamps
	read   bool
	// gen numbers the reloads that got as far as a table to publish: the
	// revision of the list each would publish.
	gen uint64
	// problem is the last failure journaled, so one that stays is said once.
	problem string
	// withheld are the providers already said to be withheld (A22), so each
	// is said once per session.
	withheld map[string]bool
}

// modelStamps are the five inputs' stamps, in the order modelInputs names
// them.
type modelStamps [5]inputStamp

// inputStamp is one input's: its fileStamp when it is there, and which of
// "missing" and "cannot be stat'ed" it is when it is not.
type inputStamp struct {
	st      fileStamp
	present bool
	failed  bool
}

// modelInputs are the five files a model table depends on, in home: the two
// the table is read from, and the ChatGPT plan's registration, its model list
// and its sign-in, which fund the plan's models (Resolve reads the sign-in's
// state; a sign-out removes only the last).
func modelInputs(home string) [5]string {
	return [5]string{
		filepath.Join(home, modeltable.ProvidersFile),
		filepath.Join(home, modeltable.ModelsFile),
		chatgptauth.ClientFile(home),
		chatgptauth.ModelsFile(home),
		chatgptauth.TokenFile(home),
	}
}

// statModelInputs stamps the five inputs, following a symlink as Load does.
func statModelInputs(home string) modelStamps {
	var out modelStamps
	for i, path := range modelInputs(home) {
		st, err := stampOf(path) // the zero stamp when err is not nil
		out[i] = inputStamp{st: st, present: err == nil, failed: err != nil && !errors.Is(err, fs.ErrNotExist)}
	}
	return out
}

// reloadModels is one reload (the file's comment), and what it came to. It
// takes modelsMu, so it must not be called with it — nor s.mu — held.
func (s *nativeSession) reloadModels(reason reloadReason) ModelsStatus {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	return s.reloadLocked(reason)
}

// reloadLocked is reloadModels with modelsMu held.
func (s *nativeSession) reloadLocked(reason reloadReason) ModelsStatus {
	s.mu.Lock()
	hs, home, prev := s.hs, s.home, s.table
	closed, loading := s.closed, s.loading
	if !closed && loading {
		// Step 4: the load's end bracket takes it up (start).
		s.reloadOwed = true
	}
	s.mu.Unlock()
	switch {
	case closed:
		return ModelsFailed
	case loading:
		return ModelsPending
	case hs == nil:
		return ModelsFailed
	case !s.models.reloadable:
		return ModelsCurrent
	}

	// Step 1: the stamps, before anything is read.
	stamps := statModelInputs(home)
	if s.models.read && stamps == s.models.stamps && reason != reloadFetched {
		return ModelsCurrent
	}
	if seam := s.reloadSeam; seam != nil {
		seam("stamped")
	}

	// Step 2, outside every lock of the adapter's and the harness's.
	t, err := modeltable.Load(home)
	if err != nil {
		s.noteReloadFailed(hs, err)
		return ModelsFailed
	}
	s.models.problem = ""
	getenv, release := sealedGetenv(s.models.getenv)
	defer release()
	current, _ := hs.Current()
	t.Carry(prev, current)
	s.noteWithheld(hs, t.WithholdFrozen(getenv, hs.FrozenKey, current))
	keys, err := t.Keys(getenv)
	if err != nil {
		// Unreachable for a table Load returned: it refuses an inline key Keys
		// would.
		s.noteReloadFailed(hs, err)
		return ModelsFailed
	}
	// The keys are learned before anything offers a model they fund. None is
	// inside a frozen surface — WithholdFrozen took those out — but for one
	// stored for the running model's own provider, which the turn-start look
	// at providers.toml has already put the session in its refusal state for
	// (native_keys.go).
	_, _ = hs.LearnKeys(keys)
	infos := choiceInfos(t.Choices(modeltable.ReadRecent(home), getenv, current))
	efforts := make(map[string][]string, len(t.Models))
	for alias, m := range t.Models {
		efforts[alias] = slices.Clone(m.Efforts)
	}
	match := nativeModelMatcher(t)

	// Step 3.
	s.models.gen++
	gen := s.models.gen
	if seam := s.reloadSeam; seam != nil {
		seam("computed")
	}

	// Step 5. A turn that holds the harness makes the reload owed: its end
	// takes it up. The claim is read in the section that marks it owed, and
	// a turn's end reads the mark in the section that releases its claim
	// (prompt, endWake), so either this marking is before that release — and
	// the end takes it up — or the claim is already released, the harness's
	// turn with it, and the swap is tried again here. A turn begun meanwhile
	// holds the claim, and its end takes the reload up.
	for attempt := 0; ; attempt++ {
		err = hs.SetTable(t, match)
		if !errors.Is(err, harness.ErrTurnRunning) {
			break
		}
		s.mu.Lock()
		owed := (s.claimed && !s.closed) || attempt == 2
		if owed {
			s.reloadOwed = true
		}
		s.mu.Unlock()
		if owed {
			return ModelsPending
		}
	}
	if err != nil {
		return ModelsFailed // closed
	}

	// Step 6.
	s.mu.Lock()
	if s.closed || gen <= s.snap.CatalogRevision {
		s.mu.Unlock()
		return ModelsFailed
	}
	s.table, s.efforts = t, efforts
	s.snap.Models, s.snap.CatalogRevision = infos, gen
	s.reloadOwed = false
	before := s.snap.Config
	s.refreshCurrentLocked()
	delta := &StateDelta{Catalog: &CatalogState{Models: append([]ModelInfo(nil), infos...), Revision: gen}}
	if !configEqual(before, s.snap.Config) {
		delta.Config = &ConfigState{Options: cloneConfig(s.snap.Config)}
	}
	s.enqueueDeltaLocked("", Event{}, delta)
	s.mu.Unlock()
	s.models.stamps, s.models.read = stamps, true
	return ModelsApplied
}

// noteReloadFailed journals a table that would not load (diagModelsReload),
// once until the reason changes. modelsMu is held.
func (s *nativeSession) noteReloadFailed(hs *harness.Session, err error) {
	msg := sanitizeLine(hs.Redact(err.Error()))
	if msg == s.models.problem {
		return
	}
	s.models.problem = msg
	s.log.Note(journal.DiagNote{Kind: diagModelsReload, Fields: map[string]any{"error": msg}})
}

// noteWithheld says, once per provider per session, that a provider is not
// offered because its key is inside what the session has already sent (A22):
// one line on the session's diagnostics, naming the provider and never the
// key. modelsMu is held.
func (s *nativeSession) noteWithheld(hs *harness.Session, ids []string) {
	for _, id := range ids {
		if s.models.withheld[id] {
			continue
		}
		if s.models.withheld == nil {
			s.models.withheld = make(map[string]bool)
		}
		s.models.withheld[id] = true
		s.note(nativeSafe{red: hs.Redact}.line(fmt.Sprintf(
			"provider %q is not offered in this session: its API key is in what this session sends with every request; a new session offers it", id)))
	}
}

// configEqual reports whether two config catalogs say the same thing.
func configEqual(a, b []ConfigOption) bool {
	return slices.EqualFunc(a, b, func(x, y ConfigOption) bool {
		return x.ID == y.ID && x.Name == y.Name && x.Category == y.Category && x.Type == y.Type &&
			x.Current == y.Current && slices.Equal(x.SelectValues, y.SelectValues)
	})
}

// takeOwedLocked reports whether a reload is owed, and clears the mark: a
// turn's end calls it in the section that releases the claim (step 5's
// comment says why there), and takes the reload up once s.mu is released.
// s.mu is held.
func (s *nativeSession) takeOwedLocked() bool {
	owed := s.reloadOwed
	s.reloadOwed = false
	return owed
}

// reloadFlushed is a reload at a turn's edge and, when it published, the
// delta flushed with it, since what a turn publishes does not wait for the
// outbox (prompt's behindAWake). It holds no lock. Its two reasons:
//
//   - reloadTurn, a turn's own reload (Q14 a), beside its key learning and
//     before the harness's claim: a model funded since the last turn is
//     offered from this turn on, and a sub-agent this turn starts can name
//     it; the list is out before the turn says anything;
//   - reloadOwed, a turn's or a load's end taking up a reload that was owed,
//     before the next turn's claim, whose own start reloads too: the list a
//     turn's end left is the one a client folds before the next turn's first
//     event.
func (s *nativeSession) reloadFlushed(reason reloadReason) {
	if s.reloadModels(reason) == ModelsApplied {
		_ = s.log.Flush(context.Background(), s.done)
	}
}

// RefreshModels is ModelsRefresher (the file's comment, Q14 b): the fetch of
// the plan's list started in the background when the account is signed in
// with plan usage, then the reload, now. ctx is not read: the reload reads
// local files and the fetch has its own bound and Close's.
func (s *nativeSession) RefreshModels(_ context.Context, nativeDir string) (ModelsRefresh, error) {
	s.mu.Lock()
	hs, home, signIn := s.hs, s.home, s.signIn
	closed, loading := s.closed, s.loading
	s.mu.Unlock()
	switch {
	case closed:
		return ModelsRefresh{}, fmt.Errorf("agent: session closed")
	case hs == nil && !loading:
		return ModelsRefresh{}, fmt.Errorf("agent: session not started")
	}
	var out ModelsRefresh
	if nativeDir != "" {
		same := home != "" && filepath.Clean(nativeDir) == filepath.Clean(home)
		out.SameDir = &same
	}
	if fetchesPlanList(signIn) {
		s.mu.Lock()
		s.refreshModelsLocked(signIn, hs.Redact)
		s.mu.Unlock()
	}
	out.Status = s.reloadModels(reloadAsked)
	s.mu.Lock()
	out.Revision = s.snap.CatalogRevision
	s.mu.Unlock()
	return out, nil
}

var _ ModelsRefresher = (*nativeSession)(nil)
