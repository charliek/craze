package chatgptauth

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// now is this package's clock: a seam for tests, time.Now in production. A
// TokenSource copies it when it is made.
var now = time.Now

// freshFor is how much life an access token must have left to be handed out
// without a look under the lock (P20): less, and the next request refreshes
// it — unless earliest_refresh_at says not yet and it still works.
const freshFor = 5 * time.Minute

// retireAfter is how long a replaced token value stays in Values (P35): an
// access token for this long after its expiry, a refresh or id token after
// its rotation, so the scrubber still recognizes it in a late error — and
// then it leaves, so a long-lived host's set stays small.
const retireAfter = time.Hour

// refreshTimeout bounds a refresh's request (plan 033 §3.10: 30 s).
const refreshTimeout = 30 * time.Second

// writeTries is how often a rotated record's write is tried before the
// failure is surfaced (plan 033 §3.10: the write, then twice more).
const writeTries = 3

// TokenSource is the bearer for every ChatGPT plan request a process sends
// (plan 033 §3.10, P20): one per process per directory (Source), shared by
// every session's steps, its sub-agents and its summarizer. It implements
// llm.Auth — Token, Invalidate, Values — structurally, so the harness knows
// nothing of it beyond that interface (P31).
//
// The token file is the truth, and other processes change it: a peer's
// refresh, a sign-in, a sign-out. So Token stats it on every call and looks
// again whenever it changed, and every refresh is made under the lock after
// re-reading it, adopting a peer's newer token rather than refreshing a
// rotated one twice (which the server answers with refresh_token_reused, the
// end of the sign-in). A file that is gone is a sign-out: it is never
// recreated.
//
// Subscribers (Subscribe) learn every token value as soon as this process
// does — on a refresh before the new record is written, on an adoption
// before it is used — so a session's redactor knows a value before any tool
// could print it (P19). The usage latch (LatchUsageLimit) stops every new
// request until the next turn the person starts (P33).
type TokenSource struct {
	dir string
	now func() time.Time

	mu      sync.Mutex
	rec     *record     // the tokens in use; nil before the first load and after a sign-out
	info    os.FileInfo // the token file's stat when rec last matched it
	gen     uint64      // names rec's access token to callers; bumped on every change of rec
	dirty   bool        // rec is a refresh whose write failed: newer than the file
	base    version     // the file's version a dirty rec was refreshed from
	flight  *flight     // the look under the lock in progress, which every other caller joins
	retired []retired
	subs    map[uint64]func([]string)
	nextSub uint64
	latched bool
}

// flight is one look under the lock (TokenSource.fly), shared by every
// caller that arrives while it runs (the in-process singleflight).
type flight struct {
	done   chan struct{}
	forced bool // an Invalidate's: the token in memory was refused
	token  string
	gen    uint64
	rec    *record
	err    error
}

// retired is a replaced token value and when it leaves Values.
type retired struct {
	value string
	until time.Time
}

var (
	sourcesMu sync.Mutex
	sources   = map[string]*TokenSource{}
)

// Source is the process's token source for dir (the native directory), made
// on first use: one per process per directory, so every session of a host
// shares its singleflight, its subscribers and its latch.
func Source(dir string) *TokenSource {
	key := filepath.Clean(dir)
	if abs, err := filepath.Abs(key); err == nil {
		key = abs
	}
	sourcesMu.Lock()
	defer sourcesMu.Unlock()
	s, ok := sources[key]
	if !ok {
		s = newSource(key)
		sources[key] = s
	}
	return s
}

// newSource is a token source of its own for dir — Source's, or a test's
// stand-in for another process.
func newSource(dir string) *TokenSource {
	return &TokenSource{dir: dir, now: now, subs: map[uint64]func([]string){}}
}

// Dir is the native directory the source reads.
func (s *TokenSource) Dir() string { return s.dir }

