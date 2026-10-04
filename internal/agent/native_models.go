package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
// any lock of the harness's. What runs under it is local: the files' reading
// — a reload's, and a switch's model memory — and a switch's building of its
// model's client (harness SetModel; r9 #1). No network, no turn:
//
//  1. The five inputs are stat'ed, through their path helpers, before anything
//     is read: providers.toml, models.toml, the plan's registration
//     (auth/chatgpt-client.json), its model list (chatgpt-models.json) and its
//     sign-in (auth/chatgpt.json, stat only — Resolve reads its state). If none
//     changed since the last reload that was published — and the list was not
//     just fetched, nor a shadowing carry just left (reloadLeft), nor either
//     since the last reload that published (forced) — the answer is
//     ModelsCurrent.
//  2. Outside every lock of the adapter's and the harness's: the table is
//     loaded; the running model's entry is carried into it unchanged (Q16,
//     modeltable.Table.Carry); every provider whose key the session cannot take
//     — one inside its frozen prompt, tools or plan path — is withheld
//     (A22, WithholdFrozen); its keys are read (Keys); and the picker's list
//     (Choices, with the model memory read now, one sealed reading of the
//     environment and the running model, held to the catalog's bounds:
//     boundCatalog), the efforts and the matcher are built from it.
//  3. A generation is taken: the list's revision, monotonic.
//  4. While the session is not started, loading or closed, nothing is
//     published: a load's replay bracket holds the reload until its end
//     bracket is out (A19), when it is taken up.
//  5. While a turn holds the session's claim — from its Begin until its
//     ending is out — the reload is owed, decided before the harness is asked
//     (r9 #4): the turn's end — every end: a success, a cancel, a withdrawal,
//     a failure, a wake's, a /compact's — takes it up once the ending is out,
//     before the next turn's claim (owed below). The turn's own start reload
//     (reloadTurn) is the one that goes on under the claim, before the
//     harness's turn begins — and, for a wake, before its opening bracket
//     (plan 034 C4r2, r11 #3). Otherwise the harness takes the table
//     (SetTable), and only then is the session taught every key the table
//     holds (LearnKeys), so a model it offers is one whose key it redacts,
//     and an owed reload learns nothing until it is taken up (r9 #7a); a
//     withheld provider gets its one note.
//  6. In one s.mu section, the adapter's table, list, efforts and revision
//     change together, and the StateDelta.Catalog saying so — with the effort
//     option, should the running model's have changed — is enqueued in that
//     section, and marked for the next turn's edge to flush
//     (catalogQueued). A generation older than the one published is dropped.
//
// Only a reload that published records the stamps it took in step 1, so one
// that failed or is owed is made again — and only one that published clears
// forced, so an unstamped reading that failed or is owed is made again so
// (plan 034 C4r5, r16 #1). A failed Load records nothing and is
// one value-free journal note (models_reload), said once until it changes.
//
// SetModel and SetConfig take modelsMu across their reading of the list and
// the harness's switch, so a switch is judged against the very list the
// session offers: the picker never offers what SetModel refuses (A24). A
// switch to the model the session runs on builds it again while the table
// resolves it, and changes nothing when it does not (the carry: its client is
// kept); a switch away from a model the table no longer funds takes it off
// the list, in the switch's own delta (r9 #1). A switch away from a carry
// that stood over another entry of the files' own (modeltable.Table.Shadowed:
// another account's model under its alias, other efforts, no entry at all)
// reloads, in the same modelsMu section, whatever the stamps say
// (reloadLeft; plan 034 C4r4, r15 #3): nothing changed on disk — the files'
// entry was there all along, under the carry — so no stamp would ever take it
// up, and the list would go on without it (or with the carry's) until some
// other file moved. Under a turn's claim that reload is owed, as any is, and
// read regardless of the stamps when it is taken up (forced); one that fails
// — a models.toml it cannot read, which no stamp records either — leaves
// that reading owed to the next trigger, a refresh's or a turn's (forced
// again; plan 034 C4r5, r16 #1). The switch's own delta has already taken an
// unfunded carry off the list meanwhile, so the list never offers what a
// switch would refuse.
//
// # Triggers (Q14)
//
//   - a turn's start — a prompt's, a /compact's, a wake's — beside the key
//     learning (native_keys.go), before the harness's claim (and a wake's
//     opening);
//   - RefreshModels (ModelsRefresher), which the wire's session.models.refresh
//     calls, and which also starts the age- and version-aware fetch of the
//     plan's list (refreshModelsLocked);
//   - that fetch's completion, which reloads through the same transaction and
//     never swaps anything itself;
//   - a switch away from a carry that shadowed the files' own entry
//     (reloadLeft, above);
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
	// reloadLeft follows a switch away from a model whose entry the table
	// held as a carry over another of the files' own
	// (modeltable.Table.Shadowed; plan 034 C4r4, r15 #3): it reloads even
	// when the stamps match — they do, since the files' entry is not new —
	// so the files' entry, no longer the running model's, replaces the
	// carry in the list.
	reloadLeft
)

