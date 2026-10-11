// Package anthropicmodels fetches the Anthropic account's model list and
// caches it beside the native harness's model files, as
// <dir>/anthropic-models.json (plan 044 §3.6): the list a load makes the
// Anthropic provider's models from (modeltable's withAnthropicList), bound to
// the key that fetched it, so a new Claude model is offered the day the
// account lists it.
//
// It is the ChatGPT plan's model-list cache (internal/chatgptauth/models.go)
// for a key: a background Refresh that takes the list's lock and holds it
// across the freshness check, the fetch and the write, so several hosts
// opening at once fetch once (P8); and a forced Fetch (craze auth login)
// that fetches outside the lock and writes under it only when no newer list
// for the same key landed meanwhile. A list is written only when it is
// complete and names at least one model once the ones already retired are
// dropped (the writer has a clock, the reader none: P25), within
// modeltable.MaxAnthropicModelsFile, atomically and 0600; anything else
// keeps the list there is (P10).
//
// The list is fetched with craze's own Messages API client
// (llm/messagesapi), and the file's name, version and shape are
// modeltable's, which reads it: this package sits outside the harness, so
// it may import both, and nothing of craze's but them and internal/atomicfile
// (deps_test.go). It runs outside the model wrapper's scrubber, so it takes
// the key out of every error it returns itself (P13), and the fetch is a
// variable tests replace, so no test reaches the real API (P30).
package anthropicmodels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/craze/internal/atomicfile"
	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// MaxAge is how old the list may be before a session's open fetches it again
// in the background (plan 044 §3.6): a day, as the ChatGPT plan's.
const MaxAge = 24 * time.Hour

// clockSkew is how far in the future a list's fetched_at may be and still be
// fresh: a clock set back a little does not refetch, one set back further
// does.
const clockSkew = 5 * time.Second

// lockWait bounds the wait for another process's lock: its holder fetches
// for at most the client's 30 s (messagesapi's ListModels bound). A variable
// only so a test can shorten it.
var lockWait = 60 * time.Second

// now is the writer's clock: a seam for the tests that age a list or retire
// a model.
var now = time.Now

// listModels is how a fetch lists the account's models with key: the
// Messages API client's ListModels against this process's API origin
// (messagesapi.BaseURL: Anthropic's, or a test's loopback override, and
// nothing else). Tests replace it, so none of them reaches the real API
// (P30).
var listModels = func(ctx context.Context, key string) ([]messagesapi.ModelInfo, bool, error) {
	base, err := messagesapi.BaseURL(nil)
	if err != nil {
		return nil, false, err
	}
	c, err := messagesapi.NewClient(messagesapi.Config{BaseURL: base})
	if err != nil {
		return nil, false, err
	}
	return c.ListModels(ctx, key)
}

// afterReply, inLock and onLockBusy are test seams: the first runs between
// Fetch's reply and its lock, the second inside every critical section,
// between the check and the write, and the third when the lock is held
// elsewhere, before the wait for it. nil in production.
var (
	afterReply func()
	inLock     func()
	onLockBusy func()
)

var (
	// ErrNoKey is a fetch asked for with no key, or a value too short to be
	// one (modeltable.MinKeyLen): nothing is sent.
	ErrNoKey = errors.New("anthropicmodels: no API key to list the models with")

	// ErrIncomplete is a list the API did not finish — a page that does not
	// say whether more follow, a repeated cursor, more pages than craze
	// follows — which never replaces a list (P10).
	ErrIncomplete = errors.New("anthropicmodels: the account's model list came back incomplete; the list craze has is kept")

	// ErrEmpty is a list with no model in it, once the retired ones are
	// dropped: never written, so an empty reply never replaces a list.
	ErrEmpty = errors.New("anthropicmodels: the account's model list came back empty; the list craze has is kept")

	// ErrTooLarge is a list over modeltable.MaxAnthropicModelsFile
	// serialized, which no reader would read: never written.
	ErrTooLarge = errors.New("anthropicmodels: the account's model list is larger than craze keeps; the list craze has is kept")
)

// List is anthropic-models.json as this package writes it: modeltable's
// AnthropicModels, its models the client's slim ModelInfo (their JSON is the
// file's; the shape pin in models_test.go keeps the two together).
type List struct {
	Version   int                     `json:"version"`
	FetchedAt time.Time               `json:"fetched_at"`
	KeyID     string                  `json:"key_id"`
	Models    []messagesapi.ModelInfo `json:"models"`
}