// Token is the access token for the next request and its generation, which
// the caller hands to Invalidate when the server refuses the token (plan 033
// §3.10):
//
//  1. The token file is stat'ed. Gone is ErrSignedOut, and it stays gone.
//  2. A token in memory with more than 5 minutes left, the file unchanged
//     since, is returned.
//  3. Otherwise, once per process at a time, under the lock (waiting for it
//     within 60 s and while ctx lasts; a timeout returns the token in memory
//     if it has not expired):
//  4. the file is re-read, and a token there that is not the one in memory —
//     a peer's refresh, a new sign-in — is adopted;
//  5. a token with 5 minutes or more left, or one that has not expired
//     while earliest_refresh_at is still to come, is returned: the hint is
//     never waited on;
//  6. otherwise it is refreshed: subscribers are told the new values, the
//     new record is written (atomicfile.WriteSync), the lock released, and
//     the new token returned.
//
// A refresh the server refuses for good (an unusable refresh token, or the
// registration refused) deletes the tokens: ErrSignInAgain. A network error
// or a 5xx keeps them, and Token returns the token in memory while it has not
// expired, else the error. A refresh whose write fails keeps the new tokens
// in this process (the file is retried on later calls) and surfaces the
// failure once. While the usage latch is set, Token is ErrUsageLimited.
func (s *TokenSource) Token(ctx context.Context) (string, uint64, error) {
	tok, gen, _, err := s.token(ctx)
	return tok, gen, err
}

