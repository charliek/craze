package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/charliek/craze/internal/anthropicmodels"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/journal"
)

// The model lists a native session fetches in the background (plan 044 §3.6,
// P9): the ChatGPT plan's (plan 033 §3.10, §3.12; plan 034 Q3, Q14) and the
// Anthropic account's. Each is a provider's own list in the native
// directory, which the model table reads when it loads (modeltable's
// withDiscovered and withAnthropicList), and each has a refresher of its own
// on the session — a listRefresher, its state under s.mu — so one provider's
// fetch never makes another's refresh skip, and one failing never holds
// another back.
//
// A refresh is asked for at two places: Start, once the harness is installed,
// and RefreshModels (session.models.refresh: /connect after a save, /model
// as it opens, a client's own call), which reloads the table first and asks
// on what it read (P23), so a key /connect just saved is the key a fetch is
// made with. What a provider's list is fetched for — and whether it is
// fetched at all — is its listSource: the plan's for an account signed in
// with plan usage (planSource), the Anthropic account's for the key craze's
// own Anthropic provider is funded by, an exported one included
// (anthropicSource). A list a fetch wrote is taken up by a reload of the
// session's table through the same transaction every reload goes through
// (native_models.go, Q14 c), which the list file's stamp, one of the
// table's inputs, also triggers at the next turn's start — never swapped in
// here. Close cancels every fetch (s.done) and waits for every goroutine a
// refresher started (nativeSession.listWorkers).

// listSource is one provider's model list as a session fetches it: what the
// list is fetched for (id), the fetch, and the journal diag a failure is
// noted as. A source is made outside s.mu — the plan's reads its
// registration, the Anthropic account's reads a table — and fetched only on
// its refresher's goroutine.
type listSource interface {
	// id names what the list is fetched for — the account a session's
	// sign-in is, the key a table is funded by — so a refresh asked for
	// another while a fetch is in flight is run once that returns, and a
	// failure's backoff holds for its own id alone. It is never a secret: a
	// key is named by its id (modeltable.AnthropicKeyID).
	id() string
	// fetch fetches the list again when it is stale — over a day old,
	// missing, another account's or another key's — holding the list's own
	// lock across its check, the request and the write, so hosts opening at
	// once fetch it once, bounded by its own timeout; and answers whether it
	// wrote one. Its errors name no secret (the fetchers' own rule); the
	// note is redacted besides (listRefreshNote).
	fetch(ctx context.Context) (wrote bool, err error)
	// diag is the journal diag a failed fetch is noted as.
	diag() string
}

// listRefresher is one provider's background fetch of its model list (P9),
// under s.mu: its own fetch in flight, its own backoff and its own done
// channel. Its zero value has none of them.
type listRefresher struct {
	// done is closed when the latest run of fetches has returned — Start's,
	// or one RefreshModels started, with every fetch it ran after it
	// (pending) — and nil when none was started: what a caller waiting for a
	// refresh's fetch waits for. Close joins every run's goroutines instead
	// (nativeSession.listWorkers), after s.done has cancelled them.
	done chan struct{}
	// running is the source of the fetch in flight, nil when none is: a
	// refresh asked meanwhile for its id joins it rather than starting a
	// second. pending is a refresh asked for another id meanwhile — a key
	// saved while the old key's fetch runs — fetched once the one in flight
	// returns; nil when there is none.
	running, pending listSource
	// decided is the table reading (modelsWatch.freshSeq) the newest
	// submission was decided on (submitListLocked): a submission decided on
	// an older one — a caller that read the table before a key was saved,
	// and reached here after the refresh that read the saved key — is stale,
	// and changes nothing, so pending always holds what the newest reading
	// asked for (C5 review 1).
	decided uint64
	// failed is when the latest fetch for each id failed — an account's or a
	// key's, for as long as its backoff runs (backedOffLocked prunes it) — so
	// a refresh for that id within listRetryBackoff starts no fetch, and
	// neither does a pending one (plan 034 C5r, r12 #3b; C5 review 2). Another
	// id's fetch leaves it be: a key replacing one the account refused is
	// fetched with at once, and going back to the refused one waits its
	// backoff out. A fetch for the id that succeeds, or finds the list
	// current, ends it.
	failed map[string]time.Time
}