// path is dir's list file.
func path(dir string) string { return filepath.Join(dir, modeltable.AnthropicModelsFile) }

// Refresh fetches the account's list again when dir's is missing, not this
// key's (its key_id another key's id), unreadable, or fetched maxAge ago or
// more (or further in the future than clockSkew) — the background refresh a
// session's open runs (plan 044 §3.6, P8). It takes the list's lock first
// and holds it across the check made again, the fetch and the write, so
// hosts that open at once fetch once. It answers whether it fetched and
// wrote; a list it found current is no error and no fetch. Every error is
// free of the key.
func Refresh(ctx context.Context, dir, key string, maxAge time.Duration) (bool, error) {
	fetched, err := refresh(ctx, dir, key, maxAge)
	return fetched, scrub(err, key)
}

func refresh(ctx context.Context, dir, key string, maxAge time.Duration) (bool, error) {
	if !usable(key) {
		return false, ErrNoKey
	}
	if current(dir, key, maxAge) {
		return false, nil
	}
	unlock, err := lock(ctx, dir)
	if err != nil {
		return false, err
	}
	defer unlock()
	if current(dir, key, maxAge) {
		return false, nil
	}
	models, err := fetch(ctx, key)
	if err != nil {
		return false, err
	}
	if inLock != nil {
		inLock()
	}
	if _, err := store(dir, key, models); err != nil {
		return false, err
	}
	return true, nil
}

// Fetch fetches the account's list now — the forced form, after craze auth
// login saves a key (plan 044 §3.6) — and answers how many models the list
// it leaves holds (once the retired ones are dropped). The request runs
// outside the list's lock; under it, the list is written unless the file
// already holds a list for the same key fetched after this fetch began
// (another host's refresh, as fresh as this one), which stands. A reply that
// is incomplete, empty — every model retired by the time the lock is held
// included — or too large writes nothing (ErrIncomplete, ErrEmpty,
// ErrTooLarge). Every error is free of the key.
func Fetch(ctx context.Context, dir, key string) (int, error) {
	n, err := forced(ctx, dir, key)
	return n, scrub(err, key)
}

func forced(ctx context.Context, dir, key string) (int, error) {
	if !usable(key) {
		return 0, ErrNoKey
	}
	began := now()
	models, err := fetch(ctx, key)
	if err != nil {
		return 0, err
	}
	if afterReply != nil {
		afterReply()
	}
	unlock, err := lock(ctx, dir)
	if err != nil {
		return 0, err
	}
	defer unlock()
	if inLock != nil {
		inLock()
	}
	if doc := keyList(dir, key); doc != nil && doc.FetchedAt.After(began) {
		return len(doc.Models), nil // a newer list for this key landed: it stands
	}
	return store(dir, key, models)
}

// usable says key, trimmed, can be a key at all: modeltable's rule, so a
// value no load would fund with is never sent.
func usable(key string) bool {
	return len(strings.TrimSpace(key)) >= modeltable.MinKeyLen
}

// fetch is the account's list with key, complete: the list a write takes.
// Which of its models have retired is the write's to judge (store), not the
// reply's: the write may wait for the lock past a model's retirement.
func fetch(ctx context.Context, key string) ([]messagesapi.ModelInfo, error) {
	models, complete, err := listModels(ctx, strings.TrimSpace(key))
	switch {
	case err != nil:
		return nil, fmt.Errorf("anthropicmodels: listing the account's models: %w", err)
	case !complete:
		return nil, ErrIncomplete
	}
	return models, nil
}

// store writes models as dir's list for key, less the ones retired, and
// answers how many it wrote: atomically, 0600, and only within
// MaxAnthropicModelsFile. The caller holds the list's lock, whose taking made
// the directory (lock). retires_at is judged here, by the clock read once the
// lock is held, the same reading the file's fetched_at takes (P25, C4 review
// 2): modeltable has no clock, so a model the file names is a model the
// reader offers until the list is fetched again — and a forced fetch may wait
// for the lock past a model's retirement. A list with no model left is
// ErrEmpty, and writes nothing.
func store(dir, key string, models []messagesapi.ModelInfo) (int, error) {
	at := now()
	var live []messagesapi.ModelInfo
	for _, m := range models {
		if m.RetiresAt.IsZero() || at.Before(m.RetiresAt) {
			live = append(live, m)
		}
	}
	if len(live) == 0 {
		return 0, ErrEmpty
	}
	b, err := json.MarshalIndent(List{
		Version:   modeltable.AnthropicModelsVersion,
		FetchedAt: at.UTC(),
		KeyID:     modeltable.AnthropicKeyID(key),
		Models:    live,
	}, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("anthropicmodels: %w", err)
	}
	b = append(b, '\n')
	if len(b) > modeltable.MaxAnthropicModelsFile {
		return 0, ErrTooLarge
	}
	if err := atomicfile.Write(path(dir), b, 0o600); err != nil {
		return 0, fmt.Errorf("anthropicmodels: writing the model list: %w", err)
	}
	return len(live), nil
}