// Invalidate is Token's answer to a request the server refused with the
// token gen named (an HTTP 401): if the token in memory is already another
// one, nothing is done; otherwise, under the lock, a token in the file newer
// than gen is adopted, and else the token is refreshed now — whatever
// earliest_refresh_at says, and however long it has left (plan 033 §3.10).
// The caller then asks Token again.
func (s *TokenSource) Invalidate(ctx context.Context, gen uint64) error {
	for {
		s.mu.Lock()
		if s.gen != gen {
			s.mu.Unlock()
			return nil
		}
		f, own := s.join(true)
		s.mu.Unlock()
		if own {
			_, _, _, err := s.run(ctx, f)
			return err
		}
		select {
		case <-f.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		s.mu.Lock()
		replaced := s.gen != gen
		s.mu.Unlock()
		switch {
		case replaced:
			return nil
		case isContextError(f.err) && ctx.Err() == nil:
			// The flight's own caller gave up; fly again.
		case f.forced && f.err != nil:
			return f.err
		case f.forced:
			return errors.New("chatgptauth: the refused token could not be replaced")
		}
		// An ordinary look kept the refused token (it had time left): look
		// again, forced.
	}
}

// token is Token with the record the token came from, for FetchModels,
// which binds the model list to the token's account.
func (s *TokenSource) token(ctx context.Context) (string, uint64, *record, error) {
	if s.UsageLimited() {
		return "", 0, nil, ErrUsageLimited
	}
	if tok, gen, rec, err := s.fast(); err != nil || rec != nil {
		return tok, gen, rec, err
	}
	for {
		s.mu.Lock()
		f, own := s.join(false)
		s.mu.Unlock()
		if own {
			return s.run(ctx, f)
		}
		select {
		case <-f.done:
		case <-ctx.Done():
			return "", 0, nil, ctx.Err()
		}
		if isContextError(f.err) && ctx.Err() == nil {
			continue // the flight's own caller gave up; fly again
		}
		return f.token, f.gen, f.rec, f.err
	}
}

// fast is Token's steps 1 and 2. It answers a record when it returns a token,
// ErrSignedOut when the file is gone, and neither when the look under the
// lock is needed.
func (s *TokenSource) fast() (string, uint64, *record, error) {
	info, err := os.Lstat(TokenFile(s.dir))
	if errors.Is(err, fs.ErrNotExist) {
		s.signedOut()
		return "", 0, nil, ErrSignedOut
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil || s.rec == nil || s.info == nil || s.dirty || !sameStat(info, s.info) || !s.freshLocked(s.rec) {
		return "", 0, nil, nil
	}
	return s.rec.AccessToken, s.gen, s.rec, nil
}

// sameStat says the file a and b describe is unchanged between them: the
// same file (a replace is a new one), size and modification time.
func sameStat(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (s *TokenSource) freshLocked(r *record) bool {
	return r.AccessExpiresAt.Sub(s.now()) > freshFor
}

func (s *TokenSource) unexpiredLocked(r *record) bool {
	return r != nil && s.now().Before(r.AccessExpiresAt)
}

// join is the flight in progress, or a new one this caller owns. s.mu is
// held.
func (s *TokenSource) join(forced bool) (*flight, bool) {
	if s.flight != nil {
		return s.flight, false
	}
	s.flight = &flight{done: make(chan struct{}), forced: forced}
	return s.flight, true
}

// run flies f, the caller's own flight, and lands it for everyone waiting.
func (s *TokenSource) run(ctx context.Context, f *flight) (string, uint64, *record, error) {
	f.token, f.gen, f.rec, f.err = s.fly(ctx, f.forced)
	s.mu.Lock()
	s.flight = nil
	s.mu.Unlock()
	close(f.done)
	return f.token, f.gen, f.rec, f.err
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// fly is Token's steps 3 to 6 (forced: Invalidate's), under the lock. The
// lock is released before fly returns, after any write: notify, write,
// unlock, return.
func (s *TokenSource) fly(ctx context.Context, forced bool) (string, uint64, *record, error) {
	if _, err := os.Lstat(TokenFile(s.dir)); errors.Is(err, fs.ErrNotExist) {
		s.signedOut()
		return "", 0, nil, ErrSignedOut
	}
	unlock, err := lock(ctx, s.dir)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", 0, nil, ctxErr
		}
		if !forced {
			s.mu.Lock()
			rec, gen := s.rec, s.gen
			ok := s.unexpiredLocked(rec)
			s.mu.Unlock()
			if ok {
				return rec.AccessToken, gen, rec, nil
			}
		}
		return "", 0, nil, fmt.Errorf("chatgptauth: taking the sign-in lock: %w", err)
	}
	defer unlock()

	file, info, err := readRecord(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		s.signedOut()
		return "", 0, nil, ErrSignedOut
	}
	if err != nil {
		return "", 0, nil, err
	}
	s.mu.Lock()
	cur, dirty, base := s.rec, s.dirty, s.base
	s.mu.Unlock()
	switch {
	case cur != nil && dirty && file.version() == base:
		// This process's refresh is still the newest, and not on disk yet:
		// try to put it there (its failure was surfaced when it happened).
		if err := s.write(cur); err == nil {
			s.settle(cur)
		}
	case cur == nil || file.version() != cur.version():
		// The first look, a peer's refresh, or a new sign-in: adopt it.
		s.adopt(file, info)
		cur = file
		if forced && s.unexpired(cur) {
			s.mu.Lock()
			gen := s.gen
			s.mu.Unlock()
			return cur.AccessToken, gen, cur, nil
		}
	default:
		s.mu.Lock()
		s.info = info
		s.mu.Unlock()
	}
	if !forced {
		s.mu.Lock()
		gen := s.gen
		keep := s.freshLocked(cur) || (cur.EarliestRefreshAt.After(s.now()) && s.unexpiredLocked(cur))
		s.mu.Unlock()
		if keep {
			return cur.AccessToken, gen, cur, nil
		}
	}
	return s.refresh(ctx, cur, forced)
}

func (s *TokenSource) unexpired(r *record) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unexpiredLocked(r)
}

// refresh is Token's step 6, under the lock: cur's refresh token for a new
// token set.
func (s *TokenSource) refresh(ctx context.Context, cur *record, forced bool) (string, uint64, *record, error) {
	ends, err := currentEndpoints()
	if err != nil {
		return "", 0, nil, err
	}
	// The request runs to its end whatever happens to ctx: a refresh the
	// server has rotated must reach the disk, or the only valid refresh
	// token is lost (R10).
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", cur.ClientID)
	form.Set("refresh_token", cur.RefreshToken)
	form.Set("resource", resource)
	reply, sent, err := ends.postToken(rctx, "refresh", form)
	if err != nil {
		if oe := refusal(err); oe != nil && (signInAgainCodes[oe.Code] || oe.Code == codeInvalidClient) {
			s.endSignIn(cur, oe.Code == codeInvalidClient, false)
			return "", 0, nil, fmt.Errorf("%w: %w", ErrSignInAgain, err)
		}
		if !forced {
			s.mu.Lock()
			rec, gen := s.rec, s.gen
			ok := rec == cur && s.unexpiredLocked(cur)
			s.mu.Unlock()
			if ok {
				return cur.AccessToken, gen, cur, nil
			}
		}
		return "", 0, nil, err
	}
	next, err := s.renewed(cur, reply, sent)
	if err != nil {
		// The server has rotated, so cur's refresh token is spent, and a
		// reply that does not verify cannot be used: the sign-in is over.
		planOff := errors.Is(err, ErrPlanUsageDisabled)
		s.endSignIn(cur, false, planOff)
		if planOff {
			return "", 0, nil, err
		}
		return "", 0, nil, fmt.Errorf("%w: %w", ErrSignInAgain, err)
	}

	s.notify(next.values())
	werr := s.write(next)
	if errors.Is(werr, errFileGone) {
		s.signedOut()
		return "", 0, nil, ErrSignedOut
	}
	s.mu.Lock()
	s.retireLocked(s.rec, next)
	s.rec = next
	s.gen++
	gen := s.gen
	if werr != nil {
		if !s.dirty {
			s.dirty, s.base = true, cur.version()
		}
		s.info = nil
	}
	s.mu.Unlock()
	if werr != nil {
		return "", 0, nil, fmt.Errorf("chatgptauth: the renewed ChatGPT sign-in could not be saved, so other craze processes cannot use it (this one keeps it in memory): %w", werr)
	}
	s.settle(next)
	return next.AccessToken, gen, next, nil
}

// renewed is cur renewed by reply, verified (plan 033 §3.10 step 6): a
// Bearer token, an access token, a replacement refresh token, a lifetime,
// the plan scope still granted (a reply with no scope keeps the grant), and
// an access token that names no other client. The incarnation is kept, the
// generation advanced.
func (s *TokenSource) renewed(cur *record, reply *tokenReply, sent time.Time) (*record, error) {
	if !strings.EqualFold(reply.TokenType, "Bearer") {
		return nil, errors.New("chatgptauth: refresh: the token type is not Bearer")
	}
	if reply.AccessToken == "" || reply.RefreshToken == "" {
		return nil, errors.New("chatgptauth: refresh: the reply lacks an access token or a replacement refresh token")
	}
	life, ok := reply.expiresIn()
	if !ok {
		return nil, errors.New("chatgptauth: refresh: the reply states no usable expiry")
	}
	granted := slices.Clone(cur.Scopes)
	if reply.Scope != nil {
		granted = strings.Fields(*reply.Scope)
	}
	if !hasScope(granted, planScope) {
		return nil, fmt.Errorf("%w (the renewed grant no longer includes it)", ErrPlanUsageDisabled)
	}
	if c := accessClientID(reply.AccessToken); c != "" && c != cur.ClientID {
		return nil, errors.New("chatgptauth: refresh: the access token is for another client id")
	}
	next := *cur
	next.Scopes = granted
	next.AccessToken = reply.AccessToken
	next.AccessExpiresAt = sent.Add(life).UTC()
	next.RefreshToken = reply.RefreshToken
	next.EarliestRefreshAt = reply.earliestRefresh()
	if reply.IDToken != "" {
		next.IDToken = reply.IDToken
	}
	next.Generation = cur.Generation + 1
	next.LastRefresh = s.now().UTC()
	return &next, nil
}

// write writes r over the token file, which must still be there, trying
// writeTries times; errFileGone is not retried.
func (s *TokenSource) write(r *record) error {
	var err error
	for range writeTries {
		if err = writeRecord(s.dir, r, true); err == nil || errors.Is(err, errFileGone) {
			return err
		}
	}
	return err
}

// settle records that r is on disk now: the source is clean, and the file's
// new stat is the one the next Token compares against.
func (s *TokenSource) settle(r *record) {
	info, err := os.Lstat(TokenFile(s.dir))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rec != r {
		return
	}
	s.dirty = false
	if err == nil {
		s.info = info
	} else {
		s.info = nil
	}
}

// adopt makes file — read under the lock — the tokens in use: subscribers
// learn its values first, then the old ones retire.
func (s *TokenSource) adopt(file *record, info os.FileInfo) {
	s.notify(file.values())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retireLocked(s.rec, file)
	s.rec = file
	s.info = info
	s.dirty = false
	s.gen++
}

// endSignIn ends a sign-in the server refused for good, under the lock:
// the token file goes (the registration and host id stay); a refused
// registration's client id goes too, and a grant without plan usage marks
// the registration so.
func (s *TokenSource) endSignIn(cur *record, dropClient, planOff bool) {
	_ = removeRecord(s.dir)
	if dropClient {
		_ = dropClientIDLocked(s.dir, cur.ClientID)
	}
	if planOff {
		if c, err := ReadClient(s.dir); err == nil && c.ClientID == cur.ClientID {
			c.PlanUsage = false
			_ = writeClient(s.dir, c)
		}
	}
	s.signedOut()
}

// signedOut drops the tokens in memory: the file is gone. Their values
// retire.
func (s *TokenSource) signedOut() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rec == nil {
		return
	}
	s.retireLocked(s.rec, nil)
	s.rec, s.info, s.dirty = nil, nil, false
	s.gen++
}