// listRetryBackoff is how long after a failed fetch of a provider's model
// list no other is started for the same account or key (submitListLocked;
// plan 034 C5r, r12 #3b): long enough that a client asking one session in a
// loop costs the account one request per half minute, short enough that a
// person retrying after fixing what failed — a network back, a sign-in
// finished — waits for no more than that. It is per session and per
// provider, as listRefresher is (plan 034 C4r3, r14 #c): several sessions
// open at once each keep their own, and can each make a request in the same
// half minute.
const listRetryBackoff = 30 * time.Second

// submitListLocked is the newest word on r's list: src, the fetch the table
// reading seq asks for — nil when it asks for none (nothing funds the
// provider) — submitted by Start (seq 0, the table it opened on) or a
// refresh (refreshLists). A closed session takes none, and a submission
// decided on a reading older than one already submitted is stale and
// changes nothing (C5 review 1). Otherwise, while a fetch is in flight, src
// is pending when it is for another id — fetched once that returns — and
// nothing is pending when it is for the same id (it joins the fetch in
// flight) or is nil; with none in flight, src's fetch starts in the
// background unless its id's backoff runs (backedOffLocked). A failure is
// journaled through redact, the session's redactor (listRefreshNote), and
// starts its id's backoff: joining a fetch in flight bounds how many run at
// once, the backoff how often a client calling session.models.refresh in a
// loop can make one, while the reload it asks for still runs and answers. A
// fetch writes no sign-in log (no Observe). s.mu is held: it only starts the
// goroutines, which Close joins (listWorkers).
func (s *nativeSession) submitListLocked(r *listRefresher, src listSource, seq uint64, redact func(string) string) {
	if s.closed || seq < r.decided {
		return
	}
	r.decided = seq
	if r.running != nil {
		r.pending = nil
		if src != nil && src.id() != r.running.id() {
			r.pending = src
		}
		return
	}
	if src == nil || s.backedOffLocked(r, src.id()) {
		return
	}
	r.running = src
	done := make(chan struct{})
	r.done = done
	// The run and its cancellation watch, both joined by Close: added here,
	// under s.mu with the session not closed, so every Add is before Close's
	// section and so before its Wait.
	s.listWorkers.Add(2)
	go s.fetchLists(r, src, redact, done)
}

// backedOffLocked reports whether a fetch for id failed within
// listRetryBackoff, forgetting every failure older than that. s.mu is held.
func (s *nativeSession) backedOffLocked(r *listRefresher, id string) bool {
	now := s.fetchNow()
	for k, at := range r.failed {
		if now.Sub(at) >= listRetryBackoff {
			delete(r.failed, k)
		}
	}
	_, ok := r.failed[id]
	return ok
}