// keyList is dir's list as modeltable reads it when it is key's and names a
// model; nil for one it cannot read, another key's, or an empty one.
func keyList(dir, key string) *modeltable.AnthropicModels {
	doc, err := modeltable.ReadAnthropicModels(dir)
	if err != nil || doc.KeyID != modeltable.AnthropicKeyID(key) || len(doc.Models) == 0 {
		return nil
	}
	return doc
}

// current says dir's list is key's, has a model, and was fetched less than
// maxAge ago (and no further in the future than clockSkew).
func current(dir, key string, maxAge time.Duration) bool {
	doc := keyList(dir, key)
	if doc == nil {
		return false
	}
	age := now().Sub(doc.FetchedAt)
	return age >= -clockSkew && age < maxAge
}

// lock takes dir's list lock (anthropic-models.json.lock): at once when it is
// free, else waiting for it within lockWait, giving up when ctx is done. The
// directory is made first.
func lock(ctx context.Context, dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("anthropicmodels: %w", err)
	}
	lockPath := path(dir) + ".lock"
	unlock, err := atomicfile.LockWithin(lockPath, 0)
	if errors.Is(err, atomicfile.ErrLockBusy) {
		if onLockBusy != nil {
			onLockBusy()
		}
		unlock, err = atomicfile.LockContext(ctx, lockPath, lockWait)
	}
	if err != nil {
		return nil, fmt.Errorf("anthropicmodels: taking the model list's lock: %w", err)
	}
	return unlock, nil
}

// redacted is what a key reads as in an error this package returns.
const redacted = "[redacted]"

// classes are what a caller tells this package's failures apart by: its
// own, the context's, and a lock held too long. A scrubbed error unwraps to
// the one its cause is.
var classes = []error{ErrNoKey, ErrIncomplete, ErrEmpty, ErrTooLarge,
	context.Canceled, context.DeadlineExceeded, atomicfile.ErrLockBusy, messagesapi.ErrOverride}

// scrub is err with the key taken out (P13): the Messages API client scrubs
// what it returns, but a fetch runs outside the model wrapper and a
// replacement fetch need not, so every error is rebuilt from its text with
// each spelling of the key — plain, trimmed, JSON-escaped, percent-encoded —
// replaced, and never keeps its cause, whose fields may hold what its text
// does not show. It keeps what a caller classifies by: the class its cause is
// (classes), and an API error's status and type, as a copy whose texts are
// scrubbed too (an *messagesapi.Error, through errors.As).
func scrub(err error, key string) error {
	if err == nil {
		return nil
	}
	s := &scrubbedError{msg: scrubText(err.Error(), key)}
	for _, c := range classes {
		if errors.Is(err, c) {
			s.class = c
			break
		}
	}
	if apiErr, ok := errors.AsType[*messagesapi.Error](err); ok {
		c := *apiErr
		c.Type, c.Message, c.RequestID = scrubText(c.Type, key), scrubText(c.Message, key), scrubText(c.RequestID, key)
		s.api = &c
	}
	return s
}

// scrubText is v with every spelling of key replaced by redacted.
func scrubText(v, key string) string {
	if !usable(key) {
		return v // no key: Fetch and Refresh refuse it before any request
	}
	k := strings.TrimSpace(key)
	quoted, _ := json.Marshal(k)
	for _, form := range []string{key, k, string(quoted[1 : len(quoted)-1]), url.QueryEscape(k), url.PathEscape(k)} {
		if form != "" {
			v = strings.ReplaceAll(v, form, redacted)
		}
	}
	return v
}

// scrubbedError is an error rebuilt from another's scrubbed text: it
// unwraps to the other's class, and to a scrubbed copy of its API error.
type scrubbedError struct {
	msg   string
	class error
	api   *messagesapi.Error
}

func (e *scrubbedError) Error() string { return e.msg }

func (e *scrubbedError) Unwrap() []error {
	var out []error
	if e.class != nil {
		out = append(out, e.class)
	}
	if e.api != nil {
		out = append(out, e.api)
	}
	return out
}