// unstamped reports whether a reload for r reads the files whatever their
// stamps say: one following a fetch of the plan's list (reloadFetched) or a
// switch away from a shadowing carry (reloadLeft). Held back or failed, it
// makes the reloads after it do the same until one publishes (forced).
func (r reloadReason) unstamped() bool {
	return r == reloadFetched || r == reloadLeft
}

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
	// forced says a reload that reads the files whatever their stamps say —
	// a fetched list's, or one leaving a shadowing carry (unstamped) — began
	// and none has published since: held back by a turn or a load, or failed
	// on the way — a Load error among them, which records no stamp (plan 034
	// C4r5, r16 #1) — so every reload reads them so until one publishes. It
	// is set as such a reload begins and cleared only where one publishes:
	// one attempt per trigger, never a loop of its own.
	forced bool
	// problem is the last failure journaled, so one that stays is said once.
	problem string
	// withheld are the providers already said to be withheld (A22), so each
	// is said once per session.
	withheld map[string]bool
	// cut is what the catalog's bounds cut from the list published last
	// (noteCatalogCut), so a cut that stays is said once.
	cut catalogCut
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
	// An unstamped reading is owed from here until a reload publishes
	// (forced): whatever stops this one — a load's bracket, a turn's claim,
	// a Load that fails — leaves it for the next.
	if reason.unstamped() {
		s.models.forced = true
	}
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
		return ModelsPending // owed (step 4)
	case hs == nil:
		return ModelsFailed
	case !s.models.reloadable:
		return ModelsCurrent
	}

	// Step 1: the stamps, before anything is read. A fetched list, or a
	// shadowing carry left behind, is read whatever they say (forced, set
	// above), and so is either one held back or failed since, as its own
	// reload would have read it.
	stamps := statModelInputs(home)
	if s.models.read && stamps == s.models.stamps && !s.models.forced {
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
	withheld := t.WithholdFrozen(getenv, hs.FrozenKey, current)
	keys, err := t.Keys(getenv)
	if err != nil {
		// Unreachable for a table Load returned: it refuses an inline key Keys
		// would.
		s.noteReloadFailed(hs, err)
		return ModelsFailed
	}
	infos, cut := choiceInfos(t.Choices(modeltable.ReadRecent(home), getenv, current), current)
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

	// Step 5. A turn's claim — a prompt's or a wake's, from its Begin to the
	// section that releases it, after its ending is out — makes the reload
	// owed, decided here, before the harness is asked: the harness's own
	// refusal ends with its turn, before the adapter's ending (EventDone) is
	// out, and a catalog published in between would reach a client ahead of
	// the ending (r9 #4). The claim is read in the section that marks the
	// reload owed, and a turn's end reads the mark in the section that
	// releases its claim (prompt, endWake), so either this marking is before
	// that release — and the end takes it up — or the claim is already
	// released and the turn's ending out. The one reload that goes on under a
	// claim is the turn's own (reloadTurn): it runs before the harness's turn
	// begins, and its catalog is flushed before the turn says anything
	// (reloadFlushed). A claim taken after this reading waits for the reload
	// at its own turn-start reload, which takes modelsMu, and flushes what
	// this one publishes (catalogQueued).
	s.mu.Lock()
	closed = s.closed
	held := !closed && s.claimed && reason != reloadTurn
	if held {
		s.reloadOwed = true
	}
	s.mu.Unlock()
	switch {
	case closed:
		return ModelsFailed
	case held:
		return ModelsPending // owed (step 5)
	}
	if seam := s.reloadSeam; seam != nil {
		seam("unclaimed")
	}
	if err := hs.SetTable(t, match); err != nil {
		if errors.Is(err, harness.ErrTurnRunning) {
			// A harness turn with no claim of the adapter's: none in this
			// adapter, whose every turn and replay runs under a claim or the
			// load's bracket — the next turn's start, or its end, takes it up.
			s.mu.Lock()
			s.reloadOwed = true
			s.mu.Unlock()
			return ModelsPending
		}
		return ModelsFailed // closed
	}
	// The keys are learned only now, by a reload the harness has taken — never
	// by one that is owed, which learns them when it is taken up (r9 #7a): a
	// turn is running then, and a key it learned that is inside the frozen
	// prompt would leave that turn sending what the session knows to be a
	// key. Nothing has used the new table yet: no turn runs (SetTable), none
	// can begin before its own turn-start reload — which waits for modelsMu —
	// and nothing offers a model of it before step 6. None is inside a frozen
	// surface — WithholdFrozen took those out — but for one stored for the
	// running model's own provider, which puts the session in its refusal
	// state, said once, as the turn-start look at providers.toml says it.
	if _, err := hs.LearnKeys(keys); errors.Is(err, harness.ErrStoredKeyFrozen) {
		s.note(nativeSafe{red: hs.Redact}.line(fmt.Sprintf(
			"a key stored in %s since this session started appears in its frozen prompt; every turn from now on is refused — start a new session",
			filepath.Join(home, modeltable.ProvidersFile))))
	}
	s.noteWithheld(hs, withheld)

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
	s.catalogQueued = true
	s.mu.Unlock()
	s.models.stamps, s.models.read, s.models.forced = stamps, true, false
	s.noteCatalogCut(hs, home, cut)
	return ModelsApplied
}