// retireLocked retires old's values that next does not keep: an access
// token until retireAfter past the later of now and its expiry, the others
// until retireAfter past now (P35). s.mu is held.
func (s *TokenSource) retireLocked(old, next *record) {
	if old == nil {
		return
	}
	t := s.now()
	var keep []string
	if next != nil {
		keep = next.values()
	}
	for _, v := range old.values() {
		if slices.Contains(keep, v) {
			continue
		}
		until := t.Add(retireAfter)
		if v == old.AccessToken && old.AccessExpiresAt.After(t) {
			until = old.AccessExpiresAt.Add(retireAfter)
		}
		found := false
		for i := range s.retired {
			if s.retired[i].value == v {
				found = true
				if until.After(s.retired[i].until) {
					s.retired[i].until = until
				}
			}
		}
		if !found {
			s.retired = append(s.retired, retired{value: v, until: until})
		}
	}
}

// Values is every token value this source holds or retired less than
// retireAfter ago (P19, P35) — the access, refresh and id tokens — for the
// scrubber. It never blocks on the network or the lock.
func (s *TokenSource) Values() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.now()
	kept := s.retired[:0]
	for _, r := range s.retired {
		if r.until.After(t) {
			kept = append(kept, r)
		}
	}
	clear(s.retired[len(kept):])
	s.retired = kept
	var out []string
	if s.rec != nil {
		out = s.rec.values()
	}
	for _, r := range s.retired {
		if !slices.Contains(out, r.value) {
			out = append(out, r.value)
		}
	}
	return out
}