// fetchLists is a refresher's goroutine: src's fetch, then the fetch pending
// as each returns — unless its id's backoff runs — until none is or the
// session has closed, closing done at the end. Each runs under a context
// Close cancels (s.done), watched by a goroutine of its own; a fetch that
// wrote a list reloads the table (reloadFetched) unless it was cancelled.
// Both goroutines are listWorkers', which Close waits for.
func (s *nativeSession) fetchLists(r *listRefresher, src listSource, redact func(string) string, done chan struct{}) {
	defer s.listWorkers.Done()
	defer s.listStage("exit")
	defer close(done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer s.listWorkers.Done()
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	for src != nil {
		wrote, err := src.fetch(ctx)
		s.listStage("fetched")
		failed := err != nil && ctx.Err() == nil
		if failed {
			s.log.Note(listRefreshNote(src.diag(), err, redact))
		}
		if wrote && ctx.Err() == nil {
			s.reloadModels(reloadFetched)
		}
		s.mu.Lock()
		switch {
		case failed:
			if r.failed == nil {
				r.failed = make(map[string]time.Time)
			}
			r.failed[src.id()] = s.fetchNow()
		case err == nil:
			delete(r.failed, src.id())
		}
		src, r.pending = r.pending, nil
		if s.closed || (src != nil && s.backedOffLocked(r, src.id())) {
			src = nil
		}
		r.running = src
		s.mu.Unlock()
	}
}

// listStage runs listSeam at stage, when a test set it.
func (s *nativeSession) listStage(stage string) {
	if seam := s.listSeam; seam != nil {
		seam(stage)
	}
}

// fetchNow is fetchClock's reading, the wall clock's when it is nil.
func (s *nativeSession) fetchNow() time.Time {
	if s.fetchClock != nil {
		return s.fetchClock()
	}
	return time.Now()
}

// The journal diags a failed background fetch of a provider's model list is
// noted as (listRefreshNote): one field, "error", naming the endpoint, the
// status and the code, never a credential — chatgptauth's errors repeat only
// the codes it knows, anthropicmodels takes the key out of every error it
// returns — and redacted by the session's redactor besides.
const (
	diagModelsRefresh          = "chatgpt_models_refresh"
	diagAnthropicModelsRefresh = "anthropic_models_refresh"
)

// listRefreshNote is the diag kind a failed background fetch is journaled
// as: err's text through the session's redactor, then made one line (plan 033
// C14r, r12 #1). A credential reaching err is not expected (the fetchers'
// own rule); the redaction is the journal boundary's own guard, as every
// other surface the session writes has one, should a wrapped error ever
// carry one. Redacted first, so the sanitiser's rewriting cannot split a
// value the redactor would have matched.
func listRefreshNote(kind string, err error, redact func(string) string) journal.DiagNote {
	return journal.DiagNote{Kind: kind, Fields: map[string]any{"error": sanitizeLine(redact(err.Error()))}}
}

// planList is the ChatGPT plan's model list for the session's sign-in
// (chatgptauth.RefreshModels, plan 033 §3.10): fetched again when it is over
// a day old, missing, another account's or fetched with another
// client_version. Its id is the sign-in's account (plan 047 D8): the first 12
// hex digits of the SHA-256 of the stored subject, as an Anthropic list's is
// of its key, so a failed fetch backs off only its own account; "" when no
// sign-in is stored.
type planList struct {
	src     *chatgptauth.TokenSource
	account string
}

func (p planList) id() string { return p.account }
func (planList) diag() string { return diagModelsRefresh }

func (p planList) fetch(ctx context.Context) (bool, error) {
	return chatgptauth.RefreshModels(ctx, p.src, chatgptauth.ModelsMaxAge, chatgptauth.FetchOptions{ClientVersion: modeltable.ChatGPTModelsClientVersion()})
}

// planSource is the plan's list for src's account when it is fetched in the
// background: for an account signed in with plan usage, since anything else
// would only fail; nil otherwise, and for no src. It reads the
// registration's and the stored sign-in's files (no refresh, no network), so
// its callers make it before s.mu.
func planSource(src *chatgptauth.TokenSource) listSource {
	if src == nil {
		return nil
	}
	if st, err := chatgptauth.ReadStatus(src.Dir()); err != nil || !st.SignedIn || !st.PlanUsage {
		return nil
	}
	return planList{src, planAccountID(src.Dir())}
}

// planAccountID is the id of the account signed in to dir: the first 12 hex
// digits of the SHA-256 of the stored subject, read without a refresh or a
// request. "" when no sign-in is stored or it cannot be read.
func planAccountID(dir string) string {
	subject, err := chatgptauth.StoredSubject(dir)
	if err != nil || subject == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:])[:12]
}

// anthropicList is the Anthropic account's model list for key, in the
// native directory dir (anthropicmodels.Refresh, plan 044 §3.6): fetched
// again when it is over a day old, missing, or another key's. Its id is the
// key's id, the one the list is bound to (P7).
type anthropicList struct{ dir, key string }

func (a anthropicList) id() string { return modeltable.AnthropicKeyID(a.key) }
func (anthropicList) diag() string { return diagAnthropicModelsRefresh }

func (a anthropicList) fetch(ctx context.Context) (bool, error) {
	return anthropicmodels.Refresh(ctx, a.dir, a.key, anthropicmodels.MaxAge)
}

// anthropicSource is the Anthropic account's list for t when it is fetched in
// the background (plan 044 §3.6 b): when t funds craze's own Anthropic
// provider through getenv — an env-only key included — with the key a load
// binds the list to (Table.AnthropicListKey), into dir, the directory t was
// read from; nil when t is nil, dir is "", or nothing funds the provider, a
// provider of the user's own named anthropic included (plan 044 X1).
func anthropicSource(t *modeltable.Table, getenv func(string) string, dir string) listSource {
	if t == nil || dir == "" {
		return nil
	}
	key, ok := t.AnthropicListKey(getenv)
	if !ok {
		return nil
	}
	return anthropicList{dir: dir, key: key.Reveal()}
}