// The catalog's bounds (plan 034 C5r, r12 #6). The list a native session
// offers is mandatory state wherever it travels — the info document's
// catalogs.models, the catalog section of a state delta, a snapshot's and
// session.state's settings.catalog, which the snapshot's per-item cap never
// cuts — and it comes from files the owner edits by hand, which the model
// table reads whole: a model named with megabytes of text would make its
// catalog event an omitted record (a reset for every attached client) and
// every later snapshot too large to attach with. So the list is held to
// these where it is built (choiceInfos), for every road at once:
//
//   - a model's name — display text only — to catalogNameMax bytes, cut on a
//     rune boundary and ended with an ellipsis: the bound chatgptauth holds
//     the plan's display names to, and a sub-agent row's model to
//     (subagentModelCap);
//   - a model whose id is over catalogIDMax bytes is not offered at all,
//     since an id cut short would be one no session.set could name. The model
//     table refuses such an alias as it loads (modeltable.MaxAliasLen, plan
//     034 C4r3, r14 #a) — a model left off its own catalog while it runs
//     could not be reselected — so this is a defence for a table built
//     otherwise; no real alias comes near it (the plan's slugs are at most
//     64 bytes);
//   - the list to its first catalogModelsMax models, in the picker's order,
//     the running model kept among them (in the last place, when its own
//     would be past the cut): a picker always has the current model's row to
//     select (plan 031 P7). 512 is the hub roster's row bound, and far above
//     any real table — the shipped catalog and the plan's list together are
//     a few dozen.
//
// Within them the list encodes to at most about 1.2 MiB — every id and name
// at its bound in characters JSON escapes to six bytes — under the 8 MiB
// record limit and the 4 MiB default snapshot budget. What a cut took is said
// once, value-free (noteCatalogCut).
const (
	catalogNameMax   = 128
	catalogIDMax     = modeltable.MaxAliasLen
	catalogModelsMax = 512
)

// catalogCut counts what boundCatalog cut: names shortened, models left off
// for an id over the bound, and models left off past the list's bound.
type catalogCut struct{ names, ids, over int }

// boundCatalog is infos within the catalog's bounds (above), current the
// model the session runs on, and what it cut. infos is not changed.
func boundCatalog(infos []ModelInfo, current string) ([]ModelInfo, catalogCut) {
	var cut catalogCut
	out := make([]ModelInfo, 0, len(infos))
	at := -1 // the running model's index in out
	for _, m := range infos {
		if len(m.ID) > catalogIDMax {
			cut.ids++
			continue
		}
		if current != "" && m.ID == current {
			at = len(out)
		}
		out = append(out, m)
	}
	if len(out) > catalogModelsMax {
		cut.over = len(out) - catalogModelsMax
		if at >= catalogModelsMax {
			out[catalogModelsMax-1] = out[at]
		}
		out = slices.Clip(out[:catalogModelsMax])
	}
	for i := range out {
		if len(out[i].Name) > catalogNameMax {
			out[i].Name = truncateUTF8(out[i].Name, catalogNameMax)
			cut.names++
		}
	}
	return out, cut
}