// Subscribe registers fn to learn token values as soon as this process does
// (P19): on a refresh, before the new record is written; on an adoption —
// the first look included — before the token is used. fn is called on the
// goroutine that learned them, with the lock held, so it must be quick and
// must not call Token or Invalidate. Subscribe first, then read Values, and
// no value is missed between the two. The returned function unsubscribes.
func (s *TokenSource) Subscribe(fn func(values []string)) (unsubscribe func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = fn
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.subs, id)
	}
}

// notify hands values to every subscriber.
func (s *TokenSource) notify(values []string) {
	s.mu.Lock()
	fns := make([]func([]string), 0, len(s.subs))
	for _, fn := range s.subs {
		fns = append(fns, fn)
	}
	s.mu.Unlock()
	for _, fn := range fns {
		fn(slices.Clone(values))
	}
}

// Sentinels are the fixed-text errors Token and Invalidate wrap, which
// callers tell apart by errors.Is: the scrubber in package llm keeps the one
// an error carries through every error it rebuilds (plan 033 §3.12), so the
// adapter can word "signed out" or "usage limit reached" for the person. Each
// is fixed text, so none can carry a token.
func (s *TokenSource) Sentinels() []error {
	return []error{ErrSignedOut, ErrSignInAgain, ErrPlanUsageDisabled, ErrUsageLimited}
}

// LatchUsageLimit stops every new request on this source (P33): the ChatGPT
// plan's usage limit was reached (the driver's usage-limit FinalError), and
// until ClearUsageLimit — the next turn the person starts — Token answers
// ErrUsageLimited at once, so neither a wake nor a sub-agent nor a retry
// sends another request.
func (s *TokenSource) LatchUsageLimit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latched = true
}

// ClearUsageLimit lifts the usage latch: the person started a turn.
func (s *TokenSource) ClearUsageLimit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latched = false
}

// UsageLimited says the usage latch is set.
func (s *TokenSource) UsageLimited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latched
}