// noteCatalogCut says what the catalog's bounds cut from the list a reload —
// or Start — publishes, in one line on the session's diagnostics: counts and
// bounds alone, never a name or an id (they are the very text that was too
// long, and the owner's own) — and the file they most likely came from, in
// home, the session's native directory. The line goes through nativeSafe,
// as every note that names a path does (plan 034 C4r3, r14 #b): a home is any
// directory CRAZE_HOME names, and one holding a newline or a terminal escape
// would otherwise write lines of its own, or controls, to the host's stderr.
// It is said once until what was cut changes: a list within its bounds clears
// it. modelsMu is held.
func (s *nativeSession) noteCatalogCut(hs *harness.Session, home string, cut catalogCut) {
	if cut == s.models.cut {
		return
	}
	s.models.cut = cut
	if cut == (catalogCut{}) {
		return
	}
	models := func(n int) string {
		if n == 1 {
			return "1 model"
		}
		return fmt.Sprintf("%d models", n)
	}
	var parts []string
	if cut.names > 0 {
		parts = append(parts, fmt.Sprintf("%s with a name over %d bytes, cut to it", models(cut.names), catalogNameMax))
	}
	if cut.ids > 0 {
		parts = append(parts, fmt.Sprintf("%s with an id over %d bytes, not offered", models(cut.ids), catalogIDMax))
	}
	if cut.over > 0 {
		parts = append(parts, fmt.Sprintf("%s past the first %d, not offered", models(cut.over), catalogModelsMax))
	}
	s.note(nativeSafe{red: hs.Redact}.line("the model list this session offers is held to its bounds: " + strings.Join(parts, "; ") +
		" — see " + filepath.Join(home, modeltable.ModelsFile)))
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

// reloadFlushed is a reload at a turn's edge and, when a catalog is queued
// — its own, or one a reload or a switch published since the last turn's
// edge (catalogQueued) — the outbox flushed with it, since what a turn
// publishes does not wait for the outbox (prompt's behindAWake). It holds no
// lock. Its two reasons:
//
//   - reloadTurn, a turn's own reload (Q14 a), beside its key learning and
//     under the turn's claim, before the harness's turn begins: a model
//     funded since the last turn is offered from this turn on, and a
//     sub-agent this turn starts can name it; the list is out before the turn
//     says anything — a wake's opening bracket included (native_wake.go) —
//     and so is one a reload that read the claim free just before it was
//     taken published meanwhile (step 5);
//   - reloadOwed, a turn's or a load's end taking up a reload that was owed,
//     once the claim is released and the turn's ending out, before the next
//     turn's claim, whose own start reloads too: the list a turn's end left
//     is the one a client folds before the next turn's first event.
func (s *nativeSession) reloadFlushed(reason reloadReason) {
	s.reloadModels(reason)
	s.mu.Lock()
	queued := s.catalogQueued
	s.catalogQueued = false
	s.mu.Unlock()
	if queued {
		_ = s.log.Flush(context.Background(), s.done)
	}
}

// RefreshModels is ModelsRefresher (the file's comment, Q14 b): the fetch of
// the plan's list started in the background when the account is signed in
// with plan usage — unless one is in flight, or one failed within
// planListRetryBackoff (refreshModelsLocked) — then the reload, now, which
// runs and answers whatever became of the fetch. ctx is not read: the reload
// reads local files and the fetch has its own bound and Close's.
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
		same := SameNativeDir(nativeDir, home)
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

// SameNativeDir reports whether a and b name one native directory (plan 034
// §3.4, Q17's sameDir): a client's, sent with session.models.refresh, and the
// one a session reads its models from. Paths are not secrets, and the answer
// is all that leaves. Each is made absolute and cleaned; two that are then one
// string are the same. Two that still differ are the same when both exist and
// are one directory by the file system's own identity (os.SameFile: device and
// inode) — a CRAZE_HOME reached through a link, macOS's /var and
// /private/var, and a spelling that differs only in case on a case-insensitive
// volume, which no comparison of strings, symlinks resolved or not, can tell
// (plan 034 C5r, r12 #4). A path that does not exist on this machine is
// compared as cleaned alone. "" is no directory, and the same as none.
func SameNativeDir(a, b string) bool { return sameNativeDir(a, b, os.Stat) }

// sameNativeDir is SameNativeDir with the stat it judges identity by: os.Stat
// in production, a test's own to stand in for a case-insensitive volume.
func sameNativeDir(a, b string, stat func(string) (fs.FileInfo, error)) bool {
	if a == "" || b == "" {
		return false
	}
	ca, errA := filepath.Abs(a)
	cb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		ca, cb = filepath.Clean(a), filepath.Clean(b)
	}
	if ca == cb {
		return true
	}
	sa, errA := stat(ca)
	sb, errB := stat(cb)
	return errA == nil && errB == nil && os.SameFile(sa, sb)
}
